package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/arch"
	"compound/internal/authority/gitrepository"
	"compound/internal/landlease"
	"compound/internal/work"
)

func TestCanonicalProofGatesReadSealedHeadDuringTransientCallerMutation(t *testing.T) {
	t.Setenv("COMPOUND_PROOF_SHARED_GOCACHE", "")
	// The canonical gates run with a per-worktree Go cache. Isolate its root
	// BEFORE the runner starts so the gates and the expectation below agree,
	// and so the fixture never writes into the real user cache.
	_, cacheBase := proofCheckGoCacheFixture(t)
	repo := initCompoundCommandGitRepo(t)
	compiler := filepath.Join(repo, "console", "node_modules", ".bin", "tsc")
	if err := os.MkdirAll(filepath.Dir(compiler), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("console/node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", ".gitignore")
	sentinelPath := filepath.Join(repo, "sealed-sentinel.txt")
	const sealedBody = "sealed candidate bytes\n"
	const transientBody = "transient caller mutation\n"
	if err := os.WriteFile(sentinelPath, []byte(sealedBody), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", "sealed-sentinel.txt")
	runCommandGit(t, repo, "commit", "-m", "seal sentinel")

	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps)
	if err != nil {
		t.Fatal(err)
	}

	controlDir := t.TempDir()
	startedPath := filepath.Join(controlDir, "started")
	allowPath := filepath.Join(controlDir, "allow")
	observedPath := filepath.Join(controlDir, "observed")
	observedDirPath := filepath.Join(controlDir, "observed-dir")
	observedCachePath := filepath.Join(controlDir, "observed-cache")
	observedCompletePath := filepath.Join(controlDir, "observed-complete")
	oncePath := filepath.Join(controlDir, "once")
	gatePath := filepath.Join(controlDir, "proof-gate")
	gateBody := `#!/bin/sh
if mkdir "$PROOF_GATE_ONCE" 2>/dev/null; then
  : > "$PROOF_GATE_STARTED"
  while [ ! -e "$PROOF_GATE_ALLOW" ]; do
    sleep 0.01
  done
  pwd > "$PROOF_GATE_OBSERVED_DIR"
  printf '%s\n' "$GOCACHE" > "$PROOF_GATE_OBSERVED_CACHE"
  cat sealed-sentinel.txt > "$PROOF_GATE_OBSERVED"
  : > "$PROOF_GATE_OBSERVED_COMPLETE"
  # Hold the first gate here until the caller cancels the run. The gate must
  # not exit on its own: a gate that returns lets the run race into later
  # gates, and the cancellation then lands on an arbitrary step instead of
  # this one. The whole process group is killed on cancellation.
  while :; do
    sleep 0.05
  done
fi
exit 0
`
	if err := os.WriteFile(gatePath, []byte(gateBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROOF_GATE_ONCE", oncePath)
	t.Setenv("PROOF_GATE_STARTED", startedPath)
	t.Setenv("PROOF_GATE_ALLOW", allowPath)
	t.Setenv("PROOF_GATE_OBSERVED", observedPath)
	t.Setenv("PROOF_GATE_OBSERVED_DIR", observedDirPath)
	t.Setenv("PROOF_GATE_OBSERVED_CACHE", observedCachePath)
	t.Setenv("PROOF_GATE_OBSERVED_COMPLETE", observedCompletePath)

	originalResolver := proofCheckRuntimeExecutable
	proofCheckRuntimeExecutable = func(string) (string, error) {
		return gatePath, nil
	}
	t.Cleanup(func() {
		proofCheckRuntimeExecutable = originalResolver
	})

	type runnerResult struct {
		report proofCheckReport
		err    error
	}
	runnerContext, cancelRunner := context.WithCancel(
		work.ContextWithCanonicalProofPlan(context.Background(), plan),
	)
	defer cancelRunner()
	done := make(chan runnerResult, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		report, runErr := proofCheckCanonicalRunner(
			runnerContext,
			repo,
			false,
			source,
			false,
			false,
			time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC),
		)
		done <- runnerResult{report: report, err: runErr}
	}()
	// Registered before any failure below can end the test: an early Fatal
	// still cancels the owned run and joins it, so no gate survives the
	// temporary paths it observes.
	t.Cleanup(func() {
		cancelRunner()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("cancelled canonical run did not return")
		}
	})

	waitForProofCheckTestPath(t, startedPath)
	if err := os.WriteFile(sentinelPath, []byte(transientBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(allowPath, []byte("continue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForProofCheckTestPath(t, observedCompletePath)
	observed, err := os.ReadFile(observedPath)
	if err != nil {
		t.Fatal(err)
	}
	observedDir, err := os.ReadFile(observedDirPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinelPath, []byte(sealedBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cancelRunner()

	// The normal receive is bounded: a hung run must fail the control here
	// instead of blocking forever, because the cleanup's join timeout cannot
	// fire while the test goroutine waits on an unbounded receive.
	var result runnerResult
	select {
	case result = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled canonical run did not return")
	}
	// Production proof-check reports the cancellation cause; a cancelled run is
	// never a successful run.
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled canonical run error = %v, want context.Canceled", result.err)
	}
	if result.report.Status != "failed" {
		t.Fatalf("cancelled canonical report status = %q, want failed", result.report.Status)
	}
	if len(result.report.Steps) == 0 {
		t.Fatal("cancelled canonical report carried no partial step")
	}
	if len(result.report.Steps) >= len(plan.Steps) {
		t.Fatalf("cancelled canonical report carried %d steps, want a partial report of fewer than %d", len(result.report.Steps), len(plan.Steps))
	}
	if result.report.Steps[0].Status != "failed" {
		t.Fatalf("cancelled canonical first step status = %q, want failed", result.report.Steps[0].Status)
	}
	if result.report.Repo != repo {
		t.Fatalf("proof report repo = %q, want caller repo identity %q", result.report.Repo, repo)
	}
	if got := string(observed); got != sealedBody {
		t.Fatalf("canonical gate read %q, want sealed HEAD bytes %q", got, sealedBody)
	}
	sealedGateDir := strings.TrimSpace(string(observedDir))
	if sealedGateDir == repo {
		t.Fatalf("canonical gate executed in mutable caller worktree %s", sealedGateDir)
	}
	if _, err := os.Stat(sealedGateDir); !os.IsNotExist(err) {
		t.Fatalf("cancelled canonical run left sealed workspace behind: %v", err)
	}
	restored, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(restored); got != sealedBody {
		t.Fatalf("caller worktree changed after proof: got %q, want %q", got, sealedBody)
	}
	if status := proofSourceGitOutput(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("caller worktree left dirty after proof: %q", status)
	}
	observedCache, err := os.ReadFile(observedCachePath)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := work.ProofGoCache(repo)
	if !ok {
		t.Fatal("no cache chosen for original caller")
	}
	if !strings.HasPrefix(want, cacheBase+string(filepath.Separator)) {
		t.Fatalf("cache %s escaped the isolated fixture cache root %s", want, cacheBase)
	}
	if got := strings.TrimSpace(string(observedCache)); got != want {
		t.Fatalf("canonical gate cache = %q, want original caller cache %q (sealed cwd %q)", got, want, sealedGateDir)
	}
}

func TestPrepareProofCheckSealedWorkspaceCopiesOnlyCeremonyInputsAndCleansUp(t *testing.T) {
	t.Parallel()

	repo := initCompoundCommandGitRepo(t)
	ignoreBody := strings.Join([]string{
		"docs_private/",
		"console/node_modules/",
		"other-ignored/",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(ignoreBody), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", ".gitignore")
	runCommandGit(t, repo, "commit", "-m", "ignore ceremony inputs")

	inputs := map[string]string{
		"docs_private/leak-tokens.txt":      "leak tokens\n",
		"docs_private/plan/HANDOFF.md":      "private plan\n",
		"console/node_modules/pkg/file.js":  "module bytes\n",
		"docs_private/profiles/registry.ts": "profile bytes\n",
	}
	for relativePath, body := range inputs {
		path := filepath.Join(repo, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unownedPath := filepath.Join(repo, "other-ignored", "private.txt")
	if err := os.MkdirAll(filepath.Dir(unownedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unownedPath, []byte("must not copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	internalLink := filepath.Join(repo, "console", "node_modules", ".bin", "fixture-tool")
	if err := os.MkdirAll(filepath.Dir(internalLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../pkg/file.js", internalLink); err != nil {
		t.Fatal(err)
	}

	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	sealedRepo, cleanup, err := prepareProofCheckSealedWorkspace(t.Context(), repo, source)
	if err != nil {
		t.Fatal(err)
	}
	for relativePath, want := range inputs {
		body, readErr := os.ReadFile(filepath.Join(sealedRepo, filepath.FromSlash(relativePath)))
		if readErr != nil {
			t.Fatalf("read copied ceremony input %s: %v", relativePath, readErr)
		}
		if got := string(body); got != want {
			t.Fatalf("copied ceremony input %s = %q, want %q", relativePath, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(sealedRepo, "other-ignored", "private.txt")); !os.IsNotExist(err) {
		t.Fatalf("non-ceremony ignored input copied into sealed workspace: %v", err)
	}
	copiedLink := filepath.Join(sealedRepo, "console", "node_modules", ".bin", "fixture-tool")
	linkTarget, err := os.Readlink(copiedLink)
	if err != nil {
		t.Fatalf("read copied internal node_modules symlink: %v", err)
	}
	if linkTarget != "../pkg/file.js" {
		t.Fatalf("copied internal symlink target = %q, want ../pkg/file.js", linkTarget)
	}
	if body, err := os.ReadFile(copiedLink); err != nil || string(body) != "module bytes\n" {
		t.Fatalf("copied internal symlink did not resolve within snapshot: body=%q err=%v", body, err)
	}
	if err := os.WriteFile(
		filepath.Join(repo, "docs_private", "plan", "HANDOFF.md"),
		[]byte("mutated source overlay\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	sealedPlan, err := os.ReadFile(filepath.Join(sealedRepo, "docs_private", "plan", "HANDOFF.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(sealedPlan); got != inputs["docs_private/plan/HANDOFF.md"] {
		t.Fatalf("sealed ceremony input followed caller mutation: %q", got)
	}
	if head := proofSourceGitOutput(t, sealedRepo, "rev-parse", "HEAD"); head != source.HeadSHA {
		t.Fatalf("sealed workspace HEAD = %s, want %s", head, source.HeadSHA)
	}
	cleanup()
	if _, err := os.Stat(sealedRepo); !os.IsNotExist(err) {
		t.Fatalf("sealed workspace survived cleanup: %v", err)
	}
}

func TestPrepareProofCheckSealedWorkspacePreservesSourceLeakOverlayForDoctor(t *testing.T) {
	t.Parallel()

	repo := initCompoundCommandGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("docs_private/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const sourceToken = "syntheticproductneedle"
	sourceRel := filepath.Join("internal", "fixture", "fixture.go")
	sourcePath := filepath.Join(repo, sourceRel)
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package fixture\n\nconst product = \""+sourceToken+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", ".gitignore", sourceRel)
	runCommandGit(t, repo, "commit", "-m", "seal source token fixture")

	overlayPath := filepath.Join(repo, "docs_private", "source-leak-tokens.txt")
	if err := os.MkdirAll(filepath.Dir(overlayPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlayPath, []byte(sourceToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	sealedRepo, cleanup, err := prepareProofCheckSealedWorkspace(t.Context(), repo, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup sealed workspace: %v", err)
		}
	})

	report := arch.DoctorSource(sealedRepo, true)
	var docLeakFindings []arch.Finding
	for _, finding := range report.Findings {
		if finding.Check == "doc-leak" {
			docLeakFindings = append(docLeakFindings, finding)
		}
		if finding.Check == "doc-leak" &&
			finding.Severity == arch.Fail &&
			strings.Contains(finding.Message, filepath.ToSlash(sourceRel)) &&
			strings.Contains(finding.Message, sourceToken) {
			return
		}
	}
	t.Fatalf("sealed architecture doctor missed source overlay token %q in %s: doc_leak_findings=%+v", sourceToken, filepath.ToSlash(sourceRel), docLeakFindings)
}

func TestPrepareProofCheckSealedWorkspaceRejectsEscapingSymlinks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target func(t *testing.T, linkDir string) string
	}{
		{
			name: "absolute",
			target: func(t *testing.T, _ string) string {
				t.Helper()
				path := filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(path, []byte("mutable outside bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "relative escape",
			target: func(t *testing.T, linkDir string) string {
				t.Helper()
				path := filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(path, []byte("mutable outside bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				relative, err := filepath.Rel(linkDir, path)
				if err != nil {
					t.Fatal(err)
				}
				return relative
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo, source := proofCheckSealedIgnoredInputRepo(t)
			linkDir := filepath.Join(repo, "docs_private", "plan")
			if err := os.MkdirAll(linkDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target(t, linkDir), filepath.Join(linkDir, "escape")); err != nil {
				t.Fatal(err)
			}
			sealedRepo, cleanup, err := prepareProofCheckSealedWorkspace(t.Context(), repo, source)
			if cleanup != nil {
				cleanup()
			}
			if err == nil {
				t.Fatalf("escaping symlink admitted into sealed workspace %s", sealedRepo)
			}
			if !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("escaping symlink error = %v, want symlink refusal", err)
			}
		})
	}
}

func TestPrepareProofCheckSealedWorkspaceDescendantKeepsExecutionBarrierAfterHelperExit(t *testing.T) {
	homePath := t.TempDir()
	holder := "proof:pfr_sealed_prepare:ratt_20260728T101500Z_aaaaaaaa:work_sealed_prepare:codex-sealed"
	now := time.Date(2026, 7, 28, 10, 15, 0, 0, time.UTC)
	_, barrier, err := landlease.AcquireWithExecutionBarrierPrepared(
		homePath,
		holder,
		time.Minute,
		now,
		func(landlease.AcquireOutcome) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = barrier.Close() })

	controlDir := t.TempDir()
	startedPath := filepath.Join(controlDir, "started")
	allowPath := filepath.Join(controlDir, "allow")
	pidPath := filepath.Join(controlDir, "pid")
	t.Setenv("PROOF_PREP_STARTED", startedPath)
	t.Setenv("PROOF_PREP_ALLOW", allowPath)
	t.Setenv("PROOF_PREP_PID", pidPath)
	proofCheckInstallFakeGit(t, `#!/bin/sh
(
  : > "$PROOF_PREP_STARTED"
  while [ ! -e "$PROOF_PREP_ALLOW" ]; do
    sleep 0.01
  done
) </dev/null >/dev/null 2>&1 &
echo $! > "$PROOF_PREP_PID"
while [ ! -e "$PROOF_PREP_STARTED" ]; do
  sleep 0.01
done
exit 42
`)
	t.Cleanup(func() {
		_ = os.WriteFile(allowPath, []byte("stop\n"), 0o644)
	})

	ctx := proofCheckContextWithExecutionBarrier(context.Background(), barrier)
	if _, _, err := prepareProofCheckSealedWorkspace(ctx, t.TempDir(), gitrepository.Snapshot{}); err == nil {
		t.Fatal("fake preparation helper unexpectedly succeeded")
	}
	waitForProofCheckTestPath(t, startedPath)
	childPID := waitForProofCheckTestPID(t, pidPath)
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}

	finalizerCalled := false
	released, err := landlease.ReleaseAfter(homePath, holder, now.Add(time.Second), func() error {
		finalizerCalled = true
		return nil
	})
	var pending *landlease.ExecutionPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("escaped preparation descendant did not hold execution barrier: released=%+v err=%v", released, err)
	}
	if finalizerCalled {
		t.Fatal("ReleaseAfter invoked finalizer while preparation descendant was alive")
	}

	if err := os.WriteFile(allowPath, []byte("stop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForProofCheckTestProcessExit(t, childPID)
	waitForProofCheckExecutionBarrierClear(t, homePath, holder)
	if _, err := landlease.ReleaseAfter(homePath, holder, now.Add(2*time.Second), func() error {
		finalizerCalled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !finalizerCalled {
		t.Fatal("ReleaseAfter did not finalize after preparation descendant exited")
	}
}

func TestPrepareProofCheckSealedWorkspaceCancellationKillsHelperDescendant(t *testing.T) {
	controlDir := t.TempDir()
	childReadyPath := filepath.Join(controlDir, "child-ready")
	childReleasePath := filepath.Join(controlDir, "child-release")
	childPIDPath := filepath.Join(controlDir, "child-pid")
	survivorPath := filepath.Join(controlDir, "survivor")
	t.Setenv("PROOF_PREP_CHILD_READY", childReadyPath)
	t.Setenv("PROOF_PREP_CHILD_RELEASE", childReleasePath)
	t.Setenv("PROOF_PREP_CHILD_PID", childPIDPath)
	t.Setenv("PROOF_PREP_SURVIVOR", survivorPath)
	proofCheckInstallFakeGit(t, `#!/bin/sh
(
  : > "$PROOF_PREP_CHILD_READY"
  while [ ! -e "$PROOF_PREP_CHILD_RELEASE" ]; do
    sleep 0.01
  done
  : > "$PROOF_PREP_SURVIVOR"
) </dev/null >/dev/null 2>&1 &
echo $! > "$PROOF_PREP_CHILD_PID"
while [ ! -e "$PROOF_PREP_CHILD_READY" ]; do
  sleep 0.01
done
while :; do
  sleep 1
done
`)

	cancel, done, _ := startProofCheckSealedWorkspaceTest(t)
	waitForProofCheckTestPath(t, childReadyPath)
	childPID := waitForProofCheckTestPID(t, childPIDPath)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled preparation returned nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled preparation did not return")
	}
	if err := os.WriteFile(childReleasePath, []byte("release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForProofCheckTestProcessExit(t, childPID)
	if _, err := os.Stat(survivorPath); !os.IsNotExist(err) {
		t.Fatalf("cancelled preparation helper descendant survived: %v", err)
	}
}

func proofCheckSealedIgnoredInputRepo(t *testing.T) (string, gitrepository.Snapshot) {
	t.Helper()
	repo := initCompoundCommandGitRepo(t)
	if err := os.WriteFile(
		filepath.Join(repo, ".gitignore"),
		[]byte("docs_private/\nconsole/node_modules/\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", ".gitignore")
	runCommandGit(t, repo, "commit", "-m", "ignore proof inputs")
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return repo, source
}

func proofCheckInstallFakeGit(t *testing.T, body string) {
	t.Helper()
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func startProofCheckSealedWorkspaceTest(t *testing.T) (context.CancelFunc, <-chan error, <-chan struct{}) {
	t.Helper()
	repo := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	// Registered before launch so readiness failures and early returns cancel
	// and join the owned process group before temporary paths are removed.
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("cleanup could not join owned sealed preparation")
		}
	})
	go func() {
		defer close(finished)
		_, _, err := prepareProofCheckSealedWorkspace(ctx, repo, gitrepository.Snapshot{})
		done <- err
	}()
	return cancel, done, finished
}

func readProofCheckTestPID(path string) (int, bool, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	value := strings.TrimSpace(string(body))
	if value == "" {
		// Redirection creates/truncates the path before echo publishes its PID.
		return 0, false, nil
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		return 0, false, err
	}
	if pid <= 0 {
		return 0, false, errors.New("published PID must be positive")
	}
	return pid, true, nil
}

func waitForProofCheckTestPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		pid, ready, err := readProofCheckTestPID(path)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for published PID at %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func proofCheckTestProcessExited(pid int) (bool, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	defer process.Release()
	err = process.Signal(syscall.Signal(0))
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	return false, err
}

func waitForProofCheckTestProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		exited, err := proofCheckTestProcessExited(pid)
		if err != nil {
			t.Fatal(err)
		}
		if exited {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for process %d to exit", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProofCheckExecutionBarrierClear(t *testing.T, homePath, holder string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, err := landlease.ExecutionPending(homePath, holder)
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for execution barrier %s to clear", holder)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProofCheckTestPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
