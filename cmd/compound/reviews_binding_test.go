package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

const reviewsBindingSubprocessEnv = "COMPOUND_REVIEWS_BINDING_SUBPROCESS"
const reviewsBindingArgsEnv = "COMPOUND_REVIEWS_BINDING_ARGS"

func TestReviewsBindingSubprocess(t *testing.T) {
	t.Parallel()
	if os.Getenv(reviewsBindingSubprocessEnv) == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(reviewsBindingArgsEnv)), &args); err != nil {
		t.Fatal(err)
	}
	cmdReviews(args)
}

func TestReviewsBindingPlanThenGovernedRecordPreservesCoreReview(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	paths, err := reviewartifact.Write(homePath, "panel-verdicts", "pver_binding_cli", map[string]any{
		"id": "pver_binding_cli", "status": "review_required",
	}, " ", "# panel verdict\n")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID: "work_binding_governor", Owner: "review-binding-test",
		SuccessPredicate: "binding record is governed",
	}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID: "work_panel_fallback", Owner: "panel-review-owner",
		CanonicalSurface: "review-area:panel-verdicts",
		SuccessPredicate: "panel review area remains actively owned",
	}, cliTestNow); err != nil {
		t.Fatal(err)
	}

	code, output := runReviewsBindingCLI(t, homePath,
		"binding-plan", "--item", "pver_binding_cli", "--area", "panel-verdicts",
		"--fallback-work", "work_panel_fallback", "--json",
	)
	if code != 0 {
		t.Fatalf("binding-plan exit=%d output=%s", code, output)
	}
	var plan reviewsBindingPlanResult
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&plan); err != nil {
		t.Fatalf("parse binding plan: %v\n%s", err, output)
	}
	if plan.Binding.ReviewSHA256 == "" || plan.Recorded {
		t.Fatalf("binding plan = %+v", plan)
	}
	if matches, _ := filepath.Glob(strings.TrimSuffix(paths.PlanPath, ".json") + ".work-binding.*.json"); len(matches) != 0 {
		t.Fatalf("read-only plan wrote bindings: %v", matches)
	}

	code, output = runReviewsBindingCLI(t, homePath,
		"binding-record", "--item", "pver_binding_cli", "--area", "panel-verdicts",
		"--fallback-work", "work_panel_fallback",
		"--expected-review-sha256", plan.Binding.ReviewSHA256,
		"--governed-work", "work_binding_governor", "--actor", "operator:test", "--json",
	)
	if code != 0 {
		t.Fatalf("binding-record exit=%d output=%s", code, output)
	}
	var recorded reviewsBindingPlanResult
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&recorded); err != nil {
		t.Fatalf("parse binding record: %v\n%s", err, output)
	}
	if !recorded.Recorded || recorded.ActionIntent == nil || recorded.PolicyDecision == nil ||
		recorded.PolicyAuthority == nil || !recorded.PolicyAuthority.Allowed {
		t.Fatalf("governed binding record = %+v", recorded)
	}
	after, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("binding record rewrote core review\nbefore=%s\nafter=%s", before, after)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: "panel-verdicts"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 || report.Items[0].WorkBinding == nil ||
		report.Items[0].WorkBinding.FallbackWorkID != "work_panel_fallback" {
		t.Fatalf("queue did not load recorded binding: %+v", report.Items)
	}
}

func TestReviewsBindingPlanAndRecordRepairDigestDrift(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	paths, err := reviewartifact.Write(homePath, "route-promotions", "route_promotion_cli", map[string]any{
		"id": "route_promotion_cli", "status": "review_required",
	}, " ", "# route promotion\n")
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []work.CreateInput{
		{
			WorkID: "work_binding_governor", Owner: "review-binding-test",
			SuccessPredicate: "binding record is governed",
		},
		{
			WorkID: "work_review_area_route_promotions", Owner: "route-review-owner",
			CanonicalSurface: "review-area:route-promotions",
			SuccessPredicate: "route promotion reviews remain actively owned",
		},
	} {
		if _, err := store.Create(input, cliTestNow); err != nil {
			t.Fatal(err)
		}
	}

	code, output := runReviewsBindingCLI(t, homePath,
		"binding-plan", "--item", "route_promotion_cli", "--area", "route-promotions",
		"--fallback-work", "work_review_area_route_promotions", "--json",
	)
	if code != 0 {
		t.Fatalf("initial binding-plan exit=%d output=%s", code, output)
	}
	var initialPlan reviewsBindingPlanResult
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&initialPlan); err != nil {
		t.Fatal(err)
	}
	code, output = runReviewsBindingCLI(t, homePath,
		"binding-record", "--item", "route_promotion_cli", "--area", "route-promotions",
		"--fallback-work", "work_review_area_route_promotions",
		"--expected-review-sha256", initialPlan.Binding.ReviewSHA256,
		"--governed-work", "work_binding_governor", "--actor", "operator:test", "--json",
	)
	if code != 0 {
		t.Fatalf("initial binding-record exit=%d output=%s", code, output)
	}
	var first reviewsBindingPlanResult
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&first); err != nil {
		t.Fatal(err)
	}

	currentReview := []byte(`{"id":"route_promotion_cli","status":"review_required","revision":2}` + "\n")
	if err := os.WriteFile(paths.PlanPath, currentReview, 0o644); err != nil {
		t.Fatal(err)
	}
	code, output = runReviewsBindingCLI(t, homePath,
		"binding-plan", "--item", "route_promotion_cli", "--area", "route-promotions",
		"--fallback-work", "work_review_area_route_promotions", "--json",
	)
	if code != 0 {
		t.Fatalf("drifted binding-plan exit=%d output=%s", code, output)
	}
	var driftedPlan reviewsBindingPlanResult
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&driftedPlan); err != nil {
		t.Fatal(err)
	}
	code, output = runReviewsBindingCLI(t, homePath,
		"binding-record", "--item", "route_promotion_cli", "--area", "route-promotions",
		"--fallback-work", "work_review_area_route_promotions",
		"--expected-review-sha256", driftedPlan.Binding.ReviewSHA256,
		"--governed-work", "work_binding_governor", "--actor", "operator:test", "--json",
	)
	if code == 0 || !strings.Contains(output, "supersedes") {
		t.Fatalf("drift repair without supersedes exit=%d output=%s", code, output)
	}

	code, output = runReviewsBindingCLI(t, homePath,
		"binding-plan", "--item", "route_promotion_cli", "--area", "route-promotions",
		"--fallback-work", "work_review_area_route_promotions",
		"--supersedes", first.Binding.ID, "--json",
	)
	if code != 0 {
		t.Fatalf("repair binding-plan exit=%d output=%s", code, output)
	}
	var repairPlan reviewsBindingPlanResult
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&repairPlan); err != nil {
		t.Fatal(err)
	}
	if repairPlan.Current == nil || repairPlan.Current.ID != first.Binding.ID ||
		repairPlan.Binding.ReviewSHA256 == first.Binding.ReviewSHA256 {
		t.Fatalf("repair binding-plan = %+v", repairPlan)
	}

	code, output = runReviewsBindingCLI(t, homePath,
		"binding-record", "--item", "route_promotion_cli", "--area", "route-promotions",
		"--fallback-work", "work_review_area_route_promotions",
		"--supersedes", first.Binding.ID,
		"--expected-review-sha256", repairPlan.Binding.ReviewSHA256,
		"--governed-work", "work_binding_governor", "--actor", "operator:test", "--json",
	)
	if code != 0 {
		t.Fatalf("repair binding-record exit=%d output=%s", code, output)
	}
	var repaired reviewsBindingPlanResult
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&repaired); err != nil {
		t.Fatal(err)
	}
	if !repaired.Recorded || repaired.Binding.SupersedesBindingID != first.Binding.ID {
		t.Fatalf("repaired binding = %+v", repaired)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: "route-promotions"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 || report.Items[0].WorkBinding == nil ||
		report.Items[0].WorkBinding.ID != repaired.Binding.ID {
		t.Fatalf("queue did not recover current binding: %+v", report.Items)
	}
	for _, warning := range report.Items[0].Warnings {
		if strings.Contains(warning, "owner-work binding invalid") {
			t.Fatalf("queue retained binding error after repair: %v", report.Items[0].Warnings)
		}
	}
}

func TestReviewBindingAuthorizationPersistsDistinctAttemptsAcrossTime(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	paths, err := reviewartifact.Write(homePath, "panel-verdicts", "pver_attempt_identity", map[string]any{
		"id":     "pver_attempt_identity",
		"status": "review_required",
	}, "  ", "# panel verdict\n")
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID:           "work_binding_governor",
		Owner:            "review-binding-test",
		SuccessPredicate: "binding record attempts remain auditable",
	}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: "panel-verdicts"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	item, err := exactReviewBindingItem(report.Items, "panel-verdicts", "pver_attempt_identity")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := reviewartifact.PlanWorkBinding(homePath, reviewartifact.RecordWorkBindingInput{
		Area:           "panel-verdicts",
		ReviewID:       "pver_attempt_identity",
		CoreReviewPath: paths.PlanPath,
		FallbackWorkID: "work_binding_governor",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	first, firstDecision, _, err := authorizeReviewBindingRecord(
		homePath, "work_binding_governor", "operator:test", item, binding, cliTestNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistReviewBindingAuthorization(homePath, first, firstDecision); err != nil {
		t.Fatal(err)
	}
	second, secondDecision, _, err := authorizeReviewBindingRecord(
		homePath, "work_binding_governor", "operator:test", item, binding, cliTestNow.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("distinct authorization attempts reused ActionIntent id %s", first.ID)
	}
	if err := persistReviewBindingAuthorization(homePath, second, secondDecision); err != nil {
		t.Fatalf("persist second authorization attempt: %v", err)
	}
}

func runReviewsBindingCLI(t *testing.T, homePath string, args ...string) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReviewsBindingSubprocess$")
	cmd.Env = append(os.Environ(),
		reviewsBindingSubprocessEnv+"=1",
		reviewsBindingArgsEnv+"="+string(encoded),
		"COMPOUND_HOME="+homePath,
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(output)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal(err)
	}
	return exitErr.ExitCode(), string(output)
}
