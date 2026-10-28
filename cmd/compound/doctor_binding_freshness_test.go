package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorReportsDriftedBindingAfterStaleInstall proves that replacing binary
// bytes without refreshing recorded attestation is visible through the real
// doctor JSON/text consumer. A path locator is not a byte revision.
func TestDoctorReportsDriftedBindingAfterStaleInstall(t *testing.T) {
	t.Setenv("COMPOUND_PLACEMENT_GUARD_MODE", "")
	t.Setenv("PLACEMENT_GUARD_MODE", "")

	home := t.TempDir()
	base := t.TempDir()
	binary := filepath.Join(t.TempDir(), "compound-new")
	currentRevision := writeDoctorBindingBinary(t, binary)
	recordedRevision := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("previous synthetic binary")))

	// The recorded bytes precede the installed file; the path is unchanged.
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema_version: compound.installed_hooks.v0.1\n" +
		"hooks:\n" +
		" - harness: claude\n" +
		"   name: PreToolUse\n" +
		"   event: PreToolUse\n" +
		"   ref: " + binary + "\n" +
		"   revision: " + recordedRevision + "\n" +
		"   status: current\n"
	if err := os.WriteFile(filepath.Join(home, "config", "installed-hooks.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", base, "--binary", binary, "--json"}
	// Exit contract (landed with the doctor exit-truth change): 0 = no problems,
	// 1 = problems found, 2 = the doctor could not run. Host-coupled checks can
	// legitimately report problems on a dev box, so only exit 2 is a failure here;
	// the binding_freshness section below is the assertion that matters.
	if code := runDoctor(args, &out, &errOut); code != 0 && code != 1 {
		t.Fatalf("doctor could not run on a drifted binding: code=%d stderr=%q", code, errOut.String())
	}
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse doctor json: %v (%q)", err, out.String())
	}
	if len(rep.BindingFreshness.Rows) != 1 {
		t.Fatalf("binding_freshness rows = %d, want 1: %+v", len(rep.BindingFreshness.Rows), rep.BindingFreshness)
	}
	row := rep.BindingFreshness.Rows[0]
	if row.Harness != "claude" || row.Status != "drifted" {
		t.Fatalf("binding_freshness row = %+v, want harness=claude status=drifted", row)
	}
	if row.RecordedAt != recordedRevision {
		t.Fatalf("binding_freshness recorded = %q", row.RecordedAt)
	}
	if row.CurrentBinary != currentRevision {
		t.Fatalf("binding_freshness current = %q, want %s", row.CurrentBinary, currentRevision)
	}
	if row.RepairCommand != "compound hook install --harness claude" {
		t.Fatalf("binding_freshness repair = %q", row.RepairCommand)
	}

	// Text rendering must include the drift line and the repair line.
	// The doctor-exit-truth branch fixes the problem/warning wiring;
	// this section only adds one block, so re-render in text mode and
	// pin its shape.
	out.Reset()
	errOut.Reset()
	if code := runDoctor([]string{"--home", home, "--base", base, "--binary", binary}, &out, &errOut); code != 0 && code != 1 { // exit contract: only 2 = could not run
		t.Fatalf("doctor text mode could not run: %s", errOut.String())
	}
	body := out.String()
	if !strings.Contains(body, "binding freshness (hook manifest vs installed binary)") {
		t.Fatalf("doctor text omitted binding freshness section:\n%s", body)
	}
	if !strings.Contains(body, "claude") || !strings.Contains(body, "drifted") {
		t.Fatalf("doctor text omitted drifted row:\n%s", body)
	}
	if !strings.Contains(body, "compound hook install --harness claude") {
		t.Fatalf("doctor text omitted repair command:\n%s", body)
	}
}

// TestDoctorReportsFreshBindingWhenRecordedRevisionMatchesCurrentBinary proves
// that actual byte-backed bindings do not generate false repair advice.
func TestDoctorReportsFreshBindingWhenRecordedRevisionMatchesCurrentBinary(t *testing.T) {
	t.Setenv("COMPOUND_PLACEMENT_GUARD_MODE", "")
	t.Setenv("PLACEMENT_GUARD_MODE", "")

	home := t.TempDir()
	base := t.TempDir()
	binary := filepath.Join(t.TempDir(), "compound-current")
	currentRevision := writeDoctorBindingBinary(t, binary)
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema_version: compound.installed_hooks.v0.1\n" +
		"hooks:\n" +
		" - harness: claude\n" +
		"   name: PreToolUse\n" +
		"   event: PreToolUse\n" +
		"   ref: " + binary + "\n" +
		"   revision: " + currentRevision + "\n" +
		"   status: current\n"
	if err := os.WriteFile(filepath.Join(home, "config", "installed-hooks.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", base, "--binary", binary, "--json"}
	if code := runDoctor(args, &out, &errOut); code != 0 && code != 1 { // see the exit-contract note above
		t.Fatalf("doctor could not run on a fresh binding: code=%d stderr=%s", code, errOut.String())
	}
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse doctor json: %v", err)
	}
	if len(rep.BindingFreshness.Rows) != 1 {
		t.Fatalf("binding_freshness rows = %d", len(rep.BindingFreshness.Rows))
	}
	if rep.BindingFreshness.Rows[0].Status != "fresh" {
		t.Fatalf("binding_freshness row = %+v, want status=fresh", rep.BindingFreshness.Rows[0])
	}
	if rep.BindingFreshness.Rows[0].RepairCommand != "" {
		t.Fatalf("fresh binding reported a repair command: %q", rep.BindingFreshness.Rows[0].RepairCommand)
	}
}

func writeDoctorBindingBinary(t *testing.T, path string) string {
	t.Helper()
	body := []byte("synthetic installed binary")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}
