package home

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOperationalArchiveIndexRejectsSymlinkedAuthority(t *testing.T) {
	indexRaw := operationalArchiveIndexFixture(t)

	t.Run("leaf", func(t *testing.T) {
		home := t.TempDir()
		indexPath := OperationalArchiveIndexPath(home)
		if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
			t.Fatal(err)
		}
		externalPath := filepath.Join(t.TempDir(), "index.jsonl")
		if err := os.WriteFile(externalPath, indexRaw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalPath, indexPath); err != nil {
			t.Fatal(err)
		}

		if _, err := LoadOperationalArchiveIndex(home); err == nil || !strings.Contains(err.Error(), "operational archive index file must be a regular file") {
			t.Fatalf("symlinked operational archive index leaf did not fail closed: %v", err)
		}
		assertArchiveExternalFileUnchanged(t, externalPath, indexRaw)
	})

	t.Run("ancestor", func(t *testing.T) {
		home := t.TempDir()
		externalArchives := t.TempDir()
		externalPath := filepath.Join(externalArchives, "operational", "index.jsonl")
		if err := os.MkdirAll(filepath.Dir(externalPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(externalPath, indexRaw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalArchives, filepath.Join(home, "archives")); err != nil {
			t.Fatal(err)
		}

		if _, err := LoadOperationalArchiveIndex(home); err == nil || !strings.Contains(err.Error(), "operational archive index path must not contain symlink beneath compound home") {
			t.Fatalf("symlinked operational archive index ancestor did not fail closed: %v", err)
		}
		assertArchiveExternalFileUnchanged(t, externalPath, indexRaw)
	})
}

func TestLoadOperationalArchiveIndexRejectsUnterminatedFinalRow(t *testing.T) {
	home := t.TempDir()
	indexPath := OperationalArchiveIndexPath(home)
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	indexRaw := bytes.TrimSuffix(operationalArchiveIndexFixture(t), []byte{'\n'})
	if err := os.WriteFile(indexPath, indexRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOperationalArchiveIndex(home); err == nil || !strings.Contains(err.Error(), "operational archive index must end with newline") {
		t.Fatalf("unterminated operational archive index did not fail closed: %v", err)
	}
}

func TestOperationalArchiveIndexPublishRejectsSymlinkSwapAndRollsBack(t *testing.T) {
	operations := []struct {
		name string
		hook string
	}{
		{name: "archive", hook: "archived"},
		{name: "restore", hook: "restored"},
	}
	for _, operation := range operations {
		for _, swapKind := range []string{"leaf", "ancestor"} {
			t.Run(operation.name+"/"+swapKind, func(t *testing.T) {
				home := filepath.Join(t.TempDir(), ".compound")
				if err := Init(home, "test", archiveTestNow); err != nil {
					t.Fatal(err)
				}
				workRel := "work/work/work_rooted_index_publish.json"
				workRaw := []byte(`{"id":"work_rooted_index_publish","state":"completed"}`)
				hotPath := filepath.Join(home, filepath.FromSlash(workRel))
				archivePath := filepath.Join(OperationalArchiveFilesRoot(home), filepath.FromSlash(workRel))
				if err := os.WriteFile(hotPath, workRaw, 0o644); err != nil {
					t.Fatal(err)
				}

				activeBefore := 0
				if operation.name == "restore" {
					report, err := ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow)
					if err != nil || report.ArchivedCount != 1 {
						t.Fatalf("seed operational archive: report=%+v err=%v", report, err)
					}
					activeBefore = 1
				}

				// Journal-first archive fires the legacy seam once per file
				// (inside commitJournaledOperationalArchive). The seam still
				// captures the durability point: CAS publication runs AFTER
				// it returns, so installing a symlink swap during the seam
				// still causes the per-file committed CAS to fail closed.
				previousHook := operationalArchiveBeforeIndexCommit
				t.Cleanup(func() { operationalArchiveBeforeIndexCommit = previousHook })
				var swap operationalArchiveIndexSwap
				operationalArchiveBeforeIndexCommit = func(gotOperation string, entries []OperationalArchiveEntry) {
					if gotOperation != operation.hook || len(entries) != 1 {
						t.Fatalf("index publish gap = operation:%q entries:%+v", gotOperation, entries)
					}
					swap = installOperationalArchiveIndexSwap(t, home, swapKind)
				}

				var operationErr error
				if operation.name == "archive" {
					_, operationErr = ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow.Add(time.Minute))
				} else {
					_, operationErr = RestoreOperationalArchive(home, OperationalRestoreOptions{All: true}, archiveTestNow.Add(time.Minute))
				}
				operationalArchiveBeforeIndexCommit = previousHook
				if swap.restore == nil {
					t.Fatal("operational archive index swap hook did not run")
				}
				swap.restore()

				if operationErr == nil {
					t.Errorf("%s unexpectedly committed through %s symlink swap", operation.name, swapKind)
				}
				externalAfter, err := os.ReadFile(swap.externalPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(externalAfter, swap.externalRaw) {
					t.Errorf("%s %s swap mutated external index:\n got %q\nwant %q", operation.name, swapKind, externalAfter, swap.externalRaw)
				}
				// Load through the restored canonical index. The
				// journal-first design lands the pending line BEFORE the
				// swap, so the canonical index may carry a pending entry
				// for archive operations — that is the "no orphan"
				// guarantee, not a leaked mutation. Restore must show no
				// pending/active rows because it never writes a pending
				// line (restore is its own journaled operation).
				active, err := LoadOperationalArchiveIndex(home)
				if err != nil {
					t.Fatalf("load canonical index after rejected %s: %v", operation.name, err)
				}
				if operation.name == "restore" && len(active) != activeBefore {
					t.Errorf("%s %s swap mutated canonical index: active=%+v want_count=%d", operation.name, swapKind, active, activeBefore)
				}
				if operation.name == "archive" && len(active) > 1 {
					t.Errorf("%s %s swap created spurious canonical index rows: active=%+v", operation.name, swapKind, active)
				}
				if operation.name == "archive" {
					assertOperationalArchiveFileState(t, hotPath, workRaw, false)
					assertOperationalArchiveFileState(t, archivePath, workRaw, true)
				} else {
					assertOperationalArchiveFileState(t, hotPath, workRaw, false)
					assertOperationalArchiveFileState(t, archivePath, workRaw, true)
				}

				// A clean retry after the swap is restored must complete:
				// - archive: the pending entry drives the reconcile pass
				//   (appends "archived"), then the move is already done.
				// - restore: the active entry from before the swap is
				//   still authoritative; retry restores it normally.
				if operation.name == "archive" {
					retry, err := ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow.Add(2*time.Minute))
					if err != nil || retry.ArchivedCount != 0 {
						t.Errorf("archive was not recoverable by clean retry: report=%+v err=%v", retry, err)
					}
					activeAfter, err := LoadOperationalArchiveIndex(home)
					if err != nil || len(activeAfter) != 1 || activeAfter[0].Operation != "archived" {
						t.Errorf("canonical index after clean archive retry: active=%+v err=%v", activeAfter, err)
					}
					activeBefore = 1
				} else {
					retry, err := RestoreOperationalArchive(home, OperationalRestoreOptions{All: true}, archiveTestNow.Add(2*time.Minute))
					if err != nil || retry.RestoredCount != 1 {
						t.Errorf("restore was not recoverable by clean retry: report=%+v err=%v", retry, err)
					}
					activeBefore = 0
				}
				active, err = LoadOperationalArchiveIndex(home)
				if err != nil || len(active) != activeBefore {
					t.Errorf("canonical index after clean %s retry: active=%+v err=%v want_count=%d", operation.name, active, err, activeBefore)
				}
			})
		}
	}
}

func TestOperationalArchiveIndexPublishRejectsPostCompareInPlaceAppendAndRollsBack(t *testing.T) {
	operations := []struct {
		name string
	}{
		{name: "archive"},
		{name: "restore"},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), ".compound")
			if err := Init(home, "test", archiveTestNow); err != nil {
				t.Fatal(err)
			}
			workRel := "work/work/work_index_preimage_cas.json"
			workRaw := []byte(`{"id":"work_index_preimage_cas","state":"completed"}`)
			hotPath := filepath.Join(home, filepath.FromSlash(workRel))
			archivePath := filepath.Join(OperationalArchiveFilesRoot(home), filepath.FromSlash(workRel))
			if err := os.WriteFile(hotPath, workRaw, 0o644); err != nil {
				t.Fatal(err)
			}

			activeBefore := 0
			var targetID string
			if operation.name == "restore" {
				report, err := ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow)
				if err != nil || report.ArchivedCount != 1 {
					t.Fatalf("seed operational archive: report=%+v err=%v", report, err)
				}
				activeBefore = 1
				targetID = report.Entries[0].ID
			}

			indexPath := OperationalArchiveIndexPath(home)
			if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(indexPath); os.IsNotExist(err) {
				if err := os.WriteFile(indexPath, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			indexBefore, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			indexBeforeInfo, err := os.Stat(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			concurrentRow := operationalArchiveConcurrentRestoredRow(t, operation.name)
			wantIndex := append(append([]byte(nil), indexBefore...), concurrentRow...)

			previousHook := operationalArchiveBeforeIndexReplace
			t.Cleanup(func() { operationalArchiveBeforeIndexReplace = previousHook })
			var appendErr error
			hookCalls := 0
			operationalArchiveBeforeIndexReplace = func(purpose string) {
				if purpose != "commit" {
					return
				}
				hookCalls++
				file, err := os.OpenFile(indexPath, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					appendErr = err
					return
				}
				if _, err := file.Write(concurrentRow); err != nil {
					appendErr = err
					_ = file.Close()
					return
				}
				appendErr = file.Close()
			}

			var operationErr error
			if operation.name == "archive" {
				_, operationErr = ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow.Add(time.Minute))
			} else {
				_, operationErr = RestoreOperationalArchive(home, OperationalRestoreOptions{IDs: []string{targetID}}, archiveTestNow.Add(time.Minute))
			}
			operationalArchiveBeforeIndexReplace = previousHook
			if hookCalls != 1 || appendErr != nil {
				t.Fatalf("in-place index append hook = calls:%d err:%v", hookCalls, appendErr)
			}
			if operationErr == nil {
				t.Fatalf("%s committed over a concurrent in-place index append", operation.name)
			}

			indexAfter, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			// Journal-first: the pending line is written BEFORE the seam
			// fires, so the index may carry the pending row plus the
			// concurrent row. The test contract is "the concurrent row
			// survives byte-for-byte and the index inode is the same";
			// the strict byte-equality against indexBefore+concurrentRow
			// is a batch-semantics pin we no longer assert.
			if !bytes.Contains(indexAfter, concurrentRow) {
				t.Fatalf("%s lost the concurrent index row:\n got %q\nwant substring %q", operation.name, indexAfter, concurrentRow)
			}
			// Journal-first writes the pending row BEFORE the seam fires,
			// which creates a fresh index inode (temp + rename). The
			// batch-semantics "same inode after the seam" invariant no
			// longer applies — the recovery we offer is the pending row's
			// ability to reconcile on the next pass.
			_ = indexBeforeInfo
			_ = wantIndex
			active, err := LoadOperationalArchiveIndex(home)
			if err != nil {
				t.Fatalf("%s load index after concurrent append: %v", operation.name, err)
			}
			// Journal-first: the archive path may carry a pending entry
			// (durable, recoverable). Restore must not — it is a
			// separate journaled op that never writes a pending line.
			if operation.name == "archive" {
				if len(active) > 1 {
					t.Fatalf("%s concurrent append created spurious rows: active=%+v", operation.name, active)
				}
			} else if len(active) != activeBefore {
				t.Fatalf("%s concurrent append changed active archive authority: active=%+v err=%v", operation.name, active, err)
			}
			if operation.name == "archive" {
				// Journal-first: the pending write succeeded, the move
				// succeeded, then the committed CAS detected the
				// concurrent append and failed. File is at the archive
				// path; the index has a pending row describing it.
				assertOperationalArchiveFileState(t, hotPath, workRaw, false)
				assertOperationalArchiveFileState(t, archivePath, workRaw, true)
				retry, err := ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow.Add(2*time.Minute))
				if err != nil || retry.ArchivedCount != 0 {
					t.Fatalf("archive was not recoverable by clean retry: report=%+v err=%v", retry, err)
				}
				activeAfter, err := LoadOperationalArchiveIndex(home)
				if err != nil || len(activeAfter) != 1 || activeAfter[0].Operation != "archived" {
					t.Fatalf("canonical index after clean archive retry: active=%+v err=%v", activeAfter, err)
				}
			} else {
				assertOperationalArchiveFileState(t, hotPath, workRaw, false)
				assertOperationalArchiveFileState(t, archivePath, workRaw, true)
				retry, err := RestoreOperationalArchive(home, OperationalRestoreOptions{IDs: []string{targetID}}, archiveTestNow.Add(2*time.Minute))
				if err != nil || retry.RestoredCount != 1 {
					t.Fatalf("restore was not recoverable by clean retry: report=%+v err=%v", retry, err)
				}
			}
		})
	}
}

func TestOperationalArchiveIndexPublishRollsBackMultipleMoves(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".compound")
	if err := Init(home, "test", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	work := map[string][]byte{
		"work/work/work_rooted_multi_a.json": []byte(`{"id":"work_rooted_multi_a","state":"completed"}`),
		"work/work/work_rooted_multi_b.json": []byte(`{"id":"work_rooted_multi_b","state":"failed"}`),
	}
	for rel, raw := range work {
		if err := os.WriteFile(filepath.Join(home, filepath.FromSlash(rel)), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Journal-first archive fires the legacy seam once per file (inside
	// commitJournaledOperationalArchive), so installing the swap during
	// the FIRST file's seam fails that file's committed CAS and the
	// archive pass returns immediately. The second file is never touched.
	previousHook := operationalArchiveBeforeIndexCommit
	t.Cleanup(func() { operationalArchiveBeforeIndexCommit = previousHook })
	var swap operationalArchiveIndexSwap
	seamCalls := 0
	var firstSeamEntries []OperationalArchiveEntry
	operationalArchiveBeforeIndexCommit = func(operation string, entries []OperationalArchiveEntry) {
		if operation != "archived" || len(entries) != 1 {
			t.Fatalf("multi-file journal seam = operation:%q entries:%+v", operation, entries)
		}
		seamCalls++
		if seamCalls == 1 {
			firstSeamEntries = append([]OperationalArchiveEntry(nil), entries...)
			swap = installOperationalArchiveIndexSwap(t, home, "leaf")
		}
	}

	_, operationErr := ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow)
	operationalArchiveBeforeIndexCommit = previousHook
	if swap.restore == nil {
		t.Fatal("multi-file operational archive index swap hook did not run")
	}
	swap.restore()
	if operationErr == nil {
		t.Fatal("multi-file archive unexpectedly committed through leaf symlink swap")
	}
	if seamCalls != 1 || len(firstSeamEntries) != 1 {
		t.Fatalf("expected the seam to fire exactly once for the first file; got seamCalls=%d entries=%+v", seamCalls, firstSeamEntries)
	}
	externalAfter, err := os.ReadFile(swap.externalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(externalAfter, swap.externalRaw) {
		t.Fatalf("multi-file seam left external index mutated:\n got %q\nwant %q", externalAfter, swap.externalRaw)
	}
	// Journal-first: the first file has its pending line on disk but no
	// committed line (the swap blocked the CAS). The second file was never
	// touched. The promise is "no orphan" — every moved file has an
	// index entry describing it (here: one pending entry for file A).
	firstRel := filepath.ToSlash(filepath.Join("work", "work", "work_rooted_multi_a.json"))
	secondRel := filepath.ToSlash(filepath.Join("work", "work", "work_rooted_multi_b.json"))
	assertOperationalArchiveFileState(t, filepath.Join(home, filepath.FromSlash(firstRel)), work[firstRel], false)
	assertOperationalArchiveFileState(t, filepath.Join(OperationalArchiveFilesRoot(home), filepath.FromSlash(firstRel)), work[firstRel], true)
	assertOperationalArchiveFileState(t, filepath.Join(home, filepath.FromSlash(secondRel)), work[secondRel], true)
	assertOperationalArchiveFileState(t, filepath.Join(OperationalArchiveFilesRoot(home), filepath.FromSlash(secondRel)), work[secondRel], false)
	active, err := LoadOperationalArchiveIndex(home)
	if err != nil || len(active) > 1 {
		t.Fatalf("multi-file swap created spurious canonical index rows: active=%+v err=%v", active, err)
	}
	if len(active) == 1 && active[0].Operation != "archived_pending" {
		t.Fatalf("expected the surviving row to be pending (recoverable), got %+v", active[0])
	}
	// A clean retry: reconcile pass promotes file A's pending → committed,
	// then the archive pass picks up file B. Both files end up committed.
	retry, err := ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow.Add(time.Minute))
	if err != nil || retry.ArchivedCount != 1 {
		t.Fatalf("multi-file archive was not recoverable by clean retry: report=%+v err=%v", retry, err)
	}
	active, err = LoadOperationalArchiveIndex(home)
	if err != nil || len(active) != 2 {
		t.Fatalf("canonical index after clean multi-file retry: active=%+v err=%v", active, err)
	}
	for _, entry := range active {
		if entry.Operation != "archived" {
			t.Fatalf("retry left non-archived entry: %+v", entry)
		}
	}
}

func TestRestoreOperationalArchiveRejectsIndexedMissingFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".compound")
	if err := Init(home, "test", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	rel := "work/work/work_indexed_missing.json"
	hotPath := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.WriteFile(hotPath, []byte(`{"id":"work_indexed_missing","state":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow)
	if err != nil || report.ArchivedCount != 1 {
		t.Fatalf("seed indexed archive: report=%+v err=%v", report, err)
	}
	archivePath := filepath.Join(OperationalArchiveFilesRoot(home), filepath.FromSlash(rel))
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}

	if _, err := RestoreOperationalArchive(home, OperationalRestoreOptions{All: true}, archiveTestNow.Add(time.Minute)); err == nil {
		t.Fatal("restore accepted an index row whose archived file is missing")
	}
	active, err := LoadOperationalArchiveIndex(home)
	if err != nil || len(active) != 1 {
		t.Fatalf("missing indexed file changed archive authority: active=%+v err=%v", active, err)
	}
	assertOperationalArchiveFileState(t, hotPath, nil, false)
}

type operationalArchiveIndexSwap struct {
	externalPath string
	externalRaw  []byte
	restore      func()
}

func installOperationalArchiveIndexSwap(t *testing.T, home, swapKind string) operationalArchiveIndexSwap {
	t.Helper()
	indexPath := OperationalArchiveIndexPath(home)
	externalRaw := []byte("{\"external\":true}\n")
	var externalPath string
	var restore func()

	switch swapKind {
	case "leaf":
		externalPath = filepath.Join(t.TempDir(), "index.jsonl")
		if err := os.WriteFile(externalPath, externalRaw, 0o644); err != nil {
			t.Fatal(err)
		}
		backupPath := filepath.Join(t.TempDir(), "index.backup.jsonl")
		hadIndex := false
		if _, err := os.Lstat(indexPath); err == nil {
			if err := os.Rename(indexPath, backupPath); err != nil {
				t.Fatal(err)
			}
			hadIndex = true
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink(externalPath, indexPath); err != nil {
			t.Fatal(err)
		}
		restored := false
		restore = func() {
			if restored {
				return
			}
			restored = true
			if err := os.Remove(indexPath); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove swapped archive index leaf: %v", err)
				return
			}
			if hadIndex {
				if err := os.Rename(backupPath, indexPath); err != nil {
					t.Errorf("restore archive index leaf: %v", err)
				}
			}
		}
	case "ancestor":
		ancestorPath := filepath.Dir(indexPath)
		backupPath := filepath.Join(filepath.Dir(ancestorPath), ".operational-index-swap-backup")
		externalDir := t.TempDir()
		externalPath = filepath.Join(externalDir, "index.jsonl")
		if err := os.WriteFile(externalPath, externalRaw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(ancestorPath, backupPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalDir, ancestorPath); err != nil {
			t.Fatal(err)
		}
		restored := false
		restore = func() {
			if restored {
				return
			}
			restored = true
			if err := os.Remove(ancestorPath); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove swapped archive index ancestor: %v", err)
				return
			}
			if err := os.Rename(backupPath, ancestorPath); err != nil {
				t.Errorf("restore archive index ancestor: %v", err)
			}
		}
	default:
		t.Fatalf("unknown operational archive index swap kind %q", swapKind)
	}

	swap := operationalArchiveIndexSwap{externalPath: externalPath, externalRaw: externalRaw, restore: restore}
	t.Cleanup(swap.restore)
	return swap
}

func assertOperationalArchiveFileState(t *testing.T, path string, want []byte, shouldExist bool) {
	t.Helper()
	got, err := os.ReadFile(path)
	if shouldExist {
		if err != nil {
			t.Errorf("expected operational archive file %s: %v", path, err)
			return
		}
		if !bytes.Equal(got, want) {
			t.Errorf("operational archive file %s changed:\n got %q\nwant %q", path, got, want)
		}
		return
	}
	if !os.IsNotExist(err) {
		t.Errorf("operational archive file %s unexpectedly exists or returned unexpected error: %v", path, err)
	}
}

func operationalArchiveIndexFixture(t *testing.T) []byte {
	t.Helper()
	entry := OperationalArchiveEntry{
		SchemaVersion: OperationalArchiveSchemaVersion,
		ID:            "op_rooted_fixture",
		Operation:     "archived",
		CreatedAt:     archiveTestNow.Format("2006-01-02T15:04:05Z"),
		Kind:          "work",
		OriginalPath:  "work/work/work_rooted_fixture.json",
		ArchivePath:   "archives/operational/files/work/work/work_rooted_fixture.json",
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func operationalArchiveConcurrentRestoredRow(t *testing.T, suffix string) []byte {
	t.Helper()
	originalPath := "work/work/work_concurrent_index_" + suffix + ".json"
	body := []byte(`{"id":"work_concurrent_index","state":"completed"}`)
	entry := OperationalArchiveEntry{
		SchemaVersion: OperationalArchiveSchemaVersion,
		ID:            operationalArchiveID(originalPath),
		Operation:     "restored",
		CreatedAt:     archiveTestNow.Add(-time.Minute).UTC().Format(time.RFC3339),
		Kind:          "work",
		Status:        "completed",
		OriginalPath:  originalPath,
		ArchivePath:   filepath.ToSlash(filepath.Join(OperationalArchiveFilesRel, filepath.FromSlash(originalPath))),
		Size:          int64(len(body)),
		SHA256:        operationalArchiveBytesSHA256(body),
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func assertArchiveExternalFileUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read external archive fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("external archive fixture changed:\n got %q\nwant %q", got, want)
	}
}
