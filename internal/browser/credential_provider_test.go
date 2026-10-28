package browser

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fake secret material. None of these strings may appear anywhere except the
// provider's protocol stdout (the one place a credential is meant to go).
const (
	fakeBWSToken    = "0.fake-machine-token.TOKEN-SHOULD-NEVER-LEAK"
	fakeBWSPassword = "FAKE-BWS-PASSWORD-never-leak"
	fakeFilePass    = "FAKE-FILE-PASSWORD-never-leak"
	fakeTOTPSeed    = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // base32("12345678901234567890")
)

// RFC 6238 Appendix B. Seeds are ASCII; codes are 8 digits.
func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	seeds := map[string]struct {
		key  []byte
		hash func() hash.Hash
	}{
		"SHA1":   {[]byte("12345678901234567890"), sha1.New},
		"SHA256": {[]byte("12345678901234567890123456789012"), sha256.New},
		"SHA512": {[]byte("1234567890123456789012345678901234567890123456789012345678901234"), sha512.New},
	}
	vectors := []struct {
		unix int64
		algo string
		want string
	}{
		{59, "SHA1", "94287082"}, {59, "SHA256", "46119246"}, {59, "SHA512", "90693936"},
		{1_111_111_109, "SHA1", "07081804"}, {1_111_111_109, "SHA256", "68084774"}, {1_111_111_109, "SHA512", "25091201"},
		{1_111_111_111, "SHA1", "14050471"}, {1_111_111_111, "SHA256", "67062674"}, {1_111_111_111, "SHA512", "99943326"},
		{1_234_567_890, "SHA1", "89005924"}, {1_234_567_890, "SHA256", "91819424"}, {1_234_567_890, "SHA512", "93441116"},
		{2_000_000_000, "SHA1", "69279037"}, {2_000_000_000, "SHA256", "90698825"}, {2_000_000_000, "SHA512", "38618901"},
		{20_000_000_000, "SHA1", "65353130"}, {20_000_000_000, "SHA256", "77737706"}, {20_000_000_000, "SHA512", "47863826"},
	}
	for _, v := range vectors {
		seed := seeds[v.algo]
		if got := totpCode(seed.key, v.unix, 8, seed.hash); got != v.want {
			t.Errorf("T=%d %s: got %s want %s", v.unix, v.algo, got, v.want)
		}
	}
}

// The production path is oathtool's default: base32 seed, SHA1, 30 s, 6 digits.
func TestTOTPFromBase32MatchesOathtoolDefaults(t *testing.T) {
	for _, seed := range []string{fakeTOTPSeed, "gezd gnbv gy3t qojq gezd gnbv gy3t qojq", fakeTOTPSeed + "===="} {
		got, err := totpFromBase32(seed, time.Unix(59, 0))
		if err != nil || got != "287082" {
			t.Fatalf("seed %q: got %q err=%v, want 287082", seed, got, err)
		}
	}
	if _, err := totpFromBase32("not base32 !!", time.Unix(59, 0)); err == nil {
		t.Fatal("invalid base32 seed must be refused")
	}
	if _, err := totpFromBase32("", time.Unix(59, 0)); err == nil {
		t.Fatal("empty seed must be refused")
	}
}

type credentialFixture struct {
	home    string
	binDir  string
	argsLog string
}

// newCredentialFixture builds a temp COMPOUND_HOME with a registry and an
// isolated PATH holding only the fakes the test asks for. No live ~/.compound.
func newCredentialFixture(t *testing.T, profiles []map[string]any) credentialFixture {
	t.Helper()
	root := t.TempDir()
	fx := credentialFixture{home: filepath.Join(root, "home"), binDir: filepath.Join(root, "bin"), argsLog: filepath.Join(root, "bws-args.log")}
	for _, dir := range []string{filepath.Join(fx.home, "registries"), filepath.Join(fx.home, "secrets", "accounts"), fx.binDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reg := map[string]any{"schema_version": "compound.browser_profile_registry.v0.1", "updated_at": "2026-10-07T00:00:00Z", "profiles": profiles}
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.home, "registries", "browser-profiles.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fx.binDir)
	t.Setenv("COMPOUND_HOME", fx.home)
	t.Setenv("BWS_ACCESS_TOKEN", "")
	return fx
}

func profileRow(id, ref string) map[string]any {
	row := map[string]any{"id": id, "user_data_dir": "/nonexistent/" + id, "backends": map[string]any{}, "logins": []string{id}, "status": "pending_enrollment"}
	if ref != "" {
		row["credential_ref"] = ref
	}
	return row
}

func (fx credentialFixture) writeAccount(t *testing.T, ref, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(fx.home, "secrets", "accounts", ref)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func (fx credentialFixture) writeBWSToken(t *testing.T, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(fx.home, "secrets", "bws-access-token")
	if err := os.WriteFile(path, []byte(fakeBWSToken+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// installFakeBWS puts a `bws` on PATH that only answers when the token arrives
// through BWS_ACCESS_TOKEN, logs its argv, and lists one secret per ref.
func (fx credentialFixture) installFakeBWS(t *testing.T, secrets map[string]string) {
	t.Helper()
	type secret struct {
		ID             string `json:"id"`
		OrganizationID string `json:"organizationId"`
		ProjectID      any    `json:"projectId"`
		Key            string `json:"key"`
		Value          string `json:"value"`
		Note           string `json:"note"`
		CreationDate   string `json:"creationDate"`
		RevisionDate   string `json:"revisionDate"`
	}
	var rows []secret
	for key, value := range secrets {
		rows = append(rows, secret{ID: "f1e2d3c4-b5a6-4978-8a9b-c0d1e2f3a4b5", OrganizationID: "a9b8c7d6-e5f4-4321-9f8e-d7c6b5a4f3e2", Key: key, Value: value, CreationDate: "2026-10-07T00:00:00Z", RevisionDate: "2026-10-07T00:00:00Z"})
	}
	listing, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	listingPath := filepath.Join(filepath.Dir(fx.binDir), "bws-listing.json")
	if err := os.WriteFile(listingPath, listing, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + fx.argsLog + "'\n" +
		"[ \"$BWS_ACCESS_TOKEN\" = '" + fakeBWSToken + "' ] || { echo 'unauthorized' >&2; exit 1; }\n" +
		"[ \"$1 $2\" = 'secret list' ] || { echo 'unexpected args' >&2; exit 2; }\n" +
		"/bin/cat '" + listingPath + "'\n"
	if err := os.WriteFile(filepath.Join(fx.binDir, "bws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func resolveRequest(profile string, item any) string {
	payload := map[string]any{
		"protocol":   "agent-browser.plugin.v1",
		"type":       "credential.resolve",
		"capability": "credential.read",
		"request":    map[string]any{"profileName": profile, "itemRef": item, "url": nil},
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

type providerReply struct {
	Protocol   string `json:"protocol"`
	Success    bool   `json:"success"`
	Error      string `json:"error"`
	Credential *struct {
		Username string `json:"username"`
		Password string `json:"password"`
		URL      string `json:"url"`
		OTP      string `json:"otp"`
	} `json:"credential"`
	Manifest *struct {
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
	} `json:"manifest"`
}

func runProvider(t *testing.T, stdin string) (providerReply, int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runCredentialProvider(nil, strings.NewReader(stdin), &out, &errOut)
	var reply providerReply
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		t.Fatalf("provider stdout is not one protocol JSON object: %v\n%s", err, out.String())
	}
	if reply.Protocol != "agent-browser.plugin.v1" {
		t.Fatalf("reply protocol = %q", reply.Protocol)
	}
	return reply, code, out.String(), errOut.String()
}

type auditLine struct {
	At      string   `json:"at"`
	Profile string   `json:"profile"`
	RefName string   `json:"ref_name"`
	Source  string   `json:"source"`
	Fields  []string `json:"fields"`
}

func readAudit(t *testing.T, home string) ([]auditLine, string) {
	t.Helper()
	raw, err := os.ReadFile(credentialAuditPath(home))
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	var lines []auditLine
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var line auditLine
		if err := json.Unmarshal([]byte(ln), &line); err != nil {
			t.Fatalf("audit line %q: %v", ln, err)
		}
		lines = append(lines, line)
	}
	return lines, string(raw)
}

func assertNoSecret(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range append(secrets, fakeBWSToken, fakeBWSPassword, fakeFilePass, fakeTOTPSeed) {
		if s != "" && strings.Contains(text, s) {
			t.Fatalf("%s leaked secret material %q", where, s)
		}
	}
}

func TestCredentialProviderManifest(t *testing.T) {
	newCredentialFixture(t, nil)
	reply, code, _, _ := runProvider(t, `{"protocol":"agent-browser.plugin.v1","type":"plugin.manifest","capability":"plugin.manifest","request":{}}`)
	if code != 0 || !reply.Success || reply.Manifest == nil || reply.Manifest.Name != "compound" || len(reply.Manifest.Capabilities) != 1 || reply.Manifest.Capabilities[0] != "credential.read" {
		t.Fatalf("manifest reply = %+v code=%d", reply, code)
	}
}

func TestCredentialProviderResolvesFromBWSFirst(t *testing.T) {
	fx := newCredentialFixture(t, []map[string]any{profileRow("site-a", "acct-a")})
	fx.writeBWSToken(t, 0o600)
	fx.installFakeBWS(t, map[string]string{"acct-a": "username=bws-user\npassword=" + fakeBWSPassword + "\nlogin_url=https://a.example/login\ntotp_seed=" + fakeTOTPSeed + "\n"})
	// The flat file also holds this ref: bws must win.
	fx.writeAccount(t, "acct-a", "email=file-user\npassword="+fakeFilePass+"\n", 0o600)
	credentialNow = func() time.Time { return time.Unix(59, 0) }
	t.Cleanup(func() { credentialNow = time.Now })

	reply, code, _, stderr := runProvider(t, resolveRequest("site-a", nil))
	if code != 0 || !reply.Success || reply.Credential == nil {
		t.Fatalf("bws resolve failed: %+v code=%d", reply, code)
	}
	if reply.Credential.Username != "bws-user" || reply.Credential.Password != fakeBWSPassword || reply.Credential.URL != "https://a.example/login" || reply.Credential.OTP != "287082" {
		t.Fatalf("credential mapped wrong (username=%q url=%q otp-len=%d)", reply.Credential.Username, reply.Credential.URL, len(reply.Credential.OTP))
	}
	args, err := os.ReadFile(fx.argsLog)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "bws argv", string(args))
	assertNoSecret(t, "provider stderr", stderr, "287082")
	lines, raw := readAudit(t, fx.home)
	assertNoSecret(t, "audit file", raw, "287082")
	if len(lines) != 1 || lines[0].Profile != "site-a" || lines[0].RefName != "acct-a" || lines[0].Source != "bws" || lines[0].At == "" {
		t.Fatalf("audit = %+v", lines)
	}
	if strings.Join(lines[0].Fields, ",") != "username,password,url,otp" {
		t.Fatalf("audit fields = %v", lines[0].Fields)
	}
}

func TestCredentialProviderFallsBackToFlatFile(t *testing.T) {
	fx := newCredentialFixture(t, []map[string]any{profileRow("site-b", "acct-b")})
	// bws installed and authorized, but it does not hold acct-b.
	fx.writeBWSToken(t, 0o600)
	fx.installFakeBWS(t, map[string]string{"other": "username=x\npassword=y\n"})
	fx.writeAccount(t, "acct-b", "# comment\nplatform=example\napple_id=file-user@example.com\npassword="+fakeFilePass+"\n", 0o600)

	reply, code, _, stderr := runProvider(t, resolveRequest("site-b", "acct-b"))
	if code != 0 || !reply.Success || reply.Credential == nil || reply.Credential.Username != "file-user@example.com" || reply.Credential.Password != fakeFilePass || reply.Credential.OTP != "" {
		t.Fatalf("file fallback failed: success=%v code=%d err=%q", reply.Success, code, reply.Error)
	}
	assertNoSecret(t, "provider stderr", stderr)
	lines, raw := readAudit(t, fx.home)
	assertNoSecret(t, "audit file", raw)
	if len(lines) != 1 || lines[0].Source != "file" || strings.Join(lines[0].Fields, ",") != "username,password" {
		t.Fatalf("audit = %+v", lines)
	}
}

func TestCredentialProviderFileOnlyWithoutBWS(t *testing.T) {
	fx := newCredentialFixture(t, []map[string]any{profileRow("site-c", "acct-c")})
	// No bws on PATH, no token: the flat file is the only source.
	fx.writeAccount(t, "acct-c", "email=c@example.com\npassword="+fakeFilePass+"\n", 0o600)
	reply, code, _, _ := runProvider(t, resolveRequest("site-c", nil))
	if code != 0 || !reply.Success || reply.Credential == nil || reply.Credential.Password != fakeFilePass {
		t.Fatalf("file-only resolve failed: %+v", reply.Error)
	}
}

func TestCredentialProviderRefusesCleanly(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		item    any
		setup   func(t *testing.T, fx credentialFixture)
		reason  string
	}{
		{name: "neither source", profile: "site-d", reason: "no credential source"},
		{name: "unregistered profile", profile: "nobody", reason: "not registered"},
		{name: "profile without ref", profile: "no-ref", reason: "no credential_ref"},
		{name: "item mismatch", profile: "site-d", item: "someone-else", setup: func(t *testing.T, fx credentialFixture) {
			fx.writeAccount(t, "acct-d", "email=d@example.com\npassword="+fakeFilePass+"\n", 0o600)
		}, reason: "does not match"},
		{name: "insecure file mode", profile: "site-d", setup: func(t *testing.T, fx credentialFixture) {
			fx.writeAccount(t, "acct-d", "email=d@example.com\npassword="+fakeFilePass+"\n", 0o644)
		}, reason: "no credential source"},
		{name: "incomplete credential", profile: "site-d", setup: func(t *testing.T, fx credentialFixture) {
			fx.writeAccount(t, "acct-d", "handle=d\nauth_method=ssh\n", 0o600)
		}, reason: "incomplete"},
		{name: "bad totp seed", profile: "site-d", setup: func(t *testing.T, fx credentialFixture) {
			fx.writeAccount(t, "acct-d", "email=d@example.com\npassword="+fakeFilePass+"\ntotp_seed=!!notbase32!!\n", 0o600)
		}, reason: "totp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCredentialFixture(t, []map[string]any{profileRow("site-d", "acct-d"), profileRow("no-ref", "")})
			if tc.setup != nil {
				tc.setup(t, fx)
			}
			reply, code, stdout, stderr := runProvider(t, resolveRequest(tc.profile, tc.item))
			if code != 0 || reply.Success || reply.Credential != nil || !strings.Contains(reply.Error, tc.reason) {
				t.Fatalf("want clean refusal containing %q, got success=%v code=%d error=%q", tc.reason, reply.Success, code, reply.Error)
			}
			assertNoSecret(t, "refusal stdout", stdout)
			assertNoSecret(t, "refusal stderr", stderr)
			lines, raw := readAudit(t, fx.home)
			assertNoSecret(t, "audit file", raw)
			if len(lines) != 1 || len(lines[0].Fields) != 0 || lines[0].Profile != tc.profile {
				t.Fatalf("refusal audit = %+v", lines)
			}
		})
	}
}

func TestCredentialProviderRefusesBadProtocol(t *testing.T) {
	newCredentialFixture(t, nil)
	for _, stdin := range []string{
		`not json`,
		`{"protocol":"agent-browser.plugin.v0","type":"credential.resolve","capability":"credential.read","request":{}}`,
		`{"protocol":"agent-browser.plugin.v1","type":"browser.launch","capability":"browser.provider","request":{}}`,
	} {
		reply, code, _, _ := runProvider(t, stdin)
		if code != 0 || reply.Success || reply.Error == "" {
			t.Fatalf("stdin %q: want protocol refusal, got %+v code=%d", stdin, reply, code)
		}
	}
}

// The credential never leaves without its audit line: an unwritable audit
// path refuses instead of handing out an unaudited secret.
func TestCredentialProviderFailsClosedWhenAuditUnwritable(t *testing.T) {
	fx := newCredentialFixture(t, []map[string]any{profileRow("site-e", "acct-e")})
	fx.writeAccount(t, "acct-e", "email=e@example.com\npassword="+fakeFilePass+"\n", 0o600)
	if err := os.WriteFile(filepath.Join(fx.home, "browser"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	reply, code, stdout, _ := runProvider(t, resolveRequest("site-e", nil))
	if code != 0 || reply.Success || reply.Credential != nil || !strings.Contains(reply.Error, "audit") {
		t.Fatalf("want audit refusal, got success=%v error=%q", reply.Success, reply.Error)
	}
	assertNoSecret(t, "refusal stdout", stdout)
}

func TestCredentialDoctorListsSourcesWithoutValues(t *testing.T) {
	fx := newCredentialFixture(t, []map[string]any{
		profileRow("bws-site", "acct-bws"),
		profileRow("file-site", "acct-file"),
		profileRow("missing-site", "acct-missing"),
		profileRow("plain-site", ""),
	})
	fx.writeBWSToken(t, 0o600)
	fx.installFakeBWS(t, map[string]string{"acct-bws": "username=u\npassword=" + fakeBWSPassword + "\n"})
	fx.writeAccount(t, "acct-file", "email=f@example.com\npassword="+fakeFilePass+"\ntotp_seed="+fakeTOTPSeed+"\n", 0o600)

	var out, errOut bytes.Buffer
	if code := runCredentialCLI([]string{"doctor", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, errOut.String())
	}
	assertNoSecret(t, "doctor json", out.String()+errOut.String())
	var report struct {
		Schema string `json:"schema"`
		BWS    struct {
			Installed bool   `json:"installed"`
			Token     string `json:"token"`
			List      string `json:"list"`
		} `json:"bws"`
		Profiles []struct {
			Profile       string `json:"profile"`
			CredentialRef string `json:"credential_ref"`
			Present       bool   `json:"present"`
			Source        string `json:"source"`
			Complete      bool   `json:"complete"`
			TOTP          bool   `json:"totp"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("doctor json: %v\n%s", err, out.String())
	}
	if report.Schema != "compound/browser-credential-doctor.v0.1" || !report.BWS.Installed || report.BWS.Token != "present" || report.BWS.List != "ok" {
		t.Fatalf("doctor header = %+v", report)
	}
	got := map[string]string{}
	for _, p := range report.Profiles {
		got[p.Profile] = strings.Join([]string{p.CredentialRef, p.Source, boolWord(p.Present), boolWord(p.Complete), boolWord(p.TOTP)}, "|")
	}
	want := map[string]string{
		"bws-site":     "acct-bws|bws|yes|yes|no",
		"file-site":    "acct-file|file|yes|yes|yes",
		"missing-site": "acct-missing|none|no|no|no",
		"plain-site":   "|none|no|no|no",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("profile %s = %q want %q", id, got[id], w)
		}
	}

	out.Reset()
	errOut.Reset()
	if code := runCredentialCLI([]string{"doctor"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor text exit %d", code)
	}
	text := out.String()
	assertNoSecret(t, "doctor text", text+errOut.String())
	for _, needle := range []string{"bws=installed", "token=present", "file-site credential_ref=acct-file present=true source=file", "totp=true", "missing-site credential_ref=acct-missing present=false source=none"} {
		if !strings.Contains(text, needle) {
			t.Errorf("doctor text missing %q:\n%s", needle, text)
		}
	}
	// The doctor reads, it never audits a credential hand-out.
	if _, err := os.Stat(credentialAuditPath(fx.home)); !os.IsNotExist(err) {
		t.Fatalf("doctor must not write the credential audit (stat err=%v)", err)
	}
}

func TestCredentialDoctorReportsInsecureToken(t *testing.T) {
	fx := newCredentialFixture(t, []map[string]any{profileRow("s", "acct-s")})
	fx.writeBWSToken(t, 0o644)
	fx.installFakeBWS(t, map[string]string{"acct-s": "username=u\npassword=" + fakeBWSPassword + "\n"})
	var out, errOut bytes.Buffer
	if code := runCredentialCLI([]string{"doctor"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor exit %d", code)
	}
	if !strings.Contains(out.String(), "token=insecure") || !strings.Contains(out.String(), "source=none") {
		t.Fatalf("insecure token must disable bws:\n%s", out.String())
	}
	assertNoSecret(t, "doctor text", out.String()+errOut.String())
}

func TestCredentialCLIRejectsUnknownUsage(t *testing.T) {
	newCredentialFixture(t, nil)
	var out, errOut bytes.Buffer
	if code := runCredentialCLI([]string{"show"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown credential verb exit = %d, want 2", code)
	}
	if code := runCredentialCLI([]string{"doctor", "--values"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown doctor flag exit = %d, want 2", code)
	}
	if code := runCredentialProvider([]string{"--bogus"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("unknown provider flag exit = %d, want 2", code)
	}
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
