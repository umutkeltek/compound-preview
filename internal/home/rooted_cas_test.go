package home

import (
	"bytes"
	"os"
	"testing"
)

func TestRootedFileCASRollbackRejectsReplacedRecoveryLeaf(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	const canonicalName = "authority.jsonl"
	const recoveryName = ".authority.expected.replacement.swap"
	expected := []byte("expected\n")
	replacement := []byte("replacement\n")
	foreign := []byte("foreign\n")
	if err := root.WriteFile(canonicalName, expected, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(recoveryName, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	expectedInfo, err := root.Lstat(canonicalName)
	if err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := root.Lstat(recoveryName)
	if err != nil {
		t.Fatal(err)
	}

	publication, err := BeginRootedFileCAS(
		root,
		canonicalName,
		recoveryName,
		expected,
		expectedInfo,
		replacement,
		replacementInfo,
	)
	if err != nil {
		t.Fatalf("begin rooted CAS publication: %v", err)
	}
	if err := root.Remove(recoveryName); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(recoveryName, foreign, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := publication.Rollback(); err == nil {
		t.Fatal("rollback promoted a replacement recovery leaf")
	}
	if publication.State() != RootedFileCASAmbiguous {
		t.Fatalf("rollback state = %q, want %q", publication.State(), RootedFileCASAmbiguous)
	}
	canonical, err := root.ReadFile(canonicalName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, replacement) {
		t.Fatalf("ambiguous rollback changed canonical authority: got %q want %q", canonical, replacement)
	}
	recovery, err := root.ReadFile(recoveryName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recovery, foreign) {
		t.Fatalf("ambiguous rollback changed replacement recovery leaf: got %q want %q", recovery, foreign)
	}
}
