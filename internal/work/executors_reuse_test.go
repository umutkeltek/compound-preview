package work

import "testing"

// TestExecutorsProjectionWorkbenchReuseEliminatesEventLogRead is the slice-5
// structural falsifier behind threading Workbench reuse into workbench_cli and
// narrate_cli: passing an already-built Workbench must NOT re-read
// events/work.jsonl, while the un-reused path pays exactly one full-log read for
// the second Workbench it builds internally. Without the reuse, workbench_cli's
// meter and narrate_cli each walk the whole event store twice per invocation.
//
// The counter is Store.eventLogReads (via eventLogReadCount), the same seam the
// ProofChainDoctor single-load falsifier uses. leaseWorkTerminal/ListLeases read
// envelope/lease files, not the work log, so the projection's only work-log read
// is the internal Workbench build.
func TestExecutorsProjectionWorkbenchReuseEliminatesEventLogRead(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "reuse falsifier"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Reused Workbench: the projection reads no additional work log.
	wb, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 8})
	if err != nil {
		t.Fatalf("workbench: %v", err)
	}
	before := s.eventLogReadCount()
	if _, err := s.ExecutorsProjection(fixedNow, ExecutorsProjectionOptions{Workbench: &wb}); err != nil {
		t.Fatalf("executors (reuse): %v", err)
	}
	if delta := s.eventLogReadCount() - before; delta != 0 {
		t.Fatalf("reused-workbench ExecutorsProjection read events/work.jsonl %d times, want 0", delta)
	}

	// No reuse: the projection builds a second Workbench and pays one full-log read.
	before = s.eventLogReadCount()
	if _, err := s.ExecutorsProjection(fixedNow); err != nil {
		t.Fatalf("executors (fresh): %v", err)
	}
	if delta := s.eventLogReadCount() - before; delta != 1 {
		t.Fatalf("un-reused ExecutorsProjection read events/work.jsonl %d times, want exactly 1 (the second Workbench walk the CLIs eliminate)", delta)
	}
}
