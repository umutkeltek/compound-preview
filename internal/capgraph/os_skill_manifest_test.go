package capgraph

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSlugSkillFaithfulIdentity(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"write-a-skill", "write-a-skill"},
		{"deploy-to-vercel", "deploy-to-vercel"},
		{"foundation-models-on-device", "foundation-models-on-device"},
		{"to-issues", "to-issues"},
		{"diff-cleanup-and-parking", "diff-cleanup-and-parking"},
		{"finishing-a-development-branch", "finishing-a-development-branch"},
		// Repeated token must not be deduped away for identity.
		{"test-the-test", "test-the-test"},
		{"", "unknown"},
		{"   ", "unknown"},
	}
	for _, tc := range cases {
		if got := slugSkill(tc.name); got != tc.want {
			t.Errorf("slugSkill(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLegacySlugSkillFrozenMangle(t *testing.T) {
	// Frozen migration helper: stop-words + dedup (the historical bug).
	if got := legacySlugSkill("write-a-skill"); got != "write-skill" {
		t.Fatalf("legacySlugSkill(write-a-skill) = %q, want write-skill", got)
	}
	if got := legacySlugSkill("test-the-test"); got != "test" {
		t.Fatalf("legacySlugSkill(test-the-test) = %q, want test (dedup)", got)
	}
}

func TestSkillTokenListUnchangedByFaithfulSlug(t *testing.T) {
	// Trigger tokens must keep stop-word filtering — byte-identical to the
	// historical collectSkillTokens(true, ...) path.
	entry := osSkillEntry{
		Name:    "write-a-skill",
		Kind:    "skill",
		Trigger: "Create new skills for agents",
		Home:    "agents",
	}
	// Name "write-a-skill" → tokens write, skill (a stopped). Kind skill, home agents, trigger words.
	got := osSkillTriggers(entry)
	want := skillTokenList(entry.Name, entry.Kind, entry.Home, entry.Trigger)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("osSkillTriggers != skillTokenList path: got %v want %v", got, want)
	}
	// Explicit: stop-word "a" must not appear in trigger tokens.
	for _, tok := range got {
		if tok == "a" || tok == "to" || tok == "for" {
			t.Fatalf("trigger tokens still include stop-word %q: %v", tok, got)
		}
	}
	// And skillTokenList must not include "a" from the name.
	if stringSliceContains(got, "a") {
		t.Fatalf("stop-word leaked into triggers: %v", got)
	}
}

func TestLoadOSSkillManifestMigratesStopWordIdentity(t *testing.T) {
	skillRoot := t.TempDir()
	writeSkillSource(t, skillRoot, "write-a-skill", `---
name: write-a-skill
description: "Synthetic skill identity fixture"
---
# Write a skill
`)
	path := filepath.Join(t.TempDir(), "skill-manifest.json")
	writeFile(t, path, strings.ReplaceAll(`{
  "_generated": "test",
  "counts": {"skills": 1},
  "entries": [
    {
      "name": "write-a-skill",
      "kind": "skill",
      "trigger": "Create new skills, modify existing skills.",
      "path": "/private/skills/write-a-skill",
      "home": "agents"
    }
  ]
}`, "/private/skills/write-a-skill", filepath.ToSlash(filepath.Join(skillRoot, "write-a-skill"))))
	manifest, err := LoadOSSkillManifest(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]SkillManifestEntry{}
	for _, skill := range manifest.Skills {
		byID[skill.ID] = skill
	}
	faithful, ok := byID["skill:write-a-skill"]
	if !ok {
		t.Fatalf("missing faithful id skill:write-a-skill; ids=%v", keysOf(byID))
	}
	if faithful.Status != "active" {
		t.Fatalf("faithful status = %q", faithful.Status)
	}
	if len(faithful.ReplacementFor) != 1 || faithful.ReplacementFor[0] != "skill:write-skill" {
		t.Fatalf("ReplacementFor = %v, want [skill:write-skill]", faithful.ReplacementFor)
	}
	legacy, ok := byID["skill:write-skill"]
	if !ok {
		t.Fatalf("missing retired stub skill:write-skill; ids=%v", keysOf(byID))
	}
	if legacy.Status != "retired" {
		t.Fatalf("legacy status = %q, want retired", legacy.Status)
	}
	if legacy.Name != "write-a-skill (legacy id)" {
		t.Fatalf("legacy name = %q", legacy.Name)
	}

	home, store := newStore(t)
	// Seed the pre-migration mangled node so the stub must supersede it.
	if err := store.Add(Node{
		ID: "skill:write-skill", Kind: "skill", Name: "write-a-skill",
		Triggers: []string{"write", "skill"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if !hasSurfaced(mustSurface(t, store, "write a skill"), "skill:write-skill") {
		t.Fatal("precondition: legacy id should surface before migration apply")
	}
	report, err := ApplySkillManifest(home, manifest, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" {
		t.Fatalf("report = %+v", report)
	}
	// retired_by edge: old -> new
	edges, err := store.Edges()
	if err != nil {
		t.Fatal(err)
	}
	foundRetiredBy := false
	for _, e := range edges {
		if e.Kind == EdRetiredBy && e.From == "skill:write-skill" && e.To == "skill:write-a-skill" {
			foundRetiredBy = true
			break
		}
	}
	if !foundRetiredBy {
		t.Fatalf("missing retired_by edge skill:write-skill -> skill:write-a-skill; edges=%+v", edges)
	}
	hits := mustSurface(t, store, "write a skill")
	if !hasSurfaced(hits, "skill:write-a-skill") {
		t.Fatalf("faithful id should surface; hits=%v", surfaceIDs(hits))
	}
	if hasSurfaced(hits, "skill:write-skill") {
		t.Fatalf("legacy id must not surface after migration; hits=%v", surfaceIDs(hits))
	}
}

func TestLoadOSSkillManifestMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill-manifest.json")
	writeFile(t, path, `{
  "_generated": "test",
  "counts": {"skills": 1},
  "entries": [
    {
      "name": "write-a-skill",
      "kind": "skill",
      "trigger": "Create new skills.",
      "path": "/private/skills/write-a-skill",
      "home": "agents"
    }
  ]
}`)
	home, store := newStore(t)
	m1, err := LoadOSSkillManifest(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySkillManifest(home, m1, t0); err != nil {
		t.Fatal(err)
	}
	m2, err := LoadOSSkillManifest(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySkillManifest(home, m2, t0); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	// Exactly two distinct ids: faithful + retired stub (last-wins load).
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if len(byID) != 2 {
		t.Fatalf("expected 2 nodes after double apply, got %d: %v", len(byID), keysOfNode(byID))
	}
	if !byID["skill:write-skill"].Retired {
		t.Fatal("legacy stub must stay retired after re-import")
	}
	if byID["skill:write-a-skill"].Retired {
		t.Fatal("faithful node must not be retired")
	}
}

func keysOf(m map[string]SkillManifestEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfNode(m map[string]Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func surfaceIDs(hits []Surfaced) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Node.ID)
	}
	return out
}
