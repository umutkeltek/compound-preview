package act

import (
	"compound/internal/policy"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func runtimeConfigInstallFixture(now time.Time) policy.RuntimeConfigInstallRequest {
	h := strings.Repeat("a", 64)
	stat := policy.RuntimeConfigInstallStat{Device: 1, Inode: 2, Bytes: 32, MtimeNS: "1792197133436814154", CtimeNS: "1792197133436814154", Mode: "0600", UID: 0, GID: 0, Links: 1, SHA256: h}
	r := policy.RuntimeConfigInstallRequest{SchemaVersion: policy.RuntimeConfigInstallSchema, ProfileID: "fixture.invalid", AttemptID: now.UTC().Format("20060102t150405z") + "-aaaaaaaaaaaaaaaa", ProgramSHA256: h, EffectSet: policy.RuntimeConfigInstallEffects, ForcedInactive: true,
		Witness:  policy.RuntimeConfigInstallWitness{Ref: "rec_fixture_metadata", SHA256: h, ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)},
		Resource: policy.RuntimeConfigInstallResource{Account: "000000000000", Region: "fixture-region", Instance: "fixture:instance:example", Address: "10.0.0.10"},
		SSH:      policy.RuntimeConfigInstallSSH{Alias: "target-fixture", User: "fixture", Port: 22, IdentityFile: "/fixture/identity", KnownHostsFile: "/fixture/known_hosts", KnownHostsSHA256: h, HostKeySHA256: "SHA256:" + strings.Repeat("A", 43), InterpreterSHA256: h, ClientPath: "/usr/lib/postgresql/17/bin/psql", ClientSHA256: h, ClientVersion: "psql (PostgreSQL) 17.11", SudoSHA256: h},
		Host:     policy.RuntimeConfigInstallHost{Hostname: "host.fixture.invalid", MachineID: strings.Repeat("a", 32), BootID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", SystemIdentifier: "1234567890123456789", VersionNum: "170011"},
		Files: [4]policy.RuntimeConfigInstallFile{
			{Slot: "app", Basename: ".env", Path: "/srv/fixture/.env", Exists: true, Stat: &stat},
			{Slot: "api", Basename: "service.env", Path: "/srv/fixture/service.env", Exists: false, Stat: nil},
			{Slot: "migrator", Basename: "migration.env", Path: "/srv/fixture/migration.env", Exists: false, Stat: nil},
			{Slot: "web", Basename: "frontend.env", Path: "/srv/fixture/frontend.env", Exists: false, Stat: nil},
		},
		Roles:    [2]policy.RuntimeConfigInstallRole{{Kind: "application", Key: "DATABASE_URL", Name: "fixture_app", SourceHost: "source.fixture.invalid", SourceDatabase: "fixture_app"}, {Kind: "migrator", Key: "MIGRATION_SOURCE_URL", Name: "fixture_migrator", SourceHost: "source.fixture.invalid", SourceDatabase: "fixture_app"}},
		Database: policy.RuntimeConfigInstallDatabase{Name: "fixture_app", AllowConnections: true, EndpointHost: "db.fixture.invalid", EndpointPort: 15439, Scheme: "postgresql+psycopg"},
		Posture:  policy.RuntimeConfigInstallPosture{Production: true, LiveFetch: false, CheckoutEnabled: false},
	}
	r.Root = policy.RuntimeConfigInstallRoot{Path: "/srv/fixture", Device: 1, Inode: 10, Mode: "0700"}
	r.Marker = policy.RuntimeConfigInstallAnchor{Basename: "inactive", Stat: stat}
	r.Lock = policy.RuntimeConfigInstallAnchor{Basename: "install.lock", Stat: stat}
	r.Marker.Stat.Inode = 11
	r.Lock.Stat.Inode = 12
	r.Profile = policy.RuntimeConfigInstallProfile{Ref: "sha256:" + h, SHA256: h, Basenames: [4]string{".env", "service.env", "migration.env", "frontend.env"}, AppKey: "APP_URL", APIKey: "DATABASE_URL", MigratorKey: "MIGRATION_URL", ProductionKey: "MODE", LiveFetchKey: "FETCH_ENABLED", CheckoutKey: "CHECKOUT_ENABLED", APIFields: []string{"DEALER_KEY", "PROVIDER_KEY"}, WebAPIKey: "API_BASE", WebDealerKey: "DEALER_KEY", WebCookieKey: "COOKIE_NAME", WebAPIBase: "http://fixture-api:9000", WebCookieName: "__Host-fixture", WebDealerSource: "DEALER_KEY", Containers: []string{"fixture-api", "fixture-web"}}
	proof := policy.RuntimeConfigInstallProofRef{Ref: "sha256:" + h, SHA256: h}
	r.Qualification = policy.RuntimeConfigInstallQualification{Plan: proof, Receipt: proof, Transition: proof, Attempt: proof, Observation: proof}
	r.Capture = policy.RuntimeConfigInstallCapture{Qualification: proof, Receipt: proof, Transition: proof, Path: "/fixture/capture.private.json", Stat: stat, ProjectionSHA256: h}

	r.ProgramSHA256 = RuntimeConfigInstallProgramSHA256()
	return r
}
func TestRuntimeConfigInstallPlanRefusesWithoutNativeQualification(t *testing.T) {
	v, e := NewRuntimeConfigInstaller(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = v.Plan(runtimeConfigInstallFixture(time.Now().UTC()), "work_missing", "fixture"); e == nil {
		t.Fatal("accepted missing Work/native proof")
	}
}
func runtimeConfigInstallObservationFixture(r policy.RuntimeConfigInstallRequest) RuntimeConfigInstallObservation {
	o := RuntimeConfigInstallObservation{Operation: "install", Changed: true, SchemaVersion: runtimeConfigInstallObservationSchema, RequestSHA256: policy.RuntimeConfigInstallDigest(r), ProgramSHA256: RuntimeConfigInstallProgramSHA256(), Transaction: ".rci-" + r.AttemptID, State: "installed_inactive", HostBefore: r.Host, HostAfter: r.Host, FencesRetained: true, ConfigurationAccepted: true}
	for i, f := range r.Files {
		o.Preimages[i] = f.Stat
		post := *r.Files[0].Stat
		post.Inode = uint64(100 + i)
		o.Postimages[i] = &post
		o.Written[i] = true
	}
	return o
}
func TestRuntimeConfigInstallObservationRejectsEchoAndActivation(t *testing.T) {
	r := runtimeConfigInstallFixture(time.Now().UTC())
	valid := runtimeConfigInstallObservationFixture(r)
	raw, _ := json.Marshal(valid)
	if _, e := decodeRuntimeConfigInstallObservation(raw, r); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*RuntimeConfigInstallObservation){func(o *RuntimeConfigInstallObservation) { o.ConfigurationAccepted = false }, func(o *RuntimeConfigInstallObservation) { o.ApplicationActivated = true }, func(o *RuntimeConfigInstallObservation) { o.ServiceActivated = true }, func(o *RuntimeConfigInstallObservation) { o.FencesRetained = false }, func(o *RuntimeConfigInstallObservation) { o.Postimages[1] = nil }, func(o *RuntimeConfigInstallObservation) { o.Written[0] = false }, func(o *RuntimeConfigInstallObservation) { o.HostAfter.Hostname = "other.invalid" }, func(o *RuntimeConfigInstallObservation) { o.State = "recovery_required" }} {
		o := valid
		mutate(&o)
		raw, _ = json.Marshal(o)
		if _, e := decodeRuntimeConfigInstallObservation(raw, r); e == nil {
			t.Fatal("accepted invalid or echo observation")
		}
	}
}
func TestRuntimeConfigInstallTransportRequiresExactGuard(t *testing.T) {
	v, e := NewRuntimeConfigInstaller(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r := runtimeConfigInstallFixture(time.Now().UTC())
	if _, e = v.transport.Start(context.Background(), r, map[string]string{}, "install", time.Now().Add(time.Second), nil); e == nil {
		t.Fatal("native transport launched without owner admission")
	}
}

func TestRuntimeConfigComposedProductionProgramParsesWithExactVerifier(t *testing.T) {
	command := exec.Command("python3", "-c", "import sys; g={'__name__':'synthetic_import'}; exec(sys.stdin.read(),g); assert callable(g['dispatch']); assert callable(g['_VERIFIER'].NativeRuntime); print('composed-production-program-valid')")
	command.Stdin = strings.NewReader(runtimeConfigInstallProgram())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("composed production program invalid: %v %s", err, output)
	}
	if string(output) != "composed-production-program-valid\n" {
		t.Fatalf("unexpected program side effect: %q", output)
	}
}
