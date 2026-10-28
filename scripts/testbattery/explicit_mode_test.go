package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The expensive inventory must never start by accident. The falsifier runs the
// real CLI binary with a recording `go` and `git` on PATH: every child launch
// leaves a trace, so an empty trace proves nothing was started at all.
func TestCLIWithoutExplicitModeStartsNoSubprocess(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "testbattery")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build testbattery: %v\n%s", err, out)
	}

	launches := filepath.Join(dir, "launches")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recorder := "#!/bin/sh\nprintf '%s %s\\n' \"$(basename \"$0\")\" \"$*\" >> \"" + launches + "\"\nexit 0\n"
	for _, name := range []string{"go", "git"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(recorder), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	for _, tt := range []struct {
		name     string
		args     []string
		wantCode int
		wantHelp bool
	}{
		{name: "no arguments", args: nil, wantCode: 2},
		{name: "help short", args: []string{"-h"}, wantCode: 0, wantHelp: true},
		{name: "help long", args: []string{"--help"}, wantCode: 0, wantHelp: true},
		{name: "flag only", args: []string{"--source-head", strings.Repeat("a", 40)}, wantCode: 2},
		{name: "unknown mode", args: []string{"bogus"}, wantCode: 2},
		{name: "unknown flag", args: []string{"--bogus"}, wantCode: 2},
		{name: "extra argument", args: []string{"test", "extra"}, wantCode: 2},
		{name: "short source head", args: []string{"test", "--source-head", "abc"}, wantCode: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(binary, tt.args...)
			cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run CLI: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
				}
				code = exitErr.ExitCode()
			}
			if code != tt.wantCode {
				t.Fatalf("exit=%d; want %d\nstdout=%s\nstderr=%s", code, tt.wantCode, &stdout, &stderr)
			}
			if !strings.Contains(stdout.String()+stderr.String(), "usage: testbattery [test|general-release|home|vet|build|credential-password-alignment-native]") {
				t.Fatalf("usage line missing: stdout=%s stderr=%s", &stdout, &stderr)
			}
			if tt.wantHelp {
				if !strings.Contains(stdout.String(), "usage: testbattery") {
					t.Fatalf("help must print usage on stdout: stdout=%s stderr=%s", &stdout, &stderr)
				}
			} else if stdout.Len() != 0 {
				t.Fatalf("refusal must not write stdout: %s", &stdout)
			}
			recorded, readErr := os.ReadFile(launches)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			if len(recorded) != 0 {
				t.Fatalf("subprocess started: %s", recorded)
			}
		})
	}
}

// An explicit mode is the only way in, and it must still cover the whole
// inventory: the real CLI over a real Git fixture runs every package once.
func TestCLIExplicitTestModeStillCoversTheWholeInventory(t *testing.T) {
	repo := sourceFixture(t)
	out, err := sourceRun(t, repo, "test")
	if err != nil {
		t.Fatalf("explicit test mode: %v\n%s", err, out)
	}
	for _, want := range []string{
		"testbattery: phase 1/4",
		"testbattery: phase 2/4",
		"testbattery: phase 3/4",
		"testbattery: phase 4/4",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("explicit test mode lost %q: %s", want, out)
		}
	}
}

// Every explicit mode stays reachable; nothing but a mode-less or malformed
// invocation is refused.
func TestParseInvocationRequiresAnExplicitMode(t *testing.T) {
	sealed := strings.Repeat("a", 40)
	for _, tt := range []struct {
		name       string
		args       []string
		wantMode   string
		wantHead   string
		wantHelp   bool
		wantRefuse bool
	}{
		{name: "no arguments", args: nil, wantRefuse: true},
		{name: "test", args: []string{"test"}, wantMode: "test"},
		{name: "general release", args: []string{"general-release"}, wantMode: "general-release"},
		{name: "home", args: []string{"home"}, wantMode: "home"},
		{name: "vet", args: []string{"vet"}, wantMode: "vet"},
		{name: "build", args: []string{"build"}, wantMode: "build"},
		{name: "credential alignment", args: []string{credentialAlignmentProofMode}, wantMode: credentialAlignmentProofMode},
		{name: "sealed head", args: []string{"test", "--source-head", sealed}, wantMode: "test", wantHead: sealed},
		{name: "help short", args: []string{"-h"}, wantHelp: true},
		{name: "help long", args: []string{"--help"}, wantHelp: true},
		{name: "help after mode", args: []string{"test", "-h"}, wantHelp: true},
		{name: "unknown mode", args: []string{"nope"}, wantRefuse: true},
		{name: "flag only", args: []string{"--source-head", sealed}, wantRefuse: true},
		{name: "unknown flag", args: []string{"--nope"}, wantRefuse: true},
		{name: "extra argument", args: []string{"test", "extra"}, wantRefuse: true},
		{name: "short source head", args: []string{"test", "--source-head", "abc"}, wantRefuse: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := parseInvocation(tt.args)
			if (err != nil) != tt.wantRefuse {
				t.Fatalf("error=%v; want refusal=%t", err, tt.wantRefuse)
			}
			if tt.wantRefuse {
				return
			}
			if inv.help != tt.wantHelp || inv.mode != tt.wantMode || inv.sourceHead != tt.wantHead {
				t.Fatalf("invocation=%+v; want help=%t mode=%q head=%q", inv, tt.wantHelp, tt.wantMode, tt.wantHead)
			}
		})
	}
}
