package knowledge

// Expertise packs are model-synthesized profession knowledge imported as EVIDENCE,
// never as authority. These tests probe the two gates that keep that promise. They
// are written to FAIL if compound's depth were wrong — if a hallucinated expertise
// claim could be shaped into memory, or if a model-extracted claim could promote to
// canon without a governed basis. See docs/architecture/knowledge/expertise-packs.md.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
)

const expertisePackFixture = "expertise-ios-performance-export.json"

// TestExpertisePackImportsAsEvidenceNotAuthority proves the import boundary:
// every item lands as evidence + parked candidates, the synthesized source is
// registered at low trust, and NOTHING reaches the promoted claims ledger.
func TestExpertisePackImportsAsEvidenceNotAuthority(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ImportFile(homePath, store, fixturePath(expertisePackFixture), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Items != 14 || report.Imported != 14 {
		t.Fatalf("import report = %+v; want 14 items imported", report)
	}
	// Each technique item is parked as a no-promote source observation — reviewable,
	// but forbidden from becoming source truth on its own.
	if len(report.NoPromoteIDs) == 0 {
		t.Fatalf("expected no-promote decisions for technique items, got none: %+v", report)
	}
	// The synthesized source is registered, but at the default low trust posterior.
	regRows, err := loadSourceRegistry(filepath.Join(homePath, "registries", "knowledge-sources.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range regRows {
		if row.Kind == "expertise-synthesis" {
			found = true
			if row.TrustPosterior > 0.5 {
				t.Fatalf("expertise-synthesis source trust_posterior = %.2f; model synthesis must not be trusted authority", row.TrustPosterior)
			}
		}
	}
	if !found {
		t.Fatalf("expertise-synthesis source not registered: %+v", regRows)
	}
	// The promoted claims ledger must be empty: import never writes authority.
	if data, err := os.ReadFile(filepath.Join(homePath, "ledger", "claims.jsonl")); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("import wrote promoted claims; authority must be earned, not imported:\n%s", data)
	}
}

// TestExpertisePackGroundingGateRejectsHallucination is the depth probe for the
// extraction bridge. A synthesized-expertise essence line whose quote is NOT in the
// pack text must be rejected as a hallucination and produce no memory candidate. If
// this test ever goes green while asserting a candidate was created, the grounding
// gate has been bypassed and confabulated "expertise" can enter memory.
func TestExpertisePackGroundingGateRejectsHallucination(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ImportFile(homePath, store, fixturePath(expertisePackFixture), testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Item 0 is the frame-loop mental model; its extraction request is first.
	frameLoopKx := report.ExtractionIDs[0]

	fixtures := t.TempDir()
	// An essence line whose quote does NOT appear in the pack content.
	writeExpertiseEssence(t, fixtures, frameLoopKx, `{
  "essence_lines": [
    {"predicate_hint": "heuristic",
     "statement": "iOS schedules frames using a quantum blockchain consensus round.",
     "quote": "iOS schedules frames using a quantum blockchain consensus round"}
  ]
}`)

	before := candidateCount(t, store)
	_, err = Extract(homePath, store, ExtractOptions{ItemID: frameLoopKx, FixturesDir: fixtures}, testNow.Add(time.Minute))
	if err == nil {
		t.Fatalf("ungrounded expertise extraction must fail; got nil error (grounding gate bypassed)")
	}
	if after := candidateCount(t, store); after != before {
		t.Fatalf("ungrounded extraction created %d candidate(s); a hallucinated claim entered memory", after-before)
	}
	// The extraction request must be left in a retryable error state, not silently done.
	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", frameLoopKx+".json"), &req)
	if req.State != ExtractionStateError {
		t.Fatalf("rejected extraction state = %q, want %q", req.State, ExtractionStateError)
	}
}

// TestExpertisePackExtractedClaimCannotSelfPromote is the authority-gate probe. A
// grounded extraction candidate is faithful to the pack, but the pack is a
// low-trust model synthesis — so the candidate carries NO promotion_basis and must
// be refused promotion. Model-synthesized expertise cannot self-certify into canon;
// only a governed basis (curated_page, explicit_confirmation, decision_record,
// runtime_proof, trusted_deterministic_import) earns it.
func TestExpertisePackExtractedClaimCannotSelfPromote(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ImportFile(homePath, store, fixturePath(expertisePackFixture), testNow)
	if err != nil {
		t.Fatal(err)
	}
	frameLoopKx := report.ExtractionIDs[0]

	fixtures := t.TempDir()
	// A grounded essence line: the quote is copied verbatim from the frame-loop item.
	writeExpertiseEssence(t, fixtures, frameLoopKx, `{
  "essence_lines": [
    {"predicate_hint": "heuristic",
     "statement": "Treat iOS performance failures as hitches measured in milliseconds per frame, not binary dropped frames.",
     "quote": "The modern unit of failure is the hitch: a frame that misses its target and is shown late"}
  ]
}`)

	extractReport, err := Extract(homePath, store, ExtractOptions{ItemID: frameLoopKx, FixturesDir: fixtures}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("grounded extraction failed: %v", err)
	}
	if len(extractReport.CandidateIDs) != 1 {
		t.Fatalf("grounded extraction candidates = %v, want exactly 1", extractReport.CandidateIDs)
	}
	candID := extractReport.CandidateIDs[0]

	// The candidate must be faithful to the pack (grounded) but basis-less.
	cand := candidateByID(t, store, candID)
	if cand.PromotionBasis != "" {
		t.Fatalf("extracted expertise candidate carries promotion_basis %q; it must earn one, not import one", cand.PromotionBasis)
	}
	if !strings.Contains(cand.Entity, "ios-perf") {
		t.Fatalf("extracted candidate entity = %q, want an ios-perf-scoped entity", cand.Entity)
	}

	// The authority gate: promotion must be refused for lack of a governed basis.
	if _, err := store.Adjudicate(candID, "promote", "", testNow.Add(2*time.Minute)); err == nil {
		t.Fatalf("extracted expertise candidate promoted without a basis; model synthesis self-certified into canon")
	} else if !strings.Contains(err.Error(), "promotion_basis") {
		t.Fatalf("promotion refused for the wrong reason: %v", err)
	}
}

// TestExpertisePackGovernedPromotionReachesRecall proves the ALIVE path: once a
// reviewer curates a pack heuristic into a knowledge page (curated_page basis), it
// promotes to an immutable claim and a real consumer (recall) serves it.
func TestExpertisePackGovernedPromotionReachesRecall(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	// The pack must be imported first: the curated page is a REVIEW of pack evidence.
	if _, err := ImportFile(homePath, store, fixturePath(expertisePackFixture), testNow); err != nil {
		t.Fatal(err)
	}

	page := filepath.Join(t.TempDir(), "ios-perf-hitch-model.md")
	writeFile(t, page, `---
schema_version: compound.knowledge_page.v0.1
id: knowledge_ios_perf_hitch_model
title: iOS performance — the hitch mental model
entity: ios-performance
owner: expertise-review
visibility: contextual
claims:
  - entity: ios-performance
    predicate: hitch_mental_model
    object: "On iOS treat performance failures as hitches measured in milliseconds per frame at the device refresh rate, not binary dropped frames."
    confidence: 0.8
    derivation: inferred
    claim_type: architecture
    promotion_basis: curated_page
    topic_refs: ["ios", "performance"]
    evidence_quote: "The modern unit of failure is the hitch"
    qualifiers:
      source_pack: "expertise:ios-performance-engineering:v1"
      reviewed_by: operator
---
# iOS performance — the hitch mental model
Curated from the iOS-performance Expertise Pack after review.
`)

	pageReport, err := store.ImportKnowledgePage(page, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(pageReport.CandidateIDs) != 1 {
		t.Fatalf("curated page candidates = %v, want 1", pageReport.CandidateIDs)
	}
	res, err := store.Adjudicate(pageReport.CandidateIDs[0], "promote", "", testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("governed promotion failed: %v", err)
	}
	if res.Claim == nil {
		t.Fatalf("promote produced no claim: %+v", res)
	}
	recall, err := store.Recall("ios-performance", mem.RecallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recall.Claims) == 0 {
		t.Fatalf("recall served no claims for ios-performance after promotion")
	}
	var served bool
	for _, c := range recall.Claims {
		if c.Predicate == "hitch_mental_model" && strings.Contains(c.Object, "hitches measured in milliseconds") {
			served = true
		}
	}
	if !served {
		t.Fatalf("promoted heuristic not served by recall: %+v", recall.Claims)
	}
}

// TestAllExpertisePacksImportAsEvidenceOnly sweeps EVERY expertise pack fixture
// (current and future — the glob picks up new packs automatically) and holds each
// to the same boundary: every item imports as evidence with a parked no-promote
// candidate and a pending extraction, the source registers at low trust, and the
// promoted claims ledger stays empty. A new pack that is malformed, oversized for
// the grounding-quote bound, or somehow promoted on import fails here.
func TestAllExpertisePacksImportAsEvidenceOnly(t *testing.T) {
	packs, err := filepath.Glob(fixturePath("expertise-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) < 4 {
		t.Fatalf("expected at least 4 expertise pack fixtures, found %d: %v", len(packs), packs)
	}
	for _, pack := range packs {
		pack := pack
		t.Run(filepath.Base(pack), func(t *testing.T) {
			homePath := t.TempDir()
			store, err := mem.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			report, err := ImportFile(homePath, store, pack, testNow)
			if err != nil {
				t.Fatalf("import failed: %v", err)
			}
			if report.Items == 0 || report.Imported != report.Items {
				t.Fatalf("import report = %+v; want every item imported", report)
			}
			if len(report.NoPromoteIDs) != report.Items {
				t.Fatalf("no-promote decisions = %d, want one per item (%d)", len(report.NoPromoteIDs), report.Items)
			}
			if len(report.ExtractionIDs) != report.Items {
				t.Fatalf("pending extractions = %d, want one per item (%d)", len(report.ExtractionIDs), report.Items)
			}
			regRows, err := loadSourceRegistry(filepath.Join(homePath, "registries", "knowledge-sources.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range regRows {
				if row.Kind == "expertise-synthesis" && row.TrustPosterior > 0.5 {
					t.Fatalf("source %s trust_posterior = %.2f; model synthesis must stay low-trust", row.Ref, row.TrustPosterior)
				}
			}
			if data, err := os.ReadFile(filepath.Join(homePath, "ledger", "claims.jsonl")); err == nil && strings.TrimSpace(string(data)) != "" {
				t.Fatalf("import wrote promoted claims; authority must be earned, not imported:\n%s", data)
			}
		})
	}
}

// --- helpers ---

func writeExpertiseEssence(t *testing.T, dir, extractionID, body string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, extractionID+".json"), body)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func candidateCount(t *testing.T, store *mem.Store) int {
	t.Helper()
	cands, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	return len(cands)
}

func candidateByID(t *testing.T, store *mem.Store, id string) mem.Candidate {
	t.Helper()
	cands, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for i := range cands {
		if cands[i].ID == id {
			return cands[i]
		}
	}
	t.Fatalf("candidate %s not found", id)
	return mem.Candidate{}
}
