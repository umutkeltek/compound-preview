package distiller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/reviewartifact"
)

const LLMRequestRecordRefPrefix = "distiller_request:"
const LLMRequestRecordArea = "distiller-requests"

type LLMRequestRecord struct {
	ID   string
	Ref  string
	Path string
}

// RecordLLMExtractionRequest persists an exact replay packet under COMPOUND_HOME.
// The artifact is still only adapter input: it does not call a model, write
// candidates, adjudicate truth, or mutate distiller policy.
func RecordLLMExtractionRequest(homePath string, req LLMExtractionRequest) (LLMRequestRecord, error) {
	if strings.TrimSpace(homePath) == "" {
		return LLMRequestRecord{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(req.TraceRef) == "" {
		return LLMRequestRecord{}, fmt.Errorf("trace_ref is required")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return LLMRequestRecord{}, err
	}
	id := reviewartifact.ID("llmreq_", req.TraceRef, string(data))
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return LLMRequestRecord{}, err
	}
	card, err := OperatorCardForLLMRequest(req)
	if err != nil {
		return LLMRequestRecord{}, err
	}
	reviewartifact.PutOperatorCard(payload, card)
	path, err := reviewartifact.WriteJSON(homePath, LLMRequestRecordArea, id+".json", payload, " ")
	if err != nil {
		return LLMRequestRecord{}, err
	}
	return LLMRequestRecord{
		ID:   id,
		Ref:  LLMRequestRecordRefPrefix + id,
		Path: path,
	}, nil
}

func AnnotateRequestQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != LLMRequestRecordArea {
			continue
		}
		annotateRequestQueueItem(item)
	}
}

func annotateRequestQueueItem(item *reviewartifact.QueueItem) {
	if strings.TrimSpace(item.JSONPath) == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "distiller request record path missing"
		item.NextAction = "regenerate distiller request --record"
		return
	}
	req, err := LoadLLMExtractionRequest("", item.JSONPath)
	if err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = err.Error()
		item.NextAction = "regenerate distiller request --record"
		return
	}
	item.Status = "recorded"
	item.Readiness = "replayable_request_recorded"
	item.BlockedReason = ""
	item.SourceKind = "distiller-llm-extraction-request"
	item.EvidenceRefs = reviewartifact.Unique(append(item.EvidenceRefs, req.SourceRefs...))
	item.NextAction = "distiller request replay packet recorded; use its distiller_request ref with distiller run/extract"
	if card, err := OperatorCardForLLMRequest(req); err == nil {
		_ = reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, reviewartifact.BandFiled)
	} else {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "distiller request operator card invalid: "+err.Error())
	}
}

func LoadLLMExtractionRequest(homePath, selector string) (LLMExtractionRequest, error) {
	path, err := LLMExtractionRequestPath(homePath, selector)
	if err != nil {
		return LLMExtractionRequest{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return LLMExtractionRequest{}, err
	}
	var req LLMExtractionRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return LLMExtractionRequest{}, err
	}
	if req.SchemaVersion != LLMExtractionSchemaVersion {
		return LLMExtractionRequest{}, fmt.Errorf("schema_version=%q, want %q", req.SchemaVersion, LLMExtractionSchemaVersion)
	}
	return req, nil
}

func LLMExtractionRequestPath(homePath, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("request selector required")
	}
	if !strings.HasPrefix(selector, LLMRequestRecordRefPrefix) {
		return selector, nil
	}
	if strings.TrimSpace(homePath) == "" {
		return "", fmt.Errorf("COMPOUND_HOME required to resolve %s refs", LLMRequestRecordRefPrefix)
	}
	id := strings.TrimSpace(strings.TrimPrefix(selector, LLMRequestRecordRefPrefix))
	if err := validateLLMRequestRecordID(id); err != nil {
		return "", err
	}
	return filepath.Join(homePath, "reviews", LLMRequestRecordArea, id+".json"), nil
}

func validateLLMRequestRecordID(id string) error {
	if id == "" || id == "." || id == ".." || id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid distiller request record id %q", id)
	}
	if !strings.HasPrefix(id, "llmreq_") {
		return fmt.Errorf("invalid distiller request record id %q", id)
	}
	return nil
}
