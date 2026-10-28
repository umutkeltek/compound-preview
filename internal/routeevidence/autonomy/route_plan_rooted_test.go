package autonomy_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileFailsClosedOnSymlinkedRoutePlanAuthority(t *testing.T) {
	t.Run("leaf", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		authorityRaw, err := os.ReadFile(plan.AuthorityPath)
		if err != nil {
			t.Fatal(err)
		}
		externalPath := filepath.Join(t.TempDir(), "model-capabilities.yaml")
		if err := os.WriteFile(externalPath, authorityRaw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(plan.AuthorityPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalPath, plan.AuthorityPath); err != nil {
			t.Fatal(err)
		}

		if _, err := compileArtifact(home, plan, 1); err == nil || !strings.Contains(err.Error(), "model-router authority file must be a regular file") {
			t.Fatalf("symlinked model-router authority leaf did not fail closed: %v", err)
		}
		assertExternalFileUnchanged(t, externalPath, authorityRaw)
	})

	t.Run("ancestor", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		authorityRaw, err := os.ReadFile(plan.AuthorityPath)
		if err != nil {
			t.Fatal(err)
		}
		externalDir := t.TempDir()
		externalPath := filepath.Join(externalDir, "model-capabilities.yaml")
		if err := os.WriteFile(externalPath, authorityRaw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(plan.AuthorityPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Dir(plan.AuthorityPath)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalDir, filepath.Dir(plan.AuthorityPath)); err != nil {
			t.Fatal(err)
		}

		if _, err := compileArtifact(home, plan, 1); err == nil || !strings.Contains(err.Error(), "model-router authority path must not contain symlink beneath compound home") {
			t.Fatalf("symlinked model-router authority ancestor did not fail closed: %v", err)
		}
		assertExternalFileUnchanged(t, externalPath, authorityRaw)
	})
}

func assertExternalFileUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read external fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("external fixture changed:\n got %q\nwant %q", got, want)
	}
}
