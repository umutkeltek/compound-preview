package authority

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportInfraDatabaseRegistryWritesAuthorityAndSourceRecord(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "database.yaml")
	writeInfraDatabaseFixture(t, source)
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	report, err := ImportInfraDatabaseRegistry(home, source, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.AuthorityRef != InfraDatabaseAuthorityRef || report.EntryCount != 2 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.TopLevelKeys) == 0 {
		t.Fatalf("report should carry top_level_keys: %+v", report)
	}

	installed, err := os.ReadFile(InfraDatabaseRegistryPath(home))
	if err != nil {
		t.Fatal(err)
	}
	sourceBody, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != string(sourceBody) {
		t.Fatalf("installed registry should be a verbatim copy of the source YAML")
	}

	records, err := LoadInfraDatabaseSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	rec := records[0]
	if rec.AuthorityRef != InfraDatabaseAuthorityRef || rec.SourcePath != source || rec.SourceRevision == "" {
		t.Fatalf("source authority record = %+v", rec)
	}
	if rec.AuthorityStatus != "compound_primary" {
		t.Fatalf("authority_status = %q, want compound_primary", rec.AuthorityStatus)
	}
	if !strings.Contains(rec.RetirementCondition, "source YAML remains evidence/rollback input") {
		t.Fatalf("retirement_condition missing evidence/rollback wording: %q", rec.RetirementCondition)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("infra database import must not promote claims, stat err=%v", err)
	}

	// Re-importing (e.g. after a real source edit) appends a new ledger
	// record rather than overwriting history, matching the append-only
	// ledger convention used by every other import door in this package.
	if _, err := ImportInfraDatabaseRegistry(home, source, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	records, err = LoadInfraDatabaseSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("re-import should append, got %d records", len(records))
	}
}

func TestImportInfraServicesRegistryWritesAuthorityAndSourceRecord(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "services.yaml")
	writeInfraServicesFixture(t, source)
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	report, err := ImportInfraServicesRegistry(home, source, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.AuthorityRef != InfraServicesAuthorityRef || report.EntryCount != 2 {
		t.Fatalf("report = %+v", report)
	}

	if _, err := os.Stat(InfraServicesRegistryPath(home)); err != nil {
		t.Fatalf("installed services registry: %v", err)
	}
	records, err := LoadInfraServicesSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].AuthorityRef != InfraServicesAuthorityRef {
		t.Fatalf("records = %+v", records)
	}
}

func TestImportInfraRegistryRejectsMissingRequiredKey(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "database.yaml")
	if err := os.WriteFile(source, []byte("authority:\n  primary_postgres: example-db.internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportInfraDatabaseRegistry(home, source, time.Now()); err == nil {
		t.Fatal("expected error for missing required top-level key")
	} else if !strings.Contains(err.Error(), "databases") {
		t.Fatalf("error should name the missing key, got %v", err)
	}
	if _, err := os.Stat(InfraDatabaseRegistryPath(home)); !os.IsNotExist(err) {
		t.Fatalf("rejected import must not install a registry file, stat err=%v", err)
	}
}

func TestImportInfraRegistryRejectsEmptyRequiredSection(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "services.yaml")
	if err := os.WriteFile(source, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportInfraServicesRegistry(home, source, time.Now()); err == nil {
		t.Fatal("expected error for empty required section")
	}
}

func writeInfraDatabaseFixture(t *testing.T, path string) {
	t.Helper()
	body := strings.Join([]string{
		"authority:",
		"  primary_postgres: example-db.internal",
		"databases:",
		"  ops:",
		"    purpose: controller_jobs",
		"    status: active",
		"  knowledge:",
		"    purpose: crawl_metadata",
		"    status: provisioned",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeInfraServicesFixture(t *testing.T, path string) {
	t.Helper()
	body := strings.Join([]string{
		"services:",
		"  example-a.internal:",
		"    role: example_role_a",
		"    authority: example_authority_a",
		"  example-b.internal:",
		"    role: example_role_b",
		"    authority: example_authority_b",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
