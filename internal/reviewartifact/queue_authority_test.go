package reviewartifact

import (
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

func TestQueueGroupActionBindsStableAuthorityIdentity(t *testing.T) {
	first := validReviewAuthorityAction(t, time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC), "compound://policy/effective")
	second := validReviewAuthorityAction(t, time.Date(2026, 7, 17, 9, 1, 0, 0, time.UTC), "compound://policy/effective")
	if first.PolicyDecision.ID == second.PolicyDecision.ID {
		t.Fatalf("fixture needs fresh policy decision ids: %q", first.PolicyDecision.ID)
	}
	if first.PolicyAuthority.Revision != second.PolicyAuthority.Revision {
		t.Fatalf("fixture needs one stable authority revision: first=%q second=%q", first.PolicyAuthority.Revision, second.PolicyAuthority.Revision)
	}

	if got, reason := queueGroupAction(reviewAuthorityGroup(), reviewAuthorityItems(first, second)); got == nil || reason != "" {
		t.Fatalf("equivalent current authority should batch despite fresh decision ids: action=%+v reason=%q", got, reason)
	}

	changed := validReviewAuthorityAction(t, time.Date(2026, 7, 17, 9, 1, 0, 0, time.UTC), "compound://policy/revised")
	if changed.PolicyAuthority.Revision == first.PolicyAuthority.Revision {
		t.Fatal("fixture did not change the owner authority revision")
	}
	if got, reason := queueGroupAction(reviewAuthorityGroup(), reviewAuthorityItems(first, changed)); got != nil || reason == "" {
		t.Fatalf("different authority revisions must not batch: action=%+v reason=%q", got, reason)
	}
}

func TestQueueGroupActionFailsClosedForPartialOrMalformedAuthority(t *testing.T) {
	valid := validReviewAuthorityAction(t, time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC), "compound://policy/effective")

	tests := []struct {
		name   string
		mutate func(*Action)
	}{
		{name: "action intent only", mutate: func(action *Action) {
			action.PolicyDecision = nil
			action.PolicyAuthority = nil
		}},
		{name: "decision missing", mutate: func(action *Action) {
			action.PolicyDecision = nil
		}},
		{name: "authority face missing", mutate: func(action *Action) {
			action.PolicyAuthority = nil
		}},
		{name: "malformed authority face", mutate: func(action *Action) {
			action.PolicyAuthority.Current = false
		}},
		{name: "mismatched authority face", mutate: func(action *Action) {
			action.PolicyAuthority.ActionIntentID = "act_other"
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := cloneReviewAuthorityAction(valid)
			second := cloneReviewAuthorityAction(valid)
			tc.mutate(&first)
			tc.mutate(&second)
			if got, reason := queueGroupAction(reviewAuthorityGroup(), reviewAuthorityItems(first, second)); got != nil || reason == "" {
				t.Fatalf("invalid authority must not become a batch identity: action=%+v reason=%q", got, reason)
			}
		})
	}
}

func TestQueueGroupActionKeepsLegacyActionsWithoutAuthorityCompatible(t *testing.T) {
	legacy := Action{ID: "owner_action", Label: "Apply", Command: "compound owner apply", Argv: []string{"compound", "owner", "apply"}}
	if got, reason := queueGroupAction(reviewAuthorityGroup(), reviewAuthorityItems(legacy, legacy)); got == nil || reason != "" {
		t.Fatalf("legacy actions without authority should retain batching behavior: action=%+v reason=%q", got, reason)
	}
}

func TestQueueGroupActionDeepCopiesValidatedAuthority(t *testing.T) {
	valid := validReviewAuthorityAction(t, time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC), "compound://policy/effective")
	items := reviewAuthorityItems(valid)
	got, reason := queueGroupAction(reviewAuthorityGroup(), items)
	if got == nil || reason != "" {
		t.Fatalf("valid authority should produce a batch action: action=%+v reason=%q", got, reason)
	}
	source := &items[0].Actions[0]
	if got.ActionIntent == source.ActionIntent || got.PolicyDecision == source.PolicyDecision || got.PolicyAuthority == source.PolicyAuthority || got.AuthorityContext == source.AuthorityContext {
		t.Fatal("safe batch action retained an authority object pointer")
	}
	if len(got.Argv) == 0 || len(got.ActionIntent.EvidenceRefs) == 0 || len(got.PolicyDecision.EvidenceRefs) == 0 || len(got.PolicyDecision.Reasons) == 0 || len(got.PolicyDecision.Obligations) == 0 || len(got.PolicyAuthority.ExpectedProof) == 0 || len(got.AuthorityContext.ExpectedProof) == 0 || len(got.AuthorityContext.EvidenceRefs) == 0 {
		t.Fatalf("fixture does not exercise deep-copy slices: %+v", got)
	}

	wantArgv := got.Argv[0]
	wantActionID := got.ActionIntent.ID
	wantActionWorkID := got.ActionIntent.IDs.WorkID
	wantActionScope := got.ActionIntent.Scope.EntityID
	wantActionEvidence := got.ActionIntent.EvidenceRefs[0]
	wantEstimatedUSD := *got.ActionIntent.EstimatedCost.USD
	wantDecisionWorkID := got.PolicyDecision.IDs.WorkID
	wantDecisionScope := got.PolicyDecision.Scope.EntityID
	wantDecisionEvidence := got.PolicyDecision.EvidenceRefs[0]
	wantReason := got.PolicyDecision.Reasons[0]
	wantObligation := got.PolicyDecision.Obligations[0]
	wantProof := got.PolicyAuthority.ExpectedProof[0]
	wantContextProof := got.AuthorityContext.ExpectedProof[0]
	wantContextEvidence := got.AuthorityContext.EvidenceRefs[0]

	source.Argv[0] = "changed"
	source.ActionIntent.ID = "act_changed"
	source.ActionIntent.IDs.WorkID = "work_changed"
	source.ActionIntent.Scope.EntityID = "entity_changed"
	source.ActionIntent.EvidenceRefs[0] = "evidence:changed"
	*source.ActionIntent.EstimatedCost.USD = 9
	source.PolicyDecision.IDs.WorkID = "work_changed"
	source.PolicyDecision.Scope.EntityID = "entity_changed"
	source.PolicyDecision.EvidenceRefs[0] = "evidence:changed"
	source.PolicyDecision.Reasons[0] = "changed"
	source.PolicyDecision.Obligations[0] = "changed"
	source.PolicyAuthority.ExpectedProof[0] = "proof:changed"
	source.AuthorityContext.ExpectedProof[0] = "proof:changed"
	source.AuthorityContext.EvidenceRefs[0] = "evidence:changed"

	if got.Argv[0] != wantArgv || got.ActionIntent.ID != wantActionID || got.ActionIntent.IDs.WorkID != wantActionWorkID || got.ActionIntent.Scope.EntityID != wantActionScope || got.ActionIntent.EvidenceRefs[0] != wantActionEvidence || *got.ActionIntent.EstimatedCost.USD != wantEstimatedUSD {
		t.Fatalf("safe batch action aliases source action intent: %+v", got.ActionIntent)
	}
	if got.PolicyDecision.IDs.WorkID != wantDecisionWorkID || got.PolicyDecision.Scope.EntityID != wantDecisionScope || got.PolicyDecision.EvidenceRefs[0] != wantDecisionEvidence || got.PolicyDecision.Reasons[0] != wantReason || got.PolicyDecision.Obligations[0] != wantObligation {
		t.Fatalf("safe batch action aliases source policy decision: %+v", got.PolicyDecision)
	}
	if got.PolicyAuthority.ExpectedProof[0] != wantProof {
		t.Fatalf("safe batch action aliases source policy authority: %+v", got.PolicyAuthority)
	}
	if got.AuthorityContext.ExpectedProof[0] != wantContextProof || got.AuthorityContext.EvidenceRefs[0] != wantContextEvidence {
		t.Fatalf("safe batch action aliases producer authority context: %+v", got.AuthorityContext)
	}
}

func validReviewAuthorityAction(t *testing.T, now time.Time, policySource string) Action {
	t.Helper()
	estimatedUSD := 0.25
	estimatedTokens := 10
	estimatedReviewMinutes := 1
	maxUSD := 1.0
	maxTokens := 100
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_review_authority",
			CreatedAt:     "2026-07-17T09:00:00Z",
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: "work_review_authority"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{"evidence:review-authority-action"},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "compound",
		Capability:    "artifact.update",
		Action:        "update",
		Target:        envelope.ActionTarget{Kind: "file", Path: "artifacts/item.json"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "repo",
		EstimatedCost: envelope.EstimatedCost{
			USD:            &estimatedUSD,
			Tokens:         &estimatedTokens,
			HumanReviewMin: &estimatedReviewMinutes,
		},
	}
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = []policy.StandingGrant{{
		ID:                   "grant_review_authority",
		Capability:           ai.Capability,
		MutationClass:        ai.MutationClass,
		ReversibilityCeiling: "reversible",
		BlastRadius:          ai.BlastRadius,
		TargetPath:           "artifacts/**",
		MaxUSD:               &maxUSD,
		MaxTokens:            &maxTokens,
	}}
	authorityContext := &policy.AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    policy.RecoverabilityVerified,
		OperatorControlContinuity: policy.OperatorControlContinuous,
		ExpectedReceipt:           "receipt for the governed action",
		ExpectedProof:             []string{"proof:recovery-path"},
		EvidenceRefs:              []string{"evidence:owner-outcome"},
	}
	got := policy.EvaluateAuthority(ai, cfg, policy.EvaluationContext{Authority: authorityContext}, policySource, now)
	return Action{
		ID:               "owner_action",
		Label:            "Apply",
		Command:          "compound owner apply",
		Argv:             []string{"compound", "owner", "apply"},
		ActionIntent:     &ai,
		PolicyDecision:   &got.Decision,
		PolicyAuthority:  &got.Authority,
		AuthorityContext: authorityContext,
	}
}

func reviewAuthorityGroup() QueueGroup {
	return QueueGroup{Area: "owner-review", Key: "owner-review|authority"}
}

func reviewAuthorityItems(actions ...Action) []QueueItem {
	items := make([]QueueItem, 0, len(actions))
	for i, action := range actions {
		items = append(items, QueueItem{
			Area:     "owner-review",
			ID:       "review_" + string(rune('a'+i)),
			Status:   "review_required",
			GroupKey: "owner-review|authority",
			Actions:  []Action{action},
		})
	}
	return items
}

func cloneReviewAuthorityAction(in Action) Action {
	out := in
	if in.ActionIntent != nil {
		copyOfIntent := *in.ActionIntent
		out.ActionIntent = &copyOfIntent
	}
	if in.PolicyDecision != nil {
		copyOfDecision := *in.PolicyDecision
		out.PolicyDecision = &copyOfDecision
	}
	if in.PolicyAuthority != nil {
		copyOfFace := *in.PolicyAuthority
		out.PolicyAuthority = &copyOfFace
	}
	return out
}
