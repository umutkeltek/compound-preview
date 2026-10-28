package mem

import (
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/policy"
)

func TestAdmitCandidateParksKnowledgeEvidence(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
		target string
	}{
		{name: "metadata import", source: "knowledge_import", want: "source metadata", target: AdmissionSurfaceSourceIndex},
		{name: "extraction output", source: "knowledge_extraction", want: "knowledge extraction", target: AdmissionSurfaceAnnotatedEvidence},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := Candidate{
				ID:             "cand_" + strings.ReplaceAll(tt.name, " ", "_"),
				Entity:         "knowledge:source:item",
				Predicate:      "reports_result",
				Object:         "grounded statement",
				Confidence:     0.8,
				Derivation:     "extracted",
				Qualifiers:     map[string]string{"source": tt.source},
				SourceTraceRef: "ev_source",
				SnippetHash:    "abc123",
				Visibility:     "candidate",
				State:          "proposed",
				CreatedAt:      rfc3339(t0),
			}

			admitted, decision := AdmitCandidate(candidate)
			if admitted.State != "parked" {
				t.Fatalf("admitted state = %q, want parked; decision=%+v", admitted.State, decision)
			}
			if !strings.Contains(admitted.Reason, tt.want) {
				t.Fatalf("reason = %q, want mention %q", admitted.Reason, tt.want)
			}
			if decision.ReviewRequired {
				t.Fatalf("knowledge evidence should not require operator review: %+v", decision)
			}
			if decision.TargetSurface != tt.target {
				t.Fatalf("target surface = %q, want %q", decision.TargetSurface, tt.target)
			}
		})
	}
}

func TestAdmitCandidateOperatorPermanentStaysProposed(t *testing.T) {
	candidate := Candidate{
		ID:             "cand_durable",
		Entity:         "project:compound",
		Predicate:      "decided",
		Object:         "operator decisions remain manually gated",
		Confidence:     0.9,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     map[string]string{"domain": "memory"},
		SourceTraceRef: "ev_durable",
		SnippetHash:    "def456",
		Visibility:     "operational",
		CreatedAt:      rfc3339(t0),
	}

	admitted, decision := AdmitCandidate(candidate)
	if admitted.State != "proposed" {
		t.Fatalf("admitted state = %q, want proposed; decision=%+v", admitted.State, decision)
	}
	if !decision.ReviewRequired || decision.TargetSurface != AdmissionSurfaceOperatorQueue {
		t.Fatalf("operator-permanent should remain operator-gated: %+v", decision)
	}
}

func TestAdmitCandidateUnEvidencedDecayParks(t *testing.T) {
	// Decisions-down: un-evidenced durable memory does NOT enter the DECIDE queue.
	candidate := Candidate{
		ID:             "cand_unevidenced",
		Entity:         "project:compound",
		Predicate:      "notes",
		Object:         "a loose observation",
		Confidence:     0.5,
		Derivation:     "extracted",
		SourceTraceRef: "ev_loose",
		SnippetHash:    "aaa",
		Visibility:     "operational",
		CreatedAt:      rfc3339(t0),
	}
	admitted, decision := AdmitCandidateAt(candidate, t0)
	if admitted.State != "parked" {
		t.Fatalf("state = %q, want parked; decision=%+v", admitted.State, decision)
	}
	if decision.Disposition != policy.DispositionDecayPark {
		t.Fatalf("disposition = %q", decision.Disposition)
	}
	if admitted.ExpiresAt == "" {
		t.Fatal("decay_park must stamp ExpiresAt")
	}
	exp, err := time.Parse(time.RFC3339, admitted.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	want := t0.AddDate(0, 0, policy.DefaultDecayParkDays)
	if !exp.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", exp, want)
	}
	if decision.ReviewRequired {
		t.Fatal("decay_park must not require operator review")
	}
}

func TestAdmitCandidatePrivateNeverAutoFiles(t *testing.T) {
	candidate := Candidate{
		ID:             "cand_private_det",
		Entity:         "project:compound",
		Predicate:      "secret_metric",
		Object:         "42",
		Confidence:     1,
		Derivation:     "deterministic",
		PromotionBasis: candidates.BasisTrustedDeterministic,
		SourceTraceRef: "ev_priv",
		SnippetHash:    "bbb",
		Visibility:     "private",
		CreatedAt:      rfc3339(t0),
	}
	admitted, decision := AdmitCandidate(candidate)
	if decision.Disposition == policy.DispositionAutoFile || admitted.State == "no_promote" {
		t.Fatalf("private must never auto_file: admitted=%+v decision=%+v", admitted, decision)
	}
	if admitted.State != "proposed" || !decision.ReviewRequired {
		t.Fatalf("private deterministic should be operator_required: state=%q decision=%+v", admitted.State, decision)
	}
}

func TestAdmitCandidateAutoFilesTrustedImport(t *testing.T) {
	candidate := Candidate{
		ID:             "cand_auto",
		Entity:         "project:compound",
		Predicate:      "imported_fact",
		Object:         "from curated page",
		Confidence:     1,
		Derivation:     "deterministic",
		PromotionBasis: candidates.BasisTrustedDeterministic,
		SourceTraceRef: "ev_imp",
		SnippetHash:    "ccc",
		Visibility:     "operational",
		CreatedAt:      rfc3339(t0),
	}
	admitted, decision := AdmitCandidate(candidate)
	if admitted.State != "no_promote" {
		t.Fatalf("state = %q, want no_promote; decision=%+v", admitted.State, decision)
	}
	if decision.Disposition != policy.DispositionAutoFile {
		t.Fatalf("disposition = %q", decision.Disposition)
	}
}

func TestCaptureAppliesDecisionsDownAdmission(t *testing.T) {
	s := newStore(t)
	// Un-evidenced capture → parked with expiry (not proposed).
	got, err := s.Capture([]Candidate{{
		Entity: "e", Predicate: "p", Object: "o", Confidence: 0.5,
		Derivation: "extracted", SourceTraceRef: "t", SnippetHash: "h",
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != "parked" || got[0].ExpiresAt == "" {
		t.Fatalf("capture should decay_park un-evidenced: %+v", got)
	}
	// Operator-permanent claim type stays proposed.
	got2, err := s.Capture([]Candidate{{
		Entity: "e2", Predicate: "decided", Object: "rule", Confidence: 0.9,
		Derivation: "deterministic", ClaimType: "decision",
		PromotionBasis: candidates.BasisExplicitConfirmation,
		SourceTraceRef: "t2", SnippetHash: "h2",
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 || got2[0].State != "proposed" {
		t.Fatalf("decision claim type should stay proposed: %+v", got2)
	}
}

func TestRecordStableCandidateAppliesAdmission(t *testing.T) {
	store := newStore(t)
	candidate := Candidate{
		ID:             "cand_metadata_admission",
		Entity:         "knowledge:source:item",
		Predicate:      "reference_title",
		Object:         "Source Title",
		Confidence:     0.8,
		Derivation:     "deterministic",
		Qualifiers:     map[string]string{"source": "knowledge_import"},
		SourceTraceRef: "ev_metadata",
		SnippetHash:    "abc123",
		Visibility:     "candidate",
		State:          "proposed",
		CreatedAt:      rfc3339(t0),
	}

	created, err := store.RecordStableCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first stable candidate write should create")
	}
	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v", candidates)
	}
	if candidates[0].State != "parked" || !strings.Contains(candidates[0].Reason, "source metadata") {
		t.Fatalf("admitted candidate = %+v, want parked source metadata", candidates[0])
	}
	report, err := store.ListCandidates(t0, CandidateListOptions{State: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Matched != 1 || !strings.Contains(report.Items[0].Reason, "source metadata") {
		t.Fatalf("candidate list admission reason = %+v", report)
	}
}
