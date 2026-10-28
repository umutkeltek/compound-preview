package authority

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestImportJobRosterWritesAuthorityAndSourceRecord(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "cron-jobs.json")
	writeJobRosterFixture(t, source)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	report, err := ImportJobRoster(home, source, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.AuthorityRef != JobRosterAuthorityRef || report.JobCount != 3 || report.EnabledCount != 2 || report.ErrorCount != 1 || report.RecentRunCount != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.RecentErrors) != 1 || report.RecentErrors[0].Name != "ibkr-exit-check-daily" {
		t.Fatalf("recent errors = %+v", report.RecentErrors)
	}

	authority, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if authority.AuthorityRef != JobRosterAuthorityRef || authority.SourcePath != source || authority.SourceRevision == "" {
		t.Fatalf("authority = %+v", authority)
	}
	if len(authority.Jobs) != 3 || authority.Jobs[0].Name != "brain digest" {
		t.Fatalf("jobs should be stable sorted: %+v", authority.Jobs)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("job roster import must not promote claims, stat err=%v", err)
	}

	records, err := LoadJobRosterSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].AuthorityRef != JobRosterAuthorityRef || records[0].SourceRole != "evidence_only_input_after_import" {
		t.Fatalf("source records = %+v", records)
	}

	replayed, err := ImportJobRoster(home, source, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	records, err = LoadJobRosterSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || replayed.SourceAuthorityID != records[0].ID {
		t.Fatalf("replay should be idempotent: report=%+v records=%+v", replayed, records)
	}
}

func TestLoadJobRosterAuthorityNormalizesStoredDerivedFields(t *testing.T) {
	home := t.TempDir()
	writeJobRosterAuthorityFixture(t, home, JobRosterAuthority{
		SchemaVersion:  JobRosterAuthoritySchema,
		AuthorityRef:   JobRosterAuthorityRef,
		SourcePath:     "jobs.json",
		SourceRevision: "sha256:stale",
		JobCount:       1,
		EnabledCount:   1,
		ErrorCount:     1,
		RecentErrors: []JobSummary{
			{JobID: "enabled-error", Name: "enabled failing", Enabled: true, LastStatus: "error"},
		},
		Jobs: []JobSummary{
			{JobID: "disabled-error", Name: "disabled failing", Enabled: false, LastStatus: "error"},
			{JobID: "enabled-error", Name: "enabled failing", Enabled: true, LastStatus: "error"},
			{JobID: "ok", Name: "healthy", Enabled: true, LastStatus: "ok"},
		},
	})

	authority, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if authority.JobCount != 3 || authority.EnabledCount != 2 || authority.ErrorCount != 2 {
		t.Fatalf("derived counters should reflect jobs, got %+v", authority)
	}
	if len(authority.RecentErrors) != 2 || authority.RecentErrors[0].Name != "disabled failing" || authority.RecentErrors[1].Name != "enabled failing" {
		t.Fatalf("recent errors should be rebuilt from current jobs: %+v", authority.RecentErrors)
	}
	if authority.ActiveErrorCount != 1 || len(authority.ActiveErrors) != 1 || authority.ActiveErrors[0].Name != "enabled failing" {
		t.Fatalf("active errors should include only actionable enabled failures: %+v", authority)
	}
	if authority.InactiveErrorCount != 1 || len(authority.InactiveErrors) != 1 || authority.InactiveErrors[0].Name != "disabled failing" {
		t.Fatalf("inactive errors should preserve disabled historical failures: %+v", authority)
	}
}

func TestImportJobTokenGovernanceOverlaysRosterAuthority(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "cron-jobs.json")
	writeJobRosterFixture(t, source)
	usage := filepath.Join(t.TempDir(), "cron-token-usage.json")
	writeJobTokenUsageFixture(t, usage)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := ImportJobRoster(home, source, now); err != nil {
		t.Fatal(err)
	}
	stale, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	stale.ErrorCount = 0
	stale.RecentErrors = nil
	writeJobRosterAuthorityFixture(t, home, stale)

	report, err := ImportJobTokenGovernance(home, usage, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.TokenSessionsCount != 12 || report.EnabledAgentCount != 2 || report.HighTokenEnabledCount != 1 {
		t.Fatalf("token governance report = %+v", report)
	}
	if len(report.TokenRiskJobs) != 1 || report.TokenRiskJobs[0].Name != "brain digest" {
		t.Fatalf("token risk jobs = %+v", report.TokenRiskJobs)
	}
	authority, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if authority.TokenMetric != "all_tokens" || authority.TokenSessionsCount != 12 || authority.HighTokenEnabledCount != 1 {
		t.Fatalf("authority token fields = %+v", authority)
	}
	stored := readRawJobRosterAuthority(t, home)
	if stored.ErrorCount != 1 || len(stored.RecentErrors) != 1 || stored.RecentErrors[0].Name != "ibkr-exit-check-daily" {
		t.Fatalf("stored authority counters should be normalized after token overlay: %+v", stored)
	}
	if stored.ActiveErrorCount != 1 || len(stored.ActiveErrors) != 1 || stored.InactiveErrorCount != 0 {
		t.Fatalf("stored active error counters should be normalized after token overlay: %+v", stored)
	}
	var digest JobSummary
	for _, job := range authority.Jobs {
		if job.JobID == "digest" {
			digest = job
		}
	}
	if digest.TotalTokens != 1500000 || digest.AvgTokensPerRun != 300000 || digest.RiskClass != "high_token_agent_job" {
		t.Fatalf("digest token fields = %+v", digest)
	}
	records, err := LoadJobRosterSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].ImportedVia != jobTokenImportedVia || records[1].HighTokenEnabledCount != 1 {
		t.Fatalf("source authority records = %+v", records)
	}
}

func writeJobRosterAuthorityFixture(t *testing.T, home string, authority JobRosterAuthority) {
	t.Helper()
	path := JobRosterAuthorityPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(authority, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readRawJobRosterAuthority(t *testing.T, home string) JobRosterAuthority {
	t.Helper()
	data, err := os.ReadFile(JobRosterAuthorityPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var authority JobRosterAuthority
	if err := json.Unmarshal(data, &authority); err != nil {
		t.Fatal(err)
	}
	return authority
}

func writeJobRosterFixture(t *testing.T, path string) {
	t.Helper()
	data := `{
  "version": 1,
  "generated_at": "2026-07-01T11:00:00Z",
  "jobs": [
    {
      "job_id": "d422776dde1c",
      "name": "ibkr-exit-check-daily",
      "enabled": true,
      "state": "scheduled",
      "schedule": "33 19 * * 1-5",
      "script": "ibkr_exit_check_daily.sh",
      "no_agent": true,
      "lane": "no_agent_script",
      "last_status": "error",
      "last_error": "missing python venv",
      "last_run_at": "2026-07-01T10:00:00Z",
      "recommended_action": "keep_no_agent"
    },
    {
      "job_id": "digest",
      "name": "brain digest",
      "enabled": true,
      "state": "scheduled",
      "schedule": "every 6h",
      "script": "recall_digest.ts",
      "last_status": "ok",
      "last_run_at": "2026-06-28T10:00:00Z"
    },
    {
      "job_id": "off",
      "name": "disabled cleanup",
      "enabled": false,
      "state": "manual",
      "script": "cleanup.sh"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeJobTokenUsageFixture(t *testing.T, path string) {
	t.Helper()
	data := `{
  "version": 1,
  "generated_at": "2026-07-01T12:00:00Z",
  "metric": "all_tokens",
  "sessions_count": 12,
  "jobs": [
    {
      "job_id": "digest",
      "name": "brain digest",
      "enabled": true,
      "no_agent": false,
      "runs": 5,
      "total_tokens": 1500000,
      "avg_tokens_per_run": 300000,
      "risk_class": "high_token_agent_job"
    },
    {
      "job_id": "d422776dde1c",
      "name": "ibkr-exit-check-daily",
      "enabled": true,
      "no_agent": true,
      "runs": 2,
      "total_tokens": 0,
      "avg_tokens_per_run": 0,
      "risk_class": "no_agent"
    },
    {
      "job_id": "other-agent",
      "name": "other agent",
      "enabled": true,
      "no_agent": false,
      "runs": 1,
      "total_tokens": 42,
      "avg_tokens_per_run": 42,
      "risk_class": "ok"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
