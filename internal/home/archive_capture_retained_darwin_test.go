//go:build darwin

package home

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit continuation of a retained synthetic scale run. It does not create
// or claim a new capture; it verifies the named archive and every surviving
// restored body, without hiding fixture cleanup in the application deadline.
func TestCapturedRetainedMillionOutcome(t *testing.T) {
	marker := os.Getenv("COMPOUND_CAPTURE_RETAINED_OUTCOME")
	if marker == "" {
		t.Skip("explicit retained scale evidence only")
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Schema             string `json:"schema"`
		Archive            string `json:"archive"`
		ArchiveSHA256      string `json:"archive_sha256"`
		Restored           string `json:"restored"`
		Files              int    `json:"files"`
		WriterTransactions int    `json:"writer_transactions"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Schema != "compound.synthetic.capture-outcome.v1" || input.Files != 1000000 || input.WriterTransactions < 2 || !filepath.IsAbs(input.Archive) || !filepath.IsAbs(input.Restored) || len(input.ArchiveSHA256) != 64 {
		t.Fatal("invalid retained synthetic outcome")
	}
	report, err := VerifyArchive(input.Archive)
	if err != nil || !report.OK || report.ArchiveSHA256 != input.ArchiveSHA256 {
		t.Fatalf("archive identity/verification: %+v %v", report, err)
	}
	if _, err := RestoreArchive(input.Archive, input.Restored, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Log("retained archive restored into caller-owned fixture")
	expected := fmt.Sprintf("generation:%d", input.WriterTransactions)
	for _, name := range []string{"a.json", "b.json"} {
		data, err := os.ReadFile(filepath.Join(input.Restored, "ledger", name))
		if err != nil || string(data) != expected {
			t.Fatalf("writer generation %s: %q %v", name, data, err)
		}
	}
	for i := 0; i < input.Files; i++ {
		path := filepath.Join(input.Restored, "context", "cardinality", fmt.Sprintf("%03d", i%256), fmt.Sprintf("%09d.json", i))
		data, err := os.ReadFile(path)
		if err != nil || string(data) != fmt.Sprintf("{\"fixture\":%d}\n", i) {
			t.Fatalf("restored body %d: %q %v", i, data, err)
		}
	}
	t.Logf("retained archive verified sha256=%s; all %d restored bodies and writer generation %d verified", input.ArchiveSHA256, input.Files, input.WriterTransactions)
}
