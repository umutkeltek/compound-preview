package policy

import (
	"slices"
	"testing"
	"time"
)

func TestCredentialPasswordAlignmentClosedContainerTopologies(t *testing.T) {
	legacy := []string{"example-api", "example-worker", "example-provider-sync", "example-web", "example-cloudflared"}
	commercial := []string{"example-auth", "example-api", "example-worker", "example-billing", "example-billing-reconcile", "example-provider-sync", "example-web"}
	for _, tc := range []struct {
		name       string
		containers []string
		accepted   bool
	}{
		{"legacy_all", legacy, true},
		{"legacy_api_only", []string{"example-api"}, true},
		{"legacy_subset", []string{"example-api", "example-web"}, true},
		{"commercial_exact_seven", commercial, true},
		{"commercial_reordered", []string{"example-web", "example-provider-sync", "example-billing-reconcile", "example-billing", "example-worker", "example-api", "example-auth"}, true},
		{"empty", nil, false},
		{"unknown_legacy", []string{"example-api", "example-scheduler"}, false},
		{"missing_api", []string{"example-web"}, false},
		{"duplicate_legacy", []string{"example-api", "example-api"}, false},
		{"partial_commercial", commercial[:6], false},
		{"mixed_partial", []string{"example-api", "example-auth", "example-web"}, false},
		{"duplicate_commercial", append(slices.Clone(commercial[:6]), "example-api"), false},
		{"unknown_commercial", append(slices.Clone(commercial[:6]), "example-extra"), false},
		{"extra_legacy_tunnel", append(slices.Clone(commercial), "example-cloudflared"), false},
		{"different_prefix", append(slices.Clone(commercial[:6]), "other-web"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			r, _ := credentialPasswordAlignmentFixture(now)
			r.Profile.Containers = tc.containers
			err := ValidateCredentialPasswordAlignmentRequest(r, r.ProgramSHA256, r.ProgramSHA256, now)
			if (err == nil) != tc.accepted {
				t.Fatalf("accepted=%v want=%v: %v", err == nil, tc.accepted, err)
			}
		})
	}
}

func TestCredentialPasswordAlignmentTopologyRequiresQualifiedAPIEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "http://example-web:9000", "http://other-api:9000", "http://example_api:9000", "http://example-api:9000/extra", "http://user@example-api:9000"} {
		t.Run(endpoint, func(t *testing.T) {
			now := time.Now().UTC()
			r, _ := credentialPasswordAlignmentFixture(now)
			r.Profile.WebAPIBase = endpoint
			if ValidateCredentialPasswordAlignmentRequest(r, r.ProgramSHA256, r.ProgramSHA256, now) == nil {
				t.Fatal("unqualified or mismatched API endpoint accepted")
			}
		})
	}
}
