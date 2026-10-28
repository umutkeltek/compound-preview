package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"compound/internal/runattempt"
)

const ompPromptOnlyAdapterName = "omp-prompt-only"

var ompPromptOnlyCredentialPattern = regexp.MustCompile(
	`(?m)^export SB_LAUNCH_CREDENTIAL_ENV='([A-Z][A-Z0-9_]*)'\r?$`,
)

var ompPromptOnlyPostureEvidence = []runattempt.ExecutionPostureEvidence{{
	Kind: "omp_prompt_only", Ref: "harness:omp-prompt-only", Revision: "v0.1",
}}

type ompPromptOnlyLaunch struct {
	ProjectionRoot     string
	Projection         OMPOwnerProjection
	WorkID             string
	Workspace          string
	Prompt             string
	OwnerHome          string
	RuntimeRoot        string
	TempRoot           string
	NativeCommandPath  string
	NativeSearchDir    string
	CredentialEnv      string
	CredentialExported bool
	Environment        []string
}

// OMPPromptOnlyAdapter is the governed consumer of the Switchback-owned
// prompt-only wrapper. The wrapper, not the ordinary pi-family adapter, owns
// inner argv, credentials, isolation, and no-tools posture.
type OMPPromptOnlyAdapter struct {
	store                *runattempt.Store
	heartbeatInterval    time.Duration
	projectionRoot       string
	profileID            string
	ownerHome            string
	runtimeRoot          string
	executableSearchPath string
}

func NewOMPPromptOnlyAdapter(store *runattempt.Store) *OMPPromptOnlyAdapter {
	return &OMPPromptOnlyAdapter{
		store: store, heartbeatInterval: defaultHeartbeatInterval, profileID: "omp",
	}
}

func (a *OMPPromptOnlyAdapter) Name() string { return ompPromptOnlyAdapterName }

func (a *OMPPromptOnlyAdapter) Preflight(_ context.Context, in RunInput) (RunInput, error) {
	if a == nil || a.store == nil {
		return RunInput{}, fmt.Errorf("omp prompt-only: runattempt store required")
	}
	prepared := in.ompPromptOnly != nil
	if prepared {
		if err := ValidateOMPPromptOnlyPreparedInput(in); err != nil {
			return RunInput{}, err
		}
	} else {
		if in.Harness != "" && in.Harness != a.Name() {
			return RunInput{}, fmt.Errorf("omp prompt-only: caller harness override is forbidden")
		}
		if strings.TrimSpace(in.Command) != "" || len(in.Args) != 0 ||
			len(in.Env) != 0 || strings.TrimSpace(in.PermissionMode) != "" ||
			in.Stdin != nil {
			return RunInput{}, fmt.Errorf("omp prompt-only: caller execution overrides are forbidden")
		}
	}
	if strings.TrimSpace(in.WorkID) == "" || strings.TrimSpace(in.Workspace) == "" ||
		in.Now.IsZero() {
		return RunInput{}, fmt.Errorf("omp prompt-only: work_id, workspace, and now are required")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" || strings.HasPrefix(prompt, "-") || strings.HasPrefix(prompt, "@") {
		return RunInput{}, fmt.Errorf("omp prompt-only: plain nonempty prompt required")
	}
	workspace, err := canonicalOMPPromptOnlyDirectory(in.Workspace)
	if err != nil {
		return RunInput{}, fmt.Errorf("omp prompt-only: OMP_WORKSPACE_INVALID")
	}
	launch, err := a.resolveLaunch(in, workspace, prompt)
	if err != nil {
		return RunInput{}, fmt.Errorf("omp prompt-only: OMP_LAUNCH_BINDING_INVALID")
	}
	in.Harness, in.Command = a.Name(), launch.Projection.PromptOnly.Wrapper.Path
	in.Workspace, in.Prompt = workspace, prompt
	in.Args, in.Env, in.Stdin = nil, nil, nil
	in.PermissionMode, in.ompPromptOnly = "prompt_only", launch
	in.LaunchProfileID = launch.Projection.Profile.ID
	return in, nil
}

func ValidateOMPPromptOnlyPreparedInput(in RunInput) error {
	launch := in.ompPromptOnly
	if launch == nil || in.LaunchProfileID != launch.Projection.Profile.ID || in.Harness != ompPromptOnlyAdapterName ||
		in.Command != launch.Projection.PromptOnly.Wrapper.Path ||
		in.WorkID != launch.WorkID || in.Workspace != launch.Workspace ||
		in.Prompt != launch.Prompt || len(in.Args) != 0 || len(in.Env) != 0 ||
		in.Stdin != nil || in.PermissionMode != "prompt_only" {
		return fmt.Errorf("OMP_PROMPT_ONLY_PREPARED_INPUT_INVALID")
	}
	if err := validateOMPPromptOnlyLaunch(launch); err != nil {
		return fmt.Errorf("OMP_PROMPT_ONLY_LAUNCH_BINDING_INVALID")
	}
	return nil
}

// ValidateOMPPromptOnlyPreparedProof binds custody to the exact projection
// root and owner projection frozen during this consumer's preflight.
func ValidateOMPPromptOnlyPreparedProof(home string, in RunInput, proof OMPNativePublicProof) error {
	if err := ValidateOMPPromptOnlyPreparedInput(in); err != nil {
		return fmt.Errorf("OMP_PROMPT_ONLY_PROOF_CUSTODY_INVALID")
	}
	current, err := validateOMPNativeProofCustodyAtRoot(
		home, in.ompPromptOnly.ProjectionRoot, proof,
	)
	if err != nil || !reflect.DeepEqual(current, in.ompPromptOnly.Projection) {
		return fmt.Errorf("OMP_PROMPT_ONLY_PROOF_CUSTODY_INVALID")
	}
	return nil
}

func (a *OMPPromptOnlyAdapter) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	prepared, err := a.Preflight(ctx, in)
	if err != nil {
		return RunOutput{}, err
	}
	if err := validateGovernedOMPPromptOnlyInput(prepared); err != nil {
		return RunOutput{}, err
	}
	outerGuard, expected := prepared.StartGuard, prepared.ompPromptOnly
	prepared.StartGuard = func(ctx context.Context, start func() error) error {
		return outerGuard(ctx, func() error {
			if err := a.revalidateLaunch(expected); err != nil {
				return err
			}
			return start()
		})
	}
	return runWithSubprocess(
		ctx, a.store, a.Name(), prepared, a.buildCmd, a.heartbeatInterval,
	)
}

func validateGovernedOMPPromptOnlyInput(in RunInput) error {
	if strings.TrimSpace(in.ActionIntentID) == "" || in.StartGuard == nil {
		return fmt.Errorf("omp prompt-only: governed action and start guard are required")
	}
	if in.ExecutionPosture == nil ||
		in.ExecutionPosture.PermissionMode != "prompt_only" ||
		!strings.HasPrefix(strings.TrimSpace(in.ExecutionPosture.PolicyDecisionID), "pol_") ||
		!reflect.DeepEqual(in.ExecutionPosture.AuthorityEvidence, ompPromptOnlyPostureEvidence) {
		return fmt.Errorf("omp prompt-only: governed prompt_only posture is required")
	}
	return nil
}

func (a *OMPPromptOnlyAdapter) buildCmd(ctx context.Context, in RunInput) *exec.Cmd {
	launch := in.ompPromptOnly
	cmd := exec.CommandContext(ctx, launch.Projection.PromptOnly.Wrapper.Path,
		"-p", "--no-session", "--cwd", launch.Workspace, "--no-tools", launch.Prompt)
	cmd.Dir = launch.Workspace
	cmd.Env = append([]string(nil), launch.Environment...)
	return cmd
}

func (a *OMPPromptOnlyAdapter) resolveLaunch(
	in RunInput, workspace, prompt string,
) (*ompPromptOnlyLaunch, error) {
	if in.ompPromptOnly != nil {
		if err := validateOMPPromptOnlyLaunch(in.ompPromptOnly); err != nil {
			return nil, err
		}
		return in.ompPromptOnly, nil
	}
	root, err := a.currentProjectionRoot()
	if err != nil {
		return nil, err
	}
	profileID := in.LaunchProfileID
	if profileID == "" {
		profileID = a.profileID
	}
	if profileID == "" {
		profileID = "omp"
	}
	projection, err := loadOMPOwnerProjectionFrom(root, profileID)
	if err != nil {
		return nil, err
	}
	ownerHome := a.ownerHome
	if ownerHome == "" {
		ownerHome, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	ownerHome, err = canonicalOMPPromptOnlyDirectory(ownerHome)
	if err != nil {
		return nil, err
	}
	runtimeRoot := a.runtimeRoot
	if runtimeRoot == "" {
		runtimeRoot = strings.TrimSpace(os.Getenv("SWITCHBACK_RUNTIME_ROOT"))
	}
	if runtimeRoot == "" {
		runtimeRoot = strings.TrimSpace(os.Getenv("SB_RUNTIME_ROOT"))
	}
	if runtimeRoot == "" {
		runtimeRoot = filepath.Join(ownerHome, ".switchback")
	}
	runtimeRoot, err = canonicalOMPPromptOnlyDirectory(runtimeRoot)
	if err != nil {
		return nil, err
	}
	tempRoot, err := canonicalOMPPromptOnlyDirectory(os.TempDir())
	if err != nil {
		return nil, err
	}
	searchPath := a.executableSearchPath
	if searchPath == "" {
		searchPath = os.Getenv("PATH")
	}
	nativeCommand, nativeDir, err := resolveOMPPromptOnlyNative(
		searchPath, projection.PromptOnly.Executable.Path,
		projection.PromptOnly.Executable.SHA256,
	)
	if err != nil {
		return nil, err
	}
	safePath := buildOMPPromptOnlyPath(
		nativeDir, filepath.Dir(projection.PromptOnly.Executable.Path),
	)
	commandCheck, dirCheck, err := resolveOMPPromptOnlyNative(
		safePath, projection.PromptOnly.Executable.Path,
		projection.PromptOnly.Executable.SHA256,
	)
	if err != nil || commandCheck != nativeCommand || dirCheck != nativeDir {
		return nil, fmt.Errorf("OMP_NATIVE_EXECUTABLE_INVALID")
	}
	credentialEnv, err := ompPromptOnlyCredentialEnv(projection.PromptOnly.Wrapper.Path)
	if err != nil {
		return nil, err
	}
	environment := []string{
		"HOME=" + ownerHome,
		"PATH=" + safePath,
		"TMPDIR=" + tempRoot,
		"LANG=C",
		"LC_ALL=C",
		"NO_COLOR=1",
		"SWITCHBACK_RUNTIME_ROOT=" + runtimeRoot,
	}
	credentialExported := false
	if value, ok := os.LookupEnv(credentialEnv); ok {
		environment = append(environment, credentialEnv+"="+value)
		credentialExported = true
	}
	launch := &ompPromptOnlyLaunch{
		ProjectionRoot: root, Projection: projection, WorkID: in.WorkID,
		Workspace: workspace, Prompt: prompt, OwnerHome: ownerHome,
		RuntimeRoot: runtimeRoot, TempRoot: tempRoot,
		NativeCommandPath: nativeCommand, NativeSearchDir: nativeDir,
		CredentialEnv: credentialEnv, CredentialExported: credentialExported,
		Environment: environment,
	}
	if err := validateOMPPromptOnlyLaunch(launch); err != nil {
		return nil, err
	}
	return launch, nil
}

func (a *OMPPromptOnlyAdapter) currentProjectionRoot() (string, error) {
	root := a.projectionRoot
	var err error
	if root == "" {
		root, err = defaultOMPOwnerProjectionRoot()
		if err != nil {
			return "", err
		}
	}
	return canonicalOMPPromptOnlyDirectory(root)
}

func (a *OMPPromptOnlyAdapter) revalidateLaunch(expected *ompPromptOnlyLaunch) error {
	if expected == nil {
		return fmt.Errorf("OMP_PROMPT_ONLY_LAUNCH_BINDING_INVALID")
	}
	current, err := a.resolveLaunch(
		RunInput{WorkID: expected.WorkID, LaunchProfileID: expected.Projection.Profile.ID}, expected.Workspace, expected.Prompt,
	)
	if err != nil || !reflect.DeepEqual(current, expected) {
		return fmt.Errorf("OMP_PROMPT_ONLY_LAUNCH_BINDING_INVALID")
	}
	return nil
}

func validateOMPPromptOnlyLaunch(launch *ompPromptOnlyLaunch) error {
	if launch == nil || launch.Projection.Validate(launch.Projection.Profile.ID) != nil {
		return fmt.Errorf("OMP_PROMPT_ONLY_LAUNCH_BINDING_INVALID")
	}
	for _, path := range []string{
		launch.ProjectionRoot, launch.OwnerHome, launch.RuntimeRoot,
		launch.TempRoot, launch.Workspace,
	} {
		canonical, err := canonicalOMPPromptOnlyDirectory(path)
		if err != nil || canonical != path {
			return fmt.Errorf("OMP_PROMPT_ONLY_PATH_INVALID")
		}
	}
	if strings.TrimSpace(launch.WorkID) == "" || launch.Prompt == "" ||
		launch.Prompt != strings.TrimSpace(launch.Prompt) {
		return fmt.Errorf("OMP_PROMPT_ONLY_INPUT_INVALID")
	}
	for _, binding := range []struct{ path, digest string }{
		{launch.Projection.PromptOnly.Wrapper.Path, launch.Projection.PromptOnly.Wrapper.SHA256},
		{launch.Projection.PromptOnly.ModelsConfig.Path, launch.Projection.PromptOnly.ModelsConfig.SHA256},
		{launch.Projection.PromptOnly.Executable.Path, launch.Projection.PromptOnly.Executable.SHA256},
	} {
		canonical, err := filepath.EvalSymlinks(binding.path)
		if err != nil || canonical != binding.path {
			return fmt.Errorf("OMP_PROMPT_ONLY_BINDING_INVALID")
		}
		digest, err := ompFileSHA256(binding.path)
		if err != nil || digest != binding.digest {
			return fmt.Errorf("OMP_PROMPT_ONLY_BINDING_DRIFT")
		}
	}
	credentialEnv, err := ompPromptOnlyCredentialEnv(
		launch.Projection.PromptOnly.Wrapper.Path,
	)
	if err != nil || credentialEnv != launch.CredentialEnv {
		return fmt.Errorf("OMP_PROMPT_ONLY_CREDENTIAL_BINDING_INVALID")
	}
	values, err := ompPromptOnlyEnvironmentValues(launch.Environment)
	if err != nil {
		return err
	}
	if values["HOME"] != launch.OwnerHome || values["TMPDIR"] != launch.TempRoot ||
		values["LANG"] != "C" || values["LC_ALL"] != "C" ||
		values["NO_COLOR"] != "1" ||
		values["SWITCHBACK_RUNTIME_ROOT"] != launch.RuntimeRoot {
		return fmt.Errorf("OMP_PROMPT_ONLY_ENVIRONMENT_INVALID")
	}
	_, hasCredential := values[launch.CredentialEnv]
	wantCount := 7
	if launch.CredentialExported {
		wantCount++
	}
	if hasCredential != launch.CredentialExported || len(values) != wantCount {
		return fmt.Errorf("OMP_PROMPT_ONLY_ENVIRONMENT_INVALID")
	}
	command, dir, err := resolveOMPPromptOnlyNative(
		values["PATH"], launch.Projection.PromptOnly.Executable.Path,
		launch.Projection.PromptOnly.Executable.SHA256,
	)
	if err != nil || command != launch.NativeCommandPath || dir != launch.NativeSearchDir {
		return fmt.Errorf("OMP_NATIVE_EXECUTABLE_INVALID")
	}
	return nil
}

func canonicalOMPPromptOnlyDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) ||
		strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("invalid directory")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("invalid directory")
	}
	return canonical, nil
}

func resolveOMPPromptOnlyNative(
	pathValue, expectedPath, expectedSHA256 string,
) (string, string, error) {
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		dir, err := filepath.EvalSymlinks(filepath.Clean(dir))
		if err != nil {
			continue
		}
		candidate := filepath.Join(dir, "omp")
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		canonical, err := filepath.EvalSymlinks(candidate)
		if err != nil || canonical != expectedPath {
			continue
		}
		digest, err := ompFileSHA256(canonical)
		if err == nil && digest == expectedSHA256 {
			return candidate, dir, nil
		}
	}
	return "", "", fmt.Errorf("OMP_NATIVE_EXECUTABLE_INVALID")
}

func buildOMPPromptOnlyPath(nativeDir, executableDir string) string {
	candidates := []string{
		nativeDir, executableDir, "/opt/homebrew/bin", "/usr/local/bin",
		"/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, dir := range candidates {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(filepath.Clean(dir))
		if err != nil || seen[canonical] {
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return strings.Join(out, string(os.PathListSeparator))
}

func ompPromptOnlyCredentialEnv(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("OMP_PROMPT_ONLY_WRAPPER_INVALID")
	}
	matches := ompPromptOnlyCredentialPattern.FindAllSubmatch(body, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("OMP_PROMPT_ONLY_CREDENTIAL_BINDING_INVALID")
	}
	name := string(matches[0][1])
	switch name {
	case "HOME", "PATH", "TMPDIR", "LANG", "LC_ALL", "NO_COLOR",
		"SWITCHBACK_RUNTIME_ROOT", "SB_RUNTIME_ROOT", "PI_CONFIG_DIR",
		"PI_CODING_AGENT_DIR", "PI_PROFILE", "OMP_PROFILE",
		"NODE_OPTIONS", "BUN_OPTIONS":
		return "", fmt.Errorf("OMP_PROMPT_ONLY_CREDENTIAL_BINDING_INVALID")
	}
	if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return "", fmt.Errorf("OMP_PROMPT_ONLY_CREDENTIAL_BINDING_INVALID")
	}
	return name, nil
}

func ompPromptOnlyEnvironmentValues(env []string) (map[string]string, error) {
	values := make(map[string]string, len(env))
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		if !ok || name == "" || strings.ContainsAny(name, "\x00\r\n") {
			return nil, fmt.Errorf("OMP_PROMPT_ONLY_ENVIRONMENT_INVALID")
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("OMP_PROMPT_ONLY_ENVIRONMENT_INVALID")
		}
		values[name] = value
	}
	return values, nil
}
