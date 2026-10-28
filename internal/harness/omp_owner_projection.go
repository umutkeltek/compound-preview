package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

const ompProfileConformanceSchema = "switchback/profile-conformance@1"

type OMPOwnerProjection struct {
	Schema              string `json:"schema"`
	ConformanceRevision string `json:"conformance_revision"`
	LaunchProfileRef    string `json:"launch_profile_ref"`
	Authority           struct {
		Owner        string `json:"owner"`
		CompoundRole string `json:"compound_role"`
		Projection   string `json:"projection"`
	} `json:"authority"`
	Profile struct {
		ID              string `json:"id"`
		Harness         string `json:"harness"`
		HarnessKind     string `json:"harness_kind"`
		ExpectedVersion string `json:"expected_version"`
		ProviderLane    string `json:"provider_lane"`
		Route           string `json:"route"`
		RequestedModel  string `json:"requested_model"`
		RequestModel    string `json:"request_model"`
		RequestedEffort string `json:"requested_effort"`
		CaptureEndpoint string `json:"capture_endpoint"`
		CapturePolicy   struct {
			ID   string `json:"id"`
			Mode string `json:"mode"`
		} `json:"capture_policy"`
	} `json:"profile"`
	PromptOnly struct {
		SourceReady      bool     `json:"source_ready"`
		ArgvContract     string   `json:"argv_contract"`
		FixedArgv        []string `json:"fixed_argv"`
		ToolPosture      string   `json:"tool_posture"`
		DiscoveryPosture string   `json:"discovery_posture"`
		HookPosture      string   `json:"hook_posture"`
		Isolation        struct {
			ConfigRootEnv string `json:"config_root_env"`
			AgentRootEnv  string `json:"agent_root_env"`
			Scope         string `json:"scope"`
		} `json:"isolation"`
		Wrapper struct {
			Path           string `json:"path"`
			SHA256         string `json:"sha256"`
			ExpectedSHA256 string `json:"expected_sha256"`
		} `json:"wrapper"`
		Executable struct {
			Path            string `json:"path"`
			SHA256          string `json:"sha256"`
			ObservedVersion string `json:"observed_version"`
		} `json:"executable"`
		ModelsConfig struct {
			Path           string `json:"path"`
			SHA256         string `json:"sha256"`
			ExpectedSHA256 string `json:"expected_sha256"`
		} `json:"models_config"`
		NativeProof struct {
			Status            string `json:"status"`
			CandidateProofRef string `json:"candidate_proof_ref"`
		} `json:"native_proof"`
	} `json:"prompt_only"`
}

func defaultOMPOwnerProjectionRoot() (string, error) {
	if root := os.Getenv("SWITCHBACK_RUNTIME_ROOT"); root != "" {
		return filepath.Join(root, "state", "profile-conformance"), nil
	}
	if root := os.Getenv("SB_RUNTIME_ROOT"); root != "" {
		return filepath.Join(root, "state", "profile-conformance"), nil
	}
	h, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(h, ".switchback", "state", "profile-conformance"), nil
}
func loadOMPOwnerProjectionFrom(root, id string) (OMPOwnerProjection, error) {
	if id == "" || filepath.Base(id) != id {
		return OMPOwnerProjection{}, fmt.Errorf("omp owner profile id invalid")
	}
	b, e := os.ReadFile(filepath.Join(root, id+".json"))
	if e != nil {
		return OMPOwnerProjection{}, e
	}
	var p OMPOwnerProjection
	if e = json.Unmarshal(b, &p); e != nil {
		return p, e
	}
	// Switchback publishes algorithm-qualified hashes; custody and file-byte
	// checks use bare digests. Normalize only this wire representation here.
	// Validate still rejects malformed, repeated, uppercase, or unknown prefixes.
	for _, digest := range []*string{
		&p.PromptOnly.Wrapper.SHA256, &p.PromptOnly.Wrapper.ExpectedSHA256,
		&p.PromptOnly.ModelsConfig.SHA256, &p.PromptOnly.ModelsConfig.ExpectedSHA256,
		&p.PromptOnly.Executable.SHA256,
	} {
		*digest = strings.TrimPrefix(*digest, "sha256:")
	}
	return p, p.Validate(id)
}
func (p OMPOwnerProjection) Validate(id string) error {
	if p.Schema != ompProfileConformanceSchema || p.Authority.Owner != "switchback" || p.Authority.CompoundRole != "consumer" || p.Authority.Projection != "non_secret_profile_conformance" || p.Profile.ID != id || p.Profile.Harness != "oh_my_pi" || p.Profile.HarnessKind != "omp" {
		return fmt.Errorf("omp projection identity")
	}
	if p.LaunchProfileRef != "switchback://launch-profiles/"+p.Profile.ID {
		return fmt.Errorf("omp projection launch profile ref")
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(p.ConformanceRevision) {
		return fmt.Errorf("omp projection conformance revision")
	}
	for _, v := range []string{p.ConformanceRevision, p.LaunchProfileRef, p.Profile.ExpectedVersion, p.Profile.ProviderLane, p.Profile.Route, p.Profile.RequestedModel, p.Profile.RequestModel, p.Profile.RequestedEffort, p.Profile.CaptureEndpoint, p.Profile.CapturePolicy.ID, p.Profile.CapturePolicy.Mode, p.PromptOnly.ArgvContract, p.PromptOnly.ToolPosture, p.PromptOnly.DiscoveryPosture, p.PromptOnly.HookPosture, p.PromptOnly.Wrapper.Path, p.PromptOnly.Wrapper.SHA256, p.PromptOnly.Wrapper.ExpectedSHA256, p.PromptOnly.Executable.Path, p.PromptOnly.Executable.SHA256, p.PromptOnly.Executable.ObservedVersion, p.PromptOnly.ModelsConfig.Path, p.PromptOnly.ModelsConfig.SHA256, p.PromptOnly.ModelsConfig.ExpectedSHA256} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("omp projection incomplete")
		}
	}
	if !p.PromptOnly.SourceReady || p.PromptOnly.ArgvContract != "omp-prompt-only@1" || p.PromptOnly.ToolPosture != "no_tools_required" || p.PromptOnly.DiscoveryPosture != "no_extensions_no_skills_no_rules_no_prewalk" || p.PromptOnly.HookPosture != "disabled" || p.PromptOnly.NativeProof.Status != "unverified_candidate" {
		return fmt.Errorf("omp projection prompt-only posture")
	}
	if p.PromptOnly.Isolation.ConfigRootEnv != "PI_CONFIG_DIR" || p.PromptOnly.Isolation.AgentRootEnv != "PI_CODING_AGENT_DIR" || p.PromptOnly.Isolation.Scope != "fresh_same_per_run_root" {
		return fmt.Errorf("omp projection isolation posture")
	}
	if p.Profile.RequestedEffort != "high" {
		return fmt.Errorf("omp projection unsupported effort")
	}
	wantArgv := []string{
		"--cwd=<workspace>",
		"--provider=switchback",
		"--model=switchback/" + p.Profile.RequestModel,
		"--mode=text",
		"--print",
		"--no-session",
		"--prompt-only",
		"--no-tools",
		"--no-extensions",
		"--no-skills",
		"--no-rules",
		"--no-prewalk",
		"--approval-mode=always-ask",
		"--thinking=" + p.Profile.RequestedEffort,
		"--",
		"<prompt>",
	}
	if !reflect.DeepEqual(p.PromptOnly.FixedArgv, wantArgv) {
		return fmt.Errorf("omp projection fixed argv")
	}
	// RequestedModel is the owner's alias, not the canonical RequestModel bound
	// by the fixed argv, native request, and custody replay.
	if p.PromptOnly.Wrapper.SHA256 != p.PromptOnly.Wrapper.ExpectedSHA256 || p.PromptOnly.ModelsConfig.SHA256 != p.PromptOnly.ModelsConfig.ExpectedSHA256 {
		return fmt.Errorf("omp projection expected hash mismatch")
	}
	for name, digest := range map[string]string{
		"wrapper":       p.PromptOnly.Wrapper.SHA256,
		"wrapper want":  p.PromptOnly.Wrapper.ExpectedSHA256,
		"models config": p.PromptOnly.ModelsConfig.SHA256,
		"config want":   p.PromptOnly.ModelsConfig.ExpectedSHA256,
		"executable":    p.PromptOnly.Executable.SHA256,
	} {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
			return fmt.Errorf("omp projection %s digest", name)
		}
	}
	if p.PromptOnly.Executable.ObservedVersion != "18.1.17" || p.Profile.ExpectedVersion != "18.1.17" {
		return fmt.Errorf("omp projection native version")
	}
	for _, path := range []string{p.PromptOnly.Wrapper.Path, p.PromptOnly.Executable.Path, p.PromptOnly.ModelsConfig.Path} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("omp projection noncanonical path")
		}
	}
	if filepath.ToSlash(p.PromptOnly.ModelsConfig.Path) == "" || !strings.HasSuffix(filepath.ToSlash(p.PromptOnly.ModelsConfig.Path), "/agent/models.yml") {
		return fmt.Errorf("omp projection models config path")
	}
	return nil
}
func ompFileSHA256(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("nonabsolute")
	}
	i, e := os.Lstat(path)
	if e != nil || !i.Mode().IsRegular() {
		return "", fmt.Errorf("unsafe projection file")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return "", e
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}
