package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/corpus"
	"compound/internal/sourceobs"
)

// TestCorpusIncludePrivateTextFlagSpellings pins the flag seam. ResolveChunkText only
// suppresses PRIVATE item text — public snippets ship by default — so the old
// `--include-text` name described the wrong thing. The accurate name is primary and
// the old spelling stays a working alias.
func TestCorpusIncludePrivateTextFlagSpellings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no flag", args: []string{"--lane", "corpus-fixture", "query"}},
		{name: "primary flag", args: []string{"--include-private-text"}, want: true},
		{name: "back-compat alias", args: []string{"--include-text"}, want: true},
		{name: "both spellings", args: []string{"--include-text", "--include-private-text"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := corpusIncludePrivateText(tt.args); got != tt.want {
				t.Fatalf("corpusIncludePrivateText(%#v) = %t, want %t", tt.args, got, tt.want)
			}
		})
	}
}

// TestCorpusFlagRevealsPrivateTextButIsNotNeededForPublic drives the flag through the
// resolver the CLI actually calls, so the flag name and the behavior it selects are
// proven together rather than separately.
func TestCorpusFlagRevealsPrivateTextButIsNotNeededForPublic(t *testing.T) {
	t.Parallel()
	privateManifest, privateBody := corpusFlagManifest(t, "item_private", "private")
	publicManifest, publicBody := corpusFlagManifest(t, "item_public", "public")

	for _, args := range [][]string{{"--include-private-text"}, {"--include-text"}} {
		resolved, err := corpus.ResolveChunkText("", privateManifest, "item_private#0", corpusIncludePrivateText(args))
		if err != nil {
			t.Fatalf("resolve private chunk with %v: %v", args, err)
		}
		if resolved.Status != corpus.ChunkTextOK || resolved.Text != privateBody {
			t.Fatalf("%v did not reveal private text: status=%q text=%q", args, resolved.Status, resolved.Text)
		}
	}

	suppressed, err := corpus.ResolveChunkText("", privateManifest, "item_private#0", corpusIncludePrivateText(nil))
	if err != nil {
		t.Fatalf("resolve private chunk without flag: %v", err)
	}
	if suppressed.Status != corpus.ChunkTextSuppressedPrivate || suppressed.Text != "" {
		t.Fatalf("private text leaked without the flag: status=%q text=%q", suppressed.Status, suppressed.Text)
	}

	public, err := corpus.ResolveChunkText("", publicManifest, "item_public#0", corpusIncludePrivateText(nil))
	if err != nil {
		t.Fatalf("resolve public chunk without flag: %v", err)
	}
	if public.Status != corpus.ChunkTextOK || public.Text != publicBody {
		t.Fatalf("public text needed a flag: status=%q text=%q", public.Status, public.Text)
	}
}

// TestCorpusHitTextLineNamesTheAccurateFlag keeps the operator-facing line honest: it
// must point at the flag that exists and say public snippets are already shown.
func TestCorpusHitTextLineNamesTheAccurateFlag(t *testing.T) {
	t.Parallel()
	line := corpusHitTextLine(corpus.QueryHit{TextStatus: string(corpus.ChunkTextSuppressedPrivate)})
	if !strings.Contains(line, "--include-private-text") {
		t.Fatalf("suppression line does not name --include-private-text: %q", line)
	}
	if !strings.Contains(line, "public") {
		t.Fatalf("suppression line does not say public snippets are shown by default: %q", line)
	}
}

func corpusFlagManifest(t *testing.T, itemID, privacy string) (corpus.Manifest, string) {
	t.Helper()
	body := "User: synthetic " + privacy + " corpus body for the flag seam."
	path := filepath.Join(t.TempDir(), "flag-seam-source.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	item := sourceobs.SourceManifestItem{
		ID:         itemID,
		SourceKind: "synthetic_dialogue",
		SourceRef:  path,
		FileHash:   "sha256:" + hex.EncodeToString(sum[:]),
		PageSlug:   "Flag Seam Fixture",
		Privacy:    privacy,
		BodyPolicy: "source_in_place",
	}
	manifest := corpus.Manifest{
		SchemaVersion:       corpus.ManifestSchemaVersion,
		Lane:                "corpus-fixture",
		PrefixSchemaVersion: corpus.PrefixSchemaVersion,
		Chunk:               corpus.ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
		Items:               []sourceobs.SourceManifestItem{item},
	}
	return manifest, body
}
