package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"compound/internal/contract"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// The delegate teardown reaper is a composition-root seam like the harness
// readiness guard: only this executable installs the real-git implementation,
// so package tests keep driving a fake and never touch a repository.
func init() {
	work.DelegateWorktreeLockReaper = work.NewGitWorktreeLockReaper()
}

type delegateCLIOptions struct {
	executionProfile                                   work.DelegateExecutionProfileIntent
	workID, harness, actor, session, prompt, workspace string
	command                                            string
	args                                               []string
	expectedMinutes                                    int
	instructionBlockPreset                             string
	confirm, dryRun, jsonOut, experimentalMutation     bool
}

type steerCLIOptions struct {
	workID, actor, text                  string
	enter, jsonOut, experimentalMutation bool
}

type delegateSweepCLIOptions struct {
	workID                                string
	dryRun, jsonOut, experimentalMutation bool
}

type delegateProgressCLIOptions struct {
	workID                         string
	done, empty, failed, remaining int
	jsonOut, experimentalMutation  bool
}

func parseDelegateSweepCLI(args []string) (delegateSweepCLIOptions, error) {
	var out delegateSweepCLIOptions
	var positional []string
	for _, arg := range args {
		switch arg {
		case "--dry-run":
			out.dryRun = true
		case "--json":
			out.jsonOut = true
		case work.ExperimentalDelegateMutationsFlag:
			out.experimentalMutation = true
		default:
			if strings.HasPrefix(arg, "-") {
				return out, fmt.Errorf("unknown flag %s", arg)
			}
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		return out, errors.New("sweep requires exactly one work_id")
	}
	out.workID = positional[0]
	return out, nil
}

func parseDelegateProgressCLI(args []string) (delegateProgressCLIOptions, error) {
	var out delegateProgressCLIOptions
	var positional []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--json":
			out.jsonOut = true
		case work.ExperimentalDelegateMutationsFlag:
			out.experimentalMutation = true
		case "--done", "--empty", "--failed", "--remaining":
			if i+1 >= len(args) {
				return out, fmt.Errorf("%s requires a value", arg)
			}
			i++
			value, err := strconv.Atoi(args[i])
			if err != nil || value < 0 {
				return out, fmt.Errorf("%s must be a non-negative integer", arg)
			}
			switch arg {
			case "--done":
				out.done = value
			case "--empty":
				out.empty = value
			case "--failed":
				out.failed = value
			case "--remaining":
				out.remaining = value
			}
			seen[arg] = true
		default:
			if strings.HasPrefix(arg, "-") {
				return out, fmt.Errorf("unknown flag %s", arg)
			}
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		return out, errors.New("progress requires exactly one work_id")
	}
	for _, required := range []string{"--done", "--empty", "--failed", "--remaining"} {
		if !seen[required] {
			return out, fmt.Errorf("progress requires %s", required)
		}
	}
	out.workID = positional[0]
	return out, nil
}

// runDelegateStatusCLI is the testable command edge for
// `compound work delegate status <work_id>`. Read-only; writes nothing.
func runDelegateStatusCLI(args []string, store *work.Store, runner work.SessionRunner, now time.Time, stdout, stderr io.Writer) int {
	var positional []string
	for _, arg := range args {
		switch arg {
		case "--json":
			// Status is JSON by definition; accept the explicit flag so the
			// documented machine-readable invocation stays composable.
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintln(stderr, "error: unknown flag", arg)
				return 2
			}
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		fmt.Fprintln(stderr, "error: status requires exactly one work_id and no flags")
		return 2
	}
	if store == nil || runner == nil {
		fmt.Fprintln(stderr, "error: delegate runtime is unavailable")
		return 1
	}
	obs, err := store.DelegateStatus(positional[0], runner)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obs); err != nil {
		fmt.Fprintln(stderr, "error: encode status:", err)
		return 1
	}
	_ = now
	return 0
}

// runDelegateSweepCLI is the testable command edge for
// `compound work delegate sweep <work_id> [--dry-run] [--json]`. Writes
// a work.delegate.exited artifact and kills the finished pane (NOT a
// live one). Dry-run writes/kills nothing.
func runDelegateSweepCLI(args []string, store *work.Store, runner work.SessionRunner, now time.Time, stdout, stderr io.Writer) int {
	if store == nil || runner == nil {
		fmt.Fprintln(stderr, "error: delegate runtime is unavailable")
		return 1
	}
	opts, err := parseDelegateSweepCLI(args)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if opts.dryRun {
		obs, err := store.DelegateStatus(opts.workID, runner)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if opts.jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(obs); err != nil {
				fmt.Fprintln(stderr, "error: encode dry-run:", err)
				return 1
			}
			return 0
		}
		fmt.Fprintf(stdout, "dry-run: would sweep work=%s bound=%t session=%s run_attempt_status=%s\n",
			obs.WorkID, obs.Bound, obs.Session, obs.RunAttemptStatus)
		_ = now
		return 0
	}
	if !opts.experimentalMutation {
		return delegateCLIUsageError(stderr, "delegate", fmt.Errorf("mutating delegate sweep requires %s and is unsupported in v0.1", work.ExperimentalDelegateMutationsFlag))
	}
	store.EnableExperimentalDelegateMutations()
	// Resolve the binding, probe the pane, and call DelegateSweep.
	binding, err := store.DelegateSweepResolve(opts.workID)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if binding == nil {
		fmt.Fprintln(stderr, "error: no binding for work", opts.workID)
		return 1
	}
	st, sErr := runner.SessionStatus(binding.Session)
	if sErr != nil {
		fmt.Fprintln(stderr, "error:", sErr)
		return 1
	}
	out, err := store.DelegateSweep(binding, st, runner, "operator", now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintln(stderr, "error: encode sweep:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "swept work=%s session=%s run_attempt=%s observed=%s status=%s worktree_lock=%s\n",
		out.WorkID, out.Session, out.RunAttemptID, out.ObservedExit, out.TerminalStatus, out.WorktreeLock)
	if out.KillError != "" {
		fmt.Fprintf(stdout, "kill-error: %s\n", out.KillError)
	}
	if out.WorktreeLockError != "" {
		fmt.Fprintf(stdout, "worktree-lock-error: %s\n", out.WorktreeLockError)
	}
	return 0
}

func runDelegateProgressCLI(args []string, store *work.Store, now time.Time, stdout, stderr io.Writer) int {
	opts, err := parseDelegateProgressCLI(args)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if !opts.experimentalMutation {
		return delegateCLIUsageError(stderr, "delegate", fmt.Errorf("delegate progress requires %s and is unsupported in v0.1", work.ExperimentalDelegateMutationsFlag))
	}
	store.EnableExperimentalDelegateMutations()
	result, err := store.DelegateProgress(opts.workID, runattempt.RunProgress{
		Done: opts.done, Empty: opts.empty, Failed: opts.failed, Remaining: opts.remaining,
	}, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if opts.jsonOut {
		if err := encodeDelegateCLIJSON(stdout, result); err != nil {
			fmt.Fprintln(stderr, "error: encode progress:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "delegate progress: work=%s run_attempt=%s done=%d empty=%d failed=%d remaining=%d\n",
		result.WorkID, result.RunAttemptID, result.Progress.Done, result.Progress.Empty, result.Progress.Failed, result.Progress.Remaining)
	return 0
}

// runWorkDelegateCLI is the testable command edge for work
// delegate|steer|attach|<status>|<sweep>. Parsing and rendering live
// here; lifecycle meaning remains in internal/work and process
// supervision remains behind runner.
//
// `compound work delegate status <work_id>` and
// `compound work delegate sweep <work_id> [--dry-run] [--json]` are
// surfaced through the same top-level verb because the work-cli
// dispatcher already routes `compound work delegate` here -- adding a
// separate top-level verb would split the documentation surface. They
// are special-cased BEFORE parseDelegate so "status" and "sweep" do
// not become a work_id.
func runWorkDelegateCLI(sub string, args []string, store *work.Store, runner work.SessionRunner, attach func([]string) error, now time.Time, stdout, stderr io.Writer) int {
	if store == nil || runner == nil {
		fmt.Fprintln(stderr, "error: delegate runtime is unavailable")
		return 1
	}
	switch sub {
	case "delegate":
		// Sub-dispatch: status/sweep/progress before parseDelegate.
		if len(args) > 0 {
			switch args[0] {
			case "status":
				return runDelegateStatusCLI(args[1:], store, runner, now, stdout, stderr)
			case "sweep":
				return runDelegateSweepCLI(args[1:], store, runner, now, stdout, stderr)
			case "progress":
				return runDelegateProgressCLI(args[1:], store, now, stdout, stderr)
			}
		}
		opts, err := parseDelegateCLI(args)
		if err != nil {
			return delegateCLIUsageError(stderr, "delegate", err)
		}
		if !opts.dryRun {
			if !opts.experimentalMutation {
				return delegateCLIUsageError(stderr, "delegate", fmt.Errorf("delegate mutation requires %s and is unsupported in v0.1", work.ExperimentalDelegateMutationsFlag))
			}
			store.EnableExperimentalDelegateMutations()
		}
		if opts.instructionBlockPreset != "" {
			augmented, appendErr := contract.AppendDelegateVariantToPrompt(opts.harness, opts.workspace, opts.instructionBlockPreset, opts.prompt)
			if appendErr != nil {
				fmt.Fprintln(stderr, "error: --instruction-block:", appendErr)
				return 1
			}
			opts.args = []string{augmented}
		}
		result, err := store.Delegate(work.DelegateOptions{
			WorkID:           opts.workID,
			Harness:          opts.harness,
			ExecutionProfile: opts.executionProfile,
			Session:          opts.session,
			Actor:            opts.actor,
			Command:          opts.command,
			Args:             opts.args,
			Workspace:        opts.workspace,
			ExpectedMinutes:  opts.expectedMinutes,
			Confirm:          opts.confirm,
			DryRun:           opts.dryRun,
			Now:              now,
		}, runner)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if opts.jsonOut {
			if err := encodeDelegateCLIJSON(stdout, result); err != nil {
				fmt.Fprintln(stderr, "error: encode delegate result:", err)
				return 1
			}
		} else {
			renderDelegateResult(stdout, result)
		}
		if result.Status == "refused" {
			return 1
		}
		return 0

	case "steer":
		opts, err := parseSteerCLI(args)
		if err != nil {
			return delegateCLIUsageError(stderr, "steer", err)
		}
		if !opts.experimentalMutation {
			return delegateCLIUsageError(stderr, "steer", fmt.Errorf("steer requires %s and is unsupported in v0.1", work.ExperimentalDelegateMutationsFlag))
		}
		store.EnableExperimentalDelegateMutations()
		result, err := store.Steer(work.SteerOptions{
			WorkID: opts.workID,
			Actor:  opts.actor,
			Text:   opts.text,
			Enter:  opts.enter,
			Now:    now,
		}, runner)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if opts.jsonOut {
			if err := encodeDelegateCLIJSON(stdout, result); err != nil {
				fmt.Fprintln(stderr, "error: encode steer result:", err)
				return 1
			}
		} else {
			fmt.Fprintf(stdout, "delegate steered: work=%s session=%s event=%s\n", result.WorkID, result.Session, result.EventID)
		}
		return 0

	case "attach":
		workID, err := parseAttachCLI(args)
		if err != nil {
			return delegateCLIUsageError(stderr, "attach", err)
		}
		result, err := store.Attach(work.AttachOptions{WorkID: workID}, runner)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if attach == nil {
			fmt.Fprintln(stderr, "error: attach runtime is unavailable")
			return 1
		}
		if err := attach(result.AttachArgv); err != nil {
			fmt.Fprintln(stderr, "error: attach session:", err)
			return 1
		}
		return 0
	default:
		return delegateCLIUsageError(stderr, sub, fmt.Errorf("unknown governed execution verb %q", sub))
	}
}

func parseDelegateCLI(args []string) (delegateCLIOptions, error) {
	var out delegateCLIOptions
	flagArgs, rawCommand := splitDelegateRawCommand(args)
	seenPrompt := false
	positionals := make([]string, 0, 1)
	for i := 0; i < len(flagArgs); i++ {
		arg := flagArgs[i]
		readValue := func() (string, error) {
			if i+1 >= len(flagArgs) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return flagArgs[i], nil
		}
		switch arg {
		case "--harness", "--actor", "--session", "--prompt", "--workspace", "--expected-minutes",
			"--execution-profile", "--role", "--capability-class", "--client-harness",
			"--transport", "--route-alias", "--model", "--semantic-effort", "--effort",
			"--orchestration", "--privacy-ceiling", "--launch-profile", "--instruction-block":
			value, err := readValue()
			if err != nil {
				return out, err
			}
			switch arg {
			case "--harness":
				out.harness = value
			case "--actor":
				out.actor = value
			case "--session":
				out.session = value
			case "--prompt":
				out.prompt, seenPrompt = value, true
			case "--workspace":
				out.workspace = value
			case "--execution-profile":
				out.executionProfile.ProfileID = value
			case "--launch-profile":
				out.executionProfile.LaunchProfileID = value
			case "--role":
				out.executionProfile.Role = value
			case "--capability-class":
				out.executionProfile.CapabilityClass = value
			case "--client-harness":
				out.executionProfile.ClientHarnessID = value
			case "--transport":
				out.executionProfile.TransportID = value
			case "--route-alias":
				out.executionProfile.RouteAlias = value
			case "--model":
				out.executionProfile.ModelConstraint = value
			case "--semantic-effort":
				out.executionProfile.SemanticEffort = value
			case "--effort":
				out.executionProfile.NativeEffort = value
			case "--orchestration":
				out.executionProfile.OrchestrationPolicy = value
			case "--privacy-ceiling":
				out.executionProfile.PrivacyCeiling = value
			case "--instruction-block":
				out.instructionBlockPreset = value
			case "--expected-minutes":
				minutes, parseErr := strconv.Atoi(value)
				if parseErr != nil || minutes <= 0 {
					return out, errors.New("--expected-minutes must be a positive integer")
				}
				out.expectedMinutes = minutes
			}
		case "--confirm":
			out.confirm = true
		case "--dry-run":
			out.dryRun = true
		case "--json":
			out.jsonOut = true
		case work.ExperimentalDelegateMutationsFlag:
			out.experimentalMutation = true
		default:
			if strings.HasPrefix(arg, "-") {
				return out, fmt.Errorf("unknown flag %s", arg)
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return out, errors.New("delegate requires exactly one work_id")
	}
	out.workID = positionals[0]
	if strings.TrimSpace(out.harness) == "" {
		return out, errors.New("delegate requires --harness")
	}
	if strings.TrimSpace(out.executionProfile.ProfileID) == "" &&
		(strings.TrimSpace(out.executionProfile.LaunchProfileID) != "" ||
			strings.TrimSpace(out.executionProfile.Role) != "" ||
			strings.TrimSpace(out.executionProfile.CapabilityClass) != "" ||
			strings.TrimSpace(out.executionProfile.ClientHarnessID) != "" ||
			strings.TrimSpace(out.executionProfile.TransportID) != "" ||
			strings.TrimSpace(out.executionProfile.RouteAlias) != "" ||
			strings.TrimSpace(out.executionProfile.ModelConstraint) != "" ||
			strings.TrimSpace(out.executionProfile.SemanticEffort) != "" ||
			strings.TrimSpace(out.executionProfile.NativeEffort) != "" ||
			strings.TrimSpace(out.executionProfile.OrchestrationPolicy) != "" ||
			strings.TrimSpace(out.executionProfile.PrivacyCeiling) != "") {
		return out, errors.New("execution selectors require --execution-profile")
	}
	if seenPrompt && strings.TrimSpace(out.prompt) == "" {
		return out, errors.New("--prompt requires non-empty text")
	}
	if seenPrompt && len(rawCommand) > 0 {
		return out, errors.New("pass either --prompt or a raw command after --, not both")
	}
	if seenPrompt {
		out.args = []string{out.prompt}
	}
	if rawCommand != nil {
		if len(rawCommand) == 0 || strings.TrimSpace(rawCommand[0]) == "" {
			return out, errors.New("-- must be followed by a command")
		}
		out.command = rawCommand[0]
		out.args = append([]string(nil), rawCommand[1:]...)
	}
	return out, nil
}

func splitDelegateRawCommand(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func parseSteerCLI(args []string) (steerCLIOptions, error) {
	var out steerCLIOptions
	positionals := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--text", "--actor":
			if i+1 >= len(args) {
				return out, fmt.Errorf("%s requires a value", arg)
			}
			i++
			if arg == "--text" {
				out.text = args[i]
			} else {
				out.actor = args[i]
			}
		case "--enter":
			out.enter = true
		case "--json":
			out.jsonOut = true
		case work.ExperimentalDelegateMutationsFlag:
			out.experimentalMutation = true
		default:
			if strings.HasPrefix(arg, "-") {
				return out, fmt.Errorf("unknown flag %s", arg)
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return out, errors.New("steer requires exactly one work_id")
	}
	if strings.TrimSpace(out.text) == "" {
		return out, errors.New("steer requires --text")
	}
	out.workID = positionals[0]
	return out, nil
}

func parseAttachCLI(args []string) (string, error) {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" || strings.HasPrefix(args[0], "-") {
		return "", errors.New("attach requires exactly one work_id and no flags")
	}
	return args[0], nil
}

// delegateCommandNeedsWritableStore classifies the request from the same
// parsers the command will execute. This prevents an experimental flag-shaped
// value or raw command token from opening the writable work habitat before the
// request has actually opted into mutation.
func delegateCommandNeedsWritableStore(sub string, args []string) bool {
	switch sub {
	case "attach":
		return false
	case "steer":
		opts, err := parseSteerCLI(args)
		return err == nil && opts.experimentalMutation
	case "delegate":
		if len(args) > 0 {
			switch args[0] {
			case "status":
				return false
			case "sweep":
				opts, err := parseDelegateSweepCLI(args[1:])
				return err == nil && !opts.dryRun && opts.experimentalMutation
			case "progress":
				opts, err := parseDelegateProgressCLI(args[1:])
				return err == nil && opts.experimentalMutation
			}
		}
		opts, err := parseDelegateCLI(args)
		return err == nil && !opts.dryRun && opts.experimentalMutation
	default:
		return true
	}
}

func delegateCLIUsageError(stderr io.Writer, sub string, err error) int {
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
	}
	fmt.Fprintln(stderr, workUsage(sub))
	return 2
}

func encodeDelegateCLIJSON(stdout io.Writer, value any) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func renderDelegateResult(stdout io.Writer, result work.DelegateResult) {
	switch result.Status {
	case "preview":
		fmt.Fprintf(stdout, "delegate: status=preview work=%s lane=%s", result.WorkID, result.Lane)
		if result.Decision != nil {
			fmt.Fprintf(stdout, " capacity=%s refused=%t confirmation_required=%t", result.Decision.Capacity.State, result.Decision.Refused, result.Decision.ConfirmationRequired)
		}
		fmt.Fprintln(stdout)
		if result.Decision != nil && strings.TrimSpace(result.Decision.Message) != "" {
			fmt.Fprintln(stdout, "decision:", result.Decision.Message)
		}
	case "spawned":
		fmt.Fprintf(stdout, "delegate spawned: work=%s session=%s harness=%s lane=%s event=%s\n", result.WorkID, result.Binding.Session, result.Binding.Harness, result.Binding.Lane, result.EventID)
		fmt.Fprintf(stdout, "attach: compound work attach %s\n", result.WorkID)
	case "refused":
		fmt.Fprintf(stdout, "delegate refused: work=%s", result.WorkID)
		if result.Refusal != nil {
			fmt.Fprintf(stdout, " reason=%s", result.Refusal.Reason)
			if result.Refusal.Session != "" {
				fmt.Fprintf(stdout, " session=%s", result.Refusal.Session)
			}
		}
		fmt.Fprintln(stdout)
		if result.Refusal != nil && strings.TrimSpace(result.Refusal.Message) != "" {
			fmt.Fprintln(stdout, "decision:", result.Refusal.Message)
		}
	}
}

func runDelegateAttachProcess(argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return errors.New("empty attach command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func productionDelegateAttach(argv []string) error {
	return runDelegateAttachProcess(argv, os.Stdin, os.Stdout, os.Stderr)
}
