package capgraph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const (
	SkillCandidateReviewSchemaVersion   = "compound.capability.skill_candidate_review.v0.1"
	SkillCandidateDecisionSchemaVersion = "compound.capability.skill_candidate_decision.v0.1"
	skillCandidateReviewArea            = "skill-candidates"
)

// ExternalSkillCandidate is a source-catalog skill record before it becomes a
// Compound inbox candidate, graph node, staged skill, or served adapter.
type ExternalSkillCandidate struct {
	SchemaVersion string                     `json:"schema_version,omitempty"`
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Slug          string                     `json:"slug,omitempty"`
	Description   string                     `json:"description,omitempty"`
	Source        string                     `json:"source,omitempty"`
	SourceType    string                     `json:"source_type,omitempty"`
	InstallURL    string                     `json:"install_url,omitempty"`
	URL           string                     `json:"url,omitempty"`
	RepositoryURL string                     `json:"repository_url,omitempty"`
	Hash          string                     `json:"hash,omitempty"`
	Installs      int                        `json:"installs,omitempty"`
	IsDuplicate   bool                       `json:"is_duplicate,omitempty"`
	Official      bool                       `json:"official,omitempty"`
	Topics        []string                   `json:"topics,omitempty"`
	Agents        []string                   `json:"agents,omitempty"`
	Tags          []string                   `json:"tags,omitempty"`
	Files         []ExternalSkillFile        `json:"files,omitempty"`
	Audits        []ExternalSkillAudit       `json:"audits,omitempty"`
	Metadata      map[string]json.RawMessage `json:"metadata,omitempty"`
}

type ExternalSkillFile struct {
	Path     string `json:"path"`
	Contents string `json:"contents,omitempty"`
}

type ExternalSkillAudit struct {
	Provider  string `json:"provider"`
	Status    string `json:"status"`
	Summary   string `json:"summary,omitempty"`
	RiskLevel string `json:"risk_level,omitempty"`
	AuditedAt string `json:"audited_at,omitempty"`
}

type SkillCandidateReview struct {
	SchemaVersion          string                  `json:"schema_version"`
	ID                     string                  `json:"id"`
	CreatedAt              string                  `json:"created_at"`
	Status                 string                  `json:"status"`
	CandidateKind          string                  `json:"candidate_kind"`
	CandidateID            string                  `json:"candidate_id"`
	SourcePath             string                  `json:"source_path"`
	SourceExists           bool                    `json:"source_exists"`
	SourceHash             string                  `json:"source_hash,omitempty"`
	TargetPath             string                  `json:"target_path,omitempty"`
	TargetExists           bool                    `json:"target_exists"`
	TargetHash             string                  `json:"target_hash,omitempty"`
	External               ExternalSkillCandidate  `json:"external"`
	ProposedClassification string                  `json:"proposed_classification"`
	LifecycleStage         string                  `json:"lifecycle_stage"`
	RecommendedDisposition string                  `json:"recommended_disposition"`
	DispositionReason      string                  `json:"disposition_reason"`
	GraphCoherence         SkillCandidateCoherence `json:"graph_coherence"`
	OverlapRefs            []SkillCandidateOverlap `json:"overlap_refs,omitempty"`
	AuditSignals           []SkillCandidateAudit   `json:"audit_signals,omitempty"`
	EvidenceRequired       []string                `json:"evidence_required"`
	EvalFixtureSuggestions []string                `json:"eval_fixture_suggestions"`
	NextAction             string                  `json:"next_action"`
	Warnings               []string                `json:"warnings"`
}

type SkillCandidateCoherence struct {
	DecisionHint       string   `json:"decision_hint"`
	DecisionConfidence float64  `json:"decision_confidence"`
	AdditiveFit        float64  `json:"additive_fit"`
	DuplicateRisk      float64  `json:"duplicate_risk"`
	EvidenceStrength   float64  `json:"evidence_strength"`
	AuditRisk          string   `json:"audit_risk"`
	TopOverlapScore    int      `json:"top_overlap_score,omitempty"`
	Reasons            []string `json:"reasons,omitempty"`
}

type SkillCandidateOverlap struct {
	NodeID        string   `json:"node_id"`
	Kind          string   `json:"kind"`
	Name          string   `json:"name"`
	MatchedTokens []string `json:"matched_tokens"`
	Score         int      `json:"score"`
	Retired       bool     `json:"retired,omitempty"`
	EvidenceOnly  bool     `json:"evidence_only,omitempty"`
}

type SkillCandidateAudit struct {
	Provider  string `json:"provider"`
	Status    string `json:"status"`
	RiskLevel string `json:"risk_level,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

type SkillCandidateReviewPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

type SkillCandidateDecision struct {
	SchemaVersion    string   `json:"schema_version" yaml:"schema_version"`
	ID               string   `json:"id" yaml:"id"`
	ReviewID         string   `json:"review_id" yaml:"review_id"`
	Decision         string   `json:"decision" yaml:"decision"`
	Rationale        string   `json:"rationale" yaml:"rationale"`
	Confidence       float64  `json:"confidence" yaml:"confidence"`
	CandidateVerdict string   `json:"candidate_verdict,omitempty" yaml:"candidate_verdict,omitempty"`
	EvidenceRefs     []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt       string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
}

type SkillCandidateDecisionReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	ReviewID      string   `json:"review_id"`
	DecisionID    string   `json:"decision_id"`
	Decision      string   `json:"decision"`
	CandidateID   string   `json:"candidate_id,omitempty"`
	CandidatePath string   `json:"candidate_path,omitempty"`
	ReviewPath    string   `json:"review_path,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

func LoadExternalSkillCandidate(path string) (ExternalSkillCandidate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ExternalSkillCandidate{}, fmt.Errorf("read external skill candidate %s: %w", path, err)
	}
	var candidate ExternalSkillCandidate
	if err := json.Unmarshal(data, &candidate); err != nil {
		return ExternalSkillCandidate{}, fmt.Errorf("parse external skill candidate %s: %w", path, err)
	}
	if err := candidate.Validate(); err != nil {
		return ExternalSkillCandidate{}, fmt.Errorf("validate external skill candidate %s: %w", path, err)
	}
	return candidate, nil
}

func LoadSkillCandidateReview(path string) (SkillCandidateReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillCandidateReview{}, fmt.Errorf("read skill candidate review %s: %w", path, err)
	}
	var review SkillCandidateReview
	if err := json.Unmarshal(data, &review); err != nil {
		return SkillCandidateReview{}, fmt.Errorf("parse skill candidate review %s: %w", path, err)
	}
	if strings.TrimSpace(review.ID) == "" {
		return SkillCandidateReview{}, fmt.Errorf("skill candidate review %s missing id", path)
	}
	return review, nil
}

func (c ExternalSkillCandidate) Validate() error {
	if strings.TrimSpace(c.ID) == "" && strings.TrimSpace(c.Name) == "" && strings.TrimSpace(c.Slug) == "" {
		return fmt.Errorf("id, name, or slug required")
	}
	if strings.TrimSpace(c.Source) == "" && strings.TrimSpace(c.URL) == "" && strings.TrimSpace(c.InstallURL) == "" && strings.TrimSpace(c.RepositoryURL) == "" {
		return fmt.Errorf("source, url, install_url, or repository_url required")
	}
	for _, file := range c.Files {
		clean := filepath.Clean(file.Path)
		if strings.TrimSpace(file.Path) == "" || clean == "." || strings.HasPrefix(filepath.ToSlash(clean), "../") || filepath.IsAbs(file.Path) {
			return fmt.Errorf("skill file path must be relative, got %q", file.Path)
		}
	}
	return nil
}

func BuildExternalSkillCandidateReview(homePath, candidatePath, targetPath string, now time.Time) (SkillCandidateReview, error) {
	candidate, err := LoadExternalSkillCandidate(candidatePath)
	if err != nil {
		return SkillCandidateReview{}, err
	}
	sourceExists, sourceHash, err := reviewartifact.HashFileTarget(candidatePath)
	if err != nil {
		return SkillCandidateReview{}, err
	}
	targetExists, targetHash, err := hashCandidateTarget(targetPath)
	if err != nil {
		return SkillCandidateReview{}, err
	}
	overlaps, err := skillCandidateOverlaps(homePath, candidate)
	if err != nil {
		return SkillCandidateReview{}, err
	}
	classification := classifyExternalSkillCandidate(candidate)
	disposition, reason := recommendExternalSkillCandidateDisposition(candidate, overlaps)
	auditSignals := externalSkillAuditSignals(candidate)
	candidateID := externalSkillCandidateID(candidate)
	review := SkillCandidateReview{
		SchemaVersion:          SkillCandidateReviewSchemaVersion,
		ID:                     reviewartifact.ID("scr_", sourceHash, candidateID, targetPath),
		CreatedAt:              now.UTC().Format(time.RFC3339),
		Status:                 "review_required",
		CandidateKind:          string(candidates.Skill),
		CandidateID:            candidateID,
		SourcePath:             candidatePath,
		SourceExists:           sourceExists,
		SourceHash:             sourceHash,
		TargetPath:             strings.TrimSpace(targetPath),
		TargetExists:           targetExists,
		TargetHash:             targetHash,
		External:               candidate,
		ProposedClassification: classification,
		LifecycleStage:         "staged_candidate",
		RecommendedDisposition: disposition,
		DispositionReason:      reason,
		GraphCoherence:         skillCandidateCoherence(candidate, overlaps, auditSignals, targetExists),
		OverlapRefs:            overlaps,
		AuditSignals:           auditSignals,
		EvidenceRequired: []string{
			"source provenance and license reviewed",
			"duplicate scan against existing CapabilityGraph and served skill roots",
			"deterministic tool/MCP/CLI replacement check completed",
			"trigger scope narrow enough for description-match surfacing",
			"security audit or manual content review recorded",
			"one eval fixture proves the skill changes a real outcome",
			"rollback/TTL for staged candidate recorded before serving",
		},
		EvalFixtureSuggestions: evalFixtureSuggestions(candidate),
		NextAction:             "review this artifact; only after review create a skill inbox candidate or staged non-served skill folder",
		Warnings: []string{
			"review-only: does not mutate CapabilityGraph",
			"does not install or serve external skill files",
			"does not execute tools, MCPs, or marketplace CLIs",
			"does not promote memory truth",
		},
	}
	return review, nil
}

func RecordExternalSkillCandidateReview(homePath string, review SkillCandidateReview) (SkillCandidateReviewPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return SkillCandidateReviewPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(review.ID) == "" {
		return SkillCandidateReviewPaths{}, fmt.Errorf("skill candidate review id required")
	}
	dir, err := reviewartifact.Dir(homePath, skillCandidateReviewArea)
	if err != nil {
		return SkillCandidateReviewPaths{}, err
	}
	out := SkillCandidateReviewPaths{
		PlanPath:       filepath.Join(dir, review.ID+".json"),
		ReviewPath:     filepath.Join(dir, review.ID+".md"),
		ReviewYAMLPath: filepath.Join(dir, review.ID+".review.yaml"),
	}
	report, hasDecision := skillCandidateDecisionReportForReview(dir, review.ID)
	if hasDecision {
		review.Status = report.Status
		if report.CandidateID != "" {
			review.NextAction = "reviewed skill candidate recorded; continue with capability skill-plan"
		} else {
			review.NextAction = "skill candidate intake decision recorded"
		}
	}
	if _, err := reviewartifact.Write(homePath, skillCandidateReviewArea, review.ID, review, " ", RenderExternalSkillCandidateReview(review)); err != nil {
		return out, err
	}
	if hasDecision {
		return out, nil
	}
	if _, err := reviewartifact.WriteRaw(homePath, skillCandidateReviewArea, review.ID+".review.yaml", []byte(RenderExternalSkillCandidateDecisionYAMLDraft(review))); err != nil {
		return out, err
	}
	return out, nil
}

func skillCandidateDecisionReportForReview(dir, reviewID string) (SkillCandidateDecisionReport, bool) {
	matches, err := filepath.Glob(filepath.Join(dir, reviewID+".*.decision.json"))
	if err != nil {
		return SkillCandidateDecisionReport{}, false
	}
	return skillCandidateDecisionReportFromExtras(matches)
}

func RenderExternalSkillCandidateDecisionYAMLDraft(review SkillCandidateReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", SkillCandidateDecisionSchemaVersion)
	fmt.Fprintf(&b, "id: decision_%s\n", review.ID)
	fmt.Fprintf(&b, "review_id: %s\n", review.ID)
	fmt.Fprintln(&b, "decision: TODO_decision_create_candidate_merge_existing_reject_reference_only_keep_needs_review")
	fmt.Fprintln(&b, "rationale: \"\"")
	fmt.Fprintln(&b, "confidence: -1")
	fmt.Fprintln(&b, "candidate_verdict: park")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range externalSkillCandidateDecisionEvidence(review) {
		fmt.Fprintf(&b, " - %s\n", ref)
	}
	return b.String()
}

func LoadSkillCandidateDecision(path string) (SkillCandidateDecision, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillCandidateDecision{}, fmt.Errorf("read skill candidate decision %s: %w", path, err)
	}
	var decision SkillCandidateDecision
	if err := yaml.Unmarshal(data, &decision); err != nil {
		return SkillCandidateDecision{}, fmt.Errorf("parse skill candidate decision %s: %w", path, err)
	}
	if err := ValidateSkillCandidateDecision(decision); err != nil {
		return SkillCandidateDecision{}, fmt.Errorf("validate skill candidate decision %s: %w", path, err)
	}
	return decision, nil
}

func ValidateSkillCandidateDecision(decision SkillCandidateDecision) error {
	if strings.TrimSpace(decision.SchemaVersion) != SkillCandidateDecisionSchemaVersion {
		return fmt.Errorf("schema_version must be %s", SkillCandidateDecisionSchemaVersion)
	}
	if strings.TrimSpace(decision.ID) == "" {
		return fmt.Errorf("skill candidate decision id required")
	}
	if strings.TrimSpace(decision.ReviewID) == "" {
		return fmt.Errorf("skill candidate decision review_id required")
	}
	if !skillCandidateDecisionOK(decision.Decision) {
		return fmt.Errorf("skill candidate decision must be create_candidate|merge_existing|reject|reference_only|keep_needs_review")
	}
	if strings.TrimSpace(decision.Rationale) == "" {
		return fmt.Errorf("skill candidate decision rationale required")
	}
	if decision.Confidence < 0 || decision.Confidence > 1 {
		return fmt.Errorf("skill candidate decision confidence must be between 0 and 1")
	}
	if len(reviewartifact.Unique(decision.EvidenceRefs)) == 0 {
		return fmt.Errorf("skill candidate decision evidence_refs required")
	}
	if strings.TrimSpace(decision.CandidateVerdict) == "" {
		decision.CandidateVerdict = candidates.Park
	}
	if decision.Decision == "create_candidate" && !skillCandidateVerdictOK(decision.CandidateVerdict) {
		return fmt.Errorf("candidate_verdict must be park or operationalize for create_candidate")
	}
	return nil
}

func RecordSkillCandidateDecision(homePath string, decision SkillCandidateDecision, sourcePath string, now time.Time) (SkillCandidateDecisionReport, error) {
	createdAt := reviewartifact.Timestamp(now)
	report := SkillCandidateDecisionReport{
		SchemaVersion: "compound.capability.skill_candidate_decision_report.v0.1",
		ID:            reviewartifact.ID("scdr_", decision.ReviewID, decision.ID, decision.Decision, createdAt),
		CreatedAt:     createdAt,
		Status:        "recorded",
		ReviewID:      decision.ReviewID,
		DecisionID:    decision.ID,
		Decision:      decision.Decision,
		ReviewPath:    strings.TrimSpace(sourcePath),
		EvidenceRefs:  reviewartifact.Unique(decision.EvidenceRefs),
		Warnings: []string{
			"does not mutate CapabilityGraph",
			"does not install or serve skill files",
			"does not execute tools, MCPs, or marketplace CLIs",
			"does not promote memory claims",
		},
	}
	if err := ValidateSkillCandidateDecision(decision); err != nil {
		report.Status = "blocked"
		return report, err
	}
	review, err := loadSkillCandidateReviewForDecision(sourcePath, decision.ReviewID)
	if err != nil {
		report.Status = "blocked"
		return report, err
	}
	if decision.Decision == "create_candidate" {
		candidate := skillCandidate{
			ID:           review.CandidateID,
			Kind:         string(candidates.Skill),
			Subject:      review.External.displayName(),
			Statement:    firstEvalNonEmpty(review.External.Description, review.External.displayName()),
			Detail:       firstEvalNonEmpty(review.ProposedClassification, review.RecommendedDisposition),
			Confidence:   decision.Confidence,
			EvidenceRefs: reviewartifact.Unique(append(externalSkillCandidateDecisionEvidence(review), decision.EvidenceRefs...)),
			Verdict:      firstEvalNonEmpty(decision.CandidateVerdict, candidates.Park),
		}
		if err := candidates.WriteCandidate(homePath, candidates.Skill, candidate.ID, candidate); err != nil {
			report.Status = "blocked"
			return report, err
		}
		report.Status = "candidate_recorded"
		report.CandidateID = candidate.ID
		report.CandidatePath = filepath.Join(homePath, candidates.InboxDir(candidates.Skill), candidate.ID+".json")
	}
	report.EvidenceRefs = reviewartifact.Unique(append(report.EvidenceRefs, "skill_candidate_review:"+review.ID))
	name := decision.ReviewID + "." + decision.ID + ".decision.json"
	if _, err := reviewartifact.WriteJSON(homePath, skillCandidateReviewArea, name, report, " "); err != nil {
		return report, err
	}
	review.Status = report.Status
	if report.CandidateID != "" {
		review.NextAction = "reviewed skill candidate recorded; continue with capability skill-plan"
	} else {
		review.NextAction = "skill candidate intake decision recorded"
	}
	if _, err := reviewartifact.WriteJSON(homePath, skillCandidateReviewArea, review.ID+".json", review, " "); err != nil {
		return report, err
	}
	return report, nil
}

func RenderExternalSkillCandidateReview(review SkillCandidateReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Skill Candidate Intake: %s\n\n", review.External.displayName())
	fmt.Fprintf(&b, "- status: `%s`\n", review.Status)
	fmt.Fprintf(&b, "- candidate_kind: `%s`\n", review.CandidateKind)
	fmt.Fprintf(&b, "- candidate_id: `%s`\n", review.CandidateID)
	fmt.Fprintf(&b, "- source: %s\n", firstEvalNonEmpty(review.External.URL, review.External.InstallURL, review.External.RepositoryURL, review.External.Source))
	fmt.Fprintf(&b, "- proposed_classification: `%s`\n", review.ProposedClassification)
	fmt.Fprintf(&b, "- lifecycle_stage: `%s`\n", review.LifecycleStage)
	fmt.Fprintf(&b, "- recommended_disposition: `%s`\n", review.RecommendedDisposition)
	fmt.Fprintf(&b, "- reason: %s\n", review.DispositionReason)
	if review.GraphCoherence.DecisionHint != "" {
		fmt.Fprintf(&b, "- graph_decision_hint: `%s` confidence=%.2f additive_fit=%.2f duplicate_risk=%.2f evidence=%.2f audit_risk=%s\n",
			review.GraphCoherence.DecisionHint,
			review.GraphCoherence.DecisionConfidence,
			review.GraphCoherence.AdditiveFit,
			review.GraphCoherence.DuplicateRisk,
			review.GraphCoherence.EvidenceStrength,
			review.GraphCoherence.AuditRisk,
		)
	}
	if review.External.Installs > 0 {
		fmt.Fprintf(&b, "- installs: %d\n", review.External.Installs)
	}
	if review.External.Hash != "" {
		fmt.Fprintf(&b, "- upstream_hash: `%s`\n", review.External.Hash)
	}
	if review.SourceHash != "" {
		fmt.Fprintf(&b, "- source_hash: `%s`\n", review.SourceHash)
	}
	if review.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", review.TargetPath)
	}
	if len(review.GraphCoherence.Reasons) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Graph Coherence")
		fmt.Fprintln(&b)
		for _, reason := range review.GraphCoherence.Reasons {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Overlap Scan")
	fmt.Fprintln(&b)
	if len(review.OverlapRefs) == 0 {
		fmt.Fprintln(&b, "- none found in current CapabilityGraph")
	} else {
		for _, overlap := range review.OverlapRefs {
			fmt.Fprintf(&b, "- `%s` (%s, %s) score=%d tokens=%s\n", overlap.NodeID, overlap.Kind, overlap.Name, overlap.Score, strings.Join(overlap.MatchedTokens, ","))
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Audit Signals")
	fmt.Fprintln(&b)
	if len(review.AuditSignals) == 0 {
		fmt.Fprintln(&b, "- none supplied; manual security/content review required")
	} else {
		for _, signal := range review.AuditSignals {
			fmt.Fprintf(&b, "- %s: %s", signal.Provider, signal.Status)
			if signal.RiskLevel != "" {
				fmt.Fprintf(&b, " (%s)", signal.RiskLevel)
			}
			if signal.Summary != "" {
				fmt.Fprintf(&b, " - %s", signal.Summary)
			}
			fmt.Fprintln(&b)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Evidence Required")
	fmt.Fprintln(&b)
	for _, evidence := range review.EvidenceRequired {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Eval Fixture Suggestions")
	fmt.Fprintln(&b)
	for _, suggestion := range review.EvalFixtureSuggestions {
		fmt.Fprintf(&b, "- %s\n", suggestion)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Boundaries")
	fmt.Fprintln(&b)
	for _, warning := range review.Warnings {
		fmt.Fprintf(&b, "- %s\n", warning)
	}
	return b.String()
}

func (c ExternalSkillCandidate) displayName() string {
	return firstEvalNonEmpty(c.Name, c.Slug, c.ID, c.Source)
}

func externalSkillCandidateID(c ExternalSkillCandidate) string {
	id := reviewartifact.Slugify(firstEvalNonEmpty(c.ID, c.Source+"/"+c.Slug, c.Name, c.Slug), 72)
	if id == "" {
		id = "external-" + reviewartifact.Digest12(c.Source+c.URL+c.InstallURL)
	}
	return id
}

func classifyExternalSkillCandidate(c ExternalSkillCandidate) string {
	text := strings.ToLower(strings.Join([]string{c.Name, c.Slug, c.Description, c.Source, strings.Join(c.Tags, " "), strings.Join(c.Topics, " ")}, " "))
	if hasSkillMD(c) {
		return "shared_skill"
	}
	if strings.Contains(text, "mcp") || strings.Contains(text, "cli") || strings.Contains(text, "tool") || strings.Contains(text, "api") {
		return "tool"
	}
	if strings.Contains(text, "recipe") || strings.Contains(text, "workflow") || strings.Contains(text, "doctrine") || strings.Contains(text, "playbook") {
		return "recipe"
	}
	if strings.Contains(text, "plugin") || strings.Contains(text, "marketplace") {
		return "plugin_or_marketplace"
	}
	return "staged_candidate"
}

func recommendExternalSkillCandidateDisposition(c ExternalSkillCandidate, overlaps []SkillCandidateOverlap) (string, string) {
	if c.IsDuplicate {
		return "reference_only", "source marks candidate as duplicate/fork; keep as provenance unless reviewer chooses merge"
	}
	for _, signal := range externalSkillAuditSignals(c) {
		status := strings.ToLower(signal.Status)
		risk := strings.ToLower(signal.RiskLevel)
		if status == "fail" || risk == "critical" || risk == "high" {
			return "reject", "external audit signal is high risk or failing"
		}
	}
	if len(overlaps) > 0 && overlaps[0].Score >= 2 {
		return "merge", "candidate overlaps existing CapabilityGraph capability; adapt useful parts into owner instead of serving a duplicate"
	}
	return "park", "external marketplace evidence is not runtime proof; keep staged until eval and review pass"
}

func skillCandidateCoherence(c ExternalSkillCandidate, overlaps []SkillCandidateOverlap, audits []SkillCandidateAudit, targetExists bool) SkillCandidateCoherence {
	topOverlap := 0
	if len(overlaps) > 0 {
		topOverlap = overlaps[0].Score
	}
	auditRisk, auditRiskScore := skillCandidateAuditRisk(audits)
	reasons := []string{}

	evidence := 0.10 // candidate file parsed and validated.
	if strings.TrimSpace(c.Description) != "" {
		evidence += 0.10
	}
	if hasSkillMD(c) {
		evidence += 0.20
		reasons = append(reasons, "candidate includes SKILL.md")
	}
	if strings.TrimSpace(firstEvalNonEmpty(c.RepositoryURL, c.InstallURL, c.URL, c.Source)) != "" {
		evidence += 0.10
	}
	if c.Official {
		evidence += 0.10
		reasons = append(reasons, "source marked official")
	}
	switch {
	case c.Installs >= 100000:
		evidence += 0.10
		reasons = append(reasons, "high install count is popularity evidence, not runtime proof")
	case c.Installs >= 1000:
		evidence += 0.05
		reasons = append(reasons, "install count is weak popularity evidence")
	}
	if len(audits) > 0 {
		evidence += 0.10
		reasons = append(reasons, "external audit signal supplied")
	}
	if len(audits) > 0 && auditRiskScore <= 0.20 {
		evidence += 0.10
		reasons = append(reasons, "audit signals are low risk")
	}
	if targetExists {
		evidence += 0.10
		reasons = append(reasons, "local target snapshot captured")
	}
	evidence = clamp01(evidence)

	duplicateRisk := 0.05
	switch {
	case c.IsDuplicate:
		duplicateRisk = 1
		reasons = append(reasons, "source marks candidate duplicate")
	case topOverlap >= 5:
		duplicateRisk = 0.95
	case topOverlap >= 3:
		duplicateRisk = 0.80
	case topOverlap >= 2:
		duplicateRisk = 0.65
	case topOverlap == 1:
		duplicateRisk = 0.35
	}
	if topOverlap > 0 {
		reasons = append(reasons, fmt.Sprintf("top overlap score %d against existing CapabilityGraph node", topOverlap))
	}

	additiveFit := clamp01(evidence*(1-duplicateRisk*0.70) - auditRiskScore*0.30)
	decisionHint := "keep_needs_review"
	decisionConfidence := 0.45
	switch {
	case auditRiskScore >= 0.85:
		decisionHint = "reject"
		decisionConfidence = auditRiskScore
		reasons = append(reasons, "audit risk is high enough to reject before graph intake")
	case c.IsDuplicate:
		decisionHint = "reference_only"
		decisionConfidence = duplicateRisk
	case duplicateRisk >= 0.60:
		decisionHint = "merge_existing"
		decisionConfidence = duplicateRisk
	case additiveFit >= 0.65 && evidence >= 0.70 && auditRiskScore <= 0.40:
		decisionHint = "create_candidate"
		decisionConfidence = additiveFit
	default:
		reasons = append(reasons, "needs eval evidence before graph or served-skill intake")
	}

	return SkillCandidateCoherence{
		DecisionHint:       decisionHint,
		DecisionConfidence: roundScore(decisionConfidence),
		AdditiveFit:        roundScore(additiveFit),
		DuplicateRisk:      roundScore(duplicateRisk),
		EvidenceStrength:   roundScore(evidence),
		AuditRisk:          auditRisk,
		TopOverlapScore:    topOverlap,
		Reasons:            reviewartifact.Unique(reasons),
	}
}

func skillCandidateAuditRisk(audits []SkillCandidateAudit) (string, float64) {
	if len(audits) == 0 {
		return "unknown", 0.45
	}
	label := "low"
	score := 0.10
	for _, audit := range audits {
		status := strings.ToLower(strings.TrimSpace(audit.Status))
		risk := strings.ToLower(strings.TrimSpace(audit.RiskLevel))
		curLabel, curScore := skillCandidateAuditRiskValue(status, risk)
		if curScore > score {
			label = curLabel
			score = curScore
		}
	}
	return label, score
}

func skillCandidateAuditRiskValue(status, risk string) (string, float64) {
	switch {
	case status == "fail" || risk == "critical":
		return "critical", 1
	case risk == "high":
		return "high", 0.90
	case status == "warn" || risk == "medium" || risk == "med":
		return "medium", 0.60
	case status == "pending" || status == "unknown" || risk == "unknown":
		return "unknown", 0.45
	case status == "pass" || status == "safe" || risk == "low":
		return "low", 0.10
	default:
		return "unknown", 0.45
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func roundScore(v float64) float64 {
	v = clamp01(v)
	return float64(int(v*100+0.5)) / 100
}

func externalSkillAuditSignals(c ExternalSkillCandidate) []SkillCandidateAudit {
	out := make([]SkillCandidateAudit, 0, len(c.Audits))
	for _, audit := range c.Audits {
		provider := strings.TrimSpace(audit.Provider)
		status := strings.TrimSpace(audit.Status)
		if provider == "" && status == "" {
			continue
		}
		out = append(out, SkillCandidateAudit{
			Provider:  firstEvalNonEmpty(provider, "unknown"),
			Status:    firstEvalNonEmpty(status, "unknown"),
			RiskLevel: strings.TrimSpace(audit.RiskLevel),
			Summary:   strings.TrimSpace(audit.Summary),
		})
	}
	return out
}

func evalFixtureSuggestions(c ExternalSkillCandidate) []string {
	name := c.displayName()
	task := strings.TrimSpace(c.Description)
	if task == "" {
		task = name
	}
	return []string{
		fmt.Sprintf("baseline prompt without candidate: complete a real %q task using current served skills", task),
		fmt.Sprintf("candidate prompt with %q staged as context only; compare output quality, specificity, and policy fit", name),
		"failure fixture: prompt near an owned overlapping skill and prove the candidate does not steal routing unless reviewed",
		"receipt fixture: record the exact state delta the candidate improved; popularity/install count is not success evidence",
	}
}

func hasSkillMD(c ExternalSkillCandidate) bool {
	for _, file := range c.Files {
		if strings.EqualFold(filepath.ToSlash(file.Path), "skill.md") || strings.HasSuffix(strings.ToLower(filepath.ToSlash(file.Path)), "/skill.md") {
			return true
		}
	}
	return false
}

func skillCandidateOverlaps(homePath string, c ExternalSkillCandidate) ([]SkillCandidateOverlap, error) {
	store, err := Open(homePath)
	if err != nil {
		return nil, err
	}
	nodes, err := store.List()
	if err != nil {
		return nil, err
	}
	candidateTokens := reviewTokenSet(c.ID, c.Name, c.Slug, c.Description, c.Source, strings.Join(c.Tags, " "), strings.Join(c.Topics, " "))
	var overlaps []SkillCandidateOverlap
	for _, node := range nodes {
		nodeTokens := reviewTokenSet(node.ID, node.Name, strings.Join(node.Triggers, " "))
		var matched []string
		for token := range candidateTokens {
			if nodeTokens[token] {
				matched = append(matched, token)
			}
		}
		sort.Strings(matched)
		score := len(matched)
		if sameSlug(node.Name, c.Name) || sameSlug(node.ID, c.ID) || sameSlug(node.ID, c.Slug) {
			score += 3
		}
		if score == 0 {
			continue
		}
		overlaps = append(overlaps, SkillCandidateOverlap{
			NodeID:        node.ID,
			Kind:          node.Kind,
			Name:          node.Name,
			MatchedTokens: matched,
			Score:         score,
			Retired:       node.Retired,
			EvidenceOnly:  node.EvidenceOnly,
		})
	}
	sort.Slice(overlaps, func(i, j int) bool {
		if overlaps[i].Score != overlaps[j].Score {
			return overlaps[i].Score > overlaps[j].Score
		}
		return overlaps[i].NodeID < overlaps[j].NodeID
	})
	if len(overlaps) > 8 {
		overlaps = overlaps[:8]
	}
	return overlaps, nil
}

func reviewTokenSet(values ...string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		for _, token := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			if len(token) < 3 {
				continue
			}
			out[token] = true
		}
	}
	return out
}

func sameSlug(a, b string) bool {
	as := reviewartifact.Slugify(a, 0)
	bs := reviewartifact.Slugify(b, 0)
	return as != "" && as == bs
}

func loadSkillCandidateReviewForDecision(reviewPath, reviewID string) (SkillCandidateReview, error) {
	if strings.TrimSpace(reviewID) == "" || reviewID != filepath.Base(reviewID) {
		return SkillCandidateReview{}, fmt.Errorf("skill candidate review_id must be bare id, got %q", reviewID)
	}
	dir := filepath.Dir(reviewPath)
	if strings.TrimSpace(dir) == "" || dir == "." {
		return SkillCandidateReview{}, fmt.Errorf("skill candidate decision source path required")
	}
	data, err := os.ReadFile(filepath.Join(dir, reviewID+".json"))
	if err != nil {
		return SkillCandidateReview{}, err
	}
	var review SkillCandidateReview
	if err := json.Unmarshal(data, &review); err != nil {
		return SkillCandidateReview{}, err
	}
	if review.ID != reviewID {
		return SkillCandidateReview{}, fmt.Errorf("skill candidate decision review_id %q does not match review %q", reviewID, review.ID)
	}
	return review, nil
}

func skillCandidateDecisionOK(decision string) bool {
	switch strings.TrimSpace(decision) {
	case "create_candidate", "merge_existing", "reject", "reference_only", "keep_needs_review":
		return true
	default:
		return false
	}
}

func skillCandidateVerdictOK(verdict string) bool {
	switch strings.TrimSpace(verdict) {
	case candidates.Park, candidates.Operationalize:
		return true
	default:
		return false
	}
}

func externalSkillCandidateDecisionEvidence(review SkillCandidateReview) []string {
	refs := []string{
		"skill_candidate_review:" + review.ID,
		"external_skill:" + review.CandidateID,
	}
	if review.SourceHash != "" {
		refs = append(refs, "sha256:"+review.SourceHash)
	}
	if review.External.URL != "" {
		refs = append(refs, review.External.URL)
	}
	if review.External.InstallURL != "" {
		refs = append(refs, review.External.InstallURL)
	}
	return reviewartifact.Unique(refs)
}

// AnnotateSkillCandidateQueue attaches capability-owned review hints to generic
// external skill candidate review artifacts. It never creates candidates,
// mutates graph state, installs skill files, or executes tools.
func AnnotateSkillCandidateQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != skillCandidateReviewArea {
			continue
		}
		annotateSkillCandidateQueueItem(item)
	}
}

func annotateSkillCandidateQueueItem(item *reviewartifact.QueueItem) {
	annotateSkillCandidateQueueCoherence(item)
	if report, ok := skillCandidateDecisionReportFromExtras(item.ExtraPaths); ok {
		switch report.Status {
		case "recorded", "candidate_recorded":
			item.Readiness = "no_action"
			item.BlockedReason = ""
			if report.CandidateID != "" {
				item.NextAction = "reviewed skill candidate recorded; continue with capability skill-plan"
			} else {
				item.NextAction = "skill candidate intake decision recorded"
			}
			item.Actions = nil
			return
		case "blocked":
			item.Readiness = "blocked"
			item.BlockedReason = "skill candidate decision blocked"
			item.NextAction = "fix reviewed skill candidate decision"
			item.Actions = nil
			return
		}
	}
	// Terminal words are owned by reviewartifact.TerminalStatus; this used to
	// re-list them inline and its stale copy omitted "completed", billing
	// finished work to the operator as a pending decision.
	if reviewartifact.TerminalStatus(item.Status) {
		item.Readiness = "no_action"
		item.NextAction = "skill candidate review artifact terminal"
		return
	}
	item.Readiness = "needs_review"
	item.NextAction = "fill review YAML after source/eval review, then record skill candidate decision"
	reviewYAML := skillCandidateDecisionYAMLPath(*item)
	if reviewYAML == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft missing"
		item.NextAction = "review YAML draft missing; regenerate skill-candidate-review"
		return
	}
	argv := []string{"compound", "capability", "skill-candidate-decision", reviewYAML}
	item.Actions = []reviewartifact.Action{{
		ID:             "record_skill_candidate_decision",
		Label:          "Record skill candidate decision",
		Command:        skillCandidateShellJoin(argv),
		Argv:           argv,
		RequiresReview: true,
	}}
	decision, err := LoadSkillCandidateDecision(reviewYAML)
	if err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft unreadable: " + skillCandidateQueueError(err)
		item.NextAction = "fix review YAML draft before recording skill candidate decision"
		return
	}
	if err := ValidateSkillCandidateDecision(decision); err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = skillCandidateQueueError(err)
		item.NextAction = "fill valid review YAML before recording skill candidate decision"
		return
	}
	item.Readiness = "ready_to_record"
	item.BlockedReason = ""
	item.NextAction = "record reviewed skill candidate decision"
}

func annotateSkillCandidateQueueCoherence(item *reviewartifact.QueueItem) {
	if item == nil || strings.TrimSpace(item.JSONPath) == "" {
		return
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return
	}
	var review SkillCandidateReview
	if err := json.Unmarshal(data, &review); err != nil {
		return
	}
	item.GraphDecisionHint = review.GraphCoherence.DecisionHint
	item.GraphDecisionConfidence = review.GraphCoherence.DecisionConfidence
	item.GraphDuplicateRisk = review.GraphCoherence.DuplicateRisk
	item.GraphEvidenceStrength = review.GraphCoherence.EvidenceStrength
	item.GraphAuditRisk = review.GraphCoherence.AuditRisk
	item.GraphTopOverlapScore = review.GraphCoherence.TopOverlapScore
}

func skillCandidateDecisionReportFromExtras(paths []string) (SkillCandidateDecisionReport, bool) {
	var found SkillCandidateDecisionReport
	for _, path := range paths {
		if !strings.HasSuffix(path, ".decision.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var report SkillCandidateDecisionReport
		if err := json.Unmarshal(data, &report); err != nil {
			continue
		}
		if strings.TrimSpace(report.ID) == "" {
			continue
		}
		if found.ID == "" || report.CreatedAt > found.CreatedAt {
			found = report
		}
	}
	return found, found.ID != ""
}

func skillCandidateDecisionYAMLPath(item reviewartifact.QueueItem) string {
	if strings.TrimSpace(item.JSONPath) == "" {
		return ""
	}
	return strings.TrimSuffix(item.JSONPath, ".json") + ".review.yaml"
}

func skillCandidateQueueError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 240 {
		msg = msg[:237] + "..."
	}
	return msg
}

func skillCandidateShellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, skillCandidateShellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func skillCandidateShellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r == '/' || r == '-' || r == '_' || r == '.' || r == ':' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func hashCandidateTarget(path string) (bool, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if info.IsDir() {
		return reviewartifact.HashTreeTarget(path)
	}
	return reviewartifact.HashFileTarget(path)
}
