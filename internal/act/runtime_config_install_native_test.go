package act

import (
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type installerNativeFixture struct {
	credential *credentialFixture
	owner      *RuntimeConfigInstaller
	request    policy.RuntimeConfigInstallRequest
	workID     string
	transport  *installerTransport
}
type installerTransport struct {
	starts    int
	selected  map[string]string
	operation string
	fail      bool
	r         policy.RuntimeConfigInstallRequest
	crash     bool
	onStart   func()
}

func (m *installerTransport) Start(ctx context.Context, r policy.RuntimeConfigInstallRequest, selected map[string]string, operation string, deadline time.Time, guard runtimeConfigInstallAdmissionGuard) (runtimeConfigInstallChild, error) {
	if err := guard(func() error { m.starts++; return nil }); err != nil {
		return nil, err
	}
	if m.onStart != nil {
		m.onStart()
	}
	m.selected = map[string]string{}
	for k, v := range selected {
		m.selected[k] = v
	}
	m.operation = operation
	m.r = r
	o := runtimeConfigInstallObservationFixture(r)
	o.Operation = operation
	if operation == "reconcile" {
		o.Changed = false
	}
	if operation == "rollback" {
		o.State = "rolled_back"
		o.ConfigurationAccepted = false
		post := *r.Files[0].Stat
		post.Inode = 999
		o.Postimages = [4]*policy.RuntimeConfigInstallStat{&post, nil, nil, nil}
		o.Written = [4]bool{}
	}
	raw, _ := json.Marshal(o)
	child := &credentialMockChild{raw: raw}
	if m.crash {
		child.beforeWait = func() { panic("synthetic process interruption") }
	}
	if m.fail {
		child.waitErr = errors.New("synthetic transport loss")
	}
	return child, nil
}
func nativeInstallerWork(t *testing.T, h, predicate, id string, now time.Time) envelope.WorkEnvelope {
	t.Helper()
	s, e := work.Open(h)
	if e != nil {
		t.Fatal(e)
	}
	w, e := s.Create(work.CreateInput{WorkID: id, Owner: "fixture", Actor: "fixture", SuccessPredicate: predicate}, now.Add(-time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Claim(w.ID, "fixture", now.Add(-30*time.Second)); e != nil {
		t.Fatal(e)
	}
	w, e = s.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "fixture"}, now.Add(-20*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	return w
}
func installerProof(t *testing.T, h, ref string, artifact bool) policy.RuntimeConfigInstallProofRef {
	t.Helper()
	var raw []byte
	if artifact {
		s, e := NewArtifactStore(h)
		if e != nil {
			t.Fatal(e)
		}
		raw, e = s.Get(ref)
		if e != nil {
			t.Fatal(e)
		}
	} else {
		s, e := eventstore.Open(h)
		if e != nil {
			t.Fatal(e)
		}
		r, e := s.Read(ref)
		if e != nil {
			t.Fatal(e)
		}
		raw = r.Bytes
	}
	return policy.RuntimeConfigInstallProofRef{Ref: ref, SHA256: runtimeConfigBytesHash(raw)}
}
func installerArtifact(t *testing.T, h string, value any) policy.RuntimeConfigInstallProofRef {
	t.Helper()
	s, e := NewArtifactStore(h)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	ref, e := s.Put(raw)
	if e != nil {
		t.Fatal(e)
	}
	return policy.RuntimeConfigInstallProofRef{Ref: ref, SHA256: runtimeConfigBytesHash(raw)}
}
func installerRecordAction(t *testing.T, h string, w envelope.WorkEnvelope, now time.Time, capability, digest string) (policy.RuntimeConfigInstallProofRef, policy.RuntimeConfigInstallProofRef) {
	t.Helper()
	ai := envelope.ActionIntent{Header: header(envelope.SVActionIntent, idFor("act_", w.ID, capability, digest), envelope.ActionIntent{Header: envelope.Header{IDs: envelope.IDs{WorkID: w.ID}}}, now), Actor: "fixture", Capability: capability, Action: "qualify synthetic source metadata", Target: envelope.ActionTarget{Kind: "synthetic_metadata", ID: "fixture"}, ArgumentsHash: digest, MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local", RollbackPlan: "remove synthetic fixture only"}
	s, e := work.Open(h)
	if e != nil {
		t.Fatal(e)
	}
	result, e := s.EvaluateAndRecordAction(w.ID, ai, now)
	if e != nil || !result.Recorded {
		t.Fatalf("native fixture action: %v %+v", e, result.Decision)
	}
	return installerProof(t, h, ai.ID, false), installerProof(t, h, result.Decision.ID, false)
}
func installCaptureFixture(t *testing.T, f *installerNativeFixture) {
	t.Helper()
	q := newCaptureQualifierFixture(t, f.credential.home, f.credential.now, f.request.Profile)
	plan := q.plan(t)
	q.approve(t, plan)
	result, err := q.owner.QualifyCapture(plan.PlanRef, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	f.request.Capture = result.Capture
	f.request.Profile = result.Profile
}

func newInstallerNativeFixture(t *testing.T) *installerNativeFixture {
	return newInstallerProfileFixture(t, nil)
}

func newInstallerProfileFixture(t *testing.T, configure func(*policy.RuntimeConfigInstallProfile)) *installerNativeFixture {
	t.Helper()
	c := newCredentialFixture(t)
	p := c.plan(t)
	c.approve(t, p)
	result, e := c.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f := &installerNativeFixture{credential: c, request: runtimeConfigInstallFixture(c.now), transport: &installerTransport{}}
	convert := func(a, b any) {
		raw, _ := json.Marshal(a)
		if e := json.Unmarshal(raw, b); e != nil {
			t.Fatal(e)
		}
	}
	r := &f.request
	convert(c.request.Resource, &r.Resource)
	convert(c.request.SSH, &r.SSH)
	convert(c.request.Host, &r.Host)
	convert(c.request.Roles, &r.Roles)
	r.Database.Name = c.request.Database.Name
	r.Database.AllowConnections = c.request.Database.AllowConnections
	r.Files[0].Path = c.request.File.Path
	r.Root.Path = filepath.Dir(c.request.File.Path)
	for i := range r.Files {
		r.Files[i].Path = filepath.Join(r.Root.Path, r.Files[i].Basename)
	}
	convert(c.request.File, r.Files[0].Stat)
	r.Files[0].Stat.Mode = "0600"
	r.Files[0].Stat.Links = 1
	convert(c.request.Witness, &r.Witness)
	attempt, e := runattempt.Open(c.home).Get(result.RunAttemptID)
	if e != nil {
		t.Fatal(e)
	}
	r.Qualification = policy.RuntimeConfigInstallQualification{Plan: installerProof(t, c.home, p.PlanRef, true), Receipt: installerProof(t, c.home, result.ReceiptID, false), Transition: installerProof(t, c.home, result.TransitionID, false), Attempt: policy.RuntimeConfigInstallProofRef{Ref: attempt.ID, SHA256: runtimeConfigInstallHash(attempt)}, Observation: installerProof(t, c.home, result.ObservationRef, true)}
	f.workID = nativeInstallerWork(t, c.home, "inactive runtime configuration install", "", c.now).ID
	f.owner, e = NewRuntimeConfigInstaller(c.home)
	if e != nil {
		t.Fatal(e)
	}
	f.owner.now = func() time.Time { return c.now }
	f.owner.transport = f.transport
	if configure != nil {
		configure(&r.Profile)
	}
	installCaptureFixture(t, f)
	return f
}
func (f *installerNativeFixture) plan(t *testing.T) RuntimeConfigInstallPlanResult {
	t.Helper()
	p, e := f.owner.Plan(f.request, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func (f *installerNativeFixture) approve(t *testing.T, p RuntimeConfigInstallPlanResult) {
	t.Helper()
	cfg, _, e := policy.LoadConfig(f.credential.home)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = policy.NewExactActionApprovalStore(f.credential.home).Approve(p.Plan.ApprovalPlanID, "operator:fixture", cfg, f.credential.now); e != nil {
		t.Fatal(e)
	}
}
func TestRuntimeConfigNativeQualificationAndSuccessfulFinalization(t *testing.T) {
	f := newInstallerNativeFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if result.State != "installed_inactive" || !result.ConfigurationAccepted || f.transport.starts != 1 || len(f.transport.selected) != 3 {
		t.Fatalf("incomplete installation: %+v", result)
	}
	if _, ok := f.transport.selected["database"]; ok {
		t.Fatal("source database forwarded")
	}
	a, e := runattempt.Open(f.credential.home).Get(result.RunAttemptID)
	if e != nil || a.Status != runattempt.StatusSucceeded || a.TerminalizationOwner != runtimeConfigInstallOwner {
		t.Fatal("native proof incomplete", e)
	}
	s, _ := work.Open(f.credential.home)
	if _, _, present, e := s.ReadReceiptedActionReservationInput(f.workID, p.Plan.ActionIntentID); e != nil || present {
		t.Fatal("successful native finalization left reservation", e)
	}
}
func TestRuntimeConfigNativeUncertaintyRequiresSeparateRecovery(t *testing.T) {
	f := newInstallerNativeFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	f.transport.fail = true
	result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || result.State != "recovery_required" || result.ReceiptID == "" || result.RecoveryWorkID == "" {
		t.Fatalf("uncertainty lost: %+v %v", result, e)
	}
	original, e := runattempt.Open(f.credential.home).Get(result.RunAttemptID)
	if e != nil || original.Status != runattempt.StatusFailed {
		t.Fatal(e)
	}
	s, _ := work.Open(f.credential.home)
	if _, e = s.Advance(f.workID, work.AdvanceInput{To: "verifying", Actor: "fixture"}, f.credential.now); e == nil {
		t.Fatal("uncertainty blocker released")
	}
	nativeInstallerWork(t, f.credential.home, "separately approved rollback", result.RecoveryWorkID, f.credential.now)
	recovery, e := f.owner.PlanRecovery(p.PlanRef, "rollback", result.RecoveryWorkID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, recovery)
	f.transport.fail = false
	release, e := f.owner.executionLock(p.Plan.ActionIntentID)
	if e != nil {
		t.Fatal(e)
	}
	blocked, e := f.owner.Execute(context.Background(), recovery.PlanRef, "fixture")
	release()
	if e == nil || !strings.Contains(e.Error(), "execution_in_progress") || blocked.ApprovalConsumed || f.transport.starts != 1 {
		t.Fatalf("recovery ignored original owner lock: %+v %v", blocked, e)
	}
	recovered, e := f.owner.Execute(context.Background(), recovery.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if recovered.State != "rolled_back" || recovered.ConfigurationAccepted || len(f.transport.selected) != 0 {
		t.Fatalf("bad recovery: %+v", recovered)
	}
	if _, _, present, e := s.ReadReceiptedActionReservationInput(f.workID, p.Plan.ActionIntentID); e != nil || present {
		t.Fatal("verified recovery did not clear original blocker", e)
	}
	after, e := runattempt.Open(f.credential.home).Get(result.RunAttemptID)
	if e != nil || after.Status != runattempt.StatusFailed || after.TerminalizationReceiptID != original.TerminalizationReceiptID {
		t.Fatal("original failed proof changed", e)
	}
}

func TestRuntimeConfigNativeInterruptedFinalizationCanOnlyRecoverSeparately(t *testing.T) {
	f := newInstallerNativeFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	f.transport.crash = true
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected synthetic process interruption")
			}
		}()
		_, _ = f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	}()
	attempts, e := runattempt.Open(f.credential.home).ForWork(f.workID)
	if e != nil {
		t.Fatal(e)
	}
	var original runattempt.RunAttempt
	for _, candidate := range attempts {
		if candidate.ActionIntentID == p.Plan.ActionIntentID {
			if original.ID != "" {
				t.Fatal("duplicate original action attempt")
			}
			original = candidate
		}
	}
	if original.ID == "" {
		t.Fatal("original native action attempt missing")
	}
	if runattempt.IsTerminal(original.Status) {
		t.Fatal("interrupted attempt already terminal")
	}
	recoveryWork := runtimeConfigRecoveryWorkID(p.Plan.ActionIntentID, original.ID)
	nativeInstallerWork(t, f.credential.home, "recovery after interrupted local finalizer", recoveryWork, f.credential.now)
	recovery, e := f.owner.PlanRecovery(p.PlanRef, "rollback", recoveryWork, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	original, e = runattempt.Open(f.credential.home).Get(original.ID)
	if e != nil || original.Status != runattempt.StatusFailed || original.TerminalizationReceiptID == "" {
		t.Fatal("missing truthful interrupted native proof", e)
	}
	f.transport.crash = false
	f.approve(t, recovery)
	if _, e = f.owner.Execute(context.Background(), recovery.PlanRef, "fixture"); e != nil {
		t.Fatal(e)
	}
}

func TestRuntimeConfigNativeRejectsChangedQualificationAndCapture(t *testing.T) {
	for _, kind := range []string{"qualification_digest", "capture_receipt", "capture_body"} {
		t.Run(kind, func(t *testing.T) {
			f := newInstallerNativeFixture(t)
			switch kind {
			case "qualification_digest":
				f.request.Qualification.Attempt.SHA256 = strings.Repeat("f", 64)
			case "capture_receipt":
				f.request.Capture.Receipt = f.request.Qualification.Receipt
			}
			if kind != "capture_body" {
				if _, e := f.owner.Plan(f.request, f.workID, "fixture"); e == nil {
					t.Fatal("accepted detached qualification")
				}
				if f.transport.starts != 0 {
					t.Fatal("launched before native qualification")
				}
				return
			}
			p := f.plan(t)
			f.approve(t, p)
			if e := os.WriteFile(f.request.Capture.Path, []byte(`{"services":{},"binding_cid":"drift"}`), 0600); e != nil {
				t.Fatal(e)
			}
			result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
			if e == nil || result.State != "failed" || f.transport.starts != 0 {
				t.Fatalf("capture drift launched: %+v %v", result, e)
			}
		})
	}
}

func TestRuntimeConfigNativeFinalizationFailureResumesWithoutRemoteReplay(t *testing.T) {
	f := newInstallerNativeFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	real := runtimeConfigCompleteAttempt
	runtimeConfigCompleteAttempt = func(string, string, string, string, string, string, time.Time) error {
		return errors.New("synthetic owner-proof persistence failure")
	}
	t.Cleanup(func() { runtimeConfigCompleteAttempt = real })
	first, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || first.State != "recovery_required" || first.ReceiptID == "" {
		t.Fatalf("lost finalization blocker: %+v %v", first, e)
	}
	s, _ := work.Open(f.credential.home)
	in, _, present, e := s.ReadReceiptedActionReservationInput(f.workID, p.Plan.ActionIntentID)
	if e != nil || !present || in.OutcomePending || in.Outcome != "completed" {
		t.Fatal("known observed outcome was lost", e)
	}
	if _, e = s.Advance(f.workID, work.AdvanceInput{To: "verifying", Actor: "fixture"}, f.credential.now); e == nil {
		t.Fatal("unfinished native proof released Work")
	}
	runtimeConfigCompleteAttempt = real
	second, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil || second.State != "installed_inactive" || second.ReceiptID != first.ReceiptID || f.transport.starts != 1 {
		t.Fatalf("local resume replayed remote or changed proof: %+v %v", second, e)
	}
	if _, _, present, e = s.ReadReceiptedActionReservationInput(f.workID, p.Plan.ActionIntentID); e != nil || present {
		t.Fatal("resumed proof did not clear", e)
	}
}

func TestRuntimeConfigNativeConcurrentRetryAndRecoveryRefuseLiveOwner(t *testing.T) {
	f := newInstallerNativeFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	f.transport.onStart = func() {
		second, err := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
		if err == nil || !strings.Contains(err.Error(), "execution_in_progress") || second.ReceiptID != "" {
			t.Fatalf("concurrent retry finalized live owner: %+v %v", second, err)
		}
		_, err = f.owner.PlanRecovery(p.PlanRef, "rollback", "unused", "fixture")
		if err == nil || !strings.Contains(err.Error(), "execution_in_progress") {
			t.Fatalf("concurrent recovery passed live owner: %v", err)
		}
		s, _ := work.Open(f.credential.home)
		in, _, present, err := s.ReadReceiptedActionReservationInput(f.workID, p.Plan.ActionIntentID)
		if err != nil || !present || !in.OutcomePending {
			t.Fatalf("live pending reservation changed: %+v %v", in, err)
		}
	}
	result, err := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if err != nil || result.State != "installed_inactive" || f.transport.starts != 1 {
		t.Fatalf("live owner failed after refused retries: %+v %v", result, err)
	}
}
