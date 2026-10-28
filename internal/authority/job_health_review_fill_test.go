package authority

import (
	"os"
	"testing"
	"time"
)

// syntheticFailingJobHealthDraft builds a one-job roster with a single active
// failure (synthetic job name — privacy law: no personal/real job names in
// tests) and returns the home dir plus the path to the unfilled
// job-health-review draft it produces. Mirrors TestJobHealthPlanUsesOnlyActiveErrors'
// direct-roster-construction pattern rather than the JSON-import path, since
// only the plan -> review-draft seam matters here.
func syntheticFailingJobHealthDraft(t *testing.T) (home, reviewPath string) {
	t.Helper()
	home = t.TempDir()
	roster := JobRosterAuthority{
		AuthorityRef:   JobRosterAuthorityRef,
		SourcePath:     "jobs.json",
		SourceRevision: "sha256:fill-fixture",
		ActiveErrors: []JobSummary{
			{JobID: "svc-alpha", Name: "svc-alpha backup", Enabled: true, State: "scheduled", LastStatus: "error", LastError: "disk full"},
		},
	}
	report, err := RecordJobHealthPlans(home, roster, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ReviewYAMLs) != 1 {
		t.Fatalf("expected one review draft, got %+v", report.ReviewYAMLs)
	}
	return home, report.ReviewYAMLs[0]
}

// Falsifier 1 (RED first — FillJobHealthReviewDraft/JobHealthReviewFillInput did
// not exist before this change, so this test could not even compile): filling
// a TODO draft writes decision/rationale/confidence/reviewed_at back to the
// SAME YAML path, and the existing Load+Validate+Record path then succeeds
// unchanged on the filled file.
func TestFillJobHealthReviewDraftRecordsFilledDraft(t *testing.T) {
	home, reviewPath := syntheticFailingJobHealthDraft(t)
	now := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)

	if err := FillJobHealthReviewDraft(reviewPath, JobHealthReviewFillInput{
		Decision:  "parked",
		Rationale: "x",
	}, now); err != nil {
		t.Fatalf("fill should succeed on an unfilled TODO draft: %v", err)
	}

	filled, err := LoadJobHealthReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if filled.Decision != "parked" || filled.Rationale != "x" || filled.Confidence != 1 {
		t.Fatalf("filled draft not written correctly on disk: %+v", filled)
	}
	if filled.ReviewedAt == "" {
		t.Fatalf("filled draft missing reviewed_at stamp: %+v", filled)
	}
	if filled.NextAction != "revisit when the job matters again" {
		t.Fatalf("filled draft next_action should use the parked default, got %q", filled.NextAction)
	}
	if err := ValidateJobHealthReview(filled); err != nil {
		t.Fatalf("filled draft should validate: %v", err)
	}

	report, err := RecordJobHealthReview(home, filled, reviewPath, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("record should succeed on filled draft: %v", err)
	}
	if report.Status != "recorded" || report.Decision != "parked" {
		t.Fatalf("bad record report: %+v", report)
	}
}

// Falsifier 2: an untouched TODO draft (Fill never invoked) must still
// produce the EXACT original error text — byte-compared against the live
// operator failure quoted in the packet — proving fill semantics are fully
// additive and change nothing about the unfilled path.
func TestUnfilledJobHealthReviewDraftKeepsOriginalError(t *testing.T) {
	_, reviewPath := syntheticFailingJobHealthDraft(t)

	draft, err := LoadJobHealthReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateJobHealthReview(draft)
	if err == nil {
		t.Fatal("unfilled draft must not validate")
	}
	want := `job-health review decision must be repair_required|parked|disabled|no_action|owner_action_done, got "TODO_decision_repair_required_parked_disabled_no_action_owner_action_done"`
	if err.Error() != want {
		t.Fatalf("error text drifted from the live operator failure:\n got:  %s\n want: %s", err.Error(), want)
	}
}

// Falsifier 3: an out-of-enum --decision is written through by Fill (it does
// not duplicate enum validation) and is then rejected by the EXISTING
// validator with its own enum error — proving there is exactly one place
// that owns the enum rule.
func TestFillJobHealthReviewDraftLeavesEnumValidationToExistingValidator(t *testing.T) {
	_, reviewPath := syntheticFailingJobHealthDraft(t)
	now := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)

	if err := FillJobHealthReviewDraft(reviewPath, JobHealthReviewFillInput{
		Decision:  "bogus",
		Rationale: "x",
	}, now); err != nil {
		t.Fatalf("fill itself should not reject an out-of-enum decision: %v", err)
	}

	filled, err := LoadJobHealthReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateJobHealthReview(filled)
	if err == nil {
		t.Fatal("bogus decision must not validate")
	}
	want := `job-health review decision must be repair_required|parked|disabled|no_action|owner_action_done, got "bogus"`
	if err.Error() != want {
		t.Fatalf("unexpected enum error: %v", err)
	}
}

// Falsifier 4: a draft whose decision is already validly recorded refuses a
// DIFFERENT --decision rather than silently overwriting it, and leaves the
// on-disk YAML byte-for-byte unchanged.
func TestFillJobHealthReviewDraftRefusesToOverwriteRecordedDecision(t *testing.T) {
	_, reviewPath := syntheticFailingJobHealthDraft(t)
	first := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)

	if err := FillJobHealthReviewDraft(reviewPath, JobHealthReviewFillInput{
		Decision:  "parked",
		Rationale: "keep watching",
	}, first); err != nil {
		t.Fatalf("first fill should succeed: %v", err)
	}
	before, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}

	second := first.Add(time.Hour)
	err = FillJobHealthReviewDraft(reviewPath, JobHealthReviewFillInput{
		Decision:  "repair_required",
		Rationale: "changed my mind",
	}, second)
	if err == nil {
		t.Fatal("re-filling an already-recorded decision with a different value must be refused")
	}
	wantErr := `job-health review already recorded as parked; edit the YAML explicitly to change a recorded decision`
	if err.Error() != wantErr {
		t.Fatalf("unexpected refusal error: %v", err)
	}

	after, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a refused re-fill must not mutate the on-disk YAML")
	}
}
