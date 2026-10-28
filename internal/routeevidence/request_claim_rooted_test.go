package routeevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRequestClaimRejectsSymlinkedOwnedPath(t *testing.T) {
	t.Run("leaf", func(t *testing.T) {
		home, claim, raw := rootedRequestClaimFixture(t)
		claimPath := requestClaimPath(home, claim.RequestID)
		if err := os.MkdirAll(filepath.Dir(claimPath), 0o755); err != nil {
			t.Fatal(err)
		}
		externalPath := filepath.Join(t.TempDir(), "claim.json")
		if err := os.WriteFile(externalPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalPath, claimPath); err != nil {
			t.Fatal(err)
		}

		if _, _, err := loadRequestClaim(home, claim.RequestID); err == nil || !strings.Contains(err.Error(), "request claim file must be a regular file") {
			t.Fatalf("symlinked request claim leaf did not fail closed: %v", err)
		}
		assertRequestClaimExternalFileUnchanged(t, externalPath, raw)
	})

	t.Run("ancestor", func(t *testing.T) {
		home, claim, raw := rootedRequestClaimFixture(t)
		claimPath := requestClaimPath(home, claim.RequestID)
		claimsDir := filepath.Dir(claimPath)
		if err := os.MkdirAll(filepath.Dir(claimsDir), 0o755); err != nil {
			t.Fatal(err)
		}
		externalDir := t.TempDir()
		externalPath := filepath.Join(externalDir, filepath.Base(claimPath))
		if err := os.WriteFile(externalPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalDir, claimsDir); err != nil {
			t.Fatal(err)
		}

		if _, _, err := loadRequestClaim(home, claim.RequestID); err == nil || !strings.Contains(err.Error(), "request claim path must not contain symlink beneath compound home") {
			t.Fatalf("symlinked request claim ancestor did not fail closed: %v", err)
		}
		assertRequestClaimExternalFileUnchanged(t, externalPath, raw)
	})
}

func TestClaimRequestIDRejectsSymlinkedOwnedPathWithoutExternalMutation(t *testing.T) {
	t.Run("leaf", func(t *testing.T) {
		home, claim, _ := rootedRequestClaimFixture(t)
		claimPath := requestClaimPath(home, claim.RequestID)
		if err := os.MkdirAll(filepath.Dir(claimPath), 0o755); err != nil {
			t.Fatal(err)
		}
		externalPath := filepath.Join(t.TempDir(), "missing-claim.json")
		if err := os.Symlink(externalPath, claimPath); err != nil {
			t.Fatal(err)
		}

		if err := ClaimRequestID(home, claim); err == nil || !strings.Contains(err.Error(), "request claim file must be a regular file") {
			t.Fatalf("symlinked request claim create leaf did not fail closed: %v", err)
		}
		if _, err := os.Stat(externalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("request claim create mutated external leaf: %v", err)
		}
		assertSymlinkStillExists(t, claimPath)
	})

	t.Run("ancestor", func(t *testing.T) {
		home, claim, _ := rootedRequestClaimFixture(t)
		claimPath := requestClaimPath(home, claim.RequestID)
		claimsDir := filepath.Dir(claimPath)
		if err := os.MkdirAll(filepath.Dir(claimsDir), 0o755); err != nil {
			t.Fatal(err)
		}
		externalDir := t.TempDir()
		externalPath := filepath.Join(externalDir, filepath.Base(claimPath))
		if err := os.Symlink(externalDir, claimsDir); err != nil {
			t.Fatal(err)
		}

		if err := ClaimRequestID(home, claim); err == nil || !strings.Contains(err.Error(), "request claim path must not contain symlink beneath compound home") {
			t.Fatalf("symlinked request claim create ancestor did not fail closed: %v", err)
		}
		if _, err := os.Stat(externalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("request claim create mutated external ancestor: %v", err)
		}
	})
}

func TestReleaseRequestClaimRejectsSymlinkedOwnedPathWithoutExternalMutation(t *testing.T) {
	t.Run("leaf", func(t *testing.T) {
		home, claim, raw := rootedRequestClaimFixture(t)
		claimPath := requestClaimPath(home, claim.RequestID)
		if err := os.MkdirAll(filepath.Dir(claimPath), 0o755); err != nil {
			t.Fatal(err)
		}
		externalPath := filepath.Join(t.TempDir(), "claim.json")
		if err := os.WriteFile(externalPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalPath, claimPath); err != nil {
			t.Fatal(err)
		}

		if err := releaseRequestClaimLocked(home, claim); err == nil || !strings.Contains(err.Error(), "request claim file must be a regular file") {
			t.Fatalf("symlinked request claim release leaf did not fail closed: %v", err)
		}
		assertRequestClaimExternalFileUnchanged(t, externalPath, raw)
		assertSymlinkStillExists(t, claimPath)
	})

	t.Run("ancestor", func(t *testing.T) {
		home, claim, raw := rootedRequestClaimFixture(t)
		claimPath := requestClaimPath(home, claim.RequestID)
		claimsDir := filepath.Dir(claimPath)
		if err := os.MkdirAll(filepath.Dir(claimsDir), 0o755); err != nil {
			t.Fatal(err)
		}
		externalDir := t.TempDir()
		externalPath := filepath.Join(externalDir, filepath.Base(claimPath))
		if err := os.WriteFile(externalPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalDir, claimsDir); err != nil {
			t.Fatal(err)
		}

		if err := releaseRequestClaimLocked(home, claim); err == nil || !strings.Contains(err.Error(), "request claim path must not contain symlink beneath compound home") {
			t.Fatalf("symlinked request claim release ancestor did not fail closed: %v", err)
		}
		assertRequestClaimExternalFileUnchanged(t, externalPath, raw)
	})
}

func TestReleaseRequestClaimDoesNotDeleteReplacementAfterValidation(t *testing.T) {
	home, claim, _ := rootedRequestClaimFixture(t)
	if err := ClaimRequestID(home, claim); err != nil {
		t.Fatalf("claim request id: %v", err)
	}
	claimPath := requestClaimPath(home, claim.RequestID)
	originalPath := claimPath + ".validated"
	replacement := claim
	replacement.RequestID += ":replacement"
	replacement.ID = requestClaimID(replacement)
	replacementRaw, err := json.MarshalIndent(replacement, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	replacementRaw = append(replacementRaw, '\n')

	previous := requestClaimBeforeReleaseRemove
	defer func() { requestClaimBeforeReleaseRemove = previous }()
	var hookErr error
	requestClaimBeforeReleaseRemove = func() {
		if err := os.Rename(claimPath, originalPath); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(claimPath, replacementRaw, 0o644)
	}

	if err := releaseRequestClaimLocked(home, claim); err == nil {
		t.Error("request claim release deleted a file substituted after validation")
	}
	if hookErr != nil {
		t.Fatalf("replace validated request claim: %v", hookErr)
	}
	got, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatalf("substituted request claim was not restored: %v", err)
	}
	if !bytes.Equal(got, replacementRaw) {
		t.Fatalf("substituted request claim changed:\n got %q\nwant %q", got, replacementRaw)
	}
}

func rootedRequestClaimFixture(t *testing.T) (string, RequestClaim, []byte) {
	t.Helper()
	home := t.TempDir()
	ev := fixtureEvidence()
	claim, err := BuildRequestClaim(ev.RequestID, ev.WorkID, ev.ClientProfile, ev.Protocol, ev.SelectedTarget)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return home, claim, append(raw, '\n')
}

func assertRequestClaimExternalFileUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read external request claim fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("external request claim fixture changed:\n got %q\nwant %q", got, want)
	}
}

func assertSymlinkStillExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("path is no longer a symlink: %s", path)
	}
}
