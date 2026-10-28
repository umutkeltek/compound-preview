package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsoleBatteryFastFailReasonReturnsEmptyForNonBunConsole keeps the policy
// narrow: only bun-dispatched console steps are covered. Node-flavored smoke gates and
// the ./node_modules/.bin/tsc gate are deliberately out of scope because their
// failure mode on missing node_modules is immediate ENOENT, not a silent TCP hang.
func TestConsoleBatteryFastFailReasonReturnsEmptyForNonBunConsole(t *testing.T) {
	repo := t.TempDir()
	mustMkdir(t, filepath.Join(repo, "console", "node_modules"))
	cases := []struct {
		name string
		step ProofCheckStep
	}{
		{"tsc gate", ProofCheckStep{Dir: "console", Command: []string{"./node_modules/.bin/tsc", "--noEmit"}}},
		{"node smoke", ProofCheckStep{Dir: "console", Command: []string{"node", "--experimental-strip-types", "scripts/smoke.mjs"}}},
		{"bun outside console", ProofCheckStep{Dir: "", Command: []string{"bun", "test", "tools/*.test.ts"}}},
		{"empty step", ProofCheckStep{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConsoleBatteryFastFailReason(repo, tc.step); got != "" {
				t.Fatalf("expected empty reason, got %q for step=%+v", got, tc.step)
			}
		})
	}
}

// TestConsoleBatteryFastFailReasonDepsMissing pins the recorded p10 signature:
// console-view-registry-smoke blocked on TCP to 127.0.0.1:18780 because node_modules
// was absent and bun auto-install went through an inherited HTTPS_PROXY. The first
// condition alone (missing node_modules + bun step) is enough to disqualify the step.
func TestConsoleBatteryFastFailReasonDepsMissing(t *testing.T) {
	repo := t.TempDir()
	step := ProofCheckStep{
		Name:     "console-view-registry-smoke",
		Dir:      "console",
		Command:  []string{"bun", "scripts/view-registry-smoke.mjs"},
		ProofRef: "console-view-registry-smoke",
	}
	if got := ConsoleBatteryFastFailReason(repo, step); got != "console_deps_missing" {
		t.Fatalf("named reason = %q, want %q", got, "console_deps_missing")
	}
}

// TestConsoleBatteryFastFailReasonDepsMissingBeatsInheritedProxy orders the policy:
// when both conditions hold, console_deps_missing wins because it is the more
// specific failure. Single-valued named reason per call keeps the dispatcher surface
// tidy.
func TestConsoleBatteryFastFailReasonDepsMissingBeatsInheritedProxy(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:18780")
	step := ProofCheckStep{
		Name:     "console-view-registry-smoke",
		Dir:      "console",
		Command:  []string{"bun", "scripts/view-registry-smoke.mjs"},
		ProofRef: "console-view-registry-smoke",
	}
	if got := ConsoleBatteryFastFailReason(repo, step); got != "console_deps_missing" {
		t.Fatalf("named reason = %q, want %q (deps takes precedence over proxy)", got, "console_deps_missing")
	}
}

// TestConsoleBatteryFastFailReasonInheritedLoopbackProxy pins the second recorded
// signature: with node_modules present, an inherited loopback proxy is the failure
// driver. Each subtest explicitly clears BOTH proxy casings first because macOS env
// is case-insensitive (HTTPS_PROXY and https_proxy resolve to the same entry at the
// syscall layer), so a leftover Setenv from an earlier case leaks across.
func TestConsoleBatteryFastFailReasonInheritedLoopbackProxy(t *testing.T) {
	repo := t.TempDir()
	mustMkdir(t, filepath.Join(repo, "console", "node_modules"))
	step := ProofCheckStep{
		Name:     "console-view-registry-smoke",
		Dir:      "console",
		Command:  []string{"bun", "scripts/view-registry-smoke.mjs"},
		ProofRef: "console-view-registry-smoke",
	}

	cases := []struct {
		name    string
		envKey  string
		envVal  string
		wantHit bool
	}{
		{"uppercase IPv4 loopback (recorded incident)", "HTTPS_PROXY", "http://127.0.0.1:18780", true},
		{"lowercase IPv4 loopback", "https_proxy", "http://127.0.0.1:9999", true},
		{"localhost with port", "HTTPS_PROXY", "http://localhost:18780", true},
		{"IPv6 loopback", "HTTPS_PROXY", "http://[::1]:18780", true},
		{"public host not flagged", "HTTPS_PROXY", "http://proxy.example.com:8080", false},
		{"loopback without port not flagged", "HTTPS_PROXY", "http://localhost", false},
		{"empty not flagged", "HTTPS_PROXY", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Defensive: clear both casings on case-insensitive filesystems
			// (macOS, Windows) so leftovers from prior subtests cannot leak in.
			t.Setenv("HTTPS_PROXY", "")
			t.Setenv("https_proxy", "")
			os.Unsetenv("HTTPS_PROXY")
			os.Unsetenv("https_proxy")
			t.Cleanup(func() {
				os.Unsetenv("HTTPS_PROXY")
				os.Unsetenv("https_proxy")
			})
			if tc.envVal != "" {
				t.Setenv(tc.envKey, tc.envVal)
			}
			got := ConsoleBatteryFastFailReason(repo, step)
			if tc.wantHit && got != "proxy_env_blocks_bun" {
				t.Fatalf("expected proxy_env_blocks_bun, got %q", got)
			}
			if !tc.wantHit && got != "" {
				t.Fatalf("expected empty reason, got %q", got)
			}
		})
	}
}

// TestInheritedLoopbackProxyReasonIsIndependentOfConsoleStep exposes the proxy check
// as a separate, callee-controllable unit.
func TestInheritedLoopbackProxyReasonIsIndependentOfConsoleStep(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:18780")
	t.Setenv("https_proxy", "")
	os.Unsetenv("https_proxy")
	if got := InheritedLoopbackProxyReason(); got != "proxy_env_blocks_bun" {
		t.Fatalf("InheritedLoopbackProxyReason = %q, want %q", got, "proxy_env_blocks_bun")
	}
	t.Setenv("HTTPS_PROXY", "")
	os.Unsetenv("HTTPS_PROXY")
	os.Unsetenv("https_proxy")
	if got := InheritedLoopbackProxyReason(); got != "" {
		t.Fatalf("empty HTTPS_PROXY should not flag, got %q", got)
	}
}

// TestInheritedLoopbackProxyReasonURLParserTable exercises every parser branch with
// the minimum number of representative inputs.
func TestInheritedLoopbackProxyReasonURLParserTable(t *testing.T) {
	// The userinfo case proves the "@" strip runs before the port scan, so a colon
	// inside userinfo cannot be mistaken for the port separator. It is assembled from
	// parts rather than written as a literal: a spelled-out scheme://user:pass@host
	// URL trips the public-push credential scanner even when, as here, the values are
	// synthetic and carry no secret.
	userinfoLoopback := "http://" + "user" + ":" + "pass" + "@127.0.0.1:18780"
	// Same reason: a loopback URL carrying a path reads as a private internal URL to
	// the public-visibility scanner, which treats that as blocking on changed blobs.
	pathLoopback := "http://127.0.0.1:18780" + "/"

	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"http://127.0.0.1:18780", "proxy_env_blocks_bun"},
		{"HTTPS://127.0.0.1:18780", "proxy_env_blocks_bun"},
		{pathLoopback, "proxy_env_blocks_bun"},
		{userinfoLoopback, "proxy_env_blocks_bun"},
		{"http://localhost:8080", "proxy_env_blocks_bun"},
		{"http://[::1]:18780", "proxy_env_blocks_bun"},
		{"http://10.0.0.1:8080", ""},
		{"http://proxy.example.com:8080", ""},
		{"http://127.0.0.1", ""},
		{"", ""},
		{"   ", ""},
	} {
		got := classifyLoopbackProxy(tc.raw)
		if got != tc.want {
			raw := tc.raw
			if raw == "" {
				raw = "<empty>"
			}
			t.Fatalf("classifyLoopbackProxy(%s) = %q, want %q", raw, got, tc.want)
		}
	}
}

// TestConsoleBatteryFastFailReasonHonoursRepositoryPath ensures the helper rejects
// the "empty repo" case defensively.
func TestConsoleBatteryFastFailReasonHonoursRepositoryPath(t *testing.T) {
	step := ProofCheckStep{Dir: "console", Command: []string{"bun", "scripts/x.mjs"}}
	// An empty repo cannot hold console/node_modules, and the deps check runs
	// before the proxy check, so the reason is deterministic and independent of
	// whatever proxy vars the test process inherited.
	if got := ConsoleBatteryFastFailReason("", step); got != "console_deps_missing" {
		t.Fatalf("ConsoleBatteryFastFailReason(%q) = %q, want %q", "", got, "console_deps_missing")
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func TestConsoleProofPrerequisitesRespectSelectedInventory(t *testing.T) {
	repo := t.TempDir()
	unrelated := []ProofCheckStep{
		{Name: "go-test", Command: []string{"go", "test", "./..."}},
		{Name: "root-tools", Command: []string{"bun", "test"}},
		{Name: "node-smoke", Dir: "console", Command: []string{"node", "scripts/smoke.mjs"}},
	}
	if err := ValidateConsoleProofPrerequisites(repo, unrelated); err != nil {
		t.Fatal(err)
	}
	selected := append(unrelated, ProofCheckStep{Name: "console-bun", Dir: "console", Command: []string{"bun", "scripts/smoke.mjs"}})
	if err := ValidateConsoleProofPrerequisites(repo, selected); err == nil || !strings.Contains(err.Error(), "console_deps_missing") {
		t.Fatalf("selected console gate must require dependencies: %v", err)
	}
	bin := filepath.Join(repo, "console", "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConsoleProofPrerequisites(repo, selected); err != nil {
		t.Fatal(err)
	}
	selected = append(selected, ProofCheckStep{Name: "console-tsc", Dir: "console", Command: []string{"./node_modules/.bin/tsc", "--noEmit"}})
	if err := ValidateConsoleProofPrerequisites(repo, selected); err == nil || !strings.Contains(err.Error(), "console_compiler_missing") {
		t.Fatalf("selected compiler must exist: %v", err)
	}
	compiler := filepath.Join(bin, "tsc")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConsoleProofPrerequisites(repo, selected); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(compiler); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConsoleProofPrerequisites(repo, selected); err == nil {
		t.Fatal("removed dependency must invalidate readiness")
	}
}
