package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
)

const (
	RuntimeConfigInstallSchema      = "compound.runtime_config_installation_request.v0.2"
	RuntimeConfigInstallCapability  = "runtime.config_installation.exact"
	RuntimeConfigRecoveryCapability = "runtime.config_recovery.exact"
	RuntimeConfigInstallAction      = "install_inactive_runtime_configuration"
	RuntimeConfigInstallTarget      = "runtime_configuration_installation"
	RuntimeConfigInstallEffects     = "inactive-aws-staging-runtime-configuration-v2"
	RuntimeConfigInstallApprovalTTL = 15 * time.Minute
)

// RuntimeConfigInstallRequest contains references and nonsecret metadata only.
// It has no credential, command, query, transport selector or export field.
// Nanosecond timestamps are decimal strings: JSON float coercion loses identity.
type RuntimeConfigInstallRequest struct {
	SchemaVersion  string                            `json:"schema_version"`
	ProfileID      string                            `json:"profile_id"`
	AttemptID      string                            `json:"attempt_id"`
	ProgramSHA256  string                            `json:"program_sha256"`
	EffectSet      string                            `json:"effect_set"`
	Witness        RuntimeConfigInstallWitness       `json:"witness"`
	Resource       RuntimeConfigInstallResource      `json:"resource"`
	SSH            RuntimeConfigInstallSSH           `json:"ssh"`
	Host           RuntimeConfigInstallHost          `json:"host"`
	Files          [4]RuntimeConfigInstallFile       `json:"files"`
	Roles          [2]RuntimeConfigInstallRole       `json:"roles"`
	Database       RuntimeConfigInstallDatabase      `json:"database"`
	Posture        RuntimeConfigInstallPosture       `json:"posture"`
	Root           RuntimeConfigInstallRoot          `json:"root"`
	Marker         RuntimeConfigInstallAnchor        `json:"marker"`
	Lock           RuntimeConfigInstallAnchor        `json:"lock"`
	Profile        RuntimeConfigInstallProfile       `json:"profile"`
	Capture        RuntimeConfigInstallCapture       `json:"capture"`
	Qualification  RuntimeConfigInstallQualification `json:"qualification"`
	ForcedInactive bool                              `json:"forced_inactive"`
}

type RuntimeConfigInstallWitness struct {
	Ref        string `json:"ref"`
	SHA256     string `json:"sha256"`
	ObservedAt string `json:"observed_at"`
}
type RuntimeConfigInstallResource struct {
	Account  string `json:"account"`
	Region   string `json:"region"`
	Instance string `json:"instance"`
	Address  string `json:"address"`
}
type RuntimeConfigInstallSSH struct {
	Alias             string `json:"alias"`
	User              string `json:"user"`
	Port              int    `json:"port"`
	IdentityFile      string `json:"identity_file"`
	KnownHostsFile    string `json:"known_hosts_file"`
	KnownHostsSHA256  string `json:"known_hosts_sha256"`
	HostKeySHA256     string `json:"host_key_sha256"`
	InterpreterSHA256 string `json:"interpreter_sha256"`
	ClientPath        string `json:"client_path"`
	ClientSHA256      string `json:"client_sha256"`
	ClientVersion     string `json:"client_version"`
	SudoSHA256        string `json:"sudo_sha256"`
}
type RuntimeConfigInstallHost struct {
	Hostname         string `json:"hostname"`
	MachineID        string `json:"machine_id"`
	BootID           string `json:"boot_id"`
	SystemIdentifier string `json:"system_identifier"`
	VersionNum       string `json:"version_num"`
}

// File absence is explicit; a nonexistent file never has a fabricated inode.
type RuntimeConfigInstallFile struct {
	Slot     string                    `json:"slot"`
	Basename string                    `json:"basename"`
	Path     string                    `json:"path"`
	Exists   bool                      `json:"exists"`
	Stat     *RuntimeConfigInstallStat `json:"stat"`
}
type RuntimeConfigInstallStat struct {
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	Bytes   int64  `json:"bytes"`
	MtimeNS string `json:"mtime_ns"`
	CtimeNS string `json:"ctime_ns"`
	Mode    string `json:"mode"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
	Links   uint64 `json:"links"`
	SHA256  string `json:"sha256"`
}
type RuntimeConfigInstallRoot struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mode   string `json:"mode"`
}
type RuntimeConfigInstallAnchor struct {
	Basename string                   `json:"basename"`
	Stat     RuntimeConfigInstallStat `json:"stat"`
}

// Ref and SHA bind the frozen profile artifact; all other fields are metadata.
type RuntimeConfigInstallProfile struct {
	Ref             string                             `json:"ref"`
	SHA256          string                             `json:"sha256"`
	Basenames       [4]string                          `json:"basenames"`
	AppKey          string                             `json:"app_key"`
	APIKey          string                             `json:"api_key"`
	MigratorKey     string                             `json:"migrator_key"`
	ProductionKey   string                             `json:"production_key"`
	LiveFetchKey    string                             `json:"live_fetch_key"`
	CheckoutKey     string                             `json:"checkout_key"`
	APIFields       []string                           `json:"api_fields"`
	APIEndpoint     *RuntimeConfigInstallModelEndpoint `json:"api_endpoint"`
	WebAPIKey       string                             `json:"web_api_key"`
	WebDealerKey    string                             `json:"web_dealer_key"`
	WebCookieKey    string                             `json:"web_cookie_key"`
	WebAPIBase      string                             `json:"web_api_base"`
	WebCookieName   string                             `json:"web_cookie_name"`
	WebDealerSource string                             `json:"web_dealer_source"`
	Containers      []string                           `json:"containers"`
}
type RuntimeConfigInstallProofRef struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

// Native qualification binds the complete verifier chain, not a standalone receipt.
type RuntimeConfigInstallQualification struct {
	Plan        RuntimeConfigInstallProofRef `json:"plan"`
	Receipt     RuntimeConfigInstallProofRef `json:"receipt"`
	Transition  RuntimeConfigInstallProofRef `json:"transition"`
	Attempt     RuntimeConfigInstallProofRef `json:"attempt"`
	Observation RuntimeConfigInstallProofRef `json:"observation"`
}

// The qualification artifact contains the original source Action/Policy and
// frozen receiver/request/result/custody evidence. No source body is in a plan.
type RuntimeConfigInstallCapture struct {
	Qualification    RuntimeConfigInstallProofRef `json:"qualification"`
	Receipt          RuntimeConfigInstallProofRef `json:"receipt"`
	Transition       RuntimeConfigInstallProofRef `json:"transition"`
	Path             string                       `json:"path"`
	Stat             RuntimeConfigInstallStat     `json:"stat"`
	ProjectionSHA256 string                       `json:"projection_sha256"`
}
type RuntimeConfigInstallRole struct {
	Kind           string `json:"kind"`
	Key            string `json:"key"`
	Name           string `json:"name"`
	SourceHost     string `json:"source_host"`
	SourceDatabase string `json:"source_database"`
}
type RuntimeConfigInstallDatabase struct {
	Name             string `json:"name"`
	AllowConnections bool   `json:"allow_connections"`
	EndpointHost     string `json:"endpoint_host"`
	EndpointPort     int    `json:"endpoint_port"`
	Scheme           string `json:"scheme"`
}
type RuntimeConfigInstallPosture struct {
	Production      bool `json:"production"`
	LiveFetch       bool `json:"live_fetch"`
	CheckoutEnabled bool `json:"checkout_enabled"`
}

var runtimeConfigInstallSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
var runtimeConfigInstallName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
var runtimeConfigInstallLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)
var runtimeConfigInstallKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
var runtimeConfigInstallAttempt = regexp.MustCompile(`^[0-9]{8}t[0-9]{6}z-[0-9a-f]{16}$`)
var runtimeConfigInstallCookie = regexp.MustCompile(`^__Host-[A-Za-z0-9_-]{1,63}$`)
var runtimeConfigInstallURL = regexp.MustCompile(`^http://[a-z0-9][a-z0-9.-]{0,127}:[0-9]{2,5}$`)

func runtimeConfigInstallRefusal(code string) error {
	return errors.New("runtime_config_install:" + code)
}

func DecodeRuntimeConfigInstallRequest(raw []byte) (RuntimeConfigInstallRequest, error) {
	var r RuntimeConfigInstallRequest
	if len(raw) == 0 || len(raw) > 65536 {
		return r, runtimeConfigInstallRefusal("request_size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return r, runtimeConfigInstallRefusal("request_shape")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return r, runtimeConfigInstallRefusal("request_trailing")
	}
	var shape struct {
		Roles []json.RawMessage `json:"roles"`
		Files []json.RawMessage `json:"files"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Roles) != 2 || len(shape.Files) != 4 {
		return r, runtimeConfigInstallRefusal("role_or_file_count")
	}
	if !credentialVerifyUniqueJSON(raw) || !runtimeConfigRequiredFields(raw, reflect.TypeOf(r)) {
		return r, runtimeConfigInstallRefusal("request_duplicate")
	}
	return r, nil
}

func RuntimeConfigInstallDigest(r RuntimeConfigInstallRequest) string {
	raw, _ := json.Marshal(r)
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&value)
	raw, _ = json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func RuntimeConfigInstallDestination(r RuntimeConfigInstallRequest) string {
	return "ssh://" + r.SSH.User + "@" + r.SSH.Alias + ":22"
}

func ValidateRuntimeConfigInstallRequest(r RuntimeConfigInstallRequest, programSHA string, now time.Time) error {
	return validateRuntimeConfigRequest(r, programSHA, now, false)
}

func ValidateRuntimeConfigRecoveryRequest(r RuntimeConfigInstallRequest, programSHA string, now time.Time) error {
	return validateRuntimeConfigRequest(r, programSHA, now, true)
}

func validateRuntimeConfigRequest(r RuntimeConfigInstallRequest, programSHA string, now time.Time, recovery bool) error {
	bad := func(code string) error { return runtimeConfigInstallRefusal(code) }
	if r.SchemaVersion != RuntimeConfigInstallSchema || r.EffectSet != RuntimeConfigInstallEffects || !runtimeConfigInstallLabel.MatchString(r.ProfileID) || !runtimeConfigInstallAttempt.MatchString(r.AttemptID) {
		return bad("protocol")
	}
	attemptTime, ae := time.Parse("20060102t150405z", r.AttemptID[:16])
	if ae != nil || attemptTime.After(now) || (!recovery && now.Sub(attemptTime) > RuntimeConfigInstallApprovalTTL) {
		return bad("attempt_stale")
	}
	if !runtimeConfigInstallSHA.MatchString(programSHA) || r.ProgramSHA256 != programSHA {
		return bad("program")
	}
	if !regexp.MustCompile(`^(find|rec|obs)_[a-zA-Z0-9_]{8,100}$`).MatchString(r.Witness.Ref) || !runtimeConfigInstallSHA.MatchString(r.Witness.SHA256) {
		return bad("witness")
	}
	observed, e := time.Parse(time.RFC3339Nano, r.Witness.ObservedAt)
	if e != nil || observed.After(now) || (!recovery && now.Sub(observed) > RuntimeConfigInstallApprovalTTL) {
		return bad("witness_stale")
	}
	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(r.Resource.Account) || !runtimeConfigInstallLabel.MatchString(r.Resource.Region) || !regexp.MustCompile(`^[a-zA-Z0-9:/_.-]{8,256}$`).MatchString(r.Resource.Instance) {
		return bad("resource")
	}
	if r.Posture.Production != true || r.Posture.LiveFetch || r.Posture.CheckoutEnabled || !r.ForcedInactive {
		return bad("posture")
	}
	s := r.SSH
	if s.IdentityFile == s.KnownHostsFile {
		return bad("ssh_custody_alias")
	}
	if !runtimeConfigInstallLabel.MatchString(s.Alias) || !runtimeConfigInstallName.MatchString(s.User) || s.Port != 22 || !runtimeConfigInstallAbsolute(s.IdentityFile) || !runtimeConfigInstallAbsolute(s.KnownHostsFile) || !runtimeConfigInstallSHA.MatchString(s.KnownHostsSHA256) || !regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`).MatchString(s.HostKeySHA256) {
		return bad("ssh_identity")
	}
	if !regexp.MustCompile(`^/usr/(?:lib/postgresql/[0-9]{1,2}/bin|bin)/psql$`).MatchString(s.ClientPath) || !credentialVerifyClientVersion(s.ClientVersion) {
		return bad("client")
	}
	for _, h := range []string{s.InterpreterSHA256, s.ClientSHA256, s.SudoSHA256} {
		if !runtimeConfigInstallSHA.MatchString(h) {
			return bad("digest")
		}
	}
	h := r.Host
	if !runtimeConfigInstallLabel.MatchString(h.Hostname) || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(h.MachineID) || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(h.BootID) || !regexp.MustCompile(`^[0-9]{10,20}$`).MatchString(h.SystemIdentifier) || !regexp.MustCompile(`^[0-9]{6}$`).MatchString(h.VersionNum) {
		return bad("host_identity")
	}
	if !runtimeConfigInstallAbsolute(r.Root.Path) || r.Root.Device == 0 || r.Root.Inode == 0 || (r.Root.Mode != "0700" && r.Root.Mode != "0750" && r.Root.Mode != "0755") {
		return bad("root")
	}
	seen := map[string]bool{}
	for i, f := range r.Files {
		if f.Slot != []string{"app", "api", "migrator", "web"}[i] || !runtimeConfigInstallBasename(f.Basename) || seen[f.Basename] || f.Basename != r.Profile.Basenames[i] || f.Path != filepath.Join(r.Root.Path, f.Basename) || f.Exists != (i == 0) || (f.Stat != nil) != f.Exists {
			return bad("file_identity")
		}
		seen[f.Basename] = true
		if f.Stat != nil && (!runtimeConfigInstallValidStat(*f.Stat, true) || f.Stat.Device != r.Root.Device || f.Stat.Bytes == 0) {
			return bad("file_stat")
		}
	}
	if r.Files[0].Basename != ".env" {
		return bad("app_basename")
	}
	for _, anchor := range []RuntimeConfigInstallAnchor{r.Marker, r.Lock} {
		if !runtimeConfigInstallBasename(anchor.Basename) || seen[anchor.Basename] || strings.HasPrefix(anchor.Basename, ".rci-") || !runtimeConfigInstallValidStat(anchor.Stat, true) || anchor.Stat.Device != r.Root.Device {
			return bad("anchor")
		}
		seen[anchor.Basename] = true
	}
	p := r.Profile
	if !ValidRuntimeConfigInstallModelEndpoint(p) || (p.APIEndpoint != nil && (p.APIEndpoint.Key == r.Roles[0].Key || p.APIEndpoint.Key == r.Roles[1].Key)) {
		return bad("model_endpoint")
	}
	if !runtimeConfigInstallRef(p.Ref) || !runtimeConfigInstallSHA.MatchString(p.SHA256) || !runtimeConfigInstallURL.MatchString(p.WebAPIBase) || !runtimeConfigInstallCookie.MatchString(p.WebCookieName) {
		return bad("profile")
	}
	for _, key := range []string{p.AppKey, p.APIKey, p.MigratorKey, p.ProductionKey, p.LiveFetchKey, p.CheckoutKey, p.WebAPIKey, p.WebDealerKey, p.WebCookieKey, p.WebDealerSource} {
		if !runtimeConfigInstallKey.MatchString(key) {
			return bad("profile_key")
		}
	}
	if p.APIKey == p.ProductionKey || p.APIKey == p.LiveFetchKey || p.APIKey == p.CheckoutKey || p.ProductionKey == p.LiveFetchKey || p.ProductionKey == p.CheckoutKey || p.LiveFetchKey == p.CheckoutKey || p.WebAPIKey == p.WebDealerKey || p.WebAPIKey == p.WebCookieKey || p.WebDealerKey == p.WebCookieKey {
		return bad("profile_collision")
	}
	keys := map[string]bool{}
	if len(p.APIFields) == 0 || len(p.APIFields) > 64 {
		return bad("api_roster")
	}
	for _, key := range p.APIFields {
		if !runtimeConfigInstallKey.MatchString(key) || keys[key] || key == p.AppKey || key == p.APIKey || key == p.MigratorKey || key == p.CheckoutKey || key == r.Roles[0].Key || key == r.Roles[1].Key || strings.Contains(key, "DATABASE") || strings.HasSuffix(key, "_DSN") {
			return bad("api_roster")
		}
		keys[key] = true
	}
	if !keys[p.WebDealerSource] {
		return bad("web_dealer_source")
	}
	if len(p.Containers) == 0 || len(p.Containers) > 20 {
		return bad("container_roster")
	}
	seen = map[string]bool{}
	for _, name := range p.Containers {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,100}$`).MatchString(name) || seen[name] {
			return bad("container_roster")
		}
		seen[name] = true
	}
	for _, ref := range []RuntimeConfigInstallProofRef{r.Qualification.Plan, r.Qualification.Receipt, r.Qualification.Transition, r.Qualification.Attempt, r.Qualification.Observation, r.Capture.Qualification, r.Capture.Receipt, r.Capture.Transition} {
		if !runtimeConfigInstallRef(ref.Ref) || !runtimeConfigInstallSHA.MatchString(ref.SHA256) {
			return bad("qualification_ref")
		}
	}
	if !runtimeConfigInstallAbsolute(r.Capture.Path) || !runtimeConfigInstallValidStat(r.Capture.Stat, false) || r.Capture.Stat.Bytes == 0 || r.Capture.Stat.Bytes > 131072 || !runtimeConfigInstallSHA.MatchString(r.Capture.ProjectionSHA256) {
		return bad("capture_custody")
	}
	for i, role := range r.Roles {
		kind := []string{"application", "migrator"}[i]
		if role.Kind != kind || !runtimeConfigInstallKey.MatchString(role.Key) || !runtimeConfigInstallName.MatchString(role.Name) || !runtimeConfigInstallLabel.MatchString(role.SourceHost) || !runtimeConfigInstallName.MatchString(role.SourceDatabase) {
			return bad("role_identity")
		}
	}
	if r.Roles[0].Key == r.Roles[1].Key || r.Roles[0].Name == r.Roles[1].Name || !runtimeConfigInstallName.MatchString(r.Database.Name) || r.Database.Name == "postgres" {
		return bad("role_pair")
	}
	if r.Database.EndpointHost == "" || r.Database.EndpointPort <= 0 || r.Database.EndpointPort > 65535 || (r.Database.Scheme != "postgresql" && r.Database.Scheme != "postgresql+psycopg") {
		return bad("endpoint")
	}
	if !runtimeConfigInstallLabel.MatchString(r.Database.EndpointHost) {
		return bad("endpoint_host")
	}
	return nil
}

func runtimeConfigInstallRef(s string) bool {
	return len(s) > 3 && len(s) < 200 && regexp.MustCompile(`^[a-zA-Z0-9_:/.-]+$`).MatchString(s)
}
func runtimeConfigInstallBasename(s string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`).MatchString(s) && s != "." && s != ".." && !strings.HasPrefix(s, ".rci-")
}
func runtimeConfigInstallValidStat(s RuntimeConfigInstallStat, root bool) bool {
	if s.Device == 0 || s.Inode == 0 || s.Bytes < 0 || s.Bytes > 131072 || s.Mode != "0600" || s.Links != 1 || !runtimeConfigInstallSHA.MatchString(s.SHA256) || root && (s.UID != 0 || s.GID != 0) {
		return false
	}
	for _, ts := range []string{s.MtimeNS, s.CtimeNS} {
		n, e := strconv.ParseInt(ts, 10, 64)
		if e != nil || n <= 0 || strconv.FormatInt(n, 10) != ts {
			return false
		}
	}
	return true
}

func runtimeConfigInstallAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && regexp.MustCompile(`^/[A-Za-z0-9_./@+-]+$`).MatchString(path) && !strings.Contains(path, "//") && len(path) <= 1024
}

// EvaluateRuntimeConfigInstall is owner-specific, never a generic read/path
// exemption. A valid protocol can at most become HOLD for exact native approval.
func EvaluateRuntimeConfigInstall(ai envelope.ActionIntent, r RuntimeConfigInstallRequest, programSHA string, cfg Config, ctx EvaluationContext, now time.Time) (envelope.PolicyDecision, bool) {
	return evaluateRuntimeConfig(ai, r, programSHA, cfg, ctx, now, false, RuntimeConfigInstallAction, "sha256:"+RuntimeConfigInstallDigest(r))
}

// Recovery binds the original failed owner identity and requested operation;
// the native owner additionally verifies its receipt/transition and reservation.
func RuntimeConfigRecoveryDigest(r RuntimeConfigInstallRequest, originalAction, originalAttempt, operation string) string {
	b, _ := json.Marshal(struct{ Request, Action, Attempt, Operation string }{RuntimeConfigInstallDigest(r), originalAction, originalAttempt, operation})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func EvaluateRuntimeConfigRecovery(ai envelope.ActionIntent, r RuntimeConfigInstallRequest, originalAction, originalAttempt, operation, programSHA string, cfg Config, ctx EvaluationContext, now time.Time) (envelope.PolicyDecision, bool) {
	if (operation != "reconcile" && operation != "rollback") || !strings.HasPrefix(originalAction, "act_") || !strings.HasPrefix(originalAttempt, "ratt_") {
		return withPolicyEvidence(mkDecision(ai, "deny", []string{"runtime_config_recovery:binding"}, nil, "", "", now), cfg), false
	}
	return evaluateRuntimeConfig(ai, r, programSHA, cfg, ctx, now, true, operation+"_inactive_runtime_configuration", "sha256:"+RuntimeConfigRecoveryDigest(r, originalAction, originalAttempt, operation))
}

func evaluateRuntimeConfig(ai envelope.ActionIntent, r RuntimeConfigInstallRequest, programSHA string, cfg Config, ctx EvaluationContext, now time.Time, recovery bool, action, digest string) (envelope.PolicyDecision, bool) {
	deny := func(code string) (envelope.PolicyDecision, bool) {
		return withPolicyEvidence(mkDecision(ai, "deny", []string{"runtime_config_install:" + code}, nil, "", "", now), cfg), false
	}
	if validateRuntimeConfigRequest(r, programSHA, now, recovery) != nil {
		return deny("invalid_request")
	}
	capability := RuntimeConfigInstallCapability
	if recovery {
		capability = RuntimeConfigRecoveryCapability
	}
	if ai.Validate() != nil || ai.Actor == "" || ai.Capability != capability || ai.Action != action || ai.Target.Kind != RuntimeConfigInstallTarget || ai.Target.ID != r.ProfileID || ai.Target.Path != r.Files[0].Path || ai.Destination != RuntimeConfigInstallDestination(r) || ai.ArgumentsHash != digest || ai.SchemaRevision != RuntimeConfigInstallSchema || ai.MutationClass != "security" || ai.Reversibility != "easy" || ai.BlastRadius != "host" || ai.RollbackPlan == "" {
		return deny("intent_binding")
	}
	// Do not stop after the first built-in match. An overlapping custom rule
	// must remain a denial regardless of order or duplicated patterns.
	builtInSet := map[string]bool{}
	resources := []string{r.Files[0].Path, r.Files[1].Path, r.Files[2].Path, r.Files[3].Path, r.SSH.IdentityFile, r.SSH.KnownHostsFile, r.SSH.ClientPath, filepath.Join(r.Root.Path, r.Marker.Basename), filepath.Join(r.Root.Path, r.Lock.Basename), r.Capture.Path, "/usr/bin/python3", "/usr/bin/sudo"}
	for _, pattern := range cfg.ForbiddenPaths {
		for _, path := range resources {
			if _, matches := matchForbidden([]string{pattern}, path); !matches {
				continue
			}
			isBuiltin := (path == r.Files[0].Path && (pattern == "**/.env" || pattern == "**/.env.*")) ||
				(path == r.Files[1].Path && pattern == "**/"+r.Files[1].Basename) ||
				(path == r.Files[2].Path && pattern == "**/"+r.Files[2].Basename) ||
				(path == r.Files[3].Path && pattern == "**/"+r.Files[3].Basename)
			if !isBuiltin {
				return deny("custom_forbidden")
			}
			key := pattern + "|" + path
			if builtInSet[key] {
				return deny("duplicate_forbidden")
			}
			builtInSet[key] = true
		}
	}
	if rule, ok := cfg.ClassOutcome["security"]; !ok || rule.Outcome == "deny" {
		return deny("class_floor")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.OperatorAutonomyLevel)) {
	case "deny", "review", "explicit":
		return deny("operator_ceiling")
	}
	base, reasons, obligations := baselineOutcome(ai, cfg)
	if base == "deny" {
		return deny("baseline_floor")
	}
	reasons = append(reasons, "runtime_config_install:closed_owner_requires_exact_one_use_approval")
	pd := withPolicyEvidence(mkDecision(ai, "hold", reasons, obligations, "", "", now), cfg)
	pd = applyActionSecurityFloor(pd, ai, now)
	pd = applyAutonomyEnvelope(pd, ai, cfg, now)
	pd = applyDesktopSurfaceObligation(pd, ai, now)
	if ctx.SourceControl != nil {
		pd = applySourceControlContext(pd, ai, cfg, *ctx.SourceControl, now)
	}
	if ctx.Placement != nil {
		pd = applyPlacementGuardContext(pd, ai, cfg, *ctx.Placement, now)
	}
	if ctx.Authority != nil {
		pd = applyAuthorityContext(pd, ai, cfg, *ctx.Authority, now)
	}
	if pd.Outcome == "deny" {
		return pd, false
	}
	// Even an allowing class, grant or L3 envelope can never authorize this
	// owner. Only the existing exact approval consumption edge may do so.
	pd.Outcome = "hold"
	pd.StandingGrantID = ""
	pd.ID = idFor("pol_", ai.ID, "hold", "runtime-config-install-owner", ts(now))
	return pd, strings.TrimSpace(ai.SecurityFloorOutcome) == ""
}

func runtimeConfigRequiredFields(raw json.RawMessage, t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true
		}
		return runtimeConfigRequiredFields(raw, t.Elem())
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return false
		}
		count := 0
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				return false
			}
			count++
			value, ok := fields[name]
			if !ok || !runtimeConfigRequiredFields(value, f.Type) {
				return false
			}
		}
		return len(fields) == count
	case reflect.Array, reflect.Slice:
		if t.Kind() == reflect.Slice && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		if t.Kind() == reflect.Array && len(values) != t.Len() {
			return false
		}
		for _, value := range values {
			if !runtimeConfigRequiredFields(value, t.Elem()) {
				return false
			}
		}
	}
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
