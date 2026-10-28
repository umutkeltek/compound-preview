package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F9 (TestImportSkillsFromDirectoryStampsEverySkill) is the integration
// falsifier for slice 5's door: a real temp root with 2 healthy skill dirs
// PLUS one dangling symlink. The LoadSkillDirManifest loader walks the
// root with skill_source_doctor's logic and returns all three entries --
// the dangling one with SkillPath set -- so the slice-2 import path can
// stamp Source.Reachable=false on it. Two import runs prove the slice-1
// upsert invariant coheres (zero duplicate ids, byte-equivalent loadNodes).
//
// On base: there is no LoadSkillDirManifest -- the existing LoadSkillManifest
// only parses YAML and would either fail (file vs dir) or treat the path as a
// single-skill YAML, missing the dangling entry entirely. The cmd/compound
// switch's last case was hard-wired to LoadSkillManifest(p[0]) and had no
// IsDir branch.
//
// F9 RED on base: compile FAIL on LoadSkillDirManifest undefined + the
// cmd-side "load manifest for directory" expectation has no path to exercise.
// F9 GREEN on this branch: see assertions below.
func TestImportSkillsFromDirectoryStampsEverySkill(t *testing.T) {
	home, store := newStore(t)
	root := t.TempDir()

	// Two healthy skills + one dangling symlink, exactly the topology F9 pins.
	writeSkillSource(t, root, "alpha-skill", `---
name: alpha-skill
description: "First healthy skill"
---
# alpha-skill body
`)
	writeSkillSource(t, root, "beta-skill", `---
name: beta-skill
description: "Second healthy skill"
---
# beta-skill body
`)
	if err := os.Symlink("/nonexistent/skills-does-not-exist-gamma-"+filepath.Base(t.TempDir()),
		filepath.Join(root, "gamma-skill")); err != nil {
		t.Fatal(err)
	}

	// Loader roundtrip: returns 3 entries, the dangling one carrying
	// SkillPath+status=needs_review so the import path stamps reachable=false.
	manifest, err := LoadSkillDirManifest(root, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(manifest.Skills); got != 3 {
		t.Fatalf("manifest Skills count = %d, want 3 (dangling included)", got)
	}
	// Each entry carries a SkillPath (load-bearing for the slice-2 stamp path).
	for _, s := range manifest.Skills {
		if strings.TrimSpace(s.SkillPath) == "" {
			t.Fatalf("skill %s missing SkillPath; the slice-2 stamp would no-op", s.ID)
		}
	}
	// gamma-skill comes back with status=needs_review because the inspection
	// found Reason=dangling_symlink.
	for _, s := range manifest.Skills {
		if s.ID == "skill:gamma-skill" && s.Status != "needs_review" {
			t.Fatalf("gamma-skill status = %q, want needs_review (dangling)", s.Status)
		}
	}

	// Run 1 through ApplySkillManifest: stamp sources, one row per id.
	if _, err := ApplySkillManifest(home, manifest, t0); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Node{}
	for _, n := range loaded {
		byID[n.ID] = n
	}
	if len(byID) != 3 {
		t.Fatalf("after run 1 loaded %d distinct ids, want 3", len(byID))
	}
	for _, id := range []string{"skill:alpha-skill", "skill:beta-skill"} {
		n, ok := byID[id]
		if !ok {
			t.Fatalf("missing healthy node %s", id)
		}
		if n.Source == nil || !n.Source.Reachable {
			t.Fatalf("healthy node %s Source = %+v, want reachable", id, n.Source)
		}
	}
	if g := byID["skill:gamma-skill"]; g.Source == nil || g.Source.Reachable {
		t.Fatalf("dangling node Source = %+v, want reachable=false", g.Source)
	} else if g.Source.Reason != "dangling_symlink" {
		t.Fatalf("dangling node Source.Reason = %q, want dangling_symlink", g.Source.Reason)
	}

	// Run 2: same manifest, idempotent at the file level. Zero duplicate ids
	// is the slice-1 upsert carry -- F4 also pins it for the OS-load path,
	// F9 pins it for the dir-load path.
	if _, err := ApplySkillManifest(home, manifest, t0); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	byID = map[string]Node{}
	for _, n := range loaded {
		byID[n.ID] = n
	}
	if len(byID) != 3 {
		t.Fatalf("after run 2 loaded %d distinct ids, want 3 (upsert invariant)", len(byID))
	}
	logPath := filepath.Join(home, "registries", "capability-nodes.jsonl")
	rawCounts := map[string]int{}
	for _, raw := range readNonEmptyLines(t, logPath) {
		var n Node
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			t.Fatalf("raw row parse: %v", err)
		}
		rawCounts[n.ID]++
	}
	for id := range byID {
		if rawCounts[id] != 1 {
			t.Fatalf("after 2 runs raw row count for %s = %d, want 1", id, rawCounts[id])
		}
	}
}

// TestLoadSkillManifestForPathRoutesByKind pins the dispatch on the cmd/compound
// side: a directory goes to LoadSkillDirManifest, a regular file to
// LoadSkillManifest. The slice-5 budget wants the CLI change to be one LOC;
// this test guards that contract -- if routing regresses, the test fails.
func TestLoadSkillManifestForPathRoutesByKind(t *testing.T) {
	dir := t.TempDir()
	writeSkillSource(t, dir, "x-skill", `---
name: x-skill
description: "x"
---
# x
`)
	m1, err := LoadSkillManifestForPath(dir, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m1.Skills) != 1 || m1.Skills[0].ID != "skill:x-skill" {
		t.Fatalf("dir dispatch returned %+v, want one entry with id=skill:x-skill", m1.Skills)
	}
	// File (YAML) roundtrip -- write a minimal manifest.
	yamlPath := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(yamlPath, []byte(`schema_version: compound.skill_manifest.v0.1
id: sample-yaml-manifest
adapter_id: test.f9
source_system: test
observed_at: 2026-08-25T00:00:00Z
source_ref:
  path: /private/x
  kind: test
  owner: test
  source_class: test
  revision: sha256:x
evidence_refs: ["test:f9"]
skills:
  - id: skill:yaml-skill
    name: yaml-skill
    status: active
    triggers: ["x"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := LoadSkillManifestForPath(yamlPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Skills) != 1 || m2.Skills[0].ID != "skill:yaml-skill" {
		t.Fatalf("file dispatch returned %+v, want yaml-skill", m2.Skills)
	}
}

func TestSkillDirectoryRevisionTracksContentsAndSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	writeSkillSource(t, root, "alpha", `---
name: alpha
description: "first"
---
# alpha
`)
	first, err := LoadSkillDirManifest(root, t0)
	if err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(root, "alpha", "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: alpha\ndescription: \"changed\"\n---\n# alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := LoadSkillDirManifest(root, t0)
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceRef.Revision == second.SourceRef.Revision {
		t.Fatalf("content change left revision at %q", first.SourceRef.Revision)
	}

	link := filepath.Join(root, "linked")
	if err := os.Symlink(filepath.Join(root, "alpha"), link); err != nil {
		t.Fatal(err)
	}
	withLink, err := LoadSkillDirManifest(root, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), link); err != nil {
		t.Fatal(err)
	}
	retargeted, err := LoadSkillDirManifest(root, t0)
	if err != nil {
		t.Fatal(err)
	}
	if withLink.SourceRef.Revision == retargeted.SourceRef.Revision {
		t.Fatal("symlink target change did not move source revision")
	}
}
