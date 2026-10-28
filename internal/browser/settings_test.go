package browser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/policy"
)

func TestBrowserSettingsDefaultAndRouteProfileFence(t *testing.T) {
	home := t.TempDir()
	settings, err := LoadSettings(home)
	if err != nil || settings.JevEnabled {
		t.Fatalf("missing settings must disable Jev: %+v, %v", settings, err)
	}
	if route := SelectRoute(settings, ""); route.Backend != "playwright" {
		t.Fatalf("default route = %+v", route)
	}
	settings.JevEnabled = true
	if route := SelectRoute(settings, ""); route.Backend != "jev" {
		t.Fatalf("public enabled route = %+v", route)
	}
	if route := SelectRoute(settings, "profile-a"); route.Backend != "playwright" || route.ProfileID != "profile-a" {
		t.Fatalf("profile must stay with Playwright: %+v", route)
	}
}

func TestBrowserProviderPricingIsGovernedAndSurvivesToggle(t *testing.T) {
	home := t.TempDir()
	now := testTime()
	cfg := policy.DefaultConfig()
	if _, err := SetJevEnabled(home, "work_browser_test", true, cfg, now); err != nil {
		t.Fatal(err)
	}
	change, err := SetProviderPricing(home, "work_browser_test", "jev-1.13.0", 0.003, cfg, now.Add(time.Second))
	if err != nil || !change.Changed || change.ReceiptID == "" || change.DecisionID == "" {
		t.Fatalf("provider pricing change not governed: %+v err=%v", change, err)
	}
	if _, err := SetJevEnabled(home, "work_browser_test", false, cfg, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSettings(home)
	if err != nil || loaded.JevEnabled || loaded.ProviderModel != "jev-1.13.0" || loaded.ProviderMaxUSDPerRequest != 0.003 {
		t.Fatalf("provider price lost after toggle: %+v err=%v", loaded, err)
	}
	if _, err := SetProviderPricing(home, "work_browser_test", "jev-latest", 0.003, cfg, now.Add(3*time.Second)); err == nil {
		t.Fatal("moving alias accepted as priced model")
	}
}

func TestBrowserSettingsMalformedRefusesRoute(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(SettingsPath(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"jev_enabled":true}`, `{"schema_version":"foreign","jev_enabled":true}`, `{"schema_version":"compound.browser_settings.v0.1","jev_enabled":"true"}`} {
		if err := os.WriteFile(SettingsPath(home), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSettings(home); err == nil {
			t.Fatalf("malformed settings accepted: %s", body)
		}
	}
}

func TestBrowserSettingsToggleIsPolicyGatedAndReceipted(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cfg := policy.DefaultConfig()
	cfg.ClassOutcome["local_reversible"] = policy.ClassRule{Outcome: "hold"}
	refused, err := SetJevEnabled(home, "work_browser_toggle", true, cfg, now)
	if err != nil || !refused.Refused || refused.Changed {
		t.Fatalf("held toggle = %+v, %v", refused, err)
	}
	if _, err := os.Stat(SettingsPath(home)); !os.IsNotExist(err) {
		t.Fatalf("held toggle wrote config: %v", err)
	}
	cfg = policy.DefaultConfig()
	changed, err := SetJevEnabled(home, "work_browser_toggle", true, cfg, now.Add(time.Second))
	if err != nil || !changed.Changed || changed.ReceiptID == "" || changed.TransitionID == "" || changed.ActionID == "" || changed.DecisionID == "" {
		t.Fatalf("allowed toggle missing proof: %+v, %v", changed, err)
	}
	loaded, err := LoadSettings(home)
	if err != nil || !loaded.JevEnabled {
		t.Fatalf("enabled readback = %+v, %v", loaded, err)
	}
	unchanged, err := SetJevEnabled(home, "work_browser_toggle", true, cfg, now.Add(2*time.Second))
	if err != nil || unchanged.Changed || unchanged.ReceiptID != "" {
		t.Fatalf("same-value toggle must not write: %+v, %v", unchanged, err)
	}
}
