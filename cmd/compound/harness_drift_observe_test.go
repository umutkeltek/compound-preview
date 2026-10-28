package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/observation"
)

// writeHarnessDriftObserveFixture reproduces the exact clean-baseline fixture
// shape used by internal/harness's own
// TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys, but built from this
// package (exported harness symbols only) since the unexported test helpers
// in internal/harness/permissions_test.go are not reachable from cmd/compound.
// It returns the rendered (intended) content so a test can mutate the live
// target file into a drifted state.
func writeHarnessDriftObserveFixture(t *testing.T) (homePath, targetPath string, render harness.PermissionRender) {
	t.Helper()
	homePath, targetPath = t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeHarnessDriftObserveJSON(t, targetPath, map[string]any{"theme": "dark"})
	policy := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow: []string{"Read", "mcp__gbrain__*"}, Deny: []string{"Bash(rm:*)"}, Ask: []string{"WebFetch"}, DefaultMode: "default",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
	writeHarnessDriftObserveJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), policy)

	render, err := harness.RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := harness.WriteHarnessArtifactManifest(homePath, []harness.HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}
	return homePath, targetPath, render
}

func writeHarnessDriftObserveJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// driftHarnessFixture mutates the target settings file to a drifted state
// (an unmanaged mcp__* wildcard rule, the same drift TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys
// exercises) and returns the freshly-probed PermissionDriftReport.
func driftHarnessFixture(t *testing.T, homePath, targetPath string, render harness.PermissionRender) harness.PermissionDriftReport {
	t.Helper()
	var settings map[string]any
	if err := json.Unmarshal(render.Content, &settings); err != nil {
		t.Fatal(err)
	}
	permissions := settings["permissions"].(map[string]any)
	permissions["allow"] = []string{"Read", "mcp__gbrain__*", "mcp__*"}
	writeHarnessDriftObserveJSON(t, targetPath, settings)
	report, err := harness.PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drifted {
		t.Fatalf("fixture should be drifted: %+v", report)
	}
	return report
}

// countFiles recursively counts regular files under root (missing root -> 0).
func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestEmitPermissionPostureDivergence_F1_EmitOnDrift is the F1 falsifier:
// a drifted fixture + the bridge must apply exactly one observation batch
// under the test $COMPOUND_HOME.
func TestEmitPermissionPostureDivergence_F1_EmitOnDrift(t *testing.T) {
	homePath, targetPath, render := writeHarnessDriftObserveFixture(t)
	report := driftHarnessFixture(t, homePath, targetPath, render)

	emission, err := emitPermissionPostureDivergence(homePath, report, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if !emission.Applied {
		t.Fatalf("expected an applied emission, got %+v", emission)
	}
	if emission.ObservationID == "" || emission.SourcePath == "" {
		t.Fatalf("emission missing observation id/source path: %+v", emission)
	}
	if _, err := os.Stat(emission.SourcePath); err != nil {
		t.Fatalf("emitted batch not persisted at %s: %v", emission.SourcePath, err)
	}
	projection, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projection.RecordCount != 1 {
		t.Fatalf("expected exactly one intake record applied, got %d", projection.RecordCount)
	}
}

// TestEmitPermissionPostureDivergence_F2_CardEndToEnd is the F2 falsifier:
// the emission must yield a divergence_declared card in
// LoadSourceIntakeProjection, driven through the real bridge (not a
// hand-built fixture batch, per intake_divergence_test.go's own pattern).
func TestEmitPermissionPostureDivergence_F2_CardEndToEnd(t *testing.T) {
	homePath, targetPath, render := writeHarnessDriftObserveFixture(t)
	report := driftHarnessFixture(t, homePath, targetPath, render)

	if _, err := emitPermissionPostureDivergence(homePath, report, cliTestNow); err != nil {
		t.Fatal(err)
	}
	projection, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	var card *observation.SourceIntakeActionCard
	for i := range projection.ActionCards {
		if projection.ActionCards[i].Kind == "divergence_declared" {
			card = &projection.ActionCards[i]
			break
		}
	}
	if card == nil {
		t.Fatalf("expected a divergence_declared card, got: %+v", projection.ActionCards)
	}
}

// TestEmitPermissionPostureDivergence_F3_SilenceOnMatch is the F3 falsifier:
// a non-drifted report must apply zero observations and produce zero cards.
func TestEmitPermissionPostureDivergence_F3_SilenceOnMatch(t *testing.T) {
	homePath, _, _ := writeHarnessDriftObserveFixture(t)
	report, err := harness.PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted {
		t.Fatalf("fixture should be clean: %+v", report)
	}
	filesBefore := countFiles(t, homePath)

	emission, err := emitPermissionPostureDivergence(homePath, report, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if emission.Applied {
		t.Fatalf("clean report must not emit, got %+v", emission)
	}
	if got := countFiles(t, homePath); got != filesBefore {
		t.Fatalf("clean report must write nothing: files before=%d after=%d", filesBefore, got)
	}
	projection, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projection.RecordCount != 0 || len(projection.ActionCards) != 0 {
		t.Fatalf("clean report must produce zero intake records/cards, got records=%d cards=%+v", projection.RecordCount, projection.ActionCards)
	}
}

// TestEmitPermissionPostureDivergence_F4_StableIdentity is the F4 falsifier:
// re-emitting on the SAME drift state dedupes to one card; mutating to a
// DIFFERENT drift state mints a second, distinct card.
func TestEmitPermissionPostureDivergence_F4_StableIdentity(t *testing.T) {
	homePath, targetPath, render := writeHarnessDriftObserveFixture(t)
	report := driftHarnessFixture(t, homePath, targetPath, render)

	first, err := emitPermissionPostureDivergence(homePath, report, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	reportAgain, err := harness.PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	second, err := emitPermissionPostureDivergence(homePath, reportAgain, cliTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.ObservationID != second.ObservationID {
		t.Fatalf("re-emitting the same drift state must reproduce the same observation id: %q vs %q", first.ObservationID, second.ObservationID)
	}

	projection, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if count := countDivergenceCards(projection); count != 1 {
		t.Fatalf("same drift state emitted twice must dedupe to ONE card, got %d: %+v", count, projection.ActionCards)
	}

	// Mutate to a genuinely different drift state and re-emit.
	var settings map[string]any
	if err := json.Unmarshal(render.Content, &settings); err != nil {
		t.Fatal(err)
	}
	permissions := settings["permissions"].(map[string]any)
	permissions["allow"] = []string{"mcp__gbrain__*"}
	writeHarnessDriftObserveJSON(t, targetPath, settings)
	reportChanged, err := harness.PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if !reportChanged.Drifted {
		t.Fatalf("changed fixture should still be drifted: %+v", reportChanged)
	}
	third, err := emitPermissionPostureDivergence(homePath, reportChanged, cliTestNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if third.ObservationID == first.ObservationID {
		t.Fatalf("a different drift state must mint a different observation id, got the same: %q", third.ObservationID)
	}

	projectionAfter, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow.Add(2*time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if count := countDivergenceCards(projectionAfter); count != 2 {
		t.Fatalf("a distinct drift state must add a second, distinct card (total 2), got %d: %+v", count, projectionAfter.ActionCards)
	}
}

func countDivergenceCards(projection observation.SourceIntakeProjection) int {
	n := 0
	for _, card := range projection.ActionCards {
		if card.Kind == "divergence_declared" {
			n++
		}
	}
	return n
}

const harnessDriftExitChildEnv = "COMPOUND_TEST_HARNESS_DRIFT_EXIT_CHILD"

// TestHarnessDriftWithoutFlag_F6_DisplayCompat is the F6 falsifier: without
// --emit-divergence, `compound harness drift --harness <h> --json` must
// behave byte-identically to base -- same JSON shape (no
// observation_applied/observation_id side channel), same exit code, and
// (the substantive no-regression proof) zero new files under $COMPOUND_HOME:
// the emission code path must never run without the flag.
func TestHarnessDriftWithoutFlag_F6_DisplayCompat(t *testing.T) {
	// In-process half: a CLEAN fixture never hits the os.Exit(1) path, so it
	// can run directly in this test binary.
	t.Run("clean_fixture_in_process", func(t *testing.T) {
		homePath, _, _ := writeHarnessDriftObserveFixture(t)
		t.Setenv("COMPOUND_HOME", homePath)
		filesBefore := countFiles(t, homePath)

		out := captureStdout(t, func() { cmdHarness([]string{"drift", "--harness", "claude_code", "--json"}) })
		var report harness.PermissionDriftReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("parse drift json: %v\n%s", err, out)
		}
		if report.Drifted {
			t.Fatalf("fixture should be clean: %+v", report)
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["observation_applied"]; ok {
			t.Fatalf("no-flag output must not carry observation_applied: %s", out)
		}
		if got := countFiles(t, homePath); got != filesBefore {
			t.Fatalf("no-flag drift must write nothing: files before=%d after=%d", filesBefore, got)
		}
	})

	// Subprocess half: a DRIFTED fixture without the flag must still exit 1
	// (unchanged prior behavior) and must still emit nothing, even though the
	// bridge exists and would trigger on --emit-divergence.
	if os.Getenv(harnessDriftExitChildEnv) == "1" {
		cmdHarness([]string{"drift", "--harness", "claude_code", "--json"})
		return
	}
	t.Run("drifted_fixture_subprocess_exit_unchanged", func(t *testing.T) {
		homePath, targetPath, render := writeHarnessDriftObserveFixture(t)
		driftHarnessFixture(t, homePath, targetPath, render)
		filesBefore := countFiles(t, homePath)

		cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessDriftWithoutFlag_F6_DisplayCompat$")
		cmd.Env = append(os.Environ(), harnessDriftExitChildEnv+"=1", "COMPOUND_HOME="+homePath)
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("drift --json (no flag) on a drifted fixture exit = %v, want exit 1; output:\n%s", err, out)
		}
		var report harness.PermissionDriftReport
		if err := json.Unmarshal(out, &report); err != nil {
			t.Fatalf("parse drift json: %v\n%s", err, out)
		}
		if !report.Drifted {
			t.Fatalf("fixture should still read as drifted: %+v", report)
		}
		var raw map[string]any
		if err := json.Unmarshal(out, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["observation_applied"]; ok {
			t.Fatalf("no-flag output must not carry observation_applied even when drifted: %s", out)
		}
		if got := countFiles(t, homePath); got != filesBefore {
			t.Fatalf("no-flag drift must write nothing even when drifted: files before=%d after=%d", filesBefore, got)
		}
	})
}
