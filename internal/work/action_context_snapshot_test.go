package work

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdmittedActionContextUsesImmutableSnapshot(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := &WorkActionContext{
		Kind:          "promoted_action_directive",
		SourceRef:     "decision://D1",
		SourceID:      "D1",
		ActionSummary: "run baseline recovery",
		EvidenceRefs:  []string{"decision://D1", "claim://claim_d1"},
	}
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_action_context_snapshot",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "admitted action context cannot be rewritten",
	}, fixedNow, CreatePlanOptions{Actor: "codex", ActionContext: ctx, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitCreatePlan(plan, plan.PlanPath, fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err != nil {
		t.Fatal(err)
	}

	_, events, err := s.Show(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotRef string
	for _, event := range events {
		for _, ref := range event.Attached {
			if strings.HasPrefix(ref, workActionContextSnapshotRefPrefix) {
				snapshotRef = ref
			}
		}
	}
	if snapshotRef == "" {
		t.Fatalf("admission event missing immutable action-context snapshot: %+v", events)
	}
	snapshotWork, err := s.Get(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateActionContextSnapshotForWork(snapshotWork, ctx); err != nil {
		t.Fatalf("matching action context snapshot rejected: %v", err)
	}
	differentContext := cloneWorkActionContext(ctx)
	differentContext.ActionSummary = "run different semantics"
	if err := s.ValidateActionContextSnapshotForWork(snapshotWork, differentContext); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("different expected action context was accepted: %v", err)
	}

	tampered := plan
	tampered.ActionContext = cloneWorkActionContext(plan.ActionContext)
	tampered.ActionContext.ActionSummary = "run attacker-selected rewrite"
	if err := s.writeJSON(plan.PlanPath, &tampered); err != nil {
		t.Fatal(err)
	}

	wb, err := s.Workbench(fixedNow.Add(2*time.Minute), WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	got := findWorkbenchItem(t, wb.Open, plan.WorkID).ActionContext
	if got == nil || got.ActionSummary != ctx.ActionSummary {
		t.Fatalf("workbench must consume admitted snapshot, got %+v", got)
	}
	if _, err := s.AdmitCreatePlan(tampered, plan.PlanPath, fixedNow.Add(3*time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "content digest mismatch") {
		t.Fatalf("rewritten sidecar must fail its plan seal, got %v", err)
	}
	resealed := tampered
	resealed.ID, err = createPlanContentID(resealed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitCreatePlan(resealed, plan.PlanPath, fixedNow.Add(4*time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "does not match admitted snapshot") {
		t.Fatalf("new sealed plan must not replace admitted context, got %v", err)
	}

	snapshotID := strings.TrimPrefix(snapshotRef, workActionContextSnapshotRefPrefix)
	snapshot, err := s.loadActionContextSnapshot(snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ActionContext.ActionSummary = "tampered snapshot payload"
	if err := s.writeJSON(filepath.Join(s.home, "work", "action-contexts", snapshotID+".json"), &snapshot); err != nil {
		t.Fatal(err)
	}
	admittedWork, err := s.Get(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	contextAfterTamper, warnings := s.actionContextForWork(admittedWork)
	if contextAfterTamper != nil || len(warnings) == 0 || !strings.Contains(strings.Join(warnings, " "), "content digest mismatch") {
		t.Fatalf("tampered snapshot must fail closed: context=%+v warnings=%v", contextAfterTamper, warnings)
	}
}

func TestAdmittedNilActionContextUsesImmutableSnapshot(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_nil_action_context_snapshot",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "absence of action context cannot be rewritten",
	}, fixedNow, CreatePlanOptions{Actor: "codex", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ActionContext != nil {
		t.Fatalf("fixture should begin without action context: %+v", plan.ActionContext)
	}
	if _, err := s.AdmitCreatePlan(plan, plan.PlanPath, fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err != nil {
		t.Fatal(err)
	}

	_, events, err := s.Show(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	foundSnapshot := false
	for _, event := range events {
		for _, ref := range event.Attached {
			foundSnapshot = foundSnapshot || strings.HasPrefix(ref, workActionContextSnapshotRefPrefix)
		}
	}
	if !foundSnapshot {
		t.Fatalf("nil action context must still record a snapshot sentinel: %+v", events)
	}

	tampered := plan
	tampered.ActionContext = &WorkActionContext{
		Kind:          "promoted_action_directive",
		SourceRef:     "decision://INJECTED",
		ActionSummary: "run injected sidecar action",
	}
	if err := s.writeJSON(plan.PlanPath, &tampered); err != nil {
		t.Fatal(err)
	}
	wb, err := s.Workbench(fixedNow.Add(2*time.Minute), WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findWorkbenchItem(t, wb.Open, plan.WorkID).ActionContext; got != nil {
		t.Fatalf("nil snapshot inherited injected sidecar context: %+v", got)
	}
	if _, err := s.AdmitCreatePlan(tampered, plan.PlanPath, fixedNow.Add(3*time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "content digest mismatch") {
		t.Fatalf("injected sidecar must fail its plan seal, got %v", err)
	}
	resealed := tampered
	resealed.ID, err = createPlanContentID(resealed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitCreatePlan(resealed, plan.PlanPath, fixedNow.Add(4*time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "does not match admitted snapshot") {
		t.Fatalf("new sealed plan must not replace admitted nil context, got %v", err)
	}
}

func TestCreatePlanRejectsPreAdmissionContentTamper(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_pre_admission_plan_tamper",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "ready",
		SuccessPredicate: "create-plan content is sealed before admission",
	}, fixedNow, CreatePlanOptions{
		Actor: "codex",
		ActionContext: &WorkActionContext{
			Kind:          "promoted_action_directive",
			SourceRef:     "decision://D1",
			ActionSummary: "run reviewed action",
		},
		Record: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tampered := plan
	tampered.ActionContext = cloneWorkActionContext(plan.ActionContext)
	tampered.ActionContext.ActionSummary = "run pre-admission injected action"
	if err := s.writeJSON(plan.PlanPath, &tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitCreatePlanPath(plan.PlanPath, fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "content digest mismatch") {
		t.Fatalf("pre-admission sidecar tamper must be rejected, got %v", err)
	}
	tampered.ID, err = createPlanContentID(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeJSON(plan.PlanPath, &tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitCreatePlanPath(plan.PlanPath, fixedNow.Add(2*time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "path identity mismatch") {
		t.Fatalf("resealed content under the old plan path must be rejected, got %v", err)
	}
	if _, err := s.Get(plan.WorkID); err == nil {
		t.Fatalf("tampered plan must not create work %s", plan.WorkID)
	}
}
