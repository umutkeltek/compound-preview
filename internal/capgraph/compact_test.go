package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// F1 (TestStoreAddReplacesDriftedSameIDRow) pins the upsert-by-id Store.Add.
// On base the same test would show 2 raw rows after a drifted Add (the append-
// only path appends a row instead of replacing); on this branch it shows 1
// raw row with the new content, and the first-seen-order position of the
// affected node is preserved.
func TestStoreAddReplacesDriftedSameIDRow(t *testing.T) {
	home, s := newStore(t)
	logPath := filepath.Join(home, "registries", "capability-nodes.jsonl")

	// Replay (byte-identical) — still a no-op at the file level.
	base := Node{ID: "skill:alpha", Kind: "skill", Name: "alpha v1", Weight: 1, Triggers: []string{"a"}}
	if err := s.Add(base); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(base); err != nil {
		t.Fatal(err)
	}
	if got := len(readNonEmptyLines(t, logPath)); got != 1 {
		t.Fatalf("replay should keep 1 row, got %d", got)
	}

	// A different id is appended AS BEFORE (NOT rewritten -- no drift to coalesce).
	if err := s.Add(Node{ID: "skill:pre", Kind: "skill", Name: "pre"}); err != nil {
		t.Fatal(err)
	}
	if got := len(readNonEmptyLines(t, logPath)); got != 2 {
		t.Fatalf("different-id add should append, want 2 rows, got %d", got)
	}

	// Drift: same id, different content. ON BASE this would append a 3rd row;
	// ON THIS BRANCH the rewrite replaces in place so the file still has 2 rows
	// and order is preserved.
	drift := Node{ID: "skill:alpha", Kind: "skill", Name: "alpha v2", Weight: 1, Triggers: []string{"a", "b"}}
	if err := s.Add(drift); err != nil {
		t.Fatal(err)
	}
	if got := len(readNonEmptyLines(t, logPath)); got != 2 {
		t.Fatalf("drift Add should replace in place (2 rows total), got %d", got)
	}

	loaded, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("List returned %d nodes, want 2", len(loaded))
	}
	// First-seen order is preserved: alpha was added first, pre second; the
	// rewrite must keep alpha at index 0 with its NEW content.
	if loaded[0].ID != "skill:alpha" || loaded[0].Name != "alpha v2" {
		t.Fatalf("loaded[0] = %+v, want {id: skill:alpha, name: alpha v2}", loaded[0])
	}
	if !reflect.DeepEqual(loaded[0].Triggers, []string{"a", "b"}) {
		t.Fatalf("loaded[0].Triggers = %v, want [a b] (drift visible to readers)", loaded[0].Triggers)
	}
	if loaded[1].ID != "skill:pre" {
		t.Fatalf("loaded[1] = %+v, want skill:pre", loaded[1])
	}
}

// F2 (TestCompactNodesLastWinsFirstSeenOrder) pins the one-shot compaction:
// last-wins per id, first-seen order preserved, byte-for-byte loadNodes
// equivalence before vs after, and the report shape {rows_before, rows_after,
// duplicate_ids}. The seeded log [a, b, a', c, b'] is the exact scenario from
// packet §F2; after compact the deduped log is [a'(v2), b'(v2), c].
func TestCompactNodesLastWinsFirstSeenOrder(t *testing.T) {
	home := t.TempDir()
	if err := writeSeedNodesLog(home, []Node{
		{ID: "skill:a", Kind: "skill", Name: "a-v1"},
		{ID: "skill:b", Kind: "skill", Name: "b-v1"},
		{ID: "skill:a", Kind: "skill", Name: "a-v2"},
		{ID: "skill:c", Kind: "skill", Name: "c-v1"},
		{ID: "skill:b", Kind: "skill", Name: "b-v2"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}

	before, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("before rows = %d, want 3 (loadNodes last-wins-first-seen)", len(before))
	}
	if before[0].ID != "skill:a" || before[0].Name != "a-v2" {
		t.Fatalf("before[0] = %+v, want last-wins a-v2", before[0])
	}
	if before[1].ID != "skill:b" || before[1].Name != "b-v2" {
		t.Fatalf("before[1] = %+v, want last-wins b-v2", before[1])
	}
	if before[2].ID != "skill:c" {
		t.Fatalf("before[2] = %+v, want skill:c", before[2])
	}

	report, err := store.CompactNodes()
	if err != nil {
		t.Fatal(err)
	}
	if report.RowsBefore != 5 || report.RowsAfter != 3 || report.DuplicateIDs != 2 {
		t.Fatalf("report = %+v, want {RowsBefore:5, RowsAfter:3, DuplicateIDs:2} (F2 3-tuple)", report)
	}
	if got := strings.Join(report.DuplicateIDList, ","); got != "skill:a,skill:b" {
		t.Fatalf("report.DuplicateIDList = %v, want [skill:a skill:b]", report.DuplicateIDList)
	}

	after, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	// Byte-for-byte equivalence is the safety proof: loadNodes() before and
	// after compact must return the same slice.
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("loadNodes drifted across compact:\nbefore=%+v\nafter=%+v", before, after)
	}

	// On-disk log must now have exactly 3 rows, in first-seen order.
	logPath := filepath.Join(home, "registries", "capability-nodes.jsonl")
	rows := readNonEmptyLines(t, logPath)
	if got := len(rows); got != 3 {
		t.Fatalf("post-compact rows = %d, want 3", got)
	}
	wantOrder := []string{"skill:a", "skill:b", "skill:c"}
	for i, raw := range rows {
		var n Node
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			t.Fatalf("post-compact row %d unmarshal: %v", i, err)
		}
		if n.ID != wantOrder[i] {
			t.Fatalf("post-compact row %d = %q, want %q (first-seen order on disk)", i, n.ID, wantOrder[i])
		}
	}
	wantLastWins := map[string]string{"skill:a": "a-v2", "skill:b": "b-v2", "skill:c": "c-v1"}
	for _, raw := range rows {
		var n Node
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			t.Fatalf("post-compact row unmarshal: %v", err)
		}
		if want, ok := wantLastWins[n.ID]; ok && n.Name != want {
			t.Fatalf("post-compact %s name = %q, want %q (last-wins on disk)", n.ID, n.Name, want)
		}
	}

	// Idempotence: a second compact must report {3, 3, 0} and the loadNodes
	// output must still equal `before` -- this is the live proof of the safety
	// promise and the property that lets PreviewCompact show "0 changes needed"
	// on an already-clean home.
	second, err := store.CompactNodes()
	if err != nil {
		t.Fatal(err)
	}
	if second.RowsBefore != 3 || second.RowsAfter != 3 || second.DuplicateIDs != 0 {
		t.Fatalf("second compact report = %+v, want {3, 3, 0}", second)
	}
	again, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, again) {
		t.Fatalf("loadNodes drifted across second compact:\nbefore=%+v\nagain=%+v", before, again)
	}

	// PreviewCompact on a dirty log must agree with a subsequent CompactNodes:
	// both compute the same dedupe, both report the same 3-tuple. This is
	// what `compound capability compact --dry-run` relies on.
	preDirtyHome := t.TempDir()
	if err := writeSeedNodesLog(preDirtyHome, []Node{
		{ID: "x", Kind: "skill", Name: "x-v1"},
		{ID: "y", Kind: "skill", Name: "y-v1"},
		{ID: "x", Kind: "skill", Name: "x-v2"},
	}); err != nil {
		t.Fatal(err)
	}
	preStore, err := Open(preDirtyHome)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := preStore.PreviewCompact()
	if err != nil {
		t.Fatal(err)
	}
	if preview.RowsBefore != 3 || preview.RowsAfter != 2 || preview.DuplicateIDs != 1 {
		t.Fatalf("preview report = %+v, want {3, 2, 1}", preview)
	}
}

func TestCompactNodesRefusesMalformedRowsWithoutRewriting(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "registries", "capability-nodes.jsonl")
	original := []byte("{\"id\":\"skill:a\",\"kind\":\"skill\",\"name\":\"a\"}\n{torn\n")
	if err := os.WriteFile(logPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	preview, err := store.PreviewCompact()
	if err == nil || preview.MalformedRows != 1 {
		t.Fatalf("preview must report malformed row and refuse: report=%+v err=%v", preview, err)
	}
	report, err := store.CompactNodes()
	if err == nil || report.MalformedRows != 1 {
		t.Fatalf("compact must report malformed row and refuse: report=%+v err=%v", report, err)
	}
	if err := store.Add(Node{ID: "skill:a", Kind: "skill", Name: "changed"}); err == nil {
		t.Fatal("drifted Add must not rewrite around a malformed row")
	}
	after, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !reflect.DeepEqual(after, original) {
		t.Fatalf("refused compact rewrote corrupt evidence:\n got %q\nwant %q", after, original)
	}
}

// writeSeedNodesLog drops a hand-crafted raw nodes log into a home's
// registries dir so tests can pin behaviour against a known-dirty starting
// state without going through the public Store surface. JSON encoding mirrors
// appendJSONL's bytes (json.Encoder adds a trailing '\n') so loadNodes reads
// the same content either way.
func writeSeedNodesLog(home string, rows []Node) error {
	logDir := filepath.Join(home, "registries")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(logDir, "capability-nodes.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for i := range rows {
		if err := enc.Encode(&rows[i]); err != nil {
			return err
		}
	}
	return nil
}
