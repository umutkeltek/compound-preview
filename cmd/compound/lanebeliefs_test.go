package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/lanebeliefs"
)

func TestCmdLaneBeliefsMutationRequiresExperimentalBeforeHomeAdmission(t *testing.T) {
	t.Parallel()
	binary := compoundTestBinary(t)
	root := t.TempDir()
	homePath := filepath.Join(root, "absent-home")
	cmd := exec.Command(binary, "lanebeliefs", "fold")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(string(output), "--experimental") {
		t.Fatalf("lane-belief mutation refusal = err=%v output=%q, want binary exit 2 naming --experimental", err, output)
	}
	if _, statErr := os.Stat(homePath); !os.IsNotExist(statErr) {
		t.Fatalf("lane-belief refusal created runtime home: %v", statErr)
	}
}

func laneBeliefWriteReceipt(t *testing.T, homePath, id, workID, laneID, moveType, outcome string, proof bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(homePath, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            id,
			CreatedAt:     "2026-07-09T13:00:00Z",
			Producer:      envelope.Producer{Kind: "test", Name: "fixture", LaneID: laneID},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    outcome,
		MoveType:   moveType,
		StateDelta: envelope.StateDelta{Changed: true, FilesChanged: []string{"a.go"}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Candidate: []string{}},
		Memory:     envelope.ReceiptMemory{Candidates: []string{}},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
	}
	if proof {
		rec.ProofRefs = []string{"proof:test"}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "events", id+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{"ts": "2026-07-09T13:00:00Z", "work_id": workID, "from": "running", "to": "completed", "attached": []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(homePath, "events", "work.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// The promote subverb is wired to the gate: an empty home reports the schema'd zero-report and exits
// clean (no os.Exit, no writes).
func TestCmdLaneBeliefsPromoteEmptyHome(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdLaneBeliefs([]string{"promote", "--experimental", "--json", "--home", homePath})
	})
	var rep lanebeliefs.PromoteReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse promote report: %v\n%s", err, out)
	}
	if rep.SchemaVersion == "" || rep.EvaluatedCount != 0 || rep.PromotedCount != 0 {
		t.Fatalf("empty-home promote report = %+v, want schema'd zero report", rep)
	}
}

// End-to-end CLI: fold seeds a parked belief from a lane's own receipts, a cross-lane proof-bearing
// receipt installs the mark, and `promote --json` promotes it through the gate with basis
// runtime_proof + contextual visibility.
func TestCmdLaneBeliefsPromoteEndToEnd(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	// Import subject + verifier lanes (B0 store membership).
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	subjectNode, _ := capgraph.AgentLaneNodeIDFromShortLaneID("fixture/judge")
	verifyNode, _ := capgraph.AgentLaneNodeIDFromShortLaneID("fixture/verify")
	for _, node := range []string{subjectNode, verifyNode} {
		if err := store.Add(capgraph.Node{ID: node, Kind: "agent-lane", Name: node, Weight: 0.25, EvidenceOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	// Subject: 5 completed proof-bearing receipts across 5 works → receipt-faithful + capability beliefs.
	for i := 0; i < 5; i++ {
		laneBeliefWriteReceipt(t, homePath, fmt.Sprintf("rec_subj_%d", i), fmt.Sprintf("work_%d", i), "fixture/judge", "control_plane_repair", "completed", true)
	}
	// A cross-lane verifier proof-bearing receipt on one of the subject's works → the mark.
	laneBeliefWriteReceipt(t, homePath, "rec_verify", "work_0", "fixture/verify", "control_plane_repair", "completed", true)

	if _, err := lanebeliefs.Fold(homePath, lanebeliefs.FoldOptions{}); err != nil {
		t.Fatalf("fold: %v", err)
	}

	out := captureStdout(t, func() {
		cmdLaneBeliefs([]string{"promote", "--experimental", "--json", "--home", homePath})
	})
	var rep lanebeliefs.PromoteReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse promote report: %v\n%s", err, out)
	}
	if rep.PromotedCount == 0 {
		t.Fatalf("expected at least one promotion via the cross-lane mark: %+v", rep)
	}
	sawRuntimeProof := false
	for _, res := range rep.Results {
		if res.Disposition != lanebeliefs.DispositionPromoted {
			continue
		}
		if res.PromotionBasis != candidates.BasisRuntimeProof || res.Visibility != "contextual" {
			t.Fatalf("promoted result basis/visibility = %q/%q, want runtime_proof/contextual: %+v", res.PromotionBasis, res.Visibility, res)
		}
		if !res.HasCrossLaneMark {
			t.Fatalf("standing-raising promotion must carry a cross-lane mark: %+v", res)
		}
		sawRuntimeProof = true
	}
	if !sawRuntimeProof {
		t.Fatalf("no runtime_proof promotion in report: %+v", rep)
	}

	// list must now surface the promoted belief (state promoted).
	listOut := captureStdout(t, func() {
		cmdLaneBeliefs([]string{"list", "--json", "--home", homePath})
	})
	if !strings.Contains(listOut, "promoted") {
		t.Fatalf("list should show the promoted belief:\n%s", listOut)
	}
}
