package sourceobs

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/eventstore"
)

func countCanonCandidateFiles(t *testing.T, home string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(home, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "src_cand_") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", home, err)
	}
	return count
}

var tCanon = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func writeCanonFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func canonBaseline(t *testing.T, home string, now time.Time, pairs []CanonPathNode) {
	t.Helper()
	manifest, err := BuildCanonManifest("harness-canon", pairs, now)
	if err != nil {
		t.Fatalf("build canon manifest: %v", err)
	}
	if _, err := RecordSourceManifest(home, manifest); err != nil {
		t.Fatalf("record canon manifest: %v", err)
	}
}

func readCanonEvents(t *testing.T, home string) []string {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Scan(eventstore.ScanOptions{})
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name)
	}
	return out
}

func canonContains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// Falsifier 4: baseline then observe with no live change mints nothing and writes no event.
func TestCanonObserveUnchangedMintsNothing(t *testing.T) {
	home := t.TempDir()
	srcDir := t.TempDir()
	fA := filepath.Join(srcDir, "a.toml")
	fB := filepath.Join(srcDir, "b.md")
	fC := filepath.Join(srcDir, "c.json")
	writeCanonFile(t, fA, "alpha")
	writeCanonFile(t, fB, "# beta")
	writeCanonFile(t, fC, `{"k":1}`)

	pairs := []CanonPathNode{
		{Path: fA, NodeRef: "harness:claude_code"},
		{Path: fB, NodeRef: "harness:codex"},
		{Path: fC, NodeRef: "tool:oracle"},
	}
	canonBaseline(t, home, tCanon, pairs)

	report, _, err := ObserveCanonDrift(home, "harness-canon", tCanon.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CandidateIDs) != 0 || len(report.NoPromoteIDs) != 0 || len(report.ChangedRefs) != 0 {
		t.Fatalf("unchanged observe should mint nothing: %+v", report)
	}
	if events := readCanonEvents(t, home); len(events) != 0 {
		t.Fatalf("unchanged observe should write no events, got %v", events)
	}
}

// Falsifier 5: mutate 2 of 3 baselined files -> exactly 2 no-promote candidates and exactly
// one minted transition whose projections.skill is the union of the two changed nodes' refs.
func TestCanonObserveDriftMintsOnePerChangedFileAndOneTransition(t *testing.T) {
	home := t.TempDir()
	srcDir := t.TempDir()
	fA := filepath.Join(srcDir, "a.toml")
	fB := filepath.Join(srcDir, "b.md")
	fC := filepath.Join(srcDir, "c.json")
	writeCanonFile(t, fA, "alpha")
	writeCanonFile(t, fB, "# beta")
	writeCanonFile(t, fC, `{"k":1}`)

	pairs := []CanonPathNode{
		{Path: fA, NodeRef: "harness:claude_code"},
		{Path: fB, NodeRef: "harness:codex"},
		{Path: fC, NodeRef: "tool:oracle"},
	}
	canonBaseline(t, home, tCanon, pairs)

	// mutate exactly 2 files (A + C)
	writeCanonFile(t, fA, "alpha-mutated")
	writeCanonFile(t, fC, `{"k":2}`)

	later := tCanon.Add(time.Hour)
	drift, _, err := ObserveCanonDrift(home, "harness-canon", later)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift.CandidateIDs) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(drift.CandidateIDs), drift)
	}
	if len(drift.NoPromoteIDs) != 2 {
		t.Fatalf("want 2 no-promote decisions, got %d", len(drift.NoPromoteIDs))
	}
	if len(drift.ChangedRefs) != 2 {
		t.Fatalf("want 2 changed refs, got %d", len(drift.ChangedRefs))
	}
	gotNodes := append([]string{}, drift.NodeRefs...)
	sort.Strings(gotNodes)
	wantNodes := []string{"harness:claude_code", "tool:oracle"}
	if !reflect.DeepEqual(gotNodes, wantNodes) {
		t.Fatalf("node refs = %v, want %v", gotNodes, wantNodes)
	}

	evidence := append(append([]string{}, drift.CandidateIDs...), drift.NoPromoteIDs...)
	tr, rep, err := capgraph.RecordCanonEvolution(home, capgraph.CanonEvolutionInput{
		NodeRefs:     drift.NodeRefs,
		DeltaCause:   "repair",
		CausedByRefs: []string{"work_canon_test"},
		NextConsumer: "capgraph",
		ChangedRefs:  drift.ChangedRefs,
		EvidenceRefs: evidence,
	}, later)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(tr.ID) == "" {
		t.Fatalf("expected a minted transition, got empty id")
	}
	if len(rep.EnsuredNodeIDs) != 2 {
		t.Fatalf("want 2 ensured nodes, got %v", rep.EnsuredNodeIDs)
	}
	if events := readCanonEvents(t, home); len(events) != 1 {
		t.Fatalf("want exactly 1 transition event, got %d: %v", len(events), events)
	}

	gotSkill := append([]string{}, tr.Projections.Skill...)
	sort.Strings(gotSkill)
	wantSkill := []string{"capability://harness:claude_code", "capability://tool:oracle"}
	if !reflect.DeepEqual(gotSkill, wantSkill) {
		t.Fatalf("projections.skill = %v, want %v", gotSkill, wantSkill)
	}
	if tr.Cause == nil || tr.Cause.DeltaCause != "repair" {
		t.Fatalf("transition should carry cause repair: %+v", tr.Cause)
	}
}

// Falsifier 6: every capability://<id> projection resolves to a registered node of kind harness|tool.
func TestCanonObserveProjectionRefResolvesToRegisteredNode(t *testing.T) {
	home := t.TempDir()
	srcDir := t.TempDir()
	fA := filepath.Join(srcDir, "a.toml")
	fC := filepath.Join(srcDir, "c.json")
	writeCanonFile(t, fA, "alpha")
	writeCanonFile(t, fC, `{"k":1}`)
	pairs := []CanonPathNode{
		{Path: fA, NodeRef: "harness:claude_code"},
		{Path: fC, NodeRef: "tool:oracle"},
	}
	canonBaseline(t, home, tCanon, pairs)
	writeCanonFile(t, fA, "alpha-2")
	writeCanonFile(t, fC, `{"k":2}`)

	later := tCanon.Add(time.Hour)
	drift, _, err := ObserveCanonDrift(home, "harness-canon", later)
	if err != nil {
		t.Fatal(err)
	}
	tr, _, err := capgraph.RecordCanonEvolution(home, capgraph.CanonEvolutionInput{
		NodeRefs:     drift.NodeRefs,
		DeltaCause:   "accept",
		CausedByRefs: []string{"work_canon_test"},
		ChangedRefs:  drift.ChangedRefs,
		EvidenceRefs: drift.CandidateIDs,
	}, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Projections.Skill) == 0 {
		t.Fatalf("expected projection refs")
	}
	store, err := capgraph.OpenReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]capgraph.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, ref := range tr.Projections.Skill {
		id := strings.TrimPrefix(ref, "capability://")
		n, ok := byID[id]
		if !ok {
			t.Fatalf("projection %s does not resolve to a registered node", ref)
		}
		if n.Kind != "harness" && n.Kind != "tool" {
			t.Fatalf("node %s kind=%s, want harness|tool", id, n.Kind)
		}
	}
}

// Falsifier 7: an unknown-kind node ref still parks a no-promote candidate, lands in SkippedNodeRefs,
// is never registered as a node, and is never projected by the transition.
func TestCanonObserveUnknownKindNodeRefParksNotSilent(t *testing.T) {
	home := t.TempDir()
	srcDir := t.TempDir()
	fA := filepath.Join(srcDir, "a.toml")
	fG := filepath.Join(srcDir, "g.toml")
	writeCanonFile(t, fA, "alpha")
	writeCanonFile(t, fG, "gremlin-body")
	pairs := []CanonPathNode{
		{Path: fA, NodeRef: "harness:claude_code"},
		{Path: fG, NodeRef: "gremlin:foo"},
	}
	canonBaseline(t, home, tCanon, pairs)
	writeCanonFile(t, fA, "alpha-2")
	writeCanonFile(t, fG, "gremlin-body-2")

	later := tCanon.Add(time.Hour)
	drift, _, err := ObserveCanonDrift(home, "harness-canon", later)
	if err != nil {
		t.Fatal(err)
	}
	// Candidate parked for BOTH changed files, including the gremlin one.
	if len(drift.CandidateIDs) != 2 {
		t.Fatalf("both changed files should park a candidate, got %d", len(drift.CandidateIDs))
	}
	if !canonContains(drift.NodeRefs, "gremlin:foo") {
		t.Fatalf("drift should carry gremlin node ref: %v", drift.NodeRefs)
	}

	tr, rep, err := capgraph.RecordCanonEvolution(home, capgraph.CanonEvolutionInput{
		NodeRefs:     drift.NodeRefs,
		DeltaCause:   "invalidate",
		CausedByRefs: []string{"work_canon_test"},
		ChangedRefs:  drift.ChangedRefs,
		EvidenceRefs: drift.CandidateIDs,
	}, later)
	if err != nil {
		t.Fatal(err)
	}
	if !canonContains(rep.SkippedNodeRefs, "gremlin:foo") {
		t.Fatalf("gremlin ref should be reported skipped, not silent: %+v", rep)
	}
	if !canonContains(rep.EnsuredNodeIDs, "harness:claude_code") {
		t.Fatalf("valid harness node should be ensured: %+v", rep)
	}
	store, err := capgraph.OpenReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == "gremlin:foo" {
			t.Fatalf("gremlin ref must not be registered as a node")
		}
	}
	for _, ref := range tr.Projections.Skill {
		if strings.Contains(ref, "gremlin") {
			t.Fatalf("transition must not project the skipped gremlin ref: %s", ref)
		}
	}
}

// Falsifier 8: the new tracked source files carry no personal path or operator identity.
func TestHarnessCanonSourceHasNoPersonalPath(t *testing.T) {
	files := []string{
		"canon_drift.go",
		filepath.Join("..", "capgraph", "canon_evolution.go"),
	}
	forbidden := []string{"/Users/", "/home/", ".claude/settings", ".compound/private"}
	if u := strings.TrimSpace(os.Getenv("USER")); len(u) >= 4 {
		forbidden = append(forbidden, u)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := string(data)
		for _, frag := range forbidden {
			if strings.Contains(body, frag) {
				t.Fatalf("%s contains forbidden personal fragment %q", f, frag)
			}
		}
	}
}

// Falsifier 9 (two-phase drift + idempotent re-park): ObserveCanonDrift parks candidates but does NOT
// commit the baseline, so re-observing the same drift still detects it and re-parks the SAME candidate
// (no duplicate). Only CommitCanonBaseline consumes the drift. This is the transaction-safety contract
// D1 relies on; against the earlier "re-store inside observe" design the second observe would see no
// drift and this test would fail.
func TestObserveCanonDriftIsTwoPhaseAndReparkIsIdempotent(t *testing.T) {
	home := t.TempDir()
	srcDir := t.TempDir()
	fA := filepath.Join(srcDir, "a.toml")
	writeCanonFile(t, fA, "alpha")
	pairs := []CanonPathNode{{Path: fA, NodeRef: "harness:claude_code"}}
	canonBaseline(t, home, tCanon, pairs)
	writeCanonFile(t, fA, "alpha-mutated")

	later := tCanon.Add(time.Hour)
	drift1, manifest, err := ObserveCanonDrift(home, "harness-canon", later)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift1.ChangedRefs) != 1 || len(drift1.CandidateIDs) != 1 {
		t.Fatalf("first observe = %+v, want one changed ref and one candidate", drift1)
	}

	// Observe did NOT commit the baseline: a second observe still detects the same drift, and re-parking
	// overwrites the same content-addressed candidate rather than spraying a duplicate.
	drift2, _, err := ObserveCanonDrift(home, "harness-canon", later)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift2.ChangedRefs) != 1 || len(drift2.CandidateIDs) != 1 {
		t.Fatalf("second observe = %+v, want the drift to still be detectable (baseline uncommitted)", drift2)
	}
	if got := countCanonCandidateFiles(t, home); got != 1 {
		t.Fatalf("re-park duplicated candidate files: got %d, want 1", got)
	}

	// Only committing the rebaselined manifest consumes the drift.
	if err := CommitCanonBaseline(home, manifest); err != nil {
		t.Fatal(err)
	}
	drift3, _, err := ObserveCanonDrift(home, "harness-canon", later)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift3.ChangedRefs) != 0 {
		t.Fatalf("CommitCanonBaseline did not consume the drift: %+v", drift3)
	}
}
