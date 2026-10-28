package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var activeRootHelpCommands = []struct{ name, useful string }{
	{"finding", "usage: compound finding <record|list|show|doctor>"},
	{"knowledge", "usage: compound knowledge <import|extract>"},
	{"writeback", "compound writeback gate <gate.json> --policy <policy-decision.json>"},
	{"placement", "usage: compound placement <target|resolve>"},
	{"contract", "usage: compound contract <compile|import-hooks|hooks-authority>"},
	{"envelope", "usage: compound envelope <validate <file>...|kinds>"},
	{"schema", "usage: compound schema <list|export|validate>"},
	{"runtime", "usage: compound runtime doctor"},
}

// Use the real binary and independent text expectations: these tests also
// compile against the exact base. Missing-home checks include empty parents.
func TestActiveRootHelpSoleHelpExitsZeroBeforeHomeAccess(t *testing.T) {
	t.Parallel()
	binary := compoundTestBinary(t)
	protected := t.TempDir()
	blocker := filepath.Join(protected, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	missingParent := filepath.Join(protected, "missing-parent")
	validHome := initializedTestHome(t)
	for _, command := range activeRootHelpCommands {
		for _, flag := range []string{"--help", "-h"} {
			t.Run(command.name+"/"+flag, func(t *testing.T) {
				for _, selectedHome := range []string{filepath.Join(blocker, "home"), filepath.Join(missingParent, "home"), validHome} {
					before, beforeHome := activeHelpTree(t, protected), activeHelpTree(t, validHome)
					stdout, stderr, exit := runActiveHelpCLI(t, binary, selectedHome, command.name, flag)
					if exit != 0 || stderr != "" || !strings.Contains(stdout, command.useful) || !strings.HasSuffix(stdout, "\n") {
						t.Fatalf("home=%s exit=%d stdout=%q stderr=%q", selectedHome, exit, stdout, stderr)
					}
					if command.name == "finding" {
						for _, required := range []string{"--body", "--body-file", "--stdin", "Evidence-only", "Never a ledger byte"} {
							if !strings.Contains(stdout, required) {
								t.Fatalf("finding help lost %q: %s", required, stdout)
							}
						}
					}
					if _, err := os.Lstat(missingParent); !os.IsNotExist(err) {
						t.Fatalf("help created home parent: %v", err)
					}
					if !reflect.DeepEqual(before, activeHelpTree(t, protected)) || !reflect.DeepEqual(beforeHome, activeHelpTree(t, validHome)) {
						t.Fatal("help mutated home entries, modes, or contents")
					}
				}
			})
		}
	}
}

func TestActiveRootHelpPreservesInvalidAndRetiredAdmission(t *testing.T) {
	t.Parallel()
	binary := compoundTestBinary(t)
	protected := t.TempDir()
	blocker := filepath.Join(protected, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	validHome := initializedTestHome(t)
	unusableHome := filepath.Join(blocker, "home")
	invalid := [][]string{nil, {"unknown-verb"}, {"--help", "unknown-verb"}, {"-h", "unknown-verb"}, {"--help", "--unknown"}, {"unknown-verb", "--help"}, {"unknown-verb", "-h"}}
	for _, command := range activeRootHelpCommands {
		for _, args := range invalid {
			for _, selectedHome := range []string{validHome, unusableHome} {
				t.Run(command.name+"/"+strings.Join(args, "_")+"/"+filepath.Base(filepath.Dir(selectedHome)), func(t *testing.T) {
					before, beforeHome := activeHelpTree(t, protected), activeHelpTree(t, validHome)
					stdout, stderr, exit := runActiveHelpCLI(t, binary, selectedHome, append([]string{command.name}, args...)...)
					wantExit, wantError := 2, "usage:"
					if selectedHome == unusableHome && command.name != "placement" {
						wantExit, wantError = 1, "not a directory"
					}
					if exit != wantExit || stdout != "" || !strings.Contains(stderr, wantError) {
						t.Fatalf("exit=%d want=%d stdout=%q stderr=%q", exit, wantExit, stdout, stderr)
					}
					if !reflect.DeepEqual(before, activeHelpTree(t, protected)) || !reflect.DeepEqual(beforeHome, activeHelpTree(t, validHome)) {
						t.Fatal("invalid invocation mutated home")
					}
				})
			}
		}
	}
	before, beforeHome := activeHelpTree(t, protected), activeHelpTree(t, validHome)
	for _, args := range [][]string{{"finding", "record", "--help"}, {"knowledge", "import", "payload", "--help"}} {
		stdout, stderr, exit := runActiveHelpCLI(t, binary, validHome, args...)
		if exit != 2 || stdout != "" || !strings.Contains(stderr, "usage:") {
			t.Fatalf("trailing help %v: exit=%d stdout=%q stderr=%q", args, exit, stdout, stderr)
		}
	}
	for _, flag := range []string{"--help", "-h"} {
		stdout, stderr, exit := runActiveHelpCLI(t, binary, unusableHome, "recall", flag)
		if exit != 2 || stdout != "" || !strings.Contains(stderr, "recall retired") {
			t.Fatalf("retired help: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
		}
		stdout, stderr, exit = runActiveHelpCLI(t, binary, unusableHome, "product", flag)
		if exit != 0 || stderr != "" || !strings.Contains(stdout, "usage: compound product") {
			t.Fatalf("product help: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
		}
	}
	if !reflect.DeepEqual(before, activeHelpTree(t, protected)) || !reflect.DeepEqual(beforeHome, activeHelpTree(t, validHome)) {
		t.Fatal("control or trailing invocation mutated home")
	}
}

func runActiveHelpCLI(t *testing.T, binary, selectedHome string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+selectedHome)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		failure, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		exit = failure.ExitCode()
	}
	return stdout.String(), stderr.String(), exit
}

func activeHelpTree(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += ":" + target
		} else if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += ":" + string(body)
		}
		entries[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
