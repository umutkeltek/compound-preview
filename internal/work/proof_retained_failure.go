package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
)

// RetainedFailedProofRecoveryHelp documents the recovery-only route that finishes an
// authentic retained failed proof after its source checkout moved or disappeared.
const RetainedFailedProofRecoveryHelp = "  --resume-retained PFR --work W --actor A --expected-pending-sha256 SHA --out FILE  finish an exact retained failed proof through its own reservation; never admits a battery"

// ProofRecoveryHelp documents every proof recovery-only route in one place.
const ProofRecoveryHelp = QuiescentProofRecoveryHelp + "\n" + RetainedFailedProofRecoveryHelp

// proofResourceAfterRetainedAdmission is a deterministic test boundary between the retained
// admission fence and the protected native mutation.
var proofResourceAfterRetainedAdmission = func(reservationID string) {}

// ProofResourceRetainedFailureInput identifies the exact retained failed proof completion a
// recovery-only route may finish. It carries no workspace, command or profile: those are read
// from the persisted reservation, never from the current checkout.
type ProofResourceRetainedFailureInput struct {
	Actor                 string
	ReservationID         string
	ExpectedPendingSHA256 string
	// PrepareOwnedReport is the existing projection-publication barrier. The retained report is
	// restored through it before the exclusive holder may be released.
	PrepareOwnedReport func(ProofResourceResult) error
}

// RetainedFailedProofOutcome is the truthful outcome of a retained failed proof recovery.
// It reports an audit completion, never a certification: VerificationRef stays empty and the
// original failed report, attempts and refusals are preserved.
type RetainedFailedProofOutcome struct {
	ReservationID     string                `json:"reservation_id"`
	WorkID            string                `json:"work_id"`
	Actor             string                `json:"actor"`
	ReportArtifactRef string                `json:"report_artifact_ref"`
	ReportStatus      string                `json:"report_status"`
	ProducerExitCode  int                   `json:"producer_exit_code"`
	VerificationRef   string                `json:"verification_ref,omitempty"`
	ResourceReleased  bool                  `json:"resource_released"`
	Work              envelope.WorkEnvelope `json:"work"`
}

// ResumeRetainedFailedProofCompletion finishes an authentic retained failed proof through the
// ordinary native completion machinery. It reads the persisted reservation for the original
// workspace, command and sealed profile, never the current checkout; it never admits a battery,
// never starts a gate subprocess, and never certifies the retained report.
//
// found=false means no retained completion matches this reservation: the caller must not fall
// back to fresh admission.
func (s *Store) ResumeRetainedFailedProofCompletion(
	workID string,
	in ProofResourceRetainedFailureInput,
	now time.Time,
) (RetainedFailedProofOutcome, bool, error) {
	workID = strings.TrimSpace(workID)
	in.Actor = strings.TrimSpace(in.Actor)
	in.ReservationID = strings.TrimSpace(in.ReservationID)
	in.ExpectedPendingSHA256 = strings.ToLower(strings.TrimSpace(in.ExpectedPendingSHA256))
	if workID == "" || in.Actor == "" || in.ReservationID == "" || in.ExpectedPendingSHA256 == "" {
		return RetainedFailedProofOutcome{}, false, fmt.Errorf(
			"retained failed proof recovery requires exact work, actor, reservation and expected pending sha256",
		)
	}
	if len(in.ExpectedPendingSHA256) != 64 || strings.Trim(in.ExpectedPendingSHA256, "0123456789abcdef") != "" {
		return RetainedFailedProofOutcome{}, false, fmt.Errorf("expected pending sha256 must be 64 lowercase hex characters")
	}
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(in.ReservationID, &reservation); err != nil {
		return RetainedFailedProofOutcome{}, false, fmt.Errorf("read retained proof reservation %s: %w", in.ReservationID, err)
	}
	if reservation.WorkID != workID || reservation.Actor != in.Actor {
		return RetainedFailedProofOutcome{}, false, fmt.Errorf(
			"retained proof reservation %s does not belong to work %q and actor %q",
			in.ReservationID,
			workID,
			in.Actor,
		)
	}
	if reservation.Status != ProofResourceAcquired {
		return RetainedFailedProofOutcome{}, false, fmt.Errorf(
			"retained proof reservation %s is not an acquired reservation",
			in.ReservationID,
		)
	}
	pending, present, _, err := s.readProofResourceCompletionPending(reservation.ID)
	if err != nil {
		return RetainedFailedProofOutcome{}, false, err
	}
	if !present {
		// The completion journal is gone. There is no retained completion to finish: the route
		// refuses safely. It never recreates consumed evidence, never writes an output, and
		// never admits a battery in its place.
		return RetainedFailedProofOutcome{}, false, nil
	}
	if err := s.verifyRetainedPendingSHA256(reservation.ID, in.ExpectedPendingSHA256); err != nil {
		return RetainedFailedProofOutcome{}, false, err
	}
	proofResourceAfterRetainedAdmission(reservation.ID)
	if err := authenticateRetainedFailedProofResult(workID, in.Actor, reservation, pending); err != nil {
		return RetainedFailedProofOutcome{}, false, err
	}
	completeInput := ProofResourceCompleteInput{
		Actor:             in.Actor,
		ReservationID:     reservation.ID,
		ReportArtifactRef: pending.Result.ReportArtifactRef,
		ProducerExitKnown: pending.Result.ProducerExitKnown,
		ProducerExitCode:  pending.Result.ProducerExitCode,
		FailureReason:     pending.Result.FailureReason,
	}
	if _, err := s.deriveProofResourceCompletionExpectation(workID, reservation, completeInput); err != nil {
		return RetainedFailedProofOutcome{}, false, fmt.Errorf(
			"retained failed proof report is not authentic for its reservation: %w",
			err,
		)
	}
	resumed, found, err := s.ResumeProofResourceCompletion(workID, ProofResourceResumeInput{
		Actor:                 in.Actor,
		Workspace:             reservation.Workspace,
		Command:               reservation.Command,
		Profile:               reservation.Profile,
		PrepareOwnedReport:    in.PrepareOwnedReport,
		ReservationID:         reservation.ID,
		ExpectedPendingSHA256: in.ExpectedPendingSHA256,
		RequireFailedResult:   true,
	}, now)
	if !found {
		if err != nil {
			return RetainedFailedProofOutcome{}, false, err
		}
		return RetainedFailedProofOutcome{}, false, nil
	}
	outcome := RetainedFailedProofOutcome{
		ReservationID:     reservation.ID,
		WorkID:            workID,
		Actor:             in.Actor,
		ReportArtifactRef: resumed.Result.ReportArtifactRef,
		ReportStatus:      resumed.Result.ReportStatus,
		ProducerExitCode:  resumed.Result.ProducerExitCode,
		VerificationRef:   resumed.VerificationRef,
		ResourceReleased:  resumed.ResourceReleased,
		Work:              resumed.Work,
	}
	if err != nil {
		return outcome, true, err
	}
	if resumed.Result.Status != ProofResourceFailed {
		return outcome, true, fmt.Errorf("retained failed proof recovery produced a %q result", resumed.Result.Status)
	}
	if resumed.VerificationRef != "" {
		return outcome, true, fmt.Errorf("retained failed proof recovery produced a verification reference")
	}
	return outcome, true, nil
}

// authenticateRetainedFailedProofResult accepts only an owned, exact failed result for this
// reservation. A successful, unknown, inconsistent, unowned or verification-bearing result can
// never use the dedicated failure route.
func authenticateRetainedFailedProofResult(
	workID, actor string,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
) error {
	result := pending.Result
	switch {
	case result.WorkID != workID || result.Actor != actor:
		return fmt.Errorf("retained proof result does not belong to the exact work and actor")
	case result.ReservationID != reservation.ID:
		return fmt.Errorf("retained proof result does not belong to reservation %s", reservation.ID)
	case result.Status != ProofResourceFailed:
		return fmt.Errorf("retained proof result status %q is not a failure", result.Status)
	case result.RunAttemptStatus != runattempt.StatusFailed:
		return fmt.Errorf("retained proof result attempt status %q is not a terminal failure", result.RunAttemptStatus)
	case !result.OwnedReport:
		return fmt.Errorf("retained proof result does not carry an owned report")
	case result.ReportArtifactRef == "":
		return fmt.Errorf("retained proof result has no owned report artifact")
	case result.ReportStatus != "failed":
		return fmt.Errorf("retained proof result report status %q is not a failed battery verdict", result.ReportStatus)
	case !result.ProducerExitKnown:
		return fmt.Errorf("retained proof result producer exit is unknown")
	case result.ProducerExitCode == 0:
		return fmt.Errorf("retained proof result producer exit was zero")
	case result.VerificationRef != "" || pending.VerificationRef != "":
		return fmt.Errorf("retained proof result carries a verification reference")
	case pending.TerminalStatus != runattempt.StatusFailed:
		return fmt.Errorf("retained proof completion terminal status %q is not a failure", pending.TerminalStatus)
	}
	return nil
}

// requireRetainedFailedProofGovernanceSeal refuses a dedicated failure route whose retained
// completion journal carries NO governance seal. The ordinary legacy reconstruction of a missing
// seal stays untouched for every other path; the dedicated failure route never reconstructs one,
// because a reconstructed seal would authenticate governance this recovery never reviewed.
//
// It runs under the lifecycle mutation guard, before the scarce mutation, the rearm and the owned
// report restoration, so a journal without a seal can never reach a recovery effect.
func (s *Store) requireRetainedFailedProofGovernanceSeal(reservationID string) error {
	pending, present, _, err := s.readProofResourceCompletionPending(reservationID)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("retained failed proof completion journal %s is gone", reservationID)
	}
	if pending.GovernanceSeal == "" {
		return fmt.Errorf("retained failed proof recovery requires an existing completion governance seal")
	}
	return nil
}

// retainedFailedProofAuthentication is the READ-ONLY authentication of a retained failed proof
// completion. It carries everything the dedicated route needs before it may write anything.
type retainedFailedProofAuthentication struct {
	Work     envelope.WorkEnvelope
	Pending  proofResourceCompletionPending
	Chain    []runattempt.RunAttempt
	Terminal runattempt.RunAttempt
}

// authenticateRetainedFailedProofCompletionLocked runs the FULL native validation of the retained
// completion and the native execution/remote quiescence probe under the lifecycle mutation guard,
// BEFORE any effect of the recovery.
//
// It is READ-ONLY: a refusal here leaves no native effect behind, and an expired holder is
// refused before its rearm. It shares the ordinary native validator instead of a second set of
// hand-written partial checks: the real current Work, the real retained RunAttempt and the derived
// completion expectation decide the verdict. The dedicated route adds exactly three extra rules —
// an existing governance seal, the current Work lease owner, and the full retry ancestry — and one
// extra probe — native execution and remote fence quiescence.
func (s *Store) authenticateRetainedFailedProofCompletionLocked(
	workID string,
	reservation ProofResourceReservation,
	completeInput ProofResourceCompleteInput,
) (retainedFailedProofAuthentication, error) {
	var currentReservation ProofResourceReservation
	if err := s.readProofReservationInto(reservation.ID, &currentReservation); err != nil {
		return retainedFailedProofAuthentication{}, err
	}
	currentPending, present, _, err := s.readProofResourceCompletionPending(reservation.ID)
	if err != nil {
		return retainedFailedProofAuthentication{}, err
	}
	if !present {
		return retainedFailedProofAuthentication{}, fmt.Errorf("retained failed proof completion journal %s is gone", reservation.ID)
	}
	if currentPending.GovernanceSeal == "" {
		return retainedFailedProofAuthentication{}, fmt.Errorf("retained failed proof recovery requires an existing completion governance seal")
	}
	// The Work lease owner is a native fact, not an operator claim: only the actor that
	// currently owns the Work lease may finish a retained completion. The ordinary native
	// completion rule owns that check; this route runs it under the SAME effects guard, before
	// the owned report is restored, so a foreign lease owner never reaches an output.
	if err := s.requireProofWorkLeaseOwner(workID, completeInput.Actor, "retained failed proof recovery"); err != nil {
		return retainedFailedProofAuthentication{}, err
	}
	w, err := s.Get(workID)
	if err != nil {
		return retainedFailedProofAuthentication{}, err
	}
	ra, err := s.runAttempts().Get(currentPending.Result.RunAttemptID)
	if err != nil {
		return retainedFailedProofAuthentication{}, fmt.Errorf("retained failed proof attempt cannot be read: %w", err)
	}
	expect, err := s.deriveProofResourceCompletionExpectation(workID, currentReservation, completeInput)
	if err != nil {
		return retainedFailedProofAuthentication{}, err
	}
	_, migrated, err := s.validateAndNormalizeProofResourceCompletionPending(
		w,
		currentReservation,
		currentPending,
		ra,
		completeInput,
		expect,
	)
	if err != nil {
		return retainedFailedProofAuthentication{}, fmt.Errorf("retained failed proof completion is not authentic: %w", err)
	}
	if migrated {
		return retainedFailedProofAuthentication{}, fmt.Errorf("retained failed proof recovery refuses a legacy completion governance reconstruction")
	}
	// The FULL retry ancestry is authenticated through the existing native lineage owner.
	// validateAndNormalizeProofResourceCompletionPending authenticates only the CURRENT
	// attempt, so a corrupt predecessor is invisible to it.
	chain, err := s.proofRunAttemptAncestry(w, currentReservation, currentPending.Result, ra)
	if err != nil {
		return retainedFailedProofAuthentication{}, fmt.Errorf("retained failed proof retry ancestry is not authentic: %w", err)
	}
	// Decide every reachability exception before rearm can persist recovery effects.
	if _, err := retainedFailedProofRetryLink(w, currentReservation, chain, ra); err != nil {
		return retainedFailedProofAuthentication{}, err
	}
	// Quiescence is a native fact read from the inherited execution barrier and the remote
	// fences. It is never inferred from a terminal attempt status or from a process list.
	resource := landlease.ProofResourceForIdentity(s.home, currentReservation.ProofCoordination)
	pendingExecution, err := resource.ExecutionPending(currentReservation.Holder)
	if err != nil {
		return retainedFailedProofAuthentication{}, fmt.Errorf("retained failed proof execution quiescence is unattested: %w", err)
	}
	if pendingExecution {
		return retainedFailedProofAuthentication{}, fmt.Errorf(
			"retained failed proof recovery refuses an active or ambiguous execution: %s",
			currentReservation.Holder,
		)
	}
	return retainedFailedProofAuthentication{
		Work:     w,
		Pending:  currentPending,
		Chain:    chain,
		Terminal: ra,
	}, nil
}

// retainedFailedProofRetryLink decides the existing reachability rule without writing.
// Authentication uses it before recovery effects; repair rechecks the current Work afterward.
func retainedFailedProofRetryLink(
	w envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	chain []runattempt.RunAttempt,
	terminal runattempt.RunAttempt,
) (*runattempt.RunAttempt, error) {
	var repair *runattempt.RunAttempt
	for index := range chain {
		attempt := chain[index]
		if oneOf(attempt.ID, w.RunAttemptIDs) {
			continue
		}
		if attempt.ID != terminal.ID || terminal.ID == reservation.RunAttemptID {
			return nil, fmt.Errorf("retained failed proof retry lineage attempt %s is not reachable from work", attempt.ID)
		}
		if repair != nil {
			return nil, fmt.Errorf("retained failed proof retry lineage has more than one unreachable attempt")
		}
		repair = &attempt
	}
	return repair, nil
}

// repairRetainedFailedProofRetryReachability requires every attempt of the authenticated retry
// ancestry to be reachable from the Work envelope, and repairs the ONE legitimate exception: the
// terminal retry whose forward link is still pending because the interrupted native recovery was
// minted and journaled before its Work reachability was persisted.
//
// Every membership exception of the chain is decided FIRST. The single permitted forward link is
// written only after the WHOLE chain is known to be reachable otherwise, so a later unreachable
// ancestor can never be discovered after the write.
//
// The repair is the EXISTING native reachability recovery, not a second implementation: the same
// owner the ordinary completion path uses persists the forward link, the action/decision records
// and the lifecycle evidence.
func (s *Store) repairRetainedFailedProofRetryReachability(
	workID string,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
	chain []runattempt.RunAttempt,
	terminal runattempt.RunAttempt,
	now time.Time,
) error {
	w, err := s.Get(workID)
	if err != nil {
		return err
	}
	repair, err := retainedFailedProofRetryLink(w, reservation, chain, terminal)
	if err != nil {
		return err
	}
	if repair == nil {
		return nil
	}
	if err := s.persistProofCompletionRetryReachability(&w, pending, *repair, now); err != nil {
		return fmt.Errorf("retained failed proof retry reachability recovery: %w", err)
	}
	return nil
}

// verifyRetainedPendingSHA256 fences the retained completion journal by its exact bytes. It runs
// at admission and again inside the guard that performs the recovery effects, before any
// report, custody, rearm or attempt mutation.
func (s *Store) verifyRetainedPendingSHA256(reservationID, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		return nil
	}
	sum, err := s.proofResourceCompletionPendingSHA256(reservationID)
	if err != nil {
		return err
	}
	if sum != expected {
		return fmt.Errorf("retained proof completion journal %s does not match the expected pending sha256", reservationID)
	}
	return nil
}

// proofResourceCompletionPendingSHA256 hashes the exact retained completion journal bytes.
func (s *Store) proofResourceCompletionPendingSHA256(reservationID string) (string, error) {
	path, err := s.proofResourceCompletionPendingPath(reservationID)
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read retained proof completion journal: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// RunProofRecoveryCLI dispatches the proof recovery-only routes. It never admits a battery.
func RunProofRecoveryCLI(
	args []string,
	stdout, stderr io.Writer,
	restore func(homePath, artifactRef, outPath string) (ProofCheckReport, error),
) int {
	if HasProofRecoveryFlag(args, "--resume-retained") {
		if HasProofRecoveryFlag(args, "--reconcile-quiescent") {
			fmt.Fprintln(stderr, "proof-check: --resume-retained and --reconcile-quiescent are mutually exclusive")
			return 2
		}
		return RunRetainedFailedProofRecoveryCLI(args, stdout, stderr, restore)
	}
	return RunQuiescentProofRecoveryCLI(args, stdout, stderr)
}

// HasProofRecoveryFlag detects the EXACT recovery-only tokens. Both "--resume-retained PFR" and
// "--resume-retained=PFR" select the retained route, and both enter it BEFORE any source capture,
// plan rebuilding or current-snapshot validation. A longer token that merely starts with the same
// characters is not a recovery flag.
func HasProofRecoveryFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

// RunRetainedFailedProofRecoveryCLI finishes an exact retained failed proof without a battery.
// The retained report is restored through the caller's owned output custody before the
// exclusive holder is released; the verdict stays a failed battery.
func RunRetainedFailedProofRecoveryCLI(
	args []string,
	stdout, stderr io.Writer,
	restore func(homePath, artifactRef, outPath string) (ProofCheckReport, error),
) int {
	flags := flag.NewFlagSet("proof-check --resume-retained", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reservation := flags.String("resume-retained", "", "exact retained proof reservation")
	workID := flags.String("work", "", "owned work")
	actor := flags.String("actor", "", "current actor")
	expected := flags.String("expected-pending-sha256", "", "expected sha256 of the retained completion journal bytes")
	outPath := flags.String("out", "", "path that receives the restored original failed report")
	homeArg := flags.String("home", "", "owning home")
	jsonOut := flags.Bool("json", false, "emit JSON")
	seen := map[string]bool{}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			key := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
			if seen[key] {
				fmt.Fprintln(stderr, "duplicate recovery option:", key)
				return 2
			}
			seen[key] = true
		}
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 ||
		strings.TrimSpace(*reservation) == "" ||
		strings.TrimSpace(*workID) == "" ||
		strings.TrimSpace(*actor) == "" ||
		strings.TrimSpace(*expected) == "" ||
		strings.TrimSpace(*outPath) == "" {
		fmt.Fprintln(stderr, "exact --resume-retained, --work, --actor, --expected-pending-sha256 and --out required")
		return 2
	}
	if restore == nil {
		fmt.Fprintln(stderr, "retained failed proof recovery requires an owned report restorer")
		return 2
	}
	resolvedOut, err := filepath.Abs(*outPath)
	if err != nil {
		fmt.Fprintln(stderr, "proof-check: resolve --out path:", err)
		return 2
	}
	root, err := home.Resolve(*homeArg)
	if err != nil {
		fmt.Fprintln(stderr, "proof-check:", err)
		return 1
	}
	store, err := Open(root)
	if err != nil {
		fmt.Fprintln(stderr, "proof-check:", err)
		return 1
	}
	var report ProofCheckReport
	outcome, found, err := store.ResumeRetainedFailedProofCompletion(*workID, ProofResourceRetainedFailureInput{
		Actor:                 *actor,
		ReservationID:         *reservation,
		ExpectedPendingSHA256: *expected,
		PrepareOwnedReport: func(result ProofResourceResult) error {
			restored, restoreErr := restore(root, result.ReportArtifactRef, resolvedOut)
			if restoreErr != nil {
				return restoreErr
			}
			report = restored
			return nil
		},
	}, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "proof-check:", err)
		return 2
	}
	if !found {
		fmt.Fprintln(stderr, "proof-check: no exact retained failed proof completion matches the requested reservation")
		return 2
	}
	if outcome.ReportArtifactRef != "" && report.SchemaVersion == "" {
		report, err = readRetainedProofCheckReport(root, outcome.ReportArtifactRef)
		if err != nil {
			fmt.Fprintln(stderr, "proof-check:", err)
			return 2
		}
	}
	state := "recovered"
	if *jsonOut {
		body, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			fmt.Fprintln(stderr, "proof-check:", marshalErr)
			return 2
		}
		fmt.Fprintln(stdout, string(body))
	}
	fmt.Fprintln(stdout, ProofCheckVerdictLine(report, resolvedOut)+" retained-failed-proof="+state)
	return 1
}

// readRetainedProofCheckReport decodes the immutable owned report artifact of a retained proof.
func readRetainedProofCheckReport(homePath, artifactRef string) (ProofCheckReport, error) {
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		return ProofCheckReport{}, err
	}
	body, err := artifacts.Get(strings.TrimSpace(artifactRef))
	if err != nil {
		return ProofCheckReport{}, fmt.Errorf("read retained proof report: %w", err)
	}
	var report ProofCheckReport
	if err := json.Unmarshal(body, &report); err != nil {
		return ProofCheckReport{}, fmt.Errorf("decode retained proof report: %w", err)
	}
	if !IsAcceptedProofCheckSchemaVersion(report.SchemaVersion, ProofCheckSchemaVersion) {
		return ProofCheckReport{}, fmt.Errorf("retained proof report schema_version %q is unsupported", report.SchemaVersion)
	}
	return report, nil
}
