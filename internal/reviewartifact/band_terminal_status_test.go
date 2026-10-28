package reviewartifact

import "testing"

// TestBandForStatusAdmitsEveryTerminalWord is the red-first falsifier for the
// one hand-copied terminal-word set that survived INSIDE the owner package:
// BandForStatus re-listed six words in its status switch and omitted
// "completed" — the exact drift status.go was created to end, in the package
// that owns the set.
//
// The hole is masked whenever an annotator sets readiness ("no_action" also
// returns BandFiled), so it bites precisely where no annotator ran: a bare
// queue item or attention group whose status alone says finished gets banded
// BandNeedsYou, and the operator's ranked surface spends attention on work
// that has nothing left to decide.
//
// Red output at this commit:
//
//	TestBandForStatusAdmitsEveryTerminalWord/completed: band = "needs_you", want "filed"
func TestBandForStatusAdmitsEveryTerminalWord(t *testing.T) {
	for _, status := range []string{"ok", "applied", "resolved", "accepted", "recorded", "candidate_recorded", "completed"} {
		t.Run(status, func(t *testing.T) {
			if got := BandForStatus(status, ""); got != BandFiled {
				t.Fatalf("BandForStatus(%q, \"\") = %q, want %q: a terminal status must band filed even when no annotator set a readiness", status, got, BandFiled)
			}
		})
	}
}

// TestBandForStatusCompletedWithReadiness pins the masked path as a control:
// readiness "no_action" must keep banding filed after the status switch is
// retired to the owner.
func TestBandForStatusCompletedWithReadiness(t *testing.T) {
	if got := BandForStatus("completed", "no_action"); got != BandFiled {
		t.Fatalf("BandForStatus(completed, no_action) = %q, want %q", got, BandFiled)
	}
	if got := BandForStatus("held", ""); got != BandWatching {
		t.Fatalf("BandForStatus(held, \"\") = %q, want %q: non-terminal words keep their bands", got, BandWatching)
	}
}
