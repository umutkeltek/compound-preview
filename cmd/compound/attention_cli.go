package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/activity"
	"compound/internal/attention"
	"compound/internal/home"
	"compound/internal/hookplan"
	"compound/internal/work"
)

const attentionUsage = "usage: compound attention [--json] [--receipt-only-grade tidy] [--home P]"

func cmdAttention(args []string) {
	if hasUsageFlag(args) {
		fmt.Println(attentionUsage)
		return
	}
	jsonOut := false
	homeFlag := ""
	receiptOnlyGrade := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--receipt-only-grade":
			if i+1 >= len(args) || args[i+1] != attention.RiskGradeTidy {
				fmt.Fprintln(os.Stderr, attentionUsage)
				os.Exit(2)
			}
			i++
			receiptOnlyGrade = attention.RiskGradeTidy
		case "--home":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				fmt.Fprintln(os.Stderr, attentionUsage)
				os.Exit(2)
			}
			i++
			homeFlag = strings.TrimSpace(args[i])
		default:
			fmt.Fprintln(os.Stderr, attentionUsage)
			os.Exit(2)
		}
	}

	homePath, err := home.Resolve(homeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	// Read-only by registry contract: attention inspects the ledger and must
	// not materialize work/ or events/ just by looking (see work.OpenReadOnly).
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	now := time.Now()
	reviews, err := buildReviewQueueReport(homePath, "", false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	// One Workbench build serves both consumers below. attention.Build never
	// reads Recent, so ExecutorsProjection's RecentLimit 8 is the binding
	// requirement; building twice (5 here, 8 inside the projection) walked
	// the whole event store twice per invocation.
	workbench, err := store.Workbench(now, work.WorkbenchOptions{RecentLimit: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	executors, err := store.ExecutorsProjection(now, work.ExecutorsProjectionOptions{Workbench: &workbench})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	report := attention.Build(attention.Inputs{
		Reviews:   &reviews,
		Workbench: &workbench,
		Executors: &executors,
		Activity:  activityAttentionInput(homePath, &workbench, now),
	}, now)
	if jsonOut {
		operatorAttention, err := hookplan.ReadOperatorAttention(hookplan.OperatorAttentionRequest{
			HomePath: homePath,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		payload := struct {
			attention.Report
			OperatorAttention hookplan.OperatorAttentionProjection `json:"operator_attention"`
		}{
			Report:            report,
			OperatorAttention: operatorAttention,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if receiptOnlyGrade == "" {
		printAttentionText(report)
		return
	}
	printAttentionTextWithReceiptOnlyGrade(report, receiptOnlyGrade)
}

// activityAttentionMaxAgeDays bounds how old the latest span day may be before the activity read
// yields nothing: a sensor that stopped draining weeks ago must not squat a stale block on the
// attention board. Staleness-gating over memory is the attention norm; freshness metadata alone
// would still rank a dead sensor's last day every morning.
const activityAttentionMaxAgeDays = 3

// activityAttentionInput is the decision-13 additive activity read: latest persisted span day ->
// activity.InferDay against the workbench's visible envelope/receipt timestamps ->
// attention.ActivityDayInput. Nil in every quiet case (no spans, stale spans, zero unaccounted
// blocks). A read error warns on stderr but never blocks the board — this input is enrichment; the
// attention projection is the product.
func activityAttentionInput(homePath string, workbench *work.WorkbenchReport, now time.Time) *attention.ActivityDayInput {
	day, ok, err := activity.LatestSpanDay(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: activity read skipped:", err)
		return nil
	}
	if !ok {
		return nil
	}
	dayStart, err := time.Parse("2006-01-02", day)
	if err != nil || now.UTC().Sub(dayStart) > time.Duration(activityAttentionMaxAgeDays)*24*time.Hour {
		return nil
	}
	spanFile, ok, err := activity.ReadSpanFile(homePath, day)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: activity read skipped:", err)
		return nil
	}
	if !ok {
		return nil
	}
	inference := activity.InferDay(day, spanFile.Spans, workbenchWorkActivities(workbench))
	if len(inference.UnaccountedBlocks) == 0 {
		return nil
	}
	input := &attention.ActivityDayInput{
		Date:                inference.Date,
		GeneratedAt:         spanFile.GeneratedAt,
		TotalUnaccountedMin: inference.TotalUnaccountedMin,
	}
	for _, block := range inference.UnaccountedBlocks {
		input.UnaccountedBlocks = append(input.UnaccountedBlocks, attention.ActivityBlock{
			Start:       block.Start,
			End:         block.End,
			ActiveMin:   block.ActiveMin,
			DurationMin: block.DurationMin,
			CategoryMix: block.CategoryMix,
		})
	}
	return input
}

// workbenchWorkActivities extracts every envelope/receipt timestamp the workbench already renders —
// claim times plus run-observation and route-outcome times, across open AND recent items — as the
// reconciliation anchors an activity block is "accounted" against (decision 11: work activity for
// slice 1 = timestamps visible in work.WorkbenchReport).
func workbenchWorkActivities(report *work.WorkbenchReport) []activity.WorkActivity {
	if report == nil {
		return nil
	}
	var out []activity.WorkActivity
	add := func(ref, ts string) {
		at, ok := workTimestamp(ts)
		if !ok {
			return
		}
		out = append(out, activity.WorkActivity{Ref: ref, At: at})
	}
	items := make([]work.WorkbenchItem, 0, len(report.Open)+len(report.Recent))
	items = append(items, report.Open...)
	items = append(items, report.Recent...)
	for _, item := range items {
		add(item.WorkID, item.ClaimedAt)
		for _, obs := range item.RunObservations {
			add(item.WorkID+":"+obs.ID, obs.GeneratedAt)
		}
		for _, route := range item.RouteOutcomes {
			add(item.WorkID+":"+route.ID, route.CreatedAt)
		}
	}
	return out
}

// workTimestamp accepts both timestamp encodings the workbench renders: RFC3339 (run observations,
// route outcomes) and integer epoch values (lease claimed_at is epoch NANOSECONDS, not RFC3339 —
// grep-verified premise correction). Mirrors internal/work's unexported executorClaimedAtTime
// (executors.go:783), which is outside this slice's touch boundary.
func workTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, true
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	switch {
	case n > 1e16:
		return time.Unix(0, n), true
	case n > 1e13:
		return time.Unix(0, n*int64(time.Microsecond)), true
	case n > 1e11:
		return time.Unix(0, n*int64(time.Millisecond)), true
	default:
		return time.Unix(n, 0), true
	}
}

func printAttentionText(report attention.Report) {
	printAttentionTextWithReceiptOnlyGrade(report, "")
}

func printAttentionTextWithReceiptOnlyGrade(report attention.Report, receiptOnlyGrade string) {
	fmt.Printf("attention needs_you=%d groups=%d actionable_now=%d watching=%d blocked_on_you=%d fallback_groups=%d\n",
		report.Counts.NeedsYou,
		report.Counts.NeedsYouGroups,
		report.Counts.ActionableNow,
		report.Counts.Watching,
		report.Counts.BlockedOnYou,
		report.Counts.FallbackGroups,
	)
	groups := report.Groups
	receiptOnlyCount := 0
	if receiptOnlyGrade != "" {
		groups = make([]attention.Group, 0, len(report.Groups))
		for _, group := range report.Groups {
			if group.Band == attention.BandNeedsYou && group.RiskGrade == receiptOnlyGrade {
				receiptOnlyCount += group.Count
				continue
			}
			groups = append(groups, group)
		}
	}
	if receiptOnlyCount > 0 {
		fmt.Printf("%d receipt-only (risk_grade %s) — still in the ledger; view with --json\n", receiptOnlyCount, receiptOnlyGrade)
	}
	limit := len(groups)
	if limit > 10 {
		limit = 10
	}
	for i := 0; i < limit; i++ {
		group := groups[i]
		fmt.Printf("- %s %.2f %s/%s count=%d %s",
			attention.Class(group),
			group.Leverage,
			group.Source,
			group.Area,
			group.Count,
			group.OperatorSummary,
		)
		if group.OperatorUnlock != "" {
			fmt.Printf(" unlock=%q", group.OperatorUnlock)
		}
		if group.Action != nil {
			fmt.Printf(" action=%s/%s", group.Action.Kind, group.Action.ID)
		}
		if group.TerminalReason != "" {
			fmt.Printf(" reason=%q", group.TerminalReason)
		}
		fmt.Println()
	}
}
