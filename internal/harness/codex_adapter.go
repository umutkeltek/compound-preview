package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/routeevidence"
	"compound/internal/runattempt"
)

// CodexAdapter drives the Codex CLI as a subprocess and wraps the
// execution in a RunAttempt. Mirrors ClaudeAdapter: thin shim over
// runWithSubprocess, harness-specific argv, harness name "codex" on
// the RunAttempt.
//
// Today Codex's non-interactive invocation is:
//
//	codex exec [PROMPT]
//
// The prompt is POSITIONAL: current Codex CLI rejects a --prompt flag
// with exit 2 ("unexpected argument '--prompt'"), which silently failed
// every governed dispatch until find_d435b414ef9b pinned it live
// (ratt_20260722T111145Z_f8859f4b). Flags remain best-effort matches
// for the public Codex CLI; a future slice wires an adapter test
// against the real binary so CLI drift fails in CI, not at dispatch.
//
// The adapter does NOT implement Codex's hook protocol. It is a
// subprocess wrapper, not a hook consumer. The HarnessRegistry continues
// to advertise Codex's capabilities from its manifest.
type CodexAdapter struct {
	store                  *runattempt.Store
	heartbeatInterval      time.Duration
	verifyNativeExecutable func(string) error // test-only seam
}

// NewCodexAdapter returns the production adapter. Heartbeats every 5s.
func NewCodexAdapter(store *runattempt.Store) *CodexAdapter {
	return &CodexAdapter{store: store, heartbeatInterval: defaultHeartbeatInterval}
}

// NewCodexAdapterWithInterval returns an adapter with a custom heartbeat
// cadence. Test-only.
func NewCodexAdapterWithInterval(store *runattempt.Store, interval time.Duration) *CodexAdapter {
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	return &CodexAdapter{store: store, heartbeatInterval: interval}
}

// Name returns the manifest adapter id for Codex. Future Registry
// integration (C.3) keys on this.
func (a *CodexAdapter) Name() string { return "codex" }

func (a *CodexAdapter) validateCodexInput(in *RunInput) error {
	native := filepath.Base(strings.TrimSpace(in.Command)) == "codex"
	if native {
		if _, _, err := codexNativeSelection(*in); err != nil {
			return err
		}
	} else if in.ExecutionSelectionSnapshot != nil && strings.TrimSpace(in.ExecutionSelectionSnapshot.RequestedEffort) != "" {
		return fmt.Errorf("codex adapter: selected wrapper cannot attest requested native effort")
	}
	bind := os.Getenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION")
	if bind != "" && bind != "1" {
		return fmt.Errorf("codex subscription binding: opt-in must equal 1")
	}
	if bind == "1" {
		if strings.TrimSpace(in.RequestedModelID) == "" || strings.TrimSpace(in.RequestedProviderRoute) == "" {
			return fmt.Errorf("codex subscription binding: explicit selected native tuple required")
		}
		if err := validateCodexSubscriptionInvocation(*in); err != nil {
			return err
		}
	}
	if bind != "1" && in.PermissionMode != "plan" {
		return nil
	}
	if !native {
		return fmt.Errorf("codex adapter: native codex executable required for read-only enforcement")
	}
	resolved, err := resolvedCodexCommand(in.Command)
	if err != nil {
		return err
	}
	verify := a.verifyNativeExecutable
	if verify == nil {
		verify = verifyNativeCodexExecutable
	}
	if err := verify(resolved); err != nil {
		return err
	}
	in.Command = resolved
	return nil
}

// Preflight resolves a selected Compound LaneTuple to its owned executable
// before a governed caller claims or advances work. Legacy direct callers
// with no selected tuple retain the raw codex command.
func (a *CodexAdapter) Preflight(_ context.Context, in RunInput) (RunInput, error) {
	requestedModel := strings.TrimSpace(in.RequestedModelID)
	requestedRoute := strings.TrimSpace(in.RequestedProviderRoute)
	command := a.Name()
	if requestedModel != "" || requestedRoute != "" {
		if requestedModel == "" || requestedRoute == "" {
			return RunInput{}, fmt.Errorf("codex adapter: selected tuple requires both model and provider route")
		}
		manifest, _, currentRevision, err := routeevidence.LoadModelCapabilitiesSnapshot(a.store.Home())
		if err != nil {
			return RunInput{}, fmt.Errorf("codex adapter: load Compound model-router authority: %w", err)
		}
		if expectedRevision := strings.TrimSpace(in.ExpectedModelRouterRevision); expectedRevision != "" && currentRevision != expectedRevision {
			return RunInput{}, fmt.Errorf("codex adapter: model-router revision drifted: planned=%q current=%q", expectedRevision, currentRevision)
		}
		modelID, tuple, err := routeevidence.ResolveModelCapabilityTuple(manifest, requestedModel)
		if err != nil {
			return RunInput{}, fmt.Errorf("codex adapter: resolve selected tuple: %w", err)
		}
		if modelID != requestedModel || canonicalHarnessName(tuple.Harness) != a.Name() || strings.TrimSpace(tuple.ProviderRoute) != requestedRoute {
			return RunInput{}, fmt.Errorf(
				"codex adapter: selected route drifted: planned model=%q route=%q resolved model=%q route=%q harness=%q",
				requestedModel, requestedRoute, modelID, tuple.ProviderRoute, tuple.Harness,
			)
		}
		command, err = routeevidence.ResolveModelCapabilityExecutable(tuple.Aliases)
		if err != nil {
			return RunInput{}, fmt.Errorf("codex adapter: selected tuple %q: %w", modelID, err)
		}
	} else if _, err := exec.LookPath(command); err != nil {
		return RunInput{}, fmt.Errorf("codex adapter: resolve codex executable: %w", err)
	}
	in.Command = command
	if err := a.validateCodexInput(&in); err != nil {
		return RunInput{}, err
	}
	in.preflighted = true
	return in, nil
}

// Run drives the codex subprocess end-to-end. Mirrors ClaudeAdapter.Run.
func (a *CodexAdapter) Run(ctx context.Context, in RunInput) (out RunOutput, runErr error) {
	if !in.preflighted {
		resolved, err := a.Preflight(ctx, in)
		if err != nil {
			return RunOutput{}, err
		}
		in = resolved
	}
	in.Harness = a.Name()
	if err := a.validateCodexInput(&in); err != nil {
		return RunOutput{}, err
	}
	if os.Getenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION") == "1" || in.PermissionMode == "plan" {
		selected := in.Command
		original, err := os.Stat(selected)
		if err != nil {
			return RunOutput{}, fmt.Errorf("codex adapter: native executable unavailable")
		}
		verify := a.verifyNativeExecutable
		if verify == nil {
			verify = verifyNativeCodexExecutable
		}
		prior := in.StartGuard
		in.StartGuard = func(ctx context.Context, start func() error) error {
			checkedStart := func() error {
				current, err := os.Stat(selected)
				if err != nil || !os.SameFile(original, current) {
					return fmt.Errorf("codex adapter: native executable changed before start")
				}
				if err := verify(selected); err != nil {
					return err
				}
				return start()
			}
			if prior != nil {
				return prior(ctx, checkedStart)
			}
			return checkedStart()
		}
	}
	nativeCommand := filepath.Base(strings.TrimSpace(in.Command)) == "codex"
	if nativeCommand {
		_, nativeEffort, _ := codexNativeSelection(in) // validated in Preflight and again above
		if in.ExecutionSelectionSnapshot != nil && nativeEffort != "" {
			in.ExecutionSelectionSnapshot.AppliedEffort = nativeEffort
			in.ExecutionSelectionSnapshot.EffortEvidence = "argv:model_reasoning_effort"
		}
	}
	bind := os.Getenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION")
	if bind == "1" {
		var isolatedHome string
		defer func() {
			if isolatedHome != "" {
				if err := os.RemoveAll(isolatedHome); err != nil {
					runErr = errors.Join(runErr, fmt.Errorf("codex subscription binding: isolated home cleanup failed"))
				}
			}
		}()
		return runWithSubprocessOptions(ctx, a.store, a.Name(), in,
			func(ctx context.Context, in RunInput) *exec.Cmd {
				return a.buildCmdWithHome(ctx, in, isolatedHome)
			}, a.heartbeatInterval, subprocessRunOptions{
				PrepareEvidence: func() error {
					var err error
					isolatedHome, err = os.MkdirTemp("", "codex-subscription-")
					if err != nil {
						return fmt.Errorf("codex subscription binding: isolated home unavailable")
					}
					return writeCodexSubscriptionSnapshot(isolatedHome)
				},
			})
	}
	return runWithSubprocess(ctx, a.store, a.Name(), in, a.buildCmd, a.heartbeatInterval)
}

// buildCmd assembles `codex exec [PROMPT]` (positional — see the type
// doc for the --prompt drift this replaces). Args[0] is used as the
// prompt if Prompt is empty (caller convenience); the explicit Prompt
// field wins.
//
// CODEX_HOME isolation: every compound-dispatched codex invocation gets a
// fresh CODEX_HOME under os.TempDir so two concurrent runs (or an exec
// running while an interactive `codex` TUI is active) cannot share state
// through ~/.codex. This is hardening, not a bug fix — the symptom
// ("scout launch leaks release-tooling content into an unrelated
// transcript") was not reproducible against codex-cli 0.145.0 in
// concurrent-exec probes, but the asymmetry between this adapter
// (no CODEX_HOME) and _launch_claude_lane (per-lane CLAUDE_CONFIG_DIR)
// was the latent risk. Caller-supplied in.Env still wins: we set the
// env line BEFORE in.Env so a caller override is honored.
func (a *CodexAdapter) buildCmd(ctx context.Context, in RunInput) *exec.Cmd {
	return a.buildCmdWithHome(ctx, in, "")
}

func (a *CodexAdapter) buildCmdWithHome(ctx context.Context, in RunInput, preparedHome string) *exec.Cmd {
	// Timeout is owned by runWithSubprocess; see OSEntrypointsAdapter.buildCmd.
	prompt := in.Prompt
	if prompt == "" && len(in.Args) > 0 {
		prompt = in.Args[0]
	}
	args := []string{"exec"}
	if in.PermissionMode == "plan" || preparedHome != "" {
		args = []string{"-a", "never", "-s", "read-only", "exec"}
	}
	var model, effort string
	if filepath.Base(strings.TrimSpace(in.Command)) == "codex" {
		model, effort, _ = codexNativeSelection(in) // Run validated before minting.
	}
	if model != "" || effort != "" {
		prefix := append([]string(nil), args[:len(args)-1]...)
		if model != "" {
			prefix = append(prefix, "-m", model)
		}
		if effort != "" {
			prefix = append(prefix, "-c", "model_reasoning_effort=\""+effort+"\"")
		}
		args = append(prefix, "exec")
	}
	if preparedHome != "" {
		args = append(args, "--ignore-user-config", "--ignore-rules", "-c", "model_provider=\"openai\"", "--json")
	}
	if prompt != "" {
		args = append(args, prompt)
	}
	command := strings.TrimSpace(in.Command)
	if command == "" {
		command = a.Name()
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = in.Workspace
	codexHome := preparedHome
	var err error
	if codexHome == "" {
		codexHome, err = os.MkdirTemp("", "codex-")
	}
	if err != nil {
		// Fall back to a path we are confident is writable; the OS temp
		// rotation will eventually reclaim it. Worst case: isolation is
		// weaker but the run still completes.
		codexHome = filepath.Join(os.TempDir(), "codex-fallback-"+in.WorkID)
		_ = os.MkdirAll(codexHome, 0o700)
	}
	if preparedHome != "" {
		cmd.Env = codexSubscriptionChildEnv(in.Env, codexHome)
	} else {
		cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
		cmd.Env = append(cmd.Env, in.Env...)
	}
	return cmd
}
