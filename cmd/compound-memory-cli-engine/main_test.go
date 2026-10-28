package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"compound/internal/mem"
)

type fakeMemoryCLI struct {
	output string
	err    error
	calls  int
	bins   []string
	args   [][]string
}

func (f *fakeMemoryCLI) run(_ context.Context, bin string, args []string) (string, error) {
	f.calls++
	f.bins = append(f.bins, bin)
	f.args = append(f.args, append([]string(nil), args...))
	return f.output, f.err
}

func TestMemoryCLIEngineUsesQueryOutputToRankIndexedClaims(t *testing.T) {
	fake := &fakeMemoryCLI{output: "Relevant memory says the API backend language is Go."}
	responses := exchange(t, fake, []mem.Request{
		{SchemaVersion: mem.ProtocolVersion, RequestID: "r1", Op: mem.OpCapabilities},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r2",
			Op:            mem.OpIndex,
			CorpusGen:     7,
			Claims: []mem.ClaimText{
				{ClaimID: "claim_go", Text: "project api backend_language go"},
				{ClaimID: "claim_swift", Text: "project api frontend_language swift"},
			},
		},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r3",
			Op:            mem.OpQuery,
			Text:          "which language should the api backend use",
			Gate:          mem.GateList,
			Allowlist:     []string{"claim_go", "claim_swift"},
			K:             1,
		},
	})
	if got := responses[0].Name; got != "memory-cli" {
		t.Fatalf("capability name = %q", got)
	}
	if responses[1].Indexed != 2 || responses[1].CorpusGen != 7 {
		t.Fatalf("index response = %+v", responses[1])
	}
	if len(responses[2].Hits) != 1 || responses[2].Hits[0].ClaimID != "claim_go" {
		t.Fatalf("query hits = %+v", responses[2].Hits)
	}
	if fake.calls != 1 {
		t.Fatalf("memory CLI calls = %d, want 1", fake.calls)
	}
	joined := strings.Join(fake.args[0], " ")
	if !strings.Contains(joined, "query") || !strings.Contains(joined, "--limit 8") {
		t.Fatalf("memory CLI args should use read-only query template: %v", fake.args[0])
	}
}

func TestMemoryCLIEngineGBrainProfileIsRetired(t *testing.T) {
	// gbrain is retired as a memory engine (2026-08-08): compound owns memory
	// via the ledger + corpus lanes. The legacy COMPOUND_MEMORY_CLI_PROFILE=gbrain
	// path that shelled to a gbrain binary must fail loudly and direct to
	// breadth, never silently run.
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "gbrain")
	fake := &fakeMemoryCLI{output: "unused"}
	var out bytes.Buffer
	err := run(strings.NewReader("{}\n"), &out, fake.run)
	if err == nil {
		t.Fatal("gbrain profile must fail closed, got no error")
	}
	if !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "breadth") {
		t.Fatalf("gbrain retirement error is not actionable: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("gbrain profile must not shell out, got %d external calls", fake.calls)
	}
}

func TestMemoryCLIEngineProfileAllowsExplicitOverrides(t *testing.T) {
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "memory-cli")
	t.Setenv("COMPOUND_MEMORY_CLI_BIN", "custom-brain")
	t.Setenv("COMPOUND_MEMORY_CLI_QUERY_ARGS", "search {query} --limit {limit}")
	t.Setenv("COMPOUND_MEMORY_CLI_ENGINE_EMBEDDER", "custom-embedder")
	fake := &fakeMemoryCLI{output: "Go backend"}
	responses := exchange(t, fake, []mem.Request{
		{SchemaVersion: mem.ProtocolVersion, RequestID: "r1", Op: mem.OpCapabilities},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r2",
			Op:            mem.OpIndex,
			CorpusGen:     1,
			Claims:        []mem.ClaimText{{ClaimID: "claim_go", Text: "backend go"}},
		},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r3",
			Op:            mem.OpQuery,
			Text:          "backend",
			Gate:          mem.GateList,
			Allowlist:     []string{"claim_go"},
			K:             1,
		},
	})
	if responses[0].Embedder != "custom-embedder" {
		t.Fatalf("embedder override = %q", responses[0].Embedder)
	}
	if fake.bins[0] != "custom-brain" {
		t.Fatalf("bin override = %v", fake.bins)
	}
	joined := strings.Join(fake.args[0], " ")
	if strings.Contains(joined, "autocut") || !strings.HasPrefix(joined, "search ") {
		t.Fatalf("query template override not honored: %q", joined)
	}
}

func TestMemoryCLIEngineRejectsUnknownProfile(t *testing.T) {
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "sync")
	var out bytes.Buffer
	err := run(strings.NewReader("{}\n"), &out, (&fakeMemoryCLI{}).run)
	if err == nil || !strings.Contains(err.Error(), "unknown COMPOUND_MEMORY_CLI_PROFILE") {
		t.Fatalf("unknown profile error = %v", err)
	}
}

func TestMemoryCLIEngineCanMatchExpansionOnlyTerms(t *testing.T) {
	fake := &fakeMemoryCLI{output: "Stored context names golangx."}
	responses := exchange(t, fake, []mem.Request{
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r1",
			Op:            mem.OpIndex,
			CorpusGen:     3,
			Claims: []mem.ClaimText{
				{ClaimID: "claim_go", Text: "project api langmarker golangx"},
				{ClaimID: "claim_swift", Text: "project api swiftmarker swift"},
			},
		},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r2",
			Op:            mem.OpQuery,
			Text:          "runtime choice",
			Gate:          mem.GateList,
			Allowlist:     []string{"claim_go", "claim_swift"},
			K:             10,
		},
	})
	if len(responses[1].Hits) != 1 || responses[1].Hits[0].ClaimID != "claim_go" {
		t.Fatalf("expansion-only hit = %+v, want claim_go", responses[1].Hits)
	}
	if fake.calls != 1 {
		t.Fatalf("memory CLI calls = %d, want 1", fake.calls)
	}
}

func TestMemoryCLIEnginePreservesDirectQueryHitBeforeExpansion(t *testing.T) {
	fake := &fakeMemoryCLI{output: "Expansion says beta gamma delta epsilon."}
	responses := exchange(t, fake, []mem.Request{
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r1",
			Op:            mem.OpIndex,
			CorpusGen:     4,
			Claims: []mem.ClaimText{
				{ClaimID: "claim_query", Text: "alpha original query owner"},
				{ClaimID: "claim_expansion", Text: "beta gamma delta epsilon expansion only"},
			},
		},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r2",
			Op:            mem.OpQuery,
			Text:          "alpha",
			Gate:          mem.GateList,
			Allowlist:     []string{"claim_query", "claim_expansion"},
			K:             1,
		},
	})
	if len(responses[1].Hits) != 1 || responses[1].Hits[0].ClaimID != "claim_query" {
		t.Fatalf("direct query hit should not be displaced by expansion: %+v", responses[1].Hits)
	}
}

func TestMemoryCLIEngineKeepsLexicalOrderAmongDirectQueryHits(t *testing.T) {
	fake := &fakeMemoryCLI{output: "Expansion says boost boost boost."}
	responses := exchange(t, fake, []mem.Request{
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r1",
			Op:            mem.OpIndex,
			CorpusGen:     5,
			Claims: []mem.ClaimText{
				{ClaimID: "claim_a", Text: "alpha plain"},
				{ClaimID: "claim_z", Text: "alpha boost"},
			},
		},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r2",
			Op:            mem.OpQuery,
			Text:          "alpha",
			Gate:          mem.GateList,
			Allowlist:     []string{"claim_a", "claim_z"},
			K:             1,
		},
	})
	if len(responses[1].Hits) != 1 || responses[1].Hits[0].ClaimID != "claim_a" {
		t.Fatalf("direct query hits should keep lexical reference order: %+v", responses[1].Hits)
	}
}

func TestMemoryCLIEngineGateListEmptyAllowlistFailsClosed(t *testing.T) {
	fake := &fakeMemoryCLI{output: "Go backend"}
	responses := exchange(t, fake, []mem.Request{
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r1",
			Op:            mem.OpIndex,
			CorpusGen:     1,
			Claims:        []mem.ClaimText{{ClaimID: "claim_go", Text: "backend go"}},
		},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r2",
			Op:            mem.OpQuery,
			Text:          "backend",
			Gate:          mem.GateList,
			Allowlist:     nil,
			K:             10,
		},
	})
	if len(responses[1].Hits) != 0 {
		t.Fatalf("empty allowlist must match nothing, got %+v", responses[1].Hits)
	}
	if fake.calls != 0 {
		t.Fatalf("empty allowlist should fail closed before memory CLI call, calls=%d", fake.calls)
	}
}

func TestMemoryCLIEngineReportsUnavailableBackend(t *testing.T) {
	fake := &fakeMemoryCLI{err: errors.New("db unavailable")}
	responses := exchange(t, fake, []mem.Request{
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r1",
			Op:            mem.OpIndex,
			CorpusGen:     1,
			Claims:        []mem.ClaimText{{ClaimID: "claim_go", Text: "backend go"}},
		},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r2",
			Op:            mem.OpQuery,
			Text:          "backend",
			Gate:          mem.GateList,
			Allowlist:     []string{"claim_go"},
			K:             10,
		},
	})
	if responses[1].OK {
		t.Fatalf("unavailable backend should not be OK: %+v", responses[1])
	}
	if responses[1].Error == nil || responses[1].Error.Code != mem.CodeEngineUnavailable {
		t.Fatalf("error = %+v, want %s", responses[1].Error, mem.CodeEngineUnavailable)
	}
}

func TestMemoryCLIEngineRejectsMutatingTemplateBeforeExternalCall(t *testing.T) {
	t.Setenv("COMPOUND_MEMORY_CLI_QUERY_ARGS", "sync --repo {query}")
	fake := &fakeMemoryCLI{output: "should not be called"}
	responses := exchange(t, fake, []mem.Request{
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r1",
			Op:            mem.OpIndex,
			CorpusGen:     1,
			Claims:        []mem.ClaimText{{ClaimID: "claim_go", Text: "backend go"}},
		},
		{
			SchemaVersion: mem.ProtocolVersion,
			RequestID:     "r2",
			Op:            mem.OpQuery,
			Text:          "backend",
			Gate:          mem.GateList,
			Allowlist:     []string{"claim_go"},
			K:             10,
		},
	})
	if responses[1].OK {
		t.Fatalf("mutating template should not be OK: %+v", responses[1])
	}
	if responses[1].Error == nil || responses[1].Error.Code != mem.CodeBadRequest {
		t.Fatalf("error = %+v, want %s", responses[1].Error, mem.CodeBadRequest)
	}
	if fake.calls != 0 {
		t.Fatalf("mutating template should be rejected before external call, calls=%d", fake.calls)
	}
}

func exchange(t *testing.T, fake *fakeMemoryCLI, requests []mem.Request) []mem.Response {
	t.Helper()
	var in bytes.Buffer
	enc := json.NewEncoder(&in)
	for _, req := range requests {
		if err := enc.Encode(req); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := run(&in, &out, fake.run); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&out)
	responses := make([]mem.Response, 0, len(requests))
	for {
		var resp mem.Response
		if err := dec.Decode(&resp); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		responses = append(responses, resp)
	}
	if len(responses) != len(requests) {
		t.Fatalf("responses = %d, want %d; raw=%s", len(responses), len(requests), out.String())
	}
	return responses
}
