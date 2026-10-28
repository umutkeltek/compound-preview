package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const presenceReadOnlySubprocessEnv = "COMPOUND_PRESENCE_READ_ONLY_SUBPROCESS"

func TestPresenceCLIReadOnlyDoesNotMaterializeRuntimeDirs(t *testing.T) {
	t.Parallel()
	if os.Getenv(presenceReadOnlySubprocessEnv) == "1" {
		cmdPresence([]string{"--json"})
		return
	}

	homePath := initializedTestHome(t)
	for _, dir := range []string{"work", "events"} {
		if err := os.RemoveAll(filepath.Join(homePath, dir)); err != nil {
			t.Fatalf("strip %s: %v", dir, err)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPresenceCLIReadOnlyDoesNotMaterializeRuntimeDirs$")
	cmd.Env = append(os.Environ(), presenceReadOnlySubprocessEnv+"=1", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("presence --json must exit 0 on a bare home: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "compound.presence_projection.v0.1") {
		t.Fatalf("presence --json must emit a projection, got %s", output)
	}
	for _, dir := range []string{"work", "events"} {
		if _, statErr := os.Stat(filepath.Join(homePath, dir)); !os.IsNotExist(statErr) {
			t.Fatalf("read-only presence created %s/ (stat err=%v)", dir, statErr)
		}
	}
}

func TestPresenceCLIDoesNotAddLandChecks(t *testing.T) {
	t.Parallel()
	if os.Getenv(presenceReadOnlySubprocessEnv) == "2" {
		cmdPresence([]string{"--json"})
		return
	}

	homePath := initializedTestHome(t)
	landChecks := filepath.Join(homePath, "work", "land-checks")
	if _, err := os.Stat(landChecks); !os.IsNotExist(err) {
		t.Fatalf("precondition: fresh home has work/land-checks (err=%v)", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPresenceCLIDoesNotAddLandChecks$")
	cmd.Env = append(os.Environ(), presenceReadOnlySubprocessEnv+"=2", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("presence --json must exit 0 on a fresh home: %v\n%s", err, output)
	}
	if _, statErr := os.Stat(landChecks); !os.IsNotExist(statErr) {
		t.Fatalf("read-only presence created work/land-checks (stat err=%v)", statErr)
	}
}
