package main

// Experiential smoke — a seeded persona replay run against an ISOLATED,
// disposable COMPOUND_HOME. It reads the SAME projection the operator console
// reads (`compound attention` composed from reviews + workbench + executors),
// measures typed friction against a stored baseline (interaction cost,
// belief-vs-ledger diff, comprehension), and — only when a dimension regresses
// — emits ONE review card through the existing governed review-finding door
// (recordReviewFinding). No regression means silence (the default).
//
// This is the smallest full vertical slice of the standing "experiential smoke"
// circuit (WorkEnvelope work_047673ca0a83). What is proven end-to-end here:
//
//   1. a regression-seeded replay emits a review card that NAMES the regression;
//   2. a no-regression replay emits NO card (silence default);
//   3. the replay never mutates the real COMPOUND_HOME — seed, projection and
//      grading all happen in the disposable replay home; the only real-home
//      write is the deliberate governed review card under --emit.
//
// EXPANSION PATH (documented, not built here):
//   - Standing trigger: call this verb from the post-land path (a proof-check
//     step or a land hook) so every console land runs the replay. The verb is
//     already idempotent and self-contained; wiring is one step.
//   - Richer persona: the seed is a fixed 3-item scenario. Grow it toward the
//     playtest seed-manifest (divergence intake cards, observation candidates,
//     ghost-held work) to exercise more of the surface.
//   - Real medium: today the replay drives the CLI projection layer (the honest,
//     browser-free data plane the console consumes). Swap in the Next.js route
//     handlers (console/src/app/api/*) or a real browser only for friction
//     classes that are specifically about DOM/CSS rendering.
//   - Autonomous fix-work: on regression, auto-file proposed diagnosis-tier fix
//     work under a `ux_friction` autonomy grant (job_health auto-admit pattern
//     generalized). The grant-check is real (director.HasActiveAutonomyGrant);
//     the AdmitAuto call is STUBBED — see the TODO in runExperientialSmoke.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/attention"
	"compound/internal/director"
	"compound/internal/home"
	"compound/internal/work"
)

const (
	experientialSmokeSchemaVersion         = "compound.experiential_smoke.v0.1"
	experientialSmokeBaselineSchemaVersion = "compound.experiential_smoke_baseline.v0.1"
	experientialSmokeDetailSchemaVersion   = "compound.experiential_smoke_detail.v0.1"
	experientialSmokeSeedProfile           = "baseline-persona"
	// uxFrictionAutoAdmitClass mirrors the job_health auto-admit class the
	// director already runs, scoped for autonomously filed UX-friction fix-work.
	uxFrictionAutoAdmitClass = "ux_friction"
	experientialSmokeWorkID  = "work_047673ca0a83"
)

const experientialSmokeUsage = "usage: compound experiential-smoke [--json] [--emit] [--inject-regression] [--write-baseline] [--baseline PATH] [--root DIR]"

// frictionMetrics are the graded dimensions of one persona replay. Higher is
// worse on every axis, so a regression is "current > baseline" per dimension.
type frictionMetrics struct {
	InteractionCost     int `json:"interaction_cost"`     // distinct attention surfaces the persona must triage
	BeliefLedgerDiff    int `json:"belief_ledger_diff"`   // items where surface-implied attention != ledger-truth open work (symmetric)
	ComprehensionMisses int `json:"comprehension_misses"` // surfaced groups with an illegible (blank) operator summary
}

// experientialSmokeBaseline is the stored "previous build" the delta is graded
// against. Regenerate with `compound experiential-smoke --write-baseline`.
type experientialSmokeBaseline struct {
	SchemaVersion string          `json:"schema_version"`
	SeedProfile   string          `json:"seed_profile"`
	GeneratedAt   string          `json:"generated_at"`
	Metrics       frictionMetrics `json:"metrics"`
	Note          string          `json:"note,omitempty"`
}

// frictionReceipt is a typed friction record, shaped like the console-playtest
// frictions (kind/severity/page/expected/observed/interactions_spent) so the
// two circuits stay legible to the same reader.
type frictionReceipt struct {
	Kind              string   `json:"kind"`
	Severity          string   `json:"severity"`
	Page              string   `json:"page"`
	Dimension         string   `json:"dimension"`
	Expected          string   `json:"expected"`
	Observed          string   `json:"observed"`
	InteractionsSpent int      `json:"interactions_spent"`
	Repro             []string `json:"repro"`
}

type dimensionDelta struct {
	Dimension string `json:"dimension"`
	Baseline  int    `json:"baseline"`
	Current   int    `json:"current"`
	Delta     int    `json:"delta"`
}

type experientialSmokeReport struct {
	SchemaVersion         string               `json:"schema_version"`
	GeneratedAt           string               `json:"generated_at"`
	ReplayHome            string               `json:"replay_home"`
	SeedProfile           string               `json:"seed_profile"`
	InjectRegression      bool                 `json:"inject_regression"`
	Baseline              frictionMetrics      `json:"baseline"`
	Current               frictionMetrics      `json:"current"`
	Regressed             bool                 `json:"regressed"`
	RegressedDims         []dimensionDelta     `json:"regressed_dimensions,omitempty"`
	Decision              string               `json:"decision"` // silent | baseline-written | regression-detected | review-card-emitted
	Receipts              []frictionReceipt    `json:"receipts,omitempty"`
	RunDir                string               `json:"run_dir,omitempty"`
	ReviewFinding         *reviewFindingRecord `json:"review_finding,omitempty"`
	UxFrictionGrantActive bool                 `json:"ux_friction_grant_active"`
	AutoFileStub          string               `json:"auto_file_stub,omitempty"`
	Warnings              []string             `json:"warnings,omitempty"`
}

type experientialSmokeOptions struct {
	Root             string
	BaselinePath     string
	Emit             bool
	InjectRegression bool
	WriteBaseline    bool
	Now              time.Time
}

func cmdExperientialSmoke(args []string) {
	if hasUsageFlag(args) {
		fmt.Println(experientialSmokeUsage)
		return
	}
	opts := experientialSmokeOptions{
		Root:             flagValue(args, "--root"),
		BaselinePath:     flagValue(args, "--baseline"),
		Emit:             hasFlag(args, "--emit"),
		InjectRegression: hasFlag(args, "--inject-regression"),
		WriteBaseline:    hasFlag(args, "--write-baseline"),
		Now:              time.Now(),
	}
	report, err := runExperientialSmoke(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "experiential-smoke:", err)
		os.Exit(1)
	}
	if hasFlag(args, "--json") {
		printJSON(report)
	} else {
		printExperientialSmoke(report)
	}
	// A regression that filed its card did its job (exit 0). A regression left
	// unfiled (--emit omitted) exits 3 so a caller notices the un-actioned delta.
	if report.Regressed && report.Decision == "regression-detected" {
		os.Exit(3)
	}
}

func runExperientialSmoke(opts experientialSmokeOptions) (experientialSmokeReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return experientialSmokeReport{}, err
		}
		root = cwd
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return experientialSmokeReport{}, err
	}

	// 1. Isolated, disposable replay home — the "isolated instance".
	replayHome, err := os.MkdirTemp("", "compound-experiential-smoke-home-*")
	if err != nil {
		return experientialSmokeReport{}, err
	}
	defer os.RemoveAll(replayHome)
	if err := home.Init(replayHome, "experiential-smoke", now); err != nil {
		return experientialSmokeReport{}, fmt.Errorf("initialize replay home: %w", err)
	}
	store, err := work.Open(replayHome)
	if err != nil {
		return experientialSmokeReport{}, err
	}

	// 2. Seed the fixed persona scenario.
	if err := seedExperientialSmokeScenario(store, now); err != nil {
		return experientialSmokeReport{}, fmt.Errorf("seed replay scenario: %w", err)
	}

	// 3. Build the persona-belief surface — the SAME projection the console reads.
	surface, err := buildExperientialSmokeSurface(replayHome, store, now)
	if err != nil {
		return experientialSmokeReport{}, err
	}
	if opts.InjectRegression {
		surface = injectSurfaceRegression(surface)
	}

	// 4. Measure typed friction of the surface vs the ledger truth.
	current, err := measureFriction(surface, store)
	if err != nil {
		return experientialSmokeReport{}, err
	}

	report := experientialSmokeReport{
		SchemaVersion:    experientialSmokeSchemaVersion,
		GeneratedAt:      now.UTC().Format(time.RFC3339),
		ReplayHome:       replayHome,
		SeedProfile:      experientialSmokeSeedProfile,
		InjectRegression: opts.InjectRegression,
		Current:          current,
	}

	baselinePath := strings.TrimSpace(opts.BaselinePath)
	if baselinePath == "" {
		baselinePath = filepath.Join(root, "fixtures", "experiential-smoke", "baseline.json")
	}

	// 5. Baseline-writing mode: capture the current clean metrics as the baseline.
	if opts.WriteBaseline {
		b := experientialSmokeBaseline{
			SchemaVersion: experientialSmokeBaselineSchemaVersion,
			SeedProfile:   experientialSmokeSeedProfile,
			GeneratedAt:   now.UTC().Format(time.RFC3339),
			Metrics:       current,
			Note:          "regenerate with `compound experiential-smoke --write-baseline`; metrics are timestamp-independent counts",
		}
		if err := writeExperientialJSONFile(baselinePath, b); err != nil {
			return report, err
		}
		report.Baseline = current
		report.Decision = "baseline-written"
		return report, nil
	}

	// 6. Load the stored baseline (the "previous build").
	baseline, err := loadExperientialSmokeBaseline(baselinePath)
	if err != nil {
		return report, fmt.Errorf("load baseline %s (run `compound experiential-smoke --write-baseline` first): %w", baselinePath, err)
	}
	report.Baseline = baseline.Metrics

	// 7. Grade the delta.
	regressed, dims := gradeFriction(current, baseline.Metrics)
	report.Regressed = regressed
	report.RegressedDims = dims

	// 8. Silence is the default: no regression means no card, no real-home write.
	if !regressed {
		report.Decision = "silent"
		report.Warnings = append(report.Warnings, "no regression vs baseline; no review card emitted (silence default); real home untouched")
		return report, nil
	}

	// 9. Regression: write typed friction evidence under state/runs (the repo,
	//    NOT the real COMPOUND_HOME). This path never touches the real home.
	receipts := frictionReceiptsFor(dims, current)
	report.Receipts = receipts
	stamp := now.UTC().Format("20060102T150405Z")
	runDir := filepath.ToSlash(filepath.Join("state", "runs", "experiential-smoke-"+stamp))
	runDirAbs := filepath.Join(root, filepath.FromSlash(runDir))
	if err := os.MkdirAll(runDirAbs, 0o755); err != nil {
		return report, err
	}
	report.RunDir = runDir
	if err := writeFrictionsJSONL(filepath.Join(runDirAbs, "playtest-frictions.jsonl"), receipts); err != nil {
		return report, err
	}
	detailRel := filepath.ToSlash(filepath.Join(runDir, "playtest-detail.json"))
	detail := map[string]any{
		"schema_version":       experientialSmokeDetailSchemaVersion,
		"generated_at":         report.GeneratedAt,
		"seed_profile":         report.SeedProfile,
		"inject_regression":    opts.InjectRegression,
		"baseline":             baseline.Metrics,
		"current":              current,
		"regressed_dimensions": dims,
		"receipts":             receipts,
	}
	if err := writeExperientialJSONFile(filepath.Join(runDirAbs, "playtest-detail.json"), detail); err != nil {
		return report, err
	}

	summary := experientialSmokeSummary(dims)

	// 10. Emit ONE governed review card into the REAL home — the only real-home
	//     mutation, and only under --emit.
	if !opts.Emit {
		report.Decision = "regression-detected"
		report.Warnings = append(report.Warnings, "regression detected but --emit not set; no review card emitted; real home untouched")
		return report, nil
	}

	frictionsRef := "frictions:" + filepath.ToSlash(filepath.Join(runDir, "playtest-frictions.jsonl"))
	rec, err := recordReviewFinding(recordReviewFindingInput{
		AutoWork:            true,
		TargetPath:          filepath.ToSlash(filepath.Join(runDir, "finding.json")),
		Summary:             summary,
		Root:                root,
		SourceArtifactPaths: []string{detailRel},
		EvidenceRefs:        []string{frictionsRef, "experiential-smoke-circuit:" + experientialSmokeWorkID},
	}, now)
	if err != nil {
		return report, fmt.Errorf("emit review card through governed door: %w", err)
	}
	report.ReviewFinding = &rec
	report.Decision = "review-card-emitted"

	// 11. Grant-check for autonomous fix-work (job_health auto-admit generalized).
	//     The grant-check is real; the AdmitAuto call is STUBBED.
	//
	// TODO(work_047673ca0a83): when an active `ux_friction` autonomy grant is
	// present, autonomously file ONE proposed diagnosis-tier fix WorkEnvelope
	// (CapabilityClass "judge-critique", read-only "diagnose, don't mutate"
	// predicate, EvidenceRefs carrying "director_trigger:ux_friction:<digest>")
	// and admit it via store.AdmitAuto with a compiled eligibility proof —
	// mirroring internal/director evaluateJobHealthEvidenceAutonomy. This is
	// NOT wired yet because internal/work/auto_admit.go hard-blocks any class
	// other than "job_health" (auto_admit.go class gate) and no
	// uxFrictionCandidates scanner exists. Widen that literal (and the parallel
	// autoAdmitCapableClasses list in internal/decisions/grant_mint.go) in
	// lockstep, then file+admit here.
	if realHome, herr := home.Resolve(""); herr == nil {
		if active, gerr := director.HasActiveAutonomyGrant(realHome, uxFrictionAutoAdmitClass); gerr == nil {
			report.UxFrictionGrantActive = active
		}
	}
	report.AutoFileStub = "STUB(" + experientialSmokeWorkID + "): autonomous ux_friction fix-work is gated behind director.HasActiveAutonomyGrant + store.AdmitAuto; not filed (auto_admit.go blocks class != job_health, no uxFrictionCandidates scanner). Review card is the shipped output."

	return report, nil
}

// seedExperientialSmokeScenario writes a fixed, deterministic persona scenario:
// three legible proposed work envelopes awaiting an operator admit/park call.
// Deterministic WorkIDs keep the replay reproducible so metrics are stable.
func seedExperientialSmokeScenario(store *work.Store, now time.Time) error {
	seeds := []work.CreateInput{
		{
			WorkID:           "work_expsmoke_alpha",
			Owner:            "experiential-smoke-persona",
			State:            "proposed",
			SuccessPredicate: "Admit or park the alpha export-crash remediation proposal.",
			DesiredOutcome:   "operator admits or parks a legible remediation predicate",
			EvidenceRefs:     []string{"seed://experiential-smoke/alpha"},
			Risk:             "tier1",
			Privacy:          "private",
		},
		{
			WorkID:           "work_expsmoke_beta",
			Owner:            "experiential-smoke-persona",
			State:            "proposed",
			SuccessPredicate: "Admit or park the beta sync-diagnostic proposal.",
			DesiredOutcome:   "operator admits or parks a legible diagnostic predicate",
			EvidenceRefs:     []string{"seed://experiential-smoke/beta"},
			Risk:             "tier1",
			Privacy:          "private",
		},
		{
			WorkID:           "work_expsmoke_gamma",
			Owner:            "experiential-smoke-persona",
			State:            "proposed",
			SuccessPredicate: "Admit or park the gamma routing-policy proposal.",
			DesiredOutcome:   "operator admits or parks a legible routing predicate",
			EvidenceRefs:     []string{"seed://experiential-smoke/gamma"},
			Risk:             "tier1",
			Privacy:          "private",
		},
	}
	for _, in := range seeds {
		if _, err := store.Create(in, now); err != nil {
			return err
		}
	}
	return nil
}

// buildExperientialSmokeSurface composes the exact projection `compound
// attention` composes (reviews + workbench + executors) — the single canonical
// "what needs the persona" surface the console reads.
func buildExperientialSmokeSurface(homePath string, store *work.Store, now time.Time) (attention.Report, error) {
	reviews, err := buildReviewQueueReport(homePath, "", false)
	if err != nil {
		return attention.Report{}, err
	}
	workbench, err := store.Workbench(now, work.WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		return attention.Report{}, err
	}
	executors, err := store.ExecutorsProjection(now)
	if err != nil {
		return attention.Report{}, err
	}
	return attention.Build(attention.Inputs{
		Reviews:   &reviews,
		Workbench: &workbench,
		Executors: &executors,
	}, now), nil
}

// injectSurfaceRegression models a regressed console build for the falsifier /
// self-test path (mirrors proof-check's synthetic placement rules). It appends
// ONE illegible, phantom needs-attention group: a blank operator summary (a
// comprehension miss) referencing an item ID with no ledger backing (a belief
// divergence), which also raises the interaction cost by one surface. In
// production the injector is NEVER applied — the real projection is graded
// against the stored baseline, so a genuine regression in attention.Build (a
// dropped summary, a phantom counter) trips these same three dimensions.
func injectSurfaceRegression(r attention.Report) attention.Report {
	r.Groups = append(r.Groups, attention.Group{
		Key:             "experiential-smoke:injected-regression",
		Area:            "experiential-smoke-regression",
		Band:            attention.BandNeedsYou,
		Source:          "workbench",
		Kind:            "gate_admit",
		Count:           1,
		OperatorSummary: "",                                              // illegible surface → comprehension miss
		ItemIDs:         []string{"work_expsmoke_phantom_not_in_ledger"}, // phantom belief
	})
	return r
}

// measureFriction computes the three graded dimensions from the persona-belief
// surface and the ledger truth in the replay home.
func measureFriction(surface attention.Report, store *work.Store) (frictionMetrics, error) {
	// interaction cost: every surfaced group is one unit the persona must triage.
	interactionCost := len(surface.Groups)

	beliefIDs := map[string]bool{}
	comprehensionMisses := 0
	for _, g := range surface.Groups {
		if strings.TrimSpace(g.OperatorSummary) == "" {
			comprehensionMisses++
		}
		for _, id := range g.ItemIDs {
			if strings.TrimSpace(id) != "" {
				beliefIDs[id] = true
			}
		}
	}

	all, err := store.List()
	if err != nil {
		return frictionMetrics{}, err
	}
	ledgerOpen := map[string]bool{}
	for _, w := range all {
		if !isTerminalWorkState(w.State) {
			ledgerOpen[w.ID] = true
		}
	}

	// belief-vs-ledger diff is the symmetric difference: phantom items the
	// surface implies but the ledger does not hold, plus open ledger items the
	// surface never surfaces (hidden). Both are real friction classes.
	diff := 0
	for id := range beliefIDs {
		if !ledgerOpen[id] {
			diff++
		}
	}
	for id := range ledgerOpen {
		if !beliefIDs[id] {
			diff++
		}
	}

	return frictionMetrics{
		InteractionCost:     interactionCost,
		BeliefLedgerDiff:    diff,
		ComprehensionMisses: comprehensionMisses,
	}, nil
}

func isTerminalWorkState(state string) bool {
	switch strings.TrimSpace(state) {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// gradeFriction reports which dimensions got strictly worse than baseline.
func gradeFriction(current, baseline frictionMetrics) (bool, []dimensionDelta) {
	var regressed []dimensionDelta
	add := func(dim string, base, cur int) {
		if cur > base {
			regressed = append(regressed, dimensionDelta{Dimension: dim, Baseline: base, Current: cur, Delta: cur - base})
		}
	}
	add("interaction_cost", baseline.InteractionCost, current.InteractionCost)
	add("belief_ledger_diff", baseline.BeliefLedgerDiff, current.BeliefLedgerDiff)
	add("comprehension_misses", baseline.ComprehensionMisses, current.ComprehensionMisses)
	return len(regressed) > 0, regressed
}

func frictionReceiptsFor(dims []dimensionDelta, current frictionMetrics) []frictionReceipt {
	out := make([]frictionReceipt, 0, len(dims))
	for _, d := range dims {
		var kind, expected, observed, severity string
		switch d.Dimension {
		case "interaction_cost":
			kind = "interaction_cost_regression"
			severity = "P1"
			expected = fmt.Sprintf("persona clears the seeded board in <= %d attention surfaces (baseline)", d.Baseline)
			observed = fmt.Sprintf("%d attention surfaces demanded triage (+%d vs baseline)", d.Current, d.Delta)
		case "belief_ledger_diff":
			kind = "belief_ledger_divergence"
			severity = "P0"
			expected = "the surface's needs-attention items reconcile 1:1 with the ledger's open work"
			observed = fmt.Sprintf("%d item(s) diverge between what the surface implies and what the ledger holds (+%d vs baseline)", d.Current, d.Delta)
		case "comprehension_misses":
			kind = "comprehension_miss"
			severity = "P1"
			expected = "every needs-attention surface carries a legible operator summary"
			observed = fmt.Sprintf("%d surface(s) rendered an illegible/blank summary (+%d vs baseline)", d.Current, d.Delta)
		default:
			kind = d.Dimension + "_regression"
			severity = "P1"
			expected = fmt.Sprintf("%s at baseline %d", d.Dimension, d.Baseline)
			observed = fmt.Sprintf("%s at %d (+%d)", d.Dimension, d.Current, d.Delta)
		}
		out = append(out, frictionReceipt{
			Kind:              kind,
			Severity:          severity,
			Page:              "/",
			Dimension:         d.Dimension,
			Expected:          expected,
			Observed:          observed,
			InteractionsSpent: current.InteractionCost,
			Repro: []string{
				"compound experiential-smoke --json",
				"seed profile: " + experientialSmokeSeedProfile + " (fixed)",
				"graded vs fixtures/experiential-smoke/baseline.json",
			},
		})
	}
	return out
}

func experientialSmokeSummary(dims []dimensionDelta) string {
	parts := make([]string, 0, len(dims))
	for _, d := range dims {
		parts = append(parts, fmt.Sprintf("%s %d→%d (+%d)", d.Dimension, d.Baseline, d.Current, d.Delta))
	}
	return "Experiential smoke: seeded persona replay regressed vs baseline on " + strings.Join(parts, ", ") +
		". Isolated disposable replay home; typed friction receipts attached. Diagnose the console surface that widened the gap between what the persona believes and what the ledger holds."
}

func loadExperientialSmokeBaseline(path string) (experientialSmokeBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return experientialSmokeBaseline{}, err
	}
	var b experientialSmokeBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		return experientialSmokeBaseline{}, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	return b, nil
}

func writeExperientialJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func writeFrictionsJSONL(path string, receipts []frictionReceipt) error {
	var b strings.Builder
	for _, r := range receipts {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func printExperientialSmoke(r experientialSmokeReport) {
	fmt.Printf("# experiential-smoke\n")
	fmt.Printf("- decision: %s\n", r.Decision)
	fmt.Printf("- seed_profile: %s (inject_regression=%t)\n", r.SeedProfile, r.InjectRegression)
	fmt.Printf("- baseline: interaction_cost=%d belief_ledger_diff=%d comprehension_misses=%d\n",
		r.Baseline.InteractionCost, r.Baseline.BeliefLedgerDiff, r.Baseline.ComprehensionMisses)
	fmt.Printf("- current:  interaction_cost=%d belief_ledger_diff=%d comprehension_misses=%d\n",
		r.Current.InteractionCost, r.Current.BeliefLedgerDiff, r.Current.ComprehensionMisses)
	fmt.Printf("- regressed: %t\n", r.Regressed)
	for _, d := range r.RegressedDims {
		fmt.Printf("  - %s %d→%d (+%d)\n", d.Dimension, d.Baseline, d.Current, d.Delta)
	}
	if r.RunDir != "" {
		fmt.Printf("- run_dir: %s\n", r.RunDir)
	}
	if r.ReviewFinding != nil {
		fmt.Printf("- review_card: %s target=%s work=%s\n", r.ReviewFinding.ID, r.ReviewFinding.TargetPath, r.ReviewFinding.WorkID)
	}
	if r.Decision == "review-card-emitted" {
		fmt.Printf("- ux_friction_grant_active: %t\n", r.UxFrictionGrantActive)
	}
	if r.AutoFileStub != "" {
		fmt.Printf("- auto_file: %s\n", r.AutoFileStub)
	}
	for _, w := range r.Warnings {
		fmt.Printf("- warning: %s\n", w)
	}
}
