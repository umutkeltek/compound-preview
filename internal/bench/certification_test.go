package bench

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func certificationSuite() Suite {
	return Suite{Suite: "certification", Cases: []Case{
		{ID: "in-exact", Kind: KindCapability, Task: "alpha", Expected: []string{"skill:alpha"}, Split: SplitHeldIn, Lane: "exact"},
		{ID: "in-casual", Kind: KindCapability, Task: "beta", Expected: []string{"skill:beta"}, Split: SplitHeldIn, Lane: "casual"},
		{ID: "out-exact", Kind: KindCapability, Task: "alpha", Expected: []string{"skill:alpha"}, Split: SplitHeldOut, Lane: "exact"},
		{ID: "out-casual", Kind: KindCapability, Task: "beta", Expected: []string{"skill:beta"}, Split: SplitHeldOut, Lane: "casual"},
	}}
}

func certificationRun(t *testing.T, suite Suite, alpha, beta bool) Report {
	t.Helper()
	home := t.TempDir()
	triggers := func(enabled bool, task string) []string {
		if enabled {
			return []string{task}
		}
		return []string{"unmatched"}
	}
	seedGraph(t, home, []fixtureNode{
		{"skill:alpha", "skill", "Alpha", triggers(alpha, "alpha"), false},
		{"skill:beta", "skill", "Beta", triggers(beta, "beta"), false},
	})
	r, err := Run(Options{Home: home, Suite: suite, BuildSHA: "build-before", Now: fixedNow()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func cloneCertificationReport(t *testing.T, r Report) Report {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out Report
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCertificationRejectsEmptyAndIncompleteReports(t *testing.T) {
	valid := certificationRun(t, certificationSuite(), true, true)
	if c := Compare(valid, valid); !c.Accept || c.ProofRef == "" {
		t.Fatalf("valid control rejected: %+v", c)
	}
	tests := map[string]func(*Report){
		"empty":                  func(r *Report) { *r = Report{} },
		"empty-name":             func(r *Report) { r.Suite = "" },
		"unknown-schema":         func(r *Report) { r.Schema = "future" },
		"no-cases":               func(r *Report) { r.Cases = nil },
		"count-mismatch":         func(r *Report) { r.CaseCount++ },
		"missing-split":          func(r *Report) { delete(r.Splits, SplitHeldOut) },
		"missing-lane":           func(r *Report) { r.Cases[0].Lane = "" },
		"skipped-case":           func(r *Report) { r.Cases[0].Skipped = true; r.SkippedCount = 1; r.ScoredCount-- },
		"duplicate-case":         func(r *Report) { r.Cases[1].ID = r.Cases[0].ID },
		"invalid-metric":         func(r *Report) { r.Overall.MRR = math.NaN() },
		"inconsistent-aggregate": func(r *Report) { r.Overall.Hit5 = 0 },
		"legacy-missing-digest": func(r *Report) {
			data, _ := json.Marshal(r)
			var object map[string]json.RawMessage
			_ = json.Unmarshal(data, &object)
			delete(object, "input_digest")
			data, _ = json.Marshal(object)
			_ = json.Unmarshal(data, r)
			// Unmarshal into a fresh report so an omitted field cannot retain its old value.
			var legacy Report
			_ = json.Unmarshal(data, &legacy)
			*r = legacy
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := cloneCertificationReport(t, valid)
			mutate(&r)
			if c := Compare(r, r); c.Accept || c.ProofRef != "" {
				t.Fatalf("invalid report certified: %+v", c)
			}
		})
	}
}

func TestCertificationRejectsChangedEvaluationInputs(t *testing.T) {
	before := certificationRun(t, certificationSuite(), true, true)
	changes := map[string]func(*Case){
		"task-only": func(c *Case) { c.Task = "alpha revised" },
		"case-id":   func(c *Case) { c.ID = "replacement" },
		"expected":  func(c *Case) { c.Expected = []string{"skill:beta"} },
		"split":     func(c *Case) { c.Split = SplitHeldOut },
		"lane":      func(c *Case) { c.Lane = "paraphrase" },
		"harness":   func(c *Case) { c.Harness = "codex" },
		"repo-kind": func(c *Case) { c.RepoKind = "service" },
		"topic":     func(c *Case) { c.Topic = "testing" },
		"forbidden": func(c *Case) { c.Forbidden = &ForbiddenTopN{N: 5, Contains: []string{"forbidden"}} },
		"outrank": func(c *Case) {
			c.MustOutrank = []OutrankAssertion{{Winner: "skill:alpha", LoserContains: []string{"forbidden"}}}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			suite := certificationSuite()
			change(&suite.Cases[0])
			after := certificationRun(t, suite, true, true)
			if c := Compare(before, after); c.Accept || c.ProofRef != "" || !c.InputMismatch {
				t.Fatalf("changed input certified: %+v", c)
			}
		})
	}
}

func TestCertificationRejectsLaneRegressionHiddenByAggregate(t *testing.T) {
	suite := certificationSuite()
	before := certificationRun(t, suite, true, false)
	after := certificationRun(t, suite, false, true)
	if !reflect.DeepEqual(before.Splits, after.Splits) {
		t.Fatalf("falsifier must hold split metrics: %v vs %v", before.Splits, after.Splits)
	}
	if c := Compare(before, after); c.Accept || !c.Regressed || c.ProofRef != "" {
		t.Fatalf("lane regression certified: %+v", c)
	}
}

func TestCertificationAcceptsEquivalentInputsAndImprovement(t *testing.T) {
	suite := certificationSuite()
	before := certificationRun(t, suite, false, false)
	after := certificationRun(t, suite, true, true)
	after.BuildSHA = "build-after"
	if before.GraphHash == after.GraphHash {
		t.Fatal("control must change graph revision")
	}
	if c := Compare(before, after); !c.Accept || c.ProofRef == "" {
		t.Fatalf("valid improvement rejected: %+v", c)
	}
	reordered := certificationSuite()
	for i, j := 0, len(reordered.Cases)-1; i < j; i, j = i+1, j-1 {
		reordered.Cases[i], reordered.Cases[j] = reordered.Cases[j], reordered.Cases[i]
	}
	same := certificationRun(t, reordered, true, true)
	if c := Compare(after, same); !c.Accept {
		t.Fatalf("case ordering changed eligibility: %+v", c)
	}
	// Sparse lanes need coverage only where declared, not a cross-product of every split/lane.
	sparse := certificationSuite()
	sparse.Cases = []Case{sparse.Cases[0], sparse.Cases[3]}
	r := certificationRun(t, sparse, true, true)
	if c := Compare(r, r); !c.Accept {
		t.Fatalf("valid sparse lanes rejected: %+v", c)
	}
}

func TestCertificationCanonicalAssertionIdentity(t *testing.T) {
	suite := certificationSuite()
	suite.Cases[0].Expected = []string{"skill:alpha", "skill:beta"}
	suite.Cases[0].Forbidden = &ForbiddenTopN{N: 5, Contains: []string{"zzz", "yyy"}}
	suite.Cases[0].MustOutrank = []OutrankAssertion{
		{Winner: "skill:alpha", LoserContains: []string{"zzz", "yyy"}},
		{Winner: "skill:beta", LoserContains: []string{"xxx"}},
	}
	before := certificationRun(t, suite, true, true)
	// The producer must not mutate the caller's assertions while canonicalizing.
	if suite.Cases[0].Forbidden.Contains[0] != "zzz" {
		t.Fatal("producer mutated suite")
	}
	suite.Cases[0].Expected = []string{"skill:beta", "skill:alpha"}
	suite.Cases[0].Forbidden.Contains = []string{"yyy", "zzz"}
	suite.Cases[0].MustOutrank[0].LoserContains = []string{"yyy", "zzz"}
	suite.Cases[0].MustOutrank[0], suite.Cases[0].MustOutrank[1] = suite.Cases[0].MustOutrank[1], suite.Cases[0].MustOutrank[0]
	after := certificationRun(t, suite, true, true)
	if c := Compare(before, after); !c.Accept || before.InputDigest != after.InputDigest {
		t.Fatalf("equivalent assertion order rejected: %+v", c)
	}
	suite.Cases[0].Forbidden.Contains = append(suite.Cases[0].Forbidden.Contains, "zzz")
	duplicate := certificationRun(t, suite, true, true)
	if c := Compare(before, duplicate); c.Accept || !c.InputMismatch {
		t.Fatalf("changed assertion multiplicity accepted: %+v", c)
	}
}

func TestCertificationRejectsTruncatedDeclaredContract(t *testing.T) {
	before := certificationRun(t, certificationSuite(), true, true)
	before.Cases = []CaseResult{before.Cases[0], before.Cases[2]}
	before.CaseCount, before.ScoredCount = len(before.Cases), len(before.Cases)
	for _, split := range []string{SplitHeldIn, SplitHeldOut} {
		before.Splits[split] = aggregate(before.Cases, split)
	}
	before.Overall = aggregate(before.Cases, "")
	after := cloneCertificationReport(t, before)
	c := Compare(before, after)
	if c.Accept || c.ProofRef != "" || len(c.InputErrors) == 0 {
		t.Fatalf("truncated declared contract certified: %+v", c)
	}
}

func TestCertificationValidatesDeclaredInventory(t *testing.T) {
	valid := certificationRun(t, certificationSuite(), true, true)
	tests := map[string]func(*Report){
		"missing": func(r *Report) { r.Evaluation = nil },
		"schema": func(r *Report) {
			r.Evaluation.Schema = "unknown"
			r.InputDigest = evaluationContractDigest(*r.Evaluation)
		},
		"count": func(r *Report) {
			r.Evaluation.Cases = r.Evaluation.Cases[:1]
			r.InputDigest = evaluationContractDigest(*r.Evaluation)
		},
		"hash": func(r *Report) {
			r.Evaluation.Cases[0].InputHash = "bad"
			r.InputDigest = evaluationContractDigest(*r.Evaluation)
		},
		"duplicate": func(r *Report) {
			r.Evaluation.Cases[1].ID = r.Evaluation.Cases[0].ID
			r.InputDigest = evaluationContractDigest(*r.Evaluation)
		},
		"lane": func(r *Report) {
			r.Evaluation.Cases[0].Lane = "wrong"
			r.InputDigest = evaluationContractDigest(*r.Evaluation)
		},
		"expected": func(r *Report) {
			r.Evaluation.Cases[0].Expected = []string{"skill:wrong"}
			r.InputDigest = evaluationContractDigest(*r.Evaluation)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := cloneCertificationReport(t, valid)
			mutate(&r)
			c := Compare(r, r)
			if c.Accept || c.ProofRef != "" || len(c.InputErrors) == 0 {
				t.Fatalf("invalid declared inventory certified: %+v", c)
			}
		})
	}
}
func TestEvaluationContractKeepsInputTextPrivate(t *testing.T) {
	suite := certificationSuite()
	suite.Cases[0].Task = "private task marker"
	suite.Cases[0].Forbidden = &ForbiddenTopN{Contains: []string{"private assertion marker"}}
	data, err := json.Marshal(evaluationContract(suite))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private task marker") || strings.Contains(string(data), "private assertion marker") {
		t.Fatal("input text leaked into evaluation contract")
	}
}
