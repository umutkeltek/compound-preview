package hookplan

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

type deliveryFixture struct {
	home         string
	store        *work.Store
	item         work.CollaborationResult
	work         string
	actor        string
	now          time.Time
	runAttemptID string
}

func newDeliveryFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(work.CreateInput{
		Owner:            "delivery",
		SuccessPredicate: "relevant coordination reaches its receiver",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:sender"
	claimed, err := store.Claim(item.ID, actor, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return &deliveryFixture{
		home: homePath, store: store, work: item.ID, actor: actor, now: now,
		runAttemptID: claimed.RunAttemptIDs[len(claimed.RunAttemptIDs)-1],
	}
}

func (fixture *deliveryFixture) record(t *testing.T, key, class string, affects work.CollaborationAffects) work.CollaborationEvent {
	return fixture.recordDelta(t, key, class, work.CollaborationStateDelta{
		Kind:       "work",
		SubjectRef: "subject:" + key,
		Before:     "before",
		After:      "after",
	}, affects)
}

func (fixture *deliveryFixture) recordDelta(t *testing.T, key, class string, delta work.CollaborationStateDelta, affects work.CollaborationAffects) work.CollaborationEvent {
	t.Helper()
	switch class {
	case work.CollaborationDeliveryInterrupt:
		attemptID := fixture.runAttemptID
		if len(affects.RunAttemptIDs) > 0 {
			attemptID = affects.RunAttemptIDs[0]
		} else {
			affects.RunAttemptIDs = []string{attemptID}
		}
		policyRef := fixture.recordInterruptEvidence(t, attemptID)
		affects.PolicyRefs = append(affects.PolicyRefs, policyRef)
		delta = work.CollaborationStateDelta{
			Kind: "run_attempt", SubjectRef: attemptID, Before: "active", After: "invalidated",
		}
	case work.CollaborationDeliveryHumanAttention:
		policyRef := fixture.recordHumanAttentionEvidence(t)
		affects.PolicyRefs = append(affects.PolicyRefs, policyRef, "compound://registries/operator-state")
		delta = work.CollaborationStateDelta{
			Kind: "authority", SubjectRef: policyRef, Before: "pending", After: "unresolved",
		}
	}
	artifacts, err := home.NewArtifactStore(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("full body for " + key))
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.store.RecordCollaboration(fixture.work, work.CollaborationInput{
		Actor:          fixture.actor,
		IdempotencyKey: key,
		BodyRef:        bodyRef,
		DeliveryClass:  class,
		Delta:          delta,
		Affects:        affects,
	}, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return result.Event
}

func (fixture *deliveryFixture) recordInterruptEvidence(t *testing.T, attemptID string) string {
	t.Helper()
	suffix := attemptID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	actionID := "act_test_interrupt_" + suffix
	policyID := "pol_test_interrupt_" + suffix
	attempts := runattempt.Open(fixture.home)
	attempt, err := attempts.Get(attemptID)
	if err != nil {
		t.Fatal(err)
	}
	action := deliveryEvidenceAction(actionID, fixture.work, attempt.Actor, "easy", fixture.now)
	decision := deliveryEvidenceDecision(policyID, fixture.work, actionID, []string{attemptID}, fixture.now)
	events, err := eventstore.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(action.ID, action, "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(decision.ID, decision, "  "); err != nil {
		t.Fatal(err)
	}
	if err := attempts.AttachActionIntent(attemptID, actionID); err != nil {
		t.Fatal(err)
	}
	return policyID
}

func (fixture *deliveryFixture) recordHumanAttentionEvidence(t *testing.T) string {
	t.Helper()
	action := deliveryEvidenceAction("act_test_human_attention", fixture.work, fixture.actor, "irreversible", fixture.now)
	decision := deliveryEvidenceDecision(
		"pol_test_human_attention", fixture.work, action.ID,
		[]string{"compound://registries/operator-state"}, fixture.now,
	)
	events, err := eventstore.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(action.ID, action, "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(decision.ID, decision, "  "); err != nil {
		t.Fatal(err)
	}
	state := policy.OperatorState{
		AutonomyLever: policy.OperatorAutonomyLever{Level: "L2"},
		Blocked: []map[string]any{{
			"work_id": fixture.work, "action_intent_id": action.ID,
			"policy_decision_id": decision.ID, "state": "unresolved",
		}},
	}
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(policy.OperatorStatePath(fixture.home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy.OperatorStatePath(fixture.home), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return decision.ID
}

func deliveryEvidenceAction(id, workID, actor, reversibility string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: id, CreatedAt: now.UTC().Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:       envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:   "private", Risk: "tier2", EvidenceRefs: []string{},
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: "work.execute", Action: "continue_run",
		Target:        envelope.ActionTarget{Kind: "work", ID: workID},
		MutationClass: "local_reversible", Reversibility: reversibility, BlastRadius: "repo",
	}
}

func deliveryEvidenceDecision(id, workID, actionID string, evidence []string, now time.Time) envelope.PolicyDecision {
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision, ID: id, CreatedAt: now.Add(time.Second).UTC().Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "policy", Name: "compound"},
			IDs:       envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:   "private", Risk: "tier2", EvidenceRefs: evidence,
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: actionID, Outcome: "hold",
		Reasons: []string{"governed state requires hold"}, Obligations: []string{},
	}
}

func (fixture *deliveryFixture) appendLegacyEvent(t *testing.T, key string) work.CollaborationEvent {
	t.Helper()
	artifacts, err := home.NewArtifactStore(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("legacy body for " + key))
	if err != nil {
		t.Fatal(err)
	}
	event := work.CollaborationEvent{
		SchemaVersion:    work.CollaborationLegacySchemaVersion,
		ID:               "wco_legacyfresh0001",
		Kind:             work.CollaborationKindObserved,
		CreatedAt:        fixture.now.Add(2 * time.Second).Format(time.RFC3339),
		WorkID:           fixture.work,
		Actor:            fixture.actor,
		IdempotencyKey:   key,
		BodyRef:          bodyRef,
		Visibility:       "work",
		Privacy:          "private",
		ActionIntentID:   "act_legacyfresh0001",
		PolicyDecisionID: "pol_legacyfresh0001",
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	store, err := eventstore.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(event.ID, event, "  "); err != nil {
		t.Fatal(err)
	}
	row, err := json.Marshal(work.Event{
		TS:       fixture.now.Add(2 * time.Second).Format(time.RFC3339),
		WorkID:   fixture.work,
		From:     "claimed",
		To:       "claimed",
		By:       fixture.actor,
		Attached: []string{event.ActionIntentID, event.PolicyDecisionID, event.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendWorkLog(append(row, '\n'), nil); err != nil {
		t.Fatal(err)
	}
	return event
}

func wideDeliveryBudget() *DeliveryBudget {
	return &DeliveryBudget{MaxCards: 1, MaxBytes: 4096, MaxEstimatedTokens: 1024, MaxItems: 8}
}

func TestIrrelevantEventsNeverEnterContext(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "irrelevant", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		WorkIDs: []string{"work_someone_else"},
	})

	report, err := DeliverCoordination(DeliveryRequest{
		HomePath:  fixture.home,
		Harness:   "codex",
		CursorKey: "irrelevant-session",
		Recipient: DeliveryRecipient{Actor: "codex:receiver", WorkID: "work_receiver"},
		Trigger:   DeliveryTriggerInterrupt,
		Budget:    wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Cards) != 0 || report.Native != nil || report.DeliveredBytes != 0 ||
		report.IrrelevantEvents != 1 || report.PendingEvents != 0 {
		t.Fatalf("irrelevant event entered delivery: %+v", report)
	}
}

func TestRepeatedCompatibleEventsDeliverOnceAsOneBoundedCard(t *testing.T) {
	fixture := newDeliveryFixture(t)
	affects := work.CollaborationAffects{WorkIDs: []string{fixture.work}}
	first := fixture.record(t, "coalesce-1", work.CollaborationDeliveryNextBoundary, affects)
	second := fixture.record(t, "coalesce-2", work.CollaborationDeliveryNextBoundary, affects)
	request := DeliveryRequest{
		HomePath:  fixture.home,
		Harness:   "codex",
		CursorKey: "coalesce-session",
		Recipient: DeliveryRecipient{Actor: "codex:receiver", WorkID: fixture.work},
		Trigger:   DeliveryTriggerBoundary,
		Budget:    wideDeliveryBudget(),
	}

	report, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Cards) != 1 || len(report.Cards[0].Items) != 2 {
		t.Fatalf("compatible events did not coalesce: %+v", report)
	}
	gotIDs := []string{report.Cards[0].Items[0].EventID, report.Cards[0].Items[1].EventID}
	if !reflect.DeepEqual(gotIDs, []string{first.ID, second.ID}) {
		t.Fatalf("coalesced event order = %v", gotIDs)
	}
	if report.Native == nil ||
		report.DeliveredBytes != len([]byte(report.Native.Context)) ||
		report.EstimatedDeliveredTokens != (len([]byte(report.Native.Context))+3)/4 ||
		len(report.DeclaredStateDeltaRefs) != 2 ||
		report.AcknowledgementMessages != 0 {
		t.Fatalf("delivery measurements/action delta = %+v", report)
	}
	if report.DeliveredBytes > request.Budget.MaxBytes || report.EstimatedDeliveredTokens > request.Budget.MaxEstimatedTokens {
		t.Fatalf("bounded card exceeded budget: %+v", report)
	}
	if report.EstimatedDeliveredTokens > DeliveryClassBudgetHypotheses[work.CollaborationDeliveryNextBoundary].MaxEstimatedTokensPerCard {
		t.Fatalf("next-boundary card exceeded class hypothesis: %+v", report)
	}
	for _, event := range []work.CollaborationEvent{first, second} {
		native := compileNativeDelivery("codex", makeDeliveryCard([]work.CollaborationEvent{event}))
		if estimatedDeliveredTokenCount(native.Context) >
			DeliveryClassBudgetHypotheses[work.CollaborationDeliveryNextBoundary].MaxEstimatedTokensPerItem {
			t.Fatalf("next-boundary item exceeded class hypothesis: %q", native.Context)
		}
	}

	repeated, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.Cards) != 0 || repeated.Native != nil || repeated.PendingEvents != 0 {
		t.Fatalf("cursor replay duplicated delivery: %+v", repeated)
	}
}

func TestDistinctEventsWithSameSubjectAndEffectiveStateInjectOnce(t *testing.T) {
	fixture := newDeliveryFixture(t)
	affects := work.CollaborationAffects{WorkIDs: []string{fixture.work}}
	for _, key := range []string{"same-state-1", "same-state-2"} {
		fixture.recordDelta(t, key, work.CollaborationDeliveryNextBoundary, work.CollaborationStateDelta{
			Kind: "reservation", SubjectRef: "resource:proof-battery", Before: "free", After: "held",
		}, affects)
	}

	report, err := DeliverCoordination(DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "semantic-same-state",
		Recipient: DeliveryRecipient{WorkID: fixture.work, RunAttemptID: fixture.runAttemptID},
		Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Cards) != 1 || len(report.Cards[0].Items) != 1 ||
		report.Cards[0].Items[0].Delta.SubjectRef != "resource:proof-battery" ||
		report.Cards[0].Items[0].Delta.After != "held" {
		t.Fatalf("semantic duplicate entered context: %+v", report)
	}
}

func TestBoundaryCoalescingDeliversOnlyLatestSubjectState(t *testing.T) {
	fixture := newDeliveryFixture(t)
	affects := work.CollaborationAffects{WorkIDs: []string{fixture.work}}
	for i, transition := range []struct{ before, after string }{
		{before: "free", after: "reserved"},
		{before: "reserved", after: "held"},
		{before: "held", after: "released"},
	} {
		fixture.recordDelta(t, "transition-"+string(rune('1'+i)), work.CollaborationDeliveryNextBoundary, work.CollaborationStateDelta{
			Kind: "reservation", SubjectRef: "resource:proof-battery",
			Before: transition.before, After: transition.after,
		}, affects)
	}

	report, err := DeliverCoordination(DeliveryRequest{
		HomePath: fixture.home, Harness: "claude-code", CursorKey: "semantic-latest",
		Recipient: DeliveryRecipient{WorkID: fixture.work, RunAttemptID: fixture.runAttemptID},
		Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Cards) != 1 || len(report.Cards[0].Items) != 1 ||
		report.Cards[0].Items[0].Delta.Before != "held" ||
		report.Cards[0].Items[0].Delta.After != "released" {
		t.Fatalf("boundary card did not collapse to latest state: %+v", report)
	}
	ledger, err := eventstore.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range report.Plan.Intentions {
		if _, err := ledger.Read(event.EventID); err != nil {
			t.Fatalf("governed ledger ref unavailable for %s: %v", event.EventID, err)
		}
	}
}

func TestDeliveryStatePersistsSemanticRevisionWithoutTerminalEventIDs(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "semantic-state", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{WorkIDs: []string{fixture.work}})
	request := DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "semantic-state-cursor",
		Recipient: DeliveryRecipient{WorkID: fixture.work, RunAttemptID: fixture.runAttemptID},
		Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	}
	if _, err := DeliverCoordination(request); err != nil {
		t.Fatal(err)
	}
	var stateBody []byte
	if err := withDeliveryStateLock(fixture.home, request.CursorKey, func(path string) error {
		var err error
		stateBody, err = os.ReadFile(path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBody), "terminal_event_refs") ||
		!strings.Contains(string(stateBody), `"recipient_revision"`) ||
		!strings.Contains(string(stateBody), `"semantic_revision"`) {
		t.Fatalf("delivery state is event-ID history, not semantic cursor state: %s", stateBody)
	}
}

func TestLegacyEventIsSilentOnFreshCursor(t *testing.T) {
	fixture := newDeliveryFixture(t)
	event := fixture.appendLegacyEvent(t, "legacy-fresh")
	report, err := DeliverCoordination(DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "legacy-fresh-cursor",
		Recipient: DeliveryRecipient{WorkID: fixture.work, RunAttemptID: fixture.runAttemptID},
		Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Native != nil || len(report.Cards) != 0 || report.SilentEvents != 1 {
		t.Fatalf("legacy event entered fresh context: event=%s report=%+v", event.ID, report)
	}
}

func TestSemanticDedupSurvivesCursorReset(t *testing.T) {
	fixture := newDeliveryFixture(t)
	event := fixture.record(t, "cursor-reset", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	request := DeliveryRequest{
		HomePath:  fixture.home,
		Harness:   "codex",
		CursorKey: "cursor-reset-session",
		Recipient: DeliveryRecipient{
			WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
			PolicyRefs: event.Affects.PolicyRefs,
		},
		Trigger: DeliveryTriggerInterrupt,
		Budget:  wideDeliveryBudget(),
	}
	first, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Cards) != 1 {
		t.Fatalf("first delivery = %+v", first)
	}
	if err := withDeliveryStateLock(fixture.home, request.CursorKey, func(path string) error {
		state, err := readDeliveryState(path, request.CursorKey, deliveryRecipientRevision(request.Recipient))
		if err != nil {
			return err
		}
		state.Cursor = 0
		return writeDeliveryState(path, state)
	}); err != nil {
		t.Fatal(err)
	}

	replayed, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Cards) != 0 || replayed.Native != nil || replayed.PendingEvents != 0 {
		t.Fatalf("semantic replay duplicated delivery: %+v", replayed)
	}
}

func TestPreviousSemanticCursorMigrationRestoresCurrentInterruptHoldWithoutRedelivery(t *testing.T) {
	fixture := newDeliveryFixture(t)
	event := fixture.record(t, "cursor-v2-interrupt", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	request := DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "test-v2",
		Recipient: DeliveryRecipient{
			WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
			PolicyRefs: event.Affects.PolicyRefs,
		},
		Trigger: DeliveryTriggerInterrupt, Budget: wideDeliveryBudget(),
	}
	first, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Native == nil || !first.ExecutionHeld {
		t.Fatalf("first interrupt did not deliver and hold: %+v", first)
	}
	if err := withDeliveryStateLock(fixture.home, request.CursorKey, func(path string) error {
		state, err := readDeliveryState(
			path, request.CursorKey, deliveryRecipientRevision(request.Recipient),
		)
		if err != nil {
			return err
		}
		state.SchemaVersion = deliveryStatePreviousVersion
		state.ActiveInterrupts = nil
		state.SemanticRevision = deliverySemanticRevisionV2(state)
		body, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(body, '\n'), 0o600)
	}); err != nil {
		t.Fatal(err)
	}

	migrated, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Native != nil || migrated.DeliveredBytes != 0 ||
		!migrated.ExecutionHeld || !containsRef(migrated.HoldEvidenceRefs, event.ID) {
		t.Fatalf("v2 cursor migration redelivered or released current interrupt: %+v", migrated)
	}
}

func TestRecipientRevisionEvolutionReevaluatesScopeDependencyAndRun(t *testing.T) {
	t.Run("scope", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		event := fixture.record(t, "scope-evolution", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{
			ScopeRefs: []string{"scope:new"},
		})
		request := DeliveryRequest{
			HomePath: fixture.home, Harness: "codex", CursorKey: "scope-evolution",
			Recipient: DeliveryRecipient{ScopeRefs: []string{"scope:old"}},
			Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
		}
		first, err := DeliverCoordination(request)
		if err != nil {
			t.Fatal(err)
		}
		if first.Native != nil || first.IrrelevantEvents != 1 {
			t.Fatalf("old scope received event: %+v", first)
		}
		request.Recipient.ScopeRefs = []string{"scope:new"}
		evolved, err := DeliverCoordination(request)
		if err != nil {
			t.Fatal(err)
		}
		if evolved.Native == nil || !strings.Contains(evolved.Native.Context, event.ID) {
			t.Fatalf("newly relevant scope event was lost: %+v", evolved)
		}
	})

	t.Run("dependency", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		event := fixture.record(t, "dependency-evolution", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{
			DependencyRefs: []string{"dependency:new"},
		})
		request := DeliveryRequest{
			HomePath: fixture.home, Harness: "claude-code", CursorKey: "dependency-evolution",
			Recipient: DeliveryRecipient{DependencyRefs: []string{"dependency:old"}},
			Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
		}
		if first, err := DeliverCoordination(request); err != nil {
			t.Fatal(err)
		} else if first.Native != nil || first.IrrelevantEvents != 1 {
			t.Fatalf("old dependency received event: %+v", first)
		}
		request.Recipient.DependencyRefs = []string{"dependency:new"}
		evolved, err := DeliverCoordination(request)
		if err != nil {
			t.Fatal(err)
		}
		if evolved.Native == nil || !strings.Contains(evolved.Native.Context, event.ID) {
			t.Fatalf("newly relevant dependency event was lost: %+v", evolved)
		}
	})

	t.Run("run_attempt", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		attempt, err := runattempt.Open(fixture.home).Mint(
			fixture.work, fixture.actor, "codex", fixture.now.Add(2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		event := fixture.record(t, "run-evolution", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
			WorkIDs: []string{fixture.work}, RunAttemptIDs: []string{attempt.ID},
		})
		request := DeliveryRequest{
			HomePath: fixture.home, Harness: "codex", CursorKey: "run-evolution",
			Recipient: DeliveryRecipient{
				Actor: fixture.actor, WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
				PolicyRefs: event.Affects.PolicyRefs,
			},
			Trigger: DeliveryTriggerInterrupt, Budget: wideDeliveryBudget(),
		}
		if first, err := DeliverCoordination(request); err != nil {
			t.Fatal(err)
		} else if first.Native != nil || first.IrrelevantEvents != 1 {
			t.Fatalf("unaddressed run received interrupt: %+v", first)
		}
		request.Recipient.RunAttemptID = attempt.ID
		evolved, err := DeliverCoordination(request)
		if err != nil {
			t.Fatal(err)
		}
		if evolved.Native == nil || !evolved.ExecutionHeld ||
			!strings.Contains(evolved.Native.Context, event.ID) {
			t.Fatalf("addressed run lost interrupt after revision evolution: %+v", evolved)
		}
	})
}

func TestInterruptBypassesNextBoundaryQueue(t *testing.T) {
	fixture := newDeliveryFixture(t)
	affects := work.CollaborationAffects{WorkIDs: []string{fixture.work}}
	deferred := fixture.record(t, "next-boundary", work.CollaborationDeliveryNextBoundary, affects)
	interrupt := fixture.record(t, "interrupt", work.CollaborationDeliveryInterrupt, affects)
	request := DeliveryRequest{
		HomePath:  fixture.home,
		Harness:   "codex",
		CursorKey: "interrupt-session",
		Recipient: DeliveryRecipient{
			WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
			PolicyRefs: interrupt.Affects.PolicyRefs,
		},
		Trigger: DeliveryTriggerInterrupt,
		Budget:  wideDeliveryBudget(),
	}

	immediate, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(immediate.Cards) != 1 || len(immediate.Cards[0].Items) != 1 ||
		immediate.Cards[0].Items[0].EventID != interrupt.ID ||
		immediate.PendingEvents != 1 {
		t.Fatalf("interrupt did not bypass queue: %+v", immediate)
	}
	if immediate.EstimatedDeliveredTokens > DeliveryClassBudgetHypotheses[work.CollaborationDeliveryInterrupt].MaxEstimatedTokensPerCard {
		t.Fatalf("interrupt card exceeded class hypothesis: %+v", immediate)
	}

	request.Trigger = DeliveryTriggerBoundary
	boundary, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(boundary.Cards) != 1 || len(boundary.Cards[0].Items) != 1 ||
		boundary.Cards[0].Items[0].EventID != deferred.ID ||
		boundary.PendingEvents != 0 {
		t.Fatalf("boundary queue did not drain exactly once: %+v", boundary)
	}
}

func TestInterruptCannotTerminalizeAtNextBoundaryBeforePreToolUse(t *testing.T) {
	fixture := newDeliveryFixture(t)
	interrupt := fixture.record(t, "interrupt-pretool-only", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	request := DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "interrupt-pretool-only",
		Recipient: DeliveryRecipient{
			Actor: fixture.actor, WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
			PolicyRefs: interrupt.Affects.PolicyRefs,
		},
		Trigger: DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	}
	boundary, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if boundary.Native != nil || boundary.ExecutionHeld || boundary.PendingEvents != 1 {
		t.Fatalf("next boundary terminalized interrupt before PreToolUse: %+v", boundary)
	}
	request.Trigger = DeliveryTriggerInterrupt
	preTool, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if preTool.Native == nil || !preTool.ExecutionHeld || preTool.PendingEvents != 0 ||
		!strings.Contains(preTool.Native.Context, interrupt.ID) {
		t.Fatalf("PreToolUse did not consume pending interrupt: %+v", preTool)
	}
}

func TestDeliveredInterruptStopsHoldingAfterRunTerminalizes(t *testing.T) {
	fixture := newDeliveryFixture(t)
	interrupt := fixture.record(t, "terminalized-after-delivery", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	request := DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "terminalized-after-delivery",
		Recipient: DeliveryRecipient{
			Actor: fixture.actor, WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
			PolicyRefs: interrupt.Affects.PolicyRefs,
		},
		Trigger: DeliveryTriggerInterrupt, Budget: wideDeliveryBudget(),
	}
	delivered, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if delivered.Native == nil || !delivered.ExecutionHeld {
		t.Fatalf("active interrupt was not delivered and held: %+v", delivered)
	}
	if _, err := runattempt.Open(fixture.home).Complete(
		fixture.runAttemptID, runattempt.StatusSucceeded, fixture.now.Add(20*time.Second),
	); err != nil {
		t.Fatal(err)
	}

	afterTerminal, err := DeliverCoordination(request)
	if err != nil {
		t.Fatalf("terminalized active interrupt must clear instead of failing delivery: %v", err)
	}
	if afterTerminal.ExecutionHeld || len(afterTerminal.HoldEvidenceRefs) != 0 {
		t.Fatalf("terminalized interrupt still held execution: %+v", afterTerminal)
	}
}

func TestDeliveryClassEscalationChangesSemanticRevisionAndHoldsInterrupt(t *testing.T) {
	fixture := newDeliveryFixture(t)
	delta := work.CollaborationStateDelta{
		Kind: "run_attempt", SubjectRef: fixture.runAttemptID,
		Before: "active", After: "invalidated",
	}
	affects := work.CollaborationAffects{
		WorkIDs: []string{fixture.work}, RunAttemptIDs: []string{fixture.runAttemptID},
	}
	firstEvent := fixture.recordDelta(
		t, "boundary-before-escalation", work.CollaborationDeliveryNextBoundary, delta, affects,
	)
	request := DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "class-escalation",
		Recipient: DeliveryRecipient{
			Actor: fixture.actor, WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
		},
		Trigger: DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	}
	first, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Native == nil || !strings.Contains(first.Native.Context, firstEvent.ID) {
		t.Fatalf("initial boundary state was not delivered: %+v", first)
	}

	interrupt := fixture.recordDelta(
		t, "interrupt-after-escalation", work.CollaborationDeliveryInterrupt, delta, affects,
	)
	request.Recipient.PolicyRefs = interrupt.Affects.PolicyRefs
	request.Trigger = DeliveryTriggerInterrupt
	escalated, err := DeliverCoordination(request)
	if err != nil {
		t.Fatal(err)
	}
	if escalated.Native == nil || !escalated.ExecutionHeld ||
		!strings.Contains(escalated.Native.Context, interrupt.ID) {
		t.Fatalf("semantic class escalation did not interrupt: %+v", escalated)
	}
}

func TestInterruptRequiresExactRecipientRunAttempt(t *testing.T) {
	fixture := newDeliveryFixture(t)
	addressed, err := runattempt.Open(fixture.home).Mint(
		fixture.work, "codex:receiver", "codex", fixture.now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	other, err := runattempt.Open(fixture.home).Mint(
		fixture.work, "codex:receiver", "codex", fixture.now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	event := fixture.record(t, "exact-attempt", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		WorkIDs: []string{fixture.work}, RunAttemptIDs: []string{addressed.ID},
	})
	base := DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", Trigger: DeliveryTriggerInterrupt,
		Budget: wideDeliveryBudget(),
	}
	addressedRequest := base
	addressedRequest.CursorKey = "attempt-addressed"
	addressedRequest.Recipient = DeliveryRecipient{
		Actor: "codex:receiver", WorkID: fixture.work, RunAttemptID: addressed.ID,
		PolicyRefs: event.Affects.PolicyRefs,
	}
	got, err := DeliverCoordination(addressedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Native == nil || got.DeliveredBytes == 0 {
		t.Fatalf("addressed attempt did not receive interrupt: %+v", got)
	}

	otherRequest := base
	otherRequest.CursorKey = "attempt-other"
	otherRequest.Recipient = DeliveryRecipient{
		Actor: "codex:receiver", WorkID: fixture.work, RunAttemptID: other.ID,
		PolicyRefs: event.Affects.PolicyRefs,
	}
	notAddressed, err := DeliverCoordination(otherRequest)
	if err != nil {
		t.Fatal(err)
	}
	if notAddressed.Native != nil || notAddressed.DeliveredBytes != 0 {
		t.Fatalf("same actor/work but unrelated attempt received interrupt: %+v", notAddressed)
	}
}

func TestActorMatchAloneDoesNotEstablishRelevance(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "actor-route-only", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{
		Actors: []string{"codex:receiver"},
	})
	report, err := DeliverCoordination(DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "actor-route-only",
		Recipient: DeliveryRecipient{Actor: "codex:receiver"},
		Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Native != nil || report.DeliveredBytes != 0 {
		t.Fatalf("actor match established relevance: %+v", report)
	}
}

func TestExplicitRelevanceDimensionsNarrowConjunctively(t *testing.T) {
	event := work.CollaborationEvent{
		WorkID:        "work_room",
		TargetActor:   "codex:receiver",
		DeliveryClass: work.CollaborationDeliveryNextBoundary,
		Affects: &work.CollaborationAffects{
			Actors:         []string{"codex:receiver"},
			WorkIDs:        []string{"work_recipient"},
			RunAttemptIDs:  []string{"ratt_20260724T120000Z_addressed"},
			ScopeRefs:      []string{"scope:hookplan"},
			DependencyRefs: []string{"dependency:stream"},
			PolicyRefs:     []string{"policy:delivery"},
			BaseRevisions:  []string{"git:base"},
			ResourceRefs:   []string{"resource:proof"},
		},
	}
	recipient := DeliveryRecipient{
		Actor:          "codex:receiver",
		WorkID:         "work_recipient",
		RunAttemptID:   "ratt_20260724T120000Z_addressed",
		ScopeRefs:      []string{"scope:hookplan"},
		DependencyRefs: []string{"dependency:stream"},
		PolicyRefs:     []string{"policy:delivery"},
		BaseRevisions:  []string{"git:base"},
		ResourceRefs:   []string{"resource:proof"},
	}
	if !deliveryRelevant(event, recipient) {
		t.Fatal("recipient matching every explicit relevance dimension was rejected")
	}

	tests := []struct {
		name   string
		mutate func(*DeliveryRecipient)
	}{
		{name: "work", mutate: func(got *DeliveryRecipient) { got.WorkID = "work_sibling" }},
		{name: "run_attempt", mutate: func(got *DeliveryRecipient) { got.RunAttemptID = "ratt_20260724T120000Z_sibling" }},
		{name: "scope", mutate: func(got *DeliveryRecipient) { got.ScopeRefs = []string{"scope:other"} }},
		{name: "dependency", mutate: func(got *DeliveryRecipient) { got.DependencyRefs = []string{"dependency:other"} }},
		{name: "policy", mutate: func(got *DeliveryRecipient) { got.PolicyRefs = []string{"policy:other"} }},
		{name: "base", mutate: func(got *DeliveryRecipient) { got.BaseRevisions = []string{"git:other"} }},
		{name: "resource", mutate: func(got *DeliveryRecipient) { got.ResourceRefs = []string{"resource:other"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recipient
			tt.mutate(&got)
			if deliveryRelevant(event, got) {
				t.Fatalf("same room/actor bypassed mismatched explicit %s dimension", tt.name)
			}
		})
	}
}

func TestSilentStateAndHumanAttentionAreContextSilentForDistinctReasons(t *testing.T) {
	fixture := newDeliveryFixture(t)
	affects := work.CollaborationAffects{ResourceRefs: []string{"resource:proof-battery"}}
	silent := fixture.record(t, "silent", work.CollaborationDeliverySilentState, affects)
	human := fixture.record(t, "human", work.CollaborationDeliveryHumanAttention, affects)

	report, err := DeliverCoordination(DeliveryRequest{
		HomePath:  fixture.home,
		Harness:   "claude-code",
		CursorKey: "class-session",
		Recipient: DeliveryRecipient{
			ResourceRefs: []string{"resource:proof-battery"},
		},
		Trigger: DeliveryTriggerInterrupt,
		Budget:  wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Native != nil || len(report.Cards) != 0 ||
		report.DeliveredBytes != 0 || report.EstimatedDeliveredTokens != 0 ||
		report.PendingEvents != 0 || report.AcknowledgementMessages != 0 {
		t.Fatalf("context-silent delivery injected or queued model context: %+v", report)
	}
	dispositions := map[string]string{}
	for _, intention := range report.Plan.Intentions {
		dispositions[intention.EventID] = intention.Disposition
	}
	if dispositions[silent.ID] != "silent_state" || dispositions[human.ID] != "operator_attention" {
		t.Fatalf("context-silent classes lost their distinct semantic owners: %+v", report.Plan)
	}
}

func TestHumanAttentionDoesNotEnterOrdinaryAgentSteering(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "human-operator-only", work.CollaborationDeliveryHumanAttention, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	report, err := DeliverCoordination(DeliveryRequest{
		HomePath: fixture.home, Harness: "claude-code", CursorKey: "human-agent",
		Recipient: DeliveryRecipient{Actor: "claude:receiver", WorkID: fixture.work},
		Trigger:   DeliveryTriggerInterrupt, Budget: wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Native != nil || report.DeliveredBytes != 0 {
		t.Fatalf("human attention entered ordinary agent context: %+v", report)
	}
}

func TestReadOperatorAttentionIsLevelTriggeredReferenceOnlyAndReadOnly(t *testing.T) {
	fixture := newDeliveryFixture(t)
	policyRef := fixture.recordHumanAttentionEvidence(t)
	artifacts, err := home.NewArtifactStore(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("targeted operator-only body"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := fixture.store.RecordCollaboration(fixture.work, work.CollaborationInput{
		Actor: fixture.actor, IdempotencyKey: "human-read-seam",
		TargetActor: "claude:ordinary-agent", BodyRef: bodyRef,
		DeliveryClass: work.CollaborationDeliveryHumanAttention,
		Delta: work.CollaborationStateDelta{
			Kind: "authority", SubjectRef: policyRef, Before: "pending", After: "unresolved",
		},
		Affects: work.CollaborationAffects{
			WorkIDs:    []string{fixture.work},
			PolicyRefs: []string{policyRef, policy.OperatorStateAuthorityRef},
		},
	}, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	latestBodyRef, err := artifacts.Put([]byte("latest targeted operator-only body"))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := fixture.store.RecordCollaboration(fixture.work, work.CollaborationInput{
		Actor: fixture.actor, IdempotencyKey: "human-read-seam-latest",
		TargetActor: "codex:another-ordinary-agent", BodyRef: latestBodyRef,
		DeliveryClass: work.CollaborationDeliveryHumanAttention,
		Delta: work.CollaborationStateDelta{
			Kind: "authority", SubjectRef: policyRef, Before: "pending", After: "unresolved",
		},
		Affects: work.CollaborationAffects{
			WorkIDs:    []string{fixture.work},
			PolicyRefs: []string{policyRef, policy.OperatorStateAuthorityRef},
		},
	}, fixture.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	before := deliveryFileInventory(t, fixture.home)
	projection, err := ReadOperatorAttention(OperatorAttentionRequest{
		HomePath: fixture.home, MaxItems: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Items) != 1 ||
		projection.Items[0].EventRef != recorded.Event.ID ||
		projection.Items[0].EventRef == first.Event.ID ||
		projection.Items[0].WorkRef != fixture.work ||
		projection.Items[0].DeliveryClass != work.CollaborationDeliveryHumanAttention ||
		!reflect.DeepEqual(projection.Items[0].Delta, work.CollaborationStateDelta{
			Kind: "authority", SubjectRef: policyRef, Before: "pending", After: "unresolved",
		}) ||
		!reflect.DeepEqual(projection.Items[0].ArtifactRefs, []string{latestBodyRef}) ||
		projection.Items[0].SemanticRevision != collaborationStateRevision(recorded.Event) ||
		projection.Total != 1 || projection.Truncated {
		t.Fatalf("operator attention projection lost reference-only human meaning: %+v", projection)
	}
	repeated, err := ReadOperatorAttention(OperatorAttentionRequest{
		HomePath: fixture.home, MaxItems: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repeated, projection) {
		t.Fatalf("level-triggered operator attention changed between reads:\nfirst=%+v\nsecond=%+v", projection, repeated)
	}
	after := deliveryFileInventory(t, fixture.home)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("operator attention read mutated Compound state:\nbefore=%v\nafter=%v", before, after)
	}
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"native", "cursor", "session", "run_attempt", "actor",
		"report_ref", "targeted operator-only body",
	} {
		if strings.Contains(strings.ToLower(string(body)), forbidden) {
			t.Fatalf("operator projection exposed forbidden %q: %s", forbidden, body)
		}
	}

	resolvedState := policy.OperatorState{
		AutonomyLever: policy.OperatorAutonomyLever{Level: "L2"},
		Blocked:       []map[string]any{},
	}
	resolvedBody, err := json.Marshal(resolvedState)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy.OperatorStatePath(fixture.home), resolvedBody, 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := ReadOperatorAttention(OperatorAttentionRequest{
		HomePath: fixture.home, MaxItems: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Items) != 0 {
		t.Fatalf("resolved operator authority remained in current projection: %+v", resolved)
	}
}

func TestReadOperatorAttentionHardCapsEightItemsAndExposesExactDTO(t *testing.T) {
	fixture := newDeliveryFixture(t)
	policyRef := fixture.recordHumanAttentionEvidence(t)
	artifacts, err := home.NewArtifactStore(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("operator projection body remains in artifact custody"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 12; index++ {
		if _, err := fixture.store.RecordCollaboration(fixture.work, work.CollaborationInput{
			Actor: fixture.actor, IdempotencyKey: fmt.Sprintf("attention-cap-%02d", index),
			TargetActor: "claude:ordinary-agent", BodyRef: bodyRef,
			DeliveryClass: work.CollaborationDeliveryHumanAttention,
			Delta: work.CollaborationStateDelta{
				Kind: "authority", SubjectRef: fmt.Sprintf("authority_gate_%02d", index),
				Before: "pending", After: "unresolved",
			},
			Affects: work.CollaborationAffects{
				WorkIDs: []string{fixture.work},
				PolicyRefs: []string{
					policyRef, policy.OperatorStateAuthorityRef,
				},
			},
		}, fixture.now.Add(time.Duration(index+2)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	omitted, err := ReadOperatorAttention(OperatorAttentionRequest{HomePath: fixture.home})
	if err != nil {
		t.Fatal(err)
	}
	oversized, err := ReadOperatorAttention(OperatorAttentionRequest{
		HomePath: fixture.home, MaxItems: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, projection := range map[string]OperatorAttentionProjection{
		"omitted": omitted, "oversized": oversized,
	} {
		if projection.Total != 12 || len(projection.Items) != 8 || !projection.Truncated {
			t.Fatalf("%s projection escaped hard max-8: %+v", name, projection)
		}
	}
	if !reflect.DeepEqual(omitted, oversized) {
		t.Fatalf("caller-selected max changed the hard-capped projection:\nomitted=%+v\noversized=%+v",
			omitted, oversized)
	}

	body, err := json.Marshal(omitted)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Items) != 8 {
		t.Fatalf("wire item count = %d, want 8", len(wire.Items))
	}
	wantKeys := []string{
		"artifact_refs", "delivery_class", "delta", "event_ref", "semantic_revision", "work_ref",
	}
	for index, item := range wire.Items {
		gotKeys := make([]string, 0, len(item))
		for key := range item {
			gotKeys = append(gotKeys, key)
		}
		sort.Strings(gotKeys)
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Fatalf("operator DTO item %d keys = %v, want exactly %v", index, gotKeys, wantKeys)
		}
	}
	if strings.Contains(string(body), "operator projection body remains in artifact custody") {
		t.Fatalf("operator DTO exposed artifact body: %s", body)
	}
}

func TestReadOperatorCoordinationFoldsCurrentStateWithoutMutatingHome(t *testing.T) {
	fixture := newDeliveryFixture(t)
	sharedDelta := work.CollaborationStateDelta{
		Kind: "scope", SubjectRef: "scope:shared", Before: "open", After: "reserved",
	}
	fixture.recordDelta(t, "operator-silent", work.CollaborationDeliverySilentState, sharedDelta, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	fixture.record(t, "operator-boundary", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	fixture.record(t, "operator-interrupt", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	fixture.record(t, "operator-human", work.CollaborationDeliveryHumanAttention, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})

	other, err := fixture.store.Create(work.CreateInput{
		Owner: "delivery", SuccessPredicate: "same semantic coordinate stays distinct across work",
	}, fixture.now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Claim(other.ID, "delivery", fixture.now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("other work body remains behind artifact custody"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RecordCollaboration(other.ID, work.CollaborationInput{
		Actor: "delivery", IdempotencyKey: "operator-other-work", BodyRef: bodyRef,
		DeliveryClass: work.CollaborationDeliverySilentState, Delta: sharedDelta,
		Affects: work.CollaborationAffects{WorkIDs: []string{other.ID}},
	}, fixture.now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}

	before := operatorProjectionHomeHash(t, fixture.home)
	projection, err := ReadOperatorCoordination(OperatorCoordinationRequest{
		HomePath: fixture.home,
	})
	if err != nil {
		t.Fatal(err)
	}
	if projection.Total != 5 || len(projection.Items) != 5 || projection.Truncated {
		t.Fatalf("operator coordination projection = %+v, want five current items", projection)
	}
	classes := map[string]int{}
	sharedWorks := map[string]bool{}
	for _, item := range projection.Items {
		classes[item.DeliveryClass]++
		if item.Delta.Kind == sharedDelta.Kind && item.Delta.SubjectRef == sharedDelta.SubjectRef {
			sharedWorks[item.WorkRef] = true
		}
	}
	for _, class := range []string{
		work.CollaborationDeliverySilentState,
		work.CollaborationDeliveryNextBoundary,
		work.CollaborationDeliveryInterrupt,
		work.CollaborationDeliveryHumanAttention,
	} {
		if classes[class] == 0 {
			t.Fatalf("operator coordination dropped %s: %+v", class, projection)
		}
	}
	if len(sharedWorks) != 2 {
		t.Fatalf("same semantic coordinate collided across work: %+v", projection.Items)
	}

	repeated, err := ReadOperatorCoordination(OperatorCoordinationRequest{
		HomePath: fixture.home, MaxItems: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repeated, projection) {
		t.Fatalf("level-triggered read changed without governed state delta:\nfirst=%+v\nsecond=%+v", projection, repeated)
	}
	after := operatorProjectionHomeHash(t, fixture.home)
	if before != after {
		t.Fatalf("operator coordination read mutated home: before=%s after=%s", before, after)
	}

	attention, err := ReadOperatorAttention(OperatorAttentionRequest{
		HomePath: fixture.home, MaxItems: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if attention.Total != 1 || len(attention.Items) != 1 ||
		attention.Items[0].DeliveryClass != work.CollaborationDeliveryHumanAttention {
		t.Fatalf("operator attention did not filter the shared fold: %+v", attention)
	}

	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{
		"artifact_refs", "delivery_class", "delta", "event_ref", "semantic_revision", "work_ref",
	}
	for index, item := range wire.Items {
		gotKeys := make([]string, 0, len(item))
		for key := range item {
			gotKeys = append(gotKeys, key)
		}
		sort.Strings(gotKeys)
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Fatalf("operator DTO item %d keys = %v, want exactly %v", index, gotKeys, wantKeys)
		}
	}
	for _, forbidden := range []string{
		"body", "actor", "session", "cursor", "report", "context", "argv", "ack",
	} {
		if strings.Contains(strings.ToLower(string(body)), forbidden) {
			t.Fatalf("operator projection exposed forbidden %q: %s", forbidden, body)
		}
	}
}

func TestReadOperatorCoordinationHardCapsAndReplacesSemanticState(t *testing.T) {
	t.Run("hard cap", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		for index := 0; index < 10; index++ {
			fixture.record(t, fmt.Sprintf("operator-cap-%02d", index), work.CollaborationDeliverySilentState, work.CollaborationAffects{
				WorkIDs: []string{fixture.work},
			})
		}
		for name, request := range map[string]OperatorCoordinationRequest{
			"default": {HomePath: fixture.home},
			"max-64":  {HomePath: fixture.home, MaxItems: 64},
		} {
			projection, err := ReadOperatorCoordination(request)
			if err != nil {
				t.Fatal(err)
			}
			if projection.Total != 10 || len(projection.Items) != 8 || !projection.Truncated {
				t.Fatalf("%s projection = %+v, want total=10 items=8 truncated", name, projection)
			}
		}
	})

	t.Run("attention filters after current-state fold", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		human := fixture.record(t, "operator-human-suppression", work.CollaborationDeliveryHumanAttention, work.CollaborationAffects{
			WorkIDs: []string{fixture.work},
		})
		clearance := fixture.recordDelta(t, "operator-human-clearance", work.CollaborationDeliverySilentState, work.CollaborationStateDelta{
			Kind: human.Delta.Kind, SubjectRef: human.Delta.SubjectRef,
			Before: "unresolved", After: "cleared",
		}, work.CollaborationAffects{WorkIDs: []string{fixture.work}})

		before := operatorProjectionHomeHash(t, fixture.home)
		coordination, err := ReadOperatorCoordination(OperatorCoordinationRequest{HomePath: fixture.home})
		if err != nil {
			t.Fatal(err)
		}
		attention, err := ReadOperatorAttention(OperatorAttentionRequest{HomePath: fixture.home})
		if err != nil {
			t.Fatal(err)
		}
		if coordination.Total != 1 || len(coordination.Items) != 1 ||
			coordination.Items[0].EventRef != clearance.ID ||
			coordination.Items[0].DeliveryClass != work.CollaborationDeliverySilentState {
			t.Fatalf("coordination did not expose latest governed clearance: %+v", coordination)
		}
		if attention.Total != 0 || len(attention.Items) != 0 {
			t.Fatalf("attention retained stale human state after clearance: %+v", attention)
		}

		repeatedCoordination, err := ReadOperatorCoordination(OperatorCoordinationRequest{HomePath: fixture.home})
		if err != nil {
			t.Fatal(err)
		}
		repeatedAttention, err := ReadOperatorAttention(OperatorAttentionRequest{HomePath: fixture.home})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(repeatedCoordination, coordination) || !reflect.DeepEqual(repeatedAttention, attention) {
			t.Fatalf("repeated operator reads changed:\ncoordination=%+v\nrepeated=%+v\nattention=%+v\nrepeated=%+v",
				coordination, repeatedCoordination, attention, repeatedAttention)
		}
		after := operatorProjectionHomeHash(t, fixture.home)
		if before != after {
			t.Fatalf("operator reads mutated home: before=%s after=%s", before, after)
		}
	})

	t.Run("latest clearance replaces suppression", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		interrupt := fixture.record(t, "operator-suppression", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
			WorkIDs: []string{fixture.work},
		})
		clearance := fixture.recordDelta(t, "operator-clearance", work.CollaborationDeliverySilentState, work.CollaborationStateDelta{
			Kind: interrupt.Delta.Kind, SubjectRef: interrupt.Delta.SubjectRef,
			Before: "invalidated", After: "cleared",
		}, work.CollaborationAffects{WorkIDs: []string{fixture.work}})

		projection, err := ReadOperatorCoordination(OperatorCoordinationRequest{HomePath: fixture.home})
		if err != nil {
			t.Fatal(err)
		}
		if projection.Total != 1 || len(projection.Items) != 1 ||
			projection.Items[0].EventRef != clearance.ID ||
			projection.Items[0].DeliveryClass != work.CollaborationDeliverySilentState {
			t.Fatalf("clearance did not replace older suppression: %+v", projection)
		}
	})
}

func TestReadOperatorCoordinationDropsResolvedOrTerminalStateAndFailsClosed(t *testing.T) {
	t.Run("resolved human authority disappears", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		fixture.record(t, "operator-resolved-human", work.CollaborationDeliveryHumanAttention, work.CollaborationAffects{
			WorkIDs: []string{fixture.work},
		})
		resolvedState := policy.OperatorState{
			AutonomyLever: policy.OperatorAutonomyLever{Level: "L2"},
			Blocked:       []map[string]any{},
		}
		body, err := json.Marshal(resolvedState)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(policy.OperatorStatePath(fixture.home), body, 0o600); err != nil {
			t.Fatal(err)
		}
		projection, err := ReadOperatorCoordination(OperatorCoordinationRequest{HomePath: fixture.home})
		if err != nil {
			t.Fatal(err)
		}
		if projection.Total != 0 || len(projection.Items) != 0 {
			t.Fatalf("resolved human authority remained operator-current: %+v", projection)
		}
	})

	t.Run("terminal interrupt disappears", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		fixture.record(t, "operator-terminal-interrupt", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
			WorkIDs: []string{fixture.work},
		})
		if _, err := runattempt.Open(fixture.home).Complete(
			fixture.runAttemptID, runattempt.StatusSucceeded, fixture.now.Add(20*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		projection, err := ReadOperatorCoordination(OperatorCoordinationRequest{HomePath: fixture.home})
		if err != nil {
			t.Fatal(err)
		}
		if projection.Total != 0 || len(projection.Items) != 0 {
			t.Fatalf("terminal exact interrupt remained operator-current: %+v", projection)
		}
	})

	t.Run("legacy is silent state", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		legacy := fixture.appendLegacyEvent(t, "operator-legacy")
		projection, err := ReadOperatorCoordination(OperatorCoordinationRequest{HomePath: fixture.home})
		if err != nil {
			t.Fatal(err)
		}
		if projection.Total != 1 || len(projection.Items) != 1 ||
			projection.Items[0].EventRef != legacy.ID ||
			projection.Items[0].DeliveryClass != work.CollaborationDeliverySilentState {
			t.Fatalf("legacy collaboration was not ledger-only silent state: %+v", projection)
		}
	})

	t.Run("malformed replay fails closed", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		fixture.record(t, "operator-before-corruption", work.CollaborationDeliverySilentState, work.CollaborationAffects{
			WorkIDs: []string{fixture.work},
		})
		logPath := filepath.Join(fixture.home, "events", "work.jsonl")
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("{malformed replay row}\n"); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadOperatorCoordination(OperatorCoordinationRequest{HomePath: fixture.home}); err == nil {
			t.Fatal("malformed replay returned a partial operator projection")
		}
	})

	t.Run("partial final replay fails closed without projection", func(t *testing.T) {
		fixture := newDeliveryFixture(t)
		fixture.record(t, "operator-before-partial-clearance", work.CollaborationDeliveryHumanAttention, work.CollaborationAffects{
			WorkIDs: []string{fixture.work},
		})
		logPath := filepath.Join(fixture.home, "events", "work.jsonl")
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		partial := fmt.Sprintf(
			`{"ts":"2026-07-24T12:03:00Z","work_id":%q,"from":"claimed","to":"claimed","by":"delivery","attached":["wco_clearance`,
			fixture.work,
		)
		if _, err := file.WriteString(partial); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}

		before := operatorProjectionHomeHash(t, fixture.home)
		coordination, coordinationErr := ReadOperatorCoordination(OperatorCoordinationRequest{HomePath: fixture.home})
		attention, attentionErr := ReadOperatorAttention(OperatorAttentionRequest{HomePath: fixture.home})
		if coordinationErr == nil || attentionErr == nil {
			t.Fatalf("partial tail did not fail both operator reads: coordination_err=%v attention_err=%v", coordinationErr, attentionErr)
		}
		if !reflect.DeepEqual(coordination, OperatorCoordinationProjection{}) ||
			!reflect.DeepEqual(attention, OperatorAttentionProjection{}) {
			t.Fatalf("partial tail returned stale projection: coordination=%+v attention=%+v", coordination, attention)
		}
		after := operatorProjectionHomeHash(t, fixture.home)
		if before != after {
			t.Fatalf("strict operator reads mutated home: before=%s after=%s", before, after)
		}
	})
}

func operatorProjectionHomeHash(t *testing.T, homePath string) string {
	t.Helper()
	digest := sha256.New()
	if err := filepath.WalkDir(homePath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(homePath, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = digest.Write([]byte(rel))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write(body)
		_, _ = digest.Write([]byte{0})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func deliveryFileInventory(t *testing.T, homePath string) []string {
	t.Helper()
	var files []string
	if err := filepath.WalkDir(homePath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(homePath, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files = append(files, fmt.Sprintf("%s:%d", rel, info.Size()))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func TestArtifactBodiesAreRetrievedOnlyOnDemand(t *testing.T) {
	fixture := newDeliveryFixture(t)
	event := fixture.record(t, "artifact-demand", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	digest := strings.TrimPrefix(event.BodyRef, home.ArtifactRefPrefix)
	artifactPath := filepath.Join(fixture.home, "artifacts", "sha256", digest)
	body, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}

	report, err := DeliverCoordination(DeliveryRequest{
		HomePath:  fixture.home,
		Harness:   "codex",
		CursorKey: "artifact-session",
		Recipient: DeliveryRecipient{
			WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
			PolicyRefs: event.Affects.PolicyRefs,
		},
		Trigger: DeliveryTriggerInterrupt,
		Budget:  wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatalf("delivery eagerly retrieved artifact body: %v", err)
	}
	if len(report.Cards) != 1 ||
		!reflect.DeepEqual(report.Cards[0].Items[0].ArtifactRefs, []string{event.BodyRef}) {
		t.Fatalf("delivery card lost artifact reference: %+v", report)
	}
	artifacts, err := home.NewArtifactStore(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Get(event.BodyRef); err == nil {
		t.Fatal("on-demand retrieval unexpectedly succeeded with missing body")
	}
	if ref, err := artifacts.Put(body); err != nil || ref != event.BodyRef {
		t.Fatalf("restore artifact ref=%q err=%v", ref, err)
	}
	got, err := artifacts.Get(event.BodyRef)
	if err != nil || !reflect.DeepEqual(got, body) {
		t.Fatalf("on-demand body=%q err=%v", got, err)
	}
}

func TestCodexAndClaudeCompileEquivalentMeaning(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "parity", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{
		DependencyRefs: []string{"dependency:base"},
	})
	base := DeliveryRequest{
		HomePath:  fixture.home,
		Recipient: DeliveryRecipient{DependencyRefs: []string{"dependency:base"}},
		Trigger:   DeliveryTriggerBoundary,
		Budget:    wideDeliveryBudget(),
	}
	codexRequest := base
	codexRequest.Harness = "codex"
	codexRequest.CursorKey = "parity-codex"
	claudeRequest := base
	claudeRequest.Harness = "claude-code"
	claudeRequest.CursorKey = "parity-claude"

	codex, err := DeliverCoordination(codexRequest)
	if err != nil {
		t.Fatal(err)
	}
	claude, err := DeliverCoordination(claudeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if codex.Native == nil || claude.Native == nil ||
		codex.Native.MeaningDigest != claude.Native.MeaningDigest ||
		!reflect.DeepEqual(codex.Native.Semantic, claude.Native.Semantic) {
		t.Fatalf("native semantic parity failed: codex=%+v claude=%+v", codex.Native, claude.Native)
	}
}

func TestCardAndTokenBudgetsFailClosedWithoutLosingPendingEvents(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "budget", work.CollaborationDeliveryInterrupt, work.CollaborationAffects{
		PolicyRefs: []string{"policy:budget"},
	})
	recipient := DeliveryRecipient{
		WorkID: fixture.work, RunAttemptID: fixture.runAttemptID,
		PolicyRefs: []string{"policy:budget"},
	}

	for _, test := range []struct {
		name   string
		budget *DeliveryBudget
	}{
		{
			name:   "card",
			budget: &DeliveryBudget{MaxCards: 0, MaxBytes: 4096, MaxEstimatedTokens: 1024, MaxItems: 8},
		},
		{
			name:   "token",
			budget: &DeliveryBudget{MaxCards: 1, MaxBytes: 4096, MaxEstimatedTokens: 1, MaxItems: 8},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := DeliveryRequest{
				HomePath:  fixture.home,
				Harness:   "codex",
				CursorKey: "budget-" + test.name,
				Recipient: recipient,
				Trigger:   DeliveryTriggerInterrupt,
				Budget:    test.budget,
			}
			held, err := DeliverCoordination(request)
			if err != nil {
				t.Fatal(err)
			}
			if !held.FailedClosed || len(held.Cards) != 0 || held.PendingEvents != 1 || held.Cursor == 0 {
				t.Fatalf("budget did not fail closed safely: %+v", held)
			}

			request.Budget = wideDeliveryBudget()
			recovered, err := DeliverCoordination(request)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.FailedClosed || len(recovered.Cards) != 1 || recovered.PendingEvents != 0 {
				t.Fatalf("budget recovery lost pending event: %+v", recovered)
			}
		})
	}
}

func TestPerClassEstimatedTokenBudgetsAreExplicitHypotheses(t *testing.T) {
	want := map[string]DeliveryClassBudget{
		work.CollaborationDeliverySilentState:    {MaxEstimatedTokensPerItem: 0, MaxEstimatedTokensPerCard: 0},
		work.CollaborationDeliveryNextBoundary:   {MaxEstimatedTokensPerItem: 120, MaxEstimatedTokensPerCard: 240},
		work.CollaborationDeliveryInterrupt:      {MaxEstimatedTokensPerItem: 80, MaxEstimatedTokensPerCard: 80},
		work.CollaborationDeliveryHumanAttention: {MaxEstimatedTokensPerItem: 0, MaxEstimatedTokensPerCard: 0},
	}
	if !reflect.DeepEqual(DeliveryClassBudgetHypotheses, want) {
		t.Fatalf("delivery class budget hypotheses = %+v, want %+v", DeliveryClassBudgetHypotheses, want)
	}
}

func TestUnchangedRecipientActionMeasuresZeroAfterDelivery(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "measure-zero", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	report, err := DeliverCoordination(DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "measure-zero",
		Recipient: DeliveryRecipient{WorkID: fixture.work},
		Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := MeasureDeliveryOutcome(fixture.home, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.CausedActionRefs) != 0 || len(receipt.ObservedActionRefs) != 0 ||
		len(receipt.ObservedRunAttemptRefs) != 0 ||
		len(receipt.ObservedWorkRefs) != 0 {
		t.Fatalf("unchanged recipient measured a caused action: %+v", receipt)
	}
}

func TestPostDeliveryMeasurementUsesNewOwnedActionAndWorkRefs(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "measure-action", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	report, err := DeliverCoordination(DeliveryRequest{
		HomePath: fixture.home, Harness: "claude-code", CursorKey: "measure-action",
		Recipient: DeliveryRecipient{WorkID: fixture.work},
		Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	action := deliveryEvidenceAction("act_post_delivery_observed", fixture.work, fixture.actor, "easy", fixture.now.Add(4*time.Second))
	action.EvidenceRefs = []string{report.ReportRef}
	decision := deliveryEvidenceDecision(
		"pol_post_delivery_observed", fixture.work, action.ID, []string{report.ReportRef}, fixture.now.Add(4*time.Second),
	)
	decision.Outcome = "allow"
	decision.Reasons = []string{"test observed governed action"}
	if _, err := fixture.store.RecordAction(fixture.work, action, decision, fixture.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	receipt, err := MeasureDeliveryOutcome(fixture.home, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt.CausedActionRefs, []string{action.ID}) ||
		!reflect.DeepEqual(receipt.ObservedActionRefs, []string{action.ID}) ||
		len(receipt.ObservedRunAttemptRefs) != 0 ||
		len(receipt.ObservedWorkRefs) != 1 {
		t.Fatalf("post-delivery action measurement = %+v", receipt)
	}
}

func TestPostDeliveryTemporalActionWithoutCausalRefRemainsObservedOnly(t *testing.T) {
	fixture := newDeliveryFixture(t)
	fixture.record(t, "measure-temporal-action", work.CollaborationDeliveryNextBoundary, work.CollaborationAffects{
		WorkIDs: []string{fixture.work},
	})
	report, err := DeliverCoordination(DeliveryRequest{
		HomePath: fixture.home, Harness: "codex", CursorKey: "measure-temporal-action",
		Recipient: DeliveryRecipient{WorkID: fixture.work},
		Trigger:   DeliveryTriggerBoundary, Budget: wideDeliveryBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	action := deliveryEvidenceAction(
		"act_temporal_after_delivery", fixture.work, fixture.actor, "easy", fixture.now.Add(4*time.Second),
	)
	action.EvidenceRefs = []string{"evidence:unrelated"}
	decision := deliveryEvidenceDecision(
		"pol_temporal_after_delivery", fixture.work, action.ID,
		[]string{report.ReportRef}, fixture.now.Add(4*time.Second),
	)
	decision.Outcome = "allow"
	decision.Reasons = []string{"unrelated governed action"}
	if _, err := fixture.store.RecordAction(
		fixture.work, action, decision, fixture.now.Add(5*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	receipt, err := MeasureDeliveryOutcome(fixture.home, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.CausedActionRefs) != 0 ||
		!reflect.DeepEqual(receipt.ObservedActionRefs, []string{action.ID}) {
		t.Fatalf("temporal-only action was mislabeled as caused: %+v", receipt)
	}
}

// TestDeliveryRelevanceIsTheWorkOwnedRuleAcrossEveryDimension pins the
// cross-surface contract: the delivery projection asks the SAME work-owned
// rule the explicit body door asks. A card that names a body the recipient
// could not fetch is the contradiction this repair removes, so the two
// surfaces must answer identically for every recipient/event pair --
// including the cross-work case, where the event belongs to another work but
// names this recipient explicitly.
func TestDeliveryRelevanceIsTheWorkOwnedRuleAcrossEveryDimension(t *testing.T) {
	events := []work.CollaborationEvent{
		{
			SchemaVersion: work.CollaborationSchemaVersion,
			ID:            "wco_0123456789abcdef",
			WorkID:        "work_source",
			Actor:         "codex:sender",
			DeliveryClass: work.CollaborationDeliveryNextBoundary,
			TargetActor:   "claude:receiver",
			Affects: &work.CollaborationAffects{
				Actors:        []string{"claude:receiver"},
				WorkIDs:       []string{"work_recipient"},
				RunAttemptIDs: []string{"ratt_20260724T120000Z_addressed"},
			},
		},
		{
			SchemaVersion: work.CollaborationSchemaVersion,
			ID:            "wco_0123456789abcdef0",
			WorkID:        "work_source",
			Actor:         "codex:sender",
			DeliveryClass: work.CollaborationDeliveryInterrupt,
			Affects: &work.CollaborationAffects{
				WorkIDs:       []string{"work_recipient"},
				RunAttemptIDs: []string{"ratt_20260724T120000Z_addressed"},
			},
		},
		{
			SchemaVersion: work.CollaborationSchemaVersion,
			ID:            "wco_0123456789abcdef1",
			WorkID:        "work_source",
			Actor:         "codex:sender",
			DeliveryClass: work.CollaborationDeliveryNextBoundary,
			Affects: &work.CollaborationAffects{
				WorkIDs: []string{"work_somebody_else"},
			},
		},
	}
	recipients := []DeliveryRecipient{
		{
			Actor: "claude:receiver", WorkID: "work_recipient",
			RunAttemptID: "ratt_20260724T120000Z_addressed",
		},
		{
			Actor: "claude:receiver", WorkID: "work_recipient",
		},
		{
			Actor: "claude:other", WorkID: "work_recipient",
			RunAttemptID: "ratt_20260724T120000Z_addressed",
		},
	}
	for _, event := range events {
		for _, recipient := range recipients {
			projected := work.CollaborationRecipientContext{
				Actor:          recipient.Actor,
				WorkID:         recipient.WorkID,
				RunAttemptID:   recipient.RunAttemptID,
				ScopeRefs:      recipient.ScopeRefs,
				DependencyRefs: recipient.DependencyRefs,
				PolicyRefs:     recipient.PolicyRefs,
				BaseRevisions:  recipient.BaseRevisions,
				ResourceRefs:   recipient.ResourceRefs,
			}
			want := work.CollaborationEventRelevant(event, projected)
			if got := deliveryRelevant(event, recipient); got != want {
				t.Fatalf("delivery and body surfaces disagree for event %s recipient %+v: delivery=%v work=%v",
					event.ID, recipient, got, want)
			}
		}
	}
	// The cross-work handoff the repair exists for: the event belongs to
	// another work but names this recipient explicitly, so BOTH surfaces
	// admit it.
	crossWork := events[0]
	explicit := recipients[0]
	if !deliveryRelevant(crossWork, explicit) {
		t.Fatal("delivery refused an explicitly relevant cross-work event")
	}
	if !work.CollaborationEventRelevant(crossWork, work.CollaborationRecipientContext{
		Actor:        explicit.Actor,
		WorkID:       explicit.WorkID,
		RunAttemptID: explicit.RunAttemptID,
	}) {
		t.Fatal("body rule refused an explicitly relevant cross-work event")
	}
}
