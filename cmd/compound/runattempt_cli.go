// runattempt_cli.go owns the `compound run-attempt ...` verb (cmdRunAttempt).
// C3: crash terminality — sweep stale pending/running RunAttempts to terminal
// so no attempt stays pending forever. RunAttempt is evidence, not enforcement:
// sweeping has NO effect on the work state machine.
//
// Subcommands:
//   - list [--non-terminal] [--home PATH]               list attempts (all, or just stuck)
//   - sweep [--attempt RATT_ID] [--threshold D] [--home PATH] terminalize stale attempts (default 30m).
//     The CLI ALWAYS passes the work-owned
//     liveness probe (buildDelegateLivenessProbe)
//     so live delegated panes are heartbeated;
//     probe faults skip the candidate. The
//     store-level SweepStale (no probe) is
//     the compatibility test path and is NOT
//     surfaced as a CLI opt-out -- the
//     operator-visible default must never
//     abandon a healthy in-flight session.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func cmdRunAttempt(args []string) {
	if len(args) == 0 {
		printRunAttemptUsage()
		return
	}
	switch args[0] {
	case "list":
		cmdRunAttemptList(args[1:])
	case "sweep":
		cmdRunAttemptSweep(args[1:])
	case "--help", "-h", "help":
		printRunAttemptUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown run-attempt subcommand: %s\n\n", args[0])
		printRunAttemptUsage()
	}
}

func printRunAttemptUsage() {
	fmt.Fprintf(os.Stderr, `usage: compound run-attempt <subcommand>

subcommands:
  list [--non-terminal] [--home PATH]   list run attempts (all, or just pending/running)
  sweep [--attempt RATT_ID] [--threshold 30m] [--home PATH]
                                        terminalize stale pending/running attempts
                                        (default threshold: %s). Live delegated
                                        panes are protected via the work-owned
                                        liveness probe; probe faults skip.
`, runattempt.DefaultSweepThreshold)
}

func resolveRunAttemptHome(homeArg string) string {
	homePath, err := home.Resolve(homeArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-attempt: %v\n", err)
		os.Exit(1)
	}
	return homePath
}

func validateRunAttemptArgs(args []string, valueFlags, boolFlags map[string]bool) error {
	if err := validateKnownFlags(args, valueFlags, boolFlags); err != nil {
		return err
	}
	if err := rejectFlagUsedAsValue(args, valueFlags); err != nil {
		return err
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valueFlags[arg] {
			i++
			continue
		}
		if boolFlags[arg] {
			continue
		}
		return fmt.Errorf("unexpected positional argument %q", arg)
	}
	return nil
}

func requireValidRunAttemptArgs(args []string, valueFlags, boolFlags map[string]bool) {
	if err := validateRunAttemptArgs(args, valueFlags, boolFlags); err != nil {
		fmt.Fprintln(os.Stderr, "run-attempt:", err)
		fmt.Fprintln(os.Stderr)
		printRunAttemptUsage()
		os.Exit(2)
	}
}

func cmdRunAttemptList(args []string) {
	if hasUsageFlag(args) {
		printRunAttemptUsage()
		return
	}
	requireValidRunAttemptArgs(args,
		map[string]bool{"--home": true},
		map[string]bool{"--non-terminal": true, "--json": true},
	)
	homePath := resolveRunAttemptHome(flagValue(args, "--home"))
	nonTerminal := hasFlag(args, "--non-terminal")
	jsonOut := hasFlag(args, "--json")
	store := runattempt.Open(homePath)

	var attempts []runattempt.RunAttempt
	var err error
	if nonTerminal {
		attempts, err = store.ListNonTerminal()
	} else {
		attempts, err = store.List()
	}
	unreadableIDs := runattempt.UnreadableRunAttemptIDs(err)
	if err != nil && len(unreadableIDs) == 0 {
		fmt.Fprintf(os.Stderr, "run-attempt list: %v\n", err)
		os.Exit(1)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encodeErr := enc.Encode(attempts); encodeErr != nil {
			fmt.Fprintf(os.Stderr, "run-attempt list: encode: %v\n", encodeErr)
			os.Exit(1)
		}
	} else {
		if len(attempts) == 0 {
			if err != nil {
				fmt.Println("no readable run attempts found")
			} else {
				fmt.Println("no run attempts found")
			}
		} else {
			for _, ra := range attempts {
				age := time.Since(ra.CreatedAt).Round(time.Second)
				origin := ra.Origin
				if origin == "" {
					origin = "unknown"
				}
				hb := ""
				if ra.LastHeartbeatAt != nil {
					hb = fmt.Sprintf(" hb=%s", time.Since(*ra.LastHeartbeatAt).Round(time.Second))
				}
				reason := ""
				if ra.SweepReason != "" {
					reason = " (" + ra.SweepReason + ")"
				}
				fmt.Printf("%s  status=%-9s origin=%s work=%s actor=%s harness=%s age=%s%s%s\n",
					ra.ID, ra.Status, origin, ra.WorkID, ra.Actor, ra.Harness, age, hb, reason)
			}
			if err != nil {
				fmt.Printf("\n%d readable attempt(s)\n", len(attempts))
			} else {
				fmt.Printf("\n%d attempt(s)\n", len(attempts))
			}
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-attempt list: %v\n", err)
		os.Exit(1)
	}
}

func cmdRunAttemptSweep(args []string) {
	// sweep mutates stale attempt records. Help, a typo, a missing value, or an
	// unexpected positional token is not consent to run it with defaults, so the
	// entire argument shape is rejected before home resolution or store access.
	if hasUsageFlag(args) {
		printRunAttemptUsage()
		return
	}
	options, err := runattempt.ParseSweepArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-attempt sweep: %v\n\n", err)
		printRunAttemptUsage()
		os.Exit(2)
	}
	homePath := resolveRunAttemptHome(options.HomePath)
	threshold := options.Threshold
	jsonOut := options.JSON
	store := runattempt.Open(homePath)
	// The default sweep ALWAYS protects stale live delegates. The probe
	// is work-owned (RecoverBoundSession + tmux SessionStatus). Probe
	// errors skip rather than abandon. nil-probe byte identity with
	// SweepStale is preserved at the store level for the focused
	// store-level compatibility test (and for any direct API caller
	// that wants the pre-probe behaviour); it is NOT surfaced as a
	// CLI opt-out, because the operator-visible default must never
	// abandon a healthy in-flight session.
	probe := buildDelegateLivenessProbe(homePath)
	var rep runattempt.SweepReport
	if options.HasAttempt {
		rep, err = store.SweepStaleAttemptWithProbe(options.AttemptID, threshold, time.Now().UTC(), probe)
	} else {
		rep, err = store.SweepStaleWithProbe(threshold, time.Now().UTC(), probe)
	}
	unreadableIDs := runattempt.UnreadableRunAttemptIDs(err)
	if err != nil && len(unreadableIDs) == 0 {
		fmt.Fprintf(os.Stderr, "run-attempt sweep: %v\n", err)
		os.Exit(1)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encodeErr := enc.Encode(rep); encodeErr != nil {
			fmt.Fprintf(os.Stderr, "run-attempt sweep: encode: %v\n", encodeErr)
			os.Exit(1)
		}
	} else {
		fmt.Printf("swept %d/%d stale attempt(s) (threshold %s, protected %d, unreadable %d)\n",
			rep.Swept, rep.Scanned, threshold, rep.Protected, rep.Unreadable)
		for _, id := range rep.SweptIDs {
			fmt.Printf("  swept:     %s\n", id)
		}
		unreadable := make(map[string]struct{}, len(rep.UnreadableIDs))
		for _, id := range rep.UnreadableIDs {
			unreadable[id] = struct{}{}
		}
		for _, id := range rep.SkippedIDs {
			if _, unsafe := unreadable[id]; unsafe {
				continue
			}
			fmt.Printf("  kept:      %s\n", id)
		}
		for _, id := range rep.UnreadableIDs {
			fmt.Printf("  unreadable: %s\n", id)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-attempt sweep: %v\n", err)
		os.Exit(1)
	}
}

// buildDelegateLivenessProbe builds the work-owned liveness probe that
// `compound run-attempt sweep` always passes to SweepStaleWithProbe.
// The probe maps each stale candidate to its exact delegate binding
// (when one exists), probes SessionStatus, and returns live=true only
// when the bound pane is alive. A candidate with no delegate binding
// returns live=false (ordinary stale abandonment); a probe error
// returns err (the store's sweeper skips on err, so the attempt is
// retried on the next pass).
func buildDelegateLivenessProbe(homePath string) runattempt.LivenessProbe {
	return buildDelegateLivenessProbeWithRunner(homePath, work.NewTmuxRunner())
}

func buildDelegateLivenessProbeWithRunner(homePath string, runner work.SessionRunner) runattempt.LivenessProbe {
	wstore, werr := work.Open(homePath)
	if werr != nil {
		// Without work store, the default would NOT protect live panes
		// -- that defeats the safety contract. Return a probe that
		// errors on every candidate so the sweeper skips all of them
		// rather than abandoning any.
		return func(ra runattempt.RunAttempt) (bool, error) {
			return false, fmt.Errorf("work store unavailable: %w", werr)
		}
	}
	if runner == nil {
		return func(ra runattempt.RunAttempt) (bool, error) {
			return false, fmt.Errorf("delegate liveness runner unavailable")
		}
	}
	return func(ra runattempt.RunAttempt) (bool, error) {

		if live, err := wstore.ProofExecutionLive(ra.ID, time.Now().UTC()); err != nil || live {
			return live, err
		}

		binding, _, rerr := wstore.RecoverBoundSession(ra.WorkID)
		if rerr != nil {
			return false, rerr
		}
		if binding == nil || binding.RunAttemptID != ra.ID {
			// No exact delegate binding for this ratt -- the candidate
			// is claim-reservation / observe-run, not a delegated
			// execution. Generic stale semantics apply (live=false
			// here means the candidate will be abandoned in the
			// caller).
			return false, nil
		}
		st, perr := runner.SessionStatus(binding.Session)
		if perr != nil {
			return false, perr
		}
		return st.Exists && !st.PaneDead, nil
	}
}
