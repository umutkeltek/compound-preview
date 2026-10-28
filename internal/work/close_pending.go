package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

const closePendingSchemaVersion = "compound.work_close_pending.v0.2"

type closePendingAuthority struct {
	Actor                         string                             `json:"actor"`
	Outcome                       string                             `json:"outcome"`
	MoveType                      string                             `json:"move_type"`
	Steal                         bool                               `json:"steal,omitempty"`
	StealReason                   string                             `json:"steal_reason,omitempty"`
	SkipRunAttemptTerminalization bool                               `json:"skip_run_attempt_terminalization,omitempty"`
	RunAttemptTerminalizations    []RunAttemptTerminalizationRequest `json:"run_attempt_terminalizations,omitempty"`
}

type closePendingRunAttempt struct {
	Attempt        runattempt.RunAttempt            `json:"attempt"`
	ExpectedStatus string                           `json:"expected_status"`
	Intent         runattempt.TerminalizationIntent `json:"intent"`
	CallerOwns     bool                             `json:"caller_owns,omitempty"`
}

type closePendingLease struct {
	Present bool   `json:"present"`
	Body    []byte `json:"body,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

type closePendingGit struct {
	RepositoryPath string                    `json:"repository_path,omitempty"`
	Expectations   []gitReferenceExpectation `json:"expectations,omitempty"`
	Proof          *LandCheckReport          `json:"proof,omitempty"`
}

// closePending is the exact transaction identity for one terminal close. It
// exists before the first close authority mutation and survives until the
// lifecycle event, terminal WorkEnvelope, Git proof locks, RunAttempts, and
// captured lease have all converged.
type closePending struct {
	NonSourceEvidence map[string][]byte        `json:"non_source_evidence,omitempty"`
	SchemaVersion     string                   `json:"schema_version"`
	FrameSHA256       string                   `json:"frame_sha256"`
	WorkID            string                   `json:"work_id"`
	InputSHA256       string                   `json:"input_sha256"`
	Authority         closePendingAuthority    `json:"authority"`
	CompletedAt       string                   `json:"completed_at"`
	SourceWork        envelope.WorkEnvelope    `json:"source_work"`
	SourceWorkSHA256  string                   `json:"source_work_sha256"`
	FinalWork         envelope.WorkEnvelope    `json:"final_work"`
	FinalWorkSHA256   string                   `json:"final_work_sha256"`
	Action            envelope.ActionIntent    `json:"action"`
	Decision          envelope.PolicyDecision  `json:"decision"`
	Receipt           envelope.Receipt         `json:"receipt"`
	Transition        envelope.Transition      `json:"transition"`
	Event             Event                    `json:"event"`
	Lease             closePendingLease        `json:"lease"`
	LeaseHolder       string                   `json:"lease_holder,omitempty"`
	LeaseStolen       bool                     `json:"lease_stolen,omitempty"`
	RunAttempts       []closePendingRunAttempt `json:"run_attempts,omitempty"`
	Git               closePendingGit          `json:"git,omitempty"`
}

func (s *Store) closePendingPath(workID string) string {
	return s.workPath(workID) + ".close-pending"
}

func closeRecoveryRequired(format string, args ...any) error {
	return fmt.Errorf("close recovery_required: "+format, args...)
}

func closePendingSHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// closePendingFrameSHA256 hashes a recursively canonical JSON representation
// with the digest field normalized to the empty string. The digest therefore
// covers every other frame field without self-hash ambiguity.
func closePendingFrameSHA256(pending closePending) (string, error) {
	pending.FrameSHA256 = ""
	body, err := json.Marshal(&pending)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return "", err
	}
	canonical, err := canonicalCloseJSON(normalized)
	if err != nil {
		return "", err
	}
	return closePendingSHA256(canonical), nil
}

func sealClosePending(pending *closePending) error {
	if pending == nil {
		return errors.New("close pending frame is nil")
	}
	digest, err := closePendingFrameSHA256(*pending)
	if err != nil {
		return err
	}
	pending.FrameSHA256 = digest
	return nil
}

func validClosePendingSHA256(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func canonicalCloseJSON(value any) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func sameCanonicalCloseJSON(left, right any) bool {
	leftBody, leftErr := json.Marshal(left)
	rightBody, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBody, rightBody)
}

func decodeStrictCloseJSON(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func (s *Store) loadClosePending(workID string) (closePending, bool, error) {
	var pending closePending
	body, err := home.ReadRootedRegularFile(s.home, s.closePendingPath(workID), "work close pending")
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, nil
	}
	if err != nil {
		return pending, false, closeRecoveryRequired("read work %s frame: %w", workID, err)
	}
	if err := decodeStrictCloseJSON(body, &pending); err != nil {
		return pending, false, closeRecoveryRequired("decode work %s frame: %w", workID, err)
	}
	if err := s.validateClosePending(pending); err != nil {
		return pending, false, closeRecoveryRequired("validate work %s frame: %w", workID, err)
	}
	if pending.WorkID != workID {
		return pending, false, closeRecoveryRequired("frame identity %s differs from requested work %s", pending.WorkID, workID)
	}
	return pending, true, nil
}

func (s *Store) fenceClosePendingMutation(workID string) error {
	pending, present, err := s.loadClosePending(workID)
	if err != nil {
		return err
	}
	if present {
		return closeRecoveryRequired("work %s has frozen close %s/%s; retry the exact work close before another lifecycle mutation", workID, pending.Action.ID, pending.Decision.ID)
	}
	return nil
}

func (s *Store) validateClosePending(p closePending) error {
	frameSHA, err := closePendingFrameSHA256(p)
	if err != nil {
		return fmt.Errorf("frame checksum: %w", err)
	}
	if !validClosePendingSHA256(p.FrameSHA256) || p.FrameSHA256 != frameSHA {
		return errors.New("frame_sha256 does not seal the canonical full frame")
	}
	if p.SchemaVersion != closePendingSchemaVersion {
		return fmt.Errorf("schema_version must be %q", closePendingSchemaVersion)
	}
	if strings.TrimSpace(p.WorkID) == "" || p.SourceWork.ID != p.WorkID || p.FinalWork.ID != p.WorkID {
		return errors.New("work identity mismatch")
	}
	if !validClosePendingSHA256(p.InputSHA256) {
		return errors.New("input_sha256 must be a sha256 reference")
	}
	completedAt, err := time.Parse(time.RFC3339Nano, p.CompletedAt)
	if err != nil || completedAt.IsZero() {
		return errors.New("completed_at must be an RFC3339 timestamp")
	}
	eventAt, err := parseLifecycleTS(p.Event.TS)
	if p.CompletedAt != p.Receipt.CreatedAt || err != nil || !eventAt.Equal(completedAt) {
		return errors.New("completed_at must equal Receipt.created_at and closing event timestamp")
	}
	if err := p.SourceWork.Validate(); err != nil {
		return fmt.Errorf("source WorkEnvelope invalid: %w", err)
	}
	if err := validateFrozenCloseNonSourceEvidence(p); err != nil {
		return err
	}
	if err := p.FinalWork.Validate(); err != nil {
		return fmt.Errorf("final WorkEnvelope invalid: %w", err)
	}
	sourceSHA, err := workEnvelopeSHA256(p.SourceWork)
	if err != nil || sourceSHA != p.SourceWorkSHA256 {
		return errors.New("source WorkEnvelope digest mismatch")
	}
	finalSHA, err := workEnvelopeSHA256(p.FinalWork)
	if err != nil || finalSHA != p.FinalWorkSHA256 {
		return errors.New("final WorkEnvelope digest mismatch")
	}
	if err := p.Action.Validate(); err != nil || p.Action.IDs.WorkID != p.WorkID {
		return errors.New("close ActionIntent invalid or work-mismatched")
	}
	if err := p.Decision.Validate(); err != nil || p.Decision.IDs.WorkID != p.WorkID || p.Decision.ActionIntentID != p.Action.ID || !policy.Allowed(p.Decision) {
		return errors.New("close PolicyDecision invalid, denied, or action-mismatched")
	}
	if err := p.Receipt.Validate(); err != nil || p.Receipt.IDs.WorkID != p.WorkID {
		return errors.New("close Receipt invalid or work-mismatched")
	}
	if err := p.Transition.Validate(); err != nil || p.Transition.IDs.WorkID != p.WorkID {
		return errors.New("close Transition invalid or work-mismatched")
	}
	if err := s.validateClosePendingAuthority(p); err != nil {
		return err
	}
	if p.FinalWork.State != p.Event.To || p.SourceWork.State != p.Event.From ||
		p.Event.WorkID != p.WorkID || p.Event.PreviousWorkSHA256 != p.SourceWorkSHA256 ||
		p.Event.WorkSHA256 != p.FinalWorkSHA256 ||
		!reflect.DeepEqual(p.Event.Attached, []string{p.Action.ID, p.Decision.ID, p.Receipt.ID, p.Transition.ID}) {
		return errors.New("closing lifecycle event does not bind the frozen generation")
	}
	if latestActionIntentID(p.FinalWork) != p.Action.ID || p.FinalWork.PolicyDecisionID != p.Decision.ID || p.FinalWork.IDs.TransitionID != p.Transition.ID {
		return errors.New("final WorkEnvelope does not bind the frozen close generation")
	}
	receiptSHA, err := receiptEnvelopeSHA256(p.Receipt)
	if err != nil || receiptSHA != p.Event.ReceiptSHA256 {
		return errors.New("closing lifecycle event Receipt digest mismatch")
	}
	if p.Lease.Present {
		if len(p.Lease.Body) == 0 || p.Lease.SHA256 != closePendingSHA256(p.Lease.Body) {
			return errors.New("captured lease body or digest invalid")
		}
		var lease Lease
		if err := decodeStrictCloseJSON(p.Lease.Body, &lease); err != nil || lease.WorkID != p.WorkID || strings.TrimSpace(lease.By) == "" {
			return errors.New("captured lease invalid or work-mismatched")
		}
		if strings.TrimSpace(p.LeaseHolder) != lease.By {
			return errors.New("captured lease holder mismatch")
		}
	} else if len(p.Lease.Body) != 0 || p.Lease.SHA256 != "" || p.LeaseHolder != "" || p.LeaseStolen {
		return errors.New("absent lease carries authority data")
	}
	if err := s.validateClosePendingGit(p); err != nil {
		return err
	}
	return validateClosePendingRunAttempts(p, completedAt)
}

func closePendingAuthorityFromInput(in CloseInput) closePendingAuthority {
	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		actor = "codex"
	}
	authority := closePendingAuthority{
		Actor:                         actor,
		Outcome:                       in.Outcome,
		MoveType:                      in.MoveType,
		Steal:                         in.Steal,
		StealReason:                   strings.TrimSpace(in.StealReason),
		SkipRunAttemptTerminalization: in.SkipRunAttemptTerminalization,
	}
	for _, request := range in.RunAttemptTerminalizations {
		request.RunAttemptID = strings.TrimSpace(request.RunAttemptID)
		request.Owner = strings.TrimSpace(request.Owner)
		request.TargetStatus = strings.TrimSpace(request.TargetStatus)
		request.Reason = strings.TrimSpace(request.Reason)
		authority.RunAttemptTerminalizations = append(authority.RunAttemptTerminalizations, request)
	}
	return authority
}

func closePendingRequests(authority closePendingAuthority) (map[string]RunAttemptTerminalizationRequest, error) {
	if authority.SkipRunAttemptTerminalization && len(authority.RunAttemptTerminalizations) == 0 {
		return nil, errors.New("frozen skip_run_attempt_terminalization lacks explicit requests")
	}
	requests := make(map[string]RunAttemptTerminalizationRequest, len(authority.RunAttemptTerminalizations))
	for _, request := range authority.RunAttemptTerminalizations {
		if request.RunAttemptID != strings.TrimSpace(request.RunAttemptID) ||
			request.Owner != strings.TrimSpace(request.Owner) ||
			request.TargetStatus != strings.TrimSpace(request.TargetStatus) ||
			request.Reason != strings.TrimSpace(request.Reason) {
			return nil, errors.New("frozen RunAttempt terminalization request is not normalized")
		}
		if !runattempt.Valid(request.RunAttemptID) || request.Owner == "" || !runattempt.IsTerminal(request.TargetStatus) {
			return nil, fmt.Errorf("frozen RunAttempt terminalization request %q is invalid", request.RunAttemptID)
		}
		if request.Owner == runattempt.TerminalizationOwnerWorkClose || request.Owner == runattempt.TerminalizationOwnerWorkSupersede {
			return nil, fmt.Errorf("frozen caller request %s claims work-owned terminalization", request.RunAttemptID)
		}
		if request.Owner == runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned &&
			request.TargetStatus != runattempt.StatusAbandoned {
			return nil, fmt.Errorf("frozen acknowledge-abandoned request %s targets %s", request.RunAttemptID, request.TargetStatus)
		}
		if _, duplicate := requests[request.RunAttemptID]; duplicate {
			return nil, fmt.Errorf("frozen RunAttempt terminalization duplicates %s", request.RunAttemptID)
		}
		requests[request.RunAttemptID] = request
	}
	return requests, nil
}

func (s *Store) validateClosePendingAuthority(p closePending) error {
	authority := p.Authority
	if authority.Actor == "" || authority.Actor != strings.TrimSpace(authority.Actor) ||
		authority.StealReason != strings.TrimSpace(authority.StealReason) {
		return errors.New("frozen close authority is not normalized")
	}
	if !terminalStates[authority.Outcome] || !oneOf(authority.MoveType, envelope.MoveTypes) {
		return errors.New("frozen close authority outcome or move_type is invalid")
	}
	if p.FinalWork.State != authority.Outcome || p.Receipt.Outcome != receiptOutcome[authority.Outcome] ||
		p.Receipt.MoveType != authority.MoveType {
		return errors.New("frozen close authority does not bind final WorkEnvelope and Receipt")
	}
	if p.Action.Actor != authority.Actor || p.Action.Capability != "work.close" ||
		p.Action.Action != "close_work_as_"+authority.Outcome ||
		p.Action.Target.Kind != "work" || p.Action.Target.ID != p.WorkID ||
		p.Action.Target.Path != s.workPath(p.WorkID) ||
		p.Action.MutationClass != "local_reversible" || p.Action.Reversibility != "manual" ||
		p.Action.BlastRadius != "local" {
		return errors.New("close ActionIntent does not match frozen normalized input")
	}
	if p.Action.CreatedAt != p.CompletedAt || p.Decision.CreatedAt != p.CompletedAt ||
		p.Transition.CreatedAt != p.CompletedAt {
		return errors.New("close authority envelope timestamps differ from completed_at")
	}
	if p.Action.IDs.ContextPackID != p.SourceWork.IDs.ContextPackID ||
		p.Decision.IDs.ContextPackID != p.SourceWork.IDs.ContextPackID ||
		p.Receipt.IDs.ContextPackID != p.SourceWork.IDs.ContextPackID ||
		p.Transition.IDs.ContextPackID != p.SourceWork.IDs.ContextPackID {
		return errors.New("close authority context-pack binding mismatch")
	}
	actionLeaseHolder := ""
	if p.Lease.Present && p.LeaseHolder != authority.Actor {
		actionLeaseHolder = p.LeaseHolder
	}
	preview := fmt.Sprintf("close work %s from %s to %s move_type %s actor %s",
		p.WorkID, p.SourceWork.State, authority.Outcome, authority.MoveType, authority.Actor)
	if authority.Steal {
		preview += fmt.Sprintf(" lease_override_from %q reason %q", actionLeaseHolder, authority.StealReason)
	}
	if p.Action.Preview != preview {
		return errors.New("close ActionIntent preview does not match frozen normalized input")
	}
	identityParts := []string{
		"work.close", "close_work_as_" + authority.Outcome, "work", p.WorkID,
		s.workPath(p.WorkID), "local_reversible", "manual", "local",
		authority.Actor, authority.Outcome, authority.MoveType,
		fmt.Sprintf("%t", authority.Steal), actionLeaseHolder, authority.StealReason, p.Event.TS,
	}
	expectedActionID := closeActionIntentIDFor(identityParts...)
	legacyActionID := idFor("act_", identityParts...)
	if p.Action.ID != expectedActionID && p.Action.ID != legacyActionID {
		return errors.New("close ActionIntent identity does not derive from frozen normalized input")
	}
	expectedStolen := p.Lease.Present && authority.Steal && p.LeaseHolder != authority.Actor
	if p.LeaseStolen != expectedStolen {
		return errors.New("lease_stolen does not derive from frozen close authority")
	}
	eventBy := ""
	eventNote := fmt.Sprintf("closed: %s (policy %s)", authority.MoveType, p.Decision.Outcome)
	if expectedStolen {
		if authority.StealReason == "" {
			return errors.New("stolen close authority requires a reason")
		}
		eventBy = authority.Actor
		eventNote = fmt.Sprintf("lease stolen from %q: %s; %s",
			p.LeaseHolder, authority.StealReason, eventNote)
	}
	if p.Event.By != eventBy || p.Event.Note != eventNote {
		return errors.New("closing lifecycle event does not derive from frozen close authority")
	}
	return nil
}

func closePendingGitFromProof(
	proof *LandCheckReport,
	repositoryPath string,
	expectations []gitReferenceExpectation,
) closePendingGit {
	if proof == nil {
		return closePendingGit{}
	}
	proofCopy := *proof
	return closePendingGit{
		RepositoryPath: repositoryPath,
		Expectations:   append([]gitReferenceExpectation(nil), expectations...),
		Proof:          &proofCopy,
	}
}

func (s *Store) validateClosePendingGit(p closePending) error {
	if p.Git.Proof == nil {
		if p.Git.RepositoryPath != "" || len(p.Git.Expectations) != 0 {
			return errors.New("Git expectations lack their frozen landing proof")
		}
		return nil
	}
	proof := *p.Git.Proof
	if proof.Status != "landed" {
		return errors.New("frozen Git proof is not landed")
	}
	if err := validateRecordedLandCheck(p.SourceWork, proof.EvidenceRef, proof.ID, proof.ProofPath, proof); err != nil {
		return fmt.Errorf("frozen Git proof invalid: %w", err)
	}
	proofCited := false
	for _, ref := range p.Receipt.ProofRefs {
		if ref == proof.EvidenceRef {
			proofCited = true
			break
		}
	}
	if !proofCited {
		return errors.New("frozen Git proof is not cited by the closing Receipt")
	}
	if err := s.validateBoundGitDelivery(p.SourceWork); err != nil {
		return fmt.Errorf("frozen Git delivery identity invalid: %w", err)
	}
	wantRefs := []string{proof.TargetBranchRef}
	if proof.PublicationMode == "required" {
		wantRefs = append(wantRefs, proof.PublicationRef)
	}
	if p.Git.RepositoryPath != proof.RepoPath || len(p.Git.Expectations) != len(wantRefs) {
		return errors.New("Git repository/ref expectations do not derive from the frozen landing proof")
	}
	for i, expectation := range p.Git.Expectations {
		if expectation.Ref != wantRefs[i] || !validLandingObjectID(expectation.SHA) {
			return errors.New("Git repository/ref/SHA expectations do not derive from the frozen landing proof")
		}
		currentSHA, err := runLandingGitOutput(
			p.Git.RepositoryPath,
			"rev-parse", "--verify", expectation.Ref+"^{commit}",
		)
		if err != nil {
			return fmt.Errorf("resolve frozen Git expectation %s: %w", expectation.Ref, err)
		}
		if currentSHA != expectation.SHA {
			return fmt.Errorf("Git expectation %s no longer matches frozen SHA %s", expectation.Ref, expectation.SHA)
		}
		containsCandidate, err := landingIsAncestor(
			p.Git.RepositoryPath,
			proof.CandidateTipSHA,
			expectation.SHA,
		)
		if err != nil {
			return fmt.Errorf("validate frozen Git expectation %s: %w", expectation.Ref, err)
		}
		if !containsCandidate {
			return fmt.Errorf("Git expectation %s does not contain frozen candidate %s", expectation.Ref, proof.CandidateTipSHA)
		}
	}
	return nil
}

func validateClosePendingRunAttempts(p closePending, completedAt time.Time) error {
	requests, err := closePendingRequests(p.Authority)
	if err != nil {
		return err
	}
	finalIDs := make(map[string]int, len(p.FinalWork.RunAttemptIDs))
	for _, id := range p.FinalWork.RunAttemptIDs {
		finalIDs[id]++
	}
	digests := map[string]string{}
	for _, binding := range p.Event.RunAttemptTerminalizationDigests {
		if digests[binding.RunAttemptID] != "" {
			return fmt.Errorf("duplicate RunAttempt terminalization digest for %s", binding.RunAttemptID)
		}
		if finalIDs[binding.RunAttemptID] != 1 {
			return fmt.Errorf("RunAttempt terminalization digest %s is not uniquely bound to final WorkEnvelope", binding.RunAttemptID)
		}
		digests[binding.RunAttemptID] = binding.IntentSHA256
	}
	seen := map[string]bool{}
	for _, frozen := range p.RunAttempts {
		if seen[frozen.Attempt.ID] {
			return fmt.Errorf("duplicate frozen RunAttempt %s", frozen.Attempt.ID)
		}
		seen[frozen.Attempt.ID] = true
		if finalIDs[frozen.Attempt.ID] != 1 {
			return fmt.Errorf("frozen RunAttempt %s is not uniquely bound to final WorkEnvelope", frozen.Attempt.ID)
		}
		if err := validateRunAttemptRecord(frozen.Attempt.ID, p.FinalWork, frozen.Attempt); err != nil ||
			runattempt.IsTerminal(frozen.Attempt.Status) || frozen.ExpectedStatus != frozen.Attempt.Status {
			return fmt.Errorf("frozen RunAttempt %s snapshot identity/status invalid", frozen.Attempt.ID)
		}
		if err := frozen.Intent.Validate(); err != nil || frozen.Intent.WorkID != p.WorkID {
			return fmt.Errorf("frozen RunAttempt %s invalid or work-mismatched", frozen.Attempt.ID)
		}
		if frozen.Attempt.ActionIntentID != frozen.Intent.RunAttemptActionIntentID {
			return fmt.Errorf("frozen RunAttempt %s action binding mismatch", frozen.Attempt.ID)
		}
		attemptPolicyID := ""
		if frozen.Attempt.ExecutionPosture != nil {
			attemptPolicyID = frozen.Attempt.ExecutionPosture.PolicyDecisionID
		}
		if attemptPolicyID != frozen.Intent.RunAttemptPolicyDecisionID {
			return fmt.Errorf("frozen RunAttempt %s policy binding mismatch", frozen.Attempt.ID)
		}
		if frozen.Attempt.TerminalizationIntent != nil &&
			!reflect.DeepEqual(*frozen.Attempt.TerminalizationIntent, frozen.Intent) {
			return fmt.Errorf("frozen RunAttempt %s prepared intent differs from frame intent", frozen.Attempt.ID)
		}
		if frozen.Intent.ExpectedWorkState != p.SourceWork.State ||
			frozen.Intent.ExpectedWorkSHA256 != p.SourceWorkSHA256 ||
			frozen.Intent.ExpectedActionIntentID != latestActionIntentID(p.SourceWork) ||
			frozen.Intent.ExpectedPolicyDecisionID != p.SourceWork.PolicyDecisionID ||
			frozen.Intent.CloseInputSHA256 != p.InputSHA256 ||
			frozen.Intent.CloseActionIntentID != p.Action.ID ||
			frozen.Intent.ClosePolicyDecisionID != p.Decision.ID ||
			frozen.Intent.Outcome != p.Authority.Outcome ||
			frozen.Intent.ReceiptID != p.Receipt.ID ||
			frozen.Intent.TransitionID != p.Transition.ID ||
			!frozen.Intent.PreparedAt.Equal(completedAt) {
			return fmt.Errorf("frozen RunAttempt %s intent does not bind close generation", frozen.Attempt.ID)
		}
		request, callerOwns := requests[frozen.Attempt.ID]
		if frozen.CallerOwns != callerOwns {
			return fmt.Errorf("frozen RunAttempt %s caller ownership does not derive from close input", frozen.Attempt.ID)
		}
		if callerOwns {
			if !p.Authority.SkipRunAttemptTerminalization ||
				frozen.Intent.Owner != request.Owner ||
				frozen.Intent.TargetStatus != request.TargetStatus ||
				frozen.Intent.TerminalizationReason != request.Reason {
				return fmt.Errorf("frozen RunAttempt %s caller-owned intent differs from reservation", frozen.Attempt.ID)
			}
		} else {
			switch frozen.Intent.Owner {
			case runattempt.TerminalizationOwnerWorkClose:
				if frozen.Intent.TargetStatus != runAttemptStatusForCloseOutcome(p.Authority.Outcome) ||
					frozen.Intent.TerminalizationReason != "" {
					return fmt.Errorf("frozen RunAttempt %s work-close target differs from outcome", frozen.Attempt.ID)
				}
			case runattempt.TerminalizationOwnerWorkSupersede:
				if frozen.Intent.TargetStatus != runattempt.StatusSuperseded ||
					frozen.Attempt.Origin != runattempt.OriginClaimReservation ||
					!strings.HasPrefix(frozen.Intent.TerminalizationReason, "superseded by ") {
					return fmt.Errorf("frozen RunAttempt %s supersede intent lacks snapshot preconditions", frozen.Attempt.ID)
				}
			default:
				return fmt.Errorf("frozen RunAttempt %s has unreserved terminalization owner %s", frozen.Attempt.ID, frozen.Intent.Owner)
			}
		}
		digest, err := terminalizationIntentSHA256(frozen.Intent)
		if err != nil || digest != digests[frozen.Attempt.ID] {
			return fmt.Errorf("frozen RunAttempt %s terminalization digest mismatch", frozen.Attempt.ID)
		}
		delete(digests, frozen.Attempt.ID)
	}
	if len(digests) != 0 {
		return errors.New("closing lifecycle event has an unowned RunAttempt terminalization digest")
	}
	return nil
}

func (s *Store) captureClosePendingLease(workID string) (closePendingLease, Lease, error) {
	body, err := home.ReadRootedRegularFile(s.home, s.leasePath(workID), "work close lease")
	if errors.Is(err, os.ErrNotExist) {
		return closePendingLease{}, Lease{}, nil
	}
	if err != nil {
		return closePendingLease{}, Lease{}, err
	}
	var lease Lease
	if err := decodeStrictCloseJSON(body, &lease); err != nil {
		return closePendingLease{}, Lease{}, fmt.Errorf("decode work %s lease: %w", workID, err)
	}
	if lease.WorkID != workID || strings.TrimSpace(lease.By) == "" {
		return closePendingLease{}, Lease{}, fmt.Errorf("work %s lease identity invalid", workID)
	}
	return closePendingLease{Present: true, Body: append([]byte(nil), body...), SHA256: closePendingSHA256(body)}, lease, nil
}

func (s *Store) newClosePending(
	sourceWork, finalWork envelope.WorkEnvelope,
	inputSHA string,
	input CloseInput,
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
	receipt envelope.Receipt,
	transition envelope.Transition,
	event Event,
	lease closePendingLease,
	leaseHolder string,
	leaseStolen bool,
	plans []runAttemptTerminalizationPlan,
	landingProof *LandCheckReport,
	repositoryPath string,
	expectations []gitReferenceExpectation,
) (closePending, error) {
	sourceSHA, err := workEnvelopeSHA256(sourceWork)
	if err != nil {
		return closePending{}, err
	}
	finalSHA, err := workEnvelopeSHA256(finalWork)
	if err != nil {
		return closePending{}, err
	}
	pending := closePending{
		SchemaVersion:    closePendingSchemaVersion,
		WorkID:           sourceWork.ID,
		InputSHA256:      inputSHA,
		Authority:        closePendingAuthorityFromInput(input),
		CompletedAt:      receipt.CreatedAt,
		SourceWork:       sourceWork,
		SourceWorkSHA256: sourceSHA,
		FinalWork:        finalWork,
		FinalWorkSHA256:  finalSHA,
		Action:           action,
		Decision:         decision,
		Receipt:          receipt,
		Transition:       transition,
		Event:            event,
		Lease:            lease,
		LeaseHolder:      leaseHolder,
		LeaseStolen:      leaseStolen,
		Git:              closePendingGitFromProof(landingProof, repositoryPath, expectations),
	}
	for _, plan := range plans {
		pending.RunAttempts = append(pending.RunAttempts, closePendingRunAttempt{
			Attempt: plan.Attempt, ExpectedStatus: plan.Attempt.Status, Intent: plan.Intent, CallerOwns: plan.CallerOwns,
		})
	}
	if err := s.captureCloseNonSourceEvidence(&pending); err != nil {
		return closePending{}, err
	}
	if err := sealClosePending(&pending); err != nil {
		return closePending{}, err
	}
	if err := s.validateClosePending(pending); err != nil {
		return closePending{}, err
	}
	return pending, nil
}

func (s *Store) persistClosePending(pending closePending) error {
	path := s.closePendingPath(pending.WorkID)
	staged, err := s.stageJSON(path, &pending)
	if err != nil {
		return err
	}
	if err := commitStagedJSONExclusive(staged, path); err != nil {
		_ = os.Remove(staged)
		return err
	}
	_, present, err := s.loadClosePending(pending.WorkID)
	if err != nil {
		return err
	}
	expectedBody, err := canonicalCloseJSON(&pending)
	if err != nil {
		return err
	}
	actualBody, err := home.ReadRootedRegularFile(s.home, path, "work close pending")
	if err != nil {
		return err
	}
	if !present || !bytes.Equal(actualBody, expectedBody) {
		return closeRecoveryRequired("persisted frame for work %s differs from the frozen transaction", pending.WorkID)
	}
	return nil
}

func plansFromClosePending(pending closePending) []runAttemptTerminalizationPlan {
	plans := make([]runAttemptTerminalizationPlan, 0, len(pending.RunAttempts))
	for _, frozen := range pending.RunAttempts {
		plans = append(plans, runAttemptTerminalizationPlan{Attempt: frozen.Attempt, Intent: frozen.Intent, CallerOwns: frozen.CallerOwns})
	}
	return plans
}

func (s *Store) verifyClosePendingLease(pending closePending, allowReleased bool) error {
	body, err := home.ReadRootedRegularFile(s.home, s.leasePath(pending.WorkID), "work close lease")
	if errors.Is(err, os.ErrNotExist) {
		if !pending.Lease.Present || allowReleased {
			return nil
		}
		return closeRecoveryRequired("work %s captured lease disappeared before terminal publication", pending.WorkID)
	}
	if err != nil {
		return closeRecoveryRequired("read work %s captured lease: %w", pending.WorkID, err)
	}
	if !pending.Lease.Present || !bytes.Equal(body, pending.Lease.Body) || closePendingSHA256(body) != pending.Lease.SHA256 {
		return closeRecoveryRequired("work %s lease differs from the frozen whole-file snapshot", pending.WorkID)
	}
	return nil
}

func (s *Store) removeClosePendingLease(pending closePending) error {
	if !pending.Lease.Present {
		return s.verifyClosePendingLease(pending, false)
	}
	_, err := home.ReadRootedRegularFile(s.home, s.leasePath(pending.WorkID), "work close lease")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return home.RemoveRootedRegularFileDurableValidated(s.home, s.leasePath(pending.WorkID), "work close lease", func(body []byte) error {
		if !bytes.Equal(body, pending.Lease.Body) || closePendingSHA256(body) != pending.Lease.SHA256 {
			return closeRecoveryRequired("work %s lease changed before release", pending.WorkID)
		}
		return nil
	})
}

func (s *Store) persistClosePendingEnvelopes(pending closePending) error {
	for _, item := range []struct {
		id    string
		value any
	}{
		{pending.Action.ID, &pending.Action},
		{pending.Decision.ID, &pending.Decision},
		{pending.Receipt.ID, &pending.Receipt},
		{pending.Transition.ID, &pending.Transition},
	} {
		if err := s.writeEventJSON(item.id, item.value); err != nil {
			return err
		}
		if item.id == pending.Action.ID {
			closeAfterFirstEnvelopeWrite()
		}
	}
	return nil
}

func (s *Store) publishClosePendingEventAndWork(pending closePending) error {
	err := s.withWorkEventLock(func() error {
		current, err := s.Get(pending.WorkID)
		if err != nil {
			return err
		}
		currentSHA, err := workEnvelopeSHA256(current)
		if err != nil {
			return err
		}
		events, err := s.readEventsStrictUnlocked(pending.WorkID)
		if err != nil {
			return err
		}
		exact := 0
		for _, event := range events {
			if sameCanonicalCloseJSON(event, pending.Event) {
				exact++
				continue
			}
			if event.WorkSHA256 == pending.FinalWorkSHA256 || event.PreviousWorkSHA256 == pending.SourceWorkSHA256 {
				return closeRecoveryRequired("work %s has a competing lifecycle event for the frozen close generation", pending.WorkID)
			}
		}
		if exact > 1 {
			return closeRecoveryRequired("work %s has %d copies of the frozen close event", pending.WorkID, exact)
		}
		publishWork := func() error {
			latest, err := s.Get(pending.WorkID)
			if err != nil {
				return err
			}
			latestSHA, err := workEnvelopeSHA256(latest)
			if err != nil {
				return err
			}
			if latestSHA != pending.SourceWorkSHA256 && latestSHA != pending.FinalWorkSHA256 {
				return closeRecoveryRequired("work %s changed before terminal WorkEnvelope publication", pending.WorkID)
			}
			if latestSHA == pending.FinalWorkSHA256 {
				return nil
			}
			return s.publishClaimJSON(s.workPath(pending.WorkID), &pending.FinalWork)
		}
		switch currentSHA {
		case pending.SourceWorkSHA256:
			if exact == 0 {
				if err := s.appendEventControlledUnlocked(pending.Event, nil, publishWork); err != nil {
					return err
				}
			} else if err := publishWork(); err != nil {
				return err
			}
		case pending.FinalWorkSHA256:
			if exact != 1 {
				return closeRecoveryRequired("terminal WorkEnvelope for %s lacks its one exact lifecycle event", pending.WorkID)
			}
		default:
			return closeRecoveryRequired("work %s matches neither frozen source nor final WorkEnvelope", pending.WorkID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	stored, err := s.Get(pending.WorkID)
	if err != nil {
		return closeRecoveryRequired("read work %s terminal WorkEnvelope: %v", pending.WorkID, err)
	}
	storedSHA, err := workEnvelopeSHA256(stored)
	if err != nil || storedSHA != pending.FinalWorkSHA256 {
		return closeRecoveryRequired("work %s terminal WorkEnvelope verification failed", pending.WorkID)
	}
	events, err := s.eventsFor(pending.WorkID)
	if err != nil {
		return err
	}
	exact := 0
	for _, event := range events {
		if sameCanonicalCloseJSON(event, pending.Event) {
			exact++
		}
	}
	if exact != 1 {
		return closeRecoveryRequired("work %s exact close event count is %d, want one", pending.WorkID, exact)
	}
	return nil
}

// closePendingAuthorityStarted proves whether the frozen close has crossed any
// authoritative boundary other than writing the recovery frame itself. It is
// deliberately conservative: uncertainty retains the frame for exact recovery.
func (s *Store) closePendingAuthorityStarted(pending closePending) (bool, error) {
	current, err := s.Get(pending.WorkID)
	if err != nil {
		return true, err
	}
	currentSHA, err := workEnvelopeSHA256(current)
	if err != nil {
		return true, err
	}
	if currentSHA != pending.SourceWorkSHA256 {
		return true, nil
	}
	events, err := s.eventsFor(pending.WorkID)
	if err != nil {
		return true, err
	}
	for _, event := range events {
		if sameCanonicalCloseJSON(event, pending.Event) || event.WorkSHA256 == pending.FinalWorkSHA256 {
			return true, nil
		}
	}
	for _, id := range []string{pending.Action.ID, pending.Decision.ID, pending.Receipt.ID, pending.Transition.ID} {
		var record json.RawMessage
		if err := s.readEventJSON(id, &record); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return true, err
		}
		return true, nil
	}
	store := s.runAttempts()
	for _, frozen := range pending.RunAttempts {
		currentAttempt, err := store.Get(frozen.Attempt.ID)
		if err != nil {
			return true, err
		}
		if currentAttempt.TerminalizationIntent != nil || currentAttempt.Status != frozen.Attempt.Status ||
			currentAttempt.TerminalizationOwner != frozen.Attempt.TerminalizationOwner ||
			currentAttempt.TerminalizationReceiptID != frozen.Attempt.TerminalizationReceiptID ||
			currentAttempt.TerminalizationTransitionID != frozen.Attempt.TerminalizationTransitionID {
			return true, nil
		}
	}
	return false, nil
}

// refreshClosePendingAfterSweptClaim repairs one narrow pre-authority race:
// a stale sweep may terminalize the frozen claim reservation after the close
// frame is durable but before work.close prepares that reservation for
// supersession. The swept record is already authoritative evidence and is
// never rewritten. Instead, an exact retry may remove only that obsolete plan
// and its digest from the still-unpublished close frame.
//
// This recovery is intentionally limited to the supported governed failure
// path. Any close-owned terminalization intent, proof object, lifecycle event,
// lease/work drift, second changed plan, or noncanonical sweep remains
// recovery_required under the ordinary close path.
func (s *Store) refreshClosePendingAfterSweptClaim(pending closePending) (closePending, error) {
	if pending.Authority.Outcome != "failed" || pending.Authority.MoveType != "audit_only" || len(pending.RunAttempts) == 0 {
		return pending, nil
	}
	if err := s.verifyClosePendingLease(pending, false); err != nil {
		return pending, err
	}

	currentWork, err := s.Get(pending.WorkID)
	if err != nil {
		return pending, err
	}
	currentWorkSHA, err := workEnvelopeSHA256(currentWork)
	if err != nil {
		return pending, err
	}
	if currentWorkSHA != pending.SourceWorkSHA256 {
		return pending, nil
	}

	events, err := s.eventsFor(pending.WorkID)
	if err != nil {
		return pending, err
	}
	for _, event := range events {
		if sameCanonicalCloseJSON(event, pending.Event) || event.WorkSHA256 == pending.FinalWorkSHA256 ||
			(event.PreviousWorkSHA256 == pending.SourceWorkSHA256 && terminalStates[event.To]) {
			return pending, nil
		}
	}
	for _, id := range []string{pending.Action.ID, pending.Decision.ID, pending.Receipt.ID, pending.Transition.ID} {
		var record json.RawMessage
		if err := s.readEventJSON(id, &record); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return pending, err
		}
		return pending, nil
	}

	store := s.runAttempts()
	currentAttempts := make(map[string]runattempt.RunAttempt, len(pending.FinalWork.RunAttemptIDs))
	for _, id := range dedupeStrings(pending.FinalWork.RunAttemptIDs) {
		attempt, err := store.Get(id)
		if err != nil {
			return pending, err
		}
		if err := validateRunAttemptRecord(id, pending.FinalWork, attempt); err != nil {
			return pending, err
		}
		currentAttempts[id] = attempt
	}
	history, err := buildAttachedRunAttemptHistory(pending.FinalWork.RunAttemptIDs, currentAttempts)
	if err != nil {
		return pending, err
	}

	changedIndex := -1
	changedID := ""
	for index, frozen := range pending.RunAttempts {
		current, ok := currentAttempts[frozen.Attempt.ID]
		if !ok {
			return pending, nil
		}
		if frozen.Attempt.TerminalizationIntent != nil ||
			frozen.Attempt.TerminalizationOwner != "" ||
			frozen.Attempt.TerminalizationReceiptID != "" ||
			frozen.Attempt.TerminalizationTransitionID != "" ||
			frozen.Attempt.TerminalizationReason != "" ||
			current.TerminalizationIntent != nil ||
			current.TerminalizationOwner != "" ||
			current.TerminalizationReceiptID != "" ||
			current.TerminalizationTransitionID != "" ||
			current.TerminalizationReason != "" {
			return pending, nil
		}
		if reflect.DeepEqual(current, frozen.Attempt) {
			continue
		}
		if changedIndex != -1 || !exactSweptClaimDelta(frozen, current) {
			return pending, nil
		}
		changedIndex = index
		changedID = frozen.Attempt.ID
	}
	if changedIndex == -1 {
		return pending, nil
	}

	replacementOwnsFailure := false
	for _, replacementID := range history.ReplacedClaimReservations[changedID] {
		replacement := currentAttempts[replacementID]
		if replacement.Origin != runattempt.OriginHarnessExecution ||
			replacement.Status != runattempt.StatusFailed ||
			!runAttemptOwnsHarnessOutcome(replacement) {
			continue
		}
		if _, err := s.resolveRunAttemptAuthorityFromEvents(pending.SourceWork, replacement, events); err != nil {
			continue
		}
		replacementOwnsFailure = true
		break
	}
	if !replacementOwnsFailure {
		return pending, nil
	}

	rebuilt := pending
	rebuilt.RunAttempts = append([]closePendingRunAttempt(nil), pending.RunAttempts[:changedIndex]...)
	rebuilt.RunAttempts = append(rebuilt.RunAttempts, pending.RunAttempts[changedIndex+1:]...)
	rebuilt.Event.RunAttemptTerminalizationDigests = nil
	removedDigests := 0
	for _, digest := range pending.Event.RunAttemptTerminalizationDigests {
		if digest.RunAttemptID == changedID {
			removedDigests++
			continue
		}
		rebuilt.Event.RunAttemptTerminalizationDigests = append(rebuilt.Event.RunAttemptTerminalizationDigests, digest)
	}
	if removedDigests != 1 {
		return pending, nil
	}
	if err := sealClosePending(&rebuilt); err != nil {
		return pending, err
	}
	if err := s.validateClosePending(rebuilt); err != nil {
		return pending, err
	}
	if err := s.replaceClosePendingExact(pending, rebuilt); err != nil {
		return pending, closeRecoveryRequired("refresh swept claim in work %s close frame: %v", pending.WorkID, err)
	}
	return rebuilt, nil
}

func exactSweptClaimDelta(frozen closePendingRunAttempt, current runattempt.RunAttempt) bool {
	if frozen.Attempt.Origin != runattempt.OriginClaimReservation ||
		runattempt.IsTerminal(frozen.Attempt.Status) ||
		frozen.ExpectedStatus != frozen.Attempt.Status ||
		!runAttemptSweptSideEffectFreeReservation(current) {
		return false
	}
	normalized := current
	normalized.Status = frozen.Attempt.Status
	normalized.CompletedAt = frozen.Attempt.CompletedAt
	normalized.SweepReason = frozen.Attempt.SweepReason
	normalized.SweepClassification = frozen.Attempt.SweepClassification
	return reflect.DeepEqual(normalized, frozen.Attempt)
}

// replaceClosePendingExact publishes a re-sealed frame only when the rooted
// canonical entry still matches the exact frozen preimage. RootedFileCAS keeps
// both complete versions when publication durability becomes ambiguous.
func (s *Store) replaceClosePendingExact(before, after closePending) error {
	path := s.closePendingPath(before.WorkID)
	expectedBody, err := canonicalCloseJSON(&before)
	if err != nil {
		return err
	}
	actualBody, err := home.ReadRootedRegularFile(s.home, path, "work close pending")
	if err != nil {
		return err
	}
	if !bytes.Equal(actualBody, expectedBody) {
		return errors.New("close frame changed before swept-claim refresh")
	}
	expectedInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if expectedInfo.Mode()&os.ModeSymlink != 0 || !expectedInfo.Mode().IsRegular() {
		return errors.New("close frame is not a regular file")
	}

	stagedPath, err := s.stageJSON(path, &after)
	if err != nil {
		return err
	}
	cleanupStaged := func(cause error) error {
		if _, statErr := os.Lstat(stagedPath); statErr == nil {
			return errors.Join(cause, removeFileAndSyncDir(stagedPath))
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return errors.Join(cause, statErr)
		}
		return cause
	}
	replacementBody, err := canonicalCloseJSON(&after)
	if err != nil {
		return cleanupStaged(err)
	}
	replacementInfo, err := os.Lstat(stagedPath)
	if err != nil {
		return cleanupStaged(err)
	}

	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return cleanupStaged(err)
	}
	defer root.Close()
	publication, err := home.BeginRootedFileCAS(
		root,
		filepath.Base(path),
		filepath.Base(stagedPath),
		expectedBody,
		expectedInfo,
		replacementBody,
		replacementInfo,
	)
	if err != nil {
		if publication == nil || publication.State() == home.RootedFileCASNotPublished {
			return cleanupStaged(err)
		}
		return err
	}
	if err := publication.Commit(); err != nil {
		return err
	}

	stored, present, err := s.loadClosePending(before.WorkID)
	if err != nil {
		return err
	}
	if !present || !reflect.DeepEqual(stored, after) {
		return errors.New("refreshed close frame does not match sealed replacement")
	}
	return nil
}

func (s *Store) failClosePendingBeforeAuthority(pending closePending, cause error) error {
	started, err := s.closePendingAuthorityStarted(pending)
	if err != nil {
		return errors.Join(cause, closeRecoveryRequired("cannot prove work %s close remained side-effect-free: %v", pending.WorkID, err))
	}
	if started {
		return closeRecoveryRequired("work %s close failed after authority preparation: %w", pending.WorkID, cause)
	}
	if err := s.clearClosePending(pending); err != nil {
		return errors.Join(cause, closeRecoveryRequired("clear side-effect-free work %s close frame: %v", pending.WorkID, err))
	}
	return cause
}

func (s *Store) clearClosePending(pending closePending) error {
	expectedBody, err := canonicalCloseJSON(&pending)
	if err != nil {
		return err
	}
	return home.RemoveRootedRegularFileDurableValidated(s.home, s.closePendingPath(pending.WorkID), "work close pending", func(body []byte) error {
		var actual closePending
		if err := decodeStrictCloseJSON(body, &actual); err != nil {
			return err
		}
		if !bytes.Equal(body, expectedBody) {
			return closeRecoveryRequired("work %s frame changed before cleanup", pending.WorkID)
		}
		return nil
	})
}

func (s *Store) completeClosePending(pending closePending, in CloseInput, now time.Time) (CloseResult, error) {
	res := CloseResult{Work: pending.SourceWork, Receipt: pending.Receipt, Transition: pending.Transition}
	inputSHA, err := closeInputSHA256(in)
	if err != nil {
		return res, err
	}
	if inputSHA != pending.InputSHA256 {
		return res, closeRecoveryRequired("work %s retry input %s differs from frozen input %s", pending.WorkID, inputSHA, pending.InputSHA256)
	}
	current, err := s.Get(pending.WorkID)
	if err != nil {
		return res, err
	}
	currentSHA, err := workEnvelopeSHA256(current)
	if err != nil {
		return res, err
	}
	currentIsFinal := currentSHA == pending.FinalWorkSHA256
	if currentSHA != pending.SourceWorkSHA256 && !currentIsFinal {
		return res, closeRecoveryRequired("work %s current generation differs from frozen close", pending.WorkID)
	}
	if !currentIsFinal {
		if err := s.revalidateCloseNonSourceEvidence(pending); err != nil {
			return res, closeRecoveryRequired("non-source delivery: %w", err)
		}
	}
	if err := s.verifyClosePendingLease(pending, currentIsFinal); err != nil {
		return res, err
	}
	if !currentIsFinal {
		pending, err = s.refreshClosePendingAfterSweptClaim(pending)
		if err != nil {
			return res, err
		}
	}

	var referenceTransaction gitReferenceTransaction
	if pending.Git.RepositoryPath != "" {
		if err := s.validateBoundGitDelivery(pending.SourceWork); err != nil {
			return res, closeRecoveryRequired("work %s Git delivery identity changed: %w", pending.WorkID, err)
		}
		referenceTransaction, err = startGitReferenceTransaction(pending.Git.RepositoryPath, pending.Git.Expectations)
		if err != nil {
			return res, s.failClosePendingBeforeAuthority(pending, fmt.Errorf("prepare Git reference transaction: %w", err))
		}
	}
	referenceFinished := false
	if referenceTransaction != nil {
		if err := s.validateBoundGitDelivery(pending.SourceWork); err != nil {
			abortErr := referenceTransaction.Abort()
			referenceFinished = true
			return res, s.failClosePendingBeforeAuthority(pending, errors.Join(fmt.Errorf("final repository identity revalidation: %w", err), abortErr))
		}
	}
	defer func() {
		if referenceTransaction != nil && !referenceFinished {
			_ = referenceTransaction.Abort()
		}
	}()
	abortReference := func(cause error) error {
		if referenceTransaction == nil {
			return cause
		}
		abortErr := referenceTransaction.Abort()
		referenceFinished = true
		if abortErr != nil {
			return errors.Join(cause, fmt.Errorf("abort Git reference transaction: %w", abortErr))
		}
		return cause
	}

	plans := plansFromClosePending(pending)
	// Hold the shared work-admission lock for workID for the entire
	// terminalization window: terminalization prepare -> terminal write ->
	// RunAttempt terminalize -> lease release. Close already runs under the
	// outer withLifecycleMutation (autonomy-admission + work-event); the
	// per-work admission lock added here is the seam every RunAttempt
	// liveness write (v3) and create-plan admission (withCreatePlanAdmissionLock,
	// legacy inode) already share, so terminalization cannot run while a
	// RunAttempt liveness write or a parallel create/sweep path holds the
	// same authority for the same workID.
	//
	// The closure captures `res` by reference and assigns the locked-section
	// return value into it; the outer return then propagates it. Close does
	// NOT call back into any RunAttempt path that takes the work-admission
	// lock — PrepareTerminalization and CompletePreparedTerminalization take
	// only the per-attempt inode — so this outer holder does not recurse.
	if err := s.withCloseAdmissionLock(pending.WorkID, func() error {
		if !currentIsFinal {
			// Mint/start/complete share this admission inode. Recheck the
			// discovery made before frame preparation while they are excluded.
			ids, censusErr := s.closeRunAttemptCensus(pending.FinalWork)
			if censusErr == nil && !slices.Equal(ids, pending.FinalWork.RunAttemptIDs) {
				censusErr = fmt.Errorf("work %s runattempt census changed before terminal publication; retry close with the complete history", pending.WorkID)
			}
			if censusErr != nil {
				return s.failClosePendingBeforeAuthority(pending, abortReference(censusErr))
			}
		}
		if prepErr := s.prepareRunAttemptTerminalizations(plans); prepErr != nil {
			res = CloseResult{Work: pending.SourceWork, Receipt: pending.Receipt, Transition: pending.Transition}
			return s.failClosePendingBeforeAuthority(pending, abortReference(prepErr))
		}
		closeAfterTerminalizationPrepare()
		if pending.LeaseStolen && !currentIsFinal {
			expected := leaseHolderStatus{Holder: pending.LeaseHolder, Stolen: true}
			if gateErr := s.leaseHolderCloseGate(pending.SourceWork, pending.Action.Actor, in, expected); gateErr != nil {
				res = CloseResult{Work: pending.SourceWork, Receipt: pending.Receipt, Transition: pending.Transition}
				return abortReference(gateErr)
			}
			closeAfterLeaseStealDecision()
		}
		if !currentIsFinal {
			if err := s.revalidateCloseNonSourceEvidence(pending); err != nil {
				return abortReference(closeRecoveryRequired("non-source delivery: %w", err))
			}
		}
		if pubErr := s.persistClosePendingEnvelopes(pending); pubErr != nil {
			res = CloseResult{Work: pending.SourceWork, Receipt: pending.Receipt, Transition: pending.Transition}
			return abortReference(pubErr)
		}
		if verErr := s.verifyClosePendingLease(pending, currentIsFinal); verErr != nil {
			res = CloseResult{Work: pending.SourceWork, Receipt: pending.Receipt, Transition: pending.Transition}
			return abortReference(verErr)
		}
		if !currentIsFinal {
			if err := s.revalidateCloseNonSourceEvidence(pending); err != nil {
				return abortReference(closeRecoveryRequired("non-source delivery: %w", err))
			}
		}
		if evErr := s.publishClosePendingEventAndWork(pending); evErr != nil {
			res = CloseResult{Work: pending.SourceWork, Receipt: pending.Receipt, Transition: pending.Transition}
			return abortReference(evErr)
		}
		if referenceTransaction != nil {
			if refErr := referenceTransaction.Commit(); refErr != nil {
				referenceFinished = true
				res = CloseResult{Work: pending.SourceWork, Receipt: pending.Receipt, Transition: pending.Transition}
				return closeRecoveryRequired("commit Git reference transaction: %w", refErr)
			}
			referenceFinished = true
		}
		unterminalized := s.terminalizePreparedRunAttempts(plans, now)
		res = CloseResult{Work: pending.FinalWork, Receipt: pending.Receipt, Transition: pending.Transition, UnterminalizedRunAttemptIDs: unterminalized}
		if len(unterminalized) > 0 {
			return closeRecoveryRequired(
				"work %s RunAttempt terminalization incomplete for %s; retry the exact close or reconcile the terminal work",
				pending.WorkID,
				strings.Join(unterminalized, ","),
			)
		}
		closeBeforeLeaseRelease()
		if relErr := s.removeClosePendingLease(pending); relErr != nil {
			return closeRecoveryRequired("release work %s lease: %w", pending.WorkID, relErr)
		}
		if clearErr := s.clearClosePending(pending); clearErr != nil {
			return closeRecoveryRequired("clear work %s frame: %w", pending.WorkID, clearErr)
		}
		return nil
	}); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Store) finishReconciledClosePending(workID string) error {
	pending, present, err := s.loadClosePending(workID)
	if err != nil || !present {
		return err
	}
	current, err := s.Get(workID)
	if err != nil {
		return err
	}
	currentSHA, err := workEnvelopeSHA256(current)
	if err != nil {
		return err
	}
	if currentSHA != pending.FinalWorkSHA256 {
		return closeRecoveryRequired("work %s is not the frozen terminal generation", workID)
	}
	store := s.runAttempts()
	for _, frozen := range pending.RunAttempts {
		if frozen.CallerOwns {
			continue
		}
		attempt, err := store.Get(frozen.Attempt.ID)
		if err != nil {
			return closeRecoveryRequired("read reconciled RunAttempt %s: %v", frozen.Attempt.ID, err)
		}
		intent := frozen.Intent
		if attempt.Status != intent.TargetStatus ||
			attempt.TerminalizationOwner != intent.Owner ||
			attempt.TerminalizationReceiptID != intent.ReceiptID ||
			attempt.TerminalizationTransitionID != intent.TransitionID ||
			attempt.TerminalizationReason != intent.TerminalizationReason {
			return closeRecoveryRequired("RunAttempt %s has not converged to its frozen terminalization", frozen.Attempt.ID)
		}
	}
	if err := s.removeClosePendingLease(pending); err != nil {
		return closeRecoveryRequired("release work %s lease after reconciliation: %v", workID, err)
	}
	if err := s.clearClosePending(pending); err != nil {
		return closeRecoveryRequired("clear work %s frame after reconciliation: %v", workID, err)
	}
	return nil
}
