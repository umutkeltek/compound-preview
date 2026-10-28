package hookplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"compound/internal/harness"
)

type HarnessIntention string

const (
	// IntentionPrepareContext is orientation taken once, when a session opens.
	IntentionPrepareContext HarnessIntention = "prepare_context"
	// IntentionPrepareTurnContext is injection repeated on every operator turn. It is a
	// separate intention from IntentionPrepareContext because the two are separately
	// wanted: a harness can carry session orientation without paying a context build per
	// prompt. Folding both into one intention made that choice unrepresentable, so a
	// harness could only take both or neither.
	IntentionPrepareTurnContext HarnessIntention = "prepare_turn_context"
	IntentionObserveAction      HarnessIntention = "observe_action"
	IntentionGuardMutation      HarnessIntention = "guard_mutation"
	IntentionObserveOutcome     HarnessIntention = "observe_outcome"
	IntentionRecordReceipt      HarnessIntention = "record_receipt"
)

type Disposition string

const (
	DispositionNative      Disposition = "native"
	DispositionFallback    Disposition = "fallback"
	DispositionDegraded    Disposition = "degraded"
	DispositionUnsupported Disposition = "unsupported"
)

type SemanticPosture struct {
	ID                 string             `json:"id"`
	Revision           string             `json:"revision"`
	AuthorityRef       string             `json:"authority_ref"`
	Harness            string             `json:"harness"`
	HandlerRef         string             `json:"handler_ref"`
	InvocationRevision string             `json:"invocation_revision,omitempty"`
	HandlerRevision    string             `json:"handler_revision"`
	Intentions         []HarnessIntention `json:"intentions"`
}

type HarnessAttestation struct {
	Harness          string          `json:"harness"`
	Family           string          `json:"family"`
	Surface          string          `json:"surface"`
	Version          string          `json:"version"`
	FeatureState     string          `json:"feature_state"`
	ConfigScope      string          `json:"config_scope"`
	Revision         string          `json:"revision"`
	FallbackRevision string          `json:"fallback_revision"`
	Features         map[string]bool `json:"features"`
}

type CompilationContext struct {
	PostureID           string `json:"posture_id"`
	PostureRevision     string `json:"posture_revision"`
	Harness             string `json:"harness"`
	HarnessFamily       string `json:"harness_family"`
	HarnessSurface      string `json:"harness_surface"`
	HarnessVersion      string `json:"harness_version"`
	HarnessFeatureState string `json:"harness_feature_state"`
	HarnessConfigScope  string `json:"harness_config_scope"`
	AttestationRevision string `json:"attestation_revision"`
	FallbackRevision    string `json:"fallback_revision"`
}

type CompiledIntention struct {
	Intention           HarnessIntention `json:"intention"`
	Disposition         Disposition      `json:"disposition"`
	EnforcementStrength string           `json:"enforcement_strength"`
	NativeEvents        []string         `json:"native_events,omitempty"`
	Reason              string           `json:"reason,omitempty"`
}

type CompiledPosture struct {
	HookPlan      HookPlan            `json:"hook_plan"`
	CompiledFrom  CompilationContext  `json:"compiled_from"`
	Intentions    []CompiledIntention `json:"intentions"`
	MutationReady bool                `json:"mutation_ready"`
}

type DefaultPostureRequest struct {
	Harness         string
	Registry        harness.Registry
	HandlerRef      string
	HandlerRevision string
	ConfigScope     string
}

type nativeBinding struct {
	event   string
	matcher string
}

type semanticHarnessAdapter interface {
	Compile(HarnessIntention, HarnessAttestation) (CompiledIntention, []nativeBinding)
	Attest(harness.Capabilities) map[string]bool
}

func unsupportedIntention(intention HarnessIntention, reason string) CompiledIntention {
	return CompiledIntention{
		Intention:           intention,
		Disposition:         DispositionUnsupported,
		EnforcementStrength: "none",
		Reason:              reason,
	}
}

func CompileSemanticPosture(
	posture SemanticPosture,
	attestation HarnessAttestation,
) (CompiledPosture, error) {
	if err := validateSemanticPosture(posture); err != nil {
		return CompiledPosture{}, err
	}
	if err := validateHarnessAttestation(posture, attestation); err != nil {
		return CompiledPosture{}, err
	}
	adapter, err := semanticAdapterFor(attestation.Family)
	if err != nil {
		return CompiledPosture{}, err
	}

	context := CompilationContext{
		PostureID:           posture.ID,
		PostureRevision:     posture.Revision,
		Harness:             attestation.Harness,
		HarnessFamily:       attestation.Family,
		HarnessSurface:      attestation.Surface,
		HarnessVersion:      attestation.Version,
		HarnessFeatureState: attestation.FeatureState,
		HarnessConfigScope:  attestation.ConfigScope,
		AttestationRevision: attestation.Revision,
		FallbackRevision:    attestation.FallbackRevision,
	}
	out := CompiledPosture{
		CompiledFrom: context,
		HookPlan: HookPlan{
			SchemaVersion: HookPlanSchemaVersion,
			ID:            "hplan_" + shortCompileDigest(posture.ID, attestation.Revision),
			AuthorityRef:  posture.AuthorityRef,
			Revision:      compileRevision(posture, attestation),
			CompiledFrom:  &context,
		},
	}

	for _, intention := range posture.Intentions {
		compiled, bindings := adapter.Compile(intention, attestation)
		out.Intentions = append(out.Intentions, compiled)
		for _, native := range bindings {
			out.HookPlan.Bindings = append(out.HookPlan.Bindings, Binding{
				ID:                  bindingID(out.HookPlan.ID, intention, native.event),
				Harness:             posture.Harness,
				Event:               native.event,
				Matcher:             native.matcher,
				HandlerRef:          posture.HandlerRef,
				InvocationRevision:  posture.InvocationRevision,
				HandlerRevision:     posture.HandlerRevision,
				Intention:           intention,
				Disposition:         compiled.Disposition,
				EnforcementStrength: compiled.EnforcementStrength,
				Blocking:            compiled.EnforcementStrength == "blocking",
			})
		}
		if intention == IntentionGuardMutation &&
			(compiled.Disposition == DispositionNative || compiled.Disposition == DispositionFallback) &&
			compiled.EnforcementStrength == "blocking" {
			out.MutationReady = true
		}
	}
	out.HookPlan.Intentions = append([]CompiledIntention(nil), out.Intentions...)
	return out, nil
}

func CompileDefaultPosture(request DefaultPostureRequest) (CompiledPosture, error) {
	return compileDefaultPosture(request, harness.ObservedVersion)
}

// Keep the version probe invocation-local: a package-wide test override races
// with concurrent posture compilation and can stamp another caller's version.
func compileDefaultPosture(request DefaultPostureRequest, observeVersion func(string) string) (CompiledPosture, error) {
	harnessName := strings.TrimSpace(request.Harness)
	registryName := strings.ReplaceAll(harnessName, "-", "_")
	if registryName == "claude" {
		registryName = "claude_code"
	}
	registered, ok := request.Registry.Get(registryName)
	if !ok {
		return CompiledPosture{}, fmt.Errorf("compile default posture: harness %q is not registered", harnessName)
	}

	family, surface, defaultScope := semanticHarnessIdentity(registryName)
	if family == "" {
		return CompiledPosture{}, fmt.Errorf("compile default posture: harness %q has no semantic adapter", harnessName)
	}
	features, err := AttestedFeatures(family, registered.Capabilities)
	if err != nil {
		return CompiledPosture{}, err
	}
	configScope := strings.TrimSpace(request.ConfigScope)
	if configScope == "" {
		configScope = defaultScope
	}
	revision := harness.RegistryRevision(request.Registry)
	// Prefer the OBSERVED installed version (prime-agent --version / claude
	// --version / codex --version) when the binary is reachable; fall back
	// to the "registry:<name>" empty socket only when ObservedVersion returns
	// "" (binary missing, --version failed, or unparseable output). The
	// registry name is the only fallback because the harness row's Name is
	// the one thing that is always reachable even when the binary is not —
	// it is the same string the RunAttempt.Harness field carries, so a
	// doctor that keys off Version can correlate with the audit trail.
	version := observeVersion(registryName)
	if version == "" {
		version = "registry:" + registered.Name
	}
	return CompileSemanticPosture(SemanticPosture{
		ID:                 "hpost_default_" + registryName,
		Revision:           revision,
		AuthorityRef:       "compound://harness-posture/default",
		Harness:            harnessName,
		HandlerRef:         request.HandlerRef,
		InvocationRevision: "compound.hook.semantic.v0.1",
		HandlerRevision:    request.HandlerRevision,
		Intentions:         defaultPostureIntentions(registryName),
	}, HarnessAttestation{
		Harness:          harnessName,
		Family:           family,
		Surface:          surface,
		Version:          version,
		FeatureState:     "declared",
		ConfigScope:      configScope,
		Revision:         revision,
		FallbackRevision: "none",
		Features:         features,
	})
}

// defaultPostureIntentions names the lifecycle intentions a harness's default posture
// requests, in the order their bindings are installed.
//
// Session orientation (IntentionPrepareContext) and per-turn injection
// (IntentionPrepareTurnContext) are requested separately because they are separately
// wanted. claude_code asks for orientation only: its per-turn build spent the whole
// inject budget re-deriving what SessionStart had already delivered, and a build that
// overruns the budget fails open, so the injection it paid for was not reliably there.
// Every other lane keeps both -- for them the per-turn hook is the only channel that
// reaches a session already in flight. Adapters that cannot do per-turn injection at all
// (opencode) are never asked for it, so the request stays honest rather than compiling to
// an unsupported intention.
func defaultPostureIntentions(registryName string) []HarnessIntention {
	intentions := []HarnessIntention{IntentionPrepareContext}
	perTurnContext := registryName != "claude_code" &&
		(registryName == "codex" || strings.HasPrefix(registryName, "claude"))
	if perTurnContext {
		intentions = append(intentions, IntentionPrepareTurnContext)
	}
	if registryName == "codex" || registryName == "claude_code" {
		intentions = append(intentions, IntentionObserveAction, IntentionObserveOutcome)
	} else {
		intentions = append(intentions, IntentionGuardMutation, IntentionObserveOutcome)
	}
	// Codex 0.149.x exposes a native SessionEnd event with transcript_path; its receipt
	// capability is now true (rule_delta, see harness.go:458). The Claude-* lanes still own
	// the bounded adapter seam (session-end-adapter); codex routes through the generic
	// session-end verb instead, so both are installed under the same IntentionRecordReceipt
	// but compiled to different runtimeHarnessName-verbed commands. Hermes is intentionally
	// absent: its SessionEnd lives behind a plugin the operator must install, not a native
	// event the binary always fires, so the registry truth (harness.go:475) is unchanged and
	// its harness tests stay green.
	if registryName == "codex" || strings.HasPrefix(registryName, "claude_") {
		intentions = append(intentions, IntentionRecordReceipt)
	}
	return intentions
}

func AttestedFeatures(
	family string,
	capabilities harness.Capabilities,
) (map[string]bool, error) {
	adapter, err := semanticAdapterFor(family)
	if err != nil {
		return nil, err
	}
	return adapter.Attest(capabilities), nil
}

func semanticAdapterFor(family string) (semanticHarnessAdapter, error) {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case "claude", "claude-code", "claude_code":
		return claudeSemanticAdapter{}, nil
	case "codex":
		return codexSemanticAdapter{}, nil
	case "opencode":
		return openCodeSemanticAdapter{}, nil
	default:
		return nil, fmt.Errorf("compile semantic posture: unsupported harness family %q", family)
	}
}

func semanticHarnessIdentity(name string) (family, surface, scope string) {
	switch {
	case strings.HasPrefix(name, "claude"):
		return "claude", "settings.json", "profile"
	case name == "codex":
		return "codex", "hooks.json", "user-or-project"
	case name == "opencode":
		return "opencode", "opencode.jsonc+plugin", "user"
	default:
		return "", "", ""
	}
}

func validateSemanticPosture(posture SemanticPosture) error {
	if strings.TrimSpace(posture.ID) == "" ||
		strings.TrimSpace(posture.Revision) == "" ||
		strings.TrimSpace(posture.AuthorityRef) == "" ||
		strings.TrimSpace(posture.Harness) == "" ||
		strings.TrimSpace(posture.HandlerRef) == "" ||
		strings.TrimSpace(posture.HandlerRevision) == "" {
		return fmt.Errorf("compile semantic posture: posture identity, authority, harness, and handler revisions are required")
	}
	if len(posture.Intentions) == 0 {
		return fmt.Errorf("compile semantic posture: at least one lifecycle intention is required")
	}
	seen := map[HarnessIntention]bool{}
	for _, intention := range posture.Intentions {
		switch intention {
		case IntentionPrepareContext, IntentionPrepareTurnContext, IntentionObserveAction,
			IntentionGuardMutation, IntentionObserveOutcome, IntentionRecordReceipt:
		default:
			return fmt.Errorf("compile semantic posture: unknown lifecycle intention %q", intention)
		}
		if seen[intention] {
			return fmt.Errorf("compile semantic posture: duplicate lifecycle intention %q", intention)
		}
		seen[intention] = true
	}
	return nil
}

func validateHarnessAttestation(posture SemanticPosture, attestation HarnessAttestation) error {
	if strings.TrimSpace(attestation.Harness) != strings.TrimSpace(posture.Harness) {
		return fmt.Errorf(
			"compile semantic posture: attested harness %q does not match posture harness %q",
			attestation.Harness,
			posture.Harness,
		)
	}
	if strings.TrimSpace(attestation.Family) == "" ||
		strings.TrimSpace(attestation.Surface) == "" ||
		strings.TrimSpace(attestation.Version) == "" ||
		strings.TrimSpace(attestation.FeatureState) == "" ||
		strings.TrimSpace(attestation.ConfigScope) == "" ||
		strings.TrimSpace(attestation.Revision) == "" {
		return fmt.Errorf("compile semantic posture: complete harness attestation is required")
	}
	return nil
}

func compileRevision(posture SemanticPosture, attestation HarnessAttestation) string {
	body, _ := json.Marshal(struct {
		Posture     SemanticPosture
		Attestation HarnessAttestation
	}{Posture: posture, Attestation: attestation})
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func shortCompileDigest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

func bindingID(planID string, intention HarnessIntention, event string) string {
	return "hbind_" + shortCompileDigest(planID, string(intention), event)
}
