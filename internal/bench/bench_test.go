package bench

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/mem"
)

// fixtureNode is a seed node; ids MUST match fixtures/bench/capability-suite.json so the
// committed suite scores real hits against the graph these tests build.
type fixtureNode struct {
	id       string
	kind     string
	name     string
	triggers []string
	retired  bool
}

func fixtureNodes() []fixtureNode {
	return []fixtureNode{
		{"skill:pricing", "skill", "Pricing", []string{"pricing", "paywall", "monetization"}, false},
		{"skill:paywall", "skill", "Paywall Design", []string{"paywall", "upgrade", "subscription"}, false},
		{"tool:revenuecat", "tool", "RevenueCat", []string{"subscription", "receipt", "entitlement"}, false},
		{"lens:reasoning", "lens", "Reasoning Lenses", []string{"inversion", "tradeoff", "decision"}, false},
		{"procedure:migration", "procedure", "DB Migration", []string{"migration", "schema", "rollback"}, false},
		{"skill:security-review", "skill", "Security Review", []string{"security", "auth", "injection"}, false},
		{"tool:agent-browser", "tool", "Agent Browser", []string{"browser", "automation", "scrape"}, false},
		{"skill:ios-concurrency", "skill", "Swift Concurrency", []string{"swift", "concurrency", "actor"}, false},
		{"procedure:release", "procedure", "App Store Release", []string{"release", "submission", "store"}, false},
		{"lens:cost", "lens", "Unit Economics", []string{"margin", "cac", "pricing"}, false},
	}
}

func seedGraph(t *testing.T, home string, nodes []fixtureNode) {
	t.Helper()
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatalf("open capgraph: %v", err)
	}
	for _, n := range nodes {
		if err := store.Add(capgraph.Node{ID: n.id, Kind: n.kind, Name: n.name, Triggers: n.triggers, Retired: n.retired}); err != nil {
			t.Fatalf("add %s: %v", n.id, err)
		}
	}
}

func fixtureNodesExcept(drop string) []fixtureNode {
	var out []fixtureNode
	for _, n := range fixtureNodes() {
		if n.id == drop {
			continue
		}
		out = append(out, n)
	}
	return out
}

func caseByID(report Report, id string) CaseResult {
	for _, c := range report.Cases {
		if c.ID == id {
			return c
		}
	}
	return CaseResult{}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func fixedNow() time.Time { return time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC) }

// TestRunSeparatesAbsentFromRankedMiss is the CRITICAL-distinction falsifier: an expected id
// that isn't in the graph at all (absent_from_graph) must be reported distinctly from an
// expected id that IS present but the engine failed to rank (a ranked miss). A scorer that only
// measures hit@k conflates the two and reports both as rank 0 — this test fails red on that.
func TestRunSeparatesAbsentFromRankedMiss(t *testing.T) {
	home := t.TempDir()
	seedGraph(t, home, fixtureNodes())

	suite := Suite{
		Suite: "distinction",
		Cases: []Case{
			// present AND ranked: skill:pricing tops "pricing".
			{ID: "hit", Kind: KindCapability, Task: "pricing paywall", Expected: []string{"skill:pricing"}, Split: SplitHeldIn, Lane: "exact"},
			// present but UNRANKED for this task: skill:pricing exists, "migration schema" won't surface it -> ranked miss, NOT absent.
			{ID: "ranked-miss", Kind: KindCapability, Task: "migration schema rollback", Expected: []string{"skill:pricing"}, Split: SplitHeldIn, Lane: "graph"},
			// ABSENT: skill:ghost was never added -> parity gap.
			{ID: "absent", Kind: KindCapability, Task: "pricing", Expected: []string{"skill:ghost"}, Split: SplitHeldOut, Lane: "exact"},
		},
	}
	report, err := Run(Options{Home: home, Suite: suite, BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := report.AbsentFromGraph; !reflect.DeepEqual(got, []string{"skill:ghost"}) {
		t.Fatalf("absent_from_graph = %v, want [skill:ghost] only (ranked miss must NOT be counted absent)", got)
	}
	byID := map[string]CaseResult{}
	for _, c := range report.Cases {
		byID[c.ID] = c
	}
	if er := byID["hit"].Expected[0]; er.AbsentFromGraph || !er.Hit || er.Rank != 1 {
		t.Fatalf("hit case expected = %+v, want present ranked #1", er)
	}
	if er := byID["ranked-miss"].Expected[0]; er.AbsentFromGraph || er.Hit || er.Rank != 0 {
		t.Fatalf("ranked-miss case expected = %+v, want present, unranked, NOT absent", er)
	}
	if er := byID["absent"].Expected[0]; !er.AbsentFromGraph || er.Hit {
		t.Fatalf("absent case expected = %+v, want absent_from_graph=true", er)
	}
}

// TestRunDeterministic falsifies nondeterminism: two runs over the same fixture home must
// produce byte-identical metrics, per-case results, absent list, and graph hash. The suite
// carries MULTIPLE absent ids so the absent-list ordering (sortedKeys) is exercised — Go
// randomizes map iteration, so an unsorted absent list would differ across the two runs.
func TestRunDeterministic(t *testing.T) {
	home := t.TempDir()
	seedGraph(t, home, fixtureNodes())
	suite := Suite{
		Suite: "determinism",
		Cases: []Case{
			{ID: "hit-1", Kind: KindCapability, Task: "pricing paywall", Expected: []string{"skill:pricing"}, Split: SplitHeldIn, Lane: "exact"},
			{ID: "hit-2", Kind: KindCapability, Task: "migration schema rollback", Expected: []string{"procedure:migration"}, Split: SplitHeldOut, Lane: "exact"},
			{ID: "absent-a", Kind: KindCapability, Task: "pricing", Expected: []string{"skill:zeta-ghost"}, Split: SplitHeldIn, Lane: "graph"},
			{ID: "absent-b", Kind: KindCapability, Task: "pricing", Expected: []string{"skill:alpha-ghost"}, Split: SplitHeldOut, Lane: "graph"},
			{ID: "absent-c", Kind: KindCapability, Task: "pricing", Expected: []string{"skill:mid-ghost"}, Split: SplitHeldIn, Lane: "graph"},
		},
	}
	a, err := Run(Options{Home: home, Suite: suite, BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run a: %v", err)
	}
	b, err := Run(Options{Home: home, Suite: suite, BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run b: %v", err)
	}
	if !reflect.DeepEqual(a.Splits, b.Splits) {
		t.Fatalf("splits differ:\n a=%+v\n b=%+v", a.Splits, b.Splits)
	}
	if !reflect.DeepEqual(a.Overall, b.Overall) {
		t.Fatalf("overall differ:\n a=%+v\n b=%+v", a.Overall, b.Overall)
	}
	if !reflect.DeepEqual(a.Cases, b.Cases) {
		t.Fatalf("cases differ")
	}
	if !reflect.DeepEqual(a.AbsentFromGraph, b.AbsentFromGraph) {
		t.Fatalf("absent differ: %v vs %v", a.AbsentFromGraph, b.AbsentFromGraph)
	}
	if a.GraphHash != b.GraphHash {
		t.Fatalf("graph hash differ: %s vs %s", a.GraphHash, b.GraphHash)
	}
}

// TestCommittedFixtureScoresRealHits proves the public fixture is not vacuous: over the graph
// its ids name, every case hits at rank 1 and nothing is absent.
func TestCommittedFixtureScoresRealHits(t *testing.T) {
	home := t.TempDir()
	seedGraph(t, home, fixtureNodes())
	suite, err := LoadSuite(filepath.Join("..", "..", "fixtures", "bench", "capability-suite.json"))
	if err != nil {
		t.Fatalf("load suite: %v", err)
	}
	report, err := Run(Options{Home: home, Suite: suite, BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(report.AbsentFromGraph) != 0 {
		t.Fatalf("absent = %v, want none", report.AbsentFromGraph)
	}
	if report.Overall.Hit1 != 1.0 || report.Overall.MRR != 1.0 {
		t.Fatalf("overall = %+v, want hit@1=1.0 mrr=1.0", report.Overall)
	}
	if report.Splits[SplitHeldIn].Cases != 5 || report.Splits[SplitHeldOut].Cases != 5 {
		t.Fatalf("split case counts = in:%d out:%d, want 5/5", report.Splits[SplitHeldIn].Cases, report.Splits[SplitHeldOut].Cases)
	}
}

// memoryFixtureClaim mirrors one synthetic candidate seeded for the committed memory suite; the
// resulting claim id (claim_ + sha256(entity|predicate|object|seq)[:12]) is what the fixture's
// expected ids reference. Fully synthetic content — never a real claim id or private query.
type memoryFixtureClaim struct {
	entity, predicate, object string
}

func memoryFixtureClaims() []memoryFixtureClaim {
	// Promotion order fixes seq (1, then 2); the fixture ids are derived from these tuples.
	return []memoryFixtureClaim{
		{"widget-calibration-doctrine", "records_mechanism", "synthetic widget calibration compensates sensor drift on the bench"},
		{"fibonacci-cache-doctrine", "records_finding", "memoized fibonacci cache eviction lowers recompute cost"},
	}
}

// seedMemoryLedger captures + promotes the synthetic claims (in order, so seq is deterministic) and
// returns entity->claim id. It uses only the public mem API (Capture/Adjudicate/AllCurrent), the
// same path a real ingestion runs, so the fixture is proven against a genuinely-built ledger.
func seedMemoryLedger(t *testing.T, home string) map[string]string {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	var cands []mem.Candidate
	for _, c := range memoryFixtureClaims() {
		cands = append(cands, mem.Candidate{
			Entity:          c.entity,
			Predicate:       c.predicate,
			Object:          c.object,
			Confidence:      0.9,
			Derivation:      "deterministic",
			ClaimType:       "architecture",
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  "ev_" + c.entity,
			SnippetHash:     "sha256:" + c.entity,
			Visibility:      "operational",
			ObservationDate: "2026-07-02T00:00:00Z",
		})
	}
	captured, err := store.Capture(cands, fixedNow())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(captured) != len(cands) {
		t.Fatalf("captured %d candidates, want %d (an echo/dedup collapsed one)", len(captured), len(cands))
	}
	for i, c := range captured {
		if _, err := store.Adjudicate(c.ID, "promote", "bench-test", fixedNow().Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("promote %s: %v", c.ID, err)
		}
	}
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatalf("all current: %v", err)
	}
	byEntity := map[string]string{}
	for _, cl := range claims {
		byEntity[cl.Entity] = cl.ID
	}
	return byEntity
}

// TestCommittedMemoryFixtureScoresRealHits is the memory mirror of TestCommittedFixtureScoresRealHits:
// over a ledger seeded with the synthetic claims, the committed memory suite hits at rank 1 with
// nothing absent — proving the fixture is NON-VACUOUS. A guard asserts the fixture's expected ids
// equal the freshly-seeded claim ids, so a future change to the mem id scheme reds loudly here
// instead of silently making the fixture score all-absent.
func TestCommittedMemoryFixtureScoresRealHits(t *testing.T) {
	home := t.TempDir()
	byEntity := seedMemoryLedger(t, home)

	suite, err := LoadSuite(filepath.Join("..", "..", "fixtures", "bench", "memory-suite.json"))
	if err != nil {
		t.Fatalf("load memory suite: %v", err)
	}

	// Guard: the committed fixture must reference exactly the ids the seed produces.
	wantIDs := map[string]string{
		"widget-calibration-doctrine": suite.Cases[0].Expected[0],
		"fibonacci-cache-doctrine":    suite.Cases[1].Expected[0],
	}
	for entity, fixtureID := range wantIDs {
		if got := byEntity[entity]; got != fixtureID {
			t.Fatalf("fixture claim id for %s = %q, seed produced %q — regenerate fixtures/bench/memory-suite.json", entity, fixtureID, got)
		}
	}

	report, err := Run(Options{Home: home, Suite: suite, BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.SkippedCount != 0 || report.ScoredCount != 2 {
		t.Fatalf("scored=%d skipped=%d, want 2/0 (ledger available, both cases scored)", report.ScoredCount, report.SkippedCount)
	}
	if len(report.AbsentFromGraph) != 0 {
		t.Fatalf("absent = %v, want none (fixture ids are seeded)", report.AbsentFromGraph)
	}
	if report.Overall.Hit1 != 1.0 || report.Overall.MRR != 1.0 {
		t.Fatalf("overall = %+v, want hit@1=1.0 mrr=1.0 (non-vacuous)", report.Overall)
	}
	if report.Splits[SplitHeldIn].Cases != 1 || report.Splits[SplitHeldOut].Cases != 1 {
		t.Fatalf("split cases = in:%d out:%d, want 1/1", report.Splits[SplitHeldIn].Cases, report.Splits[SplitHeldOut].Cases)
	}
}

// TestCompareFlagsHeldOutRegression falsifies the accept rule: a fabricated regression on
// held_out ALONE (held_in unchanged) must be rejected with held_out named.
func TestCompareFlagsHeldOutRegression(t *testing.T) {
	before := certificationRun(t, certificationSuite(), true, true)
	after := cloneCertificationReport(t, before)
	for i := range after.Cases {
		c := &after.Cases[i]
		if c.Split == SplitHeldOut {
			for j := range c.Expected {
				c.Expected[j].Rank = 0
				c.Expected[j].Hit = false
			}
			c.RR = 0
			finalizeCase(c)
		}
	}
	refreshCertificationMetrics(&after)
	cmp := Compare(before, after)
	if !cmp.Regressed || cmp.Accept {
		t.Fatalf("expected regression rejected, got regressed=%v accept=%v", cmp.Regressed, cmp.Accept)
	}
	if !reflect.DeepEqual(cmp.RegressedSplit, []string{SplitHeldOut}) {
		t.Fatalf("regressed splits = %v, want [held_out]", cmp.RegressedSplit)
	}

	// No-regression control (held_out improves) must accept.
	improve := cloneCertificationReport(t, before)
	if cmp := Compare(before, improve); cmp.Regressed || !cmp.Accept {
		t.Fatalf("no-regression compare rejected: regressed=%v accept=%v", cmp.Regressed, cmp.Accept)
	}
}

// TestCompareRejectsSuiteMismatch is the apples-to-oranges falsifier: two reports with the SAME
// metrics but DIFFERENT suites must report suite_mismatch=true and accept=false — without leaking
// into Regressed, which stays purely about metric movement. The same suite with equal metrics
// still accepts (control).
func TestCompareRejectsSuiteMismatch(t *testing.T) {
	before := certificationRun(t, certificationSuite(), true, true)
	after := before
	after.Suite = "memory-suite"

	cmp := Compare(before, after)
	if !cmp.SuiteMismatch {
		t.Fatalf("expected suite_mismatch=true for different suites")
	}
	if cmp.Accept {
		t.Fatalf("suite mismatch must force accept=false, got true")
	}
	if cmp.Regressed {
		t.Fatalf("equal metrics must not report regressed=true; Regressed is about metric movement only")
	}
	if cmp.ProofRef != "" {
		t.Fatalf("a non-accept (mismatch) must emit no proof token, got %q", cmp.ProofRef)
	}

	// Control: same suite, equal metrics accepts and emits the canonical proof token.
	same := Compare(before, before)
	if same.SuiteMismatch || !same.Accept {
		t.Fatalf("matching suite with equal metrics must accept: mismatch=%v accept=%v", same.SuiteMismatch, same.Accept)
	}
	if want := "bench_compare_accept:" + before.Suite; same.ProofRef != want {
		t.Fatalf("accept proof token = %q, want %q", same.ProofRef, want)
	}
}

// TestReportIsActionable pins the back-compat semantics: an unset (nil) Actionable — the shape of
// every report written before the field existed, and any report produced without --no-actionable —
// reads as actionable so the director keeps mining it. Only an explicit false opts a report out.
func TestReportIsActionable(t *testing.T) {
	no := false
	yes := true
	cases := []struct {
		name string
		r    *bool
		want bool
	}{
		{"unset (back-compat)", nil, true},
		{"explicit true", &yes, true},
		{"explicit false (synthetic)", &no, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Report{Actionable: c.r}
			if got := r.IsActionable(); got != c.want {
				t.Fatalf("IsActionable=%v, want %v", got, c.want)
			}
		})
	}
}

// TestForbiddenTopNRedGreen is the negative-assertion falsifier: a forbidden substring that
// surfaces inside the top-n is recorded as a violation (RED); with the offending node removed the
// same assertion is clean (GREEN). The positive expected id is a DIFFERENT still-present node so
// removing the forbidden node does not manufacture an absent expected id — isolating the forbidden
// signal from the hit signal.
func TestForbiddenTopNRedGreen(t *testing.T) {
	suite := func() Suite {
		return Suite{Suite: "forbidden", Cases: []Case{{
			ID: "f", Kind: KindCapability, Task: "paywall upgrade subscription",
			Expected: []string{"skill:paywall"}, Split: SplitHeldIn, Lane: "exact",
			Forbidden: &ForbiddenTopN{N: 10, Contains: []string{"revenuecat"}},
		}}}
	}

	// RED: tool:revenuecat present and surfaces on "subscription" within top-10.
	homeRed := t.TempDir()
	seedGraph(t, homeRed, fixtureNodes())
	red, err := Run(Options{Home: homeRed, Suite: suite(), BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run red: %v", err)
	}
	rc := caseByID(red, "f")
	if !contains(rc.ForbiddenViolations, "tool:revenuecat") {
		t.Fatalf("RED forbidden_violations = %v, want tool:revenuecat recorded", rc.ForbiddenViolations)
	}
	if red.Splits[SplitHeldIn].ForbiddenViolations != 1 {
		t.Fatalf("RED split forbidden total = %d, want 1", red.Splits[SplitHeldIn].ForbiddenViolations)
	}

	// GREEN: remove tool:revenuecat — no top-n id contains the forbidden substring.
	homeGreen := t.TempDir()
	seedGraph(t, homeGreen, fixtureNodesExcept("tool:revenuecat"))
	green, err := Run(Options{Home: homeGreen, Suite: suite(), BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run green: %v", err)
	}
	gc := caseByID(green, "f")
	if len(gc.ForbiddenViolations) != 0 {
		t.Fatalf("GREEN forbidden_violations = %v, want none", gc.ForbiddenViolations)
	}
	if green.Splits[SplitHeldIn].ForbiddenViolations != 0 {
		t.Fatalf("GREEN split forbidden total = %d, want 0", green.Splits[SplitHeldIn].ForbiddenViolations)
	}
	if len(green.AbsentFromGraph) != 0 {
		t.Fatalf("GREEN must not report absent (expected id still present): %v", green.AbsentFromGraph)
	}
}

// TestMustOutrankRedGreen is the relative-order falsifier: the winner ranked BELOW a
// loser-substring match is a violation (RED); a task that ranks the winner above the loser is
// clean (GREEN). Same graph both times; only the task ordering flips.
func TestMustOutrankRedGreen(t *testing.T) {
	mk := func(task string) Suite {
		return Suite{Suite: "outrank", Cases: []Case{{
			ID: "o", Kind: KindCapability, Task: task,
			Expected: []string{"skill:pricing"}, Split: SplitHeldOut, Lane: "graph",
			MustOutrank: []OutrankAssertion{{Winner: "skill:pricing", LoserContains: []string{"paywall"}}},
		}}}
	}
	home := t.TempDir()
	seedGraph(t, home, fixtureNodes())

	// RED: "paywall upgrade" surfaces skill:paywall (2 triggers) above skill:pricing (1 trigger).
	red, err := Run(Options{Home: home, Suite: mk("paywall upgrade"), BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run red: %v", err)
	}
	rc := caseByID(red, "o")
	if !contains(rc.OutrankViolations, "skill:pricing !> skill:paywall") {
		t.Fatalf("RED outrank_violations = %v, want winner-below-loser recorded", rc.OutrankViolations)
	}
	if red.Splits[SplitHeldOut].OutrankViolations != 1 {
		t.Fatalf("RED split outrank total = %d, want 1", red.Splits[SplitHeldOut].OutrankViolations)
	}

	// GREEN: "pricing monetization paywall" ranks skill:pricing (3 triggers) above skill:paywall (1).
	green, err := Run(Options{Home: home, Suite: mk("pricing monetization paywall"), BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run green: %v", err)
	}
	gc := caseByID(green, "o")
	if len(gc.OutrankViolations) != 0 {
		t.Fatalf("GREEN outrank_violations = %v, want none", gc.OutrankViolations)
	}
}

// TestMustOutrankWinnerAbsentIsParityGap is the winner-absent falsifier: a must_outrank winner
// that isn't in the graph must fold into absent_from_graph (the parity-gap mechanic that fails the
// --allow-absent gate) AND be recorded as a violation — never a silent pass.
func TestMustOutrankWinnerAbsentIsParityGap(t *testing.T) {
	home := t.TempDir()
	seedGraph(t, home, fixtureNodes())
	suite := Suite{Suite: "absent-winner", Cases: []Case{{
		ID: "w", Kind: KindCapability, Task: "pricing", Expected: []string{"skill:pricing"},
		Split: SplitHeldIn, Lane: "exact",
		MustOutrank: []OutrankAssertion{{Winner: "skill:ghost-winner", LoserContains: []string{"pricing"}}},
	}}}
	report, err := Run(Options{Home: home, Suite: suite, BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !contains(report.AbsentFromGraph, "skill:ghost-winner") {
		t.Fatalf("absent_from_graph = %v, want skill:ghost-winner folded in", report.AbsentFromGraph)
	}
	wc := caseByID(report, "w")
	if !contains(wc.OutrankViolations, "skill:ghost-winner absent_from_graph") {
		t.Fatalf("outrank_violations = %v, want absent winner recorded (not a silent pass)", wc.OutrankViolations)
	}
	if !report.HasAbsent() {
		t.Fatalf("HasAbsent() = false, want true so the --allow-absent gate fires on the absent winner")
	}
}

// TestCompareRegressesOnViolationIncrease falsifies the additive Compare rule: a split whose
// violation total INCREASES (hit@5/MRR unchanged) must be rejected; a decrease must not.
func TestCompareRegressesOnViolationIncrease(t *testing.T) {
	before := certificationRun(t, certificationSuite(), true, true)
	worse := cloneCertificationReport(t, before)
	worse.Cases[2].ForbiddenViolations = []string{"leak-a", "leak-b"}
	refreshCertificationMetrics(&worse)
	cmp := Compare(before, worse)
	if !cmp.Regressed || cmp.Accept {
		t.Fatalf("violation increase should reject: regressed=%v accept=%v", cmp.Regressed, cmp.Accept)
	}
	if !reflect.DeepEqual(cmp.RegressedSplit, []string{SplitHeldOut}) {
		t.Fatalf("regressed splits = %v, want [held_out]", cmp.RegressedSplit)
	}

	// Outrank-violation increase on held_in also regresses.
	worse2 := cloneCertificationReport(t, before)
	worse2.Cases[0].OutrankViolations = []string{"wrong order"}
	refreshCertificationMetrics(&worse2)
	if cmp := Compare(before, worse2); !cmp.Regressed || !reflect.DeepEqual(cmp.RegressedSplit, []string{SplitHeldIn}) {
		t.Fatalf("outrank increase on held_in should reject with held_in named, got regressed=%v splits=%v", cmp.Regressed, cmp.RegressedSplit)
	}

	// A violation DECREASE (with hits held) is an improvement — must accept.
	fewer := worse
	better := before
	if cmp := Compare(fewer, better); cmp.Regressed || !cmp.Accept {
		t.Fatalf("violation decrease should accept: regressed=%v accept=%v", cmp.Regressed, cmp.Accept)
	}
}

// TestBackwardCompatOldFixtureScoresIdentically is the no-schema-break falsifier: the existing
// 10-case capability fixture (which asserts NEITHER forbidden nor must_outrank) scores exactly as
// it did before the additive extension — hit@1/hit@5/MRR all 1.0, 5/5 splits, zero violations,
// nothing absent. If any additive field silently changed the positive scoring, this reds.
func TestBackwardCompatOldFixtureScoresIdentically(t *testing.T) {
	home := t.TempDir()
	seedGraph(t, home, fixtureNodes())
	suite, err := LoadSuite(filepath.Join("..", "..", "fixtures", "bench", "capability-suite.json"))
	if err != nil {
		t.Fatalf("load suite: %v", err)
	}
	report, err := Run(Options{Home: home, Suite: suite, BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Overall != (Metrics{Cases: 10, Hit1: 1.0, Hit5: 1.0, MRR: 1.0}) {
		t.Fatalf("overall = %+v, want {10,1,1,1} with zero violations (additive fields must default to 0)", report.Overall)
	}
	if report.Splits[SplitHeldIn] != (Metrics{Cases: 5, Hit1: 1.0, Hit5: 1.0, MRR: 1.0}) {
		t.Fatalf("held_in = %+v, want {5,1,1,1}", report.Splits[SplitHeldIn])
	}
	if report.Splits[SplitHeldOut] != (Metrics{Cases: 5, Hit1: 1.0, Hit5: 1.0, MRR: 1.0}) {
		t.Fatalf("held_out = %+v, want {5,1,1,1}", report.Splits[SplitHeldOut])
	}
	if len(report.AbsentFromGraph) != 0 {
		t.Fatalf("absent = %v, want none", report.AbsentFromGraph)
	}
	for _, c := range report.Cases {
		if len(c.ForbiddenViolations) != 0 || len(c.OutrankViolations) != 0 {
			t.Fatalf("case %s carries violations on a no-assertion fixture: %+v", c.ID, c)
		}
	}
}

// TestMemoryCaseSkippedWhenNoLedger proves graceful degradation: a memory case against a home
// with no memory ledger is marked skipped (with a reason), never a failed run.
func TestMemoryCaseSkippedWhenNoLedger(t *testing.T) {
	home := t.TempDir()
	seedGraph(t, home, fixtureNodes())
	suite := Suite{
		Suite: "mem",
		Cases: []Case{
			{ID: "mem-1", Kind: KindMemory, Task: "some recall task", Expected: []string{"claim-x"}, Split: SplitHeldIn, Lane: "exact"},
		},
	}
	report, err := Run(Options{Home: home, Suite: suite, BuildSHA: "test", Now: fixedNow()})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.SkippedCount != 1 || report.ScoredCount != 0 {
		t.Fatalf("skipped=%d scored=%d, want 1/0", report.SkippedCount, report.ScoredCount)
	}
	if len(report.Cases) != 1 || !report.Cases[0].Skipped || report.Cases[0].SkipReason == "" {
		t.Fatalf("case = %+v, want skipped with reason", report.Cases[0])
	}
	if len(report.AbsentFromGraph) != 0 {
		t.Fatalf("skipped memory case must not contribute absent ids, got %v", report.AbsentFromGraph)
	}
}

func refreshCertificationMetrics(r *Report) {
	r.Splits = map[string]Metrics{SplitHeldIn: aggregate(r.Cases, SplitHeldIn), SplitHeldOut: aggregate(r.Cases, SplitHeldOut)}
	r.Overall = aggregate(r.Cases, "")
}
