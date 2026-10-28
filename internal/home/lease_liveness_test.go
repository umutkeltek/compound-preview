package home

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStaleLeaseCandidatesKeepsLeaseWhenRunAttemptIsLive is the red-test for
// the lease-liveness fix: a lease whose work item carries a non-terminal
// RunAttempt with a fresh heartbeat (i.e. a live delegate pane) must NEVER be
// classified as stale by `home archive-history --kind lease`, even when the
// hot work envelope is in a terminal state.
//
// Before the fix, the stale-lease predicate is purely terminal/TTL-based:
// work state is "completed" → the lease is residue → the archive sweep
// removes it. The predicate is blind to the live pane, so a long-running
// delegate session that takes longer than the work envelope's terminal
// transition gets falsely archived.
//
// After the fix, the predicate consults RunAttempt liveness before
// declaring the lease stale. The fixture below puts:
//   - a hot work envelope in state "completed" (the TTL-only signal that
//     would mark the lease stale on its own),
//   - a non-terminal RunAttempt with a fresh heartbeat for that work_id
//     (the live-pane signal that must override the TTL signal).
func TestStaleLeaseCandidatesKeepsLeaseWhenRunAttemptIsLive(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	const workID = "work_live_pane"
	// hot work envelope is terminal — TTL-only heuristic would mark the
	// lease stale on this alone.
	writeHomeFile(t, h, "work/work/"+workID+".json", `{"id":"work_live_pane","state":"completed"}`)
	writeHomeFile(t, h, "work/leases/"+workID+".json", `{"work_id":"work_live_pane","by":"codex","claimed_at":"1"}`)
	// non-terminal RunAttempt with fresh heartbeat: the live-pane signal.
	// "running" status + last_heartbeat_at within the freshness window
	// means a delegate pane is genuinely progressing. Timestamps are pinned
	// to time.Now() so the freshness window reads correctly regardless of
	// when the test runs (the helper uses time.Now() for its threshold).
	nowMinus1m := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	nowMinus2m := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	writeHomeFile(t, h,
		"run_attempts/ratt_live_pane.json",
		`{"schema_version":"compound.runattempt.v0.1","id":"ratt_live_pane","work_id":"work_live_pane","actor":"compound-panel","harness":"compound-panel","status":"running","created_at":"`+nowMinus2m+`","last_heartbeat_at":"`+nowMinus1m+`"}`,
	)
	// sanity control: an actually-dead lease (terminal work, no live pane)
	// must STILL be classified stale — the fix narrows the stale set, it
	// does not silence it.
	const deadWorkID = "work_dead_pane"
	writeHomeFile(t, h, "work/work/"+deadWorkID+".json", `{"id":"work_dead_pane","state":"completed"}`)
	writeHomeFile(t, h, "work/leases/"+deadWorkID+".json", `{"work_id":"work_dead_pane","by":"codex","claimed_at":"2"}`)

	hygiene, err := OperationalHygiene(h)
	if err != nil {
		t.Fatalf("OperationalHygiene: %v", err)
	}
	if len(hygiene.StaleLeases) != 1 {
		t.Fatalf("stale-lease count = %d (paths=%v), want exactly 1 — only the dead-pane lease should be stale; the live-pane lease must be kept",
			len(hygiene.StaleLeases), hygiene.StaleLeases)
	}
	if got := hygiene.StaleLeases[0]; got != "work/leases/"+deadWorkID+".json" {
		t.Fatalf("stale lease = %q, want %q — live-pane lease leaked into the stale set",
			got, "work/leases/"+deadWorkID+".json")
	}
	// end-to-end: the lease-kind sweep must archive the dead lease and
	// leave the live-pane lease in place. This is the same code path
	// `compound home archive-history --kind lease` exercises.
	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"lease"}}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ArchiveOperationalHistory: %v", err)
	}
	if report.ArchivedCount != 1 || report.Counts["lease"] != 1 {
		t.Fatalf("lease-kind archive count = %d (counts=%v), want exactly 1 — the live-pane lease must not be swept",
			report.ArchivedCount, report.Counts)
	}
	// live-pane lease survives on disk; dead-pane lease is gone.
	mustReadHomeFile(t, h, "work/leases/"+workID+".json")
	assertMissing(t, filepath.Join(h, "work", "leases", deadWorkID+".json"))
}

// TestLiveRunAttemptForWorkIsFalseForMissingDir ensures the helper is a
// probe, not a hard gate: a fresh home with no run_attempts/ directory
// reads as "no live pane" so the orphan backstop still fires there.
func TestLiveRunAttemptForWorkIsFalseForMissingDir(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".compound")
	if got := liveRunAttemptForWork(h, "work_any", archiveTestNow); got {
		t.Fatalf("missing run_attempts dir must read as no live pane, got true")
	}
}

// TestLiveRunAttemptForWorkHonorsFreshness ensures the helper's threshold
// is the same 30-minute window runattempt.DefaultSweepThreshold uses; a
// heartbeat older than that is not "live" and the lease can be archived.
func TestLiveRunAttemptForWorkHonorsFreshness(t *testing.T) {
	now := time.Now().UTC()
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	const workID = "work_stale_pane"
	// Heartbeat 2 hours ago — beyond the 30-minute freshness window.
	nowMinus2h := now.Add(-2 * time.Hour).Format(time.RFC3339)
	nowMinus3h := now.Add(-3 * time.Hour).Format(time.RFC3339)
	writeHomeFile(t, h, "run_attempts/ratt_stale_pane.json",
		`{"schema_version":"compound.runattempt.v0.1","id":"ratt_stale_pane","work_id":"work_stale_pane","status":"running","created_at":"`+nowMinus3h+`","last_heartbeat_at":"`+nowMinus2h+`"}`,
	)
	if got := liveRunAttemptForWork(h, workID, now); got {
		t.Fatalf("stale heartbeat (>30m) must read as no live pane, got true")
	}
}

// TestLiveRunAttemptForWorkRejectsTerminalStatuses ensures the helper
// ignores succeeded/failed/abandoned/superseded RunAttempts — a terminal
// pane is not live, regardless of how fresh its heartbeat is.
func TestLiveRunAttemptForWorkRejectsTerminalStatuses(t *testing.T) {
	now := time.Now().UTC()
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	const workID = "work_terminal_pane"
	nowMinus1m := now.Add(-time.Minute).Format(time.RFC3339)
	nowMinus2m := now.Add(-2 * time.Minute).Format(time.RFC3339)
	writeHomeFile(t, h, "run_attempts/ratt_terminal_pane.json",
		`{"schema_version":"compound.runattempt.v0.1","id":"ratt_terminal_pane","work_id":"work_terminal_pane","status":"succeeded","created_at":"`+nowMinus2m+`","last_heartbeat_at":"`+nowMinus1m+`"}`,
	)
	if got := liveRunAttemptForWork(h, workID, now); got {
		t.Fatalf("terminal status must read as no live pane, got true")
	}
}
