// product_cli.go owns the `compound product ...` verb dispatcher
// (cmdProduct) and the product-specific argument loaders
// (loadProductSignalArg, productUsage). Extracted from cmd/compound/main.go
// so main.go holds only the cross-cutting dispatch table + non-product
// verbs. The product runtime (product.Bundle import, ASC/GA4/GSC/AdSense
// signal sources, loops, canonical-metrics, change-stream, hypothesis/
// observation-verdict/review/action/source-issue/outbound-dry-run/release plan and
// review seams, timeline, doctor) lives in compound/internal/product and is
// unchanged; this file is only the CLI surface that drives it.
//
// Composition with the other CLI verb files:
//   - memory_cli.go                 (cmdMemory)
//   - memory_engine_config.go       (engine config subsystem)
//   - distiller_cli.go              (cmdDistiller)
//   - decisions_cli.go              (cmdDecisions)
//   - policy_cli.go                 (cmdPolicy)
//   - authority_cli.go              (cmdAuthority)
//   - this file                     (cmdProduct — sixth verb extracted)
//
// Subcommand surface (all in cmdProduct's switch):
//   - authority                     (--json | text report)
//   - loops                         (delegates to cmdProductLoops)
//   - import                        (<product.yaml>|--project-contract [--release])
//   - where                         (<product|path>|--all [--json]; delegates to product.RunWhereCLI)
//   - essence | render-marketing    (<product> [--json] | [--out <path>|-]; delegates to product.RunEssenceCLI)
//   - signal                        (<signal.yaml> [--dry-run] (platform_change)|--asc-*|--gsc|--ga4|--adsense)
//   - canonical-metrics             (<json> --product <id>)
//   - signal-doctor                 (<signal.yaml> [--record])
//   - change-stream                 (<candidates.md>)
//   - hypothesis-plan               (--candidate <id> [--target <file>])
//   - observation-verdict-plan      (--verdict <panel-verdict.json> [--target <file>])
//   - observation-verdict-review    (<review.yaml>)
//   - review-plan                   (--hypothesis <id> --signal <id> [--target <file>])
//   - action-plan                   (--product <id> --card <id> [--target <file>])
//   - action-work-plan              (--reviewed <reviewed.json> --actor <actor> [--json])
//   - source-issue-plan             (--product <id> --issue <id> [--target <file>])
//   - outbound-dry-run-plan         (--reviewed-receipt <reviewed.json> [--target <file>])
//   - release-plan                  (--product/--change/--candidate/--rollback/--metric)
//   - release-work-plan             (--reviewed <reviewed.json> --actor <actor> [--json])
//   - release-observe               (--reviewed/--execution/--signal [--json])
//   - review                        (<review.yaml> --reviewed-by <actor>)
//   - action-review                 (<review.yaml>)
//   - source-issue-review           (<review.yaml>)
//   - outbound-dry-run-review       (<review.yaml> [--writeback])
//   - release-review                (<review.yaml>)
//   - timeline                      (<product_id>)
//   - doctor                        (<product_id> [--writeback])
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/product"
	"compound/internal/work"
)

func productUsage() string {
	return "usage: compound product authority [--json] | import <product.yaml>|--project-contract <project.yaml> [--release <release.yaml>] | signal <signal.yaml> [--dry-run]|--asc-datapoints <json> --product <id> --app-id <asc_id> [--metric <metric>]|--asc-sales-series <jsonl> --product <id> --app-id <asc_id> --before <range> --after <range> --change-event <text> [--metric <metric>]|--asc-funnel-series <jsonl> --product <id> --app-id <asc_id> --before <range> --after <range> --change-event <text> [--metric <metric>]|--asc-acquisition <tsv> --product <id> --app-id <asc_id> --before <range> --after <range> --change-event <text> [--metric first_time_downloads|paid_first_time_downloads|organic_first_time_downloads|paid_download_share]|--gsc-report <json> --product <id> --site <site> [--metric <metric>]|--ga4-report <json> --product <id> --property <id> [--metric <metric>]|--adsense-report <json> --product <id> --account <id> [--metric <metric>]|--posthog-report <json> --product <id> --project <id> [--metric <metric>] | loops [--product <id>] [--json] [--recent N] [--stale-days D] | " + product.AdsUsage() + " | " + product.WhereUsage() + " | " + product.EssenceUsage() + " | canonical-metrics <json> --product <id> | change-stream <candidates.md> | content-draft record --product <id> --platform <" + strings.Join(product.ContentDraftPlatforms(), "|") + "> [--text <body>] [--voice-pack <ref>] [--evidence <ref,ref>] [--generate <generator> --generate-prompt <text> [--generate-param k=v]... [--generate-alt <text>]] (--ttl <dur>|--expires <rfc3339>) | content-draft list [--product <id>] [--json] | content-draft park|operationalize|reject <draft_id> | content-queue plan [--json]|content-queue apply --review <review.yaml> [--json] | hypothesis-plan --candidate <candidate_id> [--target <file>] | hypothesis-approve <candidate_id> --expected-effect <sentence> [--metrics <m1,m2>] [--window <span>] [--reviewed-by <actor>] | observation-verdict-plan --verdict <panel-verdict.json> [--target <file>] | observation-verdict-review <review.yaml> | review-plan --hypothesis <hypothesis_id> --signal <signal_id> [--target <file>] | action-plan --product <product_id> --card <card_id> [--target <file>] | action-work-plan --reviewed <reviewed.json> --actor <actor> [--json] | source-issue-plan --product <product_id> --issue <issue_id> [--target <file>] | outbound-dry-run-plan --reviewed-receipt <reviewed.json> [--target <file>] | release-plan --product <id> --change <summary> --candidate <ref> --rollback <text> --metric <post-release-metric> [--action-card <id>] [--evidence <ref>] | release-work-plan --reviewed <reviewed.json> --actor <actor> [--json] | release-observe --reviewed <reviewed.json> --execution <receipt_id> --signal <signal_id> [--json] | review <review.yaml> --reviewed-by <actor> | action-review <review.yaml> | source-issue-review <review.yaml> | outbound-dry-run-review <review.yaml> | release-review <review.yaml> | timeline <product_id> | doctor <product_id> [--writeback]"
}

func loadProductSignalArg(args []string) (product.SignalDocument, string, error) {
	if len(args) == 1 {
		doc, err := product.LoadSignal(args[0])
		return doc, args[0], err
	}
	var ascDatapoints string
	var ascSalesSeries string
	var ascFunnelSeries string
	var ascAcquisition string
	datapointsOpts := product.ASCDatapointsSignalOptions{Metric: "review_count"}
	salesOpts := product.ASCSalesSeriesSignalOptions{Metric: "units"}
	funnelOpts := product.ASCFunnelSeriesSignalOptions{Metric: "product_page_conversion_rate"}
	acquisitionOpts := product.ASCAcquisitionSignalOptions{Metric: "paid_download_share"}
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return product.SignalDocument{}, "", fmt.Errorf("%s", productUsage())
		}
		value := args[i+1]
		switch args[i] {
		case "--asc-datapoints":
			ascDatapoints = value
		case "--asc-sales-series":
			ascSalesSeries = value
		case "--asc-funnel-series":
			ascFunnelSeries = value
		case "--asc-acquisition":
			ascAcquisition = value
		case "--product":
			datapointsOpts.ProductID = value
			salesOpts.ProductID = value
			funnelOpts.ProductID = value
			acquisitionOpts.ProductID = value
		case "--app-id":
			datapointsOpts.AppID = value
			salesOpts.AppID = value
			funnelOpts.AppID = value
			acquisitionOpts.AppID = value
		case "--metric":
			datapointsOpts.Metric = value
			salesOpts.Metric = value
			funnelOpts.Metric = value
			acquisitionOpts.Metric = value
		case "--before":
			salesOpts.BeforeWindow = value
			funnelOpts.BeforeWindow = value
			acquisitionOpts.BeforeWindow = value
		case "--after":
			salesOpts.AfterWindow = value
			funnelOpts.AfterWindow = value
			acquisitionOpts.AfterWindow = value
		case "--change-event":
			salesOpts.ChangeEvent = value
			funnelOpts.ChangeEvent = value
			acquisitionOpts.ChangeEvent = value
		default:
			return product.SignalDocument{}, "", fmt.Errorf("%s", productUsage())
		}
		i++
	}
	sourceCount := 0
	for _, source := range []string{ascDatapoints, ascSalesSeries, ascFunnelSeries, ascAcquisition} {
		if source != "" {
			sourceCount++
		}
	}
	if sourceCount > 1 {
		return product.SignalDocument{}, "", fmt.Errorf("choose one product signal source")
	}
	if ascDatapoints != "" {
		if datapointsOpts.ProductID == "" || datapointsOpts.AppID == "" {
			return product.SignalDocument{}, "", fmt.Errorf("%s", productUsage())
		}
		doc, err := product.LoadASCDatapointsSignal(ascDatapoints, datapointsOpts)
		return doc, ascDatapoints, err
	}
	if ascSalesSeries != "" {
		if salesOpts.ProductID == "" || salesOpts.AppID == "" || salesOpts.BeforeWindow == "" || salesOpts.AfterWindow == "" || salesOpts.ChangeEvent == "" {
			return product.SignalDocument{}, "", fmt.Errorf("%s", productUsage())
		}
		doc, err := product.LoadASCSalesSeriesSignal(ascSalesSeries, salesOpts)
		return doc, ascSalesSeries, err
	}
	if ascFunnelSeries != "" {
		if funnelOpts.ProductID == "" || funnelOpts.AppID == "" || funnelOpts.BeforeWindow == "" || funnelOpts.AfterWindow == "" || funnelOpts.ChangeEvent == "" {
			return product.SignalDocument{}, "", fmt.Errorf("%s", productUsage())
		}
		doc, err := product.LoadASCFunnelSeriesSignal(ascFunnelSeries, funnelOpts)
		return doc, ascFunnelSeries, err
	}
	if ascAcquisition != "" {
		if acquisitionOpts.ProductID == "" || acquisitionOpts.AppID == "" || acquisitionOpts.BeforeWindow == "" ||
			acquisitionOpts.AfterWindow == "" || acquisitionOpts.ChangeEvent == "" {
			return product.SignalDocument{}, "", fmt.Errorf("%s", productUsage())
		}
		doc, err := product.LoadASCAcquisitionSignal(ascAcquisition, acquisitionOpts)
		return doc, ascAcquisition, err
	}
	return product.SignalDocument{}, "", fmt.Errorf("%s", productUsage())
}

func cmdProduct(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, productUsage())
		os.Exit(2)
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println(productUsage())
		return
	}
	store, homePath := openMemStore()
	switch args[0] {
	case "authority":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--json") {
			fmt.Fprintln(os.Stderr, "usage: compound product authority [--json]")
			os.Exit(2)
		}
		report, err := product.Authority(homePath, store)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product authority:", err)
			os.Exit(1)
		}
		if len(args) == 2 && args[1] == "--json" {
			b, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				fmt.Fprintln(os.Stderr, "product authority:", err)
				os.Exit(1)
			}
			fmt.Println(string(b))
			return
		}
		fmt.Printf("# product authority\n\n")
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- release_planning: %s\n", report.ReleasePlanningRef)
		fmt.Printf("- product_release: %s\n", report.ProductReleaseCommand)
		fmt.Printf("- repository_export: %s\n", report.RepositoryExportCommand)
		fmt.Printf("- release_boundary: %s\n", report.ReleaseBoundary)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- products: %d\n", report.ProductCount)
		fmt.Printf("- signals: %d\n", report.SignalCount)
		fmt.Printf("- hypotheses: %d\n", report.HypothesisCount)
		fmt.Printf("- review_ready: %d\n", report.ReviewReadyCount)
		fmt.Printf("- source_issues: %d\n", report.SourceIssueCount)
		for _, item := range report.Products {
			fmt.Printf("- product %s: %s (%d signal, %d hypothesis, %d transition)\n", item.ProductID, item.Status, item.SignalCount, item.HypothesisCount, item.TransitionCount)
			for _, command := range item.ReviewPlanCommands {
				fmt.Printf("  review_plan: %s\n", command)
			}
		}
	case "loops":
		cmdProductLoops(args[1:], homePath)
	case "ads":
		os.Exit(product.RunAdsCLI(args[1:], homePath, store, os.Stdout, os.Stderr, time.Now()))
	case "where":
		os.Exit(product.RunWhereCLI(args[1:], store, os.Stdout, os.Stderr))
	case "essence", "render-marketing":
		os.Exit(product.RunEssenceCLI(args, store, os.Stdout, os.Stderr))
	case "import":
		var bundle product.Bundle
		sourcePath := ""
		var err error
		if len(args) == 2 {
			sourcePath = args[1]
			bundle, err = product.Load(args[1])
		} else if len(args) == 3 && args[1] == "--project-contract" {
			sourcePath = args[2]
			bundle, err = product.LoadProjectContractBundle(args[2], "")
		} else if len(args) == 5 && args[1] == "--project-contract" && args[3] == "--release" {
			sourcePath = args[2]
			bundle, err = product.LoadProjectContractBundle(args[2], args[4])
		} else {
			fmt.Fprintln(os.Stderr, "usage: compound product import <product.yaml>|--project-contract <project.yaml> [--release <release.yaml>]")
			os.Exit(2)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "product import:", err)
			os.Exit(1)
		}
		report, err := product.Apply(homePath, store, bundle, sourcePath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product import:", err)
			os.Exit(1)
		}
		fmt.Printf("# product import %s\n\n", report.ProductID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- entity: %s\n", report.EntityID)
		fmt.Printf("- evidence: %d\n", len(report.EvidenceIDs))
		fmt.Printf("- claims: %d\n", len(report.ClaimIDs))
		fmt.Printf("- signals: %d\n", len(report.SignalIDs))
		fmt.Printf("- hypotheses: %d\n", len(report.HypothesisIDs))
	case "signal":
		if tryProductGoogleSignal(args[1:], homePath, store) {
			return
		}
		if product.TrySignalKinds(args[1:], homePath, store, productUsage()) {
			return
		}
		doc, sourcePath, err := loadProductSignalArg(args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "product signal:", err)
			os.Exit(1)
		}
		report, err := product.ApplySignal(homePath, store, doc, sourcePath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product signal:", err)
			os.Exit(1)
		}
		fmt.Printf("# product signal %s\n\n", report.SignalIDs[0])
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- evidence: %s\n", report.EvidenceIDs[0])
		if len(report.TransitionIDs) > 0 {
			fmt.Printf("- transition: %s\n", report.TransitionIDs[0])
		}
	case "canonical-metrics":
		productID := valueFlag(args[1:], "--product")
		var sourcePath string
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--product":
				i++
			case strings.HasPrefix(args[i], "-"):
				fmt.Fprintln(os.Stderr, "usage: compound product canonical-metrics <json> --product <id>")
				os.Exit(2)
			case sourcePath == "":
				sourcePath = args[i]
			default:
				fmt.Fprintln(os.Stderr, "usage: compound product canonical-metrics <json> --product <id>")
				os.Exit(2)
			}
		}
		if sourcePath == "" || productID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product canonical-metrics <json> --product <id>")
			os.Exit(2)
		}
		report, err := product.ApplyCanonicalMetrics(homePath, store, sourcePath, product.CanonicalMetricsOptions{ProductID: productID}, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product canonical-metrics:", err)
			os.Exit(1)
		}
		fmt.Printf("# product canonical-metrics %s\n\n", report.SourceRef)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- source_hash: %s\n", report.SourceHash)
		fmt.Printf("- ideas_read: %d\n", report.IdeasRead)
		fmt.Printf("- signals_created: %d\n", report.SignalsCreated)
		fmt.Printf("- signals_existing: %d\n", report.SignalsExisting)
		fmt.Printf("- hypotheses_created: %d\n", report.HypothesesCreated)
		fmt.Printf("- hypotheses_existing: %d\n", report.HypothesesExisting)
		for _, id := range report.SignalIDs {
			fmt.Printf("- signal: %s\n", id)
		}
		for _, id := range report.HypothesisIDs {
			fmt.Printf("- hypothesis: %s\n", id)
		}
		for _, id := range report.TransitionIDs {
			fmt.Printf("- transition: %s\n", id)
		}
	case "signal-doctor":
		if len(args) < 2 || len(args) > 3 || (len(args) == 3 && args[2] != "--record") {
			fmt.Fprintln(os.Stderr, "usage: compound product signal-doctor <signal.yaml|source-export.yaml> [--record]")
			os.Exit(2)
		}
		report := product.SignalDoctor(args[1])
		fmt.Printf("# product signal doctor %s\n\n", report.SourceRef)
		fmt.Printf("- status: %s\n", report.Status)
		if report.SchemaVersion != "" {
			fmt.Printf("- schema: %s\n", report.SchemaVersion)
		}
		if report.SignalID != "" {
			fmt.Printf("- signal: %s\n", report.SignalID)
		}
		if report.ProductID != "" {
			fmt.Printf("- product: %s\n", report.ProductID)
		}
		if report.MetricName != "" {
			fmt.Printf("- metric: %s\n", report.MetricName)
		}
		for _, check := range report.Checks {
			fmt.Printf("- %s: %s", check.Name, check.Status)
			if check.Detail != "" {
				fmt.Printf(" (%s)", check.Detail)
			}
			fmt.Println()
		}
		if len(args) == 3 && args[2] == "--record" {
			if report.Status == "ok" {
				fmt.Printf("- no_promote: skipped valid source\n")
			} else {
				nop, appended, err := product.RecordSignalDoctorNoPromote(homePath, report, time.Now())
				if err != nil {
					fmt.Fprintln(os.Stderr, "product signal-doctor record:", err)
					os.Exit(1)
				}
				report.NoPromoteID = nop.ID
				fmt.Printf("- no_promote: %s", nop.ID)
				if !appended {
					fmt.Printf(" (existing)")
				}
				fmt.Println()
			}
		}
	case "change-stream":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound product change-stream <candidates.md>")
			os.Exit(2)
		}
		doc, err := product.LoadChangeStreamCandidates(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "product change-stream:", err)
			os.Exit(1)
		}
		report, err := product.ApplyChangeStreamCandidates(homePath, doc, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product change-stream:", err)
			os.Exit(1)
		}
		fmt.Printf("# product change-stream %s\n\n", report.SourceRef)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- candidates_read: %d\n", report.CandidatesRead)
		fmt.Printf("- issues_created: %d\n", report.IssuesCreated)
		fmt.Printf("- issues_existing: %d\n", report.IssuesExisting)
		if report.SourceHash != "" {
			fmt.Printf("- source_hash: %s\n", report.SourceHash)
		}
		for _, productID := range report.ProductIDs {
			fmt.Printf("- product: %s\n", productID)
		}
		for _, issueID := range report.IssueIDs {
			fmt.Printf("- issue: %s\n", issueID)
		}
	case "content-draft":
		cmdProductContentDraft(homePath, args[1:])
	case "content-queue":
		cmdProductContentQueue(homePath, args[1:])
	case "hypothesis-plan":
		candidateID := valueFlag(args[1:], "--candidate")
		targetPath := valueFlag(args[1:], "--target")
		if candidateID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product hypothesis-plan --candidate <candidate_id> [--target <file>]")
			os.Exit(2)
		}
		plan, err := product.BuildHypothesisPlan(homePath, candidateID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product hypothesis-plan:", err)
			os.Exit(1)
		}
		paths, err := product.RecordHypothesisPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product hypothesis-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# product hypothesis-plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- candidate: %s\n", plan.CandidateID)
		fmt.Printf("- product: %s\n", plan.ProductID)
		fmt.Printf("- hypothesis_key: %s\n", plan.HypothesisKey)
		fmt.Printf("- review_id: %s\n", plan.ReviewDraft.ReviewID)
		if plan.TargetPath != "" {
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n- evidence_refs: %d\n", len(plan.ReviewDraft.RequiredEvidence), len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n- review: %s\n- signal_draft: %s\n", paths.PlanPath, paths.ReviewPath, paths.SignalDraftPath)
	case "hypothesis-approve":
		rest := args[1:]
		effect, resume := strings.TrimSpace(valueFlag(rest, "--expected-effect")), strings.TrimSpace(valueFlag(rest, "--resume-approval"))
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" || (effect == "" && resume == "") {
			fmt.Fprintln(os.Stderr, "usage: compound product hypothesis-approve <candidate_id> (--expected-effect <sentence>|--resume-approval <approval_id>) [--metrics m1,m2] [--window <span>] [--reviewed-by <actor>]")
			os.Exit(2)
		}
		res, err := product.HypothesisApprove(homePath, product.HypothesisApproveInput{CandidateID: rest[0], ResumeApprovalID: resume, ExpectedEffect: effect, Metrics: splitContentDraftEvidence(valueFlag(rest, "--metrics")), ObservationWindow: valueFlag(rest, "--window"), ReviewedBy: valueFlag(rest, "--reviewed-by")}, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product hypothesis-approve:", err)
			os.Exit(1)
		}
		fmt.Print(product.RenderHypothesisApprovalResult(res))
	case "observation-verdict-plan":
		verdictPath := valueFlag(args[1:], "--verdict")
		targetPath := valueFlag(args[1:], "--target")
		if verdictPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product observation-verdict-plan --verdict <panel-verdict.json> [--target <file>]")
			os.Exit(2)
		}
		plan, err := product.BuildObservationVerdictPlan(homePath, verdictPath, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product observation-verdict-plan:", err)
			os.Exit(1)
		}
		paths, err := product.RecordObservationVerdictPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product observation-verdict-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# product observation-verdict-plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- product: %s\n", plan.ProductID)
		fmt.Printf("- verdict: %s\n", plan.VerdictID)
		fmt.Printf("- panel_decision: %s\n", plan.PanelDecision)
		if plan.TargetPath != "" {
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n- evidence_refs: %d\n", len(plan.ReviewDraft.RequiredEvidence), len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n- review: %s\n- review_yaml: %s\n", paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath)
	case "review-plan":
		hypothesisID := valueFlag(args[1:], "--hypothesis")
		signalID := valueFlag(args[1:], "--signal")
		targetPath := valueFlag(args[1:], "--target")
		if hypothesisID == "" || signalID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product review-plan --hypothesis <hypothesis_id> --signal <signal_id> [--target <file>]")
			os.Exit(2)
		}
		plan, err := product.BuildReviewPlan(homePath, hypothesisID, signalID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product review-plan:", err)
			os.Exit(1)
		}
		paths, err := product.RecordReviewPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product review-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# product review-plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- product: %s\n", plan.ProductID)
		fmt.Printf("- hypothesis: %s\n", plan.HypothesisID)
		fmt.Printf("- signal: %s\n", plan.SignalID)
		fmt.Printf("- review_id: %s\n", plan.ReviewDraft.ReviewID)
		if plan.TargetPath != "" {
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n- evidence_refs: %d\n", len(plan.ReviewDraft.RequiredEvidence), len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n- review: %s\n- review_yaml: %s\n", paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath)
	case "action-plan":
		productID := valueFlag(args[1:], "--product")
		cardID := valueFlag(args[1:], "--card")
		targetPath := valueFlag(args[1:], "--target")
		if productID == "" || cardID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product action-plan --product <product_id> --card <card_id> [--target <file>]")
			os.Exit(2)
		}
		plan, err := product.BuildActionPlan(homePath, productID, cardID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product action-plan:", err)
			os.Exit(1)
		}
		paths, err := product.RecordActionPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product action-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# product action-plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- product: %s\n", plan.ProductID)
		fmt.Printf("- card: %s\n", plan.CardID)
		fmt.Printf("- card_kind: %s\n", plan.CardKind)
		fmt.Printf("- review_id: %s\n", plan.ReviewDraft.ReviewID)
		if plan.TargetPath != "" {
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n- evidence_refs: %d\n", len(plan.ReviewDraft.RequiredEvidence), len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n- review: %s\n- review_yaml: %s\n", paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath)
	case "source-issue-plan":
		productID := valueFlag(args[1:], "--product")
		issueID := valueFlag(args[1:], "--issue")
		targetPath := valueFlag(args[1:], "--target")
		if productID == "" || issueID == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product source-issue-plan --product <product_id> --issue <issue_id> [--target <file>]")
			os.Exit(2)
		}
		plan, err := product.BuildSourceIssuePlan(homePath, productID, issueID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product source-issue-plan:", err)
			os.Exit(1)
		}
		paths, err := product.RecordSourceIssuePlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product source-issue-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# product source-issue-plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- product: %s\n", plan.ProductID)
		fmt.Printf("- issue: %s\n", plan.IssueID)
		fmt.Printf("- source_ref: %s\n", plan.SourceRef)
		fmt.Printf("- review_id: %s\n", plan.ReviewDraft.ReviewID)
		if plan.TargetPath != "" {
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n- evidence_refs: %d\n", len(plan.ReviewDraft.RequiredEvidence), len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n- review: %s\n- review_yaml: %s\n", paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath)
	case "outbound-dry-run-plan":
		reviewedReceiptPath := valueFlag(args[1:], "--reviewed-receipt")
		targetPath := valueFlag(args[1:], "--target")
		if reviewedReceiptPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product outbound-dry-run-plan --reviewed-receipt <reviewed.json> [--target <file>]")
			os.Exit(2)
		}
		plan, err := product.BuildOutboundDryRunEvidencePlan(homePath, reviewedReceiptPath, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product outbound-dry-run-plan:", err)
			os.Exit(1)
		}
		paths, err := product.RecordOutboundDryRunEvidencePlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product outbound-dry-run-plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# product outbound-dry-run-plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- product: %s\n", plan.ProductID)
		fmt.Printf("- receipt: %s\n", plan.ReceiptID)
		fmt.Printf("- review_id: %s\n", plan.ReviewDraft.ReviewID)
		if plan.TargetPath != "" {
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n- evidence_refs: %d\n", len(plan.ReviewDraft.RequiredEvidence), len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n- review: %s\n- review_yaml: %s\n", paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath)
	case "release-plan":
		cmdProductReleasePlan(args[1:], homePath, store)
	case "release-work-plan":
		cmdProductReleaseWorkPlan(args[1:], homePath)
	case "release-observe":
		cmdProductReleaseObserve(args[1:], homePath)
	case "review":
		rest := args[1:]
		reviewedBy := strings.TrimSpace(valueFlag(rest, "--reviewed-by"))
		if reviewedBy == "" {
			reviewedBy = strings.TrimSpace(valueFlag(rest, "--actor"))
		}
		reviewPath := ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--reviewed-by", "--actor":
				i++ // skip the flag value
			default:
				if !strings.HasPrefix(rest[i], "--") && reviewPath == "" {
					reviewPath = rest[i]
				}
			}
		}
		if reviewPath == "" || reviewedBy == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product review <review.yaml> --reviewed-by <actor>")
			os.Exit(2)
		}
		report, err := product.ApplyReviewFile(homePath, store, reviewPath, reviewedBy, time.Now())
		if hasFlag(rest, "--json") {
			printJSON(report)
		} else {
			fmt.Print(product.RenderReviewReport(report))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "product review:", err)
			os.Exit(1)
		}
	case "observation-verdict-review":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound product observation-verdict-review <review.yaml>")
			os.Exit(2)
		}
		review, err := product.LoadObservationVerdictReview(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "product observation verdict review:", err)
			os.Exit(1)
		}
		report, err := product.ApplyObservationVerdictReview(homePath, store, review, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product observation verdict review:", err)
			os.Exit(1)
		}
		fmt.Printf("# product observation-verdict-review %s\n\n", report.ReviewID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- verdict: %s\n", report.VerdictID)
		fmt.Printf("- decision: %s\n", report.Decision)
		fmt.Printf("- evidence: %s\n", report.EvidenceID)
		fmt.Printf("- evidence_refs: %d\n", len(report.EvidenceRefs))
		if report.HypothesisID != "" {
			fmt.Printf("- hypothesis: %s\n", report.HypothesisID)
		}
		if report.TransitionID != "" {
			fmt.Printf("- transition: %s\n", report.TransitionID)
		}
	case "action-review":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound product action-review <review.yaml>")
			os.Exit(2)
		}
		review, err := product.LoadActionReview(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "product action review:", err)
			os.Exit(1)
		}
		report, err := product.ApplyActionReview(homePath, review, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product action review:", err)
			os.Exit(1)
		}
		fmt.Printf("# product action-review %s\n\n", report.ReviewID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- card: %s\n", report.CardID)
		fmt.Printf("- card_kind: %s\n", report.CardKind)
		fmt.Printf("- decision: %s\n", report.Decision)
		fmt.Printf("- evidence_refs: %d\n", len(report.EvidenceRefs))
		fmt.Printf("- reviewed: %s\n", report.ReviewedPath)
	case "action-work-plan":
		reviewedPath := valueFlag(args[1:], "--reviewed")
		actor := strings.TrimSpace(valueFlag(args[1:], "--actor"))
		jsonOut := hasFlag(args[1:], "--json")
		if reviewedPath == "" || actor == "" {
			// No silent default actor: fabricated provenance on the last door
			// before a work draft is worse than missing provenance.
			fmt.Fprintln(os.Stderr, "usage: compound product action-work-plan --reviewed <reviewed.json> --actor <actor> [--json]")
			os.Exit(2)
		}
		draft, err := product.BuildActionWorkDraft(homePath, reviewedPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product action work-plan:", err)
			os.Exit(1)
		}
		workStore, err := work.Open(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product action work-plan:", err)
			os.Exit(1)
		}
		plan, err := workStore.CreatePlan(work.CreateInput{
			WorkID:           draft.WorkID,
			Owner:            draft.WorkOwner,
			Actor:            actor,
			SuccessPredicate: draft.SuccessPredicate,
			DesiredOutcome:   draft.DesiredOutcome,
			NonGoals:         draft.NonGoals,
			EvidenceRefs:     draft.EvidenceRefs,
			State:            "proposed",
			Risk:             "tier2",
			Privacy:          "private",
		}, time.Now(), work.CreatePlanOptions{
			Actor:         actor,
			Capability:    "product.action_review",
			Action:        "plan_work_from_reviewed_product_action",
			MutationClass: "local_reversible",
			Reversibility: "easy",
			BlastRadius:   "local",
			TargetKind:    "product_action_review",
			TargetID:      draft.ReviewReportID,
			TargetPath:    reviewedPath,
			ActionContext: &work.WorkActionContext{
				Kind:               "product_action_review",
				SourceRef:          "compound://reviews/" + product.ProductActionReviewArea + "/" + filepath.Base(reviewedPath),
				SourceID:           draft.ReviewReportID,
				Title:              draft.CardTitle,
				ActionSummary:      draft.ActionSummary,
				Command:            draft.ActionCommand,
				RequiredInputs:     draft.RequiredInputs,
				ExpectedValidation: draft.ExpectedValidation,
				StopConditions:     draft.StopConditions,
				Boundary:           draft.Boundary,
				Proof:              draft.Proof,
				EvidenceRefs:       draft.EvidenceRefs,
			},
			Record: true,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "product action work-plan:", err)
			os.Exit(1)
		}
		out := struct {
			SchemaVersion string                  `json:"schema_version"`
			Status        string                  `json:"status"`
			ProductAction product.ActionWorkDraft `json:"product_action"`
			WorkPlan      work.CreatePlan         `json:"work_plan"`
		}{
			SchemaVersion: "compound.product_action_work_plan_report.v0.1",
			Status:        "recorded",
			ProductAction: draft,
			WorkPlan:      plan,
		}
		if jsonOut {
			printJSON(out)
			return
		}
		fmt.Printf("# product action-work-plan %s\n\n", draft.ID)
		fmt.Printf("- status: %s\n", out.Status)
		fmt.Printf("- product: %s\n", draft.ProductID)
		fmt.Printf("- review: %s\n", draft.ReviewReportID)
		fmt.Printf("- work: %s\n", plan.WorkID)
		fmt.Printf("- work_state: %s\n", plan.WorkState)
		fmt.Printf("- policy: %s allowed=%t\n", plan.Policy.Outcome, plan.Policy.Allowed)
		fmt.Printf("- plan: %s\n", plan.PlanPath)
		if draft.ActionSummary != "" {
			fmt.Printf("- action_summary: %s\n", draft.ActionSummary)
		}
		if len(draft.ActionCommand) > 0 {
			fmt.Printf("- action_command: %s\n", strings.Join(draft.ActionCommand, " "))
		}
		fmt.Printf("- proof: %s\n", draft.Proof)
	case "outbound-dry-run-review":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound product outbound-dry-run-review <review.yaml>")
			os.Exit(2)
		}
		review, err := product.LoadOutboundDryRunEvidenceReview(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "product outbound dry-run review:", err)
			os.Exit(1)
		}
		report, err := product.ApplyOutboundDryRunEvidenceReview(homePath, store, review, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product outbound dry-run review:", err)
			os.Exit(1)
		}
		fmt.Printf("# product outbound-dry-run-review %s\n\n", report.ReviewID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- receipt: %s\n", report.ReceiptID)
		fmt.Printf("- decision: %s\n", report.Decision)
		fmt.Printf("- evidence: %s\n", report.EvidenceID)
		fmt.Printf("- evidence_refs: %d\n", len(report.EvidenceRefs))
		if report.HypothesisID != "" {
			fmt.Printf("- hypothesis: %s\n", report.HypothesisID)
		}
		if report.TransitionID != "" {
			fmt.Printf("- transition: %s\n", report.TransitionID)
		}
	case "release-review":
		cmdProductReleaseReview(args[1:], homePath)
	case "source-issue-review":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound product source-issue-review <review.yaml>")
			os.Exit(2)
		}
		review, err := product.LoadSourceIssueReview(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "product source issue review:", err)
			os.Exit(1)
		}
		report, err := product.ApplySourceIssueReview(homePath, store, review, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product source issue review:", err)
			os.Exit(1)
		}
		fmt.Printf("# product source issue review %s\n\n", report.ReviewID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- product: %s\n", report.ProductID)
		fmt.Printf("- issue: %s\n", report.IssueID)
		fmt.Printf("- result: %s\n", report.Result)
		fmt.Printf("- evidence: %s\n", report.EvidenceID)
		fmt.Printf("- evidence_refs: %d\n", len(report.EvidenceRefs))
		if report.TransitionID != "" {
			fmt.Printf("- transition: %s\n", report.TransitionID)
		}
		if report.HypothesisID != "" {
			fmt.Printf("- hypothesis: %s\n", report.HypothesisID)
		}
	case "timeline":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound product timeline <product_id>")
			os.Exit(2)
		}
		tl, err := product.LoadTimeline(homePath, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "product timeline:", err)
			os.Exit(1)
		}
		fmt.Printf("# Product timeline - %s\n\n", args[1])
		if len(tl.Signals) == 0 && len(tl.Hypotheses) == 0 && len(tl.Transitions) == 0 && len(tl.SourceIssues) == 0 {
			fmt.Println("(no product loop records)")
			return
		}
		for _, s := range tl.Signals {
			fmt.Printf("- signal %s: %s %.4g -> %.4g on %s (%s)\n", s.ID, s.MetricName, s.Before, s.After, s.Surface, s.Window)
			if s.NextAction != "" {
				fmt.Printf("  next: %s\n", s.NextAction)
			}
			if len(s.Confounders) > 0 {
				fmt.Printf("  confounders: %s\n", strings.Join(s.Confounders, ", "))
			}
		}
		for _, h := range tl.Hypotheses {
			fmt.Printf("- hypothesis %s: %s [%s]\n", h.ID, h.ExpectedEffect, h.Result)
			if h.NextAction != "" {
				fmt.Printf("  next: %s\n", h.NextAction)
			}
		}
		for _, tr := range tl.Transitions {
			fmt.Printf("- transition %s", tr.ID)
			if tr.Kind != "" {
				fmt.Printf(" [%s]", tr.Kind)
			}
			if tr.ObjectRef != "" {
				fmt.Printf(" %s", tr.ObjectRef)
			}
			fmt.Printf(": %s\n", tr.StateDelta)
		}
		for _, issue := range tl.SourceIssues {
			status := issue.Status
			if status == "" {
				status = "open"
			}
			fmt.Printf("- source_issue %s [%s]: %s\n", issue.ID, status, issue.SourceRef)
			if issue.Basis != "" {
				fmt.Printf("  basis: %s\n", issue.Basis)
			}
			if issue.ReviewID != "" {
				fmt.Printf("  review: %s (%s)\n", issue.ReviewID, issue.ReviewResult)
			}
		}
	case "doctor":
		if len(args) < 2 || len(args) > 3 || (len(args) == 3 && args[2] != "--writeback") {
			fmt.Fprintln(os.Stderr, "usage: compound product doctor <product_id> [--writeback]")
			os.Exit(2)
		}
		report, err := product.DoctorForRegisteredProduct(homePath, store, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product doctor:", err)
			os.Exit(1)
		}
		fmt.Printf("# product doctor - %s\n\n", report.ProductID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- signals: %d\n", report.SignalCount)
		fmt.Printf("- hypotheses: %d\n", report.HypothesisCount)
		fmt.Printf("- pending_review: %d\n", report.PendingReviewCount)
		fmt.Printf("- review_ready: %d\n", report.ReviewReadyCount)
		for _, ready := range report.ReviewReady {
			fmt.Printf("- review_plan: %s\n", ready.ReviewPlanCommand)
			fmt.Printf("  match: %s %s %s\n", ready.MatchReason, ready.MetricName, ready.Window)
		}
		fmt.Printf("- source_issues: %d\n", report.SourceIssueCount)
		fmt.Printf("- missing_evidence_refs: %d\n", len(report.MissingEvidenceRefs))
		for _, stale := range report.ASCSignalStaleness {
			fmt.Printf("- asc_signal_stale: %s last=%s age_hours=%d\n", stale.LastSignalID, stale.LastObservedAt, stale.AgeHours)
			fmt.Printf("  fix: %s\n", stale.FixCommand)
		}
		if len(args) == 3 && args[2] == "--writeback" && product.NeedsWriteback(report) {
			result, err := runProductWriteback(report, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "product writeback:", err)
				os.Exit(1)
			}
			fmt.Printf("- writeback_gate: %s %s\n", result.GateID, result.GateStatus)
			fmt.Printf("- work: %s\n", result.WorkID)
			if result.ReceiptID != "" {
				fmt.Printf("- receipt: %s\n", result.ReceiptID)
			}
			if result.TransitionID != "" {
				fmt.Printf("- transition: %s\n", result.TransitionID)
			}
		}
	default:
		fmt.Fprintln(os.Stderr, productUsage())
		os.Exit(2)
	}
}

// contentDraftUsage derives its platform list from product.ContentDraftPlatforms()
// rather than restating it: the names had three copies and this is the one that
// would have gone stale the moment the allowlist widened.
var contentDraftUsage = "usage: compound product content-draft record --product <id> --platform <" + strings.Join(product.ContentDraftPlatforms(), "|") + "> [--text <body>] [--voice-pack <ref>] [--evidence <ref,ref>] (--ttl <dur>|--expires <rfc3339>) | content-draft list [--product <id>] [--json] | content-draft park|operationalize|reject <draft_id>"

// cmdProductContentDraft is the distribution-loop draft door: record / list /
// park / operationalize / reject. Error and refusal paths exit non-zero; the
// owner logic (internal/product) is where the invariants are enforced and tested.
func cmdProductContentDraft(homePath string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, contentDraftUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "record":
		rest := args[1:]
		productID := valueFlag(rest, "--product")
		platform := valueFlag(rest, "--platform")
		if strings.TrimSpace(productID) == "" || strings.TrimSpace(platform) == "" {
			fmt.Fprintln(os.Stderr, contentDraftUsage)
			os.Exit(2)
		}
		body := valueFlag(rest, "--text")
		if strings.TrimSpace(body) == "" {
			piped, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintln(os.Stderr, "product content-draft record:", err)
				os.Exit(1)
			}
			body = string(piped)
		}
		now := time.Now()
		expiresAt, err := resolveContentDraftExpiry(valueFlag(rest, "--ttl"), valueFlag(rest, "--expires"), now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product content-draft record:", err)
			os.Exit(2)
		}
		media, err := generateDraftMedia(homePath, rest, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product content-draft record:", err)
			os.Exit(1)
		}
		draft, err := product.RecordContentDraft(homePath, product.ContentDraftInput{
			ProductID:    productID,
			Platform:     platform,
			Body:         body,
			VoicePackRef: valueFlag(rest, "--voice-pack"),
			EvidenceRefs: splitContentDraftEvidence(valueFlag(rest, "--evidence")),
			ExpiresAt:    expiresAt,
			Media:        media,
		}, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product content-draft record:", err)
			os.Exit(1)
		}
		printContentDraft("recorded", draft)
	case "list":
		rest := args[1:]
		drafts, err := product.ListContentDrafts(homePath, valueFlag(rest, "--product"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "product content-draft list:", err)
			os.Exit(1)
		}
		if hasFlag(rest, "--json") {
			blob, err := json.MarshalIndent(drafts, "", "  ")
			if err != nil {
				fmt.Fprintln(os.Stderr, "product content-draft list:", err)
				os.Exit(1)
			}
			fmt.Println(string(blob))
			return
		}
		fmt.Printf("# product content-drafts: %d\n", len(drafts))
		for _, d := range drafts {
			fmt.Printf("- %s product=%s platform=%s state=%s expires=%s body=%s\n",
				d.ID, d.ProductID, d.Platform, d.State, d.ExpiresAt, d.BodyRef)
		}
	case "park", "operationalize", "reject":
		rest := args[1:]
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			fmt.Fprintf(os.Stderr, "usage: compound product content-draft %s <draft_id>\n", args[0])
			os.Exit(2)
		}
		now := time.Now()
		var (
			draft product.ContentDraft
			err   error
		)
		switch args[0] {
		case "park":
			draft, err = product.ParkContentDraft(homePath, rest[0], now)
		case "operationalize":
			draft, err = product.OperationalizeContentDraft(homePath, rest[0], now)
		case "reject":
			draft, err = product.RejectContentDraft(homePath, rest[0], now)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "product content-draft %s: %v\n", args[0], err)
			os.Exit(1)
		}
		printContentDraft(draft.State, draft)
	default:
		fmt.Fprintln(os.Stderr, contentDraftUsage)
		os.Exit(2)
	}
}

func resolveContentDraftExpiry(ttl, expires string, now time.Time) (time.Time, error) {
	ttl = strings.TrimSpace(ttl)
	expires = strings.TrimSpace(expires)
	if expires != "" {
		t, err := time.Parse(time.RFC3339, expires)
		if err != nil {
			return time.Time{}, fmt.Errorf("--expires must be RFC3339: %w", err)
		}
		return t, nil
	}
	if ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return time.Time{}, fmt.Errorf("--ttl must be a Go duration: %w", err)
		}
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--ttl must be positive")
		}
		return now.Add(d), nil
	}
	return time.Time{}, fmt.Errorf("expiry required: pass --ttl <dur> or --expires <rfc3339>")
}

func splitContentDraftEvidence(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printContentDraft(action string, d product.ContentDraft) {
	fmt.Printf("# content-draft %s\n", action)
	fmt.Printf("- id: %s\n", d.ID)
	fmt.Printf("- product: %s\n", d.ProductID)
	fmt.Printf("- platform: %s\n", d.Platform)
	fmt.Printf("- state: %s\n", d.State)
	fmt.Printf("- body_ref: %s\n", d.BodyRef)
	fmt.Printf("- expires_at: %s\n", d.ExpiresAt)
	if d.VoicePackRef != "" {
		fmt.Printf("- voice_pack_ref: %s\n", d.VoicePackRef)
	}
	if len(d.EvidenceRefs) > 0 {
		fmt.Printf("- evidence_refs: %s\n", strings.Join(d.EvidenceRefs, ", "))
	}
}

const contentQueueUsage = "usage: compound product content-queue plan [--json] | content-queue apply --review <review.yaml> [--json]"

// cmdProductContentQueue is the distribution-loop batch adjudication door: plan
// renders every reviewable draft into one review YAML skeleton; apply executes
// the per-draft verdicts (approve mints a governed post ActionIntent+PolicyDecision
// and operationalizes; reject retains; hold leaves unchanged). The owner logic
// (internal/product) enforces and tests the invariants.
func cmdProductContentQueue(homePath string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, contentQueueUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "plan":
		rest := args[1:]
		plan, err := product.BuildContentQueuePlan(homePath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product content-queue plan:", err)
			os.Exit(1)
		}
		paths, err := product.RecordContentQueuePlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product content-queue plan:", err)
			os.Exit(1)
		}
		if hasFlag(rest, "--json") {
			printJSON(struct {
				Plan  product.ContentQueuePlan      `json:"plan"`
				Paths product.ContentQueuePlanPaths `json:"paths"`
			}{plan, paths})
			return
		}
		fmt.Printf("# product content-queue plan %s\n\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- reviewable: %d\n", plan.ReviewableCount)
		fmt.Printf("- expired: %d\n", plan.ExpiredCount)
		fmt.Printf("- review_id: %s\n", plan.ReviewDraft.ReviewID)
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
		fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
	case "apply":
		rest := args[1:]
		reviewPath := strings.TrimSpace(valueFlag(rest, "--review"))
		if reviewPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound product content-queue apply --review <review.yaml> [--json]")
			os.Exit(2)
		}
		review, err := product.LoadContentQueueReview(reviewPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "product content-queue apply:", err)
			os.Exit(1)
		}
		report, err := product.ApplyContentQueueReview(homePath, review, reviewPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "product content-queue apply:", err)
			os.Exit(1)
		}
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# product content-queue apply %s\n\n", report.ID)
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- approved: %d\n", report.Counts.Approved)
		fmt.Printf("- rejected: %d\n", report.Counts.Rejected)
		fmt.Printf("- held: %d\n", report.Counts.Held)
		fmt.Printf("- refused: %d\n", report.Counts.Refused)
		fmt.Printf("- errors: %d\n", report.Counts.Errors)
		for _, row := range report.Rows {
			line := fmt.Sprintf("- %s verdict=%s outcome=%s", row.DraftID, row.Verdict, row.Outcome)
			if row.ActionIntentID != "" {
				line += fmt.Sprintf(" action_intent=%s policy_decision=%s policy=%s", row.ActionIntentID, row.PolicyDecisionID, row.PolicyOutcome)
			}
			if row.Error != "" {
				line += " error=" + row.Error
			}
			fmt.Println(line)
		}
	default:
		fmt.Fprintln(os.Stderr, contentQueueUsage)
		os.Exit(2)
	}
}

// generateDraftMedia runs an enrolled generator when --generate names one, so a
// draft can be born with its asset already attached. Drafts are immutable once
// recorded, so generating here (rather than attaching later) keeps one write path.
// No --generate flag means no media and no generator run.
func generateDraftMedia(homePath string, rest []string, now time.Time) ([]product.ContentDraftMediaInput, error) {
	adapter := strings.TrimSpace(valueFlag(rest, "--generate"))
	if adapter == "" {
		return nil, nil
	}
	if err := product.RequireDistributionExperiment("content-draft record --generate"); err != nil {
		return nil, err
	}
	params := map[string]string{}
	for i := 0; i < len(rest)-1; i++ {
		if rest[i] != "--generate-param" {
			continue
		}
		key, value, ok := strings.Cut(rest[i+1], "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--generate-param must be key=value, got %q", rest[i+1])
		}
		params[strings.TrimSpace(key)] = value
	}
	media, err := product.GenerateContentDraftMedia(homePath, product.MediaGenerateRequest{
		Adapter: adapter,
		Prompt:  valueFlag(rest, "--generate-prompt"),
		AltText: valueFlag(rest, "--generate-alt"),
		Params:  params,
	}, nil, now)
	if err != nil {
		return nil, err
	}
	return []product.ContentDraftMediaInput{media}, nil
}
