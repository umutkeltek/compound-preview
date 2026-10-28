package mem

import (
	"path/filepath"
	"testing"
	"time"
)

func TestExpandQueryFailOpenAbsentRegistry(t *testing.T) {
	reg := RecallAliasRegistry{SchemaVersion: RecallAliasSchemaVersion}
	got := ExpandQuery("memory not updating", reg)
	if got.Active || got.Text != "memory not updating" || len(got.Added) != 0 {
		t.Fatalf("empty registry must be identity: %+v", got)
	}
}

func TestExpandQueryPlantedGlossaryPair(t *testing.T) {
	reg := RecallAliasRegistry{
		SchemaVersion: RecallAliasSchemaVersion,
		Entries: []RecallAliasEntry{
			{From: "memory not updating", To: []string{"extract_facts", "starvation"}, Kind: "seed"},
		},
	}
	got := ExpandQuery("why is memory not updating today", reg)
	if !got.Active {
		t.Fatalf("expected expansion active: %+v", got)
	}
	joined := map[string]bool{}
	for _, a := range got.Added {
		joined[a] = true
	}
	if !joined["extract_facts"] || !joined["starvation"] {
		t.Fatalf("expected planted terms, got %+v", got.Added)
	}
	if got.Original != "why is memory not updating today" {
		t.Fatalf("original mutated: %q", got.Original)
	}
}

func TestQueryBreadthExpansionDisabledByteIdentical(t *testing.T) {
	s := newStore(t)
	ids, err := s.Capture([]Candidate{cand("distiller", "failure", "extract_facts starvation causes freeze", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promoteForTest(t, s, ids[0].ID, "", t0); err != nil {
		t.Fatal(err)
	}
	eng := &LexicalEngine{}
	a, err := s.QueryBreadth("extract_facts starvation", 10, eng)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.QueryBreadth("extract_facts starvation", 10, eng)
	if err != nil {
		t.Fatal(err)
	}
	if !sameClaimIDs(a, b) {
		t.Fatalf("absent registry must be deterministic: %v vs %v", claimIDs(a), claimIDs(b))
	}
	if len(a) == 0 {
		t.Fatal("expected at least one hit")
	}
}

func TestQueryBreadthExpansionFindsPlantedPairWithoutDroppingPrimary(t *testing.T) {
	s := newStore(t)
	sys, err := s.Capture([]Candidate{cand("distiller", "failure", "extract_facts starvation under load", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promoteForTest(t, s, sys[0].ID, "", t0); err != nil {
		t.Fatal(err)
	}
	pri, err := s.Capture([]Candidate{cand("project:x", "note", "memory layer documentation status", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promoteForTest(t, s, pri[0].ID, "", t0); err != nil {
		t.Fatal(err)
	}

	eng := &LexicalEngine{}
	before, err := s.QueryBreadth("memory not updating", 10, eng)
	if err != nil {
		t.Fatal(err)
	}
	// System claim should not match casual phrasing without expansion (resolve
	// via content: after promote the claim id differs from the candidate id).
	if containsObject(before, "extract_facts starvation under load") {
		t.Fatalf("precondition: casual query already hits system claim: %v", objs(before))
	}

	if err := SaveRecallAliases(s.home, RecallAliasRegistry{
		SchemaVersion: RecallAliasSchemaVersion,
		GeneratedAt:   t0.Format(time.RFC3339),
		Source:        "test",
		Entries: []RecallAliasEntry{
			{From: "memory not updating", To: []string{"extract_facts", "starvation"}, Kind: "seed"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	after, err := s.QueryBreadth("memory not updating", 10, eng)
	if err != nil {
		t.Fatal(err)
	}
	if !containsObject(after, "extract_facts starvation under load") {
		t.Fatalf("expansion should surface system claim, got %v", objs(after))
	}

	// Primary-reachable claim still found when expansion is active.
	primary, err := s.QueryBreadth("memory layer documentation", 10, eng)
	if err != nil {
		t.Fatal(err)
	}
	if !containsObject(primary, "memory layer documentation status") {
		t.Fatalf("primary hit lost under expansion path: %v", objs(primary))
	}
}

func TestRebuildRecallAliasesFromEntities(t *testing.T) {
	s := newStore(t)
	if _, err := s.RegisterEntity(Entity{
		Slug: "project:compound", Kind: "project", Aliases: []string{"Company Brain"},
	}, t0); err != nil {
		t.Fatal(err)
	}
	if err := SaveRecallAliases(s.home, RecallAliasRegistry{
		SchemaVersion: RecallAliasSchemaVersion,
		Entries: []RecallAliasEntry{
			{From: "memory not updating", To: []string{"extract_facts", "starvation"}, Kind: "seed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := s.RebuildRecallAliases(t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRecallAliases(s.home, reg); err != nil {
		t.Fatal(err)
	}
	hasSeed := false
	hasEntity := false
	for _, e := range reg.Entries {
		if e.From == "memory not updating" {
			hasSeed = true
		}
		if e.From == "Company Brain" {
			hasEntity = true
		}
	}
	if !hasSeed {
		t.Fatalf("seed entry dropped: %+v", reg.Entries)
	}
	if !hasEntity {
		t.Fatalf("entity alias missing: %+v", reg.Entries)
	}
	got := ExpandQuery("look at the Company Brain", reg)
	if !got.Active {
		t.Fatalf("expected entity expansion: %+v", got)
	}
	// Path check
	if RecallAliasesPath(s.home) != filepath.Join(s.home, "registries", "recall-aliases.json") {
		t.Fatal("unexpected path")
	}
}

func sameClaimIDs(a, b []Claim) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func claimIDs(cs []Claim) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func containsObject(cs []Claim, object string) bool {
	for _, c := range cs {
		if c.Object == object {
			return true
		}
	}
	return false
}
