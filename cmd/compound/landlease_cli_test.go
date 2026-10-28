package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/landlease"
)

func runLandLeaseCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runLandLease(args, &out, &errb)
	return out.String(), errb.String(), code
}

// A different holder cannot acquire over a live lease via the CLI: the second acquire exits non-zero
// with the current holder surfaced, and status reflects the original holder.
func TestLandLeaseCLIAcquireRefusesCompetingHolder(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	_, errStr, code := runLandLeaseCLI(t, "acquire", "--holder", "lane-alpha", "--ttl", "300", "--home", home)
	if code != 0 {
		t.Fatalf("A acquire exit=%d stderr=%s", code, errStr)
	}

	// B is refused (non-zero) while A holds, and the current holder is printed.
	_, errStr, code = runLandLeaseCLI(t, "acquire", "--holder", "lane-beta", "--ttl", "300", "--home", home)
	if code == 0 {
		t.Fatalf("B acquire over A's live lease must exit non-zero")
	}
	if !strings.Contains(errStr, "lane-alpha") {
		t.Fatalf("refusal must name the current holder, got %q", errStr)
	}

	// status --json shows A still holding, live.
	out, errStr, code := runLandLeaseCLI(t, "status", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, errStr)
	}
	var st landlease.StatusResult
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("decode status: %v (%s)", err, out)
	}
	if !st.Held || st.Holder != "lane-alpha" {
		t.Fatalf("status should show lane-alpha holding: %+v", st)
	}
}

// Steal via the CLI requires a reason and records the audit trail; a reasonless steal is refused.
func TestLandLeaseCLIStealRequiresReasonAndRecords(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if _, errStr, code := runLandLeaseCLI(t, "acquire", "--holder", "lane-alpha", "--home", home); code != 0 {
		t.Fatalf("A acquire exit=%d stderr=%s", code, errStr)
	}

	// Reasonless steal is refused.
	if _, _, code := runLandLeaseCLI(t, "steal", "--holder", "lane-beta", "--home", home); code == 0 {
		t.Fatalf("steal without --reason must exit non-zero")
	}

	// Steal with a reason succeeds and records prior_holder + reason.
	out, errStr, code := runLandLeaseCLI(t, "steal", "--holder", "lane-beta", "--reason", "alpha wedged", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("steal exit=%d stderr=%s", code, errStr)
	}
	var lease landlease.Lease
	if err := json.Unmarshal([]byte(out), &lease); err != nil {
		t.Fatalf("decode steal: %v (%s)", err, out)
	}
	if lease.Holder != "lane-beta" || lease.PriorHolder != "lane-alpha" || lease.StealReason != "alpha wedged" {
		t.Fatalf("steal record wrong: %+v", lease)
	}
	if lease.StolenAt == "" {
		t.Fatalf("steal must record stolen_at")
	}
}

// Release via the CLI is holder-scoped: a non-holder release is refused; the holder frees it.
func TestLandLeaseCLIReleaseHolderScoped(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if _, errStr, code := runLandLeaseCLI(t, "acquire", "--holder", "lane-alpha", "--home", home); code != 0 {
		t.Fatalf("A acquire exit=%d stderr=%s", code, errStr)
	}
	// Non-holder release is refused.
	if _, _, code := runLandLeaseCLI(t, "release", "--holder", "lane-beta", "--home", home); code == 0 {
		t.Fatalf("non-holder release must exit non-zero")
	}
	// Holder release frees it, then status shows none held.
	if _, errStr, code := runLandLeaseCLI(t, "release", "--holder", "lane-alpha", "--home", home); code != 0 {
		t.Fatalf("holder release exit=%d stderr=%s", code, errStr)
	}
	out, _, code := runLandLeaseCLI(t, "status", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("status exit=%d", code)
	}
	var st landlease.StatusResult
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("decode status: %v (%s)", err, out)
	}
	if st.Held {
		t.Fatalf("lease should be free after holder release: %+v", st)
	}
}

// The land-lease is advisory: nothing stops a lane that reads a refusal from merging anyway.
// That makes the refusal text the actual enforcement, so each of the three gates must leave
// the reader with the right next verb rather than a restatement of the rule.
//
// RED WITHOUT the conversion: all three printed `error: land-lease held by "lane-alpha" …`
// (or `… steal requires a reason …`), so assertTeachingRefusal fails immediately on the gate
// name; nothing in the old text named status, --wait, or the steal's --reason as commands.
func TestLandLeaseCLIRefusalsTeachTheRecoveryLadder(t *testing.T) {
	t.Parallel()
	t.Run("contention names wait and steal without preferring the steal", func(t *testing.T) {
		home := t.TempDir()
		if _, errStr, code := runLandLeaseCLI(t, "acquire", "--holder", "lane-alpha", "--home", home); code != 0 {
			t.Fatalf("A acquire exit=%d stderr=%s", code, errStr)
		}
		_, errStr, code := runLandLeaseCLI(t, "acquire", "--holder", "lane-beta", "--home", home)
		if code == 0 {
			t.Fatalf("B acquire over A's live lease must exit non-zero")
		}
		assertTeachingRefusal(t, errStr, "land_lease_contention",
			"lane-alpha",
			"compound land-lease status",
			"--holder lane-beta --wait",
			"--holder lane-beta --reason",
		)
		// A steal offered without its cost reads as the obvious fix. The refusal must
		// carry the condition under which stealing is the WRONG move.
		if !strings.Contains(errStr, "NOT mid-ceremony") {
			t.Errorf("the steal rung must state when it is the wrong move, got:\n%s", errStr)
		}
	})

	t.Run("non-holder release names the holder's own release", func(t *testing.T) {
		home := t.TempDir()
		if _, errStr, code := runLandLeaseCLI(t, "acquire", "--holder", "lane-alpha", "--home", home); code != 0 {
			t.Fatalf("A acquire exit=%d stderr=%s", code, errStr)
		}
		_, errStr, code := runLandLeaseCLI(t, "release", "--holder", "lane-beta", "--home", home)
		if code == 0 {
			t.Fatalf("non-holder release must exit non-zero")
		}
		assertTeachingRefusal(t, errStr, "land_lease_holder",
			// The remedy command must carry the ACTUAL holder, not the caller: a
			// release suggestion echoing the refused holder is a command that fails
			// exactly the same way.
			"compound land-lease release --holder lane-alpha",
			"compound land-lease steal --holder lane-beta",
		)
	})

	t.Run("reasonless steal names acquire as the alternative", func(t *testing.T) {
		home := t.TempDir()
		if _, errStr, code := runLandLeaseCLI(t, "acquire", "--holder", "lane-alpha", "--home", home); code != 0 {
			t.Fatalf("A acquire exit=%d stderr=%s", code, errStr)
		}
		_, errStr, code := runLandLeaseCLI(t, "steal", "--holder", "lane-beta", "--home", home)
		if code == 0 {
			t.Fatalf("steal without --reason must exit non-zero")
		}
		assertTeachingRefusal(t, errStr, "land_lease_steal_audit",
			"lane-alpha",
			"compound land-lease acquire --holder lane-beta",
		)
		// Teeth: the refusal must not be reachable by simply passing an empty reason,
		// so the gate has to fire on the trimmed value.
		if _, _, code := runLandLeaseCLI(t, "steal", "--holder", "lane-beta", "--reason", "   ", "--home", home); code == 0 {
			t.Errorf("a whitespace-only --reason must not satisfy the steal audit gate")
		}
	})
}

func TestLandLeaseCLIUsage(t *testing.T) {
	t.Parallel()
	_, _, code := runLandLeaseCLI(t)
	if code != 2 {
		t.Fatalf("no args should print usage and exit 2, got %d", code)
	}
	_, errStr, code := runLandLeaseCLI(t, "bogus")
	if code != 2 || !strings.Contains(errStr, "usage") {
		t.Fatalf("unknown subcommand should print usage/exit 2, got code=%d err=%s", code, errStr)
	}
}
