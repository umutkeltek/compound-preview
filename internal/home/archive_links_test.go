package home

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func linkedArchiveFixture(t *testing.T) (string, string, time.Time) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(homePath, "credit-harvest", "runs", "sample")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.txt"), []byte("retained result\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return homePath, dir, now
}

func TestStreamedLinkedArchiveRoundTripWithoutSource(t *testing.T) {
	homePath, dir, now := linkedArchiveFixture(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "shared.txt"), []byte("retained result\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"absolute":       filepath.Join(dir, "result.txt"),
		"relative":       "result.txt",
		"dangling":       "missing.txt",
		"cycle":          "cycle",
		"cross-relative": "../shared.txt",
		"cross-absolute": filepath.Join(filepath.Dir(dir), "shared.txt"),
		"directory":      "..",
		"chain":          "directory/sample/result.txt",
		"cycle-a":        "cycle-b",
		"cycle-b":        "cycle-a",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// The existing streamed profile refuses links. Legacy v1 remains unchanged;
	// it historically omitted non-regular entries and is not link-custody proof.
	for _, format := range []string{"streamed-v2"} {
		archive := filepath.Join(t.TempDir(), "old.tar.gz")
		if _, err := BackupWithFormat(homePath, archive, format, now); err == nil {
			t.Fatalf("%s unexpectedly admitted links", format)
		}
		if _, err := os.Stat(archive); !os.IsNotExist(err) {
			t.Fatalf("%s published refused archive: %v", format, err)
		}
	}
	archive := filepath.Join(t.TempDir(), "linked.tar.gz")
	backed, err := BackupWithFormat(homePath, archive, "streamed-v3", now)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyArchive(archive)
	if err != nil || !verified.OK || verified.Files != backed.Files {
		t.Fatalf("verify = %+v, %v; backup = %+v", verified, err, backed)
	}
	if err := os.Rename(homePath, homePath+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(archive, restored, now); err != nil {
		t.Fatal(err)
	}
	for name, original := range links {
		p := filepath.Join(restored, "credit-harvest", "runs", "sample", name)
		got, err := os.Readlink(p)
		want := original
		if filepath.IsAbs(original) {
			var err error
			want, err = filepath.Rel(dir, original)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err != nil || got != want || filepath.IsAbs(got) {
			t.Fatalf("link %s = %q, %v; want %q", name, got, err, want)
		}
		if name == "absolute" || name == "relative" || name == "cross-relative" || name == "cross-absolute" || name == "chain" {
			body, err := os.ReadFile(p)
			if err != nil || string(body) != "retained result\n" {
				t.Fatalf("restored consumer %s = %q, %v", name, body, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(restored, "credit-harvest", "runs", "sample", "dangling")); !os.IsNotExist(err) {
		t.Fatalf("dangling historical link was not preserved: %v", err)
	}
}

func TestStreamedLinkedArchiveRejectsEscapingAndAmbiguousSourceLinks(t *testing.T) {
	for _, target := range []string{"/outside.example/secret", "../../../../outside", "alias/../result.txt", "result.txt/", "result.txt/.", "./result.txt"} {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			homePath, dir, now := linkedArchiveFixture(t)
			if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "rejected.tar.gz")
			if _, err := BackupWithFormat(homePath, archive, "streamed-v3", now); err == nil {
				t.Fatal("unsafe source link accepted")
			}
			if _, err := os.Stat(archive); !os.IsNotExist(err) {
				t.Fatalf("failed source published archive: %v", err)
			}
		})
	}
}

func linkedArchiveRows(t *testing.T) []restoreRawTarEntry {
	t.Helper()
	homePath, dir, now := linkedArchiveFixture(t)
	if err := os.Symlink("result.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "links.tar.gz")
	if _, err := BackupWithFormat(homePath, archive, ArchiveFormatStreamedV3, now); err != nil {
		t.Fatal(err)
	}
	return readStreamedRawRows(t, archive)
}

func TestStreamedLinkedArchiveRejectsMalformedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*archiveStreamIndex)
	}{
		{"checksum", func(i *archiveStreamIndex) { i.Links[0].SHA256 = strings.Repeat("0", 64) }},
		{"portable-target", func(i *archiveStreamIndex) { i.Links[0].Target = "missing" }},
		{"source-home", func(i *archiveStreamIndex) { i.SourceHome = "relative" }},
		{"version", func(i *archiveStreamIndex) { i.SchemaVersion = ArchiveStreamSchemaV2 }},
		{"duplicate", func(i *archiveStreamIndex) { i.Links = append(i.Links, i.Links[0]) }},
		{"regular-collision", func(i *archiveStreamIndex) { i.Links[0].Path = "credit-harvest/runs/sample/result.txt" }},
		{"directory-collision", func(i *archiveStreamIndex) { i.Links[0].Path = "credit-harvest/runs/sample" }},
		{"missing-parent", func(i *archiveStreamIndex) { i.Links[0].Path = "credit-harvest/missing/link" }},
		{"file-parent", func(i *archiveStreamIndex) { i.Links[0].Path = "credit-harvest/runs/sample/result.txt/child" }},
		{"link-parent", func(i *archiveStreamIndex) {
			child := i.Links[0]
			child.Path += "/child"
			i.Links = append(i.Links, child)
		}},
		{"traversal", func(i *archiveStreamIndex) { i.Links[0].Path = "../outside" }},
		{"manifest", func(i *archiveStreamIndex) { i.Links[0].Path = ManifestName }},
		{"reserved-metadata", func(i *archiveStreamIndex) { i.Links[0].Path = archiveStreamIndexName }},
		{"excluded", func(i *archiveStreamIndex) { i.Links[0].Path = "tmp/link" }},
		{"escape", func(i *archiveStreamIndex) { i.Links[0].OriginalTarget = "/outside.example/secret" }},
		{"link-count", func(i *archiveStreamIndex) {
			for len(i.Links) <= archiveStreamMaxLinks {
				i.Links = append(i.Links, i.Links[0])
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := linkedArchiveRows(t)
			for n := range rows {
				if rows[n].hdr.Name != archiveStreamIndexName {
					continue
				}
				var index archiveStreamIndex
				if err := json.Unmarshal(rows[n].body, &index); err != nil {
					t.Fatal(err)
				}
				tc.mutate(&index)
				// Other than the checksum control, bind the altered fields so
				// path validation cannot accidentally pass on checksum rejection.
				if tc.name != "checksum" {
					for k := range index.Links {
						index.Links[k].SHA256 = archiveLinkDigest(index.Links[k])
					}
				}
				body, err := json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
				rows[n].body, rows[n].hdr.Size = body, int64(len(body))
			}
			archive := writeRestoreRawTarGz(t, rows)
			if _, err := VerifyArchive(archive); err == nil {
				t.Fatal("malformed link metadata verified")
			}
			target := filepath.Join(t.TempDir(), "refused")
			if _, err := RestoreArchive(archive, target, time.Now()); err == nil {
				t.Fatal("malformed link metadata restored")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("refused restore published target: %v", err)
			}
		})
	}
}

func TestStreamedLinkedArchiveStillRejectsTarLinks(t *testing.T) {
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink} {
		rows := linkedArchiveRows(t)
		row := restoreRawTarEntry{hdr: &tar.Header{Name: "credit-harvest/unsafe", Typeflag: kind, Linkname: "../../outside"}}
		rows = append(rows[:len(rows)-1], row, rows[len(rows)-1])
		if _, err := VerifyArchive(writeRestoreRawTarGz(t, rows)); err == nil {
			t.Fatal("tar link header accepted")
		}
	}
}

func TestStreamedLinkedArchiveBudgets(t *testing.T) {
	link := archiveLink{Path: "credit-harvest/link", OriginalTarget: "body", Target: "body"}
	link.SHA256 = archiveLinkDigest(link)
	index := archiveStreamIndex{SourceHome: "/home.example", Links: []archiveLink{link}}
	seen := map[[sha256.Size]byte]bool{sha256.Sum256([]byte("credit-harvest")): true}
	limits := archiveStreamTarLimits()
	budget := archiveEntryBudget{limits: limits, entries: limits.MaxEntries}
	if err := validateArchiveLinks(index, ArchiveStreamSchemaV3, seen, &budget); err == nil {
		t.Fatal("link bypassed member budget")
	}
	for _, original := range []string{strings.Repeat("a", archiveMaxPathBytes+1), strings.Repeat("a/", archiveMaxPathDepth) + "end"} {
		if _, err := portableArchiveLink(index.SourceHome, link.Path, original); err == nil {
			t.Fatal("oversized target accepted")
		}
	}
	// Both home-relative paths are bounded, but their relative displacement is not.
	name := strings.Repeat("d/", 40) + "link"
	original := "/home.example/" + strings.Repeat("e/", 40) + "body"
	if _, err := portableArchiveLink(index.SourceHome, name, original); err == nil {
		t.Fatal("oversized portable displacement accepted")
	}
}

func TestStreamedLinkedArchiveSourceReplacementRefusesPublication(t *testing.T) {
	homePath, dir, now := linkedArchiveFixture(t)
	link := filepath.Join(dir, "link")
	if err := os.Symlink("result.txt", link); err != nil {
		t.Fatal(err)
	}
	old := archiveAfterLinkRead
	t.Cleanup(func() { archiveAfterLinkRead = old })
	archiveAfterLinkRead = func(string) error {
		if err := os.Remove(link); err != nil {
			return err
		}
		return os.Symlink("missing", link)
	}
	archive := filepath.Join(t.TempDir(), "refused.tar.gz")
	if _, err := BackupWithFormat(homePath, archive, ArchiveFormatStreamedV3, now); err == nil {
		t.Fatal("source replacement accepted")
	}
	if _, err := os.Lstat(archive); !os.IsNotExist(err) {
		t.Fatalf("failed capture published archive: %v", err)
	}
}

func TestStreamedLinkedArchiveNonUTF8SourceRefusesPublication(t *testing.T) {
	for _, kind := range []string{"target", "path", "source-home"} {
		t.Run(kind, func(t *testing.T) {
			// Some filesystems refuse non-UTF-8 entry names themselves. Exercise
			// metadata admission directly for paths, and the real Unix symlink
			// backup path for target bytes (which the filesystem permits).
			if kind == "path" || kind == "source-home" {
				source, name := "/home.example", "credit-harvest/link"
				if kind == "path" {
					name += "\xff"
				} else {
					source += "\xff"
				}
				if _, err := portableArchiveLink(source, name, "missing"); err == nil {
					t.Fatal("non-UTF-8 path metadata accepted")
				}
				return
			}
			homePath, dir, now := linkedArchiveFixture(t)
			name, target := "link", "missing\xff"
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "refused.tar.gz")
			if _, err := BackupWithFormat(homePath, archive, ArchiveFormatStreamedV3, now); err == nil {
				t.Fatal("non-UTF-8 link metadata accepted")
			}
			if _, err := os.Lstat(archive); !os.IsNotExist(err) {
				t.Fatalf("invalid metadata published archive: %v", err)
			}
		})
	}
}

func TestStreamedLinkedArchiveStagedReplacementRefusesPublication(t *testing.T) {
	for _, replacement := range []string{"link", "regular", "parent"} {
		t.Run(replacement, func(t *testing.T) {
			archive := writeRestoreRawTarGz(t, linkedArchiveRows(t))
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := restoreBeforePublish
			t.Cleanup(func() { restoreBeforePublish = old })
			restoreBeforePublish = func(staging, _ string) error {
				p := filepath.Join(staging, "credit-harvest", "runs", "sample", "link")
				if replacement == "parent" {
					p = filepath.Dir(p)
					if err := os.Rename(p, p+"-retained"); err != nil {
						return err
					}
					return os.Symlink(outside, p)
				}
				if err := os.Remove(p); err != nil {
					return err
				}
				if replacement == "regular" {
					return os.WriteFile(p, []byte("not a link"), 0o600)
				}
				return os.Symlink(sentinel, p)
			}
			target := filepath.Join(t.TempDir(), "refused")
			if _, err := RestoreArchive(archive, target, time.Now()); err == nil {
				t.Fatal("staged replacement accepted")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("refused restore published: %v", err)
			}
			body, err := os.ReadFile(sentinel)
			if err != nil || string(body) != "untouched" {
				t.Fatalf("outside sentinel changed: %q, %v", body, err)
			}
		})
	}
}
