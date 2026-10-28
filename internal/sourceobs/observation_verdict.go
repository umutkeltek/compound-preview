package sourceobs

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
	"compound/internal/verdictflow"
	"compound/internal/writeback"
	"gopkg.in/yaml.v3"
)

const (
	SourceObservationVerdictPlanSchemaVersion   = "compound.source_observation_verdict_plan.v0.1"
	SourceObservationVerdictReviewSchemaVersion = "compound.source_observation_verdict_review.v0.1"
	SourceObservationVerdictReportSchemaVersion = "compound.source_observation_verdict_review_report.v0.1"
	SourceObservationVerdictReviewArea          = "source-observation-verdicts"
)

type SourceObservationVerdictPlan struct {
	SchemaVersion       string                        `json:"schema_version"`
	ID                  string                        `json:"id"`
	CreatedAt           string                        `json:"created_at"`
	Status              string                        `json:"status"`
	OwnerStatus         string                        `json:"owner_status"`
	OwnerKind           string                        `json:"owner_kind"`
	OwnerRef            string                        `json:"owner_ref"`
	VerdictID           string                        `json:"verdict_id"`
	RunID               string                        `json:"run_id"`
	TargetKind          string                        `json:"target_kind"`
	TargetRef           string                        `json:"target_ref"`
	PanelDecision       string                        `json:"panel_decision"`
	PanelRationale      string                        `json:"panel_rationale,omitempty"`
	SourceObservationID string                        `json:"source_observation_id,omitempty"`
	SourceRef           string                        `json:"source_ref,omitempty"`
	Subject             string                        `json:"subject,omitempty"`
	Statement           string                        `json:"statement,omitempty"`
	Detail              string                        `json:"detail,omitempty"`
	NextAction          string                        `json:"next_action"`
	Readiness           string                        `json:"readiness"`
	Owner               string                        `json:"owner"`
	CandidateID         string                        `json:"candidate_id"`
	Boundaries          []string                      `json:"boundaries"`
	EvidenceRefs        []string                      `json:"evidence_refs"`
	TargetPath          string                        `json:"target_path,omitempty"`
	TargetExists        bool                          `json:"target_exists"`
	TargetHash          string                        `json:"target_hash,omitempty"`
	ReviewDraft         SourceObservationVerdictDraft `json:"review_draft"`
	ReviewID            string                        `json:"review_id,omitempty"`
	ReviewDecision      string                        `json:"review_decision,omitempty"`
	ReviewedAt          string                        `json:"reviewed_at,omitempty"`
	NoPromoteID         string                        `json:"no_promote_id,omitempty"`
}

type SourceObservationVerdictDraft struct {
	ReviewID         string   `json:"review_id"`
	AllowedDecisions []string `json:"allowed_decisions"`
	ReviewCommand    string   `json:"review_command"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewNote       string   `json:"review_note"`
}

type SourceObservationVerdictPlanPaths struct {
	PlanPath       string `json:"plan_path"`
	ReviewPath     string `json:"review_path"`
	ReviewYAMLPath string `json:"review_yaml_path"`
}

type SourceObservationVerdictReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	VerdictID     string   `json:"verdict_id" yaml:"verdict_id"`
	CandidateRef  string   `json:"candidate_ref" yaml:"candidate_ref"`
	Decision      string   `json:"decision" yaml:"decision"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	AppliedAt     string   `json:"applied_at,omitempty" yaml:"applied_at,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type SourceObservationVerdictReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	VerdictID     string   `json:"verdict_id"`
	PlanID        string   `json:"plan_id"`
	ReviewID      string   `json:"review_id"`
	Decision      string   `json:"decision"`
	OwnerKind     string   `json:"owner_kind"`
	OwnerRef      string   `json:"owner_ref"`
	NoPromoteID   string   `json:"no_promote_id"`
	EvidenceRefs  []string `json:"evidence_refs"`
	ReviewPath    string   `json:"review_path,omitempty"`
}

type sourceObservationVerdictCandidatePayload struct {
	ID                  string   `json:"id"`
	Kind                string   `json:"kind"`
	Subject             string   `json:"subject"`
	Statement           string   `json:"statement"`
	Detail              string   `json:"detail"`
	Confidence          float64  `json:"confidence"`
	State               string   `json:"state,omitempty"`
	Verdict             string   `json:"verdict,omitempty"`
	EvidenceRefs        []string `json:"evidence_refs,omitempty"`
	SourceObservationID string   `json:"source_observation_id,omitempty"`
	SourceRef           string   `json:"source_ref,omitempty"`
	PromoteAllowed      bool     `json:"promote_allowed,omitempty"`
	ObservationID       string   `json:"observation_id,omitempty"`
	ObservationKind     string   `json:"observation_kind,omitempty"`
	SourceKind          string   `json:"source_kind,omitempty"`
	SubjectKind         string   `json:"subject_kind,omitempty"`
	SubjectID           string   `json:"subject_id,omitempty"`
	Domain              string   `json:"domain,omitempty"`
	Task                string   `json:"task,omitempty"`
}

func BuildSourceObservationVerdictPlan(homePath, verdictPath, targetPath string, now time.Time) (SourceObservationVerdictPlan, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceObservationVerdictPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	verdictPath = strings.TrimSpace(verdictPath)
	if verdictPath == "" {
		return SourceObservationVerdictPlan{}, fmt.Errorf("panel verdict path required")
	}
	verdict, err := panel.LoadVerdictArtifact(verdictPath)
	if err != nil {
		return SourceObservationVerdictPlan{}, err
	}
	if strings.TrimSpace(verdict.Target.Kind) != string(candidates.CapabilityObs) {
		return SourceObservationVerdictPlan{}, fmt.Errorf("source observation verdict plan requires target_kind %q, got %q", candidates.CapabilityObs, verdict.Target.Kind)
	}
	var payload sourceObservationVerdictCandidatePayload
	if len(verdict.Target.Payload) > 0 {
		if err := json.Unmarshal(verdict.Target.Payload, &payload); err != nil {
			return SourceObservationVerdictPlan{}, fmt.Errorf("parse capability observation payload in verdict %s: %w", verdict.ID, err)
		}
	}
	if payload.ID == "" {
		payload.ID = strings.TrimSpace(verdict.Target.Ref)
	}
	if sourceObservationVerdictProductSubject(payload) {
		return SourceObservationVerdictPlan{}, fmt.Errorf("panel verdict %s is product-subject observation candidate; use compound product observation-verdict-plan", verdict.ID)
	}
	if now.IsZero() {
		now = time.Now()
	}
	targetExists, targetHash, err := sourceObservationVerdictHashTarget(targetPath)
	if err != nil {
		return SourceObservationVerdictPlan{}, err
	}
	ownerKind, ownerRef, ownerStatus := sourceObservationVerdictOwner(payload, verdict.Target.Ref)
	evidenceRefs := append([]string{}, verdict.EvidenceRefs...)
	evidenceRefs = append(evidenceRefs, payload.EvidenceRefs...)
	evidenceRefs = reviewartifact.UniqueSorted(verdictflow.Compact(append(evidenceRefs,
		"panel_verdict:"+verdict.ID,
		"capability_observation:"+payload.ID,
		verdictPath,
	)))
	id := idFor("sovp_", verdict.ID, payload.ID, ownerKind, ownerRef)
	reviewID := "review_" + strings.TrimPrefix(id, "sovp_")
	nextAction := "decide reviewed source observation verdict stays no-promote evidence"
	if ownerStatus == "unsupported_no_owner" {
		nextAction = "park unsupported observation verdict evidence with explicit no-owner status"
	}
	boundaries := []string{
		"does not apply panel verdicts",
		"does not promote memory or source truth",
		"does not mutate ProductSignal ledger",
		"does not call external accounts, post, reply, spend, or publish",
		"does not create generic observation truth authority",
	}
	return SourceObservationVerdictPlan{
		SchemaVersion:       SourceObservationVerdictPlanSchemaVersion,
		ID:                  id,
		CreatedAt:           rfc3339(now),
		Status:              "review_required",
		OwnerStatus:         ownerStatus,
		OwnerKind:           ownerKind,
		OwnerRef:            ownerRef,
		VerdictID:           verdict.ID,
		RunID:               verdict.RunID,
		TargetKind:          verdict.Target.Kind,
		TargetRef:           verdict.Target.Ref,
		PanelDecision:       verdict.Synthesis.Decision,
		PanelRationale:      verdict.Synthesis.Rationale,
		SourceObservationID: payload.SourceObservationID,
		SourceRef:           payload.SourceRef,
		Subject:             payload.Subject,
		Statement:           payload.Statement,
		Detail:              payload.Detail,
		NextAction:          nextAction,
		Readiness:           "needs_review",
		Owner:               "internal/sourceobs",
		CandidateID:         verdict.Target.Ref,
		Boundaries:          boundaries,
		EvidenceRefs:        evidenceRefs,
		TargetPath:          strings.TrimSpace(targetPath),
		TargetExists:        targetExists,
		TargetHash:          targetHash,
		ReviewDraft: SourceObservationVerdictDraft{
			ReviewID:         reviewID,
			AllowedDecisions: []string{"record_no_promote", "park", "reject"},
			ReviewCommand:    "compound source observation-verdict-review <review.yaml>",
			RequiredEvidence: []string{
				"panel verdict target is a capability_observation candidate",
				"product-subject verdicts are routed to the product owner",
				"source verdict is evidence/no-promote material, not truth",
			},
			ReviewNote: "Records source-owned no-promote evidence only. It does not apply panel verdicts, promote memory, mutate ProductSignal, post publicly, or call external accounts.",
		},
	}, nil
}

func RecordSourceObservationVerdictPlan(homePath string, plan SourceObservationVerdictPlan) (SourceObservationVerdictPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceObservationVerdictPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(plan.ID) == "" {
		return SourceObservationVerdictPlanPaths{}, fmt.Errorf("source observation verdict plan id required")
	}
	arts, err := verdictflow.RecordPlan(homePath, SourceObservationVerdictReviewArea, plan.ID, " ", plan, renderSourceObservationVerdictPlanMarkdown(plan), RenderSourceObservationVerdictReviewYAML(plan))
	return SourceObservationVerdictPlanPaths{
		PlanPath:       arts.PlanPath,
		ReviewPath:     arts.ReviewPath,
		ReviewYAMLPath: arts.ReviewYAMLPath,
	}, err
}

func RenderSourceObservationVerdictReviewYAML(plan SourceObservationVerdictPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", SourceObservationVerdictReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintf(&b, "verdict_id: %s\n", plan.VerdictID)
	fmt.Fprintf(&b, "candidate_ref: %s\n", plan.TargetRef)
	fmt.Fprintln(&b, "decision: TODO_decision_record_no_promote_park_reject")
	fmt.Fprintln(&b, "rationale: TODO_rationale")
	fmt.Fprintln(&b, "confidence: TODO_confidence_0_to_1")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, " - %q\n", ref)
	}
	if len(plan.EvidenceRefs) == 0 {
		fmt.Fprintln(&b, " - TODO_evidence_ref")
	}
	return b.String()
}

func LoadSourceObservationVerdictReview(path string) (SourceObservationVerdictReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SourceObservationVerdictReview{}, err
	}
	var review SourceObservationVerdictReview
	if err := yaml.Unmarshal(data, &review); err != nil {
		return SourceObservationVerdictReview{}, fmt.Errorf("parse source observation verdict review %s: %w", path, err)
	}
	return review, nil
}

func ApplySourceObservationVerdictReview(homePath string, review SourceObservationVerdictReview, sourcePath string, now time.Time) (SourceObservationVerdictReviewReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceObservationVerdictReviewReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if err := ValidateSourceObservationVerdictReview(review); err != nil {
		return SourceObservationVerdictReviewReport{SchemaVersion: SourceObservationVerdictReportSchemaVersion, VerdictID: review.VerdictID, ReviewID: review.ID, Status: "fail"}, err
	}
	plan, err := findSourceObservationVerdictPlanForReview(homePath, review, sourcePath)
	if err != nil {
		return SourceObservationVerdictReviewReport{}, err
	}
	if plan.VerdictID != review.VerdictID || plan.TargetRef != review.CandidateRef {
		return SourceObservationVerdictReviewReport{}, fmt.Errorf("source observation verdict review does not match plan")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = rfc3339(now)
	}
	review.AppliedAt = rfc3339(now)
	review.SourceRef = "source-observation-verdict-review:" + filepath.Base(sourcePath)
	evidenceRefs := append([]string{}, plan.EvidenceRefs...)
	evidenceRefs = append(evidenceRefs, review.EvidenceRefs...)
	evidenceRefs = reviewartifact.UniqueSorted(verdictflow.Compact(append(evidenceRefs,
		"source_observation_verdict_review:"+review.ID,
		"panel_verdict:"+plan.VerdictID,
	)))
	wb, err := writeback.Open(homePath)
	if err != nil {
		return SourceObservationVerdictReviewReport{}, err
	}
	nop, _, err := wb.RecordNoPromote(writeback.NoPromoteDecision{
		SchemaVersion: writeback.NoPromoteSchemaVersion,
		CreatedAt:     rfc3339(now),
		CandidateRef:  review.CandidateRef,
		CandidateKind: plan.TargetKind,
		OwnerTarget:   writeback.OwnerTarget{Kind: plan.OwnerKind, Ref: plan.OwnerRef},
		Basis:         "source observation panel verdict review: " + review.Decision,
		EvidenceRefs:  evidenceRefs,
		Status:        writeback.NoPromoteActive,
	}, now)
	if err != nil {
		return SourceObservationVerdictReviewReport{}, err
	}
	report := SourceObservationVerdictReviewReport{
		SchemaVersion: SourceObservationVerdictReportSchemaVersion,
		ID:            idFor("sovr_", review.ID, review.Decision, nop.ID),
		CreatedAt:     rfc3339(now),
		Status:        "recorded",
		VerdictID:     review.VerdictID,
		PlanID:        review.PlanID,
		ReviewID:      review.ID,
		Decision:      review.Decision,
		OwnerKind:     plan.OwnerKind,
		OwnerRef:      plan.OwnerRef,
		NoPromoteID:   nop.ID,
		EvidenceRefs:  evidenceRefs,
	}
	if err := recordSourceObservationVerdictReviewResult(homePath, sourcePath, plan, report); err != nil {
		return SourceObservationVerdictReviewReport{}, err
	}
	return report, nil
}

func ValidateSourceObservationVerdictReview(review SourceObservationVerdictReview) error {
	if review.SchemaVersion != SourceObservationVerdictReviewSchemaVersion {
		return fmt.Errorf("source observation verdict review schema_version = %q, want %q", review.SchemaVersion, SourceObservationVerdictReviewSchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.PlanID) == "" || strings.TrimSpace(review.VerdictID) == "" || strings.TrimSpace(review.CandidateRef) == "" {
		return fmt.Errorf("source observation verdict review id plan_id verdict_id candidate_ref required")
	}
	if strings.ContainsAny(review.PlanID, `/\`) || strings.ContainsAny(review.VerdictID, `/\`) || strings.ContainsAny(review.CandidateRef, `/\`) {
		return fmt.Errorf("source observation verdict review ids must not contain path separators")
	}
	if !verdictflow.OneOf(review.Decision, []string{"record_no_promote", "park", "reject"}) {
		return fmt.Errorf("source observation verdict review decision must be record_no_promote|park|reject, got %q", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" || strings.HasPrefix(strings.TrimSpace(review.Rationale), "TODO") {
		return fmt.Errorf("source observation verdict review rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("source observation verdict review confidence must be between 0 and 1")
	}
	if len(verdictflow.Compact(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("source observation verdict review evidence_refs required")
	}
	return nil
}

func AnnotateObservationVerdictQueue(report *reviewartifact.QueueReport) {
	verdictflow.AnnotateQueue(report, SourceObservationVerdictReviewArea, annotateSourceObservationVerdictItem)
}

func annotateSourceObservationVerdictItem(item *reviewartifact.QueueItem) {
	plan, ok := sourceObservationVerdictPlanFromQueueItem(*item)
	if record, recordOk := sourceObservationVerdictReviewedFromExtras(item.ExtraPaths); recordOk {
		item.Status = record.Status
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "source observation verdict review recorded"
		item.Actions = nil
		item.DecisionCard = nil
		if ok {
			annotateSourceObservationVerdictOperatorCard(item, plan, reviewartifact.BandFiled)
		}
		return
	}
	if !ok {
		item.Status = "blocked"
		item.Readiness = "blocked"
		item.BlockedReason = "source observation verdict plan unreadable"
		item.NextAction = "regenerate source observation-verdict-plan"
		item.Actions = nil
		return
	}
	annotateSourceObservationVerdictOperatorCard(item, plan, reviewartifact.BandNeedsYou)
	reviewPath := sourceObservationVerdictReviewYAMLPath(*item)
	if reviewPath == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft missing"
		item.NextAction = "regenerate source observation-verdict-plan"
		item.Actions = nil
		return
	}
	item.Readiness = "needs_review"
	item.BlockedReason = "source owner must decide reviewed panel verdict no-promote disposition"
	item.NextAction = "edit review YAML then record source observation verdict review"
	item.Owner = reviewartifact.FirstNonEmpty(item.Owner, "internal/sourceobs")
	item.SourceKind = reviewartifact.FirstNonEmpty(item.SourceKind, "panel_verdict")
	item.CandidateID = reviewartifact.FirstNonEmpty(item.CandidateID, plan.TargetRef)
	item.Boundaries = reviewartifact.Unique(append(item.Boundaries, plan.Boundaries...))
	item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, plan.EvidenceRefs...))
	item.DecisionCard = sourceObservationVerdictDecisionCard(plan, reviewPath)
	item.Actions = []reviewartifact.Action{{
		ID:             "record_source_observation_verdict_review",
		Label:          "Record source observation verdict review",
		Command:        verdictflow.ShellJoin([]string{"compound", "source", "observation-verdict-review", reviewPath}),
		Argv:           []string{"compound", "source", "observation-verdict-review", reviewPath},
		RequiresReview: true,
	}}
}

func annotateSourceObservationVerdictOperatorCard(item *reviewartifact.QueueItem, plan SourceObservationVerdictPlan, band string) {
	card, err := OperatorCardForObservationVerdictPlan(plan)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "source observation verdict operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "source observation verdict operator card invalid: "+err.Error())
	}
}

func sourceObservationVerdictDecisionCard(plan SourceObservationVerdictPlan, reviewPath string) *reviewartifact.DecisionCard {
	argv := []string{"compound", "source", "observation-verdict-review", reviewPath}
	return &reviewartifact.DecisionCard{
		Claim:               "Review source observation verdict as no-promote evidence, not truth.",
		PanelLean:           "record_no_promote unless reviewer rejects the lens reading",
		OptionalRationale:   false,
		CanonicalActionHint: "Edit review YAML decision/rationale/confidence, then run `compound source observation-verdict-review <review.yaml>`. This records no-promote evidence only.",
		Boundary:            plan.Boundaries,
		Meta: map[string]string{
			"review_yaml":  reviewPath,
			"verdict_id":   plan.VerdictID,
			"owner_kind":   plan.OwnerKind,
			"owner_ref":    plan.OwnerRef,
			"owner_status": plan.OwnerStatus,
		},
		Why: []reviewartifact.DecisionCardReason{
			{Label: "panel verdict", Question: "What lens synthesis is being consumed?", Detail: plan.VerdictID + " / " + plan.PanelDecision},
			{Label: "source owner", Question: "Which source surface owns the disposition?", Detail: plan.OwnerKind + ":" + plan.OwnerRef + " (" + plan.OwnerStatus + ")"},
			{Label: "candidate", Question: "What was observed?", Detail: firstNonEmpty(plan.Statement, plan.Detail, plan.TargetRef)},
			{Label: "boundary", Severity: "warning", Question: "What must stay outside this bridge?", Detail: strings.Join(plan.Boundaries, "; ")},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{ID: "record_no_promote", Label: "Record No-Promote", Effect: "Records source-owned no-promote evidence for the reviewed observation verdict.", Command: verdictflow.ShellJoin(argv), Argv: argv, RequiresReview: true},
			{ID: "park", Label: "Park", Effect: "Keeps reviewed verdict as parked source evidence without promotion or product mutation.", Command: verdictflow.ShellJoin(argv), Argv: argv, RequiresReview: true},
			{ID: "reject", Label: "Reject", Effect: "Records that this panel verdict should not feed source/product/memory work.", Command: verdictflow.ShellJoin(argv), Argv: argv, RequiresReview: true},
		},
	}
}

func renderSourceObservationVerdictPlanMarkdown(plan SourceObservationVerdictPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Source Observation Verdict Plan: %s\n\n", plan.VerdictID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- owner: %s/%s (%s)\n", plan.OwnerKind, plan.OwnerRef, plan.OwnerStatus)
	fmt.Fprintf(&b, "- verdict: %s\n", plan.VerdictID)
	fmt.Fprintf(&b, "- target: %s/%s\n", plan.TargetKind, plan.TargetRef)
	fmt.Fprintf(&b, "- panel_decision: %s\n", plan.PanelDecision)
	fmt.Fprintf(&b, "- review_command: `%s`\n", plan.ReviewDraft.ReviewCommand)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Source Basis")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- source_observation: %s\n", firstNonEmpty(plan.SourceObservationID, "none"))
	fmt.Fprintf(&b, "- source_ref: %s\n", firstNonEmpty(plan.SourceRef, "unknown"))
	fmt.Fprintf(&b, "- subject: %s\n", firstNonEmpty(plan.Subject, "unknown"))
	fmt.Fprintf(&b, "- statement: %s\n", firstNonEmpty(plan.Statement, "unknown"))
	fmt.Fprintf(&b, "- detail: %s\n", firstNonEmpty(plan.Detail, "unknown"))
	fmt.Fprintf(&b, "- next_action: %s\n", plan.NextAction)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Review")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Boundaries")
	fmt.Fprintln(&b)
	for _, boundary := range plan.Boundaries {
		fmt.Fprintf(&b, "- %s\n", boundary)
	}
	return b.String()
}

func sourceObservationVerdictProductSubject(payload sourceObservationVerdictCandidatePayload) bool {
	if strings.TrimSpace(payload.SubjectKind) == "product" && strings.TrimSpace(payload.SubjectID) != "" {
		return true
	}
	subject := strings.TrimSpace(payload.Subject)
	if strings.HasPrefix(subject, "product:") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(payload.SubjectID), "product:")
}

func sourceObservationVerdictOwner(payload sourceObservationVerdictCandidatePayload, targetRef string) (kind, ref, status string) {
	if strings.TrimSpace(payload.SourceObservationID) != "" {
		return "source_observation", strings.TrimSpace(payload.SourceObservationID), "sourceobs"
	}
	if strings.TrimSpace(payload.SourceRef) != "" {
		return "source_ref", strings.TrimSpace(payload.SourceRef), "source_ref_only"
	}
	return "observation_verdict", strings.TrimSpace(targetRef), "unsupported_no_owner"
}

func sourceObservationVerdictHashTarget(path string) (bool, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, "", nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	sum := idFor("", string(data))
	return true, "sha256:" + strings.TrimPrefix(sum, "_"), nil
}

func findSourceObservationVerdictPlanForReview(homePath string, review SourceObservationVerdictReview, sourcePath string) (SourceObservationVerdictPlan, error) {
	if stem := sourceObservationVerdictArtifactStem(sourcePath); stem != "" {
		plan, err := loadSourceObservationVerdictPlanByPath(filepath.Join(homePath, "reviews", SourceObservationVerdictReviewArea, stem+".json"))
		if err == nil && plan.ID != "" {
			return plan, nil
		}
	}
	if strings.TrimSpace(review.PlanID) != "" {
		plan, err := loadSourceObservationVerdictPlanByPath(filepath.Join(homePath, "reviews", SourceObservationVerdictReviewArea, review.PlanID+".json"))
		if err == nil && plan.ID != "" {
			return plan, nil
		}
	}
	return SourceObservationVerdictPlan{}, fmt.Errorf("source observation verdict plan for review %q not found", review.ID)
}

func loadSourceObservationVerdictPlanByPath(path string) (SourceObservationVerdictPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SourceObservationVerdictPlan{}, err
	}
	var plan SourceObservationVerdictPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return SourceObservationVerdictPlan{}, err
	}
	return plan, nil
}

func recordSourceObservationVerdictReviewResult(homePath, sourcePath string, plan SourceObservationVerdictPlan, report SourceObservationVerdictReviewReport) error {
	stem := sourceObservationVerdictArtifactStem(sourcePath)
	if stem == "" {
		stem = plan.ID
	}
	dir, err := reviewartifact.Dir(homePath, SourceObservationVerdictReviewArea)
	if err != nil {
		return err
	}
	report.ReviewPath = filepath.Join(dir, stem+"."+report.ID+".reviewed.json")
	if _, err := reviewartifact.WriteJSON(homePath, SourceObservationVerdictReviewArea, stem+"."+report.ID+".reviewed.json", report, " "); err != nil {
		return err
	}
	plan.Status = "recorded"
	plan.ReviewedAt = report.CreatedAt
	plan.ReviewID = report.ReviewID
	plan.ReviewDecision = report.Decision
	plan.NoPromoteID = report.NoPromoteID
	data, err := json.MarshalIndent(plan, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(homePath, "reviews", SourceObservationVerdictReviewArea, stem+".json"), append(data, '\n'), 0o644)
}

func sourceObservationVerdictPlanFromQueueItem(item reviewartifact.QueueItem) (SourceObservationVerdictPlan, bool) {
	plan, err := loadSourceObservationVerdictPlanByPath(item.JSONPath)
	if err != nil {
		return SourceObservationVerdictPlan{}, false
	}
	return plan, plan.SchemaVersion == SourceObservationVerdictPlanSchemaVersion
}

func sourceObservationVerdictReviewYAMLPath(item reviewartifact.QueueItem) string {
	stem := strings.TrimSuffix(filepath.Base(item.JSONPath), ".json")
	for _, path := range item.ExtraPaths {
		if filepath.Base(path) == stem+".review.yaml" {
			return path
		}
	}
	candidate := filepath.Join(filepath.Dir(item.JSONPath), stem+".review.yaml")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

func sourceObservationVerdictReviewedFromExtras(paths []string) (SourceObservationVerdictReviewReport, bool) {
	return verdictflow.LatestReviewedFromExtras(paths,
		func(r SourceObservationVerdictReviewReport) bool {
			return r.SchemaVersion == SourceObservationVerdictReportSchemaVersion && r.ID != "" && r.Status != ""
		},
		func(r SourceObservationVerdictReviewReport) string { return r.ID },
		func(r SourceObservationVerdictReviewReport) string { return r.CreatedAt },
	)
}

func sourceObservationVerdictArtifactStem(path string) string {
	base := filepath.Base(strings.TrimSpace(path))
	if base == "" || base == "." {
		return ""
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	return strings.TrimSuffix(stem, ".review")
}
