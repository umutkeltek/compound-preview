package act

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

type captureQualifierFixture struct {
	owner   *RuntimeConfigInstaller
	request RuntimeCaptureQualificationRequest
	workID  string
	home    string
	now     time.Time
	secret  string
}

func captureFixtureWrite(t *testing.T, path string, v any) RuntimeCaptureFile {
	t.Helper()
	raw, err := runtimeCaptureCanonical(v)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return RuntimeCaptureFile{Path: path, SHA256: runtimeConfigBytesHash(raw)}
}
func captureFixtureStat(t *testing.T, path string) policy.RuntimeConfigInstallStat {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := runtimeConfigLocalStat(info, raw)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
func captureFixtureSourceStat(t *testing.T, path string) map[string]any {
	t.Helper()
	st := captureFixtureStat(t, path)
	raw, _ := json.Marshal(st)
	var m map[string]any
	json.Unmarshal(raw, &m)
	// Do not round-trip nanoseconds through float64.
	m["mtime_ns"], _ = strconv.ParseInt(st.MtimeNS, 10, 64)
	m["ctime_ns"], _ = strconv.ParseInt(st.CtimeNS, 10, 64)
	m["name"] = filepath.Base(path)
	return m
}

func newCaptureQualifierFixture(t *testing.T, h string, now time.Time, profile policy.RuntimeConfigInstallProfile) *captureQualifierFixture {
	return newCaptureQualifierFixtureChanges(t, h, now, profile, nil, nil)
}

func newCaptureQualifierFixtureChanges(t *testing.T, h string, now time.Time, profile policy.RuntimeConfigInstallProfile, bindingChange, privateChange func(map[string]any)) *captureQualifierFixture {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".rci-qualifier-test-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	recipe := filepath.Join(dir, "recipe")
	output := filepath.Join(dir, "capture-fixture")
	for _, p := range []string{recipe, output} {
		if err = os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	profile.Ref = ""
	profile.SHA256 = ""
	profile.APIFields = []string{"DEALER_KEY", "PROVIDER_KEY", "USER_AGENT"}
	f := &captureQualifierFixture{home: h, now: now, secret: "synthetic_provider_must_never_enter_artifacts"}
	f.owner, err = NewRuntimeConfigInstaller(h)
	if err != nil {
		t.Fatal(err)
	}
	f.owner.now = func() time.Time { return now }
	receiver := captureFixtureWrite(t, filepath.Join(recipe, "receiver.py"), map[string]string{"reviewed": "synthetic receiver source; never executed by qualifier"})
	sampler := captureFixtureWrite(t, filepath.Join(recipe, "sampler.py"), map[string]string{"reviewed": "synthetic sampler source; never executed by qualifier"})
	containers := map[string]any{}
	for i, service := range []string{"api", "worker", "provider-sync", "web", "tunnel"} {
		suffix := service
		var project, owner any
		if service == "tunnel" {
			suffix = "cloudflared"
		}
		if i < 3 {
			project, owner = "fixture", service
		}
		containers["fixture-"+suffix] = map[string]any{"id": strings.Repeat(fmt.Sprintf("%x", i+1), 64), "image_id": "sha256:" + strings.Repeat("a", 64), "compose_project": project, "compose_service": owner}
	}
	binding := map[string]any{"schema_version": "fixture.readonly_source_capture_binding.v1", "mac_transport_root": map[string]any{"path": dir, "uid": os.Geteuid(), "gid": os.Getegid()}, "source": map[string]any{"git_revision": strings.Repeat("a", 40), "containers": containers}}
	if bindingChange != nil {
		bindingChange(binding)
	}
	bindingRaw, _ := runtimeCaptureCanonical(binding)
	bindingHash := runtimeConfigBytesHash(bindingRaw)
	template := map[string]any{"attempt_id": "fixture", "binding": binding, "binding_cid": bindingHash, "expected_boot_id": nil, "expected_machine_id": nil, "native_action_intent_id": nil, "native_policy_decision_id": nil, "receiver_sha256": receiver.SHA256, "sampler_sha256": sampler.SHA256}
	templateFile := captureFixtureWrite(t, filepath.Join(recipe, "template.json"), template)
	manifest := captureFixtureWrite(t, filepath.Join(recipe, "manifest.json"), map[string]any{"binding_cid": bindingHash, "files": []map[string]string{{"path": filepath.Base(receiver.Path), "sha256": receiver.SHA256}, {"path": filepath.Base(sampler.Path), "sha256": sampler.SHA256}, {"path": filepath.Base(templateFile.Path), "sha256": templateFile.SHA256}}})
	requestPath := filepath.Join(dir, "request.json")
	coreRaw, _ := json.Marshal(runtimeSourceCore{Argv: []string{"/usr/bin/python3", "-I", "-B", receiver.Path, "--request", requestPath}, Manifest: manifest.SHA256, Template: templateFile.SHA256, Supervisor: 70})
	sourceWork := nativeInstallerWork(t, h, "synthetic original source capture", "", now)
	ai := envelope.ActionIntent{Header: header(envelope.SVActionIntent, idFor("act_", sourceWork.ID, "source-capture"), envelope.ActionIntent{Header: envelope.Header{IDs: envelope.IDs{WorkID: sourceWork.ID}}}, now), Actor: "fixture", Capability: "deploy.run", Action: "ReceiverSHA256 " + receiver.SHA256 + "; samplerSHA256 " + sampler.SHA256 + "; bindingCID " + bindingHash + "; Exact core " + string(coreRaw), Target: envelope.ActionTarget{Kind: "runtime", ID: "fixture-capture", Path: output}, MutationClass: "external_side_effect", Reversibility: "easy", BlastRadius: "local", RollbackPlan: "retain synthetic evidence only"}
	ai.EvidenceRefs = []string{manifest.Path}
	grant := policy.StandingGrant{ID: "grant_fixture_capture", Capability: "deploy.run", MutationClass: "external_side_effect", TargetPath: output, BlastRadius: "local", Reversibility: "easy", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	config, _ := json.Marshal(map[string]any{"standing_grants": []policy.StandingGrant{grant}})
	if err = os.MkdirAll(filepath.Join(h, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(h, "config", "policy.yaml"), config, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := work.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := s.EvaluateAndRecordAction(sourceWork.ID, ai, now)
	if err != nil || !recorded.Recorded || recorded.Decision.StandingGrantID != grant.ID {
		t.Fatalf("source standing admission: %+v %v", recorded, err)
	}
	template["native_action_intent_id"] = ai.ID
	template["native_policy_decision_id"] = recorded.Decision.ID
	requestFile := captureFixtureWrite(t, requestPath, template)
	requestRaw, _ := runtimeCaptureCanonical(template)
	selected := map[string]string{"DEALER_KEY": "synthetic_dealer", "PROVIDER_KEY": f.secret, "USER_AGENT": "fixture agent/<1>&"}
	provider := map[string]string{"PROVIDER_KEY": f.secret, "USER_AGENT": selected["USER_AGENT"]}
	services := map[string]any{}
	fields := map[string][]string{}
	selectedServices := map[string]map[string]string{"api": selected, "worker": provider, "provider-sync": provider, "web": {"WEB_API_BASE": "http://api.fixture.invalid:18100"}, "tunnel": {}}
	for service, values := range selectedServices {
		name := "fixture-" + service
		if service == "tunnel" {
			name = "fixture-cloudflared"
		}
		// These identities have the native sampler's complete closed shape.
		i := map[string]any{"id": strings.Repeat("f", 64), "image_id": "sha256:" + strings.Repeat("a", 64), "name": name, "started_at": "2026-10-17T12:00:00.000Z", "running": true, "restart_count": 0, "compose_project": nil, "compose_service": nil, "compose_files": []string{}, "compose_working_dir": nil, "oci_version": nil, "oci_revision": nil}
		if c, ok := containers[name].(map[string]any); ok {
			for k, v := range c {
				i[k] = v
			}
		}
		services[service] = map[string]any{"identity": i, "record": map[string]any{"selected": values, "database": "UNRELATED_SYNTHETIC_DATABASE_NEVER_FORWARDED"}}
		fields[service] = []string{}
		for key := range values {
			fields[service] = append(fields[service], key)
		}
		sort.Strings(fields[service])
	}
	equivalence := runtimeProviderEquivalence{Different: []string{}, Equal: true, Scope: "api_worker_provider_sync_runtime_values_only"}
	private := map[string]any{"schema_version": "fixture.source_runtime_selected_capture.v1", "state": "consistent_source_capture_not_projected", "binding_cid": bindingHash, "source_git_revision": strings.Repeat("a", 40), "services": services, "origins": map[string]any{"source-app": map[string]string{"raw_sha256": strings.Repeat("b", 64)}}, "provider_equivalence": equivalence, "worker_command": map[string]int{"known_flags": 2, "old_safe_flags_matched": 2, "old_safe_flags_total": 2}, "goal": map[string]bool{"defined": true}}
	if privateChange != nil {
		privateChange(private)
	}
	capturePath := filepath.Join(output, "capture.private.json")
	captureFixtureWrite(t, capturePath, private)
	public := runtimeSourcePublic{SchemaVersion: "fixture.source_runtime_capture_public.v1", State: "source_captured_protected_not_projected", Binding: bindingHash, Revision: strings.Repeat("a", 40), Origins: map[string]string{"source-app": strings.Repeat("b", 64)}, Fields: fields, Provider: equivalence, WorkerObserved: true, WorkerFlags: 2, SafeMatched: 2, SafeTotal: 2, GoalDefined: true, GoalAbsent: true}
	publicFile := captureFixtureWrite(t, filepath.Join(output, "capture.public.json"), public)
	attempt := captureFixtureWrite(t, filepath.Join(output, "ATTEMPT.json"), runtimeSourceAttempt{SchemaVersion: "fixture.source_capture_attempt.v1", State: "source_capture_pending_no_replay", Binding: bindingHash, Action: ai.ID, Policy: recorded.Decision.ID, Receiver: receiver.SHA256, Sampler: sampler.SHA256, Request: runtimeConfigBytesHash(requestRaw)})
	result := captureFixtureWrite(t, filepath.Join(output, "RESULT.json"), map[string]any{"schema_version": "fixture.source_capture_result.v1", "state": "protected_source_capture_not_projected", "binding_cid": bindingHash, "native_action_intent_id": ai.ID, "native_policy_decision_id": recorded.Decision.ID, "private_artifact": captureFixtureSourceStat(t, capturePath), "public_artifact": captureFixtureSourceStat(t, publicFile.Path), "local_transport_custody_profile": fmt.Sprintf("actual-mac%d-selected-json-v1", os.Geteuid()), "remote_origin_custody_profile": "actual-source-origin-v1", "target_projection_accepted": false})
	f.request = RuntimeCaptureQualificationRequest{SchemaVersion: runtimeCaptureRequestSchema, SourceAction: installerProof(t, h, ai.ID, false), SourcePolicy: installerProof(t, h, recorded.Decision.ID, false), Receiver: receiver, Sampler: sampler, Request: requestFile, RequestTemplate: templateFile, Manifest: manifest, Attempt: attempt, Result: result, Public: publicFile, CapturePath: capturePath, CaptureStat: captureFixtureStat(t, capturePath), Profile: profile}
	f.workID = nativeInstallerWork(t, h, "qualify source capture locally", "", now).ID
	return f
}

func (f *captureQualifierFixture) plan(t *testing.T) RuntimeCaptureQualificationPlanResult {
	t.Helper()
	p, err := f.owner.PlanCaptureQualification(f.request, f.workID, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func (f *captureQualifierFixture) approve(t *testing.T, p RuntimeCaptureQualificationPlanResult) {
	t.Helper()
	cfg, _, err := policy.LoadConfig(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = policy.NewExactActionApprovalStore(f.home).Approve(p.Plan.ApprovalPlanID, "operator:fixture", cfg, f.now); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCaptureQualificationBindsPrivateModelEndpoint(t *testing.T) {
	c := newCredentialFixture(t)
	profile := runtimeConfigInstallFixture(c.now).Profile
	modelURL := func(last byte) string {
		return (&url.URL{Scheme: "http", Host: net.JoinHostPort(netip.AddrFrom4([4]byte{100, 64, 0, last}).String(), "11434")}).String()
	}
	profile.APIEndpoint = &policy.RuntimeConfigInstallModelEndpoint{Key: "FIXTURE_OLLAMA_URL", URL: modelURL(1)}
	f := newCaptureQualifierFixture(t, c.home, c.now, profile)
	planned := f.plan(t)
	f.approve(t, planned)
	result, err := f.owner.QualifyCapture(planned.PlanRef, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Profile.APIEndpoint, profile.APIEndpoint) {
		t.Fatal("reviewed model endpoint changed")
	}
	if err = f.owner.captureAuthority(policy.RuntimeConfigInstallRequest{Capture: result.Capture, Profile: result.Profile}); err != nil {
		t.Fatal(err)
	}
	tampered := result.Profile
	endpoint := *tampered.APIEndpoint
	endpoint.URL = modelURL(2)
	tampered.APIEndpoint = &endpoint
	if err = f.owner.captureAuthority(policy.RuntimeConfigInstallRequest{Capture: result.Capture, Profile: tampered}); err == nil {
		t.Fatal("unapproved endpoint change accepted")
	}
}

func TestRuntimeCaptureQualificationProducerNativeConsumptionAndNoSecrets(t *testing.T) {
	c := newCredentialFixture(t)
	f := newCaptureQualifierFixture(t, c.home, c.now, runtimeConfigInstallFixture(c.now).Profile)
	p := f.plan(t)
	artifacts, _ := home.NewArtifactStore(f.home)
	for _, ref := range []string{p.Plan.Qualification.SourceAttempt.Ref, p.Plan.Qualification.SourceResult.Ref, p.Plan.Qualification.SourcePublic.Ref, home.ArtifactRefPrefix + p.Plan.Qualification.ProfileSHA256} {
		if _, err := artifacts.Get(ref); err == nil {
			t.Fatal("acceptance artifact existed before approval")
		}
	}
	if r, err := f.owner.QualifyCapture(p.PlanRef, "fixture"); err == nil || r.ApprovalConsumed {
		t.Fatalf("unapproved qualification %+v %v", r, err)
	}
	f.approve(t, p)
	result, err := f.owner.QualifyCapture(p.PlanRef, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "qualified" || !result.ApprovalConsumed {
		t.Fatalf("not qualified %+v", result)
	}
	if err = f.owner.captureAuthority(policy.RuntimeConfigInstallRequest{Capture: result.Capture, Profile: result.Profile}); err != nil {
		t.Fatal("consumer refused real producer", err)
	}
	selected, err := f.owner.captureProjection(policy.RuntimeConfigInstallRequest{Capture: result.Capture, Profile: result.Profile})
	if err != nil || selected["PROVIDER_KEY"] != f.secret {
		t.Fatal("projection", err)
	}
	retry, err := f.owner.QualifyCapture(p.PlanRef, "fixture")
	if err != nil || !reflect.DeepEqual(result, retry) {
		t.Fatalf("duplicate changed result %+v %v", retry, err)
	}
	err = filepath.WalkDir(f.home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if bytes.Contains(b, []byte(f.secret)) || bytes.Contains(b, []byte("UNRELATED_SYNTHETIC_DATABASE_NEVER_FORWARDED")) {
			t.Fatalf("private value persisted at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeConfigProjectionHashMatchesShippedPython(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	selected := map[string]string{"DEALER_KEY": "synthetic_dealer", "PROVIDER_KEY": "synthetic_provider", "USER_AGENT": "fixture agent/<1>&"}
	raw, _ := json.Marshal(selected)
	cmd := exec.Command(python, "-I", "-B", "-c", `import json,runpy,sys
engine=runpy.run_path("runtime_config_install_program.py",run_name="fixture")
print(engine["digest"](engine["encoded"](json.load(sys.stdin))))`)
	cmd.Stdin = bytes.NewReader(raw)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shipped Python canonicalization: %v %s", err, output)
	}
	if runtimeConfigProjectionHash(selected) != strings.TrimSpace(string(output)) {
		t.Fatal("Go selected projection differs from shipped Python payload hash")
	}
}

func TestRuntimeCaptureQualificationResumesNativeVerifyingState(t *testing.T) {
	c := newCredentialFixture(t)
	f := newCaptureQualifierFixture(t, c.home, c.now, runtimeConfigInstallFixture(c.now).Profile)
	planned := f.plan(t)
	f.approve(t, planned)
	p, ai, err := f.owner.loadCaptureQualificationPlan(planned.PlanRef, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := work.Open(f.home)
	admitted, err := s.EvaluateAndRecordAction(p.WorkID, ai, f.now)
	if err != nil || !admitted.Recorded {
		t.Fatalf("native qualification admission: %v", err)
	}
	validated, err := f.owner.validateCaptureQualification(p.Request, p.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewArtifactStore(f.home)
	for _, body := range validated.metadata {
		if _, err = store.Put(body); err != nil {
			t.Fatal(err)
		}
	}
	q := validated.qualification
	q.Action, err = f.owner.nativeCaptureProof(ai.ID)
	if err != nil {
		t.Fatal(err)
	}
	q.Policy, err = f.owner.nativeCaptureProof(admitted.Decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(q)
	ref, err := store.Put(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Advance(p.WorkID, work.AdvanceInput{To: "verifying", Actor: "fixture", ArtifactID: ref}, f.now); err != nil {
		t.Fatal(err)
	}
	// This is the persisted state after metadata publication and Advance, before
	// Close completes. Only the real producer may complete its terminal proof.
	result, err := f.owner.QualifyCapture(planned.PlanRef, "fixture")
	if err != nil || result.State != "qualified" || !result.ApprovalConsumed {
		t.Fatalf("resume failed: %+v %v", result, err)
	}
	if err = f.owner.captureAuthority(policy.RuntimeConfigInstallRequest{Capture: result.Capture, Profile: result.Profile}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCaptureQualificationRefusesChangedOrFalseSourceClaims(t *testing.T) {
	for _, kind := range []string{"receiver", "request", "manifest", "attempt", "failed-result", "false-public", "private-mode", "private-hardlink", "private-symlink", "unknown-metadata", "wrong-action", "wrong-profile", "extra-output"} {
		t.Run(kind, func(t *testing.T) {
			c := newCredentialFixture(t)
			f := newCaptureQualifierFixture(t, c.home, c.now, runtimeConfigInstallFixture(c.now).Profile)
			rewrite := func(file *RuntimeCaptureFile, key string, value any) {
				var m map[string]any
				b, _ := os.ReadFile(file.Path)
				json.Unmarshal(b, &m)
				m[key] = value
				*file = captureFixtureWrite(t, file.Path, m)
			}
			switch kind {
			case "receiver":
				os.WriteFile(f.request.Receiver.Path, []byte("changed"), 0600)
			case "request":
				rewrite(&f.request.Request, "attempt_id", "changed")
			case "manifest":
				rewrite(&f.request.Manifest, "binding_cid", strings.Repeat("c", 64))
			case "attempt":
				rewrite(&f.request.Attempt, "state", "failed")
			case "failed-result":
				rewrite(&f.request.Result, "state", "failed")
			case "false-public":
				rewrite(&f.request.Public, "goal_defined", false)
			case "private-mode":
				os.Chmod(f.request.CapturePath, 0644)
			case "private-hardlink":
				os.Link(f.request.CapturePath, filepath.Join(filepath.Dir(filepath.Dir(f.request.CapturePath)), "alias"))
			case "private-symlink":
				dst := f.request.CapturePath + ".retained"
				os.Rename(f.request.CapturePath, dst)
				os.Symlink(dst, f.request.CapturePath)
			case "unknown-metadata":
				rewrite(&f.request.Attempt, "private_value", f.secret)
			case "wrong-action":
				f.request.SourceAction = f.request.SourcePolicy
			case "wrong-profile":
				f.request.Profile.APIFields = append(f.request.Profile.APIFields, "DATABASE_URL")
				sort.Strings(f.request.Profile.APIFields)
			case "extra-output":
				os.WriteFile(filepath.Join(filepath.Dir(f.request.CapturePath), "FAILURE.json"), []byte(`{}`), 0600)
			}
			if _, err := f.owner.PlanCaptureQualification(f.request, f.workID, "fixture"); err == nil {
				t.Fatal("accepted false source evidence")
			}
			s, _ := work.Open(f.home)
			w, _ := s.Get(f.workID)
			if w.State != "running" {
				t.Fatal("refusal closed Work")
			}
		})
	}
}

func TestRuntimeCaptureQualificationApprovalBindsCustodyAndPreservesPolicyFloors(t *testing.T) {
	for _, kind := range []string{"drift-after-approval", "forbidden-path", "security-deny", "wrong-actor"} {
		t.Run(kind, func(t *testing.T) {
			c := newCredentialFixture(t)
			f := newCaptureQualifierFixture(t, c.home, c.now, runtimeConfigInstallFixture(c.now).Profile)
			p := f.plan(t)
			f.approve(t, p)
			actor := "fixture"
			switch kind {
			case "drift-after-approval":
				os.WriteFile(f.request.Receiver.Path, []byte("changed"), 0600)
			case "forbidden-path":
				b, _ := json.Marshal(map[string]any{"forbidden_paths": []string{f.request.Request.Path}})
				os.WriteFile(filepath.Join(f.home, "config", "policy.yaml"), b, 0600)
			case "security-deny":
				os.WriteFile(filepath.Join(f.home, "config", "policy.yaml"), []byte("class_outcome:\n  local_reversible:\n    outcome: deny\n"), 0600)
			case "wrong-actor":
				actor = "other"
			}
			result, err := f.owner.QualifyCapture(p.PlanRef, actor)
			if err == nil || result.State == "qualified" || result.ApprovalConsumed {
				t.Fatalf("refusal lost %+v %v", result, err)
			}
		})
	}
}

func TestRuntimeCaptureQualificationAcceptsStructuredSourceContainers(t *testing.T) {
	c := newCredentialFixture(t)
	f := newCaptureQualifierFixture(t, c.home, c.now, runtimeConfigInstallFixture(c.now).Profile)
	p := f.plan(t)
	f.approve(t, p)
	result, err := f.owner.QualifyCapture(p.PlanRef, "fixture")
	if err != nil || result.State != "qualified" || !result.ApprovalConsumed {
		t.Fatalf("native-shaped containers refused: %+v %v", result, err)
	}
}

func TestRuntimeCaptureQualificationRejectsContainerBindingDrift(t *testing.T) {
	for _, kind := range []string{"flat-string", "missing-container", "extra-container", "missing-field", "unknown-field", "null-id", "invalid-id", "invalid-image", "duplicate-id", "wrong-project", "wrong-service", "standalone-owner", "missing-nullable"} {
		t.Run(kind, func(t *testing.T) {
			c := newCredentialFixture(t)
			change := func(binding map[string]any) {
				containers := binding["source"].(map[string]any)["containers"].(map[string]any)
				api := containers["fixture-api"].(map[string]any)
				switch kind {
				case "flat-string":
					containers["fixture-api"] = "api"
				case "missing-container":
					delete(containers, "fixture-web")
				case "extra-container":
					containers["fixture-extra"] = api
				case "missing-field":
					delete(api, "image_id")
				case "unknown-field":
					api["extra"] = "synthetic"
				case "null-id":
					api["id"] = nil
				case "invalid-id":
					api["id"] = strings.Repeat("z", 64)
				case "invalid-image":
					api["image_id"] = strings.Repeat("a", 64)
				case "duplicate-id":
					api["id"] = containers["fixture-worker"].(map[string]any)["id"]
				case "wrong-project":
					api["compose_project"] = "other"
				case "wrong-service":
					api["compose_service"] = "worker"
				case "standalone-owner":
					containers["fixture-web"].(map[string]any)["compose_project"] = "fixture"
				case "missing-nullable":
					delete(containers["fixture-cloudflared"].(map[string]any), "compose_service")
				}
			}
			f := newCaptureQualifierFixtureChanges(t, c.home, c.now, runtimeConfigInstallFixture(c.now).Profile, change, nil)
			s, _ := work.OpenReadOnly(f.home)
			before, _ := s.Get(f.workID)
			p, err := f.owner.PlanCaptureQualification(f.request, f.workID, "fixture")
			if err == nil || p.PlanRef != "" {
				t.Fatalf("container binding %s accepted: %+v %v", kind, p, err)
			}
			w, _ := s.Get(f.workID)
			if !reflect.DeepEqual(w.ActionIntentIDs, before.ActionIntentIDs) {
				t.Fatal("rejected binding published qualification action")
			}
		})
	}
}

func TestRuntimeCaptureQualificationRejectsPrivateContainerIdentityDrift(t *testing.T) {
	for _, field := range []string{"id", "image_id", "name", "compose_project", "compose_service", "missing-field", "unknown-field", "running"} {
		t.Run(field, func(t *testing.T) {
			c := newCredentialFixture(t)
			change := func(private map[string]any) {
				i := private["services"].(map[string]any)["api"].(map[string]any)["identity"].(map[string]any)
				switch field {
				case "id":
					i[field] = strings.Repeat("e", 64)
				case "image_id":
					i[field] = "sha256:" + strings.Repeat("e", 64)
				case "missing-field":
					delete(i, "compose_service")
				case "unknown-field":
					i["extra"] = "synthetic"
				case "running":
					i[field] = false
				default:
					i[field] = "other"
				}
			}
			f := newCaptureQualifierFixtureChanges(t, c.home, c.now, runtimeConfigInstallFixture(c.now).Profile, nil, change)
			p, err := f.owner.PlanCaptureQualification(f.request, f.workID, "fixture")
			if err == nil || p.PlanRef != "" {
				t.Fatalf("private identity %s accepted: %+v %v", field, p, err)
			}
		})
	}
}
