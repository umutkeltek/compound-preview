package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
)

const (
	LegacyLandCheckSchemaVersion   = "compound.work_land_check.v0.3"
	PreviousLandCheckSchemaVersion = "compound.work_land_check.v0.4"
	LandCheckSchemaVersion         = "compound.work_land_check.v0.5"
	landCheckEvidencePrefix        = "land-check:"
	landCheckIDPrefix              = "landcheck_"
)

// LandCheckOptions names every source-control input explicitly. In particular,
// RepoPath is never inferred from the process working directory.
type LandCheckOptions struct {
	// RepoPath and TargetRef are optional caller assertions. They are never
	// authority: when present they must equal the WorkEnvelope-bound contract.
	RepoPath        string
	CandidateTipSHA string
	TargetRef       string
	// ExpectedMainSHA is retained as a source-compatible tombstone. A caller
	// snapshot cannot replace the WorkEnvelope contract and is rejected.
	ExpectedMainSHA string
	Actor           string
	Record          bool
	// Steal and StealReason are forwarded only to the same-state Advance that
	// attaches a successful recorded proof. They do not transfer the lease or
	// affect evaluation, non-landed recording, or dry runs.
	Steal       bool
	StealReason string
}

// LandCheckReport is durable evidence about one candidate tip and one target
// branch for one WorkEnvelope. Non-landed results are recorded too, so a stale
// executor leaves an actionable state rather than disappearing from the queue.
type LandCheckReport struct {
	SchemaVersion                   string `json:"schema_version"`
	ID                              string `json:"id"`
	GeneratedAt                     string `json:"generated_at"`
	WorkID                          string `json:"work_id"`
	Status                          string `json:"status"`
	RepoPath                        string `json:"repo_path"`
	CandidateTipSHA                 string `json:"candidate_tip_sha"`
	CandidateArtifactRef            string `json:"candidate_artifact_ref,omitempty"`
	AttachedCandidateTipSHA         string `json:"attached_candidate_tip_sha,omitempty"`
	ExpectedBaseSHA                 string `json:"expected_base_sha,omitempty"`
	RepositoryFingerprint           string `json:"repository_fingerprint"`
	ProofVerificationRef            string `json:"proof_verification_ref,omitempty"`
	ProofHeadSHA                    string `json:"proof_head_sha,omitempty"`
	TargetRef                       string `json:"target_ref"`
	TargetBranchRef                 string `json:"target_branch_ref"`
	ExpectedMainSHA                 string `json:"expected_main_sha,omitempty"`
	TargetSHA                       string `json:"target_sha"`
	PublicationMode                 string `json:"publication_mode"`
	PublicationRef                  string `json:"publication_ref,omitempty"`
	PublicationSHA                  string `json:"publication_sha,omitempty"`
	CandidateExists                 bool   `json:"candidate_exists"`
	ExpectedBaseExists              bool   `json:"expected_base_exists"`
	CleanTarget                     bool   `json:"clean_target"`
	TargetBranchWorktree            bool   `json:"target_branch_worktree"`
	ExpectedBaseAncestorOfCandidate bool   `json:"expected_base_ancestor_of_candidate"`
	TargetAncestorOfCandidate       bool   `json:"target_ancestor_of_candidate"`
	CandidateContainedInTarget      bool   `json:"candidate_contained_in_target"`
	CandidateContainedInPublication bool   `json:"candidate_contained_in_publication"`
	NextAction                      string `json:"next_action,omitempty"`
	ContentSHA256                   string `json:"content_sha256"`
	Recorded                        bool   `json:"recorded"`
	EvidenceRef                     string `json:"evidence_ref,omitempty"`
	ProofPath                       string `json:"proof_path,omitempty"`
}

// LandCheck evaluates and optionally records the landing invariant for workID.
// A classification is not an error: dirty, stale, missing, and divergent states
// are useful evidence. Invalid caller inputs and unreadable repositories are errors.
func (s *Store) LandCheck(workID string, opts LandCheckOptions, now time.Time) (LandCheckReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	stealReason := strings.TrimSpace(opts.StealReason)
	switch {
	case opts.Steal && stealReason == "":
		return LandCheckReport{}, fmt.Errorf("--steal requires --reason <non-empty>")
	case !opts.Steal && opts.StealReason != "":
		return LandCheckReport{}, fmt.Errorf("--reason requires --steal")
	}
	opts.StealReason = stealReason
	w, err := s.Get(strings.TrimSpace(workID))
	if err != nil {
		return LandCheckReport{}, err
	}
	report, err := s.evaluateLandCheck(w, opts, now)
	if err != nil {
		return report, err
	}
	if err := sealLandCheckReport(&report); err != nil {
		return report, err
	}
	if !opts.Record {
		return report, nil
	}
	if err := s.recordLandCheck(&report); err != nil {
		return report, err
	}
	if report.Status == "landed" {
		current, err := s.Get(w.ID)
		if err != nil {
			return report, err
		}
		if !stringIn(current.EvidenceRefs, report.EvidenceRef) {
			actor := firstNonEmpty(strings.TrimSpace(opts.Actor), strings.TrimSpace(current.Authority.Owner), strings.TrimSpace(current.Owner), "compound")
			if _, err := s.Advance(current.ID, AdvanceInput{
				To:           current.State,
				Actor:        actor,
				EvidenceRefs: []string{report.EvidenceRef},
				Note:         "attached current Git delivery proof",
				Steal:        opts.Steal,
				StealReason:  opts.StealReason,
			}, now); err != nil {
				return report, fmt.Errorf("attach land-check proof to work %s: %w", current.ID, err)
			}
		}
	}
	return report, nil
}

// validateBoundGitDeliveryForLandCheck keeps the full envelope and repository
// authority checks, but defers required-publication existence to evaluation.
// A valid remote-tracking ref can legitimately be absent before first publish;
// malformed, non-canonical, or wrong-branch refs still fail WorkEnvelope
// validation instead of being mislabeled as awaiting publication.
func (s *Store) validateBoundGitDeliveryForLandCheck(w envelope.WorkEnvelope) error {
	return s.validateBoundGitDeliveryForLandCheckWithFacts(w, nil)
}

// validateBoundGitDeliveryForLandCheckWithFacts is the session-aware form: a
// non-nil facts session reuses the repository reads this validation shares
// with the rest of one projection request, while every contract check stays
// per-Work.
func (s *Store) validateBoundGitDeliveryForLandCheckWithFacts(w envelope.WorkEnvelope, facts *landingGitFacts) error {
	if w.GitDelivery == nil {
		return s.validateBoundGitDeliveryWithFacts(w, facts)
	}
	if err := w.Validate(); err != nil {
		return err
	}
	contract := *w.GitDelivery
	if contract.PublicationMode != strings.TrimSpace(contract.PublicationMode) {
		return fmt.Errorf("git_delivery.publication_mode %q is not canonical", contract.PublicationMode)
	}
	if contract.PublicationRef != strings.TrimSpace(contract.PublicationRef) {
		return fmt.Errorf("git_delivery.publication_ref %q is not canonical", contract.PublicationRef)
	}
	if contract.PublicationMode != "required" {
		return s.validateBoundGitDeliveryWithFacts(w, facts)
	}

	withoutPublication := w
	contract.PublicationMode = "local_only"
	contract.PublicationRef = ""
	withoutPublication.GitDelivery = &contract
	return s.validateBoundGitDeliveryWithFacts(withoutPublication, facts)
}

// evaluateLandCheck performs one Work's full landing evaluation with fresh Git
// reads. The governed mutation paths (land-check, close revalidation) always
// take this form.
func (s *Store) evaluateLandCheck(w envelope.WorkEnvelope, opts LandCheckOptions, now time.Time) (LandCheckReport, error) {
	return s.evaluateLandCheckWithFacts(w, opts, now, nil)
}

// evaluateLandCheckWithFacts evaluates ONE Work's delivery invariant. Every
// Work-specific input — envelope validation, the bound contract, the attached
// candidate artifact, proof resolution, publication mode, and the expected
// base — is evaluated here, per Work. The optional facts session only reuses
// repository-scoped READ FACTS (root, refs, status, worktrees, object
// existence, ancestry, identity, owned-path dirt) across the Works of one
// request; a nil session reads everything fresh.
func (s *Store) evaluateLandCheckWithFacts(w envelope.WorkEnvelope, opts LandCheckOptions, now time.Time, facts *landingGitFacts) (LandCheckReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	candidate := strings.TrimSpace(opts.CandidateTipSHA)
	if !validLandingObjectID(candidate) {
		return LandCheckReport{}, fmt.Errorf("candidate tip SHA must be a lowercase full Git object id (40 or 64 hex characters)")
	}
	if strings.TrimSpace(opts.ExpectedMainSHA) != "" {
		return LandCheckReport{}, fmt.Errorf("expected main SHA caller argument is not delivery authority; use the WorkEnvelope git_delivery contract")
	}
	if w.GitDelivery == nil {
		report := LandCheckReport{
			SchemaVersion:   LandCheckSchemaVersion,
			GeneratedAt:     now.UTC().Format(time.RFC3339Nano),
			WorkID:          w.ID,
			CandidateTipSHA: candidate,
			Status:          "missing_git_delivery_contract",
			NextAction:      "replace the incomplete source-controlled work with a WorkEnvelope carrying a complete git_delivery contract",
		}
		return report, nil
	}
	if err := s.validateBoundGitDeliveryForLandCheckWithFacts(w, facts); err != nil {
		return LandCheckReport{}, err
	}
	contract := *w.GitDelivery
	repoPath := contract.TargetWorktreeRoot
	if assertedRepo := strings.TrimSpace(opts.RepoPath); assertedRepo != "" {
		canonicalAssertion, err := facts.canonicalRepo(assertedRepo)
		if err != nil {
			return LandCheckReport{}, err
		}
		if canonicalAssertion != repoPath {
			return LandCheckReport{}, fmt.Errorf("caller repository %s does not match WorkEnvelope git_delivery.target_worktree_root %s", canonicalAssertion, repoPath)
		}
	}
	targetRef, targetBranchRef, err := facts.normalizeTargetRef(repoPath, contract.TargetLocalRef)
	if err != nil {
		return LandCheckReport{}, err
	}
	if assertedRef := strings.TrimSpace(opts.TargetRef); assertedRef != "" {
		_, normalizedAssertion, err := facts.normalizeTargetRef(repoPath, assertedRef)
		if err != nil {
			return LandCheckReport{}, err
		}
		if normalizedAssertion != targetBranchRef {
			return LandCheckReport{}, fmt.Errorf("caller target ref %s does not match WorkEnvelope git_delivery.target_local_ref %s", normalizedAssertion, targetBranchRef)
		}
	}
	targetSHA, err := facts.refCommit(repoPath, targetBranchRef)
	if err != nil {
		return LandCheckReport{}, fmt.Errorf("resolve target branch %s: %w", targetRef, err)
	}
	if !validLandingObjectID(targetSHA) {
		return LandCheckReport{}, fmt.Errorf("target branch %s did not resolve to a full Git object id", targetRef)
	}
	currentBranchRef, err := facts.currentBranchRef(repoPath)
	if err != nil {
		return LandCheckReport{}, err
	}
	statusOutput, err := facts.statusPorcelain(repoPath)
	if err != nil {
		return LandCheckReport{}, fmt.Errorf("inspect target worktree status: %w", err)
	}

	expectedBase := strings.TrimSpace(contract.ExpectedBaseSHA)
	report := LandCheckReport{
		SchemaVersion:         LandCheckSchemaVersion,
		GeneratedAt:           now.UTC().Format(time.RFC3339Nano),
		WorkID:                w.ID,
		RepoPath:              repoPath,
		CandidateTipSHA:       candidate,
		ExpectedBaseSHA:       expectedBase,
		RepositoryFingerprint: contract.RepositoryFingerprint,
		TargetRef:             targetRef,
		TargetBranchRef:       targetBranchRef,
		TargetSHA:             targetSHA,
		PublicationMode:       contract.PublicationMode,
		PublicationRef:        contract.PublicationRef,
		CleanTarget:           statusOutput == "",
		TargetBranchWorktree:  currentBranchRef == targetBranchRef,
	}

	artifactRef, attachedTip, artifactFound, artifactValid := latestCommitArtifact(w.ArtifactIDs)
	report.CandidateArtifactRef = artifactRef
	report.AttachedCandidateTipSHA = attachedTip
	if !artifactFound {
		report.Status = "missing_candidate_artifact"
		report.NextAction = fmt.Sprintf("attach the verified final tip as commit:%s to work %s, then rerun land-check", candidate, w.ID)
		return report, nil
	}
	if !artifactValid {
		report.Status = "invalid_candidate_artifact"
		report.NextAction = fmt.Sprintf("replace malformed latest candidate artifact %q by attaching commit:<full-sha>, then rerun land-check", artifactRef)
		return report, nil
	}
	if candidate != attachedTip {
		report.Status = "candidate_tip_mismatch"
		report.NextAction = fmt.Sprintf("candidate %s does not match latest attached artifact commit:%s; use that verified tip, or attach commit:%s if the provided tip is the newer final work output", candidate, attachedTip, candidate)
		return report, nil
	}
	acceptanceRef, proofResult, proofFound, err := s.latestLandingProofResolution(w)
	if err != nil {
		return report, err
	}
	if proofFound {
		report.ProofVerificationRef = acceptanceRef
		report.ProofHeadSHA = proofResult.SourceSnapshot.HeadSHA
		if proofResult.SourceSnapshot.RepositoryFingerprint != contract.RepositoryFingerprint {
			report.Status = "proof_repository_mismatch"
			report.NextAction = "run a new certifying proof battery in the WorkEnvelope repository"
			return report, nil
		}
		if report.ProofHeadSHA != candidate {
			report.Status = "proof_candidate_tip_mismatch"
			report.NextAction = fmt.Sprintf("candidate %s differs from sealed proof tip %s; run a new certifying battery after replay/rebase", candidate, report.ProofHeadSHA)
			return report, nil
		}
	}
	report.CandidateExists, err = facts.commitExists(repoPath, candidate)
	if err != nil {
		return report, err
	}
	if !report.CandidateExists {
		report.Status = "invalid_lineage"
		report.NextAction = fmt.Sprintf("recover candidate tip %s into this repository, then rerun land-check", candidate)
		return report, nil
	}
	report.ExpectedBaseExists, err = facts.commitExists(repoPath, expectedBase)
	if err != nil {
		return report, err
	}
	if !report.ExpectedBaseExists {
		report.Status = "invalid_lineage"
		report.NextAction = fmt.Sprintf("recover expected base %s into this repository or replace the malformed envelope before landing", expectedBase)
		return report, nil
	}
	report.ExpectedBaseAncestorOfCandidate, err = facts.isAncestor(repoPath, expectedBase, candidate)
	if err != nil {
		return report, err
	}
	if !report.ExpectedBaseAncestorOfCandidate {
		report.Status = "invalid_lineage"
		report.NextAction = fmt.Sprintf("rebuild candidate %s from expected base %s; this tip cannot satisfy the envelope", candidate, expectedBase)
		return report, nil
	}
	// Containment ancestry is computed from real Git history BEFORE any
	// worktree-cleanliness gate. A dirty target may still block a landed status,
	// but the containment fields must never lie about whether the candidate is
	// provably reachable from the target (and publication) refs. The historical
	// short-circuit that returned dirty_target here left CandidateContainedInTarget
	// at its false zero value even for a commit demonstrably on the target branch.
	report.CandidateContainedInTarget, err = facts.isAncestor(repoPath, candidate, targetSHA)
	if err != nil {
		return report, err
	}
	report.TargetAncestorOfCandidate, err = facts.isAncestor(repoPath, targetSHA, candidate)
	if err != nil {
		return report, err
	}
	if !report.TargetBranchWorktree {
		targetCheckedOut, err := facts.refCheckedOut(repoPath, targetBranchRef)
		if err != nil {
			return report, err
		}
		// The landing ceremony permits an atomic update-ref CAS when no
		// worktree has the target branch checked out. Once that ref contains
		// the certified candidate, the foreign feature checkout is irrelevant
		// to delivery and must not block land-check or leak its dirt into the
		// target cleanliness result. A target checked out anywhere, or a target
		// that still needs to move, retains the canonical-worktree refusal.
		if targetCheckedOut || !report.CandidateContainedInTarget {
			report.Status = "wrong_target_worktree"
			report.NextAction = fmt.Sprintf("check out %s in the explicit target worktree, then rerun land-check", targetRef)
			return report, nil
		}
		report.CleanTarget = true
		statusOutput = ""
	}

	publicationExists := false
	if report.CandidateContainedInTarget && contract.PublicationMode == "required" {
		publicationExists, err = facts.refExists(repoPath, contract.PublicationRef)
		if err != nil {
			return report, err
		}
		if publicationExists {
			report.PublicationSHA, err = facts.refCommit(repoPath, contract.PublicationRef)
			if err != nil {
				return report, fmt.Errorf("resolve publication ref %s: %w", contract.PublicationRef, err)
			}
			report.CandidateContainedInPublication, err = facts.isAncestor(repoPath, candidate, report.PublicationSHA)
			if err != nil {
				return report, err
			}
		}
	}

	if !report.CandidateContainedInTarget && !report.TargetAncestorOfCandidate {
		report.Status = "rebase_required"
		report.NextAction = fmt.Sprintf("rebase candidate %s onto current %s tip %s, attach rebased final tip commit:<full-sha>, rerun land-check on that exact tip", candidate, targetRef, targetSHA)
		return report, nil
	}

	if !proofFound {
		proofAction := fmt.Sprintf("run fresh canonical certifying proof for attached candidate commit:%s, then rerun land-check", candidate)
		if !report.CleanTarget {
			proofAction += fmt.Sprintf(
				"; preserve foreign or later uncommitted changes in %s worktree (%s)",
				targetRef,
				strings.Join(dirtyStatusPaths(statusOutput), ", "),
			)
		}
		report.Status = "missing_source_bound_proof"
		report.NextAction = proofAction
		return report, nil
	}

	if report.CandidateContainedInTarget {
		if contract.PublicationMode == "required" {
			if !publicationExists {
				report.Status = "awaiting_publication"
				report.NextAction = fmt.Sprintf("publish %s containing candidate %s to %s, then record a fresh land-check", targetBranchRef, candidate, contract.PublicationRef)
				return report, nil
			}
			if !report.CandidateContainedInPublication {
				report.Status = "awaiting_publication"
				report.NextAction = fmt.Sprintf("publish %s containing candidate %s to %s, then record a fresh land-check", targetBranchRef, candidate, contract.PublicationRef)
				return report, nil
			}
		} else {
			report.CandidateContainedInPublication = true
		}
		// Immutable target/publication containment settles delivery, but does not
		// make later worktree state disappear. Surface every dirty path as a
		// preservation warning so default text clients do not hide foreign WIP.
		if !report.CleanTarget {
			report.NextAction = landingContainedDirtyDiagnostic(targetRef, dirtyStatusPaths(statusOutput), contract.PublicationMode)
		}
		report.Status = "landed"
		return report, nil
	}

	// While landing is still necessary, scope the blocking cleanliness gate to
	// paths this envelope owns (its base..candidate diff). Unrelated dirt remains
	// diagnostic state through CleanTarget, but cannot hold the landing hostage.
	ownedDirty, ownedDirtyPaths, err := facts.ownedPathsDirty(repoPath, expectedBase, candidate)
	if err != nil {
		return report, err
	}
	if ownedDirty {
		report.Status = "dirty_target"
		report.NextAction = landingOwnedDirtyNextAction(targetRef, ownedDirtyPaths)
		return report, nil
	}
	if report.TargetAncestorOfCandidate {
		report.Status = "ready_to_land"
		report.NextAction = fmt.Sprintf("fast-forward %s to candidate %s, then run compound work land-check %s --tip %s", targetRef, candidate, w.ID, candidate)
		return report, nil
	}
	report.Status = "rebase_required"
	report.NextAction = fmt.Sprintf("rebase candidate %s onto current %s tip %s, attach the rebased final tip as commit:<full-sha>, and rerun land-check with that exact tip", candidate, targetRef, targetSHA)
	return report, nil
}

// landingOwnedPathsDirty reports whether the target worktree carries any
// uncommitted (staged, unstaged, or untracked) change to a file this envelope
// owns -- the set of paths in the candidate's base..candidate diff. Unrelated
// dirt elsewhere in an always-dirty or shared checkout is deliberately ignored,
// so a truthful containment result is never held hostage to a neighbor's WIP.
func landingOwnedPathsDirty(repoPath, expectedBase, candidate string) (bool, []string, error) {
	ownedPaths, err := landingOwnedPaths(repoPath, expectedBase, candidate)
	if err != nil {
		return false, nil, err
	}
	if len(ownedPaths) == 0 {
		return false, nil, nil
	}
	// --literal-pathspecs so an owned path that happens to contain glob magic
	// (*, [, ?) is matched exactly rather than reinterpreted as a pattern.
	args := append([]string{"--literal-pathspecs", "status", "--porcelain=v1", "--untracked-files=all", "--"}, ownedPaths...)
	scoped, err := runLandingGitOutput(repoPath, args...)
	if err != nil {
		return false, nil, fmt.Errorf("inspect owned-path worktree status: %w", err)
	}
	if scoped == "" {
		return false, nil, nil
	}
	return true, dirtyStatusPaths(scoped), nil
}

// landingOwnedPaths returns the repo-relative files this envelope's delivery
// changes (the net base..candidate diff). NUL-delimited output keeps paths with
// spaces or non-ASCII bytes intact instead of Git's default quoting.
func landingOwnedPaths(repoPath, expectedBase, candidate string) ([]string, error) {
	out, err := runLandingGitOutput(repoPath, "diff", "--name-only", "-z", expectedBase, candidate)
	if err != nil {
		return nil, fmt.Errorf("compute owned paths %s..%s: %w", expectedBase, candidate, err)
	}
	paths := make([]string, 0)
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			paths = append(paths, f)
		}
	}
	return paths, nil
}

// dirtyStatusPaths extracts the affected path from each porcelain-v1 line for a
// human-readable next action. Rename/copy lines ("XY orig -> dest") report the
// destination.
func dirtyStatusPaths(porcelain string) []string {
	lines := strings.Split(porcelain, "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		entry := strings.TrimSpace(line[3:])
		if idx := strings.Index(entry, " -> "); idx >= 0 {
			entry = entry[idx+len(" -> "):]
		}
		entry = strings.Trim(entry, "\"")
		if entry != "" {
			paths = append(paths, entry)
		}
	}
	return paths
}

func landingOwnedDirtyNextAction(targetRef string, ownedDirtyPaths []string) string {
	preview := ownedDirtyPaths
	if len(preview) > 5 {
		preview = preview[:5]
	}
	return fmt.Sprintf("the %s target worktree has uncommitted changes to files this work owns (%s); commit them, or set them aside without discarding unrelated work (compound work set-aside begin <work_id> --candidate <tip> --file <path>), then rerun land-check", targetRef, strings.Join(preview, ", "))
}

func landingContainedDirtyDiagnostic(targetRef string, dirtyPaths []string, publicationMode string) string {
	authority := "authoritative target ref"
	if publicationMode == "required" {
		authority = "authoritative target and publication refs"
	}
	return fmt.Sprintf("candidate is already contained in the %s; preserve later uncommitted changes in the %s worktree (%s)", authority, targetRef, strings.Join(dirtyPaths, ", "))
}

func (s *Store) latestSourceBoundProofResult(w envelope.WorkEnvelope) (ProofResourceResult, bool, error) {
	for i := len(w.VerificationIDs) - 1; i >= 0; i-- {
		ref := strings.TrimSpace(w.VerificationIDs[i])
		if strings.HasPrefix(ref, DifferentialProofVerificationPrefix) {
			result, err := s.validateDifferentialProofRef(w, ref)
			if err != nil {
				return ProofResourceResult{}, true, err
			}
			return result, true, nil
		}
		if !strings.HasPrefix(ref, "proof-check:") {
			continue
		}
		id := strings.TrimPrefix(ref, "proof-check:")
		if !strings.HasPrefix(id, "pfrs_") {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %q has invalid result identity", ref)
		}
		var result ProofResourceResult
		if err := s.readEventJSON(id, &result); err != nil {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s result: %w", ref, err)
		}
		if result.SchemaVersion != ProofResourceResultSchemaVersion ||
			result.ID != id ||
			result.WorkID != w.ID ||
			result.Status != ProofResourceVerified ||
			result.VerificationRef != ref ||
			!result.OwnedReport {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s is not an owned verified result for work %s", ref, w.ID)
		}
		if strings.TrimSpace(result.ReportArtifactRef) == "" {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s has no content-addressed report", ref)
		}
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			return ProofResourceResult{}, true, err
		}
		body, err := artifacts.Get(result.ReportArtifactRef)
		if err != nil {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s report artifact: %w", ref, err)
		}
		report, err := decodeOwnedProofReport(body)
		if err != nil {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s report: %w", ref, err)
		}
		if err := s.validateCommittedProofResourceChain(w, result, report); err != nil {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s proof finalizer chain: %w", ref, err)
		}
		if result.SourceSnapshot == nil {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s predates sealed source identity; run a new battery", ref)
		}
		if err := gitrepository.ValidateSnapshotShape(*result.SourceSnapshot); err != nil {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s source snapshot: %w", ref, err)
		}
		if !result.SourceSnapshot.Clean {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s source snapshot is not clean", ref)
		}
		if report.Status != "ok" ||
			report.SourceSnapshot == nil ||
			*report.SourceSnapshot != *result.SourceSnapshot {
			return ProofResourceResult{}, true, fmt.Errorf("proof verification %s source does not match content-addressed report", ref)
		}
		return result, true, nil
	}
	return ProofResourceResult{}, false, nil
}

func latestCommitArtifact(artifactIDs []string) (ref, sha string, found, valid bool) {
	for i := len(artifactIDs) - 1; i >= 0; i-- {
		candidate := artifactIDs[i]
		if !strings.HasPrefix(candidate, "commit:") {
			continue
		}
		sha = strings.TrimPrefix(candidate, "commit:")
		return candidate, sha, true, validLandingObjectID(sha)
	}
	return "", "", false, false
}

func sealLandCheckReport(report *LandCheckReport) error {
	if report == nil {
		return fmt.Errorf("land-check report required")
	}
	digest, err := landCheckContentSHA256(*report)
	if err != nil {
		return err
	}
	report.ContentSHA256 = digest
	report.ID = landCheckIDPrefix + digest[:12]
	return nil
}

func landCheckContentSHA256(report LandCheckReport) (string, error) {
	// Recording metadata is derived from the sealed semantic content. Excluding
	// it avoids a circular digest while every repository, lineage, status, and
	// work-binding field remains covered automatically as the report evolves.
	report.ID = ""
	report.ContentSHA256 = ""
	report.Recorded = false
	report.EvidenceRef = ""
	report.ProofPath = ""
	data, err := json.Marshal(report)
	if err != nil {
		return "", fmt.Errorf("marshal land-check content: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) recordLandCheck(report *LandCheckReport) error {
	if report == nil || strings.TrimSpace(report.ID) == "" {
		return fmt.Errorf("land-check report id required")
	}
	dir := filepath.Join(s.home, "work", "land-checks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, report.ID+".json")
	report.Recorded = true
	report.EvidenceRef = landCheckEvidencePrefix + report.ID
	report.ProofPath = path
	staged, err := s.stageJSON(path, report)
	if err != nil {
		return err
	}
	if err := commitStagedJSONExclusive(staged, path); err != nil {
		_ = os.Remove(staged)
		if errors.Is(err, os.ErrExist) {
			var existing LandCheckReport
			if readErr := s.readJSON(path, &existing); readErr != nil {
				return fmt.Errorf("land-check %s collides with unreadable existing proof: %w", report.ID, readErr)
			}
			existingDigest, digestErr := landCheckContentSHA256(existing)
			if digestErr != nil || existing.ContentSHA256 != existingDigest ||
				existing.ContentSHA256 != report.ContentSHA256 || existing.ID != report.ID ||
				!existing.Recorded || existing.EvidenceRef != report.EvidenceRef ||
				filepath.Clean(existing.ProofPath) != filepath.Clean(report.ProofPath) {
				return fmt.Errorf("land-check %s collides with tampered existing proof", report.ID)
			}
			return nil
		}
		return err
	}
	return nil
}

func (s *Store) recordedLandedProofForClose(w envelope.WorkEnvelope, proofRefs []string) (LandCheckReport, error) {
	seen := false
	var failures []string
	for _, ref := range proofRefs {
		id, ok := landCheckIDFromEvidenceRef(ref)
		if !ok {
			continue
		}
		seen = true
		var report LandCheckReport
		proofPath := filepath.Join(s.home, "work", "land-checks", id+".json")
		if err := s.readJSON(proofPath, &report); err != nil {
			failures = append(failures, fmt.Sprintf("%s is not a recorded land-check: %v", ref, err))
			continue
		}
		if err := validateRecordedLandCheck(w, ref, id, proofPath, report); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if report.Status != "landed" {
			failures = append(failures, fmt.Sprintf("recorded land-check %s is not landed (status %s)", id, report.Status))
			continue
		}
		if err := s.validateRecordedLandCheckProofResult(w, report); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		return report, nil
	}
	if !seen {
		return LandCheckReport{}, fmt.Errorf("completed source-controlled work %s requires a cited recorded land-check proof", w.ID)
	}
	return LandCheckReport{}, fmt.Errorf("completed source-controlled work %s has no acceptable landed proof: %s", w.ID, strings.Join(failures, "; "))
}

func validateRecordedLandCheck(w envelope.WorkEnvelope, evidenceRef, id, proofPath string, report LandCheckReport) error {
	switch report.SchemaVersion {
	case LegacyLandCheckSchemaVersion:
		return fmt.Errorf("recorded land-check %s uses legacy/stale schema %s that predates repository identity; record a fresh %s land-check", id, LegacyLandCheckSchemaVersion, LandCheckSchemaVersion)
	case PreviousLandCheckSchemaVersion:
		return fmt.Errorf("recorded land-check %s uses legacy/stale schema %s predates sealed proof source identity; record fresh %s land-check", id, PreviousLandCheckSchemaVersion, LandCheckSchemaVersion)
	case LandCheckSchemaVersion:
		// Current sealed content is verified below.
	default:
		return fmt.Errorf("recorded land-check %s has unsupported schema %q", id, report.SchemaVersion)
	}
	if !report.Recorded {
		return fmt.Errorf("recorded land-check %s has invalid schema or recording state", id)
	}
	digest, err := landCheckContentSHA256(report)
	if err != nil {
		return fmt.Errorf("recorded land-check %s content digest: %w", id, err)
	}
	if report.ContentSHA256 == "" || report.ContentSHA256 != digest || id != landCheckIDPrefix+digest[:12] {
		return fmt.Errorf("recorded land-check %s content digest mismatch; proof content was modified after recording", id)
	}
	if report.ID != id || report.EvidenceRef != evidenceRef {
		return fmt.Errorf("recorded land-check %s identity does not match cited evidence", id)
	}
	if filepath.Clean(report.ProofPath) != filepath.Clean(proofPath) {
		return fmt.Errorf("recorded land-check %s proof path does not match its recorded identity", id)
	}
	if report.WorkID != w.ID {
		return fmt.Errorf("recorded land-check %s belongs to work %s, not %s", id, report.WorkID, w.ID)
	}
	if w.GitDelivery == nil {
		return fmt.Errorf("recorded land-check %s cannot satisfy work %s without git_delivery authority", id, w.ID)
	}
	contract := *w.GitDelivery
	if report.ExpectedBaseSHA != contract.ExpectedBaseSHA {
		return fmt.Errorf("recorded land-check %s expected base no longer matches work %s", id, w.ID)
	}
	if report.RepositoryFingerprint != contract.RepositoryFingerprint {
		return fmt.Errorf("recorded land-check %s repository fingerprint no longer matches work %s", id, w.ID)
	}
	if report.RepoPath != contract.TargetWorktreeRoot || report.TargetBranchRef != contract.TargetLocalRef ||
		report.PublicationMode != contract.PublicationMode || report.PublicationRef != contract.PublicationRef {
		return fmt.Errorf("recorded land-check %s repository, target, or publication identity no longer matches work %s", id, w.ID)
	}
	artifactRef, attachedTip, found, valid := latestCommitArtifact(w.ArtifactIDs)
	if !found || !valid || report.CandidateArtifactRef != artifactRef || report.AttachedCandidateTipSHA != attachedTip || report.CandidateTipSHA != attachedTip {
		return fmt.Errorf("recorded land-check %s candidate artifact no longer matches the latest verified commit attached to work %s", id, w.ID)
	}
	if !validLandingObjectID(report.CandidateTipSHA) || strings.TrimSpace(report.RepoPath) == "" || strings.TrimSpace(report.TargetRef) == "" {
		return fmt.Errorf("recorded land-check %s is missing exact repository, candidate, or target identity", id)
	}
	switch {
	case report.ProofVerificationRef == "" && report.ProofHeadSHA != "":
		return fmt.Errorf("recorded land-check %s has proof head without verification identity", id)
	case report.ProofVerificationRef != "" && !validRecordedLandCheckVerificationRef(report.ProofVerificationRef):
		return fmt.Errorf("recorded land-check %s has invalid proof verification identity", id)
	case report.ProofVerificationRef != "" && report.ProofHeadSHA != report.CandidateTipSHA:
		return fmt.Errorf("recorded land-check %s proof tip differs from candidate tip", id)
	}
	if report.Status == "landed" && !report.CandidateContainedInTarget {
		return fmt.Errorf("recorded land-check %s does not prove containment in bound target ref %s", id, contract.TargetLocalRef)
	}
	if report.Status == "landed" && contract.PublicationMode == "required" && (!report.CandidateContainedInPublication || strings.TrimSpace(report.PublicationSHA) == "") {
		return fmt.Errorf("recorded land-check %s does not prove containment in bound publication ref %s", id, contract.PublicationRef)
	}
	return nil
}

func validRecordedLandCheckVerificationRef(ref string) bool {
	if strings.HasPrefix(ref, ProofAdoptionVerificationPrefix) {
		_, _, ok := parseProofAdoptionRef(ref)
		return ok
	}
	if strings.HasPrefix(ref, "proof-check:pfrs_") {
		return true
	}
	_, _, ok := parseDifferentialProofVerificationRef(ref)
	return ok
}

func (s *Store) validateRecordedLandCheckProofResult(w envelope.WorkEnvelope, report LandCheckReport) error {
	acceptanceRef, result, found, err := s.latestLandingProofResolution(w)
	if err != nil {
		return err
	}
	if !found {
		if report.ProofVerificationRef != "" || report.ProofHeadSHA != "" {
			return fmt.Errorf("recorded land-check %s cites proof verification no longer attached to work %s", report.ID, w.ID)
		}
		return fmt.Errorf(
			"recorded land-check %s lacks committed source-bound proof for work %s; run fresh canonical certifying proof for attached candidate commit:%s, then rerun land-check",
			report.ID,
			w.ID,
			report.CandidateTipSHA,
		)
	}
	if report.ProofVerificationRef != acceptanceRef {
		// Recorded before a later battery stored: the land-check cites the
		// pre-land verification while the envelope's latest verification_ids
		// entry is a newer proof battery sealing the SAME tip. That is the
		// post-land-battery-not-yet-attached shape (recorded twice in
		// production: landcheck_87ffb48c4f08 p13 and the p15 first attempt).
		// Quoting a "proof mismatch" between two identical tips is meaningless;
		// name the missing post-land verification and its repair instead.
		if report.ProofHeadSHA == report.CandidateTipSHA &&
			report.CandidateTipSHA == result.SourceSnapshot.HeadSHA {
			return fmt.Errorf(
				"recorded land-check %s cites proof verification %s, but the latest proof verification stored on work %s is %s at the identical tip %s; missing post-land verification %s — record a fresh land-check after that post-land battery stored",
				report.ID,
				report.ProofVerificationRef,
				w.ID,
				acceptanceRef,
				result.SourceSnapshot.HeadSHA,
				acceptanceRef,
			)
		}
		return fmt.Errorf(
			"recorded land-check %s proof tip %s does not match current stored proof %s tip %s",
			report.ID,
			report.ProofHeadSHA,
			acceptanceRef,
			result.SourceSnapshot.HeadSHA,
		)
	}
	if report.ProofHeadSHA != result.SourceSnapshot.HeadSHA ||
		report.CandidateTipSHA != result.SourceSnapshot.HeadSHA {
		return fmt.Errorf(
			"recorded land-check %s proof tip %s does not match current stored proof %s tip %s",
			report.ID,
			report.ProofHeadSHA,
			acceptanceRef,
			result.SourceSnapshot.HeadSHA,
		)
	}
	return nil
}

func (s *Store) revalidateLandedProof(w envelope.WorkEnvelope, proof LandCheckReport, now time.Time) (LandCheckReport, error) {
	current, err := s.evaluateLandCheck(w, LandCheckOptions{
		CandidateTipSHA: proof.CandidateTipSHA,
	}, now)
	if err != nil {
		return LandCheckReport{}, fmt.Errorf("land-check %s live revalidation failed: %w", proof.ID, err)
	}
	if current.Status != "landed" {
		return LandCheckReport{}, fmt.Errorf("land-check %s live revalidation status %s: %s", proof.ID, current.Status, current.NextAction)
	}
	return current, nil
}

func landCheckIDFromEvidenceRef(ref string) (string, bool) {
	if !strings.HasPrefix(ref, landCheckEvidencePrefix) {
		return "", false
	}
	id := strings.TrimPrefix(ref, landCheckEvidencePrefix)
	if len(id) != len(landCheckIDPrefix)+12 || !strings.HasPrefix(id, landCheckIDPrefix) {
		return "", false
	}
	for _, r := range strings.TrimPrefix(id, landCheckIDPrefix) {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	return id, true
}

func canonicalLandingRepo(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("explicit repository path required")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("repository path must be absolute; cwd is not landing authority")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository path must name a worktree directory")
	}
	root, err := runLandingGitOutput(canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("repository path is not a Git worktree: %w", err)
	}
	root, err = filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolve Git worktree root: %w", err)
	}
	if root != canonical {
		return "", fmt.Errorf("repository path must name the explicit Git worktree root %s", root)
	}
	return root, nil
}

func normalizeLandingTargetRef(repoPath, value string) (string, string, error) {
	branch := strings.TrimSpace(value)
	if branch == "" {
		return "", "", fmt.Errorf("target branch ref required")
	}
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if branch == "" {
		return "", "", fmt.Errorf("target branch ref required")
	}
	if _, err := runLandingGitOutput(repoPath, "check-ref-format", "--branch", branch); err != nil {
		return "", "", fmt.Errorf("target ref must name a local branch: %w", err)
	}
	return branch, "refs/heads/" + branch, nil
}

func landingCurrentBranchRef(repoPath string) (string, error) {
	cmd := exec.Command("git", "-C", repoPath, "symbolic-ref", "--quiet", "HEAD")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", nil
	}
	return "", fmt.Errorf("inspect target worktree branch: %w: %s", err, strings.TrimSpace(string(out)))
}

func landingRefCheckedOut(repoPath, targetBranchRef string) (bool, error) {
	out, err := runLandingGitOutput(repoPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, fmt.Errorf("inspect worktrees for target ref %s: %w", targetBranchRef, err)
	}
	return landingWorktreeListHasBranch(out, targetBranchRef), nil
}

// landingWorktreeListHasBranch parses one porcelain worktree list for the
// exact checked-out branch ref. Shared by the direct read and the request-local
// facts session so both answer from one definition.
func landingWorktreeListHasBranch(out, targetBranchRef string) bool {
	want := "branch " + targetBranchRef
	for _, field := range strings.Split(out, "\x00") {
		if field == want {
			return true
		}
	}
	return false
}

func landingRefExists(repoPath, ref string) (bool, error) {
	if _, err := runLandingGitOutput(repoPath, "check-ref-format", ref); err != nil {
		return false, fmt.Errorf("git_delivery.publication_ref %q is not a valid Git ref: %w", ref, err)
	}
	cmd := exec.Command("git", "-C", repoPath, "show-ref", "--verify", "--quiet", ref)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect Git ref %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
}

func landingCommitExists(repoPath, sha string) (bool, error) {
	cmd := exec.Command("git", "-C", repoPath, "cat-file", "-e", sha+"^{commit}")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, fmt.Errorf("inspect Git commit %s: %w: %s", sha, err, strings.TrimSpace(string(out)))
}

func landingIsAncestor(repoPath, ancestor, descendant string) (bool, error) {
	cmd := exec.Command("git", "-C", repoPath, "merge-base", "--is-ancestor", ancestor, descendant)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check Git ancestry %s -> %s: %w: %s", ancestor, descendant, err, strings.TrimSpace(string(out)))
}

func runLandingGitOutput(repoPath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	// Preserve leading bytes: porcelain-v1 status uses them for the XY columns,
	// and TrimSpace corrupts the first path in a dirty worktree report. Git's
	// textual commands terminate with newlines, so strip only line endings.
	return strings.TrimRight(string(out), "\r\n"), nil
}

func validLandingObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
