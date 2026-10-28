package localmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const DefaultReserveTokens = 4096

type Model struct {
	ID                  string
	State               string
	MaxContextLength    int
	LoadedContextLength *int
	LastUsedAt          *time.Time
}

type Requirement struct {
	ModelID          string
	MinContextTokens int
}

type Policy struct {
	HostContextLimitTokens int
	ReserveTokens          int
	IdleTTL                time.Duration
	Now                    time.Time
	ConserveMemory         bool
	MaxConcurrentLoaded    int
}

type Action string

const (
	ActionOK                   Action = "ok"
	ActionLoad                 Action = "load"
	ActionReloadHigherContext  Action = "reload_higher_context"
	ActionReloadLowerWhenIdle  Action = "reload_lower_context_when_idle"
	ActionUnloadIdle           Action = "unload_idle"
	ActionMissingContext       Action = "missing_loaded_context"
	ActionMissingModel         Action = "missing_model"
	ActionUnknownModelContext  Action = "unknown_model_context"
	ActionUnknownLoadedContext Action = "unknown_loaded_context"
)

type Finding struct {
	ModelID          string           `json:"model_id"`
	Action           Action           `json:"action"`
	Blocking         bool             `json:"blocking"`
	State            string           `json:"state,omitempty"`
	DesiredContext   int              `json:"desired_context,omitempty"`
	LoadedContext    int              `json:"loaded_context,omitempty"`
	MaxContext       int              `json:"max_context,omitempty"`
	Reason           string           `json:"reason"`
	RecommendedSteps []string         `json:"recommended_steps,omitempty"`
	LifecycleIntent  *LifecycleIntent `json:"lifecycle_intent,omitempty"`
}

type Report struct {
	Status         string          `json:"status"`
	Findings       []Finding       `json:"findings"`
	PipelineAdvice *PipelineAdvice `json:"pipeline_advice,omitempty"`
}

type PipelineAdvice struct {
	Mode                string      `json:"mode"`
	MaxConcurrentLoaded int         `json:"max_concurrent_loaded,omitempty"`
	Reason              string      `json:"reason"`
	Steps               []LeaseStep `json:"steps,omitempty"`
}

type LeaseStep struct {
	Order          int              `json:"order"`
	ModelID        string           `json:"model_id"`
	DesiredContext int              `json:"desired_context,omitempty"`
	LoadIntent     *LifecycleIntent `json:"load_intent,omitempty"`
	ReleaseIntent  *LifecycleIntent `json:"release_intent,omitempty"`
}

func Evaluate(models map[string]Model, requirements []Requirement, policy Policy) Report {
	needed := map[string]Requirement{}
	for _, req := range requirements {
		req.ModelID = strings.TrimSpace(req.ModelID)
		if req.ModelID == "" {
			continue
		}
		existing := needed[req.ModelID]
		if req.MinContextTokens > existing.MinContextTokens {
			existing.MinContextTokens = req.MinContextTokens
		}
		existing.ModelID = req.ModelID
		needed[req.ModelID] = existing
	}

	var findings []Finding
	ids := make([]string, 0, len(needed))
	for id := range needed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		req := needed[id]
		model, ok := models[id]
		if !ok {
			findings = append(findings, Finding{
				ModelID:          id,
				Action:           ActionMissingModel,
				Blocking:         true,
				Reason:           "required model is not listed by local model runtime",
				DesiredContext:   req.MinContextTokens,
				RecommendedSteps: []string{"load " + id + contextArg(req.MinContextTokens)},
				LifecycleIntent:  NewLifecycleIntent(id, ActionLoad, req.MinContextTokens),
			})
			continue
		}
		findings = append(findings, evaluateRequiredModel(model, req, policy))
	}

	for _, model := range sortedModels(models) {
		if _, ok := needed[model.ID]; ok {
			continue
		}
		if idleTooLong(model, policy) {
			findings = append(findings, Finding{
				ModelID:          model.ID,
				Action:           ActionUnloadIdle,
				State:            model.State,
				LoadedContext:    loadedContextValue(model),
				MaxContext:       model.MaxContextLength,
				Reason:           "loaded model is not required and has exceeded idle TTL",
				RecommendedSteps: []string{"unload " + model.ID},
				LifecycleIntent:  NewLifecycleIntent(model.ID, ActionUnloadIdle, 0),
			})
		}
	}

	status := "ok"
	for _, finding := range findings {
		if finding.Blocking {
			status = "blocked"
			break
		}
		if finding.Action != ActionOK {
			status = "actionable"
		}
	}
	advice := pipelineAdvice(models, ids, needed, policy)
	if status == "ok" && advice != nil {
		status = "actionable"
	}
	return Report{Status: status, Findings: findings, PipelineAdvice: advice}
}

func (r Report) BlockingError(prefix string) error {
	for _, finding := range r.Findings {
		if finding.Blocking {
			if prefix = strings.TrimSpace(prefix); prefix != "" {
				return fmt.Errorf("%s: %s", prefix, finding.Reason)
			}
			return fmt.Errorf("%s", finding.Reason)
		}
	}
	return nil
}

func evaluateRequiredModel(model Model, req Requirement, policy Policy) Finding {
	desired := desiredContext(model, req, policy)
	base := Finding{
		ModelID:        model.ID,
		State:          model.State,
		DesiredContext: desired,
		LoadedContext:  loadedContextValue(model),
		MaxContext:     model.MaxContextLength,
	}
	if model.MaxContextLength <= 0 {
		base.Action = ActionUnknownModelContext
		base.Blocking = true
		base.Reason = "required model has no max_context_length metadata"
		return base
	}
	if strings.TrimSpace(model.State) != "loaded" {
		base.Action = ActionLoad
		base.Blocking = true
		base.Reason = fmt.Sprintf("model %q state=%q; load it before local execution", model.ID, model.State)
		base.RecommendedSteps = []string{"load " + model.ID + contextArg(desired)}
		base.LifecycleIntent = NewLifecycleIntent(model.ID, base.Action, desired)
		return base
	}
	if model.LoadedContextLength == nil {
		base.Action = ActionUnknownLoadedContext
		base.Blocking = true
		base.Reason = fmt.Sprintf("model %q loaded_context_length missing", model.ID)
		return base
	}
	if *model.LoadedContextLength < desired {
		base.Action = ActionReloadHigherContext
		base.Blocking = true
		if desired == model.MaxContextLength {
			base.Reason = fmt.Sprintf("model %q loaded_context_length=%d below max_context_length=%d", model.ID, *model.LoadedContextLength, model.MaxContextLength)
		} else {
			base.Reason = fmt.Sprintf("model %q loaded_context_length=%d below desired_context=%d", model.ID, *model.LoadedContextLength, desired)
		}
		base.RecommendedSteps = []string{"reload " + model.ID + contextArg(desired)}
		base.LifecycleIntent = NewLifecycleIntent(model.ID, base.Action, desired)
		return base
	}
	if desired > 0 && *model.LoadedContextLength > desired && (idleTooLong(model, policy) || policy.ConserveMemory) {
		base.Action = ActionReloadLowerWhenIdle
		if idleTooLong(model, policy) {
			base.Reason = fmt.Sprintf("model %q loaded_context_length=%d exceeds desired_context=%d and is idle", model.ID, *model.LoadedContextLength, desired)
		} else {
			base.Reason = fmt.Sprintf("model %q loaded_context_length=%d exceeds desired_context=%d; memory conservation requested", model.ID, *model.LoadedContextLength, desired)
		}
		base.RecommendedSteps = []string{"reload " + model.ID + contextArg(desired)}
		base.LifecycleIntent = NewLifecycleIntent(model.ID, base.Action, desired)
		return base
	}
	base.Action = ActionOK
	base.Reason = "model loaded with sufficient context"
	return base
}

func desiredContext(model Model, req Requirement, policy Policy) int {
	desired := req.MinContextTokens
	if desired <= 0 {
		desired = model.MaxContextLength
	} else {
		reserve := policy.ReserveTokens
		if reserve == 0 {
			reserve = DefaultReserveTokens
		}
		desired += reserve
	}
	if model.MaxContextLength > 0 && (desired <= 0 || desired > model.MaxContextLength) {
		desired = model.MaxContextLength
	}
	if policy.HostContextLimitTokens > 0 && (desired <= 0 || desired > policy.HostContextLimitTokens) {
		desired = policy.HostContextLimitTokens
	}
	if desired < 0 {
		return 0
	}
	return desired
}

func pipelineAdvice(models map[string]Model, ids []string, needed map[string]Requirement, policy Policy) *PipelineAdvice {
	if len(ids) == 0 {
		return nil
	}
	maxConcurrent := policy.MaxConcurrentLoaded
	if maxConcurrent <= 0 {
		if !policy.ConserveMemory {
			return nil
		}
		maxConcurrent = 1
	}
	if maxConcurrent >= len(ids) && !policy.ConserveMemory {
		return nil
	}
	mode := "batched"
	if maxConcurrent <= 1 {
		mode = "sequential"
	}
	reason := fmt.Sprintf("%d required models exceed max_concurrent_loaded=%d; lease models through local-routing instead of keeping all resident", len(ids), maxConcurrent)
	if policy.ConserveMemory && maxConcurrent >= len(ids) {
		reason = "memory conservation requested; release required local models after active use"
	}
	steps := make([]LeaseStep, 0, len(ids))
	for i, id := range ids {
		model := models[id]
		req := needed[id]
		desired := desiredContext(model, req, policy)
		if model.ID == "" {
			desired = req.MinContextTokens
		}
		var loadIntent *LifecycleIntent
		if model.ID == "" || strings.TrimSpace(model.State) != "loaded" {
			loadIntent = NewLifecycleIntent(id, ActionLoad, desired)
		} else if model.LoadedContextLength != nil && desired > 0 && *model.LoadedContextLength < desired {
			loadIntent = NewLifecycleIntent(id, ActionReloadHigherContext, desired)
		}
		steps = append(steps, LeaseStep{
			Order:          i + 1,
			ModelID:        id,
			DesiredContext: desired,
			LoadIntent:     loadIntent,
			ReleaseIntent:  NewLifecycleIntent(id, ActionUnloadIdle, 0),
		})
	}
	return &PipelineAdvice{
		Mode:                mode,
		MaxConcurrentLoaded: maxConcurrent,
		Reason:              reason,
		Steps:               steps,
	}
}

func idleTooLong(model Model, policy Policy) bool {
	if policy.IdleTTL <= 0 || model.LastUsedAt == nil {
		return false
	}
	now := policy.Now
	if now.IsZero() {
		now = time.Now()
	}
	return now.Sub(*model.LastUsedAt) >= policy.IdleTTL
}

func loadedContextValue(model Model) int {
	if model.LoadedContextLength == nil {
		return 0
	}
	return *model.LoadedContextLength
}

func sortedModels(models map[string]Model) []Model {
	out := make([]Model, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ID) != "" {
			out = append(out, model)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func contextArg(contextTokens int) string {
	if contextTokens <= 0 {
		return ""
	}
	return fmt.Sprintf(" --context %d", contextTokens)
}

type LMStudioClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func (c LMStudioClient) Models(ctx context.Context) (map[string]Model, error) {
	modelsURL, err := LMStudioModelsURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return ParseLMStudioModels(raw)
}

func ParseLMStudioModels(raw []byte) (map[string]Model, error) {
	var payload struct {
		Data []struct {
			ID                  string `json:"id"`
			State               string `json:"state"`
			MaxContextLength    int    `json:"max_context_length"`
			LoadedContextLength *int   `json:"loaded_context_length"`
			LastUsedAt          string `json:"last_used_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}
	out := make(map[string]Model, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		var lastUsed *time.Time
		if strings.TrimSpace(item.LastUsedAt) != "" {
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(item.LastUsedAt)); err == nil {
				lastUsed = &parsed
			}
		}
		out[id] = Model{
			ID:                  id,
			State:               item.State,
			MaxContextLength:    item.MaxContextLength,
			LoadedContextLength: item.LoadedContextLength,
			LastUsedAt:          lastUsed,
		}
	}
	return out, nil
}

func LMStudioModelsURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("openai-compatible base_url must be absolute")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		path = strings.TrimSuffix(path, "/v1")
	}
	parsed.Path = strings.TrimRight(path, "/") + "/api/v0/models"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
