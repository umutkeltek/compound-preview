package contract

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComputeBindingFreshnessPinsStatusVocabulary uses real byte-backed fixtures.
func TestComputeBindingFreshnessPinsStatusVocabulary(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "compound")
	body := []byte("synthetic binary")
	if err := os.WriteFile(binaryPath, body, 0o700); err != nil {
		t.Fatal(err)
	}
	current := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	stale := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("previous")))
	for _, tc := range []struct{ name, revision, want string }{
		{"fresh", current, "fresh"}, {"drifted", stale, "drifted"}, {"unknown_empty_revision", "", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeBindingFreshnessFixture(t, home, []InstalledHook{{Harness: "claude", Name: "PreToolUse", Event: "PreToolUse", Ref: binaryPath, Revision: tc.revision, Status: "current"}})
			report, err := ComputeBindingFreshness(home, binaryPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Rows) != 1 || report.Rows[0].Status != tc.want {
				t.Fatalf("report=%+v; want=%s", report, tc.want)
			}
			if (report.Rows[0].RepairCommand != "") != (tc.want == "drifted") {
				t.Fatalf("repair=%+v", report.Rows[0])
			}
		})
	}
}

// TestComputeBindingFreshnessMissingManifest pins the absent path: an
// empty $COMPOUND_HOME/config directory produces an empty Rows slice
// without an error, so doctor renders "(no installed hooks manifest)".
func TestComputeBindingFreshnessMissingManifest(t *testing.T) {
	home := t.TempDir()
	binaryPath := filepath.Join(t.TempDir(), "compound-current")
	report, err := ComputeBindingFreshness(home, binaryPath)
	if err != nil {
		t.Fatalf("missing manifest should not error, got: %v", err)
	}
	if len(report.Rows) != 0 {
		t.Fatalf("missing manifest rows = %d, want 0", len(report.Rows))
	}
}

// TestComputeBindingFreshnessRejectsEmptyInputs makes the fail-closed
// contract explicit: an empty home or empty binary path returns an error
// so the doctor surfaces "binding freshness unavailable" instead of a
// silently-empty report.
func TestComputeBindingFreshnessRejectsEmptyInputs(t *testing.T) {
	if _, err := ComputeBindingFreshness("", "/some/binary"); err == nil {
		t.Fatalf("empty home path returned no error; want fail-closed")
	}
	if _, err := ComputeBindingFreshness("/some/home", ""); err == nil {
		t.Fatalf("empty binary path returned no error; want fail-closed")
	}
}

func writeBindingFreshnessFixture(t *testing.T, home string, hooks []InstalledHook) []byte {
	t.Helper()
	data, err := yaml.Marshal(InstalledHooksManifest{SchemaVersion: "compound.installed_hooks.v0.1", Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(InstalledHooksPath(home), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBindingFreshnessAttestsBinaryBytesAndIdentity(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "compound")
	body := []byte("synthetic installed binary")
	if err := os.WriteFile(binary, body, 0o700); err != nil {
		t.Fatal(err)
	}
	revision := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	other := filepath.Join(dir, "unrelated-plugin")
	if err := os.WriteFile(other, body, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "compound-link")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	stale := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("previous binary")))
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(userHome, binary)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, ref, recorded, binary, want, current string }{
		{"matching_bytes", binary, revision, binary, "fresh", revision},
		{"home_ref", "$HOME/" + relative, revision, binary, "fresh", revision},
		{"braced_home_ref", "${HOME}/" + relative, revision, binary, "fresh", revision},
		{"tilde_ref", "~/" + relative, revision, binary, "fresh", revision},
		{"nonhex_digest", binary, "sha256:" + strings.Repeat("z", 64), binary, "unknown", revision},
		{"changed_bytes", binary, stale, binary, "drifted", revision},
		{"legacy_same_locator", binary, "binary:" + binary, binary, "unknown", revision},
		{"legacy_other_locator", binary, "binary:/synthetic/previous", binary, "unknown", revision},
		{"empty_revision", binary, "", binary, "unknown", revision},
		{"malformed_hash", binary, "sha256:not-a-digest", binary, "unknown", revision},
		{"bare_command", "compound", revision, binary, "unknown", ""},
		{"opaque_handler", "handler:synthetic", revision, binary, "unknown", ""},
		{"opaque_hook", "hook://synthetic/handler", revision, binary, "unknown", ""},
		{"unrelated_same_bytes", other, revision, binary, "unknown", ""},
		{"missing_binary", filepath.Join(dir, "missing"), revision, filepath.Join(dir, "missing"), "unknown", ""},
		{"nonregular_binary", dir, revision, dir, "unknown", ""},
		{"symlink_unattested", link, revision, link, "unknown", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			before := writeBindingFreshnessFixture(t, home, []InstalledHook{{Harness: "claude", Name: "PreToolUse", Event: "PreToolUse", Ref: tc.ref, Revision: tc.recorded, Status: "current"}})
			report, err := ComputeBindingFreshness(home, tc.binary)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Rows) != 1 {
				t.Fatalf("rows=%+v", report.Rows)
			}
			row := report.Rows[0]
			if row.Status != tc.want || row.CurrentBinary != tc.current || row.RecordedAt != tc.recorded {
				t.Fatalf("row=%+v; want status=%s current=%q recorded=%q", row, tc.want, tc.current, tc.recorded)
			}
			if report.BinaryPath != tc.binary {
				t.Fatalf("binary path lost: %+v", report)
			}
			if (row.RepairCommand != "") != (tc.want == "drifted") {
				t.Fatalf("unexpected repair: %+v", row)
			}
			after, err := os.ReadFile(InstalledHooksPath(home))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("read-only report changed manifest: %v", err)
			}
		})
	}
}

func TestBindingFreshnessReducesAllEventsDeterministically(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "compound")
	body := []byte("synthetic current binary")
	if err := os.WriteFile(binary, body, 0o700); err != nil {
		t.Fatal(err)
	}
	current := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	old := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("synthetic previous binary")))
	older := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("synthetic older binary")))
	hooks := []InstalledHook{
		{Harness: "zeta", Name: "PreToolUse", Event: "PreToolUse", Ref: binary, Revision: current, Status: "current"},
		{Harness: "alpha", Name: "SessionStart", Event: "SessionStart", Ref: binary, Revision: old, Status: "current"},
		{Harness: "alpha", Name: "SessionEnd", Event: "SessionEnd", Ref: binary, Revision: older, Status: "current"},
		{Harness: "alpha", Name: "PostToolUse", Event: "PostToolUse", Ref: "handler:opaque", Revision: current, Status: "current"},
		{Harness: "alpha", Name: "PreToolUse", Event: "PreToolUse", Ref: binary, Revision: current, Status: "current"},
		{Harness: "beta", Name: "SessionStart", Event: "SessionStart", Ref: "handler:opaque", Revision: current, Status: "current"},
		{Harness: "beta", Name: "PreToolUse", Event: "PreToolUse", Ref: binary, Revision: current, Status: "current"},
	}
	var first BindingFreshnessReport
	for pass := 0; pass < 2; pass++ {
		home := t.TempDir()
		writeBindingFreshnessFixture(t, home, hooks)
		report, err := ComputeBindingFreshness(home, binary)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Rows) != 3 {
			t.Fatalf("rows=%+v", report.Rows)
		}
		want := []string{"alpha:drifted", "beta:unknown", "zeta:fresh"}
		var got []string
		for _, row := range report.Rows {
			got = append(got, row.Harness+":"+row.Status)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("all-event reduction=%v; want=%v", got, want)
		}
		if pass == 0 {
			first = report
		} else if !reflect.DeepEqual(first, report) {
			t.Fatalf("manifest ordering changed report: first=%+v second=%+v", first, report)
		}
		for i, j := 0, len(hooks)-1; i < j; i, j = i+1, j-1 {
			hooks[i], hooks[j] = hooks[j], hooks[i]
		}
	}
}
