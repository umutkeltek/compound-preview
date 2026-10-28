package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
)

const (
	AuthorityFaceSchema = "compound.policy_authority_face.v0.1"

	RecoverabilityVerified    = "verified"
	RecoverabilityNotVerified = "not_verified"
	RecoverabilityUnknown     = "unknown"

	OperatorControlContinuous = "continuous"
	OperatorControlImpaired   = "impaired"
	OperatorControlUnknown    = "unknown"

	AuthorityGateStandingScope       = "standing_scope"
	AuthorityGatePolicyAllow         = "policy_allow"
	AuthorityGateMissing             = "authority_missing"
	AuthorityGateRecoveryNotVerified = "recovery_not_verified"
	AuthorityGateOperatorControl     = "operator_control_continuity"
	AuthorityGatePolicyHold          = "policy_hold"
	AuthorityGatePolicyDeny          = "policy_deny"

	AuthorityOutcomeEvidenceRef = "compound://policy/outcome-authority"
	authorityFaceRevisionPrefix = "policy-authority-v1:sha256:"
)

// AuthorityContext carries outcome evidence that is not present on the
// ActionIntent itself. It is supplied by the concrete action owner at the
// policy seam; presentation clients must never reconstruct these facts from a
// target location, mutation-class label, or button availability.
type AuthorityContext struct {
	TargetScope               string   `json:"target_scope"`
	VerifiedRecoverability    string   `json:"verified_recoverability"`
	OperatorControlContinuity string   `json:"operator_control_continuity"`
	ExpectedReceipt           string   `json:"expected_receipt,omitempty"`
	ExpectedProof             []string `json:"expected_proof,omitempty"`
	EvidenceRefs              []string `json:"evidence_refs,omitempty"`
}

// AuthorityFace is the policy-owned read model for ObjectFace/Act surfaces.
// It is a projection of one exact ActionIntent + freshly evaluated
// PolicyDecision, not an additional permission layer. The mutating adapter
// remains the enforcement seam.
type AuthorityFace struct {
	SchemaVersion             string                `json:"schema_version"`
	Revision                  string                `json:"revision"`
	Current                   bool                  `json:"current"`
	Allowed                   bool                  `json:"allowed"`
	Wording                   string                `json:"wording"`
	ActionIntentID            string                `json:"action_intent_id"`
	PolicyDecisionID          string                `json:"policy_decision_id"`
	PolicyOutcome             string                `json:"policy_outcome"`
	StandingGrantID           string                `json:"standing_grant_id,omitempty"`
	Target                    envelope.ActionTarget `json:"target"`
	TargetScope               string                `json:"target_scope"`
	BlastRadius               string                `json:"blast_radius"`
	Reversibility             string                `json:"reversibility"`
	VerifiedRecoverability    string                `json:"verified_recoverability"`
	OperatorControlContinuity string                `json:"operator_control_continuity"`
	PolicySource              string                `json:"policy_source"`
	ExactGate                 string                `json:"exact_gate"`
	ExpectedReceipt           string                `json:"expected_receipt"`
	ExpectedProof             []string              `json:"expected_proof"`
}

// AuthorityEvaluation keeps the enforcement decision and its operator face
// together so an owner cannot accidentally render a different authority than
// the one it evaluated.
type AuthorityEvaluation struct {
	Decision  envelope.PolicyDecision `json:"policy_decision"`
	Authority AuthorityFace           `json:"policy_authority"`
}

// ValidateAuthorityBundle checks that a current AuthorityFace is structurally
// bound to the exact ActionIntent and PolicyDecision it accompanies. It does
// not evaluate policy; action-time adapters must still resolve current policy
// through the existing enforcement seam.
func ValidateAuthorityBundle(ai envelope.ActionIntent, pd envelope.PolicyDecision, face AuthorityFace) error {
	if err := ai.Validate(); err != nil {
		return fmt.Errorf("authority bundle action intent: %w", err)
	}
	if err := pd.Validate(); err != nil {
		return fmt.Errorf("authority bundle policy decision: %w", err)
	}
	if pd.ActionIntentID != ai.ID {
		return fmt.Errorf("authority bundle policy decision action_intent_id mismatch")
	}
	if strings.TrimSpace(pd.IDs.WorkID) == "" || pd.IDs.WorkID != ai.IDs.WorkID {
		return fmt.Errorf("authority bundle work identity mismatch")
	}
	if !reflect.DeepEqual(pd.Scope, ai.Scope) {
		return fmt.Errorf("authority bundle scope mismatch")
	}
	if face.SchemaVersion != AuthorityFaceSchema {
		return fmt.Errorf("authority bundle face schema_version invalid")
	}
	if !face.Current {
		return fmt.Errorf("authority bundle face is not current")
	}
	if !validAuthorityFaceRevision(face.Revision) {
		return fmt.Errorf("authority bundle face revision invalid")
	}
	if face.ActionIntentID != ai.ID {
		return fmt.Errorf("authority bundle face action_intent_id mismatch")
	}
	if face.PolicyDecisionID != pd.ID {
		return fmt.Errorf("authority bundle face policy_decision_id mismatch")
	}
	if face.Target != ai.Target {
		return fmt.Errorf("authority bundle face target mismatch")
	}
	if face.BlastRadius != ai.BlastRadius {
		return fmt.Errorf("authority bundle face blast_radius mismatch")
	}
	if face.Reversibility != ai.Reversibility {
		return fmt.Errorf("authority bundle face reversibility mismatch")
	}
	if face.PolicyOutcome != pd.Outcome {
		return fmt.Errorf("authority bundle face policy_outcome mismatch")
	}
	if face.StandingGrantID != pd.StandingGrantID {
		return fmt.Errorf("authority bundle face standing_grant_id mismatch")
	}
	if face.Allowed != Allowed(pd) {
		return fmt.Errorf("authority bundle face allowed outcome mismatch")
	}
	if !authorityContextValid(ai, AuthorityContext{
		TargetScope:               face.TargetScope,
		VerifiedRecoverability:    face.VerifiedRecoverability,
		OperatorControlContinuity: face.OperatorControlContinuity,
		ExpectedReceipt:           face.ExpectedReceipt,
		ExpectedProof:             face.ExpectedProof,
	}, face.PolicySource) {
		return fmt.Errorf("authority bundle face context invalid")
	}
	if expected := authorityFaceRevision(ai, pd, face); face.Revision != expected {
		return fmt.Errorf("authority bundle face revision does not match semantics")
	}
	return nil
}

func validAuthorityFaceRevision(revision string) bool {
	digest := strings.TrimPrefix(revision, authorityFaceRevisionPrefix)
	if digest == revision || len(digest) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

// EvaluateAuthority evaluates current policy once and projects the resulting
// authority face from that exact decision. A caller should stamp the returned
// face onto its existing action projection and re-evaluate on the action-time
// projection read.
func EvaluateAuthority(ai envelope.ActionIntent, cfg Config, ctx EvaluationContext, policySource string, now time.Time) AuthorityEvaluation {
	effective := effectiveAuthorityContext(ai, cfg, ctx)
	pd := EvaluateWithContext(ai, cfg, effective, now)
	face := ProjectAuthority(ai, pd, cfg, effective, policySource)
	return AuthorityEvaluation{Decision: pd, Authority: face}
}

// ProjectAuthority re-projects the exact policy-owned operator face for a
// stored ActionIntent + PolicyDecision using current policy configuration,
// owner-supplied outcome context, and policy source. Recovery adapters use this
// to fail closed when a stored face no longer matches current authority without
// duplicating revision or wording logic outside policy.
func ProjectAuthority(ai envelope.ActionIntent, pd envelope.PolicyDecision, cfg Config, ctx EvaluationContext, policySource string) AuthorityFace {
	effective := effectiveAuthorityContext(ai, cfg, ctx)
	return authorityFace(ai, pd, cfg, effective.Authority, policySource)
}

func effectiveAuthorityContext(ai envelope.ActionIntent, cfg Config, ctx EvaluationContext) EvaluationContext {
	// Evaluate is the compatibility path for existing adapters that do not
	// render an AuthorityFace. EvaluateAuthority is stricter: once an owner asks
	// for an operator face, absence of verified recovery is itself current
	// outcome evidence for a high-consequence mutation.
	effective := ctx
	if authorityHighConsequence(ai, cfg) && (effective.Authority == nil || strings.TrimSpace(effective.Authority.VerifiedRecoverability) != RecoverabilityVerified) {
		if effective.Authority == nil {
			effective.Authority = &AuthorityContext{
				VerifiedRecoverability:    RecoverabilityUnknown,
				OperatorControlContinuity: OperatorControlUnknown,
			}
		} else {
			copyOfAuthority := *effective.Authority
			effective.Authority = &copyOfAuthority
		}
	}
	return effective
}

func applyAuthorityContext(pd envelope.PolicyDecision, ai envelope.ActionIntent, cfg Config, ctx AuthorityContext, now time.Time) envelope.PolicyDecision {
	pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, AuthorityOutcomeEvidenceRef)
	pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, ctx.EvidenceRefs...)
	if ai.MutationClass == "read" || pd.Outcome == "deny" {
		return pd
	}

	reason := ""
	if strings.TrimSpace(ctx.VerifiedRecoverability) != RecoverabilityVerified && authorityHighConsequence(ai, cfg) {
		reason = "authority gate recovery_not_verified: high-consequence mutation has no verified recovery path"
	} else if strings.TrimSpace(ctx.OperatorControlContinuity) == OperatorControlImpaired {
		reason = "authority gate operator_control_continuity: continuity of operator control is impaired"
	}
	if reason == "" {
		return pd
	}

	pd.Reasons = append(pd.Reasons, reason)
	if severity[pd.Outcome] < severity["hold"] {
		pd.Outcome = "hold"
		pd.Obligations = nil
		pd.ID = idFor("pol_", ai.ID, "hold", strings.Fields(reason)[2], ts(now))
	}
	// Outcome evidence is a non-overridable boundary. Even if the declarative
	// standing scope matched first, it cannot remain attached to a held face.
	pd.StandingGrantID = ""
	pd.ExpiresAt = ""
	return pd
}

func authorityHighConsequence(ai envelope.ActionIntent, cfg Config) bool {
	builtIn := DefaultConfig()
	return contains(builtIn.HumanReversibilities, strings.TrimSpace(ai.Reversibility)) ||
		contains(builtIn.WideBlastRadii, strings.TrimSpace(ai.BlastRadius)) ||
		contains(cfg.HumanReversibilities, strings.TrimSpace(ai.Reversibility)) ||
		contains(cfg.WideBlastRadii, strings.TrimSpace(ai.BlastRadius))
}

func authorityFace(
	ai envelope.ActionIntent,
	pd envelope.PolicyDecision,
	cfg Config,
	ctx *AuthorityContext,
	policySource string,
) AuthorityFace {
	input := AuthorityContext{
		VerifiedRecoverability:    RecoverabilityUnknown,
		OperatorControlContinuity: OperatorControlUnknown,
	}
	if ctx != nil {
		input = *ctx
	}
	contextValid := ctx != nil && authorityContextValid(ai, input, policySource)
	allowed := contextValid && Allowed(pd)
	gate := AuthorityGateMissing
	if contextValid {
		gate = authorityExactGate(ai, pd, cfg, input)
	}
	face := AuthorityFace{
		SchemaVersion:             AuthorityFaceSchema,
		Current:                   contextValid,
		Allowed:                   allowed,
		Wording:                   "Needs context · current policy authority unavailable",
		ActionIntentID:            ai.ID,
		PolicyDecisionID:          pd.ID,
		PolicyOutcome:             pd.Outcome,
		StandingGrantID:           pd.StandingGrantID,
		Target:                    ai.Target,
		TargetScope:               strings.TrimSpace(input.TargetScope),
		BlastRadius:               ai.BlastRadius,
		Reversibility:             ai.Reversibility,
		VerifiedRecoverability:    firstNonEmptyAuthority(input.VerifiedRecoverability, RecoverabilityUnknown),
		OperatorControlContinuity: firstNonEmptyAuthority(input.OperatorControlContinuity, OperatorControlUnknown),
		PolicySource:              strings.TrimSpace(policySource),
		ExactGate:                 gate,
		ExpectedReceipt:           strings.TrimSpace(input.ExpectedReceipt),
		ExpectedProof:             append([]string{}, input.ExpectedProof...),
	}
	if contextValid {
		face.Wording = authorityWording(ai, pd, cfg, input, gate)
	}
	face.Revision = authorityFaceRevision(ai, pd, face)
	return face
}

func authorityContextValid(ai envelope.ActionIntent, ctx AuthorityContext, policySource string) bool {
	if strings.TrimSpace(ctx.TargetScope) == "" || strings.TrimSpace(policySource) == "" {
		return false
	}
	switch strings.TrimSpace(ctx.VerifiedRecoverability) {
	case RecoverabilityVerified, RecoverabilityNotVerified, RecoverabilityUnknown:
	default:
		return false
	}
	switch strings.TrimSpace(ctx.OperatorControlContinuity) {
	case OperatorControlContinuous, OperatorControlImpaired, OperatorControlUnknown:
	default:
		return false
	}
	if ai.MutationClass != "read" {
		if strings.TrimSpace(ctx.ExpectedReceipt) == "" {
			return false
		}
		hasExpectedProof := false
		for _, proof := range ctx.ExpectedProof {
			if strings.TrimSpace(proof) != "" {
				hasExpectedProof = true
				break
			}
		}
		if !hasExpectedProof {
			return false
		}
	}
	return true
}

func authorityExactGate(ai envelope.ActionIntent, pd envelope.PolicyDecision, cfg Config, ctx AuthorityContext) string {
	if pd.Outcome == "deny" {
		return AuthorityGatePolicyDeny
	}
	if ai.MutationClass != "read" && strings.TrimSpace(ctx.VerifiedRecoverability) != RecoverabilityVerified && authorityHighConsequence(ai, cfg) {
		return AuthorityGateRecoveryNotVerified
	}
	if ai.MutationClass != "read" && strings.TrimSpace(ctx.OperatorControlContinuity) == OperatorControlImpaired {
		return AuthorityGateOperatorControl
	}
	if Allowed(pd) {
		if strings.TrimSpace(pd.StandingGrantID) != "" {
			return AuthorityGateStandingScope
		}
		return AuthorityGatePolicyAllow
	}
	return AuthorityGatePolicyHold
}

func authorityWording(ai envelope.ActionIntent, pd envelope.PolicyDecision, cfg Config, ctx AuthorityContext, gate string) string {
	if gate == AuthorityGateRecoveryNotVerified {
		return "Needs your decision · recovery not verified"
	}
	if gate == AuthorityGateOperatorControl {
		return "Needs your decision · operator control continuity not verified"
	}
	if Allowed(pd) && strings.TrimSpace(ctx.TargetScope) == "private" && strings.TrimSpace(pd.StandingGrantID) != "" &&
		strings.TrimSpace(ctx.VerifiedRecoverability) == RecoverabilityVerified &&
		strings.TrimSpace(ctx.OperatorControlContinuity) == OperatorControlContinuous &&
		!authorityHighConsequence(ai, cfg) {
		return "Allowed in your private scope · reversible"
	}
	if Allowed(pd) {
		return "Allowed by current policy"
	}
	if pd.Outcome == "deny" {
		return "Blocked by current policy"
	}
	return "Needs your decision · current policy holds this action"
}

func authorityFaceRevision(ai envelope.ActionIntent, pd envelope.PolicyDecision, face AuthorityFace) string {
	stableDecision := struct {
		Outcome         string   `json:"outcome"`
		Reasons         []string `json:"reasons"`
		Obligations     []string `json:"obligations"`
		StandingGrantID string   `json:"standing_grant_id"`
		ExpiresAt       string   `json:"expires_at"`
		EvidenceRefs    []string `json:"evidence_refs"`
	}{pd.Outcome, pd.Reasons, pd.Obligations, pd.StandingGrantID, pd.ExpiresAt, pd.EvidenceRefs}
	stableFace := struct {
		SchemaVersion             string                `json:"schema_version"`
		Current                   bool                  `json:"current"`
		Allowed                   bool                  `json:"allowed"`
		Wording                   string                `json:"wording"`
		ActionIntentID            string                `json:"action_intent_id"`
		PolicyOutcome             string                `json:"policy_outcome"`
		StandingGrantID           string                `json:"standing_grant_id"`
		Target                    envelope.ActionTarget `json:"target"`
		TargetScope               string                `json:"target_scope"`
		BlastRadius               string                `json:"blast_radius"`
		Reversibility             string                `json:"reversibility"`
		VerifiedRecoverability    string                `json:"verified_recoverability"`
		OperatorControlContinuity string                `json:"operator_control_continuity"`
		PolicySource              string                `json:"policy_source"`
		ExactGate                 string                `json:"exact_gate"`
		ExpectedReceipt           string                `json:"expected_receipt"`
		ExpectedProof             []string              `json:"expected_proof"`
	}{
		SchemaVersion:             face.SchemaVersion,
		Current:                   face.Current,
		Allowed:                   face.Allowed,
		Wording:                   face.Wording,
		ActionIntentID:            face.ActionIntentID,
		PolicyOutcome:             face.PolicyOutcome,
		StandingGrantID:           face.StandingGrantID,
		Target:                    face.Target,
		TargetScope:               face.TargetScope,
		BlastRadius:               face.BlastRadius,
		Reversibility:             face.Reversibility,
		VerifiedRecoverability:    face.VerifiedRecoverability,
		OperatorControlContinuity: face.OperatorControlContinuity,
		PolicySource:              face.PolicySource,
		ExactGate:                 face.ExactGate,
		ExpectedReceipt:           face.ExpectedReceipt,
		ExpectedProof:             face.ExpectedProof,
	}
	payload := struct {
		Action   envelope.ActionIntent `json:"action"`
		Decision any                   `json:"decision"`
		Face     any                   `json:"face"`
	}{ai, stableDecision, stableFace}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return authorityFaceRevisionPrefix + hex.EncodeToString(sum[:])
}

func firstNonEmptyAuthority(value, fallback string) string {
	if clean := strings.TrimSpace(value); clean != "" {
		return clean
	}
	return fallback
}
