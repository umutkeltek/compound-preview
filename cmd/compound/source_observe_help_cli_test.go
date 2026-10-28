package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Run the real CLI so startup admission and the source observe parser are both tested,
// mirroring the product root-help regression test's subprocess pattern.
func TestSourceObserveSoleHelpExitsZeroWithoutOpeningHome(t *testing.T) {
	t.Parallel()
	binary := compoundTestBinary(t)

	// A regular file occupying the home's parent path makes any home access
	// fail with "not a directory", proving help never resolves a home.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	// A home whose parent does not even exist yet proves help never creates a home.
	missingParent := filepath.Join(t.TempDir(), "missing-parent")
	missingHome := filepath.Join(missingParent, "home")

	homes := []struct {
		name     string
		homePath string
	}{
		{name: "valid-initialized-home", homePath: initializedTestHome(t)},
		{name: "nonexistent-home", homePath: missingHome},
		{name: "blocked-unusable-home", homePath: filepath.Join(blocker, "unusable-home")},
	}
	for _, homeCase := range homes {
		t.Run(homeCase.name, func(t *testing.T) {
			t.Parallel()
			before := snapshotHomeTree(t, homeCase.homePath)
			for _, help := range []string{"--help", "-h"} {
				cmd := exec.Command(binary, "source", "observe", help)
				cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homeCase.homePath)
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
				if exit != 0 {
					t.Fatalf("source observe %s exit=%d, want 0; stdout=%s; stderr=%s", help, exit, &stdout, &stderr)
				}
				want := "usage: compound source observe <source-observation.yaml>\n"
				if stdout.String() != want || stderr.Len() != 0 {
					t.Fatalf("source observe %s streams: stdout=%q stderr=%q, want stdout=%q with empty stderr", help, &stdout, &stderr, want)
				}
			}
			// The whole home tree (observed candidates, writeback/no-promote,
			// events, ledger, manifest) must stay byte-for-byte identical.
			if deltas := diffHomeTrees(before, snapshotHomeTree(t, homeCase.homePath)); len(deltas) != 0 {
				t.Fatalf("source observe help mutated home %s:\n%s", homeCase.homePath, strings.Join(deltas, "\n"))
			}
			if homeCase.name == "nonexistent-home" {
				if _, err := os.Lstat(missingParent); !os.IsNotExist(err) {
					t.Fatalf("help must not create the missing home parent: %v", err)
				}
			}
			if homeCase.name == "blocked-unusable-home" {
				got, err := os.ReadFile(blocker)
				if err != nil || string(got) != "blocked" {
					t.Fatalf("help must not touch the blocked home parent: content=%q err=%v", got, err)
				}
			}
		})
	}
}

// Keep the surrounding refusals meaningful: usage errors on a valid home,
// admission refusal on an unusable home, malformed help shapes that never
// become successful help, and invalid/missing observation input failure.
func TestSourceObserveRefusalShapesStayMeaningful(t *testing.T) {
	t.Parallel()
	binary := compoundTestBinary(t)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	unusableHome := filepath.Join(blocker, "unusable-home")
	validHome := initializedTestHome(t)

	invalidObservation := filepath.Join(t.TempDir(), "invalid-observation.yaml")
	if err := os.WriteFile(invalidObservation, []byte("schema_version: compound.source_observation.v0.wrong\nid: srcobs_invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		args      []string
		exit      int
		validHome bool
		stderr    string
	}{
		{name: "no-arg", exit: 2, validHome: true, stderr: "usage: compound source observe"},
		{name: "extra-arg", args: []string{"one.yaml", "two.yaml"}, exit: 2, validHome: true, stderr: "usage: compound source observe"},
		{name: "trailing-help", args: []string{"obs.yaml", "--help"}, exit: 2, validHome: true, stderr: "usage: compound source observe"},
		{name: "leading-help-extra", args: []string{"--help", "obs.yaml"}, exit: 2, validHome: true, stderr: "usage: compound source observe"},
		{name: "help-extra-flag", args: []string{"--help", "--json"}, exit: 2, validHome: true, stderr: "usage: compound source observe"},
		{name: "missing-observation", args: []string{"does-not-exist.yaml"}, exit: 1, validHome: true, stderr: "source observe: read source observation"},
		{name: "invalid-observation", args: []string{invalidObservation}, exit: 1, validHome: true, stderr: "source observe:"},
		{name: "no-arg-unusable", exit: 1, stderr: "not a directory"},
		{name: "trailing-help-unusable", args: []string{"obs.yaml", "--help"}, exit: 1, stderr: "not a directory"},
		{name: "help-extra-unusable", args: []string{"--help", "--json"}, exit: 1, stderr: "not a directory"},
		{name: "sole-help-unusable-mutation-neighbor", args: []string{"does-not-exist.yaml"}, exit: 1, stderr: "not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			homePath := unusableHome
			if tt.validHome {
				homePath = validHome
			}
			cmd := exec.Command(binary, append([]string{"source", "observe"}, tt.args...)...)
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
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), tt.stderr) {
				t.Fatalf("failure streams: stdout=%q stderr=%q, want %q on stderr with empty stdout", &stdout, &stderr, tt.stderr)
			}
		})
	}
}

// snapshotHomeTree records a stable content+mode fingerprint of every entry
// under root. A missing root snapshots as empty so the same helper also proves
// a nonexistent home was never created.
func snapshotHomeTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A root that is absent (never-created home) or unreachable under a
			// blocking file snapshots as empty: the after-run snapshot must stay
			// equally empty, which is the no-home-created proof.
			if path == root {
				return nil
			}
			return walkErr
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		switch {
		case entry.IsDir():
			snapshot[relative] = fmt.Sprintf("dir mode=%o", info.Mode().Perm())
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sum := sha256.Sum256(data)
			snapshot[relative] = fmt.Sprintf("file mode=%o size=%d sha256=%s", info.Mode().Perm(), len(data), hex.EncodeToString(sum[:]))
		default:
			return fmt.Errorf("unexpected home entry %s (%s)", path, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot home tree %s: %v", root, err)
	}
	return snapshot
}

func diffHomeTrees(before, after map[string]string) []string {
	paths := make(map[string]struct{}, len(before)+len(after))
	for path := range before {
		paths[path] = struct{}{}
	}
	for path := range after {
		paths[path] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	var deltas []string
	for _, path := range ordered {
		beforeState, inBefore := before[path]
		afterState, inAfter := after[path]
		switch {
		case !inBefore:
			deltas = append(deltas, "created "+path+" "+afterState)
		case !inAfter:
			deltas = append(deltas, "removed "+path+" "+beforeState)
		case beforeState != afterState:
			deltas = append(deltas, "changed "+path+" "+beforeState+" -> "+afterState)
		}
	}
	return deltas
}
