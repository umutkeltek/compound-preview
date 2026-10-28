package capgraph_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/sourceobs"
)

var brainRecipeTestNow = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

func TestLoadBrainRecipeTreeUsesCuratedRecipeHeaders(t *testing.T) {
	dir := brainRecipeFixtureDir(t)
	manifest, err := capgraph.LoadBrainRecipeTree(dir, brainRecipeTestNow)
	if err != nil {
		t.Fatal(err)
	}
	again, err := capgraph.LoadBrainRecipeTree(dir, brainRecipeTestNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SourceRef.Revision != again.SourceRef.Revision {
		t.Fatalf("SourceRevision not deterministic: %q != %q", manifest.SourceRef.Revision, again.SourceRef.Revision)
	}
	if manifest.SourceRef.Kind != "brain_recipe_tree" || manifest.SourceRef.SourceClass != "solo-builder-recipes" {
		t.Fatalf("unexpected source ref: %+v", manifest.SourceRef)
	}
	if len(manifest.Skills) != 3 {
		t.Fatalf("skills = %d, want 3; ids=%v", len(manifest.Skills), brainRecipeIDs(manifest))
	}

	triage := mustBrainRecipeEntry(t, manifest, "procedure:triage")
	if triage.Kind != "procedure" || triage.Name != "Recipe: Triage" {
		t.Fatalf("triage entry = %+v", triage)
	}
	if !containsString(triage.Triggers, "incident") || !containsString(triage.Triggers, "regression") || !containsString(triage.Triggers, "broken") {
		t.Fatalf("triage trigger tokens = %v, want curated TRIGGER tokens", triage.Triggers)
	}
	for _, bodyOnly := range []string{"bodyonlytriage", "soup"} {
		if containsString(triage.Triggers, bodyOnly) {
			t.Fatalf("body token %q leaked into triggers: %v", bodyOnly, triage.Triggers)
		}
	}
	wantRead := filepath.Join(dir, "triage.md")
	if !reflect.DeepEqual(triage.Read, []string{wantRead}) {
		t.Fatalf("triage read = %v, want [%s]", triage.Read, wantRead)
	}
	if !reflect.DeepEqual(triage.Boundary, []string{"feature build without incident signal"}) {
		t.Fatalf("triage boundary = %v", triage.Boundary)
	}
	if findBrainRecipeEntry(manifest, "procedure:scratch") != nil || findBrainRecipeEntry(manifest, "procedure:hidden") != nil {
		t.Fatalf("backup/dotfile recipes should be skipped; ids=%v", brainRecipeIDs(manifest))
	}
}

func TestBrainRecipeTreeImportIsGraphIdempotentAndRecordsAuthority(t *testing.T) {
	home := t.TempDir()
	manifest, err := capgraph.LoadBrainRecipeTree(brainRecipeFixtureDir(t), brainRecipeTestNow)
	if err != nil {
		t.Fatal(err)
	}
	report := importBrainRecipeManifest(t, home, manifest, brainRecipeTestNow)
	if len(report.NodeIDs) != len(manifest.Skills) {
		t.Fatalf("node ids = %v, want %d ids", report.NodeIDs, len(manifest.Skills))
	}
	nodeLog := filepath.Join(home, "registries", "capability-nodes.jsonl")
	firstRows := readNonEmptyBrainRecipeLines(t, nodeLog)
	if len(firstRows) != len(manifest.Skills) {
		t.Fatalf("node rows after first import = %d, want %d", len(firstRows), len(manifest.Skills))
	}
	records, err := capgraph.LoadSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !containsString(records[0].NodeIDs, "procedure:triage") {
		t.Fatalf("source authority records = %+v", records)
	}

	importBrainRecipeManifest(t, home, manifest, brainRecipeTestNow)
	secondRows := readNonEmptyBrainRecipeLines(t, nodeLog)
	if !reflect.DeepEqual(firstRows, secondRows) {
		t.Fatalf("second import should not append graph rows:\nfirst=%v\nsecond=%v", firstRows, secondRows)
	}
}

func TestBrainRecipeTreeDryRunReportsDriftAndDoesNotWrite(t *testing.T) {
	recipes := copyBrainRecipeFixtureTree(t)
	home := t.TempDir()
	manifest, err := capgraph.LoadBrainRecipeTree(recipes, brainRecipeTestNow)
	if err != nil {
		t.Fatal(err)
	}
	importBrainRecipeManifest(t, home, manifest, brainRecipeTestNow)

	before := snapshotBrainRecipeDir(t, home)
	cleanOut := runCompoundForBrainRecipe(t, home, "capability", "import-skills", "--brain-recipe-dir", recipes, "--dry-run")
	after := snapshotBrainRecipeDir(t, home)
	if !strings.Contains(cleanOut, "drift: false") {
		t.Fatalf("clean dry-run output =\n%s", cleanOut)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("clean dry-run mutated COMPOUND_HOME:\nbefore=%v\nafter=%v", snapshotKeys(before), snapshotKeys(after))
	}

	triagePath := filepath.Join(recipes, "triage.md")
	f, err := os.OpenFile(triagePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\nTampered line changes the source revision.\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	before = snapshotBrainRecipeDir(t, home)
	driftOut := runCompoundForBrainRecipe(t, home, "capability", "import-skills", "--brain-recipe-dir", recipes, "--dry-run")
	after = snapshotBrainRecipeDir(t, home)
	if !strings.Contains(driftOut, "drift: true") {
		t.Fatalf("tampered dry-run output =\n%s", driftOut)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("drift dry-run mutated COMPOUND_HOME:\nbefore=%v\nafter=%v", snapshotKeys(before), snapshotKeys(after))
	}
}

func TestBrainRecipeTreeNoTriggerFallsBackToNameTokensOnly(t *testing.T) {
	manifest, err := capgraph.LoadBrainRecipeTree(brainRecipeFixtureDir(t), brainRecipeTestNow)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := mustBrainRecipeEntry(t, manifest, "procedure:nested")
	want := []string{"recipe", "nested", "dispatcher"}
	if !reflect.DeepEqual(dispatcher.Triggers, want) {
		t.Fatalf("no-trigger recipe triggers = %v, want %v", dispatcher.Triggers, want)
	}
	if containsString(dispatcher.Triggers, "dispatchbodyonly") {
		t.Fatalf("body token leaked into no-trigger recipe triggers: %v", dispatcher.Triggers)
	}
}

func TestBrainRecipeTreeNestedIDsArePathBased(t *testing.T) {
	manifest, err := capgraph.LoadBrainRecipeTree(brainRecipeFixtureDir(t), brainRecipeTestNow)
	if err != nil {
		t.Fatal(err)
	}
	parent := findBrainRecipeEntry(manifest, "procedure:nested")
	child := findBrainRecipeEntry(manifest, "procedure:nested-deep-dive")
	if parent == nil || child == nil {
		t.Fatalf("missing nested parent/child ids; got %v", brainRecipeIDs(manifest))
	}
	if parent.ID == child.ID {
		t.Fatalf("nested child id collapsed into parent id: parent=%s child=%s", parent.ID, child.ID)
	}
}

func TestPlainSkillManifestApplyDoesNotMutateLoadedFormat(t *testing.T) {
	manifest, err := capgraph.LoadSkillManifest(filepath.Join("..", "..", "fixtures", "skills", "skill-manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.ApplySkillManifest(t.TempDir(), manifest, brainRecipeTestNow); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ApplySkillManifest mutated plain loaded manifest:\nbefore=%s\nafter=%s", before, after)
	}
	for _, skill := range manifest.Skills {
		if len(skill.Read) != 0 || len(skill.Boundary) != 0 {
			t.Fatalf("plain fixture unexpectedly populated read/boundary: %+v", skill)
		}
	}
}

func importBrainRecipeManifest(t *testing.T, home string, manifest capgraph.SkillManifest, now time.Time) capgraph.SkillManifestReport {
	t.Helper()
	report, err := capgraph.ApplySkillManifest(home, manifest, now)
	if err != nil {
		t.Fatal(err)
	}
	sourceReport, err := sourceobs.Apply(home, sourceobs.FromSkillManifest(manifest, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.RecordSourceAuthority(home, capgraph.SourceAuthorityInput{
		SourcePath:          manifest.SourceRef.Path,
		SourceKind:          manifest.SourceRef.Kind,
		SourceOwner:         manifest.SourceRef.Owner,
		SourceClass:         manifest.SourceRef.SourceClass,
		SourceRevision:      manifest.SourceRef.Revision,
		SourceObservedAt:    manifest.ObservedAt,
		ImportedVia:         "compound capability import-skills",
		NodeIDs:             report.NodeIDs,
		EdgeIDs:             report.EdgeIDs,
		SourceObservationID: sourceReport.ObservationID,
		NoPromoteIDs:        sourceReport.NoPromoteIDs,
		EvidenceRefs:        manifest.EvidenceRefs,
	}, now); err != nil {
		t.Fatal(err)
	}
	return report
}

func brainRecipeFixtureDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "fixtures", "skills", "brain-recipe-tree"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func copyBrainRecipeFixtureTree(t *testing.T) string {
	t.Helper()
	src := brainRecipeFixtureDir(t)
	dst := filepath.Join(t.TempDir(), "recipes")
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func runCompoundForBrainRecipe(t *testing.T, home string, args ...string) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := capgraphCompoundTestBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"COMPOUND_HOME="+home,
		"GOCACHE="+filepath.Join(t.TempDir(), "gocache"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", bin, args, err, out)
	}
	return string(out)
}

func snapshotBrainRecipeDir(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func snapshotKeys(snapshot map[string][]byte) []string {
	out := make([]string, 0, len(snapshot))
	for key := range snapshot {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func readNonEmptyBrainRecipeLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func mustBrainRecipeEntry(t *testing.T, manifest capgraph.SkillManifest, id string) capgraph.SkillManifestEntry {
	t.Helper()
	entry := findBrainRecipeEntry(manifest, id)
	if entry == nil {
		t.Fatalf("missing entry %s; got %v", id, brainRecipeIDs(manifest))
	}
	return *entry
}

func findBrainRecipeEntry(manifest capgraph.SkillManifest, id string) *capgraph.SkillManifestEntry {
	for i := range manifest.Skills {
		if manifest.Skills[i].ID == id {
			return &manifest.Skills[i]
		}
	}
	return nil
}

func brainRecipeIDs(manifest capgraph.SkillManifest) []string {
	out := make([]string, 0, len(manifest.Skills))
	for _, skill := range manifest.Skills {
		out = append(out, skill.ID)
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
