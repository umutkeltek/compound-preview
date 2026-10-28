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
)

// essenceBundle is a synthetic curated product that declares its essence, one
// form, one learned fact, and a taste profile.
func essenceBundle() Bundle {
	b := curatedRegistryBundle()
	b.Product.Essence = Essence{
		Audience:    "Anglers who plan trips around tides",
		Problem:     "Tide tables are scattered, ad-heavy, and online-only",
		Value:       "Offline tide and moon planner with one-time Pro unlock",
		Goal:        Goal{IncomeTargetUSDMonth: 1500, Horizon: "2027-06"},
		Constraints: []string{"one-time purchase, no subscriptions", "works offline"},
		AntiGoals:   []string{"ad-supported tier"},
		Stage:       "live",
	}
	b.Product.Forms = []Form{{
		Surface:    "ios-foldable",
		Stage:      "delivery",
		Goal:       "runs on the new size class",
		Parameters: map[string]string{"size_class": "regular", "min_os": "26"},
		Acceptance: []string{"no clipped controls when unfolded"},
		Status:     "proposed",
	}}
	b.Product.Learned = []LearnedFact{{
		At:          time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		Stage:       "measure",
		Fact:        "43 Pro sales in 30 days",
		EvidenceRef: "fixture:sales-report",
	}}
	b.Product.TasteProfile = TasteProfile{
		Standards: []string{"calm utility"},
		RedLines:  []string{"never call it free, as it is not"},
	}
	return b
}

func openEssenceStore(t *testing.T) (string, *mem.Store) {
	t.Helper()
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return home, store
}

// TestEssenceFormLearnedAndTasteBecomeFoldedFacts is the depth test for the
// essence record: every declared field must land as a promoted product fact
// under its documented predicate, and the reader must rebuild the same record
// from the folded current view — not from the YAML it was imported from.
func TestEssenceFormLearnedAndTasteBecomeFoldedFacts(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, essenceBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"essence.audience":                      "Anglers who plan trips around tides",
		"essence.problem":                       "Tide tables are scattered, ad-heavy, and online-only",
		"essence.value":                         "Offline tide and moon planner with one-time Pro unlock",
		"essence.goal.income_target_usd_month":  "1500",
		"essence.goal.horizon":                  "2027-06",
		"essence.constraints":                   `["one-time purchase, no subscriptions","works offline"]`,
		"essence.anti_goals":                    `["ad-supported tier"]`,
		"essence.stage":                         "live",
		"form.ios-foldable.delivery.goal":       "runs on the new size class",
		"form.ios-foldable.delivery.parameters": `{"min_os":"26","size_class":"regular"}`,
		"form.ios-foldable.delivery.acceptance": `["no clipped controls when unfolded"]`,
		"form.ios-foldable.delivery.status":     "proposed",
		"learned.1":                             `{"at":"2026-09-29T00:00:00Z","stage":"measure","fact":"43 Pro sales in 30 days","evidence_ref":"fixture:sales-report"}`,
		"taste.standards":                       `["calm utility"]`,
		"taste.red_lines":                       `["never call it free, as it is not"]`,
	}
	for predicate, object := range want {
		if got := currentObject(t, store, "product:harbor", predicate); got != object {
			t.Errorf("%s = %q, want %q", predicate, got, object)
		}
	}

	record, err := LoadEssence(store, "Harbor")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != EssenceRecorded || record.ProductID != "product:harbor" {
		t.Fatalf("record status/product = %s/%s, want recorded/product:harbor", record.Status, record.ProductID)
	}
	declared := essenceBundle().Product
	if !jsonEqual(t, record.Essence, declared.Essence) {
		t.Errorf("essence round trip:\n got %+v\nwant %+v", record.Essence, declared.Essence)
	}
	if !jsonEqual(t, record.Forms, declared.Forms) {
		t.Errorf("forms round trip:\n got %+v\nwant %+v", record.Forms, declared.Forms)
	}
	if !jsonEqual(t, record.Learned, declared.Learned) {
		t.Errorf("learned round trip:\n got %+v\nwant %+v", record.Learned, declared.Learned)
	}
	if !jsonEqual(t, record.Taste, declared.TasteProfile) {
		t.Errorf("taste round trip:\n got %+v\nwant %+v", record.Taste, declared.TasteProfile)
	}
}

// TestEssenceUnknownIsRecordedLiterally: a declared essence with a field nobody
// could source records the literal answer "unknown" rather than leaving the
// slot empty, so a reader can tell "asked and unknown" from "never declared".
func TestEssenceUnknownIsRecordedLiterally(t *testing.T) {
	home, store := openEssenceStore(t)
	b := curatedRegistryBundle()
	b.Product.Essence = Essence{Audience: "Anglers", Stage: "live"}
	if _, err := Apply(home, store, b, "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	for _, predicate := range []string{"essence.problem", "essence.value", "essence.goal.income_target_usd_month", "essence.goal.horizon", "essence.constraints", "essence.anti_goals"} {
		if got := currentObject(t, store, "product:harbor", predicate); got != EssenceUnknown {
			t.Errorf("%s = %q, want the literal %q", predicate, got, EssenceUnknown)
		}
	}
	record, err := LoadEssence(store, "harbor")
	if err != nil {
		t.Fatal(err)
	}
	if record.Essence.Problem != EssenceUnknown || record.Essence.Goal.IncomeTargetUSDMonth != 0 || len(record.Essence.Constraints) != 0 {
		t.Errorf("unknown fields read back as %+v", record.Essence)
	}
}

// TestEssenceReimportSupersedesThroughTheCurrentView: a changed essence is a
// new version of the same fact slot, so the reader sees only the newest value.
func TestEssenceReimportSupersedesThroughTheCurrentView(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, essenceBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	changed := essenceBundle()
	changed.Product.Essence.Audience = "Coastal kayakers"
	changed.Product.Essence.Stage = "growth"
	if _, err := Apply(home, store, changed, "harbor.yaml", tProduct.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	record, err := LoadEssence(store, "product:harbor")
	if err != nil {
		t.Fatal(err)
	}
	if record.Essence.Audience != "Coastal kayakers" || record.Essence.Stage != "growth" {
		t.Errorf("reimported essence = %+v, want the superseding values", record.Essence)
	}
}

// TestContractImportNeverRecordsImporterTaste: a project contract has no taste
// section; the importer's boilerplate taste profile is provenance text, not a
// product standard, so a contract import must never write or override taste.*.
func TestContractImportNeverRecordsImporterTaste(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, essenceBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	contract, err := LoadProjectContractBundle(writeContract(t, "project.yaml", curatedContract), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, contract, "project.yaml", tProduct.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := currentObject(t, store, "product:harbor", "taste.standards"); got != `["calm utility"]` {
		t.Errorf("taste.standards = %q, want the curated standards kept", got)
	}
	if got := currentObject(t, store, "product:harbor", "essence.audience"); got != "Anglers who plan trips around tides" {
		t.Errorf("essence.audience = %q, want the curated essence kept", got)
	}

	fresh, freshStore := openEssenceStore(t)
	if _, err := Apply(fresh, freshStore, contract, "project.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	if got := currentObject(t, freshStore, "product:harbor", "taste.standards"); got != "" {
		t.Errorf("contract-only import recorded taste.standards = %q; importer boilerplate is not a product standard", got)
	}
}

// TestValidateRejectsUnaddressableFormsAndUnsourcedLearning: a form key with a
// dot would break the form.<surface>.<stage>.<field> predicate, and a learned
// fact without evidence or a date is an assertion, not something learned.
func TestValidateRejectsUnaddressableFormsAndUnsourcedLearning(t *testing.T) {
	cases := map[string]func(*Product){
		"dotted surface":  func(p *Product) { p.Forms[0].Surface = "ios.foldable" },
		"empty stage":     func(p *Product) { p.Forms[0].Stage = "" },
		"duplicate form":  func(p *Product) { p.Forms = append(p.Forms, p.Forms[0]) },
		"learned no ref":  func(p *Product) { p.Learned[0].EvidenceRef = "" },
		"learned no fact": func(p *Product) { p.Learned[0].Fact = " " },
		"learned no date": func(p *Product) { p.Learned[0].At = time.Time{} },
		"negative target": func(p *Product) { p.Essence.Goal.IncomeTargetUSDMonth = -1 },
	}
	for name, mutate := range cases {
		b := essenceBundle()
		mutate(&b.Product)
		if err := Validate(b); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	if err := Validate(essenceBundle()); err != nil {
		t.Fatalf("valid essence bundle rejected: %v", err)
	}
}

// TestLoadEssenceDistinguishesNoEssenceFromUnknownProduct.
func TestLoadEssenceDistinguishesNoEssenceFromUnknownProduct(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, curatedRegistryBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	record, err := LoadEssence(store, "harbor")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != EssenceNone || record.ProductID != "product:harbor" {
		t.Errorf("product without essence = %s/%s, want none/product:harbor", record.Status, record.ProductID)
	}
	record, err = LoadEssence(store, "no-such-product")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != WhereUnresolved {
		t.Errorf("unknown product status = %s, want unresolved", record.Status)
	}
}

func TestEssenceSummaryIsShortAndNamesTheInvariant(t *testing.T) {
	summary := EssenceSummary(essenceBundle().Product.Essence)
	for _, want := range []string{"audience: Anglers who plan trips around tides", "problem: Tide tables", "goal: USD 1500/month by 2027-06", "stage: live", "constraints: one-time purchase, no subscriptions; works offline"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q missing %q", summary, want)
		}
	}
	if strings.Contains(summary, "ad-supported") || strings.Contains(summary, "Offline tide and moon") {
		t.Errorf("summary carries fields beyond audience/problem/goal/stage/constraints: %q", summary)
	}
	if got := EssenceSummary(Essence{Stage: "live"}); !strings.Contains(got, "goal: unknown") || !strings.Contains(got, "audience: unknown") {
		t.Errorf("sparse summary %q should say unknown", got)
	}
}

func TestRunEssenceCLIJSONAndHuman(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, essenceBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := RunEssenceCLI([]string{"essence", "harbor", "--json"}, store, &out, &errOut); code != 0 {
		t.Fatalf("essence --json exit %d: %s", code, errOut.String())
	}
	var got EssenceRecord
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("essence --json is not the record: %v\n%s", err, out.String())
	}
	if got.ProductID != "product:harbor" || got.Essence.Stage != "live" || len(got.Forms) != 1 || len(got.Learned) != 1 {
		t.Errorf("essence --json = %+v", got)
	}

	out.Reset()
	if code := RunEssenceCLI([]string{"essence", "harbor"}, store, &out, &errOut); code != 0 {
		t.Fatalf("essence exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"# product essence harbor", "- product: product:harbor", "- audience: Anglers who plan trips around tides", "- goal: USD 1500/month by 2027-06", "- form ios-foldable/delivery: runs on the new size class [proposed]", "- learned 2026-09-29 (measure): 43 Pro sales in 30 days [fixture:sales-report]"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human essence output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := RunEssenceCLI([]string{"essence", "nope"}, store, &out, &errOut); code != 1 {
		t.Errorf("unresolved product exit = %d, want 1", code)
	}
}

// markerHeadings are the sections the product-marketing skill populates and the
// marketing skills read from .agents/product-marketing.md.
var markerHeadings = []string{
	"## Product Overview", "## Target Audience", "## Personas", "## Problems & Pain Points",
	"## Competitive Landscape", "## Differentiation", "## Objections & Anti-Personas",
	"## Switching Dynamics", "## Customer Language", "## Brand Voice", "## Proof Points", "## Goals",
}

func TestRenderMarketingWritesTheSkillHeadingsFromEssence(t *testing.T) {
	home, store := openEssenceStore(t)
	b := essenceBundle()
	b.Product.Surfaces = append(b.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: "/workspace/harbor"})
	if _, err := Apply(home, store, b, "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), ".agents", "product-marketing.md")
	var out, errOut bytes.Buffer
	if code := RunEssenceCLI([]string{"render-marketing", "harbor", "--out", outPath}, store, &out, &errOut); code != 0 {
		t.Fatalf("render-marketing exit %d: %s", code, errOut.String())
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	last := -1
	for _, heading := range markerHeadings {
		i := strings.Index(text, "\n"+heading+"\n")
		if i < 0 {
			t.Errorf("rendered file missing heading %q", heading)
			continue
		}
		if i < last {
			t.Errorf("heading %q out of order", heading)
		}
		last = i
	}
	for _, want := range []string{
		"Anglers who plan trips around tides",
		"Tide tables are scattered, ad-heavy, and online-only",
		"Offline tide and moon planner with one-time Pro unlock",
		"43 Pro sales in 30 days",
		"never call it free, as it is not",
		"USD 1500/month",
		"ios-foldable / delivery",
		"compound product render-marketing product:harbor",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered file missing %q", want)
		}
	}
	if !strings.Contains(text, "## Personas\n\n- unknown\n") {
		t.Errorf("a section the essence cannot fill must say unknown:\n%s", text)
	}
	if strings.Contains(text, "/workspace/") {
		t.Errorf("rendered marketing file leaked a local surface path:\n%s", text)
	}

	// A re-render replaces its own output; a hand-written file is refused.
	if code := RunEssenceCLI([]string{"render-marketing", "harbor", "--out", outPath}, store, &out, &errOut); code != 0 {
		t.Fatalf("re-render over a rendered file exit %d: %s", code, errOut.String())
	}
	handPath := filepath.Join(t.TempDir(), "product-marketing.md")
	if err := os.WriteFile(handPath, []byte("# hand-written context\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := RunEssenceCLI([]string{"render-marketing", "harbor", "--out", handPath}, store, &out, &errOut); code != 1 {
		t.Errorf("render over a hand-written file exit %d, want 1", code)
	}
	if body, _ := os.ReadFile(handPath); string(body) != "# hand-written context\n" {
		t.Errorf("hand-written file was overwritten: %q", body)
	}
	if code := RunEssenceCLI([]string{"render-marketing", "harbor", "--out", handPath, "--force"}, store, &out, &errOut); code != 0 {
		t.Errorf("render --force exit %d: %s", code, errOut.String())
	}

	out.Reset()
	if code := RunEssenceCLI([]string{"render-marketing", "harbor", "--out", "-"}, store, &out, &errOut); code != 0 || !strings.Contains(out.String(), "## Target Audience") {
		t.Errorf("render --out - should print the file: exit %d\n%s", code, out.String())
	}
}

// TestRenderMarketingDefaultsToTheProductCheckout: without --out the file goes
// to <product checkout>/.agents/product-marketing.md, the path the marketing
// skills read. The checkout here is a temp dir standing in for a product repo.
func TestRenderMarketingDefaultsToTheProductCheckout(t *testing.T) {
	home, store := openEssenceStore(t)
	checkout := t.TempDir()
	b := essenceBundle()
	b.Product.Surfaces = append(b.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: checkout})
	if _, err := Apply(home, store, b, "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := RunEssenceCLI([]string{"render-marketing", "harbor"}, store, &out, &errOut); code != 0 {
		t.Fatalf("render-marketing exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(checkout, ".agents", "product-marketing.md")); err != nil {
		t.Fatalf("default render path not written: %v", err)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(ja) == string(jb)
}
