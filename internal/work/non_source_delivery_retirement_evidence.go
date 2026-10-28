package work

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
)

func (s *Store) validateRetirementDeliveryEvidence(w envelope.WorkEnvelope, original envelope.ActionIntent, ref string) error {
	reader, err := s.privateDeliveryArtifactReader()
	if err != nil {
		return err
	}
	body, err := reader.GetPrivateBounded(ref, privateDeliveryMetadataLimit)
	if err != nil {
		return err
	}
	var evidence retirementDeliveryEvidence
	if err := decodeDeliveryCapture(body, &evidence); err != nil {
		return err
	}
	if evidence.SchemaVersion != retirementDeliveryEvidenceSchema || evidence.WorkID != w.ID {
		return fmt.Errorf("retirement evidence does not bind exact work and action")
	}
	var captured [4][]byte
	for i, ref := range []string{evidence.PlanRef, evidence.AuthorizationRef, evidence.ReviewedSourceRef, evidence.ResultRef} {
		captured[i], err = reader.GetPrivateBounded(ref, privateDeliveryMetadataLimit)
		if err != nil {
			return err
		}
	}
	delivery, err := gitrepository.ValidateRetirementDelivery(captured[0], captured[1], captured[2], captured[3])
	if err != nil {
		return err
	}
	proofs := delivery.Plan.RecoveryProofs
	if len(proofs) == 0 || len(proofs) > maxPrivateRuntimeDeliveryFiles || len(proofs) != len(evidence.RecoveryArtifacts) || len(proofs) != len(delivery.Plan.RecoveryRefs) {
		return fmt.Errorf("retirement recovery evidence inventory differs")
	}
	remaining := int64(privateDeliveryCaptureTotalBytes)
	for i, proof := range proofs {
		if proof.Kind != "sha256-file" || !filepath.IsAbs(proof.Path) || filepath.Clean(proof.Path) != proof.Path ||
			proof.Reference != delivery.Plan.RecoveryRefs[i] || proof.Reference != "sha256-file:"+strings.TrimPrefix(proof.Digest, "sha256:")+":"+proof.Path ||
			evidence.RecoveryArtifacts[i] != "artifact:"+proof.Digest {
			return fmt.Errorf("retirement recovery evidence does not match sealed proof")
		}
		body, err := reader.GetPrivateBounded(evidence.RecoveryArtifacts[i], min(remaining, privateDeliveryBodyLimit))
		if err != nil {
			return err
		}
		remaining -= int64(len(body))
	}

	var completion envelope.ActionIntent
	if evidence.FailedOutcome != nil {
		if evidence.FailedOutcome.ActionID != original.ID || evidence.ActionIntentID == original.ID {
			return fmt.Errorf("retirement failure selection differs from binding")
		}
		if err := s.readEventJSON(evidence.ActionIntentID, &completion); err != nil {
			return err
		}
	} else if evidence.ActionIntentID != original.ID {
		return fmt.Errorf("retirement evidence does not bind selected action")
	} else {
		completion = original
	}
	if err := s.validateRetirementDeliveryOutcome(w, completion, evidence, delivery); err != nil {
		return err
	}
	if evidence.FailedOutcome != nil {
		return s.validateRetirementFailureDelivery(w, original, evidence, delivery)
	}
	return nil
}

func (s *Store) validateRetirementDeliveryOutcome(w envelope.WorkEnvelope, original envelope.ActionIntent, evidence retirementDeliveryEvidence, delivery gitrepository.RetirementDelivery) error {
	plan, result := delivery.Plan, delivery.Result
	if plan.OwnerProof.WorkID != w.ID || result.ActionIntentID != original.ID || result.Actor != original.Actor || result.Actor != plan.OwnerProof.LeaseHolder ||
		original.Capability != "source-control.worktree-retire" || original.Action != "retire_redundant_git_surface" ||
		original.Target.Kind != "git-repository" || original.Target.ID != plan.ID || original.Target.Path != plan.RepoRoot ||
		original.MutationClass != "local_reversible" || original.Reversibility != "easy" || original.BlastRadius != "repo" {
		return fmt.Errorf("retirement outcome does not match exact owned effect")
	}
	if result.ID != "wtrres_"+gitrepository.DigestRetirementHex([]byte(plan.ID + "|" + delivery.Authorization.AuthorizationDigest))[:12] {
		return fmt.Errorf("retirement result identity differs from producer")
	}
	if err := validateRetirementDeliveryAction(original, delivery); err != nil {
		return err
	}
	proofRefs := gitrepository.WorktreeRetirementApplyProofRefs(plan, delivery.Authorization)
	record, err := s.events.Read(original.ID)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(record.Bytes)) != evidence.ActionSHA256 {
		return fmt.Errorf("retirement original action bytes changed")
	}
	if evidence.Outcome.ActionID != result.ActionIntentID || evidence.Outcome.PolicyDecisionID != result.PolicyDecisionID ||
		evidence.Outcome.ReceiptID != result.ReceiptID || evidence.Outcome.TransitionID != result.TransitionID ||
		evidence.Outcome.ReportRef != evidence.ResultRef {
		return fmt.Errorf("retirement result differs from captured Work transaction")
	}
	consumer, err := s.validateDeliveryOutcomeEvidence(w, evidence.Outcome)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(consumer.Action, original) {
		return fmt.Errorf("retirement original action differs from outcome")
	}
	if !consumer.Receipt.StateDelta.Changed || consumer.Receipt.MoveType != "control_plane_repair" {
		return fmt.Errorf("retirement receipt does not record completed repair effect")
	}
	wantProofRefs := mergeUnique(proofRefs, original.EvidenceRefs, []string{"git-ref:" + plan.CandidateRef + "@" + plan.CandidateTip}, []string{"worktree-retirement-result:" + result.ID}, []string{"work:" + w.ID})
	if !reflect.DeepEqual(consumer.Receipt.ProofRefs, wantProofRefs) {
		return fmt.Errorf("retirement receipt proof inventory differs from producer")
	}
	if !slices.Contains(consumer.Transition.BeforeRefs, "git-ref:"+plan.CandidateRef+"@"+plan.CandidateTip) {
		return fmt.Errorf("retirement transition omits before ref")
	}
	if !slices.Contains(consumer.Transition.AfterRefs, "worktree-retirement-result:"+result.ID) {
		return fmt.Errorf("retirement transition omits result identity")
	}
	identity, err := gitrepository.ReadIdentity(plan.RepoRoot, plan.AuthorityTip)
	if err != nil || identity.Fingerprint != plan.RepositoryFingerprint {
		return fmt.Errorf("retirement repository identity changed: %v", err)
	}
	return nil
}

// validateRetirementDeliveryAction checks the exact producer grammar, not a
// general exemption for repository mutations or action names.
func validateRetirementDeliveryAction(original envelope.ActionIntent, delivery gitrepository.RetirementDelivery) error {
	plan, result := delivery.Plan, delivery.Result
	if err := original.Validate(); err != nil {
		return err
	}
	if original.IDs.WorkID != plan.OwnerProof.WorkID || original.Actor != result.Actor ||
		original.Actor != plan.OwnerProof.LeaseHolder || original.Capability != "source-control.worktree-retire" ||
		original.Action != "retire_redundant_git_surface" || original.Target.Kind != "git-repository" ||
		original.Target.ID != plan.ID || original.Target.Path != plan.RepoRoot ||
		original.MutationClass != "local_reversible" || original.Reversibility != "easy" || original.BlastRadius != "repo" {
		return fmt.Errorf("retirement action differs from sealed operation")
	}
	proofRefs := gitrepository.WorktreeRetirementApplyProofRefs(plan, delivery.Authorization)
	// A resumed attempt may name the prior failed result. It is action evidence,
	// not a new success or permission; the receipt still binds the original plan.
	salt := ""
	for _, ref := range original.EvidenceRefs {
		if slices.Contains(proofRefs, ref) {
			continue
		}
		if salt != "" || !strings.HasPrefix(ref, "worktree-retirement-prior-result:") {
			return fmt.Errorf("retirement action has unexpected evidence")
		}
		salt = strings.TrimPrefix(ref, "worktree-retirement-prior-result:")
		if !gitrepository.ValidRetirementDigest(salt) {
			return fmt.Errorf("retirement prior-result identity malformed")
		}
	}
	wantActionID := "act_" + gitrepository.DigestRetirementHex([]byte(plan.ID + "|" + delivery.Authorization.AuthorizationDigest + "|" + result.Actor + "|" + salt))[:12]
	expectedRefs := len(proofRefs)
	if salt != "" {
		expectedRefs++
	}
	if original.ID != wantActionID || len(original.EvidenceRefs) != expectedRefs {
		return fmt.Errorf("retirement action identity differs from producer")
	}
	for _, ref := range proofRefs {
		if !slices.Contains(original.EvidenceRefs, ref) {
			return fmt.Errorf("retirement action omits sealed evidence")
		}
	}
	return nil
}
