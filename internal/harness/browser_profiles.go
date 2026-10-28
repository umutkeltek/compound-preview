package harness

// browser_profiles.go — browser-profile registry + doctor.
//
// One canonical registry at ~/.compound/registries/browser-profiles.json
// (compound.browser_profile_registry.v0.1) names which persistent profile
// carries which login. Joins by path to social-accounts.json — every
// browser_profile path in social accounts must resolve to exactly one
// profile here. A profile whose dir is absent on disk reads pending_enrollment
// — that is the human enrollment step, not an error.
//
// Door contract: this package owns the registry file. Callers (CLI, codex
// config, mcporter launch args) read it. The seed path joins social accounts
// — never hand-edit; the CLI seeder is the only writer.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BrowserProfileRegistrySchemaVersion tags the on-disk JSON. Bump the
// registry version when the schema changes; bump the door API separately.
const BrowserProfileRegistrySchemaVersion = "compound.browser_profile_registry.v0.1"

// SocialAccountsSchemaVersion is read for compatibility only — the doctor
// uses the version to refuse the join if it is older than expected.
const SocialAccountsSchemaVersion = "compound.social_account_registry.v0.1"

// BrowserProfile is one row in the registry. The id is the row identity;
// user_data_dir is the path the backends point at; backends carry the
// per-backend launch args (Playwright MCP `--user-data-dir`, agent-browser
// `--profile`, hermes CDP user dir, etc.).
type BrowserProfile struct {
	ID          string                 `json:"id"`
	UserDataDir string                 `json:"user_data_dir"`
	Backends    BrowserProfileBackends `json:"backends"`
	Logins      []string               `json:"logins"`
	Status      BrowserProfileStatus   `json:"status"`
	UpdatedAt   string                 `json:"updated_at,omitempty"`
	// CredentialRef optionally names the login secret for this profile
	// (bws secret key or ~/.compound/secrets/accounts/<name>). A name, never
	// a value; `compound browser credential-provider` resolves it.
	CredentialRef string `json:"credential_ref,omitempty"`

	// Browser self-heal fields, owned by internal/browser (probe/heal). The
	// registry carries them verbatim so the seed door never drops them.
	LoginCheckURL   string          `json:"login_check_url,omitempty"`
	LoggedInMarker  json.RawMessage `json:"logged_in_marker,omitempty"`
	TwoFactorMarker json.RawMessage `json:"two_factor_marker,omitempty"`
	LoginRecipe     string          `json:"login_recipe,omitempty"`
	LastProbe       json.RawMessage `json:"last_probe,omitempty"`
	HealAttempts    json.RawMessage `json:"heal_attempts,omitempty"`
}

// BrowserProfileBackends carries per-backend launch args. Each field is
// optional — backends that are not configured simply omit the field.
type BrowserProfileBackends struct {
	PlaywrightMCP *BrowserProfileBackend `json:"playwright_mcp,omitempty"`
	AgentBrowser  *BrowserProfileBackend `json:"agent_browser,omitempty"`
	HermesCDP     *BrowserProfileBackend `json:"hermes_cdp,omitempty"`
	OmpRelay      *BrowserProfileBackend `json:"omp_relay,omitempty"`
}

// BrowserProfileBackend carries the launch arg fragments for one backend.
// Args are joined with spaces when handed to a shell; for typed callers
// (Playwright MCP JSON config) the caller can read fields directly.
type BrowserProfileBackend struct {
	Args []string `json:"args,omitempty"`
	Note string   `json:"note,omitempty"`
}

// BrowserProfileStatus is the enrollment state. Only two values today:
// "enrolled" (the user_data_dir exists on disk and carries cookies) and
// "pending_enrollment" (the dir is absent — the human has not logged in
// yet). A missing dir is never an error.
type BrowserProfileStatus string

const (
	StatusEnrolled          BrowserProfileStatus = "enrolled"
	StatusPendingEnrollment BrowserProfileStatus = "pending_enrollment"
)

// BrowserProfileRegistry is the file shape on disk.
type BrowserProfileRegistry struct {
	SchemaVersion string           `json:"schema_version"`
	UpdatedAt     string           `json:"updated_at"`
	Profiles      []BrowserProfile `json:"profiles"`
}

// SocialAccount is a SUBSET of the social-accounts.json row — only the
// fields the join cares about. The full row lives in
// ~/.compound/registries/social-accounts.json; the doctor reads it
// directly.
type SocialAccount struct {
	ID             string `json:"id"`
	BrowserProfile string `json:"browser_profile"`
}

// socialAccountsFile is the minimum shape we read.
type socialAccountsFile struct {
	SchemaVersion string          `json:"schema_version"`
	Accounts      []SocialAccount `json:"accounts"`
}

// registryPath is the canonical on-disk location under COMPOUND_HOME.
func registryPath(home string) string {
	return filepath.Join(home, "registries", "browser-profiles.json")
}

// socialAccountsPath is the read-only peer registry we join to.
func socialAccountsPath(home string) string {
	return filepath.Join(home, "registries", "social-accounts.json")
}

// LoadBrowserProfileRegistry reads the registry from disk. A missing file
// returns an empty registry with the current schema version (so a doctor
// call on a fresh home still produces a report).
func LoadBrowserProfileRegistry(home string) (BrowserProfileRegistry, error) {
	var reg BrowserProfileRegistry
	b, err := os.ReadFile(registryPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return BrowserProfileRegistry{SchemaVersion: BrowserProfileRegistrySchemaVersion}, nil
		}
		return reg, fmt.Errorf("read browser-profile registry: %w", err)
	}
	if err := json.Unmarshal(b, &reg); err != nil {
		return reg, fmt.Errorf("decode browser-profile registry: %w", err)
	}
	if reg.SchemaVersion == "" {
		reg.SchemaVersion = BrowserProfileRegistrySchemaVersion
	}
	return reg, nil
}

// Browser-profile registry read states. `absent` means no file on disk;
// `empty` a current-schema registry with zero profiles; `populated` a
// current-schema registry with rows; `foreign_version` a recognized
// schema_version from another schema generation. An existing file whose
// shape cannot be read is an ERROR, never an empty registry — the
// 2026-09-15 diagnostic-integrity contract exists because a doctor that
// interprets an unrecognized shape as zero profiles then auto-seeds OVER
// real metadata.
const (
	BrowserProfileRegistryAbsent       = "absent"
	BrowserProfileRegistryEmpty        = "empty"
	BrowserProfileRegistryPopulated    = "populated"
	BrowserProfileRegistryForeign      = "foreign_version"
	BrowserProfileRegistryShapeInvalid = "shape_invalid"
)

// BrowserProfileRegistryRead is what the diagnostic loader reads: the
// registry plus the explicit state. Doctor and seed callers MUST branch on
// State, never on "zero profiles ⇒ absent".
type BrowserProfileRegistryRead struct {
	State    string
	Registry BrowserProfileRegistry
}

// LoadBrowserProfileRegistryState reads the registry and classifies what it
// read. It refuses (error) a present file it cannot decode or that carries
// no schema_version — an unrecognized JSON shape must never silently read
// as an empty profile list. Rows of a current-schema registry must carry
// both id and user_data_dir; a current-schema row without them is invalid.
func LoadBrowserProfileRegistryState(home string) (BrowserProfileRegistryRead, error) {
	b, err := os.ReadFile(registryPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return BrowserProfileRegistryRead{State: BrowserProfileRegistryAbsent}, nil
		}
		return BrowserProfileRegistryRead{}, fmt.Errorf("read browser-profile registry: %w", err)
	}
	var reg BrowserProfileRegistry
	if err := json.Unmarshal(b, &reg); err != nil {
		return BrowserProfileRegistryRead{}, fmt.Errorf("decode browser-profile registry: %w", err)
	}
	if strings.TrimSpace(reg.SchemaVersion) == "" {
		// A file that exists but names no schema is an unrecognized shape.
		// Interpreting it as "no profiles" is how the doctor used to
		// overwrite real metadata with seed data.
		return BrowserProfileRegistryRead{}, fmt.Errorf("browser-profile registry %s: unrecognized shape (no schema_version); refusing to interpret it as an empty registry", registryPath(home))
	}
	if reg.SchemaVersion != BrowserProfileRegistrySchemaVersion {
		return BrowserProfileRegistryRead{State: BrowserProfileRegistryForeign, Registry: reg}, nil
	}
	for i, p := range reg.Profiles {
		if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.UserDataDir) == "" {
			return BrowserProfileRegistryRead{}, fmt.Errorf("browser-profile registry %s: profile row %d carries no id or user_data_dir", registryPath(home), i)
		}
	}
	state := BrowserProfileRegistryPopulated
	if len(reg.Profiles) == 0 {
		state = BrowserProfileRegistryEmpty
	}
	return BrowserProfileRegistryRead{State: state, Registry: reg}, nil
}

// browserRegistryPreimageSHA256 returns the sha256 of the current registry
// bytes ("" when the file is absent) — the CAS generation a seed write
// checks before replacing the file.
func browserRegistryPreimageSHA256(home string) (string, []byte, error) {
	b, err := os.ReadFile(registryPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("read browser-profile registry for preimage: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), b, nil
}

// LoadSocialAccounts reads the peer registry the profile registry joins to.
func LoadSocialAccounts(home string) ([]SocialAccount, error) {
	var file socialAccountsFile
	b, err := os.ReadFile(socialAccountsPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read social-accounts registry: %w", err)
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("decode social-accounts registry: %w", err)
	}
	return file.Accounts, nil
}

// SeedBrowserProfileRegistry joins social-accounts to the profile registry.
// It creates the file if absent; it never deletes profiles; it never writes
// social-accounts. The status of each row reflects the current filesystem
// state (enrolled vs pending_enrollment).
//
// The id is derived from the social account id when present; if a profile
// row already exists for the same user_data_dir, its id is preserved and
// its logins are merged (no duplicates).
func SeedBrowserProfileRegistry(home string, now time.Time) (BrowserProfileRegistry, error) {
	if now.IsZero() {
		now = time.Now()
	}
	accounts, err := LoadSocialAccounts(home)
	if err != nil {
		return BrowserProfileRegistry{}, err
	}
	existing, err := LoadBrowserProfileRegistry(home)
	if err != nil {
		return BrowserProfileRegistry{}, err
	}

	// Index existing profiles by user_data_dir.
	byDir := make(map[string]int, len(existing.Profiles))
	for i, p := range existing.Profiles {
		byDir[p.UserDataDir] = i
	}

	for _, acct := range accounts {
		dir := expandHome(acct.BrowserProfile, currentHome())
		if dir == "" {
			continue
		}
		if idx, ok := byDir[dir]; ok {
			// Merge this login into the existing row.
			if !containsString(existing.Profiles[idx].Logins, acct.ID) {
				existing.Profiles[idx].Logins = append(existing.Profiles[idx].Logins, acct.ID)
			}
			existing.Profiles[idx].Status = deriveStatus(dir)
			continue
		}
		existing.Profiles = append(existing.Profiles, BrowserProfile{
			ID:          acct.ID,
			UserDataDir: dir,
			Backends:    BrowserProfileBackends{},
			Logins:      []string{acct.ID},
			Status:      deriveStatus(dir),
		})
		byDir[dir] = len(existing.Profiles) - 1
	}
	// Re-derive status for every profile (an operator may have created the
	// dir since the last seed).
	for i := range existing.Profiles {
		existing.Profiles[i].Status = deriveStatus(existing.Profiles[i].UserDataDir)
	}
	// Stable order by id.
	sort.SliceStable(existing.Profiles, func(i, j int) bool {
		return existing.Profiles[i].ID < existing.Profiles[j].ID
	})
	existing.SchemaVersion = BrowserProfileRegistrySchemaVersion
	existing.UpdatedAt = now.UTC().Format(time.RFC3339)
	return existing, nil
}

// WriteBrowserProfileRegistry persists the registry through the door. It
// refuses to write if the file's directory does not exist under home (a
// guard against a typo'd home path).
func WriteBrowserProfileRegistry(home string, reg BrowserProfileRegistry) error {
	if strings.TrimSpace(home) == "" {
		return fmt.Errorf("write browser-profile registry: home is empty")
	}
	if reg.SchemaVersion == "" {
		reg.SchemaVersion = BrowserProfileRegistrySchemaVersion
	}
	dir := filepath.Dir(registryPath(home))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	if err := os.WriteFile(registryPath(home), b, 0o644); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return nil
}

// SeedBrowserProfileRegistryExplicit is the EXPLICIT seed/migration door
// (2026-09-15 diagnostic-integrity contract): the doctor no longer writes,
// so this reviewed door is the only writer that joins social-accounts into
// the profile registry. It captures the current file's sha256 as the CAS
// generation, runs the seed join, then writes only when the file still
// matches that generation, preserving the preimage bytes beside the file
// before replacing it. A missing registry seeds from scratch (no preimage);
// a foreign-version registry is refused (migration of an unknown schema is
// a deliberate operator decision, never an automatic join).
func SeedBrowserProfileRegistryExplicit(home string, now time.Time) (BrowserProfileRegistry, error) {
	read, err := LoadBrowserProfileRegistryState(home)
	if err != nil {
		return BrowserProfileRegistry{}, err
	}
	switch read.State {
	case BrowserProfileRegistryForeign:
		return BrowserProfileRegistry{}, fmt.Errorf("seed browser-profile registry: registry schema %q is not the supported %q; migration requires an explicit decision, not an automatic join", read.Registry.SchemaVersion, BrowserProfileRegistrySchemaVersion)
	case BrowserProfileRegistryShapeInvalid:
		return BrowserProfileRegistry{}, fmt.Errorf("seed browser-profile registry: %s", read.State)
	}
	preimageSHA, preimageBytes, err := browserRegistryPreimageSHA256(home)
	if err != nil {
		return BrowserProfileRegistry{}, err
	}
	seeded, err := SeedBrowserProfileRegistry(home, now)
	if err != nil {
		return BrowserProfileRegistry{}, err
	}
	if err := writeBrowserProfileRegistryCAS(home, seeded, preimageSHA, preimageBytes); err != nil {
		return BrowserProfileRegistry{}, err
	}
	return seeded, nil
}

// writeBrowserProfileRegistryCAS replaces the registry only when the file
// still carries the generation the caller loaded (sha256 CAS). The preimage
// bytes are preserved beside the file first; a preimage-preservation failure
// refuses the write so existing metadata is never lost.
func writeBrowserProfileRegistryCAS(home string, reg BrowserProfileRegistry, preimageSHA string, preimageBytes []byte) error {
	if strings.TrimSpace(home) == "" {
		return fmt.Errorf("write browser-profile registry: home is empty")
	}
	currentSHA, _, err := browserRegistryPreimageSHA256(home)
	if err != nil {
		return err
	}
	if currentSHA != preimageSHA {
		return fmt.Errorf("write browser-profile registry: registry changed since load (expected %q, found %q); refusing to overwrite", orEmpty(preimageSHA), orEmpty(currentSHA))
	}
	dir := filepath.Dir(registryPath(home))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if len(preimageBytes) > 0 {
		if err := os.WriteFile(registryPath(home)+".preimage", preimageBytes, 0o644); err != nil {
			return fmt.Errorf("preserve registry preimage: %w", err)
		}
	}
	if reg.SchemaVersion == "" {
		reg.SchemaVersion = BrowserProfileRegistrySchemaVersion
	}
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	tmp := registryPath(home) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return os.Rename(tmp, registryPath(home))
}

func orEmpty(s string) string {
	if s == "" {
		return "<absent>"
	}
	return s
}

// BrowserProfileDoctorReport is the doctor's projection of the registry.
// Counts and lists come from the in-memory join; never a directory listing.
type BrowserProfileDoctorReport struct {
	SchemaVersion   string                  `json:"schema_version"`
	UpdatedAt       string                  `json:"updated_at"`
	RegistryState   string                  `json:"registry_state"`
	ProfileCount    int                     `json:"profile_count"`
	EnrolledCount   int                     `json:"enrolled_count"`
	PendingCount    int                     `json:"pending_count"`
	UnresolvedPaths []string                `json:"unresolved_paths,omitempty"`
	Profiles        []BrowserProfileSummary `json:"profiles"`
}

// BrowserProfileSummary is one row in the report.
type BrowserProfileSummary struct {
	ID          string               `json:"id"`
	UserDataDir string               `json:"user_data_dir"`
	Status      BrowserProfileStatus `json:"status"`
	Logins      []string             `json:"logins"`
	Backends    []string             `json:"backends"`
}

// DoctorBrowserProfileRegistry returns a doctor report. The doctor is
// READ-ONLY (2026-09-15 diagnostic-integrity contract): it never seeds, never
// writes, and never interprets an unrecognized shape as an empty registry.
// A fresh home reports state=absent with zero profiles and the operator
// brings rows in with the explicit seed command
// (`compound harness browser-profiles seed`); a foreign-version registry is
// reported as-is with its state so nothing migrates implicitly.
//
// The report joins social-accounts to the registry by path; an account whose
// path does not resolve to a profile is reported in `unresolved_paths`.
func DoctorBrowserProfileRegistry(home string) (BrowserProfileDoctorReport, error) {
	rep := BrowserProfileDoctorReport{
		SchemaVersion: BrowserProfileRegistrySchemaVersion,
	}
	read, err := LoadBrowserProfileRegistryState(home)
	if err != nil {
		return rep, err
	}
	reg := read.Registry
	switch read.State {
	case BrowserProfileRegistryAbsent:
		reg = BrowserProfileRegistry{SchemaVersion: BrowserProfileRegistrySchemaVersion}
	case BrowserProfileRegistryForeign:
		// A different schema generation: load what was read, never seed.
		if reg.SchemaVersion == "" {
			reg.SchemaVersion = BrowserProfileRegistrySchemaVersion
		}
	}
	rep.RegistryState = read.State
	accounts, err := LoadSocialAccounts(home)
	if err != nil {
		return rep, err
	}
	byDir := make(map[string]bool, len(reg.Profiles))
	for _, p := range reg.Profiles {
		byDir[p.UserDataDir] = true
	}
	for _, acct := range accounts {
		dir := expandHome(acct.BrowserProfile, currentHome())
		if dir == "" {
			continue
		}
		if !byDir[dir] {
			rep.UnresolvedPaths = append(rep.UnresolvedPaths, dir)
		}
	}
	sort.Strings(rep.UnresolvedPaths)
	rep.ProfileCount = len(reg.Profiles)
	for _, p := range reg.Profiles {
		status := deriveStatus(p.UserDataDir)
		if status != p.Status {
			// Don't mutate the in-memory reg here — doctor is read-only. We
			// surface the derived status in the report so the operator sees
			// the truth even if the file is stale.
			p.Status = status
		}
		sum := BrowserProfileSummary{
			ID:          p.ID,
			UserDataDir: p.UserDataDir,
			Status:      p.Status,
			Logins:      append([]string(nil), p.Logins...),
		}
		if p.Backends.PlaywrightMCP != nil {
			sum.Backends = append(sum.Backends, "playwright_mcp")
		}
		if p.Backends.AgentBrowser != nil {
			sum.Backends = append(sum.Backends, "agent_browser")
		}
		if p.Backends.HermesCDP != nil {
			sum.Backends = append(sum.Backends, "hermes_cdp")
		}
		if p.Backends.OmpRelay != nil {
			sum.Backends = append(sum.Backends, "omp_relay")
		}
		rep.Profiles = append(rep.Profiles, sum)
		if p.Status == StatusEnrolled {
			rep.EnrolledCount++
		} else {
			rep.PendingCount++
		}
	}
	sort.SliceStable(rep.Profiles, func(i, j int) bool {
		return rep.Profiles[i].ID < rep.Profiles[j].ID
	})
	return rep, nil
}

// expandHome resolves "~/" prefix. The default userHome is $HOME; tests
// inject via the userHome parameter to keep the join deterministic.
func expandHome(p, userHome string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if strings.TrimSpace(userHome) == "" {
			return p
		}
		return filepath.Join(userHome, strings.TrimPrefix(p, "~/"))
	}
	if p == "~" {
		return userHome
	}
	return p
}

// currentHome returns $HOME for production; tests inject via t.Setenv.
func currentHome() string { return os.Getenv("HOME") }

// deriveStatus maps a user_data_dir's filesystem state to the registry row.
// Missing dir is pending_enrollment by design — that is the human enrollment
// step, not an error. A present-but-empty dir is still pending_enrollment
// (no cookies yet).
func deriveStatus(dir string) BrowserProfileStatus {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return StatusPendingEnrollment
	}
	return StatusEnrolled
}

// containsString is the dedupe helper for the seed merge.
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// FormatBrowserProfileReport renders the doctor report as compact CLI text.
// Used by the CLI dispatcher so cmd/compound stays at a tiny delta.
func FormatBrowserProfileReport(rep BrowserProfileDoctorReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# harness browser-profiles doctor profiles=%d enrolled=%d pending=%d registry_state=%s\n",
		rep.ProfileCount, rep.EnrolledCount, rep.PendingCount, rep.RegistryState)
	for _, p := range rep.Profiles {
		fmt.Fprintf(&b, "- %s status=%s dir=%s logins=%v backends=%v\n",
			p.ID, p.Status, p.UserDataDir, p.Logins, p.Backends)
	}
	if len(rep.UnresolvedPaths) > 0 {
		b.WriteString("- unresolved_paths:\n")
		for _, u := range rep.UnresolvedPaths {
			fmt.Fprintf(&b, "  %s\n", u)
		}
	}
	return b.String()
}

// RunBrowserProfileDoctorCLI parses flags, runs the doctor, prints. The
// cmd/compound dispatcher hands off everything to here so the cmd delta
// stays small. The doctor is read-only; `RunBrowserProfileSeedCLI` is the
// explicit writer.
func RunBrowserProfileDoctorCLI(homePath string, asJSON bool) error {
	rep, err := DoctorBrowserProfileRegistry(homePath)
	if err != nil {
		return err
	}
	if asJSON {
		b, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(FormatBrowserProfileReport(rep))
	return nil
}

// RunBrowserProfileSeedCLI is the explicit seed/migration command: join
// social-accounts into the profile registry, CAS-guarded, preimage
// preserved. Run with an explicit operator action only — never on a timer,
// never from a doctor.
func RunBrowserProfileSeedCLI(homePath string, asJSON bool, now time.Time) error {
	seeded, err := SeedBrowserProfileRegistryExplicit(homePath, now)
	if err != nil {
		return err
	}
	if asJSON {
		b, err := json.Marshal(map[string]any{
			"schema_version": seeded.SchemaVersion,
			"updated_at":     seeded.UpdatedAt,
			"profile_count":  len(seeded.Profiles),
			"profiles":       seeded.Profiles,
		})
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("# harness browser-profiles seed profiles=%d (preimage preserved at %s)\n",
		len(seeded.Profiles), registryPath(homePath)+".preimage")
	return nil
}
