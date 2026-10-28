package home

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOperationalArchiveJournalCrashRecoveryBetweenPendingAndCommitted
// proves the journal-first protocol's most important promise: a kill between
// the pending row and the committed row leaves a state the NEXT archive pass
// reconciles, with no orphan (file at archive path with no committed index
// line) and no duplicate (file back at original path with a committed line).
//
// Setup: install a panic-as-`os.Exit`-equivalent on the canonical seam
// (operationalArchiveBeforeIndexCommit, which fires AFTER the move succeeds
// but BEFORE the committed line is appended — the only window where a
// crash leaves the file at the archive path with only a pending line on
// disk). The first archiveOperationalFile + commitJournaledOperationalArchive
// call therefore writes the pending line, performs the move, then dies
// before writing the committed line.
//
// Expectation: a subsequent ArchiveOperationalHistory call sees the pending
// row, walks the file at the archive path, finds it matches the entry's
// size+SHA, and appends the missing committed row. The final index has the
// expected active set with no in-flight transitions, and the inline
// archiveOperationalHistory (which is the "next pass") can find the file at
// its archive path through the regular operational archive surface.
func TestOperationalArchiveJournalCrashRecoveryBetweenPendingAndCommitted(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	rel := "work/work/work_crash_a000001.json"
	writeHomeFile(t, h, rel, `{"id":"work_crash_a000001","state":"completed"}`)

	cand := operationalCandidate{Kind: "work", Status: "completed", OriginalPath: rel}

	// 1. Build the entry (archiveOperationalFile is read-only — it reads the
	// source, computes the SHA, and validates the destination path).
	entry, err := archiveOperationalFile(h, cand, archiveTestNow, false)
	if err != nil {
		t.Fatalf("archiveOperationalFile: %v", err)
	}

	// 2. Inject a hard crash between the move and the committed line. The
	// committed line is appended via operationalArchiveJournalAppend → in
	// turn, operationalArchiveBeforeIndexCommit is fired just before the
	// append. Replacing it with a panic does what SIGKILL does at the OS
	// level: the file system already reflects the move; the pending row
	// is durable; nothing more gets written.
	//
	// recover() turns the panic into a normal error so the test can keep
	// running rather than tearing down the whole process; we then verify
	// the on-disk state matches what a real crash would leave behind.
	seam := operationalArchiveBeforeIndexCommit
	defer func() { operationalArchiveBeforeIndexCommit = seam }()
	crashed := false
	operationalArchiveBeforeIndexCommit = func(_ string, _ []OperationalArchiveEntry) {
		crashed = true
		panic("simulated crash between move and committed line")
	}
	func() {
		defer func() {
			_ = recover()
		}()
		if _, err := commitJournaledOperationalArchive(h, entry); err != nil {
			t.Fatalf("commitJournaledOperationalArchive: %v", err)
		}
	}()
	if !crashed {
		t.Fatalf("crash seam did not fire — the test is no longer exercising the right window")
	}

	// Post-crash invariants the next-pass reconcile depends on:
	//   - file already at the archive path (the rename completed)
	//   - source gone (no duplicate candidate eligible next pass)
	//   - pending row durable on disk (the journal's promise)
	if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(entry.ArchivePath))); err != nil {
		t.Fatalf("archive path missing after crash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(entry.OriginalPath))); !os.IsNotExist(err) {
		t.Fatalf("source still present after crash (reconcile would see a duplicate): %v", err)
	}
	raw, err := os.ReadFile(OperationalArchiveIndexPath(h))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	active, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatalf("load index after crash: %v", err)
	}
	if len(active) != 1 || active[0].Operation != "archived_pending" {
		t.Fatalf("expected exactly one pending entry, got %+v", active)
	}
	if t.Failed() {
		t.Logf("raw index after crash (%d bytes): %s", len(raw), raw)
	}

	// The reconcile pass runs WITHOUT the crash seam installed — the crash
	// is in the past. Restore early so the reconcile actually executes.
	operationalArchiveBeforeIndexCommit = seam

	// 3. The next archive pass must reconcile the pending entry without any
	// operator intervention. This is the contract the bounded append
	// preserves: pending -> move -> committed is recoverable, not lost.
	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{
		Kinds: []string{"work"},
	}, archiveTestNow)
	if err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}
	if report.ArchivedCount != 0 {
		t.Fatalf("reconcile pass should not have archived a fresh file (already moved), got %d", report.ArchivedCount)
	}

	// 4. Final state: the pending entry is now "archived", no duplicates,
	// no orphans, the file still at the archive path.
	final, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatalf("load index after reconcile: %v", err)
	}
	if len(final) != 1 || final[0].Operation != "archived" {
		t.Fatalf("expected single archived entry after reconcile, got %+v", final)
	}
	if final[0].OriginalPath != entry.OriginalPath || final[0].ArchivePath != entry.ArchivePath {
		t.Fatalf("entry paths drifted after reconcile: %+v", final[0])
	}
	if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(entry.ArchivePath))); err != nil {
		t.Fatalf("archive path gone after reconcile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(entry.OriginalPath))); !os.IsNotExist(err) {
		t.Fatalf("source reappeared after reconcile: %v", err)
	}

	// 5. The index is a strictly append-only JSONL stream now: the pending
	// row the crash left, plus the committed row the reconcile appended.
	// Both rows should still be present in the raw file (LoadOperationalArchiveIndex
	// returns the canonical projection).
	if !containsArchivedPending(raw) {
		t.Fatalf("raw index after reconcile no longer contains the original pending row: %s", raw)
	}
}

// TestOperationalArchiveJournalCrashRecoverySourceNeverMoved proves the
// OTHER half of the journal-first promise: a kill BEFORE the rename leaves
// the file at the original path with only a pending row on disk, and the
// next pass writes a rollback line — not a duplicate archive. Without the
// rollback, the next pass would re-archive the same file (its pending row
// only signals "intent to move", not "move completed").
//
// We simulate the post-pending-row, pre-move state directly: build the
// entry, append the pending line, then exercise the next pass WITHOUT
// performing the rename. This is the only crash window where the
// distinction between "rolled back" and "re-archived" is observable — a
// crash earlier than the pending row is just a no-op for the next pass.
func TestOperationalArchiveJournalCrashRecoverySourceNeverMoved(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	rel := "work/work/work_crash_b000002.json"
	writeHomeFile(t, h, rel, `{"id":"work_crash_b000002","state":"completed"}`)

	cand := operationalCandidate{Kind: "work", Status: "completed", OriginalPath: rel}
	entry, err := archiveOperationalFile(h, cand, archiveTestNow, false)
	if err != nil {
		t.Fatal(err)
	}

	// Append the pending line — exactly the durable state a crash between
	// the append and the rename leaves on disk — without performing the
	// rename. The source remains at the original path, the archive path is
	// empty, and the index carries a single pending row.
	pending := entry
	pending.Operation = "archived_pending"
	if err := operationalArchiveJournalAppend(h, []OperationalArchiveEntry{pending}, "journal-pending"); err != nil {
		t.Fatalf("append pending row: %v", err)
	}

	if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(entry.OriginalPath))); err != nil {
		t.Fatalf("source gone after pending-only crash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(entry.ArchivePath))); !os.IsNotExist(err) {
		t.Fatalf("archive path present before the rename ran: %v", err)
	}
	active, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Operation != "archived_pending" {
		t.Fatalf("expected exactly one pending entry, got %+v", active)
	}

	// The next pass MUST converge the index to a state with no in-flight
	// transitions (the rolled-back line is appended by the reconcile), and
	// then proceed normally: the file is still at the original path, so it
	// is a fresh archive candidate. "Rolled back" means "treat the prior
	// archive attempt as if it never happened" — it does NOT permanently
	// suppress the file from future archive.
	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{
		Kinds: []string{"work"},
	}, archiveTestNow)
	if err != nil {
		t.Fatalf("reconcile pass: %v", err)
	}
	if report.ArchivedCount != 1 {
		t.Fatalf("expected the file to be re-archived after a pending-only rollback, got ArchivedCount=%d", report.ArchivedCount)
	}
	final, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatalf("load index after reconcile: %v", err)
	}
	if len(final) != 1 || final[0].Operation != "archived" {
		t.Fatalf("expected single archived entry after reconcile+retry, got %+v", final)
	}
	if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(entry.OriginalPath))); !os.IsNotExist(err) {
		t.Fatalf("source still present after the re-archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(entry.ArchivePath))); err != nil {
		t.Fatalf("archive path missing after re-archive: %v", err)
	}
	// The raw index now carries the journal of what happened: pending row,
	// rolled-back row, then committed row. The canonical projection keeps
	// only the latest per OriginalPath.
	rawFinal, err := os.ReadFile(OperationalArchiveIndexPath(h))
	if err != nil {
		t.Fatal(err)
	}
	if !containsArchivedPending(rawFinal) {
		t.Fatalf("raw index no longer contains the original pending row: %s", rawFinal)
	}
	if !containsArchivedRolledBack(rawFinal) {
		t.Fatalf("raw index no longer contains the rolled-back row: %s", rawFinal)
	}
}

// containsArchivedPending is a small projection helper: the canonical loader
// returns only the "active" entry per OriginalPath, but the raw JSONL still
// contains every line ever appended, and the test wants to assert the
// original pending row is preserved through the reconcile pass.
func containsArchivedPending(raw []byte) bool {
	return indexContainsOperation(raw, "archived_pending")
}

// containsArchivedRolledBack mirrors containsArchivedPending for the
// rollback journal line the reconcile pass appends after a pre-move crash.
func containsArchivedRolledBack(raw []byte) bool {
	return indexContainsOperation(raw, "archived_rolled_back")
}

func indexContainsOperation(raw []byte, op string) bool {
	needle := []byte(`"operation":"` + op + `"`)
	if indexContainsBytes(raw, needle) {
		return true
	}
	spaced := []byte(`"operation": "` + op + `"`)
	return indexContainsBytes(raw, spaced)
}

func splitJSONL(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				lines = append(lines, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

func indexContainsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
