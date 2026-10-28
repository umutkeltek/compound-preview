package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"compound/internal/runattempt"
)

func testDelegateExecutionProfilePlan() *DelegateExecutionProfilePlan {
	return &DelegateExecutionProfilePlan{
		SchemaVersion:           "compound.execution_profile_plan.v0.1",
		ID:                      "epplan_test1234",
		ProfileID:               "coding-implementation",
		Role:                    "executor",
		CapabilityClass:         "build-implement",
		ClientHarnessID:         "claude-code",
		TransportID:             "switchback",
		RouteAlias:              "auto/build",
		ModelConstraint:         "fixture/model",
		SemanticEffort:          "deep",
		RequestedNativeEffort:   "xhigh",
		NativeEffortDisposition: "translated",
		NativeEffortReason:      "mapped through selected client",
		OrchestrationPolicy:     "standard",
		PrivacyCeiling:          "private",
		Budget:                  json.RawMessage(`{"max_requests":40}`),
		Checkpoint:              json.RawMessage(`{"required":false}`),
		Warnings:                []string{"fixture warning"},
		Snapshot:                json.RawMessage(`{"budget":{"max_requests":40},"client_harness_id":"claude-code","id":"epplan_test1234","model_constraint":"fixture/model","native_effort":{"disposition":"translated","reason":"mapped through selected client","requested":"xhigh"},"profile_id":"coding-implementation","route_alias":"auto/build","schema_version":"compound.execution_profile_plan.v0.1","transport_id":"switchback","warnings":["fixture warning"]}`),
	}
}

func testDelegateExecutionProfileResolution(plan *DelegateExecutionProfilePlan, selectedHarness, selectedLane, disposition string) *DelegateExecutionProfileResolution {
	return testDelegateExecutionProfileResolutionForTuple(
		plan,
		selectedHarness,
		selectedLane,
		"fixture/model",
		"auto/build",
		"switchback",
		disposition,
	)
}

func testDelegateExecutionProfileResolutionForTuple(
	plan *DelegateExecutionProfilePlan,
	selectedHarness string,
	selectedLane string,
	selectedModelID string,
	selectedProviderRoute string,
	selectedTransportID string,
	disposition string,
) *DelegateExecutionProfileResolution {
	return &DelegateExecutionProfileResolution{
		Snapshot: append(json.RawMessage(nil), plan.Snapshot...),
		Compatibility: DelegateExecutionProfileCompatibility{
			Disposition:           disposition,
			SelectedHarness:       selectedHarness,
			SelectedLaneID:        selectedLane,
			SelectedModelID:       selectedModelID,
			SelectedProviderRoute: selectedProviderRoute,
			SelectedTransportID:   selectedTransportID,
			PlanID:                plan.ID,
			ClientHarnessID:       plan.ClientHarnessID,
			TransportID:           plan.TransportID,
			RequestedNativeEffort: plan.RequestedNativeEffort,
		},
	}
}

func testCompatibleDelegateExecutionProfileResolution(plan *DelegateExecutionProfilePlan) *DelegateExecutionProfileResolution {
	return testDelegateExecutionProfileResolution(plan, "claude_code", "claude_code", DelegateExecutionProfileCompatible)
}

func testCompatibleDelegateLaunchProfileResolution() *DelegateLaunchProfileResolution {
	snapshot := json.RawMessage(`{"authority_revision":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","capture_mode":"segmented_full_wire","client_profile_id":"claude-minimax-default","executable_id":"claude","harness":"claude-code","id":"claude-minimax","owner_transport":"tap","provider_lane":"minimax","provider_route_kind":"direct_model","requested_effort":"xhigh","requested_model":"fixture/model","revision":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","route":"auto/build","schema_version":"compound.delegate_launch_profile_plan.v0.2","source_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","source_schema":"switchback/launch-profile-plan@1","transport_id":"switchback"}`)
	return &DelegateLaunchProfileResolution{
		Snapshot:       snapshot,
		ExecutablePath: "/synthetic/bin/claude",
		Compatibility: DelegateLaunchProfileCompatibility{
			Disposition:               DelegateLaunchProfileCompatible,
			SelectedHarness:           "claude_code",
			SelectedLaneID:            "claude_code",
			SelectedModelID:           "fixture/model",
			SelectedProviderRoute:     "auto/build",
			SelectedProviderRouteKind: "direct_model",
			SelectedTransportID:       "switchback",
			RequestedNativeEffort:     "xhigh",
			PlanID:                    "claude-minimax",
			ClientHarnessID:           "claude-code",
			ClientProfileID:           "claude-minimax-default",
			Revision:                  "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			SelectedExecutableID:      "claude",
			ExecutableID:              "claude",
		},
	}
}

func testCompatibleProviderClassDelegateLaunchProfileResolution() *DelegateLaunchProfileResolution {
	resolution := testCompatibleDelegateLaunchProfileResolution()
	resolution.Snapshot = bytes.Replace(
		resolution.Snapshot,
		[]byte(`"provider_route_kind":"direct_model"`),
		[]byte(`"provider_route_kind":"provider_class"`),
		1,
	)
	resolution.Compatibility.SelectedProviderRoute = "minimax"
	resolution.Compatibility.SelectedProviderRouteKind = "provider_class"
	return resolution
}

func installTestDelegateExecutionProfileHarness(t *testing.T) {
	t.Helper()
	installTestDelegateExecutionProfileHarnessTuple(t, "fixture/model", "auto/build", "switchback")
}

func installTestDelegateExecutionProfileHarnessTuple(t *testing.T, modelID, providerRoute, transportID string) {
	installTestDelegateExecutionProfileHarnessTupleWithRouteKind(
		t,
		modelID,
		providerRoute,
		"direct_model",
		transportID,
	)
}

func installTestDelegateExecutionProfileProviderClassHarnessTuple(t *testing.T, modelID, providerRoute, transportID string) {
	installTestDelegateExecutionProfileHarnessTupleWithRouteKind(
		t,
		modelID,
		providerRoute,
		"provider_class",
		transportID,
	)
}

func installTestDelegateExecutionProfileHarnessTupleWithRouteKind(
	t *testing.T,
	modelID,
	providerRoute,
	providerRouteKind,
	transportID string,
) {
	t.Helper()
	original := DelegateHarnessResolver
	DelegateHarnessResolver = func(req DelegateHarnessResolveRequest) (DelegateHarnessResolution, error) {
		return DelegateHarnessResolution{
			Harness:                    strings.TrimSpace(req.Harness),
			Command:                    strings.TrimSpace(req.CommandOverride),
			LaneID:                     strings.TrimSpace(req.Harness),
			RequestedModelID:           modelID,
			RequestedProviderRoute:     providerRoute,
			RequestedProviderRouteKind: providerRouteKind,
			SelectedTransportID:        transportID,
			CapacityLane:               strings.TrimSpace(req.Harness),
			TupleBacked:                true,
		}, nil
	}
	t.Cleanup(func() {
		DelegateHarnessResolver = original
	})
}

func TestDelegateLegacyTupleWithoutLaunchProfileDoesNotRequireProviderRouteKind(t *testing.T) {
	installTestDelegateExecutionProfileHarnessTupleWithRouteKind(
		t,
		"fixture/model",
		"auto/build",
		"",
		"switchback",
	)
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_code",
		Command:   "claude",
		Workspace: t.TempDir(),
		Now:       fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" || result.Refusal != nil || len(runner.creates) != 1 {
		t.Fatalf("legacy tuple without launch profile was not preserved: %+v creates=%d", result, len(runner.creates))
	}
}

func TestDelegateRefusesMissingOrUnknownProviderRouteKindBeforeProfilesReadinessCapacityMintOrCreate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route string
		kind  string
	}{
		{name: "missing route and kind", route: "", kind: ""},
		{name: "missing kind", route: "provider.example/model-primary", kind: ""},
		{name: "unknown kind", route: "provider.example/model-primary", kind: "infer_from_route"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalHarnessResolver := DelegateHarnessResolver
			originalProfileResolver := DelegateExecutionProfileResolver
			originalReadiness := DelegateHarnessReadiness
			originalCapacity := LaneCapacityGuard
			defer func() {
				DelegateHarnessResolver = originalHarnessResolver
				DelegateExecutionProfileResolver = originalProfileResolver
				DelegateHarnessReadiness = originalReadiness
				LaneCapacityGuard = originalCapacity
			}()

			DelegateHarnessResolver = func(req DelegateHarnessResolveRequest) (DelegateHarnessResolution, error) {
				return DelegateHarnessResolution{
					Harness:                    strings.TrimSpace(req.Harness),
					Command:                    strings.TrimSpace(req.CommandOverride),
					LaneID:                     strings.TrimSpace(req.Harness),
					RequestedModelID:           "provider.example/model-primary",
					RequestedProviderRoute:     tc.route,
					RequestedProviderRouteKind: tc.kind,
					SelectedTransportID:        "switchback",
					CapacityLane:               "provider.example/model-primary",
					TupleBacked:                true,
				}, nil
			}
			profileCalls := 0
			readinessCalls := 0
			capacityCalls := 0
			DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
				profileCalls++
				return testCompatibleDelegateExecutionProfileResolution(testDelegateExecutionProfilePlan()), nil
			}
			DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
				readinessCalls++
				return DelegateHarnessReadinessResult{Ready: true}, nil
			}
			LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
				capacityCalls++
				return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
			}

			store := newExperimentalDelegateStore(t)
			workID := prepareClaimedWork(t, store, "codex", "", "")
			before, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			runner := newFakeRunner()
			result, err := store.Delegate(DelegateOptions{
				WorkID:    workID,
				Harness:   "claude_code",
				Command:   "claude",
				Workspace: t.TempDir(),
				ExecutionProfile: DelegateExecutionProfileIntent{
					ProfileID:       "coding-implementation",
					LaunchProfileID: "fixture-launch",
				},
				Now: fixedNow,
			}, runner)
			if err != nil {
				t.Fatal(err)
			}
			if result.Refusal == nil || result.Refusal.Reason != "harness_unknown" {
				t.Fatalf("route kind %q did not fail closed: %+v", tc.kind, result)
			}
			after, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			if profileCalls != 0 || readinessCalls != 0 || capacityCalls != 0 ||
				len(runner.creates) != 0 || len(after) != len(before) {
				t.Fatalf(
					"route kind %q crossed mutation boundary: profile=%d readiness=%d capacity=%d creates=%d attempts=%d->%d",
					tc.kind,
					profileCalls,
					readinessCalls,
					capacityCalls,
					len(runner.creates),
					len(before),
					len(after),
				)
			}
		})
	}
}

func TestDelegateLaunchProfilePlanFromSnapshotRejectsMissingOrUnknownProviderRouteKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		kind    string
		missing bool
	}{
		{name: "missing", missing: true},
		{name: "unknown", kind: "infer_from_route"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot map[string]any
			if err := json.Unmarshal(testCompatibleDelegateLaunchProfileResolution().Snapshot, &snapshot); err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				delete(snapshot, "provider_route_kind")
			} else {
				snapshot["provider_route_kind"] = tc.kind
			}
			raw, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := delegateLaunchProfilePlanFromSnapshot(raw); err == nil ||
				!strings.Contains(err.Error(), "provider_route_kind") {
				t.Fatalf("provider route kind %q was accepted: %v", tc.kind, err)
			}
		})
	}
}

func TestDelegateExecutionProfileRefusesUnknownBeforeReadinessCapacityMintOrCreate(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	readinessCalls := 0
	capacityCalls := 0
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return nil, errTestExecutionProfileUnknown
	}
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:           workID,
		Harness:          "claude_code",
		Command:          "claude",
		Workspace:        t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{ProfileID: "missing"},
		Now:              fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "execution_profile_unresolved" {
		t.Fatalf("unexpected refusal: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if readinessCalls != 0 || capacityCalls != 0 || len(runner.creates) != 0 || len(after) != len(before) {
		t.Fatalf("unknown profile crossed mutation boundary: readiness=%d capacity=%d creates=%d attempts=%d->%d",
			readinessCalls, capacityCalls, len(runner.creates), len(before), len(after))
	}
	if countDelegateEventKind(t, store, "work.delegate.refused") != 0 {
		t.Fatal("unknown profile persisted a refusal event")
	}
}

func TestDelegateExecutionProfileRejectsNonCanonicalSnapshotBeforeReadinessOrMutation(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	for _, tc := range []struct {
		name     string
		snapshot json.RawMessage
	}{
		{name: "missing", snapshot: nil},
		{name: "invalid JSON", snapshot: json.RawMessage(`{"id":`)},
		{name: "oversized", snapshot: json.RawMessage(bytes.Repeat([]byte(" "), delegateExecutionProfileSnapshotMaxBytes+1))},
		{
			name:     "noncanonical schema whitespace",
			snapshot: json.RawMessage(`{"client_harness_id":"claude-code","id":"epplan_test1234","native_effort":{"requested":"xhigh"},"profile_id":"coding-implementation","schema_version":" compound.execution_profile_plan.v0.1 ","transport_id":"switchback"}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalProfileResolver := DelegateExecutionProfileResolver
			originalReadiness := DelegateHarnessReadiness
			originalCapacity := LaneCapacityGuard
			defer func() {
				DelegateExecutionProfileResolver = originalProfileResolver
				DelegateHarnessReadiness = originalReadiness
				LaneCapacityGuard = originalCapacity
			}()

			plan := testDelegateExecutionProfilePlan()
			plan.Snapshot = tc.snapshot
			DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
				return testCompatibleDelegateExecutionProfileResolution(plan), nil
			}
			readinessCalls := 0
			capacityCalls := 0
			DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
				readinessCalls++
				return DelegateHarnessReadinessResult{Ready: true}, nil
			}
			LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
				capacityCalls++
				return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
			}

			store := newExperimentalDelegateStore(t)
			workID := prepareClaimedWork(t, store, "codex", "", "")
			before, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			runner := newFakeRunner()
			result, err := store.Delegate(DelegateOptions{
				WorkID:           workID,
				Harness:          "claude_code",
				Command:          "claude",
				Workspace:        t.TempDir(),
				ExecutionProfile: DelegateExecutionProfileIntent{ProfileID: plan.ProfileID},
				Now:              fixedNow,
			}, runner)
			if err != nil {
				t.Fatalf("noncanonical snapshot returned infrastructure error: %v", err)
			}
			if result.Refusal == nil || result.Refusal.Reason != "execution_profile_unresolved" {
				t.Fatalf("unexpected refusal: %+v", result)
			}
			after, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			if readinessCalls != 0 || capacityCalls != 0 || len(runner.creates) != 0 || len(after) != len(before) {
				t.Fatalf("noncanonical profile crossed mutation boundary: readiness=%d capacity=%d creates=%d attempts=%d->%d",
					readinessCalls, capacityCalls, len(runner.creates), len(before), len(after))
			}
			if events := capturedDelegateEvents(t, store); len(events) != 0 {
				t.Fatalf("noncanonical profile persisted delegate events: %+v", events)
			}
		})
	}
}

func TestDelegateExecutionProfileRejectsIncompatibleSelectedTupleBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		selectedModelID       string
		selectedProviderRoute string
		selectedTransportID   string
	}{
		{
			name:                  "model mismatch",
			selectedModelID:       "fixture/other",
			selectedProviderRoute: "auto/build",
			selectedTransportID:   "switchback",
		},
		{
			name:                  "provider route mismatch",
			selectedModelID:       "fixture/model",
			selectedProviderRoute: "auto/judge",
			selectedTransportID:   "switchback",
		},
		{
			name:                  "transport mismatch",
			selectedModelID:       "fixture/model",
			selectedProviderRoute: "auto/build",
			selectedTransportID:   "native",
		},
		{
			name:                  "missing transport",
			selectedModelID:       "fixture/model",
			selectedProviderRoute: "auto/build",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installTestDelegateExecutionProfileHarnessTuple(
				t,
				tc.selectedModelID,
				tc.selectedProviderRoute,
				tc.selectedTransportID,
			)
			originalProfileResolver := DelegateExecutionProfileResolver
			originalReadiness := DelegateHarnessReadiness
			originalCapacity := LaneCapacityGuard
			defer func() {
				DelegateExecutionProfileResolver = originalProfileResolver
				DelegateHarnessReadiness = originalReadiness
				LaneCapacityGuard = originalCapacity
			}()

			plan := testDelegateExecutionProfilePlan()
			DelegateExecutionProfileResolver = func(req DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
				if req.SelectedModelID != tc.selectedModelID ||
					req.SelectedProviderRoute != tc.selectedProviderRoute ||
					req.SelectedTransportID != tc.selectedTransportID {
					t.Fatalf("resolver request lost selected tuple: got %+v", req)
				}
				return testDelegateExecutionProfileResolutionForTuple(
					plan,
					req.SelectedHarness,
					req.SelectedLaneID,
					req.SelectedModelID,
					req.SelectedProviderRoute,
					req.SelectedTransportID,
					DelegateExecutionProfileIncompatible,
				), nil
			}
			readinessCalls := 0
			capacityCalls := 0
			DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
				readinessCalls++
				return DelegateHarnessReadinessResult{Ready: true}, nil
			}
			LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
				capacityCalls++
				return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "available"}}, nil
			}

			store := newExperimentalDelegateStore(t)
			workID := prepareClaimedWork(t, store, "codex", "", "")
			before, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			runner := newFakeRunner()
			result, err := store.Delegate(DelegateOptions{
				WorkID:    workID,
				Harness:   "claude_code",
				Command:   "claude",
				Workspace: t.TempDir(),
				ExecutionProfile: DelegateExecutionProfileIntent{
					ProfileID: plan.ProfileID,
				},
				Now: fixedNow,
			}, runner)
			if err != nil {
				t.Fatal(err)
			}
			if result.Refusal == nil || result.Refusal.Reason != "execution_profile_incompatible" {
				t.Fatalf("unexpected refusal: %+v", result)
			}
			after, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			if readinessCalls != 0 || capacityCalls != 0 || len(runner.creates) != 0 || len(after) != len(before) {
				t.Fatalf(
					"incompatible selected tuple crossed activation boundary: readiness=%d capacity=%d creates=%d attempts=%d->%d",
					readinessCalls,
					capacityCalls,
					len(runner.creates),
					len(before),
					len(after),
				)
			}
			if events := capturedDelegateEvents(t, store); len(events) != 0 {
				t.Fatalf("incompatible selected tuple persisted delegate events: %+v", events)
			}
		})
	}
}

func TestDelegateExecutionProfileRejectsHarnessFamilyMismatchBeforeMutation(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	plan.ClientHarnessID = "codex"
	plan.Snapshot = json.RawMessage(`{"client_harness_id":"codex","id":"epplan_test1234","native_effort":{"disposition":"translated","reason":"mapped through selected client","requested":"xhigh"},"profile_id":"coding-implementation","schema_version":"compound.execution_profile_plan.v0.1","transport_id":"switchback"}`)
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testDelegateExecutionProfileResolution(plan, "claude_code", "claude_code", DelegateExecutionProfileIncompatible), nil
	}
	readinessCalls := 0
	capacityCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:           workID,
		Harness:          "claude_code",
		Command:          "claude",
		Workspace:        t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{ProfileID: plan.ProfileID},
		Now:              fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "execution_profile_incompatible" {
		t.Fatalf("unexpected refusal: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if readinessCalls != 0 || capacityCalls != 0 || len(runner.creates) != 0 || len(after) != len(before) {
		t.Fatalf("incompatible profile crossed activation boundary: readiness=%d capacity=%d creates=%d attempts=%d->%d",
			readinessCalls, capacityCalls, len(runner.creates), len(before), len(after))
	}
	if events := capturedDelegateEvents(t, store); len(events) != 0 {
		t.Fatalf("incompatible profile persisted delegate events: %+v", events)
	}
}

func TestDelegateExecutionProfileRejectsUnknownCodexLookalikeBeforeMutation(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	plan.ClientHarnessID = "codex"
	plan.Snapshot = json.RawMessage(`{"client_harness_id":"codex","id":"epplan_test1234","native_effort":{"requested":"xhigh"},"profile_id":"coding-implementation","schema_version":"compound.execution_profile_plan.v0.1","transport_id":"switchback"}`)
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testDelegateExecutionProfileResolution(plan, "codex-lookalike", "codex-lookalike", DelegateExecutionProfileIncompatible), nil
	}
	readinessCalls := 0
	capacityCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:           workID,
		Harness:          "codex-lookalike",
		Command:          "codex",
		Workspace:        t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{ProfileID: plan.ProfileID},
		Now:              fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "execution_profile_incompatible" {
		t.Fatalf("unknown lookalike was accepted: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if readinessCalls != 0 || capacityCalls != 0 || len(runner.creates) != 0 || len(after) != len(before) {
		t.Fatalf("lookalike profile crossed mutation boundary: readiness=%d capacity=%d creates=%d attempts=%d->%d",
			readinessCalls, capacityCalls, len(runner.creates), len(before), len(after))
	}
	if events := capturedDelegateEvents(t, store); len(events) != 0 {
		t.Fatalf("lookalike profile persisted delegate events: %+v", events)
	}
}

func TestDelegateExecutionProfileAuthorityDriftRefusesBeforeMintCreateOrPersistence(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	admittedPlan := testDelegateExecutionProfilePlan()
	driftedPlan := cloneDelegateExecutionProfilePlan(admittedPlan)
	driftedPlan.Snapshot = json.RawMessage(`{"budget":{"max_requests":20},"client_harness_id":"claude-code","id":"epplan_test1234","native_effort":{"disposition":"translated","reason":"mapped through selected client","requested":"xhigh"},"profile_id":"coding-implementation","schema_version":"compound.execution_profile_plan.v0.1","transport_id":"switchback","warnings":["authority drift"]}`)
	resolverCalls := 0
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		resolverCalls++
		if resolverCalls == 1 {
			return testCompatibleDelegateExecutionProfileResolution(admittedPlan), nil
		}
		return testCompatibleDelegateExecutionProfileResolution(driftedPlan), nil
	}
	readinessCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	var capacityReadOnly []bool
	LaneCapacityGuard = func(req LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityReadOnly = append(capacityReadOnly, req.ReadOnly)
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:           workID,
		Harness:          "claude_code",
		Command:          "claude",
		Workspace:        t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{ProfileID: admittedPlan.ProfileID},
		Now:              fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("unexpected refusal: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 2 || readinessCalls != 2 {
		t.Fatalf("authority was not checked at admission and pre-mint: resolver=%d readiness=%d", resolverCalls, readinessCalls)
	}
	if len(capacityReadOnly) != 2 || capacityReadOnly[0] || !capacityReadOnly[1] {
		t.Fatalf("capacity checks = %v, want admission mutation check then evidence-only recheck", capacityReadOnly)
	}
	if len(runner.creates) != 0 || len(after) != len(before) {
		t.Fatalf("authority drift crossed activation boundary: creates=%d attempts=%d->%d", len(runner.creates), len(before), len(after))
	}
	if events := capturedDelegateEvents(t, store); len(events) != 0 {
		t.Fatalf("authority drift persisted delegate events: %+v", events)
	}
}

func TestDelegateExecutionProfileUnchangedSnapshotWithDTOIdentityDriftRefusesBeforeMint(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	for _, tc := range []struct {
		name   string
		mutate func(*DelegateExecutionProfileResolution)
	}{
		{name: "plan id", mutate: func(resolution *DelegateExecutionProfileResolution) {
			resolution.Compatibility.PlanID = "epplan_tampered"
		}},
		{name: "plan id surrounding whitespace", mutate: func(resolution *DelegateExecutionProfileResolution) {
			resolution.Compatibility.PlanID = " " + resolution.Compatibility.PlanID + " "
		}},
		{name: "selected model", mutate: func(resolution *DelegateExecutionProfileResolution) {
			resolution.Compatibility.SelectedModelID = "fixture/other"
		}},
		{name: "selected provider route", mutate: func(resolution *DelegateExecutionProfileResolution) {
			resolution.Compatibility.SelectedProviderRoute = "auto/judge"
		}},
		{name: "selected transport", mutate: func(resolution *DelegateExecutionProfileResolution) {
			resolution.Compatibility.SelectedTransportID = "native"
		}},
		{name: "client harness", mutate: func(resolution *DelegateExecutionProfileResolution) {
			resolution.Compatibility.ClientHarnessID = "codex"
		}},
		{name: "canonical transport", mutate: func(resolution *DelegateExecutionProfileResolution) {
			resolution.Compatibility.TransportID = "native"
		}},
		{name: "native effort", mutate: func(resolution *DelegateExecutionProfileResolution) {
			resolution.Compatibility.RequestedNativeEffort = "low"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalProfileResolver := DelegateExecutionProfileResolver
			originalReadiness := DelegateHarnessReadiness
			originalCapacity := LaneCapacityGuard
			defer func() {
				DelegateExecutionProfileResolver = originalProfileResolver
				DelegateHarnessReadiness = originalReadiness
				LaneCapacityGuard = originalCapacity
			}()

			admittedPlan := testDelegateExecutionProfilePlan()
			resolverCalls := 0
			DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
				resolverCalls++
				if resolverCalls == 1 {
					return testCompatibleDelegateExecutionProfileResolution(admittedPlan), nil
				}
				drifted := testCompatibleDelegateExecutionProfileResolution(admittedPlan)
				tc.mutate(drifted)
				return drifted, nil
			}
			DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
				return DelegateHarnessReadinessResult{Ready: true}, nil
			}
			LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

			store := newExperimentalDelegateStore(t)
			workID := prepareClaimedWork(t, store, "codex", "", "")
			before, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			runner := newFakeRunner()
			result, err := store.Delegate(DelegateOptions{
				WorkID:           workID,
				Harness:          "claude_code",
				Command:          "claude",
				Workspace:        t.TempDir(),
				ExecutionProfile: DelegateExecutionProfileIntent{ProfileID: admittedPlan.ProfileID},
				Now:              fixedNow,
			}, runner)
			if err != nil {
				t.Fatal(err)
			}
			if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
				t.Fatalf("unchanged canonical body with %s drift was accepted: %+v", tc.name, result)
			}
			after, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			if len(runner.creates) != 0 || len(after) != len(before) {
				t.Fatalf("DTO drift crossed activation boundary: creates=%d attempts=%d->%d", len(runner.creates), len(before), len(after))
			}
			if events := capturedDelegateEvents(t, store); len(events) != 0 {
				t.Fatalf("DTO drift persisted delegate events: %+v", events)
			}
		})
	}
}

func TestDelegateExecutionProfileRoundTripsBindingReadinessAndRunAttempt(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	resolverCalls := 0
	DelegateExecutionProfileResolver = func(req DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		resolverCalls++
		if req.Intent.ProfileID != plan.ProfileID ||
			req.Intent.RouteAlias != "auto/build" ||
			req.Intent.NativeEffort != "xhigh" ||
			req.SelectedHarness != "claude_code" ||
			req.SelectedLaneID != "claude_code" ||
			req.SelectedModelID != "fixture/model" ||
			req.SelectedProviderRoute != "auto/build" ||
			req.SelectedTransportID != "switchback" {
			t.Fatalf("resolver request lost selectors: %+v", req)
		}
		return testCompatibleDelegateExecutionProfileResolution(plan), nil
	}
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{
			Ready: true,
			Evidence: []DelegateHarnessReadinessEvidence{{
				Kind: "harness",
				Ref:  "harness://claude_code",
			}},
		}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_code",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:    plan.ProfileID,
			RouteAlias:   "auto/build",
			NativeEffort: "xhigh",
		},
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" || result.ExecutionProfile == nil || result.ExecutionProfile.ID != plan.ID {
		t.Fatalf("profile missing from result: %+v", result)
	}
	if resolverCalls != 2 {
		t.Fatalf("execution profile resolved %d time(s), want admission plus pre-mint revalidation", resolverCalls)
	}
	if result.Binding == nil || result.Binding.ExecutionProfile == nil || result.Binding.ExecutionProfile.ID != plan.ID {
		t.Fatalf("profile missing from binding: %+v", result.Binding)
	}
	event, err := jsonFile(t, store, result.EventID)
	if err != nil {
		t.Fatal(err)
	}
	eventProfile, ok := event["execution_profile"].(map[string]any)
	if !ok || eventProfile["id"] != plan.ID {
		t.Fatalf("profile missing from spawned event: %+v", event)
	}
	profileProof := result.Readiness.ExecutionProfile
	if profileProof == nil ||
		profileProof.Ref != "execution-profile://"+plan.ID ||
		!bytes.Equal(profileProof.Snapshot, plan.Snapshot) ||
		strings.TrimSpace(profileProof.Revision) == "" ||
		strings.TrimSpace(result.Readiness.SnapshotRevision) == "" {
		t.Fatalf("profile absent from canonical readiness snapshot: %+v", result.Readiness)
	}

	recovered, alive, err := store.RecoverBoundSession(workID)
	if err != nil {
		t.Fatal(err)
	}
	if !alive || recovered.ExecutionProfile == nil || recovered.ExecutionProfile.ID != plan.ID {
		t.Fatalf("profile did not recover through event binding: alive=%v binding=%+v", alive, recovered)
	}
	if recovered.Readiness == nil ||
		recovered.Readiness.ExecutionProfile == nil ||
		!bytes.Equal(recovered.Readiness.ExecutionProfile.Snapshot, plan.Snapshot) {
		t.Fatalf("canonical execution profile body did not survive event recovery: %+v", recovered.Readiness)
	}

	attempt, err := runattempt.Open(store.home).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionProfilePlanID != plan.ID ||
		attempt.RequestedClientHarnessID != plan.ClientHarnessID ||
		attempt.RequestedTransportID != plan.TransportID ||
		attempt.RequestedNativeEffort != plan.RequestedNativeEffort {
		t.Fatalf("RunAttempt lost requested profile identity: %+v", attempt)
	}
	if attempt.ModelID != "" || attempt.ProviderRoute != "" {
		t.Fatalf("intent stamped observed fields: %+v", attempt)
	}
}

func TestDelegateExecutionProfilePassesExactWorkAndAttemptIdentityToSupervisedLaunch(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testCompatibleDelegateExecutionProfileResolution(plan), nil
	}
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:           workID,
		Harness:          "claude_code",
		Command:          "claude",
		Workspace:        t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{ProfileID: plan.ProfileID},
		Now:              fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" || result.RunAttemptID == "" {
		t.Fatalf("delegate did not spawn one governed attempt: %+v", result)
	}
	if len(runner.creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(runner.creates))
	}
	environment := runner.creates[0].Environment
	for key, want := range map[string]string{
		"COMPOUND_WORK_ID":                    workID,
		"COMPOUND_RUN_ATTEMPT_ID":             result.RunAttemptID,
		"COMPOUND_EXECUTION_PROFILE_PLAN_ID":  plan.ID,
		"COMPOUND_EXECUTION_PROFILE_REVISION": result.Readiness.ExecutionProfile.Revision,
	} {
		if got := environment[key]; got != want {
			t.Fatalf("launch environment %s = %q, want %q; environment=%+v", key, got, want, environment)
		}
	}
}

func TestDelegateLaunchProfileWithoutOwnerResolverRefusesBeforeReadinessCapacityMintOrCreate(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testCompatibleDelegateExecutionProfileResolution(plan), nil
	}
	readinessCalls := 0
	capacityCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_code",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "launch_profile_unresolved" {
		t.Fatalf("missing owner resolver did not fail closed: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if readinessCalls != 0 || capacityCalls != 0 || len(runner.creates) != 0 || len(after) != len(before) {
		t.Fatalf(
			"missing owner resolver crossed mutation boundary: readiness=%d capacity=%d creates=%d attempts=%d->%d",
			readinessCalls,
			capacityCalls,
			len(runner.creates),
			len(before),
			len(after),
		)
	}
}

func TestDelegateLaunchProfileRejectsOwnerDriftOrMissingClientBeforeReadinessCapacityMintOrCreate(t *testing.T) {
	tests := []struct {
		name     string
		resolver DelegateLaunchProfileResolverFunc
	}{
		{
			name: "generated artifact drift",
			resolver: func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
				return nil, errors.New("owner generated artifacts drift")
			},
		},
		{
			name: "missing client profile",
			resolver: func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
				resolution := testCompatibleDelegateLaunchProfileResolution()
				resolution.Snapshot = json.RawMessage(`{"authority_revision":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","capture_mode":"segmented_full_wire","client_profile_id":"","executable_id":"claude","harness":"claude-code","id":"claude-minimax","owner_transport":"tap","provider_lane":"minimax","requested_effort":"xhigh","requested_model":"fixture/model","revision":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","route":"auto/build","schema_version":"compound.delegate_launch_profile_plan.v0.1","source_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","source_schema":"switchback/launch-profile-plan@1","transport_id":"switchback"}`)
				return resolution, nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installTestDelegateExecutionProfileHarness(t)
			originalProfileResolver := DelegateExecutionProfileResolver
			originalLaunchResolver := DelegateLaunchProfileResolver
			originalReadiness := DelegateHarnessReadiness
			originalCapacity := LaneCapacityGuard
			defer func() {
				DelegateExecutionProfileResolver = originalProfileResolver
				DelegateLaunchProfileResolver = originalLaunchResolver
				DelegateHarnessReadiness = originalReadiness
				LaneCapacityGuard = originalCapacity
			}()

			plan := testDelegateExecutionProfilePlan()
			DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
				return testCompatibleDelegateExecutionProfileResolution(plan), nil
			}
			DelegateLaunchProfileResolver = tc.resolver
			readinessCalls := 0
			capacityCalls := 0
			DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
				readinessCalls++
				return DelegateHarnessReadinessResult{Ready: true}, nil
			}
			LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
				capacityCalls++
				return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
			}

			store := newExperimentalDelegateStore(t)
			workID := prepareClaimedWork(t, store, "codex", "", "")
			before, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			runner := newFakeRunner()
			result, err := store.Delegate(DelegateOptions{
				WorkID:    workID,
				Harness:   "claude_code",
				Command:   "claude",
				Workspace: t.TempDir(),
				ExecutionProfile: DelegateExecutionProfileIntent{
					ProfileID:       plan.ProfileID,
					LaunchProfileID: "claude-minimax",
				},
				Now: fixedNow,
			}, runner)
			if err != nil {
				t.Fatal(err)
			}
			if result.Refusal == nil || result.Refusal.Reason != "launch_profile_unresolved" {
				t.Fatalf("invalid owner proof did not fail closed: %+v", result)
			}
			after, err := runattempt.Open(store.home).List()
			if err != nil {
				t.Fatal(err)
			}
			if readinessCalls != 0 || capacityCalls != 0 || len(runner.creates) != 0 || len(after) != len(before) {
				t.Fatalf(
					"invalid owner proof crossed mutation boundary: readiness=%d capacity=%d creates=%d attempts=%d->%d",
					readinessCalls,
					capacityCalls,
					len(runner.creates),
					len(before),
					len(after),
				)
			}
		})
	}
}

func TestDelegateLaunchProfileRejectsForgedCompatibleClientBeforeReadinessCapacityMintOrCreate(t *testing.T) {
	installTestDelegateExecutionProfileProviderClassHarnessTuple(t, "claude-minimax (token plan)", "minimax", "switchback")
	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testDelegateExecutionProfileResolutionForTuple(
			plan,
			"claude_minimax",
			"claude_minimax",
			"claude-minimax (token plan)",
			"minimax",
			"switchback",
			DelegateExecutionProfileCompatible,
		), nil
	}
	DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		resolution := testCompatibleProviderClassDelegateLaunchProfileResolution()
		resolution.Snapshot = bytes.Replace(
			resolution.Snapshot,
			[]byte(`"harness":"claude-code"`),
			[]byte(`"harness":"codex"`),
			1,
		)
		resolution.Compatibility.SelectedHarness = "claude_minimax"
		resolution.Compatibility.SelectedLaneID = "claude_minimax"
		resolution.Compatibility.SelectedModelID = "claude-minimax (token plan)"
		resolution.Compatibility.SelectedProviderRoute = "minimax"
		resolution.Compatibility.ClientHarnessID = "codex"
		return resolution, nil
	}
	readinessCalls := 0
	capacityCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_minimax",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "launch_profile_incompatible" {
		t.Fatalf("forged compatible owner client did not fail closed: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if readinessCalls != 0 || capacityCalls != 0 || len(runner.creates) != 0 || len(after) != len(before) {
		t.Fatalf(
			"forged compatible owner crossed mutation boundary: readiness=%d capacity=%d creates=%d attempts=%d->%d",
			readinessCalls,
			capacityCalls,
			len(runner.creates),
			len(before),
			len(after),
		)
	}
}

func TestDelegateLaunchProfileRevalidationRejectsForgedCompatibleClientBeforeReadinessCapacityOrMint(t *testing.T) {
	installTestDelegateExecutionProfileProviderClassHarnessTuple(t, "claude-minimax (token plan)", "minimax", "switchback")
	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testDelegateExecutionProfileResolutionForTuple(
			plan,
			"claude_minimax",
			"claude_minimax",
			"claude-minimax (token plan)",
			"minimax",
			"switchback",
			DelegateExecutionProfileCompatible,
		), nil
	}
	launchResolverCalls := 0
	DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		launchResolverCalls++
		resolution := testCompatibleProviderClassDelegateLaunchProfileResolution()
		resolution.Compatibility.SelectedHarness = "claude_minimax"
		resolution.Compatibility.SelectedLaneID = "claude_minimax"
		resolution.Compatibility.SelectedModelID = "claude-minimax (token plan)"
		resolution.Compatibility.SelectedProviderRoute = "minimax"
		if launchResolverCalls == 2 {
			resolution.Snapshot = bytes.Replace(
				resolution.Snapshot,
				[]byte(`"harness":"claude-code"`),
				[]byte(`"harness":"codex"`),
				1,
			)
			resolution.Compatibility.ClientHarnessID = "codex"
		}
		return resolution, nil
	}
	readinessCalls := 0
	capacityCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_minimax",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("forged compatible revalidation did not fail closed: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if launchResolverCalls != 2 ||
		readinessCalls != 1 ||
		capacityCalls != 1 ||
		len(runner.creates) != 0 ||
		len(after) != len(before) {
		t.Fatalf(
			"forged revalidation crossed pre-mint boundary: launch=%d readiness=%d capacity=%d creates=%d attempts=%d->%d",
			launchResolverCalls,
			readinessCalls,
			capacityCalls,
			len(runner.creates),
			len(before),
			len(after),
		)
	}
}

func TestDelegateLaunchProfileRevalidationRejectsProviderRouteKindFlipBeforeMintOrCreate(t *testing.T) {
	installTestDelegateExecutionProfileProviderClassHarnessTuple(t, "claude-minimax (token plan)", "minimax", "switchback")
	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testDelegateExecutionProfileResolutionForTuple(
			plan,
			"claude_minimax",
			"claude_minimax",
			"claude-minimax (token plan)",
			"minimax",
			"switchback",
			DelegateExecutionProfileCompatible,
		), nil
	}
	launchResolverCalls := 0
	DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		launchResolverCalls++
		resolution := testCompatibleProviderClassDelegateLaunchProfileResolution()
		resolution.Compatibility.SelectedHarness = "claude_minimax"
		resolution.Compatibility.SelectedLaneID = "claude_minimax"
		resolution.Compatibility.SelectedModelID = "claude-minimax (token plan)"
		if launchResolverCalls == 2 {
			resolution.Snapshot = bytes.Replace(
				resolution.Snapshot,
				[]byte(`"provider_route_kind":"provider_class"`),
				[]byte(`"provider_route_kind":"direct_model"`),
				1,
			)
			resolution.Compatibility.SelectedProviderRouteKind = "direct_model"
		}
		return resolution, nil
	}
	readinessCalls := 0
	capacityCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_minimax",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("provider route kind revalidation drift did not fail closed: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if launchResolverCalls != 2 ||
		readinessCalls != 1 ||
		capacityCalls != 1 ||
		len(runner.creates) != 0 ||
		len(after) != len(before) {
		t.Fatalf(
			"route-kind revalidation crossed pre-mint boundary: launch=%d readiness=%d capacity=%d creates=%d attempts=%d->%d",
			launchResolverCalls,
			readinessCalls,
			capacityCalls,
			len(runner.creates),
			len(before),
			len(after),
		)
	}
}

func TestDelegateRevalidatesModelTupleProviderRouteKindOriginBeforeMint(t *testing.T) {
	originalHarnessResolver := DelegateHarnessResolver
	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateHarnessResolver = originalHarnessResolver
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	harnessCalls := 0
	DelegateHarnessResolver = func(req DelegateHarnessResolveRequest) (DelegateHarnessResolution, error) {
		harnessCalls++
		kind := "provider_class"
		if harnessCalls > 1 {
			kind = "direct_model"
		}
		return DelegateHarnessResolution{
			Harness:                    strings.TrimSpace(req.Harness),
			Command:                    strings.TrimSpace(req.CommandOverride),
			LaneID:                     strings.TrimSpace(req.Harness),
			RequestedModelID:           "claude-minimax (token plan)",
			RequestedProviderRoute:     "minimax",
			RequestedProviderRouteKind: kind,
			SelectedTransportID:        "switchback",
			CapacityLane:               strings.TrimSpace(req.Harness),
			TupleBacked:                true,
		}, nil
	}
	plan := testDelegateExecutionProfilePlan()
	profileCalls := 0
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		profileCalls++
		return testDelegateExecutionProfileResolutionForTuple(
			plan,
			"claude_minimax",
			"claude_minimax",
			"claude-minimax (token plan)",
			"minimax",
			"switchback",
			DelegateExecutionProfileCompatible,
		), nil
	}
	launchCalls := 0
	DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		launchCalls++
		resolution := testCompatibleProviderClassDelegateLaunchProfileResolution()
		resolution.Compatibility.SelectedHarness = "claude_minimax"
		resolution.Compatibility.SelectedLaneID = "claude_minimax"
		resolution.Compatibility.SelectedModelID = "claude-minimax (token plan)"
		return resolution, nil
	}
	readinessCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	capacityCalls := 0
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_minimax",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("changed model tuple origin did not fail closed: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if harnessCalls != 2 ||
		profileCalls != 1 ||
		launchCalls != 1 ||
		readinessCalls != 1 ||
		capacityCalls != 1 ||
		len(runner.creates) != 0 ||
		len(after) != len(before) {
		t.Fatalf(
			"model tuple drift crossed pre-mint boundary: harness=%d profile=%d launch=%d readiness=%d capacity=%d creates=%d attempts=%d->%d",
			harnessCalls,
			profileCalls,
			launchCalls,
			readinessCalls,
			capacityCalls,
			len(runner.creates),
			len(before),
			len(after),
		)
	}
}

func TestDelegateRefusesModelTupleResolverErrorBeforeMint(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	stableHarnessResolver := DelegateHarnessResolver
	harnessCalls := 0
	DelegateHarnessResolver = func(req DelegateHarnessResolveRequest) (DelegateHarnessResolution, error) {
		harnessCalls++
		if harnessCalls > 1 {
			return DelegateHarnessResolution{}, errors.New("model-router unavailable")
		}
		return stableHarnessResolver(req)
	}
	t.Cleanup(func() {
		DelegateHarnessResolver = stableHarnessResolver
	})

	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	profileCalls := 0
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		profileCalls++
		return testCompatibleDelegateExecutionProfileResolution(plan), nil
	}
	launchCalls := 0
	DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		launchCalls++
		return testCompatibleDelegateLaunchProfileResolution(), nil
	}
	readinessCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	capacityCalls := 0
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ready"}}, nil
	}

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	before, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_code",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("model tuple resolver error did not fail closed: %+v", result)
	}
	after, err := runattempt.Open(store.home).List()
	if err != nil {
		t.Fatal(err)
	}
	if harnessCalls != 2 ||
		profileCalls != 1 ||
		launchCalls != 1 ||
		readinessCalls != 1 ||
		capacityCalls != 1 ||
		len(runner.creates) != 0 ||
		len(after) != len(before) {
		t.Fatalf(
			"model tuple resolver error crossed pre-mint boundary: harness=%d profile=%d launch=%d readiness=%d capacity=%d creates=%d attempts=%d->%d",
			harnessCalls,
			profileCalls,
			launchCalls,
			readinessCalls,
			capacityCalls,
			len(runner.creates),
			len(before),
			len(after),
		)
	}
}

func TestValidateDelegateLaunchProfileResolutionUsesOnlyDeclaredProviderRouteAxis(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		profileID       string
		selectedModel   string
		selectedRoute   string
		selectedKind    string
		providerLane    string
		ownerRoute      string
		ownerModel      string
		clientProfileID string
		wantKindError   bool
		wantCompatible  bool
	}{
		{
			name:            "provider class accepts provider lane",
			profileID:       "claude-fixture-class",
			selectedModel:   "fixture token plan",
			selectedRoute:   "fixture-provider",
			selectedKind:    "provider_class",
			providerLane:    "fixture-provider",
			ownerRoute:      "fixture-provider/model-primary",
			ownerModel:      "model-primary",
			clientProfileID: "claude-fixture-class-default",
			wantCompatible:  true,
		},
		{
			name:            "direct model accepts concrete route",
			profileID:       "claude-fixture-direct",
			selectedModel:   "fixture direct model",
			selectedRoute:   "provider.example/model-primary",
			selectedKind:    "direct_model",
			providerLane:    "fixture-provider",
			ownerRoute:      "provider.example/model-primary",
			ownerModel:      "model-primary",
			clientProfileID: "claude-fixture-direct-default",
			wantCompatible:  true,
		},
		{
			name:            "provider class rejects concrete route cross-kind",
			profileID:       "claude-fixture-class",
			selectedModel:   "fixture token plan",
			selectedRoute:   "fixture-provider/model-primary",
			selectedKind:    "provider_class",
			providerLane:    "fixture-provider",
			ownerRoute:      "fixture-provider/model-primary",
			ownerModel:      "model-primary",
			clientProfileID: "claude-fixture-class-default",
			wantCompatible:  false,
		},
		{
			name:            "direct model rejects provider lane cross-kind",
			profileID:       "claude-fixture-direct",
			selectedModel:   "fixture direct model",
			selectedRoute:   "fixture-provider",
			selectedKind:    "direct_model",
			providerLane:    "fixture-provider",
			ownerRoute:      "provider.example/model-primary",
			ownerModel:      "model-primary",
			clientProfileID: "claude-fixture-direct-default",
			wantCompatible:  false,
		},
		{
			name:            "missing kind does not infer direct model from slash",
			profileID:       "claude-fixture-direct",
			selectedModel:   "fixture direct model",
			selectedRoute:   "provider.example/model-primary",
			selectedKind:    "",
			providerLane:    "fixture-provider",
			ownerRoute:      "provider.example/model-primary",
			ownerModel:      "model-primary",
			clientProfileID: "claude-fixture-direct-default",
			wantKindError:   true,
			wantCompatible:  false,
		},
		{
			name:            "unknown kind refuses",
			profileID:       "claude-fixture-direct",
			selectedModel:   "fixture direct model",
			selectedRoute:   "provider.example/model-primary",
			selectedKind:    "guess_from_requested_model",
			providerLane:    "fixture-provider",
			ownerRoute:      "provider.example/model-primary",
			ownerModel:      "provider.example/model-primary",
			clientProfileID: "claude-fixture-direct-default",
			wantKindError:   true,
			wantCompatible:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolution := testCompatibleDelegateLaunchProfileResolution()
			var snapshot map[string]any
			if err := json.Unmarshal(resolution.Snapshot, &snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot["id"] = tc.profileID
			snapshot["provider_lane"] = tc.providerLane
			snapshot["provider_route_kind"] = tc.selectedKind
			snapshot["route"] = tc.ownerRoute
			snapshot["requested_model"] = tc.ownerModel
			snapshot["client_profile_id"] = tc.clientProfileID
			raw, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			resolution.Snapshot = raw
			resolution.Compatibility.SelectedHarness = "claude_code"
			resolution.Compatibility.SelectedLaneID = "claude_code"
			resolution.Compatibility.SelectedModelID = tc.selectedModel
			resolution.Compatibility.SelectedProviderRoute = tc.selectedRoute
			resolution.Compatibility.SelectedProviderRouteKind = tc.selectedKind
			resolution.Compatibility.PlanID = tc.profileID
			resolution.Compatibility.ClientProfileID = tc.clientProfileID

			_, err = validateDelegateLaunchProfileResolution(
				resolution,
				"claude-code",
				"claude_code",
				"claude_code",
				tc.selectedModel,
				tc.selectedRoute,
				tc.selectedKind,
				"switchback",
				"xhigh",
				"claude",
			)
			if tc.wantKindError {
				if err == nil || !strings.Contains(err.Error(), "provider_route_kind") {
					t.Fatalf("provider route kind error = %v, want provider_route_kind", err)
				}
				return
			}
			if tc.wantCompatible && err != nil {
				t.Fatalf("conformant route axes rejected: %v", err)
			}
			if !tc.wantCompatible {
				var incompatible *delegateLaunchProfileIncompatibleError
				if !errors.As(err, &incompatible) {
					t.Fatalf("route-kind mismatch error = %v, want typed incompatible", err)
				}
			}
		})
	}
}

func TestDelegateLaunchProfileBindsOwnerPlanToEnvironmentAndRunAttempt(t *testing.T) {
	installTestDelegateExecutionProfileProviderClassHarnessTuple(t, "claude-minimax (token plan)", "minimax", "switchback")
	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testDelegateExecutionProfileResolutionForTuple(
			plan,
			"claude_minimax",
			"claude_minimax",
			"claude-minimax (token plan)",
			"minimax",
			"switchback",
			DelegateExecutionProfileCompatible,
		), nil
	}
	launchResolverCalls := 0
	DelegateLaunchProfileResolver = func(req DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		launchResolverCalls++
		if req.ProfileID != "claude-minimax" ||
			req.SelectedHarness != "claude_minimax" ||
			req.SelectedLaneID != "claude_minimax" ||
			req.SelectedModelID != "claude-minimax (token plan)" ||
			req.SelectedProviderRoute != "minimax" ||
			req.SelectedTransportID != "switchback" ||
			req.RequestedNativeEffort != "xhigh" {
			t.Fatalf("launch profile owner request lost selected tuple: %+v", req)
		}
		resolution := testCompatibleProviderClassDelegateLaunchProfileResolution()
		resolution.Compatibility.SelectedHarness = "claude_minimax"
		resolution.Compatibility.SelectedLaneID = "claude_minimax"
		resolution.Compatibility.SelectedModelID = "claude-minimax (token plan)"
		resolution.Compatibility.SelectedProviderRoute = "minimax"
		return resolution, nil
	}
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	runner := newFakeRunner()
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_minimax",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" || launchResolverCalls != 2 {
		t.Fatalf("launch profile was not admitted and revalidated: calls=%d result=%+v", launchResolverCalls, result)
	}
	if len(runner.creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(runner.creates))
	}
	if got, want := runner.creates[0].Command, "/synthetic/bin/claude"; got != want {
		t.Fatalf("spawn command = %q, want owner wrapper %q", got, want)
	}
	for key, want := range map[string]string{
		"SB_LAUNCH_PROFILE_ID":       "claude-minimax",
		"SB_LAUNCH_PROFILE_REVISION": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"SB_LAUNCH_HARNESS":          "claude-code",
		"SB_LAUNCH_CAPTURE_POLICY":   "segmented_full_wire",
		"SB_LAUNCH_CLIENT_PROFILE":   "claude-minimax-default",
		"COMPOUND_CLIENT_PROFILE_ID": "claude-minimax-default",
	} {
		if got := runner.creates[0].Environment[key]; got != want {
			t.Fatalf("launch environment %s = %q, want %q; environment=%+v", key, got, want, runner.creates[0].Environment)
		}
	}
	resultValue := reflect.ValueOf(result)
	launchProfile := resultValue.FieldByName("LaunchProfile")
	if !launchProfile.IsValid() || launchProfile.IsNil() {
		t.Fatalf("result lost launch profile proof: %+v", result)
	}
	attempt, err := runattempt.Open(store.home).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	attemptValue := reflect.ValueOf(attempt)
	if got, want := attempt.Command, "/synthetic/bin/claude"; got != want {
		t.Fatalf("RunAttempt command = %q, want owner wrapper %q", got, want)
	}
	for field, want := range map[string]string{
		"LaunchProfileID":          "claude-minimax",
		"LaunchProfileRevision":    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"RequestedClientProfileID": "claude-minimax-default",
	} {
		value := attemptValue.FieldByName(field)
		if !value.IsValid() || value.String() != want {
			t.Fatalf("RunAttempt %s = %v, want %q; attempt=%+v", field, value, want, attempt)
		}
	}
	recovered, alive, err := store.RecoverBoundSession(workID)
	if err != nil {
		t.Fatal(err)
	}
	if !alive || recovered.LaunchProfile == nil ||
		!bytes.Equal(recovered.LaunchProfile.Snapshot, result.LaunchProfile.Snapshot) {
		t.Fatalf("launch profile did not survive event recovery: alive=%v binding=%+v", alive, recovered)
	}
}

func TestDelegateLaunchProfileSuppliesExecutorOwnedTransportAfterHarnessResolution(t *testing.T) {
	installTestDelegateExecutionProfileProviderClassHarnessTuple(t, "claude-minimax (token plan)", "minimax", "")
	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(req DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		if req.SelectedTransportID != "" {
			t.Fatalf("harness resolver guessed executor transport: %+v", req)
		}
		return testDelegateExecutionProfileResolutionForTuple(
			plan,
			"claude_minimax",
			"claude_minimax",
			"claude-minimax (token plan)",
			"minimax",
			"",
			DelegateExecutionProfileCompatible,
		), nil
	}
	DelegateLaunchProfileResolver = func(req DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		if req.SelectedTransportID != "switchback" {
			t.Fatalf("launch resolver did not receive the execution plan transport family: %+v", req)
		}
		resolution := testCompatibleProviderClassDelegateLaunchProfileResolution()
		resolution.Compatibility.SelectedHarness = "claude_minimax"
		resolution.Compatibility.SelectedLaneID = "claude_minimax"
		resolution.Compatibility.SelectedModelID = "claude-minimax (token plan)"
		resolution.Compatibility.SelectedProviderRoute = "minimax"
		return resolution, nil
	}
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_minimax",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" || result.LaunchProfile == nil ||
		result.LaunchProfile.TransportID != "switchback" ||
		result.LaunchProfile.OwnerTransport != "tap" {
		t.Fatalf("executor-owned transport did not complete the delegated tuple: %+v", result)
	}
}

func TestRecoverBoundSessionReadsLegacyV01LaunchProfileRecord(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testCompatibleDelegateExecutionProfileResolution(plan), nil
	}
	DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		return testCompatibleDelegateLaunchProfileResolution(), nil
	}
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_code",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" {
		t.Fatalf("fixture delegate did not spawn: %+v", result)
	}

	var persisted delegateEvent
	if err := store.readEventJSON(result.EventID, &persisted); err != nil {
		t.Fatal(err)
	}
	var legacySnapshot map[string]any
	if err := json.Unmarshal(persisted.LaunchProfile.Snapshot, &legacySnapshot); err != nil {
		t.Fatal(err)
	}
	delete(legacySnapshot, "provider_route_kind")
	legacySnapshot["schema_version"] = "compound.delegate_launch_profile_plan.v0.1"
	legacyBody, err := json.Marshal(legacySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	persisted.SchemaVersion = "compound.work_delegate_event.v0.1"
	persisted.SelectedProviderRoute = ""
	persisted.SelectedProviderRouteKind = ""
	persisted.LaunchProfile.SchemaVersion = "compound.delegate_launch_profile_plan.v0.1"
	persisted.LaunchProfile.ProviderRouteKind = ""
	persisted.LaunchProfile.Snapshot = legacyBody
	persisted.Readiness.LaunchProfile.Snapshot = append(json.RawMessage(nil), legacyBody...)
	sum := sha256.Sum256(legacyBody)
	persisted.Readiness.LaunchProfile.Revision = "sha256:" + hex.EncodeToString(sum[:])
	finalizedReadiness, err := finalizeDelegateReadinessSnapshot(*persisted.Readiness)
	if err != nil {
		t.Fatal(err)
	}
	persisted.Readiness = &finalizedReadiness
	if err := store.events.Remove(result.EventID); err != nil {
		t.Fatal(err)
	}
	if err := store.writeEventJSON(result.EventID, persisted); err != nil {
		t.Fatal(err)
	}

	binding, _, err := store.RecoverBoundSession(workID)
	if err != nil {
		t.Fatalf("legacy v0.1 launch-profile record became unreadable after upgrade: %v", err)
	}
	if binding == nil || binding.LaunchProfile == nil {
		t.Fatalf("legacy v0.1 launch-profile binding was not recovered: %+v", binding)
	}
	if binding.SelectedProviderRoute != "" || binding.SelectedProviderRouteKind != "" {
		t.Fatalf("legacy binding invented independent route facts: %+v", binding)
	}
}

func TestRecoverBoundSessionRejectsTamperedLaunchProfileProof(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	tests := []struct {
		name   string
		mutate func(*delegateEvent)
		want   string
	}{
		{
			name: "event schema whitespace",
			mutate: func(event *delegateEvent) {
				event.SchemaVersion = " " + delegateEventSchemaVersion + " "
			},
			want: "invalid delegate event identity",
		},
		{
			name: "current event with legacy launch plan",
			mutate: func(event *delegateEvent) {
				legacyBody := bytes.Replace(
					event.LaunchProfile.Snapshot,
					[]byte(`"provider_route_kind":"direct_model",`),
					nil,
					1,
				)
				legacyBody = bytes.Replace(
					legacyBody,
					[]byte(`"schema_version":"compound.delegate_launch_profile_plan.v0.2"`),
					[]byte(`"schema_version":"compound.delegate_launch_profile_plan.v0.1"`),
					1,
				)
				event.LaunchProfile.SchemaVersion = delegateLegacyLaunchProfilePlanSchema
				event.LaunchProfile.ProviderRouteKind = ""
				event.LaunchProfile.Snapshot = legacyBody
				event.Readiness.LaunchProfile.Snapshot = append(json.RawMessage(nil), legacyBody...)
				sum := sha256.Sum256(legacyBody)
				event.Readiness.LaunchProfile.Revision = "sha256:" + hex.EncodeToString(sum[:])
				finalized, err := finalizeDelegateReadinessSnapshot(*event.Readiness)
				if err != nil {
					t.Fatal(err)
				}
				event.Readiness = &finalized
			},
			want: "current delegate event launch profile schema",
		},
		{
			name: "plan snapshot",
			mutate: func(event *delegateEvent) {
				event.LaunchProfile.Snapshot = json.RawMessage(`{"schema_version":"compound.delegate_launch_profile_plan.v0.1"}`)
			},
		},
		{
			name: "plan projection",
			mutate: func(event *delegateEvent) {
				event.LaunchProfile.ClientProfileID = "tampered-client-profile"
			},
		},
		{
			name: "readiness snapshot",
			mutate: func(event *delegateEvent) {
				event.Readiness.LaunchProfile.Snapshot = json.RawMessage(`{"schema_version":"compound.delegate_launch_profile_plan.v0.1"}`)
			},
		},
		{
			name: "readiness digest",
			mutate: func(event *delegateEvent) {
				event.Readiness.LaunchProfile.Revision = "sha256:tampered"
			},
		},
		{
			name: "readiness ref",
			mutate: func(event *delegateEvent) {
				event.Readiness.LaunchProfile.Ref = "launch-profile://tampered"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			originalProfileResolver := DelegateExecutionProfileResolver
			originalLaunchResolver := DelegateLaunchProfileResolver
			originalReadiness := DelegateHarnessReadiness
			originalCapacity := LaneCapacityGuard
			defer func() {
				DelegateExecutionProfileResolver = originalProfileResolver
				DelegateLaunchProfileResolver = originalLaunchResolver
				DelegateHarnessReadiness = originalReadiness
				LaneCapacityGuard = originalCapacity
			}()

			plan := testDelegateExecutionProfilePlan()
			DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
				return testCompatibleDelegateExecutionProfileResolution(plan), nil
			}
			DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
				return testCompatibleDelegateLaunchProfileResolution(), nil
			}
			DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
				return DelegateHarnessReadinessResult{Ready: true}, nil
			}
			LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

			store := newExperimentalDelegateStore(t)
			workID := prepareClaimedWork(t, store, "codex", "", "")
			result, err := store.Delegate(DelegateOptions{
				WorkID:    workID,
				Harness:   "claude_code",
				Command:   "claude",
				Workspace: t.TempDir(),
				ExecutionProfile: DelegateExecutionProfileIntent{
					ProfileID:       plan.ProfileID,
					LaunchProfileID: "claude-minimax",
				},
				Now: fixedNow,
			}, newFakeRunner())
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "spawned" {
				t.Fatalf("fixture delegate did not spawn: %+v", result)
			}

			var persisted delegateEvent
			if err := store.readEventJSON(result.EventID, &persisted); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&persisted)
			if err := store.events.Remove(result.EventID); err != nil {
				t.Fatal(err)
			}
			if err := store.writeEventJSON(result.EventID, persisted); err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = "launch profile"
			}
			if _, _, err := store.RecoverBoundSession(workID); err == nil ||
				!strings.Contains(err.Error(), want) {
				t.Fatalf("tampered %s recovery error = %v", tc.name, err)
			}
		})
	}
}

func TestRecoverBoundSessionRejectsCoherentlyResealedCrossClientProfiles(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	originalProfileResolver := DelegateExecutionProfileResolver
	originalLaunchResolver := DelegateLaunchProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateLaunchProfileResolver = originalLaunchResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()

	plan := testDelegateExecutionProfilePlan()
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		return testCompatibleDelegateExecutionProfileResolution(plan), nil
	}
	DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
		return testCompatibleDelegateLaunchProfileResolution(), nil
	}
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_code",
		Command:   "claude",
		Workspace: t.TempDir(),
		ExecutionProfile: DelegateExecutionProfileIntent{
			ProfileID:       plan.ProfileID,
			LaunchProfileID: "claude-minimax",
		},
		Now: fixedNow,
	}, newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" {
		t.Fatalf("fixture delegate did not spawn: %+v", result)
	}

	var persisted delegateEvent
	if err := store.readEventJSON(result.EventID, &persisted); err != nil {
		t.Fatal(err)
	}
	resealed := bytes.Replace(
		persisted.ExecutionProfile.Snapshot,
		[]byte(`"client_harness_id":"claude-code"`),
		[]byte(`"client_harness_id":"codex"`),
		1,
	)
	persisted.ExecutionProfile.ClientHarnessID = "codex"
	persisted.ExecutionProfile.Snapshot = resealed
	persisted.Readiness.ExecutionProfile.Snapshot = append(json.RawMessage(nil), resealed...)
	sum := sha256.Sum256(resealed)
	persisted.Readiness.ExecutionProfile.Revision = "sha256:" + hex.EncodeToString(sum[:])
	finalizedReadiness, err := finalizeDelegateReadinessSnapshot(*persisted.Readiness)
	if err != nil {
		t.Fatal(err)
	}
	persisted.Readiness = &finalizedReadiness
	if err := store.events.Remove(result.EventID); err != nil {
		t.Fatal(err)
	}
	if err := store.writeEventJSON(result.EventID, persisted); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RecoverBoundSession(workID); err == nil ||
		!strings.Contains(err.Error(), "client harness") {
		t.Fatalf("coherently resealed cross-client profiles were accepted: %v", err)
	}
}

func TestRecoverBoundSessionRejectsCoherentlyResealedProviderRouteKind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    string
		missing bool
	}{
		{name: "missing", missing: true},
		{name: "unknown", kind: "infer_from_route"},
		{name: "flipped", kind: "provider_class"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installTestDelegateExecutionProfileHarness(t)
			originalProfileResolver := DelegateExecutionProfileResolver
			originalLaunchResolver := DelegateLaunchProfileResolver
			originalReadiness := DelegateHarnessReadiness
			originalCapacity := LaneCapacityGuard
			defer func() {
				DelegateExecutionProfileResolver = originalProfileResolver
				DelegateLaunchProfileResolver = originalLaunchResolver
				DelegateHarnessReadiness = originalReadiness
				LaneCapacityGuard = originalCapacity
			}()

			plan := testDelegateExecutionProfilePlan()
			DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
				return testCompatibleDelegateExecutionProfileResolution(plan), nil
			}
			DelegateLaunchProfileResolver = func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error) {
				return testCompatibleDelegateLaunchProfileResolution(), nil
			}
			DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
				return DelegateHarnessReadinessResult{Ready: true}, nil
			}
			LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

			store := newExperimentalDelegateStore(t)
			workID := prepareClaimedWork(t, store, "codex", "", "")
			result, err := store.Delegate(DelegateOptions{
				WorkID:    workID,
				Harness:   "claude_code",
				Command:   "claude",
				Workspace: t.TempDir(),
				ExecutionProfile: DelegateExecutionProfileIntent{
					ProfileID:       plan.ProfileID,
					LaunchProfileID: "claude-minimax",
				},
				Now: fixedNow,
			}, newFakeRunner())
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "spawned" {
				t.Fatalf("fixture delegate did not spawn: %+v", result)
			}

			var persisted delegateEvent
			if err := store.readEventJSON(result.EventID, &persisted); err != nil {
				t.Fatal(err)
			}
			var snapshot map[string]any
			if err := json.Unmarshal(persisted.LaunchProfile.Snapshot, &snapshot); err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				delete(snapshot, "provider_route_kind")
				persisted.LaunchProfile.ProviderRouteKind = ""
			} else {
				snapshot["provider_route_kind"] = tc.kind
				persisted.LaunchProfile.ProviderRouteKind = tc.kind
			}
			resealed, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			persisted.LaunchProfile.Snapshot = resealed
			persisted.Readiness.LaunchProfile.Snapshot = append(json.RawMessage(nil), resealed...)
			sum := sha256.Sum256(resealed)
			persisted.Readiness.LaunchProfile.Revision = "sha256:" + hex.EncodeToString(sum[:])
			finalizedReadiness, err := finalizeDelegateReadinessSnapshot(*persisted.Readiness)
			if err != nil {
				t.Fatal(err)
			}
			persisted.Readiness = &finalizedReadiness
			if err := store.events.Remove(result.EventID); err != nil {
				t.Fatal(err)
			}
			if err := store.writeEventJSON(result.EventID, persisted); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.RecoverBoundSession(workID); err == nil ||
				!strings.Contains(err.Error(), "launch profile") {
				t.Fatalf("coherently resealed provider route kind %q was accepted: %v", tc.kind, err)
			}
		})
	}
}

func TestRecoverBoundSessionRejectsTamperedExecutionProfileProof(t *testing.T) {
	installTestDelegateExecutionProfileHarness(t)
	for _, tc := range []struct {
		name   string
		mutate func(*delegateEvent)
	}{
		{
			name: "profile snapshot body",
			mutate: func(event *delegateEvent) {
				event.ExecutionProfile.Snapshot = json.RawMessage(`{"client_harness_id":"claude-code","id":"epplan_tampered","native_effort":{"requested":"xhigh"},"profile_id":"coding-implementation","schema_version":"compound.execution_profile_plan.v0.1","transport_id":"switchback"}`)
			},
		},
		{
			name: "readiness body",
			mutate: func(event *delegateEvent) {
				event.Readiness.ExecutionProfile.Snapshot = json.RawMessage(`{"id":"epplan_tampered"}`)
			},
		},
		{
			name: "readiness digest",
			mutate: func(event *delegateEvent) {
				event.Readiness.ExecutionProfile.Revision = "sha256:tampered"
			},
		},
		{
			name: "readiness ref",
			mutate: func(event *delegateEvent) {
				event.Readiness.ExecutionProfile.Ref = "execution-profile://epplan_tampered"
			},
		},
		{
			name: "coherently resealed profile with stale readiness snapshot revision",
			mutate: func(event *delegateEvent) {
				const resealedID = "epplan_resealed"
				resealed := bytes.Replace(
					event.ExecutionProfile.Snapshot,
					[]byte(event.ExecutionProfile.ID),
					[]byte(resealedID),
					1,
				)
				event.ExecutionProfile.ID = resealedID
				event.ExecutionProfile.Snapshot = resealed
				event.Readiness.ExecutionProfile.Ref = "execution-profile://" + resealedID
				event.Readiness.ExecutionProfile.Snapshot = append(json.RawMessage(nil), resealed...)
				sum := sha256.Sum256(resealed)
				event.Readiness.ExecutionProfile.Revision = "sha256:" + hex.EncodeToString(sum[:])
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalProfileResolver := DelegateExecutionProfileResolver
			originalReadiness := DelegateHarnessReadiness
			originalCapacity := LaneCapacityGuard
			defer func() {
				DelegateExecutionProfileResolver = originalProfileResolver
				DelegateHarnessReadiness = originalReadiness
				LaneCapacityGuard = originalCapacity
			}()

			plan := testDelegateExecutionProfilePlan()
			DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
				return testCompatibleDelegateExecutionProfileResolution(plan), nil
			}
			DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
				return DelegateHarnessReadinessResult{Ready: true}, nil
			}
			LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

			store := newExperimentalDelegateStore(t)
			workID := prepareClaimedWork(t, store, "codex", "", "")
			result, err := store.Delegate(DelegateOptions{
				WorkID:           workID,
				Harness:          "claude_code",
				Command:          "claude",
				Workspace:        t.TempDir(),
				ExecutionProfile: DelegateExecutionProfileIntent{ProfileID: plan.ProfileID},
				Now:              fixedNow,
			}, newFakeRunner())
			if err != nil {
				t.Fatal(err)
			}
			var persisted delegateEvent
			if err := store.readEventJSON(result.EventID, &persisted); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&persisted)
			if err := store.events.Remove(result.EventID); err != nil {
				t.Fatal(err)
			}
			if err := store.writeEventJSON(result.EventID, persisted); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.RecoverBoundSession(workID); err == nil || !strings.Contains(err.Error(), "execution profile") {
				t.Fatalf("tampered %s recovery error = %v", tc.name, err)
			} else if strings.Contains(tc.name, "stale readiness snapshot") && !strings.Contains(err.Error(), "snapshot_revision") {
				t.Fatalf("coherently resealed profile was not rejected by the aggregate readiness revision: %v", err)
			}
		})
	}
}

func TestDelegateWithoutExecutionProfilePreservesLegacyBehavior(t *testing.T) {
	originalProfileResolver := DelegateExecutionProfileResolver
	originalReadiness := DelegateHarnessReadiness
	originalCapacity := LaneCapacityGuard
	defer func() {
		DelegateExecutionProfileResolver = originalProfileResolver
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalCapacity
	}()
	calls := 0
	DelegateExecutionProfileResolver = func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error) {
		calls++
		return testCompatibleDelegateExecutionProfileResolution(testDelegateExecutionProfilePlan()), nil
	}
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ready", "ok")

	store := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, store, "codex", "", "")
	result, err := store.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude_code",
		Command:   "claude",
		Workspace: t.TempDir(),
		Now:       fixedNow,
	}, newFakeRunner())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "spawned" || result.ExecutionProfile != nil || calls != 0 {
		t.Fatalf("legacy delegate changed: calls=%d result=%+v", calls, result)
	}
}

var errTestExecutionProfileUnknown = &testExecutionProfileError{"unknown profile"}

type testExecutionProfileError struct{ message string }

func (e *testExecutionProfileError) Error() string { return e.message }
