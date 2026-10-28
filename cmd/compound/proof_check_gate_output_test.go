package main

import (
	"bytes"
	"compound/internal/work"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestProofCheckFailedGoTestGatePersistsFullOutputToSiblingFile is the red-then-green falsifier
// for the deliverable that proves proof-check keeps the FULL output of a red go-test gate, not
// only its 4000-char tail. The fixture emits 10,000 lines of noise BEFORE the FAIL marker so
// the tail truncation can never carry the markers; the persisted file must.
//
// RED proof before the implementation: OutputPath is empty AND FailureLines is nil.
// GREEN proof after: OutputPath is set under <outPath>.gates/go-test.log, the persisted file
// contains the FAIL line, the package-fail line, AND both first/last noise lines, and
// FailureLines contains the --- FAIL: and FAIL\t markers.
func TestProofCheckFailedGoTestGatePersistsFullOutputToSiblingFile(t *testing.T) {
	originalResolver := proofCheckRuntimeExecutable
	t.Cleanup(func() { proofCheckRuntimeExecutable = originalResolver })

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fails.sh")
	scriptBody := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 10000 ]; do\n" +
		"  echo \"ok-truncation-noise-$(printf '%05d' $i)\"\n" +
		"  i=$((i + 1))\n" +
		"done\n" +
		"echo \"--- FAIL: TestBigLogFailure (0.01s)\"\n" +
		"echo \"    biglog_test.go:10: assertion failed\"\n" +
		"printf 'FAIL\\tcompound/synthetic\\t0.05s\\n'\n" +
		"echo \"FAIL\"\n" +
		"exit 1\n"
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	// Resolve the script itself as its own executable so the runner invokes it directly.
	// The runner replaces step.Command[0] with proofCheckRuntimeExecutable(step.Command[0]);
	// returning scriptPath here means exec.Command runs the script with its #!/bin/sh
	// shebang. Using "/bin/sh" as the resolver (the test pattern elsewhere) would drop the
	// script path because the runner consumes step.Command[0] as the executable.
	proofCheckRuntimeExecutable = func(name string) (string, error) {
		if name == scriptPath {
			return scriptPath, nil
		}
		return name, nil
	}

	outPath := filepath.Join(t.TempDir(), "report.json")
	ctx := work.ContextWithGateOutputReportPath(context.Background(), outPath)

	result := runProofCheckStepContext(ctx, t.TempDir(), proofCheckStep{
		Name:     "go-test",
		Command:  []string{scriptPath},
		ProofRef: "go-test-count-1-all",
	}, "")

	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed; output_tail=%q output_path=%q", result.Status, result.OutputTail, result.OutputPath)
	}
	if result.OutputPath == "" {
		t.Fatalf("OutputPath is empty; want a path under %s.gates/", outPath)
	}
	wantPrefix := outPath + ".gates" + string(filepath.Separator)
	if !strings.HasPrefix(result.OutputPath, wantPrefix) {
		t.Fatalf("OutputPath = %q, want prefix %q", result.OutputPath, wantPrefix)
	}
	if filepath.Base(result.OutputPath) != "go-test.log" {
		t.Fatalf("OutputPath leaf = %q, want go-test.log", filepath.Base(result.OutputPath))
	}
	body, err := os.ReadFile(result.OutputPath)
	if err != nil {
		t.Fatalf("read persisted output %q: %v", result.OutputPath, err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "ok-truncation-noise-00000") {
		t.Fatalf("persisted output missing first noise line; file length=%d", len(body))
	}
	if !strings.Contains(bodyStr, "ok-truncation-noise-09999") {
		t.Fatalf("persisted output missing last noise line; file length=%d", len(body))
	}
	if !strings.Contains(bodyStr, "--- FAIL: TestBigLogFailure") {
		t.Fatalf("persisted output missing FAIL line; tail=%q", tailForTest(bodyStr, 4000))
	}
	if !strings.Contains(bodyStr, "FAIL\tcompound/synthetic\t0.05s") {
		t.Fatalf("persisted output missing package FAIL line")
	}

	// The persisted file MUST be substantially larger than the OutputTail — that is the
	// whole point of the deliverable. If the file's only a few hundred bytes longer than
	// the tail, the bug the packet describes (named failure buried by 150 packages of ok
	// output) is NOT actually fixed by the implementation.
	if len(body) <= len(result.OutputTail) {
		t.Fatalf("persisted output (%d bytes) is not larger than OutputTail (%d bytes); full output was not retained",
			len(body), len(result.OutputTail))
	}

	// FailureLines must contain the two test markers we emitted.
	if len(result.FailureLines) < 2 {
		t.Fatalf("FailureLines = %v, want at least 2 entries (--- FAIL: and FAIL\\t)", result.FailureLines)
	}
	sawTest, sawPackage := false, false
	for _, line := range result.FailureLines {
		if strings.HasPrefix(line, "--- FAIL:") {
			sawTest = true
		}
		if strings.HasPrefix(line, "FAIL\t") {
			sawPackage = true
		}
	}
	if !sawTest || !sawPackage {
		t.Fatalf("FailureLines = %v, want --- FAIL: AND FAIL\\t markers", result.FailureLines)
	}
}

// TestProofCheckFailedStepLeavesOutputPathEmptyWithoutReportPath proves the runner is a
// no-op when no --out report is in flight: tests, --dry-run, and any other context that
// lacks the report path must NOT create a sibling directory or set OutputPath.
func TestProofCheckFailedStepLeavesOutputPathEmptyWithoutReportPath(t *testing.T) {
	originalResolver := proofCheckRuntimeExecutable
	proofCheckRuntimeExecutable = func(string) (string, error) { return "/bin/sh", nil }
	t.Cleanup(func() { proofCheckRuntimeExecutable = originalResolver })

	result := runProofCheckStepContext(context.Background(), t.TempDir(), proofCheckStep{
		Name:     "synthetic-no-context",
		Command:  []string{"sh", "-c", "echo 'always red'; exit 1"},
		ProofRef: "synthetic-no-context-proof",
	}, "")
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if result.OutputPath != "" {
		t.Fatalf("OutputPath = %q, want empty when no report path is set in context", result.OutputPath)
	}
}

// TestProofCheckGoTestGateFailureLinesBoundedToCap proves the 200-line cap survives a
// pathological red battery. We emit 500 distinct --- FAIL: lines plus 500 distinct panic:
// lines; the result must contain exactly ProofGateFailureLineCap (= 200) entries — first
// 200 of them in source order — and never crash.
func TestProofCheckGoTestGateFailureLinesBoundedToCap(t *testing.T) {
	originalResolver := proofCheckRuntimeExecutable
	t.Cleanup(func() { proofCheckRuntimeExecutable = originalResolver })

	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for i := 0; i < 500; i++ {
		script.WriteString("echo \"--- FAIL: TestRed")
		script.WriteString(itoaForTest(i))
		script.WriteString(" (0.01s)\"\n")
	}
	for i := 0; i < 500; i++ {
		script.WriteString("echo \"panic: synthetic panic ")
		script.WriteString(itoaForTest(i))
		script.WriteString("\"\n")
	}
	script.WriteString("exit 1\n")
	scriptPath := filepath.Join(t.TempDir(), "many-fails.sh")
	if err := os.WriteFile(scriptPath, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	proofCheckRuntimeExecutable = func(name string) (string, error) {
		if name == scriptPath {
			return scriptPath, nil
		}
		return name, nil
	}

	outPath := filepath.Join(t.TempDir(), "report.json")
	ctx := work.ContextWithGateOutputReportPath(context.Background(), outPath)
	result := runProofCheckStepContext(ctx, t.TempDir(), proofCheckStep{
		Name:     "go-test",
		Command:  []string{scriptPath},
		ProofRef: "go-test-count-1-all",
	}, "")
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if got, want := len(result.FailureLines), 200; got != want {
		t.Fatalf("FailureLines count = %d, want %d (the canonical cap)", got, want)
	}
	if !strings.HasPrefix(result.FailureLines[0], "--- FAIL:") {
		t.Fatalf("FailureLines[0] = %q, want --- FAIL: prefix (source-order preservation)", result.FailureLines[0])
	}
}

// TestProofCheckNonGoTestGateSkipsFailureLines proves the failure_lines extraction is
// gated to the go-test inventory. A red arbitrary gate MUST persist its full output to
// <out>.gates/<gate>.log but must NOT populate failure_lines (the marker set is go-test-
// specific and an empty array would mislead triagers looking for FAIL-shaped signals).
func TestProofCheckNonGoTestGateSkipsFailureLines(t *testing.T) {
	originalResolver := proofCheckRuntimeExecutable
	proofCheckRuntimeExecutable = func(string) (string, error) { return "/bin/sh", nil }
	t.Cleanup(func() { proofCheckRuntimeExecutable = originalResolver })

	outPath := filepath.Join(t.TempDir(), "report.json")
	ctx := work.ContextWithGateOutputReportPath(context.Background(), outPath)
	result := runProofCheckStepContext(ctx, t.TempDir(), proofCheckStep{
		Name:     "go-vet",
		Command:  []string{"sh", "-c", "echo 'vet error: argh'; exit 1"},
		ProofRef: "go-vet-all",
	}, "")
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if result.OutputPath == "" {
		t.Fatal("OutputPath is empty for a non-go-test failed gate; want full output persisted")
	}
	if result.FailureLines != nil {
		t.Fatalf("FailureLines = %v, want nil for non-go-test gates", result.FailureLines)
	}
}

// TestProofCheckGateOutputSafeFileNameForHostileGateName protects the sibling-directory
// invariant: a gate name with path separators must NEVER escape the gates directory. The
// safe-name filter rewrites `/` to `_`, so the resulting file stays under
// <out>.gates/ even for hostile inputs like "../etc/passwd" or "foo/bar".
func TestProofCheckGateOutputSafeFileNameForHostileGateName(t *testing.T) {
	for _, hostile := range []string{"../etc/passwd", "foo/bar", "..\\windows\\system32", ""} {
		t.Run(hostile, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "report.json")
			gatesDir := outPath + ".gates"
			target, err := work.PersistGateOutput(outPath, hostile, []byte("body\n"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(target, gatesDir+string(filepath.Separator)) {
				t.Fatalf("target %q escaped gates dir %q", target, gatesDir)
			}
			// filepath.Clean must keep the target inside gatesDir — this is the canonical
			// "no parent-dir escape" check that doesn't depend on prefix matching.
			cleaned := filepath.Clean(target)
			cleanedDir := filepath.Clean(gatesDir)
			if !strings.HasPrefix(cleaned, cleanedDir+string(filepath.Separator)) && cleaned != cleanedDir {
				t.Fatalf("cleaned target %q escaped cleaned gates dir %q", cleaned, cleanedDir)
			}
		})
	}
}

func tailForTest(s string, max int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func itoaForTest(i int) string {
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

const nonCertifyingOutputHelperEnv = "COMPOUND_TEST_NONCERTIFYING_OUTPUT_HELPER"

// Exercise the real noncertifying dispatch: a completed red Go gate must leave
// its full diagnostic output even when a later gate terminates the process.
func TestProofCheckNonCertifyingFailedOutputSurvivesLaterInterruption(t *testing.T) {
	for _, mode := range []string{"--dev", "--presubmit"} {
		t.Run(strings.TrimPrefix(mode, "--"), func(t *testing.T) {
			repo, base := newProofCheckPresubmitRepo(t)
			proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "internal/home/home.go"), "package home\n")
			proofCheckDevelopmentWriteFile(t, filepath.Join(repo, "internal/work/work.go"), "package work\n\nimport (\n\t_ \"compound/internal/home\"\n\t_ \"compound/internal/landlease\"\n)\n")
			proofCheckPresubmitCommit(t, repo, "select isolated home phase")

			root := t.TempDir()
			expected := []byte(strings.Repeat("output-before-tail\n", 600) + "--- FAIL: TestRetainedNonCertifyingFailure (0.01s)\nFAIL\tcompound/synthetic\t0.01s\n")
			bodyPath := filepath.Join(root, "failure.txt")
			if err := os.WriteFile(bodyPath, expected, 0o600); err != nil {
				t.Fatal(err)
			}
			realGo, err := exec.LookPath("go")
			if err != nil {
				t.Fatal(err)
			}
			fakeGo := filepath.Join(root, "go")
			script := "#!/bin/sh\ncase \"$1\" in\n  test|run|architecture) ;;\n  *) exec \"$COMPOUND_TEST_REAL_GO\" \"$@\" ;;\nesac\nprintf '%s\\n' --invocation-- \"$@\" >> \"$COMPOUND_TEST_NONCERTIFYING_ARGV\"\nif [ \"$1\" = test ]; then\n  case \" $* \" in *\" compound/internal/home \"*)\n    printf 'later gate reached\\n' > \"$COMPOUND_TEST_NONCERTIFYING_MARKER\"\n    kill -TERM \"$PPID\"\n    exit 29\n  esac\n  cat \"$COMPOUND_TEST_NONCERTIFYING_FAILURE_BODY\"\n  exit 1\nfi\nexit 0\n"
			if err := os.WriteFile(fakeGo, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(root, "report.json")
			marker := filepath.Join(root, "interrupted")
			argvPath := filepath.Join(root, "argv")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestProofCheckNonCertifyingOutputHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(),
				nonCertifyingOutputHelperEnv+"=1",
				"COMPOUND_TEST_NONCERTIFYING_MODE="+mode,
				"COMPOUND_TEST_NONCERTIFYING_REPO="+repo,
				"COMPOUND_TEST_NONCERTIFYING_BASE="+base,
				"COMPOUND_TEST_NONCERTIFYING_OUT="+outPath,
				"COMPOUND_TEST_NONCERTIFYING_GO="+fakeGo,
				"COMPOUND_TEST_REAL_GO="+realGo,
				"PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
				"COMPOUND_TEST_NONCERTIFYING_FAILURE_BODY="+bodyPath,
				"COMPOUND_TEST_NONCERTIFYING_MARKER="+marker,
				"COMPOUND_TEST_NONCERTIFYING_ARGV="+argvPath,
				"COMPOUND_HOME="+filepath.Join(root, "compound-home"),
				"HOME="+root,
				"XDG_CACHE_HOME="+filepath.Join(root, "cache"),
				"GOCACHE="+filepath.Join(root, "go-cache"),
			)
			output, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || (mode == "--dev" && exit.ExitCode() != 29) || (mode == "--presubmit" && exit.ExitCode() != -1) {
				t.Fatalf("later gate did not interrupt as configured: %v\n%s", err, output)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("later gate was not reached: %v", err)
			}
			body, err := os.ReadFile(outPath + ".gates/go-test.log")
			if err != nil {
				t.Fatalf("completed failed gate output was lost before final report: %v", err)
			}
			if !bytes.Equal(body, expected) {
				t.Fatalf("retained output differs: got %d bytes, want %d", len(body), len(expected))
			}
			argv, err := os.ReadFile(argvPath)
			if err != nil {
				t.Fatal(err)
			}
			fresh := strings.Contains(string(argv), "--invocation--\ntest\n-short\n-count=1\n")
			if fresh != (mode == "--presubmit") {
				t.Fatalf("presubmit must force fresh execution while dev keeps cached iteration:\n%s", argv)
			}
			if _, err := os.Stat(outPath); !os.IsNotExist(err) {
				t.Fatalf("interrupted run must not publish a final report: %v", err)
			}
		})
	}
}

func TestProofCheckNonCertifyingOutputHelper(t *testing.T) {
	if os.Getenv(nonCertifyingOutputHelperEnv) != "1" {
		return
	}
	mode := os.Getenv("COMPOUND_TEST_NONCERTIFYING_MODE")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	interrupt := func() {
		if err := os.WriteFile(os.Getenv("COMPOUND_TEST_NONCERTIFYING_MARKER"), []byte("later gate reached\n"), 0o600); err != nil {
			os.Exit(28)
		}
		os.Exit(29)
	}
	proofCheckRuntimeExecutable = func(name string) (string, error) {
		if name == "go" {
			return os.Getenv("COMPOUND_TEST_NONCERTIFYING_GO"), nil
		}
		if name == exe {
			// Development dispatches the doctor after its red Go gate.
			if mode == "--dev" {
				interrupt()
			}
			return os.Getenv("COMPOUND_TEST_NONCERTIFYING_GO"), nil
		}
		return name, nil
	}
	cmdProofCheckDevelopment([]string{
		mode,
		"--repo", os.Getenv("COMPOUND_TEST_NONCERTIFYING_REPO"),
		"--base", os.Getenv("COMPOUND_TEST_NONCERTIFYING_BASE"),
		"--out", os.Getenv("COMPOUND_TEST_NONCERTIFYING_OUT"),
	})
	os.Exit(27)
}
