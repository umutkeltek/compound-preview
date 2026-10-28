package main

import (
	"strings"
	"testing"

	"compound/internal/work"
)

// The dangerous failure is a FALSE ghost, not a missed one. A ghost verdict is
// what justifies reclaiming another owner's lease; if the classifier invents
// ghosts, a reaper acting on it takes work away from healthy sessions. Every
// case below is a way that has already nearly happened.
func TestClassifySessionLivenessNeverInventsAGhost(t *testing.T) {
	t.Parallel()
	live := work.SessionStatus{Exists: true, PaneDead: false, ExitCode: -1}
	gone := work.SessionStatus{Exists: false}
	deadPane := work.SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}

	cases := []struct {
		name  string
		obs   work.DelegateStatusObservation
		want  string
		never string
	}{
		{
			name:  "bound and observed running",
			obs:   work.DelegateStatusObservation{Bound: true, Session: "mmx-w10-contract", PaneStatus: live},
			want:  "live",
			never: "ghost",
		},
		{
			// A prober that cannot answer must not be read as "the session is
			// dead". Absence of measurement is not measurement of absence.
			name:  "probe failed on a session that is actually alive",
			obs:   work.DelegateStatusObservation{Bound: true, Session: "mmx-w10-contract", PaneStatus: live, ProbeError: "tmux: connection refused"},
			want:  "unknown",
			never: "ghost",
		},
		{
			// Probe failure outranks even a gone-looking pane: with a broken
			// prober the pane reading is not trustworthy either.
			name:  "probe failed and pane reads gone",
			obs:   work.DelegateStatusObservation{Bound: true, Session: "s", PaneStatus: gone, ProbeError: "exec: tmux not found"},
			want:  "unknown",
			never: "ghost",
		},
		{
			// No binding means there is no session that could be missing.
			name:  "no recorded binding",
			obs:   work.DelegateStatusObservation{Bound: false},
			want:  "unbound",
			never: "ghost",
		},
		{
			name: "bound, probed cleanly, session genuinely gone",
			obs:  work.DelegateStatusObservation{Bound: true, Session: "mmx-w10-breadth", PaneStatus: gone},
			want: "ghost",
		},
		{
			name: "bound, probed cleanly, pane exited",
			obs:  work.DelegateStatusObservation{Bound: true, Session: "mmx-w10-breadth", PaneStatus: deadPane},
			want: "ghost",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, detail := work.ClassifySessionLiveness("running", c.obs)
			if got != c.want {
				t.Fatalf("state = %q, want %q (detail: %s)", got, c.want, detail)
			}
			if c.never != "" && got == c.never {
				t.Fatalf("state must never be %q for this input", c.never)
			}
			if got == "ghost" && !strings.Contains(detail, c.obs.Session) {
				t.Fatalf("a ghost verdict must name the session it probed, got %q", detail)
			}
		})
	}
}

// Liveness must come from the RECORDED session name, never from resemblance
// between an actor and a session. The prototype of this tool matched actor
// "claude-minimax:wave10-contract" against session "mmx-w10-contract", found no
// substring in common, and reported four healthy lanes as ghosts. Real actor
// and session names from that incident are used verbatim here.
func TestClassifyIgnoresActorNameResemblance(t *testing.T) {
	t.Parallel()
	obs := work.DelegateStatusObservation{
		Bound:      true,
		Session:    "mmx-w10-contract", // recorded at spawn
		PaneStatus: work.SessionStatus{Exists: true, PaneDead: false, ExitCode: -1},
	}
	// The actor is "claude-minimax:wave10-contract" and shares no substring
	// with the session name. The classifier must not care.
	if got, detail := work.ClassifySessionLiveness("running", obs); got != "live" {
		t.Fatalf("a live session whose name does not resemble its actor is still live; got %q (%s)", got, detail)
	}

	// And the inverse: a name that resembles a live session is still a ghost
	// when the recorded session is gone.
	obs2 := work.DelegateStatusObservation{
		Bound:      true,
		Session:    "minimax-burst10-hook-unicode-20260726",
		PaneStatus: work.SessionStatus{Exists: false},
	}
	if got, _ := work.ClassifySessionLiveness("running", obs2); got != "ghost" {
		t.Fatalf("a gone session is a ghost regardless of what other sessions are named; got %q", got)
	}
}
