package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalModelsAdviseCLIJSONBlocksLowContext(t *testing.T) {
	loaded := 8192
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			t.Fatalf("path = %q, want /api/v0/models", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id":                    "judge",
				"state":                 "loaded",
				"max_context_length":    262144,
				"loaded_context_length": loaded,
			}},
		})
	}))
	defer server.Close()

	out := captureStdout(t, func() {
		cmdLocalModels([]string{"advise", "--lm-studio-url", server.URL + "/v1", "--required", "judge", "--json"})
	})
	var report struct {
		Status   string `json:"status"`
		Findings []struct {
			ModelID        string `json:"model_id"`
			Action         string `json:"action"`
			Blocking       bool   `json:"blocking"`
			DesiredContext int    `json:"desired_context"`
			Intent         struct {
				Owner         string `json:"owner"`
				Action        string `json:"action"`
				ContextTokens int    `json:"context_tokens"`
				Delegation    string `json:"delegation"`
			} `json:"lifecycle_intent"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if report.Status != "blocked" || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one blocked finding", report)
	}
	finding := report.Findings[0]
	if finding.ModelID != "judge" || finding.Action != "reload_higher_context" || !finding.Blocking || finding.DesiredContext != 262144 {
		t.Fatalf("finding = %+v, want max-context reload blocker", finding)
	}
	if finding.Intent.Owner != "local-routing" || finding.Intent.Action != "reload_higher_context" || finding.Intent.ContextTokens != 262144 || finding.Intent.Delegation == "" {
		t.Fatalf("intent = %+v, want local-routing reload intent", finding.Intent)
	}
}

func TestLocalModelsAdviseHumanShowsUnloadAdvice(t *testing.T) {
	loaded := 262144
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id":                    "unused",
				"state":                 "loaded",
				"max_context_length":    262144,
				"loaded_context_length": loaded,
				"last_used_at":          "1970-01-01T00:00:00Z",
			}},
		})
	}))
	defer server.Close()

	out := captureStdout(t, func() {
		cmdLocalModels([]string{"advise", "--lm-studio-url", server.URL, "--idle-ttl", "1ns"})
	})
	if !strings.Contains(out, "action=unload_idle") || !strings.Contains(out, "next: unload unused") {
		t.Fatalf("output missing unload advice:\n%s", out)
	}
}
