package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/work"
)

func testLaneLaunchProfilePlan() []byte {
	return []byte(`{
		"schema": "switchback/launch-profile-plan@1",
		"authority": {
			"owner": "switchback",
			"compound_role": "consumer",
			"projection": "non_secret_profile_conformance"
		},
		"authority_revision": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"revision": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"profile": {
			"id": "claude-minimax",
			"provider_lane": "minimax",
			"harness_preset": "claude-full",
			"capture_policy": "full-wire",
			"profile_label": "claude-minimax",
			"client_profile": "claude-minimax-default",
			"harness": "claude-code",
			"route": "minimax/minimax-m3",
			"requested_model": "minimax/minimax-m3",
			"requested_effort": "xhigh",
			"transport": "tap",
			"capture": {
				"id": "full-wire",
				"mode": "segmented_full_wire"
			},
			"launch_args": ["--synthetic"],
			"wrappers": ["claude-minimax"],
			"targets": ["synthetic-target"]
		},
		"changed": false,
  "artifacts": [{
    "kind": "profile_conformance",
    "path": "/synthetic/private/profile.json",
    "exists": true,
    "changed": false,
    "current_sha256": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "desired_sha256": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "mode": "0600",
    "actual_mode": "0600",
    "mode_matches": true
  }, {
    "kind": "wrapper",
    "path": "/synthetic/private/claude-minimax",
    "exists": true,
    "changed": false,
    "current_sha256": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
    "desired_sha256": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
    "mode": "0700",
    "actual_mode": "0700",
    "mode_matches": true
  }]
	}`)
}

func testLaunchProfileResolveRequest() work.DelegateLaunchProfileResolveRequest {
	return work.DelegateLaunchProfileResolveRequest{
		ProfileID:                 "claude-minimax",
		SelectedHarness:           "claude_minimax",
		SelectedLaneID:            "claude_minimax",
		SelectedModelID:           "claude-minimax (token plan)",
		SelectedProviderRoute:     "minimax",
		SelectedProviderRouteKind: "provider_class",
		SelectedTransportID:       "switchback",
		RequestedNativeEffort:     "xhigh",
		SelectedCommand:           "claude-minimax",
	}
}

func TestNormalizeDelegateLaunchProfilePlanConsumesSafeOwnerProjection(t *testing.T) {
	t.Parallel()
	raw := testLaneLaunchProfilePlan()
	resolution, err := normalizeDelegateLaunchProfilePlan(raw, testLaunchProfileResolveRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Compatibility.Disposition != work.DelegateLaunchProfileCompatible {
		t.Fatalf("compatible owner plan disposition = %q", resolution.Compatibility.Disposition)
	}
	if got, want := resolution.ExecutablePath, "/synthetic/private/claude-minimax"; got != want {
		t.Fatalf("attested executable path = %q, want %q", got, want)
	}

	var snapshot map[string]any
	if err := json.Unmarshal(resolution.Snapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got, want := snapshot["source_digest"], "sha256:"+hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("source digest = %v, want %s", got, want)
	}
	for _, required := range []string{
		"schema_version",
		"source_schema",
		"source_digest",
		"authority_revision",
		"revision",
		"id",
		"harness",
		"provider_lane",
		"provider_route_kind",
		"route",
		"requested_model",
		"requested_effort",
		"transport_id",
		"owner_transport",
		"client_profile_id",
		"capture_mode",
		"executable_id",
	} {
		value, ok := snapshot[required].(string)
		if !ok || strings.TrimSpace(value) == "" {
			t.Fatalf("normalized snapshot lost %s: %s", required, resolution.Snapshot)
		}
	}
	if snapshot["transport_id"] != "switchback" || snapshot["owner_transport"] != "tap" {
		t.Fatalf("transport family/subtype were not preserved exactly: %s", resolution.Snapshot)
	}
	for _, forbidden := range []string{
		"artifacts",
		"path",
		"launch_args",
		"targets",
		"wrappers",
		"harness_preset",
		"profile_label",
	} {
		if bytes.Contains(resolution.Snapshot, []byte(forbidden)) {
			t.Fatalf("normalized snapshot copied owner-private field %q: %s", forbidden, resolution.Snapshot)
		}
	}
	if bytes.Contains(resolution.Snapshot, []byte("/synthetic/private")) {
		t.Fatalf("normalized snapshot copied an owner path: %s", resolution.Snapshot)
	}
}

func testLaneLaunchProfilePlanFor(
	t *testing.T,
	profileID,
	providerLane,
	route,
	requestedModel,
	clientProfile,
	wrapper string,
) []byte {
	t.Helper()
	var plan map[string]any
	if err := json.Unmarshal(testLaneLaunchProfilePlan(), &plan); err != nil {
		t.Fatal(err)
	}
	profile := plan["profile"].(map[string]any)
	profile["id"] = profileID
	profile["provider_lane"] = providerLane
	profile["route"] = route
	profile["requested_model"] = requestedModel
	profile["client_profile"] = clientProfile
	profile["wrappers"] = []string{wrapper}
	artifacts := plan["artifacts"].([]any)
	artifacts[1].(map[string]any)["path"] = "/synthetic/private/" + wrapper
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNormalizeDelegateLaunchProfilePlanAcceptsRegisteredClaudeTokenProviderClasses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		profileID       string
		selectedHarness string
		selectedModel   string
		selectedRoute   string
		providerLane    string
		route           string
		ownerModel      string
		clientProfile   string
		wrapper         string
	}{
		{
			name:            "minimax",
			profileID:       "claude-minimax",
			selectedHarness: "claude_minimax",
			selectedModel:   "claude-minimax (token plan)",
			selectedRoute:   "minimax",
			providerLane:    "minimax",
			route:           "minimax/MiniMax-M3",
			ownerModel:      "MiniMax-M3",
			clientProfile:   "claude-minimax-default",
			wrapper:         "claude-minimax",
		},
		{
			name:            "qwen",
			profileID:       "claude-qwen",
			selectedHarness: "claude_qwen",
			selectedModel:   "claude-qwen (token plan)",
			selectedRoute:   "qwen",
			providerLane:    "qwen",
			route:           "qwen/qwen3.8-max",
			ownerModel:      "qwen3.8-max",
			clientProfile:   "claude-qwen-default",
			wrapper:         "claude-qwen",
		},
		{
			name:            "zai",
			profileID:       "claude-zai-full",
			selectedHarness: "claude_zai",
			selectedModel:   "claude-zai (glm sub)",
			selectedRoute:   "zai",
			providerLane:    "zai",
			route:           "zai/glm-5.2",
			ownerModel:      "glm-5.2",
			clientProfile:   "claude-zai-default",
			wrapper:         "claude-zai-full",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := testLaunchProfileResolveRequest()
			req.ProfileID = tc.profileID
			req.SelectedHarness = tc.selectedHarness
			req.SelectedLaneID = tc.selectedHarness
			req.SelectedModelID = tc.selectedModel
			req.SelectedProviderRoute = tc.selectedRoute
			req.SelectedProviderRouteKind = "provider_class"
			req.SelectedCommand = tc.wrapper

			resolution, err := normalizeDelegateLaunchProfilePlan(
				testLaneLaunchProfilePlanFor(
					t,
					tc.profileID,
					tc.providerLane,
					tc.route,
					tc.ownerModel,
					tc.clientProfile,
					tc.wrapper,
				),
				req,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := resolution.Compatibility.Disposition, work.DelegateLaunchProfileCompatible; got != want {
				t.Fatalf("registered Claude token lane disposition = %q, want %q", got, want)
			}
		})
	}
}

func TestNormalizeDelegateLaunchProfilePlanUsesOnlyDeclaredProviderRouteAxis(t *testing.T) {
	t.Parallel()
	raw := testLaneLaunchProfilePlanFor(
		t,
		"claude-fixture-direct",
		"fixture-provider",
		"provider.example/model-primary",
		"model-primary",
		"claude-fixture-direct-default",
		"claude-fixture-direct",
	)
	tests := []struct {
		name          string
		selectedRoute string
		selectedKind  string
		want          string
	}{
		{
			name:          "direct model accepts concrete route",
			selectedRoute: "provider.example/model-primary",
			selectedKind:  "direct_model",
			want:          work.DelegateLaunchProfileCompatible,
		},
		{
			name:          "provider class rejects concrete route cross-kind",
			selectedRoute: "provider.example/model-primary",
			selectedKind:  "provider_class",
			want:          work.DelegateLaunchProfileIncompatible,
		},
		{
			name:          "direct model rejects provider lane cross-kind",
			selectedRoute: "fixture-provider",
			selectedKind:  "direct_model",
			want:          work.DelegateLaunchProfileIncompatible,
		},
		{
			name:          "missing kind does not infer from slash",
			selectedRoute: "provider.example/model-primary",
			selectedKind:  "",
			want:          work.DelegateLaunchProfileIncompatible,
		},
		{
			name:          "unknown kind does not infer from requested model",
			selectedRoute: "provider.example/model-primary",
			selectedKind:  "requested_model_alias",
			want:          work.DelegateLaunchProfileIncompatible,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := testLaunchProfileResolveRequest()
			req.ProfileID = "claude-fixture-direct"
			req.SelectedHarness = "claude_code"
			req.SelectedLaneID = "claude_code"
			req.SelectedModelID = "provider.example/model-primary"
			req.SelectedProviderRoute = tc.selectedRoute
			req.SelectedProviderRouteKind = tc.selectedKind
			req.SelectedCommand = "claude-fixture-direct"

			resolution, err := normalizeDelegateLaunchProfilePlan(raw, req)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolution.Compatibility.Disposition; got != tc.want {
				t.Fatalf("route-kind disposition = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeDelegateLaunchProfilePlanRefusesOwnerAliasMismatch(t *testing.T) {
	t.Parallel()
	req := testLaunchProfileResolveRequest()
	req.ProfileID = "claude-fixture-stale"
	req.SelectedHarness = "claude_code"
	req.SelectedLaneID = "claude_code"
	req.SelectedModelID = "provider.example/model-primary"
	req.SelectedProviderRoute = "provider.example/model-primary"
	req.SelectedProviderRouteKind = "direct_model"
	req.SelectedCommand = "claude-fixture-stale"

	_, err := normalizeDelegateLaunchProfilePlan(
		testLaneLaunchProfilePlanFor(
			t,
			"claude-fixture-current",
			"fixture-provider",
			"provider.example/model-primary",
			"model-primary",
			"claude-fixture-current-default",
			"claude-fixture-current",
		),
		req,
	)
	if err == nil ||
		!strings.Contains(err.Error(), `returned id "claude-fixture-current" for requested id "claude-fixture-stale"`) {
		t.Fatalf("owner alias mismatch was accepted: %v", err)
	}
}

func TestNormalizeDelegateLaunchProfilePlanRefusesAbsentDedicatedProfile(t *testing.T) {
	t.Parallel()
	req := testLaunchProfileResolveRequest()
	req.ProfileID = "claude-fixture-missing"
	req.SelectedHarness = "claude_code"
	req.SelectedLaneID = "claude_code"
	req.SelectedModelID = "provider.example/model-secondary"
	req.SelectedProviderRoute = "provider.example/model-secondary"
	req.SelectedProviderRouteKind = "direct_model"
	req.SelectedCommand = "claude-fixture-missing"

	if _, err := normalizeDelegateLaunchProfilePlan(nil, req); err == nil ||
		!strings.Contains(err.Error(), "empty plan") {
		t.Fatalf("absent dedicated profile was invented: %v", err)
	}
}

func TestNormalizeDelegateLaunchProfilePlanRefusesUntrustedOrDriftedOwnerOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		replace func(map[string]any)
		want    string
	}{
		{
			name: "source schema",
			replace: func(plan map[string]any) {
				plan["schema"] = "switchback/launch-profile-plan@2"
			},
			want: "schema",
		},
		{
			name: "authority owner",
			replace: func(plan map[string]any) {
				plan["authority"].(map[string]any)["owner"] = "other"
			},
			want: "authority",
		},
		{
			name: "aggregate drift",
			replace: func(plan map[string]any) {
				plan["changed"] = true
			},
			want: "drift",
		},
		{
			name: "missing artifact",
			replace: func(plan map[string]any) {
				plan["artifacts"].([]any)[0].(map[string]any)["exists"] = false
			},
			want: "artifact",
		},
		{
			name: "missing wrapper artifact",
			replace: func(plan map[string]any) {
				plan["artifacts"] = plan["artifacts"].([]any)[:1]
			},
			want: "wrapper",
		},
		{
			name: "artifact content drift",
			replace: func(plan map[string]any) {
				plan["artifacts"].([]any)[0].(map[string]any)["changed"] = true
			},
			want: "artifact",
		},
		{
			name: "artifact mode drift",
			replace: func(plan map[string]any) {
				plan["artifacts"].([]any)[0].(map[string]any)["mode_matches"] = false
			},
			want: "artifact",
		},
		{
			name: "artifact digest drift",
			replace: func(plan map[string]any) {
				plan["artifacts"].([]any)[0].(map[string]any)["current_sha256"] = "sha256:" + strings.Repeat("d", 64)
			},
			want: "artifact",
		},
		{
			name: "missing client profile",
			replace: func(plan map[string]any) {
				plan["profile"].(map[string]any)["client_profile"] = ""
			},
			want: "client_profile",
		},
		{
			name: "unknown owner transport",
			replace: func(plan map[string]any) {
				plan["profile"].(map[string]any)["transport"] = "mystery"
			},
			want: "transport",
		},
		{
			name: "non-capturing profile",
			replace: func(plan map[string]any) {
				plan["profile"].(map[string]any)["capture"].(map[string]any)["mode"] = "metadata_only"
			},
			want: "capture_mode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var plan map[string]any
			if err := json.Unmarshal(testLaneLaunchProfilePlan(), &plan); err != nil {
				t.Fatal(err)
			}
			tc.replace(plan)
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := normalizeDelegateLaunchProfilePlan(raw, testLaunchProfileResolveRequest()); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("normalize error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeDelegateLaunchProfilePlanMarksTupleMismatchIncompatible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*work.DelegateLaunchProfileResolveRequest)
	}{
		{name: "harness", mutate: func(req *work.DelegateLaunchProfileResolveRequest) {
			req.SelectedHarness = "codex"
		}},
		{name: "missing selected model", mutate: func(req *work.DelegateLaunchProfileResolveRequest) {
			req.SelectedModelID = ""
		}},
		{name: "route", mutate: func(req *work.DelegateLaunchProfileResolveRequest) {
			req.SelectedProviderRoute = "other/route"
		}},
		{name: "transport", mutate: func(req *work.DelegateLaunchProfileResolveRequest) {
			req.SelectedTransportID = "native"
		}},
		{name: "effort", mutate: func(req *work.DelegateLaunchProfileResolveRequest) {
			req.RequestedNativeEffort = "medium"
		}},
		{name: "command", mutate: func(req *work.DelegateLaunchProfileResolveRequest) {
			req.SelectedCommand = "other-wrapper"
		}},
		{name: "unregistered Claude lookalike", mutate: func(req *work.DelegateLaunchProfileResolveRequest) {
			req.SelectedHarness = "claude_minimax_unregistered"
			req.SelectedLaneID = "claude_minimax_unregistered"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := testLaunchProfileResolveRequest()
			tc.mutate(&req)
			resolution, err := normalizeDelegateLaunchProfilePlan(testLaneLaunchProfilePlan(), req)
			if err != nil {
				t.Fatal(err)
			}
			if resolution.Compatibility.Disposition != work.DelegateLaunchProfileIncompatible {
				t.Fatalf("tuple mismatch disposition = %q, want incompatible", resolution.Compatibility.Disposition)
			}
		})
	}
}
