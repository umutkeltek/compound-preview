package browser

// probe.go — browser self-heal, part 1: the profile registry rows the
// self-heal surface reads and writes, the one agent-browser client every
// self-heal verb uses, and `compound browser probe`.
//
// Isolation contract (Browser skill, Level 0 isolated_headless): every
// agent-browser call carries BOTH --session and --profile, the child
// environment pins AGENT_BROWSER_HEADED=false and drops the variables that
// would attach to another browser, and no call ever passes --headed, --cdp,
// --auto-connect or --allowed-domains (the last conflicts with --profile).
// A profile whose user_data_dir does not exist is pending enrollment: no
// browser is launched for it, because launching would create the directory
// and make an unenrolled identity look enrolled.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	profileRegistrySchema   = "compound.browser_profile_registry.v0.1"
	probeResultSchema       = "compound.browser_probe.v0.1"
	selfHealSessionPrefix   = "compound-heal-"
	agentBrowserCallTimeout = 90 * time.Second
	agentBrowserOutputLimit = 4 << 20
)

// selfHealNow is the clock every self-heal verb reads (UTC).
var selfHealNow = func() time.Time { return time.Now().UTC() }

var (
	selfHealIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	errProfileBusy    = errors.New("profile_busy: another self-heal task holds this profile")
)

// browserMarker says what a page must show. Every non-empty field must match:
// text (visible page text contains it), role and/or name (an accessibility
// node with that role whose name contains it), url_pattern (RE2 regexp on
// the current URL).
type browserMarker struct {
	Text       string `json:"text,omitempty"`
	Role       string `json:"role,omitempty"`
	Name       string `json:"name,omitempty"`
	URLPattern string `json:"url_pattern,omitempty"`
}

func (m *browserMarker) empty() bool {
	return m == nil || (strings.TrimSpace(m.Text) == "" && strings.TrimSpace(m.Role) == "" &&
		strings.TrimSpace(m.Name) == "" && strings.TrimSpace(m.URLPattern) == "")
}

type lastProbe struct {
	At           string `json:"at"`
	LoggedIn     bool   `json:"logged_in"`
	EvidencePath string `json:"evidence_path,omitempty"`
}

type healAttempt struct {
	At     string `json:"at"`
	Result string `json:"result"`
}

// selfHealProfile is the self-heal view of one browser-profile registry row.
// The added fields are optional, so older rows keep decoding; fields this
// surface does not own are preserved byte-for-byte on every write.
type selfHealProfile struct {
	ID              string         `json:"id"`
	UserDataDir     string         `json:"user_data_dir"`
	LoginCheckURL   string         `json:"login_check_url,omitempty"`
	LoggedInMarker  *browserMarker `json:"logged_in_marker,omitempty"`
	TwoFactorMarker *browserMarker `json:"two_factor_marker,omitempty"`
	LoginRecipe     string         `json:"login_recipe,omitempty"`
	CredentialRef   string         `json:"credential_ref,omitempty"`
	LastProbe       *lastProbe     `json:"last_probe,omitempty"`
	HealAttempts    []healAttempt  `json:"heal_attempts,omitempty"`
}

func profileRegistryFile(home string) string {
	return filepath.Join(home, "registries", "browser-profiles.json")
}

func selfHealDir(home string, parts ...string) string {
	return filepath.Join(append([]string{home, "browser"}, parts...)...)
}

func validSelfHealID(kind, id string) error {
	if !selfHealIDPattern.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid %s %q", kind, id)
	}
	return nil
}

// readProfileRegistry returns the registry's raw top-level object and rows.
// A present file of another schema or shape fails closed.
func readProfileRegistry(home string) (map[string]json.RawMessage, []map[string]json.RawMessage, error) {
	body, err := os.ReadFile(profileRegistryFile(home))
	if err != nil {
		return nil, nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, nil, fmt.Errorf("decode browser-profile registry: %w", err)
	}
	var schema string
	if err := json.Unmarshal(top["schema_version"], &schema); err != nil || schema != profileRegistrySchema {
		return nil, nil, fmt.Errorf("browser-profile registry schema %q is not %q", schema, profileRegistrySchema)
	}
	var rows []map[string]json.RawMessage
	if raw := top["profiles"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, nil, fmt.Errorf("decode browser-profile rows: %w", err)
		}
	}
	return top, rows, nil
}

func decodeSelfHealProfile(row map[string]json.RawMessage) (selfHealProfile, error) {
	body, err := json.Marshal(row)
	if err != nil {
		return selfHealProfile{}, err
	}
	var p selfHealProfile
	if err := json.Unmarshal(body, &p); err != nil {
		return selfHealProfile{}, fmt.Errorf("decode browser-profile row: %w", err)
	}
	return p, nil
}

func loadSelfHealProfiles(home string) ([]selfHealProfile, error) {
	_, rows, err := readProfileRegistry(home)
	if err != nil {
		return nil, err
	}
	out := make([]selfHealProfile, 0, len(rows))
	for _, row := range rows {
		p, err := decodeSelfHealProfile(row)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func loadSelfHealProfile(home, id string) (selfHealProfile, error) {
	profiles, err := loadSelfHealProfiles(home)
	if err != nil {
		return selfHealProfile{}, err
	}
	for _, p := range profiles {
		if p.ID == id {
			return p, nil
		}
	}
	return selfHealProfile{}, fmt.Errorf("profile_unknown: no browser-profile registry row %q", id)
}

// updateProfileRow rewrites exactly one row's self-heal keys under the
// registry lock, preserving every other key and row, and swaps the file in
// atomically.
func updateProfileRow(home, id string, set map[string]any) error {
	if err := os.MkdirAll(selfHealDir(home), 0o755); err != nil {
		return err
	}
	lock, err := lockSelfHealFile(selfHealDir(home, ".registry.lock"), true)
	if err != nil {
		return fmt.Errorf("lock browser-profile registry: %w", err)
	}
	defer unlockSelfHealFile(lock)
	top, rows, err := readProfileRegistry(home)
	if err != nil {
		return err
	}
	found := false
	for _, row := range rows {
		var rowID string
		if json.Unmarshal(row["id"], &rowID) != nil || rowID != id {
			continue
		}
		found = true
		for key, value := range set {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			row[key] = raw
		}
	}
	if !found {
		return fmt.Errorf("profile_unknown: no browser-profile registry row %q", id)
	}
	rowBodies := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		rowBodies = append(rowBodies, orderedJSON(row, "id", "user_data_dir", "backends", "logins", "status", "updated_at"))
	}
	profiles, err := json.Marshal(rowBodies)
	if err != nil {
		return err
	}
	top["profiles"] = profiles
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, orderedJSON(top, "schema_version", "updated_at", "profiles"), "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	return writeFileAtomic(profileRegistryFile(home), pretty.Bytes(), 0o644)
}

// orderedJSON renders an object with the leading keys first, then the rest
// sorted, so a registry write keeps the order the seed door produces.
func orderedJSON(m map[string]json.RawMessage, first ...string) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	seen := map[string]bool{}
	write := func(key string) {
		raw, ok := m[key]
		if !ok || seen[key] {
			return
		}
		seen[key] = true
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(raw)
	}
	for _, key := range first {
		write(key)
	}
	rest := make([]string, 0, len(m))
	for key := range m {
		rest = append(rest, key)
	}
	sort.Strings(rest)
	for _, key := range rest {
		write(key)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func expandUserDir(dir string) string {
	if strings.HasPrefix(dir, "~/") {
		if userHome, err := os.UserHomeDir(); err == nil {
			return filepath.Join(userHome, dir[2:])
		}
	}
	return dir
}

// profileEnrolled reads enrollment from disk, as the registry doctor does:
// the operator's one-time headed sign-in is what creates user_data_dir.
func profileEnrolled(p selfHealProfile) bool {
	if strings.TrimSpace(p.UserDataDir) == "" {
		return false
	}
	info, err := os.Stat(expandUserDir(p.UserDataDir))
	return err == nil && info.IsDir()
}

func enrollmentLabel(p selfHealProfile) string {
	if profileEnrolled(p) {
		return "enrolled"
	}
	return "pending_enrollment"
}

// lockProfile enforces one self-heal task per profile at a time.
func lockProfile(home, id string) (*os.File, error) {
	dir := selfHealDir(home, "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lock, err := lockSelfHealFile(filepath.Join(dir, id+".lock"), false)
	if err != nil {
		return nil, errProfileBusy
	}
	return lock, nil
}

// --- agent-browser client ---------------------------------------------------

// agentBrowserEnvDrop lists variables that would make a call visible, attach
// it to another browser, or override the session/profile we pass.
var agentBrowserEnvDrop = map[string]bool{
	"AGENT_BROWSER_HEADED": true, "AGENT_BROWSER_AUTO_CONNECT": true, "AGENT_BROWSER_CDP": true,
	"AGENT_BROWSER_PROVIDER": true, "AGENT_BROWSER_ALLOWED_DOMAINS": true, "AGENT_BROWSER_SESSION": true,
	"AGENT_BROWSER_PROFILE": true, "AGENT_BROWSER_SESSION_NAME": true, "AGENT_BROWSER_RESTORE": true,
	"AGENT_BROWSER_STATE": true, "AGENT_BROWSER_JSON": true,
}

type agentBrowserClient struct {
	bin     string
	session string
	profile string
	dir     string
}

func newAgentBrowserClient(home string, p selfHealProfile) (*agentBrowserClient, error) {
	bin, err := exec.LookPath("agent-browser")
	if err != nil {
		return nil, errors.New("agent_browser_missing: agent-browser is not on PATH")
	}
	dir := selfHealDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &agentBrowserClient{bin: bin, session: selfHealSessionPrefix + p.ID, profile: expandUserDir(p.UserDataDir), dir: dir}, nil
}

func (c *agentBrowserClient) env() []string {
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if agentBrowserEnvDrop[name] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "AGENT_BROWSER_HEADED=false")
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// call runs one agent-browser command with --json and returns the decoded
// `data` member. stdin, when set, carries values that must not appear in argv.
func (c *agentBrowserClient) call(ctx context.Context, stdin []byte, args ...string) (json.RawMessage, error) {
	raw, err := c.exec(ctx, stdin, args...)
	if err != nil && len(bytes.TrimSpace(raw)) == 0 {
		return nil, err
	}
	var resp struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   json.RawMessage `json:"error"`
	}
	if decodeErr := json.Unmarshal(bytes.TrimSpace(raw), &resp); decodeErr != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("agent-browser %s: undecodable output", args[0])
	}
	if !resp.Success {
		var msg string
		_ = json.Unmarshal(resp.Error, &msg)
		if msg == "" {
			msg = "command failed"
		}
		return nil, fmt.Errorf("agent-browser %s: %s", args[0], truncateDetail(msg))
	}
	return resp.Data, nil
}

// agentBrowserReconnectDelays: agent-browser 0.38 intermittently answers a
// command issued right after `close` on the same session with "Failed to
// connect" while the old daemon is still exiting (seen 2026-10-07); the
// command never reached a daemon, so a delayed retry is safe.
var agentBrowserReconnectDelays = []time.Duration{time.Second, 2 * time.Second}

func (c *agentBrowserClient) exec(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	out, err := c.execOnce(ctx, stdin, args...)
	for _, delay := range agentBrowserReconnectDelays {
		if err == nil || !bytes.Contains(out, []byte("Failed to connect")) {
			break
		}
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(delay):
		}
		out, err = c.execOnce(ctx, stdin, args...)
	}
	return out, err
}

func (c *agentBrowserClient) execOnce(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, agentBrowserCallTimeout)
	defer cancel()
	argv := append([]string{"--session", c.session, "--profile", c.profile, "--json"}, args...)
	cmd := exec.CommandContext(callCtx, c.bin, argv...)
	cmd.Env = c.env()
	cmd.Dir = c.dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := &limitedBuffer{limit: agentBrowserOutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = &limitedBuffer{limit: 4096}
	err := cmd.Run()
	if callCtx.Err() != nil {
		return stdout.Bytes(), fmt.Errorf("agent-browser %s: timed out", args[0])
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("agent-browser %s: %v", args[0], err)
	}
	return stdout.Bytes(), nil
}

func (c *agentBrowserClient) open(ctx context.Context, url string) error {
	_, err := c.call(ctx, nil, "open", url)
	return err
}

func (c *agentBrowserClient) currentURL(ctx context.Context) (string, error) {
	data, err := c.call(ctx, nil, "get", "url")
	if err != nil {
		return "", err
	}
	var v struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("agent-browser get url: %w", err)
	}
	return v.URL, nil
}

func (c *agentBrowserClient) pageText(ctx context.Context) (string, error) {
	data, err := c.call(ctx, nil, "get", "text", "body")
	if err != nil {
		return "", err
	}
	var v struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("agent-browser get text: %w", err)
	}
	return v.Text, nil
}

type axNode struct {
	Ref  string
	Role string `json:"role"`
	Name string `json:"name"`
}

// snapshot returns the interactive accessibility nodes in document order
// (agent-browser numbers refs e1..eN as it walks the tree).
func (c *agentBrowserClient) snapshot(ctx context.Context) ([]axNode, error) {
	data, err := c.call(ctx, nil, "snapshot", "-i")
	if err != nil {
		return nil, err
	}
	var v struct {
		Refs map[string]axNode `json:"refs"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("agent-browser snapshot: %w", err)
	}
	nodes := make([]axNode, 0, len(v.Refs))
	for ref, node := range v.Refs {
		node.Ref = ref
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return refOrdinal(nodes[i].Ref) < refOrdinal(nodes[j].Ref) })
	return nodes, nil
}

func refOrdinal(ref string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(ref, "e"))
	if err != nil {
		return 1 << 30
	}
	return n
}

func (c *agentBrowserClient) screenshot(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := c.call(ctx, nil, "screenshot", path)
	return err
}

func (c *agentBrowserClient) close() {
	_, _ = c.exec(context.Background(), nil, "close")
}

var reasonPrefix = regexp.MustCompile(`^([a-z][a-z_]+):`)

// reasonOf extracts the machine reason a self-heal error leads with
// ("profile_not_enrolled: ..."), or "error" for a plain failure.
func reasonOf(err error) string {
	if m := reasonPrefix.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return "error"
}

func truncateDetail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// --- markers + probe ----------------------------------------------------------

// evaluateMarker checks the current page (no navigation) against a marker.
func evaluateMarker(ctx context.Context, c *agentBrowserClient, m *browserMarker) (bool, error) {
	if m.empty() {
		return false, errors.New("marker is empty")
	}
	if pattern := strings.TrimSpace(m.URLPattern); pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return false, fmt.Errorf("marker url_pattern: %w", err)
		}
		url, err := c.currentURL(ctx)
		if err != nil {
			return false, err
		}
		if !re.MatchString(url) {
			return false, nil
		}
	}
	if text := normalizeText(m.Text); text != "" {
		page, err := c.pageText(ctx)
		if err != nil {
			return false, err
		}
		if !strings.Contains(normalizeText(page), text) {
			return false, nil
		}
	}
	if strings.TrimSpace(m.Role) != "" || strings.TrimSpace(m.Name) != "" {
		nodes, err := c.snapshot(ctx)
		if err != nil {
			return false, err
		}
		role, name := normalizeText(m.Role), normalizeText(m.Name)
		hit := false
		for _, n := range nodes {
			if (role == "" || normalizeText(n.Role) == role) && (name == "" || strings.Contains(normalizeText(n.Name), name)) {
				hit = true
				break
			}
		}
		if !hit {
			return false, nil
		}
	}
	return true, nil
}

type probeResult struct {
	Schema       string `json:"schema"`
	Profile      string `json:"profile"`
	At           string `json:"at"`
	LoggedIn     bool   `json:"logged_in"`
	EvidencePath string `json:"evidence_path,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

func probeConfigError(p selfHealProfile) string {
	switch {
	case strings.TrimSpace(p.LoginCheckURL) == "":
		return "login_check_url_missing: registry row has no login_check_url"
	case !strings.HasPrefix(p.LoginCheckURL, "https://") && !strings.HasPrefix(p.LoginCheckURL, "http://"):
		return "login_check_url_invalid: login_check_url must be http(s)"
	case p.LoggedInMarker.empty():
		return "logged_in_marker_missing: registry row has no logged_in_marker"
	case !profileEnrolled(p):
		return "profile_not_enrolled: user_data_dir does not exist; the operator enrolls it once at Level 1 (headed sign-in)"
	}
	return ""
}

// probeOnce opens the profile's check URL, decides logged-in from the
// marker, and takes an evidence screenshot. It does not touch the registry.
func probeOnce(ctx context.Context, home string, c *agentBrowserClient, p selfHealProfile) (probeResult, error) {
	now := selfHealNow()
	res := probeResult{Schema: probeResultSchema, Profile: p.ID, At: now.Format(time.RFC3339Nano)}
	if err := c.open(ctx, p.LoginCheckURL); err != nil {
		return res, err
	}
	loggedIn, err := evaluateMarker(ctx, c, p.LoggedInMarker)
	if err != nil {
		return res, err
	}
	res.LoggedIn = loggedIn
	shot := filepath.Join(selfHealDir(home, "evidence", p.ID), "probe-"+now.Format("20060102T150405.000000000Z")+".png")
	if err := c.screenshot(ctx, shot); err != nil {
		return res, err
	}
	res.EvidencePath = shot
	return res, nil
}

func recordProbe(home string, res probeResult) error {
	return updateProfileRow(home, res.Profile, map[string]any{
		"last_probe": lastProbe{At: res.At, LoggedIn: res.LoggedIn, EvidencePath: res.EvidencePath},
	})
}

// runProbe is `compound browser probe`: exit 0 logged in, 3 not logged in,
// 2 error (including an unenrolled or misconfigured profile).
func runProbe(ctx context.Context, home, id string) (probeResult, int) {
	res := probeResult{Schema: probeResultSchema, Profile: id, At: selfHealNow().Format(time.RFC3339Nano)}
	fail := func(err error) (probeResult, int) {
		res.Error = err.Error()
		res.Reason = reasonOf(err)
		return res, 2
	}
	p, err := loadSelfHealProfile(home, id)
	if err != nil {
		return fail(err)
	}
	if msg := probeConfigError(p); msg != "" {
		return fail(errors.New(msg))
	}
	lock, err := lockProfile(home, id)
	if err != nil {
		return fail(err)
	}
	defer unlockSelfHealFile(lock)
	client, err := newAgentBrowserClient(home, p)
	if err != nil {
		return fail(err)
	}
	defer client.close()
	probed, err := probeOnce(ctx, home, client, p)
	if err != nil {
		return fail(err)
	}
	if err := recordProbe(home, probed); err != nil {
		return fail(err)
	}
	if !probed.LoggedIn {
		return probed, 3
	}
	return probed, 0
}
