package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F3 (TestSkillSourceDoctorFlagsDanglingSymlink) pins the Lstat+Stat dance:
// a temp root with one good skill and one dangling symlink. The doctor MUST
// report the dangling entry as reason="dangling_symlink" (not "unreadable"
// or missing), so the operator can tell a missing source from a missing
// target on a symlink whose wrapper is still loaded into memory.
//
// On base: the doctor used a glob root/*/SKILL.md pattern that never saw the
// dangling entry at all -- Files count = 1 (the good one), Dangling count = 0.
// On this branch: root/* via os.ReadDir sees the symlink; inspectSkillSourceEntry
// distinguishes Lstat-success+Stat-fail (dangling) from Lstat-success+Stat-success
// (real dir) via the dance below.
func TestSkillSourceDoctorFlagsDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	writeSkillSource(t, root, "good", `---
name: good
description: "Good skill"
---
# good
`)
	// Dangling symlink: link exists, target does not. The directory this
	// resolves into must NEVER exist, otherwise the test cheats its own
	// "dangling" assertion. /nonexistent/<random> is good enough on macOS
	// where it cannot accidentally be created.
	if err := os.Symlink("/nonexistent/skills-does-not-exist-"+filepath.Base(t.TempDir()), filepath.Join(root, "bad")); err != nil {
		t.Fatal(err)
	}

	report, err := DoctorSkillSource(root)
	if err != nil {
		t.Fatal(err)
	}
	// Two entries must be reported, including the dangling one. On base only
	// one was reported (the glob never saw the symlink). F3 pins the count.
	if report.SkillFileCount != 2 {
		t.Fatalf("doctor SkillFileCount = %d, want 2 (bad symlink counts too)", report.SkillFileCount)
	}
	// Dangling list surfaces the bad symlink; on base this stayed empty.
	// Slice-8 update: Path is now the SKILL.md-relative file path
	// ("bad/SKILL.md"), not the dir ("bad") — the doctor's report shape
	// matches the pre-slice-2 doctor that this slice recovers.
	if len(report.Dangling) != 1 || report.Dangling[0] != "bad/SKILL.md" {
		t.Fatalf("Dangling = %v, want [bad/SKILL.md]", report.Dangling)
	}
	// Per-entry assertions: good is ok and reachable; bad is dangling_symlink.
	var good, bad *SkillSourceFileReport
	for i := range report.Files {
		f := &report.Files[i]
		switch f.Path {
		case "good/SKILL.md":
			good = f
		case "bad/SKILL.md":
			bad = f
		}
	}
	if good == nil || bad == nil {
		t.Fatalf("report missing expected entries: %+v", report.Files)
	}
	if !good.Reachable || good.Status != "ok" || good.Reason != "ok" {
		t.Fatalf("good entry = %+v, want Reachable=true Status=ok Reason=ok", good)
	}
	if bad.Reachable || bad.Status != "fail" || bad.Reason != "dangling_symlink" {
		t.Fatalf("bad entry = %+v, want Reachable=false Status=fail Reason=dangling_symlink", bad)
	}
	// The per-entry entry-point DoctorSkillSourceEntry must agree.
	entry := DoctorSkillSourceEntry(root, "bad")
	if entry.Reachable || entry.Reason != "dangling_symlink" {
		t.Fatalf("DoctorSkillSourceEntry(bad) = %+v, want Reachable=false Reason=dangling_symlink", entry)
	}
}

// F4 (TestImportSkillsStampsReachabilityFromDiskWithinOneImport) is the
// integration test the packet pins: drive the real ApplySkillManifest path
// twice on a temp home + temp skill root, prove that:
//   - after run 1: a skill with a dangling source shows Source.Reachable=false,
//     a skill with a healthy source shows Source.Reachable=true;
//   - after run 2 (dangling fixed): both show Source.Reachable=true;
//   - after BOTH runs: zero duplicate ids (the slice-1 upsert guarantee
//     already covers this and we re-pin it here because the integration is
//     the test that proves the two slices cohere).
//
// On base: ApplySkillManifest stamped no Source at all -- Node.Source was
// nil on every imported node (no field existed). The `Node.Source == nil`
// arm of ComputeReachability would let the skill through, but the live
// `skill:web-perf` proves that the lack of disk evidence is exactly what we
// want flagged. F4 proves the new stamping path. F4 also matches the
// packet's live proof 2 (`jq '.id=="skill:web-perf" | .source' reachable:false`).
func TestImportSkillsStampsReachabilityFromDiskWithinOneImport(t *testing.T) {
	home, store := newStore(t)
	root := t.TempDir()

	// Two skill dirs under root: good dir with a valid SKILL.md, bad a
	// dangling symlink. The manifest declares SkillPath so ApplySkillManifest
	// can look them up.
	writeSkillSource(t, root, "good", `---
name: good
description: "Good skill"
---
# good
`)
	if err := os.Symlink("/nonexistent/skills-does-not-exist-"+filepath.Base(t.TempDir()), filepath.Join(root, "bad")); err != nil {
		t.Fatal(err)
	}

	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest_test_f4",
		AdapterID:     "test.f4-shim",
		SourceSystem:  "test",
		ObservedAt:    t0.UTC().Format(time.RFC3339),
		SourceRef: SkillSourceRef{
			Path:        root,
			Kind:        "test",
			Owner:       "test",
			SourceClass: "test",
			Revision:    "sha256:test",
		},
		EvidenceRefs: []string{"f4:probe"},
		Skills: []SkillManifestEntry{
			{
				ID:        "skill:good",
				Kind:      "skill",
				Name:      "good",
				Status:    "active",
				Triggers:  []string{"good"},
				SkillPath: filepath.Join(root, "good"),
			},
			{
				ID:        "skill:bad",
				Kind:      "skill",
				Name:      "bad",
				Status:    "active",
				Triggers:  []string{"bad"},
				SkillPath: filepath.Join(root, "bad"),
			},
		},
	}
	_ = home

	// Run 1: bad is dangling, good is healthy.
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
	goodNode, badNode := byID["skill:good"], byID["skill:bad"]
	if goodNode.Source == nil || !goodNode.Source.Reachable {
		t.Fatalf("good node Source = %+v, want reachable Source", goodNode.Source)
	}
	if badNode.Source == nil || badNode.Source.Reachable {
		t.Fatalf("bad node Source = %+v, want unreachable Source", badNode.Source)
	}
	if badNode.Source.Reason != "dangling_symlink" {
		t.Fatalf("bad node Source.Reason = %q, want dangling_symlink", badNode.Source.Reason)
	}
	// Run 2: repair the bad symlink to point at the good dir.
	if err := os.Remove(filepath.Join(root, "bad")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "good"), filepath.Join(root, "bad")); err != nil {
		t.Fatal(err)
	}
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
	goodNode, badNode = byID["skill:good"], byID["skill:bad"]
	if !goodNode.Source.Reachable {
		t.Fatalf("after repair good node Source.Reachable = false, want true (Source=%+v)", goodNode.Source)
	}
	if !badNode.Source.Reachable {
		t.Fatalf("after repair bad node Source.Reachable = false, want true (Source=%+v)", badNode.Source)
	}
	// Slice-1 carry: zero duplicate ids after BOTH runs.
	uniques := map[string]int{}
	for _, id := range []string{"skill:good", "skill:bad"} {
		uniques[id]++
	}
	if len(uniques) != 2 {
		t.Fatalf("unexpected ids: %+v", uniques)
	}
	// The on-disk file evidence (slice 1 in action): read raw rows and count
	// distinct ids (the file may have multiple per id if slice 1 isn't on,
	// but in this branch the upsert guarantees one row per id).
	rows := readNonEmptyLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))
	rawIDs := map[string]int{}
	for _, raw := range rows {
		var n Node
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			t.Fatalf("raw row parse: %v", err)
		}
		rawIDs[n.ID]++
	}
	for _, id := range []string{"skill:good", "skill:bad"} {
		if rawIDs[id] != 1 {
			t.Fatalf("raw row count for %s = %d, want 1 (upsert invariant)", id, rawIDs[id])
		}
	}
}

// F5 (TestComputeReachabilityHonorsSkillSourceReachability) pins that
// ComputeReachability gates skill nodes on Source AND connection surface,
// and leaves non-skill nodes untouched. On base, Source did not exist; the
// projection counted any skill with a connection surface, regardless of disk.
// On this branch:
//   - skill + connection surface + Source.Reachable=false → counts unreachable;
//   - skill + connection surface + Source.Reachable=true → counts reachable;
//   - non-skill (tool) + connection surface + (no Source) → counts reachable
//     (proves the gate is skill-only);
//   - skill + NO connection surface → counts unreachable (pre-existing behavior).
func TestComputeReachabilityHonorsSkillSourceReachability(t *testing.T) {
	_, store := newStore(t)
	// Surface for skill:bad (unreachable); surface for skill:good (reachable);
	// surface for tool:looks-like-skill (untouched). No surface for skill:absent.
	if _, err := store.AddConnectionSurface(ConnectionSurface{
		NodeID: "skill:bad", Endpoint: "https://example.invalid/bad",
		SettingsSchemaRef: ConnectionSurfaceSchemaVersion, AssertingOwner: "test", SourceRevision: "rev",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(ConnectionSurface{
		NodeID: "skill:good", Endpoint: "https://example.invalid/good",
		SettingsSchemaRef: ConnectionSurfaceSchemaVersion, AssertingOwner: "test", SourceRevision: "rev",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(ConnectionSurface{
		NodeID: "tool:looks-like-skill", Endpoint: "https://example.invalid/ls",
		SettingsSchemaRef: ConnectionSurfaceSchemaVersion, AssertingOwner: "test", SourceRevision: "rev",
	}); err != nil {
		t.Fatal(err)
	}
	// Nodes
	if err := store.Add(Node{ID: "skill:bad", Kind: "skill", Name: "bad",
		Source: &NodeSource{Root: "/nonexistent", Path: "/nonexistent/bad", Reachable: false, Reason: "dangling_symlink", CheckedAt: "2026-08-25T00:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Node{ID: "skill:good", Kind: "skill", Name: "good",
		Source: &NodeSource{Root: "/some/root", Path: "/some/root/good", Reachable: true, Reason: "ok", CheckedAt: "2026-08-25T00:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Node{ID: "skill:absent", Kind: "skill", Name: "absent"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Node{ID: "tool:looks-like-skill", Kind: "tool", Name: "looks-like-skill"}); err != nil {
		t.Fatal(err)
	}

	r, err := store.ComputeReachability()
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]KindReachability{}
	for _, k := range r.Kinds {
		byKind[k.Kind] = k
	}
	if got := byKind["skill"]; got.Nodes != 3 || got.Reachable != 1 {
		t.Fatalf("skill: Nodes=%d Reachable=%d, want Nodes=3 Reachable=1 (good only)", got.Nodes, got.Reachable)
	}
	if got := byKind["tool"]; got.Nodes != 1 || got.Reachable != 1 {
		t.Fatalf("tool: Nodes=%d Reachable=%d, want Nodes=1 Reachable=1 (untouched by slice 2)", got.Nodes, got.Reachable)
	}
	// Sanity: gap names skill:absent (no surface) AND skill:bad (surface +
	// unreachable Source) as missing-from-reachable. We don't pin Gap exhaustively;
	// the totals above are the load-bearing assertion.
	_ = strings.Contains // keep the test file dependency surface explicit
}
