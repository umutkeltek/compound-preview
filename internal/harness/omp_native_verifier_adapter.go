package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	homepkg "compound/internal/home"
	"compound/internal/runattempt"
)

const ompNativeVerifierAdapterName = "omp-native-verifier"

type ompNativeBinding struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type ompNativeVerifierLaunch struct {
	Projection       OMPOwnerProjection
	Interpreter      ompNativeBinding
	RecipeSHA        string
	failedCapture    []byte
	failedCaptureSet bool
	captureFD        *os.File
	capture          *homepkg.PrivateSpool
	probeRoot        string // Created by the parent after mint; never supplied by the caller.
}

type OMPNativeVerifierAdapter struct {
	store             *runattempt.Store
	heartbeatInterval time.Duration
	projectionRoot    string
	profileID         string
	interpreterPath   string
}

func NewOMPNativeVerifierAdapter(store *runattempt.Store) *OMPNativeVerifierAdapter {
	return &OMPNativeVerifierAdapter{store: store, heartbeatInterval: defaultHeartbeatInterval, profileID: "omp"}
}

func (a *OMPNativeVerifierAdapter) Name() string { return ompNativeVerifierAdapterName }

func (a *OMPNativeVerifierAdapter) Preflight(_ context.Context, in RunInput) (RunInput, error) {
	if a == nil || a.store == nil {
		return RunInput{}, fmt.Errorf("omp native verifier: runattempt store required")
	}
	if in.ompNativeVerifier != nil {
		if err := ValidateOMPNativeVerifierPreparedInput(in); err != nil {
			return RunInput{}, err
		}
	}
	if strings.TrimSpace(in.WorkID) == "" || strings.TrimSpace(in.Workspace) == "" || in.Now.IsZero() {
		return RunInput{}, fmt.Errorf("omp native verifier: work_id, workspace, and now are required")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" || strings.HasPrefix(prompt, "-") || strings.HasPrefix(prompt, "@") {
		return RunInput{}, fmt.Errorf("omp native verifier: plain prompt required")
	}
	if in.ompNativeVerifier == nil && (strings.TrimSpace(in.Command) != "" || len(in.Args) != 0 || len(in.Env) != 0 || strings.TrimSpace(in.PermissionMode) != "") {
		return RunInput{}, fmt.Errorf("omp native verifier: caller command, args, env, and permission mode are forbidden")
	}
	if info, err := os.Stat(in.Workspace); err != nil || !info.IsDir() {
		return RunInput{}, fmt.Errorf("omp native verifier: workspace must be an existing directory")
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(in.Workspace)
	if err != nil {
		return RunInput{}, fmt.Errorf("omp native verifier: OMP_WORKSPACE_INVALID")
	}
	launch, err := a.resolveLaunch(in.ompNativeVerifier, in.LaunchProfileID)
	if err != nil {
		return RunInput{}, fmt.Errorf("omp native verifier: OMP_LAUNCH_BINDING_INVALID")
	}
	in.Harness, in.Command, in.Prompt, in.Workspace = a.Name(), launch.Interpreter.Path, prompt, canonicalWorkspace
	in.Args, in.Env, in.PermissionMode = nil, nil, ""
	in.ompNativeVerifier = launch
	in.LaunchProfileID = launch.Projection.Profile.ID
	return in, nil
}

func (a *OMPNativeVerifierAdapter) resolveLaunch(existing *ompNativeVerifierLaunch, requestedProfileID string) (*ompNativeVerifierLaunch, error) {
	if existing != nil {
		if err := validateOMPNativeLaunch(existing); err != nil {
			return nil, err
		}
		return existing, nil
	}
	root := a.projectionRoot
	if root == "" {
		var err error
		root, err = defaultOMPOwnerProjectionRoot()
		if err != nil {
			return nil, fmt.Errorf("omp native verifier: OMP_PROJECTION_ROOT_INVALID")
		}
	}
	profileID := requestedProfileID
	if profileID == "" {
		profileID = a.profileID
	}
	if profileID == "" {
		profileID = "omp"
	}
	projection, err := loadOMPOwnerProjectionFrom(root, profileID)
	if err != nil {
		return nil, fmt.Errorf("omp native verifier: OMP_PROJECTION_INVALID")
	}
	interpreter, err := resolveOMPVerifierInterpreter(a.interpreterPath)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(ompNativeVerifierRecipe))
	launch := &ompNativeVerifierLaunch{Projection: projection, Interpreter: interpreter, RecipeSHA: hex.EncodeToString(sum[:])}
	if err := validateOMPNativeLaunch(launch); err != nil {
		return nil, err
	}
	return launch, nil
}

func resolveOMPVerifierInterpreter(explicit string) (ompNativeBinding, error) {
	candidates := []string{explicit}
	if explicit == "" {
		candidates = []string{"/opt/homebrew/bin/node", "/usr/local/bin/node", "/usr/bin/node"}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		canonical, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		sum, err := ompFileSHA256(canonical)
		if err == nil {
			return ompNativeBinding{Path: canonical, SHA256: sum}, nil
		}
	}
	return ompNativeBinding{}, fmt.Errorf("omp native verifier: fixed Node interpreter unavailable")
}

func validateOMPNativeLaunch(l *ompNativeVerifierLaunch) error {
	if l == nil {
		return fmt.Errorf("omp native verifier: resolved launch missing")
	}
	if err := l.Projection.Validate(l.Projection.Profile.ID); err != nil {
		return fmt.Errorf("omp native verifier: OMP_PROJECTION_DRIFT")
	}
	expected := map[string]ompNativeBinding{
		"wrapper":       {Path: l.Projection.PromptOnly.Wrapper.Path, SHA256: l.Projection.PromptOnly.Wrapper.SHA256},
		"models config": {Path: l.Projection.PromptOnly.ModelsConfig.Path, SHA256: l.Projection.PromptOnly.ModelsConfig.SHA256},
		"executable":    {Path: l.Projection.PromptOnly.Executable.Path, SHA256: l.Projection.PromptOnly.Executable.SHA256},
		"interpreter":   l.Interpreter,
	}
	for name, binding := range expected {
		canonical, err := filepath.EvalSymlinks(binding.Path)
		if err != nil || canonical != binding.Path {
			return fmt.Errorf("omp native verifier: %s path is not canonical", name)
		}
		got, err := ompFileSHA256(binding.Path)
		if err != nil || got != binding.SHA256 {
			return fmt.Errorf("omp native verifier: %s bytes drifted", name)
		}
	}
	sum := sha256.Sum256([]byte(ompNativeVerifierRecipe))
	if hex.EncodeToString(sum[:]) != l.RecipeSHA {
		return fmt.Errorf("omp native verifier: helper recipe drifted")
	}
	return nil
}

// ValidateOMPNativeVerifierPreparedInput validates the opaque native preflight
// result. It does not admit a producer proof or allow caller-selected launch fields.
func ValidateOMPNativeVerifierPreparedInput(in RunInput) error {
	launch := in.ompNativeVerifier
	if launch == nil || in.LaunchProfileID != launch.Projection.Profile.ID || in.Harness != ompNativeVerifierAdapterName ||
		in.Command == "" || in.Command != launch.Interpreter.Path ||
		len(in.Args) != 0 || len(in.Env) != 0 || in.PermissionMode != "" {
		return fmt.Errorf("OMP_NATIVE_PREPARED_INPUT_INVALID")
	}
	if err := validateOMPNativeLaunch(launch); err != nil {
		return fmt.Errorf("OMP_LAUNCH_BINDING_INVALID")
	}
	return nil
}

// revalidateLaunch reloads the current owner projection as well as every bound
// executable/configuration immediately inside the caller's final start guard.
func (a *OMPNativeVerifierAdapter) revalidateLaunch(expected *ompNativeVerifierLaunch) error {
	if expected == nil {
		return fmt.Errorf("OMP_LAUNCH_BINDING_INVALID")
	}
	current, err := a.resolveLaunch(nil, expected.Projection.Profile.ID)
	if err != nil || current.Interpreter != expected.Interpreter ||
		current.RecipeSHA != expected.RecipeSHA ||
		!reflect.DeepEqual(current.Projection, expected.Projection) {
		return fmt.Errorf("OMP_LAUNCH_BINDING_INVALID")
	}
	return nil
}

func (a *OMPNativeVerifierAdapter) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	if strings.TrimSpace(in.ActionIntentID) == "" || in.StartGuard == nil {
		return RunOutput{}, fmt.Errorf("omp native verifier: governed action and start guard are required")
	}
	resolved, err := a.Preflight(ctx, in)
	if err != nil {
		return RunOutput{}, err
	}
	originalGuard := resolved.StartGuard
	expectedLaunch := resolved.ompNativeVerifier
	resolved.StartGuard = func(ctx context.Context, start func() error) error {
		return originalGuard(ctx, func() error {
			if err := a.revalidateLaunch(expectedLaunch); err != nil {
				return err
			}
			return start()
		})
	}
	var spool *homepkg.PrivateSpool
	resolved.ompNativeVerifier.failedCapture = nil
	resolved.ompNativeVerifier.failedCaptureSet = false
	prepare := func() error {
		artifacts, err := homepkg.NewArtifactStore(a.store.Home())
		if err != nil {
			return fmt.Errorf("OMP_PRIVATE_STORE_UNAVAILABLE")
		}
		spool, err = artifacts.CreatePrivateSpool()
		if err != nil {
			return fmt.Errorf("OMP_PRIVATE_SPOOL_CREATE")
		}
		fd, err := spool.OpenForChild()
		if err != nil {
			return fmt.Errorf("OMP_PRIVATE_SPOOL_OPEN")
		}
		probeRoot, err := os.MkdirTemp("", "compound-omp-native-")
		if err != nil {
			return fmt.Errorf("OMP_PRIVATE_PROBE_CREATE")
		}
		resolved.ompNativeVerifier.probeRoot = probeRoot
		canonicalProbeRoot, err := filepath.EvalSymlinks(probeRoot)
		if err != nil {
			return fmt.Errorf("OMP_PRIVATE_PROBE_RESOLVE")
		}
		resolved.ompNativeVerifier.probeRoot = canonicalProbeRoot
		cfg, err := a.verifierConfig(resolved)
		if err != nil {
			return fmt.Errorf("OMP_PRIVATE_CONFIG_ENCODE")
		}
		if _, err = fd.Write(cfg); err != nil {
			return fmt.Errorf("OMP_PRIVATE_CONFIG_WRITE")
		}
		if err = fd.Sync(); err != nil {
			return fmt.Errorf("OMP_PRIVATE_CONFIG_SYNC")
		}
		if _, err = fd.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("OMP_PRIVATE_CONFIG_REWIND")
		}
		resolved.ompNativeVerifier.captureFD = fd
		resolved.ompNativeVerifier.capture = spool
		return nil
	}
	finalize := func(evidence subprocessEvidenceContext) ([]string, error) {
		return a.finalizeEvidence(evidence, resolved.ompNativeVerifier, resolved)
	}
	retainFailed := func(evidence subprocessEvidenceContext) (string, error) {
		return a.retainFailedEvidence(evidence, resolved, resolved.ompNativeVerifier)
	}
	out, runErr := runWithSubprocessOptions(ctx, a.store, a.Name(), resolved, a.buildCmd, a.heartbeatInterval, subprocessRunOptions{PrepareEvidence: prepare, FinalizeEvidence: finalize, RetainFailedEvidence: retainFailed, stableStartError: "OMP_PROCESS_START"})
	// The subprocess may be killed before its own cleanup can run. Always
	// remove only the exact temporary root allocated by this parent attempt.
	runErr = errors.Join(runErr, resolved.ompNativeVerifier.removeProbeRoot())
	if spool != nil {
		if err := spool.Remove(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("omp native verifier: OMP_PRIVATE_SPOOL_CLEANUP"))
		}
	}
	return out, runErr
}

func (l *ompNativeVerifierLaunch) removeProbeRoot() error {
	if l == nil || l.probeRoot == "" {
		return nil
	}
	if err := os.RemoveAll(l.probeRoot); err != nil {
		return fmt.Errorf("omp native verifier: OMP_PRIVATE_PROBE_CLEANUP")
	}
	l.probeRoot = ""
	return nil
}

func (a *OMPNativeVerifierAdapter) verifierConfig(in RunInput) ([]byte, error) {
	l := in.ompNativeVerifier
	cfg := map[string]any{
		"workspace": in.Workspace, "prompt": in.Prompt, "fixed_argv": l.Projection.PromptOnly.FixedArgv, "probe_root": l.probeRoot,
		"profile_id": l.Projection.Profile.ID, "request_model": l.Projection.Profile.RequestModel,
		"route": l.Projection.Profile.Route, "capture_endpoint": l.Projection.Profile.CaptureEndpoint,
		"capture_policy":       l.Projection.Profile.CapturePolicy.Mode,
		"conformance_revision": l.Projection.ConformanceRevision, "launch_profile_ref": l.Projection.LaunchProfileRef,
		"recipe_sha256": l.RecipeSHA, "interpreter": l.Interpreter,
		"wrapper":       ompNativeBinding{Path: l.Projection.PromptOnly.Wrapper.Path, SHA256: l.Projection.PromptOnly.Wrapper.SHA256},
		"models_config": ompNativeBinding{Path: l.Projection.PromptOnly.ModelsConfig.Path, SHA256: l.Projection.PromptOnly.ModelsConfig.SHA256},
		"executable":    ompNativeBinding{Path: l.Projection.PromptOnly.Executable.Path, SHA256: l.Projection.PromptOnly.Executable.SHA256},
	}
	return json.Marshal(cfg)
}

func (a *OMPNativeVerifierAdapter) buildCmd(ctx context.Context, in RunInput) *exec.Cmd {
	l := in.ompNativeVerifier
	cmd := exec.CommandContext(ctx, l.Interpreter.Path, "-e", ompNativeVerifierRecipe)
	cmd.Dir = in.Workspace
	cmd.Env = []string{"HOME=" + os.TempDir(), "TMPDIR=" + os.TempDir(), "PATH=/usr/bin:/bin", "LANG=C"}
	cmd.ExtraFiles = []*os.File{l.captureFD}
	return cmd
}

type ompNativeVerifierReport struct {
	OwnerSHA256   string           `json:"owner_sha256"`
	SchemaVersion string           `json:"schema_version"`
	Result        string           `json:"result"`
	RecipeSHA256  string           `json:"recipe_sha256"`
	Interpreter   ompNativeBinding `json:"interpreter"`
	Owner         struct {
		ProfileID           string `json:"profile_id"`
		ConformanceRevision string `json:"conformance_revision"`
		LaunchProfileRef    string `json:"launch_profile_ref"`
		RequestModel        string `json:"request_model"`
		Route               string `json:"route"`
	} `json:"-"`
	Bindings struct {
		Wrapper      ompNativeBinding `json:"wrapper"`
		ModelsConfig ompNativeBinding `json:"models_config"`
		Executable   ompNativeBinding `json:"executable"`
	} `json:"bindings"`
	WrapperProbe struct {
		CaptureShimSHA256 string `json:"capture_shim_sha256"`
		ObservedSHA256    string `json:"observed_sha256"`
		ArgvCount         int    `json:"argv_count"`
		CWDMatch          bool   `json:"cwd_match"`
		IsolationSame     bool   `json:"isolation_same"`
		IsolationFresh    bool   `json:"isolation_fresh"`
	} `json:"wrapper_probe"`
	NativeProbe struct {
		RequestCount  int    `json:"request_count"`
		RequestSHA256 string `json:"request_sha256"`
		RequestLength int    `json:"request_length"`
		ToolsCount    int    `json:"tools_count"`
		ToolMessages  int    `json:"tool_messages"`
		MarkerAbsent  bool   `json:"marker_absent"`
		StdoutSHA256  string `json:"stdout_sha256"`
		StdoutLength  int    `json:"stdout_length"`
		StderrSHA256  string `json:"stderr_sha256"`
		StderrLength  int    `json:"stderr_length"`
		ModelsSHA256  string `json:"synthetic_models_sha256"`
		EndpointKind  string `json:"endpoint_kind"`
	} `json:"native_probe"`
}

type ompNativePrivateEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Config        json.RawMessage `json:"config"`
	Report        json.RawMessage `json:"report"`
	Sanitized     json.RawMessage `json:"sanitized"`
}

// Decode exact top-level keys before binding values: encoding/json otherwise
// accepts duplicate/case-alias keys and non-object RawMessages. Nested object
// semantics remain owned by the verifier; its upstream spool owns the byte cap.
func decodeOMPPrivateEnvelope(body []byte) (ompNativePrivateEnvelope, error) {
	var envelope ompNativePrivateEnvelope
	schemaErr := fmt.Errorf("OMP_PRIVATE_CAPTURE_SCHEMA")
	if !utf8.Valid(body) {
		return envelope, schemaErr
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return envelope, schemaErr
	}
	fields := make(map[string]json.RawMessage, 4)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return envelope, schemaErr
		}
		switch key {
		case "schema_version", "config", "report", "sanitized":
		default:
			return envelope, schemaErr
		}
		if _, duplicate := fields[key]; duplicate {
			return envelope, schemaErr
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return envelope, schemaErr
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(fields) != 4 {
		return envelope, schemaErr
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return envelope, schemaErr
	}
	for _, key := range []string{"config", "report", "sanitized"} {
		value := bytes.TrimSpace(fields[key])
		if len(value) == 0 || value[0] != '{' {
			return envelope, schemaErr
		}
	}
	if err := json.Unmarshal(fields["schema_version"], &envelope.SchemaVersion); err != nil || envelope.SchemaVersion != "compound.omp_native_verifier_private.v0.1" {
		return ompNativePrivateEnvelope{}, schemaErr
	}
	envelope.Config, envelope.Report, envelope.Sanitized = fields["config"], fields["report"], fields["sanitized"]
	return envelope, nil
}

func decodeOMPPublicReport(body []byte) (ompNativeVerifierReport, []byte, error) {
	var report ompNativeVerifierReport
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return report, nil, fmt.Errorf("OMP_PUBLIC_REPORT_SCHEMA")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return report, nil, fmt.Errorf("OMP_PUBLIC_REPORT_SCHEMA")
	}
	canonical, err := json.Marshal(report)
	if err != nil {
		return report, nil, fmt.Errorf("OMP_PUBLIC_REPORT_SCHEMA")
	}
	return report, canonical, nil
}

func (a *OMPNativeVerifierAdapter) finalizeEvidence(ctx subprocessEvidenceContext, launch *ompNativeVerifierLaunch, in RunInput) ([]string, error) {
	if launch.capture == nil {
		return nil, fmt.Errorf("OMP_PRIVATE_SPOOL_MISSING")
	}
	privateBody, err := launch.capture.Consume(12 << 20)
	if err != nil {
		return nil, fmt.Errorf("OMP_PRIVATE_CAPTURE_READ")
	}
	// Keep the one-shot snapshot available for failure-only custody if any
	// later validation or publication step refuses certification.
	launch.failedCapture = append([]byte(nil), privateBody...)
	launch.failedCaptureSet = true
	envelope, err := decodeOMPPrivateEnvelope(privateBody)
	if err != nil {
		return nil, err
	}
	if err := validateOMPTranscript(ctx.TranscriptPath); err != nil {
		return nil, err
	}
	body, err := validateOMPNativeCapturedEvidence(launch, in.Workspace, in.Prompt, envelope.Sanitized, privateBody)
	if err != nil {
		return nil, err
	}
	stored, err := a.store.Get(ctx.RunAttemptID)
	if err != nil {
		return nil, fmt.Errorf("OMP_ATTEMPT_READ")
	}
	artifacts, err := homepkg.NewArtifactStore(a.store.Home())
	if err != nil {
		return nil, fmt.Errorf("OMP_ARTIFACT_STORE")
	}
	privateBodies := map[string][]byte{"capture": privateBody}
	for role, binding := range map[string]ompNativeBinding{"interpreter": launch.Interpreter, "wrapper": {Path: launch.Projection.PromptOnly.Wrapper.Path, SHA256: launch.Projection.PromptOnly.Wrapper.SHA256}, "models_config": {Path: launch.Projection.PromptOnly.ModelsConfig.Path, SHA256: launch.Projection.PromptOnly.ModelsConfig.SHA256}, "executable": {Path: launch.Projection.PromptOnly.Executable.Path, SHA256: launch.Projection.PromptOnly.Executable.SHA256}} {
		privateBytes, readErr := readOMPPrivateBinding(binding)
		if readErr != nil {
			return nil, fmt.Errorf("OMP_PRIVATE_SOURCE_READ")
		}
		privateBodies[role] = privateBytes
	}
	privateDigests := make(map[string]string, len(privateBodies))
	for role, privateBytes := range privateBodies {
		ref, putErr := artifacts.PutPrivate(privateBytes)
		if putErr != nil {
			return nil, fmt.Errorf("OMP_PRIVATE_PUT")
		}
		privateDigests[role] = strings.TrimPrefix(ref, homepkg.ArtifactRefPrefix)
	}
	if err := launch.capture.Remove(); err != nil {
		return nil, fmt.Errorf("OMP_PRIVATE_SPOOL_CLEANUP")
	}
	// Custody is durable above. Cleanup must succeed before publishing any
	// success-shaped artifact or allowing the attempt to become succeeded.
	if err := launch.removeProbeRoot(); err != nil {
		return nil, err
	}
	refs := make([]string, 0, 3)
	for _, bytes := range [][]byte{[]byte(ompNativeVerifierRecipe), body} {
		ref, err := artifacts.Put(bytes)
		if err != nil {
			return nil, fmt.Errorf("OMP_PUBLIC_PUT")
		}
		refs = append(refs, ref)
	}
	sum := sha256.Sum256(body)
	proof, err := json.Marshal(map[string]any{"schema_version": "compound.omp_prompt_only_native_proof.v0.3", "work_id": stored.WorkID, "run_attempt_id": stored.ID, "action_intent_id": stored.ActionIntentID, "report_sha256": hex.EncodeToString(sum[:]), "private_custody_sha256": privateDigests, "evidence_refs": refs})
	if err != nil {
		return nil, fmt.Errorf("OMP_PUBLIC_PROOF_ENCODE")
	}
	proofRef, err := artifacts.Put(proof)
	if err != nil {
		return nil, fmt.Errorf("OMP_PUBLIC_PUT")
	}
	return append(refs, proofRef), nil
}

// validateOMPNativeCapturedEvidence preserves the producer's semantic checks for
// reuse by the read-only custody consumer. Raw custody and current launch
// bindings are checked separately before calling this helper.
func validateOMPNativeCapturedEvidence(launch *ompNativeVerifierLaunch, workspace, prompt string, reportBody, captureBody []byte) ([]byte, error) {
	if launch == nil {
		return nil, fmt.Errorf("OMP_LAUNCH_BINDING_INVALID")
	}
	report, body, err := decodeOMPPublicReport(reportBody)
	if err != nil {
		return nil, err
	}
	p := launch.Projection
	ownerBody, _ := json.Marshal(struct {
		ProfileID           string `json:"profile_id"`
		ConformanceRevision string `json:"conformance_revision"`
		LaunchProfileRef    string `json:"launch_profile_ref"`
		RequestModel        string `json:"request_model"`
		Route               string `json:"route"`
	}{p.Profile.ID, p.ConformanceRevision, p.LaunchProfileRef, p.Profile.RequestModel, p.Profile.Route})
	ownerSum := sha256.Sum256(ownerBody)
	wantArgv := make([]string, len(p.PromptOnly.FixedArgv))
	for i, value := range p.PromptOnly.FixedArgv {
		value = strings.ReplaceAll(value, "<workspace>", workspace)
		wantArgv[i] = strings.ReplaceAll(value, "<prompt>", prompt)
	}
	if report.SchemaVersion != "compound.omp_native_verifier_report.v0.2" || report.Result != "no_tools_and_hostile_tool_call_not_executed" ||
		report.RecipeSHA256 != launch.RecipeSHA || report.Interpreter.SHA256 != launch.Interpreter.SHA256 || report.Interpreter.Path != "" ||
		report.OwnerSHA256 != hex.EncodeToString(ownerSum[:]) ||
		report.Bindings.Wrapper.SHA256 != p.PromptOnly.Wrapper.SHA256 || report.Bindings.Wrapper.Path != "" ||
		report.Bindings.ModelsConfig.SHA256 != p.PromptOnly.ModelsConfig.SHA256 || report.Bindings.ModelsConfig.Path != "" ||
		report.Bindings.Executable.SHA256 != p.PromptOnly.Executable.SHA256 || report.Bindings.Executable.Path != "" ||
		!report.WrapperProbe.CWDMatch || report.WrapperProbe.ArgvCount != len(wantArgv) ||
		!report.WrapperProbe.IsolationSame || !report.WrapperProbe.IsolationFresh || report.NativeProbe.RequestCount != 1 || report.NativeProbe.ToolsCount != 0 || report.NativeProbe.ToolMessages != 0 || !report.NativeProbe.MarkerAbsent || report.NativeProbe.EndpointKind != "loopback_fixture" {
		return nil, fmt.Errorf("invalid native verifier report")
	}
	body, _ = json.Marshal(report)
	for _, digest := range []string{report.WrapperProbe.CaptureShimSHA256, report.WrapperProbe.ObservedSHA256, report.NativeProbe.RequestSHA256, report.NativeProbe.StdoutSHA256, report.NativeProbe.StderrSHA256, report.NativeProbe.ModelsSHA256} {
		if len(digest) != 64 {
			return nil, fmt.Errorf("invalid native verifier observation digest")
		}
	}
	var private struct {
		SchemaVersion string `json:"schema_version"`
		Report        struct {
			WrapperProbe struct {
				Argv []string `json:"argv"`
				CWD  string   `json:"cwd"`
			} `json:"wrapper_probe"`
			NativeProbe struct {
				RequestBase64 string `json:"request_base64"`
				StdoutBase64  string `json:"stdout_base64"`
				StderrBase64  string `json:"stderr_base64"`
			} `json:"native_probe"`
		} `json:"report"`
	}
	if err := json.Unmarshal(captureBody, &private); err != nil || private.SchemaVersion != "compound.omp_native_verifier_private.v0.1" {
		return nil, fmt.Errorf("OMP_PRIVATE_CAPTURE_SCHEMA")
	}
	if private.Report.WrapperProbe.CWD != workspace || !reflectStringSliceEqual(private.Report.WrapperProbe.Argv, wantArgv) {
		return nil, fmt.Errorf("OMP_PRIVATE_WRAPPER_DRIFT")
	}
	for name, item := range map[string]struct {
		encoded, digest string
		length          int
	}{"request": {private.Report.NativeProbe.RequestBase64, report.NativeProbe.RequestSHA256, report.NativeProbe.RequestLength}, "stdout": {private.Report.NativeProbe.StdoutBase64, report.NativeProbe.StdoutSHA256, report.NativeProbe.StdoutLength}, "stderr": {private.Report.NativeProbe.StderrBase64, report.NativeProbe.StderrSHA256, report.NativeProbe.StderrLength}} {
		if err := validateOMPPrivateBytes(name, item.encoded, item.digest, item.length); err != nil {
			return nil, err
		}
	}
	return body, nil
}

func validateOMPPrivateBytes(name, encoded, digest string, length int) error {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != length {
		return fmt.Errorf("OMP_PRIVATE_%s_LENGTH", strings.ToUpper(name))
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("OMP_PRIVATE_%s_DIGEST", strings.ToUpper(name))
	}
	return nil
}

func validateOMPTranscript(path string) error {
	transcript, err := os.ReadFile(path)
	if err != nil || string(transcript) != "stdout: OMP_NATIVE_CAPTURE_READY\n" {
		return fmt.Errorf("OMP_PUBLIC_TRANSCRIPT_DRIFT")
	}
	return nil
}

func readOMPPrivateBinding(binding ompNativeBinding) ([]byte, error) {
	f, err := os.Open(binding.Path)
	if err != nil {
		return nil, fmt.Errorf("open bound source")
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > 256<<20 {
		return nil, fmt.Errorf("invalid bound source")
	}
	body, err := io.ReadAll(io.LimitReader(f, (256<<20)+1))
	after, statErr := f.Stat()
	if err != nil || statErr != nil || len(body) > 256<<20 || int64(len(body)) != before.Size() || !os.SameFile(before, after) || after.Size() != before.Size() {
		return nil, fmt.Errorf("read bound source")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != binding.SHA256 {
		return nil, fmt.Errorf("bound source drift")
	}
	return body, nil
}

func reflectStringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
