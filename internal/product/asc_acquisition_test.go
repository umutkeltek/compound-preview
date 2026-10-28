package product

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ascAcquisitionFixture = "../../fixtures/product/asc-acquisition.tsv"

func ascAcquisitionTestOptions(metric string) ASCAcquisitionSignalOptions {
	return ASCAcquisitionSignalOptions{
		ProductID:    "product:example",
		AppID:        "0000000042",
		Metric:       metric,
		BeforeWindow: "2026-01-01/2026-01-02",
		AfterWindow:  "2026-02-01/2026-02-01",
		ChangeEvent:  "Example release shipped.",
	}
}

func loadASCAcquisitionValues(t *testing.T, metric string) (float64, float64) {
	t.Helper()
	doc, err := LoadASCAcquisitionSignal(ascAcquisitionFixture, ascAcquisitionTestOptions(metric))
	if err != nil {
		t.Fatalf("metric %s: %v", metric, err)
	}
	return doc.Signal.Before, doc.Signal.After
}

func assertClose(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

// Redownloads, auto-updates and manual updates describe installs that already
// happened. If they were summed as acquisition the before window would read
// 169 instead of 19, and every release would look like a growth spike.
func TestASCAcquisitionCountsOnlyFirstTimeDownloads(t *testing.T) {
	before, after := loadASCAcquisitionValues(t, "first_time_downloads")
	assertClose(t, "before first_time_downloads", before, 19)
	assertClose(t, "after first_time_downloads", after, 20)
}

// The paid and organic rows in the fixture share the same Source Type
// ("App Store search") and differ only by Campaign. A split on Source Type
// would return 0 or 19 here; only reading Campaign returns 5.
func TestASCAcquisitionSplitsPaidOnCampaignNotSourceType(t *testing.T) {
	paidBefore, paidAfter := loadASCAcquisitionValues(t, "paid_first_time_downloads")
	assertClose(t, "before paid", paidBefore, 5)
	assertClose(t, "after paid", paidAfter, 12)

	organicBefore, organicAfter := loadASCAcquisitionValues(t, "organic_first_time_downloads")
	assertClose(t, "before organic", organicBefore, 14)
	assertClose(t, "after organic", organicAfter, 8)

	// Paid and organic must partition the total exactly; a row counted in
	// neither or both would break this without failing the tests above.
	totalBefore, totalAfter := loadASCAcquisitionValues(t, "first_time_downloads")
	assertClose(t, "before partition", paidBefore+organicBefore, totalBefore)
	assertClose(t, "after partition", paidAfter+organicAfter, totalAfter)
}

func TestASCAcquisitionPaidShareIsARatioWithinTheWindow(t *testing.T) {
	before, after := loadASCAcquisitionValues(t, "paid_download_share")
	assertClose(t, "before paid share", before, 5.0/19.0)
	assertClose(t, "after paid share", after, 0.6)
}

// The fixture's second app has 999 campaign-bearing first-time downloads
// inside the before window. If matching ever fell back to App Name or leaked
// across identifiers, the totals above would move.
func TestASCAcquisitionMatchesAppleIdentifierOnly(t *testing.T) {
	doc, err := LoadASCAcquisitionSignal(ascAcquisitionFixture, ascAcquisitionTestOptions("first_time_downloads"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Signal.Before >= 999 {
		t.Fatalf("before value %v absorbed another app's rows", doc.Signal.Before)
	}

	other := ascAcquisitionTestOptions("first_time_downloads")
	other.AppID = "0000000043"
	other.BeforeWindow = "2026-01-02/2026-01-02"
	other.AfterWindow = "2026-01-02/2026-01-02"
	otherDoc, err := LoadASCAcquisitionSignal(ascAcquisitionFixture, other)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, "other app first_time_downloads", otherDoc.Signal.Before, 999)
}

// "No qualifying rows" and "zero downloads" are different facts. Collapsing
// them would let a truncated export read as a collapse in acquisition.
func TestASCAcquisitionEmptyWindowIsAnErrorNotZero(t *testing.T) {
	opts := ascAcquisitionTestOptions("first_time_downloads")
	opts.AfterWindow = "2026-03-01/2026-03-31"
	_, err := LoadASCAcquisitionSignal(ascAcquisitionFixture, opts)
	if err == nil {
		t.Fatal("expected an error for a window with no first-time download rows")
	}
	if !strings.Contains(err.Error(), "no first-time download rows") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An export without a Campaign column cannot answer paid/organic questions.
// Reporting everything as organic would be the silent-wrong-answer failure.
func TestASCAcquisitionPaidMetricsFailClosedWithoutCampaignColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-campaign.tsv")
	body := strings.Join([]string{
		"Date\tApp Apple Identifier\tDownload Type\tSource Type\tCounts",
		"2026-01-01\t0000000042\tFirst-time download\tApp Store search\t10",
	}, "\n")
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := ascAcquisitionTestOptions("paid_first_time_downloads")
	opts.BeforeWindow = "2026-01-01/2026-01-01"
	opts.AfterWindow = "2026-01-01/2026-01-01"
	if _, err := LoadASCAcquisitionSignal(path, opts); err == nil {
		t.Fatal("expected paid metric to fail closed without a Campaign column")
	}

	// The total metric remains answerable from the same reduced export.
	opts.Metric = "first_time_downloads"
	doc, err := LoadASCAcquisitionSignal(path, opts)
	if err != nil {
		t.Fatalf("total metric should still work: %v", err)
	}
	assertClose(t, "reduced export total", doc.Signal.Before, 10)
}

func TestASCAcquisitionRejectsMalformedReports(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing required column",
			body: "Date\tApp Apple Identifier\tCounts\n2026-01-01\t0000000042\t10",
			want: "missing required column",
		},
		{
			name: "empty counts",
			body: "Date\tApp Apple Identifier\tDownload Type\tCounts\n2026-01-01\t0000000042\tFirst-time download\t",
			want: "empty Counts",
		},
		{
			name: "non numeric counts",
			body: "Date\tApp Apple Identifier\tDownload Type\tCounts\n2026-01-01\t0000000042\tFirst-time download\tmany",
			want: "Counts",
		},
		{
			name: "header only",
			body: "Date\tApp Apple Identifier\tDownload Type\tCounts",
			want: "no data rows",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".tsv")
			if err := os.WriteFile(path, []byte(tc.body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			opts := ascAcquisitionTestOptions("first_time_downloads")
			opts.BeforeWindow = "2026-01-01/2026-01-01"
			opts.AfterWindow = "2026-01-01/2026-01-01"
			_, err := LoadASCAcquisitionSignal(path, opts)
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestASCAcquisitionSignalCarriesAttributionConfounders(t *testing.T) {
	doc, err := LoadASCAcquisitionSignal(ascAcquisitionFixture, ascAcquisitionTestOptions("paid_download_share"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(doc.Signal.Confounders, " | ")
	for _, want := range []string{"first-time downloads", "campaign column", "never spend"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("confounders %q missing %q", joined, want)
		}
	}
	if doc.Signal.ID == "" || !strings.HasPrefix(doc.Signal.ID, "sig_") {
		t.Fatalf("unexpected signal id %q", doc.Signal.ID)
	}
}

func TestASCAcquisitionRejectsUnsupportedMetric(t *testing.T) {
	_, err := LoadASCAcquisitionSignal(ascAcquisitionFixture, ascAcquisitionTestOptions("revenue"))
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported metric error, got %v", err)
	}
}
