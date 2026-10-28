package capgraph_test

import (
	"testing"
	"time"

	"compound/internal/capgraph"
)

func TestReferenceBearingCapabilityOffersPreserveReadAndBoundary(t *testing.T) {
	home := t.TempDir()
	manifest := capgraph.SkillManifest{
		SchemaVersion: capgraph.SkillManifestSchemaVersion,
		ID:            "skill_manifest_reference_constraints",
		AdapterID:     "fixture.reference-constraints",
		SourceSystem:  "fixture",
		ObservedAt:    "2026-07-12T12:00:00Z",
		SourceRef: capgraph.SkillSourceRef{
			Path:        "fixtures/reference-constraints",
			Kind:        "synthetic_fixture",
			Owner:       "tests",
			SourceClass: "synthetic",
			Revision:    "sha256:reference-constraints",
		},
		EvidenceRefs: []string{"fixture://reference-constraints"},
		Skills: []capgraph.SkillManifestEntry{
			{
				ID:       "procedure:reference-drilldown",
				Kind:     "procedure",
				Name:     "Procedure reference drilldown",
				Status:   "active",
				Triggers: []string{"referenceconstraint"},
				Read:     []string{"fixtures/procedure.md"},
				Boundary: []string{"procedure context only; no execution"},
			},
			{
				ID:       "doctrine:reference-drilldown",
				Kind:     "doctrine",
				Name:     "Doctrine reference drilldown",
				Status:   "active",
				Triggers: []string{"referenceconstraint"},
				Read:     []string{"fixtures/doctrine.md"},
				Boundary: []string{"doctrine context only; no mutation"},
			},
			{
				ID:       "tool:reference-unrelated",
				Kind:     "tool",
				Name:     "Unrelated reference-free tool",
				Status:   "active",
				Triggers: []string{"referenceconstraint"},
			},
		},
	}
	if _, err := capgraph.ApplySkillManifest(home, manifest, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	offers, err := store.Surface("referenceconstraint")
	if err != nil {
		t.Fatal(err)
	}

	assertReferenceConstraints(t, offers, "procedure:reference-drilldown", "fixtures/procedure.md", "procedure context only; no execution")
	assertReferenceConstraints(t, offers, "doctrine:reference-drilldown", "fixtures/doctrine.md", "doctrine context only; no mutation")
	unrelated := referenceConstraintOffer(t, offers, "tool:reference-unrelated")
	if len(unrelated.Constraints.Required) != 0 || len(unrelated.Constraints.Conflicts) != 0 {
		t.Fatalf("reference-free node gained invented constraints: %+v", unrelated.Constraints)
	}
}

func assertReferenceConstraints(t *testing.T, offers []capgraph.Surfaced, id, readRef, boundary string) {
	t.Helper()
	offer := referenceConstraintOffer(t, offers, id)
	if !hasReferenceConstraint(offer.Constraints.Required, "read", readRef, "") {
		t.Fatalf("%s required constraints = %+v, want read %q", id, offer.Constraints.Required, readRef)
	}
	if !hasReferenceConstraint(offer.Constraints.Conflicts, "boundary", "", boundary) {
		t.Fatalf("%s conflict constraints = %+v, want boundary %q", id, offer.Constraints.Conflicts, boundary)
	}
}

func referenceConstraintOffer(t *testing.T, offers []capgraph.Surfaced, id string) capgraph.Surfaced {
	t.Helper()
	for _, offer := range offers {
		if offer.Node.ID == id {
			return offer
		}
	}
	t.Fatalf("missing surfaced offer %s: %+v", id, offers)
	return capgraph.Surfaced{}
}

func hasReferenceConstraint(values []capgraph.SurfacingConstraint, kind, ref, detail string) bool {
	for _, value := range values {
		if value.Kind != kind {
			continue
		}
		if ref != "" && value.Ref != ref {
			continue
		}
		if detail != "" && value.Detail != detail {
			continue
		}
		return true
	}
	return false
}
