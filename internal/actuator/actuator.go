package actuator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/harness"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// buildAdapterRegistry constructs the harness adapter registry Run dispatches
// through. It is a package var so a test can substitute a hermetic stub for the
// subprocess-spawning built-ins; production always builds the real registry over the
// home's runattempt store. Mirrors the recordEnvelopeFn seam in internal/act.
var buildAdapterRegistry = func(homePath string) *harness.AdapterRegistry {
	return harness.NewAdapterRegistry(runattempt.Open(homePath))
}

// recordSwitchbackConsumption is the accounting seam used after a governed
// switchback step lands. Production delegates to Ledger.Record; tests can force
// the otherwise timing-sensitive "write receipted, accounting failed" boundary.
var recordSwitchbackConsumption = func(ledger *Ledger, consumption Consumption) error {
	return ledger.Record(consumption)
}

var recordRunConsumption = func(ledger *Ledger, consumption Consumption) error {
	return ledger.Record(consumption)
}

// Plan is the actuator's full routing verdict for one move: the policy decision,
// the budget headroom, and the auto/hold/deny routing — computed but NOT yet
// executed. EvaluatePlan is pure orchestration over existing authority (policy,
// budget), so it is cheap, side-effect-free, and safe to call speculatively.
type Plan struct {
	Decision       Decision                `json:"decision"`
	PolicyDecision envelope.PolicyDecision `json:"policy_decision"`
	Headroom       Headroom                `json:"headroom"`
	Intent         envelope.ActionIntent   `json:"intent"`
	Channel        string                  `json:"channel"`
	ProductID      string                  `json:"product_id,omitempty"`
}

// EvaluatePlan composes the three authoritative inputs — policy.Evaluate, the
// budget Check, and Route — into a Plan. It never executes. channel + productID
// select the budget; intent carries the declared mutation_class + reversibility
// that Route reads.
func EvaluatePlan(homePath string, intent envelope.ActionIntent, channel, productID string, cfg policy.Config, now time.Time) (Plan, error) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		channel = WildcardChannel
	}
	pd := policy.Evaluate(intent, cfg, now)
	ledger := OpenLedger(homePath)
	budget, err := ledger.LoadBudget(productID, now)
	if err != nil {
		return Plan{}, err
	}
	headroom, err := ledger.Check(budget, productID, channel, now)
	if err != nil {
		return Plan{}, err
	}
	headroom = enforceRunBudgetAccounting(intent, headroom)
	decision := Route(intent, pd.Outcome, headroom)
	decision.Channel = channel
	decision.PolicyOutcome = pd.Outcome
	return Plan{
		Decision:       decision,
		PolicyDecision: pd,
		Headroom:       headroom,
		Intent:         intent,
		Channel:        channel,
		ProductID:      productID,
	}, nil
}

// StepResult is the outcome of applying one switchback step. Applied is true
// when the actuator performed it; Held means it needs an operator (with the
// reason); the receipt/transition refs are set when a governed write landed.
type StepResult struct {
	Kind          string   `json:"kind"`
	Description   string   `json:"description"`
	Applied       bool     `json:"applied"`
	Held          bool     `json:"held"`
	Reasons       []string `json:"reasons,omitempty"`
	ReceiptRef    string   `json:"receipt_ref,omitempty"`
	TransitionRef string   `json:"transition_ref,omitempty"`
	ApplyHint     string   `json:"apply_hint,omitempty"`
	// AccountingFailed is distinct from Applied: the governed mutation and
	// receipt may already be durable even though budget consumption failed to
	// record. Callers must preserve this partial result and return non-green.
	AccountingFailed bool   `json:"accounting_failed,omitempty"`
	AccountingError  string `json:"accounting_error,omitempty"`
}

// SwitchbackResult is the outcome of applying a ProjectProfile's switchback.
type SwitchbackResult struct {
	ProfileID string       `json:"profile_id"`
	WorkID    string       `json:"work_id"`
	Steps     []StepResult `json:"steps"`
}

// Switchback applies a project profile's switchback within known work. It writes
// instruction files and repoints the routing key directly (both local_reversible,
// policy-gated, budget-checked, receipt-emitting via the act seam), and surfaces
// the settings/budget steps as the exact governed commands for the operator. A
// switchback is the "adapt to this project smoothly" act: one call re-points
// routing + instructions for the project, and every mutation leaves a receipt so
// it is audited, never silent.
func Switchback(homePath string, p ProjectProfile, workID string, cfg policy.Config, now time.Time) (SwitchbackResult, error) {
	steps, err := SwitchbackPlan(p, workID, now)
	if err != nil {
		return SwitchbackResult{}, err
	}
	if err := requireKnownWork(homePath, workID); err != nil {
		return SwitchbackResult{}, fmt.Errorf("actuator switchback: %w", err)
	}
	ledger := OpenLedger(homePath)
	budget, err := ledger.LoadBudget(p.ProductID, now)
	if err != nil {
		return SwitchbackResult{}, err
	}
	result := SwitchbackResult{ProfileID: p.ID, WorkID: workID}
	var accountingErrors []error

	for _, step := range steps {
		var applied StepResult
		switch step.Kind {
		case StepInstructions:
			applied = applyInstructionStep(homePath, p, workID, step, cfg, ledger, now)
		case StepRouting:
			applied = applyRoutingStep(homePath, p, step, budget, cfg, ledger, now)
		default:
			// settings + budget: surface the governed command, do not reimplement.
			applied = StepResult{
				Kind:        step.Kind,
				Description: step.Description,
				Held:        true,
				Reasons:     []string{"operator-governed step; run the existing command to apply"},
				ApplyHint:   step.ApplyHint,
			}
		}
		result.Steps = append(result.Steps, applied)
		if applied.AccountingFailed {
			accountingErrors = append(accountingErrors, fmt.Errorf("%s step (%s): %s", applied.Kind, applied.ReceiptRef, applied.AccountingError))
		}
	}
	if len(accountingErrors) > 0 {
		return result, fmt.Errorf("switchback accounting failed after receipted mutation: %w", errors.Join(accountingErrors...))
	}
	return result, nil
}

// applyInstructionStep materializes one instruction file into the workspace THROUGH
// the governed act seam: EvaluatePlan (policy + budget + Route), then
// act.Execute(FileArtifactAdapter) when auto — so the write captures a pre-image and
// emits a receipt + transition, with F11's honest rollback (a rolled-back creation
// deletes the file). When policy/budget holds it, the step is Held with the exact
// target path in reasons; there is no raw os.WriteFile on any path.
func applyInstructionStep(homePath string, p ProjectProfile, workID string, step SwitchbackStep, cfg policy.Config, ledger *Ledger, now time.Time) StepResult {
	res := StepResult{Kind: step.Kind, Description: step.Description}
	intent := instructionIntent(p, workID, step.FilePath, now)
	plan, err := EvaluatePlan(homePath, intent, "switchback", p.ProductID, cfg, now)
	var reservation Consumption
	if err == nil && plan.Decision.Mode == ModeAuto {
		plan, reservation, err = reservePlan(homePath, plan, now)
	}
	if err != nil {
		res.Held = true
		res.Reasons = []string{fmt.Sprintf("could not evaluate instruction write for %s: %v", step.FilePath, err)}
		return res
	}
	if plan.Decision.Mode != ModeAuto {
		res.Held = true
		res.Reasons = append([]string{fmt.Sprintf("instruction write held for %s", step.FilePath)}, plan.Decision.Reasons...)
		return res
	}
	store, err := act.NewArtifactStore(homePath)
	if err != nil {
		res.Held = true
		res.Reasons = []string{fmt.Sprintf("artifact store: %v", err)}
		return res
	}
	out, err := act.Execute(store, act.NewFileArtifactAdapter(), intent, plan.PolicyDecision, []byte(step.Body), now)
	if err != nil {
		// No allowing decision OR write failure: act rolled back, target unchanged.
		res.Held = true
		res.Reasons = []string{fmt.Sprintf("governed instruction write for %s refused/rolled back: %v", step.FilePath, err)}
		return res
	}
	if recErr := recordSwitchbackConsumption(ledger, settledConsumption(reservation, out.Receipt.ID)); recErr != nil {
		// The instruction write DID land and is receipted; surface the budget
		// accounting failure rather than dropping it silently (F9.1).
		res.AccountingFailed = true
		res.AccountingError = recErr.Error()
		res.Reasons = append(res.Reasons, fmt.Sprintf("instruction write receipted %s but budget consumption settlement NOT recorded; reservation remains charged: %v", out.Receipt.ID, recErr))
	}
	res.Applied = true
	res.ReceiptRef = out.Receipt.ID
	res.TransitionRef = out.Transition.ID
	return res
}

// instructionIntent builds the local_reversible/easy/local ActionIntent for
// materializing one instruction file. local_reversible is honest: act.Execute
// captures the pre-image (with F11's absent-file bit) and rollback deletes or
// restores. Under the default policy this is allow_with_obligations so the write
// flows and is receipted — while an operator who tightens local_reversible (a lower
// autonomy level) holds it instead. Smooth within limits, never smooth past them.
func instructionIntent(p ProjectProfile, workID, filePath string, now time.Time) envelope.ActionIntent {
	target := envelope.ActionTarget{Kind: "file_artifact", Path: filePath}
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            intentIDForInvocation(now, workID, "switchback_instructions", filePath),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "actuator", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Scope:         envelope.Scope{ProjectID: p.Slug, ProductID: p.ProductID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "actuator",
		Capability:    "switchback_instructions",
		Action:        "materialize_instruction_file",
		Target:        target,
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "materialize instruction file " + filePath,
		RollbackPlan:  "restore the captured pre-image of " + filePath + " (delete if it did not exist)",
	}
}

// applyRoutingStep runs the routing repoint through the actuator's own gate:
// EvaluatePlan (policy + budget + Route), then act.Execute when auto. Because the
// intent is local_reversible/easy/local, the default policy allows it and the
// switchback flows — but act.Execute still captures the pre-image and emits a
// receipt + transition, so the repoint is audited and reversible. A hold/deny
// leaves the config byte-identical and reports why.
func applyRoutingStep(homePath string, p ProjectProfile, step SwitchbackStep, budget BudgetEnvelope, cfg policy.Config, ledger *Ledger, now time.Time) StepResult {
	res := StepResult{Kind: step.Kind, Description: step.Description}
	if step.Intent == nil {
		res.Held = true
		res.Reasons = []string{"routing step has no action intent"}
		return res
	}
	plan, err := EvaluatePlan(homePath, *step.Intent, "switchback", p.ProductID, cfg, now)
	var reservation Consumption
	if err == nil && plan.Decision.Mode == ModeAuto {
		plan, reservation, err = reservePlan(homePath, plan, now)
	}
	if err != nil {
		res.Held = true
		res.Reasons = []string{fmt.Sprintf("could not evaluate routing: %v", err)}
		return res
	}
	if plan.Decision.Mode != ModeAuto {
		res.Held = true
		res.Reasons = plan.Decision.Reasons
		return res
	}
	store, err := act.NewArtifactStore(homePath)
	if err != nil {
		res.Held = true
		res.Reasons = []string{fmt.Sprintf("artifact store: %v", err)}
		return res
	}
	out, err := act.Execute(store, act.NewConfigKeyAdapter(), *step.Intent, plan.PolicyDecision, []byte(step.NewValue), now)
	if err != nil {
		// No allowing decision OR write failure: act rolled back, target unchanged.
		res.Held = true
		res.Reasons = []string{fmt.Sprintf("governed routing repoint refused/rolled back: %v", err)}
		return res
	}
	// Record the routing repoint against the switchback channel budget. The repoint
	// DID land and is receipted; if the consumption record fails, surface it on the
	// step (F9.1) rather than dropping it silently — budget accounting must not be
	// quietly wrong. The applied write is not rolled back.
	if recErr := recordSwitchbackConsumption(ledger, settledConsumption(reservation, out.Receipt.ID)); recErr != nil {
		res.AccountingFailed = true
		res.AccountingError = recErr.Error()
		res.Reasons = append(res.Reasons, fmt.Sprintf("routing repoint receipted %s but budget consumption settlement NOT recorded; reservation remains charged: %v", out.Receipt.ID, recErr))
	}
	res.Applied = true
	res.ReceiptRef = out.Receipt.ID
	res.TransitionRef = out.Transition.ID
	return res
}

// RunInput is the actuator's request to execute one move of known work on a
// harness. It mirrors harness.RunInput's caller-facing fields plus the budget
// channel + product the run executes within.
type RunInput struct {
	WorkID         string
	ProductID      string
	Channel        string
	Harness        string
	Workspace      string
	Command        string
	Args           []string
	Prompt         string
	PermissionMode string
	Timeout        time.Duration
}

// RunResult is the outcome of an actuator Run. When held, HeldMoveID/HeldMovePath
// point at the persisted human-gated action card explaining how to release it.
type RunResult struct {
	Plan           Plan   `json:"plan"`
	Executed       bool   `json:"executed"`
	Held           bool   `json:"held"`
	RunAttemptID   string `json:"run_attempt_id,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	ExitCode       int    `json:"exit_code,omitempty"`
	TimedOut       bool   `json:"timed_out,omitempty"`
	HeldMoveID     string `json:"held_move_id,omitempty"`
	HeldMovePath   string `json:"held_move_path,omitempty"`
}

// requireKnownWork binds actuator moves to a durable WorkEnvelope. Store.Get
// resolves both active and archived envelopes; runnable-state policy remains
// separate from this identity/existence boundary.
func requireKnownWork(homePath, workID string) error {
	workStore, err := work.Open(homePath)
	if err != nil {
		return fmt.Errorf("open work store: %w", err)
	}
	if _, err := workStore.Get(workID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("work %s not found: %w", workID, err)
		}
		return fmt.Errorf("read work %s: %w", workID, err)
	}
	return nil
}

// Run executes one move of known work within limits. It builds a local_reversible
// ActionIntent for the run, evaluates the full Plan (policy + budget + Route), and
// — only if the verdict is auto — dispatches through the existing harness adapter
// (which mints a RunAttempt and captures a transcript) and records the budget
// consumption. A hold/deny returns without dispatching, carrying the reasons as a
// plain-language card. This is "execute within known work, talking to harnesses,
// within limits": the actuator never invents work and never bypasses the gate.
func Run(homePath string, in RunInput, cfg policy.Config, now time.Time) (RunResult, error) {
	if strings.TrimSpace(in.WorkID) == "" {
		return RunResult{}, fmt.Errorf("actuator run: work_id required (execution is scoped to known work)")
	}
	if strings.TrimSpace(in.Harness) == "" {
		return RunResult{}, fmt.Errorf("actuator run: harness required")
	}
	if err := requireKnownWork(homePath, in.WorkID); err != nil {
		return RunResult{}, fmt.Errorf("actuator run: %w", err)
	}
	channel := firstNonEmpty(in.Channel, "execution")
	intent := runIntent(in, now)
	plan, err := EvaluatePlan(homePath, intent, channel, in.ProductID, cfg, now)
	if err != nil {
		return RunResult{}, err
	}
	if plan.Decision.Mode != ModeAuto {
		// Held: do not dispatch. Persist a human-gated action card so the hold is
		// actionable, not silent.
		card := ReleaseGateFor(plan, now)
		path, err := WriteHeldMoveCard(homePath, card)
		if err != nil {
			return RunResult{}, fmt.Errorf("actuator run: persist held-move card: %w", err)
		}
		return RunResult{Plan: plan, Held: true, HeldMoveID: card.ID, HeldMovePath: path}, nil
	}

	registry := buildAdapterRegistry(homePath)
	adapter, ok := registry.AdapterByName(in.Harness)
	if !ok {
		return RunResult{}, fmt.Errorf("actuator run: unknown harness %q; available: %v", in.Harness, registry.Names())
	}
	if err := persistRunAuthorization(homePath, plan.Intent, plan.PolicyDecision); err != nil {
		return RunResult{}, fmt.Errorf("actuator run: persist authorization: %w", err)
	}
	plan, reservation, err := reservePlan(homePath, plan, now)
	if err != nil {
		return RunResult{Plan: plan}, err
	}
	if plan.Decision.Mode != ModeAuto {
		card := ReleaseGateFor(plan, now)
		path, err := WriteHeldMoveCard(homePath, card)
		if err != nil {
			return RunResult{}, fmt.Errorf("actuator run: persist held-move card: %w", err)
		}
		return RunResult{Plan: plan, Held: true, HeldMoveID: card.ID, HeldMovePath: path}, nil
	}
	out, runErr := adapter.Run(context.Background(), harness.RunInput{
		WorkID:         in.WorkID,
		Harness:        in.Harness,
		Workspace:      in.Workspace,
		Command:        in.Command,
		Args:           in.Args,
		Prompt:         in.Prompt,
		PermissionMode: in.PermissionMode,
		Now:            now,
		Timeout:        in.Timeout,
		ActionIntentID: intent.ID,
	})

	result := RunResult{
		Plan:           plan,
		Executed:       true,
		RunAttemptID:   out.RunAttempt.ID,
		TranscriptPath: out.TranscriptPath,
		ExitCode:       out.ExitCode,
		TimedOut:       out.TimedOut,
	}

	// Record consumption for the run regardless of subprocess exit — a run that
	// happened consumed budget even if the task failed. Infrastructure failure
	// (runErr != nil) still produced a terminalized RunAttempt we record. If the
	// recording itself fails, surface it (F9.1): the run DID happen (result reports
	// Executed=true) so budget accounting must not be silently wrong. Do NOT roll
	// back the run — it already executed.
	if recErr := settleRunBudget(homePath, reservation, out.RunAttempt, adapter.Name()); recErr != nil {
		wrapped := fmt.Errorf("actuator run: run executed (run_attempt=%s exit=%d) but settling budget consumption failed; reservation remains charged: %w", out.RunAttempt.ID, out.ExitCode, recErr)
		if runErr != nil {
			return result, errors.Join(runErr, wrapped)
		}
		return result, wrapped
	}

	return result, runErr
}

func settleRunBudget(homePath string, reservation Consumption, returned runattempt.RunAttempt, harnessName string) error {
	stored, err := runattempt.Open(homePath).Get(returned.ID)
	if err != nil {
		return fmt.Errorf("reservation %s retains unresolved execution: read terminal attempt: %w", reservation.ReservationID, err)
	}
	if stored.ID != returned.ID || !runattempt.IsTerminal(stored.Status) || returned.Status != stored.Status ||
		stored.WorkID != reservation.WorkID || returned.WorkID != stored.WorkID ||
		stored.ActionIntentID != reservation.ActionIntentID || returned.ActionIntentID != stored.ActionIntentID ||
		stored.Harness != harnessName || returned.Harness != stored.Harness {
		return fmt.Errorf("reservation %s retains unresolved execution: attempt %s is nonterminal or contradicts work/action/harness identity", reservation.ReservationID, returned.ID)
	}
	return recordRunConsumption(OpenLedger(homePath), settledConsumption(reservation, stored.ID))
}

func persistRunAuthorization(homePath string, intent envelope.ActionIntent, decision envelope.PolicyDecision) error {
	if err := intent.Validate(); err != nil {
		return fmt.Errorf("ActionIntent %s invalid: %w", intent.ID, err)
	}
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("PolicyDecision %s invalid: %w", decision.ID, err)
	}
	if decision.ActionIntentID != intent.ID || decision.IDs.WorkID != intent.IDs.WorkID {
		return fmt.Errorf("PolicyDecision %s does not authorize ActionIntent %s for work %s", decision.ID, intent.ID, intent.IDs.WorkID)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	if err := events.Preflight(); err != nil {
		return fmt.Errorf("event store preflight: %w", err)
	}
	for _, record := range []struct {
		id    string
		value any
	}{
		{id: intent.ID, value: &intent},
		{id: decision.ID, value: &decision},
	} {
		if _, err := events.PutJSON(record.id, record.value, "  "); err != nil {
			return fmt.Errorf("record %s: %w", record.id, err)
		}
		if resolved, err := events.Read(record.id); err != nil {
			return fmt.Errorf("prove %s persistence: %w", record.id, err)
		} else if resolved.ID != record.id {
			return fmt.Errorf("prove %s persistence: resolved %s", record.id, resolved.ID)
		}
	}
	return nil
}

// runPlanSafe reports whether a harness run is read-only BY CONTRACT and therefore
// safe to route AUTO: the claude adapter in plan mode (its documented read-only
// posture; an empty permission mode defaults to plan). Every other run — an arbitrary
// os-harness command, claude in acceptEdits/bypass/default, codex, or an unknown
// harness — can produce effects outside a cheaply-reversible local workspace diff.
func runPlanSafe(in RunInput) bool {
	if strings.TrimSpace(strings.ToLower(in.Harness)) != "claude" {
		return false
	}
	switch strings.TrimSpace(strings.ToLower(in.PermissionMode)) {
	case "", "plan":
		return true
	default:
		return false
	}
}

// runIntent builds the ActionIntent for a harness run of known work, classifying it
// from what is ACTUALLY being run (F8) rather than a constant. A read-only plan-mode
// claude run stays local_reversible/easy/local, so it qualifies for auto-execution
// under the default policy (the budget gate still caps how many runs happen per
// window). Everything else is external_side_effect — whose default policy outcome is
// hold and which Route never auto-executes — so an arbitrary harness command is held
// for a human by construction, never labeled safe. The escape hatch stays the
// existing policy standing-grant override; there is no new bypass flag here.
func runIntent(in RunInput, now time.Time) envelope.ActionIntent {
	target := envelope.ActionTarget{Kind: "harness_run", ID: in.Harness, Path: in.Workspace}
	preview := in.Prompt
	if preview == "" {
		preview = strings.TrimSpace(in.Command + " " + strings.Join(in.Args, " "))
	}
	mutationClass, reversibility := "external_side_effect", "manual"
	rollbackPlan := "hold for operator review — an arbitrary harness run has no captured, cheaply-reversible pre-image"
	if runPlanSafe(in) {
		mutationClass, reversibility = "local_reversible", "easy"
		rollbackPlan = "revert the workspace diff captured in the run transcript"
	}
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            intentIDForInvocation(now, in.WorkID, "harness_run", in.Harness, in.Workspace, preview),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "actuator", Name: "compound"},
			IDs:           envelope.IDs{WorkID: in.WorkID},
			Scope:         envelope.Scope{ProductID: in.ProductID, Surface: in.Harness},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "actuator",
		Capability:    "harness_run",
		Action:        "run",
		Target:        target,
		MutationClass: mutationClass,
		Reversibility: reversibility,
		BlastRadius:   "local",
		Preview:       truncate(preview, 200),
		RollbackPlan:  rollbackPlan,
	}
}

// intentIDFor derives a deterministic, collision-resistant intent id from the
// move's components, prefixed "act_" to match the governed-act id family used by
// the act CLI and internal/act's own tests.
func intentIDFor(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "act_" + hex.EncodeToString(h.Sum(nil))[:16]
}

func intentIDForInvocation(now time.Time, parts ...string) string {
	return intentIDFor(append(parts, "invoked_at:"+now.UTC().Format(time.RFC3339Nano))...)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
