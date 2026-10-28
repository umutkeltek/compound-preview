package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

func nativeFixtureForAuthority(t *testing.T) nativeSessionFixture {
	t.Helper()
	return launchNativeSessionForTest(t, newStore(t), "actor:recipient", "claude-code", "native-authority-session", t.TempDir())
}

func nativeMustRefuse(t *testing.T, fixture nativeSessionFixture, query CollaborationRecipientQuery) {
	t.Helper()
	result, ok, err := fixture.store.ResolveCollaborationRecipient(query)
	if ok || (err == nil && result.UnresolvedReason == "") {
		t.Fatalf("invalid authority silently accepted or absent: ok=%v result=%+v err=%v", ok, result, err)
	}
}

func replaceNativeEventFixture(t *testing.T, store *Store, id string, value any) {
	t.Helper()
	if err := store.events.Remove(id); err != nil {
		t.Fatal(err)
	}
	if err := store.writeEventJSON(id, value); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCollaborationCurrentGenerationMustMatchIndependently(t *testing.T) {
	for _, changed := range []string{"action", "policy"} {
		t.Run(changed, func(t *testing.T) {
			fixture := nativeFixtureForAuthority(t)
			item, err := fixture.store.Get(fixture.workID)
			if err != nil {
				t.Fatal(err)
			}
			if changed == "action" {
				item.ActionIntentIDs = append(item.ActionIntentIDs, "act_replacement_generation")
			} else {
				item.PolicyDecisionID = "pol_replacement_generation"
			}
			if item.State != "running" {
				t.Fatal("test must retain the running state")
			}
			if err := fixture.store.writeJSON(fixture.store.workPath(item.ID), item); err != nil {
				t.Fatal(err)
			}
			nativeMustRefuse(t, fixture, fixture.query())
		})
	}
}

func TestNativeCollaborationExactAuthorityBodiesAndLease(t *testing.T) {
	for _, changed := range []string{"missing lease", "reassigned lease", "wrong work lease", "invalid action", "wrong policy body id"} {
		t.Run(changed, func(t *testing.T) {
			fixture := nativeFixtureForAuthority(t)
			store := fixture.store
			switch changed {
			case "missing lease":
				if err := os.Remove(store.leasePath(fixture.workID)); err != nil {
					t.Fatal(err)
				}
			case "reassigned lease", "wrong work lease":
				lease, err := store.readLease(fixture.workID)
				if err != nil {
					t.Fatal(err)
				}
				if changed == "reassigned lease" {
					lease.By = "actor:other"
				} else {
					lease.WorkID = "work_other"
				}
				if err := store.writeJSON(store.leasePath(fixture.workID), lease); err != nil {
					t.Fatal(err)
				}
			case "invalid action":
				id := fixtureActionID(t, store, fixture.workID)
				var action envelope.ActionIntent
				if err := store.readEventJSON(id, &action); err != nil {
					t.Fatal(err)
				}
				action.Capability = ""
				if action.Validate() == nil {
					t.Fatal("control: malformed action must fail validation")
				}
				replaceNativeEventFixture(t, store, id, action)
			case "wrong policy body id":
				id := storePolicyDecisionID(t, store, fixture.workID)
				var decision envelope.PolicyDecision
				if err := store.readEventJSON(id, &decision); err != nil {
					t.Fatal(err)
				}
				decision.ID = "pol_different_valid_body"
				if err := decision.Validate(); err != nil {
					t.Fatalf("control needs valid mismatched body: %v", err)
				}
				// The event store already rejects key/body identity mismatch
				// before the native consumer can read it.
				if err := store.writeEventJSON(id, decision); err == nil {
					t.Fatal("event store accepted a mismatched policy body identity")
				}
				return
			}
			nativeMustRefuse(t, fixture, fixture.query())
		})
	}
}

func TestNativeCollaborationStopFencesTerminalWithoutForwardLinkOrLiveWork(t *testing.T) {
	for _, disposition := range []string{"live work", "cancelled work", "completed work", "archived work"} {
		t.Run(disposition, func(t *testing.T) {
			fixture := nativeFixtureForAuthority(t)
			store := fixture.store
			attempts := runattempt.Open(store.home)
			if _, err := attempts.Complete(fixture.attemptID, runattempt.StatusFailed, fixedNow.Add(9*time.Second)); err != nil {
				t.Fatal(err)
			}
			item, err := store.Get(fixture.workID)
			if err != nil {
				t.Fatal(err)
			}
			if collaborationContains(item.RunAttemptIDs, fixture.attemptID) {
				t.Fatal("test must preserve the reservation-loss/no-forward-link case")
			}
			if disposition == "cancelled work" || disposition == "completed work" {
				item.State = "cancelled"
				if disposition == "completed work" {
					item.State = "completed"
				}
				if err := store.writeJSON(store.workPath(item.ID), item); err != nil {
					t.Fatal(err)
				}
			}
			if disposition == "archived work" {
				// The fence must survive removal from the live work directory,
				// independent of archive layout or an envelope forward-link.
				if err := os.Rename(store.workPath(item.ID), filepath.Join(t.TempDir(), item.ID+".json")); err != nil {
					t.Fatal(err)
				}
			}
			query := fixture.query()
			query.CheckPriorBindings = true
			nativeMustRefuse(t, fixture, query)
		})
	}
}

func TestNativeCollaborationSnapshotHarnessAndPinEvidence(t *testing.T) {
	for _, changed := range []string{"attempt harness", "snapshot harness", "derived", "unobservable", "generic pinned flag"} {
		t.Run(changed, func(t *testing.T) {
			fixture := nativeFixtureForAuthority(t)
			path := filepath.Join(fixture.store.home, "run_attempts", fixture.attemptID+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var attempt runattempt.RunAttempt
			if err := json.Unmarshal(data, &attempt); err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "attempt harness":
				attempt.Harness = "codex"
			case "snapshot harness":
				attempt.ExecutionSelection.Harness = "codex"
			case "derived":
				attempt.ExecutionSelection.NativeSessionEvidence = "derived:session-store"
			case "unobservable":
				attempt.ExecutionSelection.NativeSessionID = ""
				attempt.ExecutionSelection.NativeSessionEvidence = "unobservable:fixture"
			case "generic pinned flag":
				attempt.ExecutionSelection.NativeSessionEvidence = "pinned:--native-session"
			}
			data, err = json.Marshal(attempt)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			result, ok, err := fixture.store.ResolveCollaborationRecipient(fixture.query())
			if changed == "generic pinned flag" {
				if err != nil || !ok || result.RunAttemptID != fixture.attemptID {
					t.Fatalf("valid generic pin rejected: %+v %v", result, err)
				}
			} else if ok {
				t.Fatalf("weaker or inconsistent snapshot resolved: %+v", result)
			}
		})
	}
}
