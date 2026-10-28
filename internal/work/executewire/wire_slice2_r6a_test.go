// Slice 2 R6A falsifier (work_r6a_slice2_bind_20260910).
//
// Pinning the failure-path outcome after slice 2 replays onto the
// heartbeat-truth-launch-evidence base: a non-Claude adapter that crashes
// mid-run mints a RunAttempt (workspace/command/transcript) AND returns an
// error. The wire must close the envelope as terminal failed with the
// minted ratt id on the envelope's RunAttemptIDs, and the canonical finalize
// must NOT refuse with "authority generation is missing (pairs=0)".
//
// RED with only cbe86acdd applied: FinalizeGovernedHarnessRun refuses because
// MintLaunchAuthority did not yet seal a same-state event attaching the
// (ActionIntent, PolicyDecision) pair, so the close-time authority gate
// finds no event to bind the RunAttempt and the envelope stays in
// `running` with no ratt id.
//
// GREEN after 1acfa760c replay: MintLaunchAuthority seals the attaching
// event, the authority gate resolves the attempt, FinalizeGovernedHarnessRun
// closes the envelope as failed with the ratt id attached.
//
// Packet: docs_private/plan/packets/R6A-slice2-authority-binding-20260910.md
// constraint #5: "cmd/compound net 0; docs/architecture/status.md only if a
// test demands it" — no arch surface moves.
package executewire

import (
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// TestAdapterErrorLeavesTheEnvelopeFailedWithTheAttemptAttached — the
// failure-path contract the base commits to and slice 2 must honor. Drives
// the wire through a non-Claude adapter (no GovernedPermissionAdapter
// surface, so no ExecutionPosture is stamped on the attempt) that mints a
// real RunAttempt, attaches a transcript, marks it failed, and returns an
// error. Asserts:
//   - envelope.State == "failed" (terminal close happened)
//   - envelope.RunAttemptIDs contains the minted ratt id
//   - finalize did NOT refuse with "authority generation is missing"
//
// RED on a tree with only cbe86acdd: the close-time authority gate refuses
// because no same-state event attaches the launch (act_, pol_) pair, so
// the envelope stays in `running` and the RunAttemptIDs stay empty.
// GREEN after 1acfa760c: the sealed event lets the gate resolve the attempt
// and the canonical finalize closes the envelope as failed.
func TestAdapterErrorLeavesTheEnvelopeFailedWithTheAttemptAttached(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifest(t, homePath, "capable")
	seedExecuteStageGrant(t, homePath)
	seedExecuteStageCapacityRow(t, homePath, 30, 2*time.Minute, now)
	capacityTestConfig(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const workID = "work_slice2_r6a_failure_attached"
	seedExecuteStageReadyWorkWithDelivery(t, store, workID, homePath)
	adapter := installPartialFailureAdapter(t, homePath, "claude_minimax")
	defect4LaunchSeed(t, store, workID, "actor:slice2-r6a", now)

	rattID, launchErr := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "slice2 r6a failure", "actor:slice2-r6a", now)
	if launchErr == nil {
		t.Fatalf("LaunchViaActuator returned nil error; the adapter failed and the caller must learn about it")
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
	if adapter.mintedID == "" {
		t.Fatalf("fixture did not mint a RunAttempt; the assertion below would be vacuous")
	}
	if rattID != adapter.mintedID {
		t.Fatalf("wire returned run attempt id %q, want the minted %q", rattID, adapter.mintedID)
	}

	after, err := store.Get(workID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if after.State != "failed" {
		t.Fatalf("envelope state = %q, want failed (the canonical finalize must terminalize the envelope on a non-success RunAttempt after the slice 2 fix; pre-fix the gate refused and the envelope stayed running)", after.State)
	}
	bound := false
	for _, id := range after.RunAttemptIDs {
		if id == adapter.mintedID {
			bound = true
			break
		}
	}
	if !bound {
		t.Fatalf("envelope RunAttemptIDs = %v does NOT contain the minted failed attempt %s; the slice 2 fix must attach the partial-evidence id, not just name it in the close summary", after.RunAttemptIDs, adapter.mintedID)
	}
	if strings.Contains(launchErr.Error(), "authority generation is missing") {
		t.Fatalf("LaunchViaActuator returned a finalize refusal carrying \"authority generation is missing\"; the close-time authority gate MUST bind the attempt from the sealed launch-authority event, not refuse with pairs=0")
	}
}
