package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"compound/internal/home"
)

// TestSynthesisDigestCLIOnEmptyHomeIsZeroActionAndReadOnly is falsifier f5 (packet
// packet-reconcile-slice3-split-card-digest.md): `compound synthesis digest` on an
// empty/unchanged temp home — reconcile never ran, nothing to fold — must render a zero-action
// page, return normally (exit 0; no die/os.Exit path taken), and leave the home byte-identical.
// Digest is strictly read-only: it never opens a WorkEnvelope, writes a checkpoint, or appends
// to the ledger, so a home with no reconcile history must come back exactly as it went in.
func TestSynthesisDigestCLIOnEmptyHomeIsZeroActionAndReadOnly(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	before := hashHomeDir(t, homePath)

	out := captureStdout(t, func() {
		cmdSynthesis([]string{"digest"})
	})

	after := hashHomeDir(t, homePath)
	if before != after {
		t.Fatalf("digest mutated an empty/unchanged home: before=%s after=%s", before, after)
	}
	if strings.TrimSpace(out) != "reconcile digest: zero actions" {
		t.Fatalf("expected a zero-action digest page on an empty home, got %q", out)
	}
}

// hashHomeDir hashes every regular file under homePath (relative path + content, sorted) so
// tests can assert a COMPOUND_HOME is byte-identical before and after a run that must be
// read-only. Directory creation alone (e.g. mem.Open's idempotent MkdirAll of already-existing
// dirs) does not change this hash; any written or modified file does.
func hashHomeDir(t *testing.T, homePath string) string {
	t.Helper()
	var rels []string
	if err := filepath.WalkDir(homePath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(homePath, path)
		if err != nil {
			return err
		}
		rels = append(rels, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(homePath, rel))
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
