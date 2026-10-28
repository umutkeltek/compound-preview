package product

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
)

// HypothesisDraftExpectedEffectPlaceholder is declared by the producer, in
// hypothesis_draft.go — one owner for the shared string. The gates below match
// on the prefix constant instead of restating that template.

// HypothesisDraftExpectedEffectPlaceholderPrefix is the fixed head every
// placeholder-formatted expected_effect starts with. Gates match on this
// prefix rather than the full template so the refusal also catches hand-typed
// variants of the same placeholder. The coupling test in
// hypothesis_approve_test.go asserts the prefix still heads the template:
// renaming the template literal without moving this prefix turns that test red
// instead of silently unlocking the gates.
const HypothesisDraftExpectedEffectPlaceholderPrefix = "OPERATOR MUST WRITE"

// HypothesisApproveInput is the operator's approval write for one parked
// product_hypothesis draft. ExpectedEffect is the sentence the operator owes;
// Metrics and ObservationWindow are optional refinements of the draft's own
// fields; ReviewedBy names the approving human (empty defaults to "operator").
type HypothesisApproveInput struct {
	ResumeApprovalID  string   `json:"resume_approval_id,omitempty"`
	CandidateID       string   `json:"candidate_id"`
	ExpectedEffect    string   `json:"expected_effect"`
	Metrics           []string `json:"metrics"`
	ObservationWindow string   `json:"observation_window"`
	ReviewedBy        string   `json:"reviewed_by"`
}

// HypothesisApproveResult is the receipted outcome of one approval.
type HypothesisApproveResult struct {
	Status       string
	Candidate    HypothesisCandidate
	ApprovalID   string // id of the approval act recorded on the loop transition
	TransitionID string
	SnapshotRef  string
}

// recordLoopTransition is the transition-recording seam the approval door
// uses (tests inject failures and concurrent-writer wins at exactly the
// step the approval-integrity contract cares about).
var recordLoopTransition = RecordTransition

// HypothesisApprove is the governed door around the placeholder gate: the one
// verb that receives the operator's expected_effect sentence and writes it
// onto a parked hypothesis draft, receipted by an approval intent and exactly
// one outcome transition instead of a silent hand-edit of the candidate JSON.
//
// Three refusals are load-bearing (falsifiers 3, 4, and the verdict gate):
//   - an expected_effect that still starts with the operator placeholder is
//     refused — approving a draft by handing back its own blank is a no-op
//     wearing a receipt;
//   - a candidate whose verdict is not `park` is refused — only a parked draft
//     awaits this door; operationalize/reject are other verbs' words;
//   - a candidate whose result is already recorded is refused — the loop is
//     closed and the review verdict (not approval) is its door.
//
// The verdict deliberately STAYS `park` and the result stays `unknown`:
// `verdict == park && result == unknown` is the predicate that keeps a
// hypothesis in PendingReview (DoctorWithOptions), which feeds review-ready
// matching, and isOpenLoopHypothesis closes on a recorded result — flipping to
// `operationalize` here would silently drop the approved hypothesis OUT of the
// measurement path. Approval writes the sentence; measurement and the review
// verdict remain the loop's own later steps.
func HypothesisApprove(homePath string, in HypothesisApproveInput, now time.Time) (HypothesisApproveResult, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return HypothesisApproveResult{}, fmt.Errorf("COMPOUND_HOME required")
	}
	candidateID := strings.TrimSpace(in.CandidateID)
	if candidateID == "" {
		return HypothesisApproveResult{}, fmt.Errorf("candidate id required")
	}
	if strings.TrimSpace(in.ResumeApprovalID) != "" {
		return resumeHypothesisApproval(homePath, in)
	}
	expectedEffect := strings.TrimSpace(in.ExpectedEffect)
	if expectedEffect == "" {
		return HypothesisApproveResult{}, fmt.Errorf("expected_effect required: the operator sentence is the hypothesis")
	}
	if strings.HasPrefix(expectedEffect, HypothesisDraftExpectedEffectPlaceholderPrefix) {
		return HypothesisApproveResult{}, fmt.Errorf("expected_effect is still the operator placeholder %q: write what the metric should do after the change, in your own words", HypothesisDraftExpectedEffectPlaceholderPrefix)
	}
	actor := strings.TrimSpace(in.ReviewedBy)
	if actor == "" {
		actor = "operator"
	}
	// Exact-generation guard (2026-09-15): the approval CASes on the exact
	// candidate record the gates just read, so a concurrent writer cannot win
	// between the gate checks and the candidate write. ReadRecord is the
	// descriptor-rooted read; ReplaceCandidateIfCurrent refuses a newer
	// generation instead of overwriting it.
	record, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
	if err != nil {
		return HypothesisApproveResult{}, err
	}
	var candidate HypothesisCandidate
	if err := json.Unmarshal(record.Body, &candidate); err != nil {
		return HypothesisApproveResult{}, fmt.Errorf("parse product hypothesis %q: %w", candidateID, err)
	}
	if pendingID, err := pendingHypothesisApproval(homePath, candidate); err != nil || pendingID != "" {
		if err == nil {
			err = fmt.Errorf("this candidate has an unsettled approval; do not mint another operation")
		}
		return approvalRecoveryRequired(HypothesisApproveResult{Candidate: candidate, ApprovalID: pendingID}, err)
	}
	if strings.TrimSpace(candidate.Verdict) != candidates.Park {
		return HypothesisApproveResult{}, fmt.Errorf("product hypothesis %s verdict is %q: only a parked hypothesis can be approved", candidateID, candidate.Verdict)
	}
	if result := strings.TrimSpace(candidate.Result); result != "" && result != "unknown" {
		return HypothesisApproveResult{}, fmt.Errorf("product hypothesis %s already has result %q: the loop is closed; approval is not the review door", candidateID, candidate.Result)
	}
	if strings.TrimSpace(candidate.ProductID) == "" {
		return HypothesisApproveResult{}, fmt.Errorf("product hypothesis %s is missing product_id: cannot receipt an approval on the product loop", candidateID)
	}
	approvalID := stableID("pha_", candidateID, expectedEffect, actor, now.UTC().Format(time.RFC3339Nano))
	candidate = projectHypothesisApproval(candidate, in, actor, approvalID)
	snapshot, snapshotRef, err := freezeHypothesisApproval(homePath, record, candidate, in, actor, approvalID, now)
	if err != nil {
		return HypothesisApproveResult{}, err
	}
	// Freeze the request before its intent; neither record claims the CAS won.
	intent, err := approvalSnapshotTransition(snapshot, snapshotRef, "product_hypothesis_approve_intent", "")
	if err != nil {
		return HypothesisApproveResult{}, err
	}
	if err := recordLoopTransition(homePath, intent); err != nil {
		return HypothesisApproveResult{}, err
	}
	result := HypothesisApproveResult{
		Candidate:  HypothesisCandidate{ID: candidate.ID, ProductID: candidate.ProductID},
		ApprovalID: approvalID, SnapshotRef: snapshotRef,
	}
	replaced, err := candidates.ReplaceCandidateIfCurrent(homePath, record, candidate)
	if err != nil {
		return approvalRecoveryRequired(result, err)
	}
	if !replaced.Replaced {
		refusal := fmt.Errorf("product hypothesis %s changed since the gate read (now %q): approval refused", candidateID, replaced.Current.Digest)
		outcome, err := approvalSnapshotTransition(snapshot, snapshotRef, "product_hypothesis_approve_refused", replaced.Current.Digest)
		if err == nil {
			err = recordLoopTransition(homePath, outcome)
		}
		if err != nil {
			return approvalRecoveryRequired(result, fmt.Errorf("%w; recording that refusal also failed: %v", refusal, err))
		}
		return HypothesisApproveResult{}, refusal
	}
	result.Candidate = candidate
	tr, err := approvalSnapshotTransition(snapshot, snapshotRef, "product_hypothesis_approve", "")
	if err != nil {
		return approvalRecoveryRequired(result, err)
	}
	if err := recordLoopTransition(homePath, tr); err != nil {
		return approvalRecoveryRequired(result, fmt.Errorf("approval committed but its outcome was not recorded: %w", err))
	}
	result.Status, result.TransitionID = "committed", tr.ID
	return result, nil
}

// approvalTransitionInput names the three approval records one act can produce:
// the intent, and the committed or refused outcome that settles it.
type approvalTransitionInput struct {
	Kind        string
	CandidateID string
	ApprovalID  string
	Actor       string
	// Committed is the only thing that puts the approval act into after_refs.
	// An intent and a refusal both leave the hypothesis where they found it, so
	// neither may name an approval among the refs that describe the state after
	// the move.
	Committed bool
	Summary   string
}

func buildApprovalTransition(candidate HypothesisCandidate, in approvalTransitionInput, now time.Time) (envelope.Transition, error) {
	after := []string{"product_hypothesis:" + in.CandidateID}
	if in.Committed {
		after = append(after, "product_hypothesis_approve:"+in.ApprovalID)
	}
	return BuildLoopTransition(LoopTransitionInput{
		Kind:      in.Kind,
		ProductID: candidate.ProductID,
		ObjectID:  in.ApprovalID,
		Actor:     in.Actor,
		// One source trace across all three kinds: intent and outcome are two
		// records of the same approval act, joinable by any reader.
		SourceTrace:  "product_hypothesis_approve:" + in.CandidateID,
		BeforeRefs:   compact(append([]string{"product_hypothesis:" + in.CandidateID}, candidate.EvidenceRefs...)),
		AfterRefs:    compact(append(after, candidate.EvidenceRefs...)),
		EvidenceRefs: candidate.EvidenceRefs,
		Summary:      in.Summary,
	}, now)
}
