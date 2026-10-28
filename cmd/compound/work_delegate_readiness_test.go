package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/harness"
	"compound/internal/hookplan"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestEvaluateDelegateHarnessReadinessRejectsUnboundInstructionPack(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	const harnessName = "claude_minimax"

	if _, err := harness.RenderInstructionPack(homePath, harnessName, "", now); err != nil {
		t.Fatal(err)
	}
	drift, err := harness.InstructionPackDrift(homePath, harnessName, "", now.Add(time.Minute))
	if err == nil {
		t.Fatalf("unbound instruction pack must not verify clean: %+v", drift)
	}

	result, err := evaluateDelegateHarnessReadiness(work.DelegateHarnessReadinessRequest{
		HomePath:    homePath,
		HarnessHome: t.TempDir(),
		Harness:     harnessName,
		Now:         now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready {
		t.Fatalf("clean-but-degraded instruction pack admitted: %+v", result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "instruction_pack: instruction-pack effective contract is unbound") {
		t.Fatalf("unbound authority refusal missing: %s", details)
	}
}

func TestEvaluateDelegateHarnessReadinessRejectsBoundButMissingLayers(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	const harnessName = "claude_minimax"
	seedCLIInstructionPackContext(t, homePath, harnessName, "")
	drift, err := harness.InstructionPackDrift(homePath, harnessName, "", cliTestNow)
	if err != nil || drift.Drifted {
		t.Fatalf("bound fixture must be drift-clean: %+v, %v", drift, err)
	}
	result, err := evaluateDelegateHarnessReadiness(work.DelegateHarnessReadinessRequest{
		HomePath: homePath, HarnessHome: t.TempDir(), Harness: harnessName, Now: cliTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready {
		t.Fatalf("bound but incomplete pack admitted: %+v", result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"instruction_pack: policy_core is absent",
		"instruction_pack: posture is absent",
		"instruction_pack: installed_hooks roster is absent",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details = %q, want %q", details, want)
		}
	}
}

func TestEvaluateDelegateHarnessReadinessBindsExecutableLiveHookAndWorkContext(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_readiness")
	request := work.DelegateHarnessReadinessRequest{
		HomePath:               fixture.compoundHome,
		HarnessHome:            fixture.harnessHome,
		WorkID:                 fixture.workID,
		ContextPackID:          fixture.contextPackID,
		Workspace:              fixture.workspace,
		Harness:                fixture.harness,
		LaneID:                 fixture.harness,
		RequestedModelID:       "fixture-zai",
		RequestedProviderRoute: "zai",
		Command:                "/bin/echo",
		Now:                    fixture.now.Add(time.Minute),
	}

	ready, err := evaluateDelegateHarnessReadiness(request)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.Ready || len(ready.Details) != 0 {
		t.Fatalf("fully attested harness rejected: %+v", ready)
	}
	kinds := map[string]bool{}
	for _, item := range ready.Evidence {
		kinds[item.Kind] = true
	}
	for _, want := range []string{
		"executable",
		"activation_argv",
		"permission_authority",
		"permission_posture",
		"blocking_hook_plan",
		"blocking_hook_installation",
		"context_pack",
		"effective_contract",
		"instruction_pack",
	} {
		if !kinds[want] {
			t.Fatalf("readiness evidence kinds = %v, missing %q", kinds, want)
		}
	}

	overridden := request
	overridden.Command = "/bin/sh"
	overridden.CommandOverridden = true
	overrideResult, err := evaluateDelegateHarnessReadiness(overridden)
	if err != nil {
		t.Fatal(err)
	}
	if overrideResult.Ready || !strings.Contains(strings.Join(overrideResult.Details, "\n"), "raw command override is not attested") {
		t.Fatalf("unattested command override admitted: %+v", overrideResult)
	}

	removeDelegateFixturePreToolUse(t, fixture.profilePath)
	liveDrift, err := evaluateDelegateHarnessReadiness(request)
	if err != nil {
		t.Fatal(err)
	}
	if liveDrift.Ready || !strings.Contains(strings.Join(liveDrift.Details, "\n"), "blocking_hook_live: mutation-covering binding status=missing") {
		t.Fatalf("missing live profile hook admitted: %+v", liveDrift)
	}
}

func TestEvaluateDelegateHarnessReadinessRejectsActivationAffectingArgv(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_activation_argv")
	base := work.DelegateHarnessReadinessRequest{
		HomePath:               fixture.compoundHome,
		HarnessHome:            fixture.harnessHome,
		WorkID:                 fixture.workID,
		ContextPackID:          fixture.contextPackID,
		Workspace:              fixture.workspace,
		Harness:                fixture.harness,
		LaneID:                 fixture.harness,
		RequestedModelID:       "fixture-zai",
		RequestedProviderRoute: "zai",
		Command:                "/bin/echo",
		Now:                    fixture.now.Add(time.Minute),
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "permission bypass", args: []string{"--dangerously-skip-permissions"}},
		{name: "profile override", args: []string{"--config-dir", t.TempDir()}},
		{name: "programmatic trailing flag", args: []string{"ordinary prompt", "--permission-mode", "bypass"}},
		{name: "unattested multi positional", args: []string{"ordinary", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			req.Args = tc.args
			result, err := evaluateDelegateHarnessReadiness(req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Ready || !strings.Contains(strings.Join(result.Details, "\n"), "activation_argv:") {
				t.Fatalf("activation-affecting argv admitted: args=%q result=%+v", tc.args, result)
			}
		})
	}

	ordinary := base
	ordinary.Args = []string{"continue the claimed work"}
	result, err := evaluateDelegateHarnessReadiness(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready {
		t.Fatalf("ordinary positional prompt rejected: %+v", result)
	}
}

func TestEvaluateDelegateHarnessReadinessRejectsUnrelatedEffectiveContract(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_wrong_contract")
	effective := fixture.effective
	effective.Harness = "codex"
	runtime := recordDelegateRuntimeForTest(t, fixture.compoundHome, effective, fixture.workID, fixture.workspace, fixture.now.Add(time.Minute))
	if _, err := harness.RenderInstructionPack(fixture.compoundHome, fixture.harness, "", fixture.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	result, err := evaluateDelegateHarnessReadiness(work.DelegateHarnessReadinessRequest{
		HomePath:               fixture.compoundHome,
		HarnessHome:            fixture.harnessHome,
		WorkID:                 fixture.workID,
		ContextPackID:          runtime.ContextPack.ID,
		Workspace:              fixture.workspace,
		Harness:                fixture.harness,
		LaneID:                 fixture.harness,
		RequestedModelID:       "fixture-zai",
		RequestedProviderRoute: "zai",
		Command:                "/bin/echo",
		Now:                    fixture.now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready || !strings.Contains(strings.Join(result.Details, "\n"), "effective contract harness identity does not match") {
		t.Fatalf("unrelated effective contract admitted: %+v", result)
	}
}

func TestEvaluateDelegateHarnessReadinessUsesWorkBoundImmutableContract(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_immutable_contract_a")
	otherWorkspace := t.TempDir()
	other := fixture.effective
	other.ID = "contract_work_delegate_immutable_contract_b"
	other.IDs.WorkID = "work_delegate_immutable_contract_b"
	other.Scope.Surface = otherWorkspace
	other.CreatedAt = fixture.now.Add(time.Minute).Format(time.RFC3339)
	recordDelegateRuntimeForTest(t, fixture.compoundHome, other, other.IDs.WorkID, otherWorkspace, fixture.now.Add(time.Minute))

	result, err := evaluateDelegateHarnessReadiness(work.DelegateHarnessReadinessRequest{
		HomePath:               fixture.compoundHome,
		HarnessHome:            fixture.harnessHome,
		WorkID:                 fixture.workID,
		ContextPackID:          fixture.contextPackID,
		Workspace:              fixture.workspace,
		Harness:                fixture.harness,
		LaneID:                 fixture.harness,
		RequestedModelID:       "fixture-zai",
		RequestedProviderRoute: "zai",
		Command:                "/bin/echo",
		Now:                    fixture.now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready {
		t.Fatalf("later orientation invalidated work-bound contract: %+v", result)
	}
	var contractEvidence *work.DelegateHarnessReadinessEvidence
	for i := range result.Evidence {
		if result.Evidence[i].Kind == "effective_contract" {
			contractEvidence = &result.Evidence[i]
			break
		}
	}
	if contractEvidence == nil || contractEvidence.Ref != "contract://"+fixture.effective.ID ||
		!strings.HasPrefix(contractEvidence.Revision, "sha256:") {
		t.Fatalf("effective-contract evidence did not bind immutable work artifact: %+v", result.Evidence)
	}
}

func TestEvaluateDelegateHarnessReadinessRequiresExactDeclaredMutationCoveringHookBinding(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_exact_mutation_hook")
	installReadyReadButMissingMutationHook(t, fixture)

	result, err := evaluateDelegateHarnessReadiness(work.DelegateHarnessReadinessRequest{
		HomePath:               fixture.compoundHome,
		HarnessHome:            fixture.harnessHome,
		WorkID:                 fixture.workID,
		ContextPackID:          fixture.contextPackID,
		Workspace:              fixture.workspace,
		Harness:                fixture.harness,
		LaneID:                 fixture.harness,
		RequestedModelID:       "fixture-zai",
		RequestedProviderRoute: "zai",
		Command:                "/bin/echo",
		Now:                    fixture.now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready || !strings.Contains(strings.Join(result.Details, "\n"), "blocking_hook_live: mutation-covering") {
		t.Fatalf("unrelated ready PreToolUse binding substituted for missing mutation-covering gate: %+v", result)
	}
}

func TestDelegateDeclaredMutationBlockingBindingRequiresCanonicalAllToolMatcher(t *testing.T) {
	t.Parallel()
	const harnessName = "claude_zai"
	binding := hookplan.Binding{
		ID:       "hook_fixture",
		Harness:  "claude-zai",
		Event:    "PreToolUse",
		Blocking: true,
	}

	binding.Matcher = hookplan.HarnessMatcher(harnessName)
	got, detail := delegateDeclaredMutationBlockingBinding(hookplan.HookPlan{Bindings: []hookplan.Binding{binding}}, harnessName)
	if got == nil || detail != "" {
		t.Fatalf("canonical all-tool matcher rejected: binding=%+v detail=%q", got, detail)
	}

	for _, tc := range []struct {
		name    string
		matcher string
	}{
		{name: "empty", matcher: ""},
		{name: "read only", matcher: "Read"},
		{name: "substring lookalike", matcher: "ReadEditMetadata"},
		{name: "legacy write tools", matcher: "Write|Edit|MultiEdit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding.Matcher = tc.matcher
			got, detail := delegateDeclaredMutationBlockingBinding(hookplan.HookPlan{Bindings: []hookplan.Binding{binding}}, harnessName)
			if got != nil || !strings.Contains(detail, "is absent") {
				t.Fatalf("non-canonical matcher %q substituted for canonical %q: binding=%+v detail=%q",
					tc.matcher, hookplan.HarnessMatcher(harnessName), got, detail)
			}
		})
	}
}

func TestDelegateBlockingReadinessUsesSemanticIntentionNotClaudeEventName(t *testing.T) {
	t.Parallel()

	binding := hookplan.Binding{
		ID:                  "hook://fixture/native-before-write",
		Harness:             "fixture",
		Event:               "native.before.write",
		Matcher:             "native-mutation-scope",
		HandlerRef:          "compound",
		HandlerRevision:     "sha256:handler",
		Blocking:            true,
		Intention:           hookplan.IntentionGuardMutation,
		Disposition:         hookplan.DispositionNative,
		EnforcementStrength: "blocking",
	}
	plan := hookplan.HookPlan{
		Intentions: []hookplan.CompiledIntention{{
			Intention:           hookplan.IntentionGuardMutation,
			Disposition:         hookplan.DispositionNative,
			EnforcementStrength: "blocking",
			NativeEvents:        []string{"native.before.write"},
		}},
		Bindings: []hookplan.Binding{binding},
	}

	got, detail := delegateDeclaredMutationBlockingBinding(plan, "fixture")
	if detail != "" {
		t.Fatalf("semantic blocking binding refused: %s", detail)
	}
	if got == nil || got.ID != binding.ID {
		t.Fatalf("semantic blocking binding = %+v, want %s", got, binding.ID)
	}
}

func TestDelegateBlockingReadinessExplainsUnsupportedSemanticGuard(t *testing.T) {
	t.Parallel()

	plan := hookplan.HookPlan{
		Intentions: []hookplan.CompiledIntention{{
			Intention:           hookplan.IntentionGuardMutation,
			Disposition:         hookplan.DispositionUnsupported,
			EnforcementStrength: "none",
			Reason:              "native callback cannot deny",
		}},
	}
	got, detail := delegateDeclaredMutationBlockingBinding(plan, "opencode")
	if got != nil {
		t.Fatalf("unsupported mutation guard returned binding: %+v", got)
	}
	if !strings.Contains(detail, "unsupported") || !strings.Contains(detail, "native callback cannot deny") {
		t.Fatalf("unsupported mutation guard detail = %q", detail)
	}
}

func TestEvaluateDelegateHarnessReadinessRejectsPermissionAuthorityRevisionChange(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_permission_revision")
	originalLoad := delegateLoadPermissionAuthority
	loads := 0
	delegateLoadPermissionAuthority = func(homePath, harnessName string) (harness.PermissionAuthority, string, error) {
		policy, revision, err := originalLoad(homePath, harnessName)
		loads++
		if err == nil && loads > 1 {
			revision = "sha256:" + strings.Repeat("f", 64)
		}
		return policy, revision, err
	}
	t.Cleanup(func() {
		delegateLoadPermissionAuthority = originalLoad
	})

	result, err := evaluateDelegateHarnessReadiness(work.DelegateHarnessReadinessRequest{
		HomePath:               fixture.compoundHome,
		HarnessHome:            fixture.harnessHome,
		WorkID:                 fixture.workID,
		ContextPackID:          fixture.contextPackID,
		Workspace:              fixture.workspace,
		Harness:                fixture.harness,
		LaneID:                 fixture.harness,
		RequestedModelID:       "fixture-zai",
		RequestedProviderRoute: "zai",
		Command:                "/bin/echo",
		Now:                    fixture.now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if loads < 2 {
		t.Fatalf("permission authority was not revalidated after drift check: loads=%d", loads)
	}
	if result.Ready || !strings.Contains(strings.Join(result.Details, "\n"), "permission_authority: revision changed during readiness") {
		t.Fatalf("permission authority TOCTOU admitted: %+v", result)
	}
}

func TestEvaluateDelegateHarnessReadinessAttestsExactPermissionBytesValidatedByOwner(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_permission_target_revision")
	originalDrift := delegatePermissionDrift
	var validatedRevision string
	delegatePermissionDrift = func(homePath, harnessName string) (harness.PermissionDriftReport, error) {
		report, err := originalDrift(homePath, harnessName)
		if err != nil {
			return report, err
		}
		validatedRevision = report.TargetRevision
		mutated := []byte("{\"permissions\":{\"defaultMode\":\"bypass\"}}\n")
		if err := os.WriteFile(report.TargetPath, mutated, 0o644); err != nil {
			return report, err
		}
		return report, nil
	}
	t.Cleanup(func() {
		delegatePermissionDrift = originalDrift
	})

	result, err := evaluateDelegateHarnessReadiness(work.DelegateHarnessReadinessRequest{
		HomePath:               fixture.compoundHome,
		HarnessHome:            fixture.harnessHome,
		WorkID:                 fixture.workID,
		ContextPackID:          fixture.contextPackID,
		Workspace:              fixture.workspace,
		Harness:                fixture.harness,
		LaneID:                 fixture.harness,
		RequestedModelID:       "fixture-zai",
		RequestedProviderRoute: "zai",
		Command:                "/bin/echo",
		Now:                    fixture.now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	var postureRevision string
	for _, evidence := range result.Evidence {
		if evidence.Kind == "permission_posture" {
			postureRevision = evidence.Revision
			break
		}
	}
	if validatedRevision == "" || postureRevision != validatedRevision {
		t.Fatalf("permission posture attested revision %q, want exact owner-validated bytes %q; result=%+v", postureRevision, validatedRevision, result)
	}
}

func TestWorkDelegatePersistsSuccessfulReadinessAttestation(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_attestation")
	store, err := work.Open(fixture.compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(work.CreateInput{
		WorkID:           fixture.workID,
		Owner:            "orchestrator:test",
		Actor:            "orchestrator:test",
		ContextPackID:    fixture.contextPackID,
		SuccessPredicate: "delegated activation preserves its admission evidence",
		State:            "ready",
	}, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(created.ID, "orchestrator:test", fixture.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	originalGuard := work.LaneCapacityGuard
	originalResolver := work.DelegateHarnessResolver
	originalReadiness := work.DelegateHarnessReadiness
	capacityCalls := 0
	work.LaneCapacityGuard = func(work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		capacityCalls++
		return work.LaneCapacityGuardResult{Capacity: work.DispatchLaneCapacity{State: "ok", Remaining: 0.8}}, nil
	}
	work.DelegateHarnessResolver = func(work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		return work.DelegateHarnessResolution{
			Harness:                    fixture.harness,
			Command:                    "/bin/echo",
			LaneID:                     fixture.harness,
			RequestedModelID:           "fixture-zai",
			RequestedProviderRoute:     "zai",
			RequestedProviderRouteKind: "provider_class",
			CapacityLane:               "zai",
			TupleBacked:                true,
		}, nil
	}
	work.DelegateHarnessReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		req.HarnessHome = fixture.harnessHome
		return evaluateDelegateHarnessReadiness(req)
	}
	t.Cleanup(func() {
		work.LaneCapacityGuard = originalGuard
		work.DelegateHarnessResolver = originalResolver
		work.DelegateHarnessReadiness = originalReadiness
	})

	runner := newDelegateCLIFakeRunner()
	blockedOut, blockedErr, blockedCode := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		fixture.workID,
		"--harness", fixture.harness,
		"--actor", "orchestrator:test",
		"--workspace", fixture.workspace,
		"--prompt", "--dangerously-skip-permissions",
		"--json",
	)
	if blockedCode != 1 || blockedErr != "" {
		t.Fatalf("activation argv refusal code=%d stderr=%q stdout=%q", blockedCode, blockedErr, blockedOut)
	}
	var blocked work.DelegateResult
	if err := json.Unmarshal([]byte(blockedOut), &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Refusal == nil || blocked.Refusal.Reason != "readiness_unmet" ||
		blocked.Readiness == nil ||
		!strings.Contains(strings.Join(blocked.Readiness.Details, "\n"), "activation_argv: flag-shaped argv is not attested") {
		t.Fatalf("activation argv refusal = %+v", blocked)
	}
	if capacityCalls != 0 || len(runner.creates) != 0 {
		t.Fatalf("activation argv reached side effects: capacity_calls=%d creates=%+v", capacityCalls, runner.creates)
	}

	result, err := store.Delegate(work.DelegateOptions{
		WorkID:    fixture.workID,
		Harness:   fixture.harness,
		Actor:     "orchestrator:test",
		Workspace: fixture.workspace,
		Now:       fixture.now.Add(2 * time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" || result.Readiness == nil || !result.Readiness.Ready ||
		result.Binding == nil || result.Binding.Readiness == nil || !result.Binding.Readiness.Ready {
		t.Fatalf("spawned readiness attestation = %+v", result)
	}
	if !strings.HasPrefix(result.Readiness.SnapshotRevision, "sha256:") ||
		result.Binding.Readiness.SnapshotRevision != result.Readiness.SnapshotRevision {
		t.Fatalf("spawned binding lost exact readiness snapshot revision: result=%+v binding=%+v", result.Readiness, result.Binding.Readiness)
	}
	evidenceKinds := map[string]bool{}
	for _, item := range result.Readiness.Evidence {
		evidenceKinds[item.Kind] = true
	}
	for _, kind := range []string{
		"permission_authority",
		"permission_posture",
		"blocking_hook_plan",
		"blocking_hook_installation",
		"context_pack",
		"effective_contract",
		"instruction_pack",
		"lane_capacity",
	} {
		if !evidenceKinds[kind] {
			t.Fatalf("snapshot revision omitted %s evidence: %+v", kind, result.Readiness)
		}
	}
	events, err := eventstore.Open(fixture.compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	record, err := events.Read(result.EventID)
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Readiness *work.DelegateHarnessReadinessResult `json:"readiness"`
	}
	if err := json.Unmarshal(record.Bytes, &event); err != nil {
		t.Fatal(err)
	}
	if event.Readiness == nil || !event.Readiness.Ready || len(event.Readiness.Evidence) == 0 ||
		event.Readiness.SnapshotRevision != result.Readiness.SnapshotRevision {
		t.Fatalf("spawn event lost readiness attestation: %s", record.Bytes)
	}
	if capacityCalls != 2 {
		t.Fatalf("successful activation capacity calls = %d, want admission + immediate pre-mint revalidation", capacityCalls)
	}
}

func TestWorkDelegateCLIRefusesUnattestedCommandOverrideBeforeMutation(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_raw_override")
	store, err := work.Open(fixture.compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(work.CreateInput{
		WorkID:           fixture.workID,
		Owner:            "orchestrator:test",
		Actor:            "orchestrator:test",
		ContextPackID:    fixture.contextPackID,
		SuccessPredicate: "an arbitrary command cannot borrow a harness readiness attestation",
		State:            "ready",
	}, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(created.ID, "orchestrator:test", fixture.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	originalGuard := work.LaneCapacityGuard
	originalResolver := work.DelegateHarnessResolver
	originalReadiness := work.DelegateHarnessReadiness
	capacityCalls := 0
	work.LaneCapacityGuard = func(work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		capacityCalls++
		return work.LaneCapacityGuardResult{Capacity: work.DispatchLaneCapacity{State: "ok", Remaining: 0.8}}, nil
	}
	work.DelegateHarnessResolver = func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		command := fixture.binary
		if strings.TrimSpace(req.CommandOverride) != "" {
			command = req.CommandOverride
		}
		return work.DelegateHarnessResolution{
			Harness:                    fixture.harness,
			Command:                    command,
			LaneID:                     fixture.harness,
			RequestedModelID:           "fixture-zai",
			RequestedProviderRoute:     "zai",
			RequestedProviderRouteKind: "provider_class",
			CapacityLane:               "zai",
			TupleBacked:                true,
		}, nil
	}
	work.DelegateHarnessReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		req.HarnessHome = fixture.harnessHome
		return evaluateDelegateHarnessReadiness(req)
	}
	t.Cleanup(func() {
		work.LaneCapacityGuard = originalGuard
		work.DelegateHarnessResolver = originalResolver
		work.DelegateHarnessReadiness = originalReadiness
	})

	runner := newDelegateCLIFakeRunner()
	before := snapshotFiles(t, fixture.compoundHome)
	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		fixture.workID,
		"--harness", fixture.harness,
		"--actor", "orchestrator:test",
		"--workspace", fixture.workspace,
		"--json",
		"--", "/bin/sh", "-c", "true",
	)
	if code != 1 || errOut != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	var result work.DelegateResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "readiness_unmet" ||
		result.Readiness == nil || !strings.Contains(strings.Join(result.Readiness.Details, "\n"), "raw command override is not attested") {
		t.Fatalf("raw override refusal = %+v", result)
	}
	if capacityCalls != 0 || len(runner.creates) != 0 {
		t.Fatalf("raw override reached side effects: capacity_calls=%d creates=%+v", capacityCalls, runner.creates)
	}
	if after := snapshotFiles(t, fixture.compoundHome); !reflect.DeepEqual(before, after) {
		t.Fatalf("raw override refusal mutated Compound state\nbefore=%v\nafter=%v", before, after)
	}
}

type readyDelegateHarnessFixture struct {
	compoundHome  string
	harnessHome   string
	workspace     string
	workID        string
	contextPackID string
	harness       string
	binary        string
	profilePath   string
	now           time.Time
	effective     envelope.EffectiveContract
}

func buildReadyDelegateHarnessFixture(t *testing.T, workID string) readyDelegateHarnessFixture {
	t.Helper()
	return buildReadyDelegateHarnessFixtureAt(t, workID, time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC))
}

func buildReadyDelegateHarnessFixtureAt(t *testing.T, workID string, now time.Time) readyDelegateHarnessFixture {
	t.Helper()
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", harnessHome)
	const harnessName = "claude_zai"

	source := filepath.Join("..", "..", "adapters", "claude-zai", "hook-registration.ts")
	if err := contract.StageSyntheticHookHandlers(harnessHome, source); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.ImportHookRegistrationTS(compoundHome, source, now); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(harnessHome, ".local", "bin", "compound")
	profileDir := hookplan.ClaudeZaiProfileDir(harnessHome)
	if _, err := hookplan.InstallHooksWithConfigDir(harnessHome, binary, "global", []string{"claude-zai"}, profileDir, false); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(profileDir, "settings.json")
	writeDelegateReadinessJSON(t, harness.PermissionAuthorityPath(compoundHome, harnessName), harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       harnessName,
		AuthorityRef:  harness.PermissionAuthorityRef(harnessName),
		TargetPath:    profilePath,
		Permissions: harness.PermissionRules{
			Allow: []string{"Read"}, Deny: []string{}, Ask: []string{}, DefaultMode: "auto",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	})
	render, err := harness.RenderPermissions(compoundHome, harnessName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := harness.WriteHarnessArtifactManifest(compoundHome, []harness.HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}

	taskContract := envelope.TaskContract{
		Intent:           "execute the claimed delegated work",
		Mode:             "execute",
		DesiredOutcome:   "the governed lane runs",
		SuccessPredicate: "readiness evidence remains attached",
	}
	effective := envelope.EffectiveContract{
		Header:             delegateReadinessHeader(envelope.SVEffectiveContract, "contract_"+workID, workID, workspace, now),
		Harness:            harnessName,
		ContextKey:         contract.Compile(contract.Input{Harness: harnessName, Cwd: workspace, TaskContract: taskContract}, contract.DefaultPolicy(), now).ContextKey,
		TaskContract:       taskContract,
		ActiveInstructions: []string{},
		ActiveHooks:        []string{"hook://" + harnessName + "/PreToolUse"},
		ActiveDecisions:    []string{},
		Excluded:           []envelope.ExcludedRef{},
		SourceRevisions:    []envelope.SourceRev{},
		Unknowns:           []string{},
		Conflicts:          []string{},
	}
	contextPackID := "ctx_" + strings.TrimPrefix(workID, "work_")
	contextPack := validCLIContextPack()
	contextPack.ID = contextPackID
	contextPack.IDs = envelope.IDs{WorkID: workID, ContractID: effective.ID}
	contextPack.Scope = envelope.Scope{Surface: workspace}
	contextPack.TaskContract = taskContract
	contextPack.CreatedAt = now.Format(time.RFC3339)
	runtime, err := contract.SealRuntimeMaterialization(contract.RuntimeMaterialization{
		EffectiveContract: effective,
		ContextPack:       contextPack,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.RecordRuntimeMaterialization(compoundHome, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.RenderInstructionPackForContext(compoundHome, harnessName, "", runtime.ContextPack.ID, now); err != nil {
		t.Fatal(err)
	}
	effective = runtime.EffectiveContract
	contextPackID = runtime.ContextPack.ID
	fixture := readyDelegateHarnessFixture{
		compoundHome:  compoundHome,
		harnessHome:   harnessHome,
		workspace:     workspace,
		workID:        workID,
		contextPackID: contextPackID,
		harness:       harnessName,
		binary:        binary,
		profilePath:   profilePath,
		now:           now,
		effective:     effective,
	}
	writeDelegateCapacityManifest(t, fixture, false)
	return fixture
}

func writeDelegateCapacityManifest(t *testing.T, fixture readyDelegateHarnessFixture, capacity bool) {
	t.Helper()
	model := map[string]any{
		"lane": "fixture lane", "harness": fixture.harness, "provider_route": "zai",
		"tier": "cheap", "packet_ceiling": "multi-file", "aliases": []string{"/bin/echo"},
		"capabilities": map[string]any{"build-implement": map[string]any{"level": "capable", "evidence": "fixture"}},
	}
	if capacity {
		model["capacity_source"] = map[string]any{"kind": "codexbar", "freshness_minutes": 15, "ref": "codexbar://provider/zai"}
	}
	writeDelegateReadinessJSON(t, filepath.Join(fixture.compoundHome, "registries", "model-capabilities.yaml"), map[string]any{
		"version": 1, "purpose": "fixture", "capability_classes": []string{"build-implement"},
		"models":           map[string]any{"fixture-zai": model},
		"routing_defaults": map[string]any{"build-implement": []string{"fixture-zai"}},
	})
}

func TestDelegateReadinessPredictsAdapterCapacityWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		used       int
		resetAfter time.Duration
		age        time.Duration
		missing    bool
		wantState  string
	}{
		{name: "alive", used: 10, resetAfter: 2 * time.Hour},
		{name: "expiring", used: 10, resetAfter: 10 * time.Minute, wantState: "expiring"},
		{name: "exhausted", used: 100, resetAfter: 2 * time.Hour, wantState: "walled"},
		{name: "missing", missing: true, wantState: "unknown"},
		{name: "stale", used: 10, resetAfter: 2 * time.Hour, age: time.Hour, wantState: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := buildReadyDelegateHarnessFixture(t, "work_capacity_"+tc.name)
			writeDelegateCapacityManifest(t, fixture, true)
			// Missing/stale evidence must not contact a real capacity provider.
			binDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(binDir, "codexbar"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			now := fixture.now.Add(time.Minute)
			if !tc.missing {
				usage := filepath.Join(fixture.harnessHome, "Library", "Application Support", "CodexBar", "usage-history.jsonl")
				body, err := json.Marshal(map[string]any{
					"provider": "zai", "windowKind": "5h", "windowMinutes": 300, "usedPercent": tc.used,
					"resetsAt": now.Add(tc.resetAfter).Format(time.RFC3339), "sampledAt": now.Add(-tc.age).Format(time.RFC3339),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(usage), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(usage, append(body, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			req := work.DelegateHarnessReadinessRequest{
				HomePath: fixture.compoundHome, HarnessHome: fixture.harnessHome,
				WorkID: fixture.workID, ContextPackID: fixture.contextPackID, Workspace: fixture.workspace,
				Harness: fixture.harness, LaneID: fixture.harness, RequestedModelID: "fixture-zai",
				RequestedProviderRoute: "zai", Command: "/bin/echo", Now: now,
			}
			before := snapshotFiles(t, fixture.compoundHome)
			harnessBefore := snapshotFiles(t, fixture.harnessHome)
			got, err := evaluateDelegateHarnessReadiness(req)
			if err != nil {
				t.Fatal(err)
			}
			if after := snapshotFiles(t, fixture.compoundHome); !reflect.DeepEqual(before, after) {
				t.Fatal("readiness mutated the Compound home")
			}
			if after := snapshotFiles(t, fixture.harnessHome); !reflect.DeepEqual(harnessBefore, after) {
				t.Fatal("readiness mutated the harness home")
			}
			wantReady := tc.wantState == ""
			if got.Ready != wantReady {
				t.Fatalf("readiness=%v want=%v for capacity %s: %v", got.Ready, wantReady, tc.name, got.Details)
			}
			if !wantReady && !strings.Contains(strings.Join(got.Details, "\n"), "capacity "+tc.wantState) {
				t.Fatalf("missing exact capacity refusal %q: %v", tc.wantState, got.Details)
			}
			adapter, ok := harness.NewAdapterRegistry(runattempt.Open(fixture.compoundHome)).AdapterByName(fixture.harness)
			if !ok {
				t.Fatal("fixture adapter missing")
			}
			_, executeErr := adapter.(harness.PreflightAdapter).Preflight(context.Background(), harness.RunInput{Now: now})
			if (executeErr == nil) != got.Ready {
				t.Fatalf("preview/execute mismatch: ready=%v execute=%v", got.Ready, executeErr)
			}
		})
	}
}

func recordDelegateRuntimeForTest(
	t *testing.T,
	compoundHome string,
	effective envelope.EffectiveContract,
	workID string,
	workspace string,
	now time.Time,
) contract.RuntimeMaterialization {
	t.Helper()
	effective.ContextKey = contract.Compile(contract.Input{Harness: effective.Harness, Cwd: workspace, TaskContract: effective.TaskContract}, contract.DefaultPolicy(), now).ContextKey
	contextPack := validCLIContextPack()
	contextPack.ID = "ctx_" + strings.TrimPrefix(workID, "work_") + "_latest"
	contextPack.IDs = envelope.IDs{WorkID: workID, ContractID: effective.ID}
	contextPack.Scope = envelope.Scope{Surface: workspace}
	contextPack.TaskContract = effective.TaskContract
	contextPack.CreatedAt = now.UTC().Format(time.RFC3339)
	runtime, err := contract.SealRuntimeMaterialization(contract.RuntimeMaterialization{
		EffectiveContract: effective,
		ContextPack:       contextPack,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.RecordRuntimeMaterialization(compoundHome, runtime); err != nil {
		t.Fatal(err)
	}
	return runtime
}

func delegateReadinessHeader(schemaVersion, id, workID, workspace string, now time.Time) envelope.Header {
	return envelope.Header{
		SchemaVersion: schemaVersion,
		ID:            id,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Producer:      envelope.Producer{Kind: "cli", Name: "compound", Version: "test"},
		IDs:           envelope.IDs{WorkID: workID},
		Scope:         envelope.Scope{Surface: workspace},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       "private",
		Risk:          "tier1",
		EvidenceRefs:  []string{},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}

func removeDelegateFixturePreToolUse(t *testing.T, profilePath string) {
	t.Helper()
	var settings map[string]any
	readDelegateReadinessJSONForTest(t, profilePath, &settings)
	hooks, _ := settings["hooks"].(map[string]any)
	delete(hooks, "PreToolUse")
	writeDelegateReadinessJSON(t, profilePath, settings)
}

func installReadyReadButMissingMutationHook(t *testing.T, fixture readyDelegateHarnessFixture) {
	t.Helper()
	var settings map[string]any
	readDelegateReadinessJSONForTest(t, fixture.profilePath, &settings)
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("profile hooks = %#v", settings["hooks"])
	}
	groups, ok := hooks["PreToolUse"].([]any)
	if !ok || len(groups) == 0 {
		t.Fatalf("profile PreToolUse = %#v", hooks["PreToolUse"])
	}
	var commandHooks []any
	for _, raw := range groups {
		group, _ := raw.(map[string]any)
		if group["matcher"] == "*" {
			commandHooks, _ = group["hooks"].([]any)
			break
		}
	}
	if len(commandHooks) == 0 {
		t.Fatalf("profile missing baseline mutation-covering hook: %#v", groups)
	}
	hooks["PreToolUse"] = []any{map[string]any{
		"matcher": "Read",
		"hooks":   commandHooks,
	}}
	writeDelegateReadinessJSON(t, fixture.profilePath, settings)
	if _, err := harness.RenderInstructionPack(fixture.compoundHome, fixture.harness, "", fixture.now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func writeDelegateReadinessJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readDelegateReadinessJSONForTest(t *testing.T, path string, out any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatal(err)
	}
}
