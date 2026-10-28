// Package hook is the capability-aware hook fixture loop — the seam where a harness's
// lifecycle is normalized into the typed spine. Hooks are CLIENTS, not brains: each one
// REQUESTS context / GUARDS a mutation / CAPTURES a receipt by producing typed envelopes,
// and is gated by what the harness verifiably supports (a deny on a harness that cannot
// block becomes fail_open, surfaced — never a silent pretense of enforcement).
// See docs/architecture/runtime.md §3. No live hook install before this loop passes.
package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"compound/internal/authority"
	"compound/internal/authority/gitrepository"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/harness"
	"compound/internal/hookplan"
	"compound/internal/mem"
	"compound/internal/policy"
	"compound/internal/tooltopology"
	"compound/internal/work"
)

// MaxContextChars is the whole-session context budget a ContextPack is capped at (see
// buildContextRequest). It doubles as the tool-output externalization threshold: a single tool
// body at or beyond the entire context budget must never flood the window — postToolUse captures
// it to the rebuildable, content-addressed blob store and replaces it with a compact pointer (C3).
// One number, one meaning ("as big as the whole context"), so the two stay tied by construction.
const MaxContextChars = 6000

// hookEnum maps the dashed CLI/fixture name to the HookEvent enum name.
var hookEnum = map[string]string{
	"session-start":      "SessionStart",
	"user-prompt-submit": "UserPromptSubmit",
	"pre-tool-use":       "PreToolUse",
	"post-tool-use":      "PostToolUse",
	"session-end":        "SessionEnd",
	"stop":               "Stop",
}

// hookCapability is the HookEvent.hook_capability for each hook.
var hookCapability = map[string]string{
	"SessionStart":     "context_injection",
	"UserPromptSubmit": "context_injection",
	"PreToolUse":       "policy_gate",
	"PostToolUse":      "telemetry",
	"SessionEnd":       "receipt",
	"Stop":             "receipt",
}

// Fixture is one normalized hook invocation (a fixture file, or a live hook's stdin).
type Fixture struct {
	Hook              string                        `json:"hook"`
	Harness           string                        `json:"harness"`
	Live              bool                          `json:"live,omitempty"`
	Cwd               string                        `json:"cwd,omitempty"`
	Task              string                        `json:"task,omitempty"`
	Mode              string                        `json:"mode,omitempty"`
	RepoKind          string                        `json:"repo_kind,omitempty"`
	Topic             string                        `json:"topic,omitempty"`   // optional; narrows ambient recall to this topic's subtree
	Prompt            string                        `json:"prompt,omitempty"`  // user-prompt-submit; only hashed, never stored raw
	Action            *envelope.ActionIntent        `json:"action,omitempty"`  // pre-tool-use
	Actions           []envelope.ActionIntent       `json:"actions,omitempty"` // pre-tool-use tool calls that target several paths
	SessionID         string                        `json:"session_id,omitempty"`
	TurnID            string                        `json:"turn_id,omitempty"`
	WorkID            string                        `json:"work_id,omitempty"`         // session-end
	TranscriptPath    string                        `json:"transcript_path,omitempty"` // session-end/stop: the harness transcript to scan for consumed injected claims
	StopHookActive    bool                          `json:"stop_hook_active,omitempty"`
	StopTriageHandled bool                          `json:"-"`
	LastAssistant     string                        `json:"last_assistant_message,omitempty"`
	MoveType          string                        `json:"move_type,omitempty"`
	Outcome           string                        `json:"outcome,omitempty"`
	Summary           string                        `json:"summary,omitempty"`
	Used              []string                      `json:"used,omitempty"`
	Missed            []string                      `json:"missed,omitempty"`
	FalseTriggered    []string                      `json:"false_triggered,omitempty"`
	Tool              string                        `json:"tool,omitempty"`        // post-tool-use
	ToolOutput        string                        `json:"tool_output,omitempty"` // post-tool-use: the tool's response body; large bodies are externalized (C3), never inlined
	Placement         *policy.PlacementGuardContext `json:"placement,omitempty"`

	// Enforced is trusted adapter posture. Fixture JSON cannot set it; live
	// callers derive it from their explicit enforcement flag after loading.
	Enforced bool `json:"-"`
	// AdapterSideEffects are governed artifacts prepared by a live harness
	// adapter before Run persists the HookEvent. Fixture JSON cannot inject them.
	AdapterSideEffects []string `json:"-"`
	// MutationInputRef is adapter-derived on successful PostToolUse and can
	// promote only the exact pending governed mutation for this session.
	MutationInputRef string `json:"-"`
	// CapabilityUse is adapter-derived, bounded Skill invocation metadata.
	// Raw tool input never reaches the HookEvent.
	CapabilityUse *envelope.CapabilityUse `json:"-"`
	// CoordinationDelivery is adapter-derived recipient state. It is excluded
	// from fixture JSON so replay fixtures cannot impersonate live relevance.
	CoordinationDelivery *CoordinationDeliveryInput `json:"-"`
}

// Deps are the services a hook orchestrates.
type Deps struct {
	Registry harness.Registry
	Orient   contract.OrientationPolicy
	Policy   policy.Config
	Store    *work.Store // required only for session-end; may be nil otherwise
	HomePath string      // optional; when present session-start persists context artifacts

	Now                         func() time.Time
	After                       func(time.Duration) <-chan time.Time
	BeforeContextBuild          func()
	BeforeContextPersist        func()
	BeforeHookEventPersist      func() error
	BeforeDeliveryReportPersist func() error
	BeforeDeliveryStatePersist  func() error
}

// InjectionArtifact records the exact harness-facing context text written under
// COMPOUND_HOME. The markdown is the consumed artifact; the pack JSON is the
// typed source it was rendered from.
type InjectionArtifact struct {
	Ref                string `json:"ref"`
	Path               string `json:"path"`
	ContextRequestID   string `json:"context_request_id"`
	ContextRequestRef  string `json:"context_request_ref"`
	ContextRequestPath string `json:"context_request_path"`
	ContractID         string `json:"contract_id"`
	ContractRef        string `json:"contract_ref"`
	ContractPath       string `json:"contract_path"`
	ContextPackID      string `json:"context_pack_id"`
	ContextPackRef     string `json:"context_pack_ref"`
	ContextPackPath    string `json:"context_pack_path"`
}

// ContextObservation exposes the exact successful production context build to
// adapters that record retrieval evidence. It is intentionally excluded from
// hook JSON: the ContextPack and raw input remain owned by their normal
// COMPOUND_HOME artifacts, while adapters persist only the parity journal row.
type ContextObservation struct {
	Input            contract.Input
	Pack             envelope.ContextPack
	RetrievalLatency time.Duration
}

// CoordinationDeliveryInput is adapter-derived recipient state for the
// hookplan LifecyclePlan boundary. Hooks pass it through; hookplan owns
// relevance, cursor, boundary, budget, and native compilation semantics.
type CoordinationDeliveryInput struct {
	CursorKey      string
	Recipient      hookplan.DeliveryRecipient
	Trigger        string
	BoundarySource string
	Budget         *hookplan.DeliveryBudget
}

// Result is what a hook produces: the normalized event, the typed objects, and the small
// harness-facing output line.
type Result struct {
	Event                envelope.HookEvent       `json:"event"`
	Produced             []envelope.Envelope      `json:"produced"`
	Summary              string                   `json:"summary"`
	ContextOutput        string                   `json:"-"`
	Injection            *InjectionArtifact       `json:"injection,omitempty"`
	Context              *ContextObservation      `json:"-"`
	CoordinationDelivery *hookplan.DeliveryReport `json:"-"`
	BreakerTripped       bool                     `json:"breaker_tripped,omitempty"`
	pendingExactApproval *pendingExactApproval
}

// NativePermissionDecision projects the typed PreToolUse result into the
// host-neutral permission decision exposed by live adapters. Approval-plan
// prose is accepted only when one canonical evidence ref backs one exact,
// command-free operator-review sentence. When the hook also minted an
// operator-approval-frontier request, its id — and only its id — is appended
// so the agent can wait, take an alternative, or record blocked-with-ticket
// against a stable reference. The resolve command stays OUT of this
// agent-visible message on purpose: handing a held agent the exact approve
// command would let it drive its own approval path. The full command remains
// on the PolicyDecision reasons and evidence refs, the operator-facing surface.
//
// The projection is posture-aware. A surviving `hold` becomes `ask` only where
// a human can answer it; on an unattended lane it becomes `deny` carrying the
// SAME reason and the SAME approval-request id. See harness.Posture for why the
// decision itself does not change: an unanswerable prompt is not a stricter
// gate, it is an outage that reports itself as work in progress.
func NativePermissionDecision(res Result) (string, string) {
	return NativePermissionDecisionForPosture(res, harness.PostureInteractive)
}

// NativePermissionDecisionForHarness resolves the posture from the registry
// that is already in force for this hook run, then projects. This is the seam
// live adapters use: they know the harness name, and the registry is the one
// place that records what that name's posture is.
func NativePermissionDecisionForHarness(res Result, registry harness.Registry, harnessName string) (string, string) {
	return NativePermissionDecisionForPosture(res, registry.Posture(harnessName))
}

// NativePermissionDecisionForPosture is the posture-taking owner. An
// unrecognized posture resolves through harness.DefaultPosture rather than
// falling back to `ask`, so a mis-typed posture cannot reintroduce the freeze.
func NativePermissionDecisionForPosture(res Result, posture string) (string, string) {
	holdDecision := "ask"
	if harness.NormalizePosture(posture) == harness.PostureUnattended {
		holdDecision = "deny"
	}
	coordinationDeny := res.CoordinationDelivery != nil &&
		res.CoordinationDelivery.ExecutionHeld
	if coordinationDeny {
		reason := ""
		for _, produced := range res.Produced {
			pd, ok := produced.(*envelope.PolicyDecision)
			if !ok || policy.Allowed(*pd) {
				continue
			}
			reason = nativePolicyDecisionReason(pd.Reasons, "")
			if reason != "" {
				break
			}
		}
		if reason == "" {
			reason = strings.TrimSpace(res.CoordinationDelivery.DegradedReason)
		}
		if reason == "" {
			reason = "relevant coordination interrupt requires governed clearance"
		}
		return "deny", reason
	}
	// Watchdog-enforced block without a typed PolicyDecision: the budget
	// watchdog answered pre-tool-use before the policy engine finished, so
	// Produced is empty but the event already records an enforced block.
	// Projecting ("", "") here made fail-closed harness shims (the omp
	// compound.ts shim treats a decision-less doc on a non-zero exit as
	// "compound policy gate unavailable") hide the real reason from the
	// agent. Deny with the watchdog summary is the faithful projection:
	// the decision itself does not change, only its legibility.
	if res.Event.Status == "blocked" && res.Event.EffectiveOutcome == "block" && len(res.Produced) == 0 {
		reason := strings.TrimSpace(res.Summary)
		if reason == "" {
			reason = "compound policy evaluation exceeded its wall-clock budget; fail-closed"
		}
		return "deny", reason
	}
	for _, produced := range res.Produced {
		pd, ok := produced.(*envelope.PolicyDecision)
		if !ok || policy.Allowed(*pd) {
			continue
		}
		if pd.Outcome == "hold" {
			if reason := exactApprovalOperatorReviewReason(pd.Reasons, pd.EvidenceRefs); reason != "" {
				return holdDecision, operatorActionApprovalFrontierIDSuffix(pd.Reasons, reason)
			}
		}
		reason := nativePolicyDecisionReason(pd.Reasons, res.Summary)
		if pd.Outcome == "hold" {
			return holdDecision, reason
		}
		return "deny", reason
	}
	return "", ""
}

// operatorActionApprovalFrontierIDSuffix appends the minted operator-approval
// request id — and only the id — to the agent-visible message. The resolve
// command is withheld deliberately: the id is inert (approving still requires
// the operator-only command, itself a governed action) while the command would
// hand a held agent the exact means to clear its own hold. Callers rely on this
// function never emitting the approve command, "--granted-by", or an
// "operator:"/"human:" grantee token.
func operatorActionApprovalFrontierIDSuffix(reasons []string, baseReason string) string {
	const prefix = "Operator approval request "
	for _, reason := range reasons {
		trimmed := strings.TrimSpace(reason)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		rest := strings.TrimPrefix(trimmed, prefix)
		id := rest
		if idx := strings.IndexAny(rest, " ;"); idx >= 0 {
			id = rest[:idx]
		}
		if id = strings.TrimSpace(id); id == "" {
			return baseReason
		}
		if strings.Contains(baseReason, id) {
			return baseReason
		}
		return baseReason + " Operator approval request " + id + " is pending operator review."
	}
	return baseReason
}

func exactApprovalOperatorReviewReason(reasons, evidenceRefs []string) string {
	const (
		reasonPrefix = "Exact approval plan "
		reasonSuffix = " is ready for operator review."
		refPrefix    = "compound://events/approval_plan_"
	)
	planID := ""
	for _, ref := range evidenceRefs {
		planLike := strings.Contains(strings.ToLower(ref), "approval_plan")
		if !strings.HasPrefix(ref, refPrefix) {
			if planLike {
				return ""
			}
			continue
		}
		digest := strings.TrimPrefix(ref, refPrefix)
		if len(digest) != 12 || strings.ToLower(digest) != digest {
			return ""
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return ""
		}
		if planID != "" {
			return ""
		}
		planID = "approval_plan_" + digest
	}
	if planID == "" {
		return ""
	}
	want := reasonPrefix + planID + reasonSuffix
	matched := 0
	for _, reason := range reasons {
		if reason == want {
			matched++
			continue
		}
		if approvalPlanLikeReason(reason) {
			return ""
		}
	}
	if matched == 1 {
		return want
	}
	return ""
}

func nativePolicyDecisionReason(reasons []string, fallback string) string {
	safe := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		reason = strings.TrimSpace(reason)
		if reason == "" || approvalPlanLikeReason(reason) {
			continue
		}
		safe = append(safe, reason)
	}
	if len(safe) > 0 {
		return strings.Join(safe, "; ")
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" && !approvalPlanLikeReason(fallback) {
		return fallback
	}
	return "Compound policy requires governed clearance"
}

func approvalPlanLikeReason(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "approval_plan") ||
		strings.Contains(lower, "approval plan") ||
		strings.Contains(lower, "exact approval")
}

type pendingExactApproval struct {
	fixture  Fixture
	prepared preparedPreToolAction
	action   envelope.ActionIntent
	decision envelope.PolicyDecision
}

// Run dispatches a hook fixture, validating everything it produces through the spine.
func Run(fx Fixture, deps Deps, now time.Time) (Result, error) {
	name, ok := hookEnum[fx.Hook]
	if !ok {
		return Result{}, fmt.Errorf("unknown hook %q (want session-start|user-prompt-submit|pre-tool-use|post-tool-use|session-end|stop)", fx.Hook)
	}
	if strings.TrimSpace(fx.Harness) == "" {
		return Result{}, fmt.Errorf("hook fixture needs a harness")
	}
	var (
		res Result
		err error
	)
	switch name {
	case "SessionStart":
		res, err = sessionStart(name, fx, deps, now)
	case "UserPromptSubmit":
		res, err = userPrompt(name, fx, deps, now)
	case "PreToolUse":
		res, err = preToolUse(name, fx, deps, now)
	case "PostToolUse":
		res, err = postToolUse(name, fx, deps, now)
	case "SessionEnd", "Stop":
		res, err = sessionEnd(name, fx, deps, now)
	default:
		return Result{}, fmt.Errorf("unhandled hook %q", name)
	}
	if err != nil {
		return res, err
	}
	if fx.CoordinationDelivery != nil {
		delivery, err := hookplan.DeliverCoordinationWithPublisher(hookplan.DeliveryRequest{
			HomePath:            deps.HomePath,
			Harness:             fx.Harness,
			CursorKey:           fx.CoordinationDelivery.CursorKey,
			Recipient:           fx.CoordinationDelivery.Recipient,
			Trigger:             fx.CoordinationDelivery.Trigger,
			BoundarySource:      fx.CoordinationDelivery.BoundarySource,
			Budget:              fx.CoordinationDelivery.Budget,
			BeforeReportPersist: deps.BeforeDeliveryReportPersist,
			BeforeStatePersist:  deps.BeforeDeliveryStatePersist,
		}, func(delivery hookplan.DeliveryReport) error {
			if delivery.ExecutionHeld && name != "PreToolUse" {
				return fmt.Errorf("coordination interrupt delivery requires PreToolUse before mutation")
			}
			res.CoordinationDelivery = &delivery
			if delivery.Native != nil {
				res.ContextOutput = appendContextOutput(res.ContextOutput, delivery.Native.Context)
			}
			if delivery.ReportRef != "" {
				res.Event.SideEffects = mergeRefs(res.Event.SideEffects, []string{delivery.ReportRef})
			}
			if name == "PreToolUse" && res.pendingExactApproval != nil && !delivery.ExecutionHeld {
				if err := finalizePendingExactApproval(&res, deps, delivery, now); err != nil {
					return err
				}
			}
			if delivery.ExecutionHeld && name == "PreToolUse" {
				if err := applyCoordinationExecutionHold(&res, deps, fx, delivery, now); err != nil {
					return err
				}
			}
			if err := validateHookResult(res); err != nil {
				return err
			}
			if deps.BeforeHookEventPersist != nil {
				if err := deps.BeforeHookEventPersist(); err != nil {
					return err
				}
			}
			if strings.TrimSpace(deps.HomePath) != "" {
				if err := writeHookEvent(deps.HomePath, res.Event, res.BreakerTripped); err != nil {
					return fmt.Errorf("persist hook event: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			return res, fmt.Errorf("coordination delivery: %w", err)
		}
		res.CoordinationDelivery = &delivery
		refreshPostToolCaches(deps.HomePath, fx.Live, name == "PostToolUse" && res.Event.CapabilityUse != nil, now)
		return res, nil
	}
	if err := validateHookResult(res); err != nil {
		return res, err
	}
	if deps.BeforeHookEventPersist != nil {
		if err := deps.BeforeHookEventPersist(); err != nil {
			return res, err
		}
	}
	if strings.TrimSpace(deps.HomePath) != "" {
		if err := writeHookEvent(deps.HomePath, res.Event, res.BreakerTripped); err != nil {
			return res, fmt.Errorf("persist hook event: %w", err)
		}
	}
	refreshPostToolCaches(deps.HomePath, fx.Live, name == "PostToolUse" && res.Event.CapabilityUse != nil, now)
	return res, nil
}

func validateHookResult(res Result) error {
	if err := res.Event.Validate(); err != nil {
		return fmt.Errorf("hook event invalid: %w", err)
	}
	for _, e := range res.Produced {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("produced %s invalid: %w", e.Kind(), err)
		}
	}
	return nil
}

func applyCoordinationExecutionHold(res *Result, deps Deps, fx Fixture, delivery hookplan.DeliveryReport, now time.Time) error {
	if res == nil {
		return fmt.Errorf("coordination execution hold requires hook result")
	}
	reason := strings.TrimSpace(delivery.DegradedReason)
	if reason == "" {
		reason = "relevant interrupt evidence requires execution hold"
	}
	deferred := res.pendingExactApproval != nil
	var holds []envelope.Envelope
	var sideEffects []string
	for _, produced := range res.Produced {
		action, ok := produced.(*envelope.ActionIntent)
		if !ok {
			continue
		}
		hold := policy.HoldForExecutionEvidence(*action, delivery.HoldEvidenceRefs, reason, now)
		if err := hold.Validate(); err != nil {
			return err
		}
		if strings.TrimSpace(deps.HomePath) != "" {
			if deferred {
				decisionRef, err := writeGateDecisionArtifact(deps.HomePath, hold)
				if err != nil {
					return err
				}
				sideEffects = append(sideEffects, decisionRef)
			} else {
				effects, err := writeGateArtifacts(deps.HomePath, *action, hold)
				if err != nil {
					return err
				}
				sideEffects = append(sideEffects, effects...)
			}
		}
		holds = append(holds, &hold)
	}
	if len(holds) == 0 {
		return fmt.Errorf("coordination interrupt hold has no PreToolUse action")
	}
	if deferred {
		if len(holds) != 1 {
			return fmt.Errorf("deferred coordination hold requires one direct action")
		}
		if err := replacePreToolPolicyDecision(res, *holds[0].(*envelope.PolicyDecision)); err != nil {
			return err
		}
		res.pendingExactApproval = nil
	} else {
		res.Produced = append(res.Produced, holds...)
	}
	res.Event.SideEffects = mergeRefs(res.Event.SideEffects, sideEffects)
	res.Summary = appendContextOutput(res.Summary, "policy hold: "+reason)
	if deps.Registry.Supports(fx.Harness, "PreToolUse") {
		res.Event.Status = "blocked"
	} else {
		res.Event.Status = "fail_open"
		res.Event.FailOpenReason = envelope.FailOpenCapabilityAbsent
	}
	return nil
}

func appendContextOutput(existing, addition string) string {
	existing = strings.TrimSpace(existing)
	addition = strings.TrimSpace(addition)
	switch {
	case existing == "":
		return addition
	case addition == "":
		return existing
	default:
		return existing + "\n\n" + addition
	}
}

type contextBuild struct {
	Input            contract.Input
	Contract         envelope.EffectiveContract
	Pack             envelope.ContextPack
	RetrievalLatency time.Duration
}

type contextBuildResult struct {
	Build contextBuild
	Err   error
}

func sessionStart(name string, fx Fixture, deps Deps, now time.Time) (Result, error) {
	mode := hookRequestedMode(name, fx)
	built, breaker, err := buildContextWithBreaker(name, fx, deps, mode, now)
	if breaker != nil {
		return *breaker, nil
	}
	if err != nil {
		return Result{}, err
	}
	req := buildContextRequest(name, fx, built.Contract, built.Pack, mode, now)
	ev := newCapabilityEvent(name, fx, fx.Harness, deps, contextHookInputHash(name, fx, mode), fx.AdapterSideEffects, now)
	ev.IDs.ContextPackID = built.Pack.Header.ID
	var injection *InjectionArtifact
	if strings.TrimSpace(deps.HomePath) != "" {
		var err error
		if deps.BeforeContextPersist != nil {
			deps.BeforeContextPersist()
		}
		injection, err = writeContextArtifacts(deps.HomePath, ev.Header.ID, req, built.Contract, built.Pack, renderInjection(injectionRender{
			HookID:  ev.Header.ID,
			Pack:    built.Pack,
			Surface: injectionSurfaceOrientation,
		}))
		if err != nil {
			return Result{}, err
		}
		// Record AFTER the artifacts are durable: a marker written before the content
		// would tell the next prompt that orientation was served when it was not.
		//
		// NON-FATAL, matching renderAndWriteInstructionPack's log-and-continue idiom:
		// a failed marker must not fail the hook that already served the orientation
		// successfully. The cost of losing it is that the per-prompt pointer falls
		// back to its "cannot tell" wording, which is the correct degradation.
		_ = recordSessionOrientationServed(deps.HomePath, fx.SessionID, ev.Header.ID, now)
		ev.SideEffects = append(ev.SideEffects, injection.ContextRequestRef, injection.ContractRef, injection.ContextPackRef, injection.Ref)
		ev.OutputRef = injection.Ref
	}
	return Result{
		Event:     ev,
		Produced:  []envelope.Envelope{&req, &built.Contract, &built.Pack},
		Summary:   built.Pack.SummaryMD,
		Injection: injection,
		// The door map rides the live session-start payload: it is the one seam every
		// served harness (claude_code and its minimax/zai launchers, codex, opencode)
		// already runs. It is a cached observation, served as a visible notice when
		// missing or stale, never as silence.
		ContextOutput: sessionStartDoorMap(fx, deps, now),
		Context: &ContextObservation{
			Input:            built.Input,
			Pack:             built.Pack,
			RetrievalLatency: built.RetrievalLatency,
		},
	}, nil
}

// sessionStartDoorMap serves the cached door map on live session starts only; fixture
// replays stay byte-identical so recorded fixtures do not depend on machine state.
func sessionStartDoorMap(fx Fixture, deps Deps, now time.Time) string {
	if !fx.Live || strings.TrimSpace(deps.HomePath) == "" {
		return ""
	}
	return tooltopology.ServeDoorMap(deps.HomePath, now, tooltopology.DefaultDoorMapTTL)
}

func userPrompt(name string, fx Fixture, deps Deps, now time.Time) (Result, error) {
	mode := hookRequestedMode(name, fx)
	built, breaker, err := buildContextWithBreaker(name, fx, deps, mode, now)
	if breaker != nil {
		return *breaker, nil
	}
	if err != nil {
		return Result{}, err
	}
	if fx.Live {
		return liveUserPrompt(name, fx, deps, now, mode, built)
	}

	// Explicit fixture replay retains the legacy proposed-work model used by
	// migration and lifecycle fixtures. Installed live prompt hooks never enter
	// this branch.
	pred := "address user intent"
	if fx.Task != "" {
		pred = "address: " + fx.Task
	}
	req := buildContextRequest(name, fx, built.Contract, built.Pack, mode, now)
	ev := newCapabilityEvent(name, fx, fx.Harness, deps, contextHookInputHash(name, fx, mode), fx.AdapterSideEffects, now)
	ev.IDs.ContextPackID = built.Pack.Header.ID
	var injection *InjectionArtifact
	if strings.TrimSpace(deps.HomePath) != "" {
		if deps.BeforeContextPersist != nil {
			deps.BeforeContextPersist()
		}
		injection, err = writeContextArtifacts(deps.HomePath, ev.Header.ID, req, built.Contract, built.Pack, renderInjection(injectionRender{
			HookID:          ev.Header.ID,
			Pack:            built.Pack,
			Surface:         injectionSurfaceTask,
			OrientationNote: orientationNoteForSession(deps.HomePath, fx.SessionID, fx.Harness),
		}))
		if err != nil {
			return Result{}, err
		}
		ev.SideEffects = append(ev.SideEffects, injection.ContextRequestRef, injection.ContractRef, injection.ContextPackRef, injection.Ref)
		ev.OutputRef = injection.Ref
	}
	create := work.CreateInput{Owner: fx.Harness, SuccessPredicate: pred, DesiredOutcome: fx.Task, State: "proposed"}
	if injection != nil {
		create.ContextPackID = built.Pack.Header.ID
	}
	cand := work.BuildWork(create, now)
	ev.IDs.WorkID = cand.ID
	produced := []envelope.Envelope{&cand}
	if deps.Store != nil && injection != nil {
		cand, err = deps.Store.Create(create, now)
		if err != nil {
			return Result{}, err
		}
		ev.IDs.WorkID = cand.ID
		ev.SideEffects = append(ev.SideEffects, filepath.Join("work", "work", cand.ID+".json"))
		produced = []envelope.Envelope{&req, &built.Contract, &built.Pack, &cand}
	}
	summary := fmt.Sprintf("candidate work %s (proposed); scope=%q repo=%s; %d ctx in / %d excluded; mutations need ActionIntent+PolicyDecision",
		cand.ID, orDash(fx.Task), built.Input.RepoKind, len(built.Pack.Included), len(built.Pack.Excluded))
	return Result{
		Event:     ev,
		Produced:  produced,
		Summary:   summary,
		Injection: injection,
		Context: &ContextObservation{
			Input:            built.Input,
			Pack:             built.Pack,
			RetrievalLatency: built.RetrievalLatency,
		},
	}, nil
}

// liveUserPrompt treats a prompt as a transient retrieval query, not a durable
// unit of work. Relevance is computed from the raw task before this function is
// called; every value exposed to persistence or adapter journaling carries only
// the opaque intent reference.
func liveUserPrompt(name string, fx Fixture, deps Deps, now time.Time, mode string, built contextBuild) (Result, error) {
	intentRef := livePromptIntentRef(fx)
	built.Input.Task = intentRef
	built.Input.TaskContract.Intent = intentRef
	built.Contract.TaskContract.Intent = intentRef
	built.Pack.TaskContract.Intent = intentRef
	// Re-key the persisted objects from the sanitized input they actually contain. The relevance
	// content above remains the raw-query-built result, but artifact identity must be reproducible
	// without access to that transient query.
	sanitizedContract := contract.Compile(built.Input, deps.Orient, now)
	sanitizedPack := contract.Pack(built.Input, deps.Orient, now)
	built.Contract.Header = sanitizedContract.Header
	built.Pack.Header = sanitizedPack.Header

	// Delta-triggered per-session injection: suppress memory/position refs already
	// injected earlier in this session (recording each as an Excluded entry) and inject
	// only new/re-opened refs. Fail-open — an absent/corrupt/unwritable session state
	// leaves the pack byte-for-byte identical to the full-injection path.
	built.Pack = applyInjectionDelta(built.Pack, built.Input, deps.HomePath, fx.SessionID, now)
	materialized, err := contract.SealRuntimeMaterialization(contract.RuntimeMaterialization{
		EffectiveContract: built.Contract,
		ContextPack:       built.Pack,
	})
	if err != nil {
		return Result{}, err
	}
	built.Contract = materialized.EffectiveContract
	built.Pack = materialized.ContextPack

	persistedFX := fx
	persistedFX.Prompt = ""
	persistedFX.Task = intentRef
	persistedFX.WorkID = ""
	req := buildContextRequest(name, persistedFX, built.Contract, built.Pack, mode, now)
	req.IntentTextRef = intentRef

	ev := newCapabilityEvent(name, persistedFX, fx.Harness, deps, contextHookInputHash(name, fx, mode), fx.AdapterSideEffects, now)
	ev.IDs.ContextPackID = built.Pack.Header.ID
	rendered := renderInjection(injectionRender{
		HookID:          ev.Header.ID,
		Pack:            built.Pack,
		Surface:         injectionSurfaceTask,
		OrientationNote: orientationNoteForSession(deps.HomePath, fx.SessionID, fx.Harness),
	})
	var injection *InjectionArtifact
	if strings.TrimSpace(deps.HomePath) != "" {
		if deps.BeforeContextPersist != nil {
			deps.BeforeContextPersist()
		}
		var err error
		injection, err = writeContextArtifacts(deps.HomePath, ev.Header.ID, req, built.Contract, built.Pack, rendered)
		if err != nil {
			return Result{}, err
		}
		ev.SideEffects = append(ev.SideEffects, injection.ContextRequestRef, injection.ContractRef, injection.ContextPackRef, injection.Ref)
		ev.OutputRef = injection.Ref
	}
	return Result{
		Event:         ev,
		Produced:      []envelope.Envelope{&req, &built.Contract, &built.Pack},
		Summary:       fmt.Sprintf("context hook=%s pack=%s chars=%d sha256:%s", ev.Header.ID, built.Pack.Header.ID, len([]rune(rendered)), hashStr(rendered)),
		ContextOutput: rendered,
		Injection:     injection,
		Context: &ContextObservation{
			Input:            built.Input,
			Pack:             built.Pack,
			RetrievalLatency: built.RetrievalLatency,
		},
	}, nil
}

func livePromptIntentRef(fx Fixture) string {
	intent := strings.TrimSpace(fx.Prompt)
	if intent == "" {
		intent = strings.TrimSpace(fx.Task)
	}
	if intent == "" {
		return ""
	}
	return "sha256:" + hashStr(intent)
}

func buildContextWithBreaker(name string, fx Fixture, deps Deps, mode string, now time.Time) (contextBuild, *Result, error) {
	budget := time.Duration(deps.Registry.InjectionBudgetMS(fx.Harness)) * time.Millisecond
	build := func() (contextBuild, error) {
		if deps.BeforeContextBuild != nil {
			deps.BeforeContextBuild()
		}
		in, err := orientationInput(fx, deps, mode)
		if err != nil {
			return contextBuild{}, err
		}
		in.ReadOnlyRetrieval = fx.Live && name == "UserPromptSubmit"
		retrievalStarted := hookNow(deps)
		var ec envelope.EffectiveContract
		var pack envelope.ContextPack
		if strings.TrimSpace(deps.HomePath) != "" {
			materialized, err := contract.MaterializeRuntime(in, deps.Orient, deps.HomePath, now)
			if err != nil {
				return contextBuild{}, err
			}
			materialized, err = contract.SealRuntimeMaterialization(materialized)
			if err != nil {
				return contextBuild{}, err
			}
			ec = materialized.EffectiveContract
			pack = materialized.ContextPack
		} else {
			ec = contract.Compile(in, deps.Orient, now)
			pack = contract.Pack(in, deps.Orient, now)
		}
		retrievalLatency := nonNegativeDuration(hookNow(deps).Sub(retrievalStarted))
		return contextBuild{Input: in, Contract: ec, Pack: pack, RetrievalLatency: retrievalLatency}, nil
	}
	if !fx.Live {
		built, err := build()
		return built, nil, err
	}

	started := hookNow(deps)
	done := make(chan contextBuildResult, 1)
	go func() {
		built, err := build()
		done <- contextBuildResult{Build: built, Err: err}
	}()
	select {
	case result := <-done:
		if result.Err != nil {
			breaker := contextBreakerResult(name, fx, deps, mode, now)
			return contextBuild{}, &breaker, nil
		}
		if budget > 0 && hookNow(deps).Sub(started) > budget {
			breaker := contextBreakerResult(name, fx, deps, mode, now)
			return contextBuild{}, &breaker, nil
		}
		return result.Build, nil, nil
	case <-hookAfter(deps, budget):
		breaker := contextBreakerResult(name, fx, deps, mode, now)
		return contextBuild{}, &breaker, nil
	}
}

func nonNegativeDuration(duration time.Duration) time.Duration {
	if duration < 0 {
		return 0
	}
	return duration
}

func contextBreakerResult(name string, fx Fixture, deps Deps, mode string, now time.Time) Result {
	ev := newEvent(name, fx, "fail_open", contextHookInputHash(name, fx, mode), fx.AdapterSideEffects, now)
	// Name the meaning ON THE EVENT. BreakerTripped below says the same thing, but it
	// lives on the Result and dies with the process: every consumer that reads the
	// persisted event back — event store, projections, an operator audit — saw a bare
	// `fail_open` and could not tell "the model received NO context" from "this harness
	// has no native hook". Pinned by
	// TestFailOpenDistinguishesContextFailureFromAbsentCapability.
	ev.FailOpenReason = envelope.FailOpenContextUnavailable
	return Result{Event: ev, Produced: []envelope.Envelope{}, Summary: "", BreakerTripped: true}
}

func hookNow(deps Deps) time.Time {
	if deps.Now != nil {
		return deps.Now()
	}
	return time.Now()
}

func hookAfter(deps Deps, d time.Duration) <-chan time.Time {
	if deps.After != nil {
		return deps.After(d)
	}
	if d <= 0 {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	return time.After(d)
}

func orientationInput(fx Fixture, deps Deps, mode string) (contract.Input, error) {
	kind, via, err := contract.ResolveRepoKindWithHome(deps.HomePath, fx.RepoKind, fx.Cwd)
	if err != nil {
		return contract.Input{}, err
	}
	return contract.Input{Harness: fx.Harness, Cwd: fx.Cwd, Task: fx.Task, RepoKind: kind, RepoKindVia: via, Mode: mode, Topic: fx.Topic}, nil
}

func preToolUse(name string, fx Fixture, deps Deps, now time.Time) (Result, error) {
	var (
		res Result
		err error
	)
	if len(fx.Actions) > 0 {
		res, err = preToolUseBatch(name, fx, deps, now)
	} else {
		if fx.Action == nil {
			return Result{}, fmt.Errorf("pre-tool-use fixture needs an action")
		}
		prepared, prepareErr := preparePreToolUseAction(fx, deps, *fx.Action, now)
		if prepareErr != nil {
			return Result{}, prepareErr
		}
		// An unparsed mutation is not an exact action: there is no bounded target
		// for an operator to approve, so its fail-closed hold cannot mint a plan.
		prepared.allowApprovalPlan = prepared.action.Action != "unparsed_apply_patch"
		reviewed, reviewErr := reviewedSourceControlMutationForBatch(deps, []preparedPreToolAction{prepared})
		if reviewErr != nil {
			return Result{}, reviewErr
		}
		res, err = evaluatePreparedPreToolUse(name, fx, deps, prepared, reviewed, now)
	}
	if err != nil {
		return Result{}, err
	}
	if res.pendingExactApproval != nil {
		return res, nil
	}
	if effect, recordErr := recordPendingSessionMutation(deps.HomePath, fx, res, now); recordErr != nil {
		return Result{}, recordErr
	} else if effect != "" {
		res.Event.SideEffects = append(res.Event.SideEffects, effect)
	}
	return res, nil
}

type preparedPreToolAction struct {
	action        envelope.ActionIntent
	placement     *policy.PlacementGuardContext
	sourceControl *policy.SourceControlContext
	targetGitRoot string
	sourceEffects []string
	// Only the direct single-action path opts in. Batch children deliberately
	// remain false after Actions is stripped for evaluation.
	allowApprovalPlan bool
}

// preparePreToolUseAction resolves every read-only input before policy runs.
// A batch prepares all actions first, then either attaches one exact reviewed
// capability to every member or to none; policy never observes a partial match.
func preparePreToolUseAction(fx Fixture, deps Deps, action envelope.ActionIntent, now time.Time) (preparedPreToolAction, error) {
	ai := action
	if fx.WorkID != "" {
		ai.IDs.WorkID = fx.WorkID
	}
	ai = normalizeActionTarget(fx, ai)
	var err error
	ai, err = guardActualPermissionTarget(fx, deps, ai)
	if err != nil {
		return preparedPreToolAction{}, err
	}
	prepared := preparedPreToolAction{action: ai}
	if fx.Placement != nil {
		prepared.placement = fx.Placement
	} else if deps.HomePath != "" && strings.TrimSpace(ai.Target.Path) != "" {
		placement, effects, err := policy.PlacementContextFromHome(deps.HomePath, ai.Target.Path)
		if err == nil && placement != nil {
			prepared.placement = placement
		}
		if err == nil {
			prepared.sourceEffects = append(prepared.sourceEffects, effects...)
		}
	}
	if deps.HomePath != "" && strings.TrimSpace(ai.Target.Path) != "" {
		sourceControl, targetGitRoot, effects, err := sourceControlContextForAction(deps.HomePath, ai.Target.Path, fx.SessionID, ai.Actor, fx.Live, now)
		if err != nil {
			return preparedPreToolAction{}, err
		}
		if sourceControl != nil {
			canonical, canonicalErr := authority.CanonicalSourceControlMutationTarget(sourceControl.GitRoot, ai.Target.Path)
			if canonicalErr != nil {
				return preparedPreToolAction{}, canonicalErr
			}
			ai.Target.Path = canonical
			prepared.action = ai
			prepared.sourceControl = sourceControl
			prepared.targetGitRoot = targetGitRoot
			prepared.sourceEffects = append(prepared.sourceEffects, effects...)
		}
	}
	return prepared, nil
}

func evaluatePreparedPreToolUse(name string, fx Fixture, deps Deps, prepared preparedPreToolAction, reviewed *policy.ReviewedSourceControlMutation, now time.Time) (Result, error) {
	ai := prepared.action
	evalCtx := policy.EvaluationContext{Placement: prepared.placement}
	var sourceControl *policy.SourceControlContext
	if prepared.sourceControl != nil {
		copy := *prepared.sourceControl
		copy.ReviewedMutation = reviewed
		sourceControl = &copy
		evalCtx.SourceControl = sourceControl
	}
	sideEffects := mergeRefs(fx.AdapterSideEffects, prepared.sourceEffects)
	pd := policy.EvaluateWithContext(ai, deps.Policy, evalCtx, now)
	if deps.HomePath != "" {
		actionRef, err := writeGateActionArtifact(deps.HomePath, ai)
		if err != nil {
			return Result{}, err
		}
		sideEffects = append(sideEffects, actionRef)
	}
	deferExactApproval := fx.CoordinationDelivery != nil && prepared.allowApprovalPlan
	if !deferExactApproval {
		var approvalEffects []string
		pd, approvalEffects = applyExactApprovalEdge(fx, deps, prepared, ai, pd, now)
		sideEffects = mergeRefs(sideEffects, approvalEffects)
	}
	if deps.HomePath != "" && !deferExactApproval {
		decisionRef, err := writeGateDecisionArtifact(deps.HomePath, pd)
		if err != nil {
			return Result{}, err
		}
		sideEffects = append(sideEffects, decisionRef)
	}
	reviewWorkID := ""
	if deps.Store != nil && sourceControl != nil && !policy.Allowed(pd) && strings.TrimSpace(sourceControl.Status) == "dirty" {
		mint, err := shouldMintGateReview(deps.Store, deps.HomePath, *sourceControl, prepared.targetGitRoot)
		if err != nil {
			return Result{}, err
		}
		if mint {
			review, err := createHeldGateReview(deps.Store, fx, ai, pd, *sourceControl, now)
			if err != nil {
				return Result{}, err
			}
			reviewWorkID = review.ID
			sideEffects = append(sideEffects, filepath.Join("work", "work", review.ID+".json"))
		}
	}

	// Capability-aware enforcement: a block only counts if the harness can actually block.
	st, warn := "ok", ""
	if !policy.Allowed(pd) {
		if deps.Registry.Supports(fx.Harness, name) {
			st = "blocked"
		} else {
			st = "fail_open"
			warn = fmt.Sprintf(" — WARNING: %s cannot block natively; enforce via wrapper/adapter", fx.Harness)
		}
	}
	ev := newEvent(name, fx, st, hashStr(ai.ID), sideEffects, now)
	if st == "fail_open" {
		ev.FailOpenReason = envelope.FailOpenCapabilityAbsent
	}
	ev.IDs.WorkID = ai.IDs.WorkID
	summary := fmt.Sprintf("policy %s for %s%s", pd.Outcome, ai.Action, warn)
	if reviewWorkID != "" {
		summary += "; held review work " + reviewWorkID
	}
	res := Result{Event: ev, Produced: []envelope.Envelope{&ai, &pd}, Summary: summary}
	if deferExactApproval {
		res.pendingExactApproval = &pendingExactApproval{
			fixture: fx, prepared: prepared, action: ai, decision: pd,
		}
	}
	return res, nil
}

func applyExactApprovalEdge(
	fx Fixture,
	deps Deps,
	prepared preparedPreToolAction,
	ai envelope.ActionIntent,
	pd envelope.PolicyDecision,
	now time.Time,
) (envelope.PolicyDecision, []string) {
	if !exactApprovalPlanEligible(fx, deps, prepared, ai, pd, now) {
		return pd, nil
	}
	sideEffects := []string{}
	approved, matched, approvalErr := policy.NewExactActionApprovalStore(deps.HomePath).
		ApplyMatching(ai, deps.Policy, pd, now)
	switch {
	case matched && approvalErr == nil:
		pd = approved
		sideEffects = mergeRefs(sideEffects, approved.EvidenceRefs)
	case approvalErr != nil && errors.Is(approvalErr, policy.ErrExactApprovalExhausted):
		// A valid generation was already consumed or expired. Rotating to the
		// next plan is the expected one-use lifecycle, not an authority failure.
	case approvalErr != nil:
		// Malformed records and store failures remain fail-closed. Keep the
		// adapter-facing reason bounded and never mint through uncertain authority.
		pd.Reasons = append(pd.Reasons, "exact approval authority unavailable for operator review")
		return pd, sideEffects
	}
	if pd.Outcome != "hold" {
		return pd, sideEffects
	}
	plan, planRef, _, planErr := policy.NewExactActionApprovalStore(deps.HomePath).
		EnsurePlanForIntent(ai, deps.Policy, 5*time.Minute, now)
	if planErr != nil {
		// The original hold remains authoritative. Keep the adapter-facing
		// failure bounded and command-free instead of turning policy friction
		// into a hook runtime failure.
		pd.Reasons = append(pd.Reasons, "exact approval plan unavailable for operator review")
		return pd, sideEffects
	}
	pd.EvidenceRefs = mergeRefs(pd.EvidenceRefs, []string{planRef})
	pd.Reasons = append(pd.Reasons,
		fmt.Sprintf("Exact approval plan %s is ready for operator review.", plan.ID))
	sideEffects = mergeRefs(sideEffects, []string{planRef})

	// When compound home is configured, mint a durable action-approval request
	// in the operator-approval-frontier registry so the agent-visible message
	// can carry a stable id. The original hold remains authoritative: a
	// frontier registry failure must never break the gate, only reduce the
	// legibility of the agent-visible reason.
	if strings.TrimSpace(deps.HomePath) != "" {
		request, mintErr := policy.MintOperatorActionApprovalRequest(
			deps.HomePath,
			ai.ID,
			fx.Harness,
			ai.Action,
			ai.Capability,
			ai.Target.Path,
			now,
		)
		if mintErr != nil {
			pd.Reasons = append(pd.Reasons, "operator-approval-frontier registry unavailable for operator review")
		} else {
			frontierRef := "compound://registries/operator-approval-frontier#" + request.ID
			pd.EvidenceRefs = mergeRefs(pd.EvidenceRefs, []string{frontierRef})
			pd.Reasons = append(pd.Reasons,
				fmt.Sprintf("Operator approval request %s filed in compound://registries/operator-approval-frontier; agent may wait, take an alternative, or record blocked against this id",
					request.ID))
			sideEffects = mergeRefs(sideEffects, []string{frontierRef})
		}
	}
	return pd, sideEffects
}

func finalizePendingExactApproval(res *Result, deps Deps, delivery hookplan.DeliveryReport, now time.Time) error {
	if res == nil || res.pendingExactApproval == nil {
		return nil
	}
	if delivery.ExecutionHeld {
		return fmt.Errorf("cannot finalize exact approval while coordination execution is held")
	}
	pending := res.pendingExactApproval
	pd, approvalEffects := applyExactApprovalEdge(
		pending.fixture,
		deps,
		pending.prepared,
		pending.action,
		pending.decision,
		now,
	)
	if strings.TrimSpace(deps.HomePath) != "" {
		decisionRef, err := writeGateDecisionArtifact(deps.HomePath, pd)
		if err != nil {
			return err
		}
		approvalEffects = mergeRefs(approvalEffects, []string{decisionRef})
	}
	if err := replacePreToolPolicyDecision(res, pd); err != nil {
		return err
	}
	res.Event.SideEffects = mergeRefs(res.Event.SideEffects, approvalEffects)
	if pd.Outcome != pending.decision.Outcome {
		status, warning := preToolPolicyStatus(deps, pending.fixture, pd)
		res.Event.Status = status
		res.Event.FailOpenReason = ""
		if status == "fail_open" {
			res.Event.FailOpenReason = envelope.FailOpenCapabilityAbsent
		}
		res.Summary = fmt.Sprintf("policy %s for %s%s", pd.Outcome, pending.action.Action, warning)
	}
	res.pendingExactApproval = nil
	if effect, err := recordPendingSessionMutation(deps.HomePath, pending.fixture, *res, now); err != nil {
		return err
	} else if effect != "" {
		res.Event.SideEffects = mergeRefs(res.Event.SideEffects, []string{effect})
	}
	return nil
}

func replacePreToolPolicyDecision(res *Result, decision envelope.PolicyDecision) error {
	if res == nil {
		return fmt.Errorf("replace PreToolUse PolicyDecision requires result")
	}
	for i, produced := range res.Produced {
		if _, ok := produced.(*envelope.PolicyDecision); ok {
			res.Produced[i] = &decision
			return nil
		}
	}
	return fmt.Errorf("PreToolUse result has no PolicyDecision")
}

func preToolPolicyStatus(deps Deps, fx Fixture, pd envelope.PolicyDecision) (string, string) {
	if policy.Allowed(pd) {
		return "ok", ""
	}
	if deps.Registry.Supports(fx.Harness, "PreToolUse") {
		return "blocked", ""
	}
	return "fail_open", fmt.Sprintf(" — WARNING: %s cannot block natively; enforce via wrapper/adapter", fx.Harness)
}

func exactApprovalPlanEligible(fx Fixture, deps Deps, prepared preparedPreToolAction, ai envelope.ActionIntent, pd envelope.PolicyDecision, now time.Time) bool {
	if !fx.Enforced || !prepared.allowApprovalPlan || strings.TrimSpace(deps.HomePath) == "" ||
		pd.Outcome != "hold" || strings.TrimSpace(ai.SecurityFloorOutcome) != "" || prepared.placement != nil {
		return false
	}
	if prepared.sourceControl != nil && strings.TrimSpace(prepared.sourceControl.Status) == "dirty" {
		return false
	}
	// A context-raised hold (placement/source-control) is not the same approval
	// class as the baseline action hold. V1 exposes plans only when baseline
	// policy independently reaches the same hold.
	return policy.Evaluate(ai, deps.Policy, now).Outcome == "hold"
}

func preToolUseBatch(name string, fx Fixture, deps Deps, now time.Time) (Result, error) {
	produced := make([]envelope.Envelope, 0, len(fx.Actions)*2)
	sideEffects := append([]string(nil), fx.AdapterSideEffects...)
	actionIDs := make([]string, 0, len(fx.Actions))
	status := "ok"
	failOpenReason := ""
	summary := ""
	workID := fx.WorkID
	prepared := make([]preparedPreToolAction, 0, len(fx.Actions))

	for i := range fx.Actions {
		child := fx
		child.Actions = nil
		item, err := preparePreToolUseAction(child, deps, fx.Actions[i], now)
		if err != nil {
			return Result{}, err
		}
		prepared = append(prepared, item)
	}
	reviewed, err := reviewedSourceControlMutationForBatch(deps, prepared)
	if err != nil {
		return Result{}, err
	}

	for i := range prepared {
		child := fx
		child.Actions = nil
		child.Action = &prepared[i].action
		res, err := evaluatePreparedPreToolUse(name, child, deps, prepared[i], reviewed, now)
		if err != nil {
			return Result{}, err
		}
		produced = append(produced, res.Produced...)
		sideEffects = append(sideEffects, res.Event.SideEffects...)
		actionIDs = append(actionIDs, prepared[i].action.ID)
		if workID == "" {
			workID = prepared[i].action.IDs.WorkID
		}
		if hookStatusRank(res.Event.Status) > hookStatusRank(status) {
			status = res.Event.Status
			// Carry the reason with the status it belongs to. Promoting a child's
			// status without its reason would rebuild the exact ambiguity this field
			// exists to remove: the batch event would say `fail_open` while the WHY
			// stayed behind on the child.
			failOpenReason = res.Event.FailOpenReason
			summary = res.Summary
		} else if summary == "" {
			summary = res.Summary
		}
	}

	ev := newEvent(name, fx, status, hashStr(strings.Join(actionIDs, "\x00")), uniqueStrings(sideEffects), now)
	ev.FailOpenReason = failOpenReason
	ev.IDs.WorkID = workID
	return Result{
		Event:    ev,
		Produced: produced,
		Summary:  fmt.Sprintf("%s; evaluated %d target(s)", summary, len(fx.Actions)),
	}, nil
}

func reviewedSourceControlMutationForBatch(deps Deps, prepared []preparedPreToolAction) (*policy.ReviewedSourceControlMutation, error) {
	if strings.TrimSpace(deps.HomePath) == "" || deps.Store == nil || len(prepared) == 0 {
		return nil, nil
	}
	snapshot, binding, ok := sourceControlMutationBatchBinding(prepared)
	if !ok {
		return nil, nil
	}
	reports, err := authority.ListRecordedSourceControlMutationReviews(deps.HomePath, snapshot, binding)
	if err != nil {
		return nil, err
	}
	for i := range reports {
		report := &reports[i]
		if report.MutationBinding == nil {
			continue
		}
		owner, ownerErr := deps.Store.Get(report.WorkID)
		if ownerErr != nil || !currentReviewedMutationOwner(report.WorkID, owner.State) {
			continue
		}
		recorded := report.MutationBinding
		return &policy.ReviewedSourceControlMutation{
			Status:                  report.Status,
			Decision:                report.Decision,
			ReviewID:                report.ReviewID,
			PlanID:                  report.PlanID,
			PlanDigest:              report.PlanDigest,
			AuthorizationDigest:     report.AuthorizationDigest,
			ReviewedBy:              report.ReviewedBy,
			OwnerWorkID:             report.WorkID,
			ActionWorkID:            recorded.ActionWorkID,
			RepoID:                  report.RepoID,
			GitRoot:                 report.GitRoot,
			Remote:                  report.Remote,
			Head:                    report.Head,
			ChangedStateFingerprint: report.ChangedStateFingerprint,
			MutationInputHash:       recorded.MutationInputHash,
			Capability:              recorded.Capability,
			Action:                  recorded.Action,
			MutationClass:           recorded.MutationClass,
			TargetPaths:             append([]string{}, recorded.TargetPaths...),
			EvidenceRefs:            append([]string{}, report.EvidenceRefs...),
		}, nil
	}
	return nil, nil
}

func sourceControlMutationBatchBinding(prepared []preparedPreToolAction) (authority.SourceControlSnapshot, authority.SourceControlMutationBinding, bool) {
	first := prepared[0]
	if first.sourceControl == nil || strings.TrimSpace(first.sourceControl.Status) != "dirty" {
		return authority.SourceControlSnapshot{}, authority.SourceControlMutationBinding{}, false
	}
	inputHash, ok := boundMutationInputHash(first.action)
	if !ok {
		return authority.SourceControlSnapshot{}, authority.SourceControlMutationBinding{}, false
	}
	binding := authority.SourceControlMutationBinding{
		ActionWorkID:      strings.TrimSpace(first.action.IDs.WorkID),
		MutationInputHash: inputHash,
		Capability:        strings.TrimSpace(first.action.Capability),
		Action:            strings.TrimSpace(first.action.Action),
		MutationClass:     strings.TrimSpace(first.action.MutationClass),
	}
	for _, item := range prepared {
		if item.sourceControl == nil || !sameSourceControlIdentity(*first.sourceControl, *item.sourceControl) {
			return authority.SourceControlSnapshot{}, authority.SourceControlMutationBinding{}, false
		}
		itemHash, hashOK := boundMutationInputHash(item.action)
		if !hashOK || itemHash != binding.MutationInputHash || strings.TrimSpace(item.action.IDs.WorkID) != binding.ActionWorkID ||
			strings.TrimSpace(item.action.Capability) != binding.Capability || strings.TrimSpace(item.action.Action) != binding.Action ||
			strings.TrimSpace(item.action.MutationClass) != binding.MutationClass {
			return authority.SourceControlSnapshot{}, authority.SourceControlMutationBinding{}, false
		}
		if strings.TrimSpace(item.action.ID) == "" || strings.TrimSpace(item.action.Target.Path) == "" {
			return authority.SourceControlSnapshot{}, authority.SourceControlMutationBinding{}, false
		}
		binding.TargetPaths = append(binding.TargetPaths, item.action.Target.Path)
		binding.ActionIntentIDs = append(binding.ActionIntentIDs, item.action.ID)
	}
	sc := first.sourceControl
	if sc.Staged < 0 || sc.Unstaged < 0 || sc.Untracked < 0 || sc.ChangedPaths <= 0 ||
		sc.ChangedPaths != sc.Staged+sc.Unstaged+sc.Untracked {
		return authority.SourceControlSnapshot{}, authority.SourceControlMutationBinding{}, false
	}
	return authority.SourceControlSnapshot{
		SchemaVersion:           authority.SourceControlSchemaVersion,
		RepoID:                  sc.EntityRef,
		GitRoot:                 sc.GitRoot,
		Remote:                  sc.Remote,
		Head:                    sc.Head,
		Status:                  sc.Status,
		Staged:                  sc.Staged,
		Unstaged:                sc.Unstaged,
		Untracked:               sc.Untracked,
		ChangedPaths:            sc.ChangedPaths,
		ChangedStateFingerprint: sc.ChangedStateFingerprint,
	}, binding, true
}

func sameSourceControlIdentity(a, b policy.SourceControlContext) bool {
	return a.EntityRef == b.EntityRef && a.GitRoot == b.GitRoot && a.Remote == b.Remote && a.Head == b.Head &&
		a.Status == b.Status && a.Staged == b.Staged && a.Unstaged == b.Unstaged && a.Untracked == b.Untracked &&
		a.ChangedPaths == b.ChangedPaths && a.ChangedStateFingerprint == b.ChangedStateFingerprint
}

func boundMutationInputHash(ai envelope.ActionIntent) (string, bool) {
	value := ""
	for _, ref := range ai.EvidenceRefs {
		if !strings.HasPrefix(ref, "mutation-input:sha256:") {
			continue
		}
		if value != "" && value != ref {
			return "", false
		}
		value = ref
	}
	return value, value != ""
}

func currentReviewedMutationOwner(workID, state string) bool {
	return authority.SourceControlMutationOwnerCurrent(workID, state)
}

func hookStatusRank(status string) int {
	switch status {
	case "blocked":
		return 2
	case "fail_open":
		return 1
	default:
		return 0
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeActionTarget(fx Fixture, ai envelope.ActionIntent) envelope.ActionIntent {
	if normalized := actionTargetPath(fx, ai); normalized != "" {
		ai.Target.Path = normalized
	}
	return ai
}

func actionTargetPath(fx Fixture, ai envelope.ActionIntent) string {
	targetPath := strings.TrimSpace(ai.Target.Path)
	if targetPath == "" || filepath.IsAbs(targetPath) {
		return targetPath
	}
	cwd := strings.TrimSpace(fx.Cwd)
	if cwd == "" {
		return targetPath
	}
	return filepath.Join(cwd, targetPath)
}

func createHeldGateReview(store *work.Store, fx Fixture, ai envelope.ActionIntent, pd envelope.PolicyDecision, sc policy.SourceControlContext, now time.Time) (envelope.WorkEnvelope, error) {
	target := strings.TrimSpace(ai.Target.Path)
	if target == "" {
		target = ai.Target.ID
	}
	actionRef, err := eventstore.ReferenceForID(ai.ID)
	if err != nil {
		return envelope.WorkEnvelope{}, err
	}
	decisionRef, err := eventstore.ReferenceForID(pd.ID)
	if err != nil {
		return envelope.WorkEnvelope{}, err
	}
	predicate := fmt.Sprintf("review held policy gate for %s on %s", ai.Action, target)
	return store.Create(work.CreateInput{
		Owner:            fx.Harness,
		State:            "held",
		Risk:             orDef(ai.Risk, "tier1"),
		Privacy:          orDef(ai.Privacy, "private"),
		SuccessPredicate: predicate,
		DesiredOutcome:   "decide whether to clean the repo, revise the action, or explicitly review the dirty source-control state before mutation",
		NonGoals: []string{
			"do not mutate the target before policy allows it",
			"do not treat git status as source truth for meaning",
		},
		EvidenceRefs: mergeRefs([]string{actionRef.String(), decisionRef.String()}, pd.EvidenceRefs, sc.EvidenceRefs),
	}, now)
}

func mergeRefs(groups ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, group := range groups {
		for _, ref := range group {
			ref = strings.TrimSpace(ref)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}

func writeGateArtifacts(homePath string, ai envelope.ActionIntent, pd envelope.PolicyDecision) ([]string, error) {
	actionRef, err := writeGateActionArtifact(homePath, ai)
	if err != nil {
		return nil, err
	}
	decisionRef, err := writeGateDecisionArtifact(homePath, pd)
	if err != nil {
		return nil, err
	}
	return []string{actionRef, decisionRef}, nil
}

func writeGateActionArtifact(homePath string, ai envelope.ActionIntent) (string, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return "", err
	}
	action, err := store.PutJSON(ai.ID, ai, "  ")
	if err != nil {
		return "", err
	}
	return action.Reference.String(), nil
}

func writeGateDecisionArtifact(homePath string, pd envelope.PolicyDecision) (string, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return "", err
	}
	decision, err := store.PutJSON(pd.ID, pd, "  ")
	if err != nil {
		return "", err
	}
	return decision.Reference.String(), nil
}

func sourceControlContextForAction(homePath, targetPath, sessionID, actor string, live bool, now time.Time) (*policy.SourceControlContext, string, []string, error) {
	snapshot, err := authority.ReadGitSnapshot(targetPath)
	if err != nil {
		return nil, "", nil, nil
	}
	ownedChangedPaths, foreignChangedPaths := 0, 0
	sessionOwnsTarget := false
	claimedWorkID := ""
	if live && snapshot.Status == "dirty" {
		changedPaths, changedErr := readSourceControlChangedPaths(snapshot.GitRoot)
		if changedErr != nil {
			return nil, "", nil, changedErr
		}
		ownedPaths, ownedErr := readSessionWrittenPaths(homePath, sessionID)
		if ownedErr != nil {
			return nil, "", nil, ownedErr
		}
		owned := make(map[string]struct{}, len(ownedPaths))
		for _, path := range ownedPaths {
			owned[filepath.Clean(path)] = struct{}{}
		}
		for _, path := range changedPaths {
			if _, ok := owned[filepath.Clean(path)]; ok {
				ownedChangedPaths++
				if filepath.Clean(path) == filepath.Clean(snapshot.TargetPath) {
					sessionOwnsTarget = true
				}
			} else {
				foreignChangedPaths++
			}
		}
		if len(changedPaths) == 0 && snapshot.ChangedPaths > 0 {
			foreignChangedPaths = snapshot.ChangedPaths
		}
		claimedWorkID, err = claimedWorktreeOwner(homePath, snapshot.GitRoot, actor)
		if err != nil {
			return nil, "", nil, err
		}
	}
	evidenceRefs := []string{snapshot.EvidenceReference()}
	if claimedWorkID != "" {
		evidenceRefs = append(evidenceRefs, "work:"+claimedWorkID)
	}
	return &policy.SourceControlContext{
		EntityRef:                  snapshot.RepoID,
		GitRoot:                    snapshot.GitRoot,
		Remote:                     snapshot.Remote,
		Head:                       snapshot.Head,
		Status:                     snapshot.Status,
		Staged:                     snapshot.Staged,
		Unstaged:                   snapshot.Unstaged,
		Untracked:                  snapshot.Untracked,
		ChangedPaths:               snapshot.ChangedPaths,
		TargetPath:                 snapshot.TargetPath,
		TargetStatus:               snapshot.TargetStatus,
		SessionOwnershipKnown:      live,
		SessionOwnedChangedPaths:   ownedChangedPaths,
		SessionForeignChangedPaths: foreignChangedPaths,
		SessionOwnsTarget:          sessionOwnsTarget,
		ClaimedWorkID:              claimedWorkID,
		ChangedStateFingerprint:    snapshot.ChangedStateFingerprint,
		EvidenceRefs:               evidenceRefs,
	}, snapshot.GitRoot, evidenceRefs, nil
}

func claimedWorktreeOwner(homePath, gitRoot, actor string) (string, error) {
	actor = strings.TrimSpace(actor)
	if strings.TrimSpace(homePath) == "" || strings.TrimSpace(gitRoot) == "" || actor == "" {
		return "", nil
	}
	branchRef := currentWorktreeBranchRef(gitRoot)
	if branchRef == "" {
		return "", nil
	}
	store, err := work.Open(homePath)
	if err != nil {
		return "", err
	}
	items, err := store.List()
	if err != nil {
		return "", err
	}
	leases, err := store.ListLeases()
	if err != nil {
		return "", err
	}
	holders := make(map[string]string, len(leases))
	for _, lease := range leases {
		holders[lease.WorkID] = strings.TrimSpace(lease.By)
	}
	matches := make([]string, 0, 1)
	for _, item := range items {
		if !claimedWorktreeState(item.State) || item.GitDelivery == nil {
			continue
		}
		holder := holders[item.ID]
		// A governed steal transfers the live lease while preserving the envelope's
		// historical authority owner. The current lease is therefore the executable
		// ownership proof; requiring it to equal Authority.Owner would reject valid
		// salvaged/taken-over worktrees.
		if holder == "" || !actorOwnsClaim(actor, holder) {
			continue
		}
		direct := filepath.Clean(item.GitDelivery.TargetWorktreeRoot) == filepath.Clean(gitRoot) &&
			strings.TrimSpace(item.GitDelivery.TargetLocalRef) == branchRef
		branched := containsWorktreeBranchArtifact(item.ArtifactIDs, branchRef)
		if !direct && !branched {
			continue
		}
		identity, identityErr := gitrepository.ReadIdentity(gitRoot, item.GitDelivery.ExpectedBaseSHA)
		if identityErr != nil || identity.Fingerprint != item.GitDelivery.RepositoryFingerprint {
			continue
		}
		matches = append(matches, item.ID)
		if len(matches) > 1 {
			return "", nil
		}
	}
	if len(matches) != 1 {
		return "", nil
	}
	return matches[0], nil
}

func currentWorktreeBranchRef(gitRoot string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "git", "-C", gitRoot, "symbolic-ref", "--quiet", "HEAD").Output()
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(output))
	if !strings.HasPrefix(ref, "refs/heads/") || strings.TrimSpace(strings.TrimPrefix(ref, "refs/heads/")) == "" {
		return ""
	}
	return ref
}

func claimedWorktreeState(state string) bool {
	switch strings.TrimSpace(state) {
	case "claimed", "running", "verifying":
		return true
	default:
		return false
	}
}

func actorOwnsClaim(actor, holder string) bool {
	actor = strings.TrimSpace(actor)
	holder = strings.TrimSpace(holder)
	return actor != "" && (holder == actor || strings.HasPrefix(holder, actor+":"))
}

func containsWorktreeBranchArtifact(artifacts []string, branchRef string) bool {
	want := "worktree-branch:" + strings.TrimSpace(branchRef)
	for _, artifact := range artifacts {
		if strings.TrimSpace(artifact) == want {
			return true
		}
	}
	return false
}

const sessionMutationStateSchemaVersion = "compound.hook_session_mutation.v0.1"

type sessionMutationState struct {
	SchemaVersion    string   `json:"schema_version"`
	SessionKey       string   `json:"session_key"`
	MutationInputRef string   `json:"mutation_input_ref"`
	WorkID           string   `json:"work_id,omitempty"`
	Paths            []string `json:"paths"`
	CreatedAt        string   `json:"created_at"`
}

// The live gate keeps an immutable per-session written-path set instead of scanning
// action-event segments on every PreToolUse. An allowing gate writes only a pending
// exact-input record; the matching successful PostToolUse promotes it to written.
// This keeps the hot path bounded and prevents failed or held attempts from claiming
// ownership. The tradeoff is path-granular attribution: any current dirty path absent
// from the session set is foreign, so mixed repositories continue to hold.
func recordPendingSessionMutation(homePath string, fx Fixture, res Result, now time.Time) (string, error) {
	if strings.TrimSpace(homePath) == "" || !fx.Live || strings.TrimSpace(fx.SessionID) == "" {
		return "", nil
	}
	actions := make([]envelope.ActionIntent, 0)
	decisions := 0
	for _, produced := range res.Produced {
		switch value := produced.(type) {
		case *envelope.ActionIntent:
			if value != nil && value.MutationClass != "read" {
				actions = append(actions, *value)
			}
		case *envelope.PolicyDecision:
			decisions++
			if value == nil || !policy.Allowed(*value) {
				return "", nil
			}
		}
	}
	if len(actions) == 0 {
		return "", nil
	}
	if decisions != len(actions) {
		return "", fmt.Errorf("record session mutation: action/policy count mismatch %d/%d", len(actions), decisions)
	}
	ref := ""
	workID := ""
	paths := make([]string, 0, len(actions))
	for _, action := range actions {
		actionRef := boundMutationInputRef(action.EvidenceRefs)
		if actionRef == "" {
			return "", nil
		}
		if ref != "" && actionRef != ref {
			return "", fmt.Errorf("record session mutation: batch mutation input mismatch")
		}
		ref = actionRef
		if workID == "" {
			workID = action.IDs.WorkID
		}
		paths = append(paths, filepath.Clean(action.Target.Path))
	}
	paths = uniqueSortedPaths(paths)
	record := sessionMutationState{
		SchemaVersion:    sessionMutationStateSchemaVersion,
		SessionKey:       hashStr(strings.TrimSpace(fx.SessionID)),
		MutationInputRef: ref,
		WorkID:           workID,
		Paths:            paths,
		CreatedAt:        now.UTC().Format(time.RFC3339Nano),
	}
	relative := sessionMutationRecordPath(fx.SessionID, "pending", ref)
	if err := writeSessionMutationState(filepath.Join(homePath, relative), record); err != nil {
		return "", fmt.Errorf("record pending session mutation: %w", err)
	}
	return relative, nil
}

func promotePendingSessionMutation(homePath string, fx Fixture) (string, error) {
	if strings.TrimSpace(homePath) == "" || !fx.Live || strings.TrimSpace(fx.SessionID) == "" || strings.TrimSpace(fx.MutationInputRef) == "" {
		return "", nil
	}
	pendingRelative := sessionMutationRecordPath(fx.SessionID, "pending", fx.MutationInputRef)
	pendingPath := filepath.Join(homePath, pendingRelative)
	body, err := os.ReadFile(pendingPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read pending session mutation: %w", err)
	}
	var record sessionMutationState
	if err := json.Unmarshal(body, &record); err != nil {
		return "", fmt.Errorf("parse pending session mutation: %w", err)
	}
	if err := validateSessionMutationState(record, fx.SessionID, fx.MutationInputRef); err != nil {
		return "", err
	}
	writtenRelative := sessionMutationRecordPath(fx.SessionID, "written", fx.MutationInputRef)
	writtenPath := filepath.Join(homePath, writtenRelative)
	if err := os.MkdirAll(filepath.Dir(writtenPath), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(pendingPath, writtenPath); err != nil {
		return "", fmt.Errorf("promote pending session mutation: %w", err)
	}
	return writtenRelative, nil
}

// sessionOrientationRecord marks that a session's orientation surface was served.
//
// The session id is HASHED into the path, matching hook-session-writes: a session
// id is operator-side state and this projection is not the place to hold it in
// clear.
type sessionOrientationRecord struct {
	SchemaVersion string `json:"schema_version"`
	SessionKey    string `json:"session_key"`
	HookID        string `json:"hook_id"`
	ServedAt      string `json:"served_at"`
}

const sessionOrientationSchemaVersion = "compound.hook_session_orientation.v0.1"

func sessionOrientationPath(homePath, sessionID string) string {
	return filepath.Join(homePath, "state", "runs", "hook-session-orientation", hashStr(strings.TrimSpace(sessionID))+".json")
}

// recordSessionOrientationServed persists the marker the task surface reads.
//
// NON-FATAL by design, like the instruction pack: failing to record that
// orientation was served must never block serving it. A missing marker degrades the
// pointer to its "cannot tell" wording, which is the honest fallback.
func recordSessionOrientationServed(homePath, sessionID, hookID string, now time.Time) error {
	if strings.TrimSpace(homePath) == "" || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	path := sessionOrientationPath(homePath, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(sessionOrientationRecord{
		SchemaVersion: sessionOrientationSchemaVersion,
		SessionKey:    hashStr(strings.TrimSpace(sessionID)),
		HookID:        strings.TrimSpace(hookID),
		ServedAt:      now.UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// orientationNoteForSession renders the task surface's pointer at the orientation
// surface, in one of THREE states — the third is the point.
//
// A two-state pointer (served / not served) has to guess when it cannot observe the
// session at all: with no home or no session id there is no marker to find, and
// reporting "NOT served" there would manufacture an alarm from missing plumbing.
// A false "orientation missing" is as corrosive as the silent gap the pointer
// exists to prevent, so unprovable is its own state and says only what is true.
func orientationNoteForSession(homePath, sessionID, harnessName string) string {
	reserve := strings.TrimSpace(harnessName)
	if reserve == "" {
		reserve = "<harness>"
	}
	command := fmt.Sprintf("`compound orient --harness %s --task \"<task>\"`", reserve)

	if strings.TrimSpace(homePath) == "" || strings.TrimSpace(sessionID) == "" {
		return "- Orientation (scope, sources, hooks, constitution) is served on SessionStart; re-serve with " + command
	}
	body, err := os.ReadFile(sessionOrientationPath(homePath, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return "- Orientation NOT served this session (no SessionStart record): run " + command
		}
		return "- Orientation (scope, sources, hooks, constitution) is served on SessionStart; re-serve with " + command
	}
	var record sessionOrientationRecord
	if json.Unmarshal(body, &record) != nil || strings.TrimSpace(record.HookID) == "" {
		return "- Orientation (scope, sources, hooks, constitution) is served on SessionStart; re-serve with " + command
	}
	return fmt.Sprintf("- Orientation served this session at `%s`; re-serve with %s", safeMD(record.HookID), command)
}

func readSessionWrittenPaths(homePath, sessionID string) ([]string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	dir := filepath.Join(homePath, "state", "runs", "hook-session-writes", hashStr(strings.TrimSpace(sessionID)), "written")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return nil, readErr
		}
		var record sessionMutationState
		if err := json.Unmarshal(body, &record); err != nil {
			return nil, fmt.Errorf("parse written session mutation %s: %w", entry.Name(), err)
		}
		if err := validateSessionMutationState(record, sessionID, record.MutationInputRef); err != nil {
			return nil, err
		}
		paths = append(paths, record.Paths...)
	}
	return uniqueSortedPaths(paths), nil
}

func validateSessionMutationState(record sessionMutationState, sessionID, mutationInputRef string) error {
	if record.SchemaVersion != sessionMutationStateSchemaVersion {
		return fmt.Errorf("session mutation state schema = %q", record.SchemaVersion)
	}
	if record.SessionKey != hashStr(strings.TrimSpace(sessionID)) || record.MutationInputRef != mutationInputRef {
		return fmt.Errorf("session mutation state identity mismatch")
	}
	if len(record.Paths) == 0 {
		return fmt.Errorf("session mutation state paths required")
	}
	return nil
}

func sessionMutationRecordPath(sessionID, state, mutationInputRef string) string {
	return filepath.Join("state", "runs", "hook-session-writes", hashStr(strings.TrimSpace(sessionID)), state, hashStr(mutationInputRef)+".json")
}

func writeSessionMutationState(path string, record sessionMutationState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".hook-session-mutation-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func boundMutationInputRef(refs []string) string {
	for _, ref := range refs {
		if strings.HasPrefix(ref, "mutation-input:sha256:") {
			return ref
		}
	}
	return ""
}

func readSourceControlChangedPaths(gitRoot string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	commands := [][]string{
		{"-C", gitRoot, "diff", "--name-only", "--no-renames", "-z", "--"},
		{"-C", gitRoot, "diff", "--cached", "--name-only", "--no-renames", "-z", "--"},
		{"-C", gitRoot, "ls-files", "--others", "--exclude-standard", "-z", "--"},
	}
	paths := make([]string, 0)
	for _, args := range commands {
		output, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git changed paths (%s): %w: %s", strings.Join(args[2:], " "), err, strings.TrimSpace(string(output)))
		}
		for _, relative := range strings.Split(string(output), "\x00") {
			if relative == "" {
				continue
			}
			canonical, err := authority.CanonicalSourceControlMutationTarget(gitRoot, filepath.FromSlash(relative))
			if err != nil {
				return nil, err
			}
			paths = append(paths, canonical)
		}
	}
	return uniqueSortedPaths(paths), nil
}

func uniqueSortedPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path = strings.TrimSpace(path); path != "" {
			seen[filepath.Clean(path)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// shouldMintGateReview scopes and dedups the source-control-dirty held gate-review mint.
// A held operator-review is filed ONLY for compound's OWN governed repo (the git repository
// that contains COMPOUND_HOME) and ONLY when no OPEN held gate-review already tracks that
// repo. Edits outside compound's own repo, or repeats while a review is already open, still
// enforce identically at the Gate + PolicyDecision audit (writeGateArtifacts) — they simply
// file no additional operator-review ledger work, which no consumer/drain ever existed for.
func shouldMintGateReview(store *work.Store, homePath string, sc policy.SourceControlContext, targetGitRoot string) (bool, error) {
	if !targetInCompoundRepo(homePath, targetGitRoot) {
		return false, nil
	}
	exists, err := openGateReviewExists(store, sc)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// targetInCompoundRepo reports whether targetGitRoot is compound's own governed repo — the
// git repository that contains COMPOUND_HOME. Both roots are derived the same way
// (`git rev-parse --show-toplevel`), so the equality is symlink-canonical and worktree-consistent.
// When the home is not inside a git repo (e.g. a global ~/.compound), there is no repo anchor
// and nothing is minted — a fail-safe: the Gate and PolicyDecision audit still enforce, and no
// operator debt is filed for repos that cannot be identified as compound's own.
func targetInCompoundRepo(homePath, targetGitRoot string) bool {
	targetGitRoot = strings.TrimSpace(targetGitRoot)
	if strings.TrimSpace(homePath) == "" || targetGitRoot == "" {
		return false
	}
	snap, err := authority.ReadGitSnapshot(homePath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(snap.GitRoot) != "" && snap.GitRoot == targetGitRoot
}

// openGateReviewExists reports whether an OPEN (held) gate-review already tracks the same
// source-control entity (repo). The dedup key is the repo identity (entity ref), not the
// head-specific evidence ref, so repeated dirty edits to the same repo — across commits —
// collapse to a single held review instead of minting fresh, orphan-prone operator debt.
func openGateReviewExists(store *work.Store, sc policy.SourceControlContext) (bool, error) {
	entity := strings.TrimSpace(sc.EntityRef)
	if entity == "" {
		return false, nil
	}
	works, err := store.List()
	if err != nil {
		return false, err
	}
	prefix := "source-control:" + entity + ":"
	for _, w := range works {
		if w.State != "held" {
			continue
		}
		if !strings.HasPrefix(w.SuccessPredicate, "review held policy gate") {
			continue
		}
		for _, ref := range w.EvidenceRefs {
			if strings.HasPrefix(ref, prefix) {
				return true, nil
			}
		}
	}
	return false, nil
}

func postToolUse(name string, fx Fixture, deps Deps, now time.Time) (Result, error) {
	inputHash := hashStr(fx.Tool)
	if fx.CapabilityUse != nil {
		inputHash = hashStr(fx.Tool + "\x00" + fx.CapabilityUse.Ref + "\x00" + fx.CapabilityUse.Trigger)
	}
	ev := newEvent(name, fx, "ok", inputHash, mergeRefs(fx.AdapterSideEffects, []string{fx.Tool}), now)
	ev.CapabilityUse = fx.CapabilityUse
	summary := "recorded tool event: " + orDash(fx.Tool) + " (ToolEvent/ArtifactRef land at P6+)"
	if effect, err := promotePendingSessionMutation(deps.HomePath, fx); err != nil {
		return Result{}, err
	} else if effect != "" {
		ev.SideEffects = append(ev.SideEffects, effect)
	}
	// C3: a tool body at or beyond the whole context budget must never flood the window. Externalize
	// it to private content-addressed custody; a candidate is minted only when adjudication policy would
	// park or propose the pointer (never for a filed-terminal one). Only the compact pointer flows back into the
	// summary — never the body. Below-threshold output and
	// homeless invocations keep the byte-identical legacy path.
	if body := fx.ToolOutput; len(body) > MaxContextChars && strings.TrimSpace(deps.HomePath) != "" {
		store, err := mem.Open(deps.HomePath)
		if err != nil {
			return Result{}, fmt.Errorf("post-tool-use externalize tool output: %w", err)
		}
		capture, err := store.CaptureToolOutputDetailed(fx.Tool, fx.WorkID, body, now)
		if err != nil {
			return Result{}, fmt.Errorf("post-tool-use externalize tool output: %w", err)
		}
		ev.SideEffects = append(ev.SideEffects, capture.Ref)
		ev.OutputRef = capture.Ref
		summary = fmt.Sprintf("recorded tool event: %s — externalized %d-byte output to private custody (%s); retrieve via `compound memory show-output %s`", orDash(fx.Tool), len(body), capture.Pointer, capture.CID)
	}
	return Result{Event: ev, Produced: []envelope.Envelope{}, Summary: summary}, nil
}

// sessionEnd handles both the SessionEnd and Stop hooks. They share this receipt/close
// path, but their honesty signal differs: Stop is consulted against StopFeedback (some
// harnesses, e.g. codex, CAN inject/continue at end-of-turn with no SessionEnd event at
// all), SessionEnd against SessionEndReceipt (does the event exist at all). A harness that
// does not support the event degrades honestly — fail_open + a WARNING in the summary,
// mirroring the PreToolUse capability-aware enforcement pattern — never a silent pretense
// that the harness delivered a receipt it cannot.
func sessionEnd(name string, fx Fixture, deps Deps, now time.Time) (Result, error) {
	st := status(deps, fx.Harness, name)
	warn := ""
	if st == "fail_open" {
		warn = fmt.Sprintf(" — WARNING: %s has no native %s event; receipt not delivered by the harness", fx.Harness, name)
	}
	if strings.TrimSpace(fx.WorkID) == "" && strings.TrimSpace(fx.SessionID) == "" {
		return Result{}, fmt.Errorf("session-end fixture needs work_id or session_id")
	}
	if name == "Stop" && (fx.StopHookActive || fx.StopTriageHandled) {
		parts := []string{name, fx.SessionID, fx.TurnID, fx.WorkID, fx.Cwd, fx.Summary, fx.Outcome, fx.MoveType}
		if fx.WorkID == "" {
			parts = []string{name, fx.SessionID, fx.TurnID, fx.Cwd, fx.Summary, fx.TranscriptPath}
		}
		ev := newCapabilityEvent(name, fx, fx.Harness, deps, hashStr(strings.Join(parts, "\x00")), fx.AdapterSideEffects, now)
		return Result{Event: ev, Produced: []envelope.Envelope{}, Summary: "recorded handled Stop event without replaying a session receipt"}, nil
	}
	if fx.WorkID == "" {
		inputHash := hashStr(strings.Join([]string{name, fx.SessionID, fx.TurnID, fx.Cwd, fx.Summary, fx.TranscriptPath}, "\x00"))
		ev := newCapabilityEvent(name, fx, fx.Harness, deps, inputHash, fx.AdapterSideEffects, now)
		summary := fmt.Sprintf("recorded %s receipt event for session %s%s", name, fx.SessionID, warn)
		rec, err := buildWorklessSessionReceipt(name, fx, now)
		if err != nil || rec == nil {
			return Result{Event: ev, Produced: []envelope.Envelope{}, Summary: summary}, nil
		}
		ref, refErr := receiptEventRef(rec)
		if refErr != nil {
			return Result{Event: ev, Produced: []envelope.Envelope{}, Summary: summary}, nil
		}
		if strings.TrimSpace(deps.HomePath) != "" {
			store, err := eventstore.Open(deps.HomePath)
			if err != nil {
				return Result{Event: ev, Produced: []envelope.Envelope{}, Summary: summary}, nil
			}
			if _, err := store.PutJSON(rec.ID, rec, "  "); err != nil {
				return Result{Event: ev, Produced: []envelope.Envelope{}, Summary: summary}, nil
			}
			ev.SideEffects = append(ev.SideEffects, ref)
		}
		summary = fmt.Sprintf("recorded %s content receipt event for session %s, receipt %s%s", name, fx.SessionID, rec.ID, warn)
		return Result{Event: ev, Produced: []envelope.Envelope{rec}, Summary: summary}, nil
	}
	outcome := strings.TrimSpace(fx.Outcome)
	moveType := strings.TrimSpace(fx.MoveType)
	if outcome == "" || moveType == "" {
		inputHash := hashStr(strings.Join([]string{name, fx.SessionID, fx.TurnID, fx.WorkID, fx.Cwd, fx.Summary, outcome, moveType}, "\x00"))
		ev := newCapabilityEvent(name, fx, fx.Harness, deps, inputHash, fx.AdapterSideEffects, now)
		summary := fmt.Sprintf("recorded %s receipt event for work %s; explicit outcome and move_type required to close work%s", name, fx.WorkID, warn)
		return Result{Event: ev, Produced: []envelope.Envelope{}, Summary: summary}, nil
	}
	if deps.Store == nil {
		return Result{}, fmt.Errorf("session-end needs a work store (run `compound home init`)")
	}
	// Consumption detection: which injected claims did this session actually reference?
	// Fail-open — a missing/unreadable transcript yields (nil, declared=false) and NEVER
	// blocks the close (falsifier). declared=true only when the detector actually ran.
	consumedRefs, usedDeclared := detectSessionConsumption(deps, fx)
	res, err := deps.Store.Close(fx.WorkID, work.CloseInput{
		Outcome:                    outcome,
		MoveType:                   moveType,
		Actor:                      fx.Harness,
		Summary:                    fx.Summary,
		Changed:                    strings.TrimSpace(fx.Summary) != "",
		UsedCapabilities:           fx.Used,
		MissedCapabilities:         fx.Missed,
		FalseTriggeredCapabilities: fx.FalseTriggered,
		ReferencedRefs:             consumedRefs,
		ObservationMethod:          "transcript_reference_scan",
		ObservationQuality:         observationQuality(usedDeclared),
		ApplicationMethod:          map[bool]string{true: "explicit_declaration", false: ""}[len(fx.Used) > 0],
	}, now)
	if err != nil {
		return Result{}, err
	}
	receiptRef, err := eventstore.ReferenceForID(res.Receipt.ID)
	if err != nil {
		return Result{}, err
	}
	transitionRef, err := eventstore.ReferenceForID(res.Transition.ID)
	if err != nil {
		return Result{}, err
	}
	ev := newCapabilityEvent(name, fx, fx.Harness, deps, hashStr(fx.WorkID), mergeRefs(fx.AdapterSideEffects, []string{receiptRef.String(), transitionRef.String()}), now)
	ev.IDs.ContextPackID = res.Work.IDs.ContextPackID
	summary := fmt.Sprintf("closed %s: %s, receipt %s, transition %s%s", res.Work.ID, res.Work.State, res.Receipt.ID, res.Transition.ID, warn)
	return Result{Event: ev, Produced: []envelope.Envelope{&res.Receipt, &res.Transition}, Summary: summary}, nil
}

// --- helpers ---

// status is "ok" when the harness supports the hook, else "fail_open" (the honest signal
// that the harness can't natively honor it and an adapter must).
func status(deps Deps, harnessName, hookName string) string {
	if deps.Registry.Supports(harnessName, hookName) {
		return "ok"
	}
	return "fail_open"
}

// newCapabilityEvent builds an event whose status came from the harness capability
// registry and stamps the matching fail-open reason.
//
// It exists so the pairing has ONE owner. Every `fail_open` produced from `status` means
// exactly one thing — the harness has no native hook — and writing that rule at each call
// site is a rule in four places, which is a rule that drifts. The reason is stated
// explicitly rather than inferred from an EMPTY field, because "no reason recorded"
// must keep meaning "not stated", never "assumed benign": that inference is the same
// shape as the defect this field exists to fix.
//
// evFX is the fixture the event is built from (a caller may persist a redacted variant),
// while harnessName is the harness whose capability is being read.
func newCapabilityEvent(name string, evFX Fixture, harnessName string, deps Deps, inputHash string, sideEffects []string, now time.Time) envelope.HookEvent {
	st := status(deps, harnessName, name)
	ev := newEvent(name, evFX, st, inputHash, sideEffects, now)
	if st == "fail_open" {
		ev.FailOpenReason = envelope.FailOpenCapabilityAbsent
	}
	return ev
}

func newEvent(name string, fx Fixture, st, inputHash string, sideEffects []string, now time.Time) envelope.HookEvent {
	return envelope.HookEvent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVHookEvent,
			ID:            idFor("hook_", fx.Harness, name, inputHash, ts(now)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{WorkID: fx.WorkID, SessionID: fx.SessionID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Harness:        fx.Harness,
		HookName:       name,
		HookCapability: hookCapability[name],
		InputHash:      inputHash,
		SideEffects:    orEmptySlice(sideEffects),
		Status:         st,
	}
}

func buildContextRequest(name string, fx Fixture, ec envelope.EffectiveContract, pack envelope.ContextPack, mode string, now time.Time) envelope.ContextRequest {
	intentRef := ""
	if strings.TrimSpace(fx.Prompt) != "" {
		intentRef = "sha256:" + hashStr(fx.Prompt)
	} else if strings.TrimSpace(fx.Task) != "" {
		intentRef = "sha256:" + hashStr(fx.Task)
	}
	return envelope.ContextRequest{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextRequest,
			ID:            idFor("ctxreq_", fx.Harness, name, fx.Cwd, fx.Task, "mode:"+strings.TrimSpace(mode), intentRef, ts(now)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{ContextPackID: pack.Header.ID, ContractID: ec.Header.ID},
			Scope:         envelope.Scope{Surface: fx.Cwd},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		IntentTextRef: intentRef,
		Harness:       fx.Harness,
		Cwd:           fx.Cwd,
		TaskGuess:     fx.Task,
		RequestedMode: mode,
		Constraints: envelope.CtxConstraints{
			MaxContextChars:         MaxContextChars,
			AllowPrivateMemory:      false,
			AllowRawCorpus:          false,
			AllowProvisionalReviews: false,
		},
	}
}

func contextHookInputHash(name string, fx Fixture, mode string) string {
	promptHash := ""
	if strings.TrimSpace(fx.Prompt) != "" {
		promptHash = hashStr(fx.Prompt)
	}
	return hashStr(strings.Join([]string{
		"hook", strings.TrimSpace(name),
		"harness", strings.TrimSpace(fx.Harness),
		"cwd", strings.TrimSpace(fx.Cwd),
		"task", strings.TrimSpace(fx.Task),
		"repo_kind", strings.TrimSpace(fx.RepoKind),
		"mode", strings.TrimSpace(mode),
		"prompt_sha256", promptHash,
	}, "\x00"))
}

func hookRequestedMode(name string, fx Fixture) string {
	if mode := strings.TrimSpace(fx.Mode); mode != "" {
		return mode
	}
	switch name {
	case "SessionStart":
		return "observe"
	case "UserPromptSubmit":
		return "execute"
	default:
		if strings.TrimSpace(fx.Task) == "" {
			return "observe"
		}
		return "execute"
	}
}

func writeContextArtifacts(homePath, hookID string, req envelope.ContextRequest, ec envelope.EffectiveContract, pack envelope.ContextPack, rendered string) (*InjectionArtifact, error) {
	if pack.IDs.ContractID != ec.ID {
		return nil, fmt.Errorf("context pack contract_id %q does not match effective contract %q", pack.IDs.ContractID, ec.ID)
	}
	runtimePaths, err := contract.RecordRuntimeMaterialization(homePath, contract.RuntimeMaterialization{
		EffectiveContract: ec,
		ContextPack:       pack,
	})
	if err != nil {
		return nil, err
	}
	contractRef, err := filepath.Rel(homePath, runtimePaths.ContractPath)
	if err != nil {
		return nil, err
	}
	requestRef := filepath.Join("context", "requests", req.Header.ID+".json")
	requestPath := filepath.Join(homePath, requestRef)
	if err := writeJSON(requestPath, req); err != nil {
		return nil, err
	}

	packRef, err := filepath.Rel(homePath, runtimePaths.PackPath)
	if err != nil {
		return nil, err
	}

	injectionRef := filepath.Join("context", "injections", hookID+".md")
	injectionPath := filepath.Join(homePath, injectionRef)
	if err := os.MkdirAll(filepath.Dir(injectionPath), 0o755); err != nil {
		return nil, err
	}

	// Render+write the S5 instruction-pack projection under
	// $COMPOUND_HOME/context/instruction-packs/<harness>/latest.json.
	// NON-FATAL by design: a pack failure must never block context
	// injection (matches the function's log-and-continue idiom). The
	// header line in renderInjection already cites the pack pointer when
	// the harness has been resolved — best-effort, derived from the
	// pack's HookReadiness entries and the on-disk pack file (when one
	// exists from a prior call or this current call). On the very first
	// cold-start call the pack is written here so future renders can cite
	// it; the present injection's pointer line appears on subsequent
	// invocations when the on-disk pack matches.
	instrPackRef, instrPackFirst12, _ := renderAndWriteInstructionPack(homePath, strings.TrimSpace(ec.Harness))
	_ = instrPackRef
	_ = instrPackFirst12

	if err := os.WriteFile(injectionPath, []byte(rendered), 0o644); err != nil {
		return nil, err
	}
	return &InjectionArtifact{
		Ref:                injectionRef,
		Path:               injectionPath,
		ContextRequestID:   req.Header.ID,
		ContextRequestRef:  requestRef,
		ContextRequestPath: requestPath,
		ContractID:         ec.ID,
		ContractRef:        contractRef,
		ContractPath:       runtimePaths.ContractPath,
		ContextPackID:      pack.Header.ID,
		ContextPackRef:     packRef,
		ContextPackPath:    runtimePaths.PackPath,
	}, nil
}

type hookEventRecord struct {
	envelope.HookEvent
	BreakerTripped bool `json:"breaker_tripped,omitempty"`
}

func writeHookEvent(homePath string, ev envelope.HookEvent, breakerTripped bool) error {
	record := hookEventRecord{HookEvent: ev, BreakerTripped: breakerTripped}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	if _, err := store.PutJSON(ev.Header.ID, record, "  "); err != nil {
		return err
	}
	// The hook_ record above is the authoritative event; out-of-seam readers now
	// consume it through eventstore.Scan (internal/work, internal/distiller,
	// cmd/compound/sessions, internal/retrievalparity). The hook-events.ndjson
	// compatibility stream is frozen once the migration completion gate closes:
	// a post-gate home never double-writes it (no reader needs it), while a
	// pre-gate home keeps appending so any legacy reader stays whole during
	// rollout. Object-before-stream publication is preserved for pre-gate homes.
	if store.MigrationComplete() {
		return nil
	}
	return store.AppendHookEvent(record)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// maxInjectionActiveMemoryRefs caps how many ActiveMemory refs a hook digest lists;
// the pack keeps the full set — the digest is a budgeted surface, not the record.
const maxInjectionActiveMemoryRefs = 8

// maxInjectionIncludedRefs caps generic provenance in the harness digest.
// Dedicated sections render the high-signal subsets separately.
//
// 8, not 24: the ref bound and the character bound have to agree, or the section
// truncates mid-ref while the ref count reports success — observed at 10 refs, which
// cut a ref in half and dropped the one after it. A real Included ref measures ~110
// runes, so the character cap was always the true limit and the ref count was
// decorative. At 8 the list fits maxOrientationIncludedChars and ends with a
// countable "N more omitted" line instead of a severed ref.
const maxInjectionIncludedRefs = 8

// maxInjectionNegativeCards caps the already-tried-and-failed cards a digest lists.
// internal/contract already bounds the pack at maxContextNegativeCards (5) when it
// joins the projection on task scope; this is the independent digest-side bound so a
// future orient-side cap change cannot silently blow the injection budget.
const maxInjectionNegativeCards = 5

// Section caps are a BUDGET, not decoration: each surface's caps must sum within
// MaxContextChars, or the caps stop bounding anything and the sections rendered
// last are amputated by capInjectionChars instead.
// TestInjectionSectionCapsFitBudget enforces both sums; keep it green when tuning
// any figure below.
//
// Every section is budgeted on both surfaces. Nothing is left unbudgeted: an
// unbudgeted section does not get "whatever is left", it gets whatever is left
// AFTER every capped section above it, which is how the orientation tail came to
// be amputated in essentially every digest while every cap looked satisfied.
const (
	maxInjectionHeaderChars = 400
	// maxInjectionActiveMemoryChars is funded to measured demand by the surface
	// split: 2100 is the median uncapped size of this section across 1077 digests,
	// and at 1600 it truncated on the live task surface every time. The 500 runes
	// come from orientation content no longer competing for this budget.
	maxInjectionActiveMemoryChars = 2100
	// maxInjectionNegativeChars: a scope-matched prior failure changes the next move,
	// where a bare `decision://…` id carries nothing an executor can act on — which
	// is why this section outranks Active Decisions (see writeActiveDecisionsPointer).
	// 900 holds the 5 cards internal/contract admits (maxInjectionNegativeCards) at
	// the 150-350 runes a real card measures.
	//
	// The figure is unchanged from when it landed; the FUNDING is what was missing.
	// It was documented as paid for by the 137 runes the Active Decisions demotion
	// freed, which is 763 short and left the cap sum at 6320 against a 6000 budget.
	// Required Checks and Active Instructions below now pay for it out of measured
	// slack, so the claim and the arithmetic agree.
	maxInjectionNegativeChars       = 900
	maxInjectionActiveDecisionChars = 220
	// maxInjectionRequiredChecksChars: a pre_write_gate row measures ~200 runes and
	// 1077 uncapped digests never rendered more than 147 runes of this section. 350
	// holds the worst observed case plus its heading; 600 reserved room no producer
	// has ever asked for.
	maxInjectionRequiredChecksChars = 350
	// maxInjectionAvoidChars holds recall SCOPE names ("self-model",
	// "personal-corpus"), not prose — a handful of short tokens, never a paragraph.
	// Measured max 62 runes. 450 reserved paragraph room for a channel that has
	// never rendered one.
	maxInjectionAvoidChars = 160
	// maxInjectionCapabilityOfferChars: offers measured 794 median but 2681 max, and
	// at 1000 the section truncated mid-offer on the live surface — cutting a
	// constraint list in half, which reads as an offer with fewer conditions than it
	// has. 1500 covers the common case with room; per-FIELD priority within an offer
	// (why > conflicts > required > rest) is the remaining fix for the long tail.
	maxInjectionCapabilityOfferChars = 1500
	// maxInjectionOrientationNoteChars holds the task surface's one-line pointer at
	// the orientation surface. Small on purpose: a pointer that grows into a summary
	// re-creates the duplication the split removed.
	maxInjectionOrientationNoteChars = 220
)

// Orientation-surface caps. Served once per session, so these buy delivery of
// content the shared surface could never fit rather than competing with task state.
const (
	// maxOrientationSummaryChars is nearly 5x the 500 the shared surface allowed,
	// because SummaryMD carries the Tier-0 constitution and measured true demand for
	// this section is ~2343 runes median across 1077 uncapped digests. At 500 the
	// constitution was cut inside Law 6 in every digest observed, so Laws 7-16 —
	// "prove the change" among them — had never reached a live agent through the
	// hook. This is the section the orientation surface exists to deliver, so it is
	// funded to its median demand first and the rest of the surface is fitted around
	// it.
	maxOrientationSummaryChars = 2400
	// Measured max 205 across 1077 uncapped digests.
	maxOrientationActiveInstructionChars = 300
	// Measured 127 runes.
	maxOrientationActiveHookChars = 250
	// Included is the one orientation section deliberately funded BELOW its demand.
	// It is generic provenance — the same reason it was demoted below high-signal
	// meaning in the first place — so when the surface has to give something up, it
	// gives up provenance rather than the constitution. writeIncludedList drops WHOLE
	// refs to stay inside this, so the list ends with an honest "N more omitted" line
	// instead of a mid-ref character cut.
	// The 1100-rune cap is an existing contract, not a repair lever. If the trust
	// legend and omission row make the list exceed it, the renderer elides reasons
	// and drops redundant static-source provenance as whole lines while preserving
	// session-specific runtime and memory refs.
	maxOrientationIncludedChars = 1100
	// Measured ~400 runes.
	maxOrientationExcludedChars = 500
	// Hook Gate Readiness plus Hook Roster share one budget: readiness measured 265
	// runes, roster 528, so 900 holds both in full. Readiness has reached a live
	// agent 3 times in 2714 digests; funding it to full width is the point.
	maxOrientationHookReadinessChars = 900
)

// renderAndWriteInstructionPack renders and writes the harness-scoped
// instruction pack projection under
// $COMPOUND_HOME/context/instruction-packs/<harness>/latest.json.
//
// NON-FATAL by design: a pack failure must never block context injection.
// The (ref, first12, error) triple is returned for the caller's
// bookkeeping; the caller is expected to discard the error after logging
// (matching the function's existing log-and-continue idiom). When the
// harness name is empty the call is a no-op.
func renderAndWriteInstructionPack(homePath, harnessName string) (ref string, first12 string, err error) {
	if strings.TrimSpace(harnessName) == "" || strings.TrimSpace(homePath) == "" {
		return "", "", nil
	}
	pack, packErr := harness.RenderInstructionPack(homePath, harnessName, "", time.Now().UTC())
	if packErr != nil {
		return "", "", packErr
	}
	rel := filepath.ToSlash(strings.TrimPrefix(harness.InstructionPackPath(homePath, harnessName, ""), strings.TrimSpace(homePath)+"/"))
	if rel == "" {
		rel = harness.InstructionPackPath(homePath, harnessName, "")
	}
	ref = rel
	short := strings.TrimPrefix(pack.ContentSHA256, "sha256:")
	if len(short) >= 12 {
		first12 = short[:12]
	} else {
		first12 = short
	}
	return ref, first12, nil
}

// instructionPackPointerLine returns the rendered single-line pointer to
// the harness-scoped instruction pack projection, best-effort:
//
//  1. It derives the session harness from pack.HookReadiness entries —
//     when there is exactly one unique Harness value across all entries
//     the harness is taken from there; otherwise the line is omitted
//     (defensive — multi-harness sessions get a generic pointer instead
//     via the second pass below).
//  2. It reads $COMPOUND_HOME (the only place the renderInjection
//     function can reach the home — renderInjection has no homePath
//     argument and the packet's hard rule forbids call-site signature
//     changes; this deviation is documented in the handoff report).
//  3. When the home + harness both resolve, it stats the on-disk
//     projection. If the projection exists (either pre-existing from a
//     prior session-start, or written seconds ago by writeContextArtifacts
//     in the same call), the line is emitted with the projection's
//     home-relative ref and the first 12 chars of the pack's content sha
//     (parse-from-disk is the only thing this function can do without
//     additional seams).
//  4. If anything fails, the line is omitted — the rest of the
//     injection is unaffected (fail-open).
func instructionPackPointerLine(pack envelope.ContextPack) string {
	homePath := strings.TrimSpace(os.Getenv("COMPOUND_HOME"))
	if homePath == "" {
		return ""
	}
	harnessName := firstHookReadinessHarness(pack)
	if harnessName == "" {
		return ""
	}
	path := harness.InstructionPackPath(homePath, harnessName, "")
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var existing harness.InstructionPack
	if err := json.Unmarshal(body, &existing); err != nil {
		return ""
	}
	rel := harnessInstructionPackRel(homePath, harnessName)
	short := strings.TrimPrefix(existing.ContentSHA256, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	if short == "" {
		return ""
	}
	return fmt.Sprintf("- Instruction pack: `%s` sha256:%s", rel, short)
}

func firstHookReadinessHarness(pack envelope.ContextPack) string {
	if len(pack.HookReadiness) == 0 {
		return ""
	}
	want := strings.TrimSpace(pack.HookReadiness[0].Harness)
	if want == "" {
		return ""
	}
	for i := 1; i < len(pack.HookReadiness); i++ {
		if strings.TrimSpace(pack.HookReadiness[i].Harness) != want {
			return ""
		}
	}
	return want
}

func harnessInstructionPackRel(homePath, harnessName string) string {
	abs := harness.InstructionPackPath(homePath, harnessName, "")
	rel := strings.TrimPrefix(abs, strings.TrimSpace(homePath)+"/")
	if rel == abs || rel == "" {
		return abs
	}
	return filepath.ToSlash(rel)
}

// injectionSurface selects WHICH half of the digest an event renders.
//
// SessionStart and UserPromptSubmit used to render the same content under the same
// MaxContextChars budget, which made the digest structurally unable to deliver its
// own tail: true uncapped demand measures ~15800 runes against 6000, so the
// sections rendered last (Included, Excluded, Hook Gate Readiness, Hook Roster)
// were amputated in essentially every live digest, and the constitution inside
// SummaryMD was cut mid-Law every time. No cap allocation closes a 2.6x
// oversubscription; splitting the surfaces by how often the content CHANGES does.
type injectionSurface string

const (
	// injectionSurfaceOrientation is the stable, once-per-session half: what this
	// repo is, which sources are in and out of scope, which hooks are installed and
	// ready, and the constitution. None of it varies with the prompt, so re-serving
	// it on every turn spent the whole budget re-asserting the unchanged.
	injectionSurfaceOrientation injectionSurface = "orientation"
	// injectionSurfaceTask is the volatile, per-prompt half: scope-matched memory
	// and negatives, decisions in force, gates outstanding, capability offers — each
	// computed against THIS prompt — plus a pointer back to the orientation surface.
	injectionSurfaceTask injectionSurface = "task"
)

// injectionRender is the input to renderInjection. A struct rather than a
// parameter list because the surface and the orientation note are independent
// knobs and a positional bool would read as noise at every call site.
type injectionRender struct {
	HookID  string
	Pack    envelope.ContextPack
	Surface injectionSurface
	// OrientationNote is the task surface's pointer at the orientation surface.
	// Ignored on the orientation surface itself.
	OrientationNote string
}

func renderInjection(in injectionRender) string {
	var header strings.Builder
	header.WriteString("# Compound Context Injection\n\n")
	fmt.Fprintf(&header, "- Hook event: `%s`\n", safeMD(in.HookID))
	fmt.Fprintf(&header, "- Context pack: `%s`\n", safeMD(in.Pack.Header.ID))
	if strings.TrimSpace(in.Pack.Header.Scope.Surface) != "" {
		fmt.Fprintf(&header, "- Scope: `%s`\n", safeMD(in.Pack.Header.Scope.Surface))
	}
	if line := instructionPackPointerLine(in.Pack); line != "" {
		fmt.Fprintf(&header, "%s\n", line)
	}
	header.WriteString("- Capability refs are offers only; execution still requires WorkEnvelope, ActionIntent, and PolicyDecision.\n\n")

	var b strings.Builder
	b.WriteString(capInjectionSection(header.String(), maxInjectionHeaderChars))

	if in.Surface == injectionSurfaceOrientation {
		writeOrientationSections(&b, in.Pack)
		return capInjectionChars(b.String())
	}
	writeTaskSections(&b, in.Pack, in.OrientationNote)
	return capInjectionChars(b.String())
}

// writeTaskSections renders the per-prompt half: everything computed against THIS
// prompt, and nothing that merely restates the session.
func writeTaskSections(b *strings.Builder, pack envelope.ContextPack, orientationNote string) {
	if note := strings.TrimSpace(orientationNote); note != "" {
		writeBudgetedInjectionSection(b, maxInjectionOrientationNoteChars, func(section *strings.Builder) {
			section.WriteString(note + "\n")
		})
	}
	if len(pack.ActiveMemory) > 0 {
		writeBudgetedInjectionSection(b, maxInjectionActiveMemoryChars, func(section *strings.Builder) {
			shown := prioritizedActiveMemoryRefs(pack.ActiveMemory, pack.ActivePositions)
			omitted := 0
			if len(shown) > maxInjectionActiveMemoryRefs {
				omitted = len(shown) - maxInjectionActiveMemoryRefs
				shown = shown[:maxInjectionActiveMemoryRefs]
			}
			writeActiveMemoryList(section, pack, shown)
			if omitted > 0 {
				fmt.Fprintf(section, "- %d more active memory ref(s) omitted from digest budget\n", omitted)
			}
		})
	}
	writeBudgetedInjectionSection(b, maxInjectionNegativeChars, func(section *strings.Builder) {
		writeNegativesList(section, pack.Negatives)
	})
	writeBudgetedInjectionSection(b, maxInjectionActiveDecisionChars, func(section *strings.Builder) {
		writeActiveDecisionsPointer(section, pack.ActiveDecisions)
	})
	writeBudgetedInjectionSection(b, maxInjectionRequiredChecksChars, func(section *strings.Builder) {
		writeStringList(section, "Required Checks", pack.RequiredChecks)
	})
	writeBudgetedInjectionSection(b, maxInjectionAvoidChars, func(section *strings.Builder) {
		// pack.Avoid carries mem.Forbidden — RECALL SCOPE names ("self-model",
		// "personal-corpus"), not behaviors to avoid. Labeling it "Avoid" promised
		// behavioral steering and delivered scope config, so a reader saw "- none"
		// and concluded nothing was worth avoiding. The scope-matched behavioral
		// warning lives in "Already Tried And Failed" above.
		//
		// Empty renders NOTHING, not "- none": with no forbidden scope configured
		// the honest digest is silence. A rendered "- none" spends budget asserting
		// the absence of a constraint on every prompt, which is how the retitled
		// channel still read as a checked-and-clear gate.
		writeStringListWhenPresent(section, "Forbidden Recall Scopes", pack.Avoid)
	})
	writeBudgetedInjectionSection(b, maxInjectionCapabilityOfferChars, func(section *strings.Builder) {
		writeCapabilityOfferList(section, pack.CapabilityOffers)
	})
}

// writeOrientationSections renders the once-per-session half.
//
// Summary gets a far larger allowance here than it ever had on the shared surface,
// because the Tier-0 constitution is carried inside SummaryMD: under the old
// 500-rune cap it was cut inside Law 6 in every digest measured, so Laws 7-16 —
// including "prove the change" — had never reached a live agent through the hook.
func writeOrientationSections(b *strings.Builder, pack envelope.ContextPack) {
	writeBudgetedInjectionSection(b, maxOrientationSummaryChars, func(section *strings.Builder) {
		section.WriteString("\n## Summary\n")
		section.WriteString(safeMD(pack.SummaryMD))
		section.WriteString("\n")
	})
	writeBudgetedInjectionSection(b, maxOrientationActiveInstructionChars, func(section *strings.Builder) {
		writeStringList(section, "Active Instructions", pack.ActiveInstructions)
	})
	writeBudgetedInjectionSection(b, maxOrientationActiveHookChars, func(section *strings.Builder) {
		writeStringList(section, "Active Hooks", pack.ActiveHooks)
	})
	writeBudgetedInjectionSection(b, maxOrientationIncludedChars, func(section *strings.Builder) {
		writeIncludedList(section, pack.Included, maxOrientationIncludedChars)
	})
	writeBudgetedInjectionSection(b, maxOrientationExcludedChars, func(section *strings.Builder) {
		section.WriteString("\n## Excluded\n")
		if len(pack.Excluded) == 0 {
			section.WriteString("- none\n")
			return
		}
		for _, ref := range pack.Excluded {
			fmt.Fprintf(section, "- `%s` reason=%s\n", safeMD(ref.Ref), safeMD(ref.Reason))
		}
	})
	writeBudgetedInjectionSection(b, maxOrientationHookReadinessChars, func(section *strings.Builder) {
		writeHookReadinessList(section, pack.HookReadiness)
		writeHookRosterList(section, pack.HookRoster)
	})
}

func writeBudgetedInjectionSection(dst *strings.Builder, limit int, render func(*strings.Builder)) {
	var section strings.Builder
	render(&section)
	dst.WriteString(capInjectionSection(section.String(), limit))
}

func capInjectionSection(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	const marker = "\n[Section truncated; ContextPack JSON retains full provenance.]\n"
	markerRunes := []rune(marker)
	contentLimit := limit - len(markerRunes)
	if contentLimit < 0 {
		contentLimit = 0
	}
	return string(runes[:contentLimit]) + marker
}

func capInjectionChars(text string) string {
	runes := []rune(text)
	if len(runes) <= MaxContextChars {
		return text
	}
	const marker = "\n\n[Compound context truncated; ContextPack JSON retains full provenance.]\n"
	markerRunes := []rune(marker)
	limit := MaxContextChars - len(markerRunes)
	if limit < 0 {
		limit = 0
	}
	return string(runes[:limit]) + marker
}

// writeNegativesList renders the scope-matched already-tried-and-failed cards the
// ContextPack carries (envelope.NegativeCard — the compound.negatives.v0.1 projection
// joined on task scope by internal/contract's appendNegatives).
//
// This is the LAST hop of the negatives chain: producer (distiller →
// inbox/failure-patterns, internal/routeevidence → reviews/route-remorse) → projection
// (internal/negatives) → scoped join at orient → *an executor actually reading it*.
// Before this renderer the cards reached the ContextPack JSON and stopped there, so a
// harness session — the only consumer that can still change its next move — never saw
// them (AGENTS.md: "a capability with no live production consumer is bench-only").
//
// Display-only, exactly as envelope.NegativeCard specifies: a card warns, it never
// routes, dispatches, or claims. An empty/absent set renders NOTHING (not "- none"):
// negatives are scope-matched, so silence is the common correct case and a "- none"
// line would spend budget asserting the absence of evidence on every prompt.
func writeNegativesList(b *strings.Builder, cards []envelope.NegativeCard) {
	if len(cards) == 0 {
		return
	}
	shown := cards
	if len(shown) > maxInjectionNegativeCards {
		shown = shown[:maxInjectionNegativeCards]
	}
	b.WriteString("\n## Already Tried And Failed\n")
	b.WriteString("- scope-matched prior failures; evidence that warns, never routing\n")
	for _, card := range shown {
		fmt.Fprintf(b, "- `%s` source=%s", safeMD(card.ApproachKey), safeMD(card.Source))
		if tried := strings.TrimSpace(card.WhatWasTried); tried != "" {
			fmt.Fprintf(b, " tried=%s", safeMD(tried))
		}
		if why := strings.TrimSpace(card.WhyItFailed); why != "" {
			fmt.Fprintf(b, " failed=%s", safeMD(why))
		}
		if card.Confidence > 0 {
			fmt.Fprintf(b, " confidence=%.2f", card.Confidence)
		}
		fmt.Fprintf(b, " ref=`%s`\n", safeMD(card.Ref))
	}
	if omitted := len(cards) - len(shown); omitted > 0 {
		fmt.Fprintf(b, "- %d more negative(s) omitted digest budget\n", omitted)
	}
}

// writeActiveDecisionsPointer renders the selected-decision COUNT plus how to resolve
// one, instead of a list of bare `decision://…` ids. New packs are task-ranked, but
// this unversioned renderer can also receive legacy packs, so it only describes the
// selected set it actually holds. An opaque id carries no decision content an executor
// can act on. The full accepted inventory remains in decision authority.
func writeActiveDecisionsPointer(b *strings.Builder, refs []string) {
	b.WriteString("\n## Active Decisions\n")
	if len(refs) == 0 {
		b.WriteString("- no decision refs carried by this ContextPack\n")
		return
	}
	label := "refs"
	if len(refs) == 1 {
		label = "ref"
	}
	fmt.Fprintf(b, "- %d decision %s carried by this ContextPack\n", len(refs), label)
	b.WriteString("- current lookup (if active): `compound decisions show <id>`; frozen ref/rev stay in ContextPack JSON\n")
}

func writeCapabilityOfferList(b *strings.Builder, offers []envelope.CapabilityOffer) {
	if len(offers) == 0 {
		return
	}
	const heading = "\n## Capability Offers\n"
	const preamble = "- offers only; execution still requires WorkEnvelope, ActionIntent, PolicyDecision\n"
	b.WriteString(heading)
	b.WriteString(preamble)

	// Per-offer budget, so one verbose offer cannot consume the section and leave the
	// rest severed. The section cap alone truncated by rune count, which cut INSIDE a
	// field: observed live cutting a `conflicts=` boundary list partway through, so
	// the offer rendered with one partial conflict when it carried several. That does
	// not read as a truncated offer — it reads as an offer with fewer conditions than
	// it has, which is a policy surface quietly weakening itself.
	perOffer := maxInjectionCapabilityOfferChars / max(1, len(offers))
	if perOffer < minCapabilityOfferChars {
		perOffer = minCapabilityOfferChars
	}

	// The per-offer budget bounds one offer; the SECTION budget bounds the list. Both
	// are needed: the per-offer floor means a long offer list would otherwise sum past
	// the section cap, and capInjectionSection would then truncate by rune count and
	// reintroduce the mid-field cut this renderer exists to remove. Stopping here, with
	// a count, keeps every rendered offer whole.
	// used counts the WHOLE section, heading and preamble included. Counting only the
	// offer lines is how the first version of this overran its own cap by 50 runes:
	// the prelude is part of what capInjectionSection would measure.
	sectionRoom := maxInjectionCapabilityOfferChars - offerOmissionReserve
	used := utf8.RuneCountInString(heading) + utf8.RuneCountInString(preamble)
	rendered := 0
	for _, offer := range offers {
		line, dropped := renderCapabilityOffer(offer, perOffer)
		if line == "" {
			break // not even the head fits; report the remainder below
		}
		if len(dropped) > 0 {
			line += fmt.Sprintf(" [%d field(s) omitted: %s]", len(dropped), strings.Join(dropped, ","))
		}
		cost := utf8.RuneCountInString(line) + 1 // +1 for the newline
		if used+cost > sectionRoom {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
		used += cost
		rendered++
	}
	if omitted := len(offers) - rendered; omitted > 0 {
		fmt.Fprintf(b, "- %d more offer(s) omitted from digest budget\n", omitted)
	}
}

// offerOmissionReserve keeps room for the "N more offer(s) omitted" line, so the
// sentence that reports the omission can never itself be the thing that gets cut.
const offerOmissionReserve = 60

// minCapabilityOfferChars floors the per-offer budget so a long offer list does not
// divide the section into slices too small to carry an offer's identity.
const minCapabilityOfferChars = 240

// renderCapabilityOffer renders one offer within budget, dropping WHOLE fields
// worst-first and naming what it dropped.
//
// The priority order is the point. `why` and `conflicts` outrank everything because
// they are what a reader acts on: why this was offered, and what it must not be used
// for. `note` ranks last because it repeats the section preamble verbatim, so losing
// it costs nothing. Dropping in this order means the fields that survive are the ones
// that change a decision, and a dropped field is REPORTED rather than silently
// severed — a named omission keeps the reader accurate about what they were not told.
//
// Returns ("", nil) when not even the head fits, so the caller can count the offer as
// omitted rather than emit a fragment.
func renderCapabilityOffer(offer envelope.CapabilityOffer, budget int) (string, []string) {
	head := fmt.Sprintf("- `%s` kind=%s name=%s score=%.2f",
		safeMD(offer.Ref), safeMD(offer.Kind), safeMD(offer.Name), offer.Score)
	if utf8.RuneCountInString(head) > budget {
		return "", nil
	}

	type field struct {
		name string
		text string
		// compact is a count-only form used when the full text will not fit. Set only
		// for fields whose ABSENCE changes whether the capability should be used at
		// all: a reader who cannot see "3 conflicts exist" reads the offer as
		// unconditional. Costs ~20 runes and preserves the safety signal when the
		// content itself cannot survive.
		compact string
	}
	constraint := func(name string, values []envelope.CapabilityOfferConstraint, safetyCritical bool) field {
		f := field{name: name, text: capabilityConstraintSummary(name, values)}
		if safetyCritical && len(values) > 0 {
			f.compact = fmt.Sprintf(" %s=%d(omitted)", name, len(values))
		}
		return f
	}
	fields := []field{
		{name: "why"},
		constraint("conflicts", offer.Conflicts, true),
		constraint("required", offer.Required, true),
		constraint("harnesses", offer.Harnesses, false),
		constraint("missed_when", offer.KnownMissedWhen, false),
		constraint("false_triggered_by", offer.KnownFalseTriggeredBy, false),
		constraint("evidence", offer.Evidence, false),
		{name: "note"},
	}
	if why := strings.TrimSpace(offer.Why); why != "" {
		fields[0].text = " why=" + safeMD(why)
	}
	if note := strings.TrimSpace(offer.CostRiskNote); note != "" {
		fields[len(fields)-1].text = " note=" + safeMD(note)
	}

	// Reserve the cheap safety counts up front, so a verbose optional field cannot
	// crowd them out. Observed live before this reserve existed: an offer with a long
	// `why` spent its whole budget there and dropped `conflicts` entirely, while
	// offers with shorter `why` values kept their counts — the safety signal survived
	// or not depending on how talkative an unrelated field happened to be.
	reserve := 0
	for _, f := range fields {
		if f.text != "" && f.compact != "" {
			reserve += utf8.RuneCountInString(f.compact)
		}
	}

	var b strings.Builder
	b.WriteString(head)
	used := utf8.RuneCountInString(head)
	var dropped []string
	for _, f := range fields {
		if f.text == "" {
			continue
		}
		// A safety-critical field spends its own reservation, so it is not competing
		// against it; every other field must leave the remaining reservations intact.
		own := 0
		if f.compact != "" {
			own = utf8.RuneCountInString(f.compact)
		}
		if size := utf8.RuneCountInString(f.text); used+size+reserve-own <= budget {
			b.WriteString(f.text)
			used += size
			reserve -= own
			continue
		}
		// Full text does not fit. For a safety-critical field, keep the count so the
		// reader still knows conditions exist; otherwise report the omission by name.
		if f.compact != "" {
			if size := utf8.RuneCountInString(f.compact); used+size <= budget {
				b.WriteString(f.compact)
				used += size
				reserve -= own
				continue
			}
		}
		dropped = append(dropped, f.name)
	}
	return b.String(), dropped
}

// trustTier is the reader-facing grade the digest attaches to an included ref.
//
// The Included list already rendered graded provenance (source_class, status,
// confidence) and the reader collapsed all of it into one flat instruction, so a stale
// generated view and the tracked constitution arrived carrying the same weight. The
// tier states the grading in the vocabulary the reader acts on.
//
// There are exactly TWO tiers, and derivation may only ever DEMOTE from the default. A
// third "act on this without verifying" tier is not missing here, it is refused:
// status and confidence are SELF-ASSERTED by whatever produced the ref, so a verb that
// authorized acting without verification would be forgeable by its own subject — the
// reader would be trusting a claim of trustworthiness. Demote-only leaves every input
// able to LOWER trust and none able to raise it, which is the only safe direction when
// the grading input is producer-controlled.
type trustTier string

const (
	// trustVerifyFirst is the default AND the ceiling: confirm the ref against real
	// code or a command's real output before acting on it. Every ref starts here and
	// nothing can rise above it.
	trustVerifyFirst trustTier = "verify-first"
	// trustEvidenceOnly is the demoted tier: cite it, re-derive it, never take it as a
	// premise. It is the only tier a derivation is allowed to assign.
	trustEvidenceOnly trustTier = "evidence-only"
)

// trustTierRank makes "nothing sits above the default" a checkable property instead of
// a comment: verify-first is the ceiling, and an unrecognized tier ranks at the floor
// rather than being read as more trusted than what it failed to match.
func trustTierRank(t trustTier) int {
	switch t {
	case trustVerifyFirst:
		return 1
	case trustEvidenceOnly:
		return 0
	default:
		return 0
	}
}

// verifyFirstSourceClasses is an ALLOWLIST over envelope.SourceClasses, not a denylist.
// A denylist would silently hand the default tier to every class added to the enum
// later; an allowlist demotes the new class until someone decides otherwise, and
// demotion is the safe direction.
//
//   - "source": a tracked body in the repo the reader can open and check.
//   - "runtime": state compound itself serves (rung 3 of the precedence ladder).
//
// Every other member of the enum is evidence about the world rather than a checkable
// body: "generated" (derived views, capability matches, memory relevance), "evidence",
// "review", and "private_overlay" — whose body is deliberately never materialized into
// the injection, so the reader is handed a ref it cannot read.
var verifyFirstSourceClasses = map[string]bool{
	"source":  true,
	"runtime": true,
}

// includedRefConfidenceFloor is the confidence below which a ref is demoted even when
// its class and status are clean.
const includedRefConfidenceFloor = 0.6

// includedRefTrustMarker is the ONLY trust token this renderer emits. verify-first is
// left unmarked (it is the default), so the digest pays runes only where the grade
// departs from it.
const includedRefTrustMarker = "trust=evidence-only"

// includedRefTrustTier derives the tier demote-only: it starts at the default and can
// only fall. Absent/blank status and confidence read as worse-than-current, not as
// clean — unknown provenance demotes.
func includedRefTrustTier(ref envelope.IncludedRef) trustTier {
	if strings.TrimSpace(ref.Status) != "current" {
		return trustEvidenceOnly
	}
	if !verifyFirstSourceClasses[strings.TrimSpace(ref.SourceClass)] {
		return trustEvidenceOnly
	}
	if ref.Confidence < includedRefConfidenceFloor {
		return trustEvidenceOnly
	}
	return trustVerifyFirst
}

// safeIncludedField neutralizes a trust verb forged inside producer-controlled text.
//
// safeMD only defuses newlines and backticks, so a ref or reason carrying the literal
// "trust=..." would render a verb the renderer never assigned — the same forgery the
// two-tier ruling exists to prevent, arriving through the string fields instead of
// through a third tier. Rewriting the "trust=" prefix is length-preserving, so the
// defense costs no budget.
func safeIncludedField(s string) string {
	return strings.ReplaceAll(safeMD(s), "trust=", "trust_")
}

// includedOmissionReserve keeps room for the "N more included ref(s) omitted" line, so
// the sentence reporting the omission can never itself be the thing that gets cut. Sized
// for a 3-digit count, since a control-plane pack routinely carries 100+ refs.
const includedOmissionReserve = 96

// minIncludedRefChars floors the per-ref budget so a long ref list cannot divide the
// section into slices too small to carry a ref's identity. A ref's identity is its ref
// string plus the trust marker and provenance fields; only the reason is elidable.
const minIncludedRefChars = 120

// elideIncludedReason trims a reason to fit `room` runes and says so.
//
// The reason is the ONLY elidable part of a ref line. The ref string, the trust marker and
// the provenance fields are identity: a ref rendered without them is a different claim, not
// a shorter one. The elision is marked because an unmarked trim reads as a complete reason
// that simply says less — the same failure as the severed ref this renderer exists to stop.
func elideIncludedReason(reason string, room int) string {
	const marker = "…[elided]"
	if reason == "" {
		return ""
	}
	if room <= 0 {
		return marker
	}
	if utf8.RuneCountInString(reason) <= room {
		return reason
	}
	markerLen := utf8.RuneCountInString(marker)
	if room <= markerLen {
		return marker
	}
	runes := []rune(reason)
	return string(runes[:room-markerLen]) + marker
}

// renderIncludedRef renders ONE ref as a whole line, newline included.
//
// Split out from the list so the caller can price a line BEFORE committing it to the
// section. budget <= 0 means unbudgeted (render in full); otherwise the reason is elided
// toward that share. The explicit marker may exceed an ultra-tight per-ref share; the
// caller prices the complete line and omits it if the section itself cannot fit.
func renderIncludedRef(ref envelope.IncludedRef, budget int) string {
	if budget > 0 {
		// Price everything except the reason, then give the reason what is left.
		skeleton := renderIncludedRefWith(ref, "")
		room := budget - utf8.RuneCountInString(skeleton)
		return renderIncludedRefWith(ref, elideIncludedReason(safeIncludedField(ref.Reason), room))
	}
	return renderIncludedRefWith(ref, safeIncludedField(ref.Reason))
}

func renderIncludedRefWith(ref envelope.IncludedRef, reason string) string {
	var line strings.Builder
	// The marker precedes the provenance fields on purpose: capInjectionSection
	// cuts on a rune index, not a line boundary, so a marker rendered at the end
	// of a long line could be cut off a ref that IS demoted — and a missing marker
	// reads as the default tier. Losing the reason to a cut is cheap; losing the
	// demotion silently elevates.
	fmt.Fprintf(&line, "- `%s`", safeIncludedField(ref.Ref))
	if includedRefTrustTier(ref) == trustEvidenceOnly {
		line.WriteString(" " + includedRefTrustMarker)
	}
	fmt.Fprintf(&line, " source=%s status=%s confidence=%.2f reason=%s",
		safeIncludedField(ref.SourceClass), safeIncludedField(ref.Status), ref.Confidence, reason)
	if strings.HasPrefix(ref.Ref, "capability://") {
		line.WriteString(" [offer, not execution]")
	}
	line.WriteString("\n")
	return line.String()
}

// writeIncludedList renders the Included section within `limit` runes, dropping WHOLE
// refs rather than letting the section cap sever one.
//
// The ref bound alone was never the true limit. maxInjectionIncludedRefs was set to 8 on
// the measurement that "a real Included ref measures ~110 runes", so 8 refs plus the
// heading fit 1100 — correct when written, and invalidated the moment the 108-rune trust
// legend landed above them. The live control-plane pack then needed 1105 runes, and since
// nothing enforced the character bound, capInjectionSection rune-cut the tail: the 8th ref
// rendered as `decision://DEC-0001 ... reason=accepted decision`, which does not read as a
// truncated ref, it reads as a ref whose reason IS "accepted decision". A severed ref is
// worse than an absent one because it is still legible, so the reader never doubts it.
//
// So the character bound is authoritative here and the ref count is a ceiling on top of
// it, mirroring what the capability-offer renderer already does one section down. Refs
// dropped for EITHER reason land in the same count, because the reader's question is "how
// many did I not see", not "which bound stopped it".

// selectIncludedRefs picks up to `max` refs from `refs`, prioritizing
// session-specific runtime/generated refs ahead of redundant SourceClass=source
// provenance. The same source refs are already named in Active Instructions,
// while runtime/generated refs have no equivalent exact-ref channel.
//
// The selection preserves the original relative order among the chosen refs:
// a later ref in the served pack must never appear before an earlier one. When
// the served pack has more non-source refs than `max`, the earliest non-source
// refs win. Served order is already the pack's precedence order; the selector
// changes only source-vs-non-source priority and does not invent a second
// freshness ranking.
//
// The returned slice is a fresh copy; `refs` is never aliased.
func selectIncludedRefs(refs []envelope.IncludedRef, max int) []envelope.IncludedRef {
	if len(refs) <= max {
		out := make([]envelope.IncludedRef, len(refs))
		copy(out, refs)
		return out
	}
	type indexedRef struct {
		idx      int
		priority int
	}
	indexed := make([]indexedRef, len(refs))
	for i, ref := range refs {
		if strings.TrimSpace(ref.SourceClass) == "source" {
			indexed[i] = indexedRef{idx: i, priority: 1}
		} else {
			indexed[i] = indexedRef{idx: i, priority: 0}
		}
	}
	// Stable sort by (priority, original index). Non-source refs sort first;
	// ties resolve by original index so the order is deterministic.
	sort.SliceStable(indexed, func(i, j int) bool {
		if indexed[i].priority != indexed[j].priority {
			return indexed[i].priority < indexed[j].priority
		}
		return indexed[i].idx < indexed[j].idx
	})
	// Re-sort the chosen slice by original index so the rendered list preserves
	// the relative order the pack served.
	chosen := indexed[:max]
	sort.SliceStable(chosen, func(i, j int) bool {
		return chosen[i].idx < chosen[j].idx
	})
	out := make([]envelope.IncludedRef, len(chosen))
	for i, x := range chosen {
		out[i] = refs[x.idx]
	}
	return out
}

func writeIncludedList(b *strings.Builder, refs []envelope.IncludedRef, limit int) {
	const heading = "\n## Included\n"
	b.WriteString(heading)
	if len(refs) == 0 {
		b.WriteString("- none\n")
		return
	}
	selected := selectIncludedRefs(refs, maxInjectionIncludedRefs)
	// The legend renders FIRST so it survives a section truncated at the tail, and it
	// defines the grammar for harnesses whose own instruction file does not.
	const legend = "- trust: unmarked = verify-first; `" + includedRefTrustMarker + "` = never a premise, re-derive it. Tiers only demote.\n"
	b.WriteString(legend)

	// used counts the WHOLE section, heading and legend included — counting only the ref
	// lines is how the offer renderer first overran its own cap by 50 runes.
	sectionRoom := limit - includedOmissionReserve
	overhead := utf8.RuneCountInString(heading) + utf8.RuneCountInString(legend)

	// Pass 1: price every ref in full. The common case fits, and a pack that fits must
	// render losslessly — an elision budget applied unconditionally would trim reasons
	// nobody needed to lose.
	lines := make([]string, 0, len(selected))
	total := overhead
	for _, ref := range selected {
		line := renderIncludedRef(ref, 0)
		lines = append(lines, line)
		total += utf8.RuneCountInString(line)
	}

	if len(selected) == len(refs) && total <= limit {
		for _, line := range lines {
			b.WriteString(line)
		}
		return
	}

	// Pass 2: if the equal share would fall below a ref's identity floor, shed
	// redundant tracked-source provenance first. The same sources are already
	// named in Active Instructions, while session-specific runtime/memory refs
	// have no equivalent exact-ref channel.
	if total > sectionRoom {
		for len(selected) > 1 &&
			(sectionRoom-overhead)/len(selected) < minIncludedRefChars {
			// Once redundant static refs are exhausted, preserve every remaining
			// session-specific ref in served order rather than dropping its head.
			drop := -1
			for i := len(selected) - 1; i >= 0; i-- {
				if strings.TrimSpace(selected[i].SourceClass) == "source" {
					drop = i
					break
				}
			}
			if drop < 0 {
				break
			}
			selected = append(selected[:drop], selected[drop+1:]...)
		}
		perRef := (sectionRoom - overhead) / max(1, len(selected))
		lines = lines[:0]
		for _, ref := range selected {
			lines = append(lines, renderIncludedRef(ref, perRef))
		}
	}

	// Emit whatever still fits, whole lines only. With an equal share this drops nothing
	// in practice; it is the backstop for a pack so large that even the floored per-ref
	// budget oversubscribes the section.
	used := overhead
	rendered := 0
	for _, line := range lines {
		cost := utf8.RuneCountInString(line)
		if used+cost > sectionRoom {
			break
		}
		b.WriteString(line)
		used += cost
		rendered++
	}
	if omitted := len(refs) - rendered; omitted > 0 {
		fmt.Fprintf(b, "- %d more included ref(s) omitted digest budget; see ContextPack JSON for full provenance\n", omitted)
	}
}

func writeCapabilityConstraintSummary(b *strings.Builder, label string, values []envelope.CapabilityOfferConstraint) {
	b.WriteString(capabilityConstraintSummary(label, values))
}

// capabilityConstraintSummary renders one labelled constraint field, or "" when the
// field is empty. Returned as a string rather than written directly so the offer
// renderer can decide whether the WHOLE field fits before committing to it.
func capabilityConstraintSummary(label string, values []envelope.CapabilityOfferConstraint) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		part := strings.TrimSpace(value.Ref)
		if strings.TrimSpace(value.Kind) != "" {
			part = strings.TrimSpace(value.Kind) + ":" + part
		}
		if strings.TrimSpace(value.Detail) != "" {
			part += "(" + strings.TrimSpace(value.Detail) + ")"
		}
		if part == "" {
			continue
		}
		parts = append(parts, safeMD(part))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf(" %s=%s", label, strings.Join(parts, ","))
}

func writeActiveMemoryList(b *strings.Builder, pack envelope.ContextPack, refs []string) {
	if len(refs) == 0 {
		return
	}
	b.WriteString("\n## Active Memory\n")
	for _, ref := range refs {
		line := activeMemoryLine(pack, ref)
		if line == "" {
			fmt.Fprintf(b, "- `%s`\n", safeMD(ref))
			continue
		}
		fmt.Fprintf(b, "- `%s` - %s\n", safeMD(ref), safeMD(line))
	}
}

func prioritizedActiveMemoryRefs(refs []string, positions []envelope.ActivePosition) []string {
	if len(refs) == 0 || len(positions) == 0 {
		return refs
	}
	positionRefs := map[string]bool{}
	for _, position := range positions {
		if strings.TrimSpace(position.Ref) != "" {
			positionRefs[position.Ref] = true
		}
	}
	if len(positionRefs) == 0 {
		return refs
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if positionRefs[ref] {
			out = append(out, ref)
		}
	}
	for _, ref := range refs {
		if !positionRefs[ref] {
			out = append(out, ref)
		}
	}
	return out
}

func activeMemoryLine(pack envelope.ContextPack, ref string) string {
	for _, included := range pack.Included {
		if included.Ref != ref {
			continue
		}
		reason := strings.TrimSpace(included.Reason)
		if strings.HasPrefix(reason, "Compound memory ledger claim;") || strings.HasPrefix(reason, "Compound memory folded position;") {
			if _, line, ok := strings.Cut(reason, ":"); ok {
				return strings.TrimSpace(line)
			}
		}
		return reason
	}
	return ""
}

func writeStringList(b *strings.Builder, title string, xs []string) {
	fmt.Fprintf(b, "\n## %s\n", title)
	if len(xs) == 0 {
		b.WriteString("- none\n")
		return
	}
	for _, x := range xs {
		fmt.Fprintf(b, "- `%s`\n", safeMD(x))
	}
}

// writeStringListWhenPresent is writeStringList for channels where absence is the
// common correct case, so "- none" would be a false all-clear rather than
// information: it renders the heading only when there is something under it.
func writeStringListWhenPresent(b *strings.Builder, title string, xs []string) {
	if len(xs) == 0 {
		return
	}
	writeStringList(b, title, xs)
}

func writeHookReadinessList(b *strings.Builder, readiness []envelope.HookReadiness) {
	if len(readiness) == 0 {
		return
	}
	b.WriteString("\n## Hook Gate Readiness\n")
	for _, r := range readiness {
		fmt.Fprintf(b, "- `%s/%s` status=%s installed=%t native_block=%t",
			safeMD(r.Harness), safeMD(r.Hook), safeMD(r.Status), r.Installed, r.NativeBlock)
		if r.InstalledRef != "" {
			fmt.Fprintf(b, " ref=`%s`", safeMD(r.InstalledRef))
		}
		if r.AdapterRequired {
			b.WriteString(" adapter_required=true")
		}
		if r.AdapterCommand != "" {
			fmt.Fprintf(b, " command=`%s`", safeMD(r.AdapterCommand))
		}
		if r.Reason != "" {
			fmt.Fprintf(b, " reason=%s", safeMD(r.Reason))
		}
		b.WriteString("\n")
	}
}

func writeHookRosterList(b *strings.Builder, roster []envelope.HookRosterEntry) {
	if len(roster) == 0 {
		return
	}
	b.WriteString("\n## Hook Roster\n")
	for _, r := range roster {
		fmt.Fprintf(b, "- `%s` harness=%s hook=%s event=%s status=%s",
			safeMD(r.Ref), safeMD(r.Harness), safeMD(r.Hook), safeMD(r.Event), safeMD(r.Status))
		if r.Matcher != "" {
			fmt.Fprintf(b, " matcher=%s", safeMD(r.Matcher))
		}
		if r.Reason != "" {
			fmt.Fprintf(b, " reason=%s", safeMD(r.Reason))
		}
		b.WriteString("\n")
	}
}

func safeMD(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "`", "'")
	return strings.TrimSpace(s)
}

func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(h[:])[:12]
}

// hashStr returns a hex digest of input — hooks store only the hash of a prompt, never the
// raw text (privacy by default).
func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func ts(now time.Time) string { return strconv.FormatInt(now.UTC().UnixNano(), 10) }

func orDef(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func orEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
