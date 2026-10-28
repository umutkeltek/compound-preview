package work

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
)

func orphanProofRetirementFixture(t *testing.T) (*Store, OrphanProofRetirementInput, string, []byte, []byte) {
	t.Helper()
	s, predecessor, acquired, _, pending, journalPath := proofReportlessCompletionPendingFixture(t, "orphan failed proof")
	attempt, err := s.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !runattempt.IsTerminal(attempt.Status) {
		if _, err := s.runAttempts().Complete(attempt.ID, runattempt.StatusFailed, fixedNow.Add(4*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	predecessor.State = "completed"
	if err := s.writeJSON(s.workPath(predecessor.ID), predecessor); err != nil {
		t.Fatal(err)
	}
	if _, err := landlease.Release(s.home, acquired.Reservation.Holder, fixedNow.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	recovery := claimedProofWork(t, s, "retire exact orphan journal", proofActorAlpha, fixedNow.Add(6*time.Second))
	recovery.State = "running"
	if err := s.writeJSON(s.workPath(recovery.ID), recovery); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanOrphanProofRetirement(recovery.ID, acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	predecessorBefore, err := os.ReadFile(s.workPath(predecessor.ID))
	if err != nil {
		t.Fatal(err)
	}
	input := OrphanProofRetirementInput{
		Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://writer-pause",
		WritersPaused: true, ExpiresAt: fixedNow.Add(8 * time.Minute), Plan: plan,
	}
	return s, input, journalPath, journalBefore, predecessorBefore
}

func TestOrphanProofRetirementRequiresExactApprovalAndPreservesFailedProof(t *testing.T) {
	s, input, journalPath, journalBefore, predecessorBefore := orphanProofRetirementFixture(t)
	now := fixedNow.Add(7 * time.Minute)
	_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
	approveResourceAction(t, s, held, now)
	if got, err := os.ReadFile(journalPath); err != nil || string(got) != string(journalBefore) {
		t.Fatalf("unapproved journal changed: %v", err)
	}
	decision, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !decision.FailedResultUncertified {
		t.Fatalf("decision certified failed proof: %+v", decision)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active journal retained: %v", err)
	}
	archive, err := os.ReadFile(input.Plan.ArchivePath)
	if err != nil || string(archive) != string(journalBefore) {
		t.Fatalf("archive differs from exact journal: %v", err)
	}
	predecessorAfter, err := os.ReadFile(s.workPath(input.Plan.PredecessorWorkID))
	if err != nil || string(predecessorAfter) != string(predecessorBefore) {
		t.Fatalf("predecessor Work changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.home, "work", "proof-resource-retirement-pending", input.Plan.ReservationID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending marker remains: %v", err)
	}
}

func TestOrphanProofRetirementReadsArchivedPredecessor(t *testing.T) {
	s, input, _, _, predecessorBefore := orphanProofRetirementFixture(t)
	originalPlan := input.Plan
	originalPath := "work/work/" + input.Plan.PredecessorWorkID + ".json"
	// Model the operational archive index without invoking the unrelated home
	// doctor, which rejects this intentionally synthetic failed-proof fixture.
	archiveRel := "archives/operational/files/" + originalPath
	archivePath := filepath.Join(s.home, filepath.FromSlash(archiveRel))
	if err := os.MkdirAll(filepath.Dir(archivePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(s.workPath(input.Plan.PredecessorWorkID), archivePath); err != nil {
		t.Fatal(err)
	}
	entry := home.OperationalArchiveEntry{
		SchemaVersion: home.OperationalArchiveSchemaVersion,
		ID:            "archive_fixture", Operation: "archived", Kind: "work",
		OriginalPath: originalPath, ArchivePath: archiveRel,
		CreatedAt: fixedNow.Add(7 * time.Minute).UTC().Format(time.RFC3339Nano),
	}
	indexPath := home.OperationalArchiveIndexPath(s.home)
	if err := os.MkdirAll(filepath.Dir(indexPath), 0700); err != nil {
		t.Fatal(err)
	}
	index, err := os.OpenFile(indexPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(index).Encode(entry); err != nil {
		index.Close()
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.workPath(input.Plan.PredecessorWorkID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("predecessor was not archived: %v", err)
	}
	archivedPath, ok := s.archivedWorkPath(input.Plan.PredecessorWorkID)
	if !ok {
		t.Fatal("archived predecessor is not indexed")
	}
	archivedBefore, err := os.ReadFile(archivedPath)
	if err != nil || string(archivedBefore) != string(predecessorBefore) {
		t.Fatalf("archived predecessor differs: %v", err)
	}
	plan, err := s.PlanOrphanProofRetirement(input.Plan.RecoveryWorkID, input.Plan.ReservationID)
	if err != nil || plan.PredecessorWorkSHA256 != originalPlan.PredecessorWorkSHA256 {
		t.Fatalf("archived predecessor plan: %+v %v", plan, err)
	}
	input.Plan = plan
	now := fixedNow.Add(7 * time.Minute)
	_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
	approveResourceAction(t, s, held, now)
	if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	archivedAfter, err := os.ReadFile(archivedPath)
	if err != nil || string(archivedAfter) != string(predecessorBefore) {
		t.Fatalf("retirement changed archived predecessor: %v", err)
	}
}

func TestOrphanProofRetirementRefusesDriftBeforeApproval(t *testing.T) {
	for _, drift := range []string{"journal", "work", "attempt", "pause"} {
		t.Run(drift, func(t *testing.T) {
			s, input, journalPath, original, _ := orphanProofRetirementFixture(t)
			now := fixedNow.Add(7 * time.Minute)
			_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
			approveResourceAction(t, s, held, now)
			switch drift {
			case "journal":
				if err := os.WriteFile(journalPath, append(original, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			case "work":
				work, err := s.Get(input.Plan.PredecessorWorkID)
				if err != nil {
					t.Fatal(err)
				}
				work.SuccessPredicate += " changed"
				if err := s.writeJSON(s.workPath(work.ID), work); err != nil {
					t.Fatal(err)
				}
			case "attempt":
				path := filepath.Join(s.home, "run_attempts", input.Plan.RunAttemptID+".json")
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(body, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			case "pause":
				input.EvidenceRef = "test://different-pause"
			}
			if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second)); err == nil {
				t.Fatal("drift accepted")
			}
			if _, err := os.Stat(journalPath); err != nil {
				t.Fatalf("refusal retired journal: %v", err)
			}
		})
	}
}

func TestOrphanProofRetirementReplaysCrashBoundaries(t *testing.T) {
	for _, stage := range []string{"reserved", "archived", "retired", "recorded"} {
		t.Run(stage, func(t *testing.T) {
			s, input, journalPath, original, _ := orphanProofRetirementFixture(t)
			now := fixedNow.Add(7 * time.Minute)
			_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
			approveResourceAction(t, s, held, now)
			orphanProofRetirementFault = func(at string) error {
				if at == stage {
					return errors.New("synthetic crash")
				}
				return nil
			}
			t.Cleanup(func() { orphanProofRetirementFault = nil })
			if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second)); err == nil || !strings.Contains(err.Error(), "retry pending") {
				t.Fatalf("crash did not retain transaction: %v", err)
			}
			if err := s.validateResourceMigrationCensus(); err == nil {
				t.Fatalf("migration did not see pending transaction: %v", err)
			}
			pendingPath, err := s.orphanProofRetirementPendingPath(input.Plan.ReservationID)
			if err != nil {
				t.Fatal(err)
			}
			if stage != "reserved" {
				if _, err := os.Stat(pendingPath); err != nil {
					t.Fatalf("retirement pending marker missing: %v", err)
				}
			}
			orphanProofRetirementFault = nil
			reopened, err := Open(s.home)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := reopened.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(5*time.Second))
			if err != nil || !decision.FailedResultUncertified {
				t.Fatalf("replay failed: %+v %v", decision, err)
			}
			if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("replay retained journal: %v", err)
			}
			archived, err := os.ReadFile(input.Plan.ArchivePath)
			if err != nil || string(archived) != string(original) {
				t.Fatalf("archive changed: %v", err)
			}
		})
	}
}

func TestCompletedExactApprovedActionCannotReserveAgain(t *testing.T) {
	s, input, _, _, _ := orphanProofRetirementFixture(t)
	now := fixedNow.Add(7 * time.Minute)
	_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
	approveResourceAction(t, s, held, now)
	orphanProofRetirementFault = func(stage string) error {
		if stage == "archived" {
			return errors.New("synthetic crash")
		}
		return nil
	}
	t.Cleanup(func() { orphanProofRetirementFault = nil })
	if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second)); err == nil {
		t.Fatal("expected interrupted retirement")
	}
	pendingPath, err := s.orphanProofRetirementPendingPath(input.Plan.ReservationID)
	if err != nil {
		t.Fatal(err)
	}
	var pending orphanProofRetirementPending
	if err := s.readJSON(pendingPath, &pending); err != nil {
		t.Fatal(err)
	}
	orphanProofRetirementFault = nil
	if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	called := false
	err = s.ReserveReceiptedAction(input.Plan.RecoveryWorkID, pending.ReceiptInput, pending.AuthorizedAt, func() error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "completed receipted action") {
		t.Fatalf("completed exact-approved action was reserved again: %v", err)
	}
	if called {
		t.Fatal("authorization callback was re-armed")
	}
	if _, present, _, err := s.preflightReceiptedActionReservation(input.Plan.RecoveryWorkID); err != nil || present {
		t.Fatalf("new reservation after completed action: present=%t err=%v", present, err)
	}
}

func TestOrphanProofRetirementPauseExpiryBeforeAndAfterRemoval(t *testing.T) {
	for _, stage := range []string{"reserved", "retired"} {
		t.Run(stage, func(t *testing.T) {
			s, input, journalPath, _, _ := orphanProofRetirementFixture(t)
			now := fixedNow.Add(7 * time.Minute)
			_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
			approveResourceAction(t, s, held, now)
			orphanProofRetirementFault = func(at string) error {
				if at == stage {
					return errors.New("synthetic crash")
				}
				return nil
			}
			t.Cleanup(func() { orphanProofRetirementFault = nil })
			if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second)); err == nil {
				t.Fatal("fault did not interrupt recovery")
			}
			orphanProofRetirementFault = nil
			reopened, err := Open(s.home)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := reopened.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, input.ExpiresAt.Add(time.Second))
			if stage == "reserved" {
				if err == nil || !strings.Contains(err.Error(), "writer pause") {
					t.Fatalf("expired pre-effect retry = %+v %v", decision, err)
				}
				if _, err := os.Stat(journalPath); err != nil {
					t.Fatalf("expired pause retired journal: %v", err)
				}
				pendingPath, err := reopened.orphanProofRetirementPendingPath(input.Plan.ReservationID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("no-effect failed receipt left pending blocker: %v", err)
				}
			} else if err != nil || !decision.FailedResultUncertified {
				t.Fatalf("post-removal forward reconciliation failed: %+v %v", decision, err)
			}
		})
	}
}

func TestOrphanProofRetirementRefusesLiveOriginalHolderAndTamperedPending(t *testing.T) {
	t.Run("original holder", func(t *testing.T) {
		s, input, journalPath, _, _ := orphanProofRetirementFixture(t)
		if _, err := landlease.Acquire(s.home, input.Plan.OriginalHolder, time.Minute, fixedNow.Add(7*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, fixedNow.Add(7*time.Minute)); err == nil || !strings.Contains(err.Error(), "normal completion recovery") {
			t.Fatalf("active original holder accepted: %v", err)
		}
		if _, err := os.Stat(journalPath); err != nil {
			t.Fatalf("active holder refusal removed journal: %v", err)
		}
	})
	t.Run("pending authority", func(t *testing.T) {
		s, input, journalPath, _, _ := orphanProofRetirementFixture(t)
		now := fixedNow.Add(7 * time.Minute)
		_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
		approveResourceAction(t, s, held, now)
		orphanProofRetirementFault = func(at string) error {
			if at == "archived" {
				return errors.New("synthetic crash")
			}
			return nil
		}
		t.Cleanup(func() { orphanProofRetirementFault = nil })
		if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second)); err == nil {
			t.Fatal("archive fault missing")
		}
		orphanProofRetirementFault = nil
		pending, present, path, err := s.readOrphanProofRetirementPending(input.Plan.ReservationID)
		if err != nil || !present {
			t.Fatalf("pending missing: %v", err)
		}
		pending.Decision.Outcome = "hold"
		if err := s.writeJSON(path, pending); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(4*time.Second)); err == nil {
			t.Fatal("tampered pending decision accepted")
		}
		if _, err := os.Stat(journalPath); err != nil {
			t.Fatalf("tampered authority removed journal: %v", err)
		}
	})
	t.Run("archive after recorded receipt", func(t *testing.T) {
		s, input, _, _, _ := orphanProofRetirementFixture(t)
		now := fixedNow.Add(7 * time.Minute)
		_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
		approveResourceAction(t, s, held, now)
		orphanProofRetirementFault = func(at string) error {
			if at == "recorded" {
				return errors.New("synthetic crash")
			}
			return nil
		}
		t.Cleanup(func() { orphanProofRetirementFault = nil })
		if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second)); err == nil {
			t.Fatal("receipt fault missing")
		}
		orphanProofRetirementFault = nil
		if err := os.WriteFile(input.Plan.ArchivePath, []byte("tampered"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(4*time.Second)); err == nil || !strings.Contains(err.Error(), "archive differs") {
			t.Fatalf("post-receipt archive drift accepted: %v", err)
		}
		pendingPath, err := s.orphanProofRetirementPendingPath(input.Plan.ReservationID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(pendingPath); err != nil {
			t.Fatalf("tampered custody cleared pending fence: %v", err)
		}
	})
}

func TestOrphanProofRetirementRequiresExactApprovalDespiteStandingGrant(t *testing.T) {
	s, input, journal, journalBefore, _ := orphanProofRetirementFixture(t)
	installBroadResourceGrant(t, s)
	now := fixedNow.Add(7 * time.Minute)
	_, held := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now)
	after, err := os.ReadFile(journal)
	if err != nil || string(after) != string(journalBefore) {
		t.Fatalf("journal changed before exact approval: %v", err)
	}
	approveResourceAction(t, s, held, now)
	decision, err := s.RetireOrphanProofCompletion(input.Plan.RecoveryWorkID, input, now.Add(3*time.Second))
	if err != nil || !decision.FailedResultUncertified {
		t.Fatalf("approved retirement: %+v %v", decision, err)
	}
}
