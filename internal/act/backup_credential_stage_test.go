package act

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

type backupStageTransportFixture struct {
	starts int
	fail   bool
	r      policy.BackupCredentialStageRequest
}

func (m *backupStageTransportFixture) Start(ctx context.Context, r policy.BackupCredentialStageRequest, selected map[string]string, operation string, _ time.Time, guard backupCredentialStageAdmissionGuard) (backupCredentialStageChild, error) {
	if e := guard(func() error { m.starts++; return nil }); e != nil {
		return nil, e
	}
	m.r = r
	for i, f := range r.Files {
		selected[fmt.Sprintf("source_sha256_%d", i)] = f.Stat.SHA256
		if f.Stat.SHA256 == "" {
			selected[fmt.Sprintf("source_sha256_%d", i)] = runtimeConfigBytesHash([]byte("synthetic-credential-" + f.Basename))
		}
	}
	o := BackupCredentialStageObservation{Operation: operation, Changed: operation == "stage", SchemaVersion: backupCredentialStageObservationSchema, RequestSHA256: policy.BackupCredentialStageDigest(r), ProgramSHA256: BackupCredentialStageProgramSHA256(), Transaction: ".bcs-" + r.AttemptID, State: "staged_backup_credentials", HostBefore: r.Host, HostAfter: r.Host, JournalRetained: true, CredentialsStaged: true}
	for i, f := range r.Files {
		s := *f.Stat
		s.SHA256 = selected[fmt.Sprintf("source_sha256_%d", i)]
		s.UID = 0
		s.GID = 0
		s.Device = 1
		s.Inode = uint64(i + 100)
		o.Postimages[i] = &s
		o.Written[i] = o.Changed
	}
	raw, _ := json.Marshal(o)
	child := &credentialMockChild{raw: raw}
	if m.fail {
		child.waitErr = errors.New("synthetic transport loss")
	}
	return child, nil
}
func backupStageFixture(t *testing.T) (*BackupCredentialStager, policy.BackupCredentialStageRequest, *backupStageTransportFixture, string, time.Time) {
	t.Helper()
	h := t.TempDir()
	sourceBase, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(sourceBase, "secrets")
	if e := os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(source, 0700); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	sha := strings.Repeat("a", 64)
	r := policy.BackupCredentialStageRequest{SchemaVersion: policy.BackupCredentialStageSchema, ProfileID: "fixture-stage", AttemptID: now.Format("20060102t150405z") + "-aaaaaaaaaaaaaaaa", ProgramSHA256: BackupCredentialStageProgramSHA256(), EffectSet: policy.BackupCredentialStageEffects, CreatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339Nano), Resource: policy.RuntimeConfigInstallResource{Account: "000000000000", Region: "fixture-region", Instance: "fixture:instance:example", Address: "127.0.0.1"}, SSH: policy.BackupCredentialStageSSH{Alias: "fixture-target", User: "fixture", Port: 22, IdentityFile: "/fixture/identity", KnownHostsFile: "/fixture/known_hosts", KnownHostsSHA256: sha, HostKeySHA256: "SHA256:" + strings.Repeat("A", 43), SSHSHA256: sha, InterpreterSHA256: sha, SudoSHA256: sha}, Host: policy.BackupCredentialStageHost{Hostname: "host.fixture.invalid", MachineID: strings.Repeat("a", 32), BootID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}, SourceDirectory: source, Root: policy.BackupCredentialStageRoot{Path: "/root/private-recovery/fixture-stage"}}
	names := [4]string{"pgbackrest-secrets.conf", "restic-credentials.json", "restic-password", "offhost-escrow.json"}
	for i, name := range names {
		raw := []byte("synthetic-credential-" + name)
		path := filepath.Join(source, name)
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
		info, _ := os.Lstat(path)
		s, e := backupCredentialStageFileStat(info, runtimeConfigBytesHash(raw))
		if e != nil {
			t.Fatal(e)
		}
		r.Files[i] = policy.BackupCredentialStageFile{Basename: name, SourcePath: path, Path: filepath.Join(r.Root.Path, name), Stat: &s}
	}
	info, _ := os.Lstat(source)
	r.SourceDirectoryStat, _ = backupCredentialStageFileStat(info, "")
	r.SSH.IdentityStat = *r.Files[0].Stat
	r.SSH.IdentityStat.SHA256 = ""
	r.SSH.KnownHostsStat = *r.Files[1].Stat
	owner, e := NewBackupCredentialStager(h)
	if e != nil {
		t.Fatal(e)
	}
	owner.now = func() time.Time { return now }
	transport := &backupStageTransportFixture{}
	owner.transport = transport
	id := "work_fixture_backup_stage"
	nativeInstallerWork(t, h, "synthetic four-file staging", id, now)
	return owner, r, transport, id, now
}
func backupStageApprove(t *testing.T, v *BackupCredentialStager, p BackupCredentialStagePlanResult, now time.Time) {
	t.Helper()
	cfg, _, e := policy.LoadConfig(v.home)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = policy.NewExactActionApprovalStore(v.home).Approve(p.Plan.ApprovalPlanID, "operator:fixture", cfg, now); e != nil {
		t.Fatal(e)
	}
}
func TestBackupCredentialStageNativeAuthority(t *testing.T) {
	v, r, m, w, now := backupStageFixture(t)
	p, e := v.Plan(r, w, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = v.Execute(context.Background(), p.PlanRef, "fixture"); e == nil || m.starts != 0 {
		t.Fatal("unapproved stage launched")
	}
	backupStageApprove(t, v, p, now)
	out, e := v.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	a, e := runattempt.Open(v.home).Get(out.RunAttemptID)
	if e != nil || a.Status != runattempt.StatusSucceeded || a.WorkID != w || a.Actor != "fixture" || a.Harness != "compound" || a.ActionIntentID != p.Plan.ActionIntentID || a.Launch == nil || a.Launch.CommittedAt == nil || a.TerminalizationOwner != backupCredentialStageOwner || a.TerminalizationReceiptID != out.ReceiptID || a.TerminalizationTransitionID != out.TransitionID || !out.CredentialsStaged || out.ApplicationActivated || out.ServiceActivated || m.starts != 1 {
		t.Fatal("missing honest native stage proof")
	}
	again, e := v.Execute(context.Background(), p.PlanRef, "fixture")
	if m.starts != 1 || (e == nil && again.RunAttemptID != out.RunAttemptID) {
		t.Fatal("exact retry repeated transport")
	}
}
func TestBackupCredentialStageRejectsGenerationDrift(t *testing.T) {
	v, r, m, w, now := backupStageFixture(t)
	p, e := v.Plan(r, w, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	backupStageApprove(t, v, p, now)
	s, _ := work.Open(v.home)
	if _, e = s.Advance(w, work.AdvanceInput{To: "running", Actor: "fixture", ArtifactID: "evidence:synthetic-generation-change"}, now.Add(time.Second)); e != nil {
		t.Fatal(e)
	}
	if _, e = v.Execute(context.Background(), p.PlanRef, "fixture"); e == nil || m.starts != 0 {
		t.Fatal("changed generation launched")
	}
}
func TestBackupCredentialStageNativeReconcile(t *testing.T) {
	v, r, m, w, now := backupStageFixture(t)
	p, e := v.Plan(r, w, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	backupStageApprove(t, v, p, now)
	m.fail = true
	failed, e := v.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || failed.State != "recovery_required" || failed.RecoveryWorkID == "" {
		t.Fatal("uncertain stage did not freeze recovery")
	}
	nativeInstallerWork(t, v.home, "reconcile synthetic backup stage", failed.RecoveryWorkID, now)
	recovery, e := v.PlanRecovery(p.PlanRef, "reconcile", failed.RecoveryWorkID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	backupStageApprove(t, v, recovery, now)
	m.fail = false
	out, e := v.Execute(context.Background(), recovery.PlanRef, "fixture")
	if e != nil || !out.CredentialsStaged {
		t.Fatal("native recovery failed", e)
	}
	s, _ := work.Open(v.home)
	_, _, present, e := s.ReadReceiptedActionReservationInput(w, p.Plan.ActionIntentID)
	if e != nil || present {
		t.Fatal("original recovery blocker did not converge")
	}
}
func TestBackupCredentialStageSourceAndObservationFences(t *testing.T) {
	v, r, _, w, _ := backupStageFixture(t)
	bad := r
	bad.Files[0].Basename = "unexpected"
	if _, e := v.Plan(bad, w, "fixture"); e == nil {
		t.Fatal("arbitrary file admitted")
	}
	raw, e := backupCredentialStageReadSources(r)
	if e != nil {
		t.Fatal(e)
	}
	for i := range raw {
		clear(raw[i])
	}
	if e = os.Remove(r.Files[0].SourcePath); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(r.Files[1].SourcePath, r.Files[0].SourcePath); e != nil {
		t.Fatal(e)
	}
	if _, e = backupCredentialStageReadSources(r); e == nil {
		t.Fatal("symlink source accepted")
	}
}

func TestBackupCredentialStageProgramSyntheticFences(t *testing.T) {
	runSyntheticProgramSuite(t, "backup_credential_stage_program_test.py")
}

// runSyntheticProgramSuite runs a synthetic Python suite under the inherited
// TMPDIR and again under a TMPDIR whose group is not the caller's egid. BSD
// filesystems (macOS) always give a new entry its parent directory's group,
// so a verifier TMPDIR under root:wheel /private/tmp made every fixture file
// gid 0; Linux does the same under a setgid directory. Owner fences in the
// synthetic suites must hold on both hosts, not only where TMPDIR is egid's.
func runSyntheticProgramSuite(t *testing.T, script string) {
	t.Helper()
	run := func(label string, env []string) {
		command := exec.Command("python3", "-I", "-B", script)
		command.Env = env
		if raw, e := command.CombinedOutput(); e != nil {
			t.Fatalf("synthetic suite %s failed under %s: %v %s", script, label, e, raw)
		}
	}
	run("inherited TMPDIR", os.Environ())
	foreign, gid, ok := foreignGroupTempDir(t)
	if !ok {
		t.Skip("host offers no group other than the caller's egid; foreign-group TMPDIR case needs one")
	}
	run(fmt.Sprintf("foreign-group TMPDIR (gid %d, egid %d)", gid, os.Getegid()), append(os.Environ(), "TMPDIR="+foreign))
}

// foreignGroupTempDir returns a private setgid directory owned by a group
// other than the caller's egid, so entries created inside inherit that group
// on BSD and Linux alike.
func foreignGroupTempDir(t *testing.T) (string, int, bool) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "foreign-group")
	if e := os.Mkdir(dir, 0o700); e != nil {
		t.Fatal(e)
	}
	egid := os.Getegid()
	var candidates []int
	if os.Geteuid() == 0 {
		candidates = append(candidates, egid+1)
	}
	groups, e := os.Getgroups()
	if e != nil {
		t.Fatal(e)
	}
	for _, g := range groups {
		if g != egid {
			candidates = append(candidates, g)
		}
	}
	for _, gid := range candidates {
		if os.Chown(dir, -1, gid) != nil {
			continue
		}
		if e := os.Chmod(dir, 0o700|os.ModeSetgid); e != nil {
			t.Fatal(e)
		}
		var s unix.Stat_t
		if e := unix.Stat(dir, &s); e != nil {
			t.Fatal(e)
		}
		if int(s.Gid) != gid || uint32(s.Mode)&unix.S_ISGID == 0 {
			t.Fatalf("foreign-group TMPDIR not applied: gid=%d mode=%o want gid=%d setgid", s.Gid, s.Mode, gid)
		}
		return dir + "/", gid, true
	}
	return "", 0, false
}
func TestBackupCredentialStagePolicyRemainsClosed(t *testing.T) {
	v, r, _, w, now := backupStageFixture(t)
	ai := backupCredentialStageIntent(r, w, "fixture", strings.Repeat("b", 64), now)
	cfg, _, e := policy.LoadConfig(v.home)
	if e != nil {
		t.Fatal(e)
	}
	if pd, ok := policy.EvaluateBackupCredentialStage(ai, r, BackupCredentialStageProgramSHA256(), cfg, policy.EvaluationContext{}, now); !ok || pd.Outcome != "hold" {
		t.Fatal("closed owner did not require exact approval")
	}
	cfg.ForbiddenPaths = append(cfg.ForbiddenPaths, r.SourceDirectory+"/**")
	if pd, ok := policy.EvaluateBackupCredentialStage(ai, r, BackupCredentialStageProgramSHA256(), cfg, policy.EvaluationContext{}, now); ok || pd.Outcome != "deny" {
		t.Fatal("custom protected floor was weakened")
	}
	cfg, _, _ = policy.LoadConfig(v.home)
	cfg.ForbiddenPaths = append(cfg.ForbiddenPaths, "**/*secret*")
	if pd, ok := policy.EvaluateBackupCredentialStage(ai, r, BackupCredentialStageProgramSHA256(), cfg, policy.EvaluationContext{}, now); ok || pd.Outcome != "deny" {
		t.Fatal("duplicated built-in floor was weakened")
	}
	cfg, _, _ = policy.LoadConfig(v.home)
	ai.SecurityFloorOutcome = "hold"
	if _, ok := policy.EvaluateBackupCredentialStage(ai, r, BackupCredentialStageProgramSHA256(), cfg, policy.EvaluationContext{}, now); ok {
		t.Fatal("security hold became grant eligible")
	}
}

func TestBackupCredentialStagePinsMetadataBeforeBodyHash(t *testing.T) {
	v, r, _, w, now := backupStageFixture(t)
	for i := range r.Files {
		s := *r.Files[i].Stat
		s.SHA256 = ""
		r.Files[i].Stat = &s
	}
	p, e := v.Plan(r, w, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	backupStageApprove(t, v, p, now)
	out, e := v.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil || !out.CredentialsStaged {
		t.Fatal("metadata-pinned native handoff failed", e)
	}
}
func TestBackupCredentialStageRequiresNativeGuard(t *testing.T) {
	if _, e := (backupCredentialStageNativeTransport{}).Start(context.Background(), policy.BackupCredentialStageRequest{}, nil, "stage", time.Now(), nil); e == nil {
		t.Fatal("transport accepted missing native start guard")
	}
}
func TestBackupCredentialStageDecoderIsClosed(t *testing.T) {
	_, r, _, _, _ := backupStageFixture(t)
	raw, _ := json.Marshal(r)
	if _, e := policy.DecodeBackupCredentialStageRequest(raw); e != nil {
		t.Fatal(e)
	}
	var shape map[string]any
	if e := json.Unmarshal(raw, &shape); e != nil {
		t.Fatal(e)
	}
	shape["credential_value"] = "synthetic-value"
	extra, _ := json.Marshal(shape)
	if _, e := policy.DecodeBackupCredentialStageRequest(extra); e == nil {
		t.Fatal("raw credential field accepted")
	}
	delete(shape, "credential_value")
	delete(shape, "source_directory_stat")
	missing, _ := json.Marshal(shape)
	if _, e := policy.DecodeBackupCredentialStageRequest(missing); e == nil {
		t.Fatal("missing metadata accepted")
	}
	duplicate := append(append([]byte{}, raw[:len(raw)-1]...), []byte(",\"profile_id\":\"fixture-stage\"}")...)
	if _, e := policy.DecodeBackupCredentialStageRequest(duplicate); e == nil {
		t.Fatal("duplicate field accepted")
	}
}

func backupDescriptorFIFOTest(t *testing.T, mode string) {
	t.Helper()
	if os.Getenv("COMPOUND_BACKUP_STAGE_FIFO_CHILD") == "" {
		executable, e := os.Executable()
		if e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run", "^"+t.Name()+"$")
		command.Env = append(os.Environ(), "COMPOUND_BACKUP_STAGE_FIFO_CHILD="+mode)
		if raw, e := command.CombinedOutput(); e != nil || ctx.Err() != nil {
			t.Fatalf("FIFO descriptor refusal was not bounded: %v %s", e, raw)
		}
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "source")
	if e := os.WriteFile(path, []byte("synthetic-regular"), 0600); e != nil {
		t.Fatal(e)
	}
	before, e := os.Lstat(path)
	if e != nil {
		t.Fatal(e)
	}
	expected, e := backupCredentialStageFileStat(before, "")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if e = unix.Mkfifo(path, 0600); e != nil {
		t.Fatal(e)
	}
	// The swap is complete before the real opened-descriptor seam is called.
	// No writer exists; removing O_NONBLOCK deterministically hangs the child.
	if mode == "source" {
		fd, e := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if e != nil {
			t.Fatal(e)
		}
		defer unix.Close(fd)
		raw, e := backupCredentialStageReadSourceAt(fd, policy.BackupCredentialStageFile{Basename: "source", Stat: &expected})
		if e == nil || len(raw) != 0 {
			t.Fatal("FIFO was accepted as credential")
		}
	} else {
		raw, e := runtimeConfigInstallReadTransportFile(path, before)
		if e == nil || len(raw) != 0 {
			t.Fatal("FIFO was accepted as known_hosts")
		}
	}
}
func TestBackupCredentialStageFIFODescriptorRefusal(t *testing.T) {
	backupDescriptorFIFOTest(t, "source")
}
func TestBackupCredentialStageKnownHostsFIFODescriptorRefusal(t *testing.T) {
	backupDescriptorFIFOTest(t, "known_hosts")
}
