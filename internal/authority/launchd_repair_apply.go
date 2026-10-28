package authority

import (
	"compound/internal/eventstore"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/work"
)

type launchdPhaseStart struct {
	Digest        string                    `json:"digest"`
	PlanID        string                    `json:"plan_id"`
	ReviewID      string                    `json:"review_id"`
	Direction     string                    `json:"direction"`
	Index         int                       `json:"index"`
	Name          string                    `json:"name"`
	BeforeWork    envelope.WorkEnvelope     `json:"before_work"`
	BeforeRuntime LaunchdRuntime            `json:"before_runtime"`
	BeforeBytes   []byte                    `json:"before_bytes"`
	BeforeOwner   []byte                    `json:"before_owner,omitempty"`
	Pending       work.ReceiptedActionInput `json:"pending"`
	AuthorizedAt  time.Time                 `json:"authorized_at"`
}
type launchdPhaseOutcome struct {
	Digest      string                    `json:"digest"`
	StartDigest string                    `json:"start_digest"`
	Runtime     LaunchdRuntime            `json:"runtime"`
	FileSHA256  string                    `json:"file_sha256"`
	OwnerSHA256 string                    `json:"owner_sha256,omitempty"`
	Failure     string                    `json:"failure,omitempty"`
	Final       work.ReceiptedActionInput `json:"final"`
}
type launchdPhaseCommit struct {
	NativeDigest  string `json:"native_digest"`
	StartDigest   string `json:"start_digest"`
	OutcomeDigest string `json:"outcome_digest"`
	WorkSHA256    string `json:"work_sha256"`
	ReceiptID     string `json:"receipt_id"`
	TransitionID  string `json:"transition_id"`
	Outcome       string `json:"outcome"`
}
type LaunchdRepairResult struct {
	PlanID    string               `json:"plan_id"`
	Direction string               `json:"direction"`
	Outcome   string               `json:"outcome"`
	Phases    []launchdPhaseCommit `json:"phases"`
	Runtime   LaunchdRuntime       `json:"runtime"`
	Failure   string               `json:"failure,omitempty"`
}

func launchdPhaseStem(p LaunchdRepairPlan, direction string, index int) string {
	return fmt.Sprintf("%s.%s.%d", p.ID, direction, index)
}
func launchdCheckpoint(env LaunchdEnvironment, point string) error {
	if env.Checkpoint != nil {
		return env.Checkpoint(point)
	}
	return nil
}
func launchdCurrent(ctx context.Context, env LaunchdEnvironment, p LaunchdRepairPlan) ([]byte, LaunchdRuntime, error) {
	var r LaunchdRuntime
	if err := validateLaunchdPath(env, p.PlistPath, p.Label); err != nil {
		return nil, r, err
	}
	b, mode, err := launchdOwnedFile(p.PlistPath, p.UID)
	if err != nil {
		return nil, r, err
	}
	if uint32(mode) != p.Mode {
		return nil, r, fmt.Errorf("plist mode drift")
	}
	r = ProbeLaunchdRuntime(ctx, env, p.Label)
	if r.Error != "" || r.OverrideState == "unknown" || r.LoadedState == "unknown" {
		return b, r, fmt.Errorf("unknown runtime state: %s", r.Error)
	}
	if !reflect.DeepEqual(r.ForeignOverrides, p.Runtime.ForeignOverrides) {
		return b, r, fmt.Errorf("unrelated launchd overrides changed; no repair permitted")
	}
	return b, r, nil
}
func launchdServiceOwned(p LaunchdRepairPlan, r LaunchdRuntime) bool {
	return r.Domain == p.Domain && r.Label == p.Label && r.LoadedState == "loaded" && r.ServicePath == p.PlistPath && r.Program == p.CandidateArgs[0] && reflect.DeepEqual(r.Arguments, p.CandidateArgs)
}
func launchdStateMatches(p LaunchdRepairPlan, b []byte, r LaunchdRuntime, bytes []byte, override, loaded string) error {
	if !bytesEqual(b, bytes) || r.OverrideState != override || r.LoadedState != loaded {
		return fmt.Errorf("selected plist/runtime drift: override=%s loaded=%s", r.OverrideState, r.LoadedState)
	}
	if loaded == "loaded" && !launchdServiceOwned(p, r) {
		return fmt.Errorf("loaded service identity is not owned by this plan")
	}
	return nil
}
func launchdAtomicReplace(path string, expected, replacement []byte, mode uint32) error {
	current, _, err := launchdOwnedFile(path, os.Getuid())
	if err != nil {
		return err
	}
	if !bytesEqual(current, expected) {
		return fmt.Errorf("foreign plist bytes; refuse replacement")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".compound-launchd-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(os.FileMode(mode)); err == nil {
		_, err = f.Write(replacement)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Recheck after durable preparation, immediately before atomic publication.
	current, _, err = launchdOwnedFile(path, os.Getuid())
	if err != nil || !bytesEqual(current, expected) {
		return fmt.Errorf("plist changed during replacement")
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func launchdEffect(ctx context.Context, env LaunchdEnvironment, p LaunchdRepairPlan, name string) error {
	switch name {
	case "replace":
		return launchdAtomicReplace(p.PlistPath, p.Original, p.Candidate, p.Mode)
	case "restore":
		return launchdAtomicReplace(p.PlistPath, p.Candidate, p.Original, p.Mode)
	}
	var args []string
	switch name {
	case "enable":
		args = []string{"enable", p.Domain + "/" + p.Label}
	case "bootstrap":
		args = []string{"bootstrap", p.Domain, p.PlistPath}
	case "bootout":
		args = []string{"bootout", p.Domain + "/" + p.Label}
	case "disable":
		args = []string{"disable", p.Domain + "/" + p.Label}
	default:
		return fmt.Errorf("unknown exact launchd phase")
	}
	r, err := launchdRun(ctx, env, "/bin/launchctl", args...)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 || r.Stderr != "" {
		return fmt.Errorf("launchctl %s failed (exit %d): %s", name, r.ExitCode, r.Stderr)
	}
	return nil
}
func launchdPostcondition(p LaunchdRepairPlan, name string, b []byte, r LaunchdRuntime) error {
	switch name {
	case "replace":
		return launchdStateMatches(p, b, r, p.Candidate, "disabled", "absent")
	case "enable":
		return launchdStateMatches(p, b, r, p.Candidate, "enabled", "absent")
	case "bootstrap":
		return launchdStateMatches(p, b, r, p.Candidate, "enabled", "loaded")
	case "bootout":
		return launchdStateMatches(p, b, r, p.Candidate, "enabled", "absent")
	case "disable":
		if !bytesEqual(b, p.Candidate) && !bytesEqual(b, p.Original) {
			return fmt.Errorf("foreign rollback file")
		}
		if r.OverrideState != "disabled" || r.LoadedState != "absent" {
			return fmt.Errorf("disable readback failed")
		}
		return nil
	case "restore":
		return launchdStateMatches(p, b, r, p.Original, "disabled", "absent")
	}
	return fmt.Errorf("unknown phase postcondition")
}
func launchdPhaseAuthorize(ctx context.Context, home string, store *work.Store, p LaunchdRepairPlan, review LaunchdRepairReview, env LaunchdEnvironment, before envelope.WorkEnvelope, bytes []byte, runtime LaunchdRuntime, action envelope.ActionIntent, decision envelope.PolicyDecision) func() error {
	return func() error {
		now := launchdNow(env)
		if err := validateLaunchdReview(home, p, review, now); err != nil {
			return err
		}
		if review.Request.NoEffectContinuation != nil {
			if _, err := validateLaunchdNoEffectContinuation(home, p, review); err != nil {
				return err
			}
		}
		current, err := store.Get(p.WorkID)
		if err != nil {
			return err
		}
		if launchdWorkDigest(current) != launchdWorkDigest(before) {
			return fmt.Errorf("unrelated work generation drift")
		}
		if err := validateLaunchdPhaseOwner(home, p, action.Action); err != nil {
			return err
		}
		b, r, err := launchdCurrent(ctx, env, p)
		if err != nil {
			return err
		}
		if !bytesEqual(b, bytes) || r.OverrideState != runtime.OverrideState || r.LoadedState != runtime.LoadedState || r.ServicePath != runtime.ServicePath || !reflect.DeepEqual(r.Arguments, runtime.Arguments) || r.Program != runtime.Program {
			return fmt.Errorf("selected launchd preimage drift")
		}
		cfg, _, err := policy.LoadConfig(home)
		if err != nil {
			return err
		}
		fresh := policy.Evaluate(action, cfg, now)
		if !policy.Allowed(fresh) || fresh.StandingGrantID != review.Request.GrantID || decision.StandingGrantID != review.Request.GrantID {
			return fmt.Errorf("fresh exact launchd phase policy denied")
		}
		for _, class := range []string{"build-implement", LaunchdRepairCapability} {
			halted, _, err := policy.AutonomyHalted(home, class)
			if err != nil {
				return err
			}
			if halted {
				return fmt.Errorf("launchd repair autonomy halted")
			}
		}
		for _, proof := range p.SourceProofs {
			if _, err := inspectLaunchdSource(proof); err != nil {
				return err
			}
		}
		return nil
	}
}
func finishLaunchdPhase(home string, store *work.Store, p LaunchdRepairPlan, start launchdPhaseStart, outcome launchdPhaseOutcome, requireCurrent bool) (launchdPhaseCommit, error) {
	var commit launchdPhaseCommit
	copy := start
	copy.Digest = ""
	outcopy := outcome
	outcopy.Digest = ""
	if start.PlanID != p.ID || start.Digest != launchdDigest(copy) || outcome.StartDigest != start.Digest || outcome.Digest != launchdDigest(outcopy) {
		return commit, fmt.Errorf("launchd phase evidence digest mismatch")
	}
	if err := store.SealReceiptedActionOutcome(p.WorkID, start.Pending, outcome.Final, start.AuthorizedAt); err != nil {
		// A completed reservation may already have been cleared after a crash. Its
		// exact replay below authenticates the immutable outcome before accepting it.
		_, _, present, readErr := store.ReadReceiptedActionReservationInput(p.WorkID, start.Pending.Action.ID)
		if readErr != nil || present {
			return commit, err
		}
	}
	recorded, err := store.RecordReservedReceiptedAction(p.WorkID, outcome.Final, start.AuthorizedAt)
	if err != nil {
		return commit, err
	}
	expected := launchdExpectedWork(start.BeforeWork, start.Pending.Action, *start.Pending.PreEvaluatedDecision)
	if requireCurrent && launchdWorkDigest(recorded.Work) != launchdWorkDigest(expected) {
		return commit, fmt.Errorf("phase receipt exists but unrelated work generation drifted; refuse continuation")
	}
	event, err := launchdNativePhaseEvent(store, p, start, recorded.Decision, recorded.Receipt.ID, recorded.Transition.ID)
	if err != nil {
		return commit, err
	}
	nativeDigest := launchdDigest(launchdNativePhaseOutputs{Action: recorded.Action, Decision: recorded.Decision, Receipt: recorded.Receipt, Transition: recorded.Transition, Event: event})
	commit = launchdPhaseCommit{NativeDigest: nativeDigest, StartDigest: start.Digest, OutcomeDigest: outcome.Digest, WorkSHA256: launchdWorkDigest(expected), ReceiptID: recorded.Receipt.ID, TransitionID: recorded.Transition.ID, Outcome: outcome.Final.Outcome}
	return commit, writeLaunchdArtifact(home, launchdPhaseStem(p, start.Direction, start.Index)+".committed.json", commit)
}
func runLaunchdPhase(ctx context.Context, home string, store *work.Store, p LaunchdRepairPlan, review LaunchdRepairReview, env LaunchdEnvironment, direction string, index int, name, expectedWork string, beforeBytes []byte, beforeRuntime LaunchdRuntime) (launchdPhaseCommit, error) {
	stem := launchdPhaseStem(p, direction, index)
	var start launchdPhaseStart
	var outcome launchdPhaseOutcome
	err := readLaunchdArtifact(home, stem+".started.json", &start)
	if err == nil {
		if start.Name != name || start.ReviewID != review.ID || start.Direction != direction || start.Index != index || launchdWorkDigest(start.BeforeWork) != expectedWork {
			return launchdPhaseCommit{}, fmt.Errorf("phase reservation lineage mismatch")
		}
		err = readLaunchdArtifact(home, stem+".outcome.json", &outcome)
		if os.IsNotExist(err) {
			// A started effect without a sealed observed outcome is ambiguous. Observe
			// and receipt that ambiguity; never repeat the effect or continue forward.
			b, r, probeErr := launchdCurrent(ctx, env, p)
			failure := "interrupted phase has ambiguous effect; explicit reviewed rollback required"
			if probeErr != nil {
				failure += "; " + probeErr.Error()
			}
			cause := fmt.Errorf("%s", failure)
			if start.Name == "restore_owner" && p.Handoff != nil && bytesEqual(start.BeforeOwner, p.Handoff.Owner) && start.Pending.PreEvaluatedDecision != nil {
				// Only a proven read-only no-effect observation may be retried.
				// Recheck fresh authority; this callback can never perform a CAS.
				authorize := launchdPhaseAuthorize(ctx, home, store, p, review, env, start.BeforeWork, start.BeforeBytes, start.BeforeRuntime, start.Pending.Action, *start.Pending.PreEvaluatedDecision)
				cause = store.ReauthorizeReceiptedActionReservation(p.WorkID, start.Pending, start.AuthorizedAt, start.Pending, start.AuthorizedAt, func() error {
					if err := authorize(); err != nil {
						return err
					}
					return observeLaunchdUnchangedOwnership(home, p)
				})
				b, r, probeErr = launchdCurrent(ctx, env, p)
				if cause == nil {
					cause = probeErr
				}
				if cause == nil {
					cause = launchdStateMatches(p, b, r, p.Original, "disabled", "absent")
				}
			}
			outcome = makeLaunchdPhaseOutcome(home, p, start, b, r, cause)
			if err = writeLaunchdArtifact(home, stem+".outcome.json", outcome); err != nil {
				return launchdPhaseCommit{}, err
			}
		} else if err != nil {
			return launchdPhaseCommit{}, err
		}
		commit, err := finishLaunchdPhase(home, store, p, start, outcome, true)
		if err != nil {
			return commit, err
		}
		if outcome.Final.Outcome != "completed" {
			return commit, fmt.Errorf("%s", outcome.Failure)
		}
		return commit, nil
	} else if !os.IsNotExist(err) {
		return launchdPhaseCommit{}, err
	}
	before, err := store.Get(p.WorkID)
	if err != nil {
		return launchdPhaseCommit{}, err
	}
	if launchdWorkDigest(before) != expectedWork {
		return launchdPhaseCommit{}, fmt.Errorf("unrelated work generation drift")
	}
	now := launchdNow(env)
	a := launchdAction(p, p.ID+direction+fmt.Sprint(index), direction+"_"+name, now)
	a.EvidenceRefs = append(a.EvidenceRefs, "launchd-review:"+review.ID)
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		return launchdPhaseCommit{}, err
	}
	pd := policy.Evaluate(a, cfg, now)
	if !policy.Allowed(pd) || pd.StandingGrantID != review.Request.GrantID {
		return launchdPhaseCommit{}, fmt.Errorf("exact launchd phase grant denied")
	}
	in := work.ReceiptedActionInput{Action: a, PreEvaluatedDecision: &pd, ExpectedWorkState: "running", OutcomePending: true, Outcome: "blocked", Summary: "awaiting observed launchd phase outcome", MoveType: "control_plane_repair", ProofRefs: []string{"launchd-review:" + review.ID, "launchd-plan:" + p.ID}, BeforeRefs: []string{"sha256:" + launchdBytesDigest(beforeBytes), "launchd-runtime:" + launchdDigest(beforeRuntime)}}
	var owner []byte
	if name == "handoff" || name == "restore_owner" {
		owner, err = readLaunchdOwner(home, p)
		if err != nil {
			return launchdPhaseCommit{}, err
		}
		in.BeforeRefs = append(in.BeforeRefs, "launchd-owner:sha256:"+launchdBytesDigest(owner))
	}
	authorize := launchdPhaseAuthorize(ctx, home, store, p, review, env, before, beforeBytes, beforeRuntime, a, pd)
	if err = store.ReserveReceiptedAction(p.WorkID, in, now, authorize); err != nil {
		return launchdPhaseCommit{}, err
	}
	start = launchdPhaseStart{PlanID: p.ID, ReviewID: review.ID, Direction: direction, Index: index, Name: name, BeforeWork: before, BeforeRuntime: beforeRuntime, BeforeBytes: beforeBytes, BeforeOwner: owner, Pending: in, AuthorizedAt: now}
	start.Digest = launchdDigest(start)
	if err = writeLaunchdArtifact(home, stem+".started.json", start); err != nil {
		return launchdPhaseCommit{}, err
	}
	if crash := launchdCheckpoint(env, name+":before_effect"); crash != nil {
		return launchdPhaseCommit{}, crash
	}
	// This store-owned reauthorization fences exact work/lease authority and
	// re-runs the live policy/readback callback immediately before the effect.
	err = store.ReauthorizeReceiptedActionReservation(p.WorkID, in, now, in, now, func() error {
		if err := authorize(); err != nil {
			return err
		}
		if review.Request.NoEffectContinuation != nil {
			if name != "restore_owner" {
				return fmt.Errorf("no-effect continuation cannot execute another phase")
			}
			return observeLaunchdUnchangedOwnership(home, p)
		}
		if name == "handoff" || name == "restore_owner" {
			return launchdOwnerEffect(home, p, name)
		}
		return launchdEffect(ctx, env, p, name)
	})
	if crash := launchdCheckpoint(env, name+":after_effect"); crash != nil {
		return launchdPhaseCommit{}, crash
	}
	b, r, readErr := launchdCurrent(ctx, env, p)
	if err == nil {
		err = readErr
	}
	if err == nil && name != "handoff" && name != "restore_owner" {
		err = launchdPostcondition(p, name, b, r)
	}
	if err == nil && (name == "handoff" || name == "restore_owner") {
		err = launchdStateMatches(p, b, r, p.Original, "disabled", "absent")
	}
	outcome = makeLaunchdPhaseOutcome(home, p, start, b, r, err)
	if saveErr := writeLaunchdArtifact(home, stem+".outcome.json", outcome); saveErr != nil {
		return launchdPhaseCommit{}, saveErr
	}
	if crash := launchdCheckpoint(env, name+":before_receipt"); crash != nil {
		return launchdPhaseCommit{}, crash
	}
	commit, sealErr := finishLaunchdPhase(home, store, p, start, outcome, true)
	if sealErr != nil {
		return commit, sealErr
	}
	if crash := launchdCheckpoint(env, name+":after_receipt"); crash != nil {
		return commit, crash
	}
	if outcome.Final.Outcome != "completed" && err == nil {
		err = fmt.Errorf("%s", outcome.Failure)
	}
	return commit, err
}
func makeLaunchdOutcome(start launchdPhaseStart, b []byte, r LaunchdRuntime, cause error) launchdPhaseOutcome {
	final := start.Pending
	final.OutcomePending = false
	final.Outcome = "completed"
	final.Summary = "observed exact launchd phase " + start.Name
	knownOverride := func(s string) bool { return s == "enabled" || s == "disabled" || s == "absent" }
	knownLoaded := func(s string) bool { return s == "loaded" || s == "absent" }
	final.Changed = (b != nil && !bytesEqual(b, start.BeforeBytes)) || (knownOverride(r.OverrideState) && knownOverride(start.BeforeRuntime.OverrideState) && r.OverrideState != start.BeforeRuntime.OverrideState) || (knownLoaded(r.LoadedState) && knownLoaded(start.BeforeRuntime.LoadedState) && r.LoadedState != start.BeforeRuntime.LoadedState)
	fileSHA := ""
	final.AfterRefs = nil
	if b != nil {
		fileSHA = launchdBytesDigest(b)
		final.AfterRefs = append(final.AfterRefs, "sha256:"+fileSHA)
	}
	final.AfterRefs = append(final.AfterRefs, "launchd-runtime:"+launchdDigest(r))
	out := launchdPhaseOutcome{StartDigest: start.Digest, Runtime: r, FileSHA256: fileSHA, Final: final}
	if cause != nil {
		out.Failure = cause.Error()
		out.Final.Outcome = "failed"
		out.Final.Summary = "launchd phase stopped: " + cause.Error()
		out.Final.FailureCause = &envelope.FailureCause{SurfaceOutcome: "launchd_phase_failed", Mechanism: cause.Error()}
	}
	out.Final.ProofRefs = append(append([]string{}, start.Pending.ProofRefs...), "launchd-phase-start:sha256:"+start.Digest, "launchd-phase-observed:sha256:"+launchdObservedPhaseDigest(out))
	out.Digest = launchdDigest(out)
	return out
}
func ApplyLaunchdRepair(ctx context.Context, home, planID, reviewID, actor string, env LaunchdEnvironment) (LaunchdRepairResult, error) {
	return executeLaunchdRepair(ctx, home, planID, reviewID, actor, env, false)
}
func RollbackLaunchdRepair(ctx context.Context, home, planID, reviewID, actor string, env LaunchdEnvironment) (LaunchdRepairResult, error) {
	return executeLaunchdRepair(ctx, home, planID, reviewID, actor, env, true)
}
func executeLaunchdRepair(ctx context.Context, home, planID, reviewID, actor string, env LaunchdEnvironment, rollback bool) (LaunchdRepairResult, error) {
	result := LaunchdRepairResult{PlanID: planID, Direction: "apply", Outcome: "blocked"}
	if rollback {
		result.Direction = "rollback"
	}
	p, err := loadLaunchdPlan(home, planID)
	if err != nil {
		return result, err
	}
	if p.Actor != actor || p.UserHome != env.UserHome || p.UID != env.UID {
		return result, fmt.Errorf("exact plan actor and current-user environment required")
	}
	unlock, err := lockLaunchdLabel(home, p.Domain+"/"+p.Label)
	if err != nil {
		return result, err
	}
	defer unlock()
	var review LaunchdRepairReview
	if err = readLaunchdArtifact(home, reviewID+".review.json", &review); err != nil {
		return result, err
	}
	if review.ID != reviewID {
		return result, fmt.Errorf("review filename mismatch")
	}
	if err = validateLaunchdReviewEvidence(home, p, review); err != nil {
		return result, err
	}
	if (rollback && review.Request.Purpose != "rollback") || (!rollback && review.Request.Purpose != "apply") {
		return result, fmt.Errorf("review does not authorize this direction")
	}
	store, err := work.Open(home)
	if err != nil {
		return result, err
	}
	activeName := launchdActiveName(p)
	var active struct {
		PlanID string `json:"plan_id"`
	}
	if err = readLaunchdArtifact(home, activeName, &active); err == nil {
		if active.PlanID != p.ID {
			if p.Handoff == nil || active.PlanID != p.Handoff.FromPlanID || (rollback && !launchdOwnerRestoreStarted(home, p) && launchdUnchangedHandoff(home, p) != nil) {
				return result, fmt.Errorf("another same-label plan requires terminal reconciliation")
			}
		}
	} else if os.IsNotExist(err) {
		if rollback || p.Handoff != nil {
			return result, fmt.Errorf("no active owned plan to roll back or hand off")
		}
		if err = validateLaunchdReview(home, p, review, launchdNow(env)); err != nil {
			return result, err
		}
		active.PlanID = p.ID
		if err = writeLaunchdArtifact(home, activeName, active); err != nil {
			return result, err
		}
	} else {
		return result, err
	}
	expectedWork := review.WorkSHA256
	continuation := review.Request.NoEffectContinuation
	names := []string{"replace", "enable", "bootstrap"}
	if p.Handoff != nil {
		names = append([]string{"handoff"}, names...)
	}
	if rollback {
		if continuation != nil {
			result.Phases, err = validateLaunchdNoEffectContinuation(home, p, review)
			names = []string{"restore_owner"}
		} else {
			names, err = launchdRollbackPhases(ctx, home, p, env, review)
		}
		if err != nil {
			return result, err
		}
	} else if continuation != nil {
		return result, fmt.Errorf("no-effect continuation authorizes rollback observation only")
	}
	for position, name := range names {
		i := position
		if continuation != nil {
			i = continuation.Index
		}
		if !rollback && p.Handoff != nil {
			// Preserve immutable legacy apply indexes; ownership is a prefix phase.
			i = map[string]int{"handoff": 3, "replace": 0, "enable": 1, "bootstrap": 2}[name]
		}
		var committed launchdPhaseCommit
		stem := launchdPhaseStem(p, result.Direction, i)
		if err = readLaunchdArtifact(home, stem+".committed.json", &committed); err == nil {
			var start launchdPhaseStart
			var outcome launchdPhaseOutcome
			if err = readLaunchdArtifact(home, stem+".started.json", &start); err != nil {
				return result, err
			}
			if err = readLaunchdArtifact(home, stem+".outcome.json", &outcome); err != nil {
				return result, err
			}
			if start.ReviewID != review.ID || start.Name != name || launchdWorkDigest(start.BeforeWork) != expectedWork {
				return result, fmt.Errorf("committed phase lineage mismatch")
			}
			if err := verifyLaunchdCommittedPhase(home, p, start, outcome, committed); err != nil {
				return result, err
			}
			result.Phases = append(result.Phases, committed)
			expectedWork = committed.WorkSHA256
			if committed.Outcome != "completed" {
				return result, fmt.Errorf("previous failed phase requires explicit rollback")
			}
			continue
		} else if !os.IsNotExist(err) {
			return result, err
		}
		b, r, readErr := launchdCurrent(ctx, env, p)
		result.Runtime = r
		if readErr != nil {
			return result, readErr
		}
		// An existing started phase is reconciled before precondition checks, so a
		// successful effect followed by a crash cannot be mistaken for fresh work.
		var started launchdPhaseStart
		startedErr := readLaunchdArtifact(home, stem+".started.json", &started)
		if os.IsNotExist(startedErr) {
			if !rollback {
				switch name {
				case "handoff", "replace":
					err = launchdStateMatches(p, b, r, p.Original, "disabled", "absent")
				case "enable":
					err = launchdStateMatches(p, b, r, p.Candidate, "disabled", "absent")
				case "bootstrap":
					err = launchdStateMatches(p, b, r, p.Candidate, "enabled", "absent")
				}
			} else {
				err = validateLaunchdRollbackPrecondition(p, name, b, r)
			}
			if err != nil {
				return result, err
			}
		} else if startedErr != nil {
			return result, startedErr
		}
		commit, phaseErr := runLaunchdPhase(ctx, home, store, p, review, env, result.Direction, i, name, expectedWork, b, r)
		if commit.ReceiptID != "" {
			result.Phases = append(result.Phases, commit)
			expectedWork = commit.WorkSHA256
		}
		if phaseErr != nil {
			result.Outcome = "partial"
			result.Failure = phaseErr.Error()
			result.Runtime = ProbeLaunchdRuntime(ctx, env, p.Label)
			return result, phaseErr
		}
	}
	current, err := store.Get(p.WorkID)
	if err != nil {
		return result, err
	}
	if launchdWorkDigest(current) != expectedWork {
		return result, fmt.Errorf("unrelated final work generation drift")
	}
	b, r, err := launchdCurrent(ctx, env, p)
	result.Runtime = r
	if err != nil {
		return result, err
	}
	if err = validateLaunchdTerminalState(home, p, rollback, b, r); err != nil {
		result.Failure = err.Error()
		return result, err
	}
	result.Outcome = "completed"
	if err = writeLaunchdArtifact(home, p.ID+"."+result.Direction+".terminal.json", result); err != nil {
		return result, err
	}
	// Keep a successful apply's ownership pointer for explicit rollback. Only a
	// verified rollback releases it; no automatic inverse effects run on error.
	if rollback && p.Handoff == nil {
		path, _ := launchdArtifactPath(home, activeName)
		if err = os.Remove(path); err != nil {
			return result, err
		}
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return result, err
		}
		defer dir.Close()
		if err = dir.Sync(); err != nil {
			return result, err
		}
	}
	return result, nil
}

// A terminal record is the last irreversible claim that native state matches
// this plan, so it revalidates the exact expected bytes, runtime/service
// identity and current ownership after the last phase receipt. Drift refuses
// completion: it never writes a terminal, rewrites a receipt, or repeats an
// effect, and a retry under the same drift repeats no OS effect.
func validateLaunchdTerminalState(home string, p LaunchdRepairPlan, rollback bool, b []byte, r LaunchdRuntime) error {
	if !rollback {
		if !bytesEqual(b, p.Candidate) {
			return fmt.Errorf("final plist is not the exact candidate bytes")
		}
		if r.OverrideState != "enabled" || r.LoadedState != "loaded" {
			return fmt.Errorf("final runtime is not the exact enabled loaded service: override=%s loaded=%s", r.OverrideState, r.LoadedState)
		}
		if !launchdServiceOwned(p, r) {
			return fmt.Errorf("final loaded service identity is not owned by this plan")
		}
		return validateLaunchdTerminalOwner(home, p, launchdOwnerBytes(p.ID))
	}
	if !bytesEqual(b, p.Original) {
		return fmt.Errorf("final plist is not the exact original bytes")
	}
	if r.OverrideState != "disabled" || r.LoadedState != "absent" {
		return fmt.Errorf("final runtime is not the exact disabled absent service: override=%s loaded=%s", r.OverrideState, r.LoadedState)
	}
	// An authenticated predecessor restoration and a read-only no-effect
	// continuation both end on the predecessor's pointer. Only an ordinary
	// rollback releases this plan's own pointer, and it does so just after this.
	if p.Handoff != nil {
		return validateLaunchdTerminalOwner(home, p, p.Handoff.Owner)
	}
	return validateLaunchdTerminalOwner(home, p, launchdOwnerBytes(p.ID))
}
func validateLaunchdTerminalOwner(home string, p LaunchdRepairPlan, expected []byte) error {
	owner, err := readLaunchdOwner(home, p)
	if err != nil {
		return fmt.Errorf("final native ownership unreadable: %v", err)
	}
	if !bytesEqual(owner, expected) {
		return fmt.Errorf("final native ownership sha256:%s is not the exact expected sha256:%s", launchdBytesDigest(owner), launchdBytesDigest(expected))
	}
	return nil
}
func validateLaunchdRollbackPrecondition(p LaunchdRepairPlan, name string, b []byte, r LaunchdRuntime) error {
	if !bytesEqual(b, p.Candidate) && !bytesEqual(b, p.Original) {
		return fmt.Errorf("foreign plist blocks rollback")
	}
	if r.LoadedState == "loaded" && !launchdServiceOwned(p, r) {
		return fmt.Errorf("foreign loaded service blocks every rollback effect")
	}
	switch name {
	case "bootout":
		return launchdStateMatches(p, b, r, p.Candidate, "enabled", "loaded")
	case "disable":
		if r.LoadedState != "absent" || r.OverrideState != "enabled" {
			return fmt.Errorf("rollback disable preimage drift")
		}
	case "restore":
		return launchdStateMatches(p, b, r, p.Candidate, "disabled", "absent")
	case "restore_owner":
		return launchdStateMatches(p, b, r, p.Original, "disabled", "absent")
	}
	return nil
}
func launchdRollbackPhases(ctx context.Context, home string, p LaunchdRepairPlan, env LaunchdEnvironment, review LaunchdRepairReview) ([]string, error) {
	var manifest struct {
		Names  []string `json:"names"`
		Anchor string   `json:"anchor"`
	}
	name := p.ID + ".rollback-manifest.json"
	if err := readLaunchdArtifact(home, name, &manifest); err == nil {
		if manifest.Anchor != review.Request.RollbackAnchor {
			return nil, fmt.Errorf("rollback anchor changed")
		}
		return manifest.Names, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	anchor, workSHA, err := launchdRollbackAnchor(home, p)
	if err != nil {
		return nil, err
	}
	if anchor != review.Request.RollbackAnchor || launchdWorkDigest(review.Request.BeforeWork) != workSHA {
		return nil, fmt.Errorf("rollback review does not follow exact owned phase generation")
	}
	b, r, err := launchdCurrent(ctx, env, p)
	if err != nil {
		return nil, err
	}
	if !bytesEqual(b, p.Candidate) && !bytesEqual(b, p.Original) {
		return nil, fmt.Errorf("foreign plist blocks rollback")
	}
	if r.OverrideState != "enabled" && r.OverrideState != "disabled" {
		return nil, fmt.Errorf("foreign override blocks rollback")
	}
	var names []string
	if r.LoadedState == "loaded" {
		var started launchdPhaseStart
		if readLaunchdArtifact(home, launchdPhaseStem(p, "apply", 2)+".started.json", &started) != nil || !launchdServiceOwned(p, r) {
			return nil, fmt.Errorf("loaded service not owned by this plan; no rollback effects")
		}
		names = append(names, "bootout")
	}
	if r.OverrideState == "enabled" {
		names = append(names, "disable")
	}
	if bytesEqual(b, p.Candidate) && !bytesEqual(p.Candidate, p.Original) {
		names = append(names, "restore")
	}
	if p.Handoff != nil {
		names = append(names, "restore_owner")
	}
	manifest.Names = names
	manifest.Anchor = anchor
	if err = writeLaunchdArtifact(home, name, manifest); err != nil {
		return nil, err
	}
	return names, nil
}
func launchdRollbackAnchor(home string, p LaunchdRepairPlan) (string, string, error) {
	var commits []launchdPhaseCommit
	indices := []int{0, 1, 2}
	if p.Handoff != nil {
		indices = append([]int{3}, indices...)
	}
	for _, i := range indices {
		var c launchdPhaseCommit
		err := readLaunchdArtifact(home, launchdPhaseStem(p, "apply", i)+".committed.json", &c)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", "", err
		}
		commits = append(commits, c)
	}
	if len(commits) == 0 {
		return "", "", fmt.Errorf("forward phase must be reconciled to a native receipt before rollback")
	}
	return launchdDigest(commits), commits[len(commits)-1].WorkSHA256, nil
}

type launchdNativePhaseOutputs struct {
	Action     envelope.ActionIntent   `json:"action"`
	Decision   envelope.PolicyDecision `json:"decision"`
	Receipt    envelope.Receipt        `json:"receipt"`
	Transition envelope.Transition     `json:"transition"`
	Event      work.Event              `json:"event"`
}

func launchdNativePhaseEvent(store *work.Store, p LaunchdRepairPlan, start launchdPhaseStart, decision envelope.PolicyDecision, receiptID, transitionID string) (work.Event, error) {
	_, events, err := store.Show(p.WorkID)
	if err != nil {
		return work.Event{}, err
	}
	refs := []string{start.Pending.Action.ID, decision.ID, receiptID, transitionID}
	var found work.Event
	count := 0
	for _, event := range events {
		touches := false
		for _, ref := range event.Attached {
			for _, wanted := range refs {
				touches = touches || ref == wanted
			}
		}
		if !touches {
			continue
		}
		if event.WorkID != p.WorkID || event.From != start.BeforeWork.State || event.To != start.BeforeWork.State || event.By != start.Pending.Action.Actor || event.Note != "receipted action recorded: "+start.Pending.Action.Action+" (policy "+decision.Outcome+")" || !reflect.DeepEqual(event.Attached, refs) {
			return found, fmt.Errorf("native phase lifecycle event conflicts")
		}
		found = event
		count++
	}
	if count != 1 {
		return found, fmt.Errorf("native phase requires exactly one owned lifecycle event")
	}
	return found, nil
}

// A historical phase never seals or replays a work reservation. A newer phase
// may own that slot; authenticate the already-published immutable native tuple
// through owned read APIs instead of touching its successor's reservation.
func verifyLaunchdCommittedPhase(home string, p LaunchdRepairPlan, start launchdPhaseStart, outcome launchdPhaseOutcome, commit launchdPhaseCommit) error {
	startCopy := start
	startCopy.Digest = ""
	outCopy := outcome
	outCopy.Digest = ""
	if start.PlanID != p.ID || start.Digest != launchdDigest(startCopy) || outcome.StartDigest != start.Digest || outcome.Digest != launchdDigest(outCopy) || commit.StartDigest != start.Digest || commit.OutcomeDigest != outcome.Digest || commit.Outcome != outcome.Final.Outcome || start.Pending.PreEvaluatedDecision == nil {
		return fmt.Errorf("committed phase evidence digest mismatch")
	}
	expected := launchdExpectedWork(start.BeforeWork, start.Pending.Action, *start.Pending.PreEvaluatedDecision)
	if commit.WorkSHA256 != launchdWorkDigest(expected) {
		return fmt.Errorf("committed phase generation mismatch")
	}
	events, err := eventstore.Open(home)
	if err != nil {
		return err
	}
	var outputs launchdNativePhaseOutputs
	for _, item := range []struct {
		id     string
		target any
	}{{start.Pending.Action.ID, &outputs.Action}, {start.Pending.PreEvaluatedDecision.ID, &outputs.Decision}, {commit.ReceiptID, &outputs.Receipt}, {commit.TransitionID, &outputs.Transition}} {
		record, err := events.Read(item.id)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(record.Bytes, item.target); err != nil {
			return err
		}
	}
	if !reflect.DeepEqual(outputs.Action, start.Pending.Action) || !reflect.DeepEqual(outputs.Decision, *start.Pending.PreEvaluatedDecision) {
		return fmt.Errorf("native phase authority tuple changed")
	}
	if err = outputs.Action.Validate(); err != nil {
		return err
	}
	if err = outputs.Decision.Validate(); err != nil {
		return err
	}
	if err = outputs.Receipt.Validate(); err != nil {
		return err
	}
	if outputs.Receipt.Outcome != outcome.Final.Outcome || outputs.Receipt.MoveType != outcome.Final.MoveType || outputs.Receipt.StateDelta.Changed != outcome.Final.Changed || outputs.Receipt.StateDelta.Summary != outcome.Final.Summary || !reflect.DeepEqual(outputs.Receipt.FailureCause, outcome.Final.FailureCause) {
		return fmt.Errorf("native receipt does not bind phase outcome")
	}
	for _, proof := range []string{"launchd-phase-start:sha256:" + start.Digest, "launchd-phase-observed:sha256:" + launchdObservedPhaseDigest(outcome)} {
		found := false
		for _, ref := range outputs.Receipt.ProofRefs {
			found = found || ref == proof
		}
		if !found {
			return fmt.Errorf("native receipt lacks exact phase evidence")
		}
	}

	if err = outputs.Transition.Validate(); err != nil {
		return err
	}
	readOnly, err := work.OpenReadOnly(home)
	if err != nil {
		return err
	}
	current, err := readOnly.Get(p.WorkID)
	if err != nil {
		return err
	}
	linked := false
	for _, id := range current.ActionIntentIDs {
		linked = linked || id == outputs.Action.ID
	}
	if !linked {
		return fmt.Errorf("native phase is no longer linked from work")
	}
	outputs.Event, err = launchdNativePhaseEvent(readOnly, p, start, outputs.Decision, commit.ReceiptID, commit.TransitionID)
	if err != nil {
		return err
	}
	if commit.NativeDigest == "" || launchdDigest(outputs) != commit.NativeDigest {
		return fmt.Errorf("committed native phase outputs changed")
	}
	return nil
}

func launchdObservedPhaseDigest(out launchdPhaseOutcome) string {
	return launchdDigest(struct {
		StartDigest  string                 `json:"start_digest"`
		Runtime      LaunchdRuntime         `json:"runtime"`
		FileSHA256   string                 `json:"file_sha256"`
		OwnerSHA256  string                 `json:"owner_sha256,omitempty"`
		Failure      string                 `json:"failure"`
		Outcome      string                 `json:"outcome"`
		Changed      bool                   `json:"changed"`
		Summary      string                 `json:"summary"`
		FailureCause *envelope.FailureCause `json:"failure_cause"`
	}{out.StartDigest, out.Runtime, out.FileSHA256, out.OwnerSHA256, out.Failure, out.Final.Outcome, out.Final.Changed, out.Final.Summary, out.Final.FailureCause})
}
