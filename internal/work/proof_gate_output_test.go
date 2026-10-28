package work

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractGoTestFailureLinesMatchesAllFourMarkers is the falsifier for the marker set:
// a synthetic output containing one of each canonical pattern must return all four in
// source order. A mutation that drops one pattern (e.g. removing "timed out") would surface
// here as a 3-element result.
func TestExtractGoTestFailureLinesMatchesAllFourMarkers(t *testing.T) {
	t.Parallel()
	output := []byte(strings.Join([]string{
		"ok  \tcompound/internal/foo\t0.01s",
		"--- FAIL: TestOne (0.01s)",
		"    foo_test.go:10: got 1, want 2",
		"FAIL\tcompound/internal/foo\t0.02s",
		"panic: runtime error: index out of range",
		"goroutine 1 [running]:",
		"main.main()",
		"	test timed out after 5m0s",
	}, "\n"))
	got := ExtractGoTestFailureLines(output)
	want := []string{
		"--- FAIL: TestOne (0.01s)",
		"FAIL\tcompound/internal/foo\t0.02s",
		"panic: runtime error: index out of range",
		"	test timed out after 5m0s",
	}
	if !equalStringSlices(got, want) {
		t.Fatalf("ExtractGoTestFailureLines:\n got=%v\nwant=%v", got, want)
	}
}

// TestExtractGoTestFailureLinesBoundedToCap proves the 200-line cap is enforced even when
// 1000 distinct markers all match. Source order is preserved by taking the first 200.
func TestExtractGoTestFailureLinesBoundedToCap(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	for i := 0; i < 1000; i++ {
		output.WriteString("--- FAIL: TestRed")
		output.WriteString(itoaCapTest(i))
		output.WriteString(" (0.01s)\n")
	}
	got := ExtractGoTestFailureLines(output.Bytes())
	if len(got) != ProofGateFailureLineCap {
		t.Fatalf("len = %d, want %d", len(got), ProofGateFailureLineCap)
	}
	if !strings.HasPrefix(got[0], "--- FAIL: TestRed0") {
		t.Fatalf("got[0] = %q, want first source-order marker", got[0])
	}
}

// TestExtractGoTestFailureLinesEmptyOutput proves nil-on-empty: the helper is documented to
// return nil for empty input so the JSON omitempty tag drops the field rather than render
// "failure_lines": [].
func TestExtractGoTestFailureLinesEmptyOutput(t *testing.T) {
	t.Parallel()
	if got := ExtractGoTestFailureLines(nil); got != nil {
		t.Fatalf("nil input = %v, want nil", got)
	}
	if got := ExtractGoTestFailureLines([]byte("")); got != nil {
		t.Fatalf("empty input = %v, want nil", got)
	}
	if got := ExtractGoTestFailureLines([]byte("ok  \tcompound/internal/foo\t0.01s\n")); got != nil {
		t.Fatalf("non-empty non-matching input = %v, want nil", got)
	}
}

// TestPersistGateOutputWritesSiblingFileInsideGatesDir is the happy path: a non-empty
// reportPath plus a non-empty output produces an absolute path under <reportPath>.gates/,
// the file exists, and its bytes match what was passed in.
func TestPersistGateOutputWritesSiblingFileInsideGatesDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	body := []byte("full output bytes\n")
	got, err := PersistGateOutput(reportPath, "go-test", body)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(dir, "report.json.gates") + string(filepath.Separator)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("got %q, want prefix %q", got, wantPrefix)
	}
	if filepath.Base(got) != "go-test.log" {
		t.Fatalf("got leaf = %q, want go-test.log", filepath.Base(got))
	}
	persisted, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(persisted, body) {
		t.Fatalf("persisted = %q, want %q", persisted, body)
	}
}

// TestPersistGateOutputNoopOnEmptyReportPath proves the helper treats a missing --out path
// as a no-op (returns "", nil). Tests, --dry-run, and any other context that lacks a
// report path must not create empty sibling directories.
func TestPersistGateOutputNoopOnEmptyReportPath(t *testing.T) {
	t.Parallel()
	got, err := PersistGateOutput("", "go-test", []byte("body\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty when reportPath is empty", got)
	}
}

// TestPersistGateOutputNoopOnEmptyOutput proves the helper treats an empty output body as
// a no-op. A 0-byte write would still create the file but convey nothing; the omitempty
// tag on OutputPath means the report would render no output_path for it anyway.
func TestPersistGateOutputNoopOnEmptyOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	got, err := PersistGateOutput(reportPath, "go-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty when output is nil", got)
	}
}

// TestPersistGateOutputRefusesPathTraversal pins the safe-filename behavior at the helper
// layer: a hostile gate name like "../../etc/passwd" must produce a path inside the gates
// directory and never escape it. The cmd/compound test covers this end-to-end too; the
// internal layer owns the invariant.
func TestPersistGateOutputRefusesPathTraversal(t *testing.T) {
	t.Parallel()
	for _, hostile := range []string{"../../../etc/passwd", "foo/bar", "..\\windows\\system32"} {
		t.Run(hostile, func(t *testing.T) {
			dir := t.TempDir()
			reportPath := filepath.Join(dir, "report.json")
			gatesDir := filepath.Join(dir, "report.json.gates")
			got, err := PersistGateOutput(reportPath, hostile, []byte("hostile\n"))
			if err != nil {
				t.Fatal(err)
			}
			cleaned := filepath.Clean(got)
			cleanedDir := filepath.Clean(gatesDir)
			if !strings.HasPrefix(cleaned, cleanedDir+string(filepath.Separator)) && cleaned != cleanedDir {
				t.Fatalf("cleaned %q escaped cleaned gates dir %q", cleaned, cleanedDir)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
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

func itoaCapTest(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
