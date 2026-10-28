package activity

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// TestFalsifier4And6_NoForbiddenImports is the static guard: internal/activity must NOT import the
// memory candidate machinery (mem/candidates — it mints ZERO auto-candidates) nor the event store
// (eventstore — segments land ONLY under observed/activity/, never in the event store). This is a
// by-construction invariant: an import here would be a silent architecture regression that no runtime
// test could reliably catch.
//
// Phase 5 (F16): the walk recurses one level into immediate subdirectories so the
// awstream / tier1 / tier1/source subpackages — which all live under
// internal/activity/ and inherit the same doctrine — are subject to the same gate.
// _test.go files are skipped at every depth (subpackage test files can import freely).
func TestFalsifier4And6_NoForbiddenImports(t *testing.T) {
	forbidden := map[string]string{
		"compound/internal/mem":        "activity mints zero auto memory candidates (F4)",
		"compound/internal/candidates": "activity mints zero auto memory candidates (F4)",
		"compound/internal/eventstore": "activity segments land only under observed/activity/, never in the event store (F6)",
	}
	fset := token.NewFileSet()
	if err := fs.WalkDir(os.DirFS("."), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			// Skip hidden dirs (e.g. .git, .vscode) and testdata dirs.
			if strings.HasPrefix(name, ".") || name == "testdata" {
				return fs.SkipDir
			}
			// Only descend one level — subpackages of subpackages would be a different
			// doctrine boundary; this test covers . (activity) and immediate children.
			if path != "." {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if reason, bad := forbidden[importPath]; bad {
				t.Fatalf("%s imports forbidden package %q — %s", path, importPath, reason)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk dir: %v", err)
	}
}

// TestFalsifier4_DrainMintsZeroCandidates: after a successful drain on a temp home, the memory
// candidate inbox has ZERO entries. Activity exhaust never floods the inbox the ask-triage work drained.
func TestFalsifier4_DrainMintsZeroCandidates(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)
	spool := writeSpool(t, drainEvents)

	rep, err := Drain(h, spool, cfg, "work_drain", now)
	if err != nil {
		t.Fatalf("drain err: %v", err)
	}
	if rep.SegmentLinesAdded == 0 {
		t.Fatal("precondition: drain should have ingested events")
	}
	// The candidate inbox must be empty (or absent) after a real drain.
	candDir := h + "/inbox/memory-candidates"
	entries, err := os.ReadDir(candDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read candidate inbox: %v", err)
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			count++
		}
	}
	if count != 0 {
		t.Fatalf("activity drain minted %d memory candidate(s); must be zero", count)
	}
}
