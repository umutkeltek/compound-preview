package home

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestArtifactStorePutFileExpectedRejectsMismatchWithoutPublication(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	dump := []byte(strings.Repeat("postgres-dump-block\n", 256*1024))
	dumpPath := filepath.Join(t.TempDir(), "gbrain.dump")
	if err := os.WriteFile(dumpPath, dump, 0o600); err != nil {
		t.Fatal(err)
	}

	wrong := sha256.Sum256([]byte("different dump"))
	wrongRef := ArtifactRefPrefix + hex.EncodeToString(wrong[:])
	if _, _, err := store.PutFileExpected(dumpPath, wrongRef); err == nil || !strings.Contains(err.Error(), "content hash mismatch") {
		t.Fatalf("mismatch error = %v", err)
	}
	wrongHex, err := ArtifactHex(wrongRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "artifacts", "sha256", wrongHex)); !os.IsNotExist(err) {
		t.Fatalf("wrong artifact was published: %v", err)
	}

	matching := sha256.Sum256(dump)
	matchingRef := ArtifactRefPrefix + hex.EncodeToString(matching[:])
	ref, written, err := store.PutFileExpected(dumpPath, matchingRef)
	if err != nil {
		t.Fatal(err)
	}
	if ref != matchingRef || written != int64(len(dump)) {
		t.Fatalf("PutFileExpected = ref %q bytes %d", ref, written)
	}
	verified, err := store.Verify(matchingRef)
	if err != nil {
		t.Fatal(err)
	}
	if verified != int64(len(dump)) {
		t.Fatalf("Verify bytes = %d", verified)
	}
}

func TestArtifactStoreRejectsTraversalAndCorruptObjects(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(homePath, "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{
		"artifact:sha256:../../outside",
		"artifact:sha256:" + strings.Repeat("a", 63) + "/",
		"sha256:" + strings.Repeat("a", 64),
	} {
		if _, err := store.Get(ref); err == nil {
			t.Fatalf("hostile ref %q was accepted", ref)
		}
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "outside" {
		t.Fatalf("outside file changed: body=%q err=%v", got, err)
	}

	ref, err := store.Put([]byte("owned body"))
	if err != nil {
		t.Fatal(err)
	}
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "artifacts", "sha256", hexDigest), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ref); err == nil || !strings.Contains(err.Error(), "content hash mismatch") {
		t.Fatalf("corrupt artifact err = %v", err)
	}
}

func TestArtifactStorePutPrivateStoresOnlyPrivateObject(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("private only")
	ref, err := store.PutPrivate(body)
	if err != nil {
		t.Fatal(err)
	}
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(homePath, "artifacts", "sha256", hexDigest)
	if _, err := os.Stat(publicPath); !os.IsNotExist(err) {
		t.Fatalf("private artifact entered public root: %v", err)
	}
	privatePath := filepath.Join(homePath, "private", "artifacts", "sha256", hexDigest)
	info, err := os.Stat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private object mode = %04o, want 0600", info.Mode().Perm())
	}
	for _, path := range []string{
		filepath.Join(homePath, "private"),
		filepath.Join(homePath, "private", "artifacts"),
		filepath.Join(homePath, "private", "artifacts", "sha256"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %04o, want 0700", path, info.Mode().Perm())
		}
	}
	if _, err := store.Get(ref); err == nil {
		t.Fatalf("public Get succeeded for private-only ref")
	}
	got, err := store.GetPrivate(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("GetPrivate body = %q", got)
	}
	verified, err := store.VerifyPrivate(ref)
	if err != nil {
		t.Fatal(err)
	}
	if verified != int64(len(body)) {
		t.Fatalf("VerifyPrivate bytes = %d", verified)
	}
}

func TestArtifactStoreMaterializePrivateRootedUsesPrivateCustody(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Repeat("private custody\n", 64))
	ref, err := store.PutPrivate(body)
	if err != nil {
		t.Fatal(err)
	}
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "artifacts", "sha256", hexDigest)); !os.IsNotExist(err) {
		t.Fatalf("private materialize source exists in public root: %v", err)
	}
	targetPath := filepath.Join(homePath, "private", "targets", "nested", "body")
	written, existed, err := store.MaterializePrivateRooted(ref, targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if existed || written != int64(len(body)) {
		t.Fatalf("first materialize = bytes %d existed %v", written, existed)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil || string(got) != string(body) {
		t.Fatalf("target body = %q err %v", got, err)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private target mode = %04o, want 0600", info.Mode().Perm())
	}
	written, existed, err = store.MaterializePrivateRooted(ref, targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !existed || written != int64(len(body)) {
		t.Fatalf("replay materialize = bytes %d existed %v", written, existed)
	}
}

func TestArtifactStoreMaterializePrivateRootedRejectsPublicOnlyRef(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put([]byte("public only"))
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(homePath, "private", "targets", "public")
	if _, _, err := store.MaterializePrivateRooted(ref, targetPath); err == nil {
		t.Fatalf("private materialized public-only ref")
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("target was published from public root: %v", err)
	}
}

func TestArtifactStoreMaterializePrivateRootedRejectsHostilePaths(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutPrivate([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "secret")
	if _, _, err := store.MaterializePrivateRooted(ref, outsidePath); err == nil || !strings.Contains(err.Error(), "beneath compound home") {
		t.Fatalf("outside home err = %v", err)
	}
	if _, err := os.Stat(outsidePath); !os.IsNotExist(err) {
		t.Fatalf("outside target was created: %v", err)
	}
	ancestorTarget := filepath.Join(homePath, "linkdir", "secret")
	if err := os.Symlink(outsideDir, filepath.Join(homePath, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MaterializePrivateRooted(ref, ancestorTarget); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink ancestor err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "secret")); !os.IsNotExist(err) {
		t.Fatalf("symlink ancestor escaped: %v", err)
	}
	leafDir := filepath.Join(homePath, "leaf")
	if err := os.MkdirAll(leafDir, 0o700); err != nil {
		t.Fatal(err)
	}
	leafTarget := filepath.Join(leafDir, "secret")
	if err := os.Symlink(filepath.Join(outsideDir, "leaf-secret"), leafTarget); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MaterializePrivateRooted(ref, leafTarget); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink leaf err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "leaf-secret")); !os.IsNotExist(err) {
		t.Fatalf("symlink leaf escaped: %v", err)
	}
}

func TestArtifactStoreMaterializePrivateRootedRefusesPermissiveAncestors(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		path func(string) string
	}{
		{name: "parent0755", mode: 0o755, path: func(homePath string) string { return filepath.Join(homePath, "private", "targets") }},
		{name: "nested0777", mode: 0o777, path: func(homePath string) string { return filepath.Join(homePath, "private", "targets", "nested") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			store, err := NewArtifactStore(homePath)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := store.PutPrivate([]byte("secret"))
			if err != nil {
				t.Fatal(err)
			}
			permissivePath := tc.path(homePath)
			if err := os.MkdirAll(permissivePath, tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(permissivePath, tc.mode); err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(homePath, "private", "targets", "nested", "secret")
			if _, _, err := store.MaterializePrivateRooted(ref, targetPath); err == nil || !strings.Contains(err.Error(), "non-private mode") {
				t.Fatalf("permissive ancestor err = %v", err)
			}
			if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
				t.Fatalf("target was published: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(targetPath))
			if tc.name == "parent0755" {
				if !os.IsNotExist(err) {
					t.Fatalf("nested dir created under permissive parent: entries=%v err=%v", entries, err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Fatalf("unexpected files published in target parent: %v", entries)
			}
			info, err := os.Stat(permissivePath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != tc.mode {
				t.Fatalf("permissive dir mode changed = %04o want %04o", info.Mode().Perm(), tc.mode)
			}
		})
	}
}

func TestArtifactStorePrivateAPIsFailClosedOnPermissivePrivateRoot(t *testing.T) {
	homePath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homePath, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(homePath, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPrivate([]byte("secret")); err == nil || !strings.Contains(err.Error(), "non-private mode") {
		t.Fatalf("PutPrivate permissive root err = %v", err)
	}
	info, err := os.Stat(filepath.Join(homePath, "private"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("private root mode changed = %04o", info.Mode().Perm())
	}
}

func TestArtifactStoreMaterializePrivateRootedRejectsHostileSourceObject(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutPrivate([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(homePath, "private", "artifacts", "sha256", hexDigest)
	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(homePath, "other"), objectPath); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(homePath, "private", "targets", "secret")
	if _, _, err := store.MaterializePrivateRooted(ref, targetPath); err == nil || !(strings.Contains(err.Error(), "regular file") || strings.Contains(err.Error(), "no such file")) {
		t.Fatalf("source symlink err = %v", err)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("target was published from hostile source: %v", err)
	}
}

func TestArtifactStoreMaterializePrivateRootedRefusesPermissiveExistingTarget(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("secret")
	ref, err := store.PutPrivate(body)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(homePath, "private", "targets", "secret")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, existed, err := store.MaterializePrivateRooted(ref, targetPath); err == nil || !existed || !strings.Contains(err.Error(), "non-private mode") {
		t.Fatalf("permissive existing = existed %v err %v", existed, err)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("target mode changed = %04o", info.Mode().Perm())
	}
}

func TestArtifactStoreMaterializePrivateRootedRetainsDifferentTarget(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutPrivate([]byte("new secret"))
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(homePath, "private", "targets", "secret")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("original secret")
	if err := os.WriteFile(targetPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, existed, err := store.MaterializePrivateRooted(ref, targetPath); err == nil || !existed || !strings.Contains(err.Error(), "non-identical") {
		t.Fatalf("different existing = existed %v err %v", existed, err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil || string(got) != string(original) {
		t.Fatalf("different target changed: body=%q err=%v", got, err)
	}
}

func TestArtifactStorePrivateAPIsRejectHostileExistingObjects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(string, string) error
		want  string
	}{
		{
			name: "symlink",
			setup: func(objectPath, homePath string) error {
				if err := os.Remove(objectPath); err != nil {
					return err
				}
				return os.Symlink(filepath.Join(homePath, "elsewhere"), objectPath)
			},
			want: "regular file",
		},
		{
			name: "directory",
			setup: func(objectPath, homePath string) error {
				if err := os.Remove(objectPath); err != nil {
					return err
				}
				return os.Mkdir(objectPath, 0o600)
			},
			want: "regular file",
		},
		{
			name: "permissive0644",
			setup: func(objectPath, homePath string) error {
				return os.Chmod(objectPath, 0o644)
			},
			want: "non-private mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			store, err := NewArtifactStore(homePath)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte("secret")
			ref, err := store.PutPrivate(body)
			if err != nil {
				t.Fatal(err)
			}
			hexDigest, err := ArtifactHex(ref)
			if err != nil {
				t.Fatal(err)
			}
			objectPath := filepath.Join(homePath, "private", "artifacts", "sha256", hexDigest)
			if err := tc.setup(objectPath, homePath); err != nil {
				t.Fatal(err)
			}
			if _, err := store.GetPrivate(ref); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GetPrivate err = %v, want %q", err, tc.want)
			}
			if _, err := store.VerifyPrivate(ref); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("VerifyPrivate err = %v, want %q", err, tc.want)
			}
			if _, err := store.PutPrivate(body); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PutPrivate existing err = %v, want %q", err, tc.want)
			}
			targetPath := filepath.Join(homePath, "private", "targets", "secret")
			if _, _, err := store.MaterializePrivateRooted(ref, targetPath); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MaterializePrivateRooted err = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
				t.Fatalf("target was published from hostile source: %v", err)
			}
		})
	}
}

func TestArtifactStorePutPrivateConcurrentIdenticalDeduplicates(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Repeat("concurrent secret\n", 1024))
	const workers = 24
	refs := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			refs[i], errs[i] = store.PutPrivate(body)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("PutPrivate[%d] err = %v", i, err)
		}
		if refs[i] != refs[0] {
			t.Fatalf("PutPrivate[%d] ref = %q, want %q", i, refs[i], refs[0])
		}
	}
	hexDigest, err := ArtifactHex(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(homePath, "private", "artifacts", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	objects := 0
	for _, entry := range entries {
		if entry.Name() == hexDigest {
			objects++
		} else if strings.HasPrefix(entry.Name(), ".artifact-") {
			t.Fatalf("temporary artifact leaked: %s", entry.Name())
		}
	}
	if objects != 1 {
		t.Fatalf("private object count for digest = %d, want 1", objects)
	}
}

func TestArtifactStoreMaterializeGeneralBehaviorUnchanged(t *testing.T) {
	homePath := t.TempDir()
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("public artifact")
	ref, err := store.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "public", "body")
	written, existed, err := store.Materialize(ref, targetPath, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if existed || written != int64(len(body)) {
		t.Fatalf("general materialize = bytes %d existed %v", written, existed)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("general target mode = %04o, want 0644", info.Mode().Perm())
	}
	written, existed, err = store.Materialize(ref, targetPath, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if !existed || written != int64(len(body)) {
		t.Fatalf("general replay = bytes %d existed %v", written, existed)
	}
}
