package policy

import (
	"bytes"
	"compound/internal/envelope"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const (
	SourceMarkerRetirementSchema      = "compound.source_marker_retirement_request.v0.1"
	SourceMarkerRetirementCapability  = "runtime.source_marker_retire.exact"
	SourceMarkerRetirementAction      = "retire_accepted_runtime_source_marker"
	SourceMarkerRetirementTarget      = "runtime_source_comparison_marker"
	SourceMarkerRetirementEffects     = "metadata-only-source-marker-rename-after-native-acceptance"
	SourceMarkerRetirementApprovalTTL = 15 * time.Minute
	SourceMarkerRecoveryCapability    = "runtime.source_marker_reconcile.exact"
)

type SourceMarkerRetirementRequest struct {
	SchemaVersion    string                            `json:"schema_version"`
	ProfileID        string                            `json:"profile_id"`
	AttemptID        string                            `json:"attempt_id"`
	ProgramSHA256    string                            `json:"program_sha256"`
	EffectSet        string                            `json:"effect_set"`
	CreatedAt        string                            `json:"created_at"`
	ExpiresAt        string                            `json:"expires_at"`
	Installation     SourceMarkerInstallation          `json:"installation"`
	Resource         RuntimeConfigInstallResource      `json:"resource"`
	SSH              BackupCredentialStageSSH          `json:"ssh"`
	Host             RuntimeConfigInstallHost          `json:"host"`
	Root             BackupCredentialStageRoot         `json:"root"`
	Files            [4]SourceMarkerRetirementFile     `json:"files"`
	Marker           SourceMarkerRetirementFile        `json:"marker"`
	Inactive         SourceMarkerRetirementFile        `json:"inactive"`
	MaintenanceLock  SourceMarkerRetirementFile        `json:"maintenance_lock"`
	RecoveryBasename string                            `json:"recovery_basename"`
	ContainerPrefix  string                            `json:"container_prefix"`
	Database         SourceMarkerRetirementDatabase    `json:"database"`
	Executables      SourceMarkerRetirementExecutables `json:"executables"`
}
type SourceMarkerInstallation struct {
	PlanRef           string `json:"plan_ref"`
	WorkID            string `json:"work_id"`
	ActionID          string `json:"action_id"`
	AttemptID         string `json:"attempt_id"`
	ReceiptID         string `json:"receipt_id"`
	TransitionID      string `json:"transition_id"`
	ObservationRef    string `json:"observation_ref"`
	ObservationSHA256 string `json:"observation_sha256"`
	RequestSHA256     string `json:"request_sha256"`
}
type SourceMarkerRetirementFile struct {
	Basename string                    `json:"basename"`
	Path     string                    `json:"path"`
	Stat     *RuntimeConfigInstallStat `json:"stat"`
}
type SourceMarkerRetirementDatabase struct {
	Name             string `json:"name"`
	OID              string `json:"oid"`
	AllowConnections bool   `json:"allow_connections"`
}
type SourceMarkerRetirementExecutables struct {
	DockerSHA256  string `json:"docker_sha256"`
	RunuserSHA256 string `json:"runuser_sha256"`
	ClientPath    string `json:"client_path"`
	ClientSHA256  string `json:"client_sha256"`
	ClientVersion string `json:"client_version"`
}

func DecodeSourceMarkerRetirementRequest(raw []byte) (SourceMarkerRetirementRequest, error) {
	var r SourceMarkerRetirementRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 32768 || !credentialVerifyUniqueJSON(raw) || d.Decode(&r) != nil || !runtimeConfigRequiredFields(raw, reflect.TypeOf(r)) {
		return r, errors.New("source_marker_retirement:request_shape")
	}
	var more any
	var shape struct {
		Files []json.RawMessage `json:"files"`
	}
	if d.Decode(&more) != io.EOF || json.Unmarshal(raw, &shape) != nil || len(shape.Files) != 4 {
		return r, errors.New("source_marker_retirement:request_shape")
	}
	return r, nil
}
func SourceMarkerRetirementDigest(r SourceMarkerRetirementRequest) string {
	raw, _ := json.Marshal(r)
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&v)
	raw, _ = json.Marshal(v)
	return backupStageHash(raw)
}
func SourceMarkerRetirementDestination(r SourceMarkerRetirementRequest) string {
	return r.SSH.User + "@" + r.Resource.Address + ":22:" + r.Marker.Path
}
func SourceMarkerRecoveryDigest(r SourceMarkerRetirementRequest, action, attempt, operation string) string {
	b, _ := json.Marshal(struct{ Request, Action, Attempt, Operation string }{SourceMarkerRetirementDigest(r), action, attempt, operation})
	return backupStageHash(b)
}
func sourceMarkerMeta(s *RuntimeConfigInstallStat, mode string, hash bool) bool {
	if s == nil || s.Device == 0 || s.Inode == 0 || s.Mode != mode || s.UID != 0 || s.GID != 0 || s.Links != 1 || s.Bytes < 0 || s.MtimeNS == "" || s.CtimeNS == "" {
		return false
	}
	if hash {
		b, e := hex.DecodeString(s.SHA256)
		return e == nil && len(b) == 32
	}
	return s.SHA256 == ""
}
func sourceMarkerName(s string) bool {
	return regexp.MustCompile("^[a-zA-Z0-9._-]{1,127}$").MatchString(s) && s != "." && s != ".."
}
func ValidateSourceMarkerRetirementRequest(r SourceMarkerRetirementRequest, program string, now time.Time) error {
	return validateSourceMarkerRetirementRequest(r, program, now, false)
}
func ValidateSourceMarkerRecoveryRequest(r SourceMarkerRetirementRequest, program string, now time.Time) error {
	return validateSourceMarkerRetirementRequest(r, program, now, true)
}
func validateSourceMarkerRetirementRequest(r SourceMarkerRetirementRequest, program string, now time.Time, recovery bool) error {
	bad := errors.New("source_marker_retirement:invalid_request")
	a, e1 := time.Parse(time.RFC3339Nano, r.CreatedAt)
	b, e2 := time.Parse(time.RFC3339Nano, r.ExpiresAt)
	if r.SchemaVersion != SourceMarkerRetirementSchema || r.EffectSet != SourceMarkerRetirementEffects || !backupStageLabel.MatchString(r.ProfileID) || !backupStageAttempt.MatchString(r.AttemptID) || r.ProgramSHA256 != program || !backupStageSHA(program) || e1 != nil || e2 != nil || !b.After(a) || b.Sub(a) > SourceMarkerRetirementApprovalTTL || (!recovery && (a.After(now) || !b.After(now))) {
		return bad
	}
	if !backupStagePath(r.Root.Path) || !backupStageLabel.MatchString(r.RecoveryBasename) || !backupStageLabel.MatchString(r.ContainerPrefix) || !sourceMarkerMeta(r.Marker.Stat, "0600", false) || !sourceMarkerMeta(r.Inactive.Stat, "0600", false) || !sourceMarkerMeta(r.MaintenanceLock.Stat, "0644", false) {
		return bad
	}
	if r.Marker.Basename != r.Files[0].Basename+".SOURCE_COMPARISON_ONLY" || r.Marker.Path != filepath.Join(r.Root.Path, r.Marker.Basename) || r.Inactive.Basename != ".runtime-staging-inactive" || r.Inactive.Path != filepath.Join(r.Root.Path, r.Inactive.Basename) || r.MaintenanceLock.Path != filepath.Join("/run/lock", r.MaintenanceLock.Basename) || !sourceMarkerName(r.MaintenanceLock.Basename) {
		return bad
	}
	seen := map[[2]uint64]bool{}
	for _, f := range r.Files {
		if !sourceMarkerName(f.Basename) || f.Path != filepath.Join(r.Root.Path, f.Basename) || !sourceMarkerMeta(f.Stat, "0600", true) {
			return bad
		}
		k := [2]uint64{f.Stat.Device, f.Stat.Inode}
		if seen[k] {
			return bad
		}
		seen[k] = true
	}
	if !strings.HasPrefix(r.Installation.WorkID, "work_") || !strings.HasPrefix(r.Installation.ActionID, "act_") || !strings.HasPrefix(r.Installation.AttemptID, "ratt_") || !strings.HasPrefix(r.Installation.ReceiptID, "rec_") || !strings.HasPrefix(r.Installation.TransitionID, "tr_") || r.Installation.PlanRef == "" || r.Installation.ObservationRef == "" || !backupStageSHA(r.Installation.ObservationSHA256) || !backupStageSHA(r.Installation.RequestSHA256) {
		return bad
	}
	if !credentialVerifyName.MatchString(r.Database.Name) || !regexp.MustCompile("^[0-9]{1,20}$").MatchString(r.Database.OID) || !backupStagePath(r.Executables.ClientPath) || !backupStageSHA(r.Executables.DockerSHA256) || !backupStageSHA(r.Executables.RunuserSHA256) || !backupStageSHA(r.Executables.ClientSHA256) || r.Executables.ClientVersion == "" {
		return bad
	}
	if !backupStageLabel.MatchString(r.SSH.Alias) || !backupStageLabel.MatchString(r.SSH.User) || r.SSH.Port != 22 || !backupStagePath(r.SSH.IdentityFile) || !backupStagePath(r.SSH.KnownHostsFile) || r.SSH.IdentityStat.SHA256 != "" || !backupStageStat(backupStageIdentityShape(r.SSH.IdentityStat), false) || !backupStageSHA(r.SSH.KnownHostsSHA256) || !backupStageSHA(r.SSH.SSHSHA256) || !backupStageSHA(r.SSH.InterpreterSHA256) || !backupStageSHA(r.SSH.SudoSHA256) {
		return bad
	}
	return nil
}
func EvaluateSourceMarkerRetirement(ai envelope.ActionIntent, r SourceMarkerRetirementRequest, program string, cfg Config, ctx EvaluationContext, now time.Time) (envelope.PolicyDecision, bool) {
	return evaluateSourceMarkerRetirement(ai, r, program, cfg, ctx, now, false, SourceMarkerRetirementAction, "sha256:"+SourceMarkerRetirementDigest(r))
}
func EvaluateSourceMarkerRecovery(ai envelope.ActionIntent, r SourceMarkerRetirementRequest, action, attempt, operation, program string, cfg Config, ctx EvaluationContext, now time.Time) (envelope.PolicyDecision, bool) {
	if operation != "reconcile" || !strings.HasPrefix(action, "act_") || !strings.HasPrefix(attempt, "ratt_") {
		return withPolicyEvidence(mkDecision(ai, "deny", []string{"source_marker_retirement:recovery_binding"}, nil, "", "", now), cfg), false
	}
	return evaluateSourceMarkerRetirement(ai, r, program, cfg, ctx, now, true, "reconcile_source_comparison_marker", "sha256:"+SourceMarkerRecoveryDigest(r, action, attempt, operation))
}
func evaluateSourceMarkerRetirement(ai envelope.ActionIntent, r SourceMarkerRetirementRequest, program string, cfg Config, ctx EvaluationContext, now time.Time, recovery bool, action, digest string) (envelope.PolicyDecision, bool) {
	deny := func(code string) (envelope.PolicyDecision, bool) {
		return withPolicyEvidence(mkDecision(ai, "deny", []string{"source_marker_retirement:" + code}, nil, "", "", now), cfg), false
	}
	if validateSourceMarkerRetirementRequest(r, program, now, recovery) != nil {
		return deny("invalid_request")
	}
	capability := SourceMarkerRetirementCapability
	if recovery {
		capability = SourceMarkerRecoveryCapability
	}
	if ai.Validate() != nil || ai.Actor == "" || ai.Capability != capability || ai.Action != action || ai.Target.Kind != SourceMarkerRetirementTarget || ai.Target.ID != r.ProfileID || ai.Target.Path != r.Marker.Path || ai.Destination != SourceMarkerRetirementDestination(r) || ai.ArgumentsHash != digest || ai.SchemaRevision != SourceMarkerRetirementSchema || ai.MutationClass != "security" || ai.Reversibility != "easy" || ai.BlastRadius != "host" || ai.RollbackPlan == "" {
		return deny("intent_binding")
	}
	recoveryPath := filepath.Join(r.Root.Path, r.RecoveryBasename, r.Marker.Basename+".preimage")
	paths := []string{r.Root.Path, r.Marker.Path, r.Inactive.Path, r.MaintenanceLock.Path, r.SSH.IdentityFile, r.SSH.KnownHostsFile, recoveryPath, filepath.Join(r.Root.Path, r.RecoveryBasename), "/usr/bin/ssh", "/usr/bin/python3", "/usr/bin/sudo", "/usr/bin/docker", "/usr/sbin/runuser", r.Executables.ClientPath}
	for _, f := range r.Files {
		paths = append(paths, f.Path)
	}
	seen := map[string]bool{}
	for _, pattern := range cfg.ForbiddenPaths {
		for _, path := range paths {
			if _, match := matchForbidden([]string{pattern}, path); !match {
				continue
			}
			allowed := (pattern == "**/.env.*" && (path == r.Marker.Path || path == recoveryPath)) || (pattern == "**/.env" && path == r.Files[0].Path) || (pattern == "**/*secret*" && (path == r.SSH.IdentityFile || path == r.SSH.KnownHostsFile)) || (path == r.SSH.IdentityFile && (pattern == "**/*.key" || pattern == "**/id_rsa*"))
			key := pattern + "|" + path
			if !allowed || seen[key] {
				return deny("forbidden_resource")
			}
			seen[key] = true
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
	reasons = append(reasons, "source_marker_retirement:closed_owner_requires_exact_one_use_approval")
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
	pd.ID = idFor("pol_", ai.ID, "hold", "source-marker-retirement-owner", ts(now))
	return pd, strings.TrimSpace(ai.SecurityFloorOutcome) == ""

}
