package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/home"
)

const orientIdentityChildEnv = "COMPOUND_TEST_ORIENT_IDENTITY_CHILD"
const orientIdentityArgsEnv = "COMPOUND_TEST_ORIENT_IDENTITY_ARGS"

func TestOrientCLIRejectsFictitiousIdentityBeforeWriting(t *testing.T) {
	t.Parallel()
	if os.Getenv(orientIdentityChildEnv) == "1" {
		var args []string
		if err := json.Unmarshal([]byte(os.Getenv(orientIdentityArgsEnv)), &args); err != nil {
			panic(err)
		}
		cmdOrient(args)
		os.Exit(0)
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	validCWD := t.TempDir()
	fileCWD := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(fileCWD, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "unknown harness",
			args: []string{"--harness", "invented-agent", "--cwd", validCWD, "--task", "orient"},
			want: "is not registered",
		},
		{
			name: "missing cwd",
			args: []string{"--harness", "codex", "--cwd", filepath.Join(t.TempDir(), "missing"), "--task", "orient"},
			want: "is unavailable",
		},
		{
			name: "cwd is a file",
			args: []string{"--harness", "codex", "--cwd", fileCWD, "--task", "orient"},
			want: "is not a directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := orientIdentityTree(t, homePath)
			encoded, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestOrientCLIRejectsFictitiousIdentityBeforeWriting$/"+tc.name+"$")
			cmd.Env = orientIdentityEnv(os.Environ(),
				"COMPOUND_HOME="+homePath,
				orientIdentityChildEnv+"=1",
				orientIdentityArgsEnv+"="+string(encoded),
			)
			output, runErr := cmd.CombinedOutput()
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok {
				t.Fatalf("orient run error = %v, want process exit; output=%s", runErr, output)
			}
			if got := exitErr.ExitCode(); got != 1 {
				t.Fatalf("orient exit = %d, want 1; output=%s", got, output)
			}
			if !strings.Contains(string(output), tc.want) {
				t.Fatalf("orient output = %q, want %q", output, tc.want)
			}
			after := orientIdentityTree(t, homePath)
			if !maps.Equal(after, before) {
				t.Fatalf("rejected orient mutated COMPOUND_HOME:\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestOrientCLIWritesCanonicalRegisteredIdentity(t *testing.T) {
	t.Parallel()
	if os.Getenv(orientIdentityChildEnv) == "1" {
		var args []string
		if err := json.Unmarshal([]byte(os.Getenv(orientIdentityArgsEnv)), &args); err != nil {
			panic(err)
		}
		cmdOrient(args)
		os.Exit(0)
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	realCWD := t.TempDir()
	requestedCWD := realCWD
	if runtime.GOOS != "windows" {
		alias := filepath.Join(t.TempDir(), "workspace-alias")
		if err := os.Symlink(realCWD, alias); err != nil {
			t.Fatal(err)
		}
		requestedCWD = alias
	}
	args := []string{"--harness", "claude-code", "--cwd", requestedCWD, "--task", "orient identity"}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOrientCLIWritesCanonicalRegisteredIdentity$")
	cmd.Env = orientIdentityEnv(os.Environ(),
		"COMPOUND_HOME="+homePath,
		orientIdentityChildEnv+"=1",
		orientIdentityArgsEnv+"="+string(encoded),
	)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("orient failed: %v", err)
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal(output, &pack); err != nil {
		t.Fatalf("decode ContextPack: %v\n%s", err, output)
	}
	wantCWD, err := filepath.EvalSymlinks(realCWD)
	if err != nil {
		t.Fatal(err)
	}
	wantCWD, err = filepath.Abs(wantCWD)
	if err != nil {
		t.Fatal(err)
	}
	if pack.Scope.Surface != filepath.Clean(wantCWD) {
		t.Fatalf("surface = %q, want %q", pack.Scope.Surface, filepath.Clean(wantCWD))
	}
	if !strings.Contains(pack.SummaryMD, "for `claude_code`") {
		t.Fatalf("summary = %q, want canonical claude_code identity", pack.SummaryMD)
	}
}

func orientIdentityEnv(base []string, replacements ...string) []string {
	keys := map[string]bool{}
	for _, replacement := range replacements {
		key, _, _ := strings.Cut(replacement, "=")
		keys[key] = true
	}
	out := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if !keys[key] {
			out = append(out, entry)
		}
	}
	return append(out, replacements...)
}

func orientIdentityTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		identity := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			identity += ":" + target
		case info.Mode().IsRegular():
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			identity += fmt.Sprintf(":%x", sha256.Sum256(body))
		}
		out[rel] = identity
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
