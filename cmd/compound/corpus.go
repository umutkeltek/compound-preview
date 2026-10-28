package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/breadth"
	"compound/internal/corpus"
	"compound/internal/home"
	"compound/internal/retrievalparity"
)

func cmdCorpus(args []string) {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(os.Stderr, corpusUsage())
		os.Exit(2)
	}
	switch args[0] {
	case "index":
		cmdCorpusIndex(args[1:])
	case "doctor":
		cmdCorpusDoctor(args[1:])
	default:
		fmt.Fprintln(os.Stderr, corpusUsage())
		os.Exit(2)
	}
}

func corpusUsage() string {
	return "usage: compound corpus <index --lane L [--rebuild] [--json]|doctor --lane L [--json]>"
}

func cmdCorpusIndex(args []string) {
	laneName := valueFlag(args, "--lane")
	if laneName == "" {
		fmt.Fprintln(os.Stderr, "usage: compound corpus index --lane L [--rebuild] [--json]")
		os.Exit(2)
	}
	homePath, lane := mustCorpusLane(laneName)
	embedder, err := newCorpusEmbedder(lane)
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus index:", err)
		os.Exit(1)
	}
	report, err := corpus.Index(context.Background(), homePath, lane, embedder, corpus.IndexOptions{Rebuild: boolFlag(args, "--rebuild")})
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus index:", err)
		os.Exit(1)
	}
	if boolFlag(args, "--json") {
		printJSON(report)
		return
	}
	fmt.Printf("# corpus index %s\n", report.Lane)
	fmt.Printf("- index: %s\n", report.IndexDir)
	fmt.Printf("- identity: %s\n", report.EmbedderIdentity)
	fmt.Printf("- items: %d\n", report.Items)
	fmt.Printf("- chunks: %d embedded=%d reused=%d\n", report.TotalChunks, report.EmbeddedChunks, report.ReusedChunks)
	fmt.Printf("- staleness: added=%d changed=%d removed=%d\n", report.Added, report.Changed, report.Removed)
}

func cmdCorpusDoctor(args []string) {
	laneName := valueFlag(args, "--lane")
	if laneName == "" {
		fmt.Fprintln(os.Stderr, "usage: compound corpus doctor --lane L [--json]")
		os.Exit(2)
	}
	homePath, lane := mustCorpusLane(laneName)
	report, err := corpus.Doctor(homePath, lane)
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus doctor:", err)
		os.Exit(1)
	}
	if boolFlag(args, "--json") {
		printJSON(report)
		if report.Status != "ok" {
			os.Exit(1)
		}
		return
	}
	fmt.Printf("# corpus doctor %s\n", report.Lane)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- index: %s\n", report.IndexDir)
	fmt.Printf("- items: %d chunks=%d embedded=%d\n", report.Items, report.Chunks, report.EmbeddedChunks)
	for _, check := range report.Checks {
		fmt.Printf("- %s: %s", check.Name, check.Status)
		if check.Detail != "" {
			fmt.Printf(" (%s)", check.Detail)
		}
		fmt.Println()
	}
	if report.Status != "ok" {
		os.Exit(1)
	}
}

func cmdMemoryCorpusSearch(homePath string, args []string) {
	laneName := valueFlag(args, "--lane")
	query := firstPositional(args, map[string]bool{"--lane": true, "--k": true, "--neighbors": true})
	if laneName == "" || query == "" {
		fmt.Fprintln(os.Stderr, "usage: compound memory search --lane L <query> [--k N] [--include-private-text] [--neighbors R] [--json]")
		os.Exit(2)
	}
	_, lane := mustCorpusLaneFromHome(homePath, laneName)
	embedder, err := newCorpusEmbedder(lane)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory search:", err)
		os.Exit(1)
	}
	k := 10
	if raw := valueFlag(args, "--k"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fmt.Fprintln(os.Stderr, "memory search: --k must be non-negative integer")
			os.Exit(2)
		}
		k = n
	}
	neighbors := 0
	if raw := valueFlag(args, "--neighbors"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fmt.Fprintln(os.Stderr, "memory search: --neighbors must be non-negative integer")
			os.Exit(2)
		}
		if n > corpus.MaxNeighborRadius {
			fmt.Fprintf(os.Stderr, "memory search: --neighbors %d exceeds max %d\n", n, corpus.MaxNeighborRadius)
			os.Exit(2)
		}
		neighbors = n
	}
	includeText := corpusIncludePrivateText(args)
	retrievalStarted := time.Now()
	report, err := corpus.QueryWithOptions(context.Background(), homePath, lane, embedder, query, k, corpus.QueryOptions{IncludeText: includeText, NeighborRadius: neighbors})
	retrievalparity.Record(homePath, retrievalparity.NewRow(retrievalparity.ConsumerSearch, query, parityCorpusRefs(report), time.Since(retrievalStarted), err == nil, time.Now()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory search:", err)
		os.Exit(1)
	}
	if boolFlag(args, "--json") {
		printJSON(report)
		return
	}
	fmt.Printf("# %d corpus hit(s) for %q lane=%s\n", len(report.Hits), report.Query, report.Lane)
	for _, hit := range report.Hits {
		fmt.Printf("%s item=%s score=%.4f %s\n", hit.ChunkID, hit.ItemID, hit.Score, hit.Title)
		fmt.Printf("  text[%s]: %s\n", hit.TextStatus, corpusHitTextLine(hit))
		for _, n := range hit.Neighbors {
			fmt.Printf("  neighbor[%s] ord=%d: %s\n", n.TextStatus, n.Ordinal, corpusHitTextLine(corpus.QueryHit{TextStatus: n.TextStatus, Text: n.Text}))
		}
	}
}

// corpusIncludePrivateText reports whether the caller opted into private item text.
// The gate in corpus.ResolveChunkText only suppresses privacy=private items — public
// snippets are returned by default — so the accurate name is --include-private-text.
// --include-text is kept as a back-compat alias for the original spelling.
func corpusIncludePrivateText(args []string) bool {
	return boolFlag(args, "--include-private-text") || boolFlag(args, "--include-text")
}

// corpusHitTextLine renders a bounded, human-readable line for a hit's text: an explicit
// reason when the text was withheld (never a silent blank), else a rune-safe snippet.
func corpusHitTextLine(hit corpus.QueryHit) string {
	switch hit.TextStatus {
	case "suppressed_private":
		return "(private source — public snippets are shown by default; pass --include-private-text to reveal this one)"
	case "stale_source":
		return "(source changed since indexing — re-run `compound corpus index` to refresh)"
	case "stale_index":
		return "(source was indexed with an older chunk schema — re-run `compound corpus index`)"
	case "source_unavailable":
		return "(source file no longer readable at its indexed path)"
	default:
		return snippetRunes(hit.Text, 240)
	}
}

func snippetRunes(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func mustCorpusLane(laneName string) (string, corpus.LaneConfig) {
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	return mustCorpusLaneFromHome(homePath, laneName)
}

func mustCorpusLaneFromHome(homePath, laneName string) (string, corpus.LaneConfig) {
	cfg, _, err := loadMemoryEngineConfig(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory engine config:", err)
		os.Exit(1)
	}
	laneName = strings.TrimSpace(laneName)
	for _, lane := range cfg.CorpusLanes {
		if lane.Lane == laneName {
			return homePath, lane
		}
	}
	fmt.Fprintf(os.Stderr, "corpus lane %q not configured\n", laneName)
	os.Exit(1)
	return "", corpus.LaneConfig{}
}

type configuredCorpusEmbedder struct {
	adapter  breadth.EmbedderAdapter
	identity string
	dim      int
}

func newCorpusEmbedder(lane corpus.LaneConfig) (corpus.Embedder, error) {
	provider := lane.Embedder.Provider
	if strings.EqualFold(provider, "local") {
		provider = "ollama"
	}
	adapter, err := breadth.NewOpenAICompatEmbedder(breadth.OpenAICompatEmbedderConfig{
		Provider: provider,
		Model:    lane.Embedder.Model,
		BaseURL:  lane.Embedder.BaseURL,
	})
	if err != nil {
		return nil, err
	}
	return configuredCorpusEmbedder{adapter: adapter, identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}, nil
}

func (e configuredCorpusEmbedder) Identity() string { return e.identity }
func (e configuredCorpusEmbedder) Dim() int         { return e.dim }

func (e configuredCorpusEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vectors, err := e.adapter.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	for i, vec := range vectors {
		if len(vec) != e.dim {
			return nil, fmt.Errorf("embedding %d dim %d != configured dim %d", i, len(vec), e.dim)
		}
	}
	return vectors, nil
}

func boolFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func firstPositional(args []string, valueFlags map[string]bool) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if valueFlags[arg] {
				i++
			}
			continue
		}
		return arg
	}
	return ""
}
