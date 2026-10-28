package main

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func TestRefreshPreflightThroughMainPreservesHomeAdmission(t *testing.T) {
	const childEnv = "COMPOUND_TEST_REFRESH_PREFLIGHT_MAIN"
	if raw := os.Getenv(childEnv); raw != "" {
		os.Args = append([]string{"compound"}, strings.Split(raw, "\x1f")...)
		main()
		os.Exit(0)
	}
	for _, tc := range []struct {
		name, manifest, external, want string
		ordinary                       bool
		native                         bool
		extraArgs                      []string
		wantExit                       int
	}{
		{name: "absent", want: "[]"},
		{name: "ordinary-absent", ordinary: true, wantExit: 1, want: "home manifest is missing"},
		{name: "existing-missing", manifest: "missing", wantExit: 1, want: "home manifest is missing"},
		{name: "existing-corrupt", manifest: "schema_version: [", wantExit: 1, want: "parse home manifest"},
		{name: "existing-incompatible", manifest: "schema_version: compound.home.v99\n", wantExit: 1, want: "unsupported home schema"},
		{name: "existing-valid", manifest: "valid", want: "[]"},
		{name: "ordinary-unknown", manifest: "valid", ordinary: true, native: true, extraArgs: []string{"--unknown"}, wantExit: 1, want: `unknown refresh-lane argument "--unknown"`},
		{name: "preflight-unknown", manifest: "valid", native: true, extraArgs: []string{"--unknown"}, wantExit: 1, want: `unknown refresh-lane argument "--unknown"`},
		{name: "absent-preflight-unknown", extraArgs: []string{"--unknown"}, wantExit: 1, want: `unknown refresh-lane argument "--unknown"`},
		{name: "help-unknown", manifest: "valid", native: true, extraArgs: []string{"--help", "--unknown"}, wantExit: 1, want: `unknown refresh-lane argument "--unknown"`},
		{name: "preflight-as-value", manifest: "valid", ordinary: true, native: true, extraArgs: []string{"--binary", "--preflight"}, wantExit: 1, want: "--binary requires a value"},
		// An adapter with a known installer is reported to that owner and exits 0:
		// it holds no pinned binary identity, so it cannot be left half-activated
		// by publication. Exiting 1 here made `./install.sh` unable to publish at
		// all on any home that enrolled one.
		{name: "external-omp", manifest: "valid", external: "oh_my_pi", wantExit: 0, want: "compound hook install --harness oh-my-pi"},
		{name: "external-opencode", manifest: "valid", external: "opencode", wantExit: 0, want: "adapters/opencode/install.ts"},
		{name: "external-unknown", manifest: "valid", external: "unrecognized", wantExit: 1, want: "no native hook refresh owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			homePath, baseDir := filepath.Join(root, "home"), filepath.Join(root, "profile")
			if tc.manifest == "valid" {
				if err := home.Init(homePath, home.HomeKindGlobal, time.Now()); err != nil {
					t.Fatal(err)
				}
			} else if tc.manifest != "" {
				if err := os.Mkdir(homePath, 0700); err != nil {
					t.Fatal(err)
				}
				if tc.manifest != "missing" {
					if err := os.WriteFile(filepath.Join(homePath, home.ManifestName), []byte(tc.manifest), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.native || tc.external != "" {
				manifest := "schema_version: compound.installed_hooks.v0.1\nhooks:\n"
				harnesses := []string{"codex"}
				if tc.external != "" {
					harnesses = append(harnesses, tc.external)
				}
				for _, name := range harnesses {
					manifest += "  - harness: " + name + "\n    name: PostToolUse\n    event: PostToolUse\n    ref: compound\n    revision: synthetic\n    status: current\n"
				}
				if err := os.WriteFile(filepath.Join(homePath, "config", "installed-hooks.yaml"), []byte(manifest), 0640); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(baseDir, ".codex"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(baseDir, ".codex", "hooks.json"), []byte("{\"synthetic_setting\":\"preserve\"}\n"), 0640); err != nil {
					t.Fatal(err)
				}
			}
			before := refreshPreflightFixtureSnapshot(t, root)
			args := []string{"harness", "refresh-lane", "--home", homePath, "--base", baseDir, "--binary", filepath.Join(root, "compound-synthetic"), "--json"}
			if !tc.ordinary {
				args = append(args, "--preflight")
			}
			args = append(args, tc.extraArgs...)
			cmd := exec.Command(os.Args[0], "-test.run=^TestRefreshPreflightThroughMainPreservesHomeAdmission$")
			cmd.Env = append(os.Environ(), childEnv+"="+strings.Join(args, "\x1f"), "COMPOUND_HOME="+homePath)
			out, err := cmd.CombinedOutput()
			if got := cmd.ProcessState.ExitCode(); got != tc.wantExit || !strings.Contains(string(out), tc.want) {
				t.Fatalf("exit=%d, want=%d; error=%v; output missing %q:\n%s", got, tc.wantExit, err, tc.want, out)
			}
			if after := refreshPreflightFixtureSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("preflight or refused refresh changed home/profile preimages: before=%v after=%v", before, after)
			}
		})
	}
}

func refreshPreflightFixtureSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		body := []byte(nil)
		if entry.Type().IsRegular() {
			body, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		out[path] = fmt.Sprintf("%s:%x", info.Mode(), sha256.Sum256(body))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
