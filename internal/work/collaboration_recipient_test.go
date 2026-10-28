package work

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestResolveCollaborationRecipientPriorBindingFence(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"terminal", "lost-attachment", "terminal-work", "wrong-workspace"} {
		t.Run(mode, func(t *testing.T) {
			store := newStore(t)
			workspace := t.TempDir()
			item, err := store.Create(CreateInput{Owner: "codex:holder", SuccessPredicate: "stale identity is not absent"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(item.ID, "codex:holder", fixedNow); err != nil {
				t.Fatal(err)
			}
			attempt := bindCollaborationRecipientForTest(t, store, item.ID, "codex:holder", "codex:holder", "codex", "known-session", workspace, fixedNow)
			current, err := store.Get(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "terminal" {
				if _, err := runattempt.Open(store.home).Complete(attempt.ID, runattempt.StatusFailed, fixedNow.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			} else if mode != "wrong-workspace" {
				if mode == "lost-attachment" {
					current.RunAttemptIDs = nil
				} else {
					current.State = "completed"
				}
				if err := store.writeJSON(store.workPath(item.ID), current); err != nil {
					t.Fatal(err)
				}
			}
			query := CollaborationRecipientQuery{Harness: "codex", SessionID: "known-session", Cwd: workspace}
			if mode == "wrong-workspace" {
				query.Cwd = t.TempDir()
			}
			before := store.eventLogReadCount()
			resolved, ok, err := store.ResolveCollaborationRecipient(query)
			if err != nil || ok || resolved.UnresolvedReason != "" || store.eventLogReadCount() != before {
				t.Fatalf("ordinary hook consulted historical binding: ok=%v resolution=%+v reads=%d err=%v", ok, resolved, store.eventLogReadCount()-before, err)
			}
			query.CheckPriorBindings = true
			started := time.Now()
			resolved, ok, err = store.ResolveCollaborationRecipient(query)
			elapsed := time.Since(started)
			if err != nil || ok || resolved.UnresolvedReason == "" {
				t.Fatalf("strict Stop discarded stale typed identity: ok=%v resolution=%+v err=%v", ok, resolved, err)
			}
			if delta := store.eventLogReadCount() - before; delta != 1 {
				t.Fatalf("strict absence fence loaded work history %d times, want 1", delta)
			}
			info, err := os.Stat(filepath.Join(store.home, "events", "work.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("synthetic strict Stop fence: history_bytes=%d snapshots=1 elapsed=%s; not populated-home latency", info.Size(), elapsed)
		})
	}
}

func TestResolveCollaborationRecipientPriorBindingPreservesCurrent(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	workspace := t.TempDir()
	item, err := store.Create(CreateInput{Owner: "codex:holder", SuccessPredicate: "new binding supersedes old"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(item.ID, "codex:holder", fixedNow); err != nil {
		t.Fatal(err)
	}
	old := bindCollaborationRecipientForTest(t, store, item.ID, "codex:holder", "codex:holder", "codex", "old-session", workspace, fixedNow)
	if _, err := runattempt.Open(store.home).Complete(old.ID, runattempt.StatusFailed, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	current := bindCollaborationRecipientForTest(t, store, item.ID, "codex:holder", "codex:holder", "codex", "new-session", workspace, fixedNow.Add(2*time.Minute))
	query := CollaborationRecipientQuery{Harness: "codex", SessionID: "new-session", Cwd: workspace, CheckPriorBindings: true}
	before := store.eventLogReadCount()
	resolved, ok, err := store.ResolveCollaborationRecipient(query)
	if err != nil || !ok || resolved.RunAttemptID != current.ID || store.eventLogReadCount()-before != 1 {
		t.Fatalf("strict option displaced current binding or added history scan: ok=%v resolution=%+v reads=%d err=%v", ok, resolved, store.eventLogReadCount()-before, err)
	}
	query.SessionID = "old-session"
	resolved, ok, err = store.ResolveCollaborationRecipient(query)
	if err != nil || ok || resolved.UnresolvedReason == "" {
		t.Fatalf("superseded old session could regain authority through legacy fallback: ok=%v resolution=%+v err=%v", ok, resolved, err)
	}
	query.SessionID = "unrelated-session"
	resolved, ok, err = store.ResolveCollaborationRecipient(query)
	if err != nil || ok || resolved.UnresolvedReason != "" {
		t.Fatalf("history poisoned unrelated never-owned session: ok=%v resolution=%+v err=%v", ok, resolved, err)
	}
}

func TestResolveCollaborationRecipientPriorBindingReadFailureRefuses(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"work-log", "delegate-event"} {
		t.Run(source, func(t *testing.T) {
			store := newStore(t)
			workspace := t.TempDir()
			item, err := store.Create(CreateInput{Owner: "codex:holder", SuccessPredicate: "unreadable prior identity refuses"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(item.ID, "codex:holder", fixedNow); err != nil {
				t.Fatal(err)
			}
			attempt := bindCollaborationRecipientForTest(t, store, item.ID, "codex:holder", "codex:holder", "codex", "known-session", workspace, fixedNow)
			if _, err := runattempt.Open(store.home).Complete(attempt.ID, runattempt.StatusFailed, fixedNow.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if source == "delegate-event" {
				// Attach a missing owned record, not a guessed physical file:
				// eventstore may already custody the real spawn in a segment.
				if err := store.appendEvent(Event{
					TS: ts(fixedNow.Add(2 * time.Minute)), WorkID: item.ID,
					From: "claimed", To: "claimed", By: "codex:holder", Attached: []string{"wde_missing_binding"},
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(store.home, "events", "work.jsonl"), []byte("{\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			query := CollaborationRecipientQuery{Harness: "codex", SessionID: "known-session", Cwd: workspace, CheckPriorBindings: true}
			resolved, ok, err := store.ResolveCollaborationRecipient(query)
			if err == nil || ok {
				t.Fatalf("unreadable owned history permitted absence: work=%s ok=%v resolution=%+v err=%v", item.ID, ok, resolved, err)
			}
		})
	}
}

func TestResolveCollaborationRecipientRequiresExactOwnedSessionBinding(t *testing.T) {
	t.Parallel()
	t.Run("sole workspace candidate does not authorize unknown session", func(t *testing.T) {
		store := newStore(t)
		workspace := t.TempDir()
		item, err := store.Create(CreateInput{
			Owner: "codex:holder", SuccessPredicate: "native session authority is exact",
		}, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(item.ID, "codex:holder", fixedNow.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		attempt := bindCollaborationRecipientForTest(
			t, store, item.ID, "codex:holder", "codex:holder", "codex", "owned-session", workspace,
			fixedNow.Add(2*time.Second),
		)
		binding, alive, err := store.latestDelegateBinding(item.ID)
		if err != nil || !alive || binding == nil || binding.Session != "owned-session" ||
			binding.RunAttemptID != attempt.ID {
			t.Fatalf("test binding was not current: binding=%+v alive=%v err=%v", binding, alive, err)
		}
		current, err := store.Get(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !collaborationContains(current.RunAttemptIDs, attempt.ID) ||
			attempt.Origin != runattempt.OriginHarnessExecution ||
			attempt.Workspace != workspace {
			t.Fatalf("test attempt was not resolvable: work=%+v attempt=%+v", current, attempt)
		}

		resolved, ok, err := store.ResolveCollaborationRecipient(CollaborationRecipientQuery{
			Harness: "codex", SessionID: "owned-session", Cwd: workspace,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !ok || resolved.RunAttemptID != attempt.ID {
			t.Fatalf("exact owned session did not resolve: ok=%v recipient=%+v", ok, resolved)
		}
		if unknown, ok, err := store.ResolveCollaborationRecipient(CollaborationRecipientQuery{
			Harness: "codex", SessionID: "unknown-session", Cwd: workspace,
		}); err != nil || ok || !reflect.DeepEqual(unknown, CollaborationRecipientResolution{}) {
			t.Fatalf("unknown session inherited sole workspace recipient: ok=%v recipient=%+v err=%v",
				ok, unknown, err)
		}
	})

	t.Run("two sessions sharing workspace and harness select only exact binding", func(t *testing.T) {
		store := newStore(t)
		workspace := t.TempDir()
		var attempts []runattempt.RunAttempt
		for index, session := range []string{"session-one", "session-two"} {
			item, err := store.Create(CreateInput{
				Owner: "codex:holder", SuccessPredicate: "session exactness " + session,
			}, fixedNow.Add(time.Duration(index)*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(item.ID, "codex:holder", fixedNow.Add(time.Duration(index+2)*time.Second)); err != nil {
				t.Fatal(err)
			}
			attempts = append(attempts, bindCollaborationRecipientForTest(
				t, store, item.ID, "codex:holder", "codex:holder", "codex", session, workspace,
				fixedNow.Add(time.Duration(index+4)*time.Second),
			))
		}
		for index, session := range []string{"session-one", "session-two"} {
			resolved, ok, err := store.ResolveCollaborationRecipient(CollaborationRecipientQuery{
				Harness: "codex", SessionID: session, Cwd: workspace,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !ok || resolved.RunAttemptID != attempts[index].ID {
				t.Fatalf("%s resolved recipient = %+v ok=%v, want %s",
					session, resolved, ok, attempts[index].ID)
			}
		}
		if unknown, ok, err := store.ResolveCollaborationRecipient(CollaborationRecipientQuery{
			Harness: "codex", SessionID: "unknown-session", Cwd: workspace,
		}); err != nil || ok || !reflect.DeepEqual(unknown, CollaborationRecipientResolution{}) {
			t.Fatalf("unknown session resolved among exact candidates: ok=%v recipient=%+v err=%v",
				ok, unknown, err)
		}
	})
}

func TestResolveCollaborationRecipientAmbiguousSpawnIsNotAbsent(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	workspace := t.TempDir()
	for _, id := range []string{"work_first", "work_second"} {
		item, err := store.Create(CreateInput{WorkID: id, Owner: "codex:holder", SuccessPredicate: "ambiguous session refuses"}, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(item.ID, "codex:holder", fixedNow); err != nil {
			t.Fatal(err)
		}
		bindCollaborationRecipientForTest(t, store, item.ID, "codex:holder", "codex:holder", "codex", "same-session", workspace, fixedNow)
	}
	resolved, ok, err := store.ResolveCollaborationRecipient(CollaborationRecipientQuery{Harness: "codex", SessionID: "same-session", Cwd: workspace})
	if err != nil || ok || resolved.UnresolvedReason == "" {
		t.Fatalf("ambiguous typed identity reported absence: ok=%v resolved=%+v err=%v", ok, resolved, err)
	}
}

func bindCollaborationRecipientForTest(
	t *testing.T,
	store *Store,
	workID string,
	leaseHolder string,
	recipientActor string,
	harness string,
	session string,
	workspace string,
	now time.Time,
) runattempt.RunAttempt {
	t.Helper()
	attempts := runattempt.Open(store.home)
	attempt, err := attempts.Mint(workID, recipientActor, harness, now)
	if err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.SetWorkspace(attempt.ID, workspace); err != nil {
		t.Fatal(err)
	}
	binding := &DelegateBinding{
		WorkID: workID, Session: session, Harness: harness, Holder: recipientActor,
		RecordedAt: now.UTC().Format(time.RFC3339), RunAttemptID: attempt.ID,
	}
	eventID, err := store.recordDelegateSpawned(workID, binding, false, attempt.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.appendDelegateLifecycleEvent(
		workID, item.State, leaseHolder, eventID, "test exact native session binding", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(workID, AdvanceInput{
		To: item.State, Actor: leaseHolder, RunAttemptIDs: []string{attempt.ID},
		Note: "test native run attached",
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.MarkRunning(attempt.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return attempt
}
