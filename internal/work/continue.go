package work

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const WorkContinueSchemaVersion = "compound.work_continue.v0.1"
const WorkContinueRunSchemaVersion = "compound.work_continue_run.v0.1"

const defaultWorkContinueMaxSteps = 10

// executeUnattendedCapability is the capability the execute stage advertises
// to the policy evaluator and the route planner. Standing grants, lane
// routing, and capacity gating all key off this string; renaming it is a
// breaking change to the unattended envelope.
const executeUnattendedCapability = "work.continue.unattended"

// executeUnattendedGrantID is the operator-signed grant the seam requires.
// It is intentionally versioned in the suffix so a future policy revision
// does not silently inherit an old envelope — operators explicitly import a
// fresh v1 grant file.
const executeUnattendedGrantID = "grant_unattended_work_continue_v1"

// Capacity freshness + ceiling knobs the seam reads. Defaults match the
// packet (10-minute freshness, 90% ceiling, strict less-than). Tests override
// these so they don't need a real capacity fixture; production wiring picks
// them up at package init.
var (
	executeStageCapacityFreshnessWindow = 10 * time.Minute
	executeStageCapacityCeilingPercent  = 90.0
)

// executeRoutePlanFn resolves an eligible lane for the given capability from
// a home's route plan. Production default reads routeevidence; tests override
// it to return a stub without importing the sibling package.
type executeRoutePlanFn func(homePath, capability string) (lane string, ok bool)

// ExecutionRouteCandidate is one owner-authorized executable tuple. Profile
// identity and harness are deliberately separate: multiple profiles may use
// one adapter, but they are not interchangeable launch choices.
type ExecutionRouteCandidate struct {
	LaunchProfileID   string
	Harness           string
	RequestedModelID  string
	ProviderRoute     string
	SourceRevision    string
	CapacityLane      string
	SelectionRevision int
}

// executeRouteCandidatesFn returns owner-ordered tuples for capacity admission.
type executeRouteCandidatesFn func(homePath, capability string) []ExecutionRouteCandidate

// executeCapacityFn reports the latest capacity snapshot for `lane` as
// (usedPercent, fresh, err). `fresh=false` means the snapshot is older than
// the freshness window — the seam must refuse on that signal regardless of
// the usedPercent value.
type executeCapacityFn func(homePath, lane string, now time.Time) (usedPercent float64, fresh bool, err error)

// executePolicyEvalFn evaluates the work.continue.unattended intent under
// the home's policy config. Production default calls policy.Evaluate;
// tests short-circuit it to simulate hold/deny.
type executePolicyEvalFn func(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim

// executeLaunchFn actually launches the harness against the ready envelope.
// Production default is wired in execute_stage.go to the actuator's
// ExecuteGovernedHarnessAdvance seam; tests stub it to control success and
// failure paths. The returned RunAttemptID lands on ContinueResult.RunAttemptIDs.
//
// The actor parameter (DEFECT 2 fix, AUT-P0-002 2026-09-04) is the claimant
// and authority holder for this launch. The wire MUST finalize under the
// actor — leases compare exact strings, and the heartbeat's --actor
// (e.g. "claude-minimax" with a hyphen) is distinct from the canonical
// lane (e.g. "claude_minimax" with an underscore). Lane and harness are
// attribution only; actor is authority.
type executeLaunchFn func(store *Store, workID, lane, harness, prompt, actor string, now time.Time) (string, error)

// Tuple-aware launch/preflight seams preserve the selected route candidate
// while the legacy hooks remain available to existing focused tests/callers.
type executeCandidateLaunchFn func(store *Store, workID string, candidate ExecutionRouteCandidate, prompt, actor string, now time.Time) (string, error)
type executeCandidatePreflightFn func(store *Store, workID string, candidate ExecutionRouteCandidate, now time.Time) error

// executePreflightFn runs BEFORE the seam claims or advances the envelope
// (DEFECT 1 fix, AUT-P0-001 2026-09-04). It resolves every prerequisite
// that can fail before any state mutation: adapter enrollment, workspace
// resolvability, and lane readiness. The hook returns nil on success or
// an error with a recognized skip prefix (lane_unready:, workspace_unresolvable:,
// adapter_missing:) on refusal — the seam treats the refusal like a launch-stub
// refusal but leaves the envelope, its lease, the event log and the RunAttempt
// set byte-identical to before. Production wires executewire.PreflightViaActuator
// via executewire.Install(); tests stub it (or rely on the default no-op) to
// control refusal paths without going through the real adapter / readiness stack.
type executePreflightFn func(store *Store, workID, lane string, now time.Time) error

// Package-level hooks. Tests MUST save and restore via restoreAllExecuteHooks
// (or the test helpers in continue_execute_test.go) to keep state isolated
// between cases. The defaults below are wired to "no-op stub" so production
// callers must explicitly bind the real ones.
var (
	executeStageRoutePlanFn          executeRoutePlanFn       = defaultExecuteStageRoutePlanFn
	executeStageRouteCandidatesFn    executeRouteCandidatesFn = defaultExecuteStageRouteCandidatesFn
	executeStageCapacityFn           executeCapacityFn        = defaultExecuteStageCapacityFn
	executeStagePolicyEvalFn         executePolicyEvalFn      = defaultExecuteStagePolicyEvalFn
	launchStub                       executeLaunchFn          = defaultExecuteStageLaunch
	executeStagePreflightFn          executePreflightFn       = defaultExecuteStagePreflight
	executeStageCandidateLaunchFn    executeCandidateLaunchFn
	executeStageCandidatePreflightFn executeCandidatePreflightFn
)

// Shim types for the policy-eval hook. The real policy package exposes
// envelope.ActionIntent and envelope.PolicyDecision; we keep the seam-level
// types narrow (an opaque struct) so the seam is not coupled to the envelope
// package, which would create an import cycle with internal/policy.

// PolicyEvalFn realises the seam by translating envelope types on the way
// in/out — see execute_stage.go for the production binding.
type policyActionIntentShim = policyActionIntent
type policyConfigShim = policyConfig
type policyDecisionShim = policyDecision

// Exported aliases of the shim types so the composition root (cmd/compound)
// and its tests can construct hook values without importing the unexported
// names. Aliases, not copies: the seam and the wiring share one shape.
type PolicyActionIntentShim = policyActionIntent
type PolicyConfigShim = policyConfig
type PolicyDecisionShim = policyDecision

// SwapExecuteStageRoutePlan installs fn as the seam's route-plan resolver and
// returns a restore function. cmd/compound binds the real resolver at init;
// tests swap in stubs and restore to keep package state isolated.
func SwapExecuteStageRoutePlan(fn executeRoutePlanFn) func() {
	prev, prevCandidate := executeStageRoutePlanFn, executeStageRouteCandidatesFn
	executeStageRoutePlanFn = fn
	executeStageRouteCandidatesFn = defaultExecuteStageRouteCandidatesFn
	return func() { executeStageRoutePlanFn = prev; executeStageRouteCandidatesFn = prevCandidate }
}

// SwapExecuteStageRouteCandidates installs the ordered route source used by
// unattended execution. The legacy single-route hook remains the default
// source so focused callers keep their existing contract.
func SwapExecuteStageRouteCandidates(fn executeRouteCandidatesFn) func() {
	prev := executeStageRouteCandidatesFn
	executeStageRouteCandidatesFn = fn
	return func() { executeStageRouteCandidatesFn = prev }
}

// SwapExecuteStageCapacity installs fn as the seam's capacity reader and
// returns a restore function (see SwapExecuteStageRoutePlan).
func SwapExecuteStageCapacity(fn executeCapacityFn) func() {
	prev := executeStageCapacityFn
	executeStageCapacityFn = fn
	return func() { executeStageCapacityFn = prev }
}

// SwapExecuteStagePolicyEval installs fn as the seam's policy evaluator and
// returns a restore function (see SwapExecuteStageRoutePlan).
func SwapExecuteStagePolicyEval(fn executePolicyEvalFn) func() {
	prev := executeStagePolicyEvalFn
	executeStagePolicyEvalFn = fn
	return func() { executeStagePolicyEvalFn = prev }
}

// SwapExecuteStageLaunch installs fn as the seam's harness launcher and
// returns a restore function (see SwapExecuteStageRoutePlan).
func SwapExecuteStageLaunch(fn executeLaunchFn) func() {
	prev, prevCandidate := launchStub, executeStageCandidateLaunchFn
	launchStub = fn
	executeStageCandidateLaunchFn = nil
	return func() { launchStub = prev; executeStageCandidateLaunchFn = prevCandidate }
}

func SwapExecuteStageCandidateLaunch(fn executeCandidateLaunchFn) func() {
	prev := executeStageCandidateLaunchFn
	executeStageCandidateLaunchFn = fn
	return func() { executeStageCandidateLaunchFn = prev }
}

// SwapExecuteStagePreflight installs fn as the seam's preflight resolver and
// returns a restore function. cmd/compound/executewire binds the real
// preflight (adapter / workspace / readiness) at init; tests stub the hook
// to drive recognized refusal paths without spinning up a real adapter stack.
func SwapExecuteStagePreflight(fn executePreflightFn) func() {
	prev, prevCandidate := executeStagePreflightFn, executeStageCandidatePreflightFn
	executeStagePreflightFn = fn
	executeStageCandidatePreflightFn = nil
	return func() { executeStagePreflightFn = prev; executeStageCandidatePreflightFn = prevCandidate }
}

func SwapExecuteStageCandidatePreflight(fn executeCandidatePreflightFn) func() {
	prev := executeStageCandidatePreflightFn
	executeStageCandidatePreflightFn = fn
	return func() { executeStageCandidatePreflightFn = prev }
}

type policyActionIntent struct {
	ID         string
	Capability string
	WorkID     string
	Risk       string
	Privacy    string
}

type policyConfig struct {
	HasGrant bool
	// Home is the Compound home root the evaluator loads policy config (and
	// imported standing grants) from. The seam always passes the store's home.
	Home string
}

type policyDecision struct {
	ID      string
	Outcome string
}

type ContinueOptions struct {
	// Selected explicitly continues only ExpectWorkID under its current lease.
	// It is not an unattended queue grant. Resume authorizes one new attempt
	// from verifying; without it verification-pending work never relaunches.
	Selected        bool
	Resume          bool
	VerificationRef string
	Actor           string
	RecentLimit     int
	ExpectWorkID    string
	Outcome         string
	MoveType        string
	Summary         string
	// MaxExecuteSteps requests how many execute-stage launches Continue may
	// perform. Zero (the flag omitted) means the default of 1, so an
	// invocation that never mentions the flag can still launch a governed
	// harness — see executeWarning. The request is clamped to
	// executeStageMaxLaunchesPerRun, which is 1, so a value above 1 is
	// accepted but not spendable today. effectiveExecuteBudget is the only
	// number that describes real behaviour; this field is the request.
	MaxExecuteSteps int
	// DryRun previews the whole continuation without lifecycle, lease, event,
	// detached-recovery or execution writes. Launch readiness is checked, but
	// the eventual mutation still requires fresh policy and reservation checks.
	DryRun bool
}

type ContinueResult struct {
	Preview             *ContinuePreview `json:"preview,omitempty"`
	executionChoice     *ExecutionRouteCandidate
	SelectedClose       *DispatchCloseResult      `json:"selected_close,omitempty"`
	SchemaVersion       string                    `json:"schema_version"`
	ID                  string                    `json:"id"`
	GeneratedAt         string                    `json:"generated_at"`
	Status              string                    `json:"status"`
	Actor               string                    `json:"actor"`
	Action              string                    `json:"action"`
	WorkID              string                    `json:"work_id,omitempty"`
	ExpectWorkID        string                    `json:"expect_work_id,omitempty"`
	ResultPath          string                    `json:"result_path,omitempty"`
	Counts              WorkbenchCounts           `json:"counts"`
	PostCounts          *WorkbenchCounts          `json:"post_counts,omitempty"`
	Admission           *AdmitCreatePlanReport    `json:"admission,omitempty"`
	AdvancePlan         *AdvancePlan              `json:"advance_plan,omitempty"`
	DispatchStep        *DispatchStepResult       `json:"dispatch_step,omitempty"`
	ActionIntentID      string                    `json:"action_intent_id,omitempty"`
	PolicyDecisionID    string                    `json:"policy_decision_id,omitempty"`
	ContextPackID       string                    `json:"context_pack_id,omitempty"`
	CapabilitiesOffered []DispatchCapabilityOffer `json:"capabilities_offered,omitempty"`
	WorkerPromptMD      string                    `json:"worker_prompt_md,omitempty"`
	NextAction          string                    `json:"next_action,omitempty"`
	CanContinue         bool                      `json:"can_continue"`
	ContinueCommand     []string                  `json:"continue_command,omitempty"`
	WaitReason          string                    `json:"wait_reason,omitempty"`
	BlockedOn           []string                  `json:"blocked_on,omitempty"`
	NoAction            string                    `json:"no_action,omitempty"`
	Warnings            []string                  `json:"warnings,omitempty"`
	Integrity           []string                  `json:"integrity,omitempty"`

	// R3A execute-stage fields. Set ONLY when the new execute seam runs;
	// existing lifecycle branches leave them zero so callers can detect a
	// "did nothing because gates refused" vs. "did nothing because there
	// was no work" with one field.
	//
	// Skipped carries one named refusal reason when Action=="none"; empty
	// on every other code path (including Action=="execute" success).
	// RunAttemptIDs carries the ratt id the launch minted on
	// Action=="execute". Human is the plain-language line a tired operator
	// can read without context.
	Skipped       string   `json:"skipped,omitempty"`
	RunAttemptIDs []string `json:"run_attempt_ids,omitempty"`
	Lane          string   `json:"lane,omitempty"`
	Human         string   `json:"human,omitempty"`

	// Refused (R6E) names every ready envelope the execute stage declined
	// to even try this tick — ineligibility (another executor's work) or a
	// recognized preflight refusal — in pick (workbench) order. Entries are
	// recorded WITHOUT mutating the envelope. Skipped keeps carrying only
	// the HEAD's refusal family so existing readers stay stable; Refused is
	// the complete list the Human line summarizes. Empty when the stage
	// never refused anything (launch, or refusal before candidates exist).
	Refused []ExecuteRefusal `json:"refused,omitempty"`

	// Execute is the structured verdict the execute stage stamps whenever
	// the caller asked for one (ContinueOptions.MaxExecuteSteps > 0).
	// The field is the single source of truth for "what did the execute
	// stage do this tick?" and is set EVEN WHEN the stage falls through
	// to DispatchStep -- deliverable 0 of R3A slice 2, after the
	// orchestrator noticed a tired human cannot tell from
	// `{"status":"waiting","step_count":1}` whether the execute stage ran
	// at all. Either Skipped+Human is set (refused / no-op) or
	// RunAttemptIDs+Status is set (a launch happened); both is a bug.
	Execute *ExecuteVerdict `json:"execute,omitempty"`

	// Detached (R6P) is the sidecar identity of a launch this tick handed to a
	// detached governed door; DetachedReconciled is what became of doors
	// handed off on earlier ticks (see execute_detached.go).
	Detached           *DetachedLaunch     `json:"detached,omitempty"`
	DetachedReconciled []DetachedReconcile `json:"detached_reconciled,omitempty"`
}

// ExecuteVerdict is the structured per-tick record of what the unattended
// execute stage did. It is the answer to the orchestrator's deliverable 0:
// "did the execute stage refuse, succeed, or not run at all this tick?".
// Skipped+Human are set on a refusal or no-op (no grant, no ready envelope,
// lane unready, etc.). RunAttemptIDs+Status are set on a successful launch.
// Lane is set whenever the route gate ran, regardless of outcome. Only one
// of {refusal, launch, would_execute} may be non-empty; the helper
// EnsureConsistent enforces this before the result is written to disk.
type ExecuteVerdict struct {
	// Stage is "execute" so callers reading the JSON can grep for the
	// stage this verdict came from without parsing context. Today the
	// seam only emits one verdict per Continue call, but the field is
	// forward-compatible with a future stage that wraps more than one.
	Stage string `json:"stage"`
	// Skipped is one of the named refusal reasons (no_grant, risk,
	// no_route, capacity_unknown, policy_hold, lane_unready,
	// workspace_unresolvable, adapter_missing, claim_failed, launch_failed,
	// preflight_failed, ineligible) or empty on a launch. "ineligible" is the
	// HEAD's family when the stage skipped it as another executor's work
	// (R6E); the complete per-id refusal list lives on ContinueResult.Refused.
	Skipped string `json:"skipped,omitempty"`
	// Human is the plain-language line. Set on every verdict so a tired
	// operator can read what the stage did without parsing the
	// structured fields.
	Human string `json:"human"`
	// Lane is the route-plan lane the stage picked (today: equal to the
	// harness name). Set whenever the route gate ran; empty on a
	// pre-route refusal (no_grant, no ready envelope).
	Lane string `json:"lane,omitempty"`
	// Status is the post-launch envelope state the seam observed, or the
	// current state if the verdict is a refusal. Always set.
	Status string `json:"status"`
	// RunAttemptIDs carries every ratt id the launch minted.
	RunAttemptIDs []string `json:"run_attempt_ids,omitempty"`
	// Detached (R6P) is set when this tick handed the launch to a detached
	// governed door; Reconciled lists what became of earlier doors.
	Detached   *DetachedLaunch     `json:"detached,omitempty"`
	Reconciled []DetachedReconcile `json:"reconciled,omitempty"`
	// WouldExecute is the dry-run planning line: "<work_id> on <lane>
	// (capacity N%, policy <id>)". Set ONLY on a dry-run pass where the
	// gates all opened but the seam deliberately did not launch.
	WouldExecute string `json:"would_execute,omitempty"`
	// CapacityPercent mirrors the gate-(d) reading the seam observed, so
	// dry-run probes can be cross-checked against the live CodexBar feed.
	CapacityPercent float64 `json:"capacity_percent,omitempty"`
}

type ContinueRunOptions struct {
	ContinueOptions
	MaxSteps int
}

type ContinueRunResult struct {
	SchemaVersion   string           `json:"schema_version"`
	ID              string           `json:"id"`
	GeneratedAt     string           `json:"generated_at"`
	Status          string           `json:"status"`
	Actor           string           `json:"actor"`
	ExpectWorkID    string           `json:"expect_work_id,omitempty"`
	ResultPath      string           `json:"result_path,omitempty"`
	MaxSteps        int              `json:"max_steps"`
	StepCount       int              `json:"step_count"`
	Steps           []ContinueResult `json:"steps"`
	CanContinue     bool             `json:"can_continue"`
	ContinueCommand []string         `json:"continue_command,omitempty"`
	WaitReason      string           `json:"wait_reason,omitempty"`
	BlockedOn       []string         `json:"blocked_on,omitempty"`
	// Execute mirrors the last step that ran the unattended execute stage so a
	// reader of the run JSON (the heartbeat parser, a tired human) sees the
	// stage's verdict at the top level instead of digging through steps[].
	Execute    *ExecuteVerdict `json:"execute,omitempty"`
	NextAction string          `json:"next_action,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
	Integrity  []string        `json:"integrity,omitempty"`
}

// executeStageMaxLaunchesPerRun is the hard ceiling on governed harness
// launches one `work continue` invocation (including an --until-wait run)
// can perform.
//
// It is 1 by construction rather than by policy: executeStage launches at
// most one envelope per Continue call, and Action="execute" is not progress
// for continueStepMadeProgress, so ContinueRun's loop always returns on the
// step that launched. Raising this is an autonomy expansion — strictly more
// unattended harness launches per heartbeat tick — and belongs to the
// operator, not to a refactor. The constant states the ceiling once so the
// operator-facing warning can quote what the system will actually do.
const executeStageMaxLaunchesPerRun = 1

// effectiveExecuteBudget is how many governed harness launches this
// invocation can ACTUALLY perform, as opposed to how many were requested.
// A dry run performs none. An omitted --max-execute-steps means the
// documented default of one, and every request is clamped to the structural
// ceiling above — so the number the operator is shown is the number the
// system can deliver.
func effectiveExecuteBudget(opts ContinueOptions) int {
	if opts.DryRun {
		return 0
	}
	requested := opts.MaxExecuteSteps
	if requested <= 0 {
		requested = 1
	}
	if requested > executeStageMaxLaunchesPerRun {
		return executeStageMaxLaunchesPerRun
	}
	return requested
}

// executeWarning states, truthfully, whether this invocation can launch a
// harness. "does not execute task commands" was unconditionally true before
// the execute stage existed. It is now false on any live call that finds a
// ready envelope under the standing grant — INCLUDING one that omitted
// --max-execute-steps, because omitting the flag still permits the default
// execute step. A tool that tells the operator it does not execute while it
// is minting RunAttempts makes the gate unreadable, so the warning is
// derived from the effective budget rather than frozen as a literal.
func executeWarning(opts ContinueOptions) string {
	if opts.DryRun {
		return "does not execute task commands (dry-run probe: gates are evaluated, nothing is launched)"
	}
	if n := effectiveExecuteBudget(opts); n != 1 {
		return fmt.Sprintf("may launch up to %d governed harness execution steps", n)
	}
	return "may launch 1 governed harness execution step"
}

func (s *Store) Continue(now time.Time, opts ContinueOptions) (ContinueResult, error) {
	if opts.Selected {
		return s.continueSelected(now, opts)
	}
	if opts.Resume || opts.VerificationRef != "" {
		return ContinueResult{}, fmt.Errorf("resume and verification require selected-work Continue")
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	expectWorkID := strings.TrimSpace(opts.ExpectWorkID)
	report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: opts.RecentLimit, Actor: actor})
	if err != nil {
		return ContinueResult{}, err
	}
	result := ContinueResult{
		SchemaVersion: WorkContinueSchemaVersion,
		ID:            idFor("wcon_", actor, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "waiting",
		Actor:         actor,
		Action:        "none",
		ExpectWorkID:  expectWorkID,
		Counts:        report.Counts,
		Warnings: []string{
			"moves safe local lifecycle plumbing only",
			executeWarning(opts),
			"does not observe or synthesize traces",
			"does not promote memory",
		},
		Integrity: append([]string{}, report.Integrity...),
	}
	if opts.DryRun {
		return s.previewContinue(now, opts, report, result)
	}

	// R6P: reconcile earlier detached launches on EVERY unattended tick, not
	// only when a ready envelope is on the bench — a door handed off on tick
	// N has to be reported on tick N+1 even when the bench is empty. The
	// verdict and the Human line carry it (see recordContinue).
	if opts.MaxExecuteSteps > 0 {
		result.DetachedReconciled = s.ReconcileDetachedLaunches(now)
	}

	// R3A execute-stage seam: runs BEFORE the existing claim branch when
	// a ready envelope is on the bench. The seam is grant-gated and
	// fail-closed:
	//
	//   - all gates pass        -> envelope is driven through running/verifying
	//   - any gate refuses      -> refusal is logged, fall through to DispatchStep
	//   - no ready envelope     -> falls through to DispatchStep (existing path)
	//
	// The "any gate refuses -> fall through to DispatchStep" branch is
	// deliberate: unattended execution is opt-in via the grant, and the
	// existing Continue path already handles ready envelopes via
	// DispatchStep when no operator has enabled the unattended envelope OR
	// when the seam has nothing good to do with the envelope (e.g. risk
	// tier2, stale capacity). Heartbeat progress on ready envelopes is
	// preserved; the refusal is observable in the Continue JSON (skipped +
	// human) so the operator can still see why unattended execution
	// declined. Once the grant is installed AND a downstream gate fails,
	// the JSON carries skipped=<reason> while DispatchStep claims the
	// envelope via the existing path.
	//
	// Deliverable 0 (R3A slice 2 orchestrator addendum, 2026-09-04): when
	// the caller asked for the execute stage (`MaxExecuteSteps > 0`), the
	// result MUST carry an `execute` verdict so a tired human can tell
	// "did nothing because no grant" from "did nothing because no work"
	// from `{"status":"waiting","step_count":1}`. The verdict is stamped
	// regardless of whether the seam ran end-to-end (a launch), refused
	// at a gate (skipped), or fell through to DispatchStep because there
	// was no ready envelope to begin with (no_grant or no-op). The
	// heartbeat summary line reads `execute=skipped:<reason>` or
	// `execute=ok ratt=<id>` off the verdict.
	if continueExecuteStageThreshold(report) {
		stage, stageErr := s.executeStage(now, opts, actor)
		if stageErr != nil {
			return result, stageErr
		}
		if stage.Action == "execute" {
			result.Action = "execute"
			result.Status = "verifying"
			if stage.Status != "" {
				result.Status = stage.Status
			}
			result.WorkID = stage.WorkID
			result.RunAttemptIDs = stage.RunAttemptIDs
			result.Skipped = stage.Skipped
			result.Lane = stage.Lane
			result.Human = stage.Human
			result.PolicyDecisionID = stage.PolicyDecisionID
			result.Refused = stage.Refused
			result.Detached = stage.Detached
			result.Execute = buildExecuteVerdict(stage, result.Status)
			return s.recordContinue(now, result)
		}
		if stage.Skipped != "" {
			// Seam refused — stamp Skipped + Human onto the result so
			// the heartbeat log shows why unattended execution declined,
			// and fall through to DispatchStep. The next Continue tick
			// re-evaluates with whatever has changed (fresh capacity
			// row, re-imported grant, lifted policy hold, etc).
			result.Skipped = stage.Skipped
			result.Lane = stage.Lane
			result.Human = stage.Human
			result.PolicyDecisionID = stage.PolicyDecisionID
			result.Refused = stage.Refused
			result.Execute = buildExecuteVerdict(stage, "refused")
			if stage.Skipped == "target_changed" {
				result.Status = "target_changed"
				result.NoAction = stage.NoAction
				return s.recordContinue(now, result)
			}
			// Clear result.Action / result.Status so DispatchStep
			// below owns the recorded outcome (its Action/Status
			// overwrite result.Action / result.Status when it claims).
			//
			// AUT-P0-001 2026-09-04: preflight refusals are different.
			// On a recognized preflight refusal (adapter_missing /
			// workspace_unresolvable / lane_unready) the envelope MUST
			// stay byte-identical — letting DispatchStep fall through
			// would claim it via the existing claim path, violating
			// the invariant. Short-circuit with a refused verdict so
			// the envelope, lease, events and RunAttempt set are
			// untouched; the next Continue tick re-runs preflight.
			//
			// R6E 2026-09-10: the same protection covers an INELIGIBLE
			// head, and any stage ending whose recorded refusals make
			// the bench head untouchable (skipped as another executor's
			// work or preflight-refused) even when the terminal Skipped
			// reason came from a later candidate's gate. The fall-through
			// claims the FIRST ready item — if that item was refused as
			// untouchable, falling through would mutate it, which the
			// refused-list contract forbids.
			if stage.Skipped == "capacity_unknown" || isPreflightRefusalReason(stage.Skipped) || executeStageRefusedHeadUntouchable(stage) {
				result.Status = "refused"
				return s.recordContinue(now, result)
			}
		}
	} else if opts.MaxExecuteSteps > 0 {
		// The seam did not run because there was no ready envelope on
		// the bench (the threshold predicate requires `ready` count >=
		// 1). Stamp a verdict so the heartbeat log shows the operator
		// WHY the unattended stage declined this tick: there was no
		// eligible work. Without this stamp the JSON carries only
		// `{"status":"waiting"}` and a tired human cannot distinguish
		// "execute stage ran and refused" from "execute stage had
		// nothing to do" from "execute stage was never invoked" — that
		// ambiguity is exactly the orchestrator's deliverable 0.
		result.Execute = &ExecuteVerdict{
			Stage:   "execute",
			Skipped: "no_ready_envelope",
			Human:   "did nothing: no ready envelope on the bench; the unattended execute stage had nothing to act on this tick",
			Status:  "not_invoked",
		}
	}

	// R6P: an envelope an alive detached door owns is not the tick's to step.
	// The door expects the envelope exactly as the seam left it (claimed); a
	// DispatchStep advance here would make the door refuse and strand the run.
	if report.DispatchNext.CanRun {
		if door, owned := DetachedDoorOwns(s.home, report.DispatchNext.WorkID); owned {
			result.Status = "waiting"
			result.WaitReason = "detached_door_running"
			result.NoAction = fmt.Sprintf("envelope %s is owned by a detached governed door (pid %d, log %s); the door advances it, the tick only reconciles", door.WorkID, door.PID, door.LogPath)
			return s.recordContinue(now, result)
		}
	}

	if report.DispatchNext.CanRun && continueMatchesExpected(expectWorkID, report.DispatchNext.WorkID) {
		// An explicit pin owns the outer snapshot. Unpinned dispatch retains
		// policy-aware selection and binds its own selected generation.
		var source WorkbenchItem
		if expectWorkID != "" {
			source = dispatchSnapshot(report, expectWorkID)
		}
		step, err := s.DispatchStep(now, DispatchStepOptions{
			ExpectWorkID:             source.WorkID,
			ExpectedWorkState:        source.State,
			ExpectedPolicyDecisionID: source.PolicyDecisionID,
			ExpectedActionIntentID:   source.sourceActionIntentID,
			Actor:                    actor,
			RecentLimit:              opts.RecentLimit,
			Outcome:                  opts.Outcome,
			MoveType:                 opts.MoveType,
			Summary:                  opts.Summary,
		})
		if errors.Is(err, ErrExecutionReservationChanged) {
			result.Status = "target_changed"
			result.NoAction = err.Error()
			return s.recordContinue(now, result)
		}
		if err != nil {
			return ContinueResult{}, err
		}
		if expectWorkID != "" && step.WorkID != "" && step.WorkID != expectWorkID {
			result.Status = "target_changed"
			result.NoAction = "next safe lifecycle target changed; refresh before continuing"
			return s.recordContinue(now, result)
		}
		result.Action = step.Action
		result.Status = step.Status
		result.WorkID = step.WorkID
		result.DispatchStep = &step
		result.ContextPackID = step.ContextPackID
		result.CapabilitiesOffered = append([]DispatchCapabilityOffer{}, step.CapabilitiesOffered...)
		result.WorkerPromptMD = step.WorkerPromptMD
		result.NoAction = step.NoAction
		return s.recordContinue(now, result)
	}

	if item, plan, blocked := s.continuePrepareCandidate(now, report, actor, expectWorkID); item != nil {
		recordedPlan, err := s.AdvancePlan(item.WorkID, now, AdvancePlanOptions{Actor: actor, To: "ready", Record: true})
		if err != nil {
			return ContinueResult{}, err
		}
		advanced, err := s.Advance(item.WorkID, AdvanceInput{
			To: "ready", Actor: actor, Note: "continue prepare",
			ExpectedWorkState: item.State, ExpectedPolicyDecisionID: item.PolicyDecisionID,
			ExpectedActionIntentID: item.sourceActionIntentID,
		}, now)
		if err != nil {
			return ContinueResult{}, err
		}
		result.Action = "prepare"
		result.Status = advanced.State
		result.WorkID = advanced.ID
		result.AdvancePlan = &recordedPlan
		result.PolicyDecisionID = advanced.PolicyDecisionID
		if len(advanced.ActionIntentIDs) > 0 {
			result.ActionIntentID = advanced.ActionIntentIDs[len(advanced.ActionIntentIDs)-1]
		}
		if plan != nil {
			result.Integrity = mergeUnique(result.Integrity, plan.Warnings)
		}
		result.BlockedOn = mergeUnique(result.BlockedOn, blocked)
		return s.recordContinue(now, result)
	}

	if preview := continueAdmissionPreview(report.AdmissionPreviews, expectWorkID); preview != nil {
		admission, err := s.AdmitCreatePlanPath(preview.PlanPath, now, AdmitCreatePlanOptions{
			Actor: actor,
			Note:  "continued_from_admission_preview",
		})
		if err != nil {
			return ContinueResult{}, err
		}
		result.Action = "admit"
		result.Status = admission.Status
		result.WorkID = admission.WorkID
		result.Admission = &admission
		result.ActionIntentID = admission.ActionIntentID
		result.PolicyDecisionID = admission.PolicyDecisionID
		result.NoAction = admission.NoAction
		return s.recordContinue(now, result)
	}

	result.NoAction = continueNoAction(report, expectWorkID)
	return s.recordContinue(now, result)
}

// isPreflightRefusalReason reports whether a skip reason comes from the
// execute-stage preflight (adapter_missing / workspace_unresolvable /
// lane_unready) so Continue can short-circuit without falling through to
// DispatchStep. AUT-P0-001 2026-09-04: on these refusals the envelope,
// lease, events and RunAttempt set must stay byte-identical — a fall-
// through would claim the envelope via the existing claim path.
func isPreflightRefusalReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "adapter_missing", "workspace_unresolvable", "lane_unready", "preflight_failed":
		return true
	default:
		return false
	}
}

// continueExecuteStageThreshold is the readiness predicate for invoking
// the R3A execute-stage seam: at least one ready envelope AND every prior
// branch returned the "no safe lifecycle step" shape. The check happens
// only here, never on admit/prepare/claim/advance paths.
func continueExecuteStageThreshold(report WorkbenchReport) bool {
	if report.Counts.Open == 0 && report.Counts.NeedsClaim == 0 {
		return false
	}
	if stateCount(report, "ready") == 0 {
		return false
	}
	return true
}

// buildExecuteVerdict translates a stage result into the structured verdict
// that lands on ContinueResult.Execute. It is the seam's single source of
// truth for "what did the execute stage do this tick?": either the launch
// minted one or more ratt ids (RunAttemptIDs non-empty, Skipped empty,
// Status the post-launch envelope state), or the stage refused / fell
// through (Skipped non-empty, RunAttemptIDs empty, Status "refused" or the
// current envelope state).
//
// Human defaults to a plain-language fallback ("execute stage ran with no
// recorded outcome") when the stage result did not bring one — defence in
// depth against a future stage refactor that forgets to stamp Human, so a
// tired operator always sees something they can read.
func buildExecuteVerdict(stage ContinueResult, finalStatus string) *ExecuteVerdict {
	status := strings.TrimSpace(finalStatus)
	if status == "" {
		status = stage.Status
	}
	if status == "" {
		status = "unknown"
	}
	human := strings.TrimSpace(stage.Human)
	if human == "" {
		human = "execute stage ran with no recorded outcome"
	}
	return &ExecuteVerdict{
		Stage:         "execute",
		Skipped:       strings.TrimSpace(stage.Skipped),
		Human:         human,
		Lane:          strings.TrimSpace(stage.Lane),
		Status:        status,
		RunAttemptIDs: append([]string(nil), stage.RunAttemptIDs...),
		Detached:      stage.Detached,
	}
}

// buildDryRunVerdict formats the dry-run planning line into the same
// ExecuteVerdict shape the live launch path uses. The seam stamps Human with
// "would execute <work_id> on <lane> (capacity N%, policy <id>)" before this
// helper runs, so the verdict's Human field carries that line verbatim. The
// envelope stays in its current state (ready) — the helper does NOT mint a
// RunAttempt or call Advance(verifying).
func buildDryRunVerdict(stage ContinueResult) *ExecuteVerdict {
	human := strings.TrimSpace(stage.Human)
	if human == "" {
		human = fmt.Sprintf("would execute %s on %s (dry-run)", stage.WorkID, stage.Lane)
	}
	verdict := buildExecuteVerdict(stage, "dry_run")
	verdict.WouldExecute = human
	verdict.Human = human
	verdict.Skipped = "would_execute"
	return verdict
}

func stateCount(report WorkbenchReport, state string) int {
	if report.Counts.ByState == nil {
		return 0
	}
	return report.Counts.ByState[state]
}

func (s *Store) ContinueRun(now time.Time, opts ContinueRunOptions) (ContinueRunResult, error) {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
		opts.Actor = actor
	}
	maxSteps := opts.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultWorkContinueMaxSteps
	}
	result := ContinueRunResult{
		SchemaVersion: WorkContinueRunSchemaVersion,
		ID:            idFor("wconr_", actor, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "waiting",
		Actor:         actor,
		ExpectWorkID:  strings.TrimSpace(opts.ExpectWorkID),
		MaxSteps:      maxSteps,
		Warnings: []string{
			"continues repeated safe local lifecycle steps until the next real wait",
			executeWarning(opts.ContinueOptions),
			"does not observe or synthesize traces",
			"does not promote memory",
		},
	}
	for i := 0; i < maxSteps; i++ {
		step, err := s.Continue(now.Add(time.Duration(i)*time.Nanosecond), opts.ContinueOptions)
		if err != nil {
			return ContinueRunResult{}, err
		}
		result.Steps = append(result.Steps, step)
		result.StepCount = len(result.Steps)
		if step.Execute != nil {
			result.Execute = step.Execute
		}
		result.NextAction = step.NextAction
		result.WaitReason = step.WaitReason
		result.BlockedOn = append([]string{}, step.BlockedOn...)
		result.Integrity = mergeUnique(result.Integrity, step.Integrity)
		if opts.DryRun {
			result.Status = step.Status
			return result, nil
		}
		if !step.CanContinue {
			result.Status = "waiting"
			result.CanContinue = false
			result.ContinueCommand = nil
			return s.recordContinueRun(result)
		}
	}
	result.Status = "max_steps_reached"
	result.CanContinue = true
	result.WaitReason = ""
	result.BlockedOn = nil
	result.ContinueCommand = continueCommand(actor, strings.TrimSpace(opts.ExpectWorkID), true, maxSteps)
	return s.recordContinueRun(result)
}

func (s *Store) recordContinue(now time.Time, result ContinueResult) (ContinueResult, error) {
	if len(result.DetachedReconciled) > 0 {
		// R6P: every recorded tick names what became of earlier detached doors.
		prefix := detachedReconcileSummary(result.DetachedReconciled)
		if result.Human != "" && !strings.HasPrefix(result.Human, prefix) {
			result.Human = prefix + result.Human
		}
		if result.Execute != nil {
			result.Execute.Reconciled = append([]DetachedReconcile(nil), result.DetachedReconciled...)
			if !strings.HasPrefix(result.Execute.Human, prefix) {
				result.Execute.Human = prefix + result.Execute.Human
			}
		}
	}
	post, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 0, Actor: result.Actor})
	if err != nil {
		return ContinueResult{}, err
	}
	result.PostCounts = &post.Counts
	result.NextAction = continueNextAction(result, post)
	if continueStepMadeProgress(result) {
		result.CanContinue, result.ContinueCommand = continueCanContinue(post, result.Actor, result.ExpectWorkID)
		// R6P: the tick never steps an envelope an alive detached door owns, so
		// an --until-wait run stops here instead of spinning on it.
		if result.CanContinue && post.DispatchNext.CanRun {
			if _, owned := DetachedDoorOwns(s.home, post.DispatchNext.WorkID); owned {
				result.CanContinue = false
				result.ContinueCommand = nil
				if result.WaitReason == "" {
					result.WaitReason = "detached_door_running"
				}
			}
		}
	}
	if !result.CanContinue {
		result.WaitReason, result.BlockedOn = continueWait(result, post)
	}
	result.Integrity = mergeUnique(result.Integrity, post.Integrity)
	path, err := s.RecordContinue(result)
	if err != nil {
		return ContinueResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

func continueStepMadeProgress(result ContinueResult) bool {
	switch result.Action {
	case "admit":
		return result.Status == "admitted" && strings.TrimSpace(result.WorkID) != ""
	case "prepare":
		return result.Status == "ready" && strings.TrimSpace(result.WorkID) != ""
	case "claim", "advance", "close":
		return dispatchStepMadeProgress(DispatchStepResult{Status: result.Status})
	default:
		return false
	}
}

func (s *Store) recordContinueRun(result ContinueRunResult) (ContinueRunResult, error) {
	path, err := s.RecordContinueRun(result)
	if err != nil {
		return ContinueRunResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

func (s *Store) continuePrepareCandidate(now time.Time, report WorkbenchReport, actor, expectWorkID string) (*WorkbenchItem, *AdvancePlan, []string) {
	var blocked []string
	for i := range report.Open {
		item := report.Open[i]
		if !continueMatchesExpected(expectWorkID, item.WorkID) {
			continue
		}
		if item.State != "proposed" && item.State != "admitted" {
			continue
		}
		// Enforce the same admission gate as continueAdmissionPreview: a candidate
		// whose action context requires operator inputs must not be advanced to
		// ready by the autonomous loop, even when policy allows the local advance.
		// Otherwise proposed->ready would bypass the operator-owned admission gate.
		if workActionContextRequiresOperator(item.ActionContext) {
			blocked = append(blocked, "work.ready_state:"+item.WorkID)
			continue
		}
		plan, err := s.AdvancePlan(item.WorkID, now, AdvancePlanOptions{Actor: actor, To: "ready"})
		if err != nil {
			blocked = append(blocked, "work.ready_state:"+item.WorkID)
			continue
		}
		if plan.Policy.Allowed && strings.TrimSpace(plan.NoAction) == "" {
			return &item, &plan, blocked
		}
		blocked = append(blocked, "work.ready_state:"+item.WorkID)
	}
	return nil, nil, blocked
}

func continueAdmissionPreview(previews []WorkbenchAdmissionPreview, expectWorkID string) *WorkbenchAdmissionPreview {
	for i := range previews {
		preview := previews[i]
		if !continueMatchesExpected(expectWorkID, preview.WorkID) {
			continue
		}
		if !preview.PolicyAllowed || preview.RequiresOperator || strings.TrimSpace(preview.AdmissionBlockedReason) != "" {
			continue
		}
		if preview.Staleness != "fresh" || strings.TrimSpace(preview.PlanPath) == "" {
			continue
		}
		return &preview
	}
	return nil
}

func continueCanContinue(report WorkbenchReport, actor, expectWorkID string) (bool, []string) {
	if report.DispatchNext.CanRun && continueMatchesExpected(expectWorkID, report.DispatchNext.WorkID) {
		return true, continueCommand(actor, expectWorkID, false, 0)
	}
	for _, item := range report.Open {
		if continueMatchesExpected(expectWorkID, item.WorkID) && (item.State == "proposed" || item.State == "admitted") {
			// Operator-required proposed/admitted work cannot be safely prepared
			// (see continuePrepareCandidate), so it is not a continuation target;
			// treating it as one would spin ContinueRun to max_steps. This mirrors
			// the operator-required filtering continueAdmissionPreview applies below.
			if workActionContextRequiresOperator(item.ActionContext) {
				continue
			}
			return true, continueCommand(actor, expectWorkID, false, 0)
		}
	}
	if continueAdmissionPreview(report.AdmissionPreviews, expectWorkID) != nil {
		return true, continueCommand(actor, expectWorkID, false, 0)
	}
	return false, nil
}

func continueWait(result ContinueResult, report WorkbenchReport) (string, []string) {
	if result.Status == "target_changed" {
		blocked := []string{"work.target_changed"}
		if result.ExpectWorkID != "" {
			blocked = append(blocked, "work.target_changed:"+result.ExpectWorkID)
		}
		return "target_changed", blocked
	}
	workID := result.WorkID
	if strings.TrimSpace(workID) == "" {
		workID = result.ExpectWorkID
	}
	reason, blockedOn := dispatchStepWait(DispatchStepResult{Actor: result.Actor, Status: result.Status, WorkID: workID}, report)
	for _, preview := range report.AdmissionPreviews {
		if !continueMatchesExpected(result.ExpectWorkID, preview.WorkID) {
			continue
		}
		if preview.RequiresOperator || strings.TrimSpace(preview.AdmissionBlockedReason) != "" || preview.Staleness != "fresh" || !preview.PolicyAllowed {
			blockedOn = append(blockedOn, "work.admission_preview:"+preview.WorkID)
		}
	}
	if len(blockedOn) == 0 && result.ExpectWorkID != "" {
		blockedOn = append(blockedOn, "work.expected:"+result.ExpectWorkID)
	}
	if len(blockedOn) > 0 && reason == "no_open_work" {
		reason = "needs_review"
	}
	return reason, mergeUnique(blockedOn)
}

func continueNextAction(result ContinueResult, post WorkbenchReport) string {
	if strings.TrimSpace(result.WorkID) != "" {
		for _, item := range post.Open {
			if item.WorkID == result.WorkID {
				return item.NextAction
			}
		}
		for _, item := range post.Recent {
			if item.WorkID == result.WorkID {
				return item.NextAction
			}
		}
	}
	if strings.TrimSpace(result.NoAction) != "" {
		return result.NoAction
	}
	if post.DispatchNext.CanRun {
		return "continue the next safe lifecycle step"
	}
	if continueAdmissionPreview(post.AdmissionPreviews, result.ExpectWorkID) != nil {
		return "add the suggested task to work"
	}
	return "waiting for work that can safely continue"
}

func continueNoAction(report WorkbenchReport, expectWorkID string) string {
	if expectWorkID != "" {
		return "expected work is not ready for a safe continue step"
	}
	if report.Counts.Open == 0 && len(report.AdmissionPreviews) == 0 {
		return "workbench has no open or suggested work"
	}
	return "no safe lifecycle step is ready"
}

func continueMatchesExpected(expectWorkID, workID string) bool {
	expectWorkID = strings.TrimSpace(expectWorkID)
	if expectWorkID == "" {
		return true
	}
	return strings.TrimSpace(workID) == expectWorkID
}

func continueCommand(actor, expectWorkID string, untilWait bool, maxSteps int) []string {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "codex"
	}
	out := []string{"compound", "work", "continue", "--actor", actor}
	if strings.TrimSpace(expectWorkID) != "" {
		out = append(out, "--expect-work", strings.TrimSpace(expectWorkID))
	}
	if untilWait {
		out = append(out, "--until-wait")
		if maxSteps > 0 {
			out = append(out, "--max-steps", strconv.Itoa(maxSteps))
		}
	}
	return out
}

func (s *Store) RecordContinue(result ContinueResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "continues")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) RecordContinueRun(result ContinueRunResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "continue-runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}
