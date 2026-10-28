package act

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

type runtimeSourceAttempt struct {
	SchemaVersion string `json:"schema_version"`
	State         string `json:"state"`
	Binding       string `json:"binding_cid"`
	Action        string `json:"native_action_intent_id"`
	Policy        string `json:"native_policy_decision_id"`
	Receiver      string `json:"receiver_sha256"`
	Request       string `json:"request_sha256"`
	Sampler       string `json:"sampler_sha256"`
}
type runtimeSourceResult struct {
	SchemaVersion  string          `json:"schema_version"`
	State          string          `json:"state"`
	Binding        string          `json:"binding_cid"`
	Action         string          `json:"native_action_intent_id"`
	Policy         string          `json:"native_policy_decision_id"`
	Private        json.RawMessage `json:"private_artifact"`
	Public         json.RawMessage `json:"public_artifact"`
	LocalProfile   string          `json:"local_transport_custody_profile"`
	RemoteProfile  string          `json:"remote_origin_custody_profile"`
	TargetAccepted bool            `json:"target_projection_accepted"`
}
type runtimeProviderEquivalence struct {
	Different      []string `json:"different_provider_fields"`
	Equal          bool     `json:"exact_provider_field_names_and_values_equal"`
	Scope          string   `json:"scope"`
	TargetAccepted bool     `json:"target_projection_accepted"`
}
type runtimeSourcePublic struct {
	SchemaVersion  string                     `json:"schema_version"`
	State          string                     `json:"state"`
	Binding        string                     `json:"binding_cid"`
	Revision       string                     `json:"source_git_revision"`
	Origins        map[string]string          `json:"origin_hashes"`
	Fields         map[string][]string        `json:"service_selected_fields"`
	Provider       runtimeProviderEquivalence `json:"provider_equivalence"`
	WorkerObserved bool                       `json:"worker_command_observed"`
	WorkerFlags    int                        `json:"worker_known_flag_count"`
	SafeMatched    int                        `json:"old_safe_flags_matched"`
	SafeTotal      int                        `json:"old_safe_flags_total"`
	GoalDefined    bool                       `json:"goal_defined"`
	GoalAbsent     bool                       `json:"goal_expected_absent"`
	TargetAccepted bool                       `json:"target_projection_accepted"`
}
type runtimeSourceRequest struct {
	AttemptID  string         `json:"attempt_id"`
	Binding    map[string]any `json:"binding"`
	BindingCID string         `json:"binding_cid"`
	Machine    *string        `json:"expected_machine_id"`
	Boot       *string        `json:"expected_boot_id"`
	Action     *string        `json:"native_action_intent_id"`
	Policy     *string        `json:"native_policy_decision_id"`
	Receiver   string         `json:"receiver_sha256"`
	Sampler    string         `json:"sampler_sha256"`
}
type runtimeSourceCore struct {
	Argv       []string `json:"argv"`
	Retries    int      `json:"automatic_retries"`
	Manifest   string   `json:"manifest_sha256"`
	Supervisor int      `json:"outer_supervisor_seconds"`
	Template   string   `json:"request_template_sha256"`
}

type runtimeSourceContainer struct {
	ID             string  `json:"id"`
	ImageID        string  `json:"image_id"`
	ComposeProject *string `json:"compose_project"`
	ComposeService *string `json:"compose_service"`
}

// The source protocol has five explicit roles; nullable ownership is meaningful.
// Container names come from the protocol prefix, never from observed labels.
func runtimeCaptureContainerNames(prefix string) map[string]string {
	return map[string]string{"api": prefix + "-api", "worker": prefix + "-worker", "provider-sync": prefix + "-provider-sync", "web": prefix + "-web", "tunnel": prefix + "-cloudflared"}
}

func runtimeCaptureContainers(raw json.RawMessage, prefix string) (map[string]runtimeSourceContainer, error) {
	var records map[string]json.RawMessage
	names := runtimeCaptureContainerNames(prefix)
	if runtimeCaptureJSON(raw, &records) != nil || len(records) != len(names) {
		return nil, captureQualificationError()
	}
	out := map[string]runtimeSourceContainer{}
	seen := map[string]bool{}
	for role, name := range names {
		var c runtimeSourceContainer
		body, ok := records[name]
		if !ok || runtimeCaptureJSON(body, &c) != nil || !runtimeCaptureRequiredFields(body, reflect.TypeOf(c)) || !runtimeCaptureSHA(c.ID) || !strings.HasPrefix(c.ImageID, "sha256:") || !runtimeCaptureSHA(strings.TrimPrefix(c.ImageID, "sha256:")) || seen[c.ID] {
			return nil, captureQualificationError()
		}
		if role == "web" || role == "tunnel" {
			if c.ComposeProject != nil || c.ComposeService != nil {
				return nil, captureQualificationError()
			}
		} else if c.ComposeProject == nil || *c.ComposeProject != prefix || c.ComposeService == nil || *c.ComposeService != role {
			return nil, captureQualificationError()
		}
		seen[c.ID] = true
		out[role] = c
	}
	return out, nil
}

func runtimeCaptureServiceIdentity(raw json.RawMessage, expected runtimeSourceContainer, name string) bool {
	var identity struct {
		ID                string   `json:"id"`
		ImageID           string   `json:"image_id"`
		Name              string   `json:"name"`
		StartedAt         string   `json:"started_at"`
		Running           bool     `json:"running"`
		RestartCount      int      `json:"restart_count"`
		ComposeProject    *string  `json:"compose_project"`
		ComposeService    *string  `json:"compose_service"`
		ComposeFiles      []string `json:"compose_files"`
		ComposeWorkingDir *string  `json:"compose_working_dir"`
		OCIVersion        *string  `json:"oci_version"`
		OCIRevision       *string  `json:"oci_revision"`
	}
	return runtimeCaptureJSON(raw, &identity) == nil && runtimeCaptureRequiredFields(raw, reflect.TypeOf(identity)) &&
		identity.ID == expected.ID && identity.ImageID == expected.ImageID && identity.Name == name &&
		reflect.DeepEqual(identity.ComposeProject, expected.ComposeProject) && reflect.DeepEqual(identity.ComposeService, expected.ComposeService) &&
		identity.Running && identity.RestartCount >= 0 && identity.ComposeFiles != nil && regexp.MustCompile(`^[0-9TZ:.+-]{20,40}$`).MatchString(identity.StartedAt)
}

func runtimeCaptureSHA(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

// Source receiver canonicalization is sorted compact JSON, without HTML
// escaping. Its closed metadata protocol is ASCII; refuse other strings rather
// than silently invent a different hash. json.Number preserves nanoseconds.
func runtimeCaptureCanonical(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, captureQualificationError()
	}
	raw := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	for _, c := range raw {
		if c > 127 {
			return nil, captureQualificationError()
		}
	}
	return raw, nil
}
func runtimeCaptureJSON(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > 131072 || !credentialVerifyUniqueJSON(raw) {
		return captureQualificationError()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return captureQualificationError()
	}
	return nil
}
func runtimeCaptureCanonicalRaw(raw []byte) ([]byte, error) {
	var v any
	if runtimeCaptureJSON(raw, &v) != nil {
		return nil, captureQualificationError()
	}
	return runtimeCaptureCanonical(v)
}

// RawMessage fields carry source stat objects, not JSON arrays. Their closed
// field contract is checked by runtimeSourceStat after this presence check.
func runtimeCaptureRequiredFields(raw json.RawMessage, t reflect.Type) bool {
	if t == reflect.TypeOf(json.RawMessage{}) {
		return json.Valid(raw) && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	}
	if t.Kind() != reflect.Struct {
		return credentialVerifyRequiredFields(raw, t)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != t.NumField() {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		value, ok := fields[name]
		if !ok || !runtimeCaptureRequiredFields(value, field.Type) {
			return false
		}
	}
	return true
}
func runtimeConfigProjectionHash(selected map[string]string) string {
	raw, err := runtimeCaptureCanonical(selected)
	if err != nil {
		return ""
	}
	return runtimeConfigBytesHash(raw)
}

func runtimeSourceStat(raw json.RawMessage) (string, policy.RuntimeConfigInstallStat, error) {
	var fields map[string]json.RawMessage
	var zero policy.RuntimeConfigInstallStat
	if runtimeCaptureJSON(raw, &fields) != nil {
		return "", zero, captureQualificationError()
	}
	var name string
	if json.Unmarshal(fields["name"], &name) != nil {
		return "", zero, captureQualificationError()
	}
	delete(fields, "name")
	for _, key := range []string{"mtime_ns", "ctime_ns"} {
		var n json.Number
		if json.Unmarshal(fields[key], &n) != nil {
			return "", zero, captureQualificationError()
		}
		fields[key], _ = json.Marshal(n.String())
	}
	encoded, _ := json.Marshal(fields)
	if runtimeConfigStrict(encoded, &zero) != nil || !credentialVerifyRequiredFields(encoded, reflect.TypeOf(zero)) || !runtimeCaptureSHA(zero.SHA256) || zero.Links != 1 || zero.Mode != "0600" || zero.UID != uint32(os.Geteuid()) || zero.Bytes <= 0 || zero.Bytes > 131072 {
		return "", zero, captureQualificationError()
	}
	return name, zero, nil
}

func runtimeCaptureProfileValid(p policy.RuntimeConfigInstallProfile) bool {
	if !policy.ValidRuntimeConfigInstallModelEndpoint(p) {
		return false
	}
	key := regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,95}$`)
	if p.Ref != "" || p.SHA256 != "" || p.Basenames[0] != ".env" || len(p.APIFields) == 0 || len(p.APIFields) > 64 || !slices.IsSorted(p.APIFields) || len(p.Containers) == 0 || len(p.Containers) > 20 {
		return false
	}
	seen := map[string]bool{}
	for _, name := range p.Basenames {
		if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "\\\x00\r\n") || name == "." || name == ".." || seen[name] {
			return false
		}
		seen[name] = true
	}
	for _, name := range []string{p.AppKey, p.APIKey, p.MigratorKey, p.ProductionKey, p.LiveFetchKey, p.CheckoutKey, p.WebAPIKey, p.WebDealerKey, p.WebCookieKey, p.WebDealerSource} {
		if !key.MatchString(name) {
			return false
		}
	}
	seen = map[string]bool{}
	for _, name := range p.APIFields {
		if !key.MatchString(name) || seen[name] || name == p.AppKey || name == p.APIKey || name == p.MigratorKey || name == p.CheckoutKey || strings.Contains(name, "DATABASE") || strings.HasSuffix(name, "_DSN") {
			return false
		}
		seen[name] = true
	}
	if !seen[p.WebDealerSource] || !regexp.MustCompile(`^http://[a-z0-9][a-z0-9.-]{0,127}:[0-9]{2,5}$`).MatchString(p.WebAPIBase) || !regexp.MustCompile(`^__Host-[A-Za-z0-9_-]{1,63}$`).MatchString(p.WebCookieName) {
		return false
	}
	for _, group := range [][]string{{p.APIKey, p.ProductionKey, p.LiveFetchKey, p.CheckoutKey}, {p.WebAPIKey, p.WebDealerKey, p.WebCookieKey}} {
		seen := map[string]bool{}
		for _, key := range group {
			if seen[key] {
				return false
			}
			seen[key] = true
		}
	}
	for _, name := range p.Containers {
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`).MatchString(name) {
			return false
		}
	}
	return true
}

func (v *RuntimeConfigInstaller) validateCaptureQualification(r RuntimeCaptureQualificationRequest, workID string) (runtimeCaptureValidated, error) {
	var out runtimeCaptureValidated
	bad := captureQualificationError()
	if r.SchemaVersion != runtimeCaptureRequestSchema || !runtimeCaptureProfileValid(r.Profile) || !filepath.IsAbs(r.CapturePath) || filepath.Clean(r.CapturePath) != r.CapturePath || r.CaptureStat.Mode != "0600" || r.CaptureStat.Links != 1 || r.CaptureStat.UID != uint32(os.Geteuid()) || !runtimeCaptureSHA(r.CaptureStat.SHA256) {
		return out, bad
	}
	root := filepath.Dir(r.CapturePath)
	if filepath.Base(r.CapturePath) != "capture.private.json" || r.Attempt.Path != filepath.Join(root, "ATTEMPT.json") || r.Result.Path != filepath.Join(root, "RESULT.json") || r.Public.Path != filepath.Join(root, "capture.public.json") {
		return out, bad
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 4 {
		return out, bad
	}
	for _, entry := range entries {
		if entry.IsDir() || !slices.Contains([]string{"capture.private.json", "capture.public.json", "ATTEMPT.json", "RESULT.json"}, entry.Name()) {
			return out, bad
		}
	}
	var sourceAI envelope.ActionIntent
	var sourcePD envelope.PolicyDecision
	if _, err = v.proof(r.SourceAction, false, &sourceAI); err != nil {
		return out, bad
	}
	if _, err = v.proof(r.SourcePolicy, false, &sourcePD); err != nil {
		return out, bad
	}
	if sourceAI.Validate() != nil || sourcePD.Validate() != nil || sourceAI.ID != r.SourceAction.Ref || sourcePD.ID != r.SourcePolicy.Ref || sourcePD.ActionIntentID != sourceAI.ID || !policy.Allowed(sourcePD) || sourceAI.Capability != "deploy.run" || sourceAI.MutationClass != "external_side_effect" || sourceAI.Target.Kind != "runtime" || sourceAI.Target.Path != root || sourcePD.IDs.WorkID != sourceAI.IDs.WorkID {
		return out, bad
	}
	ws, err := work.OpenReadOnly(v.home)
	if err != nil {
		return out, bad
	}
	sourceWork, err := ws.Get(sourceAI.IDs.WorkID)
	if err != nil || !slices.Contains(sourceWork.ActionIntentIDs, sourceAI.ID) {
		return out, bad
	}
	inputs := []RuntimeCaptureFile{r.Receiver, r.Sampler, r.Request, r.RequestTemplate, r.Manifest, r.Attempt, r.Result, r.Public}
	data := make([][]byte, len(inputs))
	for i, f := range inputs {
		data[i], err = runtimeCaptureReadPinned(f)
		if err != nil {
			return out, bad
		}
	}
	// These exact pins were already authorized in the original native action.
	// The new approval qualifies their retained output; it never executes code.
	for _, pin := range []string{"ReceiverSHA256 " + r.Receiver.SHA256 + ";", "samplerSHA256 " + r.Sampler.SHA256 + ";"} {
		if strings.Count(sourceAI.Action, pin) != 1 {
			return out, bad
		}
	}
	if strings.Count(sourceAI.Action, "Exact core ") != 1 {
		return out, bad
	}
	coreRaw := []byte(strings.SplitN(sourceAI.Action, "Exact core ", 2)[1])
	var core runtimeSourceCore
	if runtimeConfigStrict(coreRaw, &core) != nil || !runtimeCaptureRequiredFields(coreRaw, reflect.TypeOf(core)) || core.Retries != 0 || core.Supervisor != 70 || core.Manifest != r.Manifest.SHA256 || core.Template != r.RequestTemplate.SHA256 || !slices.Equal(core.Argv, []string{"/usr/bin/python3", "-I", "-B", r.Receiver.Path, "--request", r.Request.Path}) || !slices.Contains(sourceAI.EvidenceRefs, r.Manifest.Path) {
		return out, bad
	}
	var request, template runtimeSourceRequest
	if !runtimeCaptureRequiredFields(data[2], reflect.TypeOf(request)) || !runtimeCaptureRequiredFields(data[3], reflect.TypeOf(template)) {
		return out, bad
	}
	if runtimeCaptureJSON(data[2], &request) != nil || runtimeCaptureJSON(data[3], &template) != nil || request.Action == nil || request.Policy == nil || *request.Action != sourceAI.ID || *request.Policy != sourcePD.ID || template.Action != nil || template.Policy != nil || request.Receiver != r.Receiver.SHA256 || request.Sampler != r.Sampler.SHA256 {
		return out, bad
	}
	compare := request
	compare.Action = nil
	compare.Policy = nil
	if !reflect.DeepEqual(compare, template) {
		return out, bad
	}
	bindingRaw, err := runtimeCaptureCanonical(request.Binding)
	if err != nil || runtimeConfigBytesHash(bindingRaw) != request.BindingCID || strings.Count(sourceAI.Action, "bindingCID "+request.BindingCID+";") != 1 {
		return out, bad
	}
	requestRaw, err := runtimeCaptureCanonicalRaw(data[2])
	if err != nil {
		return out, bad
	}
	requestHash := runtimeConfigBytesHash(requestRaw)
	prefix, ok := request.Binding["schema_version"].(string)
	if !ok || !strings.HasSuffix(prefix, ".readonly_source_capture_binding.v1") {
		return out, bad
	}
	prefix = strings.TrimSuffix(prefix, ".readonly_source_capture_binding.v1")
	if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`).MatchString(prefix) {
		return out, bad
	}
	var binding struct {
		Root struct {
			Path string `json:"path"`
			UID  uint32 `json:"uid"`
			GID  uint32 `json:"gid"`
		} `json:"mac_transport_root"`
		Source struct {
			Revision   string          `json:"git_revision"`
			Containers json.RawMessage `json:"containers"`
		} `json:"source"`
	}
	if json.Unmarshal(bindingRaw, &binding) != nil || binding.Root.Path != filepath.Dir(root) || binding.Root.UID != r.CaptureStat.UID || binding.Root.GID != r.CaptureStat.GID || !strings.HasSuffix(root, request.AttemptID) || request.AttemptID == "" {
		return out, bad
	}
	containers, err := runtimeCaptureContainers(binding.Source.Containers, prefix)
	if err != nil {
		return out, bad
	}
	var manifest struct {
		Binding string `json:"binding_cid"`
		Files   []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if json.Unmarshal(data[4], &manifest) != nil || !credentialVerifyUniqueJSON(data[4]) || manifest.Binding != request.BindingCID {
		return out, bad
	}
	for _, f := range []RuntimeCaptureFile{r.Receiver, r.Sampler, r.RequestTemplate} {
		count := 0
		for _, mf := range manifest.Files {
			if mf.Path == filepath.Base(f.Path) && mf.SHA256 == f.SHA256 {
				count++
			}
		}
		if count != 1 || filepath.Dir(f.Path) != filepath.Dir(r.Manifest.Path) {
			return out, bad
		}
	}
	var attempt runtimeSourceAttempt
	var result runtimeSourceResult
	var public runtimeSourcePublic
	if runtimeConfigStrict(data[5], &attempt) != nil || runtimeConfigStrict(data[6], &result) != nil || runtimeConfigStrict(data[7], &public) != nil || !runtimeCaptureRequiredFields(data[5], reflect.TypeOf(attempt)) || !runtimeCaptureRequiredFields(data[6], reflect.TypeOf(result)) || !runtimeCaptureRequiredFields(data[7], reflect.TypeOf(public)) {
		return out, bad
	}
	if attempt.SchemaVersion != prefix+".source_capture_attempt.v1" || attempt.State != "source_capture_pending_no_replay" || attempt.Binding != request.BindingCID || attempt.Action != sourceAI.ID || attempt.Policy != sourcePD.ID || attempt.Receiver != r.Receiver.SHA256 || attempt.Sampler != r.Sampler.SHA256 || attempt.Request != requestHash {
		return out, bad
	}
	if result.SchemaVersion != prefix+".source_capture_result.v1" || result.State != "protected_source_capture_not_projected" || result.TargetAccepted || result.Binding != request.BindingCID || result.Action != sourceAI.ID || result.Policy != sourcePD.ID || result.LocalProfile != fmt.Sprintf("actual-mac%d-selected-json-v1", os.Geteuid()) || result.RemoteProfile != "actual-source-origin-v1" {
		return out, bad
	}
	privateName, privateStat, err := runtimeSourceStat(result.Private)
	if err != nil || privateName != "capture.private.json" || privateStat != r.CaptureStat {
		return out, bad
	}
	publicName, publicStat, err := runtimeSourceStat(result.Public)
	if err != nil || publicName != "capture.public.json" || publicStat.SHA256 != r.Public.SHA256 {
		return out, bad
	}
	if _, err = runtimeConfigReadCapture(policy.RuntimeConfigInstallCapture{Path: r.Public.Path, Stat: publicStat}); err != nil {
		return out, bad
	}
	if public.SchemaVersion != prefix+".source_runtime_capture_public.v1" || public.State != "source_captured_protected_not_projected" || public.Binding != request.BindingCID || public.TargetAccepted || !public.WorkerObserved || !public.GoalDefined || !public.GoalAbsent || public.WorkerFlags <= 0 || public.SafeMatched != public.SafeTotal || public.SafeTotal <= 0 || public.Revision != binding.Source.Revision || !slices.Equal(public.Fields["api"], r.Profile.APIFields) {
		return out, bad
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(public.Revision) {
		return out, bad
	}
	for service, fields := range public.Fields {
		if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`).MatchString(service) || len(fields) > 64 || !slices.IsSorted(fields) {
			return out, bad
		}
		seen := map[string]bool{}
		for _, field := range fields {
			if !regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,95}$`).MatchString(field) || seen[field] {
				return out, bad
			}
			seen[field] = true
		}
	}
	for name := range public.Origins {
		if !slices.Contains([]string{"compose-base", "compose-override", "source-app", "source-db"}, name) {
			return out, bad
		}
	}
	private, err := runtimeConfigReadCapture(policy.RuntimeConfigInstallCapture{Path: r.CapturePath, Stat: r.CaptureStat})
	if err != nil {
		return out, bad
	}
	defer clear(private)
	projection, err := runtimeCaptureValidatePrivate(private, public, prefix, containers)
	if err != nil {
		return out, bad
	}
	out.metadata = map[string][]byte{}
	ref := func(raw []byte) policy.RuntimeConfigInstallProofRef {
		h := runtimeConfigBytesHash(raw)
		r := policy.RuntimeConfigInstallProofRef{Ref: home.ArtifactRefPrefix + h, SHA256: h}
		out.metadata[r.Ref] = raw
		return r
	}
	profileRaw, _ := json.Marshal(r.Profile)
	profileRef := ref(profileRaw)
	out.profile = r.Profile
	out.profile.Ref = profileRef.Ref
	out.profile.SHA256 = profileRef.SHA256
	out.qualification = RuntimeConfigCaptureQualification{SchemaVersion: "compound.runtime_config_capture_qualification.v0.2", WorkID: workID, SourceInputSHA256: runtimeConfigInstallHash(r), SourceAction: r.SourceAction, SourcePolicy: r.SourcePolicy, SourceAttempt: ref(data[5]), SourceResult: ref(data[6]), SourcePublic: ref(data[7]), SourceBinding: request.BindingCID, ReceiverSHA256: r.Receiver.SHA256, SamplerSHA256: r.Sampler.SHA256, RequestSHA256: requestHash, CapturePath: r.CapturePath, CaptureStat: r.CaptureStat, ProfileSHA256: profileRef.SHA256, ProjectionSHA256: projection}
	return out, nil
}

func runtimeCaptureValidatePrivate(raw []byte, public runtimeSourcePublic, prefix string, containers map[string]runtimeSourceContainer) (string, error) {
	bad := captureQualificationError()
	if !credentialVerifyUniqueJSON(raw) {
		return "", bad
	}
	var p struct {
		Schema   string `json:"schema_version"`
		State    string `json:"state"`
		Binding  string `json:"binding_cid"`
		Revision string `json:"source_git_revision"`
		Origins  map[string]struct {
			SHA string `json:"raw_sha256"`
		} `json:"origins"`
		Services map[string]struct {
			Identity json.RawMessage `json:"identity"`
			Record   struct {
				Selected map[string]string `json:"selected"`
			} `json:"record"`
		} `json:"services"`
		Provider runtimeProviderEquivalence `json:"provider_equivalence"`
		Worker   struct {
			Flags   int `json:"known_flags"`
			Matched int `json:"old_safe_flags_matched"`
			Total   int `json:"old_safe_flags_total"`
		} `json:"worker_command"`
		Goal struct {
			Defined bool `json:"defined"`
		} `json:"goal"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Schema != prefix+".source_runtime_selected_capture.v1" || p.State != "consistent_source_capture_not_projected" || p.Binding != public.Binding || p.Revision != public.Revision || p.Worker.Flags != public.WorkerFlags || p.Worker.Matched != public.SafeMatched || p.Worker.Total != public.SafeTotal || !p.Goal.Defined || !reflect.DeepEqual(p.Provider, public.Provider) || !p.Provider.Equal || len(p.Provider.Different) != 0 || p.Provider.TargetAccepted || p.Provider.Scope != "api_worker_provider_sync_runtime_values_only" {
		return "", bad
	}
	if len(p.Origins) != len(public.Origins) || len(p.Services) != len(public.Fields) || len(containers) != len(p.Services) {
		return "", bad
	}
	for name, origin := range p.Origins {
		if !runtimeCaptureSHA(origin.SHA) || origin.SHA != public.Origins[name] {
			return "", bad
		}
	}
	for service, expected := range containers {
		captured, ok := p.Services[service]
		if !ok || !runtimeCaptureServiceIdentity(captured.Identity, expected, runtimeCaptureContainerNames(prefix)[service]) {
			return "", bad
		}
	}
	for name, service := range p.Services {
		keys := make([]string, 0, len(service.Record.Selected))
		for k := range service.Record.Selected {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !slices.Equal(keys, public.Fields[name]) {
			return "", bad
		}
	}
	api := p.Services["api"].Record.Selected
	worker := p.Services["worker"].Record.Selected
	provider := p.Services["provider-sync"].Record.Selected
	if len(api) == 0 || len(provider) == 0 || len(worker) != len(provider) {
		return "", bad
	}
	for k, value := range provider {
		if api[k] != value || worker[k] != value {
			return "", bad
		}
	}
	for _, value := range api {
		if !runtimeConfigDotenvValue(value) {
			return "", bad
		}
	}
	hash := runtimeConfigProjectionHash(api)
	if hash == "" {
		return "", bad
	}
	return hash, nil
}
