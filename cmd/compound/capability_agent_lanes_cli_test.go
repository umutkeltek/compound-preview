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
	"compound/internal/home"
)

const capabilityAgentLanesCLIRejectSubEnv = "COMPOUND_CAPABILITY_AGENT_LANES_CLI_REJECT_SUB"

func TestCapabilityImportAgentLanesCLIIdempotent(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	registryPath, overlayPath := writeCapabilityAgentLaneFixtures(t, false)

	firstOut := captureStdout(t, func() {
		cmdCapability([]string{"import-agent-lanes", "--registry", registryPath, "--overlay", overlayPath})
	})
	var first capgraph.AgentLaneImportReport
	if err := json.Unmarshal([]byte(firstOut), &first); err != nil {
		t.Fatalf("parse first report: %v\n%s", err, firstOut)
	}
	if first.Status != "ok" || first.AppliedNodes == 0 || first.AppliedEdges == 0 {
		t.Fatalf("first report = %+v, want applied nodes and edges", first)
	}

	secondOut := captureStdout(t, func() {
		cmdCapability([]string{"import-agent-lanes", "--registry", registryPath, "--overlay", overlayPath})
	})
	var second capgraph.AgentLaneImportReport
	if err := json.Unmarshal([]byte(secondOut), &second); err != nil {
		t.Fatalf("parse second report: %v\n%s", err, secondOut)
	}
	if second.Status != "ok" || second.AppliedNodes != 0 || second.AppliedEdges != 0 {
		t.Fatalf("second report = %+v, want idempotent zero applied nodes/edges", second)
	}
}

func TestCapabilityImportAgentLanesCLIRejectsUnknownTendency(t *testing.T) {
	if os.Getenv(capabilityAgentLanesCLIRejectSubEnv) == "1" {
		cmdCapability([]string{
			"import-agent-lanes",
			"--registry", os.Getenv("COMPOUND_AGENT_LANES_TEST_REGISTRY"),
			"--overlay", os.Getenv("COMPOUND_AGENT_LANES_TEST_OVERLAY"),
		})
		return
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	registryPath, overlayPath := writeCapabilityAgentLaneFixtures(t, true)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCapabilityImportAgentLanesCLIRejectsUnknownTendency$")
	cmd.Env = append(os.Environ(),
		capabilityAgentLanesCLIRejectSubEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_AGENT_LANES_TEST_REGISTRY="+registryPath,
		"COMPOUND_AGENT_LANES_TEST_OVERLAY="+overlayPath,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("import-agent-lanes should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("import-agent-lanes should exit 1, got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), `unknown tendency "unknown-tendency"`) || !strings.Contains(string(output), "allowed:") {
		t.Fatalf("import-agent-lanes output missing closed-vocabulary error:\n%s", output)
	}
}

func writeCapabilityAgentLaneFixtures(t *testing.T, unknownTendency bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "registry.ts")
	overlayPath := filepath.Join(dir, "overlay.json")
	tendency := "falsifier-first"
	if unknownTendency {
		tendency = "unknown-tendency"
	}
	writeCapabilityAgentLaneFile(t, registryPath, `export const LANES = [];`)
	writeCapabilityAgentLaneFile(t, overlayPath, `{
  "roles": [
    {
      "id": "lane-alpha",
      "family": "lane",
      "description": "synthetic lane",
      "capability_class": "synthetic-task",
      "quality_tier": "synthetic-tier",
      "emit": ["h1"],
      "models": {"h1": "m1"},
      "tendencies": ["`+tendency+`"],
      "validated_by": ["eval:lane-alpha"]
    }
  ],
  "lanes": [
    {
      "id": "lane-alpha",
      "harness": "h1",
      "route": "lane-alpha",
      "cost_class": "synthetic"
    }
  ]
}`)
	return registryPath, overlayPath
}

func writeCapabilityAgentLaneFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
