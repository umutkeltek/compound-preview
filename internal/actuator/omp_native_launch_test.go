package actuator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/runattempt"
)

func TestPrepareGovernedHarnessLaunchMarksOMPNativeVerifierPromptOnly(t *testing.T) {
	wrapper := installOMPNativeLaunchOwnerFixture(t)
	home := t.TempDir()
	adapter := harness.NewOMPNativeVerifierAdapter(runattempt.Open(home))
	in := GovernedHarnessLaunchInput{
		HomePath: home, Adapter: adapter,
		RunningWork:    envelope.WorkEnvelope{Header: envelope.Header{IDs: envelope.IDs{WorkID: "work_omp_native"}}, PolicyDecisionID: "pol_omp_native"},
		ActionIntentID: "act_omp_native", Now: time.Now().UTC(),
	}
	run := harness.RunInput{WorkID: "work_omp_native", Workspace: t.TempDir(), Prompt: "prove prompt only", Now: time.Now().UTC()}
	resolved, err := adapter.Preflight(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	in.RunInput = resolved
	out, err := PrepareGovernedHarnessLaunch(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExecutionPosture == nil || out.ExecutionPosture.PermissionMode != "prompt_only" || out.ExecutionPosture.PolicyDecisionID != "pol_omp_native" {
		t.Fatalf("native execution posture = %+v", out.ExecutionPosture)
	}
	if out.StartGuard == nil || len(out.ExecutionPosture.AuthorityEvidence) != 1 || out.ExecutionPosture.AuthorityEvidence[0].Kind != "omp_native_verifier" {
		t.Fatalf("native launch did not retain reservation guard and marker: %+v", out)
	}
	t.Run("bound_wrapper_drift_remains_rejected", func(t *testing.T) {
		if err := os.WriteFile(wrapper, []byte("changed synthetic wrapper"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Preflight(context.Background(), resolved); err == nil || !strings.Contains(err.Error(), "OMP_LAUNCH_BINDING_INVALID") {
			t.Fatalf("bound wrapper drift must fail native preflight: %v", err)
		}
	})

}

func installOMPNativeLaunchOwnerFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	t.Setenv("SWITCHBACK_RUNTIME_ROOT", runtimeRoot)
	p := validOMPNativeLaunchOwnerProjection(t)
	p.Profile.ID = "omp"
	p.LaunchProfileRef = "switchback://launch-profiles/omp"
	writeBinding := func(name string) (string, string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		body := []byte("synthetic launch fixture: " + name + "\n")
		if err := os.WriteFile(path, body, 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return canonical, hex.EncodeToString(sum[:])
	}
	p.PromptOnly.Wrapper.Path, p.PromptOnly.Wrapper.SHA256 = writeBinding("wrapper")
	p.PromptOnly.Wrapper.ExpectedSHA256 = p.PromptOnly.Wrapper.SHA256
	p.PromptOnly.Executable.Path, p.PromptOnly.Executable.SHA256 = writeBinding("bin/omp")
	p.PromptOnly.ModelsConfig.Path, p.PromptOnly.ModelsConfig.SHA256 = writeBinding("agent/models.yml")
	p.PromptOnly.ModelsConfig.ExpectedSHA256 = p.PromptOnly.ModelsConfig.SHA256
	if err := p.Validate("omp"); err != nil {
		t.Fatalf("owner fixture must satisfy the current projection contract: %v", err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	projectionRoot := filepath.Join(runtimeRoot, "state", "profile-conformance")
	if err := os.MkdirAll(projectionRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectionRoot, "omp.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	return p.PromptOnly.Wrapper.Path
}

func validOMPNativeLaunchOwnerProjection(t *testing.T) harness.OMPOwnerProjection {
	t.Helper()
	var p harness.OMPOwnerProjection
	digest := strings.Repeat("a", 64)
	body := `{
		"schema":"switchback/profile-conformance@1",
		"conformance_revision":"sha256:` + digest + `",
		"launch_profile_ref":"switchback://launch-profiles/omp-test",
		"authority":{"owner":"switchback","compound_role":"consumer","projection":"non_secret_profile_conformance"},
		"profile":{"id":"omp-test","harness":"oh_my_pi","harness_kind":"omp","expected_version":"18.1.17","provider_lane":"omp-lane","route":"omp","requested_model":"test/omp-model","request_model":"test/omp-model","requested_effort":"high","capture_endpoint":"http://fixture.invalid","capture_policy":{"id":"wire","mode":"wire"}},
		"prompt_only":{"source_ready":true,"argv_contract":"omp-prompt-only@1","fixed_argv":["--cwd=<workspace>","--provider=switchback","--model=switchback/test/omp-model","--mode=text","--print","--no-session","--prompt-only","--no-tools","--no-extensions","--no-skills","--no-rules","--no-prewalk","--approval-mode=always-ask","--thinking=high","--","<prompt>"],"tool_posture":"no_tools_required","discovery_posture":"no_extensions_no_skills_no_rules_no_prewalk","hook_posture":"disabled","isolation":{"config_root_env":"PI_CONFIG_DIR","agent_root_env":"PI_CODING_AGENT_DIR","scope":"fresh_same_per_run_root"},"wrapper":{"path":"/fixture/wrapper","sha256":"` + digest + `","expected_sha256":"` + digest + `"},"executable":{"path":"/fixture/omp","sha256":"` + digest + `","observed_version":"18.1.17"},"models_config":{"path":"/fixture/agent/models.yml","sha256":"` + digest + `","expected_sha256":"` + digest + `"},"native_proof":{"status":"unverified_candidate","candidate_proof_ref":"candidate-only"}}
	}`
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
