package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

const promptInjectionActionTraceSchema = "compound.prompt_injection_action_traces.v0.1"

type promptInjectionActionCorpus struct {
	SchemaVersion string                       `json:"schema_version"`
	Traces        []promptInjectionActionTrace `json:"traces"`
}

type promptInjectionActionTrace struct {
	ID                    string                      `json:"id"`
	Tool                  string                      `json:"tool"`
	Arguments             map[string]any              `json:"arguments"`
	SchemaRevision        string                      `json:"schema_revision"`
	Source                *promptInjectionTraceSource `json:"source,omitempty"`
	ClassifierRisk        string                      `json:"classifier_risk,omitempty"`
	ExpectedDomain        string                      `json:"expected_domain"`
	ExpectedAllowed       bool                        `json:"expected_allowed"`
	ExpectedSecurityFloor string                      `json:"expected_security_floor,omitempty"`
}

type promptInjectionTraceSource struct {
	OriginKind      string   `json:"origin_kind"`
	Content         string   `json:"content"`
	Trust           string   `json:"trust"`
	Confidentiality string   `json:"confidentiality"`
	AllowedSinks    []string `json:"allowed_sinks"`
}

func TestPromptInjectionActionTraceCorpus(t *testing.T) {
	path := filepath.Join("..", "..", "fixtures", "security", "prompt-injection-action-traces.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prompt-injection corpus: %v", err)
	}

	var corpus promptInjectionActionCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse prompt-injection corpus: %v", err)
	}
	if corpus.SchemaVersion != promptInjectionActionTraceSchema {
		t.Fatalf("schema_version = %q, want %q", corpus.SchemaVersion, promptInjectionActionTraceSchema)
	}
	if len(corpus.Traces) == 0 {
		t.Fatal("prompt-injection corpus has no traces")
	}

	seen := map[string]struct{}{}
	for i, trace := range corpus.Traces {
		if strings.TrimSpace(trace.ID) == "" {
			t.Fatalf("trace[%d] has no id", i)
		}
		if _, exists := seen[trace.ID]; exists {
			t.Fatalf("duplicate trace id %q", trace.ID)
		}
		seen[trace.ID] = struct{}{}

		t.Run(trace.ID, func(t *testing.T) {
			request := ToolActionRequest{
				WorkID:             "work_prompt_injection_corpus",
				Actor:              "codex:corpus",
				Harness:            "codex",
				Tool:               trace.Tool,
				Arguments:          trace.Arguments,
				ToolSchemaRevision: trace.SchemaRevision,
				ClassifierRisk:     trace.ClassifierRisk,
			}
			if trace.Source != nil {
				request.Sources = []envelope.SourceEnvelope{
					envelope.NewSourceEnvelope(
						promptInjectionTraceHeader("src_"+trace.ID),
						envelope.SourceOrigin{
							Kind:    trace.Source.OriginKind,
							Locator: "fixture:" + trace.ID,
						},
						[]byte(trace.Source.Content),
						trace.Source.Trust,
						trace.Source.Confidentiality,
						trace.Source.AllowedSinks,
					),
				}
			}

			monitored := MonitorToolAction(
				request,
				DefaultConfig(),
				time.Date(2026, 7, 26, 16, 0, 0, 0, time.UTC),
			)
			if monitored.Classification.Domain != trace.ExpectedDomain {
				t.Fatalf(
					"domain = %q, want %q; classification=%+v",
					monitored.Classification.Domain,
					trace.ExpectedDomain,
					monitored.Classification,
				)
			}
			if allowed := Allowed(monitored.Decision); allowed != trace.ExpectedAllowed {
				t.Fatalf(
					"allowed = %v, want %v; outcome=%q reasons=%v",
					allowed,
					trace.ExpectedAllowed,
					monitored.Decision.Outcome,
					monitored.Decision.Reasons,
				)
			}
			if monitored.Intent.SecurityFloorOutcome != trace.ExpectedSecurityFloor {
				t.Fatalf(
					"security floor = %q, want %q; reasons=%v",
					monitored.Intent.SecurityFloorOutcome,
					trace.ExpectedSecurityFloor,
					monitored.Intent.SecurityReasons,
				)
			}
		})
	}
}

func promptInjectionTraceHeader(id string) envelope.Header {
	return envelope.Header{
		SchemaVersion: envelope.SVSourceEnvelope,
		ID:            id,
		CreatedAt:     "2026-07-26T16:00:00Z",
		Producer:      envelope.Producer{Kind: "test", Name: "compound"},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       "private",
		Risk:          "tier1",
		EvidenceRefs:  []string{"fixture:prompt-injection-action-traces"},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}
