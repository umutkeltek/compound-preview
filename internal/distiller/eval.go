package distiller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

const EvalCaseSchemaVersion = "compound.distiller.eval_case.v0.1"
const EvalCorpusSchemaVersion = "compound.distiller.eval_corpus.v0.1"

type EvalCase struct {
	SchemaVersion string             `json:"schema_version"`
	ID            string             `json:"id"`
	SeedTraces    []string           `json:"seed_traces,omitempty"`
	Request       EvalRequestSources `json:"request"`
	Response      string             `json:"response"`
	Expect        EvalExpectations   `json:"expect"`
}

type EvalRequestSources struct {
	TraceRef       string   `json:"trace_ref"`
	Raw            string   `json:"raw,omitempty"`
	ContextPack    string   `json:"context_pack,omitempty"`
	Work           string   `json:"work,omitempty"`
	Receipt        string   `json:"receipt,omitempty"`
	ExtraEnvelopes []string `json:"extra_envelopes,omitempty"`
}

type EvalExpectations struct {
	Decisions      []ExpectedDecision `json:"decisions"`
	CurrentClaims  *int               `json:"current_claims,omitempty"`
	ClaimsLedger   *bool              `json:"claims_ledger_exists,omitempty"`
	SourceRefs     []string           `json:"source_refs,omitempty"`
	PriorCoverage  map[string]int     `json:"prior_coverage,omitempty"`
	PriorSignals   map[string]int     `json:"prior_signals,omitempty"`
	LiveModelCalls *int               `json:"live_model_calls,omitempty"`
	ErrorContains  string             `json:"error_contains,omitempty"`
}

type ExpectedDecision struct {
	Kind          string `json:"kind"`
	Verdict       string `json:"verdict"`
	Relation      string `json:"relation,omitempty"`
	BasisContains string `json:"basis_contains,omitempty"`
}

type EvalReport struct {
	SchemaVersion string        `json:"schema_version"`
	ID            string        `json:"id"`
	Status        string        `json:"status"`
	TraceRef      string        `json:"trace_ref"`
	Checks        []EvalCheck   `json:"checks"`
	Request       EvalReqReport `json:"request"`
	Run           EvalRunReport `json:"run"`
}

type EvalCorpusReport struct {
	SchemaVersion  string       `json:"schema_version"`
	ID             string       `json:"id"`
	Dir            string       `json:"dir"`
	Status         string       `json:"status"`
	CaseCount      int          `json:"case_count"`
	PassCount      int          `json:"pass_count"`
	FailCount      int          `json:"fail_count"`
	DecisionCount  int          `json:"decision_count"`
	CurrentClaims  int          `json:"current_claims"`
	LiveModelCalls int          `json:"live_model_calls"`
	Reports        []EvalReport `json:"reports"`
}

type EvalReqReport struct {
	PriorCoverage    PriorCoverage  `json:"prior_coverage"`
	PriorSignalKinds map[string]int `json:"prior_signal_kinds,omitempty"`
	SourceRefs       []string       `json:"source_refs"`
}

type EvalRunReport struct {
	DecisionCount      int                    `json:"decision_count"`
	CurrentClaims      int                    `json:"current_claims"`
	ClaimsLedgerExists bool                   `json:"claims_ledger_exists"`
	LiveModelCalls     int                    `json:"live_model_calls"`
	Error              string                 `json:"error,omitempty"`
	Decisions          []AdjudicationDecision `json:"decisions"`
}

type EvalCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func EvaluateCaseFile(path string, now time.Time) (EvalReport, error) {
	return EvaluateCaseFileInRoot(path, "", now)
}

func EvaluateCaseFileInRoot(path, evalRoot string, now time.Time) (EvalReport, error) {
	ec, err := LoadEvalCase(path)
	if err != nil {
		return EvalReport{}, err
	}
	base := filepath.Dir(path)
	return EvaluateCaseInRoot(base, ec, evalRoot, now)
}

func EvaluateCorpusDir(dir string, now time.Time) (EvalCorpusReport, error) {
	return EvaluateCorpusDirInRoot(dir, "", now)
}

func EvaluateCorpusDirInRoot(dir, evalRoot string, now time.Time) (EvalCorpusReport, error) {
	report := EvalCorpusReport{
		SchemaVersion: EvalCorpusSchemaVersion,
		Dir:           dir,
		Status:        "fail",
	}
	info, err := os.Stat(dir)
	if err != nil {
		return report, err
	}
	if !info.IsDir() {
		return report, fmt.Errorf("%s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return report, err
	}
	var cases []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "-eval.json") {
			continue
		}
		cases = append(cases, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(cases)
	if len(cases) == 0 {
		return report, fmt.Errorf("%s contains no eval case json files", dir)
	}
	for _, path := range cases {
		caseReport, err := EvaluateCaseFileInRoot(path, evalRoot, now)
		if err != nil {
			return report, fmt.Errorf("eval case %s: %w", path, err)
		}
		report.Reports = append(report.Reports, caseReport)
		report.CaseCount++
		report.DecisionCount += caseReport.Run.DecisionCount
		report.CurrentClaims += caseReport.Run.CurrentClaims
		report.LiveModelCalls += caseReport.Run.LiveModelCalls
		if caseReport.Status == "pass" {
			report.PassCount++
		} else {
			report.FailCount++
		}
	}
	if report.FailCount == 0 {
		report.Status = "pass"
	}
	report.ID = evalCorpusReportID(report)
	return report, nil
}

type EvalProofRecord struct {
	ProofRef string
	Path     string
}

func RecordEvalReport(homePath string, report EvalReport) (EvalProofRecord, error) {
	if strings.TrimSpace(homePath) == "" {
		return EvalProofRecord{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(report.ID) == "" {
		return EvalProofRecord{}, fmt.Errorf("eval report id required")
	}
	ref := "distiller_eval:" + report.ID
	path, err := recordEvalProofJSON(homePath, ref, report)
	if err != nil {
		return EvalProofRecord{}, err
	}
	return EvalProofRecord{ProofRef: ref, Path: path}, nil
}

func RecordEvalCorpusReport(homePath string, report EvalCorpusReport) (EvalProofRecord, error) {
	if strings.TrimSpace(homePath) == "" {
		return EvalProofRecord{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(report.ID) == "" {
		report.ID = evalCorpusReportID(report)
	}
	ref := "distiller_eval_corpus:" + report.ID
	path, err := recordEvalProofJSON(homePath, ref, report)
	if err != nil {
		return EvalProofRecord{}, err
	}
	return EvalProofRecord{ProofRef: ref, Path: path}, nil
}

func ValidateRecordedEvalProofRefs(homePath string, refs []string) error {
	refs = compactEvalProofRefs(refs)
	if strings.TrimSpace(homePath) == "" {
		return fmt.Errorf("COMPOUND_HOME required")
	}
	if len(refs) == 0 {
		return fmt.Errorf("eval_proof_refs required")
	}
	for _, ref := range refs {
		if err := validateRecordedEvalProofRef(homePath, ref); err != nil {
			return err
		}
	}
	return nil
}

func recordEvalProofJSON(homePath, proofRef string, value any) (string, error) {
	return reviewartifact.WriteJSON(homePath, "distiller-eval/proofs", safeEvalProofRef(proofRef)+".json", value, "  ")
}

func validateRecordedEvalProofRef(homePath, ref string) error {
	path := filepath.Join(homePath, "reviews", "distiller-eval", "proofs", safeEvalProofRef(ref)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("eval proof ref %q not recorded", ref)
		}
		return err
	}
	switch {
	case strings.HasPrefix(ref, "distiller_eval:"):
		var report EvalReport
		if err := json.Unmarshal(data, &report); err != nil {
			return fmt.Errorf("parse eval proof %q: %w", ref, err)
		}
		wantID := strings.TrimPrefix(ref, "distiller_eval:")
		if report.ID != wantID {
			return fmt.Errorf("eval proof ref %q id mismatch: %q", ref, report.ID)
		}
		if report.Status != "pass" {
			return fmt.Errorf("eval proof ref %q status=%q, want pass", ref, report.Status)
		}
		if report.Run.LiveModelCalls != 0 {
			return fmt.Errorf("eval proof ref %q used live_model_calls=%d", ref, report.Run.LiveModelCalls)
		}
	case strings.HasPrefix(ref, "distiller_eval_corpus:"):
		var report EvalCorpusReport
		if err := json.Unmarshal(data, &report); err != nil {
			return fmt.Errorf("parse eval corpus proof %q: %w", ref, err)
		}
		wantID := strings.TrimPrefix(ref, "distiller_eval_corpus:")
		if report.ID != wantID {
			return fmt.Errorf("eval corpus proof ref %q id mismatch: %q", ref, report.ID)
		}
		if report.Status != "pass" || report.FailCount != 0 || report.PassCount != report.CaseCount {
			return fmt.Errorf("eval corpus proof ref %q not passing: status=%q pass=%d fail=%d cases=%d", ref, report.Status, report.PassCount, report.FailCount, report.CaseCount)
		}
		if report.LiveModelCalls != 0 {
			return fmt.Errorf("eval corpus proof ref %q used live_model_calls=%d", ref, report.LiveModelCalls)
		}
	default:
		return fmt.Errorf("eval proof ref %q must start with distiller_eval: or distiller_eval_corpus:", ref)
	}
	return nil
}

func evalCorpusReportID(report EvalCorpusReport) string {
	var parts []string
	parts = append(parts, report.SchemaVersion, report.Status, fmt.Sprintf("cases=%d/pass=%d/fail=%d/decisions=%d/current=%d/live=%d", report.CaseCount, report.PassCount, report.FailCount, report.DecisionCount, report.CurrentClaims, report.LiveModelCalls))
	for _, r := range report.Reports {
		parts = append(parts, r.ID, r.Status, fmt.Sprintf("decisions=%d/current=%d/live=%d/error=%s", r.Run.DecisionCount, r.Run.CurrentClaims, r.Run.LiveModelCalls, r.Run.Error))
		for _, check := range r.Checks {
			parts = append(parts, check.Name+"="+check.Status+":"+check.Detail)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "eval_corpus_" + hex.EncodeToString(sum[:])[:12]
}

func safeEvalProofRef(ref string) string {
	ref = strings.TrimSpace(ref)
	var b strings.Builder
	lastDash := false
	for _, r := range ref {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		sum := sha256.Sum256([]byte(ref))
		return hex.EncodeToString(sum[:])[:12]
	}
	if len(out) > 96 {
		sum := sha256.Sum256([]byte(ref))
		out = strings.Trim(out[:83], "-") + "-" + hex.EncodeToString(sum[:])[:12]
	}
	return out
}

func compactEvalProofRefs(refs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

func LoadEvalCase(path string) (EvalCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return EvalCase{}, err
	}
	var ec EvalCase
	if err := json.Unmarshal(data, &ec); err != nil {
		return EvalCase{}, fmt.Errorf("parse eval case %s: %w", path, err)
	}
	if ec.SchemaVersion != EvalCaseSchemaVersion {
		return EvalCase{}, fmt.Errorf("eval case schema_version=%q, want %q", ec.SchemaVersion, EvalCaseSchemaVersion)
	}
	if strings.TrimSpace(ec.ID) == "" {
		return EvalCase{}, fmt.Errorf("eval case id required")
	}
	if strings.TrimSpace(ec.Request.TraceRef) == "" {
		return EvalCase{}, fmt.Errorf("eval request trace_ref required")
	}
	if strings.TrimSpace(ec.Response) == "" {
		return EvalCase{}, fmt.Errorf("eval response path required")
	}
	return ec, nil
}

func EvaluateCase(base string, ec EvalCase, now time.Time) (EvalReport, error) {
	return EvaluateCaseInRoot(base, ec, "", now)
}

func EvaluateCaseInRoot(base string, ec EvalCase, evalRoot string, now time.Time) (EvalReport, error) {
	report := EvalReport{SchemaVersion: EvalCaseSchemaVersion, ID: ec.ID, TraceRef: ec.Request.TraceRef}
	evalHome, err := newEvalHome(evalRoot)
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(evalHome)

	store, err := mem.Open(evalHome)
	if err != nil {
		return report, err
	}
	for _, seed := range ec.SeedTraces {
		data, err := os.ReadFile(resolveEvalPath(base, seed))
		if err != nil {
			return report, fmt.Errorf("read seed trace %s: %w", seed, err)
		}
		if _, err := Run(evalHome, store, data, DefaultPolicy(), ReferenceExtractor{}, now); err != nil {
			return report, fmt.Errorf("seed trace %s: %w", seed, err)
		}
	}
	prior, err := LoadPriorStateFromHome(evalHome, store, DefaultPolicy())
	if err != nil {
		return report, err
	}
	req, err := buildEvalRequest(base, ec, prior)
	if err != nil {
		return report, err
	}
	response, err := os.ReadFile(resolveEvalPath(base, ec.Response))
	if err != nil {
		return report, fmt.Errorf("read response %s: %w", ec.Response, err)
	}
	run, err := Run(evalHome, store, response, DefaultPolicy(), NewGroundedLLMExtractor(req), now)
	if err != nil {
		current, currentErr := store.AllCurrent()
		if currentErr != nil {
			return report, currentErr
		}
		report.Request = EvalReqReport{PriorCoverage: req.PriorCoverage, PriorSignalKinds: priorSignalKinds(req.PriorSignals), SourceRefs: req.SourceRefs}
		report.Run = EvalRunReport{
			CurrentClaims:      len(current),
			ClaimsLedgerExists: claimsLedgerExists(evalHome),
			LiveModelCalls:     0,
			Error:              err.Error(),
		}
		report.Checks = evaluateExpectations(ec.Expect, req, report.Run)
		finalizeEvalReport(&report)
		if strings.TrimSpace(ec.Expect.ErrorContains) == "" {
			return report, fmt.Errorf("run response %s: %w", ec.Response, err)
		}
		return report, nil
	}
	current, err := store.AllCurrent()
	if err != nil {
		return report, err
	}
	report.Request = EvalReqReport{PriorCoverage: req.PriorCoverage, PriorSignalKinds: priorSignalKinds(req.PriorSignals), SourceRefs: req.SourceRefs}
	report.Run = EvalRunReport{
		DecisionCount:      len(run.Decisions),
		CurrentClaims:      len(current),
		ClaimsLedgerExists: claimsLedgerExists(evalHome),
		LiveModelCalls:     0,
		Decisions:          run.Decisions,
	}
	report.Checks = evaluateExpectations(ec.Expect, req, report.Run)
	finalizeEvalReport(&report)
	return report, nil
}

func finalizeEvalReport(report *EvalReport) {
	report.Status = "pass"
	for _, check := range report.Checks {
		if check.Status != "pass" {
			report.Status = "fail"
			break
		}
	}
}

func newEvalHome(evalRoot string) (string, error) {
	if strings.TrimSpace(evalRoot) == "" {
		return os.MkdirTemp("", "compound-distiller-eval-*")
	}
	parent := filepath.Join(evalRoot, "tmp", "distiller-evals")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, "eval-*")
}

func claimsLedgerExists(homePath string) bool {
	_, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl"))
	return err == nil
}

func buildEvalRequest(base string, ec EvalCase, prior PriorState) (LLMExtractionRequest, error) {
	src := LLMExtractionSources{}
	if ec.Request.Raw != "" {
		data, err := os.ReadFile(resolveEvalPath(base, ec.Request.Raw))
		if err != nil {
			return LLMExtractionRequest{}, fmt.Errorf("read raw trace %s: %w", ec.Request.Raw, err)
		}
		src.RawTrace = data
	}
	if ec.Request.ContextPack != "" {
		env, err := readEvalEnvelope(resolveEvalPath(base, ec.Request.ContextPack))
		if err != nil {
			return LLMExtractionRequest{}, err
		}
		ctx, ok := env.(*envelope.ContextPack)
		if !ok {
			return LLMExtractionRequest{}, fmt.Errorf("%s is %s, want context_pack", ec.Request.ContextPack, env.Kind())
		}
		src.ContextPack = ctx
	}
	if ec.Request.Work != "" {
		env, err := readEvalEnvelope(resolveEvalPath(base, ec.Request.Work))
		if err != nil {
			return LLMExtractionRequest{}, err
		}
		w, ok := env.(*envelope.WorkEnvelope)
		if !ok {
			return LLMExtractionRequest{}, fmt.Errorf("%s is %s, want work", ec.Request.Work, env.Kind())
		}
		src.Work = w
	}
	if ec.Request.Receipt != "" {
		env, err := readEvalEnvelope(resolveEvalPath(base, ec.Request.Receipt))
		if err != nil {
			return LLMExtractionRequest{}, err
		}
		rec, ok := env.(*envelope.Receipt)
		if !ok {
			return LLMExtractionRequest{}, fmt.Errorf("%s is %s, want receipt", ec.Request.Receipt, env.Kind())
		}
		src.Receipt = rec
	}
	for _, extra := range ec.Request.ExtraEnvelopes {
		env, err := readEvalEnvelope(resolveEvalPath(base, extra))
		if err != nil {
			return LLMExtractionRequest{}, err
		}
		src.Extra = append(src.Extra, env)
	}
	return BuildLLMExtractionRequestFromSources(ec.Request.TraceRef, src, DefaultPolicy(), prior)
}

func readEvalEnvelope(path string) (envelope.Envelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	env := envelope.New(probe.SchemaVersion)
	if env == nil {
		return nil, fmt.Errorf("%s unknown schema_version %q", path, probe.SchemaVersion)
	}
	if err := json.Unmarshal(data, env); err != nil {
		return nil, err
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	return env, nil
}

func evaluateExpectations(expect EvalExpectations, req LLMExtractionRequest, run EvalRunReport) []EvalCheck {
	checks := []EvalCheck{}
	checks = append(checks, checkSourceRefs(expect.SourceRefs, req.SourceRefs))
	checks = append(checks, checkPriorCoverage(expect.PriorCoverage, req.PriorCoverage)...)
	checks = append(checks, checkPriorSignalKinds(expect.PriorSignals, priorSignalKinds(req.PriorSignals))...)
	checks = append(checks, checkDecisions(expect.Decisions, run.Decisions))
	if expect.CurrentClaims != nil {
		checks = append(checks, checkInt("current_claims", run.CurrentClaims, *expect.CurrentClaims))
	}
	if expect.ClaimsLedger != nil {
		checks = append(checks, checkBool("claims_ledger_exists", run.ClaimsLedgerExists, *expect.ClaimsLedger))
	}
	if expect.LiveModelCalls != nil {
		checks = append(checks, checkInt("live_model_calls", run.LiveModelCalls, *expect.LiveModelCalls))
	}
	if strings.TrimSpace(expect.ErrorContains) != "" || strings.TrimSpace(run.Error) != "" {
		checks = append(checks, checkErrorContains(expect.ErrorContains, run.Error))
	}
	return checks
}

func checkErrorContains(want, got string) EvalCheck {
	want = strings.TrimSpace(want)
	got = strings.TrimSpace(got)
	if want == "" && got != "" {
		return EvalCheck{Name: "error", Status: "fail", Detail: "unexpected error: " + got}
	}
	if want != "" && got == "" {
		return EvalCheck{Name: "error", Status: "fail", Detail: "missing error containing " + want}
	}
	if want != "" && !strings.Contains(got, want) {
		return EvalCheck{Name: "error", Status: "fail", Detail: fmt.Sprintf("%q missing %q", got, want)}
	}
	return EvalCheck{Name: "error", Status: "pass"}
}

func checkSourceRefs(want, got []string) EvalCheck {
	missing := missingStrings(want, got)
	if len(missing) > 0 {
		return EvalCheck{Name: "source_refs", Status: "fail", Detail: "missing " + strings.Join(missing, ",")}
	}
	return EvalCheck{Name: "source_refs", Status: "pass"}
}

func checkPriorCoverage(want map[string]int, got PriorCoverage) []EvalCheck {
	if len(want) == 0 {
		return nil
	}
	fields := priorCoverageFields(got)
	checks := make([]EvalCheck, 0, len(want))
	for field, expected := range want {
		actual, ok := fields[field]
		if !ok {
			checks = append(checks, EvalCheck{Name: "prior_coverage." + field, Status: "fail", Detail: "unknown field"})
			continue
		}
		checks = append(checks, checkInt("prior_coverage."+field, actual, expected))
	}
	return checks
}

// priorCoverageFields projects a PriorCoverage into a field-name -> value map
// keyed by the struct's own json tags. This makes the PriorCoverage struct the
// single source of the coverage-axis vocabulary: the struct, Coverage(), the
// eval check, and an eval case's prior_coverage keys all agree by construction.
// Every field is checkable (no hand-maintained subset), and a typo'd axis name
// is reported as an unknown field instead of silently passing.
func priorCoverageFields(c PriorCoverage) map[string]int {
	data, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	var fields map[string]int
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil
	}
	return fields
}

func priorSignalKinds(signals []LLMPriorSignal) map[string]int {
	if len(signals) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, signal := range signals {
		kind := strings.TrimSpace(signal.Kind)
		if kind == "" {
			continue
		}
		counts[kind]++
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

func checkPriorSignalKinds(want, got map[string]int) []EvalCheck {
	if len(want) == 0 {
		return nil
	}
	checks := make([]EvalCheck, 0, len(want))
	for field, expected := range want {
		checks = append(checks, checkInt("prior_signals."+field, got[field], expected))
	}
	return checks
}

func checkDecisions(want []ExpectedDecision, got []AdjudicationDecision) EvalCheck {
	if len(want) != len(got) {
		return EvalCheck{Name: "decisions", Status: "fail", Detail: fmt.Sprintf("got %d, want %d", len(got), len(want))}
	}
	for i := range want {
		if want[i].Kind != "" && got[i].Kind != want[i].Kind {
			return EvalCheck{Name: "decisions", Status: "fail", Detail: fmt.Sprintf("[%d].kind=%q want %q", i, got[i].Kind, want[i].Kind)}
		}
		if want[i].Verdict != "" && got[i].Verdict != want[i].Verdict {
			return EvalCheck{Name: "decisions", Status: "fail", Detail: fmt.Sprintf("[%d].verdict=%q want %q", i, got[i].Verdict, want[i].Verdict)}
		}
		if want[i].Relation != "" && got[i].Relation != want[i].Relation {
			return EvalCheck{Name: "decisions", Status: "fail", Detail: fmt.Sprintf("[%d].relation=%q want %q", i, got[i].Relation, want[i].Relation)}
		}
		if want[i].BasisContains != "" && !strings.Contains(got[i].Basis, want[i].BasisContains) {
			return EvalCheck{Name: "decisions", Status: "fail", Detail: fmt.Sprintf("[%d].basis=%q missing %q", i, got[i].Basis, want[i].BasisContains)}
		}
	}
	return EvalCheck{Name: "decisions", Status: "pass"}
}

func checkInt(name string, got, want int) EvalCheck {
	if got != want {
		return EvalCheck{Name: name, Status: "fail", Detail: fmt.Sprintf("got %d, want %d", got, want)}
	}
	return EvalCheck{Name: name, Status: "pass"}
}

func checkBool(name string, got, want bool) EvalCheck {
	if got != want {
		return EvalCheck{Name: name, Status: "fail", Detail: fmt.Sprintf("got %t, want %t", got, want)}
	}
	return EvalCheck{Name: name, Status: "pass"}
}

func missingStrings(want, got []string) []string {
	var missing []string
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	return missing
}

func resolveEvalPath(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Clean(filepath.Join(base, p))
}
