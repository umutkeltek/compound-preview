package act

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

const runtimeConfigInstallOwner = "act.runtime_config_installation"
const runtimeConfigRecoveryOwner = "act.runtime_config_recovery"
const runtimeConfigInstallPlanSchema = "compound.runtime_config_installation_plan.v0.2"

type RuntimeConfigInstallPlan struct {
	Operation             string                             `json:"operation"`
	Recovery              *RuntimeConfigRecoveryOrigin       `json:"recovery"`
	SchemaVersion         string                             `json:"schema_version"`
	Request               policy.RuntimeConfigInstallRequest `json:"request"`
	RequestSHA256         string                             `json:"request_sha256"`
	WorkID                string                             `json:"work_id"`
	Actor                 string                             `json:"actor"`
	WorkSHA256            string                             `json:"work_sha256"`
	ActionIntentID        string                             `json:"action_intent_id"`
	PolicyDecisionID      string                             `json:"policy_decision_id"`
	ApprovalPlanID        string                             `json:"approval_plan_id"`
	ApprovalBindingDigest string                             `json:"approval_binding_digest"`
	ApprovalExpiresAt     string                             `json:"approval_expires_at"`
}
type RuntimeConfigInstallPlanResult struct {
	PlanRef       string                   `json:"plan_ref"`
	Plan          RuntimeConfigInstallPlan `json:"plan"`
	PolicyOutcome string                   `json:"policy_outcome"`
}
type RuntimeConfigInstallResult struct {
	RecoveryWorkID        string `json:"recovery_work_id,omitempty"`
	State                 string `json:"state"`
	RunAttemptID          string `json:"run_attempt_id"`
	ReceiptID             string `json:"receipt_id"`
	TransitionID          string `json:"transition_id"`
	ObservationRef        string `json:"observation_ref,omitempty"`
	ApprovalConsumed      bool   `json:"approval_consumed"`
	ConfigurationAccepted bool   `json:"configuration_accepted"`
	ApplicationActivated  bool   `json:"application_activated"`
	ServiceActivated      bool   `json:"service_activated"`
}

// Transport injection is package-private and only tests replace it. Neither a
// request nor a CLI flag can choose a program, transport, callback or validator.
type runtimeConfigInstallTransport interface {
	Start(context.Context, policy.RuntimeConfigInstallRequest, map[string]string, string, time.Time, runtimeConfigInstallAdmissionGuard) (runtimeConfigInstallChild, error)
}
type runtimeConfigInstallChild interface {
	Wait() ([]byte, error)
	Stop(context.Context) error
}

type RuntimeConfigInstaller struct {
	home      string
	now       func() time.Time
	transport runtimeConfigInstallTransport
	alignment *CredentialPasswordAligner
}

func NewRuntimeConfigInstaller(homePath string) (*RuntimeConfigInstaller, error) {
	if homePath == "" || !filepath.IsAbs(homePath) || filepath.Clean(homePath) != homePath {
		return nil, runtimeConfigInstallError("explicit_home_required")
	}
	resolved, e := home.Resolve(homePath)
	if e != nil {
		return nil, errors.New("runtime_config_install:home_unavailable")
	}
	alignment := &CredentialPasswordAligner{home: resolved}
	return &RuntimeConfigInstaller{home: resolved, now: time.Now, alignment: alignment, transport: runtimeConfigInstallNativeTransport{home: resolved, alignment: alignment}}, nil
}

func runtimeConfigInstallHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func runtimeConfigInstallError(code string) error {
	return errors.New("runtime_config_install:" + code)
}

func (v *RuntimeConfigInstaller) requireWork(workID, actor string) (envelope.WorkEnvelope, error) {
	s, e := work.OpenReadOnly(v.home)
	if e != nil {
		return envelope.WorkEnvelope{}, runtimeConfigInstallError("work_unavailable")
	}
	w, e := s.Get(workID)
	if e != nil || actor == "" || strings.TrimSpace(actor) != actor || w.State != "running" {
		return w, runtimeConfigInstallError("work_not_executable")
	}
	leases, e := s.ListLeases()
	if e != nil {
		return w, runtimeConfigInstallError("lease_unavailable")
	}
	for _, l := range leases {
		if l.WorkID == workID && l.By == actor {
			return w, nil
		}
	}
	return w, runtimeConfigInstallError("wrong_lease_owner")
}

func (v *RuntimeConfigInstaller) witness(r policy.RuntimeConfigInstallRequest, now time.Time) error {
	if e := v.qualification(r, now); e != nil {
		return e
	}
	return v.captureAuthority(r)
}

func runtimeConfigInstallIntent(r policy.RuntimeConfigInstallRequest, workID, actor, workSHA string, now time.Time) envelope.ActionIntent {
	digest := policy.RuntimeConfigInstallDigest(r)
	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: idFor("act_", workID, actor, digest, ts(now)), CreatedAt: ts(now), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: workID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier2", EvidenceRefs: []string{r.Witness.Ref, "witness-sha256:" + r.Witness.SHA256, "work-generation:sha256:" + workSHA}, Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: actor, Capability: policy.RuntimeConfigInstallCapability, Action: policy.RuntimeConfigInstallAction, Target: envelope.ActionTarget{Kind: policy.RuntimeConfigInstallTarget, ID: r.ProfileID, Path: r.Files[0].Path}, Destination: policy.RuntimeConfigInstallDestination(r), ArgumentsHash: "sha256:" + digest, SchemaRevision: policy.RuntimeConfigInstallSchema, MutationClass: "security", Reversibility: "easy", BlastRadius: "host", RollbackPlan: "Retain original and staged remote bytes, durable journal and inactive fence; separately approved reconcile/rollback only, never automatic replay or activation."}
	return ai
}

// Plan records a real non-allowing native action/decision and creates only the
// existing exact approval plan. It never inspects credentials or opens SSH.
func (v *RuntimeConfigInstaller) Plan(r policy.RuntimeConfigInstallRequest, workID, actor string) (RuntimeConfigInstallPlanResult, error) {
	return v.plan(r, workID, actor, "install", nil)
}

func (v *RuntimeConfigInstaller) plan(r policy.RuntimeConfigInstallRequest, workID, actor, operation string, recovery *RuntimeConfigRecoveryOrigin) (RuntimeConfigInstallPlanResult, error) {
	var out RuntimeConfigInstallPlanResult
	now := v.now().UTC()
	if runtimeConfigValidateRequest(r, operation, now) != nil {
		return out, runtimeConfigInstallError("invalid_request")
	}
	w, e := v.requireWork(workID, actor)
	if e != nil {
		return out, e
	}
	if e = v.planAuthority(r, operation, recovery, workID, now); e != nil {
		return out, e
	}
	cfg, _, e := policy.LoadConfig(v.home)
	if e != nil {
		return out, runtimeConfigInstallError("policy_unavailable")
	}
	ai := runtimeConfigOperationIntent(r, workID, actor, runtimeConfigInstallHash(w), operation, recovery, now)
	pd, eligible := runtimeConfigEvaluate(ai, r, operation, recovery, cfg, now)
	if e = recordEnvelope(v.home, ai.ID, &ai); e != nil {
		return out, runtimeConfigInstallError("intent_persistence")
	}
	if e = recordEnvelope(v.home, pd.ID, &pd); e != nil {
		return out, runtimeConfigInstallError("decision_persistence")
	}
	out.PolicyOutcome = pd.Outcome
	if pd.Outcome != "hold" || !eligible {
		return out, runtimeConfigInstallError("admission_denied")
	}
	ap, _, _, e := policy.NewExactActionApprovalStore(v.home).EnsurePlanForIntent(ai, cfg, policy.RuntimeConfigInstallApprovalTTL, now)
	if e != nil {
		return out, runtimeConfigInstallError("approval_plan_unavailable")
	}
	p := RuntimeConfigInstallPlan{Operation: operation, Recovery: recovery, SchemaVersion: runtimeConfigInstallPlanSchema, Request: r, RequestSHA256: policy.RuntimeConfigInstallDigest(r), WorkID: workID, Actor: actor, WorkSHA256: runtimeConfigInstallHash(w), ActionIntentID: ai.ID, PolicyDecisionID: pd.ID, ApprovalPlanID: ap.ID, ApprovalBindingDigest: ap.BindingDigest, ApprovalExpiresAt: ap.ExpiresAt}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return out, runtimeConfigInstallError("artifact_store")
	}
	raw, _ := json.Marshal(p)
	ref, e := store.Put(raw)
	if e != nil {
		return out, runtimeConfigInstallError("plan_persistence")
	}
	out.PlanRef = ref
	out.Plan = p
	return out, nil
}

func (v *RuntimeConfigInstaller) loadPlan(ref, actor string) (RuntimeConfigInstallPlan, envelope.ActionIntent, error) {
	var p RuntimeConfigInstallPlan
	var ai envelope.ActionIntent
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return p, ai, runtimeConfigInstallError("artifact_store")
	}
	raw, e := store.Get(ref)
	if e != nil || len(raw) > 65536 {
		return p, ai, runtimeConfigInstallError("plan_unavailable")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.SchemaVersion != runtimeConfigInstallPlanSchema || p.Actor != actor || p.RequestSHA256 != policy.RuntimeConfigInstallDigest(p.Request) {
		return p, ai, runtimeConfigInstallError("plan_binding")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || !credentialVerifyUniqueJSON(raw) || !credentialVerifyRequiredFields(raw, reflect.TypeOf(p)) {
		return p, ai, runtimeConfigInstallError("plan_binding")
	}
	events, e := eventstore.Open(v.home)
	if e != nil {
		return p, ai, runtimeConfigInstallError("intent_unavailable")
	}
	record, e := events.Read(p.ActionIntentID)
	if e != nil || json.Unmarshal(record.Bytes, &ai) != nil {
		return p, ai, runtimeConfigInstallError("intent_unavailable")
	}
	created, e := time.Parse(time.RFC3339Nano, ai.CreatedAt)
	if e != nil {
		return p, ai, runtimeConfigInstallError("intent_binding")
	}
	if !runtimeConfigOperationValid(p.Operation, p.Recovery) {
		return p, ai, runtimeConfigInstallError("operation_binding")
	}
	expected := runtimeConfigOperationIntent(p.Request, p.WorkID, p.Actor, p.WorkSHA256, p.Operation, p.Recovery, created)
	if ai.ID != expected.ID || policy.CanonicalFullIntentDigest(ai) != policy.CanonicalFullIntentDigest(expected) {
		return p, ai, runtimeConfigInstallError("intent_binding")
	}
	var initial envelope.PolicyDecision
	decision, e := events.Read(p.PolicyDecisionID)
	if e != nil || json.Unmarshal(decision.Bytes, &initial) != nil || initial.ID != p.PolicyDecisionID || initial.ActionIntentID != ai.ID || initial.Outcome != "hold" {
		return p, ai, runtimeConfigInstallError("plan_decision_binding")
	}
	var approval policy.ExactActionApprovalPlan
	record, e = events.Read(p.ApprovalPlanID)
	if e != nil || json.Unmarshal(record.Bytes, &approval) != nil || approval.SchemaVersion != policy.ExactActionApprovalPlanSchema || approval.ID != p.ApprovalPlanID || approval.ActionIntentID != ai.ID || approval.BindingDigest != p.ApprovalBindingDigest || approval.ExpiresAt != p.ApprovalExpiresAt || policy.ActionBindingDigest(approval.Binding) != approval.BindingDigest {
		return p, ai, runtimeConfigInstallError("plan_approval_binding")
	}
	issued, ie := time.Parse(time.RFC3339Nano, approval.CreatedAt)
	expires, ee := time.Parse(time.RFC3339Nano, approval.ExpiresAt)
	if ie != nil || ee != nil || !expires.After(issued) || expires.Sub(issued) > policy.RuntimeConfigInstallApprovalTTL {
		return p, ai, runtimeConfigInstallError("plan_approval_binding")
	}
	return p, ai, nil
}

func (v *RuntimeConfigInstaller) current(p RuntimeConfigInstallPlan, ai envelope.ActionIntent, workSHA string) (policy.Config, envelope.PolicyDecision, error) {
	var cfg policy.Config
	var pd envelope.PolicyDecision
	now := v.now().UTC()
	if runtimeConfigValidateRequest(p.Request, p.Operation, now) != nil {
		return cfg, pd, runtimeConfigInstallError("request_stale")
	}
	w, e := v.requireWork(p.WorkID, p.Actor)
	if e != nil {
		return cfg, pd, e
	}
	if runtimeConfigInstallHash(w) != workSHA {
		return cfg, pd, runtimeConfigInstallError("work_generation_changed")
	}
	if e = v.planAuthority(p.Request, p.Operation, p.Recovery, p.WorkID, now); e != nil {
		return cfg, pd, e
	}
	halted, _, e := policy.AutonomyHalted(v.home, ai.MutationClass)
	if e != nil || halted {
		return cfg, pd, runtimeConfigInstallError("autonomy_halted")
	}
	cfg, _, e = policy.LoadConfig(v.home)
	if e != nil {
		return cfg, pd, runtimeConfigInstallError("policy_unavailable")
	}
	pd, eligible := runtimeConfigEvaluate(ai, p.Request, p.Operation, p.Recovery, cfg, now)
	if !eligible || pd.Outcome != "hold" {
		return cfg, pd, runtimeConfigInstallError("admission_denied")
	}
	if policy.ActionBindingDigest(policy.ActionBindingForIntent(ai, cfg)) != p.ApprovalBindingDigest {
		return cfg, pd, runtimeConfigInstallError("policy_generation_changed")
	}
	return cfg, pd, nil
}

// Execute takes only an immutable native plan reference, never an allowing
// decision supplied by the caller. Full current approval authority is re-read.
type runtimeConfigInstallReservation struct {
	input        work.ReceiptedActionInput
	authorizedAt time.Time
	launched     bool
}

func (v *RuntimeConfigInstaller) Execute(ctx context.Context, planRef, actor string) (out RuntimeConfigInstallResult, err error) {
	started := v.now().UTC()
	outer, outerCancel := context.WithDeadline(ctx, started.Add(60*time.Second))
	defer outerCancel()
	workCtx, workCancel := context.WithDeadline(outer, started.Add(50*time.Second))
	defer workCancel()
	p, ai, e := v.loadPlan(planRef, actor)
	if e != nil {
		return out, e
	}
	lockAction := ai.ID
	if p.Recovery != nil {
		lockAction = p.Recovery.ActionID
	}
	release, e := v.executionLock(lockAction)
	if e != nil {
		return out, e
	}
	defer release()
	if resumed, handled, resumeErr := v.resumeFinalization(p, ai, outer); handled {
		return resumed, resumeErr
	}
	var pd envelope.PolicyDecision
	var attempt runattempt.RunAttempt
	var reserved *runtimeConfigInstallReservation
	finish := func(outcome string, obs *RuntimeConfigInstallObservation, code string) (RuntimeConfigInstallResult, error) {
		return v.finish(out, p, ai, pd, outcome, obs, code, reserved, outer)
	}
	attempts := runattempt.Open(v.home)
	// Approval consumption and deterministic attempt mint share native admission.
	// The work owner then reserves its own lifecycle blocker under the same lock;
	// a fresh compare check deliberately fails if anything changed in between.
	e = home.WithAutonomyAdmissionLock(v.home, func() error {
		cfg, base, e := v.current(p, ai, p.WorkSHA256)
		if e != nil {
			return e
		}
		if workCtx.Err() != nil {
			return runtimeConfigInstallError("work_deadline")
		}
		attemptTime, _ := time.Parse("20060102t150405z", p.Request.AttemptID[:16])
		attemptID := runattempt.MintDeterministic("runtime-config-install:"+p.Request.AttemptID, attemptTime)
		if p.Operation != "install" {
			attemptID = runattempt.MintDeterministic("runtime-config-recovery:"+p.ActionIntentID, attemptTime)
		}
		if _, e = attempts.Get(attemptID); e == nil {
			return runtimeConfigInstallError("attempt_already_used")
		} else if !errors.Is(e, runattempt.ErrNotFound) {
			return runtimeConfigInstallError("attempt_authority_unavailable")
		}
		if preflightEventStore(v.home) != nil {
			return runtimeConfigInstallError("event_store_unavailable")
		}
		var matched bool
		pd, matched, e = policy.NewExactActionApprovalStore(v.home).ApplyMatchingLocked(ai, cfg, base, v.now().UTC())
		if e != nil || !matched || !policy.Allowed(pd) {
			return runtimeConfigInstallError("exact_approval_required")
		}
		out.ApprovalConsumed = true
		if e = recordEnvelope(v.home, pd.ID, &pd); e != nil {
			return runtimeConfigInstallError("approved_decision_persistence")
		}
		attempt, e = attempts.MintWithID(attemptID, p.WorkID, p.Actor, "compound", v.now().UTC())
		if e != nil {
			return runtimeConfigInstallError("attempt_persistence")
		}
		out.RunAttemptID = attempt.ID
		if e = attempts.AttachActionIntent(attempt.ID, ai.ID); e != nil {
			return runtimeConfigInstallError("attempt_binding")
		}
		return nil
	})
	if e != nil {
		if out.RunAttemptID != "" {
			return finish("failed", nil, "admission_failed")
		}
		return out, e
	}
	s, e := work.Open(v.home)
	if e != nil {
		return finish("failed", nil, "work_unavailable")
	}
	w, e := v.requireWork(p.WorkID, p.Actor)
	if e != nil {
		return finish("failed", nil, "work_unavailable")
	}
	pending := work.ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: w.State, OutcomePending: true, Outcome: "blocked", Changed: false, Summary: "awaiting bounded inactive runtime configuration installation evidence", MoveType: "audit_only", ProofRefs: []string{ai.ID, attempt.ID, "policy-decision:" + pd.ID}, BeforeRefs: []string{p.Request.Witness.Ref, "request-sha256:" + p.RequestSHA256}, FilesChanged: []string{}}
	pending.RequiredAttemptCompletion = &work.RequiredAttemptCompletion{AttemptID: attempt.ID, Owner: runtimeConfigOwner(p.Operation)}
	if p.Operation == "install" {
		out.RecoveryWorkID = runtimeConfigRecoveryWorkID(ai.ID, attempt.ID)
		pending.RequiredAttemptCompletion.Recovery = &work.RequiredRecoveryCompletion{WorkID: out.RecoveryWorkID, Owner: runtimeConfigRecoveryOwner, Capability: policy.RuntimeConfigRecoveryCapability, ArgumentsHashes: []string{
			"sha256:" + policy.RuntimeConfigRecoveryDigest(p.Request, ai.ID, attempt.ID, "reconcile"),
			"sha256:" + policy.RuntimeConfigRecoveryDigest(p.Request, ai.ID, attempt.ID, "rollback"),
		}}
	} else {
		pending.ProofRefs = append(pending.ProofRefs, p.Recovery.proofRefs()...)
	}
	authorizedAt := v.now().UTC()
	authorize := func() error {
		if _, _, e := v.current(p, ai, p.WorkSHA256); e != nil {
			return e
		}
		if policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, pd) != nil {
			return runtimeConfigInstallError("approval_consumption_unavailable")
		}
		if workCtx.Err() != nil || !v.now().Before(started.Add(50*time.Second)) {
			return runtimeConfigInstallError("work_deadline")
		}
		return nil
	}
	if e = s.ReserveReceiptedAction(p.WorkID, pending, authorizedAt, authorize); e != nil {
		return finish("failed", nil, "action_reservation")
	}
	reserved = &runtimeConfigInstallReservation{input: pending, authorizedAt: authorizedAt}
	// Exact-approval planning already published this metadata-only ActionIntent;
	// the native reauthorization API deliberately refuses published actions.
	// Read the work owner's validated reservation under native admission instead.
	guard := func(start func() error) error {
		if start == nil {
			return runtimeConfigInstallError("launch_callback")
		}
		return home.WithAutonomyAdmissionLock(v.home, func() error {
			input, at, present, e := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
			if e != nil || !present || !at.Equal(authorizedAt) || runtimeConfigInstallHash(input) != runtimeConfigInstallHash(pending) {
				return runtimeConfigInstallError("work_reservation_drift")
			}
			if e := authorize(); e != nil {
				return e
			}
			// Lock order is native autonomy, work/attempt admission, exact OS
			// Start. Release after its immediate durable disposition, never Wait.
			_, e = attempts.CommitLaunch(attempt.ID, v.now().UTC(), func() error {
				if e := authorize(); e != nil {
					return e
				}
				reserved.launched = true // attempted OS start is conservative until native evidence proves no effect
				e := start()
				if e != nil {
					reserved.launched = false
				}
				return e
			})
			return e
		})
	}
	selected := map[string]string{}
	if p.Operation == "install" {
		selected, e = v.captureProjection(p.Request)
		if e != nil {
			return finish("failed", nil, "capture_projection")
		}
	}
	defer clear(selected)
	child, e := v.transport.Start(workCtx, p.Request, selected, p.Operation, started.Add(50*time.Second), guard)
	if e != nil {
		// Start may have succeeded before native committed-evidence persistence
		// failed. The transport preserves that child so cleanup is unconditional.
		if child != nil {
			_ = child.Stop(outer)
		}
		return finish("failed", nil, "launch_failed")
	}
	if child == nil {
		return finish("failed", nil, "launch_failed")
	}
	if _, e = attempts.MarkRunning(attempt.ID, v.now().UTC()); e != nil {
		_ = child.Stop(outer)
		return finish("failed", nil, "attempt_running_failed")
	}
	raw, waitErr := child.Wait()
	stopErr := child.Stop(outer)
	if waitErr != nil || stopErr != nil {
		return finish("failed", nil, "verification_failed")
	}
	if workCtx.Err() != nil || !v.now().Before(started.Add(50*time.Second)) {
		return finish("failed", nil, "work_deadline")
	}
	obs, e := decodeRuntimeConfigInstallObservation(raw, p.Request)
	if e != nil || obs.Operation != p.Operation {
		return finish("failed", nil, "invalid_observation")
	}
	if (p.Operation == "install" || p.Operation == "reconcile") && obs.State != "installed_inactive" {
		return finish("failed", &obs, "recovery_required")
	}
	if p.Operation == "rollback" && obs.State != "rolled_back" {
		return finish("failed", &obs, "recovery_required")
	}
	result, finishErr := finish("completed", &obs, obs.State)
	if finishErr == nil && p.Operation != "install" {
		finishErr = v.clearRecoveredOriginal(*p.Recovery)
	}
	return result, finishErr
}

func (v *RuntimeConfigInstaller) finish(out RuntimeConfigInstallResult, p RuntimeConfigInstallPlan, ai envelope.ActionIntent, pd envelope.PolicyDecision, outcome string, obs *RuntimeConfigInstallObservation, code string, reserved *runtimeConfigInstallReservation, cleanup context.Context) (RuntimeConfigInstallResult, error) {
	out.State = "failed"
	attempts := runattempt.Open(v.home)
	attempt, readErr := attempts.Get(out.RunAttemptID)
	uncertain := reserved != nil && outcome != "completed" && (readErr != nil || (attempt.Launch != nil && (attempt.Launch.CommittedAt != nil || attempt.Launch.StartErrorAt == nil)))
	if uncertain || (reserved != nil && attempt.Launch != nil && (attempt.Launch.CommittedAt != nil || attempt.Launch.StartErrorAt == nil)) {
		out.State = "recovery_required"
	}
	// Persistence failure leaves the reservation and nonterminal attempt intact.
	// Recovery planning can finish this exact local outcome before a new launch.
	if cleanup.Err() != nil {
		return out, runtimeConfigInstallError("cleanup_deadline")
	}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return out, runtimeConfigInstallError("receipt_unavailable")
	}
	knowledge := ""
	if uncertain {
		knowledge = "unknown"
	}
	statusState := out.State
	if outcome == "completed" && obs != nil {
		statusState = obs.State
	}
	status := map[string]any{"schema_version": "compound.runtime_config_installation_status.v0.2", "state": statusState, "outcome": outcome, "code": code, "request_sha256": p.RequestSHA256, "action_intent_id": ai.ID, "policy_decision_id": pd.ID, "approval_consumed": out.ApprovalConsumed, "run_attempt_id": out.RunAttemptID, "change_knowledge": knowledge, "configuration_accepted": outcome == "completed" && obs != nil && obs.State == "installed_inactive", "application_activated": false, "service_activated": false}
	if obs != nil {
		raw, _ := json.Marshal(obs)
		out.ObservationRef, e = store.Put(raw)
		if e != nil {
			return out, runtimeConfigInstallError("observation_persistence")
		}
		status["observation_ref"] = out.ObservationRef
	}
	raw, _ := json.Marshal(status)
	ref, e := store.Put(raw)
	if e != nil {
		return out, runtimeConfigInstallError("receipt_unavailable")
	}
	now := v.now().UTC()
	var receipt envelope.Receipt
	var transition envelope.Transition
	var final work.ReceiptedActionInput
	var workStore *work.Store
	if reserved != nil {
		final = reserved.input
		final.OutcomePending = false
		final.Outcome = outcome
		final.Changed = outcome == "completed" && obs != nil && obs.Changed
		final.ChangeKnowledge = knowledge
		if final.Changed {
			for _, f := range p.Request.Files {
				final.FilesChanged = append(final.FilesChanged, f.Path)
			}
		}
		final.Summary = "runtime configuration " + p.Operation + ": " + code + "; application and service remain inactive"
		if uncertain {
			final.Summary += "; remote effects unknown; separately admitted recovery required"
		}
		final.AfterRefs = []string{ref}
		final.ProofRefs = append(append([]string(nil), reserved.input.ProofRefs...), ref, "check:closed-runtime-config-install-protocol")
		if out.ObservationRef != "" {
			final.AfterRefs = append(final.AfterRefs, out.ObservationRef)
			final.ProofRefs = append(final.ProofRefs, out.ObservationRef)
		}
		workStore, e = work.Open(v.home)
		if e == nil {
			e = workStore.SealReceiptedActionOutcome(p.WorkID, reserved.input, final, reserved.authorizedAt)
		}
		var completed work.ReceiptedActionResult
		if e == nil {
			completed, e = workStore.RecordReservedReceiptedAction(p.WorkID, final, reserved.authorizedAt)
		}
		if e != nil && !errors.Is(e, work.ErrRequiredAttemptCompletion) {
			return out, runtimeConfigInstallError("receipt_unavailable")
		}
		receipt, transition = completed.Receipt, completed.Transition
	} else {
		receipt = envelope.Receipt{Header: header(envelope.SVReceipt, idFor("rec_", ai.ID, ref), ai, now), Outcome: outcome, MoveType: "audit_only", StateDelta: envelope.StateDelta{Summary: "runtime configuration admission failed before launch", FilesChanged: []string{}, Artifacts: []string{ref}, Verification: []string{"check:closed-runtime-config-install-protocol"}}, Cost: envelope.ReceiptCost{ModelCalls: []string{}}, Skills: envelope.ReceiptSkills{Used: []string{ai.Capability}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true}, Memory: envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}}, Next: envelope.ReceiptNext{SuggestedWork: []string{}}, ProofRefs: []string{ref, ai.ID, out.RunAttemptID}}
		transition = envelope.Transition{Header: header(envelope.SVTransition, idFor("tr_", ai.ID, ref), ai, now), BeforeRefs: []string{ai.ID}, ActionRefs: []string{ai.ID}, AfterRefs: []string{ref, receipt.ID}, StateDelta: receipt.StateDelta.Summary, SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: false}}
		if recordEnvelope(v.home, receipt.ID, &receipt) != nil || recordEnvelope(v.home, transition.ID, &transition) != nil {
			return out, runtimeConfigInstallError("receipt_unavailable")
		}
	}
	out.ReceiptID, out.TransitionID = receipt.ID, transition.ID
	raStatus := runattempt.StatusFailed
	if outcome == "completed" {
		raStatus = runattempt.StatusSucceeded
	}
	if e = runtimeConfigCompleteAttempt(v.home, out.RunAttemptID, raStatus, runtimeConfigOwner(p.Operation), receipt.ID, transition.ID, now); e != nil {
		return out, runtimeConfigInstallError("attempt_completion")
	}
	if reserved != nil {
		_, e = workStore.RecordReservedReceiptedAction(p.WorkID, final, reserved.authorizedAt)
		if e != nil && !(uncertain && errors.Is(e, work.ErrRequiredAttemptCompletion)) {
			return out, runtimeConfigInstallError("reservation_completion")
		}
	}
	if outcome != "completed" {
		return out, runtimeConfigInstallError(out.State)
	}
	out.State = obs.State
	out.ConfigurationAccepted = obs.State == "installed_inactive"
	return out, nil
}
