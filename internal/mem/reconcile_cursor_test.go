package mem

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// closeMemCompletionGate flips the store's event log to the segment-authoritative
// read path so the reconcile Scan cursor is live (loose-fallback homes never
// cursor). migrationComplete only stats the marker.
func closeMemCompletionGate(t *testing.T, s *Store) {
	t.Helper()
	dir := filepath.Join(s.home, "events", "migration")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "completion.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Backdated events are intentionally covered separately: incremental loading
// can diverge from a full rescan because the persisted cursor is append-based.
// See reconcile_backdated_test.go for the fold-once contract.
// Falsifier (a) at the CONSUMER seam: the synthesis-reconcile fold input is
// byte-identical whether it resumes from the persisted byte cursor (incremental)
// or re-scans the whole event log (base, cursor empty) — same post-checkpoint
// event set, same order. The cursor changes only how much is parsed, never the
// result. Red if incremental consumption dropped or reordered an event.
func TestLoadReconcileEventsIncrementalMatchesFullRescanForForwardDatedEvents(t *testing.T) {
	s := newStore(t)
	closeMemCompletionGate(t, s)

	now := t0.Add(4 * time.Hour)
	for i := 0; i < 4; i++ {
		writeReconcileTransition(t, s, reconcileTransitionInput{
			ID:        fmt.Sprintf("tr_first_%d", i),
			CreatedAt: rfc3339(t0.Add(time.Duration(i) * time.Minute)),
			Scope:     "project:api",
			ClaimRefs: []string{"claim_x"},
		})
	}

	cp0 := reconcileCheckpoint{Since: rfc3339(t0.AddDate(0, 0, -1))}
	out1, cursor1, _, err := s.loadReconcileEvents(cp0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(out1) != 4 {
		t.Fatalf("first load = %d events, want 4", len(out1))
	}
	if cursor1 == "" {
		t.Fatal("segment-authoritative reconcile load must persist a non-empty cursor")
	}

	// Append a second batch strictly after the checkpoint boundary.
	for i := 0; i < 3; i++ {
		writeReconcileTransition(t, s, reconcileTransitionInput{
			ID:        fmt.Sprintf("tr_second_%d", i),
			CreatedAt: rfc3339(t0.Add(time.Hour + time.Duration(i)*time.Minute)),
			Scope:     "project:api",
			ClaimRefs: []string{"claim_x"},
		})
	}

	last := out1[len(out1)-1]
	base := reconcileCheckpoint{
		Since:         cp0.Since,
		LastEventID:   last.ID,
		LastEventTime: rfc3339(last.CreatedAt),
	}
	incremental := base
	incremental.Cursor = cursor1

	outBase, _, _, err := s.loadReconcileEvents(base, now) // full scan, app-filtered
	if err != nil {
		t.Fatal(err)
	}
	outInc, _, _, err := s.loadReconcileEvents(incremental, now) // incremental via cursor
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(outBase, outInc) {
		t.Fatalf("incremental fold input != full-rescan input:\n base=%+v\n inc =%+v", outBase, outInc)
	}
	if len(outInc) != 3 {
		t.Fatalf("post-checkpoint load = %d, want the 3 appended", len(outInc))
	}
}
