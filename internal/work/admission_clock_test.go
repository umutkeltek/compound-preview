package work

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

func admissionClockFixture(t *testing.T, initial string) (*Store, envelope.WorkEnvelope, time.Time) {
	t.Helper()
	s := newStore(t)
	origin := fixedNow
	s.advanceClock = func(logical time.Time) time.Time { return logical }
	w := claimedProofWork(t, s, "synthetic clock boundary", proofActorAlpha, origin)
	if initial == "verifying" {
		var err error
		w, err = s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running"}, origin)
		if err != nil {
			t.Fatal(err)
		}
		w, err = s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "verifying"}, origin)
		if err != nil {
			t.Fatal(err)
		}
	}
	return s, w, origin
}

func writeAdmissionClockPolicy(t *testing.T, s *Store, expiry time.Time, baseline string, grant bool) {
	t.Helper()
	body := fmt.Sprintf("class_outcome:\n  local_reversible:\n    outcome: %s\n", baseline)
	if grant {
		body += fmt.Sprintf("standing_grants:\n  - id: grant_synthetic_admission_clock\n    capability: work.advance\n    mutation_class: local_reversible\n    reversibility: easy\n    blast_radius: local\n    expires_at: %q\n", expiry.Format(time.RFC3339Nano))
	}
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceSamplesClockAfterBlockedAdmission(t *testing.T) {
	for _, initial := range []string{"claimed", "verifying"} {
		for _, delta := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second} {
			t.Run(fmt.Sprintf("%s/%s", initial, delta), func(t *testing.T) {
				s, w, origin := admissionClockFixture(t, initial)
				writeAdmissionClockPolicy(t, s, origin.Add(time.Second), "hold", true)
				var nanos atomic.Int64
				nanos.Store(origin.UnixNano())
				started := make(chan struct{})
				s.advanceClock = func(time.Time) time.Time {
					return time.Unix(0, nanos.Load()).UTC()
				}
				held := make(chan struct{})
				release := make(chan struct{})
				lockDone := make(chan error, 1)
				var releaseOnce sync.Once
				unlock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unlock()
				go func() {
					lockDone <- home.WithAutonomyAdmissionLock(s.home, func() error { close(held); <-release; return nil })
				}()
				<-held
				type result struct {
					w   envelope.WorkEnvelope
					err error
				}
				done := make(chan result, 1)
				go func() {
					close(started)
					out, err := s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running", ExpectedWorkState: initial, ExpectedPolicyDecisionID: w.PolicyDecisionID, ExpectedActionIntentID: latestActionIntentID(w)}, origin)
					done <- result{out, err}
				}()
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("Advance did not observe injected clock")
				}
				nanos.Store(origin.Add(delta).UnixNano())
				unlock()
				if err := <-lockDone; err != nil {
					t.Fatal(err)
				}
				got := <-done
				if delta < time.Second {
					if got.err != nil || got.w.State != "running" {
						t.Fatalf("still-valid grant refused: %v", got.err)
					}
				} else {
					if got.err == nil {
						t.Fatal("expired/equal grant crossed admission")
					}
					current, err := s.Get(w.ID)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(current, w) {
						t.Fatal("refused admission changed authoritative work")
					}
				}
			})
		}
	}
}

func TestExecutionStartAuthorityUsesFreshClockAndCurrentPolicy(t *testing.T) {
	for _, mode := range []string{"valid", "equal", "expired", "revoked", "generation"} {
		t.Run(mode, func(t *testing.T) {
			s, w, origin := admissionClockFixture(t, "claimed")
			expiry := origin.Add(time.Second)
			writeAdmissionClockPolicy(t, s, expiry, "hold", true)
			running, err := s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running"}, origin)
			if err != nil {
				t.Fatal(err)
			}
			current := origin.Add(500 * time.Millisecond)
			switch mode {
			case "equal":
				current = expiry
			case "expired":
				current = expiry.Add(time.Nanosecond)
			case "revoked":
				writeAdmissionClockPolicy(t, s, expiry, "hold", false)
			case "generation":
				if _, err := s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running", Note: "new generation"}, current); err != nil {
					t.Fatal(err)
				}
			}
			err = s.ValidateExecutionStartAuthority(w.ID, running.PolicyDecisionID, latestActionIntentID(running), func() time.Time { return current })
			if mode == "valid" && err != nil {
				t.Fatal(err)
			}
			if mode != "valid" && err == nil {
				t.Fatal("stale launch authority accepted", mode)
			}
		})
	}
}

func TestExpiryAfterAcceptedStartDoesNotInvalidateFinalization(t *testing.T) {
	s, w, origin := admissionClockFixture(t, "claimed")
	expiry := origin.Add(time.Second)
	writeAdmissionClockPolicy(t, s, expiry, "allow_with_obligations", true)
	running, err := s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "running"}, origin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateExecutionStartAuthority(w.ID, running.PolicyDecisionID, latestActionIntentID(running), func() time.Time { return origin.Add(500 * time.Millisecond) }); err != nil {
		t.Fatal(err)
	}
	finalized, err := s.Advance(w.ID, AdvanceInput{Actor: proofActorAlpha, To: "verifying", ExpectedWorkState: "running", ExpectedPolicyDecisionID: running.PolicyDecisionID, ExpectedActionIntentID: latestActionIntentID(running)}, expiry.Add(time.Second))
	if err != nil || finalized.State != "verifying" {
		t.Fatalf("expiry undid an admitted execution: %v", err)
	}
}
