package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// nativeSessionFixture is one governed native launch: a work reserved in
// running under its own ActionIntent/PolicyDecision, and one harness-execution
// attempt whose copied selection snapshot pins a session id, whose posture
// carries the exact running policy decision, and whose launch committed.
type nativeSessionFixture struct {
	store     *Store
	workID    string
	actor     string
	workspace string
	attemptID string
	sessionID string
}

func (f nativeSessionFixture) query() CollaborationRecipientQuery {
	return CollaborationRecipientQuery{Harness: "claude-code", SessionID: f.sessionID, Cwd: f.workspace}
}

// launchNativeSessionForTest drives the real store/runattempt APIs a governed
// launch uses: claim, advance to running (minting the act/pol reservation),
// mint the attempt against that exact action, stamp origin/workspace/snapshot/
// posture, then commit the launch so the durable prepared|committed boundary
// exists. The envelope never receives the attempt id: native resolution must
// work before the forward link exists.
func launchNativeSessionForTest(
	t *testing.T,
	store *Store,
	actor string,
	harness string,
	sessionID string,
	workspace string,
) nativeSessionFixture {
	t.Helper()
	item, err := store.Create(CreateInput{
		Owner: actor, SuccessPredicate: "native session recipient resolution",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(item.ID, actor, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	running, err := store.Advance(item.ID, AdvanceInput{To: "running", Actor: actor}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(running.ActionIntentIDs) == 0 || running.PolicyDecisionID == "" {
		t.Fatalf("running advance minted no authority: %+v", running)
	}
	attempts := runattempt.Open(store.home)
	attempt, err := attempts.MintWithActionIntent(
		item.ID, harness, harness, running.ActionIntentIDs[len(running.ActionIntentIDs)-1], fixedNow.Add(3*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.SetWorkspace(attempt.ID, workspace); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.SetExecutionSelectionSnapshot(attempt.ID, runattempt.ExecutionSelectionSnapshot{
		Source: "override", Harness: harness,
		NativeSessionID:       sessionID,
		NativeSessionEvidence: "pinned:--session-id",
	}); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.SetExecutionPosture(attempt.ID, runattempt.ExecutionPosture{
		PermissionMode:   "plan",
		PolicyDecisionID: running.PolicyDecisionID,
	}); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.CommitLaunch(attempt.ID, fixedNow.Add(4*time.Second), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	attached, err := store.Get(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range attached.RunAttemptIDs {
		if id == attempt.ID {
			t.Fatal("test fixture must not forward-link the attempt; native resolution is pre-attachment")
		}
	}
	return nativeSessionFixture{
		store: store, workID: item.ID, actor: actor, workspace: workspace,
		attemptID: attempt.ID, sessionID: sessionID,
	}
}

// TestNativeCollaborationRecipientResolvesBeforeForwardLink pins the load
// bearing case: the exact active native recipient resolves from launch
// evidence alone, with no envelope attachment and no work mutation.
func TestNativeCollaborationRecipientResolvesBeforeForwardLink(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	workspace := t.TempDir()
	fixture := launchNativeSessionForTest(t, store, "codex:recipient", "claude-code", "native-session-one", workspace)
	before, err := store.eventsFor(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := store.ResolveCollaborationRecipient(fixture.query())
	if err != nil || !ok {
		t.Fatalf("exact active native session did not resolve: ok=%v resolution=%+v err=%v", ok, resolved, err)
	}
	if resolved.Actor != "codex:recipient" || resolved.WorkID != fixture.workID || resolved.RunAttemptID != fixture.attemptID {
		t.Fatalf("native recipient = %+v", resolved)
	}
	if len(resolved.PolicyRefs) == 0 || !collaborationContains(resolved.PolicyRefs, storePolicyDecisionID(t, store, fixture.workID)) {
		t.Fatalf("native recipient carried no exact current policy reference: %+v", resolved.PolicyRefs)
	}
	after, err := store.eventsFor(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("native resolution mutated the work lifecycle: %d -> %d events", len(before), len(after))
	}
}

func storePolicyDecisionID(t *testing.T, store *Store, workID string) string {
	t.Helper()
	item, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	return item.PolicyDecisionID
}

// TestNativeCollaborationRecipientRefusesEveryWeakerCoordinate is the native
// falsifier set: each weaker or stale coordinate must refuse, and a known
// session may never resolve through a second looser path.
func TestNativeCollaborationRecipientRefusesEveryWeakerCoordinate(t *testing.T) {
	t.Parallel()

	t.Run("wrong session harness and workspace", func(t *testing.T) {
		store := newStore(t)
		workspace := t.TempDir()
		launchNativeSessionForTest(t, store, "codex:recipient", "claude-code", "native-session-one", workspace)
		outside := t.TempDir()
		for _, tt := range []struct {
			name  string
			query CollaborationRecipientQuery
			fence bool
		}{
			{name: "wrong session", query: CollaborationRecipientQuery{Harness: "claude-code", SessionID: "another-session", Cwd: workspace}},
			{name: "wrong harness", query: CollaborationRecipientQuery{Harness: "codex", SessionID: "native-session-one", Cwd: workspace}},
			// A KNOWN pinned session probed from outside its workspace is
			// fenced, not silently absent: prose must not adopt it.
			{name: "wrong workspace", query: CollaborationRecipientQuery{Harness: "claude-code", SessionID: "native-session-one", Cwd: outside}, fence: true},
		} {
			resolved, ok, err := store.ResolveCollaborationRecipient(tt.query)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if ok || (resolved.UnresolvedReason != "") != tt.fence {
				t.Fatalf("%s resolved or mis-fenced a session: ok=%v resolution=%+v", tt.name, ok, resolved)
			}
		}
	})

	t.Run("manual unlaunched and unsnapshotted attempts never resolve", func(t *testing.T) {
		store := newStore(t)
		workspace := t.TempDir()
		fixture := launchNativeSessionForTest(t, store, "codex:recipient", "claude-code", "native-session-one", workspace)
		attempts := runattempt.Open(store.home)
		// A manual/ungoverned attempt with no authority, snapshot, or launch.
		manual, err := attempts.Mint(fixture.workID, "codex:recipient", "claude-code", fixedNow.Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.SetWorkspace(manual.ID, workspace); err != nil {
			t.Fatal(err)
		}
		// A governed attempt whose launch never pinned a session.
		unsnapshotted, err := attempts.MintWithActionIntent(
			fixture.workID, "claude-code", "claude-code", fixtureActionID(t, store, fixture.workID), fixedNow.Add(6*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.SetOrigin(unsnapshotted.ID, runattempt.OriginHarnessExecution); err != nil {
			t.Fatal(err)
		}
		// A manual run that pins a session but carries no governing
		// authority: the pinned id alone must not make it a recipient.
		if _, err := attempts.SetOrigin(manual.ID, runattempt.OriginHarnessExecution); err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.SetWorkspace(manual.ID, workspace); err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.SetExecutionSelectionSnapshot(manual.ID, runattempt.ExecutionSelectionSnapshot{
			Source: "override", Harness: "claude-code",
			NativeSessionID:       "native-session-manual",
			NativeSessionEvidence: "pinned:--session-id",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.MarkRunning(manual.ID, fixedNow.Add(7*time.Second)); err != nil {
			t.Fatal(err)
		}
		for _, tt := range []struct {
			name    string
			session string
			fence   bool
		}{
			{"manual attempt session", "native-session-manual", true},
			// An empty session id is incomplete coordinates, never absence.
			{"session-less attempt", "", true},
		} {
			query := CollaborationRecipientQuery{Harness: "claude-code", Cwd: workspace}
			query.SessionID = tt.session
			resolved, ok, err := store.ResolveCollaborationRecipient(query)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if ok || (resolved.UnresolvedReason != "") != tt.fence {
				t.Fatalf("%s resolved or mis-fenced: ok=%v resolution=%+v", tt.name, ok, resolved)
			}
		}
	})

	t.Run("superseded authority and non-running work refuse", func(t *testing.T) {
		store := newStore(t)
		workspace := t.TempDir()
		fixture := launchNativeSessionForTest(t, store, "codex:recipient", "claude-code", "native-session-one", workspace)
		// Another lifecycle mutation mints a newer act/pol generation: the
		// launch's reservation is superseded even though the attempt still runs.
		if _, err := store.Advance(fixture.workID, AdvanceInput{To: "running", Actor: "codex:recipient"}, fixedNow.Add(7*time.Second)); err != nil {
			t.Fatal(err)
		}
		superseded, ok, err := store.ResolveCollaborationRecipient(fixture.query())
		if err != nil {
			t.Fatal(err)
		}
		if ok || superseded.UnresolvedReason == "" {
			t.Fatalf("superseded authority resolved: ok=%v resolution=%+v", ok, superseded)
		}
		held := newStore(t)
		heldFixture := launchNativeSessionForTest(t, held, "codex:holder", "claude-code", "native-session-two", t.TempDir())
		if _, err := held.Advance(heldFixture.workID, AdvanceInput{To: "held", Actor: "codex:holder"}, fixedNow.Add(8*time.Second)); err != nil {
			t.Fatal(err)
		}
		stopped, ok, err := held.ResolveCollaborationRecipient(heldFixture.query())
		if err != nil {
			t.Fatal(err)
		}
		if ok || stopped.UnresolvedReason == "" {
			t.Fatalf("non-running work resolved: ok=%v resolution=%+v", ok, stopped)
		}
	})

	t.Run("prepared startup race refuses and a later committed boundary delivers", func(t *testing.T) {
		store := newStore(t)
		workspace := t.TempDir()
		fixture := launchNativeSessionForTest(t, store, "codex:recipient", "claude-code", "native-session-one", workspace)
		attempts := runattempt.Open(store.home)
		racing, err := attempts.MintWithActionIntent(
			fixture.workID, "claude-code", "claude-code", fixtureActionID(t, store, fixture.workID), fixedNow.Add(5*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.SetOrigin(racing.ID, runattempt.OriginHarnessExecution); err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.SetWorkspace(racing.ID, workspace); err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.SetExecutionSelectionSnapshot(racing.ID, runattempt.ExecutionSelectionSnapshot{
			Source: "override", Harness: "claude-code",
			NativeSessionID:       "native-session-race",
			NativeSessionEvidence: "pinned:--session-id",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.SetExecutionPosture(racing.ID, runattempt.ExecutionPosture{
			PermissionMode: "plan", PolicyDecisionID: storePolicyDecisionID(t, store, fixture.workID),
		}); err != nil {
			t.Fatal(err)
		}
		writePreparedOnlyLaunchEvidence(t, store.home, racing.ID, fixedNow.Add(6*time.Second))
		if _, err := attempts.MarkRunning(racing.ID, fixedNow.Add(6*time.Second)); err != nil {
			t.Fatal(err)
		}
		race := CollaborationRecipientQuery{Harness: "claude-code", SessionID: "native-session-race", Cwd: workspace}
		resolved, ok, err := store.ResolveCollaborationRecipient(race)
		if err != nil {
			t.Fatal(err)
		}
		if ok || resolved.UnresolvedReason == "" {
			t.Fatalf("prepared-only startup race resolved: ok=%v resolution=%+v", ok, resolved)
		}
		// The committed boundary on the same attempt may deliver later. The
		// store refuses a re-commit once the attempt left pending, so the
		// disposition is published beside the durable prepared boundary.
		if _, err := attempts.CommitLaunch(racing.ID, fixedNow.Add(7*time.Second), func() error { return nil }); err == nil {
			t.Fatal("re-committing an already-running launch should refuse")
		}
		// Commit the boundary the honest way: the durable sidecar already
		// records prepared, so publish the committed disposition beside it.
		writeCommittedLaunchEvidence(t, store.home, racing.ID, fixedNow.Add(6*time.Second))
		resolved, ok, err = store.ResolveCollaborationRecipient(race)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || resolved.RunAttemptID != racing.ID {
			t.Fatalf("committed boundary did not deliver: ok=%v resolution=%+v", ok, resolved)
		}
	})

	t.Run("start_error and terminal attempts refuse", func(t *testing.T) {
		store := newStore(t)
		workspace := t.TempDir()
		fixture := launchNativeSessionForTest(t, store, "codex:recipient", "claude-code", "native-session-one", workspace)
		attempts := runattempt.Open(store.home)
		failed := mintPinnedAttempt(t, store, attempts, fixture, workspace, "native-session-failed")
		if _, err := attempts.CommitLaunch(failed.ID, fixedNow.Add(5*time.Second), func() error { return os.ErrPermission }); err == nil {
			t.Fatal("start_error launch committed")
		}
		resolved, ok, err := store.ResolveCollaborationRecipient(CollaborationRecipientQuery{
			Harness: "claude-code", SessionID: "native-session-failed", Cwd: workspace,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok || resolved.UnresolvedReason == "" {
			t.Fatalf("start_error boundary resolved: ok=%v resolution=%+v", ok, resolved)
		}
		terminal := mintPinnedAttempt(t, store, attempts, fixture, workspace, "native-session-terminal")
		if _, err := attempts.CommitLaunch(terminal.ID, fixedNow.Add(6*time.Second), func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.Complete(terminal.ID, runattempt.StatusFailed, fixedNow.Add(7*time.Second)); err != nil {
			t.Fatal(err)
		}
		resolved, ok, err = store.ResolveCollaborationRecipient(CollaborationRecipientQuery{
			Harness: "claude-code", SessionID: "native-session-terminal", Cwd: workspace,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok || resolved.UnresolvedReason != "" {
			t.Fatalf("terminal native session was treated as known live evidence: ok=%v resolution=%+v", ok, resolved)
		}
	})
}

// TestNativeCollaborationRecipientDuplicateLiveSessionRefuses pins the
// unambiguity fence: two live launches pinning one session id must refuse
// rather than guess which context owns it.
func TestNativeCollaborationRecipientDuplicateLiveSessionRefuses(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	workspace := t.TempDir()
	first := launchNativeSessionForTest(t, store, "codex:first", "claude-code", "shared-native-session", workspace)
	attempts := runattempt.Open(store.home)
	second, err := attempts.MintWithActionIntent(
		first.workID, "claude-code", "claude-code", fixtureActionID(t, store, first.workID), fixedNow.Add(5*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetOrigin(second.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetWorkspace(second.ID, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetExecutionSelectionSnapshot(second.ID, runattempt.ExecutionSelectionSnapshot{
		Source: "override", Harness: "claude-code",
		NativeSessionID:       "shared-native-session",
		NativeSessionEvidence: "pinned:--session-id",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetExecutionPosture(second.ID, runattempt.ExecutionPosture{
		PermissionMode: "plan", PolicyDecisionID: storePolicyDecisionID(t, store, first.workID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.CommitLaunch(second.ID, fixedNow.Add(6*time.Second), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := store.ResolveCollaborationRecipient(first.query())
	if err != nil {
		t.Fatal(err)
	}
	if ok || resolved.UnresolvedReason == "" {
		t.Fatalf("duplicate live native session resolved: ok=%v resolution=%+v", ok, resolved)
	}
}

// TestNativeCollaborationRecipientSuccessorIsIsolated pins successor isolation:
// after a failed attempt, its successor pins a FRESH session, and the
// predecessor's session never resolves again -- not even through the Stop
// fence's prose fallback.
func TestNativeCollaborationRecipientSuccessorIsIsolated(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	workspace := t.TempDir()
	first := launchNativeSessionForTest(t, store, "codex:recipient", "claude-code", "predecessor-session", workspace)
	attempts := runattempt.Open(store.home)
	if _, err := attempts.Complete(first.attemptID, runattempt.StatusFailed, fixedNow.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Attach the predecessor so the Stop fence can see it, then launch the
	// successor the governed way with a fresh session id.
	if _, err := store.Advance(first.workID, AdvanceInput{
		To: "verifying", Actor: first.actor, RunAttemptIDs: []string{first.attemptID},
	}, fixedNow.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(first.workID, AdvanceInput{To: "running", Actor: first.actor}, fixedNow.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	successor, err := attempts.MintWithActionIntent(
		first.workID, "claude-code", "claude-code", fixtureActionID(t, store, first.workID), fixedNow.Add(8*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetOrigin(successor.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetWorkspace(successor.ID, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetExecutionSelectionSnapshot(successor.ID, runattempt.ExecutionSelectionSnapshot{
		Source: "override", Harness: "claude-code",
		NativeSessionID:       "successor-session",
		NativeSessionEvidence: "pinned:--session-id",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetExecutionPosture(successor.ID, runattempt.ExecutionPosture{
		PermissionMode: "plan", PolicyDecisionID: storePolicyDecisionID(t, store, first.workID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.CommitLaunch(successor.ID, fixedNow.Add(9*time.Second), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if resolved, ok, err := store.ResolveCollaborationRecipient(CollaborationRecipientQuery{
		Harness: "claude-code", SessionID: "successor-session", Cwd: workspace,
	}); err != nil || !ok || resolved.RunAttemptID != successor.ID {
		t.Fatalf("successor session did not resolve: ok=%v resolution=%+v err=%v", ok, resolved, err)
	}
	predecessor, ok, err := store.ResolveCollaborationRecipient(CollaborationRecipientQuery{
		Harness: "claude-code", SessionID: "predecessor-session", Cwd: workspace, CheckPriorBindings: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok || predecessor.UnresolvedReason == "" {
		t.Fatalf("predecessor session was adoptable or silently absent: ok=%v resolution=%+v", ok, predecessor)
	}
}

// TestNativeCollaborationCrossWorkBodyDelivery proves the delivery path end to
// end through a NATIVE recipient: another work's event names this recipient's
// exact run attempt and work, and the body door releases the source-owned body
// with no lifecycle or acknowledgement mutation anywhere.
func TestNativeCollaborationCrossWorkBodyDelivery(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	workspace := t.TempDir()
	fixture := launchNativeSessionForTest(t, store, "claude:recipient", "claude-code", "native-handoff-session", workspace)
	const sender = "codex:sender"
	source, err := store.Create(CreateInput{
		Owner: sender, SuccessPredicate: "native cross-work body reaches the exact recipient",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(source.ID, sender, fixedNow.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	bodyRef := collaborationBodyRef(t, store, "native cross-work governed body")
	recorded, err := store.RecordCollaboration(source.ID, CollaborationInput{
		Actor: sender, IdempotencyKey: "native-cross-work-handoff",
		TargetActor: "claude:recipient",
		BodyRef:     bodyRef, DeliveryClass: CollaborationDeliveryNextBoundary,
		Delta: CollaborationStateDelta{Kind: "work", SubjectRef: source.ID, After: "changed"},
		Affects: CollaborationAffects{
			Actors:        []string{"claude:recipient"},
			WorkIDs:       []string{fixture.workID},
			RunAttemptIDs: []string{fixture.attemptID},
		},
	}, fixedNow.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	beforeSource, err := store.eventsFor(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRecipient, err := store.eventsFor(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.RetrieveCollaborationBody(CollaborationBodyRequest{
		WorkID: source.ID, EventID: recorded.Event.ID, Actor: fixture.actor,
		RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
	})
	if err != nil {
		t.Fatalf("native cross-work body refused for the exact recipient: %v", err)
	}
	if string(got.Body) != "native cross-work governed body" {
		t.Fatalf("native cross-work body = %q", got.Body)
	}
	if _, err := store.RetrieveCollaborationBody(CollaborationBodyRequest{
		WorkID: source.ID, EventID: recorded.Event.ID, Actor: "claude:impostor",
		RunAttemptID: fixture.attemptID, SessionID: fixture.sessionID,
	}); err == nil {
		t.Fatal("an impostor actor fetched the native cross-work body")
	}
	afterSource, err := store.eventsFor(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRecipient, err := store.eventsFor(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSource) != len(beforeSource) || len(afterRecipient) != len(beforeRecipient) {
		t.Fatalf("body fetch mutated a lifecycle: source=%d/%d recipient=%d/%d",
			len(beforeSource), len(afterSource), len(beforeRecipient), len(afterRecipient))
	}
}

func fixtureActionID(t *testing.T, store *Store, workID string) string {
	t.Helper()
	item, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.ActionIntentIDs) == 0 {
		t.Fatal("fixture work has no authorizing action")
	}
	return item.ActionIntentIDs[len(item.ActionIntentIDs)-1]
}

func mintPinnedAttempt(
	t *testing.T,
	store *Store,
	attempts *runattempt.Store,
	fixture nativeSessionFixture,
	workspace string,
	sessionID string,
) runattempt.RunAttempt {
	t.Helper()
	attempt, err := attempts.MintWithActionIntent(
		fixture.workID, "claude-code", "claude-code", fixtureActionID(t, store, fixture.workID), fixedNow.Add(5*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetWorkspace(attempt.ID, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetExecutionSelectionSnapshot(attempt.ID, runattempt.ExecutionSelectionSnapshot{
		Source: "override", Harness: "claude-code",
		NativeSessionID:       sessionID,
		NativeSessionEvidence: "pinned:--session-id",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetExecutionPosture(attempt.ID, runattempt.ExecutionPosture{
		PermissionMode: "plan", PolicyDecisionID: storePolicyDecisionID(t, store, fixture.workID),
	}); err != nil {
		t.Fatal(err)
	}
	return attempt
}

// writePreparedOnlyLaunchEvidence publishes the durable prepared boundary by
// hand for the startup-race fixture. Content-free and exactly the shape
// internal/runattempt writes, so Store.Get hydrates it as a real sidecar.
func writePreparedOnlyLaunchEvidence(t *testing.T, homePath, attemptID string, at time.Time) {
	t.Helper()
	writeLaunchEvidenceSidecar(t, homePath, attemptID, "prepared", at)
}

func writeCommittedLaunchEvidence(t *testing.T, homePath, attemptID string, at time.Time) {
	t.Helper()
	writeLaunchEvidenceSidecar(t, homePath, attemptID, "committed", at)
}

func writeLaunchEvidenceSidecar(t *testing.T, homePath, attemptID, kind string, at time.Time) {
	t.Helper()
	directory := filepath.Join(homePath, "run_attempts", "launch")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(struct {
		SchemaVersion string    `json:"schema_version"`
		AttemptID     string    `json:"attempt_id"`
		Kind          string    `json:"kind"`
		At            time.Time `json:"at"`
	}{
		SchemaVersion: "compound.runattempt.launch_evidence.v0.1",
		AttemptID:     attemptID, Kind: kind, At: at.UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, attemptID+"."+kind+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
