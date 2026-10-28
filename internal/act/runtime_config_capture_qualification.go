package act

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

const runtimeCaptureRequestSchema = "compound.runtime_capture_qualification_request.v0.1"
const runtimeCapturePlanSchema = "compound.runtime_capture_qualification_plan.v0.1"
const runtimeCaptureCapability = "runtime.capture_qualification.exact"

// Inputs are references and nonsecret profile metadata. The private capture is
// read only by the closed owner; its bytes and selected values are never stored.
type RuntimeCaptureFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type RuntimeCaptureQualificationRequest struct {
	SchemaVersion   string                              `json:"schema_version"`
	SourceAction    policy.RuntimeConfigInstallProofRef `json:"source_action"`
	SourcePolicy    policy.RuntimeConfigInstallProofRef `json:"source_policy"`
	Receiver        RuntimeCaptureFile                  `json:"receiver"`
	Sampler         RuntimeCaptureFile                  `json:"sampler"`
	Request         RuntimeCaptureFile                  `json:"request"`
	RequestTemplate RuntimeCaptureFile                  `json:"request_template"`
	Manifest        RuntimeCaptureFile                  `json:"manifest"`
	Attempt         RuntimeCaptureFile                  `json:"attempt"`
	Result          RuntimeCaptureFile                  `json:"result"`
	Public          RuntimeCaptureFile                  `json:"public"`
	CapturePath     string                              `json:"capture_path"`
	CaptureStat     policy.RuntimeConfigInstallStat     `json:"capture_stat"`
	Profile         policy.RuntimeConfigInstallProfile  `json:"profile"`
}
type RuntimeCaptureQualificationPlan struct {
	SchemaVersion  string                             `json:"schema_version"`
	Request        RuntimeCaptureQualificationRequest `json:"request"`
	RequestSHA256  string                             `json:"request_sha256"`
	WorkID         string                             `json:"work_id"`
	WorkSHA256     string                             `json:"work_sha256"`
	Actor          string                             `json:"actor"`
	ActionIntentID string                             `json:"action_intent_id"`
	ApprovalPlanID string                             `json:"approval_plan_id"`
	Qualification  RuntimeConfigCaptureQualification  `json:"qualification"`
}
type RuntimeCaptureQualificationPlanResult struct {
	PlanRef       string                          `json:"plan_ref"`
	PolicyOutcome string                          `json:"policy_outcome"`
	Plan          RuntimeCaptureQualificationPlan `json:"plan"`
}
type RuntimeCaptureQualificationResult struct {
	State            string                             `json:"state"`
	ApprovalConsumed bool                               `json:"approval_consumed"`
	Capture          policy.RuntimeConfigInstallCapture `json:"capture"`
	Profile          policy.RuntimeConfigInstallProfile `json:"profile"`
}
type runtimeCaptureValidated struct {
	qualification RuntimeConfigCaptureQualification
	metadata      map[string][]byte
	profile       policy.RuntimeConfigInstallProfile
}

func DecodeRuntimeCaptureQualificationRequest(raw []byte) (RuntimeCaptureQualificationRequest, error) {
	var r RuntimeCaptureQualificationRequest
	if runtimeConfigStrict(raw, &r) != nil || !credentialVerifyRequiredFields(raw, reflect.TypeOf(r)) || r.SchemaVersion != runtimeCaptureRequestSchema {
		return r, runtimeConfigInstallError("qualification_request")
	}
	return r, nil
}

func runtimeCaptureIntent(r RuntimeCaptureQualificationRequest, q RuntimeConfigCaptureQualification, w envelope.WorkEnvelope, actor string, now time.Time) envelope.ActionIntent {
	digest := RuntimeConfigCaptureQualificationDigest(q)
	return envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: idFor("act_", w.ID, runtimeCaptureCapability, digest), CreatedAt: ts(now), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: w.ID, ContextPackID: w.IDs.ContextPackID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1", EvidenceRefs: []string{r.SourceAction.Ref, r.SourcePolicy.Ref}, Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: actor, Capability: runtimeCaptureCapability, Action: "Qualify frozen protected source capture for the exact inactive runtime profile; local metadata publication only", Target: envelope.ActionTarget{Kind: "runtime_capture_qualification", ID: w.ID, Path: r.CapturePath}, Destination: "local:runtime-capture-qualification:" + w.ID, ArgumentsHash: "sha256:" + digest, SchemaRevision: runtimeCaptureRequestSchema, RequiresExactApproval: true, MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local", RollbackPlan: "Retain source capture unchanged and qualification evidence; no capture replay, remote operation or target installation"}
}

func (v *RuntimeConfigInstaller) captureQualificationPolicy(ai envelope.ActionIntent, r RuntimeCaptureQualificationRequest, now time.Time) (policy.Config, envelope.PolicyDecision, error) {
	cfg, _, err := policy.LoadConfig(v.home)
	if err != nil {
		return cfg, envelope.PolicyDecision{}, runtimeConfigInstallError("policy_unavailable")
	}
	for _, capability := range []string{"build-implement", runtimeCaptureCapability} {
		halted, _, err := policy.AutonomyHalted(v.home, capability)
		if err != nil || halted {
			return cfg, envelope.PolicyDecision{}, runtimeConfigInstallError("autonomy_halted")
		}
	}
	// Evaluate every read path under the existing generic floors. The typed door
	// never removes a forbidden path or overrides a security-floor decision.
	for _, path := range r.paths() {
		probe := ai
		probe.Target.Path = path
		pd := policy.Evaluate(probe, cfg, now)
		if pd.Outcome == "deny" || probe.SecurityFloorOutcome != "" {
			return cfg, pd, runtimeConfigInstallError("qualification_policy")
		}
	}
	pd := policy.Evaluate(ai, cfg, now)
	if pd.Outcome != "hold" || !ai.RequiresExactApproval {
		return cfg, pd, runtimeConfigInstallError("qualification_policy")
	}
	return cfg, pd, nil
}

func (v *RuntimeConfigInstaller) PlanCaptureQualification(r RuntimeCaptureQualificationRequest, workID, actor string) (RuntimeCaptureQualificationPlanResult, error) {
	var out RuntimeCaptureQualificationPlanResult
	w, err := v.requireWork(workID, actor)
	if err != nil {
		return out, err
	}
	// Check floors before reading protected input. The final action below binds
	// the computed projection digest as well as every caller-supplied hash.
	probe := runtimeCaptureIntent(r, RuntimeConfigCaptureQualification{SourceInputSHA256: runtimeConfigInstallHash(r)}, w, actor, v.now().UTC())
	if _, _, err = v.captureQualificationPolicy(probe, r, v.now().UTC()); err != nil {
		return out, err
	}
	validated, err := v.validateCaptureQualification(r, workID)
	if err != nil {
		return out, err
	}
	ai := runtimeCaptureIntent(r, validated.qualification, w, actor, v.now().UTC())
	cfg, pd, err := v.captureQualificationPolicy(ai, r, v.now().UTC())
	if err != nil {
		return out, err
	}
	if err = recordEnvelope(v.home, ai.ID, &ai); err != nil {
		return out, runtimeConfigInstallError("qualification_proposal")
	}
	if err = recordEnvelope(v.home, pd.ID, &pd); err != nil {
		return out, runtimeConfigInstallError("qualification_proposal")
	}
	ap, _, _, err := policy.NewExactActionApprovalStore(v.home).EnsurePlanForIntent(ai, cfg, policy.RuntimeConfigInstallApprovalTTL, v.now().UTC())
	if err != nil {
		return out, runtimeConfigInstallError("qualification_approval_plan")
	}
	p := RuntimeCaptureQualificationPlan{SchemaVersion: runtimeCapturePlanSchema, Request: r, RequestSHA256: runtimeConfigInstallHash(r), WorkID: workID, WorkSHA256: runtimeConfigInstallHash(w), Actor: actor, ActionIntentID: ai.ID, ApprovalPlanID: ap.ID, Qualification: validated.qualification}
	s, err := NewArtifactStore(v.home)
	if err != nil {
		return out, runtimeConfigInstallError("artifact_store")
	}
	raw, _ := json.Marshal(p)
	ref, err := s.Put(raw)
	if err != nil {
		return out, runtimeConfigInstallError("qualification_plan")
	}
	return RuntimeCaptureQualificationPlanResult{PlanRef: ref, PolicyOutcome: pd.Outcome, Plan: p}, nil
}

func (v *RuntimeConfigInstaller) loadCaptureQualificationPlan(ref, actor string) (RuntimeCaptureQualificationPlan, envelope.ActionIntent, error) {
	var p RuntimeCaptureQualificationPlan
	var ai envelope.ActionIntent
	s, err := NewArtifactStore(v.home)
	if err != nil {
		return p, ai, runtimeConfigInstallError("artifact_store")
	}
	raw, err := s.Get(ref)
	if err != nil || runtimeConfigStrict(raw, &p) != nil || p.SchemaVersion != runtimeCapturePlanSchema || p.Actor != actor || p.RequestSHA256 != runtimeConfigInstallHash(p.Request) || p.Qualification.SourceInputSHA256 != p.RequestSHA256 {
		return p, ai, runtimeConfigInstallError("qualification_plan")
	}
	events, err := eventstore.Open(v.home)
	if err != nil {
		return p, ai, runtimeConfigInstallError("event_store")
	}
	rec, err := events.Read(p.ActionIntentID)
	if err != nil || json.Unmarshal(rec.Bytes, &ai) != nil || ai.Validate() != nil || !ai.RequiresExactApproval || ai.IDs.WorkID != p.WorkID || ai.Actor != actor || ai.Capability != runtimeCaptureCapability || ai.ArgumentsHash != "sha256:"+RuntimeConfigCaptureQualificationDigest(p.Qualification) {
		return p, ai, runtimeConfigInstallError("qualification_plan_binding")
	}
	return p, ai, nil
}

// QualifyCapture performs only local qualification. Native exact approval is
// consumed before acceptance artifacts are written; no supplied booleans can
// stand in for the owner's source and custody checks.
func (v *RuntimeConfigInstaller) QualifyCapture(planRef, actor string) (RuntimeCaptureQualificationResult, error) {
	var out RuntimeCaptureQualificationResult
	p, ai, err := v.loadCaptureQualificationPlan(planRef, actor)
	if err != nil {
		return out, err
	}
	release, err := v.executionLock("qualification:" + p.WorkID)
	if err != nil {
		return out, err
	}
	defer release()
	s, err := work.Open(v.home)
	if err != nil {
		return out, runtimeConfigInstallError("work_unavailable")
	}
	w, err := s.Get(p.WorkID)
	if err != nil {
		return out, runtimeConfigInstallError("work_unavailable")
	}
	if w.State == "completed" {
		return v.completedCaptureQualification(p, ai, s)
	}
	if w.State == "verifying" && slices.Contains(w.ActionIntentIDs, ai.ID) {
		leases, e := s.ListLeases()
		if e != nil || !slices.ContainsFunc(leases, func(l work.Lease) bool { return l.WorkID == p.WorkID && l.By == actor }) {
			return out, runtimeConfigInstallError("wrong_lease_owner")
		}
	} else if _, err = v.requireWork(p.WorkID, actor); err != nil {
		return out, err
	}
	if _, _, err = v.captureQualificationPolicy(ai, p.Request, v.now().UTC()); err != nil {
		return out, err
	}
	validated, err := v.validateCaptureQualification(p.Request, p.WorkID)
	if err != nil {
		return out, err
	}
	if !reflect.DeepEqual(validated.qualification, p.Qualification) {
		return out, runtimeConfigInstallError("qualification_drift")
	}
	var pd envelope.PolicyDecision
	if slices.Contains(w.ActionIntentIDs, ai.ID) {
		pd, err = v.recordedCaptureDecision(w, ai, p)
		if err != nil {
			return out, err
		}
	} else {
		if runtimeConfigInstallHash(w) != p.WorkSHA256 {
			return out, runtimeConfigInstallError("work_generation_changed")
		}
		recorded, e := s.EvaluateAndRecordAction(p.WorkID, ai, v.now().UTC())
		if e != nil || !recorded.Recorded {
			return out, runtimeConfigInstallError("exact_approval_required")
		}
		pd = recorded.Decision
	}
	if policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, pd) != nil {
		return out, runtimeConfigInstallError("qualification_consumption")
	}
	out.ApprovalConsumed = true
	// Recheck after admission too; failures retain consumed authority and never
	// close a qualification Work successfully.
	again, err := v.validateCaptureQualification(p.Request, p.WorkID)
	if err != nil || !reflect.DeepEqual(again.qualification, validated.qualification) {
		return out, runtimeConfigInstallError("qualification_drift")
	}
	artifacts, err := NewArtifactStore(v.home)
	if err != nil {
		return out, runtimeConfigInstallError("artifact_store")
	}
	for ref, body := range validated.metadata {
		actual, e := artifacts.Put(body)
		if e != nil || actual != ref {
			return out, runtimeConfigInstallError("qualification_artifact")
		}
	}
	q := validated.qualification
	q.Action, err = v.nativeCaptureProof(ai.ID)
	if err != nil {
		return out, err
	}
	q.Policy, err = v.nativeCaptureProof(pd.ID)
	if err != nil {
		return out, err
	}
	raw, _ := json.Marshal(q)
	ref, err := artifacts.Put(raw)
	if err != nil {
		return out, runtimeConfigInstallError("qualification_artifact")
	}
	if w.State == "verifying" {
		if !slices.Contains(w.ArtifactIDs, ref) {
			return out, runtimeConfigInstallError("qualification_retry")
		}
	} else if _, err = s.Advance(p.WorkID, work.AdvanceInput{To: "verifying", Actor: actor, ArtifactID: ref, Note: "closed owner verified frozen source capture and metadata-only projection"}, v.now().UTC()); err != nil {
		return out, runtimeConfigInstallError("qualification_advance")
	}
	_, err = s.Close(p.WorkID, work.CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: actor, Changed: true, Summary: "Qualified exact frozen protected source capture locally; no capture replay or target effects", ProofRefs: []string{ref, ai.ID, pd.ID, q.SourceAction.Ref, q.SourcePolicy.Ref, planRef}}, v.now().UTC())
	if err != nil {
		return out, runtimeConfigInstallError("qualification_close")
	}
	return v.completedCaptureQualification(p, ai, s)
}

func (v *RuntimeConfigInstaller) recordedCaptureDecision(w envelope.WorkEnvelope, ai envelope.ActionIntent, p RuntimeCaptureQualificationPlan) (envelope.PolicyDecision, error) {
	var pd envelope.PolicyDecision
	events, err := eventstore.Open(v.home)
	if err != nil {
		return pd, runtimeConfigInstallError("event_store")
	}
	if rec, e := events.Read(w.PolicyDecisionID); e == nil && json.Unmarshal(rec.Bytes, &pd) == nil && pd.ActionIntentID == ai.ID {
		return pd, nil
	}
	artifacts, err := NewArtifactStore(v.home)
	if err != nil {
		return pd, runtimeConfigInstallError("artifact_store")
	}
	for _, ref := range w.ArtifactIDs {
		if !strings.HasPrefix(ref, home.ArtifactRefPrefix) {
			continue
		}
		raw, e := artifacts.Get(ref)
		if e != nil {
			continue
		}
		var q RuntimeConfigCaptureQualification
		if runtimeConfigStrict(raw, &q) != nil || q.Action.Ref != ai.ID || RuntimeConfigCaptureQualificationDigest(q) != RuntimeConfigCaptureQualificationDigest(p.Qualification) {
			continue
		}
		if _, e = v.proof(q.Policy, false, &pd); e == nil {
			return pd, nil
		}
	}
	return pd, runtimeConfigInstallError("qualification_retry")
}

func (v *RuntimeConfigInstaller) nativeCaptureProof(id string) (policy.RuntimeConfigInstallProofRef, error) {
	s, err := eventstore.Open(v.home)
	if err != nil {
		return policy.RuntimeConfigInstallProofRef{}, runtimeConfigInstallError("event_store")
	}
	r, err := s.Read(id)
	if err != nil {
		return policy.RuntimeConfigInstallProofRef{}, runtimeConfigInstallError("native_proof")
	}
	return policy.RuntimeConfigInstallProofRef{Ref: id, SHA256: runtimeConfigBytesHash(r.Bytes)}, nil
}

func (v *RuntimeConfigInstaller) completedCaptureQualification(p RuntimeCaptureQualificationPlan, ai envelope.ActionIntent, s *work.Store) (RuntimeCaptureQualificationResult, error) {
	var out RuntimeCaptureQualificationResult
	closed, err := s.ReadTerminalCloseProof(p.WorkID)
	if err != nil || closed.State != "completed" {
		return out, runtimeConfigInstallError("qualification_terminal_proof")
	}
	receiptRef, err := v.nativeCaptureProof(closed.ReceiptID)
	if err != nil {
		return out, err
	}
	transitionRef, err := v.nativeCaptureProof(closed.TransitionID)
	if err != nil {
		return out, err
	}
	var receipt envelope.Receipt
	if _, err = v.proof(receiptRef, false, &receipt); err != nil {
		return out, err
	}
	for _, ref := range receipt.ProofRefs {
		if !strings.HasPrefix(ref, home.ArtifactRefPrefix) {
			continue
		}
		store, err := NewArtifactStore(v.home)
		if err != nil {
			return out, runtimeConfigInstallError("artifact_store")
		}
		raw, err := store.Get(ref)
		if err != nil {
			continue
		}
		var q RuntimeConfigCaptureQualification
		if runtimeConfigStrict(raw, &q) != nil || q.Action.Ref != ai.ID || q.WorkID != p.WorkID || RuntimeConfigCaptureQualificationDigest(q) != RuntimeConfigCaptureQualificationDigest(p.Qualification) {
			continue
		}
		profile := p.Request.Profile
		profileRaw, _ := json.Marshal(profile)
		profile.Ref = home.ArtifactRefPrefix + runtimeConfigBytesHash(profileRaw)
		profile.SHA256 = runtimeConfigBytesHash(profileRaw)
		capture := policy.RuntimeConfigInstallCapture{Qualification: policy.RuntimeConfigInstallProofRef{Ref: ref, SHA256: runtimeConfigBytesHash(raw)}, Receipt: receiptRef, Transition: transitionRef, Path: q.CapturePath, Stat: q.CaptureStat, ProjectionSHA256: q.ProjectionSHA256}
		if err = v.captureAuthority(policy.RuntimeConfigInstallRequest{Capture: capture, Profile: profile}); err != nil {
			return out, err
		}
		return RuntimeCaptureQualificationResult{State: "qualified", ApprovalConsumed: true, Capture: capture, Profile: profile}, nil
	}
	return out, runtimeConfigInstallError("qualification_result_missing")
}

func (r RuntimeCaptureQualificationRequest) paths() []string {
	return []string{r.Receiver.Path, r.Sampler.Path, r.Request.Path, r.RequestTemplate.Path, r.Manifest.Path, r.Attempt.Path, r.Result.Path, r.Public.Path, r.CapturePath}
}

func captureQualificationError() error {
	return errors.New("runtime_config_install:source_qualification_refused")
}

func runtimeCaptureReadPinned(f RuntimeCaptureFile) ([]byte, error) {
	if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path || !runtimeCaptureSHA(f.SHA256) {
		return nil, captureQualificationError()
	}
	info, err := os.Lstat(f.Path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, captureQualificationError()
	}
	st, err := runtimeConfigLocalStat(info, nil)
	if err != nil || st.UID != uint32(os.Geteuid()) || st.Mode != "0600" || st.Links != 1 || st.Bytes <= 0 || st.Bytes > 131072 {
		return nil, captureQualificationError()
	}
	st.SHA256 = f.SHA256
	return runtimeConfigReadCapture(policy.RuntimeConfigInstallCapture{Path: f.Path, Stat: st})
}
