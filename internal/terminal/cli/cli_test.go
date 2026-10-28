//go:build darwin

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunTerminal_NoArgs_Usage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal(nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr missing usage header; got %q", stderr.String())
	}
}

func TestRunTerminal_HelpFlag_Usage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"--help"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "known terminals") {
		t.Fatalf("stderr missing terminal list; got %q", stderr.String())
	}
}

func TestRunTerminal_List(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"cmux"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d; want 0", code)
	}
	got := strings.TrimSpace(stdout.String())
	want := "cmux\niterm2\nterminal-app\nwarp"
	if got != want {
		t.Fatalf("stdout = %q; want %q", got, want)
	}
}

func TestRunTerminal_ListJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"--json"}, &stdout, &stderr)
	// `compound terminal` with no name but --json should also list.
	if code != 0 {
		t.Fatalf("code = %d; want 0", code)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}
	names, ok := out["terminals"].([]any)
	if !ok {
		t.Fatalf("terminals key missing or wrong type: %v", out)
	}
	if len(names) != 4 {
		t.Fatalf("expected 4 terminals, got %d: %v", len(names), names)
	}
}

func TestRunTerminal_UnknownTerminal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"bogus", "doctor"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown terminal") {
		t.Fatalf("stderr missing 'unknown terminal'; got %q", stderr.String())
	}
}

func TestRunTerminal_UnknownVerb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"cmux", "frobnicate"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown verb") {
		t.Fatalf("stderr missing 'unknown verb'; got %q", stderr.String())
	}
}

func TestRunTerminal_Doctor_KnownFlagEnforced(t *testing.T) {
	// cmux doctor with --json should work.
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"cmux", "doctor", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d (stderr=%q); want 0", code, stderr.String())
	}
	// Must produce valid JSON.
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("JSON parse: %v (stdout=%q)", err, stdout.String())
	}
	if _, ok := out["installed"]; !ok {
		t.Fatalf("Doctor JSON missing 'installed'; got %v", out)
	}
}

func TestRunTerminal_Doctor_UnknownFlagRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"cmux", "doctor", "--duraton=5s"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d; want 2 for typo flag", code)
	}
	if !strings.Contains(stderr.String(), "unknown flag") {
		t.Fatalf("stderr missing 'unknown flag'; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--duraton") {
		t.Fatalf("stderr should name the offending flag --duraton; got %q", stderr.String())
	}
}

func TestRunTerminal_DoctorHumanReadable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"iterm2", "doctor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d; want 0", code)
	}
	got := stdout.String()
	if !strings.Contains(got, "iterm2 installed=") {
		t.Fatalf("expected 'iterm2 installed=' line; got %q", got)
	}
	if !strings.Contains(got, "ipc=") {
		t.Fatalf("expected 'ipc=' segment; got %q", got)
	}
}

func TestRunTerminal_Wire_JSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"cmux", "wire", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d; want 0", code)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}
	if out["terminal"] != "cmux" {
		t.Fatalf("terminal = %v; want cmux", out["terminal"])
	}
	if _, ok := out["install"]; !ok {
		t.Fatal("install key missing")
	}
}

func TestRunTerminal_FeedPush_MissingBody(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"cmux", "feed", "push"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d; want 2 (missing --body)", code)
	}
	if !strings.Contains(stderr.String(), "--body") {
		t.Fatalf("stderr should mention --body; got %q", stderr.String())
	}
}

func TestRunTerminal_FeedPush_UnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"cmux", "feed", "push", "--body", "hello", "--bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown flag") {
		t.Fatalf("stderr missing 'unknown flag'; got %q", stderr.String())
	}
}

func TestRunTerminal_WorkspaceSelect_MissingRef(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runTerminal([]string{"cmux", "workspace", "select"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "select requires <ref>") {
		t.Fatalf("stderr missing ref hint; got %q", stderr.String())
	}
}
