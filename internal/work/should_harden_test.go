package work

import (
	"testing"
)

// runToCloseable creates a work (optionally carrying a target surface) and walks
// it ready -> claimed -> running so it can be closed to a terminal outcome.
func runToCloseable(t *testing.T, s *Store, targetSurface, predicate string) string {
	t.Helper()
	w, err := s.Create(CreateInput{
		Owner:            "compound-harness",
		Actor:            "operator-test",
		SuccessPredicate: predicate,
		TargetSurface:    targetSurface,
	}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.TargetSurface != targetSurface {
		t.Fatalf("create should preserve target_surface %q, got %q", targetSurface, w.TargetSurface)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running"}, fixedNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	return w.ID
}

// TestShouldHardenStampedOnlyOnAcceptedHarnessEditClose is falsifier 6: an
// ACCEPTED (completed) close of an explicit harness-edit envelope (one with a
// TargetSurface) stamps SecondOrder.ShouldHarden=true ONLY when it carries a
// bench-compare accept proof ref — "completed" cannot self-certify a harness edit;
// a rejected/failed close of the same envelope stamps false but STILL emits Receipt
// + Transition; and an ordinary work with no TargetSurface never hardens on close.
func TestShouldHardenStampedOnlyOnAcceptedHarnessEditClose(t *testing.T) {
	t.Parallel()
	t.Run("accepted harness-edit close hardens", func(t *testing.T) {
		s := newStore(t)
		id := runToCloseable(t, s, "capability_edges", "apply the accepted harness edit")
		res, err := s.Close(id, CloseInput{
			Outcome:   "completed",
			MoveType:  "control_plane_repair",
			Summary:   "harness edit accepted and applied",
			Changed:   true,
			ProofRefs: []string{"gate:harness-edit-accepted", "bench_compare_accept:capability-suite"},
		}, fixedNow)
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if !res.Transition.SecondOrder.ShouldHarden {
			t.Fatalf("accepted harness-edit close must stamp should_harden=true: %+v", res.Transition.SecondOrder)
		}
	})

	t.Run("rejected harness-edit close does not harden but still records", func(t *testing.T) {
		s := newStore(t)
		id := runToCloseable(t, s, "capability_edges", "reject the harness edit")
		res, err := s.Close(id, CloseInput{
			Outcome:   "failed",
			MoveType:  "control_plane_repair",
			Summary:   "harness edit rejected on review",
			ProofRefs: []string{"gate:harness-edit-rejected"},
		}, fixedNow)
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if res.Transition.SecondOrder.ShouldHarden {
			t.Fatalf("rejected harness-edit close must NOT harden: %+v", res.Transition.SecondOrder)
		}
		// A rejection is still logged: Receipt + Transition are emitted.
		if res.Receipt.ID == "" || res.Transition.ID == "" {
			t.Fatalf("rejected harness-edit close must still emit Receipt+Transition: %+v", res)
		}
	})

	t.Run("ordinary work never hardens on close", func(t *testing.T) {
		s := newStore(t)
		id := runToCloseable(t, s, "", "ordinary work with no target surface")
		res, err := s.Close(id, CloseInput{
			Outcome:   "completed",
			MoveType:  "control_plane_repair",
			Summary:   "ordinary control-plane repair",
			Changed:   true,
			ProofRefs: []string{"gate:ordinary"},
		}, fixedNow)
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if res.Transition.SecondOrder.ShouldHarden {
			t.Fatalf("ordinary (no target_surface) close must not harden: %+v", res.Transition.SecondOrder)
		}
	})

	// "completed" alone must NOT harden: a bench-report-exists ref is not a bench-compare ACCEPT.
	// This is the gate that stops a self-certified close from marking the harness as hardened.
	t.Run("completed harness-edit close without bench-compare accept proof does not harden", func(t *testing.T) {
		s := newStore(t)
		id := runToCloseable(t, s, "capability_edges", "apply the harness edit")
		res, err := s.Close(id, CloseInput{
			Outcome:   "completed",
			MoveType:  "control_plane_repair",
			Summary:   "harness edit applied",
			Changed:   true,
			ProofRefs: []string{"gate:harness-edit-accepted", "bench_report:capability-suite"},
		}, fixedNow)
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if res.Transition.SecondOrder.ShouldHarden {
			t.Fatalf("completed harness-edit close without bench_compare accept proof must NOT harden: %+v", res.Transition.SecondOrder)
		}
	})
}
