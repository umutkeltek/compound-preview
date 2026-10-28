package product

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/work"
)

var tPlatform = time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)

const platformTestSignalID = "foldphone-20261006"

var platformTestAcceptance = []string{
	"No `UIScreen.main` or hard-coded screen sizes in source.",
	"Fold/unfold mid-session keeps current screen, form input, and any open purchase sheet.",
	"Buy/Restore stay visible on the outer screen.",
}

// platformTestDocument is a platform_change signal for the ios surface with
// one machine prerequisite list, shaped like the iPhone Duo record.
func platformTestDocument() PlatformChangeDocument {
	return PlatformChangeDocument{
		SchemaVersion: SignalSchemaVersion,
		Kind:          PlatformChangeKind,
		ID:            platformTestSignalID,
		Title:         "Foldphone ships on iOS 27",
		Surface:       "ios",
		EffectiveDate: "2026-10-23",
		Sources: []PlatformSource{
			{URL: "https://example.invalid/newsroom/foldphone", Claim: "Available 2026-10-23"},
			{URL: "https://example.invalid/foldphone/prepare", Claim: "Build with Xcode 27.1 for full-screen layout"},
		},
		Facts: []string{"Runs iOS 27 (not iPadOS).", "Inner screen ignores declared orientations."},
		FormDelta: PlatformFormDelta{
			Stage:      "delivery",
			Goal:       "runs and sells on the foldphone",
			Parameters: map[string]string{"sdk": "ios27.1", "xcode": "27.1"},
			Acceptance: append([]string(nil), platformTestAcceptance...),
		},
		Prerequisites: []string{"Xcode 27.1 installed", "Foldphone simulator created"},
	}
}

const platformTestProofContract = `{
  "schema_version": "compound.proof_consumers.v1",
  "profile": "xcode_consumers_v1",
  "consumers": [
    {
      "id": "ios-app",
      "command": ["bash", "Tools/compound_proof.sh"],
      "dir": ".",
      "timeout_seconds": 1800,
      "required_tools": ["bash", "xcodebuild"],
      "report_kind": "xcresult",
      "required_tests": ["HarborTests/testTideTableLoadsOffline()"]
    }
  ]
}
`

// seedPlatformProducts imports three products into a throwaway home: harbor
// (ios channel, declared essence, a checkout with compound-proof.json), beacon
// (an ios: surface ref, no essence, no checkout), and webby (web only).
func seedPlatformProducts(t *testing.T) (home string, store *mem.Store, harborCheckout string) {
	t.Helper()
	home, store = openEssenceStore(t)
	harborCheckout = filepath.Join(t.TempDir(), "harbor-app")
	if err := os.MkdirAll(harborCheckout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(harborCheckout, "compound-proof.json"), []byte(platformTestProofContract), 0o644); err != nil {
		t.Fatal(err)
	}
	harbor := essenceBundle()
	harbor.Product.Forms = nil
	harbor.Product.Channels = []string{"app_store", "ios"}
	harbor.Product.Surfaces = append(harbor.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: harborCheckout})
	beacon := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:beacon",
			Name:           "Beacon",
			OwnerEntity:    "project:beacon",
			Surfaces:       []Surface{{ID: "app", Kind: "app_bundle", Ref: "ios:com.example.beacon"}},
			Channels:       []string{"app_store"},
			Positioning:    "Lighthouse finder.",
			PrivacyPosture: "local_first",
		},
		EvidenceRefs: []string{"fixture:beacon"},
	}
	webby := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:webby",
			Name:           "Webby",
			OwnerEntity:    "project:webby",
			Surfaces:       []Surface{{ID: "site", Kind: "web", Ref: "https://webby.example.invalid"}},
			Channels:       []string{"web", "landing_page"},
			Positioning:    "A web-only product.",
			PrivacyPosture: "hosted",
		},
		EvidenceRefs: []string{"fixture:webby"},
	}
	for _, b := range []Bundle{harbor, beacon, webby} {
		if _, err := Apply(home, store, b, strings.TrimPrefix(b.Product.ID, "product:")+".yaml", tProduct); err != nil {
			t.Fatal(err)
		}
	}
	return home, store, harborCheckout
}

func createPlansFor(t *testing.T, home string) map[string][]work.CreatePlan {
	t.Helper()
	out := map[string][]work.CreatePlan{}
	entries, err := os.ReadDir(filepath.Join(home, "create-plans"))
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		plan, err := work.LoadCreatePlan(filepath.Join(home, "create-plans", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[plan.WorkOwner] = append(out[plan.WorkOwner], plan)
	}
	return out
}

func platformFormOf(t *testing.T, store *mem.Store, query string) (Form, bool) {
	t.Helper()
	record, err := LoadEssence(store, query)
	if err != nil {
		t.Fatal(err)
	}
	var found []Form
	for _, f := range record.Forms {
		if f.Surface == platformTestSignalID {
			found = append(found, f)
		}
	}
	if len(found) > 1 {
		t.Fatalf("%s holds %d forms for signal %s, want at most one: %+v", query, len(found), platformTestSignalID, found)
	}
	if len(found) == 0 {
		return Form{}, false
	}
	return found[0], true
}

// TestPlatformSignalIntakeProposesOneFormAndWorkPerMatchingProduct is the
// acceptance test for S6: a platform_change signal for surface ios yields one
// proposed Form and one proposed work plan per product whose channels or
// surfaces name ios, exactly one shared prerequisite work plan, and a skipped
// row (with a reason) for the web-only product.
func TestPlatformSignalIntakeProposesOneFormAndWorkPerMatchingProduct(t *testing.T) {
	home, store, _ := seedPlatformProducts(t)
	report, err := IntakePlatformSignal(home, store, platformTestDocument(), "foldphone.yaml", tPlatform, false)
	if err != nil {
		t.Fatal(err)
	}
	var matched []string
	for _, m := range report.Matched {
		matched = append(matched, m.ProductID)
		if m.FormStatus != "new" || m.Work.Status != "new" {
			t.Errorf("%s form/work status = %s/%s, want new/new", m.ProductID, m.FormStatus, m.Work.Status)
		}
		if m.Work.State != "proposed" || m.Work.PlanPath == "" {
			t.Errorf("%s work state/plan = %s/%q, want a recorded proposed plan", m.ProductID, m.Work.State, m.Work.PlanPath)
		}
	}
	if strings.Join(matched, ",") != "product:beacon,product:harbor" {
		t.Fatalf("matched = %v, want beacon and harbor", matched)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].ProductID != "product:webby" || !strings.Contains(report.Skipped[0].Reason, "ios") {
		t.Fatalf("skipped = %+v, want webby with a reason naming ios", report.Skipped)
	}
	if report.Prerequisite == nil || report.Prerequisite.Status != "new" || report.Prerequisite.PlanPath == "" {
		t.Fatalf("prerequisite = %+v, want one new recorded plan", report.Prerequisite)
	}
	if c := report.Counts; c.FormsNew != 2 || c.WorkNew != 3 || c.FormsSuperseded != 0 || c.WorkUnchanged != 0 {
		t.Fatalf("counts = %+v, want 2 new forms and 3 new work plans", c)
	}

	plans := createPlansFor(t, home)
	total := 0
	for owner, list := range plans {
		total += len(list)
		for _, p := range list {
			if p.WorkState != "proposed" || p.Work.State != "proposed" || p.Mode != "proposal_only" {
				t.Errorf("%s plan %s state/mode = %s/%s, want proposed/proposal_only", owner, p.ID, p.WorkState, p.Mode)
			}
			if p.ActionContext == nil || p.ActionContext.Kind != "product_platform_signal" || p.ActionContext.SourceID != platformTestSignalID {
				t.Errorf("%s plan %s action context = %+v, want the platform signal", owner, p.ID, p.ActionContext)
			}
		}
	}
	if total != 3 || len(plans["product:harbor"]) != 1 || len(plans["product:beacon"]) != 1 || len(plans["product:webby"]) != 0 {
		t.Fatalf("recorded plans by owner = %d total %v, want one each for harbor, beacon, and the shared prerequisite", total, plans)
	}

	for _, query := range []string{"Harbor", "Beacon"} {
		form, ok := platformFormOf(t, store, query)
		if !ok {
			t.Fatalf("%s has no form for the signal", query)
		}
		want := Form{Surface: platformTestSignalID, Stage: "delivery", Goal: "runs and sells on the foldphone",
			Parameters: map[string]string{"sdk": "ios27.1", "xcode": "27.1"}, Acceptance: platformTestAcceptance, Status: "proposed"}
		if !jsonEqual(t, form, want) {
			t.Errorf("%s form = %+v, want %+v", query, form, want)
		}
	}
	if _, ok := platformFormOf(t, store, "Webby"); ok {
		t.Fatal("web-only product received an ios form")
	}
}

// TestPlatformSignalIntakeIsIdempotentAndSupersedesAChangedDelta: re-running
// the same signal files nothing new; a changed form_delta supersedes the form
// in place (still one form per signal) and re-plans only the product work.
func TestPlatformSignalIntakeIsIdempotentAndSupersedesAChangedDelta(t *testing.T) {
	home, store, _ := seedPlatformProducts(t)
	doc := platformTestDocument()
	if _, err := IntakePlatformSignal(home, store, doc, "foldphone.yaml", tPlatform, false); err != nil {
		t.Fatal(err)
	}
	again, err := IntakePlatformSignal(home, store, doc, "foldphone.yaml", tPlatform.Add(time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if c := again.Counts; c.FormsNew != 0 || c.FormsSuperseded != 0 || c.FormsUnchanged != 2 || c.WorkNew != 0 || c.WorkReplanned != 0 || c.WorkUnchanged != 3 {
		t.Fatalf("re-intake counts = %+v, want everything unchanged", c)
	}
	plans := createPlansFor(t, home)
	if n := len(plans["product:harbor"]) + len(plans["product:beacon"]); n != 2 {
		t.Fatalf("re-intake left %d product plans, want 2", n)
	}

	changed := platformTestDocument()
	changed.FormDelta.Acceptance = append(changed.FormDelta.Acceptance, "Info.plist has `UILaunchScreen`.")
	third, err := IntakePlatformSignal(home, store, changed, "foldphone.yaml", tPlatform.Add(2*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if c := third.Counts; c.FormsSuperseded != 2 || c.FormsNew != 0 || c.WorkReplanned != 2 || c.WorkUnchanged != 1 || c.WorkNew != 0 {
		t.Fatalf("changed-delta counts = %+v, want 2 superseded forms, 2 re-planned product items, prerequisite unchanged", c)
	}
	form, ok := platformFormOf(t, store, "Harbor")
	if !ok || len(form.Acceptance) != 4 || form.Acceptance[3] != "Info.plist has `UILaunchScreen`." || form.Status != "proposed" {
		t.Fatalf("harbor form after changed delta = %+v, want the four-line acceptance", form)
	}
	want, _ := json.Marshal(changed.FormDelta.Acceptance)
	if got := currentObject(t, store, "product:harbor", "form."+platformTestSignalID+".delivery.acceptance"); got != string(want) {
		t.Fatalf("current acceptance fact = %s, want %s", got, want)
	}
	plans = createPlansFor(t, home)
	if len(plans["product:harbor"]) != 2 {
		t.Fatalf("harbor plans after changed delta = %d, want the original plus one re-plan", len(plans["product:harbor"]))
	}
	for _, m := range third.Matched {
		if len(m.Work.PriorPlanIDs) != 1 {
			t.Errorf("%s re-plan names prior plans %v, want the one it replaces", m.ProductID, m.Work.PriorPlanIDs)
		}
		if !strings.Contains(strings.Join(m.Work.ActionContext.ExpectedValidation, "\n"), "UILaunchScreen") {
			t.Errorf("%s re-plan does not carry the changed acceptance", m.ProductID)
		}
	}
}

// TestPlatformSignalWorkContractCarriesEssenceAcceptanceRecipeAndSources is the
// depth test for the task contract: the recorded plan (not the report) must
// carry the product id, the orient essence block, every acceptance line, the
// checkout's compound-proof.json command, the signal sources, and the shared
// prerequisite work id; a product without a checkout says so.
func TestPlatformSignalWorkContractCarriesEssenceAcceptanceRecipeAndSources(t *testing.T) {
	home, store, checkout := seedPlatformProducts(t)
	report, err := IntakePlatformSignal(home, store, platformTestDocument(), "foldphone.yaml", tPlatform, false)
	if err != nil {
		t.Fatal(err)
	}
	plans := createPlansFor(t, home)
	harbor := plans["product:harbor"][0]
	ctx := harbor.ActionContext
	contract := strings.Join(append(append(append([]string{ctx.Title, ctx.ActionSummary, ctx.Proof, ctx.Boundary}, ctx.RequiredInputs...), ctx.ExpectedValidation...), ctx.EvidenceRefs...), "\n")

	record, err := LoadEssence(store, "Harbor")
	if err != nil {
		t.Fatal(err)
	}
	essenceBlock := "Product essence (product:harbor): " + EssenceSummary(record.Essence)
	mustContain := []string{
		"product: product:harbor",
		essenceBlock,
		"audience: Anglers who plan trips around tides",
		"form delta " + platformTestSignalID + "/delivery",
		"runs and sells on the foldphone",
		"sdk=ios27.1",
		"`bash Tools/compound_proof.sh` in " + checkout,
		"https://example.invalid/newsroom/foldphone",
		"https://example.invalid/foldphone/prepare",
		report.Prerequisite.WorkID,
	}
	for _, line := range platformTestAcceptance {
		mustContain = append(mustContain, "acceptance: "+line)
	}
	for _, want := range mustContain {
		if !strings.Contains(contract, want) {
			t.Errorf("harbor contract missing %q\n--- contract ---\n%s", want, contract)
		}
	}
	if !strings.Contains(ctx.Proof, "Tools/compound_proof.sh") {
		t.Errorf("harbor proof = %q, want the recipe command", ctx.Proof)
	}
	if harbor.Work.Owner != "product:harbor" || harbor.Work.ProposalKind != "ordinary" {
		t.Errorf("harbor work owner/kind = %s/%s, want product:harbor/ordinary", harbor.Work.Owner, harbor.Work.ProposalKind)
	}

	beacon := plans["product:beacon"][0].ActionContext
	if !strings.Contains(beacon.Proof, "no recipe recorded") {
		t.Errorf("beacon proof = %q, want no recipe recorded", beacon.Proof)
	}
	if !strings.Contains(strings.Join(beacon.RequiredInputs, "\n"), "Product essence (product:beacon): none recorded") {
		t.Errorf("beacon inputs do not say its essence is missing: %v", beacon.RequiredInputs)
	}

	var prereq []work.CreatePlan
	for owner, list := range plans {
		if owner != "product:harbor" && owner != "product:beacon" {
			prereq = append(prereq, list...)
		}
	}
	if len(prereq) != 1 || prereq[0].WorkID != report.Prerequisite.WorkID {
		t.Fatalf("prerequisite plans = %d, want exactly one for %s", len(prereq), report.Prerequisite.WorkID)
	}
	checks := strings.Join(prereq[0].ActionContext.ExpectedValidation, "\n")
	for _, p := range platformTestDocument().Prerequisites {
		if !strings.Contains(checks, p) {
			t.Errorf("prerequisite plan missing %q: %s", p, checks)
		}
	}
}

// TestPlatformSignalCLIDryRunPrintsThePlanAndWritesNothing drives the
// `compound product signal <yaml> --dry-run` door: the JSON names matched,
// skipped (with reason), forms, and work items, and the home gains no plan
// and no form fact.
func TestPlatformSignalCLIDryRunPrintsThePlanAndWritesNothing(t *testing.T) {
	home, store, _ := seedPlatformProducts(t)
	path := filepath.Join(t.TempDir(), "foldphone.yaml")
	if err := os.WriteFile(path, []byte(platformTestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	handled, code := RunPlatformSignalCLI([]string{path, "--dry-run"}, home, store, &stdout, &stderr, tPlatform)
	if !handled || code != 0 {
		t.Fatalf("handled/code = %t/%d, stderr: %s", handled, code, stderr.String())
	}
	var report PlatformIntakeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("dry-run output is not the JSON plan: %v\n%s", err, stdout.String())
	}
	if report.Mode != "dry_run" || len(report.Matched) != 2 || len(report.Skipped) != 1 || report.Prerequisite == nil {
		t.Fatalf("dry-run plan = %+v", report)
	}
	if report.Matched[0].Form.Status != "proposed" || report.Matched[0].Work.WorkID == "" || report.Skipped[0].Reason == "" {
		t.Fatalf("dry-run plan rows incomplete: %+v / %+v", report.Matched[0], report.Skipped[0])
	}
	if plans := createPlansFor(t, home); len(plans) != 0 {
		t.Fatalf("dry run recorded plans: %v", plans)
	}
	if _, ok := platformFormOf(t, store, "Harbor"); ok {
		t.Fatal("dry run recorded a form fact")
	}

	stdout.Reset()
	handled, code = RunPlatformSignalCLI([]string{path}, home, store, &stdout, &stderr, tPlatform)
	if !handled || code != 0 || !strings.Contains(stdout.String(), "product:harbor") {
		t.Fatalf("apply handled/code = %t/%d out=%s err=%s", handled, code, stdout.String(), stderr.String())
	}
	if plans := createPlansFor(t, home); len(plans["product:harbor"]) != 1 {
		t.Fatalf("apply did not record harbor's plan: %v", plans)
	}
}

// TestPlatformSignalCLILeavesOtherSignalDocumentsAlone: a metric signal
// document and flag-driven sources are not this door's; it declines them so
// the existing `product signal` path still owns them.
func TestPlatformSignalCLILeavesOtherSignalDocumentsAlone(t *testing.T) {
	home, store := openEssenceStore(t)
	path := filepath.Join(t.TempDir(), "metric.yaml")
	if err := os.WriteFile(path, []byte("schema_version: compound.product_signal.v0.1\nsource: {kind: posthog, ref: r}\nsignal: {id: s1, product_id: product:harbor}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{{path}, {"--asc-datapoints", path, "--product", "product:harbor"}} {
		if handled, _ := RunPlatformSignalCLI(args, home, store, &stdout, &stderr, tPlatform); handled {
			t.Fatalf("platform door claimed %v", args)
		}
	}
}

// TestPlatformChangeDocumentValidation: the kind is strict — unknown fields
// and a delta without acceptance are refused before anything is read.
func TestPlatformChangeDocumentValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, ok, err := LoadPlatformChange(write("ok.yaml", platformTestYAML)); !ok || err != nil {
		t.Fatalf("valid document: ok=%t err=%v", ok, err)
	}
	cases := map[string]string{
		"unknown field":   strings.Replace(platformTestYAML, "surface: ios", "surface: ios\nseverity: high", 1),
		"no acceptance":   strings.Replace(platformTestYAML, "  acceptance:\n    - \"Buy/Restore stay visible on the outer screen.\"\n", "", 1),
		"bad date":        strings.Replace(platformTestYAML, "2026-10-23", "Oct 23", 1),
		"dotted id":       strings.Replace(platformTestYAML, "id: foldphone-20261006", "id: foldphone.20261006", 1),
		"source sans url": strings.Replace(platformTestYAML, "url: https://example.invalid/newsroom/foldphone", "url: newsroom", 1),
	}
	for name, body := range cases {
		if body == platformTestYAML {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, ok, err := LoadPlatformChange(write(strings.ReplaceAll(name, " ", "-")+".yaml", body)); !ok || err == nil {
			t.Errorf("%s: ok=%t err=%v, want a refusal", name, ok, err)
		}
	}
}

const platformTestYAML = `schema_version: compound.product_signal.v0.1
kind: platform_change
id: foldphone-20261006
title: Foldphone ships on iOS 27
surface: ios
effective_date: "2026-10-23"
sources:
  - url: https://example.invalid/newsroom/foldphone
    claim: Available 2026-10-23
facts:
  - Runs iOS 27 (not iPadOS).
form_delta:
  stage: delivery
  goal: runs and sells on the foldphone
  parameters:
    sdk: ios27.1
  acceptance:
    - "Buy/Restore stay visible on the outer screen."
prerequisites:
  - Xcode 27.1 installed
`

// TestPlatformSignalCountsAppStoreAsIOS: the ledger records channels, not
// surface kinds, and this portfolio's App Store apps are iPhone apps, so an
// app_store channel or an appstore:/app_store: surface ref puts a product on
// the ios surface. The reason names the exact channel or ref that matched, an
// exact ios match outranks the App Store alias, and a web-only product is
// still skipped.
func TestPlatformSignalCountsAppStoreAsIOS(t *testing.T) {
	home, store, _ := seedPlatformProducts(t)
	extra := []Bundle{
		{SchemaVersion: SchemaVersion, EvidenceRefs: []string{"fixture:tidal"}, Product: Product{
			ID: "product:tidal", Name: "Tidal", OwnerEntity: "project:tidal", Positioning: "Tide alarms.", PrivacyPosture: "local_first",
			Surfaces: []Surface{{ID: "site", Kind: "web", Ref: "https://tidal.example.invalid"}},
			Channels: []string{"app_store"},
		}},
		{SchemaVersion: SchemaVersion, EvidenceRefs: []string{"fixture:dock"}, Product: Product{
			ID: "product:dock", Name: "Dock", OwnerEntity: "project:dock", Positioning: "Marina finder.", PrivacyPosture: "local_first",
			Surfaces: []Surface{{ID: "listing", Kind: "app_store", Ref: "appstore:id-dock-fixture"}},
			Channels: []string{"web"},
		}},
	}
	for _, b := range extra {
		if _, err := Apply(home, store, b, strings.TrimPrefix(b.Product.ID, "product:")+".yaml", tProduct); err != nil {
			t.Fatal(err)
		}
	}
	report, err := IntakePlatformSignal(home, store, platformTestDocument(), "foldphone.yaml", tPlatform, true)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, m := range report.Matched {
		reasons[m.ProductID] = m.MatchReason
	}
	want := map[string]string{
		"product:harbor": "channel ios",
		"product:beacon": "surface ios:com.example.beacon",
		"product:tidal":  "channel app_store",
		"product:dock":   "surface appstore:id-dock-fixture",
	}
	if !jsonEqual(t, reasons, want) {
		t.Fatalf("match reasons = %v, want %v", reasons, want)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].ProductID != "product:webby" || !strings.Contains(report.Skipped[0].Reason, "app_store") {
		t.Fatalf("skipped = %+v, want only webby, with a reason naming what was looked for", report.Skipped)
	}
}
