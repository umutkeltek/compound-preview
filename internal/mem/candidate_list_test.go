package mem

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

func TestListCandidatesFiltersCountsAndEvidenceGate(t *testing.T) {
	s := newStore(t)

	proposed := cand("entity:one", "status", "proposed", "")
	parked := cand("entity:two", "status", "parked", "")
	promoted := cand("entity:three", "status", "promoted", "")
	rejected := cand("entity:four", "status", "rejected", "")
	got, err := s.Capture([]Candidate{proposed, parked, promoted, rejected}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(got[1].ID, "park", "", t0); err != nil {
		t.Fatal(err)
	}
	promotedResult, err := promoteForTest(t, s, got[2].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(got[3].ID, "reject", "", t0); err != nil {
		t.Fatal(err)
	}
	tagRawCandidateKind(t, s, got[1].ID, "doctrine")

	seq, err := s.nextSeq()
	if err != nil {
		t.Fatal(err)
	}
	if err := appendJSONL(s.edgesLog(), Edge{
		Seq:          seq,
		Kind:         EdgeKindNeedsReview,
		From:         "review_fixture",
		To:           promotedResult.Claim.ID,
		Confidence:   0.9,
		EvidenceRefs: []string{"review_fixture"},
		CreatedAt:    rfc3339(t0),
	}); err != nil {
		t.Fatal(err)
	}

	report, err := s.ListCandidates(t0, CandidateListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Total != 4 || report.Counts.Matched != 4 {
		t.Fatalf("counts = %+v", report.Counts)
	}
	for state, want := range map[string]int{"proposed": 1, "parked": 1, "promoted": 1, "rejected": 1} {
		if got := report.Counts.ByState[state]; got != want {
			t.Fatalf("by_state[%s] = %d, want %d; counts=%+v", state, got, want, report.Counts)
		}
	}
	if report.Items[0].SourceTraceRef != "" || report.Items[0].SnippetHash != "" || len(report.Items[0].EvidenceRefs) != 0 {
		t.Fatalf("evidence leaked by default: %+v", report.Items[0])
	}
	limitedReport, err := s.ListCandidates(t0, CandidateListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if limitedReport.Counts.Total != 4 || len(limitedReport.Items) != 1 || limitedReport.Counts.ByState["promoted"] != 1 {
		t.Fatalf("limit truncated counts: %+v items=%d", limitedReport.Counts, len(limitedReport.Items))
	}

	kindReport, err := s.ListCandidates(t0, CandidateListOptions{Kind: "doctrine"})
	if err != nil {
		t.Fatal(err)
	}
	if kindReport.Counts.Matched != 1 || kindReport.Items[0].ID != got[1].ID || kindReport.Items[0].Kind != "doctrine" {
		t.Fatalf("kind filter report = %+v", kindReport)
	}

	reviewReport, err := s.ListCandidates(t0, CandidateListOptions{State: ClaimStateNeedsReview})
	if err != nil {
		t.Fatal(err)
	}
	if reviewReport.Counts.Matched != 1 || reviewReport.Items[0].ID != got[2].ID || reviewReport.Items[0].ReviewState != ClaimStateNeedsReview {
		t.Fatalf("needs_review report = %+v", reviewReport)
	}

	evidenceReport, err := s.ListCandidates(t0, CandidateListOptions{State: "proposed", IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidenceReport.Counts.Matched != 1 || evidenceReport.Items[0].SourceTraceRef == "" || len(evidenceReport.Items[0].EvidenceRefs) == 0 {
		t.Fatalf("include evidence report = %+v", evidenceReport)
	}
}

func TestCandidateSnapshotFailsMutatorsClosedAndListsNamedCorruption(t *testing.T) {
	s := newStore(t)
	valid := Candidate{ID: "valid", Entity: "valid", Predicate: "p", Object: "o", State: "proposed", Visibility: "operational", Derivation: "extracted", CreatedAt: rfc3339(t0)}
	if err := candidates.WriteCandidate(s.home, candidates.Memory, valid.ID, &valid); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.candDir(), "mismatch.json"), []byte(`{"id":"other","state":"proposed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "linked.json")
	if err := os.WriteFile(outside, []byte(`{"id":"linked","state":"proposed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.candDir(), "linked.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.candDir(), "scratch.tmp"), []byte("not a canonical candidate"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AllCandidates(); err == nil {
		t.Fatal("AllCandidates accepted corrupt custody on a decision/mutation seam")
	}
	report, err := s.ListCandidates(t0, CandidateListOptions{})
	if err != nil {
		t.Fatalf("projection must retain valid rows while naming corruption: %v", err)
	}
	if report.Counts.Total != 1 || len(report.Items) != 1 || report.Items[0].ID != valid.ID {
		t.Fatalf("candidate list included corrupt rows: counts=%+v items=%+v", report.Counts, report.Items)
	}
	warnings := strings.Join(report.Warnings, "\n")
	for _, entry := range []string{"linked.json", "mismatch.json", "scratch.tmp"} {
		if !strings.Contains(warnings, entry) {
			t.Fatalf("warnings %q do not name corrupt entry %s", warnings, entry)
		}
	}
}

func TestListCandidatesAppliesAdmissionToLegacySourceRows(t *testing.T) {
	s := newStore(t)
	legacy := Candidate{
		ID:             "cand_legacy_source_metadata",
		Entity:         "knowledge:source:item",
		Predicate:      "title",
		Object:         "Legacy source title",
		Confidence:     0.8,
		Derivation:     "deterministic",
		Qualifiers:     map[string]string{"source": "knowledge_import"},
		SourceTraceRef: "ev_legacy_source",
		SnippetHash:    "abc123",
		Visibility:     "candidate",
		State:          "proposed",
		CreatedAt:      rfc3339(t0),
	}
	writeRawCandidate(t, s, legacy)

	proposed, err := s.ListCandidates(t0, CandidateListOptions{State: "proposed"})
	if err != nil {
		t.Fatal(err)
	}
	if proposed.Counts.Matched != 0 || proposed.Counts.ByState["proposed"] != 0 {
		t.Fatalf("proposed report = %+v, want legacy source row admitted away from operator queue", proposed.Counts)
	}

	parked, err := s.ListCandidates(t0, CandidateListOptions{State: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	if parked.Counts.Matched != 1 || parked.Counts.ByState["parked"] != 1 {
		t.Fatalf("parked report = %+v, want legacy source row counted as parked", parked.Counts)
	}
	if parked.Items[0].State != "parked" || !strings.Contains(parked.Items[0].Reason, "source metadata") {
		t.Fatalf("parked item = %+v, want admitted source metadata", parked.Items[0])
	}

	var persisted Candidate
	data, err := os.ReadFile(filepath.Join(s.candDir(), legacy.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.State != "proposed" || persisted.Reason != "" {
		t.Fatalf("persisted candidate mutated by list projection: %+v", persisted)
	}
}

func TestListCandidatesIncludeEvidenceAddsCardContext(t *testing.T) {
	s := newStore(t)
	ev, err := s.RecordEvidence(EvidenceItem{
		SourceKind: "chatgpt",
		SourceID:   "conversation:abc",
		QuoteText:  "On June 8, keep the old memory engine only as a temporary bridge.",
		EventTime:  "2026-06-08T09:00:00Z",
		Actor:      "user",
		Visibility: "private",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := s.Capture([]Candidate{{
		Entity:          "project:compound",
		Predicate:       "memory_engine_strategy",
		Object:          "own the breadth engine; rent legacy indexes only as bridges",
		ObservationDate: "2026-07-01",
		Confidence:      0.93,
		Derivation:      "deterministic",
		PromotionBasis:  candidates.BasisExplicitConfirmation,
		SourceTraceRef:  ev.ID,
		SnippetHash:     ev.ContentHash,
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := promoteForTest(t, s, prior[0].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Capture([]Candidate{{
		Entity:          "project:compound",
		Predicate:       "memory_engine_strategy",
		Object:          "keep the old memory engine as the source of truth",
		ObservationDate: "2026-06-08",
		Confidence:      0.72,
		Derivation:      "extracted",
		PromotionBasis:  candidates.BasisExplicitConfirmation,
		SourceTraceRef:  ev.ID,
		SnippetHash:     ev.ContentHash,
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.ListCandidates(t0, CandidateListOptions{State: "proposed", IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Matched != 1 || report.Items[0].ID != next[0].ID {
		t.Fatalf("report items = %+v", report.Items)
	}
	data, err := json.Marshal(report.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ObservationDate string `json:"observation_date"`
		Evidence        struct {
			Ref       string `json:"ref"`
			Quote     string `json:"quote"`
			SourceID  string `json:"source_id"`
			EventTime string `json:"event_time"`
		} `json:"evidence"`
		BelievedBefore struct {
			ClaimID   string `json:"claim_id"`
			Object    string `json:"object"`
			CreatedAt string `json:"created_at"`
		} `json:"believed_before"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ObservationDate != "2026-06-08" {
		t.Fatalf("observation date = %q", got.ObservationDate)
	}
	if got.Evidence.Ref != ev.ID || got.Evidence.SourceID != "conversation:abc" || got.Evidence.EventTime != "2026-06-08T09:00:00Z" {
		t.Fatalf("evidence context = %+v", got.Evidence)
	}
	if !strings.Contains(got.Evidence.Quote, "temporary bridge") {
		t.Fatalf("evidence quote = %q", got.Evidence.Quote)
	}
	if got.BelievedBefore.ClaimID != promoted.Claim.ID || got.BelievedBefore.Object != promoted.Claim.Object || got.BelievedBefore.CreatedAt == "" {
		t.Fatalf("believed_before = %+v, want claim %+v", got.BelievedBefore, promoted.Claim)
	}
}

// F1 (packet work_709c9ba49c56): a candidate cited by two independent
// evidence refs/projects with confidence >= 0.8 must project
// recurrence_count = 2 (distinct evidence refs across candidates sharing the
// same normalized claim key) and a confidence_trend. A single-ref candidate
// must not. The fields are derived read-only signals, computed at projection
// time — the on-disk candidate JSON must stay byte-identical (no schema
// mutation, no new stored state).
func TestListCandidatesRecurrenceAndTrendAreDerivedSignals(t *testing.T) {
	s := newStore(t)

	// Two distinct evidence refs, same normalized claim (entity|predicate|
	// object): the chip threshold.
	rec1, err := s.RecordEvidence(EvidenceItem{
		SourceKind: "chatgpt",
		SourceID:   "conversation:alpha",
		QuoteText:  "Project alpha runbook is the canonical entry point.",
		EventTime:  "2026-07-01T09:00:00Z",
		Actor:      "user",
		Visibility: "private",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := s.RecordEvidence(EvidenceItem{
		SourceKind: "chatgpt",
		SourceID:   "conversation:beta",
		QuoteText:  "Project alpha runbook is the canonical entry point.",
		EventTime:  "2026-07-02T09:00:00Z",
		Actor:      "user",
		Visibility: "private",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}

	high := []Candidate{{
		Entity:          "project:alpha",
		Predicate:       "runbook_role",
		Object:          "canonical entry point",
		ObservationDate: "2026-07-01",
		Confidence:      0.9,
		Derivation:      "extracted",
		PromotionBasis:  candidates.BasisExplicitConfirmation,
		SourceTraceRef:  rec1.ID,
		SnippetHash:     rec1.ContentHash,
	}, {
		Entity:          "project:alpha",
		Predicate:       "runbook_role",
		Object:          "canonical entry point",
		ObservationDate: "2026-07-02",
		Confidence:      0.85,
		Derivation:      "extracted",
		PromotionBasis:  candidates.BasisExplicitConfirmation,
		SourceTraceRef:  rec2.ID,
		SnippetHash:     rec2.ContentHash,
	}}
	caps, err := s.Capture(high, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(caps[0].ID, "park", "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(caps[1].ID, "park", "", t0); err != nil {
		t.Fatal(err)
	}

	// One-off candidate with a different normalized claim (different object).
	rec3, err := s.RecordEvidence(EvidenceItem{
		SourceKind: "chatgpt",
		SourceID:   "conversation:gamma",
		QuoteText:  "Unrelated single-observation fact.",
		EventTime:  "2026-07-03T09:00:00Z",
		Actor:      "user",
		Visibility: "private",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	lone, err := s.Capture([]Candidate{{
		Entity:          "project:alpha",
		Predicate:       "runbook_role",
		Object:          "supplementary handbook",
		ObservationDate: "2026-07-03",
		Confidence:      0.92,
		Derivation:      "extracted",
		PromotionBasis:  candidates.BasisExplicitConfirmation,
		SourceTraceRef:  rec3.ID,
		SnippetHash:     rec3.ContentHash,
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(lone[0].ID, "park", "", t0); err != nil {
		t.Fatal(err)
	}

	report, err := s.ListCandidates(t0, CandidateListOptions{State: "parked", IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}

	items := map[string]CandidateListItem{}
	for _, it := range report.Items {
		items[it.ID] = it
	}

	both := items[caps[0].ID]
	if both.RecurrenceCount != 2 {
		t.Fatalf("two-ref candidate %s RecurrenceCount = %d, want 2 (item=%+v)", caps[0].ID, both.RecurrenceCount, both)
	}
	if both.ConfidenceTrend == "" {
		t.Fatalf("two-ref candidate %s ConfidenceTrend is empty, want a non-empty label", caps[0].ID)
	}

	loneItem := items[lone[0].ID]
	if loneItem.RecurrenceCount != 1 {
		t.Fatalf("lone candidate %s RecurrenceCount = %d, want 1 (item=%+v)", lone[0].ID, loneItem.RecurrenceCount, loneItem)
	}
	if loneItem.ConfidenceTrend != "new" {
		t.Fatalf("lone candidate %s ConfidenceTrend = %q, want \"new\"", lone[0].ID, loneItem.ConfidenceTrend)
	}

	// Storage invariant: derived fields must NOT mutate the on-disk candidate.
	// We re-run the projection twice and byte-compare the persisted file
	// before and after the second pass — the projection must be a pure read.
	for _, id := range []string{caps[0].ID, caps[1].ID, lone[0].ID} {
		rawBefore, err := os.ReadFile(filepath.Join(s.candDir(), id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		// Re-run the projection; it must not touch disk.
		if _, err := s.ListCandidates(t0, CandidateListOptions{State: "parked", IncludeEvidence: true}); err != nil {
			t.Fatal(err)
		}
		rawAfter, err := os.ReadFile(filepath.Join(s.candDir(), id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rawBefore, rawAfter) {
			t.Fatalf("persisted candidate %s mutated by list projection\n before: %s\n after:  %s", id, rawBefore, rawAfter)
		}
		// The persisted JSON must not carry the derived field names either —
		// they are projection-only, not stored.
		var blob map[string]any
		if err := json.Unmarshal(rawAfter, &blob); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"recurrence_count", "confidence_trend"} {
			if _, ok := blob[key]; ok {
				t.Fatalf("derived field %q leaked into stored candidate %s JSON", key, id)
			}
		}
	}
}

// F2 (packet work_709c9ba49c56): confidence_trend discriminates rising vs
// flat. The trend is computed against the SAME normalized claim key (entity +
// predicate + object, lowercased/trimmed). A second observation with a
// strictly higher confidence → "rising"; equal confidence → "flat".
func TestListCandidatesConfidenceTrendRisesWithNewerHigherConfidence(t *testing.T) {
	s := newStore(t)

	rec1, err := s.RecordEvidence(EvidenceItem{
		SourceKind: "chatgpt",
		SourceID:   "conversation:r1",
		QuoteText:  "Project alpha runbook is the canonical entry point.",
		EventTime:  "2026-07-01T09:00:00Z",
		Actor:      "user",
		Visibility: "private",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := s.RecordEvidence(EvidenceItem{
		SourceKind: "chatgpt",
		SourceID:   "conversation:r2",
		QuoteText:  "Project alpha runbook is the canonical entry point.",
		EventTime:  "2026-07-02T09:00:00Z",
		Actor:      "user",
		Visibility: "private",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}

	// Earlier observation: lower confidence.
	earlier, err := s.Capture([]Candidate{{
		Entity:          "project:alpha",
		Predicate:       "runbook_role",
		Object:          "canonical entry point",
		ObservationDate: "2026-07-01",
		Confidence:      0.7,
		Derivation:      "extracted",
		PromotionBasis:  candidates.BasisExplicitConfirmation,
		SourceTraceRef:  rec1.ID,
		SnippetHash:     rec1.ContentHash,
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(earlier[0].ID, "park", "", t0); err != nil {
		t.Fatal(err)
	}

	// Later observation: higher confidence + a second independent ref.
	later, err := s.Capture([]Candidate{{
		Entity:          "project:alpha",
		Predicate:       "runbook_role",
		Object:          "canonical entry point",
		ObservationDate: "2026-07-02",
		Confidence:      0.9,
		Derivation:      "extracted",
		PromotionBasis:  candidates.BasisExplicitConfirmation,
		SourceTraceRef:  rec2.ID,
		SnippetHash:     rec2.ContentHash,
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(later[0].ID, "park", "", t0); err != nil {
		t.Fatal(err)
	}

	report, err := s.ListCandidates(t0, CandidateListOptions{State: "parked", IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}

	items := map[string]CandidateListItem{}
	for _, it := range report.Items {
		items[it.ID] = it
	}

	if items[later[0].ID].ConfidenceTrend != "rising" {
		t.Fatalf("higher-confidence later candidate trend = %q, want \"rising\"", items[later[0].ID].ConfidenceTrend)
	}
	if items[later[0].ID].RecurrenceCount != 2 {
		t.Fatalf("rising candidate recurrence = %d, want 2 (item=%+v)", items[later[0].ID].RecurrenceCount, items[later[0].ID])
	}

	// A third observation with the SAME confidence as a prior produces a
	// "flat" trend (no upward momentum, no decline).
	rec3, err := s.RecordEvidence(EvidenceItem{
		SourceKind: "chatgpt",
		SourceID:   "conversation:r3",
		QuoteText:  "Project alpha runbook is the canonical entry point.",
		EventTime:  "2026-07-04T09:00:00Z",
		Actor:      "user",
		Visibility: "private",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	flat, err := s.Capture([]Candidate{{
		Entity:          "project:alpha",
		Predicate:       "runbook_role",
		Object:          "canonical entry point",
		ObservationDate: "2026-07-04",
		Confidence:      0.9,
		Derivation:      "extracted",
		PromotionBasis:  candidates.BasisExplicitConfirmation,
		SourceTraceRef:  rec3.ID,
		SnippetHash:     rec3.ContentHash,
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(flat[0].ID, "park", "", t0); err != nil {
		t.Fatal(err)
	}
	report2, err := s.ListCandidates(t0, CandidateListOptions{State: "parked", IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	items2 := map[string]CandidateListItem{}
	for _, it := range report2.Items {
		items2[it.ID] = it
	}
	if got := items2[flat[0].ID].ConfidenceTrend; got != "flat" {
		t.Fatalf("equal-confidence candidate trend = %q, want \"flat\"", got)
	}
}

func tagRawCandidateKind(t *testing.T, s *Store, id, kind string) {
	t.Helper()
	path := filepath.Join(s.candDir(), id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	payload["kind"] = kind
	data, err = json.MarshalIndent(payload, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRawCandidate(t *testing.T, s *Store, c Candidate) {
	t.Helper()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.candDir(), c.ID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestListCandidatesLimitReportsTrueMatchCount pins the separation between "how many
// matched" and "how many were returned". The regression it guards is a listing that
// stopped counting at Limit: over a large inbox that made Counts.Matched describe the
// page, so a caller filtering Items downstream read a bounded listing as proof the
// candidates did not exist — when they were on disk, just past the limit in iteration
// order. A confident wrong zero is worse than a slow answer.
func TestListCandidatesLimitReportsTrueMatchCount(t *testing.T) {
	s := newStore(t)

	// Distinct objects: Capture is content-addressed on entity|predicate|object, so
	// identical rows collapse to one and would never bind the limit.
	seed := make([]Candidate, 0, 6)
	for _, obj := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		seed = append(seed, cand("entity:bulk", "status", obj, ""))
	}
	if _, err := s.Capture(seed, t0); err != nil {
		t.Fatal(err)
	}

	full, err := s.ListCandidates(t0, CandidateListOptions{State: "proposed"})
	if err != nil {
		t.Fatal(err)
	}
	if full.Counts.Truncated || full.Counts.Matched != full.Counts.Returned {
		t.Fatalf("unbounded listing must not report truncation: %+v", full.Counts)
	}
	total := full.Counts.Matched
	if total < 2 {
		t.Fatalf("fixture needs >=2 proposed candidates to bind a limit, got %d", total)
	}

	limited, err := s.ListCandidates(t0, CandidateListOptions{State: "proposed", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The load-bearing assertion: Matched counts the SET even though only one item
	// was projected. Before the fix this equalled 1 and the set size was unknowable.
	if limited.Counts.Matched != total {
		t.Fatalf("Matched = %d under Limit 1, want the true match count %d; counts=%+v",
			limited.Counts.Matched, total, limited.Counts)
	}
	if limited.Counts.Returned != 1 || len(limited.Items) != 1 {
		t.Fatalf("Returned = %d / items = %d, want 1; counts=%+v",
			limited.Counts.Returned, len(limited.Items), limited.Counts)
	}
	if !limited.Counts.Truncated {
		t.Fatalf("Truncated must be set when Matched > Returned: %+v", limited.Counts)
	}
}

// The created_after window gates ONLY the returned items; Counts stay global.
// The console briefing reads its Mind Δ headline numbers from counts.by_state
// and its 14d weekly window from the bounded items, so a window that excludes
// every candidate must still report the full-set state counts — otherwise a
// bounded caller would read a confident zero as the whole truth.
func TestListCandidatesCreatedAfterGatesItemsNotCounts(t *testing.T) {
	s := newStore(t)

	one := cand("entity:week-a", "status", "proposed", "")
	two := cand("entity:week-b", "status", "promoted", "")
	got, err := s.Capture([]Candidate{one, two}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promoteForTest(t, s, got[1].ID, "", t0); err != nil {
		t.Fatal(err)
	}

	// Boundary-inclusive: candidates created exactly at the window start match.
	atBoundary, err := s.ListCandidates(t0, CandidateListOptions{CreatedAfter: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(atBoundary.Items) != 2 || atBoundary.Counts.Matched != 2 {
		t.Fatalf("created_after=t0 should match both candidates, got matched=%d items=%d", atBoundary.Counts.Matched, len(atBoundary.Items))
	}

	// A window after every candidate excludes all items but must NOT move the
	// global counts. Matched counts the FILTERED set, so a window matching
	// nothing truthfully reports matched=0 (not truncation — the limit never
	// engaged).
	after, err := s.ListCandidates(t0, CandidateListOptions{CreatedAfter: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != 0 || after.Counts.Matched != 0 || after.Counts.Returned != 0 {
		t.Fatalf("created_after past every candidate should return no items, got matched=%d items=%d returned=%d", after.Counts.Matched, len(after.Items), after.Counts.Returned)
	}
	if after.Counts.ByState["proposed"] != 1 || after.Counts.ByState["promoted"] != 1 {
		t.Fatalf("counts must stay global under created_after, got %+v", after.Counts.ByState)
	}
	if after.Counts.Total != 2 {
		t.Fatalf("total must stay 2 under created_after, got %d", after.Counts.Total)
	}
	if after.Filters["created_after"] != t0.Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("filters must echo created_after, got %+v", after.Filters)
	}
}
