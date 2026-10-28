package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/routeevidence"
)

// tryRouteSpeedCommand handles `compound route speed ...`: the file/stdin door for measured
// lane-speed probe RESULTS and a read-only view of the latest measurement per lane tuple. It
// never makes a provider or network call.
func tryRouteSpeedCommand(args []string, homePath string) bool {
	if len(args) == 0 || args[0] != "speed" {
		return false
	}
	sub := ""
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub = rest[0]
		rest = rest[1:]
	}
	switch sub {
	case "observe":
		runRouteSpeedObserve(rest, homePath)
	case "", "list":
		runRouteSpeedList(rest, homePath)
	default:
		fmt.Fprintln(os.Stderr, routeSpeedUsage)
		os.Exit(2)
	}
	return true
}

const routeSpeedUsage = "usage: compound route speed (observe [--from <probe.json>] [--json] | list [--json])"

func runRouteSpeedObserve(args []string, homePath string) {
	from := flagValue(args, "--from")
	var (
		data []byte
		err  error
	)
	if strings.TrimSpace(from) != "" {
		data, err = os.ReadFile(from)
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "route speed observe:", err)
		os.Exit(1)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		fmt.Fprintln(os.Stderr, "route speed observe: no probe data (pass --from <file> or pipe JSON on stdin)")
		os.Exit(2)
	}
	probe, wrote, err := routeevidence.IngestLaneSpeedProbe(homePath, data, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "route speed observe:", err)
		os.Exit(1)
	}
	measured := routeevidence.MeasuredLatencyClass(probe.TokensPerSec, probe.TTFTms, mustLatencyBudget(homePath))
	if hasFlag(args, "--json") {
		printJSON(map[string]any{
			"probe":                  probe,
			"recorded":               wrote,
			"measured_latency_class": string(measured),
		})
		return
	}
	fmt.Println("# route speed observe")
	fmt.Printf("- probe_id: %s\n", probe.ID)
	fmt.Printf("- lane_tuple_id: %s\n", probe.LaneTupleID)
	if probe.LaneRef != "" {
		fmt.Printf("- lane_ref: %s\n", probe.LaneRef)
	}
	fmt.Printf("- tokens_per_sec: %.2f\n", probe.TokensPerSec)
	fmt.Printf("- ttft_ms: %d\n", probe.TTFTms)
	fmt.Printf("- measured_latency_class: %s\n", measured)
	fmt.Printf("- recorded: %t\n", wrote)
	fmt.Println("- verdict: measured_route_evidence_not_task_success")
}

func runRouteSpeedList(args []string, homePath string) {
	latest, err := routeevidence.LatestLaneSpeedByTuple(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "route speed list:", err)
		os.Exit(1)
	}
	budget := mustLatencyBudget(homePath)
	if hasFlag(args, "--json") {
		printJSON(latest)
		return
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println("# route speed")
	fmt.Printf("- authority: %s\n", routeevidence.LaneSpeedAuthorityRef)
	fmt.Printf("- lane_tuples: %d\n", len(keys))
	for _, k := range keys {
		p := latest[k]
		measured := routeevidence.MeasuredLatencyClass(p.TokensPerSec, p.TTFTms, budget)
		fmt.Printf("- lane: %s tok/s=%.2f ttft_ms=%d measured=%s measured_at=%s source=%s\n",
			k, p.TokensPerSec, p.TTFTms, measured, emptyDash(p.MeasuredAt), emptyDash(p.Source))
	}
	fmt.Println("- verdict: metadata_only_not_task_success")
}

func mustLatencyBudget(homePath string) routeevidence.LatencyBudget {
	budget, _, err := routeevidence.LoadLatencyBudget(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "route speed:", err)
		os.Exit(1)
	}
	return budget
}
