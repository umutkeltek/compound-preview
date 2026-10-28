package distiller

import (
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/reviewartifact"
)

func TestLLMExtractionRequestPathResolvesRecordedRefsSafely(t *testing.T) {
	homePath := t.TempDir()
	path, err := LLMExtractionRequestPath(homePath, "distiller_request:llmreq_abc123")
	if err != nil {
		t.Fatalf("resolve recorded request ref: %v", err)
	}
	want := filepath.Join(homePath, "reviews", "distiller-requests", "llmreq_abc123.json")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	if _, err := LLMExtractionRequestPath(homePath, "distiller_request:../llmreq_escape"); err == nil || !strings.Contains(err.Error(), "invalid distiller request record id") {
		t.Fatalf("unsafe request ref error = %v", err)
	}

	plain := filepath.Join(homePath, "request.json")
	path, err = LLMExtractionRequestPath(homePath, plain)
	if err != nil {
		t.Fatalf("plain request path: %v", err)
	}
	if path != plain {
		t.Fatalf("plain path = %q, want %q", path, plain)
	}
}

func TestAnnotateRequestQueueMarksReplayPacketsRecorded(t *testing.T) {
	homePath := t.TempDir()
	req := LLMExtractionRequest{
		SchemaVersion: LLMExtractionSchemaVersion,
		TraceRef:      "trace_queue_record",
		SourceRefs:    []string{"raw_trace:trace_queue_record"},
		Instructions:  []string{"fixture request"},
	}
	record, err := RecordLLMExtractionRequest(homePath, req)
	if err != nil {
		t.Fatalf("record request: %v", err)
	}

	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: "distiller-requests"}, t0)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	AnnotateRequestQueue(&queue)
	reviewartifact.Recount(&queue)

	if queue.Counts.Total != 1 || queue.Counts.OK != 1 || queue.Counts.ReviewRequired != 0 {
		t.Fatalf("queue counts = %+v, want recorded replay packet ok", queue.Counts)
	}
	item := queue.Items[0]
	if item.ID != record.ID || item.Status != "recorded" || item.Readiness != "replayable_request_recorded" {
		t.Fatalf("queue item = %+v, want recorded replay packet", item)
	}
	if item.NextAction == "" || !stringSliceContains(item.EvidenceRefs, "raw_trace:trace_queue_record") {
		t.Fatalf("queue item missing replay metadata: %+v", item)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
