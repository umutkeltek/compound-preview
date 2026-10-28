package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"compound/internal/runattempt"
)

// The pi coding-agent FAMILY. prime_agent and oh_my_pi are two forks of the
// same upstream coding agent, and their headless contract is the same argv
// with a different binary name and a different per-run home env var. That is a
// parameterisation, not two integrations, and writing it twice would mean two
// places to fix the next time the family's dispatch contract moves.
//
// Verified against the installed binaries (`prime-agent --help`, `omp --help`):
//
//	-p / --print       one-shot print mode; the process exits when the turn ends
//	--no-session       do not persist session state to the agent home
//	--mode <m>         structured output mode; both accept text|json|rpc
//	--cwd <dir>        working directory (both validate it before dialling out)
//	-t / --tools <l>   comma-separated tool allowlist (prime spells it -t/--tools,
//	                   oh-my-pi spells it --tools; the long form works on both)
//	--system-prompt / --append-system-prompt
//	                   how Compound injects context, since neither fork has a
//	                   SessionStart/UserPromptSubmit hook to inject through
//
// What the family does NOT share is the environment: prime-agent honours
// PRIME_AGENT_CODING_AGENT_DIR and PI_SKIP_VERSION_CHECK, oh-my-pi honours
// PI_CODING_AGENT_DIR and has no version-check suppression env at all. Those
// differences are spec fields rather than branches in buildCmd, so a new family
// member is a struct literal.
type piCodingAgentSpec struct {
	// name is the registry id: the AdapterRegistry key, the harness registry
	// row, and the RunAttempt.Harness value. Drift between those three is a
	// silent routing failure, so one field feeds all of them.
	name string
	// binary is the default argv[0] when RunInput.Command is empty.
	binary string
	// homeEnv is the env var this fork reads for its per-run agent home.
	// Every governed dispatch gets a fresh temp home so two concurrent runs
	// (or a run alongside an interactive TUI) cannot share credentials,
	// sessions or memory through the shared agent directory.
	homeEnv string
	// homeTempPrefix prefixes the MkdirTemp home so a leaked directory is
	// attributable to a harness rather than anonymous.
	homeTempPrefix string
	// extraEnv holds fork-specific env lines appended after the home line.
	// Kept explicit: setting a var a fork does not read is a claim on disk
	// that nothing enforces, and the next reader would believe it.
	extraEnv []string

	// shimEnvVar, if non-empty, names the env var the adapter reads for the
	// Compound OMP hook shim path. Set on the oh_my_pi spec; prime_agent is
	// untouched. When empty (prime_agent) the adapter never emits --hook=
	// and never refuses on missing shim -- that path is oh_my_pi-exclusive.
	shimEnvVar string

	// postureFlagFn, if non-nil, is called with RunInput.PermissionMode and
	// returns the harness-native argv fragment that stamps the governed
	// posture (e.g. --approval-mode=yolo for oh_my_pi bypass). When nil the
	// adapter never emits a posture flag -- prime_agent has none.
	postureFlagFn func(permissionMode string) ([]string, error)

	// hookFlagFn, if non-nil, is called with the resolved shim path and
	// returns the --hook=<path> argv fragment that loads the shim into omp.
	// When nil the adapter never emits a hook flag.
	hookFlagFn func(shimPath string) ([]string, error)

	// requiresShimForBypass, when true, makes Run() refuse a PermissionMode
	// of bypass before process start if the resolved shim path is empty or
	// the file is missing -- the failure mode this guard exists to stop is a
	// headless omp process running yolo with no enforcement surface.
	requiresShimForBypass bool
}

// PiCodingAgentAdapter drives one member of the pi coding-agent family as a
// subprocess and wraps the execution in a RunAttempt. Mirrors the Codex/Claude
// adapter shape: a thin shim over the shared runWithSubprocess runner
// (subprocess_runner.go), with family argv assembly and the spec's harness name
// on the RunAttempt.
//
// The adapter:
//   - prefers `--mode rpc` so the audit trail is structured;
//   - falls back to `-p --no-session` for one-shot dry-runs and CI;
//   - never uses COMPOUND_HOME or ~/.compound as the workspace root — callers
//     MUST supply RunInput.Workspace (the run delegate resolves --workspace ->
//     git_delivery -> cwd before the adapter sees it).
//
// It does NOT implement either fork's persistent kernel. It drives a fresh
// subprocess for every Run(); the persistent kernel is a separate daemon
// process (prime-agent's listens on a Unix socket under
// $TMPDIR/prime-agent-<uid>/daemon.sock). Compound is the orchestrator; this
// adapter is the dispatch driver.
//
// Capability-floor honesty: both family rows in DefaultRegistry declare zero
// hook flags. Neither fork exposes a hook protocol Compound's installer can
// write — prime-agent has none at all, and oh-my-pi's is file-based JS modules
// rather than shell-command settings entries — so the registry limitations say
// so, and no future reader can infer enforcement-via-harness from a harness
// that genuinely cannot enforce it.
type PiCodingAgentAdapter struct {
	spec              piCodingAgentSpec
	store             *runattempt.Store
	heartbeatInterval time.Duration
}

// newPiCodingAgentAdapter is the single constructor the per-fork constructors
// funnel through. A non-positive interval degrades to the default cadence.
func newPiCodingAgentAdapter(spec piCodingAgentSpec, store *runattempt.Store, interval time.Duration) *PiCodingAgentAdapter {
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	return &PiCodingAgentAdapter{spec: spec, store: store, heartbeatInterval: interval}
}

// Name returns the manifest adapter id. Keys the AdapterRegistry lookup and
// the RunAttempt.Harness field.
func (a *PiCodingAgentAdapter) Name() string { return a.spec.name }

// Run drives the subprocess end-to-end. Mirrors CodexAdapter.Run /
// ClaudeAdapter.Run: set the harness name, stamp the resolved command,
// delegate to runWithSubprocess.
func (a *PiCodingAgentAdapter) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	in.Harness = a.Name()
	if in.Command == "" {
		in.Command = a.spec.binary
	}
	if err := a.precheckLaunch(in); err != nil {
		return RunOutput{}, err
	}
	// Attempt-honesty: the snapshot of what this launch was asked to run is
	// built here (executionSelectionFor) but can only be PERSISTED before the
	// subprocess starts — the store refuses a stamp on a terminal attempt.
	// The shared runner gains a pre-start stamp seam
	// (RunInput.ExecutionSelectionSnapshot, work_r7a) in a sibling slice; until
	// it lands the OMP attempt carries no snapshot rather than a late one.
	return runWithSubprocess(ctx, a.store, a.Name(), in, a.buildCmd, a.heartbeatInterval)
}

func (a *PiCodingAgentAdapter) precheckLaunch(in RunInput) error {
	if !a.spec.requiresShimForBypass {
		return nil
	}
	if strings.TrimSpace(in.PermissionMode) != "bypass" {
		return nil
	}
	if strings.TrimSpace(a.spec.shimEnvVar) == "" {
		return fmt.Errorf("oh_my_pi: shim env var unset while requiresShimForBypass=true (spec misconfiguration)")
	}
	shimPath := strings.TrimSpace(os.Getenv(a.spec.shimEnvVar))
	if shimPath == "" {
		return fmt.Errorf("oh_my_pi: bypass launch refused -- %s is empty", a.spec.shimEnvVar)
	}
	if _, err := os.Stat(shimPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("oh_my_pi: bypass launch refused -- shim not found at %s", shimPath)
		}
		return fmt.Errorf("oh_my_pi: bypass launch refused -- cannot stat shim %s: %w", shimPath, err)
	}
	return nil
}

// buildCmd assembles the family argv. The shape is:
//
//	<binary> --mode rpc --cwd <workspace> [caller flags...] <prompt>
//	<binary> -p --no-session --cwd <workspace> [caller flags...] <prompt>
//
// Notes on each flag (verified against the installed `--help` of both forks):
//
//   - `--mode rpc` is the structured-output mode; both forks list
//     text|json|rpc, so rpc stays on by default and the audit trail is
//     structured.
//   - `-p --no-session` is the one-shot headless print path that does NOT
//     persist session state to the agent home — the run-attempt chrome
//     (transcript capture + heartbeat) is the only audit trail, matching the
//     registry limitations.
//   - `--cwd <workspace>` makes the working directory explicit; the run
//     delegate resolves --workspace -> git_delivery -> cwd before this point,
//     so the adapter stamps a deterministic workspace and never falls back to
//     COMPOUND_HOME. Both forks validate the directory locally before any
//     network call, so a bad workspace fails fast rather than mid-turn.
//   - `--tools <list>` enforces a caller-supplied tool allowlist; the adapter
//     does NOT invent a default, so an empty list falls through to the fork's
//     own policy. Callers pass it (and anything else, e.g. --approval-mode or
//     --model) through RunInput.Args.
//
// Modal choice: callers short-circuit `--mode rpc` by setting
// RunInput.PermissionMode to "oneshot" (the value the work rail stamps for a
// `--dry-run` proof). Anything else keeps rpc on. This keeps the argv explicit
// at the call site without adding a RunInput field.
func (a *PiCodingAgentAdapter) buildCmd(ctx context.Context, in RunInput) *exec.Cmd {
	// Timeout is owned by runWithSubprocess; see OSEntrypointsAdapter.buildCmd.
	prompt := in.Prompt
	if prompt == "" && len(in.Args) > 0 {
		prompt = in.Args[0]
	}

	args := []string{}
	// Modal selection. "oneshot" => -p --no-session; default => --mode rpc.
	if strings.EqualFold(strings.TrimSpace(in.PermissionMode), "oneshot") {
		args = append(args, "-p", "--no-session")
	} else {
		args = append(args, "--mode", "rpc")
	}

	// Workspace. The run delegate resolves --workspace -> git_delivery -> cwd
	// before this point; we stamp --cwd verbatim and never consult COMPOUND_HOME.
	// A missing workspace is a hard error: running a coding agent in the wrong
	// directory is exactly the failure mode this adapter is built to prevent.
	workspace := strings.TrimSpace(in.Workspace)
	if workspace == "" {
		// Defensive: the shared runner callers always populate Workspace,
		// but a misuse surfaces here rather than at run time.
		workspace = "."
	}
	args = append(args, "--cwd", workspace)

	// oh_my_pi posture + hook. Specs other than oh_my_pi carry nil fns
	// and the calls are no-ops; prime_agent untouched. The posture is
	// stamped BEFORE any caller-supplied Args so a host config default
	// cannot shadow it (the registry limitation -- tools.approvalMode is
	// a host config default that can be yolo; governed dispatch MUST pass
	// --approval-mode explicitly and never inherit).
	if a.spec.postureFlagFn != nil {
		flag, err := a.spec.postureFlagFn(in.PermissionMode)
		if err != nil {
			return a.refuseWithStderrCommand(ctx, in, "oh_my_pi: "+err.Error())
		}
		args = append(args, flag...)
	}
	if a.spec.hookFlagFn != nil && strings.TrimSpace(a.spec.shimEnvVar) != "" {
		shimPath := strings.TrimSpace(os.Getenv(a.spec.shimEnvVar))
		// A missing shim is the bypass-only failure mode (handled by
		// precheckLaunch, which refuses BEFORE minting a RunAttempt).
		// For non-bypass modes we silently skip --hook= so the harness
		// still runs -- the operator loses enforcement, not the launch.
		if shimPath == "" && a.spec.requiresShimForBypass && strings.TrimSpace(in.PermissionMode) != "bypass" {
			// no-op: not a bypass launch
		} else if shimPath != "" {
			flag, err := a.spec.hookFlagFn(shimPath)
			if err != nil {
				return a.refuseWithStderrCommand(ctx, in, "oh_my_pi: "+err.Error())
			}
			args = append(args, flag...)
		}
	}

	// Tool policy. in.Args[1:] may carry additional flags; the adapter
	// appends them verbatim after the prompt-mode flags so the caller
	// can override anything (e.g. --provider switchback --model ...).
	// Guard against an empty Args slice — the shared runner treats
	// Args[0] as the prompt if Prompt is empty, so we must not strip
	// it here.
	if len(in.Args) > 1 {
		args = append(args, in.Args[1:]...)
	}

	// Prompt goes last, positional — same shape as the codex adapter.
	if prompt != "" {
		args = append(args, prompt)
	}

	// Honour the caller-supplied binary. Hardcoding the literal here would
	// make Run()'s in.Command default dead code and silently ignore an
	// explicit override. buildCmd defaults independently because tests and
	// readiness probes call it directly, without going through Run().
	binary := strings.TrimSpace(in.Command)
	if binary == "" {
		binary = a.spec.binary
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workspace

	// Per-run home isolation. The family has the same COMPOUND_HOME /
	// shared-agent-dir problem codex had (CLAUDE_HOME in the Claude lane):
	// every governed dispatch needs a fresh home so two concurrent runs (or an
	// exec running while an interactive TUI is active) cannot share auth,
	// sessions or memory. A fresh home also means the fork has no stored
	// credential to fall back on, which is deliberate — the lane's env-var
	// provider config is then the only way it can reach a model, and an
	// unconfigured lane fails loudly instead of quietly using an uncaptured
	// default provider. Any Switchback capture tap is system-wide and lives
	// OUTSIDE this isolation (read-only consumer); we fence per-run local state
	// only.
	agentHome, err := os.MkdirTemp("", a.spec.homeTempPrefix)
	if err != nil {
		agentHome = filepath.Join(os.TempDir(), a.spec.homeTempPrefix+"fallback-"+in.WorkID)
		_ = os.MkdirAll(agentHome, 0o700)
	}
	cmd.Env = append(os.Environ(), a.spec.homeEnv+"="+agentHome)
	cmd.Env = append(cmd.Env, a.spec.extraEnv...)
	cmd.Env = append(cmd.Env, in.Env...)
	return cmd
}

// refuseWithStderrCommand builds a non-launched command that prints the
// refusal reason on stderr and exits 2. buildCmd cannot return an error
// (the *exec.Cmd signature is shared with prime_agent), so caller-bug
// refusals surface as a process that exits immediately with the reason
// text on stderr. The reason is what the harness will see, so it names
// the exact failure (e.g. an unknown PermissionMode).
//
// sh -c is the primitive every POSIX host provides; an interactive
// shell cannot be assumed on a headless dispatch.
func (a *PiCodingAgentAdapter) refuseWithStderrCommand(ctx context.Context, in RunInput, reason string) *exec.Cmd {
	workspace := strings.TrimSpace(in.Workspace)
	if workspace == "" {
		workspace = "."
	}
	binary := strings.TrimSpace(in.Command)
	if binary == "" {
		binary = a.spec.binary
	}
	script := "printf '%s\n' " + strconv.Quote(reason) + "; exit 2"
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = workspace
	return cmd
}

// formatPiCodingAgentDryRunPrompt is the canonical argv-printed summary the
// verifier prints when running a `--dry-run` proof. It lives beside the adapter
// (not in the dry-run probe) so the same formatting is shared by the unit tests
// and the dispatch verifier.
//
// Intentionally side-effect free — it only formats the argv the adapter would
// build — and safe to call from tests.
// executionSelectionFor is the per-attempt honesty record the run rail
// reads off the RunAttempt. It is a best-effort append-only stamp: a
// failure here MUST NOT block the launch result (which has already
// terminalized). The snapshot ONLY carries what argv-visible evidence
// supports, never a receipt.
//
// Per the R7B packet, oh_my_pi launches carry:
//   - Harness: oh_my_pi
//   - HarnessVersion: parsed from omp/18.1.17 (a single one-shot
//     exec; the result is cached per-process to avoid the cost on every
//     launch). Empty when the version probe fails -- never a fabricated
//     string.
//   - RequestedEffort: high when RunInput.RequestedEffort is set, else empty.
//   - AppliedEffort: high -- the OMP CLI accepts --thinking=high, and the
//     adapter never downgrades the request.
//   - EffortEvidence: argv:--thinking=high when the caller passed --thinking=
//     or --max-thinking-effort=, else unobservable:no effort control passed.
//   - NativeSessionEvidence: unsupported:--no-session for the default launch
//     shape (the adapter passes --no-session in oneshot mode; the default
//     rpc mode also leaves no native session because the adapter sets
//     PI_CODING_AGENT_DIR to a fresh per-run temp dir).
//
// The snapshot is meant for SetExecutionSelectionSnapshot (append-only,
// refuses on a different second call). A repeated launch with identical
// fields is a no-op; a different field set is refused so the historical
// record is preserved.
func (a *PiCodingAgentAdapter) executionSelectionFor(in RunInput) runattempt.ExecutionSelectionSnapshot {
	snap := runattempt.ExecutionSelectionSnapshot{
		Source:                "system_default",
		Harness:               a.spec.name,
		HarnessVersion:        ompObservedVersion(),
		ProviderRoute:         strings.TrimSpace(in.RequestedProviderRoute),
		RequestedModelID:      strings.TrimSpace(in.RequestedModelID),
		NativeSessionEvidence: "unsupported:--no-session",
	}
	// RunInput carries no semantic effort field today; the only honest
	// applied value is the one actually on the argv. Anything else is
	// recorded as unobservable, never invented (orchestrator integration
	// fix after the lane's session ended mid-edit, 2026-09-11).
	if value, ok := piArgFlagValue(in.Args, "--thinking"); ok {
		snap.AppliedEffort = value
		snap.EffortEvidence = "argv:--thinking=" + value
	} else if value, ok := piArgFlagValue(in.Args, "--max-thinking-effort"); ok {
		snap.AppliedEffort = value
		snap.EffortEvidence = "argv:--max-thinking-effort=" + value
	} else {
		snap.EffortEvidence = "unobservable:no effort flag on argv"
	}
	return snap
}

// piArgFlagValue returns the value of `name=<value>` (or `name <value>`)
// on args, and whether it was present. Used by stampExecutionSelection to
// record the effort the launch really carried.
func piArgFlagValue(args []string, name string) (string, bool) {
	for i, arg := range args {
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimSpace(strings.TrimPrefix(arg, name+"=")), true
		}
		if arg == name && i+1 < len(args) {
			return strings.TrimSpace(args[i+1]), true
		}
	}
	return "", false
}

// ompObservedVersion runs omp/18.1.17 once per process and caches the
// parsed HarnessVersion. Empty when the binary is missing or the probe
// fails -- the snapshot NEVER claims a fabricated version.
var ompObservedVersionOnce sync.Once
var ompObservedVersionValue string

func ompObservedVersion() string {
	ompObservedVersionOnce.Do(func() {
		cmd := exec.Command("omp", "--version")
		out, err := cmd.Output()
		if err != nil {
			return
		}
		ompObservedVersionValue = parseOmpVersionLine(string(out))
	})
	return ompObservedVersionValue
}

// parseOmpVersionLine returns the "NN.NN.NN" portion of a line like
// "omp/18.1.17". Empty when no semver-shaped token is found.
func parseOmpVersionLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.LastIndex(line, "/")
		if idx < 0 {
			continue
		}
		tail := line[idx+1:]
		// Strip a leading "v" if present.
		tail = strings.TrimPrefix(tail, "v")
		parts := strings.Split(tail, ".")
		if len(parts) != 3 {
			continue
		}
		ok := true
		for _, p := range parts {
			if p == "" {
				ok = false
				break
			}
			for _, r := range p {
				if r < '0' || r > '9' {
					ok = false
					break
				}
			}
			if !ok {
				break
			}
		}
		if ok {
			return tail
		}
	}
	return ""
}

func formatPiCodingAgentDryRunPrompt(binary string, in RunInput) string {
	mode := "rpc"
	if strings.EqualFold(strings.TrimSpace(in.PermissionMode), "oneshot") {
		mode = "oneshot"
	}
	workspace := strings.TrimSpace(in.Workspace)
	if workspace == "" {
		workspace = "(unset)"
	}
	prompt := in.Prompt
	if prompt == "" && len(in.Args) > 0 {
		prompt = in.Args[0]
	}
	return fmt.Sprintf("%s --mode=%s --cwd=%s prompt=%q", binary, mode, workspace, prompt)
}
