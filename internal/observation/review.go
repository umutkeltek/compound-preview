// review.go is a deliberate plan+annotate SUBSET of the panel-verdict review flow: it builds a
// review plan and annotates the queue, then hands off to `compound panel plan`. It has no
// Load/Validate/Apply loop and a different domain model (Batch/Observation/RouteDecision/LensStack),
// so it intentionally does NOT use internal/verdictflow (which owns the shared
// plan->record->render->load->annotate scaffolding for the product/sourceobs/capgraph verdict bridges).

package observation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/panel"
	"compound/internal/reviewartifact"
)

const (
	ObservationReviewArea              = "observation-candidates"
	ObservationCandidateSchemaVersion  = "compound.observation_candidate.v0.1"
	ObservationReviewPlanSchemaVersion = "compound.observation_review_plan.v0.1"
)

type CandidateReviewResult struct {
	ObservationID  string   `json:"observation_id"`
	CandidateID    string   `json:"candidate_id"`
	PlanPath       string   `json:"plan_path"`
	ReviewPath     string   `json:"review_path"`
	ReviewYAMLPath string   `json:"review_yaml_path"`
	LensRefs       []string `json:"lens_refs,omitempty"`
}

type ObservationCandidateRecord struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Subject       string   `json:"subject"`
	Statement     string   `json:"statement"`
	Detail        string   `json:"detail"`
	Confidence    float64  `json:"confidence"`
	State         string   `json:"state"`
	EvidenceRefs  []string `json:"evidence_refs"`
	CreatedAt     string   `json:"created_at"`

	ObservationID   string `json:"observation_id"`
	ObservationKind string `json:"observation_kind"`
	SourceKind      string `json:"source_kind"`
	SourceRef       string `json:"source_ref"`
	SubjectKind     string `json:"subject_kind"`
	SubjectID       string `json:"subject_id"`
	Domain          string `json:"domain,omitempty"`
	Task            string `json:"task,omitempty"`
}

type ObservationReviewPlan struct {
	SchemaVersion   string                 `json:"schema_version"`
	ID              string                 `json:"id"`
	CreatedAt       string                 `json:"created_at"`
	Status          string                 `json:"status"`
	Readiness       string                 `json:"readiness"`
	NextAction      string                 `json:"next_action"`
	Owner           string                 `json:"owner"`
	SourceKind      string                 `json:"source_kind"`
	SourcePath      string                 `json:"source_path,omitempty"`
	ObservationID   string                 `json:"observation_id"`
	ObservationKind string                 `json:"observation_kind"`
	CandidateID     string                 `json:"candidate_id"`
	CandidateKind   string                 `json:"candidate_kind"`
	Route           RouteDecision          `json:"route"`
	Source          Source                 `json:"source"`
	Subject         Subject                `json:"subject"`
	LensStack       ObservationLensStack   `json:"lens_stack"`
	Boundaries      []string               `json:"boundaries"`
	EvidenceRefs    []string               `json:"evidence_refs"`
	ReviewDraft     ObservationReviewDraft `json:"review_draft"`
}

type ObservationLensStack struct {
	TargetKind         string   `json:"target_kind"`
	TargetRef          string   `json:"target_ref"`
	Domain             string   `json:"domain,omitempty"`
	Task               string   `json:"task,omitempty"`
	IncludedLensRefs   []string `json:"included_lens_refs,omitempty"`
	MissingDomainLens  []string `json:"missing_domain_lens,omitempty"`
	SelectionError     string   `json:"selection_error,omitempty"`
	PanelLensesCommand []string `json:"panel_lenses_command"`
	PanelPlanCommand   []string `json:"panel_plan_command"`
}

type ObservationReviewDraft struct {
	SchemaVersion    string   `json:"schema_version"`
	ReviewID         string   `json:"review_id"`
	CandidateID      string   `json:"candidate_id"`
	ObservationID    string   `json:"observation_id"`
	AllowedDecisions []string `json:"allowed_decisions"`
	ReviewCommand    string   `json:"review_command"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewNote       string   `json:"review_note"`
}

func RecordCandidateReviewPlan(homePath string, batch Batch, obs Observation, decision RouteDecision, sourcePath string, now time.Time) (CandidateReviewResult, error) {
	if strings.TrimSpace(homePath) == "" {
		return CandidateReviewResult{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if decision.Route != RouteCandidateOnly {
		return CandidateReviewResult{}, fmt.Errorf("observation %q route %q is not candidate-only", decision.ObservationID, decision.Route)
	}
	obsID := observationID(batch, obs)
	domain := observationReviewDomain(batch, obs)
	task := observationReviewTask(batch, obs)
	candidateID := stableID("ocand_", batch.Source.Kind, batch.Source.Ref, batch.Subject.Kind, batch.Subject.ID, obsID)
	evidenceRefs := reviewartifact.UniqueSorted(append(decision.EvidenceRefs, "route:"+decision.Route, "owner:"+decision.Owner))
	createdAt := now.UTC().Format(time.RFC3339)
	candidate := ObservationCandidateRecord{
		SchemaVersion:   ObservationCandidateSchemaVersion,
		ID:              candidateID,
		Kind:            string(candidates.CapabilityObs),
		Subject:         "observation:" + firstNonEmpty(batch.Subject.ID, batch.Subject.Kind, batch.Source.Kind),
		Statement:       observationCandidateStatement(batch, obs),
		Detail:          observationCandidateDetail(batch, obs, domain, task),
		Confidence:      observationConfidence(obs),
		State:           "needs_review",
		EvidenceRefs:    evidenceRefs,
		CreatedAt:       createdAt,
		ObservationID:   obsID,
		ObservationKind: strings.TrimSpace(obs.Kind),
		SourceKind:      strings.TrimSpace(batch.Source.Kind),
		SourceRef:       strings.TrimSpace(batch.Source.Ref),
		SubjectKind:     strings.TrimSpace(batch.Subject.Kind),
		SubjectID:       strings.TrimSpace(batch.Subject.ID),
		Domain:          domain,
		Task:            task,
	}
	if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, candidateID, candidate); err != nil {
		return CandidateReviewResult{}, err
	}
	lensStack := observationLensStack(homePath, candidateID, domain, task)
	planID := stableID("orev_", candidateID, obsID, domain, task)
	plan := ObservationReviewPlan{
		SchemaVersion:   ObservationReviewPlanSchemaVersion,
		ID:              planID,
		CreatedAt:       createdAt,
		Status:          "review_required",
		Readiness:       "needs_review",
		NextAction:      "plan contextual lens review for observation candidate",
		Owner:           "internal/observation",
		SourceKind:      "observation_batch",
		SourcePath:      sourcePath,
		ObservationID:   obsID,
		ObservationKind: strings.TrimSpace(obs.Kind),
		CandidateID:     candidateID,
		CandidateKind:   string(candidates.CapabilityObs),
		Route:           decision,
		Source:          batch.Source,
		Subject:         batch.Subject,
		LensStack:       lensStack,
		Boundaries:      observationReviewBoundaries(),
		EvidenceRefs:    evidenceRefs,
		ReviewDraft: ObservationReviewDraft{
			SchemaVersion:    "compound.observation_candidate_review.v0.1",
			ReviewID:         planID,
			CandidateID:      candidateID,
			ObservationID:    obsID,
			AllowedDecisions: []string{"plan_lens_review", "park", "reject"},
			ReviewCommand:    "compound panel plan --target-kind capability_observation --target " + candidateID,
			RequiredEvidence: []string{
				"observation route remains candidate_only",
				"candidate should not be promoted as truth",
				"contextual lenses fit domain/task before synthesis",
			},
			ReviewNote: "Records review material only. It does not run a panel, promote memory, mutate product ledgers, post publicly, or call external accounts.",
		},
	}
	paths, err := reviewartifact.Write(homePath, ObservationReviewArea, planID, plan, " ", RenderObservationReviewPlan(plan))
	if err != nil {
		return CandidateReviewResult{}, err
	}
	reviewYAMLPath, err := reviewartifact.WriteRaw(homePath, ObservationReviewArea, planID+".review.yaml", []byte(RenderObservationReviewYAML(plan)))
	if err != nil {
		return CandidateReviewResult{}, err
	}
	return CandidateReviewResult{
		ObservationID:  obsID,
		CandidateID:    candidateID,
		PlanPath:       paths.PlanPath,
		ReviewPath:     paths.ReviewPath,
		ReviewYAMLPath: reviewYAMLPath,
		LensRefs:       lensStack.IncludedLensRefs,
	}, nil
}

func AnnotateReviewQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != ObservationReviewArea {
			continue
		}
		annotateObservationReviewQueueItem(item)
	}
}

func annotateObservationReviewQueueItem(item *reviewartifact.QueueItem) {
	plan, ok := observationReviewPlanFromQueueItem(*item)
	if !ok {
		item.Status = "blocked"
		item.Readiness = "blocked"
		item.BlockedReason = "observation review artifact unreadable"
		item.NextAction = "regenerate observation candidate review plan"
		item.Actions = nil
		return
	}
	if verdict := observationReviewCandidateVerdict(*item, plan); verdict != "" {
		item.Status = "resolved"
		item.Readiness = "ok"
		item.Owner = reviewartifact.FirstNonEmpty(item.Owner, "internal/observation")
		item.SourceKind = reviewartifact.FirstNonEmpty(item.SourceKind, "observation_batch")
		item.CandidateID = reviewartifact.FirstNonEmpty(item.CandidateID, plan.CandidateID)
		item.NextAction = "observation candidate already consumed by capability observation verdict " + verdict
		item.Boundaries = reviewartifact.Unique(append(item.Boundaries, plan.Boundaries...))
		item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, plan.EvidenceRefs...))
		item.DecisionCard = nil
		item.Actions = nil
		annotateObservationOperatorCard(item, plan, reviewartifact.BandFiled)
		return
	}
	item.Status = "review_required"
	item.Readiness = "needs_review"
	item.Owner = reviewartifact.FirstNonEmpty(item.Owner, "internal/observation")
	item.SourceKind = reviewartifact.FirstNonEmpty(item.SourceKind, "observation_batch")
	item.CandidateID = reviewartifact.FirstNonEmpty(item.CandidateID, plan.CandidateID)
	item.NextAction = "plan contextual lens review for observation candidate"
	item.Boundaries = reviewartifact.Unique(append(item.Boundaries, plan.Boundaries...))
	item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, plan.EvidenceRefs...))
	card := observationReviewDecisionCard(plan)
	item.DecisionCard = card
	item.Actions = []reviewartifact.Action{observationReviewPlanAction(plan)}
	annotateObservationOperatorCard(item, plan, reviewartifact.BandNeedsYou)
}

func annotateObservationOperatorCard(item *reviewartifact.QueueItem, plan ObservationReviewPlan, band string) {
	card, err := OperatorCardForReviewPlan(plan)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "observation review operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "observation review operator card invalid: "+err.Error())
	}
}

func observationReviewCandidateVerdict(item reviewartifact.QueueItem, plan ObservationReviewPlan) string {
	homePath := observationReviewHomeFromPath(item.JSONPath)
	if homePath == "" || strings.TrimSpace(plan.CandidateID) == "" {
		return ""
	}
	var candidate struct {
		Verdict string `json:"verdict"`
	}
	if err := candidates.ReadCandidate(homePath, candidates.CapabilityObs, plan.CandidateID, &candidate); err != nil {
		return ""
	}
	switch verdict := strings.TrimSpace(candidate.Verdict); verdict {
	case string(candidates.Park), string(candidates.Reject), string(candidates.Operationalize), "promote", "merge_existing":
		return verdict
	default:
		return ""
	}
}

func observationReviewHomeFromPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	areaDir := filepath.Dir(path)
	reviewsDir := filepath.Dir(areaDir)
	if filepath.Base(reviewsDir) != "reviews" {
		return ""
	}
	return filepath.Dir(reviewsDir)
}

func observationReviewDecisionCard(plan ObservationReviewPlan) *reviewartifact.DecisionCard {
	command := observationPanelPlanCommand(plan)
	return &reviewartifact.DecisionCard{
		Claim: "Observation " + plan.ObservationID + " is reviewable contextual lens material, not truth.",
		Why: []reviewartifact.DecisionCardReason{
			{Label: "route", Question: "Why is this not applied directly?", Detail: plan.Route.Route + " / " + plan.Route.Reason},
			{Label: "lens stack", Question: "Which contextual lenses should see it?", Detail: strings.Join(plan.LensStack.IncludedLensRefs, ", ")},
			{Label: "subject", Question: "What loop or surface may consume it later?", Detail: plan.Subject.Kind + ":" + plan.Subject.ID},
			{Label: "boundary", Severity: "warning", Question: "What must not happen here?", Detail: strings.Join(plan.Boundaries, "; ")},
		},
		Choices: []reviewartifact.DecisionCardChoice{{
			ID:             "plan_lens_review",
			Label:          "Plan Lens Review",
			Effect:         "Creates a panel plan over the capability-observation candidate; does not promote truth or call external accounts.",
			Command:        command,
			Argv:           plan.LensStack.PanelPlanCommand,
			RequiresReview: true,
		}},
		PanelLean:           "needs_human",
		OptionalRationale:   true,
		CanonicalActionHint: "Use the panel plan command; review artifacts and renderers must not promote or apply observation meaning directly.",
		Boundary:            plan.Boundaries,
		Meta: map[string]string{
			"candidate_id":   plan.CandidateID,
			"observation_id": plan.ObservationID,
			"target_kind":    plan.LensStack.TargetKind,
			"domain":         plan.LensStack.Domain,
			"task":           plan.LensStack.Task,
			"review_area":    ObservationReviewArea,
			"schema_version": plan.SchemaVersion,
		},
	}
}

func observationReviewPlanAction(plan ObservationReviewPlan) reviewartifact.Action {
	return reviewartifact.Action{
		ID:             "plan_observation_lens_review",
		Label:          "Plan observation lens review",
		Command:        observationPanelPlanCommand(plan),
		Argv:           plan.LensStack.PanelPlanCommand,
		RequiresReview: true,
	}
}

func observationReviewPlanFromQueueItem(item reviewartifact.QueueItem) (ObservationReviewPlan, bool) {
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return ObservationReviewPlan{}, false
	}
	var plan ObservationReviewPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ObservationReviewPlan{}, false
	}
	return plan, strings.TrimSpace(plan.SchemaVersion) == ObservationReviewPlanSchemaVersion
}

func observationLensStack(homePath, candidateID, domain, task string) ObservationLensStack {
	stack := ObservationLensStack{
		TargetKind:         string(candidates.CapabilityObs),
		TargetRef:          candidateID,
		Domain:             domain,
		Task:               task,
		PanelLensesCommand: []string{"compound", "panel", "lenses", "--target-kind", string(candidates.CapabilityObs), "--domain", domain, "--task", task, "--json"},
		PanelPlanCommand:   []string{"compound", "panel", "plan", "--target-kind", string(candidates.CapabilityObs), "--target", candidateID, "--domain", domain, "--task", task, "--json"},
	}
	selection, err := panel.SelectLensesWithContext(homePath, string(candidates.CapabilityObs), panel.LensContext{Domain: domain, Task: task})
	if err != nil {
		stack.SelectionError = err.Error()
		return stack
	}
	for _, ref := range selection.IncludedRefs {
		stack.IncludedLensRefs = append(stack.IncludedLensRefs, ref.Ref)
	}
	stack.MissingDomainLens = append(stack.MissingDomainLens, selection.MissingDomainLens...)
	return stack
}

func RenderObservationReviewPlan(plan ObservationReviewPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Observation Candidate Review\n\n")
	fmt.Fprintf(&b, "- observation: `%s`\n", plan.ObservationID)
	fmt.Fprintf(&b, "- candidate: `%s`\n", plan.CandidateID)
	fmt.Fprintf(&b, "- route: `%s`\n", plan.Route.Route)
	fmt.Fprintf(&b, "- domain: `%s`\n", plan.LensStack.Domain)
	fmt.Fprintf(&b, "- task: `%s`\n\n", plan.LensStack.Task)
	fmt.Fprintln(&b, "## Lens Material")
	fmt.Fprintln(&b)
	if len(plan.LensStack.IncludedLensRefs) == 0 {
		fmt.Fprintln(&b, "- no reviewed lenses currently selected")
	} else {
		for _, ref := range plan.LensStack.IncludedLensRefs {
			fmt.Fprintf(&b, "- `%s`\n", ref)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Boundary")
	fmt.Fprintln(&b)
	for _, boundary := range plan.Boundaries {
		fmt.Fprintf(&b, "- %s\n", boundary)
	}
	return b.String()
}

func RenderObservationReviewYAML(plan ObservationReviewPlan) string {
	return fmt.Sprintf("schema_version: compound.observation_candidate_review.v0.1\nreview_id: %s\ncandidate_id: %s\nobservation_id: %s\ndecision: TODO_decision_plan_lens_review_park_reject\nrationale: TODO_rationale\nconfidence: TODO_confidence_0_to_1\n", plan.ID, plan.CandidateID, plan.ObservationID)
}

func observationCandidateStatement(batch Batch, obs Observation) string {
	title := strings.TrimSpace(obs.Title)
	if title != "" {
		return "External " + strings.TrimSpace(obs.Kind) + " observation needs contextual review: " + title
	}
	return "External " + strings.TrimSpace(obs.Kind) + " observation needs contextual review before becoming signal, hypothesis, memory, or action material."
}

func observationCandidateDetail(batch Batch, obs Observation, domain, task string) string {
	parts := []string{
		"source_kind=" + strings.TrimSpace(batch.Source.Kind),
		"subject_kind=" + strings.TrimSpace(batch.Subject.Kind),
		"subject_id=" + strings.TrimSpace(batch.Subject.ID),
		"observation_kind=" + strings.TrimSpace(obs.Kind),
		"domain=" + domain,
	}
	if task != "" {
		parts = append(parts, "task="+task)
	}
	return strings.Join(parts, " ")
}

func observationReviewDomain(batch Batch, obs Observation) string {
	kind := normalize(obs.Kind)
	switch kind {
	case "distribution_feedback", "social_feedback", "comment":
		return "social-distribution"
	case "support_message", "message":
		return "support-feedback"
	default:
		return firstNonEmpty(batch.Subject.Channel, batch.Subject.Surface, batch.Subject.Kind, "observation")
	}
}

func observationReviewTask(batch Batch, obs Observation) string {
	base := strings.ReplaceAll(normalize(obs.Kind), "_", " ")
	title := strings.TrimSpace(obs.Title)
	if title != "" {
		return base + " " + title
	}
	return strings.TrimSpace(base + " " + batch.Subject.Kind + " feedback")
}

func observationConfidence(obs Observation) float64 {
	if obs.Confidence == 0 {
		return 0.5
	}
	return obs.Confidence
}

func observationReviewBoundaries() []string {
	return []string{
		"review material only",
		"no truth promotion",
		"no memory adjudication",
		"no product ledger mutation",
		"no public posting or external account call",
		"no connector-specific core authority",
	}
}

func observationPanelPlanCommand(plan ObservationReviewPlan) string {
	return joinShell(plan.LensStack.PanelPlanCommand)
}

func joinShell(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, shellArg(arg))
	}
	return strings.Join(parts, " ")
}

func shellArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return r == '\'' || r == '"' || r == '\\' || r == '$' || r == '`' || r == ' ' || r == '\t' || r == '\n'
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}
