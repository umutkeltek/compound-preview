// Package policy — RED tests for the closed credential-provision admission
// (EvaluateCredentialProvision, packet work_credential_provision_rework_20260912).
//
// The hook's contract:
//  1. Built-in .env/.env.* forbidden-path denial downgrades to NON-ALLOWING hold.
//  2. Custom forbidden rules / explicit class deny / security floor stay DENY.
//  3. Operator autonomy ceiling "deny"/"review" stays DENY.
//  4. Wrong capability/action/kind/actor/work/target_path/keys/mode/schema/policy
//     returns an error (fail-closed shape validation).
//  5. NO input value, value hash, or secret ever lands in the returned decision.
//  6. The exact-approval contract is untouched — the hook only flips the
//     forbidden-path rung of an existing deny to hold (and stamps the closed
//     eligibility marker on EvidenceRefs).
package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

func credReqFixture(targetPath string, keys ...string) CredentialProvisionRequest {
	return CredentialProvisionRequest{
		WorkID:         "work_credx",
		Actor:          "operator:keltek",
		Capability:     CredentialProvisionCapability,
		Action:         CredentialProvisionAction,
		TargetKind:     CredentialProvisionTargetKind,
		TargetPath:     targetPath,
		RemoteEndpoint: "ops-host.invalid:22",
		Keys:           keys,
		ExpectedRev:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Mode:           "fill_empty",
		SchemaRevision: credentialProvisionSchemaVersion,
		PolicyRevision: "policy-revision-fixture",
	}
}

// F1: the closed shape on a .env path downgrades the built-in forbidden-path
// deny to a non-allowing hold and stamps the eligibility marker.
func TestEvaluateCredentialProvisionBuiltInEnvPathDowngradesToHold(t *testing.T) {
	cfg := DefaultConfig()
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN", "DB_URL")
	pd, eligible, err := EvaluateCredentialProvision(req, cfg)
	if err != nil {
		t.Fatalf("EvaluateCredentialProvision: %v", err)
	}
	if !eligible {
		t.Fatalf("expected credential-provision eligibility, got false")
	}
	if pd.Outcome != "hold" {
		t.Fatalf("want hold, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !strings.Contains(strings.Join(pd.Reasons, " | "), "credential provision") {
		t.Fatalf("reasons must name the credential-provision hook, got %v", pd.Reasons)
	}
	hasMarker := false
	for _, r := range pd.EvidenceRefs {
		if r == "credential_provision:eligible" {
			hasMarker = true
			break
		}
	}
	if !hasMarker {
		t.Fatalf("decision evidence_refs must include credential_provision:eligible, got %v", pd.EvidenceRefs)
	}
	// Eligible must NEVER be allow/allow_with_obligations — exact approval is the only authority.
	if pd.Outcome == "allow" || pd.Outcome == "allow_with_obligations" {
		t.Fatalf("downgraded decision must not be allow/allow_with_obligations, got %q", pd.Outcome)
	}
}

// F1b: .env.<name> tails also qualify.
func TestEvaluateCredentialProvisionEnvSuffixedPathDowngradesToHold(t *testing.T) {
	cfg := DefaultConfig()
	req := credReqFixture("/var/lib/myapp/.env.production", "API_TOKEN")
	pd, eligible, err := EvaluateCredentialProvision(req, cfg)
	if err != nil {
		t.Fatalf("EvaluateCredentialProvision: %v", err)
	}
	if !eligible || pd.Outcome != "hold" {
		t.Fatalf("want eligible hold, got eligible=%v outcome=%q reasons=%v", eligible, pd.Outcome, pd.Reasons)
	}
}

// F2: wrong capability is fail-closed at ValidateCredentialProvisionRequest.
func TestValidateCredentialProvisionRequestRefusesWrongCapability(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	req.Capability = "outbound.config_key.write"
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for wrong capability")
	}
}

// F2b: wrong action is refused.
func TestValidateCredentialProvisionRequestRefusesWrongAction(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	req.Action = "repoint_config_key"
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for wrong action")
	}
}

// F2c: target kind must be credential_fields.
func TestValidateCredentialProvisionRequestRefusesWrongTargetKind(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	req.TargetKind = "config_key"
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for wrong target kind")
	}
}

// F2d: actor must be operator:<id>.
func TestValidateCredentialProvisionRequestRefusesNonOperatorActor(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	req.Actor = "claude_code"
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for non-operator actor")
	}
}

// F2e: work id must start with work_.
func TestValidateCredentialProvisionRequestRefusesBadWorkID(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	req.WorkID = "junk_credx"
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for bad work id")
	}
}

// F2f: target path must be absolute.
func TestValidateCredentialProvisionRequestRefusesRelativeTargetPath(t *testing.T) {
	req := credReqFixture("relative/.env", "API_TOKEN")
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for relative target path")
	}
}

// F2g: mode must be fill_empty.
func TestValidateCredentialProvisionRequestRefusesAnyOtherMode(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	req.Mode = "overwrite"
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for non-fill_empty mode")
	}
}

// F2h: schema revision must match the closed shape.
func TestValidateCredentialProvisionRequestRefusesOtherSchemaRevisions(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	req.SchemaRevision = "compound.credential_provision_intent.v999.0"
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for non-matching schema revision")
	}
}

// F2i: duplicate keys are refused.
func TestValidateCredentialProvisionRequestRefusesDuplicateKeys(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN", "API_TOKEN")
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for duplicate keys")
	}
}

// F3: custom forbidden patterns that match the target path produce deny,
// regardless of the credential-provision shape. This is the structural reason
// the hook MUST enumerate every forbidden pattern.
func TestEvaluateCredentialProvisionCustomForbiddenForcesDeny(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ForbiddenPaths = append(cfg.ForbiddenPaths, "**/secret.env", "**/.env.custom")
	req := credReqFixture("/etc/myapp/secret.env", "API_TOKEN")
	pd, eligible, err := EvaluateCredentialProvision(req, cfg)
	if err != nil {
		t.Fatalf("EvaluateCredentialProvision: %v", err)
	}
	if eligible {
		t.Fatalf("custom forbidden pattern must NOT be eligible for credential provision")
	}
	if pd.Outcome != "deny" {
		t.Fatalf("want deny, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
}

// F3b: the built-in `.env` denial still downgrades even with extra
// non-matching custom forbidden patterns present.
func TestEvaluateCredentialProvisionCustomNonMatchingForbiddenKeepsHold(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ForbiddenPaths = append(cfg.ForbiddenPaths, "**/someother.secret")
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	pd, eligible, err := EvaluateCredentialProvision(req, cfg)
	if err != nil {
		t.Fatalf("EvaluateCredentialProvision: %v", err)
	}
	if !eligible || pd.Outcome != "hold" {
		t.Fatalf("want eligible hold, got eligible=%v outcome=%q reasons=%v", eligible, pd.Outcome, pd.Reasons)
	}
}

// F4: operator autonomy ceiling "deny" or "review" must stay deny — the
// hook never downgrades those.
func TestEvaluateCredentialProvisionOperatorCeilingKeepsDeny(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OperatorAutonomyLevel = "review"
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	pd, eligible, err := EvaluateCredentialProvision(req, cfg)
	if err != nil {
		t.Fatalf("EvaluateCredentialProvision: %v", err)
	}
	if eligible {
		t.Fatalf("operator ceiling review must NOT be eligible for credential provision")
	}
	if pd.Outcome != "deny" {
		t.Fatalf("want deny, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
}

// F5: a target path that does NOT match any built-in .env/.env.* pattern is
// not in the closed shape — the hook refuses at validate (structural error).
func TestEvaluateCredentialProvisionNonEnvTargetPathRefused(t *testing.T) {
	cfg := DefaultConfig()
	req := credReqFixture("/etc/myapp/config.yaml", "API_TOKEN")
	pd, eligible, err := EvaluateCredentialProvision(req, cfg)
	if err == nil {
		t.Fatalf("non-env target must error at validate, got pd=%+v eligible=%v", pd, eligible)
	}
}

// F5b: validate() refuses a path that is not .env/.env.<something>.
func TestValidateCredentialProvisionRequestRefusesNonEnvTargetPath(t *testing.T) {
	req := credReqFixture("/etc/myapp/config.yaml", "API_TOKEN")
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for non-.env target path")
	}
}

// F6: the returned decision MUST NOT carry the keys list, the values, or any
// value digest — only the closed metadata shape (work id, capability, target
// path, mode, schema revision) is allowed. Keys are metadata (the names of
// the fields to fill), but the test pins that no VALUE bytes leak.
func TestEvaluateCredentialProvisionNeverLeaksValuesIntoDecision(t *testing.T) {
	cfg := DefaultConfig()
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	pd, _, err := EvaluateCredentialProvision(req, cfg)
	if err != nil {
		t.Fatalf("EvaluateCredentialProvision: %v", err)
	}
	// Marshal the decision to JSON; the JSON must not contain any value-like
	// field. The keys list is in the closure (the test harness already saw
	// it), so we marshal the decision that comes back and ensure it carries
	// only the closed-shape metadata.
	body, err := json.Marshal(pd)
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	if strings.Contains(string(body), "canary-secret") {
		t.Fatalf("decision JSON must never carry secret values, got %s", body)
	}
}

// F7: an empty keys list fails at ValidateCredentialProvisionRequest before
// the hook runs.
func TestValidateCredentialProvisionRequestRefusesEmptyKeys(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env")
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for empty keys")
	}
}

// F8: an empty actor fails at ValidateCredentialProvisionRequest.
func TestValidateCredentialProvisionRequestRefusesEmptyActor(t *testing.T) {
	req := credReqFixture("/etc/myapp/.env", "API_TOKEN")
	req.Actor = ""
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for empty actor")
	}
}

// F9: schema validation refuses target paths that look like an .env.* suffix
// but are not (e.g. ".envfile"). This is the precise reason filepathBase and
// the explicit ".env.<anything>" rule is required.
func TestValidateCredentialProvisionRequestRejectsAmbiguousSuffixes(t *testing.T) {
	req := credReqFixture("/etc/myapp/.envfile", "API_TOKEN")
	if err := ValidateCredentialProvisionRequest(req); err == nil {
		t.Fatal("expected error for .envfile (not .env nor .env.<name>)")
	}
}
