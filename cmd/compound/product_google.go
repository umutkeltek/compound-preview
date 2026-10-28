package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/mem"
	"compound/internal/product"
)

func tryProductGoogleSignal(args []string, homePath string, store *mem.Store) bool {
	if !hasProductGoogleSignalFlag(args) {
		return false
	}
	docs, sourcePath, err := loadProductGoogleSignalArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "product signal:", err)
		os.Exit(1)
	}
	now := time.Now()
	var reports []product.Report
	for _, doc := range docs {
		report, err := product.ApplySignal(homePath, store, doc, sourcePath, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product signal:", err)
			os.Exit(1)
		}
		reports = append(reports, report)
	}
	provider := ""
	productID := ""
	if len(docs) > 0 {
		provider = docs[0].Source.Kind
		productID = docs[0].Signal.ProductID
	}
	fmt.Printf("# product signals %s\n\n", provider)
	fmt.Printf("- status: ok\n")
	fmt.Printf("- product: %s\n", productID)
	fmt.Printf("- source: %s\n", sourcePath)
	fmt.Printf("- signals: %d\n", len(reports))
	for i, report := range reports {
		fmt.Printf("- signal: %s", report.SignalIDs[0])
		if i < len(docs) {
			fmt.Printf(" %s", docs[i].Signal.MetricName)
		}
		if len(report.TransitionIDs) > 0 {
			fmt.Printf(" transition:%s", report.TransitionIDs[0])
		}
		fmt.Println()
	}
	return true
}

func hasProductGoogleSignalFlag(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--gsc-report", "--ga4-report", "--adsense-report":
			return true
		}
	}
	return false
}

func loadProductGoogleSignalArgs(args []string) ([]product.SignalDocument, string, error) {
	var gscReport string
	var ga4Report string
	var adsenseReport string
	gscOpts := product.GSCReportSignalOptions{}
	ga4Opts := product.GA4ReportSignalOptions{}
	adsenseOpts := product.AdSenseReportSignalOptions{}
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return nil, "", fmt.Errorf("%s", productUsage())
		}
		value := args[i+1]
		switch args[i] {
		case "--gsc-report":
			gscReport = value
		case "--ga4-report":
			ga4Report = value
		case "--adsense-report":
			adsenseReport = value
		case "--product":
			gscOpts.ProductID = value
			ga4Opts.ProductID = value
			adsenseOpts.ProductID = value
		case "--site":
			gscOpts.Site = value
		case "--property":
			ga4Opts.Property = value
		case "--account":
			adsenseOpts.Account = value
		case "--metric":
			gscOpts.Metric = value
			ga4Opts.Metric = value
			adsenseOpts.Metric = value
		default:
			return nil, "", fmt.Errorf("%s", productUsage())
		}
		i++
	}
	if selectedProductGoogleReports(gscReport, ga4Report, adsenseReport) != 1 {
		return nil, "", fmt.Errorf("choose one product signal source")
	}
	switch {
	case gscReport != "":
		if strings.TrimSpace(gscOpts.ProductID) == "" || strings.TrimSpace(gscOpts.Site) == "" {
			return nil, "", fmt.Errorf("%s", productUsage())
		}
		docs, err := product.LoadGSCReportSignals(gscReport, gscOpts)
		return docs, gscReport, err
	case ga4Report != "":
		if strings.TrimSpace(ga4Opts.ProductID) == "" || strings.TrimSpace(ga4Opts.Property) == "" {
			return nil, "", fmt.Errorf("%s", productUsage())
		}
		docs, err := product.LoadGA4ReportSignals(ga4Report, ga4Opts)
		return docs, ga4Report, err
	default:
		if strings.TrimSpace(adsenseOpts.ProductID) == "" || strings.TrimSpace(adsenseOpts.Account) == "" {
			return nil, "", fmt.Errorf("%s", productUsage())
		}
		docs, err := product.LoadAdSenseReportSignals(adsenseReport, adsenseOpts)
		return docs, adsenseReport, err
	}
}

func selectedProductGoogleReports(paths ...string) int {
	count := 0
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			count++
		}
	}
	return count
}
