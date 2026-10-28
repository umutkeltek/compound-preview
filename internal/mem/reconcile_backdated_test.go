package mem

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

type backdatedReconcileFixture struct {
	store       *Store
	now         time.Time
	baselineAt  time.Time
	backdatedID string
}

func prepareBackdatedReconcileFixture(t *testing.T) backdatedReconcileFixture {
	t.Helper()

	store := newStore(t)
	closeMemCompletionGate(t, store)
	now := t0.AddDate(0, 0, 2)
	baselineAt := now.Add(-2 * time.Hour)
	claim := reconcileClaim("claim_backdated_fold", 1, "project:api", "runtime", "Go", rfc3339(t0))
	appendReconcileClaim(t, store, claim)
	writeReconcileTransition(t, store, reconcileTransitionInput{
		ID:        "tr_reconcile_baseline",
		CreatedAt: rfc3339(baselineAt),
		Scope:     claim.Entity,
		ClaimRefs: []string{claim.ID},
	})

	first, err := store.SynthesisSweep([]string{"reconcile"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReconcileDismissedCount != 1 {
		t.Fatalf("baseline reconcile dismissed = %d, want 1; report=%+v", first.ReconcileDismissedCount, first)
	}
	checkpoint, err := store.loadReconcileCheckpoint(now)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Cursor == "" {
		t.Fatal("baseline reconcile did not persist its segment cursor")
	}
	if checkpoint.LastEventTime != rfc3339(baselineAt) {
		t.Fatalf("baseline last event time = %q, want %q", checkpoint.LastEventTime, rfc3339(baselineAt))
	}

	backdatedID := "tr_reconcile_backdated"
	writeReconcileTransition(t, store, reconcileTransitionInput{
		ID:        backdatedID,
		CreatedAt: rfc3339(baselineAt.Add(-time.Hour)),
		Scope:     claim.Entity,
		ClaimRefs: []string{claim.ID},
	})
	return backdatedReconcileFixture{
		store:       store,
		now:         now,
		baselineAt:  baselineAt,
		backdatedID: backdatedID,
	}
}

func TestSynthesisReconcileResumedBackdatedEventFoldsOnce(t *testing.T) {
	fixture := prepareBackdatedReconcileFixture(t)

	report, err := fixture.store.SynthesisSweep([]string{"reconcile"}, fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileDismissedCount != 1 {
		t.Fatalf("backdated resumed fold dismissed = %d, want 1; event %s was silently dropped", report.ReconcileDismissedCount, fixture.backdatedID)
	}
	checkpoint, err := fixture.store.loadReconcileCheckpoint(fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Counts["backdated_folded"] != 1 {
		t.Fatalf("checkpoint backdated_folded = %d, want 1", checkpoint.Counts["backdated_folded"])
	}
	if checkpoint.LastEventTime != rfc3339(fixture.baselineAt) {
		t.Fatalf("backdated fold regressed last event time to %q, want %q", checkpoint.LastEventTime, rfc3339(fixture.baselineAt))
	}

	again, err := fixture.store.SynthesisSweep([]string{"reconcile"}, fixture.now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if again.ReconcileDismissedCount != 0 {
		t.Fatalf("steady resumed sweep folded the backdated event again: report=%+v", again)
	}
}

func TestSynthesisReconcileFullRescanDedupsFoldedBackdatedEvent(t *testing.T) {
	fixture := prepareBackdatedReconcileFixture(t)

	folded, err := fixture.store.SynthesisSweep([]string{"reconcile"}, fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if folded.ReconcileDismissedCount != 1 {
		t.Fatalf("backdated prerequisite fold dismissed = %d, want 1", folded.ReconcileDismissedCount)
	}
	checkpoint, err := fixture.store.loadReconcileCheckpoint(fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Cursor = "garbage cursor"
	if err := writeJSON(fixture.store.reconcileCheckpointPath(), checkpoint); err != nil {
		t.Fatal(err)
	}

	rescanned, err := fixture.store.SynthesisSweep([]string{"reconcile"}, fixture.now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rescanned.ReconcileDismissedCount != 0 {
		t.Fatalf("full rescan folded the backdated event a second time: report=%+v", rescanned)
	}
}

// Falsifier: a STALE-BUT-DECODABLE cursor (structurally valid token whose
// offsets force a whole-segment reparse, e.g. past-EOF after a segment was
// replaced/truncated) must behave like the garbage-cursor full rescan above:
// the timestamp/ID filter is the dedup authority and nothing already folded
// folds again. Red if the store still reports such a page as Resumed=true —
// the filter-skip then re-serves every consumed event into a double fold.
func TestSynthesisReconcileStaleButDecodableCursorDoesNotDoubleFold(t *testing.T) {
	fixture := prepareBackdatedReconcileFixture(t)

	folded, err := fixture.store.SynthesisSweep([]string{"reconcile"}, fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if folded.ReconcileDismissedCount != 1 {
		t.Fatalf("backdated prerequisite fold dismissed = %d, want 1", folded.ReconcileDismissedCount)
	}
	checkpoint, err := fixture.store.loadReconcileCheckpoint(fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Cursor = staleButDecodableCursor(t, checkpoint.Cursor)
	if err := writeJSON(fixture.store.reconcileCheckpointPath(), checkpoint); err != nil {
		t.Fatal(err)
	}

	rescanned, err := fixture.store.SynthesisSweep([]string{"reconcile"}, fixture.now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rescanned.ReconcileDismissedCount != 0 {
		t.Fatalf("stale-but-decodable cursor re-served consumed events into a double fold: report=%+v", rescanned)
	}
}

// staleButDecodableCursor rewrites every per-segment offset in a real cursor
// token to a past-EOF value, keeping the token structurally valid (version
// intact, decodable) while guaranteeing the store degrades every segment to a
// whole-file reparse.
func staleButDecodableCursor(t *testing.T, token string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("checkpoint cursor is not a decodable token: %v", err)
	}
	var cursor struct {
		Version  int              `json:"v"`
		Segments map[string]int64 `json:"seg"`
	}
	if err := json.Unmarshal(raw, &cursor); err != nil {
		t.Fatalf("checkpoint cursor is not the scan-cursor shape: %v", err)
	}
	if len(cursor.Segments) == 0 {
		t.Fatal("checkpoint cursor holds no segment offsets; fixture no longer exercises the resume path")
	}
	for rel := range cursor.Segments {
		cursor.Segments[rel] = 1 << 40
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func TestSynthesisReconcileResumedBackdatedOutsideWindowIsParked(t *testing.T) {
	store := newStore(t)
	closeMemCompletionGate(t, store)
	now := t0.AddDate(0, 0, 40)
	baselineAt := now.Add(-2 * time.Hour)
	claim := reconcileClaim("claim_backdated_park", 1, "project:api", "runtime", "Go", rfc3339(t0))
	appendReconcileClaim(t, store, claim)
	writeReconcileTransition(t, store, reconcileTransitionInput{
		ID:        "tr_reconcile_window_baseline",
		CreatedAt: rfc3339(baselineAt),
		Scope:     claim.Entity,
		ClaimRefs: []string{claim.ID},
	})
	if _, err := store.SynthesisSweep([]string{"reconcile"}, now); err != nil {
		t.Fatal(err)
	}

	const reason = "backdated_outside_window"
	outsideID := "tr_reconcile_backdated_outside_window"
	writeReconcileTransition(t, store, reconcileTransitionInput{
		ID:        outsideID,
		CreatedAt: rfc3339(now.AddDate(0, 0, -31)),
		Scope:     claim.Entity,
		ClaimRefs: []string{claim.ID},
	})
	report, err := store.SynthesisSweep([]string{"reconcile"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileDismissedCount != 0 {
		t.Fatalf("outside-window backdated event folded instead of parking: report=%+v", report)
	}
	if report.ReconcileParkedCount != 1 || len(report.ReconcileParkedReasons) != 1 {
		t.Fatalf("outside-window backdated event was silently dropped: parked=%d reasons=%+v", report.ReconcileParkedCount, report.ReconcileParkedReasons)
	}
	parked := report.ReconcileParkedReasons[0]
	if parked.Reason != reason || parked.Count != 1 || !reflect.DeepEqual(parked.EventIDs, []string{outsideID}) {
		t.Fatalf("outside-window parked reason = %+v, want reason %q for %s", parked, reason, outsideID)
	}
	checkpoint, err := store.loadReconcileCheckpoint(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Counts[string(reconcileDispositionPark)] != 1 || !reflect.DeepEqual(checkpoint.ParkedReasons[reason], []string{outsideID}) {
		t.Fatalf("checkpoint did not retain outside-window parking: counts=%+v reasons=%+v", checkpoint.Counts, checkpoint.ParkedReasons)
	}
}
