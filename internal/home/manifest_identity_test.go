package home

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadManifestNoFollowBoundedRejectsReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(path, []byte("schema_version: compound.home.v0.1\nhome_kind: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement.yaml")
	if err := os.WriteFile(replacement, []byte("schema_version: compound.home.v0.1\nhome_kind: local_monolith\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHook := manifestOpenAfterStatHook
	manifestOpenAfterStatHook = func(gotPath string) error {
		if gotPath != path {
			t.Fatalf("hook path = %q, want %q", gotPath, path)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	defer func() { manifestOpenAfterStatHook = oldHook }()

	if _, err := readManifestNoFollowBounded(path); err == nil {
		t.Fatal("bounded no-follow read succeeded after replacement, want error")
	}
}

func TestValidateManifestIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(path, []byte("schema_version: compound.home.v0.1\nhome_kind: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifestIdentity(path, before, opened, after); err != nil {
		t.Fatalf("validate unchanged identity: %v", err)
	}
}

func TestValidateManifestIdentityRejectsReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "replacement"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "replacement"), path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifestIdentity(path, before, opened, after); err == nil {
		t.Fatal("validate replacement identity succeeded, want error")
	}
}

func TestValidateManifestIdentityRejectsSymlinkMismatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manifest.yaml")
	target := filepath.Join(root, "target.yaml")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifestIdentity(path, before, opened, after); err == nil {
		t.Fatal("validate symlink mismatch succeeded, want error")
	}
}
