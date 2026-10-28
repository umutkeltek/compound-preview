package actuator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/work"
)

func TestGovernedStartGuardRejectsExpiredGrant(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", resume), func(t *testing.T) {
			homePath := t.TempDir()
			s, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			const actor = "codex:clock-fixture"
			w, err := s.Create(work.CreateInput{Owner: actor, Actor: actor, SuccessPredicate: "synthetic launch clock"}, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			w, err = s.Claim(w.ID, actor, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if resume {
				w, err = s.Advance(w.ID, work.AdvanceInput{Actor: actor, To: "running"}, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				w, err = s.Advance(w.ID, work.AdvanceInput{Actor: actor, To: "verifying"}, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
			}
			admittedAt := time.Now().UTC()
			expiry := admittedAt.Add(time.Second)
			body := fmt.Sprintf("class_outcome:\n  local_reversible:\n    outcome: hold\nstanding_grants:\n  - id: grant_synthetic_launch_clock\n    capability: work.advance\n    mutation_class: local_reversible\n    reversibility: easy\n    blast_radius: local\n    expires_at: %q\n", expiry.Format(time.RFC3339Nano))
			if err := os.MkdirAll(filepath.Join(homePath, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			w, err = s.Advance(w.ID, work.AdvanceInput{Actor: actor, To: "running"}, admittedAt)
			if err != nil {
				t.Fatal(err)
			}
			after, err := s.AdvancePlan(w.ID, expiry.Add(time.Nanosecond), work.AdvancePlanOptions{Actor: actor, To: "running"})
			if err != nil || after.Policy.Allowed {
				t.Fatalf("fresh-time control should refuse: %+v %v", after.Policy, err)
			}
			actionID := w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
			guard := NewGovernedExecutionReservationStartGuard(homePath, w.ID, w.PolicyDecisionID, actionID)
			if resume {
				guard = newGovernedResumeStartGuard(homePath, w.ID, w.PolicyDecisionID, actionID, nil)
			}
			held := make(chan struct{})
			release := make(chan struct{})
			lockDone := make(chan error, 1)
			go func() {
				lockDone <- home.WithAutonomyAdmissionLock(homePath, func() error { close(held); <-release; return nil })
			}()
			<-held
			started := false
			done := make(chan error, 1)
			go func() { done <- guard(context.Background(), func() error { started = true; return nil }) }()
			select {
			case early := <-done:
				close(release)
				<-lockDone
				t.Fatalf("guard was not blocked: %v", early)
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
			err = <-done
			if err == nil || started {
				t.Fatalf("launch after expiry: resume=%t callback_started=%t err=%v expiry=%s unlocked=%s stable_policy=%s", resume, started, err, expiry.Format(time.RFC3339Nano), unlockedAt.Format(time.RFC3339Nano), w.PolicyDecisionID)
			}
		})
	}
}
