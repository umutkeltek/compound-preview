package harness

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// versionProbeTimeout caps every ObservedVersion probe. It is intentionally
// short: the doctor's harness_version_drift signal is a NAMED diagnostic, not a
// gate on the operator's run; a slow probe costs operator time without buying
// signal. 3s is the budget the operator-facing surface allocates.
const versionProbeTimeout = 3 * time.Second

// binaryForHarness maps the harness registry id to the binary whose --version
// output recovers the installed version. Returning empty is the documented
// unobserved path — the caller (compile.go / doctor) MUST fall back to the
// "registry:<name>" empty socket rather than fabricate a version. Keep this
// table small: every entry is a contract the doctor renders to the operator.
//
// deepseek_harness is deliberately ABSENT even though it carries an
// ExpectedVersion pin. Its version is "0.1.0-rc.7", and parseSemverToken
// recovers only "0.1.0" from it — the pre-release suffix is not part of the
// token grammar. Mapping the binary would therefore report drift on every
// probe of a machine running exactly the pinned build: a permanent false
// positive, which is how a doctor signal turns into background noise. Add the
// row when the harness ships a plain X.Y.Z release, not before.
var binaryForHarness = map[string]string{
	"prime_agent": "prime-agent",
	"oh_my_pi":    "omp",
	"qwen_code":   "qwen",
	"claude_code": "claude",
	"claude_zai":  "claude",
	"codex":       "codex",
}

// ObservedVersion shells `<binary> --version` with a 3s timeout and parses the
// first semver-looking token from the output. It is the PURE decision at the
// center of harness_version_drift: the doctor's named signal exists to surface
// drift between this observed version and the operator-pinned ExpectedVersion.
//
// Returns ("", nil) when the binary is missing or its --version output is
// unparseable — not an error. The caller is responsible for distinguishing
// "unobservable" from "error" if it needs to; this function does not, because
// the doctor signal renders unobservable as a separate status and the
// distinction lives there.
//
// Pure function: no global mutable state, no env reads, no test fixtures that
// pollute later runs. The runner hooks (versionProbeFn) are exported so tests
// can substitute a fake without invoking a real binary.
func ObservedVersion(name string) string {
	return ObservedVersionWithRunner(name, defaultVersionProbe)
}

// ObservedVersionWithRunner is the seam tests substitute. Production callers
// should use ObservedVersion; this exists so the timeout and binary lookup
// stay together with the test seam.
func ObservedVersionWithRunner(name string, runner func(binary string) (string, error)) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	binary, ok := binaryForHarness[name]
	if !ok || strings.TrimSpace(binary) == "" {
		return ""
	}
	if runner == nil {
		return ""
	}
	out, err := runner(binary)
	if err != nil {
		return ""
	}
	return parseSemverToken(out)
}

// defaultVersionProbe is the production wiring: a 3s-timeout exec of
// `<binary> --version`. Kept short — the doctor signal is informational, not a
// gate, and a slow probe costs operator time without buying signal.
func defaultVersionProbe(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	// CombinedOutput, not Output: prime-agent prints its version to STDERR
	// (claude/codex use stdout). Output() would drop it and read as unobserved.
	// The parser is lenient, so merging streams is safe for the stdout emitters.
	out, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	return string(out), err
}

// parseSemverToken pulls the first X.Y.Z (where X, Y, Z are digits) from a
// version string. It is intentionally lenient: prime-agent emits "prime-agent
// v0.7.4", claude emits "claude 1.0.x (build ...)", codex emits "codex-cli
// 0.x.y (commit)" — all must parse. Returns "" when no token matches.
//
// The function does NOT do semver comparison; it just recovers a comparable
// string. Doctor's drift verdict uses the binary relation expected !=
// observed, not a full semver compare, so the parser only needs to extract a
// stable token.
func parseSemverToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Walk rune by rune looking for the first digit. From the first digit,
	// greedily consume digits and dots (where a dot must be followed by
	// another digit so "0.7." does not match). Require at least one dot in
	// the run so a bare integer ("1", "42") doesn't render as a version.
	runes := []rune(raw)
	for i, r := range runes {
		if r < '0' || r > '9' {
			continue
		}
		end := i
		dots := 0
		for end < len(runes) {
			c := runes[end]
			if c >= '0' && c <= '9' {
				end++
				continue
			}
			if c == '.' && end+1 < len(runes) && runes[end+1] >= '0' && runes[end+1] <= '9' {
				dots++
				end++
				continue
			}
			break
		}
		if dots >= 1 && end > i {
			return strings.TrimSpace(string(runes[i:end]))
		}
		// Not a semver token at this position; keep scanning past the
		// failed candidate rather than aborting.
	}
	return ""
}
