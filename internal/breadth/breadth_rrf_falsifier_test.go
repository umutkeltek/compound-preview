package breadth

import (
	"testing"

	"compound/internal/mem"
)

// TestSemanticBeatsPositiveCosineSubstringDecoy is the sharpened form of
// TestVectorSemanticBeatsSubstringDecoy, and the standing falsifier for any proposal to
// replace semantic-primary ranking with multi-lane rank fusion (RRF).
//
// Why the older guard is not enough: its decoy has cosine EXACTLY 0, so semanticRanking's
// `if semanticScore <= 0 { continue }` filter drops it before ranking runs. That guard
// therefore cannot observe a ranking regression at all. Real embedders essentially never
// return exactly 0 for a doc sharing a token with the query, so the case it protects is
// the one case that cannot occur in production. This test uses a small-but-POSITIVE
// cosine decoy, which survives the filter and actually reaches the ranker.
//
// What it falsifies (measured 2026-07-19): equal-weight RRF over the three lanes
// QueryWithLanes already computes (cosine, bm25, reference) ranks this decoy ABOVE the
// semantically-correct winner, reinstating the 2026-07-07 substring-flooding bug. The
// cause is structural, not a tuning miss:
//
//   - RRF scores by RANK, discarding magnitude. Cosine 1.0 and cosine 0.30 become ranks 1
//     and 2 — worth 1/61 and 1/62, a 1.6% difference. The semantic gap that actually
//     distinguishes the answer is destroyed by the rank conversion.
//   - Every extra lane the decoy fires on adds a full 1/61 — 100% of a top-rank vote.
//   - bm25 and reference are CORRELATED (reference is a substring subset of the lexical
//     signal), so a common-substring decoy collects two near-independent-looking votes for
//     what is really one piece of evidence, against the winner's one.
//
// Weighting does not rescue it. With cosine weight w, the winner scores w/61 and the decoy
// w/62 + 2/61; the decoy wins whenever 2/61 > w/3782, i.e. for all w < ~124 — at which
// point cosine so dominates that the other lanes are decorative and the design is just
// semantic-primary with extra steps. Verified empirically at w=1 and w=10.
//
// The upstream architecture this was stolen from pairs RRF with a cross-encoder reranker
// precisely to catch "shares vocabulary, answers a different question". Compound has no
// reranker, so RRF here strictly loses information rather than adding consensus. If hybrid
// ranking is revisited, the shape that can work is score NORMALIZATION plus a weighted sum
// (preserving magnitude) — the approach internal/contract's memoryRelevanceRank already
// takes — or adding a reranker stage first. Not rank fusion on its own.
func TestSemanticBeatsPositiveCosineSubstringDecoy(t *testing.T) {
	claims := []mem.ClaimText{
		{ClaimID: "winner", Entity: "dec", ClaimType: "decision", Text: "decision about chaining triggers"},
		{ClaimID: "decoy", Entity: "other", ClaimType: "fact", Text: "the immune system enforces placement"},
	}
	query := "system"
	embedder := newFakeEmbedder(2)
	embedder.vectors = map[string][]float32{
		query:                  {1, 0},
		wrapDocText(claims[0]): {1, 0},        // winner: semantically closest, shares no query token
		wrapDocText(claims[1]): {0.3, 0.9539}, // decoy: cosine 0.3 — weak but POSITIVE, survives the filter
	}
	eng, err := NewEngine(EngineConfig{Embedder: embedder, CompoundHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.IndexWithError(claims, 1); err != nil {
		t.Fatal(err)
	}

	// Preconditions: the decoy must be the hit on BOTH lexical lanes, and the winner on
	// neither. Without this the test would not exercise the fusion double-count at all.
	docs := docsFromClaims(claims)
	ref := referenceContains(query, docs)
	if len(ref) == 0 || ref[0].ClaimID != "decoy" {
		t.Fatalf("test needs the decoy to be the substring/reference hit; got %+v", ref)
	}
	bm := eng.bm25(query, docs)
	if len(bm) == 0 || bm[0].ClaimID != "decoy" {
		t.Fatalf("test needs the decoy to be the bm25 hit; got %+v", bm)
	}
	for _, h := range bm {
		if h.ClaimID == "winner" {
			t.Fatalf("test needs the winner to have no lexical hit; got %+v", bm)
		}
	}

	got := eng.Query(query, mem.GateList, []string{"winner", "decoy"}, 2)
	if len(got) < 1 {
		t.Fatalf("expected hits, got none")
	}
	if got[0].ClaimID != "winner" {
		t.Fatalf("substring decoy outranked the semantically-correct claim: got %+v", got)
	}
}
