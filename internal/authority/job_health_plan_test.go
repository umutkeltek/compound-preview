package authority

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

func TestJobHealthPlanRecordsReviewWithoutMutatingScheduler(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "cron-jobs.json")
	writeJobRosterFixture(t, source)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := ImportJobRoster(home, source, now); err != nil {
		t.Fatal(err)
	}
	roster, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}

	report, err := RecordJobHealthPlans(home, roster, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != JobHealthPlanReportSchemaVersion ||
		report.PlanCount != 1 ||
		len(report.Plans) != 1 ||
		report.Plans[0].Status != "review_required" ||
		report.Plans[0].JobName != "ibkr-exit-check-daily" ||
		report.Plans[0].RepairCount != 1 {
		t.Fatalf("bad job health report: %+v", report)
	}
	if len(report.Paths) != 1 || len(report.ReviewYAMLs) != 1 {
		t.Fatalf("paths = %+v review_yamls=%+v", report.Paths, report.ReviewYAMLs)
	}
	review, err := os.ReadFile(report.Paths[0].ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(review)
	for _, want := range []string{"does not run", "Required Evidence", "Review YAML Template", "ibkr-exit-check-daily", "missing python venv"} {
		if !strings.Contains(text, want) {
			t.Fatalf("review missing %q:\n%s", want, text)
		}
	}
	draft, err := LoadJobHealthReview(report.ReviewYAMLs[0])
	if err != nil {
		t.Fatal(err)
	}
	if draft.PlanID != report.Plans[0].ID || draft.JobName != "ibkr-exit-check-daily" {
		t.Fatalf("bad review draft: %+v", draft)
	}
	if err := ValidateJobHealthReview(draft); err == nil {
		t.Fatal("unedited job-health review draft should not validate")
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("job health plan must not promote claims, stat err=%v", err)
	}
}

func TestJobHealthReviewQueueAndRecordSidecar(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "cron-jobs.json")
	writeJobRosterFixture(t, source)
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := ImportJobRoster(home, source, now); err != nil {
		t.Fatal(err)
	}
	roster, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RecordJobHealthPlans(home, roster, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: jobHealthReviewArea}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateJobHealthQueue(&queue)
	if len(queue.Items) != 1 {
		t.Fatalf("queue items = %+v", queue.Items)
	}
	if queue.Items[0].Readiness != "needs_review" || !strings.Contains(queue.Items[0].BlockedReason, "decision") {
		t.Fatalf("unedited draft should block on decision: %+v", queue.Items[0])
	}

	reviewPath := report.ReviewYAMLs[0]
	draftBytes, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	draft := string(draftBytes)
	draft = strings.ReplaceAll(draft, "decision: TODO_decision_repair_required_parked_disabled_no_action_owner_action_done", "decision: parked")
	draft = strings.ReplaceAll(draft, "rationale: \"\"", "rationale: \"owner reviewed missing dependency; keep parked until repair owner acts\"")
	draft = strings.ReplaceAll(draft, "confidence: -1", "confidence: 0.82")
	draft = strings.ReplaceAll(draft, "next_action: \"TODO: reviewed job-health next action\"", "next_action: \"keep parked; re-import roster after dependency repair\"")
	if err := os.WriteFile(reviewPath, []byte(draft), 0o644); err != nil {
		t.Fatal(err)
	}

	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: jobHealthReviewArea}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateJobHealthQueue(&queue)
	if queue.Items[0].Readiness != "ready_to_record" || len(queue.Items[0].Actions) != 1 || !strings.Contains(queue.Items[0].Actions[0].Command, "job-health-review") {
		t.Fatalf("valid draft should be recordable: %+v", queue.Items[0])
	}
	review, err := LoadJobHealthReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := RecordJobHealthReview(home, review, reviewPath, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "recorded" || record.Decision != "parked" || record.ReviewPath == "" {
		t.Fatalf("bad record report: %+v", record)
	}
	if _, err := os.Stat(record.ReviewPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("job-health review must not mutate scheduler source")
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("job-health review must not promote claims, stat err=%v", err)
	}
	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: jobHealthReviewArea}, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateJobHealthQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "review_recorded" || !strings.Contains(queue.Items[0].NextAction, "keep parked") {
		t.Fatalf("recorded review should annotate queue: %+v", queue.Items[0])
	}
	if queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 {
		t.Fatalf("recorded review should close review debt counts: %+v", queue.Counts)
	}
	replanned, err := RecordJobHealthPlans(home, roster, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replanned.PlanCount != 1 || replanned.ReviewRequiredCount != 0 || replanned.RecordedCount != 1 || len(replanned.Plans) != 1 {
		t.Fatalf("recorded job-health plan should not reopen review debt: %+v", replanned)
	}
	if replanned.Plans[0].Status != "recorded" || !strings.Contains(replanned.Plans[0].NextAction, "keep parked") {
		t.Fatalf("recorded job-health plan status not preserved: %+v", replanned.Plans[0])
	}
}

func TestJobHealthQueueDemotesSupersededRosterErrors(t *testing.T) {
	home := t.TempDir()
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "cron-jobs.json")
	writeJobRosterTwoErrorFixture(t, source)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := ImportJobRoster(home, source, now); err != nil {
		t.Fatal(err)
	}
	roster, err := LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RecordJobHealthPlans(home, roster, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.PlanCount != 2 {
		t.Fatalf("expected two job-health plans, got %+v", report)
	}
	planIDByName := map[string]string{}
	for _, plan := range report.Plans {
		planIDByName[plan.JobName] = plan.ID
	}

	current := filepath.Join(sourceDir, "cron-jobs-current.json")
	writeJobRosterFixture(t, current)
	if _, err := ImportJobRoster(home, current, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: jobHealthReviewArea}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateJobHealthQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.Total != 2 || queue.Counts.ReviewRequired != 1 || queue.Counts.OK != 1 {
		t.Fatalf("queue counts should reflect current roster authority: %+v", queue.Counts)
	}
	for _, item := range queue.Items {
		switch item.ID {
		case planIDByName["stale auth job"]:
			if item.Status != "resolved" || item.Readiness != "no_action" || len(item.Actions) != 0 {
				t.Fatalf("stale job should be terminal projection: %+v", item)
			}
		case planIDByName["ibkr-exit-check-daily"]:
			if item.Status != "review_required" || item.Readiness != "needs_review" || !strings.Contains(item.BlockedReason, "decision") {
				t.Fatalf("current failing job should still need review: %+v", item)
			}
		default:
			t.Fatalf("unexpected queue item: %+v", item)
		}
	}
}

func TestJobHealthPlanNoopsWhenRosterHealthy(t *testing.T) {
	home := t.TempDir()
	roster := JobRosterAuthority{
		SchemaVersion:  JobRosterAuthoritySchema,
		AuthorityRef:   JobRosterAuthorityRef,
		SourcePath:     "jobs.json",
		SourceRevision: "sha256:healthy",
		Jobs: []JobSummary{
			{JobID: "ok", Name: "healthy job", LastStatus: "ok"},
		},
	}
	report, err := RecordJobHealthPlans(home, roster, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.PlanCount != 0 || len(report.Plans) != 0 || len(report.Paths) != 0 {
		t.Fatalf("healthy roster should not write plans: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(home, "reviews", "job-health")); !os.IsNotExist(err) {
		t.Fatalf("healthy roster should not create review dir, stat err=%v", err)
	}
}

func writeJobRosterTwoErrorFixture(t *testing.T, path string) {
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
      "last_status": "error",
      "last_error": "missing python venv",
      "last_run_at": "2026-07-01T10:00:00Z"
    },
    {
      "job_id": "stale-auth",
      "name": "stale auth job",
      "enabled": true,
      "state": "scheduled",
      "schedule": "every 6h",
      "script": "auth_refresh.sh",
      "last_status": "error",
      "last_error": "old token consumed",
      "last_run_at": "2026-06-30T10:00:00Z"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestJobHealthPlanUsesOnlyActiveErrors(t *testing.T) {
	home := t.TempDir()
	roster := JobRosterAuthority{
		AuthorityRef:   JobRosterAuthorityRef,
		SourcePath:     "jobs.json",
		SourceRevision: "sha256:active-split",
		RecentErrors: []JobSummary{
			{JobID: "paused", Name: "paused historical", Enabled: false, State: "paused", LastStatus: "error"},
			{JobID: "active", Name: "active failure", Enabled: true, State: "scheduled", LastStatus: "error"},
		},
		ActiveErrors: []JobSummary{
			{JobID: "active", Name: "active failure", Enabled: true, State: "scheduled", LastStatus: "error"},
		},
		InactiveErrors: []JobSummary{
			{JobID: "paused", Name: "paused historical", Enabled: false, State: "paused", LastStatus: "error"},
		},
	}
	report, err := RecordJobHealthPlans(home, roster, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.PlanCount != 1 || len(report.Plans) != 1 || report.Plans[0].JobName != "active failure" {
		t.Fatalf("job-health should plan only active failures: %+v", report)
	}
}
