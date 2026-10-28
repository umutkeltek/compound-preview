package browser

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The self-heal tests drive a FAKE agent-browser (a POSIX sh script on PATH).
// It records every argv, refuses any call that lacks --session/--profile or
// carries --headed/--cdp/--auto-connect/--allowed-domains, and requires the
// child environment to pin AGENT_BROWSER_HEADED=false. State lives in files
// under FAKE_AB_DIR so a test can script drift between calls.
const fakeAgentBrowserScript = `#!/bin/sh
D="${FAKE_AB_DIR:?}"
{ printf 'ARGV'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; } >> "$D/argv.log"
session=""; profile=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session) session="$2"; shift 2 ;;
    --profile) profile="$2"; shift 2 ;;
    --json) shift ;;
    --headed|--cdp|--auto-connect|--allowed-domains) printf '{"success":false,"error":"forbidden flag %s"}\n' "$1"; exit 97 ;;
    --*) printf '{"success":false,"error":"unexpected global flag %s"}\n' "$1"; exit 96 ;;
    *) break ;;
  esac
done
if [ -z "$session" ] || [ -z "$profile" ]; then printf '{"success":false,"error":"session and profile required"}\n'; exit 98; fi
if [ "${AGENT_BROWSER_HEADED:-}" != "false" ]; then printf '{"success":false,"error":"headless not pinned"}\n'; exit 95; fi
if [ -f "$D/connect-fail-once" ]; then rm -f "$D/connect-fail-once"; printf '{"error":"Failed to connect: No such file or directory (os error 2)","success":false}\n'; exit 1; fi
cmd="$1"; shift
ok() { if [ $# -gt 0 ]; then d="$1"; else d='{}'; fi; printf '{"success":true,"data":%s,"error":null}\n' "$d"; exit 0; }
fail() { printf '{"success":false,"data":null,"error":"%s"}\n' "$1"; exit 1; }
loggedin() { [ -f "$D/logged_in" ]; }
case "$cmd" in
  open) printf '%s' "$1" > "$D/url"; ok "{\"url\":\"$1\"}" ;;
  get)
    case "$1" in
      url) if loggedin && [ -f "$D/url-in" ]; then u=$(cat "$D/url-in"); else u=$(cat "$D/url" 2>/dev/null); fi; ok "{\"url\":\"$u\"}" ;;
      text) if loggedin; then t=$(cat "$D/text-in" 2>/dev/null); else t=$(cat "$D/text-out" 2>/dev/null); fi; ok "{\"text\":\"$t\"}" ;;
      *) fail "unsupported get $1" ;;
    esac ;;
  snapshot)
    n=$(( $(cat "$D/snapcount" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$D/snapcount"
    f="$D/snapshot-$n.json"; [ -f "$f" ] || f="$D/snapshot.json"
    if loggedin && [ -f "$D/snapshot-in.json" ]; then f="$D/snapshot-in.json"; fi
    cp "$f" "$D/current.json"; ok "$(cat "$f")" ;;
  click|fill|focus)
    ref="${1#@}"
    grep -q "\"$ref\"" "$D/current.json" 2>/dev/null || fail "Unknown ref: $ref"
    if [ -f "$D/stale" ] && grep -qx "$ref" "$D/stale"; then fail "Element not found: @$ref"; fi
    if [ "$cmd" = click ] && [ -f "$D/login-ref" ] && [ "$ref" = "$(cat "$D/login-ref")" ]; then
      [ -n "${FAKE_REGISTRY:-}" ] && cp "$FAKE_REGISTRY" "$D/registry-at-login-click.json"
      [ -f "$D/login-never" ] || : > "$D/logged_in"
    fi
    ok ;;
  batch)
    n=$(( $(cat "$D/batchcount" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$D/batchcount"
    cat > "$D/batch-$n.stdin"
    ref=$(sed -n 's/.*"@\(e[0-9]*\)".*/\1/p' "$D/batch-$n.stdin" | head -1)
    if [ -z "$ref" ] || ! grep -q "\"$ref\"" "$D/current.json" 2>/dev/null; then printf '[{"command":["fill"],"success":false,"error":"Unknown ref: %s","result":null}]\n' "$ref"; exit 1; fi
    if [ -f "$D/echo-fill-error" ]; then v=$(sed -n 's/.*"@e[0-9]*","\([^"]*\)".*/\1/p' "$D/batch-$n.stdin"); printf '[{"command":["fill"],"success":false,"error":"cannot type %s here","result":null}]\n' "$v"; exit 1; fi
    printf '[{"command":["fill"],"success":true,"error":null,"result":{}}]\n'; exit 0 ;;
  screenshot) mkdir -p "$(dirname "$1")"; printf 'PNG' > "$1"; ok "{\"path\":\"$1\"}" ;;
  press|wait|close) ok ;;
  *) fail "unsupported command $cmd" ;;
esac
`

// fakeProviderScript stands in for `compound browser credential-provider`
// and speaks its contract (credential_provider.go): flags are only
// `--home DIR` (anything else is CLI misuse, exit 2 with that verb's usage),
// ONE agent-browser.plugin.v1 credential.resolve envelope on stdin, ONE JSON
// object on stdout, exit 0 for every protocol answer. It serves the username
// and $D/secret as the password, plus $D/otp as `otp` when that file exists.
// Every request body is appended to provider-requests.log.
const fakeProviderScript = `#!/bin/sh
D="${FAKE_AB_DIR:?}"
{ printf 'ARGV'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; } >> "$D/provider.log"
if [ "$1" != browser ] || [ "$2" != credential-provider ] || [ -f "$D/provider-verb-absent" ]; then echo "usage: compound browser list [--json]" >&2; exit 2; fi
shift 2
while [ $# -gt 0 ]; do
  case "$1" in
    --home) [ -n "$2" ] || { echo "usage: compound browser credential-provider [--home DIR]" >&2; exit 2; }; shift 2 ;;
    *) printf 'error: unexpected argument "%s"\nusage: compound browser credential-provider [--home DIR]\n' "$1" >&2; exit 2 ;;
  esac
done
req=$(cat)
printf '%s\n' "$req" >> "$D/provider-requests.log"
reply() { printf '{"protocol":"agent-browser.plugin.v1",%s}\n' "$1"; exit 0; }
case "$req" in *'"protocol":"agent-browser.plugin.v1"'*) ;; *) reply '"success":false,"error":"unsupported protocol"' ;; esac
case "$req" in *'"type":"credential.resolve"'*) ;; *) reply '"success":false,"error":"unsupported request type"' ;; esac
case "$req" in *'"capability":"credential.read"'*) ;; *) reply '"success":false,"error":"credential.resolve requires the credential.read capability"' ;; esac
case "$req" in *'"profileName":"` + fakeProfileID + `"'*) ;; *) reply '"success":false,"error":"profile is not registered"' ;; esac
case "$req" in *'"itemRef":"` + fakeCredentialRef + `"'*|*'"itemRef":null'*) ;; *) reply '"success":false,"error":"itemRef does not match the credential_ref"' ;; esac
[ -f "$D/secret" ] || reply '"success":false,"error":"no credential source for ref (file: absent)"'
otp=""; [ -f "$D/otp" ] && otp=",\"otp\":\"$(cat "$D/otp")\""
reply "\"success\":true,\"credential\":{\"username\":\"operator@example.test\",\"password\":\"$(cat "$D/secret")\"$otp}"
`

const (
	fakeProfileID     = "example-profile"
	fakeCredentialRef = "example-login"
	fakeSecret        = "hunter2-synthetic-secret"
)

type healFixture struct {
	t        *testing.T
	home     string
	stateDir string
	binDir   string
	userData string
}

// newHealFixture builds a temp COMPOUND_HOME with one enrolled profile, the
// fake agent-browser on PATH, and (withProvider) the fake provider on PATH.
func newHealFixture(t *testing.T, withProvider bool) *healFixture {
	t.Helper()
	root := t.TempDir()
	f := &healFixture{
		t:        t,
		home:     filepath.Join(root, "home"),
		stateDir: filepath.Join(root, "fake-state"),
		binDir:   filepath.Join(root, "bin"),
		userData: filepath.Join(root, "profiles", fakeProfileID),
	}
	for _, dir := range []string{f.home, f.stateDir, f.binDir, f.userData} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(f.binDir, "agent-browser"), fakeAgentBrowserScript)
	if withProvider {
		writeExec(t, filepath.Join(f.binDir, "compound"), fakeProviderScript)
	}
	t.Setenv("PATH", f.binDir+":/usr/bin:/bin")
	t.Setenv("FAKE_AB_DIR", f.stateDir)
	t.Setenv("FAKE_REGISTRY", filepath.Join(f.home, "registries", "browser-profiles.json"))
	t.Setenv("AGENT_BROWSER_HEADED", "true") // the client must override this
	t.Setenv("AGENT_BROWSER_AUTO_CONNECT", "1")
	f.state("secret", fakeSecret)
	f.state("text-out", "Please sign in to continue")
	f.state("text-in", "Welcome back to your dashboard")
	f.writeRegistry(map[string]any{})
	delays := agentBrowserReconnectDelays
	agentBrowserReconnectDelays = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { agentBrowserReconnectDelays = delays })
	t.Cleanup(f.assertArgvClean)
	return f
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *healFixture) state(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.stateDir, name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *healFixture) readState(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.stateDir, name))
	return string(b)
}

// snapshot writes one fake snapshot data object. n == 0 writes the default
// served once the numbered snapshots run out.
func (f *healFixture) snapshot(n int, refs map[string][2]string) {
	f.t.Helper()
	nodes := map[string]map[string]string{}
	var lines []string
	for ref, rn := range refs {
		nodes[ref] = map[string]string{"role": rn[0], "name": rn[1]}
		lines = append(lines, "- "+rn[0]+" \""+rn[1]+"\" [ref="+ref+"]")
	}
	sort.Strings(lines)
	body, err := json.Marshal(map[string]any{"refs": nodes, "snapshot": strings.Join(lines, "\n")})
	if err != nil {
		f.t.Fatal(err)
	}
	name := "snapshot.json"
	if n > 0 {
		name = "snapshot-" + strconv.Itoa(n) + ".json"
	}
	f.state(name, string(body))
}

// writeRegistry writes the profile registry with the fixture profile plus
// the extra self-heal fields given.
func (f *healFixture) writeRegistry(extra map[string]any) {
	f.t.Helper()
	row := map[string]any{
		"id":               fakeProfileID,
		"user_data_dir":    f.userData,
		"backends":         map[string]any{},
		"logins":           []string{"example-login"},
		"status":           "enrolled",
		"login_check_url":  "https://app.example.test/account",
		"logged_in_marker": map[string]any{"text": "Welcome back"},
		"login_recipe":     "login",
		"credential_ref":   fakeCredentialRef,
		"unknown_future":   "kept",
	}
	for k, v := range extra {
		if v == nil {
			delete(row, k)
			continue
		}
		row[k] = v
	}
	reg := map[string]any{
		"schema_version": "compound.browser_profile_registry.v0.1",
		"updated_at":     "2026-10-07T00:00:00Z",
		"profiles":       []any{row},
	}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	dir := filepath.Join(f.home, "registries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "browser-profiles.json"), body, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *healFixture) registryRow() map[string]any {
	f.t.Helper()
	body, err := os.ReadFile(filepath.Join(f.home, "registries", "browser-profiles.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var reg struct {
		Profiles []map[string]any `json:"profiles"`
	}
	if err := json.Unmarshal(body, &reg); err != nil {
		f.t.Fatal(err)
	}
	for _, row := range reg.Profiles {
		if row["id"] == fakeProfileID {
			return row
		}
	}
	f.t.Fatalf("profile row missing")
	return nil
}

func (f *healFixture) writeRecipe(name string, steps []map[string]any) {
	f.t.Helper()
	dir := filepath.Join(f.home, "browser", "recipes", fakeProfileID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	body, err := json.MarshalIndent(map[string]any{
		"schema_version": "compound.browser_recipe.v0.1",
		"name":           name,
		"profile":        fakeProfileID,
		"revision":       1,
		"steps":          steps,
	}, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *healFixture) readRecipe(name string) browserRecipe {
	f.t.Helper()
	body, err := os.ReadFile(filepath.Join(f.home, "browser", "recipes", fakeProfileID, name+".json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var r browserRecipe
	if err := json.Unmarshal(body, &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// cli runs one self-heal verb against the fixture home.
func (f *healFixture) cli(args ...string) (int, string, string) {
	f.t.Helper()
	var out, errOut bytes.Buffer
	full := append(append([]string{}, args...), "--home", f.home)
	handled, code := runSelfHealCLI(full, &out, &errOut)
	if !handled {
		f.t.Fatalf("verb %q not handled by the self-heal dispatcher", args[0])
	}
	return code, out.String(), errOut.String()
}

func (f *healFixture) argvLines() []string {
	raw := strings.TrimSpace(f.readState("argv.log"))
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// assertArgvClean is the per-invocation isolation check: every fake call
// carried both --session and --profile and none asked for a visible or
// attached browser.
func (f *healFixture) assertArgvClean() {
	for _, line := range f.argvLines() {
		if !strings.Contains(line, "[--session]") || !strings.Contains(line, "[--profile]") {
			f.t.Errorf("agent-browser call without --session/--profile: %s", line)
		}
		for _, banned := range []string{"[--headed]", "[--cdp]", "[--auto-connect]", "[--allowed-domains]"} {
			if strings.Contains(line, banned) {
				f.t.Errorf("agent-browser call carried %s: %s", banned, line)
			}
		}
		if strings.Contains(line, fakeSecret) {
			f.t.Errorf("secret reached agent-browser argv: %s", line)
		}
	}
}

func (f *healFixture) countArgv(fragment string) int {
	n := 0
	for _, line := range f.argvLines() {
		if strings.Contains(line, fragment) {
			n++
		}
	}
	return n
}

func decodeJSONOut(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), v); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
}
