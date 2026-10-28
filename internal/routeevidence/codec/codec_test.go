package routeevidencecodec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func validEvidence() Evidence {
	return Evidence{
		SchemaVersion: SchemaVersion,
		ID:            "rev_test123",
		CreatedAt:     "2026-07-23T12:00:00Z",
		RequestID:     "req_abc123",
		ClientProfile: "claude-code",
		Protocol:      "anthropic",
		SelectedTarget: Target{
			Provider: "anthropic",
			Model:    "claude-sonnet-5",
			Protocol: "anthropic",
		},
		Status:      "ok",
		RecordedVia: RecorderGeneric,
	}
}

func TestValidateValidEvidence(t *testing.T) {
	if err := Validate(validEvidence()); err != nil {
		t.Fatalf("Validate(valid) = %v, want nil", err)
	}
}

func TestValidateRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Evidence)
		wantErr string
	}{
		{"schema_version missing", func(e *Evidence) { e.SchemaVersion = "" }, "schema_version required"},
		{"id missing", func(e *Evidence) { e.ID = "" }, "id required"},
		{"created_at missing", func(e *Evidence) { e.CreatedAt = "" }, "created_at required"},
		{"request_id missing", func(e *Evidence) { e.RequestID = "" }, "request_id required"},
		{"client_profile missing", func(e *Evidence) { e.ClientProfile = "" }, "client_profile required"},
		{"protocol missing", func(e *Evidence) { e.Protocol = "" }, "protocol required"},
		{"selected_target.provider missing", func(e *Evidence) { e.SelectedTarget.Provider = "" }, "selected_target.provider required"},
		{"selected_target.model missing", func(e *Evidence) { e.SelectedTarget.Model = "" }, "selected_target.model required"},
		{"selected_target.protocol missing", func(e *Evidence) { e.SelectedTarget.Protocol = "" }, "selected_target.protocol required"},
		{"status missing", func(e *Evidence) { e.Status = "" }, "status required"},
		{"whitespace-only id treated as missing", func(e *Evidence) { e.ID = "   " }, "id required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := validEvidence()
			tc.mutate(&ev)
			err := Validate(ev)
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateRecordedVia(t *testing.T) {
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"", false},
		{RecorderGeneric, false},
		{RecorderExternalImport, false},
		{RecorderTrustedPanelRuntime, false},
		{"bogus_recorder", true},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			ev := validEvidence()
			ev.RecordedVia = tc.value
			err := Validate(ev)
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() with recorded_via=%q = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() with recorded_via=%q = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestValidateResolutionSource(t *testing.T) {
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"", false},
		{"gateway_route", false},
		{"response_body", false},
		{"made_up_source", true},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			ev := validEvidence()
			ev.ResolutionSource = tc.value
			err := Validate(ev)
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() with resolution_source=%q = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() with resolution_source=%q = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestValidateSchemaVersionMismatch(t *testing.T) {
	ev := validEvidence()
	ev.SchemaVersion = "compound.execution_route_evidence.v9.9"
	err := Validate(ev)
	if err == nil {
		t.Fatal("expected error for mismatched schema_version")
	}
	if !strings.Contains(err.Error(), "schema_version=") {
		t.Fatalf("error %q does not name the schema_version mismatch", err.Error())
	}
}

func TestValidateCreatedAtMustBeRFC3339(t *testing.T) {
	ev := validEvidence()
	ev.CreatedAt = "not-a-timestamp"
	err := Validate(ev)
	if err == nil {
		t.Fatal("expected error for non-RFC3339 created_at")
	}
	if !strings.Contains(err.Error(), "created_at invalid") {
		t.Fatalf("error %q does not name the created_at problem", err.Error())
	}
}

func TestValidateAccumulatesMultipleErrors(t *testing.T) {
	ev := validEvidence()
	ev.ID = ""
	ev.Status = ""
	ev.RecordedVia = "bogus"
	err := Validate(ev)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"id required", "status required", "recorded_via=\"bogus\""} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Validate() = %q, want it to contain %q", msg, want)
		}
	}
}

func TestEncodeRejectsInvalidEvidence(t *testing.T) {
	ev := validEvidence()
	ev.RequestID = ""
	if _, err := Encode(ev); err == nil {
		t.Fatal("expected Encode to reject invalid evidence")
	}
}

func TestEncodeProducesValidJSON(t *testing.T) {
	ev := validEvidence()
	ev.WorkID = "work_abc123"
	ev.Usage = Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	ev.CostMicros = 4200
	ev.LatencyMS = 850

	data, err := Encode(ev)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("Encode output is not valid JSON: %v", err)
	}
	if round["work_id"] != "work_abc123" {
		t.Fatalf("encoded work_id = %v, want work_abc123", round["work_id"])
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	ev := validEvidence()
	ev.WorkID = "work_roundtrip"
	ev.TransitionID = "tr_1"
	ev.Fallbacks = []Target{{Provider: "openai", Model: "gpt-5", Protocol: "openai"}}
	ev.Rejected = []RejectedTarget{{Target: Target{Provider: "openai", Model: "gpt-5", Protocol: "openai"}, Reason: "rate_limited"}}
	ev.Usage = Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	ev.EvidenceRefs = []string{"evidence:one", "evidence:two"}

	data, err := Encode(ev)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode(Encode(ev)): %v", err)
	}
	if !reflect.DeepEqual(got, ev) {
		t.Fatalf("round trip mismatch:\n got  = %+v\n want = %+v", got, ev)
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Fatal("expected error decoding malformed JSON")
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	raw := []byte(`{
		"schema_version": "` + SchemaVersion + `",
		"id": "rev_1",
		"created_at": "2026-07-23T12:00:00Z",
		"request_id": "req_1",
		"client_profile": "claude-code",
		"protocol": "anthropic",
		"selected_target": {"provider": "anthropic", "model": "claude-sonnet-5", "protocol": "anthropic"},
		"status": "ok",
		"unexpected_field": "boom"
	}`)
	if _, err := Decode(raw); err == nil {
		t.Fatal("expected error decoding JSON with an unknown field")
	}
}

func TestDecodeRejectsTrailingJSON(t *testing.T) {
	ev := validEvidence()
	data, err := Encode(ev)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	trailing := append(append([]byte{}, data...), []byte(`{"extra":true}`)...)
	_, err = Decode(trailing)
	if err == nil {
		t.Fatal("expected error decoding JSON with trailing data")
	}
	if !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("error %q does not name the trailing JSON problem", err.Error())
	}
}

func TestDecodeRejectsSemanticallyInvalidEvidence(t *testing.T) {
	raw := []byte(`{
		"schema_version": "` + SchemaVersion + `",
		"id": "rev_1",
		"created_at": "2026-07-23T12:00:00Z",
		"client_profile": "claude-code",
		"protocol": "anthropic",
		"selected_target": {"provider": "anthropic", "model": "claude-sonnet-5", "protocol": "anthropic"},
		"status": "ok"
	}`)
	// request_id is missing: structurally valid JSON, semantically invalid Evidence.
	_, err := Decode(raw)
	if err == nil {
		t.Fatal("expected Decode to reject an Evidence missing request_id")
	}
	if !strings.Contains(err.Error(), "request_id required") {
		t.Fatalf("error %q does not name the missing request_id", err.Error())
	}
}
