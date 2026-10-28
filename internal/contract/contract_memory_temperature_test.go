package contract

import (
	"math"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

const tempEpsilon = 1e-9

// contextualClaim is a claim that passes every rank gate (not a decision, visible for orient,
// not volatile-operational) so the temperature term is actually reached.
func contextualClaim(id, entity string) mem.Claim {
	return mem.Claim{ID: id, Entity: entity, Predicate: "fact", Object: "value", Visibility: "contextual"}
}

// Falsifier 3 (unit): equal base relevance, a consumed-warm claim outranks an ignored-cool one.
func TestMemoryRelevanceRankConsumedWarmOutranksIgnoredCool(t *testing.T) {
	entities := map[string]bool{memoryKey("compound"): true}
	warm := contextualClaim("warm", "compound")
	cool := contextualClaim("cool", "compound")
	temps := map[string]float64{"warm": 0.2, "cool": -0.2}

	rWarm := memoryRelevanceRank(warm, entities, nil, 0, temps)
	rCool := memoryRelevanceRank(cool, entities, nil, 0, temps)
	if !(rWarm > rCool) {
		t.Fatalf("warm(%v) must outrank cool(%v) at equal base relevance", rWarm, rCool)
	}
	// Both share base 2 (entity match); temperature is the only difference.
	if math.Abs(rWarm-2.2) > tempEpsilon || math.Abs(rCool-1.8) > tempEpsilon {
		t.Fatalf("warm=%v cool=%v, want 2.2 and 1.8 (base 2 ± temperature)", rWarm, rCool)
	}
}

func TestMemoryRelevanceRankCheapLaneRanksBelowEqualNonCheap(t *testing.T) {
	entities := map[string]bool{memoryKey("compound"): true}
	cheap := contextualClaim("cheap", "compound")
	cheap.ProvenanceLane = "cheap-lane"
	normal := contextualClaim("normal", "compound")

	cheapRank := memoryRelevanceRank(cheap, entities, nil, 0, nil)
	normalRank := memoryRelevanceRank(normal, entities, nil, 0, nil)
	if !(cheapRank < normalRank) {
		t.Fatalf("cheap-lane rank = %v, normal rank = %v; want cheap strictly lower", cheapRank, normalRank)
	}
}

func TestMemoryRelevanceRankCheapLanePenaltyNeverRaises(t *testing.T) {
	entities := map[string]bool{memoryKey("compound"): true}
	base := contextualClaim("base", "compound")
	cheap := contextualClaim("base", "compound")
	cheap.ProvenanceLane = "cheap-lane"
	temps := map[string]float64{"base": 0.2}

	withoutTag := memoryRelevanceRank(base, entities, nil, 0, temps)
	withTag := memoryRelevanceRank(cheap, entities, nil, 0, temps)
	if withTag > withoutTag {
		t.Fatalf("cheap-lane rank = %v, untagged rank = %v; penalty must be monotonic non-increasing", withTag, withoutTag)
	}
}

// Falsifier 4 (unit): a hot-but-irrelevant claim NEVER outranks a cold-but-entity-matched one,
// and temperature never manufactures relevance for an off-task claim (base 0 stays exactly 0).
func TestMemoryRelevanceRankHotIrrelevantNeverOutranksColdMatched(t *testing.T) {
	entities := map[string]bool{memoryKey("compound"): true}
	hotIrrelevant := contextualClaim("hot", "unrelated") // no entity/token match => base 0
	coldMatched := contextualClaim("cold", "compound")   // entity match => base 2
	temps := map[string]float64{"hot": mem.MaxTemperatureContribution, "cold": -mem.MaxTemperatureContribution}

	rHot := memoryRelevanceRank(hotIrrelevant, entities, nil, 0, temps)
	rCold := memoryRelevanceRank(coldMatched, entities, nil, 0, temps)
	if rHot != 0 {
		t.Fatalf("hot-but-irrelevant rank = %v, want exactly 0 (temperature must not manufacture relevance)", rHot)
	}
	if !(rHot < rCold) {
		t.Fatalf("hot-but-irrelevant(%v) must not outrank cold-but-entity-matched(%v)", rHot, rCold)
	}
}

// Boundedness/defense: a corrupt (out-of-range) stored temperature is clamped to the ceiling in
// the ranker, so temperature can never breach the bound and overwhelm relevance.
func TestMemoryRelevanceRankClampsTemperature(t *testing.T) {
	entities := map[string]bool{memoryKey("compound"): true}
	claim := contextualClaim("x", "compound") // base 2
	hi := memoryRelevanceRank(claim, entities, nil, 0, map[string]float64{"x": 999})
	lo := memoryRelevanceRank(claim, entities, nil, 0, map[string]float64{"x": -999})
	if math.Abs(hi-(2+mem.MaxTemperatureContribution)) > tempEpsilon {
		t.Fatalf("clamp high = %v, want %v", hi, 2+mem.MaxTemperatureContribution)
	}
	if math.Abs(lo-(2-mem.MaxTemperatureContribution)) > tempEpsilon {
		t.Fatalf("clamp low = %v, want %v", lo, 2-mem.MaxTemperatureContribution)
	}
}

// Falsifier 5 + 8 (unit): fail-open neutrality — a nil map, an empty map, and a map without this
// claim's id all leave the rank byte-identical to the pre-temperature base.
func TestMemoryRelevanceRankFailOpenNeutral(t *testing.T) {
	entities := map[string]bool{memoryKey("compound"): true}
	claim := contextualClaim("x", "compound") // base 2
	base := memoryRelevanceRank(claim, entities, nil, 0, nil)
	if math.Abs(base-2) > tempEpsilon {
		t.Fatalf("nil-temps base rank = %v, want 2", base)
	}
	if got := memoryRelevanceRank(claim, entities, nil, 0, map[string]float64{}); got != base {
		t.Fatalf("empty-temps rank = %v, want %v (identical)", got, base)
	}
	if got := memoryRelevanceRank(claim, entities, nil, 0, map[string]float64{"other": 0.25}); got != base {
		t.Fatalf("absent-id rank = %v, want %v (this claim untouched)", got, base)
	}
}

func seedTempClaim(t *testing.T, store *mem.Store, entity, predicate, object string) string {
	t.Helper()
	got, err := store.Capture([]mem.Candidate{{
		Entity:          entity,
		Predicate:       predicate,
		Object:          object,
		Confidence:      0.9,
		Derivation:      "deterministic",
		ClaimType:       "architecture",
		PromotionBasis:  candidates.BasisCuratedPage,
		SourceTraceRef:  "ev_" + predicate,
		SnippetHash:     "sha256:" + predicate,
		Visibility:      "contextual",
		ObservationDate: "2026-06-30T00:00:00Z",
	}}, fixedNow)
	if err != nil || len(got) != 1 {
		t.Fatalf("capture %s.%s: got %d err %v", entity, predicate, len(got), err)
	}
	res, err := store.Adjudicate(got[0].ID, "promote", "test", fixedNow)
	if err != nil || res.Claim == nil {
		t.Fatalf("promote %s.%s: %v", entity, predicate, err)
	}
	return res.Claim.ID
}

func writeTempProjection(t *testing.T, home string, entries []mem.TemperatureEntry) {
	t.Helper()
	if _, err := mem.WriteTemperature(home, mem.TemperatureProjection{
		SchemaVersion: mem.TemperatureSchemaVersion,
		ProjectionRef: mem.TemperatureProjectionRef,
		ComputedAt:    fixedNow.Format(time.RFC3339),
		Entries:       entries,
	}); err != nil {
		t.Fatal(err)
	}
}

// Falsifier 3 + 5 through the REAL bench path (protects the golden bench gate): with no
// projection, BenchMemoryRank ranks two equally-relevant claims by the deterministic seq/id
// tie-break; warming the lower-seq claim flips the order — proving the temperature lookup is
// threaded into BenchMemoryRank without changing its exported signature.
func TestBenchMemoryRankTemperatureReordersEqualRelevance(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// A seeded first (lower seq), B second (higher seq). Both entity-match "compound" equally.
	idA := seedTempClaim(t, store, "compound", "alpha_axis", "alpha stance detail")
	idB := seedTempClaim(t, store, "compound", "beta_axis", "beta stance detail")

	in := Input{Task: "compound", Harness: "codex", RepoKind: "control-plane"}
	pol := DefaultPolicy()

	// Baseline: no projection => fail-open. Equal rank => higher seq (B) first.
	baseline, _, available, err := BenchMemoryRank(home, in, pol)
	if err != nil {
		t.Fatal(err)
	}
	if !available || len(baseline) < 2 {
		t.Fatalf("baseline rank = %v available=%v, want both claims ranked", baseline, available)
	}
	if baseline[0] != idB {
		t.Fatalf("baseline first = %s, want higher-seq claim %s (fail-open tie-break)", baseline[0], idB)
	}

	// Warm the lower-seq claim A; it must now sort ahead of B.
	writeTempProjection(t, home, []mem.TemperatureEntry{{ClaimID: idA, Temperature: 0.2}})
	warmed, _, _, err := BenchMemoryRank(home, in, pol)
	if err != nil {
		t.Fatal(err)
	}
	if warmed[0] != idA {
		t.Fatalf("warmed first = %s, want the warmed claim %s", warmed[0], idA)
	}
}

// Falsifier 8 through the bench path: a NEUTRAL projection (all temperatures 0) leaves the
// ranked order byte-identical to the no-projection baseline — neutral temperatures never
// regress the held-out bench split.
func TestBenchMemoryRankNeutralTemperatureNoRegression(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	idA := seedTempClaim(t, store, "compound", "alpha_axis", "alpha stance detail")
	idB := seedTempClaim(t, store, "compound", "beta_axis", "beta stance detail")

	in := Input{Task: "compound", Harness: "codex", RepoKind: "control-plane"}
	pol := DefaultPolicy()

	baseline, _, _, err := BenchMemoryRank(home, in, pol)
	if err != nil {
		t.Fatal(err)
	}

	writeTempProjection(t, home, []mem.TemperatureEntry{
		{ClaimID: idA, Temperature: 0},
		{ClaimID: idB, Temperature: 0},
	})
	neutral, _, _, err := BenchMemoryRank(home, in, pol)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline) != len(neutral) {
		t.Fatalf("neutral length %d != baseline %d", len(neutral), len(baseline))
	}
	for i := range baseline {
		if baseline[i] != neutral[i] {
			t.Fatalf("neutral order %v != baseline %v (position %d)", neutral, baseline, i)
		}
	}
}
