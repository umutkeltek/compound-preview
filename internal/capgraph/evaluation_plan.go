package capgraph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const (
	CapabilityEvaluationPlanSchemaVersion   = "compound.capability.evaluation_plan.v0.1"
	CapabilityEvaluationReviewSchemaVersion = "compound.capability.evaluation_review.v0.1"
)

// CapabilityEvaluationPlan is an owner-review artifact for an evaluated
// capability that is not yet known to the CapabilityGraph. It never mutates
// graph nodes, installs skill files, executes tools, or promotes claims.
type CapabilityEvaluationPlan struct {
	SchemaVersion  string              `json:"schema_version"`
	ID             string              `json:"id"`
	CreatedAt      string              `json:"created_at"`
	Status         string              `json:"status"`
	EvaluationSet  string              `json:"evaluation_set"`
	EvaluationID   string              `json:"evaluation_id"`
	Capability     string              `json:"capability"`
	Verdict        string              `json:"verdict"`
	RubricScore    float64             `json:"rubric_score,omitempty"`
	SourceReceipt  string              `json:"source_receipt,omitempty"`
	Signals        map[string]any      `json:"signals,omitempty"`
	EvidenceRefs   []string            `json:"evidence_refs"`
	TargetPath     string              `json:"target_path,omitempty"`
	TargetExists   bool                `json:"target_exists"`
	TargetHash     string              `json:"target_hash,omitempty"`
	NodeDraft      CapabilityNodeDraft `json:"node_draft"`
	ReviewChoices  []string            `json:"review_choices"`
	RequiredReview []string            `json:"required_review"`
	Warnings       []string            `json:"warnings,omitempty"`
}

type CapabilityNodeDraft struct {
	NodeID           string   `json:"node_id" yaml:"node_id"`
	Kind             string   `json:"kind" yaml:"kind"`
	Name             string   `json:"name" yaml:"name"`
	Triggers         []string `json:"triggers" yaml:"triggers"`
	Weight           float64  `json:"weight" yaml:"weight"`
	RequiredEvidence []string `json:"required_evidence" yaml:"required_evidence"`
	ReviewNote       string   `json:"review_note" yaml:"review_note"`
}

type CapabilityEvaluationPlanPaths struct {
	PlanPath          string
	ReviewPath        string
	ReviewYAMLPath    string
	ManifestDraftPath string
}

type CapabilityEvaluationReview struct {
	SchemaVersion string              `json:"schema_version" yaml:"schema_version"`
	ReviewID      string              `json:"review_id" yaml:"review_id"`
	PlanID        string              `json:"plan_id" yaml:"plan_id"`
	EvaluationID  string              `json:"evaluation_id" yaml:"evaluation_id"`
	Capability    string              `json:"capability" yaml:"capability"`
	Decision      string              `json:"decision" yaml:"decision"`
	Rationale     string              `json:"rationale" yaml:"rationale"`
	Confidence    float64             `json:"confidence" yaml:"confidence"`
	NodeDraft     CapabilityNodeDraft `json:"node_draft" yaml:"node_draft"`
	EvidenceRefs  []string            `json:"evidence_refs" yaml:"evidence_refs"`
}

type CapabilityEvaluationReviewReport struct {
	Status       string `json:"status"`
	ReviewID     string `json:"review_id"`
	Decision     string `json:"decision"`
	NodeID       string `json:"node_id"`
	NodeStatus   string `json:"node_status"`
	ManifestPath string `json:"manifest_path"`
	PlanPath     string `json:"plan_path,omitempty"`
	ReviewedPath string `json:"reviewed_path,omitempty"`
}

func BuildCapabilityEvaluationPlan(homePath string, set CapabilityEvaluationSet, selector, targetPath string, now time.Time) (CapabilityEvaluationPlan, error) {
	if strings.TrimSpace(homePath) == "" {
		return CapabilityEvaluationPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	eval, err := selectCapabilityEvaluation(set, selector)
	if err != nil {
		return CapabilityEvaluationPlan{}, err
	}
	store, err := Open(homePath)
	if err != nil {
		return CapabilityEvaluationPlan{}, err
	}
	nodes, err := store.loadNodes()
	if err != nil {
		return CapabilityEvaluationPlan{}, err
	}
	if nodeID := resolveCapabilityNode(nodes, eval.Capability); nodeID != "" {
		return CapabilityEvaluationPlan{}, fmt.Errorf("capability %q already known as %s; use import-evaluations for graph evidence", eval.Capability, nodeID)
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return CapabilityEvaluationPlan{}, err
	}
	key := reviewartifact.Slugify(firstEvalNonEmpty(eval.Capability, eval.ID), 48)
	if key == "" {
		key = reviewartifact.Digest12(eval.ID)
	}
	kind := suggestedCapabilityKind(eval)
	nodeID := kind + ":" + key
	triggers := planSkillTriggers(eval.Capability, eval.Verdict, signalString(eval.Signals, "cluster"), signalString(eval.Signals, "source_family"))
	evidenceRefs := capabilityEvaluationPlanEvidence(set, eval)
	plan := CapabilityEvaluationPlan{
		SchemaVersion: CapabilityEvaluationPlanSchemaVersion,
		ID:            reviewartifact.ID("cep_", set.ID, eval.ID, targetPath),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        "review_required",
		EvaluationSet: set.ID,
		EvaluationID:  eval.ID,
		Capability:    eval.Capability,
		Verdict:       eval.Verdict,
		RubricScore:   eval.RubricNormalized,
		SourceReceipt: eval.SourceReceipt,
		Signals:       eval.Signals,
		EvidenceRefs:  evidenceRefs,
		TargetPath:    strings.TrimSpace(targetPath),
		TargetExists:  targetExists,
		TargetHash:    targetHash,
		NodeDraft: CapabilityNodeDraft{
			NodeID:   nodeID,
			Kind:     kind,
			Name:     humanCapabilityName(eval.Capability),
			Triggers: triggers,
			Weight:   evaluationConfidence(eval),
			RequiredEvidence: []string{
				"source capability evaluation record",
				"reviewed capability kind and trigger scope",
				"explicit input/output or operating contract",
				"overlap/replacement decision against existing CapabilityGraph nodes",
				"one validation receipt or eval showing useful behavior",
			},
			ReviewNote: "This is a CapabilityGraph node draft from an unknown evaluated capability. Reviewers may change kind/id/triggers or reject it; importing requires a separate reviewed skill/manifest path.",
		},
		ReviewChoices: append([]string(nil), NodeKinds...),
		RequiredReview: []string{
			"keep/adapt/merge/publicize does not equal install",
			"choose node kind deliberately",
			"record replacement/overlap if an owned capability already covers this",
			"do not surface to agents until imported through the graph contract",
		},
		Warnings: []string{
			"does not mutate CapabilityGraph",
			"does not install skill files",
			"does not execute tools or MCPs",
			"does not promote memory truth",
		},
	}
	for _, lens := range signalStringSlice(eval.Signals, "context_evaluation_lenses") {
		plan.RequiredReview = append(plan.RequiredReview, "context lens "+lens+" proof and silence condition reviewed")
	}
	for _, proof := range signalStringSlice(eval.Signals, "context_evaluation_proofs") {
		plan.NodeDraft.RequiredEvidence = append(plan.NodeDraft.RequiredEvidence, "context evaluation proof: "+proof)
	}
	sort.Strings(plan.ReviewChoices)
	return plan, nil
}

func RecordCapabilityEvaluationPlan(homePath string, plan CapabilityEvaluationPlan) (CapabilityEvaluationPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return CapabilityEvaluationPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(plan.ID) == "" {
		return CapabilityEvaluationPlanPaths{}, fmt.Errorf("capability evaluation plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, "capability-evaluation-intake")
	if err != nil {
		return CapabilityEvaluationPlanPaths{}, err
	}
	paths := CapabilityEvaluationPlanPaths{
		PlanPath:          filepath.Join(dir, plan.ID+".json"),
		ReviewPath:        filepath.Join(dir, plan.ID+".md"),
		ReviewYAMLPath:    filepath.Join(dir, plan.ID+".review.yaml"),
		ManifestDraftPath: filepath.Join(dir, plan.ID+".skill-manifest.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, "capability-evaluation-intake", plan.ID, plan, "  ", RenderCapabilityEvaluationPlanReview(plan)); err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, "capability-evaluation-intake", plan.ID+".review.yaml", []byte(RenderCapabilityEvaluationReviewYAMLDraft(plan))); err != nil {
		return paths, err
	}
	draft, err := CapabilityEvaluationPlanSkillManifest(plan)
	if err != nil {
		return paths, err
	}
	manifestData, err := yaml.Marshal(draft)
	if err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, "capability-evaluation-intake", plan.ID+".skill-manifest.yaml", manifestData); err != nil {
		return paths, err
	}
	return paths, nil
}

func LoadCapabilityEvaluationReview(path string) (CapabilityEvaluationReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CapabilityEvaluationReview{}, fmt.Errorf("read capability evaluation review %s: %w", path, err)
	}
	var review CapabilityEvaluationReview
	if err := yaml.Unmarshal(data, &review); err != nil {
		return CapabilityEvaluationReview{}, fmt.Errorf("parse capability evaluation review %s: %w", path, err)
	}
	if err := review.Validate(); err != nil {
		return CapabilityEvaluationReview{}, fmt.Errorf("validate capability evaluation review %s: %w", path, err)
	}
	return review, nil
}

func (r CapabilityEvaluationReview) Validate() error {
	if strings.TrimSpace(r.SchemaVersion) != CapabilityEvaluationReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s", CapabilityEvaluationReviewSchemaVersion)
	}
	if strings.TrimSpace(r.ReviewID) == "" {
		return fmt.Errorf("review_id required")
	}
	if strings.TrimSpace(r.PlanID) == "" {
		return fmt.Errorf("plan_id required")
	}
	if strings.TrimSpace(r.EvaluationID) == "" {
		return fmt.Errorf("evaluation_id required")
	}
	if strings.TrimSpace(r.Capability) == "" {
		return fmt.Errorf("capability required")
	}
	if !capabilityEvaluationReviewDecisionOK(r.Decision) {
		return fmt.Errorf("decision must be activate, reject, or keep_needs_review")
	}
	if strings.TrimSpace(r.Rationale) == "" {
		return fmt.Errorf("rationale required")
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf("confidence must be between 0 and 1")
	}
	if strings.TrimSpace(r.NodeDraft.NodeID) == "" || !contains(NodeKinds, strings.TrimSpace(r.NodeDraft.Kind)) || strings.TrimSpace(r.NodeDraft.Name) == "" || len(r.NodeDraft.Triggers) == 0 {
		return fmt.Errorf("node_draft must include id, known kind, name, and triggers")
	}
	if len(r.EvidenceRefs) == 0 {
		return fmt.Errorf("evidence_refs required")
	}
	return nil
}

func RecordCapabilityEvaluationReview(homePath string, review CapabilityEvaluationReview, sourcePath string, now time.Time) (CapabilityEvaluationReviewReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return CapabilityEvaluationReviewReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if err := review.Validate(); err != nil {
		return CapabilityEvaluationReviewReport{}, err
	}
	nodeStatus := capabilityEvaluationReviewNodeStatus(review.Decision)
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest:" + review.ReviewID,
		AdapterID:     "compound.capability-evaluation-review",
		SourceSystem:  "compound",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: SkillSourceRef{
			Path:        strings.TrimSpace(sourcePath),
			Kind:        "review_decision",
			Owner:       review.Capability,
			SourceClass: "capability_evaluation_review",
			Revision:    review.ReviewID,
		},
		EvidenceRefs: append([]string(nil), review.EvidenceRefs...),
		Skills: []SkillManifestEntry{
			{
				ID:           review.NodeDraft.NodeID,
				Kind:         review.NodeDraft.Kind,
				Name:         review.NodeDraft.Name,
				Status:       nodeStatus,
				Triggers:     append([]string(nil), review.NodeDraft.Triggers...),
				Capabilities: []string{review.Capability},
				EvidenceRefs: append([]string(nil), review.EvidenceRefs...),
				Weight:       review.NodeDraft.Weight,
			},
		},
	}
	data, err := yaml.Marshal(manifest)
	if err != nil {
		return CapabilityEvaluationReviewReport{}, err
	}
	manifestPath, err := reviewartifact.WriteRaw(homePath, "capability-evaluation-intake", review.ReviewID+".reviewed-skill-manifest.yaml", data)
	if err != nil {
		return CapabilityEvaluationReviewReport{}, err
	}
	report := CapabilityEvaluationReviewReport{
		Status:       "manifest_ready",
		ReviewID:     review.ReviewID,
		Decision:     review.Decision,
		NodeID:       review.NodeDraft.NodeID,
		NodeStatus:   nodeStatus,
		ManifestPath: manifestPath,
	}
	report, err = recordCapabilityEvaluationReviewedPlan(homePath, review, report)
	if err != nil {
		return CapabilityEvaluationReviewReport{}, err
	}
	return report, nil
}

func recordCapabilityEvaluationReviewedPlan(homePath string, review CapabilityEvaluationReview, report CapabilityEvaluationReviewReport) (CapabilityEvaluationReviewReport, error) {
	dir, err := reviewartifact.Dir(homePath, "capability-evaluation-intake")
	if err != nil {
		return report, err
	}
	reviewedData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return report, err
	}
	reviewedPath, err := reviewartifact.WriteRaw(homePath, "capability-evaluation-intake", review.PlanID+".reviewed.json", append(reviewedData, '\n'))
	if err != nil {
		return report, err
	}
	report.ReviewedPath = reviewedPath

	planPath := filepath.Join(dir, review.PlanID+".json")
	data, err := os.ReadFile(planPath)
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return report, fmt.Errorf("read capability evaluation plan %s: %w", planPath, err)
	}
	var plan CapabilityEvaluationPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return report, fmt.Errorf("parse capability evaluation plan %s: %w", planPath, err)
	}
	plan.Status = "recorded"
	paths, err := reviewartifact.Write(homePath, "capability-evaluation-intake", plan.ID, plan, "  ", RenderCapabilityEvaluationPlanReview(plan))
	if err != nil {
		return report, err
	}
	report.PlanPath = paths.PlanPath
	return report, nil
}

func CapabilityEvaluationPlanSkillManifest(plan CapabilityEvaluationPlan) (SkillManifest, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return SkillManifest{}, fmt.Errorf("capability evaluation plan id required")
	}
	if strings.TrimSpace(plan.NodeDraft.NodeID) == "" {
		return SkillManifest{}, fmt.Errorf("capability evaluation plan node draft id required")
	}
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest:" + plan.ID,
		AdapterID:     "compound.capability-evaluation-plan",
		SourceSystem:  "compound",
		ObservedAt:    plan.CreatedAt,
		SourceRef: SkillSourceRef{
			Path:        "capability-evaluation:" + plan.EvaluationID,
			Kind:        "review_artifact",
			Owner:       plan.Capability,
			SourceClass: "capability_evaluation_intake",
			Revision:    plan.ID,
		},
		EvidenceRefs: append([]string(nil), plan.EvidenceRefs...),
		Skills: []SkillManifestEntry{
			{
				ID:           plan.NodeDraft.NodeID,
				Kind:         plan.NodeDraft.Kind,
				Name:         plan.NodeDraft.Name,
				Status:       "needs_review",
				Triggers:     append([]string(nil), plan.NodeDraft.Triggers...),
				Capabilities: []string{plan.Capability},
				EvidenceRefs: append([]string(nil), plan.EvidenceRefs...),
				Weight:       plan.NodeDraft.Weight,
			},
		},
	}
	return manifest, nil
}

func RenderCapabilityEvaluationPlanReview(plan CapabilityEvaluationPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Capability Evaluation Intake: %s\n\n", plan.Capability)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- evaluation_set: %s\n", plan.EvaluationSet)
	fmt.Fprintf(&b, "- evaluation: %s\n", plan.EvaluationID)
	fmt.Fprintf(&b, "- capability: %s\n", plan.Capability)
	fmt.Fprintf(&b, "- verdict: %s\n", plan.Verdict)
	if plan.RubricScore > 0 {
		fmt.Fprintf(&b, "- rubric_score: %.2f\n", plan.RubricScore)
	}
	if plan.SourceReceipt != "" {
		fmt.Fprintf(&b, "- source_receipt: %s\n", plan.SourceReceipt)
	}
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate CapabilityGraph, install skill files, execute tools/MCPs, surface offers, or promote memory truth.")
	fmt.Fprintln(&b, "The sibling skill-manifest draft uses status `needs_review`, so importing it remains non-surfacing.")
	fmt.Fprintln(&b, "Use the sibling review YAML and `compound capability evaluation-review <review.yaml>` to produce a reviewed active manifest.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Draft CapabilityGraph Node")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- id: %s\n", plan.NodeDraft.NodeID)
	fmt.Fprintf(&b, "- kind: %s\n", plan.NodeDraft.Kind)
	fmt.Fprintf(&b, "- name: %s\n", plan.NodeDraft.Name)
	fmt.Fprintf(&b, "- triggers: %s\n", strings.Join(plan.NodeDraft.Triggers, ", "))
	fmt.Fprintf(&b, "- weight: %.2f\n", plan.NodeDraft.Weight)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Import")
	fmt.Fprintln(&b)
	for _, evidence := range plan.NodeDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Review")
	fmt.Fprintln(&b)
	for _, item := range plan.RequiredReview {
		fmt.Fprintf(&b, "- %s\n", item)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.NodeDraft.ReviewNote)
	return b.String()
}

func RenderCapabilityEvaluationReviewYAMLDraft(plan CapabilityEvaluationPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", CapabilityEvaluationReviewSchemaVersion)
	fmt.Fprintf(&b, "review_id: review_%s\n", plan.ID)
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintf(&b, "evaluation_id: %s\n", plan.EvaluationID)
	fmt.Fprintf(&b, "capability: %s\n", plan.Capability)
	fmt.Fprintln(&b, "decision: TODO_activate_reject_or_keep_needs_review")
	fmt.Fprintln(&b, "rationale: \"\"")
	fmt.Fprintln(&b, "confidence: -1")
	fmt.Fprintln(&b, "node_draft:")
	fmt.Fprintf(&b, "  node_id: %s\n", plan.NodeDraft.NodeID)
	fmt.Fprintf(&b, "  kind: %s\n", plan.NodeDraft.Kind)
	fmt.Fprintf(&b, "  name: %q\n", plan.NodeDraft.Name)
	fmt.Fprintln(&b, "  triggers:")
	for _, trigger := range plan.NodeDraft.Triggers {
		fmt.Fprintf(&b, "    - %s\n", trigger)
	}
	fmt.Fprintf(&b, "  weight: %.3g\n", plan.NodeDraft.Weight)
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "  - %s\n", ref)
	}
	return b.String()
}

func selectCapabilityEvaluation(set CapabilityEvaluationSet, selector string) (CapabilityEvaluation, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return CapabilityEvaluation{}, fmt.Errorf("--capability is required")
	}
	if len(set.Evaluations) == 0 {
		return CapabilityEvaluation{}, fmt.Errorf("capability evaluation set has no evaluations")
	}
	var matches []CapabilityEvaluation
	for _, eval := range set.Evaluations {
		if err := eval.Validate(); err != nil {
			return CapabilityEvaluation{}, err
		}
		if capabilityEvaluationMatches(eval, selector) {
			matches = append(matches, eval)
		}
	}
	if len(matches) == 0 {
		return CapabilityEvaluation{}, fmt.Errorf("capability evaluation %q not found", selector)
	}
	if len(matches) > 1 {
		return CapabilityEvaluation{}, fmt.Errorf("capability evaluation %q is ambiguous (%d matches)", selector, len(matches))
	}
	return matches[0], nil
}

func capabilityEvaluationMatches(eval CapabilityEvaluation, selector string) bool {
	candidates := []string{
		eval.Capability,
		eval.ID,
		capabilityEvaluationNodeID(eval),
		"capability://" + eval.Capability,
	}
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate), selector) {
			return true
		}
	}
	return false
}

func capabilityEvaluationPlanEvidence(set CapabilityEvaluationSet, eval CapabilityEvaluation) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, ref)
	}
	add("capability_evaluation_set:" + set.ID)
	if set.Revision != "" {
		add("source_revision:" + set.Revision)
	}
	for _, ref := range set.EvidenceRefs {
		if strings.Contains(ref, eval.ID) {
			add(ref)
		}
	}
	add("capability_evaluation:" + eval.ID)
	if eval.SourceReceipt != "" {
		add("source_receipt:" + eval.SourceReceipt)
	}
	sort.Strings(out)
	return out
}

func capabilityEvaluationReviewDecisionOK(decision string) bool {
	switch strings.TrimSpace(decision) {
	case "activate", "reject", "keep_needs_review":
		return true
	default:
		return false
	}
}

func capabilityEvaluationReviewNodeStatus(decision string) string {
	switch strings.TrimSpace(decision) {
	case "activate":
		return "active"
	case "reject":
		return "retired"
	default:
		return "needs_review"
	}
}

func suggestedCapabilityKind(eval CapabilityEvaluation) string {
	joined := strings.ToLower(strings.Join([]string{
		eval.Capability,
		eval.Verdict,
		signalString(eval.Signals, "kind"),
		signalString(eval.Signals, "cluster"),
		signalString(eval.Signals, "source_family"),
	}, " "))
	switch {
	case strings.Contains(joined, "mcp"):
		return "mcp"
	case strings.Contains(joined, "tool") || strings.Contains(joined, "cli"):
		return "tool"
	case strings.Contains(joined, "script"):
		return "script"
	case strings.Contains(joined, "procedure") || strings.Contains(joined, "workflow"):
		return "procedure"
	case strings.Contains(joined, "model"):
		return "model_profile"
	case strings.Contains(joined, "lens"):
		return "lens"
	default:
		return "skill"
	}
}

func signalString(signals map[string]any, key string) string {
	if signals == nil {
		return ""
	}
	value, ok := signals[key]
	if !ok {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

func signalStringSlice(signals map[string]any, key string) []string {
	if signals == nil {
		return nil
	}
	value, ok := signals[key]
	if !ok {
		return nil
	}
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func humanCapabilityName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Unknown capability"
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == '.'
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}
