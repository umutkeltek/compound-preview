package capgraph

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execChild is a helper child that prints one env var's value so a test can
// prove the broker injected exactly the right credential into the child.
func execChild(t *testing.T, homePath string, surface ConnectionSurface, child ...string) (int, string) {
	t.Helper()
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(surface); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("HOME", t.TempDir()) // keep home.Resolve off the real home
	var stdout, stderr bytes.Buffer
	args := append([]string{"--surface", surface.NodeID, "--"}, child...)
	code := CredentialExecCommand(args, &stdout, &stderr)
	if stderr.Len() > 0 {
		t.Logf("stderr: %s", stderr.String())
	}
	return code, stdout.String()
}

func TestCredentialExecHomeSecretInjectsValue(t *testing.T) {
	home := t.TempDir()
	secretPath := filepath.Join(home, "secrets", "minimax-lane", "token")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("super-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	surface := ConnectionSurface{
		NodeID: "tool:minimax-api",
		CredentialHandle: CredentialHandle{
			Ref:       "home-secret://minimax-lane/token",
			Custodian: "compound",
			EnvName:   "MINIMAX_TOKEN",
		},
	}
	code, out := execChild(t, home, surface, "/bin/sh", "-c", `printf '%s' "$MINIMAX_TOKEN"`)
	if code != 0 {
		t.Fatalf("exit=%d want 0", code)
	}
	if out != "super-secret-value" {
		t.Fatalf("child saw %q, want the injected value", out)
	}
}

func TestCredentialExecSelectorSetsNonSecretValue(t *testing.T) {
	home := t.TempDir()
	surface := ConnectionSurface{
		NodeID: "tool:aws-profile",
		CredentialHandle: CredentialHandle{
			Ref:       "selector://prod",
			Custodian: "aws-cli",
			EnvName:   "AWS_PROFILE",
		},
	}
	code, out := execChild(t, home, surface, "/bin/sh", "-c", `printf '%s' "$AWS_PROFILE"`)
	if code != 0 {
		t.Fatalf("exit=%d want 0", code)
	}
	if out != "prod" {
		t.Fatalf("selector child saw %q, want prod", out)
	}
}

func TestCredentialExecEnvFileReadsNamedVariable(t *testing.T) {
	home := t.TempDir()
	// A short, realistic env-file path: t.TempDir() embeds the (long) test name,
	// which would trip ValidateCredentialHandle's opaque-run tripwire for a
	// legitimate locator. os.MkdirTemp's short random suffix stays under the bar,
	// matching a real operator dotenv like ~/.secrets.env.
	envPath, err := os.MkdirTemp("", "env")
	if err != nil {
		t.Fatal(err)
	}
	envPath = filepath.Join(envPath, ".secrets.env")
	if err := os.WriteFile(envPath, []byte("# comment\nAPI_KEY=envfile-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	surface := ConnectionSurface{
		NodeID: "tool:revenuecat",
		CredentialHandle: CredentialHandle{
			Ref:       "env-file://" + envPath + "#API_KEY",
			Custodian: "operator",
			EnvName:   "REVENUECAT_API_KEY",
		},
	}
	code, out := execChild(t, home, surface, "/bin/sh", "-c", `printf '%s' "$REVENUECAT_API_KEY"`)
	if code != 0 {
		t.Fatalf("exit=%d want 0", code)
	}
	if out != "envfile-secret" {
		t.Fatalf("env-file child saw %q, want envfile-secret", out)
	}
}

func TestCredentialExecBrokeredRunsCustodianWrapper(t *testing.T) {
	home := t.TempDir()
	brokerDir := t.TempDir()
	broker := filepath.Join(brokerDir, "injector")
	script := "#!/bin/sh\nINJECTED=brokered-value \"$@\"\n"
	if err := os.WriteFile(broker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	surface := ConnectionSurface{
		NodeID: "tool:db",
		CredentialHandle: CredentialHandle{
			Ref:       "brokered://db.example",
			Custodian: "infra-os",
			EnvName:   "DB_PASSWORD",
			Broker:    broker,
		},
	}
	code, out := execChild(t, home, surface, "/bin/sh", "-c", `printf '%s' "$INJECTED"`)
	if code != 0 {
		t.Fatalf("exit=%d want 0", code)
	}
	if out != "brokered-value" {
		t.Fatalf("brokered child saw %q, want the broker's injected value", out)
	}
}

func TestCredentialExecNoHandleStillExecsWithExtraEnv(t *testing.T) {
	home := t.TempDir()
	surface := ConnectionSurface{NodeID: "tool:plain"}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(surface); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", home)
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	args := []string{"--surface", "tool:plain", "--env", "FOO=bar", "--", "/bin/sh", "-c", `printf '%s' "$FOO"`}
	code := CredentialExecCommand(args, &stdout, &stderr)
	if code != 0 || stdout.String() != "bar" {
		t.Fatalf("code=%d out=%q want 0/bar", code, stdout.String())
	}
}

func TestCredentialExecFailClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	t.Setenv("HOME", t.TempDir())

	// Unknown surface → error, no exec.
	var stdout, stderr bytes.Buffer
	if code := CredentialExecCommand([]string{"--surface", "tool:nope", "--", "/bin/sh", "-c", "exit 0"}, &stdout, &stderr); code == 0 {
		t.Fatal("unknown surface must fail closed")
	}
	// Missing command → error.
	stdout.Reset()
	stderr.Reset()
	if code := CredentialExecCommand([]string{"--surface", "tool:nope"}, &stdout, &stderr); code == 0 {
		t.Fatal("missing command must fail closed")
	}
	// Empty home-secret file → error, child never runs.
	emptyHome := t.TempDir()
	secretPath := filepath.Join(emptyHome, "secrets", "s", "f")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(emptyHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(ConnectionSurface{
		NodeID:           "tool:empty",
		CredentialHandle: CredentialHandle{Ref: "home-secret://s/f", Custodian: "compound", EnvName: "EMPTY_SECRET"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", emptyHome)
	stdout.Reset()
	stderr.Reset()
	marker := filepath.Join(t.TempDir(), "ran")
	if code := CredentialExecCommand([]string{"--surface", "tool:empty", "--", "/bin/sh", "-c", "touch " + marker}, &stdout, &stderr); code == 0 {
		t.Fatal("empty secret must fail closed")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("child ran despite empty secret — fail-closed violated")
	}
}

func TestCredentialExecRecordsUsageNeverValue(t *testing.T) {
	home := t.TempDir()
	secretPath := filepath.Join(home, "secrets", "s", "f")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	surface := ConnectionSurface{
		NodeID:           "tool:usage-probe",
		CredentialHandle: CredentialHandle{Ref: "home-secret://s/f", Custodian: "compound", EnvName: "USAGE_PROBE"},
	}
	if code, _ := execChild(t, home, surface, "/bin/sh", "-c", "exit 0"); code != 0 {
		t.Fatalf("exec failed")
	}
	body, err := os.ReadFile(filepath.Join(home, "registries", "credential-usage.jsonl"))
	if err != nil {
		t.Fatalf("usage record missing: %v", err)
	}
	line := strings.TrimSpace(string(body))
	if strings.Contains(line, "secret-value") {
		t.Fatalf("usage record leaked the credential value: %s", line)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("usage record not JSON: %v", err)
	}
	if rec["surface_id"] != "tool:usage-probe" || rec["env_name"] != "USAGE_PROBE" || rec["kind"] != "home-secret" {
		t.Fatalf("usage record fields wrong: %v", rec)
	}
}

func TestCredentialExecChildExitCodePropagates(t *testing.T) {
	home := t.TempDir()
	surface := ConnectionSurface{NodeID: "tool:exitcode"}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(surface); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", home)
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := CredentialExecCommand([]string{"--surface", "tool:exitcode", "--", "/bin/sh", "-c", "exit 7"}, &stdout, &stderr); code != 7 {
		t.Fatalf("child exit code = %d, want 7", code)
	}
}

func TestSetConnectionSurfaceCredentialBindsInPlace(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(ConnectionSurface{NodeID: "tool:db", Endpoint: "postgres://db.example"}); err != nil {
		t.Fatal(err)
	}

	// Attach a handle to the existing surface.
	h := CredentialHandle{Ref: "home-secret://db/app", Custodian: "compound", EnvName: "DB_PASSWORD"}
	changed, err := store.SetConnectionSurfaceCredential("tool:db", h)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected the handle to be added")
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	var got *ConnectionSurface
	for i := range surfaces {
		if surfaces[i].NodeID == "tool:db" {
			got = &surfaces[i]
		}
	}
	if got == nil {
		t.Fatal("surface vanished after bind")
	}
	if got.CredentialHandle != h {
		t.Fatalf("bound handle = %+v, want %+v", got.CredentialHandle, h)
	}
	if got.Endpoint != "postgres://db.example" {
		t.Fatalf("bind clobbered the surface endpoint: %q", got.Endpoint)
	}

	// Idempotent: same handle → no-op.
	changed, err = store.SetConnectionSurfaceCredential("tool:db", h)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("identical re-bind must be a no-op")
	}
}

func TestSetConnectionSurfaceCredentialFailsClosed(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown surface.
	if _, err := store.SetConnectionSurfaceCredential("tool:nope", CredentialHandle{Ref: "home-secret://s/f", Custodian: "compound", EnvName: "K"}); err == nil {
		t.Fatal("binding an unknown surface must fail closed")
	}
	// Value-shaped handle rejected at the write door.
	if _, err := store.AddConnectionSurface(ConnectionSurface{NodeID: "tool:leaky"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetConnectionSurfaceCredential("tool:leaky", CredentialHandle{Ref: "home-secret://sk-" + strings.Repeat("a", 20), Custodian: "compound", EnvName: "K"}); err == nil {
		t.Fatal("value-shaped handle must be refused at bind")
	}
}

func TestCredentialListShowsOnlySurfacesWithHandles(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(ConnectionSurface{NodeID: "tool:plain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(ConnectionSurface{
		NodeID:           "tool:db",
		CredentialHandle: CredentialHandle{Ref: "home-secret://db/app", Custodian: "compound", EnvName: "DB_PASSWORD"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", home)
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := CredentialListCommand([]string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "tool:plain") {
		t.Fatalf("list showed a handle-less surface: %s", out)
	}
	if !strings.Contains(out, "tool:db") || !strings.Contains(out, "DB_PASSWORD") || !strings.Contains(out, "home-secret://db/app") {
		t.Fatalf("list omitted the bound surface: %s", out)
	}
}
