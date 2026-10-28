package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"compound/internal/mem"
	"compound/internal/product"
	"compound/internal/work"
)

func TestProductReleaseWorkPlanCLIProducesVisibleOperatorHeldWork(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle := product.Bundle{
		SchemaVersion: product.SchemaVersion,
		Product: product.Product{
			ID:             "product:cli-release-work",
			Name:           "CLI Release Work",
			OwnerEntity:    "owner:demo",
			Surfaces:       []product.Surface{{ID: "source", Kind: "repo", Ref: "repo:cli-release-work"}},
			Channels:       []string{"app_store"},
			Positioning:    "Synthetic release work CLI fixture.",
			PrivacyPosture: "public_safe",
			ReleaseIdentity: product.ReleaseIdentity{
				BundleID:             "com.example.cli-release-work",
				AppStoreConnectAppID: "app-cli-release-work",
				Version:              "5.0.0",
				Build:                "99",
			},
		},
		EvidenceRefs: []string{"fixture:cli-release-work"},
	}
	if _, err := product.Apply(homePath, store, bundle, "cli-release-work.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}
	plan, err := product.BuildReleasePlan(homePath, store, product.ReleasePlanInput{
		ProductID:         bundle.Product.ID,
		ChangeSummary:     "Ship the exact CLI release work fixture.",
		CandidateRefs:     []string{"commit:ffffffffffffffffffffffffffffffffffffffff"},
		Rollback:          "Restore the previous App Store version.",
		PostReleaseMetric: "app_store_connect_units",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := product.RecordReleasePlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := product.ApplyReleaseReview(homePath, product.ProductReleaseReview{
		SchemaVersion:   product.ProductReleaseReviewSchemaVersion,
		ID:              plan.ReviewDraft.ReviewID,
		ProductID:       plan.ProductID,
		PlanID:          plan.ID,
		Decision:        "authorize_release",
		Rationale:       "Authorize the exact tested fixture only.",
		Confidence:      0.95,
		ReviewedBy:      "operator:test",
		CompletedChecks: append([]string(nil), plan.RequiredChecks...),
		EvidenceRefs:    []string{"operator_review:cli-release-work"},
	}, paths.ReviewYAMLPath, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdProductReleaseWorkPlan([]string{"--reviewed", reviewed.ReviewedPath, "--actor", "operator:test", "--json"}, homePath)
	})
	var report struct {
		Status         string                          `json:"status"`
		ProductRelease product.ProductReleaseWorkDraft `json:"product_release"`
		WorkPlan       work.CreatePlan                 `json:"work_plan"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse release-work-plan: %v\n%s", err, out)
	}
	if report.Status != "recorded" || report.ProductRelease.AuthorizationID != reviewed.ID || report.WorkPlan.WorkID != report.ProductRelease.WorkID {
		t.Fatalf("release-work-plan lost authorization/work identity: %+v", report)
	}
	if report.WorkPlan.Work.Risk != "tier3" || report.WorkPlan.ActionContext == nil || !report.WorkPlan.ActionContext.RequiresOperator {
		t.Fatalf("release work must remain tier3/operator-held: %+v", report.WorkPlan)
	}
	if !strings.Contains(report.WorkPlan.ActionContext.ActionSummary, plan.ChangeSummary) || report.WorkPlan.ActionContext.Proof == "" {
		t.Fatalf("release action context is not useful to the executor: %+v", report.WorkPlan.ActionContext)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Get(report.WorkPlan.WorkID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release-work-plan must not admit work, got %v", err)
	}
}
