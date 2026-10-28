package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/harness"
)

// writeDoctorPermissionPostureFixture writes one harness-permission authority
// plus a live target settings file into homePath, returning the target path.
// It reuses the same authority shape as writeHarnessDriftObserveFixture
// (harness_drift_observe_test.go) since both exercise the identical
// harness.PermissionDrift probe; this file only adds the doctor-surface
// assertions (packet work_0fc6126d8fa2 falsifier F5).
func writeDoctorPermissionPostureFixture(t *testing.T, homePath string, allow ...string) string {
	t.Helper()
	if len(allow) == 0 {
		allow = []string{"Read", "mcp__gbrain__*"}
	}
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	writeHarnessDriftObserveJSON(t, targetPath, map[string]any{"theme": "dark"})
	policy := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow: allow, Deny: []string{"Bash(rm:*)"}, Ask: []string{"WebFetch"}, DefaultMode: "default",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
	writeHarnessDriftObserveJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), policy)
	return targetPath
}

// renderAndPublishPermissionFixture renders the authority's intended
// settings, writes them as the live target file, and publishes the
// harness-artifact manifest -- the same "clean baseline" step
// TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys performs -- returning
// the rendered content so a caller can mutate it into a drifted state.
func renderAndPublishPermissionFixture(t *testing.T, homePath, harnessName, targetPath string) []byte {
	t.Helper()
	render, err := harness.RenderPermissions(homePath, harnessName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := harness.WriteHarnessArtifactManifest(homePath, []harness.HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}
	return render.Content
}

// TestDoctorPermissionPosture_F5_RowPresentAndReadOnly is the F5 falsifier:
// `compound doctor` must surface a permission-posture row per configured
// harness (drifted status + config path named), and must write NOTHING
// under the test $COMPOUND_HOME -- the "writes nothing" contract holds even
// with this new section.
func TestDoctorPermissionPosture_F5_RowPresentAndReadOnly(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	targetPath := writeDoctorPermissionPostureFixture(t, home)

	// Render a clean baseline, then drift the live file (an unmanaged
	// mcp__* wildcard, mirroring TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys).
	render := renderAndPublishPermissionFixture(t, home, "claude_code", targetPath)
	var settings map[string]any
	if err := json.Unmarshal(render, &settings); err != nil {
		t.Fatal(err)
	}
	permissions := settings["permissions"].(map[string]any)
	permissions["allow"] = []string{"Read", "mcp__gbrain__*", "mcp__*"}
	writeHarnessDriftObserveJSON(t, targetPath, settings)

	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", t.TempDir(), "--binary", "/usr/local/bin/compound", "--json"}
	// A first doctor call has its own pre-existing, unrelated side effect
	// (landlease.Status creates lock/land-lease.lock on first probe -- true
	// even on a bare fresh home with zero permission authorities configured;
	// out of scope for this packet, noted as a follow-up). Let that settle,
	// THEN snapshot: the permission-posture addition itself must write
	// nothing on top of it.
	_ = runDoctor(args, &out, &errOut)
	// Warm-up call: exit code ignored (see contract note below).
	filesBefore := listFiles(t, home)

	out.Reset()
	errOut.Reset()
	code := runDoctor(args, &out, &errOut)

	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse doctor json: %v (%q)", err, out.String())
	}
	// Doctor exit-code contract (work_3da5d3c76707): exit 1 is allowed
	// only when the report names problems (harness version drift on the
	// dev machine legitimately counts). The structural assertions below
	// remain the test.
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("doctor exited %d without naming problems, stderr=%q", code, errOut.String())
	}
	if len(rep.PermissionPosture) != 1 {
		t.Fatalf("expected exactly one permission_posture row, got %+v", rep.PermissionPosture)
	}
	row := rep.PermissionPosture[0]
	if row.Harness != "claude_code" {
		t.Fatalf("row harness = %q, want claude_code", row.Harness)
	}
	if row.Status != "drift" {
		t.Fatalf("row status = %q, want drift: %+v", row.Status, row)
	}
	if row.TargetPath != targetPath {
		t.Fatalf("row target_path = %q, want %q", row.TargetPath, targetPath)
	}
	if len(row.Details) == 0 {
		t.Fatalf("drifted row must carry details, got none: %+v", row)
	}

	if filesAfter := listFiles(t, home); !stringSlicesEqual(filesBefore, filesAfter) {
		t.Fatalf("doctor must write nothing: before=%v after=%v", filesBefore, filesAfter)
	}

	// Text mode must also surface the row (operator-legible path, not JSON-only).
	out.Reset()
	errOut.Reset()
	textArgs := []string{"--home", home, "--base", t.TempDir(), "--binary", "/usr/local/bin/compound"}
	_ = runDoctor(textArgs, &out, &errOut)
	// Exit code ignored: text mode has no problems[] to cross-check (see
	// contract note above).
	if !bytes.Contains(out.Bytes(), []byte("permission posture")) {
		t.Fatalf("text output missing permission posture section:\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("claude_code")) || !bytes.Contains(out.Bytes(), []byte("drift")) {
		t.Fatalf("text output missing drifted claude_code row:\n%s", out.String())
	}
}

// TestDoctorPermissionPosture_CleanFixtureReportsClean is the companion
// negative case: a clean fixture reports status "clean", not "drift".
func TestDoctorPermissionPosture_CleanFixtureReportsClean(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	targetPath := writeDoctorPermissionPostureFixture(t, home)
	renderAndPublishPermissionFixture(t, home, "claude_code", targetPath)

	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", t.TempDir(), "--binary", "/usr/local/bin/compound", "--json"}
	code := runDoctor(args, &out, &errOut)
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse doctor json: %v (%q)", err, out.String())
	}
	// Doctor exit-code contract (work_3da5d3c76707): exit 1 is allowed
	// only when the report names problems (harness version drift on the
	// dev machine legitimately counts).
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("doctor exited %d without naming problems, stderr=%q", code, errOut.String())
	}
	if len(rep.PermissionPosture) != 1 || rep.PermissionPosture[0].Status != "clean" {
		t.Fatalf("expected one clean row, got %+v", rep.PermissionPosture)
	}
}

// TestDoctorPermissionPosture_NoAuthoritiesConfiguredIsEmpty confirms a
// fresh home with no harness-permission authorities yields an empty (not
// crashing, not fabricated) permission_posture list.
func TestDoctorPermissionPosture_NoAuthoritiesConfiguredIsEmpty(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", t.TempDir(), "--binary", "/usr/local/bin/compound", "--json"}
	code := runDoctor(args, &out, &errOut)
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse doctor json: %v (%q)", err, out.String())
	}
	// Doctor exit-code contract (work_3da5d3c76707): exit 1 is allowed
	// only when the report names problems (harness version drift on the
	// dev machine legitimately counts).
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("doctor exited %d without naming problems, stderr=%q", code, errOut.String())
	}
	if len(rep.PermissionPosture) != 0 {
		t.Fatalf("expected no permission_posture rows on a fresh home, got %+v", rep.PermissionPosture)
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
