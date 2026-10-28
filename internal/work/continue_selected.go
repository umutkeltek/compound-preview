package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"compound/internal/envelope"
)

// SelectedContinueLauncher is wired by executewire to the existing detached
// governed door. A missing binding fails closed, never falls into queue work.
var SelectedContinueLauncher func(*Store, envelope.WorkEnvelope, string, bool, time.Time) (DetachedLaunch, error)

// ExecutionSourceRevision binds the full hydrated source generation passed to
// a detached door. It is a stale-view guard, not permission or execution proof.
func ExecutionSourceRevision(w envelope.WorkEnvelope) string {
	body, _ := json.Marshal(w)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s *Store) continueSelected(now time.Time, opts ContinueOptions) (ContinueResult, error) {
	id, actor := strings.TrimSpace(opts.ExpectWorkID), strings.TrimSpace(opts.Actor)
	if opts.Resume && (opts.Outcome != "" || opts.VerificationRef != "") {
		return ContinueResult{}, errors.New("selected Continue cannot resume and verify or accept an outcome in the same action")
	}
	if actor == "" {
		return ContinueResult{}, errors.New("selected Continue requires --actor")
	}
	if id == "" {
		var err error
		id, err = s.uniqueSelectedWork(actor)
		if err != nil {
			return ContinueResult{}, err
		}
	}
	if !strings.HasPrefix(id, "work_") || filepath.Base(id) != id {
		return ContinueResult{}, errors.New("selected Continue requires an exact work identity")
	}
	// Serialize competing explicit clicks BEFORE reading or replacing the
	// per-work launch reservation. The child uses the ordinary work CAS locks.
	if err := os.MkdirAll(DetachedLaunchDir(s.home), 0o755); err != nil {
		return ContinueResult{}, err
	}
	lock, err := os.OpenFile(filepath.Join(DetachedLaunchDir(s.home), id+".continue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return ContinueResult{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return ContinueResult{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	w, err := s.Get(id)
	if err != nil {
		return ContinueResult{}, err
	}
	lease, err := s.readLease(id)
	if err != nil || lease.By != actor {
		return ContinueResult{}, fmt.Errorf("selected Continue requires the current lease holder; actor %q is not authorized", actor)
	}
	result := ContinueResult{
		SchemaVersion: WorkContinueSchemaVersion, ID: idFor("wcon_", actor, ts(now)), GeneratedAt: now.UTC().Format(time.RFC3339),
		Actor: actor, WorkID: id, ExpectWorkID: id, Status: "waiting", Action: "none", ContextPackID: w.IDs.ContextPackID,
		Warnings: []string{"continues only the selected work; no queue dispatch", "at most one governed launch; execution success is not task acceptance"},
	}
	if _, err := ReadDetachedLaunch(s.home, id); err != nil && !os.IsNotExist(err) {
		return ContinueResult{}, fmt.Errorf("selected launch evidence is unreadable; no new launch: %w", err)
	}
	finish := func() (ContinueResult, error) {
		path, err := s.RecordContinue(result)
		result.ResultPath = path
		return result, err
	}
	if !opts.DryRun {
		result.DetachedReconciled = s.ReconcileDetachedLaunch(id, now)
	}
	if launch, owns := DetachedDoorOwns(s.home, id); owns {
		result.Detached = &launch
		result.WaitReason = "detached_door_running"
		result.Human = "Selected work already has an active or unresolved launch; no second launch."
		return finish()
	}
	// Reconciliation never grants authority. Reload after it before selecting
	// a source generation and retain unknown launch evidence as a real wait.
	w, err = s.Get(id)
	if err != nil {
		return ContinueResult{}, err
	}
	if opts.VerificationRef != "" {
		if w.State != "verifying" || strings.TrimSpace(opts.VerificationRef) == "" {
			return ContinueResult{}, errors.New("selected verification requires verifying work and a nonempty evidence reference")
		}
		if !opts.DryRun {
			w, err = s.Advance(id, AdvanceInput{
				To: "verifying", Actor: actor, VerificationID: opts.VerificationRef,
				ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID,
				ExpectedActionIntentID: selectedLastAction(w),
				Note:                   "selected Continue verifier supplied evidence; not automatic acceptance",
			}, now)
			if err != nil {
				return ContinueResult{}, err
			}
		}
	}
	if w.State == "running" {
		result.WaitReason = "run_evidence_required"
		result.Human = "The selected work is running; its exact execution must settle before continuing."
		return finish()
	}
	if w.State == "verifying" && !opts.Resume {
		if opts.Outcome != "" && !opts.DryRun {
			if strings.TrimSpace(opts.MoveType) == "" || strings.TrimSpace(opts.Summary) == "" {
				return ContinueResult{}, errors.New("accepting selected work requires an explicit move type and evidence-based summary")
			}
			if opts.Outcome == "completed" {
				if err := s.selectedVerificationReady(w); err != nil {
					result.WaitReason = "verification_required"
					result.NoAction, result.Human = err.Error(), err.Error()
					return finish()
				}
			}
			closed, err := s.DispatchClose(now, DispatchCloseOptions{
				WorkID: id, Actor: actor, Outcome: opts.Outcome, MoveType: opts.MoveType, Summary: opts.Summary,
				ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID,
				ExpectedActionIntentID: selectedLastAction(w),
			})
			if err != nil {
				return ContinueResult{}, err
			}
			result.SelectedClose = &closed
			if closed.Status == "closed" {
				result.Action, result.Status = "close", closed.FinalState
				result.Human = "Selected work closed through its existing proof and policy gates; receipt " + closed.ReceiptID
				return finish()
			}
			result.NoAction = closed.NoAction
		}
		result.WaitReason = "verification_required"
		result.Human = "Execution reached verification. Inspect its proof; a successful exit is not task acceptance."
		return finish()
	}
	if (opts.Resume && w.State != "verifying") || (!opts.Resume && w.State != "claimed") {
		return ContinueResult{}, fmt.Errorf("selected Continue requires claimed work, or explicit resume of verifying work (found %s)", w.State)
	}
	if w.ExecutionSelection == nil || w.ExecutionSelection.Revision < 1 {
		return ContinueResult{}, errors.New("selected Continue requires a saved execution selection; it never chooses a fallback")
	}
	if opts.DryRun {
		result.WaitReason = "dry_run"
		result.Human = "Selected work is eligible for a launch preflight; dry run does not execute or attest runtime readiness."
		return finish()
	}
	if SelectedContinueLauncher == nil {
		return ContinueResult{}, errors.New("selected Continue launch binding is unavailable")
	}
	if err := s.archiveSelectedDoor(id); err != nil {
		return ContinueResult{}, err
	}
	launch, err := SelectedContinueLauncher(s, w, actor, opts.Resume, now)
	var started *DetachedLaunchRecordError
	if err != nil && !errors.As(err, &started) {
		return ContinueResult{}, err
	}
	if started != nil {
		launch = started.Launch
		result.Warnings = append(result.Warnings, err.Error())
	}
	result.Action, result.Status = "execute", "launched"
	result.Detached, result.Lane = &launch, launch.Lane
	result.WaitReason = "detached_door_running"
	result.Human = "Selected work handed to the bounded governed door; verification and acceptance are still pending."
	result.Execute = buildExecuteVerdict(result, "launched")
	return finish()
}

func selectedLastAction(w envelope.WorkEnvelope) string {
	if len(w.ActionIntentIDs) == 0 {
		return ""
	}
	return w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
}

// Before reusing the per-work current sidecar, retain its exact bytes in the
// existing detached owner's history directory. Failed and lost trials remain
// evidence; a failed archive never authorizes replacement or another launch.
func (s *Store) archiveSelectedDoor(id string) error {
	previous, err := ReadDetachedLaunch(s.home, id)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if previous.Outcome != DetachedOutcomeDoorFinalized && previous.Outcome != DetachedOutcomeDoorLost && previous.Outcome != DetachedOutcomeDoorLostRunning {
		return errors.New("prior detached launch is not durably settled; no new launch")
	}
	body, err := os.ReadFile(detachedLaunchPath(s.home, id))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	dir := filepath.Join(DetachedLaunchDir(s.home), "history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, id+"."+hex.EncodeToString(sum[:])+".json")
	if stored, err := os.ReadFile(path); err == nil {
		if string(stored) != string(body) {
			return errors.New("detached launch archive collision")
		}
		directory, err := os.Open(dir)
		if err != nil {
			return err
		}
		return errors.Join(directory.Sync(), directory.Close())
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(dir, ".archive-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if n, err := file.Write(body); err != nil || n != len(body) {
		_ = file.Close()
		return errors.Join(err, io.ErrShortWrite)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	// Hard-link publication is atomic and exclusive. A concurrent writer
	// cannot replace an existing historical body; this work's lock serializes
	// normal callers, while the content address makes retries idempotent.
	if err := os.Link(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// An actor with one resumable lease needs no copied ID. Ambiguity never turns
// into queue selection: the caller must choose the work, not the scheduler.
func (s *Store) uniqueSelectedWork(actor string) (string, error) {
	leases, err := s.ListLeases()
	if err != nil {
		return "", err
	}
	var selected string
	for _, lease := range leases {
		if lease.By != actor {
			continue
		}
		w, err := s.Get(lease.WorkID)
		if err != nil {
			return "", err
		}
		if w.State != "claimed" && w.State != "running" && w.State != "verifying" {
			continue
		}
		if selected != "" {
			return "", errors.New("selected Continue is ambiguous: choose one of this actor's active works; no launch")
		}
		selected = w.ID
	}
	if selected == "" {
		return "", errors.New("selected Continue found no active work held by this actor")
	}
	return selected, nil
}
