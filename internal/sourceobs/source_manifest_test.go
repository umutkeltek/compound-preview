package sourceobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func TestSourceManifestPathAndAuthorityUseCanonicalSourcesOwner(t *testing.T) {
	homePath := t.TempDir()
	family := "canonical_owner"
	wantPath := filepath.Join(homePath, "sources", "manifests", family+".json")
	if got := SourceManifestPath(homePath, family); got != wantPath {
		t.Fatalf("SourceManifestPath = %q, want canonical %q", got, wantPath)
	}

	sourcePath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(sourcePath, []byte("# canonical\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildSourceManifest(family, []string{sourcePath}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wantAuthority := "compound://sources/manifests/" + family
	if manifest.AuthorityRef != wantAuthority {
		t.Fatalf("authority_ref = %q, want %q", manifest.AuthorityRef, wantAuthority)
	}
}

func TestSourceManifestLegacyFallbackIsObservableAndCanonicalizedOnWrite(t *testing.T) {
	homePath := t.TempDir()
	family := "legacy_storage_bridge"
	sourcePath := filepath.Join(t.TempDir(), "legacy.md")
	if err := os.WriteFile(sourcePath, []byte("# legacy bridge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy, err := BuildOwnedSourceManifest(homePath, family, []string{sourcePath}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	legacy.AuthorityRef = "compound://registries/source-manifests/" + family
	legacyPath := LegacySourceManifestPath(homePath, family)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != legacy.ID {
		t.Fatalf("legacy fallback changed manifest identity: got %q want %q", loaded.ID, legacy.ID)
	}
	doctor, err := DoctorSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "degraded" || doctor.StorageStatus != "legacy_fallback" || doctor.LoadedManifestPath != legacyPath {
		t.Fatalf("legacy fallback is not observable: %+v", doctor)
	}
	if got := sourceManifestDoctorCheckByNameForTest(doctor, "storage_location"); got.Status != "pending" {
		t.Fatalf("storage_location check = %+v, want pending bridge debt", got)
	}

	if _, err := RecordSourceManifest(homePath, loaded); err != nil {
		t.Fatal(err)
	}
	canonicalPath := SourceManifestPath(homePath, family)
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("canonical publication missing: %v", err)
	}
	canonical, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.AuthorityRef != "compound://sources/manifests/"+family {
		t.Fatalf("canonical authority_ref = %q", canonical.AuthorityRef)
	}
	doctor, err = DoctorSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" || doctor.StorageStatus != "canonical" || doctor.LoadedManifestPath != canonicalPath {
		t.Fatalf("canonical publication not consumed: %+v", doctor)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("compatibility read bridge was destructively changed: %v", err)
	}
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored.md")
	if _, err := MaterializeSourceManifestItem(homePath, family, canonical.Items[0].ID, restoredPath); err != nil {
		t.Fatalf("canonical real consumer still depends on legacy/source path: %v", err)
	}
	restored, err := os.ReadFile(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != "# legacy bridge\n" {
		t.Fatalf("restored canonical body = %q", restored)
	}
}

func TestSourceManifestCanonicalCorruptionFailsClosedWithoutLegacyFallback(t *testing.T) {
	homePath := t.TempDir()
	family := "fail_closed_storage"
	legacyPath := LegacySourceManifestPath(homePath, family)
	canonicalPath := SourceManifestPath(homePath, family)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	validLegacy := `{"schema_version":"compound.source_manifest.v0.3","id":"srcman_legacy","generated_at":"2026-07-14T00:00:00Z","source_family":"fail_closed_storage","authority_ref":"compound://registries/source-manifests/fail_closed_storage","lifecycle":{"state":"registered","revision":0,"history":[]},"items":[{"id":"srcitem_legacy","source_key":"source_legacy","version_state":"current","source_kind":"markdown","source_ref":"/unavailable/legacy.md","file_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","snapshot_time":"2026-07-14T00:00:00Z","privacy":"private","body_policy":"source_in_place"}]}`
	if err := os.WriteFile(legacyPath, []byte(validLegacy+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonicalPath, []byte("{corrupt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadSourceManifest(homePath, family); err == nil {
		t.Fatal("corrupt canonical manifest silently fell back to legacy")
	}
}

func sourceManifestDoctorCheckByNameForTest(report SourceManifestDoctorReport, name string) SourceManifestDoctorCheck {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return SourceManifestDoctorCheck{Name: name, Status: "missing"}
}

func TestOwnedSourceManifestSurvivesOriginalLossAndMaterializesExactBytes(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 7, 14, 5, 30, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "compound.md")
	body := []byte("---\ntitle: Compound\n---\n# Compound\n\nPhysical custody is not semantic promotion.\n")
	if err := os.WriteFile(sourcePath, body, 0o750); err != nil {
		t.Fatal(err)
	}

	manifest, err := BuildOwnedSourceManifest(homePath, "brain_custody_canary", []string{sourcePath}, time.Date(2026, 7, 14, 5, 31, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Items) != 1 {
		t.Fatalf("items = %d", len(manifest.Items))
	}
	item := manifest.Items[0]
	if item.BodyPolicy != "owned_snapshot" || !strings.HasPrefix(item.BodyRef, "artifact:sha256:") {
		t.Fatalf("owned item = %+v", item)
	}
	if item.BodyBytes != int64(len(body)) || item.MediaType != "text/markdown" {
		t.Fatalf("body metadata = %+v", item)
	}
	if _, err := RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}

	doctor, err := DoctorSourceManifest(homePath, manifest.SourceFamily)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" || !doctor.CustodyComplete || doctor.OwnedBodyCount != 1 {
		t.Fatalf("doctor after source loss = %+v", doctor)
	}
	if len(doctor.Items) != 1 || doctor.Items[0].BodyStatus != "ok" || doctor.Items[0].SourceStatus != "source_unavailable" {
		t.Fatalf("doctor item after source loss = %+v", doctor.Items)
	}

	target := filepath.Join(t.TempDir(), "restored", "compound.md")
	report, err := MaterializeSourceManifestItem(homePath, manifest.SourceFamily, item.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if report.BodyRef != item.BodyRef || report.Bytes != int64(len(body)) || report.TargetPath != target {
		t.Fatalf("materialize report = %+v", report)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("materialized bytes differ:\ngot  %q\nwant %q", got, body)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("materialized mode = %04o, want 0750", info.Mode().Perm())
	}
}

func TestOwnedSourceManifestDoctorFailsMissingOrCorruptBody(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, path string)
		want   string
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			want: "body_unavailable",
		},
		{
			name: "corrupt",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("corrupt"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "body_hash_mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", time.Now()); err != nil {
				t.Fatal(err)
			}
			sourcePath := filepath.Join(t.TempDir(), "source.md")
			if err := os.WriteFile(sourcePath, []byte("# body\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			manifest, err := BuildOwnedSourceManifest(homePath, "custody_failure", []string{sourcePath}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RecordSourceManifest(homePath, manifest); err != nil {
				t.Fatal(err)
			}
			artifactPath := filepath.Join(homePath, "artifacts", "sha256", strings.TrimPrefix(manifest.Items[0].BodyRef, "artifact:sha256:"))
			tt.mutate(t, artifactPath)

			doctor, err := DoctorSourceManifest(homePath, manifest.SourceFamily)
			if err != nil {
				t.Fatal(err)
			}
			if doctor.Status != "fail" || doctor.CustodyComplete || doctor.HashMismatchCount != 1 {
				t.Fatalf("doctor = %+v", doctor)
			}
			if got := doctor.Items[0].BodyStatus; got != tt.want {
				t.Fatalf("body_status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOwnedSourceManifestDeduplicatesBodiesAndRefusesDestructiveMaterialize(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	body := []byte("same body\n")
	var paths []string
	for _, name := range []string{"one.md", "two.md"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	manifest, err := BuildOwnedSourceManifest(homePath, "dedupe", paths, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Items[0].BodyRef != manifest.Items[1].BodyRef {
		t.Fatalf("same bytes produced different refs: %q != %q", manifest.Items[0].BodyRef, manifest.Items[1].BodyRef)
	}
	if _, err := RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, []byte("different\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeSourceManifestItem(homePath, manifest.SourceFamily, manifest.Items[0].ID, target); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("destructive materialize err = %v", err)
	}
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeSourceManifestItem(homePath, manifest.SourceFamily, manifest.Items[0].ID, target); err != nil {
		t.Fatalf("byte-identical materialize must be idempotent: %v", err)
	}
}

func TestOwnedSourceManifestRejectsInvalidOrMismatchedExistingMode(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "executable.sh")
	body := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(sourcePath, body, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildOwnedSourceManifest(homePath, "mode_restore", []string{sourcePath}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "executable.sh")
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeSourceManifestItem(homePath, manifest.SourceFamily, manifest.Items[0].ID, target); err == nil || !strings.Contains(err.Error(), "non-identical mode") {
		t.Fatalf("mode mismatch err = %v", err)
	}

	loaded, err := LoadSourceManifest(homePath, manifest.SourceFamily)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Items[0].FileMode = "08no"
	bodyJSON, err := json.MarshalIndent(loaded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SourceManifestPath(homePath, manifest.SourceFamily), append(bodyJSON, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeSourceManifestItem(homePath, manifest.SourceFamily, manifest.Items[0].ID, filepath.Join(t.TempDir(), "new.sh")); err == nil || !strings.Contains(err.Error(), "invalid file_mode") {
		t.Fatalf("invalid mode err = %v", err)
	}
}

func TestLegacySourceManifestV01LoadsAndUpgradesWithoutLosingIdentity(t *testing.T) {
	homePath := t.TempDir()
	family := "legacy_v01"
	path := SourceManifestPath(homePath, family)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "schema_version": "compound.source_manifest.v0.1",
  "id": "srcman_legacy_v01",
  "generated_at": "2026-07-01T00:00:00Z",
  "source_family": "legacy_v01",
  "authority_ref": "compound://registries/source-manifests/legacy_v01",
  "items": [{
    "id": "srcitem_frozen_v01",
    "source_kind": "markdown",
    "source_ref": "/migration-only/source.md",
    "file_hash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "snapshot_time": "2026-07-01T00:00:00Z",
    "privacy": "private",
    "body_policy": "source_in_place"
  }]
}`
	if err := os.WriteFile(path, []byte(legacy+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != legacySourceManifestSchemaVersion || loaded.Items[0].ID != "srcitem_frozen_v01" {
		t.Fatalf("legacy load changed frozen identity: %+v", loaded)
	}
	if _, err := RecordSourceManifest(homePath, loaded); err != nil {
		t.Fatal(err)
	}
	upgraded, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.SchemaVersion != SourceManifestSchemaVersion || upgraded.Items[0].ID != "srcitem_frozen_v01" {
		t.Fatalf("upgrade changed identity: %+v", upgraded)
	}
	if upgraded.Items[0].SourceKey == "" || upgraded.Items[0].VersionState != "current" {
		t.Fatalf("upgrade did not populate v0.2 lineage fields: %+v", upgraded.Items[0])
	}
}

func TestOwnedSourceManifestPreservesVersionHistoryAcrossCaptures(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "compound.md")
	versions := [][]byte{[]byte("# Compound v1\n"), []byte("# Compound v2\n")}
	var itemIDs []string
	for _, body := range versions {
		if err := os.WriteFile(sourcePath, body, 0o644); err != nil {
			t.Fatal(err)
		}
		manifest, err := BuildOwnedSourceManifest(homePath, "versioned_source", []string{sourcePath}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		itemIDs = append(itemIDs, manifest.Items[0].ID)
		if _, err := RecordSourceManifest(homePath, manifest); err != nil {
			t.Fatal(err)
		}
	}
	if itemIDs[0] == itemIDs[1] {
		t.Fatalf("different source bytes reused one version id %q", itemIDs[0])
	}

	manifest, err := LoadSourceManifest(homePath, "versioned_source")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Items) != 2 {
		t.Fatalf("version history items = %d, want 2: %+v", len(manifest.Items), manifest.Items)
	}
	byID := map[string]SourceManifestItem{}
	for _, item := range manifest.Items {
		byID[item.ID] = item
	}
	first, second := byID[itemIDs[0]], byID[itemIDs[1]]
	if first.SourceKey == "" || first.SourceKey != second.SourceKey {
		t.Fatalf("stable source identity missing: first=%+v second=%+v", first, second)
	}
	if first.VersionState != "superseded" || first.SupersededBy != second.ID || second.VersionState != "current" || second.Supersedes != first.ID {
		t.Fatalf("bad version lineage: first=%+v second=%+v", first, second)
	}
	for i, itemID := range itemIDs {
		target := filepath.Join(t.TempDir(), fmt.Sprintf("v%d.md", i+1))
		if _, err := MaterializeSourceManifestItem(homePath, manifest.SourceFamily, itemID, target); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, versions[i]) {
			t.Fatalf("version %d bytes = %q, want %q", i+1, got, versions[i])
		}
	}
}

func TestSourceManifestDoctorAndDrilldown(t *testing.T) {
	home := t.TempDir()
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "compound.md")
	if err := os.WriteFile(sourcePath, []byte("---\ntitle: Compound\n---\n# Compound\n\n## Facts\n- fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	manifest, err := BuildSourceManifest("brain_source_manifest", []string{sourcePath}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Items) != 1 {
		t.Fatalf("items = %d", len(manifest.Items))
	}
	item := manifest.Items[0]
	if item.BodyPolicy != "source_in_place" || item.FileHash == "" || item.SourceRef != sourcePath {
		t.Fatalf("bad item: %+v", item)
	}
	if !hasOverlay(item.StructureOverlays, "frontmatter") || !hasOverlay(item.StructureOverlays, "heading_index") || !hasOverlay(item.StructureOverlays, "facts_fence") {
		t.Fatalf("missing overlays: %+v", item.StructureOverlays)
	}
	report, err := RecordSourceManifest(home, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if report.ItemCount != 1 {
		t.Fatalf("report = %+v", report)
	}
	before, err := os.ReadFile(report.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := BuildSourceManifest("brain_source_manifest", []string{sourcePath}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSourceManifest(home, replay); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(report.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("unchanged source should produce idempotent manifest")
	}
	doctor, err := DoctorSourceManifest(home, "brain_source_manifest")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" || doctor.HashMismatchCount != 0 {
		t.Fatalf("doctor = %+v", doctor)
	}
	if doctor.CustodyComplete || doctor.OwnedBodyCount != 0 || doctor.InventoryOnlyCount != 1 {
		t.Fatalf("source-in-place must remain inventory-only: %+v", doctor)
	}
	drilldown, err := DrilldownSourceManifest(home, "brain_source_manifest", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !drilldown.SourceAvailable || !drilldown.HashMatches || drilldown.SourceRef != sourcePath {
		t.Fatalf("drilldown = %+v", drilldown)
	}
	if len(drilldown.DrilldownPath) != 3 || drilldown.DrilldownPath[0] != "source-manifest:brain_source_manifest#"+item.ID {
		t.Fatalf("drilldown path = %+v", drilldown.DrilldownPath)
	}
}

func TestSourceManifestDoctorFlagsHashMismatch(t *testing.T) {
	home := t.TempDir()
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "compound.md")
	if err := os.WriteFile(sourcePath, []byte("# One\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildSourceManifest("brain_source_manifest", []string{sourcePath}, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSourceManifest(home, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("# Two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doctor, err := DoctorSourceManifest(home, "brain_source_manifest")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "fail" || doctor.HashMismatchCount != 1 {
		t.Fatalf("expected hash mismatch, got %+v", doctor)
	}
}

func hasOverlay(overlays []SourceStructureOverlay, kind string) bool {
	for _, overlay := range overlays {
		if overlay.Kind == kind {
			return true
		}
	}
	return false
}
