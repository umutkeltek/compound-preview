package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
)

const (
	CredentialVerifySchema      = "compound.credential_verification_request.v0.1"
	CredentialVerifyCapability  = "credential.existing_local_postgres.verify"
	CredentialVerifyAction      = "verify_existing_maintenance_auth"
	CredentialVerifyTarget      = "credential_verification"
	CredentialVerifyEffects     = "existing-local-postgres-maintenance-auth-v1"
	CredentialVerifyApprovalTTL = 15 * time.Minute
)

// CredentialVerificationRequest contains references and expected nonsecret
// metadata only. It has no credential, command, query, or export field.
// Nanosecond timestamps are decimal strings: JSON float coercion loses identity.
type CredentialVerificationRequest struct {
	SchemaVersion string                   `json:"schema_version"`
	ProfileID     string                   `json:"profile_id"`
	AttemptID     string                   `json:"attempt_id"`
	ProgramSHA256 string                   `json:"program_sha256"`
	EffectSet     string                   `json:"effect_set"`
	Witness       CredentialVerifyWitness  `json:"witness"`
	Resource      CredentialVerifyResource `json:"resource"`
	SSH           CredentialVerifySSH      `json:"ssh"`
	Host          CredentialVerifyHost     `json:"host"`
	File          CredentialVerifyFile     `json:"file"`
	Roles         [2]CredentialVerifyRole  `json:"roles"`
	Database      CredentialVerifyDatabase `json:"database"`
}

type CredentialVerifyWitness struct {
	Ref        string `json:"ref"`
	SHA256     string `json:"sha256"`
	ObservedAt string `json:"observed_at"`
}
type CredentialVerifyResource struct {
	Account  string `json:"account"`
	Region   string `json:"region"`
	Instance string `json:"instance"`
	Address  string `json:"address"`
}
type CredentialVerifySSH struct {
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
type CredentialVerifyHost struct {
	Hostname         string `json:"hostname"`
	MachineID        string `json:"machine_id"`
	BootID           string `json:"boot_id"`
	SystemIdentifier string `json:"system_identifier"`
	VersionNum       string `json:"version_num"`
}
type CredentialVerifyFile struct {
	Path    string `json:"path"`
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	Bytes   int64  `json:"bytes"`
	MtimeNS string `json:"mtime_ns"`
	CtimeNS string `json:"ctime_ns"`
	SHA256  string `json:"sha256"`
}
type CredentialVerifyRole struct {
	Kind           string `json:"kind"`
	Key            string `json:"key"`
	Name           string `json:"name"`
	SourceHost     string `json:"source_host"`
	SourceDatabase string `json:"source_database"`
}
type CredentialVerifyDatabase struct {
	Name             string `json:"name"`
	AllowConnections bool   `json:"allow_connections"`
}

var credentialVerifyName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
var credentialVerifyLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)
var credentialVerifySHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
var credentialVerifyKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

var credentialVerifyClientVersionPattern = regexp.MustCompile(`^psql \(PostgreSQL\) ([0-9]{1,2}\.[0-9]{1,2})(?: \(Debian ([0-9]{1,2}\.[0-9]{1,2})-([0-9][0-9A-Za-z.+~]{0,63})\))?$`)

func credentialVerifyClientVersion(version string) bool {
	if len(version) > 128 {
		return false
	}
	match := credentialVerifyClientVersionPattern.FindStringSubmatch(version)
	return match != nil && (match[2] == "" || match[1] == match[2])
}

func credentialVerifyRefusal(code string) error { return errors.New("credential_verify:" + code) }

func DecodeCredentialVerificationRequest(raw []byte) (CredentialVerificationRequest, error) {
	var r CredentialVerificationRequest
	if len(raw) == 0 || len(raw) > 32768 {
		return r, credentialVerifyRefusal("request_size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return r, credentialVerifyRefusal("request_shape")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return r, credentialVerifyRefusal("request_trailing")
	}
	var shape struct {
		Roles []json.RawMessage `json:"roles"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Roles) != 2 {
		return r, credentialVerifyRefusal("role_count")
	}
	// A JSON decoder otherwise accepts duplicate fields. Canonical structure
	// must never give two different readers different admission parameters.
	if !credentialVerifyUniqueJSON(raw) {
		return r, credentialVerifyRefusal("request_duplicate")
	}
	return r, nil
}

func credentialVerifyUniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var visit func() bool
	visit = func() bool {
		t, err := d.Token()
		if err != nil {
			return false
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return false
					}
					key, ok := k.(string)
					if !ok || seen[key] {
						return false
					}
					seen[key] = true
					if !visit() {
						return false
					}
				}
				end, e := d.Token()
				return e == nil && end == json.Delim('}')
			case '[':
				for d.More() {
					if !visit() {
						return false
					}
				}
				end, e := d.Token()
				return e == nil && end == json.Delim(']')
			default:
				return false
			}
		}
		return true
	}
	return visit()
}

func CredentialVerificationDigest(r CredentialVerificationRequest) string {
	raw, _ := json.Marshal(r)
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&value)
	raw, _ = json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func CredentialVerificationDestination(r CredentialVerificationRequest) string {
	return "ssh://" + r.SSH.User + "@" + r.SSH.Alias + ":22"
}

func ValidateCredentialVerificationRequest(r CredentialVerificationRequest, programSHA string, now time.Time) error {
	bad := func(code string) error { return credentialVerifyRefusal(code) }
	if r.SchemaVersion != CredentialVerifySchema || r.EffectSet != CredentialVerifyEffects || !credentialVerifyLabel.MatchString(r.ProfileID) || !regexp.MustCompile(`^[0-9]{8}t[0-9]{6}z-[0-9a-f]{16}$`).MatchString(r.AttemptID) {
		return bad("protocol")
	}
	attemptTime, ae := time.Parse("20060102t150405z", r.AttemptID[:16])
	if ae != nil || attemptTime.After(now) || now.Sub(attemptTime) > CredentialVerifyApprovalTTL {
		return bad("attempt_stale")
	}
	if !credentialVerifySHA.MatchString(programSHA) || r.ProgramSHA256 != programSHA {
		return bad("program")
	}
	if !regexp.MustCompile(`^(find|rec|obs)_[a-zA-Z0-9_]{8,100}$`).MatchString(r.Witness.Ref) || !credentialVerifySHA.MatchString(r.Witness.SHA256) {
		return bad("witness")
	}
	observed, e := time.Parse(time.RFC3339Nano, r.Witness.ObservedAt)
	if e != nil || observed.After(now) || now.Sub(observed) > CredentialVerifyApprovalTTL {
		return bad("witness_stale")
	}
	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(r.Resource.Account) || !credentialVerifyLabel.MatchString(r.Resource.Region) || !regexp.MustCompile(`^[a-zA-Z0-9:/_.-]{8,256}$`).MatchString(r.Resource.Instance) || net.ParseIP(r.Resource.Address) == nil {
		return bad("resource")
	}
	s := r.SSH
	if s.IdentityFile == s.KnownHostsFile {
		return bad("ssh_custody_alias")
	}
	if !credentialVerifyLabel.MatchString(s.Alias) || !credentialVerifyName.MatchString(s.User) || s.Port != 22 || !credentialVerifyAbsolute(s.IdentityFile) || !credentialVerifyAbsolute(s.KnownHostsFile) || !credentialVerifySHA.MatchString(s.KnownHostsSHA256) || !regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`).MatchString(s.HostKeySHA256) {
		return bad("ssh_identity")
	}
	if !regexp.MustCompile(`^/usr/(?:lib/postgresql/[0-9]{1,2}/bin|bin)/psql$`).MatchString(s.ClientPath) || !credentialVerifyClientVersion(s.ClientVersion) {
		return bad("client")
	}
	for _, h := range []string{s.InterpreterSHA256, s.ClientSHA256, s.SudoSHA256, r.File.SHA256} {
		if !credentialVerifySHA.MatchString(h) {
			return bad("digest")
		}
	}
	h := r.Host
	if !credentialVerifyLabel.MatchString(h.Hostname) || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(h.MachineID) || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(h.BootID) || !regexp.MustCompile(`^[0-9]{10,20}$`).MatchString(h.SystemIdentifier) || !regexp.MustCompile(`^[0-9]{6}$`).MatchString(h.VersionNum) {
		return bad("host_identity")
	}
	f := r.File
	if !credentialVerifyAbsolute(f.Path) || filepath.Base(f.Path) != ".env" || f.Device == 0 || f.Inode == 0 || f.Bytes <= 0 || f.Bytes > 16384 {
		return bad("file_identity")
	}
	for _, s := range []string{f.MtimeNS, f.CtimeNS} {
		n, e := strconv.ParseInt(s, 10, 64)
		if e != nil || n <= 0 || strconv.FormatInt(n, 10) != s {
			return bad("file_time")
		}
	}
	for i, role := range r.Roles {
		kind := []string{"application", "migrator"}[i]
		if role.Kind != kind || !credentialVerifyKey.MatchString(role.Key) || !credentialVerifyName.MatchString(role.Name) || !credentialVerifyLabel.MatchString(role.SourceHost) || !credentialVerifyName.MatchString(role.SourceDatabase) {
			return bad("role_identity")
		}
	}
	if r.Roles[0].Key == r.Roles[1].Key || r.Roles[0].Name == r.Roles[1].Name || !credentialVerifyName.MatchString(r.Database.Name) || r.Database.Name == "postgres" {
		return bad("role_pair")
	}
	return nil
}

func credentialVerifyAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && regexp.MustCompile(`^/[A-Za-z0-9_./@+-]+$`).MatchString(path) && !strings.Contains(path, "//") && len(path) <= 1024
}

// EvaluateCredentialVerification is owner-specific, never a generic read/path
// exemption. A valid protocol can at most become HOLD for exact native approval.
func EvaluateCredentialVerification(ai envelope.ActionIntent, r CredentialVerificationRequest, programSHA string, cfg Config, ctx EvaluationContext, now time.Time) (envelope.PolicyDecision, bool) {
	deny := func(code string) (envelope.PolicyDecision, bool) {
		return withPolicyEvidence(mkDecision(ai, "deny", []string{"credential_verify:" + code}, nil, "", "", now), cfg), false
	}
	if ValidateCredentialVerificationRequest(r, programSHA, now) != nil {
		return deny("invalid_request")
	}
	if ai.Validate() != nil || ai.Actor == "" || ai.Capability != CredentialVerifyCapability || ai.Action != CredentialVerifyAction || ai.Target.Kind != CredentialVerifyTarget || ai.Target.ID != r.ProfileID || ai.Target.Path != r.File.Path || ai.Destination != CredentialVerificationDestination(r) || ai.ArgumentsHash != "sha256:"+CredentialVerificationDigest(r) || ai.SchemaRevision != CredentialVerifySchema || ai.MutationClass != "security" || ai.Reversibility != "easy" || ai.BlastRadius != "host" || ai.RollbackPlan == "" {
		return deny("intent_binding")
	}
	// Do not stop after the first built-in match. An overlapping custom rule
	// must remain a denial regardless of order or duplicated patterns.
	builtInMatches := 0
	resources := []string{r.File.Path, r.SSH.IdentityFile, r.SSH.KnownHostsFile, r.SSH.ClientPath, "/usr/bin/python3", "/usr/bin/sudo"}
	for _, pattern := range cfg.ForbiddenPaths {
		for _, path := range resources {
			if _, matches := matchForbidden([]string{pattern}, path); !matches {
				continue
			}
			if path != r.File.Path || (pattern != "**/.env" && pattern != "**/.env.*") {
				return deny("custom_forbidden")
			}
			builtInMatches++
		}
	}
	if builtInMatches > 1 {
		return deny("duplicate_forbidden")
	}
	if rule, ok := cfg.ClassOutcome["security"]; !ok || rule.Outcome == "deny" {
		return deny("class_floor")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.OperatorAutonomyLevel)) {
	case "deny", "review", "explicit":
		return deny("operator_ceiling")
	}
	// Generic Evaluate returns at its forbidden-path rung, so its first reason
	// cannot prove the other floors. Evaluate the ordinary remaining layers
	// independently without ever deleting/filtering a configured forbidden rule.
	base, reasons, obligations := baselineOutcome(ai, cfg)
	if base == "deny" {
		return deny("baseline_floor")
	}
	reasons = append(reasons, "credential_verify:closed_owner_requires_exact_one_use_approval")
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
	pd.ID = idFor("pol_", ai.ID, "hold", "credential-verify-owner", ts(now))
	return pd, strings.TrimSpace(ai.SecurityFloorOutcome) == ""
}
