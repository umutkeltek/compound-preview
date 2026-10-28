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

type sourceMarkerTransportFixture struct {
	starts int
	fail   bool
}

func sourceMarkerObservationFixture(r policy.SourceMarkerRetirementRequest, operation string) SourceMarkerRetirementObservation {
	o := SourceMarkerRetirementObservation{Operation: operation, Changed: operation == "retire", SchemaVersion: sourceMarkerRetirementObservationSchema, RequestSHA256: policy.SourceMarkerRetirementDigest(r), ProgramSHA256: SourceMarkerRetirementProgramSHA256(), Transaction: ".smr-" + r.AttemptID, State: "source_marker_retired", HostBefore: r.Host, HostAfter: r.Host, JournalRetained: true, SourceMarkerRetired: true, MarkerInodePreserved: true, DatabaseClosed: true, ApplicationContainersAbsent: true}
	for i, f := range r.Files {
		o.RuntimeBefore[i] = sourceMarkerWithoutHash(*f.Stat)
		o.RuntimeAfter[i] = o.RuntimeBefore[i]
	}
	if o.Changed {
		s := sourceMarkerWithoutHash(*r.Marker.Stat)
		o.MarkerBefore = &s
	}
	o.MarkerAfter = sourceMarkerWithoutHash(*r.Marker.Stat)
	o.InactiveBefore = sourceMarkerWithoutHash(*r.Inactive.Stat)
	o.InactiveAfter = o.InactiveBefore
	return o
}
func (m *sourceMarkerTransportFixture) Start(ctx context.Context, r policy.SourceMarkerRetirementRequest, _ map[string]string, operation string, _ time.Time, guard sourceMarkerRetirementAdmissionGuard) (sourceMarkerRetirementChild, error) {
	if e := guard(func() error { m.starts++; return nil }); e != nil {
		return nil, e
	}
	raw, _ := json.Marshal(sourceMarkerObservationFixture(r, operation))
	child := &credentialMockChild{raw: raw}
	if m.fail {
		child.waitErr = errors.New("synthetic response loss")
	}
	return child, nil
}
func sourceMarkerFixture(t *testing.T) (*SourceMarkerRetirer, policy.SourceMarkerRetirementRequest, *sourceMarkerTransportFixture, string, time.Time) {
	t.Helper()
	f := newInstallerNativeFixture(t)
	f.request.Marker.Basename = ".runtime-staging-inactive"
	p := f.plan(t)
	f.approve(t, p)
	accepted, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	artifacts, _ := NewArtifactStore(f.credential.home)
	raw, e := artifacts.Get(accepted.ObservationRef)
	if e != nil {
		t.Fatal(e)
	}
	observation, e := decodeRuntimeConfigInstallObservation(raw, f.request)
	if e != nil {
		t.Fatal(e)
	}
	now := f.credential.now
	sha := strings.Repeat("a", 64)
	r := policy.SourceMarkerRetirementRequest{SchemaVersion: policy.SourceMarkerRetirementSchema, ProfileID: "fixture-retirement", AttemptID: now.Format("20060102t150405z") + "-bbbbbbbbbbbbbbbb", ProgramSHA256: SourceMarkerRetirementProgramSHA256(), EffectSet: policy.SourceMarkerRetirementEffects, CreatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339Nano), Installation: policy.SourceMarkerInstallation{PlanRef: p.PlanRef, WorkID: f.workID, ActionID: p.Plan.ActionIntentID, AttemptID: accepted.RunAttemptID, ReceiptID: accepted.ReceiptID, TransitionID: accepted.TransitionID, ObservationRef: accepted.ObservationRef, ObservationSHA256: runtimeConfigBytesHash(raw), RequestSHA256: p.Plan.RequestSHA256}, Resource: f.request.Resource, Host: f.request.Host, Root: policy.BackupCredentialStageRoot{Path: f.request.Root.Path}, RecoveryBasename: "fixture-source-marker-recovery", ContainerPrefix: "fixture", Database: policy.SourceMarkerRetirementDatabase{Name: f.request.Database.Name, OID: "12345", AllowConnections: f.request.Database.AllowConnections}, Executables: policy.SourceMarkerRetirementExecutables{DockerSHA256: sha, RunuserSHA256: sha, ClientPath: f.request.SSH.ClientPath, ClientSHA256: f.request.SSH.ClientSHA256, ClientVersion: f.request.SSH.ClientVersion}}
	ssh := f.request.SSH
	r.SSH = policy.BackupCredentialStageSSH{Alias: ssh.Alias, User: ssh.User, Port: ssh.Port, IdentityFile: ssh.IdentityFile, KnownHostsFile: ssh.KnownHostsFile, KnownHostsSHA256: ssh.KnownHostsSHA256, HostKeySHA256: ssh.HostKeySHA256, SSHSHA256: sha, InterpreterSHA256: ssh.InterpreterSHA256, SudoSHA256: ssh.SudoSHA256}
	for i, file := range f.request.Files {
		s := *observation.Postimages[i]
		r.Files[i] = policy.SourceMarkerRetirementFile{Basename: file.Basename, Path: file.Path, Stat: &s}
	}
	marker := sourceMarkerWithoutHash(*r.Files[0].Stat)
	marker.Inode = 700
	marker.Bytes = 20
	r.Marker = policy.SourceMarkerRetirementFile{Basename: r.Files[0].Basename + ".SOURCE_COMPARISON_ONLY", Path: filepath.Join(r.Root.Path, r.Files[0].Basename+".SOURCE_COMPARISON_ONLY"), Stat: &marker}
	inactive := sourceMarkerWithoutHash(f.request.Marker.Stat)
	r.Inactive = policy.SourceMarkerRetirementFile{Basename: f.request.Marker.Basename, Path: filepath.Join(r.Root.Path, f.request.Marker.Basename), Stat: &inactive}
	lock := marker
	lock.Inode = 701
	lock.Bytes = 0
	lock.Mode = "0644"
	r.MaintenanceLock = policy.SourceMarkerRetirementFile{Basename: "fixture-runtime-maintenance.lock", Path: "/run/lock/fixture-runtime-maintenance.lock", Stat: &lock}
	identity := marker
	identity.Bytes = 100
	r.SSH.IdentityStat = identity
	known := identity
	known.Inode = 702
	known.SHA256 = sha
	r.SSH.KnownHostsStat = known
	v, e := NewSourceMarkerRetirer(f.credential.home)
	if e != nil {
		t.Fatal(e)
	}
	v.now = func() time.Time { return now }
	m := &sourceMarkerTransportFixture{}
	v.transport = m
	w := nativeInstallerWork(t, v.home, "retire synthetic source marker after genuine native acceptance", "work_fixture_marker_retirement", now)
	return v, r, m, w.ID, now
}
func sourceMarkerApprove(t *testing.T, v *SourceMarkerRetirer, p SourceMarkerRetirementPlanResult, now time.Time) {
	t.Helper()
	cfg, _, e := policy.LoadConfig(v.home)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = policy.NewExactActionApprovalStore(v.home).Approve(p.Plan.ApprovalPlanID, "operator:fixture", cfg, now); e != nil {
		t.Fatal(e)
	}
}
func TestSourceMarkerRetirementRequiresGenuineNativeAcceptance(t *testing.T) {
	v, r, m, w, _ := sourceMarkerFixture(t)
	r.Installation.AttemptID = "ratt_20261023T010203Z_aaaaaaaa"
	if _, e := v.Plan(r, w, "fixture"); e == nil || m.starts != 0 {
		t.Fatal("accepted nonexistent installation attempt")
	}
}
func TestSourceMarkerRetirementNativeProofAndIndependentEffect(t *testing.T) {
	v, r, m, w, now := sourceMarkerFixture(t)
	p, e := v.Plan(r, w, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = v.Execute(context.Background(), p.PlanRef, "fixture"); e == nil || m.starts != 0 {
		t.Fatal("unapproved marker effect launched")
	}
	sourceMarkerApprove(t, v, p, now)
	out, e := v.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	a, e := runattempt.Open(v.home).Get(out.RunAttemptID)
	if e != nil || a.Harness != "compound" || a.Actor != "fixture" || a.Status != runattempt.StatusSucceeded || a.ActionIntentID != p.Plan.ActionIntentID || a.TerminalizationOwner != sourceMarkerRetirementOwner || a.Launch == nil || a.Launch.CommittedAt == nil || !out.SourceMarkerRetired || out.ApplicationActivated || out.ServiceActivated {
		t.Fatal("missing native marker proof")
	}
	events, e := eventstore.Open(v.home)
	if e != nil {
		t.Fatal(e)
	}
	record, e := events.Read(out.ReceiptID)
	if e != nil {
		t.Fatal(e)
	}
	var receipt envelope.Receipt
	if e = json.Unmarshal(record.Bytes, &receipt); e != nil {
		t.Fatal(e)
	}
	changed := receipt.StateDelta.FilesChanged
	if len(changed) != 2 || changed[0] != r.Marker.Path || changed[1] != filepath.Join(r.Root.Path, r.RecoveryBasename, r.Marker.Basename+".preimage") {
		t.Fatal("marker receipt attributed changes outside the exact marker rename")
	}
	s, _ := work.Open(v.home)
	_, _, present, e := s.ReadReceiptedActionReservationInput(w, p.Plan.ActionIntentID)
	if e != nil || present {
		t.Fatal("native marker completion did not converge")
	}
}
func TestSourceMarkerRetirementRecoveryIsSeparate(t *testing.T) {
	v, r, m, w, now := sourceMarkerFixture(t)
	p, e := v.Plan(r, w, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	sourceMarkerApprove(t, v, p, now)
	m.fail = true
	failed, e := v.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || failed.State != "recovery_required" {
		t.Fatal("lost response did not retain recovery obligation")
	}
	nativeInstallerWork(t, v.home, "reconcile synthetic marker", failed.RecoveryWorkID, now)
	recovery, e := v.PlanRecovery(p.PlanRef, "reconcile", failed.RecoveryWorkID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	broken := sourceMarkerMissingPlanField(t, v, recovery.PlanRef, "recovery", "transport_program_sha256")
	if _, _, e = v.loadPlan(broken, "fixture"); e == nil {
		t.Fatal("fresh current recovery accepted missing transport pin")
	}
	sourceMarkerApprove(t, v, recovery, now)
	m.fail = false
	out, e := v.Execute(context.Background(), recovery.PlanRef, "fixture")
	if e != nil || !out.SourceMarkerRetired {
		t.Fatal("native marker recovery failed", e)
	}
}
func TestSourceMarkerRetirementDoesNotWeakenGenericProtectedPaths(t *testing.T) {
	v, r, _, w, now := sourceMarkerFixture(t)
	ai := sourceMarkerRetirementIntent(r, w, "fixture", strings.Repeat("b", 64), now)
	cfg, _, _ := policy.LoadConfig(v.home)
	if policy.Evaluate(ai, cfg, now).Outcome != "deny" {
		t.Fatal("generic env protected path was weakened")
	}
	if pd, ok := policy.EvaluateSourceMarkerRetirement(ai, r, SourceMarkerRetirementProgramSHA256(), cfg, policy.EvaluationContext{}, now); !ok || pd.Outcome != "hold" {
		t.Fatal("typed marker requires one-use approval")
	}
	cfg.ForbiddenPaths = append(cfg.ForbiddenPaths, r.Marker.Path)
	if pd, ok := policy.EvaluateSourceMarkerRetirement(ai, r, SourceMarkerRetirementProgramSHA256(), cfg, policy.EvaluationContext{}, now); ok || pd.Outcome != "deny" {
		t.Fatal("custom marker denial bypassed")
	}
}
func TestSourceMarkerRetirementObservationRejectsChanges(t *testing.T) {
	_, r, _, _, _ := sourceMarkerFixture(t)
	valid := sourceMarkerObservationFixture(r, "retire")
	for _, mutate := range []func(*SourceMarkerRetirementObservation){func(o *SourceMarkerRetirementObservation) { o.RuntimeAfter[0].Inode++ }, func(o *SourceMarkerRetirementObservation) { o.RuntimeAfter[1].CtimeNS = "1" }, func(o *SourceMarkerRetirementObservation) { o.MarkerAfter.Inode++ }, func(o *SourceMarkerRetirementObservation) { o.InactiveAfter.Mode = "0644" }, func(o *SourceMarkerRetirementObservation) { o.CredentialBodiesOpened = true }, func(o *SourceMarkerRetirementObservation) { o.DatabaseClosed = false }, func(o *SourceMarkerRetirementObservation) { o.ApplicationContainersAbsent = false }, func(o *SourceMarkerRetirementObservation) { o.ServiceActivated = true }} {
		o := valid
		mutate(&o)
		raw, _ := json.Marshal(o)
		if _, e := decodeSourceMarkerRetirementObservation(raw, r); e == nil {
			t.Fatal("accepted changed marker postcondition")
		}
	}
}
func TestSourceMarkerRetirementProgramSyntheticMain(t *testing.T) {
	runSyntheticProgramSuite(t, "source_marker_retirement_program_test.py")
}

func TestSourceMarkerRetirementRejectsChangedWorkGeneration(t *testing.T) {
	v, r, m, w, now := sourceMarkerFixture(t)
	p, e := v.Plan(r, w, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	sourceMarkerApprove(t, v, p, now)
	s, e := work.Open(v.home)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Advance(w, work.AdvanceInput{To: "verifying", Actor: "fixture"}, now); e != nil {
		t.Fatal(e)
	}
	if _, e = v.Execute(context.Background(), p.PlanRef, "fixture"); e == nil || m.starts != 0 {
		t.Fatal("stale Work generation launched marker rename")
	}
}

// These fixtures build the pre-repair immutable protocol through native stores.
// They never change production validation, edit ledger bytes or launch a process.
func sourceMarkerHistoricalRecoveryPlan(t *testing.T, v *SourceMarkerRetirer, original SourceMarkerRetirementPlanResult, failed SourceMarkerRetirementResult, now time.Time) SourceMarkerRetirementPlanResult {
	t.Helper()
	if SourceMarkerRetirementProgramSHA256() != sourceMarkerLegacyProgramSHA256 {
		t.Fatal("historical fixture must use exact legacy program bytes")
	}
	origin := &SourceMarkerRecoveryOrigin{PlanRef: original.PlanRef, WorkID: original.Plan.WorkID, ActionID: original.Plan.ActionIntentID, AttemptID: failed.RunAttemptID, ReceiptID: failed.ReceiptID, TransitionID: failed.TransitionID, RequestSHA256: original.Plan.RequestSHA256}
	w, err := v.requireWork(failed.RecoveryWorkID, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	ai := sourceMarkerOperationIntent(original.Plan.Request, w.ID, "fixture", sourceMarkerRetirementHash(w), "reconcile", origin, now)
	cfg, _, err := policy.LoadConfig(v.home)
	if err != nil {
		t.Fatal(err)
	}
	pd, eligible := policy.EvaluateSourceMarkerRecovery(ai, original.Plan.Request, origin.ActionID, origin.AttemptID, "reconcile", sourceMarkerLegacyProgramSHA256, cfg, policy.EvaluationContext{}, now)
	if !eligible || pd.Outcome != "hold" {
		t.Fatal("historical fixture policy refused")
	}
	if err = recordEnvelope(v.home, ai.ID, &ai); err != nil {
		t.Fatal(err)
	}
	if err = recordEnvelope(v.home, pd.ID, &pd); err != nil {
		t.Fatal(err)
	}
	ap, _, _, err := policy.NewExactActionApprovalStore(v.home).EnsurePlanForIntent(ai, cfg, policy.SourceMarkerRetirementApprovalTTL, now)
	if err != nil {
		t.Fatal(err)
	}
	p := SourceMarkerRetirementPlan{Operation: "reconcile", Recovery: origin, SchemaVersion: sourceMarkerRetirementPlanSchema, Request: original.Plan.Request, RequestSHA256: original.Plan.RequestSHA256, WorkID: w.ID, Actor: "fixture", WorkSHA256: sourceMarkerRetirementHash(w), ActionIntentID: ai.ID, PolicyDecisionID: pd.ID, ApprovalPlanID: ap.ID, ApprovalBindingDigest: ap.BindingDigest, ApprovalExpiresAt: ap.ExpiresAt}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "transport_program_sha256") {
		t.Fatal("historical fixture contains future field")
	}
	store, err := NewArtifactStore(v.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(raw)
	if err != nil {
		t.Fatal(err)
	}
	return SourceMarkerRetirementPlanResult{PlanRef: ref, Plan: p, PolicyOutcome: pd.Outcome}
}

func sourceMarkerHistoricalFailedRecovery(t *testing.T, v *SourceMarkerRetirer, plan SourceMarkerRetirementPlanResult, now time.Time) SourceMarkerRetirementResult {
	t.Helper()
	p, ai, err := v.loadPlan(plan.PlanRef, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := policy.LoadConfig(v.home)
	if err != nil {
		t.Fatal(err)
	}
	base, eligible := policy.EvaluateSourceMarkerRecovery(ai, p.Request, p.Recovery.ActionID, p.Recovery.AttemptID, p.Operation, sourceMarkerLegacyProgramSHA256, cfg, policy.EvaluationContext{}, now)
	if !eligible {
		t.Fatal("historical fixture was not eligible")
	}
	pd, matched, err := policy.NewExactActionApprovalStore(v.home).ApplyMatching(ai, cfg, base, now)
	if err != nil || !matched || !policy.Allowed(pd) {
		t.Fatal("historical fixture approval not consumed", err)
	}
	if err = recordEnvelope(v.home, pd.ID, &pd); err != nil {
		t.Fatal(err)
	}
	attemptTime, err := time.Parse("20060102t150405z", p.Request.AttemptID[:16])
	if err != nil {
		t.Fatal(err)
	}
	attempts := runattempt.Open(v.home)
	id := runattempt.MintDeterministic("source-marker-recovery:"+ai.ID, attemptTime)
	attempt, err := attempts.MintWithID(id, p.WorkID, p.Actor, "compound", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = attempts.AttachActionIntent(id, ai.ID); err != nil {
		t.Fatal(err)
	}
	w, err := v.requireWork(p.WorkID, p.Actor)
	if err != nil {
		t.Fatal(err)
	}
	pending := work.ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: w.State, OutcomePending: true, Outcome: "blocked", Summary: "synthetic historical reconcile response pending", MoveType: "audit_only", ProofRefs: append([]string{ai.ID, attempt.ID, "policy-decision:" + pd.ID}, p.Recovery.proofRefs()...), BeforeRefs: []string{"request-sha256:" + p.RequestSHA256, "request-sha256:" + p.RequestSHA256}, FilesChanged: []string{}, RequiredAttemptCompletion: &work.RequiredAttemptCompletion{AttemptID: id, Owner: sourceMarkerRecoveryOwner}}
	s, err := work.Open(v.home)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveReceiptedAction(p.WorkID, pending, now, func() error { return policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, pd) }); err != nil {
		t.Fatal(err)
	}
	if _, err = attempts.CommitLaunch(id, now, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = attempts.MarkRunning(id, now); err != nil {
		t.Fatal(err)
	}
	out, err := v.finish(SourceMarkerRetirementResult{RunAttemptID: id, ApprovalConsumed: true}, p, ai, pd, "failed", nil, "verification_failed", &sourceMarkerRetirementReservation{input: pending, authorizedAt: now, launched: true}, context.Background())
	if err == nil || out.State != "recovery_required" || out.ReceiptID == "" || out.TransitionID == "" {
		t.Fatal("historical native failure proof missing", err)
	}
	return out
}

func sourceMarkerMissingPlanField(t *testing.T, v *SourceMarkerRetirer, ref string, path ...string) string {
	t.Helper()
	store, err := NewArtifactStore(v.home)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	node := fields
	for _, part := range path[:len(path)-1] {
		node = node[part].(map[string]any)
	}
	delete(node, path[len(path)-1])
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	broken, err := store.Put(raw)
	if err != nil {
		t.Fatal(err)
	}
	return broken
}

func TestSourceMarkerRetirementLegacyHistoryRequiresFreshTransportApproval(t *testing.T) {
	currentProgram := sourceMarkerRetirementProgram
	legacy, err := os.ReadFile("testdata/source_marker_retirement_legacy_v0_1.py")
	if err != nil {
		t.Fatal(err)
	}
	if runtimeConfigBytesHash(legacy) != sourceMarkerLegacyProgramSHA256 {
		t.Fatal("legacy fixture drift")
	}
	sourceMarkerRetirementProgram = string(legacy)
	t.Cleanup(func() { sourceMarkerRetirementProgram = currentProgram })
	v, r, m, w, now := sourceMarkerFixture(t)
	clock := now
	v.now = func() time.Time { return clock }
	original, err := v.Plan(r, w, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	sourceMarkerApprove(t, v, original, clock)
	m.fail = true
	failed, err := v.Execute(context.Background(), original.PlanRef, "fixture")
	if err == nil || failed.State != "recovery_required" {
		t.Fatal("historical original did not fail", err)
	}
	nativeInstallerWork(t, v.home, "reconcile synthetic historical marker", failed.RecoveryWorkID, clock)
	clock = clock.Add(time.Second)
	historical := sourceMarkerHistoricalRecoveryPlan(t, v, original, failed, clock)
	sourceMarkerApprove(t, v, historical, clock)
	sourceMarkerRetirementProgram = currentProgram
	if _, _, err = v.loadPlan(original.PlanRef, "fixture"); err != nil {
		t.Fatal("old original no longer loads", err)
	}
	if _, _, err = v.loadPlan(historical.PlanRef, "fixture"); err != nil {
		t.Fatal("old reconcile no longer loads", err)
	}
	starts := m.starts
	if _, err = v.Execute(context.Background(), historical.PlanRef, "fixture"); err == nil || m.starts != starts || !strings.Contains(err.Error(), "transport_program_binding") {
		t.Fatal("old approval launched repaired transport", err)
	}
	sourceMarkerRetirementProgram = string(legacy)
	failedRecovery := sourceMarkerHistoricalFailedRecovery(t, v, historical, clock)
	sourceMarkerRetirementProgram = currentProgram
	for _, ref := range []string{original.PlanRef, historical.PlanRef} {
		if _, _, err = v.loadPlan(ref, "fixture"); err != nil {
			t.Fatal("immutable failed plan no longer loads", err)
		}
	}
	events, err := eventstore.Open(v.home)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{failed.ReceiptID, failedRecovery.ReceiptID} {
		record, readErr := events.Read(id)
		var receipt envelope.Receipt
		if readErr != nil || json.Unmarshal(record.Bytes, &receipt) != nil || receipt.Validate() != nil || receipt.Outcome != "failed" || receipt.StateDelta.ChangeKnowledge != "unknown" {
			t.Fatal("historical failed receipt invalid", readErr)
		}
	}
	clock = clock.Add(time.Second)
	fresh, err := v.PlanRecovery(original.PlanRef, "reconcile", failed.RecoveryWorkID, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Plan.WorkID != failed.RecoveryWorkID || fresh.Plan.RequestSHA256 != original.Plan.RequestSHA256 || fresh.Plan.Recovery.TransportProgramSHA256 != SourceMarkerRetirementProgramSHA256() {
		t.Fatal("fresh recovery did not retain original request and reserved work")
	}
	_, freshAI, err := v.loadPlan(fresh.PlanRef, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	_, oldAI, err := v.loadPlan(historical.PlanRef, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if freshAI.ArgumentsHash != oldAI.ArgumentsHash {
		t.Fatal("original recovery commitment changed")
	}
	unpinned := *fresh.Plan.Recovery
	unpinned.TransportProgramSHA256 = ""
	unpinnedAI := sourceMarkerOperationIntent(fresh.Plan.Request, fresh.Plan.WorkID, fresh.Plan.Actor, fresh.Plan.WorkSHA256, fresh.Plan.Operation, &unpinned, clock)
	if policy.CanonicalFullIntentDigest(unpinnedAI) == policy.CanonicalFullIntentDigest(freshAI) {
		t.Fatal("transport code is absent from full approval binding")
	}
	cfg, _, err := policy.LoadConfig(v.home)
	if err != nil {
		t.Fatal(err)
	}
	if policy.ActionBindingDigest(policy.ActionBindingForIntent(unpinnedAI, cfg)) == policy.ActionBindingDigest(policy.ActionBindingForIntent(freshAI, cfg)) {
		t.Fatal("transport code is absent from one-use approval binding")
	}
	for _, path := range [][]string{{"recovery", "transport_program_sha256"}, {"recovery", "request_sha256"}, {"request", "ssh", "interpreter_sha256"}, {"work_id"}} {
		broken := sourceMarkerMissingPlanField(t, v, fresh.PlanRef, path...)
		if _, _, err = v.loadPlan(broken, "fixture"); err == nil {
			t.Fatal("fresh plan accepted missing field", path)
		}
	}
	if _, err = v.Execute(context.Background(), fresh.PlanRef, "fixture"); err == nil || m.starts != starts {
		t.Fatal("fresh transport reused historical approval", err)
	}
	sourceMarkerApprove(t, v, fresh, clock)
	m.fail = false
	recovered, err := v.Execute(context.Background(), fresh.PlanRef, "fixture")
	if err != nil || !recovered.SourceMarkerRetired {
		t.Fatal("fresh exact recovery failed", err)
	}
	s, err := work.Open(v.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, present, readErr := s.ReadReceiptedActionReservationInput(original.Plan.WorkID, original.Plan.ActionIntentID); readErr != nil || present {
		t.Fatal("original recovery commitment did not clear", readErr)
	}
	for _, prior := range []SourceMarkerRetirementResult{failed, failedRecovery} {
		attempt, readErr := runattempt.Open(v.home).Get(prior.RunAttemptID)
		if readErr != nil || attempt.Status != runattempt.StatusFailed || attempt.TerminalizationReceiptID != prior.ReceiptID || attempt.TerminalizationTransitionID != prior.TransitionID {
			t.Fatal("historical failure was rewritten", readErr)
		}
	}
}

func TestSourceMarkerRetirementFreshCurrentPlanRequiresEveryField(t *testing.T) {
	v, r, _, w, _ := sourceMarkerFixture(t)
	p, err := v.Plan(r, w, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{{"operation"}, {"request", "program_sha256"}, {"request", "ssh", "interpreter_sha256"}} {
		broken := sourceMarkerMissingPlanField(t, v, p.PlanRef, path...)
		if _, _, err = v.loadPlan(broken, "fixture"); err == nil {
			t.Fatal("fresh plan accepted missing field", path)
		}
	}
	r.ProgramSHA256 = sourceMarkerLegacyProgramSHA256
	if _, err = v.Plan(r, w, "fixture"); err == nil {
		t.Fatal("new retirement accepted legacy request pin")
	}
}
