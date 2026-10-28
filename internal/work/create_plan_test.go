package work

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestCreatePlanRecordsPolicyWithoutCreatingWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create plan stays proposal-only",
		DesiredOutcome:   "show admission readiness",
		EvidenceRefs:     []string{"test://create-plan"},
	}, fixedNow, CreatePlanOptions{Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "create_policy_allowed" || !plan.Policy.Allowed {
		t.Fatalf("plan policy not allowed: %+v", plan)
	}
	if plan.ActionIntent.Capability != "work.create" || plan.Decision.ActionIntentID != plan.ActionIntent.ID {
		t.Fatalf("intent/decision not threaded: intent=%s decision=%s", plan.ActionIntent.ID, plan.Decision.ActionIntentID)
	}
	if plan.PlanPath == "" {
		t.Fatalf("missing recorded plan path: %+v", plan)
	}
	if _, err := os.Stat(plan.PlanPath); err != nil {
		t.Fatalf("recorded plan missing: %v", err)
	}
	if _, err := s.Get(plan.WorkID); err == nil {
		t.Fatalf("create-plan must not write work %s", plan.WorkID)
	}
	events, err := s.eventsFor(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("create-plan must not write lifecycle events: %+v", events)
	}
}

func TestCreatePlanRejectsRunningState(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.CreatePlan(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "running",
		SuccessPredicate: "direct running create is rejected",
	}, fixedNow, CreatePlanOptions{Record: true})
	if err == nil || !strings.Contains(err.Error(), "create at ready, then claim and advance") {
		t.Fatalf("CreatePlan running error = %v", err)
	}
	if _, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "running",
		SuccessPredicate: "direct running create is rejected",
	}, fixedNow); err == nil || !strings.Contains(err.Error(), "create at ready, then claim and advance") {
		t.Fatalf("Create running error = %v", err)
	}
}

func TestCreatePlanRejectsTerminalInitialStatesWithoutRecording(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"completed", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := newStore(t)
			workID := "work_terminal_plan_" + state
			_, err := s.CreatePlan(CreateInput{
				WorkID:           workID,
				Owner:            "compound-core",
				Actor:            "codex",
				State:            state,
				SuccessPredicate: "terminal work must be closed with proof",
			}, fixedNow, CreatePlanOptions{Record: true})
			if err == nil || !strings.Contains(err.Error(), "use Close") {
				t.Errorf("CreatePlan terminal state %q error = %v", state, err)
			}
			if _, err := s.Get(workID); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("rejected terminal plan persisted work %s: %v", workID, err)
			}
			plans, err := filepath.Glob(filepath.Join(s.home, "create-plans", "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if len(plans) != 0 {
				t.Errorf("rejected terminal plan recorded artifacts: %v", plans)
			}
		})
	}
}

func TestCreateRejectsTerminalInitialStatesWithoutLifecycleArtifacts(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"completed", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := newStore(t)
			workID := "work_terminal_create_" + state
			_, err := s.Create(CreateInput{
				WorkID:           workID,
				Owner:            "compound-core",
				Actor:            "codex",
				State:            state,
				SuccessPredicate: "terminal work must be closed with proof",
			}, fixedNow)
			if err == nil || !strings.Contains(err.Error(), "use Close") {
				t.Errorf("Create terminal state %q error = %v", state, err)
			}
			if _, err := s.Get(workID); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("rejected terminal create persisted work %s: %v", workID, err)
			}
			events, err := s.eventsFor(workID)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Errorf("rejected terminal create recorded lifecycle events: %+v", events)
			}
		})
	}
}

func TestAdmitCreatePlanRejectsResealedTerminalState(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"completed", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := newStore(t)
			workID := "work_terminal_admission_" + state
			plan, err := s.CreatePlan(CreateInput{
				WorkID:           workID,
				Owner:            "compound-core",
				Actor:            "codex",
				SuccessPredicate: "create-plan admission cannot bypass Close",
			}, fixedNow, CreatePlanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			plan.Work.State = state
			plan.WorkState = state
			plan.ID, err = createPlanContentID(plan)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := s.AdmitCreatePlan(plan, "", fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "use Close") {
				t.Errorf("AdmitCreatePlan terminal state %q error = %v", state, err)
			}
			if _, err := s.Get(workID); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("rejected terminal admission persisted work %s: %v", workID, err)
			}
			events, err := s.eventsFor(workID)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Errorf("rejected terminal admission recorded lifecycle events: %+v", events)
			}
		})
	}
}

func TestCreateRequiresDirectorProposalToStartProposed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for _, state := range []string{"", "ready", "admitted"} {
		name := state
		if name == "" {
			name = "default"
		}
		workID := "work_director_initial_" + name
		_, err := s.Create(CreateInput{
			WorkID:           workID,
			Owner:            "compound-authority",
			Actor:            "codex",
			State:            state,
			SuccessPredicate: "director-proposed work cannot skip admission",
			EvidenceRefs:     []string{"proposed_by:director", "director_trigger:job_health:" + name},
		}, fixedNow)
		if err == nil || !strings.Contains(err.Error(), "director-proposed work must start in proposed state") {
			t.Fatalf("Create director-proposed state %q error = %v", state, err)
		}
		if _, err := s.Get(workID); err == nil {
			t.Fatalf("rejected director-proposed state %q persisted work %s", state, workID)
		}
	}

	allowed, err := s.Create(CreateInput{
		WorkID:           "work_director_initial_proposed",
		Owner:            "compound-authority",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "director-proposed work starts before admission",
		EvidenceRefs:     []string{"proposed_by:director", "director_trigger:job_health:proposed"},
	}, fixedNow)
	if err != nil || allowed.State != "proposed" {
		t.Fatalf("proposed director-proposed create failed: work=%+v err=%v", allowed, err)
	}
}

func TestAdmitCreatePlanRejectsDirectorOriginOutsideProposed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_legacy_director_ready_plan",
		Owner:            "compound-authority",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "legacy director plan cannot skip admission",
		EvidenceRefs:     []string{"proposed_by:director", "director_trigger:job_health:legacy-ready"},
	}, fixedNow, CreatePlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan.Work.State = "ready"
	plan.WorkState = "ready"
	plan.ID, err = createPlanContentID(plan)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AdmitCreatePlan(plan, "", fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "codex"}); err == nil || !strings.Contains(err.Error(), "director-proposed work must start in proposed state") {
		t.Fatalf("legacy ready director create-plan admission error = %v", err)
	}
	if _, err := s.Get(plan.WorkID); err == nil {
		t.Fatalf("rejected legacy director create-plan persisted work %s", plan.WorkID)
	}
}

func TestAdmitCreatePlanCreatesWorkAndRetiresPreview(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_admit_create_plan",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "admitted create-plan becomes work",
		DesiredOutcome:   "preserve explicit admission preview",
		NonGoals:         []string{"do not run the planned action"},
		EvidenceRefs:     []string{"test://admit-create-plan"},
	}, fixedNow, CreatePlanOptions{
		Record:        true,
		Capability:    "product.action_review",
		Action:        "plan_work_from_reviewed_product_action",
		TargetKind:    "product_action_review",
		TargetID:      "reviewed_demo",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if before.Counts.AdmissionPreviews != 1 {
		t.Fatalf("expected one admission preview before admit: %+v", before.AdmissionPreviews)
	}
	planRel := filepath.ToSlash(filepath.Join("create-plans", filepath.Base(plan.PlanPath)))
	if !sameStringSlice(before.AdmissionPreviews[0].RetireCommand, []string{"compound", "home", "archive-history", "--kind", "create-plan", "--path", planRel}) ||
		!sameStringSlice(before.AdmissionPreviews[0].RetireDryRun, []string{"compound", "home", "archive-history", "--kind", "create-plan", "--path", planRel, "--dry-run"}) {
		t.Fatalf("fresh admission preview missing retire command: %+v", before.AdmissionPreviews[0])
	}

	report, err := s.AdmitCreatePlanPath(plan.PlanPath, fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "admitted" ||
		report.PlanID != plan.ID ||
		report.WorkID != plan.WorkID ||
		report.WorkState != "proposed" ||
		report.ActionIntentID != plan.ActionIntent.ID ||
		report.PolicyDecisionID != plan.Decision.ID ||
		!report.PolicyAllowed {
		t.Fatalf("admission report did not preserve plan fields: %+v", report)
	}
	w, events, err := s.Show(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != plan.WorkID ||
		w.State != "proposed" ||
		w.PolicyDecisionID != plan.Decision.ID ||
		len(w.ActionIntentIDs) != 1 ||
		w.ActionIntentIDs[0] != plan.ActionIntent.ID {
		t.Fatalf("admitted work not threaded through create-plan: %+v", w)
	}
	if len(events) != 1 ||
		events[0].To != "proposed" ||
		events[0].Note != "admitted_from_create_plan" ||
		!contains(events[0].Attached, plan.ActionIntent.ID) ||
		!contains(events[0].Attached, plan.Decision.ID) ||
		!contains(events[0].Attached, "create_plan:"+plan.ID) {
		t.Fatalf("admission event did not preserve proof chain: %+v", events)
	}
	after, err := s.Workbench(fixedNow.Add(2*time.Minute), WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if after.Counts.AdmissionPreviews != 0 || len(after.AdmissionPreviews) != 0 {
		t.Fatalf("admitted work should retire admission preview: counts=%+v previews=%+v", after.Counts, after.AdmissionPreviews)
	}

	again, err := s.AdmitCreatePlanPath(plan.PlanPath, fixedNow.Add(3*time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, repeatedEvents, err := s.Show(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "already_admitted" || again.NoAction == "" || len(repeatedEvents) != 1 {
		t.Fatalf("repeat admission should be idempotent: report=%+v events=%+v", again, repeatedEvents)
	}
}

// TestAdmitCreatePlanEvidenceDurabilityOrdering proves the fail-closed
// ordering: the action_intent and policy_decision evidence must be durable in
// the eventstore BEFORE the WorkEnvelope becomes visible at its canonical path.
// The test blocks the work-file rename by pre-creating a directory at the
// destination, then verifies that (a) the error surfaces, (b) the work file
// does not exist, and (c) the evidence files DO exist.
func TestAdmitCreatePlanEvidenceDurabilityOrdering(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_evidence_ordering",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "evidence-before-work",
	}, fixedNow, CreatePlanOptions{Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "create_policy_allowed" {
		t.Fatalf("plan not allowed: %+v", plan.Policy)
	}

	// Block the final work-file stage by making its parent directory read-only.
	// The staged file is created in the same directory, so os.CreateTemp fails
	// without preventing the preceding eventstore evidence writes (which land in
	// COMPOUND_HOME/events). The work file path itself remains absent, so s.Get
	// still returns os.ErrNotExist during the admission existence check.
	workDir := filepath.Join(s.home, "work", "work")
	if err := os.Chmod(workDir, 0o555); err != nil {
		t.Fatalf("chmod work dir read-only: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(workDir, 0o755)
	})

	_, err = s.AdmitCreatePlanPath(plan.PlanPath, fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
	if err == nil {
		t.Fatal("expected work-file stage error, got nil")
	}

	// The WorkEnvelope must NOT be visible as a file.
	if _, err := os.Stat(s.workPath(plan.WorkID)); !os.IsNotExist(err) {
		t.Fatalf("work file exists after failed stage: %v", err)
	}

	// The action intent and policy decision must be durable.
	aiBytes, err := s.eventBytes(plan.ActionIntent.ID)
	if err != nil {
		t.Fatalf("action_intent not durable: %v", err)
	}
	var ai envelope.ActionIntent
	if err := json.Unmarshal(aiBytes, &ai); err != nil {
		t.Fatalf("action_intent unmarshal: %v", err)
	}
	if ai.ID != plan.ActionIntent.ID {
		t.Fatalf("durable action_intent id mismatch: %q vs %q", ai.ID, plan.ActionIntent.ID)
	}
	pdBytes, err := s.eventBytes(plan.Decision.ID)
	if err != nil {
		t.Fatalf("policy_decision not durable: %v", err)
	}
	var decision envelope.PolicyDecision
	if err := json.Unmarshal(pdBytes, &decision); err != nil {
		t.Fatalf("policy_decision unmarshal: %v", err)
	}
	if decision.ID != plan.Decision.ID {
		t.Fatalf("durable policy_decision id mismatch: %q vs %q", decision.ID, plan.Decision.ID)
	}
}

func TestCreatePlanCommandCarriesPlannedWorkID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeContextPack(t, s, "ctx_planned")
	plan, err := s.CreatePlan(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "created work uses planned identity",
		DesiredOutcome:   "preserve planned fields",
		NonGoals:         []string{"do not recompute identity"},
		EvidenceRefs:     []string{"test://planned-work"},
		ContextPackID:    "ctx_planned",
	}, fixedNow, CreatePlanOptions{Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(plan.CreateCommand, "--work-id") || !contains(plan.CreateCommand, plan.WorkID) {
		t.Fatalf("create command must preserve planned work id %s: %v", plan.WorkID, plan.CreateCommand)
	}
	for _, want := range []string{"--outcome", "preserve planned fields", "--non-goal", "do not recompute identity", "--evidence-ref", "test://planned-work", "--context-pack", "ctx_planned"} {
		if !contains(plan.CreateCommand, want) {
			t.Fatalf("create command missing %q: %v", want, plan.CreateCommand)
		}
	}
}

func TestCreatePreservesExplicitWorkID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	plannedID := "work_planned_identity"
	w, err := s.Create(CreateInput{
		WorkID:           plannedID,
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "created work uses planned identity",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != plannedID {
		t.Fatalf("created work id = %q, want planned id %q", w.ID, plannedID)
	}
	if _, err := s.Get(plannedID); err != nil {
		t.Fatalf("planned work id was not persisted: %v", err)
	}
	events, err := s.eventsFor(plannedID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].WorkID != plannedID || events[0].To != "ready" {
		t.Fatalf("create event not threaded through planned id: %+v", events)
	}
}

func TestCreateRejectsInvalidExplicitWorkID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.Create(CreateInput{
		WorkID:           "planned_identity",
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "created work validates planned identity",
	}, fixedNow)
	if err == nil || !strings.Contains(err.Error(), `must start with "work_"`) {
		t.Fatalf("expected work id prefix validation, got %v", err)
	}
}

func TestCreateRecordsAdmissionPolicyBeforeWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeContextPack(t, s, "ctx_create")
	w, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create records policy chain",
		ContextPackID:    "ctx_create",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if w.PolicyDecisionID == "" || len(w.ActionIntentIDs) != 1 {
		t.Fatalf("created work missing admission policy/action: %+v", w)
	}
	var ai envelope.ActionIntent
	if err := s.readJSON(s.envPath(w.ActionIntentIDs[0]), &ai); err != nil {
		t.Fatalf("admission action missing: %v", err)
	}
	if ai.Capability != "work.create" || ai.IDs.WorkID != w.ID {
		t.Fatalf("admission action not bound to work: %+v", ai)
	}
	if exists, err := s.eventExists(w.PolicyDecisionID); err != nil || !exists {
		t.Fatalf("admission policy missing: %v", err)
	}
	_, events, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !contains(events[0].Attached, ai.ID) || !contains(events[0].Attached, w.PolicyDecisionID) || !contains(events[0].Attached, "ctx_create") {
		t.Fatalf("create event missing policy/context attachments: %+v", events)
	}
}

func TestCreateRejectsMissingContextPack(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create refuses missing context pack",
		ContextPackID:    "ctx_missing",
	}, fixedNow)
	if err == nil || !strings.Contains(err.Error(), `context_pack_id "ctx_missing"`) {
		t.Fatalf("expected missing context pack error, got %v", err)
	}
	works, listErr := s.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(works) != 0 {
		t.Fatalf("missing context pack create must not persist work: %+v", works)
	}
}

func TestCreateRejectsContextPackMissingEffectiveContract(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeContextPackWithContractID(t, s, "ctx_with_missing_contract", "contract_missing")
	_, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create refuses context pack with missing contract",
		ContextPackID:    "ctx_with_missing_contract",
	}, fixedNow)
	if err == nil || !strings.Contains(err.Error(), `contract_id "contract_missing"`) {
		t.Fatalf("expected missing effective contract error, got %v", err)
	}
	works, listErr := s.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(works) != 0 {
		t.Fatalf("missing effective contract create must not persist work: %+v", works)
	}
}

func TestCreateAcceptsContextPackWithEffectiveContract(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeEffectiveContract(t, s, "contract_present")
	writeContextPackWithContractID(t, s, "ctx_with_contract", "contract_present")
	w, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create accepts context pack with matching contract",
		ContextPackID:    "ctx_with_contract",
	}, fixedNow)
	if err != nil {
		t.Fatalf("create with context pack contract: %v", err)
	}
	if w.IDs.ContextPackID != "ctx_with_contract" {
		t.Fatalf("work context_pack_id = %q, want ctx_with_contract", w.IDs.ContextPackID)
	}
}

func TestCreateBlockedByPolicyDoesNotMutate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	policyYAML := []byte(`class_outcome:
  local_reversible:
    outcome: hold
`)
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), policyYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(CreateInput{Owner: "compound-core", Actor: "codex", SuccessPredicate: "held create"}, fixedNow)
	if err == nil {
		t.Fatal("create should be blocked by policy hold")
	}
	if w.ID == "" {
		t.Fatal("blocked create should return candidate work id for diagnostics")
	}
	if _, err := s.Get(w.ID); err == nil {
		t.Fatalf("blocked create wrote work %s", w.ID)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("blocked create wrote work entries: %+v", list)
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("blocked create wrote lifecycle events: %+v", events)
	}
}

func TestCreatePlanActionContextReachesWorkbenchAndDispatch(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := &WorkActionContext{
		Kind:               "product_action_review",
		SourceRef:          "compound://reviews/product-action-cards/reviewed_demo.json",
		SourceID:           "parr_demo",
		Title:              "Import daily product signal",
		Command:            []string{"compound", "product", "signal", "fixture.yaml"},
		RequiredInputs:     []string{"fixture signal"},
		ExpectedValidation: []string{"product timeline changes"},
		StopConditions:     []string{"missing product authority"},
		Boundary:           "temp home only",
		Proof:              "timeline transition appears",
		EvidenceRefs:       []string{"product_action_review:parr_demo"},
	}
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_product_action_context",
		Owner:            "product:demo",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "reviewed action carries runnable context",
		DesiredOutcome:   "future executor does not reread raw product review",
		EvidenceRefs:     []string{"product_action_review:parr_demo"},
	}, fixedNow, CreatePlanOptions{
		Actor:         "codex",
		Capability:    "product.action_review",
		Action:        "plan_work_from_reviewed_product_action",
		TargetKind:    "product_action_review",
		TargetID:      "parr_demo",
		ActionContext: ctx,
		Record:        true,
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ActionContext == nil || !sameStringSlice(plan.ActionContext.Command, ctx.Command) {
		t.Fatalf("create-plan missing action context: %+v", plan.ActionContext)
	}
	report, err := s.AdmitCreatePlan(plan, plan.PlanPath, fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.ActionContext == nil || report.ActionContext.Boundary != ctx.Boundary || report.ActionContext.Proof != ctx.Proof {
		t.Fatalf("admission report missing action context: %+v", report.ActionContext)
	}
	wb, err := s.Workbench(fixedNow.Add(2*time.Minute), WorkbenchOptions{RecentLimit: 5, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, wb.Open, plan.WorkID)
	if item.ActionContext == nil ||
		!sameStringSlice(item.ActionContext.Command, ctx.Command) ||
		item.ActionContext.Boundary != ctx.Boundary ||
		item.ActionContext.Proof != ctx.Proof ||
		!strings.Contains(item.NextAction, "prepare ready with reviewed action context") ||
		!contains(item.EvidenceRefs, "product_action_review:parr_demo") {
		t.Fatalf("workbench item missing runnable action context: %+v", item)
	}
	dispatch, err := s.DispatchPlan(fixedNow.Add(3*time.Minute), DispatchPlanOptions{Actor: "codex", RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	var candidate *DispatchPlanItem
	for i := range dispatch.Candidates {
		if dispatch.Candidates[i].WorkID == plan.WorkID {
			candidate = &dispatch.Candidates[i]
			break
		}
	}
	if candidate == nil || candidate.ActionContext == nil ||
		!sameStringSlice(candidate.ActionContext.Command, ctx.Command) ||
		candidate.ActionContext.SourceRef != ctx.SourceRef ||
		!sameStringSlice(candidate.Command, []string{"compound", "work", "advance-plan", plan.WorkID, "--to", "ready", "--actor", "codex"}) {
		t.Fatalf("dispatch candidate missing action context: candidate=%+v dispatch=%+v", candidate, dispatch)
	}

	readyPlan, err := s.AdvancePlan(plan.WorkID, fixedNow.Add(4*time.Minute), AdvancePlanOptions{To: "ready", Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if readyPlan.Status != "advance_policy_allowed" {
		t.Fatalf("ready advance-plan not allowed: %+v", readyPlan)
	}
	requireWorkActionContext(t, "ready advance-plan", readyPlan.ActionContext, ctx)

	ready, err := s.Advance(plan.WorkID, AdvanceInput{To: "ready", Actor: "codex"}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != "ready" {
		t.Fatalf("work not advanced ready: %+v", ready)
	}
	wb, err = s.Workbench(fixedNow.Add(6*time.Minute), WorkbenchOptions{RecentLimit: 5, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	requireWorkActionContext(t, "ready workbench", findWorkbenchItem(t, wb.Open, plan.WorkID).ActionContext, ctx)

	claimPlan, err := s.ClaimPlan(plan.WorkID, fixedNow.Add(7*time.Minute), ClaimPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if claimPlan.Status != "claim_policy_allowed" {
		t.Fatalf("claim-plan not allowed: %+v", claimPlan)
	}
	requireWorkActionContext(t, "claim-plan", claimPlan.ActionContext, ctx)

	claimed, err := s.Claim(plan.WorkID, "codex", fixedNow.Add(8*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != "claimed" {
		t.Fatalf("work not claimed: %+v", claimed)
	}
	wb, err = s.Workbench(fixedNow.Add(9*time.Minute), WorkbenchOptions{RecentLimit: 5, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	requireWorkActionContext(t, "claimed workbench", findWorkbenchItem(t, wb.Open, plan.WorkID).ActionContext, ctx)

	running, err := s.Advance(plan.WorkID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if running.State != "running" {
		t.Fatalf("work not running: %+v", running)
	}
	wb, err = s.Workbench(fixedNow.Add(11*time.Minute), WorkbenchOptions{RecentLimit: 5, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	requireWorkActionContext(t, "running workbench", findWorkbenchItem(t, wb.Open, plan.WorkID).ActionContext, ctx)

	trace := writeTrace(t, s, "trace_product_action_context", plan.WorkID)
	observed, err := s.ObserveRun(plan.WorkID, ObserveRunInput{Actor: "codex", TraceID: trace.ID}, fixedNow.Add(12*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if observed.State != "verifying" {
		t.Fatalf("observe-run did not move verifying: %+v", observed)
	}
	requireWorkActionContext(t, "observe-run", observed.ActionContext, ctx)
	wb, err = s.Workbench(fixedNow.Add(13*time.Minute), WorkbenchOptions{RecentLimit: 5, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	requireWorkActionContext(t, "verifying workbench", findWorkbenchItem(t, wb.Open, plan.WorkID).ActionContext, ctx)
}

func requireWorkActionContext(t *testing.T, label string, got, want *WorkActionContext) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s missing action context", label)
	}
	if got.Kind != want.Kind ||
		got.SourceRef != want.SourceRef ||
		got.SourceID != want.SourceID ||
		got.Title != want.Title ||
		got.Boundary != want.Boundary ||
		got.Proof != want.Proof ||
		!sameStringSlice(got.Command, want.Command) ||
		!sameStringSlice(got.RequiredInputs, want.RequiredInputs) ||
		!sameStringSlice(got.ExpectedValidation, want.ExpectedValidation) ||
		!sameStringSlice(got.StopConditions, want.StopConditions) ||
		!sameStringSlice(got.EvidenceRefs, want.EvidenceRefs) {
		t.Fatalf("%s action context mismatch:\ngot:  %+v\nwant: %+v", label, got, want)
	}
}

// TestCreateRejectsMissingReworkOf pins the fail-closed half of the rework lineage edge: a
// --rework-of pointing at a work id that does not resolve in THIS store refuses the create before
// anything persists. A phantom edge is the failure mode that matters -- a rework-rate fold that
// counts edges to works that never existed reports a number nobody can trace back, and unlike a
// missing edge it looks like data. Mirrors TestCreateRejectsMissingContextPack (same admission
// slot, same "nothing persisted" assertion) and runattempt.SetPredecessor's "no phantom links".
//
// Falsifier: removing the validateReworkOfRef call from admitWorkLocked turns this RED -- the
// create succeeds and List() returns one work.
func TestCreateRejectsMissingReworkOf(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create refuses a phantom rework edge",
		ReworkOfWorkID:   "work_missing123",
	}, fixedNow)
	if err == nil || !strings.Contains(err.Error(), `rework_of_work_id "work_missing123"`) {
		t.Fatalf("expected missing rework target error, got %v", err)
	}
	works, listErr := s.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(works) != 0 {
		t.Fatalf("missing rework target create must not persist work: %+v", works)
	}
}

// TestCreateAcceptsExistingReworkOfAndPersistsIt is the positive half: an edge to a work that DOES
// resolve is admitted, persisted, and readable back off disk -- which is what makes the edge
// foldable later. Re-loading through Get (rather than trusting the returned value) is the load-
// bearing part: it proves the field survived the marshal/write/read round-trip, not just the
// in-memory struct copy.
//
// Falsifier: dropping the ReworkOfWorkID copy in BuildWork turns this RED on the reloaded value.
func TestCreateAcceptsExistingReworkOfAndPersistsIt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	earlier, err := s.Create(CreateInput{
		WorkID:           "work_earlier000",
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "the earlier work whose breakage is being reworked",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create accepts a resolvable rework edge",
		ReworkOfWorkID:   earlier.ID,
	}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("create with a resolvable rework target must succeed: %v", err)
	}
	if created.ReworkOfWorkID != earlier.ID {
		t.Fatalf("created rework_of_work_id = %q, want %q", created.ReworkOfWorkID, earlier.ID)
	}
	reloaded, err := s.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ReworkOfWorkID != earlier.ID {
		t.Fatalf("persisted rework_of_work_id = %q, want %q", reloaded.ReworkOfWorkID, earlier.ID)
	}
	// The target being a plain work (not itself a rework) is the common case; an edge to a work
	// that is ITSELF a rework is explicitly fine -- no transitive validation, no cycle walk.
	chained, err := s.Create(CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "rework of a rework is admitted",
		ReworkOfWorkID:   created.ID,
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("rework of a rework must be admitted: %v", err)
	}
	if chained.ReworkOfWorkID != created.ID {
		t.Fatalf("chained rework_of_work_id = %q, want %q", chained.ReworkOfWorkID, created.ID)
	}
}

// TestCreateRejectsSelfReworkOf pins the one shape the existence check alone cannot catch: with an
// explicit --work-id, a caller can point the edge at the id being created. The target does not
// exist yet, so the store lookup would refuse it for the WRONG reason today and would silently
// ACCEPT it the moment an id were reused -- the envelope-level self check is what makes the
// refusal about self-reference rather than about ordering.
//
// Falsifier: removing the self comparison from validateReworkOfWorkID turns this RED with a
// missing-target error instead of a self-reference error.
func TestCreateRejectsSelfReworkOf(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.Create(CreateInput{
		WorkID:           "work_selfref000",
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "create refuses a self-referential rework edge",
		ReworkOfWorkID:   "work_selfref000",
	}, fixedNow)
	if err == nil {
		t.Fatal("self-referential rework edge must be refused")
	}
	if !strings.Contains(err.Error(), "rework_of_work_id") || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("self-reference refusal must name the field and the reason, got: %v", err)
	}
	works, listErr := s.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(works) != 0 {
		t.Fatalf("self-referential rework create must not persist work: %+v", works)
	}
}
