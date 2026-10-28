package home

import (
	"path/filepath"
	"strings"
	"testing"
)

// internal/work/dispatch_plan.go (RecordDispatchClosePlan / RecordDispatchClose)
// writes wdcp_*.json / wdcl_*.json directly under COMPOUND_HOME/dispatch-close-plans/
// and COMPOUND_HOME/dispatch-closes/. Before the habitat catalog registered those
// two directories, home doctor fail-closed the ENTIRE home the moment either
// directory existed: "unknown habitat entries: dispatch-close-plans,dispatch-closes"
// (find_b0542b87b5ad), taking down archive-history/home-doctor with it.
func TestArchiveHistoryLeaseSweepSucceedsWithDispatchCloseHabitatPresent(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "dispatch-close-plans/wdcp_1.json", `{"id":"wdcp_1","status":"no_closeable_work"}`)
	writeHomeFile(t, h, "dispatch-closes/wdcl_1.json", `{"id":"wdcl_1","status":"no_closeable_work"}`)

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"lease"}}, now)
	if err != nil {
		t.Fatalf("archive-history --kind lease failed with dispatch-close habitat present: %v", err)
	}
	if report.ArchivedCount != 0 {
		t.Fatalf("unexpected archive activity on an otherwise-empty home: %+v", report)
	}

	doctor := Doctor(h)
	if !doctor.OK || !doctor.HabitatOK {
		t.Fatalf("home doctor rejected known dispatch-close habitat entries: %+v", doctor)
	}

	entry, ok := HabitatForPath("dispatch-close-plans/wdcp_1.json")
	if !ok || entry.Prefix != "dispatch-close-plans" || entry.Status != HabitatStatusBridge || entry.CanonicalPrefix == "" || entry.RetirementCondition == "" {
		t.Fatalf("dispatch-close-plans habitat entry missing bridge debt: %+v ok=%t", entry, ok)
	}
	entry, ok = HabitatForPath("dispatch-closes/wdcl_1.json")
	if !ok || entry.Prefix != "dispatch-closes" || entry.Status != HabitatStatusBridge || entry.CanonicalPrefix == "" || entry.RetirementCondition == "" {
		t.Fatalf("dispatch-closes habitat entry missing bridge debt: %+v ok=%t", entry, ok)
	}
}

// The guard must keep teeth: a genuinely unknown top-level directory still
// fail-closes the whole-home doctor gate, even after dispatch-close-plans and
// dispatch-closes become known.
func TestArchiveHistoryStillFailsClosedForGenuinelyUnknownHabitatRoot(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "totally-bogus-dir/whatever.json", `{}`)

	_, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"lease"}}, now)
	if err == nil || !strings.Contains(err.Error(), "totally-bogus-dir") {
		t.Fatalf("archive-history accepted a genuinely unknown habitat root, err=%v", err)
	}

	doctor := Doctor(h)
	if doctor.OK || doctor.HabitatOK {
		t.Fatalf("home doctor accepted a genuinely unknown habitat root: %+v", doctor)
	}
}
