package mem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
)

// mustWriteCauseTransition hand-rolls a valid Cause-carrying Transition straight into the
// eventstore (bypassing capgraph entirely, per the packet's no-new-import-edges constraint for
// internal/mem) so tests can control caused_by_refs and timing precisely.
func mustWriteCauseTransition(t *testing.T, s *Store, id, workID, deltaCause string, causedByRefs []string, createdAt time.Time) envelope.Transition {
	t.Helper()
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            id,
			CreatedAt:     createdAt.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "cause_consumption_test"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  append([]string{}, causedByRefs...),
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		BeforeRefs: []string{},
		ActionRefs: []string{},
		AfterRefs:  []string{},
		Cause: &envelope.TransitionCause{
			DeltaCause:   deltaCause,
			CausedByRefs: causedByRefs,
		},
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("invalid fixture transition %s: %v", id, err)
	}
	if _, err := s.eventStore.PutJSONAtomic(tr.ID, &tr, "  ", eventstore.AtomicOptions{
		Write: func(f *os.File, data []byte) error {
			_, err := f.Write(data)
			return err
		},
	}); err != nil {
		t.Fatalf("write fixture transition %s: %v", id, err)
	}
	return tr
}

// mustWriteTransitionNoCause writes a valid Transition with Cause == nil — used to prove F2's
// "transitions with Cause == nil are not counted" toward TransitionsWithCause / ref buckets.
func mustWriteTransitionNoCause(t *testing.T, s *Store, id, workID string, createdAt time.Time) {
	t.Helper()
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            id,
			CreatedAt:     createdAt.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "cause_consumption_test"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		BeforeRefs: []string{},
		ActionRefs: []string{},
		AfterRefs:  []string{},
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("invalid fixture transition %s: %v", id, err)
	}
	if _, err := s.eventStore.PutJSONAtomic(tr.ID, &tr, "  ", eventstore.AtomicOptions{
		Write: func(f *os.File, data []byte) error {
			_, err := f.Write(data)
			return err
		},
	}); err != nil {
		t.Fatalf("write fixture transition %s: %v", id, err)
	}
}

// TestTierOfRefBucketsFiveWays is F1: each ref form buckets into exactly the tier D1 specifies.
func TestTierOfRefBucketsFiveWays(t *testing.T) {
	s := newStore(t)

	capturedLive, err := s.Capture([]Candidate{cand("entity:tier-live", "status", "live-claim-value", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	promotedLive, err := promoteForTest(t, s, capturedLive[0].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}

	capturedParked, err := s.Capture([]Candidate{cand("entity:tier-parked", "status", "parked-value", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(capturedParked[0].ID, "park", "", t0); err != nil {
		t.Fatal(err)
	}

	capturedRejected, err := s.Capture([]Candidate{cand("entity:tier-rejected", "status", "rejected-value", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(capturedRejected[0].ID, "reject", "", t0); err != nil {
		t.Fatal(err)
	}

	capturedNoPromote, err := s.Capture([]Candidate{cand("entity:tier-no-promote", "status", "no-promote-value", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(capturedNoPromote[0].ID, "no_promote", "", t0); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"claim:// live claim -> promoted", "claim://" + promotedLive.Claim.ID, CauseConsumptionTierPromoted},
		{"claim:// unknown id -> observed_raw", "claim://claim_never_existed", CauseConsumptionTierObservedRaw},
		{"bare cand_ parked -> pre_adjudication", capturedParked[0].ID, CauseConsumptionTierPreAdjudication},
		{"bare cand_ rejected -> closed", capturedRejected[0].ID, CauseConsumptionTierClosed},
		{"bare cand_ no_promote -> closed", capturedNoPromote[0].ID, CauseConsumptionTierClosed},
		{"unknown bare id -> observed_raw", "cand_totally_unknown_xyz", CauseConsumptionTierObservedRaw},
		{"commit: -> non_memory", "commit:abc123", CauseConsumptionTierNonMemory},
		{"work_ -> non_memory", "work_deadbeef1234", CauseConsumptionTierNonMemory},
		{"source: -> non_memory", "source:settings.json#hash", CauseConsumptionTierNonMemory},
		{"decision: -> non_memory", "decision:keep-the-gate", CauseConsumptionTierNonMemory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.TierOfRef(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("TierOfRef(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestBuildCauseConsumptionReportNeverLooksUpPathShapedCandidateRefs(t *testing.T) {
	homePath := t.TempDir()
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}

	outsideDir := filepath.Join(homePath, "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	external := Candidate{
		ID:        "cand_external",
		Entity:    "entity:external",
		Predicate: "status",
		Object:    "must-not-resolve",
		State:     "parked",
	}
	data, err := json.Marshal(&external)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "cand_external.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	pathRef := filepath.Join("..", "..", "outside", "cand_external")
	mustWriteCauseTransition(t, store, "tr_path_ref", "work_path_ref", "repair", []string{pathRef}, t0)
	rep, err := BuildCauseConsumptionReport(homePath, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.RefTierCounts[CauseConsumptionTierObservedRaw]; got != 1 {
		t.Fatalf("path-shaped ref observed_raw = %d, want 1; external candidate must never be read: %+v", got, rep.RefTierCounts)
	}
	if got := rep.RefTierCounts[CauseConsumptionTierPreAdjudication]; got != 0 {
		t.Fatalf("path-shaped ref resolved through candidate filesystem: pre_adjudication=%d", got)
	}
}

func TestBuildCauseConsumptionReportRejectsDamagedCandidateAuthority(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "malformed json", body: []byte("{not-json")},
		{name: "invalid state", body: []byte(`{"id":"cand_damaged","entity":"entity:damaged","predicate":"status","object":"bad","state":"future_state"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			store, err := Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			candidatePath := filepath.Join(homePath, candidates.InboxDir(candidates.Memory), "cand_damaged.json")
			if err := os.WriteFile(candidatePath, tt.body, 0o644); err != nil {
				t.Fatal(err)
			}
			mustWriteCauseTransition(t, store, "tr_damaged_candidate", "work_damaged_candidate", "repair", []string{"cand_damaged"}, t0)

			if rep, err := BuildCauseConsumptionReport(homePath, t0.Add(time.Minute)); err == nil {
				t.Fatalf("damaged candidate authority must not become observed_raw: %+v", rep.RefTierCounts)
			}
		})
	}
}

func TestBuildCauseConsumptionReportWithoutRecordIsPathAndByteReadOnly(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", t0); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"events", candidates.InboxDir(candidates.Memory)} {
		if err := os.RemoveAll(filepath.Join(homePath, rel)); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotCauseConsumptionTree(t, homePath)

	if _, err := BuildCauseConsumptionReport(homePath, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after := snapshotCauseConsumptionTree(t, homePath)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only report mutated home\nbefore=%v\nafter=%v", before, after)
	}
}

// TestBuildCauseConsumptionReportBucketsAndRatio is F2: one Cause-transition citing a claim ref
// + one citing an unknown ref buckets 1/1 and prints a thesis ratio; a Cause==nil transition
// must not be counted toward transitions_with_cause or any ref bucket.
func TestBuildCauseConsumptionReportBucketsAndRatio(t *testing.T) {
	homePath := t.TempDir()
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}

	captured, err := s.Capture([]Candidate{cand("entity:f2", "status", "claim-value", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := promoteForTest(t, s, captured[0].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Capture([]Candidate{cand("entity:f2-closed", "status", "closed-value", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(closed[0].ID, "reject", "", t0); err != nil {
		t.Fatal(err)
	}

	mustWriteCauseTransition(t, s, "tr_f2_promoted", "work_f2_a", "accept", []string{"claim://" + promoted.Claim.ID}, t0)
	mustWriteCauseTransition(t, s, "tr_f2_unknown", "work_f2_b", "repair", []string{"cand_never_existed_f2", closed[0].ID}, t0.Add(time.Minute))
	mustWriteTransitionNoCause(t, s, "tr_f2_no_cause", "work_f2_c", t0.Add(2*time.Minute))

	rep, err := BuildCauseConsumptionReport(homePath, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// TransitionsScanned counts every valid Transition the sweep finds: the 2 Cause-
	// carrying fixtures + the 1 Cause==nil fixture + the ledger's own promotion
	// transition minted by promoteForTest above (also Cause == nil) — proving the sweep
	// never silently drops a transition the eventstore actually holds.
	if rep.TransitionsScanned != 4 {
		t.Fatalf("transitions_scanned = %d, want 4 (2 cause fixtures + 1 no-cause fixture + 1 promotion transition)", rep.TransitionsScanned)
	}
	if rep.TransitionsWithCause != 2 {
		t.Fatalf("transitions_with_cause = %d, want 2 (Cause==nil transition must not count): %+v", rep.TransitionsWithCause, rep)
	}
	if rep.RefTierCounts[CauseConsumptionTierPromoted] != 1 || rep.RefTierCounts[CauseConsumptionTierObservedRaw] != 1 || rep.RefTierCounts[CauseConsumptionTierClosed] != 1 {
		t.Fatalf("bucket counts = %+v, want promoted=1 observed_raw=1 closed=1", rep.RefTierCounts)
	}
	if rep.MemoryShapedRefs != 2 || rep.UnpromotedRefs != 1 {
		t.Fatalf("memory_shaped_refs/unpromoted_refs = %d/%d, want 2/1 (closed remains reported but is excluded from the thesis denominator)", rep.MemoryShapedRefs, rep.UnpromotedRefs)
	}
	if rep.ThesisRatio != 0.5 {
		t.Fatalf("thesis ratio = %v, want 0.5", rep.ThesisRatio)
	}
	if strings.Contains(rep.ThesisNote, "at the time") || !strings.Contains(rep.ThesisNote, "currently resolve") {
		t.Fatalf("thesis note must describe present-state resolution honestly: %q", rep.ThesisNote)
	}
	if strings.TrimSpace(rep.CoverageNote) == "" {
		t.Fatal("D6 coverage honesty note must be printed")
	}
}

// TestBuildCauseConsumptionReportZeroTransitionsIsHonestZero is F4: zero Cause-transitions
// produces an honest zero report (no crash, no fabricated ratio).
func TestBuildCauseConsumptionReportZeroTransitionsIsHonestZero(t *testing.T) {
	homePath := t.TempDir()
	if _, err := Open(homePath); err != nil {
		t.Fatal(err)
	}
	rep, err := BuildCauseConsumptionReport(homePath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.TransitionsScanned != 0 || rep.TransitionsWithCause != 0 {
		t.Fatalf("expected zero scan, got %+v", rep)
	}
	for _, tier := range causeConsumptionTiers {
		if rep.RefTierCounts[tier] != 0 {
			t.Fatalf("tier %s = %d, want 0", tier, rep.RefTierCounts[tier])
		}
	}
	if rep.MemoryShapedRefs != 0 || rep.UnpromotedRefs != 0 || rep.ThesisRatio != 0 {
		t.Fatalf("expected zero ratio fields, got %+v", rep)
	}
	if strings.TrimSpace(rep.ThesisNote) == "" {
		t.Fatal("even a zero report must explain itself")
	}
}

func TestBuildCauseConsumptionReportLoadsClaimsOncePerReport(t *testing.T) {
	homePath := t.TempDir()
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := s.Capture([]Candidate{cand("entity:claim-load-shape", "status", "live", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := promoteForTest(t, s, captured[0].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteCauseTransition(t, s, "tr_claim_load_shape", "work_claim_load_shape", "repair", []string{
		"claim://" + promoted.Claim.ID,
		"claim://claim_missing_one",
		"claim://claim_missing_two",
	}, t0.Add(time.Minute))

	previousLoader := loadCauseConsumptionClaims
	loads := 0
	loadCauseConsumptionClaims = func(store *Store) ([]Claim, error) {
		loads++
		return previousLoader(store)
	}
	t.Cleanup(func() { loadCauseConsumptionClaims = previousLoader })

	if _, err := BuildCauseConsumptionReport(homePath, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("claim log loaded %d times, want exactly once per report regardless of ref count", loads)
	}
}

func TestBuildCauseConsumptionReportLoadsCandidatesOncePerReportIncludingMisses(t *testing.T) {
	homePath := t.TempDir()
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := s.Capture([]Candidate{cand("entity:candidate-load-shape", "status", "live", "")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	knownRef := captured[0].ID
	missingRef := "cand_missing_candidate_load_shape"
	mustWriteCauseTransition(t, s, "tr_candidate_load_shape_one", "work_candidate_load_shape_one", "repair", []string{
		knownRef,
		missingRef,
	}, t0.Add(time.Minute))
	mustWriteCauseTransition(t, s, "tr_candidate_load_shape_two", "work_candidate_load_shape_two", "repair", []string{
		knownRef,
		missingRef,
	}, t0.Add(2*time.Minute))

	previousLoader := loadCauseConsumptionCandidates
	loads := 0
	var requested []string
	loadCauseConsumptionCandidates = func(store *Store, ids []string) (map[string]Candidate, error) {
		loads++
		requested = append(requested, ids...)
		return previousLoader(store, ids)
	}
	t.Cleanup(func() { loadCauseConsumptionCandidates = previousLoader })

	if _, err := BuildCauseConsumptionReport(homePath, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("candidate files loaded in %d batches, want exactly one per report including repeated misses; requested=%v", loads, requested)
	}
	if len(requested) != 2 {
		t.Fatalf("candidate batch requested %d refs, want the two unique refs (hit + miss): %v", len(requested), requested)
	}
	seen := map[string]int{}
	for _, id := range requested {
		seen[id]++
	}
	if seen[knownRef] != 1 || seen[missingRef] != 1 {
		t.Fatalf("candidate batch = %v, want one lookup each for hit %s and miss %s", requested, knownRef, missingRef)
	}
}

// TestWriteCauseConsumptionRunWritesOnlyUnderStateRuns is F3: --record's artifact lands only
// under state/runs/cause-consumption/, never ledger/, projections/, candidates/, or claims/.
func TestWriteCauseConsumptionRunWritesOnlyUnderStateRuns(t *testing.T) {
	homePath := t.TempDir()
	if _, err := Open(homePath); err != nil {
		t.Fatal(err)
	}

	before := snapshotFiles(t, homePath)

	rep, err := BuildCauseConsumptionReport(homePath, t0)
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteCauseConsumptionRun(homePath, rep, t0)
	if err != nil {
		t.Fatal(err)
	}

	after := snapshotFiles(t, homePath)
	var added []string
	for f := range after {
		if !before[f] {
			added = append(added, f)
		}
	}
	if len(added) != 1 || added[0] != path {
		t.Fatalf("unexpected new files: %v (want exactly [%s])", added, path)
	}
	rel, err := filepath.Rel(homePath, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, filepath.Join("state", "runs", "cause-consumption")) {
		t.Fatalf("artifact written outside state/runs/cause-consumption: %s", rel)
	}
	for _, forbidden := range []string{"ledger", "projections", filepath.Join("candidates", "memory"), "claims"} {
		if strings.HasPrefix(rel, forbidden) {
			t.Fatalf("artifact landed under forbidden path %s: %s", forbidden, rel)
		}
	}
}

func snapshotFiles(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		out[path] = true
		return nil
	})
	return out
}

func snapshotCauseConsumptionTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = "file:" + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBuildCauseConsumptionReportJoinsDealtToCaused is F8: the report's dealt->caused join
// (S3b's kill-condition meter). A strictly later Cause-transition counts as caused; a
// same-persisted-second match is causally eligible but remains explicitly ambiguous; a
// candidate cited only in an EARLIER transition, or never cited at all, is an honest zero.
func TestBuildCauseConsumptionReportJoinsDealtToCaused(t *testing.T) {
	homePath := t.TempDir()
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}

	dealtAt := t0
	for _, row := range []StrangerDealtRow{
		{Ts: dealtAt.UTC().Format(time.RFC3339), CardID: "stranger:cand_hit", CandidateRef: "cand_hit", Topic: "topic-hit"},
		{Ts: dealtAt.UTC().Format(time.RFC3339), CardID: "stranger:cand_same_second", CandidateRef: "cand_same_second", Topic: "topic-same-second"},
		{Ts: dealtAt.UTC().Format(time.RFC3339), CardID: "stranger:cand_before", CandidateRef: "cand_before", Topic: "topic-before"},
		{Ts: dealtAt.UTC().Format(time.RFC3339), CardID: "stranger:cand_miss", CandidateRef: "cand_miss", Topic: "topic-miss"},
	} {
		if err := appendStrangerDealtRow(homePath, row); err != nil {
			t.Fatal(err)
		}
	}

	// cand_hit is cited by a Cause-transition AFTER the deal -> must join.
	mustWriteCauseTransition(t, s, "tr_f8_hit", "work_f8_hit", "accept", []string{"cand_hit"}, dealtAt.Add(time.Hour))
	// Persisted timestamps have only second precision; equality is causally eligible because
	// a genuinely later transition in the same second cannot be ordered more finely here.
	mustWriteCauseTransition(t, s, "tr_f8_same_second", "work_f8_same_second", "accept", []string{"cand_same_second"}, dealtAt)
	// cand_before is cited only BEFORE the deal -> not "later", must not join.
	mustWriteCauseTransition(t, s, "tr_f8_before", "work_f8_before", "accept", []string{"cand_before"}, dealtAt.Add(-time.Hour))
	// cand_miss is never cited anywhere -> honest zero.

	rep, err := BuildCauseConsumptionReport(homePath, dealtAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if rep.DealtCount != 4 {
		t.Fatalf("dealt_count = %d, want 4", rep.DealtCount)
	}
	if rep.DealtCausedCount != 1 || rep.DealtAmbiguousCount != 1 {
		t.Fatalf("dealt caused/ambiguous = %d/%d, want 1/1: %+v", rep.DealtCausedCount, rep.DealtAmbiguousCount, rep.DealtCards)
	}
	byRef := map[string]CauseConsumptionDealtRow{}
	for _, row := range rep.DealtCards {
		byRef[row.CandidateRef] = row
	}
	if hit := byRef["cand_hit"]; !hit.Caused || hit.CausalStatus != "caused" || hit.CausedByTransitionID != "tr_f8_hit" {
		t.Fatalf("cand_hit row = %+v, want caused=true transition=tr_f8_hit", hit)
	}
	if sameSecond := byRef["cand_same_second"]; sameSecond.Caused || sameSecond.CausalStatus != "same_second_ambiguous" || sameSecond.CausedByTransitionID != "" {
		t.Fatalf("cand_same_second row = %+v, want caused=false causal_status=same_second_ambiguous", sameSecond)
	}
	if before := byRef["cand_before"]; before.Caused || before.CausalStatus != "not_caused" {
		t.Fatalf("cand_before row = %+v, want caused=false (earlier transition does not count)", before)
	}
	if miss := byRef["cand_miss"]; miss.Caused || miss.CausalStatus != "not_caused" {
		t.Fatalf("cand_miss row = %+v, want caused=false (never cited)", miss)
	}
	if strings.TrimSpace(rep.DealtCausedNote) == "" {
		t.Fatal("dealt->caused note must be printed")
	}
}

// TestBuildCauseConsumptionReportDealtLogUnreadableDegradesGracefully proves a corrupt
// dealt-log does not brick the whole consumption sweep — the Cause bucket counts still land,
// only the S3b join is skipped for that run.
func TestBuildCauseConsumptionReportDealtLogUnreadableDegradesGracefully(t *testing.T) {
	homePath := t.TempDir()
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteCauseTransition(t, s, "tr_corrupt_log", "work_corrupt", "accept", []string{"commit:deadbeef"}, t0)

	logPath := filepath.Join(homePath, filepath.FromSlash(StrangerDealtLogRelPath))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("{not valid json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := BuildCauseConsumptionReport(homePath, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("a corrupt dealt-log must not error the whole report: %v", err)
	}
	if rep.TransitionsWithCause != 1 || rep.RefTierCounts[CauseConsumptionTierNonMemory] != 1 {
		t.Fatalf("cause sweep must still land despite corrupt dealt-log: %+v", rep)
	}
	if rep.DealtCount != 0 {
		t.Fatalf("dealt_count = %d, want 0 (join skipped on corrupt log)", rep.DealtCount)
	}
	if strings.TrimSpace(rep.DealtCausedNote) == "" {
		t.Fatal("degraded run must still explain itself")
	}
}
