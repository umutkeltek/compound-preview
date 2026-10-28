package mem

import (
	"strings"
	"testing"

	"compound/internal/candidates"
)

// A typed claim carries its ClaimType + qualifiers onto the durable claim, and a domain qualifier
// sharpens the Position/Pattern topic axis (group by domain, not the bare predicate). An unknown
// claim_type is rejected at capture.
func TestClaimTypeAndDomainTopic(t *testing.T) {
	s := newStore(t)
	c1, err := s.Capture([]Candidate{{
		Entity: "proj", Predicate: "primary_lang", Object: "go", Confidence: 0.9,
		ClaimType: "architecture", PromotionBasis: candidates.BasisExplicitConfirmation, Qualifiers: map[string]string{"domain": "stack"},
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promoteForTest(t, s, c1[0].ID, "", t0); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.AllCurrent()
	if len(cur) != 1 || cur[0].ClaimType != "architecture" || cur[0].Qualifiers["domain"] != "stack" {
		t.Fatalf("the promoted claim must carry claim_type + qualifiers; got %+v", cur)
	}
	if cur[0].Topic() != "stack" {
		t.Errorf("Topic() should prefer the domain qualifier; got %q", cur[0].Topic())
	}
	pos, _ := s.Positions()
	if len(pos) != 1 || pos[0].SchemaVersion != PositionFoldSchemaVersion || pos[0].Topic != "stack" ||
		pos[0].Predicate != "primary_lang" || pos[0].ClaimType != "architecture" ||
		!strings.HasPrefix(pos[0].ID, "proj::stack::slot-") {
		t.Errorf("position should keep the domain topic and predicate slot identity; got %+v", pos)
	}
	if _, err := s.Capture([]Candidate{{Entity: "x", Predicate: "y", Object: "z", ClaimType: "bogus"}}, t0); err == nil {
		t.Error("an unknown claim_type must be rejected at capture")
	}
}

func TestPositionIDDistinguishesOptionalSlotAxes(t *testing.T) {
	domainPredicate := Claim{Entity: "e", Predicate: "decision", Qualifiers: map[string]string{"domain": "language"}}
	typedPredicate := Claim{Entity: "e", Predicate: "language", ClaimType: "decision"}
	first := positionIDForClaim(domainPredicate)
	second := positionIDForClaim(typedPredicate)
	if first == second {
		t.Fatalf("distinct fact slots share position id %q", first)
	}
}

func TestLoadFoldedPositionsRejectsLegacyCheckpoint(t *testing.T) {
	s := newStore(t)
	if err := appendJSONL(s.positionsLog(), &Position{ID: "e::topic", Entity: "e", Topic: "topic", ActiveClaimID: "claim_old"}); err != nil {
		t.Fatal(err)
	}
	positions, err := s.LoadFoldedPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 0 {
		t.Fatalf("legacy checkpoint remained consumable: %+v", positions)
	}
}
