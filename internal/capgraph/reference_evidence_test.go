package capgraph

import (
	"compound/internal/envelope"
	"testing"
)

func TestHistoricalDeclarationDoesNotCountAsNonUse(t *testing.T) {
	home := t.TempDir()
	const ref = "claim://claim_historical_ambiguous"
	writeContextPack(t, home, "ctx_historical_ambiguous", []envelope.IncludedRef{{Ref: ref, SourceClass: "runtime", Reason: "recency", Confidence: 1, Status: "current"}})
	rec := testReceipt("rec_historical_ambiguous", "completed", true, []string{ref}, nil, nil)
	rec.IDs.ContextPackID = "ctx_historical_ambiguous"
	rec.Skills.UsedDeclared = true
	rec.Skills.ApplicationMethod = ""
	rec.Skills.Referenced = []string{ref}
	rec.Skills.ObservationMethod = "transcript_reference_scan"
	rec.Skills.ObservationQuality = "readable"
	writeReceipt(t, home, rec)
	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	got := findInjected(proj, ref)
	if got == nil || got.ReferencedCount != 1 || got.ConsumedCount != 0 || got.IgnoredCount != 0 {
		t.Fatalf("legacy declaration/reference promoted to use or non-use: %+v", got)
	}
}
