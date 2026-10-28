package browser

// credential_provider.go — `compound browser credential-provider` and
// `compound browser credential doctor`.
//
// The provider is an agent-browser `credential.read` plugin (protocol
// agent-browser.plugin.v1, cli/src/plugins.rs in agent-browser 0.38.2):
// agent-browser spawns the configured command, writes ONE JSON envelope to
// stdin, closes stdin, discards stderr, waits up to 15 s, and parses stdout
// as ONE JSON object. A non-zero exit is "exited unsuccessfully"; a
// `success:false` reply is a refusal whose error text agent-browser keeps out
// of user-facing errors. So every protocol outcome here exits 0 with exactly
// one JSON object on stdout; only CLI misuse exits 2.
//
// Resolution per registered browser profile (registries/browser-profiles.json
// `credential_ref`, a NAME): (a) the Bitwarden Secrets Manager secret whose
// key equals the ref, when `bws` is on PATH and secrets/bws-access-token is
// an owner-only regular file; (b) secrets/accounts/<ref>. Both hold the same
// `key=value` lines. TOTP is pure Go (RFC 6238, oathtool -b defaults), so no
// binary dependency. Every hand-out or refusal appends one names-only line to
// browser/credential-audit.jsonl; a credential never leaves unaudited.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"compound/internal/harness"
	"compound/internal/home"
)

const (
	agentBrowserPluginProtocol = "agent-browser.plugin.v1"
	credentialDoctorSchema     = "compound/browser-credential-doctor.v0.1"
	credentialProviderUsage    = "usage: compound browser credential-provider [--home DIR]   (agent-browser credential.read plugin; JSON on stdin/stdout)\n" +
		"       compound browser credential doctor [--json] [--home DIR]"
	maxCredentialInput = 64 << 10
	bwsListTimeout     = 10 * time.Second
)

// credentialNow is the TOTP clock; tests pin it.
var credentialNow = time.Now

var credentialRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)

func credentialAuditPath(homePath string) string {
	return filepath.Join(homePath, "browser", "credential-audit.jsonl")
}

func bwsTokenPath(homePath string) string {
	return filepath.Join(homePath, "secrets", "bws-access-token")
}

type pluginEnvelope struct {
	Protocol   string          `json:"protocol"`
	Type       string          `json:"type"`
	Capability string          `json:"capability"`
	Request    json.RawMessage `json:"request"`
}

type credentialResolveRequest struct {
	ProfileName string  `json:"profileName"`
	ItemRef     *string `json:"itemRef"`
	URL         *string `json:"url"`
}

// providedCredential is agent-browser's ResolvedCredential (camelCase serde).
type providedCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
	URL      string `json:"url,omitempty"`
	OTP      string `json:"otp,omitempty"`
}

type credentialAuditEntry struct {
	At      string   `json:"at"`
	Profile string   `json:"profile"`
	RefName string   `json:"ref_name"`
	Source  string   `json:"source"`
	Fields  []string `json:"fields"`
	Outcome string   `json:"outcome"`
	Reason  string   `json:"reason,omitempty"`
}

// runCredentialProvider is the plugin entry point. It returns the exit code.
func runCredentialProvider(args []string, in io.Reader, out, errOut io.Writer) int {
	values, _, err := parseCredentialArgs(args, nil, []string{"--home"})
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		fmt.Fprintln(errOut, credentialProviderUsage)
		return 2
	}
	reply := func(body map[string]any) int {
		body["protocol"] = agentBrowserPluginProtocol
		_ = json.NewEncoder(out).Encode(body)
		return 0
	}
	refuse := func(reason string) int {
		return reply(map[string]any{"success": false, "error": reason})
	}
	raw, err := io.ReadAll(io.LimitReader(in, maxCredentialInput+1))
	if err != nil || len(raw) > maxCredentialInput {
		return refuse("request could not be read within limit")
	}
	var envelope pluginEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return refuse("request is not a JSON plugin envelope")
	}
	if envelope.Protocol != agentBrowserPluginProtocol {
		return refuse("unsupported protocol")
	}
	switch envelope.Type {
	case "plugin.manifest":
		return reply(map[string]any{"success": true, "manifest": map[string]any{
			"name":         "compound",
			"capabilities": []string{"credential.read"},
			"description":  "Compound browser-profile credentials (bws, then secrets/accounts)",
		}})
	case "credential.resolve":
		if envelope.Capability != "credential.read" {
			return refuse("credential.resolve requires the credential.read capability")
		}
	default:
		return refuse("unsupported request type")
	}
	var request credentialResolveRequest
	if len(envelope.Request) == 0 || json.Unmarshal(envelope.Request, &request) != nil {
		return refuse("credential.resolve request is malformed")
	}
	homePath, err := home.Resolve(values["--home"])
	if err != nil {
		return refuse("compound home could not be resolved")
	}
	ctx, cancel := context.WithTimeout(context.Background(), bwsListTimeout)
	defer cancel()
	cred, entry, resolveErr := resolveProfileCredential(ctx, homePath, request)
	if resolveErr != nil {
		entry.Outcome, entry.Reason, entry.Fields, entry.Source = "refused", resolveErr.Error(), []string{}, "none"
		if auditErr := appendCredentialAudit(homePath, entry); auditErr != nil {
			return refuse(resolveErr.Error() + "; credential audit unavailable")
		}
		return refuse(resolveErr.Error())
	}
	entry.Outcome = "served"
	if err := appendCredentialAudit(homePath, entry); err != nil {
		return refuse("credential audit could not be written; refusing an unaudited credential")
	}
	return reply(map[string]any{"success": true, "credential": cred})
}

// resolveProfileCredential maps a registered profile to its credential. The
// returned entry is always populated with what is known (names only).
func resolveProfileCredential(ctx context.Context, homePath string, request credentialResolveRequest) (providedCredential, credentialAuditEntry, error) {
	profileID := strings.TrimSpace(request.ProfileName)
	if len(profileID) > 256 {
		profileID = profileID[:256]
	}
	entry := credentialAuditEntry{At: time.Now().UTC().Format(time.RFC3339Nano), Profile: profileID, Fields: []string{}}
	if profileID == "" {
		return providedCredential{}, entry, errors.New("request names no profile")
	}
	profile, err := findCredentialProfile(homePath, profileID)
	if err != nil {
		return providedCredential{}, entry, err
	}
	ref := profile.CredentialRef
	entry.RefName = ref
	if ref == "" {
		return providedCredential{}, entry, fmt.Errorf("profile %q has no credential_ref", profileID)
	}
	if !validCredentialRef(ref) {
		return providedCredential{}, entry, fmt.Errorf("profile %q has an invalid credential_ref", profileID)
	}
	if request.ItemRef != nil && *request.ItemRef != "" && *request.ItemRef != ref {
		return providedCredential{}, entry, fmt.Errorf("itemRef does not match the credential_ref of profile %q", profileID)
	}
	bws := loadBWSListing(ctx, homePath)
	material, err := lookupCredential(homePath, ref, bws)
	if err != nil {
		return providedCredential{}, entry, err
	}
	cred, served, err := buildCredential(material.fields, credentialNow())
	if err != nil {
		return providedCredential{}, entry, fmt.Errorf("%s credential for ref %q: %w", material.source, ref, err)
	}
	entry.Source, entry.Fields = material.source, served
	return cred, entry, nil
}

func findCredentialProfile(homePath, profileID string) (harness.BrowserProfile, error) {
	read, err := harness.LoadBrowserProfileRegistryState(homePath)
	if err != nil {
		return harness.BrowserProfile{}, errors.New("browser-profile registry is unreadable")
	}
	if read.State != harness.BrowserProfileRegistryPopulated {
		return harness.BrowserProfile{}, fmt.Errorf("browser-profile registry is %s; profile %q is not registered", read.State, profileID)
	}
	for _, p := range read.Registry.Profiles {
		if p.ID == profileID {
			return p, nil
		}
	}
	return harness.BrowserProfile{}, fmt.Errorf("profile %q is not registered", profileID)
}

func validCredentialRef(ref string) bool {
	return credentialRefPattern.MatchString(ref) && !strings.Contains(ref, "..")
}

// bwsListing is one `bws secret list` read. Values stay in memory only.
type bwsListing struct {
	Installed bool
	Token     string // absent | present | insecure | unreadable
	List      string // ok | error | skipped
	secrets   map[string][]string
}

func loadBWSListing(ctx context.Context, homePath string) bwsListing {
	listing := bwsListing{List: "skipped"}
	bwsPath, lookErr := exec.LookPath("bws")
	listing.Installed = lookErr == nil
	token, status := readBWSToken(homePath)
	listing.Token = status
	if !listing.Installed || status != "present" {
		return listing
	}
	cmd := exec.CommandContext(ctx, bwsPath, "secret", "list", "--output", "json", "--color", "no")
	cmd.Env = append(withoutEnv(os.Environ(), "BWS_ACCESS_TOKEN"), "BWS_ACCESS_TOKEN="+token)
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		listing.List = "error"
		return listing
	}
	var rows []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		listing.List = "error"
		return listing
	}
	listing.List = "ok"
	listing.secrets = map[string][]string{}
	for _, row := range rows {
		listing.secrets[row.Key] = append(listing.secrets[row.Key], row.Value)
	}
	return listing
}

func readBWSToken(homePath string) (string, string) {
	path := bwsTokenPath(homePath)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", "absent"
	}
	if err != nil {
		return "", "unreadable"
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 4096 {
		return "", "insecure"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "unreadable"
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return "", "absent"
	}
	return token, "present"
}

func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

type credentialMaterial struct {
	source string // bws | file
	fields map[string]string
}

// lookupCredential tries bws, then the flat file. Errors name sources and
// states, never values.
func lookupCredential(homePath, ref string, bws bwsListing) (credentialMaterial, error) {
	var notes []string
	switch {
	case bws.List == "ok":
		values := bws.secrets[ref]
		if len(values) > 1 {
			return credentialMaterial{}, fmt.Errorf("bws holds %d secrets named %q; refusing an ambiguous credential", len(values), ref)
		}
		if len(values) == 1 {
			return credentialMaterial{source: "bws", fields: parseCredentialLines(values[0])}, nil
		}
		notes = append(notes, "bws: no secret named ref")
	case !bws.Installed:
		notes = append(notes, "bws: not installed")
	case bws.Token != "present":
		notes = append(notes, "bws: token "+bws.Token)
	default:
		notes = append(notes, "bws: list "+bws.List)
	}
	fields, state := readAccountFile(homePath, ref)
	if state == "ok" {
		return credentialMaterial{source: "file", fields: fields}, nil
	}
	notes = append(notes, "file: "+state)
	return credentialMaterial{}, fmt.Errorf("no credential source for ref %q (%s)", ref, strings.Join(notes, "; "))
}

func readAccountFile(homePath, ref string) (map[string]string, string) {
	path := filepath.Join(homePath, "secrets", "accounts", ref)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, "absent"
	}
	if err != nil {
		return nil, "unreadable"
	}
	if !info.Mode().IsRegular() {
		return nil, "not a regular file"
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, "not owner-only"
	}
	if info.Size() > maxCredentialInput {
		return nil, "too large"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "unreadable"
	}
	return parseCredentialLines(string(data)), "ok"
}

// parseCredentialLines reads the secrets/accounts `key=value` format: blank
// lines and `#` comments skipped, split at the first `=`, value kept verbatim.
func parseCredentialLines(body string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = value
	}
	return fields
}

// usernameFields is the login-name precedence: an explicit username, then the
// identifiers existing secrets/accounts files already carry.
var usernameFields = []string{"username", "email", "apple_id", "handle"}

// buildCredential maps fields to the protocol shape and lists the served
// field names in protocol order.
func buildCredential(fields map[string]string, now time.Time) (providedCredential, []string, error) {
	var cred providedCredential
	for _, name := range usernameFields {
		if v := fields[name]; strings.TrimSpace(v) != "" {
			cred.Username = v
			break
		}
	}
	cred.Password = fields["password"]
	if strings.TrimSpace(cred.Username) == "" || cred.Password == "" {
		return providedCredential{}, nil, errors.New("incomplete credential (needs a username and a password)")
	}
	served := []string{"username", "password"}
	if url := strings.TrimSpace(fields["login_url"]); url != "" {
		cred.URL = url
		served = append(served, "url")
	}
	if seed := fields["totp_seed"]; strings.TrimSpace(seed) != "" {
		code, err := totpFromBase32(seed, now)
		if err != nil {
			return providedCredential{}, nil, err
		}
		cred.OTP = code
		served = append(served, "otp")
	}
	return cred, served, nil
}

// totpFromBase32 is `oathtool --totp -b SEED`: base32 seed (spaces and
// padding tolerated, any case), HMAC-SHA1, 30 s step, 6 digits.
func totpFromBase32(seed string, now time.Time) (string, error) {
	normalized := strings.TrimRight(strings.ToUpper(strings.Join(strings.Fields(seed), "")), "=")
	if normalized == "" {
		return "", errors.New("totp_seed is empty")
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalized)
	if err != nil || len(key) == 0 {
		return "", errors.New("totp_seed is not valid base32")
	}
	return totpCode(key, now.Unix(), 6, sha1.New), nil
}

// totpCode is RFC 6238 TOTP (RFC 4226 dynamic truncation) with T0=0, X=30.
func totpCode(key []byte, unix int64, digits int, newHash func() hash.Hash) string {
	if unix < 0 {
		unix = 0
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(unix/30))
	mac := hmac.New(newHash, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 | uint32(sum[offset+1])<<16 | uint32(sum[offset+2])<<8 | uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// appendCredentialAudit appends one names-only JSON line with a single write.
func appendCredentialAudit(homePath string, entry credentialAuditEntry) error {
	path := credentialAuditPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(line, '\n'))
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

type credentialDoctorReport struct {
	Schema        string                `json:"schema"`
	Generated     string                `json:"generated_at"`
	RegistryState string                `json:"registry_state"`
	BWS           credentialDoctorBWS   `json:"bws"`
	Profiles      []credentialDoctorRow `json:"profiles"`
}

type credentialDoctorBWS struct {
	Installed bool   `json:"installed"`
	Token     string `json:"token"`
	List      string `json:"list"`
}

type credentialDoctorRow struct {
	Profile       string `json:"profile"`
	CredentialRef string `json:"credential_ref"`
	Present       bool   `json:"present"`
	Source        string `json:"source"`
	Complete      bool   `json:"complete"`
	TOTP          bool   `json:"totp"`
	Note          string `json:"note,omitempty"`
}

// runCredentialCLI serves `compound browser credential doctor`. Read-only: it
// never prints a value and never writes the hand-out audit.
func runCredentialCLI(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "doctor" {
		fmt.Fprintln(errOut, credentialProviderUsage)
		return 2
	}
	values, switches, err := parseCredentialArgs(args[1:], []string{"--json"}, []string{"--home"})
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		fmt.Fprintln(errOut, credentialProviderUsage)
		return 2
	}
	homePath, err := home.Resolve(values["--home"])
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), bwsListTimeout)
	defer cancel()
	report, err := credentialDoctor(ctx, homePath)
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return 1
	}
	if switches["--json"] {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return 0
	}
	installed := "missing"
	if report.BWS.Installed {
		installed = "installed"
	}
	fmt.Fprintf(out, "# browser credential doctor registry_state=%s profiles=%d bws=%s token=%s list=%s\n",
		report.RegistryState, len(report.Profiles), installed, report.BWS.Token, report.BWS.List)
	for _, row := range report.Profiles {
		ref := row.CredentialRef
		if ref == "" {
			ref = "-"
		}
		fmt.Fprintf(out, "- %s credential_ref=%s present=%t source=%s complete=%t totp=%t", row.Profile, ref, row.Present, row.Source, row.Complete, row.TOTP)
		if row.Note != "" {
			fmt.Fprintf(out, " note=%q", row.Note)
		}
		fmt.Fprintln(out)
	}
	return 0
}

func credentialDoctor(ctx context.Context, homePath string) (credentialDoctorReport, error) {
	read, err := harness.LoadBrowserProfileRegistryState(homePath)
	if err != nil {
		return credentialDoctorReport{}, err
	}
	bws := loadBWSListing(ctx, homePath)
	report := credentialDoctorReport{
		Schema: credentialDoctorSchema, Generated: time.Now().UTC().Format(time.RFC3339Nano),
		RegistryState: read.State, BWS: credentialDoctorBWS{Installed: bws.Installed, Token: bws.Token, List: bws.List},
		Profiles: []credentialDoctorRow{},
	}
	for _, p := range read.Registry.Profiles {
		row := credentialDoctorRow{Profile: p.ID, CredentialRef: p.CredentialRef, Source: "none"}
		switch {
		case p.CredentialRef == "":
			row.Note = "no credential_ref"
		case !validCredentialRef(p.CredentialRef):
			row.Note = "invalid credential_ref"
		default:
			material, lookErr := lookupCredential(homePath, p.CredentialRef, bws)
			if lookErr != nil {
				row.Note = lookErr.Error()
				break
			}
			row.Present, row.Source = true, material.source
			_, served, buildErr := buildCredential(material.fields, credentialNow())
			if buildErr != nil {
				row.Note = buildErr.Error()
				break
			}
			row.Complete = true
			for _, name := range served {
				row.TOTP = row.TOTP || name == "otp"
			}
		}
		report.Profiles = append(report.Profiles, row)
	}
	return report, nil
}

// parseCredentialArgs accepts only the named switches and single-value flags.
func parseCredentialArgs(args, switchNames, valueNames []string) (map[string]string, map[string]bool, error) {
	values, switches := map[string]string{}, map[string]bool{}
	isSwitch := map[string]bool{}
	for _, name := range switchNames {
		isSwitch[name] = true
	}
	isValue := map[string]bool{}
	for _, name := range valueNames {
		isValue[name] = true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case isSwitch[arg] && !switches[arg]:
			switches[arg] = true
		case isValue[arg] && values[arg] == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") && args[i+1] != "":
			values[arg] = args[i+1]
			i++
		default:
			return nil, nil, fmt.Errorf("unexpected argument %q", arg)
		}
	}
	return values, switches, nil
}
