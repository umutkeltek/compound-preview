package product

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

var releaseFlowTestNow = time.Date(2026, 7, 22, 12, 30, 0, 0, time.UTC)

func TestReleasePlanResolvesCompoundOwnedIdentityAndCannotMutateStore(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:release-demo",
			Name:           "Release Demo",
			OwnerEntity:    "owner:demo",
			Surfaces:       []Surface{{ID: "source", Kind: "repo", Ref: "repo:release-demo"}},
			Channels:       []string{"app_store"},
			Positioning:    "Synthetic release-flow fixture.",
			PrivacyPosture: "public_safe",
			ReleaseIdentity: ReleaseIdentity{
				BundleID:             "com.example.release-demo",
				AppStoreConnectAppID: "app-release-demo",
				Version:              "2.4.0",
				Build:                "42",
			},
		},
		EvidenceRefs: []string{"fixture:release-demo"},
	}
	if _, err := Apply(homePath, store, bundle, "release-demo.yaml", releaseFlowTestNow); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildReleasePlan(homePath, store, ReleasePlanInput{
		ProductID:         bundle.Product.ID,
		ActionCardID:      "product-loop-review-next-release-demo",
		ChangeSummary:     "Ship the verified synchronization fix.",
		CandidateRefs:     []string{"commit:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Rollback:          "Restore the previous App Store version.",
		PostReleaseMetric: "app_store_connect_units",
		EvidenceRefs:      []string{"check:release-demo-suite", "product_review:review-release-demo"},
	}, releaseFlowTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "review_required" || plan.ProductID != bundle.Product.ID {
		t.Fatalf("release plan identity/status = %+v", plan)
	}
	if !reflect.DeepEqual(plan.ReleaseIdentity, bundle.Product.ReleaseIdentity) || plan.Mechanism != "app_store_connect" {
		t.Fatalf("release plan did not resolve Compound-owned identity: %+v", plan)
	}
	if plan.ExternalMutationAllowed || !strings.Contains(plan.Boundary, "does not execute") {
		t.Fatalf("release plan must remain non-executing: %+v", plan)
	}
	if len(plan.RequiredChecks) == 0 || plan.ReviewDraft.ReviewCommand != "compound product release-review <review.yaml>" {
		t.Fatalf("release plan is not authorization-ready: %+v", plan)
	}
	if !hasString(plan.EvidenceRefs, bundle.Product.ID) || hasString(plan.EvidenceRefs, "product:"+bundle.Product.ID) {
		t.Fatalf("release plan must preserve the canonical product id exactly: %+v", plan.EvidenceRefs)
	}

	paths, err := RecordReleasePlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("release artifact missing %s: %v", path, err)
		}
	}
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: ProductReleaseReviewArea}, releaseFlowTestNow)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.ActionableCount != 1 || queue.Counts.DecisionCardCount != 1 {
		t.Fatalf("release plan should be visible as one decision: counts=%+v items=%+v", queue.Counts, queue.Items)
	}
}

func TestReleaseReviewRecordsAuthorizationButNeverExecution(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:release-review-demo",
			Name:           "Release Review Demo",
			OwnerEntity:    "owner:demo",
			Surfaces:       []Surface{{ID: "source", Kind: "repo", Ref: "repo:release-review-demo"}},
			Channels:       []string{"app_store"},
			Positioning:    "Synthetic reviewed release fixture.",
			PrivacyPosture: "public_safe",
			ReleaseIdentity: ReleaseIdentity{
				BundleID:             "com.example.release-review-demo",
				AppStoreConnectAppID: "app-release-review-demo",
				Version:              "1.8.0",
				Build:                "19",
			},
		},
		EvidenceRefs: []string{"fixture:release-review-demo"},
	}
	if _, err := Apply(homePath, store, bundle, "release-review-demo.yaml", releaseFlowTestNow); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildReleasePlan(homePath, store, ReleasePlanInput{
		ProductID:         bundle.Product.ID,
		ChangeSummary:     "Ship the verified export fix.",
		CandidateRefs:     []string{"commit:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		Rollback:          "Restore the previous App Store version.",
		PostReleaseMetric: "app_store_connect_review_count",
		EvidenceRefs:      []string{"check:release-review-demo-suite"},
	}, releaseFlowTestNow)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordReleasePlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	review := ProductReleaseReview{
		SchemaVersion:   ProductReleaseReviewSchemaVersion,
		ID:              plan.ReviewDraft.ReviewID,
		ProductID:       plan.ProductID,
		PlanID:          plan.ID,
		Decision:        "authorize_release",
		Rationale:       "The candidate and rollback are verified; authorize only the named release.",
		Confidence:      0.9,
		ReviewedBy:      "operator:test",
		CompletedChecks: append([]string(nil), plan.RequiredChecks...),
		EvidenceRefs:    []string{"check:release-review-demo-suite", "operator_review:test"},
	}
	report, err := ApplyReleaseReview(homePath, review, paths.ReviewYAMLPath, releaseFlowTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "authorization_recorded" || !report.Authorized || report.Executed {
		t.Fatalf("review must authorize without executing: %+v", report)
	}
	if report.TransitionID == "" || !strings.Contains(report.NextAction, "governed") {
		t.Fatalf("release review must leave transition and governed next move: %+v", report)
	}

	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: ProductReleaseReviewArea}, releaseFlowTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 || queue.Items[0].Readiness != "no_action" || queue.Items[0].Status != "authorization_recorded" {
		t.Fatalf("recorded release authorization should preserve its terminal status: counts=%+v items=%+v", queue.Counts, queue.Items)
	}
}

func TestReleasePlanRefusesMissingCompoundIdentity(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildReleasePlan(homePath, store, ReleasePlanInput{
		ProductID:         "product:not-imported",
		ChangeSummary:     "Ship nothing.",
		CandidateRefs:     []string{"commit:cccccccccccccccccccccccccccccccccccccccc"},
		Rollback:          "No-op.",
		PostReleaseMetric: "app_store_connect_units",
	}, releaseFlowTestNow)
	if err == nil || !strings.Contains(err.Error(), "Compound-owned release identity") {
		t.Fatalf("missing release identity should fail closed, got %v", err)
	}
}
