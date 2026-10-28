package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/routeevidence"
	routeevidencecli "compound/internal/routeevidence/cli"
)

func tryRoutePromotionCommand(args []string, homePath string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "model-capabilities" {
		if code := routeevidencecli.RunModelCapabilities(args[1:], homePath, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return true
	}
	switch args[0] {
	case "import-promotion-criteria":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound route import-promotion-criteria <promotion-criteria.yaml>")
			os.Exit(2)
		}
		report, err := routeevidence.ImportPromotionCriteria(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "route import-promotion-criteria:", err)
			os.Exit(1)
		}
		fmt.Println("# route import-promotion-criteria")
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- authority_path: %s\n", report.AuthorityPath)
		fmt.Printf("- source_authority: %s\n", report.SourceAuthorityID)
		fmt.Printf("- entries: %d\n", report.EntryCount)
		fmt.Printf("- capability_classes: %s\n", strings.Join(report.CapabilityClasses, ","))
		fmt.Println("- verdict: compound_primary")
		return true
	case "promotion-review":
		laneID, jsonOut := parseRoutePromotionReviewArgs(args[1:])
		reviews, err := routeevidence.EvaluateLanePromotions(homePath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "route promotion-review:", err)
			os.Exit(1)
		}
		reviews = filterPromotionReviews(reviews, laneID)
		if jsonOut {
			printJSON(reviews)
			return true
		}
		printRoutePromotionReviews(reviews)
		return true
	case "promotion-record":
		reviewPath, rationale, jsonOut := parseRoutePromotionRecordArgs(args[1:])
		report, err := routeevidence.RecordPromotionHoldReview(homePath, reviewPath, rationale, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "route promotion-record:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return true
		}
		fmt.Println("# route promotion-record")
		fmt.Printf("- review: %s\n", report.ReviewID)
		fmt.Printf("- decision: %s\n", report.Decision)
		fmt.Printf("- lane: %s\n", report.LaneID)
		fmt.Printf("- capability_class: %s\n", report.CapabilityClass)
		fmt.Printf("- recorded: %s\n", report.ReviewPath)
		fmt.Println("- authority_mutation: none")
		return true
	case "promotion-apply":
		// The governed authority door. promotion-record above stays hold-only;
		// this verb is the only path that mutates a model-router claim.
		if code := routeevidencecli.RunPromotionApply(args[1:], homePath, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return true
	case "promotion-revert":
		if code := routeevidencecli.RunPromotionRevert(args[1:], homePath, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return true
	default:
		return false
	}
}

func parseRoutePromotionReviewArgs(args []string) (laneID string, jsonOut bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--lane":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, "usage: compound route promotion-review [--lane <id>] [--json]")
				os.Exit(2)
			}
			laneID = args[i]
		default:
			fmt.Fprintln(os.Stderr, "usage: compound route promotion-review [--lane <id>] [--json]")
			os.Exit(2)
		}
	}
	return laneID, jsonOut
}

func parseRoutePromotionRecordArgs(args []string) (reviewPath, rationale string, jsonOut bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--review":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, "usage: compound route promotion-record --review <route_promotion.json> --rationale <text> [--json]")
				os.Exit(2)
			}
			reviewPath = args[i]
		case "--rationale":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, "usage: compound route promotion-record --review <route_promotion.json> --rationale <text> [--json]")
				os.Exit(2)
			}
			rationale = args[i]
		default:
			fmt.Fprintln(os.Stderr, "usage: compound route promotion-record --review <route_promotion.json> --rationale <text> [--json]")
			os.Exit(2)
		}
	}
	if strings.TrimSpace(reviewPath) == "" || strings.TrimSpace(rationale) == "" {
		fmt.Fprintln(os.Stderr, "usage: compound route promotion-record --review <route_promotion.json> --rationale <text> [--json]")
		os.Exit(2)
	}
	return reviewPath, rationale, jsonOut
}

func filterPromotionReviews(reviews []routeevidence.PromotionReview, laneID string) []routeevidence.PromotionReview {
	laneID = strings.TrimSpace(laneID)
	if laneID == "" {
		return reviews
	}
	filtered := make([]routeevidence.PromotionReview, 0, len(reviews))
	for _, review := range reviews {
		if review.LaneID == laneID {
			filtered = append(filtered, review)
		}
	}
	return filtered
}

func printRoutePromotionReviews(reviews []routeevidence.PromotionReview) {
	fmt.Println("# route promotion-review")
	fmt.Printf("- reviews: %d\n", len(reviews))
	for _, review := range reviews {
		fmt.Printf("- lane: %s capability_class=%s claim=%s samples=%d failed_status_rate=%.3f judge_json_malformed_rate=%.3f comparison_wins=%d comparison_losses=%d window_days=%.2f recommendation=%s\n",
			review.LaneID,
			review.CapabilityClass,
			review.CurrentClaimLevel,
			review.Tallies.Samples,
			review.Tallies.FailedStatusRate,
			review.Tallies.JudgeJSONMalformedRate,
			review.Tallies.ComparisonWins,
			review.Tallies.ComparisonLosses,
			review.Tallies.WindowDays,
			review.Recommendation,
		)
		fmt.Printf("  reason: %s\n", review.Reason)
		if review.ProposedChange != "" {
			fmt.Printf("  proposed_change: %s\n", review.ProposedChange)
		}
	}
	fmt.Println("- verdict: review_artifacts_only_no_authority_mutation")
}
