package main

import (
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/product"
)

// End-to-end through the CLI door: record → list → reject → list. Proves the verbs
// are wired and that a rejected draft stays listable (retained exhaust). RED if the
// content-draft case is unwired, or if reject drops the record from the listing.
func TestProductContentDraftRecordListRejectCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	recordOut := captureStdout(t, func() {
		cmdProduct([]string{
			"content-draft", "record",
			"--product", "product:prod_synth_a",
			"--platform", "x",
			"--text", "launch-day thread for prod_synth_a",
			"--voice-pack", "voice:prod_synth_a_v1",
			"--evidence", "finding:synth_a,source:synth_a",
			"--ttl", "72h",
		})
	})
	if !strings.Contains(recordOut, "state: proposed") {
		t.Fatalf("record did not report proposed state:\n%s", recordOut)
	}

	listOut := captureStdout(t, func() {
		cmdProduct([]string{"content-draft", "list", "--json"})
	})
	var drafts []product.ContentDraft
	if err := json.Unmarshal([]byte(listOut), &drafts); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, listOut)
	}
	if len(drafts) != 1 {
		t.Fatalf("want 1 recorded draft, got %d: %+v", len(drafts), drafts)
	}
	if drafts[0].State != product.ContentDraftStateProposed {
		t.Fatalf("recorded draft state = %q, want proposed", drafts[0].State)
	}
	if drafts[0].BodyRef == "" || drafts[0].ExpiresAt == "" {
		t.Fatalf("recorded draft missing body ref or expiry: %+v", drafts[0])
	}
	id := drafts[0].ID

	rejectOut := captureStdout(t, func() {
		cmdProduct([]string{"content-draft", "reject", id})
	})
	if !strings.Contains(rejectOut, "state: rejected") {
		t.Fatalf("reject did not report rejected state:\n%s", rejectOut)
	}

	afterOut := captureStdout(t, func() {
		cmdProduct([]string{"content-draft", "list", "--json"})
	})
	var after []product.ContentDraft
	if err := json.Unmarshal([]byte(afterOut), &after); err != nil {
		t.Fatalf("decode post-reject list JSON: %v\n%s", err, afterOut)
	}
	if len(after) != 1 || after[0].ID != id {
		t.Fatalf("rejected draft not retained in listing: %+v", after)
	}
	if after[0].State != product.ContentDraftStateRejected {
		t.Fatalf("retained draft state = %q, want rejected", after[0].State)
	}
}
