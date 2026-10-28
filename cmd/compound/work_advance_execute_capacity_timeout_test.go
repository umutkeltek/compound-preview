package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/routeevidence"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// importQwenCapacityTupleFixture imports a claude_qwen lane tuple whose
// declared capacity source is CodexBar provider "qwen", so the execute
// door's adapter preflight reads the synthetic usage-history rows the test
// writes under $HOME.
func importQwenCapacityTupleFixture(t *testing.T, homePath string) {
	t.Helper()
	manifestPath := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	manifest := `version: 1
purpose: fixture
capability_classes: [build-implement]
models:
  fixture-qwen-capacity:
    lane: fixture qwen capacity lane
    harness: claude_qwen
    provider_route: qwen
    tier: cheap
    packet_ceiling: multi-file
    aliases: [claude-qwen]
    capacity_source: {kind: codexbar, freshness_minutes: 15, ref: "codexbar://provider/qwen"}
    capabilities: {build-implement: {level: capable, evidence: fixture}}
routing_defaults:
  build-implement: [fixture-qwen-capacity]
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, manifestPath, cliTestNow); err != nil {
		t.Fatal(err)
	}
}

// writeQwenCapacityRow installs one fresh synthetic CodexBar usage row for
// provider qwen: sampled 30s before anchor, reset `resetIn` after it. The
// execute door stamps RunInput.Now from the real clock, so rows anchor on
// wall time truncated to the second (matching the walled-capacity fixture).
func writeQwenCapacityRow(t *testing.T, userHome string, usedPercent float64, resetIn time.Duration) {
	t.Helper()
	usagePath := filepath.Join(userHome, "Library", "Application Support", "CodexBar", "usage-history.jsonl")
	if err := os.MkdirAll(filepath.Dir(usagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UTC().Truncate(time.Second)
	row := fmt.Sprintf(`{"provider":"qwen","windowKind":"5h","windowMinutes":300,"usedPercent":%g,"resetsAt":%q,"sampledAt":%q}`+"\n",
		usedPercent,
		anchor.Add(resetIn).Format(time.RFC3339),
		anchor.Add(-30*time.Second).Format(time.RFC3339))
	if err := os.WriteFile(usagePath, []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
}

type capacityTimeoutExitSignal struct{ code int }

// runExecuteCapacityStreams runs one in-process `work advance --execute`
// capturing stdout, stderr, and the exit code the governed door requested.
func runExecuteCapacityStreams(t *testing.T, args []string) (string, string, int) {
	t.Helper()
	originalExit := workAdvanceExecuteExit
	workAdvanceExecuteExit = func(code int) { panic(capacityTimeoutExitSignal{code: code}) }
	t.Cleanup(func() { workAdvanceExecuteExit = originalExit })
	code := 0
	stderrText := ""
	stdoutText := captureStdout(t, func() {
		stderrText = captureStderr(t, func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					signal, ok := recovered.(capacityTimeoutExitSignal)
					if !ok {
						panic(recovered)
					}
					code = signal.code
				}
			}()
			cmdWork(args)
		})
	})
	return stdoutText, stderrText, code
}

// TestWorkAdvanceExecuteBoundedTimeoutReachesPreflightCapacity is the CLI
// regression for binding launchTimeout to RunInput BEFORE adapter.Preflight:
// a bounded 8-minute run must pass the tuple preflight's capacity admission
// against a fresh, low-usage lane whose quota still resets 40 minutes out.
//
// The same forty-minute reset stays in place through actual child launch.
// Moving timeout binding after preflight or dropping it at start revalidation
// must refuse before the fake child runs.
func TestWorkAdvanceExecuteBoundedTimeoutReachesPreflightCapacity(t *testing.T) {
	fixture := newGovernedClaudeLaneExecutionFixture(
		t,
		"bounded run uses its own duration at capacity preflight",
		"claude_qwen",
		"claude-qwen",
	)
	importQwenCapacityTupleFixture(t, fixture.homePath)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	writeQwenCapacityRow(t, userHome, 5, 40*time.Minute)

	readinessCalls := 0
	stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return work.DelegateHarnessReadinessResult{
			Ready:    true,
			Evidence: governedClaudeReadyEvidenceFor("claude_qwen"),
		}, nil
	})

	args := append(fixture.advanceArgs("", "bounded run on fresh lane capacity"), "--timeout", "8m")
	stdout, stderr, code := runExecuteCapacityStreams(t, args)
	out := stdout + "\n" + stderr
	if code != 0 {
		t.Fatalf("bounded execute exit=%d: %s", code, out)
	}
	if strings.Contains(out, "capacity") {
		t.Fatalf("bounded run was capacity-refused:\n%s", out)
	}
	if !strings.Contains(out, "status: succeeded") {
		t.Fatalf("bounded run output missing success:\n%s", out)
	}
	if readinessCalls != 2 {
		t.Fatalf("readiness calls = %d, want pre-advance selection + pre-start revalidation (a preflight capacity refusal would leave 0)", readinessCalls)
	}
	fixture.argv(t) // child actually launched; absent file fails the test
	final, err := fixture.store.Get(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "verifying" {
		t.Fatalf("bounded run work state = %s, want verifying", final.State)
	}
	if attempt := fixture.executionAttempt(t); attempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("bounded run attempt = %s, want succeeded", attempt.Status)
	}
}

// TestWorkAdvanceExecuteBoundedTimeoutRefusesResetInsideBound pins the quota
// side at the CLI seam: the bound that now reaches preflight is a real
// deadline. A quota resetting exactly when an 8-minute run would end, or
// exactly when a 90s run's rounded 2-minute budget ends, is refused at the
// PREFLIGHT stage (the old 60-minute literal refused these too -- what must
// not regress is that the refusal stays at preflight with no state change).
// The 90s case also pins ceiling: a truncated 1-minute budget would have
// admitted the 2-minute reset at preflight and failed later at the run stage.
func TestWorkAdvanceExecuteBoundedTimeoutRefusesResetInsideBound(t *testing.T) {
	cases := []struct {
		name    string
		timeout string
		resetIn time.Duration
	}{
		{"eight_minutes_exact_reset", "8m", 8 * time.Minute},
		{"ninety_seconds_rounded_two_exact_reset", "90s", 2 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGovernedClaudeLaneExecutionFixture(
				t,
				"bounded run cannot outlive its quota reset",
				"claude_qwen",
				"claude-qwen",
			)
			importQwenCapacityTupleFixture(t, fixture.homePath)
			userHome := t.TempDir()
			t.Setenv("HOME", userHome)
			writeQwenCapacityRow(t, userHome, 5, tc.resetIn)

			args := append(fixture.advanceArgs("", "must not reach the child"), "--timeout", tc.timeout)
			stdout, stderr, code := runExecuteCapacityStreams(t, args)
			combined := stdout + "\n" + stderr
			if code != 1 {
				t.Fatalf("exit = %d, want 1 (preflight refusal):\n%s", code, combined)
			}
			if !strings.Contains(combined, "harness claude_qwen preflight:") || !strings.Contains(combined, "capacity expiring") {
				t.Fatalf("expected preflight capacity expiring refusal, got:\n%s", combined)
			}
			if _, err := os.Stat(fixture.argvLog); !os.IsNotExist(err) {
				t.Fatalf("refused bounded run launched a child; argv stat err=%v", err)
			}
			final, err := fixture.store.Get(fixture.workID)
			if err != nil {
				t.Fatal(err)
			}
			if final.State != "claimed" || len(final.RunAttemptIDs) != 1 {
				t.Fatalf("preflight refusal moved the envelope: state=%s attempts=%v, want claimed with only the claim-mint", final.State, final.RunAttemptIDs)
			}
		})
	}
}

// Exercise the production readiness adapter as well as the full execute test's
// injected authority seam. Missing unrelated authority may still refuse this
// fixture; it must never turn the bounded run back into an uncapped capacity check.
func TestDelegateHarnessReadinessPreservesBoundedCapacity(t *testing.T) {
	fixture := newGovernedClaudeLaneExecutionFixture(t, "readiness preserves timeout", "claude_qwen", "claude-qwen")
	importQwenCapacityTupleFixture(t, fixture.homePath)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	writeQwenCapacityRow(t, userHome, 5, 40*time.Minute)
	command, err := exec.LookPath("claude-qwen")
	if err != nil {
		t.Fatal(err)
	}
	req := work.DelegateHarnessReadinessRequest{HomePath: fixture.homePath, WorkID: fixture.workID, Workspace: fixture.workspace, Harness: "claude_qwen", LaneID: "codex", RequestedModelID: "fixture-qwen-capacity", RequestedProviderRoute: "qwen", Command: command, Now: time.Now().UTC(), Timeout: 8 * time.Minute}
	result, err := evaluateDelegateHarnessReadiness(req)
	if err != nil {
		t.Fatal(err)
	}
	if details := strings.Join(result.Details, "\n"); strings.Contains(details, "adapter_preflight:") {
		t.Fatalf("bounded readiness lost its resolved run input: %s", details)
	}
	req.Timeout = 0
	result, err = evaluateDelegateHarnessReadiness(req)
	if err != nil {
		t.Fatal(err)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "capacity expiring") {
		t.Fatalf("uncapped control did not refuse forty-minute window: %s", details)
	}
}
