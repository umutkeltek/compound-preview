package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/capgraph"
	"compound/internal/work"
)

const workLaneCLIRefuseSubEnv = "COMPOUND_WORK_LANE_CLI_REFUSE_SUB"

// writeWorkLaneFixture imports one agent-lane node whose id is exactly the short-id mapping of
// shortID, so `claim --lane <shortID>` resolves through the store-backed no-guessing resolver.
// registry.ts content only feeds the revision hash; the overlay derives the node.
func writeWorkLaneFixture(t *testing.T, shortID string) {
	t.Helper()
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "registry.ts")
	overlayPath := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(registryPath, []byte(`export const LANES = [];`), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	// role.models["h1"] and lane.route are both shortID; role.quality_tier "unknown" — so the
	// imported tuple is {h1, shortID, shortID, unknown} == AgentLaneTupleFromShortLaneID(shortID).
	overlay := `{
  "roles": [
    {
      "id": "lane-fast",
      "family": "lane",
      "description": "synthetic fast lane",
      "capability_class": "synthetic-task",
      "quality_tier": "unknown",
      "emit": ["h1"],
      "models": {"h1": "` + shortID + `"},
      "tendencies": ["falsifier-first"],
      "validated_by": ["eval:lane-fast"]
    }
  ],
  "lanes": [
    {"id": "lane-fast", "harness": "h1", "route": "` + shortID + `", "cost_class": "synthetic"}
  ]
}`
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	captureStdout(t, func() {
		cmdCapability([]string{"import-agent-lanes", "--registry", registryPath, "--overlay", overlayPath})
	})
}

// The CLI claim --lane path stamps the canonical agent-lane node id onto the close-minted
// receipt AND transition, byte-identical to the exported AgentLaneNodeIDFromShortLaneID mapper.
func TestWorkClaimLaneStampsCanonicalLaneIDThroughCLI(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	const shortID = "h1/fast"
	writeWorkLaneFixture(t, shortID)
	wantLane, ok := capgraph.AgentLaneNodeIDFromShortLaneID(shortID)
	if !ok {
		t.Fatalf("short id %q did not map", shortID)
	}

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := store.Create(work.CreateInput{Owner: "operator", SuccessPredicate: "cli lane proof exists"}, cliTestNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	captureStdout(t, func() {
		cmdWork([]string{"claim", w.ID, "--by", "some-actor", "--lane", shortID})
	})
	captureStdout(t, func() {
		cmdWork([]string{"advance", w.ID, "--to", "running", "--by", "some-actor"})
	})
	closeOut := captureStdout(t, func() {
		cmdWork([]string{"close", w.ID, "--outcome", "completed", "--move-type", "outward_delta", "--summary", "cli lane", "--proof", "proof:fixture", "--by", "some-actor"})
	})

	brace := strings.Index(closeOut, "{")
	if brace < 0 {
		t.Fatalf("close output has no JSON block:\n%s", closeOut)
	}
	var res struct {
		Receipt struct {
			Producer struct {
				LaneID string `json:"lane_id"`
			} `json:"producer"`
		} `json:"receipt"`
		Transition struct {
			Producer struct {
				LaneID string `json:"lane_id"`
			} `json:"producer"`
		} `json:"transition"`
	}
	if err := json.Unmarshal([]byte(closeOut[brace:]), &res); err != nil {
		t.Fatalf("parse close JSON: %v\n%s", err, closeOut)
	}
	if res.Receipt.Producer.LaneID != wantLane {
		t.Fatalf("receipt lane_id = %q, want canonical %q", res.Receipt.Producer.LaneID, wantLane)
	}
	if res.Transition.Producer.LaneID != wantLane {
		t.Fatalf("transition lane_id = %q, want canonical %q", res.Transition.Producer.LaneID, wantLane)
	}
}

// The CLI claim --lane refuses a bare harness (`codex`) fail-closed with an error naming the
// valid ref forms — even when real lanes are imported — exiting non-zero and leaving the
// envelope unclaimed (no-guessing at the CLI gate).
func TestWorkClaimLaneRefusesBareHarnessThroughCLI(t *testing.T) {
	if os.Getenv(workLaneCLIRefuseSubEnv) == "1" {
		cmdWork([]string{"claim", os.Getenv("COMPOUND_WORK_LANE_TEST_WORKID"), "--by", "some-actor", "--lane", "codex"})
		return
	}

	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	writeWorkLaneFixture(t, "h1/fast") // a real lane exists; codex still must not resolve

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := store.Create(work.CreateInput{Owner: "operator", SuccessPredicate: "cli refuse proof exists"}, cliTestNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkClaimLaneRefusesBareHarnessThroughCLI$")
	cmd.Env = append(os.Environ(),
		workLaneCLIRefuseSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_WORK_LANE_TEST_WORKID="+w.ID,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("claim --lane codex should exit non-zero, got err=%v output=%s", err, output)
	}
	if !strings.Contains(string(output), "does not resolve") || !strings.Contains(string(output), "short lane id") {
		t.Fatalf("claim --lane codex output missing no-guessing refusal:\n%s", output)
	}
	// Envelope left unclaimed by the refused CLI claim.
	got, err := store.Get(w.ID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if got.State != "ready" {
		t.Fatalf("refused claim mutated state to %q, want ready", got.State)
	}
}
