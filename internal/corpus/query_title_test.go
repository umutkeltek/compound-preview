package corpus

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestChunkTitleNeverLeaksLocalPath pins the title rule directly. Chunk text is
// privacy-gated at ResolveChunkText, but the title travels the same surfaces
// (`memory search --json`, the console, ContextPack materialization) and was
// falling back to the raw source_ref — a full local path — whenever an item had no
// page_slug. A private basename is itself sensitive, so private items get an opaque
// label rather than a trimmed path.
func TestChunkTitleNeverLeaksLocalPath(t *testing.T) {
	tests := []struct {
		name      string
		pageSlug  string
		sourceRef string
		privacy   string
		want      string
	}{
		{
			name:      "page slug wins",
			pageSlug:  "Synthetic Corpus Fixture",
			sourceRef: "/synthetic/root/notes/secret.md",
			privacy:   "private",
			want:      "Synthetic Corpus Fixture",
		},
		{
			name:      "private without slug is opaque",
			sourceRef: "/synthetic/root/notes/secret.md",
			privacy:   "private",
			want:      privateChunkTitle,
		},
		{
			name:      "private privacy is case insensitive",
			sourceRef: "/synthetic/root/notes/secret.md",
			privacy:   "  PRIVATE ",
			want:      privateChunkTitle,
		},
		{
			name:      "public without slug is basename only",
			sourceRef: "/tmp/source.md",
			privacy:   "public",
			want:      "source.md",
		},
		{
			name:      "degenerate base falls back",
			sourceRef: "/",
			privacy:   "public",
			want:      untitledChunkTitle,
		},
		{
			name:      "empty source ref falls back",
			sourceRef: "   ",
			privacy:   "public",
			want:      untitledChunkTitle,
		},
		{
			name:      "dot source ref falls back",
			sourceRef: ".",
			privacy:   "public",
			want:      untitledChunkTitle,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chunkTitle(tt.pageSlug, tt.sourceRef, tt.privacy)
			if got != tt.want {
				t.Fatalf("chunkTitle(%q, %q, %q) = %q, want %q", tt.pageSlug, tt.sourceRef, tt.privacy, got, tt.want)
			}
			if tt.pageSlug == "" && strings.ContainsRune(got, filepath.Separator) {
				t.Fatalf("title %q carries a path separator", got)
			}
		})
	}
}

// TestQueryHitTitleDoesNotLeakPrivateSourcePath proves the rule end-to-end through
// Query: a private item with no page_slug must not put any part of its local path in
// the hit title, and the text gate must stay closed at the same time.
func TestQueryHitTitleDoesNotLeakPrivateSourcePath(t *testing.T) {
	fixture := seedCorpusTextFixtureWith(t, corpusFixtureOpts{
		privacy:  "private",
		pageSlug: "",
		fileName: "secret.md",
		body:     "User: synthetic private source text.",
	})

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "private", 1)
	if err != nil {
		t.Fatalf("query private corpus: %v", err)
	}
	hit := onlyCorpusTextHit(t, report)
	if strings.Contains(hit.Title, "secret.md") {
		t.Fatalf("hit title leaked the private basename: %q", hit.Title)
	}
	if strings.Contains(hit.Title, filepath.Dir(fixture.sourcePath)) {
		t.Fatalf("hit title leaked the private directory: %q", hit.Title)
	}
	if strings.ContainsRune(hit.Title, filepath.Separator) || hit.Title == fixture.sourcePath {
		t.Fatalf("hit title leaked a local path: %q", hit.Title)
	}
	if hit.Title != privateChunkTitle {
		t.Fatalf("hit title = %q, want %q", hit.Title, privateChunkTitle)
	}
	if _, status, hasText := corpusHitTextFields(t, hit); status != string(ChunkTextSuppressedPrivate) || hasText {
		t.Fatalf("text status = %q has_text=%t, want %q with no text", status, hasText, ChunkTextSuppressedPrivate)
	}
}

// TestQueryHitTitleUsesBasenameForPublicItems keeps public items legible without
// shipping the directory tree they were indexed from.
func TestQueryHitTitleUsesBasenameForPublicItems(t *testing.T) {
	fixture := seedCorpusTextFixtureWith(t, corpusFixtureOpts{
		privacy:  "public",
		pageSlug: "",
		fileName: "source.md",
		body:     "User: synthetic public source text.",
	})

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "public", 1)
	if err != nil {
		t.Fatalf("query public corpus: %v", err)
	}
	hit := onlyCorpusTextHit(t, report)
	if hit.Title != "source.md" {
		t.Fatalf("hit title = %q, want basename %q", hit.Title, "source.md")
	}
	if hit.Title == fixture.sourcePath {
		t.Fatalf("hit title is the full source path: %q", hit.Title)
	}
}

// TestQueryHitTitlePrefersPageSlug is the regression guard for the common case.
func TestQueryHitTitlePrefersPageSlug(t *testing.T) {
	fixture := seedCorpusTextFixture(t, "public", "User: synthetic slugged source text.")

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "slugged", 1)
	if err != nil {
		t.Fatalf("query slugged corpus: %v", err)
	}
	if got := onlyCorpusTextHit(t, report).Title; got != "Synthetic Corpus Fixture" {
		t.Fatalf("hit title = %q, want the page slug", got)
	}
}
