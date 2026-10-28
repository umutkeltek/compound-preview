package product

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/work"
)

func TestReleaseAuthorizationBuildsExactOperatorHeldWorkDraft(t *testing.T) {
	homePath, _, plan, report := seedAuthorizedRelease(t)

	draft, err := BuildReleaseWorkDraft(homePath, report.ReviewedPath)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Status != "work_plan_ready" || draft.WorkID == "" || draft.WorkOwner != plan.ProductID {
		t.Fatalf("release work identity/status = %+v", draft)
	}
	if draft.PlanID != plan.ID || draft.AuthorizationID != report.ID || !reflect.DeepEqual(draft.ReleaseIdentity, plan.ReleaseIdentity) {
		t.Fatalf("release work lost exact authorization identity: %+v", draft)
	}
	if draft.ChangeSummary != plan.ChangeSummary || draft.Rollback != plan.Rollback || draft.PostReleaseMetric != plan.PostReleaseMetric {
		t.Fatalf("release work lost bounded release semantics: %+v", draft)
	}
	for _, ref := range []string{"product_release_plan:" + plan.ID, "product_release_authorization:" + report.ID, "product_release_review_transition:" + report.TransitionID} {
		if !containsString(draft.EvidenceRefs, ref) {
			t.Fatalf("release work evidence missing %q: %v", ref, draft.EvidenceRefs)
		}
	}
	if !draft.RequiresOperator || !strings.Contains(draft.Boundary, "external store") || len(draft.RequiredChecks) == 0 {
		t.Fatalf("release work must preserve the operator/external boundary: %+v", draft)
	}
}

func TestReleaseObservationRequiresGovernedReceiptAndReturnsPlannedSignal(t *testing.T) {
	homePath, store, plan, authorization := seedAuthorizedRelease(t)
	draft, err := BuildReleaseWorkDraft(homePath, authorization.ReviewedPath)
	if err != nil {
		t.Fatal(err)
	}
	receipt := seedGovernedReleaseReceipt(t, homePath, draft, authorization, releaseFlowTestNow.Add(3*time.Minute))
	signal := Signal{
		ID:          "sig_release_continuation_units",
		ProductID:   plan.ProductID,
		Surface:     "app_store",
		ChangeEvent: plan.ChangeSummary,
		MetricName:  plan.PostReleaseMetric,
		Before:      4,
		After:       7,
		Window:      "7d before vs 7d after release",
		Confidence:  0.9,
		EvidenceRefs: []string{
			"app_store_connect:synthetic-release-signal",
			"receipt:" + receipt.ID,
		},
		ObservedAt: releaseFlowTestNow.Add(10 * time.Minute).Format(time.RFC3339),
	}
	if _, err := ApplySignal(homePath, store, SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "app_store_connect", Ref: "fixture:release-continuation"},
		Signal:        signal,
	}, "fixture/release-continuation.yaml", releaseFlowTestNow.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	observed, err := ObserveProductRelease(homePath, ReleaseObservationInput{
		ReviewedPath: authorization.ReviewedPath,
		ExecutionRef: receipt.ID,
		SignalID:     signal.ID,
	}, releaseFlowTestNow.Add(11*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status != "loop_closed" || observed.AuthorizationID != authorization.ID || observed.ExecutionReceiptID != receipt.ID || observed.SignalID != signal.ID {
		t.Fatalf("release observation did not close exact loop: %+v", observed)
	}
	if observed.TransitionID == "" || observed.MetricName != plan.PostReleaseMetric {
		t.Fatalf("release observation lacks transition/metric proof: %+v", observed)
	}
}

func TestReleaseObservationRejectsWrongMetricAndUngovernedReceipt(t *testing.T) {
	homePath, store, plan, authorization := seedAuthorizedRelease(t)
	draft, err := BuildReleaseWorkDraft(homePath, authorization.ReviewedPath)
	if err != nil {
		t.Fatal(err)
	}
	signal := Signal{
		ID:           "sig_release_wrong_metric",
		ProductID:    plan.ProductID,
		Surface:      "app_store",
		ChangeEvent:  plan.ChangeSummary,
		MetricName:   "unplanned_metric",
		Before:       1,
		After:        2,
		Window:       "7d",
		Confidence:   0.8,
		EvidenceRefs: []string{"fixture:wrong-metric"},
		ObservedAt:   releaseFlowTestNow.Add(10 * time.Minute).Format(time.RFC3339),
	}
	if _, err := ApplySignal(homePath, store, SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "app_store_connect", Ref: "fixture:wrong-metric"},
		Signal:        signal,
	}, "fixture/wrong-metric.yaml", releaseFlowTestNow.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, err := ObserveProductRelease(homePath, ReleaseObservationInput{
		ReviewedPath: authorization.ReviewedPath,
		ExecutionRef: "rec_not_governed",
		SignalID:     signal.ID,
	}, releaseFlowTestNow.Add(11*time.Minute)); err == nil || !strings.Contains(err.Error(), "governed") {
		t.Fatalf("ungoverned receipt should fail closed, got %v", err)
	}

	receipt := seedGovernedReleaseReceipt(t, homePath, draft, authorization, releaseFlowTestNow.Add(3*time.Minute))
	if _, err := ObserveProductRelease(homePath, ReleaseObservationInput{
		ReviewedPath: authorization.ReviewedPath,
		ExecutionRef: receipt.ID,
		SignalID:     signal.ID,
	}, releaseFlowTestNow.Add(11*time.Minute)); err == nil || !strings.Contains(err.Error(), "planned post-release metric") {
		t.Fatalf("wrong metric should fail closed, got %v", err)
	}
}

func TestReleaseObservationRejectsReceiptForDifferentReleaseTarget(t *testing.T) {
	homePath, _, _, authorization := seedAuthorizedRelease(t)
	draft, err := BuildReleaseWorkDraft(homePath, authorization.ReviewedPath)
	if err != nil {
		t.Fatal(err)
	}
	receipt := seedGovernedReleaseReceiptForTarget(t, homePath, draft, authorization, releaseFlowTestNow.Add(3*time.Minute), "prlp_different_release")

	if _, err := ObserveProductRelease(homePath, ReleaseObservationInput{
		ReviewedPath: authorization.ReviewedPath,
		ExecutionRef: receipt.ID,
		SignalID:     "sig_not_reached",
	}, releaseFlowTestNow.Add(11*time.Minute)); err == nil || !strings.Contains(err.Error(), "exact external product release") {
		t.Fatalf("receipt for a different release target should fail closed, got %v", err)
	}
}

func seedAuthorizedRelease(t *testing.T) (string, *mem.Store, ProductReleasePlan, ProductReleaseReviewReport) {
	t.Helper()
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:release-continuation",
			Name:           "Release Continuation",
			OwnerEntity:    "owner:demo",
			Surfaces:       []Surface{{ID: "source", Kind: "repo", Ref: "repo:release-continuation"}},
			Channels:       []string{"app_store"},
			Positioning:    "Synthetic release continuation fixture.",
			PrivacyPosture: "public_safe",
			ReleaseIdentity: ReleaseIdentity{
				BundleID:             "com.example.release-continuation",
				AppStoreConnectAppID: "app-release-continuation",
				Version:              "4.0.0",
				Build:                "88",
			},
		},
		EvidenceRefs: []string{"fixture:release-continuation"},
	}
	if _, err := Apply(homePath, store, bundle, "release-continuation.yaml", releaseFlowTestNow); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildReleasePlan(homePath, store, ReleasePlanInput{
		ProductID:         bundle.Product.ID,
		ChangeSummary:     "Ship the verified release continuation.",
		CandidateRefs:     []string{"commit:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
		Rollback:          "Restore the previous App Store version.",
		PostReleaseMetric: "app_store_connect_units",
		EvidenceRefs:      []string{"check:release-continuation-suite"},
	}, releaseFlowTestNow)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordReleasePlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyReleaseReview(homePath, ProductReleaseReview{
		SchemaVersion:   ProductReleaseReviewSchemaVersion,
		ID:              plan.ReviewDraft.ReviewID,
		ProductID:       plan.ProductID,
		PlanID:          plan.ID,
		Decision:        "authorize_release",
		Rationale:       "Authorize only the exact verified candidate.",
		Confidence:      0.95,
		ReviewedBy:      "operator:test",
		CompletedChecks: append([]string(nil), plan.RequiredChecks...),
		EvidenceRefs:    []string{"operator_review:release-continuation"},
	}, paths.ReviewYAMLPath, releaseFlowTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return homePath, store, plan, report
}

func seedGovernedReleaseReceipt(t *testing.T, homePath string, draft ProductReleaseWorkDraft, authorization ProductReleaseReviewReport, at time.Time) envelope.Receipt {
	t.Helper()
	return seedGovernedReleaseReceiptForTarget(t, homePath, draft, authorization, at, draft.PlanID)
}

func seedGovernedReleaseReceiptForTarget(t *testing.T, homePath string, draft ProductReleaseWorkDraft, authorization ProductReleaseReviewReport, at time.Time, targetID string) envelope.Receipt {
	t.Helper()
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Create(work.CreateInput{
		WorkID:           draft.WorkID,
		Owner:            draft.WorkOwner,
		Actor:            "operator:test",
		SuccessPredicate: draft.SuccessPredicate,
		DesiredOutcome:   draft.DesiredOutcome,
		NonGoals:         draft.NonGoals,
		EvidenceRefs:     draft.EvidenceRefs,
		State:            "ready",
		Risk:             "tier3",
		Privacy:          "private",
	}, at.Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Claim(w.ID, "operator:test", at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	w, err = workStore.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "operator:test"}, at.Add(-30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	action := envelope.ActionIntent{
		Header:        releaseTestHeader(envelope.SVActionIntent, "act_release_continuation", w.ID, at),
		Actor:         "operator:test",
		Capability:    "product.release.execute",
		Action:        "submit_exact_app_store_release",
		Target:        envelope.ActionTarget{Kind: "app_store_release", ID: targetID},
		MutationClass: "public",
		Reversibility: "manual",
		BlastRadius:   "public",
		RollbackPlan:  draft.Rollback,
	}
	decision := envelope.PolicyDecision{
		Header:         releaseTestHeader(envelope.SVPolicyDecision, "pol_release_continuation", w.ID, at),
		ActionIntentID: action.ID,
		Outcome:        "allow_with_obligations",
		Reasons:        []string{"exact product release authorization recorded"},
		Obligations:    []string{"emit governed receipt", "observe planned post-release metric"},
		// public is a dangerous mutation class (E5): the pre-evaluated decision
		// must carry the standing-grant authority the operator review minted,
		// not merely an "allow" outcome the caller asserts.
		StandingGrantID: "grant:product-release:" + authorization.ID,
	}
	result, err := workStore.RecordReceiptedAction(w.ID, work.ReceiptedActionInput{
		Action:               action,
		PreEvaluatedDecision: &decision,
		ExpectedWorkState:    "running",
		Outcome:              "completed",
		Changed:              true,
		Summary:              "submitted exact authorized App Store release",
		MoveType:             "outward_delta",
		ProofRefs: []string{
			"product_release_authorization:" + authorization.ID,
			"app_store_connect:submission:synthetic",
		},
	}, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return result.Receipt
}

func releaseTestHeader(schemaVersion, id, workID string, at time.Time) envelope.Header {
	return envelope.Header{
		SchemaVersion: schemaVersion,
		ID:            id,
		CreatedAt:     at.UTC().Format(time.RFC3339),
		Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
		IDs:           envelope.IDs{WorkID: workID},
		Scope:         envelope.Scope{ProductID: "product:release-continuation"},
		Authority:     envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
		Privacy:       "private",
		Risk:          "tier3",
		EvidenceRefs:  []string{"fixture:release-continuation"},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}
