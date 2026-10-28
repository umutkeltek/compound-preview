package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func runtimeConfigInstallFixture(now time.Time) (RuntimeConfigInstallRequest, envelope.ActionIntent) {
	h := strings.Repeat("a", 64)
	stat := RuntimeConfigInstallStat{Device: 1, Inode: 2, Bytes: 32, MtimeNS: "1792197133436814154", CtimeNS: "1792197133436814154", Mode: "0600", UID: 0, GID: 0, Links: 1, SHA256: h}
	r := RuntimeConfigInstallRequest{SchemaVersion: RuntimeConfigInstallSchema, ProfileID: "fixture.invalid", AttemptID: now.UTC().Format("20060102t150405z") + "-aaaaaaaaaaaaaaaa", ProgramSHA256: h, EffectSet: RuntimeConfigInstallEffects, ForcedInactive: true,
		Witness:  RuntimeConfigInstallWitness{Ref: "rec_fixture_metadata", SHA256: h, ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)},
		Resource: RuntimeConfigInstallResource{Account: "000000000000", Region: "fixture-region", Instance: "fixture:instance:example", Address: "10.0.0.10"},
		SSH:      RuntimeConfigInstallSSH{Alias: "target-fixture", User: "fixture", Port: 22, IdentityFile: "/fixture/identity", KnownHostsFile: "/fixture/known_hosts", KnownHostsSHA256: h, HostKeySHA256: "SHA256:" + strings.Repeat("A", 43), InterpreterSHA256: h, ClientPath: "/usr/lib/postgresql/17/bin/psql", ClientSHA256: h, ClientVersion: "psql (PostgreSQL) 17.11", SudoSHA256: h},
		Host:     RuntimeConfigInstallHost{Hostname: "host.fixture.invalid", MachineID: strings.Repeat("a", 32), BootID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", SystemIdentifier: "1234567890123456789", VersionNum: "170011"},
		Files: [4]RuntimeConfigInstallFile{
			{Slot: "app", Basename: ".env", Path: "/srv/fixture/.env", Exists: true, Stat: &stat},
			{Slot: "api", Basename: "service.env", Path: "/srv/fixture/service.env", Exists: false, Stat: nil},
			{Slot: "migrator", Basename: "migration.env", Path: "/srv/fixture/migration.env", Exists: false, Stat: nil},
			{Slot: "web", Basename: "frontend.env", Path: "/srv/fixture/frontend.env", Exists: false, Stat: nil},
		},
		Roles:    [2]RuntimeConfigInstallRole{{Kind: "application", Key: "DATABASE_URL", Name: "fixture_app", SourceHost: "source.fixture.invalid", SourceDatabase: "fixture_app"}, {Kind: "migrator", Key: "MIGRATION_SOURCE_URL", Name: "fixture_migrator", SourceHost: "source.fixture.invalid", SourceDatabase: "fixture_app"}},
		Database: RuntimeConfigInstallDatabase{Name: "fixture_app", AllowConnections: true, EndpointHost: "db.fixture.invalid", EndpointPort: 15439, Scheme: "postgresql+psycopg"},
		Posture:  RuntimeConfigInstallPosture{Production: true, LiveFetch: false, CheckoutEnabled: false},
	}
	r.Root = RuntimeConfigInstallRoot{Path: "/srv/fixture", Device: 1, Inode: 10, Mode: "0700"}
	r.Marker = RuntimeConfigInstallAnchor{Basename: "inactive", Stat: stat}
	r.Lock = RuntimeConfigInstallAnchor{Basename: "install.lock", Stat: stat}
	r.Marker.Stat.Inode = 11
	r.Lock.Stat.Inode = 12
	r.Profile = RuntimeConfigInstallProfile{Ref: "sha256:" + h, SHA256: h, Basenames: [4]string{".env", "service.env", "migration.env", "frontend.env"}, AppKey: "APP_URL", APIKey: "DATABASE_URL", MigratorKey: "MIGRATION_URL", ProductionKey: "MODE", LiveFetchKey: "FETCH_ENABLED", CheckoutKey: "CHECKOUT_ENABLED", APIFields: []string{"DEALER_KEY", "PROVIDER_KEY"}, WebAPIKey: "API_BASE", WebDealerKey: "DEALER_KEY", WebCookieKey: "COOKIE_NAME", WebAPIBase: "http://fixture-api:9000", WebCookieName: "__Host-fixture", WebDealerSource: "DEALER_KEY", Containers: []string{"fixture-api", "fixture-web"}}
	proof := RuntimeConfigInstallProofRef{Ref: "sha256:" + h, SHA256: h}
	r.Qualification = RuntimeConfigInstallQualification{Plan: proof, Receipt: proof, Transition: proof, Attempt: proof, Observation: proof}
	r.Capture = RuntimeConfigInstallCapture{Qualification: proof, Receipt: proof, Transition: proof, Path: "/fixture/capture.private.json", Stat: stat, ProjectionSHA256: h}

	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_fixture_install", CreatedAt: now.Format(time.RFC3339Nano), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: "work_fixture_install"}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier2", Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: "codex:fixture", Capability: RuntimeConfigInstallCapability, Action: RuntimeConfigInstallAction, Target: envelope.ActionTarget{Kind: RuntimeConfigInstallTarget, ID: r.ProfileID, Path: r.Files[0].Path}, Destination: "ssh://fixture@target-fixture:22", ArgumentsHash: "sha256:" + RuntimeConfigInstallDigest(r), SchemaRevision: RuntimeConfigInstallSchema, MutationClass: "security", Reversibility: "easy", BlastRadius: "host", RollbackPlan: "Preserve staged preimage and failed attempt; no remote persistent mutation or replay."}
	return r, ai
}

func TestRuntimeConfigInstallClosedOwnerHoldsButGenericReadsStillDeny(t *testing.T) {
	now := time.Now().UTC()
	r, ai := runtimeConfigInstallFixture(now)
	cfg := DefaultConfig()
	if err := ValidateRuntimeConfigInstallRequest(r, r.ProgramSHA256, now); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"read", "local_reversible", "security"} {
		x := ai
		x.MutationClass = class
		if got := Evaluate(x, cfg, now); got.Outcome != "deny" {
			t.Fatalf("generic %s protected target outcome=%s", class, got.Outcome)
		}
	}
	pd, eligible := EvaluateRuntimeConfigInstall(ai, r, r.ProgramSHA256, cfg, EvaluationContext{}, now)
	if pd.Outcome != "hold" || !eligible {
		t.Fatalf("closed owner must only HOLD, got %s eligible=%v", pd.Outcome, eligible)
	}
}

func TestRuntimeConfigInstallRejectsUnknownDuplicateAndRoundedJSON(t *testing.T) {
	now := time.Now().UTC()
	r, _ := runtimeConfigInstallFixture(now)
	raw, _ := json.Marshal(r)
	for _, bad := range [][]byte{
		[]byte(strings.Replace(string(raw), `"schema_version":`, `"password":"SYNTHETIC_SECRET","schema_version":`, 1)),
		[]byte(strings.Replace(string(raw), `"effect_set":`, `"effect_set":"other","effect_set":`, 1)),
		[]byte(strings.Replace(string(raw), `"mtime_ns":"1792197133436814154"`, `"mtime_ns":1792197133436814154`, 1)),
	} {
		if _, e := DecodeRuntimeConfigInstallRequest(bad); e == nil {
			t.Fatal("ambiguous/secret-bearing request accepted")
		}
	}
}

func TestRuntimeConfigInstallAllForbiddenAndSecurityFloorsSurvive(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []string{"custom_first", "custom_last", "custom_duplicate", "builtin_duplicate", "class_deny", "missing_class", "operator_review", "security_deny", "security_hold", "wrong_capability", "wrong_report_target", "wrong_hash", "stale_witness", "changed_program", "wrong_basename_app", "wrong_basename_api", "wrong_basename_migrator", "wrong_basename_web", "wrong_count_files", "non_forced_inactive", "live_fetch_allowed", "checkout_enabled_allowed", "non_production"} {
		t.Run(kind, func(t *testing.T) {
			r, ai := runtimeConfigInstallFixture(now)
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
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "changed_program":
				r.ProgramSHA256 = strings.Repeat("b", 64)
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "wrong_basename_app":
				r.Files[0].Basename = "envx"
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "wrong_basename_api":
				r.Files[1].Basename = "servicex"
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "wrong_basename_migrator":
				r.Files[2].Basename = "migratorx"
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "wrong_basename_web":
				r.Files[3].Basename = "webx"
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "wrong_count_files":
				r.Files[3] = r.Files[2]
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "non_forced_inactive":
				r.ForcedInactive = false
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "live_fetch_allowed":
				r.Posture.LiveFetch = true
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "checkout_enabled_allowed":
				r.Posture.CheckoutEnabled = true
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			case "non_production":
				r.Posture.Production = false
				ai.ArgumentsHash = "sha256:" + RuntimeConfigInstallDigest(r)
			}
			pd, eligible := EvaluateRuntimeConfigInstall(ai, r, strings.Repeat("a", 64), cfg, EvaluationContext{}, now)
			if pd.Outcome != outcomeWant || eligible != eligibleWant {
				t.Fatalf("outcome=%s eligible=%v", pd.Outcome, eligible)
			}
		})
	}
}

func TestRuntimeConfigInstallCannotAutoAllowAndDoesNotMutatePolicy(t *testing.T) {
	now := time.Now().UTC()
	r, ai := runtimeConfigInstallFixture(now)
	cfg := DefaultConfig()
	cfg.ClassOutcome["security"] = ClassRule{Outcome: "allow"}
	before, _ := json.Marshal(cfg)
	pd, eligible := EvaluateRuntimeConfigInstall(ai, r, r.ProgramSHA256, cfg, EvaluationContext{}, now)
	after, _ := json.Marshal(cfg)
	if pd.Outcome != "hold" || !eligible || string(before) != string(after) {
		t.Fatal("typed owner autoallowed or mutated policy")
	}
}

func TestRuntimeConfigInstallPreservesIndependentObligationsWhileHolding(t *testing.T) {
	now := time.Now().UTC()
	r, ai := runtimeConfigInstallFixture(now)
	cfg := DefaultConfig()
	cfg.ClassOutcome["security"] = ClassRule{Outcome: "allow_with_obligations", Obligations: []string{"fixture-independent-obligation"}}
	pd, eligible := EvaluateRuntimeConfigInstall(ai, r, r.ProgramSHA256, cfg, EvaluationContext{}, now)
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

func TestRuntimeConfigInstallEndpointBounded(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		mutate func(r *RuntimeConfigInstallRequest)
		valid  bool
	}{
		{"named_endpoint", func(r *RuntimeConfigInstallRequest) {
			r.Database.EndpointHost = "db.fixture.invalid"
			r.Database.EndpointPort = 15439
			r.Database.Scheme = "postgresql+psycopg"
		}, true},
		{"http_scheme_rejected", func(r *RuntimeConfigInstallRequest) { r.Database.Scheme = "http" }, false},
		{"zero_port_rejected", func(r *RuntimeConfigInstallRequest) { r.Database.EndpointPort = 0 }, false},
		{"high_port_rejected", func(r *RuntimeConfigInstallRequest) { r.Database.EndpointPort = 70000 }, false},
		{"empty_host_rejected", func(r *RuntimeConfigInstallRequest) { r.Database.EndpointHost = "" }, false},
		{"postgres_database_rejected", func(r *RuntimeConfigInstallRequest) { r.Database.Name = "postgres" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := runtimeConfigInstallFixture(now)
			tc.mutate(&r)
			if err := ValidateRuntimeConfigInstallRequest(r, r.ProgramSHA256, now); (err == nil) != tc.valid {
				t.Fatalf("endpoint validity = %v, want %v: %v", err == nil, tc.valid, err)
			}
		})
	}
}

func TestRuntimeConfigInstallRequiresExplicitNestedAbsenceAndFalse(t *testing.T) {
	r, _ := runtimeConfigInstallFixture(time.Now().UTC())
	raw, _ := json.Marshal(r)
	for _, fragment := range []string{`"exists":false,`, `"live_fetch":false,`, `,"stat":null`, `"uid":0,`} {
		bad := strings.Replace(string(raw), fragment, "", 1)
		if bad == string(raw) {
			t.Fatal("fixture missing fragment", fragment)
		}
		if _, e := DecodeRuntimeConfigInstallRequest([]byte(bad)); e == nil {
			t.Fatal("accepted omitted required field", fragment)
		}
	}
}
