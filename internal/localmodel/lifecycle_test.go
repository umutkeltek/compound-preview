package localmodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEvaluateBlocksRequiredModelLoadedBelowAdaptiveContext(t *testing.T) {
	loaded := 8192
	report := Evaluate(map[string]Model{
		"judge": {ID: "judge", State: "loaded", MaxContextLength: 262144, LoadedContextLength: &loaded},
	}, []Requirement{{ModelID: "judge", MinContextTokens: 12000}}, Policy{ReserveTokens: 1000, HostContextLimitTokens: 64000})

	if report.Status != "blocked" || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one blocking finding", report)
	}
	finding := report.Findings[0]
	if finding.Action != ActionReloadHigherContext || !finding.Blocking || finding.DesiredContext != 13000 {
		t.Fatalf("finding = %+v, want reload higher to adaptive context", finding)
	}
	if err := report.BlockingError("preflight"); err == nil || !strings.Contains(err.Error(), "below desired_context=13000") {
		t.Fatalf("blocking error = %v, want desired context detail", err)
	}
}

func TestEvaluateCapsDesiredContextByHostAndModel(t *testing.T) {
	loaded := 32000
	report := Evaluate(map[string]Model{
		"extract": {ID: "extract", State: "loaded", MaxContextLength: 262144, LoadedContextLength: &loaded},
	}, []Requirement{{ModelID: "extract", MinContextTokens: 200000}}, Policy{ReserveTokens: 10000, HostContextLimitTokens: 64000})

	if report.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", report.Status)
	}
	if got := report.Findings[0].DesiredContext; got != 64000 {
		t.Fatalf("desired context = %d, want host cap 64000", got)
	}
}

func TestEvaluateSuggestsLowerContextWhenConservingMemory(t *testing.T) {
	loaded := 262144
	report := Evaluate(map[string]Model{
		"judge": {ID: "judge", State: "loaded", MaxContextLength: 262144, LoadedContextLength: &loaded},
	}, []Requirement{{ModelID: "judge", MinContextTokens: 12000}}, Policy{ReserveTokens: 1000, HostContextLimitTokens: 64000, ConserveMemory: true})

	if report.Status != "actionable" || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one actionable finding", report)
	}
	finding := report.Findings[0]
	if finding.Action != ActionReloadLowerWhenIdle || finding.Blocking || finding.DesiredContext != 13000 {
		t.Fatalf("finding = %+v, want non-blocking lower-context advice", finding)
	}
	if finding.LifecycleIntent == nil || finding.LifecycleIntent.Action != ActionReloadLowerWhenIdle || finding.LifecycleIntent.ContextTokens != 13000 {
		t.Fatalf("intent = %+v, want lower-context lifecycle intent", finding.LifecycleIntent)
	}
}

func TestEvaluateAddsSequentialPipelineAdviceForConstrainedHost(t *testing.T) {
	loaded := 65536
	report := Evaluate(map[string]Model{
		"ornith": {ID: "ornith", State: "loaded", MaxContextLength: 262144, LoadedContextLength: &loaded},
		"qwen":   {ID: "qwen", State: "loaded", MaxContextLength: 262144, LoadedContextLength: &loaded},
	}, []Requirement{{ModelID: "qwen", MinContextTokens: 12000}, {ModelID: "ornith", MinContextTokens: 16000}}, Policy{ReserveTokens: 1000, HostContextLimitTokens: 64000, MaxConcurrentLoaded: 1})

	if report.Status != "actionable" || report.PipelineAdvice == nil {
		t.Fatalf("report = %+v, want actionable sequential advice", report)
	}
	if report.PipelineAdvice.Mode != "sequential" || report.PipelineAdvice.MaxConcurrentLoaded != 1 {
		t.Fatalf("advice = %+v, want sequential max 1", report.PipelineAdvice)
	}
	if len(report.PipelineAdvice.Steps) != 2 {
		t.Fatalf("steps = %+v, want two lease steps", report.PipelineAdvice.Steps)
	}
	for _, step := range report.PipelineAdvice.Steps {
		if step.ReleaseIntent == nil || step.ReleaseIntent.Action != ActionUnloadIdle {
			t.Fatalf("step = %+v, want release intent", step)
		}
		if step.ModelID == "ornith" && step.DesiredContext != 17000 {
			t.Fatalf("ornith step = %+v, want desired context 17000", step)
		}
		if step.ModelID == "qwen" && step.DesiredContext != 13000 {
			t.Fatalf("qwen step = %+v, want desired context 13000", step)
		}
	}
}

func TestEvaluateSuggestsUnloadingIdleUnrequiredModels(t *testing.T) {
	loaded := 262144
	lastUsed := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)
	report := Evaluate(map[string]Model{
		"unused": {ID: "unused", State: "loaded", MaxContextLength: 262144, LoadedContextLength: &loaded, LastUsedAt: &lastUsed},
	}, nil, Policy{IdleTTL: time.Hour, Now: lastUsed.Add(2 * time.Hour)})

	if report.Status != "actionable" || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want actionable unload finding", report)
	}
	if report.Findings[0].Action != ActionUnloadIdle || report.Findings[0].Blocking {
		t.Fatalf("finding = %+v, want non-blocking unload advice", report.Findings[0])
	}
}

func TestEvaluateDefaultsZeroRequirementToModelMax(t *testing.T) {
	loaded := 8192
	report := Evaluate(map[string]Model{
		"judge": {ID: "judge", State: "loaded", MaxContextLength: 262144, LoadedContextLength: &loaded},
	}, []Requirement{{ModelID: "judge"}}, Policy{})

	finding := report.Findings[0]
	if finding.DesiredContext != 262144 || finding.Action != ActionReloadHigherContext {
		t.Fatalf("finding = %+v, want legacy max-context requirement", finding)
	}
}

func TestLMStudioClientModelsParsesModelMetadata(t *testing.T) {
	loaded := 262144
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			t.Fatalf("path = %q, want /api/v0/models", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id":                    "served",
				"state":                 "loaded",
				"max_context_length":    262144,
				"loaded_context_length": loaded,
				"last_used_at":          "2026-07-03T10:00:00Z",
			}},
		})
	}))
	defer server.Close()

	models, err := (LMStudioClient{BaseURL: server.URL + "/v1", HTTPClient: server.Client()}).Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	model := models["served"]
	if model.ID != "served" || model.LoadedContextLength == nil || *model.LoadedContextLength != loaded || model.LastUsedAt == nil {
		t.Fatalf("model = %+v, want parsed metadata", model)
	}
}

func TestLMStudioModelsURLRejectsRelativeURL(t *testing.T) {
	if _, err := LMStudioModelsURL("localhost:1234/v1"); err == nil {
		t.Fatal("expected relative URL error")
	}
}
