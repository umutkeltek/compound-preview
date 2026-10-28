package work

import (
	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WorktreeRetirementSourceProof derives retirement evidence from owned terminal
// work and its receipt, then revalidates the exact sealed landing contract.
func (s *Store) WorktreeRetirementSourceProof(workID string, now time.Time) (gitrepository.WorktreeRetirementSourceProof, error) {
	item, err := s.ProofChainForWork(strings.TrimSpace(workID))
	if err != nil {
		return gitrepository.WorktreeRetirementSourceProof{}, err
	}
	w, err := s.Get(strings.TrimSpace(workID))
	if err != nil {
		return gitrepository.WorktreeRetirementSourceProof{}, err
	}
	if (w.State != "completed" && w.State != "failed") || !item.ProofComplete {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s needs completed or failed terminal proof chain: %v", w.ID, item.Problems)
	}
	record, err := s.events.Read(item.ReceiptID)
	if err != nil {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s receipt %s unreadable: %w", w.ID, item.ReceiptID, err)
	}
	var receipt envelope.Receipt
	if err := json.Unmarshal(record.Bytes, &receipt); err != nil {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s receipt %s malformed: %w", w.ID, item.ReceiptID, err)
	}
	if err := receipt.Validate(); err != nil {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s receipt %s invalid: %w", w.ID, item.ReceiptID, err)
	}
	if receipt.IDs.WorkID != w.ID {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s receipt %s belongs to work %s", w.ID, item.ReceiptID, receipt.IDs.WorkID)
	}
	if receipt.Outcome != w.State {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s terminal state %s does not match receipt outcome %s", w.ID, w.State, receipt.Outcome)
	}
	commitRef, commitTip, err := latestWorktreeRetirementCommit(w.ArtifactIDs)
	if err != nil {
		return gitrepository.WorktreeRetirementSourceProof{}, err
	}
	var selected LandCheckReport
	var selectedDigest string
	for i := len(w.EvidenceRefs) - 1; i >= 0; i-- {
		ref := w.EvidenceRefs[i]
		if !strings.HasPrefix(ref, "land-check:landcheck_") {
			continue
		}
		id := strings.TrimPrefix(ref, "land-check:")
		path := filepath.Join(s.home, "work", "land-checks", id+".json")
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var report LandCheckReport
		if json.Unmarshal(data, &report) != nil || !validWorktreeRetirementLandCheck(report, w, id, ref, path, commitTip) {
			continue
		}
		digest := sha256.Sum256(data)
		selected = report
		selectedDigest = "sha256:" + hex.EncodeToString(digest[:])
		break
	}
	if selected.ID == "" {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s has no sealed landed land-check matching %s", w.ID, commitRef)
	}
	live, err := s.LandCheck(w.ID, LandCheckOptions{CandidateTipSHA: commitTip}, now)
	if err != nil || live.Status != "landed" {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s land-check is not currently landed: status=%s err=%v", w.ID, live.Status, err)
	}
	if selected.ProofVerificationRef != live.ProofVerificationRef || selected.ProofHeadSHA != live.ProofHeadSHA {
		return gitrepository.WorktreeRetirementSourceProof{}, fmt.Errorf("source work %s sealed proof differs from current source proof", w.ID)
	}
	data, err := json.Marshal(w)
	if err != nil {
		return gitrepository.WorktreeRetirementSourceProof{}, err
	}
	digest := sha256.Sum256(data)
	d := w.GitDelivery
	return gitrepository.WorktreeRetirementSourceProof{
		Delivery: &gitrepository.WorktreeRetirementSourceDelivery{WorkDigest: "sha256:" + hex.EncodeToString(digest[:]), TargetWorktreeRoot: d.TargetWorktreeRoot, RepositoryFingerprint: d.RepositoryFingerprint, TargetLocalRef: d.TargetLocalRef, ExpectedBaseSHA: d.ExpectedBaseSHA, PublicationMode: d.PublicationMode, PublicationRef: d.PublicationRef},
		WorkID:   w.ID, State: w.State, ProofComplete: item.ProofComplete,
		ActionIntentIDs: append([]string{}, item.ActionIntentIDs...), PolicyDecisionID: item.PolicyDecisionID,
		ReceiptID: item.ReceiptID, ReceiptOutcome: receipt.Outcome, TransitionID: item.TransitionID,
		CommitArtifactRef: commitRef, CommitTip: commitTip,
		LandCheckID: selected.ID, LandCheckEvidenceRef: selected.EvidenceRef,
		LandCheckStatus: selected.Status, LandCheckCandidateTip: selected.CandidateTipSHA,
		LandCheckFileDigest: selectedDigest,
	}, nil
}

func latestWorktreeRetirementCommit(refs []string) (string, string, error) {
	for i := len(refs) - 1; i >= 0; i-- {
		if !strings.HasPrefix(refs[i], "commit:") {
			continue
		}
		sha := strings.TrimPrefix(refs[i], "commit:")
		if !gitrepository.ValidRetirementObjectID(sha) {
			return "", "", fmt.Errorf("source work latest commit artifact malformed")
		}
		return refs[i], sha, nil
	}
	return "", "", fmt.Errorf("source work needs commit:<full-sha> artifact")
}

func validWorktreeRetirementLandCheck(report LandCheckReport, w envelope.WorkEnvelope, id, evidenceRef, path, commitTip string) bool {
	d := w.GitDelivery
	if d == nil || report.WorkID != w.ID || report.RepoPath != d.TargetWorktreeRoot || report.RepositoryFingerprint != d.RepositoryFingerprint || report.TargetBranchRef != d.TargetLocalRef || report.ExpectedBaseSHA != d.ExpectedBaseSHA || report.PublicationMode != d.PublicationMode || report.PublicationRef != d.PublicationRef {
		return false
	}
	if report.SchemaVersion != LandCheckSchemaVersion || !report.Recorded || report.Status != "landed" || report.ID != id || report.EvidenceRef != evidenceRef || report.CandidateTipSHA != commitTip || filepath.Clean(report.ProofPath) != filepath.Clean(path) {
		return false
	}
	sealed := report
	sealed.ID = ""
	sealed.ContentSHA256 = ""
	sealed.Recorded = false
	sealed.EvidenceRef = ""
	sealed.ProofPath = ""
	data, err := json.Marshal(sealed)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(data)
	content := hex.EncodeToString(digest[:])
	return report.ContentSHA256 == content && report.ID == "landcheck_"+content[:12]
}
