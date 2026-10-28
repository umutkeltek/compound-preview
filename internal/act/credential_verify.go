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

const credentialVerifyOwner = "act.credential_verification"
const credentialVerifyPlanSchema = "compound.credential_verification_plan.v0.1"

type CredentialVerificationPlan struct {
	SchemaVersion         string                               `json:"schema_version"`
	Request               policy.CredentialVerificationRequest `json:"request"`
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
type CredentialVerificationPlanResult struct {
	PlanRef       string                     `json:"plan_ref"`
	Plan          CredentialVerificationPlan `json:"plan"`
	PolicyOutcome string                     `json:"policy_outcome"`
}
type CredentialVerificationResult struct {
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
type credentialVerifyTransport interface {
	Start(context.Context, policy.CredentialVerificationRequest, time.Time, credentialVerifyAdmissionGuard) (credentialVerifyChild, error)
}
type credentialVerifyChild interface {
	Wait() ([]byte, error)
	Stop(context.Context) error
}

type CredentialVerifier struct {
	home      string
	now       func() time.Time
	transport credentialVerifyTransport
}

func NewCredentialVerifier(homePath string) (*CredentialVerifier, error) {
	if homePath == "" || !filepath.IsAbs(homePath) || filepath.Clean(homePath) != homePath {
		return nil, credentialVerifyError("explicit_home_required")
	}
	resolved, e := home.Resolve(homePath)
	if e != nil {
		return nil, errors.New("credential_verify:home_unavailable")
	}
	return &CredentialVerifier{home: resolved, now: time.Now, transport: credentialVerifyNativeTransport{}}, nil
}

func credentialVerifyHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func credentialVerifyError(code string) error { return errors.New("credential_verify:" + code) }

func (v *CredentialVerifier) requireWork(workID, actor string) (envelope.WorkEnvelope, error) {
	s, e := work.OpenReadOnly(v.home)
	if e != nil {
		return envelope.WorkEnvelope{}, credentialVerifyError("work_unavailable")
	}
	w, e := s.Get(workID)
	if e != nil || actor == "" || strings.TrimSpace(actor) != actor || w.State != "running" {
		return w, credentialVerifyError("work_not_executable")
	}
	leases, e := s.ListLeases()
	if e != nil {
		return w, credentialVerifyError("lease_unavailable")
	}
	for _, l := range leases {
		if l.WorkID == workID && l.By == actor {
			return w, nil
		}
	}
	return w, credentialVerifyError("wrong_lease_owner")
}

func (v *CredentialVerifier) witness(r policy.CredentialVerificationRequest, now time.Time) error {
	s, e := eventstore.Open(v.home)
	if e != nil {
		return credentialVerifyError("witness_unavailable")
	}
	record, e := s.Read(r.Witness.Ref)
	if e != nil {
		return credentialVerifyError("witness_unavailable")
	}
	sum := sha256.Sum256(record.Bytes)
	if hex.EncodeToString(sum[:]) != r.Witness.SHA256 {
		return credentialVerifyError("witness_changed")
	}
	var h envelope.Header
	if json.Unmarshal(record.Bytes, &h) != nil || h.ID != r.Witness.Ref || (h.SchemaVersion != envelope.SVReceipt && h.SchemaVersion != "compound.finding.v0.1" && h.SchemaVersion != "compound.source_observation.v0.1") {
		return credentialVerifyError("witness_shape")
	}
	created, e := time.Parse(time.RFC3339Nano, h.CreatedAt)
	observed, oe := time.Parse(time.RFC3339Nano, r.Witness.ObservedAt)
	if e != nil || oe != nil || created.Before(observed) || created.After(now) || now.Sub(observed) > policy.CredentialVerifyApprovalTTL {
		return credentialVerifyError("witness_stale")
	}
	return nil
}

func credentialVerificationIntent(r policy.CredentialVerificationRequest, workID, actor, workSHA string, now time.Time) envelope.ActionIntent {
	digest := policy.CredentialVerificationDigest(r)
	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: idFor("act_", workID, actor, digest, ts(now)), CreatedAt: ts(now), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: workID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier2", EvidenceRefs: []string{r.Witness.Ref, "witness-sha256:" + r.Witness.SHA256, "work-generation:sha256:" + workSHA}, Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: actor, Capability: policy.CredentialVerifyCapability, Action: policy.CredentialVerifyAction, Target: envelope.ActionTarget{Kind: policy.CredentialVerifyTarget, ID: r.ProfileID, Path: r.File.Path}, Destination: policy.CredentialVerificationDestination(r), ArgumentsHash: "sha256:" + digest, SchemaRevision: policy.CredentialVerifySchema, MutationClass: "security", Reversibility: "easy", BlastRadius: "host", RollbackPlan: "Preserve the observation and failed attempt; no remote persistent mutation, automatic replay, credential export or repair."}
	return ai
}

// Plan records a real non-allowing native action/decision and creates only the
// existing exact approval plan. It never inspects credentials or opens SSH.
func (v *CredentialVerifier) Plan(r policy.CredentialVerificationRequest, workID, actor string) (CredentialVerificationPlanResult, error) {
	var out CredentialVerificationPlanResult
	now := v.now().UTC()
	if policy.ValidateCredentialVerificationRequest(r, CredentialVerificationProgramSHA256(), now) != nil {
		return out, credentialVerifyError("invalid_request")
	}
	w, e := v.requireWork(workID, actor)
	if e != nil {
		return out, e
	}
	if e = v.witness(r, now); e != nil {
		return out, e
	}
	cfg, _, e := policy.LoadConfig(v.home)
	if e != nil {
		return out, credentialVerifyError("policy_unavailable")
	}
	ai := credentialVerificationIntent(r, workID, actor, credentialVerifyHash(w), now)
	pd, eligible := policy.EvaluateCredentialVerification(ai, r, CredentialVerificationProgramSHA256(), cfg, policy.EvaluationContext{}, now)
	if e = recordEnvelope(v.home, ai.ID, &ai); e != nil {
		return out, credentialVerifyError("intent_persistence")
	}
	if e = recordEnvelope(v.home, pd.ID, &pd); e != nil {
		return out, credentialVerifyError("decision_persistence")
	}
	out.PolicyOutcome = pd.Outcome
	if pd.Outcome != "hold" || !eligible {
		return out, credentialVerifyError("admission_denied")
	}
	ap, _, _, e := policy.NewExactActionApprovalStore(v.home).EnsurePlanForIntent(ai, cfg, policy.CredentialVerifyApprovalTTL, now)
	if e != nil {
		return out, credentialVerifyError("approval_plan_unavailable")
	}
	p := CredentialVerificationPlan{SchemaVersion: credentialVerifyPlanSchema, Request: r, RequestSHA256: policy.CredentialVerificationDigest(r), WorkID: workID, Actor: actor, WorkSHA256: credentialVerifyHash(w), ActionIntentID: ai.ID, PolicyDecisionID: pd.ID, ApprovalPlanID: ap.ID, ApprovalBindingDigest: ap.BindingDigest, ApprovalExpiresAt: ap.ExpiresAt}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return out, credentialVerifyError("artifact_store")
	}
	raw, _ := json.Marshal(p)
	ref, e := store.Put(raw)
	if e != nil {
		return out, credentialVerifyError("plan_persistence")
	}
	out.PlanRef = ref
	out.Plan = p
	return out, nil
}

func (v *CredentialVerifier) loadPlan(ref, actor string) (CredentialVerificationPlan, envelope.ActionIntent, error) {
	var p CredentialVerificationPlan
	var ai envelope.ActionIntent
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return p, ai, credentialVerifyError("artifact_store")
	}
	raw, e := store.Get(ref)
	if e != nil || len(raw) > 65536 {
		return p, ai, credentialVerifyError("plan_unavailable")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.SchemaVersion != credentialVerifyPlanSchema || p.Actor != actor || p.RequestSHA256 != policy.CredentialVerificationDigest(p.Request) {
		return p, ai, credentialVerifyError("plan_binding")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || !credentialVerifyUniqueJSON(raw) || !credentialVerifyRequiredFields(raw, reflect.TypeOf(p)) {
		return p, ai, credentialVerifyError("plan_binding")
	}
	events, e := eventstore.Open(v.home)
	if e != nil {
		return p, ai, credentialVerifyError("intent_unavailable")
	}
	record, e := events.Read(p.ActionIntentID)
	if e != nil || json.Unmarshal(record.Bytes, &ai) != nil {
		return p, ai, credentialVerifyError("intent_unavailable")
	}
	created, e := time.Parse(time.RFC3339Nano, ai.CreatedAt)
	if e != nil {
		return p, ai, credentialVerifyError("intent_binding")
	}
	expected := credentialVerificationIntent(p.Request, p.WorkID, p.Actor, p.WorkSHA256, created)
	if ai.ID != expected.ID || policy.CanonicalFullIntentDigest(ai) != policy.CanonicalFullIntentDigest(expected) {
		return p, ai, credentialVerifyError("intent_binding")
	}
	var initial envelope.PolicyDecision
	decision, e := events.Read(p.PolicyDecisionID)
	if e != nil || json.Unmarshal(decision.Bytes, &initial) != nil || initial.ID != p.PolicyDecisionID || initial.ActionIntentID != ai.ID || initial.Outcome != "hold" {
		return p, ai, credentialVerifyError("plan_decision_binding")
	}
	var approval policy.ExactActionApprovalPlan
	record, e = events.Read(p.ApprovalPlanID)
	if e != nil || json.Unmarshal(record.Bytes, &approval) != nil || approval.SchemaVersion != policy.ExactActionApprovalPlanSchema || approval.ID != p.ApprovalPlanID || approval.ActionIntentID != ai.ID || approval.BindingDigest != p.ApprovalBindingDigest || approval.ExpiresAt != p.ApprovalExpiresAt || policy.ActionBindingDigest(approval.Binding) != approval.BindingDigest {
		return p, ai, credentialVerifyError("plan_approval_binding")
	}
	issued, ie := time.Parse(time.RFC3339Nano, approval.CreatedAt)
	expires, ee := time.Parse(time.RFC3339Nano, approval.ExpiresAt)
	if ie != nil || ee != nil || !expires.After(issued) || expires.Sub(issued) > policy.CredentialVerifyApprovalTTL {
		return p, ai, credentialVerifyError("plan_approval_binding")
	}
	return p, ai, nil
}

func (v *CredentialVerifier) current(p CredentialVerificationPlan, ai envelope.ActionIntent, workSHA string) (policy.Config, envelope.PolicyDecision, error) {
	var cfg policy.Config
	var pd envelope.PolicyDecision
	now := v.now().UTC()
	if policy.ValidateCredentialVerificationRequest(p.Request, CredentialVerificationProgramSHA256(), now) != nil {
		return cfg, pd, credentialVerifyError("request_stale")
	}
	w, e := v.requireWork(p.WorkID, p.Actor)
	if e != nil {
		return cfg, pd, e
	}
	if credentialVerifyHash(w) != workSHA {
		return cfg, pd, credentialVerifyError("work_generation_changed")
	}
	if e = v.witness(p.Request, now); e != nil {
		return cfg, pd, e
	}
	halted, _, e := policy.AutonomyHalted(v.home, ai.MutationClass)
	if e != nil || halted {
		return cfg, pd, credentialVerifyError("autonomy_halted")
	}
	cfg, _, e = policy.LoadConfig(v.home)
	if e != nil {
		return cfg, pd, credentialVerifyError("policy_unavailable")
	}
	pd, eligible := policy.EvaluateCredentialVerification(ai, p.Request, CredentialVerificationProgramSHA256(), cfg, policy.EvaluationContext{}, now)
	if !eligible || pd.Outcome != "hold" {
		return cfg, pd, credentialVerifyError("admission_denied")
	}
	if policy.ActionBindingDigest(policy.ActionBindingForIntent(ai, cfg)) != p.ApprovalBindingDigest {
		return cfg, pd, credentialVerifyError("policy_generation_changed")
	}
	return cfg, pd, nil
}

// Execute takes only an immutable native plan reference, never an allowing
// decision supplied by the caller. Full current approval authority is re-read.
type credentialVerifyReservation struct {
	input        work.ReceiptedActionInput
	authorizedAt time.Time
}

func (v *CredentialVerifier) Execute(ctx context.Context, planRef, actor string) (out CredentialVerificationResult, err error) {
	started := v.now().UTC()
	outer, outerCancel := context.WithDeadline(ctx, started.Add(60*time.Second))
	defer outerCancel()
	workCtx, workCancel := context.WithDeadline(outer, started.Add(50*time.Second))
	defer workCancel()
	p, ai, e := v.loadPlan(planRef, actor)
	if e != nil {
		return out, e
	}
	var pd envelope.PolicyDecision
	var attempt runattempt.RunAttempt
	var reserved *credentialVerifyReservation
	var failureStage string
	finish := func(outcome string, obs *CredentialVerificationObservation, code string) (CredentialVerificationResult, error) {
		return v.finish(out, p, ai, pd, outcome, obs, code, failureStage, reserved, outer)
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
			return credentialVerifyError("work_deadline")
		}
		attemptTime, _ := time.Parse("20060102t150405z", p.Request.AttemptID[:16])
		attemptID := runattempt.MintDeterministic("credential-verify:"+p.Request.AttemptID, attemptTime)
		if _, e = attempts.Get(attemptID); e == nil {
			return credentialVerifyError("attempt_already_used")
		} else if !errors.Is(e, runattempt.ErrNotFound) {
			return credentialVerifyError("attempt_authority_unavailable")
		}
		if preflightEventStore(v.home) != nil {
			return credentialVerifyError("event_store_unavailable")
		}
		var matched bool
		pd, matched, e = policy.NewExactActionApprovalStore(v.home).ApplyMatchingLocked(ai, cfg, base, v.now().UTC())
		if e != nil || !matched || !policy.Allowed(pd) {
			return credentialVerifyError("exact_approval_required")
		}
		out.ApprovalConsumed = true
		if e = recordEnvelope(v.home, pd.ID, &pd); e != nil {
			return credentialVerifyError("approved_decision_persistence")
		}
		attempt, e = attempts.MintWithID(attemptID, p.WorkID, p.Actor, "compound", v.now().UTC())
		if e != nil {
			return credentialVerifyError("attempt_persistence")
		}
		out.RunAttemptID = attempt.ID
		if e = attempts.AttachActionIntent(attempt.ID, ai.ID); e != nil {
			return credentialVerifyError("attempt_binding")
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
	pending := work.ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: w.State, OutcomePending: true, Outcome: "blocked", Changed: false, Summary: "awaiting bounded existing maintenance authentication evidence", MoveType: "audit_only", ProofRefs: []string{ai.ID, attempt.ID, "policy-decision:" + pd.ID}, BeforeRefs: []string{p.Request.Witness.Ref, "request-sha256:" + p.RequestSHA256}, FilesChanged: []string{}}
	authorizedAt := v.now().UTC()
	authorize := func() error {
		if _, _, e := v.current(p, ai, p.WorkSHA256); e != nil {
			return e
		}
		if policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, pd) != nil {
			return credentialVerifyError("approval_consumption_unavailable")
		}
		if workCtx.Err() != nil || !v.now().Before(started.Add(50*time.Second)) {
			return credentialVerifyError("work_deadline")
		}
		return nil
	}
	if e = s.ReserveReceiptedAction(p.WorkID, pending, authorizedAt, authorize); e != nil {
		return finish("failed", nil, "action_reservation")
	}
	reserved = &credentialVerifyReservation{input: pending, authorizedAt: authorizedAt}
	// Exact-approval planning already published this metadata-only ActionIntent;
	// the native reauthorization API deliberately refuses published actions.
	// Read the work owner's validated reservation under native admission instead.
	guard := func(start func() error) error {
		if start == nil {
			return credentialVerifyError("launch_callback")
		}
		return home.WithAutonomyAdmissionLock(v.home, func() error {
			input, at, present, e := s.ReadReceiptedActionReservationInput(p.WorkID, ai.ID)
			if e != nil || !present || !at.Equal(authorizedAt) || credentialVerifyHash(input) != credentialVerifyHash(pending) {
				return credentialVerifyError("work_reservation_drift")
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
	child, e := v.transport.Start(workCtx, p.Request, started.Add(50*time.Second), guard)
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
		if stopErr == nil {
			failureStage = credentialVerifySafeFailureStage(waitErr)
		}
		return finish("failed", nil, "verification_failed")
	}
	if workCtx.Err() != nil || !v.now().Before(started.Add(50*time.Second)) {
		return finish("failed", nil, "work_deadline")
	}
	obs, e := decodeCredentialVerificationObservation(raw, p.Request)
	if e != nil {
		return finish("failed", nil, "invalid_observation")
	}
	return finish("completed", &obs, "verified_existing_maintenance_auth")
}

func (v *CredentialVerifier) finish(out CredentialVerificationResult, p CredentialVerificationPlan, ai envelope.ActionIntent, pd envelope.PolicyDecision, outcome string, obs *CredentialVerificationObservation, code, failureStage string, reserved *credentialVerifyReservation, cleanup context.Context) (CredentialVerificationResult, error) {
	out.State = "failed"
	// Never run the failed work deadline through this writer. The outer context
	// retains the original ten-second cleanup reserve.
	if cleanup.Err() != nil {
		_, _ = runattempt.Open(v.home).CompleteWithReason(out.RunAttemptID, runattempt.StatusFailed, "credential_verification_cleanup_deadline", v.now().UTC())
		return out, credentialVerifyError("cleanup_deadline")
	}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		_, _ = runattempt.Open(v.home).CompleteWithReason(out.RunAttemptID, runattempt.StatusFailed, "credential_verification_receipt_unavailable", v.now().UTC())
		return out, credentialVerifyError("receipt_unavailable")
	}
	status := map[string]any{"schema_version": "compound.credential_verification_status.v0.1", "state": outcome, "code": code, "request_sha256": p.RequestSHA256, "action_intent_id": ai.ID, "policy_decision_id": pd.ID, "approval_consumed": out.ApprovalConsumed, "run_attempt_id": out.RunAttemptID, "configuration_accepted": false, "application_access_accepted": false, "source_capture_accepted": false, "migration_accepted": false}
	if outcome == "failed" && credentialVerifyKnownFailureStage(failureStage) && failureStage != "cleanup" {
		status["failure_stage"] = failureStage
	}
	if obs != nil {
		raw, _ := json.Marshal(obs)
		out.ObservationRef, e = store.Put(raw)
		if e != nil {
			outcome = "failed"
			code = "observation_persistence"
			status["state"] = outcome
			status["code"] = code
		} else {
			status["observation_ref"] = out.ObservationRef
		}
	}
	raw, _ := json.Marshal(status)
	ref, e := store.Put(raw)
	if e != nil {
		_, _ = runattempt.Open(v.home).CompleteWithReason(out.RunAttemptID, runattempt.StatusFailed, "credential_verification_receipt_unavailable", v.now().UTC())
		return out, credentialVerifyError("receipt_unavailable")
	}
	now := v.now().UTC()
	var r envelope.Receipt
	var tr envelope.Transition
	if reserved != nil {
		final := reserved.input
		final.OutcomePending = false
		final.Outcome = outcome
		final.Changed = false
		final.Summary = "credential verification observation: " + code + "; no configuration, application-access, source-capture or migration acceptance"
		final.AfterRefs = []string{ref}
		final.ProofRefs = append(append([]string(nil), reserved.input.ProofRefs...), ref, "check:closed-credential-verifier-protocol")
		if out.ObservationRef != "" {
			final.AfterRefs = append(final.AfterRefs, out.ObservationRef)
			final.ProofRefs = append(final.ProofRefs, out.ObservationRef)
		}
		s, se := work.Open(v.home)
		if se == nil {
			se = s.SealReceiptedActionOutcome(p.WorkID, reserved.input, final, reserved.authorizedAt)
		}
		var completed work.ReceiptedActionResult
		if se == nil {
			completed, se = s.RecordReservedReceiptedAction(p.WorkID, final, reserved.authorizedAt)
		}
		if se != nil {
			_, _ = runattempt.Open(v.home).CompleteWithReason(out.RunAttemptID, runattempt.StatusFailed, "credential_verification_receipt_unavailable", now)
			return out, credentialVerifyError("receipt_unavailable")
		}
		r = completed.Receipt
		tr = completed.Transition
	} else {
		r = envelope.Receipt{Header: header(envelope.SVReceipt, idFor("rec_", ai.ID, ref), ai, now), Outcome: outcome, MoveType: "audit_only", StateDelta: envelope.StateDelta{Changed: false, Summary: "credential verification observation: " + code + "; no configuration, application-access or migration acceptance", FilesChanged: []string{}, Artifacts: []string{ref}, Verification: []string{"check:closed-credential-verifier-protocol"}}, Cost: envelope.ReceiptCost{ModelCalls: []string{}}, Skills: envelope.ReceiptSkills{Used: []string{policy.CredentialVerifyCapability}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true}, Memory: envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}}, Next: envelope.ReceiptNext{SuggestedWork: []string{}}, ProofRefs: []string{ref, p.ActionIntentID, out.RunAttemptID}}
		tr = envelope.Transition{Header: header(envelope.SVTransition, idFor("tr_", ai.ID, ref), ai, now), BeforeRefs: []string{p.ActionIntentID}, ActionRefs: []string{ai.ID}, AfterRefs: []string{ref, r.ID}, StateDelta: r.StateDelta.Summary, SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: false}}
	}
	if r.Validate() != nil || tr.Validate() != nil || recordEnvelope(v.home, r.ID, &r) != nil || recordEnvelope(v.home, tr.ID, &tr) != nil {
		_, _ = runattempt.Open(v.home).CompleteWithReason(out.RunAttemptID, runattempt.StatusFailed, "credential_verification_receipt_unavailable", now)
		return out, credentialVerifyError("receipt_unavailable")
	}
	statusRA := runattempt.StatusFailed
	if outcome == "completed" {
		statusRA = runattempt.StatusSucceeded
	}
	if _, e = runattempt.Open(v.home).CompleteWithOwnerProof(out.RunAttemptID, statusRA, credentialVerifyOwner, r.ID, tr.ID, now); e != nil {
		return out, credentialVerifyError("terminalization_unavailable")
	}
	out.State = outcome
	out.ReceiptID = r.ID
	out.TransitionID = tr.ID
	if outcome != "completed" {
		return out, credentialVerifyError(code)
	}
	return out, nil
}
