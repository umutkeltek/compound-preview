package distiller

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
	ImprovementPlanSchemaVersion           = "compound.distiller.improvement_plan.v0.1"
	ImprovementReviewSchemaVersion         = "compound.distiller.improvement_review.v0.1"
	ReviewedImprovementSchemaVersion       = "compound.distiller.reviewed_improvement.v0.1"
	ImprovementReviewArea                  = "distiller-improvements"
	ImprovementReviewDecisionAccept        = "accept"
	ImprovementReviewDecisionReject        = "reject"
	ImprovementReviewDecisionKeepReviewing = "keep_reviewing"
)

// ImprovementPlan is a reviewed proposal from method_upgrade or policy_patch
// candidates. It makes distiller self-improvement explicit without silently
// editing prompts, policy, code, eval corpus, or memory truth.
type ImprovementPlan struct {
	SchemaVersion    string           `json:"schema_version"`
	ID               string           `json:"id"`
	CreatedAt        string           `json:"created_at"`
	Status           string           `json:"status"`
	CandidateID      string           `json:"candidate_id"`
	CandidateKind    string           `json:"candidate_kind"`
	CandidateVerdict string           `json:"candidate_verdict,omitempty"`
	ImprovementKey   string           `json:"improvement_key"`
	Subject          string           `json:"subject"`
	Statement        string           `json:"statement"`
	Detail           string           `json:"detail,omitempty"`
	Confidence       float64          `json:"confidence,omitempty"`
	EvidenceRefs     []string         `json:"evidence_refs"`
	TargetPath       string           `json:"target_path,omitempty"`
	TargetExists     bool             `json:"target_exists"`
	TargetHash       string           `json:"target_hash,omitempty"`
	Draft            ImprovementDraft `json:"draft"`
	Warnings         []string         `json:"warnings,omitempty"`
}

type ImprovementDraft struct {
	ImprovementID    string   `json:"improvement_id" yaml:"improvement_id"`
	Area             string   `json:"area" yaml:"area"`
	Title            string   `json:"title" yaml:"title"`
	ProposedChange   string   `json:"proposed_change" yaml:"proposed_change"`
	Guardrail        string   `json:"guardrail" yaml:"guardrail"`
	EvalRequirement  string   `json:"eval_requirement" yaml:"eval_requirement"`
	RollbackPlan     string   `json:"rollback_plan" yaml:"rollback_plan"`
	RequiredEvidence []string `json:"required_evidence" yaml:"required_evidence"`
	ReviewNote       string   `json:"review_note" yaml:"review_note"`
}

type ImprovementPlanPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

type ImprovementReview struct {
	SchemaVersion string           `json:"schema_version" yaml:"schema_version"`
	ReviewID      string           `json:"review_id" yaml:"review_id"`
	PlanID        string           `json:"plan_id" yaml:"plan_id"`
	CandidateID   string           `json:"candidate_id" yaml:"candidate_id"`
	CandidateKind string           `json:"candidate_kind" yaml:"candidate_kind"`
	ImprovementID string           `json:"improvement_id" yaml:"improvement_id"`
	Area          string           `json:"area" yaml:"area"`
	Decision      string           `json:"decision" yaml:"decision"`
	Rationale     string           `json:"rationale" yaml:"rationale"`
	Confidence    float64          `json:"confidence" yaml:"confidence"`
	EvidenceRefs  []string         `json:"evidence_refs" yaml:"evidence_refs"`
	EvalProofRefs []string         `json:"eval_proof_refs" yaml:"eval_proof_refs"`
	TargetPath    string           `json:"target_path,omitempty" yaml:"target_path,omitempty"`
	TargetHash    string           `json:"target_hash,omitempty" yaml:"target_hash,omitempty"`
	Draft         ImprovementDraft `json:"draft" yaml:"draft"`
}

type ReviewedImprovement struct {
	SchemaVersion string           `json:"schema_version"`
	ID            string           `json:"id"`
	CreatedAt     string           `json:"created_at"`
	Status        string           `json:"status"`
	ReviewID      string           `json:"review_id"`
	PlanID        string           `json:"plan_id"`
	CandidateID   string           `json:"candidate_id"`
	CandidateKind string           `json:"candidate_kind"`
	ImprovementID string           `json:"improvement_id"`
	Area          string           `json:"area"`
	Decision      string           `json:"decision"`
	Rationale     string           `json:"rationale"`
	Confidence    float64          `json:"confidence"`
	EvidenceRefs  []string         `json:"evidence_refs"`
	EvalProofRefs []string         `json:"eval_proof_refs,omitempty"`
	TargetPath    string           `json:"target_path,omitempty"`
	TargetHash    string           `json:"target_hash,omitempty"`
	Draft         ImprovementDraft `json:"draft"`
	Warnings      []string         `json:"warnings,omitempty"`
}

type ImprovementReviewReport struct {
	Status       string
	ReviewID     string
	Decision     string
	ReviewedPath string
}

func BuildImprovementPlan(homePath, candidateID, targetPath string, now time.Time) (ImprovementPlan, error) {
	homePath = strings.TrimSpace(homePath)
	candidateID = strings.TrimSpace(candidateID)
	targetPath = strings.TrimSpace(targetPath)
	if homePath == "" {
		return ImprovementPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if candidateID == "" {
		return ImprovementPlan{}, fmt.Errorf("candidate id required")
	}
	c, err := readImprovementCandidate(homePath, candidateID)
	if err != nil {
		return ImprovementPlan{}, err
	}
	if c.Kind != KindMethodUpgrade && c.Kind != KindPolicyPatch {
		return ImprovementPlan{}, fmt.Errorf("candidate %s kind=%q, want %q or %q", candidateID, c.Kind, KindMethodUpgrade, KindPolicyPatch)
	}
	if c.Verdict != Park && c.Verdict != Operationalize && c.Verdict != candidates.NoPromote {
		return ImprovementPlan{}, fmt.Errorf("candidate %s verdict=%q, want park, operationalize, or no_promote", candidateID, c.Verdict)
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return ImprovementPlan{}, err
	}
	evidenceRefs := append([]string(nil), c.EvidenceRefs...)
	sort.Strings(evidenceRefs)
	key := reviewartifact.Slugify(reviewartifact.FirstNonEmpty(c.Subject, c.Detail, c.Statement, c.ID), 48)
	if key == "" {
		key = reviewartifact.Digest12(candidateID)
	}
	area := "method"
	if c.Kind == KindPolicyPatch {
		area = "policy"
	}
	statement := reviewartifact.FirstNonEmpty(c.Statement, c.Subject, "reviewed distiller improvement")
	plan := ImprovementPlan{
		SchemaVersion:    ImprovementPlanSchemaVersion,
		ID:               reviewartifact.ID("imp_", candidateID, targetPath),
		CreatedAt:        rfc3339(now),
		Status:           "review_required",
		CandidateID:      candidateID,
		CandidateKind:    c.Kind,
		CandidateVerdict: c.Verdict,
		ImprovementKey:   key,
		Subject:          c.Subject,
		Statement:        c.Statement,
		Detail:           c.Detail,
		Confidence:       c.Confidence,
		EvidenceRefs:     evidenceRefs,
		TargetPath:       targetPath,
		TargetExists:     targetExists,
		TargetHash:       targetHash,
		Draft: ImprovementDraft{
			ImprovementID:    area + "_" + key + "_" + reviewartifact.Digest12(candidateID+c.Statement),
			Area:             area,
			Title:            reviewartifact.FirstNonEmpty(c.Subject, statement),
			ProposedChange:   statement,
			Guardrail:        "Do not apply this improvement until a reviewed eval case or replay proof shows better extraction/adjudication without increasing false promotion.",
			EvalRequirement:  "Add or cite at least one distiller eval case or replay trace that fails before the change and passes after review.",
			RollbackPlan:     "Revert the reviewed target change and keep this plan as parked evidence if follow-up evals regress.",
			RequiredEvidence: []string{"source trace that produced the improvement candidate", "current policy or method behavior being changed", "expected before/after eval or replay result", "rollback condition and owner target"},
			ReviewNote:       "This distiller improvement draft is candidate evidence. Reviewers must decide whether to apply it to distiller policy, prompts, code, or eval corpus through an explicit owner write, not from candidate output alone.",
		},
		Warnings: []string{
			"does not mutate distiller policy",
			"does not edit prompts or code",
			"does not update eval corpus",
			"does not promote memory truth",
		},
	}
	return plan, nil
}

func readImprovementCandidate(homePath, candidateID string) (Candidate, error) {
	kinds := []candidates.Kind{candidates.MethodUpgrade, candidates.PolicyPatch}
	var found *Candidate
	for _, kind := range kinds {
		var c Candidate
		if err := candidates.ReadCandidate(homePath, kind, candidateID, &c); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Candidate{}, err
		}
		if c.ID == "" {
			c.ID = candidateID
		}
		if found != nil {
			return Candidate{}, fmt.Errorf("candidate id %q found in multiple improvement inboxes", candidateID)
		}
		found = &c
	}
	if found == nil {
		return Candidate{}, fmt.Errorf("candidate %s not found in method_upgrade or policy_patch inbox", candidateID)
	}
	return *found, nil
}

func RecordImprovementPlan(homePath string, plan ImprovementPlan) (ImprovementPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return ImprovementPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return ImprovementPlanPaths{}, fmt.Errorf("improvement plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, ImprovementReviewArea)
	if err != nil {
		return ImprovementPlanPaths{}, err
	}
	paths := ImprovementPlanPaths{
		PlanPath:       filepath.Join(dir, plan.ID+".json"),
		ReviewPath:     filepath.Join(dir, plan.ID+".md"),
		ReviewYAMLPath: filepath.Join(dir, plan.ID+".review.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, ImprovementReviewArea, plan.ID, plan, "  ", RenderImprovementPlanReview(plan)); err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, ImprovementReviewArea, plan.ID+".review.yaml", []byte(RenderImprovementReviewYAMLDraft(plan))); err != nil {
		return paths, err
	}
	return paths, nil
}

func RenderImprovementReviewYAMLDraft(plan ImprovementPlan) string {
	review := ImprovementReview{
		SchemaVersion: ImprovementReviewSchemaVersion,
		ReviewID:      "review_" + plan.Draft.ImprovementID,
		PlanID:        plan.ID,
		CandidateID:   plan.CandidateID,
		CandidateKind: plan.CandidateKind,
		ImprovementID: plan.Draft.ImprovementID,
		Area:          plan.Draft.Area,
		Decision:      "",
		Rationale:     "",
		Confidence:    0,
		EvidenceRefs:  append([]string(nil), plan.EvidenceRefs...),
		EvalProofRefs: []string{},
		TargetPath:    plan.TargetPath,
		TargetHash:    plan.TargetHash,
		Draft:         plan.Draft,
	}
	data, err := yaml.Marshal(review)
	if err != nil {
		return ""
	}
	return string(data)
}

func LoadImprovementReview(path string) (ImprovementReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ImprovementReview{}, fmt.Errorf("read distiller improvement review %s: %w", path, err)
	}
	var review ImprovementReview
	if err := yaml.Unmarshal(data, &review); err != nil {
		return ImprovementReview{}, fmt.Errorf("parse distiller improvement review %s: %w", path, err)
	}
	if err := review.Validate(); err != nil {
		return ImprovementReview{}, fmt.Errorf("validate distiller improvement review %s: %w", path, err)
	}
	return review, nil
}

func (r ImprovementReview) Validate() error {
	if strings.TrimSpace(r.SchemaVersion) != ImprovementReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s", ImprovementReviewSchemaVersion)
	}
	if strings.TrimSpace(r.ReviewID) == "" {
		return fmt.Errorf("review_id required")
	}
	if strings.TrimSpace(r.PlanID) == "" {
		return fmt.Errorf("plan_id required")
	}
	if strings.TrimSpace(r.CandidateID) == "" {
		return fmt.Errorf("candidate_id required")
	}
	if strings.TrimSpace(r.CandidateKind) != KindMethodUpgrade && strings.TrimSpace(r.CandidateKind) != KindPolicyPatch {
		return fmt.Errorf("candidate_kind must be %s or %s", KindMethodUpgrade, KindPolicyPatch)
	}
	if strings.TrimSpace(r.ImprovementID) == "" {
		return fmt.Errorf("improvement_id required")
	}
	if strings.TrimSpace(r.Area) != "method" && strings.TrimSpace(r.Area) != "policy" {
		return fmt.Errorf("area must be method or policy")
	}
	if !improvementReviewDecisionOK(r.Decision) {
		return fmt.Errorf("decision must be accept, reject, or keep_reviewing")
	}
	if strings.TrimSpace(r.Rationale) == "" {
		return fmt.Errorf("rationale required")
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf("confidence must be between 0 and 1")
	}
	if len(reviewartifact.Unique(r.EvidenceRefs)) == 0 {
		return fmt.Errorf("evidence_refs required")
	}
	if strings.TrimSpace(r.Decision) == ImprovementReviewDecisionAccept && len(reviewartifact.Unique(r.EvalProofRefs)) == 0 {
		return fmt.Errorf("accept decision requires at least one eval_proof_refs entry")
	}
	if strings.TrimSpace(r.Draft.ImprovementID) == "" || strings.TrimSpace(r.Draft.ProposedChange) == "" || strings.TrimSpace(r.Draft.Guardrail) == "" {
		return fmt.Errorf("draft improvement_id, proposed_change, and guardrail required")
	}
	return nil
}

func RecordImprovementReview(homePath string, review ImprovementReview, now time.Time) (ImprovementReviewReport, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ImprovementReviewReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	plan, err := readImprovementPlan(homePath, review.PlanID)
	if err != nil {
		return ImprovementReviewReport{}, err
	}
	if err := improvementReviewMatchesPlan(review, plan); err != nil {
		return ImprovementReviewReport{}, err
	}
	if strings.TrimSpace(review.Decision) == ImprovementReviewDecisionAccept {
		if err := ValidateRecordedEvalProofRefs(homePath, review.EvalProofRefs); err != nil {
			return ImprovementReviewReport{}, err
		}
	}
	reviewed := ReviewedImprovement{
		SchemaVersion: ReviewedImprovementSchemaVersion,
		ID:            "reviewed_" + review.ReviewID,
		CreatedAt:     rfc3339(now),
		Status:        improvementReviewStatus(review.Decision),
		ReviewID:      review.ReviewID,
		PlanID:        review.PlanID,
		CandidateID:   review.CandidateID,
		CandidateKind: review.CandidateKind,
		ImprovementID: review.ImprovementID,
		Area:          review.Area,
		Decision:      review.Decision,
		Rationale:     review.Rationale,
		Confidence:    review.Confidence,
		EvidenceRefs:  reviewartifact.Unique(review.EvidenceRefs),
		EvalProofRefs: reviewartifact.Unique(review.EvalProofRefs),
		TargetPath:    review.TargetPath,
		TargetHash:    review.TargetHash,
		Draft:         review.Draft,
		Warnings: []string{
			"does not mutate distiller policy",
			"does not edit prompts or code",
			"does not update eval corpus",
			"does not promote memory truth",
		},
	}
	path, err := reviewartifact.WriteJSON(homePath, ImprovementReviewArea, review.ReviewID+".reviewed.json", reviewed, " ")
	if err != nil {
		return ImprovementReviewReport{}, err
	}
	return ImprovementReviewReport{
		Status:       "reviewed",
		ReviewID:     review.ReviewID,
		Decision:     review.Decision,
		ReviewedPath: path,
	}, nil
}

func RenderImprovementPlanReview(plan ImprovementPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Distiller Improvement Review: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- candidate: %s\n", plan.CandidateID)
	fmt.Fprintf(&b, "- candidate_kind: %s\n", plan.CandidateKind)
	if plan.CandidateVerdict != "" {
		fmt.Fprintf(&b, "- candidate_verdict: %s\n", plan.CandidateVerdict)
	}
	fmt.Fprintf(&b, "- improvement_key: %s\n", plan.ImprovementKey)
	fmt.Fprintf(&b, "- improvement_id: %s\n", plan.Draft.ImprovementID)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate distiller policy, edit prompts/code, update eval corpus, or promote memory truth.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Candidate")
	fmt.Fprintln(&b)
	if plan.Subject != "" {
		fmt.Fprintf(&b, "- subject: %s\n", plan.Subject)
	}
	fmt.Fprintf(&b, "- statement: %s\n", plan.Statement)
	if plan.Detail != "" {
		fmt.Fprintf(&b, "- detail: %s\n", plan.Detail)
	}
	if plan.Confidence > 0 {
		fmt.Fprintf(&b, "- confidence: %.2f\n", plan.Confidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Draft Improvement")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- area: %s\n", plan.Draft.Area)
	fmt.Fprintf(&b, "- proposed_change: %s\n", plan.Draft.ProposedChange)
	fmt.Fprintf(&b, "- guardrail: %s\n", plan.Draft.Guardrail)
	fmt.Fprintf(&b, "- eval_requirement: %s\n", plan.Draft.EvalRequirement)
	fmt.Fprintf(&b, "- rollback_plan: %s\n", plan.Draft.RollbackPlan)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Apply")
	fmt.Fprintln(&b)
	for _, evidence := range plan.Draft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.Draft.ReviewNote)
	return b.String()
}

func readImprovementPlan(homePath, planID string) (ImprovementPlan, error) {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return ImprovementPlan{}, fmt.Errorf("plan_id required")
	}
	path := filepath.Join(homePath, "reviews", ImprovementReviewArea, planID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ImprovementPlan{}, fmt.Errorf("read distiller improvement plan %s: %w", path, err)
	}
	var plan ImprovementPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ImprovementPlan{}, fmt.Errorf("parse distiller improvement plan %s: %w", path, err)
	}
	if plan.ID != planID {
		return ImprovementPlan{}, fmt.Errorf("plan file id %q does not match review plan_id %q", plan.ID, planID)
	}
	return plan, nil
}

func improvementReviewMatchesPlan(review ImprovementReview, plan ImprovementPlan) error {
	if review.CandidateID != plan.CandidateID {
		return fmt.Errorf("review candidate_id %q does not match plan candidate_id %q", review.CandidateID, plan.CandidateID)
	}
	if review.CandidateKind != plan.CandidateKind {
		return fmt.Errorf("review candidate_kind %q does not match plan candidate_kind %q", review.CandidateKind, plan.CandidateKind)
	}
	if review.ImprovementID != plan.Draft.ImprovementID {
		return fmt.Errorf("review improvement_id %q does not match plan improvement_id %q", review.ImprovementID, plan.Draft.ImprovementID)
	}
	if review.Area != plan.Draft.Area {
		return fmt.Errorf("review area %q does not match plan area %q", review.Area, plan.Draft.Area)
	}
	if review.TargetHash != "" && plan.TargetHash != "" && review.TargetHash != plan.TargetHash {
		return fmt.Errorf("review target_hash %q does not match plan target_hash %q", review.TargetHash, plan.TargetHash)
	}
	return nil
}

func improvementReviewDecisionOK(decision string) bool {
	switch strings.TrimSpace(decision) {
	case ImprovementReviewDecisionAccept, ImprovementReviewDecisionReject, ImprovementReviewDecisionKeepReviewing:
		return true
	default:
		return false
	}
}

func improvementReviewStatus(decision string) string {
	switch strings.TrimSpace(decision) {
	case ImprovementReviewDecisionAccept:
		return "accepted"
	case ImprovementReviewDecisionReject:
		return "rejected"
	default:
		return "needs_review"
	}
}

func AnnotateImprovementQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != ImprovementReviewArea {
			continue
		}
		annotateImprovementQueueItem(item)
	}
}

func annotateImprovementQueueItem(item *reviewartifact.QueueItem) {
	if strings.TrimSpace(item.JSONPath) == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "distiller improvement plan path missing"
		item.NextAction = "regenerate distiller improvement-plan"
		return
	}
	plan, planErr := loadImprovementPlanFile(item.JSONPath)
	if reviewed, reviewedPath, ok := reviewedImprovementForPlan(*item); ok {
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.BlockedReason = ""
		item.Actions = nil
		item.ExtraPaths = reviewartifact.Unique(append(item.ExtraPaths, reviewedPath))
		item.EvidenceRefs = reviewartifact.Unique(append(item.EvidenceRefs, reviewed.EvidenceRefs...))
		item.NextAction = fmt.Sprintf("distiller improvement review recorded: %s; no source mutation performed", reviewed.Decision)
		if planErr == nil {
			annotateImprovementOperatorCard(item, plan, reviewartifact.BandFiled)
		}
		return
	}
	if planErr == nil {
		annotateImprovementOperatorCard(item, plan, reviewartifact.BandNeedsYou)
	}
	reviewYAML := improvementReviewYAMLPath(*item)
	if reviewYAML == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "distiller improvement review YAML draft missing"
		item.NextAction = "regenerate distiller improvement-plan to create review YAML draft"
		return
	}
	argv := []string{"compound", "distiller", "improvement-review", reviewYAML}
	item.Actions = []reviewartifact.Action{{
		ID:             "record_distiller_improvement_review",
		Label:          "Record distiller improvement review",
		Command:        strings.Join(argv, " "),
		Argv:           argv,
		RequiresReview: true,
	}}
	if _, err := LoadImprovementReview(reviewYAML); err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft unreadable: " + err.Error()
		item.NextAction = "edit distiller improvement review YAML before recording"
		return
	}
	item.Readiness = "ready_to_apply"
	item.NextAction = "record reviewed distiller improvement decision; review command does not mutate policy, prompts, code, eval corpus, or memory"
}

func annotateImprovementOperatorCard(item *reviewartifact.QueueItem, plan ImprovementPlan, band string) {
	card, err := OperatorCardForImprovementPlan(plan)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "distiller improvement operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "distiller improvement operator card invalid: "+err.Error())
	}
}

func loadImprovementPlanFile(path string) (ImprovementPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ImprovementPlan{}, err
	}
	var plan ImprovementPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ImprovementPlan{}, err
	}
	return plan, nil
}

func improvementReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	if strings.TrimSpace(item.JSONPath) == "" {
		return ""
	}
	path := filepath.Join(filepath.Dir(item.JSONPath), item.ID+".review.yaml")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

func reviewedImprovementForPlan(item reviewartifact.QueueItem) (ReviewedImprovement, string, bool) {
	if strings.TrimSpace(item.JSONPath) == "" {
		return ReviewedImprovement{}, "", false
	}
	dir := filepath.Dir(item.JSONPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ReviewedImprovement{}, "", false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".reviewed.json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var reviewed ReviewedImprovement
		if err := json.Unmarshal(data, &reviewed); err != nil {
			continue
		}
		if reviewed.PlanID == item.ID {
			return reviewed, path, true
		}
	}
	return ReviewedImprovement{}, "", false
}
