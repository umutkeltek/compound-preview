package panel

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/routeevidence"
)

func TestBuildPlanTargetsLensesLanesCountsAndPrivacyOverride(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	writePanelFixtureTarget(t, home, "mcand_example_api_dissent")

	run, err := BuildPlan(home, PlanInput{TargetKind: "memory_candidate", TargetRefs: []string{"mcand_example_api_clean", "mcand_example_api_dissent"}, States: []string{"parked"}, Limit: 2, PrivacyOverrideReason: "synthetic fixtures", Budget: Budget{MaxItems: 2}}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if run.Status != "planned" || run.TargetKind != "memory_candidate" || len(run.Targets) != 2 {
		t.Fatalf("run plan = %+v", run)
	}
	if got := run.CountsBefore["parked"]; got != 2 {
		t.Fatalf("parked count = %d, want 2", got)
	}
	if len(run.LensSet.Included) != 6 || run.Route.LensLane.ModelID != "panel/free-extract" || run.Route.JudgeLane.ModelID != "panel/cheap-judge" {
		t.Fatalf("lenses/lanes not populated: lenses=%d route=%+v", len(run.LensSet.Included), run.Route)
	}
	if !containsString(run.EvidenceRefs, "override:privacy_not_sensitive:synthetic fixtures") {
		t.Fatalf("override evidence refs = %+v", run.EvidenceRefs)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRun(run.ID)
	if err != nil {
		t.Fatalf("load saved run: %v", err)
	}
	if loaded.ID != run.ID || loaded.Status != "planned" {
		t.Fatalf("saved run = %+v", loaded)
	}
}

func TestBuildPlanBlocksWhenNoLensesIncluded(t *testing.T) {
	home := t.TempDir()
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml"), panelTestNow); err != nil {
		t.Fatalf("import model capabilities: %v", err)
	}
	writeComparisonFixtureForPanelTest(t, home, "cmp_candidate_better")
	run, err := BuildPlan(home, PlanInput{TargetKind: "comparison", TargetRefs: []string{"cmp_candidate_better"}}, fixedPanelNow)
	var blocked ErrBlockedNoLenses
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want blocked no lenses", err)
	}
	if blocked.TargetKind != "comparison" {
		t.Fatalf("blocked target kind = %q", blocked.TargetKind)
	}
	if run.Status != "blocked" || run.StoppedReason != "blocked_no_lenses:comparison" || len(run.LensSet.Included) != 0 {
		t.Fatalf("blocked no-lenses run not surfaced: %+v", run)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRun(run.ID)
	if err != nil {
		t.Fatalf("load blocked run: %v", err)
	}
	if loaded.Status != "blocked" || loaded.StoppedReason != run.StoppedReason {
		t.Fatalf("loaded blocked run = %+v", loaded)
	}
}

func TestBuildPlanDefaultsMemoryCandidatesToPrivacySensitive(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")
	run, err := BuildPlan(home, PlanInput{TargetKind: "memory_candidate", TargetRefs: []string{"mcand_example_api_clean"}, States: []string{"parked"}}, fixedPanelNow)
	var blocked ErrBlockedNoLane
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want blocked no lane", err)
	}
	if run.Status != "blocked" || !strings.HasPrefix(run.StoppedReason, "blocked_no_lane:") {
		t.Fatalf("blocked run not surfaced: %+v", run)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRun(run.ID)
	if err != nil {
		t.Fatalf("load blocked run: %v", err)
	}
	if loaded.Status != "blocked" || loaded.StoppedReason != run.StoppedReason {
		t.Fatalf("loaded blocked run = %+v", loaded)
	}
}

func TestBuildPlanDefaultsPublicArxivMemoryCandidatesToNotSensitive(t *testing.T) {
	home := setupPanelPlanHome(t)
	writeMemoryCandidateRecord(t, home, "mcand_public_arxiv", map[string]any{
		"id":               "mcand_public_arxiv",
		"entity":           "knowledge:arxiv:arxiv:2607.01211v1",
		"predicate":        "limitation",
		"object":           "public benchmark limitation",
		"confidence":       0.89,
		"state":            "proposed",
		"visibility":       "candidate",
		"derivation":       "extracted",
		"source_trace_ref": "knowledge_extraction:kx_public",
		"snippet_hash":     "sha256:public",
		"qualifiers": map[string]string{
			"source":   "knowledge_extraction",
			"item_ref": "arxiv:2607.01211v1",
		},
	})

	run, err := BuildPlan(home, PlanInput{TargetKind: "memory_candidate", TargetRefs: []string{"mcand_public_arxiv"}}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build public arxiv plan: %v", err)
	}
	if run.Route.LensLane.ModelID != "panel/free-extract" || run.Route.JudgeLane.ModelID != "panel/cheap-judge" {
		t.Fatalf("public arxiv memory route = %+v, want non-sensitive fixture lanes", run.Route)
	}
	if !containsString(run.EvidenceRefs, "privacy_default:not_sensitive:public_arxiv_memory_candidate") {
		t.Fatalf("evidence refs = %+v, want public arxiv privacy default", run.EvidenceRefs)
	}
}

func TestBuildPlanWritesMissingDomainLensCandidate(t *testing.T) {
	home := setupPanelPlanHome(t)
	writePanelFixtureTarget(t, home, "mcand_example_api_clean")

	run, err := BuildPlan(home, PlanInput{
		TargetKind:            "memory_candidate",
		TargetRefs:            []string{"mcand_example_api_clean"},
		States:                []string{"parked"},
		Domain:                "hardware-lab",
		PrivacyOverrideReason: "synthetic fixtures",
	}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	gapID := "cobs_missing_domain_lens_" + targetDigest("memory_candidate|hardware-lab|")
	if !containsString(run.EvidenceRefs, "candidate:"+gapID) {
		t.Fatalf("run evidence refs = %+v, want candidate gap ref %s", run.EvidenceRefs, gapID)
	}
	var obs capabilityObservationTarget
	if err := candidates.ReadCandidate(home, candidates.CapabilityObs, gapID, &obs); err != nil {
		t.Fatalf("read missing domain lens candidate: %v", err)
	}
	if obs.Kind != string(candidates.CapabilityObs) || obs.State != "needs_review" || obs.Subject != "panel_lens_selection" {
		t.Fatalf("gap candidate = %+v", obs)
	}
	for _, want := range []string{"panel:lens_selection", "target_kind:memory_candidate", "domain:hardware-lab", "missing_domain_lens:hardware,hardware-lab,lab"} {
		if !containsString(obs.EvidenceRefs, want) {
			t.Fatalf("gap candidate evidence refs missing %q: %+v", want, obs.EvidenceRefs)
		}
	}
}

func setupPanelPlanHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	_, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml"), panelTestNow)
	if err != nil {
		t.Fatalf("import model capabilities: %v", err)
	}
	_, err = ImportLensManifest(home, filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml"), panelTestNow)
	if err != nil {
		t.Fatalf("import lenses: %v", err)
	}
	return home
}

func writePanelFixtureTarget(t *testing.T, home, id string) {
	t.Helper()
	data := readTestFile(t, filepath.Join("..", "..", "fixtures", "panel", "targets", id+".json"))
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode target fixture %s: %v", id, err)
	}
	writeMemoryCandidateRecord(t, home, id, payload)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
