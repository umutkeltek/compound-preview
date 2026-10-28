package gitrepository

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRetirementPlanSchemaPreservesFrozenV01Identity(t *testing.T) {
	if WorktreeRetirementPlanSchemaVersion != "compound.worktree_retirement_plan.v0.2" {
		t.Fatal("review schema evolution before changing the current literal")
	}
	data, err := os.ReadFile("testdata/retirement-plan-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.TrimSpace(data)
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != "b15eca9e3047fca48709e190f26b96b8f1b44bc0ce6e546d44b20c692800d687" {
		t.Fatal("frozen v0.1 payload changed")
	}
	var plan WorktreeRetirementPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != "compound.worktree_retirement_plan.v0.1" || plan.ID != "wtr_e235336c0e51" || plan.PlanDigest != "sha256:87c3cca5d3aef301d16b1762d24efeb696bbdf56b696ac1a289b0e929e87a146" {
		t.Fatal("historical plan identity changed")
	}
	if err := ValidateWorktreeRetirementPlan(plan); err != nil {
		t.Fatalf("historical evidence falsely invalidated: %v", err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil || !bytes.Equal(encoded, data) {
		t.Fatalf("v0.1 bytes did not round-trip: %v", err)
	}
	if !RetirementRequiresPublication(plan.SourceProof) {
		t.Fatal("unbound historical source bypassed remote proof")
	}
	plan.PublicationRef, plan.PublicationTip = "", ""
	plan.ID, plan.PlanDigest = "", ""
	plan.ID = WorktreeRetirementPlanID(plan)
	plan.PlanDigest = WorktreeRetirementPlanDigest(plan)
	if err := ValidateWorktreeRetirementPlan(plan); err == nil || !strings.Contains(err.Error(), "publication") {
		t.Fatalf("historical source lost publication gate: %v", err)
	}
}

func TestRetirementHistoricalCompletedDeliveryRemainsReadable(t *testing.T) {
	planBody, err := os.ReadFile("testdata/retirement-plan-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var plan WorktreeRetirementPlan
	if err := json.Unmarshal(planBody, &plan); err != nil {
		t.Fatal(err)
	}
	review := WorktreeRetirementReview{
		SchemaVersion: WorktreeRetirementReviewSchemaVersion, ID: "wtrr_historical",
		PlanID: plan.ID, PlanDigest: plan.PlanDigest, Decision: "authorize_retirement",
		Rationale: "Preserve the reviewed historical completed effect.", ReviewedBy: "operator:test",
		ReviewedAt: plan.CreatedAt, EvidenceRefs: WorktreeRetirementEvidenceRefs(plan),
	}
	reviewBody, err := yaml.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	report := WorktreeRetirementReviewReport{
		SchemaVersion: WorktreeRetirementReviewReportSchemaVersion, ID: "wtrra_historical",
		Status: "authorized", PlanID: plan.ID, PlanDigest: plan.PlanDigest,
		PlanPath: "/synthetic/retirement/plan.json", ReviewPath: "/synthetic/retirement/authorization.json",
		ReviewID: review.ID, Decision: review.Decision, Rationale: review.Rationale,
		ReviewedBy: review.ReviewedBy, ReviewedAt: review.ReviewedAt,
		SourceDigest: "sha256:" + DigestRetirementHex(reviewBody),
	}
	report.AuthorizationDigest = WorktreeRetirementAuthorizationDigest(report)
	result := WorktreeRetirementResult{
		SchemaVersion: WorktreeRetirementResultSchemaVersion, ID: "wtrr_historical_effect",
		Status: "completed", Phase: "verified", PlanID: plan.ID, PlanDigest: plan.PlanDigest,
		AuthorizationDigest: report.AuthorizationDigest, WorktreeRemoved: plan.Worktree != nil,
		RefDeleted: plan.CandidateRef != "", AuthorityTip: plan.AuthorityTip, PublicationTip: plan.PublicationTip,
		ActionIntentID: "act_historical", PolicyDecisionID: "pol_historical", ReceiptID: "rec_historical",
		TransitionID: "tr_historical", LeaseWorkSHA256: plan.OwnerProof.LeaseWorkSHA256,
		WorkDigest: plan.OwnerProof.WorkDigest, ResultPath: "/synthetic/retirement/result.json",
	}
	result.ResultDigest = WorktreeRetirementResultDigest(result)
	reportBody, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	resultBody, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := ValidateRetirementDelivery(planBody, reportBody, reviewBody, resultBody)
	if err != nil {
		t.Fatalf("historical completed capture became unreadable: %v", err)
	}
	if delivery.Plan.PlanDigest != plan.PlanDigest || delivery.Plan.ID != plan.ID || delivery.Result.ResultDigest != result.ResultDigest {
		t.Fatal("historical delivery identities changed during readback")
	}
	result.PublicationTip = ""
	result.ResultDigest = WorktreeRetirementResultDigest(result)
	resultBody, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRetirementDelivery(planBody, reportBody, reviewBody, resultBody); err == nil {
		t.Fatal("historical remote evidence fence was removed")
	}
}

func TestRetirementSchemaClassificationPrecedesDigest(t *testing.T) {
	for _, tc := range []struct {
		name, schema, want string
		delivery           bool
	}{
		{"unknown", "compound.worktree_retirement_plan.v99", "schema_version", false},
		{"downgrade", "compound.worktree_retirement_plan.v0.1", "historical", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := WorktreeRetirementPlan{SchemaVersion: tc.schema, PlanDigest: "not-a-digest"}
			if tc.delivery {
				p.SourceProof.Delivery = &WorktreeRetirementSourceDelivery{PublicationMode: "local_only"}
			}
			if err := ValidateWorktreeRetirementPlan(p); err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "digest") {
				t.Fatalf("schema misclassified as current tamper: %v", err)
			}
		})
	}
}

func TestRetirementV02WireFieldInventory(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{WorktreeRetirementPlan{}, "schema_version,id,created_at,status,repo_root,repository_fingerprint,worktree,candidate_ref,candidate_tip,authority_ref,authority_tip,publication_ref,publication_tip,candidate_relation,source_relation,owner_proof,source_proof,recovery_refs,recovery_proofs,evidence_refs,eligible,block_reasons,plan_digest"},
		{WorktreeRetirementSourceProof{}, "work_id,state,proof_complete,action_intent_ids,policy_decision_id,receipt_id,receipt_outcome,transition_id,commit_artifact_ref,commit_tip,land_check_id,land_check_evidence_ref,land_check_status,land_check_candidate_tip,land_check_file_digest,delivery"},
		{WorktreeRetirementSourceDelivery{}, "work_digest,target_worktree_root,repository_fingerprint,target_local_ref,expected_base_sha,publication_mode,publication_ref"},
	} {
		typ := reflect.TypeOf(tc.value)
		names := []string{}
		for i := 0; i < typ.NumField(); i++ {
			names = append(names, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
		}
		if strings.Join(names, ",") != tc.want {
			t.Fatalf("%s changed sealed v0.2 content without a versioned codec", typ.Name())
		}
	}
}
