package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

func appendEfficiencyForeignHistory(t *testing.T, s *Store) {
	t.Helper()
	const rows = 127353
	const minimumBytes = 47 << 20
	row := Event{TS: ts(fixedNow), WorkID: "work_foreign_history", From: "ready", To: "running", By: "synthetic"}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	// Match the observed log's byte and row scale without creating unrelated
	// live WorkEnvelopes. Every foreign row still passes the strict decoder.
	row.Note = strings.Repeat("x", (minimumBytes+rows-1)/rows-len(encoded)+32)
	encoded, err = json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	var history bytes.Buffer
	history.Grow(len(encoded) * rows)
	for range rows {
		history.Write(encoded)
	}
	if history.Len() < minimumBytes {
		t.Fatalf("fixture bytes=%d, want at least %d", history.Len(), minimumBytes)
	}
	file, err := os.OpenFile(s.eventsLog(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(history.Bytes())
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	t.Logf("foreign history: rows=%d bytes=%d", rows, history.Len())
}

func TestAdvanceAttachmentHistoryReadsDoNotScaleWithRetainedAttempts(t *testing.T) {
	var baselineReads int64
	for _, count := range []int{1, 24} {
		t.Run(fmt.Sprintf("retained_%d", count), func(t *testing.T) {
			s := newStore(t)
			w, err := s.Create(CreateInput{Owner: "codex", SuccessPredicate: "retain exact attempt authority with bounded history reads"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Claim(w.ID, "codex", fixedNow.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			prior := make([]string, 0, count)
			for i := range count {
				ra := mintExecutionRunAttempt(t, s, w.ID, fixedNow.Add(time.Duration(i+2)*time.Second))
				prior = append(prior, ra.ID)
			}
			running, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex", RunAttemptIDs: prior}, fixedNow.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			attempts := runattempt.Open(s.home)
			for _, id := range prior {
				if _, err = attempts.Complete(id, runattempt.StatusSucceeded, fixedNow.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			fresh := mintExecutionRunAttempt(t, s, w.ID, fixedNow.Add(3*time.Minute))
			if _, err = attempts.Complete(fresh.ID, runattempt.StatusSucceeded, fixedNow.Add(4*time.Minute)); err != nil {
				t.Fatal(err)
			}
			all := append(append([]string{}, prior...), fresh.ID)
			appendEfficiencyForeignHistory(t, s)

			before := s.eventLogReadCount()
			advanced, err := s.Advance(w.ID, AdvanceInput{
				To: "verifying", Actor: "codex", RunAttemptIDs: all,
				ExpectedWorkState:        "running",
				ExpectedPolicyDecisionID: running.PolicyDecisionID,
				ExpectedActionIntentID:   latestActionIntentID(running),
			}, fixedNow.Add(5*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			reads := s.eventLogReadCount() - before
			t.Logf("retained attempts=%d full lifecycle reads=%d", count, reads)
			if count == 1 {
				baselineReads = reads
			} else if reads != baselineReads {
				t.Errorf("history reads scale with retained attempts: one=%d many=%d", baselineReads, reads)
			}
			events, err := s.eventsFor(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range all {
				attachments := 0
				for _, event := range events {
					if attachedHas(event.Attached, id) {
						attachments++
					}
				}
				if attachments != 1 {
					t.Fatalf("attempt %s has %d attaching events, want one", id, attachments)
				}
				ra, err := attempts.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				authority, err := s.resolveRunAttemptAuthorityFromEvents(advanced, ra, events)
				wantAction, wantPolicy := latestActionIntentID(running), running.PolicyDecisionID
				if id == fresh.ID {
					wantAction, wantPolicy = latestActionIntentID(advanced), advanced.PolicyDecisionID
				}
				if err != nil || authority.ActionIntentID != wantAction || authority.PolicyDecisionID != wantPolicy {
					t.Fatalf("attempt %s authority changed: %+v err=%v", id, authority, err)
				}
			}
			if count == 24 {
				// A scoped attachment lookup must not hide corruption elsewhere
				// in the canonical log, even after a previous successful read.
				beforeWork, err := os.ReadFile(s.workPath(w.ID))
				if err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(s.eventsLog(), os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString("{invalid foreign row}\n")
				if err = errors.Join(writeErr, file.Close()); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex", RunAttemptIDs: all}, fixedNow.Add(6*time.Minute)); err == nil {
					t.Fatal("malformed unrelated lifecycle history accepted")
				}
				afterWork, err := os.ReadFile(s.workPath(w.ID))
				if err != nil || !bytes.Equal(beforeWork, afterWork) {
					t.Fatalf("corrupt history refusal changed work: %v", err)
				}
			}
		})
	}
}

func efficiencyCloseReadyWork(t *testing.T) (*Store, envelope.WorkEnvelope, *runattempt.Store, runattempt.RunAttempt) {
	t.Helper()
	s, w, attempts, ra := censusRunningWork(t)
	if _, err := attempts.CommitLaunch(ra.ID, fixedNow.Add(4*time.Minute), func() error {
		return errors.New("synthetic refusal before subprocess start")
	}); err == nil {
		t.Fatal("synthetic start unexpectedly succeeded")
	}
	if _, err := attempts.Complete(ra.ID, runattempt.StatusFailed, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	w, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex"}, fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return s, w, attempts, ra
}

func TestCloseCensusForeignLaunchEvidenceStaysOutsideOwnedScope(t *testing.T) {
	s, w, _, owned := efficiencyCloseReadyWork(t)
	foreign := corruptUnrelatedRunAttemptLaunchEvidence(t, s, "work_foreign_launch")
	if _, err := runattempt.Open(s.home).List(); err == nil {
		t.Fatal("foreign launch corruption falsifier is not armed")
	}
	closed, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex"}, fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("unrelated launch evidence blocked exact-work close: %v", err)
	}
	if !slices.Contains(closed.Work.RunAttemptIDs, owned.ID) || slices.Contains(closed.Work.RunAttemptIDs, foreign) {
		t.Fatalf("close lost reverse-only owned evidence or imported foreign evidence: %v", closed.Work.RunAttemptIDs)
	}
	if _, err := runattempt.Open(s.home).Get(foreign); err == nil {
		t.Fatal("close silently repaired unrelated launch evidence")
	}
}

func TestCloseCensusValidatesEveryForeignMainRecord(t *testing.T) {
	for _, kind := range []string{"malformed", "unreadable", "identity-mismatch", "missing-work", "invalid-schema"} {
		t.Run(kind, func(t *testing.T) {
			s, w, attempts, _ := efficiencyCloseReadyWork(t)
			foreign, err := attempts.Mint("work_foreign_main", "synthetic", "claude-code", fixedNow.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.home, "run_attempts", foreign.ID+".json")
			switch kind {
			case "malformed":
				err = os.WriteFile(path, []byte("{invalid"), 0600)
			case "unreadable":
				if err = os.Remove(path); err == nil {
					err = os.Mkdir(path, 0700)
				}
			default:
				switch kind {
				case "identity-mismatch":
					foreign.ID = runattempt.MintDeterministic("foreign-embedded-mismatch", fixedNow)
				case "missing-work":
					foreign.WorkID = ""
				case "invalid-schema":
					foreign.SchemaVersion = "unknown.schema"
				}
				var body []byte
				body, err = json.Marshal(foreign)
				if err == nil {
					err = os.WriteFile(path, body, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex"}, fixedNow.Add(7*time.Minute)); err == nil {
				t.Fatal("uncertain foreign main-record membership accepted")
			}
			after, err := os.ReadFile(s.workPath(w.ID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected census mutated work: %v", err)
			}
		})
	}
}

func TestCloseCensusOwnedCorruptLaunchEvidenceStillRefuses(t *testing.T) {
	s, w, _, owned := efficiencyCloseReadyWork(t)
	path := filepath.Join(s.home, "run_attempts", "launch", owned.ID+".committed.json")
	if err := os.WriteFile(path, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex"}, fixedNow.Add(7*time.Minute)); err == nil {
		t.Fatal("owned corrupt launch evidence accepted")
	}
	after, err := os.ReadFile(s.workPath(w.ID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("owned launch refusal mutated work: %v", err)
	}
}
