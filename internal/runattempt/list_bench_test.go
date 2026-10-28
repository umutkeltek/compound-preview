package runattempt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkListLargeHome measures the complete fresh census, including decoding
// and validation, against a home with the attempt count seen in busy runtimes.
// Run with -benchtime=1x so fixture creation is not repeated by calibration.
func BenchmarkListLargeHome(b *testing.B) {
	const records = 24000
	root := b.TempDir()
	writer := Open(root)
	seed, err := writer.Mint("work_benchmark", "benchmark", "os-harness-entrypoints", time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		b.Fatal(err)
	}
	dir := filepath.Join(root, "run_attempts")
	for i := 0; i < records; i++ {
		ra := seed
		ra.ID = fmt.Sprintf("ratt_20261014T000000Z_%08x", i)
		body, err := json.Marshal(ra)
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ra.ID+".json"), body, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(dir, seed.ID+".json")); err != nil {
		b.Fatal(err)
	}
	reader := Open(root)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := reader.List()
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != records {
			b.Fatalf("List returned %d attempts; want %d", len(got), records)
		}
	}
}
