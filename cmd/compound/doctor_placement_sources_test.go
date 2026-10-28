package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/policy"
)

// writePlacementSourceAuthority writes an append-only import history into homePath,
// mirroring the on-disk shape `policy import-placement-rules` produces.
func writePlacementSourceAuthority(t *testing.T, homePath string, recs ...policy.PlacementSourceAuthorityRecord) {
	t.Helper()
	path := policy.PlacementSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir registries: %v", err)
	}
	var buf bytes.Buffer
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write source authority: %v", err)
	}
}

func placementRecord(sourcePath, createdAt, revision string) policy.PlacementSourceAuthorityRecord {
	return policy.PlacementSourceAuthorityRecord{
		SchemaVersion:   policy.PlacementSourceAuthoritySchema,
		ID:              "plsrc_" + createdAt,
		CreatedAt:       createdAt,
		SourcePath:      sourcePath,
		SourceRevision:  revision,
		AuthorityStatus: "compound_primary",
		SourceRole:      "evidence_only_input_after_import",
	}
}

func writeSourceView(t *testing.T, body string) (path, revision string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "placement-rules.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write source view: %v", err)
	}
	return path, "sha256:" + policy.DigestHex([]byte(body))
}

func TestPlacementSourceFreshnessCurrentWhenRevisionMatches(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sourcePath, revision := writeSourceView(t, `{"owners":[{"id":"storage-taxonomy"}]}`)
	writePlacementSourceAuthority(t, home, placementRecord(sourcePath, "2026-07-10T00:29:24Z", revision))

	rows := buildPlacementSourceFreshness(home)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].State != "current" {
		t.Fatalf("want state current, got %q (imported %s, live %s)",
			rows[0].State, rows[0].ImportedRevision, rows[0].LiveRevision)
	}
}

func TestPlacementImportKeepsFreshnessStableAcrossWorkingDirectories(t *testing.T) {
	home := t.TempDir()
	sourceDir := t.TempDir()
	sourceName := "placement-rules.json"
	sourcePath := filepath.Join(sourceDir, sourceName)
	if err := os.WriteFile(sourcePath, []byte(`{"owners":[{"id":"fixture"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(sourceDir)
	report, err := policy.ImportPlacementRules(home, sourceName, cliTestNow)
	if err != nil {
		t.Fatalf("import relative placement source: %v", err)
	}
	if report.SourceAuthority.SourcePath != sourcePath {
		t.Fatalf("stored source path = %q, want stable absolute path %q", report.SourceAuthority.SourcePath, sourcePath)
	}

	t.Chdir(t.TempDir())
	rows := buildPlacementSourceFreshness(home)
	if len(rows) != 1 || rows[0].State != "current" {
		t.Fatalf("freshness from another cwd = %+v, want one current source", rows)
	}
}

// REGRESSION: the authority file is an append-only history — one entry PER IMPORT, so a
// a single source path can appear many times.
// Selecting the first entry compares today's file against the OLDEST import and reports
// drift on a correctly-enforced source.
func TestPlacementSourceFreshnessUsesNewestImportNotFirst(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sourcePath, revision := writeSourceView(t, `{"owners":[{"id":"storage-taxonomy"}]}`)
	writePlacementSourceAuthority(t, home,
		placementRecord(sourcePath, "2026-06-30T19:18:20Z", "sha256:oldest-import"),
		placementRecord(sourcePath, "2026-07-01T08:43:03Z", "sha256:middle-import"),
		placementRecord(sourcePath, "2026-07-10T00:29:24Z", revision),
	)

	rows := buildPlacementSourceFreshness(home)
	if len(rows) != 1 {
		t.Fatalf("history must collapse to one row per path, got %d", len(rows))
	}
	if rows[0].State != "current" {
		t.Fatalf("newest import matches the file, want current, got %q", rows[0].State)
	}
	if rows[0].ImportedAt != "2026-07-10T00:29:24Z" {
		t.Fatalf("want the newest import, got %q", rows[0].ImportedAt)
	}
}

func TestPlacementSourceFreshnessStaleWhenSourceMovedOn(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sourcePath, _ := writeSourceView(t, `{"owners":[{"id":"storage-taxonomy"}]}`)
	writePlacementSourceAuthority(t, home,
		placementRecord(sourcePath, "2026-07-10T00:29:24Z", "sha256:what-compound-still-enforces"))

	rows := buildPlacementSourceFreshness(home)
	if len(rows) != 1 || rows[0].State != "stale" {
		t.Fatalf("want one stale row, got %+v", rows)
	}

	// A stale row must actually reach the operator, and must name both revisions —
	// a bare "stale" with no numbers cannot be acted on.
	var out bytes.Buffer
	printPlacementSourceFreshness(&out, doctorReport{PlacementSources: rows})
	text := out.String()
	for _, want := range []string{"source drift", sourcePath, "what-compound-still-enforces", "import-placement-rules"} {
		if !bytes.Contains([]byte(text), []byte(want)) {
			t.Fatalf("stale output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPlacementSourceFreshnessFlagsMissingSource(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	missing := filepath.Join(t.TempDir(), "deleted-placement-rules.json")
	writePlacementSourceAuthority(t, home,
		placementRecord(missing, "2026-07-10T00:29:24Z", "sha256:still-enforced"))

	rows := buildPlacementSourceFreshness(home)
	if len(rows) != 1 || rows[0].State != "source-missing" {
		t.Fatalf("want one source-missing row, got %+v", rows)
	}
	// Enforcement continues from the imported copy even with the source gone; the operator
	// needs to know that rather than assume deleting the view disabled the rules.
	var out bytes.Buffer
	printPlacementSourceFreshness(&out, doctorReport{PlacementSources: rows})
	if !bytes.Contains(out.Bytes(), []byte("still enforcing")) {
		t.Fatalf("missing-source output must say enforcement continues, got:\n%s", out.String())
	}
}

// A health surface that prints a wall of green trains the operator to skip it, so the
// all-current case collapses to one line and never enumerates.
func TestPlacementSourceFreshnessStaysQuietWhenAllCurrent(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sourcePath, revision := writeSourceView(t, `{"owners":[]}`)
	writePlacementSourceAuthority(t, home, placementRecord(sourcePath, "2026-07-10T00:29:24Z", revision))

	var out bytes.Buffer
	printPlacementSourceFreshness(&out, doctorReport{PlacementSources: buildPlacementSourceFreshness(home)})
	text := out.String()
	if bytes.Contains([]byte(text), []byte(sourcePath)) {
		t.Fatalf("all-current output must not enumerate paths, got:\n%s", text)
	}
	if !bytes.Contains([]byte(text), []byte("all current")) {
		t.Fatalf("want a one-line all-current summary, got:\n%s", text)
	}
}

func TestPlacementSourceFreshnessAbsentWhenNothingImported(t *testing.T) {
	t.Parallel()
	if rows := buildPlacementSourceFreshness(t.TempDir()); rows != nil {
		t.Fatalf("no import history must yield no rows, got %+v", rows)
	}
	var out bytes.Buffer
	printPlacementSourceFreshness(&out, doctorReport{})
	if out.Len() != 0 {
		t.Fatalf("no rows must print nothing, got:\n%s", out.String())
	}
}
