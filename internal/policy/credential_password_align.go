package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"compound/internal/envelope"
)

var credentialPasswordRevision = regexp.MustCompile(`^[0-9]{1,10}$`)
var credentialPasswordCTID = regexp.MustCompile(`^\([0-9]{1,10},[0-9]{1,5}\)$`)
var credentialPasswordContainerPrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,100}$`)

const (
	CredentialPasswordAlignSchema     = "compound.credential_password_alignment_request.v0.1"
	CredentialPasswordAlignCapability = "credential.existing_closed_postgres.align_passwords"
	CredentialPasswordAlignAction     = "align_existing_closed_passwords"
	CredentialPasswordAlignTarget     = "credential_password_alignment"
	CredentialPasswordAlignEffects    = "closed-postgres-two-role-password-alignment-v1"
)

// The embedded closed verification pins remain metadata only. No operation may
// provide a password, replacement file, SQL, command, or recovery destination.
type CredentialPasswordAlignmentRequest struct {
	CredentialVerificationRequest
	Operation string                             `json:"operation"`
	Origin    *CredentialPasswordAlignmentOrigin `json:"origin"`
	Failure   CredentialPasswordFailureOrigin    `json:"failure"`
	Catalog   CredentialPasswordCatalogPin       `json:"catalog"`
	Profile   RuntimeConfigInstallProfile        `json:"profile"`
	Capture   RuntimeConfigInstallCapture        `json:"capture"`
}

type CredentialPasswordAlignmentOrigin struct {
	PlanRef       string `json:"plan_ref"`
	WorkID        string `json:"work_id"`
	ActionID      string `json:"action_id"`
	AttemptID     string `json:"attempt_id"`
	ReceiptID     string `json:"receipt_id"`
	TransitionID  string `json:"transition_id"`
	RequestSHA256 string `json:"request_sha256"`
}

func CredentialPasswordAlignmentCapability(r CredentialPasswordAlignmentRequest) string {
	if r.Operation == "inspect" {
		return "credential.existing_closed_postgres.inspect_password_metadata"
	}
	if r.Operation == "align" {
		return CredentialPasswordAlignCapability
	}
	return "credential.existing_closed_postgres.recover_passwords"
}
func CredentialPasswordAlignmentAction(r CredentialPasswordAlignmentRequest) string {
	if r.Operation == "align" {
		return CredentialPasswordAlignAction
	}
	return r.Operation + "_existing_closed_passwords"
}

// Failure names actual immutable native failure proof; it is not a supplied
// observation or a caller-written statement that authentication failed.
type CredentialPasswordFailureOrigin struct {
	PlanRef         string `json:"plan_ref"`
	WorkID          string `json:"work_id"`
	ActionID        string `json:"action_id"`
	PolicyID        string `json:"policy_id"`
	ApprovalGrantID string `json:"approval_grant_id"`
	AttemptID       string `json:"attempt_id"`
	ReceiptID       string `json:"receipt_id"`
	TransitionID    string `json:"transition_id"`
	RequestSHA256   string `json:"request_sha256"`
}

// The digest covers the canonical private catalog projection, not password
// material. Row revisions prevent overwriting a concurrent password change.
type CredentialPasswordCatalogPin struct {
	SHA256       string                       `json:"sha256"`
	Roles        [2]CredentialPasswordRolePin `json:"roles"`
	DatabaseOID  uint32                       `json:"database_oid"`
	DockerSHA256 string                       `json:"docker_sha256"`
}
type CredentialPasswordRolePin struct {
	OID  uint32 `json:"oid"`
	Xmin string `json:"xmin"`
	CTID string `json:"ctid"`
}

func ParseCredentialPasswordAlignmentRequest(raw []byte) (CredentialPasswordAlignmentRequest, error) {
	var r CredentialPasswordAlignmentRequest
	if len(raw) == 0 || len(raw) > 32768 || !credentialVerifyUniqueJSON(raw) {
		return r, credentialVerifyRefusal("alignment_request_shape")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return r, credentialVerifyRefusal("alignment_request_shape")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return r, credentialVerifyRefusal("alignment_request_trailing")
	}
	var shape struct {
		Roles   []json.RawMessage `json:"roles"`
		Catalog struct {
			Roles []json.RawMessage `json:"roles"`
		} `json:"catalog"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Roles) != 2 || len(shape.Catalog.Roles) != 2 {
		return r, credentialVerifyRefusal("alignment_role_count")
	}
	return r, nil
}

func CredentialPasswordAlignmentDigest(r CredentialPasswordAlignmentRequest) string {
	raw, _ := json.Marshal(r)
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&v)
	raw, _ = json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func CredentialPasswordAlignmentRecoveryDigest(r CredentialPasswordAlignmentRequest, action, attempt, operation string) string {
	r.AttemptID = ""
	r.Witness = CredentialVerifyWitness{}
	r.ProgramSHA256 = ""
	r.Operation = operation
	r.Origin = &CredentialPasswordAlignmentOrigin{ActionID: action, AttemptID: attempt}
	r.Catalog = CredentialPasswordCatalogPin{DockerSHA256: r.Catalog.DockerSHA256}
	return CredentialPasswordAlignmentDigest(r)
}
func CredentialPasswordAlignmentActionDigest(r CredentialPasswordAlignmentRequest) string {
	if r.Operation == "align" || r.Origin == nil {
		return CredentialPasswordAlignmentDigest(r)
	}
	return CredentialPasswordAlignmentRecoveryDigest(r, r.Origin.ActionID, r.Origin.AttemptID, r.Operation)
}

func CredentialPasswordAlignmentVerification(r CredentialPasswordAlignmentRequest, verifierSHA string) CredentialVerificationRequest {
	b := r.CredentialVerificationRequest
	b.SchemaVersion = CredentialVerifySchema
	b.ProgramSHA256 = verifierSHA
	b.EffectSet = CredentialVerifyEffects
	return b
}

func ValidateCredentialPasswordAlignmentRequest(r CredentialPasswordAlignmentRequest, programSHA, verifierSHA string, now time.Time) error {
	bad := func() error { return credentialVerifyRefusal("alignment_invalid_request") }
	if r.SchemaVersion != CredentialPasswordAlignSchema || r.ProgramSHA256 != programSHA || r.EffectSet != CredentialPasswordAlignEffects || !credentialVerifySHA.MatchString(programSHA) {
		return bad()
	}
	if ((r.Operation == "align" || r.Operation == "inspect") && r.Origin != nil) || ((r.Operation == "reconcile" || r.Operation == "restore") && r.Origin == nil) || (r.Operation != "align" && r.Operation != "inspect" && r.Operation != "reconcile" && r.Operation != "restore") {
		return bad()
	}
	if r.Origin != nil {
		o := r.Origin
		if !strings.HasPrefix(o.PlanRef, "artifact:sha256:") || !credentialVerifySHA.MatchString(strings.TrimPrefix(o.PlanRef, "artifact:sha256:")) || !strings.HasPrefix(o.WorkID, "work_") || !strings.HasPrefix(o.ActionID, "act_") || !strings.HasPrefix(o.AttemptID, "ratt_") || !strings.HasPrefix(o.ReceiptID, "rec_") || !strings.HasPrefix(o.TransitionID, "tr_") || !credentialVerifySHA.MatchString(o.RequestSHA256) {
			return bad()
		}
	}
	b := CredentialPasswordAlignmentVerification(r, verifierSHA)
	// Inspection produces the first typed fresh witness. Its original failed
	// verifier chain is checked independently by the native owner; an old
	// diagnostic witness is not promoted into freshness authority here.
	if r.Operation == "inspect" {
		b.Witness.ObservedAt = now.UTC().Format(time.RFC3339Nano)
	}
	if ValidateCredentialVerificationRequest(b, verifierSHA, now) != nil {
		return bad()
	}
	f := r.Failure
	if !strings.HasPrefix(f.PlanRef, "artifact:sha256:") || !credentialVerifySHA.MatchString(strings.TrimPrefix(f.PlanRef, "artifact:sha256:")) || !strings.HasPrefix(f.WorkID, "work_") || !strings.HasPrefix(f.ActionID, "act_") || !strings.HasPrefix(f.PolicyID, "pol_") || !strings.HasPrefix(f.ApprovalGrantID, "approval_grant_") || !strings.HasPrefix(f.AttemptID, "ratt_") || !strings.HasPrefix(f.ReceiptID, "rec_") || !strings.HasPrefix(f.TransitionID, "tr_") || !credentialVerifySHA.MatchString(f.RequestSHA256) {
		return bad()
	}
	if !credentialVerifySHA.MatchString(r.Catalog.DockerSHA256) {
		return bad()
	}
	if r.Operation != "inspect" && (!credentialVerifySHA.MatchString(r.Catalog.SHA256) || r.Catalog.DatabaseOID == 0 || r.Catalog.Roles[0].OID == 0 || r.Catalog.Roles[1].OID == 0 || r.Catalog.Roles[0].OID == r.Catalog.Roles[1].OID) {
		return bad()
	}
	if !strings.HasPrefix(r.Profile.Ref, "artifact:sha256:") || !credentialVerifySHA.MatchString(r.Profile.SHA256) || !credentialPasswordAlignmentContainerTopology(r.Profile) {
		return bad()
	}
	for _, role := range r.Catalog.Roles {
		if r.Operation == "inspect" {
			break
		}
		if !credentialPasswordRevision.MatchString(role.Xmin) || !credentialPasswordCTID.MatchString(role.CTID) {
			return bad()
		}
	}
	return nil
}

// Source capture has five legacy roles; the frozen target may instead select
// the exact seven-service commercial topology. Neither shape is an open roster.
func credentialPasswordAlignmentContainerTopology(p RuntimeConfigInstallProfile) bool {
	if !runtimeConfigInstallURL.MatchString(p.WebAPIBase) {
		return false
	}
	u, err := url.Parse(p.WebAPIBase)
	if err != nil || !strings.HasSuffix(u.Hostname(), "-api") {
		return false
	}
	api := u.Hostname()
	prefix := strings.TrimSuffix(api, "-api")
	if !credentialPasswordContainerPrefix.MatchString(prefix) {
		return false
	}
	legacy := map[string]bool{}
	for _, suffix := range []string{"-api", "-worker", "-provider-sync", "-web", "-cloudflared"} {
		legacy[prefix+suffix] = true
	}
	commercial := map[string]bool{}
	for _, suffix := range []string{"-auth", "-api", "-worker", "-billing", "-billing-reconcile", "-provider-sync", "-web"} {
		commercial[prefix+suffix] = true
	}
	isLegacy := len(p.Containers) > 0 && len(p.Containers) <= len(legacy)
	isCommercial := len(p.Containers) == len(commercial)
	seen := map[string]bool{}
	for _, name := range p.Containers {
		if !credentialVerifyLabel.MatchString(name) || seen[name] {
			return false
		}
		seen[name] = true
		isLegacy = isLegacy && legacy[name]
		isCommercial = isCommercial && commercial[name]
	}
	return seen[api] && (isLegacy || isCommercial)
}

// The ordinary verifier's independent/custom/duplicate forbidden-rule floors
// are evaluated without deleting or filtering any configured rule. The mapped
// intent is only a local evaluator input; it is never recorded or executed.
func EvaluateCredentialPasswordAlignment(ai envelope.ActionIntent, r CredentialPasswordAlignmentRequest, programSHA, verifierSHA string, cfg Config, ctx EvaluationContext, now time.Time) (envelope.PolicyDecision, bool) {
	deny := func(code string) (envelope.PolicyDecision, bool) {
		return withPolicyEvidence(mkDecision(ai, "deny", []string{"credential_password_alignment:" + code}, nil, "", "", now), cfg), false
	}
	if ValidateCredentialPasswordAlignmentRequest(r, programSHA, verifierSHA, now) != nil {
		return deny("invalid_request")
	}
	if ai.Validate() != nil || !ai.RequiresExactApproval || ai.Actor == "" || ai.Capability != CredentialPasswordAlignmentCapability(r) || ai.Action != CredentialPasswordAlignmentAction(r) || ai.Target.Kind != CredentialPasswordAlignTarget || ai.Target.ID != r.ProfileID || ai.Target.Path != r.File.Path || ai.Destination != CredentialVerificationDestination(r.CredentialVerificationRequest) || ai.ArgumentsHash != "sha256:"+CredentialPasswordAlignmentActionDigest(r) || ai.SchemaRevision != CredentialPasswordAlignSchema || ai.MutationClass != "security" || ai.Reversibility != "manual" || ai.BlastRadius != "host" || ai.RollbackPlan == "" {
		return deny("intent_binding")
	}
	for _, path := range []string{"/usr/bin/docker", filepath.Join(filepath.Dir(r.File.Path), ".compound-credential-password-alignment"), r.Capture.Path} {
		if _, matched := matchForbidden(cfg.ForbiddenPaths, path); matched {
			return deny("custom_forbidden")
		}
	}
	b := CredentialPasswordAlignmentVerification(r, verifierSHA)
	if r.Operation == "inspect" {
		b.Witness.ObservedAt = now.UTC().Format(time.RFC3339Nano)
	}
	mapped := ai
	mapped.Capability = CredentialVerifyCapability
	mapped.Action = CredentialVerifyAction
	mapped.Target.Kind = CredentialVerifyTarget
	mapped.ArgumentsHash = "sha256:" + CredentialVerificationDigest(b)
	mapped.SchemaRevision = CredentialVerifySchema
	mapped.Reversibility = "easy"
	pd, eligible := EvaluateCredentialVerification(mapped, b, verifierSHA, cfg, ctx, now)
	if !eligible || pd.Outcome != "hold" {
		return deny("independent_verification_floor")
	}
	base, reasons, obligations := baselineOutcome(ai, cfg)
	if base == "deny" {
		return deny("baseline_floor")
	}
	reasons = append(reasons, "credential_password_alignment:closed_owner_requires_exact_one_use_approval")
	return withPolicyEvidence(mkDecision(ai, "hold", reasons, obligations, "", "", now), cfg), true
}
