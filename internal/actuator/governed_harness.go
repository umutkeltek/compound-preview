package actuator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	homepkg "compound/internal/home"
	"compound/internal/hookplan"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

var governedClaudeReadinessEvidenceKinds = []string{
	"permission_authority",
	"permission_posture",
	"blocking_hook_plan",
	"blocking_hook_installation",
}

// LaneRefusal is the loud-refusal seam R3B (2026-09-04) introduced for an
// unready Claude lane. The operator asked for a mutation-capable mode
// (bypass / default / acceptEdits); readiness came back unmet; instead of
// silently down-grading the lane to plan — which left the caller with a
// "succeeded" run_attempt and no edit — the door must REFUSE BEFORE
// minting a RunAttempt or advancing the envelope. The cmd layer renders
// this struct through the gateRefusal printer and exits non-zero.
//
// The omitted-flag path keeps the prior silent-plan fallback: an operator
// who did not specify --permission-mode is implicitly asking the rail to
// pick a safe posture, and plan is the safe answer. Refusal is reserved
// for the explicit-mutation case, where silently down-grading would hide
// the very failure the operator was asking us to prevent.
type LaneRefusal struct {
	// Command is the user-facing verb that was refused.
	Command string
	// Gate names the unready item in stable, greppable form.
	Gate string
	// Refused states what did not happen (no mint, no advance).
	Refused string
	// Tried lists each resolution step that did NOT keep the lane ready.
	Tried []string
	// DoThis lists exact commands to repair the unready state.
	DoThis []string
	// Prevents names the failure this refusal stops.
	Prevents string
}

// Error renders a single-line summary for `die(err)`-style callers; the
// cmd layer's gateRefusal printer renders the full four-section text.
func (l *LaneRefusal) Error() string {
	if l == nil {
		return "lane refused: nil"
	}
	return fmt.Sprintf("%s refused at gate %s: %s", l.Command, l.Gate, l.Refused)
}

// RenderTo writes the refusal to w in the same four-section shape the cmd
// layer's gateRefusal printer uses, so an operator reading the refusal sees
// a refusal that reads like every other refusal in `compound` (the Tried /
// DoThis / Prevents vocabulary is the discipline; the specific Gate name is
// the only thing that changes). The cmd layer still owns the exit-code
// policy — RenderTo only writes.
func (l *LaneRefusal) RenderTo(w io.Writer) {
	if l == nil {
		return
	}
	fmt.Fprintf(w, "error: %s refused at gate %s\n", l.Command, l.Gate)
	if refused := strings.TrimSpace(l.Refused); refused != "" {
		fmt.Fprintf(w, "  refused: %s\n", refused)
	}
	if len(l.Tried) > 0 {
		fmt.Fprintln(w, "  tried in order:")
		for i, step := range l.Tried {
			fmt.Fprintf(w, "    %d. %s\n", i+1, step)
		}
	}
	if len(l.DoThis) > 0 {
		fmt.Fprintln(w, "  do this:")
		for _, cmd := range l.DoThis {
			fmt.Fprintf(w, "    %s\n", cmd)
		}
	}
	if prevents := strings.TrimSpace(l.Prevents); prevents != "" {
		fmt.Fprintf(w, "  prevents: %s\n", prevents)
	}
}

// GovernedHarnessFinalizationInput binds adapter completion to the exact
// running work generation that authorized it.
type GovernedHarnessFinalizationInput struct {
	RetainFailedAttempt   bool
	ResumeVerifying       bool
	Store                 *work.Store
	WorkID                string
	Actor                 string
	HarnessName           string
	RunningPolicyID       string
	RunningActionIntentID string
	ExistingRunAttemptIDs []string
	Output                harness.RunOutput
	RunError              error
	Now                   time.Time
}

// GovernedHarnessFinalizationResult keeps CLI rendering outside the actuator
// while preserving every lifecycle and attachment warning.
type GovernedHarnessFinalizationResult struct {
	Succeeded       bool
	WorkID          string
	FinalState      string
	FailedAuditOnly bool
	Warnings        []string
}

// GovernedHarnessLaunchInput carries the resolved adapter and the exact
// claimed/running envelope generations needed to bind the launch guard and
// immutable execution posture.
type GovernedHarnessLaunchInput struct {
	HomePath          string
	Adapter           harness.Adapter
	ClaimedWork       envelope.WorkEnvelope
	RunningWork       envelope.WorkEnvelope
	RunInput          harness.RunInput
	ActionIntentID    string
	ReadinessEvidence []work.DelegateHarnessReadinessEvidence
	ReadinessRequest  *work.DelegateHarnessReadinessRequest
	// OperatorExplicitBypass is true when the operator passed --permission-mode
	// bypass (rather than omitting the flag). When true, the rail preserves the
	// requested posture across an unready readiness guard (M5 2026-09-03) and
	// uses the lane-only start guard (no Ready re-check) so the request is
	// honored end-to-end instead of silently downgraded to plan.
	OperatorExplicitBypass bool
	ResumeVerifying        bool
	Now                    time.Time
}

// GovernedHarnessReservationInput is the post-preflight execution contract:
// one exact source work generation, one resolved adapter tuple, and one requested
// permission posture. ResumeVerifying explicitly selects verifying instead of claimed.
type GovernedHarnessReservationInput struct {
	// ResumeVerifying admits a fresh execution; history is provenance, not authority.
	ResumeVerifying bool
	HomePath        string
	Store           *work.Store
	ClaimedWork     envelope.WorkEnvelope
	Adapter         harness.Adapter
	RunInput        harness.RunInput
	Actor           string
	Target          string
	Note            string
	Now             time.Time
}

// GovernedHarnessReservation is the exact running generation and bound launch
// input returned by ReserveGovernedHarnessLaunch.
type GovernedHarnessReservation struct {
	RunningWork    envelope.WorkEnvelope
	RunInput       harness.RunInput
	ActionIntentID string
}

// GovernedHarnessAdvanceExecution is the shared reserve/run/finalize result
// consumed by explicit and selector-driven governed execution commands.
type GovernedHarnessAdvanceExecution struct {
	RunningWork envelope.WorkEnvelope
	Output      harness.RunOutput
	RunError    error
	Finalized   GovernedHarnessFinalizationResult
}

// ReserveGovernedHarnessLaunch resolves the Claude posture, atomically claims
// the one-shot claimed->running execution generation, binds its ActionIntent,
// and installs the final start guard. No adapter subprocess is constructed or
// started before this returns.
func ReserveGovernedHarnessLaunch(in GovernedHarnessReservationInput) (GovernedHarnessReservation, error) {
	if in.Store == nil {
		return GovernedHarnessReservation{}, errors.New("governed harness reservation requires work store")
	}
	if in.Adapter == nil {
		return GovernedHarnessReservation{}, errors.New("governed harness reservation requires adapter")
	}
	if strings.TrimSpace(in.Target) != "running" {
		return GovernedHarnessReservation{}, fmt.Errorf("governed harness reservation target %q must be running", in.Target)
	}
	if refusal := GovernedHarnessSourceRefusal(in.ClaimedWork, in.ResumeVerifying); refusal != nil {
		return GovernedHarnessReservation{}, refusal
	}
	var predecessorID string
	expectedState := "claimed"
	if in.ResumeVerifying {
		expectedState = "verifying"
		var err error
		predecessorID, err = validateGovernedResumeHistory(in)
		if err != nil {
			return GovernedHarnessReservation{}, err
		}
	}
	claimedPolicyDecisionID := strings.TrimSpace(in.ClaimedWork.PolicyDecisionID)
	var claimedActionIntentID string
	if n := len(in.ClaimedWork.ActionIntentIDs); n > 0 {
		claimedActionIntentID = strings.TrimSpace(in.ClaimedWork.ActionIntentIDs[n-1])
	}
	if claimedPolicyDecisionID == "" || claimedActionIntentID == "" {
		return GovernedHarnessReservation{}, fmt.Errorf("governed harness reservation requires an exact %s policy/action generation", expectedState)
	}
	runInput := in.RunInput
	if in.ResumeVerifying {
		runInput.PredecessorID = predecessorID
	}
	_, claudeFamily := in.Adapter.(harness.GovernedPermissionAdapter)
	originalPermissionMode := strings.TrimSpace(runInput.PermissionMode)
	var postureEvidence []work.DelegateHarnessReadinessEvidence
	var postureReadinessRequest *work.DelegateHarnessReadinessRequest
	if claudeFamily {
		requestedMode := strings.TrimSpace(runInput.PermissionMode)
		if requestedMode != "plan" {
			var refusal *LaneRefusal
			runInput.PermissionMode, postureEvidence, postureReadinessRequest, refusal = ResolveGovernedClaudePermissionMode(
				in.HomePath,
				in.Store,
				in.ClaimedWork,
				runInput,
				requestedMode,
				in.Actor,
				in.Target,
				in.ResumeVerifying,
				in.Now,
			)
			// R3B 2026-09-04: explicit mutation request on an unready lane
			// refuses BEFORE any state change. The caller in
			// cmd/compound/work_advance_execute.go renders this through
			// gateRefusal and exits non-zero; the envelope stays "claimed"
			// and no RunAttempt is minted.
			if refusal != nil {
				return GovernedHarnessReservation{}, refusal
			}
		}
	}
	if err := validateOMPPromptOnlyBeforeAdvance(in.HomePath, in.ClaimedWork, in.Adapter, runInput); err != nil {
		return GovernedHarnessReservation{}, err
	}
	autonomyClass := ""
	if in.ResumeVerifying || (claudeFamily && ClaudeMutationPermissionMode(runInput.PermissionMode)) {
		autonomyClass = "build-implement"
	}
	running, err := in.Store.Advance(in.ClaimedWork.ID, work.AdvanceInput{
		To:                       in.Target,
		Actor:                    in.Actor,
		ExpectedWorkState:        expectedState,
		ExpectedPolicyDecisionID: claimedPolicyDecisionID,
		ExpectedActionIntentID:   claimedActionIntentID,
		AutonomyClass:            autonomyClass,
		Note:                     in.Note,
	}, in.Now)
	if err != nil {
		return GovernedHarnessReservation{}, err
	}
	var actionIntentID string
	if n := len(running.ActionIntentIDs); n > 0 {
		actionIntentID = running.ActionIntentIDs[n-1]
	}
	// Bind the adapter prompt to the exact generation just reserved. Raw argv
	// execution keeps its caller-supplied command contract unchanged.
	if (claudeFamily || in.Adapter.Name() == "codex") && (strings.TrimSpace(runInput.Prompt) != "" || (runInput.Command == "" && len(runInput.Args) == 0)) {
		runInput.Prompt = governedGoalPrompt(running, runInput.Prompt)
	}
	runInput, err = PrepareGovernedHarnessLaunch(GovernedHarnessLaunchInput{
		HomePath:               in.HomePath,
		Adapter:                in.Adapter,
		ClaimedWork:            in.ClaimedWork,
		RunningWork:            running,
		RunInput:               runInput,
		ActionIntentID:         actionIntentID,
		ReadinessEvidence:      postureEvidence,
		ReadinessRequest:       postureReadinessRequest,
		OperatorExplicitBypass: originalPermissionMode == "bypass" && !in.ResumeVerifying,
		ResumeVerifying:        in.ResumeVerifying,
		Now:                    in.Now,
	})
	if err != nil {
		return GovernedHarnessReservation{}, err
	}
	return GovernedHarnessReservation{
		RunningWork:    running,
		RunInput:       runInput,
		ActionIntentID: actionIntentID,
	}, nil
}

// governedGoalPrompt gives direct, selected, and resumed harness runs the same
// current objective boundary. The caller's newest request stays explicit and
// takes precedence over any older dispatch prose or saved context.
func governedGoalPrompt(w envelope.WorkEnvelope, request string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Current governed work %s\n\n", w.ID)
	fmt.Fprintf(&b, "- work_id: %s\n", w.ID)
	if w.IDs.ContextPackID != "" {
		fmt.Fprintf(&b, "- context_pack_id: %s\n", w.IDs.ContextPackID)
	}
	if selection := w.ExecutionSelection; selection != nil {
		fmt.Fprintf(&b, "- saved_selection: harness=%s profile=%s model=%s route=%s effort=%s revision=%d\n",
			selection.Harness, selection.LaunchProfileID, selection.RequestedModelID, selection.ProviderRoute, selection.RequestedEffort, selection.Revision)
	}
	fmt.Fprintf(&b, "\n## Desired outcome\n%s\n", strings.TrimSpace(w.DesiredOutcome))
	fmt.Fprintf(&b, "\n## Success predicate\n%s\n", strings.TrimSpace(w.SuccessPredicate))
	if len(w.NonGoals) > 0 {
		fmt.Fprintln(&b, "\n## Non-goals")
		for _, nonGoal := range w.NonGoals {
			fmt.Fprintf(&b, "- %s\n", nonGoal)
		}
	}
	fmt.Fprintln(&b, "\n## Execution instruction")
	fmt.Fprintln(&b, "Use this current work goal and context to execute the request; report evidence against the success predicate. The latest explicit request below takes precedence over older task prose.")
	if strings.TrimSpace(request) != "" {
		fmt.Fprintf(&b, "\n## Latest explicit request\n%s\n", strings.TrimSpace(request))
	}
	return strings.TrimSpace(b.String())
}

// ExecuteGovernedHarnessAdvance runs the one mutation seam shared by explicit
// work advance --execute and selector-driven execution. Callers must complete
// all read-only adapter, workspace, roster, and preflight checks first.
//
// ctx is the caller's launch context (production: signal-derived via
// executewire.LaunchContext). The adapter's Run receives it directly — the
// shared runner translates cancellation into a process-group kill with
// cancellation evidence on the attempt. Callers that pass context.Background()
// forfeit that guarantee; a caller may not silently strip cancellation from
// the canonical execution owner.
// GovernedHarnessContinuationOptions changes only failed-attempt disposition.
// It never changes source admission, permission, policy or retry authority.
type GovernedHarnessContinuationOptions struct {
	RetainFailedAttempt bool
}

func ExecuteGovernedHarnessAdvance(
	ctx context.Context,
	homePath string,
	store *work.Store,
	claimedWork envelope.WorkEnvelope,
	adapter harness.Adapter,
	runInput harness.RunInput,
	actor string,
	target string,
	note string,
	existingRunAttempts []string,
	resumeVerifying bool,
	now time.Time,
	continuation ...GovernedHarnessContinuationOptions,
) (GovernedHarnessAdvanceExecution, error) {
	if len(continuation) > 1 {
		return GovernedHarnessAdvanceExecution{}, errors.New("only one continuation disposition may be supplied")
	}
	retainFailure := len(continuation) == 1 && continuation[0].RetainFailedAttempt
	reserved, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
		HomePath: homePath, Store: store, ClaimedWork: claimedWork, Adapter: adapter,
		RunInput: runInput, Actor: actor, Target: target, Note: note, ResumeVerifying: resumeVerifying, Now: now,
	})
	if err != nil {
		return GovernedHarnessAdvanceExecution{}, err
	}
	out, runErr := adapter.Run(ctx, reserved.RunInput)
	harnessName := strings.TrimSpace(runInput.Harness)
	if harnessName == "" {
		harnessName = adapter.Name()
	}
	finalized, err := FinalizeGovernedHarnessRun(GovernedHarnessFinalizationInput{
		RetainFailedAttempt:   retainFailure,
		Store:                 store,
		WorkID:                claimedWork.ID,
		Actor:                 actor,
		HarnessName:           harnessName,
		RunningPolicyID:       reserved.RunningWork.PolicyDecisionID,
		RunningActionIntentID: reserved.ActionIntentID,
		ExistingRunAttemptIDs: existingRunAttempts,
		ResumeVerifying:       resumeVerifying,
		Output:                out,
		RunError:              runErr,
		Now:                   time.Now().UTC(),
	})
	if err != nil {
		return GovernedHarnessAdvanceExecution{}, err
	}
	return GovernedHarnessAdvanceExecution{
		RunningWork: reserved.RunningWork,
		Output:      out,
		RunError:    runErr,
		Finalized:   finalized,
	}, nil
}

// PrepareGovernedHarnessLaunch binds the ActionIntent, exact reservation
// guard, Claude lane tuple, and immutable authority posture before Adapter.Run
// can construct or start the subprocess.
func PrepareGovernedHarnessLaunch(in GovernedHarnessLaunchInput) (harness.RunInput, error) {
	if in.Adapter == nil {
		return harness.RunInput{}, errors.New("governed harness launch requires adapter")
	}
	runInput := in.RunInput
	runInput.ActionIntentID = in.ActionIntentID
	runInput.StartRevalidate = func() error {
		return RevalidateGovernedExecutionReservation(in.HomePath, in.RunningWork.ID, in.RunningWork.PolicyDecisionID, in.ActionIntentID)
	}
	runInput.StartGuard = NewGovernedExecutionReservationStartGuard(
		in.HomePath,
		in.RunningWork.ID,
		in.RunningWork.PolicyDecisionID,
		in.ActionIntentID,
	)
	if in.ResumeVerifying {
		runInput.StartGuard = newGovernedResumeStartGuard(in.HomePath, in.RunningWork.ID, in.RunningWork.PolicyDecisionID, in.ActionIntentID, nil)
	}
	if native, err := prepareOMPNativeVerifierLaunch(in, runInput); err != nil {
		return harness.RunInput{}, err
	} else {
		runInput = native
	}
	if _, claudeFamily := in.Adapter.(harness.GovernedPermissionAdapter); !claudeFamily {
		return prepareOMPPromptOnlyLaunch(in, runInput)
	}
	readinessRequest := in.ReadinessRequest
	if readinessRequest == nil {
		request := GovernedClaudeReadinessRequestForRun(in.HomePath, in.ClaimedWork, runInput, in.Now)
		readinessRequest = &request
	}
	runInput.StartGuard = NewGovernedClaudeLaneStartGuard(
		in.HomePath,
		in.Adapter.Name(),
		*readinessRequest,
		in.RunningWork.PolicyDecisionID,
		in.ActionIntentID,
	)
	if in.ResumeVerifying && !ClaudeMutationPermissionMode(runInput.PermissionMode) {
		request, adapterName := *readinessRequest, in.Adapter.Name()
		runInput.StartGuard = newGovernedResumeStartGuard(in.HomePath, in.RunningWork.ID, in.RunningWork.PolicyDecisionID, in.ActionIntentID,
			func(ctx context.Context) error { return revalidateGovernedClaudeLane(ctx, adapterName, request) })
	}
	authorities := make([]runattempt.ExecutionPostureEvidence, 0, len(in.ReadinessEvidence))
	for _, item := range in.ReadinessEvidence {
		authorities = append(authorities, runattempt.ExecutionPostureEvidence{
			Kind: item.Kind, Ref: item.Ref, Revision: item.Revision,
		})
	}
	runInput.ExecutionPosture = &runattempt.ExecutionPosture{
		PermissionMode:    runInput.PermissionMode,
		PolicyDecisionID:  in.RunningWork.PolicyDecisionID,
		AuthorityEvidence: authorities,
	}
	// M5 2026-09-03: explicit --permission-mode bypass honors the operator's
	// request across an unready readiness guard; the rail still binds the
	// authorities that WERE attested, but does not re-gate launch on a
	// re-checked Ready=true verdict the operator already chose to override.
	if ClaudeMutationPermissionMode(runInput.PermissionMode) && !in.OperatorExplicitBypass {
		runInput.StartGuard = NewGovernedClaudeStartGuard(
			in.HomePath,
			in.Adapter.Name(),
			*readinessRequest,
			in.ReadinessEvidence,
			in.RunningWork.PolicyDecisionID,
			in.ActionIntentID,
		)
	}
	return runInput, nil
}

// FinalizeGovernedHarnessRun advances or closes only the exact running
// reservation that launched the adapter. A newer lifecycle generation wins;
// the refused attempt remains reverse-linked by work+intent without mutating
// that newer WorkEnvelope generation.
func FinalizeGovernedHarnessRun(in GovernedHarnessFinalizationInput) (GovernedHarnessFinalizationResult, error) {
	if in.Store == nil {
		return GovernedHarnessFinalizationResult{}, errors.New("governed harness finalization requires work store")
	}
	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	allRunAttempts := append([]string(nil), in.ExistingRunAttemptIDs...)
	if in.Output.RunAttempt.ID != "" {
		allRunAttempts = append(allRunAttempts, in.Output.RunAttempt.ID)
	}

	result := GovernedHarnessFinalizationResult{
		Succeeded: in.RunError == nil && in.Output.RunAttempt.Status == runattempt.StatusSucceeded,
	}
	if in.RetainFailedAttempt && !result.Succeeded {
		// An attemptless or unverifiable infrastructure error is unresolved,
		// not a verified failed execution. Keep the exact running reservation
		// for recovery rather than permitting another launch over uncertainty.
		attempt, err := runattempt.Open(in.Store.HomePath()).Get(in.Output.RunAttempt.ID)
		if err != nil || attempt.WorkID != in.WorkID || attempt.ActionIntentID != in.RunningActionIntentID ||
			attempt.Origin != runattempt.OriginHarnessExecution ||
			(attempt.Status != runattempt.StatusFailed && attempt.Status != runattempt.StatusAbandoned) {
			return result, errors.New("selected execution failure lacks a new bound terminal failed attempt; running work remains unresolved for exact recovery")
		}
		for _, prior := range in.ExistingRunAttemptIDs {
			if prior == attempt.ID {
				return result, errors.New("selected failure cannot reuse a prior attempt as new recovery evidence")
			}
		}
	}
	var reservationErr error
	if result.Succeeded || in.ResumeVerifying || in.RetainFailedAttempt {
		advanced, err := in.Store.Advance(in.WorkID, work.AdvanceInput{
			To:                       "verifying",
			Actor:                    in.Actor,
			ExpectedWorkState:        "running",
			ExpectedPolicyDecisionID: in.RunningPolicyID,
			ExpectedActionIntentID:   in.RunningActionIntentID,
			RunAttemptIDs:            allRunAttempts,
			VerificationID:           in.Output.TranscriptPath,
			Note:                     "harness " + in.HarnessName + " " + in.Output.RunAttempt.Status,
		}, now)
		if errors.Is(err, work.ErrExecutionReservationChanged) {
			reservationErr = err
		} else if err != nil {
			return GovernedHarnessFinalizationResult{}, err
		} else {
			result.WorkID = advanced.ID
			result.FinalState = advanced.State
			if !result.Succeeded {
				result.Warnings = append(result.Warnings, "execution attempt failed; work remains verifying for governed retry, not completed")
			}
		}
	} else {
		closeIn := work.CloseInput{
			Outcome:                  "failed",
			MoveType:                 "audit_only",
			Actor:                    in.Actor,
			ExpectedWorkState:        "running",
			ExpectedPolicyDecisionID: in.RunningPolicyID,
			ExpectedActionIntentID:   in.RunningActionIntentID,
			Summary:                  "harness " + in.HarnessName + " " + in.Output.RunAttempt.Status,
			Changed:                  true,
			RunAttemptIDs:            allRunAttempts,
		}
		if in.Output.TranscriptPath != "" {
			closeIn.ProofRefs = []string{in.Output.TranscriptPath}
		}
		if _, err := in.Store.Close(in.WorkID, closeIn, now); errors.Is(err, work.ErrExecutionReservationChanged) {
			reservationErr = err
		} else if err != nil {
			return GovernedHarnessFinalizationResult{}, err
		} else {
			result.WorkID = in.WorkID
			result.FinalState = "failed"
			result.FailedAuditOnly = true
		}
	}
	if reservationErr != nil {
		result.Succeeded = false
		result.Warnings = append(result.Warnings, fmt.Sprintf("%v; preserving the newer work lifecycle state", reservationErr))
		result.Warnings = append(result.Warnings, ReportRevokedExecutionAttempt(
			in.Store,
			in.WorkID,
			in.Output.RunAttempt.ID,
		)...)
	}
	return result, nil
}

// ReportRevokedExecutionAttempt surfaces the explicit reverse-link-only race
// exception. It deliberately does not touch the WorkEnvelope: even a
// same-state Advance mints a new policy/action generation and would revoke a
// newer running reservation while trying to preserve it.
func ReportRevokedExecutionAttempt(store *work.Store, workID, executionAttemptID string) []string {
	if store == nil || strings.TrimSpace(executionAttemptID) == "" {
		return nil
	}
	current, err := store.Get(workID)
	if err != nil {
		return []string{fmt.Sprintf("report revoked execution attempt %s: read work: %v", executionAttemptID, err)}
	}
	if current.State == "completed" || current.State == "failed" || current.State == "cancelled" {
		return []string{fmt.Sprintf(
			"execution attempt %s remains reverse-linked only: work %s became terminal (%s), terminal receipt/work hashes are immutable, and the newer lifecycle generation is preserved",
			executionAttemptID,
			workID,
			current.State,
		)}
	}
	return []string{fmt.Sprintf(
		"execution attempt %s remains reverse-linked only: work %s moved to a newer lifecycle generation (%s); preserving its state, policy, and action identity",
		executionAttemptID,
		workID,
		current.State,
	)}
}

// ResolveGovernedClaudePermissionMode resolves every non-plan Claude-family
// posture. Plan is the fail-closed result when the operator omits --permission-mode;
// an explicit "bypass" request is preserved across an unready readiness guard so
// the rail does not silently down-grade to plan (M5 defect 2026-09-03).
// Explicit "default" / "acceptEdits" still require current authority.
//
// R3B (2026-09-04) tightens the explicit-mutation path: when the operator
// asked for `bypass` / `default` / `acceptEdits` and the lane resolves to
// plan for ANY reason (store nil, advance plan refused, autonomy halted,
// readiness unmet without evidence, no context pack), the function
// captures the unready detail and returns it via the refusal out-param.
// The caller (ExecuteGovernedHarnessAdvance) refuses BEFORE minting a
// RunAttempt or advancing the envelope. The omitted-flag case still
// falls back to plan silently — the operator didn't pick a posture, so
// the rail does.
func ResolveGovernedClaudePermissionMode(
	homePath string,
	store *work.Store,
	wenv envelope.WorkEnvelope,
	runInput harness.RunInput,
	requestedMode string,
	actor string,
	target string,
	resumeVerifying bool,
	now time.Time,
) (string, []work.DelegateHarnessReadinessEvidence, *work.DelegateHarnessReadinessRequest, *LaneRefusal) {
	const fallback = "plan"
	trimmed := strings.TrimSpace(requestedMode)
	// The historical explicit-bypass exception is not authority for a new resume.
	explicitBypass := trimmed == "bypass" && !resumeVerifying
	// explicitMutationRequested is true when the operator picked a
	// mutation-capable posture themselves. "plan" is read-only and never
	// mutation-capable; "" (omitted) preserves the silent-plan fallback.
	explicitMutationRequested := trimmed != "plan" && (trimmed != "" || resumeVerifying)
	resolvedMode := trimmed
	if resolvedMode == "" {
		resolvedMode = "bypass"
	}
	expectedState := "claimed"
	if resumeVerifying {
		expectedState = "verifying"
	}
	if store == nil || wenv.State != expectedState {
		if explicitMutationRequested {
			return fallback, nil, nil, unreadyLaneRefusal(runInput.Harness, "store_unavailable_or_state_not_claimed")
		}
		return fallback, nil, nil, nil
	}
	plan, err := store.AdvancePlan(wenv.ID, now, work.AdvancePlanOptions{Actor: actor, To: target})
	if err != nil || plan.Status != "advance_policy_allowed" || !plan.Policy.Allowed || plan.NoAction != "" {
		if explicitMutationRequested {
			reason := "advance_plan_refused"
			if err != nil {
				reason = "advance_plan_refused:" + err.Error()
			} else if plan.NoAction != "" {
				reason = "advance_plan_no_action:" + plan.NoAction
			}
			return fallback, nil, nil, unreadyLaneRefusal(runInput.Harness, reason)
		}
		return fallback, nil, nil, nil
	}
	halted, haltReason, err := policy.AutonomyHalted(homePath, "build-implement")
	if err != nil || halted {
		if explicitMutationRequested {
			reason := "autonomy_halted"
			if err != nil {
				reason = "autonomy_halted:" + err.Error()
			} else if strings.TrimSpace(haltReason) != "" {
				reason = "autonomy_halted:" + haltReason
			}
			return fallback, nil, nil, unreadyLaneRefusal(runInput.Harness, reason)
		}
		return fallback, nil, nil, nil
	}
	readinessGuard := work.DelegateHarnessReadiness
	if readinessGuard == nil {
		// No readiness check available: explicit bypass still preserves
		// through M5 2026-09-03 only when the operator asked for bypass.
		// explicit default/acceptEdits without a readiness guard is not
		// safe to honor — refuse.
		if explicitMutationRequested && !explicitBypass {
			return fallback, nil, nil, unreadyLaneRefusal(runInput.Harness, "readiness_guard_unavailable")
		}
		if explicitBypass {
			return "bypass", nil, nil, nil
		}
		return fallback, nil, nil, nil
	}
	readinessRequest := GovernedClaudeReadinessRequestForRun(homePath, wenv, runInput, now)
	readiness, err := readinessGuard(readinessRequest)
	if err != nil || !readiness.Ready {
		// M5 2026-09-03: an explicit --permission-mode bypass is preserved
		// across an unready readiness guard ONLY when the guard still
		// published the four authority revisions the runattempt posture
		// requires. Without those revisions, silently down-grading to plan
		// remains the safe default.
		//
		// R3B 2026-09-04: when the operator asked for an EXPLICIT mutation
		// mode and we cannot keep the requested posture across an unready
		// readiness guard, the door must REFUSE instead of silently
		// down-grading. Build the refusal with the exact readiness
		// details so the operator sees the precise unmet item and the
		// matching repair command.
		if explicitBypass {
			if selected, _ := selectGovernedClaudeReadinessEvidence(readinessRequest.Harness, readiness.Evidence); selected != nil {
				return "bypass", selected, &readinessRequest, nil
			}
		}
		if explicitMutationRequested {
			return fallback, nil, nil, unreadyReadinessRefusal(runInput.Harness, readinessRequest, readiness, err)
		}
		return fallback, nil, nil, nil
	}
	selected, ok := selectGovernedClaudeReadinessEvidence(readinessRequest.Harness, readiness.Evidence)
	if !ok {
		if explicitBypass {
			return "bypass", nil, nil, nil
		}
		if explicitMutationRequested {
			return fallback, nil, nil, unreadyReadinessRefusal(runInput.Harness, readinessRequest, readiness, nil)
		}
		return fallback, nil, nil, nil
	}
	return resolvedMode, selected, &readinessRequest, nil
}

// unreadyLaneRefusal builds a refusal for non-readiness unready conditions
// (store nil, state not claimed, advance plan refused, autonomy halted,
// readiness guard unavailable). The readiness_unmet:<reason> namespace is
// what the red test asserts in stderr at gate lane_readiness.
func unreadyLaneRefusal(harnessName, ready string) *LaneRefusal {
	if strings.TrimSpace(harnessName) == "" {
		harnessName = "claude"
	}
	return &LaneRefusal{
		Command: "compound work advance --execute",
		Gate:    "lane_readiness",
		Refused: fmt.Sprintf("work cannot run with an explicit mutation-capable mode on harness %q: the lane is not ready (readiness_unmet: %s)", harnessName, ready),
		Tried: []string{
			fmt.Sprintf("readiness_unmet: %s", ready),
		},
		DoThis: []string{
			"compound harness readiness --harness " + harnessName,
			"compound work delegate --dry-run",
		},
		Prevents: "silently down-grading an explicit --permission-mode request to plan on an unready lane: the caller would see status: succeeded and a no-edit RunAttempt, with no signal that the lane could not actually run.",
	}
}

// unreadyReadinessRefusal builds a refusal for the readiness-met failure
// path. It copies the readiness details verbatim (each "kind: detail" line)
// so the operator sees the precise unmet items, and lists the matching
// repair verbs so the refusal is actionable.
func unreadyReadinessRefusal(harnessName string, req work.DelegateHarnessReadinessRequest, readiness work.DelegateHarnessReadinessResult, readinessErr error) *LaneRefusal {
	if strings.TrimSpace(harnessName) == "" {
		harnessName = strings.TrimSpace(req.Harness)
	}
	if strings.TrimSpace(harnessName) == "" {
		harnessName = "claude"
	}
	tried := append([]string(nil), readiness.Details...)
	if readinessErr != nil {
		tried = append(tried, "readiness_guard:"+readinessErr.Error())
	}
	return &LaneRefusal{
		Command: "compound work advance --execute",
		Gate:    "lane_readiness",
		Refused: fmt.Sprintf("work cannot run with an explicit mutation-capable mode on harness %q: the lane is not ready", harnessName),
		Tried: append([]string{
			"readiness_unmet: harness readiness returned Ready=false (no authority evidence attached)",
		}, tried...),
		DoThis:   laneUnreadyRepairs(harnessName, req, readiness.Details),
		Prevents: "silently down-grading an explicit --permission-mode request to plan on an unready lane: the caller would see status: succeeded and a no-edit RunAttempt, with no signal that the lane could not actually run.",
	}
}

// laneUnreadyRepairs returns one DoThis line per unique unmet readiness
// detail category. It uses simple substring matching against the readiness
// detail prefix so the refusal stays accurate without coupling to
// internal package symbols beyond what is already imported by the readiness
// evaluator.
func laneUnreadyRepairs(harnessName string, req work.DelegateHarnessReadinessRequest, details []string) []string {
	out := []string{
		"compound harness readiness --harness " + harnessName,
		"compound work delegate --dry-run",
	}
	seen := map[string]bool{}
	for _, d := range details {
		var line string
		switch {
		case strings.HasPrefix(d, "permission_authority") || strings.HasPrefix(d, "permission_posture"):
			line = "compound harness permission-mode --harness " + harnessName + "    # permission authority / posture"
		case strings.HasPrefix(d, "blocking_hook") || strings.HasPrefix(d, "mutation-covering"):
			line = "compound hook install --harness " + harnessName + "             # blocking-hook binding"
		case strings.HasPrefix(d, "instruction_pack"):
			line = "compound harness instruction-pack --harness " + harnessName + " # instruction pack / context"
		case strings.HasPrefix(d, "activation_argv"):
			line = "compound harness readiness --harness " + harnessName + " --workspace \"" + strings.TrimSpace(req.Workspace) + "\"  # argv-policy"
		default:
			continue
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// GovernedClaudeReadinessRequestForRun binds readiness to the exact resolved
// lane tuple and activation command that will cross cmd.Start.
func GovernedClaudeReadinessRequestForRun(homePath string, wenv envelope.WorkEnvelope, runInput harness.RunInput, now time.Time) work.DelegateHarnessReadinessRequest {
	activationArgs := append([]string(nil), runInput.Args...)
	if strings.TrimSpace(runInput.Prompt) != "" {
		activationArgs = []string{runInput.Prompt}
	}
	return work.DelegateHarnessReadinessRequest{
		HomePath:               homePath,
		WorkID:                 wenv.ID,
		ContextPackID:          wenv.IDs.ContextPackID,
		Workspace:              runInput.Workspace,
		Harness:                normalizeDelegateReadinessHarness(runInput.Harness),
		LaneID:                 runInput.LaneID,
		LaunchProfileID:        runInput.LaunchProfileID,
		RequestedModelID:       runInput.RequestedModelID,
		RequestedProviderRoute: runInput.RequestedProviderRoute,
		Command:                runInput.Command,
		Args:                   activationArgs,
		Timeout:                runInput.Timeout,
		Now:                    now,
	}
}

// ClaudeMutationPermissionMode reports modes that can mutate without a
// per-tool prompt and therefore require the full authority tuple.
func ClaudeMutationPermissionMode(mode string) bool {
	switch strings.TrimSpace(mode) {
	case "default", "acceptEdits", "bypass":
		return true
	default:
		return false
	}
}

// NewGovernedClaudeLaneStartGuard revalidates every Claude-family lane tuple
// and exact running reservation immediately around cmd.Start.
func NewGovernedClaudeLaneStartGuard(
	homePath string,
	adapterName string,
	readinessRequest work.DelegateHarnessReadinessRequest,
	expectedPolicyDecisionID string,
	expectedActionIntentID string,
) func(context.Context, func() error) error {
	return func(ctx context.Context, start func() error) error {
		if start == nil {
			return fmt.Errorf("governed Claude start operation required")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return homepkg.WithAutonomyAdmissionLock(homePath, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := revalidateGovernedClaudeLane(ctx, adapterName, readinessRequest); err != nil {
				return err
			}
			if err := RevalidateGovernedExecutionReservation(
				homePath,
				readinessRequest.WorkID,
				expectedPolicyDecisionID,
				expectedActionIntentID,
			); err != nil {
				return err
			}
			return start()
		})
	}
}

// NewGovernedClaudeStartGuard adds halt and exact blocking-authority
// revalidation for mutation-capable Claude-family modes.
func NewGovernedClaudeStartGuard(
	homePath string,
	adapterName string,
	readinessRequest work.DelegateHarnessReadinessRequest,
	expected []work.DelegateHarnessReadinessEvidence,
	expectedPolicyDecisionID string,
	expectedActionIntentID string,
) func(context.Context, func() error) error {
	expected = append([]work.DelegateHarnessReadinessEvidence(nil), expected...)
	return func(ctx context.Context, start func() error) error {
		if start == nil {
			return fmt.Errorf("governed Claude start operation required")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Permission publication takes the permission lock before it enters
		// work's autonomy-admission lock. Keep the same order here.
		return harness.WithPermissionPostureLock(homePath, func() error {
			return homepkg.WithAutonomyAdmissionLock(homePath, func() error {
				halted, reason, err := policy.AutonomyHalted(homePath, "build-implement")
				if err != nil {
					return fmt.Errorf("revalidate autonomy halt before launch: %w", err)
				}
				if halted {
					return fmt.Errorf("autonomy halted before launch: %s", reason)
				}
				if err := revalidateGovernedClaudeLane(ctx, adapterName, readinessRequest); err != nil {
					return err
				}
				readinessGuard := work.DelegateHarnessReadiness
				if readinessGuard == nil {
					return fmt.Errorf("governed Claude readiness unavailable before launch")
				}
				readinessRequest.Now = time.Now().UTC()
				readiness, err := readinessGuard(readinessRequest)
				if err != nil {
					return fmt.Errorf("revalidate governed Claude readiness before launch: %w", err)
				}
				if !readiness.Ready {
					return fmt.Errorf("governed Claude readiness became stale before launch: %s", strings.Join(readiness.Details, "; "))
				}
				observed, ok := selectGovernedClaudeReadinessEvidence(readinessRequest.Harness, readiness.Evidence)
				if !ok || !sameGovernedClaudeReadinessEvidence(expected, observed) {
					return fmt.Errorf("governed Claude authority revisions changed before launch")
				}
				if err := RevalidateGovernedExecutionReservation(
					homePath,
					readinessRequest.WorkID,
					expectedPolicyDecisionID,
					expectedActionIntentID,
				); err != nil {
					return err
				}
				return start()
			})
		})
	}
}

// NewGovernedExecutionReservationStartGuard protects every adapter, including
// non-Claude harnesses, from starting after its running generation changed.
func NewGovernedExecutionReservationStartGuard(
	homePath string,
	workID string,
	expectedPolicyDecisionID string,
	expectedActionIntentID string,
) func(context.Context, func() error) error {
	return func(ctx context.Context, start func() error) error {
		if start == nil {
			return fmt.Errorf("governed start operation required")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return homepkg.WithAutonomyAdmissionLock(homePath, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := RevalidateGovernedExecutionReservation(homePath, workID, expectedPolicyDecisionID, expectedActionIntentID); err != nil {
				return err
			}
			return start()
		})
	}
}

// RevalidateGovernedExecutionReservation checks current launch authority and the exact running
// state/policy/action generation under the caller's admission lock.
func RevalidateGovernedExecutionReservation(homePath, workID, expectedPolicyDecisionID, expectedActionIntentID string) error {
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return fmt.Errorf("revalidate governed execution reservation: %w", err)
	}
	return store.ValidateExecutionStartAuthority(workID, expectedPolicyDecisionID, expectedActionIntentID, time.Now)
}

func revalidateGovernedClaudeLane(ctx context.Context, adapterName string, expected work.DelegateHarnessReadinessRequest) error {
	registry := harness.NewAdapterRegistry(runattempt.Open(expected.HomePath))
	adapter, ok := registry.AdapterByName(strings.TrimSpace(adapterName))
	if !ok {
		return fmt.Errorf("governed Claude lane adapter %q disappeared before launch", expected.Harness)
	}
	revalidator, ok := adapter.(harness.GovernedPermissionAdapter)
	if !ok {
		return fmt.Errorf("governed Claude lane adapter %q cannot revalidate launch authority", expected.Harness)
	}
	resolved, err := revalidator.RevalidateGovernedRun(ctx, harness.RunInput{
		WorkID:                 expected.WorkID,
		Harness:                strings.TrimSpace(adapterName),
		Workspace:              expected.Workspace,
		Command:                expected.Command,
		Args:                   append([]string(nil), expected.Args...),
		PermissionMode:         "plan",
		LaneID:                 expected.LaneID,
		LaunchProfileID:        expected.LaunchProfileID,
		RequestedModelID:       expected.RequestedModelID,
		RequestedProviderRoute: expected.RequestedProviderRoute,
		Timeout:                expected.Timeout,
		Now:                    time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("revalidate governed Claude lane before launch: %w", err)
	}
	if normalizeDelegateReadinessHarness(resolved.Harness) != normalizeDelegateReadinessHarness(expected.Harness) ||
		resolved.Command != expected.Command ||
		resolved.LaneID != expected.LaneID ||
		resolved.RequestedModelID != expected.RequestedModelID ||
		resolved.RequestedProviderRoute != expected.RequestedProviderRoute {
		return fmt.Errorf("governed Claude lane tuple changed before launch")
	}
	return nil
}

func sameGovernedClaudeReadinessEvidence(a, b []work.DelegateHarnessReadinessEvidence) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func selectGovernedClaudeReadinessEvidence(harnessName string, evidence []work.DelegateHarnessReadinessEvidence) ([]work.DelegateHarnessReadinessEvidence, bool) {
	harnessName = normalizeDelegateReadinessHarness(harnessName)
	if harnessName == "" {
		return nil, false
	}
	required := make(map[string]bool, len(governedClaudeReadinessEvidenceKinds))
	for _, kind := range governedClaudeReadinessEvidenceKinds {
		required[kind] = true
	}
	selected := make(map[string]work.DelegateHarnessReadinessEvidence, len(required))
	for _, item := range evidence {
		if !required[item.Kind] {
			continue
		}
		if strings.TrimSpace(item.Ref) == "" || strings.TrimSpace(item.Revision) == "" ||
			!GovernedClaudeEvidenceRefMatchesHarness(harnessName, item.Kind, item.Ref) {
			return nil, false
		}
		if _, duplicate := selected[item.Kind]; duplicate {
			return nil, false
		}
		selected[item.Kind] = work.DelegateHarnessReadinessEvidence{
			Kind: strings.TrimSpace(item.Kind), Ref: strings.TrimSpace(item.Ref), Revision: strings.TrimSpace(item.Revision),
		}
	}
	out := make([]work.DelegateHarnessReadinessEvidence, 0, len(required))
	for _, kind := range governedClaudeReadinessEvidenceKinds {
		item, ok := selected[kind]
		if !ok {
			return nil, false
		}
		out = append(out, item)
	}
	return out, true
}

// GovernedClaudeEvidenceRefMatchesHarness accepts only the named harness's
// canonical permission/posture/install refs and either a bare PreToolUse ref
// or the exact canonical matcher digest.
func GovernedClaudeEvidenceRefMatchesHarness(harnessName, kind, ref string) bool {
	harnessName = normalizeDelegateReadinessHarness(harnessName)
	ref = strings.TrimSpace(ref)
	switch strings.TrimSpace(kind) {
	case "permission_authority":
		return ref == harness.PermissionAuthorityRef(harnessName)
	case "permission_posture":
		return ref == "harness-posture://"+harnessName
	case "blocking_hook_plan":
		base := "hook://" + harnessName + "/PreToolUse"
		if ref == base {
			return true
		}
		matcherRevision := strings.TrimPrefix(ref, base+"?matcher=")
		if matcherRevision == ref {
			return false
		}
		digest := sha256.Sum256([]byte(hookplan.HarnessMatcher(harnessName)))
		return matcherRevision == hex.EncodeToString(digest[:])[:12]
	case "blocking_hook_installation":
		return ref == "harness-installation://live"
	default:
		return false
	}
}

func normalizeDelegateReadinessHarness(value string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "-", "_")
	if normalized == "claude" {
		return "claude_code"
	}
	return normalized
}
