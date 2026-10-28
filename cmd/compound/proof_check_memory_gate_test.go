package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

// proofCheckMemoryGateStep returns the real gate step, pointed at the once-built test binary.
func proofCheckMemoryGateStep(t *testing.T) (repo string, step proofCheckStep) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..")), proofCheckStep{
		Name:     "memory-doctor",
		Command:  []string{compoundTestBinary(t), "memory", "doctor", "--json"},
		ProofRef: "memory-doctor",
	}
}

func TestProofCheckMemoryDoctorUsesCandidateRuntimeInsteadOfPATHCompound(t *testing.T) {
	repo, _ := proofCheckMemoryGateStep(t)
	homePath := proofCheckMemoryGateHome(t, false)
	resolver := proofCheckRuntimeExecutable
	resolved, err := resolver("compound")
	if err != nil {
		t.Fatal(err)
	}
	wantRuntime, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved != wantRuntime {
		t.Fatalf("compound candidate = %q, want current runtime %q", resolved, wantRuntime)
	}
	candidate := compoundTestBinary(t)
	proofCheckRuntimeExecutable = func(logical string) (string, error) {
		if logical == "compound" {
			return candidate, nil
		}
		return resolver(logical)
	}
	t.Cleanup(func() { proofCheckRuntimeExecutable = resolver })
	marker := filepath.Join(t.TempDir(), "stale-compound-ran")
	binDir := t.TempDir()
	stale := filepath.Join(binDir, "compound")
	if err := os.WriteFile(stale, []byte("#!/bin/sh\nprintf stale > \"$COMPOUND_STALE_MARKER\"\nexit 97\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPOUND_STALE_MARKER", marker)

	out, err := runProofCheckMemoryDoctorInHomeContext(context.Background(), repo, homePath, proofCheckStep{
		Name: "memory-doctor", Command: []string{"compound", "memory", "doctor", "--json"},
	})
	if err != nil {
		t.Fatalf("candidate runtime did not execute memory doctor successfully: %v\n%s", err, out)
	}
	for _, hop := range proofCheckMemoryDoctorRequiredHops {
		if !strings.Contains(string(out), `"`+hop+`"`) {
			t.Fatalf("candidate runtime output does not contain memory doctor hop %q:\n%s", hop, out)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale PATH compound ran instead of the candidate runtime: %v", err)
	}
}

// proofCheckMemoryGateHome returns an initialized home, seeded unless bare is true.
// The seed clock is WALL TIME, not a fixed fixture date: `memory doctor`
// runs on the gate's own clock and its freshness gates (pending-state
// reconciliation age) compare against it — a fixture date that ages past
// the freshness window would red the capture hop days after a green land.
func proofCheckMemoryGateHome(t *testing.T, bare bool) string {
	t.Helper()
	dir := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(dir, "proof-check-test", now); err != nil {
		t.Fatalf("home.Init: %v", err)
	}
	if !bare {
		if err := seedProofCheckMemoryHome(dir, now); err != nil {
			t.Fatalf("seed fixture: %v", err)
		}
	}
	return dir
}

// TestProofCheckMemoryDoctorGateProvesRealHops is the positive case: the gate owns its home,
// so it must be green without any live COMPOUND_HOME — and green here means the five seeded
// hops each reported "ok", not merely that nothing failed.
func TestProofCheckMemoryDoctorGateProvesRealHops(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, step := proofCheckMemoryGateStep(t)
	out, err := runProofCheckMemoryDoctorGate(repo, step)
	if err != nil {
		t.Fatalf("memory-doctor gate failed on its own seeded home: %v\n%s", err, out)
	}
	for _, hop := range proofCheckMemoryDoctorRequiredHops {
		if !strings.Contains(string(out), `"`+hop+`"`) {
			t.Fatalf("gate output does not mention required hop %q:\n%s", hop, out)
		}
	}
}

// TestProofCheckMemoryDoctorGateRefusesVacuousHome is the control that makes the isolation
// safe. On a bare home every hop reports "empty" and `memory doctor` exits 0, so isolation
// alone would have traded a slow real check for a fast one that asserts nothing. The gate
// must refuse that report.
func TestProofCheckMemoryDoctorGateRefusesVacuousHome(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, step := proofCheckMemoryGateStep(t)
	bare := proofCheckMemoryGateHome(t, true)
	out, err := runProofCheckMemoryDoctorInHome(repo, bare, step)
	if err == nil {
		t.Fatalf("gate passed on an unseeded home — a green verdict asserting nothing:\n%s", out)
	}
	if !strings.Contains(err.Error(), "assert nothing") {
		t.Fatalf("gate failed for the wrong reason: %v\n%s", err, out)
	}
	// The vacuous report itself must still be green, or this control is proving the wrong
	// thing: the point is that the CLI reports ok while having exercised nothing.
	if strings.Contains(string(out), `"ok":false`) {
		t.Fatalf("unseeded home reported a failed verdict; this control no longer tests vacuity:\n%s", out)
	}
}

// TestProofCheckMemoryDoctorGateFailsOnUnreadableLedger and its zero-provenance sibling
// hand-append to ledger/claims.jsonl, deliberately bypassing the write door: neither
// failure is reachable through the public API (promotion always writes a parsable row and
// always fills Derivation + EvidenceRefs), so a control that respected the door could not
// prove the gate is able to go red at all.
func TestProofCheckMemoryDoctorGateFailsOnUnreadableLedger(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, step := proofCheckMemoryGateStep(t)
	seeded := proofCheckMemoryGateHome(t, false)
	// A torn FINAL line is a crash-truncated append the ledger law tolerates
	// as never-written; genuine corruption must be mid-file to fail loud. The
	// second garbage line puts the first one mid-file.
	appendToClaimsLedger(t, seeded, "{ this is not a claim\n{ neither is this\n")
	out, err := runProofCheckMemoryDoctorInHome(repo, seeded, step)
	if err == nil {
		t.Fatalf("gate passed with an unparsable claims ledger:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot read claims") {
		t.Fatalf("promotion hop did not report the unreadable ledger: %v\n%s", err, out)
	}
}

func TestProofCheckMemoryDoctorGateFailsOnZeroProvenanceClaim(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, step := proofCheckMemoryGateStep(t)
	seeded := proofCheckMemoryGateHome(t, false)
	// A distinct entity so exactly one hop turns red: sharing the fixture's entity would
	// also make the recall-quality probe ambiguous and mask which assertion caught this.
	appendToClaimsLedger(t, seeded, `{"id":"claim_zero_provenance","seq":9001,"entity":"Zeroprov","predicate":"hand_appended","object":"no derivation, no evidence","valid_from":"2026-07-27","confidence":0.9,"derivation":"","evidence_refs":[],"visibility":"operational","created_at":"2026-07-27T12:00:00Z"}`+"\n")
	out, err := runProofCheckMemoryDoctorInHome(repo, seeded, step)
	if err == nil {
		t.Fatalf("gate passed with a claim carrying no provenance:\n%s", out)
	}
	if !strings.Contains(string(out), "missing provenance") {
		t.Fatalf("authority hop did not report the zero-provenance claim: %v\n%s", err, out)
	}
}

func appendToClaimsLedger(t *testing.T, homePath, line string) {
	t.Helper()
	path := filepath.Join(homePath, "ledger", "claims.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open claims ledger: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("append to claims ledger: %v", err)
	}
}
