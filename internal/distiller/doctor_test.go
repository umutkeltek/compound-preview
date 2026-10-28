package distiller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

func TestDoctorHistoricalDuplicatesDoNotRequireAttention(t *testing.T) {
	home, store := setup(t)
	raw := readTraceFixture(t, "distill-duplicate-memory.json")
	for i := 0; i < 2; i++ {
		if _, err := Run(home, store, raw, DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Doctor(home)
	if err != nil {
		t.Fatal(err)
	}
	if report.DuplicateMemoryRejections != 1 || report.NoPromoteRows != 1 {
		t.Fatalf("historical totals lost: %+v", report)
	}
	if report.Status != "ok" {
		t.Fatalf("historical duplicate set status %q, want ok", report.Status)
	}
	for _, check := range report.Checks {
		if check.Name == "duplicate_churn" {
			if check.Status != "observed" || !strings.Contains(check.Detail, "recent rate unknown") {
				t.Fatalf("duplicate check = %+v, want observed history with unknown recent rate", check)
			}
			return
		}
	}
	t.Fatal("duplicate_churn check missing")
}

func TestDoctorCountsOnlyUnresolvedImprovementReviewsAsDebt(t *testing.T) {
	for _, tc := range []struct {
		name, decision, status string
		wantDebt               int
	}{
		{name: "no review", wantDebt: 1},
		{name: "accepted review", decision: ImprovementReviewDecisionAccept, status: "accepted", wantDebt: 0},
		{name: "rejected review", decision: ImprovementReviewDecisionReject, status: "rejected", wantDebt: 0},
		{name: "keep reviewing", decision: ImprovementReviewDecisionKeepReviewing, status: "needs_review", wantDebt: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, store := setup(t)
			result, err := Run(home, store, readTraceFixture(t, "distill-session.json"), DefaultPolicy(), ReferenceExtractor{}, t0)
			if err != nil {
				t.Fatal(err)
			}
			var candidateID string
			for _, decision := range result.Decisions {
				if decision.Kind == KindMethodUpgrade {
					candidateID = decision.CandidateID
				}
			}
			if candidateID == "" {
				t.Fatal("fixture did not produce method upgrade")
			}
			if tc.decision != "" {
				plan, err := BuildImprovementPlan(home, candidateID, filepath.Join(t.TempDir(), "target.go"), t0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := RecordImprovementPlan(home, plan); err != nil {
					t.Fatal(err)
				}
				reviewed := ReviewedImprovement{
					SchemaVersion: ReviewedImprovementSchemaVersion,
					ID:            "reviewed_fixture", ReviewID: "fixture", PlanID: plan.ID,
					CreatedAt:   rfc3339(t0),
					CandidateID: candidateID, CandidateKind: KindMethodUpgrade,
					ImprovementID: plan.Draft.ImprovementID,
					Decision:      tc.decision, Status: tc.status,
				}
				if _, err := reviewartifact.WriteJSON(home, ImprovementReviewArea, "fixture.reviewed.json", reviewed, "  "); err != nil {
					t.Fatal(err)
				}
			}
			report, err := Doctor(home)
			if err != nil {
				t.Fatal(err)
			}
			if report.CandidateInboxCounts[KindMethodUpgrade] != 1 || report.MethodUpgradeDebt != tc.wantDebt || report.SelfImprovementDebt != tc.wantDebt {
				t.Fatalf("raw inbox/debt = %d/%d/%d, want 1/%d/%d", report.CandidateInboxCounts[KindMethodUpgrade], report.MethodUpgradeDebt, report.SelfImprovementDebt, tc.wantDebt, tc.wantDebt)
			}
			wantStatus := "ok"
			if tc.wantDebt > 0 {
				wantStatus = "attention"
			}
			if report.Status != wantStatus {
				t.Fatalf("status = %q, want %q", report.Status, wantStatus)
			}
		})
	}
}

func TestDoctorRefusesMalformedImprovementReview(t *testing.T) {
	home, _ := setup(t)
	dir := filepath.Join(home, "reviews", ImprovementReviewArea)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.reviewed.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Doctor(home); err == nil || !strings.Contains(err.Error(), "parse improvement review") {
		t.Fatalf("malformed terminal evidence must surface, got %v", err)
	}
}

func TestDoctorLaterKeepReviewingReopensImprovementDebt(t *testing.T) {
	home, store := setup(t)
	result, err := Run(home, store, readTraceFixture(t, "distill-session.json"), DefaultPolicy(), ReferenceExtractor{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	var candidateID string
	for _, decision := range result.Decisions {
		if decision.Kind == KindMethodUpgrade {
			candidateID = decision.CandidateID
		}
	}
	if candidateID == "" {
		t.Fatal("fixture did not produce method upgrade")
	}
	plan, err := BuildImprovementPlan(home, candidateID, filepath.Join(t.TempDir(), "target.go"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordImprovementPlan(home, plan); err != nil {
		t.Fatal(err)
	}
	for _, review := range []struct {
		id, decision, status string
		at                   time.Time
	}{
		{id: "z_accepted", decision: ImprovementReviewDecisionAccept, status: "accepted", at: t0},
		{id: "a_reopened", decision: ImprovementReviewDecisionKeepReviewing, status: "needs_review", at: t0.Add(time.Minute)},
	} {
		reviewed := ReviewedImprovement{
			SchemaVersion: ReviewedImprovementSchemaVersion,
			ID:            "reviewed_" + review.id, ReviewID: review.id, PlanID: plan.ID,
			CreatedAt: rfc3339(review.at), CandidateID: candidateID,
			CandidateKind: KindMethodUpgrade, ImprovementID: plan.Draft.ImprovementID,
			Decision: review.decision, Status: review.status,
		}
		if _, err := reviewartifact.WriteJSON(home, ImprovementReviewArea, review.id+".reviewed.json", reviewed, "  "); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Doctor(home)
	if err != nil {
		t.Fatal(err)
	}
	if report.MethodUpgradeDebt != 1 || report.Status != "attention" {
		t.Fatalf("latest keep_reviewing must reopen debt, got debt=%d status=%q", report.MethodUpgradeDebt, report.Status)
	}
}

func TestDoctorReportsSelfImprovementState(t *testing.T) {
	home, store := setup(t)
	raw := readTraceFixture(t, "distill-session.json")

	if _, err := Run(home, store, raw, DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
		t.Fatalf("run trace fixture: %v", err)
	}

	report, err := Doctor(home)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "attention" {
		t.Fatalf("status = %q, want attention", report.Status)
	}
	if report.TotalDecisions != 6 {
		t.Fatalf("total decisions = %d, want 6", report.TotalDecisions)
	}
	if report.MemoryPromoted != 0 || report.MemoryParked != 2 || report.MemoryRejected != 0 {
		t.Fatalf("memory counts promoted/parked/rejected = %d/%d/%d, want 0/2/0",
			report.MemoryPromoted, report.MemoryParked, report.MemoryRejected)
	}
	if report.EvalCases != 1 {
		t.Fatalf("eval cases = %d, want 1", report.EvalCases)
	}
	if report.MethodUpgradeDebt != 1 || report.PolicyPatchDebt != 0 || report.SelfImprovementDebt != 1 {
		t.Fatalf("self-improvement debt method/policy/total = %d/%d/%d, want 1/0/1",
			report.MethodUpgradeDebt, report.PolicyPatchDebt, report.SelfImprovementDebt)
	}
	if report.CapabilityObservations != 1 || report.OperationalizedCapabilityObservations != 1 {
		t.Fatalf("capability observations candidate/operationalized = %d/%d, want 1/1",
			report.CapabilityObservations, report.OperationalizedCapabilityObservations)
	}
	if report.CandidateInboxCounts[KindEvalCase] != 1 {
		t.Fatalf("eval-case inbox count = %d, want 1", report.CandidateInboxCounts[KindEvalCase])
	}
	if report.CandidateInboxCounts[KindMethodUpgrade] != 1 {
		t.Fatalf("method-upgrade inbox count = %d, want 1", report.CandidateInboxCounts[KindMethodUpgrade])
	}
}

func TestDoctorReportsDuplicateNoPromoteChurn(t *testing.T) {
	home, store := setup(t)
	raw := readTraceFixture(t, "distill-duplicate-memory.json")

	if _, err := Run(home, store, raw, DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := Run(home, store, raw, DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
		t.Fatalf("duplicate run: %v", err)
	}

	report, err := Doctor(home)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q, want ok", report.Status)
	}
	if report.MemoryPromoted != 1 || report.MemoryRejected != 1 {
		t.Fatalf("memory promoted/rejected = %d/%d, want 1/1", report.MemoryPromoted, report.MemoryRejected)
	}
	if report.DuplicateMemoryRejections != 1 {
		t.Fatalf("duplicate memory rejections = %d, want 1", report.DuplicateMemoryRejections)
	}
	if report.NoPromoteRows != 1 || report.DuplicateMemoryNoPromoteRows != 1 {
		t.Fatalf("no-promote rows duplicate/total = %d/%d, want 1/1",
			report.DuplicateMemoryNoPromoteRows, report.NoPromoteRows)
	}
}

func TestDoctorWritebackGateForMethodPolicyDebt(t *testing.T) {
	home, store := setup(t)
	raw := readTraceFixture(t, "distill-session.json")

	if _, err := Run(home, store, raw, DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
		t.Fatalf("run trace fixture: %v", err)
	}
	report, err := Doctor(home)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !NeedsWriteback(report) {
		t.Fatal("NeedsWriteback = false, want true")
	}

	gate := WritebackGate(report, "act_distiller", "pol_distiller", t0)
	if gate.CandidateKind != KindMethodUpgrade {
		t.Fatalf("candidate_kind = %q, want %q", gate.CandidateKind, KindMethodUpgrade)
	}
	if gate.OwnerTarget.Kind != "distillation_policy" || gate.OwnerTarget.Ref != "distillation_policy:default.v1" {
		t.Fatalf("owner target = %+v", gate.OwnerTarget)
	}
	if gate.ActionIntentRef != "act_distiller" || gate.PolicyDecisionRef != "pol_distiller" {
		t.Fatalf("gate action/policy refs = %q/%q", gate.ActionIntentRef, gate.PolicyDecisionRef)
	}
	if !hasString(gate.EvidenceRefs, "distiller:method_upgrades:1") || !hasString(gate.EvidenceRefs, "candidate_kind:"+KindMethodUpgrade) {
		t.Fatalf("evidence refs missing method upgrade markers: %v", gate.EvidenceRefs)
	}
	if gate.CandidateRef == "" || gate.GateBasis == "" {
		t.Fatalf("gate missing candidate ref or basis: %+v", gate)
	}
}

func TestDuplicateChurnDoesNotNeedWriteback(t *testing.T) {
	home, store := setup(t)
	raw := readTraceFixture(t, "distill-duplicate-memory.json")

	if _, err := Run(home, store, raw, DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := Run(home, store, raw, DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
		t.Fatalf("duplicate run: %v", err)
	}
	report, err := Doctor(home)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if NeedsWriteback(report) {
		t.Fatal("duplicate churn should be handled by no-promote, not policy writeback")
	}
}

func TestDoctorReportsDuplicateCandidateChurn(t *testing.T) {
	home, store := setup(t)
	first := `{"trace_ref":"t1","candidates":[{"kind":"skill","subject":"paywall-cro","statement":"use this lens","confidence":0.9}]}`
	dupe := `{"trace_ref":"t2","candidates":[{"kind":"skill","subject":"paywall-cro","statement":"use this lens","confidence":0.9}]}`
	if _, err := Run(home, store, []byte(first), DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := Run(home, store, []byte(dupe), DefaultPolicy(), ReferenceExtractor{}, t0); err != nil {
		t.Fatalf("duplicate run: %v", err)
	}

	report, err := Doctor(home)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.DuplicateCandidateRejections != 1 {
		t.Fatalf("duplicate candidate rejections = %d, want 1", report.DuplicateCandidateRejections)
	}
	if report.DuplicateCandidateNoPromoteRows != 1 {
		t.Fatalf("duplicate candidate no-promote rows = %d, want 1", report.DuplicateCandidateNoPromoteRows)
	}
	if NeedsWriteback(report) {
		t.Fatal("duplicate candidate churn should be handled by no-promote, not policy writeback")
	}
}

func readTraceFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "traces", name))
	if err != nil {
		t.Fatalf("read trace fixture %s: %v", name, err)
	}
	return data
}
