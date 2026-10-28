package reviewartifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func decisionBriefFixture() map[string]any {
	return map[string]any{
		"schema_version": DecisionBriefSchemaVersion,
		"headline":       "Choose the bounded next move",
		"what_this_is":   "A prepared translation of the underlying review artifact.",
		"findings": []any{
			map[string]any{
				"finding":      "The proposed change has no wired action.",
				"evidence_ref": "compound://evidence/review-fixture",
			},
		},
		"options": []any{
			map[string]any{
				"value":               "approve",
				"label":               "Approve the direction",
				"consequence_approve": "A separate governed action may move the work forward.",
				"consequence_hold":    "The review remains open without mutation.",
				"command_argv":        nil,
			},
		},
		"recommendation": map[string]any{
			"option":     "approve",
			"confidence": 0.82,
			"rationale":  "The cited evidence supports the bounded direction.",
		},
		"prepared_by":  "switchback_api",
		"prepared_at":  "2026-07-10T04:00:00Z",
		"propose_only": true,
	}
}

func writeDecisionBriefCore(t *testing.T, homePath, area, id, status, band string) {
	t.Helper()
	_, err := Write(homePath, area, id, map[string]any{
		"id":               id,
		"status":           status,
		"band":             band,
		"created_at":       "2026-07-10T03:00:00Z",
		"evidence_refs":    []string{"compound://evidence/" + id},
		"operator_summary": "Review " + id + ".",
		"context_label":    "Decision brief fixture",
		"group_key":        area + "|fixture",
	}, "  ", "# Review\n")
	if err != nil {
		t.Fatal(err)
	}
}

func writeDecisionBriefSidecar(t *testing.T, homePath, area, id string, payload map[string]any) {
	t.Helper()
	if _, err := WriteJSON(homePath, area, id+".brief.json", payload, "  "); err != nil {
		t.Fatal(err)
	}
}

func decisionBriefItemMap(t *testing.T, item QueueItem) map[string]any {
	t.Helper()
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQueueRejectsDecisionBriefWithoutRequiredEvidenceOrProvenance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "finding evidence ref",
			mutate: func(payload map[string]any) {
				payload["findings"] = []any{map[string]any{"finding": "Unsupported finding"}}
			},
			want: "evidence_ref",
		},
		{
			name: "prepared by",
			mutate: func(payload map[string]any) {
				payload["prepared_by"] = ""
			},
			want: "prepared_by",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			const area = "decision-brief-fixtures"
			const id = "brief_required_fields"
			writeDecisionBriefCore(t, homePath, area, id, "review_required", BandNeedsYou)
			payload := decisionBriefFixture()
			tt.mutate(payload)
			writeDecisionBriefSidecar(t, homePath, area, id, payload)

			report, err := Queue(homePath, QueueOptions{Area: area}, time.Date(2026, 7, 10, 5, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if report.Counts.Total != 1 || len(report.Items) != 1 {
				t.Fatalf("decision brief must be a sidecar, counts=%+v items=%d", report.Counts, len(report.Items))
			}
			item := report.Items[0]
			projection := decisionBriefItemMap(t, item)
			if _, ok := projection["brief"]; ok {
				t.Fatalf("invalid brief must not attach: %+v", projection["brief"])
			}
			warnings := strings.Join(stringSlice(projection["warnings"]), "\n")
			if !strings.Contains(warnings, tt.want) {
				t.Fatalf("warning %q missing from %q", tt.want, warnings)
			}
			if item.Status != "review_required" || item.Band != BandNeedsYou || item.OperatorSummary != "Review "+id+"." {
				t.Fatalf("invalid brief changed card state: %+v", item)
			}
		})
	}
}

func TestQueueDecisionBriefCannotFlipArtifactBandOrStatus(t *testing.T) {
	homePath := t.TempDir()
	const area = "decision-brief-fixtures"
	const id = "brief_propose_only"
	writeDecisionBriefCore(t, homePath, area, id, "held", BandWatching)
	payload := decisionBriefFixture()
	payload["status"] = "recorded"
	payload["band"] = BandFiled
	writeDecisionBriefSidecar(t, homePath, area, id, payload)

	report, err := Queue(homePath, QueueOptions{Area: area}, time.Date(2026, 7, 10, 5, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Total != 1 || len(report.Items) != 1 {
		t.Fatalf("state-injecting brief must remain one sidecar, counts=%+v items=%d", report.Counts, len(report.Items))
	}
	item := report.Items[0]
	projection := decisionBriefItemMap(t, item)
	if item.Status != "held" || item.Band != BandWatching {
		t.Fatalf("brief flipped status/band: status=%q band=%q", item.Status, item.Band)
	}
	if _, ok := projection["brief"]; ok {
		t.Fatalf("brief with status/band injection must be rejected: %+v", projection["brief"])
	}
	warnings := strings.Join(stringSlice(projection["warnings"]), "\n")
	if !strings.Contains(warnings, "unknown field") {
		t.Fatalf("state-injecting brief should warn about unknown fields: %q", warnings)
	}
}

func TestQueueArchivesDecisionBriefAsSidecarWithoutCountingIt(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	const area = "decision-brief-fixtures"
	const id = "brief_archive"
	writeDecisionBriefCore(t, homePath, area, id, "recorded", BandFiled)
	writeDecisionBriefSidecar(t, homePath, area, id, decisionBriefFixture())

	active, err := Queue(homePath, QueueOptions{Area: area}, time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if active.Counts.Total != 1 || len(active.Items) != 1 {
		t.Fatalf("active brief sidecar counted as review: counts=%+v items=%d", active.Counts, len(active.Items))
	}
	activeProjection := decisionBriefItemMap(t, active.Items[0])
	brief, ok := activeProjection["brief"].(map[string]any)
	if !ok || brief["schema_version"] != DecisionBriefSchemaVersion || brief["prepared_by"] != "switchback_api" {
		t.Fatalf("valid brief not attached: %+v", activeProjection["brief"])
	}
	if active.Items[0].Status != "recorded" || active.Items[0].Band != BandFiled {
		t.Fatalf("valid brief changed terminal card: %+v", active.Items[0])
	}

	if _, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"review"}}, time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "reviews", area, id+".brief.json")); !os.IsNotExist(err) {
		t.Fatalf("brief sidecar should move with archived parent, stat err=%v", err)
	}

	archived, err := Queue(homePath, QueueOptions{Area: area, IncludeArchived: true}, time.Date(2026, 7, 10, 5, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if archived.Counts.Total != 1 || len(archived.Items) != 1 {
		t.Fatalf("archived brief sidecar counted as review: counts=%+v items=%d", archived.Counts, len(archived.Items))
	}
	archivedProjection := decisionBriefItemMap(t, archived.Items[0])
	if _, ok := archivedProjection["brief"].(map[string]any); !ok {
		t.Fatalf("archived brief not reattached: %+v", archivedProjection["brief"])
	}
	if !strings.Contains(strings.Join(archived.Items[0].ExtraPaths, "\n"), id+".brief.json") {
		t.Fatalf("archived brief path missing from extras: %+v", archived.Items[0].ExtraPaths)
	}
}

// TestQueueItemWithoutDecisionBriefRendersUnchanged is the no-brief-renders-
// as-today falsifier: an item with NO `.brief.json` sidecar must carry
// neither a "brief" nor a "warnings" key at all (not merely null/empty) —
// the JSON shape for every pre-existing review artifact stays byte-for-byte
// what it was before this packet landed.
func TestQueueItemWithoutDecisionBriefRendersUnchanged(t *testing.T) {
	homePath := t.TempDir()
	const area = "decision-brief-fixtures"
	const id = "brief_absent"
	writeDecisionBriefCore(t, homePath, area, id, "review_required", BandNeedsYou)

	report, err := Queue(homePath, QueueOptions{Area: area}, time.Date(2026, 7, 10, 5, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Total != 1 || len(report.Items) != 1 {
		t.Fatalf("counts=%+v items=%d", report.Counts, len(report.Items))
	}
	item := report.Items[0]
	if item.Brief != nil {
		t.Fatalf("brief must be nil without a sidecar: %+v", item.Brief)
	}
	if len(item.Warnings) != 0 {
		t.Fatalf("warnings must be empty without a sidecar: %+v", item.Warnings)
	}
	projection := decisionBriefItemMap(t, item)
	if _, ok := projection["brief"]; ok {
		t.Fatalf("brief key must be entirely absent from JSON when there is no sidecar: %+v", projection)
	}
	if _, ok := projection["warnings"]; ok {
		t.Fatalf("warnings key must be entirely absent from JSON when there is no sidecar: %+v", projection)
	}
}

// TestParseDecisionBrief exercises the schema validator directly (below the
// Queue integration), table-driven, per the golang-testing convention.
func TestParseDecisionBrief(t *testing.T) {
	marshal := func(t *testing.T, payload map[string]any) []byte {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	t.Run("valid brief parses with no warnings", func(t *testing.T) {
		brief, warnings := ParseDecisionBrief(marshal(t, decisionBriefFixture()))
		if brief == nil {
			t.Fatalf("expected a parsed brief, warnings=%v", warnings)
		}
		if len(warnings) != 0 {
			t.Fatalf("valid brief must carry no warnings: %v", warnings)
		}
		if brief.Headline != "Choose the bounded next move" || brief.PreparedBy != "switchback_api" {
			t.Fatalf("brief fields not decoded: %+v", brief)
		}
		if len(brief.Findings) != 1 || brief.Findings[0].EvidenceRef != "compound://evidence/review-fixture" {
			t.Fatalf("findings not decoded: %+v", brief.Findings)
		}
	})

	t.Run("missing headline warns and rejects", func(t *testing.T) {
		payload := decisionBriefFixture()
		payload["headline"] = ""
		brief, warnings := ParseDecisionBrief(marshal(t, payload))
		if brief != nil {
			t.Fatalf("brief without headline must be rejected: %+v", brief)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), "headline") {
			t.Fatalf("expected a headline warning, got %v", warnings)
		}
	})

	t.Run("empty findings warns and rejects", func(t *testing.T) {
		payload := decisionBriefFixture()
		payload["findings"] = []any{}
		brief, warnings := ParseDecisionBrief(marshal(t, payload))
		if brief != nil {
			t.Fatalf("brief without findings must be rejected: %+v", brief)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), "findings") {
			t.Fatalf("expected a findings warning, got %v", warnings)
		}
	})

	t.Run("unknown top-level field rejects with unknown field warning", func(t *testing.T) {
		payload := decisionBriefFixture()
		payload["status"] = "recorded"
		brief, warnings := ParseDecisionBrief(marshal(t, payload))
		if brief != nil {
			t.Fatalf("brief with an unknown field must be rejected: %+v", brief)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), "unknown field") {
			t.Fatalf("expected an unknown-field warning, got %v", warnings)
		}
	})
}

func TestDecisionBriefWritePathAndSchema(t *testing.T) {
	got := DecisionBriefWritePath("/home/.compound/reviews/area/item_1.json", "item_1")
	want := filepath.Join("/home/.compound/reviews/area", "item_1.brief.json")
	if got != want {
		t.Fatalf("write path = %q, want %q", got, want)
	}
	schema := DecisionBriefJSONSchema()
	if schema["$id"] != DecisionBriefSchemaVersion {
		t.Fatalf("schema $id = %v, want %v", schema["$id"], DecisionBriefSchemaVersion)
	}
	if len(DecisionBriefProposeOnlyWarnings) == 0 {
		t.Fatal("propose-only warnings must not be empty")
	}
}
