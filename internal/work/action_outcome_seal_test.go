package work

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"compound/internal/envelope"
)

// Frozen synthetic reservation emitted by the unmodified base implementation
// a6ce4ace3c81ca30066adea1b30c7feb41c28f5f using an external Go test overlay.
// It must NOT be regenerated through the current input struct: that would hide
// a compatibility regression caused by adding a zero-valued JSON field.
func TestPendingOutcomePreservesFrozenLegacyReservationDigests(t *testing.T) {
	body, err := os.ReadFile("testdata/legacy_index_lock_reservation_v02.json")
	if err != nil {
		t.Fatal(err)
	}
	var legacy receiptedActionReservation
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatal(err)
	}
	if err := legacy.validate(); err != nil {
		t.Fatalf("legacy reservation became unreadable: %v", err)
	}
	at, err := time.Parse(time.RFC3339Nano, legacy.AuthorizedAt)
	if err != nil {
		t.Fatal(err)
	}
	next, err := newReceiptedActionReservation(legacy.WorkID, *legacy.Input, at)
	if err != nil {
		t.Fatal(err)
	}
	if !receiptedActionReservationEqual(legacy, next) {
		t.Fatal("legacy transaction or reauthorization digest changed")
	}
}

func TestPendingReceiptedActionSealsActualFailureNotAnticipatedSuccess(t *testing.T) {
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running", OutcomePending: true, Outcome: "blocked", Summary: "awaiting filesystem outcome", MoveType: "control_plane_repair"}
	at := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, in, at, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReservedReceiptedAction(w.ID, in, at); err == nil {
		t.Fatal("published pending outcome")
	}
	final := in
	final.OutcomePending, final.Outcome, final.Changed, final.Summary = false, "failed", true, "partial filesystem change retained for reconciliation"
	final.FailureCause = &envelope.FailureCause{SurfaceOutcome: "recovery incomplete", Mechanism: "post-move identity mismatch"}
	if err := s.SealReceiptedActionOutcome(w.ID, in, final, at); err != nil {
		t.Fatal(err)
	}
	if err := s.SealReceiptedActionOutcome(w.ID, in, final, at); err != nil {
		t.Fatalf("exact seal replay: %v", err)
	}
	changed := final
	changed.Outcome = "completed"
	if err := s.SealReceiptedActionOutcome(w.ID, in, changed, at); err == nil {
		t.Fatal("resealed different outcome")
	}
	result, err := s.RecordReservedReceiptedAction(w.ID, final, at)
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.Outcome != "failed" || !result.Receipt.StateDelta.Changed {
		t.Fatal("lost partial-failure truth")
	}
}

func TestSealReceiptedActionCannotChangeAuthority(t *testing.T) {
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	in := ReceiptedActionInput{Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running", OutcomePending: true, Outcome: "blocked", Summary: "pending", MoveType: "control_plane_repair"}
	at := fixedNow.Add(2 * time.Minute)
	if err := s.ReserveReceiptedAction(w.ID, in, at, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	next := in
	next.OutcomePending = false
	next.Outcome = "completed"
	next.Action.Target.Path = "/different-target"
	if err := s.SealReceiptedActionOutcome(w.ID, in, next, at); err == nil {
		t.Fatal("changed target accepted")
	}
	next = in
	next.OutcomePending = false
	next.Outcome = "completed"
	next.ExpectedWorkState = "verifying"
	if err := s.SealReceiptedActionOutcome(w.ID, in, next, at); err == nil {
		t.Fatal("changed work admission accepted")
	}
}
