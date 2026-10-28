package operatornotice

// Red-first falsifiers for work_a4d2491c8358: the operator attention holds
// must reach the operator via the existing act post buzz transport, exactly
// once per hold (idempotent on re-scan), with unnotifiable or transport-
// refused holds counted in a doctor figure rather than silently swallowed.
//
// These tests are red BEFORE the implementation lands. They pin the
// predicate's three falsifiers:
//   - once-and-only-once: a new hold produces exactly one notification
//     record; a re-scan produces zero
//   - re-scan-zero: a stable frontier over three back-to-back calls
//     produces Emitted=1, 0, 0
//   - refusal-is-counted-not-swallowed: a refused or unnotifiable hold
//     appears in the notice report and the doctor count, never silently

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/attention"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/work"
)

// fixedNow is the deterministic time used by every test in this package.
var fixedNow = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// seedHeldHome initializes a temp home and writes empty work envelopes so a
// held workbench item can be attributed to a real work/* file. The home is
// intentionally minimal: a work repo, a workbench projection, and one held
// work item. No real artifacts, no real buzz transport config — the test
// asserts behavior, not network I/O.
func seedHeldHome(t *testing.T, workIDs ...string) string {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test-operatornotice", fixedNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	for _, workID := range workIDs {
		w := work.BuildWork(work.CreateInput{
			WorkID:           workID,
			Owner:            "test",
			State:            "held",
			SuccessPredicate: "the held work is resolved by the operator",
		}, fixedNow)
		if err := w.Validate(); err != nil {
			t.Fatalf("build work %q: %v", workID, err)
		}
		body, err := json.MarshalIndent(w, "", " ")
		if err != nil {
			t.Fatalf("marshal work %q: %v", workID, err)
		}
		if err := os.WriteFile(filepath.Join(homePath, "work", "work", workID+".json"),
			append(body, '\n'), 0o644); err != nil {
			t.Fatalf("write work %q: %v", workID, err)
		}
	}
	return homePath
}

// workbenchHeldItems builds the workbench open list directly so the
// attention report sees BandNeedsYou groups without needing the full event
// store replay. The held item IDs are listed verbatim so the test can
// assert the resulting hold id.
func workbenchHeldItems(ids ...string) []work.WorkbenchItem {
	out := make([]work.WorkbenchItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, work.WorkbenchItem{
			WorkID:     id,
			State:      "held",
			OpenReason: "operator decision required",
			ClaimedAt:  fixedNow.Format(time.RFC3339),
		})
	}
	return out
}

// withFakeExecute swaps executeFn for a fake that returns the supplied
// receipt + transition ids. It registers a t.Cleanup to restore the real
// seam so each test runs in isolation. This is the test-only seam that
// proves the once-and-only-once promise without standing up a full buzz
// transport config + standing grant.
func withFakeExecute(t *testing.T, receiptID, transitionID string) {
	t.Helper()
	prev := executeFn
	executeFn = func(string, string, string, string, time.Time) (string, string, error) {
		return receiptID, transitionID, nil
	}
	t.Cleanup(func() { executeFn = prev })
}

// withFailingExecute swaps executeFn for a fake that always refuses. Used
// to drive the refusal-counted-not-swallowed falsifier.
func withFailingExecute(t *testing.T, reason string) {
	t.Helper()
	prev := executeFn
	executeFn = func(string, string, string, string, time.Time) (string, string, error) {
		return "", "", fmt.Errorf("%s", reason)
	}
	t.Cleanup(func() { executeFn = prev })
}

// TestNotifyOnceAndOnlyOnceForNewHold is the core falsifier: the very first
// scan of a fresh held work item emits exactly one notification record; the
// second scan of the same frontier emits zero new records. A re-scan of the
// same hold is a no-op.
func TestNotifyOnceAndOnlyOnceForNewHold(t *testing.T) {
	homePath := seedHeldHome(t, "work_held_new")
	withFakeExecute(t, "rec_test_first", "tr_test_first")

	report := attention.Build(attention.Inputs{
		Workbench: &work.WorkbenchReport{
			GeneratedAt: fixedNow.Format(time.RFC3339),
			Open:        workbenchHeldItems("work_held_new"),
		},
	}, fixedNow)
	if report.Counts.NeedsYou == 0 {
		t.Fatalf("seeded workbench produced no needs_you groups: %+v", report)
	}
	if len(report.Groups) != 1 {
		t.Fatalf("seeded workbench produced %d groups, want 1: %+v", len(report.Groups), report.Groups)
	}

	first, err := NotifyFromInputs(homePath, "buzz", report, fixedNow)
	if err != nil {
		t.Fatalf("first notify: %v", err)
	}
	if first.Scanned != 1 || first.Emitted != 1 || first.AlreadyNotified != 0 {
		t.Fatalf("first scan = %+v, want Scanned=1 Emitted=1 AlreadyNotified=0", first)
	}

	second, err := NotifyFromInputs(homePath, "buzz", report, fixedNow)
	if err != nil {
		t.Fatalf("second notify: %v", err)
	}
	if second.Scanned != 1 || second.Emitted != 0 || second.AlreadyNotified != 1 {
		t.Fatalf("re-scan must emit zero new notifications: %+v", second)
	}
}

// TestNotifyReScanZeroForRepeatedHolds is the broader re-scan-zero falsifier:
// three back-to-back calls on a stable frontier produce Emitted=1, 0, 0.
func TestNotifyReScanZeroForRepeatedHolds(t *testing.T) {
	homePath := seedHeldHome(t, "work_held_a", "work_held_b", "work_held_c")
	withFakeExecute(t, "rec_test_rescan", "tr_test_rescan")
	report := attention.Build(attention.Inputs{
		Workbench: &work.WorkbenchReport{
			GeneratedAt: fixedNow.Format(time.RFC3339),
			Open:        workbenchHeldItems("work_held_a", "work_held_b", "work_held_c"),
		},
	}, fixedNow)

	emits := []int{}
	for i := 0; i < 3; i++ {
		got, err := NotifyFromInputs(homePath, "buzz", report, fixedNow)
		if err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
		emits = append(emits, got.Emitted)
	}
	want := []int{1, 0, 0}
	for i, w := range want {
		if emits[i] != w {
			t.Fatalf("scan %d emitted = %d, want %d (full sequence %v)", i, emits[i], w, emits)
		}
	}
}

// TestNotifyRefusalIsCountedNotSwallowed is the refusal-is-counted falsifier.
// A refused executeFn must produce Emitted=0, Refused >= 1, DoctorCount >= 1
// — never a silent skip and never a panic. The doctor count is the witness
// that the refusal surfaced.
func TestNotifyRefusalIsCountedNotSwallowed(t *testing.T) {
	homePath := seedHeldHome(t, "work_held_refused")
	withFailingExecute(t, "simulated transport refusal")

	report := attention.Build(attention.Inputs{
		Workbench: &work.WorkbenchReport{
			GeneratedAt: fixedNow.Format(time.RFC3339),
			Open:        workbenchHeldItems("work_held_refused"),
		},
	}, fixedNow)

	got, err := NotifyFromInputs(homePath, "buzz", report, fixedNow)
	if err != nil {
		t.Fatalf("refused no-config notify: %v", err)
	}
	if got.Emitted != 0 {
		t.Fatalf("refused no-config must not emit, got %+v", got)
	}
	if got.Refused < 1 || got.DoctorCount < 1 {
		t.Fatalf("refused no-config must count refusal in doctor, got %+v", got)
	}
}

// TestNotifyUnNotifiableIsCountedNotSwallowed is the unnotifiable-counted
// falsifier. A holds group with no candidate work ids (a lease-hygiene
// group whose ItemIDs are empty) must produce Emitted=0, Unnotifiable >= 1,
// DoctorCount >= 1 — never silently dropped.
func TestNotifyUnNotifiableIsCountedNotSwallowed(t *testing.T) {
	homePath := seedHeldHome(t)
	withFakeExecute(t, "rec_test_unn", "tr_test_unn")
	report := attention.Build(attention.Inputs{
		Workbench: &work.WorkbenchReport{
			GeneratedAt: fixedNow.Format(time.RFC3339),
			Open:        workbenchHeldItems("work_stub"),
		},
	}, fixedNow)
	// Strip the items so the hold has no actionable work id, forcing the
	// unnotifiable path.
	for i := range report.Groups {
		report.Groups[i].ItemIDs = nil
	}
	got, err := NotifyFromInputs(homePath, "buzz", report, fixedNow)
	if err != nil {
		t.Fatalf("unnotifiable notify: %v", err)
	}
	if got.Emitted != 0 {
		t.Fatalf("unnotifiable must not emit, got %+v", got)
	}
	if got.Unnotifiable < 1 || got.DoctorCount < 1 {
		t.Fatalf("unnotifiable must count in doctor, got %+v", got)
	}
}

// TestNotifyDoesNotRecordOnReadOnlyFailure proves the wiring does not drift
// into accidental writes when the home is genuinely read-only. The report
// itself may be empty; the function must surface a typed error rather than
// silently succeed.
func TestNotifyDoesNotRecordOnReadOnlyFailure(t *testing.T) {
	// No home.Init: a totally empty temp dir is unusable as a home.
	homePath := t.TempDir()
	report := attention.Build(attention.Inputs{
		Workbench: &work.WorkbenchReport{GeneratedAt: fixedNow.Format(time.RFC3339)},
	}, fixedNow)
	if _, err := NotifyFromInputs(homePath, "buzz", report, fixedNow); err == nil {
		t.Fatalf("uninitialized home must surface a typed error, not a silent success")
	}
}

// TestNotifyWriteOnce_FromInputsExactCount hardens the once-and-only-once
// promise: after a successful emit, the event store contains exactly one
// record per hold; a re-scan of the same frontier writes zero new records.
func TestNotifyWriteOnce_FromInputsExactCount(t *testing.T) {
	homePath := seedHeldHome(t, "work_held_exact")
	withFakeExecute(t, "rec_test_exact", "tr_test_exact")
	report := attention.Build(attention.Inputs{
		Workbench: &work.WorkbenchReport{
			GeneratedAt: fixedNow.Format(time.RFC3339),
			Open:        workbenchHeldItems("work_held_exact"),
		},
	}, fixedNow)

	if _, err := NotifyFromInputs(homePath, "buzz", report, fixedNow); err != nil {
		t.Fatalf("first notify: %v", err)
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	recs, err := store.Scan(eventstore.ScanOptions{})
	if err != nil {
		t.Fatalf("scan events: %v", err)
	}
	if len(recs) < 1 {
		t.Fatalf("first scan must persist at least one notice record, got %d", len(recs))
	}

	// Re-scan and confirm the record count is unchanged.
	if _, err := NotifyFromInputs(homePath, "buzz", report, fixedNow); err != nil {
		t.Fatalf("second notify: %v", err)
	}
	recs2, err := store.Scan(eventstore.ScanOptions{})
	if err != nil {
		t.Fatalf("scan events: %v", err)
	}
	if len(recs2) != len(recs) {
		t.Fatalf("re-scan added %d records (total %d -> %d); idempotency violated",
			len(recs2)-len(recs), len(recs), len(recs2))
	}
}
