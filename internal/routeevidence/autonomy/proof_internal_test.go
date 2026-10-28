package autonomy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBoundReceiptFailsClosedOnSymlinkedAncestor(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "events")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "envelopes", "receipt.distill.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "rec_distill_fixture.json"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, "events")); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := loadBoundReceipt(home, "rec_distill_fixture"); err == nil || !strings.Contains(err.Error(), "receipt path must not contain symlink beneath compound home") {
		t.Fatalf("symlinked receipt ancestor did not fail closed: %v", err)
	}
}
