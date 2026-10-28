package executewire

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/work"
)

// R6P (2026-09-11): the detached governed door.
//
// LaunchDetachedViaDoor is the production binding for work's
// executeStageDetachedLaunchFn. The seam has already claimed the envelope
// under the tick actor; this function spawns the SAME governed door a human
// dispatches (`compound work advance <id> --to running --by <actor> --execute
// --harness <lane> --permission-mode bypass --timeout <t> --prompt <p>`) as its own process
// group, wrapped in GNU timeout so the run carries a finite budget that
// outlives the heartbeat tick, with stdin closed and stdout/stderr custodied
// in a log beside the RunAttempt launch evidence. It returns as soon as the
// door has started. The door owns claimed -> running -> verifying/failed;
// nothing here mints, advances or finalizes.
//
// Cancellation: `kill -TERM -<pgid>` (the sidecar carries pgid). The door's
// launch context cancels on SIGTERM and terminates the lane's process group,
// so the attempt is finalized failed with cancellation evidence rather than
// orphaned. The timeout wrapper does exactly that at the budget.

// DefaultDetachedLaunchBudget bounds one detached door. It is the budget
// the timeout wrapper enforces; the door itself runs with the shorter
// --timeout from detachedDoorTimeout so its own adapter deadline fires
// first and it finalizes the RunAttempt before the wrapper TERMs it.
var DefaultDetachedLaunchBudget = 45 * time.Minute

// detachedDoorTimeout is the door's own --timeout inside a wrapper budget:
// two minutes of finalize margin, never less than half the budget.
func detachedDoorTimeout(budget time.Duration) time.Duration {
	if inner := budget - 2*time.Minute; inner >= budget/2 {
		return inner
	}
	return budget / 2
}

// DetachedDoorBinary resolves the compound binary the door runs as. Tests
// swap it for a script that records its argv.
var DetachedDoorBinary = func() (string, error) { return os.Executable() }

// DetachedTimeoutBinary resolves the GNU timeout wrapper. A missing wrapper
// is a refusal, not a silent unbounded launch.
var DetachedTimeoutBinary = func() (string, error) {
	for _, name := range []string{"gtimeout", "timeout"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("detached launch: no GNU timeout wrapper (gtimeout/timeout) on PATH; refusing an unbounded door")
}

// DetachedLaunchArgv is the exact command a detached door runs, so tests and
// operators can read it without spawning anything.
func DetachedLaunchArgv(timeoutBin, doorBin string, budget time.Duration, workID, lane, prompt, actor string) []string {
	seconds := int(budget.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return []string{
		timeoutBin, "--signal=TERM", "--kill-after=60", strconv.Itoa(seconds),
		doorBin, "work", "advance", workID,
		"--to", "running", "--by", actor, "--execute",
		"--harness", lane, "--permission-mode", "bypass",
		"--timeout", detachedDoorTimeout(budget).String(),
		"--prompt", prompt,
	}
}

// writeDetachedLaunch is the sidecar write seam (tests inject failures at
// the exact steps the start/record contract cares about).
var writeDetachedLaunch = work.WriteDetachedLaunch

// detachedAfterStartDecision runs between this launcher WINNING the start
// decision and cmd.Start(). Production leaves it nil; it exists so a falsifier
// can drive a real expired-grace reconcile into exactly the window the old
// check-then-act guard left open, without a sleep or a goroutine race.
var detachedAfterStartDecision func()

// These seams put recording failures and a launcher crash at the exact
// post-start boundary. Production keeps the hook nil.
var detachedAfterStart func(*exec.Cmd)
var syncDetachedDoorLog = func(file *os.File) error { return file.Sync() }
var writeDetachedStartedLog = func(file *os.File, line string) error {
	n, err := file.WriteString(line)
	if err != nil {
		return err
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	return syncDetachedDoorLog(file)
}

// LaunchDetachedViaDoor spawns the detached door for an already-claimed
// envelope and records the sidecar. The record protocol is start/record-safe:
//
//  1. a RESERVED sidecar (no pid) carrying a freshly minted launch id is
//     written BEFORE the start — a failure there is a true not-started
//     refusal (no child exists, plain error);
//  2. the START GUARD: a sidecar pre-check, then the OWNERSHIP DECISION for
//     this launch id, taken under a kernel lock that is HELD for the rest of
//     the launch. Nothing is started unless this launch decided `start`, and
//     no recovery can classify the reservation while the lock is held — which
//     is true even if this launcher is suspended mid-function;
//  3. the guarded start; a start failure removes the reserved sidecar
//     best-effort (nothing started);
//  4. after a successful start the pid line is appended to the door log
//     (the log fd predates the start, so it usually survives the same
//     filesystem failure that ate the sidecar); it carries the launch id, so
//     recovery adopts the pid only into the reservation that started it;
//  5. the sidecar is updated to running+pid with one retry. A log or
//     sidecar recording failure returns a *work.DetachedLaunchRecordError carrying the started
//     identity: the caller must treat the door as owning the envelope, never
//     close the work as failed-to-spawn. Matching log evidence can recover
//     the pid; absent evidence leaves the reservation owned and explicitly
//     recovery_required, including a crash immediately after cmd.Start.
func LaunchDetachedViaDoor(store *work.Store, workID, lane, prompt, actor string, now time.Time) (work.DetachedLaunch, error) {
	return LaunchDetachedCandidateViaDoor(store, workID, work.ExecutionRouteCandidate{LaunchProfileID: lane, Harness: lane}, prompt, actor, now)
}

func LaunchDetachedCandidateViaDoor(store *work.Store, workID string, candidate work.ExecutionRouteCandidate, prompt, actor string, now time.Time) (work.DetachedLaunch, error) {
	return launchDetachedCandidateViaDoor(store, workID, candidate, prompt, actor, now, nil)
}

func launchDetachedCandidateViaDoor(store *work.Store, workID string, candidate work.ExecutionRouteCandidate, prompt, actor string, now time.Time, source *envelope.WorkEnvelope) (work.DetachedLaunch, error) {
	var launch work.DetachedLaunch
	if store == nil {
		return launch, errors.New("detached launch: nil work store")
	}
	workID, candidate.LaunchProfileID, actor = strings.TrimSpace(workID), strings.TrimSpace(candidate.LaunchProfileID), strings.TrimSpace(actor)
	if workID == "" || candidate.LaunchProfileID == "" || actor == "" {
		return launch, fmt.Errorf("detached launch: workID=%q candidate.LaunchProfileID=%q actor=%q; all required", workID, candidate.LaunchProfileID, actor)
	}
	wenv, err := store.Get(workID)
	if err != nil {
		return launch, fmt.Errorf("detached launch: load envelope %s: %w", workID, err)
	}
	if candidate.SourceRevision != "" {
		if _, err := resolveExecutionRouteCandidate(store.HomePath(), candidate, now); err != nil {
			return launch, err
		}
		revision := 0
		if wenv.ExecutionSelection != nil {
			revision = wenv.ExecutionSelection.Revision
		}
		if revision != candidate.SelectionRevision {
			return launch, fmt.Errorf("execution selection revision changed before detached handoff")
		}
		if err := validateSavedSelectionTuple(wenv, candidate, now); err != nil {
			return launch, err
		}
	}
	if source != nil && work.ExecutionSourceRevision(wenv) != work.ExecutionSourceRevision(*source) {
		return launch, errors.New("selected work changed before detached handoff")
	}
	if wenv.State != "claimed" && !(source != nil && wenv.State == "verifying") {
		return launch, fmt.Errorf("detached launch: envelope %s is %s, expected claimed (the seam claims before it hands off)", workID, wenv.State)
	}
	if wenv.GitDelivery == nil || strings.TrimSpace(wenv.GitDelivery.TargetWorktreeRoot) == "" {
		return launch, errors.New("workspace_unresolvable:envelope " + workID + " has no git_delivery.target_worktree_root")
	}
	if err := AssertLaunchContextInstalled(); err != nil {
		return launch, err
	}
	timeoutBin, err := DetachedTimeoutBinary()
	if err != nil {
		return launch, err
	}
	doorBin, err := DetachedDoorBinary()
	if err != nil {
		return launch, fmt.Errorf("detached launch: resolve door binary: %w", err)
	}
	home := store.HomePath()
	dir := work.DetachedLaunchDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return launch, fmt.Errorf("detached launch: %w", err)
	}
	logPath := filepath.Join(dir, workID+".door.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return launch, fmt.Errorf("detached launch: open door log: %w", err)
	}
	defer logFile.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return launch, fmt.Errorf("detached launch: %w", err)
	}
	defer devnull.Close()

	budget := DefaultDetachedLaunchBudget
	argv := DetachedCandidateLaunchArgv(timeoutBin, doorBin, budget, workID, candidate, prompt, actor)
	if source != nil {
		// Use the explicit door's ordinary permission derivation, never force
		// unattended bypass. The same fresh authority checks still own launch.
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--permission-mode" {
				argv = append(argv[:i], argv[i+2:]...)
				break
			}
		}
		argv = append(argv, "--expected-work-revision", work.ExecutionSourceRevision(*source), "--keep-verifying-on-failure")
		if source.State == "verifying" {
			argv = append(argv, "--resume-verifying")
		}
	}
	// 1. Durable reserved launch identity BEFORE the process exists. A
	// failure here is a clean not-started refusal: no child, no mutation.
	// The launch id is minted HERE, once: it is what the log marker, the
	// started-pid line, the start guard below and recovery all agree on.
	launchID := work.NewDetachedLaunchID(now)
	reserved := work.DetachedLaunch{
		SchemaVersion: work.DetachedLaunchSchemaVersion,
		WorkID:        workID,
		LaunchID:      launchID,
		Lane:          candidate.LaunchProfileID,
		Actor:         actor,
		Command:       argv,
		LogPath:       logPath,
		BudgetSeconds: int(budget.Seconds()),
		StartedAt:     now.UTC().Format(time.RFC3339),
		Outcome:       work.DetachedOutcomeReserved,
	}
	if source != nil {
		reserved.Explicit = true
		reserved.SourceRevision = work.ExecutionSourceRevision(*source)
		reserved.SourceRunAttemptIDs = append([]string(nil), source.RunAttemptIDs...)
	}
	if err := writeDetachedLaunch(home, reserved); err != nil {
		return launch, fmt.Errorf("detached launch: the reserved sidecar could not be written, so no door was started: %w", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = strings.TrimSpace(wenv.GitDelivery.TargetWorktreeRoot)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+home)
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setProcessGroup(cmd)
	// 2. START GUARD. Every step above can be arbitrarily delayed — a stalled
	// tick, a host sleep/resume — and the reservation grace window only bounds
	// how long recovery WAITS; it never proves this launcher stopped. Two
	// steps, and only the second one decides:
	//
	// 2a. the pre-check reads the sidecar. It is check-then-act, so it proves
	// nothing on its own; it is here to fail fast, and to catch the one thing
	// a per-launch decision cannot — a NEWER reservation that took this
	// workID.
	if err := work.AssertDetachedReservationHeld(home, workID, launchID); err != nil {
		return launch, err
	}
	// 2b. THE DECISION, under a kernel lock this launcher KEEPS HOLDING until
	// the child is started and recorded. It settles "a child starts for this
	// launch id" against "this reservation is classified lost": recovery must
	// take the same lock to classify, and cannot, for as long as this launcher
	// exists — including while it is suspended mid-function, which is the case
	// no timeout could ever get right. The kernel drops the lock the instant
	// this process dies, so nothing has to wait a grace period out either.
	// Nothing below runs unless this launch decided `start`; an undurable
	// decision is equally a refusal (audit 2026-09-19 follow-up).
	decision, ownership, err := work.AcquireDetachedStartOwnership(home, workID, launchID, now)
	if err != nil {
		return launch, err
	}
	// Held to the end of the launch, then released on every path. The lock fd
	// is O_CLOEXEC and is never placed in cmd.ExtraFiles: it stands for THIS
	// launcher's critical section, not the child's lifetime.
	defer func() { _ = ownership.Release() }()
	if decision != work.DetachedDecisionStart {
		return launch, fmt.Errorf("detached launch: the reservation for %s was already %sd for launch %s; refusing to start", workID, decision, launchID)
	}
	if detachedAfterStartDecision != nil {
		detachedAfterStartDecision()
	}
	_, logErr := fmt.Fprintf(logFile, "%s detached door start work=%s lane=%s actor=%s budget=%s argv=%q\n", now.UTC().Format(time.RFC3339), workID, candidate.LaunchProfileID, actor, budget, argv)
	// Per-launch boundary marker written BEFORE the started-pid line
	// (audit 2026-09-16 finding 5). A reconcile that scopes its pid
	// recovery to lines after the most recent marker for this workID
	// cannot adopt a prior launch's stale pid when the sidecar write
	// fails on this launch. The marker carries the workID so a
	// concurrent launch on a different workID (theoretical; the log
	// file is per-workID today but the guard costs nothing) does not
	// bleed across envelopes.
	_, markerErr := fmt.Fprintf(logFile, "%s %s%s %s%s started_at=%s\n", now.UTC().Format(time.RFC3339), work.DetachedDoorLaunchMarker, workID, work.DetachedDoorLaunchIDField, launchID, now.UTC().Format(time.RFC3339Nano))
	logErr = errors.Join(logErr, markerErr)
	// 3. Guarded start. Nothing started means the reserved record goes away
	// best-effort; a stale reserved record would otherwise read as owned.
	if err := cmd.Start(); err != nil {
		_ = os.Remove(filepath.Join(dir, workID+".json"))
		return launch, fmt.Errorf("detached launch: start door: %w", err)
	}
	defer func() { _ = cmd.Process.Release() }()
	if detachedAfterStart != nil {
		detachedAfterStart(cmd)
	}
	pid := cmd.Process.Pid
	launch = reserved
	launch.PID = pid
	launch.PGID = pid // Setpgid makes the child its own group leader.
	launch.Outcome = work.DetachedOutcomeRunning
	// 4. Started-identity evidence in the log, before any sidecar attempt:
	// the log fd is already open, so this line is the recovery authority a
	// reconcile tick adopts when step 5 keeps failing. It carries the launch
	// id, which is what binds the pid to THIS reservation — a prior launch's
	// pid line carries a different id and is never adopted.
	logErr = errors.Join(logErr, writeDetachedStartedLog(logFile, fmt.Sprintf("%s %s%d pgid=%d %s%s\n", now.UTC().Format(time.RFC3339), work.DetachedDoorStartedLogLine, pid, pid, work.DetachedDoorLaunchIDField, launchID)))
	// 5. Record the started identity. One retry; a persistent failure is
	// reported as recovery_required, never as failed-to-spawn.
	var writeErr error
	for attempt := 0; attempt < 2; attempt++ {
		writeErr = writeDetachedLaunch(home, launch)
		if writeErr == nil {
			break
		}
	}
	if logErr != nil || writeErr != nil {
		// The door is running; do not lose its identity. Report loudly and
		// typed so the caller cannot mistake this for a no-start refusal.
		return launch, &work.DetachedLaunchRecordError{Launch: launch, Err: errors.Join(logErr, writeErr)}
	}
	// Do not wait: the bounded door lives on. Release the parent's handle
	// on success and on every returned post-start recording error.
	return launch, nil
}

func DetachedCandidateLaunchArgv(timeoutBin, doorBin string, budget time.Duration, workID string, candidate work.ExecutionRouteCandidate, prompt, actor string) []string {
	argv := DetachedLaunchArgv(timeoutBin, doorBin, budget, workID, candidate.LaunchProfileID, prompt, actor)
	for i := range argv {
		if argv[i] == "--harness" && i+1 < len(argv) {
			argv[i+1] = candidate.Harness
		}
	}
	if candidate.SourceRevision != "" {
		argv = append(argv, "--expected-execution-tuple", ExecutionTupleDigest(candidate), "--expected-selection-revision", strconv.Itoa(candidate.SelectionRevision))
		if candidate.SelectionRevision == 0 {
			argv = append(argv, "--launch-profile", candidate.LaunchProfileID)
		}
	}
	return argv
}
