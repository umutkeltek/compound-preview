package capgraph

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReceiptDeclarationsDoNotMintCapabilityValidation(t *testing.T) {
	for _, label := range []string{"ordinary", "verified", "oracle", "human-reviewed", "human_reviewed"} {
		t.Run(label, func(t *testing.T) {
			home, s := newStore(t)
			for _, id := range []string{"skill:one", "skill:two", "skill:offered"} {
				if err := s.Add(Node{ID: id, Kind: "skill", Name: id, Triggers: []string{"receipt"}}); err != nil {
					t.Fatal(err)
				}
			}
			rec := testReceipt("rec_declared", "completed", true,
				[]string{"capability://skill:one", "capability://skill:two"}, nil,
				[]string{"capability://skill:offered"})
			rec.Producer.Name = label
			rec.Cost.ModelCalls = []string{label}
			rec.ProofRefs = []string{"artifact:" + label}
			writeReceipt(t, home, rec)
			report, err := s.ConsumeReceipts(home, t0)
			if err != nil {
				t.Fatal(err)
			}
			if report.AppliedValidatedEdges != 0 {
				t.Fatalf("declarations minted validated evidence: %+v", report)
			}
			edges := readCapEdges(t, home)
			for _, id := range []string{"skill:one", "skill:two"} {
				if !hasEdge(edges, "declared_used_by", id, rec.ID) {
					t.Fatalf("declared use lost for %s: %+v", id, edges)
				}
			}
			for _, e := range edges {
				if e.Kind == EdValidatedBy || e.Weight > 1 || e.Provenance != "receipt_declared" ||
					e.From == "skill:offered" || e.To == "skill:offered" {
					t.Fatalf("offer/label/declaration overstated evidence: %+v", e)
				}
			}
			replay, err := s.ConsumeReceipts(home, t0)
			if err != nil || replay.AppliedEdges != 0 {
				t.Fatalf("receipt replay duplicated evidence: %+v %v", replay, err)
			}
		})
	}
}

func TestLegacyReceiptValidationReadsAsDeclarationWithoutRewriting(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:legacy", Kind: "skill", Name: "Legacy", Triggers: []string{"receipt"}}); err != nil {
		t.Fatal(err)
	}
	legacy := Edge{Kind: EdValidatedBy, From: "skill:legacy", To: "rec_legacy", Confidence: 1,
		Weight: 2, Provenance: "verified_lane", SourceReceipt: "compound://events/rec_legacy"}
	if err := s.AddEdge(legacy, t0); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.edgesLog())
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.Edges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Kind != "declared_used_by" || edges[0].Weight > 1 || edges[0].Provenance != "receipt_declared" {
		t.Fatalf("legacy receipt labels still imply verification: %+v", edges)
	}
	encoded, err := json.Marshal(FeedbackCountsByKind(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"declared_used_by":1`) || !strings.Contains(string(encoded), `"validated_by":0`) {
		t.Fatalf("operator projection overstated receipt evidence: %s", encoded)
	}
	after, err := os.ReadFile(s.edgesLog())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read projection rewrote historical bytes: %v", err)
	}
}

func TestReviewedValidationRemainsDistinctFromReceiptDeclarations(t *testing.T) {
	_, s := newStore(t)
	reviewed := Edge{Kind: EdValidatedBy, From: "skill:reviewed", To: "eval:reviewed",
		Confidence: 1, Weight: 2, SourceReceipt: "artifact:reviewed-evaluation"}
	if err := s.AddEdge(reviewed, t0); err != nil {
		t.Fatal(err)
	}
	edges, err := s.Edges()
	if err != nil || len(edges) != 1 || edges[0].Kind != EdValidatedBy || edges[0].Weight != 2 {
		t.Fatalf("reviewed evidence was conflated with declarations: %+v %v", edges, err)
	}
}

func TestCallerLabelsCannotBoostNegativeReceiptFeedback(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:declared", Kind: "skill", Name: "Declared", Triggers: []string{"receipt"}}); err != nil {
		t.Fatal(err)
	}
	rec := testReceipt("rec_rejected", "failed", true, []string{"capability://skill:declared"},
		[]string{"capability://skill:declared"}, nil)
	rec.Producer.Name = "oracle-verified-human-reviewed"
	rec.Skills.FalseTriggered = []string{"capability://skill:declared"}
	writeReceipt(t, home, rec)
	if _, err := s.ConsumeReceipts(home, t0); err != nil {
		t.Fatal(err)
	}
	for _, e := range readCapEdges(t, home) {
		if e.Kind == EdValidatedBy || e.Kind == "declared_used_by" || e.Weight > 1 || e.Provenance != "receipt_declared" {
			t.Fatalf("rejected result or caller label earned positive/verified credit: %+v", e)
		}
	}
}
