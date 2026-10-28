package product

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/mem"
	"compound/internal/writeback"
)

var tProduct = time.Date(2026, 6, 29, 14, 0, 0, 0, time.UTC)

func TestApplyImportsProductSignalAndHypothesisCandidate(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}

	report, err := Apply(home, store, bundle, "fixtures/product/market-loop.yaml", tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.EntityID != "product:focus-notes" || len(report.ClaimIDs) == 0 || len(report.SignalIDs) != 1 || len(report.HypothesisIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}

	res, err := store.ResolveEntity("appstore:123456789")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "resolved" || res.Slug != "product:focus-notes" {
		t.Fatalf("product identifier should resolve, got %+v", res)
	}

	tl, err := LoadTimeline(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Signals) != 1 || tl.Signals[0].MetricName != "app_store_product_page_conversion" {
		t.Fatalf("signals = %+v", tl.Signals)
	}
	if len(tl.Hypotheses) != 1 || tl.Hypotheses[0].Verdict != candidates.Park {
		t.Fatalf("hypotheses = %+v", tl.Hypotheses)
	}

	if _, err := os.Stat(filepath.Join(home, candidates.InboxDir(candidates.ProductHypothesis), "hyp_focus_visuals_conversion.json")); err != nil {
		t.Fatal(err)
	}

	doctor, err := Doctor(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.SignalCount != 1 || doctor.HypothesisCount != 1 || doctor.PendingReviewCount != 1 {
		t.Fatalf("doctor = %+v", doctor)
	}
	if doctor.ReviewReadyCount != 1 || len(doctor.ReviewReady) != 1 {
		t.Fatalf("review-ready doctor = %+v", doctor)
	}
	ready := doctor.ReviewReady[0]
	if ready.HypothesisID != "hyp_focus_visuals_conversion" ||
		ready.SignalID != "sig_focus_screenshot_20260629" ||
		ready.MatchReason != "metric_match" ||
		ready.ReviewPlanCommand != "compound product review-plan --hypothesis hyp_focus_visuals_conversion --signal sig_focus_screenshot_20260629" {
		t.Fatalf("review-ready = %+v", ready)
	}
	refs := EvidenceRefs(doctor)
	if !hasString(refs, "product_review_ready:hyp_focus_visuals_conversion:sig_focus_screenshot_20260629") ||
		!hasString(refs, "product_signal:sig_focus_screenshot_20260629") {
		t.Fatalf("doctor evidence refs missing review-ready pair: %+v", refs)
	}
	if !NeedsWriteback(doctor) {
		t.Fatal("NeedsWriteback = false, want true")
	}
	gate := WritebackGate(doctor, "act_product_review", "pol_product_review", tProduct)
	if gate.CandidateKind != string(candidates.ProductHypothesis) || gate.OwnerTarget.Kind != "product_strategy" || gate.OwnerTarget.Ref != "product:focus-notes" {
		t.Fatalf("gate = %+v", gate)
	}
	if !strings.Contains(gate.GateBasis, "1 signal-backed review-ready pair") {
		t.Fatalf("gate basis missing review-ready count: %s", gate.GateBasis)
	}
	checked, err := writeback.EvaluateGate(gate, nil, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Status != writeback.GateNoPolicy {
		t.Fatalf("gate status = %q, want %q", checked.Status, writeback.GateNoPolicy)
	}

	review, err := LoadReview("../../fixtures/product/review.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reviewReport, err := ApplyReview(home, store, review, "fixtures/product/review.yaml", tProduct.Add(14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reviewReport.Status != "updated" || reviewReport.CandidateOutcome != "committed" || reviewReport.Result != "inconclusive" || reviewReport.EvidenceID == "" {
		t.Fatalf("review report = %+v", reviewReport)
	}
	if reviewReport.TransitionID == "" {
		t.Fatalf("review transition missing: %+v", reviewReport)
	}
	reviewTransition := readTransition(t, home, reviewReport.TransitionID)
	if reviewTransition.Scope.ProductID != "product:focus-notes" || !hasString(reviewTransition.Projections.Product, "product_review:review_focus_visuals_20260713") {
		t.Fatalf("review transition = %+v", reviewTransition)
	}
	reviewed, err := LoadTimeline(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed.Hypotheses) != 1 || reviewed.Hypotheses[0].Result != "inconclusive" {
		t.Fatalf("reviewed hypotheses = %+v", reviewed.Hypotheses)
	}
	if !hasString(reviewed.Hypotheses[0].EvidenceRefs, reviewReport.EvidenceID) || !hasString(reviewed.Hypotheses[0].EvidenceRefs, "product_review:review_focus_visuals_20260713") {
		t.Fatalf("review evidence refs = %+v", reviewed.Hypotheses[0].EvidenceRefs)
	}
	postReview, err := DoctorWithNow(home, "product:focus-notes", tProduct.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if postReview.Status != "ok" || postReview.PendingReviewCount != 0 {
		t.Fatalf("post-review doctor = %+v", postReview)
	}

	signalDoc, err := LoadSignal("../../fixtures/product/signal.asc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	signalReport, err := ApplySignal(home, store, signalDoc, "fixtures/product/signal.asc.yaml", tProduct.Add(14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if signalReport.Status != "ok" || len(signalReport.SignalIDs) != 1 || len(signalReport.EvidenceIDs) != 1 {
		t.Fatalf("signal report = %+v", signalReport)
	}
	if len(signalReport.TransitionIDs) != 1 {
		t.Fatalf("signal transitions = %+v", signalReport.TransitionIDs)
	}
	signalTransition := readTransition(t, home, signalReport.TransitionIDs[0])
	if signalTransition.Scope.ProductID != "product:focus-notes" || !hasString(signalTransition.Projections.Product, "product_signal:sig_focus_conversion_14d_20260713") {
		t.Fatalf("signal transition = %+v", signalTransition)
	}
	withSignal, err := LoadTimeline(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(withSignal.Signals) != 2 {
		t.Fatalf("signals after standalone import = %+v", withSignal.Signals)
	}
	importedSignal := findSignal(withSignal.Signals, "sig_focus_conversion_14d_20260713")
	if importedSignal.ID == "" {
		t.Fatalf("standalone signal missing: %+v", withSignal.Signals)
	}
	if !hasString(importedSignal.EvidenceRefs, signalReport.EvidenceIDs[0]) || !hasString(importedSignal.EvidenceRefs, "fixture:asc-export") {
		t.Fatalf("signal evidence refs = %+v", importedSignal.EvidenceRefs)
	}

	exportDoc, err := LoadSignal("../../fixtures/product/source-export.asc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if exportDoc.SchemaVersion != SignalSchemaVersion || exportDoc.Signal.ID != "sig_focus_asc_export_conversion_20260713" || exportDoc.Signal.MetricName != "app_store_product_page_conversion" {
		t.Fatalf("export adapter = %+v", exportDoc)
	}
	exportReport, err := ApplySignal(home, store, exportDoc, "fixtures/product/source-export.asc.yaml", tProduct.Add(14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if exportReport.Status != "ok" || len(exportReport.SignalIDs) != 1 || len(exportReport.EvidenceIDs) != 1 || len(exportReport.TransitionIDs) != 1 {
		t.Fatalf("export report = %+v", exportReport)
	}
	exportTransition := readTransition(t, home, exportReport.TransitionIDs[0])
	if exportTransition.Scope.ProductID != "product:focus-notes" || !hasString(exportTransition.Projections.Product, "product_signal:sig_focus_asc_export_conversion_20260713") {
		t.Fatalf("export transition = %+v", exportTransition)
	}
	withExport, err := LoadTimeline(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	exportedSignal := findSignal(withExport.Signals, "sig_focus_asc_export_conversion_20260713")
	if exportedSignal.ID == "" || exportedSignal.MetricName != "app_store_product_page_conversion" {
		t.Fatalf("exported signal = %+v", exportedSignal)
	}
	if !hasString(exportedSignal.EvidenceRefs, "fixture:asc:conversion:before") || !hasString(exportedSignal.EvidenceRefs, "fixture:asc:conversion:after") || !hasString(exportedSignal.EvidenceRefs, exportReport.EvidenceIDs[0]) {
		t.Fatalf("exported evidence refs = %+v", exportedSignal.EvidenceRefs)
	}
	if len(withExport.Transitions) != 4 {
		t.Fatalf("timeline transitions = %+v", withExport.Transitions)
	}
	if !hasTimelineObject(withExport.Transitions, "product_signal:sig_focus_asc_export_conversion_20260713") || !hasTimelineObject(withExport.Transitions, "product_review:review_focus_visuals_20260713") || !hasTimelineObject(withExport.Transitions, "product_review_intent:review_focus_visuals_20260713") {
		t.Fatalf("timeline transitions missing product refs: %+v", withExport.Transitions)
	}
}

func TestLoadProductTransitionsPreservesMalformedRecordPolicy(t *testing.T) {
	home := t.TempDir()
	eventsDir := filepath.Join(home, "events")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "torn.json"), []byte("{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "foreign.json"), []byte("{\"schema_version\":\"foreign.v1\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	transitions, err := loadProductTransitions(home, "product:fixture")
	if err != nil || len(transitions) != 0 {
		t.Fatalf("torn/foreign transitions = %+v, err=%v", transitions, err)
	}

	invalid, err := json.Marshal(map[string]any{
		"schema_version": envelope.SVTransition,
		"id":             "tr_invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "tr_invalid.json"), append(invalid, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProductTransitions(home, "product:fixture"); err == nil ||
		!strings.Contains(err.Error(), "product transition tr_invalid.json") {
		t.Fatalf("invalid product transition error = %v", err)
	}
}

func TestAuthoritySummarizesProductFeedbackState(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, bundle, "fixtures/product/market-loop.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	sourceBundle, err := LoadProjectContractBundle("../../fixtures/product/project-contract.yaml", "../../fixtures/product/release-contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, sourceBundle, "fixtures/product/project-contract.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	signalDoc, err := LoadSignal("../../fixtures/product/signal.asc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySignal(home, store, signalDoc, "fixtures/product/signal.asc.yaml", tProduct.Add(14*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	report, err := Authority(home, store)
	if err != nil {
		t.Fatal(err)
	}
	if report.AuthorityRef != AuthorityRef || report.Status != "needs_review" {
		t.Fatalf("authority header = %+v", report)
	}
	if report.ReleasePlanningRef != "compound://product/release-flow" ||
		!strings.Contains(report.ProductReleaseCommand, "compound product release-plan") ||
		!strings.Contains(report.RepositoryExportCommand, "compound release export-public") ||
		!strings.Contains(report.ReleaseBoundary, "not product release") {
		t.Fatalf("authority must disambiguate product release from repository export: %+v", report)
	}
	if !slices.Contains(report.AuthorityPaths, "compound://events/<id>") || slices.Contains(report.AuthorityPaths, "events/*.json") {
		t.Fatalf("authority event reference is not logical: %v", report.AuthorityPaths)
	}
	if report.ProductCount != 2 || report.SignalCount != 2 || report.HypothesisCount != 1 || report.ReviewReadyCount != 2 || report.TransitionCount != 1 {
		t.Fatalf("authority counts = %+v", report)
	}
	focus := findAuthorityProduct(report.Products, "product:focus-notes")
	if focus.ProductID == "" || focus.Status != "needs_review" || focus.LatestSignalID != "sig_focus_conversion_14d_20260713" {
		t.Fatalf("focus authority = %+v", focus)
	}
	if len(focus.ReviewPlanCommands) != 2 || !strings.Contains(focus.ReviewPlanCommands[0], "compound product review-plan") {
		t.Fatalf("review commands = %+v", focus.ReviewPlanCommands)
	}
	sourceContract := findAuthorityProduct(report.Products, "product:source_contract_demo")
	if sourceContract.ProductID == "" || sourceContract.SignalCount != 0 || sourceContract.HypothesisCount != 0 {
		t.Fatalf("source contract authority = %+v", sourceContract)
	}
}

func findAuthorityProduct(items []ProductAuthorityItem, id string) ProductAuthorityItem {
	for _, item := range items {
		if item.ProductID == id {
			return item
		}
	}
	return ProductAuthorityItem{}
}

func TestProjectContractBundleMapsSourceInPlaceProductRegistry(t *testing.T) {
	bundle, err := LoadProjectContractBundle("../../fixtures/product/project-contract.yaml", "../../fixtures/product/release-contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.SchemaVersion != SchemaVersion || bundle.Product.ID != "product:source_contract_demo" {
		t.Fatalf("bundle identity = %+v", bundle)
	}
	if !hasString(bundle.Product.Channels, "app_store") || !hasString(bundle.Product.Channels, "web") || !hasString(bundle.Product.Channels, "api") {
		t.Fatalf("channels = %+v", bundle.Product.Channels)
	}
	if bundle.Product.ReleaseIdentity.BundleID != "com.example.sourcecontractdemo" ||
		bundle.Product.ReleaseIdentity.AppStoreConnectAppID != "app-demo-sourcecontract" ||
		bundle.Product.ReleaseIdentity.Version != "1.0.0" ||
		bundle.Product.ReleaseIdentity.Build != "42" {
		t.Fatalf("release identity = %+v", bundle.Product.ReleaseIdentity)
	}
	for _, ref := range []string{
		"https://example.invalid/source-contract-demo.git",
		"bundle:com.example.sourcecontractdemo",
		"appstore:app-demo-sourcecontract",
		"https://source-contract-demo.example.invalid/health",
	} {
		if !hasSurfaceRef(bundle.Product.Surfaces, ref) {
			t.Fatalf("surfaces missing %q: %+v", ref, bundle.Product.Surfaces)
		}
	}
	if !hasString(bundle.EvidenceRefs, "project-contract:project-contract.yaml") || !hasString(bundle.EvidenceRefs, "release-contract:release-contract.yaml") {
		t.Fatalf("evidence refs = %+v", bundle.EvidenceRefs)
	}

	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Apply(home, store, bundle, "fixtures/product/project-contract.yaml", tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.EntityID != "product:source_contract_demo" || len(report.ClaimIDs) != 13 || len(report.SignalIDs) != 0 || len(report.HypothesisIDs) != 0 {
		t.Fatalf("report = %+v", report)
	}
	for predicate, want := range map[string]string{
		"release_bundle_id":                "com.example.sourcecontractdemo",
		"release_app_store_connect_app_id": "app-demo-sourcecontract",
		"release_version":                  "1.0.0",
		"release_build":                    "42",
		"release_subscription_ids":         "com.example.sourcecontractdemo.pro.monthly",
		"release_mechanism":                "app_store_connect",
		// Deployment reaches the store under the product entity, not a parallel
		// bare-slug one — the join that lets a reader answer "where does this run".
		"deploys_to":          "place:fixture-runtime",
		"deploy_services":     "source-contract-demo-api",
		"deploy_healthchecks": "https://source-contract-demo.example.invalid/health",
	} {
		claim, ok, err := store.CurrentClaim(bundle.Product.ID, predicate)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || claim.Object != want {
			t.Fatalf("release claim %s = %+v found=%t, want %q", predicate, claim, ok, want)
		}
	}
	for _, token := range []string{"appstore:app-demo-sourcecontract", "bundle:com.example.sourcecontractdemo"} {
		res, err := store.ResolveEntity(token)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "resolved" || res.Slug != "product:source_contract_demo" {
			t.Fatalf("resolve %s = %+v", token, res)
		}
	}
}

func TestLoadProjectContractBundleAcceptsReleaseOnly(t *testing.T) {
	releasePath := writeProjectContractTestFile(t, "release.yaml", `
schema_version: 1
project: alpha-guard
release_status: ready
`)

	bundle, err := LoadProjectContractBundle("", releasePath)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Product.ID != "product:alpha_guard" {
		t.Fatalf("product id = %q, want product:alpha_guard", bundle.Product.ID)
	}
	if len(bundle.EvidenceRefs) != 1 || bundle.EvidenceRefs[0] != "release-contract:release.yaml" {
		t.Fatalf("release-only evidence refs = %+v", bundle.EvidenceRefs)
	}
}

func TestLoadProjectContractBundleProjectOnlyOmitsReleaseEvidence(t *testing.T) {
	projectPath := writeProjectContractTestFile(t, "project.yaml", `
schema_version: 1
project:
  slug: alpha-guard
  name: Alpha Guard
`)

	bundle, err := LoadProjectContractBundle(projectPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.EvidenceRefs) != 1 || bundle.EvidenceRefs[0] != "project-contract:project.yaml" {
		t.Fatalf("project-only evidence refs = %+v", bundle.EvidenceRefs)
	}
}

func TestLoadProjectContractBundleUsesCanonicalProjectSlug(t *testing.T) {
	projectPath := writeProjectContractTestFile(t, "project.yaml", `
schema_version: 1
project:
  slug: wid-get-alpha
  name: Alpha Guard
  aliases:
    - alpha_guard
    - alphaguard
`)

	bundle, err := LoadProjectContractBundle(projectPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Product.ID != "product:wid_get_alpha" {
		t.Fatalf("product id = %q, want canonical project.slug identity", bundle.Product.ID)
	}
}

func TestLoadProjectContractBundleRejectsUnsupportedCanonicalPunctuation(t *testing.T) {
	for _, identity := range []string{"widget/alpha", "widget.alpha"} {
		t.Run(identity, func(t *testing.T) {
			projectPath := writeProjectContractTestFile(t, "project.yaml",
				"schema_version: 1\nproject:\n  slug: \""+identity+"\"\n")
			if _, err := LoadProjectContractBundle(projectPath, ""); err == nil {
				t.Fatalf("canonical identity %q with unsupported punctuation should be rejected", identity)
			}
		})
	}
}

func TestLoadProjectContractBundleRejectsGenericCanonicalIdentities(t *testing.T) {
	genericIdentities := []string{
		"",
		"-",
		"--",
		"__",
		"_-_",
		"unknown",
		"none",
		"null",
		"n/a",
		"na",
		"tbd",
		"todo",
		"not_configured",
		"not-configured",
		"unconfigured",
	}
	sources := []struct {
		name        string
		fileName    string
		body        func(string) string
		projectPath bool
	}{
		{
			name:     "project contract",
			fileName: "project.yaml",
			body: func(identity string) string {
				return "schema_version: 1\nproject:\n  slug: \"" + identity + "\"\n"
			},
			projectPath: true,
		},
		{
			name:     "release contract",
			fileName: "release.yaml",
			body: func(identity string) string {
				return "schema_version: 1\nproject: \"" + identity + "\"\n"
			},
		},
	}

	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			for _, identity := range genericIdentities {
				testName := identity
				if testName == "" {
					testName = "empty"
				}
				t.Run(testName, func(t *testing.T) {
					path := writeProjectContractTestFile(t, source.fileName, source.body(identity))
					projectPath, releasePath := "", path
					if source.projectPath {
						projectPath, releasePath = path, ""
					}
					if _, err := LoadProjectContractBundle(projectPath, releasePath); err == nil {
						t.Fatalf("generic canonical identity %q should be rejected", identity)
					}
				})
			}
		})
	}
}

func TestLoadProjectContractBundleRejectsMultipleYAMLDocuments(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		body        string
		projectPath bool
	}{
		{
			name:     "project contract",
			fileName: "project.yaml",
			body: `
schema_version: 1
project:
  slug: alpha-guard
---
schema_version: 1
project:
  slug: beta-store
`,
			projectPath: true,
		},
		{
			name:     "release contract",
			fileName: "release.yaml",
			body: `
schema_version: 1
project: alpha-guard
---
schema_version: 1
project: beta-store
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProjectContractTestFile(t, tt.fileName, tt.body)
			projectPath, releasePath := "", path
			if tt.projectPath {
				projectPath, releasePath = path, ""
			}
			if _, err := LoadProjectContractBundle(projectPath, releasePath); err == nil {
				t.Fatal("multiple YAML documents should be rejected")
			}
		})
	}
}

func TestLoadProjectContractBundleDoesNotMaskInvalidPairedIdentity(t *testing.T) {
	tests := []struct {
		name            string
		projectIdentity string
		releaseIdentity string
	}{
		{
			name:            "invalid project identity",
			projectIdentity: "",
			releaseIdentity: "alpha-guard",
		},
		{
			name:            "invalid release identity",
			projectIdentity: "alpha-guard",
			releaseIdentity: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectPath := writeProjectContractTestFile(t, "project.yaml",
				"schema_version: 1\nproject:\n  slug: \""+tt.projectIdentity+"\"\n")
			releasePath := writeProjectContractTestFile(t, "release.yaml",
				"schema_version: 1\nproject: \""+tt.releaseIdentity+"\"\n")
			if _, err := LoadProjectContractBundle(projectPath, releasePath); err == nil {
				t.Fatal("a valid paired contract must not mask an invalid canonical identity")
			}
		})
	}
}

func TestLoadProjectContractBundleRequiresAContractPath(t *testing.T) {
	_, err := LoadProjectContractBundle("", "")
	if err == nil || !strings.Contains(err.Error(), "project or release contract path required") {
		t.Fatalf("error = %v, want missing contract path rejection", err)
	}
}

func TestLoadProjectContractBundleRejectsDuplicateYAMLKeys(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		body        string
		projectPath bool
	}{
		{
			name:     "project contract",
			fileName: "project.yaml",
			body: `
schema_version: 1
project:
  slug: alpha-guard
  slug: beta-store
`,
			projectPath: true,
		},
		{
			name:     "release contract",
			fileName: "release.yaml",
			body: `
schema_version: 1
project: alpha-guard
project: beta-store
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProjectContractTestFile(t, tt.fileName, tt.body)
			projectPath, releasePath := "", path
			if tt.projectPath {
				projectPath, releasePath = path, ""
			}
			if _, err := LoadProjectContractBundle(projectPath, releasePath); err == nil {
				t.Fatal("duplicate YAML key should be rejected")
			}
		})
	}
}

func TestLoadProjectContractBundleRejectsCaseInsensitiveDuplicateYAMLKeys(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		body        string
		projectPath bool
	}{
		{
			name:     "nested project key",
			fileName: "project.yaml",
			body: `
schema_version: 1
project:
  slug: alpha-guard
  SLUG: beta-store
`,
			projectPath: true,
		},
		{
			name:     "top-level release key",
			fileName: "release.yaml",
			body: `
schema_version: 1
project: alpha-guard
PROJECT: beta-store
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProjectContractTestFile(t, tt.fileName, tt.body)
			projectPath, releasePath := "", path
			if tt.projectPath {
				projectPath, releasePath = path, ""
			}
			if _, err := LoadProjectContractBundle(projectPath, releasePath); err == nil {
				t.Fatal("case-insensitive duplicate YAML key should be rejected")
			}
		})
	}
}

func TestLoadProjectContractBundleRejectsYAMLMergeKeys(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		body        string
		projectPath bool
	}{
		{
			name:     "nested project merge",
			fileName: "project.yaml",
			body: `
schema_version: 1
defaults: &defaults
  slug: beta-store
project:
  <<: *defaults
  slug: alpha-guard
`,
			projectPath: true,
		},
		{
			name:     "top-level release merge",
			fileName: "release.yaml",
			body: `
defaults: &defaults
  schema_version: 1
  project: beta-store
<<: *defaults
schema_version: 1
project: alpha-guard
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProjectContractTestFile(t, tt.fileName, tt.body)
			projectPath, releasePath := "", path
			if tt.projectPath {
				projectPath, releasePath = path, ""
			}
			if _, err := LoadProjectContractBundle(projectPath, releasePath); err == nil {
				t.Fatal("YAML merge key should be rejected")
			}
		})
	}
}

func TestLoadProjectContractBundleAcceptsQuotedSchemaVersions(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		body        string
		projectPath bool
	}{
		{
			name:     "project contract",
			fileName: "project.yaml",
			body: `
schema_version: "1"
project:
  slug: alpha-guard
`,
			projectPath: true,
		},
		{
			name:     "release contract",
			fileName: "release.yaml",
			body: `
schema_version: "1"
project: alpha-guard
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProjectContractTestFile(t, tt.fileName, tt.body)
			projectPath, releasePath := "", path
			if tt.projectPath {
				projectPath, releasePath = path, ""
			}
			if _, err := LoadProjectContractBundle(projectPath, releasePath); err != nil {
				t.Fatalf("quoted schema_version 1 should be accepted: %v", err)
			}
		})
	}
}

func TestLoadProjectContractBundleRejectsUnsupportedSchemaVersions(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		body        string
		projectPath bool
	}{
		{
			name:     "project contract",
			fileName: "project.yaml",
			body: `
schema_version: 2
project:
  slug: alpha-guard
`,
			projectPath: true,
		},
		{
			name:     "release contract",
			fileName: "release.yaml",
			body: `
schema_version: 2
project: alpha-guard
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProjectContractTestFile(t, tt.fileName, tt.body)
			projectPath, releasePath := "", path
			if tt.projectPath {
				projectPath, releasePath = path, ""
			}
			_, err := LoadProjectContractBundle(projectPath, releasePath)
			if err == nil || !strings.Contains(err.Error(), "schema_version") {
				t.Fatalf("error = %v, want unsupported schema_version rejection", err)
			}
		})
	}
}

func TestLoadProjectContractBundleRejectsMissingSchemaVersions(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		body        string
		projectPath bool
	}{
		{
			name:     "project contract",
			fileName: "project.yaml",
			body: `
project:
  slug: alpha-guard
`,
			projectPath: true,
		},
		{
			name:     "release contract",
			fileName: "release.yaml",
			body: `
project: alpha-guard
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProjectContractTestFile(t, tt.fileName, tt.body)
			projectPath, releasePath := "", path
			if tt.projectPath {
				projectPath, releasePath = path, ""
			}
			_, err := LoadProjectContractBundle(projectPath, releasePath)
			if err == nil || !strings.Contains(err.Error(), "schema_version") {
				t.Fatalf("error = %v, want missing schema_version rejection", err)
			}
		})
	}
}

func writeProjectContractTestFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateRejectsSignalForOtherProduct(t *testing.T) {
	b := validBundle()
	b.Signals[0].ProductID = "product:other"
	if err := Validate(b); err == nil {
		t.Fatal("signal for another product should be rejected")
	}
}

func TestValidateReviewRejectsUnknownResult(t *testing.T) {
	review := validReview()
	review.Result = "unknown"
	if err := ValidateReview(review); err == nil {
		t.Fatal("review with unknown result should be rejected")
	}
}

func TestValidateSignalDocumentRejectsMissingSource(t *testing.T) {
	doc := validSignalDocument()
	doc.Source.Kind = ""
	if err := ValidateSignalDocument(doc); err == nil {
		t.Fatal("signal document without source should be rejected")
	}
}

func TestValidateSignalExportRejectsUnsupportedSource(t *testing.T) {
	doc := validSignalExportDocument()
	doc.Source.Kind = "spreadsheet"
	if _, err := AdaptSignalExport(doc); err == nil {
		t.Fatal("unsupported signal export source should be rejected")
	}
}

func TestSignalExportProviderAllowlistAndCanonicalMetricMapDriveImport(t *testing.T) {
	type metricCase struct {
		metricName    string
		canonicalName string
	}

	metricByProvider := map[string]metricCase{}
	metricKeys := make([]string, 0, len(canonicalSignalMetricNames))
	for key := range canonicalSignalMetricNames {
		metricKeys = append(metricKeys, key)
	}
	sort.Strings(metricKeys)
	for _, key := range metricKeys {
		provider, metricName, ok := strings.Cut(key, ":")
		if !ok {
			t.Fatalf("canonical metric key %q must be provider:metric", key)
		}
		canonicalName := canonicalSignalMetricNames[key]
		if _, ok := signalExportProviderAllowlist[provider]; !ok {
			t.Fatalf("canonical metric %q is wired for unsupported provider %q", key, provider)
		}
		if got := canonicalMetricName(provider, metricName); got != canonicalName {
			t.Fatalf("canonicalMetricName(%q, %q) = %q; want %q", provider, metricName, got, canonicalName)
		}
		if _, exists := metricByProvider[provider]; !exists {
			metricByProvider[provider] = metricCase{metricName: metricName, canonicalName: canonicalName}
		}
	}

	for _, provider := range []string{"google_search_console", "google_analytics", "google_adsense"} {
		if _, ok := signalExportProviderAllowlist[provider]; !ok {
			t.Fatalf("new Google provider %q missing from signal export allowlist", provider)
		}
	}

	providers := make([]string, 0, len(signalExportProviderAllowlist))
	for provider := range signalExportProviderAllowlist {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		provider := provider
		metric, ok := metricByProvider[provider]
		if !ok {
			t.Fatalf("provider %q missing canonical metric map entry", provider)
		}

		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			store, err := mem.Open(home)
			if err != nil {
				t.Fatal(err)
			}

			doc := SignalExportDocument{
				SchemaVersion: SignalExportSchemaVersion,
				Source:        SignalSource{Kind: provider, Ref: "fixture:" + provider + ":example.com:2026-07-01"},
				ProductID:     "product:example-site",
				Surface:       "provider-ingress",
				ChangeEvent:   "Provider metric snapshot observed.",
				Metric: ExportMetric{
					Name: metric.metricName,
					Before: ExportMetricPoint{
						Value:  100,
						Period: "2026-06-01/2026-06-30",
						Ref:    "fixture:" + provider + ":before",
					},
					After: ExportMetricPoint{
						Value:  100,
						Period: "2026-06-01/2026-06-30",
						Ref:    "fixture:" + provider + ":after",
					},
				},
				Window:     "neutral snapshot 2026-07-01",
				Confidence: 0.5,
				Confounders: []string{
					"neutral provider snapshot; no causal movement implied",
				},
				Interpretations: []Interpretation{{
					Lens:  "provider_snapshot",
					Claim: "records provider metric observation only",
				}},
				NextAction:   "Review alongside product context before taking action.",
				EvidenceRefs: []string{"fixture:" + provider + ":source-export"},
				ObservedAt:   "2026-07-01T00:00:00Z",
			}
			data, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), provider+".json")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}

			loaded, err := LoadSignal(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Signal.MetricName != metric.canonicalName || loaded.Signal.Before != loaded.Signal.After {
				t.Fatalf("loaded signal = %+v; want metric %q with neutral movement", loaded.Signal, metric.canonicalName)
			}

			report, err := ApplySignal(home, store, loaded, path, tProduct)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != "ok" || len(report.SignalIDs) != 1 || len(report.EvidenceIDs) != 1 || len(report.TransitionIDs) != 1 {
				t.Fatalf("report = %+v", report)
			}
			if len(report.ClaimIDs) != 0 || len(report.HypothesisIDs) != 0 {
				t.Fatalf("signal import promoted claims or hypotheses: %+v", report)
			}

			signals, err := loadSignals(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(signals) != 1 || signals[0].MetricName != metric.canonicalName || signals[0].Before != signals[0].After {
				t.Fatalf("stored signals = %+v; want one neutral %s signal", signals, metric.canonicalName)
			}
			if signals[0].ChangeEvent != "Provider metric snapshot observed." || !hasString(signals[0].Confounders, "neutral provider snapshot; no causal movement implied") {
				t.Fatalf("stored signal should remain neutral observation: %+v", signals[0])
			}
			for _, interpretation := range signals[0].Interpretations {
				if strings.Contains(strings.ToLower(interpretation.Claim), "caus") {
					t.Fatalf("interpretation claim should not assert causality: %+v", interpretation)
				}
			}

			hypotheses, err := loadHypotheses(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(hypotheses) != 0 {
				t.Fatalf("hypotheses = %+v, want none", hypotheses)
			}
			claims, err := os.ReadFile(filepath.Join(home, "ledger", "claims.jsonl"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(claims)) != "" {
				t.Fatalf("claims ledger should be empty, got %s", string(claims))
			}
			transition := readTransition(t, home, report.TransitionIDs[0])
			if !strings.Contains(transition.StateDelta, "no product surface changed") {
				t.Fatalf("transition should not imply product change: %+v", transition)
			}
		})
	}
}

func TestLoadASCDatapointsSignalAdaptsProviderSnapshot(t *testing.T) {
	doc, err := LoadASCDatapointsSignal("../../fixtures/product/asc-datapoints.json", ASCDatapointsSignalOptions{
		ProductID: "product:focus-notes",
		AppID:     "123456789",
		Metric:    "review_count",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSignalDocument(doc); err != nil {
		t.Fatal(err)
	}
	if doc.Source.Kind != "app_store_connect" || doc.Source.Ref != "asc:datapoints:123456789:2026-07-13" {
		t.Fatalf("source = %+v", doc.Source)
	}
	s := doc.Signal
	if s.ProductID != "product:focus-notes" || s.Surface != "app_store" || s.MetricName != "app_store_connect_review_count" {
		t.Fatalf("signal identity = %+v", s)
	}
	if s.ChangeEvent != "Apple App Store Connect datapoints snapshot observed" || s.Before != 11 || s.After != 11 {
		t.Fatalf("snapshot movement should be neutral, got %+v", s)
	}
	if s.Window != "snapshot 2026-07-13" || s.ObservedAt != "2026-07-13T12:00:00Z" {
		t.Fatalf("snapshot time = %+v", s)
	}
	if !hasString(s.Confounders, "snapshot observation; no causal movement is implied") {
		t.Fatalf("confounders = %+v", s.Confounders)
	}
	if !hasString(s.EvidenceRefs, "asc:datapoints:2026-07-13:123456789") || !hasString(s.EvidenceRefs, "asc:rating_counts:5:9") {
		t.Fatalf("evidence refs = %+v", s.EvidenceRefs)
	}
}

func TestDoctorWarnsWhenASCSignalIsStale(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := LoadASCDatapointsSignal("../../fixtures/product/asc-datapoints.json", ASCDatapointsSignalOptions{
		ProductID: "product:focus-notes",
		AppID:     "123456789",
		Metric:    "review_count",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySignal(home, store, doc, "fixtures/product/asc-datapoints.json", tProduct); err != nil {
		t.Fatal(err)
	}
	report, err := DoctorWithNow(home, "product:focus-notes", time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "warn" || len(report.ASCSignalStaleness) != 1 {
		t.Fatalf("stale ASC doctor report = %+v", report)
	}
	stale := report.ASCSignalStaleness[0]
	if stale.LastSignalID != doc.Signal.ID || stale.Reason != "last_asc_signal_older_than_48h" || !strings.Contains(stale.FixCommand, "tools/asc_daily_signals.ts") || !strings.Contains(stale.FixCommand, "--asc-command") {
		t.Fatalf("stale ASC warning = %+v", stale)
	}
}

func TestASCStalenessTreatsCanonicalConversionMetricAsASC(t *testing.T) {
	signals := []Signal{{
		ID:         "sig_conversion",
		ProductID:  "product:focus-notes",
		MetricName: "app_store_product_page_conversion",
		ObservedAt: "2026-07-13T12:00:00Z",
	}}
	stale := staleASCSignals(signals, "product:focus-notes", time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC), nil)
	if len(stale) != 1 || stale[0].LastSignalID != "sig_conversion" {
		t.Fatalf("canonical ASC conversion staleness = %+v", stale)
	}
}

func TestDoctorWarnsWhenRegisteredAppHasNoASCSignal(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:empty-store-app",
			Name:           "Empty Store App",
			OwnerEntity:    "project:empty-store-app",
			Surfaces:       []Surface{{ID: "app-store-page", Kind: "app_store", Ref: "appstore:555555555"}},
			Channels:       []string{"app_store"},
			Positioning:    "Synthetic app used to prove ASC silence warnings.",
			PrivacyPosture: "local_first",
		},
		EvidenceRefs: []string{"fixture:product-registry"},
	}
	if _, err := Apply(home, store, bundle, "fixtures/product/empty-store-app.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	report, err := DoctorForRegisteredProduct(home, store, "product:empty-store-app", tProduct.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "warn" || len(report.ASCSignalStaleness) != 1 {
		t.Fatalf("registered app silence should warn: %+v", report)
	}
	stale := report.ASCSignalStaleness[0]
	if stale.LastSignalID != "" || stale.Reason != "no_asc_signal_recorded" || !strings.Contains(stale.FixCommand, "COMPOUND_ASC_DAILY_COMMAND") {
		t.Fatalf("registered app silence warning = %+v", stale)
	}
}

func TestASCSignalReplayIsIdempotentPerMetricDate(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := LoadSignal("../../fixtures/product/source-export.asc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	first, err := ApplySignal(home, store, doc, "fixtures/product/source-export.asc.yaml", tProduct)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ApplySignal(home, store, doc, "fixtures/product/source-export.asc.yaml", tProduct.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.SignalIDs) != 1 || len(second.SignalIDs) != 1 || first.SignalIDs[0] != second.SignalIDs[0] {
		t.Fatalf("signal replay ids first=%+v second=%+v", first, second)
	}
	tl, err := LoadTimeline(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Signals) != 1 || tl.Signals[0].ID != first.SignalIDs[0] {
		t.Fatalf("replay should keep one signal row: %+v", tl.Signals)
	}
}

func TestApplyCanonicalMetricsCreatesReviewReadyHypothesis(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyCanonicalMetrics(home, store, "../../fixtures/product/canonical-latest.json", CanonicalMetricsOptions{
		ProductID: "product:focus-notes",
	}, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.IdeasRead != 1 || report.SignalsCreated != 2 || report.HypothesesCreated != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.SourceRef != "product_metrics:canonical_latest:focus_notes:2026_07_13" || report.SourceHash == "" {
		t.Fatalf("source = %s hash=%s", report.SourceRef, report.SourceHash)
	}
	if !hasString(report.HypothesisIDs, "focus-notes.appstore-low-conversion") {
		t.Fatalf("hypotheses = %+v", report.HypothesisIDs)
	}
	tl, err := LoadTimeline(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Signals) != 2 || len(tl.Hypotheses) != 1 {
		t.Fatalf("timeline = %+v", tl)
	}
	h := tl.Hypotheses[0]
	if h.ID != "focus-notes.appstore-low-conversion" || h.Verdict != candidates.Park || h.Result != "unknown" {
		t.Fatalf("hypothesis = %+v", h)
	}
	if !hasString(h.Metrics, "app.conversion_rate") || !hasString(h.EvidenceRefs, "product_metrics:metric:app_conversion_rate") {
		t.Fatalf("hypothesis metrics/evidence = %+v %+v", h.Metrics, h.EvidenceRefs)
	}
	var conversion Signal
	for _, signal := range tl.Signals {
		if signal.MetricName == "app.conversion_rate" {
			conversion = signal
			break
		}
	}
	if conversion.ID == "" || conversion.Before != 0.0135 || conversion.After != 0.0135 || conversion.Surface != "app_store" {
		t.Fatalf("conversion signal = %+v", conversion)
	}
	if !hasString(conversion.Confounders, "canonical metrics snapshot; no causal movement is implied") {
		t.Fatalf("confounders = %+v", conversion.Confounders)
	}
	doctor, err := Doctor(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.PendingReviewCount != 1 || doctor.ReviewReadyCount != 2 {
		t.Fatalf("doctor = %+v", doctor)
	}
	if !strings.Contains(doctor.ReviewReady[0].ReviewPlanCommand, "compound product review-plan --hypothesis focus-notes.appstore-low-conversion --signal ") {
		t.Fatalf("review ready = %+v", doctor.ReviewReady)
	}
}

func TestApplyCanonicalMetricsReplayDoesNotOverwriteExistingHypothesis(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	fixture := "../../fixtures/product/canonical-latest.json"
	first, err := ApplyCanonicalMetrics(home, store, fixture, CanonicalMetricsOptions{ProductID: "product:focus-notes"}, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if first.HypothesesCreated != 1 {
		t.Fatalf("first import = %+v", first)
	}
	candidate, err := loadHypothesisCandidate(home, "focus-notes.appstore-low-conversion")
	if err != nil {
		t.Fatal(err)
	}
	candidate.Result = "supported"
	candidate.Verdict = "supported"
	if err := writeHypothesisCandidate(home, candidate); err != nil {
		t.Fatal(err)
	}
	second, err := ApplyCanonicalMetrics(home, store, fixture, CanonicalMetricsOptions{ProductID: "product:focus-notes"}, tProduct.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.HypothesesCreated != 0 || second.HypothesesExisting != 1 || second.SignalsCreated != 0 || second.SignalsExisting != 2 {
		t.Fatalf("second import = %+v", second)
	}
	kept, err := loadHypothesisCandidate(home, "focus-notes.appstore-low-conversion")
	if err != nil {
		t.Fatal(err)
	}
	if kept.Result != "supported" || kept.Verdict != "supported" {
		t.Fatalf("replay overwrote reviewed candidate: %+v", kept)
	}
}

func TestLoadASCDatapointsSignalRejectsMissingApp(t *testing.T) {
	_, err := LoadASCDatapointsSignal("../../fixtures/product/asc-datapoints.json", ASCDatapointsSignalOptions{
		ProductID: "product:focus-notes",
		AppID:     "missing",
		Metric:    "review_count",
	})
	if err == nil {
		t.Fatal("missing ASC app id should be rejected")
	}
}

func TestASCSignalImportIsIdempotentPerDay(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := LoadASCDatapointsSignal("../../fixtures/product/asc-datapoints.json", ASCDatapointsSignalOptions{
		ProductID: "product:focus-notes",
		AppID:     "123456789",
		Metric:    "review_count",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySignal(home, store, doc, "fixtures/product/asc-datapoints.json", tProduct); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySignal(home, store, doc, "fixtures/product/asc-datapoints.json", tProduct.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	signals, err := loadSignals(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0].ID != doc.Signal.ID {
		t.Fatalf("signals = %+v, want one deduped daily ASC signal %s", signals, doc.Signal.ID)
	}
}

func TestProductDoctorWarnsWhenASCSignalsGoStale(t *testing.T) {
	home := t.TempDir()
	staleAt := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	if err := appendUniqueSignal(home, Signal{
		ID:          "sig_stale_asc",
		ProductID:   "product:focus-notes",
		Surface:     "app-store-page",
		ChangeEvent: "App Store Connect daily metric snapshot observed.",
		MetricName:  "app_store_connect_units",
		Before:      3,
		After:       3,
		Window:      "daily snapshot 2026-07-10",
		Confidence:  0.72,
		EvidenceRefs: []string{
			"asc:daily:123456789:2026-07-10:units",
			"asc:app:123456789",
		},
		ObservedAt: staleAt.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	report, err := DoctorWithNow(home, "product:focus-notes", staleAt.Add(49*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "warn" || len(report.ASCSignalStaleness) != 1 {
		t.Fatalf("doctor report = %+v, want stale ASC warning", report)
	}
	if report.ASCSignalStaleness[0].LastSignalID != "sig_stale_asc" ||
		!strings.Contains(report.ASCSignalStaleness[0].FixCommand, "tools/asc_daily_signals.ts") {
		t.Fatalf("staleness = %+v", report.ASCSignalStaleness[0])
	}

	fresh, err := DoctorWithNow(home, "product:focus-notes", staleAt.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != "ok" || len(fresh.ASCSignalStaleness) != 0 {
		t.Fatalf("fresh doctor report = %+v, want no stale ASC warning", fresh)
	}
}

func TestLoadASCSalesSeriesSignalAggregatesExplicitWindows(t *testing.T) {
	doc, err := LoadASCSalesSeriesSignal("../../fixtures/product/asc-sales-series.jsonl", ASCSalesSeriesSignalOptions{
		ProductID:    "product:focus-notes",
		AppID:        "123456789",
		Metric:       "units",
		BeforeWindow: "2026-07-01/2026-07-03",
		AfterWindow:  "2026-07-08/2026-07-10",
		ChangeEvent:  "Changed App Store screenshot order.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSignalDocument(doc); err != nil {
		t.Fatal(err)
	}
	if doc.Source.Kind != "app_store_connect" || doc.Source.Ref != "asc:sales_series:123456789:2026-07-01/2026-07-03:2026-07-08/2026-07-10" {
		t.Fatalf("source = %+v", doc.Source)
	}
	s := doc.Signal
	if s.ProductID != "product:focus-notes" || s.Surface != "app_store" || s.MetricName != "app_store_connect_units" {
		t.Fatalf("signal identity = %+v", s)
	}
	if s.ChangeEvent != "Changed App Store screenshot order." || s.Before != 9 || s.After != 18 {
		t.Fatalf("window aggregate = %+v", s)
	}
	if s.Window != "before 2026-07-01/2026-07-03; after 2026-07-08/2026-07-10" {
		t.Fatalf("window = %q", s.Window)
	}
	for _, ref := range []string{
		"asc:sales_series:before_rows:3",
		"asc:sales_series:after_rows:3",
		"asc:sales_series:metric:units",
		"asc:sales_series:before_sum:9",
		"asc:sales_series:after_sum:18",
	} {
		if !hasString(s.EvidenceRefs, ref) {
			t.Fatalf("missing evidence ref %q in %+v", ref, s.EvidenceRefs)
		}
	}
	if !hasString(s.Confounders, "explicit window comparison; causality is not implied") {
		t.Fatalf("confounders = %+v", s.Confounders)
	}
}

func TestLoadASCSalesSeriesSignalRejectsEmptyWindow(t *testing.T) {
	_, err := LoadASCSalesSeriesSignal("../../fixtures/product/asc-sales-series.jsonl", ASCSalesSeriesSignalOptions{
		ProductID:    "product:focus-notes",
		AppID:        "123456789",
		Metric:       "units",
		BeforeWindow: "2026-07-01/2026-07-03",
		AfterWindow:  "2026-08-01/2026-08-03",
		ChangeEvent:  "Changed App Store screenshot order.",
	})
	if err == nil {
		t.Fatal("empty after window should be rejected")
	}
}

func TestSignalDoctorRejectsUnknownExportFieldsAndRecordsNoPromote(t *testing.T) {
	_, err := LoadSignal("../../fixtures/product/source-export.bad-extra.yaml")
	if err == nil {
		t.Fatal("unknown export field should fail strict load")
	}
	report := SignalDoctor("../../fixtures/product/source-export.bad-extra.yaml")
	if report.Status != "fail" || report.SourceHash == "" || !checkStatus(report.Checks, "strict_contract", "fail") {
		t.Fatalf("signal doctor report = %+v", report)
	}
	if report.ProductID != "product:focus-notes" || report.SourceKind != "app_store_connect" {
		t.Fatalf("signal doctor hints = %+v", report)
	}
	home := t.TempDir()
	nop, appended, err := RecordSignalDoctorNoPromote(home, report, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if !appended || nop.ID == "" || nop.OwnerTarget.Kind != "product_signal_source" || !hasString(nop.EvidenceRefs, report.SourceHash) || !hasString(nop.EvidenceRefs, "product_id:product:focus-notes") {
		t.Fatalf("no-promote = %+v appended=%v", nop, appended)
	}
	replayed, appended, err := RecordSignalDoctorNoPromote(home, report, tProduct.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if appended || replayed.ID != nop.ID {
		t.Fatalf("replay no-promote = %+v appended=%v, want existing %s", replayed, appended, nop.ID)
	}
	tl, err := LoadTimeline(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.SourceIssues) != 1 || tl.SourceIssues[0].ID != nop.ID {
		t.Fatalf("timeline source issues = %+v", tl.SourceIssues)
	}
	doctor, err := Doctor(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.SourceIssueCount != 1 || len(doctor.SourceIssues) != 1 || !NeedsWriteback(doctor) {
		t.Fatalf("source issue doctor = %+v", doctor)
	}
	refs := EvidenceRefs(doctor)
	if !hasString(refs, "product_source_issue:"+nop.ID) || !hasString(refs, "product:source_issues:1") {
		t.Fatalf("doctor evidence refs = %+v", refs)
	}
	gate := WritebackGate(doctor, "act_source_issue", "pd_source_issue", tProduct)
	if gate.CandidateKind != "" || gate.CandidateRef == "" || gate.OwnerTarget.Ref != "product:focus-notes" {
		t.Fatalf("source issue gate = %+v", gate)
	}
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	review := validSourceIssueReview(nop.ID)
	reviewReport, err := ApplySourceIssueReview(home, store, review, "fixtures/product/source-issue-review.yaml", tProduct.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reviewReport.Status != "closed" || reviewReport.IssueID != nop.ID || reviewReport.TransitionID == "" || reviewReport.EvidenceID == "" {
		t.Fatalf("source issue review report = %+v", reviewReport)
	}
	closed, err := LoadTimeline(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(closed.SourceIssues) != 1 || closed.SourceIssues[0].Status != "closed" || closed.SourceIssues[0].ReviewID != review.ID {
		t.Fatalf("closed source issues = %+v", closed.SourceIssues)
	}
	if !hasTimelineObject(closed.Transitions, "product_source_issue_review:"+review.ID) {
		t.Fatalf("timeline transitions missing source issue review: %+v", closed.Transitions)
	}
	postReviewDoctor, err := Doctor(home, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if postReviewDoctor.Status != "ok" || postReviewDoctor.SourceIssueCount != 0 || NeedsWriteback(postReviewDoctor) {
		t.Fatalf("post source issue review doctor = %+v", postReviewDoctor)
	}
}

func TestSourceIssueReviewResolvedFollowUpCreatesParkedHypothesis(t *testing.T) {
	home := t.TempDir()
	doc, err := LoadChangeStreamCandidates("../../fixtures/product/change-stream-candidates.md")
	if err != nil {
		t.Fatal(err)
	}
	changeStream, err := ApplyChangeStreamCandidates(home, doc, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	review := SourceIssueReview{
		SchemaVersion: SourceIssueReviewSchemaVersion,
		ID:            "source_issue_review_follow_up",
		ProductID:     "product:focus_notes",
		IssueID:       changeStream.IssueIDs[0],
		Result:        "resolved",
		Rationale:     "reviewed source issue represents a real change worth measuring, not a metric claim yet",
		EvidenceRefs:  []string{"fixture:source-issue-review"},
		FollowUp: &SourceIssueFollowUp{
			ChangeEvent:       "Reviewed change-stream candidate as real App Store screenshot change",
			ExpectedEffect:    "clearer first screenshot may improve product page conversion",
			Metrics:           []string{"app_store_product_page_conversion"},
			ObservationWindow: "14d after reviewed change",
			NextAction:        "import an explicit ProductSignal after the analytics window",
			EvidenceRefs:      []string{"fixture:source-issue-follow-up"},
		},
	}
	invalid := review
	invalid.Result = "false_positive"
	if err := ValidateSourceIssueReview(invalid); err == nil {
		t.Fatal("follow_up should require resolved source issue review")
	}
	report, err := ApplySourceIssueReview(home, store, review, "fixtures/product/source-issue-review.yaml", tProduct.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "closed" || report.HypothesisID == "" || report.TransitionID == "" {
		t.Fatalf("source issue follow-up report = %+v", report)
	}
	hypothesis, err := loadHypothesisCandidate(home, report.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	if hypothesis.Kind != string(candidates.ProductHypothesis) ||
		hypothesis.ProductID != "product:focus_notes" ||
		hypothesis.Verdict != candidates.Park ||
		hypothesis.Result != "unknown" ||
		!hasString(hypothesis.EvidenceRefs, "product_source_issue_review:"+review.ID) ||
		!hasString(hypothesis.EvidenceRefs, "fixture:source-issue-follow-up") {
		t.Fatalf("follow-up hypothesis = %+v", hypothesis)
	}
	tl, err := LoadTimeline(home, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Hypotheses) != 1 || tl.Hypotheses[0].ID != report.HypothesisID {
		t.Fatalf("timeline hypotheses = %+v", tl.Hypotheses)
	}
	if !hasTimelineObject(tl.Transitions, "product_source_issue_review:"+review.ID) {
		t.Fatalf("timeline transitions missing review: %+v", tl.Transitions)
	}
	doctor, err := Doctor(home, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.PendingReviewCount != 1 || doctor.SourceIssueCount != 1 {
		t.Fatalf("doctor should keep follow-up review debt visible: %+v", doctor)
	}
	if _, err := os.Stat(signalsLog(home)); !os.IsNotExist(err) {
		t.Fatalf("follow-up should not create ProductSignal ledger: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("follow-up should not create claims ledger: %v", err)
	}
}

func TestChangeStreamCandidatesRecordProductSourceIssues(t *testing.T) {
	doc, err := LoadChangeStreamCandidates("../../fixtures/product/change-stream-candidates.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.SourceHash == "" || doc.SourceCapture == "" || len(doc.Candidates) != 2 {
		t.Fatalf("change stream doc = %+v", doc)
	}
	if doc.Candidates[0].ProductID != "product:focus_notes" || doc.Candidates[0].Surface != "App Store screenshots" || len(doc.Candidates[0].DirtyPreview) != 3 {
		t.Fatalf("first candidate = %+v", doc.Candidates[0])
	}
	home := t.TempDir()
	report, err := ApplyChangeStreamCandidates(home, doc, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.CandidatesRead != 2 || report.IssuesCreated != 2 || report.IssuesExisting != 0 || len(report.IssueIDs) != 2 {
		t.Fatalf("change stream report = %+v", report)
	}
	replayed, err := ApplyChangeStreamCandidates(home, doc, tProduct.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.IssuesCreated != 0 || replayed.IssuesExisting != 2 || len(replayed.IssueIDs) != 2 || replayed.IssueIDs[0] != report.IssueIDs[0] {
		t.Fatalf("replay report = %+v want existing issues %+v", replayed, report.IssueIDs)
	}
	tl, err := LoadTimeline(home, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.SourceIssues) != 2 {
		t.Fatalf("timeline source issues = %+v", tl.SourceIssues)
	}
	if !strings.Contains(tl.SourceIssues[0].Basis, "review before treating as product change") {
		t.Fatalf("source issue basis should preserve review gate, got %+v", tl.SourceIssues[0])
	}
	doctor, err := Doctor(home, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.SourceIssueCount != 2 || !NeedsWriteback(doctor) {
		t.Fatalf("doctor = %+v", doctor)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("change stream import should not create claims ledger: %v", err)
	}
}

func validBundle() Bundle {
	return Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:x",
			Name:           "X",
			OwnerEntity:    "project:x",
			Surfaces:       []Surface{{ID: "store", Kind: "app_store", Ref: "appstore:1"}},
			Channels:       []string{"app_store"},
			Positioning:    "Focused utility",
			PrivacyPosture: "local_first",
		},
		Signals: []Signal{{
			ID:           "sig_x",
			ProductID:    "product:x",
			Surface:      "store",
			ChangeEvent:  "changed screenshots",
			MetricName:   "conversion",
			Before:       0.1,
			After:        0.12,
			Window:       "7d",
			Confidence:   0.5,
			EvidenceRefs: []string{"fixture"},
		}},
		Hypotheses: []Hypothesis{{
			ID:                "hyp_x",
			ProductID:         "product:x",
			ChangeEvent:       "changed screenshots",
			ExpectedEffect:    "conversion improves",
			Metrics:           []string{"conversion"},
			ObservationWindow: "14d",
			Result:            "unknown",
			NextAction:        "wait",
			EvidenceRefs:      []string{"fixture"},
		}},
		EvidenceRefs: []string{"fixture"},
	}
}

func validReview() Review {
	return Review{
		SchemaVersion: ReviewSchemaVersion,
		ID:            "review_x",
		ProductID:     "product:x",
		HypothesisID:  "hyp_x",
		Result:        "inconclusive",
		Rationale:     "movement exists but evidence is not causal",
		Confidence:    0.5,
		EvidenceRefs:  []string{"fixture"},
		ReviewedBy:    "operator",
	}
}

func validSourceIssueReview(issueID string) SourceIssueReview {
	return SourceIssueReview{
		SchemaVersion: SourceIssueReviewSchemaVersion,
		ID:            "source_issue_review_x",
		ProductID:     "product:focus-notes",
		IssueID:       issueID,
		Result:        "resolved",
		Rationale:     "source contract was reviewed and the adapter/source follow-up is captured",
		EvidenceRefs:  []string{"fixture:source-issue-review"},
	}
}

func validSignalDocument() SignalDocument {
	b := validBundle()
	return SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "app_store_connect", Ref: "fixture"},
		Signal:        b.Signals[0],
	}
}

func validSignalExportDocument() SignalExportDocument {
	return SignalExportDocument{
		SchemaVersion: SignalExportSchemaVersion,
		Source:        SignalSource{Kind: "app_store_connect", Ref: "fixture"},
		ProductID:     "product:x",
		Surface:       "store",
		ChangeEvent:   "changed screenshots",
		Metric: ExportMetric{
			Name:   "product_page_conversion_rate",
			Before: ExportMetricPoint{Value: 0.1, Ref: "fixture:before"},
			After:  ExportMetricPoint{Value: 0.12, Ref: "fixture:after"},
		},
		Window:       "14d",
		Confidence:   0.5,
		EvidenceRefs: []string{"fixture:export"},
	}
}

func hasString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func hasSurfaceRef(values []Surface, want string) bool {
	for _, value := range values {
		if value.Ref == want {
			return true
		}
	}
	return false
}

func checkStatus(checks []SignalCheck, name, status string) bool {
	for _, c := range checks {
		if c.Name == name && c.Status == status {
			return true
		}
	}
	return false
}

func hasTimelineObject(transitions []TimelineTransition, objectRef string) bool {
	for _, tr := range transitions {
		if tr.ObjectRef == objectRef {
			return true
		}
	}
	return false
}

func findSignal(signals []Signal, id string) Signal {
	for _, s := range signals {
		if s.ID == id {
			return s
		}
	}
	return Signal{}
}

func readTransition(t *testing.T, home, id string) envelope.Transition {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	var tr envelope.Transition
	if err := json.Unmarshal(record.Bytes, &tr); err != nil {
		t.Fatal(err)
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(&tr)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if !bytes.Equal(record.Bytes, want) {
		t.Fatalf("product transition bytes changed\ngot:  %q\nwant: %q", record.Bytes, want)
	}
	return tr
}
