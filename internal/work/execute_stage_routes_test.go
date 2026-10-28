package work

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestExecuteStageSkipsUnavailableRouteAndLaunchesNextCandidate(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_route_next")
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	restoreRoutes := SwapExecuteStageRouteCandidates(func(string, string) []ExecutionRouteCandidate {
		return []ExecutionRouteCandidate{{LaunchProfileID: "claude-zai", Harness: "claude-zai"}, {LaunchProfileID: "claude-minimax", Harness: "claude-minimax"}}
	})
	t.Cleanup(restoreRoutes)
	executeStageCapacityFn = func(_ string, lane string, _ time.Time) (float64, bool, error) {
		if lane == "claude-zai" {
			return 0, false, nil
		}
		return 20, true, nil
	}
	executeStagePolicyEvalFn = func(policyActionIntentShim, policyConfigShim, time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_route_next"}
	}
	var launches int
	launchStub = func(_ *Store, workID, lane, harness, _, _ string, _ time.Time) (string, error) {
		launches++
		if workID != "work_route_next" || lane != "claude-minimax" || harness != "claude-minimax" {
			t.Fatalf("launch tuple = work=%s lane=%s harness=%s", workID, lane, harness)
		}
		return "ratt_route_next", nil
	}

	res, err := s.Continue(fixedNow, ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "execute" || res.Lane != "claude-minimax" || launches != 1 || !strings.Contains(res.Human, "claude-zai: capacity snapshot") {
		t.Fatalf("result=%+v launches=%d; want one MiniMax launch", res, launches)
	}
}

func TestExecuteStageAllRouteCapacityUnavailableLeavesWorkReady(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_route_unavailable")
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	restoreRoutes := SwapExecuteStageRouteCandidates(func(string, string) []ExecutionRouteCandidate {
		return []ExecutionRouteCandidate{{LaunchProfileID: "claude-zai", Harness: "claude-zai"}, {LaunchProfileID: "claude-minimax", Harness: "claude-minimax"}}
	})
	t.Cleanup(restoreRoutes)
	executeStageCapacityFn = func(_ string, lane string, _ time.Time) (float64, bool, error) {
		if lane == "claude-zai" {
			return 99, true, nil
		}
		return 0, false, nil
	}
	launchStub = func(*Store, string, string, string, string, string, time.Time) (string, error) {
		t.Fatal("launch must not run when every candidate has unavailable capacity")
		return "", nil
	}

	res, err := s.Continue(fixedNow, ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != "capacity_unknown" || res.Action != "none" {
		t.Fatalf("result=%+v; want capacity refusal", res)
	}
	after, err := s.Get("work_route_unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "ready" {
		t.Fatalf("state=%s; unavailable capacity must not mutate work", after.State)
	}
}

func TestExecuteStageTriesLaterRouteWhenFirstHasNoCompatibleWork(t *testing.T) {
	s := newStore(t)
	_, err := s.Create(CreateInput{
		WorkID: "work_route_selected_minimax", Owner: "orchestrator:claude_minimax", Actor: "codex:orchestrator",
		CreatorHarness: "codex", State: "ready", Risk: "tier1", Privacy: "private", SuccessPredicate: "selected route is preserved",
		ExecutionSelection: &envelope.ExecutionSelection{
			LaunchProfileID: "claude-minimax", Harness: "claude_minimax", Source: "explicit", Revision: 1,
			SelectedBy: "codex:orchestrator", SelectedAt: fixedNow.Format(time.RFC3339),
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	restoreRoutes := SwapExecuteStageRouteCandidates(func(string, string) []ExecutionRouteCandidate {
		return []ExecutionRouteCandidate{{LaunchProfileID: "claude-zai", Harness: "claude-zai"}, {LaunchProfileID: "claude-minimax", Harness: "claude-minimax"}}
	})
	t.Cleanup(restoreRoutes)
	executeStageCapacityFn = func(string, string, time.Time) (float64, bool, error) { return 20, true, nil }
	executeStagePolicyEvalFn = func(policyActionIntentShim, policyConfigShim, time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_route_selected"}
	}
	var launchedLane string
	launchStub = func(_ *Store, workID, lane, _, _, _ string, _ time.Time) (string, error) {
		if workID != "work_route_selected_minimax" {
			t.Fatalf("work=%q", workID)
		}
		launchedLane = lane
		return "ratt_route_selected", nil
	}

	res, err := s.Continue(fixedNow, ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if launchedLane != "claude-minimax" || res.Lane != "claude-minimax" {
		t.Fatalf("selected work launched on %q; result=%+v", launchedLane, res)
	}
	if len(res.Refused) == 0 || res.Refused[0].WorkID != "work_route_selected_minimax" {
		t.Fatalf("first incompatible route must be recorded without mutation: %+v", res.Refused)
	}
}

func TestExecuteStageDistinctProfilesShareHarnessWithoutSubstitution(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_distinct_profiles")
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	first := ExecutionRouteCandidate{LaunchProfileID: "fixture-basic", Harness: "claude_minimax", RequestedModelID: "fixture-basic", ProviderRoute: "provider-basic", CapacityLane: "provider-basic", SourceRevision: "revision"}
	second := ExecutionRouteCandidate{LaunchProfileID: "fixture-quality", Harness: "claude_minimax", RequestedModelID: "fixture-quality", ProviderRoute: "provider-quality", CapacityLane: "provider-quality", SourceRevision: "revision"}
	t.Cleanup(SwapExecuteStageRouteCandidates(func(string, string) []ExecutionRouteCandidate { return []ExecutionRouteCandidate{first, second} }))
	var capacity []string
	executeStageCapacityFn = func(_ string, lane string, _ time.Time) (float64, bool, error) {
		capacity = append(capacity, lane)
		return 10, true, nil
	}
	executeStagePolicyEvalFn = func(policyActionIntentShim, policyConfigShim, time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_tuple"}
	}
	var checked []ExecutionRouteCandidate
	t.Cleanup(SwapExecuteStageCandidatePreflight(func(_ *Store, _ string, c ExecutionRouteCandidate, _ time.Time) error {
		checked = append(checked, c)
		if c == first {
			return errors.New("lane_unready:fixture basic refused")
		}
		return nil
	}))
	launches := 0
	t.Cleanup(SwapExecuteStageCandidateLaunch(func(_ *Store, _ string, c ExecutionRouteCandidate, _, _ string, _ time.Time) (string, error) {
		launches++
		if c != second {
			t.Fatalf("tuple substituted: %+v", c)
		}
		return "ratt_exact_tuple", nil
	}))
	res, err := s.Continue(fixedNow, ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if launches != 1 || res.Lane != second.LaunchProfileID || !reflect.DeepEqual(checked, []ExecutionRouteCandidate{first, second}) || !reflect.DeepEqual(capacity, []string{"provider-basic", "provider-quality"}) {
		t.Fatalf("result=%+v checked=%+v capacity=%v launches=%d", res, checked, capacity, launches)
	}
}

func TestExecuteStageBoundsRoutesAndWorkWithoutClaims(t *testing.T) {
	s := newStore(t)
	before := map[string]envelope.WorkEnvelope{}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("work_bound_%d", i)
		seedReadyExecuteWork(t, s, id)
		w, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = w
	}
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	routes := make([]ExecutionRouteCandidate, 8)
	for i := range routes {
		routes[i] = ExecutionRouteCandidate{LaunchProfileID: fmt.Sprintf("fixture-%d", i), Harness: "claude_minimax"}
	}
	t.Cleanup(SwapExecuteStageRouteCandidates(func(string, string) []ExecutionRouteCandidate { return routes }))
	capacities, preflights := 0, 0
	executeStageCapacityFn = func(string, string, time.Time) (float64, bool, error) { capacities++; return 10, true, nil }
	executeStagePolicyEvalFn = func(policyActionIntentShim, policyConfigShim, time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_bounded"}
	}
	t.Cleanup(SwapExecuteStageCandidatePreflight(func(*Store, string, ExecutionRouteCandidate, time.Time) error {
		preflights++
		return errors.New("lane_unready:fixture refusal")
	}))
	res, err := s.Continue(fixedNow, ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if capacities != executeStageMaxRouteCandidates || preflights != executeStageMaxRouteCandidates*executeStageMaxPickAttempts || res.Action != "none" || len(res.Refused) != preflights {
		t.Fatalf("result=%+v capacity=%d preflight=%d", res, capacities, preflights)
	}
	for id, w := range before {
		after, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(w, after) {
			t.Fatalf("refused work %s mutated", id)
		}
	}
}

func TestUnattendedEligibleCandidateProtectsExactSavedIntent(t *testing.T) {
	candidate := ExecutionRouteCandidate{LaunchProfileID: "fixture-quality", Harness: "claude_qwen", RequestedModelID: "fixture-model", ProviderRoute: "fixture-provider"}
	selection := envelope.ExecutionSelection{LaunchProfileID: candidate.LaunchProfileID, Harness: candidate.Harness, RequestedModelID: candidate.RequestedModelID, ProviderRoute: candidate.ProviderRoute}
	cases := []struct {
		name, owner, model, provider string
		want                         bool
	}{
		{"dispatcher selected", "orchestrator:fixture", candidate.RequestedModelID, candidate.ProviderRoute, true},
		{"foreign owner", "codex:executor", candidate.RequestedModelID, candidate.ProviderRoute, false},
		{"owner absent", "", candidate.RequestedModelID, candidate.ProviderRoute, false},
		{"model changed", "orchestrator:fixture", "other-model", candidate.ProviderRoute, false},
		{"provider changed", "orchestrator:fixture", candidate.RequestedModelID, "other-provider", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel := selection
			sel.RequestedModelID = tc.model
			sel.ProviderRoute = tc.provider
			w := envelope.WorkEnvelope{Owner: tc.owner, ExecutionSelection: &sel}
			got, why := unattendedEligibleCandidate(w, candidate)
			if got != tc.want {
				t.Fatalf("eligible=%t want=%t: %s", got, tc.want, why)
			}
		})
	}
}

func TestExecuteStageSavedAliasRequiresOwnerPreflightBeforeClaim(t *testing.T) {
	for _, alias := range []string{"fixture-wrapper", "removed-wrapper"} {
		t.Run(alias, func(t *testing.T) {
			s := newStore(t)
			w, err := s.Create(CreateInput{WorkID: "work_saved_alias", Owner: "orchestrator:fixture", Actor: "codex", CreatorHarness: "codex", State: "ready", Risk: "tier1", Privacy: "private", SuccessPredicate: "saved wrapper reaches its owner", ExecutionSelection: &envelope.ExecutionSelection{LaunchProfileID: alias, Harness: "claude_minimax", RequestedModelID: "fixture model (token plan)", ProviderRoute: "fixture-provider", Revision: 1, Source: "explicit", SelectedBy: "operator", SelectedAt: fixedNow.Format(time.RFC3339)}}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			installExecuteGrant(t, s.home, true)
			restoreAllExecuteHooks(t)
			candidate := ExecutionRouteCandidate{LaunchProfileID: "fixture model (token plan)", Harness: "claude_minimax", RequestedModelID: "fixture model (token plan)", ProviderRoute: "fixture-provider", CapacityLane: "fixture-provider", SourceRevision: "fixture-revision"}
			t.Cleanup(SwapExecuteStageRouteCandidates(func(string, string) []ExecutionRouteCandidate { return []ExecutionRouteCandidate{candidate} }))
			executeStageCapacityFn = func(string, string, time.Time) (float64, bool, error) { return 10, true, nil }
			executeStagePolicyEvalFn = func(policyActionIntentShim, policyConfigShim, time.Time) policyDecisionShim {
				return policyDecisionShim{Outcome: "allow", ID: "pol_alias"}
			}
			checked, launched := 0, 0
			t.Cleanup(SwapExecuteStageCandidatePreflight(func(store *Store, id string, c ExecutionRouteCandidate, _ time.Time) error {
				checked++
				current, err := store.Get(id)
				if err != nil || !reflect.DeepEqual(w, current) {
					t.Fatalf("work mutated before owner preflight: %v", err)
				}
				if c.LaunchProfileID != alias || c.RequestedModelID != candidate.RequestedModelID {
					t.Fatalf("saved tuple substituted: %+v", c)
				}
				if alias == "removed-wrapper" {
					return errors.New("lane_unready:owner no longer resolves saved wrapper")
				}
				return nil
			}))
			t.Cleanup(SwapExecuteStageCandidateLaunch(func(_ *Store, _ string, c ExecutionRouteCandidate, _, _ string, _ time.Time) (string, error) {
				launched++
				if c.LaunchProfileID != alias {
					t.Fatalf("profile lost at launch: %+v", c)
				}
				return "ratt_saved_alias", nil
			}))
			result, err := s.Continue(fixedNow, ContinueOptions{Actor: "codex"})
			if err != nil || checked != 1 {
				t.Fatalf("result=%+v err=%v checks=%d", result, err, checked)
			}
			if alias == "removed-wrapper" {
				after, err := s.Get(w.ID)
				if err != nil || launched != 0 || !reflect.DeepEqual(w, after) {
					t.Fatalf("refused alias mutated or launched: err=%v launches=%d", err, launched)
				}
			} else if launched != 1 || result.Action != "execute" {
				t.Fatalf("saved alias did not launch: %+v", result)
			}
		})
	}
}
