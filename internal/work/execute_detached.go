package work

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"compound/internal/runattempt"
)

// R6P (2026-09-11): detached unattended launch.
//
// The synchronous seam runs the lane INSIDE the heartbeat tick, so a job had
// to finish inside the launch wire's 5-minute budget and the tick's own 600 s
// ceiling. Detached mode hands the launch to a separate governed-door process
// (`compound work advance --execute`, the same door every human-dispatched
// lane uses) in its own process group, bounded by its own budget, and returns
// at once. Ownership stays single and durable:
//
//   - the seam CLAIMS the envelope (ready -> claimed) under the tick actor and
//     writes one sidecar record (DetachedLaunch) BEFORE spawning;
//   - the door advances claimed -> running, mints the RunAttempt, runs the
//     lane, and is the ONLY writer of the terminal running -> verifying/failed
//     transition (FinalizeGovernedHarnessRun) — the seam never finalizes;
//   - later ticks reconcile: a door that exited after finalizing is recorded
//     as door_finalized; a door that died while the envelope was still
//     claimed (never reached running) is recorded as door_lost and the
//     envelope closed failed (one bad launch fails that envelope only); a
//     door that died while running is left to the RunAttempt sweep, which
//     already owns stale-attempt recovery, and is reported as door_lost_running.
//
// Why a second tick cannot double-launch: the seam only ever picks `ready`
// candidates, and the envelope leaves `ready` under the lifecycle lock at
// claim time, before the door exists. Why a crash cannot double-finalize: the
// seam has no finalize path in detached mode, and the door's finalize is the
// existing CAS-guarded one.

// DetachedLaunchSchemaVersion is the sidecar record schema.
const DetachedLaunchSchemaVersion = "compound.detached_launch.v0.1"

// Detached-launch outcomes. `reserved` is written BEFORE the door process
// starts (the durable launch identity); `running` after a successful start;
// the rest are stamped by ReconcileDetachedLaunches.
const (
	DetachedOutcomeReserved        = "reserved"
	DetachedOutcomeRunning         = "running"
	DetachedOutcomeDoorFinalized   = "door_finalized"
	DetachedOutcomeDoorLost        = "door_lost"
	DetachedOutcomeDoorLostRunning = "door_lost_running"
	// RecoveryRequired is a verdict, not a terminal sidecar state. The
	// reservation remains owned until positive evidence settles it.
	DetachedOutcomeRecoveryRequired = "recovery_required"
)

// DetachedDoorStartedLogLine is the log prefix the launcher appends AFTER
// cmd.Start succeeded, carrying the pid/pgid. It is the recovery evidence a
// reconcile tick adopts when the sidecar write failed after start: the log
// file handle was opened before the start, so the pid line usually survives
// the same filesystem failure that ate the sidecar.
const DetachedDoorStartedLogLine = "detached door started pid="

// DetachedDoorLaunchMarker is the per-launch boundary the launcher writes
// to the log BEFORE the started-pid line. A new launcher appends one
// before each start. It was the whole binding in the 2026-09-16 repair —
// recovery scoped its pid read to lines after the most recent marker — and
// it is still the binding for a sidecar written before launch ids existed.
// Marker order alone cannot say WHICH launch a pid belongs to, so a marker
// now also carries DetachedDoorLaunchIDField.
const DetachedDoorLaunchMarker = "detached door launch marker work="

// DetachedDoorLaunchIDField is the log field that both the launch marker and
// the started-pid line carry: the ONE identity minted when the reservation is
// written. The marker alone only ever said "some launch of this workID
// started here", which is the wrong launch whenever a reconcile lands between
// a new reservation and its launcher's first log line — so recovery binds a
// pid to this id instead (audit 2026-09-19).
const DetachedDoorLaunchIDField = "launch="

// NewDetachedLaunchID mints the identity one detached launch carries from
// reservation through start to recovery. It must be unique per launch and a
// single whitespace-free log field: it is read back out of a log line by
// position-independent field match.
func NewDetachedLaunchID(now time.Time) string {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// A clock-only id still separates two launches of the same work:
		// the reservation is written before the start, never concurrently.
		return fmt.Sprintf("dl_%d", now.UTC().UnixNano())
	}
	return fmt.Sprintf("dl_%d_%x", now.UTC().UnixNano(), suffix[:])
}

// DetachedReservationGrace bounds how long recovery waits before checking
// launch ownership. Expiry never proves that no child started; a held kernel
// lock still excludes recovery and an unrecorded start remains uncertain.
const DetachedReservationGrace = 5 * time.Minute

// DetachedLaunchRecordError reports a started door whose log or sidecar
// recording failed. The caller must never treat it as a no-start refusal.
// The returned identity is observable now; recovery needs matching durable
// evidence, otherwise the reservation remains owned and recovery_required.
type DetachedLaunchRecordError struct {
	Launch DetachedLaunch
	Err    error
}

func (e *DetachedLaunchRecordError) Error() string {
	return fmt.Sprintf("detached launch: door pid %d started but start evidence could not be durably recorded: %v", e.Launch.PID, e.Err)
}

func (e *DetachedLaunchRecordError) Unwrap() error { return e.Err }

// DetachedLaunch is the durable sidecar the seam writes for one detached
// launch. It is the runtime identity a human needs to observe or cancel the
// run without the heartbeat: pid, process group, log path, exact argv.
type DetachedLaunch struct {
	// Explicit continuations retain their source generation. A pre-existing
	// verifying state cannot prove that this new door ever executed.
	Explicit            bool     `json:"explicit,omitempty"`
	SourceRevision      string   `json:"source_revision,omitempty"`
	SourceRunAttemptIDs []string `json:"source_run_attempt_ids,omitempty"`
	SchemaVersion       string   `json:"schema_version"`
	WorkID              string   `json:"work_id"`
	// LaunchID is the identity minted with the reservation and stamped into
	// the door log, so a start, a recovery and the launcher's own start guard
	// all name the same launch. Empty on sidecars written before 2026-09-19:
	// those keep marker-scoped recovery.
	LaunchID      string   `json:"launch_id,omitempty"`
	Lane          string   `json:"lane"`
	Actor         string   `json:"actor"`
	PID           int      `json:"pid"`
	PGID          int      `json:"pgid"`
	Command       []string `json:"command"`
	LogPath       string   `json:"log_path"`
	BudgetSeconds int      `json:"budget_seconds,omitempty"`
	StartedAt     string   `json:"started_at"`
	// Stamped by reconciliation; empty while the door is alive.
	ExitObservedAt string `json:"exit_observed_at,omitempty"`
	Outcome        string `json:"outcome,omitempty"`
	OutcomeDetail  string `json:"outcome_detail,omitempty"`
}

// DetachedReconcile is one reconciliation verdict, surfaced on the tick.
type DetachedReconcile struct {
	WorkID  string `json:"work_id"`
	PID     int    `json:"pid"`
	Outcome string `json:"outcome"`
	State   string `json:"state,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// executeDetachedLaunchFn spawns the detached door for an already-claimed
// envelope and returns the sidecar identity. nil keeps the synchronous seam.
type executeDetachedLaunchFn func(store *Store, workID, lane, prompt, actor string, now time.Time) (DetachedLaunch, error)
type executeDetachedCandidateLaunchFn func(store *Store, workID string, candidate ExecutionRouteCandidate, prompt, actor string, now time.Time) (DetachedLaunch, error)

var executeStageDetachedLaunchFn executeDetachedLaunchFn
var executeStageDetachedCandidateLaunchFn executeDetachedCandidateLaunchFn

// SwapExecuteStageDetachedLaunch installs fn as the seam's detached launcher
// (nil restores the synchronous launch) and returns a restore function.
func SwapExecuteStageDetachedLaunch(fn executeDetachedLaunchFn) func() {
	prev, prevCandidate := executeStageDetachedLaunchFn, executeStageDetachedCandidateLaunchFn
	executeStageDetachedLaunchFn = fn
	executeStageDetachedCandidateLaunchFn = nil
	return func() { executeStageDetachedLaunchFn = prev; executeStageDetachedCandidateLaunchFn = prevCandidate }
}

func SwapExecuteStageDetachedCandidateLaunch(fn executeDetachedCandidateLaunchFn) func() {
	prev := executeStageDetachedCandidateLaunchFn
	executeStageDetachedCandidateLaunchFn = fn
	return func() { executeStageDetachedCandidateLaunchFn = prev }
}

// DetachedLaunchDir is where the seam custodies detached-launch sidecars.
// It is a sibling of the RunAttempt launch-lifecycle evidence directory
// (`run_attempts/launch/`), never inside it: the attempt reader walks that
// directory for its own content-free launch events and treats a foreign
// file there as unreadable evidence.
func DetachedLaunchDir(home string) string {
	return filepath.Join(home, "run_attempts", "detached")
}

func detachedLaunchPath(home, workID string) string {
	return filepath.Join(DetachedLaunchDir(home), workID+".json")
}

// WriteDetachedLaunch syncs a complete temporary sidecar before rename, then
// syncs its directory. A failed durability step is reported to the existing
// launch/recovery owner, even if rename already made the record visible.
func WriteDetachedLaunch(home string, launch DetachedLaunch) error {
	if strings.TrimSpace(launch.WorkID) == "" {
		return errors.New("detached launch: work_id is required")
	}
	if launch.SchemaVersion == "" {
		launch.SchemaVersion = DetachedLaunchSchemaVersion
	}
	dir := DetachedLaunchDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(launch, "", "  ")
	if err != nil {
		return err
	}
	tmp := detachedLaunchPath(home, launch.WorkID) + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	data := append(body, '\n')
	n, writeErr := file.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = syncDetachedLaunchRecord(file)
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp, detachedLaunchPath(home, launch.WorkID)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(syncDetachedLaunchRecord(directory), directory.Close())
}

// The file and directory durability boundaries are independently falsifiable.
var syncDetachedLaunchRecord = func(file *os.File) error { return file.Sync() }

// ReadDetachedLaunch loads one sidecar.
func ReadDetachedLaunch(home, workID string) (DetachedLaunch, error) {
	var launch DetachedLaunch
	body, err := os.ReadFile(detachedLaunchPath(home, workID))
	if err != nil {
		return launch, err
	}
	if err := json.Unmarshal(body, &launch); err != nil {
		return launch, fmt.Errorf("detached launch %s: %w", workID, err)
	}
	return launch, nil
}

// ListDetachedLaunches returns every sidecar, sorted by work id.
func ListDetachedLaunches(home string) ([]DetachedLaunch, error) {
	entries, err := os.ReadDir(DetachedLaunchDir(home))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []DetachedLaunch
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		launch, err := ReadDetachedLaunch(home, strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		out = append(out, launch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkID < out[j].WorkID })
	return out, nil
}

// detachedProcessAlive answers whether pid still exists. EPERM means it
// exists under another user, which for our purposes is "alive".
var detachedProcessAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// detachedDoorStartedPID recovers the (pid, pgid) a launcher logged after a
// successful start, from the door log. ok=false when the log carries no
// started line (the start never happened, or the log write failed too).
//
// The log is opened with O_APPEND, so a stale "started pid=" line from a
// prior launch can survive. launchID is the reservation's own identity and is
// the exact binding (audit 2026-09-19): when it is set, only a started-pid
// line stamped with that same id may be adopted, so a previous launch's pid
// cannot land in a new reservation no matter how the two interleave — and a
// reservation whose launcher has not logged anything yet simply recovers
// nothing and stays inside its grace window.
//
// An empty launchID is a sidecar written before the id existed. Those keep the
// 2026-09-16 behaviour: scope to the most recent per-launch boundary marker
// and ignore any "started pid=" line that precedes it. The log may by then be
// written by a newer launcher, so the id on those lines is not required to
// match anything — the marker is still the only boundary such a sidecar has.
func detachedDoorStartedPID(logPath, launchID string) (int, int, bool) {
	body, err := os.ReadFile(logPath)
	if err != nil {
		return 0, 0, false
	}
	launchID = strings.TrimSpace(launchID)
	scoped := string(body)
	if launchID == "" {
		scoped = scopeToLastLaunchMarker(scoped)
	}
	for _, line := range strings.Split(scoped, "\n") {
		idx := strings.Index(line, DetachedDoorStartedLogLine)
		if idx < 0 {
			continue
		}
		fields := strings.Fields(line[idx+len(DetachedDoorStartedLogLine):])
		if len(fields) == 0 {
			continue
		}
		if launchID != "" && detachedLogLaunchID(fields) != launchID {
			continue // another launch's pid; this reservation does not own it
		}
		pid, perr := strconv.Atoi(fields[0])
		if perr != nil || pid <= 0 {
			continue
		}
		pgid := pid
		if len(fields) >= 3 && fields[1] == "pgid" {
			if g, gerr := strconv.Atoi(fields[2]); gerr == nil && g > 0 {
				pgid = g
			}
		}
		return pid, pgid, true
	}
	return 0, 0, false
}

// detachedLogLaunchID reads the launch= field off the tail of a door-log line.
// The field is position-independent so the line's older fields keep their
// meaning; it is absent on lines written before 2026-09-19, which read as "no
// identity" and therefore match no reservation that has one.
func detachedLogLaunchID(fields []string) string {
	for _, f := range fields {
		if strings.HasPrefix(f, DetachedDoorLaunchIDField) {
			return strings.TrimPrefix(f, DetachedDoorLaunchIDField)
		}
	}
	return ""
}

// AssertDetachedReservationHeld reports whether launchID still holds workID's
// reservation on disk. It is the launcher's cheap PRE-CHECK, not the ownership
// decision: reading the sidecar is check-then-act, so on its own it only
// narrows the window in which a delayed launcher starts a child for a
// reservation recovery has already taken. Its job is to catch what the
// per-launch decision below cannot — a NEWER reservation that took this
// workID — and to fail fast before the decision is spent. The binding call is
// AcquireDetachedStartOwnership. A reservation with no id (written before
// 2026-09-19) is held as long as it still reads reserved.
func AssertDetachedReservationHeld(home, workID, launchID string) error {
	held, err := ReadDetachedLaunch(home, workID)
	if err != nil {
		return fmt.Errorf("detached launch: the reservation for %s is gone or unreadable: %w", workID, err)
	}
	if held.Outcome != DetachedOutcomeReserved {
		return fmt.Errorf("detached launch: the reservation for %s is now %q — recovery or another launcher owns it", workID, held.Outcome)
	}
	if launchID != "" && held.LaunchID != launchID {
		return fmt.Errorf("detached launch: the reservation for %s now belongs to launch %q, not %q", workID, held.LaunchID, launchID)
	}
	return nil
}

// THE OWNERSHIP INVARIANT (audit 2026-09-19, follow-up).
//
// For one launch id, "a child was started" and "this reservation was
// classified lost" are MUTUALLY EXCLUSIVE. Exactly one side wins, the win is
// decided by a single atomic act, and the loser observes the winner's decision
// rather than a stale read of its own. Elapsed time decides nothing: a
// launcher can be descheduled at any instruction (a host sleep/resume does
// exactly that), so any rule of the form "it has been N minutes, therefore the
// launcher is gone" is a guess, and a guess that starts an orphan child or
// fails a live envelope.
//
// The decision lives in one file per launch id, and it is guarded by a KERNEL
// LOCK the launcher HOLDS across cmd.Start() (syscall.Flock, this repo's
// existing advisory-lock idiom — internal/candidates/read.go, internal/home).
// A held flock is the one fact about another process that needs no clock:
//
//   - it stays held while the holder is merely suspended — a host sleep/resume,
//     a SIGSTOP, a wedged disk — which is exactly the case where elapsed time
//     lies and a timeout would wrongly declare the launcher gone;
//   - it is released BY THE KERNEL the moment the holder dies, so a dead
//     launcher frees its launch id immediately, with no grace period to wait
//     out and nothing to guess.
//
// So recovery never asks "has it been long enough". It asks the kernel whether
// the launcher is still inside its critical section, and reads the decision
// under the same lock. There is deliberately no time-based override: an
// override of a held lock is the bug this replaced, not a safety net.
//
// LEGACY: reservations without launch ids still accept marker-scoped log
// recovery. Without a pid they remain uncertain: no ownership token exists
// that can prove a delayed launcher will never start.
const (
	// DetachedDecisionStart is the launcher's claim: this launch will start
	// a child, so its reservation must never be classified lost.
	DetachedDecisionStart = "start"
	// DetachedDecisionRevoke is recovery's claim: this reservation is being
	// classified lost, so its launcher must never start a child.
	DetachedDecisionRevoke = "revoke"
)

// ErrDetachedLaunchInProgress is recovery's answer when the launcher still
// holds its launch id's ownership lock. It is not a failure: it is the kernel
// stating that the launcher is inside its critical section, which is the one
// question elapsed time could never answer.
var ErrDetachedLaunchInProgress = errors.New("detached ownership: the launcher holds the ownership lock")

// detachedOwnershipToken is the durable content of one decision, read and
// written under the lock. An empty file is an undecided launch id.
type detachedOwnershipToken struct {
	LaunchID  string `json:"launch_id"`
	WorkID    string `json:"work_id"`
	Decision  string `json:"decision"`
	DecidedAt string `json:"decided_at"`
}

// detachedOwnershipOpenFn opens a launch id's ownership file. Both sides open
// it the same way — the lock, not the open, decides. It is a var so a
// falsifier can prove the undurable-revoke path: a revoke that cannot be made
// durable must never classify a reservation lost.
//
// os.OpenFile sets O_CLOEXEC, so the door child never inherits this
// descriptor: the lock stands for the LAUNCHER's critical section, and the
// launcher must not keep it alive through the child. It is likewise never
// passed in cmd.ExtraFiles.
var detachedOwnershipOpenFn = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}

// DetachedOwnershipLock is a HELD ownership decision. While it is open the
// kernel says this launcher is still inside its critical section; the launcher
// releases it only after the child is started and recorded.
type DetachedOwnershipLock struct{ file *os.File }

// Release drops the kernel lock by closing the descriptor that holds it. Safe
// on a nil lock, which is what a legacy id-less launch carries.
func (l *DetachedOwnershipLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

// reconcileWriteDetachedLaunch is the reconciler's sidecar writer, a var for
// the same reason: a classification whose sidecar stamp cannot be made durable
// must not close an envelope.
var reconcileWriteDetachedLaunch = WriteDetachedLaunch

// detachedOwnershipTokenPath names the decision for one launch id. The suffix
// is deliberately not .json: ListDetachedLaunches reads only *.json entries in
// this directory, so a token can never be misread as a sidecar, and nothing
// else in the tree inventories this directory's contents file by file.
func detachedOwnershipTokenPath(home, launchID string) string {
	return filepath.Join(DetachedLaunchDir(home), launchID+".owner")
}

// AcquireDetachedStartOwnership is the launcher's decision point. It BLOCKS
// for the launch id's ownership lock, reads the decision under it, and returns
// the lock still HELD so the caller can keep holding it across cmd.Start().
// The caller starts nothing unless the returned decision is
// DetachedDecisionStart, and Releases only after the child is started and
// recorded — while that lock is held, no recovery can classify this launch.
//
// An error is a refusal: a decision that could not be taken or recorded is one
// both sides could still believe they won.
//
// An empty launchID is a legacy reservation: there is no lock to take, so the
// launcher's own intent is returned with a nil lock and the pre-2026-09-19
// behaviour stands.
func AcquireDetachedStartOwnership(home, workID, launchID string, now time.Time) (string, *DetachedOwnershipLock, error) {
	launchID = strings.TrimSpace(launchID)
	if launchID == "" {
		return DetachedDecisionStart, nil, nil
	}
	file, err := openDetachedOwnershipFile(home, launchID)
	if err != nil {
		return "", nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return "", nil, fmt.Errorf("detached ownership: lock %s: %w", launchID, err)
	}
	decision, err := readDetachedOwnershipDecision(file)
	if err != nil {
		_ = file.Close()
		return "", nil, err
	}
	if decision != "" {
		// Recovery already revoked (or, impossibly, this id decided twice).
		// Either way the launcher does not own the launch: drop the lock.
		_ = file.Close()
		return decision, nil, nil
	}
	if err := writeDetachedOwnershipDecision(file, workID, launchID, DetachedDecisionStart, now); err != nil {
		_ = file.Close()
		return "", nil, err
	}
	return DetachedDecisionStart, &DetachedOwnershipLock{file: file}, nil
}

// TakeDetachedRevokeOwnership is recovery's decision point. It takes the same
// lock NON-BLOCKINGLY, so a launcher still inside its critical section is
// reported as ErrDetachedLaunchInProgress rather than overridden — that
// refusal is the whole point, and it costs no waiting because the kernel frees
// the lock the instant a dead launcher's process exits.
//
// With the lock taken, the file's content is the settled decision:
//
//   - empty: nobody has decided, so recovery decides — the revoke is written
//     and fsynced before the caller stamps or closes anything;
//   - start: the launcher has left its critical section, but a crash just
//     after Start or failed recording may hide a live child. Missing pid
//     evidence leaves recovery_required, never proof of a side-effect-free run;
//   - revoke: an earlier tick already decided; idempotent.
//
// The lock is released before the caller stamps the sidecar and closes the
// envelope: the decision is durable in the file by then, and the file's
// content — not the lock — is what a launcher reads.
func TakeDetachedRevokeOwnership(home, workID, launchID string, now time.Time) (string, error) {
	launchID = strings.TrimSpace(launchID)
	if launchID == "" {
		return DetachedDecisionRevoke, nil
	}
	file, err := openDetachedOwnershipFile(home, launchID)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return "", ErrDetachedLaunchInProgress
		}
		return "", fmt.Errorf("detached ownership: lock %s: %w", launchID, err)
	}
	decision, err := readDetachedOwnershipDecision(file)
	if err != nil {
		return "", err
	}
	if decision != "" {
		return decision, nil
	}
	if err := writeDetachedOwnershipDecision(file, workID, launchID, DetachedDecisionRevoke, now); err != nil {
		return "", err
	}
	return DetachedDecisionRevoke, nil
}

func openDetachedOwnershipFile(home, launchID string) (*os.File, error) {
	if err := os.MkdirAll(DetachedLaunchDir(home), 0o755); err != nil {
		return nil, fmt.Errorf("detached ownership: %s: %w", launchID, err)
	}
	file, err := detachedOwnershipOpenFn(detachedOwnershipTokenPath(home, launchID))
	if err != nil {
		return nil, fmt.Errorf("detached ownership: %s could not be opened: %w", launchID, err)
	}
	return file, nil
}

// readDetachedOwnershipDecision reads the decision under a held lock. Empty
// means undecided; a partial record (a writer that died mid-write) is an
// error, never "undecided", so neither side decides over a torn one.
func readDetachedOwnershipDecision(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("detached ownership: rewind: %w", err)
	}
	body, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("detached ownership: read: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return "", nil
	}
	var token detachedOwnershipToken
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("detached ownership: unreadable decision record: %w", err)
	}
	if strings.TrimSpace(token.Decision) == "" {
		return "", errors.New("detached ownership: decision record carries no decision")
	}
	return token.Decision, nil
}

func writeDetachedOwnershipDecision(file *os.File, workID, launchID, decision string, now time.Time) error {
	body, err := json.MarshalIndent(detachedOwnershipToken{
		LaunchID:  launchID,
		WorkID:    workID,
		Decision:  decision,
		DecidedAt: now.UTC().Format(time.RFC3339Nano),
	}, "", "  ")
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("detached ownership: %s could not record %s: %w", launchID, decision, err)
	}
	if _, err := file.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("detached ownership: %s could not record %s: %w", launchID, decision, err)
	}
	// fsync before the lock is dropped: the next holder must find the
	// decision, not an empty file it would decide over.
	if err := file.Sync(); err != nil {
		return fmt.Errorf("detached ownership: %s could not record %s durably: %w", launchID, decision, err)
	}
	return nil
}

// scopeToLastLaunchMarker returns the slice of logBody starting at the
// last "detached door launch marker work=<id>" line. Older content
// (including a prior launch's started pid= line) is excluded so a
// failed-sidecar reconcile cannot adopt a stale identity (audit
// 2026-09-16 finding 5). Returns body unchanged when no marker is
// present, so a log without the new marker still recovers a pid when
// the very first launch ever appended one.
func scopeToLastLaunchMarker(logBody string) string {
	idx := strings.LastIndex(logBody, DetachedDoorLaunchMarker)
	if idx < 0 {
		return logBody
	}
	if nl := strings.LastIndex(logBody[:idx], "\n"); nl >= 0 {
		idx = nl + 1
	}
	return logBody[idx:]
}

// reservationInGrace reports whether a reserved sidecar's elapsed time
// since reservation is still inside the launcher grace window. A
// missing StartedAt is treated as in-grace so a malformed sidecar
// never races its own launcher; the operator inspects the sidecar
// before the next reconcile.
func reservationInGrace(launch DetachedLaunch, now time.Time) bool {
	started, err := time.Parse(time.RFC3339, strings.TrimSpace(launch.StartedAt))
	if err != nil {
		return true
	}
	return now.Sub(started) <= DetachedReservationGrace
}

// ReconcileDetachedLaunches walks every open sidecar and records what became
// of its door. It runs at the top of each unattended tick so the lifecycle of
// a launch handed off on tick N is visible on tick N+1 without a human
// reading a log. It mutates an envelope in exactly one case: the door died
// before it ever advanced the envelope to running, which leaves a claimed
// envelope nobody will finish — that envelope is closed failed (audit_only)
// so the bench stays honest and the next tick moves on.
//
// A `reserved` sidecar (written before the start, whose post-start update
// failed) is adopted here: the reconcile reads the started pid line from the
// door log, re-stamps the sidecar, and treats the door as running. A
// reserved record with NO started line is ambiguous once its launcher decided
// start. The child may still be alive. Recovery reports recovery_required and
// retains ownership across restarts rather than closing or redispatching it.
func (s *Store) ReconcileDetachedLaunches(now time.Time) []DetachedReconcile {
	launches, err := ListDetachedLaunches(s.home)
	if err != nil || len(launches) == 0 {
		return nil
	}
	return s.reconcileDetachedLaunches(launches, now)
}

// ReconcileDetachedLaunch never scans or changes unrelated work.
func (s *Store) ReconcileDetachedLaunch(workID string, now time.Time) []DetachedReconcile {
	launch, err := ReadDetachedLaunch(s.home, workID)
	if err != nil {
		return nil
	}
	return s.reconcileDetachedLaunches([]DetachedLaunch{launch}, now)
}

func (s *Store) reconcileDetachedLaunches(launches []DetachedLaunch, now time.Time) []DetachedReconcile {
	var out []DetachedReconcile
	for _, launch := range launches {
		if launch.Outcome == DetachedOutcomeDoorFinalized || launch.Outcome == DetachedOutcomeDoorLost || launch.Outcome == DetachedOutcomeDoorLostRunning {
			continue // already reconciled
		}
		verdict := DetachedReconcile{WorkID: launch.WorkID, PID: launch.PID}
		if launch.Outcome == DetachedOutcomeReserved && launch.PID <= 0 {
			pid, pgid, ok := detachedDoorStartedPID(launch.LogPath, launch.LaunchID)
			if ok {
				launch.PID, launch.PGID = pid, pgid
				launch.Outcome = DetachedOutcomeRunning
				verdict.PID = pid
				if writeErr := reconcileWriteDetachedLaunch(s.home, launch); writeErr != nil {
					verdict.Detail = "adopted started pid from the door log but the sidecar still cannot be written: " + writeErr.Error()
				}
			}
		}
		w, loadErr := s.Get(launch.WorkID)
		if loadErr == nil {
			verdict.State = w.State
		}
		// Give an ordinary launcher time to finish recording before checking
		// its durable decision. Expiry is only a reason to inspect ownership;
		// it proves neither that the launcher stopped nor that no child exists.
		if launch.Outcome == DetachedOutcomeReserved && launch.PID <= 0 && reservationInGrace(launch, now) {
			verdict.Outcome = DetachedOutcomeReserved
			verdict.Detail = fmt.Sprintf("reserved sidecar within grace (%s); launcher in flight, not yet classified lost", DetachedReservationGrace)
			out = append(out, verdict)
			continue
		}
		if detachedProcessAlive(launch.PID) {
			verdict.Outcome = DetachedOutcomeRunning
			verdict.Detail = fmt.Sprintf("door pid %d alive; envelope %s", launch.PID, verdict.State)
			out = append(out, verdict)
			continue
		}
		// THE DECISION. Recovery may revoke an undecided launch only while
		// holding its ownership lock. A held lock excludes recovery; a durable
		// start decision without a pid remains uncertain after the lock is free.
		if launch.Outcome == DetachedOutcomeReserved && launch.PID <= 0 {
			if launch.LaunchID == "" {
				verdict.Outcome = DetachedOutcomeRecoveryRequired
				verdict.Detail = "legacy reservation has no launch ownership identity or started pid; process start is uncertain, reservation retained"
				out = append(out, verdict)
				continue
			}
			decision, decErr := TakeDetachedRevokeOwnership(s.home, launch.WorkID, launch.LaunchID, now)
			switch {
			case errors.Is(decErr, ErrDetachedLaunchInProgress):
				// The kernel says the launcher is still inside its critical
				// section. That is true through a suspend and false the
				// instant the process dies, so it is the whole answer — there
				// is no elapsed time that would justify overriding it.
				verdict.Outcome = DetachedOutcomeReserved
				verdict.Detail = fmt.Sprintf("launch %s holds the ownership lock; its launcher is inside its critical section", launch.LaunchID)
				out = append(out, verdict)
				continue
			case decErr != nil:
				verdict.Outcome = DetachedOutcomeReserved
				verdict.Detail = "not classified: the revoke could not be made durable (" + decErr.Error() + "); retrying next tick"
				out = append(out, verdict)
				continue
			case decision != DetachedDecisionRevoke:
				// The free lock only ends the launcher's critical section. Death
				// before Start, death after Start, and failed recording are
				// indistinguishable without a pid. Retain the owned reservation.
				verdict.Outcome = DetachedOutcomeRecoveryRequired
				verdict.Detail = fmt.Sprintf("launch %s has decision %q and no matching started pid after its ownership lock was released; process start is uncertain, reservation retained", launch.LaunchID, decision)
				out = append(out, verdict)
				continue
			}
		}
		launch.ExitObservedAt = now.UTC().Format(time.RFC3339)
		switch {
		case loadErr != nil:
			launch.Outcome = DetachedOutcomeDoorLost
			launch.OutcomeDetail = "envelope unreadable: " + loadErr.Error()
		case launch.Explicit && w.State != "running" && !s.detachedHasNewTerminalAttempt(launch, w.RunAttemptIDs):
			launch.Outcome = DetachedOutcomeDoorLost
			launch.OutcomeDetail = "explicit door exited without a new linked terminal attempt; source work retained, no execution success inferred"
		case w.State == "verifying" || w.State == "completed" || w.State == "failed" || w.State == "cancelled":
			launch.Outcome = DetachedOutcomeDoorFinalized
			launch.OutcomeDetail = "door exited after finalizing; envelope " + w.State
		case w.State == "running":
			// The door minted an attempt and died before finalizing. The
			// RunAttempt sweep owns stale-attempt recovery; the seam only
			// names the loss so the operator can run the exact recovery.
			launch.Outcome = DetachedOutcomeDoorLostRunning
			launch.OutcomeDetail = fmt.Sprintf("door pid %d exited while envelope still running; attempts=%v — recover with the run-attempt sweep for the exact attempt", launch.PID, w.RunAttemptIDs)
		default:
			// claimed (or any pre-running state): the door never took the
			// envelope. Close it failed so the bench is not blocked by a
			// claim nobody will finish. Same shape as a synchronous launch
			// failure (one bad launch fails that envelope only).
			launch.Outcome = DetachedOutcomeDoorLost
			launch.OutcomeDetail += fmt.Sprintf("door pid %d exited while envelope still %s (never reached running); log %s", launch.PID, w.State, launch.LogPath)
			// Revoke DURABLY, then fail. The decision above already took
			// ownership, but the sidecar is what every other reader — the
			// launcher's own pre-check included — consults, so it must carry
			// the classification BEFORE the envelope is closed: a failed
			// envelope behind a sidecar that still reads `reserved` is the
			// same hole read from the other end. An undurable stamp is
			// reported, never discarded, and the envelope is left claimed for
			// the next tick to retry.
			if writeErr := reconcileWriteDetachedLaunch(s.home, launch); writeErr != nil {
				verdict.Outcome = DetachedOutcomeReserved
				verdict.Detail = "not classified: the revoke is durable but the sidecar stamp is not (" + writeErr.Error() + "); envelope left claimed, retrying next tick"
				out = append(out, verdict)
				continue
			}
			if closeErr := s.failClaimedUnattendedEnvelope(launch.WorkID, launch.Actor, "unattended detached launch lost: "+launch.OutcomeDetail, "execute_stage_detached_door_lost", now); closeErr != nil {
				launch.OutcomeDetail += "; close refused: " + closeErr.Error()
			} else {
				verdict.State = "failed"
			}
		}
		if writeErr := reconcileWriteDetachedLaunch(s.home, launch); writeErr != nil {
			launch.OutcomeDetail += "; sidecar stamp failed: " + writeErr.Error()
		}
		verdict.Outcome = launch.Outcome
		verdict.Detail = launch.OutcomeDetail
		out = append(out, verdict)
	}
	return out
}

func (s *Store) detachedHasNewTerminalAttempt(launch DetachedLaunch, ids []string) bool {
	previous := make(map[string]bool, len(launch.SourceRunAttemptIDs))
	for _, id := range launch.SourceRunAttemptIDs {
		previous[id] = true
	}
	for _, id := range ids {
		if previous[id] {
			continue
		}
		a, err := runattempt.Open(s.home).Get(id)
		if err == nil && a.WorkID == launch.WorkID && (a.Status == runattempt.StatusSucceeded || a.Status == runattempt.StatusFailed || a.Status == runattempt.StatusAbandoned) {
			return true
		}
	}
	return false
}

// detachedReconcileSummary renders reconciliation verdicts for the Human line.
func detachedReconcileSummary(items []DetachedReconcile) string {
	if len(items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s=%s(pid %d, %s)", item.WorkID, item.Outcome, item.PID, item.State))
	}
	return "detached: " + strings.Join(parts, ", ") + "; "
}

// failClaimedUnattendedEnvelope records a launch that will never run on a
// CLAIMED envelope. The lifecycle table has no claimed -> failed edge (a
// claim is not an execution), so this takes the same two steps the
// synchronous seam takes around a launch failure: the running reservation
// under the same actor, then the failed close with the proof ref. Idempotent
// on an envelope that is already past claimed.
func (s *Store) failClaimedUnattendedEnvelope(workID, actor, summary, proofRef string, now time.Time) error {
	w, err := s.Get(workID)
	if err != nil {
		return err
	}
	if strings.HasPrefix(w.State, "claimed") {
		if _, err := s.Advance(workID, AdvanceInput{To: "running", Actor: actor, Note: "execute stage: running (recording a launch that never ran)"}, now); err != nil {
			return err
		}
	}
	_, err = s.Close(workID, CloseInput{
		Outcome:   "failed",
		MoveType:  "audit_only",
		Actor:     actor,
		Summary:   summary,
		ProofRefs: []string{proofRef},
	}, now)
	return err
}

// DetachedDoorOwns reports whether an ALIVE detached door currently owns the
// envelope: an open sidecar whose pid still exists. The tick's own lifecycle
// stepping (DispatchStep) must not touch such an envelope — the door expects
// to find it exactly as the seam left it (claimed) and refuses otherwise. A
// dead door is not "owning": ReconcileDetachedLaunches records it first.
//
// A `reserved` sidecar (post-start record still missing) reads as OWNED:
// the pid is unknown but a start was armed, and touching the envelope on the
// strength of a missing pid is the one wrong side to fail on.
func DetachedDoorOwns(home, workID string) (DetachedLaunch, bool) {
	launch, err := ReadDetachedLaunch(home, strings.TrimSpace(workID))
	if err != nil {
		return DetachedLaunch{}, false
	}
	if launch.Outcome == DetachedOutcomeReserved {
		return launch, true
	}
	if launch.Outcome != "" && launch.Outcome != DetachedOutcomeRunning {
		return launch, false
	}
	return launch, detachedProcessAlive(launch.PID)
}
