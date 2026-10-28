package observation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/knowledge"
	"compound/internal/product"
)

func TestSourceConnectionsTreatKnowledgeImportRowsAsImported(t *testing.T) {
	home := t.TempDir()
	rows := []knowledge.ImportIndexRow{
		{
			CanonicalRef: "https://example.invalid/bookmarks/synthetic-design-reference",
			EvidenceRef:  "ev_synthetic_bookmark_001",
			ImportedAt:   tObservation.Format(time.RFC3339),
			ExportDigest: "digest-synthetic-001",
			SourceKind:   "x_bookmarks",
			SourceRef:    "fixture-batch:synthetic-bookmarks",
		},
		{
			CanonicalRef: "https://example.invalid/bookmarks/synthetic-market-note",
			EvidenceRef:  "ev_synthetic_bookmark_002",
			ImportedAt:   tObservation.Add(2 * time.Minute).Format(time.RFC3339),
			ExportDigest: "digest-synthetic-002",
			SourceKind:   "x_bookmarks",
			SourceRef:    "fixture-batch:synthetic-bookmarks",
		},
	}
	writeSourceConnectionKnowledgeImports(t, home, rows)

	projection, err := LoadSourceConnectionsProjection(home, tObservation.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	xBookmarks := findSourceConnection(projection.Connections, "x_bookmarks")
	if xBookmarks == nil || xBookmarks.Status != "imported" || xBookmarks.RowCount != 2 {
		t.Fatalf("x_bookmarks connection = %+v", xBookmarks)
	}
	if xBookmarks.SourceKind != "x_bookmarks" || xBookmarks.SourceRef != "fixture-batch:synthetic-bookmarks" {
		t.Fatalf("x_bookmarks source refs = %+v", xBookmarks)
	}
	if !strings.Contains(xBookmarks.NextSafeAction, "bookmark") || !containsString(xBookmarks.Produces, "knowledge evidence") {
		t.Fatalf("x_bookmarks next/produces = %+v", xBookmarks)
	}
	if projection.Counts.Imported != 1 || projection.Counts.Connected != 0 {
		t.Fatalf("source counts should distinguish imported evidence from live connection: %+v", projection.Counts)
	}
	githubStars := findSourceConnection(projection.Connections, "github_stars")
	if githubStars == nil || githubStars.Status != "not_connected" || githubStars.RowCount != 0 {
		t.Fatalf("github stars should remain unconnected: %+v", githubStars)
	}
}

func TestSourceConnectionsTreatProductSignalsAsObservedSources(t *testing.T) {
	home := t.TempDir()
	writeSourceConnectionProductSignals(t, home, []product.Signal{
		{
			ID:           "sig_asc_units",
			ProductID:    "product:sample-health-app",
			MetricName:   "app_store_connect_units",
			Before:       1,
			After:        1,
			Window:       "daily snapshot 2026-07-04",
			EvidenceRefs: []string{"asc:daily:sample-app:2026-07-04:units", "ev_sample"},
			ObservedAt:   tObservation.Format(time.RFC3339),
		},
	})

	projection, err := LoadSourceConnectionsProjection(home, tObservation.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	asc := findSourceConnection(projection.Connections, "asc_daily_signals")
	if asc == nil || asc.Status != "observed" || asc.RowCount != 1 {
		t.Fatalf("asc connection should reflect observed product signal evidence: %+v", asc)
	}
	if !asc.PrivateSetupNeeded {
		t.Fatalf("asc connection should still flag private setup for automatic refresh: %+v", asc)
	}
	if asc.SourceKind != "app_store_connect" || asc.SourceRef != "product-signals:app_store_connect" {
		t.Fatalf("asc source refs = %+v", asc)
	}
	if !strings.Contains(asc.NextSafeAction, "product signals") {
		t.Fatalf("asc next action = %q", asc.NextSafeAction)
	}
	if projection.Counts.Observed != 1 || projection.Counts.NeedsInput != 2 {
		t.Fatalf("source counts should show observed ASC plus chatgpt and app-store-reviews private-config input: %+v", projection.Counts)
	}
}

func writeSourceConnectionKnowledgeImports(t *testing.T, home string, rows []knowledge.ImportIndexRow) {
	t.Helper()
	dir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "knowledge-imports.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
}

func writeSourceConnectionProductSignals(t *testing.T, home string, rows []product.Signal) {
	t.Helper()
	dir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "product-signals.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
}

func findSourceConnection(connections []SourceConnection, id string) *SourceConnection {
	for i := range connections {
		if connections[i].ID == id {
			return &connections[i]
		}
	}
	return nil
}
