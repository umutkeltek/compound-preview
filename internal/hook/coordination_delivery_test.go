package hook

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/hookplan"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestLiveHookConsumesLifecycleDeliveryWithoutReadingArtifactBody(t *testing.T) {
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
		SuccessPredicate: "live hook consumes relevant coordination",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	const sender = "codex:sender"
	claimed, err := store.Claim(item.ID, sender, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runAttemptID := claimed.RunAttemptIDs[len(claimed.RunAttemptIDs)-1]
	action, decision := hookInterruptEvidence(item.ID, sender, runAttemptID, now)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(action.ID, action, "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(decision.ID, decision, "  "); err != nil {
		t.Fatal(err)
	}
	if err := runattempt.Open(homePath).AttachActionIntent(runAttemptID, action.ID); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const privateBody = "private body must remain behind its artifact reference"
	bodyRef, err := artifacts.Put([]byte(privateBody))
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.RecordCollaboration(item.ID, work.CollaborationInput{
		Actor:          sender,
		IdempotencyKey: "hook-interrupt",
		BodyRef:        bodyRef,
		DeliveryClass:  work.CollaborationDeliveryInterrupt,
		Delta: work.CollaborationStateDelta{
			Kind:       "run_attempt",
			SubjectRef: runAttemptID,
			Before:     "active",
			After:      "invalidated",
		},
		Affects: work.CollaborationAffects{
			WorkIDs: []string{item.ID}, RunAttemptIDs: []string{runAttemptID},
			PolicyRefs: []string{decision.ID},
		},
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	toolAction := action
	toolAction.ID = "act_pretool_body_reference"
	toolAction.Action = "write_file"
	toolAction.Target = envelope.ActionTarget{Kind: "file", Path: "README.md"}
	result, err := Run(Fixture{
		Hook:    "pre-tool-use",
		Harness: "codex",
		Live:    true,
		Cwd:     hermeticHookRepo(t),
		WorkID:  item.ID,
		Action:  &toolAction,
		CoordinationDelivery: &CoordinationDeliveryInput{
			CursorKey: "hook-session",
			Recipient: hookplan.DeliveryRecipient{
				Actor: "codex:receiver", WorkID: item.ID, RunAttemptID: runAttemptID,
				PolicyRefs: []string{decision.ID},
			},
			Trigger: hookplan.DeliveryTriggerInterrupt,
		},
	}, Deps{
		Registry: harness.DefaultRegistry(),
		HomePath: homePath,
		Store:    store,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.CoordinationDelivery == nil || result.CoordinationDelivery.Native == nil ||
		!strings.Contains(result.ContextOutput, event.Event.ID) ||
		!strings.Contains(result.ContextOutput, bodyRef) ||
		strings.Contains(result.ContextOutput, privateBody) {
		t.Fatalf("live hook delivery = %+v context=%q", result.CoordinationDelivery, result.ContextOutput)
	}
	if result.CoordinationDelivery.DeliveredBytes != len([]byte(result.CoordinationDelivery.Native.Context)) ||
		result.CoordinationDelivery.EstimatedDeliveredTokens == 0 ||
		len(result.CoordinationDelivery.DeclaredStateDeltaRefs) != 1 ||
		result.CoordinationDelivery.ReportRef == "" ||
		!containsString(result.Event.SideEffects, result.CoordinationDelivery.ReportRef) {
		t.Fatalf("delivery observability missing: report=%+v event=%+v", result.CoordinationDelivery, result.Event)
	}
}

func TestHookPersistenceFailureLeavesCoordinationCardRetryableExactlyOnce(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(work.CreateInput{Owner: "delivery", SuccessPredicate: "hook and delivery commit together"}, now)
	if err != nil {
		t.Fatal(err)
	}
	const sender = "codex:sender"
	if _, err := store.Claim(item.ID, sender, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("retryable body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCollaboration(item.ID, work.CollaborationInput{
		Actor: sender, IdempotencyKey: "atomic-boundary", BodyRef: bodyRef,
		DeliveryClass: work.CollaborationDeliveryNextBoundary,
		Delta:         work.CollaborationStateDelta{Kind: "work", SubjectRef: item.ID, After: "changed"},
		Affects:       work.CollaborationAffects{WorkIDs: []string{item.ID}},
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture := Fixture{
		Hook: "post-tool-use", Harness: "codex", Live: true, WorkID: item.ID, Tool: "Bash",
		CoordinationDelivery: &CoordinationDeliveryInput{
			CursorKey: "atomic-session", Recipient: hookplan.DeliveryRecipient{WorkID: item.ID},
			Trigger: hookplan.DeliveryTriggerBoundary,
		},
	}
	injected := errors.New("injected hook persistence failure")
	_, err = Run(fixture, Deps{
		Registry: harness.DefaultRegistry(), HomePath: homePath, Store: store,
		BeforeHookEventPersist: func() error { return injected },
	}, now.Add(3*time.Second))
	if !errors.Is(err, injected) {
		t.Fatalf("first hook error = %v, want injected failure", err)
	}

	retry, err := Run(fixture, Deps{
		Registry: harness.DefaultRegistry(), HomePath: homePath, Store: store,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retry.CoordinationDelivery == nil || retry.CoordinationDelivery.Native == nil ||
		retry.CoordinationDelivery.DeliveredBytes == 0 {
		t.Fatalf("failed hook persistence consumed delivery: %+v", retry.CoordinationDelivery)
	}
	onceMore, err := Run(fixture, Deps{
		Registry: harness.DefaultRegistry(), HomePath: homePath, Store: store,
	}, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if onceMore.CoordinationDelivery == nil || onceMore.CoordinationDelivery.Native != nil ||
		onceMore.CoordinationDelivery.DeliveredBytes != 0 {
		t.Fatalf("retry emitted card more than once: %+v", onceMore.CoordinationDelivery)
	}
}

func TestOverBudgetInterruptHoldsPreToolUseExecution(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(work.CreateInput{Owner: "delivery", SuccessPredicate: "interrupt overflow holds execution"}, now)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:receiver"
	claimed, err := store.Claim(item.ID, actor, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runAttemptID := claimed.RunAttemptIDs[len(claimed.RunAttemptIDs)-1]
	action, decision := hookInterruptEvidence(item.ID, actor, runAttemptID, now)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(action.ID, action, "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(decision.ID, decision, "  "); err != nil {
		t.Fatal(err)
	}
	if err := runattempt.Open(homePath).AttachActionIntent(runAttemptID, action.ID); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("interrupt evidence body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCollaboration(item.ID, work.CollaborationInput{
		Actor: actor, IdempotencyKey: "interrupt-overflow", BodyRef: bodyRef,
		DeliveryClass: work.CollaborationDeliveryInterrupt,
		Delta: work.CollaborationStateDelta{
			Kind: "run_attempt", SubjectRef: runAttemptID, Before: "active", After: "invalidated",
		},
		Affects: work.CollaborationAffects{
			WorkIDs: []string{item.ID}, RunAttemptIDs: []string{runAttemptID},
			PolicyRefs: []string{decision.ID},
		},
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	toolAction := action
	toolAction.ID = "act_pretool_current"
	toolAction.Action = "write_file"
	toolAction.Target = envelope.ActionTarget{Kind: "file", Path: "README.md"}
	result, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Live: true, Cwd: hermeticHookRepo(t), WorkID: item.ID, Action: &toolAction,
		CoordinationDelivery: &CoordinationDeliveryInput{
			CursorKey: "interrupt-overflow",
			Recipient: hookplan.DeliveryRecipient{
				Actor: actor, WorkID: item.ID, RunAttemptID: runAttemptID,
				PolicyRefs: []string{decision.ID},
			},
			Trigger: hookplan.DeliveryTriggerInterrupt,
			Budget:  &hookplan.DeliveryBudget{MaxCards: 1, MaxBytes: 4096, MaxEstimatedTokens: 1, MaxItems: 1},
		},
	}, Deps{
		Registry: harness.DefaultRegistry(), HomePath: homePath, Store: store,
		Policy: policy.DefaultConfig(),
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.Status != "blocked" || result.CoordinationDelivery == nil ||
		!result.CoordinationDelivery.ExecutionHeld || result.CoordinationDelivery.PendingEvents != 1 {
		t.Fatalf("interrupt overflow did not hold execution: event=%+v report=%+v", result.Event, result.CoordinationDelivery)
	}
	foundHold := false
	for _, produced := range result.Produced {
		if pd, ok := produced.(*envelope.PolicyDecision); ok && pd.Outcome == "hold" {
			foundHold = true
		}
	}
	if !foundHold {
		t.Fatalf("interrupt overflow produced no governed hold: %+v", result.Produced)
	}
}

func TestEligibleInterruptHoldsPreToolUseBeforeMutationWithinBudget(t *testing.T) {
	testCase := newHookInterruptCase(t, "normal-interrupt")
	result, err := Run(testCase.fixture, Deps{
		Registry: harness.DefaultRegistry(), HomePath: testCase.homePath, Store: testCase.store,
		Policy: policy.DefaultConfig(),
	}, testCase.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.Status != "blocked" || result.CoordinationDelivery == nil ||
		result.CoordinationDelivery.Native == nil || !result.CoordinationDelivery.ExecutionHeld ||
		result.CoordinationDelivery.PendingEvents != 0 ||
		!strings.Contains(result.ContextOutput, testCase.event.ID) {
		t.Fatalf("eligible interrupt did not block before mutation: event=%+v delivery=%+v",
			result.Event, result.CoordinationDelivery)
	}
	foundHold := false
	for _, produced := range result.Produced {
		if decision, ok := produced.(*envelope.PolicyDecision); ok && decision.Outcome == "hold" &&
			containsString(decision.EvidenceRefs, testCase.event.ID) {
			foundHold = true
		}
	}
	if !foundHold {
		t.Fatalf("eligible interrupt produced no causal governed hold: %+v", result.Produced)
	}
}

func TestDeliveredInterruptHoldsEveryPreToolUseUntilGovernedClearance(t *testing.T) {
	testCase := newHookInterruptCase(t, "level-triggered-interrupt")
	testCase.fixture.Action.Target.Path = filepath.Join(t.TempDir(), "mutation-sentinel.txt")
	deps := Deps{
		Registry: harness.DefaultRegistry(), HomePath: testCase.homePath, Store: testCase.store,
		Policy: policy.DefaultConfig(),
	}
	mutationRan := false
	invokeMutationBoundary := func(now time.Time) Result {
		t.Helper()
		result, err := Run(testCase.fixture, deps, now)
		if err != nil {
			t.Fatal(err)
		}
		if result.Event.Status != "blocked" {
			mutationRan = true
		}
		return result
	}

	first := invokeMutationBoundary(testCase.now.Add(3 * time.Second))
	if first.CoordinationDelivery == nil || first.CoordinationDelivery.Native == nil ||
		!first.CoordinationDelivery.ExecutionHeld ||
		!strings.Contains(first.ContextOutput, testCase.event.ID) {
		t.Fatalf("first interrupt boundary did not deliver and hold: event=%+v delivery=%+v",
			first.Event, first.CoordinationDelivery)
	}
	repeated := invokeMutationBoundary(testCase.now.Add(4 * time.Second))
	if repeated.CoordinationDelivery == nil || repeated.CoordinationDelivery.Native != nil ||
		repeated.CoordinationDelivery.DeliveredBytes != 0 ||
		!repeated.CoordinationDelivery.ExecutionHeld ||
		!containsString(repeated.CoordinationDelivery.HoldEvidenceRefs, testCase.event.ID) {
		t.Fatalf("unchanged interrupt was redelivered or stopped holding: event=%+v delivery=%+v",
			repeated.Event, repeated.CoordinationDelivery)
	}
	if mutationRan {
		t.Fatal("mutation sentinel ran while the exact recipient interrupt remained active")
	}

	if _, err := testCase.store.RecordCollaboration(testCase.event.WorkID, work.CollaborationInput{
		Actor: testCase.event.Actor, IdempotencyKey: "level-triggered-interrupt-clearance",
		BodyRef: testCase.event.BodyRef, DeliveryClass: work.CollaborationDeliverySilentState,
		Delta: work.CollaborationStateDelta{
			Kind: testCase.event.Delta.Kind, SubjectRef: testCase.event.Delta.SubjectRef,
			Before: "invalidated", After: "active",
		},
		Affects: work.CollaborationAffects{
			WorkIDs: testCase.event.Affects.WorkIDs, RunAttemptIDs: testCase.event.Affects.RunAttemptIDs,
		},
	}, testCase.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	cleared, err := Run(testCase.fixture, deps, testCase.now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Event.Status == "blocked" || cleared.CoordinationDelivery == nil ||
		cleared.CoordinationDelivery.ExecutionHeld || cleared.CoordinationDelivery.Native != nil {
		t.Fatalf("governed superseding state did not clear execution hold: event=%+v delivery=%+v",
			cleared.Event, cleared.CoordinationDelivery)
	}
}

func TestHookDeliveryPublicationFaultsLeaveInterruptRetryableExactlyOnce(t *testing.T) {
	tests := []struct {
		name string
		fail func(*Deps, error)
	}{
		{
			name: "after_hook_event_before_report",
			fail: func(deps *Deps, injected error) {
				deps.BeforeDeliveryReportPersist = func() error { return injected }
			},
		},
		{
			name: "after_report_before_state",
			fail: func(deps *Deps, injected error) {
				deps.BeforeDeliveryStatePersist = func() error { return injected }
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCase := newHookInterruptCase(t, tt.name)
			injected := errors.New("injected " + tt.name)
			deps := Deps{
				Registry: harness.DefaultRegistry(), HomePath: testCase.homePath, Store: testCase.store,
				Policy: policy.DefaultConfig(),
			}
			tt.fail(&deps, injected)
			if _, err := Run(testCase.fixture, deps, testCase.now.Add(3*time.Second)); !errors.Is(err, injected) {
				t.Fatalf("fault error = %v, want %v", err, injected)
			}

			retry, err := Run(testCase.fixture, Deps{
				Registry: harness.DefaultRegistry(), HomePath: testCase.homePath, Store: testCase.store,
				Policy: policy.DefaultConfig(),
			}, testCase.now.Add(3*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if retry.Event.Status != "blocked" || retry.CoordinationDelivery == nil ||
				retry.CoordinationDelivery.Native == nil || !retry.CoordinationDelivery.ExecutionHeld ||
				!strings.Contains(retry.ContextOutput, testCase.event.ID) {
				t.Fatalf("fault consumed interrupt: event=%+v delivery=%+v", retry.Event, retry.CoordinationDelivery)
			}
			if _, err := os.Stat(filepath.Join(testCase.homePath, retry.CoordinationDelivery.ReportRef)); err != nil {
				t.Fatalf("retry left dangling delivery report ref: %v", err)
			}
			// The retry reuses the fault run's hook_ id (pinned now), so its
			// source-control evidence must come from the fixture's private repo:
			// a snapshot of the process cwd drifts between runs and conflicts.
			snapshot, err := authority.ReadGitSnapshot(filepath.Join(testCase.repo, "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.GitRoot != testCase.repo || !containsString(retry.Event.SideEffects, snapshot.EvidenceReference()) {
				t.Fatalf("retry source-control evidence not bound to fixture repo %s: root=%s side_effects=%v",
					testCase.repo, snapshot.GitRoot, retry.Event.SideEffects)
			}

			onceMore, err := Run(testCase.fixture, Deps{
				Registry: harness.DefaultRegistry(), HomePath: testCase.homePath, Store: testCase.store,
				Policy: policy.DefaultConfig(),
			}, testCase.now.Add(4*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if onceMore.CoordinationDelivery == nil || onceMore.CoordinationDelivery.Native != nil ||
				onceMore.CoordinationDelivery.DeliveredBytes != 0 {
				t.Fatalf("recovered interrupt delivered more than once: %+v", onceMore.CoordinationDelivery)
			}
		})
	}
}

func TestCorruptCoordinationReplayHoldsPreToolUseUntilRepairThenDeliversOnce(t *testing.T) {
	testCase := newHookInterruptCase(t, "corrupt-replay")
	events, err := eventstore.Open(testCase.homePath)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := testCase.event
	sentinel.ID = "wco_ffffffffffffffff"
	sentinel.IdempotencyKey = "corrupt-replay-sentinel"
	if _, err := events.PutJSON(sentinel.ID, sentinel, " "); err != nil {
		t.Fatal(err)
	}
	segmentPath, original := eventSegmentContaining(t, testCase.homePath, testCase.event.ID)
	corruptID := "wcx_" + strings.TrimPrefix(testCase.event.ID, "wco_")
	corrupted := bytes.Replace(original, []byte(testCase.event.ID), []byte(corruptID), 1)
	if bytes.Equal(corrupted, original) {
		t.Fatal("test did not locate collaboration event in canonical segment")
	}
	rewriteSegmentInLaterTick(t, segmentPath, corrupted)

	integrityHold, err := Run(testCase.fixture, Deps{
		Registry: harness.DefaultRegistry(), HomePath: testCase.homePath, Store: testCase.store,
		Policy: policy.DefaultConfig(),
	}, testCase.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if integrityHold.Event.Status != "blocked" || integrityHold.CoordinationDelivery == nil ||
		!integrityHold.CoordinationDelivery.FailedClosed ||
		!integrityHold.CoordinationDelivery.ExecutionHeld ||
		integrityHold.CoordinationDelivery.Native != nil ||
		!strings.Contains(integrityHold.CoordinationDelivery.DegradedReason, "replay integrity") {
		t.Fatalf("corrupt replay did not fail closed before mutation: event=%+v delivery=%+v",
			integrityHold.Event, integrityHold.CoordinationDelivery)
	}

	rewriteSegmentInLaterTick(t, segmentPath, original)
	retry, err := Run(testCase.fixture, Deps{
		Registry: harness.DefaultRegistry(), HomePath: testCase.homePath, Store: testCase.store,
		Policy: policy.DefaultConfig(),
	}, testCase.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retry.Event.Status != "blocked" || retry.CoordinationDelivery == nil ||
		retry.CoordinationDelivery.Native == nil ||
		!strings.Contains(retry.ContextOutput, testCase.event.ID) {
		t.Fatalf("repaired interrupt was not retried: event=%+v delivery=%+v",
			retry.Event, retry.CoordinationDelivery)
	}

	onceMore, err := Run(testCase.fixture, Deps{
		Registry: harness.DefaultRegistry(), HomePath: testCase.homePath, Store: testCase.store,
		Policy: policy.DefaultConfig(),
	}, testCase.now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if onceMore.CoordinationDelivery == nil || onceMore.CoordinationDelivery.Native != nil ||
		onceMore.CoordinationDelivery.DeliveredBytes != 0 {
		t.Fatalf("repaired interrupt delivered more than once: %+v", onceMore.CoordinationDelivery)
	}
}

func eventSegmentContaining(t *testing.T, homePath, eventID string) (string, []byte) {
	t.Helper()
	var foundPath string
	var foundBody []byte
	err := filepath.WalkDir(filepath.Join(homePath, "events", "segments"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || foundPath != "" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte(eventID)) &&
			bytes.Contains(body, []byte(work.CollaborationSchemaVersion)) {
			foundPath = path
			foundBody = append([]byte{}, body...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if foundPath == "" {
		t.Fatalf("canonical segment for %s not found", eventID)
	}
	return foundPath, foundBody
}

type hookInterruptCase struct {
	homePath string
	repo     string
	store    *work.Store
	now      time.Time
	event    work.CollaborationEvent
	fixture  Fixture
}

// hermeticHookRepo returns the symlink-resolved root of a private git repository
// for a live pre-tool-use fixture. A relative action target with no fixture cwd
// resolves against the TEST PROCESS cwd, so the source-control evidence ref in the
// hook event would bind to whatever repository the test binary runs in (the
// proof-check battery's sealed copy, dirtied by concurrent gates). A fault run and
// its retry share a pinned `now`, hence the same hook_ id: any drift in that ref
// between the two runs is an id conflict. The repository under test must be one
// nothing else touches.
func hermeticHookRepo(t *testing.T) string {
	t.Helper()
	repo, err := filepath.EvalSymlinks(initHookGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func newHookInterruptCase(t *testing.T, key string) hookInterruptCase {
	t.Helper()
	homePath := t.TempDir()
	repo := hermeticHookRepo(t)
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:receiver"
	item, err := store.Create(work.CreateInput{
		Owner: actor, SuccessPredicate: "eligible coordination interrupt blocks execution",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(item.ID, actor, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runAttemptID := claimed.RunAttemptIDs[len(claimed.RunAttemptIDs)-1]
	action, decision := hookInterruptEvidence(item.ID, actor, runAttemptID, now)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(action.ID, action, "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(decision.ID, decision, "  "); err != nil {
		t.Fatal(err)
	}
	if err := runattempt.Open(homePath).AttachActionIntent(runAttemptID, action.ID); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("interrupt evidence body " + key))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := store.RecordCollaboration(item.ID, work.CollaborationInput{
		Actor: actor, IdempotencyKey: key, BodyRef: bodyRef,
		DeliveryClass: work.CollaborationDeliveryInterrupt,
		Delta: work.CollaborationStateDelta{
			Kind: "run_attempt", SubjectRef: runAttemptID, Before: "active", After: "invalidated",
		},
		Affects: work.CollaborationAffects{
			WorkIDs: []string{item.ID}, RunAttemptIDs: []string{runAttemptID},
			PolicyRefs: []string{decision.ID},
		},
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	toolAction := action
	toolAction.ID = "act_pretool_" + strings.ReplaceAll(key, "-", "_")
	toolAction.Action = "write_file"
	toolAction.Target = envelope.ActionTarget{Kind: "file", Path: "README.md"}
	return hookInterruptCase{
		homePath: homePath, repo: repo, store: store, now: now, event: recorded.Event,
		fixture: Fixture{
			Hook: "pre-tool-use", Harness: "codex", Live: true, Cwd: repo, WorkID: item.ID, Action: &toolAction,
			CoordinationDelivery: &CoordinationDeliveryInput{
				CursorKey: "interrupt-" + key,
				Recipient: hookplan.DeliveryRecipient{
					Actor: actor, WorkID: item.ID, RunAttemptID: runAttemptID,
					PolicyRefs: []string{decision.ID},
				},
				Trigger: hookplan.DeliveryTriggerInterrupt,
			},
		},
	}
}

func hookInterruptEvidence(workID, actor, runAttemptID string, now time.Time) (envelope.ActionIntent, envelope.PolicyDecision) {
	action := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: "act_hook_interrupt_evidence",
			CreatedAt: now.UTC().Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:       envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:   "private", Risk: "tier2", EvidenceRefs: []string{},
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: "work.execute", Action: "continue_run",
		Target:        envelope.ActionTarget{Kind: "work", ID: workID},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "repo",
	}
	decision := envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision, ID: "pol_hook_interrupt_evidence",
			CreatedAt: now.Add(time.Second).UTC().Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "policy", Name: "compound"},
			IDs:       envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:   "private", Risk: "tier2", EvidenceRefs: []string{runAttemptID},
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: action.ID, Outcome: "hold",
		Reasons: []string{"active action invalidated"}, Obligations: []string{},
	}
	return action, decision
}

// rewriteSegmentInLaterTick replaces a segment in place the way a foreign writer
// in a later filesystem tick would. Hot-segment reads are stat-guarded per
// process, and the corruption keeps the segment size. On a coarse clock (Linux
// CI) a same-tick rewrite keeps the mtime too, so this process's warm cache would
// still serve the old frames. A real hook starts with a cold cache.
func rewriteSegmentInLaterTick(t *testing.T, path string, data []byte) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	later := before.ModTime().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}
