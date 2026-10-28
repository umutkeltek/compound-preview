package work

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/home"
)

func TestExecutorJobReferencesDoNotTruncateCustomIDs(t *testing.T) {
	job := codexJobFile{
		Summary: "Compare `work_alpha_1` with work_alpha_2",
		Title:   "notwork_alpha work_release-v2.1",
		Request: json.RawMessage(`{"prompt":"Continue work_alpha_1 and work_\u00e9tape_3"}`),
	}
	want := []string{"work_alpha_1", "work_alpha_2", "work_release-v2.1", "work_étape_3"}
	if got := extractWorkRefTokens(job); !reflect.DeepEqual(got, want) {
		t.Fatalf("complete legacy references=%q, want %q", got, want)
	}
}

// deadPID spawns a trivial process, waits for it to exit, and returns its pid.
// That pid is guaranteed not to reference a live process for the test window,
// so the liveness probe must classify a job holding it as stalled.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		cmd = exec.Command("sh", "-c", "exit 0")
		if err := cmd.Start(); err != nil {
			t.Fatalf("spawn short-lived process: %v", err)
		}
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait short-lived process: %v", err)
	}
	return pid
}

func writeCodexJob(t *testing.T, stateDir, repoHash, taskID string, job map[string]any) {
	t.Helper()
	jobsDir := filepath.Join(stateDir, repoHash, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatalf("mkdir jobs dir: %v", err)
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobsDir, taskID+".json"), data, 0o644); err != nil {
		t.Fatalf("write job: %v", err)
	}
}

func newExecutorTestStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

// T1: a running-status job whose pid is dead must NOT count its matched work as
// running; the matched work item gets the needs_me card, and the stalled job is
// surfaced on the codex row.
func TestExecutorsProbeJobsFlipsDeadRunningWork(t *testing.T) {
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)

	w, err := store.Create(CreateInput{
		Owner:            "codex",
		Actor:            "operator-test",
		SuccessPredicate: "codex edits with proof",
	}, now)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatalf("claim work: %v", err)
	}

	stateDir := t.TempDir()
	writeCodexJob(t, stateDir, "repo-abc", "task-dead", map[string]any{
		"id":        "task-dead",
		"status":    "running",
		"phase":     "edit",
		"pid":       deadPID(t),
		"startedAt": now.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		"logFile":   "/machine-local/logs/task-dead.log",
		"summary":   "editing for " + w.ID + " until interrupted",
	})

	projection, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{
		ProbeJobs:    true,
		JobsStateDir: stateDir,
	})
	if err != nil {
		t.Fatalf("executors projection: %v", err)
	}

	if projection.Counts.Running != 0 {
		t.Fatalf("dead-executor work must not count as running: running=%d", projection.Counts.Running)
	}
	if projection.Counts.NeedsMe != 1 {
		t.Fatalf("dead-executor work must count as needs_me: needs_me=%d", projection.Counts.NeedsMe)
	}
	if projection.Counts.StalledJobs != 1 {
		t.Fatalf("stalled job count = %d, want 1", projection.Counts.StalledJobs)
	}

	codex := requireExecutorProjectionRow(t, projection, "codex")
	if codex.RunningCount != 0 || codex.NeedsMeCount != 1 {
		t.Fatalf("codex row counts wrong: running=%d needs_me=%d", codex.RunningCount, codex.NeedsMeCount)
	}
	if len(codex.CurrentWork) != 1 || codex.CurrentWork[0].NeedsMe != executorDiedNeedsMe {
		t.Fatalf("matched work missing needs_me card: %+v", codex.CurrentWork)
	}
	if len(codex.StalledJobs) != 1 {
		t.Fatalf("codex stalled_jobs = %+v, want 1", codex.StalledJobs)
	}
	job := codex.StalledJobs[0]
	if job.JobID != "task-dead" || job.WorkID != w.ID || job.LogPath == "" || job.AgeSeconds <= 0 || job.Reason == "" {
		t.Fatalf("stalled job ref incomplete: %+v", job)
	}
	if !containsSourceRef(projection.SourceRefs, ExecutorJobsProbeSourceRef) {
		t.Fatalf("probe source ref missing: %+v", projection.SourceRefs)
	}
}

// T1b: without --probe-jobs the output must be byte-identical regardless of the
// jobs state dir. The dead job must be invisible when the probe is off.
func TestExecutorsProbeJobsOffIsByteIdentical(t *testing.T) {
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)

	w, err := store.Create(CreateInput{Owner: "codex", Actor: "op", SuccessPredicate: "codex edits"}, now)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	stateDir := t.TempDir()
	writeCodexJob(t, stateDir, "repo-abc", "task-dead", map[string]any{
		"id": "task-dead", "status": "running", "pid": deadPID(t),
		"startedAt": now.Add(-time.Hour).UTC().Format(time.RFC3339),
		"summary":   "editing for " + w.ID,
	})

	withDir, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{JobsStateDir: stateDir})
	if err != nil {
		t.Fatalf("projection with dir: %v", err)
	}
	withoutDir, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{})
	if err != nil {
		t.Fatalf("projection without dir: %v", err)
	}
	a, _ := json.Marshal(withDir)
	b, _ := json.Marshal(withoutDir)
	if string(a) != string(b) {
		t.Fatalf("probe-off output must ignore jobs dir\nwith=%s\nwithout=%s", a, b)
	}
	if withDir.Counts.Running != 1 || withDir.Counts.NeedsMe != 0 {
		t.Fatalf("probe-off must still count the work as running: %+v", withDir.Counts)
	}
	if withDir.Counts.StalledJobs != 0 {
		t.Fatalf("probe-off must not surface stalled jobs: %+v", withDir.Counts)
	}
}

// An unmatched stalled job (no work_<id> token matching a running item) still
// owes proof: it appears in stalled_jobs but flips nothing.
func TestExecutorsProbeJobsUnmatchedStillSurfaces(t *testing.T) {
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)

	stateDir := t.TempDir()
	writeCodexJob(t, stateDir, "repo-x", "task-orphan", map[string]any{
		"id": "task-orphan", "status": "running", "pid": deadPID(t),
		"startedAt": now.Add(-time.Hour).UTC().Format(time.RFC3339),
		"summary":   "no work reference here",
	})
	// A live-status job (with our own live pid) must never be treated as stalled.
	writeCodexJob(t, stateDir, "repo-x", "task-live", map[string]any{
		"id": "task-live", "status": "running", "pid": os.Getpid(),
		"startedAt": now.Add(-time.Minute).UTC().Format(time.RFC3339),
	})

	projection, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{ProbeJobs: true, JobsStateDir: stateDir})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if projection.Counts.StalledJobs != 1 || projection.Counts.NeedsMe != 0 {
		t.Fatalf("unmatched dead job should surface once, flip nothing: %+v", projection.Counts)
	}
	codex := requireExecutorProjectionRow(t, projection, "codex")
	if len(codex.StalledJobs) != 1 || codex.StalledJobs[0].JobID != "task-orphan" {
		t.Fatalf("orphan stalled job not surfaced: %+v", codex.StalledJobs)
	}
}

func containsSourceRef(refs []string, want string) bool {
	for _, r := range refs {
		if r == want {
			return true
		}
	}
	return false
}
