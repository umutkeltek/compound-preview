package jobprobe_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/director"
	"compound/internal/jobprobe"
	"compound/internal/routeevidence"
)

var falsifierNow = time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)

type deadFakeRunner struct{ dead map[string]string }

func (r deadFakeRunner) Probe(target jobprobe.ProbeTarget, _ time.Time) (jobprobe.ProbeResult, error) {
	key := target.JobID
	if key == "" {
		key = target.Name
	}
	if detail, ok := r.dead[key]; ok {
		return jobprobe.ProbeResult{Alive: false, Detail: detail}, nil
	}
	return jobprobe.ProbeResult{Alive: true, Detail: "fake: alive"}, nil
}

func writeFalsifierTargets(t *testing.T, home string, targets jobprobe.ProbeTargetsFile) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(jobprobe.ProbeTargetsPath(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(targets, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobprobe.ProbeTargetsPath(home), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasJobHealthProposalFor(report director.Report, jobName string) bool {
	for _, p := range report.Proposals {
		if p.Kind == "job_health" && strings.Contains(p.Title, jobName) {
			return true
		}
	}
	return false
}

// TestCollectorRunMakesDirectorSurfaceDeadUnit is the full-chain falsifier the
// envelope names: a roster row the snapshot claims is ALIVE, whose real unit a
// probe reports DEAD, must (a) be invisible to `director propose --dry-run`
// BEFORE the collector runs, and (b) surface as a job_health proposal AFTER one
// collector run refreshes it to the observed failure state. The RED half (before
// the collector) is asserted first, so the test would fail if the collector did
// nothing.
func TestCollectorRunMakesDirectorSurfaceDeadUnit(t *testing.T) {
	home := t.TempDir()

	// Route fixture so the job_health (judge-critique) capability route resolves;
	// without it the proposal would be skipped for reasons unrelated to the probe.
	routeSource := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, routeSource, falsifierNow); err != nil {
		t.Fatalf("import route fixture: %v", err)
	}

	// Import a HEALTHY roster: the blind snapshot claims the unit is alive/ok.
	rosterSource := filepath.Join(t.TempDir(), "cron-jobs.json")
	roster := `{
  "version": 1,
  "generated_at": "2026-07-13T08:00:00Z",
  "jobs": [
    {
      "job_id": "example-nightly-worker",
      "name": "example nightly worker",
      "enabled": true,
      "state": "scheduled",
      "schedule": "every day",
      "script": "worker.sh",
      "last_status": "ok",
      "last_run_at": "2026-07-13T07:30:00Z"
    }
  ]
}`
	if err := os.WriteFile(rosterSource, []byte(roster), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ImportJobRoster(home, rosterSource, falsifierNow); err != nil {
		t.Fatalf("import roster: %v", err)
	}

	// RED: the snapshot says alive, so the director surfaces no job_health for it.
	before, err := director.Propose(home, director.Options{Now: falsifierNow})
	if err != nil {
		t.Fatalf("director propose (before collector): %v", err)
	}
	if hasJobHealthProposalFor(before, "example nightly worker") {
		t.Fatalf("baseline must NOT surface job_health for a snapshot-alive row: %+v", before.Proposals)
	}

	// Declare a probe target for that row (private definition under COMPOUND_HOME).
	writeFalsifierTargets(t, home, jobprobe.ProbeTargetsFile{
		SchemaVersion: jobprobe.ProbeTargetsSchema,
		Targets: []jobprobe.ProbeTarget{
			{JobID: "example-nightly-worker", Name: "example nightly worker", Kind: jobprobe.KindLocalLaunchd, Unit: "com.example.nightly-worker"},
		},
	})

	// One collector run with a FAKE probe reporting the real unit dead.
	runner := deadFakeRunner{dead: map[string]string{
		"example-nightly-worker": "fake probe: launchd unit not loaded",
	}}
	collect, err := jobprobe.Collect(home, runner, falsifierNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("collector run: %v", err)
	}
	if collect.DeadCount != 1 || collect.Refresh.FailureRefreshedCount != 1 {
		t.Fatalf("collector must refresh exactly one observed-dead row: %+v", collect)
	}

	// The roster row now carries the OBSERVED failure state.
	refreshed, err := authority.LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, job := range refreshed.Jobs {
		if job.JobID != "example-nightly-worker" {
			continue
		}
		found = true
		if job.LastStatus != "error" || job.ProbeState != "dead" || job.LastError == "" {
			t.Fatalf("row not refreshed to observed failure: %+v", job)
		}
	}
	if !found {
		t.Fatalf("row missing after refresh: %+v", refreshed.Jobs)
	}

	// GREEN: `director propose --dry-run` now surfaces the observed failure.
	after, err := director.Propose(home, director.Options{Now: falsifierNow.Add(time.Hour)})
	if err != nil {
		t.Fatalf("director propose (after collector): %v", err)
	}
	if !hasJobHealthProposalFor(after, "example nightly worker") {
		t.Fatalf("one collector run must make the director surface the observed-dead row: %+v", after.Proposals)
	}
}
