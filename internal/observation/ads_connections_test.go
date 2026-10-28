package observation

import (
	"path/filepath"
	"testing"
	"time"

	"compound/internal/product"
)

// TestSourceConnectionsFoldAdsLedgerAndBridgedSignals would FAIL if an ad
// platform stopped appearing in the registry (so it could never read as
// not_connected), if imported ad rows did not lift it to observed, or if a
// bridged ads signal's evidence refs no longer resolved to the platform.
func TestSourceConnectionsFoldAdsLedgerAndBridgedSignals(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	before, err := LoadSourceConnectionsProjection(home, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"apple_search_ads", "google_ads", "meta_ads", "tiktok_ads", "x_ads", "reddit_ads", "linkedin_ads"} {
		conn := findSourceConnection(before.Connections, id)
		if conn == nil {
			t.Fatalf("registry must list %s even with no rows", id)
		}
		if conn.RowCount != 0 || conn.EvidenceState != "never_seen" {
			t.Fatalf("%s before import = %+v", id, conn)
		}
	}

	if _, err := product.ImportAdsReport(home, filepath.Join("..", "..", "fixtures", "product", "ads", "apple-search-ads.csv"), product.AdsImportOptions{
		Platform: "apple_search_ads", ProductID: "product:demo-ios", Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := LoadSourceConnectionsProjection(home, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	conn := findSourceConnection(after.Connections, "apple_search_ads")
	if conn == nil || conn.RowCount != 6 || conn.EvidenceState == "never_seen" || conn.LastSeen != now.Format(time.RFC3339) {
		t.Fatalf("apple after import = %+v", conn)
	}
	if untouched := findSourceConnection(after.Connections, "google_ads"); untouched == nil || untouched.RowCount != 0 {
		t.Fatalf("google must stay untouched: %+v", untouched)
	}

	docs, err := product.BuildAdsSignals(home, product.AdsSignalOptions{
		Platform: "apple_search_ads", ProductID: "product:demo-ios",
		Before: "2026-09-01/2026-09-01", After: "2026-09-02/2026-09-03", Metric: "spend",
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind := productSignalSourceKind(docs[0].Signal); kind != "apple_search_ads" {
		t.Fatalf("bridged signal source kind = %q, want apple_search_ads", kind)
	}
}
