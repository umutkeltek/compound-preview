package routeevidence

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderEndpointAuthorityProjectionRevisionAndDrift(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "models.yaml")
	body := `version: 1
purpose: synthetic endpoint tuple
capability_classes: [build-implement]
models:
  fixture/control:
    lane: synthetic
    harness: opencode
    provider_route: fixture
    provider_endpoint: https://provider.invalid/v1
    tier: premium
    packet_ceiling: repo-scoped
    capabilities: {build-implement: {level: capable, evidence: fixture}}
routing_defaults:
  build-implement: [fixture/control]
`
	if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, source, t0); err != nil {
		t.Fatal(err)
	}
	manifest, _, revision, err := LoadModelCapabilitiesSnapshot(home)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Models["fixture/control"].ProviderEndpoint != "https://provider.invalid/v1" {
		t.Fatal("authority omitted endpoint")
	}
	projection, err := ProjectModelCapabilities(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(projection)
	if !strings.Contains(string(encoded), `"provider_endpoint":"https://provider.invalid/v1"`) {
		t.Fatal("JSON projection omitted endpoint")
	}
	view, err := ProjectModelCapabilitiesYAML(home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(view), "provider_endpoint: https://provider.invalid/v1") {
		t.Fatal("YAML projection omitted endpoint")
	}
	changed, err := DecodeModelCapabilitiesYAML([]byte(strings.ReplaceAll(body, "provider.invalid/v1", "provider.invalid/v2")))
	if err != nil {
		t.Fatal(err)
	}
	if diff := strings.Join(diffModelCapabilities(manifest, changed), "\n"); !strings.Contains(diff, "provider_endpoint") {
		t.Fatalf("endpoint drift invisible: %s", diff)
	}
	if err := os.WriteFile(source, []byte(strings.ReplaceAll(body, "provider.invalid/v1", "provider.invalid/v2")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, source, t0.Add(1)); err != nil {
		t.Fatal(err)
	}
	_, _, next, err := LoadModelCapabilitiesSnapshot(home)
	if err != nil || next == revision {
		t.Fatalf("endpoint did not revise authority: %v", err)
	}
}

func TestProviderEndpointMergeAndProjectionRedaction(t *testing.T) {
	existing := ModelCapabilityEntry{ProviderEndpoint: "https://provider.invalid/v1"}
	if got := mergeModelCapabilityEntry(existing, ModelCapabilityEntry{}); got.ProviderEndpoint != existing.ProviderEndpoint {
		t.Fatal("empty import erased endpoint")
	}
	if got := mergeModelCapabilityEntry(existing, ModelCapabilityEntry{ProviderEndpoint: "https://provider.invalid/v2"}); got.ProviderEndpoint != "https://provider.invalid/v2" {
		t.Fatal("endpoint update lost")
	}
	userinfo := &url.URL{Scheme: "https", Host: "provider.invalid", Path: "/v1", User: url.UserPassword("synthetic-user", "synthetic-key")}
	for _, endpoint := range []string{userinfo.String(), "https://provider.invalid/v1?key=secret", "https://provider.invalid/v1#secret"} {
		if got := sanitizeModelCapabilitiesProjectionEndpoint(endpoint); strings.Contains(got, "secret") || got == endpoint {
			t.Fatalf("sensitive endpoint projected: %s", got)
		}
	}
	if got := sanitizeModelCapabilitiesProjectionEndpoint(""); got != "" {
		t.Fatalf("legacy empty endpoint changed: %s", got)
	}
}
