package capgraph

import (
	"encoding/json"
	"testing"
	"time"

	"compound/internal/envelope"
)

func findProbeGroup(proj ProbeGroupUsageProjection, group string) *ProbeGroupUsage {
	for i := range proj.Groups {
		if proj.Groups[i].Group == group {
			return &proj.Groups[i]
		}
	}
	return nil
}

// TestBuildProbeGroupUsageAggregatesAcrossReceipts is the primary falsifier: labeled
// refs from multiple receipts must roll up into the correct per-group
// injected/consumed/ignored/unknown counts, and two DIFFERENT classes that happen to
// share a bare group name ("standing-law" under both "system-work" and "product-work")
// must never conflate — each "<class>/<group>" pair is its own group.
func TestBuildProbeGroupUsageAggregatesAcrossReceipts(t *testing.T) {
	home := t.TempDir()
	const sysStandingRef = "claim://claim_sys_standing"
	const prodStandingRef = "claim://claim_prod_standing"
	const undeclaredRef = "claim://claim_undeclared_unknown"

	writeContextPack(t, home, "ctx_g1", []envelope.IncludedRef{
		{
			Ref: sysStandingRef, SourceClass: "runtime",
			Reason:     "probe:system-work/standing-law; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-a.state = shipped",
			Confidence: 1, Status: "current",
		},
		{
			Ref: prodStandingRef, SourceClass: "runtime",
			Reason:     "probe:product-work/standing-law; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-b.state = shipped",
			Confidence: 1, Status: "current",
		},
	})
	declared := testReceipt("rec_g1", "completed", true, []string{sysStandingRef}, nil, nil)
	declared.Skills.UsedDeclared = true
	declared.Skills.ExplicitNonUse = []string{prodStandingRef}
	declared.Skills.NonUseEvidence = "evidence:explicit-non-use"
	declared.Skills.NonUseMethod = "explicit_declaration"
	declared.Skills.NonUseActor = "operator:test"
	declared.ProofRefs = append(declared.ProofRefs, declared.Skills.NonUseEvidence)
	declared.EvidenceRefs = []string{"evidence:explicit-non-use"}
	declared.IDs.ContextPackID = "ctx_g1"
	writeReceipt(t, home, declared)

	// A second receipt injects the SAME system-work/standing-law ref again but never
	// consumes it AND declares its used-list -> ignored increments once more.
	writeContextPack(t, home, "ctx_g2", []envelope.IncludedRef{
		{
			Ref: sysStandingRef, SourceClass: "runtime",
			Reason:     "probe:system-work/standing-law; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-a.state = shipped",
			Confidence: 1, Status: "current",
		},
	})
	declaredIgnore := testReceipt("rec_g2", "completed", true, nil, nil, nil)
	declaredIgnore.Skills.UsedDeclared = true
	declaredIgnore.Skills.ExplicitNonUse = []string{sysStandingRef}
	declaredIgnore.Skills.NonUseEvidence = "evidence:explicit-non-use"
	declaredIgnore.Skills.NonUseMethod = "explicit_declaration"
	declaredIgnore.Skills.NonUseActor = "operator:test"
	declaredIgnore.ProofRefs = append(declaredIgnore.ProofRefs, declaredIgnore.Skills.NonUseEvidence)
	declaredIgnore.EvidenceRefs = []string{"evidence:explicit-non-use"}
	declaredIgnore.IDs.ContextPackID = "ctx_g2"
	writeReceipt(t, home, declaredIgnore)

	// A third receipt injects an undeclared-unknown ref under a distinct group.
	writeContextPack(t, home, "ctx_g3", []envelope.IncludedRef{
		{
			Ref: undeclaredRef, SourceClass: "runtime",
			Reason:     "probe:general/opportunity; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-c.state = shipped",
			Confidence: 1, Status: "current",
		},
	})
	undeclared := testReceipt("rec_g3", "completed", true, nil, nil, nil)
	undeclared.IDs.ContextPackID = "ctx_g3"
	writeReceipt(t, home, undeclared)

	proj, err := BuildProbeGroupUsage(home, ProbeGroupUsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatalf("build probe group usage: %v", err)
	}

	sysGroup := findProbeGroup(proj, "system-work/standing-law")
	if sysGroup == nil {
		t.Fatalf("group %q missing from projection: %+v", "system-work/standing-law", proj.Groups)
	}
	if sysGroup.Injected != 2 || sysGroup.Consumed != 1 || sysGroup.Ignored != 1 || sysGroup.Unknown != 0 {
		t.Fatalf("system-work/standing-law = %+v, want injected 2 consumed 1 ignored 1 unknown 0", sysGroup)
	}

	prodGroup := findProbeGroup(proj, "product-work/standing-law")
	if prodGroup == nil {
		t.Fatalf("group %q missing from projection: %+v", "product-work/standing-law", proj.Groups)
	}
	// Same bare group name as sysGroup, different class: must NOT have absorbed sysGroup's counts.
	if prodGroup.Injected != 1 || prodGroup.Consumed != 0 || prodGroup.Ignored != 1 || prodGroup.Unknown != 0 {
		t.Fatalf("product-work/standing-law = %+v, want injected 1 consumed 0 ignored 1 unknown 0 (must not conflate with system-work/standing-law)", prodGroup)
	}

	generalGroup := findProbeGroup(proj, "general/opportunity")
	if generalGroup == nil {
		t.Fatalf("group %q missing from projection: %+v", "general/opportunity", proj.Groups)
	}
	if generalGroup.Injected != 1 || generalGroup.Consumed != 0 || generalGroup.Ignored != 0 || generalGroup.Unknown != 1 {
		t.Fatalf("general/opportunity = %+v, want injected 1 consumed 0 ignored 0 unknown 1 (undeclared receipt)", generalGroup)
	}

	if proj.ReceiptsScanned != 3 || proj.ReceiptsInWindow != 3 {
		t.Fatalf("receipts_scanned/in_window = %d/%d, want 3/3", proj.ReceiptsScanned, proj.ReceiptsInWindow)
	}
	if proj.ContextPacksScanned != 3 {
		t.Fatalf("context_packs_scanned = %d, want 3", proj.ContextPacksScanned)
	}

	// injected == consumed+ignored+unknown must hold for every emitted group.
	for _, g := range proj.Groups {
		if g.Injected != g.Consumed+g.Ignored+g.Unknown {
			t.Fatalf("group %q partition broken: %+v", g.Group, g)
		}
	}
}

// TestBuildProbeGroupUsageMalformedLabelExcludedFromGroups proves a malformed probe
// label never fabricates a group and never errors — the ref simply contributes to no
// group at all (usage.go's per-ref join already carries such a ref as unlabeled).
func TestBuildProbeGroupUsageMalformedLabelExcludedFromGroups(t *testing.T) {
	home := t.TempDir()
	const malformedRef = "claim://claim_probe_malformed_group"

	writeContextPack(t, home, "ctx_malformed", []envelope.IncludedRef{
		{
			Ref: malformedRef, SourceClass: "runtime",
			Reason:     "probe:system-work/; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional.state = value",
			Confidence: 1, Status: "current",
		},
	})
	rec := testReceipt("rec_malformed_group", "completed", true, nil, nil, nil)
	rec.Skills.UsedDeclared = true
	rec.IDs.ContextPackID = "ctx_malformed"
	writeReceipt(t, home, rec)

	proj, err := BuildProbeGroupUsage(home, ProbeGroupUsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatalf("build probe group usage on malformed label: %v", err)
	}
	if len(proj.Groups) != 0 {
		t.Fatalf("malformed-label projection groups = %+v, want none", proj.Groups)
	}
}

// TestBuildProbeGroupUsageLegacyHomeIsEmpty proves a home with no probe-group-labeled
// refs at all (every pack predates probe-group selection) returns a valid, empty
// projection — never an error.
func TestBuildProbeGroupUsageLegacyHomeIsEmpty(t *testing.T) {
	home := t.TempDir()
	const legacyRef = "claim://claim_legacy_group"

	writeContextPack(t, home, "ctx_legacy", []envelope.IncludedRef{
		{Ref: legacyRef, SourceClass: "runtime", Reason: "recency", Confidence: 1, Status: "current"},
	})
	rec := testReceipt("rec_legacy_group", "completed", true, []string{legacyRef}, nil, nil)
	rec.Skills.UsedDeclared = true
	rec.IDs.ContextPackID = "ctx_legacy"
	writeReceipt(t, home, rec)

	proj, err := BuildProbeGroupUsage(home, ProbeGroupUsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatalf("build probe group usage on legacy home: %v", err)
	}
	if len(proj.Groups) != 0 {
		t.Fatalf("legacy-home projection groups = %+v, want none", proj.Groups)
	}
	if proj.SchemaVersion != ProbeGroupUsageSchemaVersion {
		t.Fatalf("schema_version = %q, want %q", proj.SchemaVersion, ProbeGroupUsageSchemaVersion)
	}
}

// TestBuildProbeGroupUsageNoEventsIsEmpty proves a home with no events/ directory at all
// (a fresh, never-run home) returns a valid, empty projection rather than an error —
// mirroring BuildCapabilityUsage's identical fail-open contract.
func TestBuildProbeGroupUsageNoEventsIsEmpty(t *testing.T) {
	home := t.TempDir()
	proj, err := BuildProbeGroupUsage(home, ProbeGroupUsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatalf("build probe group usage on empty home: %v", err)
	}
	if len(proj.Groups) != 0 {
		t.Fatalf("empty-home projection groups = %+v, want none", proj.Groups)
	}
}

// TestBuildProbeGroupUsageDeterministic proves the sweep is deterministic: two rebuilds
// over the same home are byte-identical modulo generated_at. A map that leaked iteration
// order (unsorted groups) would red this.
func TestBuildProbeGroupUsageDeterministic(t *testing.T) {
	home := t.TempDir()
	writeContextPack(t, home, "ctx_det", []envelope.IncludedRef{
		{
			Ref: "claim://claim_zzz", SourceClass: "runtime",
			Reason: "probe:system-work/trap; Compound memory ledger claim: x", Confidence: 1, Status: "current",
		},
		{
			Ref: "claim://claim_aaa", SourceClass: "runtime",
			Reason: "probe:system-work/standing-law; Compound memory ledger claim: y", Confidence: 1, Status: "current",
		},
	})
	rec := testReceipt("rec_det", "completed", true, []string{"claim://claim_aaa"}, nil, nil)
	rec.Skills.UsedDeclared = true
	rec.IDs.ContextPackID = "ctx_det"
	writeReceipt(t, home, rec)

	a, err := BuildProbeGroupUsage(home, ProbeGroupUsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildProbeGroupUsage(home, ProbeGroupUsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("identical rebuild differs:\nA=%s\nB=%s", ab, bb)
	}

	c, err := BuildProbeGroupUsage(home, ProbeGroupUsageBuildOptions{GeneratedAt: t0.Add(72 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	a.GeneratedAt = ""
	c.GeneratedAt = ""
	an, _ := json.Marshal(a)
	cn, _ := json.Marshal(c)
	if string(an) != string(cn) {
		t.Fatalf("rebuild differs modulo generated_at:\nA=%s\nC=%s", an, cn)
	}
}

// TestWriteProbeGroupUsagePersists proves the write path round-trips under projections/.
func TestWriteProbeGroupUsagePersists(t *testing.T) {
	home := t.TempDir()
	proj, err := BuildProbeGroupUsage(home, ProbeGroupUsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteProbeGroupUsage(home, proj)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("write probe group usage returned empty path")
	}
}
