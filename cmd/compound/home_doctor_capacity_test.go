package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/home"
)

const homeDoctorCapacityChildEnv = "COMPOUND_TEST_HOME_DOCTOR_CAPACITY_CHILD"

// F8 (work_9928ccb3621b): `compound home doctor --json` must surface the non-fatal
// capacity.blackout:<family> watchdog as an additive field, without ever gating the doctor
// verdict on it. Follows home_doctor_exit_test.go's subprocess convention so the assertion
// exercises the REAL CLI path (cmdHome -> buildHomeDoctorReport), not just the helper in
// isolation — "landed != working" requires watching the actual consumer's output.
func TestHomeDoctorCapacityFeedWarnings(t *testing.T) {
	t.Parallel()
	if os.Getenv(homeDoctorCapacityChildEnv) == "1" {
		cmdHome([]string{"doctor", "--json"})
		// A verdict:"ok" doctor run falls through to a plain return inside cmdHome (only a
		// non-"ok" verdict calls os.Exit(1)), which would otherwise let this child process fall
		// back into the `go test` binary's own runner and append its "PASS"/ok summary line
		// after the JSON on stdout. Exit explicitly so the parent's captured stdout is bare JSON.
		os.Exit(0)
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "global", cliTestNow); err != nil {
		t.Fatal(err)
	}
	projectionsDir := filepath.Join(homePath, "projections")
	if err := os.MkdirAll(projectionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A claude row that is real (not zero rows -> not-enrolled) but 6h stale is comfortably past
	// 4x claude's own 15m freshness window (60m) -- freshened at runtime (time.Now(), not a fixed
	// fixture date) so this falsifier cannot red days after a green land.
	staleClaudeRow := map[string]any{
		"provider":      "claude",
		"windowKind":    "5h",
		"windowMinutes": 300,
		"usedPercent":   10.0,
		"resetsAt":      time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339),
		"sampledAt":     time.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(staleClaudeRow)
	if err != nil {
		t.Fatal(err)
	}
	supplementPath := filepath.Join(projectionsDir, "usage-supplement.jsonl")
	if err := os.WriteFile(supplementPath, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// Isolate os.UserHomeDir() so a real ~/Library/Application Support/CodexBar/usage-history.jsonl
	// on the machine running this test can never leak into the assertion. The doctor child runs
	// `go version`; a plain TempDir HOME lets Go telemetry write after the child exits and race
	// the cleanup (5 of 40 runs red in the Linux CI image).
	fakeUserHome := isolatedTestUserHome(t)

	cmd := exec.Command(os.Args[0], "-test.run=^TestHomeDoctorCapacityFeedWarnings$")
	cmd.Env = append(os.Environ(),
		homeDoctorCapacityChildEnv+"=1",
		"COMPOUND_HOME="+homePath,
		"HOME="+fakeUserHome,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("home doctor --json: %v\noutput:\n%s", err, out)
	}

	var report homeDoctorJSONReport
	if unmarshalErr := json.Unmarshal(out, &report); unmarshalErr != nil {
		t.Fatalf("parse doctor JSON: %v\n%s", unmarshalErr, out)
	}
	if report.Verdict != "ok" {
		t.Fatalf("verdict = %q, want ok (capacity_feed_warnings must never gate the verdict): %+v", report.Verdict, report)
	}
	var found bool
	for _, w := range report.CapacityFeedWarnings {
		if w.Name != "capacity.blackout:claude" {
			continue
		}
		found = true
		if w.OK {
			t.Fatalf("capacity.blackout:claude must report ok=false: %+v", w)
		}
		if w.Detail == "" {
			t.Fatalf("capacity.blackout:claude must carry a detail: %+v", w)
		}
	}
	if !found {
		t.Fatalf("capacity_feed_warnings missing capacity.blackout:claude: %+v", report.CapacityFeedWarnings)
	}
}
