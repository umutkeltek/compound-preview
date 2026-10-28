package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestWorkAdvanceExecuteSavedOpenCodeTupleReachesNativePromptOnlyAdapter(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native prompt-only adapter requires macOS sandbox")
	}
	home := initializedExecutionHome(t)
	command := filepath.Join(t.TempDir(), "fixture-native")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"text\",\"part\":{\"text\":\"CONTROL_OK\"}}' '{\"type\":\"step_finish\",\"part\":{\"reason\":\"stop\"}}'\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "model.yaml")
	body := fmt.Sprintf(`version: 1
purpose: synthetic native selection
capability_classes: [build-implement]
models:
  fixture/control:
    lane: synthetic
    harness: opencode
    provider_route: fixture
    provider_endpoint: https://provider.invalid/v1
    aliases: [%q]
    tier: premium
    packet_ceiling: repo-scoped
    capabilities: {build-implement: {level: capable, evidence: fixture}}
routing_defaults:
  build-implement: [fixture/control]
`, command)
	if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, source, cliTestNow); err != nil {
		t.Fatal(err)
	}
	_, _, revision, err := routeevidence.LoadModelCapabilitiesSnapshot(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "native prompt-only selection reaches adapter"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveExecutionSelection(w.ID, 0, envelope.ExecutionSelection{LaunchProfileID: "fixture/control", LaunchProfileRevision: revision, Harness: "opencode", ProviderRoute: "fixture", RequestedModelID: "fixture/control", Source: "explicit", Revision: 1, SelectedBy: "codex", SelectedAt: cliTestNow.Format("2006-01-02T15:04:05Z07:00")}, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	ownerHome := t.TempDir()
	authDir := filepath.Join(ownerHome, ".local/share/opencode")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"fixture":{"type":"api","key":"synthetic-selected-credential"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	requestedWorkspace := t.TempDir()
	cmd := exec.Command(compoundTestBinary(t), "work", "advance", w.ID, "--to", "running", "--execute", "--timeout", "5m", "--prompt", "Return CONTROL_OK", "--workspace", requestedWorkspace)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+home, "HOME="+ownerHome)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("governed native selection: %v\n%s", err, output)
	}
	final, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := runattempt.Open(home).Get(final.RunAttemptIDs[len(final.RunAttemptIDs)-1])
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "verifying" || attempt.Status != runattempt.StatusSucceeded || attempt.Harness != "opencode" || attempt.Command != "/usr/bin/sandbox-exec" || attempt.Workspace == requestedWorkspace || attempt.RequestedModelID != "fixture/control" || attempt.RequestedProviderRoute != "fixture" {
		t.Fatalf("native selection/lifecycle not applied: state=%s attempt=%+v", final.State, attempt)
	}
	if attempt.Observed == nil || attempt.Observed.ObservedModelID != "fixture/control" || !strings.Contains(attempt.Observed.Source, "opencode") {
		t.Fatalf("native argv selection not observed: %+v", attempt.Observed)
	}
	if !strings.Contains(strings.Join(attempt.ProofRefs, " "), "timeout_max=90s") {
		t.Fatal("native bound was not recorded separately from the caller five-minute request")
	}
	if _, err := os.Stat(filepath.Dir(attempt.Workspace)); !os.IsNotExist(err) {
		t.Fatal("native isolated credential root retained")
	}
}
