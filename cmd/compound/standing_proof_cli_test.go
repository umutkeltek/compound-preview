package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/work"
)

func completedCloseResult(workID, receiptID, predicate string) work.CloseResult {
	return closeResultWithState(workID, receiptID, predicate, "completed")
}

func closeResultWithState(workID, receiptID, predicate, state string) work.CloseResult {
	return work.CloseResult{
		Work: envelope.WorkEnvelope{
			Header:           envelope.Header{ID: workID},
			State:            state,
			SuccessPredicate: predicate,
		},
		Receipt: envelope.Receipt{Header: envelope.Header{ID: receiptID}},
	}
}

func inboxCandidateCount(t *testing.T, home string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "inbox", "memory-candidates"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read inbox: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// TestStandingProofGraduationOnCloseMintsForCommandPredicate proves the cmd-layer close exhaust
// fires on a SUCCESSFUL close whose success predicate is a machine-checkable proof command: it
// mints exactly one proposed candidate keyed to the work + command, and an idempotent repeat
// (re-close) mints no duplicate.
func TestStandingProofGraduationOnCloseMintsForCommandPredicate(t *testing.T) {
	home := t.TempDir()
	workID := "work_cliproof01"
	command := "go test ./internal/work/..."
	res := completedCloseResult(workID, "rec_cli01", command)

	offerStandingProofGraduationOnClose(home, res)

	if n := inboxCandidateCount(t, home); n != 1 {
		t.Fatalf("candidate count = %d, want exactly 1", n)
	}
	id := mem.StandingProofCandidateID(workID, command)
	var c mem.Candidate
	if err := candidates.ReadCandidate(home, candidates.Memory, id, &c); err != nil {
		t.Fatalf("read minted candidate: %v", err)
	}
	if c.State != "proposed" || c.Predicate != "standing_proof_offered" {
		t.Fatalf("minted candidate state=%q predicate=%q, want proposed/standing_proof_offered", c.State, c.Predicate)
	}
	if !strings.Contains(c.Object, command) {
		t.Fatalf("candidate object must carry verbatim command %q, got %q", command, c.Object)
	}

	// Idempotent re-close: no duplicate.
	offerStandingProofGraduationOnClose(home, completedCloseResult(workID, "rec_cli01_again", command))
	if n := inboxCandidateCount(t, home); n != 1 {
		t.Fatalf("candidate count after re-close = %d, want exactly 1 (no duplicate)", n)
	}
}

// TestStandingProofGraduationOnCloseSkipsProseAndNonCompleted proves the gate is conservative:
// a prose success predicate mints nothing, and a non-completed (failed) close mints nothing even
// when its predicate is command-shaped.
func TestStandingProofGraduationOnCloseSkipsProseAndNonCompleted(t *testing.T) {
	home := t.TempDir()

	// Prose predicate on a completed close -> nothing.
	offerStandingProofGraduationOnClose(home, completedCloseResult("work_prose", "rec_p", "candidate reaches main"))
	if n := inboxCandidateCount(t, home); n != 0 {
		t.Fatalf("prose predicate minted %d candidates, want 0", n)
	}

	// Command predicate but a FAILED close -> nothing (only a success graduates a proof).
	offerStandingProofGraduationOnClose(home, closeResultWithState("work_failed", "rec_f", "go test ./...", "failed"))
	if n := inboxCandidateCount(t, home); n != 0 {
		t.Fatalf("failed close minted %d candidates, want 0", n)
	}
}

// TestStandingProofGraduationOnCloseMintFailureDoesNotFailClose proves a mint failure NEVER fails
// the (already-successful) close: the function returns normally and surfaces a VISIBLE stderr
// warning rather than panicking or exiting. The failure is forced by planting a regular file where
// the blob directory must be created.
func TestStandingProofGraduationOnCloseMintFailureDoesNotFailClose(t *testing.T) {
	home := t.TempDir()
	// Plant a file at observed/standing-proof so the mem door's MkdirAll fails.
	if err := os.MkdirAll(filepath.Join(home, "observed"), 0o755); err != nil {
		t.Fatalf("mkdir observed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "observed", "standing-proof"), []byte("blocker"), 0o644); err != nil {
		t.Fatalf("plant blocker file: %v", err)
	}

	stderr := captureStderrStandingProof(t, func() {
		offerStandingProofGraduationOnClose(home, completedCloseResult("work_mintfail", "rec_mf", "go test ./..."))
	})
	if !strings.Contains(stderr, "warning: standing-proof graduation offer failed") {
		t.Fatalf("expected a visible mint-failure warning on stderr, got %q", stderr)
	}
	// No candidate was written, and the process did not exit -- the close stands.
	if n := inboxCandidateCount(t, home); n != 0 {
		t.Fatalf("mint failure should write no candidate, got %d", n)
	}
}

func captureStderrStandingProof(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
