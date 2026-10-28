// Command compound-memory-cli-engine is a read-only MemoryEngine sidecar over
// an existing local memory CLI. It speaks compound's owned breadth-engine
// protocol on stdin/stdout and returns only claim IDs that compound indexed for
// this run.
//
// The external process is rented expansion context, never authority: it does
// not receive promotion rights, visibility rights, or ledger write access.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"compound/internal/mem"
)

var errUnsafeQueryTemplate = errors.New("memory CLI query template must start with a read-only verb")

type cliRunner func(context.Context, string, []string) (string, error)

type sidecar struct {
	claims   []mem.ClaimText
	gen      int64
	ready    bool
	name     string
	embedder string
	bin      string
	template string
	runner   cliRunner
}

func main() {
	if err := run(os.Stdin, os.Stdout, execMemoryCLI); err != nil {
		fmt.Fprintln(os.Stderr, "compound-memory-cli-engine:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer, runner cliRunner) error {
	if runner == nil {
		runner = execMemoryCLI
	}
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	s := &sidecar{
		name:     cfg.Name,
		embedder: cfg.Embedder,
		bin:      cfg.Bin,
		template: cfg.QueryTemplate,
		runner:   runner,
	}
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	for {
		var req mem.Request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		resp := s.handle(req)
		if err := enc.Encode(&resp); err != nil {
			return err
		}
	}
}

func (s *sidecar) handle(req mem.Request) mem.Response {
	resp := mem.Response{
		RequestID:     req.RequestID,
		SchemaVersion: mem.ProtocolVersion,
	}
	if req.SchemaVersion != 0 && req.SchemaVersion != mem.ProtocolVersion {
		resp.OK = false
		resp.Error = &mem.WireError{Code: mem.CodeBadRequest, Message: "unsupported schema_version"}
		return resp
	}
	switch req.Op {
	case mem.OpCapabilities:
		resp.OK = true
		resp.Name = s.name
		resp.Breadth = true
		resp.Embedder = s.embedder
		resp.Ready = s.ready
		resp.CorpusGen = s.gen
	case mem.OpIndex:
		if s.ready && s.gen == req.CorpusGen {
			resp.OK = true
			resp.Indexed = 0
			resp.CorpusGen = s.gen
			return resp
		}
		s.claims = append([]mem.ClaimText(nil), req.Claims...)
		s.gen = req.CorpusGen
		s.ready = true
		resp.OK = true
		resp.Indexed = len(s.claims)
		resp.CorpusGen = s.gen
	case mem.OpQuery:
		if !s.ready {
			resp.OK = false
			resp.Error = &mem.WireError{Code: mem.CodeIndexMissing, Message: "index before query"}
			return resp
		}
		if req.Gate != mem.GateAll && len(req.Allowlist) == 0 {
			resp.OK = true
			resp.Hits = nil
			resp.CorpusGen = s.gen
			return resp
		}
		output, err := s.queryMemoryCLI(req.Text)
		if err != nil {
			code := mem.CodeEngineUnavailable
			if errors.Is(err, errUnsafeQueryTemplate) {
				code = mem.CodeBadRequest
			}
			resp.OK = false
			resp.Error = &mem.WireError{Code: code, Message: err.Error()}
			return resp
		}
		resp.OK = true
		resp.Hits = rankClaims(s.claims, req.Text, output, req.Gate, req.Allowlist, req.K)
		resp.CorpusGen = s.gen
	default:
		resp.OK = false
		resp.Error = &mem.WireError{Code: mem.CodeBadRequest, Message: "unknown op"}
	}
	return resp
}

func (s *sidecar) queryMemoryCLI(text string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout())
	defer cancel()
	args, err := buildQueryArgs(s.template, text)
	if err != nil {
		return "", err
	}
	return s.runner(ctx, s.bin, args)
}

func buildQueryArgs(template, text string) ([]string, error) {
	limit := envDefault("COMPOUND_MEMORY_CLI_LIMIT", "8")
	detail := envDefault("COMPOUND_MEMORY_CLI_DETAIL", "low")
	parts := strings.Fields(template)
	args := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "{query}":
			args = append(args, text)
		case "{limit}":
			args = append(args, limit)
		case "{detail}":
			args = append(args, detail)
		default:
			args = append(args, part)
		}
	}
	if !isReadOnlyQueryVerb(args) {
		return nil, fmt.Errorf("%w: %q", errUnsafeQueryTemplate, template)
	}
	return args, nil
}

type cliConfig struct {
	Name          string
	Bin           string
	QueryTemplate string
	Embedder      string
}

func configFromEnv() (cliConfig, error) {
	profile := strings.ToLower(envDefault("COMPOUND_MEMORY_CLI_PROFILE", "generic"))
	var cfg cliConfig
	switch profile {
	case "generic", "memory-cli":
		cfg = cliConfig{
			Name:          "memory-cli",
			Bin:           "memory-cli",
			QueryTemplate: "query {query} --limit {limit} --detail {detail}",
			Embedder:      "memory-cli",
		}
	case "gbrain":
		// Retired as a memory engine (2026-08-08): compound owns memory via the
		// ledger + corpus lanes. A config that still names gbrain is a stale
		// holdover and must fail loudly, never silently shell to a gbrain binary.
		return cliConfig{}, fmt.Errorf("memory engine profile %q is retired; compound owns memory — use %q (ledger + corpus lanes)", "gbrain", "breadth")
	default:
		return cliConfig{}, fmt.Errorf("unknown COMPOUND_MEMORY_CLI_PROFILE %q", profile)
	}
	cfg.Bin = envDefault("COMPOUND_MEMORY_CLI_BIN", cfg.Bin)
	cfg.QueryTemplate = envDefault("COMPOUND_MEMORY_CLI_QUERY_ARGS", cfg.QueryTemplate)
	cfg.Embedder = envDefault("COMPOUND_MEMORY_CLI_ENGINE_EMBEDDER", cfg.Embedder)
	return cfg, nil
}

func isReadOnlyQueryVerb(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "query", "search", "recall", "get":
		return true
	default:
		return false
	}
}

func execMemoryCLI(ctx context.Context, bin string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func rankClaims(claims []mem.ClaimText, query, expansion, gate string, allowlist []string, k int) []mem.Hit {
	allow := make(map[string]bool, len(allowlist))
	for _, id := range allowlist {
		allow[id] = true
	}
	queryTerms := strings.Fields(strings.ToLower(query))
	expansionWeights := termWeights(expansion, 1)
	if len(queryTerms) == 0 && len(expansionWeights) == 0 {
		return nil
	}
	type scoredHit struct {
		hit            mem.Hit
		expansionScore float64
	}
	direct := make([]mem.Hit, 0, len(claims))
	expansionOnly := make([]scoredHit, 0, len(claims))
	for _, claim := range claims {
		if gate != mem.GateAll && !allow[claim.ClaimID] {
			continue
		}
		haystack := strings.ToLower(claim.Text)
		var queryScore float64
		for _, term := range queryTerms {
			if strings.Contains(haystack, term) {
				queryScore++
			}
		}
		var expansionScore float64
		for term, weight := range expansionWeights {
			if strings.Contains(haystack, term) {
				expansionScore += float64(weight)
			}
		}
		if queryScore > 0 {
			direct = append(direct, mem.Hit{ClaimID: claim.ClaimID, Score: queryScore})
			continue
		}
		if expansionScore > 0 {
			expansionOnly = append(expansionOnly, scoredHit{
				hit:            mem.Hit{ClaimID: claim.ClaimID, Score: expansionScore},
				expansionScore: expansionScore,
			})
		}
	}
	sort.Slice(direct, func(i, j int) bool {
		return direct[i].Score > direct[j].Score
	})
	if k > 0 && len(direct) >= k {
		return direct[:k]
	}
	sort.Slice(expansionOnly, func(i, j int) bool {
		if expansionOnly[i].expansionScore != expansionOnly[j].expansionScore {
			return expansionOnly[i].expansionScore > expansionOnly[j].expansionScore
		}
		return expansionOnly[i].hit.ClaimID < expansionOnly[j].hit.ClaimID
	})
	out := append([]mem.Hit(nil), direct...)
	for _, hit := range expansionOnly {
		out = append(out, hit.hit)
	}
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

func termWeights(text string, weight int) map[string]int {
	out := map[string]int{}
	for _, term := range tokenize(text) {
		if len(term) < 2 {
			continue
		}
		out[term] = weight
	}
	return out
}

func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	return fields
}

func queryTimeout() time.Duration {
	ms := 5000
	if raw := strings.TrimSpace(os.Getenv("COMPOUND_MEMORY_CLI_TIMEOUT_MS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			ms = parsed
		}
	}
	return time.Duration(ms) * time.Millisecond
}

func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
