package home

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArchiveHeaderRetainsMetadataWithoutAccountNames(t *testing.T) {
	want := tar.Header{
		Name: "entry", Typeflag: tar.TypeReg, Size: 17, Mode: 0o640,
		Uid: 1234, Gid: 2345, Uname: "archive-user", Gname: "archive-group",
		ModTime: time.Unix(1_700_000_000, 0), AccessTime: time.Unix(1_700_000_001, 0),
		ChangeTime: time.Unix(1_700_000_002, 0),
	}
	got, err := archiveFileInfoHeader(want.FileInfo(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Uname != "" || got.Gname != "" {
		t.Fatalf("archive includes account names: user=%q group=%q", got.Uname, got.Gname)
	}
	if got.Uid != want.Uid || got.Gid != want.Gid || got.Mode != want.Mode || got.Size != want.Size ||
		got.Name != want.Name || got.Typeflag != want.Typeflag || !got.ModTime.Equal(want.ModTime) ||
		!got.AccessTime.Equal(want.AccessTime) || !got.ChangeTime.Equal(want.ChangeTime) {
		t.Fatalf("archive metadata changed: got %+v, want %+v except account names", got, want)
	}
}

func TestArchiveFileWritersOmitAccountNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body")
	body := []byte("archive body\n")
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, format := range []string{"legacy", "streamed"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			tw := tar.NewWriter(&out)
			if format == "legacy" {
				_, err = writeFileEntry(tw, path, "body", info)
			} else {
				_, err = writeRootedArchiveFile(tw, root, "body", info)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			h, err := tar.NewReader(&out).Next()
			if err != nil {
				t.Fatal(err)
			}
			if h.Uname != "" || h.Gname != "" {
				t.Fatalf("archive writer resolved account names: user=%q group=%q", h.Uname, h.Gname)
			}
			if h.Name != "body" || h.Size != int64(len(body)) || h.Mode != int64(info.Mode().Perm()) {
				t.Fatalf("archive file metadata changed: %+v", h)
			}
		})
	}
}
