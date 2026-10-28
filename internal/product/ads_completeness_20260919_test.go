package product

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Review 2026-09-19 (ads): aggregation still lost completeness in three
// places after the 2026-09-16 currency partition landed. Each test below is
// the falsifier for one of them; each would pass on a reducer that invents
// a number where the evidence is missing.

func writeAdsCSV(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAdsTotalsRefuseToLabelUnknownCurrencySpend is case 1: spend was summed
// for every row but only a non-empty currency entered the currency set, so
// 100 USD next to 900 with no currency read as "1000 USD" — 900 of it
// borrowed a code it was never given. The unknown spend must be its own
// bucket, the total must not carry the known code, and ROAS must not be
// reported as if the window were one currency.
func TestAdsTotalsRefuseToLabelUnknownCurrencySpend(t *testing.T) {
	home := t.TempDir()
	report := writeAdsCSV(t, "known-and-unknown.csv",
		"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
			"2026-09-01,c1,Named,100,1000,50,2,200,USD\n"+
			"2026-09-01,c2,Unnamed,900,9000,450,18,1800,\n")
	imported, err := ImportAdsReport(home, report, AdsImportOptions{
		Platform: "other", ProductID: "product:demo.example",
		Now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Import.Currency == "USD" {
		t.Fatalf("an import where only part of the spend named a currency must not claim that currency: %+v", imported.Import)
	}
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC), WindowDays: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proj.Totals.Currency == "USD" {
		t.Fatalf("100 USD + 900 unlabelled must NOT read as 1000 USD: %+v", proj.Totals)
	}
	if !proj.Totals.MixedCurrency || proj.Totals.Currency != adsCurrencyMixed {
		t.Fatalf("a window mixing a named currency with unnamed spend is not one currency's number: %+v", proj.Totals)
	}
	if !near(proj.Totals.SpendByCurrency["USD"], 100) {
		t.Fatalf("SpendByCurrency must keep the named part: %+v", proj.Totals.SpendByCurrency)
	}
	if !near(proj.Totals.SpendByCurrency[adsCurrencyUnknown], 900) {
		t.Fatalf("SpendByCurrency must show the unresolved part as its own bucket: %+v", proj.Totals.SpendByCurrency)
	}
	if !near(proj.Totals.SpendUnknownCurrency, 900) || proj.Totals.RowsUnknownCurrency != 1 {
		t.Fatalf("unresolved spend must be visible on the totals: %+v", proj.Totals)
	}
	if proj.Totals.ROAS != 0 {
		t.Fatalf("ROAS across a named and an unnamed currency is not a ratio: %+v", proj.Totals)
	}
	kinds := map[string]int{}
	for _, g := range proj.Gaps {
		kinds[g.Kind]++
	}
	if kinds["unknown_currency"] != 1 {
		t.Fatalf("unresolved currency must surface as a gap; gaps = %+v", proj.Gaps)
	}
	if kinds["mixed_currency"] != 0 {
		t.Fatalf("only one currency was named, so the two-currency gap must not fire; gaps = %+v", proj.Gaps)
	}
	// The whole spend is still reported — the rows are real, only the label
	// is missing.
	if !near(proj.Totals.Spend, 1000) {
		t.Fatalf("spend total = %v, want 1000", proj.Totals.Spend)
	}
}

// TestAdsRevenueCoverageCountsContributionsNotGroupedRows is case 2: the
// importer merges same campaign/day/currency contributions and set
// HasRevenue when ANY of them reported revenue, so a row built from
// "spend 100 rev 200" plus "spend 900 rev unknown" read as fully
// revenue-covered. Coverage must be computed from the contributions the
// stored row folded, not from the folded row.
func TestAdsRevenueCoverageCountsContributionsNotGroupedRows(t *testing.T) {
	home := t.TempDir()
	report := writeAdsCSV(t, "partial-revenue.csv",
		"date,campaign_id,campaign_name,device,spend,impressions,clicks,conversions,revenue,currency\n"+
			"2026-09-01,c1,Partial,phone,100,1000,50,2,200,USD\n"+
			"2026-09-01,c1,Partial,tablet,900,9000,450,18,,USD\n")
	if _, err := ImportAdsReport(home, report, AdsImportOptions{
		Platform: "other", ProductID: "product:demo.example",
		Now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := LoadAdRows(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("contributions for one campaign × day × currency must still fold into one row: %+v", rows)
	}
	if !near(rows[0].Spend, 1000) || !near(rows[0].Revenue, 200) || !rows[0].HasRevenue {
		t.Fatalf("folded row = %+v", rows[0])
	}
	if rows[0].Contributions != 2 || rows[0].RevenueContributions != 1 {
		t.Fatalf("the stored row must keep contribution-level completeness: %+v", rows[0])
	}
	// The counts have to survive the ledger, not just the in-memory row.
	raw, err := os.ReadFile(adsRowsLog(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"contributions"`) || !strings.Contains(string(raw), `"revenue_contributions"`) {
		t.Fatalf("ledger line must carry the contribution counts: %s", raw)
	}
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC), WindowDays: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proj.Totals.RevenueCoverage >= 1 {
		t.Fatalf("one of two contributions reported revenue: coverage = %v, must be < 1 (%+v)", proj.Totals.RevenueCoverage, proj.Totals)
	}
	if proj.Totals.ROAS != 0 {
		t.Fatalf("200 of revenue against 1000 of spend where 900 reported none is not a ROAS: %+v", proj.Totals)
	}

	// Teeth in the other direction: when every contribution reports
	// revenue the window is complete and ROAS is defined again.
	complete := t.TempDir()
	full := writeAdsCSV(t, "full-revenue.csv",
		"date,campaign_id,campaign_name,device,spend,impressions,clicks,conversions,revenue,currency\n"+
			"2026-09-01,c1,Full,phone,100,1000,50,2,200,USD\n"+
			"2026-09-01,c1,Full,tablet,900,9000,450,18,1800,USD\n")
	if _, err := ImportAdsReport(complete, full, AdsImportOptions{
		Platform: "other", ProductID: "product:demo.example",
		Now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	fullProj, err := LoadAdsProjection(complete, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC), WindowDays: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullProj.Totals.RevenueCoverage < 1 || !near(fullProj.Totals.ROAS, 2000.0/1000.0) {
		t.Fatalf("a fully covered window must still report ROAS: %+v", fullProj.Totals)
	}
}

// TestAdsLedgerUpgradeLeavesOneActiveGeneration is case 3: the reader folded
// history by the stored row id, and the currency-qualified id is newer than
// some ledgers. Re-importing an observation already stored under the old id
// must supersede it, not leave both generations active and double the spend.
func TestAdsLedgerUpgradeLeavesOneActiveGeneration(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(adsRowsLog(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	// The pre-currency identity: (platform, product, account, campaign, date).
	legacyID := stableID("adrow_", "other", "product:demo.example", "", "c1", "2026-09-01")
	legacy := AdRow{
		SchemaVersion: AdsRowSchemaVersion,
		ID:            legacyID,
		Platform:      "other",
		ProductID:     "product:demo.example",
		CampaignID:    "c1",
		CampaignName:  "Legacy",
		Date:          "2026-09-01",
		Currency:      "USD",
		Spend:         100,
		Impressions:   1000,
		Clicks:        50,
		Conversions:   2,
		Revenue:       200,
		HasRevenue:    true,
		SourceRef:     "legacy.csv@sha256:000000000000",
		ImportID:      "adimp_000000000000",
		ImportedAt:    "2026-09-01T12:00:00Z",
	}
	if err := appendJSONL(adsRowsLog(home), &legacy); err != nil {
		t.Fatal(err)
	}

	same := writeAdsCSV(t, "legacy-same.csv",
		"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
			"2026-09-01,c1,Legacy,100,1000,50,2,200,USD\n")
	if _, err := ImportAdsReport(home, same, AdsImportOptions{
		Platform: "other", ProductID: "product:demo.example",
		Now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	history, err := LoadAdRows(home)
	if err != nil {
		t.Fatal(err)
	}
	if active := LatestAdRows(history); len(active) != 1 {
		t.Fatalf("re-importing an observation stored under the old row id must leave ONE active generation, got %d: %+v", len(active), active)
	}
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC), WindowDays: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !near(proj.Totals.Spend, 100) {
		t.Fatalf("the upgrade must not double the spend: %v, want 100 (%+v)", proj.Totals.Spend, proj.Totals)
	}

	// A changed re-import still versions the observation: one active
	// generation, and it is the newer one.
	changed := writeAdsCSV(t, "legacy-changed.csv",
		"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
			"2026-09-01,c1,Legacy,120,1000,50,2,200,USD\n")
	if _, err := ImportAdsReport(home, changed, AdsImportOptions{
		Platform: "other", ProductID: "product:demo.example",
		Now: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	history, err = LoadAdRows(home)
	if err != nil {
		t.Fatal(err)
	}
	active := LatestAdRows(history)
	if len(active) != 1 || !near(active[0].Spend, 120) {
		t.Fatalf("a changed re-import must supersede the legacy generation, not add to it: %+v", active)
	}
}

// adsRatioWindow imports one report into a fresh home and projects the days
// it covers, so each ratio case below is one CSV and one assertion block.
func adsRatioWindow(t *testing.T, name, body string) AdsProjection {
	t.Helper()
	home := t.TempDir()
	if _, err := ImportAdsReport(home, writeAdsCSV(t, name, body), AdsImportOptions{
		Platform: "other", ProductID: "product:demo.example",
		Now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	proj, err := LoadAdsProjection(home, AdsProjectionOptions{
		AsOf: time.Date(2026, 9, 2, 23, 0, 0, 0, time.UTC), WindowDays: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return proj
}

// TestAdsMoneyRatiosNeedOneResolvedCurrency is the follow-up to the currency
// partition: CPM, CPC and CPA divide spend by a count, so they inherit
// whatever the spend total is. When the window is not one resolved currency
// the numerator is not a currency's number, and neither is the quotient —
// the same reason ROAS is already withheld. CTR has no money in it and must
// survive, which is what keeps this from becoming over-withholding.
func TestAdsMoneyRatiosNeedOneResolvedCurrency(t *testing.T) {
	withheld := func(t *testing.T, label string, got AdsTotals) {
		t.Helper()
		if got.CPM != 0 || got.CPC != 0 || got.CPA != 0 {
			t.Fatalf("%s: spend is not one currency's number, so cost-per-unit is not a cost: %+v", label, got)
		}
		if got.ROAS != 0 {
			t.Fatalf("%s: ROAS must stay withheld too: %+v", label, got)
		}
		if !near(got.CTR, 0.05) {
			t.Fatalf("%s: CTR carries no money and must survive: %+v", label, got)
		}
	}

	t.Run("two named currencies", func(t *testing.T) {
		proj := adsRatioWindow(t, "usd-and-eur.csv",
			"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
				"2026-09-01,c1,Named USD,100,1000,50,2,200,USD\n"+
				"2026-09-01,c2,Named EUR,900,9000,450,18,1800,EUR\n")
		if !proj.Totals.MixedCurrency {
			t.Fatalf("precondition: window must read mixed, got %+v", proj.Totals)
		}
		withheld(t, "usd+eur", proj.Totals)
	})

	t.Run("named next to unnamed currency", func(t *testing.T) {
		proj := adsRatioWindow(t, "usd-and-unnamed.csv",
			"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
				"2026-09-01,c1,Named,100,1000,50,2,200,USD\n"+
				"2026-09-01,c2,Unnamed,900,9000,450,18,1800,\n")
		withheld(t, "usd+unknown", proj.Totals)
	})

	t.Run("no currency named at all", func(t *testing.T) {
		// Not "mixed" — nothing disagrees — but still unresolved: two
		// un-currency-tagged reports can be two different currencies and
		// the ledger cannot tell. This is the half of the condition a
		// MixedCurrency-only guard would miss.
		proj := adsRatioWindow(t, "unnamed-only.csv",
			"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue\n"+
				"2026-09-01,c1,Unnamed One,100,1000,50,2,200\n"+
				"2026-09-01,c2,Unnamed Two,900,9000,450,18,1800\n")
		if proj.Totals.MixedCurrency || proj.Totals.RowsUnknownCurrency != 2 {
			t.Fatalf("precondition: window must be unresolved but not mixed, got %+v", proj.Totals)
		}
		withheld(t, "all-unknown", proj.Totals)
	})

	t.Run("one resolved currency still reports all three", func(t *testing.T) {
		proj := adsRatioWindow(t, "usd-only.csv",
			"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
				"2026-09-01,c1,Named One,100,1000,50,2,200,USD\n"+
				"2026-09-01,c2,Named Two,900,9000,450,18,1800,USD\n")
		got := proj.Totals
		if got.Currency != "USD" || got.MixedCurrency {
			t.Fatalf("precondition: window must be single-currency, got %+v", got)
		}
		if !near(got.CPM, 100) || !near(got.CPC, 2) || !near(got.CPA, 50) {
			t.Fatalf("a resolved single-currency window must still report CPM/CPC/CPA: %+v", got)
		}
		if !near(got.ROAS, 2) || !near(got.CTR, 0.05) {
			t.Fatalf("a resolved single-currency window must still report ROAS and CTR: %+v", got)
		}
	})

	// The campaign view groups by (platform, product, account, campaign) and
	// NOT by currency, so one campaign reported in two currencies on two days
	// is one grouped view over unlike money. It reduces through the same
	// adsTotals, so the guard reaches it — this proves that rather than
	// assuming it.
	t.Run("campaign view combining unlike currencies", func(t *testing.T) {
		proj := adsRatioWindow(t, "campaign-switched-currency.csv",
			"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
				"2026-09-01,c1,Switched,100,1000,50,2,200,USD\n"+
				"2026-09-02,c1,Switched,900,9000,450,18,1800,EUR\n")
		if len(proj.Campaigns) != 1 {
			t.Fatalf("precondition: campaign grouping is currency-blind, so this is one view: %+v", proj.Campaigns)
		}
		view := proj.Campaigns[0]
		if !near(view.Totals.Spend, 1000) || !view.Totals.MixedCurrency {
			t.Fatalf("precondition: campaign view must combine both currencies: %+v", view.Totals)
		}
		withheld(t, "campaign usd+eur", view.Totals)
	})

	// The text CLI must not print a withheld ratio as "0.00"; it already has
	// a dash for undefined, and an unresolved currency is undefined.
	t.Run("cli prints a dash, never a zero", func(t *testing.T) {
		home := t.TempDir()
		now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
		report := writeAdsCSV(t, "cli-mixed.csv",
			"date,campaign_id,campaign_name,spend,impressions,clicks,conversions,revenue,currency\n"+
				"2026-09-01,c1,Named USD,100,1000,50,2,200,USD\n"+
				"2026-09-01,c2,Named EUR,900,9000,450,18,1800,EUR\n")
		var stdout, stderr bytes.Buffer
		if code := RunAdsCLI([]string{"import", "--platform", "other", "--product", "product:demo.example", "--report", report}, home, nil, &stdout, &stderr, now); code != 0 {
			t.Fatalf("import exit %d: %s", code, stderr.String())
		}
		stdout.Reset()
		if code := RunAdsCLI([]string{"--as-of", "2026-09-02", "--window", "2"}, home, nil, &stdout, &stderr, now); code != 0 {
			t.Fatalf("projection exit %d: %s", code, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "cpc: — · cpa: — · roas: —") {
			t.Fatalf("withheld ratios must render as dashes, not zeros:\n%s", out)
		}
		if !strings.Contains(out, "ctr: 5.00%") {
			t.Fatalf("CTR must still render:\n%s", out)
		}
	})
}
