package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Audit 2026-09-16, finding 3 (ads panel): currency disagreement was collapsed
// into one row. The auditor's reading of the committed source was that
// ImportAdsReport groups by (platform, product, account, campaign, date)
// and sums later rows without checking their currency. The audit asks for
// a regression that fails when two distinct currencies contribute to the
// same identity, before the fix is applied.
//
// TestAdsImportRefusesCurrencyCollapseInsideOneRow is the red falsifier: a
// campaign on the same day reported in two currencies MUST land as two
// distinct rows. A single combined row labelled with the first-seen
// currency and a numerically-summed Spend is the failure mode the audit
// named.
func TestAdsImportRefusesCurrencyCollapseInsideOneRow(t *testing.T) {
	home := t.TempDir()
	tmp := t.TempDir()
	mixed := filepath.Join(tmp, "mixed-currency.csv")
	if err := os.WriteFile(mixed, []byte("date,campaign_id,campaign_name,spend,impressions,clicks,conversions,currency\n2026-09-01,c1,Mix,100,1000,50,2,USD\n2026-09-01,c1,Mix,100,1000,50,2,EUR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAdsReport(home, mixed, AdsImportOptions{
		Platform: "other", ProductID: "product:demo",
		Now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("a same-day mixed-currency import must not error: %v", err)
	}
	rows, err := LoadAdRows(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 1 {
		t.Fatalf("two distinct currencies for the same (platform,product,account,campaign,date) collapsed into one row: %+v — the audit's exact failure mode", rows)
	}
	byCurrency := map[string]int{}
	for _, r := range rows {
		if r.Currency == "" {
			t.Fatalf("row has empty currency: %+v", r)
		}
		byCurrency[r.Currency]++
	}
	if byCurrency["USD"] != 1 || byCurrency["EUR"] != 1 {
		t.Fatalf("expected one USD and one EUR row, got %+v (rows=%+v)", byCurrency, rows)
	}
	// The projection's MixedCurrency flag must be set: SpendByCurrency
	// will not collapse two currencies into one sum.
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC), WindowDays: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !proj.Totals.MixedCurrency {
		t.Fatalf("mixed-currency import must set the projection's MixedCurrency flag; got %+v", proj.Totals)
	}
	if proj.Totals.SpendByCurrency["USD"] == 0 || proj.Totals.SpendByCurrency["EUR"] == 0 {
		t.Fatalf("SpendByCurrency must carry both currencies; got %+v", proj.Totals.SpendByCurrency)
	}
}

// Audit 2026-09-16, finding 3 (ads panel): revenue completeness was
// represented by an "any revenue exists" flag, so a window with one row
// reporting revenue and many rows with empty revenue cells looked as
// observed-and-complete as a window where every row reported revenue.
//
// TestAdsImportRevenueCoverageCannotPretendToBeComplete is the red
// falsifier: the projection must tell the consumer what fraction of the
// contributing rows actually reported revenue, not collapse the
// distinction into a boolean.
func TestAdsImportRevenueCoverageCannotPretendToBeComplete(t *testing.T) {
	home := t.TempDir()
	tmp := t.TempDir()
	csv1 := filepath.Join(tmp, "p.csv")
	if err := os.WriteFile(csv1,
		[]byte("date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
			"2026-09-01,c1,Mixed,10,1000,50,2,30,USD\n"+
			"2026-09-02,c1,Mixed,12,1100,55,3,,USD\n"+
			"2026-09-03,c1,Mixed,14,1200,60,4,,USD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAdsReport(home, csv1, AdsImportOptions{
		Platform: "other", ProductID: "product:demo",
		Now: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 3, 23, 0, 0, 0, time.UTC), WindowDays: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := 1.0 / 3.0
	if proj.Totals.RevenueCoverage < want-1e-6 || proj.Totals.RevenueCoverage > want+1e-6 {
		t.Fatalf("RevenueCoverage = %v, want %v (1 of 3 rows reported revenue)", proj.Totals.RevenueCoverage, want)
	}
	if proj.Totals.ROAS != 0 {
		t.Fatalf("partial-coverage ROAS must NOT be reported as a complete ratio: %+v", proj.Totals)
	}
	// 3 of 3 days reporting revenue IS the complete case: ROAS defined.
	csv2 := filepath.Join(tmp, "p2.csv")
	if err := os.WriteFile(csv2,
		[]byte("date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
			"2026-09-01,c1,Mixed,10,1000,50,2,30,USD\n"+
			"2026-09-02,c1,Mixed,12,1100,55,3,36,USD\n"+
			"2026-09-03,c1,Mixed,14,1200,60,4,42,USD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAdsReport(home, csv2, AdsImportOptions{
		Platform: "other", ProductID: "product:demo",
		Now: time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	proj2, err := LoadAdsProjection(home, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 3, 23, 0, 0, 0, time.UTC), WindowDays: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proj2.Totals.RevenueCoverage < 1-1e-6 {
		t.Fatalf("complete-coverage window must read coverage = 1, got %+v", proj2.Totals)
	}
	if proj2.Totals.ROAS == 0 {
		t.Fatalf("complete-coverage window must report ROAS, got 0")
	}
}

// TestAdsImportRevenueCoverageJSONShape pins the projection JSON contract:
// a consumer reading projection JSON must see a RevenueCoverage field that
// tells it how complete the revenue evidence is, in addition to the boolean
// HasRevenue kept for backwards compatibility.
func TestAdsImportRevenueCoverageJSONShape(t *testing.T) {
	home := t.TempDir()
	tmp := t.TempDir()
	p := filepath.Join(tmp, "p.csv")
	if err := os.WriteFile(p,
		[]byte("date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n2026-09-01,c1,Mixed,10,1000,50,2,,USD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAdsReport(home, p, AdsImportOptions{
		Platform: "other", ProductID: "product:demo",
		Now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC), WindowDays: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"revenue_coverage"`) {
		t.Fatalf("projection JSON must carry revenue_coverage; got %s", raw)
	}
}
