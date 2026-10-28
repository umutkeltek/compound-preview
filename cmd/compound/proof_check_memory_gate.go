package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/work"
)

// The memory-doctor gate runs on a home this gate OWNS, seeded in-process, and it asserts
// which hops reported healthy. Both halves are load-bearing, for two different reasons.
//
// COST. `memory doctor` is 0.01s on a fresh home and 4.3s on the operator's live one, and
// that gap grows every time Compound is used — a gate whose cost no diff caused, and which
// no diff can bound.
//
// TEETH. Isolation ALONE would have been worse than the cost it saved. On a home with no
// candidates and no claims every hop reports "empty", and mem.Report only flips OK on
// "fail" (statusOf: internal/mem/mem.go), so the gate exits 0 having asserted NOTHING. The
// live home was at least real data; a bare temp home is a vacuous green. So the fixture
// below makes each hop execute against actual bytes, and proofCheckAssertMemoryDoctorHops
// refuses a report whose hops are merely "not failed".
//
// The seeded home also makes the gate honestly CI-portable: it no longer reads a
// COMPOUND_HOME body it did not create.
const proofCheckMemoryFixtureEntity = "Compound"

func runProofCheckMemoryDoctorGate(repo string, step proofCheckStep) ([]byte, error) {
	return runProofCheckMemoryDoctorGateContext(context.Background(), repo, step)
}

func runProofCheckMemoryDoctorGateContext(ctx context.Context, repo string, step proofCheckStep) ([]byte, error) {
	tmpHome, err := os.MkdirTemp("", "compound-proofcheck-mem-home-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpHome)
	now := time.Now().UTC()
	if err := home.Init(tmpHome, "proof-check", now); err != nil {
		return nil, fmt.Errorf("initialize proof-check memory home: %w", err)
	}
	if err := seedProofCheckMemoryHome(tmpHome, now); err != nil {
		return nil, fmt.Errorf("seed proof-check memory fixture: %w", err)
	}
	return runProofCheckMemoryDoctorInHomeContext(ctx, repo, tmpHome, step)
}

// seedProofCheckMemoryHome promotes one claim through the real public door (Capture →
// Adjudicate), so the fixture cannot drift from what promotion actually writes.
//
// The entity is deliberately alphabetic and unique under case-folding: the recall-quality
// hop probes an alternate-case spelling of an unambiguous entity and SKIPS (reporting
// "empty") when no candidate qualifies — a digits-only or duplicated entity would silently
// cost us that hop.
func seedProofCheckMemoryHome(homePath string, now time.Time) error {
	store, err := mem.Open(homePath)
	if err != nil {
		return err
	}
	seed := mem.Candidate{
		Entity:          proofCheckMemoryFixtureEntity,
		Predicate:       "proof_check_memory_gate",
		Object:          "seeded fixture claim",
		ObservationDate: now.Format("2006-01-02"),
		Confidence:      0.9,
		Derivation:      "deterministic",
		ClaimType:       "status",
		// trusted_deterministic_import, not explicit_confirmation: the latter requires
		// governed confirmation evidence and refuses the plain Adjudicate door.
		PromotionBasis: candidates.BasisTrustedDeterministic,
		SourceTraceRef: "proof-check/memory-doctor-gate",
		EvidenceRefs:   []string{"proof-check/memory-doctor-gate"},
		SnippetHash:    "proofcheckmemoryfixture",
		Visibility:     "operational",
	}
	captured, err := store.Capture([]mem.Candidate{seed}, now)
	if err != nil {
		return fmt.Errorf("capture fixture candidate: %w", err)
	}
	if len(captured) != 1 {
		return fmt.Errorf("capture fixture candidate: got %d candidates, want 1", len(captured))
	}
	if _, err := store.Adjudicate(captured[0].ID, "promote", "", now); err != nil {
		return fmt.Errorf("promote fixture candidate: %w", err)
	}
	// Reconcile: the capture hop only reports a truthful backlog from a
	// reconciled state (the promote-driver sweep writes it in production),
	// so the fixture models the driver-ran steady state — zero pending.
	if _, err := store.ReconcilePendingState("proof-check fixture", now); err != nil {
		return fmt.Errorf("reconcile fixture pending state: %w", err)
	}
	return nil
}

// runProofCheckMemoryDoctorInHome runs the gate's command against an explicit home. Split
// from runProofCheckMemoryDoctorGate so the negative controls can hand it a home whose
// ledger has been corrupted on purpose, and prove this gate can still go red.
func runProofCheckMemoryDoctorInHome(repo, homePath string, step proofCheckStep) ([]byte, error) {
	return runProofCheckMemoryDoctorInHomeContext(context.Background(), repo, homePath, step)
}

func runProofCheckMemoryDoctorInHomeContext(ctx context.Context, repo, homePath string, step proofCheckStep) ([]byte, error) {
	executable, err := work.ProofRuntimeExecutable(ctx, step.Command[0], proofCheckRuntimeExecutable)
	if err != nil {
		return nil, err
	}
	cmd := proofCheckCommandContext(ctx, executable, step.Command[1:]...)
	cmd.Dir = repo
	// COMPOUND_MEM_ENGINE_BIN is cleared, not just left unset: the operator's shell may
	// export it, and a wired engine would add a hop whose result depends on a rented
	// index rather than on the diff under test.
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, "COMPOUND_MEM_ENGINE_BIN=")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, err
	}
	if err := proofCheckAssertMemoryDoctorHops(output); err != nil {
		return output, err
	}
	return output, nil
}

// proofCheckMemoryDoctorRequiredHops are the hops the fixture makes reachable. Each must
// report "ok" — "empty" is the vacuous-pass state this gate exists to refuse.
var proofCheckMemoryDoctorRequiredHops = []string{"capture", "promotion", "retrieval", "recall-quality", "authority"}

func proofCheckAssertMemoryDoctorHops(output []byte) error {
	var report mem.Report
	if err := json.Unmarshal(output, &report); err != nil {
		return fmt.Errorf("parse memory doctor report: %w", err)
	}
	if !report.OK {
		return fmt.Errorf("memory doctor verdict not ok: %s", report.Note)
	}
	status := make(map[string]string, len(report.Hops))
	for _, hop := range report.Hops {
		status[hop.Name] = hop.Status
	}
	for _, name := range proofCheckMemoryDoctorRequiredHops {
		got, present := status[name]
		if !present {
			return fmt.Errorf("memory doctor report has no %q hop: the fixture no longer reaches it", name)
		}
		if got != "ok" {
			return fmt.Errorf("memory doctor %q hop = %q, want ok: the seeded fixture no longer exercises this hop, so a green verdict would assert nothing", name, got)
		}
	}
	return nil
}
