package authority

import (
	"compound/internal/authority/gitrepository"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

const (
	WorktreeRetirementPendingSchemaVersion = "compound.worktree_retirement_pending.v0.1"
	WorktreeRetirementResultSchemaVersion  = gitrepository.WorktreeRetirementResultSchemaVersion

	WorktreeRetirementPhasePrepared               = "prepared"
	WorktreeRetirementPhaseWorktreeRemovalStarted = "worktree_removal_started"
	WorktreeRetirementPhaseWorktreeRemoved        = "worktree_removed"
	WorktreeRetirementPhaseRefDeletionStarted     = "ref_deletion_started"
	WorktreeRetirementPhaseRefDeleted             = "ref_deleted"
	WorktreeRetirementPhaseVerified               = "verified"
)

type WorktreeRetirementApplyRequest struct {
	HomePath         string
	PlanPath         string
	ReviewReportPath string
	Actor            string
	OwnerProof       WorktreeRetirementOwnerProof
	SourceProof      WorktreeRetirementSourceProof
}

type WorktreeRetirementActionOutcome struct {
	Action       envelope.ActionIntent   `json:"action"`
	Decision     envelope.PolicyDecision `json:"decision"`
	Outcome      string                  `json:"outcome"`
	Changed      bool                    `json:"changed"`
	Summary      string                  `json:"summary"`
	ProofRefs    []string                `json:"proof_refs"`
	BeforeRefs   []string                `json:"before_refs"`
	AfterRefs    []string                `json:"after_refs"`
	FailureCause *envelope.FailureCause  `json:"failure_cause,omitempty"`
}

type WorktreeRetirementActionProof struct {
	ActionIntentID   string `json:"action_intent_id"`
	PolicyDecisionID string `json:"policy_decision_id"`
	ReceiptID        string `json:"receipt_id"`
	TransitionID     string `json:"transition_id"`
	LeaseWorkSHA256  string `json:"lease_work_sha256"`
	WorkDigest       string `json:"work_digest"`
}

type WorktreeRetirementApplyDependencies struct {
	Git                        WorktreeRetirementGit
	Liveness                   WorktreeRetirementLiveness
	RecoveryResolver           WorktreeRetirementRecoveryResolver
	RecoveryInventoryPreflight func(WorktreeRetirementPlan) error
	SourceProofResolver        func(string) (WorktreeRetirementSourceProof, error)
	Evaluate                   func(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error)
	RecordOutcome              func(outcome WorktreeRetirementActionOutcome, now time.Time) (WorktreeRetirementActionProof, error)
	// AfterPhase is an injectable crash boundary used to prove durable resume.
	AfterPhase func(phase string) error
	// AfterGitMutation injects a crash after Git reports success but before the
	// corresponding completed phase is persisted.
	AfterGitMutation func(startedPhase string) error
}

type WorktreeRetirementPending struct {
	SchemaVersion       string                  `json:"schema_version"`
	PlanID              string                  `json:"plan_id"`
	PlanDigest          string                  `json:"plan_digest"`
	AuthorizationDigest string                  `json:"authorization_digest"`
	Actor               string                  `json:"actor"`
	Phase               string                  `json:"phase"`
	Action              envelope.ActionIntent   `json:"action"`
	Decision            envelope.PolicyDecision `json:"decision"`
	UpdatedAt           string                  `json:"updated_at"`
	PendingDigest       string                  `json:"pending_digest"`
}

type WorktreeRetirementResult = gitrepository.WorktreeRetirementResult

func ApplyWorktreeRetirement(ctx context.Context, req WorktreeRetirementApplyRequest, deps WorktreeRetirementApplyDependencies, now time.Time) (WorktreeRetirementResult, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if deps.Git == nil {
		deps.Git = ExecWorktreeRetirementGit{}
	}
	if deps.Liveness == nil {
		deps.Liveness = LSOFWorktreeRetirementLiveness{}
	}
	if deps.RecoveryResolver == nil {
		deps.RecoveryResolver = SHA256FileWorktreeRetirementRecoveryResolver{}
	}
	if deps.Evaluate == nil {
		deps.Evaluate = func(action envelope.ActionIntent, at time.Time) (envelope.PolicyDecision, error) {
			return policy.Evaluate(action, policy.DefaultConfig(), at), nil
		}
	}
	if deps.RecordOutcome == nil {
		return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement action outcome recorder required")
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement actor required")
	}
	homePath, err := filepath.Abs(filepath.Clean(req.HomePath))
	if err != nil || strings.TrimSpace(req.HomePath) == "" {
		return WorktreeRetirementResult{}, fmt.Errorf("COMPOUND_HOME required")
	}
	planPath, err := filepath.Abs(filepath.Clean(req.PlanPath))
	if err != nil {
		return WorktreeRetirementResult{}, err
	}
	reportPath, err := filepath.Abs(filepath.Clean(req.ReviewReportPath))
	if err != nil {
		return WorktreeRetirementResult{}, err
	}
	plan, err := LoadWorktreeRetirementPlan(planPath)
	if err != nil {
		return WorktreeRetirementResult{}, err
	}
	if plan.SchemaVersion != WorktreeRetirementPlanSchemaVersion {
		return WorktreeRetirementResult{}, fmt.Errorf("historical retirement plan requires fresh v0.2 planning and review")
	}
	if !plan.Eligible {
		return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement plan is not eligible")
	}
	report, err := ValidateAuthorizedWorktreeRetirement(planPath, reportPath)
	if err != nil {
		return WorktreeRetirementResult{}, err
	}
	if actor != plan.OwnerProof.LeaseHolder || actor != req.OwnerProof.LeaseHolder {
		return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement actor is not owner work lease holder")
	}
	if !reflect.DeepEqual(req.SourceProof, plan.SourceProof) {
		return WorktreeRetirementResult{}, fmt.Errorf("source work proof drift before retirement")
	}
	if err := validateRetirementOwnerProof(req.OwnerProof); err != nil {
		return WorktreeRetirementResult{}, err
	}
	if err := validateRetirementSourceProof(req.SourceProof); err != nil {
		return WorktreeRetirementResult{}, err
	}
	if err := verifyRetirementSourceCurrent(deps, plan); err != nil {
		return WorktreeRetirementResult{}, err
	}
	if err := VerifyWorktreeRetirementRecoveryProofs(ctx, plan.RecoveryProofs, deps.RecoveryResolver); err != nil {
		return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement recovery evidence invalid before apply/resume: %w", err)
	}

	dir, err := worktreeRetirementArtifactDir(homePath, plan.ID)
	if err != nil {
		return WorktreeRetirementResult{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return WorktreeRetirementResult{}, err
	}
	pendingPath := filepath.Join(dir, plan.ID+".retirement.pending.json")
	resultPath := filepath.Join(dir, plan.ID+".retirement.result.json")
	var previousResult *WorktreeRetirementResult
	if existing, loadErr := loadWorktreeRetirementResult(resultPath); loadErr == nil {
		if err := validateWorktreeRetirementResult(existing, plan, report, resultPath); err != nil {
			return WorktreeRetirementResult{}, err
		}
		if existing.Status == "completed" {
			if err := verifyRetirementFinalState(ctx, deps.Git, plan); err != nil {
				return WorktreeRetirementResult{}, fmt.Errorf("completed retirement result no longer matches Git state: %w", err)
			}
			existing.Replayed = true
			return existing, nil
		}
		if existing.Status != "partial" && existing.Status != "failed" {
			return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement result has non-resumable status %q", existing.Status)
		}
		previousResult = &existing
	} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return WorktreeRetirementResult{}, loadErr
	}

	// Settled completed-result replay above reports historical Git truth without
	// admitting another mutation or claiming that recovery bytes were custodied.
	if deps.RecoveryInventoryPreflight == nil {
		return WorktreeRetirementResult{}, fmt.Errorf("retirement recovery-inventory preflight is required")
	}
	if err := deps.RecoveryInventoryPreflight(plan); err != nil {
		return WorktreeRetirementResult{}, fmt.Errorf("retirement recovery-inventory preflight before apply/resume: %w", err)
	}
	pending, pendingExists, err := loadWorktreeRetirementPending(pendingPath)
	if err != nil {
		return WorktreeRetirementResult{}, err
	}
	resumeNeedsPolicyRefreshBeforeRef := false
	if pendingExists {
		if err := validateWorktreeRetirementPending(pending, plan, report, actor); err != nil {
			return WorktreeRetirementResult{}, err
		}
		if err := validateRetirementResumeOwnerProof(req.OwnerProof, plan.OwnerProof, previousResult); err != nil {
			return WorktreeRetirementResult{}, err
		}
		switch pending.Phase {
		case WorktreeRetirementPhasePrepared:
			fresh, err := BuildWorktreeRetirementPlan(ctx, WorktreeRetirementPlanRequest{
				RepoRoot: plan.RepoRoot, WorktreePath: retirementWorktreePath(plan), CandidateRef: plan.CandidateRef,
				AuthorityRef: plan.AuthorityRef, PublicationRef: plan.PublicationRef,
				OwnerProof: req.OwnerProof, SourceProof: req.SourceProof, RecoveryRefs: plan.RecoveryRefs,
			}, WorktreeRetirementDependencies{Git: deps.Git, Liveness: deps.Liveness, RecoveryResolver: deps.RecoveryResolver, RecoveryInventoryPreflight: deps.RecoveryInventoryPreflight}, now)
			if err != nil {
				return WorktreeRetirementResult{}, err
			}
			if err := compareRetirementPlanState(plan, fresh); err != nil {
				return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement state drift before prepared resume: %w", err)
			}
		case WorktreeRetirementPhaseWorktreeRemovalStarted:
			removed, err := reconcileRetirementWorktreeRemovalStarted(ctx, deps, plan, req, now)
			if err != nil {
				return WorktreeRetirementResult{}, err
			}
			if removed {
				pending.Phase = WorktreeRetirementPhaseWorktreeRemoved
				if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
					return WorktreeRetirementResult{}, err
				}
			}
		case WorktreeRetirementPhaseRefDeletionStarted:
			deleted, err := reconcileRetirementRefDeletionStarted(ctx, deps.Git, plan)
			if err != nil {
				return WorktreeRetirementResult{}, err
			}
			if deleted {
				pending.Phase = WorktreeRetirementPhaseRefDeleted
				if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
					return WorktreeRetirementResult{}, err
				}
			}
		default:
			if err := verifyRetirementResumeState(ctx, deps.Git, plan, pending.Phase); err != nil {
				return WorktreeRetirementResult{}, err
			}
		}
		resumeNeedsPolicyRefreshBeforeRef = plan.Worktree != nil && (pending.Phase == WorktreeRetirementPhasePrepared || pending.Phase == WorktreeRetirementPhaseWorktreeRemovalStarted)
		if previousResult != nil {
			pending.Action = newWorktreeRetirementActionForAttempt(plan, report, actor, now, previousResult.ResultDigest)
		}
		// A recorded failed outcome starts a new action even when Git effects
		// already settled. Its completion still needs its own current decision.
		if previousResult != nil || retirementPhaseRequiresGitMutation(plan, pending.Phase) {
			if err := reevaluateWorktreeRetirementResumePolicy(deps, &pending, now); err != nil {
				return WorktreeRetirementResult{}, err
			}
		}
		if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
			return WorktreeRetirementResult{}, err
		}
	} else {
		if !reflect.DeepEqual(req.OwnerProof, plan.OwnerProof) {
			return WorktreeRetirementResult{}, fmt.Errorf("owner work or lease drift before retirement")
		}
		fresh, err := BuildWorktreeRetirementPlan(ctx, WorktreeRetirementPlanRequest{
			RepoRoot: plan.RepoRoot, WorktreePath: retirementWorktreePath(plan), CandidateRef: plan.CandidateRef,
			AuthorityRef: plan.AuthorityRef, PublicationRef: plan.PublicationRef,
			OwnerProof: req.OwnerProof, SourceProof: req.SourceProof, RecoveryRefs: plan.RecoveryRefs,
		}, WorktreeRetirementDependencies{Git: deps.Git, Liveness: deps.Liveness, RecoveryResolver: deps.RecoveryResolver, RecoveryInventoryPreflight: deps.RecoveryInventoryPreflight}, now)
		if err != nil {
			return WorktreeRetirementResult{}, err
		}
		if err := compareRetirementPlanState(plan, fresh); err != nil {
			return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement state drift before mutation: %w", err)
		}
		action := newWorktreeRetirementAction(plan, report, actor, now)
		decision, err := deps.Evaluate(action, now)
		if err != nil {
			return WorktreeRetirementResult{}, err
		}
		if err := action.Validate(); err != nil {
			return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement action invalid: %w", err)
		}
		if err := decision.Validate(); err != nil {
			return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement policy invalid: %w", err)
		}
		if decision.ActionIntentID != action.ID || decision.IDs.WorkID != action.IDs.WorkID || !policy.Allowed(decision) {
			return WorktreeRetirementResult{}, fmt.Errorf("worktree retirement policy does not allow exact action")
		}
		pending = WorktreeRetirementPending{
			SchemaVersion: WorktreeRetirementPendingSchemaVersion,
			PlanID:        plan.ID, PlanDigest: plan.PlanDigest, AuthorizationDigest: report.AuthorizationDigest,
			Actor: actor, Phase: WorktreeRetirementPhasePrepared, Action: action, Decision: decision,
		}
		if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
			return WorktreeRetirementResult{}, err
		}
	}

	baseResult := WorktreeRetirementResult{
		SchemaVersion: WorktreeRetirementResultSchemaVersion,
		ID:            "wtrres_" + digestRetirementHex([]byte(plan.ID + "|" + report.AuthorizationDigest))[:12],
		CreatedAt:     now.UTC().Format(time.RFC3339Nano), Status: "running", Phase: pending.Phase,
		PlanID: plan.ID, PlanDigest: plan.PlanDigest, AuthorizationDigest: report.AuthorizationDigest, Actor: actor,
		AuthorityTip: plan.AuthorityTip, PublicationTip: plan.PublicationTip,
		ActionIntentID: pending.Action.ID, PolicyDecisionID: pending.Decision.ID,
		LeaseWorkSHA256: req.OwnerProof.LeaseWorkSHA256, WorkDigest: req.OwnerProof.WorkDigest,
		PendingPath: pendingPath, ResultPath: resultPath,
	}
	baseResult.WorktreeRemoved = retirementPhaseHasRemovedWorktree(plan, pending.Phase)
	baseResult.RefDeleted = retirementPhaseHasDeletedRef(plan, pending.Phase)
	if pending.Phase == WorktreeRetirementPhasePrepared && deps.AfterPhase != nil {
		if err := deps.AfterPhase(pending.Phase); err != nil {
			return baseResult, err
		}
	}

	if pending.Phase == WorktreeRetirementPhasePrepared && plan.Worktree != nil {
		pending.Phase = WorktreeRetirementPhaseWorktreeRemovalStarted
		if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
			return baseResult, err
		}
		baseResult.Phase = pending.Phase
	}
	if pending.Phase == WorktreeRetirementPhaseWorktreeRemovalStarted {
		if err := verifyRetirementMutationBoundary(ctx, deps, plan, true, now); err != nil {
			return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, false, "retirement authority changed before worktree removal", err, now)
		}
		if _, err := deps.Git.Run(ctx, plan.RepoRoot, "worktree", "remove", plan.Worktree.Path); err != nil {
			return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, false, "worktree removal failed", err, now)
		}
		baseResult.WorktreeRemoved = true
		if deps.AfterGitMutation != nil {
			if err := deps.AfterGitMutation(pending.Phase); err != nil {
				return baseResult, err
			}
		}
		if err := verifyRetirementWorktreeAbsent(ctx, deps.Git, plan); err != nil {
			return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, true, "worktree removal could not be verified", err, now)
		}
		pending.Phase = WorktreeRetirementPhaseWorktreeRemoved
		if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
			return baseResult, err
		}
		baseResult.Phase = pending.Phase
		baseResult.WorktreeRemoved = true
		if deps.AfterPhase != nil {
			if err := deps.AfterPhase(pending.Phase); err != nil {
				return baseResult, err
			}
		}
	}
	if resumeNeedsPolicyRefreshBeforeRef && plan.CandidateRef != "" && pending.Phase == WorktreeRetirementPhaseWorktreeRemoved {
		if err := reevaluateWorktreeRetirementResumePolicy(deps, &pending, now); err != nil {
			return recordWorktreeRetirementFailure(
				deps, plan, report, pending, baseResult, resultPath, true,
				"policy refused remaining candidate ref mutation", err, now,
			)
		}
		if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
			return baseResult, err
		}
	}

	refReady := plan.CandidateRef != "" && (pending.Phase == WorktreeRetirementPhasePrepared || pending.Phase == WorktreeRetirementPhaseWorktreeRemoved)
	if refReady {
		pending.Phase = WorktreeRetirementPhaseRefDeletionStarted
		if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
			return baseResult, err
		}
		baseResult.Phase = pending.Phase
	}
	if pending.Phase == WorktreeRetirementPhaseRefDeletionStarted {
		if err := verifyRetirementMutationBoundary(ctx, deps, plan, false, now); err != nil {
			return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, baseResult.WorktreeRemoved, "retirement authority changed before ref deletion", err, now)
		}
		if _, err := deps.Git.Run(ctx, plan.RepoRoot, "update-ref", "-d", plan.CandidateRef, plan.CandidateTip); err != nil {
			baseResult.WorktreeRemoved = plan.Worktree != nil
			return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, baseResult.WorktreeRemoved, "candidate ref CAS deletion failed", err, now)
		}
		baseResult.RefDeleted = true
		if deps.AfterGitMutation != nil {
			if err := deps.AfterGitMutation(pending.Phase); err != nil {
				return baseResult, err
			}
		}
		if err := verifyRetirementRefAbsent(ctx, deps.Git, plan); err != nil {
			return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, true, "candidate ref deletion could not be verified", err, now)
		}
		pending.Phase = WorktreeRetirementPhaseRefDeleted
		if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
			return baseResult, err
		}
		baseResult.Phase = pending.Phase
		baseResult.RefDeleted = true
		baseResult.WorktreeRemoved = plan.Worktree != nil
		if deps.AfterPhase != nil {
			if err := deps.AfterPhase(pending.Phase); err != nil {
				return baseResult, err
			}
		}
	}

	if err := verifyRetirementFinalState(ctx, deps.Git, plan); err != nil {
		return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, true, "final retirement verification failed", err, now)
	}
	if err := verifyRetirementSourceCurrent(deps, plan); err != nil {
		return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, true, "final source proof verification failed", err, now)
	}
	if err := VerifyWorktreeRetirementRecoveryProofs(ctx, plan.RecoveryProofs, deps.RecoveryResolver); err != nil {
		return recordWorktreeRetirementFailure(deps, plan, report, pending, baseResult, resultPath, true, "final recovery evidence verification failed", err, now)
	}
	pending.Phase = WorktreeRetirementPhaseVerified
	if err := persistWorktreeRetirementPending(pendingPath, &pending, now); err != nil {
		return baseResult, err
	}
	baseResult.Phase = pending.Phase
	baseResult.WorktreeRemoved = plan.Worktree != nil
	baseResult.RefDeleted = plan.CandidateRef != ""
	proof, err := deps.RecordOutcome(WorktreeRetirementActionOutcome{
		Action: pending.Action, Decision: pending.Decision, Outcome: "completed", Changed: true,
		Summary:    "retired redundant Git worktree/ref without changing authority or publication tips",
		ProofRefs:  worktreeRetirementApplyProofRefs(plan, report),
		BeforeRefs: []string{"git-ref:" + plan.CandidateRef + "@" + plan.CandidateTip},
		AfterRefs:  []string{"worktree-retirement-result:" + baseResult.ID},
	}, now)
	if err != nil {
		return baseResult, fmt.Errorf("record worktree retirement outcome: %w", err)
	}
	if err := validateWorktreeRetirementActionProof(proof, pending, plan); err != nil {
		return baseResult, err
	}
	baseResult.Status = "completed"
	baseResult.ActionIntentID = proof.ActionIntentID
	baseResult.PolicyDecisionID = proof.PolicyDecisionID
	baseResult.ReceiptID = proof.ReceiptID
	baseResult.TransitionID = proof.TransitionID
	baseResult.LeaseWorkSHA256 = proof.LeaseWorkSHA256
	baseResult.WorkDigest = proof.WorkDigest
	if err := persistWorktreeRetirementResult(resultPath, &baseResult); err != nil {
		return baseResult, err
	}
	if err := os.Remove(pendingPath); err != nil && !os.IsNotExist(err) {
		return baseResult, err
	}
	return baseResult, nil
}

func newWorktreeRetirementAction(plan WorktreeRetirementPlan, report WorktreeRetirementReviewReport, actor string, now time.Time) envelope.ActionIntent {
	return newWorktreeRetirementActionForAttempt(plan, report, actor, now, "")
}

func newWorktreeRetirementActionForAttempt(plan WorktreeRetirementPlan, report WorktreeRetirementReviewReport, actor string, now time.Time, attemptSalt string) envelope.ActionIntent {
	evidence := worktreeRetirementApplyProofRefs(plan, report)
	if attemptSalt != "" {
		evidence = sortedUniqueNonEmpty(append(evidence, "worktree-retirement-prior-result:"+attemptSalt))
	}
	id := "act_" + digestRetirementHex([]byte(plan.ID + "|" + report.AuthorizationDigest + "|" + actor + "|" + attemptSalt))[:12]
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: id, CreatedAt: now.UTC().Format(time.RFC3339Nano),
			Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: plan.OwnerProof.WorkID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: evidence, Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: "source-control.worktree-retire", Action: "retire_redundant_git_surface",
		Target:        envelope.ActionTarget{Kind: "git-repository", ID: plan.ID, Path: plan.RepoRoot},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "repo",
		Preview:      "remove the exact clean inactive linked worktree and CAS-delete the exact redundant local ref",
		RollbackPlan: "restore the sealed candidate tip from a cited recovery artifact into a new local ref/worktree",
	}
}

func compareRetirementPlanState(sealed, fresh WorktreeRetirementPlan) error {
	if !fresh.Eligible {
		return fmt.Errorf("fresh plan is blocked: %s", strings.Join(fresh.BlockReasons, "; "))
	}
	if sealed.RepoRoot != fresh.RepoRoot || sealed.RepositoryFingerprint != fresh.RepositoryFingerprint ||
		sealed.CandidateRef != fresh.CandidateRef || sealed.CandidateTip != fresh.CandidateTip ||
		sealed.AuthorityRef != fresh.AuthorityRef || sealed.AuthorityTip != fresh.AuthorityTip ||
		sealed.PublicationRef != fresh.PublicationRef || sealed.PublicationTip != fresh.PublicationTip ||
		sealed.CandidateRelation != fresh.CandidateRelation || !reflect.DeepEqual(sealed.OwnerProof, fresh.OwnerProof) ||
		!reflect.DeepEqual(sealed.SourceProof, fresh.SourceProof) || !reflect.DeepEqual(sealed.RecoveryRefs, fresh.RecoveryRefs) {
		return fmt.Errorf("sealed repository/ref/proof state changed")
	}
	if (sealed.Worktree == nil) != (fresh.Worktree == nil) {
		return fmt.Errorf("sealed worktree presence changed")
	}
	if sealed.Worktree != nil {
		a, b := sealed.Worktree, fresh.Worktree
		if a.Path != b.Path || a.Head != b.Head || a.BranchRef != b.BranchRef || a.GitDir != b.GitDir || a.Locked != b.Locked || a.Prunable != b.Prunable || a.Clean != b.Clean || a.Liveness.State != b.Liveness.State {
			return fmt.Errorf("sealed worktree identity/cleanliness/liveness changed")
		}
	}
	return nil
}

func reconcileRetirementWorktreeRemovalStarted(ctx context.Context, deps WorktreeRetirementApplyDependencies, plan WorktreeRetirementPlan, req WorktreeRetirementApplyRequest, now time.Time) (bool, error) {
	if plan.Worktree == nil {
		return false, fmt.Errorf("worktree removal started without a sealed worktree")
	}
	if err := verifyRetirementStableRefs(ctx, deps.Git, plan, "before worktree removal resume"); err != nil {
		return false, err
	}
	out, err := deps.Git.Run(ctx, plan.RepoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	records, err := parseRetirementWorktrees(out)
	if err != nil {
		return false, err
	}
	registered := false
	for _, record := range records {
		if sameRetirementPath(record.Path, plan.Worktree.Path) {
			registered = true
			break
		}
	}
	if !registered {
		if _, statErr := os.Stat(plan.Worktree.Path); os.IsNotExist(statErr) {
			return true, nil
		} else if statErr != nil {
			return false, fmt.Errorf("observe worktree path after started removal: %w", statErr)
		}
		return false, fmt.Errorf("worktree removal started state is ambiguous: path exists but is not registered")
	}
	fresh, err := BuildWorktreeRetirementPlan(ctx, WorktreeRetirementPlanRequest{
		RepoRoot: plan.RepoRoot, WorktreePath: retirementWorktreePath(plan), CandidateRef: plan.CandidateRef,
		AuthorityRef: plan.AuthorityRef, PublicationRef: plan.PublicationRef,
		OwnerProof: plan.OwnerProof, SourceProof: req.SourceProof, RecoveryRefs: plan.RecoveryRefs,
	}, WorktreeRetirementDependencies{Git: deps.Git, Liveness: deps.Liveness, RecoveryResolver: deps.RecoveryResolver, RecoveryInventoryPreflight: deps.RecoveryInventoryPreflight}, now)
	if err != nil {
		return false, err
	}
	if err := compareRetirementPlanState(plan, fresh); err != nil {
		return false, fmt.Errorf("worktree retirement state drift before retrying started removal: %w", err)
	}
	return false, nil
}

func reconcileRetirementRefDeletionStarted(ctx context.Context, git WorktreeRetirementGit, plan WorktreeRetirementPlan) (bool, error) {
	if plan.CandidateRef == "" {
		return false, fmt.Errorf("ref deletion started without a sealed candidate ref")
	}
	if err := verifyRetirementStableRefs(ctx, git, plan, "before candidate ref deletion resume"); err != nil {
		return false, err
	}
	tip, err := retirementResolveCommit(ctx, git, plan.RepoRoot, plan.CandidateRef)
	if err == nil {
		if tip != plan.CandidateTip {
			return false, fmt.Errorf("candidate ref drift before retirement resume")
		}
		return false, nil
	}
	if absentErr := verifyRetirementRefAbsent(ctx, git, plan); absentErr != nil {
		return false, fmt.Errorf("candidate ref state unknown before retirement resume: %w", absentErr)
	}
	return true, nil
}

func verifyRetirementSourceCurrent(deps WorktreeRetirementApplyDependencies, plan WorktreeRetirementPlan) error {
	if plan.SourceProof.Delivery == nil {
		return nil
	}
	if deps.SourceProofResolver == nil {
		return fmt.Errorf("current terminal source proof resolver required")
	}
	current, err := deps.SourceProofResolver(plan.SourceProof.WorkID)
	if err != nil {
		return fmt.Errorf("current terminal source proof: %w", err)
	}
	if !reflect.DeepEqual(current, plan.SourceProof) {
		return fmt.Errorf("terminal source delivery changed; fresh retirement planning required")
	}
	return validateRetirementSourceProof(current)
}

func verifyRetirementMutationBoundary(ctx context.Context, deps WorktreeRetirementApplyDependencies, plan WorktreeRetirementPlan, worktreePresent bool, now time.Time) error {
	if err := verifyRetirementSourceCurrent(deps, plan); err != nil {
		return err
	}
	if err := verifyRetirementStableRefs(ctx, deps.Git, plan, "before retirement effect"); err != nil {
		return err
	}
	if err := VerifyWorktreeRetirementRecoveryProofs(ctx, plan.RecoveryProofs, deps.RecoveryResolver); err != nil {
		return err
	}
	if worktreePresent {
		fresh, err := BuildWorktreeRetirementPlan(ctx, WorktreeRetirementPlanRequest{
			RepoRoot: plan.RepoRoot, WorktreePath: retirementWorktreePath(plan), CandidateRef: plan.CandidateRef,
			AuthorityRef: plan.AuthorityRef, PublicationRef: plan.PublicationRef,
			OwnerProof: plan.OwnerProof, SourceProof: plan.SourceProof, RecoveryRefs: plan.RecoveryRefs,
		}, WorktreeRetirementDependencies{Git: deps.Git, Liveness: deps.Liveness, RecoveryResolver: deps.RecoveryResolver, RecoveryInventoryPreflight: deps.RecoveryInventoryPreflight}, now)
		if err != nil {
			return err
		}
		return compareRetirementPlanState(plan, fresh)
	}
	return nil
}

func verifyRetirementStableRefs(ctx context.Context, git WorktreeRetirementGit, plan WorktreeRetirementPlan, boundary string) error {
	authority, err := retirementResolveCommit(ctx, git, plan.RepoRoot, plan.AuthorityRef)
	if err != nil || authority != plan.AuthorityTip {
		return fmt.Errorf("authority ref drift %s", boundary)
	}
	if !gitrepository.RetirementRequiresPublication(plan.SourceProof) {
		if plan.PublicationRef != "" || plan.PublicationTip != "" {
			return fmt.Errorf("local-only retirement publication authority changed %s", boundary)
		}
		return nil
	}
	publication, err := retirementResolveCommit(ctx, git, plan.RepoRoot, plan.PublicationRef)
	if err != nil || publication != plan.PublicationTip {
		return fmt.Errorf("publication ref drift %s", boundary)
	}
	return nil
}

func retirementPhaseHasRemovedWorktree(plan WorktreeRetirementPlan, phase string) bool {
	if plan.Worktree == nil {
		return false
	}
	switch phase {
	case WorktreeRetirementPhaseWorktreeRemoved, WorktreeRetirementPhaseRefDeletionStarted, WorktreeRetirementPhaseRefDeleted, WorktreeRetirementPhaseVerified:
		return true
	default:
		return false
	}
}

func retirementPhaseHasDeletedRef(plan WorktreeRetirementPlan, phase string) bool {
	if plan.CandidateRef == "" {
		return false
	}
	return phase == WorktreeRetirementPhaseRefDeleted || phase == WorktreeRetirementPhaseVerified
}

func retirementPhaseRequiresGitMutation(plan WorktreeRetirementPlan, phase string) bool {
	if plan.Worktree != nil && !retirementPhaseHasRemovedWorktree(plan, phase) {
		return true
	}
	return plan.CandidateRef != "" && !retirementPhaseHasDeletedRef(plan, phase)
}

func verifyRetirementResumeState(ctx context.Context, git WorktreeRetirementGit, plan WorktreeRetirementPlan, phase string) error {
	if err := verifyRetirementStableRefs(ctx, git, plan, "before retirement resume"); err != nil {
		return err
	}
	switch phase {
	case WorktreeRetirementPhaseWorktreeRemoved:
		if err := verifyRetirementWorktreeAbsent(ctx, git, plan); err != nil {
			return err
		}
		if plan.CandidateRef != "" {
			tip, err := retirementResolveCommit(ctx, git, plan.RepoRoot, plan.CandidateRef)
			if err != nil || tip != plan.CandidateTip {
				return fmt.Errorf("candidate ref drift before retirement resume")
			}
		}
	case WorktreeRetirementPhaseRefDeleted, WorktreeRetirementPhaseVerified:
		return verifyRetirementFinalState(ctx, git, plan)
	default:
		return fmt.Errorf("unknown retirement pending phase %q", phase)
	}
	return nil
}

func verifyRetirementFinalState(ctx context.Context, git WorktreeRetirementGit, plan WorktreeRetirementPlan) error {
	if plan.Worktree != nil {
		if err := verifyRetirementWorktreeAbsent(ctx, git, plan); err != nil {
			return err
		}
	}
	if plan.CandidateRef != "" {
		if err := verifyRetirementRefAbsent(ctx, git, plan); err != nil {
			return err
		}
	}
	if err := verifyRetirementStableRefs(ctx, git, plan, "during retirement"); err != nil {
		return err
	}
	if len(plan.RecoveryRefs) == 0 {
		return fmt.Errorf("recovery evidence disappeared")
	}
	return nil
}

func verifyRetirementWorktreeAbsent(ctx context.Context, git WorktreeRetirementGit, plan WorktreeRetirementPlan) error {
	if plan.Worktree == nil {
		return nil
	}
	out, err := git.Run(ctx, plan.RepoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	records, err := parseRetirementWorktrees(out)
	if err != nil {
		return err
	}
	for _, record := range records {
		if sameRetirementPath(record.Path, plan.Worktree.Path) {
			return fmt.Errorf("retired worktree remains registered")
		}
	}
	if _, err := os.Stat(plan.Worktree.Path); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("retired worktree path still exists")
	}
	return nil
}

func verifyRetirementRefAbsent(ctx context.Context, git WorktreeRetirementGit, plan WorktreeRetirementPlan) error {
	if plan.CandidateRef == "" {
		return nil
	}
	_, err := git.Run(ctx, plan.RepoRoot, "rev-parse", "--verify", plan.CandidateRef)
	if err == nil {
		return fmt.Errorf("retired candidate ref still exists")
	}
	var commandErr *WorktreeRetirementCommandError
	if errors.As(err, &commandErr) && (commandErr.ExitCode == 1 || commandErr.ExitCode == 128) {
		return nil
	}
	return err
}

func recordWorktreeRetirementFailure(deps WorktreeRetirementApplyDependencies, plan WorktreeRetirementPlan, report WorktreeRetirementReviewReport, pending WorktreeRetirementPending, result WorktreeRetirementResult, resultPath string, changed bool, summary string, cause error, now time.Time) (WorktreeRetirementResult, error) {
	status := "failed"
	if changed {
		status = "partial"
	}
	result.Status = status
	result.Phase = pending.Phase
	result.Error = cause.Error()
	result.WorktreeRemoved = retirementPhaseHasRemovedWorktree(plan, pending.Phase)
	result.RefDeleted = retirementPhaseHasDeletedRef(plan, pending.Phase)
	proof, recordErr := deps.RecordOutcome(WorktreeRetirementActionOutcome{
		Action: pending.Action, Decision: pending.Decision, Outcome: "failed", Changed: changed,
		Summary: summary + ": " + cause.Error(), ProofRefs: worktreeRetirementApplyProofRefs(plan, report),
		FailureCause: &envelope.FailureCause{SurfaceOutcome: status, Mechanism: summary},
	}, now)
	if recordErr != nil {
		return result, fmt.Errorf("%s; record failure outcome: %v", cause, recordErr)
	}
	if err := validateWorktreeRetirementActionProof(proof, pending, plan); err != nil {
		return result, fmt.Errorf("%s; record failure outcome: %w", cause, err)
	}
	result.ActionIntentID = proof.ActionIntentID
	result.PolicyDecisionID = proof.PolicyDecisionID
	result.ReceiptID = proof.ReceiptID
	result.TransitionID = proof.TransitionID
	result.LeaseWorkSHA256 = proof.LeaseWorkSHA256
	result.WorkDigest = proof.WorkDigest
	if persistErr := persistWorktreeRetirementResult(resultPath, &result); persistErr != nil {
		return result, persistErr
	}
	return result, cause
}

func persistWorktreeRetirementPending(path string, pending *WorktreeRetirementPending, now time.Time) error {
	pending.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	pending.PendingDigest = worktreeRetirementPendingDigest(*pending)
	return writeRetirementDurableJSON(path, pending)
}

func worktreeRetirementPendingDigest(pending WorktreeRetirementPending) string {
	pending.PendingDigest = ""
	data, _ := json.Marshal(pending)
	return "sha256:" + digestRetirementHex(data)
}

func loadWorktreeRetirementPending(path string) (WorktreeRetirementPending, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return WorktreeRetirementPending{}, false, nil
	}
	if err != nil {
		return WorktreeRetirementPending{}, false, err
	}
	var pending WorktreeRetirementPending
	if err := json.Unmarshal(data, &pending); err != nil {
		return WorktreeRetirementPending{}, false, err
	}
	if !validRetirementDigest(pending.PendingDigest) || pending.PendingDigest != worktreeRetirementPendingDigest(pending) {
		return WorktreeRetirementPending{}, false, fmt.Errorf("worktree retirement pending digest mismatch")
	}
	return pending, true, nil
}

func validateWorktreeRetirementPending(pending WorktreeRetirementPending, plan WorktreeRetirementPlan, report WorktreeRetirementReviewReport, actor string) error {
	if pending.SchemaVersion != WorktreeRetirementPendingSchemaVersion || pending.PlanID != plan.ID || pending.PlanDigest != plan.PlanDigest || pending.AuthorizationDigest != report.AuthorizationDigest || pending.Actor != actor {
		return fmt.Errorf("worktree retirement pending identity mismatch")
	}
	if err := pending.Action.Validate(); err != nil {
		return err
	}
	if err := pending.Decision.Validate(); err != nil || pending.Decision.ActionIntentID != pending.Action.ID || !policy.Allowed(pending.Decision) {
		return fmt.Errorf("worktree retirement pending policy invalid")
	}
	switch pending.Phase {
	case WorktreeRetirementPhasePrepared,
		WorktreeRetirementPhaseWorktreeRemovalStarted,
		WorktreeRetirementPhaseWorktreeRemoved,
		WorktreeRetirementPhaseRefDeletionStarted,
		WorktreeRetirementPhaseRefDeleted,
		WorktreeRetirementPhaseVerified:
	default:
		return fmt.Errorf("worktree retirement pending phase invalid: %q", pending.Phase)
	}
	return nil
}

func persistWorktreeRetirementResult(path string, result *WorktreeRetirementResult) error {
	result.ResultDigest = worktreeRetirementResultDigest(*result)
	return writeRetirementDurableJSON(path, result)
}

func worktreeRetirementResultDigest(result WorktreeRetirementResult) string {
	return gitrepository.WorktreeRetirementResultDigest(result)
}

func loadWorktreeRetirementResult(path string) (WorktreeRetirementResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return WorktreeRetirementResult{}, err
	}
	var result WorktreeRetirementResult
	if err := json.Unmarshal(data, &result); err != nil {
		return WorktreeRetirementResult{}, err
	}
	return result, nil
}

func validateWorktreeRetirementResult(result WorktreeRetirementResult, plan WorktreeRetirementPlan, report WorktreeRetirementReviewReport, path string) error {
	return gitrepository.ValidateWorktreeRetirementResult(result, plan, report, path)
}

func writeRetirementDurableJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".worktree-retirement-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func worktreeRetirementApplyProofRefs(plan WorktreeRetirementPlan, report WorktreeRetirementReviewReport) []string {
	return gitrepository.WorktreeRetirementApplyProofRefs(plan, report)
}

func retirementWorktreePath(plan WorktreeRetirementPlan) string {
	if plan.Worktree == nil {
		return ""
	}
	return plan.Worktree.Path
}

func validateRetirementResumeOwnerProof(live, sealed WorktreeRetirementOwnerProof, previous *WorktreeRetirementResult) error {
	if live.WorkID != sealed.WorkID || live.State != "running" || sealed.State != "running" || live.LeaseHolder != sealed.LeaseHolder || live.LeaseClaimedAt != sealed.LeaseClaimedAt || live.LeaseWorkSHA256 != sealed.LeaseWorkSHA256 {
		return fmt.Errorf("owner work lease drift before retirement resume")
	}
	expectedWorkDigest := sealed.WorkDigest
	if previous != nil {
		expectedWorkDigest = previous.WorkDigest
	}
	if live.WorkDigest != expectedWorkDigest {
		return fmt.Errorf("owner work digest drift before retirement resume: current digest is not the exact self-caused prior outcome digest")
	}
	return nil
}

func reevaluateWorktreeRetirementResumePolicy(deps WorktreeRetirementApplyDependencies, pending *WorktreeRetirementPending, now time.Time) error {
	decision, err := deps.Evaluate(pending.Action, now)
	if err != nil {
		return err
	}
	if err := pending.Action.Validate(); err != nil {
		return fmt.Errorf("worktree retirement retry action invalid: %w", err)
	}
	if err := decision.Validate(); err != nil || decision.ActionIntentID != pending.Action.ID || decision.IDs.WorkID != pending.Action.IDs.WorkID || !policy.Allowed(decision) {
		return fmt.Errorf("worktree retirement current policy does not allow exact resumed action")
	}
	pending.Decision = decision
	return nil
}

func validateWorktreeRetirementActionProof(proof WorktreeRetirementActionProof, pending WorktreeRetirementPending, plan WorktreeRetirementPlan) error {
	if proof.ActionIntentID != pending.Action.ID || proof.PolicyDecisionID != pending.Decision.ID {
		return fmt.Errorf("worktree retirement outcome proof chain incomplete: action or policy mismatch")
	}
	if strings.TrimSpace(proof.ReceiptID) == "" {
		return fmt.Errorf("worktree retirement outcome proof chain incomplete: receipt required")
	}
	if strings.TrimSpace(proof.TransitionID) == "" {
		return fmt.Errorf("worktree retirement outcome proof chain incomplete: transition required")
	}
	if proof.LeaseWorkSHA256 != plan.OwnerProof.LeaseWorkSHA256 || !validRetirementDigest(proof.WorkDigest) {
		return fmt.Errorf("worktree retirement outcome proof chain incomplete: owner work proof invalid")
	}
	return nil
}
