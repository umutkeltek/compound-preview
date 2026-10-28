package harness

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"

	"compound/internal/routeevidence"
	"compound/internal/runattempt"
)

const openCodePromptLimit = 64 << 10
const openCodeTranscriptLimit = 64 << 10
const openCodeNativeGitignore = "node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore\n"

// openCodeSupportedEffort is the only requested effort value the opencode
// adapter carries through today: explicitly "low", which the adapter places
// as provider.models[model].options.reasoningEffort='low' in the immutable
// settings file before the subprocess reads it. Any other value — including
// high/medium/xhigh, "none", and whitespace-padded variants — is refused
// before mint; there is no normalization and no provider/model default.
//
// The empty default is NOT refused. It stays compatible: it leaves the config,
// and therefore the wire, with no options key and no reasoning_effort at all.
const openCodeSupportedEffort = "low"

// openCodeEffortBinding is the exact effort state frozen onto an immutable
// launch at preflight. All three legs are recorded, so afterwards a raised
// request, a swapped applied value, a rewritten evidence claim, and a snapshot
// dropped or replaced wholesale all read as the same drift rather than as a
// fresh instruction — checked on preflight reuse and again immediately before
// the native subprocess starts.
type openCodeEffortBinding struct {
	Requested string
	Applied   string
	Evidence  string
	Bound     bool // a selection snapshot was present when this launch was built
}

// openCodeEffortBindingOf reads the effort legs of a selection snapshot. A nil
// snapshot requests nothing and binds nothing.
func openCodeEffortBindingOf(snap *runattempt.ExecutionSelectionSnapshot) openCodeEffortBinding {
	if snap == nil {
		return openCodeEffortBinding{}
	}
	return openCodeEffortBinding{Requested: snap.RequestedEffort, Applied: snap.AppliedEffort, Evidence: snap.EffortEvidence, Bound: true}
}

var openCodeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

type openCodeLaunch struct {
	WorkID, Workspace, Prompt, Model, Provider, Endpoint, Revision, Profile string
	Effort                                                                  string
	EffortBinding                                                           openCodeEffortBinding
	Command, CommandSHA, AuthPath, Root                                     string
	Config                                                                  []byte
}

// OpenCodeAdapter executes one native prompt-only run. It never exposes the
// caller's workspace, environment, config, tools, or session to native search.
type OpenCodeAdapter struct {
	store             *runattempt.Store
	heartbeatInterval time.Duration
	// Internal test seams; production has no caller override door.
	authPath, sandboxCommand, testCAPath string
	testNetworkPort                      int
	removeRoot                           func(string) error
}

func NewOpenCodeAdapter(store *runattempt.Store) *OpenCodeAdapter {
	return &OpenCodeAdapter{store: store, heartbeatInterval: defaultHeartbeatInterval}
}
func (a *OpenCodeAdapter) Name() string { return "opencode" }

func (a *OpenCodeAdapter) Preflight(_ context.Context, in RunInput) (RunInput, error) {
	if a == nil || a.store == nil || runtime.GOOS != "darwin" {
		return RunInput{}, errors.New("OPENCODE_PLATFORM_UNAVAILABLE")
	}
	if in.opencodeLaunch != nil {
		if err := a.validatePrepared(in); err != nil {
			return RunInput{}, err
		}
		return in, nil
	}
	// The work wire already owns a selection snapshot. Some native adapters
	// bind it themselves; only Codex currently receives duplicated scalar fields.
	if snap := in.ExecutionSelectionSnapshot; snap != nil && snap.RequestedModelID != "" {
		if snap.Harness != a.Name() || snap.ProviderRoute == "" || snap.LaunchProfileRevision == "" || (in.RequestedModelID != "" && in.RequestedModelID != snap.RequestedModelID) || (in.RequestedProviderRoute != "" && in.RequestedProviderRoute != snap.ProviderRoute) || (in.ExpectedModelRouterRevision != "" && in.ExpectedModelRouterRevision != snap.LaunchProfileRevision) {
			return RunInput{}, errors.New("OPENCODE_SELECTION_MISMATCH")
		}
		in.RequestedModelID, in.RequestedProviderRoute, in.ExpectedModelRouterRevision = snap.RequestedModelID, snap.ProviderRoute, snap.LaunchProfileRevision
	}
	if (in.Harness != "" && in.Harness != a.Name()) || len(in.Args) != 0 || len(in.Env) != 0 || in.Stdin != nil || in.PermissionMode != "" || (in.LaunchProfileID != "" && in.LaunchProfileID != in.RequestedModelID) {
		return RunInput{}, errors.New("OPENCODE_CALLER_OVERRIDE_FORBIDDEN")
	}
	if strings.TrimSpace(in.WorkID) == "" || in.Now.IsZero() || strings.TrimSpace(in.Prompt) == "" || len(in.Prompt) > openCodePromptLimit || strings.ContainsRune(in.Prompt, '\x00') {
		return RunInput{}, errors.New("OPENCODE_INPUT_INVALID")
	}
	workspace, err := canonicalOMPPromptOnlyDirectory(in.Workspace)
	if err != nil {
		return RunInput{}, errors.New("OPENCODE_WORKSPACE_INVALID")
	}
	if in.Timeout < 0 {
		return RunInput{}, errors.New("OPENCODE_TIMEOUT_INVALID")
	}
	if in.Timeout == 0 || in.Timeout > 90*time.Second {
		in.Timeout = 90 * time.Second
	}
	if err := CheckSelectionApplicable(a.Name(), in.ExecutionSelectionSnapshot); err != nil {
		return RunInput{}, err
	}
	if snap := in.ExecutionSelectionSnapshot; snap != nil && snap.RequestedModelID != in.RequestedModelID {
		return RunInput{}, errors.New("OPENCODE_SELECTION_MISMATCH")
	}
	// Selection snapshot is the SOLE effort source for this adapter. We never
	// introduce caller args/env/permission/config override doors: the request
	// comes through the snapshot only, and we refuse whitespace/normalization
	// ambiguity or unsupported values before mint. The empty default preserves
	// legacy no-effort behavior; "low" is the single supported explicit value.
	//
	// The adapter NEVER writes through the caller-owned snapshot pointer. Every
	// leg it records lands on an owned copy, so the caller's RunInput stays
	// deeply equal before and after — including on the paths that refuse below,
	// which is why the copy is taken before the first validation, not after it.
	effort := ""
	if snap := in.ExecutionSelectionSnapshot; snap != nil {
		owned := *snap
		in.ExecutionSelectionSnapshot = &owned
		requested := owned.RequestedEffort
		if strings.TrimSpace(requested) != requested {
			return RunInput{}, errors.New("OPENCODE_EFFORT_NORMALIZATION_FORBIDDEN")
		}
		switch requested {
		case "":
			// preserve the no-effort default: no options key in the config, no
			// reasoning_effort on the wire, and whatever honest EffortEvidence
			// the caller set is carried through verbatim.
		case openCodeSupportedEffort:
			effort = openCodeSupportedEffort
			// RequestedEffort is the caller's statement and is never rewritten
			// here: "low" in is "low" recorded, not a normalization of " low".
			// The evidence names the immutable settings application only — this
			// adapter never observed the provider request or a provider receipt.
			owned.AppliedEffort = openCodeSupportedEffort
			owned.EffortEvidence = "settings:opencode.json"
		default:
			return RunInput{}, errors.New("OPENCODE_EFFORT_UNSUPPORTED")
		}
		if owned.AppliedEffort != effort {
			// A launch that carries no effort cannot honestly claim one was
			// applied, and a launch that carries one has just recorded it.
			return RunInput{}, errors.New("OPENCODE_EFFORT_APPLIED_MISMATCH")
		}
	}
	launch, err := a.resolve(in)
	if err != nil {
		return RunInput{}, err
	}
	launch.WorkID, launch.Workspace, launch.Prompt = in.WorkID, workspace, in.Prompt
	launch.Profile = in.LaunchProfileID
	launch.Effort = effort
	// Freeze every effort leg now. This is the state the launch is bound to;
	// anything else observed later is drift.
	launch.EffortBinding = openCodeEffortBindingOf(in.ExecutionSelectionSnapshot)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return RunInput{}, errors.New("OPENCODE_ISOLATION_INVALID")
	}
	temp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return RunInput{}, errors.New("OPENCODE_ISOLATION_INVALID")
	}
	launch.Root = filepath.Join(temp, "compound-opencode-"+hex.EncodeToString(nonce[:]))
	launch.Config, err = openCodeConfig(launch)
	if err != nil {
		return RunInput{}, errors.New("OPENCODE_CONFIG_INVALID")
	}
	in.Harness, in.Command, in.Workspace = a.Name(), launch.Command, workspace
	in.RequestedModelID, in.RequestedProviderRoute = launch.Model, launch.Provider
	in.ExpectedModelRouterRevision = launch.Revision
	in.opencodeLaunch = launch
	return in, nil
}

func (a *OpenCodeAdapter) resolve(in RunInput) (*openCodeLaunch, error) {
	manifest, _, revision, err := routeevidence.LoadModelCapabilitiesSnapshot(a.store.Home())
	if err != nil || in.ExpectedModelRouterRevision == "" || in.ExpectedModelRouterRevision != revision || in.RequestedModelID == "" {
		return nil, errors.New("OPENCODE_AUTHORITY_INVALID")
	}
	id, tuple, err := routeevidence.ResolveModelCapabilityTuple(manifest, in.RequestedModelID)
	provider, model, ok := strings.Cut(id, "/")
	if err != nil || id != in.RequestedModelID || tuple.Harness != a.Name() || !ok || !openCodeID.MatchString(provider) || !openCodeID.MatchString(model) || tuple.ProviderRoute != provider || in.RequestedProviderRoute != provider {
		return nil, errors.New("OPENCODE_TUPLE_INVALID")
	}
	u, err := url.Parse(tuple.ProviderEndpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != tuple.ProviderEndpoint || strings.ContainsAny(tuple.ProviderEndpoint, "\x00\r\n") {
		return nil, errors.New("OPENCODE_ENDPOINT_INVALID")
	}
	if port := u.Port(); port != "" && port != "443" && a.testNetworkPort == 0 {
		return nil, errors.New("OPENCODE_ENDPOINT_INVALID")
	}
	command, err := routeevidence.ResolveModelCapabilityExecutable(tuple.Aliases)
	if err != nil {
		return nil, errors.New("OPENCODE_EXECUTABLE_INVALID")
	}
	command, err = filepath.EvalSymlinks(command)
	if err != nil || !filepath.IsAbs(command) {
		return nil, errors.New("OPENCODE_EXECUTABLE_INVALID")
	}
	if in.Command != "" && in.Command != command {
		return nil, errors.New("OPENCODE_CALLER_OVERRIDE_FORBIDDEN")
	}
	digest, err := ompFileSHA256(command)
	if err != nil {
		return nil, errors.New("OPENCODE_EXECUTABLE_INVALID")
	}
	ownerHome, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.New("OPENCODE_AUTH_LOCATION_INVALID")
	}
	// Late host-managed native configuration cannot be replaced by isolation.
	for _, p := range []string{
		"/Library/Application Support/opencode/opencode.json", "/Library/Application Support/opencode/opencode.jsonc",
		"/Library/Managed Preferences/ai.opencode.managed.plist",
		filepath.Join("/Library/Managed Preferences", filepath.Base(ownerHome), "ai.opencode.managed.plist"),
	} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return nil, errors.New("OPENCODE_MANAGED_CONFIG_PRESENT")
		}
	}
	authPath := a.authPath
	if authPath == "" {
		authPath = filepath.Join(ownerHome, ".local", "share", "opencode", "auth.json")
	}
	return &openCodeLaunch{Model: id, Provider: provider, Endpoint: tuple.ProviderEndpoint, Revision: revision, Command: command, CommandSHA: digest, AuthPath: authPath}, nil
}

func (a *OpenCodeAdapter) validatePrepared(in RunInput) error {
	l := in.opencodeLaunch
	// Nil guard FIRST: every leg below dereferences the launch.
	if l == nil {
		return errors.New("OPENCODE_PREPARED_INPUT_INVALID")
	}
	if err := CheckSelectionApplicable(a.Name(), in.ExecutionSelectionSnapshot); err != nil {
		return err
	}
	if snap := in.ExecutionSelectionSnapshot; snap != nil && snap.RequestedModelID != "" && (snap.Harness != a.Name() || snap.RequestedModelID != in.RequestedModelID || snap.ProviderRoute != in.RequestedProviderRoute || snap.LaunchProfileRevision != in.ExpectedModelRouterRevision || snap.LaunchProfileID != in.LaunchProfileID) {
		return errors.New("OPENCODE_SELECTION_MISMATCH")
	}
	// All three effort legs were frozen onto the launch at preflight. Comparing
	// the whole binding — not just the applied leg — refuses a raised request, a
	// downgraded applied value, a rewritten evidence claim, and a snapshot
	// dropped or replaced outright, all as the same drift. It runs on preflight
	// reuse and again inside the start guard, immediately before native start,
	// so the check is not just a re-read of our own write.
	if openCodeEffortBindingOf(in.ExecutionSelectionSnapshot) != l.EffortBinding {
		return errors.New("OPENCODE_LAUNCH_BINDING_DRIFT")
	}
	if in.Harness != a.Name() || in.Command != l.Command || in.Workspace != l.Workspace || in.WorkID != l.WorkID || in.Prompt != l.Prompt || in.RequestedModelID != l.Model || in.RequestedProviderRoute != l.Provider || in.ExpectedModelRouterRevision != l.Revision || in.Timeout <= 0 || in.Timeout > 90*time.Second || len(in.Args) != 0 || len(in.Env) != 0 || in.Stdin != nil || in.PermissionMode != "" || in.LaunchProfileID != l.Profile {
		return errors.New("OPENCODE_PREPARED_INPUT_INVALID")
	}
	current, err := a.resolve(in)
	if err != nil {
		return err
	}
	current.WorkID, current.Workspace, current.Prompt, current.Root, current.Config = l.WorkID, l.Workspace, l.Prompt, l.Root, l.Config
	current.Profile, current.Effort, current.EffortBinding = l.Profile, l.Effort, l.EffortBinding
	if !reflect.DeepEqual(current, l) {
		return errors.New("OPENCODE_LAUNCH_BINDING_DRIFT")
	}
	return nil
}

func (a *OpenCodeAdapter) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	// The work wire may supply its general five-minute default after preflight;
	// this adapter always applies the stronger native prompt-only bound.
	if in.Timeout == 0 || in.Timeout > 90*time.Second {
		in.Timeout = 90 * time.Second
	}
	prepared, err := a.Preflight(ctx, in)
	if err != nil {
		return RunOutput{}, err
	}
	if prepared.ActionIntentID == "" || prepared.StartGuard == nil {
		return RunOutput{}, errors.New("OPENCODE_GOVERNED_START_REQUIRED")
	}
	l := prepared.opencodeLaunch
	// All filesystem effects, including credential copy, start inside PrepareEvidence
	// after the shared runner has minted the attempt. Cleanup covers every return.
	owned := false
	key := ""
	outerGuard := prepared.StartGuard
	run := prepared
	run.Command = a.sandboxCommand
	if run.Command == "" {
		run.Command = "/usr/bin/sandbox-exec"
	}
	run.Workspace = filepath.Join(l.Root, "project")
	run.Stdin = strings.NewReader(l.Prompt)
	requestedDigest := sha256.Sum256([]byte(l.Workspace))
	run.ProofRefs = append(append([]string(nil), prepared.ProofRefs...),
		"opencode:requested_workspace_sha256:"+hex.EncodeToString(requestedDigest[:]),
		"opencode:native_executable_sha256:"+l.CommandSHA,
		"opencode:model_router_revision:"+l.Revision,
		"opencode:prompt_only:v1:output_tokens=800:timeout_max=90s:sdk_retries=0")
	run.StartGuard = func(ctx context.Context, start func() error) error {
		return outerGuard(ctx, func() error {
			if err := a.validatePrepared(prepared); err != nil {
				return err
			}
			if err := verifyOpenCodeFiles(l); err != nil {
				return err
			}
			return start()
		})
	}
	return runWithSubprocessOptions(ctx, a.store, a.Name(), run, func(ctx context.Context, input RunInput) *exec.Cmd {
		cmd := exec.CommandContext(ctx, input.Command, "-p", a.sandboxProfile(l), l.Command, "run", "--pure", "--format", "json", "--agent", "compound-prompt-only", "--model", l.Model, "--title", "Compound prompt-only run", "--dir", run.Workspace)
		cmd.Dir, cmd.Env = run.Workspace, a.environment(l)
		return cmd
	}, a.heartbeatInterval, subprocessRunOptions{
		CleanupEvidence: func() error {
			if owned {
				if a.removeRoot != nil {
					if a.removeRoot(l.Root) != nil {
						return errors.New("OPENCODE_CLEANUP_FAILED")
					}
					return nil
				}
				return removeOpenCodeRoot(l.Root)
			}
			return nil
		},
		MaxTranscriptBytes: openCodeTranscriptLimit,
		SanitizeTranscriptLine: func(line string) string {
			if key != "" {
				return strings.ReplaceAll(line, key, "[REDACTED]")
			}
			return line
		},
		PrepareEvidence: func() error {
			if os.Mkdir(l.Root, 0o700) != nil {
				return errors.New("OPENCODE_ISOLATION_PREPARE_FAILED")
			}
			owned = true
			var err error
			key, err = readOpenCodeAPIKey(l.AuthPath, l.Provider)
			if err != nil {
				return err
			}
			return prepareOpenCodeFiles(l, key)
		},
		FinalizeEvidence: func(e subprocessEvidenceContext) ([]string, error) { return nil, validateOpenCodeTranscript(e) },
		stableStartError: "OPENCODE_START_FAILED",
	})
}

func openCodeConfig(l *openCodeLaunch) ([]byte, error) {
	_, model, _ := strings.Cut(l.Model, "/")
	modelEntry := map[string]any{"name": model, "limit": map[string]int{"context": 8192, "output": 800}}
	// Place reasoning_effort='low' on the model entry only when the launch was
	// bound with explicit low effort. The empty (no-effort default) path keeps
	// the legacy shape with no `options` key, so a no-effort run is byte-for-byte
	// indistinguishable from the pre-bounded config.
	if l.Effort != "" {
		modelEntry["options"] = map[string]any{"reasoningEffort": l.Effort}
	}
	return json.Marshal(map[string]any{
		"model": l.Model, "small_model": l.Model, "enabled_providers": []string{l.Provider},
		"provider":   map[string]any{l.Provider: map[string]any{"npm": "@ai-sdk/openai-compatible", "name": l.Provider, "options": map[string]any{"baseURL": l.Endpoint}, "models": map[string]any{model: modelEntry}}},
		"permission": map[string]string{"*": "deny"}, "agent": map[string]any{"compound-prompt-only": map[string]any{"mode": "primary", "permission": map[string]string{"*": "deny"}, "steps": 1}},
		"plugin": []string{}, "mcp": map[string]any{}, "lsp": false, "formatter": false, "autoupdate": false, "share": "disabled", "compaction": map[string]bool{"auto": false},
	})
}

func prepareOpenCodeFiles(l *openCodeLaunch, key string) error {
	invalid := errors.New("OPENCODE_ISOLATION_PREPARE_FAILED")
	for _, dir := range []string{"project", "config/opencode", "search-home/.opencode", "data/opencode", "state", "cache", "tmp"} {
		if os.MkdirAll(filepath.Join(l.Root, dir), 0o700) != nil {
			return invalid
		}
	}
	body, err := json.Marshal(map[string]any{l.Provider: map[string]string{"type": "api", "key": key}})
	if err != nil || os.WriteFile(filepath.Join(l.Root, "data/opencode/auth.json"), body, 0o600) != nil {
		return invalid
	}
	for _, dir := range []string{"config/opencode", "search-home/.opencode"} {
		if os.WriteFile(filepath.Join(l.Root, dir, "opencode.json"), l.Config, 0o400) != nil {
			return invalid
		}
		if os.WriteFile(filepath.Join(l.Root, dir, ".gitignore"), []byte(openCodeNativeGitignore), 0o400) != nil {
			return invalid
		}
	}
	for _, dir := range []string{"config/opencode", "config", "search-home/.opencode", "search-home"} {
		if os.Chmod(filepath.Join(l.Root, dir), 0o500) != nil {
			return invalid
		}
	}
	return verifyOpenCodeFiles(l)
}

func verifyOpenCodeFiles(l *openCodeLaunch) error {
	for _, dir := range []string{"config/opencode", "config", "search-home/.opencode", "search-home"} {
		info, err := os.Lstat(filepath.Join(l.Root, dir))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o500 {
			return errors.New("OPENCODE_IMMUTABLE_CONFIG_DRIFT")
		}
	}
	for _, file := range []string{"config/opencode/opencode.json", "search-home/.opencode/opencode.json", "config/opencode/.gitignore", "search-home/.opencode/.gitignore"} {
		path := filepath.Join(l.Root, file)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
			return errors.New("OPENCODE_IMMUTABLE_CONFIG_DRIFT")
		}
		body, err := os.ReadFile(path)
		expected := string(l.Config)
		if filepath.Base(file) == ".gitignore" {
			expected = openCodeNativeGitignore
		}
		if err != nil || string(body) != expected {
			return errors.New("OPENCODE_IMMUTABLE_CONFIG_DRIFT")
		}
	}
	return nil
}

func removeOpenCodeRoot(root string) error {
	for _, dir := range []string{"config", "config/opencode", "search-home", "search-home/.opencode"} {
		if err := os.Chmod(filepath.Join(root, dir), 0o700); err != nil && !os.IsNotExist(err) {
			return errors.New("OPENCODE_CLEANUP_FAILED")
		}
	}
	if err := os.RemoveAll(root); err != nil {
		return errors.New("OPENCODE_CLEANUP_FAILED")
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		return errors.New("OPENCODE_CLEANUP_FAILED")
	}
	return nil
}

func (a *OpenCodeAdapter) environment(l *openCodeLaunch) []string {
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8", "HOME=" + filepath.Join(l.Root, "search-home"), "TMPDIR=" + filepath.Join(l.Root, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(l.Root, "config"), "XDG_DATA_HOME=" + filepath.Join(l.Root, "data"), "XDG_STATE_HOME=" + filepath.Join(l.Root, "state"), "XDG_CACHE_HOME=" + filepath.Join(l.Root, "cache"),
		"OPENCODE_TEST_HOME=" + filepath.Join(l.Root, "search-home"), "OPENCODE_CONFIG_DIR=" + filepath.Join(l.Root, "config/opencode"), "OPENCODE_CONFIG_CONTENT=" + string(l.Config), "OPENCODE_PERMISSION={\"*\":\"deny\"}",
		"OPENCODE_PURE=1", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_DEFAULT_PLUGINS=1", "OPENCODE_DISABLE_EXTERNAL_SKILLS=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_AUTOCOMPACT=1", "OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX=800", "OPENCODE_AUTO_SHARE=false"}
	if a.testCAPath != "" {
		env = append(env, "NODE_EXTRA_CA_CERTS="+a.testCAPath)
	}
	return env
}

func (a *OpenCodeAdapter) sandboxProfile(l *openCodeLaunch) string {
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	// Port restriction permits provider HTTPS transport. It does not enforce
	// hostname identity or TLS; the validated immutable baseURL owns endpoint choice.
	// macOS hostname lookup uses this local resolver socket. Keep its exception
	// literal so unrelated Unix sockets remain denied.
	network := `(allow network-outbound (remote ip "*:443")) (allow network-outbound (literal "/private/var/run/mDNSResponder"))`
	if a.testNetworkPort > 0 {
		network += fmt.Sprintf(` (allow network-outbound (remote ip "localhost:%d"))`, a.testNetworkPort)
	}
	return `(version 1) (allow default) (deny network-outbound) ` + network + ` (deny file-write*) (allow file-write* (subpath ` + quote(l.Root) + `)) (allow file-write* (literal "/dev/null")) (deny file-write* (subpath ` + quote(filepath.Join(l.Root, "config")) + `) (subpath ` + quote(filepath.Join(l.Root, "search-home")) + `))`
}

func validateOpenCodeTranscript(e subprocessEvidenceContext) error {
	if e.TranscriptTruncated || e.CaptureFailed {
		return errors.New("OPENCODE_OUTPUT_CAPTURE_INCOMPLETE")
	}
	f, err := os.Open(e.TranscriptPath)
	if err != nil {
		return errors.New("OPENCODE_OUTPUT_INVALID")
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	text, complete := false, false
	for s.Scan() {
		line, ok := strings.CutPrefix(s.Text(), "stdout: ")
		if !ok {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Part struct {
				Text   string `json:"text"`
				Reason string `json:"reason"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			return errors.New("OPENCODE_OUTPUT_INVALID")
		}
		switch event.Type {
		case "error":
			return errors.New("OPENCODE_PROVIDER_ERROR")
		case "text":
			if complete {
				return errors.New("OPENCODE_OUTPUT_INVALID")
			}
			text = text || strings.TrimSpace(event.Part.Text) != ""
		case "step_finish":
			if complete || event.Part.Reason != "stop" {
				return errors.New("OPENCODE_FINISH_INVALID")
			}
			complete = true
		case "step_start":
			if complete {
				return errors.New("OPENCODE_OUTPUT_INVALID")
			}
		default:
			return errors.New("OPENCODE_EVENT_FORBIDDEN")
		}
	}
	if s.Err() != nil || !text || !complete {
		return errors.New("OPENCODE_OUTPUT_INCOMPLETE")
	}
	return nil
}
