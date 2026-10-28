package mem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBakeoffRunFixtureReportLedgerDigestAndDeterminism(t *testing.T) {
	s := newStore(t)
	seedBakeoffClaims(t, s)

	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.RunBakeoff(BakeoffRunOptions{
		QueriesPath:    filepath.Join("..", "..", "fixtures", "bakeoff", "queries.yaml"),
		K:              10,
		Candidate:      &LexicalEngine{},
		AllowExpansion: true,
		Now:            t0,
	})
	if err != nil {
		t.Fatalf("run bakeoff: %v", err)
	}
	after, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	if before != after || !report.LedgerDigestUnchanged || report.LedgerDigestBefore != before || report.LedgerDigestAfter != after {
		t.Fatalf("ledger digest changed: before=%s after=%s report=%+v", before, after, report)
	}
	if !report.Determinism.Passed || len(report.Determinism.Mismatches) != 0 {
		t.Fatalf("determinism should hold: %+v", report.Determinism)
	}
	if report.QueryCount != 3 || len(report.Queries) != 3 {
		t.Fatalf("query count = %d/%d", report.QueryCount, len(report.Queries))
	}
	if report.Metrics.RecallAt5 != 1 || report.Metrics.RecallAt10 != 1 || report.Metrics.MRR != 1 {
		t.Fatalf("unexpected recall metrics: %+v", report.Metrics)
	}
	if report.Metrics.ReferenceCovered != 1 {
		t.Fatalf("reference coverage = %f", report.Metrics.ReferenceCovered)
	}
	if report.Metrics.LatencyP95MS < report.Metrics.LatencyP50MS {
		t.Fatalf("latency percentiles out of order: %+v", report.Metrics)
	}
	if report.ReportPath == "" {
		t.Fatal("report path should be populated")
	}
	data, err := os.ReadFile(report.ReportPath)
	if err != nil {
		t.Fatalf("read report artifact: %v", err)
	}
	var artifact BakeoffReport
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("parse report artifact: %v\n%s", err, data)
	}
	if artifact.ReportPath != report.ReportPath || artifact.Metrics.RecallAt10 != 1 {
		t.Fatalf("artifact mismatch: %+v", artifact)
	}
}

func TestBakeoffRunFailsOnLedgerMutation(t *testing.T) {
	s := newStore(t)
	seedBakeoffClaims(t, s)

	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RunBakeoff(BakeoffRunOptions{
		QueriesPath:    filepath.Join("..", "..", "fixtures", "bakeoff", "queries.yaml"),
		K:              10,
		Candidate:      &mutatingBakeoffEngine{store: s},
		AllowExpansion: true,
		Now:            t0,
	})
	if err == nil {
		t.Fatal("expected mutation to fail bakeoff")
	}
	after, digestErr := s.LedgerDigest()
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	if before == after {
		t.Fatal("test engine did not mutate the ledger")
	}
}

func TestBakeoffRunFailsOnNondeterministicRankOrder(t *testing.T) {
	s := newStore(t)
	seedBakeoffClaims(t, s)

	report, err := s.RunBakeoff(BakeoffRunOptions{
		QueriesPath:    filepath.Join("..", "..", "fixtures", "bakeoff", "queries.yaml"),
		K:              10,
		Candidate:      &flippingBakeoffEngine{},
		AllowExpansion: true,
		Now:            t0,
	})
	if err == nil {
		t.Fatal("expected nondeterministic ranking to fail bakeoff")
	}
	if report.Determinism.Passed || len(report.Determinism.Mismatches) == 0 {
		t.Fatalf("determinism mismatch should be reported: %+v", report.Determinism)
	}
	if report.ReportPath == "" {
		t.Fatal("nondeterministic bakeoff should still write evidence")
	}
}

func TestBakeoffClinicClassifiesMissesAndRanksTopThreeCauses(t *testing.T) {
	claims := map[string]Claim{
		"private":  {ID: "private", Visibility: "private"},
		"volatile": {ID: "volatile", Visibility: "volatile"},
		"ranked":   {ID: "ranked", Entity: "e"},
		"twin":     {ID: "twin", Supersedes: "older"},
		"entity":   {ID: "entity"},
		"lane":     {ID: "lane", Entity: "e", ProvenanceLane: "semantic"},
		"gated":    {ID: "gated", Entity: "e"},
		"noise":    {ID: "noise", Entity: "e"},
		"alias":    {ID: "alias", Qualifiers: map[string]string{"clinic_cause": "synonym_alias_missing"}},
		"id":       {ID: "id", Qualifiers: map[string]string{"clinic_cause": "expected_id_wrong"}},
	}
	query := BakeoffQueryReport{Lane: "lexical", ReferenceClaimIDs: []string{"private", "volatile", "ranked", "twin", "entity", "lane", "noise", "alias", "id"}, CandidateRankedClaimIDs: append(make([]string, 10), "ranked")}
	want := map[string]string{
		"absent": "absent_claim", "private": "privacy_filter", "volatile": "stale_or_volatile_claim",
		"ranked": "rank_miss", "twin": "duplicate_twin", "entity": "wrong_entity",
		"lane": "semantic_lane_inactive", "gated": "held_by_review_gate", "noise": "noisy_claim_pollution",
		"alias": "synonym_alias_missing", "id": "expected_id_wrong",
	}
	for id, cause := range want {
		got := classifyBakeoffMiss(query, id, claims)
		if got.Cause != cause {
			t.Errorf("%s cause=%s want=%s", id, got.Cause, cause)
		}
		if got.Evidence == "" {
			t.Errorf("%s missing classification evidence", id)
		}
	}

	s := newStore(t)
	report := s.buildBakeoffClinic([]BakeoffQueryReport{{ID: "q", MissingExpectedClaimIDs: []string{"a", "b", "c", "d"}}})
	if len(report.TopCauses) != 1 || report.TopCauses[0].Cause != "absent_claim" || report.TopCauses[0].Count != 4 || len(report.TopCauses[0].EvidenceRefs) != 4 || len(report.RecommendedFixes) != 1 {
		t.Fatalf("clinic summary: %+v", report)
	}
}

func TestBakeoffClinicTopThreeAreMeasuredAndDeterministic(t *testing.T) {
	s := newStore(t)
	claims := []Claim{
		{ID: "gate-a", Seq: 1, Entity: "decision-a"},
		{ID: "gate-b", Seq: 2, Entity: "decision-b"},
		{ID: "twin-a", Seq: 3, Entity: "decision-c", Supersedes: "older-a"},
		{ID: "rank-a", Seq: 4, Entity: "product-a"},
	}
	for _, claim := range claims {
		if err := appendJSONL(s.claimsLog(), claim); err != nil {
			t.Fatal(err)
		}
	}
	ranked := append(make([]string, 10), "rank-a")
	report := s.buildBakeoffClinic([]BakeoffQueryReport{{
		ID: "live-shaped", Lane: "lexical",
		ReferenceClaimIDs:       []string{"twin-a", "rank-a"},
		CandidateRankedClaimIDs: ranked,
		MissingExpectedClaimIDs: []string{"gate-a", "gate-b", "twin-a", "rank-a", "absent-a"},
	}})
	want := []BakeoffClinicCause{
		{Cause: "held_by_review_gate", Count: 2, EvidenceRefs: []string{"live-shaped:gate-a", "live-shaped:gate-b"}},
		{Cause: "absent_claim", Count: 1, EvidenceRefs: []string{"live-shaped:absent-a"}},
		{Cause: "duplicate_twin", Count: 1, EvidenceRefs: []string{"live-shaped:twin-a"}},
	}
	if !reflect.DeepEqual(report.TopCauses, want) {
		t.Fatalf("top causes = %+v, want %+v", report.TopCauses, want)
	}
}

func TestBakeoffClinicShortRankingDoesNotPanic(t *testing.T) {
	claims := map[string]Claim{"present": {ID: "present", Entity: "project-a"}}
	got := classifyBakeoffMiss(BakeoffQueryReport{ReferenceClaimIDs: []string{"present"}}, "present", claims)
	if got.Cause != "noisy_claim_pollution" {
		t.Fatalf("short ranking cause = %s", got.Cause)
	}
}

func TestBakeoffClinicReviewGateIsPrimaryOverTwinShape(t *testing.T) {
	claims := map[string]Claim{"held-twin": {ID: "held-twin", Entity: "decision-a", Supersedes: "older"}}
	got := classifyBakeoffMiss(BakeoffQueryReport{}, "held-twin", claims)
	if got.Cause != "held_by_review_gate" {
		t.Fatalf("held twin cause = %s, want review gate as failed transition", got.Cause)
	}
}

func TestBakeoffClinicFindsExpectedIDReplacement(t *testing.T) {
	claims := map[string]Claim{
		"claim-current": {
			ID: "claim-current", Entity: "decisions",
			Qualifiers: map[string]string{"decision_id": "DEC-EXPECTED"},
		},
	}
	got := classifyBakeoffMiss(BakeoffQueryReport{}, "DEC-EXPECTED", claims)
	if got.Cause != "expected_id_wrong" || !strings.Contains(got.Evidence, "claim-current") {
		t.Fatalf("replacement classification = %+v", got)
	}
}

func seedBakeoffClaims(t *testing.T, s *Store) {
	t.Helper()
	claims := []Claim{
		{
			ID:         "claim_memory_spine",
			Seq:        1,
			Entity:     "system-memory-spine",
			Predicate:  "authority_owner",
			Object:     "compound ledger owns real memory truth and system stuff",
			ValidFrom:  t0.Format(time.RFC3339),
			Confidence: 0.95,
			Derivation: "deterministic",
			Visibility: "operational",
			CreatedAt:  t0.Format(time.RFC3339),
		},
		{
			ID:         "claim_reference_engine",
			Seq:        2,
			Entity:     "reference-engine",
			Predicate:  "baseline_check",
			Object:     "baseline checks engine answers with lexical reference coverage",
			ValidFrom:  t0.Format(time.RFC3339),
			Confidence: 0.95,
			Derivation: "deterministic",
			Visibility: "operational",
			CreatedAt:  t0.Format(time.RFC3339),
		},
		{
			ID:         "claim_no_mutation",
			Seq:        3,
			Entity:     "recall-bakeoff",
			Predicate:  "mutation_policy",
			Object:     "recall does not mess with real memory truth ledger digest unchanged",
			ValidFrom:  t0.Format(time.RFC3339),
			Confidence: 0.95,
			Derivation: "deterministic",
			Visibility: "operational",
			CreatedAt:  t0.Format(time.RFC3339),
		},
	}
	for _, claim := range claims {
		if err := appendJSONL(s.claimsLog(), claim); err != nil {
			t.Fatal(err)
		}
	}
}

type mutatingBakeoffEngine struct {
	store *Store
	base  LexicalEngine
}

func (e *mutatingBakeoffEngine) Capabilities() EngineCapabilities {
	caps := e.base.Capabilities()
	caps.Name = "mutating"
	caps.Breadth = true
	return caps
}

func (e *mutatingBakeoffEngine) Index(claims []Claim, gen int64) error {
	return e.base.Index(claims, gen)
}

func (e *mutatingBakeoffEngine) Query(text, gate string, allowlist []string, k int) ([]Hit, error) {
	_ = appendJSONL(e.store.claimsLog(), Claim{
		ID:         "claim_illegal_mutation",
		Seq:        99,
		Entity:     "illegal-mutation",
		Predicate:  "should_not_exist",
		Object:     "bakeoff query mutated the ledger",
		ValidFrom:  t0.Format(time.RFC3339),
		Confidence: 0.1,
		Derivation: "deterministic",
		Visibility: "operational",
		CreatedAt:  t0.Format(time.RFC3339),
	})
	return e.base.Query(text, gate, allowlist, k)
}

type flippingBakeoffEngine struct {
	claims []Claim
	gen    int64
	flip   bool
}

func (e *flippingBakeoffEngine) Capabilities() EngineCapabilities {
	return EngineCapabilities{Name: "flipping", Breadth: true, CorpusGen: e.gen}
}

func (e *flippingBakeoffEngine) Index(claims []Claim, gen int64) error {
	e.claims = append([]Claim(nil), claims...)
	e.gen = gen
	return nil
}

func (e *flippingBakeoffEngine) Query(_ string, _ string, allowlist []string, k int) ([]Hit, error) {
	allowed := map[string]bool{}
	for _, id := range allowlist {
		allowed[id] = true
	}
	var hits []Hit
	for _, c := range e.claims {
		if allowed[c.ID] {
			hits = append(hits, Hit{ClaimID: c.ID, Score: 1})
		}
	}
	e.flip = !e.flip
	if e.flip {
		for i, j := 0, len(hits)-1; i < j; i, j = i+1, j-1 {
			hits[i], hits[j] = hits[j], hits[i]
		}
	}
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}
