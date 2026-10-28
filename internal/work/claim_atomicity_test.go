package work

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestClaimRejectsClaimedWorkWithoutExactLeaseProof(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, *Store, envelope.WorkEnvelope, Lease)
	}{
		{
			name: "missing lease",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope, _ Lease) {
				t.Helper()
				if err := os.Remove(s.leasePath(claimed.ID)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed lease",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope, _ Lease) {
				t.Helper()
				if err := os.WriteFile(s.leasePath(claimed.ID), []byte("not-json\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lease names contender while Work and event name original actor",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope, lease Lease) {
				t.Helper()
				lease.By = "claimant-b"
				if err := s.writeJSON(s.leasePath(claimed.ID), &lease); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, claimed, lease := seedClaimProof(t)
			tt.corrupt(t, s, claimed, lease)
			beforeEvents := claimEvents(t, s, claimed.ID)

			if _, err := s.Claim(claimed.ID, "claimant-b", fixedNow.Add(2*time.Minute)); err == nil {
				t.Fatal("claim succeeded without exact Work/lease/event agreement")
			}
			persisted, err := s.Get(claimed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Authority.Owner != "claimant-a" {
				t.Fatalf("failed claim changed Work owner to %q", persisted.Authority.Owner)
			}
			if after := claimEvents(t, s, claimed.ID); !reflect.DeepEqual(after, beforeEvents) {
				t.Fatalf("failed claim changed events: before=%+v after=%+v", beforeEvents, after)
			}
		})
	}
}

func TestClaimIdempotencyRequiresExactOwnerLeaseAndEvent(t *testing.T) {
	t.Run("exact proof is idempotent", func(t *testing.T) {
		s, before, claimed, _ := seedClaimProof(t)
		beforeEvents := claimEvents(t, s, claimed.ID)
		reclaimed, err := s.Claim(claimed.ID, "claimant-a", fixedNow.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("exact idempotent claim: %v", err)
		}
		if !reflect.DeepEqual(reclaimed, claimed) {
			t.Fatalf("idempotent claim changed Work: before=%+v after=%+v", claimed, reclaimed)
		}
		assertExactClaimProof(t, s, before, claimed, "claimant-a")
		if after := claimEvents(t, s, claimed.ID); !reflect.DeepEqual(after, beforeEvents) {
			t.Fatalf("idempotent claim duplicated event: before=%+v after=%+v", beforeEvents, after)
		}
	})

	tests := []struct {
		name    string
		corrupt func(*testing.T, *Store, envelope.WorkEnvelope)
	}{
		{
			name: "Work owner differs",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope) {
				t.Helper()
				claimed.Authority.Owner = "claimant-z"
				if err := s.writeJSON(s.workPath(claimed.ID), &claimed); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lease timestamp differs",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope) {
				t.Helper()
				lease, err := s.readLease(claimed.ID)
				if err != nil {
					t.Fatal(err)
				}
				lease.ClaimAt = ts(fixedNow.Add(9 * time.Minute))
				if err := s.writeJSON(s.leasePath(claimed.ID), &lease); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "claim event is missing",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope) {
				t.Helper()
				rewriteClaimEvents(t, s, claimed.ID, func([]Event) []Event { return nil })
			},
		},
		{
			name: "claim event is duplicated",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope) {
				t.Helper()
				events := claimEvents(t, s, claimed.ID)
				if len(events) != 1 {
					t.Fatalf("claim events = %d, want one before duplication", len(events))
				}
				if err := s.appendEvent(events[0]); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "claim event actor differs",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope) {
				t.Helper()
				rewriteClaimEvents(t, s, claimed.ID, func(events []Event) []Event {
					events[0].By = "claimant-z"
					return events
				})
			},
		},
		{
			name: "claim event Work hash differs",
			corrupt: func(t *testing.T, s *Store, claimed envelope.WorkEnvelope) {
				t.Helper()
				rewriteClaimEvents(t, s, claimed.ID, func(events []Event) []Event {
					events[0].WorkSHA256 = strings.Repeat("0", 64)
					return events
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, claimed, _ := seedClaimProof(t)
			tt.corrupt(t, s, claimed)
			if _, err := s.Claim(claimed.ID, "claimant-a", fixedNow.Add(2*time.Minute)); err == nil {
				t.Fatal("idempotent claim succeeded without exact Work/lease/event agreement")
			}
		})
	}
}

func TestClaimRejectsConflictingDuplicateClaimEvidence(t *testing.T) {
	s, _, claimed, _ := seedClaimProof(t)
	events := claimEvents(t, s, claimed.ID)
	if len(events) != 1 {
		t.Fatalf("claim events = %d, want one before conflict", len(events))
	}
	conflict := events[0]
	conflict.By = "claimant-b"
	if err := s.appendEvent(conflict); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Claim(claimed.ID, "claimant-a", fixedNow.Add(2*time.Minute)); err == nil {
		t.Fatal("idempotent claim accepted conflicting duplicate authority evidence")
	}
}

func TestAdvanceAllowsExactLegacyClaimHolder(t *testing.T) {
	s, _, claimed, lease := seedClaimProof(t)
	lease.WorkSHA256 = ""
	if err := s.writeJSON(s.leasePath(claimed.ID), &lease); err != nil {
		t.Fatal(err)
	}
	rewriteClaimEvents(t, s, claimed.ID, func(events []Event) []Event {
		events[0].PreviousWorkSHA256 = ""
		events[0].WorkSHA256 = ""
		return events
	})

	advanced, err := s.Advance(claimed.ID, AdvanceInput{To: "running", Actor: "claimant-a"}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("exact legacy claim holder could not continue normal work: %v", err)
	}
	if advanced.State != "running" || advanced.Authority.Owner != "claimant-a" {
		t.Fatalf("legacy claim advance changed authority: %+v", advanced)
	}
}

func TestAdvanceAllowsLegacyClaimWithSameTimestampActionAudit(t *testing.T) {
	s, _, claimed, lease := seedClaimProof(t)
	lease.WorkSHA256 = ""
	if err := s.writeJSON(s.leasePath(claimed.ID), &lease); err != nil {
		t.Fatal(err)
	}
	rewriteClaimEvents(t, s, claimed.ID, func(events []Event) []Event {
		events[0].PreviousWorkSHA256 = ""
		events[0].WorkSHA256 = ""
		return events
	})
	at := fixedNow.Add(time.Minute)
	action := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_legacy_same_timestamp",
			CreatedAt:     at.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{WorkID: claimed.ID},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Capability:    "capability://skill:filesystem-write",
		Action:        "write_file",
		Target:        envelope.ActionTarget{Kind: "file", Path: "docs/example.md"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
	}
	decision := envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            "pol_legacy_same_timestamp",
			CreatedAt:     at.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: claimed.ID},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: action.ID,
		Outcome:        "allow",
		Obligations:    []string{},
	}
	if _, err := s.RecordAction(claimed.ID, action, decision, at); err != nil {
		t.Fatalf("record same-timestamp legacy action: %v", err)
	}

	advanced, err := s.Advance(claimed.ID, AdvanceInput{To: "running", Actor: "claimant-a"}, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("same-timestamp action audit was mistaken for a duplicate claim: %v", err)
	}
	if advanced.State != "running" {
		t.Fatalf("legacy work state = %q, want running", advanced.State)
	}
}

func TestClaimRecoversTornPublicationAsOneExactOriginalClaim(t *testing.T) {
	for _, boundary := range []string{"work", "lease"} {
		t.Run("after "+boundary+" publication", func(t *testing.T) {
			s := newStore(t)
			before, err := s.Create(CreateInput{Owner: "claim-test", SuccessPredicate: "recover one exact claim"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			claimAt := fixedNow.Add(time.Minute)
			crashClaimAtBoundary(t, s, before.ID, "claimant-a", claimAt, boundary)

			torn, err := s.Get(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if torn.State != "claimed" || torn.Authority.Owner != "claimant-a" {
				t.Fatalf("crash did not preserve claimed Work publication: %+v", torn)
			}
			if _, err := os.Stat(s.claimPendingPath(before.ID)); err != nil {
				t.Fatalf("crash did not preserve durable claim pending record: %v", err)
			}
			events := claimEvents(t, s, before.ID)
			if len(events) != 1 {
				t.Fatalf("durable claim events after crash = %d, want one: %+v", len(events), events)
			}
			if boundary == "work" {
				if _, err := s.readLease(before.ID); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("after-Work crash lease error = %v, want missing", err)
				}
			} else if lease, err := s.readLease(before.ID); err != nil || lease.By != "claimant-a" {
				t.Fatalf("after-Lease crash lease = %+v, err=%v", lease, err)
			}

			if _, err := s.Claim(before.ID, "claimant-b", fixedNow.Add(2*time.Minute)); err == nil {
				t.Fatal("different actor succeeded while recovering original torn claim")
			}
			recovered, err := s.Get(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertExactClaimProof(t, s, before, recovered, "claimant-a")
			if _, err := os.Stat(s.claimPendingPath(before.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovered pending record still exists: %v", err)
			}

			if _, err := s.Claim(before.ID, "claimant-a", fixedNow.Add(3*time.Minute)); err != nil {
				t.Fatalf("original claimant retry after recovery: %v", err)
			}
			if events := claimEvents(t, s, before.ID); len(events) != 1 {
				t.Fatalf("claim event count after repeated recovery = %d, want one: %+v", len(events), events)
			}
		})
	}
}

func TestAdvanceCannotOvertakeTornClaim(t *testing.T) {
	s := newStore(t)
	before, err := s.Create(CreateInput{Owner: "claim-test", SuccessPredicate: "recover claim before any other lifecycle writer"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	crashClaimAtBoundary(t, s, before.ID, "claimant-a", fixedNow.Add(time.Minute), "work")

	if _, err := s.Advance(before.ID, AdvanceInput{To: "running", Actor: "claimant-b"}, fixedNow.Add(2*time.Minute)); err == nil {
		t.Fatal("different actor advanced a torn claim before original ownership recovered")
	}
	recovered, err := s.Get(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExactClaimProof(t, s, before, recovered, "claimant-a")
	if _, err := os.Stat(s.claimPendingPath(before.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("advance did not converge pending claim before refusing contender: %v", err)
	}
	for _, event := range claimEvents(t, s, before.ID) {
		if event.To == "running" {
			t.Fatalf("contender published running event after torn claim: %+v", event)
		}
	}
}

func TestClaimRecoversAfterTornEventWrite(t *testing.T) {
	s := newStore(t)
	before, err := s.Create(CreateInput{Owner: "claim-test", SuccessPredicate: "recover after torn claim event write"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	previous := writeWorkEvent
	writeWorkEvent = func(file *os.File, data []byte) (int, error) {
		partial := len(data) / 2
		if partial == 0 {
			partial = 1
		}
		n, err := file.Write(data[:partial])
		if err != nil {
			return n, err
		}
		return n, io.ErrUnexpectedEOF
	}
	_, claimErr := s.Claim(before.ID, "claimant-a", fixedNow.Add(time.Minute))
	writeWorkEvent = previous
	if claimErr == nil {
		t.Fatal("injected torn claim event write unexpectedly succeeded")
	}
	t.Cleanup(func() { writeWorkEvent = previous })

	recovered, err := s.Claim(before.ID, "claimant-a", fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("retry could not recover after torn claim event write: %v", err)
	}
	assertExactClaimProof(t, s, before, recovered, "claimant-a")
	if _, err := os.Stat(s.claimPendingPath(before.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered pending record still exists: %v", err)
	}
}

func crashClaimAtBoundary(t *testing.T, s *Store, workID, actor string, now time.Time, boundary string) {
	t.Helper()
	token := &struct{}{}
	var restore func()
	switch boundary {
	case "work":
		previous := claimAfterWorkPublish
		claimAfterWorkPublish = func() { panic(token) }
		restore = func() { claimAfterWorkPublish = previous }
	case "lease":
		previous := claimAfterLeasePublish
		claimAfterLeasePublish = func() { panic(token) }
		restore = func() { claimAfterLeasePublish = previous }
	default:
		t.Fatalf("unknown crash boundary %q", boundary)
	}
	defer restore()

	crashed := false
	func() {
		defer func() {
			recovered := recover()
			if recovered == token {
				crashed = true
				return
			}
			if recovered != nil {
				panic(recovered)
			}
		}()
		if _, err := s.Claim(workID, actor, now); err != nil {
			t.Fatalf("claim before injected crash: %v", err)
		}
	}()
	if !crashed {
		t.Fatalf("claim did not reach injected %s publication crash seam", boundary)
	}
}

func seedClaimProof(t *testing.T) (*Store, envelope.WorkEnvelope, envelope.WorkEnvelope, Lease) {
	t.Helper()
	s := newStore(t)
	before, err := s.Create(CreateInput{Owner: "claim-test", SuccessPredicate: "claim proof stays exact"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(before.ID, "claimant-a", fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.readLease(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, before, claimed, lease
}

func claimEvents(t *testing.T, s *Store, workID string) []Event {
	t.Helper()
	events, err := s.eventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}
	claims := make([]Event, 0, 1)
	for _, event := range events {
		if event.To == "claimed" && event.WorkSHA256 != "" {
			claims = append(claims, event)
		}
	}
	return claims
}

func rewriteClaimEvents(t *testing.T, s *Store, workID string, rewrite func([]Event) []Event) {
	t.Helper()
	events, err := s.eventsFor("")
	if err != nil {
		t.Fatal(err)
	}
	claimIndex := -1
	for i, event := range events {
		if event.WorkID == workID && event.To == "claimed" {
			if claimIndex >= 0 {
				t.Fatal("expected one claim event before rewrite, found multiple")
			}
			claimIndex = i
		}
	}
	if claimIndex < 0 {
		t.Fatal("expected one claim event before rewrite, found none")
	}
	replacement := rewrite([]Event{events[claimIndex]})
	updated := append([]Event{}, events[:claimIndex]...)
	updated = append(updated, replacement...)
	updated = append(updated, events[claimIndex+1:]...)
	body := make([]byte, 0)
	for _, event := range updated {
		encoded, err := marshalWorkEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, encoded...)
	}
	if err := os.WriteFile(s.eventsLog(), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertExactClaimProof(t *testing.T, s *Store, before, claimed envelope.WorkEnvelope, actor string) {
	t.Helper()
	events := claimEvents(t, s, claimed.ID)
	if len(events) != 1 {
		t.Fatalf("claim events = %d, want exactly one: %+v", len(events), events)
	}
	event := events[0]
	lease, err := s.readLease(claimed.ID)
	if err != nil {
		t.Fatalf("read exact claim lease: %v", err)
	}
	previousHash, err := workEnvelopeSHA256(before)
	if err != nil {
		t.Fatal(err)
	}
	workHash, err := workEnvelopeSHA256(claimed)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != "claimed" || claimed.Authority.Owner != actor {
		t.Fatalf("claimed Work authority = state %q owner %q, want claimed/%q", claimed.State, claimed.Authority.Owner, actor)
	}
	if lease.WorkID != claimed.ID || lease.By != actor || lease.ClaimAt != event.TS || lease.WorkSHA256 != workHash {
		t.Fatalf("lease does not exactly match Work/event identity: lease=%+v event=%+v", lease, event)
	}
	if event.WorkID != claimed.ID || event.From != before.State || event.To != "claimed" || event.By != actor ||
		event.PreviousWorkSHA256 != previousHash || event.WorkSHA256 != workHash {
		t.Fatalf("claim event does not bind exact Work versions and actor: %+v", event)
	}
	// C1: claim events carry [action, policy] or [action, policy, ratt_*].
	if len(event.Attached) < 2 || len(event.Attached) > 3 {
		t.Fatalf("claim event attached = %v, want action/policy[/ratt_*]", event.Attached)
	}
	if len(event.Attached) == 3 && !strings.HasPrefix(event.Attached[2], "ratt_") {
		t.Fatalf("claim event third attached %q is not a ratt_ id", event.Attached[2])
	}
	actionID, policyID := event.Attached[0], event.Attached[1]
	if len(claimed.ActionIntentIDs) == 0 || claimed.ActionIntentIDs[len(claimed.ActionIntentIDs)-1] != actionID || claimed.PolicyDecisionID != policyID {
		t.Fatalf("claim event attachments disagree with Work: event=%v runs=%v policy=%q", event.Attached, claimed.ActionIntentIDs, claimed.PolicyDecisionID)
	}
	var action envelope.ActionIntent
	if err := s.readJSON(s.envPath(actionID), &action); err != nil {
		t.Fatalf("read claim action %s: %v", actionID, err)
	}
	var decision envelope.PolicyDecision
	if err := s.readJSON(s.envPath(policyID), &decision); err != nil {
		t.Fatalf("read claim policy %s: %v", policyID, err)
	}
	if action.ID != actionID || action.IDs.WorkID != claimed.ID || action.Actor != actor ||
		action.Capability != "work.claim" || action.Action != "claim_work" ||
		decision.ID != policyID || decision.IDs.WorkID != claimed.ID || decision.ActionIntentID != actionID ||
		(decision.Outcome != "allow" && decision.Outcome != "allow_with_obligations") ||
		event.Note != "claimed (policy "+decision.Outcome+")" {
		t.Fatalf("claim action/policy/event identity mismatch: action=%+v policy=%+v event=%+v", action, decision, event)
	}
}
