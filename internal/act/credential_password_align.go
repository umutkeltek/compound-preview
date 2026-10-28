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

const credentialPasswordAlignOwner = "act.credential_password_alignment"
const credentialPasswordAlignPlanSchema = "compound.credential_password_alignment_plan.v0.1"

type CredentialPasswordAlignmentPlan struct {
	SchemaVersion         string                                    `json:"schema_version"`
	Request               policy.CredentialPasswordAlignmentRequest `json:"request"`
	RequestSHA256         string                                    `json:"request_sha256"`
	WorkID                string                                    `json:"work_id"`
	Actor                 string                                    `json:"actor"`
	WorkSHA256            string                                    `json:"work_sha256"`
	ActionIntentID        string                                    `json:"action_intent_id"`
	PolicyDecisionID      string                                    `json:"policy_decision_id"`
	ApprovalPlanID        string                                    `json:"approval_plan_id"`
	ApprovalBindingDigest string                                    `json:"approval_binding_digest"`
	ApprovalExpiresAt     string                                    `json:"approval_expires_at"`
}
type CredentialPasswordAlignmentPlanResult struct {
	PlanRef       string                          `json:"plan_ref"`
	Plan          CredentialPasswordAlignmentPlan `json:"plan"`
	PolicyOutcome string                          `json:"policy_outcome"`
}
type CredentialPasswordAlignmentResult struct {
	RecoveryWorkID            string `json:"recovery_work_id,omitempty"`
	ApplicationActivated      bool   `json:"application_activated"`
	ServiceActivated          bool   `json:"service_activated"`
	State                     string `json:"state"`
	RunAttemptID              string `json:"run_attempt_id"`
	ReceiptID                 string `json:"receipt_id"`
	TransitionID              string `json:"transition_id"`
	ObservationRef            string `json:"observation_ref,omitempty"`
	ApprovalConsumed          bool   `json:"approval_consumed"`
	ConfigurationAccepted     bool   `json:"configuration_accepted"`
	ApplicationAccessAccepted bool   `json:"application_access_accepted"`
	SourceCaptureAccepted     bool   `json:"source_capture_accepted"`
	MigrationAccepted         bool   `json:"migration_accepted"`
}

// Transport injection is package-private and only tests replace it. Neither a
// request nor a CLI flag can choose a program, transport, callback or validator.
type credentialPasswordAlignTransport interface {
	Start(context.Context, policy.CredentialPasswordAlignmentRequest, string, time.Time, credentialPasswordAlignAdmissionGuard) (credentialPasswordAlignChild, error)
}
type credentialPasswordAlignChild interface {
	Wait() ([]byte, error)
	Stop(context.Context) error
}

type CredentialPasswordAligner struct {
	home      string
	now       func() time.Time
	transport credentialPasswordAlignTransport
	// Package-private scan seam; no request or CLI can select it.
	fenceScan   func(eventstore.ScanOptions) (eventstore.ScanPage, error)
	fenceCursor string
	fenceIDs    map[string]struct{}
}

func NewCredentialPasswordAligner(homePath string) (*CredentialPasswordAligner, error) {
	if homePath == "" || !filepath.IsAbs(homePath) || filepath.Clean(homePath) != homePath {
		return nil, credentialPasswordAlignError("explicit_home_required")
	}
	resolved, e := home.Resolve(homePath)
	if e != nil {
		return nil, credentialPasswordAlignError("home_unavailable")
	}
	return &CredentialPasswordAligner{home: resolved, now: time.Now, transport: credentialPasswordAlignNativeTransport{}}, nil
}

func credentialPasswordAlignHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func credentialPasswordAlignError(code string) error {
	return errors.New("credential_password_align:" + code)
}

func (v *CredentialPasswordAligner) requireWork(workID, actor string) (envelope.WorkEnvelope, error) {
	s, e := work.OpenReadOnly(v.home)
	if e != nil {
		return envelope.WorkEnvelope{}, credentialPasswordAlignError("work_unavailable")
	}
	w, e := s.Get(workID)
	if e != nil || actor == "" || strings.TrimSpace(actor) != actor || w.State != "running" {
		return w, credentialPasswordAlignError("work_not_executable")
	}
	leases, e := s.ListLeases()
	if e != nil {
		return w, credentialPasswordAlignError("lease_unavailable")
	}
	for _, l := range leases {
		if l.WorkID == workID && l.By == actor {
			return w, nil
		}
	}
	return w, credentialPasswordAlignError("wrong_lease_owner")
}

func (v *CredentialPasswordAligner) witness(r policy.CredentialPasswordAlignmentRequest, now time.Time) error {
	return v.alignmentAuthority(r, now)
}

func credentialPasswordAlignmentIntent(r policy.CredentialPasswordAlignmentRequest, workID, actor, workSHA string, now time.Time) envelope.ActionIntent {
	digest := policy.CredentialPasswordAlignmentDigest(r)
	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: idFor("act_", workID, actor, digest, ts(now)), CreatedAt: ts(now), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: workID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier2", EvidenceRefs: []string{r.Witness.Ref, "witness-sha256:" + r.Witness.SHA256, "work-generation:sha256:" + workSHA, "request-sha256:" + digest}, Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: actor, Capability: policy.CredentialPasswordAlignmentCapability(r), Action: policy.CredentialPasswordAlignmentAction(r), Target: envelope.ActionTarget{Kind: policy.CredentialPasswordAlignTarget, ID: r.ProfileID, Path: r.File.Path}, Destination: policy.CredentialVerificationDestination(r.CredentialVerificationRequest), ArgumentsHash: "sha256:" + policy.CredentialPasswordAlignmentActionDigest(r), SchemaRevision: policy.CredentialPasswordAlignSchema, MutationClass: "security", Reversibility: "manual", BlastRadius: "host", RollbackPlan: "Retain root-only durable old/new SCRAM custody and the original failed attempt. Separately approved fixed reconcile or exact-postimage restore only; keep all database and runtime fences closed."}
	ai.RequiresExactApproval = true
	return ai
}

// Plan records a real non-allowing native action/decision and creates only the
// existing exact approval plan. It never inspects credentials or opens SSH.
func (v *CredentialPasswordAligner) Plan(r policy.CredentialPasswordAlignmentRequest, workID, actor string) (CredentialPasswordAlignmentPlanResult, error) {
	var out CredentialPasswordAlignmentPlanResult
	now := v.now().UTC()
	if policy.ValidateCredentialPasswordAlignmentRequest(r, CredentialPasswordAlignmentProgramSHA256(), CredentialVerificationProgramSHA256(), now) != nil {
		return out, credentialPasswordAlignError("invalid_request")
	}
	w, e := v.requireWork(workID, actor)
	if e != nil {
		return out, e
	}
	if r.Origin != nil && workID != credentialPasswordAlignRecoveryWorkID(r.Origin.ActionID, r.Origin.AttemptID) {
		return out, credentialPasswordAlignError("recovery_work_binding")
	}
	if e = v.witness(r, now); e != nil {
		return out, e
	}
	cfg, _, e := policy.LoadConfig(v.home)
	if e != nil {
		return out, credentialPasswordAlignError("policy_unavailable")
	}
	ai := credentialPasswordAlignmentIntent(r, workID, actor, credentialPasswordAlignHash(w), now)
	if e = v.priorAlignmentFence(r, ai.ID); e != nil {
		return out, e
	}
	if e = v.priorRecoveryFence(r, ai.ID); e != nil {
		return out, e
	}
	pd, eligible := policy.EvaluateCredentialPasswordAlignment(ai, r, CredentialPasswordAlignmentProgramSHA256(), CredentialVerificationProgramSHA256(), cfg, policy.EvaluationContext{}, now)
	if e = recordEnvelope(v.home, ai.ID, &ai); e != nil {
		return out, credentialPasswordAlignError("intent_persistence")
	}
	if e = recordEnvelope(v.home, pd.ID, &pd); e != nil {
		return out, credentialPasswordAlignError("decision_persistence")
	}
	out.PolicyOutcome = pd.Outcome
	if pd.Outcome != "hold" || !eligible {
		return out, credentialPasswordAlignError("admission_denied")
	}
	ap, _, _, e := policy.NewExactActionApprovalStore(v.home).EnsurePlanForIntent(ai, cfg, policy.CredentialVerifyApprovalTTL, now)
	if e != nil {
		return out, credentialPasswordAlignError("approval_plan_unavailable")
	}
	p := CredentialPasswordAlignmentPlan{SchemaVersion: credentialPasswordAlignPlanSchema, Request: r, RequestSHA256: policy.CredentialPasswordAlignmentDigest(r), WorkID: workID, Actor: actor, WorkSHA256: credentialPasswordAlignHash(w), ActionIntentID: ai.ID, PolicyDecisionID: pd.ID, ApprovalPlanID: ap.ID, ApprovalBindingDigest: ap.BindingDigest, ApprovalExpiresAt: ap.ExpiresAt}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return out, credentialPasswordAlignError("artifact_store")
	}
	raw, _ := json.Marshal(p)
	ref, e := store.Put(raw)
	if e != nil {
		return out, credentialPasswordAlignError("plan_persistence")
	}
	out.PlanRef = ref
	out.Plan = p
	return out, nil
}

func (v *CredentialPasswordAligner) loadPlan(ref, actor string) (CredentialPasswordAlignmentPlan, envelope.ActionIntent, error) {
	var p CredentialPasswordAlignmentPlan
	var ai envelope.ActionIntent
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return p, ai, credentialPasswordAlignError("artifact_store")
	}
	raw, e := store.Get(ref)
	if e != nil || len(raw) > 65536 {
		return p, ai, credentialPasswordAlignError("plan_unavailable")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.SchemaVersion != credentialPasswordAlignPlanSchema || p.Actor != actor || p.RequestSHA256 != policy.CredentialPasswordAlignmentDigest(p.Request) {
		return p, ai, credentialPasswordAlignError("plan_binding")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || !credentialVerifyUniqueJSON(raw) || !credentialPasswordAlignRequiredFields(raw, reflect.TypeOf(p)) {
		return p, ai, credentialPasswordAlignError("plan_binding")
	}
	events, e := eventstore.Open(v.home)
	if e != nil {
		return p, ai, credentialPasswordAlignError("intent_unavailable")
	}
	record, e := events.Read(p.ActionIntentID)
	if e != nil || json.Unmarshal(record.Bytes, &ai) != nil {
		return p, ai, credentialPasswordAlignError("intent_unavailable")
	}
	created, e := time.Parse(time.RFC3339Nano, ai.CreatedAt)
	if e != nil {
		return p, ai, credentialPasswordAlignError("intent_binding")
	}
	expected := credentialPasswordAlignmentIntent(p.Request, p.WorkID, p.Actor, p.WorkSHA256, created)
	if ai.ID != expected.ID || policy.CanonicalFullIntentDigest(ai) != policy.CanonicalFullIntentDigest(expected) {
		return p, ai, credentialPasswordAlignError("intent_binding")
	}
	var initial envelope.PolicyDecision
	decision, e := events.Read(p.PolicyDecisionID)
	if e != nil || json.Unmarshal(decision.Bytes, &initial) != nil || initial.ID != p.PolicyDecisionID || initial.ActionIntentID != ai.ID || initial.Outcome != "hold" {
		return p, ai, credentialPasswordAlignError("plan_decision_binding")
	}
	var approval policy.ExactActionApprovalPlan
	record, e = events.Read(p.ApprovalPlanID)
	if e != nil || json.Unmarshal(record.Bytes, &approval) != nil || approval.SchemaVersion != policy.ExactActionApprovalPlanSchema || approval.ID != p.ApprovalPlanID || approval.ActionIntentID != ai.ID || approval.BindingDigest != p.ApprovalBindingDigest || approval.ExpiresAt != p.ApprovalExpiresAt || policy.ActionBindingDigest(approval.Binding) != approval.BindingDigest {
		return p, ai, credentialPasswordAlignError("plan_approval_binding")
	}
	issued, ie := time.Parse(time.RFC3339Nano, approval.CreatedAt)
	expires, ee := time.Parse(time.RFC3339Nano, approval.ExpiresAt)
	if ie != nil || ee != nil || !expires.After(issued) || expires.Sub(issued) > policy.CredentialVerifyApprovalTTL {
		return p, ai, credentialPasswordAlignError("plan_approval_binding")
	}
	return p, ai, nil
}

func (v *CredentialPasswordAligner) current(p CredentialPasswordAlignmentPlan, ai envelope.ActionIntent, workSHA string) (policy.Config, envelope.PolicyDecision, error) {
	var cfg policy.Config
	var pd envelope.PolicyDecision
	now := v.now().UTC()
	if p.Request.Origin != nil && p.WorkID != credentialPasswordAlignRecoveryWorkID(p.Request.Origin.ActionID, p.Request.Origin.AttemptID) {
		return cfg, pd, credentialPasswordAlignError("recovery_work_binding")
	}
	if policy.ValidateCredentialPasswordAlignmentRequest(p.Request, CredentialPasswordAlignmentProgramSHA256(), CredentialVerificationProgramSHA256(), now) != nil {
		return cfg, pd, credentialPasswordAlignError("request_stale")
	}
	w, e := v.requireWork(p.WorkID, p.Actor)
	if e != nil {
		return cfg, pd, e
	}
	if credentialPasswordAlignHash(w) != workSHA {
		return cfg, pd, credentialPasswordAlignError("work_generation_changed")
	}
	if e = v.witness(p.Request, now); e != nil {
		return cfg, pd, e
	}
	if e = v.priorAlignmentFence(p.Request, ai.ID); e != nil {
		return cfg, pd, e
	}
	if e = v.priorRecoveryFence(p.Request, ai.ID); e != nil {
		return cfg, pd, e
	}
	halted, _, e := policy.AutonomyHalted(v.home, ai.MutationClass)
	if e != nil || halted {
		return cfg, pd, credentialPasswordAlignError("autonomy_halted")
	}
	cfg, _, e = policy.LoadConfig(v.home)
	if e != nil {
		return cfg, pd, credentialPasswordAlignError("policy_unavailable")
	}
	pd, eligible := policy.EvaluateCredentialPasswordAlignment(ai, p.Request, CredentialPasswordAlignmentProgramSHA256(), CredentialVerificationProgramSHA256(), cfg, policy.EvaluationContext{}, now)
	if !eligible || pd.Outcome != "hold" {
		return cfg, pd, credentialPasswordAlignError("admission_denied")
	}
	if policy.ActionBindingDigest(policy.ActionBindingForIntent(ai, cfg)) != p.ApprovalBindingDigest {
		return cfg, pd, credentialPasswordAlignError("policy_generation_changed")
	}
	return cfg, pd, nil
}

// Execute takes only an immutable native plan reference, never an allowing
// decision supplied by the caller. Full current approval authority is re-read.
type credentialPasswordAlignReservation struct {
	input        work.ReceiptedActionInput
	authorizedAt time.Time
	launched     bool
}

func (v *CredentialPasswordAligner) Execute(ctx context.Context, planRef, actor string) (out CredentialPasswordAlignmentResult, err error) {
	started := v.now().UTC()
	outer, outerCancel := context.WithDeadline(ctx, started.Add(90*time.Second))
	defer outerCancel()
	workCtx, workCancel := context.WithDeadline(outer, started.Add(80*time.Second))
	defer workCancel()
	p, ai, e := v.loadPlan(planRef, actor)
	if e != nil {
		return out, e
	}
	unlock, e := v.executionLock(ai.ID)
	if e != nil {
		return out, e
	}
	defer unlock()
	if resumed, handled, resumeErr := v.resumeFinalization(p, ai, outer); handled {
		return resumed, resumeErr
	}
	var pd envelope.PolicyDecision
	var attempt runattempt.RunAttempt
	var reserved *credentialPasswordAlignReservation
	var fence *credentialPasswordAlignmentFence

	finish := func(outcome string, obs *CredentialPasswordAlignmentObservation, code string) (CredentialPasswordAlignmentResult, error) {
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
			return credentialPasswordAlignError("work_deadline")
		}
		attemptTime, _ := time.Parse("20060102t150405z", p.Request.AttemptID[:16])
		attemptID := runattempt.MintDeterministic("credential-password-align:"+p.Request.AttemptID, attemptTime)
		if _, e = attempts.Get(attemptID); e == nil {
			return credentialPasswordAlignError("attempt_already_used")
		} else if !errors.Is(e, runattempt.ErrNotFound) {
			return credentialPasswordAlignError("attempt_authority_unavailable")
		}
		if preflightEventStore(v.home) != nil {
			return credentialPasswordAlignError("event_store_unavailable")
		}
		var matched bool
		pd, matched, e = policy.NewExactActionApprovalStore(v.home).ApplyMatchingLocked(ai, cfg, base, v.now().UTC())
		if e != nil || !matched || !policy.Allowed(pd) {
			return credentialPasswordAlignError("exact_approval_required")
		}
		out.ApprovalConsumed = true
		if e = recordEnvelope(v.home, pd.ID, &pd); e != nil {
			return credentialPasswordAlignError("approved_decision_persistence")
		}
		attempt, e = attempts.MintWithID(attemptID, p.WorkID, p.Actor, "compound", v.now().UTC())
		if e != nil {
			return credentialPasswordAlignError("attempt_persistence")
		}
		out.RunAttemptID = attempt.ID
		if e = attempts.AttachActionIntent(attempt.ID, ai.ID); e != nil {
			return credentialPasswordAlignError("attempt_binding")
		}
		if p.Request.Operation != "inspect" {
			f := newCredentialPasswordAlignmentFence(p, ai, pd, attempt.ID, v.now().UTC())
			f.PreviousRecoveryActionID, e = v.previousRecovery(p.Request)
			if e != nil {
				return e
			}
			if e = recordEnvelope(v.home, f.ID, &f); e != nil {
				return credentialPasswordAlignError("attempt_fence_persistence")
			}
			fence = &f
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
	pending := work.ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: w.State, OutcomePending: true, Outcome: "blocked", Changed: false, Summary: "awaiting closed password operation evidence", MoveType: "control_plane_repair", ProofRefs: []string{ai.ID, attempt.ID, "policy-decision:" + pd.ID}, BeforeRefs: []string{p.Request.Witness.Ref, "request-sha256:" + p.RequestSHA256}, FilesChanged: []string{}}
	pending.RequiredAttemptCompletion = &work.RequiredAttemptCompletion{AttemptID: attempt.ID, Owner: credentialPasswordAlignOwnerFor(p.Request.Operation)}
	if origin := p.Request.Origin; origin != nil {
		pending.ProofRefs = append(pending.ProofRefs, origin.AttemptID, origin.ActionID, "sha256:"+origin.RequestSHA256, origin.ReceiptID, origin.TransitionID)
	}
	if p.Request.Operation == "align" {
		out.RecoveryWorkID = credentialPasswordAlignRecoveryWorkID(ai.ID, attempt.ID)
		pending.RequiredAttemptCompletion.Recovery = &work.RequiredRecoveryCompletion{WorkID: out.RecoveryWorkID, Owner: credentialPasswordAlignRecoveryOwner, Capability: "credential.existing_closed_postgres.recover_passwords", ArgumentsHashes: []string{"sha256:" + policy.CredentialPasswordAlignmentRecoveryDigest(p.Request, ai.ID, attempt.ID, "reconcile"), "sha256:" + policy.CredentialPasswordAlignmentRecoveryDigest(p.Request, ai.ID, attempt.ID, "restore")}}
	}
	authorizedAt := v.now().UTC()
	authorize := func() error {
		if _, _, e := v.current(p, ai, p.WorkSHA256); e != nil {
			return e
		}
		if policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, pd) != nil {
			return credentialPasswordAlignError("approval_consumption_unavailable")
		}
		if fence != nil {
			var observed credentialPasswordAlignmentFence
			if e := v.event(fence.ID, &observed); e != nil || credentialPasswordAlignHash(observed) != credentialPasswordAlignHash(*fence) {
				return credentialPasswordAlignError("attempt_fence_drift")
			}
		}
		if workCtx.Err() != nil || !v.now().Before(started.Add(80*time.Second)) {
			return credentialPasswordAlignError("work_deadline")
		}
		return nil
	}
	if e = s.ReserveReceiptedAction(p.WorkID, pending, authorizedAt, authorize); e != nil {
		return finish("failed", nil, "action_reservation")
	}
	reserved = &credentialPasswordAlignReservation{input: pending, authorizedAt: authorizedAt}
	// Exact-approval planning already published this metadata-only ActionIntent;
	// the native reauthorization API deliberately refuses published actions.
	// Read the work owner's validated reservation under native admission instead.
	guard := func(start func() error) error {
		if start == nil {
			return credentialPasswordAlignError("launch_callback")
		}
		return home.WithAutonomyAdmissionLock(v.home, func() error {
			input, at, present, e := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
			if e != nil || !present || !at.Equal(authorizedAt) || credentialPasswordAlignHash(input) != credentialPasswordAlignHash(pending) {
				return credentialPasswordAlignError("work_reservation_drift")
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
				return start()
			})
			return e
		})
	}
	child, e := v.transport.Start(workCtx, p.Request, ai.ID, started.Add(80*time.Second), guard)
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
		code := "operation_failed"
		var staged credentialPasswordAlignStageError
		if stopErr == nil && errors.As(waitErr, &staged) && credentialPasswordAlignKnownFailureStage(staged.stage) {
			code += ":" + staged.stage
		}
		return finish("failed", nil, code)
	}
	if workCtx.Err() != nil || !v.now().Before(started.Add(80*time.Second)) {
		return finish("failed", nil, "work_deadline")
	}
	obs, e := decodeCredentialPasswordAlignmentObservation(raw, p.Request)
	if e != nil || obs.ActionID != ai.ID {
		code := "invalid_observation"
		if e != nil {
			code += ":" + strings.TrimPrefix(e.Error(), "credential_password_align:")
		}
		return finish("failed", nil, code)
	}
	return finish("completed", &obs, "closed_password_operation_completed")
}

func (v *CredentialPasswordAligner) finish(out CredentialPasswordAlignmentResult, p CredentialPasswordAlignmentPlan, ai envelope.ActionIntent, pd envelope.PolicyDecision, outcome string, obs *CredentialPasswordAlignmentObservation, code string, reserved *credentialPasswordAlignReservation, cleanup context.Context) (CredentialPasswordAlignmentResult, error) {
	out.State = "failed"
	attempts := runattempt.Open(v.home)
	attempt, readErr := attempts.Get(out.RunAttemptID)
	uncertain := p.Request.Operation != "inspect" && reserved != nil && outcome != "completed" && (readErr != nil || (attempt.Launch != nil && (attempt.Launch.CommittedAt != nil || attempt.Launch.StartErrorAt == nil)))
	if uncertain || (p.Request.Operation != "inspect" && reserved != nil && attempt.Launch != nil && (attempt.Launch.CommittedAt != nil || attempt.Launch.StartErrorAt == nil)) {
		out.State = "recovery_required"
	}
	// Persistence failure leaves the reservation and nonterminal attempt intact.
	// Recovery planning can finish this exact local outcome before a new launch.
	if cleanup.Err() != nil {
		return out, credentialPasswordAlignError("cleanup_deadline")
	}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return out, credentialPasswordAlignError("receipt_unavailable")
	}
	knowledge := ""
	if uncertain {
		knowledge = "unknown"
	}
	statusState := outcome
	if uncertain {
		statusState = "recovery_required"
	}
	status := map[string]any{"schema_version": "compound.credential_password_alignment_status.v0.1", "state": statusState, "outcome": outcome, "code": code, "request_sha256": p.RequestSHA256, "request": p.Request, "action_intent_id": ai.ID, "policy_decision_id": pd.ID, "approval_consumed": out.ApprovalConsumed, "run_attempt_id": out.RunAttemptID, "change_knowledge": knowledge, "configuration_accepted": false, "credential_qualification_accepted": false, "application_activated": false, "service_activated": false}
	if stage := strings.TrimPrefix(code, "operation_failed:"); stage != code && credentialPasswordAlignKnownFailureStage(stage) {
		status["code"] = "operation_failed"
		status["failure_stage"] = stage
	}
	if obs != nil {
		raw, _ := json.Marshal(obs)
		out.ObservationRef, e = store.Put(raw)
		if e != nil {
			return out, credentialPasswordAlignError("observation_persistence")
		}
		status["observation_ref"] = out.ObservationRef
	}
	raw, _ := json.Marshal(status)
	ref, e := store.Put(raw)
	if e != nil {
		return out, credentialPasswordAlignError("receipt_unavailable")
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
		final.FilesChanged = []string{}
		final.Summary = "closed password " + p.Request.Operation + ": " + code + "; database and runtime remain closed"
		if uncertain {
			final.Summary += "; remote effects unknown; separately admitted recovery required"
		}
		final.AfterRefs = []string{ref}
		final.ProofRefs = append(append([]string(nil), reserved.input.ProofRefs...), ref, "check:closed-password-alignment-protocol")
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
			return out, credentialPasswordAlignError("receipt_unavailable")
		}
		receipt, transition = completed.Receipt, completed.Transition
	} else {
		receipt = envelope.Receipt{Header: header(envelope.SVReceipt, idFor("rec_", ai.ID, ref), ai, now), Outcome: outcome, MoveType: "audit_only", StateDelta: envelope.StateDelta{Summary: "closed password operation refused before launch", FilesChanged: []string{}, Artifacts: []string{ref}, Verification: []string{"check:closed-password-alignment-protocol"}}, Cost: envelope.ReceiptCost{ModelCalls: []string{}}, Skills: envelope.ReceiptSkills{Used: []string{ai.Capability}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true}, Memory: envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}}, Next: envelope.ReceiptNext{SuggestedWork: []string{}}, ProofRefs: []string{ref, ai.ID, out.RunAttemptID}}
		transition = envelope.Transition{Header: header(envelope.SVTransition, idFor("tr_", ai.ID, ref), ai, now), BeforeRefs: []string{ai.ID}, ActionRefs: []string{ai.ID}, AfterRefs: []string{ref, receipt.ID}, StateDelta: receipt.StateDelta.Summary, SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: false}}
		if recordEnvelope(v.home, receipt.ID, &receipt) != nil || recordEnvelope(v.home, transition.ID, &transition) != nil {
			return out, credentialPasswordAlignError("receipt_unavailable")
		}
	}
	out.ReceiptID, out.TransitionID = receipt.ID, transition.ID
	raStatus := runattempt.StatusFailed
	if outcome == "completed" {
		raStatus = runattempt.StatusSucceeded
	}
	if e = credentialPasswordAlignCompleteAttempt(v.home, out.RunAttemptID, raStatus, credentialPasswordAlignOwnerFor(p.Request.Operation), receipt.ID, transition.ID, now); e != nil {
		return out, credentialPasswordAlignError("attempt_completion")
	}
	if reserved != nil {
		_, e = workStore.RecordReservedReceiptedAction(p.WorkID, final, reserved.authorizedAt)
		if e != nil && !(uncertain && errors.Is(e, work.ErrRequiredAttemptCompletion)) {
			return out, credentialPasswordAlignError("reservation_completion")
		}
	}
	if outcome != "completed" {
		return out, credentialPasswordAlignError(out.State + ":" + code)
	}
	out.State = outcome
	out.ConfigurationAccepted = false
	if p.Request.Origin != nil {
		if e = v.clearRecoveredOriginal(*p.Request.Origin); e != nil {
			return out, e
		}
	}
	return out, nil
}
