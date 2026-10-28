package director

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/candidates"
	"compound/internal/decisions"
)

// TestAbsorptionDebtProposalForManualRow is T1 (positive half): a row whose
// absorption is self-asserted (proof_status=manual, no auto-proof wired)
// must surface as an absorption_debt director proposal naming the row and
// citing the os-absorption doctor authority ref (W5 J15).
func TestAbsorptionDebtProposalForManualRow(t *testing.T) {
	home := seededDirectorHome(t)
	seedOSAbsorptionMatrix(t, home, directorTestNow, []authority.OSAbsorptionMatrixRow{
		absorptionRow("os_source_test_wrapper", "cli:test wrapper", "wrapper",
			"compound://registries/test-owner", "test consumer", "asserted, no auto-proof wired",
			"consumer reads Compound authority"),
	})

	report, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	found := findProposalByKind(report, "absorption_debt")
	if found == nil {
		t.Fatalf("expected an absorption_debt proposal, got %+v", report.Proposals)
	}
	if !strings.Contains(found.Title, "os_source_test_wrapper") {
		t.Fatalf("proposal should name the row: %+v", found)
	}
	assertHasRef(t, found.EvidenceRefs, authority.OSAbsorptionAuthorityRef)
	assertHasPrefix(t, found.EvidenceRefs, "os_absorption_row:os_source_test_wrapper")
}

// TestAbsorptionDebtNoProposalWhenAllRowsProven is T1 (negative half): once
// every row is machine-proven (proof_status=ok), the scanner must not invent
// absorption debt.
func TestAbsorptionDebtNoProposalWhenAllRowsProven(t *testing.T) {
	home := seededDirectorHome(t)
	if _, err := decisions.RecordSourceAuthority(home, decisions.DecisionImportReport{
		SourcePath:  "/synthetic/operating-system/decisions/DEC-0001.md",
		SourceKind:  "decision_source",
		EvidenceID:  "ev_dec_1",
		CandidateID: "cand_dec_1",
		ClaimID:     "claim_dec_1",
		Verdict:     candidates.Promote,
		Lifecycle:   decisions.Lifecycle{ID: "dec_0001"},
	}, "test import", directorTestNow); err != nil {
		t.Fatalf("record decisions source authority: %v", err)
	}
	seedOSAbsorptionMatrix(t, home, directorTestNow, []authority.OSAbsorptionMatrixRow{
		absorptionRow("os_source_decisions_repo", "/synthetic/operating-system/decisions/", "evidence_only",
			"compound://ledger/decisions", "compound decisions authority", "auto-proofed",
			"old markdown is citation and rollback evidence"),
	})

	report, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if found := findProposalByKind(report, "absorption_debt"); found != nil {
		t.Fatalf("expected no absorption_debt proposal when every row is proven: %+v", found)
	}
}

// TestAbsorptionDebtDoesNotStarveHigherSeverityClasses is T2: absorption_debt
// ranks below synthesis_contradiction, but class-fair round-robin must still
// reach it (not zero proposals) while synthesis_contradiction — the more
// severe, more abundant class here — is also not starved (zero proposals).
func TestAbsorptionDebtDoesNotStarveHigherSeverityClasses(t *testing.T) {
	home := seededDirectorHome(t)
	for i := 0; i < 6; i++ {
		seedSynthesisContradiction(t, home, fmt.Sprintf("synth-%03d", i), directorTestNow.Add(time.Duration(i)*time.Minute))
	}
	seedOSAbsorptionMatrix(t, home, directorTestNow, []authority.OSAbsorptionMatrixRow{
		absorptionRow("os_source_test_wrapper_a", "cli:test wrapper a", "wrapper",
			"compound://registries/test-owner-a", "consumer a", "asserted a", "gate a"),
		absorptionRow("os_source_test_wrapper_b", "cli:test wrapper b", "wrapper",
			"compound://registries/test-owner-b", "consumer b", "asserted b", "gate b"),
	})

	report, err := Propose(home, Options{Now: directorTestNow.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProposalCount != 5 {
		t.Fatalf("director run should still cap proposals at five: %+v", report)
	}
	if !hasProposalKind(report, "absorption_debt") {
		t.Fatalf("absorption_debt was starved by synthesis contradiction volume: %+v", report.Proposals)
	}
	if !hasProposalKind(report, "synthesis_contradiction") {
		t.Fatalf("higher severity synthesis_contradiction class was starved: %+v", report.Proposals)
	}
}

// TestAbsorptionDebtDedupesAcrossRuns is T3: a second propose run over an
// unchanged manual row must not re-propose it, and must record a suppressed
// duplicate instead of a fresh envelope.
func TestAbsorptionDebtDedupesAcrossRuns(t *testing.T) {
	home := seededDirectorHome(t)
	seedDirectorApplyGrant(t, home, directorTestNow)
	seedOSAbsorptionMatrix(t, home, directorTestNow, []authority.OSAbsorptionMatrixRow{
		absorptionRow("os_source_test_wrapper", "cli:test wrapper", "wrapper",
			"compound://registries/test-owner", "test consumer", "asserted", "gate text"),
	})

	first, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if !hasProposalKind(first, "absorption_debt") {
		t.Fatalf("expected absorption_debt proposal on first run: %+v", first)
	}

	second, err := Propose(home, Options{Apply: true, Now: directorTestNow.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if hasProposalKind(second, "absorption_debt") {
		t.Fatalf("second run should not re-propose the same row: %+v", second)
	}
	if second.SuppressedDuplicateCount == 0 {
		t.Fatalf("second run should record a suppressed duplicate: %+v", second)
	}
}

// TestAbsorptionDebtNamesOperatorGateWithoutInstructingExecution is T4: a
// pending row whose retirement_gate names an operator-only next step must
// produce a proposal that names the gate in its text, and whose action
// context never instructs running the gated (ssh/DB) step itself.
func TestAbsorptionDebtNamesOperatorGateWithoutInstructingExecution(t *testing.T) {
	home := seededDirectorHome(t)
	seedDirectorApplyGrant(t, home, directorTestNow)
	const gate = "blocked on operator: read-only gbrain DB export (J14 packet)"
	seedOSAbsorptionMatrix(t, home, directorTestNow, []authority.OSAbsorptionMatrixRow{
		absorptionRow("os_source_test_gbrain", "gbrain://synthetic-store", "pending",
			"compound://adapters/memory-engine/gbrain", "compound memory recall", "gbrain rented adapter",
			gate),
	})

	report, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	found := findProposalByKind(report, "absorption_debt")
	if found == nil {
		t.Fatalf("expected an absorption_debt proposal for the pending row: %+v", report.Proposals)
	}
	if !strings.Contains(found.Title, gate) && !strings.Contains(found.Consequence, gate) {
		t.Fatalf("proposal text should name the operator gate %q: %+v", gate, found)
	}
	if found.WorkPlan.ActionContext == nil {
		t.Fatalf("expected an action context on the applied proposal: %+v", found.WorkPlan)
	}
	for _, cmd := range found.WorkPlan.ActionContext.Command {
		if strings.Contains(strings.ToLower(cmd), "ssh") {
			t.Fatalf("proposal command must not instruct the gated ssh step: %+v", found.WorkPlan.ActionContext)
		}
	}
	boundary := strings.ToLower(found.WorkPlan.ActionContext.Boundary)
	if !strings.Contains(boundary, "never execute") {
		t.Fatalf("boundary should forbid executing the operator-gated step: %q", found.WorkPlan.ActionContext.Boundary)
	}
	joinedStops := strings.ToLower(strings.Join(found.WorkPlan.ActionContext.StopConditions, "\n"))
	if !strings.Contains(joinedStops, "operator") {
		t.Fatalf("stop conditions should call out the operator gate: %v", found.WorkPlan.ActionContext.StopConditions)
	}
}

func TestAbsorptionDebtCandidatesSkipOperatorGatedHead(t *testing.T) {
	home := seededDirectorHome(t)
	const (
		gatedID      = "os_source_001_operator_gated"
		actionableID = "os_source_002_actionable"
	)
	seedOSAbsorptionMatrix(t, home, directorTestNow, []authority.OSAbsorptionMatrixRow{
		absorptionRow(gatedID, "legacy://operator-gated", "pending",
			"compound://registries/test-owner-gated", "gated consumer", "proof pending",
			"blocked on operator confirmation"),
		absorptionRow(actionableID, "legacy://actionable", "pending",
			"compound://registries/test-owner-actionable", "actionable consumer", "proof pending",
			"verify the Compound consumer"),
	})

	candidates, err := absorptionDebtCandidates(home, directorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("pending absorption candidate count = %d; want 1: %+v", len(candidates), candidates)
	}
	if got := candidates[0].SourceID; got != actionableID {
		t.Fatalf("pending absorption candidate source = %q; want actionable row %q (lowest row %q is operator-gated)", got, actionableID, gatedID)
	}
}

func TestAbsorptionDebtCandidatesFallBackToLowestIDWhenAllPendingRowsOperatorGated(t *testing.T) {
	home := seededDirectorHome(t)
	const lowestID = "os_source_001_operator_gated"
	seedOSAbsorptionMatrix(t, home, directorTestNow, []authority.OSAbsorptionMatrixRow{
		absorptionRow(lowestID, "legacy://operator-gated-low", "pending",
			"compound://registries/test-owner-gated-low", "low consumer", "proof pending",
			"blocked on operator confirmation for low row"),
		absorptionRow("os_source_002_operator_gated", "legacy://operator-gated-high", "pending",
			"compound://registries/test-owner-gated-high", "high consumer", "proof pending",
			"blocked on operator confirmation for high row"),
	})

	candidates, err := absorptionDebtCandidates(home, directorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("pending absorption candidate count = %d; want 1: %+v", len(candidates), candidates)
	}
	if got := candidates[0].SourceID; got != lowestID {
		t.Fatalf("all-gated pending absorption candidate source = %q; want lowest row %q", got, lowestID)
	}
}

func seedOSAbsorptionMatrix(t *testing.T, home string, now time.Time, rows []authority.OSAbsorptionMatrixRow) {
	t.Helper()
	matrixPath := filepath.Join(t.TempDir(), "os-absorption-source-matrix.json")
	matrix := authority.OSAbsorptionSourceMatrix{
		SchemaVersion: authority.OSAbsorptionMatrixSchema,
		Rows:          rows,
	}
	body, err := json.MarshalIndent(matrix, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(matrixPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ImportOSAbsorptionMatrix(home, matrixPath, now); err != nil {
		t.Fatalf("import os absorption matrix: %v", err)
	}
}

func absorptionRow(id, source, status, compoundOwner, consumer, proof, retirementGate string) authority.OSAbsorptionMatrixRow {
	return authority.OSAbsorptionMatrixRow{
		ID:             id,
		SourceRef:      source,
		OldOwner:       "legacy",
		OldRole:        "legacy source",
		CurrentRole:    "evidence_only_input",
		Privacy:        "private",
		CompoundOwner:  compoundOwner,
		Consumer:       consumer,
		Proof:          proof,
		RetirementGate: retirementGate,
		Status:         status,
	}
}

func seedSynthesisContradiction(t *testing.T, home, id string, createdAt time.Time) {
	t.Helper()
	seedReviewDebtWithCreatedAt(t, home, "synthesis-contradictions", id, createdAt)
}

func findProposalByKind(report Report, kind string) *Proposal {
	for i := range report.Proposals {
		if report.Proposals[i].Kind == kind {
			return &report.Proposals[i]
		}
	}
	return nil
}
