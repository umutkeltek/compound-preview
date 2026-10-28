package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/lanebeliefs"
	"compound/internal/mem"
)

func TestCmdLaneBeliefsFoldDryRunAndListJSON(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	seedCLILaneNode(t, homePath, "fixture/judge")
	for i := 0; i < lanebeliefs.MIN_LANE_RECEIPTS; i++ {
		writeCLILaneBeliefReceiptEvent(t, homePath, fmt.Sprintf("rec_cli_%02d", i), "work_cli", "fixture/judge")
	}

	dryOut := captureStdout(t, func() { cmdLaneBeliefs([]string{"fold", "--dry-run", "--json"}) })
	var dry lanebeliefs.FoldReport
	if err := json.Unmarshal([]byte(dryOut), &dry); err != nil {
		t.Fatalf("parse dry-run fold: %v\n%s", err, dryOut)
	}
	// Each completed proof-bearing receipt yields two beliefs (capability + tendency).
	if !dry.DryRun || dry.CandidateCount != 2 || dry.NewObservationCount != 2 {
		t.Fatalf("dry-run report = %+v, want two unwritten candidates", dry)
	}
	listEmptyOut := captureStdout(t, func() { cmdLaneBeliefs([]string{"list", "--json"}) })
	var listEmpty lanebeliefs.ListReport
	if err := json.Unmarshal([]byte(listEmptyOut), &listEmpty); err != nil {
		t.Fatalf("parse empty list: %v\n%s", err, listEmptyOut)
	}
	if listEmpty.CandidateCount != 0 {
		t.Fatalf("dry-run wrote candidates: %+v", listEmpty)
	}

	foldOut := captureStdout(t, func() { cmdLaneBeliefs([]string{"fold", "--experimental", "--json"}) })
	var folded lanebeliefs.FoldReport
	if err := json.Unmarshal([]byte(foldOut), &folded); err != nil {
		t.Fatalf("parse fold: %v\n%s", err, foldOut)
	}
	if folded.DryRun || folded.CandidateCount != 2 || folded.NewObservationCount != 2 {
		t.Fatalf("fold report = %+v, want two written candidates", folded)
	}
	listOut := captureStdout(t, func() { cmdLaneBeliefs([]string{"list", "--json"}) })
	var list lanebeliefs.ListReport
	if err := json.Unmarshal([]byte(listOut), &list); err != nil {
		t.Fatalf("parse list: %v\n%s", err, listOut)
	}
	if list.CandidateCount != 2 || len(list.Candidates) != 2 {
		t.Fatalf("list = %+v, want two lane beliefs", list)
	}
	predicates := map[string]bool{}
	for _, c := range list.Candidates {
		predicates[c.Predicate] = true
	}
	if !predicates[mem.BeliefPredicateTendsTo] || !predicates[mem.BeliefPredicateCapableOf] {
		t.Fatalf("list = %+v, want one tendency and one capability belief", list)
	}
}

func seedCLILaneNode(t *testing.T, homePath, shortID string) {
	t.Helper()
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	nodeID, ok := capgraph.AgentLaneNodeIDFromShortLaneID(shortID)
	if !ok {
		t.Fatalf("short lane id %q has no node mapping", shortID)
	}
	if err := store.Add(capgraph.Node{ID: nodeID, Kind: "agent-lane", Name: nodeID, Weight: 0.25, EvidenceOnly: true}); err != nil {
		t.Fatalf("seed lane node %q: %v", nodeID, err)
	}
}

func writeCLILaneBeliefReceiptEvent(t *testing.T, homePath, receiptID, workID, laneID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(homePath, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	created := cliTestNow.Format("2006-01-02T15:04:05Z07:00")
	receipt := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            receiptID,
			CreatedAt:     created,
			Producer:      envelope.Producer{Kind: "test", Name: "fixture", LaneID: laneID},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    "completed",
		MoveType:   "control_plane_repair",
		StateDelta: envelope.StateDelta{Changed: true, FilesChanged: []string{"x.go"}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Candidate: []string{}},
		Memory:     envelope.ReceiptMemory{Candidates: []string{}},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs:  []string{"proof:test"},
	}
	writeCLIJSONFile(t, filepath.Join(homePath, "events", receiptID+".json"), receipt)
	line := map[string]any{"ts": created, "work_id": workID, "from": "running", "to": "completed", "attached": []string{receiptID}}
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(homePath, "events", "work.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func writeCLIJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
