package activity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeSpanFixture(t *testing.T, home, day string, sf SpanFile) {
	t.Helper()
	if err := os.MkdirAll(SpansRoot(home), 0o700); err != nil {
		t.Fatalf("mkdir spans root: %v", err)
	}
	body, err := json.Marshal(sf)
	if err != nil {
		t.Fatalf("marshal span fixture: %v", err)
	}
	if err := os.WriteFile(SpanPath(home, day), body, 0o600); err != nil {
		t.Fatalf("write span fixture: %v", err)
	}
}

func TestReadSpanFileAbsentIsQuietNotError(t *testing.T) {
	home := t.TempDir()
	_, ok, err := ReadSpanFile(home, "2026-07-15")
	if err != nil {
		t.Fatalf("absent span file must not error, got %v", err)
	}
	if ok {
		t.Fatal("absent span file must report ok=false")
	}
}

func TestReadSpanFileRoundTrip(t *testing.T) {
	home := t.TempDir()
	want := SpanFile{
		SchemaVersion: SpansSchemaVersion,
		Date:          "2026-07-15",
		GeneratedAt:   "2026-07-15T18:00:00Z",
		Spans: []ActivitySpan{{
			Start: "2026-07-15T09:00:00Z", End: "2026-07-15T09:47:00Z", ActiveSec: 2700,
			BundleID: "com.example.browser", AppName: "Browser",
			Category: CategoryBrowsing, Coverage: CoverageComplete,
		}},
	}
	writeSpanFixture(t, home, want.Date, want)
	got, ok, err := ReadSpanFile(home, want.Date)
	if err != nil || !ok {
		t.Fatalf("read span file: ok=%v err=%v", ok, err)
	}
	if got.Date != want.Date || len(got.Spans) != 1 || got.Spans[0].BundleID != want.Spans[0].BundleID {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

// A corrupt span file must surface as an error — never silently read as an empty day.
func TestReadSpanFileCorruptIsError(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(SpansRoot(home), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(SpanPath(home, "2026-07-15"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if _, _, err := ReadSpanFile(home, "2026-07-15"); err == nil {
		t.Fatal("corrupt span file must error, got nil")
	}
}

func TestLatestSpanDayPicksMaxAndIgnoresStrays(t *testing.T) {
	home := t.TempDir()
	if _, ok, err := LatestSpanDay(home); err != nil || ok {
		t.Fatalf("absent spans dir must be quiet: ok=%v err=%v", ok, err)
	}
	for _, day := range []string{"2026-07-13", "2026-07-15", "2026-07-14"} {
		writeSpanFixture(t, home, day, SpanFile{SchemaVersion: SpansSchemaVersion, Date: day})
	}
	// Stray files must not be mistaken for day files.
	if err := os.WriteFile(filepath.Join(SpansRoot(home), "cursor.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	if err := os.WriteFile(filepath.Join(SpansRoot(home), "9999-99.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write malformed-name stray: %v", err)
	}
	day, ok, err := LatestSpanDay(home)
	if err != nil || !ok {
		t.Fatalf("latest span day: ok=%v err=%v", ok, err)
	}
	if day != "2026-07-15" {
		t.Fatalf("latest span day = %q, want 2026-07-15", day)
	}
}
