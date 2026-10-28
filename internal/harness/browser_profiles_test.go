package harness

// browser_profiles_test.go — falsifier F4.
//
// F4: the browser-profile registry joins social-accounts.json by path.
// Seed through the door → 3 profiles, all pending_enrollment; create one
// dir → that one enrolled; an account path with no profile → doctor names
// it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureSocialAccounts writes a synthetic social-accounts.json with the
// given rows. Returns the home so callers can pass it on.
func fixtureSocialAccounts(t *testing.T, home string, accounts []map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "registries"), 0o755); err != nil {
		t.Fatalf("mkdir registries: %v", err)
	}
	body := map[string]any{
		"schema_version": SocialAccountsSchemaVersion,
		"accounts":       accounts,
	}
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "registries", "social-accounts.json"), b, 0o644); err != nil {
		t.Fatalf("write social-accounts: %v", err)
	}
}

func fixedProfileNow() time.Time {
	return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
}

func TestBrowserOwnerHomeUsesCanonicalResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("COMPOUND_HOME", "")
	got, err := browserOwnerHome("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".compound"); got != want {
		t.Fatalf("default home = %q, want %q", got, want)
	}
	custom := filepath.Join(root, "selected")
	got, err = browserOwnerHome(custom)
	if err != nil || got != custom {
		t.Fatalf("explicit home = %q, %v; want %q", got, err, custom)
	}
}

// F4: seed joins social-accounts; doctor counts; creating one dir flips
// that one to enrolled; an account whose path has no profile is named.
func TestBrowserProfileRegistryJoinsSocialAccounts(t *testing.T) {
	home := t.TempDir()
	// expandHome reads $HOME; redirect it to the test temp dir so
	// "~/social-profiles/profile-a" expands under the test fixture.
	t.Setenv("HOME", home)
	now := fixedProfileNow()

	// Three accounts, all pointing at profile dirs that do NOT exist yet.
	// The seeder will create 3 profiles, all pending_enrollment.
	profilesDir := filepath.Join(home, "social-profiles")
	accounts := []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
		{"id": "acct-0002", "browser_profile": "~/social-profiles/profile-b"},
		{"id": "acct-0003", "browser_profile": "~/social-profiles/profile-c"},
	}
	fixtureSocialAccounts(t, home, accounts)

	// Seed — should create 3 profiles, all pending.
	reg, err := SeedBrowserProfileRegistry(home, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := len(reg.Profiles); got != 3 {
		t.Fatalf("F4: seed wants 3 profiles, got %d", got)
	}
	for _, p := range reg.Profiles {
		if p.Status != StatusPendingEnrollment {
			t.Errorf("F4: profile %s status=%s want pending_enrollment (no dir on disk)", p.ID, p.Status)
		}
		if len(p.Logins) != 1 {
			t.Errorf("F4: profile %s logins=%d want 1", p.ID, len(p.Logins))
		}
	}

	// Persist through the door — write then re-read.
	if err := WriteBrowserProfileRegistry(home, reg); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	got, err := LoadBrowserProfileRegistry(home)
	if err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if len(got.Profiles) != 3 {
		t.Fatalf("F4: reloaded registry wants 3 profiles, got %d", len(got.Profiles))
	}

	// Create the dir for profile-a only — doctor should flip that one.
	if err := os.MkdirAll(filepath.Join(profilesDir, "profile-a"), 0o755); err != nil {
		t.Fatalf("mkdir profile-a: %v", err)
	}
	rep, err := DoctorBrowserProfileRegistry(home)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if rep.ProfileCount != 3 {
		t.Errorf("F4 doctor: profile_count=%d want 3", rep.ProfileCount)
	}
	if rep.EnrolledCount != 1 {
		t.Errorf("F4 doctor: enrolled_count=%d want 1", rep.EnrolledCount)
	}
	if rep.PendingCount != 2 {
		t.Errorf("F4 doctor: pending_count=%d want 2", rep.PendingCount)
	}
	// Find profile-a's row; assert enrolled.
	var sawA, sawB, sawC bool
	for _, p := range rep.Profiles {
		switch p.ID {
		case "acct-0001":
			sawA = true
			if p.Status != StatusEnrolled {
				t.Errorf("F4: profile-a status=%s want enrolled", p.Status)
			}
		case "acct-0002":
			sawB = true
			if p.Status != StatusPendingEnrollment {
				t.Errorf("F4: profile-b status=%s want pending", p.Status)
			}
		case "acct-0003":
			sawC = true
			if p.Status != StatusPendingEnrollment {
				t.Errorf("F4: profile-c status=%s want pending", p.Status)
			}
		}
	}
	if !sawA || !sawB || !sawC {
		t.Errorf("F4 doctor: missing expected profile rows (a=%v b=%v c=%v)", sawA, sawB, sawC)
	}
}

// F4 extension: an account path that does NOT resolve to a profile row is
// reported in unresolved_paths. We exercise this by deleting one profile
// row AFTER the seed — the next doctor run should name that path.
func TestBrowserProfileRegistryUnresolvedPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := fixedProfileNow()

	accounts := []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
		{"id": "acct-0002", "browser_profile": "~/social-profiles/profile-b"},
	}
	fixtureSocialAccounts(t, home, accounts)

	reg, err := SeedBrowserProfileRegistry(home, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteBrowserProfileRegistry(home, reg); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Hand-edit: drop profile-a so its path becomes unresolved.
	loaded, _ := LoadBrowserProfileRegistry(home)
	for i, p := range loaded.Profiles {
		if p.ID == "acct-0001" {
			loaded.Profiles = append(loaded.Profiles[:i], loaded.Profiles[i+1:]...)
		}
	}
	if err := WriteBrowserProfileRegistry(home, loaded); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	rep, err := DoctorBrowserProfileRegistry(home)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(rep.UnresolvedPaths) != 1 {
		t.Fatalf("F4 unresolved: got %d paths, want 1", len(rep.UnresolvedPaths))
	}
	if !strings.HasSuffix(rep.UnresolvedPaths[0], "profile-a") {
		t.Errorf("F4 unresolved: path %q does not name profile-a", rep.UnresolvedPaths[0])
	}
}

// F4 edge: missing social-accounts.json → empty join, registry created
// with zero profiles. This is the fresh-home state.
func TestBrowserProfileRegistryFreshHome(t *testing.T) {
	home := t.TempDir()
	now := fixedProfileNow()

	reg, err := SeedBrowserProfileRegistry(home, now)
	if err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	if len(reg.Profiles) != 0 {
		t.Errorf("F4 fresh home: profiles=%d want 0", len(reg.Profiles))
	}
	if reg.SchemaVersion != BrowserProfileRegistrySchemaVersion {
		t.Errorf("F4 fresh home: schema_version=%q want %q", reg.SchemaVersion, BrowserProfileRegistrySchemaVersion)
	}
	// Write should be a no-op for an empty registry — no rows to add.
	if err := WriteBrowserProfileRegistry(home, reg); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	rep, err := DoctorBrowserProfileRegistry(home)
	if err != nil {
		t.Fatalf("doctor fresh: %v", err)
	}
	if rep.ProfileCount != 0 {
		t.Errorf("F4 fresh home: profile_count=%d want 0", rep.ProfileCount)
	}
	if len(rep.UnresolvedPaths) != 0 {
		t.Errorf("F4 fresh home: unresolved=%d want 0", len(rep.UnresolvedPaths))
	}
}

// Diagnostic-integrity contract 2026-09-15. The external audit reproduced
// this exact defect: an existing registry with an unrecognized shape (no
// schema_version, unfamiliar profile fields) read as ZERO profiles, and the
// doctor's auto-seed then OVERWROTE the file with social-account seeds.
// The doctor must now refuse the shape, never write, and leave the bytes
// byte-identical.
func TestBrowserDoctorRefusesUnknownShapeAndNeverOverwrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fixtureSocialAccounts(t, home, []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
	})
	foreign := []byte("{\n  \"kind\": \"browser_profiles\",\n  \"items\": [\n    {\"name\": \"a\", \"dir\": \"/tmp/a\"}\n  ]\n}\n")
	registry := filepath.Join(home, "registries", "browser-profiles.json")
	if err := os.WriteFile(registry, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := DoctorBrowserProfileRegistry(home)
	if err == nil || !strings.Contains(err.Error(), "unrecognized shape") {
		t.Fatalf("doctor must refuse an unrecognized registry shape, got err=%v", err)
	}
	after, readErr := os.ReadFile(registry)
	if readErr != nil || string(after) != string(foreign) {
		t.Fatalf("a failed doctor must leave the registry byte-identical, err=%v", readErr)
	}

	// The explicit seed must refuse it too (migration of an unknown shape is
	// a deliberate operator decision).
	if _, err := SeedBrowserProfileRegistryExplicit(home, fixedProfileNow()); err == nil {
		t.Fatal("seed must refuse an unrecognized registry shape")
	}
	after2, _ := os.ReadFile(registry)
	if string(after2) != string(foreign) {
		t.Fatal("seed must not touch a registry it cannot interpret")
	}
}

// A current-format registry is read correctly and the doctor is read-only:
// two doctor runs leave the bytes unchanged and never create a sidecar.
func TestBrowserDoctorLoadsCurrentRegistryReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := fixedProfileNow()
	fixtureSocialAccounts(t, home, []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
	})
	reg, err := SeedBrowserProfileRegistry(home, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBrowserProfileRegistry(home, reg); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(home, "registries", "browser-profiles.json"))

	rep, err := DoctorBrowserProfileRegistry(home)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if rep.RegistryState != BrowserProfileRegistryPopulated || rep.ProfileCount != 1 {
		t.Fatalf("doctor must read the populated registry, got state=%q count=%d", rep.RegistryState, rep.ProfileCount)
	}
	after, _ := os.ReadFile(filepath.Join(home, "registries", "browser-profiles.json"))
	if string(after) != string(before) {
		t.Fatal("doctor must be read-only: the registry changed")
	}
	if _, err := os.Stat(filepath.Join(home, "registries", "browser-profiles.json.preimage")); !os.IsNotExist(err) {
		t.Fatalf("doctor must not write anything: %v", err)
	}
}

// A foreign (different) schema_version is reported as-is; the doctor must
// NOT auto-seed it back to the current format.
func TestBrowserDoctorReportsForeignVersionWithoutSeeding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fixtureSocialAccounts(t, home, []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
	})
	foreign := map[string]any{
		"schema_version": "compound.browser_profile_registry.v9.9",
		"updated_at":     "2026-01-01T00:00:00Z",
		"profiles":       []any{},
	}
	b, _ := json.MarshalIndent(foreign, "", "  ")
	registry := filepath.Join(home, "registries", "browser-profiles.json")
	if err := os.WriteFile(registry, b, 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := DoctorBrowserProfileRegistry(home)
	if err != nil {
		t.Fatalf("doctor on a foreign version must report it, not fail: %v", err)
	}
	if rep.RegistryState != BrowserProfileRegistryForeign {
		t.Fatalf("registry_state = %q, want foreign_version", rep.RegistryState)
	}
	after, _ := os.ReadFile(registry)
	if string(after) != string(b) {
		t.Fatal("doctor must not rewrite a foreign-version registry")
	}
}

// A valid current-format EMPTY registry is "valid but empty" — never seeded
// implicitly, and distinct from absent.
func TestBrowserDoctorDistinguishesValidEmptyFromAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fixtureSocialAccounts(t, home, []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
	})
	if err := os.MkdirAll(filepath.Join(home, "registries"), 0o755); err != nil {
		t.Fatal(err)
	}
	empty := map[string]any{"schema_version": BrowserProfileRegistrySchemaVersion, "updated_at": "2026-01-01T00:00:00Z", "profiles": []any{}}
	b, _ := json.MarshalIndent(empty, "", "  ")
	registry := filepath.Join(home, "registries", "browser-profiles.json")
	if err := os.WriteFile(registry, b, 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := DoctorBrowserProfileRegistry(home)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RegistryState != BrowserProfileRegistryEmpty {
		t.Fatalf("registry_state = %q, want empty", rep.RegistryState)
	}
	if len(rep.UnresolvedPaths) != 1 {
		t.Fatalf("the seeded account path should be named as unresolved so the operator seeds, got %v", rep.UnresolvedPaths)
	}
	after, _ := os.ReadFile(registry)
	if string(after) != string(b) {
		t.Fatal("a valid-empty registry must not be rewritten by the doctor")
	}

	// Absent state on a fresh home.
	fresh := t.TempDir()
	rep2, err := DoctorBrowserProfileRegistry(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.RegistryState != BrowserProfileRegistryAbsent || rep2.ProfileCount != 0 {
		t.Fatalf("fresh home must read absent with 0 profiles, got state=%q count=%d", rep2.RegistryState, rep2.ProfileCount)
	}
	if _, statErr := os.Stat(filepath.Join(fresh, "registries", "browser-profiles.json")); !os.IsNotExist(statErr) {
		t.Fatalf("the doctor must not create the registry: %v", statErr)
	}
}

// The explicit seed: CAS guard refuses a concurrent change; the preimage is
// preserved beside the file; a populated registry merges without clobber.
func TestBrowserSeedExplicitCASPresetAndMerge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := fixedProfileNow()
	fixtureSocialAccounts(t, home, []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
	})
	reg, err := SeedBrowserProfileRegistryExplicit(home, now)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if len(reg.Profiles) != 1 {
		t.Fatalf("first seed wants 1 profile, got %d", len(reg.Profiles))
	}
	// The preimage of an ABSENT file leaves no .preimage copy.
	if _, err := os.Stat(filepath.Join(home, "registries", "browser-profiles.json.preimage")); !os.IsNotExist(err) {
		t.Fatalf("absent preimage must not be materialized: %v", err)
	}

	// Second seed with a NEW account joins it into the existing registry
	// (merge, never clobber), preserving the preimage of the populated file.
	fixtureSocialAccounts(t, home, []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
		{"id": "acct-0002", "browser_profile": "~/social-profiles/profile-b"},
	})
	reg2, err := SeedBrowserProfileRegistryExplicit(home, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("merge seed: %v", err)
	}
	if len(reg2.Profiles) != 2 {
		t.Fatalf("merge seed wants 2 profiles, got %d", len(reg2.Profiles))
	}
	pre, err := os.ReadFile(filepath.Join(home, "registries", "browser-profiles.json.preimage"))
	if err != nil {
		t.Fatalf("preimage must be preserved: %v", err)
	}
	if !strings.Contains(string(pre), "acct-0001") {
		t.Fatalf("preserved preimage should carry the pre-seed content:\n%s", pre)
	}

	// CAS: a stale generation refuses the write; the preimage stands; the
	// current bytes are untouched.
	registry := filepath.Join(home, "registries", "browser-profiles.json")
	current, _ := os.ReadFile(registry)
	staleSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	seededReg, err := SeedBrowserProfileRegistry(home, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrowserProfileRegistryCAS(home, seededReg, staleSHA, nil); err == nil {
		t.Fatal("a stale preimage must refuse the seed write")
	}
	after, _ := os.ReadFile(registry)
	if string(after) != string(current) {
		t.Fatalf("a refused CAS write must leave the registry untouched")
	}
	// The correct generation writes through and preserves the preimage.
	if err := writeBrowserProfileRegistryCAS(home, seededReg, mustSHA256(t, home), current); err != nil {
		t.Fatalf("matching-generation seed write: %v", err)
	}
	pre2, err := os.ReadFile(registry + ".preimage")
	if err != nil {
		t.Fatalf("preimage must be preserved on the CAS write: %v", err)
	}
	if !strings.Contains(string(pre2), "acct-0001") {
		t.Fatalf("preserved preimage should carry the pre-write content:\n%s", pre2)
	}
}

// mustSHA256 returns the sha256 hex of the current registry file for CAS
// tests.
func mustSHA256(t *testing.T, home string) string {
	t.Helper()
	sha, _, err := browserRegistryPreimageSHA256(home)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// The public door: `compound harness browser-profiles doctor` is read-only
// (a fresh home reports absent and creates nothing) and `seed` is the
// explicit writer.
func TestBrowserOwnerSubcommandDoctorReadOnlySeedExplicit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COMPOUND_HOME", home)

	if !RunBrowserOwnerSubcommand("browser-profiles", []string{"doctor", "--home", home}) {
		t.Fatal("browser-profiles doctor must be handled by the browser-owner subcommand")
	}
	if _, statErr := os.Stat(filepath.Join(home, "registries", "browser-profiles.json")); !os.IsNotExist(statErr) {
		t.Fatalf("doctor must not create the registry on a fresh home: %v", statErr)
	}

	fixtureSocialAccounts(t, home, []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
	})
	if !RunBrowserOwnerSubcommand("browser-profiles", []string{"seed", "--home", home}) {
		t.Fatal("browser-profiles seed must be handled by the browser-owner subcommand")
	}
	read, err := LoadBrowserProfileRegistryState(home)
	if err != nil {
		t.Fatalf("post-seed registry: %v", err)
	}
	if read.State != BrowserProfileRegistryPopulated || len(read.Registry.Profiles) != 1 {
		t.Fatalf("seed must join the account into the registry: state=%q profiles=%d", read.State, len(read.Registry.Profiles))
	}
	// An unknown verb is refused, not silently mapped to the doctor.
	if !RunBrowserOwnerSubcommand("browser-profiles", []string{"no-such-verb", "--home", home}) {
		t.Fatal("browser-profiles owns the whole verb space (usage refusal still counts as handled)")
	}
}

// credential_ref is an optional pointer (a secret NAME, never a value). Rows
// without it keep loading; the explicit seed writer must carry it through.
func TestBrowserProfileCredentialRefIsOptionalAndSurvivesSeed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fixtureSocialAccounts(t, home, []map[string]string{
		{"id": "acct-0001", "browser_profile": "~/social-profiles/profile-a"},
	})
	dirA := filepath.Join(home, "social-profiles", "profile-a")
	body := `{"schema_version":"` + BrowserProfileRegistrySchemaVersion + `","updated_at":"2026-10-07T00:00:00Z","profiles":[` +
		`{"id":"acct-0001","user_data_dir":"` + dirA + `","backends":{},"logins":["acct-0001"],"status":"pending_enrollment","credential_ref":"example-login"},` +
		`{"id":"legacy","user_data_dir":"/nonexistent/legacy","backends":{},"logins":[],"status":"pending_enrollment"}]}`
	if err := os.MkdirAll(filepath.Join(home, "registries"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath(home), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := LoadBrowserProfileRegistryState(home)
	if err != nil || read.State != BrowserProfileRegistryPopulated {
		t.Fatalf("load: state=%q err=%v", read.State, err)
	}
	refs := map[string]string{}
	for _, p := range read.Registry.Profiles {
		refs[p.ID] = p.CredentialRef
	}
	if refs["acct-0001"] != "example-login" || refs["legacy"] != "" {
		t.Fatalf("credential_ref load = %v", refs)
	}
	if _, err := SeedBrowserProfileRegistryExplicit(home, fixedProfileNow()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	after, err := os.ReadFile(registryPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `"credential_ref": "example-login"`) {
		t.Fatalf("seed dropped credential_ref:\n%s", after)
	}
	if strings.Count(string(after), "credential_ref") != 1 {
		t.Fatalf("absent credential_ref must stay absent (omitempty):\n%s", after)
	}
}
