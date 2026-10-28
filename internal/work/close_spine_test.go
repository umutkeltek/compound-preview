package work

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// Spine close-path hardening falsifiers: idempotent re-close, derived
// future-behavior stake, act_-only apex action refs, and never persisting an
// envelope that fails the current schema.

func closeSpineWork(t *testing.T, s *Store, owner string, at time.Time) string {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: owner, SuccessPredicate: "spine close hardening"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running"}, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", VerificationID: "ver_" + w.ID}, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return w.ID
}

// closeSpineWorkNoVerify reaches verifying without attaching any verification,
// artifact, or proof ids, so a later close has zero effective-change evidence.
func closeSpineWorkNoVerify(t *testing.T, s *Store, owner string, at time.Time) string {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: owner, SuccessPredicate: "spine close hardening"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running"}, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying"}, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return w.ID
}

// A same-outcome re-close must return the EXISTING terminal proof pair — the
// closing receipt is not last-writer-wins.
func TestCloseIsIdempotentOnSameOutcome(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := closeSpineWork(t, s, "spine-idempotent", fixedNow)
	in := CloseInput{Outcome: "completed", MoveType: "outward_delta", Summary: "close once", Changed: true, ProofRefs: []string{"check:once"}}
	first, err := s.Close(id, in, fixedNow.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Close(id, in, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Receipt.ID != first.Receipt.ID || second.Transition.ID != first.Transition.ID {
		t.Fatalf("re-close re-minted the proof pair: first rec=%s tr=%s, second rec=%s tr=%s",
			first.Receipt.ID, first.Transition.ID, second.Receipt.ID, second.Transition.ID)
	}
	w, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if w.IDs.TransitionID != first.Transition.ID {
		t.Fatalf("spine link moved on re-close: %s, want %s", w.IDs.TransitionID, first.Transition.ID)
	}

	// A re-close carrying NEW information is not a replay: it must refuse with
	// a pointer to the existing terminal receipt, not silently discard it.
	if _, err := s.Close(id, CloseInput{
		Outcome: "completed", MoveType: "rule_delta", Summary: "different move",
		Changed: true, ProofRefs: []string{"check:other"},
	}, fixedNow.Add(6*time.Minute)); err == nil || !strings.Contains(err.Error(), "not a replay") {
		t.Fatalf("re-close with a different move_type must refuse as not-a-replay, got %v", err)
	}
	if _, err := s.Close(id, CloseInput{
		Outcome: "completed", MoveType: "outward_delta", Summary: "new proof",
		Changed: true, ProofRefs: []string{"check:brand-new"},
	}, fixedNow.Add(7*time.Minute)); err == nil || !strings.Contains(err.Error(), "new proof ref") {
		t.Fatalf("re-close with a new proof ref must refuse with a pointer, got %v", err)
	}
}

// future_behavior_changed is derived from the effective change, never from the
// move class alone: a no-op close must not stake a claim it cannot evidence.
func TestCloseDerivesFutureBehaviorChangedFromEffectiveChange(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	changedID := closeSpineWork(t, s, "spine-changed", fixedNow)
	changed, err := s.Close(changedID, CloseInput{
		Outcome: "completed", MoveType: "outward_delta", Summary: "real change",
		Changed: true, ProofRefs: []string{"check:real"},
	}, fixedNow.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Transition.SecondOrder.FutureBehaviorChanged {
		t.Fatal("a genuinely changed close must stake future_behavior_changed")
	}

	// The only schema-admissible no-effective-change close: failed, with
	// Changed=false and no proofs (every completed non-audit close is forced
	// to carry change evidence by receipt validation).
	noopID := closeSpineWorkNoVerify(t, s, "spine-noop", fixedNow.Add(10*time.Minute))
	noop, err := s.Close(noopID, CloseInput{
		Outcome: "failed", MoveType: "outward_delta", Summary: "no effective change",
		Changed: false,
	}, fixedNow.Add(14*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if noop.Transition.SecondOrder.FutureBehaviorChanged {
		t.Fatal("a close with no effective change must NOT stake future_behavior_changed")
	}
}

// The apex action_refs carry only ActionIntent ids — legacy prun_* run refs
// are run history, not actions.
func TestCloseFiltersLegacyRunRefsFromActionRefs(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := closeSpineWork(t, s, "spine-actionrefs", fixedNow)
	var w envelope.WorkEnvelope
	if err := s.readJSON(s.workPath(id), &w); err != nil {
		t.Fatal(err)
	}
	w.ActionIntentIDs = append(w.ActionIntentIDs, "prun_legacy_run_ref")
	if err := s.writeJSON(s.workPath(id), &w); err != nil {
		t.Fatal(err)
	}
	res, err := s.Close(id, CloseInput{
		Outcome: "completed", MoveType: "outward_delta", Summary: "close with legacy run ref",
		Changed: true, ProofRefs: []string{"check:filter"},
	}, fixedNow.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Transition.ActionRefs) == 0 {
		t.Fatal("close-minted transition must bind at least the close's own action intent")
	}
	for _, ref := range res.Transition.ActionRefs {
		if !strings.HasPrefix(ref, "act_") {
			t.Fatalf("apex action_refs carried a non-action ref %q: %v", ref, res.Transition.ActionRefs)
		}
	}
}

// The lifecycle never persists an envelope that fails the current schema.
func TestCloseRefusesToPersistInvalidEnvelope(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := closeSpineWork(t, s, "spine-invalid-persist", fixedNow)
	var w envelope.WorkEnvelope
	if err := s.readJSON(s.workPath(id), &w); err != nil {
		t.Fatal(err)
	}
	w.Owner = "" // fails WorkEnvelope.Validate (owner is required)
	if err := s.writeJSON(s.workPath(id), &w); err != nil {
		t.Fatal(err)
	}
	_, err := s.Close(id, CloseInput{
		Outcome: "completed", MoveType: "outward_delta", Summary: "close over a corrupted envelope",
		Changed: true, ProofRefs: []string{"check:corrupt"},
	}, fixedNow.Add(4*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("close must refuse to persist an invalid envelope, got %v", err)
	}
}
