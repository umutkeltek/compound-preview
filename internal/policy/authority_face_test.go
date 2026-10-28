package policy

import (
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestEvaluateAuthorityFaceAllowsExactPrivateReversibleScope(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:                   "grant_private_reversible",
		Capability:           "artifact.update",
		MutationClass:        "local_reversible",
		ReversibilityCeiling: "reversible",
		BlastRadius:          "repo",
		TargetPath:           "artifacts/**",
	}}
	ai := mkAction("local_reversible", "easy", "artifacts/item.json", "repo")
	ai.Capability = "artifact.update"
	ctx := EvaluationContext{Authority: &AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    RecoverabilityVerified,
		OperatorControlContinuity: OperatorControlContinuous,
		ExpectedReceipt:           "receipt linking the governed action to its work envelope",
		ExpectedProof:             []string{"proof:recovery-path", "proof:control-continuity"},
		EvidenceRefs:              []string{"evidence:authority-input"},
	}}

	first := EvaluateAuthority(ai, cfg, ctx, "compound://policy/effective", fixedNow)
	second := EvaluateAuthority(ai, cfg, ctx, "compound://policy/effective", fixedNow.Add(time.Minute))

	if !Allowed(first.Decision) || first.Decision.StandingGrantID != "grant_private_reversible" {
		t.Fatalf("exact standing scope should allow, decision=%+v", first.Decision)
	}
	face := first.Authority
	if !face.Current || !face.Allowed {
		t.Fatalf("authority should be current and allowed: %+v", face)
	}
	if face.Wording != "Allowed in your private scope · reversible" {
		t.Fatalf("wording = %q", face.Wording)
	}
	if face.ExactGate != AuthorityGateStandingScope || face.TargetScope != "private" || face.BlastRadius != "repo" || face.Reversibility != "easy" {
		t.Fatalf("authority axes = %+v", face)
	}
	if face.VerifiedRecoverability != RecoverabilityVerified || face.OperatorControlContinuity != OperatorControlContinuous {
		t.Fatalf("outcome evidence axes = %+v", face)
	}
	if face.PolicySource != "compound://policy/effective" || face.StandingGrantID != "grant_private_reversible" {
		t.Fatalf("authority sources = %+v", face)
	}
	if face.ExpectedReceipt != ctx.Authority.ExpectedReceipt || len(face.ExpectedProof) != 2 {
		t.Fatalf("expected proof contract = %+v", face)
	}
	if face.Revision == "" || face.Revision != second.Authority.Revision {
		t.Fatalf("equivalent fresh evaluations need a stable revision: first=%q second=%q", face.Revision, second.Authority.Revision)
	}
	if first.Decision.ID == second.Decision.ID {
		t.Fatalf("test did not prove revision stability across fresh PolicyDecision ids: %q", first.Decision.ID)
	}
	if !testContains(first.Decision.EvidenceRefs, AuthorityOutcomeEvidenceRef) || !testContains(first.Decision.EvidenceRefs, "evidence:authority-input") {
		t.Fatalf("decision evidence refs = %v", first.Decision.EvidenceRefs)
	}
}

func TestEvaluateAuthorityFaceHoldsWhenRecoveryIsNotVerified(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:            "grant_private_high_consequence",
		Capability:    "artifact.replace",
		MutationClass: "local_reversible",
		Reversibility: "hard",
		BlastRadius:   "repo",
		TargetPath:    "artifacts/**",
	}}
	ai := mkAction("local_reversible", "hard", "artifacts/item.json", "repo")
	ai.Capability = "artifact.replace"
	ctx := EvaluationContext{Authority: &AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    RecoverabilityNotVerified,
		OperatorControlContinuity: OperatorControlContinuous,
		ExpectedReceipt:           "receipt linking the governed action to its work envelope",
		ExpectedProof:             []string{"proof:recovery-path"},
	}}

	got := EvaluateAuthority(ai, cfg, ctx, "compound://policy/effective", fixedNow)

	if got.Decision.Outcome != "hold" || got.Decision.StandingGrantID != "" {
		t.Fatalf("unverified recovery must override a matching grant: %+v", got.Decision)
	}
	if got.Authority.Allowed || !got.Authority.Current {
		t.Fatalf("held authority face = %+v", got.Authority)
	}
	if got.Authority.ExactGate != AuthorityGateRecoveryNotVerified {
		t.Fatalf("exact gate = %q, reasons=%v", got.Authority.ExactGate, got.Decision.Reasons)
	}
	if got.Authority.Wording != "Needs your decision · recovery not verified" {
		t.Fatalf("wording = %q", got.Authority.Wording)
	}
	if got.Authority.TargetScope != "private" || got.Authority.VerifiedRecoverability != RecoverabilityNotVerified {
		t.Fatalf("private location must not erase the recovery boundary: %+v", got.Authority)
	}
}

func TestEvaluateAuthorityFaceHoldsWhenOperatorControlContinuityIsImpaired(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:                   "grant_private_reversible",
		Capability:           "artifact.update",
		MutationClass:        "local_reversible",
		ReversibilityCeiling: "reversible",
		BlastRadius:          "repo",
		TargetPath:           "artifacts/**",
	}}
	ai := mkAction("local_reversible", "easy", "artifacts/item.json", "repo")
	ai.Capability = "artifact.update"
	ctx := EvaluationContext{Authority: &AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    RecoverabilityVerified,
		OperatorControlContinuity: OperatorControlImpaired,
		ExpectedReceipt:           "receipt linking the governed action to its work envelope",
		ExpectedProof:             []string{"proof:control-continuity"},
	}}

	got := EvaluateAuthority(ai, cfg, ctx, "compound://policy/effective", fixedNow)

	if got.Decision.Outcome != "hold" || got.Decision.StandingGrantID != "" {
		t.Fatalf("impaired operator control continuity must override a matching grant: %+v", got.Decision)
	}
	if got.Authority.ExactGate != AuthorityGateOperatorControl || got.Authority.Allowed {
		t.Fatalf("authority gate = %+v", got.Authority)
	}
}

func TestAuthorityFaceJSONCarriesConcretePolicyAndTargetIdentity(t *testing.T) {
	ai := mkAction("read", "easy", "artifacts/item.json", "repo")
	got := EvaluateAuthority(ai, DefaultConfig(), EvaluationContext{Authority: &AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    RecoverabilityVerified,
		OperatorControlContinuity: OperatorControlContinuous,
	}}, "built-in", fixedNow)

	if got.Authority.ActionIntentID != ai.ID || got.Authority.PolicyDecisionID != got.Decision.ID {
		t.Fatalf("authority is not bound to the exact action/decision: %+v", got.Authority)
	}
	if got.Authority.Target.Kind != ai.Target.Kind || got.Authority.Target.Path != ai.Target.Path {
		t.Fatalf("authority target = %+v want %+v", got.Authority.Target, ai.Target)
	}
}

func TestEvaluateAuthorityFaceFailsClosedWithoutOwnerOutcomeEvidence(t *testing.T) {
	ai := mkAction("local_reversible", "easy", "artifacts/item.json", "repo")
	got := EvaluateAuthority(ai, DefaultConfig(), EvaluationContext{}, "built-in", fixedNow)

	if !Allowed(got.Decision) {
		t.Fatalf("baseline enforcement decision unexpectedly changed: %+v", got.Decision)
	}
	if got.Authority.Current || got.Authority.Allowed || got.Authority.ExactGate != AuthorityGateMissing {
		t.Fatalf("an owner did not supply an exact authority context, so its face must fail closed: %+v", got.Authority)
	}
}

func TestEvaluateAuthorityFaceHighConsequenceRequiresVerifiedRecovery(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:            "grant_private_high_consequence",
		Capability:    "artifact.replace",
		MutationClass: "local_reversible",
		Reversibility: "hard",
		BlastRadius:   "repo",
		TargetPath:    "artifacts/**",
	}}
	ai := mkAction("local_reversible", "hard", "artifacts/item.json", "repo")
	ai.Capability = "artifact.replace"

	cases := []struct {
		name      string
		ctx       EvaluationContext
		current   bool
		exactGate string
		wording   string
	}{
		{
			name: "explicit unknown",
			ctx: EvaluationContext{Authority: &AuthorityContext{
				TargetScope:               "private",
				VerifiedRecoverability:    RecoverabilityUnknown,
				OperatorControlContinuity: OperatorControlContinuous,
				ExpectedReceipt:           "receipt linking the governed action to its work envelope",
				ExpectedProof:             []string{"proof:recovery-path"},
			}},
			current:   true,
			exactGate: AuthorityGateRecoveryNotVerified,
			wording:   "Needs your decision · recovery not verified",
		},
		{
			name: "empty value",
			ctx: EvaluationContext{Authority: &AuthorityContext{
				TargetScope:               "private",
				OperatorControlContinuity: OperatorControlContinuous,
				ExpectedReceipt:           "receipt linking the governed action to its work envelope",
				ExpectedProof:             []string{"proof:recovery-path"},
			}},
			exactGate: AuthorityGateMissing,
			wording:   "Needs context · current policy authority unavailable",
		},
		{
			name:      "absent authority context",
			ctx:       EvaluationContext{},
			exactGate: AuthorityGateMissing,
			wording:   "Needs context · current policy authority unavailable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateAuthority(ai, cfg, tc.ctx, "compound://policy/effective", fixedNow)
			if got.Decision.Outcome != "hold" || got.Decision.StandingGrantID != "" {
				t.Fatalf("missing verified recovery must override matching private scope: %+v", got.Decision)
			}
			if got.Authority.Allowed || got.Authority.Current != tc.current || got.Authority.ExactGate != tc.exactGate {
				t.Fatalf("authority face = %+v", got.Authority)
			}
			if got.Authority.Wording != tc.wording {
				t.Fatalf("wording = %q", got.Authority.Wording)
			}
		})
	}
}

func TestLegacyEvaluateRemainsCompatibleWhileEvaluateAuthorityRequiresOutcomeEvidence(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:            "grant_existing_adapter",
		Capability:    "artifact.replace",
		MutationClass: "local_reversible",
		Reversibility: "hard",
		BlastRadius:   "repo",
		TargetPath:    "artifacts/**",
	}}
	ai := mkAction("local_reversible", "hard", "artifacts/item.json", "repo")
	ai.Capability = "artifact.replace"

	legacy := Evaluate(ai, cfg, fixedNow)
	if !Allowed(legacy) || legacy.StandingGrantID != "grant_existing_adapter" {
		t.Fatalf("legacy adapters without an authority face must keep their existing decision contract: %+v", legacy)
	}
	current := EvaluateAuthority(ai, cfg, EvaluationContext{}, "compound://policy/effective", fixedNow)
	if current.Decision.Outcome != "hold" || current.Authority.Current || current.Authority.Allowed || current.Authority.ExactGate != AuthorityGateMissing {
		t.Fatalf("the authority-face path must not turn missing recovery evidence into permission: %+v", current)
	}
}

func TestAuthorityFaceRejectsIncompleteOrMalformedContext(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:                   "grant_private_reversible",
		Capability:           "artifact.update",
		MutationClass:        "local_reversible",
		ReversibilityCeiling: "reversible",
		BlastRadius:          "repo",
		TargetPath:           "artifacts/**",
	}}
	ai := mkAction("local_reversible", "easy", "artifacts/item.json", "repo")
	ai.Capability = "artifact.update"
	valid := AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    RecoverabilityVerified,
		OperatorControlContinuity: OperatorControlContinuous,
		ExpectedReceipt:           "receipt linking the governed action to its work envelope",
		ExpectedProof:             []string{"proof:recovery-path"},
	}

	cases := []struct {
		name         string
		context      *AuthorityContext
		policySource string
	}{
		{name: "non-nil empty context", context: &AuthorityContext{}, policySource: "compound://policy/effective"},
		{name: "malformed recoverability", context: mutateAuthorityContext(valid, func(ctx *AuthorityContext) { ctx.VerifiedRecoverability = "claimed" }), policySource: "compound://policy/effective"},
		{name: "malformed control continuity", context: mutateAuthorityContext(valid, func(ctx *AuthorityContext) { ctx.OperatorControlContinuity = "likely" }), policySource: "compound://policy/effective"},
		{name: "missing target scope", context: mutateAuthorityContext(valid, func(ctx *AuthorityContext) { ctx.TargetScope = "" }), policySource: "compound://policy/effective"},
		{name: "missing policy source", context: &valid, policySource: ""},
		{name: "missing expected receipt", context: mutateAuthorityContext(valid, func(ctx *AuthorityContext) { ctx.ExpectedReceipt = "" }), policySource: "compound://policy/effective"},
		{name: "missing expected proof", context: mutateAuthorityContext(valid, func(ctx *AuthorityContext) { ctx.ExpectedProof = nil }), policySource: "compound://policy/effective"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateAuthority(ai, cfg, EvaluationContext{Authority: tc.context}, tc.policySource, fixedNow)
			if !Allowed(got.Decision) {
				t.Fatalf("face validation must not rewrite the compatible enforcement decision: %+v", got.Decision)
			}
			if got.Authority.Current || got.Authority.Allowed || got.Authority.ExactGate != AuthorityGateMissing {
				t.Fatalf("invalid authority context must fail closed at representation: %+v", got.Authority)
			}
			if got.Authority.Wording != "Needs context · current policy authority unavailable" {
				t.Fatalf("wording = %q", got.Authority.Wording)
			}
		})
	}
}

func TestAuthorityWordingUsesConfiguredHighConsequencePredicate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HumanReversibilities = append(cfg.HumanReversibilities, "manual")
	cfg.StandingGrants = []StandingGrant{{
		ID:            "grant_private_manual",
		Capability:    "artifact.update",
		MutationClass: "local_reversible",
		Reversibility: "manual",
		BlastRadius:   "repo",
		TargetPath:    "artifacts/**",
	}}
	ai := mkAction("local_reversible", "manual", "artifacts/item.json", "repo")
	ai.Capability = "artifact.update"
	ctx := EvaluationContext{Authority: &AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    RecoverabilityVerified,
		OperatorControlContinuity: OperatorControlContinuous,
		ExpectedReceipt:           "receipt linking the governed action to its work envelope",
		ExpectedProof:             []string{"proof:recovery-path"},
	}}

	got := EvaluateAuthority(ai, cfg, ctx, "compound://policy/effective", fixedNow)
	if !Allowed(got.Decision) || !got.Authority.Current || !got.Authority.Allowed {
		t.Fatalf("verified, explicitly granted authority should remain allowed: %+v", got)
	}
	if got.Authority.Wording != "Allowed by current policy" {
		t.Fatalf("configured high-consequence reversibility must not be presented as routine reversible: %q", got.Authority.Wording)
	}
}

func mutateAuthorityContext(base AuthorityContext, mutate func(*AuthorityContext)) *AuthorityContext {
	copyOfContext := base
	copyOfContext.ExpectedProof = append([]string{}, base.ExpectedProof...)
	mutate(&copyOfContext)
	return &copyOfContext
}

func TestValidateAuthorityBundleAcceptsOnlyExactCurrentIdentity(t *testing.T) {
	ai, decision, face := validAuthorityBundleFixture(t)
	if err := ValidateAuthorityBundle(ai, decision, face); err != nil {
		t.Fatalf("exact authority bundle rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*envelope.ActionIntent, *envelope.PolicyDecision, *AuthorityFace)
	}{
		{name: "malformed action intent", mutate: func(ai *envelope.ActionIntent, _ *envelope.PolicyDecision, _ *AuthorityFace) {
			ai.SchemaVersion = "compound.action_intent.invalid"
		}},
		{name: "malformed policy decision", mutate: func(_ *envelope.ActionIntent, pd *envelope.PolicyDecision, _ *AuthorityFace) {
			pd.Outcome = "maybe"
		}},
		{name: "policy decision action mismatch", mutate: func(_ *envelope.ActionIntent, pd *envelope.PolicyDecision, _ *AuthorityFace) {
			pd.ActionIntentID = "act_other"
		}},
		{name: "missing policy work identity", mutate: func(_ *envelope.ActionIntent, pd *envelope.PolicyDecision, _ *AuthorityFace) {
			pd.IDs.WorkID = ""
		}},
		{name: "policy work identity mismatch", mutate: func(_ *envelope.ActionIntent, pd *envelope.PolicyDecision, _ *AuthorityFace) {
			pd.IDs.WorkID = "work_other"
		}},
		{name: "policy scope mismatch", mutate: func(_ *envelope.ActionIntent, pd *envelope.PolicyDecision, _ *AuthorityFace) {
			pd.Scope = envelope.Scope{EntityID: "entity_other"}
		}},
		{name: "wrong face schema", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.SchemaVersion = "compound.policy_authority_face.invalid"
		}},
		{name: "noncurrent face", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.Current = false
		}},
		{name: "missing face revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.Revision = ""
		}},
		{name: "malformed face revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.Revision = "policy-authority-v1:sha256:not-a-digest"
		}},
		{name: "face action mismatch", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.ActionIntentID = "act_other"
		}},
		{name: "face decision mismatch", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.PolicyDecisionID = "pol_other"
		}},
		{name: "face target mismatch", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.Target.Path = "artifacts/other.json"
		}},
		{name: "face blast radius mismatch", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.BlastRadius = "local"
		}},
		{name: "face reversibility mismatch", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.Reversibility = "manual"
		}},
		{name: "face policy outcome mismatch", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.PolicyOutcome = "hold"
		}},
		{name: "face standing grant mismatch", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.StandingGrantID = "grant_other"
		}},
		{name: "face exact gate changed under old revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.ExactGate = AuthorityGatePolicyAllow
		}},
		{name: "face wording changed under old revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.Wording = "Authority wording changed"
		}},
		{name: "face policy source changed under old revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.PolicySource = "compound://policy/revised"
		}},
		{name: "face target scope changed under old revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.TargetScope = "shared"
		}},
		{name: "face recoverability changed under old revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.VerifiedRecoverability = RecoverabilityUnknown
		}},
		{name: "face control continuity changed under old revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.OperatorControlContinuity = OperatorControlImpaired
		}},
		{name: "face expected receipt changed under old revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.ExpectedReceipt = "a different governed receipt"
		}},
		{name: "face expected proof changed under old revision", mutate: func(_ *envelope.ActionIntent, _ *envelope.PolicyDecision, face *AuthorityFace) {
			face.ExpectedProof = append(face.ExpectedProof, "proof:changed")
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotAI := ai
			gotDecision := decision
			gotFace := face
			tc.mutate(&gotAI, &gotDecision, &gotFace)
			if err := ValidateAuthorityBundle(gotAI, gotDecision, gotFace); err == nil {
				t.Fatal("mismatched authority bundle validated")
			}
		})
	}
}

func validAuthorityBundleFixture(t *testing.T) (envelope.ActionIntent, envelope.PolicyDecision, AuthorityFace) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:                   "grant_bundle_fixture",
		Capability:           "artifact.update",
		MutationClass:        "local_reversible",
		ReversibilityCeiling: "reversible",
		BlastRadius:          "repo",
		TargetPath:           "artifacts/**",
	}}
	ai := mkAction("local_reversible", "easy", "artifacts/item.json", "repo")
	ai.Capability = "artifact.update"
	got := EvaluateAuthority(ai, cfg, EvaluationContext{Authority: &AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    RecoverabilityVerified,
		OperatorControlContinuity: OperatorControlContinuous,
		ExpectedReceipt:           "receipt for the governed action",
		ExpectedProof:             []string{"proof:recovery-path"},
	}}, "compound://policy/effective", fixedNow)
	if err := ai.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := got.Decision.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Authority.Revision == "" {
		t.Fatal("fixture authority revision missing")
	}
	return ai, got.Decision, got.Authority
}
