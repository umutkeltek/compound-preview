package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/behaviorallens"
	"compound/internal/home"
	"compound/internal/lensmodulator"
	"compound/internal/policy"
)

// cmdLensModulate is the operator door onto the GOVERNED MODULATOR — the organ that turns a recorded
// behavioral-lens signal into a reversible, policy-gated route/role demote. It delegates to
// runLensModulate, which returns an exit code (never calls os.Exit) so the dry-run default and the
// governed/refused paths are exercised by tests.
func cmdLensModulate(args []string) {
	if code := runLensModulate(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func lensModulateUsage() string {
	return "usage:\n" +
		"  compound lens-modulate arm --lens ID --min V --demote-to X [--subject S] [--baseline B] [--home P] [--json]\n" +
		"    arm a threshold. CALIBRATION-GUARDED: refused unless >=1 real measured signal exists for --lens.\n" +
		"  compound lens-modulate thresholds [--home P] [--json]\n" +
		"    list armed thresholds (none => modulator is OFF / fail-closed).\n" +
		"  compound lens-modulate modulate --work W [--confirm] [--home P] [--json]\n" +
		"    DRY-RUN by default (names lanes that WOULD be demoted, writes nothing). --confirm performs the\n" +
		"    governed reversible demote through act — it REQUIRES an allowing PolicyDecision (a standing\n" +
		"    grant for the demote capability); without one the act refuses and NOTHING is written.\n" +
		"  compound lens-modulate revert --lane L --work W [--home P] [--json]\n" +
		"    governed undo of a lane's latest demote (restores the captured pre-image through act).\n" +
		"signals are read from $COMPOUND_HOME; this command makes NO provider calls."
}

func runLensModulate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(stderr, lensModulateUsage())
		return 2
	}
	switch args[0] {
	case "arm":
		return runLensModulateArm(args[1:], stdout, stderr)
	case "thresholds":
		return runLensModulateThresholds(args[1:], stdout, stderr)
	case "modulate":
		return runLensModulateModulate(args[1:], stdout, stderr)
	case "revert":
		return runLensModulateRevert(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, lensModulateUsage())
		return 2
	}
}

func runLensModulateArm(args []string, stdout, stderr io.Writer) int {
	lens := strings.TrimSpace(flagValue(args, "--lens"))
	minRaw := strings.TrimSpace(flagValue(args, "--min"))
	demoteTo := strings.TrimSpace(flagValue(args, "--demote-to"))
	if lens == "" || minRaw == "" || demoteTo == "" {
		fmt.Fprintln(stderr, "usage: compound lens-modulate arm --lens ID --min V --demote-to X [--subject S] [--baseline B]")
		return 2
	}
	minV, err := strconv.ParseFloat(minRaw, 64)
	if err != nil {
		fmt.Fprintf(stderr, "error: --min must be a number: %v\n", err)
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	th := lensmodulator.Threshold{
		LensID:       lens,
		SubjectScope: strings.TrimSpace(flagValue(args, "--subject")),
		MinValue:     minV,
		DemoteTo:     demoteTo,
		Baseline:     strings.TrimSpace(flagValue(args, "--baseline")),
	}
	if err := lensmodulator.Arm(homePath, th, time.Now()); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, th); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "armed threshold: lens=%s min=%.3f demote-to=%s subject=%q\n", th.LensID, th.MinValue, th.DemoteTo, th.SubjectScope)
	return 0
}

func runLensModulateThresholds(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, err := lensmodulator.LoadThresholds(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, cfg); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	if len(cfg.Thresholds) == 0 {
		fmt.Fprintln(stdout, "# lens-modulate thresholds: NONE armed (modulator is OFF / fail-closed)")
		return 0
	}
	fmt.Fprintln(stdout, "# lens-modulate thresholds")
	for _, th := range cfg.Thresholds {
		fmt.Fprintf(stdout, "- lens=%s min=%.3f demote-to=%s subject=%q baseline=%s\n", th.LensID, th.MinValue, th.DemoteTo, th.SubjectScope, th.Baseline)
	}
	return 0
}

func runLensModulateModulate(args []string, stdout, stderr io.Writer) int {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	confirm := hasFlag(args, "--confirm")
	if !confirm {
		// DRY-RUN default: name what WOULD be demoted, write nothing.
		res, err := lensmodulator.Plan(homePath, time.Now())
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return printModulation(stdout, res, hasFlag(args, "--json"), "dry-run")
	}
	work := strings.TrimSpace(flagValue(args, "--work"))
	if work == "" {
		fmt.Fprintln(stderr, "error: --work W is required for --confirm (a governed act belongs to a work)")
		return 2
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	res, err := lensmodulator.Modulate(homePath, cfg, work, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return printModulation(stdout, res, hasFlag(args, "--json"), "confirmed")
}

func runLensModulateRevert(args []string, stdout, stderr io.Writer) int {
	lane := strings.TrimSpace(flagValue(args, "--lane"))
	work := strings.TrimSpace(flagValue(args, "--work"))
	if lane == "" || work == "" {
		fmt.Fprintln(stderr, "usage: compound lens-modulate revert --lane L --work W [--home P] [--json]")
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	out, err := lensmodulator.Revert(homePath, cfg, lane, work, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, out); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	if out.Reverted {
		fmt.Fprintf(stdout, "reverted lane=%s restored=%s receipt=%s transition=%s\n", out.Lane, out.RestoredValue, out.ReceiptID, out.TransitionID)
	} else {
		fmt.Fprintf(stdout, "not reverted lane=%s reason=%s\n", out.Lane, out.Reason)
	}
	return 0
}

func printModulation(stdout io.Writer, res lensmodulator.ModulationResult, asJSON bool, mode string) int {
	if asJSON {
		if err := encodeJSON(stdout, res); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# lens-modulate (%s)\n", mode)
	if len(res.Modulations) == 0 {
		fmt.Fprintln(stdout, "- no lane crosses an armed threshold (or modulator is OFF)")
	}
	for _, m := range res.Modulations {
		state := "would-demote"
		if m.Applied {
			state = "demoted"
		} else if m.Refused {
			state = "REFUSED (no allowing PolicyDecision)"
		}
		vis := ""
		if m.Visibility == behaviorallens.VisibilityOperatorPrivate {
			vis = " [operator-local]"
		}
		fmt.Fprintf(stdout, "- lane=%s lens=%s value=%.3f>=%.3f -> %s%s receipt=%s\n",
			m.Lane, m.LensID, m.SignalValue, m.Threshold, state, vis, m.ReceiptID)
	}
	for _, s := range res.Skipped {
		fmt.Fprintf(stdout, "- skipped lane=%s lens=%s reason=%s\n", s.Lane, s.LensID, s.Reason)
	}
	return 0
}
