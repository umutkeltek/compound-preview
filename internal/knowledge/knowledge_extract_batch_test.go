package knowledge

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"compound/internal/mem"
)

// seedEvidenceHome imports a synthetic export to seed one real evidence item
// into the store, then removes the auto-created extraction requests so a test
// controls the full pending batch itself. It returns the home, the store, and
// the evidence id every synthetic request grounds against.
func seedEvidenceHome(t *testing.T) (string, *mem.Store, string) {
	t.Helper()
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(importReport.EvidenceIDs) == 0 {
		t.Fatalf("import seeded no evidence: %+v", importReport)
	}
	for _, id := range importReport.ExtractionIDs {
		removeExtractionRequest(t, homePath, id)
	}
	return homePath, store, importReport.EvidenceIDs[0]
}

func removeExtractionRequest(t *testing.T, homePath, id string) {
	t.Helper()
	if err := os.Remove(filepath.Join(homePath, "inbox", "knowledge-extractions", id+".json")); err != nil {
		t.Fatal(err)
	}
}

// seedPending writes a synthetic pending extraction request plus a matching
// fixture. createdAtOffset lets a test control deterministic selection order;
// sourceKind is a generic category label (never a personal handle).
func seedPending(t *testing.T, homePath, fixtures, evidenceID, id, sourceKind string, createdAtOffset time.Duration) {
	t.Helper()
	req := ExtractionRequest{
		SchemaVersion: ExtractionRequestSchemaVersion,
		ID:            id,
		ItemRef:       "knowledge:" + sourceKind + ":" + id,
		EvidenceRef:   evidenceID,
		SourceKind:    sourceKind,
		ContentDigest: "digest-" + id,
		State:         ExtractionStatePending,
		CreatedAt:     testNow.Add(createdAtOffset).Format(time.RFC3339),
	}
	if err := writeExtractionRequestFile(homePath, req); err != nil {
		t.Fatal(err)
	}
	writeExtractionFixture(t, fixtures, id, evidenceID)
}

func extractionState(t *testing.T, homePath, id string) string {
	t.Helper()
	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", id+".json"), &req)
	return req.State
}

func assertStates(t *testing.T, homePath string, want map[string]string) {
	t.Helper()
	for id, state := range want {
		if got := extractionState(t, homePath, id); got != state {
			t.Fatalf("request %s state = %q, want %q", id, got, state)
		}
	}
}

// Falsifier 1: home with 5 pending + `--limit 2` processes exactly the first 2
// by (created_at, id); a second run processes the next 2. created_at order is
// the REVERSE of id/filename order, so a naive directory-order impl selects the
// wrong items and reds this test.
func TestExtractPendingLimitProcessesDeterministicFirstN(t *testing.T) {
	homePath, store, evidenceID := seedEvidenceHome(t)
	fixtures := t.TempDir()
	// created_at ascending = e < d < c < b < a  (opposite of lexical id order).
	seedPending(t, homePath, fixtures, evidenceID, "kx_a", "arxiv", 5*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_b", "arxiv", 4*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_c", "arxiv", 3*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_d", "arxiv", 2*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_e", "arxiv", 1*time.Minute)

	report, err := Extract(homePath, store, ExtractOptions{Pending: true, Limit: 2, FixturesDir: fixtures}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("first batch error: %v", err)
	}
	if report.Processed != 2 {
		t.Fatalf("first batch processed = %d, want 2 (report %+v)", report.Processed, report)
	}
	// Earliest two by created_at are kx_e, kx_d.
	assertStates(t, homePath, map[string]string{
		"kx_e": "done", "kx_d": "done",
		"kx_c": "pending", "kx_b": "pending", "kx_a": "pending",
	})

	report2, err := Extract(homePath, store, ExtractOptions{Pending: true, Limit: 2, FixturesDir: fixtures}, testNow.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("second batch error: %v", err)
	}
	if report2.Processed != 2 {
		t.Fatalf("second batch processed = %d, want 2 (report %+v)", report2.Processed, report2)
	}
	assertStates(t, homePath, map[string]string{
		"kx_e": "done", "kx_d": "done", "kx_c": "done", "kx_b": "done",
		"kx_a": "pending",
	})
}

// Falsifier 2: mixed kinds (3 x_likes, 2 arxiv) + `--source-kind x_likes
// --limit 10` processes exactly the 3 x_likes and touches no arxiv item.
func TestExtractPendingSourceKindFilterProcessesOnlyMatchingKind(t *testing.T) {
	homePath, store, evidenceID := seedEvidenceHome(t)
	fixtures := t.TempDir()
	seedPending(t, homePath, fixtures, evidenceID, "kx_like_1", "x_likes", 1*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_like_2", "x_likes", 2*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_like_3", "x_likes", 3*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_arxiv_1", "arxiv", 4*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_arxiv_2", "arxiv", 5*time.Minute)

	report, err := Extract(homePath, store, ExtractOptions{Pending: true, SourceKind: "x_likes", Limit: 10, FixturesDir: fixtures}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if report.Processed != 3 {
		t.Fatalf("processed = %d, want 3 (report %+v)", report.Processed, report)
	}
	assertStates(t, homePath, map[string]string{
		"kx_like_1": "done", "kx_like_2": "done", "kx_like_3": "done",
		"kx_arxiv_1": "pending", "kx_arxiv_2": "pending",
	})
}

// Falsifier 3 (no-regression guard): with --limit and --source-kind omitted,
// every pending item across all kinds is processed — no limit, no filter.
func TestExtractPendingOmittedFlagsProcessesAll(t *testing.T) {
	homePath, store, evidenceID := seedEvidenceHome(t)
	fixtures := t.TempDir()
	ids := []string{"kx_one", "kx_two", "kx_three", "kx_four"}
	kinds := []string{"x_likes", "arxiv", "x_likes", "arxiv"}
	for i, id := range ids {
		seedPending(t, homePath, fixtures, evidenceID, id, kinds[i], time.Duration(i+1)*time.Minute)
	}

	report, err := Extract(homePath, store, ExtractOptions{Pending: true, FixturesDir: fixtures}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if report.Processed != len(ids) || report.Skipped != 0 || report.Failed != 0 {
		t.Fatalf("report = %+v, want processed=%d skipped=0 failed=0", report, len(ids))
	}
	gotIDs := append([]string(nil), report.ExtractionIDs...)
	sort.Strings(gotIDs)
	wantIDs := append([]string(nil), ids...)
	sort.Strings(wantIDs)
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("extraction ids = %v, want %v", gotIDs, wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("extraction ids = %v, want %v", gotIDs, wantIDs)
		}
	}
	for _, id := range ids {
		if got := extractionState(t, homePath, id); got != "done" {
			t.Fatalf("request %s state = %q, want done", id, got)
		}
	}
}

// Falsifier 4: `--limit -1` is a deterministic usage error and processes
// nothing (every request stays pending).
func TestExtractPendingNegativeLimitRejectsBeforeProcessing(t *testing.T) {
	homePath, store, evidenceID := seedEvidenceHome(t)
	fixtures := t.TempDir()
	seedPending(t, homePath, fixtures, evidenceID, "kx_neg_1", "arxiv", 1*time.Minute)
	seedPending(t, homePath, fixtures, evidenceID, "kx_neg_2", "arxiv", 2*time.Minute)

	report, err := Extract(homePath, store, ExtractOptions{Pending: true, Limit: -1, FixturesDir: fixtures}, testNow.Add(time.Hour))
	if err == nil {
		t.Fatalf("want usage error for --limit -1, got nil (report %+v)", report)
	}
	if report.Processed != 0 {
		t.Fatalf("processed = %d, want 0 on rejected limit", report.Processed)
	}
	assertStates(t, homePath, map[string]string{
		"kx_neg_1": "pending", "kx_neg_2": "pending",
	})
}
