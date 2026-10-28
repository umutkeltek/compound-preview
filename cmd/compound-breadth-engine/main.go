package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/breadth"
)

func main() {
	cfg, err := readConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "compound-breadth-engine:", err)
		os.Exit(1)
	}
	if err := breadth.RunProtocol(os.Stdin, os.Stdout, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "compound-breadth-engine:", err)
		os.Exit(1)
	}
}

func readConfig() (breadth.ProtocolConfig, error) {
	cfg := breadth.ProtocolConfig{
		CompoundHome: os.Getenv("COMPOUND_HOME"),
		Stderr:       os.Stderr,
	}
	if os.Getenv("COMPOUND_BREADTH_DISABLE_VECTOR") == "1" {
		return cfg, nil
	}
	model := strings.TrimSpace(os.Getenv("COMPOUND_BREADTH_EMBED_MODEL"))
	if model == "" {
		return cfg, nil
	}
	provider := strings.TrimSpace(os.Getenv("COMPOUND_BREADTH_EMBED_PROVIDER"))
	if provider == "" {
		provider = "ollama"
	}
	baseURL := strings.TrimSpace(os.Getenv("COMPOUND_BREADTH_EMBED_BASE_URL"))
	if baseURL == "" {
		baseURL = "http://" + "localhost:11434/v1"
	}
	batch := 32
	if raw := strings.TrimSpace(os.Getenv("COMPOUND_BREADTH_EMBED_BATCH")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return cfg, fmt.Errorf("invalid COMPOUND_BREADTH_EMBED_BATCH %q", raw)
		}
		batch = parsed
	}
	timeoutMS := 10000
	if raw := strings.TrimSpace(os.Getenv("COMPOUND_BREADTH_EMBED_TIMEOUT_MS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return cfg, fmt.Errorf("invalid COMPOUND_BREADTH_EMBED_TIMEOUT_MS %q", raw)
		}
		timeoutMS = parsed
	}
	embedder, err := breadth.NewOpenAICompatEmbedder(breadth.OpenAICompatEmbedderConfig{
		Provider: provider,
		Model:    model,
		BaseURL:  baseURL,
		APIKey:   os.Getenv("COMPOUND_BREADTH_EMBED_API_KEY"),
		Batch:    batch,
		Timeout:  time.Duration(timeoutMS) * time.Millisecond,
	})
	if err != nil {
		return cfg, err
	}
	cfg.Embedder = embedder
	return cfg, nil
}
