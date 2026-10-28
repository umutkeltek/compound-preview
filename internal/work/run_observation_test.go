package work

import (
	"os"
	"path/filepath"
	"testing"

	"compound/internal/envelope"
)

func TestObserveRunAttachesTraceAndMovesToVerifying(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := runningWork(t, s, "observe run attaches trace")
	trace := writeTrace(t, s, "trace_observe_good", w.ID)
	beforeRuns := append([]string{}, w.ActionIntentIDs...)
	result, err := s.ObserveRun(w.ID, ObserveRunInput{Actor: "codex", TraceID: trace.ID}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "observed" || result.WorkID != w.ID || result.TraceID != trace.ID || result.State != "verifying" {
		t.Fatalf("observe result = %+v", result)
	}
	if result.ResultPath == "" {
		t.Fatalf("observe result missing path: %+v", result)
	}
	if _, err := os.Stat(result.ResultPath); err != nil {
		t.Fatalf("observe result not recorded: %v", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "verifying" || !contains(after.VerificationIDs, trace.ID) {
		t.Fatalf("work not moved to verifying with trace evidence: %+v", after)
	}
	if len(after.ActionIntentIDs) != len(beforeRuns)+1 {
		t.Fatalf("observe-run should add only advance action to run ids: before=%v after=%v", beforeRuns, after.ActionIntentIDs)
	}
	if after.IDs.TransitionID != "" {
		t.Fatalf("observe-run must not close work: %+v", after.IDs)
	}
}

func TestObserveRunRejectsMismatchedTraceWithoutMutation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := runningWork(t, s, "observe run rejects mismatched trace")
	trace := writeTrace(t, s, "trace_observe_wrong", "work_other")
	if _, err := s.ObserveRun(w.ID, ObserveRunInput{Actor: "codex", TraceID: trace.ID}, fixedNow); err == nil {
		t.Fatal("mismatched trace should be rejected")
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || contains(after.VerificationIDs, trace.ID) {
		t.Fatalf("mismatched trace mutated work: %+v", after)
	}
}

func TestObserveRunBlockedByPolicyDoesNotMutate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := runningWork(t, s, "policy hold blocks observe run")
	trace := writeTrace(t, s, "trace_observe_blocked", w.ID)
	baselineRuns := append([]string{}, w.ActionIntentIDs...)
	baselinePolicy := w.PolicyDecisionID
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	policyYAML := []byte(`class_outcome:
  local_reversible:
    outcome: hold
`)
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), policyYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveRun(w.ID, ObserveRunInput{Actor: "codex", TraceID: trace.ID}, fixedNow); err == nil {
		t.Fatal("observe-run should be blocked by policy hold")
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || len(after.ActionIntentIDs) != len(baselineRuns) || after.PolicyDecisionID != baselinePolicy || contains(after.VerificationIDs, trace.ID) {
		t.Fatalf("blocked observe-run mutated work: %+v", after)
	}
}

func runningWork(t *testing.T, s *Store, predicate string) envelope.WorkEnvelope {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: predicate}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	running, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return running
}

func writeTrace(t *testing.T, s *Store, id, workID string) envelope.TraceEnvelope {
	t.Helper()
	trace := envelope.TraceEnvelope{
		Header: newHeader(envelope.SVTraceEnvelope, id, fixedNow, workID, "", "private", "tier1"),
		Source: envelope.TraceSource{
			System:     "test",
			SourceKind: "unit",
			EventClass: "run_observation",
		},
		Actor:           "codex",
		StartedAt:       fixedNow.Format("2006-01-02T15:04:05Z07:00"),
		EndedAt:         fixedNow.Format("2006-01-02T15:04:05Z07:00"),
		MutationClass:   "local_reversible",
		Summary:         "observed test run",
		ToolsModelsUsed: []string{},
		FilesTouched:    []string{},
		Outputs:         []string{"trace-output:test"},
		Cost:            envelope.ReceiptCost{},
	}
	if err := trace.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := s.writeJSON(s.envPath(trace.ID), &trace); err != nil {
		t.Fatal(err)
	}
	return trace
}
