package product

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProductSignalsFiltersByProduct(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"sig_a","product_id":"product:one","metric_name":"m1","evidence_refs":["e1"],"confidence":0.5}
{"id":"sig_b","product_id":"product:two","metric_name":"m2","evidence_refs":["e2"],"confidence":0.5}
{"id":"sig_c","product_id":"product:one","metric_name":"m3","evidence_refs":["e3"],"confidence":0.5}
not-valid-json
`
	if err := os.WriteFile(filepath.Join(dir, "product-signals.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	one, err := LoadProductSignals(home, "product:one")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 2 {
		t.Fatalf("expected 2 signals for product:one (invalid line skipped), got %d", len(one))
	}
	for _, s := range one {
		if s.ProductID != "product:one" {
			t.Fatalf("filter leaked signal from %s", s.ProductID)
		}
	}

	all, err := LoadProductSignals(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 signals with empty filter, got %d", len(all))
	}
}

func TestLoadProductSignalsEmptyHome(t *testing.T) {
	got, err := LoadProductSignals(t.TempDir(), "product:one")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no signals on empty home, got %d", len(got))
	}
}
