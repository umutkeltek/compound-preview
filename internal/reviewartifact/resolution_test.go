package reviewartifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
)

var resolutionTestNow = time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)

func TestQueueAppliesGenericResolutionToUnknownStatusItem(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", resolutionTestNow); err != nil {
		t.Fatal(err)
	}
	paths, err := Write(homePath, "parity", "parity_unknown_fixture", map[string]any{
		"id":     "parity_unknown_fixture",
		"status": "unknown",
	}, " ", "# Parity review\n")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := map[string]any{
		"schema_version":      "compound.review_resolution.v0.1",
		"id":                  "rres_fixture",
		"status":              "recorded",
		"decision":            "parked",
		"reason":              "keep the parity finding held while evidence accumulates",
		"actor":               "operator",
		"reviewed_by":         "operator",
		"resolved_at":         resolutionTestNow.Format(time.RFC3339),
		"source_artifact_ref": "reviews/parity/parity_unknown_fixture.json",
		"previous_status":     "unknown",
		"previous_readiness":  "",
		"previous_band":       BandNeedsYou,
		"undo_hint":           "compound reviews resolve --item parity_unknown_fixture --undo --reason '<why>'",
		"receipt_id":          "rec_resolution_fixture",
	}
	data, err := json.MarshalIndent(sidecar, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(paths.PlanPath, ".json")+".reviewed.json", append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Queue(homePath, QueueOptions{Area: "parity"}, resolutionTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("queue items = %d, want 1", len(report.Items))
	}
	item := report.Items[0]
	if item.Status != "recorded" || item.Readiness != "no_action" || item.Band != BandFiled {
		t.Fatalf("resolved unknown item = %+v, want recorded/no_action/filed", item)
	}
	if report.Counts.OK != 1 || report.Counts.ReviewRequired != 0 || len(report.Groups) != 0 {
		t.Fatalf("resolved unknown queue counts/groups = %+v groups=%+v", report.Counts, report.Groups)
	}
}

func TestReviewResolutionRejectsUnknownFields(t *testing.T) {
	fixture := ReviewResolution{
		SchemaVersion:     ReviewResolutionSchemaVersion,
		ID:                "rres_strict_fixture",
		Status:            "recorded",
		Decision:          "reviewed",
		Reason:            "strict schema fixture",
		Actor:             "operator",
		ReviewedBy:        "operator",
		ResolvedAt:        resolutionTestNow.Format(time.RFC3339),
		SourceArtifactRef: "reviews/bench/bench_strict_fixture.json",
		PreviousStatus:    "unknown",
		PreviousReadiness: "",
		PreviousBand:      BandNeedsYou,
		ReceiptID:         "rec_strict_fixture",
		UndoHint:          "compound reviews resolve --item bench_strict_fixture --undo --reason '<why>'",
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"unexpected":true}`)...)
	if _, err := ParseReviewResolution(data); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
		t.Fatalf("strict resolution decode error = %v", err)
	}
}

func TestResolveUnknownStatusReviewAreasAndUndo(t *testing.T) {
	cases := []struct {
		area      string
		decision  string
		readiness string
	}{
		{area: "bench", decision: "reviewed", readiness: "review_recorded"},
		{area: "parity", decision: "parked", readiness: "no_action"},
		{area: "reconcile", decision: "dismissed", readiness: "no_action"},
		{area: "skill-surfacing", decision: "parked", readiness: "no_action"},
	}
	for _, tc := range cases {
		t.Run(tc.area, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := home.Init(homePath, "test", resolutionTestNow); err != nil {
				t.Fatal(err)
			}
			itemID := strings.ReplaceAll(tc.area, "-", "_") + "_unknown_fixture"
			if _, err := Write(homePath, tc.area, itemID, map[string]any{"id": itemID}, " ", "# Unknown review\n"); err != nil {
				t.Fatal(err)
			}
			initial, err := Queue(homePath, QueueOptions{Area: tc.area}, resolutionTestNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(initial.Items) != 1 || initial.Items[0].Status != "unknown" {
				t.Fatalf("initial unknown item = %+v", initial)
			}
			result, err := ResolveQueueItem(homePath, initial.Items[0], ResolutionInput{
				Decision: tc.decision,
				Reason:   "explicit operator disposition for unknown-status fixture",
				Actor:    "operator",
			}, resolutionTestNow)
			if err != nil {
				t.Fatal(err)
			}
			events, err := eventstore.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := events.Read(result.ReceiptID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(receipt.Bytes), "{\"schema_version\"") ||
				!strings.HasSuffix(string(receipt.Bytes), "\n") || strings.HasSuffix(string(receipt.Bytes), "\n\n") {
				t.Fatalf("resolution receipt is not canonical JSON with exactly one newline: %q", receipt.Bytes)
			}
			resolved, err := Queue(homePath, QueueOptions{Area: tc.area}, resolutionTestNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(resolved.Items) != 1 || resolved.Items[0].Status != "recorded" || resolved.Items[0].Readiness != tc.readiness || resolved.Counts.ReviewRequired != 0 {
				t.Fatalf("resolved unknown item = %+v", resolved)
			}
			if _, err := ResolveQueueItem(homePath, resolved.Items[0], ResolutionInput{
				Decision: tc.decision,
				Reason:   "duplicate disposition",
				Actor:    "operator",
			}, resolutionTestNow.Add(time.Second)); err == nil || !strings.Contains(err.Error(), "undo with: compound reviews resolve") {
				t.Fatalf("re-resolve error = %v, want undo pointer", err)
			}
			if _, err := UndoQueueItem(homePath, resolved.Items[0], UndoResolutionInput{
				Reason: "restore the card for new evidence",
				Actor:  "operator",
			}, resolutionTestNow.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			restored, err := Queue(homePath, QueueOptions{Area: tc.area}, resolutionTestNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(restored.Items) != 1 || restored.Items[0].Status != "unknown" || restored.Counts.ReviewRequired != 1 {
				t.Fatalf("restored unknown item = %+v", restored)
			}
			entries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"rec_"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("resolve+undo receipts = %d, want 2", len(entries))
			}
		})
	}
}

func TestResolveQueueItemRequiresReasonBeforeWriting(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", resolutionTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(homePath, "bench", "bench_reason_fixture", map[string]any{"id": "bench_reason_fixture"}, " ", "# Bench\n"); err != nil {
		t.Fatal(err)
	}
	report, err := Queue(homePath, QueueOptions{Area: "bench"}, resolutionTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveQueueItem(homePath, report.Items[0], ResolutionInput{
		Decision: "parked",
		Reason:   "   ",
		Actor:    "operator",
	}, resolutionTestNow); err == nil || !strings.Contains(err.Error(), "reason required") {
		t.Fatalf("blank reason error = %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(report.Items[0].JSONPath, ".json") + ".reviewed.json"); !os.IsNotExist(err) {
		t.Fatalf("blank reason wrote a sidecar: %v", err)
	}
}

func TestWriteResolutionPairRollsBackReceiptWhenSidecarExclusiveWriteFails(t *testing.T) {
	homePath := t.TempDir()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	sidecarPath := filepath.Join(homePath, "reviews", "bench", "bench_pair.reviewed.json")
	if err := os.MkdirAll(filepath.Dir(sidecarPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, []byte("existing sidecar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	receipt := resolutionReceipt(
		"rec_pair_rollback",
		"operator",
		"bench",
		resolutionTestNow,
		"resolve",
		"parked",
		"retain evidence while the mismatch is investigated",
		"reviews/bench/bench_pair.json",
		"reviews/bench/bench_pair.reviewed.json",
		true,
	)
	resolution := ReviewResolution{
		SchemaVersion:     ReviewResolutionSchemaVersion,
		ID:                "rres_pair_rollback",
		Status:            "recorded",
		Decision:          "parked",
		Reason:            "retain evidence while the mismatch is investigated",
		Actor:             "operator",
		ReviewedBy:        "operator",
		ResolvedAt:        resolutionTestNow.Format(time.RFC3339),
		SourceArtifactRef: "reviews/bench/bench_pair.json",
		PreviousStatus:    "unknown",
		PreviousBand:      BandNeedsYou,
		ReceiptID:         receipt.ID,
		UndoHint:          "compound reviews resolve --item bench_pair --undo --reason '<why>'",
	}

	if _, err := writeResolutionPair(store, receipt, sidecarPath, resolution); err == nil {
		t.Fatal("paired write unexpectedly replaced an existing sidecar")
	}
	exists, err := store.Exists(receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("receipt survived failed paired sidecar publication")
	}
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing sidecar\n" {
		t.Fatalf("existing sidecar changed: %q", data)
	}
}
