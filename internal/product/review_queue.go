package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/envelope"
	"compound/internal/reviewartifact"
)

const productReviewArea = "product-reviews"

// productHypothesisReviewArea is the reviews area the product package files
// hypothesis plans under (reviewartifact.Dir "product-hypotheses"; see
// RecordHypothesisPlan).
const productHypothesisReviewArea = "product-hypotheses"

// AnnotateReviewQueue attaches product-review hints to generic review artifacts.
// It does not load product ledgers, mutate artifacts, apply reviews, or change
// hypotheses; it only makes the existing reviewed-result import seam visible.
func AnnotateReviewQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		switch item.Area {
		case productReviewArea:
			annotateProductReviewItem(item)
		case productHypothesisReviewArea:
			annotateProductHypothesisItem(item)
		case ProductActionReviewArea:
			annotateProductActionReviewItem(item)
		case ObservationVerdictReviewArea:
			annotateObservationVerdictItem(item)
		case ProductReleaseReviewArea:
			annotateProductReleaseItem(item)
		}
	}
}

// annotateProductHypothesisItem replaces the product-hypotheses area's
// words-only group fallback ("approve it" with no command) with the governed
// approve verb as the item's queue Action, so `compound reviews` and the
// console can fire it. The action's argv deliberately omits
// --expected-effect — the operator sentence is required input a renderer
// collects (the route-promotions --rationale precedent); running the bare
// argv fails closed with "expected_effect required".
func annotateProductHypothesisItem(item *reviewartifact.QueueItem) {
	if reviewartifact.TerminalStatus(item.Status) {
		item.Readiness = "no_action"
		item.NextAction = "product hypothesis artifact terminal"
		item.Actions = nil
		return
	}
	candidateID := strings.TrimSpace(item.CandidateID)
	if candidateID == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "hypothesis plan artifact carries no candidate id"
		item.NextAction = "regenerate product hypothesis-plan so the candidate id is recorded"
		item.Actions = nil
		return
	}
	item.Readiness = "needs_review"
	item.BlockedReason = ""
	action := productHypothesisApproveAction(candidateID)
	seedProductHypothesisApproveAuthority(&action, *item)
	item.Actions = []reviewartifact.Action{action}
	item.NextAction = "write your expected effect and approve the hypothesis"
	if plan, err := hypothesisPlanFromQueueItem(*item); err == nil && strings.HasPrefix(strings.TrimSpace(plan.ExpectedEffect), HypothesisDraftExpectedEffectPlaceholderPrefix) {
		// A plan minted before the placeholder gate landed (legacy row): the
		// hypothesis literally is the blank. Approval is the repair path —
		// re-plan after it succeeds.
		item.BlockedReason = "plan was minted from an unfilled draft: its expected_effect is the operator placeholder"
		item.NextAction = "approve the candidate with your expected effect, then re-run product hypothesis-plan"
	}
}

// productHypothesisApproveAction emits the one clean-argv (argv[0]=="compound",
// no shell) action for a parked hypothesis awaiting the operator's sentence.
// The CLI requires --expected-effect; it is deliberately omitted from Argv —
// a governed renderer appends ["--expected-effect", <operator text>] at
// execution time (the route-promotions --rationale precedent), and the verb
// itself refuses the "OPERATOR MUST WRITE" placeholder, so no placeholder
// sentence can ever run through this action.
func productHypothesisApproveAction(candidateID string) reviewartifact.Action {
	argv := []string{"compound", "product", "hypothesis-approve", candidateID}
	return reviewartifact.Action{
		ID:             "product_hypothesis_approve",
		Label:          "Approve product hypothesis",
		Command:        shellJoin([]string{"compound", "product", "hypothesis-approve", candidateID, "--expected-effect", "<what should the metric do after the change?>"}),
		Argv:           argv,
		RequiresReview: true,
		PolicyNote:     "writes the operator expected_effect and one approval receipt; verdict stays park so the hypothesis stays in pending review; refuses the OPERATOR MUST WRITE placeholder",
	}
}

func seedProductHypothesisApproveAuthority(action *reviewartifact.Action, item reviewartifact.QueueItem) {
	workID := reviewartifact.AuthoritySeedWorkID(item, "")
	if workID == "" {
		return
	}
	spec := reviewartifact.ActionAuthoritySeedSpec{
		WorkID: workID, CreatedAt: item.CreatedAt, ProducerName: "compound/internal/product",
		Capability: "review.product_hypothesis", Action: "product_hypothesis_approve",
		Target:        envelope.ActionTarget{Kind: "product_hypothesis_candidate", ID: strings.TrimSpace(item.CandidateID), Path: item.JSONPath},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local",
		Preview:      "Write the reviewed expected_effect onto the parked hypothesis candidate and record one approval receipt; the verdict stays park and no product surface changes.",
		RollbackPlan: "Approve again with a corrected sentence; the approval transition log is append-only.",
		Privacy:      "private", Risk: "tier1", TargetScope: "private",
		ExpectedReceipt: "One product_hypothesis_approve loop transition for the candidate.",
		ExpectedProof:   []string{"product_hypothesis:" + strings.TrimSpace(item.CandidateID)},
	}
	_ = reviewartifact.SeedActionAuthority(action, spec)
}

func hypothesisPlanFromQueueItem(item reviewartifact.QueueItem) (HypothesisPlan, error) {
	var plan HypothesisPlan
	if strings.TrimSpace(item.JSONPath) == "" {
		return plan, fmt.Errorf("product hypothesis plan path missing")
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return plan, err
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return plan, err
	}
	return plan, nil
}

func annotateProductReviewItem(item *reviewartifact.QueueItem) {
	if record, ok := productReviewRecordFromExtras(item.ExtraPaths); ok {
		item.Status = record.Status
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "product review recorded"
		item.Actions = nil
		return
	}
	// Terminal words are owned by reviewartifact.TerminalStatus; the switch
	// this replaced listed five of the seven and billed candidate_recorded
	// and completed reviews to the operator as missing drafts.
	if reviewartifact.TerminalStatus(item.Status) {
		item.Readiness = "no_action"
		item.NextAction = "product review artifact is terminal"
		return
	}
	item.Readiness = "needs_review"
	item.NextAction = "fill review YAML after evidence review, then record product review"
	reviewYAML := productReviewYAMLPath(*item)
	if reviewYAML == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft missing"
		item.NextAction = "review YAML draft missing; regenerate product review-plan"
		return
	}
	argv := []string{"compound", "product", "review", reviewYAML}
	item.Actions = []reviewartifact.Action{{
		ID:             "record_product_review",
		Label:          "Record product review",
		Command:        shellJoin(argv),
		Argv:           argv,
		RequiresReview: true,
	}}
	review, err := LoadReview(reviewYAML)
	if err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft unreadable: " + productReviewQueueError(err)
		item.NextAction = "fix review YAML draft before recording product review"
		return
	}
	if err := ValidateReview(review); err != nil {
		item.BlockedReason = productReviewQueueError(err)
		item.NextAction = "fill valid review YAML before recording product review"
		return
	}
	item.Readiness = "ready_to_record"
	item.NextAction = "record product review"
}

func annotateProductActionReviewItem(item *reviewartifact.QueueItem) {
	plan, planErr := actionPlanFromQueueItem(*item)
	if record, ok := productActionReviewRecordFromExtras(item.ExtraPaths); ok {
		item.Status = record.Status
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "product action review recorded"
		item.Actions = nil
		if planErr == nil {
			annotateActionPlanOperatorCard(item, plan, reviewartifact.BandFiled)
		}
		return
	}
	// Terminal words are owned by reviewartifact.TerminalStatus; this switch
	// carried the same stale five-word copy as the product review annotator.
	if reviewartifact.TerminalStatus(item.Status) {
		item.Readiness = "no_action"
		item.NextAction = "product action review artifact terminal"
		item.Actions = nil
		if planErr == nil {
			annotateActionPlanOperatorCard(item, plan, reviewartifact.BandFiled)
		}
		return
	}
	item.Readiness = "needs_review"
	item.NextAction = "fill action review YAML, then record product action review"
	if planErr == nil {
		annotateActionPlanOperatorCard(item, plan, reviewartifact.BandNeedsYou)
	}
	reviewYAML := productActionReviewYAMLPath(*item)
	if reviewYAML == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "action review YAML draft missing"
		item.NextAction = "action review YAML draft missing; regenerate product action-plan"
		return
	}
	argv := []string{"compound", "product", "action-review", reviewYAML}
	item.Actions = []reviewartifact.Action{{
		ID:             "record_product_action_review",
		Label:          "Record product action review",
		Command:        shellJoin(argv),
		Argv:           argv,
		RequiresReview: true,
	}}
	item.DecisionCard = &reviewartifact.DecisionCard{
		Claim: "Product loop action card is ready for operator review before work admission",
		Why: []reviewartifact.DecisionCardReason{
			{Label: "Projected action", Detail: item.ID},
			{Label: "Evidence refs", Detail: strings.Join(item.EvidenceRefs, ", ")},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{ID: "propose_work", Label: "Propose work", Effect: "record reviewed action-card intent for a later WorkEnvelope/implementation step"},
			{ID: "park", Label: "Park", Effect: "keep evidence visible without admitting work"},
			{ID: "reject", Label: "Reject", Effect: "record that this projected action should not proceed"},
		},
		Boundary: []string{
			"review records no product signal, hypothesis, review, or transition",
			"review grants no public, financial, credential, destructive, or security-sensitive authority",
		},
		CanonicalActionHint: shellJoin(argv),
	}
	if _, err := os.ReadFile(reviewYAML); err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "action review YAML draft unreadable: " + productReviewQueueError(err)
		item.NextAction = "regenerate product action-plan"
		item.Actions = nil
	}
}

func annotateActionPlanOperatorCard(item *reviewartifact.QueueItem, plan ActionPlan, band string) {
	card, err := OperatorCardForActionPlan(plan)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "product action plan operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "product action plan operator card invalid: "+err.Error())
	}
}

func actionPlanFromQueueItem(item reviewartifact.QueueItem) (ActionPlan, error) {
	if strings.TrimSpace(item.JSONPath) == "" {
		return ActionPlan{}, fmt.Errorf("product action plan path missing")
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return ActionPlan{}, err
	}
	var plan ActionPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ActionPlan{}, err
	}
	return plan, nil
}

func productActionReviewYAMLPath(item reviewartifact.QueueItem) string {
	if item.JSONPath == "" {
		return ""
	}
	path := strings.TrimSuffix(item.JSONPath, ".json") + ".review.yaml"
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func productActionReviewRecordFromExtras(paths []string) (ActionReviewReport, bool) {
	var found ActionReviewReport
	for _, path := range paths {
		if !strings.HasSuffix(path, ".reviewed.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record ActionReviewReport
		if err := json.Unmarshal(data, &record); err != nil {
			continue
		}
		if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.Status) == "" {
			continue
		}
		if found.ID == "" || record.CreatedAt > found.CreatedAt {
			found = record
		}
	}
	return found, found.ID != ""
}

func productReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(filepath.Base(path), ".review.yaml") {
			return path
		}
	}
	if item.JSONPath == "" {
		return ""
	}
	path := strings.TrimSuffix(item.JSONPath, ".json") + ".review.yaml"
	return path
}

func productReviewRecordFromExtras(paths []string) (ReviewArtifactRecord, bool) {
	var found ReviewArtifactRecord
	for _, path := range paths {
		if !strings.HasSuffix(path, ".reviewed.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record ReviewArtifactRecord
		if err := json.Unmarshal(data, &record); err != nil {
			continue
		}
		if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.Status) == "" {
			continue
		}
		if found.ID == "" || record.CreatedAt > found.CreatedAt {
			found = record
		}
	}
	return found, found.ID != ""
}

func shellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '/' || r == '.' || r == ':' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func productReviewQueueError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 240 {
		msg = msg[:237] + "..."
	}
	return msg
}
