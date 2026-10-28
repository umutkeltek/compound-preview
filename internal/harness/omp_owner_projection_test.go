package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validOMPOwnerProjection(t *testing.T) OMPOwnerProjection {
	t.Helper()
	var p OMPOwnerProjection
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

func TestOMPOwnerProjectionAcceptsExactOwnerContract(t *testing.T) {
	p := validOMPOwnerProjection(t)
	if err := p.Validate("omp-test"); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "omp-test.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOMPOwnerProjectionFrom(root, "omp-test"); err != nil {
		t.Fatal(err)
	}
}

func TestOMPOwnerProjectionRejectsLooseOrUnreadyOwnerClaims(t *testing.T) {
	for name, mutate := range map[string]func(*OMPOwnerProjection){
		"source not ready":    func(p *OMPOwnerProjection) { p.PromptOnly.SourceReady = false },
		"wrong projection":    func(p *OMPOwnerProjection) { p.Authority.Projection = "secret" },
		"unpinned launch ref": func(p *OMPOwnerProjection) { p.LaunchProfileRef = "switchback://launch-profiles/other" },
		"loose revision":      func(p *OMPOwnerProjection) { p.ConformanceRevision = "sha256:x" },
		"loose argv contract": func(p *OMPOwnerProjection) { p.PromptOnly.ArgvContract = "six" },
		"partial argv":        func(p *OMPOwnerProjection) { p.PromptOnly.FixedArgv = []string{"-p"} },
		"wrong model argv":    func(p *OMPOwnerProjection) { p.PromptOnly.FixedArgv[2] = "--model=switchback/other" },
		"wrong effort argv":   func(p *OMPOwnerProjection) { p.PromptOnly.FixedArgv[13] = "--thinking=low" },
		"unsupported effort":  func(p *OMPOwnerProjection) { p.Profile.RequestedEffort = "low" },
		"tools allowed":       func(p *OMPOwnerProjection) { p.PromptOnly.ToolPosture = "none" },
		"discovery allowed":   func(p *OMPOwnerProjection) { p.PromptOnly.DiscoveryPosture = "none" },
		"hooks allowed":       func(p *OMPOwnerProjection) { p.PromptOnly.HookPosture = "none" },
		"wrong config root":   func(p *OMPOwnerProjection) { p.PromptOnly.Isolation.ConfigRootEnv = "HOME" },
		"wrong agent root":    func(p *OMPOwnerProjection) { p.PromptOnly.Isolation.AgentRootEnv = "HOME" },
		"shared run root":     func(p *OMPOwnerProjection) { p.PromptOnly.Isolation.Scope = "shared" },
		"verified candidate":  func(p *OMPOwnerProjection) { p.PromptOnly.NativeProof.Status = "verified" },
		"short wrapper digest": func(p *OMPOwnerProjection) {
			p.PromptOnly.Wrapper.SHA256, p.PromptOnly.Wrapper.ExpectedSHA256 = "a", "a"
		},
		"uppercase wrapper digest": func(p *OMPOwnerProjection) {
			p.PromptOnly.Wrapper.SHA256, p.PromptOnly.Wrapper.ExpectedSHA256 = strings.Repeat("A", 64), strings.Repeat("A", 64)
		},
		"short config digest": func(p *OMPOwnerProjection) {
			p.PromptOnly.ModelsConfig.SHA256, p.PromptOnly.ModelsConfig.ExpectedSHA256 = "b", "b"
		},
		"short executable digest": func(p *OMPOwnerProjection) { p.PromptOnly.Executable.SHA256 = "c" },
		"wrong native version":    func(p *OMPOwnerProjection) { p.PromptOnly.Executable.ObservedVersion = "18" },
		"wrong config path":       func(p *OMPOwnerProjection) { p.PromptOnly.ModelsConfig.Path = "/fixture/models.yml" },
	} {
		t.Run(name, func(t *testing.T) {
			p := validOMPOwnerProjection(t)
			mutate(&p)
			if err := p.Validate("omp-test"); err == nil {
				t.Fatal("Validate accepted loose or unready owner projection")
			}
		})
	}
}

func TestOMPOwnerProjection18117Contract(t *testing.T) {
	current := func() OMPOwnerProjection {
		p := validOMPOwnerProjection(t)
		p.Profile.ExpectedVersion = "18.1.17"
		p.PromptOnly.Executable.ObservedVersion = "18.1.17"
		p.PromptOnly.FixedArgv[6], p.PromptOnly.FixedArgv[7] = "--prompt-only", "--no-tools"
		return p
	}
	t.Run("exact_current_owner", func(t *testing.T) {
		if err := current().Validate("omp-test"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("obsolete_version", func(t *testing.T) {
		p := current()
		p.Profile.ExpectedVersion, p.PromptOnly.Executable.ObservedVersion = "18.1.11", "18.1.11"
		if err := p.Validate("omp-test"); err == nil {
			t.Fatal("accepted obsolete native version")
		}
	})
	t.Run("obsolete_argv_order", func(t *testing.T) {
		p := current()
		p.PromptOnly.FixedArgv[6], p.PromptOnly.FixedArgv[7] = "--no-tools", "--prompt-only"
		if err := p.Validate("omp-test"); err == nil {
			t.Fatal("accepted obsolete native argv order")
		}
	})
}

func TestDefaultOMPOwnerProjectionRootHonorsSwitchbackRuntimePrecedence(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "explicit")
	legacy := filepath.Join(t.TempDir(), "legacy")
	t.Setenv("SWITCHBACK_RUNTIME_ROOT", explicit)
	t.Setenv("SB_RUNTIME_ROOT", legacy)
	got, err := defaultOMPOwnerProjectionRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(explicit, "state", "profile-conformance"); got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}

	t.Setenv("SWITCHBACK_RUNTIME_ROOT", "")
	got, err = defaultOMPOwnerProjectionRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(legacy, "state", "profile-conformance"); got != want {
		t.Fatalf("legacy root = %q, want %q", got, want)
	}
}
