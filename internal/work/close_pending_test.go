package work

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

func closePendingTestPath(s *Store, workID string) string {
	return s.workPath(workID) + ".close-pending"
}

func countCloseActionID(ids []string, want string) int {
	count := 0
	for _, id := range ids {
		if id == want {
			count++
		}
	}
	return count
}

func TestCloseNonWholeSecondPersistsOneExactTimestamp(t *testing.T) {
	s := newStore(t)
	createdAt := time.Date(2026, 8, 30, 1, 2, 3, 456789123, time.UTC)
	const contextPackID = "ctx_close_timestamp_identity"
	writeContextPack(t, s, contextPackID)
	w, err := s.Create(CreateInput{
		Owner:            "close-timestamp-identity",
		SuccessPredicate: "receipt transition and lifecycle event share one close instant",
		ContextPackID:    contextPackID,
	}, createdAt.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	closed, err := s.Close(w.ID, CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "close-timestamp-identity",
		Summary:  "persist one exact close timestamp",
	}, createdAt)
	if err != nil {
		t.Fatalf("close at non-whole-second instant: %v", err)
	}
	if closed.Receipt.CreatedAt != closed.Transition.CreatedAt {
		t.Fatalf("receipt created_at %q differs from transition created_at %q", closed.Receipt.CreatedAt, closed.Transition.CreatedAt)
	}
	for name, got := range map[string]string{
		"work":       closed.Work.IDs.ContextPackID,
		"receipt":    closed.Receipt.IDs.ContextPackID,
		"transition": closed.Transition.IDs.ContextPackID,
	} {
		if got != contextPackID {
			t.Fatalf("%s context_pack_id = %q, want %q", name, got, contextPackID)
		}
	}
	receiptAt, err := time.Parse(time.RFC3339Nano, closed.Receipt.CreatedAt)
	if err != nil {
		t.Fatalf("parse receipt created_at: %v", err)
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	closing := events[len(events)-1]
	closingAt, err := parseLifecycleTS(closing.TS)
	if err != nil {
		t.Fatalf("parse closing lifecycle timestamp: %v", err)
	}
	if !closingAt.Equal(receiptAt) {
		t.Fatalf("closing event timestamp %s differs from receipt timestamp %s", closingAt, receiptAt)
	}
	if _, err := os.Stat(closePendingTestPath(s, w.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful close left recovery frame: %v", err)
	}
}

func TestCloseCrashAfterFirstEnvelopeWithNoPreparedAttemptsUsesOneGeneration(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "close-pending-zero-attempt",
		SuccessPredicate: "an interrupted zero-attempt close resumes one exact generation",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.RunAttemptIDs) != 0 {
		t.Fatalf("setup RunAttemptIDs = %v, want none", w.RunAttemptIDs)
	}

	input := CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "whoever",
		Summary:  "close one frozen zero-attempt generation",
	}
	closeAt := fixedNow.Add(5*time.Minute + 456789123*time.Nanosecond)
	plan, err := s.ClosePlan(w.ID, closeAt, ClosePlanOptions{
		Actor:    input.Actor,
		Outcome:  input.Outcome,
		MoveType: input.MoveType,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReceiptID := idFor("rec_", w.ID, ts(closeAt))
	wantTransitionID := idFor("tr_", w.ID, ts(closeAt))

	previous := closeAfterFirstEnvelopeWrite
	t.Cleanup(func() { closeAfterFirstEnvelopeWrite = previous })
	closeAfterFirstEnvelopeWrite = func() { panic("injected crash after first close envelope") }
	crashed := false
	func() {
		defer func() {
			if recover() != nil {
				crashed = true
			}
		}()
		_, _ = s.Close(w.ID, input, closeAt)
	}()
	closeAfterFirstEnvelopeWrite = previous
	if !crashed {
		t.Fatal("close did not reach the first-envelope crash seam")
	}

	interrupted, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.State != "ready" {
		t.Fatalf("interrupted work state = %q, want ready", interrupted.State)
	}
	if _, err := os.Stat(closePendingTestPath(s, w.ID)); err != nil {
		t.Errorf("interrupted close recovery frame: %v", err)
	}
	pending, present, err := s.loadClosePending(w.ID)
	if err != nil || !present {
		t.Fatalf("load interrupted close recovery frame: present=%v err=%v", present, err)
	}
	wantCreatedAt := closeAt.UTC().Format(time.RFC3339Nano)
	for name, got := range map[string]string{
		"completed_at": pending.CompletedAt,
		"action":       pending.Action.CreatedAt,
		"decision":     pending.Decision.CreatedAt,
		"receipt":      pending.Receipt.CreatedAt,
		"transition":   pending.Transition.CreatedAt,
	} {
		if got != wantCreatedAt {
			t.Fatalf("interrupted %s timestamp = %q, want %q", name, got, wantCreatedAt)
		}
	}

	reopened, err := Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := reopened.Close(w.ID, input, closeAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("resume exact zero-attempt close: %v", err)
	}
	if got := countCloseActionID(closed.Work.ActionIntentIDs, plan.ActionIntent.ID); got != 1 {
		t.Fatalf("frozen close ActionIntent occurrences = %d, want 1; ids=%v", got, closed.Work.ActionIntentIDs)
	}
	if closed.Work.PolicyDecisionID != plan.Decision.ID {
		t.Fatalf("close policy id = %s, want frozen %s", closed.Work.PolicyDecisionID, plan.Decision.ID)
	}
	if closed.Receipt.ID != wantReceiptID || closed.Transition.ID != wantTransitionID {
		t.Fatalf("resumed proof ids = (%s,%s), want frozen (%s,%s)", closed.Receipt.ID, closed.Transition.ID, wantReceiptID, wantTransitionID)
	}
	if _, err := os.Stat(closePendingTestPath(reopened, w.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful retry left close recovery frame: %v", err)
	}
}

func TestCloseZeroWriteEventFailureExactRetryCompletesSameGeneration(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "close-pending-event-write",
		SuccessPredicate: "a zero-byte event failure preserves one retryable close generation",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.RunAttemptIDs) != 0 {
		t.Fatalf("setup RunAttemptIDs = %v, want none", w.RunAttemptIDs)
	}

	input := CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "whoever",
		Summary:  "recover exact zero-write close generation",
	}
	closeAt := fixedNow.Add(5 * time.Minute)
	plan, err := s.ClosePlan(w.ID, closeAt, ClosePlanOptions{
		Actor:    input.Actor,
		Outcome:  input.Outcome,
		MoveType: input.MoveType,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReceiptID := idFor("rec_", w.ID, ts(closeAt))
	wantTransitionID := idFor("tr_", w.ID, ts(closeAt))
	beforeWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	beforeLog, err := s.events.ReadWorkLog()
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected zero-byte close event write failure")
	previousWriter := writeWorkEvent
	t.Cleanup(func() { writeWorkEvent = previousWriter })
	writeWorkEvent = func(_ *os.File, _ []byte) (int, error) {
		return 0, injected
	}
	_, closeErr := s.Close(w.ID, input, closeAt)
	writeWorkEvent = previousWriter
	if !errors.Is(closeErr, injected) {
		t.Fatalf("close error = %v, want injected event failure", closeErr)
	}

	afterWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterWork, beforeWork) {
		t.Error("zero-write event failure changed the canonical WorkEnvelope")
	}
	afterLog, err := s.events.ReadWorkLog()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterLog, beforeLog) {
		t.Error("zero-write event failure changed the canonical lifecycle log")
	}
	if _, err := os.Stat(closePendingTestPath(s, w.ID)); err != nil {
		t.Errorf("zero-write failure recovery frame: %v", err)
	}

	reopened, err := Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := reopened.Close(w.ID, input, closeAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("exact retry after reopen: %v", err)
	}
	if got := countCloseActionID(closed.Work.ActionIntentIDs, plan.ActionIntent.ID); got != 1 {
		t.Fatalf("frozen close ActionIntent occurrences = %d, want 1; ids=%v", got, closed.Work.ActionIntentIDs)
	}
	if closed.Work.PolicyDecisionID != plan.Decision.ID {
		t.Fatalf("close policy id = %s, want frozen %s", closed.Work.PolicyDecisionID, plan.Decision.ID)
	}
	if closed.Receipt.ID != wantReceiptID || closed.Transition.ID != wantTransitionID {
		t.Fatalf("retry proof ids = (%s,%s), want frozen (%s,%s)", closed.Receipt.ID, closed.Transition.ID, wantReceiptID, wantTransitionID)
	}
	events, err := reopened.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	terminalEvents := 0
	for _, event := range events {
		if event.To == input.Outcome {
			terminalEvents++
		}
	}
	if terminalEvents != 1 {
		t.Fatalf("terminal close events = %d, want exactly 1; events=%+v", terminalEvents, events)
	}
	if _, err := os.Stat(closePendingTestPath(reopened, w.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful retry left close recovery frame: %v", err)
	}
}
