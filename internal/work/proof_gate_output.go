package work

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ProofCheckSchemaVersion is the canonical schema version stamped on every freshly produced
// proof-check report. The accept-list in IsAcceptedProofCheckSchemaVersion keeps every
// previously shipped version readable so an in-flight batch can survive a code upgrade.
const ProofCheckSchemaVersion = "compound.proof_check_report.v0.6"

// IsAcceptedProofCheckSchemaVersion reports whether v is one of the schema versions the
// admission path can decode — the five legacy versions plus the canonical one supplied by
// the caller. Centralising the list keeps the source-bound proof version checks
// from drifting away from the equivalent accept-list in decodeOwnedProofCheckReport.
func IsAcceptedProofCheckSchemaVersion(v, canonical string) bool {
	switch v {
	case RemoteProofReportSchemaVersion, "compound.proof_check_report.v0.1",
		"compound.proof_check_report.v0.2",
		"compound.proof_check_report.v0.3",
		"compound.proof_check_report.v0.4",
		"compound.proof_check_report.v0.5", canonical:
		return true
	}
	return false
}

// ProofGateOutputTail bounds the displayed gate output while preserving full persisted output.
func ProofGateOutputTail(output []byte, max int) string {
	out := strings.TrimSpace(string(output))
	if len(out) <= max {
		return out
	}
	return out[len(out)-max:]
}

// ProofGateFailureLineCap bounds the per-gate failure_lines array. A red go-test run over
// ~150 packages can emit dozens of FAIL lines per package; 200 keeps the report JSON bounded
// while still covering every named failing test in any realistic battery run.
const ProofGateFailureLineCap = 200

// proofGateFailureLinePatterns is the canonical set of markers extracted into FailureLines.
// Each entry is a Go regexp tested against a single line. The match-any semantics intentionally
// captures any of the four markers a triager searches for first: the test name (--- FAIL:),
// the package summary (FAIL\t), a panic stack head (panic:), or a go-test timeout notice.
var proofGateFailureLinePatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{name: "test-fail", pattern: regexp.MustCompile(`^--- FAIL:`)},
	{name: "package-fail", pattern: regexp.MustCompile(`^FAIL\t`)},
	{name: "panic", pattern: regexp.MustCompile(`^panic:`)},
	{name: "timeout", pattern: regexp.MustCompile(`timed out`)},
}

// ExtractGoTestFailureLines returns up to ProofGateFailureLineCap lines from output that match
// any proofGateFailureLinePatterns entry, preserving source order. The result is nil when
// nothing matches; an empty output also yields nil. Each returned line is the raw line without
// trailing newline, exactly as the test emitted it.
func ExtractGoTestFailureLines(output []byte) []string {
	if len(output) == 0 {
		return nil
	}
	var matched []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		for _, entry := range proofGateFailureLinePatterns {
			if entry.pattern.MatchString(line) {
				matched = append(matched, line)
				break
			}
		}
		if len(matched) >= ProofGateFailureLineCap {
			break
		}
	}
	if len(matched) == 0 {
		return nil
	}
	return matched
}

// PersistGateOutput writes the full output bytes next to the --out report at
// <reportPath>.gates/<safe-gate-name>.log and returns the absolute path. Returns "" with no
// error when reportPath is empty (a gate run without --out has no sibling directory to own) or
// when output is empty (nothing to persist). The safe filename drops any character that is not
// alphanumerics or [-_.] so a maliciously-named gate cannot escape the gates directory.
func PersistGateOutput(reportPath, gateName string, output []byte) (string, error) {
	if strings.TrimSpace(reportPath) == "" || len(output) == 0 {
		return "", nil
	}
	reportDir := filepath.Dir(reportPath)
	leaf := filepath.Base(reportPath)
	gatesDir := filepath.Join(reportDir, leaf+".gates")
	if err := os.MkdirAll(gatesDir, 0o755); err != nil {
		return "", fmt.Errorf("create gates directory: %w", err)
	}
	target := filepath.Join(gatesDir, proofGateSafeFileName(gateName)+".log")
	if err := os.WriteFile(target, output, 0o600); err != nil {
		return "", fmt.Errorf("write gate output: %w", err)
	}
	return target, nil
}

// GateOutputReportPathContextKey carries the resolved --out report path into the per-step
// runner so a failed gate can persist its full output to <out>.gates/<gate>.log alongside
// the truncated OutputTail. Empty/absent means no report is in flight (a gate runner invoked
// from --dry-run or from tests) — PersistGateOutput treats empty as a no-op.
type GateOutputReportPathContextKey struct{}

// ContextWithGateOutputReportPath stamps the resolved --out report path into ctx so a
// downstream gate runner can persist its full output alongside the report. Empty path is a
// no-op so callers don't need a separate empty-check.
func ContextWithGateOutputReportPath(ctx context.Context, path string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(path) == "" {
		return ctx
	}
	return context.WithValue(ctx, GateOutputReportPathContextKey{}, path)
}

// GateOutputReportPathFromContext returns the resolved --out report path stamped by
// ContextWithGateOutputReportPath, or "" when none is in flight.
func GateOutputReportPathFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if path, ok := ctx.Value(GateOutputReportPathContextKey{}).(string); ok {
		return path
	}
	return ""
}

// ProofGateOutputTailMaxLen bounds the OutputTail rewrite when AttachFailedGateOutput surfaces
// a persist error. Matches the truncation the rest of proof-check applies to the tail, so a
// appended persist error stays the same shape as the surrounding tail.
const ProofGateOutputTailMaxLen = 4000

// AttachFailedGateOutput is the single-call replacement for the inline persistence +
// failure_lines plumbing that used to live in cmd/compound/proof_check.go:
//
//   - If the context carries a resolved --out report path, PersistGateOutput archives the
//     gate's full output to <out>.gates/<safe-gate>.log. The resulting absolute path is
//     written to result.OutputPath on success. PersistGateOutput is a no-op when the report
//     path is empty (--dry-run or a direct test invocation of the runner).
//
//   - For the go-test gate, ExtractGoTestFailureLines extracts up to ProofGateFailureLineCap
//     lines matching the canonical marker set and attaches them to result.FailureLines.
//
// When surfacePersistErr is true, any persist error is appended (truncated to
// ProofGateOutputTailMaxLen) to result.OutputTail so the gate's triage surface keeps both
// the failure and the persistence anomaly — the main gate-failure branch sets this. When
// false, the persist error is silently dropped — the console-battery fast-fail branch sets
// this because the synthetic reason is already in OutputTail and the helper must not
// overwrite it.
func AttachFailedGateOutput(
	result *ProofCheckStep,
	ctx context.Context,
	stepName string,
	output []byte,
	surfacePersistErr bool,
) {
	if reportPath := GateOutputReportPathFromContext(ctx); reportPath != "" {
		persisted, err := PersistGateOutput(reportPath, stepName, output)
		if err != nil {
			if surfacePersistErr {
				combined := strings.TrimSpace(
					fmt.Sprintf("%s\npersisted-gate-output error: %v", result.OutputTail, err),
				)
				if len(combined) > ProofGateOutputTailMaxLen {
					combined = combined[len(combined)-ProofGateOutputTailMaxLen:]
				}
				result.OutputTail = combined
			}
			return
		}
		if persisted != "" {
			result.OutputPath = persisted
		}
	}
	if stepName == "go-test" || stepName == HomeFixtureGateName {
		result.FailureLines = ExtractGoTestFailureLines(output)
	}
}

func proofGateSafeFileName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "gate"
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "gate"
	}
	return b.String()
}

// ExitCodeFromRunErr returns the process exit code carried in err when err is an
// *exec.ExitError, otherwise 1. Used by the proof-check step runner to stamp ExitCode on a
// failed ProofCheckStep result without re-implementing the errors.As walk at every call site.
func ExitCodeFromRunErr(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
