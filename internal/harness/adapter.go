// Package harness: the Adapter interface is the executor contract for a
// real harness. Today the package exposes only a registry of capabilities
// (Harness, Capabilities, Registry); nothing drives a side effect. S1+S2
// shipped the RunAttempt spine (internal/runattempt) and panel.Apply is
// the only caller that mints one. Slice C introduces the Adapter
// interface so future harness-specific executors (Claude, Codex,
// OpenCode, the OS-harness-entrypoints shell) can drive the same spine.
//
// The Adapter contract:
//   - Mint a RunAttempt BEFORE any side effect
//   - Populate the S2 fields (workspace / command / transcript / heartbeats
//     / diff / test / proof_refs) AS DATA EMERGES -- not all-at-once
//   - Complete the RunAttempt (succeeded|failed) on terminal status
//   - Return the terminal RunAttempt plus a sentinel error ONLY for
//     infrastructure failure (couldn't mint, couldn't spawn, etc.);
//     subprocess exit codes are NOT errors -- they map to Status
//
// Compound is the orchestrator; the Adapter is the driver. The S1+S2
// spine guarantees that whatever the adapter does is auditable through a
// single ratt_* id. The adapter is responsible for populating the S2
// fields with the harness-specific evidence.
package harness

import (
	"context"
	"io"
	"time"

	"compound/internal/runattempt"
)

// Adapter is the executor contract. Concrete adapters live in
// internal/harness (os_entrypoints_adapter.go today; future claude / codex /
// opencode adapters in their own files). The Registry today maps
// harness name -> Capabilities; the future slice keys adapters to
// registry rows via Name().
type Adapter interface {
	// Name returns the adapter id. Matches Harness.Name in the registry and
	// the AdapterID in OSEntrypointsFile. Pinned by
	// TestOSEntrypointsAdapterName.
	Name() string
	// Run drives one execution end-to-end. MUST mint a RunAttempt before
	// any side effect. MUST terminalize the RunAttempt on every return
	// path (succeeded|failed). The returned RunOutput.RunAttempt is the
	// terminal artifact; out.Err is non-nil only for infrastructure
	// failure (could not mint, could not spawn, could not write transcript).
	Run(ctx context.Context, in RunInput) (RunOutput, error)
}

// PreflightAdapter resolves every prerequisite that can fail before governed
// work enters running. Tuple-backed adapters use it for authority lookup,
// executable discovery, permission validation, and capacity admission. The
// returned RunInput is the exact resolved input the caller may execute.
type PreflightAdapter interface {
	Preflight(ctx context.Context, in RunInput) (RunInput, error)
}

// GovernedPermissionAdapter marks adapters whose PermissionMode is a
// mutation/read-only execution boundary governed by the work rail. Wrappers
// can preserve this capability by embedding or forwarding the marker; callers
// must not rely on one concrete adapter type.
type GovernedPermissionAdapter interface {
	Adapter
	GovernedPermissionPosture()
	// RevalidateGovernedRun reloads every adapter-owned launch authority at
	// the final process-start boundary. It must be read-only: callers hold the
	// autonomy-admission lock while invoking it.
	RevalidateGovernedRun(context.Context, RunInput) (RunInput, error)
}

// RunInput is the per-Run caller contract. All fields are required unless
// marked optional. A future slice (harness-adapter-feature) adds
// PermissionDecision and CapabilityRef once the policy seam is in place.
type RunInput struct {
	// WorkID is the work envelope this run belongs to. The adapter does
	// not validate the id exists -- the caller is responsible for
	// admitting the work first. The adapter records the id on the
	// minted RunAttempt for proof-chain linkage.
	WorkID string
	// Harness is the registry name of the harness driving this run.
	// Today only "os-harness-entrypoints" has a real adapter. Pinned
	// onto the RunAttempt.Harness field.
	Harness string
	// Workspace is the filesystem root the subprocess runs inside. Pinned
	// onto RunAttempt.Workspace.
	Workspace string
	// Command is the program path. Pinned onto RunAttempt.Command.
	Command string
	// Prompt is the harness-specific prompt text. Optional: today only the
	// Claude and Codex adapters read it (it goes into argv as the prompt
	// argument). OSEntrypointsAdapter ignores it. Callers that already have
	// Args can use Args[0] instead; Prompt is a convenience field.
	Prompt string
	// PermissionMode selects the harness's permission posture for adapters
	// that have one (today: Claude). Recognized values for Claude: "" (safe
	// "plan" direct-adapter default -- read-only; the only mode that does not
	// hang a non-interactive `claude --print` run), "default", "plan",
	// "acceptEdits", and "bypass" (--dangerously-skip-permissions). Governed
	// work execution resolves this field from envelope/policy/readiness before
	// the adapter sees it; manual harness runs retain the safe plan default.
	// The chosen mode is reflected in the subprocess argv.
	// OSEntrypointsAdapter and CodexAdapter ignore it.
	PermissionMode string
	// ExecutionPosture is the resolved governed permission evidence for this
	// launch. The shared runner persists it on the RunAttempt before command
	// construction or subprocess start. Nil for harnesses without a permission
	// posture.
	ExecutionPosture *runattempt.ExecutionPosture
	// ExecutionSelectionSnapshot is the per-run selection snapshot (what
	// this launch was asked to run and what the adapter could apply). The
	// shared runner persists it on the RunAttempt after Mint and before
	// subprocess start, so the historical record is honest: the snapshot
	// reflects what was asked before the subprocess began. Nil for calls
	// that don't go through the wire (direct harness runs, observe-run,
	// etc.); the wire ALWAYS sets this field for governed launches.
	//
	// work_r7a_execution_selection_20260911.
	ExecutionSelectionSnapshot *runattempt.ExecutionSelectionSnapshot
	// StartGuard owns the final governed launch boundary. The shared runner
	// passes it the one-shot cmd.Start operation after command construction and
	// pipe setup; the guard must call that operation exactly once only when its
	// live authority revalidation succeeds. Nil starts normally.
	StartGuard func(context.Context, func() error) error
	// StartRevalidate runs inside CommitLaunch after its locks and immediately before cmd.Start.
	// It must be read-only and must not reacquire the outer admission lock.
	StartRevalidate func() error
	// Args is the argv (after the program path). May be empty.
	Args []string
	// Env is the additional env vars to set on the subprocess. Optional.
	Env []string
	// Stdin is the optional stdin. Optional.
	Stdin io.Reader
	// Timeout caps the subprocess lifetime. 0 = no cap. Optional.
	Timeout time.Duration
	// Now is the wall-clock anchor for the RunAttempt's CreatedAt and the
	// first heartbeat. The adapter does not call time.Now() itself; the
	// caller supplies Now for testability. Required.
	Now time.Time
	// ActionIntentID optionally binds the RunAttempt to the authorizing
	// ActionIntent (closes the attempt->intent->policy-decision audit
	// chain). Optional today; future slice requires it.
	ActionIntentID string

	// PredecessorID is immutable retry provenance, persisted before command
	// construction/start. It never grants execution authority.
	PredecessorID string
	// LaneID is the resolved actor / adapter lane that dispatched this run
	// (e.g. the --by actor on `work advance --execute`). Pinned onto
	// RunAttempt.LaneID via SetAttribution, pre-terminalization, inside
	// runWithSubprocess. Empty = unattributed run: an empty LaneID stamps
	// NOTHING (the runner skips SetAttribution entirely, so the JSON omits
	// lane_id via omitempty). Model and provider are not stampable from
	// RunInput today -- no adapter config exposes them -- so they are
	// passed empty and remain out of scope.
	LaneID string

	// LaunchProfileID is the requested owner profile, not execution authority.
	// Consuming adapters freeze its resolved identity during preflight.
	LaunchProfileID string
	// RequestedModelID and RequestedProviderRoute are resolved from Compound's
	// model-router LaneTuple before a tuple-backed adapter mints its RunAttempt.
	// They describe dispatch intent, not observed execution identity; observed
	// model/provider fields remain empty until route evidence attests them.
	RequestedModelID       string
	RequestedProviderRoute string
	// ExpectedModelRouterRevision binds a selected tuple's executable aliases
	// to the same model-router authority revision cited by the route plan.
	ExpectedModelRouterRevision string
	// ProofRefs are dispatch-time authority references the shared runner must
	// bind to the RunAttempt before subprocess start. They describe why this
	// executor tuple was selected; they are not proof that the model actually
	// consumed the run.
	ProofRefs []string
	// preflighted is an adapter-owned handoff marker. Governed callers cannot
	// forge it outside this package; it prevents Run from repeating an
	// evidence-writing capacity check after the caller has moved work to
	// running. GovernedPermissionAdapter.RevalidateGovernedRun performs the
	// read-only launch-boundary check instead.
	preflighted bool
	// nativeSessionID is the adapter-allocated native session identity for
	// THIS launch. It is adapter-owned: the adapter allocates a fresh value
	// per Run and pins it on the launch argv. A caller can neither supply a
	// session id nor reuse one from an earlier launch, so two concurrent
	// launches never share a native session and a successor never inherits
	// its predecessor's. Empty means the launch pins no native session.
	nativeSessionID string
	// ompNativeVerifier is an adapter-owned resolved proof handoff. External
	// callers cannot populate it; the verifier revalidates it again at Run.
	ompNativeVerifier *ompNativeVerifierLaunch
	// ompPromptOnly is an adapter-owned immutable launch handoff.
	ompPromptOnly *ompPromptOnlyLaunch
	// opencodeLaunch is the adapter-owned immutable prompt-only handoff.
	opencodeLaunch *openCodeLaunch
}

// RunOutput is the per-Run return contract. Always non-nil RunAttempt;
// Err is non-nil only on infrastructure failure.
type RunOutput struct {
	// RunAttempt is the terminalized RunAttempt. Re-Get from store if the
	// caller needs the most-recent state (heartbeats land after this is
	// returned; for long runs, see RunAttempt.LastHeartbeatAt).
	RunAttempt runattempt.RunAttempt
	// TranscriptPath is the path to the captured transcript under
	// <home>/run_attempts/<id>.transcript. Set on every successful
	// subprocess exit AND on every timeout -- partial capture is the
	// audit trail even when the process did not finish.
	TranscriptPath string
	// ExitCode is the subprocess exit code. 0 for success. May be -1
	// when the process was killed by a signal (timeout).
	ExitCode int
	// TimedOut is true when the subprocess exceeded RunInput.Timeout and
	// was killed. The RunAttempt is then Status=Failed and ExitCode=-1.
	TimedOut bool
	// Duration is the wall time the subprocess ran for.
	Duration time.Duration
}
