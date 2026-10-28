package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const presenceCLIBoundarySubEnv = "COMPOUND_PRESENCE_CLI_BOUNDARY_SUB"

// TestPresenceCLIReadOnlyDoesNotMaterializeDirs proves the registry's read-only
// contract in fact, not just in label: `compound presence --json` is registered
// via readCommand, so running it against a home WITHOUT work/ or events/ must
// exit 0, print the projection, and leave the home exactly as it found it —
// no work/, no events/, nothing. The previous implementation opened the store
// with work.Open, which MkdirAll'd work/* and events/ on every inspection, so
// "who is here?" left footprints in COMPOUND_HOME.
func TestPresenceCLIReadOnlyDoesNotMaterializeDirs(t *testing.T) {
	t.Parallel()
	if os.Getenv(presenceCLIBoundarySubEnv) == "read-json" {
		cmdPresence([]string{"--json"})
		return
	}
	homePath := initializedTestHome(t)
	// A fresh init scaffolds the habitat; strip work/ and events/ to stand in
	// for a minimal or migrated home where the read path must not materialize.
	for _, d := range []string{"work", "events"} {
		if err := os.RemoveAll(filepath.Join(homePath, d)); err != nil {
			t.Fatalf("strip %s: %v", d, err)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPresenceCLIReadOnlyDoesNotMaterializeDirs$")
	cmd.Env = append(os.Environ(), presenceCLIBoundarySubEnv+"=read-json", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("presence --json must exit 0 on a bare home, got %v output=%s", err, output)
	}
	if !strings.Contains(string(output), "compound.presence_projection.v0.1") {
		t.Fatalf("presence --json must print the projection, got %s", output)
	}
	for _, d := range []string{"work", "events"} {
		if _, statErr := os.Stat(filepath.Join(homePath, d)); !os.IsNotExist(statErr) {
			t.Fatalf("read-only presence must not create %s/ (stat err=%v)", d, statErr)
		}
	}
}

// TestPresenceCLIDoesNotAddLandChecksOnFreshHome narrows the same contract to a
// normally-initialized home: even where work/ already exists, a read-class
// command must not grow it. work.Open used to MkdirAll work/land-checks on
// every presence inspection; OpenReadOnly must not.
func TestPresenceCLIDoesNotAddLandChecksOnFreshHome(t *testing.T) {
	t.Parallel()
	if os.Getenv(presenceCLIBoundarySubEnv) == "read-fresh" {
		cmdPresence([]string{"--json"})
		return
	}
	homePath := initializedTestHome(t)
	if _, err := os.Stat(filepath.Join(homePath, "work", "land-checks")); !os.IsNotExist(err) {
		t.Fatalf("precondition: fresh home must not have work/land-checks (err=%v)", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPresenceCLIDoesNotAddLandChecksOnFreshHome$")
	cmd.Env = append(os.Environ(), presenceCLIBoundarySubEnv+"=read-fresh", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("presence --json must exit 0 on a fresh home, got %v output=%s", err, output)
	}
	if _, statErr := os.Stat(filepath.Join(homePath, "work", "land-checks")); !os.IsNotExist(statErr) {
		t.Fatalf("read-only presence must not create work/land-checks (stat err=%v)", statErr)
	}
}
