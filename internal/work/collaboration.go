package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

const (
	CollaborationSchemaVersion       = "compound.work_collaboration_event.v0.2"
	CollaborationLegacySchemaVersion = "compound.work_collaboration_event.v0.1"
	CollaborationKindObserved        = "work.collaboration.observed"
)

const (
	CollaborationDeliverySilentState    = "silent_state"
	CollaborationDeliveryNextBoundary   = "next_boundary"
	CollaborationDeliveryInterrupt      = "interrupt"
	CollaborationDeliveryHumanAttention = "human_attention"
)

// ErrCollaborationAuthorityResolved distinguishes current resolution from
// malformed or missing governed evidence during replay.
var ErrCollaborationAuthorityResolved = errors.New("collaboration operator authority is resolved")

// ErrCollaborationInterruptInactive distinguishes a once-governed exact
// interrupt whose run has since terminalized from malformed or missing
// interrupt evidence during replay.
var ErrCollaborationInterruptInactive = errors.New("collaboration interrupt run is inactive")

// CollaborationStateDelta is deliberately structural and bounded. Full prose
// remains in BodyRef; delivery cards need only the changed coordinate.
type CollaborationStateDelta struct {
	Kind       string `json:"kind"`
	SubjectRef string `json:"subject_ref"`
	Before     string `json:"before,omitempty"`
	After      string `json:"after,omitempty"`
}

// CollaborationAffects is the relevance index carried with an event. A
// receiver is eligible only when at least one coordinate intersects its live
// work context.
type CollaborationAffects struct {
	Actors         []string `json:"actors,omitempty"`
	WorkIDs        []string `json:"work_ids,omitempty"`
	RunAttemptIDs  []string `json:"run_attempt_ids,omitempty"`
	ScopeRefs      []string `json:"scope_refs,omitempty"`
	DependencyRefs []string `json:"dependency_refs,omitempty"`
	PolicyRefs     []string `json:"policy_refs,omitempty"`
	BaseRevisions  []string `json:"base_revisions,omitempty"`
	ResourceRefs   []string `json:"resource_refs,omitempty"`
}

// CollaborationInput is metadata for one content-free multiplayer observation.
// InlineBody exists only as a fail-closed API guard: callers must preserve the
// body through a governed artifact door and pass BodyRef instead.
type CollaborationInput struct {
	Actor           string
	IdempotencyKey  string
	SessionID       string
	RunAttemptID    string
	TargetActor     string
	CausalParentIDs []string
	BodyRef         string
	Visibility      string
	DeliveryClass   string
	Delta           CollaborationStateDelta
	Affects         CollaborationAffects
	InlineBody      string
}

// CollaborationEvent is evidence attached to one WorkEnvelope lifecycle. It
// intentionally has no message, prompt, transcript, or inline body field.
type CollaborationEvent struct {
	SchemaVersion    string                   `json:"schema_version"`
	ID               string                   `json:"id"`
	Kind             string                   `json:"kind"`
	CreatedAt        string                   `json:"created_at"`
	WorkID           string                   `json:"work_id"`
	Actor            string                   `json:"actor"`
	IdempotencyKey   string                   `json:"idempotency_key"`
	SessionID        string                   `json:"session_id,omitempty"`
	RunAttemptID     string                   `json:"run_attempt_id,omitempty"`
	TargetActor      string                   `json:"target_actor,omitempty"`
	CausalParentIDs  []string                 `json:"causal_parent_ids,omitempty"`
	BodyRef          string                   `json:"body_ref"`
	Visibility       string                   `json:"visibility"`
	DeliveryClass    string                   `json:"delivery_class,omitempty"`
	Delta            *CollaborationStateDelta `json:"delta,omitempty"`
	Affects          *CollaborationAffects    `json:"affects,omitempty"`
	Privacy          string                   `json:"privacy"`
	ActionIntentID   string                   `json:"action_intent_id"`
	PolicyDecisionID string                   `json:"policy_decision_id"`
}

type CollaborationResult struct {
	SchemaVersion string                `json:"schema_version"`
	Created       bool                  `json:"created"`
	Event         CollaborationEvent    `json:"event"`
	Work          envelope.WorkEnvelope `json:"work"`
}

// CollaborationBodyRequest authorizes the explicit on-demand body door. The
// event target, work, actor, active run attempt, and owned native session must
// all agree.
type CollaborationBodyRequest struct {
	WorkID       string
	EventID      string
	Actor        string
	RunAttemptID string
	SessionID    string
}

type CollaborationBodyResult struct {
	EventID string `json:"event_id"`
	BodyRef string `json:"body_ref"`
	Body    []byte `json:"body"`
}

// CollaborationRecipientContext is the recipient-side coordinate set read by
// CollaborationEventRelevant. The delivery projection (internal/hookplan) and
// the explicit body door (this package) both project their recipient onto
// this one shape, so the two surfaces cannot disagree about which event
// belongs to whom.
type CollaborationRecipientContext struct {
	Actor          string
	WorkID         string
	RunAttemptID   string
	ScopeRefs      []string
	DependencyRefs []string
	PolicyRefs     []string
	BaseRevisions  []string
	ResourceRefs   []string
}

// RelevanceContext projects a resolved recipient onto the shared predicate
// shape. It is reference-only: no coordinate is derived, widened, or dropped.
func (r CollaborationRecipientResolution) RelevanceContext() CollaborationRecipientContext {
	return CollaborationRecipientContext{
		Actor:          strings.TrimSpace(r.Actor),
		WorkID:         strings.TrimSpace(r.WorkID),
		RunAttemptID:   strings.TrimSpace(r.RunAttemptID),
		ScopeRefs:      append([]string{}, r.ScopeRefs...),
		DependencyRefs: append([]string{}, r.DependencyRefs...),
		PolicyRefs:     append([]string{}, r.PolicyRefs...),
		BaseRevisions:  append([]string{}, r.BaseRevisions...),
		ResourceRefs:   append([]string{}, r.ResourceRefs...),
	}
}

// CollaborationEventRelevant is the ONE recipient relevance rule.
//
// Both the delivery projection and the explicit body door call it, so a card
// that was delivered and a body that can be fetched can never disagree about
// whether the event concerns this recipient. Before this rule existed the two
// surfaces disagreed by construction: delivery admitted an explicitly
// relevant cross-work event while the body door demanded the event's own
// work, so a delivered card could name a body the recipient was refused.
//
// The rule is conjunctive over every dimension the event declares, and
// disjunctive only at the end:
//
//   - TargetActor, when set, must equal the recipient actor.
//   - Affects.Actors, when non-empty, must contain the recipient actor.
//   - Every other non-empty Affects dimension must intersect the recipient's
//     own coordinate for that dimension.
//   - An interrupt additionally requires the recipient's exact live run
//     attempt and either the recipient's work or that work in Affects.WorkIDs.
//
// Only then does the event count as relevant: it either belongs to the
// recipient's own work, or it names the recipient explicitly through one of
// the affected coordinates. That last clause is what makes a cross-work
// handoff addressable without making every work visible to every other.
func CollaborationEventRelevant(event CollaborationEvent, recipient CollaborationRecipientContext) bool {
	affects := collaborationAffectsValue(event)
	actor := strings.TrimSpace(recipient.Actor)
	workID := strings.TrimSpace(recipient.WorkID)
	runAttemptID := strings.TrimSpace(recipient.RunAttemptID)

	if target := strings.TrimSpace(event.TargetActor); target != "" && target != actor {
		return false
	}
	if len(affects.Actors) > 0 && !collaborationRefsIntersect(affects.Actors, []string{actor}) {
		return false
	}
	for _, dimension := range []struct {
		affected  []string
		recipient []string
	}{
		{affected: affects.WorkIDs, recipient: []string{workID}},
		{affected: affects.RunAttemptIDs, recipient: []string{runAttemptID}},
		{affected: affects.ScopeRefs, recipient: recipient.ScopeRefs},
		{affected: affects.DependencyRefs, recipient: recipient.DependencyRefs},
		{affected: affects.PolicyRefs, recipient: recipient.PolicyRefs},
		{affected: affects.BaseRevisions, recipient: recipient.BaseRevisions},
		{affected: affects.ResourceRefs, recipient: recipient.ResourceRefs},
	} {
		if len(dimension.affected) > 0 && !collaborationRefsIntersect(dimension.affected, dimension.recipient) {
			return false
		}
	}
	switch collaborationEventDeliveryClass(event) {
	case CollaborationDeliveryInterrupt:
		return runAttemptID != "" &&
			collaborationContains(affects.RunAttemptIDs, runAttemptID) &&
			workID != "" &&
			(workID == event.WorkID || collaborationContains(affects.WorkIDs, workID))
	}
	return collaborationContains([]string{workID}, event.WorkID) ||
		collaborationRefsIntersect([]string{workID}, affects.WorkIDs) ||
		(runAttemptID != "" && collaborationContains(affects.RunAttemptIDs, runAttemptID)) ||
		collaborationRefsIntersect(recipient.ScopeRefs, affects.ScopeRefs) ||
		collaborationRefsIntersect(recipient.DependencyRefs, affects.DependencyRefs) ||
		collaborationRefsIntersect(recipient.PolicyRefs, affects.PolicyRefs) ||
		collaborationRefsIntersect(recipient.BaseRevisions, affects.BaseRevisions) ||
		collaborationRefsIntersect(recipient.ResourceRefs, affects.ResourceRefs)
}

// collaborationEventDeliveryClass is the effective delivery class of an event.
// A legacy (v0.1) event carries no class; it is treated as a non-interrupt
// observation, exactly as the delivery projection treats it.
func collaborationEventDeliveryClass(event CollaborationEvent) string {
	if strings.TrimSpace(event.DeliveryClass) == "" {
		return CollaborationDeliveryNextBoundary
	}
	return event.DeliveryClass
}

var appendCollaborationLifecycle = func(s *Store, event Event) error {
	return s.appendEventControlled(event, nil, nil)
}

// RecordCollaboration records one governed, work-scoped, content-free
// collaboration observation. The idempotency key is scoped to the work; a
// retry recovers and finishes any missing lifecycle attachment.
func (s *Store) RecordCollaboration(workID string, in CollaborationInput, now time.Time) (CollaborationResult, error) {
	var result CollaborationResult
	err := s.withLifecycleMutation(func() error {
		var err error
		result, err = s.recordCollaborationLocked(workID, in, now)
		return err
	})
	return result, err
}

func (s *Store) recordCollaborationLocked(workID string, in CollaborationInput, now time.Time) (CollaborationResult, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return CollaborationResult{}, fmt.Errorf("collaboration work_id is required")
	}
	in = normalizeCollaborationInput(in)
	if in.Delta.Kind == "" {
		in.Delta = CollaborationStateDelta{Kind: "observation", SubjectRef: workID, After: "observed"}
	}
	if in.TargetActor != "" {
		in.Affects.Actors = mergeCollaborationRefs(in.Affects.Actors, []string{in.TargetActor})
	}
	if err := validateCollaborationInput(in); err != nil {
		return CollaborationResult{}, err
	}

	w, err := s.recoverPendingClaimBeforeMutation(workID)
	if err != nil {
		return CollaborationResult{}, err
	}
	if terminalStates[w.State] {
		return CollaborationResult{}, fmt.Errorf("work %s %s (terminal); cannot record collaboration", workID, w.State)
	}
	lease, err := s.readLease(workID)
	if err != nil {
		return CollaborationResult{}, fmt.Errorf("collaboration requires a claimed work lease: %w", err)
	}
	if strings.TrimSpace(lease.By) != in.Actor {
		return CollaborationResult{}, fmt.Errorf("cannot record collaboration for %s: lease held by %q, actor is %q", workID, lease.By, in.Actor)
	}
	artifactStore, err := home.NewArtifactStore(s.home)
	if err != nil {
		return CollaborationResult{}, fmt.Errorf("collaboration body_ref store: %w", err)
	}
	if _, err := artifactStore.Verify(in.BodyRef); err != nil {
		return CollaborationResult{}, fmt.Errorf("collaboration body_ref %s does not resolve to verified Compound custody: %w", in.BodyRef, err)
	}
	if in.RunAttemptID != "" {
		attempt, err := s.runAttempts().Get(in.RunAttemptID)
		if err != nil {
			return CollaborationResult{}, fmt.Errorf("collaboration run_attempt_id %s: %w", in.RunAttemptID, err)
		}
		if attempt.WorkID != workID {
			return CollaborationResult{}, fmt.Errorf("collaboration run attempt %s belongs to work %s, not %s", in.RunAttemptID, attempt.WorkID, workID)
		}
	}
	if err := s.validateCollaborationClassEvidence(workID, in); err != nil {
		return CollaborationResult{}, err
	}

	eventID := collaborationEventID(workID, in.IdempotencyKey)
	if err := s.validateCollaborationCausalParents(workID, eventID, in.CausalParentIDs); err != nil {
		return CollaborationResult{}, err
	}
	semanticDigest := collaborationSemanticDigest(in)
	actionID := collaborationActionID(eventID)
	action, err := s.loadOrBuildCollaborationAction(actionID, eventID, semanticDigest, w, in, now)
	if err != nil {
		return CollaborationResult{}, err
	}
	decisionID := collaborationPolicyID(eventID)
	decision, err := s.loadOrBuildCollaborationDecision(decisionID, action, now)
	if err != nil {
		return CollaborationResult{}, err
	}
	if !policy.Allowed(decision) {
		return CollaborationResult{}, fmt.Errorf("cannot record collaboration %s: policy %s", eventID, decision.Outcome)
	}

	if err := s.writeEventJSON(action.ID, &action); err != nil {
		return CollaborationResult{}, err
	}
	if err := s.writeEventJSON(decision.ID, &decision); err != nil {
		return CollaborationResult{}, err
	}

	collaboration := CollaborationEvent{
		SchemaVersion:    CollaborationSchemaVersion,
		ID:               eventID,
		Kind:             CollaborationKindObserved,
		CreatedAt:        action.CreatedAt,
		WorkID:           workID,
		Actor:            in.Actor,
		IdempotencyKey:   in.IdempotencyKey,
		SessionID:        in.SessionID,
		RunAttemptID:     in.RunAttemptID,
		TargetActor:      in.TargetActor,
		CausalParentIDs:  append([]string{}, in.CausalParentIDs...),
		BodyRef:          in.BodyRef,
		Visibility:       in.Visibility,
		DeliveryClass:    in.DeliveryClass,
		Delta:            &in.Delta,
		Affects:          &in.Affects,
		Privacy:          w.Privacy,
		ActionIntentID:   action.ID,
		PolicyDecisionID: decision.ID,
	}
	if err := collaboration.Validate(); err != nil {
		return CollaborationResult{}, err
	}

	var stored CollaborationEvent
	switch err := s.readEventJSON(eventID, &stored); {
	case err == nil:
		if !collaborationEventsEquivalent(stored, collaboration) {
			return CollaborationResult{}, fmt.Errorf("collaboration idempotency key %q conflicts with existing event %s", in.IdempotencyKey, eventID)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := s.writeEventJSON(eventID, &collaboration); err != nil {
			return CollaborationResult{}, err
		}
	default:
		return CollaborationResult{}, err
	}

	lifecycle, err := s.eventsFor(workID)
	if err != nil {
		return CollaborationResult{}, err
	}
	attachments := countAttached(lifecycle, eventID)
	if attachments > 1 {
		return CollaborationResult{}, fmt.Errorf("collaboration event %s is attached %d times", eventID, attachments)
	}
	if attachments == 1 {
		return CollaborationResult{
			SchemaVersion: CollaborationSchemaVersion,
			Created:       false,
			Event:         collaboration,
			Work:          w,
		}, nil
	}

	if !oneOf(action.ID, w.ActionIntentIDs) {
		w.ActionIntentIDs = append(w.ActionIntentIDs, action.ID)
		w.PolicyDecisionID = decision.ID
		if err := s.writeJSON(s.workPath(workID), &w); err != nil {
			return CollaborationResult{}, err
		}
	}
	if err := appendCollaborationLifecycle(s, Event{
		TS:       ts(now),
		WorkID:   workID,
		From:     w.State,
		To:       w.State,
		By:       in.Actor,
		Note:     "collaboration observation recorded",
		Attached: []string{action.ID, decision.ID, collaboration.ID},
	}); err != nil {
		return CollaborationResult{}, err
	}
	return CollaborationResult{
		SchemaVersion: CollaborationSchemaVersion,
		Created:       true,
		Event:         collaboration,
		Work:          w,
	}, nil
}

func normalizeCollaborationInput(in CollaborationInput) CollaborationInput {
	in.Actor = strings.TrimSpace(in.Actor)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.SessionID = strings.TrimSpace(in.SessionID)
	in.RunAttemptID = strings.TrimSpace(in.RunAttemptID)
	in.TargetActor = strings.TrimSpace(in.TargetActor)
	in.BodyRef = strings.TrimSpace(in.BodyRef)
	in.Visibility = strings.TrimSpace(in.Visibility)
	if in.Visibility == "" {
		in.Visibility = "work"
	}
	in.DeliveryClass = strings.TrimSpace(in.DeliveryClass)
	if in.DeliveryClass == "" {
		in.DeliveryClass = CollaborationDeliveryNextBoundary
	}
	in.Delta.Kind = strings.TrimSpace(in.Delta.Kind)
	in.Delta.SubjectRef = strings.TrimSpace(in.Delta.SubjectRef)
	in.Delta.Before = strings.TrimSpace(in.Delta.Before)
	in.Delta.After = strings.TrimSpace(in.Delta.After)
	in.Affects.Actors = normalizeCollaborationRefs(in.Affects.Actors)
	in.Affects.WorkIDs = normalizeCollaborationRefs(in.Affects.WorkIDs)
	in.Affects.RunAttemptIDs = normalizeCollaborationRefs(in.Affects.RunAttemptIDs)
	in.Affects.ScopeRefs = normalizeCollaborationRefs(in.Affects.ScopeRefs)
	in.Affects.DependencyRefs = normalizeCollaborationRefs(in.Affects.DependencyRefs)
	in.Affects.PolicyRefs = normalizeCollaborationRefs(in.Affects.PolicyRefs)
	in.Affects.BaseRevisions = normalizeCollaborationRefs(in.Affects.BaseRevisions)
	in.Affects.ResourceRefs = normalizeCollaborationRefs(in.Affects.ResourceRefs)
	for i := range in.CausalParentIDs {
		in.CausalParentIDs[i] = strings.TrimSpace(in.CausalParentIDs[i])
	}
	return in
}

func validateCollaborationInput(in CollaborationInput) error {
	if !validCollaborationToken(in.Actor) {
		return fmt.Errorf("collaboration actor must be a bounded identifier")
	}
	if !validCollaborationToken(in.IdempotencyKey) {
		return fmt.Errorf("collaboration idempotency_key must be a bounded identifier")
	}
	if strings.TrimSpace(in.InlineBody) != "" {
		return fmt.Errorf("inline collaboration content is forbidden; preserve it and pass --body-ref")
	}
	if in.SessionID != "" && !validCollaborationToken(in.SessionID) {
		return fmt.Errorf("collaboration session_id must be a bounded identifier")
	}
	if in.TargetActor != "" && !validCollaborationToken(in.TargetActor) {
		return fmt.Errorf("collaboration target_actor must be a bounded identifier")
	}
	if !validCollaborationBodyRef(in.BodyRef) {
		return fmt.Errorf("collaboration body_ref must be artifact:sha256:<64 lowercase hex>")
	}
	if in.Visibility != "work" {
		return fmt.Errorf("collaboration visibility must be %q in v1", "work")
	}
	if !oneOf(in.DeliveryClass, []string{
		CollaborationDeliverySilentState,
		CollaborationDeliveryNextBoundary,
		CollaborationDeliveryInterrupt,
		CollaborationDeliveryHumanAttention,
	}) {
		return fmt.Errorf("collaboration delivery_class %q is invalid", in.DeliveryClass)
	}
	if !validCollaborationToken(in.Delta.Kind) || !validCollaborationToken(in.Delta.SubjectRef) {
		return fmt.Errorf("collaboration delta kind and subject_ref must be bounded identifiers")
	}
	if in.Delta.Before == "" && in.Delta.After == "" {
		return fmt.Errorf("collaboration delta requires before or after")
	}
	if (in.Delta.Before != "" && !validCollaborationToken(in.Delta.Before)) ||
		(in.Delta.After != "" && !validCollaborationToken(in.Delta.After)) {
		return fmt.Errorf("collaboration delta before and after must be bounded identifiers")
	}
	for _, item := range []struct {
		name   string
		values []string
	}{
		{name: "actors", values: in.Affects.Actors},
		{name: "work_ids", values: in.Affects.WorkIDs},
		{name: "run_attempt_ids", values: in.Affects.RunAttemptIDs},
		{name: "scope_refs", values: in.Affects.ScopeRefs},
		{name: "dependency_refs", values: in.Affects.DependencyRefs},
		{name: "policy_refs", values: in.Affects.PolicyRefs},
		{name: "base_revisions", values: in.Affects.BaseRevisions},
		{name: "resource_refs", values: in.Affects.ResourceRefs},
	} {
		for _, value := range item.values {
			if !validCollaborationToken(value) {
				return fmt.Errorf("collaboration affects.%s must contain bounded identifiers", item.name)
			}
		}
	}
	switch in.DeliveryClass {
	case CollaborationDeliveryInterrupt:
		if len(in.Affects.RunAttemptIDs) != 1 ||
			in.Delta.Kind != "run_attempt" ||
			in.Delta.SubjectRef != in.Affects.RunAttemptIDs[0] ||
			in.Delta.After != "invalidated" ||
			len(collaborationPolicyDecisionRefs(in.Affects.PolicyRefs)) != 1 {
			return fmt.Errorf("interrupt requires one exact invalidated run_attempt and one governed policy decision")
		}
	case CollaborationDeliveryHumanAttention:
		if in.Delta.Kind != "authority" || in.Delta.After != "unresolved" ||
			!oneOf(policy.OperatorStateAuthorityRef, in.Affects.PolicyRefs) ||
			len(collaborationPolicyDecisionRefs(in.Affects.PolicyRefs)) != 1 {
			return fmt.Errorf("human_attention requires unresolved operator authority evidence routed to %s", policy.OperatorStateAuthorityRef)
		}
	}
	return nil
}

func normalizeCollaborationRefs(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func mergeCollaborationRefs(left, right []string) []string {
	return normalizeCollaborationRefs(append(append([]string{}, left...), right...))
}

func (e CollaborationEvent) Validate() error {
	switch e.SchemaVersion {
	case CollaborationLegacySchemaVersion:
		if e.DeliveryClass != "" || e.Delta != nil || e.Affects != nil {
			return fmt.Errorf("legacy collaboration event cannot carry v0.2 delivery metadata")
		}
	case CollaborationSchemaVersion:
		input := CollaborationInput{
			Actor:          e.Actor,
			IdempotencyKey: e.IdempotencyKey,
			SessionID:      e.SessionID,
			RunAttemptID:   e.RunAttemptID,
			TargetActor:    e.TargetActor,
			BodyRef:        e.BodyRef,
			Visibility:     e.Visibility,
			DeliveryClass:  e.DeliveryClass,
		}
		if e.Delta == nil || e.Affects == nil {
			return fmt.Errorf("current collaboration event requires delta and affects")
		}
		input.Delta = *e.Delta
		input.Affects = *e.Affects
		if err := validateCollaborationInput(input); err != nil {
			return err
		}
	default:
		return fmt.Errorf("collaboration schema_version = %q, want %q or %q", e.SchemaVersion, CollaborationLegacySchemaVersion, CollaborationSchemaVersion)
	}
	if !strings.HasPrefix(e.ID, "wco_") {
		return fmt.Errorf("collaboration id %q must start with wco_", e.ID)
	}
	if e.Kind != CollaborationKindObserved {
		return fmt.Errorf("collaboration kind = %q, want %q", e.Kind, CollaborationKindObserved)
	}
	if _, err := time.Parse(time.RFC3339, e.CreatedAt); err != nil {
		return fmt.Errorf("collaboration created_at invalid: %w", err)
	}
	if strings.TrimSpace(e.WorkID) == "" || !validCollaborationToken(e.Actor) || !validCollaborationToken(e.IdempotencyKey) {
		return fmt.Errorf("collaboration work_id, actor, and idempotency_key must be bounded identifiers")
	}
	if e.SessionID != "" && !validCollaborationToken(e.SessionID) {
		return fmt.Errorf("collaboration session_id must be a bounded identifier")
	}
	if e.TargetActor != "" && !validCollaborationToken(e.TargetActor) {
		return fmt.Errorf("collaboration target_actor must be a bounded identifier")
	}
	if !validCollaborationBodyRef(e.BodyRef) {
		return fmt.Errorf("collaboration body_ref must be artifact:sha256:<64 lowercase hex>")
	}
	if e.Visibility != "work" {
		return fmt.Errorf("collaboration visibility must be %q", "work")
	}
	if strings.TrimSpace(e.Privacy) == "" {
		return fmt.Errorf("collaboration privacy is required")
	}
	if !strings.HasPrefix(e.ActionIntentID, "act_") || !strings.HasPrefix(e.PolicyDecisionID, "pol_") {
		return fmt.Errorf("collaboration action_intent_id and policy_decision_id are required")
	}
	return nil
}

func validCollaborationBodyRef(ref string) bool {
	const prefix = "artifact:sha256:"
	if !strings.HasPrefix(ref, prefix) {
		return false
	}
	digest := strings.TrimPrefix(ref, prefix)
	if len(digest) != 64 {
		return false
	}
	for _, r := range digest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validCollaborationToken(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if i > 0 && strings.ContainsRune("._:/@-", r) {
			continue
		}
		return false
	}
	return true
}

func collaborationEventID(workID, key string) string {
	sum := sha256.Sum256([]byte(workID + "\x00" + key))
	return "wco_" + hex.EncodeToString(sum[:])[:16]
}

func collaborationActionID(eventID string) string {
	sum := sha256.Sum256([]byte("work.collaboration.record\x00" + eventID))
	return "act_" + hex.EncodeToString(sum[:])[:16]
}

func collaborationPolicyID(eventID string) string {
	sum := sha256.Sum256([]byte("work.collaboration.policy\x00" + eventID))
	return "pol_" + hex.EncodeToString(sum[:])[:16]
}

func collaborationSemanticDigest(in CollaborationInput) string {
	body, _ := json.Marshal(struct {
		SessionID       string                  `json:"session_id,omitempty"`
		RunAttemptID    string                  `json:"run_attempt_id,omitempty"`
		TargetActor     string                  `json:"target_actor,omitempty"`
		CausalParentIDs []string                `json:"causal_parent_ids,omitempty"`
		BodyRef         string                  `json:"body_ref"`
		Visibility      string                  `json:"visibility"`
		DeliveryClass   string                  `json:"delivery_class"`
		Delta           CollaborationStateDelta `json:"delta"`
		Affects         CollaborationAffects    `json:"affects"`
	}{
		SessionID:       in.SessionID,
		RunAttemptID:    in.RunAttemptID,
		TargetActor:     in.TargetActor,
		CausalParentIDs: in.CausalParentIDs,
		BodyRef:         in.BodyRef,
		Visibility:      in.Visibility,
		DeliveryClass:   in.DeliveryClass,
		Delta:           in.Delta,
		Affects:         in.Affects,
	})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func collaborationPolicyDecisionRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref, "pol_") {
			out = append(out, ref)
		}
	}
	return out
}

func (s *Store) validateCollaborationClassEvidence(workID string, in CollaborationInput) error {
	switch in.DeliveryClass {
	case CollaborationDeliveryInterrupt:
		attemptID := in.Affects.RunAttemptIDs[0]
		attempt, err := s.runAttempts().Get(attemptID)
		if err != nil {
			return fmt.Errorf("interrupt run attempt %s: %w", attemptID, err)
		}
		if attempt.WorkID != workID || attempt.ActionIntentID == "" {
			return fmt.Errorf("interrupt requires exact active run/action evidence for %s", attemptID)
		}
		if runattempt.IsTerminal(attempt.Status) {
			return fmt.Errorf("%w: %s", ErrCollaborationInterruptInactive, attemptID)
		}
		var action envelope.ActionIntent
		if err := s.readEventJSON(attempt.ActionIntentID, &action); err != nil {
			return fmt.Errorf("interrupt action evidence %s: %w", attempt.ActionIntentID, err)
		}
		var decision envelope.PolicyDecision
		policyRef := collaborationPolicyDecisionRefs(in.Affects.PolicyRefs)[0]
		if err := s.readEventJSON(policyRef, &decision); err != nil {
			return fmt.Errorf("interrupt policy evidence %s: %w", policyRef, err)
		}
		if action.ID != attempt.ActionIntentID || action.IDs.WorkID != workID ||
			decision.IDs.WorkID != workID || decision.ActionIntentID != action.ID ||
			(decision.Outcome != "hold" && decision.Outcome != "deny") {
			return fmt.Errorf("interrupt policy %s does not invalidate active action %s", decision.ID, action.ID)
		}
	case CollaborationDeliveryHumanAttention:
		policyRef := collaborationPolicyDecisionRefs(in.Affects.PolicyRefs)[0]
		var decision envelope.PolicyDecision
		if err := s.readEventJSON(policyRef, &decision); err != nil {
			return fmt.Errorf("human_attention policy evidence %s: %w", policyRef, err)
		}
		var action envelope.ActionIntent
		if err := s.readEventJSON(decision.ActionIntentID, &action); err != nil {
			return fmt.Errorf("human_attention action evidence %s: %w", decision.ActionIntentID, err)
		}
		if action.IDs.WorkID != workID || decision.IDs.WorkID != workID ||
			(decision.Outcome != "hold" && decision.Outcome != "deny") ||
			!collaborationRequiresOperatorAuthority(action) {
			return fmt.Errorf("human_attention requires unresolved hold/deny for an operator-authority decision")
		}
		unresolved, err := policy.CurrentOperatorAuthorityUnresolved(
			s.home, workID, action.ID, decision.ID,
		)
		if err != nil {
			return fmt.Errorf("human_attention current operator authority: %w", err)
		}
		if !unresolved {
			return fmt.Errorf(
				"%w: human_attention policy %s is not currently unresolved in %s",
				ErrCollaborationAuthorityResolved, decision.ID, policy.OperatorStateAuthorityRef,
			)
		}
	}
	return nil
}

func collaborationRequiresOperatorAuthority(action envelope.ActionIntent) bool {
	if oneOf(action.Reversibility, []string{"hard", "irreversible", "none"}) {
		return true
	}
	if oneOf(action.MutationClass, []string{"public", "security", "financial", "destructive"}) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(action.Target.Kind), "authority") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(action.Capability)), "authority.") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(action.Action)), "authority_")
}

// ValidateCollaborationDeliveryEvidence rechecks class eligibility at replay
// time, so a structurally valid but ungoverned event cannot become an interrupt
// or human-attention card merely by appearing in event storage.
func (s *Store) ValidateCollaborationDeliveryEvidence(event CollaborationEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if event.SchemaVersion == CollaborationLegacySchemaVersion {
		return nil
	}
	return s.validateCollaborationClassEvidence(event.WorkID, CollaborationInput{
		Actor:         event.Actor,
		DeliveryClass: event.DeliveryClass,
		Delta:         *event.Delta,
		Affects:       *event.Affects,
	})
}

// RetrieveCollaborationBody is the production on-demand body door. It performs
// no lifecycle mutation and emits no acknowledgement or collaboration event.
//
// The request names the SOURCE work: the work that owns the event and the
// body artifact. The RECIPIENT is resolved by the same recipient resolver the
// delivery projection uses, seeded with the attempt's OWN recorded harness and
// workspace and the caller's session, and the resolved work/actor/run must
// equal what the caller claimed. The body is released only when the event is
// relevant to that recipient under the one shared relevance rule.
//
// Authority is checked once, here, through ValidateCollaborationDeliveryEvidence
// — the same recheck the delivery projection performs — so a structurally valid
// but ungoverned event cannot become readable merely by naming a live attempt.
func (s *Store) RetrieveCollaborationBody(in CollaborationBodyRequest) (CollaborationBodyResult, error) {
	in.WorkID = strings.TrimSpace(in.WorkID)
	in.EventID = strings.TrimSpace(in.EventID)
	in.Actor = strings.TrimSpace(in.Actor)
	in.RunAttemptID = strings.TrimSpace(in.RunAttemptID)
	in.SessionID = strings.TrimSpace(in.SessionID)
	if in.WorkID == "" || !strings.HasPrefix(in.EventID, "wco_") ||
		in.Actor == "" || !runattempt.Valid(in.RunAttemptID) || in.SessionID == "" {
		return CollaborationBodyResult{}, fmt.Errorf("collaboration body requires source work, event, actor, ratt_ run attempt, and native session")
	}
	var event CollaborationEvent
	if err := s.readEventJSON(in.EventID, &event); err != nil {
		return CollaborationBodyResult{}, err
	}
	if err := s.ValidateCollaborationDeliveryEvidence(event); err != nil {
		return CollaborationBodyResult{}, err
	}
	// The named source work must be the event's owner. The body is
	// source-owned; the recipient's own work is derived from the attempt.
	if event.WorkID != in.WorkID {
		return CollaborationBodyResult{}, fmt.Errorf("collaboration body request does not match event work")
	}
	attempt, err := s.runAttempts().Get(in.RunAttemptID)
	if err != nil {
		return CollaborationBodyResult{}, err
	}
	if runattempt.IsTerminal(attempt.Status) || attempt.Origin != runattempt.OriginHarnessExecution {
		return CollaborationBodyResult{}, fmt.Errorf("collaboration body requires the exact active recipient run attempt")
	}
	// Resolve the recipient with the SAME resolver the delivery projection
	// uses, seeded from the attempt's own recorded coordinates. There is no
	// second, looser authority path here: whatever this returns is exactly
	// what a delivered card would have been addressed to.
	recipient, ok, err := s.ResolveCollaborationRecipient(CollaborationRecipientQuery{
		Harness:   attempt.Harness,
		SessionID: in.SessionID,
		Cwd:       attempt.Workspace,
	})
	if err != nil {
		return CollaborationBodyResult{}, err
	}
	if !ok ||
		recipient.Actor != in.Actor ||
		recipient.RunAttemptID != attempt.ID ||
		recipient.WorkID != attempt.WorkID {
		return CollaborationBodyResult{}, fmt.Errorf("collaboration body requires the exact active recipient binding")
	}
	if !CollaborationEventRelevant(event, recipient.RelevanceContext()) {
		return CollaborationBodyResult{}, fmt.Errorf("collaboration body request does not match current recipient relevance")
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return CollaborationBodyResult{}, err
	}
	if _, err := artifacts.Verify(event.BodyRef); err != nil {
		return CollaborationBodyResult{}, fmt.Errorf("collaboration body reference verification: %w", err)
	}
	body, err := artifacts.Get(event.BodyRef)
	if err != nil {
		return CollaborationBodyResult{}, err
	}
	return CollaborationBodyResult{EventID: event.ID, BodyRef: event.BodyRef, Body: body}, nil
}

func collaborationRefsIntersect(left, right []string) bool {
	for _, value := range left {
		if collaborationContains(right, value) {
			return true
		}
	}
	return false
}

func collaborationAffectsValue(event CollaborationEvent) CollaborationAffects {
	if event.Affects == nil {
		return CollaborationAffects{}
	}
	return *event.Affects
}

func collaborationEventsEquivalent(a, b CollaborationEvent) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func (s *Store) validateCollaborationCausalParents(workID, eventID string, parentIDs []string) error {
	if len(parentIDs) == 0 {
		return nil
	}
	lifecycle, err := s.eventsFor(workID)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(parentIDs))
	for _, parentID := range parentIDs {
		if !strings.HasPrefix(parentID, "wco_") {
			return fmt.Errorf("collaboration causal parent %q must start with wco_", parentID)
		}
		if parentID == eventID {
			return fmt.Errorf("collaboration causal parent %s cannot reference itself", parentID)
		}
		if seen[parentID] {
			return fmt.Errorf("collaboration causal parent %s is duplicated", parentID)
		}
		seen[parentID] = true
		if countAttached(lifecycle, parentID) != 1 {
			return fmt.Errorf("collaboration causal parent %s is not attached exactly once to work %s", parentID, workID)
		}
		var parent CollaborationEvent
		if err := s.readEventJSON(parentID, &parent); err != nil {
			return fmt.Errorf("collaboration causal parent %s: %w", parentID, err)
		}
		if err := parent.Validate(); err != nil {
			return fmt.Errorf("collaboration causal parent %s invalid: %w", parentID, err)
		}
		if parent.WorkID != workID {
			return fmt.Errorf("collaboration causal parent %s belongs to work %s, not %s", parentID, parent.WorkID, workID)
		}
	}
	return nil
}

func (s *Store) loadOrBuildCollaborationAction(id, eventID, semanticDigest string, w envelope.WorkEnvelope, in CollaborationInput, now time.Time) (envelope.ActionIntent, error) {
	expected := func(createdAt time.Time) envelope.ActionIntent {
		return envelope.ActionIntent{
			Header:        newHeader(envelope.SVActionIntent, id, createdAt, w.ID, w.IDs.ContextPackID, w.Privacy, w.Risk),
			Actor:         in.Actor,
			Capability:    "work.collaboration.record",
			Action:        "record_collaboration_observation",
			Target:        envelope.ActionTarget{Kind: "work_collaboration", ID: eventID, Path: in.BodyRef},
			MutationClass: "local_reversible",
			Reversibility: "manual",
			BlastRadius:   "local",
			Preview:       "record content-free collaboration metadata sha256:" + semanticDigest,
			RollbackPlan:  "preserve the append-only evidence and supersede or retire it through the work lifecycle",
		}
	}

	var stored envelope.ActionIntent
	err := s.readEventJSON(id, &stored)
	if err == nil {
		createdAt, parseErr := time.Parse(time.RFC3339, stored.CreatedAt)
		if parseErr != nil {
			return envelope.ActionIntent{}, fmt.Errorf("stored collaboration action %s created_at: %w", id, parseErr)
		}
		want := expected(createdAt)
		if !reflect.DeepEqual(stored, want) {
			return envelope.ActionIntent{}, fmt.Errorf("collaboration action %s conflicts with idempotency key", id)
		}
		return stored, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return envelope.ActionIntent{}, err
	}
	action := expected(now)
	if err := action.Validate(); err != nil {
		return envelope.ActionIntent{}, err
	}
	return action, nil
}

func (s *Store) loadOrBuildCollaborationDecision(id string, action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
	var stored envelope.PolicyDecision
	err := s.readEventJSON(id, &stored)
	if err == nil {
		if err := stored.Validate(); err != nil {
			return envelope.PolicyDecision{}, err
		}
		if stored.ActionIntentID != action.ID {
			return envelope.PolicyDecision{}, fmt.Errorf("collaboration policy %s is for action %s, want %s", id, stored.ActionIntentID, action.ID)
		}
		return stored, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return envelope.PolicyDecision{}, err
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return envelope.PolicyDecision{}, err
	}
	decision := policy.Evaluate(action, cfg, now)
	decision.ID = id
	if err := decision.Validate(); err != nil {
		return envelope.PolicyDecision{}, err
	}
	return decision, nil
}

func countAttached(events []Event, id string) int {
	count := 0
	for _, event := range events {
		for _, attached := range event.Attached {
			if attached == id {
				count++
			}
		}
	}
	return count
}
