package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseSemverTokenTable is the parser table test for ObservedVersion's
// extraction step. Pin the cases the operator-visible surface depends on:
//
//   - prime-agent's "prime-agent v0.7.4" format
//   - claude's "claude 1.0.x (build ...)" format (x in parens, not a digit)
//   - codex's "codex-cli 0.x.y (commit)" format
//   - bare integers like "1" or "42" do NOT count (no dot)
//   - empty / whitespace-only inputs return ""
//   - a non-semver preface like "error: not found" returns ""
//
// Adding a case here is cheap; losing one is a regression the operator reads
// as "doctor says unobserved when actually 0.7.4 was sitting in stdout".
func TestParseSemverTokenTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "prime-agent v-prefixed", in: "prime-agent v0.7.4", want: "0.7.4"},
		{name: "claude build suffix", in: "claude 1.0.x (build abcdef)", want: "1.0"},
		{name: "codex commit suffix", in: "codex-cli 0.42.1 (commit a1b2c3d)", want: "0.42.1"},
		{name: "plain semver", in: "1.2.3", want: "1.2.3"},
		{name: "trailing whitespace", in: "  0.7.4  \n", want: "0.7.4"},
		{name: "two digit minor", in: "v0.7.10", want: "0.7.10"},
		{name: "bare integer rejected", in: "version 42", want: ""},
		{name: "single digit rejected", in: "v1", want: ""},
		{name: "empty input", in: "", want: ""},
		{name: "whitespace only", in: "   \n  ", want: ""},
		{name: "non-semver preface", in: "error: not found", want: ""},
		{name: "trailing dot extracts the version prefix", in: "0.7.", want: "0.7"},
		{name: "pre-release suffix preserved is intentionally not preserved", in: "1.2.3-rc1", want: "1.2.3"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := parseSemverToken(c.in)
			if got != c.want {
				t.Fatalf("parseSemverToken(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestObservedVersionWithRunnerFake pins the runner seam: the function MUST
// fall back to "" when the runner errors, MUST return the parsed token when
// the runner returns a parseable string, and MUST NOT call the runner for
// harness names that have no binary in binaryForHarness. The fallback paths
// are what keep the doctor signal honest on machines without prime-agent.
func TestObservedVersionWithRunnerFake(t *testing.T) {
	t.Run("happy path — runner returns parseable version", func(t *testing.T) {
		got := ObservedVersionWithRunner("prime_agent", func(binary string) (string, error) {
			if binary != "prime-agent" {
				t.Fatalf("runner called with %q, want prime-agent", binary)
			}
			return "prime-agent v0.7.4", nil
		})
		if got != "0.7.4" {
			t.Fatalf("ObservedVersionWithRunner = %q, want %q", got, "0.7.4")
		}
	})

	t.Run("runner error falls back to empty", func(t *testing.T) {
		got := ObservedVersionWithRunner("prime_agent", func(string) (string, error) {
			return "", errFake
		})
		if got != "" {
			t.Fatalf("ObservedVersionWithRunner = %q, want empty string on runner error", got)
		}
	})

	t.Run("runner returns unparseable output falls back to empty", func(t *testing.T) {
		got := ObservedVersionWithRunner("prime_agent", func(string) (string, error) {
			return "this is not a version", nil
		})
		if got != "" {
			t.Fatalf("ObservedVersionWithRunner = %q, want empty when output unparseable", got)
		}
	})

	t.Run("unknown harness is short-circuited — runner never called", func(t *testing.T) {
		called := false
		got := ObservedVersionWithRunner("not_a_real_harness", func(string) (string, error) {
			called = true
			return "0.0.0", nil
		})
		if got != "" {
			t.Fatalf("ObservedVersionWithRunner = %q, want empty for unknown harness", got)
		}
		if called {
			t.Fatal("runner was called for unknown harness — short-circuit missing")
		}
	})

	t.Run("nil runner returns empty", func(t *testing.T) {
		got := ObservedVersionWithRunner("prime_agent", nil)
		if got != "" {
			t.Fatalf("ObservedVersionWithRunner(nil runner) = %q, want empty", got)
		}
	})

	t.Run("empty harness name returns empty without invoking runner", func(t *testing.T) {
		called := false
		got := ObservedVersionWithRunner("", func(string) (string, error) {
			called = true
			return "0.0.0", nil
		})
		if got != "" {
			t.Fatalf("ObservedVersionWithRunner(\"\") = %q, want empty", got)
		}
		if called {
			t.Fatal("runner was called for empty harness name")
		}
	})
}

// errFake is the test-only error used in runner-failure cases.
var errFake = stringError("runner exploded")

// stringError lets us construct an error literal without a separate import.
type stringError string

func (e stringError) Error() string { return string(e) }

// TestObservedVersionDefaultsPrimeAgentToPrimeAgentBinary pins the binary
// lookup table: prime_agent MUST shell prime-agent, claude_code MUST shell
// claude, codex MUST shell codex. A regression here (e.g. an accidental
// rename of the binary the doctor probes) is a silent no-op — the signal
// would render "unobserved" instead of "drift", and the operator would
// trust it. The test is the seam that catches it.
func TestObservedVersionDefaultsPrimeAgentToPrimeAgentBinary(t *testing.T) {
	for harnessName, wantBinary := range map[string]string{
		"prime_agent": "prime-agent",
		"claude_code": "claude",
		"claude_zai":  "claude",
		"codex":       "codex",
	} {
		got, ok := binaryForHarness[harnessName]
		if !ok {
			t.Fatalf("binaryForHarness missing entry for %q", harnessName)
		}
		if got != wantBinary {
			t.Fatalf("binaryForHarness[%q] = %q, want %q", harnessName, got, wantBinary)
		}
	}
}

// TestDefaultVersionProbeReadsStderr pins the real prime-agent behaviour that
// the doctor drift signal depends on: prime-agent emits its version on STDERR,
// not stdout. Before the CombinedOutput fix this returned "" (rendered
// "unobserved" in doctor even though prime-agent --version prints 0.7.x fine).
func TestDefaultVersionProbeReadsStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stderr-versioner")
	// #nosec G306 -- test fixture, must be executable
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 9.9.9 >&2\n"), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out, err := defaultVersionProbe(script)
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if got := parseSemverToken(out); got != "9.9.9" {
		t.Fatalf("stderr version not observed: parsed %q from %q", got, out)
	}
}
