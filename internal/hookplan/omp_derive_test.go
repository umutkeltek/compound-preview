package hookplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F6 has two halves: a substring check on the rendered TS (matches the
// existing omp_shim_test.go style and pins the contract the shim ships),
// and an end-to-end run of the Go-side mirror against a real temp omp session
// directory with two files (one stale cwd mismatch, one matching). On base
// the shim had no transcript_path at all and no discovery code anywhere;
// F6 turns both into red.

// TestOmpShimSourceCarriesTranscriptPathContract is the substring half.
// On base, the marker pinned only the @1 shim, which contained no
// `transcript_path` key and no slug derivation -- the substring fail is
// the red signal.
func TestOmpShimSourceCarriesTranscriptPathContract(t *testing.T) {
	if !strings.HasPrefix(ompHookShimSource, OmpShimMarker) {
		t.Fatalf("embedded shim lost ownership marker %q", OmpShimMarker)
	}
	required := []string{
		`pi.on("session_start"`,
		`pi.on("before_agent_start"`,
		`pi.on("tool_call"`,
		`pi.on("tool_result"`,
		`pi.on("session_shutdown"`,
		`export default function compoundHook`,
		// Slice-3 additions: the shim MUST discover the omp session file and
		// forward `transcript_path`. Both substrings below were absent on
		// base.
		`deriveOmpSession`,
		`slugFromCwd`,
		`transcript_path`,
		`.omp/agent/sessions`,
		// Native context takes priority over compatibility disk discovery.
		`getSessionId`,
		`getSessionFile`,
		`sessionFixture(context)`,
	}
	for _, needle := range required {
		if !strings.Contains(ompHookShimSource, needle) {
			t.Errorf("embedded shim lost %q — shim surface would silently shrink", needle)
		}
	}
}

// TestOmpDeriveSessionPicksMatchingCwd is the runtime half: it builds a
// synthetic homedir with two candidates under the expected slug dir, one
// whose line-2 cwd matches the test cwd and one that does not, plus a stale
// older match. The Go mirror must return the matching one.
func TestOmpDeriveSessionPicksMatchingCwd(t *testing.T) {
	cwd := "/tmp/x/sample"
	home := t.TempDir()
	slug := ompSlugFromCwd(cwd)
	dir := filepath.Join(home, ".omp", "agent", "sessions", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Stale match (oldest mtime) — wrong cwd; must NOT be returned.
	writeSessionFile(t, dir, "2026-08-22T10-00-00-000Z_uuid-old.jsonl", "/tmp/x/other", "old-uuid", false)
	// Right match (middle mtime).
	writeSessionFile(t, dir, "2026-08-25T12-00-00-000Z_uuid-mid.jsonl", cwd, "mid-uuid", false)
	// Right match (newest mtime) — must be returned.
	writeSessionFile(t, dir, "2026-08-25T18-00-00-000Z_uuid-newest.jsonl", cwd, "newest-uuid", true)

	gotPath, gotID, ok := OmpDeriveSession(home, cwd)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.HasSuffix(gotPath, "uuid-newest.jsonl") {
		t.Fatalf("got %q, want newest match by mtime", gotPath)
	}
	if gotID != "newest-uuid" {
		t.Fatalf("got sessionID %q, want newest-uuid", gotID)
	}
}

// TestOmpDeriveSessionReturnsFalseOnMismatch proves the safety property
// pin the packet calls out: when no match exists, transcriptPath is "" --
// the parser MUST treat "" as "no transcript", never as a probing fallback.
func TestOmpDeriveSessionReturnsFalseOnMismatch(t *testing.T) {
	home := t.TempDir()
	cwd := "/tmp/x/sample"
	slug := ompSlugFromCwd(cwd)
	dir := filepath.Join(home, ".omp", "agent", "sessions", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionFile(t, dir, "2026-08-25T10-00-00-000Z_uuid-other.jsonl", "/tmp/x/other", "wrong", false)
	gotPath, _, ok := OmpDeriveSession(home, cwd)
	if ok {
		t.Fatalf("expected ok=false on cwd mismatch, got path=%q", gotPath)
	}
	if gotPath != "" {
		t.Fatalf("expected empty transcript_path on miss, got %q", gotPath)
	}
}

func writeSessionFile(t *testing.T, dir, name, cwdVal, id string, newest bool) {
	t.Helper()
	full := filepath.Join(dir, name)
	f, err := os.Create(full)
	if err != nil {
		t.Fatal(err)
	}
	// Two-line header: title + session. omp's on-disk shape -- the test
	// parser only consults the JSON line for `type:"session"`.
	titleLine := `{"type":"title","v":1,"title":"","updatedAt":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	sessionLine, err := json.Marshal(map[string]any{
		"type":      "session",
		"version":   3,
		"id":        id,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"cwd":       cwdVal,
	})
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteString(titleLine + "\n" + string(sessionLine) + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Force a deterministic mtime ordering so the "newest" assertion is
	// independent of `time.Now()` resolution.
	mtime := time.Now().Add(-1 * time.Hour)
	if newest {
		mtime = time.Now().Add(1 * time.Hour)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
