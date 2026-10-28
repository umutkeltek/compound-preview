package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"compound/internal/product"
)

func cmdProductLoops(args []string, homePath string) {
	opts, asJSON, err := parseProductLoopArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "product loops:", err)
		os.Exit(2)
	}
	model, err := product.BuildLoopModel(homePath, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "product loops:", err)
		os.Exit(1)
	}
	if asJSON {
		b, err := json.MarshalIndent(model, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "product loops:", err)
			os.Exit(1)
		}
		fmt.Println(string(b))
		return
	}
	renderProductLoops(model)
}

func parseProductLoopArgs(args []string) (product.LoopOptions, bool, error) {
	var opts product.LoopOptions
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--product":
			if i+1 >= len(args) {
				return opts, false, fmt.Errorf("usage: compound product loops [--product <id>] [--json] [--recent N] [--stale-days D]")
			}
			opts.ProductID = args[i+1]
			i++
		case "--recent":
			value, err := parsePositiveIntFlag(args, &i, "--recent")
			if err != nil {
				return opts, false, err
			}
			opts.Recent = value
		case "--stale-days":
			value, err := parsePositiveIntFlag(args, &i, "--stale-days")
			if err != nil {
				return opts, false, err
			}
			opts.StaleDays = value
		default:
			return opts, false, fmt.Errorf("usage: compound product loops [--product <id>] [--json] [--recent N] [--stale-days D]")
		}
	}
	return opts, asJSON, nil
}

func parsePositiveIntFlag(args []string, index *int, flag string) (int, error) {
	if *index+1 >= len(args) {
		return 0, fmt.Errorf("%s requires a positive integer", flag)
	}
	value, err := strconv.Atoi(args[*index+1])
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s requires a positive integer", flag)
	}
	*index = *index + 1
	return value, nil
}

func renderProductLoops(model product.LoopModel) {
	fmt.Printf("# product loops\n\n")
	fmt.Printf("- generated_at: %s\n", model.GeneratedAt)
	fmt.Printf("- products: %d\n", len(model.Products))
	for _, loop := range model.Products {
		fmt.Printf("\n## %s\n", loop.ProductID)
		fmt.Printf("- name: %s\n", loop.Name)
		fmt.Printf("- freshness: latest_signal=%s latest_review=%s stale=%t\n", none(loop.Freshness.LatestSignalAt), none(loop.Freshness.LatestReviewAt), loop.Freshness.Stale)
		fmt.Printf("- open_source_issues: %d", loop.OpenSourceIssues.Count)
		if len(loop.OpenSourceIssues.Refs) > 0 {
			fmt.Printf(" %s", strings.Join(loop.OpenSourceIssues.Refs, ", "))
		}
		fmt.Println()
		renderLoopMetrics(loop.SignalsByMetric)
		renderLoopHypotheses(loop.OpenHypotheses)
		renderLoopMissingSignalSources(loop.OpenHypothesesByMissingSignalSource)
		renderLoopSignalHypothesisGaps(loop.SignalsWithoutHypothesis)
		renderLoopReviews(loop.Reviews)
		renderLoopReviewNextActions(loop.ClosedReviewsByNextAction)
		renderLoopActionCards(loop.ActionCards)
		if len(loop.LoopGaps) > 0 {
			fmt.Printf("- gaps: %s\n", strings.Join(loop.LoopGaps, ", "))
		} else {
			fmt.Printf("- gaps: none\n")
		}
	}
}

func renderLoopMetrics(signalsByMetric map[string][]product.LoopSignal) {
	keys := make([]string, 0, len(signalsByMetric))
	for key := range signalsByMetric {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Printf("- metrics: none\n")
		return
	}
	fmt.Printf("- metrics:\n")
	for _, key := range keys {
		signals := signalsByMetric[key]
		if len(signals) == 0 {
			continue
		}
		latest := signals[0]
		fmt.Printf("  - %s: %.4g -> %.4g (%s, %s, %s)\n", key, latest.Before, latest.After, latest.Provider, latest.SignalID, none(latest.ObservedAt))
	}
}

func renderLoopHypotheses(hypotheses []product.LoopHypothesis) {
	if len(hypotheses) == 0 {
		fmt.Printf("- open_hypotheses: none\n")
		return
	}
	fmt.Printf("- open_hypotheses:\n")
	for _, hypothesis := range hypotheses {
		fmt.Printf("  - %s: %s -> %s\n", hypothesis.ID, hypothesis.Statement, hypothesis.ExpectedEffect)
		details := []string{}
		if len(hypothesis.Metrics) > 0 {
			details = append(details, "metrics="+strings.Join(hypothesis.Metrics, ","))
		}
		if hypothesis.ObservationWindow != "" {
			details = append(details, "window="+hypothesis.ObservationWindow)
		}
		if hypothesis.NextAction != "" {
			details = append(details, "next="+hypothesis.NextAction)
		}
		if len(details) > 0 {
			fmt.Printf("    %s\n", strings.Join(details, " "))
		}
	}
}

func renderLoopReviews(reviews []product.LoopReview) {
	if len(reviews) == 0 {
		fmt.Printf("- reviews: none\n")
		return
	}
	fmt.Printf("- reviews:\n")
	for _, review := range reviews {
		parts := []string{review.ID, review.Verdict, "hypothesis=" + review.HypothesisID}
		if review.SignalID != "" {
			parts = append(parts, "signal="+review.SignalID)
		}
		if review.ReviewedAt != "" {
			parts = append(parts, "reviewed="+review.ReviewedAt)
		}
		fmt.Printf("  - %s\n", strings.Join(parts, " "))
	}
}

func renderLoopMissingSignalSources(groups []product.LoopMissingSignalSource) {
	if len(groups) == 0 {
		fmt.Printf("- open_hypotheses_by_missing_signal_source: none\n")
		return
	}
	fmt.Printf("- open_hypotheses_by_missing_signal_source:\n")
	for _, group := range groups {
		parts := []string{group.Source, "hypotheses=" + strings.Join(group.HypothesisIDs, ",")}
		if len(group.Metrics) > 0 {
			parts = append(parts, "metrics="+strings.Join(group.Metrics, ","))
		}
		if len(group.NextActions) > 0 {
			parts = append(parts, "next="+strings.Join(group.NextActions, " | "))
		}
		fmt.Printf(" - %s\n", strings.Join(parts, " "))
	}
}

func renderLoopSignalHypothesisGaps(groups []product.LoopSignalHypothesisGap) {
	if len(groups) == 0 {
		fmt.Printf("- signals_without_hypothesis: none\n")
		return
	}
	fmt.Printf("- signals_without_hypothesis:\n")
	for _, group := range groups {
		parts := []string{group.ChangeEvent, "signals=" + strings.Join(group.SignalIDs, ",")}
		if len(group.Metrics) > 0 {
			parts = append(parts, "metrics="+strings.Join(group.Metrics, ","))
		}
		if group.Consumed {
			parts = append(parts, "consumed=true")
			if len(group.ConsumedByWorkIDs) > 0 {
				parts = append(parts, "work="+strings.Join(group.ConsumedByWorkIDs, ","))
			}
		}
		fmt.Printf(" - %s\n", strings.Join(parts, " "))
	}
}

func renderLoopReviewNextActions(groups []product.LoopReviewNextAction) {
	if len(groups) == 0 {
		fmt.Printf("- closed_reviews_by_next_action: none\n")
		return
	}
	fmt.Printf("- closed_reviews_by_next_action:\n")
	for _, group := range groups {
		parts := []string{group.NextAction, "reviews=" + strings.Join(group.ReviewIDs, ",")}
		if len(group.HypothesisIDs) > 0 {
			parts = append(parts, "hypotheses="+strings.Join(group.HypothesisIDs, ","))
		}
		if len(group.SignalIDs) > 0 {
			parts = append(parts, "signals="+strings.Join(group.SignalIDs, ","))
		}
		if group.Consumed {
			parts = append(parts, "consumed=true")
			if len(group.ConsumedByWorkIDs) > 0 {
				parts = append(parts, "work="+strings.Join(group.ConsumedByWorkIDs, ","))
			}
		}
		fmt.Printf(" - %s\n", strings.Join(parts, " "))
	}
}

func renderLoopActionCards(cards []product.LoopActionCard) {
	if len(cards) == 0 {
		fmt.Printf("- action_cards: none\n")
		return
	}
	fmt.Printf("- action_cards:\n")
	for _, card := range cards {
		parts := []string{card.Kind, card.ID}
		if len(card.Command) > 0 {
			parts = append(parts, "command="+strings.Join(card.Command, " "))
		}
		if card.Boundary != "" {
			parts = append(parts, "boundary="+card.Boundary)
		}
		fmt.Printf(" - %s\n", strings.Join(parts, " "))
	}
}

func none(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}
