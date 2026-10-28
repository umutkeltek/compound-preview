package work

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func freezeClosePendingAfterPrepare(t *testing.T, s *Store, workID string, in CloseInput, at time.Time) closePending {
	t.Helper()
	token := &struct{}{}
	previous := closeAfterTerminalizationPrepare
	closeAfterTerminalizationPrepare = func() { panic(token) }
	t.Cleanup(func() { closeAfterTerminalizationPrepare = previous })
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
	closeAfterTerminalizationPrepare = previous
	if !crashed {
		t.Fatal("close did not reach the post-prepare crash seam")
	}
	pending, present, err := s.loadClosePending(workID)
	if err != nil || !present {
		t.Fatalf("load frozen close frame: present=%v err=%v", present, err)
	}
	return pending
}

func writeTamperedClosePending(t *testing.T, s *Store, pending closePending, reseal bool) {
	t.Helper()
	body, err := canonicalCloseJSON(&pending)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if reseal {
		if _, framed := raw["frame_sha256"]; framed {
			raw["frame_sha256"] = ""
			unsealed, err := canonicalCloseJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			raw["frame_sha256"] = closePendingSHA256(unsealed)
		}
	}
	body, err = canonicalCloseJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.closePendingPath(pending.WorkID), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertClosePendingFrameRejected(t *testing.T, s *Store, workID string) {
	t.Helper()
	beforeWork, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := s.eventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}
	beforeLease := readOptionalCloseAuthorityFile(t, s.leasePath(workID))
	if _, present, err := s.loadClosePending(workID); err == nil || present || !strings.Contains(err.Error(), "close recovery_required") {
		t.Fatalf("tampered close frame load: present=%v err=%v, want recovery_required rejection", present, err)
	}
	afterWork, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := s.eventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}
	afterLease := readOptionalCloseAuthorityFile(t, s.leasePath(workID))
	if !reflect.DeepEqual(afterWork, beforeWork) {
		t.Fatal("tampered close frame load changed the WorkEnvelope")
	}
	if !reflect.DeepEqual(afterEvents, beforeEvents) {
		t.Fatal("tampered close frame load changed lifecycle events")
	}
	if !bytes.Equal(afterLease, beforeLease) {
		t.Fatal("tampered close frame load changed lease bytes")
	}
}

func readOptionalCloseAuthorityFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestClosePendingFrameChecksumRejectsParseableTamper(t *testing.T) {
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "close-frame-checksum", fixedNow)
	pending := freezeClosePendingAfterPrepare(t, s, workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "codex",
		Summary:   "seal the entire close frame",
		ProofRefs: []string{"check:close-frame-checksum"},
	}, fixedNow.Add(5*time.Minute))
	pending.InputSHA256 = "sha256:" + strings.Repeat("a", 64)
	writeTamperedClosePending(t, s, pending, false)
	assertClosePendingFrameRejected(t, s, workID)
}

func TestClosePendingFrameRejectsResealedSemanticTamper(t *testing.T) {
	tests := []struct {
		name   string
		stolen bool
		mutate func(*closePending)
	}{
		{
			name:   "lease_stolen_derivation",
			stolen: true,
			mutate: func(p *closePending) { p.LeaseStolen = false },
		},
		{
			name: "caller_owns_derivation",
			mutate: func(p *closePending) {
				p.RunAttempts[0].CallerOwns = !p.RunAttempts[0].CallerOwns
			},
		},
		{
			name: "completed_at_receipt_binding",
			mutate: func(p *closePending) {
				createdAt, err := time.Parse(time.RFC3339Nano, p.Receipt.CreatedAt)
				if err != nil {
					t.Fatal(err)
				}
				p.CompletedAt = createdAt.Add(time.Second).Format(time.RFC3339Nano)
			},
		},
		{
			name: "runattempt_snapshot_intent_linkage",
			mutate: func(p *closePending) {
				p.RunAttempts[0].Attempt.ActionIntentID = "act_tampered_snapshot"
			},
		},
		{
			name: "runattempt_status_precondition",
			mutate: func(p *closePending) {
				if p.RunAttempts[0].Attempt.Status == "pending" {
					p.RunAttempts[0].Attempt.Status = "running"
				} else {
					p.RunAttempts[0].Attempt.Status = "pending"
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, _ := closeTerminalizeWork(t, s, "close-frame-semantic-"+tt.name, fixedNow)
			in := CloseInput{
				Outcome:   "completed",
				MoveType:  "control_plane_repair",
				Actor:     "codex",
				Summary:   "bind close frame semantics",
				ProofRefs: []string{"check:close-frame-semantic"},
			}
			if tt.stolen {
				in.Actor = "actor-b"
				in.Steal = true
				in.StealReason = "codex is unavailable"
			}
			pending := freezeClosePendingAfterPrepare(t, s, workID, in, fixedNow.Add(5*time.Minute))
			if len(pending.RunAttempts) == 0 {
				t.Fatal("semantic fixture has no frozen RunAttempt")
			}
			tt.mutate(&pending)
			writeTamperedClosePending(t, s, pending, true)
			assertClosePendingFrameRejected(t, s, workID)
		})
	}
}

func TestClosePendingFrameRejectsResealedGitExpectationTamper(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *closePending)
	}{
		{name: "repository_path", mutate: func(t *testing.T, p *closePending) {
			p.Git.RepositoryPath = t.TempDir()
		}},
		{name: "ref", mutate: func(_ *testing.T, p *closePending) {
			p.Git.Expectations[0].Ref = "refs/heads/tampered"
		}},
		{name: "sha", mutate: func(_ *testing.T, p *closePending) {
			p.Git.Expectations[0].SHA = strings.Repeat("a", 40)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 7, 14, 6, 0, 0, 0, time.UTC)
			fixture := newLandingProofFixture(t, now)
			pending := freezeClosePendingAfterPrepare(t, fixture.store, fixture.workID, CloseInput{
				Outcome:   "completed",
				MoveType:  "outward_delta",
				Actor:     "codex",
				Summary:   "bind Git expectation to frozen landing proof",
				ProofRefs: []string{fixture.proof.EvidenceRef},
			}, now.Add(10*time.Minute))
			if len(pending.Git.Expectations) != 1 {
				t.Fatalf("Git expectations = %+v, want one local target", pending.Git.Expectations)
			}
			tt.mutate(t, &pending)
			writeTamperedClosePending(t, fixture.store, pending, true)
			assertClosePendingFrameRejected(t, fixture.store, fixture.workID)
		})
	}
}

func TestClosePendingFrameRejectsRunAttemptMembershipTamper(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*closePending)
	}{
		{name: "omitted", mutate: func(p *closePending) {
			p.RunAttempts = nil
		}},
		{name: "duplicated", mutate: func(p *closePending) {
			p.RunAttempts = append(p.RunAttempts, p.RunAttempts[0])
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, _ := closeTerminalizeWork(t, s, "close-frame-membership-"+tt.name, fixedNow)
			pending := freezeClosePendingAfterPrepare(t, s, workID, CloseInput{
				Outcome:   "completed",
				MoveType:  "control_plane_repair",
				Actor:     "codex",
				Summary:   "bind exact RunAttempt membership",
				ProofRefs: []string{"check:close-frame-membership"},
			}, fixedNow.Add(5*time.Minute))
			if len(pending.RunAttempts) == 0 {
				t.Fatal("membership fixture has no frozen RunAttempt")
			}
			tt.mutate(&pending)
			writeTamperedClosePending(t, s, pending, true)
			assertClosePendingFrameRejected(t, s, workID)
		})
	}
}
