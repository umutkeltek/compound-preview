package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPermissionAuthorityRejectsSymlinkedCustody(t *testing.T) {
	for _, leaf := range []bool{true, false} {
		name := "parent"
		if leaf {
			name = "leaf"
		}
		t.Run(name, func(t *testing.T) {
			homePath := t.TempDir()
			writePermissionFixture(t, homePath, "/synthetic/settings.json", permissionFixture([]string{"Read"}))
			before, revision, err := LoadPermissionAuthority(homePath, "claude_code")
			if err != nil || revision == "" {
				t.Fatalf("valid regular-file control: revision=%q error=%v", revision, err)
			}
			path := PermissionAuthorityPath(homePath, "claude_code")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			source := path
			external := filepath.Join(t.TempDir(), "external.yaml")
			if !leaf {
				source = filepath.Dir(path)
				external = filepath.Join(t.TempDir(), "external-authorities")
			}
			if err := os.Rename(source, external); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, source); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(body, got) {
				t.Fatalf("symlink fixture must retain valid bytes: %v", err)
			}
			assertCustodyRejected := func() {
				t.Helper()
				_, _, err := LoadPermissionAuthority(homePath, "claude_code")
				want := "permission authority path must not contain symlink beneath compound home"
				if leaf {
					want = "permission authority file must be a regular file"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("escaped authority accepted or rejected for wrong reason: %v", err)
				}
			}
			assertCustodyRejected()
			// Direct writes to an escaped referent cannot become accepted authority.
			externalFile := external
			if !leaf {
				externalFile = filepath.Join(external, filepath.Base(path))
			}
			before.Permissions.Allow = []string{"Bash"}
			if err := before.Validate(); err != nil {
				t.Fatal(err)
			}
			writeTestJSON(t, externalFile, before)
			assertCustodyRejected()
		})
	}
}
