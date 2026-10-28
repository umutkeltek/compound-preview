package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func credentialVerifyFixture(now time.Time) (CredentialVerificationRequest, envelope.ActionIntent) {
	h := strings.Repeat("a", 64)
	r := CredentialVerificationRequest{SchemaVersion: CredentialVerifySchema, ProfileID: "fixture.invalid", AttemptID: now.UTC().Format("20060102t150405z") + "-aaaaaaaaaaaaaaaa", ProgramSHA256: h, EffectSet: CredentialVerifyEffects,
		Witness:  CredentialVerifyWitness{Ref: "rec_fixture_metadata", SHA256: h, ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)},
		Resource: CredentialVerifyResource{Account: "000000000000", Region: "fixture-region", Instance: "fixture:instance:example", Address: "10.0.0.10"},
		SSH:      CredentialVerifySSH{Alias: "target.fixture.invalid", User: "fixture", Port: 22, IdentityFile: "/fixture/identity", KnownHostsFile: "/fixture/known_hosts", KnownHostsSHA256: h, HostKeySHA256: "SHA256:" + strings.Repeat("A", 43), InterpreterSHA256: h, ClientPath: "/usr/lib/postgresql/17/bin/psql", ClientSHA256: h, ClientVersion: "psql (PostgreSQL) 17.11", SudoSHA256: h},
		Host:     CredentialVerifyHost{Hostname: "host.fixture.invalid", MachineID: strings.Repeat("a", 32), BootID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", SystemIdentifier: "1234567890123456789", VersionNum: "170011"},
		File:     CredentialVerifyFile{Path: "/srv/fixture/.env", Device: 1, Inode: 2, Bytes: 300, MtimeNS: "1792197133436814154", CtimeNS: "1792197133436814154", SHA256: h},
		Roles:    [2]CredentialVerifyRole{{Kind: "application", Key: "APP_URL", Name: "fixture_app", SourceHost: "source.fixture.invalid", SourceDatabase: "fixture_app"}, {Kind: "migrator", Key: "MIGRATE_URL", Name: "fixture_migrator", SourceHost: "source.fixture.invalid", SourceDatabase: "fixture_app"}}, Database: CredentialVerifyDatabase{Name: "fixture_app", AllowConnections: true}}
	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_fixture_verify", CreatedAt: now.Format(time.RFC3339Nano), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: "work_fixture_verify"}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier2", Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: "codex:fixture", Capability: CredentialVerifyCapability, Action: CredentialVerifyAction, Target: envelope.ActionTarget{Kind: CredentialVerifyTarget, ID: r.ProfileID, Path: r.File.Path}, Destination: CredentialVerificationDestination(r), ArgumentsHash: "sha256:" + CredentialVerificationDigest(r), SchemaRevision: CredentialVerifySchema, MutationClass: "security", Reversibility: "easy", BlastRadius: "host", RollbackPlan: "Preserve observation and failed attempt; no remote persistent mutation or replay."}
	return r, ai
}

func TestCredentialVerificationClosedOwnerHoldsButGenericReadsStillDeny(t *testing.T) {
	now := time.Now().UTC()
	r, ai := credentialVerifyFixture(now)
	cfg := DefaultConfig()
	if err := ValidateCredentialVerificationRequest(r, r.ProgramSHA256, now); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"read", "local_reversible", "security"} {
		x := ai
		x.MutationClass = class
		if got := Evaluate(x, cfg, now); got.Outcome != "deny" {
			t.Fatalf("generic %s protected target outcome=%s", class, got.Outcome)
		}
	}
	pd, eligible := EvaluateCredentialVerification(ai, r, r.ProgramSHA256, cfg, EvaluationContext{}, now)
	if pd.Outcome != "hold" || !eligible {
		t.Fatalf("closed owner must only HOLD, got %s eligible=%v", pd.Outcome, eligible)
	}
}

func TestCredentialVerificationRejectsUnknownDuplicateAndRoundedJSON(t *testing.T) {
	now := time.Now().UTC()
	r, _ := credentialVerifyFixture(now)
	raw, _ := json.Marshal(r)
	for _, bad := range [][]byte{[]byte(strings.Replace(string(raw), `"schema_version":`, `"password":"SYNTHETIC_SECRET","schema_version":`, 1)), []byte(strings.Replace(string(raw), `"effect_set":`, `"effect_set":"other","effect_set":`, 1)), []byte(strings.Replace(string(raw), `"mtime_ns":"1792197133436814154"`, `"mtime_ns":1792197133436814154`, 1))} {
		if _, e := DecodeCredentialVerificationRequest(bad); e == nil {
			t.Fatal("ambiguous/secret-bearing request accepted")
		}
	}
}

func TestCredentialVerificationAllForbiddenAndSecurityFloorsSurvive(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []string{"custom_first", "custom_last", "custom_duplicate", "builtin_duplicate", "class_deny", "missing_class", "operator_review", "security_deny", "security_hold", "wrong_capability", "wrong_report_target", "wrong_hash", "stale_witness", "changed_program"} {
		t.Run(kind, func(t *testing.T) {
			r, ai := credentialVerifyFixture(now)
			cfg := DefaultConfig()
			eligibleWant := false
			outcomeWant := "deny"
			switch kind {
			case "custom_first":
				cfg.ForbiddenPaths = append([]string{"**/fixture/**"}, cfg.ForbiddenPaths...)
			case "custom_last":
				cfg.ForbiddenPaths = append(cfg.ForbiddenPaths, "**/fixture/**")
			case "custom_duplicate":
				cfg.ForbiddenPaths = append(cfg.ForbiddenPaths, "**/fixture/**", "**/fixture/**")
			case "builtin_duplicate":
				cfg.ForbiddenPaths = append(cfg.ForbiddenPaths, "**/.env")
			case "class_deny":
				cfg.ClassOutcome["security"] = ClassRule{Outcome: "deny"}
			case "missing_class":
				delete(cfg.ClassOutcome, "security")
			case "operator_review":
				cfg.OperatorAutonomyLevel = "review"
			case "security_deny":
				ai.SecurityFloorOutcome = "deny"
				ai.SecurityReasons = []string{"fixture floor"}
			case "security_hold":
				ai.SecurityFloorOutcome = "hold"
				ai.SecurityReasons = []string{"fixture floor"}
				outcomeWant = "hold"
			case "wrong_capability":
				ai.Capability = "runtime.inspect"
			case "wrong_report_target":
				ai.Target.Path = "/fixture/result.json"
			case "wrong_hash":
				ai.ArgumentsHash = "sha256:" + strings.Repeat("b", 64)
			case "stale_witness":
				r.Witness.ObservedAt = now.Add(-16 * time.Minute).Format(time.RFC3339Nano)
				ai.ArgumentsHash = "sha256:" + CredentialVerificationDigest(r)
			case "changed_program":
				r.ProgramSHA256 = strings.Repeat("b", 64)
				ai.ArgumentsHash = "sha256:" + CredentialVerificationDigest(r)
			}
			pd, eligible := EvaluateCredentialVerification(ai, r, strings.Repeat("a", 64), cfg, EvaluationContext{}, now)
			if pd.Outcome != outcomeWant || eligible != eligibleWant {
				t.Fatalf("outcome=%s eligible=%v", pd.Outcome, eligible)
			}
		})
	}
}

func TestCredentialVerificationCannotAutoAllowAndDoesNotMutatePolicy(t *testing.T) {
	now := time.Now().UTC()
	r, ai := credentialVerifyFixture(now)
	cfg := DefaultConfig()
	cfg.ClassOutcome["security"] = ClassRule{Outcome: "allow"}
	before, _ := json.Marshal(cfg)
	pd, eligible := EvaluateCredentialVerification(ai, r, r.ProgramSHA256, cfg, EvaluationContext{}, now)
	after, _ := json.Marshal(cfg)
	if pd.Outcome != "hold" || !eligible || string(before) != string(after) {
		t.Fatal("typed owner autoallowed or mutated policy")
	}
}

func TestCredentialVerificationPreservesIndependentObligationsWhileHolding(t *testing.T) {
	now := time.Now().UTC()
	r, ai := credentialVerifyFixture(now)
	cfg := DefaultConfig()
	cfg.ClassOutcome["security"] = ClassRule{Outcome: "allow_with_obligations", Obligations: []string{"fixture-independent-obligation"}}
	pd, eligible := EvaluateCredentialVerification(ai, r, r.ProgramSHA256, cfg, EvaluationContext{}, now)
	found := false
	for _, obligation := range pd.Obligations {
		if obligation == "fixture-independent-obligation" {
			found = true
		}
	}
	if !eligible || pd.Outcome != "hold" || !found {
		t.Fatal("closed owner erased independent class obligation")
	}
}

func TestCredentialVerificationClientVersionBoundedDebianFormat(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		version string
		valid   bool
	}{
		{"psql (PostgreSQL) 17.11", true},
		{"psql (PostgreSQL) 17.11 (Debian 17.11-1.pgdg12+2)", true},
		{"psql (PostgreSQL) 18.2 (Debian 18.2-2.pgdg13+1~fixture1)", true},
		{"psql (PostgreSQL) 17.11 (Debian 17.11-1111111111111111111111111111111111111111111111111111111111111111)", true},
		{"psql (PostgreSQL) 17.11 (Ubuntu 17.11-1)", false},
		{"psql (PostgreSQL) 17.11 (Debian 18.2-1.pgdg12+2)", false},
		{"psql (PostgreSQL) 17.11 (Debian 17.11-)", false},
		{"psql (PostgreSQL) 17.11 (Debian 17.11-1xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx)", false},
		{"psql (PostgreSQL) 17.11 (Debian 17.11-1);command", false},
		{"psql (PostgreSQL) 17.11 (Debian 17.11-1\"x)", false},
		{"psql (PostgreSQL) 17.11 (Debian 17.11-1\\x)", false},
		{"psql (PostgreSQL) 17.11\n", false},
		{"psql (PostgreSQL) 17.11\r\n", false},
		{"psql (PostgreSQL) 17.11 (Debian 17.11-1\u0000x)", false},
		{"psql (PostgreSQL) 17.11\nextra output", false},
		{"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			r, _ := credentialVerifyFixture(now)
			r.SSH.ClientVersion = tc.version
			if err := ValidateCredentialVerificationRequest(r, r.ProgramSHA256, now); (err == nil) != tc.valid {
				t.Fatalf("version validity = %v, want %v: %v", err == nil, tc.valid, err)
			}
		})
	}
}
