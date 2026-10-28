package panel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

const verdictReviewArea = "panel-verdicts"

// AnnotateVerdictQueue makes recorded panel verdicts legible in the shared
// review queue without applying any verdict or mutating memory authority.
func AnnotateVerdictQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	if !hasPanelVerdictQueueItems(report.Items) {
		return
	}
	memoryCandidateIDs := memoryCandidateIDsFromQueueReport(report)
	homePath := homeFromReviewRoot(report.ReviewRoot)
	applications := verdictApplicationsByArtifact(report.ReviewRoot)
	memoryTargets := memoryCandidatePanelTargets(homePath, memoryCandidateIDs)
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != verdictReviewArea {
			continue
		}
		annotateVerdictQueueItem(item, applications, homePath, memoryTargets)
	}
}

func hasPanelVerdictQueueItems(items []reviewartifact.QueueItem) bool {
	for _, item := range items {
		if item.Area == verdictReviewArea {
			return true
		}
	}
	return false
}

func annotateVerdictQueueItem(item *reviewartifact.QueueItem, applications map[string]verdictApplication, homePath string, memoryTargets map[string]memoryCandidatePanelTarget) {
	verdict, ok := verdictFromQueueItem(*item)
	if !ok {
		item.Status = "blocked"
		item.Readiness = "blocked"
		item.BlockedReason = "panel verdict artifact unreadable"
		item.NextAction = "regenerate panel verdict artifacts"
		item.Band = reviewartifact.BandNeedsYou
		item.Actions = nil
		return
	}

	item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, verdict.EvidenceRefs...))
	item.Actions = nil

	if reviewedPath := panelVerdictReviewedPath(*item); reviewedPath != "" {
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.BlockedReason = ""
		item.NextAction = "panel verdict human review recorded"
		annotateVerdictOperatorCard(item, verdict, reviewartifact.BandFiled, "The judge panel verdict for "+panelVerdictContextLabel(verdict.Target)+" is already recorded.")
		if report, err := LoadVerdictHumanReviewReport(reviewedPath); err == nil {
			item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, report.EvidenceRefs...))
			item.NextAction = reviewartifact.FirstNonEmpty(report.NextAction, item.NextAction)
		}
		return
	}

	if app, ok := applicationForVerdictItem(*item, applications); ok && terminalPanelApplication(app.Application, verdict.Synthesis.Decision) {
		item.Status = "applied"
		item.Readiness = "no_action"
		item.BlockedReason = ""
		target := panelVerdictTarget(verdict.Target)
		item.NextAction = fmt.Sprintf("panel verdict already applied by %s (target=%s decision=%s mapped=%s)", app.RunID, target, strings.TrimSpace(verdict.Synthesis.Decision), app.Application.MappedVerdict)
		annotateVerdictOperatorCard(item, verdict, reviewartifact.BandFiled, "The judge panel verdict for "+panelVerdictContextLabel(verdict.Target)+" is already applied.")
		return
	}
	if nextAction, ok := resolvedPanelVerdictTarget(homePath, verdict, memoryTargets); ok {
		item.Status = "resolved"
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = nextAction
		annotateVerdictOperatorCard(item, verdict, reviewartifact.BandFiled, "The judge panel verdict for "+panelVerdictContextLabel(verdict.Target)+" is already resolved.")
		return
	}

	status := strings.TrimSpace(item.Status)
	// Terminal words are owned by reviewartifact.TerminalStatus; the switch
	// this replaced listed five of the seven and billed candidate_recorded
	// and completed verdicts to the operator as open decisions.
	if reviewartifact.TerminalStatus(status) {
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "panel verdict artifact terminal"
		annotateVerdictOperatorCard(item, verdict, reviewartifact.BandFiled, "")
		return
	}

	decision := strings.TrimSpace(verdict.Synthesis.Decision)
	if decision == "" {
		if status == "" || status == "unknown" {
			item.Status = "blocked"
		}
		item.Readiness = "blocked"
		item.BlockedReason = "panel verdict missing synthesis.decision"
		item.NextAction = "regenerate panel verdict artifact with synthesis decision"
		item.Band = reviewartifact.BandNeedsYou
		return
	}

	if status == "" || status == "unknown" {
		item.Status = "review_required"
	}
	item.Readiness = "needs_review"
	item.BlockedReason = ""
	item.DecisionCard = buildPanelDecisionCard(*item, verdict)
	item.Actions = panelDecisionCardActions(item.DecisionCard)
	item.Actions = append(item.Actions, panelMemberCleanAction(item.JSONPath))
	seedPanelVerdictActions(item, verdict, homePath)
	annotateVerdictOperatorCard(item, verdict, reviewartifact.BandNeedsYou, "")
	target := panelVerdictTarget(verdict.Target)
	failedLenses := failedLensCount(verdict.LensFindings)
	switch decision {
	case "needs_human":
		item.NextAction = fmt.Sprintf("owner review required; panel requested human decision (target=%s decision=%s failed_lenses=%d)", target, decision, failedLenses)
	case "promote", "park", "reject":
		item.NextAction = fmt.Sprintf("owner review required before panel apply (target=%s decision=%s failed_lenses=%d)", target, decision, failedLenses)
	default:
		item.NextAction = fmt.Sprintf("owner review required for panel verdict (target=%s decision=%s failed_lenses=%d)", target, decision, failedLenses)
	}
}

func annotateVerdictOperatorCard(item *reviewartifact.QueueItem, verdict verdictArtifactPayload, band, summaryOverride string) {
	card, err := OperatorCardForVerdict(verdict)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "panel verdict operator card invalid: "+err.Error())
		return
	}
	if strings.TrimSpace(summaryOverride) != "" {
		card.Summary = reviewartifact.ShortLabel(summaryOverride, 159)
		if !strings.HasSuffix(card.Summary, ".") {
			card.Summary += "."
		}
		card.Unlock = ""
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "panel verdict operator card invalid: "+err.Error())
	}
}

func init() {
	reviewartifact.RegisterGroupSummary(verdictReviewArea, panelVerdictGroupBatchAction)
}

// panelVerdictGroupBatchAction exposes only owner-proven group verbs. Human
// review groups get a record-only batch command; canonical apply remains
// run-scoped and is offered only after the run itself permits apply.
func panelVerdictGroupBatchAction(group reviewartifact.QueueGroup) reviewartifact.GroupSummaryResult {
	if group.Band != reviewartifact.BandNeedsYou {
		return reviewartifact.GroupSummaryResult{}
	}
	runID, ok := panelVerdictGroupRunID(group.Key)
	if !ok {
		return reviewartifact.GroupSummaryResult{}
	}
	if group.Sample == nil || group.Sample.JSONPath == "" {
		return reviewartifact.GroupSummaryResult{}
	}
	if decision, ok := panelVerdictGroupDecision(group.Key); ok && panelDecisionRecordableByHuman(decision) {
		argv := []string{
			"compound",
			"panel",
			"verdict-record",
			"--group",
			group.Key,
			"--rationale",
			"operator recorded grouped panel human-review verdicts",
			"--reviewed-by",
			defaultPanelReviewedBy(),
		}
		return reviewartifact.GroupSummaryResult{
			BatchAction: panelSeededGroupAction(&reviewartifact.Action{
				ID:             "panel_verdict_record_group",
				Label:          fmt.Sprintf("Record %d reviews", group.Count),
				Command:        panelShellJoin(argv),
				Argv:           argv,
				RequiresReview: true,
				PolicyNote:     "one operator act; member review refs included; no panel apply or memory mutation",
			}, group, runID, "record_panel_review_group"),
		}
	}
	homePath := homeFromReviewRoot(filepath.Dir(filepath.Dir(group.Sample.JSONPath)))
	if homePath == "" {
		return reviewartifact.GroupSummaryResult{}
	}
	store, err := Open(homePath)
	if err != nil {
		return reviewartifact.GroupSummaryResult{}
	}
	run, err := store.LoadRun(runID)
	if err != nil {
		return reviewartifact.GroupSummaryResult{
			BatchUnavailableReason: "panel run record unreadable; apply from the run directly",
		}
	}
	if run.Status != "verdicts_recorded" && run.Status != "applied" {
		return reviewartifact.GroupSummaryResult{
			BatchUnavailableReason: fmt.Sprintf("panel run is %s, not ready to apply yet", reviewartifact.PlainLabel(run.Status)),
		}
	}
	argv := []string{"compound", "panel", "run", "--plan", runID, "--apply"}
	return reviewartifact.GroupSummaryResult{
		BatchAction: panelSeededGroupAction(&reviewartifact.Action{
			ID:             "panel_run_apply",
			Label:          fmt.Sprintf("Apply %d verdicts", group.Count),
			Command:        panelShellJoin(argv),
			Argv:           argv,
			RequiresReview: true,
			PolicyNote:     "one operator act; each target gets its own PolicyDecision",
		}, group, runID, "apply_panel_run"),
	}
}

func seedPanelVerdictActions(item *reviewartifact.QueueItem, verdict verdictArtifactPayload, homePath string) {
	if item == nil {
		return
	}
	workID := panelVerdictSeedWorkID(*item, verdict.RunID, homePath)
	if workID == "" {
		return
	}
	evidenceRefs := reviewartifact.UniqueSorted(append(append([]string{}, verdict.EvidenceRefs...),
		"panel-run:"+verdict.RunID,
		"panel-verdict:"+verdict.ID,
	))
	for i := range item.Actions {
		action := &item.Actions[i]
		target := envelope.ActionTarget{Kind: verdict.Target.Kind, ID: verdict.Target.Ref}
		capability := "panel.adjudicate"
		actionName := "adjudicate_panel_verdict_" + action.ID
		preview := "Record the reviewed panel verdict and apply the selected bounded adjudication to " + panelVerdictTarget(verdict.Target) + "."
		rollback := "Retain the append-only review evidence and record the inverse or superseding candidate disposition."
		expectedReceipt := "Recorded and adjudicated panel verdict " + verdict.ID + " under run " + verdict.RunID + "."
		if action.ID == "panel_verdict_record_member" {
			target = envelope.ActionTarget{Kind: "panel_verdict", ID: verdict.ID, Path: item.JSONPath}
			capability = "review.panel_verdict"
			actionName = "record_panel_verdict_review"
			preview = "Record one human-review sidecar for panel verdict " + verdict.ID + "; memory authority remains unchanged."
			rollback = "Record a superseding human-review disposition; the core verdict artifact remains sealed."
			expectedReceipt = "Recorded human review for panel verdict " + verdict.ID + "."
		}
		if err := reviewartifact.SeedActionAuthority(action, reviewartifact.ActionAuthoritySeedSpec{
			WorkID: workID, CreatedAt: verdict.CreatedAt, ProducerName: "compound/internal/panel",
			Capability: capability, Action: actionName, Target: target,
			MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local",
			Preview: preview, RollbackPlan: rollback, Privacy: "private", Risk: "tier1",
			RunID: verdict.RunID, EvidenceRefs: evidenceRefs, TargetScope: "private",
			ExpectedReceipt: expectedReceipt,
			ExpectedProof:   append([]string{"panel-verdict:" + verdict.ID, "panel-run:" + verdict.RunID}, evidenceRefs...),
		}); err != nil {
			item.Warnings = reviewartifact.Unique(append(item.Warnings, "panel authority seed blocked: "+err.Error()))
		}
	}
}

func panelSeededGroupAction(action *reviewartifact.Action, group reviewartifact.QueueGroup, runID, actionName string) *reviewartifact.Action {
	if action == nil || group.Sample == nil {
		return action
	}
	workID := reviewartifact.AuthoritySeedWorkID(*group.Sample, "")
	createdAt := group.Sample.CreatedAt
	if workID == "" {
		homePath := homeFromReviewRoot(filepath.Dir(filepath.Dir(group.Sample.JSONPath)))
		if store, err := Open(homePath); err == nil {
			if run, err := store.LoadRun(runID); err == nil && run.WorkBound {
				workID = run.WorkID
				createdAt = reviewartifact.FirstNonEmpty(createdAt, run.CreatedAt)
			}
		}
	}
	if workID == "" {
		return action
	}
	capability := "review.panel_verdict"
	target := envelope.ActionTarget{Kind: "panel_verdict_group", ID: group.Key}
	preview := fmt.Sprintf("Record human-review sidecars for %d panel verdicts in group %s; memory authority remains unchanged.", group.Count, group.Key)
	rollback := "Record superseding human-review dispositions; core panel verdict artifacts remain sealed."
	receipt := fmt.Sprintf("Recorded %d panel verdict reviews for group %s.", group.Count, group.Key)
	if actionName == "apply_panel_run" {
		capability = "panel.adjudicate"
		target = envelope.ActionTarget{Kind: "panel_run", ID: runID}
		preview = fmt.Sprintf("Apply the already-reviewed verdicts in panel run %s; each target retains its own policy decision.", runID)
		rollback = "Use the target-specific inverse or superseding adjudication recorded by panel apply receipts."
		receipt = "Applied reviewed panel run " + runID + "."
	}
	evidenceRefs := reviewartifact.UniqueSorted(append(append([]string{}, group.EvidenceRefs...), "panel-run:"+runID))
	_ = reviewartifact.SeedActionAuthority(action, reviewartifact.ActionAuthoritySeedSpec{
		WorkID: workID, CreatedAt: createdAt, ProducerName: "compound/internal/panel",
		Capability: capability, Action: actionName, Target: target,
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local",
		Preview: preview, RollbackPlan: rollback, Privacy: "private", Risk: "tier1",
		RunID: runID, EvidenceRefs: evidenceRefs, TargetScope: "private",
		ExpectedReceipt: receipt,
		ExpectedProof:   append([]string{"panel-run:" + runID}, evidenceRefs...),
	})
	return action
}

func panelVerdictSeedWorkID(item reviewartifact.QueueItem, runID, homePath string) string {
	if workID := reviewartifact.AuthoritySeedWorkID(item, ""); workID != "" {
		return workID
	}
	store, err := Open(homePath)
	if err != nil {
		return ""
	}
	run, err := store.LoadRun(runID)
	if err != nil || !run.WorkBound {
		return ""
	}
	return strings.TrimSpace(run.WorkID)
}

// panelVerdictGroupRunID parses the run id out of a panel-verdicts group_key
// shaped "panel-verdicts|{run_id}|{decision}" (reviewartifact.OperatorCardForVerdict
// composes this key; the group_key is machine-facing and stable across a run).
func panelVerdictGroupRunID(groupKey string) (string, bool) {
	parts := strings.Split(groupKey, "|")
	if len(parts) != 3 || parts[0] != verdictReviewArea || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return parts[1], true
}

func panelVerdictGroupDecision(groupKey string) (string, bool) {
	parts := strings.Split(groupKey, "|")
	if len(parts) != 3 || parts[0] != verdictReviewArea || strings.TrimSpace(parts[2]) == "" {
		return "", false
	}
	return normalizePanelPosition(parts[2]), true
}

func panelDecisionRecordableByHuman(decision string) bool {
	switch normalizePanelPosition(decision) {
	case "needs_human", "needs_trial":
		return true
	default:
		return false
	}
}

func defaultPanelReviewedBy() string {
	for _, key := range []string{"COMPOUND_OPERATOR_ID", "COMPOUND_REVIEWED_BY"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "operator"
}

func panelShellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, panelShellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func panelShellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '/' || r == '.' || r == ':' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func buildPanelDecisionCard(item reviewartifact.QueueItem, verdict verdictArtifactPayload) *reviewartifact.DecisionCard {
	card := &reviewartifact.DecisionCard{
		Claim:               panelDecisionClaim(verdict),
		PanelLean:           panelLean(verdict.Synthesis),
		OptionalRationale:   true,
		CanonicalActionHint: "Record the human review, then run the selected canonical memory adjudication command. Renderers must not write review or memory state directly.",
		Boundary: []string{
			"read-only projection",
			"operator choice required before mutation",
			"no console-side state writes",
			"verdict-record plus memory adjudicate are the canonical mutation path",
		},
		Meta: map[string]string{
			"verdict_id":  verdict.ID,
			"run_id":      verdict.RunID,
			"target_kind": strings.TrimSpace(verdict.Target.Kind),
			"target_ref":  strings.TrimSpace(verdict.Target.Ref),
			"review_path": item.JSONPath,
		},
	}
	if rationale := panelVerdictPlainRationale(item.MarkdownPath, verdict.Synthesis.Rationale); rationale != "" {
		card.Why = append(card.Why, reviewartifact.DecisionCardReason{
			Label:  "panel rationale",
			Detail: rationale,
		})
	}
	for _, finding := range verdict.LensFindings {
		card.Why = append(card.Why, panelLensReasons(finding)...)
	}
	card.Choices = panelDecisionChoices(item.JSONPath, verdict.Target)
	return card
}

func panelDecisionClaim(verdict verdictArtifactPayload) string {
	var payload map[string]any
	if len(verdict.Target.Payload) > 0 && json.Unmarshal(verdict.Target.Payload, &payload) == nil {
		if object, ok := payload["object"].(string); ok && strings.TrimSpace(object) != "" {
			return oneLine(object)
		}
	}
	if s := strings.TrimSpace(verdict.Target.Summary); s != "" {
		return oneLine(s)
	}
	target := panelVerdictTarget(verdict.Target)
	if target == "" {
		return "A panel verdict needs an operator decision."
	}
	return "A panel verdict for this " + target + " needs an operator decision."
}

func panelLean(s Synthesis) string {
	decision := strings.TrimSpace(s.Decision)
	if decision == "" {
		decision = "unknown"
	}
	if s.Confidence > 0 {
		return fmt.Sprintf("%s (confidence %.2f)", decision, s.Confidence)
	}
	return decision
}

func panelVerdictPlainRationale(markdownPath, fallback string) string {
	if rationale := markdownSection(markdownPath, "## Rationale"); rationale != "" {
		return rationale
	}
	return oneLine(fallback)
}

func markdownSection(path, heading string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	inSection := false
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == heading {
			inSection = true
			continue
		}
		if inSection && strings.HasPrefix(trimmed, "## ") {
			break
		}
		if inSection && trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return oneLine(strings.Join(out, " "))
}

func panelLensReasons(finding LensFinding) []reviewartifact.DecisionCardReason {
	label, question := panelLensLabelQuestion(finding.LensRef)
	if len(finding.Findings) == 0 {
		if finding.Error == "" {
			return nil
		}
		return []reviewartifact.DecisionCardReason{{
			Label:    label,
			Question: question,
			Severity: "error",
			Detail:   oneLine(finding.Error),
		}}
	}
	out := make([]reviewartifact.DecisionCardReason, 0, len(finding.Findings))
	for _, f := range finding.Findings {
		out = append(out, reviewartifact.DecisionCardReason{
			Label:    label,
			Question: question,
			Severity: strings.TrimSpace(f.Severity),
			Detail:   oneLine(f.Claim),
		})
	}
	return out
}

func panelLensLabelQuestion(ref string) (string, string) {
	lower := strings.ToLower(ref)
	switch {
	case strings.Contains(lower, "evidence-strength"):
		return "evidence strength", "How well is this supported?"
	case strings.Contains(lower, "fossilization-risk"):
		return "fossilization risk", "Could this harden into a false permanent rule?"
	case strings.Contains(lower, "inversion-falsification"):
		return "inversion / falsification", "Could this be overstated backwards?"
	case strings.Contains(lower, "duplicate"):
		return "duplicate risk", "Do we already believe something like this?"
	case strings.Contains(lower, "contradiction"):
		return "contradiction risk", "Do we already believe something against this?"
	case strings.Contains(lower, "privacy"):
		return "privacy risk", "Anything sensitive?"
	default:
		label := strings.TrimPrefix(strings.TrimSpace(ref), "lens:")
		if label == "" {
			label = "panel lens"
		}
		return label, "What did this lens notice?"
	}
}

func panelDecisionChoices(reviewPath string, target TargetInput) []reviewartifact.DecisionCardChoice {
	if strings.TrimSpace(target.Kind) != "memory_candidate" || strings.TrimSpace(target.Ref) == "" {
		return nil
	}
	choices := []struct {
		id     string
		label  string
		effect string
	}{
		{"promote", "Promote", "Becomes ambient canon, recallable and citeable in future work."},
		{"park", "Park", "Kept as evidence but not ambient; can resurface when new evidence arrives."},
		{"reject", "Reject", "Discarded as noise for this memory lane."},
		{"no_promote", "No-promote", "Records that this was declined so echoes stop re-asking."},
	}
	out := make([]reviewartifact.DecisionCardChoice, 0, len(choices))
	for _, choice := range choices {
		cmd := panelChoiceCommand(reviewPath, target.Ref, choice.id)
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

// panelMemberCleanAction emits the one clean-argv (argv[0]=="compound", no shell)
// per-member action so governed renderers can execute it directly. The CLI requires
// --rationale; it is deliberately omitted here — the console appends
// ["--rationale", <operator text>] at execution time, after its own review gate.
func panelMemberCleanAction(reviewPath string) reviewartifact.Action {
	argv := []string{"compound", "panel", "verdict-record", "--review", reviewPath, "--reviewed-by", defaultPanelReviewedBy()}
	return reviewartifact.Action{
		ID:             "panel_verdict_record_member",
		Label:          "Record this verdict",
		Command:        "compound panel verdict-record --review " + shellQuote(reviewPath) + " --reviewed-by " + panelShellQuote(defaultPanelReviewedBy()) + " --rationale '<why>'",
		Argv:           argv,
		RequiresReview: true,
		PolicyNote:     "records the human review sidecar only; memory adjudication stays a separate explicit act",
	}
}

func panelChoiceCommand(reviewPath, candidateID, verdict string) string {
	reviewCmd := "compound panel verdict-record --review " + shellQuote(reviewPath) + " --rationale \"$RATIONALE\" --reviewed-by " + panelShellQuote(defaultPanelReviewedBy())
	adjudicateCmd := "compound memory adjudicate " + shellQuote(candidateID) + " --verdict " + shellQuote(verdict)
	return reviewCmd + " && " + adjudicateCmd
}

func panelDecisionCardActions(card *reviewartifact.DecisionCard) []reviewartifact.Action {
	if card == nil {
		return nil
	}
	out := make([]reviewartifact.Action, 0, len(card.Choices))
	for _, choice := range card.Choices {
		out = append(out, reviewartifact.Action{
			ID:             "decision:" + choice.ID,
			Label:          choice.Label,
			Command:        choice.Command,
			Argv:           choice.Argv,
			RequiresReview: choice.RequiresReview,
		})
	}
	return out
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

type verdictApplication struct {
	RunID       string
	Application Application
}

type memoryCandidatePanelTarget struct {
	State       string
	Annotations []mem.CandidateAnnotation
}

func memoryCandidateIDsFromQueueReport(report *reviewartifact.QueueReport) []string {
	if report == nil {
		return nil
	}
	ids := make([]string, 0)
	seen := make(map[string]struct{})
	for _, item := range report.Items {
		if item.Area != verdictReviewArea {
			continue
		}
		verdict, ok := verdictFromQueueItem(item)
		if !ok || strings.TrimSpace(verdict.Target.Kind) != "memory_candidate" {
			continue
		}
		id := strings.TrimSpace(verdict.Target.Ref)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func memoryCandidatePanelTargets(homePath string, ids []string) map[string]memoryCandidatePanelTarget {
	if strings.TrimSpace(homePath) == "" || len(ids) == 0 {
		return nil
	}
	store, err := mem.Open(homePath)
	if err != nil {
		return nil
	}
	candidates, err := store.CandidatesByID(ids)
	if err != nil {
		return nil
	}
	out := make(map[string]memoryCandidatePanelTarget, len(candidates))
	for _, candidate := range candidates {
		out[candidate.ID] = memoryCandidatePanelTarget{
			State:       candidate.State,
			Annotations: append([]mem.CandidateAnnotation(nil), candidate.Annotations...),
		}
	}
	return out
}

func verdictApplicationsByArtifact(reviewRoot string) map[string]verdictApplication {
	out := map[string]verdictApplication{}
	homePath := homeFromReviewRoot(reviewRoot)
	if homePath == "" {
		return out
	}
	runDir := filepath.Join(homePath, "panel-runs")
	entries, err := os.ReadDir(runDir)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(runDir, entry.Name()))
		if err != nil {
			continue
		}
		run, err := Decode(data)
		if err != nil {
			continue
		}
		for _, finding := range run.Findings {
			artifact := strings.TrimSpace(finding.VerdictArtifact)
			if artifact == "" {
				continue
			}
			app := verdictApplication{RunID: run.ID, Application: finding.Application}
			for _, key := range verdictArtifactKeys(artifact) {
				out[key] = app
			}
		}
	}
	return out
}

func homeFromReviewRoot(reviewRoot string) string {
	clean := filepath.Clean(strings.TrimSpace(reviewRoot))
	if clean == "." || filepath.Base(clean) != "reviews" {
		return ""
	}
	return filepath.Dir(clean)
}

func applicationForVerdictItem(item reviewartifact.QueueItem, applications map[string]verdictApplication) (verdictApplication, bool) {
	for _, key := range verdictArtifactKeys(item.JSONPath) {
		if app, ok := applications[key]; ok {
			return app, true
		}
	}
	if item.ID != "" {
		if app, ok := applications[item.ID]; ok {
			return app, true
		}
	}
	return verdictApplication{}, false
}

func panelVerdictReviewedPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".reviewed.json") {
			return path
		}
	}
	return ""
}

func verdictArtifactKeys(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	keys := []string{clean}
	if base != "." && base != clean {
		keys = append(keys, base)
	}
	if strings.HasSuffix(base, ".json") {
		keys = append(keys, strings.TrimSuffix(base, ".json"))
	}
	return keys
}

func terminalPanelApplication(app Application, decision string) bool {
	if !app.Applied {
		return false
	}
	mapped := strings.TrimSpace(app.MappedVerdict)
	if mapped == "" {
		mapped = normalizePanelPosition(decision)
	}
	return mapped != "needs_human" && mapped != "needs_trial"
}

func resolvedPanelVerdictTarget(homePath string, verdict verdictArtifactPayload, memoryTargets map[string]memoryCandidatePanelTarget) (string, bool) {
	if strings.TrimSpace(homePath) == "" {
		return "", false
	}
	kind := strings.TrimSpace(verdict.Target.Kind)
	ref := strings.TrimSpace(verdict.Target.Ref)
	if kind == "memory_candidate" {
		return resolvedMemoryCandidatePanelTarget(ref, memoryTargets)
	}
	if kind != "comparison" || ref == "" {
		return "", false
	}
	path := comparisonCandidatePath(homePath, ref)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	candidate, err := decodeComparisonCandidate(data, path)
	if err != nil || candidate.State != comparisonStateJudged {
		return "", false
	}
	target := panelVerdictTarget(verdict.Target)
	verdictText := reviewartifact.FirstNonEmpty(candidate.Verdict, "unknown")
	source := strings.TrimSpace(candidate.VerdictArtifact)
	if source == "" {
		return fmt.Sprintf("comparison target already judged; no panel action needed (target=%s verdict=%s)", target, verdictText), true
	}
	return fmt.Sprintf("comparison target already judged; no panel action needed (target=%s verdict=%s source=%s)", target, verdictText, filepath.Base(source)), true
}

func resolvedMemoryCandidatePanelTarget(candidateID string, memoryTargets map[string]memoryCandidatePanelTarget) (string, bool) {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return "", false
	}
	item, ok := memoryTargets[candidateID]
	if !ok {
		return "", false
	}
	state := strings.TrimSpace(item.State)
	if state == "" || state == "proposed" {
		return "", false
	}
	if len(item.Annotations) == 0 {
		return "", false
	}
	return fmt.Sprintf("memory candidate already %s with %d annotation(s); panel verdict no longer open review debt (target=memory_candidate/%s)", state, len(item.Annotations), candidateID), true
}

func verdictFromQueueItem(item reviewartifact.QueueItem) (verdictArtifactPayload, bool) {
	if strings.TrimSpace(item.JSONPath) == "" {
		return verdictArtifactPayload{}, false
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return verdictArtifactPayload{}, false
	}
	var verdict verdictArtifactPayload
	if err := json.Unmarshal(data, &verdict); err != nil {
		return verdictArtifactPayload{}, false
	}
	return verdict, strings.TrimSpace(verdict.ID) != ""
}

func panelVerdictTarget(target TargetInput) string {
	kind := strings.TrimSpace(target.Kind)
	ref := strings.TrimSpace(target.Ref)
	switch {
	case kind != "" && ref != "":
		return kind + "/" + ref
	case kind != "":
		return kind
	case ref != "":
		return ref
	default:
		return "unknown"
	}
}

func failedLensCount(findings []LensFinding) int {
	count := 0
	for _, finding := range findings {
		if strings.TrimSpace(finding.Status) == "failed" {
			count++
		}
	}
	return count
}
