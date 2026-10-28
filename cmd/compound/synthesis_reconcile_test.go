package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/mem"
)

func TestSynthesisSweepCLIReconcileStageRunsAndCheckpoints(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if _, err := mem.Open(homePath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimAt := now.Add(-2 * time.Hour).Format(time.RFC3339)
	receiptAt := now.Add(-time.Hour).Format(time.RFC3339)

	appendClaimLine(t, filepath.Join(homePath, "ledger", "claims.jsonl"), mem.Claim{
		ID:             "claim_cli_used",
		Seq:            1,
		Entity:         "project:cli",
		Predicate:      "runtime",
		Object:         "Go",
		ValidFrom:      claimAt,
		Confidence:     0.9,
		Derivation:     "extracted",
		PromotionBasis: "explicit_confirmation",
		EvidenceRefs:   []string{"trace_cli_used"},
		Visibility:     "ambient",
		CreatedAt:      claimAt,
	})
	writeSynthesisReconcileCLIReceipt(t, homePath, "rec_cli_usage", receiptAt, []string{"claim://claim_cli_used"})

	first := captureStdout(t, func() {
		cmdSynthesis([]string{"sweep", "--stage", "reconcile", "--json"})
	})
	var firstResult synthesisSweepOutput
	if err := json.Unmarshal([]byte(first), &firstResult); err != nil {
		t.Fatalf("parse first reconcile output: %v\n%s", err, first)
	}
	if len(firstResult.Report.Stages) != 1 || firstResult.Report.Stages[0] != "reconcile" {
		t.Fatalf("stages = %+v, want [reconcile]", firstResult.Report.Stages)
	}
	if firstResult.Report.ReconcileDecayRoutedCount != 1 {
		t.Fatalf("first decay-routed = %d, want 1; report=%+v", firstResult.Report.ReconcileDecayRoutedCount, firstResult.Report)
	}
	if firstResult.Report.ReconcileReopenedCount != 0 || firstResult.Report.ReviewDebtCount != 0 {
		t.Fatalf("usage-only CLI reconcile must not reopen claims: %+v", firstResult.Report)
	}
	if len(firstResult.ContradictionReviewPaths) != 0 || len(firstResult.SeedCorroborationReviewPaths) != 0 {
		t.Fatalf("reconcile CLI should not create review cards: contradictions=%v seeds=%v", firstResult.ContradictionReviewPaths, firstResult.SeedCorroborationReviewPaths)
	}
	if _, err := os.Stat(filepath.Join(homePath, "reviews", "reconcile", "checkpoint.json")); err != nil {
		t.Fatalf("reconcile checkpoint missing: %v", err)
	}

	second := captureStdout(t, func() {
		cmdSynthesis([]string{"sweep", "--stage", "reconcile", "--json"})
	})
	var secondResult synthesisSweepOutput
	if err := json.Unmarshal([]byte(second), &secondResult); err != nil {
		t.Fatalf("parse second reconcile output: %v\n%s", err, second)
	}
	if secondResult.Report.ReconcileDismissedCount != 0 ||
		secondResult.Report.ReconcileReopenedCount != 0 ||
		secondResult.Report.ReconcileRaisedCount != 0 ||
		secondResult.Report.ReconcileDecayRoutedCount != 0 ||
		secondResult.Report.ReconcileParkedCount != 0 ||
		secondResult.Report.ReviewDebtCount != 0 {
		t.Fatalf("second reconcile should be idempotent, got %+v", secondResult.Report)
	}
}

func writeSynthesisReconcileCLIReceipt(t *testing.T, homePath, id, createdAt string, used []string) {
	t.Helper()
	rec := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            id,
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "test", Name: "compound-test"},
			IDs:           envelope.IDs{WorkID: "work_cli_usage"},
			Scope:         envelope.Scope{EntityID: "project:cli"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    "completed",
		MoveType:   "audit_only",
		StateDelta: envelope.StateDelta{Changed: false, Summary: "usage-only close"},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Used: used, UsedDeclared: true, ApplicationMethod: "explicit_declaration"},
		Memory:     envelope.ReceiptMemory{},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(homePath, "events", id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
