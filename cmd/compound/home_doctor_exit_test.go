package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"compound/internal/home"
)

const homeDoctorExitChildEnv = "COMPOUND_TEST_HOME_DOCTOR_EXIT_CHILD"

func TestHomeDoctorJSONFailureExitsNonZero(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeDoctorExitChildEnv) == "1" {
		cmdHome([]string{"doctor", "--json"})
		return
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "global", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(homePath, "invented-feature-root"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHomeDoctorJSONFailureExitsNonZero$")
	cmd.Env = append(os.Environ(), homeDoctorExitChildEnv+"=1", "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("home doctor --json failure exit = %v, want exit 1; output:\n%s", err, out)
	}
	var report homeDoctorJSONReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parse doctor JSON: %v\n%s", err, out)
	}
	if report.Verdict != "fail" || report.Habitat.OK {
		t.Fatalf("doctor failure report = %+v", report)
	}
}
