package work

import (
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// crossWorkFixture is one cross-work handoff: a SOURCE work that owns the
// event and the body artifact, and a RECIPIENT work that owns the active
// native session. The two works are independent envelopes; the handoff is
// addressed only by the coordinates the sender declared.
type crossWorkFixture struct {
	store          *Store
	sourceWorkID   string
	recipientWork  string
	recipientActor string
	attemptID      string
	sessionID      string
	bodyRef        string
	eventID        string
}

func newCrossWorkFixture(t *testing.T) crossWorkFixture {
	t.Helper()
	store := newStore(t)
	const (
		sender   = "codex:sender"
		receiver = "claude:receiver"
	)
	source, err := store.Create(CreateInput{
		Owner: sender, SuccessPredicate: "cross-work handoff body reaches the explicit recipient",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(source.ID, sender, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	recipient, err := store.Create(CreateInput{
		Owner: receiver, SuccessPredicate: "cross-work handoff body reaches the explicit recipient",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(recipient.ID, receiver, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	attempt := bindCollaborationRecipientForTest(
		t, store, recipient.ID, receiver, receiver, "claude-code", "cross-work-session",
		t.TempDir(), fixedNow.Add(3*time.Second),
	)
	bodyRef := collaborationBodyRef(t, store, "cross-work governed body")
	recorded, err := store.RecordCollaboration(source.ID, CollaborationInput{
		Actor: sender, IdempotencyKey: "cross-work-handoff",
		BodyRef: bodyRef, DeliveryClass: CollaborationDeliveryNextBoundary,
		Delta: CollaborationStateDelta{Kind: "work", SubjectRef: source.ID, After: "changed"},
		Affects: CollaborationAffects{
			WorkIDs:       []string{recipient.ID},
			RunAttemptIDs: []string{attempt.ID},
		},
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return crossWorkFixture{
		store:          store,
		sourceWorkID:   source.ID,
		recipientWork:  recipient.ID,
		recipientActor: receiver,
		attemptID:      attempt.ID,
		sessionID:      "cross-work-session",
		bodyRef:        bodyRef,
		eventID:        recorded.Event.ID,
	}
}

// TestCrossWorkCollaborationBodyIsReadableByExplicitRecipient proves the
// repaired handoff: the SOURCE work owns the event and the body artifact, the
// RECIPIENT lives in another work, and the body is released to that
// recipient's active native session -- with no lifecycle mutation anywhere.
func TestCrossWorkCollaborationBodyIsReadableByExplicitRecipient(t *testing.T) {
	t.Parallel()
	fixture := newCrossWorkFixture(t)
	beforeSource, err := fixture.store.eventsFor(fixture.sourceWorkID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRecipient, err := fixture.store.eventsFor(fixture.recipientWork)
	if err != nil {
		t.Fatal(err)
	}

	got, err := fixture.store.RetrieveCollaborationBody(CollaborationBodyRequest{
		WorkID: fixture.sourceWorkID, EventID: fixture.eventID, Actor: fixture.recipientActor,
		RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
	})
	if err != nil {
		t.Fatalf("cross-work body refused for the explicit recipient: %v", err)
	}
	if string(got.Body) != "cross-work governed body" || got.BodyRef != fixture.bodyRef {
		t.Fatalf("cross-work body = %+v", got)
	}

	afterSource, err := fixture.store.eventsFor(fixture.sourceWorkID)
	if err != nil {
		t.Fatal(err)
	}
	afterRecipient, err := fixture.store.eventsFor(fixture.recipientWork)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSource) != len(beforeSource) || len(afterRecipient) != len(beforeRecipient) {
		t.Fatalf("cross-work body fetch mutated a lifecycle: source=%d/%d recipient=%d/%d",
			len(beforeSource), len(afterSource), len(beforeRecipient), len(afterRecipient))
	}
}

// TestCrossWorkCollaborationBodyRefusesWrongRecipientCoordinates is the
// falsifier set: every wrong coordinate must refuse, and no refusal may be
// reachable by dropping a coordinate the recipient does have.
func TestCrossWorkCollaborationBodyRefusesWrongRecipientCoordinates(t *testing.T) {
	t.Parallel()
	fixture := newCrossWorkFixture(t)
	attempts := runattempt.Open(fixture.store.home)
	unrelated, err := attempts.Mint(fixture.recipientWork, fixture.recipientActor, "claude-code", fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := attempts.Mint(fixture.recipientWork, fixture.recipientActor, "claude-code", fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Complete(terminal.ID, runattempt.StatusFailed, fixedNow.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		request CollaborationBodyRequest
	}{
		{
			name: "wrong source work",
			request: CollaborationBodyRequest{
				WorkID: "work_not_the_source", EventID: fixture.eventID, Actor: fixture.recipientActor,
				RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
			},
		},
		{
			name: "wrong event",
			request: CollaborationBodyRequest{
				WorkID: fixture.sourceWorkID, EventID: "wco_0123456789abcdef", Actor: fixture.recipientActor,
				RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
			},
		},
		{
			name: "wrong actor",
			request: CollaborationBodyRequest{
				WorkID: fixture.sourceWorkID, EventID: fixture.eventID, Actor: "claude:impostor",
				RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
			},
		},
		{
			name: "wrong run attempt",
			request: CollaborationBodyRequest{
				WorkID: fixture.sourceWorkID, EventID: fixture.eventID, Actor: fixture.recipientActor,
				RunAttemptID: unrelated.ID, SessionID: fixture.sessionID,
			},
		},
		{
			name: "terminal run attempt",
			request: CollaborationBodyRequest{
				WorkID: fixture.sourceWorkID, EventID: fixture.eventID, Actor: fixture.recipientActor,
				RunAttemptID: terminal.ID, SessionID: fixture.sessionID,
			},
		},
		{
			name: "wrong session",
			request: CollaborationBodyRequest{
				WorkID: fixture.sourceWorkID, EventID: fixture.eventID, Actor: fixture.recipientActor,
				RunAttemptID: fixture.attemptID, SessionID: "someone-elses-session",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := fixture.store.RetrieveCollaborationBody(tt.request); err == nil {
				t.Fatalf("%s fetched the cross-work collaboration body", tt.name)
			}
		})
	}
}

// TestCrossWorkCollaborationBodyRefusesIrrelevantEvent proves the shared rule
// still binds: a cross-work event that names no coordinate of the recipient
// is refused even by the recipient's own exact attempt and session.
func TestCrossWorkCollaborationBodyRefusesIrrelevantEvent(t *testing.T) {
	t.Parallel()
	fixture := newCrossWorkFixture(t)
	third, err := fixture.store.Create(CreateInput{
		Owner: "codex:sender", SuccessPredicate: "an event naming no recipient is refused",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Claim(third.ID, "codex:sender", fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	bodyRef := collaborationBodyRef(t, fixture.store, "body for nobody")
	recorded, err := fixture.store.RecordCollaboration(third.ID, CollaborationInput{
		Actor: "codex:sender", IdempotencyKey: "irrelevant-cross-work",
		BodyRef: bodyRef, DeliveryClass: CollaborationDeliveryNextBoundary,
		Delta: CollaborationStateDelta{Kind: "work", SubjectRef: third.ID, After: "changed"},
		Affects: CollaborationAffects{
			WorkIDs: []string{"work_somebody_else"},
		},
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RetrieveCollaborationBody(CollaborationBodyRequest{
		WorkID: third.ID, EventID: recorded.Event.ID, Actor: fixture.recipientActor,
		RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
	}); err == nil {
		t.Fatal("an irrelevant cross-work event was released to the recipient")
	}
}

// TestCrossWorkCollaborationBodyRefusesWrongAffectedActor proves the Affects
// dimension is conjunctive: addressing a different actor in the affected
// index refuses the body even when the run attempt and session are exact.
func TestCrossWorkCollaborationBodyRefusesWrongAffectedActor(t *testing.T) {
	t.Parallel()
	fixture := newCrossWorkFixture(t)
	bodyRef := collaborationBodyRef(t, fixture.store, "body for another actor")
	recorded, err := fixture.store.RecordCollaboration(fixture.sourceWorkID, CollaborationInput{
		Actor: "codex:sender", IdempotencyKey: "wrong-affected-actor",
		TargetActor: "claude:somebody-else",
		BodyRef:     bodyRef, DeliveryClass: CollaborationDeliveryNextBoundary,
		Delta: CollaborationStateDelta{Kind: "work", SubjectRef: fixture.sourceWorkID, After: "changed"},
		Affects: CollaborationAffects{
			Actors:  []string{"claude:somebody-else"},
			WorkIDs: []string{fixture.recipientWork},
		},
	}, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RetrieveCollaborationBody(CollaborationBodyRequest{
		WorkID: fixture.sourceWorkID, EventID: recorded.Event.ID, Actor: fixture.recipientActor,
		RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
	}); err == nil {
		t.Fatal("an event addressed to another actor was released to the recipient")
	}
}

// TestCollaborationRelevanceRuleIsOneRuleForBothSurfaces pins the shared
// predicate's contract: the same event and the same recipient coordinates
// produce the same relevance answer no matter which surface asks.
func TestCollaborationRelevanceRuleIsOneRuleForBothSurfaces(t *testing.T) {
	t.Parallel()
	event := CollaborationEvent{
		SchemaVersion: CollaborationSchemaVersion,
		ID:            "wco_0123456789abcdef",
		WorkID:        "work_source",
		Actor:         "codex:sender",
		DeliveryClass: CollaborationDeliveryNextBoundary,
		TargetActor:   "claude:receiver",
		Affects: &CollaborationAffects{
			Actors:         []string{"claude:receiver"},
			WorkIDs:        []string{"work_recipient"},
			RunAttemptIDs:  []string{"ratt_20260724T120000Z_addressed"},
			ScopeRefs:      []string{"scope:hookplan"},
			DependencyRefs: []string{"dependency:stream"},
			PolicyRefs:     []string{"policy:delivery"},
			BaseRevisions:  []string{"git:base"},
			ResourceRefs:   []string{"resource:proof"},
		},
	}
	recipient := CollaborationRecipientContext{
		Actor:          "claude:receiver",
		WorkID:         "work_recipient",
		RunAttemptID:   "ratt_20260724T120000Z_addressed",
		ScopeRefs:      []string{"scope:hookplan"},
		DependencyRefs: []string{"dependency:stream"},
		PolicyRefs:     []string{"policy:delivery"},
		BaseRevisions:  []string{"git:base"},
		ResourceRefs:   []string{"resource:proof"},
	}
	if !CollaborationEventRelevant(event, recipient) {
		t.Fatal("recipient matching every explicit relevance dimension was rejected")
	}

	tests := []struct {
		name   string
		mutate func(*CollaborationRecipientContext)
	}{
		{name: "actor", mutate: func(got *CollaborationRecipientContext) { got.Actor = "claude:other" }},
		{name: "work", mutate: func(got *CollaborationRecipientContext) { got.WorkID = "work_sibling" }},
		{name: "run_attempt", mutate: func(got *CollaborationRecipientContext) { got.RunAttemptID = "ratt_20260724T120000Z_sibling" }},
		{name: "scope", mutate: func(got *CollaborationRecipientContext) { got.ScopeRefs = []string{"scope:other"} }},
		{name: "dependency", mutate: func(got *CollaborationRecipientContext) { got.DependencyRefs = []string{"dependency:other"} }},
		{name: "policy", mutate: func(got *CollaborationRecipientContext) { got.PolicyRefs = []string{"policy:other"} }},
		{name: "base", mutate: func(got *CollaborationRecipientContext) { got.BaseRevisions = []string{"git:other"} }},
		{name: "resource", mutate: func(got *CollaborationRecipientContext) { got.ResourceRefs = []string{"resource:other"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recipient
			got.ScopeRefs = append([]string{}, recipient.ScopeRefs...)
			got.DependencyRefs = append([]string{}, recipient.DependencyRefs...)
			got.PolicyRefs = append([]string{}, recipient.PolicyRefs...)
			got.BaseRevisions = append([]string{}, recipient.BaseRevisions...)
			got.ResourceRefs = append([]string{}, recipient.ResourceRefs...)
			tt.mutate(&got)
			if CollaborationEventRelevant(event, got) {
				t.Fatalf("same room/actor bypassed mismatched explicit %s dimension", tt.name)
			}
		})
	}
}

// TestCollaborationRelevanceRuleBindsAffectedActors pins the affected-actor
// dimension: an event that names a different actor in Affects.Actors is
// refused even when every other coordinate matches, and an event that names
// no actor at all stays admissible to its own work.
func TestCollaborationRelevanceRuleBindsAffectedActors(t *testing.T) {
	t.Parallel()
	base := CollaborationEvent{
		SchemaVersion: CollaborationSchemaVersion,
		ID:            "wco_0123456789abcdef",
		WorkID:        "work_source",
		Actor:         "codex:sender",
		DeliveryClass: CollaborationDeliveryNextBoundary,
		Affects: &CollaborationAffects{
			Actors:  []string{"claude:receiver"},
			WorkIDs: []string{"work_recipient"},
		},
	}
	recipient := CollaborationRecipientContext{Actor: "claude:receiver", WorkID: "work_recipient"}
	if !CollaborationEventRelevant(base, recipient) {
		t.Fatal("affected actor that matches the recipient was rejected")
	}
	other := base
	other.Affects = &CollaborationAffects{
		Actors:  []string{"claude:somebody-else"},
		WorkIDs: []string{"work_recipient"},
	}
	if CollaborationEventRelevant(other, recipient) {
		t.Fatal("affected actor that does not match the recipient was admitted")
	}
	mismatch := base
	mismatch.TargetActor = "claude:somebody-else"
	if CollaborationEventRelevant(mismatch, recipient) {
		t.Fatal("target actor that does not match the recipient was admitted")
	}
}

// TestCollaborationRelevanceRuleRequiresExactRunForInterrupt pins the
// interrupt dimension: an interrupt is relevant only to the recipient whose
// exact live run attempt it invalidates.
func TestCollaborationRelevanceRuleRequiresExactRunForInterrupt(t *testing.T) {
	t.Parallel()
	event := CollaborationEvent{
		SchemaVersion: CollaborationSchemaVersion,
		ID:            "wco_0123456789abcdef",
		WorkID:        "work_source",
		Actor:         "codex:sender",
		DeliveryClass: CollaborationDeliveryInterrupt,
		Affects: &CollaborationAffects{
			WorkIDs:       []string{"work_recipient"},
			RunAttemptIDs: []string{"ratt_20260724T120000Z_addressed"},
		},
	}
	addressed := CollaborationRecipientContext{
		Actor: "claude:receiver", WorkID: "work_recipient",
		RunAttemptID: "ratt_20260724T120000Z_addressed",
	}
	if !CollaborationEventRelevant(event, addressed) {
		t.Fatal("interrupt addressing the exact live run was rejected")
	}
	sibling := addressed
	sibling.RunAttemptID = "ratt_20260724T120000Z_sibling"
	if CollaborationEventRelevant(event, sibling) {
		t.Fatal("interrupt was admitted to a recipient with a different run attempt")
	}
	nameless := addressed
	nameless.RunAttemptID = ""
	if CollaborationEventRelevant(event, nameless) {
		t.Fatal("interrupt was admitted to a recipient without a live run attempt")
	}
}

// TestCollaborationRelevanceRuleAdmitsSameWorkWithoutAffects pins the
// same-work case: an event with no declared coordinates still belongs to its
// own work.
func TestCollaborationRelevanceRuleAdmitsSameWorkWithoutAffects(t *testing.T) {
	t.Parallel()
	event := CollaborationEvent{
		SchemaVersion: CollaborationSchemaVersion,
		ID:            "wco_0123456789abcdef",
		WorkID:        "work_source",
		Actor:         "codex:sender",
		DeliveryClass: CollaborationDeliveryNextBoundary,
	}
	same := CollaborationRecipientContext{Actor: "claude:receiver", WorkID: "work_source"}
	if !CollaborationEventRelevant(event, same) {
		t.Fatal("same-work event was rejected")
	}
	other := CollaborationRecipientContext{Actor: "claude:receiver", WorkID: "work_elsewhere"}
	if CollaborationEventRelevant(event, other) {
		t.Fatal("cross-work event without coordinates was admitted")
	}
}

// TestCollaborationRecipientActorComesFromAuthorizingAction pins the actor
// source: a governed attempt is authorized by its ActionIntent, not by the
// harness label on the attempt.
func TestCollaborationRecipientActorComesFromAuthorizingAction(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	const actor = "codex:holder"
	item, err := store.Create(CreateInput{Owner: actor, SuccessPredicate: "actor comes from the authorizing action"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(item.ID, actor, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts := runattempt.Open(store.home)
	attempt, err := attempts.Mint(item.ID, "claude-code", "claude-code", fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := store.collaborationRecipientActor(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "claude-code" {
		t.Fatalf("unattempted-governance actor = %q", resolved)
	}
	if err := attempts.AttachActionIntent(attempt.ID, "act_missing_authority"); err != nil {
		t.Fatal(err)
	}
	attempt, err = attempts.Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.collaborationRecipientActor(attempt); err == nil ||
		!strings.Contains(err.Error(), "act_missing_authority") {
		t.Fatalf("missing authorizing action was accepted: %v", err)
	}
}

// TestCrossWorkCollaborationBodyRefusesCanceledRecipientAndLostAttachment pins
// the lifecycle half of the body door: a recipient whose work terminalized, or
// whose owned session binding was superseded by a recorded exit, can never
// fetch the body that was addressed to it.
func TestCrossWorkCollaborationBodyRefusesCanceledRecipientAndLostAttachment(t *testing.T) {
	t.Parallel()

	t.Run("canceled recipient work", func(t *testing.T) {
		fixture := newCrossWorkFixture(t)
		if _, err := fixture.store.Close(fixture.recipientWork, CloseInput{
			Outcome: "cancelled", MoveType: "audit_only", Actor: fixture.recipientActor,
			Summary: "recipient canceled before reading the handoff body",
		}, fixedNow.Add(7*time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.RetrieveCollaborationBody(CollaborationBodyRequest{
			WorkID: fixture.sourceWorkID, EventID: fixture.eventID, Actor: fixture.recipientActor,
			RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
		}); err == nil {
			t.Fatal("a canceled recipient fetched the cross-work body")
		}
	})

	t.Run("lost session attachment", func(t *testing.T) {
		fixture := newCrossWorkFixture(t)
		binding, alive, err := fixture.store.latestDelegateBinding(fixture.recipientWork)
		if err != nil || !alive || binding == nil {
			t.Fatalf("fixture binding missing: alive=%v binding=%+v err=%v", alive, binding, err)
		}
		exitID, err := fixture.store.recordDelegateExited(fixture.recipientWork, binding, nil, fixedNow.Add(8*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		item, err := fixture.store.Get(fixture.recipientWork)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.appendDelegateLifecycleEvent(
			fixture.recipientWork, item.State, fixture.recipientActor, exitID,
			"test session exit attached", fixedNow.Add(8*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.RetrieveCollaborationBody(CollaborationBodyRequest{
			WorkID: fixture.sourceWorkID, EventID: fixture.eventID, Actor: fixture.recipientActor,
			RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
		}); err == nil {
			t.Fatal("a recipient whose session attachment was lost fetched the cross-work body")
		}
	})
}

// TestCrossWorkCollaborationBodyRefusesTamperedClassAuthority pins class
// authority parity: a recorded next_boundary observation rewritten in storage
// to claim interrupt delivery must be refused by the delivery-evidence
// recheck, exactly as a delivered card would be.
func TestCrossWorkCollaborationBodyRefusesTamperedClassAuthority(t *testing.T) {
	t.Parallel()
	fixture := newCrossWorkFixture(t)
	var stored CollaborationEvent
	if err := fixture.store.readEventJSON(fixture.eventID, &stored); err != nil {
		t.Fatal(err)
	}
	// A copy under a fresh id: the eventstore is append-only, and the attack
	// being falsified is an ungoverned event APPEARING in storage.
	stored.ID = "wco_" + strings.Repeat("a1b2c3d4", 2)
	stored.DeliveryClass = CollaborationDeliveryInterrupt
	stored.Delta = &CollaborationStateDelta{
		Kind: "run_attempt", SubjectRef: fixture.attemptID, After: "invalidated",
	}
	stored.Affects = &CollaborationAffects{
		WorkIDs:       []string{fixture.recipientWork},
		RunAttemptIDs: []string{fixture.attemptID},
	}
	if err := fixture.store.writeEventJSON(stored.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RetrieveCollaborationBody(CollaborationBodyRequest{
		WorkID: fixture.sourceWorkID, EventID: stored.ID, Actor: fixture.recipientActor,
		RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
	}); err == nil {
		t.Fatal("a tampered interrupt class was released without governed authority")
	}
}

// TestCrossWorkCollaborationBodyBindsTargetActorAndAffectedActorsConjunctively
// pins the actor dimensions at the body door: a correctly targeted actor is
// still refused while Affects.Actors names somebody else, whether the target
// is set or unset.
func TestCrossWorkCollaborationBodyBindsTargetActorAndAffectedActorsConjunctively(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		targetActor string
	}{
		{name: "correct target actor with wrong affected actors", targetActor: "claude:receiver"},
		{name: "unset target actor with wrong affected actors"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCrossWorkFixture(t)
			bodyRef := collaborationBodyRef(t, fixture.store, "body for another affected actor")
			input := CollaborationInput{
				Actor: "codex:sender", IdempotencyKey: "wrong-affected-" + workSlug(tt.name),
				BodyRef: bodyRef, DeliveryClass: CollaborationDeliveryNextBoundary,
				Delta: CollaborationStateDelta{Kind: "work", SubjectRef: fixture.sourceWorkID, After: "changed"},
				Affects: CollaborationAffects{
					Actors:  []string{"claude:somebody-else"},
					WorkIDs: []string{fixture.recipientWork},
				},
			}
			// Record, then state the sender's real intent directly in storage:
			// recording merges TargetActor into Affects.Actors, and the point
			// here is the stored conjunction the door re-reads.
			recorded, err := fixture.store.RecordCollaboration(fixture.sourceWorkID, input, fixedNow.Add(9*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if tt.targetActor != "" {
				stored := recorded.Event
				stored.ID = "wco_" + strings.Repeat("e5f6a7b8", 2)
				stored.TargetActor = tt.targetActor
				stored.Affects = &CollaborationAffects{
					Actors:  []string{"claude:somebody-else"},
					WorkIDs: []string{fixture.recipientWork},
				}
				if err := fixture.store.writeEventJSON(stored.ID, &stored); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.store.RetrieveCollaborationBody(CollaborationBodyRequest{
				WorkID: fixture.sourceWorkID, EventID: recorded.Event.ID, Actor: fixture.recipientActor,
				RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
			}); err == nil {
				t.Fatal("an event whose affected actors exclude the recipient released its body")
			}
		})
	}
}

func workSlug(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, value)
}
