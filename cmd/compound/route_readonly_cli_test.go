package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/routeevidence"
)

func TestRoutePlanReadOnlyRefusesCapacityWithoutWritingEvidence(t *testing.T) {
	h := t.TempDir()
	if err := home.Init(h, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", h)
	t.Setenv("HOME", t.TempDir())
	manifest := `version: 1
capabilities: [classify-extract]
models:
  fixture-codex:
    lane: fixture-codex
    harness: codex
    provider_route: codex
    tier: premium
    packet_ceiling: multi-file
    capabilities:
      classify-extract: {level: primary, evidence: fixture}
  fixture-fallback:
    lane: fixture-fallback
    harness: switchback
    provider_route: fixture-provider
    tier: cheap
    packet_ceiling: multi-file
    capabilities:
      classify-extract: {level: capable, evidence: fixture}
routing_defaults:
  classify-extract: [fixture-codex, fixture-fallback]
`
	src := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	if err := os.WriteFile(src, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(h, src, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportAccountProjectionFile(h, filepath.Join("..", "..", "fixtures", "route", "account-projection-source.json"), time.Now()); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, h)
	out := captureStdout(t, func() { cmdRoute([]string{"plan", "--capability-class", "classify-extract", "--read-only", "--json"}) })
	var plan routeevidence.CapabilityRoutePlan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.ReadOnly || len(plan.Omitted) == 0 || !strings.Contains(plan.Omitted[0].OmitReason, "dead") {
		t.Fatalf("missing read-only capacity refusal: %s", out)
	}
	if after := snapshotTree(t, h); !reflect.DeepEqual(before, after) {
		t.Fatal("read-only route plan mutated the home")
	}
	// The control proves the fixture actually exercises a writing refusal.
	captureStdout(t, func() { cmdRoute([]string{"plan", "--capability-class", "classify-extract", "--json"}) })
	rows, _, err := routeevidence.LoadRouteEvidenceLedger(h)
	if err != nil || len(rows) == 0 {
		t.Fatalf("ordinary refusal control did not record evidence: rows=%d err=%v", len(rows), err)
	}
	if !strings.Contains(routeUsage(""), "--read-only") {
		t.Fatal("feature probe cannot discover read-only planning")
	}
}

func TestRoutePlanReadOnlyRejectsCandidateBeforeEffects(t *testing.T) {
	const childEnv = "COMPOUND_TEST_ROUTE_READONLY_CANDIDATE"
	if os.Getenv(childEnv) == "1" {
		cmdRoute([]string{"plan", "--candidate", "candidate_fixture", "--read-only", "--json"})
		os.Exit(0)
	}
	h := t.TempDir()
	if err := home.Init(h, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, h)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRoutePlanReadOnlyRejectsCandidateBeforeEffects$")
	cmd.Env = append(os.Environ(), childEnv+"=1", "COMPOUND_HOME="+h)
	out, err := cmd.CombinedOutput()
	if err == nil || cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "--read-only requires --capability-class") {
		t.Fatalf("expected usage refusal, got %v: %s", err, out)
	}
	if after := snapshotTree(t, h); !reflect.DeepEqual(before, after) {
		t.Fatal("invalid read-only request mutated the home")
	}
}
