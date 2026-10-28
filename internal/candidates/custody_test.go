package candidates

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The read side never treats the payload's "kind" as custody metadata: kind
// selection is the caller's Kind argument, and a schema-owned body kind
// decodes as ordinary data. (The WRITE side's kind guard is the opposite
// contract, pinned by TestCandidateCodecGuards.)
func TestCandidateCustodyTreatsPayloadKindAsSchemaOwned(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, InboxDir(Memory))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "classified.json"),
		[]byte(`{"id":"classified","kind":"doctrine"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if err := ReadCandidate(home, Memory, "classified", &got); err != nil {
		t.Fatalf("schema-owned kind was treated as custody metadata: %v", err)
	}
	if got.ID != "classified" || got.Kind != "doctrine" {
		t.Fatalf("decoded candidate = %+v", got)
	}
}

// A refused point read returns a typed, named corruption: the exact directory
// entry and the stable reason code travel up to callers through errors.As.
func TestCandidateCorruptionIsTypedAndNamed(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, InboxDir(Memory))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blank.json"), []byte(`{"id":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var target struct {
		ID string `json:"id"`
	}
	err := ReadCandidate(home, Memory, "blank", &target)
	var corruption *Corruption
	if !errors.As(err, &corruption) {
		t.Fatalf("error = %v, want typed corruption", err)
	}
	if corruption.Code != CorruptionBlankID || corruption.EntryCursor != "blank.json" {
		t.Fatalf("corruption = %+v", corruption)
	}
}
