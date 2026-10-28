package distiller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"compound/internal/routeevidence"
)

const LiveExtractionSchemaVersion = "compound.distiller.live_extraction.v0.1"

type OpenAICompatibleOptions struct {
	BaseURL       string
	APIKey        string
	Model         string
	EvalCorpusDir string
	EvalRoot      string
	HTTPClient    *http.Client
	Now           time.Time
}

type OpenAIUsage = routeevidence.Usage

type LiveExtractionReport struct {
	SchemaVersion    string                `json:"schema_version"`
	Provider         string                `json:"provider"`
	Model            string                `json:"model"`
	ResolvedProvider string                `json:"resolved_provider,omitempty"`
	ResolvedModel    string                `json:"resolved_model,omitempty"`
	ResolutionSource string                `json:"resolution_source,omitempty"`
	GatewayRequestID string                `json:"gateway_request_id,omitempty"`
	Called           bool                  `json:"called"`
	Eval             EvalCorpusReport      `json:"eval"`
	Usage            OpenAIUsage           `json:"usage,omitempty"`
	LatencyMS        int64                 `json:"latency_ms,omitempty"`
	Response         LLMExtractionResponse `json:"response,omitempty"`
	LiveModelCalls   int                   `json:"live_model_calls"`
}

type openAIChatRequest struct {
	Model          string              `json:"model"`
	Messages       []openAIChatMessage `json:"messages"`
	Temperature    float64             `json:"temperature"`
	ResponseFormat map[string]string   `json:"response_format,omitempty"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Model   string `json:"model,omitempty"`
	Choices []struct {
		Message openAIChatMessage `json:"message"`
	} `json:"choices"`
	Usage OpenAIUsage `json:"usage,omitempty"`
}

type OpenAICompatibleResponseMeta struct {
	ResolvedProvider string `json:"resolved_provider,omitempty"`
	ResolvedModel    string `json:"resolved_model,omitempty"`
	ResolutionSource string `json:"resolution_source,omitempty"`
	GatewayRequestID string `json:"gateway_request_id,omitempty"`
}

func ExtractWithOpenAICompatible(ctx context.Context, req LLMExtractionRequest, opts OpenAICompatibleOptions) (LiveExtractionReport, error) {
	report := LiveExtractionReport{
		SchemaVersion: LiveExtractionSchemaVersion,
		Provider:      "openai-compatible",
		Model:         strings.TrimSpace(opts.Model),
	}
	if strings.TrimSpace(opts.EvalCorpusDir) == "" {
		return report, fmt.Errorf("eval corpus is required before provider extraction")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	eval, err := EvaluateCorpusDirInRoot(opts.EvalCorpusDir, opts.EvalRoot, now)
	report.Eval = eval
	if err != nil {
		return report, err
	}
	if eval.Status != "pass" {
		return report, fmt.Errorf("eval corpus status %s; provider call blocked", eval.Status)
	}
	if eval.LiveModelCalls != 0 {
		return report, fmt.Errorf("eval corpus reported %d live model call(s); provider call blocked", eval.LiveModelCalls)
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return report, fmt.Errorf("openai-compatible base_url is required")
	}
	if report.Model == "" {
		return report, fmt.Errorf("model is required")
	}

	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	started := time.Now()
	response, usage, liveModelCalls, meta, err := callOpenAICompatible(ctx, client, opts.BaseURL, opts.APIKey, report.Model, req)
	report.Called = true
	report.LiveModelCalls = liveModelCalls
	report.Usage = usage
	report.LatencyMS = time.Since(started).Milliseconds()
	report.Response = response
	report.ResolvedProvider = meta.ResolvedProvider
	report.ResolvedModel = meta.ResolvedModel
	report.ResolutionSource = meta.ResolutionSource
	report.GatewayRequestID = meta.GatewayRequestID
	if err != nil {
		return report, err
	}
	return report, nil
}

func ChatCompletionJSON(ctx context.Context, client *http.Client, baseURL, apiKey, model, system, user string) (content []byte, usage OpenAIUsage, latencyMS int64, err error) {
	content, usage, latencyMS, _, err = ChatCompletionJSONWithMeta(ctx, client, baseURL, apiKey, model, system, user)
	return content, usage, latencyMS, err
}

func ChatCompletionJSONWithMeta(ctx context.Context, client *http.Client, baseURL, apiKey, model, system, user string) (content []byte, usage OpenAIUsage, latencyMS int64, meta OpenAICompatibleResponseMeta, err error) {
	if client == nil {
		client = http.DefaultClient
	}
	jsonResponseFormat := true
	content, usage, latencyMS, meta, err = chatCompletionOnce(ctx, client, baseURL, apiKey, model, system, user, jsonResponseFormat)
	// Some local gateways (e.g. LM Studio) reject response_format "json_object";
	// retry once without it — the prompts already demand strict JSON and callers
	// strict-decode the content.
	if err != nil && strings.Contains(err.Error(), "response_format") {
		var retryLatency int64
		var retryMeta OpenAICompatibleResponseMeta
		jsonResponseFormat = false
		content, usage, retryLatency, retryMeta, err = chatCompletionOnce(ctx, client, baseURL, apiKey, model, system, user, jsonResponseFormat)
		latencyMS += retryLatency
		meta = retryMeta
	}
	if isTransientOpenAICompatibleUpstreamError(err) {
		var retryLatency int64
		var retryMeta OpenAICompatibleResponseMeta
		content, usage, retryLatency, retryMeta, err = chatCompletionOnce(ctx, client, baseURL, apiKey, model, system, user, jsonResponseFormat)
		latencyMS += retryLatency
		meta = retryMeta
	}
	if err != nil {
		return content, usage, latencyMS, meta, err
	}
	content, err = sanitizeModelJSONContent(content)
	return content, usage, latencyMS, meta, err
}

func sanitizeModelJSONContent(content []byte) ([]byte, error) {
	text := strings.TrimSpace(string(content))
	if strings.HasPrefix(text, "<think>") {
		end := strings.LastIndex(text, "</think>")
		if end < 0 {
			return nil, fmt.Errorf("openai-compatible response reasoning block not closed")
		}
		text = text[end+len("</think>"):]
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) >= 3 && (strings.TrimSpace(lines[0]) == "```" || strings.TrimSpace(lines[0]) == "```json") && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			text = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("openai-compatible response contained only reasoning content")
	}
	return []byte(text), nil
}

func isTransientOpenAICompatibleUpstreamError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "openai-compatible status 400") {
		return false
	}
	return strings.Contains(msg, "upstream_error") ||
		strings.Contains(msg, "upstream 400 error") ||
		strings.Contains(msg, "missing `choices[0]`")
}

func chatCompletionOnce(ctx context.Context, client *http.Client, baseURL, apiKey, model, system, user string, jsonResponseFormat bool) (content []byte, usage OpenAIUsage, latencyMS int64, meta OpenAICompatibleResponseMeta, err error) {
	body := openAIChatRequest{
		Model:       model,
		Temperature: 0,
		Messages: []openAIChatMessage{
			{
				Role:    "system",
				Content: system,
			},
			{
				Role:    "user",
				Content: user,
			},
		},
	}
	if jsonResponseFormat {
		body.ResponseFormat = map[string]string{
			"type": "json_object",
		}
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, OpenAIUsage{}, 0, OpenAICompatibleResponseMeta{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsURL(baseURL), bytes.NewReader(rawBody))
	if err != nil {
		return nil, OpenAIUsage{}, 0, OpenAICompatibleResponseMeta{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	started := time.Now()
	resp, err := client.Do(httpReq)
	latencyMS = time.Since(started).Milliseconds()
	if err != nil {
		return nil, OpenAIUsage{}, latencyMS, OpenAICompatibleResponseMeta{}, err
	}
	defer resp.Body.Close()
	meta = openAICompatibleResponseMeta(resp.Header)
	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, OpenAIUsage{}, latencyMS, meta, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, OpenAIUsage{}, latencyMS, meta, fmt.Errorf("openai-compatible status %d: %s", resp.StatusCode, strings.TrimSpace(string(rawResp)))
	}
	var chat openAIChatResponse
	if err := json.Unmarshal(rawResp, &chat); err != nil {
		return nil, OpenAIUsage{}, latencyMS, meta, fmt.Errorf("parse openai-compatible response: %w", err)
	}
	if len(chat.Choices) == 0 || strings.TrimSpace(chat.Choices[0].Message.Content) == "" {
		return nil, chat.Usage, latencyMS, meta, fmt.Errorf("openai-compatible response missing choice content")
	}
	if meta.ResolvedModel == "" && strings.TrimSpace(chat.Model) != "" {
		meta.ResolvedModel = strings.TrimSpace(chat.Model)
		meta.ResolutionSource = "response_body"
	}
	return []byte(chat.Choices[0].Message.Content), chat.Usage, latencyMS, meta, nil
}

func openAICompatibleResponseMeta(header http.Header) OpenAICompatibleResponseMeta {
	model := parseSwitchbackSelectedModel(header.Get("x-switchback-route"))
	provider := ""
	if prefix, _, ok := strings.Cut(model, "/"); ok {
		provider = strings.TrimSpace(prefix)
	}
	source := ""
	if model != "" {
		source = "gateway_route"
	}
	return OpenAICompatibleResponseMeta{
		ResolvedProvider: provider,
		ResolvedModel:    model,
		ResolutionSource: source,
		GatewayRequestID: strings.TrimSpace(header.Get("x-switchback-request-id")),
	}
}

func parseSwitchbackSelectedModel(routeHeader string) string {
	for _, field := range strings.Fields(routeHeader) {
		value, ok := strings.CutPrefix(field, "selected=")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

func callOpenAICompatible(ctx context.Context, client *http.Client, baseURL, apiKey, model string, req LLMExtractionRequest) (LLMExtractionResponse, OpenAIUsage, int, OpenAICompatibleResponseMeta, error) {
	packet, err := json.MarshalIndent(req, "", " ")
	if err != nil {
		return LLMExtractionResponse{}, OpenAIUsage{}, 0, OpenAICompatibleResponseMeta{}, err
	}
	system := strings.Join([]string{
		"Return only JSON matching compound.distiller.llm_extraction.v0.1.",
		"Do not include promotion_basis or any authority-bearing field.",
		"You extract candidates only; Compound owns adjudication, promotion, writeback.",
	}, "\n")
	user := string(packet)
	content, usage, _, meta, err := ChatCompletionJSONWithMeta(ctx, client, baseURL, apiKey, model, system, user)
	liveModelCalls := 1
	if err != nil {
		return LLMExtractionResponse{}, usage, liveModelCalls, meta, err
	}
	extraction, err := decodeOpenAICompatibleExtraction(content, req.TraceRef)
	if err == nil {
		extraction, err = validateOpenAICompatibleExtraction(req, extraction)
	}
	if isCorrectableExtractionError(err) {
		correctiveSystem := system + "\n" + correctiveExtractionInstruction(err)
		var retryUsage OpenAIUsage
		content, retryUsage, _, meta, err = ChatCompletionJSONWithMeta(ctx, client, baseURL, apiKey, model, correctiveSystem, user)
		liveModelCalls++
		usage = addOpenAIUsage(usage, retryUsage)
		if err != nil {
			return LLMExtractionResponse{}, usage, liveModelCalls, meta, err
		}
		extraction, err = decodeOpenAICompatibleExtraction(content, req.TraceRef)
		if err == nil {
			extraction, err = validateOpenAICompatibleExtraction(req, extraction)
		}
	}
	if err != nil {
		return LLMExtractionResponse{}, usage, liveModelCalls, meta, err
	}
	return extraction, usage, liveModelCalls, meta, nil
}

func validateOpenAICompatibleExtraction(req LLMExtractionRequest, extraction LLMExtractionResponse) (LLMExtractionResponse, error) {
	normalized, err := json.Marshal(extraction)
	if err != nil {
		return LLMExtractionResponse{}, err
	}
	if _, err := NewGroundedLLMExtractor(req).Extract(normalized, DefaultPolicy()); err != nil {
		return LLMExtractionResponse{}, err
	}
	return extraction, nil
}

func isCorrectableExtractionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "attempted promotion_basis") {
		return false
	}
	for _, marker := range []string{
		"parse llm extraction content",
		"llm extraction schema_version",
		"llm extraction response missing trace_ref",
		"unknown kind",
		"memory requires",
		"missing evidence_refs",
		"evidence_ref",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func correctiveExtractionInstruction(err error) string {
	reason := strings.TrimSpace(err.Error())
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return strings.Join([]string{
		"Your previous extraction response was rejected by Compound validation: " + reason,
		"Retry once. Return only valid JSON with schema_version, trace_ref, source_refs, and memory candidates grounded in the provided evidence_refs.",
		"Candidate kind must be \"memory\". Each memory candidate must include entity, predicate, object, confidence, and evidence_refs.",
	}, "\n")
}

func addOpenAIUsage(a, b OpenAIUsage) OpenAIUsage {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.TotalTokens += b.TotalTokens
	return a
}

func decodeOpenAICompatibleExtraction(content []byte, requestTraceRef string) (LLMExtractionResponse, error) {
	content = stripJSONFence(content)
	var extraction LLMExtractionResponse
	if err := json.Unmarshal(content, &extraction); err != nil {
		return LLMExtractionResponse{}, fmt.Errorf("parse llm extraction content: %w", err)
	}
	var repairs []string
	if strings.TrimSpace(extraction.SchemaVersion) == "" {
		extraction.SchemaVersion = LLMExtractionSchemaVersion
		repairs = append(repairs, "repaired_missing_schema_version")
	}
	if extraction.SchemaVersion != LLMExtractionSchemaVersion {
		return LLMExtractionResponse{}, fmt.Errorf("llm extraction schema_version=%q, want %q", extraction.SchemaVersion, LLMExtractionSchemaVersion)
	}
	if strings.TrimSpace(extraction.TraceRef) == "" && strings.TrimSpace(requestTraceRef) != "" {
		extraction.TraceRef = strings.TrimSpace(requestTraceRef)
		repairs = append(repairs, "repaired_missing_trace_ref")
	}
	for i := range extraction.Candidates {
		if strings.TrimSpace(extraction.Candidates[i].Kind) == "memory_candidate" {
			extraction.Candidates[i].Kind = KindMemory
			repairs = append(repairs, "normalized_kind_memory_candidate")
		}
	}
	if len(repairs) > 0 {
		extraction.Warnings = append(extraction.Warnings, repairs...)
	}
	return extraction, nil
}

func stripJSONFence(data []byte) []byte {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return data
	}
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 3 && isJSONFenceOpener(lines[0]) && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			return []byte(strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n")))
		}
	}
	if strings.HasSuffix(trimmed, "```") {
		withoutFence := strings.TrimSpace(strings.TrimSuffix(trimmed, "```"))
		if strings.HasPrefix(withoutFence, "{") {
			return []byte(withoutFence)
		}
	}
	return data
}

func isJSONFenceOpener(line string) bool {
	opener := strings.TrimSpace(line)
	if opener == "```" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(opener, "```")), "json")
}

func chatCompletionsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}
