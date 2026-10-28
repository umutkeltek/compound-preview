package work

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func crashCloseAfterFirstEnvelope(t *testing.T, s *Store, workID string, in CloseInput, at time.Time) {
	t.Helper()
	token := &struct{}{}
	previous := closeAfterFirstEnvelopeWrite
	closeAfterFirstEnvelopeWrite = func() { panic(token) }
	t.Cleanup(func() { closeAfterFirstEnvelopeWrite = previous })
	crashed := false
	func() {
		defer func() {
			switch recovered := recover(); recovered {
			case token:
				crashed = true
			case nil:
			default:
				panic(recovered)
			}
		}()
		_, _ = s.Close(workID, in, at)
	}()
	closeAfterFirstEnvelopeWrite = previous
	if !crashed {
		t.Fatal("close did not reach the first-envelope crash seam")
	}
}

func TestClosePendingFencesClaimBeforeClaimPublication(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "close-pending-claim-fence",
		SuccessPredicate: "a frozen close fences a later claim generation",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	in := CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "closer",
		Summary:  "freeze close before claim",
	}
	crashCloseAfterFirstEnvelope(t, s, w.ID, in, fixedNow.Add(time.Minute))
	beforeWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := s.events.ReadWorkLog()
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.Claim(w.ID, "claimant", fixedNow.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "close recovery_required") {
		t.Fatalf("claim while exact close is frozen error = %v, want close recovery_required fence", err)
	}
	afterWork, readErr := os.ReadFile(s.workPath(w.ID))
	if readErr != nil {
		t.Fatal(readErr)
	}
	afterEvents, readErr := s.events.ReadWorkLog()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(afterWork) != string(beforeWork) || string(afterEvents) != string(beforeEvents) {
		t.Fatal("claim overtook a frozen close and changed source Work/event authority")
	}
}

func TestCloseRejectsWholeLeaseDriftImmediatelyBeforeTerminalPublication(t *testing.T) {
	for _, tt := range []struct {
		name   string
		leased bool
		steal  bool
	}{
		{name: "ordinary", leased: true},
		{name: "stolen", leased: true, steal: true},
		{name: "absent_becomes_present"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			w, err := s.Create(CreateInput{
				Owner:            "close-pending-lease-drift",
				SuccessPredicate: "the exact whole lease is stable through terminal publication",
			}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			actor := "closer"
			if tt.leased {
				actor = "holder-a"
				w, err = s.Claim(w.ID, actor, fixedNow.Add(time.Minute))
				if err != nil {
					t.Fatal(err)
				}
			}
			in := CloseInput{
				Outcome:  "cancelled",
				MoveType: "audit_only",
				Actor:    actor,
				Summary:  "reject late whole-lease drift",
			}
			if tt.steal {
				in.Actor = "holder-b"
				in.Steal = true
				in.StealReason = "holder-a is unavailable"
			}
			beforeWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			beforeEvents, err := s.events.ReadWorkLog()
			if err != nil {
				t.Fatal(err)
			}

			previous := closeAfterFirstEnvelopeWrite
			t.Cleanup(func() { closeAfterFirstEnvelopeWrite = previous })
			closeAfterFirstEnvelopeWrite = func() {
				if tt.leased {
					lease, readErr := s.readLease(w.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					lease.LaneID = "agent-lane://late-whole-file-drift"
					if writeErr := s.publishClaimJSON(s.leasePath(w.ID), &lease); writeErr != nil {
						t.Fatal(writeErr)
					}
					return
				}
				workSHA, hashErr := workEnvelopeSHA256(w)
				if hashErr != nil {
					t.Fatal(hashErr)
				}
				late := Lease{WorkID: w.ID, By: "late-intruder", ClaimAt: ts(fixedNow.Add(2 * time.Minute)), WorkSHA256: workSHA}
				if writeErr := s.publishClaimJSON(s.leasePath(w.ID), &late); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			_, closeErr := s.Close(w.ID, in, fixedNow.Add(3*time.Minute))
			closeAfterFirstEnvelopeWrite = previous
			if closeErr == nil || !strings.Contains(closeErr.Error(), "close recovery_required") {
				t.Fatalf("late lease drift close error = %v, want close recovery_required", closeErr)
			}
			afterWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			afterEvents, err := s.events.ReadWorkLog()
			if err != nil {
				t.Fatal(err)
			}
			if string(afterWork) != string(beforeWork) || string(afterEvents) != string(beforeEvents) {
				t.Fatal("late lease drift published terminal Work/event authority")
			}
		})
	}
}

func TestCloseStolenRetryConvergesAfterLeaseReleasedBeforeMarkerCleanup(t *testing.T) {
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "close-stolen-marker-cleanup", fixedNow)
	in := CloseInput{
		Outcome:     "completed",
		MoveType:    "audit_only",
		Actor:       "actor-b",
		Steal:       true,
		StealReason: "actor-a is unavailable",
		Summary:     "retry cleanup without rerunning live steal gate",
	}
	token := &struct{}{}
	previous := closeBeforeLeaseRelease
	closeBeforeLeaseRelease = func() {
		s.releaseLease(workID)
		panic(token)
	}
	t.Cleanup(func() { closeBeforeLeaseRelease = previous })
	crashed := false
	func() {
		defer func() { crashed = recover() == token }()
		_, _ = s.Close(workID, in, fixedNow.Add(5*time.Minute))
	}()
	closeBeforeLeaseRelease = previous
	if !crashed {
		t.Fatal("close did not crash after test released the exact lease")
	}
	if _, err := os.Stat(s.leasePath(workID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test did not release lease: %v", err)
	}
	if _, err := os.Stat(s.closePendingPath(workID)); err != nil {
		t.Fatalf("close marker missing after cleanup crash: %v", err)
	}

	closed, err := s.Close(workID, in, fixedNow.Add(9*time.Minute))
	if err != nil {
		t.Fatalf("exact stolen retry after lease release: %v", err)
	}
	if closed.Work.State != "completed" {
		t.Fatalf("retry state = %q, want completed", closed.Work.State)
	}
	if _, err := os.Stat(s.closePendingPath(workID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful cleanup retry left close marker: %v", err)
	}
}
