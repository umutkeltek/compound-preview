package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/actuator"
	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/hookplan"
	"compound/internal/runattempt"
	"compound/internal/work"
	"compound/internal/work/executewire"
)

var workAdvanceExecuteExit = os.Exit

// runWorkAdvanceExecute is the --execute branch of compound work advance.
// It runs a registered harness adapter inline as part of the work
// advance so the work moves through running -> verifying (or failed) in
// a single CLI invocation.
//
// The flow:
//  1. Reject any --to value other than "running" (or empty, which
//     defaults to "running"), then validate the work envelope + resolve
//     the adapter + resolve the workspace BEFORE any state change. An
//     unknown harness, a missing adapter, or an unresolvable workspace
//     fails closed here -- the work never moves, so it can never be left
//     stuck in "running".
//  2. Advance to "running". RunAttemptIDs are empty here because the
//     adapter has not yet minted -- the validating envelope rule (ratt_
//     prefix) forbids stale ids on the running state. The advance mints
//     an ActionIntent whose id is bound onto the adapter run below.
//  3. Mint a RunAttempt via the adapter, run the subprocess, capture
//     transcript. The adapter drives its own lifecycle (C.1). The
//     RunAttempt is bound to the ActionIntent from step 2
//     (RunInput.ActionIntentID), closing the attempt->intent->
//     policy-decision audit chain.
//  4. Advance to "verifying" (succeeded) or close as "failed" (non-zero
//     exit, timeout, or infrastructure failure). The ratt_ id lands on
//     RunAttemptIDs on BOTH paths -- the failed-path forward-link is the
//     close-time RunAttemptIDs (see work.CloseInput.RunAttemptIDs). The
//     audit chain is complete either way.
//
// Exit code: 0 on succeeded, non-zero on failed/timeout/infra. Matches the
// compound harness run contract.
func runWorkAdvanceExecute(homePath string, store *work.Store, workID string, rest []string, existingRunAttempts []string, explicitSelection *envelope.ExecutionSelection, now time.Time) {
	// Split before reading any Compound option; child argv has its own owner.
	var childArgs []string
	if idx := indexOf(rest, "--"); idx >= 0 {
		childArgs = rest[idx+1:]
		rest = rest[:idx]
	}
	if workID == "" {
		fmt.Fprintln(os.Stderr, "error: --execute requires a work_id")
		workAdvanceExecuteExit(2)
	}
	harnessName := strings.TrimSpace(valueFlag(rest, "--harness"))
	prompt := valueFlag(rest, "--prompt")
	actor := firstNonEmpty(valueFlag(rest, "--by"), valueFlag(rest, "--actor"))
	if hasFlag(rest, "--context-pack") {
		state := "<current-state>"
		if current, err := store.Get(workID); err == nil {
			state = current.State
		}
		fmt.Fprintf(os.Stderr, "error: --context-pack cannot be combined with --execute; attach it with a separate same-state advance first:\n  compound work advance %q --to %s --by %q --context-pack %q\nthen execute without --context-pack\n", workID, state, actor, valueFlag(rest, "--context-pack"))
		workAdvanceExecuteExit(2)
		return
	}
	// The launch context is the production signal-derived one: SIGINT/SIGTERM
	// cancel the governed run, and the shared runner kills the adapter's
	// process group and stamps cancellation evidence on the attempt. Tests
	// swap it via executewire.InstallLaunchContext.
	launchCtx := executewire.LaunchContext()

	// --execute only ever advances the work through "running" -- the
	// adapter mint below, and the verifying/failed advance that follows
	// it, both assume the work is IN running when the harness drives.
	// Reject any other --to value up front, before store.Get, adapter
	// resolution, workspace resolution, or store.Advance -- none of
	// those side effects should happen for a request this flow cannot
	// honor. Empty --to keeps defaulting to "running".
	target := valueFlag(rest, "--to")
	if target != "" && target != "running" {
		fmt.Fprintf(os.Stderr, "error: --execute requires --to running (got %q)\n", target)
		workAdvanceExecuteExit(2)
	}
	if target == "" {
		target = "running"
	}
	launchTimeout, err := executewire.ParseLaunchTimeoutFlag(valueFlag(rest, "--timeout"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v; no RunAttempt minted and envelope was not advanced\n", err)
		workAdvanceExecuteExit(2)
	}

	// Parse post-`--` for command + args. Everything before the `--` is
	// flag parsing; everything after is the subprocess argv. The first
	// post-`--` token is the program path; the rest are its args.
	restArgs := childArgs
	flagArgs := rest
	resumeVerifying := hasFlag(flagArgs, "--resume-verifying")
	var command string
	var commandArgs []string
	if len(restArgs) > 0 {
		command = restArgs[0]
		commandArgs = restArgs[1:]
	}
	if command == "" && prompt == "" {
		fmt.Fprintln(os.Stderr, "error: --execute requires either --prompt or a post-`--` command")
		workAdvanceExecuteExit(2)
	}

	// Validate work envelope exists. Read-only check -- we are about to
	// advance, not admit. The returned envelope feeds workspace resolution.
	wenv, err := store.Get(workID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "work envelope %q: %v\n", workID, err)
		workAdvanceExecuteExit(1)
	}
	if refusal := actuator.GovernedHarnessSourceRefusal(wenv, resumeVerifying); refusal != nil {
		refusal.RenderTo(os.Stderr)
		workAdvanceExecuteExit(1)
	}
	if expected := valueFlag(rest, "--expected-work-revision"); expected != "" && expected != work.ExecutionSourceRevision(wenv) {
		fmt.Fprintln(os.Stderr, "error: selected work changed before governed execution; no RunAttempt minted")
		workAdvanceExecuteExit(1)
	}
	// work_r7a_execution_selection_20260911: fall back to the saved selection's harness when --harness is omitted; refuse when neither is set.
	if harnessName == "" {
		if wenv.ExecutionSelection == nil || strings.TrimSpace(wenv.ExecutionSelection.Harness) == "" {
			fmt.Fprintln(os.Stderr, "error: --execute requires --harness <name> when the work has no saved execution_selection")
			workAdvanceExecuteExit(2)
		}
		harnessName = strings.TrimSpace(wenv.ExecutionSelection.Harness)
	}
	// work_r7a_execution_selection_20260911: --expected-selection-revision is
	// the CAS guard for the saved selection. On the execute path it is a
	// pre-side-effect check: a stale guard must produce ZERO execution and
	// lifecycle delta, so it is validated here — after the read-only
	// envelope load, before adapter resolution or the governed advance.
	if expectedSelRev := strings.TrimSpace(valueFlag(rest, "--expected-selection-revision")); expectedSelRev != "" {
		n, perr := strconv.Atoi(expectedSelRev)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "error: --expected-selection-revision %q must be an integer\n", expectedSelRev)
			workAdvanceExecuteExit(2)
		}
		found := 0
		if wenv.ExecutionSelection != nil {
			found = wenv.ExecutionSelection.Revision
		}
		if found != n {
			fmt.Fprintf(os.Stderr, "error: execution_selection revision changed: expected %d, found %d; no RunAttempt was minted and the envelope was not advanced\n", n, found)
			workAdvanceExecuteExit(1)
		}
	}

	// Resolve the adapter BEFORE any state change. An unknown harness name
	// or a missing adapter must fail closed here -- the old flow advanced
	// first and looked up second, so a typo in --harness left the work
	// stuck in "running" with no RunAttempt and no recovery path.
	launchProfileID := ""
	if explicitSelection != nil {
		launchProfileID = strings.TrimSpace(explicitSelection.LaunchProfileID)
	} else if wenv.ExecutionSelection != nil && harnessName == strings.TrimSpace(wenv.ExecutionSelection.Harness) {
		// Saved-selection fallback: the work's own choice drives the profile.
		launchProfileID = wenv.ExecutionSelection.LaunchProfileID
	}
	if err := executewire.ValidateExpectedExecutionTuple(homePath, valueFlag(rest, "--expected-execution-tuple"), harnessName, launchProfileID, wenv, now); err != nil {
		fmt.Fprintf(os.Stderr, "error: execution tuple handoff: %v; no RunAttempt minted and envelope was not advanced\n", err)
		workAdvanceExecuteExit(1)
	}
	runAttemptStore := runattempt.Open(homePath)
	registry := harness.NewAdapterRegistry(runAttemptStore)
	adapter, ok := registry.AdapterByName(harnessName)
	if !ok {
		gateRefusal{
			Command: "compound work advance --execute",
			Gate:    "adapter_resolution",
			Refused: fmt.Sprintf("no adapter is registered for harness %q, so no RunAttempt was minted and the envelope was not advanced", harnessName),
			Tried: []string{
				fmt.Sprintf("--harness %s against the adapter registry: no match", harnessName),
				"registered adapters: " + strings.Join(registry.Names(), ", "),
			},
			DoThis: []string{
				fmt.Sprintf("compound work advance %s --execute --harness <name>", workID),
				"compound executors --json   # which lanes are actually live, with their cost limits",
			},
			Prevents: "advancing the envelope to running before the adapter is known. Resolving second left a typo'd --harness stuck in running with no RunAttempt and no recovery path.",
		}.writeTo(os.Stderr)
		workAdvanceExecuteExit(1)
	}

	// Resolve the execution workspace BEFORE advancing. Never default to
	// COMPOUND_HOME -- running a real code-editing harness there edits the
	// state/config dir, not the project. See resolveExecuteWorkspace.
	workspace, workspaceTried := resolveExecuteWorkspace(rest, wenv.GitDelivery)
	if workspace == "" {
		gateRefusal{
			Command: "compound work advance --execute",
			Gate:    "workspace_resolution",
			Refused: fmt.Sprintf("no execution workspace could be resolved for %s, so no RunAttempt was minted and the envelope was not advanced", workID),
			Tried:   workspaceTried,
			DoThis: []string{
				fmt.Sprintf("compound work advance %s --execute --harness %s --workspace <project-path>", workID, harnessName),
				fmt.Sprintf("compound work show %s   # confirm git_delivery.target_worktree_root if the work should carry its own workspace", workID),
			},
			Prevents: "running a code-editing harness against COMPOUND_HOME. There is no home fallback by design: the harness would edit the state and config directory instead of the project, and the edits would look like successful work.",
		}.writeTo(os.Stderr)
		workAdvanceExecuteExit(2)
	}

	// Resolving a workspace is not proving one. The ladder returns whatever
	// path it found -- including a git_delivery.target_worktree_root recorded
	// months ago for a worktree since retired -- and Preflight never touches
	// Workspace either. The path therefore reached cmd.Start(), which failed
	// with "chdir: no such file or directory" AFTER the governed advance to
	// running; failTerm then terminalized the RunAttempt and the caller closed
	// the envelope "failed (audit_only)". failed is terminal, so a stale root
	// destroyed the work item in a single invocation with no recovery path.
	// Stat before the state change so the envelope survives a bad path.
	if observed, ok := checkExecuteWorkspaceExists(workspace); !ok {
		gateRefusal{
			Command: "compound work advance --execute",
			Gate:    "workspace_existence",
			Refused: fmt.Sprintf("the resolved execution workspace %q is not a usable directory, so no RunAttempt was minted and %s was not advanced", workspace, workID),
			Tried:   append(workspaceTried, observed),
			DoThis: []string{
				fmt.Sprintf("compound work advance %s --execute --harness %s --workspace <existing-checkout>", workID, harnessName),
				fmt.Sprintf("compound work show %s   # git_delivery.target_worktree_root may name a worktree that was retired", workID),
			},
			Prevents: "burning the work item. The launch fails with chdir, and a failed launch terminalizes the envelope 'failed (audit_only)' -- a terminal state with no recovery path.",
		}.writeTo(os.Stderr)
		workAdvanceExecuteExit(2)
	}

	if explicitSelection != nil && harnessName != strings.TrimSpace(explicitSelection.Harness) {
		// A one-run override across harness families is not carried by the
		// registry lane adapters (an explicit profile names a lane of its own
		// harness); refuse before any state change instead of silently
		// launching the saved harness under the override's profile.
		fmt.Fprintf(os.Stderr, "error: --launch-profile %q resolves harness %q, which is not the invoked harness %q; no RunAttempt was minted and the envelope was not advanced\n",
			explicitSelection.LaunchProfileID, explicitSelection.Harness, harnessName)
		workAdvanceExecuteExit(1)
	}
	// Resolve adapter-owned prerequisites before the governed state change.
	// Tuple-backed lanes use this seam to prove their Compound tuple, wrapper,
	// permission posture, and declared capacity. A preflight failure leaves the
	// envelope claimed and mints no execution RunAttempt.
	runInput := harness.RunInput{
		WorkID:          workID,
		LaunchProfileID: launchProfileID,
		Harness:         harnessName,
		Workspace:       workspace,
		Command:         command,
		Args:            commandArgs,
		Prompt:          prompt,
		PermissionMode:  valueFlag(rest, "--permission-mode"),
		LaneID:          actor,
		Now:             time.Now().UTC(),
		ProofRefs:       append([]string(nil), wenv.ArtifactIDs...),
	}
	// The same saved or one-run tuple must select the actual Codex executable.
	runInput, err = executewire.BindSelectionBeforePreflight(wenv, harnessName, explicitSelection, runInput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bind execution selection: %v; no RunAttempt minted and envelope was not advanced\n", err)
		workAdvanceExecuteExit(1)
	}
	// Capacity preflight and guarded start must see the same bounded run.
	// The signal-derived context still owns cancellation of the process group.
	runInput.Timeout = launchTimeout
	if preflight, ok := adapter.(harness.PreflightAdapter); ok {
		resolved, err := preflight.Preflight(launchCtx, runInput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "harness %s preflight: %v\n", harnessName, err)
			workAdvanceExecuteExit(1)
		}
		runInput = resolved
	}
	// A hook-governed lane must run the full roster for governed spawn; refuse
	// (never install) before any state change (hookplan.GovernedFullRosterGate).
	if gate := hookplan.GovernedFullRosterGate(harnessName); gate != nil {
		gateRefusal{Command: gate.Command, Gate: gate.Gate, Refused: gate.Refused, Tried: gate.Tried, DoThis: gate.DoThis, Prevents: gate.Prevents}.writeTo(os.Stderr)
		workAdvanceExecuteExit(1)
	}
	note := valueFlag(rest, "--note")
	if note == "" {
		note = "harness execute: " + harnessName
	}
	executed, err := actuator.ExecuteGovernedHarnessAdvance(launchCtx, homePath, store, wenv, adapter, runInput, actor, target, note, existingRunAttempts, resumeVerifying, now, actuator.GovernedHarnessContinuationOptions{RetainFailedAttempt: hasFlag(flagArgs, "--keep-verifying-on-failure")})
	var laneRefusal *actuator.LaneRefusal
	if errors.As(err, &laneRefusal) && laneRefusal != nil { // R3B 2026-09-04: explicit --permission-mode on unready lane refused BEFORE mint.
		laneRefusal.RenderTo(os.Stderr)
		workAdvanceExecuteExit(1)
	}
	die(err)
	w := executed.RunningWork
	fmt.Printf("%s -> %s policy=%s\n", w.ID, w.State, w.PolicyDecisionID)
	out := executed.Output
	runErr := executed.RunError
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "harness %s run: %v\n", harnessName, runErr)
	}
	finalized := executed.Finalized
	if finalized.WorkID != "" {
		if finalized.FailedAuditOnly {
			fmt.Printf("%s -> %s (audit_only)\n", finalized.WorkID, finalized.FinalState)
		} else {
			fmt.Printf("%s -> %s\n", finalized.WorkID, finalized.FinalState)
		}
	}
	for _, warning := range finalized.Warnings {
		fmt.Fprintln(os.Stderr, warning)
	}

	// Print the RunAttempt summary on the same stdout stream as the
	// advance output. Operators can pipe to jq or grep for the id.
	fmt.Printf("run_attempt_id: %s\n", out.RunAttempt.ID)
	fmt.Printf("status: %s\n", out.RunAttempt.Status)
	fmt.Printf("harness: %s\n", out.RunAttempt.Harness)
	// The per-run selection snapshot, on the same boundary: what this run
	// was ASKED to run (source, profile, consumed revision, effort).
	if sel := out.RunAttempt.ExecutionSelection; sel != nil {
		fmt.Printf("selection: source=%s profile=%s harness=%s rev=%d effort=%q\n",
			sel.Source, sel.LaunchProfileID, sel.Harness, sel.SelectionRevision, sel.RequestedEffort)
	} else {
		fmt.Printf("selection: none\n")
	}
	if out.TranscriptPath != "" {
		fmt.Printf("transcript: %s\n", out.TranscriptPath)
	}
	fmt.Printf("exit_code: %d\n", out.ExitCode)
	if out.TimedOut {
		fmt.Printf("timed_out: true\n")
	}
	fmt.Printf("duration_ms: %d\n", out.Duration.Milliseconds())

	// Non-zero exit on failed harness run -- the operator contract
	// matches `compound harness run`. A failed work lifecycle (state
	// "failed") is independent of the process exit code; we use the exit
	// code to signal "this whole thing did not succeed", which CI / make
	// / orchestration can react to.
	if !finalized.Succeeded {
		workAdvanceExecuteExit(1)
	}
}
