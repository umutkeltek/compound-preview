package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/landlease"
)

func proofMigrationSuccessor(t *testing.T) (*Store, envelope.WorkEnvelope, envelope.WorkEnvelope, ResourceMigrationInput, []byte, []byte, time.Time) {
	t.Helper()
	s, old, in, oldBody, prep := completedProofMigrationFixture(t)
	now := fixedNow.Add(10 * time.Minute)
	next := claimedProofWork(t, s, "independent native v3 successor", proofActorAlpha, now)
	in.ProofTargetRevision = landlease.ProofDomainFencedV3
	in.EvidenceRef = "test://new-pause"
	in.ExpiresAt = now.Add(3 * time.Minute)
	return s, old, next, in, oldBody, prep, now
}

func TestProofMigrationHistoryRefusesUnauthenticatedOrStalePredecessor(t *testing.T) {
	for _, mode := range []string{"action", "decision", "incomplete-work", "lost-action", "lost-event", "duplicate-event", "newer-domain", "changed-generation", "changed-enrollment", "missing-fence"} {
		t.Run(mode, func(t *testing.T) {
			s, old, next, in, _, _, now := proofMigrationSuccessor(t)
			var pending resourceMigrationPending
			data, err := os.ReadFile(s.proofMigrationPendingPath())
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &pending); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "action", "decision":
				if mode == "action" {
					pending.Action.Actor = "fixture:other"
				} else {
					pending.Decision.ActionIntentID = "act_other"
				}
				if err := s.writeJSON(s.proofMigrationPendingPath(), pending); err != nil {
					t.Fatal(err)
				}
			case "incomplete-work", "lost-action":
				w, err := s.Get(old.ID)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "incomplete-work" {
					w.State = "verifying"
				} else {
					for i, id := range w.ActionIntentIDs {
						if id == pending.Action.ID {
							w.ActionIntentIDs = append(w.ActionIntentIDs[:i], w.ActionIntentIDs[i+1:]...)
							break
						}
					}
				}
				if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
					t.Fatal(err)
				}
			case "lost-event", "duplicate-event":
				raw, err := s.events.ReadWorkLog()
				if err != nil {
					t.Fatal(err)
				}
				var body []byte
				for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
					var event Event
					if err := json.Unmarshal(line, &event); err != nil {
						t.Fatal(err)
					}
					match := event.WorkID == old.ID && event.Note == "coordination cutover published; exact marker reconciled"
					if match && mode == "lost-event" {
						continue
					}
					body = append(append(body, line...), '\n')
					if match && mode == "duplicate-event" {
						body = append(append(body, line...), '\n')
					}
				}
				if err := s.events.ReplaceWorkLog(body, nil, eventstore.WorkLogReplaceOptions{}); err != nil {
					t.Fatal(err)
				}
			default:
				root := filepath.Join(in.BackingHome, "work", "resource-leases", "landlease", "proof")
				if mode == "missing-fence" {
					if err := os.Remove(filepath.Join(root, "canonical-full", "state", "land-lease.json")); err != nil {
						t.Fatal(err)
					}
					break
				}
				path := filepath.Join(root, "domain.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var domain map[string]any
				if err := json.Unmarshal(raw, &domain); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "newer-domain":
					domain["schema_version"] = "compound.proof_domain.v0.3"
				case "changed-generation":
					domain["generation"] = strings.Repeat("f", 64)
				case "changed-enrollment":
					domain["homes"] = []string{"/synthetic/other"}
				}
				if err := s.writeJSON(path, domain); err != nil {
					t.Fatal(err)
				}
			}
			beforePrep, _ := os.ReadFile(s.proofMigrationPendingPath())
			beforeOld, _ := os.ReadFile(s.workPath(old.ID))
			beforeNext, _ := os.ReadFile(s.workPath(next.ID))
			_, err = s.MigrateCoordinationResources(next.ID, in, now)
			if err == nil || strings.Contains(err.Error(), "policy hold") {
				t.Fatalf("unauthenticated history reached new approval: %v", err)
			}
			for path, before := range map[string][]byte{s.proofMigrationPendingPath(): beforePrep, s.workPath(old.ID): beforeOld, s.workPath(next.ID): beforeNext} {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("refusal changed %s: %v", path, err)
				}
			}
		})
	}
}

func TestProofMigrationHistoryFreshApprovalDoesNotCoverDrift(t *testing.T) {
	for _, mode := range []string{"evidence", "pause-expiry", "successor-generation", "expired-pause"} {
		t.Run(mode, func(t *testing.T) {
			s, old, next, in, oldBody, prep, now := proofMigrationSuccessor(t)
			_, held := s.MigrateCoordinationResources(next.ID, in, now)
			approveResourceAction(t, s, held, now)
			at := now.Add(3 * time.Second)
			switch mode {
			case "evidence":
				in.EvidenceRef = "test://changed-pause"
			case "pause-expiry":
				in.ExpiresAt = in.ExpiresAt.Add(time.Second)
			case "successor-generation":
				w, err := s.Get(next.ID)
				if err != nil {
					t.Fatal(err)
				}
				w.DesiredOutcome = "newer authority"
				if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
					t.Fatal(err)
				}
			case "expired-pause":
				at = now.Add(4 * time.Minute)
			}
			_, err := s.MigrateCoordinationResources(next.ID, in, at)
			if err == nil {
				t.Fatal("old approval covered drift")
			}
			for path, before := range map[string][]byte{s.proofMigrationPendingPath(): prep, s.workPath(old.ID): oldBody} {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("drift changed history: %v", err)
				}
			}
		})
	}
}

func TestProofMigrationHistoryInterruptionsKeepExactCustody(t *testing.T) {
	for _, stage := range []string{"proof-history-custodied", "proof-prepared", "before-result", "before-work"} {
		t.Run(stage, func(t *testing.T) {
			s, old, next, in, oldBody, prep, now := proofMigrationSuccessor(t)
			_, held := s.MigrateCoordinationResources(next.ID, in, now)
			approveResourceAction(t, s, held, now)
			s.resourceMigrationFault = func(at string) error {
				if at == stage {
					return errors.New("fixture interruption")
				}
				return nil
			}
			if _, err := s.MigrateCoordinationResources(next.ID, in, now.Add(3*time.Second)); err == nil {
				t.Fatal("fault not exercised")
			}
			s.resourceMigrationFault = nil
			if stage == "proof-history-custodied" {
				got, _ := os.ReadFile(s.proofMigrationPendingPath())
				if !bytes.Equal(got, prep) {
					t.Fatal("custody fault replaced old preparation")
				}
				_, exhausted := s.MigrateCoordinationResources(next.ID, in, now.Add(4*time.Second))
				if exhausted == nil || !strings.Contains(exhausted.Error(), "already consumed") {
					t.Fatalf("consumed approval replay: %v", exhausted)
				}
				in.EvidenceRef = "test://fresh-pause-after-custody-interruption"
				in.ExpiresAt = in.ExpiresAt.Add(time.Second)
				_, held = s.MigrateCoordinationResources(next.ID, in, now.Add(4*time.Second))
				approveResourceAction(t, s, held, now.Add(4*time.Second))
			}
			if _, err := s.MigrateCoordinationResources(next.ID, in, now.Add(7*time.Second)); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(s.workPath(old.ID))
			if !bytes.Equal(after, oldBody) {
				t.Fatal("recovery changed predecessor Work")
			}
			raw, _ := os.ReadFile(s.proofMigrationPendingPath())
			var pending resourceMigrationPending
			if err := json.Unmarshal(raw, &pending); err != nil {
				t.Fatal(err)
			}
			if pending.PredecessorPreparationRef != proofMigrationHistoryRef(prep) {
				t.Fatal("recovery lost exact predecessor reference")
			}
			body, err := s.readProofMigrationHistory(pending)
			if err != nil || !bytes.Equal(body, prep) {
				t.Fatalf("recovery history: %v", err)
			}
			status, err := landlease.ReadProofDomainStatus(pending.BackingRoot)
			if err != nil || status.SchemaVersion != "compound.proof_domain.v0.3" || status.Phase != "active" {
				t.Fatalf("recovery domain %+v %v", status, err)
			}
		})
	}
}

func TestProofMigrationHistoryReapprovalPreservesPendingAndLineage(t *testing.T) {
	s, old, next, in, oldBody, prep, now := proofMigrationSuccessor(t)
	_, held := s.MigrateCoordinationResources(next.ID, in, now)
	approveResourceAction(t, s, held, now)
	s.resourceMigrationFault = func(stage string) error {
		if stage == "proof-prepared" {
			return errors.New("fixture prepared")
		}
		return nil
	}
	if _, err := s.MigrateCoordinationResources(next.ID, in, now.Add(3*time.Second)); err == nil {
		t.Fatal("missing prepared interruption")
	}
	s.resourceMigrationFault = nil
	pendingBefore, err := os.ReadFile(s.proofMigrationPendingPath())
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(4 * time.Minute)
	in.ExpiresAt = later.Add(time.Minute)
	in.EvidenceRef = "test://renewed-pause"
	_, held = s.MigrateCoordinationResources(next.ID, in, later)
	pendingAfter, err := os.ReadFile(s.proofMigrationPendingPath())
	if err != nil || !bytes.Equal(pendingBefore, pendingAfter) {
		t.Fatalf("reapproval hold consumed pending authority: %v", err)
	}
	approveResourceAction(t, s, held, later)
	if _, err := s.MigrateCoordinationResources(next.ID, in, later.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.proofMigrationPendingPath())
	var pending resourceMigrationPending
	if err := json.Unmarshal(raw, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.PredecessorPreparationRef != proofMigrationHistoryRef(prep) {
		t.Fatal("reapproval lost history binding")
	}
	after, _ := os.ReadFile(s.workPath(old.ID))
	if !bytes.Equal(after, oldBody) {
		t.Fatal("reapproval changed old Work")
	}
}

func TestProofMigrationHistoryPreparedRejectsNewerSuccessorGeneration(t *testing.T) {
	s, _, next, in, _, _, now := proofMigrationSuccessor(t)
	_, held := s.MigrateCoordinationResources(next.ID, in, now)
	approveResourceAction(t, s, held, now)
	s.resourceMigrationFault = func(stage string) error {
		if stage == "proof-prepared" {
			return errors.New("fixture prepared")
		}
		return nil
	}
	if _, err := s.MigrateCoordinationResources(next.ID, in, now.Add(3*time.Second)); err == nil {
		t.Fatal("missing interruption")
	}
	s.resourceMigrationFault = nil
	w, err := s.Get(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.DesiredOutcome = "newer generation"
	if err := s.writeJSON(s.workPath(next.ID), w); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.workPath(next.ID))
	pending, _ := os.ReadFile(s.proofMigrationPendingPath())
	if _, err := s.MigrateCoordinationResources(next.ID, in, now.Add(4*time.Second)); err == nil {
		t.Fatal("prepared recovery overwrote newer generation")
	}
	after, _ := os.ReadFile(s.workPath(next.ID))
	afterPending, _ := os.ReadFile(s.proofMigrationPendingPath())
	if !bytes.Equal(before, after) || !bytes.Equal(pending, afterPending) {
		t.Fatal("generation refusal changed authority")
	}
}

func TestProofMigrationHistoryLiveAndExpiredProofsRemainBlocked(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expired), func(t *testing.T) {
			s, old, next, in, oldBody, _, now := proofMigrationSuccessor(t)
			identity, err := landlease.ProofCoordinationFor(s.home, landlease.ProofCapacitySystemFull, strings.Repeat("b", 64))
			if err != nil {
				t.Fatal(err)
			}
			ticket, err := landlease.BeginProofAdmission(s.home, *identity, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := ticket.Cancel(); err != nil {
					t.Error(err)
				}
			})
			var barrier *landlease.ExecutionBarrier
			at := time.Now()
			if expired {
				at = at.Add(-2 * time.Minute)
			}
			err = ticket.WithTurn(context.Background(), false, func(resource *landlease.Resource) error {
				_, acquired, err := resource.AcquireWithExecutionBarrierPrepared("fixture:proof-holder", time.Minute, at, func(landlease.AcquireOutcome) error { return nil })
				barrier = acquired
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if expired {
				if err := barrier.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Cleanup(func() {
					if err := barrier.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			_, held := s.MigrateCoordinationResources(next.ID, in, now)
			approveResourceAction(t, s, held, now)
			if _, err := s.MigrateCoordinationResources(next.ID, in, now.Add(3*time.Second)); err == nil {
				t.Fatal("upgrade ignored unresolved proof")
			}
			status, err := landlease.ReadProofDomainStatus(filepath.Join(in.BackingHome, "work", "resource-leases"))
			if err != nil || status.SchemaVersion != "compound.proof_domain.v0.2" || status.Phase != "active" {
				t.Fatalf("proof refusal changed domain %+v %v", status, err)
			}
			after, _ := os.ReadFile(s.workPath(old.ID))
			if !bytes.Equal(after, oldBody) {
				t.Fatal("proof refusal changed old Work")
			}
		})
	}
}
