package work

import (
	"os"
	"path/filepath"
	"testing"
)

// The events dir on a real home holds ~50x more tr_/act_/pol_/hook_ files than
// receipts; hold-evidence must open only receipt-named files, not the whole dir.
func TestHoldEvidenceReadsOnlyReceiptNamedFiles(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, "events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"tr_aaaaaaaaaaaa.json", "tr_bbbbbbbbbbbb.json", "act_cccccccccccc.json",
		"pol_dddddddddddd.json", "hook_eeeeeeeeeeee.json", "trace_ffffffffffff.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"rec_111111111111.json", "rec_222222222222.json", "rec_333333333333.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s.workHoldEvidenceIndex()

	if got := s.holdEvidenceFileReadCount(); got != 3 {
		t.Fatalf("hold-evidence opened %d event files, want 3 (only rec_-named files)", got)
	}
}
