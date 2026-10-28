package product

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
)

const hypothesisApprovalSnapshotSchema = "compound.product_hypothesis_approval_snapshot.v0.1"

// The intent's content-addressed body freezes one operation, not another
// transaction registry. Byte images retain the exact candidate generations;
// the custody token records which gate-read generation authorized the CAS.
type hypothesisApprovalSnapshot struct {
	SchemaVersion string                 `json:"schema_version"`
	ApprovalID    string                 `json:"approval_id"`
	CandidateID   string                 `json:"candidate_id"`
	ProductID     string                 `json:"product_id"`
	Actor         string                 `json:"actor"`
	CreatedAt     string                 `json:"created_at"`
	Request       HypothesisApproveInput `json:"request"`
	Preimage      []byte                 `json:"preimage"`
	PreDigest     string                 `json:"preimage_digest"`
	PreCustody    string                 `json:"preimage_custody_token"`
	Postimage     []byte                 `json:"proposed_postimage"`
	PostDigest    string                 `json:"proposed_postimage_digest"`
}

func approvalWitness(id string) string { return "product_hypothesis_approve:" + id }

func projectHypothesisApproval(candidate HypothesisCandidate, in HypothesisApproveInput, actor, approvalID string) HypothesisCandidate {
	candidate.ExpectedEffect = strings.TrimSpace(in.ExpectedEffect)
	if metrics := compact(in.Metrics); len(metrics) > 0 {
		candidate.Metrics = metrics
	}
	if window := strings.TrimSpace(in.ObservationWindow); window != "" {
		candidate.ObservationWindow = window
	}
	if strings.TrimSpace(candidate.Result) == "" {
		candidate.Result = "unknown"
	}
	candidate.NextAction = fmt.Sprintf("approved by %s: import before/after metric evidence and record product review", actor)
	// The dedicated witness becomes visible in the SAME replacement as the
	// sentence. ApplyReview preserves it; its input can only append citations.
	candidate.CommittedApprovalIDs = compact(append(candidate.CommittedApprovalIDs, approvalID))
	candidate.EvidenceRefs = compact(append(candidate.EvidenceRefs, approvalWitness(approvalID)))
	return candidate
}

func approvalCandidateImage(candidate HypothesisCandidate) ([]byte, error) {
	// Match candidates' canonical payload encoding; recovery tests compare the
	// frozen image with the actual descriptor-rooted committed record.
	body, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func approvalCandidateDigest(body []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}

func freezeHypothesisApproval(homePath string, before candidates.Record, after HypothesisCandidate, in HypothesisApproveInput, actor, id string, now time.Time) (hypothesisApprovalSnapshot, string, error) {
	postimage, err := approvalCandidateImage(after)
	if err != nil {
		return hypothesisApprovalSnapshot{}, "", err
	}
	snapshot := hypothesisApprovalSnapshot{
		SchemaVersion: hypothesisApprovalSnapshotSchema, ApprovalID: id, CandidateID: before.ID,
		ProductID: after.ProductID, Actor: actor, CreatedAt: now.UTC().Format(time.RFC3339Nano), Request: in,
		Preimage: before.Body, PreDigest: before.Digest, PreCustody: before.CustodyToken,
		Postimage: postimage, PostDigest: approvalCandidateDigest(postimage),
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return snapshot, "", err
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		return snapshot, "", err
	}
	ref, err := artifacts.PutPrivate(body)
	return snapshot, ref, err
}

// Every replay builds the same terminal bytes from the frozen operation,
// including its original clock and evidence. A measurement is never used to
// reconstruct or replace the old approval payload.
func approvalSnapshotTransition(snapshot hypothesisApprovalSnapshot, ref, kind, currentDigest string) (envelope.Transition, error) {
	var before HypothesisCandidate
	if err := json.Unmarshal(snapshot.Preimage, &before); err != nil {
		return envelope.Transition{}, err
	}
	candidate := before
	committed := kind == "product_hypothesis_approve"
	if committed {
		if err := json.Unmarshal(snapshot.Postimage, &candidate); err != nil {
			return envelope.Transition{}, err
		}
	}
	at, err := time.Parse(time.RFC3339Nano, snapshot.CreatedAt)
	if err != nil {
		return envelope.Transition{}, err
	}
	candidate.EvidenceRefs = compact(append(candidate.EvidenceRefs, ref))
	var summary string
	switch kind {
	case "product_hypothesis_approve_intent":
		summary = fmt.Sprintf("product hypothesis %s approval intent by %s: frozen sentence, parameters and candidate generations; no committed outcome until exact-generation CAS settles", snapshot.CandidateID, snapshot.Actor)
	case "product_hypothesis_approve":
		summary = fmt.Sprintf("product hypothesis %s approved by %s: expected_effect written in the recorded generation, verdict stays park in that generation; later measurement state is unchanged; no product surface changed", snapshot.CandidateID, snapshot.Actor)
	case "product_hypothesis_approve_refused":
		summary = fmt.Sprintf("product hypothesis %s approval refused for %s: candidate changed since the gate read (now %q), so this approval wrote no expected_effect and no product surface changed", snapshot.CandidateID, snapshot.Actor, currentDigest)
	default:
		return envelope.Transition{}, fmt.Errorf("unknown hypothesis approval outcome %q", kind)
	}
	tr, err := buildApprovalTransition(candidate, approvalTransitionInput{
		Kind: kind, CandidateID: snapshot.CandidateID, ApprovalID: snapshot.ApprovalID,
		Actor: snapshot.Actor, Committed: committed, Summary: summary,
	}, at)
	// ActionRefs names the one frozen payload; EvidenceRefs also carries its
	// direct artifact reference for existing transition consumers.
	tr.ActionRefs = append(tr.ActionRefs, ref)
	tr.BeforeRefs = compact(append([]string{"product_hypothesis:" + snapshot.CandidateID}, before.EvidenceRefs...))
	return tr, err
}

func readHypothesisApprovalSnapshot(homePath string, intent envelope.Transition, candidateID, approvalID string) (hypothesisApprovalSnapshot, string, error) {
	var snapshot hypothesisApprovalSnapshot
	ref := ""
	for _, actionRef := range intent.ActionRefs {
		if strings.HasPrefix(actionRef, home.ArtifactRefPrefix) {
			if ref != "" {
				return snapshot, "", fmt.Errorf("approval intent has multiple frozen payloads")
			}
			ref = actionRef
		}
	}
	if ref == "" || !slices.Contains(intent.EvidenceRefs, ref) {
		return snapshot, ref, fmt.Errorf("approval intent has no bound frozen payload; legacy intent alone cannot prove a committed replacement")
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		return snapshot, ref, err
	}
	body, err := artifacts.GetPrivate(ref)
	if err != nil {
		return snapshot, ref, err
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return snapshot, ref, err
	}
	if snapshot.SchemaVersion != hypothesisApprovalSnapshotSchema || snapshot.ApprovalID != approvalID || snapshot.CandidateID != candidateID || snapshot.PreCustody == "" ||
		snapshot.PreDigest != approvalCandidateDigest(snapshot.Preimage) || snapshot.PostDigest != approvalCandidateDigest(snapshot.Postimage) {
		return snapshot, ref, fmt.Errorf("approval payload identity or generation evidence does not match its intent")
	}
	var before HypothesisCandidate
	if err := json.Unmarshal(snapshot.Preimage, &before); err != nil {
		return snapshot, ref, err
	}
	if before.ID != candidateID || before.ProductID != snapshot.ProductID || strings.TrimSpace(snapshot.Request.CandidateID) != candidateID || snapshot.Request.ResumeApprovalID != "" {
		return snapshot, ref, fmt.Errorf("approval payload candidate binding is invalid")
	}
	at, err := time.Parse(time.RFC3339Nano, snapshot.CreatedAt)
	if err != nil {
		return snapshot, ref, err
	}
	actor := strings.TrimSpace(snapshot.Request.ReviewedBy)
	if actor == "" {
		actor = "operator"
	}
	if actor != snapshot.Actor || approvalID != stableID("pha_", candidateID, strings.TrimSpace(snapshot.Request.ExpectedEffect), actor, at.UTC().Format(time.RFC3339Nano)) {
		return snapshot, ref, fmt.Errorf("approval payload request does not match its operation identity")
	}
	expectedPost, err := approvalCandidateImage(projectHypothesisApproval(before, snapshot.Request, actor, approvalID))
	if err != nil || !bytes.Equal(expectedPost, snapshot.Postimage) {
		return snapshot, ref, fmt.Errorf("approval payload postimage does not match its frozen request")
	}
	expectedIntent, err := approvalSnapshotTransition(snapshot, ref, "product_hypothesis_approve_intent", "")
	if err != nil {
		return snapshot, ref, err
	}
	expectedBytes, err := json.Marshal(expectedIntent)
	if err != nil {
		return snapshot, ref, err
	}
	actualBytes, err := json.Marshal(intent)
	if err != nil || !bytes.Equal(expectedBytes, actualBytes) {
		return snapshot, ref, fmt.Errorf("approval intent differs from its frozen payload")
	}
	return snapshot, ref, nil
}

func approvalRecoveryRequired(result HypothesisApproveResult, err error) (HypothesisApproveResult, error) {
	result.Status = "recovery_required"
	return result, fmt.Errorf("recovery_required: approval %s: %w; resume the original with hypothesis-approve %s --resume-approval %s", result.ApprovalID, err, result.Candidate.ID, result.ApprovalID)
}

func resumeHypothesisApproval(homePath string, in HypothesisApproveInput) (HypothesisApproveResult, error) {
	id := strings.TrimSpace(in.ResumeApprovalID)
	result := HypothesisApproveResult{ApprovalID: id, Candidate: HypothesisCandidate{ID: strings.TrimSpace(in.CandidateID)}}
	if in.ExpectedEffect != "" || len(in.Metrics) > 0 || in.ObservationWindow != "" || in.ReviewedBy != "" {
		return approvalRecoveryRequired(result, fmt.Errorf("resume cannot change the original sentence, parameters or actor"))
	}
	current, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, result.Candidate.ID)
	if err != nil {
		return approvalRecoveryRequired(result, err)
	}
	if err := json.Unmarshal(current.Body, &result.Candidate); err != nil {
		return approvalRecoveryRequired(result, err)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return approvalRecoveryRequired(result, err)
	}
	intentID := stableID("tr_", "product_hypothesis_approve_intent", result.Candidate.ProductID, id)
	record, err := events.Read(intentID)
	if err != nil {
		return approvalRecoveryRequired(result, fmt.Errorf("read original intent: %w", err))
	}
	var intent envelope.Transition
	if err := json.Unmarshal(record.Bytes, &intent); err != nil {
		return approvalRecoveryRequired(result, err)
	}
	snapshot, ref, err := readHypothesisApprovalSnapshot(homePath, intent, result.Candidate.ID, id)
	result.SnapshotRef = ref
	if err != nil {
		return approvalRecoveryRequired(result, err)
	}
	committed, err := approvalSnapshotTransition(snapshot, ref, "product_hypothesis_approve", "")
	if err != nil {
		return approvalRecoveryRequired(result, err)
	}
	_, committedErr := events.Read(committed.ID)
	refusedID := stableID("tr_", "product_hypothesis_approve_refused", snapshot.ProductID, id)
	refusedRecord, refusedErr := events.Read(refusedID)
	if committedErr != nil && !errors.Is(committedErr, os.ErrNotExist) {
		return approvalRecoveryRequired(result, committedErr)
	}
	if refusedErr != nil && !errors.Is(refusedErr, os.ErrNotExist) {
		return approvalRecoveryRequired(result, refusedErr)
	}
	if refusedErr == nil {
		if committedErr == nil || slices.Contains(result.Candidate.CommittedApprovalIDs, id) {
			return approvalRecoveryRequired(result, fmt.Errorf("conflicting refusal and committed evidence"))
		}
		var refused envelope.Transition
		if err := json.Unmarshal(refusedRecord.Bytes, &refused); err != nil {
			return approvalRecoveryRequired(result, err)
		}
		if refused.Validate() != nil || refused.ID != refusedID || refused.SourceTraceID != intent.SourceTraceID ||
			refused.Scope.ProductID != snapshot.ProductID || !slices.Contains(refused.ActionRefs, ref) ||
			slices.Contains(refused.AfterRefs, approvalWitness(id)) {
			return approvalRecoveryRequired(result, fmt.Errorf("recorded refusal does not match the original approval"))
		}
		result.Status, result.TransitionID = "refused", refusedID
		return result, fmt.Errorf("approval %s was refused; recovery leaves the current candidate unchanged", id)
	}
	if committedErr != nil && current.Digest != snapshot.PostDigest && !slices.Contains(result.Candidate.CommittedApprovalIDs, id) {
		return approvalRecoveryRequired(result, fmt.Errorf("no committed outcome or surviving replacement witness; current generation %s is not proof of whether this approval committed", current.Digest))
	}
	// PutJSON is immutable and idempotent: a raced or retried outcome must
	// have these exact original bytes. Recovery NEVER replaces the candidate.
	if err := recordLoopTransition(homePath, committed); err != nil {
		return approvalRecoveryRequired(result, err)
	}
	result.Status, result.TransitionID = "committed", committed.ID
	return result, nil
}

func pendingHypothesisApproval(homePath string, candidate HypothesisCandidate) (string, error) {
	events, err := eventstore.Open(homePath)
	if err != nil {
		return "", err
	}
	for _, id := range candidate.CommittedApprovalIDs {
		_, err := events.Read(stableID("tr_", "product_hypothesis_approve", candidate.ProductID, id))
		if errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
		if err != nil {
			return id, err
		}
	}
	return "", nil
}

// Approval history and the current measured candidate are separate facts.
func RenderHypothesisApprovalResult(result HypothesisApproveResult) string {
	return fmt.Sprintf("# product hypothesis-approve %s\n\n- approval: %s\n- status: %s\n- candidate_expected_effect: %s\n- candidate_result: %s\n- verdict: %s\n- next_action: %s\n- transition: %s\n",
		result.Candidate.ID, result.ApprovalID, result.Status, result.Candidate.ExpectedEffect,
		result.Candidate.Result, result.Candidate.Verdict, result.Candidate.NextAction, result.TransitionID)
}
