package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"compound/internal/distiller"
	"compound/internal/localmodel"
)

const defaultGatewayCallTimeout = 120 * time.Second

// GatewayCaller calls an OpenAI-compatible chat-completions endpoint for panel lenses.
type GatewayCaller struct {
	BaseURL       string
	APIKey        string
	HTTPClient    *http.Client
	ModelResolver func(lane LaneRef) string

	// RequireLoadedContextMax keeps the existing panel invariant: when this is
	// true, required local models must be loaded with their full runtime context.
	RequireLoadedContextMax bool
	ContextPolicy           localmodel.Policy
}

func (g GatewayCaller) RequiresMeteredBudget() bool { return true }

func (g GatewayCaller) Preflight(ctx context.Context, run Run) error {
	if !g.RequireLoadedContextMax {
		return nil
	}
	baseURL := strings.TrimSpace(g.BaseURL)
	if baseURL == "" {
		return fmt.Errorf("openai-compatible base_url is required")
	}
	models, err := (localmodel.LMStudioClient{BaseURL: baseURL, HTTPClient: g.HTTPClient}).Models(ctx)
	if err != nil {
		return fmt.Errorf("lm studio context preflight: %w", err)
	}
	requirements := make([]localmodel.Requirement, 0, 2)
	seen := map[string]bool{}
	for _, lane := range []LaneRef{run.Route.LensLane, run.Route.JudgeLane} {
		model := g.resolveModel(lane)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		requirements = append(requirements, localmodel.Requirement{ModelID: model})
	}
	report := localmodel.Evaluate(models, requirements, g.ContextPolicy)
	return report.BlockingError("lm studio context preflight")
}

func (g GatewayCaller) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	baseURL := strings.TrimSpace(g.BaseURL)
	if baseURL == "" {
		return CallResult{}, fmt.Errorf("openai-compatible base_url is required")
	}
	model := g.resolveModel(req.Lane)
	if model == "" {
		return CallResult{}, fmt.Errorf("model is required")
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultGatewayCallTimeout)
	defer cancel()
	content, usage, latencyMS, meta, err := distiller.ChatCompletionJSONWithMeta(callCtx, g.HTTPClient, baseURL, g.APIKey, model, req.System, req.User)
	resolvedProvider := strings.TrimSpace(meta.ResolvedProvider)
	if resolvedProvider == "" && meta.ResolutionSource == "response_body" {
		resolvedProvider = providerForResolvedModel(req.Lane, meta.ResolvedModel)
	}
	result := CallResult{Content: content, TotalTokens: usage.TotalTokens, LatencyMS: latencyMS, ResolvedProvider: resolvedProvider, ResolvedModel: meta.ResolvedModel, ResolutionSource: meta.ResolutionSource, GatewayRequestID: meta.GatewayRequestID}
	if err != nil {
		return result, err
	}
	return result, nil
}

func providerForResolvedModel(lane LaneRef, model string) string {
	model = strings.TrimSpace(model)
	provider := ""
	for _, target := range lane.Resolution.Targets {
		if strings.TrimSpace(target.Model) != model {
			continue
		}
		candidate := strings.TrimSpace(target.Provider)
		if candidate == "" || (provider != "" && provider != candidate) {
			return ""
		}
		provider = candidate
	}
	return provider
}

func (g GatewayCaller) resolveModel(lane LaneRef) string {
	model := lane.ModelID
	if g.ModelResolver != nil {
		model = g.ModelResolver(lane)
	}
	return strings.TrimSpace(model)
}
