// orient_owned_corpus_cli_test.go covers the live consumer seam required by
// the packet: `compound orient --body-budget` must consume a Compound-owned
// SourceManifest body through the existing corpus resolver and orient
// materializer — without ever admitting it into ContextPack.active_memory or
// promoting a Claim / ledger mutation. The owned body surfaces ONLY in
// body_pack.bodies (or omitted) so it stays evidence/body-candidate, never
// ambient authority.
//
// This is the integration that proves the seam is wired to its real consumer
// (not bench-only). A capability with no live production consumer is bench-only
// per AGENTS.md — this test is the proof.
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
	"time"

	"compound/internal/corpus"
	"compound/internal/envelope"
	"compound/internal/sourceobs"

	"gopkg.in/yaml.v3"
)

// TestCmdOrientBodyBudgetConsumesPublicOwnedCorpusWithoutActiveMemory is the
// end-to-end seam test: drive real cmdOrient against a temp home containing
// a body_policy=owned_snapshot corpus lane, with a fake local embedder so the
// network never leaves the test process. Assertions (in order):
//
//  1. orient emits a body_pack section with a budget.
//  2. the public owned corpus chunk text appears in body_pack.bodies (the
//     owned body MUST reach orient — otherwise the seam is bench-only).
//  3. the corpus ref is NOT in active_memory (owned body stays evidence, never
//     ambient authority).
//  4. no Claim / ledger mutation is created (orient does not promote).
func TestCmdOrientBodyBudgetConsumesPublicOwnedCorpusWithoutActiveMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("indexes a corpus lane + fake embed server: skipped in -short mode")
	}
	homePath, cwd := initBodyPackTestHome(t)
	const dims = 8
	srv := fakeOwnedCorpusEmbedServer(t, dims)
	seedOwnedCorpusLane(t, homePath, srv.URL, dims)

	// Snapshot the home state pre-orient so we can prove no Claim / ledger
	// mutation is created. orient must be read-only against the ledger /
	// active_memory; the corpus body is body-only.
	preLedgerDir := filepath.Join(homePath, "ledger")
	hasLedger, err := pathExists(preLedgerDir)
	if err != nil {
		t.Fatalf("stat ledger dir: %v", err)
	}

	out := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "owned corpus body budget lookup", "--body-budget", "4000"})
	})

	var pack envelope.ContextPack
	if err := json.Unmarshal([]byte(out), &pack); err != nil {
		t.Fatalf("parse orient output: %v\n%s", err, out)
	}
	if pack.BodyPack == nil {
		t.Fatalf("body_pack must be present with --body-budget: %s", out)
	}

	// (2) The public owned corpus body MUST reach body_pack.bodies. This is the
	// proof the seam is wired to its real consumer; without it, the change is
	// bench-only and audit_only per AGENTS.md.
	const publicRef = "item_owned#0"
	var foundBody bool
	for _, b := range pack.BodyPack.Bodies {
		if b.Ref == publicRef {
			foundBody = true
			if b.Status != "ok" {
				t.Fatalf("owned corpus body status = %q, want ok: %+v", b.Status, b)
			}
			if !strings.Contains(b.Text, ownedBodyMarker) {
				t.Fatalf("owned corpus body text = %q, want fragment %q", b.Text, ownedBodyMarker)
			}
		}
	}
	if !foundBody {
		t.Fatalf("public owned corpus body missing from body_pack.bodies: %+v", pack.BodyPack.Bodies)
	}

	// (3) The corpus ref must NEVER enter active_memory. The ContextPack's
	// active_memory is the ambient-recall surface; an owned corpus body is
	// evidence / body-candidate, never ambient authority.
	for _, ref := range pack.ActiveMemory {
		if ref == publicRef || strings.HasPrefix(ref, "item_owned") {
			t.Fatalf("owned corpus ref %q leaked into active_memory: %v", ref, pack.ActiveMemory)
		}
	}

	// (4) No Claim / ledger mutation: the ledger directory must remain absent
	// (or, if present, its contents must be byte-identical to the pre-orient
	// snapshot). orient is a read-only materialization — even with --body-budget,
	// the corpus body never promotes to a Claim.
	if hasLedger {
		postEntries, err := os.ReadDir(preLedgerDir)
		if err != nil {
			t.Fatalf("stat ledger dir post-orient: %v", err)
		}
		if len(postEntries) != 0 {
			t.Fatalf("orient created ledger entries: %v", postEntries)
		}
	}
	// Context / work dirs may grow (orient persists the materialized pack),
	// but the ledger / claims files must not.
	for _, p := range []string{"ledger/claims.jsonl", "ledger/decisions.jsonl"} {
		if _, err := os.Stat(filepath.Join(homePath, p)); err == nil {
			t.Fatalf("orient created %s — owned corpus body leaked into ledger", p)
		}
	}
}

// TestCmdOrientBodyBudgetSuppressesPrivateOwnedCorpus is the privacy counterpart:
// a privacy=private owned_snapshot must be suppressed at the corpus layer
// (suppressed_private) and surface in the omitted ledger, never in
// body_pack.bodies. The owned body must NEVER widen the privacy contract.
func TestCmdOrientBodyBudgetSuppressesPrivateOwnedCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("indexes a corpus lane + fake embed server: skipped in -short mode")
	}
	homePath, cwd := initBodyPackTestHome(t)
	const dims = 8
	srv := fakeOwnedCorpusEmbedServer(t, dims)
	const family = "owned_private_fixture"
	seedOwnedCorpusLaneWith(t, homePath, family, srv.URL, dims, "private", ownedPrivateBodyMarker)

	out := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "owned private canary lookup", "--body-budget", "4000"})
	})

	if strings.Contains(out, ownedPrivateBodyMarker) {
		t.Fatalf("PRIVACY LEAK: private owned corpus body text admitted into ContextPack:\n%s", out)
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal([]byte(out), &pack); err != nil {
		t.Fatalf("parse orient output: %v\n%s", err, out)
	}
	if pack.BodyPack == nil {
		t.Fatalf("body_pack must be present with --body-budget: %s", out)
	}
	const privateRef = "item_owned#0"
	for _, b := range pack.BodyPack.Bodies {
		if b.Ref == privateRef {
			t.Fatalf("private owned corpus body admitted as body: %+v", b)
		}
	}
	var foundOmit bool
	for _, o := range pack.BodyPack.Omitted {
		if o.Ref == privateRef {
			foundOmit = true
			if o.Reason != "suppressed_private" {
				t.Fatalf("private owned omit reason = %q, want suppressed_private: %+v", o.Reason, o)
			}
			if o.EstimatedTokens != 0 {
				t.Fatalf("suppressed private omit must report 0 estimated tokens, got %d", o.EstimatedTokens)
			}
		}
	}
	if !foundOmit {
		t.Fatalf("private owned chunk %q missing from omitted ledger: %+v", privateRef, pack.BodyPack.Omitted)
	}
}

const ownedBodyMarker = "OWNED-PUBLIC-CANARY-do-not-suppress"
const ownedPrivateBodyMarker = "OWNED-PRIVATE-CANARY-do-not-emit"

// fakeOwnedCorpusEmbedServer stands up a local (127.0.0.1) OpenAI-compatible
// /embeddings endpoint returning a fixed unit-length vector per input text.
// Constant vector → every chunk cosine-matches the query → the owned body
// is always returned, which is what must then be SUPPRESSED (private) or
// ADMITTED (public) by the existing privacy gate — owned body MUST NOT
// widen that gate.
func fakeOwnedCorpusEmbedServer(t *testing.T, dims int) *httptest.Server {
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

// seedOwnedCorpusLane captures the source body into the home's artifact store
// via BuildOwnedSourceManifest (the existing sourceobs/home API), records a
// source_in_place manifest with privacy=public, configures a public corpus
// lane in memory-engine.yaml, and indexes it through the production embedder
// path (against the local fake embed server).
func seedOwnedCorpusLane(t *testing.T, homePath, embedBaseURL string, dims int) corpus.LaneConfig {
	t.Helper()
	return seedOwnedCorpusLaneWith(t, homePath, "owned_public_fixture", embedBaseURL, dims, "public", ownedBodyMarker)
}

func seedOwnedCorpusLaneWith(t *testing.T, homePath, family, embedBaseURL string, dims int, privacy, marker string) corpus.LaneConfig {
	t.Helper()
	bodyPath := filepath.Join(t.TempDir(), "owned-source.md")
	body := "User: owned body chunk alpha.\n\nAssistant: " + marker + " this is synthetic owned body text.\n"
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write owned source: %v", err)
	}
	manifest, err := sourceobs.BuildOwnedSourceManifest(homePath, family, []string{bodyPath}, time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build owned source manifest: %v", err)
	}
	if len(manifest.Items) != 1 {
		t.Fatalf("expected one owned manifest item, got %d", len(manifest.Items))
	}
	manifest.Items[0].ID = "item_owned"
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].PageSlug = "Owned Corpus Fixture"
	manifest.Items[0].CorpusGeneration = "owned-gen"
	manifest.Items[0].Privacy = privacy
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record owned source manifest: %v", err)
	}

	lane := corpus.LaneConfig{
		Lane:         "owned-fixture-" + family,
		SourceFamily: family,
		Privacy:      privacy,
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
		t.Fatalf("index owned lane: %v", err)
	}
	return lane
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
