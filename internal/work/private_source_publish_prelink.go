package work

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
)

// This context is available only to the native publisher. It is never part of
// ReceiptedActionInput or its persisted reservation, seal or input digest.
type privatePublicationCompletion struct {
	request PrivateSourcePublishRequest
}

func (s *Store) preflightPrivatePublicationPrelink(request PrivateSourcePublishRequest, authorizedAt time.Time) error {
	w, err := s.Get(request.WorkID)
	if err != nil {
		return err
	}
	for _, ref := range w.ActionIntentIDs {
		if ref == request.Action.ID {
			return s.validatePrivatePublicationPrelink(w, request, authorizedAt)
		}
	}
	if w.PolicyDecisionID != "" {
		var decision envelope.PolicyDecision
		if err := s.readEventJSON(w.PolicyDecisionID, &decision); err != nil {
			return err
		}
		if decision.ActionIntentID == request.Action.ID {
			return fmt.Errorf("publication policy is linked without its action")
		}
	}
	events, err := s.admissionEventsFor(w.ID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if len(event.Attached) > 0 && event.Attached[0] == request.Action.ID && strings.HasPrefix(event.Note, "action recorded: ") {
			return fmt.Errorf("publication pre-admission action reference disappeared")
		}
	}
	return nil
}

// RecordAction changes only these two Work fields. Rewind that one authenticated
// pre-admission event and compare the entire preimage with the preceding native
// lifecycle anchor. Missing anchors and unexplained later changes fail closed.
func (s *Store) validatePrivatePublicationPrelink(w envelope.WorkEnvelope, request PrivateSourcePublishRequest, authorizedAt time.Time) error {
	events, err := s.admissionEventsFor(w.ID)
	if err != nil {
		return err
	}
	return s.validatePrivatePublicationPrelinkEvents(w, request, authorizedAt, events)
}

func (s *Store) validatePrivatePublicationPrelinkEvents(w envelope.WorkEnvelope, request PrivateSourcePublishRequest, authorizedAt time.Time, events []Event) error {
	if w.ID != request.WorkID || w.State != "running" || len(w.ActionIntentIDs) == 0 || w.ActionIntentIDs[len(w.ActionIntentIDs)-1] != request.Action.ID {
		return fmt.Errorf("publication is not the latest running Work action")
	}
	count := 0
	for _, ref := range w.ActionIntentIDs {
		if ref == request.Action.ID {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("publication action must have one Work reference")
	}
	expectedAction, err := privatePublishAction(w, request)
	if err != nil || !reflect.DeepEqual(expectedAction, request.Action) {
		return fmt.Errorf("publication pre-admission action changed")
	}
	var baseline envelope.PolicyDecision
	if err := s.readEventJSON(w.PolicyDecisionID, &baseline); err != nil {
		return fmt.Errorf("read publication baseline policy: %w", err)
	}
	if err := baseline.Validate(); err != nil || baseline.ID != w.PolicyDecisionID || baseline.IDs.WorkID != w.ID || baseline.ActionIntentID != request.Action.ID || !policy.Allowed(baseline) {
		return fmt.Errorf("publication baseline policy is invalid, denied or foreign")
	}
	baselineAt, err := time.Parse(time.RFC3339Nano, baseline.CreatedAt)
	if err != nil || baselineAt.After(authorizedAt) {
		return fmt.Errorf("publication baseline policy postdates exact authorization")
	}
	if len(events) < 2 {
		return fmt.Errorf("publication baseline lifecycle provenance unavailable")
	}
	linked, anchor := events[len(events)-1], events[len(events)-2]
	linkedNanos, err := strconv.ParseInt(linked.TS, 10, 64)
	linkedAt := time.Unix(0, linkedNanos)
	if err != nil || ts(linkedAt) != linked.TS || linkedAt.After(authorizedAt) || linked.From != "running" || linked.To != "running" ||
		linked.Note != "action recorded: "+request.Action.Action+" (policy "+baseline.Outcome+")" ||
		!reflect.DeepEqual(linked.Attached, []string{request.Action.ID, baseline.ID}) {
		return fmt.Errorf("publication baseline is not the exact pre-admission lifecycle event")
	}
	if anchor.To != "running" || anchor.WorkSHA256 == "" || anchor.PreviousWorkSHA256 == "" {
		return fmt.Errorf("publication baseline has no preceding native Work preimage")
	}
	previousPolicy := ""
	for _, ref := range anchor.Attached {
		if strings.HasPrefix(ref, "pol_") {
			if previousPolicy != "" {
				return fmt.Errorf("publication Work anchor has ambiguous policies")
			}
			previousPolicy = ref
		}
	}
	if previousPolicy == "" {
		return fmt.Errorf("publication Work anchor has no policy")
	}
	previous := w
	previous.ActionIntentIDs = append([]string(nil), w.ActionIntentIDs[:len(w.ActionIntentIDs)-1]...)
	previous.PolicyDecisionID = previousPolicy
	preimage, err := workEnvelopeSHA256(previous)
	if err != nil || preimage != anchor.WorkSHA256 {
		return fmt.Errorf("publication Work differs from its native pre-admission preimage")
	}
	return nil
}

// A pre-admission RecordAction event is not a completion event. Preserve it in
// history, while excluding exactly that authenticated event from the receiver's
// completion-conflict view. Every other event remains subject to its normal guard.
func (s *Store) privatePublicationCompletionEvents(w envelope.WorkEnvelope, request PrivateSourcePublishRequest, in ReceiptedActionInput, authorizedAt time.Time, expected Event, events []Event) ([]Event, error) {
	index := -1
	for i, event := range events {
		if len(event.Attached) > 0 && event.Attached[0] == request.Action.ID && strings.HasPrefix(event.Note, "action recorded: ") {
			if index >= 0 {
				return nil, fmt.Errorf("private publication has duplicate pre-admission events")
			}
			index = i
		}
	}
	if index < 0 {
		return events, nil
	}
	baseline := events[index]
	if len(baseline.Attached) != 2 || in.PreEvaluatedDecision == nil ||
		(w.PolicyDecisionID != baseline.Attached[1] && w.PolicyDecisionID != in.PreEvaluatedDecision.ID) {
		return nil, fmt.Errorf("private publication pre-admission policy link changed")
	}
	if len(events) > index+2 || (len(events) == index+2 && !reflect.DeepEqual(events[index+1], expected)) {
		return nil, fmt.Errorf("private publication has unexplained later lifecycle events")
	}
	prelinked := w
	prelinked.PolicyDecisionID = baseline.Attached[1]
	if err := s.validatePrivatePublicationPrelinkEvents(prelinked, request, authorizedAt, events[:index+1]); err != nil {
		return nil, err
	}
	filtered := append([]Event(nil), events[:index]...)
	return append(filtered, events[index+1:]...), nil
}

func (s *Store) privatePublicationCompletedEventView(w envelope.WorkEnvelope, request PrivateSourcePublishRequest, in ReceiptedActionInput, receipt envelope.Receipt, transition envelope.Transition, authorized string, events []Event) ([]Event, error) {
	hasPreAdmission := false
	for _, event := range events {
		if len(event.Attached) > 0 && event.Attached[0] == request.Action.ID && strings.HasPrefix(event.Note, "action recorded: ") {
			hasPreAdmission = true
			break
		}
	}
	if !hasPreAdmission {
		return events, nil
	}
	authorizedAt, err := time.Parse(time.RFC3339Nano, authorized)
	if err != nil {
		return nil, err
	}
	if err := s.validatePrivatePublicationCompletion(w, request, in, authorizedAt); err != nil {
		return nil, err
	}
	completedAt, err := time.Parse(time.RFC3339Nano, receipt.CreatedAt)
	if err != nil {
		return nil, err
	}
	expected := receiptedActionEvent(w, in.Action, *in.PreEvaluatedDecision, receipt, transition, completedAt)
	return s.privatePublicationCompletionEvents(w, request, in, authorizedAt, expected, events)
}

func (s *Store) validatePrivatePublicationCompletion(w envelope.WorkEnvelope, request PrivateSourcePublishRequest, in ReceiptedActionInput, authorizedAt time.Time) error {
	expectedAction, err := privatePublishAction(w, request)
	if err != nil || request.SchemaVersion != PrivateSourcePublishSchema || request.WorkID != w.ID || w.State != "running" ||
		!reflect.DeepEqual(expectedAction, request.Action) || !reflect.DeepEqual(in.Action, request.Action) || in.PreEvaluatedDecision == nil {
		return fmt.Errorf("private publication completion request or action changed")
	}
	decision := *in.PreEvaluatedDecision
	if decision.StandingGrantID != "" || !policy.Allowed(decision) {
		return fmt.Errorf("private publication completion requires its original exact decision")
	}
	if err := validatePreEvaluatedReceiptedActionAt(request.Action, decision, authorizedAt); err != nil {
		return err
	}
	if err := policy.NewExactActionApprovalStore(s.home).ValidateConsumedDecision(request.Action, decision); err != nil {
		return fmt.Errorf("private publication consumed approval: %w", err)
	}
	if len(in.ProofRefs) != 2 || in.ProofRefs[0] != request.ProofRef || !strings.HasPrefix(in.ProofRefs[1], "evidence:artifact:sha256:") {
		return fmt.Errorf("private publication completion has no exact source observation")
	}
	canonicalHome, err := filepath.EvalSymlinks(s.home)
	if err != nil {
		return err
	}
	reader, err := home.OpenArtifactReader(canonicalHome)
	if err != nil {
		return err
	}
	body, err := reader.GetPrivateBounded(strings.TrimPrefix(in.ProofRefs[1], "evidence:"), 1<<20)
	if err != nil {
		return fmt.Errorf("read private publication observation: %w", err)
	}
	var observation privatePublicationObservation
	if err := publicReviewStrictJSON(body, &observation); err != nil {
		return fmt.Errorf("decode private publication observation: %w", err)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, observation.ObservedAt)
	if err != nil || observedAt.Before(authorizedAt) || observation.SchemaVersion != "compound.private_source_publication_observation.v1" ||
		observation.ActionID != request.Action.ID || observation.RemoteSHA != request.Source.HeadSHA ||
		!observation.RemoteConfirmed || !observation.TrackingConfirmed || !samePrivateSourceDestination(request.Destination, observation.Destination) {
		return fmt.Errorf("private publication observation is unconfirmed, foreign or changed")
	}
	if err := validatePrivateSourceDestination(observation.Destination, observedAt); err != nil {
		return fmt.Errorf("private publication observed destination: %w", err)
	}
	expected := ReceiptedActionInput{
		Action: request.Action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running", Outcome: "completed",
		Changed: request.ExpectedRemoteSHA != request.Source.HeadSHA, Summary: privatePublicationSuccessSummary,
		MoveType: "outward_delta", ProofRefs: in.ProofRefs,
		BeforeRefs: []string{"git:" + request.Destination.FullName + "@" + request.ExpectedRemoteSHA},
		AfterRefs:  []string{"git:" + request.Destination.FullName + "@" + request.Source.HeadSHA},
	}
	if !reflect.DeepEqual(in, expected) {
		return fmt.Errorf("private publication sealed outcome or preimages changed")
	}
	return nil
}
