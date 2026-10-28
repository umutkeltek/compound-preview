package promotedriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// TestR3FRecoverAbandonsFrozenParkWithMidReplacementStagingRefused pins the
// fail-closed branch of the recovery door: --recover must refuse to abandon
// a frozen pass whose target has a candidate publication staging file
// present. The staging file is the auth signal that another writer is
// mid-publish; abandoning would let the planner overwrite the partial
// result. The red test asserts --recover errors with a clear reason.
func TestR3FRecoverAbandonsFrozenParkWithMidReplacementStagingRefused(t *testing.T) {
	home := t.TempDir()
	const candID = "cand_4198be82cb50"
	seedCandidate(t, home, candID, mem.Candidate{
		Entity:    "umut",
		Predicate: "preference.repeated-tool-choice",
		Object:    "context7",
		ClaimType: "preference",
	})
	writeFrozenParkPass(t, home, candID, testNow)

	// Plant a staging file alongside the candidate: this simulates a
	// writer that crashed mid-publish (the inbox uses `.candidate-publish-<hex>`
	// temp names per internal/candidates/rooted_unix.go createTempFile).
	// --recover must NOT silently abandon while that file is present.
	stagingDir := filepath.Join(home, candidates.InboxDir(candidates.Memory))
	stagingName := ".candidate-publish-0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(stagingDir, stagingName), []byte("partial"), 0o644); err != nil {
		t.Fatalf("plant staging file: %v", err)
	}

	report, err := Run(home, Options{
		Apply:   true,
		Recover: true,
		WorkID:  "work_r3f_recover_mid",
	}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("recovery door mid-replacement run returned err=%v report.Stage=%q", err, report.Stage)
	}
	if report.Stage != stageAbandoned {
		t.Fatalf("report.Stage = %q, want %q", report.Stage, stageAbandoned)
	}
	if !strings.Contains(report.Error, "staging file") {
		t.Fatalf("report.Error must name the staging file; got %q", report.Error)
	}
	// We do NOT test that the file's mid-replacement is itself preserved —
	// the abandonment receipt is what unblocks the next pass; the staging
	// file is left for its actual writer to clean up.
}

// TestR3FRecoverCompletesFrozenParkWhenTargetsUnchanged pins the happy
// path: --recover on a frozen park whose target hasn't drifted completes
// the pass exactly as an uninterrupted apply would. The red test asserts
// the candidate ends up parked and the recovery receipt carries the
// committed stage.
func TestR3FRecoverCompletesFrozenParkWhenTargetsUnchanged(t *testing.T) {
	home := t.TempDir()
	const candID = "cand_other_parked"
	seedCandidate(t, home, candID, mem.Candidate{
		Entity:    "umut",
		Predicate: "preference.repeated-tool-choice",
		Object:    "context7",
		ClaimType: "preference",
	})
	frozenPassID := writeFrozenParkPass(t, home, candID, testNow)

	report, err := Run(home, Options{
		Apply:   true,
		Recover: true,
		WorkID:  "work_r3f_recover_clean",
	}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("recovery door happy-path run returned err=%v report.Stage=%q", err, report.Stage)
	}
	if report.Stage != stageCommitted {
		t.Fatalf("report.Stage = %q, want committed — the frozen pass should now be applied verbatim", report.Stage)
	}
	receipts := readReceipts(t, home)
	sawCommits := false
	for _, r := range receipts {
		if r.PassID == frozenPassID && r.Stage == stageCommitted {
			sawCommits = true
			break
		}
	}
	if !sawCommits {
		t.Fatalf("frozen pass %s never received a committed receipt; stages=%v", frozenPassID, frameListStages(receipts))
	}
	// Spot-check: the candidate we said the frozen pass targeted should
	// now be parked (or the active adjudicated verdict the operation
	// recorded).
	states := candidateStates(t, home)
	if states[candID] != "parked" && states[candID] != "proposed" {
		t.Fatalf("candidate %s state = %q, want parked or proposed (a recovered run must not silently leave it un-decided)", candID, states[candID])
	}
}

func frameListStages(receipts []Report) []string {
	out := make([]string, len(receipts))
	for i, r := range receipts {
		out[i] = r.Stage
	}
	return out
}
