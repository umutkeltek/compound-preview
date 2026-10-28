package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	homepkg "compound/internal/home"
	"compound/internal/runattempt"
)

var errOMPNativeProofCustody = errors.New("OMP_NATIVE_PROOF_CUSTODY_INVALID")

type ompNativeCustodyConfig struct {
	Workspace           string
	Prompt              string
	FixedArgv           []string
	ProbeRoot           string
	ProfileID           string
	RequestModel        string
	Route               string
	CaptureEndpoint     string
	CapturePolicy       string
	ConformanceRevision string
	LaunchProfileRef    string
	RecipeSHA256        string
	Interpreter         ompNativeBinding
	Wrapper             ompNativeBinding
	ModelsConfig        ompNativeBinding
	Executable          ompNativeBinding
}

// ValidateOMPNativeProofCustody proves that a decoded public proof still
// resolves to exact public and private bodies in Compound custody and that
// those bodies agree with the current OMP owner projection and live bindings.
// It is read-only and deliberately does not admit a producer attempt.
func ValidateOMPNativeProofCustody(home string, proof OMPNativePublicProof) error {
	projectionRoot, err := defaultOMPOwnerProjectionRoot()
	if err != nil {
		return errOMPNativeProofCustody
	}
	return validateOMPNativeProofCustodyAtProjectionRoot(home, projectionRoot, proof)
}

func validateOMPNativeProofCustodyAtProjectionRoot(home, projectionRoot string, proof OMPNativePublicProof) error {
	_, err := validateOMPNativeProofCustodyAtRoot(home, projectionRoot, proof)
	return err
}

func validateOMPNativeProofCustodyAtRoot(home, projectionRoot string, proof OMPNativePublicProof) (OMPOwnerProjection, error) {
	if !validOMPNativeCustodyProof(proof) ||
		strings.TrimSpace(projectionRoot) == "" {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}

	artifacts, err := homepkg.OpenArtifactReader(home)
	if err != nil {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}
	recipeBody, err := artifacts.Get(proof.EvidenceRefs[0])
	if err != nil {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}
	reportBody, err := artifacts.Get(proof.EvidenceRefs[1])
	if err != nil {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}
	reportSum := sha256.Sum256(reportBody)
	if hex.EncodeToString(reportSum[:]) != proof.ReportSHA256 {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}

	roles := [...]string{"capture", "interpreter", "wrapper", "models_config", "executable"}
	privateBodies := make(map[string][]byte, len(roles))
	for _, role := range roles {
		body, getErr := artifacts.GetPrivate(
			homepkg.ArtifactRefPrefix + proof.PrivateCustodySHA256[role],
		)
		if getErr != nil {
			return OMPOwnerProjection{}, errOMPNativeProofCustody
		}
		privateBodies[role] = body
	}

	envelope, err := decodeOMPPrivateEnvelope(privateBodies["capture"])
	if err != nil {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}
	config, err := decodeOMPNativeCustodyConfig(envelope.Config)
	if err != nil {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}

	recipeSum := sha256.Sum256([]byte(ompNativeVerifierRecipe))
	recipeDigest := hex.EncodeToString(recipeSum[:])
	if !bytes.Equal(recipeBody, []byte(ompNativeVerifierRecipe)) ||
		config.RecipeSHA256 != recipeDigest {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}

	projection, err := loadOMPOwnerProjectionFrom(projectionRoot, config.ProfileID)
	if err != nil || projection.Validate(config.ProfileID) != nil ||
		!ompNativeCustodyConfigMatchesProjection(config, projection) {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}
	currentInterpreter, err := resolveOMPVerifierInterpreter("")
	if err != nil || currentInterpreter != config.Interpreter {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}
	launch := &ompNativeVerifierLaunch{
		Projection:  projection,
		Interpreter: config.Interpreter,
		RecipeSHA:   config.RecipeSHA256,
	}
	if validateOMPNativeLaunch(launch) != nil {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}

	bindings := [...]struct {
		role    string
		binding ompNativeBinding
	}{
		{"interpreter", config.Interpreter},
		{"wrapper", config.Wrapper},
		{"models_config", config.ModelsConfig},
		{"executable", config.Executable},
	}
	for _, item := range bindings {
		liveBody, readErr := readOMPPrivateBinding(item.binding)
		if readErr != nil || !bytes.Equal(liveBody, privateBodies[item.role]) {
			return OMPOwnerProjection{}, errOMPNativeProofCustody
		}
	}

	_, sanitizedCanonical, err := decodeOMPPublicReport(envelope.Sanitized)
	if err != nil {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}
	publicCanonical, err := validateOMPNativeCapturedEvidence(
		launch,
		config.Workspace,
		config.Prompt,
		reportBody,
		privateBodies["capture"],
	)
	if err != nil ||
		!bytes.Equal(reportBody, publicCanonical) ||
		!bytes.Equal(sanitizedCanonical, publicCanonical) {
		return OMPOwnerProjection{}, errOMPNativeProofCustody
	}
	return projection, nil
}

func validOMPNativeCustodyProof(proof OMPNativePublicProof) bool {
	if proof.SchemaVersion != ompNativePublicProofSchemaVersion ||
		strings.TrimSpace(proof.WorkID) == "" ||
		!runattempt.Valid(proof.RunAttemptID) ||
		strings.TrimSpace(proof.ActionIntentID) == "" ||
		!isOMPNativePublicProofLowerHex(proof.ReportSHA256, ompNativePublicProofDigestHexLen) ||
		len(proof.EvidenceRefs) != ompNativePublicProofEvidenceRefCount ||
		len(proof.PrivateCustodySHA256) != len(ompNativePublicProofCustodyRoles) {
		return false
	}
	for index, ref := range proof.EvidenceRefs {
		if !isOMPNativePublicProofEvidenceRef(ref) ||
			(index == 1 && ref != ompNativePublicProofEvidenceRefPrefix+proof.ReportSHA256) {
			return false
		}
	}
	for role := range ompNativePublicProofCustodyRoles {
		digest, ok := proof.PrivateCustodySHA256[role]
		if !ok || !isOMPNativePublicProofLowerHex(digest, ompNativePublicProofDigestHexLen) {
			return false
		}
	}
	return true
}

func decodeOMPNativeCustodyConfig(body []byte) (ompNativeCustodyConfig, error) {
	var config ompNativeCustodyConfig
	fields, err := decodeOMPNativeCustodyObject(body, []string{
		"workspace",
		"prompt",
		"fixed_argv",
		"probe_root",
		"profile_id",
		"request_model",
		"route",
		"capture_endpoint",
		"capture_policy",
		"conformance_revision",
		"launch_profile_ref",
		"recipe_sha256",
		"interpreter",
		"wrapper",
		"models_config",
		"executable",
	})
	if err != nil {
		return config, errOMPNativeProofCustody
	}

	stringFields := []struct {
		key string
		out *string
	}{
		{"workspace", &config.Workspace},
		{"prompt", &config.Prompt},
		{"probe_root", &config.ProbeRoot},
		{"profile_id", &config.ProfileID},
		{"request_model", &config.RequestModel},
		{"route", &config.Route},
		{"capture_endpoint", &config.CaptureEndpoint},
		{"capture_policy", &config.CapturePolicy},
		{"conformance_revision", &config.ConformanceRevision},
		{"launch_profile_ref", &config.LaunchProfileRef},
		{"recipe_sha256", &config.RecipeSHA256},
	}
	for _, field := range stringFields {
		if json.Unmarshal(fields[field.key], field.out) != nil || strings.TrimSpace(*field.out) == "" {
			return ompNativeCustodyConfig{}, errOMPNativeProofCustody
		}
	}
	if !filepath.IsAbs(config.Workspace) ||
		filepath.Clean(config.Workspace) != config.Workspace ||
		!filepath.IsAbs(config.ProbeRoot) ||
		filepath.Clean(config.ProbeRoot) != config.ProbeRoot {
		return ompNativeCustodyConfig{}, errOMPNativeProofCustody
	}
	prompt := strings.TrimSpace(config.Prompt)
	if strings.HasPrefix(prompt, "-") || strings.HasPrefix(prompt, "@") ||
		!isOMPNativePublicProofLowerHex(config.RecipeSHA256, ompNativePublicProofDigestHexLen) ||
		!strings.HasPrefix(config.ConformanceRevision, "sha256:") ||
		!isOMPNativePublicProofLowerHex(
			strings.TrimPrefix(config.ConformanceRevision, "sha256:"),
			ompNativePublicProofDigestHexLen,
		) {
		return ompNativeCustodyConfig{}, errOMPNativeProofCustody
	}

	if json.Unmarshal(fields["fixed_argv"], &config.FixedArgv) != nil || len(config.FixedArgv) == 0 {
		return ompNativeCustodyConfig{}, errOMPNativeProofCustody
	}
	for _, arg := range config.FixedArgv {
		if arg == "" {
			return ompNativeCustodyConfig{}, errOMPNativeProofCustody
		}
	}
	for _, item := range []struct {
		key string
		out *ompNativeBinding
	}{
		{"interpreter", &config.Interpreter},
		{"wrapper", &config.Wrapper},
		{"models_config", &config.ModelsConfig},
		{"executable", &config.Executable},
	} {
		binding, bindingErr := decodeOMPNativeCustodyBinding(fields[item.key])
		if bindingErr != nil {
			return ompNativeCustodyConfig{}, errOMPNativeProofCustody
		}
		*item.out = binding
	}
	return config, nil
}

func decodeOMPNativeCustodyBinding(body []byte) (ompNativeBinding, error) {
	var binding ompNativeBinding
	fields, err := decodeOMPNativeCustodyObject(body, []string{"path", "sha256"})
	if err != nil ||
		json.Unmarshal(fields["path"], &binding.Path) != nil ||
		json.Unmarshal(fields["sha256"], &binding.SHA256) != nil ||
		!filepath.IsAbs(binding.Path) ||
		filepath.Clean(binding.Path) != binding.Path ||
		!isOMPNativePublicProofLowerHex(binding.SHA256, ompNativePublicProofDigestHexLen) {
		return ompNativeBinding{}, errOMPNativeProofCustody
	}
	return binding, nil
}

func decodeOMPNativeCustodyObject(body []byte, expectedKeys []string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(body) {
		return nil, errOMPNativeProofCustody
	}
	expected := make(map[string]struct{}, len(expectedKeys))
	for _, key := range expectedKeys {
		expected[key] = struct{}{}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errOMPNativeProofCustody
	}
	fields := make(map[string]json.RawMessage, len(expected))
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errOMPNativeProofCustody
		}
		if _, allowed := expected[key]; !allowed {
			return nil, errOMPNativeProofCustody
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, errOMPNativeProofCustody
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errOMPNativeProofCustody
		}
		fields[key] = value
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || len(fields) != len(expected) {
		return nil, errOMPNativeProofCustody
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, errOMPNativeProofCustody
	}
	return fields, nil
}

func ompNativeCustodyConfigMatchesProjection(config ompNativeCustodyConfig, projection OMPOwnerProjection) bool {
	return config.ProfileID == projection.Profile.ID &&
		config.RequestModel == projection.Profile.RequestModel &&
		config.Route == projection.Profile.Route &&
		config.CaptureEndpoint == projection.Profile.CaptureEndpoint &&
		config.CapturePolicy == projection.Profile.CapturePolicy.Mode &&
		config.ConformanceRevision == projection.ConformanceRevision &&
		config.LaunchProfileRef == projection.LaunchProfileRef &&
		projection.Profile.RequestedEffort == "high" &&
		reflectStringSliceEqual(config.FixedArgv, projection.PromptOnly.FixedArgv) &&
		config.Wrapper == (ompNativeBinding{
			Path: projection.PromptOnly.Wrapper.Path, SHA256: projection.PromptOnly.Wrapper.SHA256,
		}) &&
		config.ModelsConfig == (ompNativeBinding{
			Path: projection.PromptOnly.ModelsConfig.Path, SHA256: projection.PromptOnly.ModelsConfig.SHA256,
		}) &&
		config.Executable == (ompNativeBinding{
			Path: projection.PromptOnly.Executable.Path, SHA256: projection.PromptOnly.Executable.SHA256,
		})
}
