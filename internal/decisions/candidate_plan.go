package decisions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
)

const (
	DecisionCandidatePlanSchemaVersion = "compound.decision_candidate_plan.v0.1"
	DecisionCandidateReviewArea        = "decision-candidates"
	DecisionCandidateReviewActionID    = "plan_decision_candidate_review"
	decisionCandidateReviewTask        = "review falsifier, review trigger, source ownership, and second-order effects"
)

// DecisionCandidatePlan is a reviewed DecisionLifecycle source proposal from a
// distiller decision candidate. It never mutates decision source files and never
// promotes memory truth.
type DecisionCandidatePlan struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	Status        string `json:"status"`
	CandidateID   string `json:"candidate_id"`
	// WorkID binds this review to a durable owner WorkEnvelope. When present and
	// live, the queue annotator seeds an owner ActionIntent + AuthorityContext so
	// the shared review-authority stamp can sign the plan-review action. Absent
	// (the plan was recorded without an owner work), the action stays unsigned and
	// the group renders fail-closed — never a fabricated button.
	WorkID           string                 `json:"work_id,omitempty"`
	CandidateVerdict string                 `json:"candidate_verdict,omitempty"`
	DecisionKey      string                 `json:"decision_key"`
	Subject          string                 `json:"subject,omitempty"`
	Statement        string                 `json:"statement,omitempty"`
	Detail           string                 `json:"detail,omitempty"`
	Entity           string                 `json:"entity,omitempty"`
	Predicate        string                 `json:"predicate,omitempty"`
	Object           string                 `json:"object,omitempty"`
	Confidence       float64                `json:"confidence,omitempty"`
	Qualifiers       map[string]string      `json:"qualifiers,omitempty"`
	EvidenceRefs     []string               `json:"evidence_refs"`
	TargetPath       string                 `json:"target_path,omitempty"`
	TargetExists     bool                   `json:"target_exists"`
	TargetHash       string                 `json:"target_hash,omitempty"`
	LifecycleDraft   DecisionLifecycleDraft `json:"lifecycle_draft"`
	Warnings         []string               `json:"warnings,omitempty"`
}

type DecisionLifecycleDraft struct {
	DecisionID       string   `json:"decision_id"`
	EntityID         string   `json:"entity_id"`
	Title            string   `json:"title"`
	Kind             string   `json:"kind"`
	Status           string   `json:"status"`
	Choice           string   `json:"choice"`
	Rationale        string   `json:"rationale"`
	Falsifier        string   `json:"falsifier"`
	ReviewTrigger    string   `json:"review_trigger"`
	Owner            string   `json:"owner,omitempty"`
	Scope            string   `json:"scope,omitempty"`
	EffectiveFrom    string   `json:"effective_from,omitempty"`
	Supersedes       []string `json:"supersedes,omitempty"`
	ReplacedBy       string   `json:"replaced_by,omitempty"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewNote       string   `json:"review_note"`
}

type DecisionCandidatePlanPaths struct {
	PlanPath        string
	ReviewPath      string
	WorkBindingPath string
}

type decisionCandidateRecord struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	Entity       string            `json:"entity,omitempty"`
	Predicate    string            `json:"predicate,omitempty"`
	Object       string            `json:"object,omitempty"`
	Subject      string            `json:"subject,omitempty"`
	Statement    string            `json:"statement,omitempty"`
	Detail       string            `json:"detail,omitempty"`
	Confidence   float64           `json:"confidence,omitempty"`
	Qualifiers   map[string]string `json:"qualifiers,omitempty"`
	EvidenceRefs []string          `json:"evidence_refs,omitempty"`
	Verdict      string            `json:"verdict,omitempty"`
}

func BuildDecisionCandidatePlan(homePath, candidateID, targetPath string, now time.Time) (DecisionCandidatePlan, error) {
	homePath = strings.TrimSpace(homePath)
	candidateID = strings.TrimSpace(candidateID)
	targetPath = strings.TrimSpace(targetPath)
	if homePath == "" {
		return DecisionCandidatePlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if candidateID == "" {
		return DecisionCandidatePlan{}, fmt.Errorf("candidate id required")
	}
	c, err := readDecisionCandidate(homePath, candidateID)
	if err != nil {
		return DecisionCandidatePlan{}, err
	}
	if c.Kind != string(candidates.Decision) {
		return DecisionCandidatePlan{}, fmt.Errorf("candidate %s kind=%q, want %q", candidateID, c.Kind, candidates.Decision)
	}
	if c.Verdict != candidates.Park {
		return DecisionCandidatePlan{}, fmt.Errorf("candidate %s verdict=%q, want park", candidateID, c.Verdict)
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return DecisionCandidatePlan{}, err
	}
	evidenceRefs := append([]string(nil), c.EvidenceRefs...)
	sort.Strings(evidenceRefs)
	qualifiers := copyStringMap(c.Qualifiers)
	key := decisionCandidateKey(c)
	choice := reviewartifact.FirstNonEmpty(c.Statement, c.Object, c.Detail, c.Subject, "review decision candidate")
	title := reviewartifact.FirstNonEmpty(c.Subject, c.Statement, c.Object, key)
	status := decisionCandidateStatus(qualifiers)
	kind := reviewartifact.FirstNonEmpty(qualifiers["kind"], KindOperational)
	falsifier := reviewartifact.FirstNonEmpty(qualifiers["falsifier"], "review required: falsifier before acceptance")
	reviewTrigger := reviewartifact.FirstNonEmpty(qualifiers["review_trigger"], "review required: trigger before acceptance")
	rationale := reviewartifact.FirstNonEmpty(c.Detail, c.Statement, "review candidate evidence and owner context")
	decisionID := reviewartifact.FirstNonEmpty(qualifiers["decision_id"], key)
	if strings.HasPrefix(decisionID, "decision:") {
		decisionID = strings.TrimPrefix(decisionID, "decision:")
	}
	warnings := []string{
		"does not mutate decision source files",
		"does not promote memory truth",
		"does not apply policy or doctrine",
	}
	if len(evidenceRefs) == 0 {
		warnings = append(warnings, "candidate has no evidence refs; reviewer must attach source evidence before acceptance")
	}
	if falsifier == "review required: falsifier before acceptance" || reviewTrigger == "review required: trigger before acceptance" {
		warnings = append(warnings, "accepted decisions require falsifier and review_trigger before promotion")
	}
	plan := DecisionCandidatePlan{
		SchemaVersion:    DecisionCandidatePlanSchemaVersion,
		ID:               reviewartifact.ID("dcp_", candidateID, targetPath),
		CreatedAt:        now.UTC().Format(time.RFC3339),
		Status:           "review_required",
		CandidateID:      candidateID,
		CandidateVerdict: c.Verdict,
		DecisionKey:      key,
		Subject:          c.Subject,
		Statement:        c.Statement,
		Detail:           c.Detail,
		Entity:           c.Entity,
		Predicate:        c.Predicate,
		Object:           c.Object,
		Confidence:       c.Confidence,
		Qualifiers:       qualifiers,
		EvidenceRefs:     evidenceRefs,
		TargetPath:       targetPath,
		TargetExists:     targetExists,
		TargetHash:       targetHash,
		LifecycleDraft: DecisionLifecycleDraft{
			DecisionID:    decisionID,
			EntityID:      decisionEntity(decisionID),
			Title:         title,
			Kind:          kind,
			Status:        status,
			Choice:        choice,
			Rationale:     rationale,
			Falsifier:     falsifier,
			ReviewTrigger: reviewTrigger,
			Owner:         qualifiers["owner"],
			Scope:         qualifiers["scope"],
			EffectiveFrom: reviewartifact.FirstNonEmpty(qualifiers["effective_from"], qualifiers["effective_date"]),
			Supersedes:    splitCSV(qualifiers["supersedes"]),
			ReplacedBy:    qualifiers["replaced_by"],
			RequiredEvidence: []string{
				"source trace evidence supporting the decision candidate",
				"owner/source file that will hold the reviewed decision",
				"falsifier and review_trigger for any accepted decision",
				"explicit reviewer approval before source mutation or promotion",
			},
			ReviewNote: "This is a DecisionLifecycle draft from a decision candidate. Reviewers must decide whether to create or update an owned decision source before any import/promotion.",
		},
		Warnings: warnings,
	}
	return plan, nil
}

func readDecisionCandidate(homePath, candidateID string) (decisionCandidateRecord, error) {
	var c decisionCandidateRecord
	if err := candidates.ReadCandidate(homePath, candidates.Decision, candidateID, &c); err != nil {
		return c, err
	}
	if c.ID == "" {
		c.ID = candidateID
	}
	if c.Kind == "" {
		c.Kind = string(candidates.Decision)
	}
	return c, nil
}

func RecordDecisionCandidatePlan(homePath string, plan DecisionCandidatePlan) (DecisionCandidatePlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return DecisionCandidatePlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return DecisionCandidatePlanPaths{}, fmt.Errorf("decision candidate plan id required")
	}
	paths, err := reviewartifact.Write(homePath, "decision-candidates", plan.ID, plan, " ", RenderDecisionCandidatePlanReview(plan))
	if err != nil {
		return DecisionCandidatePlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
	}
	out := DecisionCandidatePlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}
	if strings.TrimSpace(plan.WorkID) != "" {
		_, bindingPath, bindErr := reviewartifact.RecordWorkBinding(homePath, reviewartifact.RecordWorkBindingInput{
			Area:           DecisionCandidateReviewArea,
			ReviewID:       plan.ID,
			CoreReviewPath: paths.PlanPath,
			SourceWorkID:   plan.WorkID,
			EvidenceRefs:   reviewartifact.UniqueSorted(append(append([]string{}, plan.EvidenceRefs...), "decision-candidate-plan:"+plan.ID)),
		}, timeFromDecisionCandidatePlan(plan))
		if bindErr != nil {
			return out, bindErr
		}
		out.WorkBindingPath = bindingPath
	}
	return out, nil
}

// AnnotateDecisionCandidateQueue gives each still-open DecisionLifecycle draft
// one bounded next step through the existing panel owner. The action creates
// review evidence only: accepting, importing, or writing a decision source
// remains a separate owner-approved operation.
func AnnotateDecisionCandidateQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != DecisionCandidateReviewArea {
			continue
		}
		annotateDecisionCandidateQueueItem(item)
	}
}

func annotateDecisionCandidateQueueItem(item *reviewartifact.QueueItem) {
	item.Actions = nil
	if item.Status != "review_required" {
		return
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "decision candidate review artifact unreadable")
		return
	}
	var plan DecisionCandidatePlan
	if err := json.Unmarshal(data, &plan); err != nil ||
		plan.SchemaVersion != DecisionCandidatePlanSchemaVersion ||
		strings.TrimSpace(plan.ID) == "" || strings.TrimSpace(plan.CandidateID) == "" {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "decision candidate review artifact invalid")
		return
	}
	homePath := decisionCandidateReviewHome(item.JSONPath)
	if homePath == "" {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "decision candidate review home unresolved")
		return
	}
	candidate, err := readDecisionCandidate(homePath, plan.CandidateID)
	if err != nil || candidate.Verdict != candidates.Park {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "decision candidate is no longer available for panel review")
		return
	}
	argv := decisionCandidatePanelPlanArgv(plan.CandidateID)
	item.CandidateID = plan.CandidateID
	item.GroupKey = decisionCandidateReviewGroupKey(plan)
	item.Owner = "internal/decisions"
	item.NextAction = "plan decision-specific lens review"
	action := reviewartifact.Action{
		ID:             DecisionCandidateReviewActionID,
		Label:          "Plan decision review",
		Command:        shellCommand(argv),
		Argv:           argv,
		RequiresReview: true,
		PolicyNote:     "creates review evidence only; decision source and authority stay unchanged",
	}
	// Seed the owner authority bundle so the shared review-authority stamp
	// (reviewqueue.StampReviewActionAuthority) can sign the action. The stamp is
	// the sole signer; a producer that omits ActionIntent + AuthorityContext ships
	// an unsigned action that fails closed on every authority-aware surface.
	authorityPlan := plan
	authorityPlan.WorkID = reviewartifact.AuthoritySeedWorkID(*item, plan.WorkID)
	action.ActionIntent, action.AuthorityContext = decisionCandidateReviewAuthoritySeed(authorityPlan)
	item.Actions = []reviewartifact.Action{action}
}

func timeFromDecisionCandidatePlan(plan DecisionCandidatePlan) time.Time {
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(plan.CreatedAt)); err == nil {
		return parsed
	}
	return time.Now()
}

// decisionCandidateReviewAuthoritySeed builds the owner ActionIntent and
// AuthorityContext for the plan-review action. It returns (nil, nil) when the
// plan is not bound to a durable, non-synthetic owner work id, or when the
// intent it would produce is not structurally valid — either way the caller
// keeps the action unsigned and the group stays fail-closed. Liveness of the
// work id is re-checked at stamp time against the work store; this seed only
// requires a well-formed identity so the stamp has something to validate.
func decisionCandidateReviewAuthoritySeed(plan DecisionCandidatePlan) (*envelope.ActionIntent, *policy.AuthorityContext) {
	workID := strings.TrimSpace(plan.WorkID)
	if len(workID) <= len("work_") || !strings.HasPrefix(workID, "work_") ||
		strings.HasPrefix(workID, "work_live_") || filepath.Base(workID) != workID {
		return nil, nil
	}
	candidateID := strings.TrimSpace(plan.CandidateID)
	createdAt := strings.TrimSpace(plan.CreatedAt)
	if candidateID == "" || createdAt == "" {
		return nil, nil
	}
	evidenceRefs := reviewartifact.Unique(append([]string{}, plan.EvidenceRefs...))
	intent := &envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID: "act_" + reviewartifact.Digest12(strings.Join([]string{
				"decision-candidate-review", DecisionCandidateReviewActionID, plan.ID, workID, candidateID,
			}, "\x00")),
			CreatedAt:    createdAt,
			Producer:     envelope.Producer{Kind: "owner_projection", Name: "compound"},
			IDs:          envelope.IDs{WorkID: workID},
			Authority:    envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:      "private",
			Risk:         "tier1",
			EvidenceRefs: evidenceRefs,
			Uncertainty:  envelope.Uncertainty{Level: "known"},
		},
		Actor:         "compound",
		Capability:    "review.decision_candidate",
		Action:        DecisionCandidateReviewActionID,
		Target:        envelope.ActionTarget{Kind: "decision_candidate", ID: candidateID},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "Plan a decision-specific lens review for decision candidate " + candidateID + "; creates review evidence only.",
		RollbackPlan:  "Discard the generated panel plan review artifact; no decision source, memory, or authority is mutated.",
	}
	if err := intent.Validate(); err != nil {
		return nil, nil
	}
	context := &policy.AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    policy.RecoverabilityVerified,
		OperatorControlContinuity: policy.OperatorControlContinuous,
		ExpectedReceipt:           "Recorded a panel plan review for decision candidate " + candidateID + " under work " + workID + "; decision source and memory remain unchanged.",
		ExpectedProof: append([]string{
			"decision-candidate:" + candidateID,
			"decision-candidate-plan:" + plan.ID,
			"work:" + workID,
		}, evidenceRefs...),
		EvidenceRefs: evidenceRefs,
	}
	return intent, context
}

func decisionCandidatePanelPlanArgv(candidateID string) []string {
	return []string{
		"compound", "panel", "plan",
		"--target-kind", "decision_candidate",
		"--target", strings.TrimSpace(candidateID),
		"--domain", "reasoning-lenses",
		"--task", decisionCandidateReviewTask,
		"--supervised",
		"--max-items", "1",
		"--json",
	}
}

func decisionCandidateReviewHome(path string) string {
	areaDir := filepath.Dir(strings.TrimSpace(path))
	reviewsDir := filepath.Dir(areaDir)
	if filepath.Base(areaDir) != DecisionCandidateReviewArea || filepath.Base(reviewsDir) != "reviews" {
		return ""
	}
	return filepath.Dir(reviewsDir)
}

func RenderDecisionCandidatePlanReview(plan DecisionCandidatePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Decision Candidate Review Plan: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- candidate: %s\n", plan.CandidateID)
	if plan.CandidateVerdict != "" {
		fmt.Fprintf(&b, "- candidate_verdict: %s\n", plan.CandidateVerdict)
	}
	fmt.Fprintf(&b, "- decision_key: %s\n", plan.DecisionKey)
	fmt.Fprintf(&b, "- decision_id: %s\n", plan.LifecycleDraft.DecisionID)
	fmt.Fprintf(&b, "- entity: %s\n", plan.LifecycleDraft.EntityID)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate decision source files, apply policy/doctrine, or promote memory truth.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Candidate")
	fmt.Fprintln(&b)
	if plan.Subject != "" {
		fmt.Fprintf(&b, "- subject: %s\n", plan.Subject)
	}
	if plan.Statement != "" {
		fmt.Fprintf(&b, "- statement: %s\n", plan.Statement)
	}
	if plan.Detail != "" {
		fmt.Fprintf(&b, "- detail: %s\n", plan.Detail)
	}
	if plan.Object != "" {
		fmt.Fprintf(&b, "- object: %s\n", plan.Object)
	}
	if plan.Confidence > 0 {
		fmt.Fprintf(&b, "- confidence: %.2f\n", plan.Confidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Draft DecisionLifecycle")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- title: %s\n", plan.LifecycleDraft.Title)
	fmt.Fprintf(&b, "- kind: %s\n", plan.LifecycleDraft.Kind)
	fmt.Fprintf(&b, "- status: %s\n", plan.LifecycleDraft.Status)
	fmt.Fprintf(&b, "- choice: %s\n", plan.LifecycleDraft.Choice)
	fmt.Fprintf(&b, "- rationale: %s\n", plan.LifecycleDraft.Rationale)
	fmt.Fprintf(&b, "- falsifier: %s\n", plan.LifecycleDraft.Falsifier)
	fmt.Fprintf(&b, "- review_trigger: %s\n", plan.LifecycleDraft.ReviewTrigger)
	if plan.LifecycleDraft.Owner != "" {
		fmt.Fprintf(&b, "- owner: %s\n", plan.LifecycleDraft.Owner)
	}
	if plan.LifecycleDraft.Scope != "" {
		fmt.Fprintf(&b, "- scope: %s\n", plan.LifecycleDraft.Scope)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Apply")
	fmt.Fprintln(&b)
	for _, evidence := range plan.LifecycleDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.LifecycleDraft.ReviewNote)
	return b.String()
}

func decisionCandidateStatus(q map[string]string) string {
	status := reviewartifact.FirstNonEmpty(q["lifecycle_status"], q["status"])
	if status == "" {
		return StatusProposed
	}
	return CanonicalStatus(status)
}

func decisionCandidateKey(c decisionCandidateRecord) string {
	raw := reviewartifact.FirstNonEmpty(c.Qualifiers["decision_id"], c.Subject, c.Statement, c.Object, c.ID)
	raw = strings.TrimPrefix(raw, "decision:")
	key := slugDecisionCandidate(raw)
	if key == "" {
		return digest12(c.ID)
	}
	return key
}

func slugDecisionCandidate(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == ':' || r == '_' || r == '-' || r == '/' || r == ' ' || r == '.':
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 72 {
		out = strings.Trim(out[:72], "-")
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
