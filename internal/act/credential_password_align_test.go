package act

import (
	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type alignmentMockTransport struct {
	starts  int
	stage   func()
	fail    bool
	waitErr error
	action  string
	catalog policy.CredentialPasswordCatalogPin
}

func (m *alignmentMockTransport) Start(ctx context.Context, r policy.CredentialPasswordAlignmentRequest, action string, deadline time.Time, guard credentialPasswordAlignAdmissionGuard) (credentialPasswordAlignChild, error) {
	if m.stage != nil {
		m.stage()
	}
	if e := guard(func() error { m.starts++; return nil }); e != nil {
		return nil, e
	}
	o := CredentialPasswordAlignmentObservation{SchemaVersion: "compound.credential_password_alignment_observation.v0.1", Operation: r.Operation, ActionID: action, OriginalActionID: credentialPasswordAlignOriginalAction(r, action), RequestSHA256: policy.CredentialPasswordAlignmentDigest(r), Closed: true, ProtectedFileUnchanged: true, AttributesUnchanged: true, ResolvedRecoveryActions: []string{}}
	switch r.Operation {
	case "inspect":
		o.Classification = "metadata_only"
		o.Effect = "observed"
		o.Catalog = &m.catalog
	case "align":
		o.Classification = "new_new"
		o.Effect = "committed"
	case "reconcile":
		o.Classification = "new_new"
		o.Effect = "reconciled_new_new"
	case "restore":
		o.Classification = "new_new"
		o.Effect = "restored"
	}
	raw, _ := json.Marshal(o)
	m.action = action
	child := &credentialMockChild{raw: raw}
	if m.fail {
		child.waitErr = credentialVerifyError("synthetic_failed")
	}
	if m.waitErr != nil {
		child.waitErr = m.waitErr
	}
	return child, nil
}

func alignmentTestWork(t *testing.T, h string, now time.Time) string {
	t.Helper()
	s, e := work.Open(h)
	if e != nil {
		t.Fatal(e)
	}
	w, e := s.Create(work.CreateInput{WorkID: idFor("work_", "alignment-fixture", time.Now().Format(time.RFC3339Nano)), Owner: "fixture", SuccessPredicate: "closed native password capability evidence"}, now.Add(-time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	w, e = s.Claim(w.ID, "fixture", now.Add(-30*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	w, e = s.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "fixture"}, now.Add(-20*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	return w.ID
}

type alignmentFixture struct {
	linuxMetadata *alignmentLinuxMetadata
	installer     *installerNativeFixture
	owner         *CredentialPasswordAligner
	r             policy.CredentialPasswordAlignmentRequest
	transport     *alignmentMockTransport
	workID        string
	now           time.Time
}

func newAlignmentFixture(t *testing.T) *alignmentFixture {
	return newConfiguredAlignmentFixture(t, nil)
}

func newConfiguredAlignmentFixture(t *testing.T, configure func(*credentialFixture)) *alignmentFixture {
	return newConfiguredAlignmentProfileFixture(t, configure, nil)
}

func newConfiguredAlignmentProfileFixture(t *testing.T, configure func(*credentialFixture), configureProfile func(*policy.RuntimeConfigInstallProfile)) *alignmentFixture {
	t.Helper()
	i := newInstallerProfileFixture(t, configureProfile)
	c := i.credential
	c.workID = alignmentTestWork(t, c.home, c.now)
	c.request.AttemptID = c.now.Format("20060102t150405z") + "-bbbbbbbbbbbbbbbb"
	if configure != nil {
		configure(c)
	} else {
		c.transport.child.waitErr = credentialVerifyStageError{stage: "app_existing"}
	}
	p := c.plan(t)
	c.approve(t, p)
	failed, e := c.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || failed.State != "failed" {
		t.Fatal("missing original failure", e)
	}
	store, _ := NewArtifactStore(c.home)
	var receipt envelope.Receipt
	events := &CredentialPasswordAligner{home: c.home}
	if events.event(failed.ReceiptID, &receipt) != nil {
		t.Fatal("failure receipt")
	}
	failure := policy.CredentialPasswordFailureOrigin{PlanRef: p.PlanRef, WorkID: c.workID, ActionID: p.Plan.ActionIntentID, AttemptID: failed.RunAttemptID, ReceiptID: failed.ReceiptID, TransitionID: failed.TransitionID, RequestSHA256: p.Plan.RequestSHA256}
	for _, ref := range receipt.ProofRefs {
		if !strings.HasPrefix(ref, "artifact:sha256:") {
			continue
		}
		raw, _ := store.Get(ref)
		var status struct {
			PolicyID string `json:"policy_decision_id"`
		}
		_ = json.Unmarshal(raw, &status)
		if status.PolicyID != "" {
			failure.PolicyID = status.PolicyID
		}
	}
	var pd envelope.PolicyDecision
	if events.event(failure.PolicyID, &pd) != nil {
		t.Fatal("approved failure policy")
	}
	for _, reason := range pd.Reasons {
		if strings.HasPrefix(reason, "exact one-use operator approval ") {
			failure.ApprovalGrantID = strings.TrimSuffix(strings.TrimPrefix(reason, "exact one-use operator approval "), " matches action binding")
		}
	}
	o, e := NewCredentialPasswordAligner(c.home)
	if e != nil {
		t.Fatal(e)
	}
	o.now = func() time.Time { return c.now }
	m := &alignmentMockTransport{catalog: policy.CredentialPasswordCatalogPin{SHA256: strings.Repeat("a", 64), DatabaseOID: 16384, DockerSHA256: strings.Repeat("d", 64), Roles: [2]policy.CredentialPasswordRolePin{{OID: 16385, Xmin: "100", CTID: "(0,1)"}, {OID: 16386, Xmin: "100", CTID: "(0,2)"}}}}
	o.transport = m
	r := policy.CredentialPasswordAlignmentRequest{CredentialVerificationRequest: c.request, Operation: "inspect", Failure: failure, Profile: i.request.Profile, Capture: i.request.Capture, Catalog: policy.CredentialPasswordCatalogPin{DockerSHA256: m.catalog.DockerSHA256}}
	r.SchemaVersion = policy.CredentialPasswordAlignSchema
	r.EffectSet = policy.CredentialPasswordAlignEffects
	r.ProgramSHA256 = CredentialPasswordAlignmentProgramSHA256()
	r.AttemptID = c.now.Format("20060102t150405z") + "-cccccccccccccccc"
	return &alignmentFixture{installer: i, owner: o, r: r, transport: m, workID: alignmentTestWork(t, c.home, c.now), now: c.now}
}
func (f *alignmentFixture) approve(t *testing.T, p CredentialPasswordAlignmentPlanResult) {
	t.Helper()
	cfg, _, e := policy.LoadConfig(f.owner.home)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = policy.NewExactActionApprovalStore(f.owner.home).Approve(p.Plan.ApprovalPlanID, "operator:fixture", cfg, f.now); e != nil {
		t.Fatal(e)
	}
}

func TestCredentialPasswordAlignmentNativeMetadataHasActualOneUseOwnerProof(t *testing.T) {
	f := newAlignmentFixture(t)
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if result.State != "completed" || !result.ApprovalConsumed || f.transport.starts != 1 || result.ConfigurationAccepted {
		t.Fatalf("wrong metadata-only result: %+v", result)
	}
	a, e := runattempt.Open(f.owner.home).Get(result.RunAttemptID)
	if e != nil || a.TerminalizationOwner != credentialPasswordAlignOwnerFor("inspect") || a.TerminalizationReceiptID != result.ReceiptID || a.Status != runattempt.StatusSucceeded {
		t.Fatal("missing actual native owner terminal proof", e)
	}
	repeated, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil || f.transport.starts != 1 || repeated.RunAttemptID != result.RunAttemptID || repeated.ReceiptID != result.ReceiptID {
		t.Fatal("exact retry did not preserve the original one-use proof", e)
	}
}

func TestCredentialPasswordAlignmentNativeMetadataRequiresOriginalGenuineFailure(t *testing.T) {
	for _, mutate := range []func(*policy.CredentialPasswordAlignmentRequest){func(r *policy.CredentialPasswordAlignmentRequest) {
		r.Failure.ApprovalGrantID = "approval_grant_ffffffffffff"
	}, func(r *policy.CredentialPasswordAlignmentRequest) { r.Failure.AttemptID = "ratt_substituted" }, func(r *policy.CredentialPasswordAlignmentRequest) { r.File.Inode++ }, func(r *policy.CredentialPasswordAlignmentRequest) { r.Profile.Containers[0] = "invented.example" }} {
		f := newAlignmentFixture(t)
		mutate(&f.r)
		if _, e := f.owner.Plan(f.r, f.workID, "fixture"); e == nil || f.transport.starts != 0 {
			t.Fatal("substituted original failure/profile admitted")
		}
	}
}
