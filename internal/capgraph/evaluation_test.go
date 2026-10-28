package capgraph

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCapabilityEvaluationsReadsOSStyleRecord(t *testing.T) {
	path := filepath.Join("..", "..", "fixtures", "capability-evaluations", "web-design-guidelines.json")
	set, err := LoadCapabilityEvaluations(path)
	if err != nil {
		t.Fatal(err)
	}
	if set.SchemaVersion != CapabilityEvaluationSetSchemaVersion || len(set.Evaluations) != 1 {
		t.Fatalf("set = %+v", set)
	}
	eval := set.Evaluations[0]
	if eval.ID != "capability-evaluation.external.vercel.web-design-guidelines" || eval.Capability != "web-design-guidelines" || eval.Verdict != "adapt" {
		t.Fatalf("eval = %+v", eval)
	}
	if set.Revision == "" || len(set.EvidenceRefs) == 0 {
		t.Fatalf("missing revision/evidence: %+v", set)
	}
}

func TestApplyCapabilityEvaluationsRecordsEvalEdgesForKnownNodesOnly(t *testing.T) {
	home, store := newStore(t)
	if err := store.Add(Node{ID: "skill:web-design-guidelines", Kind: "skill", Name: "Web Design Guidelines", Triggers: []string{"web", "design"}}); err != nil {
		t.Fatal(err)
	}
	set := CapabilityEvaluationSet{
		SchemaVersion: CapabilityEvaluationSetSchemaVersion,
		ID:            "capability_evaluations_fixture",
		AdapterID:     "os.capability-evaluations",
		SourceSystem:  "capability-evaluations",
		SourcePath:    "state/capability-evaluations",
		SourceKind:    "state_records_dir",
		Revision:      "sha256:fixture",
		EvidenceRefs:  []string{"capability_evaluation:known", "capability_evaluation:unknown"},
		Evaluations: []CapabilityEvaluation{
			{
				SchemaVersion:    1,
				ID:               "known",
				CreatedAt:        "2026-06-24T05:49:48Z",
				SourceReceipt:    "receipt:known",
				Capability:       "web-design-guidelines",
				Verdict:          "adapt",
				RubricNormalized: 0.86,
				LocalStateOnly:   true,
			},
			{
				SchemaVersion:  1,
				ID:             "unknown",
				CreatedAt:      "2026-06-24T05:50:00Z",
				SourceReceipt:  "receipt:unknown",
				Capability:     "not-installed-skill",
				Verdict:        "downgrade_to_index",
				LocalStateOnly: true,
			},
		},
	}
	report, err := ApplyCapabilityEvaluations(home, set, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "review" || report.EvaluationsRead != 2 || report.AppliedEvaluations != 1 || len(report.SkippedUnknown) != 1 {
		t.Fatalf("report = %+v", report)
	}
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdEvaluatedBy, "skill:web-design-guidelines", "eval:known") {
		t.Fatalf("expected evaluated_by edge, got %+v", edges)
	}
	if !hasEdge(edges, EdValidatedBy, "skill:web-design-guidelines", "eval:known") {
		t.Fatalf("positive evaluation should validate known capability, got %+v", edges)
	}
	if hasSurfaced(mustSurface(t, store, "web design guidelines"), "eval:known") {
		t.Fatal("evaluation records are evidence nodes and must not surface as capability offers")
	}
	if hasEdge(edges, EdEvaluatedBy, "skill:not-installed-skill", "eval:unknown") {
		t.Fatalf("unknown capability must not receive graph edge: %+v", edges)
	}
	nodeRows := readNonEmptyLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))
	replay, err := ApplyCapabilityEvaluations(home, set, t0)
	if err != nil {
		t.Fatal(err)
	}
	if replay.AppliedEvaluations != 1 {
		t.Fatalf("replay report = %+v", replay)
	}
	if got := len(readCapEdges(t, home)); got != len(edges) {
		t.Fatalf("replay appended duplicate edges: before %d after %d", len(edges), got)
	}
	if got := len(readNonEmptyLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))); got != len(nodeRows) {
		t.Fatalf("replay appended duplicate nodes: before %d after %d", len(nodeRows), got)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation import must not promote claims, stat err=%v", err)
	}
}
