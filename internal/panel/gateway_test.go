package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"compound/internal/localmodel"
	"compound/internal/routeevidence"
)

func TestGatewayCallerSuccess(t *testing.T) {
	var gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		var body struct {
			Model          string              `json:"model"`
			ResponseFormat map[string]string   `json:"response_format"`
			Messages       []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = body.Model
		if body.ResponseFormat["type"] != "json_object" || len(body.Messages) != 2 || body.Messages[0]["content"] != "system" || body.Messages[1]["content"] != "user" {
			t.Fatalf("request body = %+v", body)
		}
		w.Header().Set("x-switchback-route", "strategy=auto/judge selected=nvidia/minimaxai/minimax-m3 fallbacks=[] rejected=0")
		w.Header().Set("x-switchback-request-id", "req_panel_gateway")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"confidence":0.8,"findings":[]}`}}},
			"usage":   map[string]int{"total_tokens": 33},
		})
	}))
	defer server.Close()

	caller := GatewayCaller{BaseURL: server.URL, APIKey: "secret", HTTPClient: server.Client(), ModelResolver: func(lane LaneRef) string {
		if lane.ModelID != "lane/free" {
			t.Fatalf("lane = %+v", lane)
		}
		return "served-model"
	}}
	got, err := caller.Call(context.Background(), CallRequest{Lane: LaneRef{ModelID: "lane/free"}, System: "system", User: "user"})
	if err != nil {
		t.Fatalf("gateway call: %v", err)
	}
	if gotModel != "served-model" || got.TotalTokens != 33 || !strings.Contains(string(got.Content), `"confidence"`) || got.LatencyMS < 0 {
		t.Fatalf("result model=%q got=%+v", gotModel, got)
	}
	if got.ResolvedProvider != "nvidia" || got.ResolvedModel != "nvidia/minimaxai/minimax-m3" || got.ResolutionSource != "gateway_route" || got.GatewayRequestID != "req_panel_gateway" {
		t.Fatalf("gateway metadata = %+v", got)
	}
}

func TestGatewayCallerMapsDirectResponseModelToDeclaredProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "qwen-fixture-served",
			"choices": []map[string]any{{"message": map[string]string{"content": `{"confidence":0.8,"findings":[]}`}}},
		})
	}))
	defer server.Close()
	lane := LaneRef{ModelID: "mac/qwen-fixture", Resolution: routeevidence.LaneResolution{Mode: "direct", Targets: []routeevidence.ResolvedTarget{{Provider: "lmstudio", Model: "qwen-fixture-served"}}}}
	got, err := (GatewayCaller{BaseURL: server.URL, HTTPClient: server.Client()}).Call(context.Background(), CallRequest{Lane: lane})
	if err != nil {
		t.Fatal(err)
	}
	if got.ResolvedProvider != "lmstudio" || got.ResolvedModel != "qwen-fixture-served" || got.ResolutionSource != "response_body" || got.GatewayRequestID != "" {
		t.Fatalf("direct metadata = %+v", got)
	}
}

func TestGatewayCallerPreflightRequiresLoadedMaxContext(t *testing.T) {
	loadedContext := 8192
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id":                    "served-model",
				"state":                 "loaded",
				"max_context_length":    262144,
				"loaded_context_length": loadedContext,
			}},
		})
	}))
	defer server.Close()
	caller := GatewayCaller{
		BaseURL:                 server.URL + "/v1",
		HTTPClient:              server.Client(),
		RequireLoadedContextMax: true,
		ModelResolver: func(lane LaneRef) string {
			if lane.ModelID == "lens-lane" {
				return "served-model"
			}
			return lane.ModelID
		},
	}
	err := caller.Preflight(context.Background(), Run{Route: RoutePlan{LensLane: LaneRef{ModelID: "lens-lane"}}})
	if err == nil || !strings.Contains(err.Error(), "loaded_context_length=8192 below max_context_length=262144") {
		t.Fatalf("err = %v, want context preflight failure", err)
	}
	if gotPath != "/api/v0/models" {
		t.Fatalf("models path = %q, want /api/v0/models", gotPath)
	}
}

func TestGatewayCallerPreflightAllowsLoadedMaxContext(t *testing.T) {
	loadedContext := 262144
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id":                    "served-model",
				"state":                 "loaded",
				"max_context_length":    262144,
				"loaded_context_length": loadedContext,
			}},
		})
	}))
	defer server.Close()
	caller := GatewayCaller{
		BaseURL:                 server.URL + "/v1",
		HTTPClient:              server.Client(),
		RequireLoadedContextMax: true,
		ModelResolver:           func(LaneRef) string { return "served-model" },
	}
	if err := caller.Preflight(context.Background(), Run{Route: RoutePlan{LensLane: LaneRef{ModelID: "lens-lane"}, JudgeLane: LaneRef{ModelID: "judge-lane"}}}); err != nil {
		t.Fatalf("preflight: %v", err)
	}
}

func TestGatewayCallerPreflightUsesHostContextLimit(t *testing.T) {
	loadedContext := 65536
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id":                    "served-model",
				"state":                 "loaded",
				"max_context_length":    262144,
				"loaded_context_length": loadedContext,
			}},
		})
	}))
	defer server.Close()
	caller := GatewayCaller{
		BaseURL:                 server.URL + "/v1",
		HTTPClient:              server.Client(),
		RequireLoadedContextMax: true,
		ContextPolicy:           localmodel.Policy{HostContextLimitTokens: 65536},
		ModelResolver:           func(LaneRef) string { return "served-model" },
	}
	if err := caller.Preflight(context.Background(), Run{Route: RoutePlan{LensLane: LaneRef{ModelID: "lens-lane"}}}); err != nil {
		t.Fatalf("preflight with host context cap: %v", err)
	}
	caller.ContextPolicy.HostContextLimitTokens = 131072
	err := caller.Preflight(context.Background(), Run{Route: RoutePlan{LensLane: LaneRef{ModelID: "lens-lane"}}})
	if err == nil || !strings.Contains(err.Error(), "loaded_context_length=65536 below desired_context=131072") {
		t.Fatalf("err = %v, want desired context cap failure", err)
	}
}

func TestGatewayCallerRetriesTransientUpstream400(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "missing `choices[0]`",
					"type":    "upstream_error",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"confidence":0.8,"findings":[]}`}}},
			"usage":   map[string]int{"total_tokens": 12},
		})
	}))
	defer server.Close()

	got, err := (GatewayCaller{BaseURL: server.URL, HTTPClient: server.Client()}).Call(context.Background(), CallRequest{Lane: LaneRef{ModelID: "model"}})
	if err != nil {
		t.Fatalf("gateway call after transient upstream 400: %v", err)
	}
	if calls != 2 || got.TotalTokens != 12 || !strings.Contains(string(got.Content), `"confidence"`) {
		t.Fatalf("calls=%d result=%+v", calls, got)
	}
}

func TestGatewayCallerNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := (GatewayCaller{BaseURL: server.URL, HTTPClient: server.Client()}).Call(context.Background(), CallRequest{Lane: LaneRef{ModelID: "model"}})
	if err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("err = %v, want status 502", err)
	}
}

func TestGatewayCallerMalformedJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()

	_, err := (GatewayCaller{BaseURL: server.URL, HTTPClient: server.Client()}).Call(context.Background(), CallRequest{Lane: LaneRef{ModelID: "model"}})
	if err == nil || !strings.Contains(err.Error(), "parse openai-compatible response") {
		t.Fatalf("err = %v, want malformed JSON error", err)
	}
}

func TestGatewayCallerContextTimeout(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := (GatewayCaller{BaseURL: server.URL, HTTPClient: server.Client()}).Call(ctx, CallRequest{Lane: LaneRef{ModelID: "model"}})
	<-started
	if err == nil {
		t.Fatalf("expected context timeout error")
	}
}
