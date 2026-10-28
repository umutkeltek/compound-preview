// reseal_cli.go owns the `compound work reseal` verb — the governed repair
// door for terminal envelopes whose recorded custody seal disagrees with
// their bytes (see reseal.go for the incident and contract). It mirrors the
// sweep-stale CLI shape: DRY-RUN IS THE DEFAULT, `--yes` is the only mutation
// gate, and the verb is global (a stray work_id positional is rejected) so a
// repair pass always audits the whole custody surface in one report.
package work

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// RunWorkResealCLI is the testable command edge for
// `compound work reseal [--dry-run|--yes] [--by <actor>] [--json]`.
func RunWorkResealCLI(args []string, store *Store, now time.Time, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Fprintln(stdout, resealUsageText())
			return 0
		}
	}
	apply := false
	jsonOut := false
	actor := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
		case "--json":
			jsonOut = true
		case "--yes":
			apply = true
		case "--by":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "error: --by requires a value")
				fmt.Fprintln(stderr, resealUsageText())
				return 2
			}
			i++
			actor = args[i]
		default:
			fmt.Fprintf(stderr, "error: unexpected argument %q\n", args[i])
			fmt.Fprintln(stderr, resealUsageText())
			return 2
		}
	}
	if strings.TrimSpace(actor) == "" {
		actor = ResealActorDefault
	}

	var (
		rep ResealReport
		err error
	)
	if apply {
		rep, err = store.ResealApply(actor, now)
	} else {
		rep, err = store.ResealPlan(actor, now)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: work reseal: %v\n", err)
		return 1
	}

	if jsonOut {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if encodeErr := encoder.Encode(rep); encodeErr != nil {
			fmt.Fprintf(stderr, "error: encode reseal report: %v\n", encodeErr)
			return 1
		}
		return resealApplyExit(rep, apply)
	}

	mode := "dry-run"
	if apply {
		mode = "applied"
	}
	fmt.Fprintf(stdout, "reseal %s scanned=%d mismatched=%d resealed=%d skipped=%d unreadable=%d\n",
		mode, rep.Scanned, rep.Mismatched, rep.Resealed, rep.Skipped, rep.Unreadable)
	for _, c := range rep.Candidates {
		status := "would-reseal"
		switch {
		case c.Resealed:
			status = "resealed"
		case c.ResealError != "":
			status = "error:" + c.ResealError
		case apply:
			status = "already-sealed"
		}
		fmt.Fprintf(stdout, "  %s state=%s location=%s stored=%s computed=%s %s\n",
			c.WorkID, c.State, c.Location, orNone(c.StoredWorkSHA256), c.ComputedWorkSHA256, status)
	}
	for _, id := range rep.UnreadableIDs {
		fmt.Fprintf(stdout, "  unreadable: %s\n", id)
	}
	return resealApplyExit(rep, apply)
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none)"
	}
	return value
}

// resealApplyExit keeps the verb safe to schedule: dry-run always exits 0;
// --yes exits 1 only when a repair errored (a mismatch that a concurrent
// writer resolved is not an error).
func resealApplyExit(rep ResealReport, apply bool) int {
	if !apply {
		return 0
	}
	for _, c := range rep.Candidates {
		if c.ResealError != "" {
			return 1
		}
	}
	return 0
}

func resealUsageText() string {
	return "usage: compound work reseal [--dry-run|--yes] [--by <actor>] [--json]\n  default is dry-run (lists work id, stored seal, computed seal; exits 0).\n  --yes appends one sealed same-state lifecycle event per mismatching terminal\n  envelope (from==to, work_sha256 = current envelope hash, previous_work_sha256\n  = stale seal) so the autonomy director's custody check verifies again.\n  Never rewrites envelope bytes or history; non-terminal envelopes are skipped."
}
