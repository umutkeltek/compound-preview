package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/corpus"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/sourceobs"

	"gopkg.in/yaml.v3"
)

// initBodyPackTestHome sets COMPOUND_HOME to a fresh temp dir + a tiny synthetic
// repo cwd (so SourceRefPaths has files to read) and returns both. The cwd gets a
// minimal AGENTS.md + docs/architecture/universal-principles.md so source-ref body
// materialization has something to read without crossing into real-repo state.
func initBodyPackTestHome(t *testing.T) (homePath, cwd string) {
	t.Helper()
	homePath = t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	cwd = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "docs", "architecture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("# synthetic AGENTS\nbody materializer smoke fixture.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "docs", "architecture", "universal-principles.md"), []byte("# universal principles\nsynthetic tier-0 fixture.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return homePath, cwd
}

// CLI — `--body-budget` parse test: bad value → usage error (exit 2 convention).
// The parse function returns the error; the orient wrapper turns it into exit 2.
// Test the parse layer directly (no subprocess) to keep it deterministic + fast.
func TestParseBodyBudgetParseErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "non-integer", args: []string{"--body-budget", "abc"}},
		{name: "negative", args: []string{"--body-budget", "-1"}},
		{name: "float", args: []string{"--body-budget", "1.5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBodyBudget(tc.args); err == nil {
				t.Fatalf("parseBodyBudget(%v) should error", tc.args)
			}
		})
	}
	// Absent / 0 are both valid and mean "feature off".
	if n, err := parseBodyBudget(nil); err != nil || n != 0 {
		t.Fatalf("absent flag should parse to (0, nil), got (%d, %v)", n, err)
	}
	if n, err := parseBodyBudget([]string{"--body-budget", "0"}); err != nil || n != 0 {
		t.Fatalf("--body-budget 0 should parse to (0, nil), got (%d, %v)", n, err)
	}
	if n, err := parseBodyBudget([]string{"--body-budget", "2000"}); err != nil || n != 2000 {
		t.Fatalf("--body-budget 2000 should parse to (2000, nil), got (%d, %v)", n, err)
	}
}

// CLI smoke (F4 CLI half + the packet's "build / init / orient" verification step):
// with `--body-budget 2000`, `compound orient` emits a body_pack section with
// budget respected + an omitted ledger; with the flag ABSENT, no body_pack key.
func TestCmdOrientBodyBudgetSmoke(t *testing.T) {
	_, cwd := initBodyPackTestHome(t)

	// With --body-budget 2000: body_pack present, budget respected, omitted ledger
	// present (every candidate accounted for as either body or omission).
	withBudget := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "body-budget smoke", "--body-budget", "2000"})
	})
	var packWith envelope.ContextPack
	if err := json.Unmarshal([]byte(withBudget), &packWith); err != nil {
		t.Fatalf("parse orient output: %v\n%s", err, withBudget)
	}
	if packWith.BodyPack == nil {
		t.Fatalf("body_pack must be present with --body-budget 2000: %s", withBudget)
	}
	if packWith.BodyPack.BudgetTokens != 2000 {
		t.Fatalf("budget_tokens = %d, want 2000", packWith.BodyPack.BudgetTokens)
	}
	if packWith.BodyPack.EstimatedUsedTokens > packWith.BodyPack.BudgetTokens {
		t.Fatalf("used %d > budget %d (F1)", packWith.BodyPack.EstimatedUsedTokens, packWith.BodyPack.BudgetTokens)
	}
	if packWith.BodyPack.Bodies == nil {
		t.Fatalf("bodies slice must be present (non-nil) even when empty: %+v", packWith.BodyPack)
	}
	if packWith.BodyPack.Omitted == nil {
		t.Fatalf("omitted ledger must be present (non-nil): %+v", packWith.BodyPack)
	}
	// Body item shape: ref/source/rank/estimated_tokens/sha256/status/text all populated.
	for _, b := range packWith.BodyPack.Bodies {
		if b.Ref == "" || b.Source == "" || b.Status != "ok" || b.Text == "" || b.SHA256 == "" {
			t.Fatalf("body item shape incomplete: %+v", b)
		}
		if b.EstimatedTokens != (len(b.Text)+3)/4 {
			t.Fatalf("estimated_tokens %d != (len+3)/4 = %d for ref %s", b.EstimatedTokens, (len(b.Text)+3)/4, b.Ref)
		}
	}

	// Absent flag: NO body_pack key on the wire (compare the raw JSON, not just the
	// typed nil — both must hold).
	withoutBudget := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "no body budget"})
	})
	if strings.Contains(withoutBudget, `"body_pack"`) {
		t.Fatalf("absent --body-budget must not emit body_pack key: %s", withoutBudget)
	}
	var packWithout envelope.ContextPack
	if err := json.Unmarshal([]byte(withoutBudget), &packWithout); err != nil {
		t.Fatalf("parse orient output (no budget): %v\n%s", err, withoutBudget)
	}
	if packWithout.BodyPack != nil {
		t.Fatalf("BodyPack must be nil when --body-budget absent, got %+v", packWithout.BodyPack)
	}

	// Zero flag is identical to absent (feature off).
	zeroBudget := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "zero body budget", "--body-budget", "0"})
	})
	if strings.Contains(zeroBudget, `"body_pack"`) {
		t.Fatalf("--body-budget 0 must not emit body_pack key: %s", zeroBudget)
	}
}

// CLI smoke for F4 "semantically identical" half (excluded volatile header fields):
// orient with and without --body-budget produce packs that are byte-identical once
// the volatile header (id, created_at) and the additive body_pack key are excluded.
// Proves the flag is fully additive (no other field changes shape).
func TestCmdOrientBodyBudgetIsAdditive(t *testing.T) {
	homePath, cwd := initBodyPackTestHome(t)
	_ = homePath
	out1 := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "additive check A"})
	})
	out2 := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "additive check A", "--body-budget", "500"})
	})
	var p1, p2 map[string]any
	if err := json.Unmarshal([]byte(out1), &p1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out2), &p2); err != nil {
		t.Fatal(err)
	}
	// Strip volatile + additive keys before comparing.
	for _, m := range []map[string]any{p1, p2} {
		delete(m, "id")
		delete(m, "created_at")
		delete(m, "body_pack")
		// ids.contract_id is a content-addressed runtime snapshot reference and
		// therefore changes when the two CLI calls cross a wall-clock second.
		// Both runtime ids are orthogonal to whether body_pack is requested.
		if ids, ok := m["ids"].(map[string]any); ok {
			delete(ids, "contract_id")
			delete(ids, "context_pack_id")
		}
	}
	if len(p1) != len(p2) {
		t.Fatalf("additive: pack field count differs no-budget=%d with-budget=%d", len(p1), len(p2))
	}
	for k, v := range p1 {
		got, ok := p2[k]
		if !ok {
			t.Fatalf("additive: key %q missing from with-budget pack", k)
		}
		// Marshal-and-compare works for nested maps/slices without reflect.DeepEqual noise.
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Fatalf("additive: field %q differs\n no-budget=%s\n with-budget=%s", k, a, b)
		}
	}
}

// privateBodySecretMarker is a synthetic, unmistakable token planted inside the
// private corpus body. It must NEVER appear anywhere in orient's emitted
// ContextPack — its presence is a P0 privacy leak (private text admitted into a
// body that would flow to a model provider).
const privateBodySecretMarker = "SYNTHETIC-PRIVATE-BODY-CANARY-do-not-emit"

// fakeCorpusEmbedServer stands up a local (127.0.0.1) OpenAI-compatible
// /embeddings endpoint returning a fixed unit-length vector per input text. A
// private corpus lane requires a LOCAL embedder, and httptest binds 127.0.0.1, so
// this keeps the whole test hermetic (no network, no ollama) while still driving
// the real production embedder wiring (newCorpusEmbedder → HTTP). The constant
// vector makes every chunk cosine-match the query so the private hit is always
// returned — which is exactly what must then be SUPPRESSED, not admitted.
func fakeCorpusEmbedServer(t *testing.T, dims int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := len(req.Input)
		if n == 0 {
			n = 1
		}
		vec := make([]float64, dims)
		for i := range vec {
			vec[i] = 1
		}
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		out := struct {
			Data []datum `json:"data"`
		}{}
		for i := 0; i < n; i++ {
			out.Data = append(out.Data, datum{Index: i, Embedding: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedPrivateCorpusLane writes a private source body carrying the secret marker,
// records a source manifest with privacy=private, configures a private corpus
// lane in memory-engine.yaml, and indexes it through the production embedder path
// (against the local fake embed server). Returns the lane so the caller can name
// the private chunk id ("item_private#0").
func seedPrivateCorpusLane(t *testing.T, homePath, embedBaseURL string, dims int) corpus.LaneConfig {
	t.Helper()
	family := "corpus_private_fixture"
	bodyPath := filepath.Join(t.TempDir(), "private-source.md")
	body := "User: describe the private fixture.\n\nAssistant: " + privateBodySecretMarker + " this is synthetic private text.\n"
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write private source: %v", err)
	}
	manifest, err := sourceobs.BuildSourceManifest(family, []string{bodyPath}, cliTestNow)
	if err != nil {
		t.Fatalf("build source manifest: %v", err)
	}
	manifest.Items[0].ID = "item_private"
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].Privacy = "private"
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record source manifest: %v", err)
	}

	lane := corpus.LaneConfig{
		Lane:         "private-fixture",
		SourceFamily: family,
		Privacy:      "private",
		Embedder: corpus.EmbedderConfig{
			Provider: "local",
			Model:    "fake",
			Dims:     dims,
			Bits:     "f32",
			BaseURL:  embedBaseURL,
		},
		Chunk: corpus.ChunkConfig{Unit: "runes", Size: 512, Overlap: 0, Boundary: "structure"},
	}

	cfg := memoryEngineConfig{
		SchemaVersion: memoryEngineConfigSchemaVersion,
		CorpusLanes:   []corpus.LaneConfig{lane},
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal engine config: %v", err)
	}
	if err := os.WriteFile(memoryEngineConfigPath(homePath), raw, 0o644); err != nil {
		t.Fatalf("write engine config: %v", err)
	}

	emb, err := newCorpusEmbedder(lane)
	if err != nil {
		t.Fatalf("build corpus embedder: %v", err)
	}
	if _, err := corpus.Index(context.Background(), homePath, lane, emb, corpus.IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index private lane: %v", err)
	}
	return lane
}

// TestCmdOrientBodyBudgetSuppressesPrivateCorpusBody is the P0 privacy regression
// guard: `compound orient --body-budget` must NOT admit a private corpus body into
// the ContextPack without an explicit private-text grant. The corpus layer already
// suppresses private text by default (corpus.ResolveChunkText gates on includeText);
// the orient body resolver must therefore query with the privacy-safe default, not
// a blanket IncludeText opt-in.
//
// Falsifier: with the resolver passing QueryOptions{IncludeText: true} (the base
// defect), the private chunk's text is re-derived and packed into body_pack.bodies,
// so the secret marker appears in the emitted JSON and this test is RED.
func TestCmdOrientBodyBudgetSuppressesPrivateCorpusBody(t *testing.T) {
	if testing.Short() {
		t.Skip("indexes a corpus lane: skipped in -short mode")
	}
	homePath, cwd := initBodyPackTestHome(t)
	const dims = 8
	srv := fakeCorpusEmbedServer(t, dims)
	seedPrivateCorpusLane(t, homePath, srv.URL, dims)

	out := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "synthetic private canary lookup", "--body-budget", "4000"})
	})

	// Hardest assertion first: the secret must not appear ANYWHERE on the wire.
	if strings.Contains(out, privateBodySecretMarker) {
		t.Fatalf("PRIVACY LEAK: private corpus body text admitted into ContextPack:\n%s", out)
	}

	var pack envelope.ContextPack
	if err := json.Unmarshal([]byte(out), &pack); err != nil {
		t.Fatalf("parse orient output: %v\n%s", err, out)
	}
	if pack.BodyPack == nil {
		t.Fatalf("body_pack must be present with --body-budget: %s", out)
	}

	const privateRef = "item_private#0"

	// The private hit must never be an admitted body, and no body may carry the marker.
	for _, b := range pack.BodyPack.Bodies {
		if b.Ref == privateRef {
			t.Fatalf("private corpus chunk admitted as body: %+v", b)
		}
		if strings.Contains(b.Text, privateBodySecretMarker) {
			t.Fatalf("body carries private secret marker: %+v", b)
		}
	}

	// The private hit must be accounted for in the omitted ledger as suppressed_private,
	// with zero estimated tokens (no byte/token accounting can leak length either).
	var found bool
	for _, o := range pack.BodyPack.Omitted {
		if o.Ref != privateRef {
			continue
		}
		found = true
		if o.Reason != "suppressed_private" {
			t.Fatalf("private omission reason = %q, want suppressed_private: %+v", o.Reason, o)
		}
		if o.EstimatedTokens != 0 {
			t.Fatalf("suppressed private omission must report 0 estimated tokens, got %d", o.EstimatedTokens)
		}
	}
	if !found {
		t.Fatalf("private chunk %q missing from omitted ledger: %+v", privateRef, pack.BodyPack.Omitted)
	}

	// Public path intact: source-ref bodies (e.g. AGENTS.md) still materialize.
	var sawSourceRef bool
	for _, b := range pack.BodyPack.Bodies {
		if b.Source == "source-ref" && b.Status == "ok" && b.Text != "" {
			sawSourceRef = true
		}
	}
	if !sawSourceRef {
		t.Fatalf("public source-ref body must still materialize (fix must not break public path): %+v", pack.BodyPack)
	}
}

// CLI — the orient usage string now enumerates --body-budget. Pinned so a future
// flag-loop refactor that forgets to surface it reds immediately. Reads the source
// directly because the no---harness path calls os.Exit(2) (hard to capture inside
// the test process); the existing cmdHome usage tests use the same source-grep
// strategy where exec isn't appropriate.
func TestCmdOrientUsageEnumeratesBodyBudget(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(src), "usage: compound orient --harness <h> [--cwd <dir>] [--task <t>] [--repo-kind <k>] [--mode <m>] [--topic <topic-slug>] [--body-budget <estimated-tokens>]") {
		t.Fatalf("orient usage line in main.go must enumerate --body-budget")
	}
}
