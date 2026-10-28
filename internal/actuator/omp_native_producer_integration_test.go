package actuator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestOMPNativeProducerReachesGovernedAdvance(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skipf("zsh unavailable: %v", err)
	}

	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node unavailable: %v", err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	t.Setenv("SWITCHBACK_RUNTIME_ROOT", runtimeRoot)

	homeDir := filepath.Join(root, "home")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	models := filepath.Join(root, "profile", "agent", "models.yml")
	if err := os.MkdirAll(filepath.Dir(models), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(models, []byte("endpoint=https://capture.fixture.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	nativeDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(nativeDir, "omp")
	nativeBody := `#!/usr/bin/python3
import json,os,urllib.request,datetime,sys
endpoint=open(os.path.join(os.environ["PI_CODING_AGENT_DIR"],"models.yml")).read().strip().split("=",1)[1]
if endpoint == "https://capture.fixture.invalid":
    print('{"type":"text","text":"synthetic prompt-only response"}')
    sys.exit(0)
prompt=sys.argv[-1]
cwd=os.getcwd().replace(chr(92),"/")
if sys.platform=="darwin" and cwd.startswith("/private/") and os.path.exists(cwd[8:]) and os.path.realpath(cwd)==os.path.realpath(cwd[8:]):
    cwd=cwd[8:]
date=datetime.date.today().isoformat()
reminder="<system-reminder>\nToday: "+date+"; current working directory: '"+cwd+"'. Do not repeat this information in your reply.\n</system-reminder>"
body=json.dumps({"model":"test/omp-model","stream":True,"messages":[{"role":"user","content":[{"type":"text","text":reminder},{"type":"text","text":prompt}]}],"tools":[]}).encode()
headers={"content-type":"application/json","x-switchback-launch-profile":"omp","x-switchback-conformance-revision":"sha256:"+("a"*64),"x-switchback-capture-policy":"wire","x-switchback-requested-effort":"high"}
with urllib.request.urlopen(urllib.request.Request(endpoint+"/v1/chat/completions",data=body,headers=headers),timeout=10) as response:
    print(response.read().decode())
`
	if err := os.WriteFile(native, []byte(nativeBody), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", nativeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OMP_TEST_KEY", "synthetic-credential")

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
exec omp --cwd="$4" --provider=switchback --model=switchback/test/omp-model --mode=text --print --no-session --prompt-only --no-tools --no-extensions --no-skills --no-rules --no-prewalk --approval-mode=always-ask --thinking=high -- "$6"
`, models)
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o700); err != nil {
		t.Fatal(err)
	}

	p := validOMPNativeLaunchOwnerProjection(t)
	p.Profile.ID = "omp"
	p.LaunchProfileRef = "switchback://launch-profiles/omp"
	p.Profile.CaptureEndpoint = "https://capture.fixture.invalid"
	p.Profile.RequestedModel, p.Profile.RequestModel = "test/omp-model", "test/omp-model"
	p.PromptOnly.FixedArgv[2] = "--model=switchback/test/omp-model"
	p.PromptOnly.Wrapper.Path, p.PromptOnly.Wrapper.SHA256 = ompProducerBinding(t, wrapper)
	p.PromptOnly.Wrapper.ExpectedSHA256 = p.PromptOnly.Wrapper.SHA256
	p.PromptOnly.ModelsConfig.Path, p.PromptOnly.ModelsConfig.SHA256 = ompProducerBinding(t, models)
	p.PromptOnly.ModelsConfig.ExpectedSHA256 = p.PromptOnly.ModelsConfig.SHA256
	p.PromptOnly.Executable.Path, p.PromptOnly.Executable.SHA256 = ompProducerBinding(t, native)
	if err := p.Validate("omp"); err != nil {
		t.Fatalf("projection contract: %v", err)
	}

	projectionRoot := filepath.Join(runtimeRoot, "state", "profile-conformance")
	if err := os.MkdirAll(projectionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	projectionBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectionRoot, "omp.json"), projectionBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if err := home.Init(homeDir, "test", now); err != nil {
		t.Fatal(err)
	}
	canonicalHome, err := filepath.EvalSymlinks(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}

	store, err := work.Open(canonicalHome)
	if err != nil {
		t.Fatal(err)
	}

	w, err := store.Create(work.CreateInput{
		Owner:            "omp-producer-test",
		SuccessPredicate: "synthetic governed native proof",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	actor := "omp-producer-test"
	if _, err := store.Claim(w.ID, actor, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != "claimed" {
		t.Fatalf("fresh claimed work state = %q, want claimed", claimed.State)
	}

	adapter := harness.NewOMPNativeVerifierAdapter(runattempt.Open(canonicalHome))

	runInput := harness.RunInput{
		WorkID:    claimed.ID,
		Workspace: canonicalWorkspace,
		Prompt:    "synthetic native producer",
		Timeout:   time.Minute,
		Now:       now,
	}
	resolved, err := adapter.Preflight(context.Background(), runInput)
	if err != nil {
		t.Fatal(err)
	}

	executed, err := ExecuteGovernedHarnessAdvance(
		context.Background(),
		canonicalHome,
		store,
		claimed,
		adapter,
		resolved,
		actor,
		"running",
		"synthetic native producer",
		claimed.RunAttemptIDs,
		false,
		now,
	)
	if err != nil {
		t.Fatalf("ExecuteGovernedHarnessAdvance: %v", err)
	}
	if executed.RunError != nil {
		t.Fatalf("adapter.Run returned RunError: %v", executed.RunError)
	}
	if !executed.Finalized.Succeeded {
		t.Fatalf("Finalized.Succeeded = false: %+v", executed.Finalized)
	}

	final, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "verifying" {
		t.Fatalf("final state = %q, want verifying", final.State)
	}

	refs := executed.Output.RunAttempt.ProofRefs
	if len(refs) != 3 {
		t.Fatalf("expected 3 proof refs, got %d: %v", len(refs), refs)
	}
	attemptID := executed.Output.RunAttempt.ID
	if !runattempt.Valid(attemptID) {
		t.Fatal("governed execution did not return a valid RunAttempt ID")
	}
	attemptStore := runattempt.Open(canonicalHome)
	attempt, err := attemptStore.Get(attemptID)
	if err != nil {
		t.Fatalf("attemptStore.Get(%q): %v", attemptID, err)
	}
	if attempt.Status != "succeeded" {
		t.Fatalf("attempt.Status = %q, want succeeded", attempt.Status)
	}
	if attempt.Origin != "harness_execution" {
		t.Fatalf("attempt.Origin = %q, want harness_execution", attempt.Origin)
	}
	if attempt.Harness != "omp-native-verifier" {
		t.Fatalf("attempt.Harness = %q, want omp-native-verifier", attempt.Harness)
	}
	if attempt.WorkID != executed.RunningWork.ID {
		t.Fatalf("attempt.WorkID = %q, want %q", attempt.WorkID, executed.RunningWork.ID)
	}
	runningAction := ompProducerLastActionIntent(executed.RunningWork)
	if attempt.ActionIntentID != runningAction {
		t.Fatalf("attempt.ActionIntentID = %q, want %q", attempt.ActionIntentID, runningAction)
	}
	if attempt.ExecutionPosture == nil {
		t.Fatal("ExecutionPosture nil")
	}
	if attempt.ExecutionPosture.PermissionMode != "prompt_only" {
		t.Fatalf("PermissionMode = %q, want prompt_only", attempt.ExecutionPosture.PermissionMode)
	}
	if attempt.ExecutionPosture.PolicyDecisionID != executed.RunningWork.PolicyDecisionID {
		t.Fatalf("PolicyDecisionID = %q, want %q", attempt.ExecutionPosture.PolicyDecisionID, executed.RunningWork.PolicyDecisionID)
	}
	if len(attempt.ExecutionPosture.AuthorityEvidence) != 1 {
		t.Fatalf("AuthorityEvidence len = %d, want 1", len(attempt.ExecutionPosture.AuthorityEvidence))
	}
	ev := attempt.ExecutionPosture.AuthorityEvidence[0]
	if ev.Kind != "omp_native_verifier" || ev.Ref != "harness:omp-native-verifier" || ev.Revision != "v0.1" {
		t.Fatalf("authority evidence = %+v", ev)
	}
	if attempt.Launch == nil || attempt.Launch.PreparedAt == nil || attempt.Launch.CommittedAt == nil || attempt.Launch.StartErrorAt != nil {
		t.Fatalf("Launch = %+v", attempt.Launch)
	}
	if len(attempt.ProofRefs) != 3 {
		t.Fatalf("attempt.ProofRefs len = %d, want 3", len(attempt.ProofRefs))
	}

	if len(final.RunAttemptIDs) <= len(claimed.RunAttemptIDs) {
		t.Fatalf("final RunAttemptIDs did not grow past claim: before=%v after=%v", claimed.RunAttemptIDs, final.RunAttemptIDs)
	}
	seen := false
	for _, id := range final.RunAttemptIDs {
		if id == attempt.ID {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatalf("attempt %q not present in final RunAttemptIDs %v", attempt.ID, final.RunAttemptIDs)
	}
	for _, id := range claimed.RunAttemptIDs {
		if id == attempt.ID {
			t.Fatalf("forward attempt %q duplicated in claim attempts %v", attempt.ID, claimed.RunAttemptIDs)
		}
	}

	artifacts, err := home.NewArtifactStore(canonicalHome)
	if err != nil {
		t.Fatal(err)
	}
	proofBytes, err := artifacts.Get(refs[2])
	if err != nil {
		t.Fatalf("ArtifactStore.Get last proof ref: %v", err)
	}
	decoded, err := harness.DecodeOMPNativePublicProof(proofBytes)
	if err != nil {
		t.Fatalf("DecodeOMPNativePublicProof: %v", err)
	}
	if decoded.WorkID != executed.RunningWork.ID {
		t.Fatalf("proof WorkID = %q, want %q", decoded.WorkID, executed.RunningWork.ID)
	}
	if decoded.RunAttemptID != attempt.ID {
		t.Fatalf("proof AttemptID = %q, want %q", decoded.RunAttemptID, attempt.ID)
	}
	if decoded.ActionIntentID != runningAction {
		t.Fatalf("proof ActionIntentID = %q, want %q", decoded.ActionIntentID, runningAction)
	}
	if len(decoded.EvidenceRefs) != 2 {
		t.Fatalf("proof EvidenceRefs len = %d, want 2", len(decoded.EvidenceRefs))
	}
	if decoded.EvidenceRefs[0] != refs[0] || decoded.EvidenceRefs[1] != refs[1] {
		t.Fatalf("proof evidence refs not ordered first two: got %v want [%s,%s,...]", decoded.EvidenceRefs, refs[0], refs[1])
	}

	for role, digest := range decoded.PrivateCustodySHA256 {
		body, err := artifacts.GetPrivate(home.ArtifactRefPrefix + digest)
		if err != nil {
			t.Fatalf("private %s custody: %v", role, err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != digest {
			t.Fatalf("private %s digest mismatch", role)
		}
	}

	runOMPPromptOnlyConsumerIntegration(t, ompPromptOnlyProducerFixture{
		homePath: canonicalHome, workspace: canonicalWorkspace,
		runtimeRoot: runtimeRoot, projectionPath: filepath.Join(projectionRoot, "omp.json"),
		profileID: p.Profile.ID, requestedModel: p.Profile.RequestedModel, route: p.Profile.Route,
		prompt: "synthetic native producer", producerActor: actor,
		recipeRef: refs[0], proofRef: refs[2], proof: decoded, store: store, now: now,
	})

	t.Run("reservation_loss_prevents_launch", func(t *testing.T) {
		w2, err := store.Create(work.CreateInput{
			Owner:            "omp-producer-test",
			SuccessPredicate: "synthetic reservation loss proof",
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(w2.ID, actor, now); err != nil {
			t.Fatal(err)
		}
		claimed2, err := store.Get(w2.ID)
		if err != nil {
			t.Fatal(err)
		}

		runInput2 := harness.RunInput{
			WorkID:    claimed2.ID,
			Workspace: canonicalWorkspace,
			Prompt:    "reservation loss probe",
			Timeout:   time.Minute,
			Now:       now,
		}
		resolved2, err := adapter.Preflight(context.Background(), runInput2)
		if err != nil {
			t.Fatal(err)
		}

		reserved, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath:    canonicalHome,
			Store:       store,
			ClaimedWork: claimed2,
			Adapter:     adapter,
			RunInput:    resolved2,
			Actor:       actor,
			Target:      "running",
			Note:        "reservation loss probe",
			Now:         now,
		})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := store.Advance(reserved.RunningWork.ID, work.AdvanceInput{
			To:    "verifying",
			Actor: actor,
		}, now); err != nil {
			t.Fatal(err)
		}

		out2, _ := adapter.Run(context.Background(), reserved.RunInput)
		if out2.RunAttempt.Status != runattempt.StatusFailed || out2.RunAttempt.ID == attempt.ID {
			t.Fatalf("stale reservation outcome: %+v", out2.RunAttempt)
		}
		failed, err := attemptStore.Get(out2.RunAttempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != runattempt.StatusFailed || (failed.Launch != nil && failed.Launch.CommittedAt != nil) {
			t.Fatalf("stale reservation launched or was not durably failed: %+v", failed)
		}
		for _, ref := range failed.ProofRefs {
			if strings.HasPrefix(ref, home.ArtifactRefPrefix) {
				t.Fatal("stale reservation produced success proof")
			}
		}
		newer, err := store.Get(w2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if newer.State != "verifying" {
			t.Fatalf("newer generation changed: %s", newer.State)
		}
	})
}

func ompProducerLastActionIntent(w envelope.WorkEnvelope) string {
	if n := len(w.ActionIntentIDs); n == 0 {
		return ""
	}
	return w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
}

func ompProducerFileSHA256Hex(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func ompProducerBinding(t *testing.T, path string) (string, string) {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ompProducerFileSHA256Hex(canonical)
	if err != nil {
		t.Fatal(err)
	}
	return canonical, digest
}
