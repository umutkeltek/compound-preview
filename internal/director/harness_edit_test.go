package director

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/bench"
	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

// --- seeding helpers ---

func seedBenchReport(t *testing.T, home, suite string, absent []string, splits map[string]bench.Metrics) {
	t.Helper()
	report := bench.Report{
		Schema:          bench.SchemaVersion,
		Suite:           suite,
		Timestamp:       directorTestNow.UTC().Format(time.RFC3339),
		BuildSHA:        "testsha",
		GraphHash:       "testgraph",
		CaseCount:       1,
		ScoredCount:     1,
		Splits:          splits,
		AbsentFromGraph: absent,
		Cases:           []bench.CaseResult{},
	}
	dir := filepath.Join(home, "reviews", "bench")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	name := directorTestNow.UTC().Format("20060102T150405Z") + "-" + suite + ".json"
	if err := os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedNonActionableBenchReport writes a bench report explicitly marked actionable=false — the shape
// `bench run --no-actionable` produces for a synthetic/diagnostic suite — so a test can assert the
// director refuses to mine it.
func seedNonActionableBenchReport(t *testing.T, home, suite string, absent []string, splits map[string]bench.Metrics) {
	t.Helper()
	actionable := false
	report := bench.Report{
		Schema:          bench.SchemaVersion,
		Suite:           suite,
		SuiteKind:       "synthetic_fixture",
		Actionable:      &actionable,
		Timestamp:       directorTestNow.UTC().Format(time.RFC3339),
		BuildSHA:        "testsha",
		GraphHash:       "testgraph",
		CaseCount:       1,
		ScoredCount:     1,
		Splits:          splits,
		AbsentFromGraph: absent,
		Cases:           []bench.CaseResult{},
	}
	dir := filepath.Join(home, "reviews", "bench")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	name := directorTestNow.UTC().Format("20060102T150405Z") + "-" + suite + ".json"
	if err := os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedRouteRemorseReview(t *testing.T, home, id string) {
	t.Helper()
	review := routeevidence.RouteOutcomeReview{
		SchemaVersion: routeevidence.RouteOutcomeReviewSchemaVersion,
		ID:            id,
		CreatedAt:     directorTestNow.UTC().Format(time.RFC3339),
		Status:        "review_required",
		ReceiptID:     "rec_" + id,
		EvidenceRefs:  []string{"route_evidence:" + id},
		Assessment: routeevidence.RouteOutcomeAssessment{
			Class:              "route_remorse",
			ShouldReviewPolicy: true,
			Rationale:          "synthetic remorse",
		},
	}
	dir := filepath.Join(home, "reviews", "route-remorse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(review, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedUsageReceipt(t *testing.T, home, id string, offered, used, falseTriggered []string) {
	t.Helper()
	rec := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            "rec_" + id,
			CreatedAt:     directorTestNow.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: "work_" + id},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    "completed",
		MoveType:   "outward_delta",
		StateDelta: envelope.StateDelta{Changed: true, Summary: "synthetic", FilesChanged: []string{}, Artifacts: []string{}, Verification: []string{}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills: envelope.ReceiptSkills{
			Used:                used,
			Missed:              []string{},
			FalseTriggered:      falseTriggered,
			Candidate:           []string{},
			CapabilitiesOffered: offered,
		},
		Memory:    envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}, Rejected: []string{}},
		Next:      envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: []string{},
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("seed receipt invalid: %v", err)
	}
	dir := filepath.Join(home, "events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(&rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rec.ID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func harnessEditProposals(report Report) []Proposal {
	var out []Proposal
	for _, p := range report.Proposals {
		if p.Kind == "harness_edit" {
			out = append(out, p)
		}
	}
	return out
}

// --- Falsifier 1: the scanner emits harness_edit candidates carrying a target
// surface and non-empty evidence refs from all three seeded signals. ---
func TestHarnessEditScannerEmitsTargetedCandidatesFromEverySignal(t *testing.T) {
	home := seededDirectorHome(t)
	seedBenchReport(t, home, "capability-suite", []string{"capability://skill:missing-node"}, nil)
	seedRouteRemorseReview(t, home, "ror_synthetic_remorse")
	seedUsageReceipt(t, home, "cap01", []string{"capability://skill:example-helper"}, nil, nil)

	report, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	edits := harnessEditProposals(report)
	if len(edits) == 0 {
		t.Fatalf("expected harness_edit proposals, got none: %+v", report.Proposals)
	}

	surfaces := map[string]Proposal{}
	for _, p := range edits {
		if strings.TrimSpace(p.TargetSurface) == "" {
			t.Fatalf("harness_edit proposal missing target_surface: %+v", p)
		}
		if isHardDenyCoreSurface(p.TargetSurface) {
			t.Fatalf("harness_edit proposal must never target a hard-deny-core surface, got %q", p.TargetSurface)
		}
		if len(compactRefs(p.EvidenceRefs)) == 0 {
			t.Fatalf("harness_edit proposal must carry evidence refs: %+v", p)
		}
		surfaces[p.TargetSurface] = p
	}
	for _, want := range []string{"capability_edges", "skill_descriptions", "steering_config"} {
		if _, ok := surfaces[want]; !ok {
			t.Fatalf("expected a harness_edit proposal for surface %q, got surfaces %v", want, surfaceKeys(surfaces))
		}
	}
	// Each candidate cites its own signal source, not a generic marker.
	assertHasRef(t, surfaces["capability_edges"].EvidenceRefs, "bench_report:capability-suite")
	assertHasPrefix(t, surfaces["skill_descriptions"].EvidenceRefs, "capability_usage_never_fired:")
	assertHasRef(t, surfaces["steering_config"].EvidenceRefs, "route_remorse_review:ror_synthetic_remorse")
}

// TestHarnessEditIgnoresNonActionableBenchReports is the synthetic-fixture falsifier: a bench
// report marked actionable=false (synthetic_fixture / diagnostic) must NOT seed harness-edit
// candidates even when it reports absent graph nodes — fixture noise must not become suggested work.
// The same gap in an actionable report (the back-compat default) still seeds its candidate.
func TestHarnessEditIgnoresNonActionableBenchReports(t *testing.T) {
	home := seededDirectorHome(t)
	seedNonActionableBenchReport(t, home, "synthetic-fixture", []string{"capability://skill:fixture-gap"}, nil)

	report, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range harnessEditProposals(report) {
		if refsContain(p.EvidenceRefs, "bench_report:synthetic-fixture") {
			t.Fatalf("director mined a non-actionable (synthetic) bench report into a proposal: %+v", p)
		}
	}

	// Control: an actionable report carrying the same gap DOES seed an absent_from_graph candidate.
	seedBenchReport(t, home, "real-suite", []string{"capability://skill:real-gap"}, nil)
	report, err = Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if !anyProposalRefsContain(harnessEditProposals(report), "bench_report:real-suite") {
		t.Fatalf("actionable bench report must seed an absent_from_graph candidate")
	}
}

func refsContain(refs []string, needle string) bool {
	for _, r := range refs {
		if r == needle {
			return true
		}
	}
	return false
}

func anyProposalRefsContain(ps []Proposal, needle string) bool {
	for _, p := range ps {
		if refsContain(p.EvidenceRefs, needle) {
			return true
		}
	}
	return false
}

// --- Falsifier 2: the scanner REFUSES to construct a candidate targeting a
// hard-deny-core surface even when a signal tempts it (a misfiring eval/judge
// capability maps to the evaluator core surface); a non-core sibling signal in
// the same receipt still produces its candidate, proving the refusal is specific
// to core surfaces, not a blanket drop. ---
func TestHarnessEditRefusesHardDenyCoreSurfaceAtConstruction(t *testing.T) {
	home := seededDirectorHome(t)
	// eval:* tempts the evaluator core surface; tool:* is an ordinary edge.
	seedUsageReceipt(t, home, "tempt01", nil, nil, []string{
		"capability://eval:panel-judge",
		"capability://tool:example-linter",
	})

	cands, err := harnessEditCandidates(home, directorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	sawTool := false
	for _, c := range cands {
		if isHardDenyCoreSurface(c.TargetSurface) {
			t.Fatalf("scanner constructed a candidate targeting hard-deny-core surface %q: %+v", c.TargetSurface, c)
		}
		if c.SourceID == "capability_usage:false_triggered:capability://tool:example-linter" {
			sawTool = true
			if c.TargetSurface != "capability_edges" {
				t.Fatalf("non-core false trigger should target capability_edges, got %q", c.TargetSurface)
			}
		}
		if c.SourceID == "capability_usage:false_triggered:capability://eval:panel-judge" {
			t.Fatalf("evaluator-mapped signal must yield NO candidate, got %+v", c)
		}
	}
	if !sawTool {
		t.Fatalf("the non-core sibling signal should still produce its candidate; candidates=%+v", cands)
	}
}

// --- Falsifier 3 (companion to the boundary allowlist extension): a harness-edit
// --apply writes exactly one propose-only operator card under
// reviews/harness-edit-proposals/ and NOTHING escapes the extended allowlist. ---
func TestHarnessEditProposeApplyWritesCardConfined(t *testing.T) {
	home := seededDirectorHome(t)
	seedDirectorApplyGrant(t, home, directorTestNow)
	seedBenchReport(t, home, "capability-suite", []string{"capability://skill:missing-node"}, nil)

	before := snapshotHomeTree(t, home)
	report, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(harnessEditProposals(report)) == 0 {
		t.Fatalf("expected a harness_edit proposal to exercise the card surface: %+v", report.Proposals)
	}
	writeSet := diffHomeTree(before, snapshotHomeTree(t, home))

	allowedPrefixes := []string{
		"create-plans/",
		"projections/director/",
		"work/work/",
		"work/leases/",
		"work/action-contexts/",
		"events/",
		"reviews/harness-edit-proposals/", // the harness-edit operator card
		"run_attempts/",                   // C1: every claim mints a RunAttempt; harness-edit --apply claims, so a run-attempt write is legitimate, not an escape
		"lock/runattempts.lock",           // v0.1 compatibility fence for old and new RunAttempt writers
		"lock/runattempts/",               // validated per-attempt companions beneath the compatibility fence
		// DELIBERATE allowlist extension (REL-P0-004 adoption, 2026-09-04): the shared
		// work-admission lock (internal/home/work_admission_lock.go) pins one flocked
		// file per work id under this directory; like lock/runattempts/ it is a lock,
		// not a proposal surface.
		"lock/capability-lifecycle-work-admission/",
		// DELIBERATE allowlist extension (W5 J9-R3G, 2026-09-04): a starved-product
		// draft candidate is a parked product_hypothesis with a placeholder
		// expected_effect. The director mints exactly ONE per starved product in
		// apply mode; the draft is review-gated by construction (verdict=park,
		// BuildHypothesisPlan refuses to invent causality), never touches a
		// ProductSignal, and never invents a metric value.
		"inbox/product-hypotheses/",
	}
	var escaped, sawCard []string
	for _, rel := range writeSet {
		if !hasAllowedPrefix(rel, allowedPrefixes) {
			escaped = append(escaped, rel)
		}
		if strings.HasPrefix(rel, "reviews/harness-edit-proposals/") && strings.HasSuffix(rel, ".json") {
			sawCard = append(sawCard, rel)
		}
	}
	if len(escaped) > 0 {
		t.Fatalf("harness-edit --apply wrote OUTSIDE the allowlist: %v\nfull write set: %v", escaped, writeSet)
	}
	if len(sawCard) == 0 {
		t.Fatalf("expected an operator card under reviews/harness-edit-proposals/; write set: %v", writeSet)
	}

	// The written card carries a valid operator card + honest admission state.
	data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(sawCard[0])))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	card, ok := reviewartifact.OperatorCardFromMap(payload)
	if !ok {
		t.Fatalf("card json missing operator card fields: %s", string(data))
	}
	if err := reviewartifact.ValidateOperatorCard(card); err != nil {
		t.Fatalf("written card fails operator grammar: %v", err)
	}
	if payload["target_surface"] != "capability_edges" {
		t.Fatalf("card target_surface = %v, want capability_edges", payload["target_surface"])
	}
	// No whitelist installed in this home ⇒ admission is honestly reported blocked,
	// but the proposal is still emitted (state is information, not suppression).
	if payload["admission_status"] != "blocked_until_operator_whitelists_surface" {
		t.Fatalf("card admission_status = %v, want blocked_until_operator_whitelists_surface", payload["admission_status"])
	}
}

// --- Falsifier 4: a director-emitted harness-edit proposal, once its create-plan
// is admitted into a proposed WorkEnvelope, cannot AdmitAuto while its surface is
// unwhitelisted (fail-closed). Once whitelisted it reaches the next gate and is
// still refused because the first evidence-autonomy grant is job_health-only.
// Red without TargetSurface threading: an untargeted work would fail-open. ---
func TestHarnessEditProposalGatedByEditableSurfacesWhitelist(t *testing.T) {
	home := seededDirectorHome(t)
	seedBenchReport(t, home, "capability-suite", []string{"capability://skill:missing-node"}, nil)

	store, workID, surface := applyAndAdmitHarnessEditProposal(t, home)
	w, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if w.TargetSurface != surface || surface == "" {
		t.Fatalf("admitted harness-edit work must carry target_surface %q, got %q", surface, w.TargetSurface)
	}

	eligible := work.AutoAdmitInput{By: "director"}

	// Registry absent ⇒ fail-closed deny.
	res, err := store.AdmitAuto(workID, eligible, directorTestNow.Add(2*time.Minute))
	if err == nil || res.Admitted {
		t.Fatalf("unwhitelisted harness-edit proposal must be denied: err=%v res=%+v", err, res)
	}
	if !strings.Contains(err.Error(), "editable surface denied") {
		t.Fatalf("denial must name the editable-surfaces gate, got %v", err)
	}
	if got, _ := store.Get(workID); got.State != "proposed" {
		t.Fatalf("denied harness-edit proposal must stay proposed, got %q", got.State)
	}

	// Operator whitelists the surface ⇒ the editable-surface denial clears, but
	// harness_edit still has no enabled class-scoped autonomy slice.
	if err := policy.WriteEditableSurfaces(home, policy.EditableSurfaces{Entries: []policy.EditableSurface{
		{Surface: surface, ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusActive},
	}}); err != nil {
		t.Fatal(err)
	}
	res, err = store.AdmitAuto(workID, eligible, directorTestNow.Add(3*time.Minute))
	if err == nil || res.Admitted || !strings.Contains(err.Error(), "no enabled evidence-autonomy slice") {
		t.Fatalf("whitelisted harness-edit must stop at the class gate: err=%v res=%+v", err, res)
	}
	if got, _ := store.Get(workID); got.State != "proposed" {
		t.Fatalf("harness-edit work should remain proposed, got %q", got.State)
	}
}

// --- Falsifier 5: the kill-switch halts admission of a director harness-edit
// proposal even WHEN its surface is actively whitelisted (halt wins). ---
func TestHarnessEditProposalHaltWinsOverWhitelist(t *testing.T) {
	home := seededDirectorHome(t)
	seedBenchReport(t, home, "capability-suite", []string{"capability://skill:missing-node"}, nil)

	store, workID, surface := applyAndAdmitHarnessEditProposal(t, home)

	// Surface actively whitelisted...
	if err := policy.WriteEditableSurfaces(home, policy.EditableSurfaces{Entries: []policy.EditableSurface{
		{Surface: surface, ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusActive},
	}}); err != nil {
		t.Fatal(err)
	}
	// ...but the global autonomy halt is engaged.
	if err := policy.WriteAutonomyHalt(home, policy.AutonomyHalt{Global: true, Reason: "incident review"}); err != nil {
		t.Fatal(err)
	}

	res, err := store.AdmitAuto(workID, work.AutoAdmitInput{By: "director"}, directorTestNow.Add(2*time.Minute))
	if err == nil || res.Admitted {
		t.Fatalf("halt must deny even a whitelisted harness-edit proposal: err=%v res=%+v", err, res)
	}
	if !res.Halted || !strings.Contains(err.Error(), "autonomy halted") {
		t.Fatalf("the halt (not the surface gate) must be the denial reason: halted=%t err=%v", res.Halted, err)
	}
	if got, _ := store.Get(workID); got.State != "proposed" {
		t.Fatalf("halted harness-edit proposal must stay proposed, got %q", got.State)
	}
}

// --- Falsifier 7: a real harness-edit operator card passes the grammar, and a
// card the writer cannot fill (empty template hook) FAILS rather than emitting
// mush. ---
func TestHarnessEditOperatorCardGrammar(t *testing.T) {
	good := harnessEditProposalCard{
		TargetSurface: "capability_edges",
		SurfaceLabel:  reviewartifact.PlainLabel("capability_edges"),
		EditAction:    "trim the never-fired example helper offer",
		EditSignal:    "it was offered 12 times and never fired",
		Consequence:   "the capability edges surface drops an offer nothing uses",
	}
	card, err := good.ReviewOperatorCard()
	if err != nil {
		t.Fatalf("real harness-edit card should build: %v", err)
	}
	if err := reviewartifact.ValidateOperatorCard(card); err != nil {
		t.Fatalf("harness-edit card fails operator grammar: %v", err)
	}
	if !strings.HasSuffix(card.Summary, "?") {
		t.Fatalf("decision card must end with '?': %q", card.Summary)
	}
	if !strings.HasPrefix(card.GroupKey, harnessEditReviewArea+"|") {
		t.Fatalf("group key must be area-scoped: %q", card.GroupKey)
	}

	for _, name := range []string{"empty_action", "empty_signal"} {
		bad := good
		if name == "empty_action" {
			bad.EditAction = ""
		} else {
			bad.EditSignal = ""
		}
		if _, err := bad.ReviewOperatorCard(); err == nil {
			t.Fatalf("%s: card writer must fail when a template hook is empty, not emit mush", name)
		}
	}
}

// --- Falsifier 8: the harness-edit scanner is deterministic — the same seeded
// home yields an identical proposal set across runs. ---
func TestHarnessEditProposalsAreDeterministic(t *testing.T) {
	home := seededDirectorHome(t)
	seedBenchReport(t, home, "capability-suite", []string{"capability://skill:missing-node", "capability://tool:absent-tool"}, map[string]bench.Metrics{
		bench.SplitHeldIn:  {Cases: 4, Hit1: 0.25},
		bench.SplitHeldOut: {Cases: 4, Hit1: 0.75},
	})
	seedRouteRemorseReview(t, home, "ror_synthetic_remorse")
	seedUsageReceipt(t, home, "cap01", []string{"capability://skill:example-helper"}, nil, nil)
	run := func() Report {
		report, err := Propose(home, Options{Now: directorTestNow})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	a := harnessEditProposals(run())
	b := harnessEditProposals(run())
	if len(a) == 0 {
		t.Fatalf("expected harness_edit proposals to compare")
	}
	if len(a) != len(b) {
		t.Fatalf("nondeterministic proposal count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].TargetSurface != b[i].TargetSurface {
			t.Fatalf("proposal %d differs: %+v vs %+v", i, a[i], b[i])
		}
		if strings.Join(a[i].EvidenceRefs, "|") != strings.Join(b[i].EvidenceRefs, "|") {
			t.Fatalf("proposal %d evidence refs differ: %v vs %v", i, a[i].EvidenceRefs, b[i].EvidenceRefs)
		}
	}
}

// applyAndAdmitHarnessEditProposal runs a --apply propose, admits the emitted
// harness-edit proposal's create-plan into a proposed WorkEnvelope (operator
// provenance), and returns the store, work id, and target surface.
func applyAndAdmitHarnessEditProposal(t *testing.T, home string) (*work.Store, string, string) {
	t.Helper()
	seedDirectorApplyGrant(t, home, directorTestNow)
	report, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	prop := findProposalByKind(report, "harness_edit")
	if prop == nil {
		t.Fatalf("no harness_edit proposal emitted: %+v", report.Proposals)
	}
	if prop.TargetSurface == "" || prop.PlanPath == "" {
		t.Fatalf("harness-edit proposal missing surface/plan: %+v", prop)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := store.AdmitCreatePlanPath(prop.PlanPath, directorTestNow.Add(time.Minute), work.AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.WorkState != "proposed" {
		t.Fatalf("admitted harness-edit work should be proposed, got %q", admitted.WorkState)
	}
	return store, admitted.WorkID, prop.TargetSurface
}

func surfaceKeys(m map[string]Proposal) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
