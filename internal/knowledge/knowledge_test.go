package knowledge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/sourceobs"
)

var testNow = time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)

func TestImportRecordsEvidenceCandidateExtractionAndSource(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}

	report, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Items != 3 || report.Imported != 3 || report.Echoes != 0 || report.Reused != 0 {
		t.Fatalf("report counts = %+v", report)
	}
	if len(report.EvidenceIDs) != 3 || len(report.CandidateIDs) != 15 || len(report.ExtractionIDs) != 3 {
		t.Fatalf("report ids = %+v", report)
	}

	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 3 {
		t.Fatalf("evidence rows = %d; want 3", len(evidence))
	}
	if evidence[0].SourceKind != "arxiv" || evidence[0].Actor != "tool" {
		t.Fatalf("evidence[0] = %+v", evidence[0])
	}

	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 15 {
		t.Fatalf("candidate rows = %d; want 15", len(candidates))
	}
	if candidates[0].State != "parked" || candidates[0].Qualifiers["source"] != "knowledge_import" {
		t.Fatalf("candidate[0] = %+v", candidates[0])
	}
	if !strings.Contains(candidates[0].Reason, "source metadata") {
		t.Fatalf("metadata candidate reason = %q, want source metadata admission", candidates[0].Reason)
	}
	if candidates[0].PromotionBasis != "trusted_deterministic_import" {
		t.Fatalf("metadata candidate basis = %q, want trusted_deterministic_import (deterministic tool import)", candidates[0].PromotionBasis)
	}
	if candidates[0].Visibility != "contextual" {
		t.Fatalf("public metadata candidate visibility = %q, want contextual", candidates[0].Visibility)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("knowledge import must not create promoted claims, stat err=%v", err)
	}

	extractionPath := filepath.Join(homePath, "inbox", "knowledge-extractions", report.ExtractionIDs[0]+".json")
	var extraction ExtractionRequest
	readJSON(t, extractionPath, &extraction)
	if extraction.SchemaVersion != ExtractionRequestSchemaVersion || extraction.State != "pending" || extraction.EvidenceRef == "" {
		t.Fatalf("extraction = %+v", extraction)
	}

	var sources []SourceRegistryRow
	readJSONLRows(t, filepath.Join(homePath, "registries", "knowledge-sources.jsonl"), &sources)
	if len(sources) != 1 || sources[0].SourceID != "source:arxiv:cs.AI+cs.CL" || sources[0].TrustPosterior != 0.5 {
		t.Fatalf("sources = %+v", sources)
	}
}

func TestImportNormalizesKnowledgeEntityAndSplitsMetadataCandidates(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}

	export := Export{
		SchemaVersion: ExportSchemaVersion,
		Source: Source{
			Kind:       "arxiv",
			Ref:        "arxiv:cs.AI",
			CapturedAt: testNow.Format(time.RFC3339),
		},
		Privacy: "public",
		Items: []Item{{
			Ref:         "arxiv:2506.12345v1",
			URL:         "https://arxiv.org/abs/2506.12345",
			Title:       "Synthetic Agents Learn Evidence Discipline",
			Authors:     []string{"A. Researcher", "B. Builder"},
			PublishedAt: "2026-06-30",
			Topics:      []string{"cs.AI", "cs.CL"},
			Content:     "Captured artifacts become evidence before any memory claim is promoted.",
		}},
	}
	report, err := Import(homePath, store, export, "digest-a", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 1 || len(report.CandidateIDs) != 5 || len(report.ExtractionIDs) != 1 {
		t.Fatalf("report = %+v, want one item, five metadata candidates, one extraction", report)
	}
	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 5 {
		t.Fatalf("candidate rows = %d; want 5", len(candidates))
	}
	wantObjects := map[string]string{
		"reference_title":        "Synthetic Agents Learn Evidence Discipline",
		"reference_authors":      "A. Researcher; B. Builder",
		"reference_published_at": "2026-06-30",
		"reference_topics":       "cs.AI; cs.CL",
		"reference_url":          "https://arxiv.org/abs/2506.12345",
	}
	for _, candidate := range candidates {
		if candidate.Entity != "knowledge:arxiv:2506.12345v1" {
			t.Fatalf("candidate entity = %q, want normalized knowledge entity", candidate.Entity)
		}
		want, ok := wantObjects[candidate.Predicate]
		if !ok {
			t.Fatalf("unexpected predicate %q in %+v", candidate.Predicate, candidates)
		}
		if candidate.Object != want {
			t.Fatalf("%s object = %q, want %q", candidate.Predicate, candidate.Object, want)
		}
		if candidate.SourceTraceRef != report.EvidenceIDs[0] {
			t.Fatalf("candidate %s source trace = %q, want shared evidence %q", candidate.ID, candidate.SourceTraceRef, report.EvidenceIDs[0])
		}
		if candidate.State != "parked" || !strings.Contains(candidate.Reason, "source metadata") {
			t.Fatalf("metadata candidate admission = %+v, want parked source evidence", candidate)
		}
		delete(wantObjects, candidate.Predicate)
	}
	if len(wantObjects) != 0 {
		t.Fatalf("missing metadata predicates: %+v", wantObjects)
	}
	if got := knowledgeEntity("arxiv", "2506.12345v1"); got != "knowledge:arxiv:2506.12345v1" {
		t.Fatalf("bare ref entity = %q", got)
	}
}

func TestImportFoldsEchoAcrossSourceKinds(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}

	echo, err := ImportFile(homePath, store, fixturePath("web-echo-export.json"), testNow.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if echo.Items != 1 || echo.Imported != 0 || echo.Echoes != 1 || echo.Reused != 0 {
		t.Fatalf("echo report counts = %+v", echo)
	}
	if len(echo.CandidateIDs) != 0 || len(echo.ExtractionIDs) != 0 {
		t.Fatalf("echo must not create candidates/extractions: %+v", echo)
	}

	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 15 {
		t.Fatalf("echo created duplicate candidates: %+v", candidates)
	}

	var events []DistributionEvent
	readJSONLRows(t, filepath.Join(homePath, "ledger", "knowledge-distribution.jsonl"), &events)
	if len(events) != 1 {
		t.Fatalf("distribution events = %+v", events)
	}
	if events[0].CanonicalRef != "https://arxiv.org/abs/2506.12345" || events[0].EchoSourceKind != "web" || events[0].FirstEvidenceRef != first.EvidenceIDs[0] {
		t.Fatalf("distribution event = %+v", events[0])
	}
}

func TestImportChatGPTConversationUsesGenericKnowledgeDoor(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixturePath("chatgpt-conversation-export.json")
	export, _, err := LoadExport(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(export.Items) != 1 || len(export.Items[0].Content) <= maxEvidenceQuoteChars {
		t.Fatalf("fixture must carry one long content item, got items=%d content_len=%d", len(export.Items), len(export.Items[0].Content))
	}

	report, err := ImportFile(homePath, store, fixture, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Items != 1 || report.Imported != 1 || report.Echoes != 0 || report.Reused != 0 {
		t.Fatalf("first import report = %+v", report)
	}
	if len(report.EvidenceIDs) != 1 || len(report.CandidateIDs) != 3 || len(report.ExtractionIDs) != 1 {
		t.Fatalf("first import ids = %+v", report)
	}
	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 {
		t.Fatalf("evidence rows = %d, want 1", len(evidence))
	}
	if evidence[0].SourceKind != "chatgpt" || evidence[0].SourceID != export.Items[0].Ref {
		t.Fatalf("evidence provenance = %+v", evidence[0])
	}
	if evidence[0].EventTime != export.Items[0].PublishedAt || evidence[0].EventTime == rfc3339(testNow) {
		t.Fatalf("evidence event time = %q, want item published_at %q and not import time %q", evidence[0].EventTime, export.Items[0].PublishedAt, rfc3339(testNow))
	}
	if evidence[0].Visibility != "private" {
		t.Fatalf("evidence visibility = %q, want private", evidence[0].Visibility)
	}
	if len(evidence[0].QuoteText) != maxEvidenceQuoteChars {
		t.Fatalf("quote len = %d, want bounded %d", len(evidence[0].QuoteText), maxEvidenceQuoteChars)
	}
	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("candidate rows = %d, want 3: %+v", len(candidates), candidates)
	}
	for _, candidate := range candidates {
		if candidate.Visibility != "private" {
			t.Fatalf("candidate %s visibility = %q, want private", candidate.ID, candidate.Visibility)
		}
		if candidate.SourceTraceRef != report.EvidenceIDs[0] {
			t.Fatalf("candidate %s source trace = %q, want %q", candidate.ID, candidate.SourceTraceRef, report.EvidenceIDs[0])
		}
		if candidate.State != "parked" || !strings.Contains(candidate.Reason, "source metadata") {
			t.Fatalf("chatgpt metadata admission = %+v, want parked source evidence", candidate)
		}
	}

	reimport, err := ImportFile(homePath, store, fixture, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reimport.Imported != 0 || reimport.Echoes != 0 || reimport.Reused != 1 {
		t.Fatalf("reimport report = %+v", reimport)
	}

	echoExport := export
	echoExport.Source = Source{Kind: "claude-code", Ref: "session:synthetic-echo", CapturedAt: testNow.Add(2 * time.Hour).Format(time.RFC3339)}
	echoExport.Producer = Producer{Kind: "edge-script", Name: "claude-session-capture", Version: "0.1"}
	echoExport.Items[0].Content = "A second synthetic source cites the same private conversation ref, so the importer should fold it as distribution evidence."
	echo, err := Import(homePath, store, echoExport, "synthetic-echo-digest", testNow.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if echo.Imported != 0 || echo.Echoes != 1 || echo.Reused != 0 || len(echo.CandidateIDs) != 0 || len(echo.ExtractionIDs) != 0 {
		t.Fatalf("echo report = %+v", echo)
	}
	assertCandidateCount(t, store, 3)
	var events []DistributionEvent
	readJSONLRows(t, filepath.Join(homePath, "ledger", "knowledge-distribution.jsonl"), &events)
	if len(events) != 1 || events[0].EchoSourceKind != "claude-code" || events[0].FirstEvidenceRef != report.EvidenceIDs[0] {
		t.Fatalf("distribution events = %+v", events)
	}
}

func TestImportXBookmarksExportRecordsSourceObservationsWithoutPromotion(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixturePath("x-bookmarks-export.json")
	export, _, err := LoadExport(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if export.Privacy != "private" {
		t.Fatalf("x_bookmarks privacy default = %q, want private", export.Privacy)
	}
	if len(export.Items) != 2 || export.Items[0].BookmarkedAt == "" || len(export.Items[0].MediaRefs) != 1 || export.Items[0].AuthorHandle == "" {
		t.Fatalf("bookmark fixture shape = %+v", export.Items)
	}

	report, err := ImportFile(homePath, store, fixture, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Items != 2 || report.Imported != 2 || report.Echoes != 0 || report.Reused != 0 {
		t.Fatalf("bookmark import report counts = %+v", report)
	}
	if len(report.EvidenceIDs) != 2 || len(report.ExtractionIDs) != 2 || len(report.CandidateIDs) == 0 {
		t.Fatalf("bookmark evidence/candidates/extractions = %+v", report)
	}
	if len(report.SourceObservationIDs) != 2 || len(report.SourceCandidateIDs) != 2 || len(report.NoPromoteIDs) != 2 {
		t.Fatalf("bookmark source observations = %+v", report)
	}

	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 2 || evidence[0].SourceKind != "x_bookmarks" || evidence[0].Visibility != "private" {
		t.Fatalf("bookmark evidence = %+v", evidence)
	}
	if evidence[0].EventTime != export.Items[0].BookmarkedAt {
		t.Fatalf("bookmark event time = %q, want %q", evidence[0].EventTime, export.Items[0].BookmarkedAt)
	}
	memCandidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(memCandidates) == 0 {
		t.Fatal("bookmark import should create parked metadata memory candidates")
	}
	for _, candidate := range memCandidates {
		if candidate.Visibility != "private" {
			t.Fatalf("metadata candidate visibility = %+v, want private", candidate)
		}
		if candidate.State != "parked" {
			t.Fatalf("metadata candidate state = %+v, want parked", candidate)
		}
	}

	sourceList, err := sourceobs.ListSourceObservations(homePath, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if sourceList.Counts.SourceObservations != 2 || sourceList.Counts.NoPromoteDecisions != 2 || sourceList.Counts.PromoteDisallowed != 2 {
		t.Fatalf("source observation list = %+v", sourceList.Counts)
	}
	gotKinds := map[string]int{}
	for _, item := range sourceList.Items {
		gotKinds[item.Kind]++
		if item.PromoteAllowed || item.NoPromoteID == "" {
			t.Fatalf("source observation item should be no-promote: %+v", item)
		}
	}
	wantKinds := map[string]int{
		string(candidates.CapabilityObs): 2,
	}
	for kind, want := range wantKinds {
		if gotKinds[kind] != want {
			t.Fatalf("source candidate kind %s = %d, want %d; all=%+v", kind, gotKinds[kind], want, gotKinds)
		}
	}
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("source candidate kinds = %+v, want only %+v", gotKinds, wantKinds)
	}

	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("bookmark import must not promote claims, stat err=%v", err)
	}
}

func TestImportXBookmarksExplicitCandidateClassesControlsFanout(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	export := Export{
		SchemaVersion: ExportSchemaVersion,
		Source: Source{
			Kind:       "x_bookmarks",
			Ref:        "fixture-batch:explicit-classes",
			CapturedAt: rfc3339(testNow),
		},
		Items: []Item{{
			Ref:              "x-bookmark:explicit-001",
			URL:              "https://example.invalid/bookmarks/explicit-classes",
			BookmarkedAt:     rfc3339(testNow.Add(-time.Hour)),
			CandidateClasses: []string{"reference", "technique", "market_take", "market_take", "unknown"},
			Content:          "Synthetic bookmark text with explicitly requested source candidate classes.",
		}},
	}

	report, err := Import(homePath, store, export, "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SourceObservationIDs) != 1 || len(report.SourceCandidateIDs) != 3 || len(report.NoPromoteIDs) != 3 {
		t.Fatalf("explicit bookmark source observations = %+v", report)
	}
	sourceList, err := sourceobs.ListSourceObservations(homePath, testNow)
	if err != nil {
		t.Fatal(err)
	}
	gotKinds := map[string]int{}
	for _, item := range sourceList.Items {
		gotKinds[item.Kind]++
	}
	wantKinds := map[string]int{
		string(candidates.CapabilityObs):     1,
		string(candidates.MethodUpgrade):     1,
		string(candidates.ProductHypothesis): 1,
	}
	for kind, want := range wantKinds {
		if gotKinds[kind] != want {
			t.Fatalf("explicit source candidate kind %s = %d, want %d; all=%+v", kind, gotKinds[kind], want, gotKinds)
		}
	}
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("explicit source candidate kinds = %+v, want %+v", gotKinds, wantKinds)
	}
}

func TestImportDedupSurvivesBackupRestore(t *testing.T) {
	sourceHome := initializedHome(t)
	sourceStore, err := mem.Open(sourceHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFile(sourceHome, sourceStore, fixturePath("arxiv-export.json"), testNow); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "compound-home.tar.gz")
	if _, err := home.Backup(sourceHome, archivePath, testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	restoredHome := filepath.Join(t.TempDir(), ".compound")
	if _, err := home.RestoreArchive(archivePath, restoredHome, testNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	restoredStore, err := mem.Open(restoredHome)
	if err != nil {
		t.Fatal(err)
	}

	reimport, err := ImportFile(restoredHome, restoredStore, fixturePath("arxiv-export.json"), testNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if reimport.Imported != 0 || reimport.Echoes != 0 || reimport.Reused != 3 {
		t.Fatalf("reimport after restore = %+v", reimport)
	}
	assertCandidateCount(t, restoredStore, 15)

	echo, err := ImportFile(restoredHome, restoredStore, fixturePath("web-echo-export.json"), testNow.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if echo.Imported != 0 || echo.Echoes != 1 || echo.Reused != 0 {
		t.Fatalf("echo after restore = %+v", echo)
	}
	assertCandidateCount(t, restoredStore, 15)
	assertDistributionCount(t, restoredHome, 1)
	assertImportRows(t, filepath.Join(restoredHome, "ledger", "knowledge-imports.jsonl"), 4)
	assertImportRows(t, filepath.Join(restoredHome, "indexes", "knowledge-imports.jsonl"), 4)
	t.Logf("backup_restore: reimport imported=%d reused=%d echoes=%d; echo imported=%d reused=%d echoes=%d; candidates=15 import_rows=4", reimport.Imported, reimport.Reused, reimport.Echoes, echo.Imported, echo.Reused, echo.Echoes)
}

func TestImportDedupSurvivesRebuildableCompact(t *testing.T) {
	homePath := initializedHome(t)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow); err != nil {
		t.Fatal(err)
	}

	if _, err := home.Compact(homePath, home.CompactOptions{Rebuildable: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "indexes", "knowledge-imports.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("knowledge import index should be deleted by compact, stat err=%v", err)
	}

	reimport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reimport.Imported != 0 || reimport.Echoes != 0 || reimport.Reused != 3 {
		t.Fatalf("reimport after compact = %+v", reimport)
	}
	assertCandidateCount(t, store, 15)

	echo, err := ImportFile(homePath, store, fixturePath("web-echo-export.json"), testNow.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if echo.Imported != 0 || echo.Echoes != 1 || echo.Reused != 0 {
		t.Fatalf("echo after compact = %+v", echo)
	}
	assertCandidateCount(t, store, 15)
	assertDistributionCount(t, homePath, 1)
	assertImportRows(t, filepath.Join(homePath, "ledger", "knowledge-imports.jsonl"), 4)
	assertImportRows(t, filepath.Join(homePath, "indexes", "knowledge-imports.jsonl"), 4)
	t.Logf("compact_rebuildable: reimport imported=%d reused=%d echoes=%d; echo imported=%d reused=%d echoes=%d; candidates=15 import_rows=4", reimport.Imported, reimport.Reused, reimport.Echoes, echo.Imported, echo.Reused, echo.Echoes)
}

func TestImportIndexMigratesToLedgerOnLoad(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	row := ImportIndexRow{
		CanonicalRef: "https://arxiv.org/abs/2506.12345",
		EvidenceRef:  "ev_legacy",
		ImportedAt:   rfc3339(testNow),
		ExportDigest: fixtureDigest(t, "arxiv-export.json"),
	}
	if err := appendImportIndex(homePath, row); err != nil {
		t.Fatal(err)
	}

	reimport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reimport.Imported != 2 || reimport.Echoes != 0 || reimport.Reused != 1 {
		t.Fatalf("legacy index migration import = %+v", reimport)
	}
	assertImportRows(t, filepath.Join(homePath, "ledger", "knowledge-imports.jsonl"), 3)
	assertCandidateCount(t, store, 10)
}

func TestExtractPendingParksKnowledgeEvidenceCandidatesAndMarksDone(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	writeExtractionFixture(t, fixtures, importReport.ExtractionIDs[0], importReport.EvidenceIDs[0])

	report, err := Extract(homePath, store, ExtractOptions{ItemID: importReport.ExtractionIDs[0], FixturesDir: fixtures}, testNow.Add(time.Minute))
	if err != nil {
		var req ExtractionRequest
		readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
		t.Fatalf("extract error: %v; request = %+v", err, req)
	}
	if report.Processed != 1 || report.Created != 1 || len(report.CandidateIDs) != 1 {
		t.Fatalf("extract report = %+v", report)
	}

	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
	if req.State != "done" || req.CompletedAt == "" || len(req.CandidateIDs) != 1 || req.RouteEvidence == nil || req.RouteEvidence.Provider != "fixture" {
		t.Fatalf("extraction request not marked done: %+v", req)
	}
	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	var extracted *mem.Candidate
	for i := range candidates {
		if candidates[i].ID == report.CandidateIDs[0] {
			extracted = &candidates[i]
			break
		}
	}
	if extracted == nil {
		t.Fatalf("extracted candidate %s not found in %+v", report.CandidateIDs[0], candidates)
	}
	if extracted.State != "parked" || extracted.Qualifiers["source"] != "knowledge_extraction" || extracted.Qualifiers["extraction_id"] != req.ID {
		t.Fatalf("candidate shape = %+v", extracted)
	}
	if !strings.Contains(extracted.Reason, "knowledge extraction") {
		t.Fatalf("extracted candidate reason = %q, want knowledge extraction admission", extracted.Reason)
	}
	if extracted.SourceTraceRef != "knowledge_extraction:"+req.ID || extracted.Visibility != "contextual" {
		t.Fatalf("candidate provenance = %+v", extracted)
	}

	replay, err := Extract(homePath, store, ExtractOptions{ItemID: req.ID, FixturesDir: fixtures}, testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replay.Processed != 0 || replay.Skipped != 1 || len(replay.CandidateIDs) != 1 || replay.CandidateIDs[0] != extracted.ID {
		t.Fatalf("idempotent replay = %+v", replay)
	}
}

func TestExtractEssenceFixtureShapesCandidatesAndPersistsResidue(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	writeEssenceFixture(t, fixtures, importReport.ExtractionIDs[0])

	report, err := Extract(homePath, store, ExtractOptions{ItemID: importReport.ExtractionIDs[0], FixturesDir: fixtures}, testNow.Add(time.Minute))
	if err != nil {
		var req ExtractionRequest
		readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
		t.Fatalf("extract error: %v; request = %+v", err, req)
	}
	if report.Processed != 1 || report.Created != 1 || len(report.CandidateIDs) != 1 {
		t.Fatalf("extract report = %+v", report)
	}

	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
	if req.State != "done" || len(req.EssenceLines) != 2 || len(req.EssenceResidue) != 1 {
		t.Fatalf("essence request = %+v", req)
	}
	if req.RouteEvidence == nil ||
		req.RouteEvidence.EssenceLines != 2 ||
		req.RouteEvidence.ShapedCandidates != 1 ||
		req.RouteEvidence.ResidueLines != 1 ||
		req.RouteEvidence.HallucinationRejections != 1 {
		t.Fatalf("route evidence = %+v", req.RouteEvidence)
	}

	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	var shaped *mem.Candidate
	for i := range candidates {
		if candidates[i].ID == report.CandidateIDs[0] {
			shaped = &candidates[i]
			break
		}
	}
	if shaped == nil {
		t.Fatalf("missing shaped candidate %q in %+v", report.CandidateIDs[0], candidates)
	}
	if shaped.Entity != "knowledge:arxiv:2506.12345v1" || shaped.Predicate != "reports_result" {
		t.Fatalf("shaped candidate identity = %+v", shaped)
	}
	if shaped.Qualifiers["source"] != "knowledge_extraction" ||
		shaped.Qualifiers["extracted_by"] != "fixture" ||
		shaped.Qualifiers["shaped_by"] != "deterministic" {
		t.Fatalf("shaped provenance qualifiers = %+v", shaped.Qualifiers)
	}

	var rows []ExtractionModelEvidenceRow
	readJSONLRows(t, filepath.Join(homePath, "ledger", "knowledge-extraction-model-evidence.jsonl"), &rows)
	if len(rows) != 1 {
		t.Fatalf("model evidence rows = %+v", rows)
	}
	if rows[0].ExtractionID != req.ID || rows[0].EssenceLines != 2 || rows[0].ShapedCandidates != 1 || rows[0].ResidueLines != 1 || rows[0].HallucinationRejections != 1 {
		t.Fatalf("model evidence row = %+v", rows[0])
	}
}

// TestBuildKnowledgeEssencePromptForbidsUngroundedFactsAndAllowsEmpty
// certifies the two faithfulness clauses of the essence prompt survive:
// (1) statements must not add facts absent from the evidence quote, and
// (2) an empty essence_lines list is preferred over restating the quote.
// Teeth recipe: delete or weaken either clause string in
// buildKnowledgeEssencePrompt and this test goes RED on the matching want;
// the two wants are intentionally clause-specific substrings so a clause
// dropping or silently rewording fails the falsifier rather than passing it.
func TestBuildKnowledgeEssencePromptForbidsUngroundedFactsAndAllowsEmpty(t *testing.T) {
	req := ExtractionRequest{ID: "kx_1", ItemRef: "arxiv:1234.5678v1", SourceKind: "arxiv", Title: "T", EvidenceRef: "ev_1"}
	system, _ := buildKnowledgeEssencePrompt(req, mem.EvidenceItem{ID: "ev_1", QuoteText: "quote"})
	for _, want := range []string{
		"Every fact in statement must appear in the evidence quote. Never add numbers, names, dates, or claims that are not in the evidence.",
		`return {"essence_lines":[]}`,
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("essence prompt missing %q:\n%s", want, system)
		}
	}
}

func TestExtractRejectsUngroundedFixtureEvidenceRefs(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	writeExtractionFixture(t, fixtures, importReport.ExtractionIDs[0], "ev_wrong")

	report, err := Extract(homePath, store, ExtractOptions{ItemID: importReport.ExtractionIDs[0], FixturesDir: fixtures}, testNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "failure(s)") {
		t.Fatalf("expected completed-with-failures error, got %v", err)
	}
	if report.Failed != 1 || report.Processed != 0 {
		t.Fatalf("report = %+v", report)
	}
	// The grounded-evidence rejection is preserved on the item itself.
	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
	if req.State != "error" || !strings.Contains(req.Error, "evidence_ref") {
		t.Fatalf("item should hold the grounded-evidence error: %+v", req)
	}
}

func TestExtractRejectsZeroCandidateResponse(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	body := fmt.Sprintf(`{
 "schema_version": "compound.distiller.llm_extraction.v0.1",
 "trace_ref": "knowledge_extraction:%s",
 "source_refs": ["%s"],
 "candidates": []
}`, importReport.ExtractionIDs[0], importReport.EvidenceIDs[0])
	if err := os.WriteFile(filepath.Join(fixtures, importReport.ExtractionIDs[0]+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Extract(homePath, store, ExtractOptions{ItemID: importReport.ExtractionIDs[0], FixturesDir: fixtures}, testNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "failure(s)") {
		t.Fatalf("expected completed-with-failures error, got %v", err)
	}
	if report.Failed != 1 || report.Processed != 0 || report.Created != 0 {
		t.Fatalf("report = %+v", report)
	}
	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
	if req.State != "error" || !strings.Contains(req.Error, "zero candidates") {
		t.Fatalf("zero-candidate response should stay retryable error: %+v", req)
	}
}

func TestExtractRetriesDoneRequestWithZeroCandidates(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
	req.State = "done"
	req.CompletedAt = rfc3339(testNow.Add(time.Minute))
	req.CandidateIDs = nil
	if err := writeExtractionRequestFile(homePath, req); err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	writeExtractionFixture(t, fixtures, importReport.ExtractionIDs[0], importReport.EvidenceIDs[0])

	report, err := Extract(homePath, store, ExtractOptions{ItemID: importReport.ExtractionIDs[0], FixturesDir: fixtures}, testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Processed != 1 || report.Created != 1 || len(report.CandidateIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
	if req.State != "done" || len(req.CandidateIDs) != 1 || req.RouteEvidence == nil {
		t.Fatalf("done-zero request should be healed: %+v", req)
	}
}

func TestExtractOpenAICompatibleUsesEvalCorpusAndRecordsRouteEvidence(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		body := essenceFixtureBody()
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"role": "assistant", "content": body},
			}},
			"usage": map[string]int{"total_tokens": 12},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	report, err := Extract(homePath, store, ExtractOptions{
		ItemID:              importReport.ExtractionIDs[0],
		OpenAICompatibleURL: server.URL,
		Model:               "fixture-model",
		EvalCorpusDir:       filepath.Join("..", "..", "fixtures", "distiller"),
	}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || report.Processed != 1 || report.Created != 1 || len(report.CandidateIDs) != 1 {
		t.Fatalf("calls=%d report=%+v", calls, report)
	}
	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
	if req.RouteEvidence == nil || req.RouteEvidence.Provider != "openai-compatible" || !req.RouteEvidence.Called || req.RouteEvidence.LiveModelCalls != 1 {
		t.Fatalf("route evidence = %+v", req.RouteEvidence)
	}
	if req.RouteEvidence.Model != "fixture-model" || req.RouteEvidence.Usage.TotalTokens != 12 {
		t.Fatalf("route evidence model/usage = %+v", req.RouteEvidence)
	}
}

func TestExtractOpenAICompatibleEssenceResponseRecordsModelEvidence(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body := essenceFixtureBody()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"role": "assistant", "content": body},
			}},
			"usage": map[string]int{"total_tokens": 18},
		})
	}))
	defer server.Close()

	report, err := Extract(homePath, store, ExtractOptions{
		ItemID:              importReport.ExtractionIDs[0],
		OpenAICompatibleURL: server.URL,
		Model:               "fixture-model",
		EvalCorpusDir:       filepath.Join("..", "..", "fixtures", "distiller"),
	}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || report.Processed != 1 || report.Created != 1 {
		t.Fatalf("calls=%d report=%+v", calls, report)
	}
	var req ExtractionRequest
	readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", importReport.ExtractionIDs[0]+".json"), &req)
	if req.RouteEvidence == nil ||
		req.RouteEvidence.LiveModelCalls != 1 ||
		req.RouteEvidence.Usage.TotalTokens != 18 ||
		req.RouteEvidence.EssenceLines != 2 ||
		req.RouteEvidence.ShapedCandidates != 1 ||
		req.RouteEvidence.ResidueLines != 1 {
		t.Fatalf("route evidence = %+v", req.RouteEvidence)
	}
	var rows []ExtractionModelEvidenceRow
	readJSONLRows(t, filepath.Join(homePath, "ledger", "knowledge-extraction-model-evidence.jsonl"), &rows)
	if len(rows) != 1 || rows[0].ExtractedBy != "openai-compatible/fixture-model" || rows[0].ShapedCandidates != 1 {
		t.Fatalf("model evidence rows = %+v", rows)
	}
}

func TestWholeFileImportIsIdempotent(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.Items != 3 || second.Imported != 0 || second.Echoes != 0 || second.Reused != 3 {
		t.Fatalf("second report counts = %+v", second)
	}
	if strings.Join(second.EvidenceIDs, ",") != strings.Join(first.EvidenceIDs, ",") {
		t.Fatalf("reused evidence ids changed: first=%+v second=%+v", first.EvidenceIDs, second.EvidenceIDs)
	}

	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 3 {
		t.Fatalf("idempotent import created duplicate evidence: %+v", evidence)
	}
	var imports []ImportIndexRow
	readJSONLRows(t, filepath.Join(homePath, "indexes", "knowledge-imports.jsonl"), &imports)
	if len(imports) != 3 {
		t.Fatalf("idempotent import appended rows: %+v", imports)
	}
}

func TestDecodeExportRejectsStrictShapeProblems(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown_field",
			body: `{"schema_version":"compound.knowledge_export.v0.1","source":{"kind":"arxiv","ref":"arxiv:cs.AI","captured_at":"2026-07-02T12:00:00Z"},"items":[{"ref":"x","content":"body","extra":true}]}`,
			want: "unknown field",
		},
		{
			name: "missing_ref",
			body: `{"schema_version":"compound.knowledge_export.v0.1","source":{"kind":"arxiv","ref":"arxiv:cs.AI","captured_at":"2026-07-02T12:00:00Z"},"items":[{"content":"body"}]}`,
			want: "items[0].ref required",
		},
		{
			name: "empty_items",
			body: `{"schema_version":"compound.knowledge_export.v0.1","source":{"kind":"arxiv","ref":"arxiv:cs.AI","captured_at":"2026-07-02T12:00:00Z"},"items":[]}`,
			want: "items nonempty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeExport([]byte(tt.body)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeExport error = %v; want %q", err, tt.want)
			}
		})
	}
}

func TestNewMediumImportsWithoutCoreChange(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "podcast-export.json")
	if err := os.WriteFile(path, []byte(`{
  "schema_version": "compound.knowledge_export.v0.1",
  "source": {"kind": "podcast", "ref": "podcast:meaning-machines", "captured_at": "2026-07-02T12:00:00Z"},
  "items": [{
    "ref": "episode:42",
    "title": "Evidence Before Truth",
    "authors": ["Host Example"],
    "published_at": "2026-07-01",
    "topics": ["memory"],
    "content": "A podcast episode discusses why captured material should become evidence before candidate claims.",
    "lang": "en"
  }]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := ImportFile(homePath, store, path, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Items != 1 || report.Imported != 1 || len(report.CandidateIDs) != 4 {
		t.Fatalf("podcast report = %+v", report)
	}
	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].SourceKind != "podcast" {
		t.Fatalf("podcast evidence = %+v", evidence)
	}
}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "fixtures", "knowledge", name)
}

func initializedHome(t *testing.T) string {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "local", testNow); err != nil {
		t.Fatal(err)
	}
	return homePath
}

func fixtureDigest(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatal(err)
	}
	return digestBytes(data)
}

func assertCandidateCount(t *testing.T, store *mem.Store, want int) {
	t.Helper()
	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != want {
		t.Fatalf("candidate rows = %d, want %d: %+v", len(candidates), want, candidates)
	}
}

func assertDistributionCount(t *testing.T, homePath string, want int) {
	t.Helper()
	var events []DistributionEvent
	readJSONLRows(t, filepath.Join(homePath, "ledger", "knowledge-distribution.jsonl"), &events)
	if len(events) != want {
		t.Fatalf("distribution events = %d, want %d: %+v", len(events), want, events)
	}
}

func assertImportRows(t *testing.T, path string, want int) {
	t.Helper()
	var rows []ImportIndexRow
	readJSONLRows(t, path, &rows)
	if len(rows) != want {
		t.Fatalf("%s rows = %d, want %d: %+v", path, len(rows), want, rows)
	}
}

func writeExtractionFixture(t *testing.T, dir, extractionID, evidenceID string) {
	t.Helper()
	body := extractionFixtureBody(extractionID, evidenceID)
	if err := os.WriteFile(filepath.Join(dir, extractionID+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeEssenceFixture(t *testing.T, dir, extractionID string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, extractionID+".json"), []byte(essenceFixtureBody()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func essenceFixtureBody() string {
	return `{
 "essence_lines": [
 {
 "predicate_hint": "reports result",
 "statement": "Captured artifacts become evidence before any memory claim is promoted.",
 "quote": "captured artifacts become evidence before any memory claim is promoted"
 },
 {
 "predicate_hint": "claims",
 "statement": "This paper proves a production deployment strategy.",
 "quote": "this quote is not in the evidence"
 }
 ]
}`
}

func extractionFixtureBody(extractionID, evidenceID string) string {
	return fmt.Sprintf(`{
 "schema_version": "compound.distiller.llm_extraction.v0.1",
 "trace_ref": "knowledge_extraction:%s",
 "extractor": "fixture",
 "model": "fixture-rules",
 "source_refs": [%q],
 "candidates": [
  {
   "kind": "memory",
   "entity": "paper:retrieval-agents",
   "predicate": "claim",
   "object": "Retrieval agents need grounded evidence to avoid echo amplification.",
   "confidence": 0.78,
   "derivation": "extracted",
   "claim_type": "architecture",
   "qualifiers": {"domain": "knowledge-intake"},
   "evidence_refs": [%q]
  }
 ]
}`, extractionID, evidenceID, evidenceID)
}

func readJSON(t *testing.T, path string, dst any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatal(err)
	}
}

func readJSONLRows[T any](t *testing.T, path string, out *[]T) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row T
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		*out = append(*out, row)
	}
}

func TestExtractFailsSoftPerItem(t *testing.T) {
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	importReport, err := ImportFile(homePath, store, fixturePath("arxiv-export.json"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(importReport.ExtractionIDs) < 2 {
		t.Fatalf("need >=2 extraction requests, got %d", len(importReport.ExtractionIDs))
	}
	// Fixture responses exist for exactly one request: every other item fails
	// and must not strand the run (state=error, retryable) while the good one
	// completes.
	fixtures := t.TempDir()
	writeExtractionFixture(t, fixtures, importReport.ExtractionIDs[0], importReport.EvidenceIDs[0])

	report, err := Extract(homePath, store, ExtractOptions{Pending: true, FixturesDir: fixtures}, testNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "failure(s)") {
		t.Fatalf("want completed-with-failures error, got %v", err)
	}
	want := len(importReport.ExtractionIDs) - 1
	if report.Processed != 1 || report.Failed != want {
		t.Fatalf("processed=%d failed=%d, want 1 and %d", report.Processed, report.Failed, want)
	}
	states := map[string]int{}
	for _, id := range importReport.ExtractionIDs {
		var req ExtractionRequest
		readJSON(t, filepath.Join(homePath, "inbox", "knowledge-extractions", id+".json"), &req)
		states[req.State]++
	}
	if states["done"] != 1 || states["error"] != want {
		t.Fatalf("states = %v", states)
	}
}

func TestExtractionPromptCarriesCandidateShape(t *testing.T) {
	req := ExtractionRequest{ID: "kx_x", ItemRef: "arxiv:1234.5678v1", SourceKind: "arxiv", Title: "T", EvidenceRef: "ev_1"}
	llmReq := buildKnowledgeExtractionRequest(req, mem.EvidenceItem{ID: "ev_1", QuoteText: "quote"})
	joined := strings.Join(llmReq.Instructions, "\n")
	for _, want := range []string{"knowledge:arxiv:1234.5678v1", "trace_ref must be knowledge_extraction:kx_x", `Candidate kind must be "memory"`, "entity, predicate, and object", "snake_case", "falsifiable"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("instructions missing %q:\n%s", want, joined)
		}
	}
}
