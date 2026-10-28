package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/mem"
	"compound/internal/retrievalparity"
)

func cmdMemoryImportTopics(store *mem.Store, args []string) {
	path := firstMemoryPos(args, map[string]bool{})
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: compound memory import-topics <topics.json> [--json]")
		os.Exit(2)
	}
	report, err := store.ImportTopicsFile(path, time.Now())
	die(err)
	if hasFlag(args, "--json") {
		printJSON(report)
		return
	}
	fmt.Printf("# memory import-topics %d\n", report.TopicCount)
	fmt.Printf("- source_ref: %s\n", report.SourceRef)
	fmt.Printf("- source_hash: %s\n", report.SourceHash)
	fmt.Printf("- evidence_ref: %s\n", report.EvidenceRef)
	fmt.Printf("- entity_created: %d\n", report.EntityCreated)
	fmt.Printf("- entity_updated: %d\n", report.EntityUpdated)
	fmt.Printf("- entity_reused: %d\n", report.EntityReused)
	fmt.Printf("- edges_created: %d\n", report.EdgesCreated)
	fmt.Printf("- edges_reused: %d\n", report.EdgesReused)
}

func cmdMemoryFold(store *mem.Store, args []string) {
	report, err := store.FoldPositions(mem.PositionFoldOptions{Entity: valueFlag(args, "--entity")}, time.Now())
	die(err)
	if hasFlag(args, "--json") {
		printJSON(report)
		return
	}
	fmt.Printf("# memory fold positions=%d as_of_seq=%d\n", report.Count, report.AsOfSeq)
	for _, p := range report.Positions {
		fmt.Printf("%-28s = %-12s [%s]  support:%d history:%d\n", p.ID, p.ActiveObject, p.Status, len(p.SupportingClaimRefs), len(p.SupersededHistoryRefs))
	}
}

func cmdMemoryPosition(store *mem.Store, args []string) {
	entity := firstMemoryPos(args, map[string]bool{"--as-of": true, "--topic": true})
	if entity == "" {
		fmt.Fprintln(os.Stderr, "usage: compound memory position <entity> [--as-of N] [--topic topic-slug] [--json]")
		os.Exit(2)
	}
	asOf, err := parseMemoryAsOf(args)
	die(err)
	result, err := store.Position(entity, mem.PositionFoldOptions{AsOfSeq: asOf, Topic: valueFlag(args, "--topic")})
	die(err)
	if hasFlag(args, "--json") {
		printJSON(result)
		return
	}
	fmt.Println("# " + result.Coverage)
	if len(result.TopicSubtree) > 0 {
		fmt.Printf("topic_subtree: %s\n", strings.Join(result.TopicSubtree, ", "))
	}
	for _, detail := range result.Positions {
		p := detail.Position
		fmt.Printf("%s [predicate=%s, claim_type=%s] = %s  [%s, claim %s]\n", p.ID, p.Predicate, p.ClaimType, p.ActiveObject, p.Status, p.ActiveClaimID)
		if len(p.SupportingClaimRefs) > 0 {
			fmt.Printf("  supporting: %s\n", strings.Join(p.SupportingClaimRefs, ","))
		}
		if len(p.SupersededHistoryRefs) > 0 {
			fmt.Printf("  superseded: %s\n", strings.Join(p.SupersededHistoryRefs, ","))
		}
		if len(p.ReviewRefs) > 0 {
			fmt.Printf("  review: %s\n", strings.Join(p.ReviewRefs, ","))
		}
		for _, claim := range detail.Claims {
			fmt.Printf("  claim %s seq=%d %s.%s=%s evidence=%s\n", claim.ID, claim.Seq, claim.Entity, claim.Predicate, claim.Object, strings.Join(claim.EvidenceRefs, ","))
		}
		for _, evidence := range detail.Evidence {
			fmt.Printf("  evidence %s source=%s:%s\n", evidence.ID, evidence.SourceKind, evidence.SourceID)
		}
	}
}

func cmdMemoryRecall(homePath string, store *mem.Store, args []string) {
	asOf, err := parseMemoryAsOf(args)
	die(err)
	entity := firstMemoryPos(args, map[string]bool{"--as-of": true, "--topic": true})
	retrievalStarted := time.Now()
	rr, err := store.Recall(entity, mem.RecallOpts{
		AsOfSeq:            asOf,
		IncludeRetired:     hasFlag(args, "--include-retired"),
		IncludeNeedsReview: hasFlag(args, "--include-needs-review"),
		IncludeEvidence:    hasFlag(args, "--include-evidence"),
		Topic:              valueFlag(args, "--topic"),
	})
	retrievalparity.Record(homePath, retrievalparity.NewRow(retrievalparity.ConsumerRecall, entity, parityRecallRefs(rr), time.Since(retrievalStarted), err == nil, time.Now()))
	die(err)
	if hasFlag(args, "--json") {
		printJSON(rr)
		return
	}
	fmt.Println("# " + rr.Coverage)
	if len(rr.TopicSubtree) > 0 {
		fmt.Printf("topic_subtree: %s\n", strings.Join(rr.TopicSubtree, ", "))
	}
	for _, c := range rr.Claims {
		fmt.Printf("%s.%s = %s  (seq %d, since %s, %s, conf %.2f)\n", c.Entity, c.Predicate, c.Object, c.Seq, c.ValidFrom, c.Derivation, c.Confidence)
	}
	for _, c := range rr.EvidenceCandidates {
		fmt.Printf("evidence %s.%s = %s (%s, conf %.2f, annotations %d)\n", c.Entity, c.Predicate, c.Object, c.State, c.Confidence, len(c.Annotations))
		for _, ann := range c.Annotations {
			fmt.Printf(" annotation %s decision=%s original=%s confidence=%.2f run=%s verdict=%s\n", ann.ID, ann.Decision, ann.OriginalDecision, ann.Confidence, ann.RunID, ann.VerdictArtifact)
		}
	}
	// `recall <name>` is EXACT/as-of recall over a single entity. A multi-word natural-language
	// arg is almost always meant as a free-text question — point it at the breadth engine so the
	// verb split isn't a silent footgun (recall returns nothing where query would answer).
	if len(rr.Claims) == 0 && len(rr.EvidenceCandidates) == 0 && strings.ContainsAny(strings.TrimSpace(entity), " ") {
		fmt.Printf("# hint: `recall` matches one entity by name; for a free-text/semantic question use `compound memory query %q`\n", entity)
	}
}

func cmdMemoryBakeoff(store *mem.Store, args []string, engineFactory func() (mem.Engine, func())) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound memory bakeoff <init|seed|run --queries file>")
		os.Exit(2)
	}
	switch args[0] {
	case "init":
		path := valueFlag(args[1:], "--queries")
		if path == "" {
			path = filepath.Join("fixtures", "bakeoff", "queries.yaml")
		}
		report, err := mem.WriteDefaultBakeoffQueries(path, hasFlag(args[1:], "--force"))
		die(err)
		if hasFlag(args[1:], "--json") {
			printJSON(report)
			return
		}
		fmt.Println("# memory bakeoff init")
		fmt.Printf("- path: %s\n", report.Path)
		fmt.Printf("- created: %t\n", report.Created)
		fmt.Printf("- queries: %d\n", report.QueryCount)
		fmt.Printf("- lanes: %s\n", strings.Join(report.Lanes, ","))
	case "seed":
		// Writes the three fixture-shaped claims (claim_memory_spine,
		// claim_reference_engine, claim_no_mutation) through the trusted
		// adapter door so a freshly-initialized home can run bakeoff end-to-end.
		// Idempotent: re-seeding on an already-seeded home reports
		// already_seeded and writes nothing new.
		rest := args[1:]
		if err := validateKnownFlags(rest, nil, map[string]bool{"--json": true}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, "usage: compound memory bakeoff seed [--json]")
			os.Exit(2)
		}
		report, err := store.SeedBakeoffClaims(time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory bakeoff seed:", err)
			os.Exit(1)
		}
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Println("# memory bakeoff seed")
		fmt.Printf("- schema_version: %s\n", report.SchemaVersion)
		fmt.Printf("- seeded: %d\n", len(report.Seeded))
		for _, row := range report.Seeded {
			fmt.Printf("  %s evidence=%s created=%t\n", row.ClaimID, row.EvidenceID, row.Created)
		}
		fmt.Printf("- already_seeded: %d\n", len(report.AlreadySeeded))
		for _, id := range report.AlreadySeeded {
			fmt.Printf("  %s\n", id)
		}
		if len(report.Errors) > 0 {
			fmt.Fprintf(os.Stderr, "- errors: %s\n", strings.Join(report.Errors, "; "))
		}
	case "run":
		rest := args[1:]
		queriesPath := valueFlag(rest, "--queries")
		if queriesPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound memory bakeoff run --queries <file> [--engine <bin>] [--k N] [--json]")
			os.Exit(2)
		}
		k := 10
		if raw := valueFlag(rest, "--k"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				fmt.Fprintln(os.Stderr, "error: --k must be a positive integer")
				os.Exit(2)
			}
			k = n
		}
		var restore func()
		if bin := valueFlag(rest, "--engine"); bin != "" {
			restore = captureEnv("COMPOUND_MEM_ENGINE_BIN", "COMPOUND_MEM_ENGINE_EMBEDDER")
			if err := os.Setenv("COMPOUND_MEM_ENGINE_BIN", bin); err != nil {
				die(err)
			}
			if embedder := valueFlag(rest, "--embedder"); embedder != "" {
				if err := os.Setenv("COMPOUND_MEM_ENGINE_EMBEDDER", embedder); err != nil {
					restore()
					die(err)
				}
			}
		} else {
			restore = func() {}
		}
		eng, closeEng := engineFactory()
		report, err := store.RunBakeoff(mem.BakeoffRunOptions{
			QueriesPath:    queriesPath,
			K:              k,
			Candidate:      eng,
			AllowExpansion: true,
			Now:            time.Now(),
		})
		closeEng()
		restore()
		if hasFlag(rest, "--json") {
			printJSON(report)
			if err != nil {
				os.Exit(1)
			}
			return
		}
		fmt.Println("# memory bakeoff run")
		fmt.Printf("- report: %s\n", report.ReportPath)
		fmt.Printf("- queries: %d\n", report.QueryCount)
		fmt.Printf("- candidate_engine: %s\n", report.CandidateEngine)
		fmt.Printf("- recall_at_5: %.3f\n", report.Metrics.RecallAt5)
		fmt.Printf("- recall_at_10: %.3f\n", report.Metrics.RecallAt10)
		fmt.Printf("- mrr: %.3f\n", report.Metrics.MRR)
		fmt.Printf("- reference_covered: %.3f\n", report.Metrics.ReferenceCovered)
		fmt.Printf("- latency_p50_ms: %.3f\n", report.Metrics.LatencyP50MS)
		fmt.Printf("- latency_p95_ms: %.3f\n", report.Metrics.LatencyP95MS)
		fmt.Printf("- ledger_digest_unchanged: %t\n", report.LedgerDigestUnchanged)
		fmt.Printf("- determinism: %t\n", report.Determinism.Passed)
		printMemoryBakeoffClinic(report.Clinic)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory bakeoff:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: compound memory bakeoff <init|seed|run --queries file>")
		os.Exit(2)
	}
}

func printMemoryBakeoffClinic(clinic mem.BakeoffClinicReport) {
	for i, cause := range clinic.TopCauses {
		fmt.Printf("- clinic_cause_%d: %s (%d)\n", i+1, cause.Cause, cause.Count)
		fmt.Printf("- clinic_evidence_%d: %s\n", i+1, strings.Join(cause.EvidenceRefs, ","))
	}
	for _, fix := range clinic.RecommendedFixes {
		fmt.Printf("- recommended_fix: %s\n", fix)
	}
}

func parseMemoryAsOf(args []string) (int64, error) {
	raw := valueFlag(args, "--as-of")
	if raw == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("--as-of must be a non-negative ledger seq")
	}
	return seq, nil
}

func firstMemoryPos(args []string, valueFlags map[string]bool) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if valueFlags[arg] {
				i++
			}
			continue
		}
		return arg
	}
	return ""
}

// cmdMemoryTemperature is the read/rebuild door onto the memory-temperature projection (Weng
// item 5): the derived reinforcement score (consumed reinforces, ignored cools, decayed by
// recency) that feeds ambient recall ranking as a bounded additive term. --rebuild joins the
// capability-usage projection against current claims and writes
// projections/memory-temperature.json (also the lander's live-artifact step); the default read
// renders a summary of the last-built projection.
func cmdMemoryTemperature(homePath string, args []string) {
	wantJSON := hasFlag(args, "--json")
	if hasFlag(args, "--rebuild") {
		proj, err := mem.BuildTemperature(homePath, mem.TemperatureBuildOptions{Now: time.Now()})
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		path, err := mem.WriteTemperature(homePath, proj)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if wantJSON {
			printJSON(proj)
			return
		}
		fmt.Println("# memory temperature (rebuilt)")
		fmt.Printf("- wrote: %s\n", path)
		printTemperatureSummary(proj)
		return
	}

	proj, ok, err := mem.ReadTemperature(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if !ok {
		if wantJSON {
			printJSON(mem.TemperatureProjection{
				SchemaVersion: mem.TemperatureSchemaVersion,
				ProjectionRef: mem.TemperatureProjectionRef,
				Entries:       []mem.TemperatureEntry{},
			})
			return
		}
		fmt.Println("# memory temperature")
		fmt.Println("- projection not built yet; run `compound memory temperature --rebuild`")
		return
	}
	if wantJSON {
		printJSON(proj)
		return
	}
	fmt.Println("# memory temperature")
	printTemperatureSummary(proj)
}

// printTemperatureSummary renders the human view: computed_at, entry count, and the warmest N
// entries (temperature desc, then claim_id) with their consumed/ignored reinforcement counts.
func printTemperatureSummary(proj mem.TemperatureProjection) {
	fmt.Printf("- computed_at: %s\n", proj.ComputedAt)
	fmt.Printf("- entries: %d\n", len(proj.Entries))
	if len(proj.Entries) == 0 {
		return
	}
	warmest := append([]mem.TemperatureEntry(nil), proj.Entries...)
	sort.Slice(warmest, func(i, j int) bool {
		if warmest[i].Temperature != warmest[j].Temperature {
			return warmest[i].Temperature > warmest[j].Temperature
		}
		return warmest[i].ClaimID < warmest[j].ClaimID
	})
	const topN = 10
	if len(warmest) > topN {
		warmest = warmest[:topN]
	}
	fmt.Printf("- warmest %d:\n", len(warmest))
	for _, e := range warmest {
		fmt.Printf("  %+.4f  %s  (consumed %d / ignored %d)\n", e.Temperature, e.ClaimID, e.Consumed, e.Ignored)
	}
}

// cmdMemoryObservation is the read-only door onto the memory-injection observation
// report (work_7df249b46755): it classifies every observed session (a closed work's
// receipt that carries a context_pack_id) by memory-injection condition — on/off/reduced
// — purely from what its ContextPack already recorded, and reads consumption by joining
// each receipt's own used-skills set against the claims its pack served. See
// internal/capgraph/injection_observation.go for the condition discriminator and report
// math; this function only sweeps (never mutates) and renders. --record persists the
// report as a plain timestamped artifact under $COMPOUND_HOME/state/runs/
// memory-injection-observation/ — the runtime home, never the repo, never projections/.
// The `legibility` form renders the GLANCEABLE per-turn meter (the operator-legibility
// half): latest ContextPack + its rendered injection, in one line and --json.
func cmdMemoryObservation(homePath string, args []string) {
	if len(args) > 0 && args[0] == "legibility" {
		cmdMemoryLegibility(homePath, args[1:])
		return
	}
	wantJSON := hasFlag(args, "--json")
	rep, err := capgraph.BuildInjectionObservationReport(homePath, capgraph.InjectionObservationOptions{GeneratedAt: time.Now()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if hasFlag(args, "--record") {
		path, err := capgraph.WriteInjectionObservationRun(homePath, rep, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "recorded: %s\n", path)
	}
	if wantJSON {
		printJSON(rep)
		return
	}
	printInjectionObservationSummary(rep)
}

// printInjectionObservationSummary renders the human view: the discriminator (so the
// reader never has to trust an out-of-band description of on/off/reduced), the
// per-condition rollup the packet's report-content spec requires, the explicit f2 line,
// and a per-session table (session/receipt id, condition, served, consumed).
// cmdMemoryLegibility renders the glanceable per-turn injection meter: the latest
// ContextPack's served/excluded/memory/positions/offers plus the rendered injection
// size, in one line. Read-only, best-effort (a home that never oriented says so).
func cmdMemoryLegibility(homePath string, args []string) {
	leg, err := capgraph.LatestInjectionLegibility(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if hasFlag(args, "--json") {
		printJSON(leg)
		return
	}
	fmt.Println(leg.Glance())
}

func printInjectionObservationSummary(rep capgraph.InjectionObservationReport) {
	fmt.Println("# memory injection observation")
	fmt.Printf("- discriminator: %s\n", rep.Discriminator)
	fmt.Printf("- generated_at: %s\n", rep.GeneratedAt)
	fmt.Printf("- receipts_scanned: %d  with_context_pack: %d  context_packs_missing: %d\n", rep.ReceiptsScanned, rep.ReceiptsWithContextPack, rep.ContextPacksMissing)
	fmt.Printf("- f2 (consumed_count=0 across ALL on-condition sessions): %s — %s\n", yesNo(rep.F2AllZero), rep.F2Note)
	fmt.Println()
	for _, c := range rep.Conditions {
		fmt.Printf("## condition=%s\n", c.Condition)
		fmt.Printf("- sessions: %d\n", c.SessionCount)
		fmt.Printf("- served_claim_events: %d  distinct_served_claims: %d\n", c.ServedClaimEvents, c.DistinctServedClaims)
		fmt.Printf("- consumed_claim_events: %d\n", c.ConsumedClaimEvents)
		fmt.Printf("- consumed_count_distribution: 0=%d 1=%d 2=%d 3+=%d\n",
			c.ConsumedCountDistribution["0"], c.ConsumedCountDistribution["1"], c.ConsumedCountDistribution["2"], c.ConsumedCountDistribution["3+"])
		fmt.Printf("- cold_served: %d/%d (ratio %.2f)\n", c.ColdServedCount, c.DistinctServedClaims, c.ColdServedRatio)
		if len(c.HotClaims) == 0 {
			fmt.Println("- hot_claims: (none)")
		} else {
			fmt.Println("- hot_claims:")
			for _, hc := range c.HotClaims {
				fmt.Printf("    %-4d %s\n", hc.Count, hc.ClaimID)
			}
		}
		fmt.Println()
	}
	if len(rep.Sessions) == 0 {
		fmt.Println("- sessions: (none observed)")
		return
	}
	fmt.Println("## sessions")
	fmt.Printf("%-16s %-8s %6s %8s  %s\n", "receipt_id", "cond", "served", "consumed", "work_id")
	for _, s := range rep.Sessions {
		fmt.Printf("%-16s %-8s %6d %8d  %s\n", s.ReceiptID, s.Condition, s.ServedCount, s.ConsumedCount, s.WorkID)
	}
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}

// cmdMemoryCanon is the read-only door onto the canon-class taxonomy and live
// distribution of EffectiveCanonClass over current claims. Zero ledger writes.
func cmdMemoryCanon(store *mem.Store, args []string) {
	wantJSON := hasFlag(args, "--json")
	rep, err := store.CanonDistribution()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if wantJSON {
		printJSON(rep)
		return
	}
	fmt.Println("# memory canon")
	fmt.Printf("- vocabulary: %s\n", strings.Join(rep.Vocabulary, ", "))
	fmt.Println("- claim_type mapping:")
	for _, ct := range mem.ClaimTypes {
		fmt.Printf("  - %s → %s\n", ct, rep.Mapping[ct])
	}
	fmt.Println("- risk tiers:")
	for _, class := range append(append([]string{}, rep.Vocabulary...), mem.CanonClassUnclassified) {
		fmt.Printf("  - %s → %s\n", class, rep.Tiers[class])
	}
	fmt.Printf("- total current claims: %d\n", rep.Total)
	fmt.Println("- distribution:")
	// Stable order: vocabulary then unclassified.
	for _, class := range append(append([]string{}, rep.Vocabulary...), mem.CanonClassUnclassified) {
		if n := rep.Distribution[class]; n > 0 {
			fmt.Printf("  - %s: %d\n", class, n)
		}
	}
}
