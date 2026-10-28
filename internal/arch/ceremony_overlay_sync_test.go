package arch

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCeremonyOverlaySyncReplacesStaleCopiesWithIdempotentRelativeSymlinks(t *testing.T) {
	tempRoot := t.TempDir()
	ceremonies := []string{"compound-execute", "compound-land", "compound-verify", "extract-approach"}
	for _, ceremony := range ceremonies {
		mustWriteTestFile(t, filepath.Join(tempRoot, "skills", ceremony, "SKILL.md"), fmt.Sprintf("tracked owner: %s\n", ceremony))
		for _, overlay := range []string{".agents", ".claude"} {
			mustWriteTestFile(t, filepath.Join(tempRoot, overlay, "skills", ceremony, "SKILL.md"), "stale copied body\n")
		}
	}

	script := filepath.Join(repoRoot, "scripts", "sync-ceremony-overlays.sh")
	for run := 1; run <= 2; run++ {
		cmd := exec.Command("sh", script, tempRoot)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sync run %d failed: %v\n%s", run, err, out)
		}
		for _, ceremony := range ceremonies {
			for _, overlay := range []string{".agents", ".claude"} {
				path := filepath.Join(tempRoot, overlay, "skills", ceremony, "SKILL.md")
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("run %d left %s as %s, want symlink", run, path, info.Mode())
				}
				wantTarget := filepath.ToSlash(filepath.Join("..", "..", "..", "skills", ceremony, "SKILL.md"))
				gotTarget, err := os.Readlink(path)
				if err != nil {
					t.Fatal(err)
				}
				if gotTarget != wantTarget {
					t.Fatalf("run %d link target for %s = %q, want %q", run, path, gotTarget, wantTarget)
				}
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				wantBody := fmt.Sprintf("tracked owner: %s\n", ceremony)
				if string(body) != wantBody {
					t.Fatalf("run %d resolved body for %s = %q, want %q", run, path, body, wantBody)
				}
			}
		}
	}
}

func TestCeremonyOverlaySyncIsSafeUnderConcurrentInstallers(t *testing.T) {
	tempRoot := t.TempDir()
	ceremonies := []string{"compound-execute", "compound-land", "compound-verify", "extract-approach"}
	for _, ceremony := range ceremonies {
		mustWriteTestFile(t, filepath.Join(tempRoot, "skills", ceremony, "SKILL.md"), fmt.Sprintf("tracked owner: %s\n", ceremony))
	}

	const workers = 24
	realLN, err := exec.LookPath("ln")
	if err != nil {
		t.Fatal(err)
	}
	barrierDir := filepath.Join(tempRoot, "ln-barrier")
	if err := os.MkdirAll(barrierDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapperDir := filepath.Join(tempRoot, "bin")
	wrapperPath := filepath.Join(wrapperDir, "ln")
	mustWriteTestFile(t, wrapperPath, `#!/bin/sh
set -eu
: > "$SYNC_TEST_BARRIER/ready.$$"
attempts=0
while :; do
  count=$(find "$SYNC_TEST_BARRIER" -name 'ready.*' -type f | wc -l | tr -d ' ')
  if [ "$count" -ge "$SYNC_TEST_WORKERS" ]; then
    break
  fi
  attempts=$((attempts + 1))
  if [ "$attempts" -ge 1000 ]; then
    echo "ln barrier timed out: ready=$count want=$SYNC_TEST_WORKERS" >&2
    exit 97
  fi
  sleep 0.01
done
exec "$SYNC_TEST_REAL_LN" "$@"
`)
	if err := os.Chmod(wrapperPath, 0o755); err != nil {
		t.Fatal(err)
	}

	type result struct {
		worker int
		out    []byte
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	script := filepath.Join(repoRoot, "scripts", "sync-ceremony-overlays.sh")
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			<-start
			cmd := exec.Command("sh", script, tempRoot)
			cmd.Env = append(os.Environ(),
				"PATH="+wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"SYNC_TEST_BARRIER="+barrierDir,
				fmt.Sprintf("SYNC_TEST_WORKERS=%d", workers),
				"SYNC_TEST_REAL_LN="+realLN,
			)
			out, err := cmd.CombinedOutput()
			results <- result{worker: worker, out: out, err: err}
		}(worker)
	}
	close(start)
	var firstFailure *result
	for worker := 0; worker < workers; worker++ {
		got := <-results
		if got.err != nil && firstFailure == nil {
			got := got
			firstFailure = &got
		}
	}
	if firstFailure != nil {
		t.Fatalf("sync worker %d failed: %v\n%s", firstFailure.worker, firstFailure.err, firstFailure.out)
	}

	for _, ceremony := range ceremonies {
		for _, overlay := range []string{".agents", ".claude"} {
			path := filepath.Join(tempRoot, overlay, "skills", ceremony, "SKILL.md")
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("concurrent sync left %s as %s, want symlink", path, info.Mode())
			}
			pending, err := filepath.Glob(path + ".pending.*")
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("concurrent sync left pending links for %s: %v", path, pending)
			}
		}
	}
}

func TestCeremonyOverlaySyncRemovesPendingLinkOnTermination(t *testing.T) {
	tempRoot := t.TempDir()
	for _, ceremony := range []string{"compound-execute", "compound-land", "compound-verify", "extract-approach"} {
		mustWriteTestFile(t, filepath.Join(tempRoot, "skills", ceremony, "SKILL.md"), fmt.Sprintf("tracked owner: %s\n", ceremony))
	}

	wrapperDir := filepath.Join(tempRoot, "bin")
	wrapperPath := filepath.Join(wrapperDir, "mv")
	readyPath := filepath.Join(tempRoot, "mv-ready")
	mustWriteTestFile(t, wrapperPath, `#!/bin/sh
set -eu
: > "$SYNC_TEST_MV_READY"
while :; do
  sleep 1
done
`)
	if err := os.Chmod(wrapperPath, 0o755); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(repoRoot, "scripts", "sync-ceremony-overlays.sh")
	cmd := exec.Command("sh", script, tempRoot)
	cmd.Env = append(os.Environ(),
		"PATH="+wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SYNC_TEST_MV_READY="+readyPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waiting := true
	defer func() {
		if waiting {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for mv boundary:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	waiting = false
	if waitErr == nil {
		t.Fatal("terminated sync unexpectedly succeeded")
	}

	pending, err := filepath.Glob(filepath.Join(tempRoot, ".agents", "skills", "compound-execute", "SKILL.md.pending.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("terminated sync left pending links: %v\n%s", pending, out.String())
	}
}
