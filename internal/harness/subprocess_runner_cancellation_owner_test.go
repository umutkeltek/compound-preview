package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// cancelOwnershipWindow bounds the negative assertion in the cancellation
// ownership tests: with the runner's own process-group kill neutralized,
// NO OTHER CANCELLATION PATH may end the child, so the run must still be in
// flight when this window closes. The competing owner (os/exec's ctx
// watchdog) fires within microseconds of ctx.Done(), so a window orders of
// magnitude larger than that separates "the exec watchdog killed the child"
// from "no cancellation path killed the child". It is a liveness bound on a
// MUST-NOT-HAPPEN claim, not a performance measurement of the run.
const cancelOwnershipWindow = 1500 * time.Millisecond

// cancelOwnershipFixtureSeconds is the helper's OWN lifetime bound. These
// tests deliberately neutralize the runner's group kill, so a test that dies
// before its cleanup releases the file would otherwise leave the helper
// alive in its own process group. The helper therefore ends by itself after
// this many seconds even if the release file never appears.
const cancelOwnershipFixtureSeconds = 25

// cancelOwnershipFixture returns the `sh` arguments of a finite helper: it
// records its own pid (the runner's process-group leader) in $1, then waits
// for the release file $2 and exits 0 printing "released". If the release
// file never appears the helper still ends on its own, printing "expired"
// and exiting 1, once its own deadline passes.
//
// The two paths are passed as POSITIONAL parameters (sh -c SCRIPT sh $1 $2)
// and quoted in the script, never interpolated into the script text, so a
// temp path containing a space or a quote cannot alter the script.
//
// The helper starts no background descendants, never escapes the runner's
// process group, and never outlives its own deadline.
func cancelOwnershipFixture(readyPath, releasePath string) []string {
	script := `echo $$ > "$1"
i=0
while [ "$i" -lt ` + strconv.Itoa(cancelOwnershipFixtureSeconds) + ` ]; do
  if [ -f "$2" ]; then
    echo released
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done
echo expired
exit 1`
	return []string{"-c", script, "sh", readyPath, releasePath}
}

// cancelOwnerPaths are the fixture handshake paths inside the test home.
type cancelOwnerPaths struct {
	ready   string
	release string
}

func newCancelOwnerPaths(home string) cancelOwnerPaths {
	return cancelOwnerPaths{
		ready:   filepath.Join(home, "child-ready"),
		release: filepath.Join(home, "child-release"),
	}
}

// waitForFixtureReady blocks until the child recorded its pid and returns
// it. The child's own readiness marker -- not a sleep -- is the handshake,
// so the test never races process start.
func waitForFixtureReady(t *testing.T, readyPath string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(readyPath)
		if err == nil {
			if pid := strings.TrimSpace(string(body)); pid != "" {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture never became ready (no pid recorded in %s)", readyPath)
	return ""
}

// cancelOwnerRun is the result carrier for a run executed in a goroutine.
type cancelOwnerRun struct {
	out RunOutput
	err error
}

// cancelOwnerHarness is a started runner plus the channels that settle it.
type cancelOwnerHarness struct {
	paths    cancelOwnerPaths
	done     chan cancelOwnerRun
	finished chan struct{}
}

// startCancelOwner launches the runner in a goroutine and returns the handle.
//
// The fixture cleanup is registered BEFORE the goroutine starts, so the
// helper is always released and joined even if the test fails during
// startup: the cleanup writes the release file (failing the test if that
// write fails) and joins the goroutine (failing the test if the join does
// not complete). It adds no supervisor and no post-reap signalling: the
// helper ends by itself on its own deadline.
func startCancelOwner(
	t *testing.T,
	ctx context.Context,
	home string,
	harnessName string,
	paths cancelOwnerPaths,
	build buildCmdFn,
	options subprocessRunOptions,
) *cancelOwnerHarness {
	t.Helper()
	store := runattempt.Open(home)

	finished := make(chan struct{})
	done := make(chan cancelOwnerRun, 1)

	t.Cleanup(func() {
		if err := os.WriteFile(paths.release, []byte("release\n"), 0o644); err != nil {
			t.Errorf("release the fixture helper: %v", err)
		}
		select {
		case <-finished:
		case <-time.After(40 * time.Second):
			t.Errorf("the runner goroutine never finished within 40s: the fixture helper may still be alive")
		}
	})

	go func() {
		defer close(finished)
		out, err := runWithSubprocessOptions(ctx, store, harnessName, RunInput{
			WorkID:    "work_" + strings.ReplaceAll(harnessName, "-", "_"),
			Harness:   harnessName,
			Workspace: home,
			Command:   "sh",
			Args:      cancelOwnershipFixture(paths.ready, paths.release),
			Now:       time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		}, build, defaultHeartbeatInterval, options)
		done <- cancelOwnerRun{out: out, err: err}
	}()

	return &cancelOwnerHarness{paths: paths, done: done, finished: finished}
}

// awaitCancelOwnerRun waits for the released run to settle and returns it.
func awaitCancelOwnerRun(t *testing.T, h *cancelOwnerHarness) cancelOwnerRun {
	t.Helper()
	select {
	case res := <-h.done:
		return res
	case <-time.After(20 * time.Second):
		t.Fatalf("the run never completed after the child was released")
	}
	return cancelOwnerRun{}
}

// releaseFixture writes the release file that lets the helper finish.
func releaseFixture(t *testing.T, paths cancelOwnerPaths) {
	t.Helper()
	if err := os.WriteFile(paths.release, []byte("release\n"), 0o644); err != nil {
		t.Fatalf("release the fixture helper: %v", err)
	}
}

// TestRunnerCommandContextCancellationOwner proves the shared runner -- not
// os/exec -- owns cancellation for a command built with the REAL
// exec.CommandContext path.
//
// The adapter builds with exec.CommandContext, so the command carries
// os/exec's default Cancel callback (kill only the DIRECT process). The test
// neutralizes the runner's own process-group kill through the
// killProcessTree injection seam, so ANY death of the child can only come
// from a competing cancellation path. After the context is cancelled the
// runner must still be the one acting (its group-kill path is invoked) while
// the child must still be alive (no other cancellation path killed it). The
// child is then released and must exit 0, proving it really executed to the
// end.
//
// RED before the fix: os/exec's watchdog kills the direct process on
// ctx.Done(), the run ends without the runner's kill, and the "no competing
// cancellation path may kill the child" assertion fails.
func TestRunnerCommandContextCancellationOwner(t *testing.T) {
	home := t.TempDir()
	paths := newCancelOwnerPaths(home)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The adapter's own cancel callback. exec.CommandContext installs a
	// default one that kills only the direct process; an adapter may
	// replace it. Either way it belongs to os/exec's watchdog, which must
	// never run: the runner is the cancellation owner.
	cancelObserved := make(chan struct{}, 1)
	killTreeObserved := make(chan struct{}, 1)

	build := func(bctx context.Context, in RunInput) *exec.Cmd {
		cmd := exec.CommandContext(bctx, "sh", cancelOwnershipFixture(paths.ready, paths.release)...)
		cmd.Cancel = func() error {
			select {
			case cancelObserved <- struct{}{}:
			default:
			}
			return cmd.Process.Kill()
		}
		return cmd
	}

	h := startCancelOwner(t, ctx, home, "cancel-owner", paths, build, subprocessRunOptions{
		killProcessTree: func(*exec.Cmd) {
			select {
			case killTreeObserved <- struct{}{}:
			default:
			}
			// Deliberately no kill: with the runner's group kill
			// neutralized, any death of the child can only come from a
			// competing cancellation path.
		},
	})

	waitForFixtureReady(t, paths.ready)

	// Cancel: the runner's ctx goroutine must take the kill authority.
	cancel()

	select {
	case <-killTreeObserved:
	case <-time.After(20 * time.Second):
		t.Fatalf("the runner never invoked its process-group kill on ctx cancellation")
	}

	// No other cancellation path may end the child while the runner's kill
	// is neutralized.
	select {
	case res := <-h.done:
		t.Fatalf("a competing cancellation owner ended the run (exit=%d err=%v): "+
			"os/exec's ctx watchdog killed the child, so the runner is not the sole owner",
			res.out.ExitCode, res.err)
	case <-time.After(cancelOwnershipWindow):
	}

	select {
	case <-cancelObserved:
		t.Fatalf("os/exec invoked the command's cancel callback: the exec package is a second cancellation owner")
	default:
	}

	// Release the child: it survived the cancellation and must now run to
	// completion on its own.
	releaseFixture(t, paths)
	res := awaitCancelOwnerRun(t, h)
	if res.err != nil {
		t.Fatalf("run error: %v", res.err)
	}
	if res.out.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (the child survived cancellation and exited cleanly)", res.out.ExitCode)
	}
	body, err := os.ReadFile(res.out.TranscriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(body), "released") {
		t.Fatalf("transcript %q must contain the child's own output", string(body))
	}
	// The runner's own cancellation evidence must still be recorded.
	got, gerr := runattempt.Open(home).Get(res.out.RunAttempt.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	var evidence bool
	for _, ref := range got.ProofRefs {
		if strings.HasPrefix(ref, "cancel:context_cancelled") {
			evidence = true
			break
		}
	}
	if !evidence {
		t.Fatalf("RunAttempt.ProofRefs %v must include cancel:context_cancelled evidence", got.ProofRefs)
	}
}

// TestRunnerCommandContextWaitDelayOwnership proves a nonzero WaitDelay
// supplied by an adapter cannot become a second direct-process killer.
//
// The adapter builds with exec.CommandContext but supplies NO cancel
// callback and a nonzero WaitDelay -- the shape an adapter takes when it
// wants the exec package to bound its wait. os/exec arms a ctx watchdog
// for WaitDelay alone: on ctx.Done() that watchdog SIGKILLs the DIRECT
// process after the delay, racing the runner's process-group kill. The
// runner's group kill is neutralized here, so a death of the child can only
// come from that watchdog.
//
// RED before the fix: the WaitDelay watchdog kills the child ~50ms after the
// cancellation and the run ends without the runner's kill.
func TestRunnerCommandContextWaitDelayOwnership(t *testing.T) {
	home := t.TempDir()
	paths := newCancelOwnerPaths(home)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	killTreeObserved := make(chan struct{}, 1)

	build := func(bctx context.Context, in RunInput) *exec.Cmd {
		cmd := exec.CommandContext(bctx, "sh", cancelOwnershipFixture(paths.ready, paths.release)...)
		// This adapter asks os/exec to bound its wait and asks for no
		// cancel callback of its own.
		cmd.Cancel = nil
		cmd.WaitDelay = 50 * time.Millisecond
		return cmd
	}

	h := startCancelOwner(t, ctx, home, "wait-delay-owner", paths, build, subprocessRunOptions{
		killProcessTree: func(*exec.Cmd) {
			select {
			case killTreeObserved <- struct{}{}:
			default:
			}
		},
	})

	waitForFixtureReady(t, paths.ready)

	cancel()

	select {
	case <-killTreeObserved:
	case <-time.After(20 * time.Second):
		t.Fatalf("the runner never invoked its process-group kill on ctx cancellation")
	}

	select {
	case res := <-h.done:
		t.Fatalf("a nonzero WaitDelay supplied a second direct-process killer (exit=%d err=%v): "+
			"os/exec's watchdog killed the child instead of the runner owning cancellation",
			res.out.ExitCode, res.err)
	case <-time.After(cancelOwnershipWindow):
	}

	// Release the child: it survived the cancellation and must now run to
	// completion on its own.
	releaseFixture(t, paths)
	res := awaitCancelOwnerRun(t, h)
	if res.err != nil {
		t.Fatalf("run error: %v", res.err)
	}
	if res.out.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (the child survived cancellation and exited cleanly)", res.out.ExitCode)
	}
}

// TestRunnerCommandContextPreStartCancellation pins the pre-start rejection
// the runner must keep even after it takes ownership of cancellation.
//
// A context that is already cancelled must refuse the launch: no child may
// start, so the child cannot produce its side effect. The runner's own
// ctx.Err() check inside start() refuses first, and os/exec independently
// refuses to start a command whose context is already done -- clearing the
// command's Cancel callback must not weaken either refusal.
func TestRunnerCommandContextPreStartCancellation(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	sideEffect := filepath.Join(home, "child-side-effect")
	// The side-effect path is passed positionally, not interpolated into the
	// script text.
	args := []string{"-c", `touch "$1"; sleep 5`, "sh", sideEffect}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled: the runner must refuse before any child starts.

	build := func(bctx context.Context, in RunInput) *exec.Cmd {
		return exec.CommandContext(bctx, "sh", args...)
	}
	out, err := runWithSubprocess(ctx, store, "pre-start-cancel", RunInput{
		WorkID:    "work_pre_start_cancel",
		Harness:   "pre-start-cancel",
		Workspace: home,
		Command:   "sh",
		Args:      args,
		Now:       time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}, build, defaultHeartbeatInterval)
	if err == nil {
		t.Fatalf("a pre-canceled context must refuse the run, got err=nil")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want a context-canceled refusal", err)
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed", out.RunAttempt.Status)
	}
	if _, statErr := os.Stat(sideEffect); !os.IsNotExist(statErr) {
		t.Fatalf("the child started despite the pre-canceled context (side effect %s exists)", sideEffect)
	}
}
