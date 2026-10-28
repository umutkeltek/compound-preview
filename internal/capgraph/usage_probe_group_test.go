package capgraph

import (
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/envelope"
)

// TestBuildCapabilityUsageInjectedRefsCarryProbeGroup is the primary falsifier for
// work_8713c4206232 item 1: a ref selected through the probe-group path (Reason stamped
// "probe:<class>/<group>; ...") must carry that label as InjectedRefUsage.ProbeGroup,
// joined onto the SAME consumed/ignored verdict usage.go already computes.
func TestBuildCapabilityUsageInjectedRefsCarryProbeGroup(t *testing.T) {
	home := t.TempDir()
	const consumedRef = "claim://claim_probe_consumed"
	const ignoredRef = "claim://claim_probe_ignored"

	writeContextPack(t, home, "ctx_probe1", []envelope.IncludedRef{
		{
			Ref: consumedRef, SourceClass: "runtime",
			Reason:     "probe:system-work/standing-law; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-widget.state = shipped",
			Confidence: 1, Status: "current",
		},
		{
			Ref: ignoredRef, SourceClass: "runtime",
			Reason:     "probe:system-work/trap; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-gadget.state = archived",
			Confidence: 1, Status: "current",
		},
	})
	rec := testReceipt("rec_probe1", "completed", true, []string{consumedRef}, nil, nil)
	rec.Skills.UsedDeclared = true
	rec.Skills.ExplicitNonUse = []string{ignoredRef}
	rec.Skills.NonUseEvidence = "evidence:explicit-non-use"
	rec.Skills.NonUseMethod = "explicit_declaration"
	rec.Skills.NonUseActor = "operator:test"
	rec.ProofRefs = append(rec.ProofRefs, rec.Skills.NonUseEvidence)
	rec.EvidenceRefs = []string{"evidence:explicit-non-use"}
	rec.IDs.ContextPackID = "ctx_probe1"
	writeReceipt(t, home, rec)

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}

	consumed := findInjected(proj, consumedRef)
	if consumed == nil {
		t.Fatalf("consumed ref %q missing from projection", consumedRef)
	}
	if consumed.ProbeGroup != "system-work/standing-law" {
		t.Fatalf("consumed ref probe_group = %q, want %q", consumed.ProbeGroup, "system-work/standing-law")
	}
	if consumed.ConsumedCount != 1 || consumed.IgnoredCount != 0 {
		t.Fatalf("consumed ref counts = %+v, want consumed 1 ignored 0", consumed)
	}

	ignored := findInjected(proj, ignoredRef)
	if ignored == nil {
		t.Fatalf("ignored ref %q missing from projection", ignoredRef)
	}
	if ignored.ProbeGroup != "system-work/trap" {
		t.Fatalf("ignored ref probe_group = %q, want %q", ignored.ProbeGroup, "system-work/trap")
	}
	if ignored.ConsumedCount != 0 || ignored.IgnoredCount != 1 {
		t.Fatalf("ignored ref counts = %+v, want consumed 0 ignored 1", ignored)
	}
}

// TestBuildCapabilityUsageMalformedProbeLabelLeavesProbeGroupEmpty proves a malformed
// label (present "probe:" text that doesn't parse) never surfaces as an error and never
// populates ProbeGroup — it degrades to exactly the legacy/unlabeled shape.
func TestBuildCapabilityUsageMalformedProbeLabelLeavesProbeGroupEmpty(t *testing.T) {
	home := t.TempDir()
	const malformedRef = "claim://claim_probe_malformed"

	writeContextPack(t, home, "ctx_malformed", []envelope.IncludedRef{
		{
			Ref: malformedRef, SourceClass: "runtime",
			// "probe:" prefix present but no "/" class/group separator — malformed.
			Reason:     "probe:system-work; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-thing.state = value",
			Confidence: 1, Status: "current",
		},
	})
	rec := testReceipt("rec_malformed", "completed", true, nil, nil, nil)
	rec.Skills.UsedDeclared = true
	rec.Skills.ExplicitNonUse = []string{malformedRef}
	rec.Skills.NonUseEvidence = "evidence:explicit-non-use"
	rec.Skills.NonUseMethod = "explicit_declaration"
	rec.Skills.NonUseActor = "operator:test"
	rec.ProofRefs = append(rec.ProofRefs, rec.Skills.NonUseEvidence)
	rec.EvidenceRefs = []string{"evidence:explicit-non-use"}
	rec.IDs.ContextPackID = "ctx_malformed"
	writeReceipt(t, home, rec)

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatalf("build usage on malformed probe label: %v", err)
	}
	inj := findInjected(proj, malformedRef)
	if inj == nil {
		t.Fatalf("malformed-label ref %q missing from projection", malformedRef)
	}
	if inj.ProbeGroup != "" {
		t.Fatalf("malformed-label ref probe_group = %q, want empty (counted as unlabeled)", inj.ProbeGroup)
	}
	if inj.IgnoredCount != 1 {
		t.Fatalf("malformed-label ref ignored_count = %d, want 1 (attribution still joins normally)", inj.IgnoredCount)
	}
}

// TestBuildCapabilityUsageLegacyPacksOmitProbeGroup is the byte-identical falsifier for
// work_8713c4206232 item 3: a legacy pack (no probe-group label at all — the shape every
// pre-probe-group ContextPack has, and the shape a flat/non-probed selection still
// produces today) must never emit a "probe_group" key at all. This would red the moment
// the omitempty tag is dropped or a non-empty default sneaks in, which is exactly the
// byte-identical guarantee this slice must not break.
func TestBuildCapabilityUsageLegacyPacksOmitProbeGroup(t *testing.T) {
	home := t.TempDir()
	const legacyRef = "claim://claim_legacy"

	writeContextPack(t, home, "ctx_legacy", []envelope.IncludedRef{
		{Ref: legacyRef, SourceClass: "runtime", Reason: "recency", Confidence: 1, Status: "current"},
	})
	rec := testReceipt("rec_legacy", "completed", true, nil, nil, nil)
	rec.Skills.UsedDeclared = true
	rec.IDs.ContextPackID = "ctx_legacy"
	writeReceipt(t, home, rec)

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	legacy := findInjected(proj, legacyRef)
	if legacy == nil {
		t.Fatalf("legacy ref %q missing from projection", legacyRef)
	}
	if legacy.ProbeGroup != "" {
		t.Fatalf("legacy ref probe_group = %q, want empty", legacy.ProbeGroup)
	}
	b, err := json.Marshal(proj)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "probe_group") {
		t.Fatalf("legacy-pack projection JSON must never carry a probe_group key: %s", b)
	}
}
