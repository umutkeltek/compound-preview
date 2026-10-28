package main

import (
	"fmt"
	"os"
	"sort"

	"compound/internal/home"
	"compound/internal/lanebeliefs"
)

func sortedReasonKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func laneBeliefsUsage() string {
	return "usage: compound lanebeliefs <fold [--dry-run|--experimental] [--json] [--home P]|promote [--dry-run|--experimental] [--json] [--home P]|cool [--dry-run|--experimental] [--json] [--home P]|list [--json] [--home P]>"
}

func cmdLaneBeliefs(args []string) {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(os.Stderr, laneBeliefsUsage())
		os.Exit(2)
	}
	sub := args[0]
	jsonOut := false
	dryRun := false
	experimental := false
	homeFlag := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--dry-run":
			dryRun = true
		case "--experimental":
			experimental = true
		case "--home":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "lanebeliefs: --home requires a path")
				os.Exit(2)
			}
			homeFlag = args[i+1]
			i++
		default:
			fmt.Fprintln(os.Stderr, laneBeliefsUsage())
			os.Exit(2)
		}
	}
	if !dryRun && !experimental && (sub == "fold" || sub == "promote" || sub == "cool") {
		fmt.Fprintf(os.Stderr, "lanebeliefs %s: mutation is experimental; pass --experimental\n", sub)
		os.Exit(2)
	}
	homePath, err := home.Resolve(homeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lanebeliefs:", err)
		os.Exit(1)
	}
	switch sub {
	case "fold":
		report, err := lanebeliefs.Fold(homePath, lanebeliefs.FoldOptions{DryRun: dryRun})
		if err != nil {
			fmt.Fprintln(os.Stderr, "lanebeliefs fold:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# lanebeliefs fold\n")
		fmt.Printf("- raw_receipts: %d\n", report.RawReceiptCount)
		fmt.Printf("- observations: %d\n", report.NewObservationCount)
		fmt.Printf("- candidates: %d\n", report.CandidateCount)
		fmt.Printf("- unresolved_receipts: %d\n", report.UnresolvedReceiptCount)
		fmt.Printf("- below_floor_lanes: %d\n", report.BelowFloorLaneCount)
		fmt.Printf("- valve_dropped: %d\n", report.ValveDroppedCount)
		for _, drop := range report.ValveDrops {
			fmt.Printf("  - dropped %s %s: %s\n", drop.Kind, drop.Domain, drop.Reason)
		}
		fmt.Printf("- cursor: %d -> %d\n", report.CursorBefore, report.CursorAfter)
	case "promote":
		report, err := lanebeliefs.Promote(homePath, lanebeliefs.PromoteOptions{DryRun: dryRun})
		if err != nil {
			fmt.Fprintln(os.Stderr, "lanebeliefs promote:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# lanebeliefs promote\n")
		if report.DryRun {
			fmt.Printf("- dry_run: true (no writes)\n")
		}
		fmt.Printf("- evaluated: %d\n", report.EvaluatedCount)
		fmt.Printf("- promoted: %d\n", report.PromotedCount)
		fmt.Printf("- refused: %d\n", report.RefusedCount)
		for _, reason := range sortedReasonKeys(report.ReasonCounts) {
			fmt.Printf("  - %s: %d\n", reason, report.ReasonCounts[reason])
		}
		for _, res := range report.Results {
			if res.Disposition == lanebeliefs.DispositionPromoted {
				fmt.Printf("%s %s.%s[%s] %s -> promoted basis=%s visibility=%s\n",
					res.CandidateID, res.Subject, res.Domain, res.Valence, res.Kind, res.PromotionBasis, res.Visibility)
			} else {
				fmt.Printf("%s %s.%s[%s] %s -> parked (%s) works=%d posterior=%.4f mark=%t\n",
					res.CandidateID, res.Subject, res.Domain, res.Valence, res.Kind, res.Reason, res.WorkCount, res.PosteriorMean, res.HasCrossLaneMark)
			}
		}
	case "cool":
		report, err := lanebeliefs.Cool(homePath, lanebeliefs.CoolOptions{DryRun: dryRun})
		if err != nil {
			fmt.Fprintln(os.Stderr, "lanebeliefs cool:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# lanebeliefs cool\n")
		if report.DryRun {
			fmt.Printf("- dry_run: true (no writes)\n")
		}
		fmt.Printf("- run_count: %d (cool_after_folds=%d)\n", report.RunCount, report.CoolAfterFolds)
		fmt.Printf("- evaluated: %d\n", report.EvaluatedCount)
		fmt.Printf("- cooled: %d\n", report.CooledCount)
		fmt.Printf("- skipped: %d\n", report.SkippedCount)
		for _, res := range report.Results {
			if res.Disposition == lanebeliefs.CoolDispositionCooled {
				fmt.Printf("%s %s.%s[%s] observed -> cooled (%s) quiet=%d claim=%s basis=%s visibility=%s\n",
					res.ObservedClaimID, res.Subject, res.Domain, res.Valence, res.Reason, res.QuietFolds, res.CooledClaimID, res.PromotionBasis, res.Visibility)
			} else {
				fmt.Printf("%s %s.%s[%s] observed -> kept (%s) quiet=%d last_reinforced=%d\n",
					res.ObservedClaimID, res.Subject, res.Domain, res.Valence, res.Reason, res.QuietFolds, res.LastReinforcedRun)
			}
		}
	case "list":
		report, err := lanebeliefs.List(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "lanebeliefs list:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# lanebeliefs list\n")
		fmt.Printf("- candidates: %d\n", report.CandidateCount)
		for _, candidate := range report.Candidates {
			fmt.Printf("%s %s.%s=%s state=%s\n", candidate.ID, candidate.Entity, candidate.Predicate, candidate.Object, candidate.State)
		}
	default:
		fmt.Fprintln(os.Stderr, laneBeliefsUsage())
		os.Exit(2)
	}
}
