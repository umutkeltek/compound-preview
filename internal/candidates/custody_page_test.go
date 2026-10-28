package candidates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"
	"unicode/utf8"
)

func TestReadPagePreservesOpaqueCorruptCursorsInDirectoryOrder(t *testing.T) {
	homePath := t.TempDir()
	directory := filepath.Join(homePath, InboxDir(Memory))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".json":          `{"id":""}`,
		"alpha.json":     `{"id":"alpha","marker":"valid"}`,
		"bad\\name.json": `{"id":"bad\\name"}`,
		"blank.json":     `{"id":""}`,
		"mismatch.json":  `{"id":"other"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(directory, "directory.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alpha.json", filepath.Join(directory, "symlink.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "residue"), []byte("not a candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wantOrder := []string{".json", "alpha.json", "bad\\name.json", "blank.json", "directory.json", "mismatch.json", "residue", "symlink.json"}
	sort.Strings(wantOrder)
	page, err := ReadPage(homePath, Memory, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Walked != len(wantOrder) || !page.AtEnd {
		t.Fatalf("page boundary = walked:%d at_end:%t, want walked:%d at_end:true", page.Walked, page.AtEnd, len(wantOrder))
	}
	gotOrder := make([]string, 0, len(page.Entries))
	for _, entry := range page.Entries {
		gotOrder = append(gotOrder, string(entry.EntryCursor))
		if (entry.Record == nil) == (entry.Corruption == nil) {
			t.Fatalf("entry %q must have exactly one outcome: %+v", entry.EntryCursor, entry)
		}
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("entry order = %q, want %q", gotOrder, wantOrder)
	}
	if string(page.LastCursor) != wantOrder[len(wantOrder)-1] {
		t.Fatalf("last cursor = %q, want %q", page.LastCursor, wantOrder[len(wantOrder)-1])
	}
	records := candidatePageRecords(page)
	if len(records) != 1 || records[0].ID != "alpha" {
		t.Fatalf("actionable records = %+v, want alpha only", records)
	}

	wantCodes := map[string]CorruptionCode{
		".json":          CorruptionInvalidFilename,
		"bad\\name.json": CorruptionInvalidFilename,
		"blank.json":     CorruptionBlankID,
		"directory.json": CorruptionNonRegular,
		"mismatch.json":  CorruptionIDMismatch,
		"residue":        CorruptionInvalidFilename,
		"symlink.json":   CorruptionSymlink,
	}
	for _, corruption := range candidatePageCorruptions(page) {
		if want := wantCodes[string(corruption.EntryCursor)]; corruption.Code != want {
			t.Fatalf("corruption %q code = %q, want %q", corruption.EntryCursor, corruption.Code, want)
		}
		delete(wantCodes, string(corruption.EntryCursor))
	}
	if len(wantCodes) != 0 {
		t.Fatalf("missing corruption outcomes: %v", wantCodes)
	}

	cursor := EntryCursor("")
	resumed := make([]string, 0, len(wantOrder))
	for steps := 0; steps <= len(wantOrder); steps++ {
		one, err := ReadPage(homePath, Memory, cursor, 1)
		if err != nil {
			t.Fatalf("resume after %q: %v", cursor, err)
		}
		if one.Walked == 0 {
			if !one.AtEnd {
				t.Fatalf("empty terminal page after %q did not report at_end", cursor)
			}
			break
		}
		if one.Walked != 1 || len(one.Entries) != 1 {
			t.Fatalf("single-entry page after %q = %+v", cursor, one)
		}
		if one.LastCursor == cursor {
			t.Fatalf("cursor did not advance from %q", cursor)
		}
		resumed = append(resumed, string(one.LastCursor))
		cursor = one.LastCursor
	}
	if !reflect.DeepEqual(resumed, wantOrder) {
		t.Fatalf("one-entry resume order = %q, want %q", resumed, wantOrder)
	}
}

func TestReadPageRejectsPathShapedCursorButAcceptsOpaqueCorruptFilename(t *testing.T) {
	homePath := t.TempDir()
	for _, cursor := range []string{"../escape.json", "/absolute.json", "bad\x00name.json"} {
		if _, err := ReadPage(homePath, Memory, EntryCursor(cursor), 1); err == nil {
			t.Fatalf("path-shaped cursor %q accepted", cursor)
		}
	}
	for _, cursor := range []string{".json", "bad\\name.json", "not-json"} {
		page, err := ReadPage(homePath, Memory, EntryCursor(cursor), 1)
		if err != nil {
			t.Fatalf("opaque corrupt cursor %q rejected: %v", cursor, err)
		}
		if !page.AtEnd {
			t.Fatalf("empty page after %q did not report at_end", cursor)
		}
	}
}

func TestReadPageCursorJSONRoundTripPreservesInvalidUTF8FilenameBytes(t *testing.T) {
	name := string([]byte{'b', 'a', 'd', '-', 0xff, '.', 'j', 's', 'o', 'n'})
	if utf8.ValidString(name) {
		t.Fatal("fixture filename unexpectedly valid UTF-8")
	}
	wrapper := struct {
		Cursor EntryCursor `json:"cursor"`
	}{Cursor: EntryCursor(name)}
	body, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	var restoredWrapper struct {
		Cursor EntryCursor `json:"cursor"`
	}
	if err := json.Unmarshal(body, &restoredWrapper); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(restoredWrapper.Cursor), []byte(name)) {
		t.Fatalf("round-tripped cursor bytes = %x, want %x; json=%s", []byte(restoredWrapper.Cursor), []byte(name), body)
	}
	if runtime.GOOS != "linux" {
		return
	}

	homePath := t.TempDir()
	directory := filepath.Join(homePath, InboxDir(Memory))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(`{"id":"unusable"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "later.json"), []byte(`{"id":"later"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	page, err := ReadPage(homePath, Memory, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(page.LastCursor) != name {
		t.Fatalf("first cursor bytes = %x, want %x", []byte(page.LastCursor), []byte(name))
	}
	body, err = json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var restored Page
	if err := json.Unmarshal(body, &restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(restored.LastCursor), []byte(name)) {
		t.Fatalf("round-tripped cursor bytes = %x, want %x; json=%s", []byte(restored.LastCursor), []byte(name), body)
	}
	resumed, err := ReadPage(homePath, Memory, restored.LastCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	records := candidatePageRecords(resumed)
	if len(records) != 1 || records[0].ID != "later" {
		t.Fatalf("resume after invalid UTF-8 cursor = %+v", resumed)
	}
}

func TestEntryCursorDurableJSONRejectsAmbiguousRawFilename(t *testing.T) {
	var wrapper struct {
		Cursor EntryCursor `json:"cursor"`
	}
	if err := json.Unmarshal([]byte(`{"cursor":"cand_01.json"}`), &wrapper); err == nil {
		t.Fatalf("durable JSON accepted raw cursor %q; versioned token required", wrapper.Cursor)
	}
	parsed, err := ParseEntryCursor("cand_01.json")
	if err != nil {
		t.Fatalf("interactive raw filename cursor rejected: %v", err)
	}
	if parsed != "cand_01.json" {
		t.Fatalf("interactive cursor = %q, want cand_01.json", parsed)
	}
}

func TestCandidateIdentityRequiresUniqueExactJSONIDKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "case alias", body: `{"ID":"canonical"}`},
		{name: "duplicate", body: `{"id":"wrong","id":"canonical"}`},
		{name: "alias plus exact", body: `{"ID":"canonical","id":"canonical"}`},
	} {
		t.Run(tc.name+"/read", func(t *testing.T) {
			homePath := t.TempDir()
			writeCustodyContractFile(t, homePath, "canonical.json", tc.body)
			if _, err := ReadRecord(homePath, Memory, "canonical"); err == nil {
				t.Fatalf("noncanonical identity was actionable: %s", tc.body)
			}
		})
		t.Run(tc.name+"/write", func(t *testing.T) {
			homePath := t.TempDir()
			if err := WriteCandidate(homePath, Memory, "canonical", json.RawMessage(tc.body)); err == nil {
				t.Fatalf("noncanonical payload identity was writable: %s", tc.body)
			}
		})
	}
}

func TestCandidateIDRejectsInvalidUTF8AndControlsBeforeCustodyMutation(t *testing.T) {
	ids := []string{"line\nbreak", "nul\x00byte", string([]byte{'b', 'a', 'd', 0xff})}
	for _, id := range ids {
		t.Run(fmt.Sprintf("%x", []byte(id)), func(t *testing.T) {
			homePath := t.TempDir()
			payload := custodyContractRecord{ID: id, Marker: "body"}
			if err := WriteCandidate(homePath, Memory, id, payload); err == nil {
				t.Fatalf("candidate id bytes %x accepted", []byte(id))
			}
			entries, err := os.ReadDir(homePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("invalid id mutated custody before rejection: %v", entries)
			}
		})
	}
}

func TestReadPageUnreadableEntryHasStableNamedOutcome(t *testing.T) {
	homePath := t.TempDir()
	path := writeCustodyContractFile(t, homePath, "unreadable.json", `{"id":"unreadable"}`)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	var first Corruption
	for attempt := 0; attempt < 2; attempt++ {
		page, err := ReadPage(homePath, Memory, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		corruptions := candidatePageCorruptions(page)
		if len(corruptions) != 1 {
			t.Fatalf("attempt %d corruptions = %+v", attempt, corruptions)
		}
		got := corruptions[0]
		if got.EntryCursor != "unreadable.json" || got.Code != CorruptionUnreadable {
			t.Fatalf("attempt %d outcome = %+v", attempt, got)
		}
		if attempt == 0 {
			first = got
		} else if got.EntryCursor != first.EntryCursor || got.Code != first.Code {
			t.Fatalf("unreadable outcome changed: first=%+v second=%+v", first, got)
		}
	}
}

func TestCandidateRecordValidateDetectsExportedFieldTampering(t *testing.T) {
	homePath := t.TempDir()
	payload := custodyContractRecord{ID: "sealed", Marker: "body"}
	if err := WriteCandidate(homePath, Memory, payload.ID, payload); err != nil {
		t.Fatal(err)
	}
	record, err := ReadRecord(homePath, Memory, payload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("fresh record invalid: %v", err)
	}
	mutations := map[string]func(*Record){
		"body":          func(candidate *Record) { candidate.Body = append(candidate.Body, ' ') },
		"digest":        func(candidate *Record) { candidate.Digest = "sha256:tampered" },
		"entry cursor":  func(candidate *Record) { candidate.EntryCursor = "other.json" },
		"id":            func(candidate *Record) { candidate.ID = "other" },
		"kind":          func(candidate *Record) { candidate.Kind = Decision },
		"custody token": func(candidate *Record) { candidate.CustodyToken = "sha256:tampered" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := record
			changed.Body = append([]byte(nil), record.Body...)
			mutate(&changed)
			if err := changed.Validate(); err == nil {
				t.Fatalf("tampered record accepted: %+v", changed)
			}
		})
	}
}

func TestReadRecordRejectsSameInodeRewriteDuringRead(t *testing.T) {
	homePath := t.TempDir()
	path := writeCustodyContractFile(t, homePath, "rewrite.json", `{"id":"rewrite","marker":"before"}`)
	originalHook := candidateAfterOpenHook
	t.Cleanup(func() { candidateAfterOpenHook = originalHook })
	candidateAfterOpenHook = func(entryCursor string) {
		if entryCursor != "rewrite.json" {
			t.Fatalf("hook entry = %q", entryCursor)
		}
		if err := os.WriteFile(path, []byte(`{"id":"rewrite","marker":"after!"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := ReadRecord(homePath, Memory, "rewrite")
	var corruption *Corruption
	if !errors.As(err, &corruption) || corruption.Code != CorruptionRaced {
		t.Fatalf("same-inode rewrite error = %v, want %s corruption", err, CorruptionRaced)
	}
}

func TestCandidateWritersPreserveCorruptExistingTarget(t *testing.T) {
	type operation struct {
		name string
		run  func(string, custodyContractRecord) error
	}
	operations := []operation{
		{name: "WriteCandidate", run: func(homePath string, payload custodyContractRecord) error {
			return WriteCandidate(homePath, Memory, payload.ID, payload)
		}},
		{name: "CreateCandidateIfAbsent", run: func(homePath string, payload custodyContractRecord) error {
			_, err := CreateCandidateIfAbsent(homePath, Memory, payload.ID, payload)
			return err
		}},
	}
	for _, existing := range []struct {
		name  string
		build func(*testing.T, string, string)
	}{
		{name: "blank id", build: func(t *testing.T, target, _ string) {
			if err := os.WriteFile(target, []byte(`{"id":"","marker":"original"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mismatched id", build: func(t *testing.T, target, _ string) {
			if err := os.WriteFile(target, []byte(`{"id":"other","marker":"original"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", build: func(t *testing.T, target, sentinel string) {
			if err := os.Symlink(sentinel, target); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "nonregular", build: func(t *testing.T, target, _ string) {
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, operation := range operations {
			t.Run(existing.name+"/"+operation.name, func(t *testing.T) {
				homePath := t.TempDir()
				directory := filepath.Join(homePath, InboxDir(Memory))
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				payload := custodyContractRecord{ID: "preserved", Marker: "replacement"}
				target := filepath.Join(directory, payload.ID+".json")
				sentinel := filepath.Join(homePath, "sentinel.json")
				wantSentinel := []byte("sentinel\n")
				if err := os.WriteFile(sentinel, wantSentinel, 0o644); err != nil {
					t.Fatal(err)
				}
				existing.build(t, target, sentinel)
				before, err := snapshotCandidateEntry(target)
				if err != nil {
					t.Fatal(err)
				}

				if err := operation.run(homePath, payload); err == nil {
					t.Fatalf("%s accepted corrupt existing target", operation.name)
				}
				after, err := snapshotCandidateEntry(target)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(after, before) {
					t.Fatalf("target changed: got %q want %q", after, before)
				}
				gotSentinel, err := os.ReadFile(sentinel)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotSentinel, wantSentinel) {
					t.Fatalf("symlink referent changed: got %q want %q", gotSentinel, wantSentinel)
				}
			})
		}
	}
}

func TestCandidateWritersRejectSymlinkedLockDirectory(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(string, custodyContractRecord) error
	}{
		{name: "WriteCandidate", run: func(homePath string, payload custodyContractRecord) error {
			return WriteCandidate(homePath, Memory, payload.ID, payload)
		}},
		{name: "CreateCandidateIfAbsent", run: func(homePath string, payload custodyContractRecord) error {
			_, err := CreateCandidateIfAbsent(homePath, Memory, payload.ID, payload)
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			homePath := t.TempDir()
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(homePath, "lock")); err != nil {
				t.Fatal(err)
			}
			payload := custodyContractRecord{ID: "lock-probe", Marker: "body"}
			if err := operation.run(homePath, payload); err == nil {
				t.Fatalf("%s accepted symlinked lock directory", operation.name)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("aliased lock target mutated: %v", entries)
			}
		})
	}
}

func snapshotCandidateEntry(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		return []byte("symlink:" + target), err
	case info.IsDir():
		return []byte("directory:" + info.Mode().String()), nil
	case info.Mode().IsRegular():
		return os.ReadFile(path)
	default:
		return nil, fmt.Errorf("unsupported test entry mode %s", info.Mode())
	}
}

func candidatePageRecords(page Page) []Record {
	records := make([]Record, 0, len(page.Entries))
	for _, entry := range page.Entries {
		if entry.Record != nil {
			records = append(records, *entry.Record)
		}
	}
	return records
}

func candidatePageCorruptions(page Page) []Corruption {
	corruptions := make([]Corruption, 0, len(page.Entries))
	for _, entry := range page.Entries {
		if entry.Corruption != nil {
			corruptions = append(corruptions, *entry.Corruption)
		}
	}
	return corruptions
}
