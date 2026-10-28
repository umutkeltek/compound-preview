package panel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
	workpkg "compound/internal/work"
)

func TestBuildPlanBindsExistingWorkAndTaskContract(t *testing.T) {
	home, task, parent, run := buildBoundPanelPlan(t)
	if run.WorkID != parent.ID || !run.WorkBound {
		t.Fatalf("panel work binding = id %q bound=%t, want %q/true", run.WorkID, run.WorkBound, parent.ID)
	}
	if !reflect.DeepEqual(run.TaskContract, task) {
		t.Fatalf("panel task_contract = %+v, want %+v", run.TaskContract, task)
	}
	loaded, err := mustPanelStore(t, home).LoadRun(run.ID)
	if err != nil {
		t.Fatalf("load bound run: %v", err)
	}
	if loaded.WorkID != parent.ID || !reflect.DeepEqual(loaded.TaskContract, task) {
		t.Fatalf("saved panel binding = work %q task %+v", loaded.WorkID, loaded.TaskContract)
	}
}

func TestBoundPanelRunIDIncludesWorkIdentity(t *testing.T) {
	home, _, parent, first := buildBoundPanelPlan(t)
	workStore, err := workpkg.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	secondParent, err := workStore.Create(workpkg.CreateInput{
		Owner:            "second-task-owner",
		Actor:            "second-task-owner",
		DesiredOutcome:   parent.DesiredOutcome,
		SuccessPredicate: parent.SuccessPredicate + " for second task",
		NonGoals:         parent.NonGoals,
		ContextPackID:    parent.IDs.ContextPackID,
	}, panelTestNow)
	if err != nil {
		t.Fatalf("create second parent work: %v", err)
	}
	second, err := BuildPlan(home, PlanInput{
		WorkID:                secondParent.ID,
		TargetKind:            "memory_candidate",
		TargetRefs:            []string{"mcand_example_api_clean"},
		States:                []string{"parked"},
		PrivacyOverrideReason: "synthetic fixtures",
		Budget:                Budget{MaxItems: 1},
	}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build second bound plan: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("different work ids collided on panel run id %q", first.ID)
	}
}

func TestBoundPanelRunIDIncludesTaskContract(t *testing.T) {
	home, _, parent, first := buildBoundPanelPlan(t)
	packPath := filepath.Join(home, "context", "packs", parent.IDs.ContextPackID+".json")
	var pack envelope.ContextPack
	readJSONForTest(t, packPath, &pack)
	pack.TaskContract.DesiredOutcome = "a materially different desired outcome"
	data, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := BuildPlan(home, PlanInput{
		WorkID:                parent.ID,
		TargetKind:            "memory_candidate",
		TargetRefs:            []string{"mcand_example_api_clean"},
		States:                []string{"parked"},
		PrivacyOverrideReason: "synthetic fixtures",
		Budget:                Budget{MaxItems: 1},
	}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build changed-contract plan: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("different bound task contracts collided on panel run id %q", first.ID)
	}
}

func TestBoundPanelPlanRejectsTaskOverride(t *testing.T) {
	home, _, parent, _ := buildBoundPanelPlan(t)
	_, err := BuildPlan(home, PlanInput{
		WorkID:                parent.ID,
		TargetKind:            "memory_candidate",
		TargetRefs:            []string{"mcand_example_api_clean"},
		States:                []string{"parked"},
		Task:                  "a different task that would select different lenses",
		PrivacyOverrideReason: "synthetic fixtures",
		Budget:                Budget{MaxItems: 1},
	}, fixedPanelNow)
	if err == nil {
		t.Fatal("bound panel accepted a free-form task override")
	}
}

func TestBoundPanelRunKeepsOneWorkAndApplyFailsWithoutOperationLease(t *testing.T) {
	home, _, parent, run := buildBoundPanelPlan(t)
	workStore, err := workpkg.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	before, err := workStore.List()
	if err != nil {
		t.Fatal(err)
	}

	run, err = ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("execute bound panel: %v", err)
	}
	if run.Status != "verdicts_recorded" {
		t.Fatalf("bound panel status = %q", run.Status)
	}
	for _, finding := range run.Findings {
		for _, lens := range finding.LensFindings {
			assertPanelRouteWorkID(t, home, lens.RouteEvidenceRef, parent.ID)
		}
		assertPanelRouteWorkID(t, home, finding.Synthesis.JudgeRouteEvidenceRef, parent.ID)
	}

	notApplied, err := Apply(home, run, fixedPanelNow)
	if err == nil || !strings.Contains(err.Error(), "operation lease") {
		t.Fatalf("bound apply error = %v, want operation lease requirement", err)
	}
	after, err := workStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("bound panel created another WorkEnvelope: before=%d after=%d", len(before), len(after))
	}
	if notApplied.Status != "verdicts_recorded" || notApplied.WorkID != parent.ID || notApplied.ReceiptID != "" || notApplied.TransitionID != "" {
		t.Fatalf("rejected bound apply changed run = status %q work %q receipt %q transition %q", notApplied.Status, notApplied.WorkID, notApplied.ReceiptID, notApplied.TransitionID)
	}
	parentAfter, err := workStore.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parentAfter.State != parent.State || !reflect.DeepEqual(parentAfter.ActionIntentIDs, parent.ActionIntentIDs) || parentAfter.PolicyDecisionID != parent.PolicyDecisionID {
		t.Fatalf("rejected bound apply mutated parent: before=%+v after=%+v", parent, parentAfter)
	}
	for _, finding := range notApplied.Findings {
		if finding.Application.ActionIntentRef != "" || finding.Application.PolicyDecisionRef != "" {
			t.Fatalf("rejected bound apply wrote action refs: %+v", finding.Application)
		}
	}
}

func TestBoundPanelCallsConsumeFrozenTaskContract(t *testing.T) {
	home, task, _, run := buildBoundPanelPlan(t)
	caller := &recordingPanelCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err != nil {
		t.Fatalf("execute bound panel: %v", err)
	}
	if len(caller.requests) == 0 {
		t.Fatal("panel made no model calls")
	}
	for _, request := range caller.requests {
		for _, want := range []string{task.Intent, task.DesiredOutcome, task.SuccessPredicate, task.NonGoals[0], task.ProofMedium} {
			if !strings.Contains(request.User, want) {
				t.Fatalf("%s prompt missing task-contract value %q:\n%s", request.LensID, want, request.User)
			}
		}
	}
}

func TestBoundPanelRejectsCallerWithoutOperationLease(t *testing.T) {
	home, _, _, run := buildBoundPanelPlan(t)
	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil || !strings.Contains(err.Error(), "operation lease") {
		t.Fatalf("unleased bound caller error = %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("unleased bound caller made %d calls", caller.calls)
	}
}

type recordingPanelCaller struct {
	delegate LensCaller
	requests []CallRequest
}

func (c *recordingPanelCaller) Call(ctx context.Context, request CallRequest) (CallResult, error) {
	c.requests = append(c.requests, request)
	return c.delegate.Call(ctx, request)
}

func (*recordingPanelCaller) allowsBoundWorkWithoutOperationLease() bool { return true }

func TestBoundPanelRefusesTerminalWorkBeforeProviderCall(t *testing.T) {
	home, _, parent, run := buildBoundPanelPlan(t)
	workStore, err := workpkg.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Close(parent.ID, workpkg.CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "task-owner",
		Summary:  "terminalize before panel execution",
	}, fixedPanelNow()); err != nil {
		t.Fatalf("close parent work: %v", err)
	}
	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil {
		t.Fatal("terminal parent work should block panel execution")
	}
	if caller.calls != 0 {
		t.Fatalf("terminal parent allowed %d provider calls", caller.calls)
	}
}

func TestAppliedBoundPanelRemainsReadableAfterParentCloses(t *testing.T) {
	home, _, parent, run := buildBoundPanelPlan(t)
	run.Status = "applied"
	if err := mustPanelStore(t, home).SaveRun(run); err != nil {
		t.Fatal(err)
	}
	workStore, err := workpkg.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Close(parent.ID, workpkg.CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "task-owner",
		Summary:  "parent closed after panel result",
	}, fixedPanelNow()); err != nil {
		t.Fatalf("close parent work: %v", err)
	}
	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	got, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow)
	if err != nil {
		t.Fatalf("read terminal bound panel: %v", err)
	}
	if got.Status != "applied" || caller.calls != 0 {
		t.Fatalf("terminal bound panel = status %q calls %d", got.Status, caller.calls)
	}
}

func TestBoundPanelRechecksWorkAfterPreflightBeforeProviderCall(t *testing.T) {
	home, _, parent, run := buildBoundPanelPlan(t)
	workStore, err := workpkg.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	caller := &terminalizingPanelCaller{
		delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")},
		store:    workStore,
		workID:   parent.ID,
	}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil {
		t.Fatal("work terminalized during preflight should block provider call")
	}
	if caller.calls != 0 {
		t.Fatalf("terminalized work allowed %d provider calls", caller.calls)
	}
}

func TestBoundPanelRejectsTaskContractDriftBeforeProviderCall(t *testing.T) {
	home, _, parent, run := buildBoundPanelPlan(t)
	packPath := filepath.Join(home, "context", "packs", parent.IDs.ContextPackID+".json")
	var pack envelope.ContextPack
	readJSONForTest(t, packPath, &pack)
	pack.TaskContract.Intent = "mutated task meaning"
	data, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	caller := &countingLensCaller{delegate: FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}}
	if _, err := ExecuteDryRun(context.Background(), home, run, caller, fixedPanelNow); err == nil {
		t.Fatal("task contract drift should block panel execution")
	}
	if caller.calls != 0 {
		t.Fatalf("task contract drift allowed %d provider calls", caller.calls)
	}
}

type terminalizingPanelCaller struct {
	delegate LensCaller
	store    *workpkg.Store
	workID   string
	calls    int
}

func (c *terminalizingPanelCaller) Preflight(context.Context, Run) error {
	_, err := c.store.Close(c.workID, workpkg.CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "task-owner",
		Summary:  "terminalized during panel preflight",
	}, panelTestNow)
	return err
}

func (c *terminalizingPanelCaller) Call(ctx context.Context, request CallRequest) (CallResult, error) {
	c.calls++
	return c.delegate.Call(ctx, request)
}

func (*terminalizingPanelCaller) allowsBoundWorkWithoutOperationLease() bool { return true }

func assertPanelRouteWorkID(t *testing.T, home, routeID, workID string) {
	t.Helper()
	if routeID == "" {
		t.Fatal("missing panel route evidence ref")
	}
	evidence, err := routeevidence.LoadEvidenceByID(home, routeID)
	if err != nil {
		t.Fatalf("load route evidence %s: %v", routeID, err)
	}
	if evidence.WorkID != workID {
		t.Fatalf("route evidence %s work_id = %q, want %q", routeID, evidence.WorkID, workID)
	}
}

func buildBoundPanelPlan(t *testing.T) (string, envelope.TaskContract, envelope.WorkEnvelope, Run) {
	t.Helper()
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	task := envelope.TaskContract{
		Intent:           "review task-scoped capability evidence",
		Mode:             "review",
		DesiredOutcome:   "Panel evidence remains attached to the originating task.",
		SuccessPredicate: "Every panel route and action uses one work id.",
		NonGoals:         []string{"close the parent task from the panel"},
		ProofMedium:      "cli",
	}
	pack := envelope.ContextPack{
		Header:             panelTaskHeader(envelope.SVContextPack, "ctx_panel_task_contract"),
		SummaryMD:          "typed panel fixture",
		TaskContract:       task,
		Included:           []envelope.IncludedRef{},
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       []string{},
		SourceRevisions:    []envelope.SourceRev{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	packPath := filepath.Join(home, "context", "packs", pack.ID+".json")
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	workStore, err := workpkg.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := workStore.Create(workpkg.CreateInput{
		Owner:            "task-owner",
		Actor:            "task-owner",
		DesiredOutcome:   task.DesiredOutcome,
		SuccessPredicate: task.SuccessPredicate,
		NonGoals:         task.NonGoals,
		ContextPackID:    pack.ID,
	}, panelTestNow)
	if err != nil {
		t.Fatalf("create parent work: %v", err)
	}

	run, err := BuildPlan(home, PlanInput{
		WorkID:                parent.ID,
		TargetKind:            "memory_candidate",
		TargetRefs:            []string{"mcand_example_api_clean"},
		States:                []string{"parked"},
		PrivacyOverrideReason: "synthetic fixtures",
		Budget:                Budget{MaxItems: 1},
	}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build bound panel plan: %v", err)
	}
	return home, task, parent, run
}

func panelTaskHeader(schemaVersion, id string) envelope.Header {
	return envelope.Header{
		SchemaVersion: schemaVersion,
		ID:            id,
		CreatedAt:     panelTestNow.Format("2006-01-02T15:04:05Z07:00"),
		Producer:      envelope.Producer{Kind: "manual", Name: "panel-test"},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       "private",
		Risk:          "tier0",
		EvidenceRefs:  []string{},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}

func mustPanelStore(t *testing.T, home string) *Store {
	t.Helper()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
