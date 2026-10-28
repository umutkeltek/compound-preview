package act

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func (v *CredentialPasswordAligner) event(id string, out any) error {
	s, e := eventstore.Open(v.home)
	if e != nil {
		return credentialPasswordAlignError("proof_unavailable")
	}
	r, e := s.Read(id)
	if e != nil || json.Unmarshal(r.Bytes, out) != nil {
		return credentialPasswordAlignError("proof_unavailable")
	}
	return nil
}

func credentialPasswordSameTarget(a, b policy.CredentialVerificationRequest) bool {
	return a.ProfileID == b.ProfileID && a.Resource == b.Resource && a.SSH == b.SSH && a.Host == b.Host && a.File == b.File && a.Roles == b.Roles && a.Database == b.Database
}

func (v *CredentialPasswordAligner) failureAuthority(r policy.CredentialPasswordAlignmentRequest) error {
	bad := func(stage string) error {
		return credentialPasswordAlignError("original_verifier_failure_chain_" + stage)
	}
	f := r.Failure
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return bad("artifact_store")
	}
	raw, e := store.Get(f.PlanRef)
	var original CredentialVerificationPlan
	if e != nil || len(raw) > 65536 || json.Unmarshal(raw, &original) != nil {
		return bad("plan_artifact")
	}
	verifier := &CredentialVerifier{home: v.home, now: v.now}
	p, ai, e := verifier.loadPlan(f.PlanRef, original.Actor)
	if e != nil || p.WorkID != f.WorkID || ai.ID != f.ActionID || p.RequestSHA256 != f.RequestSHA256 || !credentialPasswordSameTarget(p.Request, r.CredentialVerificationRequest) {
		return bad("plan_binding")
	}
	var pd envelope.PolicyDecision
	var receipt envelope.Receipt
	var transition envelope.Transition
	if v.event(f.PolicyID, &pd) != nil || v.event(f.ReceiptID, &receipt) != nil || v.event(f.TransitionID, &transition) != nil {
		return bad("native_records")
	}
	if pd.Validate() != nil || receipt.Validate() != nil || transition.Validate() != nil || pd.ActionIntentID != ai.ID || !slices.Contains(pd.Reasons, "exact one-use operator approval "+f.ApprovalGrantID+" matches action binding") || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, pd) != nil {
		return bad("consumed_approval")
	}
	if receipt.IDs.WorkID != f.WorkID || receipt.Outcome != "failed" || transition.IDs.WorkID != f.WorkID || !slices.Contains(transition.ActionRefs, ai.ID) || !slices.Contains(transition.AfterRefs, receipt.ID) {
		return bad("failure_receipt")
	}
	a, e := runattempt.Open(v.home).Get(f.AttemptID)
	if e != nil || a.Status != runattempt.StatusFailed || a.ActionIntentID != ai.ID || a.WorkID != f.WorkID || a.TerminalizationOwner != credentialVerifyOwner || a.TerminalizationReceiptID != receipt.ID || a.TerminalizationTransitionID != transition.ID || a.Launch == nil || a.Launch.CommittedAt == nil || a.Launch.StartErrorAt != nil {
		return bad("actual_native_attempt")
	}
	w, e := work.OpenReadOnly(v.home)
	if e != nil {
		return bad("work_store")
	}
	originalWork, e := w.Get(f.WorkID)
	// The existing verifier predates RequiredAttemptCompletion's forward-link
	// invariant. Its immutable reverse proof binds this exact Work, Action,
	// Receipt and Transition above; never invent a later forward-link repair.
	if e != nil || originalWork.ID != a.WorkID {
		return bad("original_work")
	}
	matches := 0
	for _, ref := range receipt.ProofRefs {
		if !strings.HasPrefix(ref, "artifact:sha256:") {
			continue
		}
		body, e := store.Get(ref)
		if e != nil {
			return bad("status_artifact")
		}
		var status struct {
			SchemaVersion    string `json:"schema_version"`
			State            string `json:"state"`
			Code             string `json:"code"`
			FailureStage     string `json:"failure_stage"`
			RequestSHA256    string `json:"request_sha256"`
			ActionID         string `json:"action_intent_id"`
			PolicyID         string `json:"policy_decision_id"`
			AttemptID        string `json:"run_attempt_id"`
			ApprovalConsumed bool   `json:"approval_consumed"`
		}
		if json.Unmarshal(body, &status) != nil || status.SchemaVersion != "compound.credential_verification_status.v0.1" {
			continue
		}
		if status.State != "failed" || status.Code != "verification_failed" || status.FailureStage != "app_existing" || status.RequestSHA256 != p.RequestSHA256 || status.ActionID != ai.ID || status.PolicyID != pd.ID || status.AttemptID != a.ID || !status.ApprovalConsumed {
			return bad("app_existing_failure")
		}
		matches++
	}
	if matches != 1 {
		return bad("unique_failure_status")
	}
	return nil
}

func (v *CredentialPasswordAligner) alignmentAuthority(r policy.CredentialPasswordAlignmentRequest, now time.Time) error {
	if e := v.failureAuthority(r); e != nil {
		return e
	}
	installer := &RuntimeConfigInstaller{home: v.home, now: v.now}
	if installer.captureAuthority(policy.RuntimeConfigInstallRequest{Profile: r.Profile, Capture: r.Capture}) != nil {
		return credentialPasswordAlignError("qualified_profile_required")
	}
	if r.Operation != "inspect" {
		if e := v.inspectionAuthority(r, now); e != nil {
			return e
		}
	}
	if r.Origin != nil {
		return v.recoveryAuthority(r)
	}
	return nil
}

func (v *CredentialPasswordAligner) inspectionAuthority(r policy.CredentialPasswordAlignmentRequest, now time.Time) error {
	bad := func() error { return credentialPasswordAlignError("fresh_native_inspection_required") }
	events, e := eventstore.Open(v.home)
	if e != nil {
		return bad()
	}
	record, e := events.Read(r.Witness.Ref)
	if e != nil {
		return bad()
	}
	sum := sha256.Sum256(record.Bytes)
	if hex.EncodeToString(sum[:]) != r.Witness.SHA256 {
		return bad()
	}
	var receipt envelope.Receipt
	if json.Unmarshal(record.Bytes, &receipt) != nil || receipt.Validate() != nil || receipt.Outcome != "completed" {
		return bad()
	}
	created, e := time.Parse(time.RFC3339Nano, receipt.CreatedAt)
	observed, oe := time.Parse(time.RFC3339Nano, r.Witness.ObservedAt)
	if e != nil || oe != nil || created.Before(observed) || created.After(now) || now.Sub(observed) > policy.CredentialVerifyApprovalTTL {
		return bad()
	}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return bad()
	}
	var status struct {
		SchemaVersion  string                                    `json:"schema_version"`
		Request        policy.CredentialPasswordAlignmentRequest `json:"request"`
		State          string                                    `json:"state"`
		ActionID       string                                    `json:"action_intent_id"`
		PolicyID       string                                    `json:"policy_decision_id"`
		AttemptID      string                                    `json:"run_attempt_id"`
		ObservationRef string                                    `json:"observation_ref"`
	}
	matches := 0
	for _, ref := range receipt.ProofRefs {
		if !strings.HasPrefix(ref, "artifact:sha256:") {
			continue
		}
		raw, e := store.Get(ref)
		if e != nil {
			return bad()
		}
		var header struct {
			SchemaVersion string `json:"schema_version"`
		}
		if json.Unmarshal(raw, &header) != nil {
			return bad()
		}
		if header.SchemaVersion == "compound.credential_password_alignment_status.v0.1" {
			if json.Unmarshal(raw, &status) != nil {
				return bad()
			}
			matches++
		}
	}
	if matches != 1 || status.State != "completed" || status.Request.Operation != "inspect" || status.Request.ProgramSHA256 != CredentialPasswordAlignmentProgramSHA256() || !credentialPasswordSameTarget(status.Request.CredentialVerificationRequest, r.CredentialVerificationRequest) || !reflect.DeepEqual(status.Request.Profile, r.Profile) || status.Request.Capture != r.Capture || status.Request.Failure != r.Failure {
		return bad()
	}
	var ai envelope.ActionIntent
	var pd envelope.PolicyDecision
	if v.event(status.ActionID, &ai) != nil || v.event(status.PolicyID, &pd) != nil || ai.Validate() != nil || pd.Validate() != nil || ai.IDs.WorkID != receipt.IDs.WorkID || ai.Capability != "credential.existing_closed_postgres.inspect_password_metadata" || ai.ArgumentsHash != "sha256:"+policy.CredentialPasswordAlignmentActionDigest(status.Request) || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, pd) != nil {
		return bad()
	}
	a, e := runattempt.Open(v.home).Get(status.AttemptID)
	if e != nil || a.WorkID != receipt.IDs.WorkID || a.ActionIntentID != ai.ID || a.Status != runattempt.StatusSucceeded || a.TerminalizationOwner != credentialPasswordAlignOwnerFor("inspect") || a.TerminalizationReceiptID != receipt.ID || a.Launch == nil || a.Launch.CommittedAt == nil || a.Launch.StartErrorAt != nil {
		return bad()
	}
	var tr envelope.Transition
	if v.event(a.TerminalizationTransitionID, &tr) != nil || tr.Validate() != nil || !slices.Contains(tr.ActionRefs, ai.ID) || !slices.Contains(tr.AfterRefs, receipt.ID) {
		return bad()
	}
	if !slices.Contains(receipt.ProofRefs, status.ObservationRef) {
		return bad()
	}
	raw, e := store.Get(status.ObservationRef)
	if e != nil {
		return bad()
	}
	o, e := decodeCredentialPasswordAlignmentObservation(raw, status.Request)
	if e != nil || o.ActionID != ai.ID || o.Catalog == nil || *o.Catalog != r.Catalog {
		return bad()
	}
	return nil
}

func (v *CredentialPasswordAligner) recoveryAuthority(r policy.CredentialPasswordAlignmentRequest) error {
	bad := func() error { return credentialPasswordAlignError("original_alignment_chain") }
	o := r.Origin
	if o == nil {
		return bad()
	}
	store, e := NewArtifactStore(v.home)
	if e != nil {
		return bad()
	}
	raw, e := store.Get(o.PlanRef)
	var saved CredentialPasswordAlignmentPlan
	if e != nil || json.Unmarshal(raw, &saved) != nil {
		return bad()
	}
	p, ai, e := v.loadPlan(o.PlanRef, saved.Actor)
	if e != nil || p.Request.Operation != "align" || p.WorkID != o.WorkID || ai.ID != o.ActionID || p.RequestSHA256 != o.RequestSHA256 || !credentialPasswordSameTarget(p.Request.CredentialVerificationRequest, r.CredentialVerificationRequest) || !reflect.DeepEqual(p.Request.Profile, r.Profile) || p.Request.Capture != r.Capture || p.Request.Failure != r.Failure || p.Request.Catalog.DockerSHA256 != r.Catalog.DockerSHA256 {
		return bad()
	}
	a, e := runattempt.Open(v.home).Get(o.AttemptID)
	if e != nil || !runattempt.IsTerminal(a.Status) || a.WorkID != o.WorkID || a.ActionIntentID != o.ActionID || a.TerminalizationOwner != credentialPasswordAlignOwner || a.TerminalizationReceiptID != o.ReceiptID || a.TerminalizationTransitionID != o.TransitionID {
		return bad()
	}
	fence, fencedAttempt, e := credentialPasswordAlignmentReadFence(v.home, idFor("cpaf_", o.AttemptID))
	if e != nil || fencedAttempt.ID != a.ID || fence.ActionID != ai.ID || fence.RequestSHA256 != p.RequestSHA256 || !reflect.DeepEqual(fence.Request, p.Request) || a.Launch == nil || a.Launch.CommittedAt == nil || a.Launch.StartErrorAt != nil {
		return bad()
	}
	native, _ := credentialPasswordAlignmentReadOutcome(v.home, a)
	if !native.NativeProof || policy.CredentialPasswordAlignmentDigest(native.Request) != p.RequestSHA256 {
		return bad()
	}
	var receipt envelope.Receipt
	var tr envelope.Transition
	if v.event(o.ReceiptID, &receipt) != nil || v.event(o.TransitionID, &tr) != nil || receipt.Validate() != nil || tr.Validate() != nil || receipt.IDs.WorkID != o.WorkID || !slices.Contains(tr.ActionRefs, ai.ID) || !slices.Contains(tr.AfterRefs, receipt.ID) {
		return bad()
	}
	return nil
}
