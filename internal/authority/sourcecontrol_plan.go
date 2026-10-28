package authority

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const (
	SourceControlReviewPlanSchemaVersion   = "compound.source_control_review_plan.v0.1"
	SourceControlReviewSchemaVersion       = "compound.source_control_review.v0.1"
	SourceControlReviewReportSchemaVersion = "compound.source_control_review_report.v0.1"
	SourceControlCleanupReviewArea         = "source-control-cleanup"

	// SourceControlReviewSupersessionSchemaVersion names the system-written
	// disposition that retires an older open dirty-plan for a repo path once a
	// newer snapshot of that same path replaces it.
	SourceControlReviewSupersessionSchemaVersion = "compound.source_control_review_supersession.v0.1"
)

// SourceControlReviewSupersession retires an older open dirty-plan when a newer
// snapshot of the same repo path replaces it. It is written by the producer, not
// by an operator: it carries no decision, no reviewer, and no authorization, so
// it can never satisfy the mutation-authorization path — that still requires a
// sealed `.reviewed.json` report. It exists so one repo path keeps exactly one
// open review item instead of one per snapshot.
type SourceControlReviewSupersession struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	PlanID        string   `json:"plan_id"`
	RepoID        string   `json:"repo_id"`
	GitRoot       string   `json:"git_root"`
	SupersededBy  string   `json:"superseded_by"`
	Reason        string   `json:"reason"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
}

// SourceControlReviewPlan turns a dirty source-control snapshot into an
// owner-review artifact. It never stages, cleans, commits, resets, pushes, or
// promotes source-control claims.
type SourceControlReviewPlan struct {
	SchemaVersion           string                        `json:"schema_version"`
	ID                      string                        `json:"id"`
	CreatedAt               string                        `json:"created_at"`
	Status                  string                        `json:"status"`
	RepoID                  string                        `json:"repo_id"`
	WorkID                  string                        `json:"work_id,omitempty"`
	RequestedPath           string                        `json:"requested_path,omitempty"`
	GitRoot                 string                        `json:"git_root"`
	Remote                  string                        `json:"remote,omitempty"`
	Branch                  string                        `json:"branch,omitempty"`
	Head                    string                        `json:"head,omitempty"`
	Upstream                string                        `json:"upstream,omitempty"`
	Ahead                   int                           `json:"ahead"`
	Behind                  int                           `json:"behind"`
	SourceStatus            string                        `json:"source_status"`
	Staged                  int                           `json:"staged"`
	Unstaged                int                           `json:"unstaged"`
	Untracked               int                           `json:"untracked"`
	ChangedPaths            int                           `json:"changed_paths"`
	ChangedStateFingerprint string                        `json:"changed_state_fingerprint,omitempty"`
	MutationBinding         *SourceControlMutationBinding `json:"mutation_binding,omitempty"`
	PlanDigest              string                        `json:"plan_digest,omitempty"`
	EvidenceRefs            []string                      `json:"evidence_refs"`
	ReviewDraft             SourceControlReviewDraft      `json:"review_draft"`
	Warnings                []string                      `json:"warnings,omitempty"`
}

// SourceControlMutationBinding is executable identity derived from persisted
// ActionIntents. ActionIntentIDs are provenance; retries are matched by the
// session-bound input hash plus action work, semantics, and the full target set.
type SourceControlMutationBinding struct {
	ActionWorkID      string   `json:"action_work_id"`
	MutationInputHash string   `json:"mutation_input_hash"`
	Capability        string   `json:"capability"`
	Action            string   `json:"action"`
	MutationClass     string   `json:"mutation_class"`
	TargetPaths       []string `json:"target_paths"`
	ActionIntentIDs   []string `json:"action_intent_ids"`
}

type SourceControlReviewDraft struct {
	ReviewID          string   `json:"review_id"`
	Title             string   `json:"title"`
	Situation         string   `json:"situation"`
	ProposedNextStep  string   `json:"proposed_next_step"`
	CleanupGuardrail  string   `json:"cleanup_guardrail"`
	Verification      string   `json:"verification"`
	RequiredEvidence  []string `json:"required_evidence"`
	RetirementTrigger string   `json:"retirement_trigger"`
	ReviewNote        string   `json:"review_note"`
}

type SourceControlReviewPlanPaths struct {
	PlanPath       string `json:"plan_path"`
	ReviewPath     string `json:"review_path"`
	ReviewYAMLPath string `json:"review_yaml_path"`
}

type SourceControlReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	RepoID        string   `json:"repo_id" yaml:"repo_id"`
	Decision      string   `json:"decision" yaml:"decision"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	NextAction    string   `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedBy    string   `json:"reviewed_by,omitempty" yaml:"reviewed_by,omitempty"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type SourceControlReviewReport struct {
	SchemaVersion           string                        `json:"schema_version"`
	ID                      string                        `json:"id"`
	CreatedAt               string                        `json:"created_at"`
	Status                  string                        `json:"status"`
	PlanID                  string                        `json:"plan_id"`
	ReviewID                string                        `json:"review_id"`
	RepoID                  string                        `json:"repo_id"`
	WorkID                  string                        `json:"work_id,omitempty"`
	GitRoot                 string                        `json:"git_root,omitempty"`
	Remote                  string                        `json:"remote,omitempty"`
	Head                    string                        `json:"head,omitempty"`
	ChangedStateFingerprint string                        `json:"changed_state_fingerprint,omitempty"`
	MutationBinding         *SourceControlMutationBinding `json:"mutation_binding,omitempty"`
	PlanDigest              string                        `json:"plan_digest,omitempty"`
	Decision                string                        `json:"decision"`
	ReviewedBy              string                        `json:"reviewed_by,omitempty"`
	ReviewedAt              string                        `json:"reviewed_at,omitempty"`
	NextAction              string                        `json:"next_action,omitempty"`
	SourcePath              string                        `json:"source_path,omitempty"`
	SourceDigest            string                        `json:"source_digest,omitempty"`
	ReviewPath              string                        `json:"review_path,omitempty"`
	AuthorizationDigest     string                        `json:"authorization_digest,omitempty"`
	EvidenceRefs            []string                      `json:"evidence_refs,omitempty"`
	Warnings                []string                      `json:"warnings,omitempty"`
}

func BuildSourceControlReviewPlan(snapshot SourceControlSnapshot, workID string, now time.Time) (SourceControlReviewPlan, error) {
	return buildSourceControlReviewPlan(snapshot, strings.TrimSpace(workID), nil, now)
}

// BuildSourceControlMutationReviewPlan creates the only source-control review
// plan that may authorize a dirty mutation. Legacy/count-only plans remain
// cleanup evidence and can never satisfy the executable binding.
func BuildSourceControlMutationReviewPlan(snapshot SourceControlSnapshot, ownerWorkID string, binding SourceControlMutationBinding, now time.Time) (SourceControlReviewPlan, error) {
	ownerWorkID = strings.TrimSpace(ownerWorkID)
	if ownerWorkID == "" {
		return SourceControlReviewPlan{}, fmt.Errorf("source-control mutation review owner work_id required")
	}
	normalized, err := normalizeSourceControlMutationBinding(snapshot, binding)
	if err != nil {
		return SourceControlReviewPlan{}, err
	}
	if !strings.HasPrefix(normalized.ActionWorkID, "work_live_") && normalized.ActionWorkID != ownerWorkID {
		return SourceControlReviewPlan{}, fmt.Errorf("source-control mutation action work_id %q must equal owner work_id %q unless it is synthetic work_live_*", normalized.ActionWorkID, ownerWorkID)
	}
	return buildSourceControlReviewPlan(snapshot, ownerWorkID, &normalized, now)
}

func buildSourceControlReviewPlan(snapshot SourceControlSnapshot, workID string, binding *SourceControlMutationBinding, now time.Time) (SourceControlReviewPlan, error) {
	if strings.TrimSpace(snapshot.RepoID) == "" {
		return SourceControlReviewPlan{}, fmt.Errorf("source-control snapshot repo_id required")
	}
	if strings.TrimSpace(snapshot.GitRoot) == "" {
		return SourceControlReviewPlan{}, fmt.Errorf("source-control snapshot git_root required")
	}
	if snapshot.Status != "dirty" || snapshot.ChangedPaths == 0 {
		return SourceControlReviewPlan{}, fmt.Errorf("dirty source-control snapshot required")
	}
	if strings.TrimSpace(snapshot.Head) == "" || !validSHA256(snapshot.ChangedStateFingerprint) {
		return SourceControlReviewPlan{}, fmt.Errorf("dirty source-control snapshot head and changed_state_fingerprint required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	evidence := snapshot.EvidenceReference()
	keyParts := []string{snapshot.RepoID, snapshot.GitRoot, snapshot.Remote, snapshot.Head, snapshot.ChangedStateFingerprint, strings.TrimSpace(workID)}
	if binding != nil {
		encoded, _ := json.Marshal(binding)
		keyParts = append(keyParts, string(encoded))
	}
	key := strings.Join(keyParts, "\x00")
	reviewID := "scr_" + reviewartifact.Digest12(key)
	plan := SourceControlReviewPlan{
		SchemaVersion:           SourceControlReviewPlanSchemaVersion,
		ID:                      reviewID,
		CreatedAt:               now.UTC().Format(time.RFC3339),
		Status:                  "review_required",
		RepoID:                  snapshot.RepoID,
		WorkID:                  strings.TrimSpace(workID),
		RequestedPath:           snapshot.RequestedPath,
		GitRoot:                 snapshot.GitRoot,
		Remote:                  snapshot.Remote,
		Branch:                  snapshot.Branch,
		Head:                    snapshot.Head,
		Upstream:                snapshot.Upstream,
		Ahead:                   snapshot.Ahead,
		Behind:                  snapshot.Behind,
		SourceStatus:            snapshot.Status,
		Staged:                  snapshot.Staged,
		Unstaged:                snapshot.Unstaged,
		Untracked:               snapshot.Untracked,
		ChangedPaths:            snapshot.ChangedPaths,
		ChangedStateFingerprint: snapshot.ChangedStateFingerprint,
		MutationBinding:         binding,
		EvidenceRefs:            []string{evidence},
		ReviewDraft: SourceControlReviewDraft{
			ReviewID:         reviewID,
			Title:            "Review dirty source-control state for " + snapshot.RepoID,
			Situation:        fmt.Sprintf("%d changed path(s): %d staged, %d unstaged, %d untracked", snapshot.ChangedPaths, snapshot.Staged, snapshot.Unstaged, snapshot.Untracked),
			ProposedNextStep: "Classify each change as keep, park, commit, or discard in the owning repo before continuing mutation work.",
			CleanupGuardrail: "Do not run git add, commit, reset, checkout, clean, pull, push, or stash from this plan. Cleanup requires an explicit owner action and separate receipt.",
			Verification:     "A follow-up read-only source-control snapshot shows the repo clean or the remaining dirty paths are intentionally parked and cited.",
			RequiredEvidence: []string{
				"read-only source-control snapshot evidence",
				"owner decision for each staged/unstaged/untracked class",
				"post-review source-control snapshot",
				"receipt for any actual cleanup action",
			},
			RetirementTrigger: "Close this review when dirty ambiguity no longer blocks the held work or when the owner deliberately parks the remaining changes.",
			ReviewNote:        "This plan makes dirty source-control ambiguity visible. It is not a cleanup script and does not change repository state.",
		},
		Warnings: []string{
			"does not stage files",
			"does not reset checkout clean stash commit pull or push",
			"does not promote source-control claims",
			"does not resolve policy holds by itself",
		},
	}
	if binding != nil {
		plan.EvidenceRefs = append(plan.EvidenceRefs, prefixedActionIntentRefs(binding.ActionIntentIDs)...)
		plan.ReviewDraft.ProposedNextStep = "Review the exact persisted mutation payload and dirty snapshot. Record authorize_mutation only if this one state and payload may proceed."
		plan.ReviewDraft.Verification = "The recorded review digest and every repo/HEAD/fingerprint/work/input/action/target field match at PreToolUse; any drift remains held."
		plan.ReviewDraft.RequiredEvidence = append(plan.ReviewDraft.RequiredEvidence,
			"persisted ActionIntent evidence for the complete target set",
			"session-bound content-free mutation-input digest",
		)
		plan.Warnings = append(plan.Warnings,
			"does not authorize until authorize_mutation is recorded",
			"authorization expires on any repo state or mutation payload drift",
		)
	}
	plan.PlanDigest = SourceControlReviewPlanDigest(plan)
	if err := ValidateSourceControlReviewPlan(plan); err != nil {
		return SourceControlReviewPlan{}, err
	}
	return plan, nil
}

// SourceControlReviewPlanDigest is a semantic digest over the immutable plan
// with its self-digest blanked. A reviewed report copies this digest and checks
// it again at consumption, so editing the sibling plan after review fails shut.
func SourceControlReviewPlanDigest(plan SourceControlReviewPlan) string {
	plan.PlanDigest = ""
	encoded, _ := json.Marshal(plan)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:])
}

// ValidateSourceControlReviewPlan validates the complete immutable review
// owner, not only its self-digest. Every writer and consumer uses this seam so
// a structurally corrupt plan cannot mint, display, or record review authority.
func ValidateSourceControlReviewPlan(plan SourceControlReviewPlan) error {
	if plan.SchemaVersion != SourceControlReviewPlanSchemaVersion {
		return fmt.Errorf("source-control review plan schema_version must be %s, got %q", SourceControlReviewPlanSchemaVersion, plan.SchemaVersion)
	}
	if len(plan.ID) <= len("scr_") || !strings.HasPrefix(plan.ID, "scr_") || filepath.Base(plan.ID) != plan.ID {
		return fmt.Errorf("source-control review plan id must be a bare scr_ id")
	}
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(plan.CreatedAt))
	if err != nil || createdAt.IsZero() {
		return fmt.Errorf("source-control review plan created_at must be a non-zero RFC3339 timestamp")
	}
	if plan.Status != "review_required" {
		return fmt.Errorf("source-control review plan status must be review_required, got %q", plan.Status)
	}
	if strings.TrimSpace(plan.RepoID) == "" {
		return fmt.Errorf("source-control review plan repo_id required")
	}
	if strings.TrimSpace(plan.GitRoot) == "" || !filepath.IsAbs(plan.GitRoot) {
		return fmt.Errorf("source-control review plan git_root must be absolute")
	}
	if plan.SourceStatus != "dirty" {
		return fmt.Errorf("source-control review plan source_status must be dirty, got %q", plan.SourceStatus)
	}
	if !validGitObjectID(plan.Head) {
		return fmt.Errorf("source-control review plan head must be a full git object id")
	}
	if !validSHA256(plan.ChangedStateFingerprint) {
		return fmt.Errorf("source-control review plan changed_state_fingerprint must be sha256")
	}
	if plan.Ahead < 0 || plan.Behind < 0 || plan.Staged < 0 || plan.Unstaged < 0 || plan.Untracked < 0 {
		return fmt.Errorf("source-control review plan snapshot counts must be non-negative")
	}
	if plan.ChangedPaths <= 0 || plan.ChangedPaths != plan.Staged+plan.Unstaged+plan.Untracked {
		return fmt.Errorf("source-control review plan changed_paths must equal staged+unstaged+untracked and be positive")
	}
	if len(plan.EvidenceRefs) == 0 {
		return fmt.Errorf("source-control review plan evidence_refs required")
	}
	for _, ref := range plan.EvidenceRefs {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("source-control review plan evidence_refs must be non-empty")
		}
	}
	if plan.ReviewDraft.ReviewID != plan.ID || filepath.Base(plan.ReviewDraft.ReviewID) != plan.ReviewDraft.ReviewID {
		return fmt.Errorf("source-control review plan review_draft.review_id must equal plan id")
	}
	requiredDraft := []struct {
		field string
		value string
	}{
		{"title", plan.ReviewDraft.Title},
		{"situation", plan.ReviewDraft.Situation},
		{"proposed_next_step", plan.ReviewDraft.ProposedNextStep},
		{"cleanup_guardrail", plan.ReviewDraft.CleanupGuardrail},
		{"verification", plan.ReviewDraft.Verification},
		{"retirement_trigger", plan.ReviewDraft.RetirementTrigger},
		{"review_note", plan.ReviewDraft.ReviewNote},
	}
	for _, field := range requiredDraft {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("source-control review plan review_draft.%s required", field.field)
		}
	}
	if len(plan.ReviewDraft.RequiredEvidence) == 0 {
		return fmt.Errorf("source-control review plan review_draft.required_evidence required")
	}
	for _, ref := range plan.ReviewDraft.RequiredEvidence {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("source-control review plan review_draft.required_evidence must be non-empty")
		}
	}
	workID := strings.TrimSpace(plan.WorkID)
	if plan.WorkID != workID {
		return fmt.Errorf("source-control review plan work_id must be normalized")
	}
	if workID != "" {
		if len(workID) <= len("work_") || plan.WorkID != workID || !strings.HasPrefix(workID, "work_") || filepath.Base(workID) != workID {
			return fmt.Errorf("source-control review plan work_id must be a bare work_ id")
		}
	}
	if plan.MutationBinding != nil {
		if strings.TrimSpace(plan.WorkID) == "" || strings.HasPrefix(plan.WorkID, "work_live_") {
			return fmt.Errorf("source-control review plan mutation_binding requires a durable owner work_id")
		}
		if err := validateNormalizedSourceControlMutationBinding(*plan.MutationBinding); err != nil {
			return fmt.Errorf("source-control review plan mutation_binding invalid: %w", err)
		}
		if plan.MutationBinding.ActionWorkID != strings.TrimSpace(plan.MutationBinding.ActionWorkID) ||
			plan.MutationBinding.Capability != strings.TrimSpace(plan.MutationBinding.Capability) ||
			plan.MutationBinding.Action != strings.TrimSpace(plan.MutationBinding.Action) ||
			plan.MutationBinding.MutationClass != strings.TrimSpace(plan.MutationBinding.MutationClass) {
			return fmt.Errorf("source-control review plan mutation_binding scalar fields must be normalized")
		}
		if len(plan.MutationBinding.ActionWorkID) <= len("work_") || !strings.HasPrefix(plan.MutationBinding.ActionWorkID, "work_") || filepath.Base(plan.MutationBinding.ActionWorkID) != plan.MutationBinding.ActionWorkID {
			return fmt.Errorf("source-control review plan mutation_binding action_work_id must be a bare work_ id")
		}
		if !strings.HasPrefix(plan.MutationBinding.ActionWorkID, "work_live_") && plan.MutationBinding.ActionWorkID != plan.WorkID {
			return fmt.Errorf("source-control review plan mutation_binding action_work_id must equal owner or be synthetic")
		}
		if !validSourceControlMutationClass(plan.MutationBinding.MutationClass) {
			return fmt.Errorf("source-control review plan mutation_binding mutation_class invalid")
		}
		if !stringSlicesEqual(plan.MutationBinding.ActionIntentIDs, sortedUniqueStrings(plan.MutationBinding.ActionIntentIDs)) ||
			!stringSlicesEqual(plan.MutationBinding.TargetPaths, sortedUniqueStrings(plan.MutationBinding.TargetPaths)) {
			return fmt.Errorf("source-control review plan mutation_binding identities and targets must be canonical unique order")
		}
		root := filepath.Clean(plan.GitRoot)
		for _, target := range plan.MutationBinding.TargetPaths {
			if !filepath.IsAbs(target) {
				return fmt.Errorf("source-control review plan mutation_binding target must be absolute")
			}
			canonicalTarget, err := CanonicalSourceControlMutationTarget(root, target)
			if err != nil || canonicalTarget != target {
				return fmt.Errorf("source-control review plan mutation_binding target must be canonical")
			}
			rel, err := filepath.Rel(root, filepath.Clean(target))
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("source-control review plan mutation_binding target outside git_root")
			}
		}
		if !containsEvery(plan.EvidenceRefs, prefixedActionIntentRefs(plan.MutationBinding.ActionIntentIDs)) {
			return fmt.Errorf("source-control review plan mutation_binding action evidence missing")
		}
	}
	if !validSHA256(plan.PlanDigest) || plan.PlanDigest != SourceControlReviewPlanDigest(plan) {
		return fmt.Errorf("source-control review plan plan_digest invalid")
	}
	return nil
}

func validGitObjectID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func validSourceControlMutationClass(value string) bool {
	value = strings.TrimSpace(value)
	for _, mutationClass := range envelope.MutationClasses {
		if value == mutationClass {
			return true
		}
	}
	return false
}

func RecordSourceControlReviewPlan(homePath string, plan SourceControlReviewPlan) (SourceControlReviewPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceControlReviewPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if err := ValidateSourceControlReviewPlan(plan); err != nil {
		return SourceControlReviewPlanPaths{}, err
	}
	paths, err := reviewartifact.Write(homePath, SourceControlCleanupReviewArea, plan.ID, plan, "  ", RenderSourceControlReviewPlan(plan))
	if err != nil {
		return SourceControlReviewPlanPaths{}, err
	}
	reviewYAMLPath, err := reviewartifact.WriteRaw(homePath, SourceControlCleanupReviewArea, plan.ID+".review.yaml", []byte(RenderSourceControlReviewYAMLDraft(plan)))
	if err != nil {
		return SourceControlReviewPlanPaths{}, err
	}
	if err := supersedeStaleSourceControlReviewPlans(homePath, plan); err != nil {
		return SourceControlReviewPlanPaths{}, err
	}
	return SourceControlReviewPlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath, ReviewYAMLPath: reviewYAMLPath}, nil
}

// supersedeStaleSourceControlReviewPlans keeps one open review item per repo
// path. Plan identity digests the snapshot, so every changed HEAD or dirty-state
// fingerprint minted a brand-new item for the same root and the same question;
// the area grew monotonically and its counts stopped meaning anything.
//
// Retiring is narrow on purpose. It only touches an older plan that is still
// open, describes the SAME repo path, and describes a DIFFERENT snapshot than
// the plan replacing it:
//   - a plan with a recorded disposition is immutable operator evidence (a
//     sealed mutation authorization lives there) and is never rewritten;
//   - a plan describing the same snapshot is a live sibling, not a stale one —
//     a count-only plan and a mutation-bound plan share one snapshot under
//     different ids, and retiring the live one would drop an authorization the
//     operator still owes.
func supersedeStaleSourceControlReviewPlans(homePath string, plan SourceControlReviewPlan) error {
	dir := filepath.Join(homePath, "reviews", SourceControlCleanupReviewArea)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(plan.CreatedAt))
	if err != nil {
		return nil
	}
	gitRoot := filepath.Clean(plan.GitRoot)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "scr_") || !strings.HasSuffix(name, ".json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		if stem == plan.ID || strings.Contains(stem, ".") {
			continue
		}
		stale, err := loadSourceControlPlanFile(filepath.Join(dir, name))
		if err != nil {
			// Not a currently valid dirty-plan: foreign or legacy evidence the
			// producer does not own. Leave it exactly as it is.
			continue
		}
		if filepath.Clean(stale.GitRoot) != gitRoot {
			continue
		}
		if stale.Head == plan.Head && stale.ChangedStateFingerprint == plan.ChangedStateFingerprint {
			continue
		}
		staleAt, err := time.Parse(time.RFC3339, strings.TrimSpace(stale.CreatedAt))
		if err != nil || staleAt.After(createdAt) {
			continue
		}
		if sourceControlReviewPlanDisposed(dir, stale) {
			continue
		}
		if err := writeSourceControlReviewSupersession(homePath, stale, plan); err != nil {
			return err
		}
	}
	return nil
}

// sourceControlReviewPlanDisposed reports whether a plan already carries a
// terminal disposition: an operator-recorded review on either the sidecar stem
// or the pre-de-dupe doubled stem, or an earlier supersession.
func sourceControlReviewPlanDisposed(dir string, plan SourceControlReviewPlan) bool {
	names := []string{
		sourceControlReviewReportName(plan.ID, plan.ReviewDraft.ReviewID),
		sourceControlLegacyReviewReportName(plan.ID, plan.ReviewDraft.ReviewID),
		sourceControlReviewSupersessionName(plan.ID),
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// sourceControlReviewSupersessionName owns the supersession file name. The
// `<stem>.decision.json` suffix is the one internal/home already selects as a
// terminal review sidecar, so a superseded plan drains with its own core item
// and never becomes a second core item of its own.
func sourceControlReviewSupersessionName(planID string) string {
	return planID + ".decision.json"
}

func writeSourceControlReviewSupersession(homePath string, stale, successor SourceControlReviewPlan) error {
	supersession := SourceControlReviewSupersession{
		SchemaVersion: SourceControlReviewSupersessionSchemaVersion,
		ID:            "scrs_" + reviewartifact.Digest12(stale.ID+"\x00"+successor.ID),
		CreatedAt:     successor.CreatedAt,
		Status:        "superseded",
		PlanID:        stale.ID,
		RepoID:        stale.RepoID,
		GitRoot:       stale.GitRoot,
		SupersededBy:  successor.ID,
		Reason:        "a newer read-only snapshot of the same repo path replaced this dirty-plan before it was reviewed",
		EvidenceRefs:  reviewartifact.Unique(append(append([]string{}, stale.EvidenceRefs...), successor.EvidenceRefs...)),
	}
	_, err := reviewartifact.WriteJSON(homePath, SourceControlCleanupReviewArea, sourceControlReviewSupersessionName(stale.ID), supersession, "  ")
	return err
}

func RenderSourceControlReviewPlan(plan SourceControlReviewPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Source-Control Cleanup Review: %s\n\n", plan.RepoID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- repo: %s\n", plan.RepoID)
	if plan.WorkID != "" {
		fmt.Fprintf(&b, "- work: %s\n", plan.WorkID)
	}
	fmt.Fprintf(&b, "- git_root: %s\n", plan.GitRoot)
	if plan.Branch != "" {
		fmt.Fprintf(&b, "- branch: %s\n", plan.Branch)
	}
	if plan.Head != "" {
		fmt.Fprintf(&b, "- head: %s\n", plan.Head)
	}
	fmt.Fprintf(&b, "- status_snapshot: %s\n", plan.SourceStatus)
	fmt.Fprintf(&b, "- changed_paths: %d\n", plan.ChangedPaths)
	fmt.Fprintf(&b, "- changed_state_fingerprint: %s\n", plan.ChangedStateFingerprint)
	fmt.Fprintf(&b, "- staged: %d\n", plan.Staged)
	fmt.Fprintf(&b, "- unstaged: %d\n", plan.Unstaged)
	fmt.Fprintf(&b, "- untracked: %d\n", plan.Untracked)
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	if plan.MutationBinding != nil {
		fmt.Fprintf(&b, "- action_work: %s\n", plan.MutationBinding.ActionWorkID)
		fmt.Fprintf(&b, "- mutation_input_hash: %s\n", plan.MutationBinding.MutationInputHash)
		fmt.Fprintf(&b, "- capability: %s\n", plan.MutationBinding.Capability)
		fmt.Fprintf(&b, "- action: %s\n", plan.MutationBinding.Action)
		fmt.Fprintf(&b, "- mutation_class: %s\n", plan.MutationBinding.MutationClass)
		fmt.Fprintf(&b, "- target_paths: %s\n", strings.Join(plan.MutationBinding.TargetPaths, ", "))
	}
	fmt.Fprintf(&b, "- plan_digest: %s\n\n", plan.PlanDigest)
	fmt.Fprintln(&b, "This owner-review artifact does not stage, reset, clean, commit, push, or promote source-control claims.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Draft Review")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- situation: %s\n", plan.ReviewDraft.Situation)
	fmt.Fprintf(&b, "- proposed_next_step: %s\n", plan.ReviewDraft.ProposedNextStep)
	fmt.Fprintf(&b, "- cleanup_guardrail: %s\n", plan.ReviewDraft.CleanupGuardrail)
	fmt.Fprintf(&b, "- verification: %s\n", plan.ReviewDraft.Verification)
	fmt.Fprintf(&b, "- retirement_trigger: %s\n", plan.ReviewDraft.RetirementTrigger)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Review YAML Template")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```yaml")
	fmt.Fprint(&b, RenderSourceControlReviewYAMLDraft(plan))
	fmt.Fprintln(&b, "```")
	if len(plan.Warnings) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Warnings")
		fmt.Fprintln(&b)
		for _, warning := range plan.Warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
	}
	return b.String()
}

func RenderSourceControlReviewYAMLDraft(plan SourceControlReviewPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", SourceControlReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintf(&b, "repo_id: %q\n", plan.RepoID)
	decisions := "TODO_decision_owner_action_required_owner_action_done_parked_no_action"
	if plan.MutationBinding != nil {
		decisions += "_authorize_mutation"
	}
	fmt.Fprintln(&b, "decision: "+decisions)
	fmt.Fprintln(&b, "rationale: \"\"")
	fmt.Fprintln(&b, "confidence: -1")
	if plan.MutationBinding != nil {
		fmt.Fprintln(&b, "reviewed_by: TODO_operator_provenance")
	}
	fmt.Fprintln(&b, "next_action: \"TODO: reviewed source-control cleanup next action\"")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, " - %q\n", ref)
	}
	return b.String()
}

func LoadSourceControlReview(path string) (SourceControlReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SourceControlReview{}, err
	}
	var review SourceControlReview
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&review); err != nil {
		return SourceControlReview{}, err
	}
	return review, nil
}

func ValidateSourceControlReview(review SourceControlReview) error {
	if review.SchemaVersion != SourceControlReviewSchemaVersion {
		return fmt.Errorf("source-control review schema_version must be %s, got %q", SourceControlReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.PlanID) == "" {
		return fmt.Errorf("source-control review id plan_id required")
	}
	if review.ID != filepath.Base(review.ID) {
		return fmt.Errorf("source-control review id %q must be bare id", review.ID)
	}
	if review.PlanID != filepath.Base(review.PlanID) {
		return fmt.Errorf("source-control review plan_id %q must be bare id", review.PlanID)
	}
	if strings.TrimSpace(review.RepoID) == "" {
		return fmt.Errorf("source-control review repo_id required")
	}
	if !sourceControlReviewOneOf(review.Decision, []string{"owner_action_required", "owner_action_done", "parked", "no_action", "authorize_mutation"}) {
		return fmt.Errorf("source-control review decision must be owner_action_required|owner_action_done|parked|no_action|authorize_mutation, got %q", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("source-control review rationale required")
	}
	if math.IsNaN(review.Confidence) || math.IsInf(review.Confidence, 0) || review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("source-control review confidence must be between 0 and 1")
	}
	if len(review.EvidenceRefs) == 0 {
		return fmt.Errorf("source-control review evidence_refs required")
	}
	for _, ref := range review.EvidenceRefs {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("source-control review evidence_refs must not contain empty entries")
		}
	}
	if review.ReviewedAt != "" {
		reviewedAt, err := time.Parse(time.RFC3339, review.ReviewedAt)
		if err != nil || reviewedAt.IsZero() {
			return fmt.Errorf("source-control review reviewed_at must be a non-zero RFC3339 timestamp")
		}
	}
	if review.Decision == "authorize_mutation" && !policy.HasOperatorProvenance(review.ReviewedBy) {
		return fmt.Errorf("source-control authorize_mutation reviewed_by with operator provenance required")
	}
	return nil
}

// SourceControlReviewSourceDigest returns the canonical semantic digest of a
// validated review source. YAML presentation details are intentionally absent;
// every typed disposition field remains part of the identity.
func SourceControlReviewSourceDigest(review SourceControlReview) (string, error) {
	if err := ValidateSourceControlReview(review); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(review)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func RecordSourceControlReview(homePath string, review SourceControlReview, sourcePath string, now time.Time, expectedSourceDigest ...string) (SourceControlReviewReport, error) {
	createdAt := reviewartifact.Timestamp(now)
	report := SourceControlReviewReport{
		SchemaVersion: SourceControlReviewReportSchemaVersion,
		ID:            reviewartifact.ID("scrr_", review.PlanID, review.ID, review.Decision, createdAt),
		CreatedAt:     createdAt,
		Status:        "recorded",
		PlanID:        review.PlanID,
		ReviewID:      review.ID,
		RepoID:        review.RepoID,
		Decision:      review.Decision,
		ReviewedBy:    strings.TrimSpace(review.ReviewedBy),
		NextAction:    strings.TrimSpace(review.NextAction),
		SourcePath:    strings.TrimSpace(sourcePath),
		EvidenceRefs:  reviewartifact.Unique(review.EvidenceRefs),
		Warnings: []string{
			"does not stage files",
			"does not reset checkout clean stash commit pull or push",
			"does not promote source-control claims",
			"does not resolve policy holds by itself",
		},
	}
	if err := ValidateSourceControlReview(review); err != nil {
		report.Status = "blocked"
		return report, err
	}
	expectedDigest, err := validateExpectedSourceControlReviewDigest(expectedSourceDigest)
	if err != nil {
		report.Status = "blocked"
		return report, err
	}
	if expectedDigest != "" {
		if err := verifySourceControlReviewSourceDigest(sourcePath, review, expectedDigest); err != nil {
			report.Status = "blocked"
			return report, err
		}
	}
	expectedReview := review
	plan, err := loadSourceControlPlanForReview(sourcePath, review.PlanID)
	if err != nil {
		report.Status = "blocked"
		return report, err
	}
	if plan.ID != review.PlanID {
		report.Status = "blocked"
		return report, fmt.Errorf("source-control review plan_id %q does not match plan %q", review.PlanID, plan.ID)
	}
	if review.ID != plan.ReviewDraft.ReviewID {
		report.Status = "blocked"
		return report, fmt.Errorf("source-control review id %q does not match plan review_id %q", review.ID, plan.ReviewDraft.ReviewID)
	}
	expectedSourcePath := filepath.Join(filepath.Dir(sourcePath), plan.ID+".review.yaml")
	if filepath.Clean(sourcePath) != filepath.Clean(expectedSourcePath) {
		report.Status = "blocked"
		return report, fmt.Errorf("source-control review source path must be %s", expectedSourcePath)
	}
	if strings.TrimSpace(plan.RepoID) != "" && review.RepoID != plan.RepoID {
		report.Status = "blocked"
		return report, fmt.Errorf("source-control review repo_id %q does not match plan repo_id %q", review.RepoID, plan.RepoID)
	}
	computedPlanDigest := SourceControlReviewPlanDigest(plan)
	if plan.PlanDigest != "" && plan.PlanDigest != computedPlanDigest {
		report.Status = "blocked"
		return report, fmt.Errorf("source-control review plan %s digest mismatch", plan.ID)
	}
	if review.Decision == "authorize_mutation" {
		if !sourceControlPlanAuthorizable(plan) {
			report.Status = "blocked"
			return report, fmt.Errorf("source-control authorize_mutation requires exact fingerprint and mutation-bound plan")
		}
		if !containsEvery(review.EvidenceRefs, plan.EvidenceRefs) {
			report.Status = "blocked"
			return report, fmt.Errorf("source-control authorize_mutation evidence_refs must include every plan evidence ref")
		}
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = createdAt
	}
	review.ReviewedBy = strings.TrimSpace(review.ReviewedBy)
	review.SourceRef = "source-control-cleanup-review:" + filepath.Base(sourcePath)
	report.RepoID = reviewartifact.FirstNonEmpty(report.RepoID, plan.RepoID)
	report.WorkID = plan.WorkID
	report.GitRoot = plan.GitRoot
	report.Remote = plan.Remote
	report.Head = plan.Head
	report.ChangedStateFingerprint = plan.ChangedStateFingerprint
	report.MutationBinding = cloneSourceControlMutationBinding(plan.MutationBinding)
	report.PlanDigest = computedPlanDigest
	report.ReviewedBy = review.ReviewedBy
	report.ReviewedAt = review.ReviewedAt
	report.EvidenceRefs = reviewartifact.Unique(append(append([]string{}, plan.EvidenceRefs...), report.EvidenceRefs...))
	report.EvidenceRefs = reviewartifact.Unique(append(report.EvidenceRefs, "source-control-review:"+report.ID))
	if expectedDigest != "" {
		if err := verifySourceControlReviewSourceDigest(sourcePath, expectedReview, expectedDigest); err != nil {
			report.Status = "blocked"
			return report, err
		}
	}
	sourceDigest, err := writeSourceControlReviewedSource(sourcePath, review)
	if err != nil {
		return SourceControlReviewReport{}, err
	}
	report.SourceDigest = sourceDigest
	report.EvidenceRefs = reviewartifact.Unique(append(report.EvidenceRefs, sourceDigest))
	if review.ReviewedBy != "" {
		report.EvidenceRefs = reviewartifact.Unique(append(report.EvidenceRefs, "source-control-reviewer:"+review.ReviewedBy))
	}
	if review.Decision == "authorize_mutation" {
		report.Warnings = []string{
			"authorizes only the exact recorded repo/head/fingerprint/work/mutation binding",
			"any state, session payload, action, class, capability, or target-set drift fails closed",
		}
	}
	report.ReviewPath = filepath.Join(filepath.Dir(sourcePath), sourceControlReviewReportName(review.PlanID, review.ID))
	report.AuthorizationDigest = sourceControlReviewReportDigest(report)
	if _, err := reviewartifact.WriteJSON(homePath, SourceControlCleanupReviewArea, filepath.Base(report.ReviewPath), report, " "); err != nil {
		return SourceControlReviewReport{}, err
	}
	return report, nil
}

func validateExpectedSourceControlReviewDigest(values []string) (string, error) {
	if len(values) > 1 {
		return "", fmt.Errorf("source-control review expected source digest accepts at most one value")
	}
	if len(values) == 0 {
		return "", nil
	}
	digest := strings.TrimSpace(values[0])
	if !validSHA256(digest) {
		return "", fmt.Errorf("source-control review expected source digest must be sha256")
	}
	return digest, nil
}

func verifySourceControlReviewSourceDigest(path string, review SourceControlReview, expectedDigest string) error {
	liveReview, err := LoadSourceControlReview(path)
	if err != nil {
		return fmt.Errorf("source digest drift: load current source: %w", err)
	}
	liveDigest, err := SourceControlReviewSourceDigest(liveReview)
	if err != nil {
		return fmt.Errorf("source digest drift: current source invalid: %w", err)
	}
	providedDigest, err := SourceControlReviewSourceDigest(review)
	if err != nil {
		return err
	}
	if liveDigest != expectedDigest || providedDigest != expectedDigest {
		return fmt.Errorf("source digest drift: expected %s, current %s", expectedDigest, liveDigest)
	}
	return nil
}

// FindRecordedSourceControlMutationReview returns the newest recorded
// authorization whose immutable plan, live snapshot, and executable binding all
// match. Invalid, legacy, stale, or tampered artifacts are ignored so policy
// keeps the dirty hold.
func FindRecordedSourceControlMutationReview(homePath string, snapshot SourceControlSnapshot, binding SourceControlMutationBinding) (*SourceControlReviewReport, error) {
	reports, err := ListRecordedSourceControlMutationReviews(homePath, snapshot, binding)
	if err != nil || len(reports) == 0 {
		return nil, err
	}
	report := reports[0]
	return &report, nil
}

// ListRecordedSourceControlMutationReviews enumerates every semantically exact,
// sealed authorization newest-first. Owner lifecycle is deliberately checked by
// the hook/CLI boundary that owns the WorkEnvelope store; keeping all exact
// historical reports visible lets that boundary skip an unusable owner without
// deleting or rewriting immutable review evidence.
func ListRecordedSourceControlMutationReviews(homePath string, snapshot SourceControlSnapshot, binding SourceControlMutationBinding) ([]SourceControlReviewReport, error) {
	normalized, err := normalizeSourceControlMutationBinding(snapshot, binding)
	if err != nil {
		return nil, nil
	}
	dir := filepath.Join(homePath, "reviews", SourceControlCleanupReviewArea)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type matchedReview struct {
		report    SourceControlReviewReport
		createdAt time.Time
	}
	matches := make([]matchedReview, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".reviewed.json") {
			continue
		}
		reportPath := filepath.Join(dir, entry.Name())
		report, loadErr := LoadSourceControlReviewReport(reportPath)
		if loadErr != nil || report.Status != "recorded" || report.Decision != "authorize_mutation" || report.MutationBinding == nil {
			continue
		}
		if filepath.Clean(report.ReviewPath) != filepath.Clean(reportPath) || !sourceControlAuthorizationReportSealed(report, dir) {
			continue
		}
		plan, planErr := loadSourceControlPlanFile(filepath.Join(dir, report.PlanID+".json"))
		if planErr != nil || !recordedSourceControlReportMatchesPlan(report, plan) {
			continue
		}
		if !sourceControlPlanMatchesSnapshot(plan, snapshot) || !sourceControlMutationExecutionEqual(*plan.MutationBinding, normalized) {
			continue
		}
		createdAt, parseErr := time.Parse(time.RFC3339Nano, report.CreatedAt)
		if parseErr != nil {
			continue
		}
		matches = append(matches, matchedReview{report: report, createdAt: createdAt})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].createdAt.Equal(matches[j].createdAt) {
			return matches[i].createdAt.After(matches[j].createdAt)
		}
		return matches[i].report.ID > matches[j].report.ID
	})
	reports := make([]SourceControlReviewReport, len(matches))
	for i := range matches {
		reports[i] = matches[i].report
	}
	return reports, nil
}

// SourceControlMutationOwnerCurrent is the one lifecycle predicate shared by
// mutation-review creation and consumption. work_live_* identifies a synthetic
// hook/session correlation id, never a durable owner WorkEnvelope.
func SourceControlMutationOwnerCurrent(workID, state string) bool {
	workID = strings.TrimSpace(workID)
	if workID == "" || !strings.HasPrefix(workID, "work_") || strings.HasPrefix(workID, "work_live_") {
		return false
	}
	switch strings.TrimSpace(state) {
	case "claimed", "running", "verifying", "held":
		return true
	default:
		return false
	}
}

// sourceControlReviewReportName is the single owner of the recorded-disposition
// file name. ValidateSourceControlReviewPlan pins review_id == plan_id, so
// joining the pair produced a doubled `<id>.<id>.reviewed.json` stem that no
// sidecar reader stats — the core item stayed non-terminal forever and its
// review area could never drain. De-duping keeps the disposition on the sidecar
// stem its own core item owns while the joined form survives for any future
// plan whose review id genuinely differs.
func sourceControlReviewReportName(planID, reviewID string) string {
	if planID == reviewID {
		return planID + ".reviewed.json"
	}
	return planID + "." + reviewID + ".reviewed.json"
}

// sourceControlLegacyReviewReportName is the pre-de-dupe doubled stem. Recorded
// dispositions are immutable evidence, so readers must keep accepting it.
func sourceControlLegacyReviewReportName(planID, reviewID string) string {
	return planID + "." + reviewID + ".reviewed.json"
}

func writeSourceControlReviewedSource(path string, review SourceControlReview) (string, error) {
	data, err := yaml.Marshal(review)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func sourceControlReviewReportDigest(report SourceControlReviewReport) string {
	report.AuthorizationDigest = ""
	encoded, _ := json.Marshal(report)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:])
}

func sourceControlAuthorizationReportSealed(report SourceControlReviewReport, dir string) bool {
	return report.Decision == "authorize_mutation" && policy.HasOperatorProvenance(report.ReviewedBy) &&
		sourceControlReviewReportSealed(report, dir)
}

func sourceControlReviewReportSealed(report SourceControlReviewReport, dir string) bool {
	if !validSHA256(report.SourceDigest) || !validSHA256(report.AuthorizationDigest) ||
		report.AuthorizationDigest != sourceControlReviewReportDigest(report) {
		return false
	}
	expectedSourcePath := filepath.Join(dir, report.PlanID+".review.yaml")
	if filepath.Clean(report.SourcePath) != filepath.Clean(expectedSourcePath) {
		return false
	}
	data, err := os.ReadFile(expectedSourcePath)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(data)
	if report.SourceDigest != fmt.Sprintf("sha256:%x", digest[:]) {
		return false
	}
	review, err := LoadSourceControlReview(expectedSourcePath)
	if err != nil || ValidateSourceControlReview(review) != nil {
		return false
	}
	return review.Decision == report.Decision && review.ID == report.ReviewID && review.PlanID == report.PlanID &&
		review.RepoID == report.RepoID && strings.TrimSpace(review.ReviewedBy) == report.ReviewedBy &&
		review.ReviewedAt == report.ReviewedAt && strings.TrimSpace(review.NextAction) == report.NextAction &&
		containsEvery(report.EvidenceRefs, review.EvidenceRefs)
}

func sourceControlPlanAuthorizable(plan SourceControlReviewPlan) bool {
	return strings.TrimSpace(plan.WorkID) != "" && strings.TrimSpace(plan.RepoID) != "" && strings.TrimSpace(plan.GitRoot) != "" &&
		strings.TrimSpace(plan.Head) != "" && validSHA256(plan.ChangedStateFingerprint) && plan.MutationBinding != nil &&
		validateNormalizedSourceControlMutationBinding(*plan.MutationBinding) == nil && validSHA256(plan.PlanDigest) &&
		plan.PlanDigest == SourceControlReviewPlanDigest(plan)
}

func recordedSourceControlReportMatchesPlan(report SourceControlReviewReport, plan SourceControlReviewPlan) bool {
	return report.PlanID == plan.ID && report.ReviewID == plan.ReviewDraft.ReviewID && report.RepoID == plan.RepoID && report.WorkID == plan.WorkID &&
		filepath.Clean(report.GitRoot) == filepath.Clean(plan.GitRoot) && report.Remote == plan.Remote && report.Head == plan.Head &&
		report.ChangedStateFingerprint == plan.ChangedStateFingerprint && report.PlanDigest == plan.PlanDigest &&
		report.PlanDigest == SourceControlReviewPlanDigest(plan) && report.MutationBinding != nil && plan.MutationBinding != nil &&
		sourceControlMutationArtifactEqual(*report.MutationBinding, *plan.MutationBinding)
}

func sourceControlPlanMatchesSnapshot(plan SourceControlReviewPlan, snapshot SourceControlSnapshot) bool {
	return plan.SourceStatus == "dirty" && plan.RepoID == snapshot.RepoID && filepath.Clean(plan.GitRoot) == filepath.Clean(snapshot.GitRoot) &&
		plan.Remote == snapshot.Remote && plan.Head == snapshot.Head && plan.ChangedStateFingerprint == snapshot.ChangedStateFingerprint
}

func normalizeSourceControlMutationBinding(snapshot SourceControlSnapshot, binding SourceControlMutationBinding) (SourceControlMutationBinding, error) {
	binding.ActionWorkID = strings.TrimSpace(binding.ActionWorkID)
	binding.MutationInputHash = strings.TrimSpace(binding.MutationInputHash)
	binding.Capability = strings.TrimSpace(binding.Capability)
	binding.Action = strings.TrimSpace(binding.Action)
	binding.MutationClass = strings.TrimSpace(binding.MutationClass)
	binding.ActionIntentIDs = sortedUniqueStrings(binding.ActionIntentIDs)
	root := filepath.Clean(snapshot.GitRoot)
	targets := make([]string, 0, len(binding.TargetPaths))
	for _, target := range binding.TargetPaths {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		var err error
		target, err = canonicalSourceControlMutationTarget(root, target)
		if err != nil {
			return SourceControlMutationBinding{}, err
		}
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return SourceControlMutationBinding{}, fmt.Errorf("source-control mutation target %q outside git root", target)
		}
		targets = append(targets, target)
	}
	binding.TargetPaths = sortedUniqueStrings(targets)
	if err := validateNormalizedSourceControlMutationBinding(binding); err != nil {
		return SourceControlMutationBinding{}, err
	}
	return binding, nil
}

// CanonicalSourceControlMutationTarget resolves a mutation target against the
// repository's canonical root. It preserves a future path's missing suffix
// while resolving symlinks in its nearest existing parent, so adapters bind
// /var and /private spellings (and equivalent worktree paths) identically.
func CanonicalSourceControlMutationTarget(root, target string) (string, error) {
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target = filepath.Clean(target)
	if canonical, err := filepath.EvalSymlinks(target); err == nil {
		return canonical, nil
	}
	parent, err := nearestExistingParent(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(parent, target)
	if err != nil {
		return "", err
	}
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(canonicalParent, rel)), nil
}

func canonicalSourceControlMutationTarget(root, target string) (string, error) {
	return CanonicalSourceControlMutationTarget(root, target)
}

func validateNormalizedSourceControlMutationBinding(binding SourceControlMutationBinding) error {
	if binding.ActionWorkID == "" || binding.Capability == "" || binding.Action == "" || binding.MutationClass == "" {
		return fmt.Errorf("source-control mutation binding action_work_id capability action mutation_class required")
	}
	if !strings.HasPrefix(binding.MutationInputHash, "mutation-input:sha256:") || !validHexDigest(strings.TrimPrefix(binding.MutationInputHash, "mutation-input:sha256:")) {
		return fmt.Errorf("source-control mutation binding valid mutation_input_hash required")
	}
	if len(binding.TargetPaths) == 0 || len(binding.ActionIntentIDs) == 0 {
		return fmt.Errorf("source-control mutation binding target_paths and action_intent_ids required")
	}
	return nil
}

func sourceControlMutationExecutionEqual(a, b SourceControlMutationBinding) bool {
	return a.ActionWorkID == b.ActionWorkID && a.MutationInputHash == b.MutationInputHash && a.Capability == b.Capability &&
		a.Action == b.Action && a.MutationClass == b.MutationClass && stringSlicesEqual(a.TargetPaths, b.TargetPaths)
}

func sourceControlMutationArtifactEqual(a, b SourceControlMutationBinding) bool {
	return sourceControlMutationExecutionEqual(a, b) && stringSlicesEqual(a.ActionIntentIDs, b.ActionIntentIDs)
}

func cloneSourceControlMutationBinding(binding *SourceControlMutationBinding) *SourceControlMutationBinding {
	if binding == nil {
		return nil
	}
	clone := *binding
	clone.TargetPaths = append([]string{}, binding.TargetPaths...)
	clone.ActionIntentIDs = append([]string{}, binding.ActionIntentIDs...)
	return &clone
}

func prefixedActionIntentRefs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "action-intent:"+id)
	}
	return out
}

func sortedUniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsEvery(have, required []string) bool {
	set := map[string]struct{}{}
	for _, value := range have {
		set[value] = struct{}{}
	}
	for _, value := range required {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validHexDigest(strings.TrimPrefix(value, "sha256:"))
}

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func LoadSourceControlReviewReport(path string) (SourceControlReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SourceControlReviewReport{}, err
	}
	var report SourceControlReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return SourceControlReviewReport{}, err
	}
	return report, nil
}

func AnnotateSourceControlCleanupQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != SourceControlCleanupReviewArea {
			continue
		}
		annotateSourceControlCleanupItem(item)
	}
}

func annotateSourceControlCleanupItem(item *reviewartifact.QueueItem) {
	if item.JSONPath == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "source-control cleanup plan path missing"
		item.NextAction = "regenerate dirty-plan"
		return
	}
	plan, planErr := readSourceControlPlanFile(item.JSONPath)
	if planErr != nil {
		item.Actions = nil
		item.Readiness = "blocked"
		item.BlockedReason = "source-control cleanup plan invalid: " + sourceControlQueueError(planErr)
		item.NextAction = "regenerate dirty-plan from a current source-control snapshot"
		return
	}
	planErr = ValidateSourceControlReviewPlan(plan)
	if planErr != nil {
		if sourceControlLegacyPlanIsNowClean(plan) {
			fileHistoricalSourceControlPlan(item, plan)
			return
		}
		item.Actions = nil
		item.Readiness = "blocked"
		item.BlockedReason = "source-control cleanup plan invalid: " + sourceControlQueueError(planErr)
		item.NextAction = "regenerate dirty-plan from a current source-control snapshot"
		return
	}
	if reviewedPath := sourceControlReviewedPath(*item); reviewedPath != "" {
		report, reportErr := LoadSourceControlReviewReport(reviewedPath)
		if sourceControlMutationReviewNeedsValidation(plan, planErr, report, reportErr) {
			if err := validateRecordedSourceControlMutationQueueItem(*item, plan, planErr, report, reportErr, reviewedPath); err != nil {
				blockSourceControlMutationQueueItem(item, plan, err)
				if planErr == nil {
					annotateSourceControlOperatorCard(item, plan, reviewartifact.BandNeedsYou)
				}
				return
			}
		}
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.NextAction = "source-control cleanup review recorded"
		if reportErr == nil {
			item.NextAction = reviewartifact.FirstNonEmpty(report.NextAction, "source-control cleanup review recorded: "+report.Decision)
		}
		item.Actions = nil
		if planErr == nil {
			annotateSourceControlOperatorCard(item, plan, reviewartifact.BandFiled)
		}
		return
	}
	if supersession, ok := sourceControlSupersession(*item); ok {
		fileSupersededSourceControlPlan(item, plan, supersession)
		return
	}
	currentSnapshot, snapshotErr := ReadGitSnapshot(plan.GitRoot)
	if snapshotErr != nil {
		blockSourceControlPlanCurrentState(
			item,
			plan,
			"cannot read current source-control state: "+sourceControlQueueError(snapshotErr),
			"restore repository visibility, then retry the read-only review projection",
			"The current repository state cannot be verified; this dirty source-control review is blocked.",
		)
		return
	}
	if currentSnapshot.Status == "clean" {
		fileHistoricalSourceControlPlan(item, plan)
		return
	}
	if !sourceControlReviewPlanMatchesSnapshot(plan, currentSnapshot) {
		blockSourceControlPlanCurrentState(
			item,
			plan,
			"current source-control state changed since this dirty-plan; stored review authority is stale",
			"regenerate dirty-plan from the current read-only source-control snapshot before reviewing",
			"The repository is still dirty, but this source-control review no longer matches the current state.",
		)
		return
	}
	annotateSourceControlOperatorCard(item, plan, reviewartifact.BandNeedsYou)
	reviewYAML := sourceControlReviewYAMLPath(*item)
	if reviewYAML == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "source-control cleanup review YAML draft missing"
		item.NextAction = "regenerate dirty-plan to create review YAML draft"
		return
	}
	review, err := LoadSourceControlReview(reviewYAML)
	if err != nil {
		item.Actions = nil
		item.Readiness = "blocked"
		item.BlockedReason = "source-control cleanup review YAML unreadable: " + sourceControlQueueError(err)
		item.NextAction = "fix source-control cleanup review YAML before recording"
		return
	}
	if err := ValidateSourceControlReview(review); err != nil {
		item.Actions = nil
		item.Readiness = "needs_review"
		item.BlockedReason = sourceControlQueueError(err)
		item.NextAction = "fill valid source-control cleanup review YAML before recording"
		return
	}
	sourceDigest, err := SourceControlReviewSourceDigest(review)
	if err != nil {
		item.Actions = nil
		item.Readiness = "blocked"
		item.BlockedReason = "source-control cleanup review digest invalid: " + sourceControlQueueError(err)
		item.NextAction = "fix source-control cleanup review YAML before recording"
		return
	}
	argv := []string{"compound", "authority", "dirty-review", reviewYAML, "--expected-source-digest", sourceDigest}
	action := reviewartifact.Action{
		ID:             "record_source_control_cleanup_review",
		Label:          "Record source-control cleanup review",
		Command:        sourceControlShellJoin(argv),
		Argv:           argv,
		RequiresReview: true,
	}
	action.ActionIntent, action.AuthorityContext = sourceControlRecordReviewAuthoritySeed(*item, plan, review, reviewYAML, sourceDigest, action.ID)
	if action.ActionIntent == nil || action.AuthorityContext == nil {
		item.Actions = nil
		item.Readiness = "blocked"
		item.BlockedReason = "source-control review owner authority is incomplete or invalid"
		item.NextAction = "regenerate dirty-plan under a durable owner work envelope"
		return
	}
	item.Actions = []reviewartifact.Action{action}
	item.Readiness = "ready_to_record"
	item.NextAction = "record reviewed source-control cleanup disposition"
}

func sourceControlLegacyPlanIsNowClean(plan SourceControlReviewPlan) bool {
	if plan.SchemaVersion != SourceControlReviewPlanSchemaVersion ||
		strings.TrimSpace(plan.RepoID) == "" ||
		!filepath.IsAbs(strings.TrimSpace(plan.GitRoot)) {
		return false
	}
	current, err := ReadGitSnapshot(plan.GitRoot)
	return err == nil && current.Status == "clean" && current.RepoID == plan.RepoID
}

func fileHistoricalSourceControlPlan(item *reviewartifact.QueueItem, plan SourceControlReviewPlan) {
	item.Status = "resolved"
	item.Readiness = "no_action"
	item.BlockedReason = ""
	item.NextAction = "historical dirty-plan evidence is superseded by the current clean repository; create a new dirty-plan only if uncommitted state returns"
	item.Actions = nil
	annotateSourceControlOperatorCard(item, plan, reviewartifact.BandFiled)
	item.OperatorSummary = "The repository is clean; this dirty source-control review is historical evidence."
	item.OperatorUnlock = ""
}

// fileSupersededSourceControlPlan files a plan the producer retired because a
// newer snapshot of the same repo path replaced it. Without this the retired
// item keeps asking the operator to review a question its successor already
// carries, which is exactly the pile the supersession exists to end.
func fileSupersededSourceControlPlan(item *reviewartifact.QueueItem, plan SourceControlReviewPlan, supersession SourceControlReviewSupersession) {
	item.Status = "resolved"
	item.Readiness = "no_action"
	item.BlockedReason = ""
	item.NextAction = "superseded by a newer snapshot of the same repo path: " + supersession.SupersededBy
	item.Actions = nil
	annotateSourceControlOperatorCard(item, plan, reviewartifact.BandFiled)
	item.OperatorSummary = "A newer dirty source-control review for this repository replaced this one; review " + supersession.SupersededBy + " instead."
	item.OperatorUnlock = ""
}

func sourceControlSupersession(item reviewartifact.QueueItem) (SourceControlReviewSupersession, bool) {
	want := sourceControlReviewSupersessionName(item.ID)
	for _, path := range item.ExtraPaths {
		if filepath.Base(path) != want {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return SourceControlReviewSupersession{}, false
		}
		var supersession SourceControlReviewSupersession
		if err := json.Unmarshal(data, &supersession); err != nil {
			return SourceControlReviewSupersession{}, false
		}
		if supersession.SchemaVersion != SourceControlReviewSupersessionSchemaVersion ||
			supersession.Status != "superseded" ||
			supersession.PlanID != item.ID ||
			strings.TrimSpace(supersession.SupersededBy) == "" {
			return SourceControlReviewSupersession{}, false
		}
		return supersession, true
	}
	return SourceControlReviewSupersession{}, false
}

func blockSourceControlPlanCurrentState(item *reviewartifact.QueueItem, plan SourceControlReviewPlan, reason, nextAction, summary string) {
	item.Status = "blocked"
	item.Readiness = "blocked"
	item.BlockedReason = reason
	item.NextAction = nextAction
	item.Actions = nil
	annotateSourceControlOperatorCard(item, plan, reviewartifact.BandNeedsYou)
	item.OperatorSummary = summary
	item.OperatorUnlock = nextAction
}

func sourceControlReviewPlanMatchesSnapshot(plan SourceControlReviewPlan, snapshot SourceControlSnapshot) bool {
	return snapshot.Status == "dirty" &&
		plan.RepoID == snapshot.RepoID &&
		filepath.Clean(plan.GitRoot) == filepath.Clean(snapshot.GitRoot) &&
		plan.Remote == snapshot.Remote &&
		plan.Branch == snapshot.Branch &&
		plan.Head == snapshot.Head &&
		plan.Upstream == snapshot.Upstream &&
		plan.Ahead == snapshot.Ahead &&
		plan.Behind == snapshot.Behind &&
		plan.Staged == snapshot.Staged &&
		plan.Unstaged == snapshot.Unstaged &&
		plan.Untracked == snapshot.Untracked &&
		plan.ChangedPaths == snapshot.ChangedPaths &&
		plan.ChangedStateFingerprint == snapshot.ChangedStateFingerprint
}

func sourceControlRecordReviewAuthoritySeed(item reviewartifact.QueueItem, plan SourceControlReviewPlan, review SourceControlReview, reviewYAML, sourceDigest, actionID string) (*envelope.ActionIntent, *policy.AuthorityContext) {
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(plan.CreatedAt))
	dir := filepath.Dir(item.JSONPath)
	validWorkID := len(plan.WorkID) > len("work_") && strings.HasPrefix(plan.WorkID, "work_") &&
		!strings.HasPrefix(plan.WorkID, "work_live_") && filepath.Base(plan.WorkID) == plan.WorkID
	validPlanIdentity := ValidateSourceControlReviewPlan(plan) == nil && plan.ID == item.ID &&
		filepath.Clean(item.JSONPath) == filepath.Clean(filepath.Join(dir, plan.ID+".json"))
	validReviewIdentity := plan.ReviewDraft.ReviewID != "" && filepath.Base(plan.ReviewDraft.ReviewID) == plan.ReviewDraft.ReviewID &&
		filepath.Clean(reviewYAML) == filepath.Clean(filepath.Join(dir, plan.ID+".review.yaml")) &&
		ValidateSourceControlReview(review) == nil && review.ID == plan.ReviewDraft.ReviewID && review.PlanID == plan.ID && review.RepoID == plan.RepoID
	computedSourceDigest, digestErr := SourceControlReviewSourceDigest(review)
	validReviewIdentity = validReviewIdentity && digestErr == nil && validSHA256(sourceDigest) && sourceDigest == computedSourceDigest
	if err != nil || createdAt.IsZero() || !validWorkID || !validPlanIdentity || !validReviewIdentity || strings.TrimSpace(actionID) == "" {
		return nil, nil
	}

	evidenceRefs := reviewartifact.Unique(append(append([]string{}, plan.EvidenceRefs...), review.EvidenceRefs...))
	evidenceRefs = reviewartifact.Unique(append(evidenceRefs, sourceDigest))
	hasEvidence := false
	for _, ref := range evidenceRefs {
		if strings.TrimSpace(ref) != "" {
			hasEvidence = true
			break
		}
	}
	if !hasEvidence {
		return nil, nil
	}
	planPath := filepath.Clean(item.JSONPath)
	reviewYAML = filepath.Clean(reviewYAML)
	reviewReportPath := filepath.Join(dir, sourceControlReviewReportName(plan.ID, plan.ReviewDraft.ReviewID))
	actionIntent := &envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID: "act_" + reviewartifact.Digest12(strings.Join([]string{
				"source-control-cleanup", actionID, plan.ID, plan.WorkID, filepath.Clean(reviewYAML),
				review.Decision, strings.TrimSpace(review.ReviewedBy), strings.TrimSpace(review.NextAction), strings.Join(review.EvidenceRefs, "\x1f"), sourceDigest,
			}, "\x00")),
			CreatedAt:    strings.TrimSpace(plan.CreatedAt),
			Producer:     envelope.Producer{Kind: "owner_projection", Name: "compound"},
			IDs:          envelope.IDs{WorkID: plan.WorkID},
			Authority:    envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:      "private",
			Risk:         "tier1",
			EvidenceRefs: evidenceRefs,
			Uncertainty:  envelope.Uncertainty{Level: "known"},
		},
		Actor:         "compound",
		Capability:    "review.source_control",
		Action:        actionID,
		Target:        envelope.ActionTarget{Kind: "file", Path: reviewYAML},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "Record the validated " + review.Decision + " source-control review at " + sourceDigest + ".",
		RollbackPlan: "Restore " + reviewYAML + " from the immutable review draft in " + planPath +
			" at " + plan.PlanDigest + " and archive " + reviewReportPath + " from active review state.",
	}
	if err := actionIntent.Validate(); err != nil {
		return nil, nil
	}
	authorityContext := &policy.AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    policy.RecoverabilityVerified,
		OperatorControlContinuity: policy.OperatorControlContinuous,
		ExpectedReceipt:           "Recorded source-control cleanup review report for " + sourceDigest + " linked to work " + plan.WorkID + "; repository state remains unchanged.",
		ExpectedProof: append([]string{
			"review-report:" + reviewReportPath,
			"review-source:" + sourceDigest,
			"work:" + plan.WorkID,
			"recovery-source:" + planPath + "#" + plan.PlanDigest,
			"restore-target:" + reviewYAML,
		}, evidenceRefs...),
		EvidenceRefs: evidenceRefs,
	}
	return actionIntent, authorityContext
}

func sourceControlMutationReviewNeedsValidation(plan SourceControlReviewPlan, planErr error, report SourceControlReviewReport, reportErr error) bool {
	return (planErr == nil && plan.MutationBinding != nil) ||
		(reportErr == nil && (report.Decision == "authorize_mutation" || report.MutationBinding != nil))
}

func validateRecordedSourceControlMutationQueueItem(item reviewartifact.QueueItem, plan SourceControlReviewPlan, planErr error, report SourceControlReviewReport, reportErr error, reviewedPath string) error {
	if planErr != nil {
		return fmt.Errorf("mutation-bound plan unreadable: %w", planErr)
	}
	if reportErr != nil {
		return fmt.Errorf("recorded mutation review unreadable: %w", reportErr)
	}
	if plan.SchemaVersion != SourceControlReviewPlanSchemaVersion || plan.Status != "review_required" || plan.SourceStatus != "dirty" ||
		plan.ID != item.ID || strings.TrimSpace(plan.ReviewDraft.ReviewID) == "" || !sourceControlPlanAuthorizable(plan) {
		return fmt.Errorf("mutation-bound plan identity or digest is invalid")
	}
	dir := filepath.Dir(item.JSONPath)
	expectedPlanPath := filepath.Join(dir, plan.ID+".json")
	expectedReviewYAMLPath := filepath.Join(dir, plan.ID+".review.yaml")
	expectedReportPath := filepath.Join(dir, sourceControlReviewReportName(plan.ID, plan.ReviewDraft.ReviewID))
	// A disposition recorded before the de-dupe sits at the doubled stem and can
	// never be rewritten. Accept it, but only when the file and the report's own
	// self-recorded path agree on that one form — the triple identity check below
	// stays exact either way.
	if legacy := filepath.Join(dir, sourceControlLegacyReviewReportName(plan.ID, plan.ReviewDraft.ReviewID)); filepath.Clean(reviewedPath) == filepath.Clean(legacy) {
		expectedReportPath = legacy
	}
	if filepath.Clean(item.JSONPath) != filepath.Clean(expectedPlanPath) ||
		filepath.Clean(sourceControlReviewYAMLPath(item)) != filepath.Clean(expectedReviewYAMLPath) ||
		filepath.Clean(reviewedPath) != filepath.Clean(expectedReportPath) ||
		filepath.Clean(report.ReviewPath) != filepath.Clean(expectedReportPath) {
		return fmt.Errorf("mutation authorization artifact path or identity is invalid")
	}
	if report.SchemaVersion != SourceControlReviewReportSchemaVersion || report.Status != "recorded" ||
		!recordedSourceControlReportMatchesPlan(report, plan) {
		return fmt.Errorf("mutation authorization report does not match its plan")
	}
	if report.Decision == "authorize_mutation" {
		if !sourceControlAuthorizationReportSealed(report, dir) {
			return fmt.Errorf("mutation authorization report or reviewed source seal is invalid")
		}
		return nil
	}
	if !sourceControlReviewReportSealed(report, dir) {
		return fmt.Errorf("mutation review report or reviewed source seal is invalid")
	}
	return nil
}

func blockSourceControlMutationQueueItem(item *reviewartifact.QueueItem, plan SourceControlReviewPlan, cause error) {
	item.Status = "blocked"
	item.Readiness = "blocked"
	item.BlockedReason = "source-control mutation authorization invalid or stale: " + sourceControlQueueError(cause)
	item.NextAction = "regenerate the mutation-bound dirty-plan from persisted action intents and re-review before retrying the mutation"
	item.Actions = sourceControlMutationReviewRegenerationActions(plan)
}

func sourceControlMutationReviewRegenerationActions(plan SourceControlReviewPlan) []reviewartifact.Action {
	if strings.TrimSpace(plan.GitRoot) == "" || strings.TrimSpace(plan.WorkID) == "" || plan.MutationBinding == nil || len(plan.MutationBinding.ActionIntentIDs) == 0 {
		return nil
	}
	argv := []string{"compound", "authority", "dirty-plan", plan.GitRoot, "--work-id", plan.WorkID}
	for _, id := range sortedUniqueStrings(plan.MutationBinding.ActionIntentIDs) {
		argv = append(argv, "--action-intent", id)
	}
	argv = append(argv, "--json")
	return []reviewartifact.Action{{
		ID:         "regenerate_source_control_mutation_review",
		Label:      "Regenerate mutation-bound source-control review",
		Command:    sourceControlShellJoin(argv),
		Argv:       argv,
		PolicyNote: "Creates fresh review evidence only; it does not mutate the repository or authorize the mutation.",
	}}
}

func annotateSourceControlOperatorCard(item *reviewartifact.QueueItem, plan SourceControlReviewPlan, band string) {
	card, err := OperatorCardForSourceControlReviewPlan(plan)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "source-control cleanup operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "source-control cleanup operator card invalid: "+err.Error())
	}
}

func loadSourceControlPlanFile(path string) (SourceControlReviewPlan, error) {
	plan, err := readSourceControlPlanFile(path)
	if err != nil {
		return SourceControlReviewPlan{}, err
	}
	if err := ValidateSourceControlReviewPlan(plan); err != nil {
		return SourceControlReviewPlan{}, err
	}
	return plan, nil
}

func readSourceControlPlanFile(path string) (SourceControlReviewPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SourceControlReviewPlan{}, err
	}
	var plan SourceControlReviewPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return SourceControlReviewPlan{}, err
	}
	return plan, nil
}

func loadSourceControlPlanForReview(reviewPath, planID string) (SourceControlReviewPlan, error) {
	if strings.TrimSpace(reviewPath) == "" {
		return SourceControlReviewPlan{}, fmt.Errorf("source-control review source path required")
	}
	if strings.TrimSpace(planID) == "" || planID != filepath.Base(planID) {
		return SourceControlReviewPlan{}, fmt.Errorf("source-control review plan_id %q must be bare id", planID)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(reviewPath), planID+".json"))
	if err != nil {
		return SourceControlReviewPlan{}, err
	}
	var plan SourceControlReviewPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return SourceControlReviewPlan{}, err
	}
	if err := ValidateSourceControlReviewPlan(plan); err != nil {
		return SourceControlReviewPlan{}, err
	}
	return plan, nil
}

func sourceControlReviewYAMLPath(item reviewartifact.QueueItem) string {
	want := item.ID + ".review.yaml"
	for _, path := range item.ExtraPaths {
		if filepath.Base(path) == want {
			return path
		}
	}
	return ""
}

func sourceControlReviewedPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		name := filepath.Base(path)
		if strings.HasPrefix(name, item.ID+".") && strings.HasSuffix(name, ".reviewed.json") {
			return path
		}
	}
	return ""
}

func sourceControlReviewOneOf(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func sourceControlShellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		if strings.ContainsAny(arg, " \t\n\"'") {
			parts = append(parts, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

func sourceControlQueueError(err error) string {
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "unknown source-control cleanup review error"
	}
	return msg
}
