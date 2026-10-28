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
	"strings"
	"syscall"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	homepkg "compound/internal/home"
	"compound/internal/mem"
	"compound/internal/policy"
)

// ReceiptedActionInput describes one bounded action outcome on a claimed,
// running, or verifying WorkEnvelope. The action may advance the run without
// claiming terminal task success; work close remains the only writer of
// WorkEnvelope.ids.transition_id.
type RequiredAttemptCompletion struct {
	AttemptID string `json:"attempt_id"`
	Owner     string `json:"owner"`
	// Frozen before launch; a failed launched attempt needs separate recovery.
	Recovery *RequiredRecoveryCompletion `json:"recovery,omitempty"`
}

type RequiredRecoveryCompletion struct {
	WorkID          string   `json:"work_id"`
	Owner           string   `json:"owner"`
	Capability      string   `json:"capability"`
	ArgumentsHashes []string `json:"arguments_hashes"`
}

type ReceiptedActionInput struct {
	// A durable, optional finalizer obligation. The reservation cannot clear
	// until this native attempt has same-action owner proof. Exact retries and
	// older callers cannot omit it because it participates in the input digest.
	RequiredAttemptCompletion *RequiredAttemptCompletion `json:",omitempty"`
	// OutcomePending reserves authority before an external effect without
	// predicting success. It must be sealed exactly once before publication.
	// Omission preserves digests of previously persisted transaction inputs.
	OutcomePending             bool `json:",omitempty"`
	Action                     envelope.ActionIntent
	OperationalDecisionClaimID string
	// PreEvaluatedDecision records an exact operator-governed decision after the
	// bounded action has actually completed or failed. It is deliberately
	// incompatible with the autonomous operational-decision path below.
	PreEvaluatedDecision *envelope.PolicyDecision
	ExpectedWorkState    string
	Outcome              string
	ChangeKnowledge      string `json:",omitempty"`
	Changed              bool
	Summary              string
	MoveType             string
	ProofRefs            []string
	BeforeRefs           []string
	AfterRefs            []string
	FilesChanged         []string
	FailureCause         *envelope.FailureCause
}

type ReceiptedActionResult struct {
	Work       envelope.WorkEnvelope   `json:"work"`
	Action     envelope.ActionIntent   `json:"action"`
	Decision   envelope.PolicyDecision `json:"decision"`
	Receipt    envelope.Receipt        `json:"receipt"`
	Transition envelope.Transition     `json:"transition"`
	Replayed   bool                    `json:"replayed"`
}

const receiptedActionPendingSchemaVersion = "compound.receipted_action_pending.v0.2"
const receiptedActionPendingLegacySchemaVersion = "compound.receipted_action_pending.v0.1"

var receiptedActionEligibleStates = map[string]bool{"claimed": true, "running": true, "verifying": true}

// receiptedActionPending is the direct-addressed stage zero that prevents a
// later action from overtaking an incomplete same-work transaction.
type receiptedActionPending struct {
	SchemaVersion     string `json:"schema_version"`
	WorkID            string `json:"work_id"`
	ActionID          string `json:"action_intent_id"`
	PolicyDecisionID  string `json:"policy_decision_id"`
	ReceiptID         string `json:"receipt_id"`
	TransitionID      string `json:"transition_id"`
	CreatedAt         string `json:"created_at"`
	CompletedAt       string `json:"completed_at"`
	TransactionDigest string `json:"transaction_digest"`
}

var receiptedActionBeforeCommit = func() {}
var receiptedActionBeforePublish = func(string) error { return nil }
var receiptedActionAfterWrite = func(string) error { return nil }
var writeReceiptedActionTemp = func(_ string, file *os.File, data []byte) error {
	return writeReceiptedActionTempFully(file, data)
}

func (s *Store) RecordReceiptedAction(workID string, in ReceiptedActionInput, now time.Time) (ReceiptedActionResult, error) {
	var out ReceiptedActionResult
	var recordErr error
	err := homepkg.WithAutonomyAdmissionLock(s.home, func() error {
		out, recordErr = s.recordReceiptedActionLocked(workID, in, now, now, true)
		return recordErr
	})
	return out, err
}

func (s *Store) recordReceiptedActionLocked(workID string, in ReceiptedActionInput, authorityAt, completedAt time.Time, pendingReplaysAuthority bool) (ReceiptedActionResult, error) {
	return s.recordReceiptedActionWithPublicationLocked(workID, in, authorityAt, completedAt, pendingReplaysAuthority, nil)
}

func (s *Store) recordReceiptedActionWithPublicationLocked(workID string, in ReceiptedActionInput, authorityAt, completedAt time.Time, pendingReplaysAuthority bool, publication *privatePublicationCompletion) (ReceiptedActionResult, error) {
	var out ReceiptedActionResult
	if in.OutcomePending {
		return out, fmt.Errorf("receipted action outcome is not sealed")
	}
	if authorityAt.IsZero() && completedAt.IsZero() {
		authorityAt = time.Now()
		completedAt = authorityAt
	} else {
		if authorityAt.IsZero() {
			authorityAt = completedAt
		}
		if completedAt.IsZero() {
			completedAt = authorityAt
		}
	}
	w, err := s.recoverPendingClaimBeforeMutation(workID)
	if err != nil {
		return out, err
	}
	if err := w.Validate(); err != nil {
		return out, fmt.Errorf("work envelope invalid: %w", err)
	}
	explicitDecision := in.PreEvaluatedDecision != nil
	if explicitDecision && w.State != "running" {
		return out, fmt.Errorf("work %s is %s; pre-evaluated governed actions require state running", workID, w.State)
	}
	if !explicitDecision && !receiptedActionEligibleStates[w.State] {
		return out, fmt.Errorf("work %s is %s; receipted actions require state claimed|running|verifying", workID, w.State)
	}
	if expected := strings.TrimSpace(in.ExpectedWorkState); expected != "" && w.State != expected {
		return out, fmt.Errorf("work %s state %q no longer matches expected state %q", workID, w.State, expected)
	}
	requestedAction := in.Action
	if requestedAction.IDs.WorkID != workID {
		return out, fmt.Errorf("action work_id %q does not match work %q", requestedAction.IDs.WorkID, workID)
	}
	if err := requestedAction.Validate(); err != nil {
		return out, fmt.Errorf("action intent invalid: %w", err)
	}
	pending, pendingPresent, pendingPath, err := s.preflightReceiptedActionPending(workID)
	if err != nil {
		return out, err
	}
	if pendingPresent {
		if pending.ActionID != requestedAction.ID {
			return out, fmt.Errorf("work %s has pending receipted action %s; cannot record %s", workID, pending.ActionID, requestedAction.ID)
		}
		requestedAction.CreatedAt = pending.CreatedAt
	}
	claimID := strings.TrimSpace(in.OperationalDecisionClaimID)
	var cfg policy.Config
	if !explicitDecision {
		if claimID == "" {
			return out, fmt.Errorf("operational decision claim id required")
		}
		if err := currentReceiptedActionLaw(s.home, claimID); err != nil {
			return out, err
		}
		cfg, _, err = policy.LoadConfig(s.home)
		if err != nil {
			return out, err
		}
	}
	action, actionPresent, err := s.preflightReceiptedAction(requestedAction)
	if err != nil {
		return out, err
	}
	transactionNow, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		return out, fmt.Errorf("receipted action %s created_at invalid: %w", action.ID, err)
	}
	var currentDecision envelope.PolicyDecision
	if explicitDecision {
		currentDecision = *in.PreEvaluatedDecision
		if err := validatePreEvaluatedReceiptedActionStatic(s, w, action, currentDecision); err != nil {
			return out, err
		}
		decisionAt, err := time.Parse(time.RFC3339Nano, currentDecision.CreatedAt)
		if err != nil {
			return out, fmt.Errorf("pre-evaluated policy decision created_at invalid: %w", err)
		}
		if decisionAt.After(transactionNow) {
			transactionNow = decisionAt
		}
		if completedAt.After(transactionNow) {
			transactionNow = completedAt
		}
	} else {
		if reason := policy.AutonomyOperatorOnlyReason(action, cfg); reason != "" {
			return out, fmt.Errorf("cannot record autonomous receipted action: %s", reason)
		}
		currentDecision = policy.Evaluate(action, cfg, authorityAt)
		if !policy.Allowed(currentDecision) {
			return out, fmt.Errorf("cannot record receipted action %s: current policy %s (%s)", action.ID, currentDecision.Outcome, strings.Join(currentDecision.Reasons, "; "))
		}
	}
	if pendingPresent {
		if pending.CompletedAt != "" {
			transactionNow, err = time.Parse(time.RFC3339Nano, pending.CompletedAt)
			if err != nil {
				return out, fmt.Errorf("work %s pending receipted action completion time invalid: %w", workID, err)
			}
		}
	}
	if action.MutationClass != "read" && strings.TrimSpace(action.RollbackPlan) == "" {
		return out, fmt.Errorf("receipted action %s requires rollback_plan", action.ID)
	}

	proofRefs := mergeUnique(in.ProofRefs, action.EvidenceRefs, in.BeforeRefs, in.AfterRefs, []string{"work:" + workID})
	if !explicitDecision {
		proofRefs = mergeUnique(proofRefs, []string{"claim:" + claimID})
	}
	moveType := strings.TrimSpace(in.MoveType)
	if moveType == "" {
		moveType = "control_plane_repair"
	}
	if !oneOf(moveType, envelope.MoveTypes) {
		return out, fmt.Errorf("receipted action move_type must be one of %v, got %q", envelope.MoveTypes, moveType)
	}
	if strings.TrimSpace(in.Summary) == "" {
		return out, fmt.Errorf("receipted action summary required")
	}

	decisionID := idFor("pol_", "receipted-action", workID, action.ID, claimID)
	if explicitDecision {
		decisionID = currentDecision.ID
	}
	outcome := strings.TrimSpace(in.Outcome)
	if outcome == "" {
		outcome = "completed"
	}
	changed := in.Changed
	if !explicitDecision {
		changed = true
	}
	receiptID := idFor("rec_", "receipted-action", workID, action.ID, claimID)
	transitionID := idFor("tr_", "receipted-action", workID, action.ID, claimID)
	if explicitDecision {
		receiptID = idFor("rec_", "receipted-action", workID, action.ID, decisionID, outcome)
		transitionID = idFor("tr_", "receipted-action", workID, action.ID, decisionID, outcome)
	}
	decision := currentDecision
	if !explicitDecision {
		decision.ID = decisionID
		decision.CreatedAt = action.CreatedAt
		decision.EvidenceRefs = mergeUnique(decision.EvidenceRefs, proofRefs)
	}
	used := normalizeCapabilityRefs([]string{action.Capability})
	receipt := envelope.Receipt{
		Header:     newHeader(envelope.SVReceipt, receiptID, transactionNow, workID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Outcome:    outcome,
		MoveType:   moveType,
		StateDelta: envelope.StateDelta{Changed: changed, ChangeKnowledge: in.ChangeKnowledge, Summary: strings.TrimSpace(in.Summary), FilesChanged: mergeUnique(in.FilesChanged), Artifacts: []string{}, Verification: []string{}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills: envelope.ReceiptSkills{
			Used: used, Missed: []string{}, Candidate: []string{}, UsedDeclared: true,
		},
		Memory:    envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:      envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: proofRefs, FailureCause: in.FailureCause,
	}
	if explicitDecision {
		receipt.CreatedAt = transactionNow.UTC().Format(time.RFC3339Nano)
	}
	receipt.EvidenceRefs = proofRefs
	if lane := strings.TrimSpace(action.Producer.LaneID); lane != "" {
		receipt.Producer.LaneID = lane
	}
	if err := receipt.Validate(); err != nil {
		return out, fmt.Errorf("emitted receipted-action receipt invalid: %w", err)
	}
	beforeRefs := mergeUnique([]string{w.IDs.ContextPackID}, in.BeforeRefs)
	if !explicitDecision {
		beforeRefs = mergeUnique(beforeRefs, []string{"claim:" + claimID})
	}
	transition := envelope.Transition{
		Header:     newHeader(envelope.SVTransition, transitionID, transactionNow, workID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		BeforeRefs: beforeRefs,
		ActionRefs: []string{action.ID},
		AfterRefs:  mergeUnique([]string{receipt.ID}, in.AfterRefs),
		StateDelta: receipt.StateDelta.Summary,
		Projections: envelope.Projections{
			Memory: []string{}, Skill: used, Product: []string{}, Cost: []string{}, Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{},
			Typed: closeProjectionRefs(receipt, w.IDs.ContextPackID),
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: changed},
	}
	if explicitDecision {
		transition.CreatedAt = transactionNow.UTC().Format(time.RFC3339Nano)
	}
	transition.EvidenceRefs = proofRefs
	if lane := strings.TrimSpace(action.Producer.LaneID); lane != "" {
		transition.Producer.LaneID = lane
	}
	if err := transition.Validate(); err != nil {
		return out, fmt.Errorf("emitted receipted-action transition invalid: %w", err)
	}
	expectedPending, err := newReceiptedActionPending(workID, action, decision, receipt, transition)
	if err != nil {
		return out, err
	}
	legacyReplayAuthorityAt := time.Time{}
	if pendingPresent && pending.SchemaVersion == receiptedActionPendingLegacySchemaVersion {
		transactionNow, receipt, transition, expectedPending, err = s.selectLegacyReceiptedActionTransaction(workID, pending, action, decision, receipt, transition)
		if err != nil {
			return out, err
		}
		legacyReplayAuthorityAt, err = s.sealedReceiptedActionAuthorityAt(workID, action, decision)
		if err != nil {
			return out, err
		}
	}
	if pendingPresent && !receiptedActionPendingEqual(pending, expectedPending) {
		return out, fmt.Errorf("work %s pending receipted action %s conflicts with requested transaction", workID, pending.ActionID)
	}
	if explicitDecision {
		effectiveAuthorityAt := authorityAt
		if pendingPresent && pendingReplaysAuthority {
			if pending.SchemaVersion == receiptedActionPendingLegacySchemaVersion {
				effectiveAuthorityAt = legacyReplayAuthorityAt
			} else {
				effectiveAuthorityAt = transactionNow
			}
		}
		if err := validatePreEvaluatedReceiptedActionAt(action, currentDecision, effectiveAuthorityAt); err != nil {
			return out, err
		}
	}

	decisionPresent, err := s.preflightReceiptedEnvelope("decision", decision.ID, &decision, &envelope.PolicyDecision{})
	if err != nil {
		return out, err
	}
	receiptPresent, err := s.preflightReceiptedEnvelope("receipt", receipt.ID, &receipt, &envelope.Receipt{})
	if err != nil {
		return out, err
	}
	transitionPresent, err := s.preflightReceiptedEnvelope("transition", transition.ID, &transition, &envelope.Transition{})
	if err != nil {
		return out, err
	}

	if publication != nil {
		if err := s.validatePrivatePublicationCompletion(w, publication.request, in, authorityAt); err != nil {
			return out, err
		}
	}
	workLinked, historicalReplay, err := s.preflightReceiptedActionWorkLink(w, action.ID, decision.ID)
	if err != nil && publication != nil {
		if baselineErr := s.validatePrivatePublicationPrelink(w, publication.request, authorityAt); baselineErr != nil {
			return out, fmt.Errorf("private publication pre-admission link: %w", baselineErr)
		}
		workLinked, historicalReplay, err = false, false, nil
	}
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(w.IDs.TransitionID) != "" {
		return out, fmt.Errorf("receipted action %s conflicts with persisted work transition_id %s", action.ID, w.IDs.TransitionID)
	}
	expectedWork := w
	if !historicalReplay {
		expectedWork.ActionIntentIDs = mergeUnique(expectedWork.ActionIntentIDs, []string{action.ID})
		expectedWork.PolicyDecisionID = decision.ID
	}
	if err := expectedWork.Validate(); err != nil {
		return out, fmt.Errorf("receipted action %s expected work invalid: %w", action.ID, err)
	}
	expectedEvent := receiptedActionEvent(expectedWork, action, decision, receipt, transition, transactionNow)
	events, err := s.admissionEventsFor(workID)
	if err != nil {
		return out, fmt.Errorf("receipted action %s event preflight: %w", action.ID, err)
	}
	if publication != nil {
		events, err = s.privatePublicationCompletionEvents(w, publication.request, in, authorityAt, expectedEvent, events)
		if err != nil {
			return out, err
		}
	}
	eventPresent, err := preflightReceiptedActionEvent(events, expectedEvent)
	if err != nil {
		return out, err
	}
	if historicalReplay && !eventPresent {
		return out, fmt.Errorf("receipted action %s historical transaction is missing its event", action.ID)
	}
	stages := []struct {
		name    string
		present bool
	}{
		{"action", actionPresent},
		{"decision", decisionPresent},
		{"receipt", receiptPresent},
		{"transition", transitionPresent},
		{"work", workLinked},
		{"event", eventPresent},
	}
	if err := validateReceiptedActionPrefix(action.ID, stages); err != nil {
		return out, err
	}
	replayed := pendingPresent
	transactionComplete := true
	for _, stage := range stages {
		replayed = replayed || stage.present
		transactionComplete = transactionComplete && stage.present
	}

	// The direct writer passes pendingReplaysAuthority=true: an ActionIntent
	// alone may be only an approval proposal, so fresh source still needs
	// admission. The only false caller is RecordReservedReceiptedAction, after
	// authenticating the exact reservation already admitted before its effect.
	// Matching pending recovery and fully complete transactions retain their
	// sealed admission, rather than reclassifying a completed effect.
	if pendingReplaysAuthority && !pendingPresent && !transactionComplete {
		if err := s.validateSourceActionAdmission(w, action); err != nil {
			return out, err
		}
	}
	receiptedActionBeforeCommit()
	markerOwned := pendingPresent
	if !transactionComplete && !markerOwned {
		if err := ensureReceiptedActionPendingDir(filepath.Dir(pendingPath)); err != nil {
			return out, err
		}
		if err := s.writeReceiptedActionBoundary("pending", pendingPath, &expectedPending); err != nil {
			return out, err
		}
		markerOwned = true
	}
	for _, item := range []struct {
		name    string
		present bool
		id      string
		payload envelope.Envelope
	}{
		{"action", actionPresent, action.ID, &action},
		{"decision", decisionPresent, decision.ID, &decision},
		{"receipt", receiptPresent, receipt.ID, &receipt},
		{"transition", transitionPresent, transition.ID, &transition},
	} {
		if item.present {
			continue
		}
		if err := s.writeReceiptedActionEventBoundary(item.name, item.id, item.payload); err != nil {
			return out, err
		}
	}
	if !workLinked {
		if err := s.writeReceiptedActionBoundary("work", s.workPath(workID), &expectedWork); err != nil {
			return out, err
		}
	}
	if !eventPresent {
		if err := s.appendEventControlled(
			expectedEvent,
			func() error { return receiptedActionBeforePublish("event") },
			func() error { return receiptedActionAfterWrite("event") },
		); err != nil {
			return out, err
		}
	}
	if markerOwned {
		if err := s.clearReceiptedActionPending(pendingPath, expectedPending); err != nil {
			return out, err
		}
	}
	return ReceiptedActionResult{Work: expectedWork, Action: action, Decision: decision, Receipt: receipt, Transition: transition, Replayed: replayed}, nil
}

func validatePreEvaluatedReceiptedAction(s *Store, w envelope.WorkEnvelope, action envelope.ActionIntent, decision envelope.PolicyDecision, now time.Time) error {
	if err := validatePreEvaluatedReceiptedActionStatic(s, w, action, decision); err != nil {
		return err
	}
	return validatePreEvaluatedReceiptedActionAt(action, decision, now)
}

func validatePreEvaluatedReceiptedActionStatic(s *Store, w envelope.WorkEnvelope, action envelope.ActionIntent, decision envelope.PolicyDecision) error {
	dangerous := preEvaluatedReceiptedActionIsDangerous(action.MutationClass)
	exactApproved := false
	if dangerous {
		exactApproved = policy.NewExactActionApprovalStore(s.home).ValidateConsumedDecision(action, decision) == nil
	}
	if dangerous && !policy.HasOperatorProvenance(action.Actor) && !exactApproved {
		return fmt.Errorf("pre-evaluated %s action requires operator provenance", action.MutationClass)
	}
	lease, err := s.readLease(w.ID)
	if err != nil {
		return fmt.Errorf("pre-evaluated governed action requires current lease holder: %w", err)
	}
	if lease.WorkID != w.ID || strings.TrimSpace(lease.By) == "" || lease.By != action.Actor {
		return fmt.Errorf("pre-evaluated governed action actor %q does not match lease holder %q", action.Actor, lease.By)
	}
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("pre-evaluated policy decision invalid: %w", err)
	}
	if decision.ActionIntentID != action.ID || decision.IDs.WorkID != action.IDs.WorkID {
		return fmt.Errorf("pre-evaluated policy decision does not bind action intent")
	}
	if !policy.Allowed(decision) {
		return fmt.Errorf("pre-evaluated policy decision is not allowed: %s", decision.Outcome)
	}
	if dangerous && strings.TrimSpace(decision.StandingGrantID) == "" && !exactApproved {
		return fmt.Errorf("pre-evaluated %s action requires standing-grant or consumed exact-approval authority", action.MutationClass)
	}
	return nil
}

// preEvaluatedReceiptedActionIsDangerous reports whether a caller-supplied
// (pre-evaluated) PolicyDecision for this mutation class must independently
// prove operator provenance and standing-grant authority before being
// trusted, rather than accepting the caller's "allow" outcome at face value.
// A pre-evaluated decision never calls policy.Evaluate, so for a dangerous
// class this is the only re-validation the governance plane performs (E5).
// Enumerated from envelope.MutationClasses: read/local_reversible/
// external_side_effect are low-stakes and pass through unchanged; security/
// destructive/financial/public are dangerous and always re-validated; any
// future or unrecognized mutation class fails closed into the dangerous set.
func preEvaluatedReceiptedActionIsDangerous(mutationClass string) bool {
	switch mutationClass {
	case "read", "local_reversible", "external_side_effect":
		return false
	case "security", "destructive", "financial", "public":
		return true
	default:
		return true
	}
}

func validatePreEvaluatedReceiptedActionAt(action envelope.ActionIntent, decision envelope.PolicyDecision, now time.Time) error {
	actionAt, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		return fmt.Errorf("pre-evaluated action created_at invalid: %w", err)
	}
	decisionAt, err := time.Parse(time.RFC3339Nano, decision.CreatedAt)
	if err != nil {
		return fmt.Errorf("pre-evaluated policy decision created_at invalid: %w", err)
	}
	if actionAt.After(now) || decisionAt.After(now) {
		return fmt.Errorf("pre-evaluated action and policy decision must not postdate authorization")
	}
	if expiresAt := strings.TrimSpace(decision.ExpiresAt); expiresAt != "" {
		expires, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			return fmt.Errorf("pre-evaluated policy decision expires_at invalid: %w", err)
		}
		if !now.Before(expires) {
			return fmt.Errorf("pre-evaluated policy decision expired at %s", expiresAt)
		}
	}
	return nil
}

func currentReceiptedActionLaw(homePath, claimID string) error {
	store, err := mem.Open(homePath)
	if err != nil {
		return err
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		return err
	}
	for _, claim := range mem.AuthoritativeActiveOperationalDecisions(claims) {
		if claim.ID == claimID {
			return nil
		}
	}
	return fmt.Errorf("claim %s is not a current authoritative operational decision", claimID)
}

func (s *Store) preflightReceiptedAction(requested envelope.ActionIntent) (envelope.ActionIntent, bool, error) {
	var stored envelope.ActionIntent
	err := s.readEventJSON(requested.ID, &stored)
	if errors.Is(err, os.ErrNotExist) {
		return requested, false, nil
	}
	if err != nil {
		return envelope.ActionIntent{}, false, fmt.Errorf("receipted action %s persisted action unreadable: %w", requested.ID, err)
	}
	if err := stored.Validate(); err != nil {
		return envelope.ActionIntent{}, false, fmt.Errorf("receipted action %s persisted action invalid: %w", requested.ID, err)
	}
	// The adapter rebuilds the same deterministic action on retry with a fresh
	// hook timestamp. CreatedAt is the only deliberately volatile field; the
	// first durable action fixes the transaction timestamp for the suffix.
	normalized := requested
	normalized.CreatedAt = stored.CreatedAt
	if !reflect.DeepEqual(stored, normalized) {
		return envelope.ActionIntent{}, false, fmt.Errorf("receipted action %s persisted action conflicts with requested transaction", requested.ID)
	}
	return stored, true, nil
}

func (s *Store) preflightReceiptedEnvelope(kind, id string, expected, target envelope.Envelope) (bool, error) {
	err := s.readEventJSON(id, target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("receipted action %s persisted %s unreadable: %w", id, kind, err)
	}
	if err := target.Validate(); err != nil {
		return false, fmt.Errorf("receipted action %s persisted %s invalid: %w", id, kind, err)
	}
	if !reflect.DeepEqual(expected, target) {
		return false, fmt.Errorf("receipted action %s persisted %s conflicts with requested transaction", id, kind)
	}
	return true, nil
}

func (s *Store) preflightReceiptedActionPending(workID string) (receiptedActionPending, bool, string, error) {
	var pending receiptedActionPending
	if filepath.Base(workID) != workID || !strings.HasPrefix(workID, "work_") {
		return pending, false, "", fmt.Errorf("invalid receipted-action work id %q", workID)
	}
	path := filepath.Join(s.home, "work", "receipted-actions", workID+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, path, nil
	}
	if err != nil {
		return pending, false, path, fmt.Errorf("work %s pending receipted action unreadable: %w", workID, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil {
		return pending, false, path, fmt.Errorf("work %s pending receipted action invalid: %w", workID, err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return pending, false, path, fmt.Errorf("work %s pending receipted action invalid: %w", workID, err)
	}
	if err := pending.validate(); err != nil {
		return pending, false, path, fmt.Errorf("work %s pending receipted action invalid: %w", workID, err)
	}
	if pending.WorkID != workID {
		return pending, false, path, fmt.Errorf("work %s pending receipted action belongs to %s", workID, pending.WorkID)
	}
	return pending, true, path, nil
}

func (s *Store) pendingReceiptedActionID(workID string) (string, error) {
	pending, present, _, err := s.preflightReceiptedActionPending(workID)
	if err != nil {
		return "", err
	}
	reservation, reserved, _, err := s.preflightReceiptedActionReservation(workID)
	if err != nil {
		return "", err
	}
	if present && reserved && pending.ActionID != reservation.ActionID {
		return "", fmt.Errorf("work %s has conflicting pending receipted actions %s and %s", workID, pending.ActionID, reservation.ActionID)
	}
	if present {
		return pending.ActionID, nil
	}
	if reserved {
		return reservation.ActionID, nil
	}
	return "", nil
}

func (p receiptedActionPending) validate() error {
	switch p.SchemaVersion {
	case receiptedActionPendingSchemaVersion:
		if strings.TrimSpace(p.CompletedAt) == "" {
			return fmt.Errorf("completed_at required")
		}
	case receiptedActionPendingLegacySchemaVersion:
		if strings.TrimSpace(p.CompletedAt) != "" {
			return fmt.Errorf("completed_at must be absent for legacy schema_version %q", receiptedActionPendingLegacySchemaVersion)
		}
	default:
		return fmt.Errorf("schema_version must be %q or %q", receiptedActionPendingLegacySchemaVersion, receiptedActionPendingSchemaVersion)
	}
	for _, field := range []struct {
		name, value string
	}{
		{"work_id", p.WorkID},
		{"action_intent_id", p.ActionID},
		{"policy_decision_id", p.PolicyDecisionID},
		{"receipt_id", p.ReceiptID},
		{"transition_id", p.TransitionID},
		{"created_at", p.CreatedAt},
		{"transaction_digest", p.TransactionDigest},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s required", field.name)
		}
	}
	if filepath.Base(p.WorkID) != p.WorkID || !strings.HasPrefix(p.WorkID, "work_") {
		return fmt.Errorf("invalid work_id %q", p.WorkID)
	}
	for _, item := range []struct {
		name, value, prefix string
	}{
		{"action_intent_id", p.ActionID, "act_"},
		{"policy_decision_id", p.PolicyDecisionID, "pol_"},
		{"receipt_id", p.ReceiptID, "rec_"},
		{"transition_id", p.TransitionID, "tr_"},
	} {
		if !strings.HasPrefix(item.value, item.prefix) {
			return fmt.Errorf("%s must start with %q", item.name, item.prefix)
		}
	}
	if _, err := time.Parse(time.RFC3339, p.CreatedAt); err != nil {
		return fmt.Errorf("created_at invalid: %w", err)
	}
	if p.CompletedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, p.CompletedAt); err != nil {
			return fmt.Errorf("completed_at invalid: %w", err)
		}
	}
	digest, err := hex.DecodeString(p.TransactionDigest)
	if err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("transaction_digest must be a SHA-256 hex digest")
	}
	return nil
}

// receiptedActionPendingEqual preserves the exact v0.1 byte contract without
// weakening v0.2. A legacy marker omitted completed_at; compatibility may fill
// it only after the full transaction digest selects a sealed historical form.
func receiptedActionPendingEqual(actual, expected receiptedActionPending) bool {
	if actual.SchemaVersion == receiptedActionPendingLegacySchemaVersion && actual.CompletedAt == "" {
		actual.SchemaVersion = expected.SchemaVersion
		actual.CompletedAt = expected.CompletedAt
	}
	return reflect.DeepEqual(actual, expected)
}

// selectLegacyReceiptedActionTransaction reconstructs only the timestamp form
// the released v0.1 writer emitted: transactionNow came from
// ActionIntent.created_at and newHeader formatted it as RFC3339. Reservation
// and decision timestamps never authenticated a sealed v0.1 transaction, so a
// pending digest matching only such a form fails closed.
func (s *Store) selectLegacyReceiptedActionTransaction(
	workID string,
	pending receiptedActionPending,
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
	receipt envelope.Receipt,
	transition envelope.Transition,
) (time.Time, envelope.Receipt, envelope.Transition, receiptedActionPending, error) {
	actionAt, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		return time.Time{}, receipt, transition, receiptedActionPending{}, err
	}
	// Historical newHeader formatting was RFC3339, even when sealed inputs
	// carried finer precision.
	createdAt := actionAt.UTC().Format(time.RFC3339)
	candidateReceipt := receipt
	candidateReceipt.CreatedAt = createdAt
	candidateTransition := transition
	candidateTransition.CreatedAt = createdAt
	candidatePending, err := newReceiptedActionPending(workID, action, decision, candidateReceipt, candidateTransition)
	if err != nil {
		return time.Time{}, receipt, transition, receiptedActionPending{}, err
	}
	if !receiptedActionPendingEqual(pending, candidatePending) {
		return time.Time{}, receipt, transition, receiptedActionPending{}, fmt.Errorf("work %s legacy pending receipted action %s conflicts with every sealed historical transaction", workID, pending.ActionID)
	}
	parsed, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return time.Time{}, receipt, transition, receiptedActionPending{}, err
	}
	return parsed, candidateReceipt, candidateTransition, candidatePending, nil
}

func (s *Store) sealedReceiptedActionAuthorityAt(workID string, action envelope.ActionIntent, decision envelope.PolicyDecision) (time.Time, error) {
	authorityAt, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		return time.Time{}, err
	}
	decisionAt, err := time.Parse(time.RFC3339Nano, decision.CreatedAt)
	if err != nil {
		return time.Time{}, err
	}
	if decisionAt.After(authorityAt) {
		authorityAt = decisionAt
	}
	reservation, reserved, _, err := s.preflightReceiptedActionReservation(workID)
	if err != nil {
		return time.Time{}, err
	}
	if reserved && reservation.ActionID == action.ID && reservation.PolicyDecisionID == decision.ID {
		reservedAt, err := time.Parse(time.RFC3339Nano, reservation.AuthorizedAt)
		if err != nil {
			return time.Time{}, err
		}
		if reservedAt.After(authorityAt) {
			authorityAt = reservedAt
		}
	}
	return authorityAt, nil
}

func newReceiptedActionPending(workID string, action envelope.ActionIntent, decision envelope.PolicyDecision, receipt envelope.Receipt, transition envelope.Transition) (receiptedActionPending, error) {
	payload := struct {
		Action     envelope.ActionIntent   `json:"action"`
		Decision   envelope.PolicyDecision `json:"decision"`
		Receipt    envelope.Receipt        `json:"receipt"`
		Transition envelope.Transition     `json:"transition"`
	}{Action: action, Decision: decision, Receipt: receipt, Transition: transition}
	data, err := json.Marshal(payload)
	if err != nil {
		return receiptedActionPending{}, fmt.Errorf("marshal receipted-action pending digest: %w", err)
	}
	digest := sha256.Sum256(data)
	return receiptedActionPending{
		SchemaVersion: receiptedActionPendingSchemaVersion, WorkID: workID, ActionID: action.ID,
		PolicyDecisionID: decision.ID, ReceiptID: receipt.ID, TransitionID: transition.ID,
		CreatedAt: action.CreatedAt, CompletedAt: receipt.CreatedAt, TransactionDigest: hex.EncodeToString(digest[:]),
	}, nil
}

func ensureReceiptedActionPendingDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return syncReceiptedActionDir(filepath.Dir(dir))
}

func (s *Store) clearReceiptedActionPending(path string, expected receiptedActionPending) error {
	if err := receiptedActionBeforePublish("pending_clear"); err != nil {
		return err
	}
	actual, present, actualPath, err := s.preflightReceiptedActionPending(expected.WorkID)
	if err != nil {
		return err
	}
	if !present || actualPath != path || !receiptedActionPendingEqual(actual, expected) {
		return fmt.Errorf("work %s pending receipted action %s changed before cleanup", expected.WorkID, expected.ActionID)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := syncReceiptedActionDir(filepath.Dir(path)); err != nil {
		return err
	}
	return receiptedActionAfterWrite("pending_clear")
}

func (s *Store) preflightReceiptedActionWorkLink(w envelope.WorkEnvelope, actionID, decisionID string) (bool, bool, error) {
	actionIndex := -1
	latestActionIndex := -1
	for i, ref := range w.ActionIntentIDs {
		if strings.HasPrefix(ref, "act_") {
			latestActionIndex = i
		}
		if ref != actionID {
			continue
		}
		if actionIndex >= 0 {
			return false, false, fmt.Errorf("receipted action %s conflicts with persisted work links: duplicate run reference", actionID)
		}
		actionIndex = i
	}

	policyLinked := w.PolicyDecisionID == decisionID
	if actionIndex < 0 {
		if policyLinked {
			return false, false, fmt.Errorf("receipted action %s conflicts with persisted work links: policy is linked without its action", actionID)
		}
		return false, false, nil
	}
	if actionIndex == latestActionIndex {
		if !policyLinked {
			return false, false, fmt.Errorf("receipted action %s conflicts with persisted work links: latest action policy differs", actionID)
		}
		return true, false, nil
	}
	if policyLinked {
		return false, false, fmt.Errorf("receipted action %s conflicts with persisted work links: historical action owns the current policy link", actionID)
	}

	latestActionID := w.ActionIntentIDs[latestActionIndex]
	if err := s.validateReceiptedActionNewerWorkLink(w, latestActionID); err != nil {
		return false, false, fmt.Errorf("receipted action %s conflicts with persisted work links: %w", actionID, err)
	}
	return true, true, nil
}

func (s *Store) validateReceiptedActionNewerWorkLink(w envelope.WorkEnvelope, latestActionID string) error {
	var latestAction envelope.ActionIntent
	if err := s.readEventJSON(latestActionID, &latestAction); err != nil {
		return fmt.Errorf("latest action %s unreadable: %w", latestActionID, err)
	}
	if err := latestAction.Validate(); err != nil {
		return fmt.Errorf("latest action %s invalid: %w", latestActionID, err)
	}
	if latestAction.ID != latestActionID || latestAction.IDs.WorkID != w.ID {
		return fmt.Errorf("latest action %s does not belong to work %s", latestActionID, w.ID)
	}

	policyID := strings.TrimSpace(w.PolicyDecisionID)
	if policyID == "" {
		return fmt.Errorf("latest action %s has no current policy", latestActionID)
	}
	var latestDecision envelope.PolicyDecision
	if err := s.readEventJSON(policyID, &latestDecision); err != nil {
		return fmt.Errorf("current policy %s unreadable: %w", policyID, err)
	}
	if err := latestDecision.Validate(); err != nil {
		return fmt.Errorf("current policy %s invalid: %w", policyID, err)
	}
	if latestDecision.ID != policyID || latestDecision.IDs.WorkID != w.ID || latestDecision.ActionIntentID != latestActionID {
		return fmt.Errorf("current policy %s does not belong to latest action %s", policyID, latestActionID)
	}
	if !policy.Allowed(latestDecision) {
		return fmt.Errorf("current policy %s did not allow latest action %s", policyID, latestActionID)
	}
	return nil
}

func validateReceiptedActionPrefix(actionID string, stages []struct {
	name    string
	present bool
}) error {
	missing := false
	for _, stage := range stages {
		if !stage.present {
			missing = true
			continue
		}
		if missing {
			return fmt.Errorf("receipted action %s persisted transaction is out of order at %s", actionID, stage.name)
		}
	}
	return nil
}

func (s *Store) writeReceiptedActionBoundary(name, path string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := writeReceiptedActionTemp(name, tmp, data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := receiptedActionBeforePublish(name); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := syncReceiptedActionDir(dir); err != nil {
		return err
	}
	return receiptedActionAfterWrite(name)
}

func (s *Store) writeReceiptedActionEventBoundary(name, id string, payload any) error {
	_, err := s.events.PutJSONAtomic(id, payload, "  ", eventstore.AtomicOptions{
		SyncDirectory: true,
		Write: func(file *os.File, data []byte) error {
			return writeReceiptedActionTemp(name, file, data)
		},
		BeforePublish: func() error {
			return receiptedActionBeforePublish(name)
		},
		AfterPublish: func() error {
			return receiptedActionAfterWrite(name)
		},
	})
	return err
}

func writeReceiptedActionTempFully(file *os.File, data []byte) error {
	for remaining := data; len(remaining) > 0; {
		n, err := file.Write(remaining)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		remaining = remaining[n:]
	}
	return nil
}

func syncReceiptedActionDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

func preflightReceiptedActionEvent(events []Event, expected Event) (bool, error) {
	matching := 0
	for _, event := range events {
		if !receiptedActionEventTouches(event, expected.Attached) {
			continue
		}
		if !sameReceiptedActionEvent(event, expected) {
			return false, fmt.Errorf("receipted action %s persisted event conflicts with requested transaction", expected.Attached[0])
		}
		matching++
	}
	if matching > 1 {
		return false, fmt.Errorf("receipted action %s has %d duplicate persisted events", expected.Attached[0], matching)
	}
	return matching == 1, nil
}

func receiptedActionEventTouches(event Event, refs []string) bool {
	for _, attached := range event.Attached {
		if proofChainContains(refs, attached) {
			return true
		}
	}
	return false
}

func sameReceiptedActionEvent(a, b Event) bool {
	// Event time was nanosecond precision before recovery fixed its transaction
	// clock to ActionIntent.created_at. Every authority/link field must match;
	// TS is provenance-only and may differ across the runtime cutover.
	a.TS = b.TS
	return reflect.DeepEqual(a, b)
}

func receiptedActionEvent(w envelope.WorkEnvelope, action envelope.ActionIntent, decision envelope.PolicyDecision, receipt envelope.Receipt, transition envelope.Transition, now time.Time) Event {
	return Event{TS: ts(now), WorkID: w.ID, From: w.State, To: w.State, By: action.Actor, Note: "receipted action recorded: " + action.Action + " (policy " + decision.Outcome + ")", Attached: []string{action.ID, decision.ID, receipt.ID, transition.ID}}
}
