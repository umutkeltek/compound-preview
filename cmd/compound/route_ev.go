package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/lanebeliefs"
	"compound/internal/routeev"
	"compound/internal/routeevidence"
)

// routeEvUsage is the S2 CLI surface (spec docs_private/plan/ev-card-spec.md §7): price a
// deviation lane against the served policy line in one commensurable unit (expected
// cost-to-success). --class-bucket unknown is the S0-named escape hatch: most completion
// evidence today sits under capability class "unknown" (S0 finding), so the default read is
// exact-class-or-missing; this flag makes an explicit, visible fallback to the "unknown" bucket
// instead of a silent one.
const routeEvUsage = "usage: compound route ev --capability-class <class> --deviation-lane <lane> [--risk-grade gate|judge|tidy] [--class-bucket unknown] [--json]"

// tryRouteEvCommand handles `compound route ev ...`. It never makes a provider or network call
// and never mutates model-router, gateway, or provider config.
func tryRouteEvCommand(args []string, homePath string) bool {
	if len(args) == 0 || args[0] != "ev" {
		return false
	}
	runRouteEv(args[1:], homePath)
	return true
}

func runRouteEv(args []string, homePath string) {
	capabilityClass := strings.TrimSpace(valueFlag(args, "--capability-class"))
	deviationLane := strings.TrimSpace(valueFlag(args, "--deviation-lane"))
	riskGrade := strings.TrimSpace(valueFlag(args, "--risk-grade"))
	classBucket := strings.TrimSpace(valueFlag(args, "--class-bucket"))
	jsonOut := hasFlag(args, "--json")

	if capabilityClass == "" || deviationLane == "" {
		fmt.Fprintln(os.Stderr, routeEvUsage)
		os.Exit(2)
	}
	switch riskGrade {
	case "", "gate", "judge", "tidy":
	default:
		fmt.Fprintln(os.Stderr, "route ev: --risk-grade must be gate|judge|tidy (empty allowed; derivation is S3's business, this is pass-through)")
		os.Exit(2)
	}
	if classBucket != "" && classBucket != "unknown" {
		fmt.Fprintln(os.Stderr, `route ev: --class-bucket only accepts "unknown" (the S0 escape hatch)`)
		os.Exit(2)
	}

	now := time.Now()

	// POLICY LINE resolution: the SAME code path `compound route plan --capability-class`
	// calls, never a second routing opinion (depth test: route ev and route plan must resolve
	// the identical policy lane against one fixture).
	plan, err := routeevidence.PlanCapabilityRoute(homePath, capabilityClass, routeevidence.CapabilityRoutePlanOptions{}, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "route ev:", err)
		os.Exit(1)
	}

	var policyLaneRef string
	if len(plan.Chain) > 0 {
		policyLaneRef = plan.Chain[0].ModelID
	}

	policyLine := resolveEvLaneInput(homePath, policyLaneRef, capabilityClass, classBucket)
	deviation := resolveEvLaneInput(homePath, deviationLane, capabilityClass, classBucket)

	computedFromRevision := plan.SourceRevision
	if computedFromRevision == "" {
		computedFromRevision = plan.AuthorityPath
	}

	card := routeev.Compose(routeev.ComposeInput{
		CapabilityClass:      capabilityClass,
		PolicyLine:           policyLine,
		Deviation:            deviation,
		RiskGrade:            riskGrade,
		RoutePlanRef:         plan.ID,
		ComputedFromRevision: computedFromRevision,
	})

	// A refusing (unpriceable) card is a successful computation: it is stored and printed like
	// any other card, never treated as a command failure (exit code stays 0).
	ref, path, err := routeev.Store(homePath, card)
	if err != nil {
		fmt.Fprintln(os.Stderr, "route ev:", err)
		os.Exit(1)
	}

	if jsonOut {
		printJSON(map[string]any{
			"card": card,
			"ref":  ref,
			"path": path,
		})
		return
	}
	printRouteEvCard(card, ref, path)
}

// resolveEvLaneInput resolves ONE lane's priced evidence from the real doors (provider-ledger
// cost, lanebeliefs completion posterior) and returns it as a routeev.LaneInput. This is the
// only place in `compound route ev` (and, by card.go's own package doc, the only place at all)
// that touches those doors — internal/routeev never imports them, so a wiring mistake here can
// never make the composer develop a second belief/routing opinion of its own.
func resolveEvLaneInput(homePath, laneRef, capabilityClass, classBucket string) routeev.LaneInput {
	in := routeev.LaneInput{LaneID: laneRef}
	if strings.TrimSpace(laneRef) == "" {
		return in
	}

	if cost, window, err := laneCostPerAttempt(homePath, laneRef); err == nil && cost != nil {
		in.CostPerAttempt = cost
		in.LedgerWindow = window
	}

	nodeID, resolved, err := resolveEvLaneNodeID(homePath, laneRef)
	if err != nil || !resolved {
		return in
	}

	report, err := lanebeliefs.CompletionPosterior(homePath, nodeID, capabilityClass)
	if err == nil && report.MeetsFloor {
		mean := report.PosteriorMean
		in.Posterior = &mean
		in.Attempts = report.Attempts
		in.Completions = report.Completions
		in.BeliefRefs = append(in.BeliefRefs, report.ObservationRefs...)
		return in
	}

	// The requested class has no cell today: that is a missing input, not an excuse to
	// silently fall back to "unknown" (S0 finding: most completion evidence sits there).
	// --class-bucket unknown is the explicit, visible escape hatch — it never fires unless
	// the operator asked for it, and it always leaves a marker in belief_refs so the card
	// never reads as knowledge about the REQUESTED class.
	if classBucket == "unknown" && capabilityClass != "unknown" {
		fallback, ferr := lanebeliefs.CompletionPosterior(homePath, nodeID, "unknown")
		if ferr == nil && fallback.MeetsFloor {
			mean := fallback.PosteriorMean
			in.Posterior = &mean
			in.Attempts = fallback.Attempts
			in.Completions = fallback.Completions
			in.BeliefRefs = append(in.BeliefRefs, fallback.ObservationRefs...)
			in.BeliefRefs = append(in.BeliefRefs, "posterior_class_bucket:unknown")
		}
	}
	return in
}

// resolveEvLaneNodeID resolves a raw lane ref to its canonical agent-lane node id through the
// SAME B0 door the lanebeliefs fold uses (capgraph.Store.ResolveLaneRef) — never a second,
// hand-rolled lane identity. A ref that names no imported agent-lane node is zero evidence,
// never a guess (mirrors internal/lanebeliefs's openLaneResolver: a home with no capability
// graph resolves nothing rather than fabricating a node).
func resolveEvLaneNodeID(homePath, laneRef string) (string, bool, error) {
	if !capgraph.Exists(homePath) {
		return "", false, nil
	}
	store, err := capgraph.OpenReadOnly(homePath)
	if err != nil {
		return "", false, err
	}
	nodeID, ok := store.ResolveLaneRef(laneRef)
	return nodeID, ok, nil
}

// laneCostPerAttempt reuses the SAME provider-ledger evidence the route cost-doctor /
// import-provider-ledger machinery populates (routeevidence.LoadRouteEvidenceLedger reads the
// exact ledger those commands write into) and aggregates it into one cost_per_attempt for a
// lane ref: the mean CostMicros (USD) over every recorded evidence row whose selected target or
// provider-ledger route alias exactly names this lane. It never fuzzy-matches — an unmatched
// lane is a missing input (nil), never a guessed cost. The window is the full imported ledger
// (no --tail/--limit choice for S2; a caller wanting a narrower window re-imports a narrower
// slice), named so the card's provenance is never a silent number.
func laneCostPerAttempt(homePath, laneRef string) (*float64, string, error) {
	rows, exists, err := routeevidence.LoadRouteEvidenceLedger(homePath)
	if err != nil {
		return nil, "", err
	}
	if !exists {
		return nil, "", nil
	}
	var total int64
	var count int
	for _, ev := range rows {
		if !laneEvidenceMatches(ev, laneRef) {
			continue
		}
		total += ev.CostMicros
		count++
	}
	if count == 0 {
		return nil, "", nil
	}
	mean := (float64(total) / float64(count)) / 1_000_000
	window := fmt.Sprintf("ledger:full n=%d", count)
	return &mean, window, nil
}

// laneEvidenceMatches is an EXACT, never-fuzzy join between a route-plan/CLI lane ref and one
// recorded ExecutionRouteEvidence row: the resolved model name, the resolved provider/model
// pair, or the original provider-ledger route_alias (carried in evidence_refs as
// "provider-ledger-route:<alias>"). Anything else does not match — a lossy join that guessed
// would be worse than a card that refuses to price.
func laneEvidenceMatches(ev routeevidence.Evidence, laneRef string) bool {
	laneRef = strings.TrimSpace(laneRef)
	if laneRef == "" {
		return false
	}
	if ev.SelectedTarget.Model == laneRef {
		return true
	}
	if ev.SelectedTarget.Provider != "" && ev.SelectedTarget.Provider+"/"+ev.SelectedTarget.Model == laneRef {
		return true
	}
	for _, ref := range ev.EvidenceRefs {
		if ref == "provider-ledger-route:"+laneRef {
			return true
		}
	}
	return false
}

func printRouteEvCard(card routeev.Card, ref, path string) {
	fmt.Println("# route ev")
	fmt.Printf("- capability_class: %s\n", card.CapabilityClass)
	fmt.Printf("- policy_line: %s\n", laneCardSummary(card.PolicyLine))
	fmt.Printf("- deviation: %s\n", laneCardSummary(card.Deviation))
	if card.RequiredPosterior != nil {
		fmt.Printf("- required_posterior: %.4f\n", *card.RequiredPosterior)
	}
	fmt.Printf("- verdict: %s\n", card.Verdict)
	if len(card.MissingInputs) > 0 {
		fmt.Printf("- missing_inputs: %s\n", strings.Join(card.MissingInputs, ", "))
	}
	if card.RiskGrade != "" {
		fmt.Printf("- risk_grade: %s\n", card.RiskGrade)
	}
	fmt.Printf("- prediction_required: %t\n", card.PredictionRequired)
	if card.RoutePlanRef != "" {
		fmt.Printf("- route_plan_ref: %s\n", card.RoutePlanRef)
	}
	if card.ComputedFromRevision != "" {
		fmt.Printf("- computed_from_revision: %s\n", card.ComputedFromRevision)
	}
	fmt.Printf("- ref: %s\n", ref)
	fmt.Printf("- stored: %s\n", path)
	fmt.Println("- advisory: evidence attached to the ActionIntent; never auto-blocks in v0")
}

func laneCardSummary(lane routeev.LaneCard) string {
	laneID := lane.LaneID
	if laneID == "" {
		laneID = "(none)"
	}
	parts := []string{laneID}
	if lane.EV != nil {
		parts = append(parts, fmt.Sprintf("ev=%.4f", *lane.EV))
	}
	if lane.CostPerAttempt != nil {
		parts = append(parts, fmt.Sprintf("cost_per_attempt=%.4f", *lane.CostPerAttempt))
	} else {
		parts = append(parts, "cost_per_attempt=missing")
	}
	if lane.Posterior != nil {
		parts = append(parts, fmt.Sprintf("posterior=%.4f", *lane.Posterior))
	} else {
		parts = append(parts, "posterior=missing")
	}
	parts = append(parts, fmt.Sprintf("(attempts=%d completions=%d)", lane.Attempts, lane.Completions))
	return strings.Join(parts, " ")
}
