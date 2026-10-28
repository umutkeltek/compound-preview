package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
)

// Slice 3 (product-loop-momentum 2026-09-08): admitWorkLocked MUST
// persist the transition event BEFORE the WorkEnvelope, so a crash
// between the two writes leaves either (a) neither — recoverable by
// re-running admit, or (b) the event with no envelope — recovery
// sees the orphan and rebuilds. The previous order persisted the
// envelope first; a crash there left a fully formed candidate with
// no transition record, which recovery could not detect because
// nothing marked the gap.
//
// TestAdmitCreatePlanWritesEventBeforeEnvelope proves the new ordering
// by inspecting the on-disk event log AND the work file's mtime /
// state after admit. We block the work-file write (the LAST step) and
// confirm that the transition event is already present in the event
// log; the next attempt can read it as an orphan marker.

func TestAdmitCreatePlanWritesEventBeforeEnvelope(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_slice3_event_first",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "slice-3 event-before-envelope",
	}, fixedNow, CreatePlanOptions{Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "create_policy_allowed" {
		t.Fatalf("plan not allowed: %+v", plan.Policy)
	}

	// Block the work-file write by making the work directory read-only.
	// The event log lives at COMPOUND_HOME/events/, which is a different
	// directory; the block isolates the envelope write.
	workDir := filepath.Join(s.home, "work", "work")
	if err := os.Chmod(workDir, 0o555); err != nil {
		t.Fatalf("chmod work dir read-only: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(workDir, 0o755)
	})

	_, admitErr := s.AdmitCreatePlanPath(plan.PlanPath, fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
	if admitErr == nil {
		t.Fatal("expected work-file stage error, got nil")
	}

	// The work envelope must NOT be visible as a file.
	if _, err := os.Stat(s.workPath(plan.WorkID)); !os.IsNotExist(err) {
		t.Fatalf("work file should not exist after failed stage: %v", err)
	}

	// The transition event MUST be durable in the event log. This is
	// the slice-3 invariant: event before envelope. The event's
	// presence lets recovery recognize the orphan and rebuild the
	// candidate from the create-plan, instead of leaving an orphan
	// candidate with no transition record.
	events, err := s.eventsFor(plan.WorkID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("transition event must be present in the event log even when the envelope write fails (slice 3 invariant); events read returned empty")
	}
	last := events[len(events)-1]
	if last.WorkID != plan.WorkID {
		t.Fatalf("event WorkID = %q, want %q", last.WorkID, plan.WorkID)
	}
	if last.To != "proposed" {
		t.Fatalf("event To = %q, want proposed (the initial admit publishes the proposed-state transition)", last.To)
	}
	// The event must reference the freshly minted (action_intent,
	// policy_decision) pair so recovery sees the exact generation.
	hasAction := false
	hasDecision := false
	for _, ref := range last.Attached {
		if ref == plan.ActionIntent.ID {
			hasAction = true
		}
		if ref == plan.Decision.ID {
			hasDecision = true
		}
	}
	if !hasAction || !hasDecision {
		t.Fatalf("transition event must attach the freshly minted (action_intent, policy_decision) pair; has_action=%v has_decision=%v attached=%v", hasAction, hasDecision, last.Attached)
	}

	// Sanity: the durable action_intent and policy_decision are
	// reachable via the eventstore (already covered by
	// TestAdmitCreatePlanEvidenceDurabilityOrdering but re-asserted
	// here because the slice-3 invariant bundles them).
	aiBytes, err := s.eventBytes(plan.ActionIntent.ID)
	if err != nil {
		t.Fatalf("action_intent not durable: %v", err)
	}
	var ai envelope.ActionIntent
	if err := json.Unmarshal(aiBytes, &ai); err != nil {
		t.Fatalf("action_intent unmarshal: %v", err)
	}
	if ai.ID != plan.ActionIntent.ID {
		t.Fatalf("action_intent id mismatch: %q vs %q", ai.ID, plan.ActionIntent.ID)
	}
	pdBytes, err := s.eventBytes(plan.Decision.ID)
	if err != nil {
		t.Fatalf("policy_decision not durable: %v", err)
	}
	var pd envelope.PolicyDecision
	if err := json.Unmarshal(pdBytes, &pd); err != nil {
		t.Fatalf("policy_decision unmarshal: %v", err)
	}
	if pd.ID != plan.Decision.ID {
		t.Fatalf("policy_decision id mismatch: %q vs %q", pd.ID, plan.Decision.ID)
	}
}
