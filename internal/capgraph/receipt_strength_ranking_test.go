package capgraph

import (
	"strconv"
	"testing"
)

func TestDeclaredUseIsWeakerBoundedAndCannotCreateRelevance(t *testing.T) {
	_, s := newStore(t)
	const nodeID = "skill:declared"
	if err := s.Add(Node{ID: nodeID, Kind: "skill", Name: "Declared", Triggers: []string{"receipt"}}); err != nil {
		t.Fatal(err)
	}
	before := surfaced(s, "receipt")[nodeID]
	if before <= 0 {
		t.Fatal("fixture must be relevant before feedback")
	}
	var first float64
	for i := 0; i < 404; i++ {
		if err := s.AddEdge(Edge{Kind: EdDeclaredUsedBy, From: "context:receipt", To: nodeID,
			Confidence: 1, Weight: 2, Provenance: "verified_lane", SourceReceipt: "rec_rank_" + strconv.Itoa(i)}, t0); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = surfaced(s, "receipt")[nodeID]
		}
	}
	after := surfaced(s, "receipt")[nodeID]
	if first <= before || after != first || after-before > maxDeclaredUseAdd || maxDeclaredUseAdd >= maxValidatedByAdd {
		t.Fatalf("declarations did not remain weak/bounded: before=%v first=%v after=%v", before, first, after)
	}
	if score := surfaced(s, "unrelated")[nodeID]; score != 0 {
		t.Fatalf("declarations created irrelevant offer: %v", score)
	}
}
