package work_test

import (
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/work"
)

const causalLoopEntity = "loop:causal-action"
const causalLoopMetric = "checkout_recovery_rate"

var causalLoopNow = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

func TestPromotedMeaningChangesNextWorkActionAndKeepsOldWorkReplayable(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	s, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}

	d1 := promoteDirective(t, ms, "D1", "", "run baseline checkout recovery", candidates.BasisDecisionRecord, causalLoopNow)
	pack1 := recordDirectiveContextPack(t, homePath, causalLoopNow.Add(time.Minute))
	requireOnlyActiveDecision(t, pack1, "decision://D1")
	w1 := createDirectiveWork(t, s, "work_causal_w1", pack1.ID, "", causalLoopNow.Add(2*time.Minute))
	requireActionSummary(t, s, w1.Work.ID, "baseline checkout recovery", "speculative checkout rewrite")

	parked := captureDirectiveCandidate(t, ms, "PARKED", "", "run speculative checkout rewrite", "", causalLoopNow.Add(3*time.Minute))
	parkRes, err := ms.Adjudicate(parked.ID, "park", "work_causal_w1", causalLoopNow.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if parkRes.Candidate.State != "parked" {
		t.Fatalf("conflicting unpromoted evidence should stay parked: %+v", parkRes.Candidate)
	}

	d2 := promoteDirective(t, ms, "D2", "D1", "run focused checkout recovery", candidates.BasisDecisionRecord, causalLoopNow.Add(5*time.Minute))
	if d2.Claim.Qualifiers["supersedes"] != "D1" || d1.Claim.Qualifiers["decision_id"] != "D1" {
		t.Fatalf("D2 should carry lifecycle supersession of D1: d1=%+v d2=%+v", d1.Claim.Qualifiers, d2.Claim.Qualifiers)
	}
	pack2 := recordDirectiveContextPack(t, homePath, causalLoopNow.Add(6*time.Minute))
	requireOnlyActiveDecision(t, pack2, "decision://D2")
	w2 := createDirectiveWork(t, s, "work_causal_w2", pack2.ID, "route_ok_d2", causalLoopNow.Add(7*time.Minute))
	requireActionSummary(t, s, w1.Work.ID, "baseline checkout recovery", "focused checkout recovery")
	requireActionSummary(t, s, w2.Work.ID, "focused checkout recovery", "baseline checkout recovery")
	requireActionSummary(t, s, w2.Work.ID, "focused checkout recovery", "speculative checkout rewrite")

	ai := envelope.ActionIntent{
		Header:        testHeader(envelope.SVActionIntent, "act_without_current_allow", causalLoopNow.Add(8*time.Minute), w2.Work.ID, pack2.ID, "private", "tier1"),
		Actor:         "codex",
		Capability:    "fixture.write",
		Action:        "mutate_without_current_policy_allow",
		Target:        envelope.ActionTarget{Kind: "fixture", ID: "causal-loop", Path: "fixtures/causal-loop.invalid"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		RollbackPlan:  "remove fixture write",
	}
	pd := envelope.PolicyDecision{
		Header:         testHeader(envelope.SVPolicyDecision, "pol_without_current_allow", causalLoopNow.Add(8*time.Minute), w2.Work.ID, pack2.ID, "private", "tier1"),
		ActionIntentID: ai.ID,
		Outcome:        "hold",
		Reasons:        []string{"no current allowing PolicyDecision"},
	}
	if _, err := s.RecordAction(w2.Work.ID, ai, pd, causalLoopNow.Add(8*time.Minute)); err == nil || !strings.Contains(err.Error(), "requires an allowing PolicyDecision") {
		t.Fatalf("mutation without allowing PolicyDecision should be rejected, got %v", err)
	}

	d3Candidate := captureDirectiveCandidate(t, ms, "D3", "D2", "run recovered checkout recovery", candidates.BasisRuntimeProof, causalLoopNow.Add(9*time.Minute))
	claimAndRun(t, s, w2.Work.ID, causalLoopNow.Add(10*time.Minute))
	failed, err := s.Close(w2.Work.ID, work.CloseInput{
		Outcome:          "failed",
		MoveType:         "outward_delta",
		Actor:            "codex",
		Summary:          causalLoopMetric + " stayed at 0.41 after route success",
		ProofRefs:        []string{"route:route_ok_d2:success", "metric:" + causalLoopMetric + "=0.41"},
		MemoryCandidates: []string{d3Candidate.ID},
	}, causalLoopNow.Add(11*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Work.RouteDecisionID != "route_ok_d2" || failed.Receipt.Outcome != "failed" || !containsString(failed.Receipt.ProofRefs, "route:route_ok_d2:success") {
		t.Fatalf("route success must coexist with task failure: work=%+v receipt=%+v", failed.Work, failed.Receipt)
	}
	if !containsString(failed.Receipt.Memory.Candidates, d3Candidate.ID) || len(failed.Receipt.Memory.Promoted) != 0 {
		t.Fatalf("failed result should create evidence candidate, not truth: %+v", failed.Receipt.Memory)
	}
	packBeforeD3Promotion := recordDirectiveContextPack(t, homePath, causalLoopNow.Add(12*time.Minute))
	requireOnlyActiveDecision(t, packBeforeD3Promotion, "decision://D2")

	d3, err := ms.Adjudicate(d3Candidate.ID, "promote", w2.Work.ID, causalLoopNow.Add(13*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if d3.Claim == nil || d3.Claim.Qualifiers["supersedes"] != "D2" {
		t.Fatalf("D3 promotion should carry lifecycle supersession of D2: %+v", d3)
	}
	if _, err := ms.FoldPositions(mem.PositionFoldOptions{}, causalLoopNow.Add(14*time.Minute)); err != nil {
		t.Fatal(err)
	}
	pack3 := recordDirectiveContextPack(t, homePath, causalLoopNow.Add(15*time.Minute))
	requireOnlyActiveDecision(t, pack3, "decision://D3")
	w3 := createDirectiveWork(t, s, "work_causal_w3", pack3.ID, "", causalLoopNow.Add(16*time.Minute))
	requireActionSummary(t, s, w3.Work.ID, "recovered checkout recovery", "focused checkout recovery")

	claimAndRun(t, s, w3.Work.ID, causalLoopNow.Add(17*time.Minute))
	completed, err := s.Close(w3.Work.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Changed:   true,
		Summary:   causalLoopMetric + " improved from 0.41 to 0.93",
		ProofRefs: []string{"metric:" + causalLoopMetric + "=0.93>0.41", "test:causal-loop-fixture"},
	}, causalLoopNow.Add(18*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Transition.SecondOrder.FutureBehaviorChanged || !strings.Contains(completed.Receipt.StateDelta.Summary, causalLoopMetric) {
		t.Fatalf("completed work should prove named metric improvement and future behavior change: %+v %+v", completed.Receipt, completed.Transition)
	}
}

func captureDirectiveCandidate(t *testing.T, ms *mem.Store, decisionID, supersedes, directive, basis string, now time.Time) mem.Candidate {
	t.Helper()
	qualifiers := map[string]string{
		"decision_id":      decisionID,
		"lifecycle_status": "accepted",
		"metric":           causalLoopMetric,
	}
	if strings.TrimSpace(supersedes) != "" {
		qualifiers["supersedes"] = strings.TrimSpace(supersedes)
	}
	cands, err := ms.Capture([]mem.Candidate{{
		Entity:          causalLoopEntity,
		Predicate:       "action_directive",
		Object:          directive,
		ObservationDate: now.UTC().Format(time.RFC3339),
		Confidence:      0.97,
		Derivation:      "deterministic",
		ClaimType:       "decision",
		Qualifiers:      qualifiers,
		PromotionBasis:  basis,
		SourceTraceRef:  "fixture://causal-loop/" + decisionID,
		SnippetHash:     "sha256:" + strings.ToLower(decisionID),
		Visibility:      "operational",
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("capture produced %d candidates, want 1", len(cands))
	}
	return cands[0]
}

func promoteDirective(t *testing.T, ms *mem.Store, decisionID, supersedes, directive, basis string, now time.Time) mem.AdjudicateResult {
	t.Helper()
	cand := captureDirectiveCandidate(t, ms, decisionID, supersedes, directive, basis, now)
	res, err := ms.Adjudicate(cand.ID, "promote", "work_promote_"+strings.ToLower(decisionID), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if res.Claim == nil {
		t.Fatalf("promotion produced no claim: %+v", res)
	}
	if _, err := ms.FoldPositions(mem.PositionFoldOptions{}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return res
}

func requireOnlyActiveDecision(t *testing.T, pack envelope.ContextPack, want string) {
	t.Helper()
	if len(pack.ActiveDecisions) != 1 || pack.ActiveDecisions[0] != want {
		t.Fatalf("active decisions = %v, want only %s", pack.ActiveDecisions, want)
	}
}

func recordDirectiveContextPack(t *testing.T, homePath string, now time.Time) envelope.ContextPack {
	t.Helper()
	in := contract.Input{
		Harness:     "codex",
		Cwd:         "/repo/causal-loop",
		Task:        "execute causal loop action directive for checkout recovery metric",
		RepoKind:    "control-plane",
		RepoKindVia: "fixture",
		Mode:        "execute",
	}
	runtime, err := contract.MaterializeRuntime(in, contract.DefaultPolicy(), homePath, now)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = contract.SealRuntimeMaterialization(runtime)
	if err != nil {
		t.Fatal(err)
	}
	pack := runtime.ContextPack
	if len(pack.ActivePositions) == 0 && len(pack.ActiveDecisions) == 0 {
		t.Fatalf("context pack has no active positions or decisions: active_memory=%v summary=%s", pack.ActiveMemory, pack.SummaryMD)
	}
	if _, err := contract.RecordRuntimeMaterialization(homePath, runtime); err != nil {
		t.Fatal(err)
	}
	return pack
}

func createDirectiveWork(t *testing.T, s *work.Store, workID, contextPackID, routeDecisionID string, now time.Time) work.AdmitCreatePlanReport {
	t.Helper()
	plan, err := s.CreatePlan(work.CreateInput{
		WorkID:           workID,
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "causal loop action follows promoted directive",
		DesiredOutcome:   "meaning changes the next action",
		ContextPackID:    contextPackID,
		RouteDecisionID:  routeDecisionID,
	}, now, work.CreatePlanOptions{Actor: "codex", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.AdmitCreatePlan(plan, plan.PlanPath, now.Add(time.Second), work.AdmitCreatePlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if report.ActionContext == nil {
		t.Fatalf("admitted work missing action context from promoted meaning: %+v", report)
	}
	return report
}

func requireActionSummary(t *testing.T, s *work.Store, workID, want, forbidden string) {
	t.Helper()
	wb, err := s.Workbench(causalLoopNow.Add(30*time.Minute), work.WorkbenchOptions{RecentLimit: 50, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	var ctx *work.WorkActionContext
	for _, item := range wb.Open {
		if item.WorkID == workID {
			ctx = item.ActionContext
			break
		}
	}
	if ctx == nil {
		t.Fatalf("work %s has no action context in workbench: %+v", workID, wb.Open)
	}
	if !strings.Contains(ctx.ActionSummary, want) {
		t.Fatalf("work %s action summary = %q, want %q", workID, ctx.ActionSummary, want)
	}
	if forbidden != "" && strings.Contains(ctx.ActionSummary, forbidden) {
		t.Fatalf("work %s action summary = %q, must not contain %q", workID, ctx.ActionSummary, forbidden)
	}
}

func claimAndRun(t *testing.T, s *work.Store, workID string, now time.Time) {
	t.Helper()
	if _, err := s.Claim(workID, "codex", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(workID, work.AdvanceInput{To: "running", Actor: "codex"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func testHeader(schemaVersion, id string, now time.Time, workID, contextPackID, privacy, risk string) envelope.Header {
	return envelope.Header{
		SchemaVersion: schemaVersion,
		ID:            id,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Producer:      envelope.Producer{Kind: "test", Name: "causal-loop"},
		IDs:           envelope.IDs{WorkID: workID, ContextPackID: contextPackID},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       privacy,
		Risk:          risk,
		EvidenceRefs:  []string{},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
