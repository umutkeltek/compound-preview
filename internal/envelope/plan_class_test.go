package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPlanClassEnumIsValidatedAndOptional pins the plan_class vocabulary at the envelope
// boundary. plan_class declares how much ALIGNMENT this work owes before it may run
// (oneshot -> plan_doc -> phased); the design-sketch gate in internal/work binds on the
// `phased` value, so an unvalidated free string here would silently become an ungated class.
// EMPTY is deliberately legal and means oneshot -- every envelope that predates the field
// keeps validating, mirroring the freeze rule that "absent means nothing gated".
//
// Falsifier: dropping the `plan_class` enum check from WorkEnvelope.Validate turns this RED
// with "must be rejected" on the first bad value.
func TestPlanClassEnumIsValidatedAndOptional(t *testing.T) {
	mk := func(planClass string) *WorkEnvelope {
		return &WorkEnvelope{
			Header:           hdr(SVWork, "work_plan_class_enum"),
			Owner:            "compound",
			State:            "ready",
			SuccessPredicate: "plan_class vocabulary is enforced",
			PlanClass:        planClass,
		}
	}
	for _, bad := range []string{
		"bogus",     // not in the vocabulary at all
		"Phased",    // the enum is lowercase
		"one-shot",  // near-miss spelling of oneshot
		"plan doc",  // near-miss spelling of plan_doc
		"  phased ", // enums are compared verbatim; a padded value is not the gated class
	} {
		err := mk(bad).Validate()
		if err == nil {
			t.Fatalf("plan_class=%q must be rejected: validator accepted a value outside PlanClasses", bad)
		}
		if !strings.Contains(err.Error(), "plan_class") {
			t.Fatalf("plan_class=%q rejection must name the field, got: %v", bad, err)
		}
	}
	// Every declared class validates, and so does the empty (legacy == oneshot) value.
	for _, good := range append([]string{""}, PlanClasses...) {
		if err := mk(good).Validate(); err != nil {
			t.Fatalf("plan_class=%q must validate: %v", good, err)
		}
	}
}

// TestPlanClassRoundTripsAndLegacyMissingFieldLoads proves additivity: a work carrying a
// plan_class round-trips byte-identical, and a legacy envelope JSON without the field loads,
// re-marshals byte-identically, and still validates -- so introducing the field cannot
// perturb any stored work_sha256 (see internal/work.workEnvelopeSHA256).
func TestPlanClassRoundTripsAndLegacyMissingFieldLoads(t *testing.T) {
	w := WorkEnvelope{
		Header:           hdr(SVWork, "work_plan_class_roundtrip"),
		Owner:            "compound",
		State:            "ready",
		SuccessPredicate: "plan_class round-trips",
		PlanClass:        "phased",
	}
	if err := w.Validate(); err != nil {
		t.Fatalf("phased work should validate: %v", err)
	}
	if err := RoundTrip(&w); err != nil {
		t.Fatalf("plan_class work round-trip: %v", err)
	}

	// Written in canonical marshal order so the byte-identity assertion below is meaningful:
	// this is the exact shape json.Marshal emits for a stored envelope that predates plan_class.
	legacyJSON := `{"schema_version":"compound.work.v0.1","id":"work_legacy_shape","created_at":"2026-06-28T00:00:00Z","producer":{"kind":"cli","name":"compound","version":""},"ids":{},"scope":{},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"compound","state":"ready","success_predicate":"legacy remains valid","non_goals":[],"run_ids":[],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`
	if strings.Contains(legacyJSON, "plan_class") {
		t.Fatal("legacy fixture unexpectedly contains a plan_class field; test setup error")
	}
	var legacy WorkEnvelope
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("decode legacy work: %v", err)
	}
	if legacy.PlanClass != "" {
		t.Fatalf("legacy plan_class = %q, want empty", legacy.PlanClass)
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy work without a plan_class must remain valid: %v", err)
	}
	// Additivity: re-marshaling a decoded legacy envelope must emit ZERO bytes for the new
	// field AND reproduce the original bytes exactly, so the field is hash-neutral.
	remarshaled, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatalf("re-marshal legacy work: %v", err)
	}
	if strings.Contains(string(remarshaled), "plan_class") {
		t.Fatalf("plan_class-less envelope must not emit a plan_class key, got %s", remarshaled)
	}
	if string(remarshaled) != legacyJSON {
		t.Fatalf("legacy envelope did not re-marshal byte-identically:\n got %s\nwant %s", remarshaled, legacyJSON)
	}
}
