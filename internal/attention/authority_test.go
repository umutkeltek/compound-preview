package attention

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
)

func TestReviewAuthorityAllowedBundleIsForwardedAndRunnable(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	source := attentionAuthorityAction(t, now, "compound://policy/effective", true)
	report := Build(Inputs{Reviews: singletonAuthorityReport(now, source)}, now)

	action := groupByKey(t, report, "authority-singleton").Action
	if action == nil {
		t.Fatal("allowed authority action missing")
	}
	if action.Kind != ActionOneClick || action.RequiresConfirmation {
		t.Fatalf("allowed current authority action = %+v, want one_click without confirmation", action)
	}
	if action.ActionIntent == nil || action.PolicyDecision == nil || action.PolicyAuthority == nil {
		t.Fatalf("authority bundle was not forwarded: %+v", action)
	}
	if action.ActionIntent.ID != source.ActionIntent.ID || action.PolicyDecision.ID != source.PolicyDecision.ID || action.PolicyAuthority.Revision != source.PolicyAuthority.Revision {
		t.Fatalf("forwarded authority identity changed: got=%+v source=%+v", action, source)
	}
	if err := policy.ValidateAuthorityBundle(*action.ActionIntent, *action.PolicyDecision, *action.PolicyAuthority); err != nil {
		t.Fatalf("forwarded authority bundle is not exact: %v", err)
	}
}

func TestReviewAuthorityAllowedWideStandingScopeIsNotRegatedByAttention(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	source := attentionAuthorityActionWithRisk(t, now, "compound://policy/effective", true, "easy", "public")
	report := Build(Inputs{Reviews: singletonAuthorityReport(now, source)}, now)

	group := groupByKey(t, report, "authority-singleton")
	if group.Action == nil || group.Action.Kind != ActionOneClick || group.Action.RequiresConfirmation {
		t.Fatalf("standing-scope authority was re-gated by Attention: %+v", group.Action)
	}
	if group.Action.PolicyAuthority.ExactGate != policy.AuthorityGateStandingScope {
		t.Fatalf("fixture gate = %q, want %q", group.Action.PolicyAuthority.ExactGate, policy.AuthorityGateStandingScope)
	}
	if group.RiskGrade == RiskGradeGate || Class(group) == RiskGradeGate {
		t.Fatalf("allowed current authority was reclassified as a gate: risk_grade=%q class=%q group=%+v", group.RiskGrade, Class(group), group)
	}
}

func TestReviewAuthorityCurrentHoldRemainsVisibleButCannotRun(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	source := attentionAuthorityAction(t, now, "compound://policy/effective", false)
	report := Build(Inputs{Reviews: singletonAuthorityReport(now, source)}, now)

	action := groupByKey(t, report, "authority-singleton").Action
	if action == nil || action.PolicyAuthority == nil {
		t.Fatalf("current held authority must remain representable: %+v", action)
	}
	if action.PolicyAuthority.Allowed || action.Kind != ActionCopyOnly || !action.RequiresConfirmation {
		t.Fatalf("held authority became runnable: %+v", action)
	}
}

func TestReviewAuthorityHeldBundleOwnsGateRankAcrossPolicyRiskVocabulary(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		reversibility string
		blastRadius   string
	}{
		{name: "hard reversibility", reversibility: "hard", blastRadius: "repo"},
		{name: "no reversibility", reversibility: "none", blastRadius: "repo"},
		{name: "production radius", reversibility: "easy", blastRadius: "production"},
		{name: "account radius", reversibility: "easy", blastRadius: "account"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			held := attentionAuthorityActionWithRisk(t, now, "compound://policy/effective", false, tt.reversibility, tt.blastRadius)
			allowed := attentionAuthorityAction(t, now, "compound://policy/effective", true)
			heldGroup := singletonAuthorityReport(now, held).Groups[0]
			heldGroup.Key = "held"
			allowedGroup := singletonAuthorityReport(now, allowed).Groups[0]
			allowedGroup.Key = "allowed"
			allowedGroup.Count = 100
			reviews := &reviewartifact.QueueReport{
				GeneratedAt: now.Format(time.RFC3339),
				Groups:      []reviewartifact.QueueGroup{allowedGroup, heldGroup},
			}
			report := Build(Inputs{Reviews: reviews}, now)
			group := groupByKey(t, report, "held")
			if group.RiskGrade != RiskGradeGate || Class(group) != RiskGradeGate {
				t.Fatalf("held authority risk_grade/class = %q/%q, want gate for %q/%q", group.RiskGrade, Class(group), tt.reversibility, tt.blastRadius)
			}
			if report.Top == nil || report.Top.Key != "held" {
				t.Fatalf("held authority did not sort ahead of high-volume allowed work: top=%+v groups=%+v", report.Top, report.Groups)
			}
		})
	}
}

func TestLegacyRiskClassificationStillGatesWithoutAuthority(t *testing.T) {
	action := &Action{
		ID:                   "legacy",
		Argv:                 []string{"compound", "reviews", "inspect"},
		Kind:                 ActionOneClick,
		RequiresConfirmation: false,
	}
	group := Group{
		Kind:          KindHygiene,
		BlastRadius:   BlastPublic,
		Reversibility: Reversible,
		Action:        action,
	}

	if Class(group) != RiskGradeGate {
		t.Fatalf("legacy public-risk group class = %q, want gate", Class(group))
	}
	normalizeAction(action, group)
	if !action.RequiresConfirmation {
		t.Fatalf("legacy risk behavior changed: %+v", action)
	}
}

func TestReviewAuthorityInvalidBundlesFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	valid := attentionAuthorityAction(t, now, "compound://policy/effective", true)
	tests := []struct {
		name   string
		mutate func(*reviewartifact.Action)
	}{
		{name: "partial bundle", mutate: func(action *reviewartifact.Action) { action.PolicyDecision = nil }},
		{name: "malformed intent", mutate: func(action *reviewartifact.Action) { action.ActionIntent.SchemaVersion = "invalid" }},
		{name: "mismatched action id", mutate: func(action *reviewartifact.Action) { action.PolicyDecision.ActionIntentID = "act_other" }},
		{name: "mismatched revision", mutate: func(action *reviewartifact.Action) {
			action.PolicyAuthority.Revision = "policy-authority-v1:sha256:" + strings.Repeat("0", 64)
		}},
		{name: "noncurrent face", mutate: func(action *reviewartifact.Action) { action.PolicyAuthority.Current = false }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := cloneAttentionAuthorityAction(valid)
			tt.mutate(&action)
			report := Build(Inputs{Reviews: singletonAuthorityReport(now, action)}, now)
			got := groupByKey(t, report, "authority-singleton").Action
			if got == nil {
				t.Fatal("invalid producer action should remain visible for inspection")
			}
			if got.Kind != ActionCopyOnly || !got.RequiresConfirmation {
				t.Fatalf("invalid authority became runnable: %+v", got)
			}
		})
	}
}

func TestReviewActionWithoutAuthorityFailsClosed(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	source := reviewartifact.Action{
		ID:    "legacy_action",
		Label: "Inspect legacy action",
		Argv:  []string{"compound", "reviews", "inspect"},
	}
	report := Build(Inputs{Reviews: singletonAuthorityReport(now, source)}, now)

	action := groupByKey(t, report, "authority-singleton").Action
	if action == nil {
		t.Fatal("legacy action should remain visible")
	}
	if action.Kind != ActionCopyOnly || !action.RequiresConfirmation {
		t.Fatalf("legacy action without authority became runnable: %+v", action)
	}
}

func TestReviewAuthorityConcreteRiskOutranksCardTextInference(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	source := attentionAuthorityAction(t, now, "compound://policy/effective", true)
	reviews := singletonAuthorityReport(now, source)
	reviews.Groups[0].Area = "unknown-review-area"
	reviews.Groups[0].OperatorSummary = "Public irreversible wording must not replace producer-owned risk."
	reviews.Groups[0].OperatorUnlock = "Publish after review."
	report := Build(Inputs{Reviews: reviews}, now)

	group := groupByKey(t, report, "authority-singleton")
	if group.BlastRadius != source.ActionIntent.BlastRadius || group.Reversibility != source.ActionIntent.Reversibility {
		t.Fatalf("group risk = %q/%q, want exact action intent %q/%q", group.BlastRadius, group.Reversibility, source.ActionIntent.BlastRadius, source.ActionIntent.Reversibility)
	}
	if group.RiskGrade != Class(group) {
		t.Fatalf("risk_grade = %q, want existing Class result %q", group.RiskGrade, Class(group))
	}
}

func TestReviewAuthorityProjectionDoesNotAliasProducerState(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	source := attentionAuthorityAction(t, now, "compound://policy/effective", true)
	report := Build(Inputs{Reviews: singletonAuthorityReport(now, source)}, now)
	got := groupByKey(t, report, "authority-singleton").Action
	if got == nil || got.ActionIntent == nil || got.PolicyDecision == nil || got.PolicyAuthority == nil {
		t.Fatalf("forwarded bundle missing: %+v", got)
	}
	if got.ActionIntent == source.ActionIntent || got.PolicyDecision == source.PolicyDecision || got.PolicyAuthority == source.PolicyAuthority {
		t.Fatal("attention projection retained a producer authority pointer")
	}

	wantArgv := got.Argv[0]
	wantIntentEvidence := got.ActionIntent.EvidenceRefs[0]
	wantDecisionEvidence := got.PolicyDecision.EvidenceRefs[0]
	wantReason := got.PolicyDecision.Reasons[0]
	wantObligation := got.PolicyDecision.Obligations[0]
	wantProof := got.PolicyAuthority.ExpectedProof[0]
	wantUSD := *got.ActionIntent.EstimatedCost.USD
	source.Argv[0] = "changed"
	source.ActionIntent.EvidenceRefs[0] = "evidence:changed"
	*source.ActionIntent.EstimatedCost.USD = 99
	source.PolicyDecision.EvidenceRefs[0] = "evidence:changed"
	source.PolicyDecision.Reasons[0] = "changed"
	source.PolicyDecision.Obligations[0] = "changed"
	source.PolicyAuthority.ExpectedProof[0] = "proof:changed"

	if got.Argv[0] != wantArgv || got.ActionIntent.EvidenceRefs[0] != wantIntentEvidence || *got.ActionIntent.EstimatedCost.USD != wantUSD {
		t.Fatalf("action projection aliases producer action intent: %+v", got)
	}
	if got.PolicyDecision.EvidenceRefs[0] != wantDecisionEvidence || got.PolicyDecision.Reasons[0] != wantReason || got.PolicyDecision.Obligations[0] != wantObligation {
		t.Fatalf("action projection aliases producer policy decision: %+v", got.PolicyDecision)
	}
	if got.PolicyAuthority.ExpectedProof[0] != wantProof {
		t.Fatalf("action projection aliases producer authority face: %+v", got.PolicyAuthority)
	}
}

func TestReviewAuthorityIdentitySurvivesSingletonAndSafeBatch(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	first := attentionAuthorityAction(t, now, "compound://policy/effective", true)
	second := attentionAuthorityAction(t, now.Add(time.Minute), "compound://policy/effective", true)
	if first.PolicyDecision.ID == second.PolicyDecision.ID {
		t.Fatalf("fixture needs distinct policy decision ids: %q", first.PolicyDecision.ID)
	}
	if first.PolicyAuthority.Revision != second.PolicyAuthority.Revision {
		t.Fatalf("equivalent authority should have stable revision: %q != %q", first.PolicyAuthority.Revision, second.PolicyAuthority.Revision)
	}

	singleton := Build(Inputs{Reviews: singletonAuthorityReport(now, first)}, now)
	singleAction := groupByKey(t, singleton, "authority-singleton").Action
	if singleAction.ActionIntent.ID != first.ActionIntent.ID || singleAction.PolicyAuthority.Revision != first.PolicyAuthority.Revision {
		t.Fatalf("singleton authority identity changed: %+v", singleAction)
	}

	batchReport := authorityQueueReport(now, first, second)
	batch := Build(Inputs{Reviews: &batchReport}, now)
	batchAction := groupByKey(t, batch, "authority-batch").Action
	if batchAction == nil || batchAction.Kind != ActionBatch {
		t.Fatalf("equivalent authority did not survive safe batching: %+v", batchAction)
	}
	if batchAction.ActionIntent.ID != first.ActionIntent.ID || batchAction.PolicyAuthority.Revision != first.PolicyAuthority.Revision {
		t.Fatalf("batch authority identity changed: %+v", batchAction)
	}
}

func TestReviewAuthorityBatchRejectsDifferentAuthorityIdentity(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	first := attentionAuthorityAction(t, now, "compound://policy/effective", true)
	second := attentionAuthorityAction(t, now.Add(time.Minute), "compound://policy/revised", true)
	if first.PolicyAuthority.Revision == second.PolicyAuthority.Revision {
		t.Fatal("fixture needs different authority revisions")
	}

	reviews := authorityQueueReport(now, first, second)
	if len(reviews.Groups) != 1 || reviews.Groups[0].SafeBatchAction != nil {
		t.Fatalf("review producer unexpectedly exposed a batch action: %+v", reviews.Groups)
	}
	report := Build(Inputs{Reviews: &reviews}, now)
	if action := groupByKey(t, report, "authority-batch").Action; action != nil {
		t.Fatalf("attention recombined differing authority identity: %+v", action)
	}
}

func singletonAuthorityReport(now time.Time, action reviewartifact.Action) *reviewartifact.QueueReport {
	return &reviewartifact.QueueReport{
		GeneratedAt: now.Format(time.RFC3339),
		Groups: []reviewartifact.QueueGroup{{
			Key:             "authority-singleton",
			Area:            "owner-review",
			Band:            reviewartifact.BandNeedsYou,
			Count:           1,
			OperatorSummary: "Review the governed change.",
			ItemIDs:         []string{"review_a"},
			Sample:          &reviewartifact.QueueItem{ID: "review_a", Area: "owner-review", Actions: []reviewartifact.Action{action}},
		}},
	}
}

func authorityQueueReport(now time.Time, actions ...reviewartifact.Action) reviewartifact.QueueReport {
	report := reviewartifact.QueueReport{GeneratedAt: now.Format(time.RFC3339)}
	for i, action := range actions {
		report.Items = append(report.Items, reviewartifact.QueueItem{
			Area:     "owner-review",
			ID:       "review_" + string(rune('a'+i)),
			Status:   "review_required",
			GroupKey: "authority-batch",
			Band:     reviewartifact.BandNeedsYou,
			Actions:  []reviewartifact.Action{action},
		})
	}
	reviewartifact.Recount(&report)
	return report
}

func attentionAuthorityAction(t *testing.T, now time.Time, policySource string, allowed bool) reviewartifact.Action {
	return attentionAuthorityActionWithRisk(t, now, policySource, allowed, "easy", "repo")
}

func attentionAuthorityActionWithRisk(t *testing.T, now time.Time, policySource string, allowed bool, reversibility, blastRadius string) reviewartifact.Action {
	t.Helper()
	estimatedUSD := 0.25
	estimatedTokens := 10
	estimatedReviewMinutes := 1
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_attention_authority",
			CreatedAt:     "2026-07-17T09:00:00Z",
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: "work_attention_authority"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{"evidence:attention-authority"},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "compound",
		Capability:    "artifact.update",
		Action:        "update",
		Target:        envelope.ActionTarget{Kind: "file", Path: "artifacts/item.json"},
		MutationClass: "local_reversible",
		Reversibility: reversibility,
		BlastRadius:   blastRadius,
		EstimatedCost: envelope.EstimatedCost{
			USD:            &estimatedUSD,
			Tokens:         &estimatedTokens,
			HumanReviewMin: &estimatedReviewMinutes,
		},
	}
	cfg := policy.DefaultConfig()
	if allowed {
		maxUSD := 1.0
		maxTokens := 100
		cfg.StandingGrants = []policy.StandingGrant{{
			ID:                   "grant_attention_authority",
			Capability:           ai.Capability,
			MutationClass:        ai.MutationClass,
			ReversibilityCeiling: "reversible",
			BlastRadius:          ai.BlastRadius,
			TargetPath:           "artifacts/**",
			MaxUSD:               &maxUSD,
			MaxTokens:            &maxTokens,
		}}
	} else {
		cfg.ClassOutcome[ai.MutationClass] = policy.ClassRule{Outcome: "hold"}
	}
	context := &policy.AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    policy.RecoverabilityVerified,
		OperatorControlContinuity: policy.OperatorControlContinuous,
		ExpectedReceipt:           "governed action receipt",
		ExpectedProof:             []string{"proof:recovery-path"},
		EvidenceRefs:              []string{"evidence:owner-outcome"},
	}
	evaluation := policy.EvaluateAuthority(ai, cfg, policy.EvaluationContext{Authority: context}, policySource, now)
	if err := policy.ValidateAuthorityBundle(ai, evaluation.Decision, evaluation.Authority); err != nil {
		t.Fatalf("invalid authority fixture: %v", err)
	}
	if evaluation.Authority.Allowed != allowed {
		t.Fatalf("authority fixture allowed=%v, want %v", evaluation.Authority.Allowed, allowed)
	}
	return reviewartifact.Action{
		ID:              "owner_action",
		Label:           "Apply",
		Argv:            []string{"compound", "owner", "apply"},
		ActionIntent:    &ai,
		PolicyDecision:  &evaluation.Decision,
		PolicyAuthority: &evaluation.Authority,
	}
}

func cloneAttentionAuthorityAction(in reviewartifact.Action) reviewartifact.Action {
	out := in
	out.Argv = append([]string{}, in.Argv...)
	if in.ActionIntent != nil {
		intent := *in.ActionIntent
		intent.EvidenceRefs = append([]string{}, in.ActionIntent.EvidenceRefs...)
		intent.EstimatedCost.USD = float64PointerValue(in.ActionIntent.EstimatedCost.USD)
		intent.EstimatedCost.Tokens = intPointerValue(in.ActionIntent.EstimatedCost.Tokens)
		intent.EstimatedCost.HumanReviewMin = intPointerValue(in.ActionIntent.EstimatedCost.HumanReviewMin)
		out.ActionIntent = &intent
	}
	if in.PolicyDecision != nil {
		decision := *in.PolicyDecision
		decision.EvidenceRefs = append([]string{}, in.PolicyDecision.EvidenceRefs...)
		decision.Reasons = append([]string{}, in.PolicyDecision.Reasons...)
		decision.Obligations = append([]string{}, in.PolicyDecision.Obligations...)
		out.PolicyDecision = &decision
	}
	if in.PolicyAuthority != nil {
		face := *in.PolicyAuthority
		face.ExpectedProof = append([]string{}, in.PolicyAuthority.ExpectedProof...)
		out.PolicyAuthority = &face
	}
	return out
}

func float64Pointer(value float64) *float64 { return &value }

func float64PointerValue(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return float64Pointer(*value)
}

func intPointerValue(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
