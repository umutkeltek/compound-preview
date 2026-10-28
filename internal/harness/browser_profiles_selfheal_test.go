package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The browser self-heal surface (internal/browser probe/heal) stores its
// config and state on registry rows. The explicit seed door rewrites the
// file through BrowserProfile, so it must carry those fields verbatim.
func TestSeedPreservesBrowserSelfHealFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "profiles", "example")
	fixtureSocialAccounts(t, home, []map[string]string{{"id": "example", "browser_profile": dir}})
	row := map[string]any{
		"id": "example", "user_data_dir": dir, "backends": map[string]any{}, "logins": []string{"example"}, "status": "pending_enrollment",
		"login_check_url":   "https://app.example.test/account",
		"logged_in_marker":  map[string]any{"text": "Welcome back"},
		"two_factor_marker": map[string]any{"text": "verification code"},
		"login_recipe":      "login",
		"last_probe":        map[string]any{"at": "2026-10-07T00:00:00Z", "logged_in": false},
		"heal_attempts":     []any{map[string]any{"at": "2026-10-07T00:00:00Z", "result": "login_failed"}},
	}
	body, _ := json.Marshal(map[string]any{"schema_version": BrowserProfileRegistrySchemaVersion, "profiles": []any{row}})
	if err := os.WriteFile(registryPath(home), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SeedBrowserProfileRegistryExplicit(home, fixedProfileNow()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	after, err := os.ReadFile(registryPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Profiles []map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(after, &reg); err != nil || len(reg.Profiles) != 1 {
		t.Fatalf("decode seeded registry: %v (%s)", err, after)
	}
	for _, key := range []string{"login_check_url", "logged_in_marker", "two_factor_marker", "login_recipe", "last_probe", "heal_attempts"} {
		if len(reg.Profiles[0][key]) == 0 {
			t.Errorf("seed dropped self-heal field %s:\n%s", key, after)
		}
	}
}
