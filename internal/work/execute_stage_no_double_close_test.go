// work_r6m_seam_posture_20260911 — second defect: the stage double-closes.
//
// Live run evidence (work_r6m_seam_posture_20260911 scratchpad, JSON in
// live-continue-2.json): the seam minted a ratt, FinalizeGovernedHarnessRun
// (the canonical finalize path R6A replayed) closed the envelope as failed
// with the attempt attached, AND THEN execute_stage.launchStub's failure
// branch tried its own Close and reported the refusal as "executed but
// failed to record failure on work_…". The envelope WAS failed (running
// → failed), the receipt carried the real proof ref, and the second Close
// is the noise that hides the truth.
//
// RED shape (this file pins the bug before any code change):
//   - launchStub returns an adapter error WITH a non-empty ratt id AND
//     drives the canonical finalize equivalent (closing the envelope as
//     failed with the attempt attached) before returning — the production
//     shape FinalizeGovernedHarnessRun produces today.
//   - The stage's Human line MUST say the envelope is failed and name the
//     ratt id, NOT contain the "executed but failed to record failure"
//     text and NOT pretend the second Close is the failure recorder.
//
// GREEN: stage detects that the envelope has already moved past `running`
// (FinalizeGovernedHarnessRun owns the close), records the outcome as a
// launch_failed skip with the attempt attached, and never attempts the
// second Close.
package work

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestExecuteStageDoesNotCloseTwiceAfterFinalizeFailed — the stage's
// failure branch must NOT attempt a second Close when the envelope has
// already moved past `running`. Today the stage's Close call refuses with
// "close=work … already failed", the stage reports that refusal as the
// failure (the live-run noise), and the Human line says "executed but
// failed to record failure on work_…" — which is wrong, the failure WAS
// recorded, by the canonical finalize the stage is shadowing.
//
// RED today: the Human line contains "executed but failed to record
// failure" and `res.Skipped` is `launch_failed` only by accident (the
// stage sets it after the refused Close).
func TestExecuteStageDoesNotCloseTwiceAfterFinalizeFailed(t *testing.T) {
	s := newStore(t)
	const workID = "work_no_double_close"
	const actor = "codex"
	seedReadyExecuteWork(t, s, workID)
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openPickGates(t)

	now := fixedNow.Add(2 * time.Minute)
	// Deterministic but well-formed ratt id (envelope validation rejects
	// free-form strings on Close). The shape matches runattempt.Mint's
	// `ratt_<UTC>_<hex>` so the canonical-finalize close accepts it.
	rattID := "ratt_20260628T120200Z_aabbccdd"
	adapterErr := errors.New("harness subprocess exited 137 (killed)")

	// launchStub is the production seam — in tests it's whatever the
	// fixture installs. We model the R6A canonical finalize shape:
	//   1. the adapter errored
	//   2. FinalizeGovernedHarnessRun already closed the envelope as
	//      failed with the minted ratt id attached (production proof
	//      refs come from the adapter output, NOT "execute_stage_launch_failed")
	//   3. launchStub returns the ratt id and the error to the stage
	//
	// The stage's pre-fix behavior: try its own Close with proof ref
	// "execute_stage_launch_failed" (the hard-coded value at the call
	// site), get refused because the existing terminal receipt carries a
	// DIFFERENT proof ref (the live-run shape), and report the refusal
	// as the failure.
	launchStub = func(store *Store, wid, _, _, _, _ string, _ time.Time) (string, error) {
		if wid != workID {
			return "", &executeStageBadWorkIDErr{got: wid}
		}
		// Mirror FinalizeGovernedHarnessRun: Close as failed with a
		// transcript-derived proof ref (NOT the "execute_stage_launch_failed"
		// string the stage hard-codes later — that's the live-run
		// divergence that drives the second-Close refusal).
		// No RunAttemptIDs: the production finalize attaches the ratt id
		// because it minted one earlier. This fixture skips the mint; the
		// stage still receives the ratt id from launchStub's return value
		// and records it in result.RunAttemptIDs as partial evidence.
		if _, err := store.Close(wid, CloseInput{
			Outcome:   "failed",
			MoveType:  "audit_only",
			Actor:     actor,
			Summary:   "harness claude_minimax failed (canonical finalize)",
			Changed:   true,
			ProofRefs: []string{"transcript://partial-evidence"},
		}, now); err != nil {
			return "", err
		}
		return rattID, adapterErr
	}

	res, err := s.Continue(now.Add(time.Minute), ContinueOptions{Actor: actor})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if res.WorkID != workID {
		t.Fatalf("work_id = %q, want %s (the stage picked up the wrong envelope); result=%+v", res.WorkID, workID, res)
	}
	if strings.Contains(res.Human, "executed but failed to record failure") {
		t.Fatalf("human line %q contains the pre-fix `failed to record failure` text; the canonical finalize already closed the envelope, the stage must NOT attempt a second Close and must NOT report the refusal as the failure", res.Human)
	}
	if !strings.Contains(res.Human, "failed") {
		t.Fatalf("human line %q must say the envelope failed; the operator needs the truth, not the silence of a swallowed refusal", res.Human)
	}
	if !strings.Contains(res.Human, rattID) {
		t.Fatalf("human line %q must name the minted ratt id %q so the operator can find the partial evidence; the pre-fix stage names it inside the `failed to record failure` clause", res.Human, rattID)
	}
	if res.Skipped != "launch_failed" {
		t.Fatalf("skipped = %q, want launch_failed (the adapter errored; the stage's outcome is launch_failed either way); result=%+v", res.Skipped, res)
	}
	if len(res.RunAttemptIDs) != 1 || res.RunAttemptIDs[0] != rattID {
		t.Fatalf("run_attempt_ids = %v, want [%s]; the stage must record the partial-evidence id even when finalize already closed the envelope", res.RunAttemptIDs, rattID)
	}
	// The envelope for the driven fixture MUST remain in `failed` — the
	// canonical finalize owns that state and the stage must not touch it
	// (a second Close attempt would refuse).
	after, err := s.Get(workID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.State != "failed" {
		t.Fatalf("envelope state = %q, want failed (the canonical finalize's terminal state; the stage must NOT touch it again)", after.State)
	}
}
