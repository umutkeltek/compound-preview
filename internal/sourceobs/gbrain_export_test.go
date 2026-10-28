package sourceobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildGbrainExportManifestFromSyntheticFixture(t *testing.T) {
	exportPath := filepath.Join("..", "..", "fixtures", "sourceobs", "gbrain-db-export-synthetic.json")
	manifest, err := BuildGbrainExportManifest(exportPath, time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SourceFamily != GbrainDBExportManifestFamily {
		t.Fatalf("family = %q", manifest.SourceFamily)
	}
	if manifest.FamilyMeta == nil || manifest.FamilyMeta.GenerationLabel != "current-2026-07-09" {
		t.Fatalf("family meta = %+v", manifest.FamilyMeta)
	}
	if len(manifest.Items) != 3 {
		t.Fatalf("items = %d", len(manifest.Items))
	}
	home := t.TempDir()
	if _, err := RecordSourceManifest(home, manifest); err != nil {
		t.Fatal(err)
	}
	doctor, err := DoctorSourceManifest(home, GbrainDBExportManifestFamily)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" {
		t.Fatalf("doctor = %+v", doctor)
	}
	if doctor.CustodyComplete || doctor.OwnedBodyCount != 0 || doctor.InventoryOnlyCount != len(manifest.Items) {
		t.Fatalf("db export metadata must not masquerade as physical body custody: %+v", doctor)
	}
}

func TestFilterSourceManifestByGenerationDisjoint(t *testing.T) {
	exportPath := filepath.Join("..", "..", "fixtures", "sourceobs", "gbrain-db-export-synthetic.json")
	manifest, err := BuildGbrainExportManifest(exportPath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	current := FilterSourceManifestByGeneration(manifest, "current-2026-07-09")
	pre := FilterSourceManifestByGeneration(manifest, "pre-collapse-2026-06-04")
	if len(current) != 2 {
		t.Fatalf("current items = %d", len(current))
	}
	if len(pre) != 1 {
		t.Fatalf("pre items = %d", len(pre))
	}
	for _, item := range current {
		if item.CorpusGeneration != "current-2026-07-09" {
			t.Fatalf("current filter leaked generation %q", item.CorpusGeneration)
		}
	}
	for _, item := range pre {
		if item.CorpusGeneration != "pre-collapse-2026-06-04" {
			t.Fatalf("pre filter leaked generation %q", item.CorpusGeneration)
		}
	}
	if len(FilterSourceManifestByGeneration(manifest, "never-existed")) != 0 {
		t.Fatal("unknown generation should return empty")
	}
}

func TestGbrainExportRejectsMissingEmbedding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lossy.json")
	// valid page hashes computed for single chunk; embedding dims omitted (0) = lossy
	body := `{
  "schema_version": "compound.gbrain_db_export.v0.1",
  "db_host": "db.example.invalid",
  "db_name": "gbrain_synthetic",
  "dump_ref": "sha256:dump",
  "export_time": "2026-07-09T12:00:00Z",
  "generation_label": "current-2026-07-09",
  "pages": [{
    "page_slug": "x",
    "db_only": true,
    "page_hash": "sha256:ce915cc0e3f140dfdc6181b0283b1d356391e6d66fbcc4b177b9f228caa87156",
    "chunks": [{"ordinal": 0, "chunk_hash": "sha256:chunk_bbb111"}],
    "embedding": {"provider": "ollama", "model": "nomic-embed-text", "dims": 0},
    "corpus_generation": "current-2026-07-09",
    "imported_at": "2026-07-09T12:00:00Z",
    "source_kind": "gbrain_page"
  }]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildGbrainExportManifest(path, time.Now().UTC()); err == nil {
		t.Fatal("expected lossy export (dims=0) to fail")
	}
}

func TestGbrainExportAcceptsDeclaredUnindexedPageCoveredByDump(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unindexed.json")
	body := `{
  "schema_version": "compound.gbrain_db_export.v0.1",
  "db_host": "db.example.invalid",
  "db_name": "gbrain_synthetic",
  "dump_ref": "sha256:dump",
  "export_time": "2026-07-14T00:00:00Z",
  "generation_label": "retirement-2026-07-14",
  "pages": [{
    "page_slug": "unindexed-page",
    "db_only": true,
    "page_hash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "index_state": "unindexed",
    "chunks": [],
    "corpus_generation": "legacy-unassigned",
    "imported_at": "2026-07-14T00:00:00Z",
    "source_kind": "gbrain_page"
  }]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildGbrainExportManifest(path, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Items) != 1 || manifest.Items[0].IndexState != "unindexed" {
		t.Fatalf("items = %+v", manifest.Items)
	}
	if manifest.Items[0].BodyPolicy != "db_export" {
		t.Fatalf("body policy = %q, want db_export", manifest.Items[0].BodyPolicy)
	}
	if manifest.Items[0].Embedding != nil || len(manifest.Items[0].Chunks) != 0 {
		t.Fatalf("unindexed page grew index metadata: %+v", manifest.Items[0])
	}
	home := t.TempDir()
	if _, err := RecordSourceManifest(home, manifest); err != nil {
		t.Fatal(err)
	}
	doctor, err := DoctorSourceManifest(home, GbrainDBExportManifestFamily)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" {
		t.Fatalf("doctor = %+v", doctor)
	}
}

func TestGbrainExportRejectsMissingChunkOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-chunks.json")
	body := `{
  "schema_version": "compound.gbrain_db_export.v0.1",
  "db_host": "db.example.invalid",
  "db_name": "gbrain_synthetic",
  "dump_ref": "sha256:dump",
  "export_time": "2026-07-09T12:00:00Z",
  "generation_label": "current-2026-07-09",
  "pages": [{
    "page_slug": "x",
    "db_only": true,
    "page_hash": "sha256:dead",
    "chunks": [],
    "embedding": {"provider": "ollama", "model": "nomic-embed-text", "dims": 768},
    "corpus_generation": "current-2026-07-09",
    "imported_at": "2026-07-09T12:00:00Z",
    "source_kind": "gbrain_page"
  }]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildGbrainExportManifest(path, time.Now().UTC()); err == nil {
		t.Fatal("expected empty chunks to fail")
	}
}

func TestGbrainExportRejectsPageHashChunkMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mismatch.json")
	body := `{
  "schema_version": "compound.gbrain_db_export.v0.1",
  "db_host": "db.example.invalid",
  "db_name": "gbrain_synthetic",
  "dump_ref": "sha256:dump",
  "export_time": "2026-07-09T12:00:00Z",
  "generation_label": "current-2026-07-09",
  "pages": [{
    "page_slug": "x",
    "db_only": true,
    "page_hash": "sha256:WRONG_PAGE_HASH",
    "chunks": [{"ordinal": 0, "chunk_hash": "sha256:chunk_bbb111"}],
    "embedding": {"provider": "ollama", "model": "nomic-embed-text", "dims": 768},
    "corpus_generation": "current-2026-07-09",
    "imported_at": "2026-07-09T12:00:00Z",
    "source_kind": "gbrain_page"
  }]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildGbrainExportManifest(path, time.Now().UTC()); err == nil {
		t.Fatal("expected page_hash/chunk mismatch to fail at build")
	}
}

func TestGbrainDoctorFailsOnTamperedChunkSet(t *testing.T) {
	exportPath := filepath.Join("..", "..", "fixtures", "sourceobs", "gbrain-db-export-synthetic.json")
	manifest, err := BuildGbrainExportManifest(exportPath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Plant a mismatch after build (simulates corrupted registry write).
	manifest.Items[0].PageHash = "sha256:tampered"
	manifest.Items[0].FileHash = "sha256:tampered"
	home := t.TempDir()
	// Bypass Build validation by writing directly.
	path := SourceManifestPath(home, GbrainDBExportManifestFamily)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	doctor, err := DoctorSourceManifest(home, GbrainDBExportManifestFamily)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "fail" || doctor.HashMismatchCount < 1 {
		t.Fatalf("expected doctor fail on chunk set mismatch, got %+v", doctor)
	}
}
