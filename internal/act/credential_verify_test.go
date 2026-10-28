package act

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

type credentialFixture struct {
	home      string
	now       time.Time
	request   policy.CredentialVerificationRequest
	workID    string
	verifier  *CredentialVerifier
	transport *credentialMockTransport
}
type credentialMockTransport struct {
	starts    int
	child     *credentialMockChild
	preflight func()
	atStart   func() error
	deadline  time.Time
	ctx       context.Context
	startErr  error
}
type credentialMockChild struct {
	native          *credentialVerifyNativeChild
	raw             []byte
	waitErr         error
	stopErr         error
	stopped         bool
	beforeWait      func()
	cleanupDeadline time.Time
}

func (m *credentialMockTransport) Start(ctx context.Context, r policy.CredentialVerificationRequest, deadline time.Time, guard credentialVerifyAdmissionGuard) (credentialVerifyChild, error) {
	m.starts++
	m.deadline = deadline
	m.ctx = ctx
	if m.preflight != nil {
		m.preflight()
	}
	started := false
	err := guard(func() error {
		if m.startErr != nil {
			return m.startErr
		}
		if m.atStart != nil {
			if err := m.atStart(); err != nil {
				return err
			}
		}
		started = true
		return nil
	})
	if err != nil {
		if started {
			return m.child, err
		}
		return nil, err
	}
	return m.child, nil
}

func (c *credentialMockChild) Wait() ([]byte, error) {
	if c.beforeWait != nil {
		c.beforeWait()
	}
	if c.native != nil {
		return c.native.Wait()
	}
	return c.raw, c.waitErr
}
func (c *credentialMockChild) Stop(ctx context.Context) error {
	c.stopped = true
	c.cleanupDeadline, _ = ctx.Deadline()
	if c.native != nil && c.stopErr == nil {
		return c.native.Stop(ctx)
	}
	return c.stopErr
}

func TestCredentialVerificationFailureStageIsMetadataOnlyAfterCleanup(t *testing.T) {
	for _, kind := range []string{"stage", "cleanup-failure", "remote-cleanup", "unknown-stage", "split-marker", "raw-stderr", "duplicate-marker", "oversize-marker", "contradictory-success"} {
		t.Run(kind, func(t *testing.T) {
			f := newCredentialFixture(t)
			const secret = "FIXTURE_FAILED_OUTPUT_SECRET_NEVER_PERSIST"
			stage, want := "source_format", "source_format"
			switch kind {
			case "cleanup-failure":
				f.transport.child.stopErr = errors.New(secret)
				want = ""
			case "remote-cleanup":
				stage, want = "cleanup", ""
			case "unknown-stage":
				stage, want = secret, ""
			case "split-marker", "raw-stderr", "duplicate-marker", "oversize-marker", "contradictory-success":
				done := make(chan struct{})
				close(done)
				native := &credentialVerifyNativeChild{done: done, failed: kind != "contradictory-success"}
				native.stdout.data = append([]byte(nil), f.transport.child.raw...)
				marker := "credential_verify:refused:source_format\n"
				chunks := []string{marker}
				want = ""
				switch kind {
				case "split-marker":
					chunks, want = []string{"credential_verify:refused:source_", "format\n"}, "source_format"
				case "raw-stderr":
					chunks = []string{secret}
				case "duplicate-marker":
					chunks = []string{marker, marker}
				case "oversize-marker":
					chunks = []string{strings.Repeat(secret, 10000)}
				}
				for _, chunk := range chunks {
					_, _ = native.stderr.Write([]byte(chunk))
				}
				f.transport.child.native = native
			}
			f.transport.child.raw = []byte(secret)
			f.transport.child.waitErr = credentialVerifyStageError{stage: stage}
			p := f.plan(t)
			f.approve(t, p)
			result, err := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
			if err == nil || result.State != "failed" || !result.ApprovalConsumed || result.ObservationRef != "" || result.ReceiptID == "" || result.TransitionID == "" || !f.transport.child.stopped || result.ConfigurationAccepted || result.ApplicationAccessAccepted || result.SourceCaptureAccepted || result.MigrationAccepted {
				t.Fatalf("failure accepted: %+v %v", result, err)
			}
			attempt, err := runattempt.Open(f.home).Get(result.RunAttemptID)
			if err != nil || attempt.Status != runattempt.StatusFailed {
				t.Fatalf("failure not terminal: %+v %v", attempt, err)
			}
			statuses := 0
			err = filepath.WalkDir(f.home, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if strings.Contains(string(raw), secret) {
					t.Fatal("failed output persisted")
				}
				var status map[string]any
				if json.Unmarshal(raw, &status) == nil && status["schema_version"] == "compound.credential_verification_status.v0.1" {
					statuses++
					actual, present := status["failure_stage"]
					if (want == "" && present) || (want != "" && actual != want) {
						t.Fatalf("status stage = %v, want %q", actual, want)
					}
					if status["state"] != "failed" || status["observation_ref"] != nil {
						t.Fatal("failed status accepted observation")
					}
					for _, field := range []string{"configuration_accepted", "application_access_accepted", "source_capture_accepted", "migration_accepted"} {
						if status[field] != false {
							t.Fatalf("failed status accepted %s", field)
						}
					}
				}
				return nil
			})
			if err != nil || statuses == 0 {
				t.Fatalf("missing failed-only status: %d %v", statuses, err)
			}
			if _, err := f.verifier.Execute(context.Background(), p.PlanRef, "fixture"); err == nil || f.transport.starts != 1 {
				t.Fatal("failed attempt was retried")
			}
		})
	}
}

func newCredentialFixture(t *testing.T) *credentialFixture {
	t.Helper()
	h := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	s, e := work.Open(h)
	if e != nil {
		t.Fatal(e)
	}
	w, e := s.Create(work.CreateInput{Owner: "fixture", SuccessPredicate: "closed credential protocol produces only bounded evidence"}, now.Add(-time.Minute))
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
	v, e := NewCredentialVerifier(h)
	if e != nil {
		t.Fatal(e)
	}
	fixture := &credentialFixture{home: h, now: now, workID: w.ID, verifier: v}
	v.now = func() time.Time { return fixture.now }
	digest := strings.Repeat("a", 64)
	r := policy.CredentialVerificationRequest{SchemaVersion: policy.CredentialVerifySchema, ProfileID: "fixture.invalid", AttemptID: now.Format("20060102t150405z") + "-aaaaaaaaaaaaaaaa", ProgramSHA256: CredentialVerificationProgramSHA256(), EffectSet: policy.CredentialVerifyEffects,
		Witness:  policy.CredentialVerifyWitness{Ref: "rec_fixture_metadata", ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)},
		Resource: policy.CredentialVerifyResource{Account: "000000000000", Region: "fixture-region", Instance: "fixture:instance:example", Address: "10.0.0.10"},
		SSH:      policy.CredentialVerifySSH{Alias: "target.fixture.invalid", User: "fixture", Port: 22, IdentityFile: "/fixture/identity", KnownHostsFile: "/fixture/known_hosts", KnownHostsSHA256: digest, HostKeySHA256: "SHA256:" + strings.Repeat("A", 43), InterpreterSHA256: digest, ClientPath: "/usr/lib/postgresql/17/bin/psql", ClientSHA256: digest, ClientVersion: "psql (PostgreSQL) 17.11", SudoSHA256: digest},
		Host:     policy.CredentialVerifyHost{Hostname: "host.fixture.invalid", MachineID: strings.Repeat("a", 32), BootID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", SystemIdentifier: "1234567890123456789", VersionNum: "170011"},
		File:     policy.CredentialVerifyFile{Path: "/srv/fixture/.env", Device: 1, Inode: 2, Bytes: 300, MtimeNS: "1792197133436814154", CtimeNS: "1792197133436814154", SHA256: digest},
		Roles:    [2]policy.CredentialVerifyRole{{Kind: "application", Key: "APP_URL", Name: "fixture_app", SourceHost: "source.fixture.invalid", SourceDatabase: "fixture_db"}, {Kind: "migrator", Key: "MIGRATE_URL", Name: "fixture_migrator", SourceHost: "source.fixture.invalid", SourceDatabase: "fixture_db"}}, Database: policy.CredentialVerifyDatabase{Name: "fixture_db", AllowConnections: true}}
	events, e := eventstore.Open(h)
	if e != nil {
		t.Fatal(e)
	}
	witness := map[string]any{"schema_version": envelope.SVReceipt, "id": r.Witness.Ref, "created_at": r.Witness.ObservedAt, "summary": "fixture metadata only; no credential payload"}
	if _, e = events.PutJSON(witness["id"].(string), witness, ""); e != nil {
		t.Fatal(e)
	}
	record, e := events.Read(r.Witness.Ref)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(record.Bytes)
	r.Witness.SHA256 = hex.EncodeToString(sum[:])
	fixture.request = r
	child := &credentialMockChild{raw: credentialObservationJSON(t, r)}
	fixture.transport = &credentialMockTransport{child: child}
	v.transport = fixture.transport
	return fixture
}

func credentialObservationJSON(t *testing.T, r policy.CredentialVerificationRequest) []byte {
	t.Helper()
	c := credentialVerifyCatalog{SystemIdentifier: r.Host.SystemIdentifier, VersionNum: r.Host.VersionNum, Port: 5432, Privileged: true, Sessions: []credentialVerifySession{}, Database: credentialVerifyCatalogDatabase{Name: r.Database.Name, Owner: r.Roles[1].Name, ConnectionLimit: 0, AllowConnections: r.Database.AllowConnections}}
	for _, role := range r.Roles {
		c.Roles = append(c.Roles, credentialVerifyCatalogRole{Name: role.Name, Login: true, SCRAMPresent: true, Inherit: true, ConnectionLimit: -1})
	}
	o := CredentialVerificationObservation{SchemaVersion: credentialVerifyObservationSchema, RequestSHA256: policy.CredentialVerificationDigest(r), ProgramSHA256: CredentialVerificationProgramSHA256(), HostBefore: r.Host, HostAfter: r.Host, FileMatched: true, Before: c, After: c}
	for index, role := range r.Roles {
		pid := 8001 + index
		identity := &credentialVerifyIdentity{User: role.Name, SessionUser: role.Name, Database: "postgres", Address: "127.0.0.1", Port: 5432, VersionNum: r.Host.VersionNum, PID: pid}
		o.Authentication = append(o.Authentication, credentialVerifyAuth{Kind: role.Kind + "_existing", Role: role.Name, Outcome: "authenticated", Identity: identity}, credentialVerifyAuth{Kind: role.Kind + "_wrong", Role: role.Name, Outcome: "password_rejected"})
		o.After.Sessions = append(o.After.Sessions, credentialVerifySession{PID: pid, User: role.Name, Database: "postgres", Address: "127.0.0.1"})
	}
	raw, e := json.Marshal(o)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}

func (f *credentialFixture) plan(t *testing.T) CredentialVerificationPlanResult {
	t.Helper()
	p, e := f.verifier.Plan(f.request, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func (f *credentialFixture) approve(t *testing.T, p CredentialVerificationPlanResult) {
	t.Helper()
	cfg, _, e := policy.LoadConfig(f.home)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = policy.NewExactActionApprovalStore(f.home).Approve(p.Plan.ApprovalPlanID, "operator:fixture", cfg, f.now); e != nil {
		t.Fatal(e)
	}
}

func TestCredentialVerificationNativeOwnerPlansHoldAndCompletesOnlyEvidence(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	if p.PolicyOutcome != "hold" || p.Plan.ApprovalPlanID == "" || p.Plan.Request.File.Path != "/srv/fixture/.env" {
		t.Fatal("plan did not preserve protected target and exact hold")
	}
	f.approve(t, p)
	result, e := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if result.State != "completed" || !result.ApprovalConsumed || result.ReceiptID == "" || result.TransitionID == "" || result.ObservationRef == "" {
		t.Fatalf("missing native evidence: %+v", result)
	}
	if result.ConfigurationAccepted || result.ApplicationAccessAccepted || result.SourceCaptureAccepted || result.MigrationAccepted {
		t.Fatal("evidence probe accepted migration authority")
	}
	if f.transport.starts != 1 || !f.transport.child.stopped {
		t.Fatal("wrong transport/cleanup count")
	}
	if !f.transport.deadline.Equal(f.now.Add(50*time.Second)) || !f.transport.child.cleanupDeadline.Equal(f.now.Add(60*time.Second)) {
		t.Fatal("work/cleanup reserve changed")
	}
	ra, e := runattempt.Open(f.home).Get(result.RunAttemptID)
	if e != nil || ra.Status != runattempt.StatusSucceeded {
		t.Fatalf("attempt not terminal: %+v %v", ra, e)
	}
	events, _ := eventstore.Open(f.home)
	rec, e := events.Read(result.ReceiptID)
	if e != nil {
		t.Fatal(e)
	}
	var receipt envelope.Receipt
	if json.Unmarshal(rec.Bytes, &receipt) != nil || receipt.Validate() != nil || receipt.StateDelta.Changed || receipt.MoveType != "audit_only" {
		t.Fatal("invalid/non-evidence receipt")
	}
	transition, e := events.Read(result.TransitionID)
	if e != nil {
		t.Fatal(e)
	}
	var tr envelope.Transition
	if json.Unmarshal(transition.Bytes, &tr) != nil || tr.Validate() != nil || tr.SecondOrder.FutureBehaviorChanged {
		t.Fatal("invalid transition")
	}
	if _, e = f.verifier.Execute(context.Background(), p.PlanRef, "fixture"); e == nil || f.transport.starts != 1 {
		t.Fatal("replay reached transport")
	}
}

func TestCredentialVerificationNativeOwnerMissingExpiredApprovalAndWrongActorNeverLaunch(t *testing.T) {
	for _, which := range []string{"missing", "expired", "actor", "witness", "work", "policy"} {
		t.Run(which, func(t *testing.T) {
			f := newCredentialFixture(t)
			p := f.plan(t)
			if which != "missing" {
				f.approve(t, p)
			}
			actor := "fixture"
			switch which {
			case "expired":
				f.now = f.now.Add(16 * time.Minute)
			case "actor":
				actor = "other"
			case "witness":
				events, _ := eventstore.Open(f.home)
				if e := events.Remove(f.request.Witness.Ref); e != nil {
					t.Fatal(e)
				}
			case "work":
				s, _ := work.Open(f.home)
				if _, e := s.Advance(f.workID, work.AdvanceInput{To: "running", Actor: "fixture"}, f.now.Add(time.Second)); e != nil {
					t.Fatal(e)
				}
			case "policy":
				if e := os.MkdirAll(filepath.Join(f.home, "config"), 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(f.home, "config", "policy.yaml"), []byte("forbidden_paths:\n  - '**/.env'\n  - '**/fixture/**'\n"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			result, e := f.verifier.Execute(context.Background(), p.PlanRef, actor)
			if e == nil || result.ApprovalConsumed || f.transport.starts != 0 {
				t.Fatalf("%s reached launch/consumption: %+v %v", which, result, e)
			}
		})
	}
}

func TestCredentialVerificationNativeOwnerPreflightDriftConsumesApprovalButNeverStartsChild(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	f.transport.preflight = func() { f.now = f.now.Add(16 * time.Minute) }
	result, e := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || !result.ApprovalConsumed || result.State != "failed" || f.transport.child.stopped {
		t.Fatalf("drift did not fail closed: %+v %v", result, e)
	}
	ra, err := runattempt.Open(f.home).Get(result.RunAttemptID)
	if err != nil || ra.Status != runattempt.StatusFailed {
		t.Fatalf("failed attempt not terminal: %+v %v", ra, err)
	}
}

func TestCredentialVerificationNativeOwnerSanitizesPostLaunchFailuresAndNeverRetries(t *testing.T) {
	for _, which := range []string{"raw_error", "secret_stdout", "unknown_field", "negative_missing", "catalog_fence", "cleanup_error", "deadline"} {
		t.Run(which, func(t *testing.T) {
			f := newCredentialFixture(t)
			p := f.plan(t)
			f.approve(t, p)
			sentinel := "FIXTURE_SECRET_MUST_NOT_REACH_NATIVE_EVIDENCE"
			switch which {
			case "raw_error":
				f.transport.child.waitErr = errors.New(sentinel)
			case "secret_stdout":
				f.transport.child.raw = []byte(sentinel)
			case "unknown_field":
				f.transport.child.raw = append([]byte(`{"password":"`+sentinel+`",`), f.transport.child.raw[1:]...)
			case "negative_missing":
				var o CredentialVerificationObservation
				json.Unmarshal(f.transport.child.raw, &o)
				o.Authentication = o.Authentication[:3]
				f.transport.child.raw, _ = json.Marshal(o)
			case "catalog_fence":
				var o CredentialVerificationObservation
				json.Unmarshal(f.transport.child.raw, &o)
				o.After.Database.ApplicationConnect = true
				f.transport.child.raw, _ = json.Marshal(o)
			case "cleanup_error":
				f.transport.child.stopErr = errors.New(sentinel)
			case "deadline":
				f.transport.child.beforeWait = func() { f.now = f.now.Add(51 * time.Second) }
			}
			result, e := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
			if e == nil || !result.ApprovalConsumed || result.State != "failed" || result.ObservationRef != "" || !f.transport.child.stopped {
				t.Fatalf("failed effect not terminal/nonaccepting: %+v %v", result, e)
			}
			if strings.Contains(e.Error(), sentinel) {
				t.Fatal("raw error escaped")
			}
			filepath.Walk(f.home, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !info.IsDir() {
					b, _ := os.ReadFile(path)
					if strings.Contains(string(b), sentinel) {
						t.Errorf("raw remote payload persisted at %s", path)
					}
				}
				return nil
			})
			_, err := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
			if err == nil || f.transport.starts != 1 {
				t.Fatal("failed probe was replayed")
			}
		})
	}
}

func TestCredentialVerificationNativeOwnerForgedPlansCannotConsumeApproval(t *testing.T) {
	for _, which := range []string{"trailing", "duplicate", "approval_plan", "binding", "expiry", "initial_decision", "work_generation", "role_array", "unknown"} {
		t.Run(which, func(t *testing.T) {
			f := newCredentialFixture(t)
			p := f.plan(t)
			f.approve(t, p)
			raw, _ := json.Marshal(p.Plan)
			switch which {
			case "trailing":
				raw = append(raw, []byte(` {}`)...)
			case "duplicate":
				raw = append([]byte(`{"actor":"fixture",`), raw[1:]...)
			case "approval_plan":
				p.Plan.ApprovalPlanID = "approval_plan_forged"
				raw, _ = json.Marshal(p.Plan)
			case "binding":
				p.Plan.ApprovalBindingDigest = strings.Repeat("b", 64)
				raw, _ = json.Marshal(p.Plan)
			case "expiry":
				p.Plan.ApprovalExpiresAt = f.now.Add(time.Hour).Format(time.RFC3339Nano)
				raw, _ = json.Marshal(p.Plan)
			case "initial_decision":
				p.Plan.PolicyDecisionID = "pol_forged"
				raw, _ = json.Marshal(p.Plan)
			case "work_generation":
				p.Plan.WorkSHA256 = strings.Repeat("b", 64)
				raw, _ = json.Marshal(p.Plan)
			case "role_array":
				var fields map[string]any
				json.Unmarshal(raw, &fields)
				r := fields["request"].(map[string]any)
				roles := r["roles"].([]any)
				r["roles"] = append(roles, roles[0])
				raw, _ = json.Marshal(fields)
			case "unknown":
				raw = append([]byte(`{"allowed":true,`), raw[1:]...)
			}
			store, e := NewArtifactStore(f.home)
			if e != nil {
				t.Fatal(e)
			}
			ref, e := store.Put(raw)
			if e != nil {
				t.Fatal(e)
			}
			result, e := f.verifier.Execute(context.Background(), ref, "fixture")
			if e == nil || result.ApprovalConsumed || f.transport.starts != 0 {
				t.Fatalf("forged %s reached admission", which)
			}
		})
	}
}

func TestCredentialVerificationNativeOwnerMalformedAttemptAuthorityNeverConsumes(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	attemptTime, _ := time.Parse("20060102t150405z", f.request.AttemptID[:16])
	id := runattempt.MintDeterministic("credential-verify:"+f.request.AttemptID, attemptTime)
	// Temp-home compatibility marker only; this is an intentional corruption
	// falsifier. It never touches the operator's native authority.
	dir := filepath.Join(f.home, "run_attempts")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"broken":true}`), 0600); e != nil {
		t.Fatal(e)
	}
	result, e := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || result.ApprovalConsumed || f.transport.starts != 0 {
		t.Fatalf("malformed attempt did not fail closed: %+v %v", result, e)
	}
}

func TestCredentialVerificationNativeOwnerFreshPlanCannotReplayAttemptOrConsumeSecondApproval(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	if _, e := f.verifier.Execute(context.Background(), p.PlanRef, "fixture"); e != nil {
		t.Fatal(e)
	}
	f.now = f.now.Add(2 * time.Second)
	next := f.plan(t)
	f.approve(t, next)
	result, e := f.verifier.Execute(context.Background(), next.PlanRef, "fixture")
	if e == nil || result.ApprovalConsumed || f.transport.starts != 1 {
		t.Fatalf("new plan replayed old attempt: %+v %v", result, e)
	}
}

func TestCredentialVerificationNativeOwnerStartFailureConsumesAndTerminalizesWithoutRawError(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	f.transport.startErr = errors.New("FIXTURE_START_DIAGNOSTIC")
	result, e := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || strings.Contains(e.Error(), "FIXTURE_START_DIAGNOSTIC") || !result.ApprovalConsumed || result.State != "failed" || result.ReceiptID == "" || f.transport.child.stopped {
		t.Fatalf("start failure mishandled: %+v %v", result, e)
	}
	attempt, err := runattempt.Open(f.home).Get(result.RunAttemptID)
	if err != nil || attempt.Status != runattempt.StatusFailed {
		t.Fatal("start failure not terminal")
	}
}

func TestCredentialVerificationNativeOwnerConcurrentConsumptionLaunchesOnce(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	type outcome struct {
		result CredentialVerificationResult
		err    error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, e := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
			results <- outcome{r, e}
		}()
	}
	completed, consumed := 0, 0
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err == nil && got.result.State == "completed" {
			completed++
		}
		if got.result.ApprovalConsumed {
			consumed++
		}
	}
	if completed != 1 || consumed != 1 || f.transport.starts != 1 {
		t.Fatalf("concurrent execution counts completed=%d consumed=%d starts=%d", completed, consumed, f.transport.starts)
	}
}

func TestCredentialVerificationNativeOwnerReceiptStorageFailureIsFailedAndKeepsNativeBlocker(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	root := filepath.Join(f.home, "artifacts", "sha256")
	parked := root + ".fixture-parked"
	f.transport.child.beforeWait = func() {
		if e := os.Rename(root, parked); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(root, []byte("fixture storage blocker"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() { os.Remove(root); os.Rename(parked, root) })
	result, e := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || !result.ApprovalConsumed || result.State != "failed" || result.ObservationRef != "" || result.ReceiptID != "" {
		t.Fatalf("storage failure became accepting/ambiguous: %+v %v", result, e)
	}
	attempt, err := runattempt.Open(f.home).Get(result.RunAttemptID)
	if err != nil || attempt.Status != runattempt.StatusFailed {
		t.Fatal("storage failure not terminal")
	}
	s, err := work.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	_, _, present, err := s.ReadReceiptedActionReservationInput(f.workID, p.Plan.ActionIntentID)
	if err != nil || !present {
		t.Fatal("failed persistence discarded native recovery blocker")
	}
}

func TestCredentialVerificationObservationRequiresEveryFactAndExplicitFalseAcceptance(t *testing.T) {
	f := newCredentialFixture(t)
	var raw map[string]json.RawMessage
	valid := credentialObservationJSON(t, f.request)
	json.Unmarshal(valid, &raw)
	for _, key := range []string{"file_matched", "configuration_accepted", "application_access_accepted", "source_capture_accepted", "migration_accepted"} {
		var altered map[string]json.RawMessage
		json.Unmarshal(valid, &altered)
		delete(altered, key)
		encoded, _ := json.Marshal(altered)
		if _, e := decodeCredentialVerificationObservation(encoded, f.request); e == nil {
			t.Fatalf("omitted %s accepted", key)
		}
	}
	var before map[string]json.RawMessage
	json.Unmarshal(raw["before"], &before)
	var roles []map[string]json.RawMessage
	json.Unmarshal(before["roles"], &roles)
	delete(roles[0], "superuser")
	before["roles"], _ = json.Marshal(roles)
	raw["before"], _ = json.Marshal(before)
	encoded, _ := json.Marshal(raw)
	if _, e := decodeCredentialVerificationObservation(encoded, f.request); e == nil {
		t.Fatal("omitted dangerous attribute became false evidence")
	}
}

func TestCredentialVerificationObservationNullIsNotFalseZeroOrEmptyEvidence(t *testing.T) {
	f := newCredentialFixture(t)
	valid := string(credentialObservationJSON(t, f.request))
	for _, pair := range [][2]string{{`"superuser":false`, `"superuser":null`}, {`"membership_rows":0`, `"membership_rows":null`}, {`"sessions":[]`, `"sessions":null`}, {`"configuration_accepted":false`, `"configuration_accepted":null`}} {
		raw := strings.Replace(valid, pair[0], pair[1], 1)
		if raw == valid {
			t.Fatal("falsifier did not change fixture")
		}
		if _, e := decodeCredentialVerificationObservation([]byte(raw), f.request); e == nil {
			t.Fatalf("null replaced fact %s", pair[0])
		}
	}
}

func TestCredentialVerificationProgramPureMockedProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-B", "credential_verify_program_test.py")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "LANG=C", "HOME=/nonexistent"}
	raw, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("pure protocol fixtures: %s", raw)
	}
}

func TestCredentialVerificationGoPythonDigestAndNanosecondIdentityAgree(t *testing.T) {
	f := newCredentialFixture(t)
	r := f.request
	r.File.Device = uint64(1)<<63 + 7
	raw, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	script := `import ast,json,sys,types
tree=ast.parse(open("credential_verify_program.py").read())
tree.body=[node for node in tree.body if not isinstance(node,ast.If)]
module=types.ModuleType("pure_cross_language_fixture")
exec(compile(tree,"<pure_cross_language_fixture>","exec"),module.__dict__)
r=json.load(sys.stdin)
print(json.dumps({"digest":module.canonical_digest(r),"mtime_ns":r["file"]["mtime_ns"],"ctime_ns":r["file"]["ctime_ns"]}))
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-B", "-c", script)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "LANG=C"}
	cmd.Stdin = strings.NewReader(string(raw))
	output, e := cmd.Output()
	if e != nil {
		t.Fatal("pure digest fixture failed")
	}
	var result map[string]string
	if json.Unmarshal(output, &result) != nil || result["digest"] != policy.CredentialVerificationDigest(r) || result["mtime_ns"] != r.File.MtimeNS || result["ctime_ns"] != r.File.CtimeNS {
		t.Fatal("canonical digest or nanosecond identity was coerced")
	}
}

func TestCredentialVerificationRequiresExplicitHome(t *testing.T) {
	for _, h := range []string{"", "relative", "/fixture/../other"} {
		if _, e := NewCredentialVerifier(h); e == nil {
			t.Fatal("ambient or unclean home accepted")
		}
	}
}

func TestCredentialVerificationReservationShape(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	_, ai, e := f.verifier.loadPlan(p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	cfg, base, e := f.verifier.current(p.Plan, ai, p.Plan.WorkSHA256)
	if e != nil {
		t.Fatal(e)
	}
	pd, ok, e := policy.NewExactActionApprovalStore(f.home).ApplyMatching(ai, cfg, base, f.now)
	if e != nil || !ok {
		t.Fatal(e)
	}
	if e = recordEnvelope(f.home, pd.ID, &pd); e != nil {
		t.Fatal(e)
	}
	s, e := work.Open(f.home)
	if e != nil {
		t.Fatal(e)
	}
	in := work.ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: "running", OutcomePending: true, Outcome: "blocked", Changed: false, Summary: "awaiting bounded existing maintenance authentication evidence", MoveType: "audit_only", ProofRefs: []string{ai.ID, "ratt_fixture", "policy-decision:" + pd.ID}, BeforeRefs: []string{p.Plan.Request.Witness.Ref, "request-sha256:" + p.Plan.RequestSHA256}, FilesChanged: []string{}}
	if e = s.ReserveReceiptedAction(f.workID, in, f.now, func() error { return nil }); e != nil {
		t.Fatal(e)
	}
}

func TestCredentialVerificationOwnerHoldsAdmissionAtStartAndReleasesBeforeWait(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	atStart, atWait := false, false
	f.transport.atStart = func() error {
		held, err := home.AutonomyAdmissionLockHeld(f.home)
		if err != nil || !held {
			t.Fatalf("exact start outside native admission: held=%t err=%v", held, err)
		}
		atStart = true
		return nil
	}
	f.transport.child.beforeWait = func() {
		held, err := home.AutonomyAdmissionLockHeld(f.home)
		if err != nil || held {
			t.Fatalf("child Wait retained admission: held=%t err=%v", held, err)
		}
		atWait = true
	}
	result, err := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if err != nil || result.State != "completed" || !atStart || !atWait {
		t.Fatalf("bounded start admission failed: result=%+v err=%v atStart=%t atWait=%t", result, err, atStart, atWait)
	}
}

func TestCredentialVerificationOwnerStopsChildAfterCommittedEvidenceFailure(t *testing.T) {
	f := newCredentialFixture(t)
	p := f.plan(t)
	f.approve(t, p)
	at, err := time.Parse("20060102t150405z", f.request.AttemptID[:16])
	if err != nil {
		t.Fatal(err)
	}
	id := runattempt.MintDeterministic("credential-verify:"+f.request.AttemptID, at)
	mockStarted := false
	f.transport.atStart = func() error {
		// Block only the forthcoming native committed disposition in synthetic
		// storage. No real subprocess, SSH, credential or configuration is used.
		path := filepath.Join(f.home, "run_attempts", "launch", id+".committed.json")
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		mockStarted = true
		return nil
	}
	result, err := f.verifier.Execute(context.Background(), p.PlanRef, "fixture")
	if err == nil || !mockStarted || !result.ApprovalConsumed || result.State != "failed" ||
		!f.transport.child.stopped || result.ObservationRef != "" {
		t.Fatalf("post-start evidence failure escaped cleanup: result=%+v err=%v started=%t stopped=%t", result, err, mockStarted, f.transport.child.stopped)
	}
	if f.transport.child.cleanupDeadline.IsZero() || f.transport.child.cleanupDeadline.After(f.now.Add(60*time.Second)) {
		t.Fatalf("cleanup lost outer deadline: %s", f.transport.child.cleanupDeadline)
	}
	if _, err := f.verifier.Execute(context.Background(), p.PlanRef, "fixture"); err == nil || f.transport.starts != 1 {
		t.Fatalf("uncertain start replayed: starts=%d err=%v", f.transport.starts, err)
	}
}
