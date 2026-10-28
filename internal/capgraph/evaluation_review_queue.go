package capgraph

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"compound/internal/reviewartifact"
)

const capabilityEvaluationReviewArea = "capability-evaluation-intake"

// AnnotateCapabilityEvaluationQueue makes reviewed capability-evaluation plans
// terminal in the shared queue without importing manifests or mutating graph
// offers.
func AnnotateCapabilityEvaluationQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != capabilityEvaluationReviewArea {
			continue
		}
		annotateCapabilityEvaluationQueueItem(item)
	}
}

func annotateCapabilityEvaluationQueueItem(item *reviewartifact.QueueItem) {
	item.EvidenceRefs = reviewartifact.UniqueSorted(item.EvidenceRefs)
	plan, ok := capabilityEvaluationPlanFromQueueItem(*item)
	// Terminal words are owned by reviewartifact.TerminalStatus; this used to
	// re-list them inline and its stale copy omitted "completed", so completed
	// artifacts matched no case and left the annotator without a readiness.
	if reviewartifact.TerminalStatus(item.Status) {
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "capability evaluation review recorded; graph mutation requires separate reviewed manifest import"
		item.Actions = nil
		if ok {
			annotateCapabilityEvaluationOperatorCard(item, plan, reviewartifact.BandFiled)
		}
		return
	}
	switch strings.TrimSpace(item.Status) {
	case "", "unknown", "review_required":
		if !ok {
			item.Status = "blocked"
			item.Readiness = "blocked"
			item.BlockedReason = "capability evaluation plan artifact unreadable"
			item.NextAction = "regenerate capability evaluation plan"
			item.Actions = nil
			return
		}
		if strings.TrimSpace(item.Status) == "" || strings.TrimSpace(item.Status) == "unknown" {
			item.Status = "review_required"
		}
		item.Readiness = "needs_review"
		item.BlockedReason = ""
		item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, plan.EvidenceRefs...))
		item.DecisionCard = buildCapabilityEvaluationDecisionCard(*item, plan)
		item.Actions = capabilityEvaluationDecisionCardActions(item.DecisionCard)
		item.NextAction = fmt.Sprintf("review capability evaluation for %s; edit YAML, then run evaluation-review", reviewartifact.FirstNonEmpty(plan.Capability, plan.NodeDraft.NodeID, item.ID))
		annotateCapabilityEvaluationOperatorCard(item, plan, reviewartifact.BandNeedsYou)
	}
}

func annotateCapabilityEvaluationOperatorCard(item *reviewartifact.QueueItem, plan CapabilityEvaluationPlan, band string) {
	card, err := OperatorCardForEvaluationPlan(plan)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "capability evaluation operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "capability evaluation operator card invalid: "+err.Error())
	}
}

func capabilityEvaluationPlanFromQueueItem(item reviewartifact.QueueItem) (CapabilityEvaluationPlan, bool) {
	if strings.TrimSpace(item.JSONPath) == "" {
		return CapabilityEvaluationPlan{}, false
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return CapabilityEvaluationPlan{}, false
	}
	var plan CapabilityEvaluationPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return CapabilityEvaluationPlan{}, false
	}
	return plan, strings.TrimSpace(plan.ID) != ""
}

func buildCapabilityEvaluationDecisionCard(item reviewartifact.QueueItem, plan CapabilityEvaluationPlan) *reviewartifact.DecisionCard {
	reviewPath := capabilityEvaluationReviewYAMLPath(item)
	manifestPath := capabilityEvaluationManifestDraftPath(item)
	externalRepo := signalString(plan.Signals, "external_repo")
	owner := signalString(plan.Signals, "existing_owner")
	requiredNextProof := signalString(plan.Signals, "required_next_proof")
	mainRisks := signalString(plan.Signals, "main_risks")
	claim := "Review capability draft " + reviewartifact.FirstNonEmpty(plan.NodeDraft.NodeID, plan.Capability, item.ID) + "."
	if externalRepo != "" {
		claim = "Decide whether " + externalRepo + " should remain review evidence, be rejected, or become a reviewed capability manifest."
	}
	card := &reviewartifact.DecisionCard{
		Claim:               claim,
		OptionalRationale:   true,
		CanonicalActionHint: "Edit the review YAML with decision/rationale/confidence, then run `compound capability evaluation-review <review.yaml>`. This emits a reviewed manifest only; graph import/surfacing is a separate gate.",
		Boundary: []string{
			"read-only projection",
			"review YAML must be edited before command",
			"evaluation-review does not import graph offers",
			"no tool execution, install, auth, or memory promotion",
		},
		Meta: map[string]string{
			"capability":     plan.Capability,
			"node_id":        plan.NodeDraft.NodeID,
			"node_kind":      plan.NodeDraft.Kind,
			"review_yaml":    reviewPath,
			"manifest_draft": manifestPath,
		},
	}
	if externalRepo != "" {
		card.Meta["external_repo"] = externalRepo
	}
	if owner != "" {
		card.Meta["owner"] = owner
	}
	card.Why = []reviewartifact.DecisionCardReason{
		{
			Label:    "source",
			Question: "Where did this capability draft come from?",
			Detail:   reviewartifact.FirstNonEmpty(externalRepo, plan.SourceReceipt, plan.EvaluationID),
		},
		{
			Label:    "draft",
			Question: "What would be reviewed?",
			Detail:   capabilityEvaluationDraftDetail(plan),
		},
		{
			Label:    "boundary",
			Question: "What must not happen automatically?",
			Severity: "warning",
			Detail:   reviewartifact.FirstNonEmpty(mainRisks, strings.Join(plan.Warnings, "; "), "no graph import or execution from review queue"),
		},
	}
	if requiredNextProof != "" {
		card.Why = append(card.Why, reviewartifact.DecisionCardReason{
			Label:    "next proof",
			Question: "What evidence is still required?",
			Detail:   requiredNextProof,
		})
	}
	contextProofs := signalStringSlice(plan.Signals, "context_evaluation_proofs")
	contextSilence := signalStringSlice(plan.Signals, "context_evaluation_silence")
	contextRetirement := signalStringSlice(plan.Signals, "context_evaluation_retirement")
	for i, lens := range signalStringSlice(plan.Signals, "context_evaluation_lenses") {
		detail := strings.Join(nonEmptyStrings(
			indexedSignal(contextProofs, i),
			indexedSignal(contextSilence, i),
			indexedSignal(contextRetirement, i),
		), "; ")
		if detail == "" {
			detail = "review proof, silence condition, and retirement path before graph import"
		}
		card.Why = append(card.Why, reviewartifact.DecisionCardReason{
			Label:    "evaluation lens: " + lens,
			Question: "What context-aware constraint must be reviewed before adoption?",
			Detail:   detail,
		})
	}
	card.Choices = capabilityEvaluationDecisionChoices(reviewPath)
	return card
}

func indexedSignal(values []string, i int) string {
	if i < 0 || i >= len(values) {
		return ""
	}
	return values[i]
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func capabilityEvaluationDraftDetail(plan CapabilityEvaluationPlan) string {
	parts := []string{}
	if plan.NodeDraft.Kind != "" {
		parts = append(parts, "kind="+plan.NodeDraft.Kind)
	}
	if plan.NodeDraft.Name != "" {
		parts = append(parts, "name="+plan.NodeDraft.Name)
	}
	if len(plan.NodeDraft.Triggers) > 0 {
		triggers := append([]string(nil), plan.NodeDraft.Triggers...)
		if len(triggers) > 6 {
			triggers = triggers[:6]
			triggers = append(triggers, "...")
		}
		parts = append(parts, "triggers="+strings.Join(triggers, ", "))
	}
	if len(parts) == 0 {
		return reviewartifact.FirstNonEmpty(plan.Capability, plan.ID)
	}
	return strings.Join(parts, "; ")
}

func capabilityEvaluationDecisionChoices(reviewPath string) []reviewartifact.DecisionCardChoice {
	choices := []struct {
		id     string
		label  string
		effect string
	}{
		{"keep_needs_review", "Keep Review", "Keep as reviewed evidence or needs_review manifest; does not activate or import graph offers."},
		{"activate", "Activate Manifest", "Emit a reviewed active manifest; graph import/surfacing still requires a separate explicit import gate."},
		{"reject", "Reject", "Record rejection; no manifest adoption, execution, or graph import."},
	}
	out := make([]reviewartifact.DecisionCardChoice, 0, len(choices))
	for _, choice := range choices {
		cmd := capabilityEvaluationReviewCommand(reviewPath)
		out = append(out, reviewartifact.DecisionCardChoice{
			ID:             choice.id,
			Label:          choice.label,
			Effect:         choice.effect,
			Command:        cmd,
			Argv:           []string{"/bin/sh", "-lc", cmd},
			RequiresReview: true,
		})
	}
	return out
}

func capabilityEvaluationReviewCommand(reviewPath string) string {
	if strings.TrimSpace(reviewPath) == "" {
		return "compound capability evaluation-review <review.yaml>"
	}
	quoted := shellQuote(reviewPath)
	return "$EDITOR " + quoted + " && compound capability evaluation-review " + quoted
}

func capabilityEvaluationDecisionCardActions(card *reviewartifact.DecisionCard) []reviewartifact.Action {
	if card == nil {
		return nil
	}
	out := make([]reviewartifact.Action, 0, len(card.Choices))
	for _, choice := range card.Choices {
		out = append(out, reviewartifact.Action{
			ID:             choice.ID,
			Label:          choice.Label,
			Command:        choice.Command,
			Argv:           append([]string(nil), choice.Argv...),
			RequiresReview: choice.RequiresReview,
		})
	}
	return out
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
