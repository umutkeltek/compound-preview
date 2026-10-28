// C2 tests: observe-run --job-file mints a deterministic RunAttempt from a
// codex plugin job file. Falsifiers:
//
//   - a completed job observed twice under the SAME work produces one
//     attempt (idempotent); under a DIFFERENT work it errors (F4)
//   - a running job maps to status=running (F5), and a later terminal
//     observe of the same job still terminalizes it normally

package work

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

func writeCodexJobFile(t *testing.T, dir, jobID, status, logFile, summary string) string {
	t.Helper()
	job := codexJobFile{
		ID:        jobID,
		Status:    status,
		Phase:     "execute",
		PID:       12345,
		StartedAt: "2026-07-20T12:00:00Z",
		LogFile:   logFile,
		Summary:   summary,
		Title:     "test job " + jobID,
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, jobID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// runningWorkForObserve creates a work in "running" state for observe-run tests.
// Uses the existing runningWork helper from run_observation_test.go.
func runningWorkForObserve(t *testing.T, s *Store, predicate string) string {
	t.Helper()
	return runningWork(t, s, predicate).ID
}

// seedTraceEnvelope creates a trace envelope bound to workID.
// Uses the existing writeTrace helper from run_observation_test.go.
func seedTraceEnvelope(t *testing.T, s *Store, workID string) envelope.TraceEnvelope {
	t.Helper()
	id := "trace_c2_" + workID[len(workID)-8:]
	return writeTrace(t, s, id, workID)
}

func TestObserveRunWithJobFileMintsRunAttempt(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "job file mints attempt")
	trace := seedTraceEnvelope(t, s, workID)
	jobPath := writeCodexJobFile(t, t.TempDir(), "job-abc123", "completed", "/tmp/job-abc.log", "fix the bug")

	result, err := s.ObserveRun(workID, ObserveRunInput{
		Actor:       "codex",
		TraceID:     trace.ID,
		JobFilePath: jobPath,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if result.RunAttemptID == "" {
		t.Fatal("observe-run with job file must produce a RunAttemptID")
	}
	if !runattempt.Valid(result.RunAttemptID) {
		t.Fatalf("RunAttemptID %q is not valid", result.RunAttemptID)
	}
	// The RunAttempt exists on disk with harness=codex.
	ra, err := runattempt.Open(s.home).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Harness != "codex" {
		t.Fatalf("harness = %q, want codex", ra.Harness)
	}
	if ra.WorkID != workID {
		t.Fatalf("work_id = %q, want %q", ra.WorkID, workID)
	}
	// Completed job → terminal succeeded.
	if ra.Status != runattempt.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", ra.Status)
	}
	// S2 fields populated from job data.
	if ra.TranscriptRef != "/tmp/job-abc.log" {
		t.Fatalf("transcript = %q, want /tmp/job-abc.log", ra.TranscriptRef)
	}
	// The ratt_ id landed on the envelope's RunAttemptIDs.
	advanced, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rid := range advanced.RunAttemptIDs {
		if rid == result.RunAttemptID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("envelope RunAttemptIDs %v does not include %q", advanced.RunAttemptIDs, result.RunAttemptID)
	}
}

// TestObserveRunWithJobFileCrossWorkErrors rewrites the pre-F4
// TestObserveRunWithJobFileIdempotent, which pinned the WRONG behavior: the
// same job file observed under two different works silently reused the
// first work's ratt_ id. F4 (T3): a codex job id is globally meaningful, so
// that reuse is now a fail-closed error naming both work ids; only one
// ratt_ file lands on disk and the first work's record is untouched.
func TestObserveRunWithJobFileCrossWorkErrors(t *testing.T) {
	s := newStore(t)
	workID1 := runningWorkForObserve(t, s, "cross-work job observe 1")
	trace1 := seedTraceEnvelope(t, s, workID1)
	jobDir := t.TempDir()
	jobPath := writeCodexJobFile(t, jobDir, "job-idem", "completed", "/tmp/idem.log", "idempotent job")

	result1, err := s.ObserveRun(workID1, ObserveRunInput{
		Actor: "codex", TraceID: trace1.ID, JobFilePath: jobPath,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// Second work, same job file → deterministic ratt_ id collides → must error.
	workID2 := runningWorkForObserve(t, s, "cross-work job observe 2")
	trace2 := seedTraceEnvelope(t, s, workID2)
	_, err = s.ObserveRun(workID2, ObserveRunInput{
		Actor: "codex", TraceID: trace2.ID, JobFilePath: jobPath,
	}, fixedNow.Add(3*time.Minute))
	if err == nil {
		t.Fatal("observing the same job file under a different work must error (F4 identity check)")
	}
	if !strings.Contains(err.Error(), workID1) || !strings.Contains(err.Error(), workID2) {
		t.Fatalf("error must name both work ids, got: %v", err)
	}
	// Only one ratt_ file on disk for this job -- the second work's mint never landed.
	entries, err := os.ReadDir(filepath.Join(s.home, "run_attempts"))
	if err != nil {
		t.Fatal(err)
	}
	rattCount := 0
	for _, e := range entries {
		if e.Name() == result1.RunAttemptID+".json" {
			rattCount++
		}
	}
	if rattCount != 1 {
		t.Fatalf("expected exactly 1 file for the deterministic id, got %d", rattCount)
	}
	// First work's record is intact.
	got, gerr := runattempt.Open(s.home).Get(result1.RunAttemptID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.WorkID != workID1 {
		t.Fatalf("existing record mutated: work_id = %q, want %q", got.WorkID, workID1)
	}
}

// TestObserveRunWithJobFileSameWorkIdempotent (T4): the idempotency half of
// the pre-F4 pinned test -- re-observing the SAME job file under the SAME
// work must still succeed with no error and the same ratt_ id.
func TestObserveRunWithJobFileSameWorkIdempotent(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "same-work idempotent job observe")
	trace := seedTraceEnvelope(t, s, workID)
	jobPath := writeCodexJobFile(t, t.TempDir(), "job-idem-same", "completed", "/tmp/idem-same.log", "idempotent job")

	result1, err := s.ObserveRun(workID, ObserveRunInput{
		Actor: "codex", TraceID: trace.ID, JobFilePath: jobPath,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	result2, err := s.ObserveRun(workID, ObserveRunInput{
		Actor: "codex", TraceID: trace.ID, JobFilePath: jobPath,
	}, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("same-work re-observe must stay idempotent: %v", err)
	}
	if result1.RunAttemptID != result2.RunAttemptID {
		t.Fatalf("same job file under same work produced different RunAttempt ids: %q vs %q", result1.RunAttemptID, result2.RunAttemptID)
	}
	entries, err := os.ReadDir(filepath.Join(s.home, "run_attempts"))
	if err != nil {
		t.Fatal(err)
	}
	rattCount := 0
	for _, e := range entries {
		if e.Name() == result1.RunAttemptID+".json" {
			rattCount++
		}
	}
	if rattCount != 1 {
		t.Fatalf("expected exactly 1 file for the deterministic id, got %d", rattCount)
	}
}

// TestObserveRunWithRunningJobStaysNonTerminal rewrites the pre-F5 version,
// which only asserted non-terminal (satisfied by the bug: status silently
// stuck at "pending" forever). F5 (T5): a running job must produce the
// explicit StatusRunning, not merely "not yet terminal".
func TestObserveRunWithRunningJobStaysNonTerminal(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "running job stays non-terminal")
	trace := seedTraceEnvelope(t, s, workID)
	jobPath := writeCodexJobFile(t, t.TempDir(), "job-running", "running", "/tmp/running.log", "still going")

	result, err := s.ObserveRun(workID, ObserveRunInput{
		Actor: "codex", TraceID: trace.ID, JobFilePath: jobPath,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ra, err := runattempt.Open(s.home).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Status != runattempt.StatusRunning {
		t.Fatalf("status = %q, want %q", ra.Status, runattempt.StatusRunning)
	}
}

// TestObserveRunAfterRunningTerminalizesNormally (T6): once a job's
// RunAttempt has been marked genuinely running (F5), a LATER observe
// reporting the job as completed must still terminalize it normally. Guards
// against the F5 wiring reintroducing the "already exists, skip
// re-populate" early return for a non-terminal (running) status -- that
// early return must trigger only on a truly terminal existing record.
func TestObserveRunAfterRunningTerminalizesNormally(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "running then terminal job observe")
	trace := seedTraceEnvelope(t, s, workID)
	jobDir := t.TempDir()
	jobPath := writeCodexJobFile(t, jobDir, "job-then-terminal", "running", "/tmp/then-terminal.log", "still going")

	first, err := s.ObserveRun(workID, ObserveRunInput{
		Actor: "codex", TraceID: trace.ID, JobFilePath: jobPath,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ra, err := runattempt.Open(s.home).Get(first.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Status != runattempt.StatusRunning {
		t.Fatalf("status after first observe = %q, want running", ra.Status)
	}

	// Same job file (same path), now reporting completed -- must terminalize.
	writeCodexJobFile(t, jobDir, "job-then-terminal", "completed", "/tmp/then-terminal.log", "done now")
	second, err := s.ObserveRun(workID, ObserveRunInput{
		Actor: "codex", TraceID: trace.ID, JobFilePath: jobPath,
	}, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.RunAttemptID != first.RunAttemptID {
		t.Fatalf("second observe minted a different RunAttempt: %q vs %q", second.RunAttemptID, first.RunAttemptID)
	}
	ra2, err := runattempt.Open(s.home).Get(second.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if ra2.Status != runattempt.StatusSucceeded {
		t.Fatalf("status after terminal re-observe = %q, want succeeded", ra2.Status)
	}
}

func TestObserveRunWithFailedJobTerminalizesFailed(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "failed job terminalizes")
	trace := seedTraceEnvelope(t, s, workID)
	jobPath := writeCodexJobFile(t, t.TempDir(), "job-fail", "failed", "/tmp/fail.log", "broken")

	result, err := s.ObserveRun(workID, ObserveRunInput{
		Actor: "codex", TraceID: trace.ID, JobFilePath: jobPath,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ra, err := runattempt.Open(s.home).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed", ra.Status)
	}
}

func TestObserveRunWithoutJobFileProducesNoRunAttempt(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "no job file no attempt")
	trace := seedTraceEnvelope(t, s, workID)

	result, err := s.ObserveRun(workID, ObserveRunInput{
		Actor: "codex", TraceID: trace.ID,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if result.RunAttemptID != "" {
		t.Fatalf("observe-run without job file must not produce RunAttemptID, got %q", result.RunAttemptID)
	}
}

func TestObserveRunTerminalJobFailsWhenRunAttemptCannotTerminalize(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "terminalization failure is hard")
	trace := seedTraceEnvelope(t, s, workID)
	jobPath := writeCodexJobFile(t, t.TempDir(), "job-terminalize-fail", "completed", "/tmp/terminalize-fail.log", "done")
	original := completeObservedRunAttempt
	completeObservedRunAttempt = func(*runattempt.Store, string, string, time.Time) (runattempt.RunAttempt, error) {
		return runattempt.RunAttempt{}, errors.New("forced terminal persistence failure")
	}
	t.Cleanup(func() { completeObservedRunAttempt = original })

	_, err := s.ObserveRun(workID, ObserveRunInput{
		Actor:       "codex",
		TraceID:     trace.ID,
		JobFilePath: jobPath,
	}, fixedNow.Add(2*time.Minute))

	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal job must fail when RunAttempt terminalization fails, got %v", err)
	}
	workAfter, getErr := s.Get(workID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if workAfter.State != "running" {
		t.Fatalf("failed terminalization must abort before work advances: state=%q", workAfter.State)
	}
}

func TestObserveRunRunningJobFailsWhenRunAttemptCannotMarkRunning(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "running status persistence failure is hard")
	trace := seedTraceEnvelope(t, s, workID)
	jobPath := writeCodexJobFile(t, t.TempDir(), "job-running-mark-fail", "running", "/tmp/running-mark-fail.log", "still running")
	original := markObservedRunAttemptRunning
	markObservedRunAttemptRunning = func(*runattempt.Store, string, time.Time) (runattempt.RunAttempt, error) {
		return runattempt.RunAttempt{}, errors.New("forced running persistence failure")
	}
	t.Cleanup(func() { markObservedRunAttemptRunning = original })

	_, err := s.ObserveRun(workID, ObserveRunInput{
		Actor:       "codex",
		TraceID:     trace.ID,
		JobFilePath: jobPath,
	}, fixedNow.Add(2*time.Minute))

	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("running job must fail when RunAttempt status persistence fails, got %v", err)
	}
	workAfter, getErr := s.Get(workID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if workAfter.State != "running" {
		t.Fatalf("failed running-status persistence must abort before work advances: state=%q", workAfter.State)
	}
}

// TestObserveRunStampsAttributionFromInput: the S3 attribution fields on
// ObserveRunInput land on the job-file-minted RunAttempt while it is still
// pending (before the status-map Complete), so the terminal-refusing setters
// accept them. This is the work-layer round-trip proof of the observe-run
// stamping policy; the CLI tests (work_cli_test.go) prove the flag plumbing
// and the no-keys omitempty case end-to-end.
func TestObserveRunStampsAttributionFromInput(t *testing.T) {
	s := newStore(t)
	workID := runningWorkForObserve(t, s, "attribution stamps from input")
	trace := seedTraceEnvelope(t, s, workID)
	jobPath := writeCodexJobFile(t, t.TempDir(), "job-attr", "completed", "/tmp/job-attr.log", "attribution job")

	result, err := s.ObserveRun(workID, ObserveRunInput{
		Actor:         "codex",
		TraceID:       trace.ID,
		JobFilePath:   jobPath,
		LaneID:        "lane-a:worker",
		ModelID:       "model-x",
		ProviderRoute: "provider-y",
		TokensIn:      100,
		TokensOut:     250,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ra, err := runattempt.Open(s.home).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.LaneID != "lane-a:worker" || ra.ModelID != "model-x" || ra.ProviderRoute != "provider-y" {
		t.Fatalf("attribution not stamped on the minted RunAttempt: lane=%q model=%q provider=%q", ra.LaneID, ra.ModelID, ra.ProviderRoute)
	}
	if ra.TokensIn != 100 || ra.TokensOut != 250 {
		t.Fatalf("token usage not stamped: in=%d out=%d", ra.TokensIn, ra.TokensOut)
	}
}
