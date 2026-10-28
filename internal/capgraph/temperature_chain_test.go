package capgraph

import (
	"testing"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/mem"
)

// seedCurrentClaim captures and promotes one claim, returning its (current) ledger id.
func seedCurrentClaim(t *testing.T, store *mem.Store, entity, object string) string {
	t.Helper()
	got, err := store.Capture([]mem.Candidate{{
		Entity:          entity,
		Predicate:       "state",
		Object:          object,
		Confidence:      0.9,
		Derivation:      "deterministic",
		ClaimType:       "status",
		Qualifiers:      map[string]string{"domain": entity},
		PromotionBasis:  candidates.BasisRuntimeProof,
		SourceTraceRef:  "ev_" + entity,
		SnippetHash:     "sha256:" + entity,
		Visibility:      "operational",
		ObservationDate: "2026-06-29T00:00:00Z",
	}}, t0)
	if err != nil {
		t.Fatalf("capture %s: %v", entity, err)
	}
	res, err := store.Adjudicate(got[0].ID, "promote", "test", t0)
	if err != nil {
		t.Fatalf("promote %s: %v", entity, err)
	}
	if res.Claim == nil {
		t.Fatalf("claim %s not promoted", entity)
	}
	return res.Claim.ID
}

func findTempEntry(proj mem.TemperatureProjection, claimID string) *mem.TemperatureEntry {
	for i := range proj.Entries {
		if proj.Entries[i].ClaimID == claimID {
			return &proj.Entries[i]
		}
	}
	return nil
}

// TestUsageTemperatureChainWarmCoolNeutral is the end-to-end anti-famine falsifier (§5): a seeded
// pack + fabricated receipts drive a usage rebuild, which drives a temperature rebuild, and the
// three attribution regimes land as distinct signals:
//   - WARM   : injected + consumed by a declared receipt  => temperature > 0
//   - COOL   : injected + unused by a DECLARED receipt     => temperature < 0
//   - NEUTRAL: injected + unused by an UNDECLARED receipt  => temperature == 0 (present, not cold)
//
// Before the honest-attribution fix the undeclared receipt cooled its injected ref (net -1), so the
// NEUTRAL claim would come back COOL — the exact famine the 10 all-cooling live entries showed.
func TestUsageTemperatureChainWarmCoolNeutral(t *testing.T) {
	home := t.TempDir()
	memStore, err := mem.Open(home)
	if err != nil {
		t.Fatalf("open mem store: %v", err)
	}
	warmID := seedCurrentClaim(t, memStore, "warm-claim", "the warm path was consumed")
	coolID := seedCurrentClaim(t, memStore, "cool-claim", "the cool path was declared-ignored")
	neutralID := seedCurrentClaim(t, memStore, "neutral-claim", "the neutral path was never declared")

	warmRef := "claim://" + warmID
	coolRef := "claim://" + coolID
	neutralRef := "claim://" + neutralID

	// Declared session-end receipt injects warm+cool and consumes ONLY warm.
	writeContextPack(t, home, "ctx_declared", []envelope.IncludedRef{
		{Ref: warmRef, SourceClass: "source", Reason: "recency", Confidence: 1, Status: "current"},
		{Ref: coolRef, SourceClass: "source", Reason: "recency", Confidence: 1, Status: "current"},
	})
	declared := testReceipt("rec_declared", "completed", true, []string{warmRef}, nil, nil)
	declared.Skills.UsedDeclared = true
	declared.Skills.ExplicitNonUse = []string{coolRef}
	declared.Skills.NonUseMethod = "explicit_declaration"
	declared.Skills.NonUseActor = "operator:test"
	declared.Skills.NonUseEvidence = "evidence:cool-claim-declaration"
	declared.ProofRefs = append(declared.ProofRefs, declared.Skills.NonUseEvidence)
	declared.EvidenceRefs = []string{"evidence:cool-claim-declaration"}
	declared.IDs.ContextPackID = "ctx_declared"
	writeReceipt(t, home, declared)

	// Legacy automatic receipt quoted neutral; its method is ambiguous, so it stays neutral.
	writeContextPack(t, home, "ctx_undeclared", []envelope.IncludedRef{
		{Ref: neutralRef, SourceClass: "source", Reason: "recency", Confidence: 1, Status: "current"},
	})
	undeclared := testReceipt("rec_undeclared", "completed", true, []string{neutralRef}, nil, nil)
	undeclared.Skills.ApplicationMethod = ""
	undeclared.Skills.UsedDeclared = true
	undeclared.Skills.Referenced = []string{neutralRef}
	undeclared.Skills.ObservationMethod = "transcript_reference_scan"
	undeclared.Skills.ObservationQuality = "readable"
	undeclared.IDs.ContextPackID = "ctx_undeclared"
	writeReceipt(t, home, undeclared)

	// usage rebuild -> persist, then temperature rebuild reads it as data.
	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatalf("build usage: %v", err)
	}
	if _, err := WriteCapabilityUsage(home, proj); err != nil {
		t.Fatalf("write usage: %v", err)
	}
	temp, err := mem.BuildTemperature(home, mem.TemperatureBuildOptions{Now: t0})
	if err != nil {
		t.Fatalf("build temperature: %v", err)
	}

	warm := findTempEntry(temp, warmID)
	cool := findTempEntry(temp, coolID)
	neutral := findTempEntry(temp, neutralID)

	if warm == nil || warm.Temperature <= 0 {
		t.Fatalf("consumed claim must be WARM (>0), got %+v", warm)
	}
	if cool == nil || cool.Temperature >= 0 {
		t.Fatalf("declared-ignored claim must be COOL (<0), got %+v", cool)
	}
	if neutral == nil || neutral.Temperature != 0 {
		t.Fatalf("undeclared claim must be NEUTRAL (==0, present not cold), got %+v", neutral)
	}
	// Guard the counts the temperature derived from, so a future attribution regression is legible.
	if warm.Consumed != 1 || warm.Ignored != 0 {
		t.Fatalf("warm counts = consumed %d ignored %d, want 1/0", warm.Consumed, warm.Ignored)
	}
	if cool.Consumed != 0 || cool.Ignored != 1 {
		t.Fatalf("cool counts = consumed %d ignored %d, want 0/1", cool.Consumed, cool.Ignored)
	}
	if neutral.Consumed != 0 || neutral.Ignored != 0 || neutral.Injected != 1 {
		t.Fatalf("neutral counts = injected %d consumed %d ignored %d, want 1/0/0", neutral.Injected, neutral.Consumed, neutral.Ignored)
	}
}
