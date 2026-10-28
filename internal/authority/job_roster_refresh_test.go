package authority

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var refreshTestNow = time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)

func importRefreshFixtureRoster(t *testing.T, home, body string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "cron-jobs.json")
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportJobRoster(home, source, refreshTestNow); err != nil {
		t.Fatalf("import roster fixture: %v", err)
	}
}

func rosterRow(t *testing.T, roster JobRosterAuthority, jobID string) JobSummary {
	t.Helper()
	for _, job := range roster.Jobs {
		if job.JobID == jobID {
			return job
		}
	}
	t.Fatalf("row %q missing from roster: %+v", jobID, roster.Jobs)
	return JobSummary{}
}

// TestRefreshBridgesDeadObservationToError is the core authority falsifier: a
// row the snapshot claims is alive/ok, once a probe observes it dead, is
// refreshed to the observed failure state (LastStatus=error) so it can surface
// through BuildJobHealthPlans — which it must NOT do before the refresh.
func TestRefreshBridgesDeadObservationToError(t *testing.T) {
	home := t.TempDir()
	importRefreshFixtureRoster(t, home, `{
  "version": 1,
  "jobs": [
    {"job_id": "alive-worker", "name": "alive worker", "enabled": true, "state": "scheduled", "last_status": "ok", "last_run_at": "2026-07-13T07:30:00Z"}
  ]
}`)

	// Baseline: the snapshot-alive row yields no job-health plan.
	before, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if plans, err := BuildJobHealthPlans(home, before, refreshTestNow); err != nil {
		t.Fatal(err)
	} else if len(plans) != 0 {
		t.Fatalf("baseline snapshot-alive row must not yield a job-health plan, got %d", len(plans))
	}

	report, err := RefreshJobRosterFromObservations(home, "jobprobe:test", []JobRosterObservation{
		{JobID: "alive-worker", Alive: false, Detail: "fake probe: unit not loaded", ObservedAt: refreshTestNow.Format(time.RFC3339)},
	}, refreshTestNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if report.MatchedCount != 1 || report.DeadCount != 1 || report.FailureRefreshedCount != 1 {
		t.Fatalf("expected one matched dead row refreshed to failure, got %+v", report)
	}
	if report.SourceAuthorityID == "" {
		t.Fatalf("refresh must record a source-authority provenance record: %+v", report)
	}

	after, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	row := rosterRow(t, after, "alive-worker")
	if row.LastStatus != "error" {
		t.Fatalf("dead observation must bridge LastStatus to error, got %q", row.LastStatus)
	}
	if row.ProbeState != "dead" {
		t.Fatalf("row must carry observed probe state dead, got %q", row.ProbeState)
	}
	if row.LastError != "fake probe: unit not loaded" {
		t.Fatalf("row must carry observed failure detail, got %q", row.LastError)
	}
	if after.ProbeRefreshedAt == "" {
		t.Fatalf("authority must record probe_refreshed_at after a refresh")
	}

	plans, err := BuildJobHealthPlans(home, after, refreshTestNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].JobName != "alive worker" {
		t.Fatalf("refreshed dead row must yield exactly one job-health plan, got %+v", plans)
	}
}

// TestRefreshAliveClearsProbeDerivedError is the recovery falsifier (audit E8):
// a row whose LastStatus=error / LastError came from a PRIOR probe observation
// (not from the snapshot import) must have those fields cleared when a
// subsequent probe observes the unit alive. Otherwise recovered jobs stay red
// forever, inflating the operator's needs_you backlog with error claims the
// probe itself wrote and should retract. The existing
// TestRefreshAliveDoesNotDowngradeExistingError pins the asymmetric bridge for
// imported errors — this test pins the symmetric bridge for probe-derived
// errors.
func TestRefreshAliveClearsProbeDerivedError(t *testing.T) {
	home := t.TempDir()
	importRefreshFixtureRoster(t, home, `{
  "version": 1,
  "jobs": [
    {"job_id": "recovered-job", "name": "recovered job", "enabled": true, "state": "scheduled", "last_status": "ok", "last_run_at": "2026-07-13T07:30:00Z"}
  ]
}`)

	// First pass: probe observes the unit dead → we write LastStatus=error.
	if _, err := RefreshJobRosterFromObservations(home, "jobprobe:test", []JobRosterObservation{
		{JobID: "recovered-job", Alive: false, Detail: "unit not loaded", ObservedAt: refreshTestNow.Format(time.RFC3339)},
	}, refreshTestNow.Add(time.Hour)); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	mid, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	row := rosterRow(t, mid, "recovered-job")
	if row.LastStatus != "error" || row.LastError != "unit not loaded" || row.ProbeState != "dead" {
		t.Fatalf("after first dead pass want status=error error=%q state=dead, got status=%q error=%q state=%q",
			"unit not loaded", row.LastStatus, row.LastError, row.ProbeState)
	}

	// Second pass: probe observes the unit alive → probe-derived error MUST clear.
	report, err := RefreshJobRosterFromObservations(home, "jobprobe:test", []JobRosterObservation{
		{JobID: "recovered-job", Alive: true, Detail: "unit loaded", ObservedAt: refreshTestNow.Format(time.RFC3339)},
	}, refreshTestNow.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("recovery refresh: %v", err)
	}
	if report.AliveCount != 1 {
		t.Fatalf("alive observation count = %d, want 1", report.AliveCount)
	}

	after, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	row2 := rosterRow(t, after, "recovered-job")
	if row2.LastStatus == "error" {
		t.Fatalf("alive observation must clear probe-derived LastStatus=error, still got %q", row2.LastStatus)
	}
	if row2.LastError != "" {
		t.Fatalf("alive observation must clear probe-derived LastError, still got %q", row2.LastError)
	}
	if row2.ProbeState != "alive" {
		t.Fatalf("row must record ProbeState=alive, got %q", row2.ProbeState)
	}

	// And it must NOT yield a job-health plan now that the probe-derived error
	// has been retracted (the recovered job is no longer actionable).
	plans, err := BuildJobHealthPlans(home, after, refreshTestNow.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 {
		t.Fatalf("recovered row must not yield a job-health plan, got %d", len(plans))
	}
}

// TestRefreshAliveDoesNotDowngradeExistingError proves the asymmetric bridge: an
// observed-alive verdict records probe metadata but never clears a genuine
// imported error (a loaded unit can still have a failed last run).
func TestRefreshAliveDoesNotDowngradeExistingError(t *testing.T) {
	home := t.TempDir()
	importRefreshFixtureRoster(t, home, `{
  "version": 1,
  "jobs": [
    {"job_id": "erroring-job", "name": "erroring job", "enabled": true, "state": "scheduled", "last_status": "error", "last_error": "imported failure", "last_run_at": "2026-07-13T07:30:00Z"}
  ]
}`)
	report, err := RefreshJobRosterFromObservations(home, "jobprobe:test", []JobRosterObservation{
		{JobID: "erroring-job", Alive: true, Detail: "unit loaded"},
	}, refreshTestNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if report.AliveCount != 1 || report.FailureRefreshedCount != 0 {
		t.Fatalf("alive observation must not refresh a failure, got %+v", report)
	}
	after, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	row := rosterRow(t, after, "erroring-job")
	if row.LastStatus != "error" || row.LastError != "imported failure" {
		t.Fatalf("alive verdict must not clear an existing imported error, got status=%q error=%q", row.LastStatus, row.LastError)
	}
	if row.ProbeState != "alive" {
		t.Fatalf("row must record the observed alive probe state, got %q", row.ProbeState)
	}
}

// TestRefreshUnmatchedObservationIsReported proves an observation with no
// matching row does not silently vanish and mutates no row.
func TestRefreshUnmatchedObservationIsReported(t *testing.T) {
	home := t.TempDir()
	importRefreshFixtureRoster(t, home, `{
  "version": 1,
  "jobs": [
    {"job_id": "known-job", "name": "known job", "enabled": true, "state": "scheduled", "last_status": "ok"}
  ]
}`)
	report, err := RefreshJobRosterFromObservations(home, "jobprobe:test", []JobRosterObservation{
		{JobID: "ghost-job", Alive: false, Detail: "dead"},
	}, refreshTestNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if report.MatchedCount != 0 || len(report.UnmatchedRefs) != 1 {
		t.Fatalf("unmatched observation must be reported, got %+v", report)
	}
	after, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	row := rosterRow(t, after, "known-job")
	if row.LastStatus != "ok" || row.ProbeState != "" {
		t.Fatalf("unmatched observation must not mutate an unrelated row, got %+v", row)
	}
}
