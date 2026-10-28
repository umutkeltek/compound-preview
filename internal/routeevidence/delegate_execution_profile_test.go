package routeevidence

import (
	"encoding/json"
	"testing"

	"compound/internal/work"
)

func TestResolveDelegateExecutionProfileUsesCanonicalOwnerAndLosslessSnapshot(t *testing.T) {
	resolved, err := resolveDelegateExecutionProfile(work.DelegateExecutionProfileResolveRequest{
		SelectedHarness:       "codex",
		SelectedLaneID:        "codex",
		SelectedModelID:       "fixture/model",
		SelectedProviderRoute: "auto/build",
		SelectedTransportID:   "switchback",
		Intent: work.DelegateExecutionProfileIntent{
			ProfileID:       "coding-implementation",
			ClientHarnessID: "codex",
			TransportID:     "switchback",
			RouteAlias:      "auto/build",
			ModelConstraint: "fixture/model",
			SemanticEffort:  "deep",
			NativeEffort:    "xhigh",
			PrivacyCeiling:  "private",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Compatibility.Disposition != work.DelegateExecutionProfileCompatible ||
		resolved.Compatibility.SelectedHarness != "codex" ||
		resolved.Compatibility.SelectedLaneID != "codex" ||
		resolved.Compatibility.SelectedModelID != "fixture/model" ||
		resolved.Compatibility.SelectedProviderRoute != "auto/build" ||
		resolved.Compatibility.SelectedTransportID != "switchback" {
		t.Fatalf("resolver lost owner compatibility attestation: %+v", resolved)
	}
	var canonical ExecutionProfilePlan
	if err := json.Unmarshal(resolved.Snapshot, &canonical); err != nil {
		t.Fatalf("lossless snapshot is not canonical plan JSON: %v", err)
	}
	if canonical.ID != resolved.Compatibility.PlanID ||
		canonical.ClientHarnessID != resolved.Compatibility.ClientHarnessID ||
		canonical.TransportID != resolved.Compatibility.TransportID ||
		canonical.NativeEffort.Requested != resolved.Compatibility.RequestedNativeEffort {
		t.Fatalf("snapshot and attestation diverged: canonical=%+v attestation=%+v", canonical, resolved.Compatibility)
	}
}

func TestResolveDelegateExecutionProfileAttestsOnlyExactRegisteredHarnessFamilies(t *testing.T) {
	for _, tc := range []struct {
		name          string
		selected      string
		lane          string
		clientHarness string
		want          string
	}{
		{name: "registered Claude Code hyphen", selected: "claude-code", lane: "claude-code", clientHarness: "claude-code", want: work.DelegateExecutionProfileCompatible},
		{name: "registered Claude Code underscore", selected: "claude_code", lane: "claude_code", clientHarness: "claude-code", want: work.DelegateExecutionProfileCompatible},
		{name: "registered Claude ZAI hyphen", selected: "claude-zai", lane: "claude-zai", clientHarness: "claude-code", want: work.DelegateExecutionProfileCompatible},
		{name: "registered Claude ZAI underscore", selected: "claude_zai", lane: "claude_zai", clientHarness: "claude-code", want: work.DelegateExecutionProfileCompatible},
		{name: "registered Claude MiniMax", selected: "claude_minimax", lane: "claude_minimax", clientHarness: "claude-code", want: work.DelegateExecutionProfileCompatible},
		{name: "registered Claude Qwen", selected: "claude_qwen", lane: "claude_qwen", clientHarness: "claude-code", want: work.DelegateExecutionProfileCompatible},
		{name: "registered Claude ZAI full", selected: "claude_zai_full", lane: "claude_zai_full", clientHarness: "claude-code", want: work.DelegateExecutionProfileCompatible},
		{name: "registered Codex", selected: "codex", lane: "codex", clientHarness: "codex", want: work.DelegateExecutionProfileCompatible},
		{name: "unknown Claude lookalike", selected: "claude_preview", lane: "claude_preview", clientHarness: "claude-code", want: work.DelegateExecutionProfileIncompatible},
		{name: "unknown Claude suffix", selected: "claude_zai_preview", lane: "claude_zai_preview", clientHarness: "claude-code", want: work.DelegateExecutionProfileIncompatible},
		{name: "unknown Codex lookalike", selected: "codex-preview", lane: "codex-preview", clientHarness: "codex", want: work.DelegateExecutionProfileIncompatible},
		{name: "Claude lane with Codex client", selected: "claude_qwen", lane: "claude_qwen", clientHarness: "codex", want: work.DelegateExecutionProfileIncompatible},
		{name: "Codex lane with Claude client", selected: "codex", lane: "codex", clientHarness: "claude-code", want: work.DelegateExecutionProfileIncompatible},
		{name: "mixed selected harness and lane families", selected: "claude_code", lane: "codex", clientHarness: "claude-code", want: work.DelegateExecutionProfileIncompatible},
		{name: "different registered Claude lane identities", selected: "claude_minimax", lane: "claude_qwen", clientHarness: "claude-code", want: work.DelegateExecutionProfileIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := resolveDelegateExecutionProfile(work.DelegateExecutionProfileResolveRequest{
				SelectedHarness:     tc.selected,
				SelectedLaneID:      tc.lane,
				SelectedTransportID: "switchback",
				Intent: work.DelegateExecutionProfileIntent{
					ProfileID:       "coding-implementation",
					ClientHarnessID: tc.clientHarness,
					TransportID:     "switchback",
					SemanticEffort:  "deep",
					NativeEffort:    "xhigh",
					PrivacyCeiling:  "private",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Compatibility.Disposition != tc.want {
				t.Fatalf("compatibility for %q = %+v, want %q", tc.selected, resolved.Compatibility, tc.want)
			}
		})
	}
}

func TestResolveDelegateExecutionProfileBindsSelectedCanonicalTuple(t *testing.T) {
	base := work.DelegateExecutionProfileResolveRequest{
		SelectedHarness:       "codex",
		SelectedLaneID:        "codex",
		SelectedModelID:       "fixture/model",
		SelectedProviderRoute: "auto/build",
		SelectedTransportID:   "switchback",
		Intent: work.DelegateExecutionProfileIntent{
			ProfileID:       "coding-implementation",
			ClientHarnessID: "codex",
			TransportID:     "switchback",
			RouteAlias:      "auto/build",
			ModelConstraint: "fixture/model",
			SemanticEffort:  "deep",
			NativeEffort:    "xhigh",
			PrivacyCeiling:  "private",
		},
	}
	for _, tc := range []struct {
		name   string
		mutate func(*work.DelegateExecutionProfileResolveRequest)
		want   string
	}{
		{name: "matching tuple", want: work.DelegateExecutionProfileCompatible},
		{
			name: "matching trimmed exact tuple identities",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.SelectedModelID = " fixture/model "
				req.SelectedProviderRoute = " auto/build "
				req.SelectedTransportID = " switchback "
			},
			want: work.DelegateExecutionProfileCompatible,
		},
		{
			name: "model punctuation variant is opaque mismatch",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.Intent.ModelConstraint = "fixture/model-name"
				req.SelectedModelID = "fixture/model_name"
			},
			want: work.DelegateExecutionProfileIncompatible,
		},
		{
			name: "provider route case variant is opaque mismatch",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.SelectedProviderRoute = "AUTO/BUILD"
			},
			want: work.DelegateExecutionProfileIncompatible,
		},
		{
			name: "transport case variant is opaque mismatch",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.SelectedTransportID = "SWITCHBACK"
			},
			want: work.DelegateExecutionProfileIncompatible,
		},
		{
			name: "route alias matches selected route identity",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.Intent.ModelConstraint = ""
				req.SelectedModelID = "auto/build"
				req.SelectedProviderRoute = "provider/physical-model"
			},
			want: work.DelegateExecutionProfileCompatible,
		},
		{
			name: "route identity case variant is opaque mismatch",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.Intent.ModelConstraint = ""
				req.SelectedModelID = "AUTO/BUILD"
				req.SelectedProviderRoute = "provider/physical-model"
			},
			want: work.DelegateExecutionProfileIncompatible,
		},
		{
			name: "model mismatch",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.SelectedModelID = "fixture/other"
			},
			want: work.DelegateExecutionProfileIncompatible,
		},
		{
			name: "provider route mismatch",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.SelectedProviderRoute = "auto/judge"
			},
			want: work.DelegateExecutionProfileIncompatible,
		},
		{
			name: "transport mismatch",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.SelectedTransportID = "native"
			},
			want: work.DelegateExecutionProfileIncompatible,
		},
		{
			name: "missing transport",
			mutate: func(req *work.DelegateExecutionProfileResolveRequest) {
				req.SelectedTransportID = ""
			},
			want: work.DelegateExecutionProfileIncompatible,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			resolved, err := resolveDelegateExecutionProfile(req)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Compatibility.Disposition != tc.want {
				t.Fatalf("compatibility = %+v, want disposition %q", resolved.Compatibility, tc.want)
			}
			if resolved.Compatibility.SelectedModelID != req.SelectedModelID ||
				resolved.Compatibility.SelectedProviderRoute != req.SelectedProviderRoute ||
				resolved.Compatibility.SelectedTransportID != req.SelectedTransportID {
				t.Fatalf("selected tuple attestation drifted: request=%+v compatibility=%+v", req, resolved.Compatibility)
			}
		})
	}
}

func TestResolveDelegateExecutionProfileDefersUnknownSwitchbackTransportToSelectedOwnerProfile(t *testing.T) {
	resolved, err := resolveDelegateExecutionProfile(work.DelegateExecutionProfileResolveRequest{
		Intent: work.DelegateExecutionProfileIntent{
			ProfileID:       "coding-implementation",
			LaunchProfileID: "claude-minimax",
			ClientHarnessID: "claude-code",
			TransportID:     "switchback",
			ModelConstraint: "minimax/minimax-m3",
			RouteAlias:      "minimax/minimax-m3",
			NativeEffort:    "xhigh",
		},
		SelectedHarness:       "claude_code",
		SelectedLaneID:        "claude_code",
		SelectedModelID:       "minimax/minimax-m3",
		SelectedProviderRoute: "minimax/minimax-m3",
		SelectedTransportID:   "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Compatibility.Disposition != work.DelegateExecutionProfileCompatible {
		t.Fatalf(
			"owner-attested transport was rejected before launch-profile resolution: %+v",
			resolved.Compatibility,
		)
	}

	withoutOwner, err := resolveDelegateExecutionProfile(work.DelegateExecutionProfileResolveRequest{
		Intent: work.DelegateExecutionProfileIntent{
			ProfileID:       "coding-implementation",
			ClientHarnessID: "claude-code",
			TransportID:     "switchback",
			ModelConstraint: "minimax/minimax-m3",
			RouteAlias:      "minimax/minimax-m3",
			NativeEffort:    "xhigh",
		},
		SelectedHarness:       "claude_code",
		SelectedLaneID:        "claude_code",
		SelectedModelID:       "minimax/minimax-m3",
		SelectedProviderRoute: "minimax/minimax-m3",
		SelectedTransportID:   "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if withoutOwner.Compatibility.Disposition != work.DelegateExecutionProfileIncompatible {
		t.Fatalf("missing owner profile incorrectly bypassed transport proof: %+v", withoutOwner.Compatibility)
	}
}
