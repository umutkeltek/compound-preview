package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/home"
)

const homeManifestCompatibilityChildEnv = "COMPOUND_TEST_HOME_MANIFEST_COMPATIBILITY_CHILD"

const (
	homeManifestCompatibilityHomeEnv    = "COMPOUND_TEST_HOME_MANIFEST_COMPATIBILITY_HOME"
	homeManifestCompatibilityArchiveEnv = "COMPOUND_TEST_HOME_MANIFEST_COMPATIBILITY_ARCHIVE"
	homeManifestCompatibilityTargetEnv  = "COMPOUND_TEST_HOME_MANIFEST_COMPATIBILITY_TARGET"
)

func TestHomeDoctorSurfacesManifestFailureAndBackupFirstRecovery(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "1" {
		cmdHome([]string{"doctor"})
		return
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, home.ManifestName), []byte("schema_version: compound.home.v9.9\nhome_kind: global\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := buildHomeDoctorReport(home.Doctor(homePath))
	if report.ManifestOK || report.Verdict != "fail" || !strings.Contains(report.ManifestError, "unsupported home schema") {
		t.Fatalf("doctor JSON report = %+v", report)
	}
	if !strings.Contains(report.ManifestRecovery, "backup") || !strings.Contains(report.ManifestRecovery, "restore") {
		t.Fatalf("doctor JSON recovery = %q", report.ManifestRecovery)
	}
	if _, err := json.Marshal(report); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHomeDoctorSurfacesManifestFailureAndBackupFirstRecovery$")
	cmd.Env = append(os.Environ(), homeManifestCompatibilityChildEnv+"=1", "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("home doctor exit = %v, want 1; output:\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{"manifest_error: unsupported home schema", "manifest_recovery:", "backup", "restore", "will not rewrite"} {
		if !strings.Contains(text, want) {
			t.Fatalf("home doctor output missing %q:\n%s", want, text)
		}
	}
}

func TestDispatchCompatibilityGateBlocksFindingRecordBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "finding-record" {
		os.Args = []string{"compound", "finding", "record", "--topic", "compatibility-gate", "--body", "must not be written"}
		main()
		return
	}
	homePath := incompatibleCanonicalHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDispatchCompatibilityGateBlocksFindingRecordBeforeAnyWrite$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=finding-record",
		"COMPOUND_HOME="+homePath,
	)
	out, err := cmd.CombinedOutput()
	assertHomeManifestCompatibilityCLIError(t, err, out, "manifest kind")
	if _, statErr := os.Lstat(filepath.Join(homePath, "observed")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("finding record bypass mutated incompatible home: %v", statErr)
	}
}

func TestDispatchCompatibilityGateDoesNotTreatHelpLookingValueAsUsage(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "help-looking-value" {
		os.Args = []string{"compound", "finding", "record", "--topic", "compatibility-gate", "--body", "--help"}
		main()
		return
	}
	homePath := incompatibleCanonicalHome(t)
	manifestPath := filepath.Join(homePath, home.ManifestName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDispatchCompatibilityGateDoesNotTreatHelpLookingValueAsUsage$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=help-looking-value",
		"COMPOUND_HOME="+homePath,
	)
	out, err := cmd.CombinedOutput()
	assertHomeManifestCompatibilityCLIError(t, err, out, "manifest kind")
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("help-looking value bypass changed manifest: body=%q err=%v", after, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, "observed")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("help-looking value bypass mutated incompatible home: %v", statErr)
	}
}

func TestDispatchCompatibilityGateDoesNotTreatGenericHelpAsAdmission(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	root := t.TempDir()
	homePath := filepath.Join(root, "missing-home")
	cmd := exec.Command(binary, "orient", "--help", "--harness", "codex", "--cwd", root, "--task", "compatibility probe")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("orient --help bypassed mutation admission; output:\n%s", out)
	}
	if _, statErr := os.Lstat(homePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("orient --help created a manifestless home: %v", statErr)
	}
}

func TestDispatchCompatibilityGateAdmitsParserProvenHomeHelpOnly(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	root := t.TempDir()
	homePath := filepath.Join(root, "missing-home")
	archivePath := filepath.Join(root, "must-not-exist.tar.gz")
	cmd := exec.Command(binary, "home", "--target", archivePath, "backup", "--help")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("parser-proven home help was blocked: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "usage: compound home") {
		t.Fatalf("home help output missing usage:\n%s", out)
	}
	for _, path := range []string{homePath, archivePath} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("home help wrote %s: %v", path, statErr)
		}
	}
}

func TestDispatchCompatibilityGateAdmitsParserProvenHookInstallHelpOnly(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	root := t.TempDir()
	homePath := filepath.Join(root, "missing-home")
	basePath := filepath.Join(root, "missing-base")
	cmd := exec.Command(binary, "hook", "install", "--harness", "codex", "--scope", "global", "--base", basePath, "--binary", binary, "--help")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("parser-proven hook install help was blocked: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "usage: compound hook install") {
		t.Fatalf("hook install help output missing usage:\n%s", out)
	}
	for _, path := range []string{homePath, basePath} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("hook install help wrote %s: %v", path, statErr)
		}
	}
}

func TestDispatchCompatibilityGateMatchesReadFlagsExactly(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	for _, flag := range []string{"--CHECK", " --check ", "--check "} {
		t.Run(strings.ReplaceAll(flag, " ", "space"), func(t *testing.T) {
			root := t.TempDir()
			homePath := filepath.Join(root, "missing-home")
			basePath := filepath.Join(root, "missing-base")
			cmd := exec.Command(binary, "hook", "install", flag, "--harness", "codex", "--scope", "global", "--base", basePath, "--binary", binary)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("inexact read flag %q bypassed mutation admission; output:\n%s", flag, out)
			}
			for _, path := range []string{homePath, basePath} {
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("inexact read flag %q wrote %s: %v", flag, path, statErr)
				}
			}
		})
	}
}

func TestDispatchCompatibilityGateConsumesHomeOptionValuesBeforeClassifyingSubcommand(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	tests := []struct {
		name       string
		args       []string
		wantOutput string
		noWriteRel []string
	}{
		{
			name:       "prefixed archive verify reaches recovery reader",
			args:       []string{"home", "--archive", "missing.tar.gz", "verify"},
			wantOutput: "home verify:",
			noWriteRel: []string{"missing.tar.gz"},
		},
		{
			name:       "prefixed target and archive restore reaches recovery reader",
			args:       []string{"home", "--target", "restored-home", "--archive", "missing.tar.gz", "restore"},
			wantOutput: "home restore:",
			noWriteRel: []string{"missing.tar.gz", "restored-home"},
		},
		{
			name:       "read-looking target value cannot disguise backup",
			args:       []string{"home", "--target", "verify", "backup"},
			wantOutput: "home manifest is missing",
			noWriteRel: []string{"verify"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			homePath := filepath.Join(root, "missing-home")
			cmd := exec.Command(binary, tc.args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("home command unexpectedly succeeded; output:\n%s", out)
			}
			if !strings.Contains(strings.ToLower(string(out)), strings.ToLower(tc.wantOutput)) {
				t.Fatalf("home command output missing %q:\n%s", tc.wantOutput, out)
			}
			if _, statErr := os.Lstat(homePath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("home option classification wrote manifestless home: %v", statErr)
			}
			for _, rel := range tc.noWriteRel {
				path := filepath.Join(root, rel)
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("home option classification wrote %s: %v", path, statErr)
				}
			}
		})
	}
}

func TestHookCompatibilityFailureStaysProtocolShapedAndWriteFree(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	for _, homeState := range []string{"missing", "incompatible"} {
		for _, posture := range []string{hookPostureObservationOnly, hookPostureEnforced} {
			t.Run(homeState+"/"+posture, func(t *testing.T) {
				root := t.TempDir()
				homePath := filepath.Join(root, ".compound")
				var manifestBefore []byte
				if homeState == "incompatible" {
					homePath = incompatibleCanonicalHome(t)
					var err error
					manifestBefore, err = os.ReadFile(filepath.Join(homePath, home.ManifestName))
					if err != nil {
						t.Fatal(err)
					}
				}

				nonce := "act_compatprobe" + strings.ReplaceAll(homeState+posture, "_", "")
				fixturePath := filepath.Join(root, "pre-tool-use.json")
				if err := os.WriteFile(fixturePath, hookEnforcementProbeFixture(nonce), 0o600); err != nil {
					t.Fatal(err)
				}
				args := []string{"hook", "pre-tool-use"}
				if posture == hookPostureEnforced {
					args = append(args, "--enforce")
				}
				args = append(args, fixturePath)
				cmd := exec.Command(binary, args...)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
				out, runErr := cmd.Output()
				exitCode := 0
				if runErr != nil {
					var exitErr *exec.ExitError
					if !errors.As(runErr, &exitErr) {
						t.Fatalf("run hook: %v", runErr)
					}
					exitCode = exitErr.ExitCode()
				}
				outcome, detail := inspectHookEnforcementResponse(out, nonce)
				if detail != "" {
					t.Fatalf("hook response is not protocol-shaped: %s\n%s", detail, out)
				}
				wantExit, ok := hookPostureExpectedExit(posture, outcome)
				if !ok || exitCode != wantExit {
					t.Fatalf("hook posture=%s outcome=%s exit=%d, want supported outcome and exit=%d", posture, outcome, exitCode, wantExit)
				}

				if homeState == "missing" {
					if _, err := os.Lstat(homePath); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("hook created missing incompatible home: %v", err)
					}
					return
				}
				manifestAfter, err := os.ReadFile(filepath.Join(homePath, home.ManifestName))
				if err != nil || string(manifestAfter) != string(manifestBefore) {
					t.Fatalf("hook changed incompatible manifest: body=%q err=%v", manifestAfter, err)
				}
				entries, err := os.ReadDir(homePath)
				if err != nil || len(entries) != 1 || entries[0].Name() != home.ManifestName {
					t.Fatalf("hook wrote into incompatible home: entries=%v err=%v", entries, err)
				}
			})
		}
	}
}

func TestLivePreToolUseCompatibilityFailureHoldsRoutineWrite(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	for _, harnessName := range []string{"claude_code", "claude-zai"} {
		for _, homeState := range []string{"missing", "incompatible"} {
			for _, posture := range []string{hookPostureObservationOnly, hookPostureEnforced} {
				t.Run(harnessName+"/"+homeState+"/"+posture, func(t *testing.T) {
					root := t.TempDir()
					homePath := filepath.Join(root, ".compound")
					var manifestBefore []byte
					if homeState == "incompatible" {
						homePath = incompatibleCanonicalHome(t)
						var err error
						manifestBefore, err = os.ReadFile(filepath.Join(homePath, home.ManifestName))
						if err != nil {
							t.Fatal(err)
						}
					}

					targetPath := filepath.Join(root, "routine-write.txt")
					input, err := json.Marshal(claudeLiveHookInput{
						HookEventName:      "PreToolUse",
						SessionID:          "sess_compatibility_" + strings.ReplaceAll(harnessName, "-", "_") + "_" + homeState + "_" + posture,
						Cwd:                root,
						ToolName:           "Write",
						ToolSchemaRevision: "builtin:write",
						ToolInput: map[string]any{
							"file_path": targetPath,
							"content":   "fixture",
						},
					})
					if err != nil {
						t.Fatal(err)
					}
					args := []string{"hook", "pre-tool-use", "--live", "--harness", harnessName}
					if posture == hookPostureEnforced {
						args = append(args, "--enforce")
					}
					cmd := exec.Command(binary, args...)
					cmd.Dir = root
					cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
					cmd.Stdin = bytes.NewReader(input)
					out, runErr := cmd.Output()
					exitCode := 0
					if runErr != nil {
						var exitErr *exec.ExitError
						if !errors.As(runErr, &exitErr) {
							t.Fatalf("run live hook: %v", runErr)
						}
						exitCode = exitErr.ExitCode()
					}

					var response claudeLiveHookResponse
					if err := json.Unmarshal(out, &response); err != nil {
						t.Fatalf("hook response is not protocol-shaped: %v\n%s", err, out)
					}
					if response.HookSpecificOutput.HookEventName != "PreToolUse" {
						t.Fatalf("hook response event = %q, want PreToolUse: %s", response.HookSpecificOutput.HookEventName, out)
					}
					decision := response.HookSpecificOutput.PermissionDecision
					if posture == hookPostureObservationOnly {
						if exitCode != 0 || decision != "" {
							t.Fatalf("observation-only hook exit=%d decision=%q, want 0 and no native decision: %s", exitCode, decision, out)
						}
					} else {
						// Two channels, two facts. The EXIT CODE reports the governed
						// outcome (hold=3, deny=4) and is what tells an auditor whether
						// a lane was held -- recoverable through approval -- or denied.
						// The JSON permissionDecision is the harness-facing PROJECTION,
						// and posture decides it: a hold reaches an operator session as
						// `ask` and an unattended lane as `deny`, because a lane has
						// nobody to answer a prompt. This assertion used to derive one
						// from the other, which only worked while they were the same
						// fact.
						wantDecision := map[string]string{
							harness.PostureInteractive: "ask",
							harness.PostureUnattended:  "deny",
						}[harness.DefaultRegistry().Posture(harnessName)]
						if decision != wantDecision {
							t.Fatalf("enforced hook decision=%q for %s, want %q: %s", decision, harnessName, wantDecision, out)
						}
						if exitCode != 3 && exitCode != 4 {
							t.Fatalf("enforced hook exit=%d, want the governed gate code 3 (hold) or 4 (deny): %s", exitCode, out)
						}
					}

					if _, err := os.Lstat(targetPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("hook executed the routine write target: %v", err)
					}
					if homeState == "missing" {
						if _, err := os.Lstat(homePath); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("hook created missing home: %v", err)
						}
						return
					}
					manifestAfter, err := os.ReadFile(filepath.Join(homePath, home.ManifestName))
					if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
						t.Fatalf("hook changed incompatible manifest: body=%q err=%v", manifestAfter, err)
					}
					entries, err := os.ReadDir(homePath)
					if err != nil || len(entries) != 1 || entries[0].Name() != home.ManifestName {
						t.Fatalf("hook wrote into incompatible home: entries=%v err=%v", entries, err)
					}
				})
			}
		}
	}
}

func TestSessionEndAdapterCompatibilityFailureStaysProtocolShapedAndWriteFree(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	for _, homeState := range []string{"missing", "incompatible"} {
		t.Run(homeState, func(t *testing.T) {
			root := t.TempDir()
			homePath := filepath.Join(root, ".compound")
			var manifestBefore []byte
			if homeState == "incompatible" {
				homePath = incompatibleCanonicalHome(t)
				var err error
				manifestBefore, err = os.ReadFile(filepath.Join(homePath, home.ManifestName))
				if err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command(binary, "hook", "session-end-adapter", "--live", "--harness", "claude_code")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"SessionEnd","session_id":"session_compatibility_probe"}`)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("session-end adapter must fail open through native response: %v\n%s", err, out)
			}
			var response map[string]any
			if err := json.Unmarshal(out, &response); err != nil || response == nil {
				t.Fatalf("session-end adapter response is not protocol-shaped: body=%q err=%v", out, err)
			}

			if homeState == "missing" {
				if _, err := os.Lstat(homePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("session-end adapter created missing incompatible home: %v", err)
				}
				return
			}
			manifestAfter, err := os.ReadFile(filepath.Join(homePath, home.ManifestName))
			if err != nil || string(manifestAfter) != string(manifestBefore) {
				t.Fatalf("session-end adapter changed incompatible manifest: body=%q err=%v", manifestAfter, err)
			}
			entries, err := os.ReadDir(homePath)
			if err != nil || len(entries) != 1 || entries[0].Name() != home.ManifestName {
				t.Fatalf("session-end adapter wrote into incompatible home: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestLiveContextHookCompatibilityFailureNeverFallsThroughToHomeAwareWriters(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and executes the compound binary: skipped in -short mode")
	}
	binary := compoundTestBinary(t)
	for _, homeState := range []string{"missing", "incompatible"} {
		t.Run(homeState, func(t *testing.T) {
			root := t.TempDir()
			homePath := filepath.Join(root, ".compound")
			var manifestBefore []byte
			if homeState == "incompatible" {
				homePath = incompatibleCanonicalHome(t)
				var err error
				manifestBefore, err = os.ReadFile(filepath.Join(homePath, home.ManifestName))
				if err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command(binary, "hook", "session-start", "--live", "--harness", "claude_code")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"session_compatibility_probe","cwd":"` + root + `"}`)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("live context hook must fail open through native response: %v\n%s", err, out)
			}
			var response map[string]any
			if err := json.Unmarshal(out, &response); err != nil || response == nil {
				t.Fatalf("live context hook response is not protocol-shaped: body=%q err=%v", out, err)
			}
			rootEntries, err := os.ReadDir(root)
			if err != nil || len(rootEntries) != 0 {
				t.Fatalf("empty-home hook fell through to a cwd-relative writer: entries=%v err=%v", rootEntries, err)
			}

			if homeState == "missing" {
				if _, err := os.Lstat(homePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("live context hook created missing incompatible home: %v", err)
				}
				return
			}
			manifestAfter, err := os.ReadFile(filepath.Join(homePath, home.ManifestName))
			if err != nil || string(manifestAfter) != string(manifestBefore) {
				t.Fatalf("live context hook changed incompatible manifest: body=%q err=%v", manifestAfter, err)
			}
			entries, err := os.ReadDir(homePath)
			if err != nil || len(entries) != 1 || entries[0].Name() != home.ManifestName {
				t.Fatalf("live context hook wrote into incompatible home: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestDispatchCompatibilityGateBlocksPrivateCustodyRepairBeforeChmod(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "private-custody-repair" {
		os.Args = []string{"compound", "home", "private-custody", "repair"}
		main()
		return
	}
	homePath := incompatibleCanonicalHome(t)
	if err := os.Chmod(homePath, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDispatchCompatibilityGateBlocksPrivateCustodyRepairBeforeChmod$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=private-custody-repair",
		"COMPOUND_HOME="+homePath,
	)
	out, err := cmd.CombinedOutput()
	assertHomeManifestCompatibilityCLIError(t, err, out, "manifest kind")
	info, statErr := os.Lstat(homePath)
	if statErr != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("private-custody repair bypass changed incompatible home mode: info=%v err=%v", info, statErr)
	}
}

func TestDispatchCompatibilityGateBlocksMutationWhenManifestIsMissing(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "missing-manifest" {
		os.Args = []string{"compound", "finding", "record", "--topic", "compatibility-gate", "--body", "must not be written"}
		main()
		return
	}
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(homePath, "operator-data")
	if err := os.WriteFile(markerPath, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDispatchCompatibilityGateBlocksMutationWhenManifestIsMissing$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=missing-manifest",
		"COMPOUND_HOME="+homePath,
	)
	out, err := cmd.CombinedOutput()
	assertHomeManifestCompatibilityCLIError(t, err, out, "manifest is missing")
	if _, statErr := os.Lstat(filepath.Join(homePath, "observed")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing-manifest gate allowed mutation: %v", statErr)
	}
	if got, readErr := os.ReadFile(markerPath); readErr != nil || string(got) != "preserve" {
		t.Fatalf("missing-manifest gate changed existing data: body=%q err=%v", got, readErr)
	}
}

func TestDispatchCompatibilityGatePreservesReadOnlyHomeWhere(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "home-where" {
		os.Args = []string{"compound", "home", "where"}
		main()
		return
	}
	homePath := incompatibleCanonicalHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDispatchCompatibilityGatePreservesReadOnlyHomeWhere$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=home-where",
		"COMPOUND_HOME="+homePath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), homePath+"\n") {
		t.Fatalf("home where lost read-only recovery access: err=%v output=%q", err, out)
	}
}

func TestDispatchCompatibilityGateUsesLocalHomeForLocalMutation(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "local-private-custody" {
		os.Args = []string{"compound", "home", "--local", "private-custody", "repair"}
		main()
		return
	}
	ambientHome := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(ambientHome, "ambient-profile", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	localHome := filepath.Join(workspace, ".compound")
	if err := os.Mkdir(localHome, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("schema_version: " + home.SchemaVersion + "\nkind: compound_server\nhome_kind: local-profile\n")
	if err := os.WriteFile(filepath.Join(localHome, home.ManifestName), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDispatchCompatibilityGateUsesLocalHomeForLocalMutation$")
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=local-private-custody",
		"COMPOUND_HOME="+ambientHome,
	)
	out, err := cmd.CombinedOutput()
	assertHomeManifestCompatibilityCLIError(t, err, out, "manifest kind")
	info, statErr := os.Lstat(localHome)
	if statErr != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("local compatibility failure changed home mode: info=%v err=%v", info, statErr)
	}
}

func TestDispatchCompatibilityGateUsesExplicitHomeAroundCommand(t *testing.T) {
	t.Parallel()
	childMode := os.Getenv(homeManifestCompatibilityChildEnv)
	selectedHome := os.Getenv(homeManifestCompatibilityHomeEnv)
	if childMode == "explicit-home-after-command" {
		os.Args = []string{"compound", "home", "--home", selectedHome, "private-custody", "repair"}
		main()
		return
	}
	if childMode == "explicit-home-before-command" {
		os.Args = []string{"compound", "--home", selectedHome, "home", "private-custody", "repair"}
		main()
		return
	}

	ambientHome := filepath.Join(t.TempDir(), "ambient")
	if err := home.Init(ambientHome, "ambient-profile", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"explicit-home-after-command", "explicit-home-before-command"} {
		t.Run(mode, func(t *testing.T) {
			selected := incompatibleCanonicalHome(t)
			manifestPath := filepath.Join(selected, home.ManifestName)
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestDispatchCompatibilityGateUsesExplicitHomeAroundCommand$/"+mode+"$")
			cmd.Env = append(os.Environ(),
				homeManifestCompatibilityChildEnv+"="+mode,
				homeManifestCompatibilityHomeEnv+"="+selected,
				"COMPOUND_HOME="+ambientHome,
			)
			out, runErr := cmd.CombinedOutput()
			assertHomeManifestCompatibilityCLIError(t, runErr, out, "manifest kind")
			after, readErr := os.ReadFile(manifestPath)
			if readErr != nil || !bytes.Equal(after, before) {
				t.Fatalf("explicit-home refusal changed selected manifest: body=%q err=%v", after, readErr)
			}
		})
	}
}

func TestDispatchCompatibilityGateDoesNotNormalizeHomeFlagTokens(t *testing.T) {
	t.Parallel()
	childMode := os.Getenv(homeManifestCompatibilityChildEnv)
	selectedHome := os.Getenv(homeManifestCompatibilityHomeEnv)
	if childMode == "padded-home-flag" {
		os.Args = []string{"compound", "home", "private-custody", "repair", " --home ", selectedHome}
		main()
		return
	}

	ambientHome := incompatibleCanonicalHome(t)
	manifestPath := filepath.Join(ambientHome, home.ManifestName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(t.TempDir(), "selected")
	if err := home.Init(selected, home.HomeKindGlobal, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDispatchCompatibilityGateDoesNotNormalizeHomeFlagTokens$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=padded-home-flag",
		homeManifestCompatibilityHomeEnv+"="+selected,
		"COMPOUND_HOME="+ambientHome,
	)
	out, runErr := cmd.CombinedOutput()
	assertHomeManifestCompatibilityCLIError(t, runErr, out, "manifest kind")
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("padded --home admission changed ambient manifest: body=%q err=%v", after, readErr)
	}
	entries, readDirErr := os.ReadDir(ambientHome)
	if readDirErr != nil || len(entries) != 1 || entries[0].Name() != home.ManifestName {
		t.Fatalf("padded --home admission mutated ambient home: entries=%v err=%v", entries, readDirErr)
	}
}

func incompatibleCanonicalHome(t *testing.T) string {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("schema_version: " + home.SchemaVersion + "\nkind: compound_server\nhome_kind: global\n")
	if err := os.WriteFile(filepath.Join(homePath, home.ManifestName), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	return homePath
}

func TestHomeInitCLIManifestIncompatibility(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "init" {
		cmdHome([]string{"init", "--home", os.Getenv(homeManifestCompatibilityHomeEnv), "--local"})
		return
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(homePath, home.ManifestName)
	before := []byte("schema_version: compound.home.v9.9\nhome_kind: local\n")
	if err := os.WriteFile(manifestPath, before, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHomeInitCLIManifestIncompatibility$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=init",
		homeManifestCompatibilityHomeEnv+"="+homePath,
	)
	out, err := cmd.CombinedOutput()
	assertHomeManifestCompatibilityCLIError(t, err, out, "unsupported home schema")

	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("home init CLI mutated manifest: body=%q err=%v", after, readErr)
	}
	for _, rel := range home.HabitatDirs() {
		if _, statErr := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("home init CLI created habitat %s: %v", rel, statErr)
		}
	}
}

func TestHomeRestoreCLIManifestIncompatibility(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "restore" {
		cmdHome([]string{
			"restore",
			os.Getenv(homeManifestCompatibilityArchiveEnv),
			"--target",
			os.Getenv(homeManifestCompatibilityTargetEnv),
		})
		return
	}

	manifestBody := []byte("schema_version: " + home.SchemaVersion + "\nhome_kind: local\nunknown_identity: true\n")
	archivePath := writeManifestCompatibilityCLIArchive(t, manifestBody)
	target := filepath.Join(t.TempDir(), ".compound-restored")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHomeRestoreCLIManifestIncompatibility$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=restore",
		homeManifestCompatibilityArchiveEnv+"="+archivePath,
		homeManifestCompatibilityTargetEnv+"="+target,
	)
	out, err := cmd.CombinedOutput()
	assertHomeManifestCompatibilityCLIError(t, err, out, "unknown_identity")
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("home restore CLI published incompatible target: %v", statErr)
	}
}

func TestHomeRestoreCLIArchiveSchemaIncompatibility(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeManifestCompatibilityChildEnv) == "restore-archive-schema" {
		cmdHome([]string{
			"restore",
			os.Getenv(homeManifestCompatibilityArchiveEnv),
			"--target",
			os.Getenv(homeManifestCompatibilityTargetEnv),
		})
		return
	}

	manifestBody := []byte("schema_version: " + home.SchemaVersion + "\nhome_kind: global\ncreated_at: 2026-01-01T00:00:00Z\nactive_modules: []\n")
	archivePath := writeManifestCompatibilityCLIArchiveWithHomeSchema(t, manifestBody, "compound.home.v9.9")
	target := filepath.Join(t.TempDir(), ".compound-restored")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHomeRestoreCLIArchiveSchemaIncompatibility$")
	cmd.Env = append(os.Environ(),
		homeManifestCompatibilityChildEnv+"=restore-archive-schema",
		homeManifestCompatibilityArchiveEnv+"="+archivePath,
		homeManifestCompatibilityTargetEnv+"="+target,
	)
	out, err := cmd.CombinedOutput()
	assertArchiveCompatibilityCLIError(t, err, out, "archive home_schema_version must be")
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("home restore CLI published archive-schema-incompatible target: %v", statErr)
	}
}

func assertHomeManifestCompatibilityCLIError(t *testing.T, err error, output []byte, reason string) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("home command exit = %v, want 1; output:\n%s", err, output)
	}
	text := strings.ToLower(string(output))
	for _, want := range []string{
		reason,
		"compound home backup --target <archive>",
		"compound home verify <archive>",
		"preserve the original home",
		"roll back",
		"compound home restore <archive> --target <new-absent-home-path>",
		"will not rewrite manifest.yaml",
	} {
		if !strings.Contains(text, strings.ToLower(want)) {
			t.Fatalf("home command output missing %q:\n%s", want, output)
		}
	}
}

func assertArchiveCompatibilityCLIError(t *testing.T, err error, output []byte, reason string) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("home command exit = %v, want 1; output:\n%s", err, output)
	}
	text := strings.ToLower(string(output))
	for _, want := range []string{
		reason,
		"preserve the archive unchanged",
		"compound home verify <archive>",
		"restore only to a new absent target",
		"compound home restore <archive> --target <new-absent-home-path>",
		"will not rewrite the archive",
	} {
		if !strings.Contains(text, strings.ToLower(want)) {
			t.Fatalf("home command output missing %q:\n%s", want, output)
		}
	}
}

func writeManifestCompatibilityCLIArchive(t *testing.T, manifestBody []byte) string {
	return writeManifestCompatibilityCLIArchiveWithHomeSchema(t, manifestBody, home.SchemaVersion)
}

func writeManifestCompatibilityCLIArchiveWithHomeSchema(t *testing.T, manifestBody []byte, homeSchemaVersion string) string {
	t.Helper()
	digest := sha256.Sum256(manifestBody)
	manifest := home.ArchiveManifest{
		SchemaVersion:     home.ArchiveSchemaVersion,
		Kind:              home.ArchiveKindBackup,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339),
		HomeSchemaVersion: homeSchemaVersion,
		Entries: []home.ArchiveEntry{{
			Path:   home.ManifestName,
			Size:   int64(len(manifestBody)),
			SHA256: fmt.Sprintf("%x", digest[:]),
		}},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "incompatible-home.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	writeEntry := func(name string, body []byte) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry(home.ManifestName, manifestBody)
	writeEntry(home.ArchiveManifestName, manifestJSON)
	if err := errors.Join(tw.Close(), gz.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	return archivePath
}
