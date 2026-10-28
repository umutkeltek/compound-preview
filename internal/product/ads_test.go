package product

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
)

func adsFixture(name string) string {
	return filepath.Join("..", "..", "fixtures", "product", "ads", name)
}

func mustImportAds(t *testing.T, home, platform, productID, fixture string) AdsImportReport {
	t.Helper()
	report, err := ImportAdsReport(home, adsFixture(fixture), AdsImportOptions{
		Platform:  platform,
		ProductID: productID,
		Now:       time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("import %s (%s): %v", fixture, platform, err)
	}
	return report
}

func adsLedgerLines(t *testing.T, home string) int {
	t.Helper()
	b, err := os.ReadFile(adsRowsLog(home))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}

func near(a, b float64) bool { return a-b < 1e-6 && b-a < 1e-6 }

// TestAdsImportParsesEveryPlatformDialect would FAIL if any platform's real
// export headers stopped matching, if Google's title lines or Total row
// leaked into rows, if "(USD)" stopped supplying a currency, if US slash dates
// or Turkish "1.250,40" numbers were misread, or if taps/results/installs
// stopped mapping onto clicks/conversions.
func TestAdsImportParsesEveryPlatformDialect(t *testing.T) {
	cases := []struct {
		platform, fixture  string
		rows, skipped      int
		campaigns          int
		spend, impressions float64
		clicks, conv       float64
		revenue            float64
		hasRevenue         bool
		currency           string
		firstDate, lastDat string
	}{
		{"apple_search_ads", "apple-search-ads.csv", 6, 0, 2, 112.5, 24097, 915, 55, 0, false, "USD", "2026-09-01", "2026-09-03"},
		{"google_ads", "google-ads.csv", 3, 1, 1, 3753.15, 37180, 1245, 55, 9090, true, "TRY", "2026-09-01", "2026-09-03"},
		{"meta_ads", "meta-ads.csv", 3, 0, 1, 119.75, 66500, 933, 12, 288, true, "USD", "2026-09-01", "2026-09-03"},
		{"tiktok_ads", "tiktok-ads.csv", 3, 0, 1, 45, 29370, 369, 6, 0, false, "USD", "2026-09-01", "2026-09-03"},
		{"x_ads", "x-ads.csv", 3, 0, 1, 24, 12350, 179, 3, 0, false, "USD", "2026-09-01", "2026-09-03"},
		{"other", "canonical.csv", 4, 0, 2, 41, 3700, 175, 9, 135, true, "USD", "2026-09-01", "2026-09-03"},
	}
	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			home := t.TempDir()
			report := mustImportAds(t, home, tc.platform, "product:demo", tc.fixture)
			imp := report.Import
			if imp.RowsParsed != tc.rows || imp.RowsSkipped != tc.skipped || imp.RowsNew != tc.rows {
				t.Fatalf("rows parsed/skipped/new = %d/%d/%d, want %d/%d/%d", imp.RowsParsed, imp.RowsSkipped, imp.RowsNew, tc.rows, tc.skipped, tc.rows)
			}
			if imp.CampaignsSeen != tc.campaigns || imp.Currency != tc.currency || imp.DateStart != tc.firstDate || imp.DateEnd != tc.lastDat {
				t.Fatalf("import summary = %+v", imp)
			}
			rows, err := LoadAdRows(home)
			if err != nil {
				t.Fatal(err)
			}
			totals := adsTotals(rows)
			if !near(totals.Spend, tc.spend) || !near(totals.Impressions, tc.impressions) || !near(totals.Clicks, tc.clicks) || !near(totals.Conversions, tc.conv) || !near(totals.Revenue, tc.revenue) || totals.HasRevenue != tc.hasRevenue {
				t.Fatalf("totals = %+v, want spend %v impr %v clicks %v conv %v revenue %v hasRevenue %v", totals, tc.spend, tc.impressions, tc.clicks, tc.conv, tc.revenue, tc.hasRevenue)
			}
			for _, r := range rows {
				if r.Platform != tc.platform || r.ProductID != "product:demo" || r.Currency != tc.currency || r.SchemaVersion != AdsRowSchemaVersion {
					t.Fatalf("row = %+v", r)
				}
				if !strings.HasPrefix(r.ID, "adrow_") || !strings.HasPrefix(r.ImportID, "adimp_") || !strings.Contains(r.SourceRef, "@sha256:") {
					t.Fatalf("row identity = %+v", r)
				}
			}
		})
	}
}

func TestAdsImportSumsRowsThatShareCampaignAndDay(t *testing.T) {
	home := t.TempDir()
	report := mustImportAds(t, home, "other", "product:demo", "split-rows.csv")
	if report.Import.RowsParsed != 2 || report.Import.RowsNew != 1 {
		t.Fatalf("split import = %+v", report.Import)
	}
	rows, _ := LoadAdRows(home)
	if len(rows) != 1 || rows[0].Spend != 10 || rows[0].Impressions != 1000 || rows[0].Clicks != 50 || rows[0].Conversions != 2 {
		t.Fatalf("split rows folded = %+v", rows)
	}
}

// TestAdsImportIsIdempotentAndVersionsChangedRows would FAIL if a re-import
// duplicated ledger lines, if a changed value were silently ignored, or if the
// projection read the older observation instead of the newest.
func TestAdsImportIsIdempotentAndVersionsChangedRows(t *testing.T) {
	home := t.TempDir()
	first := mustImportAds(t, home, "other", "product:demo", "canonical.csv")
	if first.Import.RowsNew != 4 || adsLedgerLines(t, home) != 4 {
		t.Fatalf("first import = %+v lines=%d", first.Import, adsLedgerLines(t, home))
	}
	second := mustImportAds(t, home, "other", "product:demo", "canonical.csv")
	if second.Import.RowsNew != 0 || second.Import.RowsUpdated != 0 || second.Import.RowsUnchanged != 4 || adsLedgerLines(t, home) != 4 {
		t.Fatalf("re-import = %+v lines=%d", second.Import, adsLedgerLines(t, home))
	}
	imports, _ := LoadAdsImports(home)
	if len(imports) != 2 {
		t.Fatalf("import receipts = %d, want 2 (a re-import is evidence too)", len(imports))
	}

	changed := filepath.Join(t.TempDir(), "canonical-changed.csv")
	b, _ := os.ReadFile(adsFixture("canonical.csv"))
	if err := os.WriteFile(changed, bytes.Replace(b, []byte("2026-09-03,c1,Canonical One,14,"), []byte("2026-09-03,c1,Canonical One,20,"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := ImportAdsReport(home, changed, AdsImportOptions{Platform: "other", ProductID: "product:demo", Now: time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if third.Import.RowsUpdated != 1 || third.Import.RowsUnchanged != 3 || third.Import.RowsNew != 0 || adsLedgerLines(t, home) != 5 {
		t.Fatalf("changed import = %+v lines=%d", third.Import, adsLedgerLines(t, home))
	}
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{AsOf: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC), WindowDays: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !near(proj.Totals.Spend, 47) {
		t.Fatalf("projection must fold to the newest observation: spend = %v, want 47", proj.Totals.Spend)
	}
}

// TestAdsImportEmptyRevenueColumnDoesNotClaimRevenue would FAIL if a
// conversion-value header with empty cells made ROAS read as 0.00× instead of
// undefined — the exact shape Apple Search Ads exports produce.
func TestAdsImportEmptyRevenueColumnDoesNotClaimRevenue(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(t.TempDir(), "empty-revenue.csv")
	if err := os.WriteFile(p, []byte("date,campaign_id,spend,impressions,clicks,conversions,revenue,currency\n2026-09-01,c1,10,1000,50,2,,USD\n2026-09-02,c1,12,1100,55,3,,USD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := ImportAdsReport(home, p, AdsImportOptions{Platform: "other", ProductID: "product:demo", Now: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Import.HasRevenueCol {
		t.Fatalf("the column exists and the receipt must say so: %+v", report.Import)
	}
	rows, _ := LoadAdRows(home)
	for _, r := range rows {
		if r.HasRevenue {
			t.Fatalf("empty revenue cell must not claim revenue: %+v", r)
		}
	}
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{AsOf: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), WindowDays: 3})
	if err != nil {
		t.Fatal(err)
	}
	if proj.Totals.HasRevenue || proj.Totals.ROAS != 0 {
		t.Fatalf("ROAS must be undefined: %+v", proj.Totals)
	}
	withValues := filepath.Join(t.TempDir(), "revenue.csv")
	os.WriteFile(withValues, []byte("date,campaign_id,spend,impressions,clicks,conversions,revenue,currency\n2026-09-01,c2,10,1000,50,2,30,USD\n"), 0o644)
	if _, err := ImportAdsReport(home, withValues, AdsImportOptions{Platform: "other", ProductID: "product:demo"}); err != nil {
		t.Fatal(err)
	}
	proj, _ = LoadAdsProjection(home, AdsProjectionOptions{AsOf: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), WindowDays: 3})
	// 1 of 3 rows in the window reports revenue: coverage is partial,
	// so ROAS is gated to 0 (audit 2026-09-16 finding 3). The
	// consumer reads RevenueCoverage and HasRevenue together.
	if !proj.Totals.HasRevenue || proj.Totals.ROAS != 0 {
		t.Fatalf("partial-coverage window must NOT report a complete ROAS: %+v", proj.Totals)
	}
	if proj.Totals.RevenueCoverage == 0 {
		t.Fatalf("partial-coverage window must surface RevenueCoverage: %+v", proj.Totals)
	}
}

func TestAdsImportFailsClosedOnBadInput(t *testing.T) {
	home := t.TempDir()
	if _, err := ImportAdsReport(home, adsFixture("canonical.csv"), AdsImportOptions{Platform: "bing_ads", ProductID: "product:demo"}); err == nil || !strings.Contains(err.Error(), "unknown platform") {
		t.Fatalf("unknown platform err = %v", err)
	}
	if _, err := ImportAdsReport(home, adsFixture("canonical.csv"), AdsImportOptions{Platform: "other"}); err == nil || !strings.Contains(err.Error(), "--product") {
		t.Fatalf("missing product err = %v", err)
	}
	noSpend := filepath.Join(t.TempDir(), "no-spend.csv")
	os.WriteFile(noSpend, []byte("date,campaign_id,impressions\n2026-09-01,c1,100\n"), 0o644)
	if _, err := ImportAdsReport(home, noSpend, AdsImportOptions{Platform: "other", ProductID: "product:demo"}); err == nil || !strings.Contains(err.Error(), "required columns") {
		t.Fatalf("missing spend column err = %v", err)
	}
	onlyTotals := filepath.Join(t.TempDir(), "totals.csv")
	os.WriteFile(onlyTotals, []byte("date,spend,impressions\nTotal,10,100\n"), 0o644)
	if _, err := ImportAdsReport(home, onlyTotals, AdsImportOptions{Platform: "other", ProductID: "product:demo"}); err == nil || !strings.Contains(err.Error(), "parseable date") {
		t.Fatalf("totals-only err = %v", err)
	}
	if adsLedgerLines(t, home) != 0 {
		t.Fatalf("a refused import must not write rows, got %d lines", adsLedgerLines(t, home))
	}
}

// TestAdsProjectionWindowsStatusesAndGaps would FAIL if the window cut the
// wrong days, if a platform with no rows disappeared instead of reading
// not_connected, if stale/connected flipped at the wrong age, if unlike
// currencies were summed, or if the filter leaked other products' rows.
func TestAdsProjectionWindowsStatusesAndGaps(t *testing.T) {
	home := t.TempDir()
	mustImportAds(t, home, "apple_search_ads", "product:demo-ios", "apple-search-ads.csv")
	mustImportAds(t, home, "google_ads", "product:demo-web", "google-ads.csv")

	proj, err := LoadAdsProjection(home, AdsProjectionOptions{AsOf: time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC), WindowDays: 3})
	if err != nil {
		t.Fatal(err)
	}
	if proj.SchemaVersion != AdsProjectionSchemaVersion || proj.AuthorityRef != AdsAuthorityRef || proj.Status != "ok" {
		t.Fatalf("header = %+v", proj)
	}
	if proj.Window.Start != "2026-09-01" || proj.Window.End != "2026-09-03" || proj.Window.PreviousStart != "2026-08-29" || proj.Window.PreviousEnd != "2026-08-31" {
		t.Fatalf("window = %+v", proj.Window)
	}
	if proj.Counts.Rows != 9 || proj.Counts.RowsInWindow != 9 || proj.Counts.Products != 2 || proj.Counts.Campaigns != 3 || proj.Counts.CampaignsActive != 3 {
		t.Fatalf("counts = %+v", proj.Counts)
	}
	if !proj.Totals.MixedCurrency || proj.Totals.Currency != adsCurrencyMixed || !near(proj.Totals.SpendByCurrency["USD"], 112.5) || !near(proj.Totals.SpendByCurrency["TRY"], 3753.15) {
		t.Fatalf("mixed currency totals = %+v", proj.Totals)
	}
	if proj.Previous.Rows != 0 || proj.Previous.Spend != 0 {
		t.Fatalf("previous window should be empty: %+v", proj.Previous)
	}
	if len(proj.Platforms) != len(adPlatforms) {
		t.Fatalf("platforms rendered = %d, want the whole registry (%d)", len(proj.Platforms), len(adPlatforms))
	}
	status := map[string]string{}
	for _, p := range proj.Platforms {
		status[p.Platform] = p.Status
		if len(p.Daily) != 3 {
			t.Fatalf("%s daily = %d points, want 3 (zero-filled)", p.Platform, len(p.Daily))
		}
		if p.NextMove.Command == "" || p.NextMove.Text == "" {
			t.Fatalf("%s has no next move", p.Platform)
		}
	}
	if status["apple_search_ads"] != "connected" || status["google_ads"] != "connected" || status["meta_ads"] != "not_connected" || status["other"] != "not_connected" {
		t.Fatalf("statuses = %v", status)
	}
	if proj.Counts.PlatformsConnected != 2 || proj.Counts.PlatformsNotConnected != len(adPlatforms)-2 || proj.Counts.PlatformsStale != 0 {
		t.Fatalf("platform counts = %+v", proj.Counts)
	}
	apple := proj.Platforms[0]
	if apple.Platform != "apple_search_ads" || !near(apple.Totals.Spend, 112.5) || apple.Totals.Currency != "USD" || apple.LastDate != "2026-09-03" || apple.CampaignsActive != 2 || !near(apple.Totals.CTR, 915.0/24097.0) || !near(apple.Totals.CPA, 112.5/55) {
		t.Fatalf("apple view = %+v", apple)
	}
	if !strings.Contains(apple.NextMove.Command, "ads signal --platform apple_search_ads --product product:demo-ios --before 2026-08-29/2026-08-31 --after 2026-09-01/2026-09-03") {
		t.Fatalf("connected next move = %+v", apple.NextMove)
	}
	if len(proj.Campaigns) != 3 || proj.Campaigns[0].Platform != "google_ads" || !near(proj.Campaigns[0].Totals.Spend, 3753.15) || proj.Campaigns[0].Status != "active" || !near(proj.Campaigns[0].Totals.ROAS, 9090.0/3753.15) {
		t.Fatalf("campaigns = %+v", proj.Campaigns)
	}
	if len(proj.Products) != 2 || proj.Products[0].ProductID != "product:demo-web" || len(proj.Products[1].ByPlatform) != 1 || !near(proj.Products[1].ByPlatform[0].Share, 1) {
		t.Fatalf("products = %+v", proj.Products)
	}
	kinds := map[string]int{}
	for _, g := range proj.Gaps {
		kinds[g.Kind]++
	}
	if kinds["platform_not_connected"] != len(adPlatforms)-3 || kinds["mixed_currency"] != 1 || kinds["platform_stale"] != 0 {
		t.Fatalf("gaps = %v", kinds)
	}
	if len(proj.Imports) != 2 || proj.Imports[0].Platform != "google_ads" {
		t.Fatalf("imports newest-first = %+v", proj.Imports)
	}

	// A week later nothing new was imported: both platforms read stale, the
	// window is quiet, and every campaign is quiet.
	later, err := LoadAdsProjection(home, AdsProjectionOptions{AsOf: time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC), WindowDays: 3})
	if err != nil {
		t.Fatal(err)
	}
	if later.Status != "quiet" || later.Counts.RowsInWindow != 0 || later.Counts.PlatformsStale != 2 || later.Counts.CampaignsActive != 0 {
		t.Fatalf("later = status %s counts %+v", later.Status, later.Counts)
	}
	if later.Platforms[0].Status != "stale" || later.Platforms[0].DaysSinceLastDate != 7 || !strings.Contains(later.Platforms[0].NextMove.Text, "7 days old") {
		t.Fatalf("stale view = %+v", later.Platforms[0])
	}

	// Product filter: only that product's rows, but still the whole registry.
	only, err := LoadAdsProjection(home, AdsProjectionOptions{AsOf: time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC), WindowDays: 3, ProductID: "product:demo-ios"})
	if err != nil {
		t.Fatal(err)
	}
	if only.Counts.Rows != 6 || len(only.Products) != 1 || only.Totals.MixedCurrency || only.Totals.Currency != "USD" || len(only.Platforms) != len(adPlatforms) || only.Counts.PlatformsConnected != 1 {
		t.Fatalf("filtered = counts %+v totals %+v platforms %d", only.Counts, only.Totals, len(only.Platforms))
	}
	if _, err := LoadAdsProjection(home, AdsProjectionOptions{Platform: "bing"}); err == nil {
		t.Fatal("unknown platform filter must refuse")
	}
	empty, err := LoadAdsProjection(t.TempDir(), AdsProjectionOptions{})
	if err != nil || empty.Status != "empty" || len(empty.Platforms) != len(adPlatforms) || empty.Window.Days != adsDefaultWindowDays {
		t.Fatalf("empty home = %+v err %v", empty, err)
	}
}

// TestAdsSignalBridgeAppliesThroughProductSignalDoor would FAIL if the bridge
// bypassed ApplySignal, fabricated a metric whose denominator is zero, lost
// the platform in the evidence refs, or produced unstable signal ids.
func TestAdsSignalBridgeAppliesThroughProductSignalDoor(t *testing.T) {
	home := t.TempDir()
	mustImportAds(t, home, "apple_search_ads", "product:demo-ios", "apple-search-ads.csv")
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := BuildAdsSignals(home, AdsSignalOptions{
		Platform:  "apple_search_ads",
		ProductID: "product:demo-ios",
		Before:    "2026-09-01/2026-09-01",
		After:     "2026-09-02/2026-09-03",
		Metric:    "spend,conversions,cpa,roas",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("docs = %d, want 3 (roas undefined without revenue must be skipped)", len(docs))
	}
	byMetric := map[string]SignalDocument{}
	for _, doc := range docs {
		byMetric[doc.Signal.MetricName] = doc
	}
	spend := byMetric["ads_spend"]
	if spend.Source.Kind != "apple_search_ads" || !near(spend.Signal.Before, 37.5) || !near(spend.Signal.After, 75) || spend.Signal.Surface != "paid-acquisition" || spend.Signal.ObservedAt != "2026-09-03T00:00:00Z" {
		t.Fatalf("spend doc = %+v", spend)
	}
	foundPlatformRef := false
	for _, ref := range spend.Signal.EvidenceRefs {
		if strings.HasPrefix(ref, "ads:apple_search_ads:") {
			foundPlatformRef = true
		}
	}
	if !foundPlatformRef {
		t.Fatalf("evidence refs must name the platform: %v", spend.Signal.EvidenceRefs)
	}
	if !near(byMetric["ads_cpa"].Signal.Before, 37.5/18) || !near(byMetric["ads_cpa"].Signal.After, 75.0/37) {
		t.Fatalf("cpa doc = %+v", byMetric["ads_cpa"])
	}
	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	for _, doc := range docs {
		if _, err := ApplySignal(home, store, doc, "product-ads.jsonl", now); err != nil {
			t.Fatalf("apply %s: %v", doc.Signal.MetricName, err)
		}
	}
	// Replay is idempotent through the same door.
	again, _ := BuildAdsSignals(home, AdsSignalOptions{Platform: "apple_search_ads", ProductID: "product:demo-ios", Before: "2026-09-01/2026-09-01", After: "2026-09-02/2026-09-03", Metric: "spend"})
	if _, err := ApplySignal(home, store, again[0], "product-ads.jsonl", now.Add(time.Hour)); err != nil {
		t.Fatalf("replay must be accepted: %v", err)
	}
	tl, err := LoadTimeline(home, "product:demo-ios")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Signals) != 3 {
		t.Fatalf("timeline signals = %d, want 3", len(tl.Signals))
	}
	if _, err := BuildAdsSignals(home, AdsSignalOptions{Platform: "apple_search_ads", ProductID: "product:demo-ios", Before: "2026-01-01/2026-01-02", After: "2026-01-03/2026-01-04"}); err == nil || !strings.Contains(err.Error(), "no apple_search_ads rows") {
		t.Fatalf("empty windows must refuse, got %v", err)
	}
	if _, err := BuildAdsSignals(home, AdsSignalOptions{Platform: "apple_search_ads", ProductID: "product:demo-ios", Before: "2026-09-01/2026-09-01", After: "2026-09-02/2026-09-03", Metric: "ctr,nope"}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown metric must refuse, got %v", err)
	}
}

// TestRunAdsCLIImportProjectionAndPlatforms drives the verb family the way
// cmd/compound does, proving the JSON doors the console reads.
func TestRunAdsCLIImportProjectionAndPlatforms(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	var stdout, stderr bytes.Buffer

	if code := RunAdsCLI([]string{"import", "--platform", "meta_ads", "--product", "product:demo-app", "--report", adsFixture("meta-ads.csv"), "--json"}, home, nil, &stdout, &stderr, now); code != 0 {
		t.Fatalf("import exit %d: %s", code, stderr.String())
	}
	var imported AdsImportReport
	if err := json.Unmarshal(stdout.Bytes(), &imported); err != nil || imported.Import.RowsNew != 3 || imported.Import.Currency != "USD" {
		t.Fatalf("import json = %s err %v", stdout.String(), err)
	}

	stdout.Reset()
	if code := RunAdsCLI([]string{"--json", "--window", "7d", "--as-of", "2026-09-03"}, home, nil, &stdout, &stderr, now); code != 0 {
		t.Fatalf("projection exit %d: %s", code, stderr.String())
	}
	var proj AdsProjection
	if err := json.Unmarshal(stdout.Bytes(), &proj); err != nil {
		t.Fatalf("projection json: %v\n%s", err, stdout.String())
	}
	if proj.SchemaVersion != AdsProjectionSchemaVersion || proj.Status != "ok" || proj.Window.Days != 7 || !near(proj.Totals.Spend, 119.75) || !near(proj.Totals.ROAS, 288/119.75) {
		t.Fatalf("projection = %+v", proj)
	}

	stdout.Reset()
	if code := RunAdsCLI([]string{"--as-of", "2026-09-03", "--window", "3"}, home, nil, &stdout, &stderr, now); code != 0 || !strings.Contains(stdout.String(), "# product ads (2026-09-01 → 2026-09-03, 3 days)") || !strings.Contains(stdout.String(), "platform meta_ads: connected") {
		t.Fatalf("markdown projection exit %d:\n%s", code, stdout.String())
	}

	stdout.Reset()
	if code := RunAdsCLI([]string{"platforms", "--json"}, home, nil, &stdout, &stderr, now); code != 0 {
		t.Fatalf("platforms exit %d", code)
	}
	var platforms struct {
		Platforms []struct{ ID string } `json:"platforms"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &platforms); err != nil || len(platforms.Platforms) != len(adPlatforms) {
		t.Fatalf("platforms json = %s err %v", stdout.String(), err)
	}

	stderr.Reset()
	if code := RunAdsCLI([]string{"--window", "never"}, home, nil, &stdout, &stderr, now); code != 2 || !strings.Contains(stderr.String(), "--window") {
		t.Fatalf("bad window exit %d stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := RunAdsCLI([]string{"import", "--platform", "meta_ads", "--product", "product:demo-app"}, home, nil, &stdout, &stderr, now); code != 2 {
		t.Fatalf("missing report exit %d", code)
	}
	stderr.Reset()
	if code := RunAdsCLI([]string{"import", "--platform", "nope", "--product", "p", "--report", adsFixture("meta-ads.csv")}, home, nil, &stdout, &stderr, now); code != 1 || !strings.Contains(stderr.String(), "unknown platform") {
		t.Fatalf("unknown platform exit %d stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := RunAdsCLI([]string{"signal", "--platform", "meta_ads", "--product", "product:demo-app", "--before", "2026-09-01/2026-09-01", "--after", "2026-09-02/2026-09-03"}, home, nil, &stdout, &stderr, now); code != 1 || !strings.Contains(stderr.String(), "memory store required") {
		t.Fatalf("signal without store exit %d stderr %q", code, stderr.String())
	}
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := RunAdsCLI([]string{"signal", "--platform", "meta_ads", "--product", "product:demo-app", "--before", "2026-09-01/2026-09-01", "--after", "2026-09-02/2026-09-03", "--metric", "spend,roas"}, home, store, &stdout, &stderr, now); code != 0 || !strings.Contains(stdout.String(), "ads_spend") || !strings.Contains(stdout.String(), "ads_roas") {
		t.Fatalf("signal exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
}

func TestParseAdNumberAndDate(t *testing.T) {
	numbers := map[string]float64{
		"12.50": 12.5, "$40.00": 40, "1,840": 1840, "1.250,40": 1250.40, "12,5": 12.5,
		"2.980,00": 2980, "1,234,567.89": 1234567.89, "₺1.322,75": 1322.75, "": 0, "--": 0, "3%": 3, "1 234,50": 1234.5,
	}
	for raw, want := range numbers {
		got, err := parseAdNumber(raw)
		if err != nil || !near(got, want) {
			t.Fatalf("parseAdNumber(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := parseAdNumber("abc"); err == nil {
		t.Fatal("letters must not parse")
	}
	dates := map[string]string{
		"2026-09-01": "2026-09-01", "09/01/2026": "2026-09-01", "9/1/2026": "2026-09-01", "01.09.2026": "2026-09-01",
		"Sep 1, 2026": "2026-09-01", "1 Sep 2026": "2026-09-01", "2026-09-01T00:00:00Z": "2026-09-01", "2026-09-01 - 2026-09-01": "2026-09-01",
	}
	for raw, want := range dates {
		got, ok := parseAdDate(raw)
		if !ok || got != want {
			t.Fatalf("parseAdDate(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"Total", "", "Totals: Account", "n/a"} {
		if _, ok := parseAdDate(raw); ok {
			t.Fatalf("parseAdDate(%q) must not parse", raw)
		}
	}
}
