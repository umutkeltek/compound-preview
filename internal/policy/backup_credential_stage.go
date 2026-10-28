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
	"reflect"
	"regexp"
	"strings"
	"time"

	"compound/internal/envelope"
)

const (
	BackupCredentialStageSchema        = "compound.backup_credential_stage_request.v0.1"
	BackupCredentialStageCapability    = "credential.backup_stage.exact"
	BackupCredentialStageAction        = "stage_existing_backup_credentials"
	BackupCredentialStageTarget        = "backup_credential_stage"
	BackupCredentialStageEffects       = "four-existing-files-root-private-create-only"
	BackupCredentialStageApprovalTTL   = 15 * time.Minute
	BackupCredentialRecoveryCapability = "credential.backup_stage_reconcile.exact"
)

var backupStageLabel = regexp.MustCompile("^[a-z][a-z0-9.-]{1,79}$")
var backupStageAttempt = regexp.MustCompile("^[0-9]{8}t[0-9]{6}z-[a-f0-9]{16}$")
var backupStageNames = [4]string{"pgbackrest-secrets.conf", "restic-credentials.json", "restic-password", "offhost-escrow.json"}

type BackupCredentialStageRequest struct {
	SchemaVersion       string                       `json:"schema_version"`
	ProfileID           string                       `json:"profile_id"`
	AttemptID           string                       `json:"attempt_id"`
	ProgramSHA256       string                       `json:"program_sha256"`
	EffectSet           string                       `json:"effect_set"`
	CreatedAt           string                       `json:"created_at"`
	ExpiresAt           string                       `json:"expires_at"`
	Resource            RuntimeConfigInstallResource `json:"resource"`
	SSH                 BackupCredentialStageSSH     `json:"ssh"`
	Host                BackupCredentialStageHost    `json:"host"`
	SourceDirectory     string                       `json:"source_directory"`
	SourceDirectoryStat RuntimeConfigInstallStat     `json:"source_directory_stat"`
	Root                BackupCredentialStageRoot    `json:"root"`
	Files               [4]BackupCredentialStageFile `json:"files"`
}
type BackupCredentialStageSSH struct {
	Alias             string                   `json:"alias"`
	User              string                   `json:"user"`
	Port              int                      `json:"port"`
	IdentityFile      string                   `json:"identity_file"`
	IdentityStat      RuntimeConfigInstallStat `json:"identity_stat"`
	KnownHostsFile    string                   `json:"known_hosts_file"`
	KnownHostsStat    RuntimeConfigInstallStat `json:"known_hosts_stat"`
	KnownHostsSHA256  string                   `json:"known_hosts_sha256"`
	HostKeySHA256     string                   `json:"host_key_sha256"`
	SSHSHA256         string                   `json:"ssh_sha256"`
	InterpreterSHA256 string                   `json:"interpreter_sha256"`
	SudoSHA256        string                   `json:"sudo_sha256"`
}
type BackupCredentialStageHost struct {
	Hostname  string `json:"hostname"`
	MachineID string `json:"machine_id"`
	BootID    string `json:"boot_id"`
}
type BackupCredentialStageRoot struct {
	Path string `json:"path"`
}
type BackupCredentialStageFile struct {
	Basename   string                    `json:"basename"`
	SourcePath string                    `json:"source_path"`
	Path       string                    `json:"path"`
	Stat       *RuntimeConfigInstallStat `json:"stat"`
}

func DecodeBackupCredentialStageRequest(raw []byte) (BackupCredentialStageRequest, error) {
	var r BackupCredentialStageRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 32768 || !credentialVerifyUniqueJSON(raw) || d.Decode(&r) != nil || !runtimeConfigRequiredFields(raw, reflect.TypeOf(r)) {
		return r, errors.New("backup_credential_stage:request_shape")
	}
	var shape struct {
		Files []json.RawMessage `json:"files"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Files) != 4 {
		return r, errors.New("backup_credential_stage:file_count")
	}
	var more any
	if d.Decode(&more) != io.EOF {
		return r, errors.New("backup_credential_stage:request_shape")
	}
	return r, nil
}
func backupStageHash(raw []byte) string { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }
func BackupCredentialStageDigest(r BackupCredentialStageRequest) string {
	raw, _ := json.Marshal(r)
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&v)
	raw, _ = json.Marshal(v)
	return backupStageHash(raw)
}
func BackupCredentialStageDestination(r BackupCredentialStageRequest) string {
	return r.SSH.User + "@" + r.Resource.Address + ":22:" + r.Root.Path
}
func BackupCredentialRecoveryDigest(r BackupCredentialStageRequest, action, attempt, operation string) string {
	b, _ := json.Marshal(struct{ Request, Action, Attempt, Operation string }{BackupCredentialStageDigest(r), action, attempt, operation})
	return backupStageHash(b)
}
func backupStageSHA(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && strings.ToLower(s) == s
}
func backupStageKnownHostsShape(s RuntimeConfigInstallStat) RuntimeConfigInstallStat {
	s.Mode = "0600"
	return s
}
func backupStageSourceShape(s RuntimeConfigInstallStat) RuntimeConfigInstallStat {
	if s.SHA256 == "" {
		s.SHA256 = strings.Repeat("0", 64)
	}
	return s
}
func backupStageIdentityShape(s RuntimeConfigInstallStat) RuntimeConfigInstallStat {
	s.SHA256 = strings.Repeat("0", 64)
	return s
}
func backupStagePath(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && s != "/" && !strings.ContainsAny(s, "\x00\r\n")
}
func backupStageStat(s RuntimeConfigInstallStat, directory bool) bool {
	mode := "0600"
	if directory {
		mode = "0700"
	}
	if s.Device == 0 || s.Inode == 0 || s.Mode != mode || s.Links == 0 || s.MtimeNS == "" || s.CtimeNS == "" {
		return false
	}
	if !directory && (s.Links != 1 || s.Bytes <= 0 || s.Bytes > 65536) {
		return false
	}
	return directory || backupStageSHA(s.SHA256)
}
func ValidateBackupCredentialStageRequest(r BackupCredentialStageRequest, program string, now time.Time) error {
	return validateBackupCredentialStageRequest(r, program, now, false)
}
func ValidateBackupCredentialRecoveryRequest(r BackupCredentialStageRequest, program string, now time.Time) error {
	return validateBackupCredentialStageRequest(r, program, now, true)
}
func validateBackupCredentialStageRequest(r BackupCredentialStageRequest, program string, now time.Time, recovery bool) error {
	bad := errors.New("backup_credential_stage:invalid_request")
	created, e1 := time.Parse(time.RFC3339Nano, r.CreatedAt)
	expires, e2 := time.Parse(time.RFC3339Nano, r.ExpiresAt)
	if r.SchemaVersion != BackupCredentialStageSchema || r.EffectSet != BackupCredentialStageEffects || !backupStageLabel.MatchString(r.ProfileID) || !backupStageAttempt.MatchString(r.AttemptID) || r.ProgramSHA256 != program || !backupStageSHA(program) || e1 != nil || e2 != nil || !expires.After(created) || expires.Sub(created) > BackupCredentialStageApprovalTTL || (!recovery && (created.After(now) || !expires.After(now))) {
		return bad
	}
	if r.Root.Path != "/root/private-recovery/"+r.ProfileID || !backupStagePath(r.SourceDirectory) || !backupStageStat(r.SourceDirectoryStat, true) {
		return bad
	}
	if net.ParseIP(r.Resource.Address) == nil || r.Resource.Account == "" || r.Resource.Region == "" || r.Resource.Instance == "" || !backupStageLabel.MatchString(r.SSH.Alias) || !backupStageLabel.MatchString(r.SSH.User) || r.SSH.Port != 22 || !backupStagePath(r.SSH.IdentityFile) || !backupStagePath(r.SSH.KnownHostsFile) || r.SSH.IdentityFile == r.SSH.KnownHostsFile || !backupStageStat(backupStageIdentityShape(r.SSH.IdentityStat), false) || r.SSH.IdentityStat.SHA256 != "" || !backupStageStat(backupStageKnownHostsShape(r.SSH.KnownHostsStat), false) || (r.SSH.KnownHostsStat.Mode != "0600" && r.SSH.KnownHostsStat.Mode != "0644") || !backupStageSHA(r.SSH.KnownHostsSHA256) || !backupStageSHA(r.SSH.SSHSHA256) || !backupStageSHA(r.SSH.InterpreterSHA256) || !backupStageSHA(r.SSH.SudoSHA256) || !strings.HasPrefix(r.SSH.HostKeySHA256, "SHA256:") || len(r.SSH.HostKeySHA256) != 50 {
		return bad
	}
	if !backupStageLabel.MatchString(r.Host.Hostname) || !regexp.MustCompile("^[a-f0-9]{32}$").MatchString(r.Host.MachineID) || !regexp.MustCompile("^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$").MatchString(r.Host.BootID) {
		return bad
	}
	seen := map[[2]uint64]bool{}
	for i, f := range r.Files {
		if f.Basename != backupStageNames[i] || f.SourcePath != filepath.Join(r.SourceDirectory, f.Basename) || f.Path != filepath.Join(r.Root.Path, f.Basename) || f.Stat == nil || !backupStageStat(backupStageSourceShape(*f.Stat), false) || f.Stat.Device != r.SourceDirectoryStat.Device || f.Stat.UID != r.SourceDirectoryStat.UID || f.Stat.GID != r.SourceDirectoryStat.GID {
			return bad
		}
		key := [2]uint64{f.Stat.Device, f.Stat.Inode}
		if seen[key] {
			return bad
		}
		seen[key] = true
	}
	return nil
}
func EvaluateBackupCredentialStage(ai envelope.ActionIntent, r BackupCredentialStageRequest, program string, cfg Config, ctx EvaluationContext, now time.Time) (envelope.PolicyDecision, bool) {
	return evaluateBackupCredentialStage(ai, r, program, cfg, ctx, now, false, BackupCredentialStageAction, "sha256:"+BackupCredentialStageDigest(r))
}
func EvaluateBackupCredentialRecovery(ai envelope.ActionIntent, r BackupCredentialStageRequest, action, attempt, operation, program string, cfg Config, ctx EvaluationContext, now time.Time) (envelope.PolicyDecision, bool) {
	if operation != "reconcile" || !strings.HasPrefix(action, "act_") || !strings.HasPrefix(attempt, "ratt_") {
		return withPolicyEvidence(mkDecision(ai, "deny", []string{"backup_credential_stage:operation"}, nil, "", "", now), cfg), false
	}
	return evaluateBackupCredentialStage(ai, r, program, cfg, ctx, now, true, operation+"_existing_backup_credentials", "sha256:"+BackupCredentialRecoveryDigest(r, action, attempt, operation))
}
func evaluateBackupCredentialStage(ai envelope.ActionIntent, r BackupCredentialStageRequest, program string, cfg Config, ctx EvaluationContext, now time.Time, recovery bool, action, digest string) (envelope.PolicyDecision, bool) {
	deny := func(code string) (envelope.PolicyDecision, bool) {
		return withPolicyEvidence(mkDecision(ai, "deny", []string{"backup_credential_stage:" + code}, nil, "", "", now), cfg), false
	}
	if validateBackupCredentialStageRequest(r, program, now, recovery) != nil {
		return deny("invalid_request")
	}
	capability := BackupCredentialStageCapability
	if recovery {
		capability = BackupCredentialRecoveryCapability
	}
	if ai.Validate() != nil || ai.Actor == "" || ai.Capability != capability || ai.Action != action || ai.Target.Kind != BackupCredentialStageTarget || ai.Target.ID != r.ProfileID || ai.Target.Path != r.Files[0].SourcePath || ai.Destination != BackupCredentialStageDestination(r) || ai.ArgumentsHash != digest || ai.SchemaRevision != BackupCredentialStageSchema || ai.MutationClass != "security" || ai.Reversibility != "easy" || ai.BlastRadius != "host" || ai.RollbackPlan == "" {
		return deny("intent_binding")
	}
	// This closed owner names every protected source, identity, and destination.
	// Only one instance of each built-in credential pattern is excepted, per
	// exact typed resource. Custom or overlapping/duplicated rules still deny.
	resources := []string{r.SourceDirectory, r.SSH.IdentityFile, r.SSH.KnownHostsFile, r.Root.Path, "/usr/bin/ssh", "/usr/bin/python3", "/usr/bin/sudo"}
	for _, f := range r.Files {
		resources = append(resources, f.SourcePath, f.Path)
	}
	seen := map[string]bool{}
	for _, pattern := range cfg.ForbiddenPaths {
		for _, path := range resources {
			if _, matched := matchForbidden([]string{pattern}, path); !matched {
				continue
			}
			source := path == r.SourceDirectory || path == r.SSH.IdentityFile || path == r.SSH.KnownHostsFile
			for _, f := range r.Files {
				source = source || path == f.SourcePath || path == f.Path
			}
			builtin := source && (pattern == "**/*secret*" || (path == r.SSH.IdentityFile && (pattern == "**/*.key" || pattern == "**/id_rsa*")))
			key := pattern + "|" + path
			if !builtin || seen[key] {
				return deny("forbidden_resource")
			}
			seen[key] = true
		}
	}
	// Native baseline, floors, ceilings and contextual obligations are preserved.
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
	reasons = append(reasons, "backup_credential_stage:closed_owner_requires_exact_one_use_approval")
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
	pd.ID = idFor("pol_", ai.ID, "hold", "backup-credential-stage-owner", ts(now))
	return pd, strings.TrimSpace(ai.SecurityFloorOutcome) == ""

}
