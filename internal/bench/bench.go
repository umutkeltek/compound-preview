// Package bench is the evaluator organ: `compound bench`. It scores whether the SAME
// surfacing paths the agent-facing `compound orient` uses actually rank the ids a suite
// says they should — capability offers via internal/capgraph, memory recall via
// internal/contract. It is the anti-"landed ≠ working" instrument: a deterministic,
// zero-LLM, zero-authority-mutation measurement whose report is EVIDENCE, not a decision.
//
// Two invariants make the report trustworthy:
//   - It calls the real surfacing/ranking code (capgraph.Store.SurfaceForContext,
//     contract.BenchMemoryRank), never a parallel scorer with different semantics.
//   - It separates a RANKED MISS ("the engine failed to rank this id") from
//     ABSENT_FROM_GRAPH ("the id isn't in the graph/ledger to be found at all"). That
//     parity gap was the real finding of the Wave-2 gate (39/121 ids absent) and is
//     invisible to a scorer that only measures hit@k.
//
// Scoring has three families, all read off the SAME ranked id list from one surfacing call:
//   - Positive: per-expected-id hit@1/hit@5/MRR (the best-ranked relevant id wins the case).
//   - Negative (Case.Forbidden): none of the top-N surfaced ids may contain a forbidden
//     substring — a leakage guard mirroring the acceptance suite's forbidden_top_n_contains.
//   - Relative (Case.MustOutrank): a winner id must rank strictly above every ranked
//     loser-substring match — mirroring the acceptance suite's must_outrank. An absent winner
//     folds into the absent_from_graph parity gap, never a silent pass.
//
// The negative/relative families produce VIOLATION lists per case and violation TOTALS per split;
// Compare regresses when a split's violation total increases, on top of the hit@5/MRR-drop rule.
// The additions are strictly additive: no positive hit/rank field changed, so a suite that asserts
// neither scores byte-identically to the pre-extension organ.
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/contract"
)

// SchemaVersion identifies the report shape; bump on any breaking field change.
const SchemaVersion = "compound.bench_report.v0.1"

// CompareSchemaVersion identifies the compare-report shape.
const CompareSchemaVersion = "compound.bench_compare.v0.1"

// Case kinds.
const (
	KindCapability = "capability"
	KindMemory     = "memory"
)

// Splits. The held-in / held-out accept rule (no regression on EITHER) is what keeps a
// tuning change from overfitting the cases it was tuned on.
const (
	SplitHeldIn  = "held_in"
	SplitHeldOut = "held_out"
)

// Suite is the on-disk evaluation suite. Dropped is optional converter provenance (empty for
// hand-written suites): every assertion the converter could NOT express is named here with a
// count, so a converted suite is never a silent truncation of its source.
type Suite struct {
	Suite   string       `json:"suite"`
	Dropped []DropRecord `json:"dropped,omitempty"`
	Cases   []Case       `json:"cases"`
}

// DropRecord names one class of source assertion the converter deliberately did not carry into a
// case, with how many it dropped and why. It is loud accounting, not a silent omission.
type DropRecord struct {
	Field  string `json:"field"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

// Case is one evaluation case. Harness is an additive, optional scoping hint for capability
// surfacing (empty = generic, surfaces all non-harness-restricted nodes, matching orient's
// behavior for a generic task); it is not part of the required suite schema.
type Case struct {
	ID          string             `json:"id"`
	Kind        string             `json:"kind"`                   // "capability" | "memory"
	Task        string             `json:"task"`                   // free text scored through the real surfacing path
	Expected    []string           `json:"expected"`               // node ids (capability) or claim ids (memory)
	Lane        string             `json:"lane"`                   // "exact" | "paraphrase" | "casual" | "graph"
	Split       string             `json:"split"`                  // "held_in" | "held_out"
	Harness     string             `json:"harness,omitempty"`      // optional capability surfacing scope
	RepoKind    string             `json:"repo_kind,omitempty"`    // optional; memory recall rule scope (default control-plane)
	Topic       string             `json:"topic,omitempty"`        // optional; narrows memory ambient recall
	Forbidden   *ForbiddenTopN     `json:"forbidden,omitempty"`    // optional; none of top-n surfaced ids may contain a listed substring
	MustOutrank []OutrankAssertion `json:"must_outrank,omitempty"` // optional; winner must rank strictly above every loser-substring match
}

// ForbiddenTopN is a negative assertion: the case violates it if any of the top-N surfaced ids
// contains any listed substring. It mirrors the acceptance suite's forbidden_top_n_contains
// (substring semantics) — a leakage guard, distinct from the positive hit/rank scoring which is
// left untouched. Nil = not asserted.
type ForbiddenTopN struct {
	N        int      `json:"n"`
	Contains []string `json:"contains"`
}

// OutrankAssertion is a relative-order assertion: the Winner id's rank must be strictly better
// (numerically lower) than every RANKED id matching any LoserContains substring. It mirrors the
// acceptance suite's must_outrank[{winner, loser_contains}] (substring losers, exact winner).
// A winner that is absent from the graph is folded into the absent_from_graph mechanics (a parity
// gap), never a silent pass.
type OutrankAssertion struct {
	Winner        string   `json:"winner"`
	LoserContains []string `json:"loser_contains"`
}

// ExpectedResult is the outcome for one expected id in a case.
type ExpectedResult struct {
	ID              string `json:"id"`
	Rank            int    `json:"rank"`              // 1-based position in the ranked offers; 0 = not ranked
	Hit             bool   `json:"hit"`               // ranked at all (rank > 0)
	AbsentFromGraph bool   `json:"absent_from_graph"` // id not present as a node/claim at all (parity gap)
}

// CaseResult is the per-case scored outcome.
type CaseResult struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"`
	Split      string           `json:"split"`
	Lane       string           `json:"lane,omitempty"`
	Skipped    bool             `json:"skipped,omitempty"`
	SkipReason string           `json:"skip_reason,omitempty"`
	OfferCount int              `json:"offer_count"`
	Expected   []ExpectedResult `json:"expected"`
	Hit1       bool             `json:"hit_1"`
	Hit5       bool             `json:"hit_5"`
	RR         float64          `json:"reciprocal_rank"`
	BestRank   int              `json:"best_rank"` // best (min) rank across ranked expected ids; 0 = none ranked
	// Violation outcomes for the additive negative/relative assertions. Empty when the case
	// asserts none (or asserts them and passes). They never touch the hit/rank fields above.
	ForbiddenViolations []string `json:"forbidden_violations,omitempty"` // top-n ids that contain a forbidden substring
	OutrankViolations   []string `json:"outrank_violations,omitempty"`   // "<winner> !> <loser>" or "<winner> absent_from_graph"
}

// Metrics aggregates ranking quality over a set of scored (non-skipped) cases. The violation
// counters are additive TOTALS (not means): the number of forbidden / outrank violation entries
// across the split's cases. Compare regresses when either total increases (see splitDelta).
type Metrics struct {
	Cases               int     `json:"cases"`
	Hit1                float64 `json:"hit_1"`
	Hit5                float64 `json:"hit_5"`
	MRR                 float64 `json:"mrr"`
	ForbiddenViolations int     `json:"forbidden_violations"`
	OutrankViolations   int     `json:"outrank_violations"`
}

// Report is the evaluator's evidence artifact. Identity fields (BuildSHA, GraphHash, Suite,
// Timestamp) identify the evaluated build and graph. InputDigest binds comparable evaluation
// questions independently of those revisions. The report never carries the private home path.
type Report struct {
	Evaluation      *EvaluationContract `json:"evaluation_contract,omitempty"`
	InputDigest     string              `json:"input_digest,omitempty"` // canonical evaluation inputs; absent on legacy reports
	Schema          string              `json:"schema"`
	Suite           string              `json:"suite"`
	SuiteKind       string              `json:"suite_kind,omitempty"` // synthetic_fixture | acceptance | golden | dogfood — a label, not authority
	Actionable      *bool               `json:"actionable,omitempty"` // nil/true => director may mine; false => synthetic/diagnostic, director must ignore
	Timestamp       string              `json:"timestamp"`
	BuildSHA        string              `json:"build_sha"`
	GraphHash       string              `json:"graph_hash"`
	CaseCount       int                 `json:"case_count"`
	ScoredCount     int                 `json:"scored_count"`
	SkippedCount    int                 `json:"skipped_count"`
	Splits          map[string]Metrics  `json:"splits"`
	Overall         Metrics             `json:"overall"`
	AbsentFromGraph []string            `json:"absent_from_graph"` // sorted unique across scored cases
	Cases           []CaseResult        `json:"cases"`
}

// IsActionable reports whether the director may mine this report for harness-edit candidates. An
// unset (nil) Actionable reads as actionable so reports written before the field existed — and any
// report produced without an explicit --no-actionable declaration — keep feeding the director; only
// a deliberate false (synthetic fixture / diagnostic) opts a report out.
func (r Report) IsActionable() bool {
	return r.Actionable == nil || *r.Actionable
}

// Options configure a run. Now is injected so the report timestamp is deterministic in tests
// (the repo forbids comparing wall-clock against fixed fixtures).
type Options struct {
	Home       string
	Suite      Suite
	BuildSHA   string
	Now        time.Time
	Policy     contract.OrientationPolicy
	SuiteKind  string // copied into the report label
	Actionable *bool  // copied into the report; nil => actionable (back-compat)
}

// LoadSuite reads and validates a suite JSON file.
func LoadSuite(path string) (Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, err
	}
	var s Suite
	if err := json.Unmarshal(data, &s); err != nil {
		return Suite{}, fmt.Errorf("parse suite %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return Suite{}, err
	}
	return s, nil
}

func (s Suite) validate() error {
	if strings.TrimSpace(s.Suite) == "" {
		return fmt.Errorf("suite name is required")
	}
	if len(s.Cases) == 0 {
		return fmt.Errorf("suite %q has no cases", s.Suite)
	}
	seen := map[string]bool{}
	for i, c := range s.Cases {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("case %d has no id", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		switch c.Kind {
		case KindCapability, KindMemory:
		default:
			return fmt.Errorf("case %q: kind must be %q or %q, got %q", c.ID, KindCapability, KindMemory, c.Kind)
		}
		switch c.Split {
		case SplitHeldIn, SplitHeldOut:
		default:
			return fmt.Errorf("case %q: split must be %q or %q, got %q", c.ID, SplitHeldIn, SplitHeldOut, c.Split)
		}
		if len(c.Expected) == 0 {
			return fmt.Errorf("case %q: expected ids are required (a must-hit set)", c.ID)
		}
	}
	return nil
}

// Run scores the suite against the live home and returns the evidence report. It is read-only
// and deterministic: capability offers come from capgraph.Store.SurfaceForContext with no
// embedder attached, memory ranking from contract.BenchMemoryRank with the semantic sidecar
// off — the same code the agent path runs, minus only the non-deterministic re-rankers.
func Run(opts Options) (Report, error) {
	if err := opts.Suite.validate(); err != nil {
		return Report{}, err
	}
	pol := opts.Policy
	if len(pol.RepoKinds) == 0 {
		pol = contract.DefaultPolicy()
	}
	graphHash, err := capgraph.AuthorityRevision(opts.Home)
	if err != nil {
		return Report{}, err
	}

	evaluation := evaluationContract(opts.Suite)
	report := Report{
		Evaluation:  &evaluation,
		InputDigest: evaluationInputDigest(opts.Suite),
		Schema:      SchemaVersion,
		Suite:       opts.Suite.Suite,
		SuiteKind:   opts.SuiteKind,
		Actionable:  opts.Actionable,
		Timestamp:   opts.Now.UTC().Format(time.RFC3339),
		BuildSHA:    strings.TrimSpace(opts.BuildSHA),
		GraphHash:   graphHash,
		CaseCount:   len(opts.Suite.Cases),
		Splits:      map[string]Metrics{},
	}

	// Cache the memory ranking per (repo_kind, topic, task) is unnecessary; each memory case
	// carries its own task. Capability surfacing likewise runs per case task. We do reuse the
	// capability node present-set across the whole run since it is graph-wide.
	capPresent, capPresentErr := capabilityPresentSet(opts.Home)

	absent := map[string]bool{}
	var scored []CaseResult

	for _, c := range opts.Suite.Cases {
		res := CaseResult{ID: c.ID, Kind: c.Kind, Split: c.Split, Lane: c.Lane}
		switch c.Kind {
		case KindCapability:
			if capPresentErr != nil {
				return Report{}, capPresentErr
			}
			ranks, offerCount, err := capabilityRanks(opts.Home, c)
			if err != nil {
				return Report{}, err
			}
			res.OfferCount = offerCount
			scoreExpected(&res, c.Expected, ranks, capPresent)
			scoreAssertions(&res, c, ranks, capPresent)
		case KindMemory:
			in := contract.Input{Task: c.Task, Harness: c.Harness, RepoKind: memoryRepoKind(c), Topic: c.Topic}
			rankedIDs, present, available, err := contract.BenchMemoryRank(opts.Home, in, pol)
			if err != nil {
				return Report{}, err
			}
			if !available {
				res.Skipped = true
				res.SkipReason = "no memory recall available in home (ledger empty/absent or repo-kind has no ambient memory rule)"
				report.Cases = append(report.Cases, res)
				continue
			}
			ranks := ranksFromOrder(rankedIDs)
			res.OfferCount = len(rankedIDs)
			scoreExpected(&res, c.Expected, ranks, present)
			scoreAssertions(&res, c, ranks, present)
		}
		finalizeCase(&res)
		for _, er := range res.Expected {
			if er.AbsentFromGraph {
				absent[er.ID] = true
			}
		}
		scored = append(scored, res)
		report.Cases = append(report.Cases, res)
	}

	report.ScoredCount = len(scored)
	report.SkippedCount = report.CaseCount - report.ScoredCount
	report.Splits[SplitHeldIn] = aggregate(scored, SplitHeldIn)
	report.Splits[SplitHeldOut] = aggregate(scored, SplitHeldOut)
	report.Overall = aggregate(scored, "")
	report.AbsentFromGraph = sortedKeys(absent)
	return report, nil
}

// capabilityPresentSet returns the set of all node ids in the graph (including retired ones —
// a retired node exists and is a RANKED miss, not absent). An absent home yields an empty set.
func capabilityPresentSet(home string) (map[string]bool, error) {
	present := map[string]bool{}
	if !capgraph.Exists(home) {
		return present, nil
	}
	store, err := capgraph.Open(home)
	if err != nil {
		return nil, err
	}
	nodes, err := store.List()
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		present[n.ID] = true
	}
	return present, nil
}

// capabilityRanks surfaces the real capability offers for a case task and returns a 1-based
// rank per node id plus the offer count. It mirrors contract.surfaceCapabilityHits' guard and
// context construction, minus the embedder (kept off for determinism / zero LLM).
func capabilityRanks(home string, c Case) (map[string]int, int, error) {
	if !capgraph.Exists(home) {
		return map[string]int{}, 0, nil
	}
	store, err := capgraph.Open(home)
	if err != nil {
		return nil, 0, err
	}
	offers, err := store.SurfaceForContext(capgraph.SurfaceContext{
		Task:    c.Task,
		Harness: c.Harness,
	})
	if err != nil {
		return nil, 0, err
	}
	return ranksFromOffers(offers), len(offers), nil
}

func ranksFromOffers(offers []capgraph.Surfaced) map[string]int {
	ranks := make(map[string]int, len(offers))
	for i, o := range offers {
		if _, seen := ranks[o.Node.ID]; !seen {
			ranks[o.Node.ID] = i + 1
		}
	}
	return ranks
}

func ranksFromOrder(ids []string) map[string]int {
	ranks := make(map[string]int, len(ids))
	for i, id := range ids {
		if _, seen := ranks[id]; !seen {
			ranks[id] = i + 1
		}
	}
	return ranks
}

// scoreExpected fills res.Expected: for each expected id, absent if not in present-set, else
// its 1-based rank (0 if present-but-unranked = a genuine engine miss).
func scoreExpected(res *CaseResult, expected []string, ranks map[string]int, present map[string]bool) {
	for _, id := range expected {
		er := ExpectedResult{ID: id}
		if !present[id] {
			er.AbsentFromGraph = true
			res.Expected = append(res.Expected, er)
			continue
		}
		if r, ok := ranks[id]; ok && r > 0 {
			er.Rank = r
			er.Hit = true
		}
		res.Expected = append(res.Expected, er)
	}
}

// scoreAssertions fills the additive negative/relative violation outcomes on res from the SAME
// ranked id list the hit scoring used (no second surfacing pass). Matching is substring/prefix
// based, matching the acceptance file's semantics. It never touches res.Hit*/RR/BestRank.
//
//   - Forbidden: a violation for each of the top-N surfaced ids that contains any forbidden
//     substring. Present-agnostic — a forbidden id that surfaced is a leak regardless of whether
//     it is a "real" node.
//   - MustOutrank: for each assertion, a violation for every RANKED loser-substring match the
//     winner does not rank strictly above (winner unranked, or winner rank >= loser rank). A
//     winner ABSENT from the graph is folded into the absent_from_graph mechanics (appended as an
//     absent ExpectedResult so Run counts it in the parity gap) and recorded as a violation —
//     never a silent pass.
func scoreAssertions(res *CaseResult, c Case, ranks map[string]int, present map[string]bool) {
	if c.Forbidden != nil {
		var viol []string
		seen := map[string]bool{}
		for id, r := range ranks {
			if r < 1 || r > c.Forbidden.N {
				continue
			}
			if containsAny(id, c.Forbidden.Contains) && !seen[id] {
				seen[id] = true
				viol = append(viol, id)
			}
		}
		sort.Strings(viol)
		res.ForbiddenViolations = viol
	}
	if len(c.MustOutrank) == 0 {
		return
	}
	var viol []string
	for _, a := range c.MustOutrank {
		winnerRank, winnerRanked := ranks[a.Winner]
		if !present[a.Winner] {
			// Absent winner: fold into absent_from_graph (unless already an expected id there) and
			// record the violation. It is a parity gap, not a pass.
			if !expectedHasID(res.Expected, a.Winner) {
				res.Expected = append(res.Expected, ExpectedResult{ID: a.Winner, AbsentFromGraph: true})
			}
			viol = append(viol, a.Winner+" absent_from_graph")
			continue
		}
		for id, r := range ranks {
			if r < 1 || id == a.Winner {
				continue
			}
			if !containsAny(id, a.LoserContains) {
				continue
			}
			if !winnerRanked || winnerRank >= r {
				viol = append(viol, a.Winner+" !> "+id)
			}
		}
	}
	sort.Strings(viol)
	res.OutrankViolations = viol
}

func containsAny(id string, subs []string) bool {
	for _, s := range subs {
		if s != "" && strings.Contains(id, s) {
			return true
		}
	}
	return false
}

func expectedHasID(expected []ExpectedResult, id string) bool {
	for _, er := range expected {
		if er.ID == id {
			return true
		}
	}
	return false
}

// finalizeCase computes per-case hit@1/hit@5/MRR from the best (min) rank across ranked
// expected ids. A case with multiple relevant ids is credited by its best-ranked one (the
// standard IR convention for a query with several relevant documents).
func finalizeCase(res *CaseResult) {
	best := 0
	for _, er := range res.Expected {
		if er.Rank > 0 && (best == 0 || er.Rank < best) {
			best = er.Rank
		}
	}
	res.BestRank = best
	res.Hit1 = best == 1
	res.Hit5 = best >= 1 && best <= 5
	if best > 0 {
		res.RR = 1.0 / float64(best)
	}
}

// aggregate computes mean metrics over non-skipped cases; split=="" aggregates all.
func aggregate(cases []CaseResult, split string) Metrics {
	var m Metrics
	var hit1, hit5, rr float64
	for _, c := range cases {
		if c.Skipped {
			continue
		}
		if split != "" && c.Split != split {
			continue
		}
		m.Cases++
		if c.Hit1 {
			hit1++
		}
		if c.Hit5 {
			hit5++
		}
		rr += c.RR
		m.ForbiddenViolations += len(c.ForbiddenViolations)
		m.OutrankViolations += len(c.OutrankViolations)
	}
	if m.Cases > 0 {
		n := float64(m.Cases)
		m.Hit1 = hit1 / n
		m.Hit5 = hit5 / n
		m.MRR = rr / n
	}
	return m
}

func memoryRepoKind(c Case) string {
	if strings.TrimSpace(c.RepoKind) != "" {
		return c.RepoKind
	}
	return "control-plane"
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HasAbsent reports whether any scored expected id was absent from the graph/ledger. The CLI
// uses this to fail loud (nonzero exit) unless --allow-absent is passed: a must-hit id that
// isn't even present is a parity gap, not a tuning miss.
func (r Report) HasAbsent() bool { return len(r.AbsentFromGraph) > 0 }

// SplitDelta is the per-split before→after change.
type SplitDelta struct {
	Split      string  `json:"split"`
	BeforeHit1 float64 `json:"before_hit_1"`
	AfterHit1  float64 `json:"after_hit_1"`
	Hit1Delta  float64 `json:"hit_1_delta"`
	BeforeHit5 float64 `json:"before_hit_5"`
	AfterHit5  float64 `json:"after_hit_5"`
	Hit5Delta  float64 `json:"hit_5_delta"`
	BeforeMRR  float64 `json:"before_mrr"`
	AfterMRR   float64 `json:"after_mrr"`
	MRRDelta   float64 `json:"mrr_delta"`
	// Additive assertion-violation deltas. A split regresses when violations INCREASE, on top of
	// the existing hit@5/MRR-drop rule.
	BeforeForbiddenViolations int  `json:"before_forbidden_violations"`
	AfterForbiddenViolations  int  `json:"after_forbidden_violations"`
	BeforeOutrankViolations   int  `json:"before_outrank_violations"`
	AfterOutrankViolations    int  `json:"after_outrank_violations"`
	Regressed                 bool `json:"regressed"` // hit@5 OR MRR lower, OR a violation count higher, after
}

// CompareReport is the before→after delta with the held-in/held-out accept verdict.
type CompareReport struct {
	InputErrors    []string     `json:"input_errors,omitempty"`
	InputMismatch  bool         `json:"input_mismatch"`
	Lanes          []LaneDelta  `json:"lanes"`
	RegressedLanes []string     `json:"regressed_lanes,omitempty"`
	Schema         string       `json:"schema"`
	BeforeSuite    string       `json:"before_suite"`
	AfterSuite     string       `json:"after_suite"`
	BeforeBuildSHA string       `json:"before_build_sha"`
	AfterBuildSHA  string       `json:"after_build_sha"`
	Splits         []SplitDelta `json:"splits"`
	Overall        SplitDelta   `json:"overall"`
	Regressed      bool         `json:"regressed"`           // any split or lane regressed => reject
	RegressedSplit []string     `json:"regressed_splits"`    // which splits regressed
	Accept         bool         `json:"accept"`              // complete comparable inputs, same suite, no split/lane regression
	ProofRef       string       `json:"proof_ref,omitempty"` // bench_compare_accept:<suite> on accept — the token a harness-edit close cites to harden
	SuiteMismatch  bool         `json:"suite_mismatch"`      // before/after are different suites — comparison is apples-to-oranges; never Accept
}

// Compare certifies complete reports for the same evaluation inputs only when neither
// a split nor any declared lane regresses. Build and graph revisions may differ.
// Legacy reports remain readable, but cannot provide missing input identity.
func Compare(before, after Report) CompareReport {
	cr := CompareReport{
		Schema:         CompareSchemaVersion,
		BeforeSuite:    before.Suite,
		AfterSuite:     after.Suite,
		BeforeBuildSHA: before.BuildSHA,
		AfterBuildSHA:  after.BuildSHA,
		SuiteMismatch:  before.Suite != after.Suite,
	}
	for _, split := range []string{SplitHeldIn, SplitHeldOut} {
		d := splitDelta(split, before.Splits[split], after.Splits[split])
		if d.Regressed {
			cr.Regressed = true
			cr.RegressedSplit = append(cr.RegressedSplit, split)
		}
		cr.Splits = append(cr.Splits, d)
	}
	cr.Overall = splitDelta("overall", before.Overall, after.Overall)
	if err := certificationError(before); err != nil {
		cr.InputErrors = append(cr.InputErrors, "before: "+err.Error())
	}
	if err := certificationError(after); err != nil {
		cr.InputErrors = append(cr.InputErrors, "after: "+err.Error())
	}
	cr.InputMismatch = before.InputDigest != after.InputDigest || !sameCaseInventory(before, after)
	if len(cr.InputErrors) == 0 && !cr.InputMismatch {
		cr.Lanes = compareLanes(before, after)
		for _, lane := range cr.Lanes {
			if lane.Regressed {
				cr.Regressed = true
				cr.RegressedLanes = append(cr.RegressedLanes, lane.Split+"/"+lane.Lane)
			}
		}
	}
	cr.Accept = len(cr.InputErrors) == 0 && !cr.InputMismatch && !cr.Regressed && !cr.SuiteMismatch
	if cr.Accept {
		cr.ProofRef = "bench_compare_accept:" + after.Suite
	}
	return cr
}

func splitDelta(split string, before, after Metrics) SplitDelta {
	d := SplitDelta{
		Split:      split,
		BeforeHit1: before.Hit1, AfterHit1: after.Hit1, Hit1Delta: after.Hit1 - before.Hit1,
		BeforeHit5: before.Hit5, AfterHit5: after.Hit5, Hit5Delta: after.Hit5 - before.Hit5,
		BeforeMRR: before.MRR, AfterMRR: after.MRR, MRRDelta: after.MRR - before.MRR,
		BeforeForbiddenViolations: before.ForbiddenViolations, AfterForbiddenViolations: after.ForbiddenViolations,
		BeforeOutrankViolations: before.OutrankViolations, AfterOutrankViolations: after.OutrankViolations,
	}
	d.Regressed = after.Hit5 < before.Hit5 || after.MRR < before.MRR ||
		after.ForbiddenViolations > before.ForbiddenViolations ||
		after.OutrankViolations > before.OutrankViolations
	return d
}
