package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	homepkg "compound/internal/home"
	"compound/internal/runattempt"
)

type ompVerifierFixture struct {
	root, workspace, models, native, wrapper, endpoint, home string
	interpreter                                              ompNativeBinding
	adapter                                                  *OMPNativeVerifierAdapter
	input                                                    RunInput
}

func newOMPVerifierFixture(t *testing.T, nativeOverride string) ompVerifierFixture {
	t.Helper()
	root, workspace := t.TempDir(), t.TempDir()
	models := filepath.Join(root, "profile", "agent", "models.yml")
	if err := os.MkdirAll(filepath.Dir(models), 0o700); err != nil {
		t.Fatal(err)
	}
	endpoint, requestModel, profileID := "https://capture.fixture.invalid", "test/omp-model", "omp-test"
	modelsBody := []byte("endpoint=" + endpoint + "\n")
	if source := os.Getenv("COMPOUND_OMP_NATIVE_DEV_MODELS"); source != "" && nativeOverride == "" {
		var err error
		modelsBody, err = os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		endpoint = os.Getenv("COMPOUND_OMP_NATIVE_DEV_ENDPOINT")
		requestModel = os.Getenv("COMPOUND_OMP_NATIVE_DEV_MODEL")
		profileID = os.Getenv("COMPOUND_OMP_NATIVE_DEV_PROFILE")
		if endpoint == "" || requestModel == "" || profileID == "" {
			t.Fatal("native dev fixture requires endpoint, model, and profile")
		}
	}
	if err := os.WriteFile(models, modelsBody, 0o600); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(root, "native-omp")
	nativeBody := `#!/usr/bin/python3
import json,os,urllib.request,datetime,sys
endpoint=open(os.path.join(os.environ["PI_CODING_AGENT_DIR"],"models.yml")).read().strip().split("=",1)[1]
prompt=sys.argv[-1]
cwd=os.getcwd().replace(chr(92),"/")
if sys.platform=="darwin" and cwd.startswith("/private/") and os.path.exists(cwd[8:]) and os.path.realpath(cwd)==os.path.realpath(cwd[8:]):
 cwd=cwd[8:]
date=datetime.date.today().isoformat()
reminder="<system-reminder>\nToday: "+date+"; current working directory: '"+cwd+"'. Do not repeat this information in your reply.\n</system-reminder>"
body=json.dumps({"model":"test/omp-model","stream":True,"messages":[{"role":"user","content":[{"type":"text","text":reminder},{"type":"text","text":prompt}]}],"tools":[]}).encode()
headers={"content-type":"application/json","x-switchback-launch-profile":"omp-test","x-switchback-conformance-revision":"sha256:"+("a"*64),"x-switchback-capture-policy":"wire","x-switchback-requested-effort":"high"}
with urllib.request.urlopen(urllib.request.Request(endpoint+"/v1/chat/completions",data=body,headers=headers),timeout=10) as response:
    print(response.read().decode())
`
	if nativeOverride != "" {
		nativeBody = nativeOverride
	}
	if devNative := os.Getenv("COMPOUND_OMP_NATIVE_DEV_BINARY"); devNative != "" && nativeOverride == "" {
		var err error
		native, err = filepath.EvalSymlinks(devNative)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ompFileSHA256(native)
		if err != nil || got != os.Getenv("COMPOUND_OMP_NATIVE_DEV_SHA256") {
			t.Fatalf("native dev binary digest = %q, err=%v", got, err)
		}
		t.Log("non-certifying exact native development probe")
	} else if err := os.WriteFile(native, []byte(nativeBody), 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "wrapper")
	wrapperBody := fmt.Sprintf(`#!/bin/sh
# Use POSIX sh: Linux runners may not provide /bin/zsh.
# A missing shebang interpreter causes ENOENT before the probe runs.
export SB_LAUNCH_CREDENTIAL_ENV='OMP_TEST_KEY'
[ "$#" -eq 6 ] && [ "$1" = -p ] && [ "$2" = --no-session ] && [ "$3" = --cwd ] && [ "$5" = --no-tools ] || exit 64
run="$(mktemp -d "$TMPDIR/sb-omp-run.XXXXXXXX")" || exit 73
trap '/bin/rm -rf "$run"' EXIT
/bin/cp %q "$run/models.yml" || exit 74
export PI_CONFIG_DIR="$run" PI_CODING_AGENT_DIR="$run"
exec omp --cwd="$4" --provider=switchback --model=%s --mode=text --print --no-session --prompt-only --no-tools --no-extensions --no-skills --no-rules --no-prewalk --approval-mode=always-ask --thinking=high -- "$6"
`, models, "switchback/"+requestModel)
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o700); err != nil {
		t.Fatal(err)
	}
	p := validOMPOwnerProjection(t)
	p.Profile.ID = profileID
	p.LaunchProfileRef = "switchback://launch-profiles/" + profileID
	p.Profile.CaptureEndpoint = endpoint
	p.Profile.RequestedModel, p.Profile.RequestModel = requestModel, requestModel
	p.PromptOnly.FixedArgv[2] = "--model=switchback/" + requestModel
	p.PromptOnly.Wrapper.Path, _ = filepath.EvalSymlinks(wrapper)
	p.PromptOnly.Wrapper.SHA256, _ = ompFileSHA256(wrapper)
	p.PromptOnly.Wrapper.ExpectedSHA256 = p.PromptOnly.Wrapper.SHA256
	p.PromptOnly.ModelsConfig.Path, _ = filepath.EvalSymlinks(models)
	p.PromptOnly.ModelsConfig.SHA256, _ = ompFileSHA256(models)
	p.PromptOnly.ModelsConfig.ExpectedSHA256 = p.PromptOnly.ModelsConfig.SHA256
	p.PromptOnly.Executable.Path, _ = filepath.EvalSymlinks(native)
	p.PromptOnly.Executable.SHA256, _ = ompFileSHA256(native)
	projectionRoot := filepath.Join(root, "projection")
	if err := os.MkdirAll(projectionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(projectionRoot, profileID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	interpreter, err := resolveOMPVerifierInterpreter("")
	if err != nil {
		t.Skip(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Exercise the production cadence; millisecond durable heartbeats add unrelated
	// filesystem contention to the native subprocess deadline under parallel load.
	a := &OMPNativeVerifierAdapter{store: runattempt.Open(home), heartbeatInterval: defaultHeartbeatInterval, projectionRoot: projectionRoot, profileID: profileID, interpreterPath: interpreter.Path}
	in := RunInput{WorkID: "work_omp_native", Workspace: workspace, Prompt: "prove prompt only", Now: time.Now().UTC(), ActionIntentID: "act_fixture", StartGuard: func(_ context.Context, start func() error) error { return start() }, Timeout: time.Minute}

	return ompVerifierFixture{root, workspace, models, native, wrapper, endpoint, home, interpreter, a, in}
}
func TestOMPNativeVerifierRunsOwnedWrapperAndNativeHostileProbe(t *testing.T) {
	f := newOMPVerifierFixture(t, "")
	root, workspace, models, native, wrapper, endpoint, home := f.root, f.workspace, f.models, f.native, f.wrapper, f.endpoint, f.home
	interpreter, a, in := f.interpreter, f.adapter, f.input
	out, err := a.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := os.ReadFile(out.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("status = %s transcript=%s", out.RunAttempt.Status, transcript)
	}
	if len(out.RunAttempt.ProofRefs) != 3 {
		t.Fatalf("proof refs = %v", out.RunAttempt.ProofRefs)
	}
	if !strings.Contains(string(transcript), "OMP_NATIVE_CAPTURE_READY") {
		t.Fatal("content-free capture readiness missing")
	}
	for _, canary := range []string{workspace, "prove prompt only", endpoint, wrapper, models, native} {
		if strings.Contains(string(transcript), canary) || strings.Contains(out.RunAttempt.Command, canary) {
			t.Fatalf("private canary escaped public transcript/command: %q", canary)
		}
	}
	artifacts, err := homepkg.NewArtifactStore(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range out.RunAttempt.ProofRefs {
		body, err := artifacts.Get(ref)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{workspace, "prove prompt only", endpoint, wrapper, models, native, "must execute"} {
			if strings.Contains(string(body), canary) {
				t.Fatalf("private canary escaped public artifact %s: %q", ref, canary)
			}
		}
	}
	proofBody, err := artifacts.Get(out.RunAttempt.ProofRefs[len(out.RunAttempt.ProofRefs)-1])
	if err != nil {
		t.Fatal(err)
	}
	proof, err := DecodeOMPNativePublicProof(proofBody)
	if err != nil {
		t.Fatalf("native emitter proof rejected by strict decoder: %v", err)
	}
	if proof.WorkID != out.RunAttempt.WorkID || proof.RunAttemptID != out.RunAttempt.ID || proof.ActionIntentID != out.RunAttempt.ActionIntentID {
		t.Fatal("decoded proof identity differs from actual producer attempt")
	}
	if !reflectStringSliceEqual(proof.EvidenceRefs, out.RunAttempt.ProofRefs[:2]) {
		t.Fatal("decoded evidence refs differ from emitted recipe-first/report-second refs")
	}
	wantPrivate := map[string][]byte{"interpreter": mustReadTestFile(t, interpreter.Path), "wrapper": mustReadTestFile(t, wrapper), "models_config": mustReadTestFile(t, models), "executable": mustReadTestFile(t, native)}
	for role, want := range wantPrivate {
		digest := proof.PrivateCustodySHA256[role]
		if len(digest) != 64 {
			t.Fatalf("private custody digest missing for %s", role)
		}
		ref := homepkg.ArtifactRefPrefix + digest
		if strings.Contains(strings.Join(out.RunAttempt.ProofRefs, "\n"), ref) {
			t.Fatalf("private ref entered public proof refs: %s", ref)
		}
		got, err := artifacts.GetPrivate(ref)
		if err != nil {
			t.Fatalf("GetPrivate(%s): %v", role, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("private custody bytes differ for %s", role)
		}
	}
	var privateBodies [][]byte
	err = filepath.Walk(filepath.Join(home, "private", "artifacts", "sha256"), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().IsRegular() {
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			privateBodies = append(privateBodies, body)
		}
		return nil
	})
	if err != nil || len(privateBodies) != 5 {
		t.Fatalf("private capture inventory = %d, %v", len(privateBodies), err)
	}
	var private struct {
		SchemaVersion string          `json:"schema_version"`
		Config        json.RawMessage `json:"config"`
		Report        json.RawMessage `json:"report"`
	}
	var captureBody []byte
	for _, candidate := range privateBodies {
		var probe struct {
			SchemaVersion string `json:"schema_version"`
		}
		if json.Unmarshal(candidate, &probe) == nil && probe.SchemaVersion == "compound.omp_native_verifier_private.v0.1" {
			captureBody = candidate
		}
	}
	if err := json.Unmarshal(captureBody, &private); err != nil || private.SchemaVersion != "compound.omp_native_verifier_private.v0.1" || len(private.Config) == 0 || len(private.Report) == 0 {
		t.Fatalf("private capture was not one exact typed replacement envelope: schema=%q config=%d report=%d err=%v", private.SchemaVersion, len(private.Config), len(private.Report), err)
	}
	for _, canary := range []string{workspace, "prove prompt only", endpoint, wrapper, models, native} {
		if !strings.Contains(string(captureBody), canary) {
			t.Fatalf("raw canary missing from private capture: %q", canary)
		}
	}
	if len(proof.PrivateCustodySHA256) != 5 {
		t.Fatalf("private custody roles = %v, want exact five roles", proof.PrivateCustodySHA256)
	}
	for _, role := range []string{"capture", "interpreter", "wrapper", "models_config", "executable"} {
		if len(proof.PrivateCustodySHA256[role]) != 64 {
			t.Fatalf("private custody role missing: %s", role)
		}
	}
	captureSum := sha256.Sum256(captureBody)
	if proof.PrivateCustodySHA256["capture"] != hex.EncodeToString(captureSum[:]) {
		t.Fatalf("capture role digest = %q", proof.PrivateCustodySHA256["capture"])
	}
	storedCapture, err := artifacts.GetPrivate(homepkg.ArtifactRefPrefix + proof.PrivateCustodySHA256["capture"])
	if err != nil || !bytes.Equal(storedCapture, captureBody) {
		t.Fatalf("capture role GetPrivate bytes differ: %v", err)
	}

	badHome := filepath.Join(root, "put-private-failure-home")
	poison := filepath.Join(badHome, "private", "artifacts", "sha256", interpreter.SHA256)
	if err := os.MkdirAll(poison, 0o700); err != nil {
		t.Fatal(err)
	}
	badAdapter := *a
	badAdapter.store = runattempt.Open(badHome)
	badInput := in
	badInput.WorkID = "work_omp_private_put_refused"
	badInput.Now = time.Now().UTC().Add(2 * time.Second)
	badOut, badErr := badAdapter.Run(context.Background(), badInput)
	if badErr == nil || !strings.Contains(badErr.Error(), "OMP_PRIVATE_PUT") || strings.Contains(badErr.Error(), badHome) || strings.Contains(badErr.Error(), "prove prompt only") || badOut.RunAttempt.Status != runattempt.StatusFailed || len(badOut.RunAttempt.ProofRefs) != 1 {
		t.Fatalf("private Put refusal out=%+v err=%v", badOut, badErr)
	}
	retained := assertOMPFailedPrivateCapture(t, badHome, badInput, badOut)
	if _, err := decodeOMPPrivateEnvelope(retained); err != nil {
		t.Fatal("private Put failure lost its exact capture envelope")
	}
	var publicBodies int
	_ = filepath.Walk(filepath.Join(badHome, "artifacts", "sha256"), func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && info.Mode().IsRegular() {
			publicBodies++
		}
		return nil
	})
	if publicBodies != 0 {
		t.Fatalf("private Put refusal published %d public bodies", publicBodies)
	}
	failedInput := in
	failedInput.WorkID = "work_omp_native_start_refused"
	failedInput.Now = time.Now().UTC().Add(time.Second)
	failedInput.StartGuard = func(context.Context, func() error) error { return errors.New("private-start-canary") }
	failedOut, failedErr := a.Run(context.Background(), failedInput)
	if failedErr == nil || strings.Contains(failedErr.Error(), "private-start-canary") || !strings.Contains(failedErr.Error(), "OMP_PROCESS_START") || failedOut.RunAttempt.Status != runattempt.StatusFailed || len(failedOut.RunAttempt.ProofRefs) != 0 {
		t.Fatalf("non-finalizer cleanup/start privacy out=%+v err=%v", failedOut, failedErr)
	}
}

func mustReadTestFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
