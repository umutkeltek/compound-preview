package product

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
)

// twoOfEachBundle declares two forms, two learned facts, and both taste lists,
// so a re-import can drop one of each and keep the other.
func twoOfEachBundle() Bundle {
	b := essenceBundle()
	b.Product.Forms = append(b.Product.Forms, Form{
		Surface:    "ios-phone",
		Stage:      "release",
		Goal:       "ships the one-time Pro unlock",
		Acceptance: []string{"restore purchases works offline"},
		Status:     "accepted",
	})
	b.Product.Learned = append([]LearnedFact{{
		At:          time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Stage:       "measure",
		Fact:        "first paid unit from search ads",
		EvidenceRef: "fixture:ads-report",
	}}, b.Product.Learned...)
	return b
}

// viewObjects is every predicate the folded current view holds for entity.
func viewObjects(t *testing.T, store *mem.Store, entity string) map[string]string {
	t.Helper()
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range view.Claims() {
		if c.Entity == entity {
			out[c.Predicate] = c.Object
		}
	}
	return out
}

// TestReimportRetractsFormsLearnedAndTasteNoLongerDeclared is the depth test
// for retraction. The registry bundle is the one authority for a product's
// forms, learned facts, and taste, so an entry it stops declaring must stop
// being current — the same way a channel dropped from `channels` does — while
// the ledger keeps the old value as superseded history, not a deletion.
func TestReimportRetractsFormsLearnedAndTasteNoLongerDeclared(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, twoOfEachBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	before := viewObjects(t, store, "product:harbor")
	for _, predicate := range []string{"form.ios-foldable.delivery.goal", "learned.2", "taste.red_lines"} {
		if before[predicate] == "" {
			t.Fatalf("setup: %s not recorded by the first import", predicate)
		}
	}

	// Drop the foldable form, the older learned fact (so learned.2's value
	// moves to learned.1 and learned.2 is no longer declared), and red lines.
	changed := twoOfEachBundle()
	changed.Product.Forms = changed.Product.Forms[1:]
	changed.Product.Learned = changed.Product.Learned[1:]
	changed.Product.TasteProfile.RedLines = nil
	report, err := Apply(home, store, changed, "harbor.yaml", tProduct.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	wantRetracted := []string{
		"form.ios-foldable.delivery.acceptance",
		"form.ios-foldable.delivery.goal",
		"form.ios-foldable.delivery.parameters",
		"form.ios-foldable.delivery.status",
		"learned.2",
		"taste.red_lines",
	}
	if !reflect.DeepEqual(report.Retracted, wantRetracted) {
		t.Errorf("report.Retracted = %v, want %v", report.Retracted, wantRetracted)
	}

	after := viewObjects(t, store, "product:harbor")
	for _, predicate := range wantRetracted {
		if got, ok := after[predicate]; ok {
			t.Errorf("%s still current after it left the declaration: %q", predicate, got)
		}
	}
	if got := after["form.ios-phone.release.goal"]; got != "ships the one-time Pro unlock" {
		t.Errorf("kept form goal = %q, want it untouched", got)
	}
	if got := after["taste.standards"]; got != `["calm utility"]` {
		t.Errorf("kept taste.standards = %q, want it untouched", got)
	}
	if got := after["essence.audience"]; got != "Anglers who plan trips around tides" {
		t.Errorf("essence.audience = %q, want it untouched", got)
	}

	record, err := LoadEssence(store, "product:harbor")
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, record.Forms, changed.Product.Forms) {
		t.Errorf("forms after retraction:\n got %+v\nwant %+v", record.Forms, changed.Product.Forms)
	}
	if !jsonEqual(t, record.Learned, changed.Product.Learned) {
		t.Errorf("learned after retraction:\n got %+v\nwant %+v", record.Learned, changed.Product.Learned)
	}
	if len(record.Taste.RedLines) != 0 || len(record.Taste.Standards) != 1 {
		t.Errorf("taste after retraction = %+v, want standards only", record.Taste)
	}

	// Evidence, not deletion: the old value is still in the ledger, superseded
	// by a retired version that names where the retraction came from.
	timeline, err := store.Timeline("product:harbor")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]mem.Claim{}
	recorded := map[string]bool{}
	for _, c := range timeline {
		byID[c.ID] = c
		recorded[c.Predicate+"="+c.Object] = true
	}
	for _, want := range []string{
		"form.ios-foldable.delivery.goal=runs on the new size class",
		"form.ios-phone.release.goal=ships the one-time Pro unlock",
	} {
		if !recorded[want] {
			t.Errorf("ledger history lost %s; retraction must supersede, never delete", want)
		}
	}
	var retired *mem.Claim
	for i, c := range timeline {
		if c.Predicate == "form.ios-foldable.delivery.goal" && c.Visibility == "retired" {
			retired = &timeline[i]
		}
	}
	if retired == nil {
		t.Fatal("no retired version of form.ios-foldable.delivery.goal in the timeline")
	}
	if prior := byID[retired.Supersedes]; prior.Object != "runs on the new size class" {
		t.Errorf("retired version supersedes %q (%q), want the recorded form goal", retired.Supersedes, prior.Object)
	}
	if !strings.Contains(retired.Object, "harbor.yaml") {
		t.Errorf("retired object %q does not name the declaration it was retracted by", retired.Object)
	}
	positions, err := store.Positions()
	if err != nil {
		t.Fatal(err)
	}
	var status string
	for _, p := range positions {
		if p.Entity == "product:harbor" && p.Predicate == "form.ios-foldable.delivery.goal" {
			status = p.Status
		}
	}
	if status != "retired" {
		t.Errorf("position status for the retracted form goal = %q, want retired", status)
	}
}

// TestRestatedDeclarationRetractsNothing: re-importing the same declaration is
// a no-op for retraction, so a scheduled re-import cannot churn the ledger.
func TestRestatedDeclarationRetractsNothing(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, twoOfEachBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	before := viewObjects(t, store, "product:harbor")
	report, err := Apply(home, store, twoOfEachBundle(), "harbor.yaml", tProduct.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Retracted) != 0 {
		t.Errorf("restated declaration retracted %v", report.Retracted)
	}
	if after := viewObjects(t, store, "product:harbor"); !reflect.DeepEqual(after, before) {
		t.Errorf("restated declaration changed the current view:\n got %v\nwant %v", after, before)
	}
}

// TestRetractedFormIsCurrentAgainWhenRedeclared: retraction is a version, not
// a ban; declaring the form again supersedes the retired version.
func TestRetractedFormIsCurrentAgainWhenRedeclared(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, essenceBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	dropped := essenceBundle()
	dropped.Product.Forms = nil
	if _, err := Apply(home, store, dropped, "harbor.yaml", tProduct.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := viewObjects(t, store, "product:harbor")["form.ios-foldable.delivery.goal"]; got != "" {
		t.Fatalf("setup: dropped form goal still current: %q", got)
	}
	if _, err := Apply(home, store, essenceBundle(), "harbor.yaml", tProduct.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	record, err := LoadEssence(store, "product:harbor")
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, record.Forms, essenceBundle().Product.Forms) {
		t.Errorf("redeclared forms = %+v, want the declared form current again", record.Forms)
	}
}

// TestContractImportNeverRetractsEssenceFacts: a project contract declares no
// forms, learned facts, or taste, and it is not their authority, so its import
// must leave every one of them current.
func TestContractImportNeverRetractsEssenceFacts(t *testing.T) {
	home, store := openEssenceStore(t)
	if _, err := Apply(home, store, twoOfEachBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	before := viewObjects(t, store, "product:harbor")
	contract, err := LoadProjectContractBundle(writeContract(t, "project.yaml", curatedContract), "")
	if err != nil {
		t.Fatal(err)
	}
	report, err := Apply(home, store, contract, "project.yaml", tProduct.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Retracted) != 0 {
		t.Errorf("contract import retracted %v", report.Retracted)
	}
	after := viewObjects(t, store, "product:harbor")
	for predicate, object := range before {
		if !strings.HasPrefix(predicate, "form.") && !strings.HasPrefix(predicate, "learned.") && !strings.HasPrefix(predicate, "taste.") {
			continue
		}
		if after[predicate] != object {
			t.Errorf("contract import changed %s: %q -> %q", predicate, object, after[predicate])
		}
	}
}
