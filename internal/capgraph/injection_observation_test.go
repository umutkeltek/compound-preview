package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
)

// --- fixture helpers -------------------------------------------------------

// writeInjectionContextPack writes a valid ContextPack fixture with explicit
// active_memory / excluded content — the two fields ClassifyInjectionCondition reads.
// Kept local to this file (rather than extending the shared writeContextPack helper in
// usage_test.go) because those two fields are specific to this report.
func writeInjectionContextPack(t *testing.T, home, id string, activeMemory []string, excluded []envelope.ExcludedRef) {
	t.Helper()
	if excluded == nil {
		excluded = []envelope.ExcludedRef{}
	}
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
		Included:           []envelope.IncludedRef{},
		Excluded:           excluded,
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       activeMemory,
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

// injectionReceipt builds a valid receipt with an explicit context_pack_id, used-set,
// and used_declared flag — the shape sessionEnd close writes in production.
func injectionReceipt(id, workID, contextPackID string, createdAt time.Time, used []string, usedDeclared bool) envelope.Receipt {
	rec := testReceipt(id, "completed", true, used, nil, nil)
	rec.CreatedAt = createdAt.Format(time.RFC3339)
	rec.IDs.WorkID = workID
	rec.IDs.ContextPackID = contextPackID
	rec.Skills.UsedDeclared = usedDeclared
	return rec
}

// --- ClassifyInjectionCondition ---------------------------------------------------

func TestClassifyInjectionCondition(t *testing.T) {
	cases := []struct {
		name             string
		activeMemory     []string
		excluded         []envelope.ExcludedRef
		wantCondition    string
		wantServed       []string
		wantExcludedCand []string
	}{
		{
			name:          "on: active_memory carries a claim ref",
			activeMemory:  []string{"claim://claim_a", "claim://claim_b", "claim://claim_a"}, // dup on purpose
			wantCondition: InjectionConditionOn,
			wantServed:    []string{"claim://claim_a", "claim://claim_b"},
		},
		{
			name:             "reduced: active_memory empty, excluded carries a claim ref",
			activeMemory:     nil,
			excluded:         []envelope.ExcludedRef{{Ref: "claim://claim_c", Reason: "topic scope"}},
			wantCondition:    InjectionConditionReduced,
			wantExcludedCand: []string{"claim://claim_c"},
		},
		{
			name:          "off: neither active_memory nor a claim excluded ref",
			activeMemory:  nil,
			excluded:      []envelope.ExcludedRef{{Ref: "skill://diagnosing-bugs", Reason: "not relevant"}},
			wantCondition: InjectionConditionOff,
		},
		{
			name:          "off: no memory and no exclusions at all",
			wantCondition: InjectionConditionOff,
		},
		{
			name:             "on wins even when excluded also carries claim refs",
			activeMemory:     []string{"claim://claim_a"},
			excluded:         []envelope.ExcludedRef{{Ref: "claim://claim_z", Reason: "rank"}},
			wantCondition:    InjectionConditionOn,
			wantServed:       []string{"claim://claim_a"},
			wantExcludedCand: []string{"claim://claim_z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pack := envelope.ContextPack{ActiveMemory: tc.activeMemory, Excluded: tc.excluded}
			cond, served, excludedCand := ClassifyInjectionCondition(pack)
			if cond != tc.wantCondition {
				t.Fatalf("condition = %q, want %q", cond, tc.wantCondition)
			}
			if !stringSliceEqual(served, tc.wantServed) {
				t.Fatalf("served = %v, want %v", served, tc.wantServed)
			}
			if !stringSliceEqual(excludedCand, tc.wantExcludedCand) {
				t.Fatalf("excludedCandidates = %v, want %v", excludedCand, tc.wantExcludedCand)
			}
		})
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- BuildInjectionObservationReport ------------------------------------------------

// TestBuildInjectionObservationReportClassifiesAndJoinsConsumption is the primary
// falsifier: a session whose pack served claims it also used must classify ON with
// consumed_count > 0; a session that served claims but never used any must classify ON
// with consumed_count 0; a session whose pack only shows suppressed candidates must
// classify REDUCED; a session with no memory signal at all must classify OFF. Without the
// receipt->pack join and the per-session used-set intersection, these all collapse to the
// same (wrong) bucket.
func TestBuildInjectionObservationReportClassifiesAndJoinsConsumption(t *testing.T) {
	home := t.TempDir()

	// ON, consumed: pack served two claims, receipt used one of them.
	writeInjectionContextPack(t, home, "ctx_on_hot", []string{"claim://claim_hot", "claim://claim_cold"}, nil)
	writeReceipt(t, home, injectionReceipt("rec_on_hot", "work_on_hot", "ctx_on_hot", t0, []string{"claim://claim_hot"}, true))

	// ON, cold: pack served one claim, receipt used nothing.
	writeInjectionContextPack(t, home, "ctx_on_cold", []string{"claim://claim_cold"}, nil)
	writeReceipt(t, home, injectionReceipt("rec_on_cold", "work_on_cold", "ctx_on_cold", t0.Add(time.Minute), nil, true))

	// REDUCED: pack served nothing but excluded a claim candidate.
	writeInjectionContextPack(t, home, "ctx_reduced", nil, []envelope.ExcludedRef{{Ref: "claim://claim_gated", Reason: "topic scope"}})
	writeReceipt(t, home, injectionReceipt("rec_reduced", "work_reduced", "ctx_reduced", t0.Add(2*time.Minute), nil, false))

	// OFF: pack served nothing and excluded nothing memory-shaped.
	writeInjectionContextPack(t, home, "ctx_off", nil, []envelope.ExcludedRef{{Ref: "skill://tdd", Reason: "not relevant"}})
	writeReceipt(t, home, injectionReceipt("rec_off", "work_off", "ctx_off", t0.Add(3*time.Minute), nil, false))

	rep, err := BuildInjectionObservationReport(home, InjectionObservationOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Discriminator == "" {
		t.Fatal("report header must carry the condition discriminator")
	}
	if rep.ReceiptsScanned != 4 || rep.ReceiptsWithContextPack != 4 || rep.ContextPacksMissing != 0 {
		t.Fatalf("scan counters = scanned:%d withPack:%d missing:%d, want 4/4/0", rep.ReceiptsScanned, rep.ReceiptsWithContextPack, rep.ContextPacksMissing)
	}
	if len(rep.Sessions) != 4 {
		t.Fatalf("sessions = %d, want 4", len(rep.Sessions))
	}

	byReceipt := map[string]InjectionSessionRow{}
	for _, s := range rep.Sessions {
		byReceipt[s.ReceiptID] = s
	}
	hot := byReceipt["rec_on_hot"]
	if hot.Condition != InjectionConditionOn || hot.ServedCount != 2 || hot.ConsumedCount != 1 {
		t.Fatalf("rec_on_hot = %+v, want condition=on served=2 consumed=1", hot)
	}
	cold := byReceipt["rec_on_cold"]
	if cold.Condition != InjectionConditionOn || cold.ServedCount != 1 || cold.ConsumedCount != 0 {
		t.Fatalf("rec_on_cold = %+v, want condition=on served=1 consumed=0", cold)
	}
	reduced := byReceipt["rec_reduced"]
	if reduced.Condition != InjectionConditionReduced || reduced.ServedCount != 0 {
		t.Fatalf("rec_reduced = %+v, want condition=reduced served=0", reduced)
	}
	off := byReceipt["rec_off"]
	if off.Condition != InjectionConditionOff || off.ServedCount != 0 {
		t.Fatalf("rec_off = %+v, want condition=off served=0", off)
	}

	// Per-condition rollup for ON: 2 sessions, 3 served events, 2 distinct served
	// claims, 1 cold (claim_cold, served twice, consumed never), ratio 1/2.
	onSum := findConditionSummary(rep.Conditions, InjectionConditionOn)
	if onSum == nil {
		t.Fatal("missing ON condition summary")
	}
	if onSum.SessionCount != 2 || onSum.ServedClaimEvents != 3 || onSum.DistinctServedClaims != 2 {
		t.Fatalf("ON summary = %+v, want session_count=2 served_claim_events=3 distinct=2", onSum)
	}
	if onSum.ColdServedCount != 1 || onSum.ColdServedRatio != 0.5 {
		t.Fatalf("ON cold-served = count:%d ratio:%v, want 1/0.5", onSum.ColdServedCount, onSum.ColdServedRatio)
	}
	if onSum.ConsumedCountDistribution["0"] != 1 || onSum.ConsumedCountDistribution["1"] != 1 {
		t.Fatalf("ON consumed distribution = %+v, want {0:1,1:1,...}", onSum.ConsumedCountDistribution)
	}
	if len(onSum.HotClaims) != 1 || onSum.HotClaims[0].ClaimID != "claim_hot" || onSum.HotClaims[0].Count != 1 {
		t.Fatalf("ON hot claims = %+v, want [{claim_hot 1}]", onSum.HotClaims)
	}

	reducedSum := findConditionSummary(rep.Conditions, InjectionConditionReduced)
	if reducedSum == nil || reducedSum.SessionCount != 1 || reducedSum.DistinctServedClaims != 0 {
		t.Fatalf("REDUCED summary = %+v, want session_count=1 distinct_served=0", reducedSum)
	}
	offSum := findConditionSummary(rep.Conditions, InjectionConditionOff)
	if offSum == nil || offSum.SessionCount != 1 {
		t.Fatalf("OFF summary = %+v, want session_count=1", offSum)
	}

	// f2: ON sessions exist (2), one with nonzero consumption -> f2 does NOT fire.
	if rep.F2OnSessionCount != 2 {
		t.Fatalf("f2_on_session_count = %d, want 2", rep.F2OnSessionCount)
	}
	if rep.F2AllZero {
		t.Fatalf("f2_all_on_sessions_zero_consumed = true, want false (rec_on_hot consumed a claim)")
	}
}

// TestBuildInjectionObservationReportF2FiresWhenAllOnSessionsColdi is the packet's f2
// falsifier case: every ON-condition session shows consumed_count=0.
func TestBuildInjectionObservationReportF2FiresWhenAllOnSessionsCold(t *testing.T) {
	home := t.TempDir()
	writeInjectionContextPack(t, home, "ctx_1", []string{"claim://claim_a"}, nil)
	writeReceipt(t, home, injectionReceipt("rec_1", "work_1", "ctx_1", t0, nil, true))
	writeInjectionContextPack(t, home, "ctx_2", []string{"claim://claim_b"}, nil)
	writeReceipt(t, home, injectionReceipt("rec_2", "work_2", "ctx_2", t0.Add(time.Minute), nil, true))

	rep, err := BuildInjectionObservationReport(home, InjectionObservationOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.F2AllZero {
		t.Fatalf("f2_all_on_sessions_zero_consumed = false, want true (both ON sessions cold)")
	}
	if rep.F2OnSessionCount != 2 || rep.F2OnSessionsWithConsumption != 0 {
		t.Fatalf("f2 counts = on:%d withConsumption:%d, want 2/0", rep.F2OnSessionCount, rep.F2OnSessionsWithConsumption)
	}
	if rep.F2Note == "" {
		t.Fatal("f2_note must be populated")
	}
}

// TestBuildInjectionObservationReportF2VacuousWithNoOnSessions covers the zero-evidence
// edge: no ON-condition sessions observed at all. AllZero reports true (vacuously) but
// the note must say so explicitly rather than implying the falsifier fired on evidence.
func TestBuildInjectionObservationReportF2VacuousWithNoOnSessions(t *testing.T) {
	home := t.TempDir()
	writeInjectionContextPack(t, home, "ctx_off", nil, nil)
	writeReceipt(t, home, injectionReceipt("rec_off", "work_off", "ctx_off", t0, nil, false))

	rep, err := BuildInjectionObservationReport(home, InjectionObservationOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if rep.F2OnSessionCount != 0 {
		t.Fatalf("f2_on_session_count = %d, want 0", rep.F2OnSessionCount)
	}
	if !rep.F2AllZero {
		t.Fatal("f2_all_on_sessions_zero_consumed should be vacuously true with zero ON sessions")
	}
	if rep.F2Note != "no ON-condition sessions observed yet; f2 not yet falsifiable" {
		t.Fatalf("f2_note = %q, want the not-yet-falsifiable note", rep.F2Note)
	}
}

// TestBuildInjectionObservationReportSkipsReceiptsWithoutContextPack: a receipt with no
// context_pack_id (never entered ambient recall) must not appear as a session row.
func TestBuildInjectionObservationReportSkipsReceiptsWithoutContextPack(t *testing.T) {
	home := t.TempDir()
	rec := testReceipt("rec_no_pack", "completed", true, nil, nil, nil)
	writeReceipt(t, home, rec)

	rep, err := BuildInjectionObservationReport(home, InjectionObservationOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReceiptsScanned != 1 {
		t.Fatalf("receipts_scanned = %d, want 1", rep.ReceiptsScanned)
	}
	if rep.ReceiptsWithContextPack != 0 || len(rep.Sessions) != 0 {
		t.Fatalf("receipt without context_pack_id must not become a session row: %+v", rep)
	}
}

// TestBuildInjectionObservationReportEmptyHome: no events/ directory at all must be a
// clean empty report, not an error (mirrors BuildCapabilityUsage's fail-open contract).
func TestBuildInjectionObservationReportEmptyHome(t *testing.T) {
	home := t.TempDir()
	rep, err := BuildInjectionObservationReport(home, InjectionObservationOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sessions) != 0 || rep.ReceiptsScanned != 0 {
		t.Fatalf("empty home report = %+v, want zero-valued", rep)
	}
	if len(rep.Conditions) != 3 {
		t.Fatalf("conditions = %d, want all 3 buckets always emitted", len(rep.Conditions))
	}
}

func findConditionSummary(conditions []InjectionConditionSummary, condition string) *InjectionConditionSummary {
	for i := range conditions {
		if conditions[i].Condition == condition {
			return &conditions[i]
		}
	}
	return nil
}

// --- WriteInjectionObservationRun ---------------------------------------------------

func TestWriteInjectionObservationRunWritesUnderStateRunsOnly(t *testing.T) {
	home := t.TempDir()
	rep, err := BuildInjectionObservationReport(home, InjectionObservationOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteInjectionObservationRun(home, rep, t0)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(home, "state", "runs", "memory-injection-observation")
	if filepath.Dir(path) != wantDir {
		t.Fatalf("run artifact dir = %s, want %s", filepath.Dir(path), wantDir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("run artifact not written: %v", err)
	}
	var reread InjectionObservationReport
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &reread); err != nil {
		t.Fatal(err)
	}
	if reread.SchemaVersion != InjectionObservationSchemaVersion {
		t.Fatalf("round-tripped schema_version = %q, want %q", reread.SchemaVersion, InjectionObservationSchemaVersion)
	}
	// Read-only observer: must never touch the ledger or any projection.
	if _, err := os.Stat(filepath.Join(home, "projections")); err == nil {
		t.Fatal("WriteInjectionObservationRun must never create projections/")
	}
}

func TestInjectionObservationLegacyMentionIsNotConsumption(t *testing.T) {
	home := t.TempDir()
	const ref = "claim://claim_legacy_mention"
	writeInjectionContextPack(t, home, "ctx_legacy_mention", []string{ref}, nil)
	rec := injectionReceipt("rec_legacy_mention", "work_legacy_mention", "ctx_legacy_mention", t0, []string{ref}, true)
	rec.Skills.ApplicationMethod = ""
	writeReceipt(t, home, rec)
	report, err := BuildInjectionObservationReport(home, InjectionObservationOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 1 || report.Sessions[0].ConsumedCount != 0 {
		t.Fatalf("legacy automatic mention counted as consumption: %+v", report.Sessions)
	}
}
