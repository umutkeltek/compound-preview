package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func TestMemoryEngineConfigRejectsPrivateCorpusLaneWithHostedEmbedder(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	cfgPath := memoryEngineConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	body := `schema_version: compound.memory_engine_config.v0.1
corpus_lanes:
  - lane: corpus-chatgpt
    source_family: chatgpt_import
    privacy: private
    embedder:
      provider: openai
      model: text-embedding-3-small
      dims: 1536
      bits: f32
      base_url: https://api.openai.com/v1
    chunk:
      unit: runes
      size: 4000
      overlap: 400
      boundary: structure
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, _, err := loadMemoryEngineConfig(homePath)
	if err == nil {
		t.Fatalf("private corpus lane with hosted embedder should be refused at config load")
	}
	if got := err.Error(); !strings.Contains(got, "corpus-chatgpt") || !strings.Contains(got, "private") {
		t.Fatalf("load error = %q, want lane/private context", got)
	}
}

func TestCorpusLanesAreNotAmbientMemoryEngineProfiles(t *testing.T) {
	if _, err := normalizeMemoryEngineProfile("corpus-chatgpt"); err == nil {
		t.Fatalf("corpus lanes must not enter ambient memory engine profile enum")
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	cfgPath := memoryEngineConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	body := fmt.Sprintf(`schema_version: compound.memory_engine_config.v0.1
corpus_lanes:
  - lane: corpus-chatgpt
    source_family: chatgpt_import
    privacy: private
    embedder:
      provider: ollama
      model: nomic-embed-text
      dims: 768
      bits: f32
      base_url: %s
    chunk:
      unit: runes
      size: 4000
      overlap: 400
      boundary: structure
`, corpusConfigOnlyEmbedderURL(t))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "")
	restore, err := configureMemoryEngineFromHome(homePath, "")
	if err != nil {
		t.Fatalf("configure memory engine should ignore corpus-only lanes: %v", err)
	}
	defer restore()
	if got := os.Getenv("COMPOUND_MEM_ENGINE_BIN"); got != "" {
		t.Fatalf("corpus lane configured ambient breadth engine bin: %q", got)
	}
	if got := os.Getenv("COMPOUND_MEMORY_CLI_PROFILE"); got != "" {
		t.Fatalf("corpus lane configured ambient memory profile: %q", got)
	}
}

func TestCorpusLaneConfigIgnoresBreadthEmbedEnvFillIn(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	cfgPath := memoryEngineConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	body := fmt.Sprintf(`schema_version: compound.memory_engine_config.v0.1
corpus_lanes:
  - lane: corpus-chatgpt
    source_family: chatgpt_import
    privacy: private
    embedder:
      provider: ollama
      model: nomic-embed-text
      dims: 768
      bits: f32
      base_url: %s
    chunk:
      unit: runes
      size: 4000
      overlap: 400
      boundary: structure
`, corpusConfigOnlyEmbedderURL(t))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "openai")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", "https://api.openai.com/v1")
	cfg, _, err := loadMemoryEngineConfig(homePath)
	if err != nil {
		t.Fatalf("load config should use corpus config, not breadth env: %v", err)
	}
	if len(cfg.CorpusLanes) != 1 || cfg.CorpusLanes[0].Embedder.Provider != "ollama" {
		t.Fatalf("corpus lane was filled from breadth env: %+v", cfg.CorpusLanes)
	}
}

// corpusConfigOnlyEmbedderURL owns a local endpoint without depending on a
// developer's embedder. These configuration tests must never issue requests.
func corpusConfigOnlyEmbedderURL(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("corpus configuration unexpectedly contacted embedder: %s %s", r.Method, r.URL.Path)
		http.Error(w, "configuration tests must not contact embedder", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/v1"
}
