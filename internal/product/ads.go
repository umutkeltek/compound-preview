package product

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Paid acquisition ("ads") is one more rented edge: Compound never talks to an
// ad platform. Each platform's own campaign report export (CSV/TSV) is
// normalized into ONE ledger of daily campaign rows, and every reader — the
// console's ads panel, the loop bridge, the sources page — projects from that
// ledger. The export is the operator's evidence; the ledger keeps every
// observation (a re-import appends a newer version of a row, never rewrites
// one) and the projection folds to the latest observation per row identity.
//
// This mirrors the ASC / Search Console / GA4 / AdSense adapters in this
// package on purpose: one CLI shape (`--platform`, `--product`, a report
// file), fail-closed parsing, and no fetcher.

const (
	AdsRowSchemaVersion        = "compound.product_ads_row.v0.1"
	AdsImportSchemaVersion     = "compound.product_ads_import.v0.1"
	AdsProjectionSchemaVersion = "compound.product_ads.v0.2"
	AdsAuthorityRef            = "compound://ledger/product-ads"

	// adsDefaultWindowDays is the panel's default look-back.
	adsDefaultWindowDays = 30
	// adsDefaultStaleAfter: ad platforms report daily, so a platform whose
	// newest row is older than this reads as stale rather than connected.
	adsDefaultStaleAfter = 72 * time.Hour
	// adsCurrencyMixed marks totals that would otherwise sum unlike
	// currencies. Summing them would be a number that is not evidence.
	adsCurrencyMixed = "MIXED"
	// adsCurrencyUnknown is the bucket for spend whose report never named a
	// currency. It is not an ISO code and cannot collide with one. Without
	// an explicit bucket, unnamed spend silently borrowed whichever code the
	// other rows in the window carried (review 2026-09-19).
	adsCurrencyUnknown = "UNKNOWN"
)

// AdPlatform is one entry of the platform registry. The registry is the
// "all platforms" the panel renders: a platform with no rows is shown as
// not connected, with the exact import command, rather than being absent.
type AdPlatform struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	ExportHint string `json:"export_hint"`
	aliases    adColumnAliases
}

// adColumnAliases lists the report headers each platform is known to emit
// for a canonical column. Matching is case-insensitive and ignores a
// trailing parenthesised qualifier such as "(USD)". The canonical names
// (date, campaign_id, spend, ...) are accepted for every platform, so any
// export can be hand-mapped without teaching the adapter a new dialect.
type adColumnAliases struct {
	date, campaignID, campaignName, spend, impressions, clicks, conversions, revenue, currency []string
}

var adsCanonicalAliases = adColumnAliases{
	date:         []string{"date"},
	campaignID:   []string{"campaign_id"},
	campaignName: []string{"campaign_name"},
	spend:        []string{"spend"},
	impressions:  []string{"impressions"},
	clicks:       []string{"clicks"},
	conversions:  []string{"conversions"},
	revenue:      []string{"revenue"},
	currency:     []string{"currency"},
}

var adPlatforms = []AdPlatform{
	{
		ID:         "apple_search_ads",
		Label:      "Apple Search Ads",
		ExportHint: "Apple Search Ads → Campaigns → set the date range → Export (CSV), campaign level, daily.",
		aliases: adColumnAliases{
			date:         []string{"day", "report date"},
			campaignID:   []string{"campaign id"},
			campaignName: []string{"campaign name", "campaign"},
			spend:        []string{"spend", "cost", "total spend"},
			impressions:  []string{"impressions"},
			clicks:       []string{"taps", "clicks"},
			conversions:  []string{"installs", "total installs", "installs (tap-through)", "tap-through installs", "new downloads", "conversions"},
			revenue:      []string{"conversion value", "conv. value"},
			currency:     []string{"currency", "spend currency"},
		},
	},
	{
		ID:         "google_ads",
		Label:      "Google Ads",
		ExportHint: "Google Ads → Campaigns → segment by Day → Download (CSV). The two title lines and the Total rows are skipped.",
		aliases: adColumnAliases{
			date:         []string{"day", "week", "month"},
			campaignID:   []string{"campaign id"},
			campaignName: []string{"campaign", "campaign name"},
			spend:        []string{"cost", "spend"},
			impressions:  []string{"impr.", "impr", "impressions"},
			clicks:       []string{"clicks", "interactions"},
			conversions:  []string{"conversions", "conv.", "all conv.", "installs"},
			revenue:      []string{"conv. value", "all conv. value", "total conv. value", "conversion value"},
			currency:     []string{"currency code", "currency"},
		},
	},
	{
		ID:         "meta_ads",
		Label:      "Meta Ads",
		ExportHint: "Ads Manager → Campaigns → Breakdown: By time → Day → Export table data (CSV).",
		aliases: adColumnAliases{
			date:         []string{"day", "reporting starts", "date start"},
			campaignID:   []string{"campaign id"},
			campaignName: []string{"campaign name", "campaign"},
			spend:        []string{"amount spent", "spend"},
			impressions:  []string{"impressions"},
			clicks:       []string{"link clicks", "clicks (all)", "clicks", "unique link clicks"},
			conversions:  []string{"results", "app installs", "mobile app installs", "purchases", "conversions", "leads"},
			revenue:      []string{"purchase conversion value", "purchases conversion value", "website purchases conversion value", "conversion value"},
			currency:     []string{"currency"},
		},
	},
	{
		ID:         "tiktok_ads",
		Label:      "TikTok Ads",
		ExportHint: "TikTok Ads Manager → Campaign → Custom report by day → Export (CSV, not XLSX).",
		aliases: adColumnAliases{
			date:         []string{"date", "day", "by day", "time"},
			campaignID:   []string{"campaign id"},
			campaignName: []string{"campaign name", "campaign"},
			spend:        []string{"cost", "total cost", "spend", "amount spent"},
			impressions:  []string{"impressions", "impression"},
			clicks:       []string{"clicks (destination)", "clicks", "click"},
			conversions:  []string{"conversions", "conversion", "app install", "app installs", "installs", "results", "total complete payment"},
			revenue:      []string{"total revenue", "conversion value", "total purchase value", "complete payment value"},
			currency:     []string{"currency"},
		},
	},
	{
		ID:         "x_ads",
		Label:      "X Ads",
		ExportHint: "X Ads → Campaigns → set the date range → Export → Campaign, daily (CSV).",
		aliases: adColumnAliases{
			date:         []string{"time period", "date", "day", "start date"},
			campaignID:   []string{"campaign id"},
			campaignName: []string{"campaign name", "campaign"},
			spend:        []string{"spend", "total spend", "amount spent", "cost"},
			impressions:  []string{"impressions"},
			clicks:       []string{"link clicks", "clicks", "url clicks"},
			conversions:  []string{"app installs", "installs", "conversions", "results", "purchases"},
			revenue:      []string{"conversion value", "revenue", "purchase value"},
			currency:     []string{"currency"},
		},
	},
	{
		ID:         "reddit_ads",
		Label:      "Reddit Ads",
		ExportHint: "Reddit Ads → Campaigns → Reporting → group by Day → Export (CSV).",
		aliases: adColumnAliases{
			date:         []string{"date", "day"},
			campaignID:   []string{"campaign id"},
			campaignName: []string{"campaign name", "campaign"},
			spend:        []string{"amount spent", "spend", "cost"},
			impressions:  []string{"impressions"},
			clicks:       []string{"clicks", "link clicks"},
			conversions:  []string{"conversions", "app installs", "purchases", "results"},
			revenue:      []string{"conversion value", "revenue"},
			currency:     []string{"currency"},
		},
	},
	{
		ID:         "linkedin_ads",
		Label:      "LinkedIn Ads",
		ExportHint: "Campaign Manager → Campaigns → Export → Campaign performance, daily (CSV).",
		aliases: adColumnAliases{
			date:         []string{"start date (in utc)", "start date", "date", "day"},
			campaignID:   []string{"campaign id"},
			campaignName: []string{"campaign name", "campaign"},
			spend:        []string{"total spent", "amount spent", "spend", "cost"},
			impressions:  []string{"impressions"},
			clicks:       []string{"clicks"},
			conversions:  []string{"conversions", "leads", "external website conversions"},
			revenue:      []string{"conversion value", "revenue"},
			currency:     []string{"currency"},
		},
	},
	{
		ID:         "other",
		Label:      "Other",
		ExportHint: "Any platform: rename the export's headers to date, campaign_id, campaign_name, spend, impressions, clicks, conversions, revenue, currency.",
		aliases:    adColumnAliases{},
	},
}

// Every registry platform (except the catch-all) is a signal provider: the
// bridge emits ads_<metric> ProductSignals with Source.Kind = platform, and a
// hand-written SignalExportDocument with that source kind normalizes the same
// way. Registering from the registry keeps the two tables from drifting.
func init() {
	for _, p := range adPlatforms {
		if p.ID == "other" {
			continue
		}
		for _, metric := range adsSignalMetrics {
			canonicalSignalMetricNames[p.ID+":"+metric] = "ads_" + metric
		}
	}
}

// AdPlatforms returns the registry in display order.
func AdPlatforms() []AdPlatform {
	out := make([]AdPlatform, len(adPlatforms))
	copy(out, adPlatforms)
	return out
}

// AdPlatformIDs lists the registry ids for usage strings.
func AdPlatformIDs() []string {
	ids := make([]string, 0, len(adPlatforms))
	for _, p := range adPlatforms {
		ids = append(ids, p.ID)
	}
	return ids
}

// LookupAdPlatform resolves a registry entry by id (normalized).
func LookupAdPlatform(id string) (AdPlatform, bool) {
	id = normalizeIDPart(id)
	for _, p := range adPlatforms {
		if p.ID == id {
			return p, true
		}
	}
	return AdPlatform{}, false
}

// AdRow is one campaign × day observation, as reported by the platform.
// Identity is (platform, product, account, campaign_id, date, currency);
// several report rows that share that identity (an export split by device or
// country) are summed at import so the ledger row is the campaign's day.
//
// Summing loses which contributions carried what, so the row also keeps the
// counts: a row folded from two contributions where only one reported revenue
// is NOT revenue-complete, and only the counts can still say so once the
// numbers are summed (review 2026-09-19).
type AdRow struct {
	SchemaVersion string  `json:"schema_version"`
	ID            string  `json:"id"`
	Platform      string  `json:"platform"`
	ProductID     string  `json:"product_id"`
	Account       string  `json:"account,omitempty"`
	CampaignID    string  `json:"campaign_id"`
	CampaignName  string  `json:"campaign_name,omitempty"`
	Date          string  `json:"date"`
	Currency      string  `json:"currency,omitempty"`
	Spend         float64 `json:"spend"`
	Impressions   float64 `json:"impressions"`
	Clicks        float64 `json:"clicks"`
	Conversions   float64 `json:"conversions"`
	Revenue       float64 `json:"revenue"`
	HasRevenue    bool    `json:"has_revenue"`
	// Contributions is how many report rows were summed into this row, and
	// RevenueContributions how many of those reported revenue. Additive and
	// omitempty: a row written before the counts existed reads as one
	// contribution, so an older ledger keeps its current meaning.
	Contributions        int    `json:"contributions,omitempty"`
	RevenueContributions int    `json:"revenue_contributions,omitempty"`
	SourceRef            string `json:"source_ref"`
	ImportID             string `json:"import_id"`
	ImportedAt           string `json:"imported_at"`
}

// AdsImport is the receipt of one report import. It is recorded even when
// every row was already known: that the operator re-imported is evidence too.
type AdsImport struct {
	SchemaVersion  string `json:"schema_version"`
	ID             string `json:"id"`
	Platform       string `json:"platform"`
	ProductID      string `json:"product_id"`
	Account        string `json:"account,omitempty"`
	SourceFile     string `json:"source_file"`
	SourceSHA256   string `json:"source_sha256"`
	RowsParsed     int    `json:"rows_parsed"`
	RowsSkipped    int    `json:"rows_skipped"`
	RowsNew        int    `json:"rows_new"`
	RowsUpdated    int    `json:"rows_updated"`
	RowsUnchanged  int    `json:"rows_unchanged"`
	DateStart      string `json:"date_start,omitempty"`
	DateEnd        string `json:"date_end,omitempty"`
	Currency       string `json:"currency,omitempty"`
	CampaignsSeen  int    `json:"campaigns_seen"`
	HasCampaignCol bool   `json:"has_campaign_column"`
	HasRevenueCol  bool   `json:"has_revenue_column"`
	ImportedAt     string `json:"imported_at"`
}

// AdsImportOptions configures ImportAdsReport.
type AdsImportOptions struct {
	Platform  string
	ProductID string
	// Account labels the ad account when one product runs on several
	// accounts of the same platform; it is part of row identity.
	Account string
	// Currency overrides the report's currency when the export carries none.
	Currency string
	Now      time.Time
}

// AdsImportReport is what the CLI prints after an import.
type AdsImportReport struct {
	Import   AdsImport `json:"import"`
	Warnings []string  `json:"warnings,omitempty"`
}

// ImportAdsReport reads one exported campaign report and appends its rows to
// the ads ledger. It never fetches, never rewrites a ledger line, and refuses
// a report it cannot parse rather than importing zeros.
func ImportAdsReport(homePath, path string, opts AdsImportOptions) (AdsImportReport, error) {
	platform, ok := LookupAdPlatform(opts.Platform)
	if !ok {
		return AdsImportReport{}, fmt.Errorf("ads import: unknown platform %q (known: %s)", opts.Platform, strings.Join(AdPlatformIDs(), ", "))
	}
	productID := strings.TrimSpace(opts.ProductID)
	if productID == "" {
		return AdsImportReport{}, fmt.Errorf("ads import: --product is required")
	}
	if strings.TrimSpace(homePath) == "" {
		return AdsImportReport{}, fmt.Errorf("ads import: home path required")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return AdsImportReport{}, err
	}
	parsed, err := parseAdsReport(b, platform, path)
	if err != nil {
		return AdsImportReport{}, err
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	account := strings.TrimSpace(opts.Account)
	importedAt := now.UTC().Format(time.RFC3339)
	// The receipt id must be unique per import event, not per report: the same
	// file re-imported twice within one second is two receipts, and the console
	// keys its import trail by this id.
	importID := stableID("adimp_", platform.ID, productID, account, sha, now.UTC().Format(time.RFC3339Nano))
	sourceRef := filepath.Base(path) + "@sha256:" + sha[:12]

	currencyOverride := strings.ToUpper(strings.TrimSpace(opts.Currency))
	byID := map[string]*AdRow{}
	order := []string{}
	for _, r := range parsed.rows {
		currency := strings.ToUpper(strings.TrimSpace(r.Currency))
		if currency == "" {
			currency = currencyOverride
		}
		if currency == "" {
			currency = parsed.currency
		}
		// Row identity INCLUDES currency. Two distinct currencies for the
		// same (platform, product, account, campaign, date) are NOT
		// collapsed into one row — folding them numerically and labelling
		// the row with the first-seen currency made a 100 USD + 100 EUR
		// spend total look like a single 200 USD figure (audit
		// 2026-09-16 finding 3). An empty currency falls back to a
		// shared sentinel so two un-currency-tagged rows still fold
		// (preserving the existing split-rows contract), but a
		// currency-tagged row NEVER folds with an un-currency-tagged
		// row — the distinction is the data.
		id := stableID("adrow_", platform.ID, productID, account, r.CampaignID, r.Date, currency)
		row, exists := byID[id]
		if !exists {
			row = &AdRow{
				SchemaVersion: AdsRowSchemaVersion,
				ID:            id,
				Platform:      platform.ID,
				ProductID:     productID,
				Account:       account,
				CampaignID:    r.CampaignID,
				CampaignName:  r.CampaignName,
				Date:          r.Date,
				Currency:      currency,
				SourceRef:     sourceRef,
				ImportID:      importID,
				ImportedAt:    importedAt,
			}
			byID[id] = row
			order = append(order, id)
		}
		row.Spend += r.Spend
		row.Impressions += r.Impressions
		row.Clicks += r.Clicks
		row.Conversions += r.Conversions
		row.Revenue += r.Revenue
		row.Contributions++
		if r.HasRevenue {
			row.HasRevenue = true
			row.RevenueContributions++
		}
		if row.CampaignName == "" {
			row.CampaignName = r.CampaignName
		}
	}

	existing, err := LoadAdRows(homePath)
	if err != nil {
		return AdsImportReport{}, err
	}
	latest := latestAdRowsByIdentity(existing)

	imp := AdsImport{
		SchemaVersion:  AdsImportSchemaVersion,
		ID:             importID,
		Platform:       platform.ID,
		ProductID:      productID,
		Account:        account,
		SourceFile:     filepath.Base(path),
		SourceSHA256:   sha,
		RowsParsed:     len(parsed.rows),
		RowsSkipped:    parsed.skipped,
		HasCampaignCol: parsed.hasCampaign,
		HasRevenueCol:  parsed.hasRevenueColumn,
		ImportedAt:     importedAt,
	}
	campaigns := map[string]struct{}{}
	currencies := map[string]struct{}{}
	unnamedCurrency := false
	toAppend := make([]AdRow, 0, len(order))
	for _, id := range order {
		row := byID[id]
		campaigns[row.CampaignID] = struct{}{}
		if row.Currency != "" {
			currencies[row.Currency] = struct{}{}
		} else if row.Spend != 0 || row.Revenue != 0 {
			unnamedCurrency = true
		}
		if imp.DateStart == "" || row.Date < imp.DateStart {
			imp.DateStart = row.Date
		}
		if imp.DateEnd == "" || row.Date > imp.DateEnd {
			imp.DateEnd = row.Date
		}
		// Look the previous observation up by its natural identity, not by
		// the stored id: a ledger written before the currency joined the
		// identity holds the same observation under an older id, and
		// keying on the id would file the re-import as a second live row.
		prev, seen := latest[adRowIdentity(*row)]
		switch {
		case !seen:
			imp.RowsNew++
			toAppend = append(toAppend, *row)
		case sameAdRowValues(prev, *row):
			imp.RowsUnchanged++
		default:
			imp.RowsUpdated++
			toAppend = append(toAppend, *row)
		}
	}
	imp.CampaignsSeen = len(campaigns)
	imp.Currency = singleOrMixedCurrency(currencies)
	if unnamedCurrency && imp.Currency != "" {
		// Part of the money named a currency and part named none, so the
		// receipt cannot claim the named one for the whole report.
		imp.Currency = adsCurrencyMixed
	}

	ledger := adsRowsLog(homePath)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		return AdsImportReport{}, err
	}
	for _, row := range toAppend {
		if err := appendJSONL(ledger, &row); err != nil {
			return AdsImportReport{}, err
		}
	}
	if err := appendJSONL(adsImportsLog(homePath), &imp); err != nil {
		return AdsImportReport{}, err
	}

	report := AdsImportReport{Import: imp}
	if !parsed.hasCampaign {
		report.Warnings = append(report.Warnings, "report has no campaign column; rows were recorded under one account-level campaign")
	}
	if imp.Currency == "" {
		report.Warnings = append(report.Warnings, "report carries no currency; pass --currency <code> so spend is comparable across platforms")
	}
	if imp.Currency == adsCurrencyMixed {
		report.Warnings = append(report.Warnings, "report mixes currencies; totals across it are flagged, not summed")
	}
	if parsed.skipped > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d row(s) without a parseable date were skipped (totals or summary lines)", parsed.skipped))
	}
	if !parsed.hasClicks {
		report.Warnings = append(report.Warnings, "report has no clicks/taps column; CTR and CPC read as undefined")
	}
	if !parsed.hasConversions {
		report.Warnings = append(report.Warnings, "report has no conversions/installs column; CPA reads as undefined")
	}
	return report, nil
}

func sameAdRowValues(a, b AdRow) bool {
	// The contribution counts are compared through the same fallback the
	// readers use, so an older row without them still reads as unchanged
	// against a re-import of the same single-contribution observation.
	aTotal, aRevenue := adRowContributions(a)
	bTotal, bRevenue := adRowContributions(b)
	return a.CampaignName == b.CampaignName &&
		a.Currency == b.Currency &&
		a.Spend == b.Spend &&
		a.Impressions == b.Impressions &&
		a.Clicks == b.Clicks &&
		a.Conversions == b.Conversions &&
		a.Revenue == b.Revenue &&
		a.HasRevenue == b.HasRevenue &&
		aTotal == bTotal &&
		aRevenue == bRevenue
}

// adRowIdentity is the observation a row records: the same tuple the row id
// hashes. Readers fold on it rather than on the stored id, so a ledger
// written before the currency became part of the identity upgrades in place
// — a re-import supersedes the old-id generation instead of standing beside
// it and doubling the spend (review 2026-09-19).
func adRowIdentity(r AdRow) string {
	return strings.Join([]string{r.Platform, r.ProductID, r.Account, r.CampaignID, r.Date, r.Currency}, "\x00")
}

// adRowContributions reports how many report rows were summed into this row
// and how many of them carried revenue. A row stored before the counts
// existed reads as a single contribution whose revenue completeness is its
// HasRevenue flag — exactly the meaning it had then.
func adRowContributions(r AdRow) (total, withRevenue int) {
	if r.Contributions <= 0 {
		if r.HasRevenue {
			return 1, 1
		}
		return 1, 0
	}
	withRevenue = r.RevenueContributions
	if withRevenue > r.Contributions {
		withRevenue = r.Contributions
	}
	if withRevenue < 0 {
		withRevenue = 0
	}
	return r.Contributions, withRevenue
}

func singleOrMixedCurrency(set map[string]struct{}) string {
	switch len(set) {
	case 0:
		return ""
	case 1:
		for c := range set {
			return c
		}
	}
	return adsCurrencyMixed
}

/* ---------- ledger ---------- */

func adsRowsLog(homePath string) string {
	return filepath.Join(homePath, "ledger", "product-ads.jsonl")
}

func adsImportsLog(homePath string) string {
	return filepath.Join(homePath, "ledger", "product-ads-imports.jsonl")
}

// LoadAdRows returns every ledger line in append order, history included.
func LoadAdRows(homePath string) ([]AdRow, error) {
	var out []AdRow
	err := readJSONL(adsRowsLog(homePath), func(line []byte) error {
		var r AdRow
		if err := json.Unmarshal(line, &r); err != nil || r.ID == "" {
			return nil
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// LoadAdsImports returns import receipts in append order.
func LoadAdsImports(homePath string) ([]AdsImport, error) {
	var out []AdsImport
	err := readJSONL(adsImportsLog(homePath), func(line []byte) error {
		var r AdsImport
		if err := json.Unmarshal(line, &r); err != nil || r.ID == "" {
			return nil
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// latestAdRowsByIdentity folds history to the newest observation per row
// identity. Append order is the tie-break, so a later import always wins.
// Keying on the identity rather than the stored id is what makes the ledger
// upgradeable: two generations of the same observation, written under
// different id schemes, collapse to the newer one instead of both counting.
func latestAdRowsByIdentity(rows []AdRow) map[string]AdRow {
	out := make(map[string]AdRow, len(rows))
	for _, r := range rows {
		out[adRowIdentity(r)] = r
	}
	return out
}

// LatestAdRows returns the folded ledger in a deterministic order.
func LatestAdRows(rows []AdRow) []AdRow {
	byIdentity := latestAdRowsByIdentity(rows)
	out := make([]AdRow, 0, len(byIdentity))
	for _, r := range byIdentity {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.ProductID != b.ProductID {
			return a.ProductID < b.ProductID
		}
		if a.CampaignID != b.CampaignID {
			return a.CampaignID < b.CampaignID
		}
		return a.Date < b.Date
	})
	return out
}

/* ---------- report parsing ---------- */

type adsParsedRow struct {
	CampaignID   string
	CampaignName string
	Date         string
	Currency     string
	Spend        float64
	Impressions  float64
	Clicks       float64
	Conversions  float64
	Revenue      float64
	// HasRevenue is per row: a report can carry a conversion-value column
	// whose cells are empty, and an empty cell is "not reported", not 0.
	HasRevenue bool
}

type adsParsedReport struct {
	rows        []adsParsedRow
	skipped     int
	currency    string
	hasCampaign bool
	// hasRevenueColumn: the header exists. hasRevenue: at least one row
	// carried a value. Only the latter makes ROAS defined.
	hasRevenueColumn bool
	hasRevenue       bool
	hasClicks        bool
	hasConversions   bool
}

type adsHeaderIndex struct {
	date, campaignID, campaignName, spend, impressions, clicks, conversions, revenue, currency int
	spendCurrency                                                                              string
}

// adsRequiredColumns names what a report cannot be imported without. A
// report missing any of them is refused rather than contributing zeros.
var adsRequiredColumns = []string{"date", "spend", "impressions"}

func parseAdsReport(b []byte, platform AdPlatform, source string) (adsParsedReport, error) {
	b = bytes.TrimPrefix(b, []byte("\ufeff"))
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if strings.TrimSpace(text) == "" {
		return adsParsedReport{}, fmt.Errorf("ads report %s is empty", source)
	}

	headerLine, delimiter, index, ok := findAdsHeader(lines, platform)
	if !ok {
		return adsParsedReport{}, fmt.Errorf("ads report %s: no header row with the required columns %s for platform %s; rename the export's headers to the canonical names (date, campaign_id, campaign_name, spend, impressions, clicks, conversions, revenue, currency) if the platform dialect is not recognised",
			source, strings.Join(adsRequiredColumns, ", "), platform.ID)
	}

	reader := csv.NewReader(strings.NewReader(strings.Join(lines[headerLine+1:], "\n")))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return adsParsedReport{}, fmt.Errorf("ads report %s: %w", source, err)
	}

	out := adsParsedReport{
		hasCampaign:      index.campaignID >= 0 || index.campaignName >= 0,
		hasRevenueColumn: index.revenue >= 0,
		hasClicks:        index.clicks >= 0,
		hasConversions:   index.conversions >= 0,
		currency:         index.spendCurrency,
	}
	cell := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	for n, rec := range records {
		if len(rec) == 0 || (len(rec) == 1 && strings.TrimSpace(rec[0]) == "") {
			continue
		}
		date, ok := parseAdDate(cell(rec, index.date))
		if !ok {
			out.skipped++
			continue
		}
		row := adsParsedRow{Date: date}
		row.CampaignID = cell(rec, index.campaignID)
		row.CampaignName = cell(rec, index.campaignName)
		if row.CampaignID == "" {
			row.CampaignID = row.CampaignName
		}
		if row.CampaignID == "" {
			row.CampaignID = "account"
		}
		if row.CampaignName == "" {
			row.CampaignName = row.CampaignID
		}
		var perr error
		if row.Spend, perr = parseAdNumber(cell(rec, index.spend)); perr != nil {
			return adsParsedReport{}, fmt.Errorf("ads report %s row %d: spend %q: %w", source, n+1, cell(rec, index.spend), perr)
		}
		if row.Impressions, perr = parseAdNumber(cell(rec, index.impressions)); perr != nil {
			return adsParsedReport{}, fmt.Errorf("ads report %s row %d: impressions %q: %w", source, n+1, cell(rec, index.impressions), perr)
		}
		if index.clicks >= 0 {
			if row.Clicks, perr = parseAdNumber(cell(rec, index.clicks)); perr != nil {
				return adsParsedReport{}, fmt.Errorf("ads report %s row %d: clicks %q: %w", source, n+1, cell(rec, index.clicks), perr)
			}
		}
		if index.conversions >= 0 {
			if row.Conversions, perr = parseAdNumber(cell(rec, index.conversions)); perr != nil {
				return adsParsedReport{}, fmt.Errorf("ads report %s row %d: conversions %q: %w", source, n+1, cell(rec, index.conversions), perr)
			}
		}
		if raw := cell(rec, index.revenue); index.revenue >= 0 && raw != "" {
			if row.Revenue, perr = parseAdNumber(raw); perr != nil {
				return adsParsedReport{}, fmt.Errorf("ads report %s row %d: revenue %q: %w", source, n+1, raw, perr)
			}
			row.HasRevenue = true
			out.hasRevenue = true
		}
		if index.currency >= 0 {
			row.Currency = strings.ToUpper(cell(rec, index.currency))
		}
		out.rows = append(out.rows, row)
	}
	if len(out.rows) == 0 {
		return adsParsedReport{}, fmt.Errorf("ads report %s: header found but no row carried a parseable date (%d skipped)", source, out.skipped)
	}
	return out, nil
}

// findAdsHeader scans the first lines for the row that names the required
// columns. Google Ads exports carry two title lines before the header, so the
// header is searched for rather than assumed to be line one.
func findAdsHeader(lines []string, platform AdPlatform) (int, rune, adsHeaderIndex, bool) {
	limit := len(lines)
	if limit > 25 {
		limit = 25
	}
	for i := 0; i < limit; i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, delimiter := range []rune{'\t', ',', ';'} {
			cells := splitAdsHeader(line, delimiter)
			if len(cells) < 2 {
				continue
			}
			index := matchAdsHeader(cells, platform)
			if index.date >= 0 && index.spend >= 0 && index.impressions >= 0 {
				return i, delimiter, index, true
			}
		}
	}
	return 0, ',', adsHeaderIndex{}, false
}

func splitAdsHeader(line string, delimiter rune) []string {
	r := csv.NewReader(strings.NewReader(line))
	r.Comma = delimiter
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	cells, err := r.Read()
	if err != nil {
		return strings.Split(line, string(delimiter))
	}
	return cells
}

func matchAdsHeader(cells []string, platform AdPlatform) adsHeaderIndex {
	index := adsHeaderIndex{date: -1, campaignID: -1, campaignName: -1, spend: -1, impressions: -1, clicks: -1, conversions: -1, revenue: -1, currency: -1}
	match := func(aliases, canonical []string, cellNorm, cellBase string) bool {
		for _, a := range aliases {
			if cellNorm == a || cellBase == a {
				return true
			}
		}
		for _, a := range canonical {
			if cellNorm == a || cellBase == a {
				return true
			}
		}
		return false
	}
	al := platform.aliases
	can := adsCanonicalAliases
	for i, raw := range cells {
		norm, base, paren := normalizeAdHeader(raw)
		switch {
		case index.date < 0 && match(al.date, can.date, norm, base):
			index.date = i
		case index.campaignID < 0 && match(al.campaignID, can.campaignID, norm, base):
			index.campaignID = i
		case index.campaignName < 0 && match(al.campaignName, can.campaignName, norm, base):
			index.campaignName = i
		case index.spend < 0 && match(al.spend, can.spend, norm, base):
			index.spend = i
			if len(paren) == 3 && isASCIILetters(paren) {
				index.spendCurrency = strings.ToUpper(paren)
			}
		case index.impressions < 0 && match(al.impressions, can.impressions, norm, base):
			index.impressions = i
		case index.clicks < 0 && match(al.clicks, can.clicks, norm, base):
			index.clicks = i
		case index.conversions < 0 && match(al.conversions, can.conversions, norm, base):
			index.conversions = i
		case index.revenue < 0 && match(al.revenue, can.revenue, norm, base):
			index.revenue = i
		case index.currency < 0 && match(al.currency, can.currency, norm, base):
			index.currency = i
		}
	}
	return index
}

// normalizeAdHeader lower-cases and collapses a header cell, returning the
// full normalized text, the text before any parenthesised qualifier, and the
// qualifier itself ("amount spent (usd)" → "amount spent (usd)", "amount
// spent", "usd").
func normalizeAdHeader(raw string) (norm, base, paren string) {
	s := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff")))
	s = strings.Join(strings.Fields(s), " ")
	norm = s
	base = s
	if open := strings.Index(s, "("); open >= 0 {
		base = strings.TrimSpace(s[:open])
		if closeIdx := strings.Index(s[open:], ")"); closeIdx > 0 {
			paren = strings.TrimSpace(s[open+1 : open+closeIdx])
		}
	}
	return norm, base, paren
}

func isASCIILetters(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}

var adDateLayouts = []string{
	"2006-01-02",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006/01/02",
	"01/02/2006",
	"1/2/2006",
	"02.01.2006",
	"2.1.2006",
	"Jan 2, 2006",
	"January 2, 2006",
	"2 Jan 2006",
	"02 Jan 2006",
	"20060102",
}

// parseAdDate normalizes the many date spellings ad exports use to
// YYYY-MM-DD. Slash dates are read as US month/day (every listed platform's
// export defaults to that); dotted dates are read as day.month. A cell that
// is not a date (a "Total" line) reports !ok and the caller skips the row.
func parseAdDate(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if i := strings.Index(s, " - "); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	for _, layout := range adDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	return "", false
}

// parseAdNumber reads a report number that may carry a currency symbol,
// thousands separators, a percent sign, or a locale decimal comma.
func parseAdNumber(raw string) (float64, error) {
	s := strings.TrimSpace(raw)
	switch s {
	case "", "-", "--", "—", "n/a", "N/A":
		return 0, nil
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) || r == '.' || r == ',' || r == '-' {
			b.WriteRune(r)
		}
	}
	s = b.String()
	if s == "" || s == "-" {
		return 0, fmt.Errorf("not a number")
	}
	lastComma := strings.LastIndex(s, ",")
	lastDot := strings.LastIndex(s, ".")
	switch {
	case lastComma >= 0 && lastDot >= 0:
		if lastComma > lastDot {
			s = strings.ReplaceAll(s, ".", "")
			s = strings.Replace(s, ",", ".", 1)
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	case lastComma >= 0:
		// "1,234" / "1,234,567" are thousands groups; "12,5" is a decimal comma.
		if strings.Count(s, ",") > 1 || len(s)-lastComma-1 == 3 {
			s = strings.ReplaceAll(s, ",", "")
		} else {
			s = strings.Replace(s, ",", ".", 1)
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("not a number")
	}
	return v, nil
}
