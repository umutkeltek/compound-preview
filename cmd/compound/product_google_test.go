package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/product"
)

func TestProductSignalImportsGoogleReportSourceInPlace(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{
			"signal",
			"--gsc-report", filepath.Join("..", "..", "fixtures", "product", "gsc-report.json"),
			"--product", "product:google-demo",
			"--site", "example.com",
			"--metric", "clicks",
		})
	})
	if !strings.Contains(out, "# product signals google_search_console") ||
		!strings.Contains(out, "- signals: 1") ||
		!strings.Contains(out, "gsc_clicks") {
		t.Fatalf("product signal output missing Google import proof:\n%s", out)
	}
	tl, err := product.LoadTimeline(homePath, "product:google-demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Signals) != 1 || tl.Signals[0].MetricName != "gsc_clicks" || tl.Signals[0].Before != 42 || tl.Signals[0].Before != tl.Signals[0].After {
		t.Fatalf("Google signal timeline = %+v", tl.Signals)
	}
	if len(tl.Transitions) != 1 {
		t.Fatalf("Google signal transitions = %+v", tl.Transitions)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("Google signal import should not promote claims ledger: %v", err)
	}
}
