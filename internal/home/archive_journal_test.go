package home

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestArchiveOperationalHistoryInterruptLeavesNoOrphans is the journal-first
// falsifier (audit finding LATEST-172 (3)): any interruption between the
// per-file move and the index commit must not leave files that no index line
// describes. On the legacy (post-all-moves commit) implementation the first
// archive pass leaves k moved files behind with no index entry — a silent
// orphan that `home archive-restore` cannot find.
func TestArchiveOperationalHistoryInterruptLeavesNoOrphans(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	const total = 6
	for i := 0; i < total; i++ {
		writeHomeFile(t, h,
			filepath.ToSlash(filepath.Join("work", "work", workName(i))),
			`{"id":"`+workName(i)+`","state":"completed"}`)
	}

	// Inject the simulated crash inside the journal-first flow: after the
	// per-file rename succeeds but before the committed index line lands.
	// This is the only window the journal-first design still has between a
	// move and durable index state — and the fix's promise is that even
	// THIS window cannot leave an orphan (a file with no index entry
	// describing it).
	abortErr := errors.New("simulated interrupt between per-file move and committed journal line")
	previousHook := operationalArchiveBeforeJournalCommit
	operationalArchiveBeforeJournalCommit = func(OperationalArchiveEntry) {
		// Only stop the FIRST archive pass; a rerun after reconcile must
		// complete so the test also proves the recovery path.
		panic(abortErr)
	}
	defer func() { operationalArchiveBeforeJournalCommit = previousHook }()

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected the simulated interrupt to fire")
			}
			if r != abortErr {
				panic(r)
			}
		}()
		_, _ = ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"work"}}, now)
	}()

	// After the simulated crash: figure out which candidates the legacy batch
	// completed before the seam fired.
	movedOriginals := make([]string, 0, total)
	stillHotOriginals := make([]string, 0, total)
	for i := 0; i < total; i++ {
		name := workName(i)
		rel := filepath.ToSlash(filepath.Join("work", "work", name))
		originalAbs := filepath.Join(h, filepath.FromSlash(rel))
		if _, err := os.Stat(originalAbs); os.IsNotExist(err) {
			movedOriginals = append(movedOriginals, rel)
		} else {
			stillHotOriginals = append(stillHotOriginals, rel)
		}
	}

	// An orphan is a moved file whose path is absent from the index. The
	// fix's promise is "no orphans after interruption"; the legacy code
	// violates it on every interrupted batch.
	index, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatalf("load index after interrupt: %v", err)
	}
	indexedOriginals := make(map[string]bool, len(index))
	for _, entry := range index {
		indexedOriginals[entry.OriginalPath] = true
	}
	var orphans []string
	for _, rel := range movedOriginals {
		if !indexedOriginals[rel] {
			orphans = append(orphans, rel)
		}
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Fatalf("interrupted archive pass left %d orphan(s) (moved but absent from index); fix should leave zero. moved=%v stillHot=%v index=%v",
			len(orphans), movedOriginals, stillHotOriginals, indexOriginals(index))
	}
}

func workName(i int) string {
	return "work_journal_" + intToASCII(i) + ".json"
}

func intToASCII(i int) string {
	if i < 0 || i > 25 {
		return "x"
	}
	return string(rune('a' + i))
}

func indexOriginals(entries []OperationalArchiveEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.OriginalPath)
	}
	sort.Strings(out)
	return out
}

// TestArchiveOperationalHistoryDryRunWritesNothing pins the second invariant
// the fix must keep green: `--dry-run` is a preview and must not journal any
// pending entries. Both before and after the journal-first change, the index
// file must remain empty (and unreadable as anything but "no active entries").
func TestArchiveOperationalHistoryDryRunWritesNothing(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	const total = 3
	for i := 0; i < total; i++ {
		writeHomeFile(t, h,
			filepath.ToSlash(filepath.Join("work", "work", "dry_work_"+intToASCII(i)+".json")),
			`{"id":"dry_work_`+intToASCII(i)+`","state":"completed"}`)
	}

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"work"}, DryRun: true}, now)
	if err != nil {
		t.Fatalf("dry-run archive failed: %v", err)
	}
	if report.ArchivedCount != total {
		t.Fatalf("dry-run archive report = %+v, want %d candidates", report, total)
	}
	index, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatalf("load index after dry-run: %v", err)
	}
	if len(index) != 0 {
		t.Fatalf("dry-run leaked %d active index entries; want 0: %+v", len(index), index)
	}
	indexPath := OperationalArchiveIndexPath(h)
	if data, err := os.ReadFile(indexPath); err == nil {
		if strings.TrimSpace(string(data)) != "" {
			t.Fatalf("dry-run wrote non-empty index file: %q", string(data))
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat index file after dry-run: %v", err)
	}
	for i := 0; i < total; i++ {
		rel := filepath.ToSlash(filepath.Join("work", "work", "dry_work_"+intToASCII(i)+".json"))
		if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("dry-run moved %s: %v", rel, err)
		}
	}
}
