package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestProofCheckAppendTimingWarningsKeepsTimingStateAdvisory(t *testing.T) {
	originalRecorder := proofCheckTimingHistoryRecorder
	t.Cleanup(func() { proofCheckTimingHistoryRecorder = originalRecorder })

	t.Run("unresolved home", func(t *testing.T) {
		called := false
		proofCheckTimingHistoryRecorder = func(string, proofCheckReport, time.Duration, time.Time) (string, error) {
			called = true
			return "", nil
		}
		report := timingFixtureReport("certifying-full", map[string]int64{"go-test": 10})
		proofCheckAppendTimingWarnings(
			&report,
			"",
			errors.New("synthetic home failure"),
			time.Second,
			time.Now(),
		)
		if called {
			t.Fatal("timing recorder called without a resolved home")
		}
		if got := report.Warnings; len(got) != 1 ||
			got[0] != "COMPOUND_HOME unresolved; proof-check timing history not recorded: synthetic home failure" {
			t.Fatalf("warnings = %v", got)
		}
	})

	t.Run("record error", func(t *testing.T) {
		proofCheckTimingHistoryRecorder = func(string, proofCheckReport, time.Duration, time.Time) (string, error) {
			return "", errors.New("synthetic timing write failure")
		}
		report := timingFixtureReport("certifying-full", map[string]int64{"go-test": 10})
		proofCheckAppendTimingWarnings(&report, "/synthetic/home", nil, time.Second, time.Now())
		if got := report.Warnings; len(got) != 1 ||
			got[0] != "proof-check timing history not recorded: synthetic timing write failure" {
			t.Fatalf("warnings = %v", got)
		}
		if report.Status != "ok" {
			t.Fatalf("timing failure changed proof status to %q", report.Status)
		}
	})

	t.Run("record warning", func(t *testing.T) {
		proofCheckTimingHistoryRecorder = func(string, proofCheckReport, time.Duration, time.Time) (string, error) {
			return "synthetic timing history reset", nil
		}
		report := timingFixtureReport("certifying-full", map[string]int64{"go-test": 10})
		proofCheckAppendTimingWarnings(&report, "/synthetic/home", nil, time.Second, time.Now())
		if got := report.Warnings; len(got) != 1 || got[0] != "synthetic timing history reset" {
			t.Fatalf("warnings = %v", got)
		}
	})
}

func timingFixtureReport(profileKind string, durations map[string]int64) proofCheckReport {
	report := proofCheckReport{Status: "ok", ProfileKind: profileKind}
	for name, ms := range durations {
		report.Steps = append(report.Steps, proofCheckStep{
			Name:       name,
			ProofRef:   profileKind + ":" + name,
			Status:     "ok",
			DurationMS: ms,
		})
	}
	return report
}

func readStoreForTest(t *testing.T, homePath string) proofCheckTimingsStore {
	t.Helper()
	store, warning, err := readProofCheckTimings(filepath.Join(homePath, proofCheckTimingsStateRelPath))
	if err != nil {
		t.Fatalf("read timings: %v", err)
	}
	if warning != "" {
		t.Fatalf("unexpected warning reading timings: %s", warning)
	}
	return store
}

func TestProofCheckTimingHistoryRoundTripsRecordedDurations(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	report := timingFixtureReport("", map[string]int64{"go-test": 601_600, "go-vet": 4_200})
	if _, err := recordProofCheckTimingHistory(homePath, report, 12*time.Minute, now); err != nil {
		t.Fatalf("record run 1: %v", err)
	}
	if _, err := recordProofCheckTimingHistory(homePath, report, 11*time.Minute, now.Add(time.Hour)); err != nil {
		t.Fatalf("record run 2: %v", err)
	}

	store := readStoreForTest(t, homePath)
	if len(store.Runs) != 2 {
		t.Fatalf("want 2 recorded runs, got %d", len(store.Runs))
	}
	if store.SchemaVersion != proofCheckTimingsSchemaVersion {
		t.Fatalf("schema version = %q, want %q", store.SchemaVersion, proofCheckTimingsSchemaVersion)
	}
	// The whole point of the store: the durations it reports must be the durations the battery
	// measured, not a re-derivation. StepsTotalMS is the sum of the gates; WallMS is real elapsed.
	if got := store.Runs[0].StepsTotalMS; got != 605_800 {
		t.Fatalf("steps_total_ms = %d, want 605800", got)
	}
	if got := store.Runs[0].WallMS; got != (12 * time.Minute).Milliseconds() {
		t.Fatalf("wall_ms = %d, want %d", got, (12 * time.Minute).Milliseconds())
	}
	for _, step := range store.Runs[0].Steps {
		if step.Name == "go-test" && step.DurationMS != 601_600 {
			t.Fatalf("go-test duration = %d, want 601600", step.DurationMS)
		}
	}
}

// The load-bearing separation: --dev's first gate is ALSO named "go-test" but runs
// `go test -short` with the cache enabled. If the two lanes blended, the certifying gate's p90
// would collapse toward the cached lane's seconds and every budget derived from it would be wrong
// in the dangerous direction (too tight), producing exactly the false reds this work exists to end.
func TestProofCheckTimingStatsKeepCertifyingAndDevelopmentLanesApart(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	certifying := proofCheckReport{Status: "ok", Steps: []proofCheckStep{
		{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "ok", DurationMS: 600_000},
	}}
	development := proofCheckReport{Status: "ok", ProfileKind: proofCheckProfileDevelopment, Steps: []proofCheckStep{
		{Name: "go-test", ProofRef: "development:go-test-short-cached-all", Status: "ok", DurationMS: 30_000},
	}}
	for i := 0; i < 3; i++ {
		if _, err := recordProofCheckTimingHistory(homePath, certifying, time.Minute, now); err != nil {
			t.Fatalf("record certifying: %v", err)
		}
		if _, err := recordProofCheckTimingHistory(homePath, development, time.Minute, now); err != nil {
			t.Fatalf("record development: %v", err)
		}
	}

	store := readStoreForTest(t, homePath)

	certStats := proofCheckTimingStats(store, "", "", 20)
	if len(certStats) != 1 {
		t.Fatalf("certifying lane: want 1 gate, got %d", len(certStats))
	}
	if certStats[0].P50MS != 600_000 {
		t.Fatalf("certifying go-test p50 = %d, want 600000 (development runs must not blend in)", certStats[0].P50MS)
	}

	devStats := proofCheckTimingStats(store, proofCheckProfileDevelopment, "", 20)
	if len(devStats) != 1 {
		t.Fatalf("development lane: want 1 gate, got %d", len(devStats))
	}
	if devStats[0].P50MS != 30_000 {
		t.Fatalf("development go-test p50 = %d, want 30000", devStats[0].P50MS)
	}
}

// A dry run produces steps with status "planned" and no durations; a battery that fails early
// leaves later gates unexecuted. Neither carries a timing signal, and admitting them as 0ms
// samples would drag every percentile toward zero.
func TestProofCheckTimingHistoryIgnoresUnexecutedGates(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	dryRun := proofCheckReport{Status: "ok", Steps: []proofCheckStep{
		{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "planned"},
		{Name: "go-vet", ProofRef: "go-vet-all", Status: "planned"},
	}}
	if _, err := recordProofCheckTimingHistory(homePath, dryRun, time.Second, now); err != nil {
		t.Fatalf("record dry run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, proofCheckTimingsStateRelPath)); !os.IsNotExist(err) {
		t.Fatalf("a run with no executed gates must not create the history file (stat err = %v)", err)
	}

	mixed := proofCheckReport{Status: "failed", Steps: []proofCheckStep{
		{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "failed", DurationMS: 1_200_453},
		{Name: "go-vet", ProofRef: "go-vet-all", Status: "planned"},
		{Name: "console-smoke", ProofRef: "console-smoke", Status: "skipped"},
	}}
	if _, err := recordProofCheckTimingHistory(homePath, mixed, 20*time.Minute, now); err != nil {
		t.Fatalf("record mixed run: %v", err)
	}
	store := readStoreForTest(t, homePath)
	if len(store.Runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(store.Runs))
	}
	if len(store.Runs[0].Steps) != 1 {
		t.Fatalf("want only the executed gate recorded, got %d steps", len(store.Runs[0].Steps))
	}
	// A red run's durations ARE recorded — excluding them would bias the baseline toward
	// whatever happens to be green.
	if store.Runs[0].Steps[0].Status != "failed" {
		t.Fatalf("failed gate must be recorded, got status %q", store.Runs[0].Steps[0].Status)
	}

	// ...but a failed gate must not describe what the gate normally COSTS.
	stats := proofCheckTimingStats(store, "", "", 20)
	if len(stats) != 1 {
		t.Fatalf("want 1 gate in stats, got %d", len(stats))
	}
	if stats[0].Samples != 0 {
		t.Fatalf("a failed gate must contribute no baseline sample, got %d", stats[0].Samples)
	}
	if stats[0].LastStatus != "failed" || stats[0].LastMS != 1_200_453 {
		t.Fatalf("last observation should still be visible, got %+v", stats[0])
	}
}

func TestProofCheckTimingHistoryTrimsToRollingWindow(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	total := proofCheckTimingsMaxRuns + 7
	for i := 0; i < total; i++ {
		report := proofCheckReport{Status: "ok", Steps: []proofCheckStep{
			{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "ok", DurationMS: int64(i)},
		}}
		if _, err := recordProofCheckTimingHistory(homePath, report, time.Minute, now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("record run %d: %v", i, err)
		}
	}

	store := readStoreForTest(t, homePath)
	if len(store.Runs) != proofCheckTimingsMaxRuns {
		t.Fatalf("want %d runs after trim, got %d", proofCheckTimingsMaxRuns, len(store.Runs))
	}
	// FIFO: the OLDEST are evicted, so the first surviving run is #7.
	if got := store.Runs[0].Steps[0].DurationMS; got != 7 {
		t.Fatalf("oldest surviving run duration = %d, want 7 (eviction must drop the oldest)", got)
	}
	if got := store.Runs[len(store.Runs)-1].Steps[0].DurationMS; got != int64(total-1) {
		t.Fatalf("newest run duration = %d, want %d", got, total-1)
	}
}

// Telemetry must never block its own repair. A corrupt history has no correctness consequence, so
// unlike a corrupt lease it resets and reports rather than failing hard — otherwise every
// subsequent run would fail to write and the history could never recover.
func TestProofCheckTimingHistoryResetsUnreadableBodyAndKeepsRecording(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	statePath := filepath.Join(homePath, proofCheckTimingsStateRelPath)

	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("{not json at all"), 0o644); err != nil {
		t.Fatalf("seed corrupt body: %v", err)
	}

	report := timingFixtureReport("", map[string]int64{"go-test": 500})
	warning, err := recordProofCheckTimingHistory(homePath, report, time.Minute, now)
	if err != nil {
		t.Fatalf("a corrupt history must not fail the record: %v", err)
	}
	if warning == "" {
		t.Fatal("resetting a corrupt history must warn the operator, not do it silently")
	}

	store := readStoreForTest(t, homePath)
	if len(store.Runs) != 1 {
		t.Fatalf("want the new run recorded after reset, got %d runs", len(store.Runs))
	}
}

// This machine runs several agent harnesses at once by design, so two batteries finishing together
// is a normal event. Without the flock the read-modify-write loses runs or writes a torn body.
func TestProofCheckTimingHistorySurvivesConcurrentWriters(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	const writers = 12
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			report := proofCheckReport{Status: "ok", Steps: []proofCheckStep{
				{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "ok", DurationMS: int64(i + 1)},
			}}
			_, errs[i] = recordProofCheckTimingHistory(homePath, report, time.Minute, now)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	store := readStoreForTest(t, homePath)
	if len(store.Runs) != writers {
		t.Fatalf("want all %d concurrent runs recorded, got %d (lost update)", writers, len(store.Runs))
	}
	seen := map[int64]bool{}
	for _, run := range store.Runs {
		seen[run.Steps[0].DurationMS] = true
	}
	for i := 1; i <= writers; i++ {
		if !seen[int64(i)] {
			t.Fatalf("run with duration %d was lost to a racing writer", i)
		}
	}
}

func TestProofCheckTimingsFileIsValidJSONWithStableSchema(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	report := timingFixtureReport("", map[string]int64{"go-test": 1})
	if _, err := recordProofCheckTimingHistory(homePath, report, time.Minute, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(homePath, proofCheckTimingsStateRelPath))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("history body must be valid JSON: %v", err)
	}
	if probe["schema_version"] != proofCheckTimingsSchemaVersion {
		t.Fatalf("schema_version = %v, want %s", probe["schema_version"], proofCheckTimingsSchemaVersion)
	}
}

func TestPercentileMSUsesNearestRank(t *testing.T) {
	t.Parallel()
	cases := []struct {
		values []int64
		p      int
		want   int64
	}{
		{nil, 50, 0},
		{[]int64{7}, 50, 7},
		{[]int64{7}, 90, 7},
		{[]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 50, 5},
		{[]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 90, 9},
		{[]int64{10, 1, 5}, 50, 5},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("p%d_n%d", tc.p, len(tc.values)), func(t *testing.T) {
			if got := percentileMS(tc.values, tc.p); got != tc.want {
				t.Fatalf("percentileMS(%v, %d) = %d, want %d", tc.values, tc.p, got, tc.want)
			}
		})
	}
}

// The defect this fixes, reproduced: the landing ceremony mandates running the battery in a
// rehearsal worktree, so grouping runs by worktree PATH excluded exactly the runs that matter.
// Live symptom on the day it landed: `--timings` in the main tree reported runs=2, gates=[].
func TestProofCheckTimingStatsFoldWorktreesOfOneRepoButSplitDifferentRepos(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	// Same repository, two different worktrees — a rehearsal tree and the main tree.
	sameRepoID := "/repo/.git"
	for i, worktree := range []string{"/tmp/land-rehearsal", "/fixture/main-tree"} {
		store, _, err := readProofCheckTimings(filepath.Join(homePath, proofCheckTimingsStateRelPath))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		store.SchemaVersion = proofCheckTimingsSchemaVersion
		store.Runs = append(store.Runs, proofCheckTimingRun{
			GeneratedAt:  now.Add(time.Duration(i) * time.Hour).Format(time.RFC3339),
			Repo:         worktree,
			RepoIdentity: sameRepoID,
			Status:       "ok",
			Steps: []proofCheckTimingStep{
				{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "ok", DurationMS: 450_000},
			},
		})
		// A run from an unrelated module must NOT fold in — this is the case the field exists for.
		if i == 0 {
			store.Runs = append(store.Runs, proofCheckTimingRun{
				GeneratedAt:  now.Format(time.RFC3339),
				Repo:         "/tmp/fixture-module",
				RepoIdentity: "/tmp/fixture-module/.git",
				Status:       "ok",
				Steps: []proofCheckTimingStep{
					{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "ok", DurationMS: 441},
				},
			})
		}
		body, err := json.MarshalIndent(store, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(homePath, "state"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(homePath, proofCheckTimingsStateRelPath), body, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	store := readStoreForTest(t, homePath)
	if len(store.Runs) != 3 {
		t.Fatalf("fixture should hold 3 runs, got %d", len(store.Runs))
	}

	stats := proofCheckTimingStats(store, "", sameRepoID, 20)
	if len(stats) != 1 {
		t.Fatalf("want 1 gate, got %d", len(stats))
	}
	// Both worktrees of this repo contribute; the foreign module's 441ms does not.
	if stats[0].Samples != 2 {
		t.Fatalf("want 2 samples (both worktrees of the same repo), got %d", stats[0].Samples)
	}
	if stats[0].P50MS != 450_000 {
		t.Fatalf("p50 = %d, want 450000 — a foreign repo's 441ms must not fold in", stats[0].P50MS)
	}
}

// Records written before RepoIdentity existed carry only a path; they must not vanish silently.
func TestProofCheckTimingStatsFallBackToPathForPreIdentityRecords(t *testing.T) {
	t.Parallel()
	store := proofCheckTimingsStore{
		SchemaVersion: proofCheckTimingsSchemaVersion,
		Runs: []proofCheckTimingRun{{
			GeneratedAt: "2026-07-26T12:00:00Z",
			Repo:        "/fixture/main-tree",
			Status:      "ok",
			Steps: []proofCheckTimingStep{
				{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "ok", DurationMS: 1234},
			},
		}},
	}
	if got := proofCheckTimingStats(store, "", "/fixture/main-tree", 20); len(got) != 1 || got[0].Samples != 1 {
		t.Fatalf("a pre-identity record must still match on its path, got %+v", got)
	}
	if got := proofCheckTimingStats(store, "", "/somewhere/else", 20); len(got) != 0 {
		t.Fatalf("a pre-identity record must not match a different path, got %+v", got)
	}
}

// proofCheckRepoIdentity must return the SAME key for two real worktrees of one repository, and a
// different key for an unrelated repository. Uses real git because that is the behaviour relied on.
func TestProofCheckRepoIdentityIsSharedAcrossWorktreesOfOneRepo(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	repoA := t.TempDir()
	run(repoA, "init", "-q", ".")
	run(repoA, "config", "user.email", "t@example.invalid")
	run(repoA, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoA, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repoA, "add", "-A")
	run(repoA, "commit", "-qm", "init")

	worktree := filepath.Join(t.TempDir(), "wt")
	run(repoA, "worktree", "add", "--detach", worktree)

	idMain := proofCheckRepoIdentity(repoA)
	idWorktree := proofCheckRepoIdentity(worktree)
	if idMain == "" || idMain != idWorktree {
		t.Fatalf("a worktree must share its repo's identity: main=%q worktree=%q", idMain, idWorktree)
	}

	repoB := t.TempDir()
	run(repoB, "init", "-q", ".")
	if idB := proofCheckRepoIdentity(repoB); idB == idMain {
		t.Fatalf("an unrelated repository must not share identity %q", idB)
	}
}

// percentileMS must not reorder its input: the caller's slice order carries the run sequence.
func TestPercentileMSDoesNotMutateCallerSlice(t *testing.T) {
	t.Parallel()
	values := []int64{9, 3, 7}
	_ = percentileMS(values, 90)
	if values[0] != 9 || values[1] != 3 || values[2] != 7 {
		t.Fatalf("percentileMS mutated its input: %v", values)
	}
}
