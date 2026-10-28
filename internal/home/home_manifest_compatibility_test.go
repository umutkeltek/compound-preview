package home

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitWritesFixedManifestIdentityWithoutNormalizingHomeProfile(t *testing.T) {
	for _, kind := range []string{HomeKindGlobal, HomeKindLocal, homeKindTest, "local_monolith", "experiential-smoke"} {
		t.Run(kind, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := Init(homePath, kind, time.Unix(0, 0)); err != nil {
				t.Fatal(err)
			}
			manifest, err := LoadManifest(homePath)
			if err != nil || manifest.SchemaVersion != SchemaVersion || manifest.HomeKind != kind {
				t.Fatalf("LoadManifest = %+v, %v", manifest, err)
			}
			raw, err := os.ReadFile(filepath.Join(homePath, ManifestName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte("kind: compound_home\n")) {
				t.Fatalf("fresh manifest lacks fixed kind identity:\n%s", raw)
			}
			if !bytes.Contains(raw, []byte("home_kind: "+kind+"\n")) {
				t.Fatalf("fresh manifest normalized home profile %q:\n%s", kind, raw)
			}
		})
	}
}

func TestLegacyV01ManifestWithoutKindRemainsReadableAndBytePreserved(t *testing.T) {
	for _, profile := range []string{"local_monolith", "canonical-claude-hook-test"} {
		t.Run(profile, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := os.Mkdir(homePath, 0o700); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(homePath, ManifestName)
			legacy := []byte("schema_version: " + SchemaVersion + "\nhome_kind: " + profile + "\ncreated_at: 2026-01-01T00:00:00Z\nactive_modules: []\n")
			if err := os.WriteFile(manifestPath, legacy, 0o644); err != nil {
				t.Fatal(err)
			}

			manifest, err := LoadManifest(homePath)
			if err != nil {
				t.Fatalf("LoadManifest legacy v0.1: %v", err)
			}
			if manifest.SchemaVersion != SchemaVersion || manifest.HomeKind != profile {
				t.Fatalf("legacy manifest = %+v", manifest)
			}
			if err := Init(homePath, profile, time.Unix(1, 0)); err != nil {
				t.Fatalf("Init must scaffold a compatible legacy v0.1 home without rewriting identity: %v", err)
			}
			after, err := os.ReadFile(manifestPath)
			if err != nil || !bytes.Equal(after, legacy) {
				t.Fatalf("legacy manifest rewritten: body=%q err=%v", after, err)
			}
			if report := Doctor(homePath); !report.OK {
				t.Fatalf("Doctor rejected compatible legacy v0.1 home: %+v", report)
			}
		})
	}
}

func TestHomeSchemaChangelogCoversCurrentCompatibilityContract(t *testing.T) {
	raw, err := os.ReadFile("SCHEMA.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, want := range []string{SchemaVersion, "global", "local", "test", "backup", "restore", "no automatic migration"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("SCHEMA.md missing %q", want)
		}
	}
}

func TestInitRejectsBlankHomeProfileBeforeMutation(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	err := Init(homePath, "   ", time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "home kind is required") {
		t.Fatalf("Init blank profile error = %v", err)
	}
	if _, statErr := os.Lstat(homePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("blank profile mutated home path: %v", statErr)
	}
}

func TestInitRejectsIncompatibleExistingManifestBeforeScaffolding(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		match    string
	}{
		{name: "missing schema", manifest: "kind: compound_home\nhome_kind: local\n", match: "schema_version is required"},
		{name: "unsupported schema", manifest: "schema_version: compound.home.v9.9\nkind: compound_home\nhome_kind: local\n", match: "unsupported home schema"},
		{name: "missing fixed kind without legacy profile", manifest: "schema_version: compound.home.v0.1\n", match: "kind is required"},
		{name: "unsupported fixed kind", manifest: "schema_version: compound.home.v0.1\nkind: shared_server\nhome_kind: local\n", match: "manifest kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := os.Mkdir(homePath, 0o700); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(homePath, ManifestName)
			before := []byte(tc.manifest)
			if err := os.WriteFile(manifestPath, before, 0o644); err != nil {
				t.Fatal(err)
			}

			err := Init(homePath, "local", time.Unix(0, 0))
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("Init incompatible manifest error = %v, want %q", err, tc.match)
			}
			assertManifestRecoveryGuidance(t, err)
			after, readErr := os.ReadFile(manifestPath)
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("manifest mutated: body=%q err=%v", after, readErr)
			}
			for _, rel := range HabitatDirs() {
				if _, statErr := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel))); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("incompatible manifest created habitat %s: %v", rel, statErr)
				}
			}
		})
	}
}

func TestInitRevalidatesManifestThroughPinnedRootBeforeScaffolding(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(homePath, ManifestName)
	if err := os.WriteFile(manifestPath, []byte("schema_version: compound.home.v0.1\nhome_kind: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHook := initAfterRootSecured
	t.Cleanup(func() { initAfterRootSecured = oldHook })
	replacement := []byte("schema_version: compound.home.v9.9\nhome_kind: local\n")
	initAfterRootSecured = func(string) error {
		return os.WriteFile(manifestPath, replacement, 0o644)
	}

	err := Init(homePath, "local", time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "unsupported home schema") {
		t.Fatalf("Init rooted manifest revalidation error = %v", err)
	}
	assertManifestRecoveryGuidance(t, err)
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || !bytes.Equal(after, replacement) {
		t.Fatalf("rooted revalidation mutated incompatible manifest: body=%q err=%v", after, readErr)
	}
	for _, rel := range HabitatDirs() {
		if _, statErr := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rooted revalidation failure created habitat %s: %v", rel, statErr)
		}
	}
}

func TestInitDoesNotRecreatePreflightManifestRemovedAfterRootPin(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(homePath, ManifestName)
	if err := os.WriteFile(manifestPath, []byte("schema_version: compound.home.v0.1\nhome_kind: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHook := initAfterRootSecured
	t.Cleanup(func() { initAfterRootSecured = oldHook })
	initAfterRootSecured = func(string) error { return os.Remove(manifestPath) }

	err := Init(homePath, HomeKindLocal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "manifest.yaml") {
		t.Fatalf("Init removed-manifest race error = %v", err)
	}
	if _, statErr := os.Lstat(manifestPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init recreated removed preflight manifest: %v", statErr)
	}
	for _, rel := range HabitatDirs() {
		if _, statErr := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("removed preflight manifest created habitat %s: %v", rel, statErr)
		}
	}
}

func TestInitRefusesManifestThatAppearsAfterMissingPreflight(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	manifestPath := filepath.Join(homePath, ManifestName)
	oldHook := initAfterRootSecured
	t.Cleanup(func() { initAfterRootSecured = oldHook })
	initAfterRootSecured = func(string) error {
		return os.WriteFile(manifestPath, []byte("schema_version: compound.home.v0.1\nhome_kind: global\n"), 0o644)
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "appeared after initialization preflight") {
		t.Fatalf("Init appeared-manifest race error = %v", err)
	}
	for _, rel := range HabitatDirs() {
		if _, statErr := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("appeared manifest created habitat %s: %v", rel, statErr)
		}
	}
}

func TestInitRefusesExistingHomeKindMismatchWithoutMutation(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, HomeKindLocal, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(homePath, ManifestName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	err = Init(homePath, HomeKindGlobal, time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "does not match requested kind") {
		t.Fatalf("Init kind mismatch error = %v", err)
	}
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("kind mismatch mutated manifest: body=%q err=%v", after, readErr)
	}
}

func TestInitDoesNotAdoptPopulatedManifestlessDirectory(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(homePath, "operator-data")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "populated path has no manifest") {
		t.Fatalf("Init manifestless populated directory error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init adopted manifestless directory: %v", statErr)
	}
	got, readErr := os.ReadFile(marker)
	if readErr != nil || string(got) != "keep" {
		t.Fatalf("manifestless directory marker changed: body=%q err=%v", got, readErr)
	}
}

func TestInitRollsBackFreshManifestWhenRootIsPopulatedBeforePublication(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	markerBody := []byte("foreign content\n")
	oldBeforePublish := initManifestBeforePublish
	t.Cleanup(func() { initManifestBeforePublish = oldBeforePublish })
	initManifestBeforePublish = func(root *os.Root, _ string, _ []byte) error {
		marker, err := root.OpenFile("foreign-marker", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := marker.Write(markerBody); err != nil {
			_ = marker.Close()
			return err
		}
		return errors.Join(marker.Sync(), marker.Close())
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "populated during initialization") {
		t.Fatalf("Init error = %v, want concurrent-population refusal", err)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init adopted concurrently populated root: %v", statErr)
	}
	got, readErr := os.ReadFile(filepath.Join(homePath, "foreign-marker"))
	if readErr != nil || !bytes.Equal(got, markerBody) {
		t.Fatalf("foreign marker changed: body=%q err=%v", got, readErr)
	}
	entries, readDirErr := os.ReadDir(homePath)
	if readDirErr != nil || len(entries) != 1 || entries[0].Name() != "foreign-marker" {
		t.Fatalf("failed init left non-foreign entries: entries=%v err=%v", entries, readDirErr)
	}
}

func TestLoadManifestAndDoctorRejectIncompatibleManifestWithRecoveryGuidance(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		match    string
	}{
		{name: "missing schema", manifest: "kind: compound_home\nhome_kind: local\n", match: "schema_version is required"},
		{name: "unsupported schema", manifest: "schema_version: compound.home.v9.9\nkind: compound_home\nhome_kind: local\n", match: "unsupported home schema"},
		{name: "missing fixed kind without legacy profile", manifest: "schema_version: compound.home.v0.1\n", match: "kind is required"},
		{name: "unsupported fixed kind", manifest: "schema_version: compound.home.v0.1\nkind: shared_server\nhome_kind: local\n", match: "manifest kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := os.Mkdir(homePath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(homePath, ManifestName), []byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := LoadManifest(homePath); err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("LoadManifest error = %v, want %q", err, tc.match)
			}
			report := Doctor(homePath)
			if report.ManifestOK || report.OK || !strings.Contains(report.ManifestError, tc.match) {
				t.Fatalf("Doctor incompatible manifest = %+v, want %q", report, tc.match)
			}
			guidance := strings.ToLower(report.ManifestRecovery)
			for _, want := range []string{"before migration", "backup", "verify", "preserve", "roll back", "restore", "new-absent-home-path", "will not rewrite"} {
				if !strings.Contains(guidance, want) {
					t.Fatalf("Doctor recovery guidance missing %q: %q", want, report.ManifestRecovery)
				}
			}
		})
	}
}

func TestRestoreArchiveReusesHomeManifestValidator(t *testing.T) {
	tests := []struct {
		name         string
		manifestBody []byte
		match        string
	}{
		{
			name:         "unsupported fixed identity kind",
			manifestBody: []byte("schema_version: " + SchemaVersion + "\nkind: compound_server\nhome_kind: shared-server-profile\ncreated_at: 2026-01-01T00:00:00Z\nactive_modules: []\n"),
			match:        "manifest kind",
		},
		{
			name:         "strict decoder rejects unknown identity field",
			manifestBody: []byte("schema_version: " + SchemaVersion + "\nhome_kind: local\nunknown_identity: true\n"),
			match:        "unknown_identity",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			digest := sha256.Sum256(tc.manifestBody)
			archivePath := writeRestoreTestArchive(t,
				[]restoreTestEntry{{name: ManifestName, body: tc.manifestBody}},
				[]ArchiveEntry{{Path: ManifestName, Size: int64(len(tc.manifestBody)), SHA256: fmt.Sprintf("%x", digest[:])}},
			)
			target := filepath.Join(t.TempDir(), ".compound-restored")
			_, err := RestoreArchive(archivePath, target, archiveTestNow)
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("RestoreArchive incompatible home manifest error = %v, want %q", err, tc.match)
			}
			assertManifestRecoveryGuidance(t, err)
			if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("incompatible restored home was published: %v", statErr)
			}
		})
	}
}

func TestVerifyArchiveRejectsIncompatibleEmbeddedManifestWhenEnvelopeClaimsCurrent(t *testing.T) {
	tests := []struct {
		name         string
		manifestBody []byte
		match        string
	}{
		{
			name:         "unsupported embedded schema",
			manifestBody: []byte("schema_version: compound.home.v9.9\nkind: compound_home\nhome_kind: global\n"),
			match:        "unsupported home schema",
		},
		{
			name:         "unsupported embedded kind",
			manifestBody: []byte("schema_version: " + SchemaVersion + "\nkind: compound_server\nhome_kind: global\n"),
			match:        "manifest kind",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			digest := sha256.Sum256(tc.manifestBody)
			archivePath := writeRestoreTestArchive(t,
				[]restoreTestEntry{{name: ManifestName, body: tc.manifestBody}},
				[]ArchiveEntry{{Path: ManifestName, Size: int64(len(tc.manifestBody)), SHA256: fmt.Sprintf("%x", digest[:])}},
			)
			if _, err := VerifyArchive(archivePath); err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("VerifyArchive embedded manifest error = %v, want %q", err, tc.match)
			} else {
				assertTypedCompatibilityFailure(t, err, "home_manifest")
			}
		})
	}
}

func TestVerifyArchiveEnvelopeIdentityFailuresAreTyped(t *testing.T) {
	manifestBody := []byte("schema_version: " + SchemaVersion + "\nkind: compound_home\nhome_kind: global\n")
	digest := sha256.Sum256(manifestBody)
	for _, tc := range []struct {
		name          string
		archiveSchema string
		archiveKind   string
		match         string
	}{
		{name: "unsupported schema", archiveSchema: "compound.home_archive.v9.9", archiveKind: ArchiveKindBackup, match: "archive manifest schema_version"},
		{name: "unsupported kind", archiveSchema: ArchiveSchemaVersion, archiveKind: "snapshot", match: "archive kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := ArchiveManifest{
				SchemaVersion:     tc.archiveSchema,
				Kind:              tc.archiveKind,
				CreatedAt:         archiveTestNow.UTC().Format(time.RFC3339),
				HomeSchemaVersion: SchemaVersion,
				Entries: []ArchiveEntry{{
					Path: ManifestName, Size: int64(len(manifestBody)), SHA256: fmt.Sprintf("%x", digest[:]),
				}},
			}
			envelopeRaw, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			archivePath := writeRestoreRawTarGz(t, []restoreRawTarEntry{
				{hdr: &tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Mode: 0o600}, body: manifestBody},
				{hdr: &tar.Header{Name: ArchiveManifestName, Typeflag: tar.TypeReg, Mode: 0o600}, body: envelopeRaw},
			})
			if _, err := VerifyArchive(archivePath); err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("VerifyArchive envelope error = %v, want %q", err, tc.match)
			} else {
				assertTypedCompatibilityFailure(t, err, "archive_envelope")
			}
		})
	}
}

func TestBackupPinsExactManifestBytesBeforeWalkingHome(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "historical-profile", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(homePath, ManifestName)
	pinned, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte("schema_version: " + SchemaVersion + "\nkind: " + ManifestKind + "\nhome_kind: raced-profile\ncreated_at: 2026-01-01T00:00:00Z\nactive_modules: []\n")
	oldAfterPinned := archiveAfterHomeManifestPinned
	t.Cleanup(func() { archiveAfterHomeManifestPinned = oldAfterPinned })
	archiveAfterHomeManifestPinned = func(gotPath string) error {
		if gotPath != manifestPath {
			t.Fatalf("manifest pin seam path = %q, want %q", gotPath, manifestPath)
		}
		return os.WriteFile(manifestPath, replacement, 0o644)
	}

	archivePath := filepath.Join(t.TempDir(), "home.tar.gz")
	if _, err := Backup(homePath, archivePath, archiveTestNow); err != nil {
		t.Fatalf("Backup with manifest pathname replacement: %v", err)
	}
	manifest, _, err := readArchive(archivePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest.embeddedHomeManifest, pinned) {
		t.Fatalf("archive manifest bytes followed raced pathname:\n got: %q\nwant: %q", manifest.embeddedHomeManifest, pinned)
	}
	if bytes.Equal(manifest.embeddedHomeManifest, replacement) {
		t.Fatal("archive captured the post-pin replacement manifest")
	}
	if _, err := VerifyArchive(archivePath); err != nil {
		t.Fatalf("VerifyArchive pinned backup: %v", err)
	}
}

func TestRestoreArchiveHomeSchemaMismatchUsesArchiveRecoveryGuidance(t *testing.T) {
	manifestBody := []byte("schema_version: " + SchemaVersion + "\nhome_kind: global\ncreated_at: 2026-01-01T00:00:00Z\nactive_modules: []\n")
	digest := sha256.Sum256(manifestBody)
	archiveManifest := ArchiveManifest{
		SchemaVersion:     ArchiveSchemaVersion,
		Kind:              ArchiveKindBackup,
		CreatedAt:         archiveTestNow.UTC().Format(time.RFC3339),
		HomeSchemaVersion: "compound.home.v9.9",
		Entries: []ArchiveEntry{{
			Path:   ManifestName,
			Size:   int64(len(manifestBody)),
			SHA256: fmt.Sprintf("%x", digest[:]),
		}},
	}
	archiveManifestRaw, err := json.Marshal(archiveManifest)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := writeRestoreRawTarGz(t, []restoreRawTarEntry{
		{hdr: &tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Mode: 0o600}, body: manifestBody},
		{hdr: &tar.Header{Name: ArchiveManifestName, Typeflag: tar.TypeReg, Mode: 0o600}, body: archiveManifestRaw},
	})
	target := filepath.Join(t.TempDir(), ".compound-restored")

	_, err = RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "archive home_schema_version must be") || !strings.Contains(err.Error(), "compound.home.v9.9") {
		t.Fatalf("RestoreArchive home schema mismatch error = %v", err)
	}
	assertTypedCompatibilityFailure(t, err, "archive_envelope")
	guidance := strings.ToLower(err.Error())
	for _, want := range []string{"preserve the archive unchanged", "compound home verify <archive>", "restore only to a new absent target", "will not rewrite the archive"} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("archive recovery guidance missing %q: %v", want, err)
		}
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("home-schema-incompatible archive published target: %v", statErr)
	}
}

func TestManifestRecoveryGuidanceIsLimitedToCompatibilityFailures(t *testing.T) {
	nonHomePath := filepath.Join(t.TempDir(), "ordinary-file")
	if err := os.WriteFile(nonHomePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(nonHomePath, HomeKindGlobal, time.Unix(0, 0)); err == nil {
		t.Fatal("Init unexpectedly accepted an ordinary file as a home")
	} else {
		assertNoManifestRecoveryGuidance(t, err.Error())
	}

	missingHome := filepath.Join(t.TempDir(), ".compound-missing")
	report := Doctor(missingHome)
	if report.ManifestError == "" {
		t.Fatalf("Doctor missing home report = %+v, want manifest read error", report)
	}
	if report.ManifestRecovery != "" {
		t.Fatalf("Doctor added compatibility recovery guidance to I/O failure: %q", report.ManifestRecovery)
	}

	missingArchive := filepath.Join(t.TempDir(), "missing.tar.gz")
	target := filepath.Join(t.TempDir(), ".compound-restored")
	if _, err := RestoreArchive(missingArchive, target, archiveTestNow); err == nil {
		t.Fatal("RestoreArchive unexpectedly accepted a missing archive")
	} else {
		assertNoManifestRecoveryGuidance(t, err.Error())
	}
}

func assertManifestRecoveryGuidance(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected manifest compatibility error")
	}
	assertTypedCompatibilityFailure(t, err, "")
	guidance := strings.ToLower(err.Error())
	for _, want := range []string{
		"compound home backup --target <archive>",
		"compound home verify <archive>",
		"preserve the original home",
		"roll back",
		"compound home restore <archive> --target <new-absent-home-path>",
		"will not rewrite manifest.yaml",
	} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("manifest recovery guidance missing %q: %v", want, err)
		}
	}
}

func assertTypedCompatibilityFailure(t *testing.T, err error, wantSurface string) {
	t.Helper()
	var recovery interface {
		CompatibilitySurface() string
		RecoveryGuidance() string
	}
	if !errors.As(err, &recovery) {
		t.Fatalf("compatibility failure lacks typed recovery contract: %T %v", err, err)
	}
	if wantSurface != "" && recovery.CompatibilitySurface() != wantSurface {
		t.Fatalf("compatibility surface = %q, want %q", recovery.CompatibilitySurface(), wantSurface)
	}
	if strings.TrimSpace(recovery.RecoveryGuidance()) == "" {
		t.Fatalf("compatibility failure has empty recovery guidance: %v", err)
	}
}

func assertNoManifestRecoveryGuidance(t *testing.T, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, unexpected := range []string{"compound home backup --target <archive>", "new-absent-home-path", "will not rewrite manifest.yaml"} {
		if strings.Contains(lower, unexpected) {
			t.Fatalf("non-compatibility error included manifest recovery guidance %q: %s", unexpected, text)
		}
	}
}

func TestInitManifestShortWriteDoesNotPublishAndRetrySucceeds(t *testing.T) {
	exerciseInitManifestPublicationFailure(t, func() {
		initManifestFileWrite = func(file *os.File, body []byte) (int, error) {
			return file.Write(body[:len(body)/2])
		}
	})
}

func TestInitManifestWriteFailureDoesNotPublishAndRetrySucceeds(t *testing.T) {
	exerciseInitManifestPublicationFailure(t, func() {
		initManifestFileWrite = func(*os.File, []byte) (int, error) {
			return 0, errors.New("injected manifest write failure")
		}
	})
}

func TestInitManifestFileSyncFailureDoesNotPublishAndRetrySucceeds(t *testing.T) {
	exerciseInitManifestPublicationFailure(t, func() {
		initManifestFileSync = func(*os.File) error {
			return errors.New("injected manifest sync failure")
		}
	})
}

func TestInitManifestCloseFailureDoesNotPublishAndRetrySucceeds(t *testing.T) {
	exerciseInitManifestPublicationFailure(t, func() {
		initManifestFileClose = func(file *os.File) error {
			if err := file.Close(); err != nil {
				return err
			}
			return errors.New("injected manifest close failure")
		}
	})
}

func TestInitManifestPrePublicationFailureDoesNotPublishAndRetrySucceeds(t *testing.T) {
	exerciseInitManifestPublicationFailure(t, func() {
		initManifestBeforePublish = func(*os.Root, string, []byte) error {
			return errors.New("injected manifest pre-publication failure")
		}
	})
}

func TestInitManifestModeDriftBeforePublicationDoesNotPublishAndRetrySucceeds(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	oldBeforePublish := initManifestBeforePublish
	t.Cleanup(func() { initManifestBeforePublish = oldBeforePublish })
	initManifestBeforePublish = func(root *os.Root, tempName string, _ []byte) error {
		return root.Chmod(tempName, 0o666)
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("Init staging mode drift error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init published permissive manifest: %v", statErr)
	}
	entries, readErr := os.ReadDir(homePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if isManifestInitStagingName(entry.Name()) {
			t.Fatalf("mode-drift refusal left staging residue %q", entry.Name())
		}
	}

	initManifestBeforePublish = oldBeforePublish
	if err := Init(homePath, HomeKindGlobal, time.Unix(1, 0)); err != nil {
		t.Fatalf("Init retry after staging mode drift: %v", err)
	}
	manifestInfo, statErr := os.Lstat(filepath.Join(homePath, ManifestName))
	if statErr != nil || manifestInfo.Mode().Perm() != 0o644 {
		t.Fatalf("retried manifest mode = %v err=%v, want 0644", manifestInfo, statErr)
	}
	if report := Doctor(homePath); !report.OK {
		t.Fatalf("Doctor after staging mode drift retry = %+v", report)
	}
}

func exerciseInitManifestPublicationFailure(t *testing.T, inject func()) {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), ".compound")
	oldWrite := initManifestFileWrite
	oldSync := initManifestFileSync
	oldClose := initManifestFileClose
	oldBeforePublish := initManifestBeforePublish
	restore := func() {
		initManifestFileWrite = oldWrite
		initManifestFileSync = oldSync
		initManifestFileClose = oldClose
		initManifestBeforePublish = oldBeforePublish
	}
	t.Cleanup(restore)
	inject()

	if err := Init(homePath, HomeKindGlobal, time.Unix(0, 0)); err == nil {
		t.Fatal("Init succeeded despite injected manifest publication failure")
	}
	if _, err := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed Init left authoritative manifest: %v", err)
	}
	entries, err := os.ReadDir(homePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), manifestInitStagingPrefix) {
			t.Fatalf("failed Init left owned staging residue %q", entry.Name())
		}
	}
	for _, rel := range HabitatDirs() {
		if _, err := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed Init created habitat %s: %v", rel, err)
		}
	}

	restore()
	if err := Init(homePath, HomeKindGlobal, time.Unix(0, 0)); err != nil {
		t.Fatalf("Init retry after publication failure: %v", err)
	}
	manifest, err := LoadManifest(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.HomeKind != HomeKindGlobal {
		t.Fatalf("retry manifest = %+v", manifest)
	}
}

func TestInitRetriesPastOwnedManifestStagingResidue(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	residueName := manifestInitStagingPrefix + "0123456789abcdef0123456789abcdef"
	residuePath := filepath.Join(homePath, residueName)
	residue := []byte("partial crash residue")
	if err := os.WriteFile(residuePath, residue, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Init(homePath, HomeKindGlobal, time.Unix(0, 0)); err != nil {
		t.Fatalf("Init with owned staging residue: %v", err)
	}
	if _, err := LoadManifest(homePath); err != nil {
		t.Fatalf("LoadManifest after staging-residue retry: %v", err)
	}
	if _, err := os.Lstat(residuePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Init retry left recovered staging residue: %v", err)
	}
	report := Doctor(homePath)
	if !report.OK || !report.HabitatOK || len(report.Habitat.UnknownEntries) != 0 {
		t.Fatalf("Doctor after staging-residue retry = %+v", report)
	}
}

func TestInitRetriesPastOwnedManifestStagingRemovalResidue(t *testing.T) {
	baseName := manifestInitStagingPrefix + "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name  string
		setup func(t *testing.T, homePath string)
	}{
		{
			name: "quarantine only",
			setup: func(t *testing.T, homePath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(homePath, baseName+".remove-pending"), []byte("interrupted cleanup"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "original and quarantine hardlink",
			setup: func(t *testing.T, homePath string) {
				t.Helper()
				originalPath := filepath.Join(homePath, baseName)
				if err := os.WriteFile(originalPath, []byte("interrupted publication"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(originalPath, originalPath+".remove-pending"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "repeated quarantine suffix",
			setup: func(t *testing.T, homePath string) {
				t.Helper()
				name := baseName + ".remove-pending.remove-pending"
				if err := os.WriteFile(filepath.Join(homePath, name), []byte("repeated interrupted cleanup"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := os.Mkdir(homePath, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, homePath)

			if err := Init(homePath, HomeKindGlobal, time.Unix(0, 0)); err != nil {
				t.Fatalf("Init with owned staging removal residue: %v", err)
			}
			if _, err := LoadManifest(homePath); err != nil {
				t.Fatalf("LoadManifest after staging-removal recovery: %v", err)
			}
			entries, err := os.ReadDir(homePath)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), manifestInitStagingPrefix) {
					t.Fatalf("Init retry left staging removal residue %q", entry.Name())
				}
			}
		})
	}
}

func TestInitRefusesSplitManifestStagingRemovalChainWithoutRemovingEither(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	baseName := manifestInitStagingPrefix + "0123456789abcdef0123456789abcdef"
	originalPath := filepath.Join(homePath, baseName)
	quarantinePath := originalPath + ".remove-pending"
	originalBody := []byte("original inode must survive")
	quarantineBody := []byte("different quarantine inode must survive")
	if err := os.WriteFile(originalPath, originalBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quarantinePath, quarantineBody, 0o600); err != nil {
		t.Fatal(err)
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "do not identify one owned residue") {
		t.Fatalf("Init split staging removal chain error = %v", err)
	}
	for path, want := range map[string][]byte{
		originalPath:   originalBody,
		quarantinePath: quarantineBody,
	} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("split staging removal chain changed %s: body=%q err=%v", path, got, readErr)
		}
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init published manifest beside split staging removal chain: %v", statErr)
	}
}

func TestInitFreshPublicationDoesNotAdoptStagingRemovalName(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	residueName := manifestInitStagingPrefix + "0123456789abcdef0123456789abcdef.remove-pending"
	residueBody := []byte("concurrent population must survive")
	oldBeforePublish := initManifestBeforePublish
	t.Cleanup(func() { initManifestBeforePublish = oldBeforePublish })
	initManifestBeforePublish = func(root *os.Root, _ string, _ []byte) error {
		file, err := root.OpenFile(residueName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := file.Write(residueBody); err != nil {
			_ = file.Close()
			return err
		}
		return errors.Join(file.Sync(), file.Close())
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "populated during initialization") {
		t.Fatalf("Init fresh-publication removal-name error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init adopted a concurrent staging-removal name: %v", statErr)
	}
	residuePath := filepath.Join(homePath, residueName)
	if got, readErr := os.ReadFile(residuePath); readErr != nil || !bytes.Equal(got, residueBody) {
		t.Fatalf("concurrent staging-removal residue changed: body=%q err=%v", got, readErr)
	}
}

func TestInitRefusesSymlinkManifestStagingResidueWithoutRemovingIt(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "operator-data")
	targetBody := []byte("preserve")
	if err := os.WriteFile(targetPath, targetBody, 0o600); err != nil {
		t.Fatal(err)
	}
	residueName := manifestInitStagingPrefix + "0123456789abcdef0123456789abcdef"
	residuePath := filepath.Join(homePath, residueName)
	if err := os.Symlink(targetPath, residuePath); err != nil {
		t.Fatal(err)
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("Init symlink staging residue error = %v", err)
	}
	if info, statErr := os.Lstat(residuePath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Init removed or replaced symlink residue: info=%v err=%v", info, statErr)
	}
	if got, readErr := os.ReadFile(targetPath); readErr != nil || !bytes.Equal(got, targetBody) {
		t.Fatalf("Init changed symlink target: body=%q err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init published manifest beside hostile residue: %v", statErr)
	}
}

func TestInitRefusesUnownedManifestStagingResidueWithoutRemovingIt(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	residueName := manifestInitStagingPrefix + "0123456789abcdef0123456789abcdef"
	residuePath := filepath.Join(homePath, residueName)
	residueBody := []byte("foreign owner")
	if err := os.WriteFile(residuePath, residueBody, 0o600); err != nil {
		t.Fatal(err)
	}
	residueInfo, err := os.Lstat(residuePath)
	if err != nil {
		t.Fatal(err)
	}
	oldOwned := manifestInitResidueOwnedByCurrent
	t.Cleanup(func() { manifestInitResidueOwnedByCurrent = oldOwned })
	manifestInitResidueOwnedByCurrent = func(info os.FileInfo) bool {
		if os.SameFile(residueInfo, info) {
			return false
		}
		return oldOwned(info)
	}

	err = Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "current-user-owned") {
		t.Fatalf("Init unowned staging residue error = %v", err)
	}
	if got, readErr := os.ReadFile(residuePath); readErr != nil || !bytes.Equal(got, residueBody) {
		t.Fatalf("Init removed or changed unowned residue: body=%q err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init published manifest beside unowned residue: %v", statErr)
	}
}

func TestInitRefusesRacedManifestStagingResidueWithoutDeletingReplacement(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	residueName := manifestInitStagingPrefix + "0123456789abcdef0123456789abcdef"
	residuePath := filepath.Join(homePath, residueName)
	if err := os.WriteFile(residuePath, []byte("crash residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementBody := []byte("replacement must survive")
	var replacementInfo os.FileInfo
	oldBeforeUnlink := removeRootedBeforeUnlink
	t.Cleanup(func() { removeRootedBeforeUnlink = oldBeforeUnlink })
	removeRootedBeforeUnlink = func(quarantinePath string) error {
		swapPath := filepath.Join(homePath, "replacement.swap")
		if err := os.WriteFile(swapPath, replacementBody, 0o600); err != nil {
			return err
		}
		if err := os.Rename(swapPath, quarantinePath); err != nil {
			return err
		}
		info, err := os.Lstat(quarantinePath)
		if err != nil {
			return err
		}
		replacementInfo = info
		return nil
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "changed after validation") {
		t.Fatalf("Init raced staging residue error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init published manifest after residue race: %v", statErr)
	}
	survivorPath := residuePath
	survivorInfo, statErr := os.Lstat(survivorPath)
	if statErr != nil {
		survivorPath = residuePath + ".remove-pending"
		survivorInfo, statErr = os.Lstat(survivorPath)
	}
	if statErr != nil || replacementInfo == nil || !os.SameFile(replacementInfo, survivorInfo) {
		t.Fatalf("raced replacement did not survive: path=%s info=%v err=%v", survivorPath, survivorInfo, statErr)
	}
	if got, readErr := os.ReadFile(survivorPath); readErr != nil || !bytes.Equal(got, replacementBody) {
		t.Fatalf("raced replacement bytes changed: body=%q err=%v", got, readErr)
	}
}

func TestInitQuarantinePublicationNeverReplacesCompetingResidue(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	residueName := manifestInitStagingPrefix + "0123456789abcdef0123456789abcdef"
	residuePath := filepath.Join(homePath, residueName)
	residueBody := []byte("owned interrupted-init residue")
	if err := os.WriteFile(residuePath, residueBody, 0o600); err != nil {
		t.Fatal(err)
	}
	quarantinePath := residuePath + ".remove-pending"
	competingBody := []byte("competing quarantine must survive")
	oldBeforePublish := removeRootedBeforeQuarantinePublish
	t.Cleanup(func() { removeRootedBeforeQuarantinePublish = oldBeforePublish })
	removeRootedBeforeQuarantinePublish = func(gotPath string) error {
		if gotPath != quarantinePath {
			t.Fatalf("quarantine seam path = %q, want %q", gotPath, quarantinePath)
		}
		return os.WriteFile(quarantinePath, competingBody, 0o600)
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "quarantine") {
		t.Fatalf("Init quarantine publication race error = %v", err)
	}
	if got, readErr := os.ReadFile(residuePath); readErr != nil || !bytes.Equal(got, residueBody) {
		t.Fatalf("original residue changed: body=%q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(quarantinePath); readErr != nil || !bytes.Equal(got, competingBody) {
		t.Fatalf("competing quarantine changed: body=%q err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Init published manifest after quarantine race: %v", statErr)
	}
}

func TestInitNeverReplacesManifestPublishedByConcurrentInitializer(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	concurrentBody := []byte("schema_version: " + SchemaVersion + "\nhome_kind: local\ncreated_at: 1970-01-01T00:00:01Z\nactive_modules: []\n")
	oldBeforePublish := initManifestBeforePublish
	t.Cleanup(func() { initManifestBeforePublish = oldBeforePublish })
	initManifestBeforePublish = func(root *os.Root, _ string, _ []byte) error {
		file, err := root.OpenFile(ManifestName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := file.Write(concurrentBody); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if err == nil {
		t.Fatal("Init adopted a concurrently published manifest")
	}
	got, readErr := os.ReadFile(filepath.Join(homePath, ManifestName))
	if readErr != nil || !bytes.Equal(got, concurrentBody) {
		t.Fatalf("Init replaced concurrent manifest: body=%q err=%v", got, readErr)
	}
	for _, rel := range HabitatDirs() {
		if _, err := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("concurrent publication created habitat %s: %v", rel, err)
		}
	}
}

func TestConcurrentInitPublishesExactlyOneManifestWithoutAdoption(t *testing.T) {
	for attempt := range 20 {
		t.Run(fmt.Sprintf("attempt-%02d", attempt), func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			lockContended := instrumentInitRootLockContention(t)
			ready := make(chan struct{}, 1)
			release := make(chan struct{})
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})
			oldBeforePublish := initManifestBeforePublish
			t.Cleanup(func() { initManifestBeforePublish = oldBeforePublish })
			initManifestBeforePublish = func(*os.Root, string, []byte) error {
				ready <- struct{}{}
				<-release
				return nil
			}
			type result struct {
				kind string
				err  error
			}
			results := make(chan result, 2)
			for index, kind := range []string{HomeKindGlobal, HomeKindLocal} {
				go func(index int, kind string) {
					results <- result{kind: kind, err: Init(homePath, kind, time.Unix(int64(index), 0))}
				}(index, kind)
			}
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent Init did not reach the publication boundary")
			}
			select {
			case <-lockContended:
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent Init did not contend on the pinned directory lock")
			}
			close(release)
			released = true

			var succeeded, failed []result
			for range 2 {
				select {
				case got := <-results:
					if got.err == nil {
						succeeded = append(succeeded, got)
					} else {
						failed = append(failed, got)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("concurrent Init did not return")
				}
			}
			if len(succeeded) != 1 || len(failed) != 1 {
				t.Fatalf("concurrent Init results: succeeded=%+v failed=%+v", succeeded, failed)
			}
			manifest, err := LoadManifest(homePath)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.HomeKind != succeeded[0].kind {
				t.Fatalf("published kind = %q, successful initializer = %q", manifest.HomeKind, succeeded[0].kind)
			}
			entries, err := os.ReadDir(homePath)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), manifestInitStagingPrefix) {
					t.Fatalf("concurrent Init left staging entry %q", entry.Name())
				}
			}
		})
	}
}

func TestHomeManifestDecoderRejectsNonCanonicalYAML(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown field",
			body: "schema_version: " + SchemaVersion + "\nhome_kind: local\nunknown_identity: true\n",
			want: "field unknown_identity not found",
		},
		{
			name: "second document",
			body: "schema_version: " + SchemaVersion + "\nhome_kind: local\n---\nschema_version: " + SchemaVersion + "\nhome_kind: global\n",
			want: "exactly one YAML document",
		},
		{
			name: "duplicate key",
			body: "schema_version: " + SchemaVersion + "\nschema_version: " + SchemaVersion + "\nhome_kind: local\n",
			want: "already defined",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := os.Mkdir(homePath, 0o700); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(homePath, ManifestName)
			before := []byte(tc.body)
			if err := os.WriteFile(manifestPath, before, 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := LoadManifest(homePath); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadManifest error = %v, want %q", err, tc.want)
			}
			report := Doctor(homePath)
			if report.ManifestOK || report.OK || !strings.Contains(report.ManifestError, tc.want) {
				t.Fatalf("Doctor non-canonical manifest = %+v, want %q", report, tc.want)
			}
			if err := Init(homePath, HomeKindLocal, time.Unix(0, 0)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Init non-canonical manifest error = %v, want %q", err, tc.want)
			}
			after, err := os.ReadFile(manifestPath)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("non-canonical manifest mutated: body=%q err=%v", after, err)
			}
		})
	}
}
