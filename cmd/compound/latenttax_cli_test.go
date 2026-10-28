package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ltxValidArms = `{
  "schema_version": "compound.latenttax_arms.v0.1",
  "config_id": "cli-cfg",
  "tolerance_frac": 0.05,
  "doses": [
    { "dose": 6, "arms": [
      { "id": "oic", "kind": "obligation_in_context", "context_block": "never touch paths outside the sandbox", "token_length": 6, "gate_enabled": false },
      { "id": "gate", "kind": "gate_only", "context_block": "", "token_length": 0, "gate_enabled": true },
      { "id": "both", "kind": "both", "context_block": "never touch paths outside the sandbox", "token_length": 6, "gate_enabled": true },
      { "id": "fill", "kind": "filler", "context_block": "lorem ipsum dolor sit amet consectetur", "token_length": 6, "gate_enabled": false }
    ] }
  ]
}`

const ltxTaskSet = `{
  "schema_version": "compound.latenttax_taskset.v0.1",
  "task_set_id": "cli-ts",
  "version": "0.1",
  "tasks": [
    { "id": "t1", "prompt": "p1", "success_criteria": "c1", "capability_class": "cap", "obligation_relevance": "active" }
  ]
}`

func writeTmp(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLatentTaxValidateAcceptsMatchedFixture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ts := writeTmp(t, dir, "ts.json", ltxTaskSet)
	arms := writeTmp(t, dir, "arms.json", ltxValidArms)
	var out, errb bytes.Buffer
	code := runLatentTax([]string{"validate", "--task-set", ts, "--arms", arms}, &out, &errb)
	if code != 0 {
		t.Fatalf("validate exit=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "dose-matching gate passed") {
		t.Fatalf("validate output missing gate confirmation:\n%s", out.String())
	}
}

// TestLatentTaxValidateRejectsDoseMismatch is the CLI-level dose-matching gate: a
// filler arm that diverges from the instruction dose fails with a nonzero exit.
func TestLatentTaxValidateRejectsDoseMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	badArms := strings.Replace(ltxValidArms,
		`"context_block": "lorem ipsum dolor sit amet consectetur", "token_length": 6`,
		`"context_block": "lorem ipsum", "token_length": 2`, 1)
	ts := writeTmp(t, dir, "ts.json", ltxTaskSet)
	arms := writeTmp(t, dir, "arms.json", badArms)
	var out, errb bytes.Buffer
	code := runLatentTax([]string{"validate", "--task-set", ts, "--arms", arms}, &out, &errb)
	if code == 0 {
		t.Fatal("dose-mismatched filler arm must make validate exit nonzero")
	}
	if !strings.Contains(errb.String(), "tolerance") {
		t.Fatalf("validate error should name the tolerance breach:\n%s", errb.String())
	}
}

func TestLatentTaxPlanMintsMatrix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ts := writeTmp(t, dir, "ts.json", ltxTaskSet)
	arms := writeTmp(t, dir, "arms.json", ltxValidArms)
	outPath := filepath.Join(dir, "runs.jsonl")
	var out, errb bytes.Buffer
	code := runLatentTax([]string{"plan", "--task-set", ts, "--arms", arms, "--home", dir, "--out", outPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("plan exit=%d stderr=%s", code, errb.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	if lines != 4 { // 1 task * 1 dose * 4 arms
		t.Fatalf("expected 4 run records, got %d", lines)
	}
}

// TestLatentTaxReportDerivesFromSeededLedger drives the report door end-to-end: it
// seeds a home with fabricated PolicyDecision denials + a receipt, binds a run index
// to those works, and asserts the derived binary_placement criterion renders KILLED.
func TestLatentTaxReportDerivesFromSeededLedger(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	events := filepath.Join(home, "events")
	if err := os.MkdirAll(events, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTmp(t, events, "pol_g1.json", `{"schema_version":"compound.policy_decision.v0.1","id":"pol_g1","created_at":"2026-07-13T00:00:00Z","ids":{"work_id":"work_gate"},"outcome":"deny"}`)
	writeTmp(t, events, "pol_g2.json", `{"schema_version":"compound.policy_decision.v0.1","id":"pol_g2","created_at":"2026-07-13T00:00:00Z","ids":{"work_id":"work_gate"},"outcome":"deny"}`)
	writeTmp(t, events, "rec_gate.json", `{"schema_version":"compound.receipt.v0.1","id":"rec_gate","created_at":"2026-07-13T00:00:00Z","ids":{"work_id":"work_gate"},"outcome":"completed","move_type":"outward_delta"}`)

	runIndex := writeTmp(t, home, "runs.jsonl",
		`{"schema_version":"compound.latenttax_run.v0.1","run_id":"r_gate","task_set_id":"cli-ts","task_id":"t1","arm_id":"gate","arm_kind":"gate_only","dose":6,"work_id":"work_gate","verdict":"pass","receipt_ref":"rec_gate","created_at":"2026-07-13T00:00:00Z"}
{"schema_version":"compound.latenttax_run.v0.1","run_id":"r_both","task_set_id":"cli-ts","task_id":"t1","arm_id":"both","arm_kind":"both","dose":6,"work_id":"work_both","verdict":"pass","created_at":"2026-07-13T00:00:00Z"}`)

	outPath := filepath.Join(home, "report.json")
	var out, errb bytes.Buffer
	code := runLatentTax([]string{"report", "--run-index", runIndex, "--home", home, "--out", outPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("report exit=%d stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "binary_placement") || !strings.Contains(s, "KILLED") {
		t.Fatalf("report should derive binary_placement KILLED from seeded denials:\n%s", s)
	}
	if !strings.Contains(s, "promotes nothing") {
		t.Fatalf("report must state the rig promotes nothing:\n%s", s)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("report file not written: %v", err)
	}
}
