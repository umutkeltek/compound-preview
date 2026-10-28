package home

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWriteFileAtomicNeverExposesAPartialBody is the falsifier for the whole reason
// this helper exists.
//
// A reader hammering the path while a writer replaces it must ALWAYS see a complete,
// parseable body — never a truncated one. With os.WriteFile (truncate in place) a
// reader arriving mid-write sees the partial file, and the projections this guards are
// read on the live orient path by a consumer that fail-opens on a parse error: a torn
// read does not surface as an error, it silently returns "no signal". So the test
// asserts parseability, not just absence of error.
func TestWriteFileAtomicNeverExposesAPartialBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projections", "body.json")

	// Two bodies of very different size, so a truncated write of the large one would
	// be trivially detectable as invalid JSON.
	small := mustJSON(t, map[string]any{"schema_version": "v1", "entries": []int{1}})
	large := mustJSON(t, map[string]any{"schema_version": "v1", "entries": makeRange(4000)})

	if err := WriteFileAtomic(path, small, 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var readErr error
	var mu sync.Mutex
	reads := 0

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			body, err := os.ReadFile(path)
			if err != nil {
				// A missing file would mean the rename left a gap; that is also a defect.
				mu.Lock()
				readErr = err
				mu.Unlock()
				return
			}
			var doc map[string]any
			if err := json.Unmarshal(body, &doc); err != nil {
				mu.Lock()
				readErr = err
				mu.Unlock()
				return
			}
			mu.Lock()
			reads++
			mu.Unlock()
		}
	}()

	for i := 0; i < 200; i++ {
		body := large
		if i%2 == 0 {
			body = small
		}
		if err := WriteFileAtomic(path, body, 0o644); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("write %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if readErr != nil {
		t.Fatalf("concurrent reader observed an incomplete body after %d good reads: %v", reads, readErr)
	}
	if reads == 0 {
		t.Fatal("reader never observed the file, so this proves nothing")
	}
	t.Logf("%d concurrent reads, all complete and parseable", reads)
}

// TestWriteFileAtomicLeavesNoTempFiles pins cleanup. A leftover dotfile under
// projections/ becomes an unknown entry in the habitat inventory, so a helper that
// litters would turn every write into a health-report finding.
func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projections")
	path := filepath.Join(dir, "body.json")
	for i := 0; i < 5; i++ {
		if err := WriteFileAtomic(path, []byte(`{"a":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly the target file, got %d entries", len(entries))
	}
}

// TestWriteFileAtomicAppliesTheRequestedMode pins that the target does not inherit
// CreateTemp's 0600. A projection readable only by its writer would fail-open to "no
// signal" for every other reader, which is the silent degradation this file is about.
func TestWriteFileAtomicAppliesTheRequestedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projections", "body.json")
	if err := WriteFileAtomic(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644 (CreateTemp's 0600 must not survive the rename)", got)
	}
}

// TestWriteFileAtomicSyncFailurePreventsPublication is the falsifier for the durability
// half of the contract: atomicity alone does not survive a crash. A failed temp-file
// sync must abort the write BEFORE the rename — the target keeps its previous body —
// and a failed directory sync must surface as an error instead of a volatile-only
// success. Without the syncs the write would report success while a crash could still
// resurrect the previous (or an empty) body, and this test would have nothing to fail.
func TestWriteFileAtomicSyncFailurePreventsPublication(t *testing.T) {
	fault := errors.New("injected sync failure")

	t.Run("temp file sync", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "projections", "body.json")
		if err := WriteFileAtomic(path, []byte(`{"v":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
		previous := atomicWriteTempFileSync
		defer func() { atomicWriteTempFileSync = previous }()
		atomicWriteTempFileSync = func(*os.File) error { return fault }

		err := WriteFileAtomic(path, []byte(`{"v":2}`), 0o644)
		if !errors.Is(err, fault) {
			t.Fatalf("temp file sync error = %v, want %v", err, fault)
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(body) != `{"v":1}` {
			t.Errorf("target body changed after a failed temp-file sync: %q", body)
		}
	})

	t.Run("directory sync", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "projections", "body.json")
		previous := atomicWriteDirSync
		defer func() { atomicWriteDirSync = previous }()
		atomicWriteDirSync = func(string) error { return fault }

		err := WriteFileAtomic(path, []byte(`{"v":1}`), 0o644)
		if !errors.Is(err, fault) {
			t.Fatalf("directory sync error = %v, want %v", err, fault)
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func makeRange(n int) []int {
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, i)
	}
	return out
}
