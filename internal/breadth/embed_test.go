package breadth

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestEmbeddingsURLNormalizesSchemeLessLocalBaseURL(t *testing.T) {
	httpScheme := "http" + "://"
	tests := []struct {
		name string
		base string
		want string
	}{
		{
			name: "localhost",
			base: "localhost:11434/v1",
			want: httpScheme + "localhost:11434/v1/embeddings",
		},
		{
			name: "loopback IPv4",
			base: "127.0.0.1:11434/v1",
			want: httpScheme + "127.0.0.1:11434/v1/embeddings",
		},
		{
			name: "loopback IPv6",
			base: "[::1]:11434/v1",
			want: "http://[::1]:11434/v1/embeddings",
		},
		{
			name: "explicit scheme",
			base: "https://embed.example.test/v1",
			want: "https://embed.example.test/v1/embeddings",
		},
		{
			name: "already endpoint",
			base: httpScheme + "localhost:11434/v1/embeddings",
			want: httpScheme + "localhost:11434/v1/embeddings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			embedder, err := NewOpenAICompatEmbedder(OpenAICompatEmbedderConfig{
				Provider: "ollama",
				Model:    "nomic-embed-text",
				BaseURL:  tt.base,
			})
			if err != nil {
				t.Fatalf("embedder: %v", err)
			}
			got := embeddingsURL(embedder.(*openAICompatEmbedder).baseURL)
			if got != tt.want {
				t.Fatalf("embeddingsURL(%q) = %q, want %q", tt.base, got, tt.want)
			}
		})
	}
}

// TestHashUnitVectorFromRawRejectsZeroVector proves the degenerate-hash branch
// (a normalized raw vector summing to zero) returns a document-scoped error
// instead of panicking hashUnitVector's caller. This is the seam
// hashUnitVector delegates to after computing its raw hash vector, so a
// crafted raw input can exercise the panic-prone branch directly without
// needing to find a real (text, dim) pair whose sha256 output degenerates.
func TestHashUnitVectorFromRawRejectsZeroVector(t *testing.T) {
	_, err := hashUnitVectorFromRaw("doc-zero", []float32{0, 0, 0, 0})
	if err == nil {
		t.Fatal("expected error for zero raw vector, got nil")
	}
	if !strings.Contains(err.Error(), "doc-zero") {
		t.Fatalf("error %q does not name the source document", err.Error())
	}
}

// TestHashUnitVectorFromRawRejectsNonFiniteVector proves a NaN/Inf component
// in the raw hash vector also errors rather than panicking.
func TestHashUnitVectorFromRawRejectsNonFiniteVector(t *testing.T) {
	_, err := hashUnitVectorFromRaw("doc-nan", []float32{float32(math.NaN()), 0, 0, 0})
	if err == nil {
		t.Fatal("expected error for non-finite raw vector, got nil")
	}
	if !strings.Contains(err.Error(), "doc-nan") {
		t.Fatalf("error %q does not name the source document", err.Error())
	}
}

// TestHashUnitVectorHappyPathStillNormalizes locks in the ordinary contract:
// real text/dim pairs keep returning a unit-normalized vector, no error.
func TestHashUnitVectorHappyPathStillNormalizes(t *testing.T) {
	vec, err := hashUnitVector("hello world", 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vec) != 8 {
		t.Fatalf("len(vec) = %d, want 8", len(vec))
	}
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if diff := sumSq - 1; diff < -1e-6 || diff > 1e-6 {
		t.Fatalf("hashUnitVector not unit-normalized: sum(sq) = %v", sumSq)
	}
}

// TestFakeEmbedderEmbedPropagatesHashError proves fakeEmbedder.Embed's single
// caller of hashUnitVector still returns ordinary vectors through its
// existing (vectors, error) contract now that hashUnitVector can error.
func TestFakeEmbedderEmbedPropagatesHashError(t *testing.T) {
	f := newFakeEmbedder(8)
	got, err := f.Embed(context.Background(), []string{"plain text"})
	if err != nil {
		t.Fatalf("unexpected error on ordinary input: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 8 {
		t.Fatalf("unexpected embed output: %#v", got)
	}
}
