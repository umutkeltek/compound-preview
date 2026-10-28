package runattempt

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestForWorkDiscoversOlderWriterAfterLookup(t *testing.T) {
	s := Open(t.TempDir())
	first, err := s.Mint("work_reverse_compatibility", "codex:test", "claude-code", byWorkNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForWork(first.WorkID); err != nil {
		t.Fatal(err)
	}
	// A pre-index writer, or a crash before index publication, leaves this
	// real main record without ever touching a newer runtime's index.
	hidden := first
	hidden.ID = MintDeterministic("older-writer-after-index", byWorkNow.Add(time.Second))
	hidden.CreatedAt = byWorkNow.Add(time.Second)
	if err := s.write(hidden); err != nil {
		t.Fatal(err)
	}
	got, err := Open(s.home).ForWork(first.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != hidden.ID {
		t.Fatalf("reverse-only authority hidden after older writer: %+v", got)
	}
}

func TestForWorkRejectsOwnedCorruptLaunchEvidence(t *testing.T) {
	s := Open(t.TempDir())
	ra, err := s.Mint("work_owned_corrupt_launch", "codex:test", "claude-code", byWorkNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForWork(ra.WorkID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, "run_attempts", "launch", ra.ID+".committed.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForWork(ra.WorkID); err == nil {
		t.Fatal("owned launch corruption hidden by cached record")
	}
}
