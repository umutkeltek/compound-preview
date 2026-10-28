package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// buildDriftFixture reproduces the exact fixture shape from
// TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys (permissions_test.go) so
// the observation builder is exercised against the SAME red/green states the
// PermissionDrift falsifier already proves, rather than inventing a parallel
// fixture. It returns the live render (intended) content alongside the
// homePath/targetPath so a caller can mutate the live target file and
// re-probe PermissionDrift.
func buildDriftFixture(t *testing.T) (homePath, targetPath string, render PermissionRender) {
	t.Helper()
	homePath, targetPath = t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read", "mcp__gbrain__*"}))
	render, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteHarnessArtifactManifest(homePath, []HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}
	return homePath, targetPath, render
}

// TestBuildPermissionPostureObservationSilentOnCleanReport is F3's
// package-level half: a non-drifted report must never produce an
// observation (silence-on-match, checked defensively even though the cmd
// bridge already gates on Drifted before calling this).
func TestBuildPermissionPostureObservationSilentOnCleanReport(t *testing.T) {
	homePath, _, render := buildDriftFixture(t)
	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted {
		t.Fatalf("fixture should be clean before mutation: %+v", report)
	}
	if _, ok := BuildPermissionPostureObservation(report, render.Content, render.Content); ok {
		t.Fatal("clean report must not produce a permission posture observation")
	}
}

// TestBuildPermissionPostureObservationDrifted is F1's package-level half:
// a drifted report must produce an observation whose expected/observed
// states differ (the divergence card's non-negotiable precondition).
func TestBuildPermissionPostureObservationDrifted(t *testing.T) {
	homePath, targetPath, render := buildDriftFixture(t)
	settings := decodeTestJSON(t, render.Content)
	permissions := settings["permissions"].(map[string]any)
	permissions["allow"] = []string{"Read", "mcp__gbrain__*", "mcp__*"}
	writeTestJSON(t, targetPath, settings)

	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drifted {
		t.Fatalf("fixture should be drifted: %+v", report)
	}
	live, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	obs, ok := BuildPermissionPostureObservation(report, render.Content, live)
	if !ok {
		t.Fatal("drifted report must produce a permission posture observation")
	}
	if obs.ExpectedState == "" || obs.ObservedState == "" || obs.ExpectedState == obs.ObservedState {
		t.Fatalf("expected/observed states must be non-empty and differ: expected=%q observed=%q", obs.ExpectedState, obs.ObservedState)
	}
	if obs.ObservationID == "" {
		t.Fatal("observation id must not be empty")
	}
	if obs.Title == "" {
		t.Fatal("title must not be empty")
	}
	wantRef := PermissionAuthorityRef("claude_code")
	found := false
	for _, ref := range obs.EvidenceRefs {
		if ref == wantRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence refs = %+v, want authority ref %q", obs.EvidenceRefs, wantRef)
	}
}

// TestBuildPermissionPostureObservationStableIdentity is F4's package-level
// half: re-deriving the observation from the identical drift state must
// reproduce the SAME observation id (so the intake dedupes to one card);
// mutating to a genuinely different drift state must mint a different id.
func TestBuildPermissionPostureObservationStableIdentity(t *testing.T) {
	homePath, targetPath, render := buildDriftFixture(t)
	settings := decodeTestJSON(t, render.Content)
	permissions := settings["permissions"].(map[string]any)
	permissions["allow"] = []string{"Read", "mcp__gbrain__*", "mcp__*"}
	writeTestJSON(t, targetPath, settings)

	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := BuildPermissionPostureObservation(report, render.Content, live)
	if !ok {
		t.Fatal("expected a drifted observation")
	}
	// Re-derive from the identical inputs (as a re-run of `compound harness
	// drift --emit-divergence` against the same live state would).
	reportAgain, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	liveAgain, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	second, ok := BuildPermissionPostureObservation(reportAgain, render.Content, liveAgain)
	if !ok {
		t.Fatal("expected a drifted observation on re-derivation")
	}
	if first.ObservationID != second.ObservationID {
		t.Fatalf("same drift state must reproduce the same observation id: %q vs %q", first.ObservationID, second.ObservationID)
	}

	// Now mutate to a genuinely different drift state (a different missing
	// managed rule) and confirm the id changes.
	permissions["allow"] = []string{"mcp__gbrain__*"}
	writeTestJSON(t, targetPath, settings)
	reportChanged, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	liveChanged, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	third, ok := BuildPermissionPostureObservation(reportChanged, render.Content, liveChanged)
	if !ok {
		t.Fatal("expected a drifted observation on the changed state")
	}
	if third.ObservationID == first.ObservationID {
		t.Fatalf("a different drift state must mint a different observation id, got the same: %q", third.ObservationID)
	}
}

// TestPermissionPostureStateIgnoresUnrelatedKeys mirrors
// TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys's own guarantee at the
// posture-digest level: an unrelated top-level key (theme, a hand-owned
// hook) must never change the computed state, only the managed permission
// surface may.
func TestPermissionPostureStateIgnoresUnrelatedKeys(t *testing.T) {
	a := permissionPostureState([]byte(`{"theme":"dark","permissions":{"allow":["Read"],"defaultMode":"default"},"autoMode":{"allow":["$defaults"]}}`), nil)
	b := permissionPostureState([]byte(`{"theme":"light","permissions":{"allow":["Read"],"defaultMode":"default"},"autoMode":{"allow":["$defaults"]}}`), nil)
	if a != b {
		t.Fatalf("unrelated key must not change posture state: %q vs %q", a, b)
	}
	c := permissionPostureState([]byte(`{"theme":"dark","permissions":{"allow":["Read","mcp__*"],"defaultMode":"default"},"autoMode":{"allow":["$defaults"]}}`), nil)
	if a == c {
		t.Fatal("a managed rule change must change posture state")
	}
}
