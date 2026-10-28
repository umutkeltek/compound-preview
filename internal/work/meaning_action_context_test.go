package work_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/work"
)

func TestPromotedMeaningRequiresExactCurrentContextRevision(t *testing.T) {
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

	promoteDirective(t, ms, "EXACT-D1", "", "run exact baseline", candidates.BasisDecisionRecord, causalLoopNow)
	oldPack := recordDirectiveContextPack(t, homePath, causalLoopNow.Add(time.Minute))
	promoteDirective(t, ms, "EXACT-D2", "EXACT-D1", "run exact successor", candidates.BasisDecisionRecord, causalLoopNow.Add(2*time.Minute))

	stalePlan, err := s.CreatePlan(work.CreateInput{
		WorkID:           "work_stale_meaning_revision",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "stale ContextPack revision cannot steer new work",
		ContextPackID:    oldPack.ID,
	}, causalLoopNow.Add(3*time.Minute), work.CreatePlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if stalePlan.ActionContext != nil {
		t.Fatalf("superseded exact claim revision steered new work: %+v", stalePlan.ActionContext)
	}
	if stalePlan.MeaningResolution != "none" || len(stalePlan.AmbiguousRefs) != 0 {
		t.Fatalf("stale exact revision resolution = %q refs=%v", stalePlan.MeaningResolution, stalePlan.AmbiguousRefs)
	}

	currentPack := recordDirectiveContextPack(t, homePath, causalLoopNow.Add(4*time.Minute))
	currentPlan, err := s.CreatePlan(work.CreateInput{
		WorkID:           "work_current_meaning_revision",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "current ContextPack revision steers new work",
		ContextPackID:    currentPack.ID,
	}, causalLoopNow.Add(5*time.Minute), work.CreatePlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if currentPlan.ActionContext == nil || currentPlan.ActionContext.ActionSummary != "run exact successor" {
		t.Fatalf("current exact revision missing from action context: %+v", currentPlan.ActionContext)
	}
	if currentPlan.MeaningResolution != "resolved" || len(currentPlan.AmbiguousRefs) != 0 {
		t.Fatalf("current exact revision resolution = %q refs=%v", currentPlan.MeaningResolution, currentPlan.AmbiguousRefs)
	}
}

func TestPromotedMeaningFailsClosedWhenContextAuthorityUnreadable(t *testing.T) {
	t.Parallel()
	s, err := work.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreatePlan(work.CreateInput{
		WorkID:           "work_missing_meaning_authority",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "unreadable meaning authority holds plan creation",
		ContextPackID:    "ctx_missing_authority",
	}, causalLoopNow, work.CreatePlanOptions{Actor: "codex"})
	if err == nil || !strings.Contains(err.Error(), "read ContextPack ctx_missing_authority for promoted meaning") {
		t.Fatalf("missing ContextPack must fail closed, got %v", err)
	}
}

func TestPromotedMeaningIgnoresNonAcceptedAndAmbiguousDirectives(t *testing.T) {
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

	nonAccepted := captureDirectiveWithStatus(t, ms, "REJECTED", "rejected", "run rejected directive", causalLoopNow)
	if _, err := ms.Adjudicate(nonAccepted.ID, "promote", "work_promote_rejected", causalLoopNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	packWithoutAccepted := recordPossiblyEmptyDirectiveContextPack(t, homePath, causalLoopNow.Add(2*time.Minute))
	planWithoutAccepted, err := s.CreatePlan(work.CreateInput{
		WorkID:           "work_nonaccepted_meaning",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "non-accepted meaning cannot steer work",
		ContextPackID:    packWithoutAccepted.ID,
	}, causalLoopNow.Add(3*time.Minute), work.CreatePlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if planWithoutAccepted.ActionContext != nil {
		t.Fatalf("non-accepted directive steered work: %+v", planWithoutAccepted.ActionContext)
	}
	if planWithoutAccepted.MeaningResolution != "none" || len(planWithoutAccepted.AmbiguousRefs) != 0 {
		t.Fatalf("non-accepted resolution = %q refs=%v", planWithoutAccepted.MeaningResolution, planWithoutAccepted.AmbiguousRefs)
	}

	promoteDirective(t, ms, "AMB-A", "", "run ambiguous A", candidates.BasisDecisionRecord, causalLoopNow.Add(4*time.Minute))
	promoteDirective(t, ms, "AMB-B", "", "run ambiguous B", candidates.BasisDecisionRecord, causalLoopNow.Add(5*time.Minute))
	ambiguousPack := recordDirectiveContextPack(t, homePath, causalLoopNow.Add(6*time.Minute))
	ambiguousPlan, err := s.CreatePlan(work.CreateInput{
		WorkID:           "work_ambiguous_meaning",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "ambiguous meaning cannot select an action",
		ContextPackID:    ambiguousPack.ID,
	}, causalLoopNow.Add(7*time.Minute), work.CreatePlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if ambiguousPlan.ActionContext != nil {
		t.Fatalf("ambiguous directives selected an action: %+v", ambiguousPlan.ActionContext)
	}
	wantAmbiguousRefs := []string{"decision://AMB-A", "decision://AMB-B"}
	if ambiguousPlan.MeaningResolution != "ambiguous" || !reflect.DeepEqual(ambiguousPlan.AmbiguousRefs, wantAmbiguousRefs) {
		t.Fatalf("ambiguous resolution = %q refs=%v, want refs=%v", ambiguousPlan.MeaningResolution, ambiguousPlan.AmbiguousRefs, wantAmbiguousRefs)
	}
	if _, err := s.AdmitCreatePlan(ambiguousPlan, "", causalLoopNow.Add(8*time.Minute), work.AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous plan admission error = %v", err)
	}
}

func recordPossiblyEmptyDirectiveContextPack(t *testing.T, homePath string, now time.Time) envelope.ContextPack {
	t.Helper()
	in := contract.Input{
		Harness:     "codex",
		Cwd:         "/repo/meaning-authority",
		Task:        "evaluate promoted action directives",
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
	if _, err := contract.RecordRuntimeMaterialization(homePath, runtime); err != nil {
		t.Fatal(err)
	}
	return runtime.ContextPack
}

func captureDirectiveWithStatus(t *testing.T, ms *mem.Store, decisionID, status, directive string, now time.Time) mem.Candidate {
	t.Helper()
	candidatesOut, err := ms.Capture([]mem.Candidate{{
		Entity:          "loop:status-meaning",
		Predicate:       "action_directive",
		Object:          directive,
		ObservationDate: now.UTC().Format(time.RFC3339),
		Confidence:      0.99,
		Derivation:      "deterministic",
		ClaimType:       "decision",
		Qualifiers: map[string]string{
			"decision_id":      decisionID,
			"lifecycle_status": status,
		},
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "fixture://meaning-status/" + decisionID,
		SnippetHash:    "sha256:" + strings.ToLower(decisionID),
		Visibility:     "operational",
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidatesOut) != 1 {
		t.Fatalf("capture produced %d candidates", len(candidatesOut))
	}
	return candidatesOut[0]
}
