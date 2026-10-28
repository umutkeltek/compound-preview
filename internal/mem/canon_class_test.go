package mem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
)

func TestEffectiveCanonClassDerivation(t *testing.T) {
	cases := []struct {
		claimType, canonClass, want string
	}{
		{"decision", "", "decision"},
		{"preference", "", "principle"},
		{"constraint", "", "principle"},
		{"architecture", "", "decision"},
		{"status", "", "status"},
		{"", "", CanonClassUnclassified},
		{"unknown_type", "", CanonClassUnclassified},
		// Explicit wins over ClaimType.
		{"decision", "research_fact", "research_fact"},
		{"preference", "behavior", "behavior"},
		// Invalid explicit → unclassified (Capture refuses; derivation is defensive).
		{"decision", "vibe", CanonClassUnclassified},
	}
	for _, tc := range cases {
		if got := EffectiveCanonClass(tc.claimType, tc.canonClass); got != tc.want {
			t.Errorf("EffectiveCanonClass(%q,%q) = %q, want %q", tc.claimType, tc.canonClass, got, tc.want)
		}
	}
}

func TestCanonClassLowRiskTable(t *testing.T) {
	// Full vocabulary: every CanonClasses entry has a defined tier; only the
	// three low-risk classes return true. A future vocabulary edit that forgets
	// the tier table fails here.
	wantLow := map[string]bool{
		"research_fact":  true,
		"product_metric": true,
		"status":         true,
	}
	for _, class := range CanonClasses {
		got := CanonClassLowRisk(class)
		if got != wantLow[class] {
			t.Errorf("CanonClassLowRisk(%q) = %v, want %v (tier=%s)", class, got, wantLow[class], CanonRiskTier(class))
		}
	}
	// Unknown / unclassified never low-risk.
	for _, class := range []string{CanonClassUnclassified, "vibe", ""} {
		if CanonClassLowRisk(class) {
			t.Errorf("CanonClassLowRisk(%q) = true, want false", class)
		}
	}
	// Operator-permanent never low-risk.
	for _, class := range []string{"decision", "principle", "behavior"} {
		if !CanonClassOperatorPermanent(class) {
			t.Errorf("CanonClassOperatorPermanent(%q) = false", class)
		}
		if CanonClassLowRisk(class) {
			t.Errorf("operator-permanent class %q must not be low-risk", class)
		}
	}
}

func TestCaptureRefusesInvalidAndUnclassifiedCanonClass(t *testing.T) {
	s := newStore(t)
	now := t0
	base := Candidate{
		Entity: "project:compound", Predicate: "has_rule", Object: "x",
		Confidence: 0.9, Derivation: "deterministic", SourceTraceRef: "trace:t1",
		SnippetHash: "h1", Visibility: "operational",
	}

	// Invalid class refused, naming vocabulary.
	bad := base
	bad.CanonClass = "vibe"
	_, err := s.Capture([]Candidate{bad}, now)
	if err == nil || !strings.Contains(err.Error(), "canon_class") || !strings.Contains(err.Error(), "vibe") {
		t.Fatalf("expected bad canon_class error, got %v", err)
	}
	if !strings.Contains(err.Error(), "research_fact") {
		t.Fatalf("error should name allowed vocabulary, got %v", err)
	}

	// unclassified is derived-only — not storable.
	unc := base
	unc.CanonClass = CanonClassUnclassified
	_, err = s.Capture([]Candidate{unc}, now)
	if err == nil || !strings.Contains(err.Error(), "unclassified") {
		t.Fatalf("expected unclassified refused, got %v", err)
	}
}

func TestCanonClassSurvivesPromotion(t *testing.T) {
	s := newStore(t)
	got, err := s.Capture([]Candidate{{
		Entity: "project:compound", Predicate: "bench_metric", Object: "hit@1",
		Confidence: 0.95, Derivation: "deterministic", ClaimType: "status",
		CanonClass: "research_fact", SourceTraceRef: "trace:t1", SnippetHash: "h1",
		Visibility: "operational", PromotionBasis: candidates.BasisExplicitConfirmation,
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("captured %d", len(got))
	}
	res, err := promoteForTest(t, s, got[0].ID, "work_test_canon", t0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claim == nil || res.Claim.CanonClass != "research_fact" {
		t.Fatalf("promoted claim CanonClass = %+v, want research_fact", res.Claim)
	}
	// Durable log carries the field.
	raw, err := os.ReadFile(filepath.Join(s.home, "ledger", "claims.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var claim Claim
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.CanonClass != "research_fact" {
		t.Fatalf("claims.jsonl CanonClass = %q", claim.CanonClass)
	}
}

func TestCanonDistributionDerivesLegacy(t *testing.T) {
	s := newStore(t)
	// One explicit research_fact, one ClaimType=decision (→ decision), one preference (→ principle).
	cands := []Candidate{
		{
			Entity: "e1", Predicate: "p1", Object: "o1", Confidence: 0.9,
			Derivation: "deterministic", ClaimType: "decision",
			SourceTraceRef: "t1", SnippetHash: "h1", Visibility: "operational",
			PromotionBasis: candidates.BasisExplicitConfirmation,
		},
		{
			Entity: "e2", Predicate: "p2", Object: "o2", Confidence: 0.9,
			Derivation: "deterministic", ClaimType: "preference",
			SourceTraceRef: "t2", SnippetHash: "h2", Visibility: "operational",
			PromotionBasis: candidates.BasisExplicitConfirmation,
		},
		{
			Entity: "e3", Predicate: "p3", Object: "o3", Confidence: 0.9,
			Derivation: "deterministic", CanonClass: "research_fact",
			SourceTraceRef: "t3", SnippetHash: "h3", Visibility: "operational",
			PromotionBasis: candidates.BasisExplicitConfirmation,
		},
	}
	got, err := s.Capture(cands, t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if _, err := promoteForTest(t, s, c.ID, "work_dist", t0); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.CanonDistribution()
	if err != nil {
		t.Fatal(err)
	}
	if rep.SchemaVersion != CanonClassReportSchemaVersion {
		t.Fatalf("schema = %s", rep.SchemaVersion)
	}
	if rep.Total != 3 {
		t.Fatalf("total = %d", rep.Total)
	}
	if rep.Distribution["decision"] != 1 {
		t.Fatalf("decision dist = %d (from ClaimType decision)", rep.Distribution["decision"])
	}
	if rep.Distribution["principle"] != 1 {
		t.Fatalf("principle dist = %d (from ClaimType preference)", rep.Distribution["principle"])
	}
	if rep.Distribution["research_fact"] != 1 {
		t.Fatalf("research_fact dist = %d", rep.Distribution["research_fact"])
	}
}
