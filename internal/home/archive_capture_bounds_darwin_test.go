//go:build darwin

package home

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCapturedBulkFallbackRetainsOriginalDirectoryFence(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "selected"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	previous := captureReadBulk
	defer func() { captureReadBulk = previous }()
	captureReadBulk = func(*os.File, []byte) (int, error) {
		if err := root.Rename("selected", "original"); err != nil {
			t.Fatal(err)
		}
		if err := root.Mkdir("selected", 0700); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile("selected/substitute", []byte("not selected"), 0600); err != nil {
			t.Fatal(err)
		}
		return 0, syscall.ENOTSUP
	}
	err = walkCaptureRootBulk(root, "selected", func(string, fs.FileInfo) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replaced fallback directory accepted: %v", err)
	}
}

func TestCapturedCacheRejectsResizedFilesBeforeHash(t *testing.T) {
	for _, name := range []string{"cache-index", "warm-journal"} {
		t.Run(name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			cache, err := newArchiveCaptureCache(root)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.index.Close()
			if err := root.WriteFile("journal", nil, 0600); err != nil {
				t.Fatal(err)
			}
			if name == "cache-index" {
				if err := cache.index.Truncate(1); err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := root.OpenFile("journal", os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(archiveStreamTotalMeta + 1)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := cache.seal(root, "unused"); err == nil {
				t.Fatal("resized cache accepted")
			}
			if cache.indexHash != "" {
				t.Fatal("invalid sizes were discovered only after hashing")
			}
			if _, err := root.Stat("journal"); err != nil {
				t.Fatalf("invalid input renamed before refusal: %v", err)
			}
		})
	}
}

func TestCapturedShardRejectsReplacementBeforeFirstClone(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	shards, err := newArchiveCaptureShards(root)
	if err != nil {
		t.Fatal(err)
	}
	defer shards.close()
	previous := captureOpenShard
	defer func() { captureOpenShard = previous }()
	captureOpenShard = func(stage *os.Root, name string) (*os.Root, error) {
		if err := stage.Rename(name, name+"-original"); err != nil {
			return nil, err
		}
		if err := stage.Mkdir(name, 0700); err != nil {
			return nil, err
		}
		if err := stage.WriteFile(name+"/sentinel", []byte("outside selected shard"), 0600); err != nil {
			return nil, err
		}
		return stage.OpenRoot(name)
	}
	if err := shards.ensure(1); err == nil {
		t.Fatal("substituted shard accepted for first clone")
	}
	if _, err := root.Stat(captureShardName(0) + "/" + captureBodyLeaf(1)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("body appeared before identity check: %v", err)
	}
	data, err := root.ReadFile(captureShardName(0) + "/sentinel")
	if err != nil || string(data) != "outside selected shard" {
		t.Fatalf("substitution was modified: %q %v", data, err)
	}
}
