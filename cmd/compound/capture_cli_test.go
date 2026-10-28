package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const captureCLIBoundarySubEnv = "COMPOUND_CAPTURE_CLI_BOUNDARY_SUB"

// TestCaptureCLITextFlagCapturesAndShowRoundTrips proves F6's --text path: a capture through
// --text mints a proposed candidate and `compound capture show <cid>` returns the exact bytes.
func TestCaptureCLITextFlagCapturesAndShowRoundTrips(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdCapture([]string{"--source", "phone-voice", "--text", "call the vet about the appointment", "--work", "work_50662769fd78"})
	})
	if !strings.Contains(out, "captured sha256:") {
		t.Fatalf("capture output missing cid prefix: %q", out)
	}
	if !strings.Contains(out, "source=phone-voice") {
		t.Fatalf("capture output missing source: %q", out)
	}
	if !strings.Contains(out, "candidate=cand_") {
		t.Fatalf("capture output missing candidate id: %q", out)
	}
	if !strings.Contains(out, "(proposed)") {
		t.Fatalf("first capture must report (proposed): %q", out)
	}

	fields := strings.Fields(out)
	var cid string
	for _, f := range fields {
		if strings.HasPrefix(f, "sha256:") {
			cid = f
			break
		}
	}
	if cid == "" {
		t.Fatalf("could not find cid in output: %q", out)
	}

	showOut := captureStdout(t, func() {
		cmdCapture([]string{"show", cid})
	})
	if showOut != "call the vet about the appointment" {
		t.Fatalf("show round-trip mismatch: got %q", showOut)
	}
}

// TestCaptureCLIStdinPathCaptures proves F6's stdin path: omitting --text reads the body from
// stdin.
func TestCaptureCLIStdinPathCaptures(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	var out string
	withStdin(t, "a stdin-fed capture body", func() {
		out = captureStdout(t, func() {
			cmdCapture([]string{"--source", "phone-voice"})
		})
	})
	if !strings.Contains(out, "captured sha256:") {
		t.Fatalf("stdin capture output missing cid prefix: %q", out)
	}
	if !strings.Contains(out, "(proposed)") {
		t.Fatalf("stdin capture must report (proposed): %q", out)
	}
}

// TestCaptureCLISameDayRecaptureReportsSkippedExisting proves the CLI success line distinguishes
// a fresh mint from a same-day skip-if-exists.
func TestCaptureCLISameDayRecaptureReportsSkippedExisting(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	first := captureStdout(t, func() {
		cmdCapture([]string{"--source", "phone-voice", "--text", "repeated reminder"})
	})
	if !strings.Contains(first, "(proposed)") {
		t.Fatalf("first capture must report (proposed): %q", first)
	}
	second := captureStdout(t, func() {
		cmdCapture([]string{"--source", "phone-voice", "--text", "repeated reminder"})
	})
	if !strings.Contains(second, "(skipped-existing)") {
		t.Fatalf("same-day recapture must report (skipped-existing): %q", second)
	}
}

// TestCaptureCLIEmptyBodyExitsUsage proves F6: an empty/whitespace-only body is an explicit
// usage error, exit 2 — never a silent empty capture. Runs in a subprocess because the CLI path
// calls os.Exit.
func TestCaptureCLIEmptyBodyExitsUsage(t *testing.T) {
	t.Parallel()

	if os.Getenv(captureCLIBoundarySubEnv) == "empty-body" {
		cmdCapture([]string{"--source", "phone-voice", "--text", "   "})
		return
	}
	homePath := initializedTestHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCaptureCLIEmptyBodyExitsUsage$")
	cmd.Env = append(os.Environ(), captureCLIBoundarySubEnv+"=empty-body", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("empty body must exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("empty body must exit 2 (usage), got %d output=%s", exitErr.ExitCode(), output)
	}
}

// TestCaptureCLIMissingSourceExitsUsage proves --source is required: omitting it is a usage
// error, exit 2.
func TestCaptureCLIMissingSourceExitsUsage(t *testing.T) {
	t.Parallel()

	if os.Getenv(captureCLIBoundarySubEnv) == "missing-source" {
		cmdCapture([]string{"--text", "a body with no source"})
		return
	}
	homePath := initializedTestHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCaptureCLIMissingSourceExitsUsage$")
	cmd.Env = append(os.Environ(), captureCLIBoundarySubEnv+"=missing-source", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("missing --source must exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("missing --source must exit 2 (usage), got %d output=%s", exitErr.ExitCode(), output)
	}
}

// TestCaptureCLIShowMissingCIDExitsError proves `show` on a missing/invalid cid is an explicit
// error, never a silent empty stdout.
func TestCaptureCLIShowMissingCIDExitsError(t *testing.T) {
	t.Parallel()

	if os.Getenv(captureCLIBoundarySubEnv) == "show-missing" {
		cmdCapture([]string{"show", "sha256:" + strings.Repeat("0", 64)})
		return
	}
	homePath := initializedTestHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCaptureCLIShowMissingCIDExitsError$")
	cmd.Env = append(os.Environ(), captureCLIBoundarySubEnv+"=show-missing", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("show on a missing cid must exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("show on a missing cid must exit 1 (explicit error), got %d output=%s", exitErr.ExitCode(), output)
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		t.Fatalf("show on a missing cid must not print silent empty output")
	}
}

// TestCaptureUsageLineIsRegisteredAtTopLevel proves `compound capture` is a top-level verb whose
// usage string is discoverable from the top-level help text (mirrors the finding/act verbs).
func TestCaptureUsageLineIsRegisteredAtTopLevel(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "compound", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(data), "compound capture ") {
		t.Fatalf("top-level usage() must document `compound capture`")
	}
}
