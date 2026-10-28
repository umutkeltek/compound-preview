package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestWorkAdvanceExecuteSavedCodexTupleSelectsExecutable(t *testing.T) {
	homePath := initializedExecutionHome(t)
	writeDispatchExecuteModelRouterFixtureForHarnessAlias(t, homePath, "codex", "fixture-codex-wrapper")
	_, _, revision, err := routeevidence.LoadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "selected Codex executable runs"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveExecutionSelection(w.ID, 0, envelope.ExecutionSelection{
		LaunchProfileID: "fixture-codex", LaunchProfileRevision: revision,
		Harness: "codex", ProviderRoute: "fixture-provider", RequestedModelID: "fixture-codex",
		Source: "explicit", Revision: 1, SelectedBy: "codex", SelectedAt: cliTestNow.Format("2006-01-02T15:04:05Z07:00"),
	}, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	launchLog := filepath.Join(t.TempDir(), "launch.txt")
	for name, marker := range map[string]string{"codex": "default", "fixture-codex-wrapper": "selected"} {
		script := "#!/bin/sh\nprintf '%s\\n' " + marker + " > \"$COMPOUND_TEST_LAUNCH_LOG\"\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(compoundTestBinary(t), "work", "advance", w.ID, "--to", "running", "--execute", "--prompt", "inspect saved tuple", "--workspace", t.TempDir())
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "COMPOUND_TEST_LAUNCH_LOG="+launchLog)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("governed execute: %v\n%s", err, out)
	}
	final, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.RunAttemptIDs) == 0 {
		t.Fatalf("no execution attempt: %s", out)
	}
	attempt, err := runattempt.Open(homePath).Get(final.RunAttemptIDs[len(final.RunAttemptIDs)-1])
	if err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(launchLog)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(attempt.Command) != "fixture-codex-wrapper" || strings.TrimSpace(string(logged)) != "selected" {
		t.Fatalf("saved tuple only stamped, not launched: command=%q log=%q selection=%+v", attempt.Command, logged, attempt.ExecutionSelection)
	}
	if attempt.RequestedModelID != "fixture-codex" || attempt.RequestedProviderRoute != "fixture-provider" {
		t.Fatalf("selected tuple not carried into attempt: %+v", attempt)
	}
}
