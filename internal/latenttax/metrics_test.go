package latenttax

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
)

// --- fixture-home seeding: fabricated ledger objects (no provider/network) ---

func writeEvent(t *testing.T, home, id string, obj any) {
	t.Helper()
	dir := filepath.Join(home, "events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedPolicy(t *testing.T, home, id, workID, outcome string) {
	t.Helper()
	writeEvent(t, home, id, envelope.PolicyDecision{
		Header:         envelope.Header{SchemaVersion: envelope.SVPolicyDecision, ID: id, CreatedAt: "2026-07-13T00:00:00Z", IDs: envelope.IDs{WorkID: workID}},
		ActionIntentID: "act_" + id,
		Outcome:        outcome,
	})
}

func seedReceipt(t *testing.T, home, id, workID string) {
	t.Helper()
	writeEvent(t, home, id, envelope.Receipt{
		Header:   envelope.Header{SchemaVersion: envelope.SVReceipt, ID: id, CreatedAt: "2026-07-13T00:00:00Z", IDs: envelope.IDs{WorkID: workID}},
		Outcome:  "completed",
		MoveType: "outward_delta",
	})
}

func seedRoute(t *testing.T, home, id, workID string, tokens int, latency int64) {
	t.Helper()
	dir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ev := routeevidence.Evidence{
		SchemaVersion:  routeevidence.SchemaVersion,
		ID:             id,
		CreatedAt:      "2026-07-13T00:00:00Z",
		RequestID:      "req_" + id,
		WorkID:         workID,
		ClientProfile:  "test",
		Protocol:       "openai-compatible",
		SelectedTarget: routeevidence.Target{Provider: "p", Model: "m", Protocol: "openai-compatible"},
		Usage:          routeevidence.Usage{TotalTokens: tokens},
		LatencyMS:      latency,
		Status:         "completed",
		RecordedVia:    routeevidence.RecorderGeneric,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "execution-route-evidence.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// run is a compact run-record builder for tests.
func run(runID, taskID, armID, kind string, dose int, workID, verdict, receiptRef string) RunRecord {
	return RunRecord{
		SchemaVersion: RunRecordSchemaVersion, RunID: runID, TaskSetID: "ts", TaskID: taskID,
		ArmID: armID, ArmKind: kind, Dose: dose, WorkID: workID, Verdict: verdict,
		ReceiptRef: receiptRef, JudgeLaneID: "auto/judge", Blinded: true, CreatedAt: "2026-07-13T00:00:00Z",
	}
}

// TestMetricExtractionPerArmDistributions seeds a home with fabricated ledger objects
// spanning all four arms and asserts the derived per-arm distributions are correct:
// thrash from PolicyDecision DENIALS only (allows and other works must not count),
// tokens/latency from route evidence, success from the recorded verdict, and receipt
// traceability. It fails if the extraction miscounts any of these.
func TestMetricExtractionPerArmDistributions(t *testing.T) {
	home := t.TempDir()

	// gate_only work: 3 denials + 1 allow (only the 3 denials are thrash), tokens 100+50.
	seedPolicy(t, home, "pol_g1", "work_gate", "deny")
	seedPolicy(t, home, "pol_g2", "work_gate", "deny")
	seedPolicy(t, home, "pol_g3", "work_gate", "deny")
	seedPolicy(t, home, "pol_g4", "work_gate", "allow") // must NOT count as thrash
	seedReceipt(t, home, "rec_gate", "work_gate")
	seedRoute(t, home, "route_g1", "work_gate", 100, 40)
	seedRoute(t, home, "route_g2", "work_gate", 50, 20)

	// both work: 0 denials, tokens 150.
	seedReceipt(t, home, "rec_both", "work_both")
	seedRoute(t, home, "route_b1", "work_both", 150, 90)

	// obligation_in_context work: 0 denials, tokens 200.
	seedReceipt(t, home, "rec_oic", "work_oic")
	seedRoute(t, home, "route_o1", "work_oic", 200, 120)

	// A denial on a DIFFERENT, unrelated work — must not leak into any arm's thrash.
	seedPolicy(t, home, "pol_x", "work_unrelated", "deny")

	runs := []RunRecord{
		run("r_gate", "t1", "gate", ArmGateOnly, 20, "work_gate", VerdictPass, "rec_gate"),
		run("r_both", "t1", "both", ArmBoth, 20, "work_both", VerdictPass, "rec_both"),
		run("r_oic", "t1", "oic", ArmObligationInContext, 20, "work_oic", VerdictFail, "rec_oic"),
		// a filler run that is planned but NOT dispatched (no work_id) — must be pending, excluded.
		{SchemaVersion: RunRecordSchemaVersion, RunID: "r_fill", TaskSetID: "ts", TaskID: "t1", ArmID: "fill", ArmKind: ArmFiller, Dose: 20, Verdict: VerdictUngraded, CreatedAt: "2026-07-13T00:00:00Z"},
	}

	idx, err := buildLedgerIndex(home)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	arms := aggregateArms(idx, runs)

	byKind := map[string]ArmMetrics{}
	for _, a := range arms {
		byKind[a.ArmKind] = a
	}

	gate, ok := byKind[ArmGateOnly]
	if !ok {
		t.Fatal("gate_only arm missing from metrics")
	}
	if gate.Thrash.Values[0] != 3 {
		t.Fatalf("gate_only thrash must be exactly 3 denials (not 4 with the allow), got %v", gate.Thrash.Values)
	}
	if gate.Tokens.Values[0] != 150 { // 100 + 50 summed across route rows
		t.Fatalf("gate_only tokens must sum route rows to 150, got %v", gate.Tokens.Values)
	}
	if gate.LatencyMS.Values[0] != 60 { // 40 + 20
		t.Fatalf("gate_only latency must sum to 60, got %v", gate.LatencyMS.Values)
	}
	if gate.SuccessRate != 1.0 {
		t.Fatalf("gate_only success rate must be 1.0, got %v", gate.SuccessRate)
	}
	if !gate.Rows[0].ReceiptFound {
		t.Fatal("gate_only receipt should be found + work-bound in the ledger")
	}

	both := byKind[ArmBoth]
	if both.Thrash.Values[0] != 0 {
		t.Fatalf("both thrash must be 0, got %v", both.Thrash.Values)
	}

	oic := byKind[ArmObligationInContext]
	if oic.SuccessRate != 0.0 {
		t.Fatalf("obligation_in_context success rate must be 0.0 (verdict fail), got %v", oic.SuccessRate)
	}

	// The pending filler run has no work_id: it must not appear as a dispatched arm.
	if _, present := byKind[ArmFiller]; present {
		t.Fatal("a pending (no work_id) filler run must be excluded from dispatched arm metrics")
	}
}

// TestReceiptTraceabilityRejectsCrossWorkCitation proves a graded verdict citing a
// receipt bound to a DIFFERENT work is not treated as found — traceability is real,
// not a mere existence check.
func TestReceiptTraceabilityRejectsCrossWorkCitation(t *testing.T) {
	home := t.TempDir()
	seedReceipt(t, home, "rec_other", "work_other") // receipt binds work_other
	idx, err := buildLedgerIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	// run says work_a but cites a receipt bound to work_other
	m := deriveRunMetric(idx, run("r1", "t1", "a", ArmBoth, 0, "work_a", VerdictPass, "rec_other"))
	if m.ReceiptFound {
		t.Fatal("a receipt bound to another work must not count as found for this run")
	}
}
