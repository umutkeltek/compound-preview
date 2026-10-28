package runattempt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSchemaChangelogNamesCurrentWriterVersionsAndChangeClass(t *testing.T) {
	body, err := os.ReadFile("SCHEMA.md")
	if err != nil {
		t.Fatalf("read SCHEMA.md: %v", err)
	}
	doc := string(body)
	for _, version := range []string{SchemaVersion, TerminalizationIntentSchemaVersion} {
		if !strings.Contains(doc, "`"+version+"`") {
			t.Fatalf("SCHEMA.md must document writer schema %q", version)
		}
	}
	if !strings.Contains(strings.ToLower(doc), "minor") {
		t.Fatal("SCHEMA.md must name a MINOR change class")
	}
}

func TestAcceptedSchemaVersionsIncludeCurrentAndV01(t *testing.T) {
	for _, version := range []string{SchemaVersion, LegacySchemaVersionV01} {
		if !AcceptsSchemaVersion(version) {
			t.Fatalf("RunAttempt read window does not accept %q", version)
		}
	}
	if AcceptsSchemaVersion("compound.runattempt.v0.99") {
		t.Fatal("RunAttempt read window accepted an unknown future schema")
	}
}

func TestStoreReadsV01ButRefusesUnknownSchema(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "run_attempts"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 13, 13, 0, 0, 0, time.UTC)
	id := MintDeterministic("legacy-v01-read-window", now)
	write := func(version string) {
		t.Helper()
		body, err := json.Marshal(RunAttempt{
			SchemaVersion: version,
			ID:            id,
			WorkID:        "work_legacy_v01",
			Actor:         "codex",
			Harness:       HarnessPlaceholder,
			Status:        StatusPending,
			CreatedAt:     now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "run_attempts", id+".json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(LegacySchemaVersionV01)
	legacy, err := Open(home).Get(id)
	if err != nil {
		t.Fatalf("read accepted %s RunAttempt: %v", LegacySchemaVersionV01, err)
	}
	if legacy.SchemaVersion != LegacySchemaVersionV01 {
		t.Fatalf("legacy schema rewritten on read: got %q want %q", legacy.SchemaVersion, LegacySchemaVersionV01)
	}

	write("compound.runattempt.v0.99")
	if _, err := Open(home).Get(id); err == nil || !strings.Contains(err.Error(), "unsupported schema_version") {
		t.Fatalf("unknown RunAttempt schema read error = %v, want unsupported schema_version", err)
	}
}
