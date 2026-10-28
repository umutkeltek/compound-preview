package contract

import (
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// Falsifier (b) of the volatile-claim-expiry packet: a claim with
// visibility=volatile is promoted and recallable, but must NEVER enter ambient
// memory / the ContextPack active set — even when the breadth engine ranks it
// #1. Mirrors the landed volatile-repo-state ambient gate one block above the
// engine-selection test: the engine RANKS, the ledger GATES.
//
// Note the gate is a visibility WHITELIST (memoryVisibilityAllowedForOrient),
// so this test also pins that admitting "volatile" to mem.Visibilities must not
// admit it to the orient whitelist — the red state of this falsifier is exactly
// that one-line mistake.
func TestVolatileVisibilityClaimNeverEntersAmbient(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cands := []mem.Candidate{
		{ // control: operational sibling the engine also surfaces
			Entity: "vector-index", Predicate: "state", Object: "embeddings enable semantic retrieval over the corpus",
			Confidence: 0.9, Derivation: "deterministic", ClaimType: "status",
			Qualifiers:     map[string]string{"domain": "vector-index"},
			PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "ev_vb", SnippetHash: "sha256:vb",
			Visibility: "operational", ObservationDate: "2026-07-02T00:00:00Z",
		},
		{ // volatile: clocked, low-risk class, ranked #1 by the engine below
			Entity: "project:compound", Predicate: "build_status", Object: "lookup path build green",
			Confidence: 0.9, Derivation: "deterministic", ClaimType: "status",
			Qualifiers:     map[string]string{"domain": "build_status"},
			PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "ev_vv", SnippetHash: "sha256:vv",
			Visibility: "volatile", ExpiresAt: fixedNow.Add(365 * 24 * time.Hour).Format(time.RFC3339),
			ObservationDate: "2026-07-02T00:00:00Z",
		},
	}
	got, err := store.Capture(cands, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for i := range got {
		res, err := store.Adjudicate(got[i].ID, "promote", "test", fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if res.Claim != nil {
			ids[res.Claim.Entity] = res.Claim.ID
		}
	}
	idB, idV := ids["vector-index"], ids["project:compound"]
	if idB == "" || idV == "" {
		t.Fatalf("seed claims not promoted: ids=%v", ids)
	}

	fake := &fakeBreadthEngine{hits: []mem.Hit{{ClaimID: idV, Score: 0.99}, {ClaimID: idB, Score: 0.80}}}
	orig := breadthEngineFactory
	breadthEngineFactory = func(string) (mem.Engine, func()) { return fake, func() {} }
	t.Cleanup(func() { breadthEngineFactory = orig })
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "fake-sidecar")

	in := Input{Harness: "claude", Cwd: "/repo/compound", Task: "speed up the lookup path", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	refB := "claim://" + idB
	if !contains(pack.ActiveMemory, refB) {
		t.Fatalf("operational control claim %s not surfaced: active_memory=%v", refB, pack.ActiveMemory)
	}
	refV := "claim://" + idV
	if contains(pack.ActiveMemory, refV) || hasIncluded(pack, refV) {
		t.Fatalf("volatile claim %s leaked into ambient despite engine rank #1: active=%v", refV, pack.ActiveMemory)
	}
}
