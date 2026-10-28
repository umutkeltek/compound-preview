package browser

// heal.go — browser self-heal, part 3: `compound browser heal` and the
// self-heal section of `compound browser doctor`.
//
// heal = probe; when logged out, run the profile's login_recipe with
// credentials from `compound browser credential-provider` (a subprocess on
// PATH spoken to in agent-browser's plugin protocol; values go to
// agent-browser on stdin only), then re-probe. It never
// loops: one login attempt per invocation, at most two attempts per profile
// per six hours (persisted as heal_attempts in the registry row, recorded
// BEFORE the page is driven so a crash still counts). Anything that needs
// the operator — exhausted attempts, a 2FA prompt, a missing provider or
// recipe, a failed login — appends one {at, profile, reason, next_action}
// line to $COMPOUND_HOME/browser/return-queue.jsonl (deduplicated per
// profile and reason inside the window) and exits 3.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/home"
)

const (
	healResultSchema       = "compound.browser_heal.v0.1"
	selfHealDoctorSchema   = "compound.browser_self_heal_doctor.v0.1"
	selfHealWindow         = 6 * time.Hour
	maxHealAttemptsWindow  = 2
	healAttemptRetention   = 7 * 24 * time.Hour
	maxHealAttemptsKept    = 20
	credentialProviderWait = 30 * time.Second
)

type returnQueueEntry struct {
	At         string `json:"at"`
	Profile    string `json:"profile"`
	Reason     string `json:"reason"`
	NextAction string `json:"next_action"`
}

func returnQueuePath(home string) string { return selfHealDir(home, "return-queue.jsonl") }

func nextActionFor(profile, reason string) string {
	switch reason {
	case "heal_attempts_exhausted":
		return fmt.Sprintf("Automatic heals for %s are paused for this 6h window. Sign in once at Level 1 (headed, operator present) or fix the login recipe, then run `compound browser heal --profile %s`.", profile, profile)
	case "two_factor_required":
		return fmt.Sprintf("%s asked for a second factor. Complete it at Level 1 while present, then run `compound browser probe --profile %s`.", profile, profile)
	case "credential_provider_missing":
		return fmt.Sprintf("No `compound browser credential-provider` on PATH; install a build that has it (and put it on the scheduler's PATH), then run `compound browser heal --profile %s`.", profile)
	case "credential_unavailable":
		return fmt.Sprintf("The credential provider could not return a field the %s login recipe needs; check that entry. No login was attempted.", profile)
	case "login_recipe_missing":
		return fmt.Sprintf("Record a login recipe under browser/recipes/%s/ and set login_recipe on the registry row.", profile)
	case "login_failed":
		return fmt.Sprintf("The %s login recipe ran but the probe still reads logged out. Read the latest heal_login run receipt (compound browser doctor), fix the recipe or the stored credentials.", profile)
	case "profile_not_enrolled":
		return fmt.Sprintf("Enroll %s once: the operator signs in at Level 1 (headed) with this profile. Never automatically.", profile)
	}
	return fmt.Sprintf("Inspect `compound browser doctor` for %s.", profile)
}

func readReturnQueue(home string) ([]returnQueueEntry, error) {
	file, err := os.Open(returnQueuePath(home))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []returnQueueEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		var e returnQueueEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil && e.Profile != "" {
			out = append(out, e)
		}
	}
	return out, scanner.Err()
}

// enqueueReturn appends one return-queue line unless the same profile and
// reason were queued inside the heal window. It reports whether it wrote.
func enqueueReturn(home, profile, reason string, now time.Time) (bool, error) {
	if err := os.MkdirAll(selfHealDir(home), 0o755); err != nil {
		return false, err
	}
	lock, err := lockSelfHealFile(selfHealDir(home, ".queue.lock"), true)
	if err != nil {
		return false, fmt.Errorf("lock return queue: %w", err)
	}
	defer unlockSelfHealFile(lock)
	existing, err := readReturnQueue(home)
	if err != nil {
		return false, err
	}
	for _, e := range existing {
		at, perr := time.Parse(time.RFC3339Nano, e.At)
		if e.Profile == profile && e.Reason == reason && perr == nil && now.Sub(at) < selfHealWindow {
			return false, nil
		}
	}
	line, err := json.Marshal(returnQueueEntry{At: now.Format(time.RFC3339Nano), Profile: profile, Reason: reason, NextAction: nextActionFor(profile, reason)})
	if err != nil {
		return false, err
	}
	file, err := os.OpenFile(returnQueuePath(home), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return false, err
	}
	return true, nil
}

func attemptsInWindow(attempts []healAttempt, now time.Time) int {
	n := 0
	for _, a := range attempts {
		at, err := time.Parse(time.RFC3339Nano, a.At)
		if err != nil || (now.Sub(at) < selfHealWindow && !at.After(now.Add(time.Minute))) {
			n++ // an unparseable stamp counts: fail toward fewer logins
		}
	}
	return n
}

func pruneAttempts(attempts []healAttempt, now time.Time) []healAttempt {
	out := []healAttempt{}
	for _, a := range attempts {
		if at, err := time.Parse(time.RFC3339Nano, a.At); err == nil && now.Sub(at) > healAttemptRetention {
			continue
		}
		out = append(out, a)
	}
	if len(out) > maxHealAttemptsKept {
		out = out[len(out)-maxHealAttemptsKept:]
	}
	return out
}

// --- credential provider ------------------------------------------------------

// credentialProviderReply is the ONE JSON object `compound browser
// credential-provider` (credential_provider.go) writes for a
// credential.resolve request; it exits 0 for every protocol answer.
type credentialProviderReply struct {
	Protocol   string              `json:"protocol"`
	Success    bool                `json:"success"`
	Error      string              `json:"error"`
	Credential *providedCredential `json:"credential"`
}

// credentialResolveRequestJSON is the agent-browser.plugin.v1
// credential.resolve envelope, the same request agent-browser sends the
// plugin: profileName is the registry id, itemRef its credential_ref (the
// provider refuses a mismatch; null when the row names none).
func credentialResolveRequestJSON(profile, credentialRef string) ([]byte, error) {
	req := credentialResolveRequest{ProfileName: profile}
	if ref := strings.TrimSpace(credentialRef); ref != "" {
		req.ItemRef = &ref
	}
	request, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginEnvelope{Protocol: agentBrowserPluginProtocol, Type: "credential.resolve", Capability: "credential.read", Request: request})
}

// parseCredentialProviderReply reads the provider's one stdout object.
// Errors carry the provider's refusal text (names only), never a value.
func parseCredentialProviderReply(body []byte) (providedCredential, error) {
	var reply credentialProviderReply
	if err := json.Unmarshal(bytes.TrimSpace(body), &reply); err != nil || reply.Protocol != agentBrowserPluginProtocol {
		return providedCredential{}, errors.New("credential_unavailable: the credential provider reply is not one agent-browser.plugin.v1 JSON object")
	}
	if !reply.Success || reply.Credential == nil {
		reason := truncateDetail(reply.Error)
		if reason == "" {
			reason = "the credential provider refused"
		}
		return providedCredential{}, fmt.Errorf("credential_unavailable: %s", reason)
	}
	return *reply.Credential, nil
}

// credentialReplyField maps a recipe {{secret:FIELD}} to the reply field that
// carries it: username, password, or the provider-generated otp for any
// OTP/TOTP-like name. "" means credential.resolve has no such field.
func credentialReplyField(field string) string {
	switch lower := strings.ToLower(strings.TrimSpace(field)); {
	case lower == "username" || lower == "password":
		return lower
	case strings.Contains(lower, "otp"):
		return "otp"
	}
	return ""
}

func (c providedCredential) field(name string) string {
	switch name {
	case "username":
		return c.Username
	case "password":
		return c.Password
	case "otp":
		return c.OTP
	}
	return ""
}

// healCredentials binds `compound browser credential-provider` (B3) for one
// profile. Every resolve is one subprocess that reads one credential.resolve
// envelope on stdin and writes one reply object on stdout (exit 0 for every
// protocol answer). prefetch makes the one call per heal and holds
// username/password; an otp is re-resolved at step time because codes expire.
type healCredentials struct {
	bin, home string
	request   []byte
	held      *providedCredential
}

// newCredentialProvider finds the provider on PATH. A compound on PATH
// without the verb is detected at the first call and reads as missing, not
// as a failed login.
func newCredentialProvider(homePath string, p selfHealProfile) (*healCredentials, error) {
	bin, err := exec.LookPath("compound")
	if err != nil {
		return nil, errors.New("credential_provider_missing: `compound browser credential-provider` is not on PATH")
	}
	request, err := credentialResolveRequestJSON(p.ID, p.CredentialRef)
	if err != nil {
		return nil, err
	}
	return &healCredentials{bin: bin, home: homePath, request: request}, nil
}

// fetch runs one credential.resolve round trip.
func (c *healCredentials) fetch(ctx context.Context) (providedCredential, error) {
	callCtx, cancel := context.WithTimeout(ctx, credentialProviderWait)
	defer cancel()
	cmd := exec.CommandContext(callCtx, c.bin, "browser", "credential-provider", "--home", c.home)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+c.home)
	cmd.Stdin = bytes.NewReader(c.request)
	stdout := &limitedBuffer{limit: 8192}
	stderr := &limitedBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "usage: compound browser") || strings.Contains(stderr.String(), "unknown browser") {
			return providedCredential{}, errors.New("credential_provider_missing: installed compound has no `browser credential-provider`")
		}
		return providedCredential{}, errors.New("credential_unavailable: the credential provider exited unsuccessfully")
	}
	return parseCredentialProviderReply(stdout.Bytes())
}

// prefetch resolves once before any attempt is recorded and proves every
// field the login recipe needs is present, so a provider gap never burns an
// attempt. A recipe that fills an otp the provider cannot produce needs the
// operator's second factor: two_factor_required, also before any attempt.
func (c *healCredentials) prefetch(ctx context.Context, fields []string) (secretResolver, error) {
	cred, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	for _, field := range fields {
		name := credentialReplyField(field)
		switch {
		case name == "":
			return nil, fmt.Errorf("credential_unavailable: field %s is not served by credential.resolve (username, password, otp)", field)
		case cred.field(name) != "":
		case name == "otp":
			return nil, fmt.Errorf("two_factor_required: the credential provider returned no otp for field %s", field)
		default:
			return nil, fmt.Errorf("credential_unavailable: field %s is empty", field)
		}
	}
	c.held = &cred
	return c.resolve, nil
}

// resolve is the step-time secretResolver.
func (c *healCredentials) resolve(ctx context.Context, field string) (string, error) {
	name := credentialReplyField(field)
	if name == "" {
		return "", fmt.Errorf("credential_unavailable: field %s is not served by credential.resolve", field)
	}
	if c.held == nil || name == "otp" {
		cred, err := c.fetch(ctx)
		if err != nil {
			return "", err
		}
		if name == "otp" {
			if cred.OTP == "" {
				return "", fmt.Errorf("two_factor_required: the credential provider returned no otp for field %s", field)
			}
			return cred.OTP, nil
		}
		c.held = &cred
	}
	if value := c.held.field(name); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("credential_unavailable: field %s is empty", field)
}

// --- heal ----------------------------------------------------------------------

type healResult struct {
	Schema           string       `json:"schema"`
	Profile          string       `json:"profile"`
	Status           string       `json:"status"`
	Reason           string       `json:"reason,omitempty"`
	LoggedIn         bool         `json:"logged_in"`
	AttemptsInWindow int          `json:"attempts_in_window"`
	RunID            string       `json:"run_id,omitempty"`
	Queued           bool         `json:"queued"`
	Probe            *probeResult `json:"probe,omitempty"`
	Error            string       `json:"error,omitempty"`
}

// runHeal is `compound browser heal`: 0 logged in (already or healed), 3
// needs the operator (refused, login failed, 2FA), 2 error.
func runHeal(ctx context.Context, homePath, id string) (healResult, int) {
	res := healResult{Schema: healResultSchema, Profile: id}
	fail := func(err error) (healResult, int) {
		res.Status, res.Reason, res.Error = "error", reasonOf(err), err.Error()
		return res, 2
	}
	needsOperator := func(status, reason string) (healResult, int) {
		res.Status, res.Reason = status, reason
		queued, err := enqueueReturn(homePath, id, reason, selfHealNow())
		if err != nil {
			return fail(err)
		}
		res.Queued = queued
		return res, 3
	}
	p, err := loadSelfHealProfile(homePath, id)
	if err != nil {
		return fail(err)
	}
	if msg := probeConfigError(p); msg != "" {
		if !profileEnrolled(p) && strings.HasPrefix(msg, "profile_not_enrolled") {
			return needsOperator("refused", "profile_not_enrolled")
		}
		return fail(errors.New(msg))
	}
	lock, err := lockProfile(homePath, id)
	if err != nil {
		return fail(err)
	}
	defer unlockSelfHealFile(lock)
	client, err := newAgentBrowserClient(homePath, p)
	if err != nil {
		return fail(err)
	}
	defer client.close()

	probed, err := probeOnce(ctx, homePath, client, p)
	if err != nil {
		return fail(err)
	}
	if err := recordProbe(homePath, probed); err != nil {
		return fail(err)
	}
	res.Probe = &probed
	now := selfHealNow()
	res.AttemptsInWindow = attemptsInWindow(p.HealAttempts, now)
	if probed.LoggedIn {
		res.Status, res.LoggedIn = "already_logged_in", true
		return res, 0
	}
	if strings.TrimSpace(p.LoginRecipe) == "" {
		return needsOperator("refused", "login_recipe_missing")
	}
	recipe, err := loadRecipe(homePath, id, p.LoginRecipe)
	if err != nil {
		if reasonOf(err) == "recipe_missing" {
			return needsOperator("refused", "login_recipe_missing")
		}
		return fail(err)
	}
	if params, _ := recipeRequirements(recipe); len(params) > 0 {
		return fail(fmt.Errorf("recipe_invalid: a login recipe takes no params (found %s)", strings.Join(params, ",")))
	}
	if res.AttemptsInWindow >= maxHealAttemptsWindow {
		return needsOperator("refused", "heal_attempts_exhausted")
	}
	_, secretFields := recipeRequirements(recipe)
	var secrets secretResolver
	if len(secretFields) > 0 {
		provider, err := newCredentialProvider(homePath, p)
		if err != nil {
			res.Error = err.Error()
			return needsOperator("refused", reasonOf(err))
		}
		if secrets, err = provider.prefetch(ctx, secretFields); err != nil {
			res.Error = err.Error()
			return needsOperator("refused", reasonOf(err))
		}
	}

	attempts := pruneAttempts(append(p.HealAttempts, healAttempt{At: now.Format(time.RFC3339Nano), Result: "started"}), now)
	if err := updateProfileRow(homePath, id, map[string]any{"heal_attempts": attempts}); err != nil {
		return fail(err)
	}
	res.AttemptsInWindow++
	result := "login_failed"
	finish := func(r string) error {
		attempts[len(attempts)-1].Result = r
		return updateProfileRow(homePath, id, map[string]any{"heal_attempts": attempts})
	}

	receipt, runErr := executeRecipe(ctx, homePath, client, recipe, nil, secrets, runPurposeHealLogin)
	res.RunID = receipt.ID
	if runErr != nil {
		_ = finish("error")
		return fail(runErr)
	}
	if !p.TwoFactorMarker.empty() {
		if twoFactor, err := evaluateMarker(ctx, client, p.TwoFactorMarker); err == nil && twoFactor {
			result = "two_factor_required"
		}
	}
	if result != "two_factor_required" {
		reprobe, err := probeOnce(ctx, homePath, client, p)
		if err != nil {
			_ = finish("error")
			return fail(err)
		}
		if err := recordProbe(homePath, reprobe); err != nil {
			return fail(err)
		}
		res.Probe = &reprobe
		if reprobe.LoggedIn {
			result = "logged_in"
		}
	}
	if err := finish(result); err != nil {
		return fail(err)
	}
	switch result {
	case "logged_in":
		res.Status, res.LoggedIn = "healed", true
		return res, 0
	case "two_factor_required":
		return needsOperator("two_factor_required", "two_factor_required")
	}
	return needsOperator("login_failed", "login_failed")
}

// --- doctor ----------------------------------------------------------------------

type selfHealDoctorRow struct {
	Profile              string     `json:"profile"`
	Enrollment           string     `json:"enrollment"`
	LoginCheckConfigured bool       `json:"login_check_configured"`
	LoginRecipe          string     `json:"login_recipe,omitempty"`
	LastProbe            *lastProbe `json:"last_probe,omitempty"`
	HealAttemptsInWindow int        `json:"heal_attempts_in_window"`
	Recipes              int        `json:"recipes"`
	Runs7d               int        `json:"runs_7d"`
	FailedRuns7d         int        `json:"failed_runs_7d"`
	HealedSteps7d        int        `json:"healed_steps_7d"`
	QueueEntries         int        `json:"queue_entries"`
}

type selfHealDoctorReport struct {
	Schema        string              `json:"schema"`
	GeneratedAt   string              `json:"generated_at"`
	RegistryState string              `json:"registry_state"`
	QueueEntries  int                 `json:"queue_entries"`
	Profiles      []selfHealDoctorRow `json:"profiles"`
	Error         string              `json:"error,omitempty"`
}

// buildSelfHealDoctor projects counts and stamps only: no recipe values,
// params, credentials or page text ever reach the report.
func buildSelfHealDoctor(homePath string, now time.Time) (selfHealDoctorReport, error) {
	report := selfHealDoctorReport{Schema: selfHealDoctorSchema, GeneratedAt: now.Format(time.RFC3339Nano), Profiles: []selfHealDoctorRow{}}
	profiles, err := loadSelfHealProfiles(homePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		report.RegistryState = "absent"
	case err != nil:
		report.RegistryState = "error"
		return report, err
	default:
		report.RegistryState = "populated"
	}
	queue, err := readReturnQueue(homePath)
	if err != nil {
		return report, err
	}
	report.QueueEntries = len(queue)
	recipes, err := listRecipes(homePath, "")
	if err != nil {
		return report, err
	}
	receipts := recentRunReceipts(homePath, now.Add(-7*24*time.Hour))
	for _, p := range profiles {
		row := selfHealDoctorRow{
			Profile:              p.ID,
			Enrollment:           enrollmentLabel(p),
			LoginCheckConfigured: strings.TrimSpace(p.LoginCheckURL) != "" && !p.LoggedInMarker.empty(),
			LoginRecipe:          p.LoginRecipe,
			LastProbe:            p.LastProbe,
			HealAttemptsInWindow: attemptsInWindow(p.HealAttempts, now),
		}
		for _, r := range recipes {
			if r.Profile == p.ID {
				row.Recipes++
			}
		}
		for _, r := range receipts {
			if r.Profile != p.ID {
				continue
			}
			row.Runs7d++
			row.HealedSteps7d += r.HealedSteps
			if r.Status != "ok" {
				row.FailedRuns7d++
			}
		}
		for _, e := range queue {
			if e.Profile == p.ID {
				row.QueueEntries++
			}
		}
		report.Profiles = append(report.Profiles, row)
	}
	return report, nil
}

func recentRunReceipts(homePath string, since time.Time) []runReceipt {
	paths, _ := filepath.Glob(selfHealDir(homePath, "runs", "brun_*.json"))
	sort.Strings(paths)
	var out []runReceipt
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r runReceipt
		if json.Unmarshal(body, &r) != nil || r.Schema != runReceiptSchema {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, r.StartedAt); err != nil || at.Before(since) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func renderSelfHealDoctorText(w io.Writer, r selfHealDoctorReport) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# browser self-heal (%s) registry=%s profiles=%d return_queue=%d\n", r.Schema, r.RegistryState, len(r.Profiles), r.QueueEntries)
	if r.Error != "" {
		fmt.Fprintf(&b, "- error: %s\n", truncateDetail(r.Error))
	}
	for _, p := range r.Profiles {
		probe := "never"
		if p.LastProbe != nil {
			probe = fmt.Sprintf("%s logged_in=%t", p.LastProbe.At, p.LastProbe.LoggedIn)
		}
		recipe := p.LoginRecipe
		if recipe == "" {
			recipe = "-"
		}
		fmt.Fprintf(&b, "- %s enrollment=%s check=%t login_recipe=%s last_probe=%s heal_attempts_6h=%d/%d recipes=%d runs_7d=%d failed_7d=%d healed_7d=%d queue=%d\n",
			p.Profile, p.Enrollment, p.LoginCheckConfigured, recipe, probe, p.HealAttemptsInWindow, maxHealAttemptsWindow,
			p.Recipes, p.Runs7d, p.FailedRuns7d, p.HealedSteps7d, p.QueueEntries)
	}
	_, _ = w.Write(b.Bytes())
}

// selfHealDoctorSection is the additive self_heal block of `compound browser
// doctor`; a broken registry is reported in the block, never fatal.
func selfHealDoctorSection() *selfHealDoctorReport {
	homePath, err := home.Resolve("")
	if err != nil {
		return &selfHealDoctorReport{Schema: selfHealDoctorSchema, Profiles: []selfHealDoctorRow{}, Error: err.Error()}
	}
	report, err := buildSelfHealDoctor(homePath, selfHealNow())
	if err != nil {
		report.Error = err.Error()
	}
	return &report
}
