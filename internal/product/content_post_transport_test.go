package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/act"
	"compound/internal/home"
)

func authorizedDraftFixture(t *testing.T, homePath string) ContentDraft {
	t.Helper()
	if err := home.Init(homePath, "test", contentQueueNow); err != nil {
		t.Fatal(err)
	}
	draft := recordQueueDraft(t, homePath, "grounded synthetic post", contentQueueNow.Add(72*time.Hour), contentQueueNow)
	if _, err := ApplyContentQueueReview(homePath, approveReview(t, homePath, "cqr_transport", draft.ID), "", contentQueueNow); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(homePath, "actuator", "transports")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"argv": []string{os.Args[0]}, "content_contract": act.ContentTransportContract})
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	return draft
}

func writeSocialRegistry(t *testing.T, homePath string, accounts []SocialAccount) {
	t.Helper()
	dir := filepath.Join(homePath, "registries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"schema_version": SocialAccountRegistrySchemaVersion,
		"accounts":       accounts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "social-accounts.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildContentPostTransportRequestJoinsApprovedDraftAndActiveAccount(t *testing.T) {
	homePath := t.TempDir()
	draft := authorizedDraftFixture(t, homePath)
	writeSocialRegistry(t, homePath, []SocialAccount{{
		ID:                  "prod-synth-x",
		ProductID:           draft.ProductID,
		Platform:            draft.Platform,
		Transport:           "fixture",
		PostizIntegrationID: "integration-synth",
		Status:              "active",
	}})

	request, auth, err := BuildContentPostTransportRequest(homePath, draft.ID, "fixture", contentQueueNow.Add(time.Hour), contentQueueNow)
	if err != nil {
		t.Fatal(err)
	}
	if request.SchemaVersion != ContentPostTransportRequestSchemaVersion || request.DraftID != draft.ID ||
		request.ProductID != draft.ProductID || request.Platform != draft.Platform ||
		request.Transport != "fixture" || request.IntegrationID != "integration-synth" ||
		request.PostType != "schedule" || request.Body != "grounded synthetic post" {
		t.Fatalf("transport request lost authorization data: %+v", request)
	}
	if request.ScheduleAt != contentQueueNow.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("schedule = %q", request.ScheduleAt)
	}
	if auth.ActionIntentID == "" || auth.PolicyDecisionID == "" {
		t.Fatalf("authorization missing governed pair: %+v", auth)
	}
}

func TestBuildContentPostTransportRequestRefusesUnreadyAccountJoins(t *testing.T) {
	tests := []struct {
		name     string
		accounts []SocialAccount
		want     string
	}{
		{
			name:     "pending enrollment",
			accounts: []SocialAccount{{ID: "prod-synth-x", ProductID: "product:prod_synth_a", Platform: "x", Transport: "fixture", Status: "pending_enrollment"}},
			want:     "human enrollment",
		},
		{
			name:     "missing integration",
			accounts: []SocialAccount{{ID: "prod-synth-x", ProductID: "product:prod_synth_a", Platform: "x", Transport: "fixture", Status: "active"}},
			want:     "no transport integration id",
		},
		{
			name: "ambiguous",
			accounts: []SocialAccount{
				{ID: "prod-synth-x-a", ProductID: "product:prod_synth_a", Platform: "x", Transport: "fixture", PostizIntegrationID: "a", Status: "active"},
				{ID: "prod-synth-x-b", ProductID: "product:prod_synth_a", Platform: "x", Transport: "fixture", PostizIntegrationID: "b", Status: "active"},
			},
			want: "ambiguous",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			draft := authorizedDraftFixture(t, homePath)
			writeSocialRegistry(t, homePath, tt.accounts)
			_, _, err := BuildContentPostTransportRequest(homePath, draft.ID, "fixture", contentQueueNow.Add(time.Hour), contentQueueNow)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBuildContentPostTransportRequestRefusesTamperedAuthorization(t *testing.T) {
	homePath := t.TempDir()
	draft := authorizedDraftFixture(t, homePath)
	writeSocialRegistry(t, homePath, []SocialAccount{{
		ID: "prod-synth-x", ProductID: draft.ProductID, Platform: draft.Platform,
		Transport: "fixture", PostizIntegrationID: "integration-synth", Status: "active",
	}})
	auth, err := LoadContentPostAuthorization(homePath, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	auth.BodyRef = "artifact:sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(homePath, "content-post-authorizations", draft.ID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err = BuildContentPostTransportRequest(homePath, draft.ID, "fixture", contentQueueNow.Add(time.Hour), contentQueueNow)
	if err == nil || !strings.Contains(err.Error(), "not bound to body ref") {
		t.Fatalf("tampered authorization error = %v", err)
	}
}
