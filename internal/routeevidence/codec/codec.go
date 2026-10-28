// Package routeevidencecodec owns the leaf JSON contract for execution route
// evidence. Higher-level route recording and projection packages re-export this
// contract; leaf consumers can validate rows without importing those organs.
package routeevidencecodec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const SchemaVersion = "compound.execution_route_evidence.v0.1"

const (
	RecorderGeneric             = "generic"
	RecorderExternalImport      = "external_import"
	RecorderTrustedPanelRuntime = "trusted_panel_runtime"
)

type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Protocol string `json:"protocol"`
}

type RejectedTarget struct {
	Target Target `json:"target"`
	Reason string `json:"reason"`
}

type Evidence struct {
	SchemaVersion    string           `json:"schema_version"`
	ID               string           `json:"id"`
	CreatedAt        string           `json:"created_at"`
	RequestID        string           `json:"request_id"`
	WorkID           string           `json:"work_id,omitempty"`
	TransitionID     string           `json:"transition_id,omitempty"`
	ClientProfile    string           `json:"client_profile"`
	Protocol         string           `json:"protocol"`
	SelectedTarget   Target           `json:"selected_target"`
	ResolvedProvider string           `json:"resolved_provider,omitempty"`
	ResolvedModel    string           `json:"resolved_model,omitempty"`
	ResolutionSource string           `json:"resolution_source,omitempty"`
	GatewayRequestID string           `json:"gateway_request_id,omitempty"`
	Fallbacks        []Target         `json:"fallbacks,omitempty"`
	Rejected         []RejectedTarget `json:"rejected,omitempty"`
	Usage            Usage            `json:"usage,omitempty"`
	CostMicros       int64            `json:"cost_micros,omitempty"`
	LatencyMS        int64            `json:"latency_ms,omitempty"`
	Status           string           `json:"status"`
	EvidenceRefs     []string         `json:"evidence_refs,omitempty"`
	RecordedVia      string           `json:"recorded_via"`
}

func Decode(data []byte) (Evidence, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var ev Evidence
	if err := dec.Decode(&ev); err != nil {
		return Evidence{}, fmt.Errorf("decode execution route evidence: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Evidence{}, fmt.Errorf("decode execution route evidence: trailing JSON")
		}
		return Evidence{}, fmt.Errorf("decode execution route evidence trailing JSON: %w", err)
	}
	if err := Validate(ev); err != nil {
		return Evidence{}, err
	}
	return ev, nil
}

// Encode validates and emits the canonical compact JSON form used by the
// governed route-evidence writer.
func Encode(ev Evidence) ([]byte, error) {
	if err := Validate(ev); err != nil {
		return nil, err
	}
	return json.Marshal(&ev)
}

func Validate(ev Evidence) error {
	var errs []string
	req := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, name+" required")
		}
	}
	req("schema_version", ev.SchemaVersion)
	req("id", ev.ID)
	req("created_at", ev.CreatedAt)
	req("request_id", ev.RequestID)
	req("client_profile", ev.ClientProfile)
	req("protocol", ev.Protocol)
	req("selected_target.provider", ev.SelectedTarget.Provider)
	req("selected_target.model", ev.SelectedTarget.Model)
	req("selected_target.protocol", ev.SelectedTarget.Protocol)
	req("status", ev.Status)
	switch ev.RecordedVia {
	case "", RecorderGeneric, RecorderExternalImport, RecorderTrustedPanelRuntime:
	default:
		errs = append(errs, fmt.Sprintf("recorded_via=%q is not recognized", ev.RecordedVia))
	}
	if ev.ResolutionSource != "" && ev.ResolutionSource != "gateway_route" && ev.ResolutionSource != "response_body" {
		errs = append(errs, fmt.Sprintf("resolution_source=%q, want gateway_route or response_body", ev.ResolutionSource))
	}
	if ev.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version=%q, want %q", ev.SchemaVersion, SchemaVersion))
	}
	if ev.CreatedAt != "" {
		if _, err := time.Parse(time.RFC3339, ev.CreatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("created_at invalid: %v", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("execution route evidence invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}
