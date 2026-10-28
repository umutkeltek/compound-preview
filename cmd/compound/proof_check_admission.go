package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/work"
)

const (
	defaultProofCheckReservationTTL = 45 * time.Minute
	// A refresh persists governed preparation before extending the scarce lease.
	// Keep the public floor above ordinary fsync/admission contention so a supported
	// custom window does not expire while its unchanged holder is proving authority.
	minimumProofCheckReservationTTL = 30 * time.Second
)

type proofCheckAdmissionInput struct {
	Context                context.Context
	HomePath               string
	WorkID                 string
	Actor                  string
	Workspace              string
	Command                string
	Profile                work.ProofResourceProfile
	ReportPath             string
	ReportCustody          *proofCheckOutCustody
	TTL                    time.Duration
	Steal                  bool
	StealReason            string
	TerminatedRunAttemptID string
	TerminationEvidenceRef string
	Wait                   time.Duration
	WaitProgress           io.Writer
	Now                    func() time.Time
	// ExecutionBudget bounds the active operation of this governed proof run.
	// It is consumed by pre-admission preparation and by the admitted run; the
	// queue wait below never touches it. Nil keeps the unbounded behavior.
	ExecutionBudget *work.ProofExecutionBudget
}

type proofCheckRunner func() (proofCheckReport, error)
type proofCheckContextRunner func(context.Context) (proofCheckReport, error)

// proofCheckBeforeRetainedOutMatch is a deterministic race-test boundary after
// initial custody validation and before the no-write recovery shortcut.
var proofCheckBeforeRetainedOutMatch = func(string) {}

var proofCheckLegacyRecoverer = func(
	homePath string,
	workID string,
	in work.ProofResourceLegacyRecoveryInput,
	now time.Time,
) (work.ProofResourceLegacyRecoveryResult, bool, error) {
	store, err := work.Open(homePath)
	if err != nil {
		return work.ProofResourceLegacyRecoveryResult{}, false, err
	}
	return store.RecoverLegacyProofResourceCompletion(workID, in, now)
}

// proofCheckAfterRunnerContextCancel is a deterministic test boundary before barrier release.
var proofCheckAfterRunnerContextCancel = func() {}

func proofCheckNeedsAdmission(dryRun, ciOnly, ciConsole bool) bool {
	return !dryRun && !ciOnly && !ciConsole
}

// runProofCheckWithAdmission is the canonical full-battery process boundary. ReserveProofResource
// must return before runner is called, so a conflicting request cannot enter runProofCheck or start
// even its first gate subprocess. Before reserving, an exact retained completion is resumed through
// its existing governed reservation and owned report without re-entering runner. A newly-produced
// report is placed in immutable Compound custody and finalized while the exact lease remains held.
func runProofCheckWithAdmission(
	in proofCheckAdmissionInput,
	runner proofCheckRunner,
) (proofCheckReport, work.ProofResourceCompleteResult, error) {
	if runner == nil {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, fmt.Errorf("proof-check runner required")
	}
	return runProofCheckWithAdmissionContext(in, func(context.Context) (proofCheckReport, error) {
		return runner()
	})
}

func runProofCheckWithAdmissionContext(
	in proofCheckAdmissionInput,
	runner proofCheckContextRunner,
) (proofCheckReport, work.ProofResourceCompleteResult, error) {
	preparationStarted := time.Now()
	preparationContext, cancelPreparation := in.ExecutionBudget.PreparationContext(in.Context)
	defer cancelPreparation()
	preparationContext = gitrepository.ContextWithCommandFactory(preparationContext, proofCheckCommandContext)
	if runner == nil {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, fmt.Errorf("proof-check runner required")
	}
	in.HomePath = strings.TrimSpace(in.HomePath)
	in.WorkID = strings.TrimSpace(in.WorkID)
	in.Actor = strings.TrimSpace(in.Actor)
	in.Workspace = strings.TrimSpace(in.Workspace)
	in.Command = strings.TrimSpace(in.Command)
	in.ReportPath = strings.TrimSpace(in.ReportPath)
	if in.ReportPath != "" && in.ReportCustody == nil {
		custody, err := proofCheckCaptureOutPath(in.ReportPath)
		if err != nil {
			return proofCheckReport{}, work.ProofResourceCompleteResult{}, err
		}
		in.ReportCustody = &custody
	}
	if in.ReportCustody != nil {
		defer in.ReportCustody.close()
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	if in.TTL <= 0 {
		in.TTL = defaultProofCheckReservationTTL
	}
	if in.TTL < minimumProofCheckReservationTTL {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, fmt.Errorf(
			"proof-check reservation TTL %s is unsafe; minimum is %s",
			in.TTL,
			minimumProofCheckReservationTTL,
		)
	}
	if in.Profile.SourceSnapshot != nil {
		if err := gitrepository.RevalidateSnapshotContext(preparationContext, in.Workspace, *in.Profile.SourceSnapshot); err != nil {
			return proofCheckReport{}, work.ProofResourceCompleteResult{}, err
		}
	}
	store, err := work.Open(in.HomePath)
	if err != nil {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, err
	}
	var preparedReport proofCheckReport
	resumed, found, resumeErr := store.ResumeProofResourceCompletion(in.WorkID, work.ProofResourceResumeInput{
		Actor:     in.Actor,
		Workspace: in.Workspace,
		Command:   in.Command,
		Profile:   in.Profile,
		PrepareOwnedReport: func(result work.ProofResourceResult) error {
			report, prepareErr := restoreOwnedProofCheckReport(
				in.HomePath,
				result.ReportArtifactRef,
				in.ReportPath,
				in.ReportCustody,
			)
			preparedReport = report
			return prepareErr
		},
	}, in.Now())
	if found {
		priorReport := preparedReport
		if resumed.Result.ReportArtifactRef != "" && priorReport.SchemaVersion == "" {
			priorReport, err = readOwnedProofCheckReport(in.HomePath, resumed.Result.ReportArtifactRef)
			if err != nil {
				return proofCheckReport{}, resumed, errors.Join(resumeErr, err)
			}
		}
		if resumed.Result.ReportArtifactRef != "" {
			if priorReport.Status != resumed.Result.ReportStatus {
				return proofCheckReport{}, resumed, errors.Join(
					resumeErr,
					fmt.Errorf(
						"resumed owned report status %q does not match governed result %q",
						priorReport.Status,
						resumed.Result.ReportStatus,
					),
				)
			}
		}
		if resumeErr != nil {
			return priorReport, resumed, resumeErr
		}
		if resumed.Result.ReportArtifactRef == "" {
			return proofCheckReport{}, resumed, fmt.Errorf(
				"resumed prior proof failure without an owned report: %s",
				resumed.Result.FailureReason,
			)
		}
		return priorReport, resumed, nil
	}
	if resumeErr != nil {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, resumeErr
	}
	if in.ReportCustody != nil {
		if err := in.ReportCustody.probeWriteCapability(); err != nil {
			return proofCheckReport{}, work.ProofResourceCompleteResult{}, err
		}
	}
	in.ExecutionBudget.Charge(time.Since(preparationStarted))
	cancelPreparation()
	if in.Context != nil && in.Context.Err() != nil {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, context.Cause(in.Context)
	}
	if in.ExecutionBudget != nil && in.ExecutionBudget.Remaining() <= 0 {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, in.ExecutionBudget.ExpiredError(work.ProofBudgetPhasePreparation)
	}
	reserveInput := work.ProofResourceReserveInput{
		Actor:                  in.Actor,
		TTL:                    in.TTL,
		Workspace:              in.Workspace,
		Command:                in.Command,
		Profile:                in.Profile,
		Steal:                  in.Steal,
		StealReason:            in.StealReason,
		TerminatedRunAttemptID: in.TerminatedRunAttemptID,
		TerminationEvidenceRef: in.TerminationEvidenceRef,
	}
	var reserved work.ProofResourceReserveResult
	if in.Wait > 0 {
		reserved, err = store.ReserveProofResourceWhenFree(in.Context, in.WorkID, reserveInput, in.Wait, landlease.RealClock{}, in.WaitProgress)
	} else {
		reserved, err = store.ReserveProofResource(in.WorkID, reserveInput, in.Now())
	}
	if err != nil {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, err
	}

	if reserved.ExecutionBarrier == nil {
		return proofCheckReport{}, work.ProofResourceCompleteResult{}, fmt.Errorf("proof reservation did not return execution barrier")
	}
	defer reserved.ExecutionBarrier.Close()
	parentContext := in.Context
	if parentContext == nil {
		parentContext = context.Background()
	}
	runnerContext, cancelRunner := context.WithCancel(
		proofCheckContextWithExecutionBarrier(parentContext, reserved.ExecutionBarrier),
	)
	defer cancelRunner()
	stopHeartbeat, heartbeatErr := startProofCheckReservationHeartbeat(
		store,
		in,
		reserved.Reservation,
		cancelRunner,
	)
	runnerContext = work.ContextWithHomeFixtureReuse(runnerContext, in.HomePath, in.WorkID, in.Profile.SourceSnapshot, in.Workspace)
	runnerContext = work.ContextWithRemoteProof(runnerContext, in.HomePath, reserved.Reservation, proofCheckCommandContext)
	// The bounded execution budget starts HERE, after the queue wait for the
	// scarce proof resource: waiting is not active operation, so it must not
	// consume the budget. What preparation already consumed does, so the
	// admitted run continues with the remainder rather than a fresh full
	// budget.
	var report proofCheckReport
	budgetContext, cancelBudget, runErr := in.ExecutionBudget.RunContext(runnerContext)
	defer cancelBudget()
	runnerContext = gitrepository.ContextWithCommandFactory(budgetContext, proofCheckCommandContext)
	if runErr == nil {
		report, runErr = runner(runnerContext)
	}
	if cause := work.ProofExecutionBudgetCause(runnerContext); cause != nil {
		runErr = errors.Join(runErr, cause)
	}

	stopHeartbeat()
	if hbErr := heartbeatErr(); hbErr != nil {
		if runErr == nil {
			runErr = fmt.Errorf("proof resource heartbeat: %w", hbErr)
		} else {
			runErr = errors.Join(runErr, fmt.Errorf("proof resource heartbeat: %w", hbErr))
		}
	}
	if runErr == nil && in.Profile.SourceSnapshot != nil {
		runErr = gitrepository.RevalidateSnapshotContext(runnerContext, in.Workspace, *in.Profile.SourceSnapshot)
	}
	if cause := work.ProofExecutionBudgetCause(runnerContext); cause != nil {
		runErr = errors.Join(runErr, cause)
	}
	if runnerContext.Err() != nil && runErr == nil {
		runErr = context.Cause(runnerContext)
	}
	if cause := work.ProofExecutionBudgetCause(runnerContext); cause != nil && report.Status != "failed" {
		var publishErr error
		report, publishErr = proofCheckPublishDeadlineReport(in.ReportPath, report, in.ReportCustody, cause)
		runErr = errors.Join(runErr, publishErr)
	}
	// Stop every context-bound gate start before dropping the supervisor's execution-barrier
	// descriptor. A runner may have returned while one of its goroutines is still queued to spawn;
	// cancellation makes exec.CommandContext refuse that late start instead of letting it escape
	// after completion has attested the process tree clear.
	cancelRunner()
	proofCheckAfterRunnerContextCancel()
	// The supervisor's descriptor must close before governed completion starts.
	// Any background descendant that escaped its gate process still owns an
	// inherited copy, so the Work finalizer can attest the whole process tree
	// terminated while the exact lease holder remains authoritative.
	if barrierErr := reserved.ExecutionBarrier.Close(); barrierErr != nil {
		if runErr == nil {
			runErr = fmt.Errorf("close proof execution barrier: %w", barrierErr)
		} else {
			runErr = errors.Join(runErr, fmt.Errorf("close proof execution barrier: %w", barrierErr))
		}
	}

	if runErr != nil {
		completed, completeErr := store.CompleteProofResource(in.WorkID, work.ProofResourceCompleteInput{
			Actor:         in.Actor,
			ReservationID: reserved.Reservation.ID,
			FailureReason: runErr.Error(),
		}, in.Now())
		if completeErr != nil {
			return report, completed, errors.Join(runErr, completeErr)
		}
		return report, completed, runErr
	}

	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		failed, failErr := failProofCheckAdmission(store, in, reserved.Reservation.ID, err)
		return report, failed, failErr
	}
	body = append(body, '\n')
	if in.ReportPath != "" {
		ownedBytes, readErr := proofCheckReadPublishedReport(in.ReportPath)
		if readErr != nil {
			failed, failErr := failProofCheckAdmission(store, in, reserved.Reservation.ID, readErr)
			return report, failed, failErr
		}
		if !bytes.Equal(ownedBytes, body) {
			cause := fmt.Errorf("owned --out report differs from internally produced report")
			failed, failErr := failProofCheckAdmission(store, in, reserved.Reservation.ID, cause)
			return report, failed, failErr
		}
		body = ownedBytes
	}
	artifacts, err := home.NewArtifactStore(in.HomePath)
	if err != nil {
		failed, failErr := failProofCheckAdmission(store, in, reserved.Reservation.ID, err)
		return report, failed, failErr
	}
	reportRef, err := artifacts.Put(body)
	if err != nil {
		failed, failErr := failProofCheckAdmission(store, in, reserved.Reservation.ID, err)
		return report, failed, failErr
	}
	exitCode := 0
	if report.Status != "ok" {
		exitCode = 1
	}
	completed, err := store.CompleteProofResource(in.WorkID, work.ProofResourceCompleteInput{
		Actor:             in.Actor,
		ReservationID:     reserved.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  exitCode,
	}, in.Now())
	return report, completed, err
}

func restoreOwnedProofCheckReport(
	homePath,
	artifactRef,
	reportPath string,
	custody *proofCheckOutCustody,
) (proofCheckReport, error) {
	artifactRef = strings.TrimSpace(artifactRef)
	reportPath = strings.TrimSpace(reportPath)
	if artifactRef == "" || reportPath == "" {
		return proofCheckReport{}, fmt.Errorf("owned proof artifact and --out path required")
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		return proofCheckReport{}, err
	}
	body, err := artifacts.Get(artifactRef)
	if err != nil {
		return proofCheckReport{}, fmt.Errorf("read retained proof report: %w", err)
	}
	report, err := decodeOwnedProofCheckReport(body)
	if err != nil {
		return proofCheckReport{}, err
	}
	if custody == nil {
		return proofCheckReport{}, fmt.Errorf("owned proof --out custody required")
	}
	if err := custody.revalidate(reportPath); err != nil {
		return proofCheckReport{}, err
	}
	proofCheckBeforeRetainedOutMatch(reportPath)
	restoreCustody := *custody
	if custody.existed {
		currentCustody, err := custody.snapshotCurrentTarget(reportPath)
		if err != nil {
			return report, fmt.Errorf("verify retained --out before recovery: %w", err)
		}
		if bytes.Equal(currentCustody.targetData, body) {
			return report, nil
		}
		// The exact-match shortcut compared the fresh bytes before this snapshot
		// becomes the expected CAS preimage for a safe restoration.
		restoreCustody = currentCustody
	}
	if err := restoreCustody.probeWriteCapability(); err != nil {
		return report, fmt.Errorf("restore retained --out preflight: %w", err)
	}
	if err := proofCheckWriteReportBytesWithCustody(reportPath, body, restoreCustody); err != nil {
		return report, fmt.Errorf("restore retained --out: %w", err)
	}
	published, err := proofCheckReadPublishedReport(reportPath)
	if err != nil {
		return report, fmt.Errorf("verify restored --out: %w", err)
	}
	if !bytes.Equal(published, body) {
		return report, fmt.Errorf("verify restored --out: published bytes differ from immutable artifact")
	}
	return report, nil
}

// proofCheckRestoreRetainedReport restores a retained owned report through the existing --out custody.
func proofCheckRestoreRetainedReport(homePath, artifactRef, outPath string) (proofCheckReport, error) {
	custody, err := proofCheckCaptureOutPath(outPath)
	if err != nil {
		return proofCheckReport{}, err
	}
	defer custody.close()
	return restoreOwnedProofCheckReport(homePath, artifactRef, outPath, &custody)
}

func readOwnedProofCheckReport(homePath, artifactRef string) (proofCheckReport, error) {
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		return proofCheckReport{}, err
	}
	body, err := artifacts.Get(strings.TrimSpace(artifactRef))
	if err != nil {
		return proofCheckReport{}, fmt.Errorf("read retained proof report: %w", err)
	}
	return decodeOwnedProofCheckReport(body)
}

func decodeOwnedProofCheckReport(body []byte) (proofCheckReport, error) {
	var report proofCheckReport
	if err := json.Unmarshal(body, &report); err != nil {
		return proofCheckReport{}, fmt.Errorf("decode retained proof report: %w", err)
	}
	if !work.IsAcceptedProofCheckSchemaVersion(report.SchemaVersion, work.ProofCheckSchemaVersion) {
		return proofCheckReport{}, fmt.Errorf(
			"retained proof report schema_version %q is unsupported (current %q)",
			report.SchemaVersion,
			work.ProofCheckSchemaVersion,
		)
	}
	return report, nil
}

func failProofCheckAdmission(
	store *work.Store,
	in proofCheckAdmissionInput,
	reservationID string,
	cause error,
) (work.ProofResourceCompleteResult, error) {
	completed, completeErr := store.CompleteProofResource(in.WorkID, work.ProofResourceCompleteInput{
		Actor:         in.Actor,
		ReservationID: reservationID,
		FailureReason: cause.Error(),
	}, in.Now())
	if completeErr != nil {
		return completed, errors.Join(cause, completeErr)
	}
	return completed, cause
}

func startProofCheckReservationHeartbeat(
	store *work.Store,
	in proofCheckAdmissionInput,
	reservation work.ProofResourceReservation,
	cancelRunner context.CancelFunc,
) (stop func(), result func() error) {
	interval := in.TTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var once sync.Once
	var heartbeatErr error
	go func() {
		defer close(doneCh)
		refreshTicker := time.NewTicker(interval)
		defer refreshTicker.Stop()
		holderTicker := time.NewTicker(250 * time.Millisecond)
		defer holderTicker.Stop()
		fail := func(err error) {
			heartbeatErr = err
			if cancelRunner != nil {
				cancelRunner()
			}
		}
		for {
			select {
			case <-refreshTicker.C:
				if _, err := store.RefreshProofResource(in.WorkID, reservation.ID, in.Actor, in.TTL, in.Now()); err != nil {
					fail(err)
					return
				}
			case <-holderTicker.C:
				status, err := landlease.ProofResourceForIdentity(in.HomePath, reservation.ProofCoordination).Status(in.Now())
				if err != nil {
					fail(err)
					return
				}
				if status.Holder == reservation.Holder && status.TerminationRequestedBy != "" {
					fail(fmt.Errorf(
						"proof reservation %s received governed termination request from %q: %s",
						reservation.ID,
						status.TerminationRequestedBy,
						status.TerminationRequestReason,
					))
					return
				}
				if !status.Held || status.Holder != reservation.Holder {
					fail(fmt.Errorf(
						"proof reservation %s lost exclusive holder %q; current holder %q",
						reservation.ID,
						reservation.Holder,
						status.Holder,
					))
					return
				}
			case <-stopCh:
				return
			}
		}
	}()
	stop = func() {
		once.Do(func() {
			close(stopCh)
			<-doneCh
		})
	}
	result = func() error {
		stop()
		return heartbeatErr
	}
	return stop, result
}
