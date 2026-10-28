package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

// T3 + T3b: a lease on terminal work is stale residue — excluded from the active
// lease list/count and counted in stale_leases. The existing governed sweep
// (`home archive-history --kind lease`) lists it in dry-run without mutating,
// archives it for real, and never touches a lease on live work; the projection
// is clean afterward.
func TestExecutorsStaleLeaseResidueAndSweep(t *testing.T) {
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)
	homePath := os.Getenv("COMPOUND_HOME")

	// Terminal-work lease residue. Since I1 a normal close releases its own lease,
	// so this fixture writes the residue directly — see the helper for the two
	// sources that still produce it.
	terminalWork, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "terminal work with residual lease"}, now)
	if err != nil {
		t.Fatal(err)
	}
	closeWorkbenchTestWork(t, store, terminalWork.ID, now, "terminal work closed")
	writeResidualLeaseForTerminalWork(t, store, terminalWork.ID, "codex", now)

	// Live-work lease: claimed and still open.
	liveWork, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "live claimed work"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(liveWork.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	terminalLease := filepath.Join(homePath, "work", "leases", terminalWork.ID+".json")
	liveLease := filepath.Join(homePath, "work", "leases", liveWork.ID+".json")
	if !fileExists(terminalLease) || !fileExists(liveLease) {
		t.Fatalf("both leases should exist before sweep")
	}

	proj, err := store.ExecutorsProjection(now)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Counts.StaleLeases != 1 || proj.Counts.Leases != 1 {
		t.Fatalf("counts should split stale vs active: %+v", proj.Counts)
	}
	codex := requireExecutorProjectionRow(t, proj, "codex")
	if codex.StaleLeaseCount != 1 || codex.LeaseCount != 1 {
		t.Fatalf("codex lease split wrong: stale=%d active=%d", codex.StaleLeaseCount, codex.LeaseCount)
	}
	for _, l := range codex.Leases {
		if l.WorkID == terminalWork.ID {
			t.Fatalf("terminal-work lease must not be in active leases: %+v", codex.Leases)
		}
	}

	// Dry-run sweep lists the terminal lease, never the live one, mutates nothing.
	dry, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"lease"}, DryRun: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !sweepListsLease(dry, terminalWork.ID) {
		t.Fatalf("dry-run sweep should list terminal-work lease: %+v", dry)
	}
	if sweepListsLease(dry, liveWork.ID) {
		t.Fatalf("dry-run sweep must NOT list live-work lease: %+v", dry)
	}
	if !fileExists(terminalLease) || !fileExists(liveLease) {
		t.Fatalf("dry-run must not move any lease file")
	}

	// Real sweep archives the terminal lease and never the live one (T3b).
	real, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"lease"}}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !sweepListsLease(real, terminalWork.ID) {
		t.Fatalf("real sweep should archive terminal-work lease: %+v", real)
	}
	if fileExists(terminalLease) {
		t.Fatalf("terminal-work lease should be swept")
	}
	if !fileExists(liveLease) {
		t.Fatalf("live-work lease must survive the sweep (T3b)")
	}

	// Projection is clean after the sweep.
	after, err := store.ExecutorsProjection(now.Add(2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if after.Counts.StaleLeases != 0 || after.Counts.Leases != 1 {
		t.Fatalf("post-sweep counts not clean: %+v", after.Counts)
	}
}

// writeResidualLeaseForTerminalWork reproduces the lease residue a terminal work
// can still carry AFTER I1. Close now releases its own lease inside the same
// transaction, so a normal close no longer produces residue; the two remaining
// sources are a pre-I1 home (leases accumulated before the release landed) and a
// crash inside the release window (see
// TestCloseCrashBeforeLeaseReleaseLeavesSweepableResidueNeverALiveWorkWithNoLease).
// The stale-lease projection and the archive sweep are the orphan BACKSTOP for
// exactly that residue, so they are still proven against it — just no longer
// against a normal close, which is the invariant I1 added.
// The construction itself lives on Store as WriteResidualLeaseForTerminalWork: the
// proof-check agents gate needs the same state from outside this package, and one
// owner for the lease layout beats two copies that drift.
func writeResidualLeaseForTerminalWork(t *testing.T, s *Store, workID, by string, at time.Time) {
	t.Helper()
	if err := s.WriteResidualLeaseForTerminalWork(workID, by, at); err != nil {
		t.Fatal(err)
	}
}

func sweepListsLease(report home.OperationalArchiveReport, workID string) bool {
	for _, e := range report.Entries {
		if e.Kind == "lease" && strings.HasSuffix(e.OriginalPath, "/"+workID+".json") {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
