package act

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

// This explicit fixture door is test-only. No production request, CLI option or
// transport can select it. It launches the exact embedded programs through the
// same native owner admission callback, with real Linux PG17/libpq and Docker.
type alignmentLinuxFixture struct{ container string }

type alignmentLinuxMetadata struct {
	File          policy.CredentialVerifyFile       `json:"file"`
	Host          policy.CredentialVerifyHost       `json:"host"`
	Interpreter   string                            `json:"interpreter_sha256"`
	Client        string                            `json:"client_sha256"`
	ClientVersion string                            `json:"client_version"`
	Sudo          string                            `json:"sudo_sha256"`
	Docker        string                            `json:"docker_sha256"`
	Root          policy.RuntimeConfigInstallRoot   `json:"root"`
	Marker        policy.RuntimeConfigInstallAnchor `json:"marker"`
	Lock          policy.RuntimeConfigInstallAnchor `json:"lock"`
	OriginalStat  policy.RuntimeConfigInstallStat   `json:"original_stat"`
}

func alignmentLinux(t *testing.T) alignmentLinuxFixture {
	t.Helper()
	name := os.Getenv("COMPOUND_ALIGNMENT_NATIVE_CONTAINER")
	if name == "" {
		t.Skip("requires explicitly selected disposable Linux/PG17 Docker fixture")
	}
	if os.Getenv("COMPOUND_ALIGNMENT_ALLOW_FIXTURE") != "1" {
		t.Fatal("disposable fixture acknowledgement required")
	}
	meta, e := exec.Command("docker", "inspect", "--format", `{{.State.Running}} {{index .Config.Labels "compound.work"}} {{len .HostConfig.PortBindings}}`, name).Output()
	fields := strings.Fields(string(meta))
	if e != nil || len(fields) != 3 || fields[0] != "true" || !strings.HasPrefix(fields[1], "work_") || fields[2] != "0" {
		t.Fatal("fixture identity refused")
	}
	dir := "/root/compound-alignment-tests"
	if e = exec.Command("docker", "exec", name, "mkdir", "-p", dir).Run(); e != nil {
		t.Fatal("fixture directory unavailable")
	}
	for _, file := range []string{"credential_verify_program.py", "credential_password_align_program.py", "credential_password_align_program_test.py", "credential_password_align_native_fixture.py", "runtime_config_install_program.py", "runtime_config_install_program_test.py", "runtime_config_install_alignment_fence_test.py"} {
		path, _ := filepath.Abs(file)
		if e = exec.Command("docker", "cp", path, name+":"+dir+"/"+file).Run(); e != nil {
			t.Fatal("fixture source copy refused")
		}
	}
	return alignmentLinuxFixture{name}
}

func (f alignmentLinuxFixture) start(ctx context.Context, program, sha string, wire any, guard credentialVerifyAdmissionGuard) (credentialPasswordAlignChild, error) {
	payload, e := json.Marshal(wire)
	if e != nil {
		return nil, e
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", "--interactive", f.container, "/usr/bin/python3", "-I", "-B", "-c", program, sha)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	child := &credentialVerifyNativeChild{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = &child.stdout, &child.stderr
	started := false
	e = guard(func() error {
		if e := cmd.Start(); e != nil {
			return e
		}
		started = true
		go func() { child.failed = cmd.Wait() != nil; close(child.done) }()
		return nil
	})
	if e != nil && !started {
		return nil, e
	}
	return child, e
}

type alignmentLinuxVerifyTransport struct{ alignmentLinuxFixture }

func (f alignmentLinuxVerifyTransport) Start(ctx context.Context, r policy.CredentialVerificationRequest, deadline time.Time, guard credentialVerifyAdmissionGuard) (credentialVerifyChild, error) {
	return f.start(ctx, credentialVerificationProgram, CredentialVerificationProgramSHA256(), map[string]any{"request": r, "issued_ns": strconv.FormatInt(time.Now().UnixNano(), 10), "deadline_ns": strconv.FormatInt(deadline.UnixNano(), 10)}, guard)
}

type alignmentLinuxAlignTransport struct {
	alignmentLinuxFixture
	crashCommit   bool
	crashBoundary string
}

type alignmentLinuxInstallerTransport struct {
	alignmentLinuxFixture
	owner *RuntimeConfigInstaller
}

func (f alignmentLinuxInstallerTransport) Start(ctx context.Context, r policy.RuntimeConfigInstallRequest, selected map[string]string, operation string, deadline time.Time, guard runtimeConfigInstallAdmissionGuard) (runtimeConfigInstallChild, error) {
	acceptances, e := f.owner.alignmentAcceptances(r)
	if e != nil {
		return nil, e
	}
	if acceptances == nil {
		acceptances = []credentialPasswordAlignmentAcceptance{}
	}
	return f.start(ctx, runtimeConfigInstallProgram(), RuntimeConfigInstallProgramSHA256(), map[string]any{"request": r, "selected": selected, "operation": operation, "issued_ns": strconv.FormatInt(time.Now().UnixNano(), 10), "deadline_ns": strconv.FormatInt(deadline.UnixNano(), 10), "alignment_acceptances": acceptances}, credentialVerifyAdmissionGuard(guard))
}

func (f alignmentLinuxAlignTransport) Start(ctx context.Context, r policy.CredentialPasswordAlignmentRequest, action string, deadline time.Time, guard credentialPasswordAlignAdmissionGuard) (credentialPasswordAlignChild, error) {
	program := credentialPasswordAlignmentProgram()
	if f.crashCommit {
		// Test-only process death after writing COMMIT but before consuming an
		// acknowledgement. No production program/CLI/request has a crash knob.
		at := strings.LastIndex(program, "if __name__ == \"__main__\":")
		program = program[:at] + "\ndef fixture_commit_without_ack(self):\n    self.child.stdin.write(b'COMMIT;\\n')\n    self.child.stdin.flush()\n    os._exit(99)\nAlignmentSession.commit=fixture_commit_without_ack\nsys.exit(alignment_main())\n"
	} else if f.crashBoundary != "" {
		program = alignmentFixturePrecommitKillProgram(program, action, f.crashBoundary)
	}
	return f.start(ctx, program, CredentialPasswordAlignmentProgramSHA256(), map[string]any{"request": r, "action_intent_id": action, "original_action_intent_id": credentialPasswordAlignOriginalAction(r, action), "issued_ns": strconv.FormatInt(time.Now().UnixNano(), 10), "deadline_ns": strconv.FormatInt(deadline.UnixNano(), 10)}, guard)
}

// This fault transport exists only in the test binary. It kills the real native
// helper and its actual psql process tree, bypassing Python finally/close and
// their explicit ROLLBACK. No production program, request or CLI gains a knob.
func alignmentFixturePrecommitKillProgram(program, action, boundary string) string {
	at := strings.LastIndex(program, "if __name__ == \"__main__\":")
	shim := "\nfixture_boundary=" + strconv.Quote(boundary) + "\nfixture_action=" + strconv.Quote(action) + "\n" + `
from pathlib import Path
fixture_session=None
fixture_before=None
fixture_closes=0
fixture_alters=0
fixture_original_init=AlignmentSession.__init__
fixture_original_exchange=AlignmentSession.exchange
fixture_original_put=AlignmentCustody.put
fixture_original_close=AlignmentSession.close

def fixture_clients(root):
    parents={}
    commands={}
    for path in Path('/proc').iterdir():
        if not path.name.isdigit():
            continue
        try:
            fields=(path/'stat').read_text().rsplit(')',1)[1].split()
            parents[int(path.name)]=int(fields[1])
            commands[int(path.name)]=(path/'comm').read_text().strip()
        except (FileNotFoundError, ProcessLookupError):
            pass
    owned={root}
    while True:
        more={pid for pid,parent in parents.items() if parent in owned}
        if more.issubset(owned):
            break
        owned.update(more)
    require(any(commands.get(pid)=='psql' for pid in owned))
    return sorted(owned)

def fixture_die():
    current=fixture_session.catalog(fixture_session.fixture_request)
    require(fixture_closes==0 and fixture_before is not None)
    changed=sum(a['verifier']!=b['verifier'] for a,b in zip(current['roles'],fixture_before['roles']))
    parent=Path(fixture_session.fixture_request['file']['path']).parent
    custody=parent/ALIGN_NAMESPACE/fixture_action
    clients=fixture_clients(fixture_session.child.pid)
    marker={'stage':fixture_boundary,'changed_roles':changed,'client_pids':clients,
            'helper_pid':os.getpid(),'real_psql_present':True,'explicit_close_called':False,
            'preimage_present':(custody/'PREIMAGE.json').exists(),
            'postimage_present':(custody/'POSTIMAGE.json').exists(),
            'commit_intent_present':(custody/'COMMIT_INTENT.json').exists()}
    fd=os.open(parent/'.fixture-kill-boundary.json',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    try:
        raw=json.dumps(marker,sort_keys=True,separators=(',',':')).encode()
        require(os.write(fd,raw)==len(raw))
        os.fsync(fd)
    finally:
        os.close(fd)
    for pid in reversed(clients):
        try:
            os.kill(pid,signal.SIGKILL)
        except ProcessLookupError:
            pass
    os.kill(os.getpid(),signal.SIGKILL)

def fixture_init(self,r,processes):
    global fixture_session
    self.fixture_request=r
    fixture_original_init(self,r,processes)
    fixture_session=self

def fixture_exchange(self,sql,passwords=()):
    global fixture_alters
    value=fixture_original_exchange(self,sql,passwords)
    if sql.startswith('\\password '):
        fixture_alters+=1
        if ((fixture_boundary=='after_first_alter' and fixture_alters==1) or
            (fixture_boundary=='after_second_alter' and fixture_alters==2)):
            fixture_die()
    return value

def fixture_put(self,name,value):
    global fixture_before
    if name=='PREIMAGE.json' and value.get('schema_version')=='compound.credential_password_preimage.v0.1':
        fixture_before=value['catalog']
        if fixture_boundary=='before_preimage':
            fixture_die()
    if name=='COMMIT_INTENT.json' and fixture_boundary=='after_postimage_before_commit':
        fixture_die()
    result=fixture_original_put(self,name,value)
    if name=='PREIMAGE.json' and fixture_boundary=='after_preimage':
        fixture_die()
    return result

def fixture_close(self):
    global fixture_closes
    fixture_closes+=1
    return fixture_original_close(self)

AlignmentSession.__init__=fixture_init
AlignmentSession.exchange=fixture_exchange
AlignmentCustody.put=fixture_put
AlignmentSession.close=fixture_close
sys.exit(alignment_main())
`
	return program[:at] + shim
}

func nativeAlignmentFixture(t *testing.T, linux alignmentLinuxFixture) *alignmentFixture {
	return nativeAlignmentProfileFixture(t, linux, nil)
}

func nativeAlignmentProfileFixture(t *testing.T, linux alignmentLinuxFixture, configureProfile func(*policy.RuntimeConfigInstallProfile)) *alignmentFixture {
	t.Helper()
	raw, e := exec.Command("docker", "exec", linux.container, "/usr/bin/python3", "-I", "-B", "/root/compound-alignment-tests/credential_password_align_native_fixture.py", "prepare").Output()
	if e != nil {
		t.Fatal("native fixture preparation refused")
	}
	var metadata alignmentLinuxMetadata
	if json.Unmarshal(raw, &metadata) != nil {
		t.Fatal("native fixture metadata refused")
	}
	f := newConfiguredAlignmentProfileFixture(t, func(c *credentialFixture) {
		c.request.File, c.request.Host = metadata.File, metadata.Host
		c.request.SSH.InterpreterSHA256, c.request.SSH.ClientSHA256, c.request.SSH.ClientVersion, c.request.SSH.SudoSHA256 = metadata.Interpreter, metadata.Client, metadata.ClientVersion, metadata.Sudo
		c.verifier.transport = alignmentLinuxVerifyTransport{linux}
	}, configureProfile)
	f.r.Catalog.DockerSHA256 = metadata.Docker
	f.linuxMetadata = &metadata
	f.owner.transport = alignmentLinuxAlignTransport{alignmentLinuxFixture: linux}
	return f
}

func TestCredentialPasswordAlignmentActualLinuxOwnerThenUnchangedVerifier(t *testing.T) {
	linux := alignmentLinux(t)
	f := nativeAlignmentFixture(t, linux)
	testCredentialAlignmentThenUnchangedVerifier(t, linux, f)
}

func commercialAlignmentFixture(t *testing.T, linux alignmentLinuxFixture) *alignmentFixture {
	t.Helper()
	prefix := "fixture-seven-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	// Select the target before its first genuine capture qualification. The
	// unchanged owner freezes it; no caller-written replacement is admitted.
	return nativeAlignmentProfileFixture(t, linux, func(profile *policy.RuntimeConfigInstallProfile) {
		profile.WebAPIBase = "http://" + prefix + "-api:9000"
		profile.Containers = nil
		for _, suffix := range []string{"-auth", "-api", "-worker", "-billing", "-billing-reconcile", "-provider-sync", "-web"} {
			profile.Containers = append(profile.Containers, prefix+suffix)
		}
	})
}

func TestCredentialPasswordAlignmentActualCommercialSevenThenUnchangedVerifier(t *testing.T) {
	linux := alignmentLinux(t)
	testCredentialAlignmentThenUnchangedVerifier(t, linux, commercialAlignmentFixture(t, linux))
}

func TestCredentialPasswordAlignmentActualCommercialSevenStoppedContainers(t *testing.T) {
	linux := alignmentLinux(t)
	for _, suffix := range []string{"auth", "cloudflared"} {
		t.Run(suffix, func(t *testing.T) {
			f := commercialAlignmentFixture(t, linux)
			api := strings.TrimPrefix(strings.TrimSuffix(f.r.Profile.WebAPIBase, ":9000"), "http://")
			name := strings.TrimSuffix(api, "-api") + "-" + suffix
			image, err := exec.Command("docker", "inspect", "--format", "{{.Image}}", linux.container).Output()
			if err != nil {
				t.Fatal("owned fixture image unavailable")
			}
			raw, err := exec.Command("docker", "create", "--pull", "never", "--network", "none", "--name", name, "--label", "compound.work="+f.workID, strings.TrimSpace(string(image)), "/bin/true").Output()
			if err != nil {
				t.Fatal("stopped owned fixture could not be created")
			}
			id := strings.TrimSpace(string(raw))
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
				t.Fatal("stopped fixture identity refused")
			}
			expected := "false /" + name + " " + f.workID
			check := func() bool {
				meta, err := exec.Command("docker", "inspect", "--format", `{{.State.Running}} {{.Name}} {{index .Config.Labels "compound.work"}}`, id).Output()
				return err == nil && strings.TrimSpace(string(meta)) == expected
			}
			t.Cleanup(func() {
				if !check() {
					t.Error("stopped fixture ownership changed; cleanup refused")
					return
				}
				if err := exec.Command("docker", "rm", "--volumes", id).Run(); err != nil {
					t.Error("stopped owned fixture cleanup failed")
				}
			})
			if !check() {
				t.Fatal("fixture was not stopped with exact ownership")
			}
			p, err := f.owner.Plan(f.r, f.workID, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			f.approve(t, p)
			result, err := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
			if err == nil || result.State == "completed" || !result.ApprovalConsumed || result.ConfigurationAccepted {
				t.Fatal("stopped service did not refuse genuine native inspection")
			}
			a, err := runattempt.Open(f.owner.home).Get(result.RunAttemptID)
			if err != nil || a.Status != runattempt.StatusFailed {
				t.Fatal("stopped-service refusal lost failed native attempt")
			}
		})
	}
}

func testCredentialAlignmentThenUnchangedVerifier(t *testing.T, linux alignmentLinuxFixture, f *alignmentFixture) {
	t.Helper()
	f.inspect(t)
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil || result.State != "completed" || result.ConfigurationAccepted {
		t.Fatal("actual native alignment failed", e)
	}
	if _, e = credentialPasswordAlignmentInstallerFence(f.owner.home, f.r.File.Path, policy.CredentialVerificationDestination(f.r.CredentialVerificationRequest), f.now.Add(-time.Second)); e == nil {
		t.Fatal("old verifier cleared actual alignment")
	}
	c := f.installer.credential
	c.now = time.Now().UTC()
	c.workID = alignmentTestWork(t, c.home, c.now)
	c.request.AttemptID = c.now.Format("20060102t150405z") + "-1111111111111111"
	qualifiedPlan := c.plan(t)
	c.approve(t, qualifiedPlan)
	qualified, e := c.verifier.Execute(context.Background(), qualifiedPlan.PlanRef, "fixture")
	if e != nil || qualified.State != "completed" || qualified.ConfigurationAccepted {
		t.Fatal("unchanged verifier did not genuinely authenticate repaired credentials", e)
	}
	var receipt envelope.Receipt
	if f.owner.event(qualified.ReceiptID, &receipt) != nil {
		t.Fatal("missing actual verifier receipt")
	}
	created, _ := time.Parse(time.RFC3339Nano, receipt.CreatedAt)
	accepted, e := credentialPasswordAlignmentInstallerFence(c.home, f.r.File.Path, policy.CredentialVerificationDestination(c.request), created)
	if e != nil || len(accepted) != 1 || accepted[0].ActionID != p.Plan.ActionIntentID {
		t.Fatal("genuine unchanged verification failed to consume completed repair proof", e)
	}
	// Consume the same real repair/qualification proof through the actual native
	// four-file installer, with the source capture fixture owned by this test.
	i := f.installer
	convert := func(from, to any) {
		raw, _ := json.Marshal(from)
		if e := json.Unmarshal(raw, to); e != nil {
			t.Fatal(e)
		}
	}
	convert(c.request.Resource, &i.request.Resource)
	convert(c.request.SSH, &i.request.SSH)
	convert(c.request.Host, &i.request.Host)
	convert(c.request.Roles, &i.request.Roles)
	i.request.Root, i.request.Marker, i.request.Lock = f.linuxMetadata.Root, f.linuxMetadata.Marker, f.linuxMetadata.Lock
	for index := range i.request.Files {
		i.request.Files[index].Path = filepath.Join(i.request.Root.Path, i.request.Files[index].Basename)
	}
	i.request.Files[0].Stat = &f.linuxMetadata.OriginalStat
	i.request.Witness = policy.RuntimeConfigInstallWitness{Ref: qualified.ReceiptID, SHA256: installerProof(t, c.home, qualified.ReceiptID, false).SHA256, ObservedAt: receipt.CreatedAt}
	a, e := runattempt.Open(c.home).Get(qualified.RunAttemptID)
	if e != nil {
		t.Fatal(e)
	}
	i.request.Qualification = policy.RuntimeConfigInstallQualification{Plan: installerProof(t, c.home, qualifiedPlan.PlanRef, true), Receipt: installerProof(t, c.home, qualified.ReceiptID, false), Transition: installerProof(t, c.home, qualified.TransitionID, false), Attempt: policy.RuntimeConfigInstallProofRef{Ref: a.ID, SHA256: runtimeConfigInstallHash(a)}, Observation: installerProof(t, c.home, qualified.ObservationRef, true)}
	c.now = time.Now().UTC()
	i.request.AttemptID = c.now.Format("20060102t150405z") + "-9999999999999999"
	i.owner.transport = alignmentLinuxInstallerTransport{alignmentLinuxFixture: linux, owner: i.owner}
	installation := i.plan(t)
	i.approve(t, installation)
	installed, e := i.owner.Execute(context.Background(), installation.PlanRef, "fixture")
	if e != nil || !installed.ConfigurationAccepted || installed.ApplicationActivated {
		t.Fatal("real installer did not consume repaired credential proof", e)
	}
	logs, e := exec.Command("docker", "logs", linux.container).CombinedOutput()
	if e != nil || bytes.Contains(logs, []byte("synthetic-canary-")) || regexp.MustCompile(`SCRAM-SHA-256\$[1-9][0-9]*:[A-Za-z0-9+/]+=*\$[A-Za-z0-9+/]+=*:[A-Za-z0-9+/]+=*`).Match(logs) {
		t.Fatal("fixture logs contain credential material or are unavailable")
	}
}

func TestCredentialPasswordAlignmentProgramActualLinuxPrimitiveConsumer(t *testing.T) {
	linux := alignmentLinux(t)
	for _, file := range []string{"credential_password_align_program_test.py", "runtime_config_install_alignment_fence_test.py"} {
		cmd := exec.Command("docker", "exec", "--env", "COMPOUND_ALIGNMENT_LINUX_PG17=1", linux.container, "/usr/bin/python3", "-I", "-B", "/root/compound-alignment-tests/"+file)
		if raw, e := cmd.CombinedOutput(); e != nil || bytes.Contains(raw, []byte("skipped=")) {
			t.Fatal("actual Linux acceptance suite failed or skipped", string(raw))
		}
	}
}

func TestCredentialPasswordAlignmentPurePythonConsumers(t *testing.T) {
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Skip("Python 3 unavailable")
	}
	for _, file := range []string{"credential_password_align_program_test.py", "runtime_config_install_alignment_fence_test.py"} {
		t.Run(file, func(t *testing.T) {
			cmd := exec.Command(python, "-I", "-B", file)
			if raw, e := cmd.CombinedOutput(); e != nil {
				t.Fatal("synthetic Python acceptance failed", string(raw))
			}
		})
	}
}

func TestCredentialPasswordAlignmentActualCommitAckLossAndRestoreAckLoss(t *testing.T) {
	linux := alignmentLinux(t)
	f := nativeAlignmentFixture(t, linux)
	f.inspect(t)
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	f.owner.transport = alignmentLinuxAlignTransport{alignmentLinuxFixture: linux, crashCommit: true}
	original, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || original.State != "recovery_required" {
		t.Fatal("commit acknowledgement loss was accepted", e)
	}
	f.owner.transport = alignmentLinuxAlignTransport{alignmentLinuxFixture: linux}
	f.freshRecovery(t, p, original, "reconcile")
	reconcile, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, reconcile)
	resolved, e := f.owner.Execute(context.Background(), reconcile.PlanRef, "fixture")
	if e != nil || resolved.State != "completed" {
		t.Fatal("real committed postimage did not reconcile", e)
	}
	store, _ := NewArtifactStore(f.owner.home)
	raw, _ := store.Get(resolved.ObservationRef)
	o, e := decodeCredentialPasswordAlignmentObservation(raw, reconcile.Plan.Request)
	if e != nil || o.Effect != "reconciled_new_new" {
		t.Fatal("lost COMMIT acknowledgement did not preserve new/new classification", e)
	}
	f.freshRecovery(t, p, original, "restore")
	restore, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, restore)
	f.owner.transport = alignmentLinuxAlignTransport{alignmentLinuxFixture: linux, crashCommit: true}
	failedRestore, e := f.owner.Execute(context.Background(), restore.PlanRef, "fixture")
	if e == nil || failedRestore.State != "recovery_required" {
		t.Fatal("restore acknowledgement loss was accepted", e)
	}
	f.owner.transport = alignmentLinuxAlignTransport{alignmentLinuxFixture: linux}
	f.freshRecovery(t, p, original, "restore")
	if _, e = f.owner.Plan(f.r, f.workID, "fixture"); e == nil {
		t.Fatal("blind restore retry reached admission")
	}
	f.r.Operation = "reconcile"
	recovery, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, recovery)
	completed, e := f.owner.Execute(context.Background(), recovery.PlanRef, "fixture")
	if e != nil {
		t.Fatal("journaled restore postimage with new row revisions was not reconciled", e)
	}
	raw, _ = store.Get(completed.ObservationRef)
	o, e = decodeCredentialPasswordAlignmentObservation(raw, recovery.Plan.Request)
	if e != nil || o.Effect != "reconciled_old_old" || len(o.ResolvedRecoveryActions) != 2 {
		t.Fatal("restored catalog was confused with original old row revisions", e)
	}
	for _, id := range []string{original.RunAttemptID, failedRestore.RunAttemptID} {
		a, e := runattempt.Open(f.owner.home).Get(id)
		if e != nil || a.Status != runattempt.StatusFailed {
			t.Fatal("failed acknowledgement-loss history was rewritten")
		}
	}
}

func TestCredentialPasswordAlignmentActualTamperedOldVerifierSwapHolds(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run("rewrite_request_"+strconv.FormatBool(rewrite), func(t *testing.T) {
			credentialPasswordAlignmentTamperedOldVerifierSwapHolds(t, rewrite)
		})
	}
}

func credentialPasswordAlignmentTamperedOldVerifierSwapHolds(t *testing.T, rewrite bool) {
	t.Helper()
	linux := alignmentLinux(t)
	f := nativeAlignmentFixture(t, linux)
	f.inspect(t)
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil || result.State != "completed" {
		t.Fatal("legitimate alignment failed", e)
	}
	script := `import hashlib,json,sys
from pathlib import Path
p=Path(sys.argv[1]).parent/'.compound-credential-password-alignment'/sys.argv[2]/'PREIMAGE.json'
v=json.loads(p.read_bytes())
r=v['catalog']['roles']
r[0]['verifier'],r[1]['verifier']=r[1]['verifier'],r[0]['verifier']
if sys.argv[3]=='true':
    digest=lambda value:hashlib.sha256(json.dumps(value,sort_keys=True,separators=(',',':')).encode()).hexdigest()
    v['request']['catalog']['sha256']=digest(v['catalog'])
    v['request_sha256']=digest(v['request'])
p.write_text(json.dumps(v))
`
	if e := exec.Command("docker", "exec", linux.container, "/usr/bin/python3", "-I", "-B", "-c", script, f.r.File.Path, p.Plan.ActionIntentID, strconv.FormatBool(rewrite)).Run(); e != nil {
		t.Fatal("synthetic custody swap failed")
	}
	f.freshRecovery(t, p, result, "restore")
	before := f.r.Catalog
	repair, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal("separately approved restore plan did not reach native custody checks", e)
	}
	f.approve(t, repair)
	restored, e := f.owner.Execute(context.Background(), repair.PlanRef, "fixture")
	if e == nil || restored.State != "recovery_required" {
		t.Fatal("valid SCRAM verifier swap in original PREIMAGE was accepted and restored")
	}
	f.freshRecovery(t, p, result, "reconcile")
	if f.r.Catalog != before {
		t.Fatal("rejected tampered custody changed the two live role revisions or verifiers")
	}
}

func TestCredentialPasswordAlignmentActualPrecommitSIGKILLMatrix(t *testing.T) {
	for _, boundary := range []struct {
		name    string
		changed int
	}{
		{"before_preimage", 0},
		{"after_preimage", 0},
		{"after_first_alter", 1},
		{"after_second_alter", 2},
		{"after_postimage_before_commit", 2},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			linux := alignmentLinux(t)
			f := nativeAlignmentFixture(t, linux)
			f.inspect(t)
			before := f.r.Catalog
			plan, e := f.owner.Plan(f.r, f.workID, "fixture")
			if e != nil {
				t.Fatal(e)
			}
			f.approve(t, plan)
			f.owner.transport = alignmentLinuxAlignTransport{alignmentLinuxFixture: linux, crashBoundary: boundary.name}
			failed, e := f.owner.Execute(context.Background(), plan.PlanRef, "fixture")
			if e == nil || failed.State != "recovery_required" || failed.ConfigurationAccepted || failed.ApplicationAccessAccepted || failed.SourceCaptureAccepted || failed.MigrationAccepted || failed.ApplicationActivated || failed.ServiceActivated {
				t.Fatal("abrupt native client/helper death was accepted or lost uncertainty", e)
			}
			markerScript := `import json,sys
from pathlib import Path
v=json.loads((Path(sys.argv[1]).parent/'.fixture-kill-boundary.json').read_bytes())
def dead(pid):
    try:
        return Path('/proc',str(pid),'stat').read_text().rsplit(')',1)[1].split()[0] in ('Z','X')
    except FileNotFoundError:
        return True
v['client_dead']=bool(v['client_pids']) and all(dead(pid) for pid in v['client_pids'])
v['helper_dead']=dead(v['helper_pid'])
print(json.dumps(v,sort_keys=True,separators=(',',':')))
`
			raw, e := exec.Command("docker", "exec", linux.container, "/usr/bin/python3", "-I", "-B", "-c", markerScript, f.r.File.Path).Output()
			var marker struct {
				Stage         string `json:"stage"`
				Changed       int    `json:"changed_roles"`
				ClientDead    bool   `json:"client_dead"`
				HelperDead    bool   `json:"helper_dead"`
				RealPsql      bool   `json:"real_psql_present"`
				ExplicitClose bool   `json:"explicit_close_called"`
				Preimage      bool   `json:"preimage_present"`
				Postimage     bool   `json:"postimage_present"`
				CommitIntent  bool   `json:"commit_intent_present"`
			}
			if e != nil || json.Unmarshal(raw, &marker) != nil || marker.Stage != boundary.name || marker.Changed != boundary.changed || !marker.ClientDead || !marker.HelperDead || !marker.RealPsql || marker.ExplicitClose || marker.Preimage != (boundary.name != "before_preimage") || marker.Postimage != (boundary.name == "after_postimage_before_commit") || marker.CommitIntent {
				t.Fatal("actual SIGKILL boundary or custody evidence missing/different", boundary.name, e)
			}
			t.Logf("SIGKILL boundary=%s in_transaction_changed_roles=%d clients_dead=%t helper_dead=%t preimage=%t postimage=%t commit_intent=%t", marker.Stage, marker.Changed, marker.ClientDead, marker.HelperDead, marker.Preimage, marker.Postimage, marker.CommitIntent)
			if _, e = credentialPasswordAlignmentInstallerFence(f.owner.home, f.r.File.Path, policy.CredentialVerificationDestination(f.r.CredentialVerificationRequest), time.Now().Add(time.Second)); e == nil {
				t.Fatal("abrupt uncertain attempt did not hold installer")
			}
			f.owner.transport = alignmentLinuxAlignTransport{alignmentLinuxFixture: linux}
			f.freshRecovery(t, plan, failed, "reconcile")
			if f.r.Catalog != before {
				t.Fatal("uncommitted real password changes did not roll back on abrupt client death")
			}
			retry := f.r
			retry.Operation, retry.Origin = "align", nil
			retry.AttemptID = time.Now().UTC().Format("20060102t150405z") + "-" + credentialPasswordAlignHash(time.Now())[:16]
			if _, e = f.owner.Plan(retry, alignmentTestWork(t, f.owner.home, time.Now()), "fixture"); e == nil {
				t.Fatal("abrupt failed attempt admitted blind alignment retry")
			}
			recovery, e := f.owner.Plan(f.r, f.workID, "fixture")
			if e != nil {
				t.Fatal("separate original-failure recovery plan refused", e)
			}
			f.approve(t, recovery)
			resolved, e := f.owner.Execute(context.Background(), recovery.PlanRef, "fixture")
			if boundary.name == "before_preimage" {
				if e == nil || resolved.State != "recovery_required" {
					t.Fatal("missing original PREIMAGE did not conservatively hold recovery")
				}
			} else {
				if e != nil || resolved.State != "completed" {
					t.Fatal("durable original preimage did not reconcile exact old/old after SIGKILL", e)
				}
				artifacts, _ := NewArtifactStore(f.owner.home)
				raw, e := artifacts.Get(resolved.ObservationRef)
				if e != nil {
					t.Fatal(e)
				}
				observation, e := decodeCredentialPasswordAlignmentObservation(raw, recovery.Plan.Request)
				if e != nil || observation.Effect != "reconciled_old_old" || resolved.ConfigurationAccepted || resolved.ApplicationAccessAccepted || resolved.SourceCaptureAccepted || resolved.MigrationAccepted {
					t.Fatal("actual old/old reconciliation or acceptance boundary differs", e)
				}
			}
			attempt, e := runattempt.Open(f.owner.home).Get(failed.RunAttemptID)
			if e != nil || attempt.Status != runattempt.StatusFailed {
				t.Fatal("original SIGKILL attempt history was rewritten")
			}
		})
	}
}
