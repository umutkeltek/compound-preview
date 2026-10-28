package hookplan

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkOmpDeriveSessionLargeStore measures the worst-case cost of
// OmpDeriveSession against a fixture with N .jsonl files under the cwd slug.
// The repro packet reports a 2.0KB header read per file plus open/close; we
// pin both ends so a future bound stays honest.
func BenchmarkOmpDeriveSessionLargeStore(b *testing.B) {
	home := b.TempDir()
	slug := "--tmp-bench--"
	dir := filepath.Join(home, ".omp", "agent", "sessions", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		path := filepath.Join(dir, fmt.Sprintf("omp-%d.jsonl", i))
		body := fmt.Sprintf("{\"type\":\"title\",\"id\":\"t-%d\"}\n{\"type\":\"session\",\"id\":\"sess-%d\",\"cwd\":\"/tmp/bench\"}\n", i, i)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
		ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
		_ = os.Chtimes(path, ts, ts)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = OmpDeriveSession(home, "/tmp/bench")
	}
}
