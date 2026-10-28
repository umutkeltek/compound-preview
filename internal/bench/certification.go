package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

const evaluationContractSchema = "compound.bench_evaluation_contract.v0.1"

// EvaluationContract preserves the complete declared population independently of
// the scored rows. Input hashes bind private questions and assertions without
// copying their text into reports. This is identity, not authentication.
type EvaluationContract struct {
	Schema string                   `json:"schema"`
	Cases  []EvaluationCaseContract `json:"cases"`
}

type EvaluationCaseContract struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Lane      string   `json:"lane"`
	Split     string   `json:"split"`
	Expected  []string `json:"expected"`
	InputHash string   `json:"input_hash"`
}

func evaluationContract(s Suite) EvaluationContract {
	cases := append([]Case(nil), s.Cases...)
	for i := range cases {
		c := &cases[i]
		c.Expected = sortedCopy(c.Expected)
		if c.Forbidden != nil {
			f := *c.Forbidden
			f.Contains = sortedCopy(f.Contains)
			c.Forbidden = &f
		}
		c.MustOutrank = append([]OutrankAssertion(nil), c.MustOutrank...)
		for j := range c.MustOutrank {
			c.MustOutrank[j].LoserContains = sortedCopy(c.MustOutrank[j].LoserContains)
		}
		sort.Slice(c.MustOutrank, func(a, b int) bool {
			x, _ := json.Marshal(c.MustOutrank[a])
			y, _ := json.Marshal(c.MustOutrank[b])
			return string(x) < string(y)
		})
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	out := EvaluationContract{Schema: evaluationContractSchema, Cases: make([]EvaluationCaseContract, 0, len(cases))}
	for _, c := range cases {
		data, _ := json.Marshal(c) // Case contains only JSON-safe scalar/list fields.
		sum := sha256.Sum256(append([]byte("compound.bench_case.v1\n"), data...))
		out.Cases = append(out.Cases, EvaluationCaseContract{
			ID: c.ID, Kind: c.Kind, Lane: c.Lane, Split: c.Split,
			Expected: c.Expected, InputHash: hex.EncodeToString(sum[:]),
		})
	}
	return out
}

func evaluationContractDigest(c EvaluationContract) string {
	c.Cases = append([]EvaluationCaseContract(nil), c.Cases...)
	for i := range c.Cases {
		c.Cases[i].Expected = sortedCopy(c.Cases[i].Expected)
	}
	sort.Slice(c.Cases, func(i, j int) bool { return c.Cases[i].ID < c.Cases[j].ID })
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(append([]byte("compound.bench_inputs.v2\n"), data...))
	return hex.EncodeToString(sum[:])
}

func evaluationInputDigest(s Suite) string {
	return evaluationContractDigest(evaluationContract(s))
}

// validateEvaluationContract checks declared required coverage even when both
// supplied reports have been shortened in the same way.
func validateEvaluationContract(r Report) error {
	if r.Evaluation == nil || r.Evaluation.Schema != evaluationContractSchema {
		return fmt.Errorf("missing or unsupported evaluation contract")
	}
	if len(r.Evaluation.Cases) != r.CaseCount {
		return fmt.Errorf("declared evaluation case count disagrees with report coverage")
	}
	if evaluationContractDigest(*r.Evaluation) != r.InputDigest {
		return fmt.Errorf("evaluation contract disagrees with input digest")
	}
	actual := make(map[string]CaseResult, len(r.Cases))
	for _, c := range r.Cases {
		actual[c.ID] = c
	}
	seen := make(map[string]bool, len(r.Evaluation.Cases))
	for _, required := range r.Evaluation.Cases {
		if strings.TrimSpace(required.ID) == "" || seen[required.ID] {
			return fmt.Errorf("evaluation contract has missing or duplicate case identity")
		}
		seen[required.ID] = true
		hash, err := hex.DecodeString(required.InputHash)
		if err != nil || len(hash) != sha256.Size || strings.ToLower(required.InputHash) != required.InputHash {
			return fmt.Errorf("case %q has invalid input identity", required.ID)
		}
		c, ok := actual[required.ID]
		if !ok || c.Kind != required.Kind || c.Split != required.Split || c.Lane != required.Lane {
			return fmt.Errorf("case %q does not match declared kind, split or lane", required.ID)
		}
		want := sortedCopy(required.Expected)
		got := make([]string, len(c.Expected))
		for i, expected := range c.Expected {
			got[i] = expected.ID
		}
		sort.Strings(got)
		if len(want) == 0 || len(want) != len(got) {
			return fmt.Errorf("case %q does not cover declared expected outcomes", required.ID)
		}
		for i := range want {
			if want[i] != got[i] {
				return fmt.Errorf("case %q expected outcomes differ from evaluation contract", required.ID)
			}
		}
	}
	return nil
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// certificationError checks the evidence coverage, not report authenticity.
// Legacy reports remain readable; missing identity or coverage cannot certify.
func certificationError(r Report) error {
	if r.Schema != SchemaVersion {
		return fmt.Errorf("unsupported or missing report schema")
	}
	if strings.TrimSpace(r.Suite) == "" {
		return fmt.Errorf("suite name is missing")
	}
	digest, err := hex.DecodeString(r.InputDigest)
	if err != nil || len(digest) != sha256.Size || r.InputDigest != strings.ToLower(r.InputDigest) {
		return fmt.Errorf("evaluation input digest is missing or invalid")
	}
	if r.CaseCount <= 0 || r.CaseCount != len(r.Cases) || r.ScoredCount != r.CaseCount || r.SkippedCount != 0 {
		return fmt.Errorf("complete unskipped case coverage is required")
	}
	seen := map[string]bool{}
	for _, c := range r.Cases {
		if strings.TrimSpace(c.ID) == "" || seen[c.ID] {
			return fmt.Errorf("case identity is missing or duplicated")
		}
		seen[c.ID] = true
		if c.Skipped || c.SkipReason != "" || strings.TrimSpace(c.Lane) == "" {
			return fmt.Errorf("case %q is skipped or has no lane", c.ID)
		}
		if c.Kind != KindCapability && c.Kind != KindMemory {
			return fmt.Errorf("case %q has an unknown kind", c.ID)
		}
		if c.Split != SplitHeldIn && c.Split != SplitHeldOut {
			return fmt.Errorf("case %q has an unknown split", c.ID)
		}
		if len(c.Expected) == 0 || c.OfferCount < 0 {
			return fmt.Errorf("case %q has incomplete outcomes", c.ID)
		}
		for _, e := range c.Expected {
			if strings.TrimSpace(e.ID) == "" || e.Rank < 0 || e.Rank > c.OfferCount || e.Hit != (e.Rank > 0) || (e.AbsentFromGraph && e.Rank != 0) {
				return fmt.Errorf("case %q has invalid expected outcomes", c.ID)
			}
		}
		expected := c
		expected.RR = 0
		finalizeCase(&expected)
		if c.BestRank != expected.BestRank || c.Hit1 != expected.Hit1 || c.Hit5 != expected.Hit5 || !equalMetric(c.RR, expected.RR) {
			return fmt.Errorf("case %q has inconsistent rank metrics", c.ID)
		}
	}
	if len(r.Splits) != 2 {
		return fmt.Errorf("both evaluation splits are required")
	}
	for _, split := range []string{SplitHeldIn, SplitHeldOut} {
		m, ok := r.Splits[split]
		if !ok || m.Cases == 0 || !equalMetrics(m, aggregate(r.Cases, split)) {
			return fmt.Errorf("split %q has missing or inconsistent coverage", split)
		}
	}
	if !equalMetrics(r.Overall, aggregate(r.Cases, "")) {
		return fmt.Errorf("overall metrics disagree with case outcomes")
	}
	return validateEvaluationContract(r)
}

func equalMetric(a, b float64) bool {
	return !math.IsNaN(a) && !math.IsInf(a, 0) && math.Abs(a-b) <= 1e-12
}

func equalMetrics(a, b Metrics) bool {
	return a.Cases == b.Cases && a.ForbiddenViolations == b.ForbiddenViolations && a.OutrankViolations == b.OutrankViolations && equalMetric(a.Hit1, b.Hit1) && equalMetric(a.Hit5, b.Hit5) && equalMetric(a.MRR, b.MRR)
}

func sameCaseInventory(before, after Report) bool {
	if len(before.Cases) != len(after.Cases) {
		return false
	}
	byID := map[string]CaseResult{}
	for _, c := range before.Cases {
		byID[c.ID] = c
	}
	for _, c := range after.Cases {
		b, ok := byID[c.ID]
		if !ok || b.Kind != c.Kind || b.Split != c.Split || b.Lane != c.Lane {
			return false
		}
		ids := func(values []ExpectedResult) []string {
			out := make([]string, len(values))
			for i, e := range values {
				out[i] = e.ID
			}
			sort.Strings(out)
			return out
		}
		x, y := ids(b.Expected), ids(c.Expected)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
	}
	return true
}

// LaneDelta preserves the existing split metric semantics for each declared lane.
type LaneDelta struct {
	Lane string `json:"lane"`
	SplitDelta
}

func compareLanes(before, after Report) []LaneDelta {
	type key struct{ split, lane string }
	groups := func(r Report) map[key][]CaseResult {
		out := map[key][]CaseResult{}
		for _, c := range r.Cases {
			k := key{c.Split, c.Lane}
			out[k] = append(out[k], c)
		}
		return out
	}
	b, a := groups(before), groups(after)
	keys := map[key]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	ordered := make([]key, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].split == ordered[j].split {
			return ordered[i].lane < ordered[j].lane
		}
		return ordered[i].split < ordered[j].split
	})
	out := make([]LaneDelta, 0, len(ordered))
	for _, k := range ordered {
		out = append(out, LaneDelta{Lane: k.lane, SplitDelta: splitDelta(k.split, aggregate(b[k], ""), aggregate(a[k], ""))})
	}
	return out
}

// WriteCompareSummary renders comparison evidence and never labels a non-certifying report accepted.
func WriteCompareSummary(w io.Writer, cmp CompareReport) {
	verdict := "ACCEPT"
	if !cmp.Accept {
		verdict = "REJECT"
	}
	fmt.Fprintf(w, "# compound bench compare: %s\n", verdict)
	if cmp.SuiteMismatch {
		fmt.Fprintf(w, "warning: before suite %q != after suite %q (comparison is apples-to-oranges)\n", cmp.BeforeSuite, cmp.AfterSuite)
	}
	for _, problem := range cmp.InputErrors {
		fmt.Fprintf(w, "non-certifying: %s\n", problem)
	}
	if cmp.InputMismatch {
		fmt.Fprintln(w, "evaluation inputs or case coverage differ")
	}
	for _, lane := range cmp.RegressedLanes {
		fmt.Fprintf(w, "regressed lane: %s\n", lane)
	}
	for _, d := range cmp.Splits {
		flag := ""
		if d.Regressed {
			flag = "  <- REGRESSED"
		}
		fmt.Fprintf(w, "  %-9s hit@1 %+.3f  hit@5 %+.3f  mrr %+.3f%s\n", d.Split, d.Hit1Delta, d.Hit5Delta, d.MRRDelta, flag)
	}
	fmt.Fprintf(w, "  %-9s hit@1 %+.3f  hit@5 %+.3f  mrr %+.3f\n", "overall", cmp.Overall.Hit1Delta, cmp.Overall.Hit5Delta, cmp.Overall.MRRDelta)
	if cmp.ProofRef != "" {
		fmt.Fprintf(w, "proof: %s  (cite this in the harness-edit close ProofRefs to stamp should_harden)\n", cmp.ProofRef)
	}
}
