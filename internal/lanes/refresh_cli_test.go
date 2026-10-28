package lanes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseCLIRefreshArgsRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown", args: []string{"--unknown"}, want: `unknown refresh-lane argument "--unknown"`},
		{name: "unknown-preflight", args: []string{"--preflight", "--unknown"}, want: `unknown refresh-lane argument "--unknown"`},
		{name: "unknown-help", args: []string{"--help", "--unknown"}, want: `unknown refresh-lane argument "--unknown"`},
		{name: "missing-home", args: []string{"--home"}, want: "--home requires a value"},
		{name: "missing-binary", args: []string{"--binary"}, want: "--binary requires a value"},
		{name: "missing-base", args: []string{"--base"}, want: "--base requires a value"},
		{name: "missing-harness", args: []string{"--harness"}, want: "--harness requires a value"},
		{name: "blank-value", args: []string{"--base", " \t"}, want: "--base requires a value"},
		{name: "preflight-as-value", args: []string{"--binary", "--preflight"}, want: "--binary requires a value"},
		{name: "help-as-value", args: []string{"--harness", "-h"}, want: "--harness requires a value"},
		{name: "unexpected-positional", args: []string{"profile"}, want: `unknown refresh-lane argument "profile"`},
		{name: "unsupported-equals", args: []string{"--home=profile"}, want: `unknown refresh-lane argument "--home=profile"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, usage, err := ParseCLIRefreshArgs(tc.args, "default-home", "default-binary", "default-base", time.Time{})
			if err == nil || !strings.Contains(err.Error(), tc.want) || usage || opts != (CLIRefreshOpts{}) {
				t.Fatalf("opts=%+v usage=%v err=%v; want refusal containing %q", opts, usage, err, tc.want)
			}
		})
	}
}

func TestParseCLIRefreshArgsPreservesSupportedArguments(t *testing.T) {
	now := time.Now().UTC()
	defaults := CLIRefreshOpts{HomePath: "default-home", BinaryPath: "default-binary", BaseDir: "default-base", Now: now}
	for _, tc := range []struct {
		name  string
		args  []string
		want  CLIRefreshOpts
		usage bool
	}{
		{name: "defaults", want: defaults},
		{name: "preflight-defaults", args: []string{"--preflight"}, want: defaults},
		{name: "explicit", args: []string{"--home", "chosen home", "--binary", "chosen-binary", "--base", "chosen-base", "--harness", "claude-zai", "--json", "--preflight"}, want: CLIRefreshOpts{HomePath: "chosen home", BinaryPath: "chosen-binary", BaseDir: "chosen-base", HarnessName: "claude-zai", JSON: true, Now: now}},
		{name: "help", args: []string{"--help"}, usage: true},
		{name: "short-help", args: []string{"-h"}, usage: true},
		{name: "preflight-help", args: []string{"--preflight", "--help"}, usage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, usage, err := ParseCLIRefreshArgs(tc.args, defaults.HomePath, defaults.BinaryPath, defaults.BaseDir, now)
			if err != nil || usage != tc.usage || opts != tc.want {
				t.Fatalf("opts=%+v usage=%v err=%v; want opts=%+v usage=%v", opts, usage, err, tc.want, tc.usage)
			}
		})
	}
}

func TestRefreshCLIRefusesUnreadableManifest(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "installed-hooks.yaml"), []byte("hooks: [invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunCLIFromArgs([]string{"--home", home, "--base", t.TempDir(), "--binary", "missing"}, time.Now()); err == nil {
		t.Fatal("refresh CLI reported success after manifest read failed")
	}
}

func TestRefreshCLIRefusesIncompleteLane(t *testing.T) {
	home, base := t.TempDir(), t.TempDir()
	binary := filepath.Join(t.TempDir(), "compound-synthetic")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	writeInstalledHooksManifest(t, home, map[string]string{"codex": "binary:" + binary})
	if err := RunCLIFromArgs([]string{"--home", home, "--base", base, "--binary", binary}, time.Now()); err == nil {
		t.Fatal("refresh CLI reported success for an unbound installed lane")
	}
}

func TestRefreshCLIAllowsEmptyFreshHome(t *testing.T) {
	if err := RunCLIFromArgs([]string{"--home", t.TempDir(), "--base", t.TempDir(), "--binary", "unused"}, time.Now()); err != nil {
		t.Fatalf("fresh home has no required bindings: %v", err)
	}
}
