// Lane liveness for compound work delegate -- the keeper-visible signal.
// Today `compound work delegate status --json` reports run_attempt_status
// "running" and pane_status.exists true for a lane that has been frozen on
// an unanswered permission dialog for 50 minutes. A keeper can only tell
// "working" from "stalled" by hand-stat-ing transcript files. This file
// pins the contract: every status observation carries a transcript
// write-age (newest transcript mtime, as a duration string) and a derived
// liveness verdict (one of: working, stalled, starting, exited, unbound,
// unknown). Both fields are absent from the struct today; the test below
// fails at compile time before any behaviour is checked, so RED is real.
package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// spawnBindingWithTranscripts drives a Store all the way through
// Delegate so a binding + RunAttempt exist, then pre-populates the
// harness's session-store transcript directory with a .jsonl file whose
// mtime we control. Returns the Store, work id, and the resolved
// session-store root so the caller can also clear transcripts (for the
// starting branch) and inspect the observation.
//
// Cleanup restores the package-level seams the helper overwrites.
func spawnBindingWithTranscripts(t *testing.T, age time.Duration, leaveTranscript bool) (s *Store, workID, root string, cleanup func()) {
	t.Helper()

	root = t.TempDir()
	origResolver := DelegateSessionStoreResolver
	DelegateSessionStoreResolver = func(harness, lane string) (string, error) { return root, nil }

	ws := t.TempDir()
	origGetwd := delegateOsGetwd
	delegateOsGetwd = func() (string, error) { return ws, nil }

	origGuard := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	origReadiness := DelegateHarnessReadiness
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}

	s = newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID:  wID,
		Harness: "claude",
		Actor:   "codex",
		Command: "claude",
		Args:    []string{"--print"},
		Now:     time.Now(),
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "spawned" || res.Binding == nil {
		t.Fatalf("delegate did not spawn: %+v", res.Binding)
	}

	dir, dirErr := TranscriptDirFor(root, ws)
	if dirErr != nil {
		t.Fatalf("TranscriptDirFor: %v", dirErr)
	}
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		t.Fatalf("mkdir transcript dir: %v", mkErr)
	}
	if leaveTranscript {
		transcriptPath := filepath.Join(dir, "session-1.jsonl")
		if writeErr := os.WriteFile(transcriptPath, []byte("{}\n"), 0o600); writeErr != nil {
			t.Fatalf("write transcript: %v", writeErr)
		}
		mtime := time.Now().Add(-age)
		if chtErr := os.Chtimes(transcriptPath, mtime, mtime); chtErr != nil {
			t.Fatalf("chtimes: %v", chtErr)
		}
	}

	cleanup = func() {
		DelegateSessionStoreResolver = origResolver
		delegateOsGetwd = origGetwd
		LaneCapacityGuard = origGuard
		DelegateHarnessReadiness = origReadiness
	}
	return s, wID, root, cleanup
}

// probePane tells the fake runner the bound session reports the given
// status. DelegateStatus walks the binding to find the session name.
func probePane(t *testing.T, s *Store, runner *fakeSessionRunner, workID string, pane SessionStatus) {
	t.Helper()
	b, _, err := s.recoverBoundSession(workID)
	if err != nil || b == nil {
		t.Fatalf("recoverBoundSession: %v %+v", err, b)
	}
	if runner.panes == nil {
		runner.panes = map[string]SessionStatus{}
	}
	runner.panes[b.Session] = pane
}

// TestDelegateStatusSurfacesTranscriptWriteAge pins the field's existence
// and shape. The newest transcript file's mtime is the source of truth;
// the field carries that age as a duration string ("30s", "5m12s", ...)
// so a keeper reading --json output can see "5 minutes since the lane
// last wrote anything" without stat'ing a file.
func TestDelegateStatusSurfacesTranscriptWriteAge(t *testing.T) {
	s, wID, _, done := spawnBindingWithTranscripts(t, 30*time.Second, true)
	defer done()

	obs, err := s.DelegateStatus(wID, newFakeRunner())
	if err != nil {
		t.Fatalf("delegate status: %v", err)
	}
	if !obs.Bound {
		t.Fatalf("expected bound observation: %+v", obs)
	}
	if obs.TranscriptWriteAge == "" {
		t.Fatalf("transcript_write_age must be present on a bound run with transcripts; got %+v", obs)
	}
	age, parseErr := time.ParseDuration(obs.TranscriptWriteAge)
	if parseErr != nil {
		t.Fatalf("transcript_write_age %q must be a Go duration string: %v", obs.TranscriptWriteAge, parseErr)
	}
	if age < 25*time.Second || age > 2*time.Minute {
		t.Fatalf("transcript_write_age = %s, want ~30s window", obs.TranscriptWriteAge)
	}
}

// TestDelegateStatusVerdictWorkingOnFreshTranscript pins the working
// branch of the liveness verdict: pane alive + transcript fresh = a lane
// a keeper can trust to be actively progressing.
func TestDelegateStatusVerdictWorkingOnFreshTranscript(t *testing.T) {
	s, wID, _, done := spawnBindingWithTranscripts(t, 5*time.Second, true)
	defer done()

	runner := newFakeRunner()
	probePane(t, s, runner, wID, SessionStatus{Exists: true, PaneDead: false, ExitCode: -1})

	obs, err := s.DelegateStatus(wID, runner)
	if err != nil {
		t.Fatalf("delegate status: %v", err)
	}
	if !obs.Bound {
		t.Fatalf("expected bound observation: %+v", obs)
	}
	if obs.LivenessVerdict != "working" {
		t.Fatalf("liveness_verdict = %q, want %q (pane alive, transcript 5s old); full observation: %+v",
			obs.LivenessVerdict, "working", obs)
	}
}

// TestDelegateStatusVerdictStalledOnStaleTranscript pins the failure mode
// from the goal statement: pane is alive, run_attempt_status is running,
// but the transcript has not been written for an extended period. A
// keeper looking at --json must see "stalled" -- not "running".
func TestDelegateStatusVerdictStalledOnStaleTranscript(t *testing.T) {
	const stallThreshold = 90 * time.Second
	s, wID, _, done := spawnBindingWithTranscripts(t, stallThreshold+30*time.Second, true)
	defer done()

	runner := newFakeRunner()
	probePane(t, s, runner, wID, SessionStatus{Exists: true, PaneDead: false, ExitCode: -1})

	obs, err := s.DelegateStatus(wID, runner)
	if err != nil {
		t.Fatalf("delegate status: %v", err)
	}
	if !obs.Bound {
		t.Fatalf("expected bound observation: %+v", obs)
	}
	if obs.RunAttemptStatus != runattempt.StatusRunning {
		t.Fatalf("precondition: run attempt status = %q, want running", obs.RunAttemptStatus)
	}
	if obs.LivenessVerdict != "stalled" {
		t.Fatalf("liveness_verdict = %q, want %q (pane alive, transcript %s old, threshold %s); full: %+v",
			obs.LivenessVerdict, "stalled", obs.TranscriptWriteAge, stallThreshold, obs)
	}
}

// TestDelegateStatusVerdictStartingBeforeAnyTranscript pins the
// "starting" branch: pane alive, no transcripts yet (legitimate start
// window). Distinct from "stalled" so a keeper does not page on a brand
// new lane.
func TestDelegateStatusVerdictStartingBeforeAnyTranscript(t *testing.T) {
	s, wID, _, done := spawnBindingWithTranscripts(t, 0, false)
	defer done()

	runner := newFakeRunner()
	probePane(t, s, runner, wID, SessionStatus{Exists: true, PaneDead: false, ExitCode: -1})

	obs, err := s.DelegateStatus(wID, runner)
	if err != nil {
		t.Fatalf("delegate status: %v", err)
	}
	if !obs.Bound {
		t.Fatalf("expected bound observation: %+v", obs)
	}
	// "starting" is the keeper-visible verdict when the lane has not
	// yet written a transcript; "stalled" only fires after transcripts
	// exist but have stopped.
	if obs.LivenessVerdict == "stalled" {
		t.Fatalf("a lane with zero transcripts must NOT read as stalled: %+v", obs)
	}
	if obs.LivenessVerdict != "starting" && obs.LivenessVerdict != "working" {
		t.Fatalf("liveness_verdict = %q, want starting or working (no transcripts yet): %+v",
			obs.LivenessVerdict, obs)
	}
}

// TestDelegateStatusJSONKeysExposeLiveness pins that the JSON wire
// format -- the byte stream a keeper actually reads from --json --
// carries the two fields by name. A compile-time struct assertion would
// miss a renamed json tag; this test reads the marshaled bytes.
func TestDelegateStatusJSONKeysExposeLiveness(t *testing.T) {
	s, wID, _, done := spawnBindingWithTranscripts(t, 12*time.Second, true)
	defer done()

	obs, err := s.DelegateStatus(wID, newFakeRunner())
	if err != nil {
		t.Fatalf("delegate status: %v", err)
	}
	raw, marshalErr := json.Marshal(obs)
	if marshalErr != nil {
		t.Fatalf("marshal observation: %v", marshalErr)
	}
	if !strings.Contains(string(raw), `"transcript_write_age"`) {
		t.Fatalf("JSON wire format missing transcript_write_age: %s", raw)
	}
	if !strings.Contains(string(raw), `"liveness_verdict"`) {
		t.Fatalf("JSON wire format missing liveness_verdict: %s", raw)
	}
}
