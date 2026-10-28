package product

import (
	"strings"
	"testing"

	"compound/internal/mem"
)

const curatedContract = `schema_version: 1
project:
  slug: harbor
  name: Harbor
  class: product
  status: live
  owner: operator
  notes: Backend pipeline that publishes a static artifact.
source:
  authority:
    host: workstation
    path: /workspace/harbor
    repo: https://example.invalid/harbor.git
runtime:
  authority: build-host
  urls:
    - https://harbor.example.invalid
  deployment:
    method: launchd_in_place_repo
    command: make deploy
data:
  classification: appdata
release_identity:
  bundle_id: com.example.harbor
`

// curatedRegistryBundle is the market-facing record a person curates: the
// positioning and privacy posture a product is sold on, and the store identity
// other readers resolve it by.
func curatedRegistryBundle() Bundle {
	return Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:harbor",
			Name:           "Harbor",
			OwnerEntity:    "project:harbor",
			Aliases:        []string{"Harbor Tides"},
			Surfaces:       []Surface{{ID: "app-store", Kind: "app_store", Ref: "appstore:com.example.harbor.legacy"}, {ID: "site", Kind: "web", Ref: "https://harbor.example.invalid"}},
			Channels:       []string{"app_store", "landing_page"},
			Positioning:    "Tide and moon planner for anglers; one-time Pro unlock.",
			PrivacyPosture: "local_first",
		},
		EvidenceRefs: []string{"fixture:curated"},
	}
}

func currentObject(t *testing.T, store *mem.Store, entity, predicate string) string {
	t.Helper()
	claim, ok, err := store.CurrentClaim(entity, predicate)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return ""
	}
	return claim.Object
}

// TestContractImportNeverOverridesCuratedProductFacts is the depth test for the
// contract importer's authority. A project contract is the authority for where a
// product runs and how it is released, not for how it is positioned: its notes
// are a repo summary and data.classification is a storage class. Importing it
// onto a curated product must add the deployment and the new identifiers while
// leaving every curated fact — positioning, privacy posture, and each surface,
// channel, alias, and identifier already recorded — exactly as it was.
func TestContractImportNeverOverridesCuratedProductFacts(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, curatedRegistryBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	contract, err := LoadProjectContractBundle(writeContract(t, "project.yaml", curatedContract), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, contract, "project.yaml", tProduct); err != nil {
		t.Fatal(err)
	}

	if got := currentObject(t, store, "product:harbor", "positioning"); got != "Tide and moon planner for anglers; one-time Pro unlock." {
		t.Errorf("positioning = %q, want the curated positioning kept", got)
	}
	if got := currentObject(t, store, "product:harbor", "privacy_posture"); got != "local_first" {
		t.Errorf("privacy_posture = %q, want the curated posture kept", got)
	}
	surfaces := currentObject(t, store, "product:harbor", "surfaces")
	for _, ref := range []string{"appstore:com.example.harbor.legacy", "https://harbor.example.invalid", "bundle:com.example.harbor", "https://example.invalid/harbor.git"} {
		if !strings.Contains(surfaces, ref) {
			t.Errorf("surfaces %q lost or never gained %q", surfaces, ref)
		}
	}
	channels := currentObject(t, store, "product:harbor", "channels")
	for _, channel := range []string{"app_store", "landing_page", "web", "repo"} {
		if !strings.Contains(channels, channel) {
			t.Errorf("channels %q missing %q", channels, channel)
		}
	}
	if got := currentObject(t, store, "product:harbor", "deploys_to"); got != "place:build-host" {
		t.Errorf("deploys_to = %q, want the contract's place added", got)
	}
	entities, err := store.Entities()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entities {
		if e.Slug == "product:harbor" && e.Parent != "project:harbor" {
			t.Errorf("entity parent = %q, want the recorded owner project:harbor kept", e.Parent)
		}
	}
	for _, token := range []string{"appstore:com.example.harbor.legacy", "bundle:com.example.harbor", "Harbor Tides"} {
		res, err := store.ResolveEntity(token)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "resolved" || res.Slug != "product:harbor" {
			t.Errorf("resolve %q = %+v, want product:harbor — re-registration must not drop an identifier or alias", token, res)
		}
	}
}

// TestContractImportLeavesAContestedFactUnderReview: a fact with an open review
// is hidden from the current claims, so an importer that only reads those sees
// an empty slot and supersedes it with the contract's narrower value — dropping
// recorded refs and closing the review without anyone deciding it. The contract
// must leave a contested slot alone and still add what it owns.
func TestContractImportLeavesAContestedFactUnderReview(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, curatedRegistryBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	surfaces, ok, err := store.CurrentClaim("product:harbor", "surfaces")
	if err != nil || !ok {
		t.Fatalf("curated surfaces claim: ok=%v err=%v", ok, err)
	}
	if _, err := store.ReduceConcurrence(mem.ConcurrenceProbe{
		ID:        "probe:harbor-surfaces",
		ClaimID:   surfaces.ID,
		Content:   surfaces.Object,
		Lanes:     []string{"lane-a"},
		Threshold: 1,
	}, []mem.ConcurrenceAnswer{{Lane: "lane-a", Verdict: mem.ConcurrenceVerdictDisagree, Summary: "store listing moved"}}, tProduct); err != nil {
		t.Fatal(err)
	}

	contract, err := LoadProjectContractBundle(writeContract(t, "project.yaml", curatedContract), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, contract, "project.yaml", tProduct); err != nil {
		t.Fatal(err)
	}

	claims, err := store.LedgerClaims()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		if c.Supersedes == surfaces.ID {
			t.Fatalf("contract import superseded the contested surfaces claim with %q — it must stay under review", c.Object)
		}
	}
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pos := range view.Positions() {
		if pos.Entity == "product:harbor" && pos.Predicate == "surfaces" {
			found = true
			if pos.Status != "contested" || pos.ActiveClaimID != surfaces.ID {
				t.Errorf("surfaces position = %s/%s, want contested on %s", pos.Status, pos.ActiveClaimID, surfaces.ID)
			}
		}
	}
	if !found {
		t.Error("surfaces position missing after import")
	}
	if got := currentObject(t, store, "product:harbor", "deploys_to"); got != "place:build-host" {
		t.Errorf("deploys_to = %q, want the contract's place still added", got)
	}
	res, err := store.ResolveEntity("bundle:com.example.harbor")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "resolved" || res.Slug != "product:harbor" {
		t.Errorf("resolve bundle id = %+v, want the contract's identifier still added", res)
	}
}

// TestContractImportSeedsMarketFactsForANewProduct keeps the contract useful on
// its own: a product nothing else describes still gets a positioning and
// privacy posture from its contract.
func TestContractImportSeedsMarketFactsForANewProduct(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := LoadProjectContractBundle(writeContract(t, "project.yaml", curatedContract), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, contract, "project.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	if got := currentObject(t, store, "product:harbor", "positioning"); got != "Backend pipeline that publishes a static artifact." {
		t.Errorf("positioning = %q, want it seeded from the contract", got)
	}
	if got := currentObject(t, store, "product:harbor", "privacy_posture"); got != "appdata" {
		t.Errorf("privacy_posture = %q, want it seeded from the contract", got)
	}
}

// TestCuratedImportStillReplacesItsOwnFacts: the additive rule belongs to the
// contract source only. A curated registry import remains the owner of market
// facts and replaces them, or a correction could never land.
func TestCuratedImportStillReplacesItsOwnFacts(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, store, curatedRegistryBundle(), "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	corrected := curatedRegistryBundle()
	corrected.Product.Positioning = "Tide planner for anglers."
	corrected.Product.Surfaces = corrected.Product.Surfaces[1:]
	if _, err := Apply(home, store, corrected, "harbor.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	if got := currentObject(t, store, "product:harbor", "positioning"); got != "Tide planner for anglers." {
		t.Errorf("positioning = %q, want the curated correction", got)
	}
	if got := currentObject(t, store, "product:harbor", "surfaces"); strings.Contains(got, "appstore:com.example.harbor.legacy") {
		t.Errorf("surfaces = %q, a curated import must be able to remove a surface", got)
	}
}
