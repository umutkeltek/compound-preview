package breadth

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type embedResolverConfig struct {
	Embed *embedResolverEmbedConfig `json:"embed,omitempty" yaml:"embed,omitempty"`
}

type embedResolverEmbedConfig struct {
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model    string `json:"model,omitempty" yaml:"model,omitempty"`
	BaseURL  string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	Embedder string `json:"embedder,omitempty" yaml:"embedder,omitempty"`
}

// ResolveEmbedder is the shared opt-in resolver for breadth's semantic lane.
// No model means OFF: callers must not inherit a silent default vector space.
func ResolveEmbedder(compoundHome string) (EmbedderAdapter, bool) {
	cfg := loadEmbedResolverConfig(compoundHome)
	envModel := strings.TrimSpace(os.Getenv("COMPOUND_BREADTH_EMBED_MODEL"))
	envProvider := strings.TrimSpace(os.Getenv("COMPOUND_BREADTH_EMBED_PROVIDER"))
	model := firstNonEmpty(envModel, cfg.Model)
	if model == "" {
		return nil, false
	}
	provider := firstNonEmpty(envProvider, cfg.Provider)
	baseURL := firstNonEmpty(os.Getenv("COMPOUND_BREADTH_EMBED_BASE_URL"), cfg.BaseURL)
	embedder, err := NewOpenAICompatEmbedder(OpenAICompatEmbedderConfig{
		Provider: provider,
		Model:    model,
		BaseURL:  baseURL,
		APIKey:   os.Getenv("COMPOUND_BREADTH_EMBED_API_KEY"),
	})
	if err != nil {
		return nil, false
	}
	guard := strings.TrimSpace(cfg.Embedder)
	if envModel != "" || envProvider != "" {
		guard = ""
	}
	if guard != "" && guard != embedder.Identity() {
		return nil, false
	}
	return embedder, true
}

func loadEmbedResolverConfig(compoundHome string) embedResolverEmbedConfig {
	compoundHome = strings.TrimSpace(compoundHome)
	if compoundHome == "" {
		return embedResolverEmbedConfig{}
	}
	raw, err := os.ReadFile(filepath.Join(compoundHome, "config", "memory-engine.yaml"))
	if err != nil {
		return embedResolverEmbedConfig{}
	}
	var cfg embedResolverConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil || cfg.Embed == nil {
		return embedResolverEmbedConfig{}
	}
	return *cfg.Embed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
