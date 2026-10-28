package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

func TestEnforcedBaselineHoldPersistsOneNeutralApprovalPlanRef(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	deps := baseDeps()
	deps.HomePath = homePath
	action := approvalPlanHoldAction("act_hook_plan", "work_hook_plan")
	res, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Enforced: true,
		WorkID: action.IDs.WorkID, Action: &action,
	}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	plans := hookApprovalPlanEntries(t, homePath)
	if len(plans) != 1 {
		t.Fatalf("approval plans=%d, want one", len(plans))
	}
	planRef := plans[0].Reference.String()
	pd := res.Produced[1].(*envelope.PolicyDecision)
	if got := hookRefCount(pd.EvidenceRefs, planRef); got != 1 {
		t.Fatalf("PolicyDecision evidence count=%d refs=%v, want %s exactly once", got, pd.EvidenceRefs, planRef)
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(pd.ID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted envelope.PolicyDecision
	if err := json.Unmarshal(record.Bytes, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := hookRefCount(persisted.EvidenceRefs, planRef); got != 1 {
		t.Fatalf("persisted PolicyDecision evidence count=%d refs=%v, want %s exactly once", got, persisted.EvidenceRefs, planRef)
	}
	if got := hookRefCount(res.Event.SideEffects, planRef); got != 1 {
		t.Fatalf("HookEvent side-effect count=%d refs=%v, want %s exactly once", got, res.Event.SideEffects, planRef)
	}
	wantReason := "Exact approval plan " + plans[0].ID + " is ready for operator review."
	if !hookContainsRef(pd.Reasons, wantReason) {
		t.Fatalf("PolicyDecision reasons=%v, want %q", pd.Reasons, wantReason)
	}
}

func TestEnforcedBaselineHoldRejectsMatchingPlanlessApprovalAuthority(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	deps := baseDeps()
	deps.HomePath = homePath
	action := approvalPlanHoldAction("act_hook_planless", "work_hook_planless")
	approval, err := policy.NewExactActionApproval(
		"approval_grant_raw_planless_marker",
		"operator:raw_planless_marker",
		policy.ActionBindingForIntent(action, deps.Policy),
		fixedNow.Add(5*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(homePath)
	if _, err := approvals.Save(approval); err != nil {
		t.Fatal(err)
	}

	res, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Enforced: true,
		WorkID: action.IDs.WorkID, Action: &action,
	}, deps, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pd := res.Produced[1].(*envelope.PolicyDecision)
	if pd.Outcome != "hold" || !hookContainsRef(pd.Reasons, "exact approval authority unavailable for operator review") {
		t.Fatalf("planless authority decision=%+v, want bounded hold", pd)
	}
	if joined := strings.Join(pd.Reasons, " "); strings.Contains(joined, "raw_planless_marker") || strings.Contains(joined, approval.ID) {
		t.Fatalf("planless authority leaked raw detail into PolicyDecision: %v", pd.Reasons)
	}
	assertNoHookApprovalPlans(t, homePath)
	status, err := approvals.Check(approval.ID, fixedNow.Add(2*time.Second))
	if err != nil || status.Status != policy.ExactApprovalStatusReady {
		t.Fatalf("planless authority status=%+v err=%v, want ready and unconsumed", status, err)
	}
}

func TestApprovalPlanExcludesObserveBatchDenySecurityPlacementAndDirtySourceControl(t *testing.T) {
	t.Run("observe-only", func(t *testing.T) {
		homePath := initHookApprovalHome(t)
		deps := baseDeps()
		deps.HomePath = homePath
		action := approvalPlanHoldAction("act_observe_no_plan", "work_observe_no_plan")
		res, err := Run(Fixture{Hook: "pre-tool-use", Harness: "codex", Action: &action}, deps, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		assertNoHookApprovalPlans(t, homePath)
		pd := res.Produced[1].(*envelope.PolicyDecision)
		if got := hookApprovalPlanRefCount(pd.EvidenceRefs); got != 0 {
			t.Fatalf("observe-only returned PolicyDecision plan refs=%d: %v", got, pd.EvidenceRefs)
		}
		store, err := eventstore.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.Read(pd.ID)
		if err != nil {
			t.Fatal(err)
		}
		var persisted envelope.PolicyDecision
		if err := json.Unmarshal(record.Bytes, &persisted); err != nil {
			t.Fatal(err)
		}
		if got := hookApprovalPlanRefCount(persisted.EvidenceRefs); got != 0 {
			t.Fatalf("observe-only persisted PolicyDecision plan refs=%d: %v", got, persisted.EvidenceRefs)
		}
		if got := hookApprovalPlanRefCount(res.Event.SideEffects); got != 0 {
			t.Fatalf("observe-only HookEvent plan side effects=%d: %v", got, res.Event.SideEffects)
		}
	})

	t.Run("batch", func(t *testing.T) {
		homePath := initHookApprovalHome(t)
		deps := baseDeps()
		deps.HomePath = homePath
		first := approvalPlanHoldAction("act_batch_no_plan_a", "work_batch_no_plan")
		second := approvalPlanHoldAction("act_batch_no_plan_b", "work_batch_no_plan")
		if _, err := Run(Fixture{
			Hook: "pre-tool-use", Harness: "codex", Enforced: true,
			Actions: []envelope.ActionIntent{first, second},
		}, deps, fixedNow); err != nil {
			t.Fatal(err)
		}
		assertNoHookApprovalPlans(t, homePath)
	})

	t.Run("deny", func(t *testing.T) {
		homePath := initHookApprovalHome(t)
		deps := baseDeps()
		deps.HomePath = homePath
		action := approvalPlanHoldAction("act_deny_no_plan", "work_deny_no_plan")
		action.Target.Path = filepath.Join(t.TempDir(), ".env")
		res, err := Run(Fixture{Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &action}, deps, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Produced[1].(*envelope.PolicyDecision).Outcome; got != "deny" {
			t.Fatalf("policy outcome=%q, want deny", got)
		}
		assertNoHookApprovalPlans(t, homePath)
	})

	t.Run("security-floor", func(t *testing.T) {
		homePath := initHookApprovalHome(t)
		deps := baseDeps()
		deps.HomePath = homePath
		action := approvalPlanHoldAction("act_floor_no_plan", "work_floor_no_plan")
		action.SecurityFloorOutcome = "deny"
		action.SecurityReasons = []string{"synthetic security-floor falsifier"}
		if _, err := Run(Fixture{Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &action}, deps, fixedNow); err != nil {
			t.Fatal(err)
		}
		assertNoHookApprovalPlans(t, homePath)
	})

	t.Run("placement", func(t *testing.T) {
		homePath := initHookApprovalHome(t)
		deps := baseDeps()
		deps.HomePath = homePath
		action := approvalPlanLocalWriteAction("act_placement_no_plan", "work_placement_no_plan", filepath.Join(t.TempDir(), "README.md"))
		res, err := Run(Fixture{
			Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &action,
			Placement: &policy.PlacementGuardContext{Mode: "block", Confidence: "block", Reason: "synthetic wrong owner"},
		}, deps, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Produced[1].(*envelope.PolicyDecision).Outcome; got != "hold" {
			t.Fatalf("placement policy outcome=%q, want hold", got)
		}
		assertNoHookApprovalPlans(t, homePath)
	})

	t.Run("dirty-source-control", func(t *testing.T) {
		repo := initHookGitRepo(t)
		homePath := filepath.Join(repo, ".compound")
		if err := home.Init(homePath, "test", fixedNow); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(repo, "README.md")
		if err := os.WriteFile(target, []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		deps := baseDeps()
		deps.HomePath = homePath
		action := approvalPlanLocalWriteAction("act_dirty_no_plan", "work_dirty_no_plan", target)
		res, err := Run(Fixture{Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &action}, deps, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Produced[1].(*envelope.PolicyDecision).Outcome; got != "hold" {
			t.Fatalf("dirty-source-control outcome=%q reasons=%v, want hold", got, res.Produced[1].(*envelope.PolicyDecision).Reasons)
		}
		assertNoHookApprovalPlans(t, homePath)
	})

	t.Run("coordination-execution-hold", func(t *testing.T) {
		testCase := newHookInterruptCase(t, "approval-plan-exclusion")
		action := approvalPlanHoldAction("act_coordination_no_plan", testCase.fixture.WorkID)
		testCase.fixture.Action = &action
		testCase.fixture.Enforced = true
		deps := baseDeps()
		deps.HomePath = testCase.homePath
		deps.Store = testCase.store
		res, err := Run(testCase.fixture, deps, testCase.now.Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if res.CoordinationDelivery == nil || !res.CoordinationDelivery.ExecutionHeld {
			t.Fatalf("coordination execution hold missing: %+v", res.CoordinationDelivery)
		}
		assertNoHookApprovalPlans(t, testCase.homePath)
	})
}

func TestNonHoldingCoordinationDeliveryRemainsExactApprovalEligible(t *testing.T) {
	testCase := newHookInterruptCase(t, "approval-plan-nonholding")
	deps := baseDeps()
	deps.HomePath = testCase.homePath
	deps.Store = testCase.store
	first, err := Run(testCase.fixture, deps, testCase.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if first.CoordinationDelivery == nil || !first.CoordinationDelivery.ExecutionHeld {
		t.Fatalf("first coordination delivery=%+v, want active interrupt hold", first.CoordinationDelivery)
	}
	attemptID := testCase.fixture.CoordinationDelivery.Recipient.RunAttemptID
	if _, err := runattempt.Open(testCase.homePath).Complete(
		attemptID,
		runattempt.StatusSucceeded,
		testCase.now.Add(4*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	action := approvalPlanHoldAction("act_coordination_plan_eligible", testCase.fixture.WorkID)
	testCase.fixture.Action = &action
	testCase.fixture.Enforced = true
	res, err := Run(testCase.fixture, deps, testCase.now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if res.CoordinationDelivery == nil || res.CoordinationDelivery.ExecutionHeld {
		t.Fatalf("coordination delivery=%+v, want computed non-holding report", res.CoordinationDelivery)
	}
	plans := hookApprovalPlanEntries(t, testCase.homePath)
	if len(plans) != 1 {
		t.Fatalf("non-holding coordination approval plans=%d, want one", len(plans))
	}
	planRef := plans[0].Reference.String()
	pd := res.Produced[1].(*envelope.PolicyDecision)
	if got := hookRefCount(pd.EvidenceRefs, planRef); got != 1 {
		t.Fatalf("non-holding coordination PolicyDecision plan refs=%d refs=%v, want %s once", got, pd.EvidenceRefs, planRef)
	}
	if got := hookRefCount(res.Event.SideEffects, planRef); got != 1 {
		t.Fatalf("non-holding coordination HookEvent plan refs=%d refs=%v, want %s once", got, res.Event.SideEffects, planRef)
	}
}

func TestMalformedApprovalAuthorityFailsClosedWithoutMintingPlan(t *testing.T) {
	homePath := initHookApprovalHome(t)
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON("approval_grant_malformed", map[string]any{
		"schema_version": "malformed",
		"id":             "approval_grant_malformed",
	}, "  "); err != nil {
		t.Fatal(err)
	}
	deps := baseDeps()
	deps.HomePath = homePath
	action := approvalPlanHoldAction("act_malformed_approval_authority", "work_malformed_approval_authority")
	res, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &action,
	}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	assertNoHookApprovalPlans(t, homePath)
	pd := res.Produced[1].(*envelope.PolicyDecision)
	if pd.Outcome != "hold" {
		t.Fatalf("malformed approval authority outcome=%q, want hold", pd.Outcome)
	}
	joined := strings.Join(pd.Reasons, "; ")
	if strings.Contains(joined, "approval_grant_malformed") || strings.Contains(joined, "schema_version") {
		t.Fatalf("malformed approval authority leaked raw store error: %q", joined)
	}
}

func TestFixtureJSONCannotForgeEnforcedPosture(t *testing.T) {
	var fixture Fixture
	if err := json.Unmarshal([]byte(`{"hook":"pre-tool-use","harness":"codex","enforced":true}`), &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Enforced {
		t.Fatal("fixture JSON forged trusted enforced posture")
	}
}

func TestApprovedPlanCannotCrossPlacementOrDirtySourceControlDrift(t *testing.T) {
	t.Run("placement", func(t *testing.T) {
		homePath := initHookApprovalHome(t)
		deps := baseDeps()
		deps.HomePath = homePath
		action := approvalPlanHoldAction("act_placement_grant_clean", "work_placement_grant_clean")
		if _, err := Run(Fixture{Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &action}, deps, fixedNow); err != nil {
			t.Fatal(err)
		}
		plans := hookApprovalPlanEntries(t, homePath)
		if len(plans) != 1 {
			t.Fatalf("clean approval plans=%d, want one", len(plans))
		}
		approvals := policy.NewExactActionApprovalStore(homePath)
		approval, _, err := approvals.Approve(plans[0].ID, "operator:fixture", deps.Policy, fixedNow.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}

		retry := action
		retry.ID = "act_placement_grant_drifted"
		res, err := Run(Fixture{
			Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &retry,
			Placement: &policy.PlacementGuardContext{Mode: "block", Confidence: "block", Reason: "synthetic owner drift"},
		}, deps, fixedNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if pd := res.Produced[1].(*envelope.PolicyDecision); pd.Outcome != "hold" {
			t.Fatalf("approved action crossed placement drift: %+v", pd)
		}
		if status, err := approvals.Check(approval.ID, fixedNow.Add(3*time.Second)); err != nil || status.Status != policy.ExactApprovalStatusReady {
			t.Fatalf("placement drift consumed grant: status=%+v err=%v", status, err)
		}
		if got := len(hookApprovalPlanEntries(t, homePath)); got != 1 {
			t.Fatalf("placement drift created plan debt: plans=%d, want one", got)
		}
	})

	t.Run("dirty-source-control", func(t *testing.T) {
		repo := initHookGitRepo(t)
		homePath := filepath.Join(repo, ".compound")
		if err := home.Init(homePath, "test", fixedNow); err != nil {
			t.Fatal(err)
		}
		deps := baseDeps()
		deps.HomePath = homePath
		target := filepath.Join(repo, "README.md")
		action := approvalPlanLocalWriteAction("act_dirty_grant_clean", "work_dirty_grant_clean", target)
		events, err := eventstore.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := events.PutJSON(action.ID, action, "  "); err != nil {
			t.Fatal(err)
		}
		approvals := policy.NewExactActionApprovalStore(homePath)
		plan, _, _, err := approvals.EnsurePlanForIntent(action, deps.Policy, 5*time.Minute, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		approval, _, err := approvals.Approve(plan.ID, "operator:fixture", deps.Policy, fixedNow.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		retry := action
		retry.ID = "act_dirty_grant_drifted"
		res, err := Run(Fixture{Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &retry}, deps, fixedNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if pd := res.Produced[1].(*envelope.PolicyDecision); pd.Outcome != "hold" {
			t.Fatalf("approved action crossed dirty source-control drift: %+v", pd)
		}
		if status, err := approvals.Check(approval.ID, fixedNow.Add(3*time.Second)); err != nil || status.Status != policy.ExactApprovalStatusReady {
			t.Fatalf("dirty source-control drift consumed grant: status=%+v err=%v", status, err)
		}
		if got := len(hookApprovalPlanEntries(t, homePath)); got != 1 {
			t.Fatalf("dirty source-control drift created plan debt: plans=%d, want one", got)
		}
	})
}

func TestExcludedExactApprovalPathsPreserveReadyGrantAndPlanGeneration(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		homePath := initHookApprovalHome(t)
		deps := baseDeps()
		deps.HomePath = homePath
		action := approvalPlanHoldAction("act_batch_ready_seed", "work_batch_ready_seed")
		plan, approval := seedReadyHookApproval(t, homePath, deps, action, fixedNow)
		retry := action
		retry.ID = "act_batch_ready_retry"
		if _, err := Run(Fixture{
			Hook: "pre-tool-use", Harness: "codex", Enforced: true,
			Actions: []envelope.ActionIntent{retry},
		}, deps, fixedNow.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		assertHookApprovalGenerationUnchanged(t, homePath, approval.ID, plan.ID, fixedNow.Add(3*time.Second))
	})

	t.Run("contextual-deny", func(t *testing.T) {
		homePath := initHookApprovalHome(t)
		deps := baseDeps()
		deps.HomePath = homePath
		action := approvalPlanHoldAction("act_contextual_deny_seed", "work_contextual_deny_seed")
		plan, approval := seedReadyHookApproval(t, homePath, deps, action, fixedNow)
		retry := action
		retry.ID = "act_contextual_deny_retry"
		retry.Target.Path = filepath.Join(t.TempDir(), ".env")
		res, err := Run(Fixture{
			Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &retry,
		}, deps, fixedNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Produced[1].(*envelope.PolicyDecision).Outcome; got != "deny" {
			t.Fatalf("contextual deny outcome=%q, want deny", got)
		}
		assertHookApprovalGenerationUnchanged(t, homePath, approval.ID, plan.ID, fixedNow.Add(3*time.Second))
	})

	for _, floor := range []string{"hold", "deny"} {
		t.Run("security-floor-"+floor, func(t *testing.T) {
			homePath := initHookApprovalHome(t)
			deps := baseDeps()
			deps.HomePath = homePath
			action := approvalPlanHoldAction("act_security_floor_"+floor+"_seed", "work_security_floor_"+floor)
			plan, approval := seedReadyHookApproval(t, homePath, deps, action, fixedNow)
			retry := action
			retry.ID = "act_security_floor_" + floor + "_retry"
			retry.SecurityFloorOutcome = floor
			retry.SecurityReasons = []string{"synthetic security-floor " + floor}
			res, err := Run(Fixture{
				Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &retry,
			}, deps, fixedNow.Add(2*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Produced[1].(*envelope.PolicyDecision).Outcome; got != floor {
				t.Fatalf("security-floor outcome=%q, want %q", got, floor)
			}
			assertHookApprovalGenerationUnchanged(t, homePath, approval.ID, plan.ID, fixedNow.Add(3*time.Second))
		})
	}

	t.Run("coordination-execution-hold", func(t *testing.T) {
		testCase := newHookInterruptCase(t, "ready-grant-exclusion")
		deps := baseDeps()
		deps.HomePath = testCase.homePath
		deps.Store = testCase.store
		action := approvalPlanHoldAction("act_coordination_ready_seed", testCase.fixture.WorkID)
		plan, approval := seedReadyHookApproval(t, testCase.homePath, deps, action, testCase.now)
		retry := action
		retry.ID = "act_coordination_ready_retry"
		testCase.fixture.Action = &retry
		testCase.fixture.Enforced = true
		res, err := Run(testCase.fixture, deps, testCase.now.Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if res.CoordinationDelivery == nil || !res.CoordinationDelivery.ExecutionHeld {
			t.Fatalf("coordination execution hold missing: %+v", res.CoordinationDelivery)
		}
		assertHookApprovalGenerationUnchanged(
			t, testCase.homePath, approval.ID, plan.ID, testCase.now.Add(4*time.Second),
		)
	})
}

func approvalPlanHoldAction(id, workID string) envelope.ActionIntent {
	monitored := policy.MonitorToolAction(policy.ToolActionRequest{
		WorkID:             workID,
		Actor:              "codex:fixture",
		Harness:            "codex",
		Tool:               "browser.submit",
		Arguments:          map[string]any{"target": "publish", "form": "prepared-draft"},
		ToolSchemaRevision: "builtin:browser.submit",
	}, policy.DefaultConfig(), fixedNow)
	action := monitored.Intent
	action.ID = id
	return action
}

func seedReadyHookApproval(
	t *testing.T,
	homePath string,
	deps Deps,
	action envelope.ActionIntent,
	now time.Time,
) (policy.ExactActionApprovalPlan, policy.ExactActionApproval) {
	t.Helper()
	if _, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Enforced: true, Action: &action,
	}, deps, now); err != nil {
		t.Fatal(err)
	}
	plans := hookApprovalPlanEntries(t, homePath)
	if len(plans) != 1 {
		t.Fatalf("approval seed plans=%d, want one", len(plans))
	}
	record, err := plans[0].Read()
	if err != nil {
		t.Fatal(err)
	}
	var plan policy.ExactActionApprovalPlan
	if err := json.Unmarshal(record.Bytes, &plan); err != nil {
		t.Fatal(err)
	}
	approval, _, err := policy.NewExactActionApprovalStore(homePath).
		Approve(plan.ID, "operator:fixture", deps.Policy, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return plan, approval
}

func assertHookApprovalGenerationUnchanged(
	t *testing.T,
	homePath, approvalID, planID string,
	now time.Time,
) {
	t.Helper()
	status, err := policy.NewExactActionApprovalStore(homePath).Check(approvalID, now)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != policy.ExactApprovalStatusReady {
		t.Fatalf("excluded path changed grant %s status=%q, want ready", approvalID, status.Status)
	}
	plans := hookApprovalPlanEntries(t, homePath)
	if len(plans) != 1 || plans[0].ID != planID {
		t.Fatalf("excluded path changed plan generation: plans=%v, want only %s", plans, planID)
	}
}

func approvalPlanLocalWriteAction(id, workID, target string) envelope.ActionIntent {
	action := approvalPlanHoldAction(id, workID)
	action.Capability = "filesystem.write"
	action.Action = "write_file"
	action.Target = envelope.ActionTarget{Kind: "file", Path: target}
	action.MutationClass = "local_reversible"
	action.Reversibility = "easy"
	action.BlastRadius = "repo"
	action.Destination = ""
	return action
}

func initHookApprovalHome(t *testing.T) string {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	return homePath
}

func hookApprovalPlanEntries(t *testing.T, homePath string) []eventstore.Entry {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"approval_plan_"}, RegularOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func assertNoHookApprovalPlans(t *testing.T, homePath string) {
	t.Helper()
	if plans := hookApprovalPlanEntries(t, homePath); len(plans) != 0 {
		ids := make([]string, 0, len(plans))
		for _, plan := range plans {
			ids = append(ids, plan.ID)
		}
		t.Fatalf("unexpected approval plans: %s", strings.Join(ids, ", "))
	}
}

func hookContainsRef(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hookRefCount(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func hookApprovalPlanRefCount(values []string) int {
	count := 0
	for _, value := range values {
		if strings.HasPrefix(value, "compound://events/approval_plan_") {
			count++
		}
	}
	return count
}
