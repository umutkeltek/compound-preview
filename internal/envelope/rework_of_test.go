package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestReworkOfWorkIDShapeIsValidatedAndOptional pins the shape of the rework lineage edge at the
// envelope boundary. rework_of_work_id says "this work exists because an earlier landed work broke
// something"; a rework-rate fold reads it later, so a value that is not a resolvable work id shape
// is a silently un-foldable edge -- worse than no edge, because it looks like data.
//
// The check mirrors the loose work-id rule already used across the tree (see
// reviewartifact.validDurableWorkID): `work_` prefix, a single path element, and NOT the
// `work_live_` namespace. There is deliberately no strict hex regex, because `--work-id` admits
// arbitrary shapes and a strict rule here would reject ids the store itself accepts.
//
// Self-reference is refused in the same place: a work cannot be its own rework, and the check has
// to live where both ids are visible at once.
//
// Falsifier: dropping validateReworkOfWorkID from WorkEnvelope.Validate turns this RED with
// "must be rejected" on the first bad value.
func TestReworkOfWorkIDShapeIsValidatedAndOptional(t *testing.T) {
	mk := func(id, reworkOf string) *WorkEnvelope {
		return &WorkEnvelope{
			Header:           hdr(SVWork, id),
			Owner:            "compound",
			State:            "ready",
			SuccessPredicate: "rework_of_work_id shape is enforced",
			ReworkOfWorkID:   reworkOf,
		}
	}
	for _, bad := range []struct {
		name     string
		id       string
		reworkOf string
	}{
		{"no work prefix", "work_rework_shape", "x"},
		{"bare prefix with no body", "work_rework_shape", "work_"},
		{"live namespace is not a durable work", "work_rework_shape", "work_live_abc"},
		{"path traversal", "work_rework_shape", "a/b"},
		{"work-shaped path element", "work_rework_shape", "dir/work_abc123"},
		{"padded value is not the id it resembles", "work_rework_shape", " work_abc123 "},
		{"self reference", "work_rework_self", "work_rework_self"},
	} {
		err := mk(bad.id, bad.reworkOf).Validate()
		if err == nil {
			t.Fatalf("%s: rework_of_work_id=%q must be rejected: validator accepted an unfoldable edge", bad.name, bad.reworkOf)
		}
		if !strings.Contains(err.Error(), "rework_of_work_id") {
			t.Fatalf("%s: rework_of_work_id=%q rejection must name the field, got: %v", bad.name, bad.reworkOf, err)
		}
	}
	// Empty (not a rework) and a well-formed edge to another work both validate.
	for _, good := range []string{"", "work_abc123def456", "work_custom-shaped-id"} {
		if err := mk("work_rework_shape", good).Validate(); err != nil {
			t.Fatalf("rework_of_work_id=%q must validate: %v", good, err)
		}
	}
}

// TestReworkOfWorkIDRoundTripsAndLegacyMissingFieldLoads proves additivity: a work carrying the
// lineage edge round-trips, and a legacy envelope JSON without the field loads, re-marshals
// BYTE-IDENTICALLY, and still validates -- so introducing the field cannot perturb any stored
// work_sha256 (see internal/work.workEnvelopeSHA256).
//
// Falsifier: dropping `omitempty` from the field tag, or declaring it anywhere other than last,
// turns this RED on the byte-identity assertion.
func TestReworkOfWorkIDRoundTripsAndLegacyMissingFieldLoads(t *testing.T) {
	w := WorkEnvelope{
		Header:           hdr(SVWork, "work_rework_roundtrip"),
		Owner:            "compound",
		State:            "ready",
		SuccessPredicate: "rework_of_work_id round-trips",
		ReworkOfWorkID:   "work_earlier_landed",
	}
	if err := w.Validate(); err != nil {
		t.Fatalf("rework work should validate: %v", err)
	}
	if err := RoundTrip(&w); err != nil {
		t.Fatalf("rework_of_work_id work round-trip: %v", err)
	}
	encoded, err := json.Marshal(&w)
	if err != nil {
		t.Fatalf("marshal rework work: %v", err)
	}
	if !strings.Contains(string(encoded), `"rework_of_work_id":"work_earlier_landed"`) {
		t.Fatalf("a rework work must emit the edge on the wire, got %s", encoded)
	}

	// Written in canonical marshal order so the byte-identity assertion below is meaningful:
	// this is the exact shape json.Marshal emits for a stored envelope that predates the field.
	legacyJSON := `{"schema_version":"compound.work.v0.1","id":"work_legacy_shape","created_at":"2026-06-28T00:00:00Z","producer":{"kind":"cli","name":"compound","version":""},"ids":{},"scope":{},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"compound","state":"ready","success_predicate":"legacy remains valid","non_goals":[],"run_ids":[],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`
	if strings.Contains(legacyJSON, "rework_of_work_id") {
		t.Fatal("legacy fixture unexpectedly contains a rework_of_work_id field; test setup error")
	}
	var legacy WorkEnvelope
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("decode legacy work: %v", err)
	}
	if legacy.ReworkOfWorkID != "" {
		t.Fatalf("legacy rework_of_work_id = %q, want empty", legacy.ReworkOfWorkID)
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy work without a rework_of_work_id must remain valid: %v", err)
	}
	remarshaled, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatalf("re-marshal legacy work: %v", err)
	}
	if strings.Contains(string(remarshaled), "rework_of_work_id") {
		t.Fatalf("rework_of_work_id-less envelope must not emit the key, got %s", remarshaled)
	}
	if string(remarshaled) != legacyJSON {
		t.Fatalf("legacy envelope did not re-marshal byte-identically:\n got %s\nwant %s", remarshaled, legacyJSON)
	}
}
