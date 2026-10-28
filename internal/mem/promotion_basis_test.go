package mem

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

// Falsifier F1 (per packet work_2742cffbeb5f, scoped to the actual raw-intake
// door): a candidate arriving through the CLI's JSON-parsing boundary
// (StructuredExtractor) carrying a system-derived promotion_basis is stripped
// at write seam — the candidate captured via Capture(...) lands with an empty
// basis, the original is recorded under Qualifiers[promotion_basis_stripped],
// and the existing promote gate at mem.go:680 rejects promote with the
// canonical "cannot promote without valid promotion_basis" error. Subtests
// cover every system-derived basis.
func TestStructuredExtractorStripsSystemDerivedPromotionBasis(t *testing.T) {
	bases := []string{
		candidates.BasisRuntimeProof,
		candidates.BasisCuratedPage,
		candidates.BasisDecisionRecord,
		candidates.BasisTrustedDeterministic,
	}
	for _, basis := range bases {
		basis := basis
		t.Run(basis, func(t *testing.T) {
			home := t.TempDir()
			s, err := Open(home)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)
			trace := map[string]any{
				"candidates": []map[string]any{{
					"entity":           "project:compound",
					"predicate":        "envelope_" + basis,
					"object":           "v",
					"observation_date": now.Format(time.RFC3339),
					"confidence":       0.9,
					"derivation":       "deterministic",
					"claim_type":       "decision",
					"promotion_basis":  basis,
					"source_trace_ref": "t_" + basis,
					"snippet_hash":     "h_" + basis,
				}},
				"relations": []any{},
			}
			bytes, err := json.Marshal(trace)
			if err != nil {
				t.Fatal(err)
			}
			ext, err := StructuredExtractor{}.Extract(bytes)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got := ext.Candidates[0].PromotionBasis; got != "" {
				t.Fatalf("extracted basis = %q, want empty (stripped at parser)", got)
			}
			if got := ext.Candidates[0].Qualifiers[PromotionBasisStrippedQualifier]; got != basis {
				t.Fatalf("extracted stripped qualifier = %q, want %q", got, basis)
			}
			captured, err := s.Capture(ext.Candidates, now)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if len(captured) != 1 {
				t.Fatalf("captured len = %d, want 1", len(captured))
			}
			if captured[0].PromotionBasis != "" {
				t.Fatalf("stored basis = %q, want empty", captured[0].PromotionBasis)
			}
			if got := captured[0].Qualifiers[PromotionBasisStrippedQualifier]; got != basis {
				t.Fatalf("stripped qualifier after capture = %q, want %q", got, basis)
			}
			// Re-read from disk to confirm the persisted candidate carries
			// the stripped value, not just the in-memory return value.
			persisted, err := s.getCandidate(captured[0].ID)
			if err != nil {
				t.Fatalf("read persisted candidate: %v", err)
			}
			if persisted.PromotionBasis != "" {
				t.Fatalf("persisted basis = %q, want empty", persisted.PromotionBasis)
			}
			_, err = s.Adjudicate(captured[0].ID, "promote", "work_strip_f1", now)
			if err == nil {
				t.Fatal("promote must fail when intake-supplied basis was stripped")
			}
			if !strings.Contains(err.Error(), "cannot promote without valid promotion_basis") {
				t.Fatalf("error = %q, want ...cannot promote without valid promotion_basis", err.Error())
			}
			claims, err := s.loadClaims()
			if err != nil {
				t.Fatalf("load claims: %v", err)
			}
			if len(claims) != 0 {
				t.Fatalf("refused promote must not append claims; got %d", len(claims))
			}
		})
	}
}

// Negative mirror: a bare []Candidate JSON shape (the alternate parser form)
// also has its system-derived basis stripped. The two forms share the same
// compiler path; explicit assertion keeps the contract visible.
func TestStructuredExtractorBareListStripsSystemDerivedPromotionBasis(t *testing.T) {
	now := time.Date(2026, 7, 9, 9, 1, 0, 0, time.UTC)
	trace := []map[string]any{{
		"entity":           "project:compound",
		"predicate":        "intake_bare",
		"object":           "v",
		"observation_date": now.Format(time.RFC3339),
		"confidence":       0.9,
		"derivation":       "deterministic",
		"claim_type":       "decision",
		"promotion_basis":  candidates.BasisRuntimeProof,
		"source_trace_ref": "t_bare",
		"snippet_hash":     "h_bare",
	}}
	bytes, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	ext, err := StructuredExtractor{}.Extract(bytes)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if ext.Candidates[0].PromotionBasis != "" {
		t.Fatalf("bare-form extracted basis = %q, want empty", ext.Candidates[0].PromotionBasis)
	}
}

// Falsifier F2 (negative path only, per packet): intake with
// promotion_basis=explicit_confirmation is preserved through the
// StructuredExtractor (it is self-attested), and Adjudicate("promote")
// without intent.ExplicitConfirmationRef still fails with the existing
// "explicit confirmation requires governed confirmation evidence" guard. The
// positive path is exercised by TestCaptureExplicitConfirmationPositive
// below (governed confirmation door produces a claim with
// BasisExplicitConfirmation).
func TestStructuredExtractorPreservesExplicitConfirmationBasis(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 9, 9, 2, 0, 0, time.UTC)
	trace := map[string]any{
		"candidates": []map[string]any{{
			"entity":           "operator",
			"predicate":        "preference.communication",
			"object":           "prefer concise updates",
			"observation_date": now.Format(time.RFC3339),
			"confidence":       0.9,
			"derivation":       "extracted",
			"claim_type":       "preference",
			"qualifiers":       map[string]string{"domain": "comm"},
			"promotion_basis":  candidates.BasisExplicitConfirmation,
			"source_trace_ref": "t_efc",
			"snippet_hash":     "h_efc",
		}},
		"relations": []any{},
	}
	bytes, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	ext, err := StructuredExtractor{}.Extract(bytes)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if ext.Candidates[0].PromotionBasis != candidates.BasisExplicitConfirmation {
		t.Fatalf("explicit_confirmation must survive the parser strip; got %q",
			ext.Candidates[0].PromotionBasis)
	}
	if _, flagged := ext.Candidates[0].Qualifiers[PromotionBasisStrippedQualifier]; flagged {
		t.Fatalf("explicit_confirmation must not be flagged as stripped: %+v",
			ext.Candidates[0])
	}
	captured, err := s.Capture(ext.Candidates, now)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if captured[0].PromotionBasis != candidates.BasisExplicitConfirmation {
		t.Fatalf("stored basis = %q, want preserved", captured[0].PromotionBasis)
	}
	if _, flagged := captured[0].Qualifiers[PromotionBasisStrippedQualifier]; flagged {
		t.Fatalf("explicit_confirmation must not be flagged as stripped after capture: %+v",
			captured[0])
	}
	_, err = s.Adjudicate(captured[0].ID, "promote", "work_efc_neg", now)
	if err == nil {
		t.Fatal("ordinary adjudication accepted unproven explicit confirmation")
	}
	if !strings.Contains(err.Error(), "confirmation") {
		t.Fatalf("error = %q, want ...confirmation...", err.Error())
	}
}

// F2 positive path: a captured explicit_confirmation candidate promoted
// through the governed confirmation door does promote and stamps
// BasisExplicitConfirmation onto the resulting claim. This proves the
// self-attested class is not destroyed by the intake strip.
func TestCaptureExplicitConfirmationPositiveGovernedDoorStillWorks(t *testing.T) {
	s := newStore(t)
	seeded := []Candidate{{
		Entity:         "operator",
		Predicate:      "preference.communication",
		Object:         "prefer concise updates",
		Confidence:     0.9,
		Derivation:     "extracted",
		ClaimType:      "preference",
		Qualifiers:     map[string]string{"domain": "comm"},
		PromotionBasis: candidates.BasisExplicitConfirmation,
		SourceTraceRef: "t_efc_pos",
		SnippetHash:    "h_efc_pos",
	}}
	captured, err := s.Capture(seeded, t0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if _, err := s.Adjudicate(captured[0].ID, "park", "work_park", t0); err != nil {
		t.Fatalf("park: %v", err)
	}
	conf, err := s.RecordOperatorConfirmation(captured[0].ID,
		"Keep this preference.", "operator:envelope", t0)
	if err != nil {
		t.Fatalf("record operator confirmation: %v", err)
	}
	res, err := s.AdjudicateExplicitConfirmation(captured[0].ID, conf.ID,
		"work_efc_pos", t0)
	if err != nil {
		t.Fatalf("explicit-confirmation promote: %v", err)
	}
	if res.Claim == nil {
		t.Fatal("positive path must produce a claim")
	}
	if res.Claim.PromotionBasis != candidates.BasisExplicitConfirmation {
		t.Fatalf("claim basis = %q, want %q",
			res.Claim.PromotionBasis, candidates.BasisExplicitConfirmation)
	}
}

// Negative mirror on Capture direct path: the doctrinally important guarantee
// is that direct Capture does NOT strip (system adapters and tests carry
// vetted bases through that door unchanged). This guardrail would catch any
// future regression that moves the strip back into Capture.
func TestCaptureDirectDoesNotStripVettedBases(t *testing.T) {
	s := newStore(t)
	bases := []string{
		candidates.BasisRuntimeProof,
		candidates.BasisCuratedPage,
		candidates.BasisDecisionRecord,
		candidates.BasisTrustedDeterministic,
		candidates.BasisExplicitConfirmation,
	}
	for _, basis := range bases {
		basis := basis
		t.Run(basis, func(t *testing.T) {
			c := Candidate{
				Entity:         "project:compound",
				Predicate:      "direct_" + basis,
				Object:         "v",
				Confidence:     0.9,
				Derivation:     "deterministic",
				ClaimType:      "decision",
				PromotionBasis: basis,
				SourceTraceRef: "t_" + basis,
				SnippetHash:    "h_" + basis,
			}
			captured, err := s.Capture([]Candidate{c}, t0)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if captured[0].PromotionBasis != basis {
				t.Fatalf("direct Capture stripped %q → %q; capture must preserve vetted bases", basis, captured[0].PromotionBasis)
			}
			if _, flagged := captured[0].Qualifiers[PromotionBasisStrippedQualifier]; flagged {
				t.Fatalf("direct Capture must not flag %q as stripped: %+v", basis, captured[0])
			}
		})
	}
}

// Unit coverage for the classifier so the doctrine is testable in isolation
// (both the spec values AND that an unknown / self-attested basis is left
// alone).
func TestSystemDerivedPromotionBasisClassifier(t *testing.T) {
	cases := map[string]bool{
		candidates.BasisRuntimeProof:         true,
		candidates.BasisCuratedPage:          true,
		candidates.BasisDecisionRecord:       true,
		candidates.BasisTrustedDeterministic: true,
		candidates.BasisExplicitConfirmation: false, // self-attested
		"":                                   false,
		"garbage":                            false,
	}
	for basis, want := range cases {
		basis, want := basis, want
		t.Run(basis, func(t *testing.T) {
			if got := systemDerivedPromotionBasis(basis); got != want {
				t.Fatalf("systemDerivedPromotionBasis(%q) = %v, want %v", basis, got, want)
			}
		})
	}
}

// Unit coverage for the strip mutator: it must shallow-copy the Qualifiers
// map (the caller's map stays untouched), set the audit qualifier, and zero
// the PromotionBasis. Empty and self-attested bases are no-ops.
func TestStripIntakePromotionBasisShallowCopy(t *testing.T) {
	callerQ := map[string]string{"domain": "scope", "owner": "tester"}
	c := &Candidate{
		PromotionBasis: candidates.BasisRuntimeProof,
		Qualifiers:     callerQ,
	}
	stripIntakePromotionBasis(c)
	if c.PromotionBasis != "" {
		t.Fatalf("basis = %q, want empty", c.PromotionBasis)
	}
	if c.Qualifiers[PromotionBasisStrippedQualifier] != candidates.BasisRuntimeProof {
		t.Fatalf("stripped qualifier = %q, want %q",
			c.Qualifiers[PromotionBasisStrippedQualifier], candidates.BasisRuntimeProof)
	}
	// Original caller's map must be untouched: stripping is non-mutating
	// for callers that reuse candidate literals.
	if callerQ[PromotionBasisStrippedQualifier] != "" {
		t.Fatalf("caller's Qualifiers map was mutated: %+v", callerQ)
	}

	// Empty basis: no-op.
	c2 := &Candidate{PromotionBasis: ""}
	stripIntakePromotionBasis(c2)
	if c2.PromotionBasis != "" {
		t.Fatalf("empty basis should be no-op; got %q", c2.PromotionBasis)
	}
	if c2.Qualifiers != nil {
		t.Fatalf("empty basis should leave Qualifiers nil; got %+v", c2.Qualifiers)
	}

	// Self-attested basis: preserved unchanged.
	c3 := &Candidate{
		PromotionBasis: candidates.BasisExplicitConfirmation,
		Qualifiers:     map[string]string{"source": "operator"},
	}
	stripIntakePromotionBasis(c3)
	if c3.PromotionBasis != candidates.BasisExplicitConfirmation {
		t.Fatalf("explicit_confirmation must be preserved; got %q", c3.PromotionBasis)
	}
	if _, flagged := c3.Qualifiers[PromotionBasisStrippedQualifier]; flagged {
		t.Fatalf("explicit_confirmation must NOT be flagged stripped: %+v", c3)
	}
}
