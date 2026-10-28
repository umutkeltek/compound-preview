package routeevidence

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestResolveExecutionProfileBuiltInMappings(t *testing.T) {
	tests := []struct {
		name          string
		intent        ExecutionProfileIntent
		wantSemantic  string
		wantNative    string
		wantPolicy    string
		wantTransport string
	}{
		{
			name: "interactive Claude Code stays below ultra",
			intent: ExecutionProfileIntent{
				ProfileID:       "interactive-default",
				ClientHarnessID: "claude-code",
				TransportID:     "native",
			},
			wantSemantic:  "balanced",
			wantNative:    "medium",
			wantPolicy:    "standard",
			wantTransport: "native",
		},
		{
			name: "implementation Claude Code maps deep to xhigh",
			intent: ExecutionProfileIntent{
				ProfileID:       "coding-implementation",
				ClientHarnessID: "claude-code",
				TransportID:     "native",
			},
			wantSemantic:  "deep",
			wantNative:    "xhigh",
			wantPolicy:    "standard",
			wantTransport: "native",
		},
		{
			name: "bulk mining offers Codex over Switchback",
			intent: ExecutionProfileIntent{
				ProfileID:       "mining-bulk",
				ClientHarnessID: "codex",
				TransportID:     "switchback",
			},
			wantSemantic:  "economy",
			wantNative:    "low",
			wantPolicy:    "standard",
			wantTransport: "switchback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := ResolveExecutionProfile(tt.intent)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if plan.SemanticEffort != tt.wantSemantic ||
				plan.NativeEffort.Requested != tt.wantNative ||
				plan.OrchestrationPolicy != tt.wantPolicy ||
				plan.TransportID != tt.wantTransport {
				t.Fatalf("unexpected mapping: %+v", plan)
			}
			if plan.NativeEffort.Requested == "ultra" || plan.OrchestrationPolicy == "ultracode" {
				t.Fatalf("routine/default profile selected an opt-in value: %+v", plan)
			}
			if plan.EffectiveProvider != UnknownUntilReceipt ||
				plan.EffectiveModel != UnknownUntilReceipt ||
				plan.EffectiveNativeEffort != UnknownUntilReceipt {
				t.Fatalf("plan claimed effective execution before a receipt: %+v", plan)
			}
		})
	}
}

func TestResolveExecutionProfileKeepsUltracodeInOrchestrationOnly(t *testing.T) {
	plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "orchestration-ultracode",
		ClientHarnessID: "codex",
		TransportID:     "native",
		ModelConstraint: "openai/gpt-5.6-sol",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.OrchestrationPolicy != "ultracode" || plan.SemanticEffort != "deep" {
		t.Fatalf("explicit orchestration profile was not preserved: %+v", plan)
	}
	if plan.NativeEffort.Requested != "xhigh" ||
		strings.Contains(plan.NativeEffort.Requested, "ultracode") ||
		strings.Contains(plan.ModelConstraint, "ultracode") {
		t.Fatalf("orchestration policy leaked into model/native effort: %+v", plan)
	}
}

func TestResolveExecutionProfileSemanticOverrideRemapsThroughAdapter(t *testing.T) {
	plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
		SemanticEffort:  "balanced",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SemanticEffort != "balanced" || plan.NativeEffort.Requested != "medium" || plan.NativeEffort.Disposition != "translated" {
		t.Fatalf("semantic override did not remap through the Codex adapter: %+v", plan)
	}
}

func TestResolveExecutionProfileMarksWeakerMaxMappingsDegraded(t *testing.T) {
	tests := []struct {
		name       string
		harness    string
		transport  string
		wantNative string
	}{
		{name: "Codex max is capped at xhigh", harness: "codex", transport: "native", wantNative: "xhigh"},
		{name: "worker max is capped at high", harness: "worker", transport: "local", wantNative: "high"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
				ProfileID:       "coding-implementation",
				ClientHarnessID: tt.harness,
				TransportID:     tt.transport,
				SemanticEffort:  "max",
			})
			if err != nil {
				t.Fatal(err)
			}
			if plan.NativeEffort.Requested != tt.wantNative ||
				plan.NativeEffort.Disposition != "degraded" ||
				plan.Disposition != "degraded" {
				t.Fatalf("weaker max mapping was not marked degraded: %+v", plan)
			}
		})
	}
}

func TestResolveExecutionProfileRejectsSemanticNativeContradictions(t *testing.T) {
	tests := []struct {
		name   string
		intent ExecutionProfileIntent
	}{
		{
			name: "mining economy cannot request Codex ultra",
			intent: ExecutionProfileIntent{
				ProfileID:       "mining-bulk",
				ClientHarnessID: "codex",
				TransportID:     "switchback",
				NativeEffort:    "ultra",
			},
		},
		{
			name: "balanced cannot request high",
			intent: ExecutionProfileIntent{
				ProfileID:       "coding-implementation",
				ClientHarnessID: "codex",
				TransportID:     "native",
				SemanticEffort:  "balanced",
				NativeEffort:    "high",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolveExecutionProfile(tt.intent)
			if err == nil || !strings.Contains(err.Error(), "requested_intent.native_effort") || !strings.Contains(err.Error(), "semantic effort") {
				t.Fatalf("contradictory native effort error = %v", err)
			}
		})
	}
}

func TestDeriveChildExecutionProfileEnforcesParentNativeCeiling(t *testing.T) {
	parent, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
		SemanticEffort:  "max",
	})
	if err != nil {
		t.Fatal(err)
	}

	remaining := parent.Budget
	_, err = DeriveChildExecutionProfile(parent, ChildExecutionProfileIntent{
		ClientHarnessID: "claude-code",
		TransportID:     "native",
		SemanticEffort:  "max",
		NativeEffort:    "max",
		RemainingBudget: &remaining,
		Budget:          parent.Budget,
	})
	if err == nil || !strings.Contains(err.Error(), "child_intent.native_effort") || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("child native ceiling error = %v", err)
	}

	_, err = DeriveChildExecutionProfile(parent, ChildExecutionProfileIntent{
		ClientHarnessID: "codex",
		TransportID:     "native",
		SemanticEffort:  "balanced",
		NativeEffort:    "high",
		RemainingBudget: &remaining,
	})
	if err == nil || !strings.Contains(err.Error(), "child_intent.native_effort") || !strings.Contains(err.Error(), "semantic effort") {
		t.Fatalf("child semantic/native contradiction error = %v", err)
	}
}

func TestResolveExecutionProfileRejectsIncompatibleOrUnownedSelectors(t *testing.T) {
	tests := []struct {
		name     string
		intent   ExecutionProfileIntent
		wantPath string
		wantText string
	}{
		{
			name: "Claude Code ultra",
			intent: ExecutionProfileIntent{
				ProfileID:       "coding-implementation",
				ClientHarnessID: "claude-code",
				TransportID:     "native",
				NativeEffort:    "ultra",
			},
			wantPath: "requested_intent.native_effort",
			wantText: "claude-code",
		},
		{
			name: "transport used as harness",
			intent: ExecutionProfileIntent{
				ProfileID:       "mining-bulk",
				ClientHarnessID: "switchback",
				TransportID:     "switchback",
			},
			wantPath: "requested_intent.client_harness_id",
			wantText: "transport",
		},
		{
			name: "ultracode injected into ordinary profile",
			intent: ExecutionProfileIntent{
				ProfileID:           "coding-implementation",
				ClientHarnessID:     "codex",
				TransportID:         "native",
				OrchestrationPolicy: "ultracode",
			},
			wantPath: "requested_intent.orchestration_policy",
			wantText: "explicit profile",
		},
		{
			name: "terra alias",
			intent: ExecutionProfileIntent{
				ProfileID:       "coding-implementation",
				ClientHarnessID: "codex",
				TransportID:     "native",
				ModelConstraint: "terra",
			},
			wantPath: "requested_intent.model_constraint",
			wantText: "fully qualified",
		},
		{
			name: "luna alias",
			intent: ExecutionProfileIntent{
				ProfileID:       "coding-implementation",
				ClientHarnessID: "codex",
				TransportID:     "native",
				ModelConstraint: "luna",
			},
			wantPath: "requested_intent.model_constraint",
			wantText: "fully qualified",
		},
		{
			name: "friendly sol alias",
			intent: ExecutionProfileIntent{
				ProfileID:       "coding-implementation",
				ClientHarnessID: "codex",
				TransportID:     "native",
				ModelConstraint: "sol",
			},
			wantPath: "requested_intent.model_constraint",
			wantText: "fully qualified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolveExecutionProfile(tt.intent)
			if err == nil || !strings.Contains(err.Error(), tt.wantPath) || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %v, want path %q and text %q", err, tt.wantPath, tt.wantText)
			}
		})
	}
}

func TestResolveExecutionProfileKeepsHarnessTransportAndModelDistinct(t *testing.T) {
	plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "claude-code",
		TransportID:     "native",
		ModelConstraint: "openai/gpt-5.6-sol",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ClientHarnessID != "claude-code" || plan.TransportID != "native" || plan.ModelConstraint != "openai/gpt-5.6-sol" {
		t.Fatalf("identities collapsed: %+v", plan)
	}
	if plan.EffectiveProvider != UnknownUntilReceipt || plan.EffectiveModel != UnknownUntilReceipt {
		t.Fatalf("requested constraint was reported as observed identity: %+v", plan)
	}
}

func TestResolveExecutionProfileKeepsClaudeHarnessAndSwitchbackTransportDistinct(t *testing.T) {
	plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "interactive-default",
		ClientHarnessID: "claude-code",
		TransportID:     "switchback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ClientHarnessID != "claude-code" || plan.TransportID != "switchback" {
		t.Fatalf("Claude harness and Switchback transport collapsed: %+v", plan)
	}
	if plan.EffectiveProvider != UnknownUntilReceipt || plan.EffectiveModel != UnknownUntilReceipt {
		t.Fatalf("transport construction overclaimed observed provider/model identity: %+v", plan)
	}
}

func TestResolveExecutionProfileMiningConstructsBoundedCheckpointedPlan(t *testing.T) {
	plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "mining-bulk",
		ClientHarnessID: "codex",
		TransportID:     "switchback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Budget.MaxRequests <= 0 || plan.Budget.MaxInputTokens <= 0 ||
		plan.Budget.MaxOutputTokens <= 0 || plan.Budget.MaxDurationSeconds <= 0 ||
		plan.Budget.MaxParallelism <= 1 {
		t.Fatalf("mining budget is not bounded/high-parallelism: %+v", plan.Budget)
	}
	if !plan.Checkpoint.Required || !plan.Checkpoint.PersistPartial || plan.Checkpoint.EveryRequests <= 0 {
		t.Fatalf("mining plan lacks checkpointing: %+v", plan.Checkpoint)
	}
	if len(plan.Candidates) == 0 || plan.Candidates[0].ClientHarnessID != "codex" || plan.Candidates[0].TransportID != "switchback" {
		t.Fatalf("mining plan lacks Codex/Switchback alternative: %+v", plan.Candidates)
	}
	if explanation := plan.Candidates[0].Explanation; !strings.Contains(explanation, "construction") || strings.Contains(explanation, "provider call") {
		t.Fatalf("candidate explanation overclaims proof beyond construction: %q", explanation)
	}
}

func TestExecutionProfilePlanIDIsDeterministic(t *testing.T) {
	intent := ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		Role:            "executor",
		CapabilityClass: "build-implement",
		ClientHarnessID: "codex",
		TransportID:     "native",
		ModelConstraint: "openai/gpt-5.6-sol",
	}
	first, err := ResolveExecutionProfile(intent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveExecutionProfile(intent)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("plan ids are not stable: %q != %q", first.ID, second.ID)
	}
}

func TestDeriveChildExecutionProfileEnforcesCeilingsAndExplicitSelectors(t *testing.T) {
	parent, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
		ModelConstraint: "openai/gpt-5.6-sol",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = DeriveChildExecutionProfile(parent, ChildExecutionProfileIntent{
		ClientHarnessID: "codex",
		TransportID:     "switchback",
		SemanticEffort:  "balanced",
		NativeEffort:    "medium",
		RemainingBudget: &parent.Budget,
		Budget: ExecutionBudget{
			MaxRequests: parent.Budget.MaxRequests + 1,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "child_intent.budget.max_requests") {
		t.Fatalf("budget expansion error = %v", err)
	}

	childBudget := parent.Budget
	childBudget.MaxRequests--
	childBudget.MaxInputTokens--
	childBudget.MaxOutputTokens--
	childBudget.MaxDurationSeconds--
	childBudget.MaxUSD = executionBudgetUSD(*parent.Budget.MaxUSD - 1)
	childBudget.MaxParallelism = 1
	child, err := DeriveChildExecutionProfile(parent, ChildExecutionProfileIntent{
		Role:            "scout",
		CapabilityClass: "classify-extract",
		ClientHarnessID: "codex",
		TransportID:     "switchback",
		ModelConstraint: "openai/gpt-5.6-sol",
		SemanticEffort:  "balanced",
		NativeEffort:    "medium",
		RemainingBudget: &parent.Budget,
		Budget:          childBudget,
	})
	if err != nil {
		t.Fatalf("narrow child: %v", err)
	}
	if child.ParentPlanID != parent.ID || child.ID == parent.ID || !reflect.DeepEqual(child.Budget, childBudget) {
		t.Fatalf("child identity/ceiling mismatch: parent=%+v child=%+v", parent, child)
	}
	if child.RequestedIntent.ClientHarnessID != "codex" ||
		child.RequestedIntent.TransportID != "switchback" ||
		child.RequestedIntent.ModelConstraint != "openai/gpt-5.6-sol" ||
		child.RequestedIntent.NativeEffort != "medium" {
		t.Fatalf("child selectors were not explicit in the receipt-shaped request: %+v", child.RequestedIntent)
	}
	if child.EffectiveProvider != UnknownUntilReceipt || child.EffectiveModel != UnknownUntilReceipt || child.EffectiveNativeEffort != UnknownUntilReceipt {
		t.Fatalf("child claimed effective execution before receipt: %+v", child)
	}
}

func TestDeriveChildExecutionProfileRequiresAndEnforcesRemainingBudget(t *testing.T) {
	parent, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseChild := ChildExecutionProfileIntent{
		ClientHarnessID: "codex",
		TransportID:     "native",
		SemanticEffort:  "balanced",
		NativeEffort:    "medium",
	}

	_, err = DeriveChildExecutionProfile(parent, baseChild)
	if err == nil || !strings.Contains(err.Error(), "child_intent.remaining_budget") {
		t.Fatalf("missing remaining budget error = %v", err)
	}

	expandedRemaining := parent.Budget
	expandedRemaining.MaxRequests++
	child := baseChild
	child.RemainingBudget = &expandedRemaining
	_, err = DeriveChildExecutionProfile(parent, child)
	if err == nil || !strings.Contains(err.Error(), "child_intent.remaining_budget.max_requests") {
		t.Fatalf("expanded remaining budget error = %v", err)
	}

	remaining := parent.Budget
	remaining.MaxRequests--
	child.RemainingBudget = &remaining
	child.Budget.MaxRequests = parent.Budget.MaxRequests
	_, err = DeriveChildExecutionProfile(parent, child)
	if err == nil || !strings.Contains(err.Error(), "child_intent.budget.max_requests") || !strings.Contains(err.Error(), "remaining") {
		t.Fatalf("remaining budget ceiling error = %v", err)
	}

	child.Budget.MaxRequests = remaining.MaxRequests
	plan, err := DeriveChildExecutionProfile(parent, child)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RemainingBudget == nil || plan.RemainingBudget.MaxRequests != remaining.MaxRequests {
		t.Fatalf("child plan did not represent derivation remaining budget: %+v", plan.RemainingBudget)
	}
}

func TestDeriveChildExecutionProfileRejectsInvalidBudgets(t *testing.T) {
	parent, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		mutate   func(*ChildExecutionProfileIntent)
		wantPath string
		wantText string
	}{
		{
			name: "negative request budget",
			mutate: func(child *ChildExecutionProfileIntent) {
				child.Budget.MaxRequests = -1
			},
			wantPath: "child_intent.budget.max_requests",
			wantText: "positive",
		},
		{
			name: "NaN request USD budget",
			mutate: func(child *ChildExecutionProfileIntent) {
				child.Budget.MaxUSD = executionBudgetUSD(math.NaN())
			},
			wantPath: "child_intent.budget.max_usd",
			wantText: "finite",
		},
		{
			name: "infinite request USD budget",
			mutate: func(child *ChildExecutionProfileIntent) {
				child.Budget.MaxUSD = executionBudgetUSD(math.Inf(1))
			},
			wantPath: "child_intent.budget.max_usd",
			wantText: "finite",
		},
		{
			name: "negative request USD budget",
			mutate: func(child *ChildExecutionProfileIntent) {
				child.Budget.MaxUSD = executionBudgetUSD(-1)
			},
			wantPath: "child_intent.budget.max_usd",
			wantText: "non-negative",
		},
		{
			name: "NaN remaining USD budget",
			mutate: func(child *ChildExecutionProfileIntent) {
				remaining := *child.RemainingBudget
				remaining.MaxUSD = executionBudgetUSD(math.NaN())
				child.RemainingBudget = &remaining
			},
			wantPath: "child_intent.remaining_budget.max_usd",
			wantText: "finite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remaining := parent.Budget
			child := ChildExecutionProfileIntent{
				ClientHarnessID: "codex",
				TransportID:     "native",
				SemanticEffort:  "balanced",
				NativeEffort:    "medium",
				RemainingBudget: &remaining,
			}
			tt.mutate(&child)
			_, err := DeriveChildExecutionProfile(parent, child)
			if err == nil || !strings.Contains(err.Error(), tt.wantPath) || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("invalid budget error = %v, want path %q and text %q", err, tt.wantPath, tt.wantText)
			}
		})
	}
}

func TestDeriveChildExecutionProfileExplicitZeroUSDNarrowsParent(t *testing.T) {
	parent, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
	})
	if err != nil {
		t.Fatal(err)
	}
	remaining := parent.Budget
	plan, err := DeriveChildExecutionProfile(parent, ChildExecutionProfileIntent{
		ClientHarnessID: "codex",
		TransportID:     "native",
		SemanticEffort:  "balanced",
		NativeEffort:    "medium",
		RemainingBudget: &remaining,
		Budget: ExecutionBudget{
			MaxUSD: executionBudgetUSD(0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Budget.MaxUSD == nil || *plan.Budget.MaxUSD != 0 {
		t.Fatalf("explicit zero USD was inherited instead of narrowed: %+v", plan.Budget.MaxUSD)
	}
}

func TestResolveExecutionProfileCarriesValidatedPrivacyCeiling(t *testing.T) {
	plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
		PrivacyCeiling:  "sensitive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.PrivacyCeiling != "sensitive" || plan.RequestedIntent.PrivacyCeiling != "sensitive" {
		t.Fatalf("root privacy ceiling was not preserved: %+v", plan)
	}

	_, err = ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
		PrivacyCeiling:  "confidential",
	})
	if err == nil || !strings.Contains(err.Error(), "requested_intent.privacy_ceiling") {
		t.Fatalf("invalid root privacy ceiling error = %v", err)
	}
}

func TestDeriveChildExecutionProfileOnlyNarrowsPrivacyCeiling(t *testing.T) {
	parent, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:       "coding-implementation",
		ClientHarnessID: "codex",
		TransportID:     "native",
		PrivacyCeiling:  "private",
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		privacy     string
		wantPrivacy string
		wantErr     bool
	}{
		{name: "omitted inherits", wantPrivacy: "private"},
		{name: "equal is allowed", privacy: "private", wantPrivacy: "private"},
		{name: "public narrows private", privacy: "public", wantPrivacy: "public"},
		{name: "sensitive expands private", privacy: "sensitive", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remaining := parent.Budget
			plan, err := DeriveChildExecutionProfile(parent, ChildExecutionProfileIntent{
				ClientHarnessID: "codex",
				TransportID:     "native",
				SemanticEffort:  "balanced",
				NativeEffort:    "medium",
				PrivacyCeiling:  tt.privacy,
				RemainingBudget: &remaining,
			})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "child_intent.privacy_ceiling") {
					t.Fatalf("privacy expansion error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.PrivacyCeiling != tt.wantPrivacy {
				t.Fatalf("child privacy = %q, want %q", plan.PrivacyCeiling, tt.wantPrivacy)
			}
			if !strings.Contains(strings.Join(plan.InheritedCeilings, "\n"), "privacy_ceiling") {
				t.Fatalf("privacy ceiling absent from inherited ceilings: %v", plan.InheritedCeilings)
			}
		})
	}
}

func TestExecutionProfilePlanIDPropagatesMarshalErrors(t *testing.T) {
	_, err := executionProfilePlanID(ExecutionProfilePlan{
		Budget: ExecutionBudget{MaxUSD: executionBudgetUSD(math.NaN())},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("plan ID marshal error = %v", err)
	}
}

func executionBudgetUSD(value float64) *float64 {
	return &value
}

func TestObservedExecutionsIndependentRejectsSameProviderModel(t *testing.T) {
	left := ObservedExecutionIdentity{Provider: "openai", Model: "gpt-5.6-sol"}
	if ObservedExecutionsIndependent(left, left) {
		t.Fatal("two selectors landing on the same observed provider/model counted as independent")
	}
	if !ObservedExecutionsIndependent(left, ObservedExecutionIdentity{Provider: "anthropic", Model: "claude-opus"}) {
		t.Fatal("different observed provider/model identities were not independent")
	}
}

func TestExecutionProfileResolverLeavesCapabilityRouteOrderingUntouched(t *testing.T) {
	home := t.TempDir()
	if _, err := ImportModelCapabilitiesYAML(home, "../../fixtures/route/model-capabilities.yaml", t0); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanCapabilityRoute(home, "judge-critique", CapabilityRoutePlanOptions{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(plan.Chain))
	for _, lane := range plan.Chain {
		got = append(got, lane.ModelID)
	}
	want := []string{"auto/judge", "deepseek/deepseek-v4-pro", "scout/chat"}
	if len(got) != len(want) {
		t.Fatalf("route chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("route chain = %v, want %v", got, want)
		}
	}
}
