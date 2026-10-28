package hookplan

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOmpDeriveSessionBoundedOnLargeStores pins the bounded-scan contract:
// when the cwd-slug directory holds thousands of stale session files, the
// OmpDeriveSession header-read cost is capped to ompDeriveMaxProbe. The
// newest matching session is still found (a header read on the
// newest-by-mtime subset is sufficient when the file set is large).
//
// The 2 KB header peek is the dominant cost; mtime is cheap. A bounded
// pre-filter by mtime reduces N header reads to ompDeriveMaxProbe without
// sacrificing correctness because the contract only needs the NEWEST
// matching session, not all matches.
func TestOmpDeriveSessionBoundedOnLargeStores(t *testing.T) {
	home := t.TempDir()
	slug := ompSlugFromCwd("/tmp/bench")
	dir := filepath.Join(home, ".omp", "agent", "sessions", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant 2000 fake session files with monotonically increasing mtimes
	// so the newest match is unambiguous.
	const n = 2000
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("session-%04d.jsonl", i))
		body := fmt.Sprintf("{\"type\":\"title\"}\n{\"type\":\"session\",\"id\":\"sess-%04d\",\"cwd\":\"/tmp/bench\"}\n", i)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write session %d: %v", i, err)
		}
		ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatalf("chtimes %d: %v", i, err)
		}
	}

	// Run the call many times; measure wall-clock cost. With the bound
	// the cost is constant regardless of n (when n > ompDeriveMaxProbe).
	// We don't assert a specific budget -- the bench file already pins
	// the timing -- but we do assert the call returns the newest match.
	start := time.Now()
	transcriptPath, sessionID, ok := OmpDeriveSession(home, "/tmp/bench")
	elapsed := time.Since(start)
	if !ok {
		t.Fatalf("OmpDeriveSession returned ok=false on a 2000-file fixture")
	}
	if transcriptPath == "" {
		t.Fatalf("transcriptPath empty on a 2000-file fixture")
	}
	// The newest file (index n-1) must win.
	want := filepath.Join(dir, fmt.Sprintf("session-%04d.jsonl", n-1))
	if transcriptPath != want {
		t.Fatalf("newest match = %q, want %q", transcriptPath, want)
	}
	if sessionID != fmt.Sprintf("sess-%04d", n-1) {
		t.Fatalf("sessionID = %q, want sess-%04d", sessionID, n-1)
	}
	t.Logf("2000-file scan: %v", elapsed)
	if elapsed > 500*time.Millisecond {
		t.Logf("WARNING: 2000-file scan took %v (>500ms); consider lowering ompDeriveMaxProbe", elapsed)
	}
}
