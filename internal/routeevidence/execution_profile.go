package routeevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	ExecutionProfilePlanSchema = "compound.execution_profile_plan.v0.1"
	UnknownUntilReceipt        = "unknown_until_receipt"
)

const (
	dispositionExact      = "exact"
	dispositionTranslated = "translated"
	dispositionDegraded   = "degraded"
)

type ExecutionProfileIntent struct {
	ProfileID           string `json:"profile_id"`
	Role                string `json:"role,omitempty"`
	CapabilityClass     string `json:"capability_class,omitempty"`
	ClientHarnessID     string `json:"client_harness_id,omitempty"`
	TransportID         string `json:"transport_id,omitempty"`
	RouteAlias          string `json:"route_alias,omitempty"`
	ModelConstraint     string `json:"model_constraint,omitempty"`
	SemanticEffort      string `json:"semantic_effort,omitempty"`
	NativeEffort        string `json:"native_effort,omitempty"`
	OrchestrationPolicy string `json:"orchestration_policy,omitempty"`
	PrivacyCeiling      string `json:"privacy_ceiling,omitempty"`
}

type ExecutionBudget struct {
	MaxRequests        int      `json:"max_requests"`
	MaxInputTokens     int      `json:"max_input_tokens"`
	MaxOutputTokens    int      `json:"max_output_tokens"`
	MaxDurationSeconds int      `json:"max_duration_seconds"`
	MaxUSD             *float64 `json:"max_usd,omitempty"`
	MaxParallelism     int      `json:"max_parallelism"`
}

type ExecutionCheckpointPolicy struct {
	Required       bool `json:"required"`
	EveryRequests  int  `json:"every_requests,omitempty"`
	PersistPartial bool `json:"persist_partial"`
}

type NativeEffortRequest struct {
	Requested   string `json:"requested"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

type ExecutionProfileCandidate struct {
	ClientHarnessID string `json:"client_harness_id"`
	TransportID     string `json:"transport_id"`
	SemanticEffort  string `json:"semantic_effort"`
	NativeEffort    string `json:"native_effort"`
	Terminal        bool   `json:"terminal"`
	Explanation     string `json:"explanation"`
}

type ExecutionIndependenceRequirement struct {
	CrossModelRequired bool   `json:"cross_model_required"`
	Basis              string `json:"basis,omitempty"`
}

type ExecutionProfileDefinition struct {
	ID                    string                           `json:"id"`
	Role                  string                           `json:"role"`
	CapabilityClass       string                           `json:"capability_class"`
	DefaultClientHarness  string                           `json:"default_client_harness_id"`
	DefaultTransport      string                           `json:"default_transport_id"`
	SemanticEffort        string                           `json:"semantic_effort"`
	OrchestrationPolicy   string                           `json:"orchestration_policy"`
	PrivacyCeiling        string                           `json:"privacy_ceiling"`
	NativeEffortByHarness map[string]string                `json:"native_effort_by_harness"`
	Budget                ExecutionBudget                  `json:"budget"`
	Checkpoint            ExecutionCheckpointPolicy        `json:"checkpoint"`
	Candidates            []ExecutionProfileCandidate      `json:"candidates"`
	Independence          ExecutionIndependenceRequirement `json:"independence_requirement"`
	Explanation           string                           `json:"explanation"`
}

type ExecutionProfilePlan struct {
	SchemaVersion           string                           `json:"schema_version"`
	ID                      string                           `json:"id"`
	RequestedIntent         ExecutionProfileIntent           `json:"requested_intent"`
	ProfileID               string                           `json:"profile_id"`
	Role                    string                           `json:"role"`
	CapabilityClass         string                           `json:"capability_class"`
	ClientHarnessID         string                           `json:"client_harness_id"`
	TransportID             string                           `json:"transport_id"`
	RouteAlias              string                           `json:"route_alias,omitempty"`
	ModelConstraint         string                           `json:"model_constraint,omitempty"`
	SemanticEffort          string                           `json:"semantic_effort"`
	NativeEffort            NativeEffortRequest              `json:"native_effort"`
	OrchestrationPolicy     string                           `json:"orchestration_policy"`
	PrivacyCeiling          string                           `json:"privacy_ceiling"`
	Disposition             string                           `json:"disposition"`
	Budget                  ExecutionBudget                  `json:"budget"`
	Checkpoint              ExecutionCheckpointPolicy        `json:"checkpoint"`
	Candidates              []ExecutionProfileCandidate      `json:"candidates"`
	CandidateExplanation    string                           `json:"candidate_explanation"`
	ParentPlanID            string                           `json:"parent_plan_id,omitempty"`
	RemainingBudget         *ExecutionBudget                 `json:"remaining_budget,omitempty"`
	InheritedCeilings       []string                         `json:"inherited_ceilings,omitempty"`
	IndependenceRequirement ExecutionIndependenceRequirement `json:"independence_requirement"`
	EffectiveProvider       string                           `json:"effective_provider"`
	EffectiveModel          string                           `json:"effective_model"`
	EffectiveNativeEffort   string                           `json:"effective_native_effort"`
	Warnings                []string                         `json:"warnings,omitempty"`
}

type ChildExecutionProfileIntent struct {
	Role                string           `json:"role,omitempty"`
	CapabilityClass     string           `json:"capability_class,omitempty"`
	ClientHarnessID     string           `json:"client_harness_id"`
	TransportID         string           `json:"transport_id"`
	RouteAlias          string           `json:"route_alias,omitempty"`
	ModelConstraint     string           `json:"model_constraint,omitempty"`
	SemanticEffort      string           `json:"semantic_effort"`
	NativeEffort        string           `json:"native_effort,omitempty"`
	OrchestrationPolicy string           `json:"orchestration_policy,omitempty"`
	PrivacyCeiling      string           `json:"privacy_ceiling,omitempty"`
	RemainingBudget     *ExecutionBudget `json:"remaining_budget,omitempty"`
	Budget              ExecutionBudget  `json:"budget"`
}

type ObservedExecutionIdentity struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type ExecutionProfileCompatibilityError struct {
	Path   string
	Reason string
}

func (e *ExecutionProfileCompatibilityError) Error() string {
	return e.Path + ": " + e.Reason
}

func ListExecutionProfiles() []ExecutionProfileDefinition {
	profiles := []ExecutionProfileDefinition{
		{
			ID: "interactive-default", Role: "interactive", CapabilityClass: "interactive",
			DefaultClientHarness: "claude-code", DefaultTransport: "native",
			SemanticEffort: "balanced", OrchestrationPolicy: "standard", PrivacyCeiling: "private",
			NativeEffortByHarness: map[string]string{"claude-code": "medium", "codex": "medium", "worker": "medium"},
			Budget:                ExecutionBudget{MaxRequests: 8, MaxInputTokens: 120_000, MaxOutputTokens: 24_000, MaxDurationSeconds: 1_200, MaxUSD: usdCeiling(5), MaxParallelism: 1},
			Checkpoint:            ExecutionCheckpointPolicy{},
			Explanation:           "interactive work favors a bounded balanced request and standard orchestration",
		},
		{
			ID: "coding-implementation", Role: "executor", CapabilityClass: "build-implement",
			DefaultClientHarness: "codex", DefaultTransport: "native",
			SemanticEffort: "deep", OrchestrationPolicy: "standard", PrivacyCeiling: "private",
			NativeEffortByHarness: map[string]string{"claude-code": "xhigh", "codex": "xhigh", "worker": "high"},
			Budget:                ExecutionBudget{MaxRequests: 40, MaxInputTokens: 1_000_000, MaxOutputTokens: 200_000, MaxDurationSeconds: 7_200, MaxUSD: usdCeiling(25), MaxParallelism: 2},
			Checkpoint:            ExecutionCheckpointPolicy{},
			Explanation:           "implementation maps semantic depth through the selected harness without opting into ultracode",
		},
		{
			ID: "mining-bulk", Role: "miner", CapabilityClass: "classify-extract",
			DefaultClientHarness: "codex", DefaultTransport: "switchback",
			SemanticEffort: "economy", OrchestrationPolicy: "standard", PrivacyCeiling: "private",
			NativeEffortByHarness: map[string]string{"claude-code": "low", "codex": "low", "worker": "low"},
			Budget:                ExecutionBudget{MaxRequests: 500, MaxInputTokens: 10_000_000, MaxOutputTokens: 2_000_000, MaxDurationSeconds: 14_400, MaxUSD: usdCeiling(30), MaxParallelism: 16},
			Checkpoint:            ExecutionCheckpointPolicy{Required: true, EveryRequests: 20, PersistPartial: true},
			Candidates:            []ExecutionProfileCandidate{{ClientHarnessID: "codex", TransportID: "switchback", SemanticEffort: "economy", NativeEffort: "low", Terminal: true, Explanation: "bounded Codex client over Switchback transport; construction leaves execution unknown until receipt"}},
			Explanation:           "bulk mining prefers cheap transport, bounded parallelism, and persistent checkpoints",
		},
		{
			ID: "judge-critique", Role: "judge", CapabilityClass: "judge-critique",
			DefaultClientHarness: "claude-code", DefaultTransport: "native",
			SemanticEffort: "deep", OrchestrationPolicy: "standard", PrivacyCeiling: "private",
			NativeEffortByHarness: map[string]string{"claude-code": "xhigh", "codex": "xhigh", "worker": "high"},
			Budget:                ExecutionBudget{MaxRequests: 20, MaxInputTokens: 400_000, MaxOutputTokens: 80_000, MaxDurationSeconds: 3_600, MaxUSD: usdCeiling(20), MaxParallelism: 3},
			Checkpoint:            ExecutionCheckpointPolicy{},
			Independence:          ExecutionIndependenceRequirement{CrossModelRequired: true, Basis: "terminal critique requires distinct observed provider/model identities; free lanes are tripwire-only"},
			Explanation:           "critique uses deep semantic effort; free candidates may object but cannot terminate judgment",
		},
		{
			ID: "orchestration-ultracode", Role: "orchestrator", CapabilityClass: "plan-decompose",
			DefaultClientHarness: "codex", DefaultTransport: "native",
			SemanticEffort: "deep", OrchestrationPolicy: "ultracode", PrivacyCeiling: "private",
			NativeEffortByHarness: map[string]string{"claude-code": "xhigh", "codex": "xhigh", "worker": "high"},
			Budget:                ExecutionBudget{MaxRequests: 80, MaxInputTokens: 2_000_000, MaxOutputTokens: 400_000, MaxDurationSeconds: 10_800, MaxUSD: usdCeiling(50), MaxParallelism: 4},
			Checkpoint:            ExecutionCheckpointPolicy{Required: true, EveryRequests: 20, PersistPartial: true},
			Explanation:           "ultracode is an explicit orchestration policy, never a model or native effort value",
		},
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return profiles
}

func ResolveExecutionProfile(intent ExecutionProfileIntent) (ExecutionProfilePlan, error) {
	requested := intent
	definition, ok := executionProfileDefinition(strings.TrimSpace(intent.ProfileID))
	if !ok {
		return ExecutionProfilePlan{}, compatibilityError("requested_intent.profile_id", "unknown execution profile "+quoted(intent.ProfileID))
	}

	intent.ProfileID = definition.ID
	intent.Role = defaultString(intent.Role, definition.Role)
	intent.CapabilityClass = defaultString(intent.CapabilityClass, definition.CapabilityClass)
	intent.ClientHarnessID = defaultString(intent.ClientHarnessID, definition.DefaultClientHarness)
	intent.TransportID = defaultString(intent.TransportID, definition.DefaultTransport)
	intent.SemanticEffort = defaultString(intent.SemanticEffort, definition.SemanticEffort)
	intent.OrchestrationPolicy = defaultString(intent.OrchestrationPolicy, definition.OrchestrationPolicy)
	intent.PrivacyCeiling = defaultString(intent.PrivacyCeiling, definition.PrivacyCeiling)

	if err := validateExecutionSelectors(intent, definition); err != nil {
		return ExecutionProfilePlan{}, err
	}
	if err := validateConcreteBudget("profile.budget", definition.Budget); err != nil {
		return ExecutionProfilePlan{}, err
	}
	nativeEffort, err := resolveNativeEffort(
		definition,
		intent.ClientHarnessID,
		intent.SemanticEffort,
		intent.NativeEffort,
		"requested_intent.native_effort",
	)
	if err != nil {
		return ExecutionProfilePlan{}, err
	}

	plan := ExecutionProfilePlan{
		SchemaVersion:           ExecutionProfilePlanSchema,
		RequestedIntent:         requested,
		ProfileID:               definition.ID,
		Role:                    intent.Role,
		CapabilityClass:         intent.CapabilityClass,
		ClientHarnessID:         intent.ClientHarnessID,
		TransportID:             intent.TransportID,
		RouteAlias:              strings.TrimSpace(intent.RouteAlias),
		ModelConstraint:         strings.TrimSpace(intent.ModelConstraint),
		SemanticEffort:          intent.SemanticEffort,
		NativeEffort:            nativeEffort,
		OrchestrationPolicy:     intent.OrchestrationPolicy,
		PrivacyCeiling:          intent.PrivacyCeiling,
		Disposition:             nativeEffort.Disposition,
		Budget:                  definition.Budget,
		Checkpoint:              definition.Checkpoint,
		Candidates:              append([]ExecutionProfileCandidate(nil), definition.Candidates...),
		CandidateExplanation:    definition.Explanation,
		IndependenceRequirement: definition.Independence,
		EffectiveProvider:       UnknownUntilReceipt,
		EffectiveModel:          UnknownUntilReceipt,
		EffectiveNativeEffort:   UnknownUntilReceipt,
	}
	if plan.TransportID == "switchback" {
		plan.Warnings = append(plan.Warnings, "Switchback is transport only; effective provider, model, and native effort remain unknown until receipt")
	}
	if plan.ProfileID == "judge-critique" {
		plan.Warnings = append(plan.Warnings, "free judge candidates are tripwire-only and cannot provide a terminal verdict")
	}
	plan.ID, err = executionProfilePlanID(plan)
	if err != nil {
		return ExecutionProfilePlan{}, fmt.Errorf("generate execution profile plan id: %w", err)
	}
	return plan, nil
}

func DeriveChildExecutionProfile(parent ExecutionProfilePlan, child ChildExecutionProfileIntent) (ExecutionProfilePlan, error) {
	if strings.TrimSpace(parent.ID) == "" {
		return ExecutionProfilePlan{}, compatibilityError("parent_plan.id", "parent execution plan id is required")
	}
	if strings.TrimSpace(child.ClientHarnessID) == "" {
		return ExecutionProfilePlan{}, compatibilityError("child_intent.client_harness_id", "child selector must be explicit; harness is not inherited")
	}
	if strings.TrimSpace(child.TransportID) == "" {
		return ExecutionProfilePlan{}, compatibilityError("child_intent.transport_id", "child selector must be explicit; transport is not inherited")
	}
	if strings.TrimSpace(child.SemanticEffort) == "" {
		return ExecutionProfilePlan{}, compatibilityError("child_intent.semantic_effort", "child effort must be explicit")
	}
	if child.RemainingBudget == nil {
		return ExecutionProfilePlan{}, compatibilityError("child_intent.remaining_budget", "explicit remaining budget is required")
	}
	parentPrivacyRank := privacyRank(parent.PrivacyCeiling)
	if parentPrivacyRank == 0 {
		return ExecutionProfilePlan{}, compatibilityError("parent_plan.privacy_ceiling", "parent privacy ceiling is unsupported")
	}
	childPrivacy := defaultString(child.PrivacyCeiling, parent.PrivacyCeiling)
	childPrivacyRank := privacyRank(childPrivacy)
	if childPrivacyRank == 0 {
		return ExecutionProfilePlan{}, compatibilityError("child_intent.privacy_ceiling", "unsupported privacy ceiling "+quoted(childPrivacy))
	}
	if childPrivacyRank > parentPrivacyRank {
		return ExecutionProfilePlan{}, compatibilityError("child_intent.privacy_ceiling", "child privacy ceiling cannot expand the parent ceiling")
	}
	if effortRank(child.SemanticEffort) > effortRank(parent.SemanticEffort) {
		return ExecutionProfilePlan{}, compatibilityError("child_intent.semantic_effort", "child semantic effort cannot exceed the parent")
	}
	if parent.OrchestrationPolicy == "ultracode" && strings.TrimSpace(child.OrchestrationPolicy) == "" {
		return ExecutionProfilePlan{}, compatibilityError("child_intent.orchestration_policy", "ultracode is not inherited; child opt-in must be explicit")
	}
	definition, ok := executionProfileDefinition(parent.ProfileID)
	if !ok {
		return ExecutionProfilePlan{}, compatibilityError("parent_plan.profile_id", "unknown execution profile "+quoted(parent.ProfileID))
	}
	if _, err := resolveNativeEffort(
		definition,
		strings.TrimSpace(child.ClientHarnessID),
		strings.TrimSpace(child.SemanticEffort),
		child.NativeEffort,
		"child_intent.native_effort",
	); err != nil {
		return ExecutionProfilePlan{}, err
	}

	remainingBudget, budget, err := childBudgetWithinCeiling(parent.Budget, *child.RemainingBudget, child.Budget)
	if err != nil {
		return ExecutionProfilePlan{}, err
	}
	role := defaultString(child.Role, parent.Role)
	capabilityClass := defaultString(child.CapabilityClass, parent.CapabilityClass)
	plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:           parent.ProfileID,
		Role:                role,
		CapabilityClass:     capabilityClass,
		ClientHarnessID:     child.ClientHarnessID,
		TransportID:         child.TransportID,
		RouteAlias:          child.RouteAlias,
		ModelConstraint:     child.ModelConstraint,
		SemanticEffort:      child.SemanticEffort,
		NativeEffort:        child.NativeEffort,
		OrchestrationPolicy: child.OrchestrationPolicy,
		PrivacyCeiling:      childPrivacy,
	})
	if err != nil {
		return ExecutionProfilePlan{}, err
	}
	parentNativeRank := nativeEffortRank(parent.NativeEffort.Requested)
	if parentNativeRank == 0 {
		return ExecutionProfilePlan{}, compatibilityError("parent_plan.native_effort", "parent native effort is unsupported")
	}
	if nativeEffortRank(plan.NativeEffort.Requested) > parentNativeRank {
		return ExecutionProfilePlan{}, compatibilityError(
			"child_intent.native_effort",
			fmt.Sprintf("child native effort %q exceeds parent native ceiling %q", plan.NativeEffort.Requested, parent.NativeEffort.Requested),
		)
	}
	plan.RequestedIntent = ExecutionProfileIntent{
		ProfileID:           parent.ProfileID,
		Role:                child.Role,
		CapabilityClass:     child.CapabilityClass,
		ClientHarnessID:     child.ClientHarnessID,
		TransportID:         child.TransportID,
		RouteAlias:          child.RouteAlias,
		ModelConstraint:     child.ModelConstraint,
		SemanticEffort:      child.SemanticEffort,
		NativeEffort:        child.NativeEffort,
		OrchestrationPolicy: child.OrchestrationPolicy,
		PrivacyCeiling:      child.PrivacyCeiling,
	}
	plan.ParentPlanID = parent.ID
	plan.RemainingBudget = &remainingBudget
	plan.InheritedCeilings = []string{"privacy_ceiling", "remaining_budget.max_requests", "remaining_budget.max_input_tokens", "remaining_budget.max_output_tokens", "remaining_budget.max_duration_seconds", "remaining_budget.max_usd", "remaining_budget.max_parallelism"}
	plan.Budget = budget
	plan.ID, err = executionProfilePlanID(plan)
	if err != nil {
		return ExecutionProfilePlan{}, fmt.Errorf("generate child execution profile plan id: %w", err)
	}
	return plan, nil
}

func ObservedExecutionsIndependent(left, right ObservedExecutionIdentity) bool {
	leftProvider := strings.TrimSpace(left.Provider)
	leftModel := strings.TrimSpace(left.Model)
	rightProvider := strings.TrimSpace(right.Provider)
	rightModel := strings.TrimSpace(right.Model)
	if leftProvider == "" || leftModel == "" || rightProvider == "" || rightModel == "" {
		return false
	}
	return leftProvider != rightProvider || leftModel != rightModel
}

func executionProfileDefinition(id string) (ExecutionProfileDefinition, bool) {
	for _, profile := range ListExecutionProfiles() {
		if profile.ID == id {
			return profile, true
		}
	}
	return ExecutionProfileDefinition{}, false
}

func validateExecutionSelectors(intent ExecutionProfileIntent, definition ExecutionProfileDefinition) error {
	if !oneOf(intent.ClientHarnessID, "claude-code", "codex", "worker") {
		reason := "unsupported client harness " + quoted(intent.ClientHarnessID)
		if intent.ClientHarnessID == "switchback" {
			reason = "switchback is a transport, not a client harness"
		}
		return compatibilityError("requested_intent.client_harness_id", reason)
	}
	if !oneOf(intent.TransportID, "native", "switchback", "local") {
		return compatibilityError("requested_intent.transport_id", "unsupported transport "+quoted(intent.TransportID))
	}
	if intent.ClientHarnessID == "claude-code" && intent.TransportID == "local" {
		return compatibilityError("requested_intent.transport_id", "claude-code supports native or switchback transport in this binding")
	}
	if intent.ClientHarnessID == "codex" && intent.TransportID == "local" {
		return compatibilityError("requested_intent.transport_id", "codex supports native or switchback transport in this binding")
	}
	if !oneOf(intent.SemanticEffort, "economy", "balanced", "deep", "max") {
		return compatibilityError("requested_intent.semantic_effort", "unsupported semantic effort "+quoted(intent.SemanticEffort))
	}
	if !oneOf(intent.OrchestrationPolicy, "direct", "standard", "ultracode") {
		return compatibilityError("requested_intent.orchestration_policy", "unsupported orchestration policy "+quoted(intent.OrchestrationPolicy))
	}
	if privacyRank(intent.PrivacyCeiling) == 0 {
		return compatibilityError("requested_intent.privacy_ceiling", "unsupported privacy ceiling "+quoted(intent.PrivacyCeiling))
	}
	if intent.OrchestrationPolicy == "ultracode" && definition.ID != "orchestration-ultracode" {
		return compatibilityError("requested_intent.orchestration_policy", "ultracode requires explicit profile orchestration-ultracode")
	}
	if strings.TrimSpace(intent.RouteAlias) != "" {
		if intent.TransportID != "switchback" {
			return compatibilityError("requested_intent.route_alias", "route aliases require switchback transport")
		}
		if !fullyQualifiedSelector(intent.RouteAlias) {
			return compatibilityError("requested_intent.route_alias", "route alias must be owner-qualified (for example auto/extract)")
		}
	}
	if model := strings.TrimSpace(intent.ModelConstraint); model != "" && !fullyQualifiedSelector(model) {
		return compatibilityError("requested_intent.model_constraint", "model constraint must be a fully qualified provider/model selector; friendly aliases are unowned")
	}
	return nil
}

func supportsNativeEffort(harness, effort string) bool {
	switch harness {
	case "claude-code":
		return oneOf(effort, "low", "medium", "high", "xhigh", "max")
	case "codex":
		return oneOf(effort, "low", "medium", "high", "xhigh", "ultra")
	case "worker":
		return oneOf(effort, "low", "medium", "high")
	default:
		return false
	}
}

func resolveNativeEffort(definition ExecutionProfileDefinition, harness, semanticEffort, requested, path string) (NativeEffortRequest, error) {
	mapped := translatedNativeEffort(definition, harness, semanticEffort)
	if !supportsNativeEffort(harness, mapped) {
		return NativeEffortRequest{}, compatibilityError(
			path,
			fmt.Sprintf("%s does not support mapped native effort %q", harness, mapped),
		)
	}

	requested = strings.TrimSpace(requested)
	if requested != "" {
		if !supportsNativeEffort(harness, requested) {
			return NativeEffortRequest{}, compatibilityError(
				path,
				fmt.Sprintf("%s does not support native effort %q", harness, requested),
			)
		}
		if requested != mapped {
			return NativeEffortRequest{}, compatibilityError(
				path,
				fmt.Sprintf("native effort %q contradicts semantic effort %q; %s maps it to %q", requested, semanticEffort, harness, mapped),
			)
		}
	}

	disposition := dispositionTranslated
	reason := fmt.Sprintf("semantic effort %s maps to %s for %s", semanticEffort, mapped, harness)
	if requested != "" {
		disposition = dispositionExact
		reason = "explicit native effort matches the semantic effort mapping for the selected client adapter"
	}
	if nativeEffortRank(mapped) < effortRank(semanticEffort) {
		disposition = dispositionDegraded
		reason = fmt.Sprintf("semantic effort %s is capped at native effort %s for %s", semanticEffort, mapped, harness)
	}
	return NativeEffortRequest{Requested: mapped, Disposition: disposition, Reason: reason}, nil
}

func translatedNativeEffort(definition ExecutionProfileDefinition, harness, semanticEffort string) string {
	if semanticEffort == definition.SemanticEffort {
		return definition.NativeEffortByHarness[harness]
	}
	switch semanticEffort {
	case "economy":
		return "low"
	case "balanced":
		return "medium"
	case "deep":
		if harness == "worker" {
			return "high"
		}
		return "xhigh"
	case "max":
		switch harness {
		case "claude-code":
			return "max"
		case "worker":
			return "high"
		default:
			return "xhigh"
		}
	default:
		return ""
	}
}

func childBudgetWithinCeiling(parent, remaining, requested ExecutionBudget) (ExecutionBudget, ExecutionBudget, error) {
	parent = cloneExecutionBudget(parent)
	remaining = cloneExecutionBudget(remaining)
	if err := validateConcreteBudget("parent_plan.budget", parent); err != nil {
		return ExecutionBudget{}, ExecutionBudget{}, err
	}
	if err := validateConcreteBudget("child_intent.remaining_budget", remaining); err != nil {
		return ExecutionBudget{}, ExecutionBudget{}, err
	}
	if err := validateBudgetWithinCeiling(parent, remaining, "child_intent.remaining_budget", "parent"); err != nil {
		return ExecutionBudget{}, ExecutionBudget{}, err
	}

	child := cloneExecutionBudget(requested)
	inheritInt := func(value, ceiling int) int {
		if value == 0 {
			return ceiling
		}
		return value
	}
	child.MaxRequests = inheritInt(child.MaxRequests, remaining.MaxRequests)
	child.MaxInputTokens = inheritInt(child.MaxInputTokens, remaining.MaxInputTokens)
	child.MaxOutputTokens = inheritInt(child.MaxOutputTokens, remaining.MaxOutputTokens)
	child.MaxDurationSeconds = inheritInt(child.MaxDurationSeconds, remaining.MaxDurationSeconds)
	child.MaxParallelism = inheritInt(child.MaxParallelism, remaining.MaxParallelism)
	if child.MaxUSD == nil {
		child.MaxUSD = cloneFloat64(remaining.MaxUSD)
	}
	if err := validateConcreteBudget("child_intent.budget", child); err != nil {
		return ExecutionBudget{}, ExecutionBudget{}, err
	}
	if err := validateBudgetWithinCeiling(remaining, child, "child_intent.budget", "remaining"); err != nil {
		return ExecutionBudget{}, ExecutionBudget{}, err
	}
	return remaining, child, nil
}

func validateConcreteBudget(path string, budget ExecutionBudget) error {
	checks := []struct {
		name  string
		value int
	}{
		{"max_requests", budget.MaxRequests},
		{"max_input_tokens", budget.MaxInputTokens},
		{"max_output_tokens", budget.MaxOutputTokens},
		{"max_duration_seconds", budget.MaxDurationSeconds},
		{"max_parallelism", budget.MaxParallelism},
	}
	for _, check := range checks {
		if check.value <= 0 {
			return compatibilityError(path+"."+check.name, fmt.Sprintf("budget value must be positive, got %d", check.value))
		}
	}
	if budget.MaxUSD == nil {
		return compatibilityError(path+".max_usd", "concrete budget must provide max_usd explicitly")
	}
	if math.IsNaN(*budget.MaxUSD) || math.IsInf(*budget.MaxUSD, 0) {
		return compatibilityError(path+".max_usd", "budget value must be finite")
	}
	if *budget.MaxUSD < 0 {
		return compatibilityError(path+".max_usd", "budget value must be non-negative")
	}
	return nil
}

func validateBudgetWithinCeiling(ceiling, value ExecutionBudget, path, ceilingName string) error {
	checks := []struct {
		name           string
		value, ceiling int
	}{
		{"max_requests", value.MaxRequests, ceiling.MaxRequests},
		{"max_input_tokens", value.MaxInputTokens, ceiling.MaxInputTokens},
		{"max_output_tokens", value.MaxOutputTokens, ceiling.MaxOutputTokens},
		{"max_duration_seconds", value.MaxDurationSeconds, ceiling.MaxDurationSeconds},
		{"max_parallelism", value.MaxParallelism, ceiling.MaxParallelism},
	}
	for _, check := range checks {
		if check.value > check.ceiling {
			return compatibilityError(path+"."+check.name, fmt.Sprintf("budget value %d exceeds %s ceiling %d", check.value, ceilingName, check.ceiling))
		}
	}
	if *value.MaxUSD > *ceiling.MaxUSD {
		return compatibilityError(path+".max_usd", fmt.Sprintf("budget value %.2f exceeds %s ceiling %.2f", *value.MaxUSD, ceilingName, *ceiling.MaxUSD))
	}
	return nil
}

func cloneExecutionBudget(budget ExecutionBudget) ExecutionBudget {
	budget.MaxUSD = cloneFloat64(budget.MaxUSD)
	return budget
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func usdCeiling(value float64) *float64 {
	return &value
}

func executionProfilePlanID(plan ExecutionProfilePlan) (string, error) {
	canonical := struct {
		ProfileID               string
		Role                    string
		CapabilityClass         string
		ClientHarnessID         string
		TransportID             string
		RouteAlias              string
		ModelConstraint         string
		SemanticEffort          string
		NativeEffort            string
		OrchestrationPolicy     string
		PrivacyCeiling          string
		Budget                  ExecutionBudget
		Checkpoint              ExecutionCheckpointPolicy
		ParentPlanID            string
		RemainingBudget         *ExecutionBudget
		IndependenceRequirement ExecutionIndependenceRequirement
	}{
		ProfileID: plan.ProfileID, Role: plan.Role, CapabilityClass: plan.CapabilityClass,
		ClientHarnessID: plan.ClientHarnessID, TransportID: plan.TransportID,
		RouteAlias: plan.RouteAlias, ModelConstraint: plan.ModelConstraint,
		SemanticEffort: plan.SemanticEffort, NativeEffort: plan.NativeEffort.Requested,
		OrchestrationPolicy: plan.OrchestrationPolicy, PrivacyCeiling: plan.PrivacyCeiling, Budget: plan.Budget,
		Checkpoint: plan.Checkpoint, ParentPlanID: plan.ParentPlanID,
		RemainingBudget:         plan.RemainingBudget,
		IndependenceRequirement: plan.IndependenceRequirement,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "epp_" + hex.EncodeToString(sum[:6]), nil
}

func compatibilityError(path, reason string) error {
	return &ExecutionProfileCompatibilityError{Path: path, Reason: reason}
}

func defaultString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func fullyQualifiedSelector(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return true
}

func effortRank(effort string) int {
	switch strings.TrimSpace(effort) {
	case "economy":
		return 1
	case "balanced":
		return 2
	case "deep":
		return 3
	case "max":
		return 4
	default:
		return 100
	}
}

func nativeEffortRank(effort string) int {
	switch strings.TrimSpace(effort) {
	case "low":
		return 1
	case "medium":
		return 2
	case "high", "xhigh":
		return 3
	case "max", "ultra":
		return 4
	default:
		return 0
	}
}

func privacyRank(privacy string) int {
	switch strings.TrimSpace(privacy) {
	case "public":
		return 1
	case "private":
		return 2
	case "sensitive":
		return 3
	case "secret":
		return 4
	default:
		return 0
	}
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func quoted(value string) string {
	return fmt.Sprintf("%q", strings.TrimSpace(value))
}
