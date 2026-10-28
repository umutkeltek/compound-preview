package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var proofCheckHomeOwningGateNames = []string{
	"codex-pretooluse-native-gate",
	"memory-doctor",
	"workbench-projection",
	"console-attention-consistency-smoke",
	"console-agents-consistency-smoke",
}

// TestHomeOwningGatesAreAllRealGates keeps the declared set and the gate list from drifting
// apart. A renamed or retired gate would otherwise leave a dangling runner that never fires —
// the gate would quietly fall through to the generic path and read the operator's home again.
func TestHomeOwningGatesAreAllRealGates(t *testing.T) {
	t.Parallel()
	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(steps))
	for _, s := range steps {
		names[s.Name] = true
	}
	for name := range proofCheckHomeOwningGates {
		if !names[name] {
			t.Fatalf("proofCheckHomeOwningGates names %q, which is not a gate in proofCheckSteps — the runner is dead code and that gate now inherits the live home", name)
		}
	}
	gotNames := make([]string, 0, len(proofCheckHomeOwningGates))
	for _, name := range proofCheckHomeOwningGateNames {
		if _, ok := proofCheckHomeOwningGates[name]; ok {
			gotNames = append(gotNames, name)
		}
	}
	if len(gotNames) != len(proofCheckHomeOwningGates) || !reflect.DeepEqual(gotNames, proofCheckHomeOwningGateNames) {
		t.Fatalf("proofCheckHomeOwningGates inventory = %v, want exactly %v", gotNames, proofCheckHomeOwningGateNames)
	}
}

// TestHomeOwningGateDispatchPassesCancelledContext protects the certifying runner's
// cancellation path. These gates create their own homes, but their descendants still hold the
// same admission barrier as every other proof step; a runner that swaps in Background would
// outlive a revoked lease.
func TestHomeOwningGateDispatchPassesCancelledContext(t *testing.T) {
	original := proofCheckHomeOwningGates
	t.Cleanup(func() { proofCheckHomeOwningGates = original })

	received := make(map[string]error, len(proofCheckHomeOwningGateNames))
	proofCheckHomeOwningGates = make(map[string]func(context.Context, string, proofCheckStep) ([]byte, error), len(proofCheckHomeOwningGateNames))
	for _, name := range proofCheckHomeOwningGateNames {
		name := name
		proofCheckHomeOwningGates[name] = func(ctx context.Context, _ string, _ proofCheckStep) ([]byte, error) {
			received[name] = ctx.Err()
			return nil, ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, name := range proofCheckHomeOwningGateNames {
		result := runProofCheckStepContext(ctx, t.TempDir(), proofCheckStep{Name: name, Command: []string{"true"}}, "")
		if result.Status != "failed" {
			t.Fatalf("%s status = %q, want failed after cancellation", name, result.Status)
		}
		if received[name] != context.Canceled {
			t.Fatalf("%s received context error %v, want context.Canceled", name, received[name])
		}
	}
}

// TestAgentsHomeOwningGateDoesNotRequirePATHCompound keeps the pre-dispatch availability
// check aligned with the gate's real invocation: its runner provides COMPOUND_BIN from the
// candidate runtime, so PATH never participates in its compound subprocess.
func TestAgentsHomeOwningGateDoesNotRequirePATHCompound(t *testing.T) {
	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatal(err)
	}
	var step proofCheckStep
	for _, candidate := range steps {
		if candidate.Name == "console-agents-consistency-smoke" {
			step = candidate
			break
		}
	}
	if step.Name == "" {
		t.Fatal("console-agents-consistency-smoke missing from proof steps")
	}

	original := proofCheckHomeOwningGates
	t.Cleanup(func() { proofCheckHomeOwningGates = original })
	runners := make(map[string]func(context.Context, string, proofCheckStep) ([]byte, error), len(original))
	for name, runner := range original {
		runners[name] = runner
	}
	called := false
	runners[step.Name] = func(_ context.Context, _ string, _ proofCheckStep) ([]byte, error) {
		called = true
		return []byte("agents runner invoked\n"), nil
	}
	proofCheckHomeOwningGates = runners
	t.Setenv("PATH", t.TempDir())

	result := runProofCheckStepContext(context.Background(), t.TempDir(), step, "")
	if result.Status == "skipped" {
		t.Fatalf("agents home-owning gate was skipped before its candidate runtime could be injected: %s", result.SkipReason)
	}
	if !called {
		t.Fatalf("agents home-owning gate runner was not invoked with compound absent from PATH: %+v", result)
	}
	if result.Status != "ok" {
		t.Fatalf("agents home-owning gate status = %q, want ok: %s", result.Status, result.OutputTail)
	}
}

// TestHomeOwningGateProductionPathsAreContextOnly makes the process-control contract
// mechanically visible: raw exec.Command cannot attach either the process group or the
// inheritable execution-barrier descriptor, and contextless runners can silently evade a
// revoked certifying run.
func TestHomeOwningGateProductionPathsAreContextOnly(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"proof_check_memory_gate.go",
		"proof_check_workbench_gate.go",
		"proof_check_console_attention_gate.go",
		"proof_check_agents_gate.go",
	} {
		body, err := os.ReadFile(filepath.Join(testRepoRoot, "cmd", "compound", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "exec.Command(") {
			t.Fatalf("%s still has a raw exec.Command production path", name)
		}
		if strings.Contains(string(body), "os.Executable(") {
			t.Errorf("%s bypasses pinned proof runtime custody with os.Executable", name)
		}
		if !strings.Contains(string(body), "proofCheckCommandContext(") {
			t.Fatalf("%s has no proofCheckCommandContext subprocess path", name)
		}
	}
}

func TestProofCheckAttentionFixtureProbeCancellationKillsDescendant(t *testing.T) {
	started, survivor, script := proofCheckGateCancellationScript(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := proofCheckAttentionFixtureGroupsContext(ctx, t.TempDir(), script)
		done <- err
	}()
	proofCheckWaitForFile(t, started)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("fixture probe survived cancellation")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("fixture probe context = %v, want context.Canceled", ctx.Err())
	}
	proofCheckAssertNoSurvivor(t, survivor)
}

func TestProofCheckGateCancellationReadinessNamesLiveDescendant(t *testing.T) {
	started, _, script := proofCheckGateCancellationScript(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := proofCheckCommandContext(ctx, script)
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	proofCheckWaitForFile(t, started)
	rawPID, err := os.ReadFile(started)
	if err != nil {
		t.Fatal(err)
	}
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatalf("cancellation readiness %q does not name the descendant: %v", rawPID, err)
	}
	if descendantPID <= 0 {
		t.Fatalf("cancellation readiness pid = %d, want positive", descendantPID)
	}
	if err := syscall.Kill(descendantPID, 0); err != nil {
		t.Fatalf("published descendant pid %d is not alive: %v", descendantPID, err)
	}
}

func TestProofCheckConsoleSmokeCancellationKillsDescendant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	repo, _ := proofCheckConsoleAttentionStep(t)
	homePath := proofCheckWorkbenchGateHome(t, false)
	started, survivor, script := proofCheckGateCancellationScript(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runProofCheckConsoleAttentionInHomeContext(ctx, repo, homePath, compoundTestBinary(t), proofCheckStep{
			Name: "console-attention-consistency-smoke", Command: []string{script},
		})
		done <- err
	}()
	proofCheckWaitForFile(t, started)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("console smoke survived cancellation")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("console smoke context = %v, want context.Canceled", ctx.Err())
	}
	proofCheckAssertNoSurvivor(t, survivor)
}

func proofCheckGateCancellationScript(t *testing.T) (started, survivor, script string) {
	t.Helper()
	dir := t.TempDir()
	started = filepath.Join(dir, "started")
	survivor = filepath.Join(dir, "survivor")
	script = filepath.Join(dir, "gate")
	// The descendant heartbeats its marker forever instead of writing once after
	// a fixed delay: a one-shot write races kill delivery under load (a 0.35s
	// timer vs >0.35s signal latency reads as a phantom survivor), while a marker
	// that reappears after the group wait has returned can only be a live process.
	//
	// Publish readiness only after the descendant exists. The prior leader marker
	// let cancellation race between publication and fork, testing a late-spawn
	// race instead of the claimed existing-descendant contract. Rename a complete
	// PID file so the observer cannot see create-before-content either.
	body := "#!/bin/sh\n( while :; do printf survivor > \"$PROOF_CHECK_GATE_SURVIVOR\"; sleep 0.1; done ) &\ndescendant=$!\nprintf '%s\\n' \"$descendant\" > \"$PROOF_CHECK_GATE_STARTED.tmp\"\nmv \"$PROOF_CHECK_GATE_STARTED.tmp\" \"$PROOF_CHECK_GATE_STARTED\"\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROOF_CHECK_GATE_STARTED", started)
	t.Setenv("PROOF_CHECK_GATE_SURVIVOR", survivor)
	return started, survivor, script
}

func proofCheckWaitForFile(t *testing.T, path string) {
	t.Helper()
	// Generous on purpose: spawn latency under a loaded machine (the full-suite
	// battery is itself the load generator) is not the property under test.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func proofCheckAssertNoSurvivor(t *testing.T, path string) {
	t.Helper()
	// Callers reach here only after the group wait has returned, so a marker on
	// disk may be a pre-death heartbeat, not a survivor. Erase it and require it
	// to STAY absent: only a live descendant (heartbeating every 100ms) can bring
	// it back, and it gets 1.2s of chances to do so.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("cancelled gate left a live descendant heartbeating at %s", path)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHomeOwningGatesIgnoreTheAmbientHome is the WIRING proof, and it is separate from the
// per-gate tests on purpose: those call their runner directly, so deleting a gate's entry in
// the dispatch map leaves every one of them green while the gate silently returns to reading
// COMPOUND_HOME. This drives the real runProofCheckStep and points COMPOUND_HOME at a path
// that does not exist — a gate that reads it fails, a gate that creates its own does not.
//
// Covers the two gates whose command carries an explicit binary path. The console gates
// resolve the compound binary from os.Executable(), which under `go test` is the test binary,
// so they cannot be driven end-to-end from here; their entry in the map is covered by the
// membership test above and their behavior by their own files.
func TestHomeOwningGatesIgnoreTheAmbientHome(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Clean(filepath.Join(wd, "..", ".."))
	exe := compoundTestBinary(t)
	t.Setenv("COMPOUND_HOME", filepath.Join(t.TempDir(), "no-such-home"))

	for _, tc := range []struct {
		name    string
		command []string
	}{
		{"memory-doctor", []string{exe, "memory", "doctor", "--json"}},
		{"workbench-projection", []string{exe, "workbench", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, owns := proofCheckHomeOwningGates[tc.name]; !owns {
				t.Fatalf("%s is no longer declared home-owning", tc.name)
			}
			got := runProofCheckStep(repo, proofCheckStep{Name: tc.name, Command: tc.command, ProofRef: tc.name}, "")
			if got.Status != "ok" {
				t.Fatalf("%s = %s (exit %d) with an absent ambient COMPOUND_HOME — it read the environment instead of seeding its own home:\n%s",
					tc.name, got.Status, got.ExitCode, got.OutputTail)
			}
		})
	}
}
