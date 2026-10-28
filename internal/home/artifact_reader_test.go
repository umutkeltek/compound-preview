package home

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestArtifactReaderReadsPublicArtifactWithoutMutation(t *testing.T) {
	homePath := canonicalArtifactReaderHome(t)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("public artifact\n")
	ref, err := store.Put(want)
	if err != nil {
		t.Fatal(err)
	}
	before := artifactReaderTreeSnapshot(t, homePath)

	reader, err := OpenArtifactReader(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Get() = %q, want %q", got, want)
	}
	if methods := exportedArtifactReaderMethods(); !reflect.DeepEqual(methods, []string{"Get", "GetPrivate", "GetPrivateBounded"}) {
		t.Fatalf("ArtifactReader exported methods = %v", methods)
	}
	if after := artifactReaderTreeSnapshot(t, homePath); !reflect.DeepEqual(after, before) {
		t.Fatalf("reader mutated home\nbefore=%v\nafter=%v", before, after)
	}
}

func TestArtifactReaderReadsPrivateArtifactWithoutMutation(t *testing.T) {
	homePath := canonicalArtifactReaderHome(t)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("private artifact\n")
	ref, err := store.PutPrivate(want)
	if err != nil {
		t.Fatal(err)
	}
	before := artifactReaderTreeSnapshot(t, homePath)
	reader, err := OpenArtifactReader(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.GetPrivate(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GetPrivate() = %q, want %q", got, want)
	}
	if after := artifactReaderTreeSnapshot(t, homePath); !reflect.DeepEqual(after, before) {
		t.Fatalf("private read mutated home\nbefore=%v\nafter=%v", before, after)
	}
}

func TestArtifactReaderMissingRootsNeverCreatesThem(t *testing.T) {
	homePath := canonicalArtifactReaderHome(t)
	reader, err := OpenArtifactReader(homePath)
	if err != nil {
		t.Fatal(err)
	}
	before := artifactReaderTreeSnapshot(t, homePath)
	missingRef := ArtifactRefPrefix + string(make([]byte, 64))
	if _, err := reader.Get(missingRef); err == nil {
		t.Fatal("Get accepted missing public root")
	}
	if _, err := reader.GetPrivate(missingRef); err == nil {
		t.Fatal("GetPrivate accepted missing private root")
	}
	if after := artifactReaderTreeSnapshot(t, homePath); !reflect.DeepEqual(after, before) {
		t.Fatalf("missing-root reads mutated home\nbefore=%v\nafter=%v", before, after)
	}
	for _, path := range []string{filepath.Join(homePath, "artifacts"), filepath.Join(homePath, "private")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("missing root was created at %s: %v", path, err)
		}
	}
}

func TestOpenArtifactReaderRejectsNonCanonicalOrUnsafeHomeWithoutMutation(t *testing.T) {
	canonical := canonicalArtifactReaderHome(t)
	missing := filepath.Join(canonical, "missing")
	alias := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(canonical, alias); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(canonical, "not-a-directory")
	if err := os.WriteFile(filePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	permissive := canonicalArtifactReaderHome(t)
	if err := os.Chmod(permissive, 0o755); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"blank": "", "relative": ".", "unclean": canonical + string(filepath.Separator) + ".",
		"symlink alias": alias, "missing": missing, "file": filePath, "permissive": permissive,
	} {
		t.Run(name, func(t *testing.T) {
			parent := filepath.Dir(path)
			if path == "" || path == "." {
				parent = canonical
			}
			before := artifactReaderTreeSnapshot(t, parent)
			if _, err := OpenArtifactReader(path); err == nil {
				t.Fatalf("OpenArtifactReader(%q) succeeded", path)
			}
			if after := artifactReaderTreeSnapshot(t, parent); !reflect.DeepEqual(after, before) {
				t.Fatalf("constructor failure mutated tree\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestArtifactReaderRejectsUnsafePrivateCorpusWithoutMutation(t *testing.T) {
	body := []byte("private corpus\n")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	ref := ArtifactRefPrefix + digest

	tests := []struct {
		name  string
		shape func(t *testing.T, homePath, privateDir, target string)
	}{
		{"corrupt content", func(t *testing.T, _, _, target string) {
			writeArtifactReaderFile(t, target, []byte("corrupt\n"), 0o600)
		}},
		{"unsafe file mode", func(t *testing.T, _, _, target string) { writeArtifactReaderFile(t, target, body, 0o644) }},
		{"directory substitution", func(t *testing.T, _, _, target string) {
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink substitution", func(t *testing.T, homePath, _, target string) {
			outside := filepath.Join(filepath.Dir(homePath), "outside-private-object")
			writeArtifactReaderFile(t, outside, body, 0o600)
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
		}},
		{"unsafe directory mode", func(t *testing.T, _, privateDir, target string) {
			writeArtifactReaderFile(t, target, body, 0o600)
			if err := os.Chmod(filepath.Dir(privateDir), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink directory substitution", func(t *testing.T, homePath, privateDir, _ string) {
			if err := os.RemoveAll(filepath.Dir(privateDir)); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(filepath.Dir(homePath), "outside-artifacts")
			if err := os.MkdirAll(filepath.Join(outside, "sha256"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeArtifactReaderFile(t, filepath.Join(outside, "sha256", digest), body, 0o600)
			if err := os.Symlink(outside, filepath.Dir(privateDir)); err != nil {
				t.Fatal(err)
			}
		}},
		{"removal residue", func(t *testing.T, _, _, target string) {
			writeArtifactReaderFile(t, target, body, 0o600)
			if err := os.Link(target, target+rootedRemovalPendingSuffix); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := canonicalArtifactReaderHome(t)
			privateDir := filepath.Join(homePath, "private", "artifacts", "sha256")
			if err := os.MkdirAll(privateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{filepath.Join(homePath, "private"), filepath.Join(homePath, "private", "artifacts"), privateDir} {
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(privateDir, digest)
			tc.shape(t, homePath, privateDir, target)
			reader, err := OpenArtifactReader(homePath)
			if err != nil {
				t.Fatal(err)
			}
			before := artifactReaderTreeSnapshot(t, filepath.Dir(homePath))
			if _, err := reader.GetPrivate(ref); err == nil {
				t.Fatal("GetPrivate accepted unsafe private corpus")
			}
			if after := artifactReaderTreeSnapshot(t, filepath.Dir(homePath)); !reflect.DeepEqual(after, before) {
				t.Fatalf("private refusal mutated tree\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func writeArtifactReaderFile(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func canonicalArtifactReaderHome(t *testing.T) string {
	t.Helper()
	homePath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	return homePath
}

func exportedArtifactReaderMethods() []string {
	typ := reflect.TypeOf((*ArtifactReader)(nil))
	methods := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		methods = append(methods, typ.Method(i).Name)
	}
	return methods
}

func artifactReaderTreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += ":" + target
		case info.Mode().IsRegular():
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += ":" + string(body)
		}
		snapshot[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
