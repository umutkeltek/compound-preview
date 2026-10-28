package product

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"compound/internal/mem"
)

// RunAdsCLI is the `compound product ads ...` verb family. It lives here
// rather than in cmd/compound because that package sits on its file-count
// ratchet; cmd/compound only dispatches. Returns the process exit code and
// writes to the supplied streams so it is testable without os.Exit.
//
//	compound product ads [--json] [--window 7d|30d|90d] [--product <id>] [--platform <id>] [--as-of YYYY-MM-DD]
//	compound product ads import --platform <id> --product <id> --report <export.csv> [--account <label>] [--currency <code>] [--json]
//	compound product ads signal --platform <id> --product <id> --before <range> --after <range> [--metric spend,conversions,cpa] [--change-event <text>] [--account <label>]
//	compound product ads platforms [--json]
func RunAdsCLI(args []string, homePath string, store *mem.Store, stdout, stderr io.Writer, now time.Time) int {
	if now.IsZero() {
		now = time.Now()
	}
	if len(args) > 0 {
		switch args[0] {
		case "import":
			return runAdsImport(args[1:], homePath, stdout, stderr, now)
		case "signal":
			return runAdsSignal(args[1:], homePath, store, stdout, stderr, now)
		case "platforms":
			return runAdsPlatforms(args[1:], stdout, stderr)
		case "help", "--help", "-h":
			fmt.Fprintln(stdout, AdsUsage())
			return 0
		}
	}
	return runAdsProjection(args, homePath, stdout, stderr, now)
}

// AdsUsage is the verb family's usage line, appended to productUsage().
func AdsUsage() string {
	return "ads [--json] [--window <days|7d|30d|90d>] [--product <id>] [--platform <id>] [--as-of <YYYY-MM-DD>] | " +
		"ads import --platform <" + strings.Join(AdPlatformIDs(), "|") + "> --product <id> --report <export.csv> [--account <label>] [--currency <code>] [--json] | " +
		"ads signal --platform <id> --product <id> --before <range> --after <range> [--metric " + strings.Join(adsSignalMetrics, ",") + "] [--change-event <text>] [--account <label>] | " +
		"ads platforms [--json]"
}

func runAdsProjection(args []string, homePath string, stdout, stderr io.Writer, now time.Time) int {
	opts := AdsProjectionOptions{AsOf: now}
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
			continue
		}
		if i+1 >= len(args) {
			fmt.Fprintln(stderr, "usage: compound product "+AdsUsage())
			return 2
		}
		value := args[i+1]
		switch args[i] {
		case "--window":
			days, err := parseAdsWindow(value)
			if err != nil {
				fmt.Fprintln(stderr, "product ads:", err)
				return 2
			}
			opts.WindowDays = days
		case "--product":
			opts.ProductID = value
		case "--platform":
			opts.Platform = value
		case "--as-of":
			t, err := time.Parse("2006-01-02", strings.TrimSpace(value))
			if err != nil {
				fmt.Fprintln(stderr, "product ads: --as-of must be YYYY-MM-DD")
				return 2
			}
			opts.AsOf = t
		case "--recent":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				fmt.Fprintln(stderr, "product ads: --recent must be a positive integer")
				return 2
			}
			opts.RecentImports = n
		default:
			fmt.Fprintln(stderr, "usage: compound product "+AdsUsage())
			return 2
		}
		i++
	}
	proj, err := LoadAdsProjection(homePath, opts)
	if err != nil {
		fmt.Fprintln(stderr, "product ads:", err)
		return 1
	}
	if asJSON {
		return writeAdsJSON(stdout, stderr, proj)
	}
	fmt.Fprintf(stdout, "# product ads (%s → %s, %d days)\n\n", proj.Window.Start, proj.Window.End, proj.Window.Days)
	fmt.Fprintf(stdout, "- status: %s\n", proj.Status)
	fmt.Fprintf(stdout, "- authority: %s\n", proj.AuthorityRef)
	fmt.Fprintf(stdout, "- rows: %d (%d in window)\n", proj.Counts.Rows, proj.Counts.RowsInWindow)
	fmt.Fprintf(stdout, "- spend: %s\n", adsMoney(proj.Totals))
	fmt.Fprintf(stdout, "- impressions: %.0f · clicks: %.0f · conversions: %.0f\n", proj.Totals.Impressions, proj.Totals.Clicks, proj.Totals.Conversions)
	// A withheld ratio must read as "—", never as "0.00": a rendered zero is
	// a number the reducer refused to state. The "defined" tests mirror the
	// reducer's own guards rather than restating them.
	oneCurrency := adsSpendIsOneCurrency(proj.Totals)
	fmt.Fprintf(stdout, "- ctr: %s · cpc: %s · cpa: %s · roas: %s\n", adsRatio(proj.Totals.CTR, proj.Totals.Impressions > 0, true), adsRatio(proj.Totals.CPC, oneCurrency && proj.Totals.Clicks > 0, false), adsRatio(proj.Totals.CPA, oneCurrency && proj.Totals.Conversions > 0, false), adsRatio(proj.Totals.ROAS, adsROASIsComplete(proj.Totals), false))
	fmt.Fprintf(stdout, "- platforms: %d connected, %d stale, %d not connected\n", proj.Counts.PlatformsConnected, proj.Counts.PlatformsStale, proj.Counts.PlatformsNotConnected)
	for _, p := range proj.Platforms {
		fmt.Fprintf(stdout, "- platform %s: %s", p.Platform, p.Status)
		if p.Status != "not_connected" {
			fmt.Fprintf(stdout, " · spend %s · conv %.0f · last %s", adsMoney(p.Totals), p.Totals.Conversions, p.LastDate)
		}
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "  next: %s\n", p.NextMove.Text)
		fmt.Fprintf(stdout, "  cmd: %s\n", p.NextMove.Command)
	}
	for _, c := range proj.Campaigns {
		fmt.Fprintf(stdout, "- campaign %s/%s %q: %s · spend %s · conv %.0f · %s..%s\n", c.Platform, c.ProductID, c.CampaignName, c.Status, adsMoney(c.Totals), c.Totals.Conversions, c.FirstDate, c.LastDate)
	}
	for _, g := range proj.Gaps {
		fmt.Fprintf(stdout, "- gap %s: %s\n", g.Kind, g.Text)
	}
	return 0
}

func runAdsImport(args []string, homePath string, stdout, stderr io.Writer, now time.Time) int {
	opts := AdsImportOptions{Now: now}
	report := ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--json" {
			asJSON = true
			continue
		}
		if i+1 >= len(args) {
			fmt.Fprintln(stderr, "usage: compound product "+AdsUsage())
			return 2
		}
		value := args[i+1]
		switch args[i] {
		case "--platform":
			opts.Platform = value
		case "--product":
			opts.ProductID = value
		case "--report":
			report = value
		case "--account":
			opts.Account = value
		case "--currency":
			opts.Currency = value
		default:
			fmt.Fprintln(stderr, "usage: compound product "+AdsUsage())
			return 2
		}
		i++
	}
	if strings.TrimSpace(report) == "" || strings.TrimSpace(opts.Platform) == "" || strings.TrimSpace(opts.ProductID) == "" {
		fmt.Fprintln(stderr, "product ads import: --platform, --product and --report are required")
		return 2
	}
	out, err := ImportAdsReport(homePath, report, opts)
	if err != nil {
		fmt.Fprintln(stderr, "product ads import:", err)
		return 1
	}
	if asJSON {
		return writeAdsJSON(stdout, stderr, out)
	}
	imp := out.Import
	fmt.Fprintf(stdout, "# product ads import %s\n\n", imp.Platform)
	fmt.Fprintf(stdout, "- status: ok\n")
	fmt.Fprintf(stdout, "- import: %s\n", imp.ID)
	fmt.Fprintf(stdout, "- product: %s\n", imp.ProductID)
	fmt.Fprintf(stdout, "- source: %s (sha256:%s)\n", imp.SourceFile, imp.SourceSHA256[:12])
	fmt.Fprintf(stdout, "- rows: %d parsed, %d new, %d updated, %d unchanged, %d skipped\n", imp.RowsParsed, imp.RowsNew, imp.RowsUpdated, imp.RowsUnchanged, imp.RowsSkipped)
	fmt.Fprintf(stdout, "- campaigns: %d · dates: %s..%s · currency: %s\n", imp.CampaignsSeen, imp.DateStart, imp.DateEnd, orDash(imp.Currency))
	for _, w := range out.Warnings {
		fmt.Fprintf(stdout, "- warning: %s\n", w)
	}
	fmt.Fprintf(stdout, "- next: compound product ads --platform %s --product %s\n", imp.Platform, imp.ProductID)
	return 0
}

func runAdsSignal(args []string, homePath string, store *mem.Store, stdout, stderr io.Writer, now time.Time) int {
	opts := AdsSignalOptions{}
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			fmt.Fprintln(stderr, "usage: compound product "+AdsUsage())
			return 2
		}
		value := args[i+1]
		switch args[i] {
		case "--platform":
			opts.Platform = value
		case "--product":
			opts.ProductID = value
		case "--account":
			opts.Account = value
		case "--before":
			opts.Before = value
		case "--after":
			opts.After = value
		case "--metric":
			opts.Metric = value
		case "--change-event":
			opts.ChangeEvent = value
		default:
			fmt.Fprintln(stderr, "usage: compound product "+AdsUsage())
			return 2
		}
		i++
	}
	if store == nil {
		fmt.Fprintln(stderr, "product ads signal: memory store required")
		return 1
	}
	docs, err := BuildAdsSignals(homePath, opts)
	if err != nil {
		fmt.Fprintln(stderr, "product ads signal:", err)
		return 1
	}
	sourcePath := "product-ads.jsonl"
	fmt.Fprintf(stdout, "# product signals %s\n\n", docs[0].Source.Kind)
	fmt.Fprintf(stdout, "- status: ok\n")
	fmt.Fprintf(stdout, "- product: %s\n", docs[0].Signal.ProductID)
	fmt.Fprintf(stdout, "- source: %s\n", sourcePath)
	fmt.Fprintf(stdout, "- signals: %d\n", len(docs))
	for _, doc := range docs {
		report, err := ApplySignal(homePath, store, doc, sourcePath, now)
		if err != nil {
			fmt.Fprintln(stderr, "product ads signal:", err)
			return 1
		}
		fmt.Fprintf(stdout, "- signal: %s %s %.4g -> %.4g", report.SignalIDs[0], doc.Signal.MetricName, doc.Signal.Before, doc.Signal.After)
		if len(report.TransitionIDs) > 0 {
			fmt.Fprintf(stdout, " transition:%s", report.TransitionIDs[0])
		}
		fmt.Fprintln(stdout)
	}
	return 0
}

func runAdsPlatforms(args []string, stdout, stderr io.Writer) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) > 0 && !asJSON {
		fmt.Fprintln(stderr, "usage: compound product ads platforms [--json]")
		return 2
	}
	platforms := AdPlatforms()
	if asJSON {
		type view struct {
			ID         string   `json:"id"`
			Label      string   `json:"label"`
			ExportHint string   `json:"export_hint"`
			Columns    []string `json:"columns"`
		}
		out := struct {
			SchemaVersion string   `json:"schema_version"`
			Platforms     []view   `json:"platforms"`
			Canonical     []string `json:"canonical_columns"`
		}{SchemaVersion: "compound.product_ads_platforms.v0.1", Canonical: []string{"date", "campaign_id", "campaign_name", "spend", "impressions", "clicks", "conversions", "revenue", "currency"}}
		for _, p := range platforms {
			out.Platforms = append(out.Platforms, view{ID: p.ID, Label: p.Label, ExportHint: p.ExportHint, Columns: adsUnderstoodColumns(p)})
		}
		return writeAdsJSON(stdout, stderr, out)
	}
	fmt.Fprintf(stdout, "# product ads platforms\n\n")
	for _, p := range platforms {
		fmt.Fprintf(stdout, "- %s: %s\n", p.ID, p.Label)
		fmt.Fprintf(stdout, "  export: %s\n", p.ExportHint)
		fmt.Fprintf(stdout, "  columns: %s\n", strings.Join(adsUnderstoodColumns(p), " · "))
	}
	return 0
}

func adsUnderstoodColumns(p AdPlatform) []string {
	pair := func(name string, aliases []string) string {
		all := append([]string{}, aliases...)
		if len(all) == 0 {
			return name
		}
		return name + " ← " + strings.Join(all, " | ")
	}
	a := p.aliases
	return []string{
		pair("date", a.date), pair("campaign_id", a.campaignID), pair("campaign_name", a.campaignName),
		pair("spend", a.spend), pair("impressions", a.impressions), pair("clicks", a.clicks),
		pair("conversions", a.conversions), pair("revenue", a.revenue), pair("currency", a.currency),
	}
}

func parseAdsWindow(raw string) (int, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimSuffix(s, "d")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 366 {
		return 0, fmt.Errorf("--window must be a day count between 1 and 366 (e.g. 7d, 30d, 90d)")
	}
	return n, nil
}

func writeAdsJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "product ads:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}

func adsMoney(t AdsTotals) string {
	if t.MixedCurrency {
		parts := make([]string, 0, len(t.SpendByCurrency))
		for _, c := range adsSortedKeys(adsCurrencySet(t.SpendByCurrency)) {
			parts = append(parts, fmt.Sprintf("%.2f %s", t.SpendByCurrency[c], c))
		}
		return "mixed (" + strings.Join(parts, ", ") + ")"
	}
	if t.Currency == "" {
		return fmt.Sprintf("%.2f", t.Spend)
	}
	return fmt.Sprintf("%.2f %s", t.Spend, t.Currency)
}

func adsRatio(v float64, defined bool, percent bool) string {
	if !defined {
		return "—"
	}
	if percent {
		return fmt.Sprintf("%.2f%%", v*100)
	}
	return fmt.Sprintf("%.2f", v)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func adsCurrencySet(m map[string]float64) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}
