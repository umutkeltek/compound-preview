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

const sourceMarkerRetirementOwner = "act.source_marker_retirement"
const sourceMarkerRecoveryOwner = "act.source_marker_recovery"
const sourceMarkerRetirementPlanSchema = "compound.source_marker_retirement_plan.v0.1"

type SourceMarkerRetirementPlan struct {
	Operation             string                               `json:"operation"`
	Recovery              *SourceMarkerRecoveryOrigin          `json:"recovery"`
	SchemaVersion         string                               `json:"schema_version"`
	Request               policy.SourceMarkerRetirementRequest `json:"request"`
	RequestSHA256         string                               `json:"request_sha256"`
	WorkID                string                               `json:"work_id"`
	Actor                 string                               `json:"actor"`
	WorkSHA256            string                               `json:"work_sha256"`
	ActionIntentID        string                               `json:"action_intent_id"`
	PolicyDecisionID      string                               `json:"policy_decision_id"`
	ApprovalPlanID        string                               `json:"approval_plan_id"`
	ApprovalBindingDigest string                               `json:"approval_binding_digest"`
	ApprovalExpiresAt     string                               `json:"approval_expires_at"`
}
type SourceMarkerRetirementPlanResult struct {
	PlanRef       string                     `json:"plan_ref"`
	Plan          SourceMarkerRetirementPlan `json:"plan"`
	PolicyOutcome string                     `json:"policy_outcome"`
}
type SourceMarkerRetirementResult struct {
	RecoveryWorkID       string `json:"recovery_work_id,omitempty"`
	State                string `json:"state"`
	RunAttemptID         string `json:"run_attempt_id"`
	ReceiptID            string `json:"receipt_id"`
	TransitionID         string `json:"transition_id"`
	ObservationRef       string `json:"observation_ref,omitempty"`
	ApprovalConsumed     bool   `json:"approval_consumed"`
	SourceMarkerRetired  bool   `json:"source_marker_retired"`
	ApplicationActivated bool   `json:"application_activated"`
	ServiceActivated     bool   `json:"service_activated"`
}

// Transport injection is package-private and only tests replace it. Neither a
// request nor a CLI flag can choose a program, transport, callback or validator.
type sourceMarkerRetirementTransport interface {
	Start(context.Context, policy.SourceMarkerRetirementRequest, map[string]string, string, time.Time, sourceMarkerRetirementAdmissionGuard) (sourceMarkerRetirementChild, error)
}
type sourceMarkerRetirementChild interface {
	Wait() ([]byte, error)
	Stop(context.Context) error
}

type SourceMarkerRetirer struct {
	home      string
	now       func() time.Time
	transport sourceMarkerRetirementTransport
}

func NewSourceMarkerRetirer(homePath string) (*SourceMarkerRetirer, error) {
	if !filepath.IsAbs(homePath) {
		return nil, sourceMarkerRetirementError("home")
	}
	return &SourceMarkerRetirer{home: homePath, now: time.Now, transport: sourceMarkerRetirementNativeTransport{}}, nil
}

func sourceMarkerRetirementHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func sourceMarkerRetirementError(code string) error {
	return errors.New("source_marker_retirement:" + code)
}

func (v *SourceMarkerRetirer) requireWork(workID, actor string) (envelope.WorkEnvelope, error) {
	s, e := work.OpenReadOnly(v.home)
	if e != nil {
		return envelope.WorkEnvelope{}, sourceMarkerRetirementError("work_unavailable")
	}
	w, e := s.Get(workID)
	if e != nil || actor == "" || strings.TrimSpace(actor) != actor || w.State != "running" {
		return w, sourceMarkerRetirementError("work_not_executable")
	}
	leases, e := s.ListLeases()
	if e != nil {
		return w, sourceMarkerRetirementError("lease_unavailable")
	}
	for _, l := range leases {
		if l.WorkID == workID && l.By == actor {
			return w, nil
		}
	}
	return w, sourceMarkerRetirementError("wrong_lease_owner")
}

func (v *SourceMarkerRetirer) witness(r policy.SourceMarkerRetirementRequest, now time.Time) error {
	if policy.ValidateSourceMarkerRetirementRequest(r, SourceMarkerRetirementProgramSHA256(), now) != nil {
		return sourceMarkerRetirementError("request_invalid")
	}
	return v.installationAcceptance(r)
}

func sourceMarkerRetirementIntent(r policy.SourceMarkerRetirementRequest, workID, actor, workSHA string, now time.Time) envelope.ActionIntent {
	digest := policy.SourceMarkerRetirementDigest(r)
	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: idFor("act_", workID, actor, digest, ts(now)), CreatedAt: ts(now), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: workID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier2", EvidenceRefs: []string{"request-sha256:" + policy.SourceMarkerRetirementDigest(r), "work-generation:sha256:" + workSHA, r.Installation.PlanRef, r.Installation.ReceiptID, r.Installation.TransitionID, r.Installation.ObservationRef}, Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: actor, Capability: policy.SourceMarkerRetirementCapability, Action: policy.SourceMarkerRetirementAction, Target: envelope.ActionTarget{Kind: policy.SourceMarkerRetirementTarget, ID: r.ProfileID, Path: r.Marker.Path}, Destination: policy.SourceMarkerRetirementDestination(r), ArgumentsHash: "sha256:" + digest, SchemaRevision: policy.SourceMarkerRetirementSchema, MutationClass: "security", Reversibility: "easy", BlastRadius: "host", RollbackPlan: "Preserve the original marker inode in the root-private recovery directory; separately admitted native reconciliation only, never rewrite configuration or activate services."}
	return ai
}

// Plan records a real non-allowing native action/decision and creates only the
// existing exact approval plan. It never inspects credentials or opens SSH.
func (v *SourceMarkerRetirer) Plan(r policy.SourceMarkerRetirementRequest, workID, actor string) (SourceMarkerRetirementPlanResult, error) {
	return v.plan(r, workID, actor, "retire", nil)
}

func (v *SourceMarkerRetirer) plan(r policy.SourceMarkerRetirementRequest, workID, actor, operation string, recovery *SourceMarkerRecoveryOrigin) (SourceMarkerRetirementPlanResult, error) {
	var out SourceMarkerRetirementPlanResult
	now := v.now().UTC()
	if sourceMarkerValidateRequest(r, operation, now) != nil {
		return out, sourceMarkerRetirementError("invalid_request")
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
		return out, sourceMarkerRetirementError("policy_unavailable")
	}
	ai := sourceMarkerOperationIntent(r, workID, actor, sourceMarkerRetirementHash(w), operation, recovery, now)
	pd, eligible := sourceMarkerEvaluate(ai, r, operation, recovery, cfg, now)
	if e = recordEnvelope(v.home, ai.ID, &ai); e != nil {
		return out, sourceMarkerRetirementError("intent_persistence")
	}
	if e = recordEnvelope(v.home, pd.ID, &pd); e != nil {
		return out, sourceMarkerRetirementError("decision_persistence")
	}
	out.PolicyOutcome = pd.Outcome
	if pd.Outcome != "hold" || !eligible {
		return out, sourceMarkerRetirementError("admission_denied")
	}
	ap, _, _, e := policy.NewExactActionApprovalStore(v.home).EnsurePlanForIntent(ai, cfg, policy.SourceMarkerRetirementApprovalTTL, now)
	if e != nil {
		return out, sourceMarkerRetirementError("approval_plan_unavailable")
	}
	p := SourceMarkerRetirementPlan{Operation: operation, Recovery: recovery, SchemaVersion: sourceMarkerRetirementPlanSchema, Request: r, RequestSHA256: policy.SourceMarkerRetirementDigest(r), WorkID: workID, Actor: actor, WorkSHA256: sourceMarkerRetirementHash(w), ActionIntentID: ai.ID, PolicyDecisionID: pd.ID, ApprovalPlanID: ap.ID, ApprovalBindingDigest: ap.BindingDigest, ApprovalExpiresAt: ap.ExpiresAt}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return out, sourceMarkerRetirementError("artifact_store")
	}
	raw, _ := json.Marshal(p)
	ref, e := store.Put(raw)
	if e != nil {
		return out, sourceMarkerRetirementError("plan_persistence")
	}
	out.PlanRef = ref
	out.Plan = p
	return out, nil
}

// Historical recovery plans omitted only the transport pin. Validate a copy of
// that exact legacy shape; the immutable bytes and intent remain unchanged and
// are authenticated by loadPlan. Fresh execution still requires a current pin.
func sourceMarkerPlanRequiredFields(raw json.RawMessage, p SourceMarkerRetirementPlan) bool {
	if credentialVerifyRequiredFields(raw, reflect.TypeOf(p)) {
		return true
	}
	if p.Operation != "reconcile" || p.Recovery == nil || p.Request.ProgramSHA256 != sourceMarkerLegacyProgramSHA256 || p.Recovery.TransportProgramSHA256 != "" || p.Recovery.RequestSHA256 != p.RequestSHA256 || p.WorkID != sourceMarkerRecoveryWorkID(p.Recovery.ActionID, p.Recovery.AttemptID) {
		return false
	}
	var fields map[string]json.RawMessage
	var recovery map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["recovery"], &recovery) != nil || recovery == nil {
		return false
	}
	if _, present := recovery["transport_program_sha256"]; present {
		return false
	}
	recovery["transport_program_sha256"] = json.RawMessage(`""`)
	fields["recovery"], _ = json.Marshal(recovery)
	shape, err := json.Marshal(fields)
	return err == nil && credentialVerifyRequiredFields(shape, reflect.TypeOf(p))
}

func (v *SourceMarkerRetirer) loadPlan(ref, actor string) (SourceMarkerRetirementPlan, envelope.ActionIntent, error) {
	var p SourceMarkerRetirementPlan
	var ai envelope.ActionIntent
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return p, ai, sourceMarkerRetirementError("artifact_store")
	}
	raw, e := store.Get(ref)
	if e != nil || len(raw) > 65536 {
		return p, ai, sourceMarkerRetirementError("plan_unavailable")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.SchemaVersion != sourceMarkerRetirementPlanSchema || p.Actor != actor || p.RequestSHA256 != policy.SourceMarkerRetirementDigest(p.Request) {
		return p, ai, sourceMarkerRetirementError("plan_binding")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || !credentialVerifyUniqueJSON(raw) || !sourceMarkerPlanRequiredFields(raw, p) {
		return p, ai, sourceMarkerRetirementError("plan_binding")
	}
	events, e := eventstore.Open(v.home)
	if e != nil {
		return p, ai, sourceMarkerRetirementError("intent_unavailable")
	}
	record, e := events.Read(p.ActionIntentID)
	if e != nil || json.Unmarshal(record.Bytes, &ai) != nil {
		return p, ai, sourceMarkerRetirementError("intent_unavailable")
	}
	created, e := time.Parse(time.RFC3339Nano, ai.CreatedAt)
	if e != nil {
		return p, ai, sourceMarkerRetirementError("intent_binding")
	}
	if !sourceMarkerOperationValid(p.Operation, p.Recovery) {
		return p, ai, sourceMarkerRetirementError("operation_binding")
	}
	expected := sourceMarkerOperationIntent(p.Request, p.WorkID, p.Actor, p.WorkSHA256, p.Operation, p.Recovery, created)
	if ai.ID != expected.ID || policy.CanonicalFullIntentDigest(ai) != policy.CanonicalFullIntentDigest(expected) {
		return p, ai, sourceMarkerRetirementError("intent_binding")
	}
	var initial envelope.PolicyDecision
	decision, e := events.Read(p.PolicyDecisionID)
	if e != nil || json.Unmarshal(decision.Bytes, &initial) != nil || initial.ID != p.PolicyDecisionID || initial.ActionIntentID != ai.ID || initial.Outcome != "hold" {
		return p, ai, sourceMarkerRetirementError("plan_decision_binding")
	}
	var approval policy.ExactActionApprovalPlan
	record, e = events.Read(p.ApprovalPlanID)
	if e != nil || json.Unmarshal(record.Bytes, &approval) != nil || approval.SchemaVersion != policy.ExactActionApprovalPlanSchema || approval.ID != p.ApprovalPlanID || approval.ActionIntentID != ai.ID || approval.BindingDigest != p.ApprovalBindingDigest || approval.ExpiresAt != p.ApprovalExpiresAt || policy.ActionBindingDigest(approval.Binding) != approval.BindingDigest {
		return p, ai, sourceMarkerRetirementError("plan_approval_binding")
	}
	issued, ie := time.Parse(time.RFC3339Nano, approval.CreatedAt)
	expires, ee := time.Parse(time.RFC3339Nano, approval.ExpiresAt)
	if ie != nil || ee != nil || !expires.After(issued) || expires.Sub(issued) > policy.SourceMarkerRetirementApprovalTTL {
		return p, ai, sourceMarkerRetirementError("plan_approval_binding")
	}
	return p, ai, nil
}

func (v *SourceMarkerRetirer) current(p SourceMarkerRetirementPlan, ai envelope.ActionIntent, workSHA string) (policy.Config, envelope.PolicyDecision, error) {
	var cfg policy.Config
	var pd envelope.PolicyDecision
	now := v.now().UTC()
	if sourceMarkerValidateRequest(p.Request, p.Operation, now) != nil {
		return cfg, pd, sourceMarkerRetirementError("request_stale")
	}
	w, e := v.requireWork(p.WorkID, p.Actor)
	if e != nil {
		return cfg, pd, e
	}
	if sourceMarkerRetirementHash(w) != workSHA {
		return cfg, pd, sourceMarkerRetirementError("work_generation_changed")
	}
	if e = v.planAuthority(p.Request, p.Operation, p.Recovery, p.WorkID, now); e != nil {
		return cfg, pd, e
	}
	halted, _, e := policy.AutonomyHalted(v.home, ai.MutationClass)
	if e != nil || halted {
		return cfg, pd, sourceMarkerRetirementError("autonomy_halted")
	}
	cfg, _, e = policy.LoadConfig(v.home)
	if e != nil {
		return cfg, pd, sourceMarkerRetirementError("policy_unavailable")
	}
	pd, eligible := sourceMarkerEvaluate(ai, p.Request, p.Operation, p.Recovery, cfg, now)
	if !eligible || pd.Outcome != "hold" {
		return cfg, pd, sourceMarkerRetirementError("admission_denied")
	}
	if policy.ActionBindingDigest(policy.ActionBindingForIntent(ai, cfg)) != p.ApprovalBindingDigest {
		return cfg, pd, sourceMarkerRetirementError("policy_generation_changed")
	}
	return cfg, pd, nil
}

// Execute takes only an immutable native plan reference, never an allowing
// decision supplied by the caller. Full current approval authority is re-read.
type sourceMarkerRetirementReservation struct {
	input        work.ReceiptedActionInput
	authorizedAt time.Time
	launched     bool
}

func (v *SourceMarkerRetirer) Execute(ctx context.Context, planRef, actor string) (out SourceMarkerRetirementResult, err error) {
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
	var reserved *sourceMarkerRetirementReservation
	finish := func(outcome string, obs *SourceMarkerRetirementObservation, code string) (SourceMarkerRetirementResult, error) {
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
			return sourceMarkerRetirementError("work_deadline")
		}
		attemptTime, _ := time.Parse("20060102t150405z", p.Request.AttemptID[:16])
		attemptID := runattempt.MintDeterministic("source-marker-retirement:"+p.Request.AttemptID, attemptTime)
		if p.Operation != "retire" {
			attemptID = runattempt.MintDeterministic("source-marker-recovery:"+p.ActionIntentID, attemptTime)
		}
		if _, e = attempts.Get(attemptID); e == nil {
			return sourceMarkerRetirementError("attempt_already_used")
		} else if !errors.Is(e, runattempt.ErrNotFound) {
			return sourceMarkerRetirementError("attempt_authority_unavailable")
		}
		if preflightEventStore(v.home) != nil {
			return sourceMarkerRetirementError("event_store_unavailable")
		}
		var matched bool
		pd, matched, e = policy.NewExactActionApprovalStore(v.home).ApplyMatchingLocked(ai, cfg, base, v.now().UTC())
		if e != nil || !matched || !policy.Allowed(pd) {
			return sourceMarkerRetirementError("exact_approval_required")
		}
		out.ApprovalConsumed = true
		if e = recordEnvelope(v.home, pd.ID, &pd); e != nil {
			return sourceMarkerRetirementError("approved_decision_persistence")
		}
		attempt, e = attempts.MintWithID(attemptID, p.WorkID, p.Actor, "compound", v.now().UTC())
		if e != nil {
			return sourceMarkerRetirementError("attempt_persistence")
		}
		out.RunAttemptID = attempt.ID
		if e = attempts.AttachActionIntent(attempt.ID, ai.ID); e != nil {
			return sourceMarkerRetirementError("attempt_binding")
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
	pending := work.ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: w.State, OutcomePending: true, Outcome: "blocked", Changed: false, Summary: "awaiting bounded inactive source-comparison marker retirement installation evidence", MoveType: "audit_only", ProofRefs: []string{ai.ID, attempt.ID, "policy-decision:" + pd.ID}, BeforeRefs: []string{"request-sha256:" + p.RequestSHA256, "request-sha256:" + p.RequestSHA256}, FilesChanged: []string{}}
	pending.RequiredAttemptCompletion = &work.RequiredAttemptCompletion{AttemptID: attempt.ID, Owner: sourceMarkerOwner(p.Operation)}
	if p.Operation == "retire" {
		out.RecoveryWorkID = sourceMarkerRecoveryWorkID(ai.ID, attempt.ID)
		pending.RequiredAttemptCompletion.Recovery = &work.RequiredRecoveryCompletion{WorkID: out.RecoveryWorkID, Owner: sourceMarkerRecoveryOwner, Capability: policy.SourceMarkerRecoveryCapability, ArgumentsHashes: []string{
			"sha256:" + policy.SourceMarkerRecoveryDigest(p.Request, ai.ID, attempt.ID, "reconcile"),
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
			return sourceMarkerRetirementError("approval_consumption_unavailable")
		}
		if workCtx.Err() != nil || !v.now().Before(started.Add(50*time.Second)) {
			return sourceMarkerRetirementError("work_deadline")
		}
		return nil
	}
	if e = s.ReserveReceiptedAction(p.WorkID, pending, authorizedAt, authorize); e != nil {
		return finish("failed", nil, "action_reservation")
	}
	reserved = &sourceMarkerRetirementReservation{input: pending, authorizedAt: authorizedAt}
	// Exact-approval planning already published this metadata-only ActionIntent;
	// the native reauthorization API deliberately refuses published actions.
	// Read the work owner's validated reservation under native admission instead.
	guard := func(start func() error) error {
		if start == nil {
			return sourceMarkerRetirementError("launch_callback")
		}
		return home.WithAutonomyAdmissionLock(v.home, func() error {
			input, at, present, e := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
			if e != nil || !present || !at.Equal(authorizedAt) || sourceMarkerRetirementHash(input) != sourceMarkerRetirementHash(pending) {
				return sourceMarkerRetirementError("work_reservation_drift")
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
	obs, e := decodeSourceMarkerRetirementObservation(raw, p.Request)
	if e != nil || obs.Operation != p.Operation {
		return finish("failed", nil, "invalid_observation")
	}
	if (p.Operation == "retire" || p.Operation == "reconcile") && obs.State != "source_marker_retired" {
		return finish("failed", &obs, "recovery_required")
	}
	result, finishErr := finish("completed", &obs, obs.State)
	if finishErr == nil && p.Operation != "retire" {
		finishErr = v.clearRecoveredOriginal(*p.Recovery)
	}
	return result, finishErr
}

func (v *SourceMarkerRetirer) finish(out SourceMarkerRetirementResult, p SourceMarkerRetirementPlan, ai envelope.ActionIntent, pd envelope.PolicyDecision, outcome string, obs *SourceMarkerRetirementObservation, code string, reserved *sourceMarkerRetirementReservation, cleanup context.Context) (SourceMarkerRetirementResult, error) {
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
		return out, sourceMarkerRetirementError("cleanup_deadline")
	}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return out, sourceMarkerRetirementError("receipt_unavailable")
	}
	knowledge := ""
	if uncertain {
		knowledge = "unknown"
	}
	statusState := out.State
	if outcome == "completed" && obs != nil {
		statusState = obs.State
	}
	status := map[string]any{"schema_version": "compound.source_marker_retirement_status.v0.1", "state": statusState, "outcome": outcome, "code": code, "request_sha256": p.RequestSHA256, "action_intent_id": ai.ID, "policy_decision_id": pd.ID, "approval_consumed": out.ApprovalConsumed, "run_attempt_id": out.RunAttemptID, "change_knowledge": knowledge, "source_marker_retired": outcome == "completed" && obs != nil && obs.State == "source_marker_retired", "application_activated": false, "service_activated": false}
	if obs != nil {
		raw, _ := json.Marshal(obs)
		out.ObservationRef, e = store.Put(raw)
		if e != nil {
			return out, sourceMarkerRetirementError("observation_persistence")
		}
		status["observation_ref"] = out.ObservationRef
	}
	raw, _ := json.Marshal(status)
	ref, e := store.Put(raw)
	if e != nil {
		return out, sourceMarkerRetirementError("receipt_unavailable")
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
			final.FilesChanged = append(final.FilesChanged, p.Request.Marker.Path, filepath.Join(p.Request.Root.Path, p.Request.RecoveryBasename, p.Request.Marker.Basename+".preimage"))
		}
		final.Summary = "source-comparison marker retirement " + p.Operation + ": " + code + "; application and service remain inactive"
		if uncertain {
			final.Summary += "; remote effects unknown; separately admitted recovery required"
		}
		final.AfterRefs = []string{ref}
		final.ProofRefs = append(append([]string(nil), reserved.input.ProofRefs...), ref, "check:closed-source-marker-retirement-protocol")
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
			return out, sourceMarkerRetirementError("receipt_unavailable")
		}
		receipt, transition = completed.Receipt, completed.Transition
	} else {
		receipt = envelope.Receipt{Header: header(envelope.SVReceipt, idFor("rec_", ai.ID, ref), ai, now), Outcome: outcome, MoveType: "audit_only", StateDelta: envelope.StateDelta{Summary: "source-comparison marker retirement admission failed before launch", FilesChanged: []string{}, Artifacts: []string{ref}, Verification: []string{"check:closed-source-marker-retirement-protocol"}}, Cost: envelope.ReceiptCost{ModelCalls: []string{}}, Skills: envelope.ReceiptSkills{Used: []string{ai.Capability}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true}, Memory: envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}}, Next: envelope.ReceiptNext{SuggestedWork: []string{}}, ProofRefs: []string{ref, ai.ID, out.RunAttemptID}}
		transition = envelope.Transition{Header: header(envelope.SVTransition, idFor("tr_", ai.ID, ref), ai, now), BeforeRefs: []string{ai.ID}, ActionRefs: []string{ai.ID}, AfterRefs: []string{ref, receipt.ID}, StateDelta: receipt.StateDelta.Summary, SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: false}}
		if recordEnvelope(v.home, receipt.ID, &receipt) != nil || recordEnvelope(v.home, transition.ID, &transition) != nil {
			return out, sourceMarkerRetirementError("receipt_unavailable")
		}
	}
	out.ReceiptID, out.TransitionID = receipt.ID, transition.ID
	raStatus := runattempt.StatusFailed
	if outcome == "completed" {
		raStatus = runattempt.StatusSucceeded
	}
	if e = sourceMarkerCompleteAttempt(v.home, out.RunAttemptID, raStatus, sourceMarkerOwner(p.Operation), receipt.ID, transition.ID, now); e != nil {
		return out, sourceMarkerRetirementError("attempt_completion")
	}
	if reserved != nil {
		_, e = workStore.RecordReservedReceiptedAction(p.WorkID, final, reserved.authorizedAt)
		if e != nil && !(uncertain && errors.Is(e, work.ErrRequiredAttemptCompletion)) {
			return out, sourceMarkerRetirementError("reservation_completion")
		}
	}
	if outcome != "completed" {
		return out, sourceMarkerRetirementError(out.State)
	}
	out.State = obs.State
	out.SourceMarkerRetired = obs.State == "source_marker_retired"
	return out, nil
}
