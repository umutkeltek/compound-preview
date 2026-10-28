package work

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

func reviewClockFixture(t *testing.T, initial string) (*Store, envelope.WorkEnvelope) {
	t.Helper()
	s := newStore(t)
	w := claimedProofWork(t, s, "synthetic admission clock", proofActorAlpha, time.Now().UTC())
	if initial == "verifying" {
		var err error
		w, err = s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running"}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		w, err = s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "verifying"}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
	}
	return s, w
}

func reviewClockGrant(t *testing.T, s *Store, expiry time.Time) {
	t.Helper()
	body := fmt.Sprintf("class_outcome:\n  local_reversible:\n    outcome: hold\nstanding_grants:\n  - id: grant_synthetic_clock\n    capability: work.advance\n    mutation_class: local_reversible\n    reversibility: easy\n    blast_radius: local\n    expires_at: %q\n", expiry.Format(time.RFC3339Nano))
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceGrantExpiryEquality(t *testing.T) {
	for _, initial := range []string{"claimed", "verifying"} {
		t.Run(initial, func(t *testing.T) {
			s, w := reviewClockFixture(t, initial)
			expiry := time.Now().UTC().Add(time.Minute)
			reviewClockGrant(t, s, expiry)
			opts := AdvancePlanOptions{Actor: proofActorAlpha, To: "running"}
			before, err := s.AdvancePlan(w.ID, expiry.Add(-time.Nanosecond), opts)
			if err != nil || !before.Policy.Allowed || before.Decision.StandingGrantID != "grant_synthetic_clock" {
				t.Fatalf("before-expiry control: %+v %v", before.Policy, err)
			}
			after, err := s.AdvancePlan(w.ID, expiry.Add(time.Nanosecond), opts)
			if err != nil || after.Policy.Allowed {
				t.Fatalf("after-expiry control: %+v %v", after.Policy, err)
			}
			equal, err := s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running"}, expiry)
			if err == nil {
				t.Fatalf("equality admitted %s->%s with grant expiry=%s at the exact expiry instant, policy=%s", initial, equal.State, expiry.Format(time.RFC3339Nano), equal.PolicyDecisionID)
			}
		})
	}
}

func TestAdvanceBlockedPastGrantExpiry(t *testing.T) {
	for _, initial := range []string{"claimed", "verifying"} {
		t.Run(initial, func(t *testing.T) {
			s, w := reviewClockFixture(t, initial)
			s.advanceClock = nil
			callerTime := time.Now().UTC()
			expiry := callerTime.Add(3 * time.Second)
			reviewClockGrant(t, s, expiry)
			opts := AdvancePlanOptions{Actor: proofActorAlpha, To: "running"}
			before, err := s.AdvancePlan(w.ID, callerTime, opts)
			if err != nil || !before.Policy.Allowed || before.Decision.StandingGrantID != "grant_synthetic_clock" {
				t.Fatalf("initial valid grant control: %+v %v", before.Policy, err)
			}
			after, err := s.AdvancePlan(w.ID, expiry.Add(time.Nanosecond), opts)
			if err != nil || after.Policy.Allowed {
				t.Fatalf("fresh-time expired control: %+v %v", after.Policy, err)
			}
			locked := make(chan struct{})
			release := make(chan struct{})
			lockDone := make(chan error, 1)
			go func() {
				lockDone <- home.WithAutonomyAdmissionLock(s.home, func() error { close(locked); <-release; return nil })
			}()
			<-locked
			type result struct {
				w        envelope.WorkEnvelope
				err      error
				returned time.Time
			}
			done := make(chan result, 1)
			entered := make(chan struct{})
			go func() {
				close(entered)
				advanced, err := s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running"}, callerTime)
				done <- result{advanced, err, time.Now().UTC()}
			}()
			<-entered
			select {
			case early := <-done:
				close(release)
				<-lockDone
				t.Fatalf("control not blocked by held global admission: %v", early.err)
			case <-time.After(20 * time.Millisecond):
			}
			if delay := time.Until(expiry.Add(25 * time.Millisecond)); delay > 0 {
				time.Sleep(delay)
			}
			unlockedAt := time.Now().UTC()
			close(release)
			if err := <-lockDone; err != nil {
				t.Fatal(err)
			}
			got := <-done
			if !unlockedAt.After(expiry) {
				t.Fatal("test failed to release after expiry")
			}
			if got.err == nil {
				var pd envelope.PolicyDecision
				if err := s.readEventJSON(got.w.PolicyDecisionID, &pd); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("stale admission: %s->%s accepted grant=%s expires=%s caller=%s unlocked=%s returned=%s decision=%s", initial, got.w.State, pd.StandingGrantID, pd.ExpiresAt, callerTime.Format(time.RFC3339Nano), unlockedAt.Format(time.RFC3339Nano), got.returned.Format(time.RFC3339Nano), pd.ID)
			}
		})
	}
}

func TestAdvanceRejectsCallerTimeAlreadyStaleBeforeEntry(t *testing.T) {
	for _, initial := range []string{"claimed", "verifying"} {
		t.Run(initial, func(t *testing.T) {
			s, w := reviewClockFixture(t, initial)
			s.advanceClock = nil
			actual := time.Now()
			stale := actual.Add(-2 * time.Second)
			expiry := actual.Add(-time.Second)
			reviewClockGrant(t, s, expiry)
			before, err := s.AdvancePlan(w.ID, stale, AdvancePlanOptions{Actor: proofActorAlpha, To: "running"})
			if err != nil || !before.Policy.Allowed {
				t.Fatalf("logical-time control: %+v %v", before.Policy, err)
			}
			if _, err = s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running"}, stale); err == nil {
				t.Fatal("stale caller time authorized expired grant")
			}
			current, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != w.State || current.PolicyDecisionID != w.PolicyDecisionID || latestActionIntentID(current) != latestActionIntentID(w) {
				t.Fatal("refusal changed work generation")
			}
		})
	}
}
