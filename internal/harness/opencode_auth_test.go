package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCodeAuthReadsOnlySelectedAPIEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	body := `{"selected":{"type":"api","key":"synthetic-selected"},"other":{"type":"api","key":"synthetic-other"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := readOpenCodeAPIKey(path, "selected")
	if err != nil || key != "synthetic-selected" {
		t.Fatalf("selected-only read failed: %v", err)
	}
	for _, provider := range []string{"missing", "other-missing"} {
		if _, err := readOpenCodeAPIKey(path, provider); err == nil {
			t.Fatal("missing entry accepted")
		}
	}
}

func TestOpenCodeAuthRejectsUnsafeSourcesWithoutSecretErrors(t *testing.T) {
	for name, body := range map[string]string{
		"oauth":     `{"selected":{"type":"oauth","key":"synthetic-secret"}}`,
		"empty":     `{"selected":{"type":"api","key":" "}}`,
		"newline":   `{"selected":{"type":"api","key":"synthetic-secret\nsecond"}}`,
		"malformed": `{"synthetic-secret":`,
		"large":     strings.Repeat("synthetic-secret", 100000),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			if os.WriteFile(path, []byte(body), 0o600) != nil {
				t.Fatal("write fixture")
			}
			if key, err := readOpenCodeAPIKey(path, "selected"); err == nil || key != "" || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatalf("unsafe source result key-present=%v err=%v", key != "", err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	_ = os.WriteFile(path, []byte(`{"selected":{"type":"api","key":"synthetic-secret"}}`), 0o644)
	if _, err := readOpenCodeAPIKey(path, "selected"); err == nil {
		t.Fatal("broad mode accepted")
	}
	_ = os.Chmod(path, 0o600)
	link := filepath.Join(t.TempDir(), "auth-link.json")
	if os.Symlink(path, link) != nil {
		t.Fatal("symlink fixture")
	}
	if _, err := readOpenCodeAPIKey(link, "selected"); err == nil {
		t.Fatal("symlink accepted")
	}
}
