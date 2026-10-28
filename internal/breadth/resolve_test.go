package breadth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveEmbedderPrecedence(t *testing.T) {
	t.Run("absent config is off", func(t *testing.T) {
		clearResolveEmbedderEnv(t)
		if embedder, ok := ResolveEmbedder(t.TempDir()); ok || embedder != nil {
			t.Fatalf("absent embedder config = %v ok=%t, want off", embedder, ok)
		}
	})

	t.Run("yaml enables embedder", func(t *testing.T) {
		clearResolveEmbedderEnv(t)
		home := t.TempDir()
		writeResolveEmbedderConfig(t, home, `
profile: breadth
embed:
  provider: openai
  model: text-embedding-3-small
  base_url: http://yaml.invalid/v1
  embedder: openai:text-embedding-3-small:1536:f32
`)
		embedder, ok := ResolveEmbedder(home)
		if !ok {
			t.Fatal("yaml embedder config did not resolve")
		}
		if got := embedder.Identity(); got != "openai:text-embedding-3-small:1536:f32" {
			t.Fatalf("identity = %q, want yaml identity", got)
		}
	})

	t.Run("env wins over yaml", func(t *testing.T) {
		clearResolveEmbedderEnv(t)
		home := t.TempDir()
		writeResolveEmbedderConfig(t, home, `
profile: breadth
embed:
  provider: ollama
  model: nomic-embed-text
  base_url: http://yaml.invalid/v1
  embedder: ollama:nomic-embed-text:768:f32
`)
		t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "text-embedding-3-small")
		t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "openai")
		t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", "http://env.invalid/v1")
		embedder, ok := ResolveEmbedder(home)
		if !ok {
			t.Fatal("env embedder config did not resolve")
		}
		if got := embedder.Identity(); got != "openai:text-embedding-3-small:1536:f32" {
			t.Fatalf("identity = %q, want env identity", got)
		}
	})
}

func clearResolveEmbedderEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"COMPOUND_BREADTH_EMBED_MODEL",
		"COMPOUND_BREADTH_EMBED_PROVIDER",
		"COMPOUND_BREADTH_EMBED_BASE_URL",
		"COMPOUND_BREADTH_EMBED_API_KEY",
	} {
		t.Setenv(key, "")
	}
}

func writeResolveEmbedderConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory-engine.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
