package mem

import "testing"

// Richer EdgeType: the entity-relation taxonomy now matches the Gen-1 memory.EdgeType
// (refines/supports/mentions/owns/part_of in addition to relates/contradicts/depends_on) — schema
// parity, the last capability the converging memory stack needed on the spine. An unknown kind is
// still rejected (a relation kind is typed).
func TestRicherEdgeTypesAccepted(t *testing.T) {
	s := newStore(t)
	for _, k := range []string{"refines", "supports", "mentions", "owns", "part_of", "relates", "contradicts", "depends_on"} {
		e, err := s.Relate("entity:a", k, "entity:b", 0.8, t0)
		if err != nil {
			t.Errorf("relation kind %q must be accepted (schema parity with Gen-1 EdgeType); got %v", k, err)
		}
		if e.Kind != k {
			t.Errorf("edge should record kind %q; got %q", k, e.Kind)
		}
	}
	if _, err := s.Relate("entity:a", "telepathy", "entity:b", 0.8, t0); err == nil {
		t.Error("an unknown relation kind must still be rejected")
	}
}
