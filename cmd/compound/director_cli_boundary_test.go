package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// directorCLIBoundarySubEnv, when set, tells the re-executed test binary to
// invoke cmdDirector directly with the named subcommand so the parent process
// can observe the real os.Exit code without terminating the test runner.
const directorCLIBoundarySubEnv = "COMPOUND_DIRECTOR_CLI_BOUNDARY_SUB"

// TestDirectorCLISurfaceIsProposeOnly enforces that the director CLI exposes
// exactly one verb — propose — and that every other imperative subcommand a
// scheduler/boss would want (execute, run, apply, schedule, ...) is rejected
// with a usage error and exit code 2. The propose surface must also speak of
// "suggested work", never "scheduled" or "executed" work.
func TestDirectorCLISurfaceIsProposeOnly(t *testing.T) {
	// Child arm: actually run the forbidden subcommand and let it exit.
	if sub := os.Getenv(directorCLIBoundarySubEnv); sub != "" {
		cmdDirector([]string{sub})
		// cmdDirector must have exited already for any non-propose verb; if it
		// returns, exit 0 so the parent's exit-2 assertion fails loudly.
		return
	}

	for _, sub := range []string{"execute", "run", "apply", "schedule", "dispatch", "admit"} {
		t.Run(sub, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDirectorCLISurfaceIsProposeOnly$")
			cmd.Env = append(os.Environ(), directorCLIBoundarySubEnv+"="+sub)
			output, err := cmd.CombinedOutput()

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("director %q should exit non-zero, got err=%v output=%s", sub, err, output)
			}
			if exitErr.ExitCode() != 2 {
				t.Fatalf("director %q should exit 2 (usage), got exit %d output=%s", sub, exitErr.ExitCode(), output)
			}
			if !strings.Contains(string(output), "usage: compound director propose") {
				t.Fatalf("director %q should print propose usage, got %s", sub, output)
			}
		})
	}
}

// TestDirectorProposeSaysSuggestedWork proves the one allowed verb frames its
// output as suggestion, not command: the human-facing line says "suggested
// work". This is the user-facing half of the suggestion-only boundary.
func TestDirectorProposeSaysSuggestedWork(t *testing.T) {
	t.Setenv("COMPOUND_HOME", t.TempDir())
	out := captureStdout(t, func() {
		cmdDirector([]string{"propose", "--dry-run"})
	})
	if !strings.Contains(out, "suggested work") {
		t.Fatalf("director propose human output must say %q, got %q", "suggested work", out)
	}
	if strings.Contains(out, "scheduled work") || strings.Contains(out, "executed work") {
		t.Fatalf("director propose output must not imply scheduling/execution, got %q", out)
	}
}
