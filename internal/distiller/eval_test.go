package distiller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluateCaseFilePassesFixture(t *testing.T) {
	path := filepath.Join("..", "..", "fixtures", "distiller", "llm-offline-eval.json")
	report, err := EvaluateCaseFile(path, t0)
	if err != nil {
		t.Fatalf("evaluate case: %v", err)
	}
	if report.Status != "pass" {
		t.Fatalf("status = %q checks=%+v", report.Status, report.Checks)
	}
	if report.Run.DecisionCount != 4 {
		t.Fatalf("decision_count = %d, want 4", report.Run.DecisionCount)
	}
	if report.Run.CurrentClaims != 0 {
		t.Fatalf("current_claims = %d, want 0", report.Run.CurrentClaims)
	}
	if report.Run.LiveModelCalls != 0 {
		t.Fatalf("live_model_calls = %d, want 0", report.Run.LiveModelCalls)
	}
	if report.Request.PriorCoverage.MethodUpgrades != 1 || report.Request.PriorCoverage.CapabilityObs != 1 {
		t.Fatalf("prior coverage = %+v", report.Request.PriorCoverage)
	}
	if report.Request.PriorSignalKinds[KindMethodUpgrade] != 1 || report.Request.PriorSignalKinds[KindEvalCase] != 1 {
		t.Fatalf("prior signal kinds = %+v", report.Request.PriorSignalKinds)
	}
	for _, want := range []string{"work:work_distill_fixture", "context_pack:ctx_distill_fixture", "receipt:rec_distill_fixture", "raw_trace:trace_fixture"} {
		if !hasString(report.Request.SourceRefs, want) {
			t.Fatalf("source refs missing %q: %v", want, report.Request.SourceRefs)
		}
	}
}

func TestEvaluateCaseFailsWrongExpectation(t *testing.T) {
	path := filepath.Join("..", "..", "fixtures", "distiller", "llm-offline-eval.json")
	ec, err := LoadEvalCase(path)
	if err != nil {
		t.Fatalf("load case: %v", err)
	}
	ec.Expect.Decisions[0].Verdict = Promote
	report, err := EvaluateCase(filepath.Dir(path), ec, t0)
	if err != nil {
		t.Fatalf("evaluate case: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail", report.Status)
	}
	if !checkFailed(report.Checks, "decisions") {
		t.Fatalf("expected decisions check failure, got %+v", report.Checks)
	}
}

func TestEvaluateCaseExpectedExtractorErrors(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{
			name: "promotion basis",
			file: "llm-bad-promotion-basis-eval.json",
			want: "attempted promotion_basis",
		},
		{
			name: "unknown kind",
			file: "llm-bad-unknown-kind-eval.json",
			want: "unknown kind",
		},
		{
			name: "missing confidence",
			file: "llm-bad-missing-confidence-eval.json",
			want: "missing confidence",
		},
		{
			name: "bad confidence",
			file: "llm-bad-confidence-eval.json",
			want: "outside [0,1]",
		},
		{
			name: "thin candidate",
			file: "llm-bad-thin-candidate-eval.json",
			want: "skill requires statement",
		},
		{
			name: "malformed json",
			file: "llm-bad-malformed-eval.json",
			want: "parse llm extraction response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join("..", "..", "fixtures", "distiller", tt.file)
			report, err := EvaluateCaseFile(path, t0)
			if err != nil {
				t.Fatalf("evaluate case: %v", err)
			}
			if report.Status != "pass" {
				t.Fatalf("status = %q checks=%+v", report.Status, report.Checks)
			}
			if !strings.Contains(report.Run.Error, tt.want) {
				t.Fatalf("run error = %q, want containing %q", report.Run.Error, tt.want)
			}
			if report.Run.DecisionCount != 0 {
				t.Fatalf("decision_count = %d, want 0", report.Run.DecisionCount)
			}
			if report.Run.CurrentClaims != 0 {
				t.Fatalf("current_claims = %d, want 0", report.Run.CurrentClaims)
			}
			if report.Run.LiveModelCalls != 0 {
				t.Fatalf("live_model_calls = %d, want 0", report.Run.LiveModelCalls)
			}
			if !checkPassed(report.Checks, "error") {
				t.Fatalf("expected error check pass, got %+v", report.Checks)
			}
		})
	}
}

func TestEvaluateCaseUnexpectedExtractorErrorFailsHard(t *testing.T) {
	path := filepath.Join("..", "..", "fixtures", "distiller", "llm-bad-promotion-basis-eval.json")
	ec, err := LoadEvalCase(path)
	if err != nil {
		t.Fatalf("load case: %v", err)
	}
	ec.Expect.ErrorContains = ""

	report, err := EvaluateCase(filepath.Dir(path), ec, t0)
	if err == nil {
		t.Fatalf("expected hard eval error, got report %+v", report)
	}
	if !strings.Contains(err.Error(), "attempted promotion_basis") {
		t.Fatalf("err = %v, want attempted promotion_basis", err)
	}
	if !checkFailed(report.Checks, "error") {
		t.Fatalf("expected error check failure, got %+v", report.Checks)
	}
}

func TestEvaluateCorpusDirPassesFixtureSuite(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures", "distiller")
	report, err := EvaluateCorpusDir(dir, t0)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	if report.Status != "pass" {
		t.Fatalf("status = %q", report.Status)
	}
	if report.CaseCount != 9 || report.PassCount != 9 || report.FailCount != 0 {
		t.Fatalf("counts = cases:%d pass:%d fail:%d, want 9/9/0", report.CaseCount, report.PassCount, report.FailCount)
	}
	if report.DecisionCount != 5 {
		t.Fatalf("decision_count = %d, want 5", report.DecisionCount)
	}
	if report.CurrentClaims != 0 {
		t.Fatalf("current_claims = %d, want 0", report.CurrentClaims)
	}
	if report.LiveModelCalls != 0 {
		t.Fatalf("live_model_calls = %d, want 0", report.LiveModelCalls)
	}
	if got := report.Reports[0].ID; got != "eval_rejects_llm_bad_confidence" {
		t.Fatalf("first report id = %q, want sorted corpus order", got)
	}
}

func TestRecordEvalProofRefsValidateOnlyRecordedPassingReports(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join("..", "..", "fixtures", "distiller", "llm-offline-eval.json")
	report, err := EvaluateCaseFile(path, t0)
	if err != nil {
		t.Fatalf("evaluate case: %v", err)
	}
	proof, err := RecordEvalReport(home, report)
	if err != nil {
		t.Fatal(err)
	}
	if proof.ProofRef != "distiller_eval:"+report.ID || proof.Path == "" {
		t.Fatalf("proof = %+v", proof)
	}
	if err := ValidateRecordedEvalProofRefs(home, []string{proof.ProofRef}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordedEvalProofRefs(home, []string{"distiller_eval:missing"}); err == nil || !strings.Contains(err.Error(), "not recorded") {
		t.Fatalf("missing proof err = %v", err)
	}

	failing := report
	failing.ID = "failing-eval-proof"
	failing.Status = "fail"
	failingProof, err := RecordEvalReport(home, failing)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordedEvalProofRefs(home, []string{failingProof.ProofRef}); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("failing proof err = %v", err)
	}
}

func TestRecordEvalCorpusProofRef(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join("..", "..", "fixtures", "distiller")
	report, err := EvaluateCorpusDir(dir, t0)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	proof, err := RecordEvalCorpusReport(home, report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(proof.ProofRef, "distiller_eval_corpus:eval_corpus_") || proof.Path == "" {
		t.Fatalf("proof = %+v", proof)
	}
	if err := ValidateRecordedEvalProofRefs(home, []string{proof.ProofRef}); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateCorpusDirReportsFailingCase(t *testing.T) {
	dir := t.TempDir()
	response := `{"schema_version":"compound.distiller.llm_extraction.v0.1","trace_ref":"trace_fail","source_refs":["raw:one"],"candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.91,"evidence_refs":["manual:test"]}]}`
	if err := os.WriteFile(filepath.Join(dir, "response.json"), []byte(response), 0o644); err != nil {
		t.Fatal(err)
	}
	evalCase := `{
  "schema_version": "compound.distiller.eval_case.v0.1",
  "id": "eval_expected_failure",
  "request": {"trace_ref": "trace_fail"},
  "response": "response.json",
  "expect": {
    "decisions": [{"kind":"memory","verdict":"promote"}],
    "current_claims": 0,
    "live_model_calls": 0
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "case-eval.json"), []byte(evalCase), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := EvaluateCorpusDir(dir, t0)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail", report.Status)
	}
	if report.CaseCount != 1 || report.PassCount != 0 || report.FailCount != 1 {
		t.Fatalf("counts = cases:%d pass:%d fail:%d, want 1/0/1", report.CaseCount, report.PassCount, report.FailCount)
	}
	if !checkFailed(report.Reports[0].Checks, "decisions") {
		t.Fatalf("expected decisions check failure, got %+v", report.Reports[0].Checks)
	}
}

func TestEvaluateCorpusDirRejectsEmptyDirectory(t *testing.T) {
	_, err := EvaluateCorpusDir(t.TempDir(), t0)
	if err == nil || !strings.Contains(err.Error(), "contains no eval case") {
		t.Fatalf("err = %v, want contains no eval case", err)
	}
}

func TestLoadEvalCaseRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	data, err := json.Marshal(EvalCase{SchemaVersion: EvalCaseSchemaVersion, ID: "bad"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEvalCase(path); err == nil {
		t.Fatal("expected malformed eval case to fail")
	}
}

func checkPassed(checks []EvalCheck, name string) bool {
	for _, check := range checks {
		if check.Name == name && check.Status == "pass" {
			return true
		}
	}
	return false
}

func checkFailed(checks []EvalCheck, name string) bool {
	for _, check := range checks {
		if check.Name == name && check.Status == "fail" {
			return true
		}
	}
	return false
}
