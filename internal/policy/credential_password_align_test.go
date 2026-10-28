package policy

import (
	"compound/internal/envelope"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func credentialPasswordAlignmentFixture(now time.Time) (CredentialPasswordAlignmentRequest, envelope.ActionIntent) {
	b, ai := credentialVerifyFixture(now)
	r := CredentialPasswordAlignmentRequest{CredentialVerificationRequest: b, Operation: "align", Failure: CredentialPasswordFailureOrigin{PlanRef: "artifact:sha256:" + strings.Repeat("d", 64), WorkID: "work_original", ActionID: "act_0123456789ab", PolicyID: "pol_0123456789ab", ApprovalGrantID: "approval_grant_0123456789ab", AttemptID: "ratt_original", ReceiptID: "rec_0123456789ab", TransitionID: "tr_0123456789ab", RequestSHA256: strings.Repeat("e", 64)}, Catalog: CredentialPasswordCatalogPin{SHA256: strings.Repeat("c", 64), DockerSHA256: strings.Repeat("b", 64), DatabaseOID: 16384, Roles: [2]CredentialPasswordRolePin{{OID: 16385, Xmin: "123", CTID: "(0,1)"}, {OID: 16386, Xmin: "123", CTID: "(0,2)"}}}, Profile: RuntimeConfigInstallProfile{Ref: "artifact:sha256:" + strings.Repeat("f", 64), SHA256: strings.Repeat("f", 64), WebAPIBase: "http://example-api:9000", Containers: []string{"example-api", "example-worker", "example-provider-sync", "example-web", "example-cloudflared"}}}
	r.SchemaVersion = CredentialPasswordAlignSchema
	r.EffectSet = CredentialPasswordAlignEffects
	ai.RequiresExactApproval = true
	ai.Capability = CredentialPasswordAlignmentCapability(r)
	ai.Action = CredentialPasswordAlignmentAction(r)
	ai.Target.Kind = CredentialPasswordAlignTarget
	ai.SchemaRevision = CredentialPasswordAlignSchema
	ai.Reversibility = "manual"
	ai.ArgumentsHash = "sha256:" + CredentialPasswordAlignmentActionDigest(r)
	return r, ai
}

func TestCredentialPasswordAlignmentIsSeparateExactOwner(t *testing.T) {
	now := time.Now().UTC()
	r, ai := credentialPasswordAlignmentFixture(now)
	cfg := DefaultConfig()
	before, _ := json.Marshal(cfg)
	pd, ok := EvaluateCredentialPasswordAlignment(ai, r, r.ProgramSHA256, r.ProgramSHA256, cfg, EvaluationContext{}, now)
	if !ok || pd.Outcome != "hold" {
		t.Fatalf("closed owner failed to hold: %+v", pd)
	}
	if generic := Evaluate(ai, cfg, now); generic.Outcome == "allow" || generic.Outcome == "allow_with_obligations" {
		t.Fatal("generic path bypassed protected read")
	}
	after, _ := json.Marshal(cfg)
	if string(before) != string(after) {
		t.Fatal("evaluation mutated policy")
	}
	ai.Capability = CredentialVerifyCapability
	if pd, ok = EvaluateCredentialPasswordAlignment(ai, r, r.ProgramSHA256, r.ProgramSHA256, cfg, EvaluationContext{}, now); ok || pd.Outcome != "deny" {
		t.Fatal("verifier capability substituted")
	}
}

func TestCredentialPasswordAlignmentRetainsAllIndependentAndDuplicateFloors(t *testing.T) {
	now := time.Now().UTC()
	r, ai := credentialPasswordAlignmentFixture(now)
	for _, paths := range [][]string{{"**/.env", "**/.env"}, {"**/.env", "/srv/**"}, {"**/.env", "**/python3"}, {"**/.env", "**/sudo"}, {"**/.env", "**/psql"}, {"**/.env", "**/known_hosts"}} {
		cfg := DefaultConfig()
		cfg.ForbiddenPaths = paths
		pd, ok := EvaluateCredentialPasswordAlignment(ai, r, r.ProgramSHA256, r.ProgramSHA256, cfg, EvaluationContext{}, now)
		if ok || pd.Outcome != "deny" {
			t.Fatalf("custom/duplicate floor lost: %v %+v", paths, pd)
		}
	}
	for _, level := range []string{"deny", "review", "explicit"} {
		cfg := DefaultConfig()
		cfg.OperatorAutonomyLevel = level
		if _, ok := EvaluateCredentialPasswordAlignment(ai, r, r.ProgramSHA256, r.ProgramSHA256, cfg, EvaluationContext{}, now); ok {
			t.Fatal("operator ceiling bypassed", level)
		}
	}
}

func TestCredentialPasswordAlignmentStrictRequestAndRecoveryBinding(t *testing.T) {
	now := time.Now().UTC()
	r, _ := credentialPasswordAlignmentFixture(now)
	raw, _ := json.Marshal(r)
	parsed, e := ParseCredentialPasswordAlignmentRequest(raw)
	if e != nil || !reflect.DeepEqual(parsed, r) {
		t.Fatal("strict round trip", e)
	}
	for _, raw := range [][]byte{[]byte(strings.Replace(string(raw), `"operation":"align"`, `"operation":"align","password":"canary"`, 1)), []byte(strings.Replace(string(raw), `"operation":"align"`, `"operation":"align","operation":"restore"`, 1)), append(raw, []byte(" {}")...)} {
		if _, e := ParseCredentialPasswordAlignmentRequest(raw); e == nil {
			t.Fatal("unknown/duplicate/trailing request accepted")
		}
	}
	digest := CredentialPasswordAlignmentRecoveryDigest(r, "act_0123456789ab", "ratt_original", "restore")
	r.AttemptID = "20261020t120001z-0123456789abcdef"
	r.Witness.ObservedAt = now.Add(time.Minute).Format(time.RFC3339Nano)
	r.Catalog.Roles[0].Xmin = "456"
	if digest != CredentialPasswordAlignmentRecoveryDigest(r, "act_0123456789ab", "ratt_original", "restore") {
		t.Fatal("freshness metadata changed fixed recovery scope")
	}
	r.File.Inode++
	if digest == CredentialPasswordAlignmentRecoveryDigest(r, "act_0123456789ab", "ratt_original", "restore") {
		t.Fatal("protected file revision disappeared from recovery scope")
	}
}
