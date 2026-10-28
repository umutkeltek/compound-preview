package browser

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserPortableCLISettingsReachRouteConsumer(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	handled, err := runPortableBrowser([]string{"route", "--json", "--home", home}, &out)
	if !handled || err != nil {
		t.Fatalf("default route: handled=%t err=%v", handled, err)
	}
	var route Route
	if err := json.Unmarshal(out.Bytes(), &route); err != nil || route.Backend != "playwright" {
		t.Fatalf("default route output: %s, %v", out.String(), err)
	}
	out.Reset()
	handled, err = runPortableBrowser([]string{"settings", "--enable", "--work", "work_browser_cli", "--json", "--home", home}, &out)
	if !handled || err != nil {
		t.Fatalf("enable: handled=%t err=%v output=%s", handled, err, out.String())
	}
	var changed SettingsChange
	if err := json.Unmarshal(out.Bytes(), &changed); err != nil || !changed.Changed || changed.ReceiptID == "" {
		t.Fatalf("enable output: %s, %v", out.String(), err)
	}
	out.Reset()
	handled, err = runPortableBrowser([]string{"route", "--json", "--home", home}, &out)
	if !handled || err != nil {
		t.Fatalf("enabled route: handled=%t err=%v", handled, err)
	}
	if err := json.Unmarshal(out.Bytes(), &route); err != nil || route.Backend != "jev" {
		t.Fatalf("enabled route output: %s, %v", out.String(), err)
	}
	out.Reset()
	handled, err = runPortableBrowser([]string{"route", "--json", "--home", home, "--profile", "profile-a"}, &out)
	if !handled || err != nil {
		t.Fatalf("profile route: handled=%t err=%v", handled, err)
	}
	if err := json.Unmarshal(out.Bytes(), &route); err != nil || route.Backend != "playwright" {
		t.Fatalf("profile route output: %s, %v", out.String(), err)
	}
}

func TestBrowserProviderPricingCLIUpdatesCompoundSettings(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	handled, err := runPortableBrowser([]string{"provider-pricing", "--work", "work_browser_cli", "--model", "jev-1.13.0", "--max-usd-per-request", "0.003", "--json", "--home", home}, &out)
	if !handled || err != nil {
		t.Fatalf("provider pricing CLI: handled=%t err=%v", handled, err)
	}
	settings, err := LoadSettings(home)
	if err != nil || settings.ProviderModel != "jev-1.13.0" || settings.ProviderMaxUSDPerRequest != 0.003 {
		t.Fatalf("provider pricing not persisted: %+v err=%v", settings, err)
	}
}

func TestBrowserCredentialCLIReadsSecretOnlyFromInput(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	policySource := []byte("standing_grants:\n  - id: test-jev-secret\n    capability: browser.credential.store\n    mutation_class: security\n    reversibility_ceiling: hard\n    blast_radius: local\n")
	if err := os.WriteFile(filepath.Join(home, "config", "policy.yaml"), policySource, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	handled, err := runPortableBrowserInput([]string{"credential-set", "--work", "work_browser_cli", "--confirm", "--json", "--home", home}, &out, strings.NewReader("synthetic-cli-key\n"))
	if !handled || err != nil {
		t.Fatalf("credential CLI: handled=%t err=%v output=%s", handled, err, out.String())
	}
	if strings.Contains(out.String(), "synthetic-cli-key") {
		t.Fatal("credential CLI printed secret")
	}
	var result CredentialChange
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Stored {
		t.Fatalf("credential CLI response: %s err=%v", out.String(), err)
	}
	out.Reset()
	handled, err = runPortableBrowserInput([]string{"credential-rotate", "--work", "work_browser_cli", "--confirm", "--json", "--home", home}, &out, strings.NewReader("synthetic-new-cli-key\n"))
	if !handled || err != nil || strings.Contains(out.String(), "synthetic-new-cli-key") {
		t.Fatalf("credential rotation CLI: handled=%t err=%v output=%s", handled, err, out.String())
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Stored {
		t.Fatalf("credential rotation CLI response: %s err=%v", out.String(), err)
	}
}

func TestBrowserPortableCLIRejectsMalformedFlagsBeforeMutation(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{
		{"settings", "--enable", "--disable", "--work", "work_browser_cli", "--home", home},
		{"settings", "--enable", "--home", home},
		{"route", "--enable", "--home", home},
		{"settings", "--unknown", "--home", home},
	} {
		if handled, err := runPortableBrowser(args, &bytes.Buffer{}); !handled || err == nil {
			t.Fatalf("invalid args accepted: %v (handled=%t, err=%v)", args, handled, err)
		}
	}
	settings, err := LoadSettings(home)
	if err != nil || settings.JevEnabled {
		t.Fatalf("invalid args changed settings: %+v, %v", settings, err)
	}
}

func TestBrowserGoalProfileHandoffNeedsNoProviderBudget(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	handled, err := runPortableBrowser([]string{"goal", "--home", home, "--work", "work_browser_cli", "--actor", "browser-test", "--url", "https://example.com/", "--goal", "Read the heading", "--expect-text", "Example Domain", "--profile", "profile-a", "--json"}, &out)
	if !handled || err != ErrRoutedToPlaywright {
		t.Fatalf("profile handoff: handled=%t err=%v", handled, err)
	}
	var result TaskResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Backend != "playwright" || result.Status != "routed" || result.Verified {
		t.Fatalf("profile handoff result: %+v, %v", result, err)
	}
}
