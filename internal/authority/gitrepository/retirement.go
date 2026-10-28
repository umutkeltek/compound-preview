// Retirement wire identities and pure validators are shared by the existing
// Authority producer and Work consumer. Planning and mutations stay in Authority.
package gitrepository

import (
	"compound/internal/policy"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	LegacyWorktreeRetirementPlanSchemaVersion   = "compound.worktree_retirement_plan.v0.1"
	WorktreeRetirementPlanSchemaVersion         = "compound.worktree_retirement_plan.v0.2"
	WorktreeRetirementReviewSchemaVersion       = "compound.worktree_retirement_review.v0.1"
	WorktreeRetirementReviewReportSchemaVersion = "compound.worktree_retirement_review_report.v0.1"
	WorktreeRetirementResultSchemaVersion       = "compound.worktree_retirement_result.v0.1"
	WorktreeCandidateContained                  = "contained"
	WorktreeCandidatePatchEquivalent            = "patch_equivalent"
	WorktreeCandidateArchivedUnique             = "archived_unique"
)

type WorktreeLivenessState string

type WorktreeLivenessObservation struct {
	State    WorktreeLivenessState `json:"state"`
	Evidence string                `json:"evidence,omitempty"`
}

type WorktreeRetirementRecoveryProof struct {
	Reference string `json:"reference"`
	Kind      string `json:"kind"`
	Digest    string `json:"digest"`
	Path      string `json:"path"`
}

type WorktreeRetirementOwnerProof struct {
	WorkID          string `json:"work_id"`
	State           string `json:"state"`
	LeaseHolder     string `json:"lease_holder"`
	LeaseClaimedAt  string `json:"lease_claimed_at"`
	LeaseWorkSHA256 string `json:"lease_work_sha256"`
	WorkDigest      string `json:"work_digest"`
}

type WorktreeRetirementSourceProof struct {
	WorkID                string                            `json:"work_id"`
	State                 string                            `json:"state"`
	ProofComplete         bool                              `json:"proof_complete"`
	ActionIntentIDs       []string                          `json:"action_intent_ids"`
	PolicyDecisionID      string                            `json:"policy_decision_id"`
	ReceiptID             string                            `json:"receipt_id"`
	ReceiptOutcome        string                            `json:"receipt_outcome"`
	TransitionID          string                            `json:"transition_id"`
	CommitArtifactRef     string                            `json:"commit_artifact_ref"`
	CommitTip             string                            `json:"commit_tip"`
	LandCheckID           string                            `json:"land_check_id"`
	LandCheckEvidenceRef  string                            `json:"land_check_evidence_ref"`
	LandCheckStatus       string                            `json:"land_check_status"`
	LandCheckCandidateTip string                            `json:"land_check_candidate_tip"`
	LandCheckFileDigest   string                            `json:"land_check_file_digest"`
	Delivery              *WorktreeRetirementSourceDelivery `json:"delivery,omitempty"`
}

// WorktreeRetirementSourceDelivery is projected by Work from the terminal
// envelope only after its receipt and sealed, current land-check agree.
type WorktreeRetirementSourceDelivery struct {
	WorkDigest            string `json:"work_digest"`
	TargetWorktreeRoot    string `json:"target_worktree_root"`
	RepositoryFingerprint string `json:"repository_fingerprint"`
	TargetLocalRef        string `json:"target_local_ref"`
	ExpectedBaseSHA       string `json:"expected_base_sha"`
	PublicationMode       string `json:"publication_mode"`
	PublicationRef        string `json:"publication_ref,omitempty"`
}

type WorktreeRetirementWorktree struct {
	Path       string                      `json:"path"`
	Head       string                      `json:"head"`
	BranchRef  string                      `json:"branch_ref,omitempty"`
	GitDir     string                      `json:"git_dir,omitempty"`
	Locked     bool                        `json:"locked"`
	LockReason string                      `json:"lock_reason,omitempty"`
	Prunable   bool                        `json:"prunable"`
	Clean      bool                        `json:"clean"`
	Liveness   WorktreeLivenessObservation `json:"liveness"`
}

type WorktreeRetirementPlan struct {
	SchemaVersion         string                            `json:"schema_version"`
	ID                    string                            `json:"id"`
	CreatedAt             string                            `json:"created_at"`
	Status                string                            `json:"status"`
	RepoRoot              string                            `json:"repo_root"`
	RepositoryFingerprint string                            `json:"repository_fingerprint"`
	Worktree              *WorktreeRetirementWorktree       `json:"worktree,omitempty"`
	CandidateRef          string                            `json:"candidate_ref,omitempty"`
	CandidateTip          string                            `json:"candidate_tip"`
	AuthorityRef          string                            `json:"authority_ref"`
	AuthorityTip          string                            `json:"authority_tip"`
	PublicationRef        string                            `json:"publication_ref"`
	PublicationTip        string                            `json:"publication_tip"`
	CandidateRelation     string                            `json:"candidate_relation"`
	SourceRelation        string                            `json:"source_relation"`
	OwnerProof            WorktreeRetirementOwnerProof      `json:"owner_proof"`
	SourceProof           WorktreeRetirementSourceProof     `json:"source_proof"`
	RecoveryRefs          []string                          `json:"recovery_refs"`
	RecoveryProofs        []WorktreeRetirementRecoveryProof `json:"recovery_proofs"`
	EvidenceRefs          []string                          `json:"evidence_refs"`
	Eligible              bool                              `json:"eligible"`
	BlockReasons          []string                          `json:"block_reasons"`
	PlanDigest            string                            `json:"plan_digest"`
}

type WorktreeRetirementReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	PlanDigest    string   `json:"plan_digest" yaml:"plan_digest"`
	Decision      string   `json:"decision" yaml:"decision"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	ReviewedBy    string   `json:"reviewed_by" yaml:"reviewed_by"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
}

type WorktreeRetirementReviewReport struct {
	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	Status              string   `json:"status"`
	PlanID              string   `json:"plan_id"`
	PlanDigest          string   `json:"plan_digest"`
	PlanPath            string   `json:"plan_path"`
	ReviewID            string   `json:"review_id"`
	Decision            string   `json:"decision"`
	Rationale           string   `json:"rationale"`
	ReviewedBy          string   `json:"reviewed_by"`
	ReviewedAt          string   `json:"reviewed_at"`
	SourcePath          string   `json:"source_path"`
	SourceDigest        string   `json:"source_digest"`
	ReviewPath          string   `json:"review_path"`
	AuthorizationDigest string   `json:"authorization_digest"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

type WorktreeRetirementResult struct {
	SchemaVersion       string `json:"schema_version"`
	ID                  string `json:"id"`
	CreatedAt           string `json:"created_at"`
	Status              string `json:"status"`
	Phase               string `json:"phase"`
	PlanID              string `json:"plan_id"`
	PlanDigest          string `json:"plan_digest"`
	AuthorizationDigest string `json:"authorization_digest"`
	Actor               string `json:"actor"`
	WorktreeRemoved     bool   `json:"worktree_removed"`
	RefDeleted          bool   `json:"ref_deleted"`
	AuthorityTip        string `json:"authority_tip"`
	PublicationTip      string `json:"publication_tip"`
	ActionIntentID      string `json:"action_intent_id,omitempty"`
	PolicyDecisionID    string `json:"policy_decision_id,omitempty"`
	ReceiptID           string `json:"receipt_id,omitempty"`
	TransitionID        string `json:"transition_id,omitempty"`
	LeaseWorkSHA256     string `json:"lease_work_sha256,omitempty"`
	WorkDigest          string `json:"work_digest,omitempty"`
	Error               string `json:"error,omitempty"`
	PendingPath         string `json:"pending_path"`
	ResultPath          string `json:"result_path"`
	ResultDigest        string `json:"result_digest"`
	Replayed            bool   `json:"replayed,omitempty"`
}

func WorktreeRetirementPlanID(plan WorktreeRetirementPlan) string {
	plan.ID = ""
	plan.PlanDigest = ""
	encoded, _ := json.Marshal(plan)
	return "wtr_" + DigestRetirementHex(encoded)[:12]
}

func WorktreeRetirementPlanDigest(plan WorktreeRetirementPlan) string {
	plan.PlanDigest = ""
	encoded, _ := json.Marshal(plan)
	return "sha256:" + DigestRetirementHex(encoded)
}

func ValidateRetirementOwnerProof(proof WorktreeRetirementOwnerProof) error {
	if strings.TrimSpace(proof.WorkID) == "" || proof.State != "running" || strings.TrimSpace(proof.LeaseHolder) == "" || strings.TrimSpace(proof.LeaseClaimedAt) == "" {
		return fmt.Errorf("running work id and lease holder/timestamp required")
	}
	if (!ValidRetirementHex(proof.LeaseWorkSHA256, 64) && !ValidRetirementDigest(proof.LeaseWorkSHA256)) || !ValidRetirementDigest(proof.WorkDigest) {
		return fmt.Errorf("lease/work digests required")
	}
	return nil
}

func ValidateRetirementSourceProof(proof WorktreeRetirementSourceProof) error {
	if d := proof.Delivery; d != nil {
		if !ValidRetirementDigest(d.WorkDigest) || !filepath.IsAbs(d.TargetWorktreeRoot) || filepath.Clean(d.TargetWorktreeRoot) != d.TargetWorktreeRoot || !ValidRetirementHex(d.RepositoryFingerprint, 64) || !strings.HasPrefix(d.TargetLocalRef, "refs/heads/") || !ValidRetirementObjectID(d.ExpectedBaseSHA) {
			return fmt.Errorf("source delivery identity invalid")
		}
		switch d.PublicationMode {
		case "local_only":
			if d.PublicationRef != "" {
				return fmt.Errorf("local-only source delivery must not name publication")
			}
		case "required":
			if !strings.HasPrefix(d.PublicationRef, "refs/remotes/") {
				return fmt.Errorf("source delivery publication ref required")
			}
		default:
			return fmt.Errorf("source delivery publication mode invalid")
		}
	}
	if strings.TrimSpace(proof.WorkID) == "" || (proof.State != "completed" && proof.State != "failed") || !proof.ProofComplete {
		return fmt.Errorf("completed or failed terminal proof chain required")
	}
	if len(proof.ActionIntentIDs) == 0 || strings.TrimSpace(proof.PolicyDecisionID) == "" || strings.TrimSpace(proof.ReceiptID) == "" || strings.TrimSpace(proof.TransitionID) == "" {
		return fmt.Errorf("action policy receipt transition ids required")
	}
	if proof.ReceiptOutcome != proof.State {
		return fmt.Errorf("terminal source state %s requires matching receipt outcome, got %s", proof.State, proof.ReceiptOutcome)
	}
	if !ValidRetirementObjectID(proof.CommitTip) || proof.CommitArtifactRef != "commit:"+proof.CommitTip {
		return fmt.Errorf("consistent full commit artifact required")
	}
	if proof.LandCheckStatus != "landed" || proof.LandCheckCandidateTip != proof.CommitTip {
		return fmt.Errorf("landed land-check matching commit required")
	}
	if !strings.HasPrefix(proof.LandCheckID, "landcheck_") || proof.LandCheckEvidenceRef != "land-check:"+proof.LandCheckID || !ValidRetirementDigest(proof.LandCheckFileDigest) {
		return fmt.Errorf("sealed land-check evidence required")
	}
	return nil
}

// RetirementRequiresPublication fails closed for historical/unbound evidence.
// A convenience land-check containment boolean is never publication authority.
func RetirementRequiresPublication(proof WorktreeRetirementSourceProof) bool {
	return ValidateRetirementSourceProof(proof) != nil || proof.Delivery == nil || proof.Delivery.PublicationMode != "local_only"
}

func ValidRetirementObjectID(value string) bool {
	return ValidRetirementHex(value, 40) || ValidRetirementHex(value, 64)
}

func ValidRetirementDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && ValidRetirementHex(strings.TrimPrefix(value, "sha256:"), 64)
}

func ValidRetirementHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func DigestRetirementHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ValidateWorktreeRetirementPlan(plan WorktreeRetirementPlan) error {
	switch plan.SchemaVersion {
	case LegacyWorktreeRetirementPlanSchemaVersion:
		if plan.SourceProof.Delivery != nil {
			return fmt.Errorf("historical retirement plan cannot carry v0.2 source delivery")
		}
	case WorktreeRetirementPlanSchemaVersion:
	default:
		return fmt.Errorf("worktree retirement plan schema_version must be %s", WorktreeRetirementPlanSchemaVersion)
	}
	if !strings.HasPrefix(plan.ID, "wtr_") || filepath.Base(plan.ID) != plan.ID {
		return fmt.Errorf("worktree retirement plan id invalid")
	}
	if !filepath.IsAbs(plan.RepoRoot) || !ValidRetirementHex(plan.RepositoryFingerprint, 64) {
		return fmt.Errorf("worktree retirement plan repository identity invalid")
	}
	if !ValidRetirementDigest(plan.PlanDigest) || plan.PlanDigest != WorktreeRetirementPlanDigest(plan) {
		return fmt.Errorf("worktree retirement plan digest mismatch")
	}
	if plan.ID != WorktreeRetirementPlanID(plan) {
		return fmt.Errorf("worktree retirement plan id invalid")
	}
	if !ValidRetirementObjectID(plan.AuthorityTip) {
		return fmt.Errorf("worktree retirement plan requires full sealed authority tips")
	}
	if !strings.HasPrefix(plan.AuthorityRef, "refs/heads/") {
		return fmt.Errorf("worktree retirement plan requires canonical authority/publication refs")
	}
	if RetirementRequiresPublication(plan.SourceProof) {
		if !ValidRetirementObjectID(plan.PublicationTip) || !strings.HasPrefix(plan.PublicationRef, "refs/remotes/") {
			return fmt.Errorf("worktree retirement plan publication invalid")
		}
	} else if plan.PublicationRef != "" || plan.PublicationTip != "" {
		return fmt.Errorf("local-only retirement plan must not name publication")
	}
	if d := plan.SourceProof.Delivery; d != nil && (plan.RepositoryFingerprint != d.RepositoryFingerprint || plan.AuthorityRef != d.TargetLocalRef || plan.PublicationRef != d.PublicationRef) {
		return fmt.Errorf("retirement target differs from source delivery authority")
	}
	if d := plan.SourceProof.Delivery; d != nil && plan.Worktree != nil && filepath.Clean(plan.Worktree.Path) == d.TargetWorktreeRoot {
		return fmt.Errorf("retained source delivery checkout cannot be retired")
	}
	if plan.CandidateRef == "" && plan.Worktree == nil {
		return fmt.Errorf("worktree retirement plan has no retirement target")
	}
	if plan.Eligible {
		if plan.Status != "review_required" || len(plan.BlockReasons) != 0 || (plan.CandidateRelation != WorktreeCandidateContained && plan.CandidateRelation != WorktreeCandidatePatchEquivalent && plan.CandidateRelation != WorktreeCandidateArchivedUnique) {
			return fmt.Errorf("eligible worktree retirement plan has inconsistent eligibility")
		}
		if !ValidRetirementObjectID(plan.CandidateTip) {
			return fmt.Errorf("eligible worktree retirement plan requires full candidate tip")
		}
		if err := ValidateRetirementOwnerProof(plan.OwnerProof); err != nil {
			return fmt.Errorf("worktree retirement owner proof: %w", err)
		}
		if err := ValidateRetirementSourceProof(plan.SourceProof); err != nil {
			return fmt.Errorf("worktree retirement source proof: %w", err)
		}
		if len(plan.RecoveryRefs) == 0 {
			return fmt.Errorf("worktree retirement recovery refs required")
		}
	} else if plan.Status != "blocked" || len(plan.BlockReasons) == 0 {
		return fmt.Errorf("blocked worktree retirement plan has no block reason")
	}
	return nil
}

func ValidateWorktreeRetirementReview(review WorktreeRetirementReview) error {
	if review.SchemaVersion != WorktreeRetirementReviewSchemaVersion || !strings.HasPrefix(review.ID, "wtrr_") || filepath.Base(review.PlanID) != review.PlanID {
		return fmt.Errorf("worktree retirement review identity invalid")
	}
	if review.Decision != "authorize_retirement" {
		return fmt.Errorf("worktree retirement review decision must be authorize_retirement")
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("worktree retirement review rationale required")
	}
	if !policy.HasOperatorProvenance(review.ReviewedBy) {
		return fmt.Errorf("worktree retirement review requires operator provenance")
	}
	if !ValidRetirementDigest(review.PlanDigest) || len(review.EvidenceRefs) == 0 {
		return fmt.Errorf("worktree retirement review plan digest and evidence required")
	}
	if review.ReviewedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, review.ReviewedAt); err != nil {
			return fmt.Errorf("worktree retirement review reviewed_at must be RFC3339Nano: %w", err)
		}
	}
	return nil
}

func WorktreeRetirementAuthorizationDigest(report WorktreeRetirementReviewReport) string {
	report.AuthorizationDigest = ""
	data, _ := json.Marshal(report)
	return "sha256:" + DigestRetirementHex(data)
}

func WorktreeRetirementResultDigest(result WorktreeRetirementResult) string {
	result.ResultDigest = ""
	result.Replayed = false
	data, _ := json.Marshal(result)
	return "sha256:" + DigestRetirementHex(data)
}

func ValidateWorktreeRetirementResult(result WorktreeRetirementResult, plan WorktreeRetirementPlan, report WorktreeRetirementReviewReport, path string) error {
	if result.SchemaVersion != WorktreeRetirementResultSchemaVersion || result.PlanID != plan.ID || result.PlanDigest != plan.PlanDigest || result.AuthorizationDigest != report.AuthorizationDigest || filepath.Clean(result.ResultPath) != filepath.Clean(path) {
		return fmt.Errorf("worktree retirement result identity mismatch")
	}
	if !ValidRetirementDigest(result.ResultDigest) || result.ResultDigest != WorktreeRetirementResultDigest(result) {
		return fmt.Errorf("worktree retirement result digest mismatch")
	}
	if result.Status != "completed" && result.Status != "partial" && result.Status != "failed" {
		return fmt.Errorf("worktree retirement result status invalid")
	}
	if strings.TrimSpace(result.ActionIntentID) == "" || strings.TrimSpace(result.PolicyDecisionID) == "" || strings.TrimSpace(result.ReceiptID) == "" || strings.TrimSpace(result.TransitionID) == "" {
		return fmt.Errorf("worktree retirement result proof chain incomplete")
	}
	if result.LeaseWorkSHA256 != plan.OwnerProof.LeaseWorkSHA256 || !ValidRetirementDigest(result.WorkDigest) {
		return fmt.Errorf("worktree retirement result owner proof invalid")
	}
	return nil
}
