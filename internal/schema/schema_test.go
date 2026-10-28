package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/knowledge"
	"compound/internal/product"
)

func TestExportContextPackSchemaIncludesCapabilityOffers(t *testing.T) {
	doc, err := Export("context-pack")
	if err != nil {
		t.Fatal(err)
	}
	props := doc["properties"].(map[string]any)
	if _, ok := props["capability_offers"]; !ok {
		t.Fatalf("context_pack schema missing capability_offers: %+v", props)
	}
	if _, ok := props["active_memory"]; !ok {
		t.Fatalf("context_pack schema missing active_memory: %+v", props)
	}
	if _, ok := props["active_hooks"]; !ok {
		t.Fatalf("context_pack schema missing active_hooks: %+v", props)
	}
	if doc["$id"] != "compound://schemas/context_pack" {
		t.Fatalf("schema id = %v", doc["$id"])
	}
}

func TestValidateContextPackUsesEnvelopeSemantics(t *testing.T) {
	path := writeSchemaFixture(t, "context-pack.json", validContextPack(t))
	if err := Validate("context_pack", path); err != nil {
		t.Fatal(err)
	}

	var bad map[string]any
	if err := json.Unmarshal(validContextPack(t), &bad); err != nil {
		t.Fatal(err)
	}
	delete(bad, "excluded")
	data, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	badPath := writeSchemaFixture(t, "bad-context-pack.json", data)
	if err := Validate("context_pack", badPath); err == nil || !strings.Contains(err.Error(), "excluded") {
		t.Fatalf("expected excluded validation error, got %v", err)
	}
}

func TestKnowledgeExtractionRequestContractValidatesStateEnum(t *testing.T) {
	doc, err := Export("knowledge_extraction_request")
	if err != nil {
		t.Fatal(err)
	}
	if doc["$id"] != "compound://schemas/knowledge_extraction_request" {
		t.Fatalf("schema id = %v", doc["$id"])
	}

	req := knowledge.ExtractionRequest{
		SchemaVersion: knowledge.ExtractionRequestSchemaVersion,
		ID:            "kx_schema",
		ItemRef:       "arxiv:2506.12345v1",
		EvidenceRef:   "ev_schema",
		SourceKind:    "arxiv",
		ContentDigest: "digest",
		State:         "pending",
		CreatedAt:     "2026-07-02T00:00:00Z",
		CandidateIDs:  []string{},
		RouteEvidence: nil,
		CompletedAt:   "",
		Title:         "Schema test",
		Error:         "",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	path := writeSchemaFixture(t, "knowledge-extraction-request.json", data)
	if err := Validate("knowledge-extraction-request", path); err != nil {
		t.Fatal(err)
	}

	req.State = "stuck"
	badData, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	badPath := writeSchemaFixture(t, "bad-knowledge-extraction-request.json", badData)
	if err := Validate("knowledge_extraction_request", badPath); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("expected state validation error, got %v", err)
	}
}

func TestProductSignalExportContractValidatesSourceExport(t *testing.T) {
	doc, err := Export("product_signal_export")
	if err != nil {
		t.Fatal(err)
	}
	if doc["$id"] != "compound://schemas/product_signal_export" {
		t.Fatalf("schema id = %v", doc["$id"])
	}
	if err := Validate("product_signal_export", "../../fixtures/product/source-export.gsc.yaml"); err != nil {
		t.Fatal(err)
	}

	badPath := writeSchemaFixture(t, "bad-product-signal-export.yaml", []byte(`schema_version: compound.product_signal_export.v0.1
source:
  kind: spreadsheet
  ref: fixture:spreadsheet
product_id: product:example-site
surface: provider-ingress
change_event: Provider metric snapshot observed.
metric:
  name: clicks
  before: {value: 1, ref: fixture:before}
  after: {value: 1, ref: fixture:after}
window: neutral snapshot
confidence: 0.5
evidence_refs: [fixture:bad-export]
`))
	if err := Validate("product_signal_export", badPath); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported provider validation error, got %v", err)
	}
}

func TestValidateCandidateSetRejectsUnknownKind(t *testing.T) {
	path := writeSchemaFixture(t, "candidate-set.json", []byte(`{
		"trace_ref": "trace_bad",
		"candidates": [{
			"kind": "unknown_kind",
			"subject": "x",
			"statement": "y",
			"confidence": 0.9,
			"derivation": "test",
			"evidence_refs": ["ev_1"]
		}]
	}`))
	if err := Validate("candidate_set", path); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("expected unknown kind validation error, got %v", err)
	}
}

func validContextPack(t *testing.T) []byte {
	t.Helper()
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            "ctx_schema_test",
			CreatedAt:     time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound", Version: "test"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD: "schema test",
		Included: []envelope.IncludedRef{{
			Ref:         "capability://skill:test",
			SourceClass: "generated",
			Reason:      "test offer",
			Confidence:  1,
			Status:      "current",
		}},
		Excluded: []envelope.ExcludedRef{},
		CapabilityOffers: []envelope.CapabilityOffer{{
			Ref:          "capability://skill:test",
			Kind:         "skill",
			Name:         "Test Skill",
			Score:        1,
			Why:          "test",
			CostRiskNote: "offer only; execution still requires WorkEnvelope, ActionIntent, and PolicyDecision",
		}},
		ActiveInstructions: []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       []string{"claim://claim_test"},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	data, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeSchemaFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProductLoopsContractValidatesReadModel(t *testing.T) {
	doc, err := Export("product_loops")
	if err != nil {
		t.Fatal(err)
	}
	if doc["$id"] != "compound://schemas/product_loops" {
		t.Fatalf("schema id = %v", doc["$id"])
	}
	model := product.LoopModel{
		SchemaVersion: product.LoopModelSchemaVersion,
		GeneratedAt:   "2026-07-02T12:00:00Z",
		Products: []product.ProductLoop{{
			ProductID: "product:schema-loop",
			Name:      "product:schema-loop",
			SignalsByMetric: map[string][]product.LoopSignal{
				"gsc_clicks": {{
					SignalID:   "sig_schema_gsc",
					Provider:   "google_search_console",
					Metric:     "gsc_clicks",
					Before:     1,
					After:      2,
					ObservedAt: "2026-07-02T11:00:00Z",
				}},
			},
			OpenSourceIssues: product.LoopSourceIssues{},
			Freshness: product.LoopFreshness{
				LatestSignalAt: "2026-07-02T11:00:00Z",
				Stale:          false,
			},
		}},
	}
	data, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	path := writeSchemaFixture(t, "product-loops.json", data)
	if err := Validate("product-loops", path); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeExportContractValidatesFixture(t *testing.T) {
	if _, err := Export("knowledge_export"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "fixtures", "knowledge", "arxiv-export.json")
	if err := Validate("knowledge_export", path); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptSchemaDistinguishesUnknownFromKnownEffects(t *testing.T) {
	schema, err := Export("receipt")
	if err != nil {
		t.Fatal(err)
	}
	if len(schema["allOf"].([]any)) != 1 {
		t.Fatal("schema lacks completed/no-op known-effect condition")
	}
	props := schema["properties"].(map[string]any)
	delta := props["state_delta"].(map[string]any)
	branches := delta["oneOf"].([]any)
	if len(branches) != 2 {
		t.Fatal("missing known/unknown structural alternatives")
	}
	unknown := branches[1].(map[string]any)
	forbidden := unknown["not"].(map[string]any)["required"].([]string)
	if len(forbidden) != 1 || forbidden[0] != "changed" {
		t.Fatal("schema permits a boolean assertion for unknown effects")
	}
}
