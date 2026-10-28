package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the real CLI so startup admission and the product parser are both tested.
func TestProductHelpExitsZeroAndDoesNotOpenHome(t *testing.T) {
	t.Parallel()
	binary := compoundTestBinary(t)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	unusableHome := filepath.Join(blocker, "unusable-home")
	validHome := initializedTestHome(t)
	tests := []struct {
		name      string
		args      []string
		exit      int
		validHome bool
		stderr    string
	}{
		{name: "help-long", args: []string{"--help"}},
		{name: "help-short", args: []string{"-h"}},
		{name: "empty-valid", exit: 2, validHome: true, stderr: "usage: compound product"},
		{name: "unknown-valid", args: []string{"unknown-subcommand"}, exit: 2, validHome: true, stderr: "usage: compound product"},
		{name: "malformed-valid", args: []string{"authority", "--unknown-flag"}, exit: 2, validHome: true, stderr: "usage: compound product authority"},
		{name: "help-extra-valid", args: []string{"--help", "import", "payload"}, exit: 2, validHome: true, stderr: "usage: compound product"},
		{name: "empty-unusable", exit: 1},
		{name: "unknown-unusable", args: []string{"unknown-subcommand"}, exit: 1},
		{name: "malformed-unusable", args: []string{"authority", "--unknown-flag"}, exit: 1},
		{name: "trailing-help", args: []string{"import", "payload", "--help"}, exit: 1},
		{name: "help-before-mutation", args: []string{"--help", "import", "payload"}, exit: 1},
		{name: "help-extra-flag", args: []string{"--help", "--unknown"}, exit: 1},
		{name: "short-help-extra", args: []string{"-h", "import", "payload"}, exit: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := unusableHome
			if tt.validHome {
				homePath = validHome
			}
			cmd := exec.Command(binary, append([]string{"product"}, tt.args...)...)
			cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			exit := 0
			if err != nil {
				failure, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run CLI: %v", err)
				}
				exit = failure.ExitCode()
			}
			if exit != tt.exit {
				t.Fatalf("exit=%d, want %d; stdout=%s; stderr=%s", exit, tt.exit, &stdout, &stderr)
			}
			if tt.exit == 0 {
				if stdout.String() != productUsage()+"\n" || stderr.Len() != 0 {
					t.Fatalf("help streams: stdout=%q stderr=%q", &stdout, &stderr)
				}
			} else {
				want := tt.stderr
				if !tt.validHome {
					want = "not a directory"
				}
				if stdout.Len() != 0 || !strings.Contains(stderr.String(), want) {
					t.Fatalf("failure streams: stdout=%q stderr=%q, want %q", &stdout, &stderr, want)
				}
			}
		})
	}
}
