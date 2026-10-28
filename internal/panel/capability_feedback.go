package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const (
	PanelCapabilityFeedbackPlanSchemaVersion   = "compound.panel_capability_feedback_plan.v0.1"
	PanelCapabilityFeedbackReviewSchemaVersion = "compound.panel_capability_feedback_review.v0.1"
	PanelCapabilityFeedbackReportSchemaVersion = "compound.panel_capability_feedback_report.v0.1"
	PanelCapabilityFeedbackReviewArea          = "capability-panel-feedback"
)

type PanelCapabilityFeedbackPlan struct {
	SchemaVersion      string                             `json:"schema_version"`
	ID                 string                             `json:"id"`
	CreatedAt          string                             `json:"created_at"`
	Status             string                             `json:"status"`
	VerdictID          string                             `json:"verdict_id"`
	RunID              string                             `json:"run_id"`
	TargetKind         string                             `json:"target_kind"`
	TargetRef          string                             `json:"target_ref"`
	PanelDecision      string                             `json:"panel_decision"`
	SourcePath         string                             `json:"source_path"`
	SourceHash         string                             `json:"source_hash"`
	EvidenceRefs       []string                           `json:"evidence_refs"`
	FeedbackCandidates []PanelCapabilityFeedbackCandidate `json:"feedback_candidates"`
	Boundaries         []string                           `json:"boundaries"`
	ReviewYAMLPath     string                             `json:"review_yaml_path,omitempty"`
}

type PanelCapabilityFeedbackCandidate struct {
	ID           string   `json:"id" yaml:"candidate_id"`
	NodeID       string   `json:"node_id" yaml:"node_id"`
	EdgeKind     string   `json:"edge_kind" yaml:"edge_kind"`
	Context      string   `json:"context" yaml:"context"`
	Confidence   float64  `json:"confidence" yaml:"confidence"`
	LensRef      string   `json:"lens_ref,omitempty" yaml:"lens_ref,omitempty"`
	Severity     string   `json:"severity,omitempty" yaml:"severity,omitempty"`
	Claim        string   `json:"claim,omitempty" yaml:"claim,omitempty"`
	Rationale    string   `json:"rationale,omitempty" yaml:"rationale,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty" yaml:"evidence_refs,omitempty"`
}

type PanelCapabilityFeedbackPaths struct {
	PlanPath       string `json:"plan_path"`
	ReviewPath     string `json:"review_path"`
	ReviewYAMLPath string `json:"review_yaml_path"`
}

type PanelCapabilityFeedbackReview struct {
	SchemaVersion string                              `json:"schema_version" yaml:"schema_version"`
	ReviewID      string                              `json:"review_id" yaml:"review_id"`
	PlanID        string                              `json:"plan_id" yaml:"plan_id"`
	VerdictID     string                              `json:"verdict_id" yaml:"verdict_id"`
	Decision      string                              `json:"decision" yaml:"decision"`
	Rationale     string                              `json:"rationale" yaml:"rationale"`
	ReviewedBy    string                              `json:"reviewed_by,omitempty" yaml:"reviewed_by,omitempty"`
	Feedback      []PanelCapabilityFeedbackReviewItem `json:"feedback" yaml:"feedback"`
}

type PanelCapabilityFeedbackReviewItem struct {
	CandidateID string  `json:"candidate_id" yaml:"candidate_id"`
	NodeID      string  `json:"node_id" yaml:"node_id"`
	EdgeKind    string  `json:"edge_kind" yaml:"edge_kind"`
	Context     string  `json:"context" yaml:"context"`
	Confidence  float64 `json:"confidence" yaml:"confidence"`
	Decision    string  `json:"decision" yaml:"decision"`
	Rationale   string  `json:"rationale,omitempty" yaml:"rationale,omitempty"`
}

type PanelCapabilityFeedbackReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	ReviewID      string   `json:"review_id"`
	PlanID        string   `json:"plan_id"`
	VerdictID     string   `json:"verdict_id"`
	Decision      string   `json:"decision"`
	AppliedEdges  int      `json:"applied_edges"`
	EdgeRefs      []string `json:"edge_refs,omitempty"`
	ReviewedBy    string   `json:"reviewed_by,omitempty"`
	ReviewPath    string   `json:"review_path,omitempty"`
	ReviewedPath  string   `json:"reviewed_path,omitempty"`
	PlanPath      string   `json:"plan_path,omitempty"`
	Boundaries    []string `json:"boundaries"`
}

func BuildPanelCapabilityFeedbackPlan(homePath, verdictPath string, now time.Time) (PanelCapabilityFeedbackPlan, error) {
	if strings.TrimSpace(homePath) == "" {
		return PanelCapabilityFeedbackPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	verdict, err := LoadVerdictArtifact(verdictPath)
	if err != nil {
		return PanelCapabilityFeedbackPlan{}, err
	}
	sourceHash, err := fileSHA256(verdictPath)
	if err != nil {
		return PanelCapabilityFeedbackPlan{}, err
	}
	candidates := panelCapabilityFeedbackCandidates(verdict)
	if len(candidates) == 0 {
		return PanelCapabilityFeedbackPlan{}, fmt.Errorf("panel verdict %s has no capability feedback candidates", verdict.ID)
	}
	plan := PanelCapabilityFeedbackPlan{
		SchemaVersion:      PanelCapabilityFeedbackPlanSchemaVersion,
		ID:                 reviewartifact.ID("pcfp_", verdict.ID, sourceHash),
		CreatedAt:          reviewartifact.Timestamp(now),
		Status:             "review_required",
		VerdictID:          verdict.ID,
		RunID:              verdict.RunID,
		TargetKind:         strings.TrimSpace(verdict.Target.Kind),
		TargetRef:          strings.TrimSpace(verdict.Target.Ref),
		PanelDecision:      strings.TrimSpace(verdict.Synthesis.Decision),
		SourcePath:         verdictPath,
		SourceHash:         sourceHash,
		EvidenceRefs:       reviewartifact.UniqueSorted(verdict.EvidenceRefs),
		FeedbackCandidates: candidates,
		Boundaries: []string{
			"review artifact only; no graph mutation during plan",
			"feedback-review is the only command that may write CapabilityGraph feedback edges",
			"panel verdict evidence is not capability authority until reviewed",
			"no tool execution, memory promotion, provider mutation, or public side effect",
		},
	}
	return plan, nil
}

func RecordPanelCapabilityFeedbackPlan(homePath string, plan PanelCapabilityFeedbackPlan) (PanelCapabilityFeedbackPaths, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return PanelCapabilityFeedbackPaths{}, fmt.Errorf("panel capability feedback plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, PanelCapabilityFeedbackReviewArea)
	if err != nil {
		return PanelCapabilityFeedbackPaths{}, err
	}
	paths := PanelCapabilityFeedbackPaths{
		PlanPath:       filepath.Join(dir, plan.ID+".json"),
		ReviewPath:     filepath.Join(dir, plan.ID+".md"),
		ReviewYAMLPath: filepath.Join(dir, plan.ID+".review.yaml"),
	}
	plan.ReviewYAMLPath = paths.ReviewYAMLPath
	if _, err := reviewartifact.Write(homePath, PanelCapabilityFeedbackReviewArea, plan.ID, plan, " ", renderPanelCapabilityFeedbackPlan(plan)); err != nil {
		return paths, err
	}
	review := panelCapabilityFeedbackReviewDraft(plan)
	data, err := yaml.Marshal(review)
	if err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, PanelCapabilityFeedbackReviewArea, plan.ID+".review.yaml", data); err != nil {
		return paths, err
	}
	return paths, nil
}

func LoadPanelCapabilityFeedbackReview(path string) (PanelCapabilityFeedbackReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PanelCapabilityFeedbackReview{}, fmt.Errorf("read panel capability feedback review %s: %w", path, err)
	}
	var review PanelCapabilityFeedbackReview
	if err := yaml.Unmarshal(data, &review); err != nil {
		return PanelCapabilityFeedbackReview{}, fmt.Errorf("parse panel capability feedback review %s: %w", path, err)
	}
	if err := review.Validate(); err != nil {
		return PanelCapabilityFeedbackReview{}, fmt.Errorf("validate panel capability feedback review %s: %w", path, err)
	}
	return review, nil
}

func (r PanelCapabilityFeedbackReview) Validate() error {
	if strings.TrimSpace(r.SchemaVersion) != PanelCapabilityFeedbackReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s", PanelCapabilityFeedbackReviewSchemaVersion)
	}
	if strings.TrimSpace(r.ReviewID) == "" {
		return fmt.Errorf("review_id required")
	}
	if strings.TrimSpace(r.PlanID) == "" {
		return fmt.Errorf("plan_id required")
	}
	if strings.TrimSpace(r.VerdictID) == "" {
		return fmt.Errorf("verdict_id required")
	}
	switch strings.TrimSpace(r.Decision) {
	case "apply_feedback", "reject", "keep_needs_review":
	default:
		return fmt.Errorf("decision must be apply_feedback, reject, or keep_needs_review")
	}
	if strings.TrimSpace(r.Rationale) == "" {
		return fmt.Errorf("rationale required")
	}
	if r.Decision == "apply_feedback" && len(r.Feedback) == 0 {
		return fmt.Errorf("feedback required for apply_feedback")
	}
	for _, item := range r.Feedback {
		if strings.TrimSpace(item.CandidateID) == "" || strings.TrimSpace(item.NodeID) == "" {
			return fmt.Errorf("feedback item must include candidate_id and node_id")
		}
		if item.Confidence <= 0 || item.Confidence > 1 {
			return fmt.Errorf("feedback item %s confidence must be >0 and <=1", item.CandidateID)
		}
		switch strings.TrimSpace(item.Decision) {
		case "apply", "reject":
		default:
			return fmt.Errorf("feedback item %s decision must be apply or reject", item.CandidateID)
		}
		switch strings.TrimSpace(item.EdgeKind) {
		case capgraph.EdValidatedBy:
		case capgraph.EdMissedWhen, capgraph.EdFalseTriggeredBy:
			if strings.TrimSpace(item.Context) == "" {
				return fmt.Errorf("feedback item %s context required for %s", item.CandidateID, item.EdgeKind)
			}
		default:
			return fmt.Errorf("feedback item %s edge_kind must be validated_by, missed_when, or false_triggered_by", item.CandidateID)
		}
	}
	return nil
}

func RecordPanelCapabilityFeedbackReview(homePath string, review PanelCapabilityFeedbackReview, sourcePath string, now time.Time) (PanelCapabilityFeedbackReviewReport, error) {
	report := PanelCapabilityFeedbackReviewReport{
		SchemaVersion: PanelCapabilityFeedbackReportSchemaVersion,
		ID:            reviewartifact.ID("pcfr_", review.ReviewID, review.Decision),
		CreatedAt:     reviewartifact.Timestamp(now),
		Status:        panelCapabilityFeedbackReportStatus(review.Decision),
		ReviewID:      review.ReviewID,
		PlanID:        review.PlanID,
		VerdictID:     review.VerdictID,
		Decision:      review.Decision,
		ReviewedBy:    strings.TrimSpace(review.ReviewedBy),
		ReviewPath:    strings.TrimSpace(sourcePath),
		Boundaries: []string{
			"reviewed capability feedback only",
			"does not execute tools or panel runs",
			"does not promote memory or mutate provider/model config",
		},
	}
	if review.Decision == "apply_feedback" {
		applied, refs, err := applyPanelCapabilityFeedback(homePath, review, now)
		if err != nil {
			return report, err
		}
		report.AppliedEdges = applied
		report.EdgeRefs = reviewartifact.UniqueSorted(refs)
	}
	return recordPanelCapabilityFeedbackReviewedPlan(homePath, review, report)
}

func AnnotateCapabilityFeedbackQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != PanelCapabilityFeedbackReviewArea {
			continue
		}
		annotateCapabilityFeedbackQueueItem(item)
	}
}

func annotateCapabilityFeedbackQueueItem(item *reviewartifact.QueueItem) {
	switch strings.TrimSpace(item.Status) {
	case "applied_feedback", "rejected", "kept_needs_review", "recorded", "ok", "resolved":
		item.Status = "recorded"
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "panel capability feedback review recorded"
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	plan, ok := capabilityFeedbackPlanFromQueueItem(*item)
	if !ok {
		item.Status = "blocked"
		item.Readiness = "blocked"
		item.BlockedReason = "panel capability feedback plan unreadable"
		item.NextAction = "regenerate panel capability feedback plan"
		item.Actions = nil
		return
	}
	if item.Status == "" || item.Status == "unknown" {
		item.Status = "review_required"
	}
	item.Readiness = "needs_review"
	item.BlockedReason = ""
	item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, plan.EvidenceRefs...))
	item.DecisionCard = buildCapabilityFeedbackDecisionCard(*item, plan)
	item.Actions = capabilityFeedbackDecisionCardActions(item.DecisionCard)
	item.NextAction = fmt.Sprintf("review panel capability feedback for verdict %s; edit YAML then run panel feedback-review", plan.VerdictID)
}

func panelCapabilityFeedbackCandidates(verdict verdictArtifactPayload) []PanelCapabilityFeedbackCandidate {
	var out []PanelCapabilityFeedbackCandidate
	baseContext := panelCapabilityFeedbackContext(verdict, "")
	for _, lens := range verdict.LensFindings {
		nodeID := strings.TrimSpace(lens.LensRef)
		if nodeID == "" {
			continue
		}
		evidenceRefs := []string{lens.RouteEvidenceRef}
		evidenceRefs = append(evidenceRefs, verdict.EvidenceRefs...)
		if len(lens.Findings) == 0 {
			if strings.TrimSpace(lens.Error) == "" {
				continue
			}
			out = append(out, PanelCapabilityFeedbackCandidate{
				ID:           reviewartifact.ID("pcfc_", verdict.ID, nodeID, "error"),
				NodeID:       nodeID,
				EdgeKind:     capgraph.EdFalseTriggeredBy,
				Context:      panelCapabilityFeedbackContext(verdict, lens.Error),
				Confidence:   0.6,
				LensRef:      nodeID,
				Severity:     "error",
				Claim:        lens.Error,
				Rationale:    "lens returned error during panel verdict; reviewer may confirm false trigger or reject",
				EvidenceRefs: reviewartifact.UniqueSorted(evidenceRefs),
			})
			continue
		}
		for i, finding := range lens.Findings {
			itemEvidence := append([]string(nil), evidenceRefs...)
			itemEvidence = append(itemEvidence, finding.EvidenceRefs...)
			edgeKind := panelCapabilityFeedbackEdgeKind(nodeID, finding)
			out = append(out, PanelCapabilityFeedbackCandidate{
				ID:           reviewartifact.ID("pcfc_", verdict.ID, nodeID, finding.Claim, fmt.Sprint(i)),
				NodeID:       nodeID,
				EdgeKind:     edgeKind,
				Context:      panelCapabilityFeedbackContext(verdict, finding.Claim),
				Confidence:   panelCapabilityFeedbackConfidence(lens.Confidence, verdict.Synthesis.Confidence),
				LensRef:      nodeID,
				Severity:     strings.TrimSpace(finding.Severity),
				Claim:        strings.TrimSpace(finding.Claim),
				Rationale:    panelCapabilityFeedbackRationale(edgeKind),
				EvidenceRefs: reviewartifact.UniqueSorted(itemEvidence),
			})
		}
		if len(lens.Findings) > 0 && baseContext == "" {
			baseContext = panelCapabilityFeedbackContext(verdict, lens.LensRef)
		}
	}
	return out
}

func panelCapabilityFeedbackEdgeKind(nodeID string, finding Finding) string {
	nodeID = strings.TrimSpace(nodeID)
	claim := strings.ToLower(strings.TrimSpace(finding.Claim))
	if nodeID == "lens:anti-pattern-operational-detector" {
		switch {
		case strings.Contains(claim, "false trigger") ||
			strings.Contains(claim, "false-trigger") ||
			strings.Contains(claim, "false positive") ||
			strings.Contains(claim, "over-trigger") ||
			strings.Contains(claim, "should not trigger"):
			return capgraph.EdFalseTriggeredBy
		case strings.Contains(claim, "lacks") ||
			strings.Contains(claim, "missing") ||
			strings.Contains(claim, "without") ||
			strings.Contains(claim, "passive") ||
			strings.Contains(claim, "prose") ||
			strings.Contains(claim, "needs"):
			return capgraph.EdMissedWhen
		}
	}
	return capgraph.EdValidatedBy
}

func panelCapabilityFeedbackRationale(edgeKind string) string {
	switch edgeKind {
	case capgraph.EdMissedWhen:
		return "anti-pattern detector finding names missing trigger/redirect/consumer/proof behavior; reviewer may apply missed_when feedback so future surfacing learns the redirect context"
	case capgraph.EdFalseTriggeredBy:
		return "anti-pattern detector finding names a false trigger; reviewer may apply false_triggered_by feedback so future surfacing learns the silence context"
	default:
		return "lens produced review evidence in panel verdict; reviewer may validate, redirect missed/false-trigger feedback, or reject"
	}
}

func panelCapabilityFeedbackReviewDraft(plan PanelCapabilityFeedbackPlan) PanelCapabilityFeedbackReview {
	review := PanelCapabilityFeedbackReview{
		SchemaVersion: PanelCapabilityFeedbackReviewSchemaVersion,
		ReviewID:      reviewartifact.ID("pcfr_", plan.ID, plan.VerdictID),
		PlanID:        plan.ID,
		VerdictID:     plan.VerdictID,
		Decision:      "TODO_apply_feedback_reject_keep_needs_review",
		Rationale:     "",
		ReviewedBy:    "",
		Feedback:      make([]PanelCapabilityFeedbackReviewItem, 0, len(plan.FeedbackCandidates)),
	}
	for _, candidate := range plan.FeedbackCandidates {
		review.Feedback = append(review.Feedback, PanelCapabilityFeedbackReviewItem{
			CandidateID: candidate.ID,
			NodeID:      candidate.NodeID,
			EdgeKind:    candidate.EdgeKind,
			Context:     candidate.Context,
			Confidence:  candidate.Confidence,
			Decision:    "TODO_apply_or_reject",
			Rationale:   "",
		})
	}
	return review
}

func applyPanelCapabilityFeedback(homePath string, review PanelCapabilityFeedbackReview, now time.Time) (int, []string, error) {
	store, err := capgraph.Open(homePath)
	if err != nil {
		return 0, nil, err
	}
	if err := validateFeedbackNodes(store, review); err != nil {
		return 0, nil, err
	}
	var applied int
	var refs []string
	for _, item := range review.Feedback {
		if item.Decision != "apply" {
			continue
		}
		switch item.EdgeKind {
		case capgraph.EdValidatedBy:
			edge := capgraph.Edge{
				Kind:       capgraph.EdValidatedBy,
				From:       item.NodeID,
				To:         "panel_verdict:" + review.VerdictID,
				Confidence: item.Confidence,
				CreatedAt:  reviewartifact.Timestamp(now),
			}
			if err := store.AddEdge(edge, now); err != nil {
				return applied, refs, err
			}
			applied++
			refs = append(refs, panelCapabilityFeedbackEdgeRef(edge))
		case capgraph.EdMissedWhen, capgraph.EdFalseTriggeredBy:
			edges, err := store.Observe(item.NodeID, item.EdgeKind, item.Context, item.Confidence, now)
			if err != nil {
				return applied, refs, err
			}
			applied += len(edges)
			for _, edge := range edges {
				refs = append(refs, panelCapabilityFeedbackEdgeRef(edge))
			}
		}
	}
	return applied, refs, nil
}

func validateFeedbackNodes(store *capgraph.Store, review PanelCapabilityFeedbackReview) error {
	nodes, err := store.List()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, node := range nodes {
		if !node.Retired {
			known[node.ID] = true
		}
	}
	for _, item := range review.Feedback {
		if item.Decision != "apply" {
			continue
		}
		if !known[item.NodeID] {
			return fmt.Errorf("feedback item %s references unknown or retired capability node %s", item.CandidateID, item.NodeID)
		}
	}
	return nil
}

func recordPanelCapabilityFeedbackReviewedPlan(homePath string, review PanelCapabilityFeedbackReview, report PanelCapabilityFeedbackReviewReport) (PanelCapabilityFeedbackReviewReport, error) {
	reportPath, err := reviewartifact.WriteJSON(homePath, PanelCapabilityFeedbackReviewArea, review.PlanID+".reviewed.json", report, " ")
	if err != nil {
		return report, err
	}
	report.ReviewedPath = reportPath
	dir, err := reviewartifact.Dir(homePath, PanelCapabilityFeedbackReviewArea)
	if err != nil {
		return report, err
	}
	planPath := filepath.Join(dir, review.PlanID+".json")
	data, err := os.ReadFile(planPath)
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return report, fmt.Errorf("read panel capability feedback plan %s: %w", planPath, err)
	}
	var plan PanelCapabilityFeedbackPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return report, fmt.Errorf("parse panel capability feedback plan %s: %w", planPath, err)
	}
	plan.Status = report.Status
	paths, err := reviewartifact.Write(homePath, PanelCapabilityFeedbackReviewArea, plan.ID, plan, " ", renderPanelCapabilityFeedbackPlan(plan))
	if err != nil {
		return report, err
	}
	report.PlanPath = paths.PlanPath
	if updated, err := reviewartifact.WriteJSON(homePath, PanelCapabilityFeedbackReviewArea, review.PlanID+".reviewed.json", report, " "); err == nil {
		report.ReviewedPath = updated
	}
	return report, nil
}

func capabilityFeedbackPlanFromQueueItem(item reviewartifact.QueueItem) (PanelCapabilityFeedbackPlan, bool) {
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return PanelCapabilityFeedbackPlan{}, false
	}
	var plan PanelCapabilityFeedbackPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return PanelCapabilityFeedbackPlan{}, false
	}
	if plan.SchemaVersion != PanelCapabilityFeedbackPlanSchemaVersion || strings.TrimSpace(plan.ID) == "" {
		return PanelCapabilityFeedbackPlan{}, false
	}
	return plan, true
}

func buildCapabilityFeedbackDecisionCard(item reviewartifact.QueueItem, plan PanelCapabilityFeedbackPlan) *reviewartifact.DecisionCard {
	reviewPath := panelCapabilityFeedbackReviewYAMLPath(item, plan)
	card := &reviewartifact.DecisionCard{
		Claim:               fmt.Sprintf("Review panel verdict %s capability feedback before graph learning", plan.VerdictID),
		CanonicalActionHint: "Edit the review YAML, then run `compound panel feedback-review <review.yaml>`. This is the only path that writes CapabilityGraph feedback edges.",
		Boundary: []string{
			"read-only review projection",
			"review YAML must be edited before graph feedback",
			"no automatic panel-to-graph mutation",
			"no tool execution, memory promotion, or provider mutation",
		},
		Meta: map[string]string{
			"plan_id":     plan.ID,
			"verdict_id":  plan.VerdictID,
			"target_kind": plan.TargetKind,
			"target_ref":  plan.TargetRef,
			"review_yaml": reviewPath,
		},
		Why: []reviewartifact.DecisionCardReason{
			{Label: "panel verdict", Question: "What generated feedback?", Detail: plan.VerdictID},
			{Label: "target", Question: "What context was reviewed?", Detail: strings.TrimSpace(plan.TargetKind + " " + plan.TargetRef)},
			{Label: "feedback candidates", Question: "What could change capability authority?", Detail: fmt.Sprintf("%d candidate edge(s)", len(plan.FeedbackCandidates))},
			{Label: "boundary", Question: "What must not happen automatically?", Severity: "warning", Detail: "panel verdict evidence needs edited review before CapabilityGraph feedback"},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{
				ID:             "apply_feedback",
				Label:          "Apply Feedback",
				Effect:         "Apply reviewed validated_by, missed_when, or false_triggered_by edges to CapabilityGraph.",
				Command:        "$EDITOR " + shellQuote(reviewPath) + " && compound panel feedback-review " + shellQuote(reviewPath),
				Argv:           []string{"/bin/sh", "-lc", "$EDITOR " + shellQuote(reviewPath) + " && compound panel feedback-review " + shellQuote(reviewPath)},
				RequiresReview: true,
			},
			{
				ID:             "reject",
				Label:          "Reject",
				Effect:         "Record rejection; no graph feedback edges.",
				Command:        "$EDITOR " + shellQuote(reviewPath) + " && compound panel feedback-review " + shellQuote(reviewPath),
				Argv:           []string{"/bin/sh", "-lc", "$EDITOR " + shellQuote(reviewPath) + " && compound panel feedback-review " + shellQuote(reviewPath)},
				RequiresReview: true,
			},
		},
	}
	return card
}

func capabilityFeedbackDecisionCardActions(card *reviewartifact.DecisionCard) []reviewartifact.Action {
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

func panelCapabilityFeedbackReviewYAMLPath(item reviewartifact.QueueItem, plan PanelCapabilityFeedbackPlan) string {
	if strings.TrimSpace(plan.ReviewYAMLPath) != "" {
		return plan.ReviewYAMLPath
	}
	if strings.TrimSpace(item.JSONPath) == "" {
		return ""
	}
	return strings.TrimSuffix(item.JSONPath, ".json") + ".review.yaml"
}

func renderPanelCapabilityFeedbackPlan(plan PanelCapabilityFeedbackPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Panel Capability Feedback: %s\n\n", plan.VerdictID)
	fmt.Fprintf(&b, "- status: `%s`\n", plan.Status)
	fmt.Fprintf(&b, "- target: `%s` `%s`\n", plan.TargetKind, plan.TargetRef)
	fmt.Fprintf(&b, "- decision: `%s`\n", plan.PanelDecision)
	fmt.Fprintf(&b, "- candidates: %d\n\n", len(plan.FeedbackCandidates))
	fmt.Fprintln(&b, "## Boundary")
	for _, boundary := range plan.Boundaries {
		fmt.Fprintf(&b, "- %s\n", boundary)
	}
	fmt.Fprintln(&b, "\n## Feedback Candidates")
	for _, candidate := range plan.FeedbackCandidates {
		fmt.Fprintf(&b, "- `%s` `%s` `%s` context=%q claim=%q\n", candidate.ID, candidate.NodeID, candidate.EdgeKind, candidate.Context, candidate.Claim)
	}
	fmt.Fprintln(&b, "\nEdit the sibling review YAML and run `compound panel feedback-review <review.yaml>`.")
	return b.String()
}

func panelCapabilityFeedbackContext(verdict verdictArtifactPayload, detail string) string {
	parts := []string{
		verdict.Target.Kind,
		verdict.Target.Ref,
		verdict.Target.Summary,
		verdict.Synthesis.Decision,
		verdict.Synthesis.Rationale,
		detail,
	}
	return oneLine(strings.Join(nonEmptyPanelFeedbackStrings(parts...), " "))
}

func panelCapabilityFeedbackConfidence(values ...float64) float64 {
	for _, value := range values {
		if value > 0 && value <= 1 {
			return value
		}
	}
	return 0.8
}

func panelCapabilityFeedbackReportStatus(decision string) string {
	switch strings.TrimSpace(decision) {
	case "apply_feedback":
		return "applied_feedback"
	case "reject":
		return "rejected"
	case "keep_needs_review":
		return "kept_needs_review"
	default:
		return "unknown"
	}
}

func panelCapabilityFeedbackEdgeRef(edge capgraph.Edge) string {
	return edge.Kind + ":" + edge.From + "->" + edge.To
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func nonEmptyPanelFeedbackStrings(values ...string) []string {
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
