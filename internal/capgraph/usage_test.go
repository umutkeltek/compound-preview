package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
)

// writeContextPack writes a valid ContextPack fixture under context/packs.
func writeContextPack(t *testing.T, home, id string, included []envelope.IncludedRef) {
	t.Helper()
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            id,
			CreatedAt:     t0.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound"},
			Authority:     envelope.Authority{SourceClass: "generated", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD:          "test pack",
		Included:           included,
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("context pack fixture invalid: %v", err)
	}
	b, err := json.MarshalIndent(pack, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "context", "packs", id+".json"), string(b))
}

func findCap(proj CapabilityUsageProjection, ref string) *CapabilityUsage {
	for i := range proj.Capabilities {
		if proj.Capabilities[i].Ref == ref {
			return &proj.Capabilities[i]
		}
	}
	return nil
}

func findInjected(proj CapabilityUsageProjection, ref string) *InjectedRefUsage {
	for i := range proj.InjectedRefs {
		if proj.InjectedRefs[i].Ref == ref {
			return &proj.InjectedRefs[i]
		}
	}
	return nil
}

func inSummary(entries []UsageSummaryEntry, ref string) bool {
	for _, e := range entries {
		if e.Ref == ref {
			return true
		}
	}
	return false
}

// TestBuildCapabilityUsageCountsUsedAndIgnored is the primary falsifier: a close
// receipt carrying a used skill ref must show used_count 1 for that ref, an offered
// ref never used must land in never_fired, and a ContextPack.included ref never
// echoed in a used ref must show as ignored (the reinforcement signal). Without the
// receipt→used and pack→injected joins these counts are all zero and this reds.
func TestBuildCapabilityUsageCountsUsedAndIgnored(t *testing.T) {
	home := t.TempDir()
	const usedRef = "capability://skill:diagnosing-bugs"
	const offeredOnlyRef = "capability://skill:tdd"
	const ignoredRef = "memory://m-never-touched"

	writeContextPack(t, home, "ctx_u1", []envelope.IncludedRef{
		{Ref: usedRef, SourceClass: "source", Reason: "match", Confidence: 1, Status: "current"},
		{Ref: ignoredRef, SourceClass: "source", Reason: "recency", Confidence: 1, Status: "current"},
	})

	rec := testReceipt("rec_u1", "completed", true, []string{usedRef}, nil, []string{usedRef, offeredOnlyRef})
	rec.Skills.CapabilitiesOffered = []string{usedRef, offeredOnlyRef}
	rec.Skills.UsedDeclared = true // historical declaration alone does not supply negative evidence
	rec.Skills.ExplicitNonUse = []string{ignoredRef}
	rec.Skills.NonUseEvidence = "evidence:explicit-non-use"
	rec.Skills.NonUseMethod = "explicit_declaration"
	rec.Skills.NonUseActor = "operator:test"
	rec.ProofRefs = append(rec.ProofRefs, rec.Skills.NonUseEvidence)
	rec.EvidenceRefs = []string{"evidence:explicit-non-use"}
	rec.IDs.ContextPackID = "ctx_u1"
	writeReceipt(t, home, rec)

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}

	used := findCap(proj, usedRef)
	if used == nil {
		t.Fatalf("used ref %q missing from projection", usedRef)
	}
	if used.UsedCount != 1 {
		t.Fatalf("used_count for %q = %d, want 1", usedRef, used.UsedCount)
	}
	if used.OfferedCount != 1 {
		t.Fatalf("offered_count for %q = %d, want 1", usedRef, used.OfferedCount)
	}
	if used.ValidatedCount != 0 || used.DeclaredUseCount != 1 || used.AcceptedUseDeclarationCount != 1 {
		t.Fatalf("receipt use strength for %q = %+v, want one accepted declaration, not validation", usedRef, used)
	}
	if len(used.SourceReceipts) != 1 || used.SourceReceipts[0] != "rec_u1" {
		t.Fatalf("source_receipts for %q = %v, want [rec_u1]", usedRef, used.SourceReceipts)
	}

	offered := findCap(proj, offeredOnlyRef)
	if offered == nil || offered.UsedCount != 0 || offered.OfferedCount != 1 {
		t.Fatalf("offered-only ref %q = %+v, want used 0 offered 1", offeredOnlyRef, offered)
	}
	if !inSummary(proj.Summary.NeverFired, offeredOnlyRef) {
		t.Fatalf("offered-but-unused ref %q missing from never_fired: %+v", offeredOnlyRef, proj.Summary.NeverFired)
	}

	consumed := findInjected(proj, usedRef)
	if consumed == nil || consumed.ConsumedCount != 1 || consumed.IgnoredCount != 0 {
		t.Fatalf("injected used ref %q = %+v, want consumed 1 ignored 0", usedRef, consumed)
	}
	ignored := findInjected(proj, ignoredRef)
	if ignored == nil || ignored.IgnoredCount != 1 || ignored.ConsumedCount != 0 {
		t.Fatalf("injected unused ref %q = %+v, want ignored 1 consumed 0", ignoredRef, ignored)
	}
	if !inSummary(proj.Summary.TopIgnored, ignoredRef) {
		t.Fatalf("ignored ref %q missing from top_ignored: %+v", ignoredRef, proj.Summary.TopIgnored)
	}
	if proj.ContextPacksScanned != 1 {
		t.Fatalf("context_packs_scanned = %d, want 1", proj.ContextPacksScanned)
	}
}

// TestBuildCapabilityUsageUndeclaredDoesNotInflateIgnored is the honest-attribution
// falsifier (§2): an injected ref that is NOT used counts as ignored ONLY when the receipt
// declared its used-list. An undeclared receipt (Skills.UsedDeclared=false — the shape of
// every ceremony close that omitted --used) must leave the injected ref at injected-only:
// consumed 0, ignored 0. On the PRE-CHANGE code (unconditional ignored++) the undeclared
// case reds because ignored would be 1. A declared twin of the same shape still cools.
func TestBuildCapabilityUsageUndeclaredDoesNotInflateIgnored(t *testing.T) {
	home := t.TempDir()
	const injectedRef = "claim://claim_famine"

	writeContextPack(t, home, "ctx_undeclared", []envelope.IncludedRef{
		{Ref: injectedRef, SourceClass: "source", Reason: "recency", Confidence: 1, Status: "current"},
	})
	writeContextPack(t, home, "ctx_declared", []envelope.IncludedRef{
		{Ref: injectedRef, SourceClass: "source", Reason: "recency", Confidence: 1, Status: "current"},
	})

	// Undeclared close: injected-but-unused, Skills.UsedDeclared stays false.
	undeclared := testReceipt("rec_undeclared", "completed", true, nil, nil, nil)
	undeclared.IDs.ContextPackID = "ctx_undeclared"
	writeReceipt(t, home, undeclared)

	// Declared close: same shape (injected-but-unused) but it DECLARED its used-list.
	declared := testReceipt("rec_declared", "completed", true, nil, nil, nil)
	declared.Skills.UsedDeclared = true
	declared.Skills.ExplicitNonUse = []string{injectedRef}
	declared.Skills.NonUseEvidence = "evidence:explicit-non-use"
	declared.Skills.NonUseMethod = "explicit_declaration"
	declared.Skills.NonUseActor = "operator:test"
	declared.ProofRefs = append(declared.ProofRefs, declared.Skills.NonUseEvidence)
	declared.EvidenceRefs = []string{"evidence:explicit-non-use"}
	declared.IDs.ContextPackID = "ctx_declared"
	writeReceipt(t, home, declared)

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}

	inj := findInjected(proj, injectedRef)
	if inj == nil {
		t.Fatalf("injected ref %q missing from projection", injectedRef)
	}
	// Two packs each inject it once => injected 2; neither used it => consumed 0.
	if inj.InjectedCount != 2 || inj.ConsumedCount != 0 {
		t.Fatalf("injected ref %q = %+v, want injected 2 consumed 0", injectedRef, inj)
	}
	// Only the DECLARED receipt may mark it ignored: exactly 1, not 2.
	if inj.IgnoredCount != 1 {
		t.Fatalf("ignored_count for %q = %d, want 1 (declared only; undeclared must not inflate)", injectedRef, inj.IgnoredCount)
	}
	// consumed==0 with ignored>0 => it appears in top_ignored — but only via the declared receipt.
	if !inSummary(proj.Summary.TopIgnored, injectedRef) {
		t.Fatalf("declared-ignored ref %q missing from top_ignored: %+v", injectedRef, proj.Summary.TopIgnored)
	}
}

// TestBuildCapabilityUsageDeterministic proves the sweep is deterministic: two
// rebuilds over the same home are byte-identical modulo generated_at. A map that
// leaked iteration order (unsorted capabilities/injected refs) would red this.
func TestBuildCapabilityUsageDeterministic(t *testing.T) {
	home := t.TempDir()
	writeContextPack(t, home, "ctx_d1", []envelope.IncludedRef{
		{Ref: "capability://skill:aaa", SourceClass: "source", Reason: "x", Confidence: 1, Status: "current"},
		{Ref: "capability://skill:zzz", SourceClass: "source", Reason: "x", Confidence: 1, Status: "current"},
		{Ref: "memory://m1", SourceClass: "source", Reason: "x", Confidence: 1, Status: "current"},
	})
	r1 := testReceipt("rec_d1", "completed", true, []string{"capability://skill:zzz", "capability://skill:aaa"}, nil, []string{"capability://skill:zzz", "capability://skill:aaa", "capability://skill:mmm"})
	r1.Skills.CapabilitiesOffered = []string{"capability://skill:zzz", "capability://skill:aaa", "capability://skill:mmm"}
	r1.IDs.ContextPackID = "ctx_d1"
	writeReceipt(t, home, r1)
	r2 := testReceipt("rec_d2", "failed", true, nil, []string{"capability://skill:mmm"}, nil)
	writeReceipt(t, home, r2)

	// Same generated_at => byte-identical output.
	a, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("identical rebuild differs:\nA=%s\nB=%s", ab, bb)
	}

	// Different generated_at => identical once generated_at is excluded.
	c, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0.Add(72 * time.Hour)})
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

// TestBuildCapabilityUsageSinceFilter proves --since excludes older receipts.
func TestBuildCapabilityUsageSinceFilter(t *testing.T) {
	home := t.TempDir()
	old := testReceipt("rec_old", "completed", true, []string{"capability://skill:old-one"}, nil, nil)
	old.CreatedAt = t0.Format(time.RFC3339)
	writeReceipt(t, home, old)
	recent := testReceipt("rec_new", "completed", true, []string{"capability://skill:new-one"}, nil, nil)
	recent.CreatedAt = t0.Add(48 * time.Hour).Format(time.RFC3339)
	writeReceipt(t, home, recent)

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{
		GeneratedAt: t0.Add(96 * time.Hour),
		Since:       t0.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if proj.ReceiptsScanned != 2 {
		t.Fatalf("receipts_scanned = %d, want 2", proj.ReceiptsScanned)
	}
	if proj.ReceiptsInWindow != 1 {
		t.Fatalf("receipts_in_window = %d, want 1", proj.ReceiptsInWindow)
	}
	if findCap(proj, "capability://skill:old-one") != nil {
		t.Fatal("pre-since receipt ref leaked into projection")
	}
	if findCap(proj, "capability://skill:new-one") == nil {
		t.Fatal("in-window receipt ref missing from projection")
	}
}

// TestBuildCapabilityUsageFailureCause proves the optional additive failure_cause
// field round-trips through the receipt writer into the projection aggregate.
func TestBuildCapabilityUsageFailureCause(t *testing.T) {
	home := t.TempDir()
	rec := testReceipt("rec_fc", "failed", true, nil, nil, nil)
	rec.FailureCause = &envelope.FailureCause{SurfaceOutcome: "blank_screen", Mechanism: "readiness_gate_never_fired"}
	writeReceipt(t, home, rec)

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(proj.FailureCauses) != 1 {
		t.Fatalf("failure_causes = %+v, want 1 entry", proj.FailureCauses)
	}
	fc := proj.FailureCauses[0]
	if fc.SurfaceOutcome != "blank_screen" || fc.Mechanism != "readiness_gate_never_fired" || fc.Count != 1 {
		t.Fatalf("failure_cause aggregate = %+v", fc)
	}
}

func writeUsageHookEvent(t *testing.T, home string, ev envelope.HookEvent) {
	t.Helper()
	if err := ev.Validate(); err != nil {
		t.Fatalf("hook event fixture invalid: %v", err)
	}
	body, err := json.MarshalIndent(ev, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "events", ev.ID+".json"), string(body))
}

func addUsagePackOffers(t *testing.T, home, packID string, offers []envelope.CapabilityOffer) {
	t.Helper()
	path := filepath.Join(home, "context", "packs", packID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal(data, &pack); err != nil {
		t.Fatal(err)
	}
	pack.CapabilityOffers = offers
	if err := pack.Validate(); err != nil {
		t.Fatalf("context pack with offers invalid: %v", err)
	}
	updated, err := json.MarshalIndent(pack, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(updated))
}

func TestBuildCapabilityUsageFoldsOffersAndSkillUsesFromHookEvents(t *testing.T) {
	home := t.TempDir()
	const (
		ref       = "capability://skill:diagnosing-bugs"
		packID    = "ctx_usage_hooks"
		sessionID = "sess_usage_hooks"
		workID    = "work_0123456789ab"
	)
	writeContextPack(t, home, packID, nil)
	addUsagePackOffers(t, home, packID, []envelope.CapabilityOffer{{
		Ref: ref, Kind: "skill", Name: "diagnosing-bugs", Score: 1.5,
		Why: "matched flaky regression",
	}})
	header := func(id, created string) envelope.Header {
		return envelope.Header{
			SchemaVersion: envelope.SVHookEvent,
			ID:            id,
			CreatedAt:     created,
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID, SessionID: sessionID, ContextPackID: packID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		}
	}
	writeUsageHookEvent(t, home, envelope.HookEvent{
		Header:  header("hook_usage_offer", t0.Format(time.RFC3339)),
		Harness: "claude_code", HookName: "SessionStart", HookCapability: "context_injection",
		SideEffects: []string{}, Status: "ok",
	})
	writeUsageHookEvent(t, home, envelope.HookEvent{
		Header:  header("hook_usage_fire", t0.Add(time.Second).Format(time.RFC3339)),
		Harness: "claude_code", HookName: "PostToolUse", HookCapability: "telemetry",
		SideEffects: []string{"Skill"}, Status: "ok",
		CapabilityUse: &envelope.CapabilityUse{
			Ref: ref, Skill: "diagnosing-bugs", Lane: "claude_code", Outcome: "used",
		},
	})
	rec := testReceipt("rec_usage_hooks", "completed", true, nil, nil, nil)
	rec.IDs.ContextPackID = packID
	rec.Skills.CapabilitiesOffered = []string{ref}
	writeReceipt(t, home, rec)

	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if proj.HookEventsScanned != 2 || proj.HookEventsInWindow != 2 {
		t.Fatalf("hook scan counts = %d/%d, want 2/2", proj.HookEventsScanned, proj.HookEventsInWindow)
	}
	capability := findCap(proj, ref)
	if capability == nil || capability.OfferedCount != 1 || capability.FiredCount != 1 {
		t.Fatalf("capability usage = %+v, want one deduplicated offer and one fire", capability)
	}
	if capability.LastFired != t0.Add(time.Second).Format(time.RFC3339) ||
		len(capability.SourceEvents) != 1 || capability.SourceEvents[0] != "hook_usage_fire" {
		t.Fatalf("capability fire evidence = %+v", capability)
	}
	if inSummary(proj.Summary.NeverFired, ref) {
		t.Fatalf("fired capability reported as never fired: %+v", proj.Summary.NeverFired)
	}
	if len(proj.Events) != 2 {
		t.Fatalf("usage events = %+v, want offered+used", proj.Events)
	}
	used := proj.Events[1]
	if used.Skill != "diagnosing-bugs" || used.Harness != "claude_code" || used.Lane != "claude_code" ||
		used.WorkID != workID || used.SessionID != sessionID || used.Trigger != "matched flaky regression" ||
		used.Outcome != "used" {
		t.Fatalf("used event lost attribution: %+v", used)
	}
}
