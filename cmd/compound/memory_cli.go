// memory_cli.go owns the `compound memory ...` verb dispatcher (cmdMemory) and its help-text
// helper (memoryUsage). Extracted from cmd/compound/main.go so main.go holds only the
// cross-cutting dispatch table + non-memory verbs. The memory engine config subsystem
// (memBreadthEngine, memoryEngineConfig*, applyEmbedConfigEnv, captureEnv, …) is shared
// with cmd/compound/memory_parity_replay.go and stays in main.go until a focused
// memory_engine_config.go extraction lands.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/doctor/exitcode"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/promotedriver"
	"compound/internal/retrievalparity"
)

func parseMemoryAfterCursor(value string) (candidates.EntryCursor, error) {
	if value == "" {
		return "", nil
	}
	return candidates.ParseEntryCursor(value)
}

func memoryUsage(sub string) string {
	switch sub {
	case "candidate":
		return "usage: compound memory candidate <trace.json>"
	case "candidates":
		return "usage: compound memory candidates --json [--state proposed|parked|promoted|rejected|no_promote|needs_review] [--limit N] [--kind K] [--created-after <RFC3339>] [--include-evidence]"
	case "adjudicate":
		return "usage: compound memory adjudicate <id> --verdict reject|park|promote|no_promote [--work W] [--by ACTOR] [--confirmation-ref ev_ID]|adjudicate --pending [--limit N] [--scan-limit N] [--after <cursor-token-or-filename>] [--verdict reject|park|no_promote] [--work W] [--yes] [--dry-run] (batch drain; --yes required to mutate; --limit caps adjudications, --scan-limit caps entries read, --after resumes from a prior pass's next cursor)"
	case "confirmation":
		return "usage: compound memory confirmation <candidate_id> --quote <text> --reviewed-by operator:<id>"
	case "graph":
		return "usage: compound memory graph <seed> [--depth N] [--budget N] [--min-conf F] [--direction up|down] [--kind KIND[,KIND]...]"
	case "import-topics":
		return "usage: compound memory import-topics <topics.json> [--json]"
	case "temperature":
		return "usage: compound memory temperature [--json] [--rebuild]"
	case "observation":
		return "usage: compound memory observation [legibility] [--json] [--record]"
	case "consumption":
		return "usage: compound memory consumption [--json] [--record]"
	case "canon":
		return "usage: compound memory canon [--json]"
	case "volatile-restore":
		return "usage: compound memory volatile-restore <cooled_claim_id>"
	case "aliases":
		return "usage: compound memory aliases <show|rebuild|expand <query>> [--json]"
	case "fold":
		return "usage: compound memory fold [--entity E] [--json]"
	case "position":
		return "usage: compound memory position <entity> [--as-of N] [--topic topic-slug] [--json]"
	case "engine-config":
		return "usage: compound memory engine-config [--profile generic|memory-cli|breadth] [--clear] [--json]"
	case "bakeoff":
		return "usage: compound memory bakeoff <init [--queries file] [--force]|seed [--json]|run --queries file [--engine bin] [--k N] [--json]>"
	case "concur":
		return "usage: compound memory concur --claim <claim_id> --content \"<text>\" --lanes zai,minimax,qwen [--threshold N] [--id <probe_id>] [--json]"
	case "parity-replay":
		return "usage: compound memory parity-replay --fixture <file.json> --consumer orient|query [--k N (query only)] [--resume-from <checkpoint.json>] [--json]"
	case "parity-freeze":
		return "usage: compound memory parity-freeze --from-journal --window-days N --control-report <report.json> [--out <home-relative path>]"
	case "search":
		return "usage: compound memory search --lane L <query> [--k N] [--json]"
	case "show-output":
		return "usage: compound memory show-output <cid>  (writes the exact externalized tool-output body for sha256:<hex> to stdout)"
	case "render-harness-digest":
		return "usage: compound memory render-harness-digest --harness <name> --out <absolute path> [--json]"
	case "digest-doctor":
		return "usage: compound memory digest-doctor --path <file> [--json]"
	case "promote-driver":
		return promotedriver.Usage
	default:
		return "usage: compound memory <candidate <trace.json>|candidates --json|import-page <markdown>|import-topics <topics.json>|confirmation <candidate_id> --quote <text> --reviewed-by operator:<id>|adjudicate <id> --verdict V|recall [entity] [--as-of N] [--topic topic-slug] [--include-needs-review] [--include-evidence]|timeline <entity>|fold [--entity E] [--json]|position <entity> [--as-of N] [--topic topic-slug] [--json]|graph <seed> [--direction up|down] [--kind K]|write-plan --candidate C --target brain.md|query <text>|search --lane L <query>|show-output <cid>|canon [--json]|aliases <show|rebuild|expand Q>|engine-doctor <text>|engine-compare <text> [--allow-expansion]|engine-config|bakeoff <init|seed|run>|concur --claim C --content Q --lanes a,b [--threshold N] [--json]|parity-replay --fixture F --consumer orient|query [--resume-from checkpoint.json]|parity-freeze --from-journal --window-days N --control-report report.json|temperature [--rebuild]|observation [--json] [--record]|render-harness-digest --harness H --out FILE|digest-doctor --path FILE|promote-driver [--confirm] [--json]|doctor>"
	}
}
func cmdMemory(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, memoryUsage(""))
		os.Exit(2)
	}
	if hasUsageFlag(args) {
		fmt.Println(memoryUsage(args[0]))
		return
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	sub, rest := args[0], args[1:]
	var store *mem.Store
	// memory search is a corpus query (cmdMemoryCorpusSearch) and must NOT
	// open the mutable memory store: mem.Open() creates ledger/, inbox/, and
	// events/ under COMPOUND_HOME, so a read-only subcommand would scatter
	// partial state into a fresh home before the operator ever admits
	// anything. Pinned by TestMemorySearchDoesNotMutateCompoundHome.
	// digest-doctor reads an arbitrary operator-local file (usually OUTSIDE
	// COMPOUND_HOME entirely) and must stay read-only over the ledger too —
	// same rationale as the consumption/search exclusion just above.
	// promote-driver is dry-run BY DEFAULT and opens the ledger read-only
	// (mem.OpenScan) for its plan passes; a mutable mem.Open here would
	// materialize ledger/inbox/events on every scheduled read pass — the
	// read path must actually be read-only, not merely non-mutating by
	// convention. Pinned by TestMemoryPromoteDriverDryRunDoesNotMutateCompoundHome.
	if sub != "consumption" && sub != "search" && sub != "digest-doctor" && sub != "promote-driver" {
		store, err = mem.Open(homePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	flag := func(name string) string {
		for i := 0; i < len(rest); i++ {
			if rest[i] == name && i+1 < len(rest) {
				return rest[i+1]
			}
		}
		return ""
	}
	has := func(name string) bool {
		for _, a := range rest {
			if a == name {
				return true
			}
		}
		return false
	}
	pos := func() string {
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				return a
			}
		}
		return ""
	}
	die := func(e error) {
		if e != nil {
			fmt.Fprintln(os.Stderr, "error:", e)
			os.Exit(1)
		}
	}

	positionals := func(valueFlags map[string]bool) []string {
		var out []string
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if strings.HasPrefix(a, "-") {
				if valueFlags[a] {
					i++
				}
				continue
			}
			out = append(out, a)
		}
		return out
	}
	// firstPos returns the first positional, correctly skipping value flags placed BEFORE it (plain
	// pos() would consume a flag's value as the entity/seed/id).
	firstPos := func(valueFlags map[string]bool) string {
		if p := positionals(valueFlags); len(p) > 0 {
			return p[0]
		}
		return ""
	}

	var restoreMemoryEngineProfile func()
	if sub != "engine-config" {
		var err error
		restoreMemoryEngineProfile, err = configureMemoryEngineFromHome(homePath, flag("--engine-profile"))
		die(err)
		defer restoreMemoryEngineProfile()
	}

	switch sub {
	case "candidate":
		data, err := os.ReadFile(pos())
		die(err)
		ext, err := mem.StructuredExtractor{}.Extract(data)
		die(err)
		got, err := store.Capture(ext.Candidates, time.Now())
		die(err)
		for _, c := range got {
			fmt.Printf("captured %s  %s.%s = %s  (%s)\n", c.ID, c.Entity, c.Predicate, c.Object, c.State)
		}
		for _, r := range ext.Relations {
			e, err := store.Relate(r.From, r.Kind, r.To, r.Confidence, time.Now())
			die(err)
			fmt.Printf("related  %s -%s-> %s  (seq %d, conf %.2f)\n", e.From, e.Kind, e.To, e.Seq, e.Confidence)
		}
	case "import-topics":
		cmdMemoryImportTopics(store, rest)
	case "temperature":
		cmdMemoryTemperature(homePath, rest)
	case "observation":
		cmdMemoryObservation(homePath, rest)
	case "consumption":
		err = mem.RunCauseConsumptionCLI(homePath, rest, os.Stdout, os.Stderr, time.Now)
		die(err)
	case "canon":
		cmdMemoryCanon(store, rest)
	case "volatile-restore":
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
			fmt.Fprintln(os.Stderr, "usage: compound memory volatile-restore <cooled_claim_id>")
			os.Exit(2)
		}
		claim, err := store.VolatileRestore(rest[0], time.Now())
		die(err)
		fmt.Printf("restored %s -> %s visibility=volatile expires_at=%s (undo of %s)\n", rest[0], claim.ID, claim.ExpiresAt, mem.CooledReasonVolatileExpired)
	case "aliases":
		cmdMemoryAliases(homePath, store, rest)
	case "candidates":
		limit := 0
		if v := flag("--limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "error: --limit must be a non-negative integer")
				os.Exit(2)
			}
			limit = n
		}
		var createdAfter time.Time
		if v := flag("--created-after"); v != "" {
			ts, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: --created-after must be an RFC3339 timestamp:", err)
				os.Exit(2)
			}
			createdAfter = ts
		}
		kindFilter := flag("--kind")
		report, err := store.ListCandidates(time.Now(), mem.CandidateListOptions{
			State:           flag("--state"),
			Kind:            kindFilter,
			Limit:           limit,
			CreatedAfter:    createdAfter,
			IncludeEvidence: has("--include-evidence"),
		})
		die(err)
		if has("--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# memory candidates %d/%d\n", report.Counts.Matched, report.Counts.Total)
		if report.Counts.Truncated {
			fmt.Printf("# showing %d of %d matched (--limit %d); raise --limit to see the rest\n",
				report.Counts.Returned, report.Counts.Matched, limit)
		}
		// Falsifier 3 (work_3c70f011f6ba): surface cross-namespace door.
		if kindFilter != "" {
			mem.MaybePrintCrossKindPointer(os.Stdout, kindFilter)
		}
		for _, item := range report.Items {
			line := fmt.Sprintf("%s %s %s.%s = %s", item.ID, item.State, item.Entity, item.Predicate, item.Object)
			if item.ReviewState != "" {
				line += " review=" + item.ReviewState
			}
			fmt.Println(line)
		}
	case "import-page":
		pagePath := pos()
		if pagePath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound memory import-page <markdown>")
			os.Exit(2)
		}
		report, err := store.ImportKnowledgePage(pagePath, time.Now())
		die(err)
		fmt.Printf("# memory import-page %s\n", report.PageID)
		fmt.Printf("- evidence: %s\n", report.EvidenceID)
		fmt.Printf("- candidates: %d\n", len(report.CandidateIDs))
		fmt.Printf("- created: %d\n", report.Created)
		fmt.Printf("- reused: %d\n", report.Reused)
		for _, id := range report.CandidateIDs {
			fmt.Printf(" - candidate %s\n", id)
		}
	case "relate":
		args := positionals(map[string]bool{"--conf": true})
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: compound memory relate <from-entity> <relates|contradicts|depends_on|refines|supports|mentions|owns|part_of> <to-entity> [--conf 0.8]")
			os.Exit(2)
		}
		conf := 0.8
		if v := flag("--conf"); v != "" {
			conf, _ = strconv.ParseFloat(v, 64)
		}
		e, err := store.Relate(args[0], args[1], args[2], conf, time.Now())
		die(err)
		fmt.Printf("related %s -%s-> %s  (seq %d, conf %.2f)\n", e.From, e.Kind, e.To, e.Seq, e.Confidence)
	case "graph":
		depth, budget := 2, 20
		minConf := 0.0
		direction := flag("--direction")
		kinds := flagCSVValues(rest, "--kind", "--kinds")
		if v := flag("--depth"); v != "" {
			depth, _ = strconv.Atoi(v)
		}
		if v := flag("--budget"); v != "" {
			budget, _ = strconv.Atoi(v)
		}
		if v := flag("--min-conf"); v != "" {
			minConf, _ = strconv.ParseFloat(v, 64)
		}
		sg, err := store.RecallGraphOpts(firstPos(map[string]bool{"--depth": true, "--budget": true, "--min-conf": true, "--direction": true, "--kind": true, "--kinds": true}), mem.GraphOpts{Depth: depth, Budget: budget, MinConf: minConf, Direction: direction, Kinds: kinds})
		die(err)
		fmt.Println("# " + sg.Coverage)
		if len(sg.SeedEntities) > 0 {
			fmt.Printf("seeds: %s\n", strings.Join(sg.SeedEntities, ", "))
		}
		for _, c := range sg.Claims {
			fmt.Printf("  %s.%s = %s  (seq %d, conf %.2f)\n", c.Entity, c.Predicate, c.Object, c.Seq, c.Confidence)
		}
		for _, r := range sg.Relations {
			fmt.Printf("  %s -%s-> %s  (conf %.2f)\n", r.From, r.Kind, r.To, r.Confidence)
		}
	case "confirmation":
		candidateID := firstPos(map[string]bool{"--quote": true, "--reviewed-by": true})
		quote := strings.TrimSpace(flag("--quote"))
		reviewer := strings.TrimSpace(flag("--reviewed-by"))
		if candidateID == "" || quote == "" || reviewer == "" {
			fmt.Fprintln(os.Stderr, memoryUsage("confirmation"))
			os.Exit(2)
		}
		evidence, err := store.RecordOperatorConfirmation(candidateID, quote, reviewer, time.Now())
		die(err)
		fmt.Printf("%s: recorded confirmation %s reviewed_by=%s\n", candidateID, evidence.ID, evidence.Actor)
	case "adjudicate":
		// Batch face (--pending): drain the inbox in stable id order.
		// Dry-run by default; --yes applies. Refuses --verdict promote
		// (the per-id door owns promotion so a stale batch cannot promote).
		if has("--pending") {
			pendingUsage := memoryUsage("adjudicate")
			limit := 0
			if v, err := intFlag(rest, "--limit"); err != nil {
				// An unparseable --limit must never fall back to zero,
				// because zero means UNCAPPED — a typo would turn a bounded
				// drain into a full-inbox mutation.
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				fmt.Fprintln(os.Stderr, pendingUsage)
				os.Exit(2)
			} else {
				limit = v
			}
			scanLimit := 0
			if v, err := intFlag(rest, "--scan-limit"); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				fmt.Fprintln(os.Stderr, pendingUsage)
				os.Exit(2)
			} else {
				scanLimit = v
			}
			afterCursor, err := parseMemoryAfterCursor(flag("--after"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: invalid --after cursor:", err)
				fmt.Fprintln(os.Stderr, pendingUsage)
				os.Exit(2)
			}
			yes := has("--yes")
			verdict := flag("--verdict")
			if verdict != "" && verdict != "reject" && verdict != "park" && verdict != "no_promote" {
				fmt.Fprintln(os.Stderr, "error: --verdict must be one of reject|park|no_promote in --pending mode")
				fmt.Fprintln(os.Stderr, pendingUsage)
				os.Exit(2)
			}
			// --dry-run short-circuits the per-candidate adjudication so a
			// 132k-candidate backlog is previewable in seconds, not minutes.
			// --dry-run is the DEFAULT for the verb: --yes is the only way to
			// mutate, so a stray script cannot drain the inbox by accident.
			dryRun := has("--dry-run") || !yes
			opts := mem.AdjudicatePendingOptions{Verdict: verdict, WorkID: flag("--work"), DryRun: dryRun, Limit: limit, ScanLimit: scanLimit, AfterCursor: afterCursor}
			report := store.AdjudicatePending(opts, time.Now())
			if report.LimitReached || report.ScanCapHit {
				// The caps are budgets, not completion claims: tell the
				// operator the drain is resumable and exactly where from.
				if report.LimitReached {
					fmt.Fprintf(os.Stderr, "note: --limit %d reached (adjudicated=%d, skipped=%d already closed) — re-run to continue the drain\n", limit, report.Adjudicated, report.Skipped)
				}
				if report.ScanCapHit {
					fmt.Fprintf(os.Stderr, "note: --scan-limit %d reached (walked=%d decoded=%d corrupt=%d) — re-run to continue the scan\n", scanLimit, report.Walked, report.Decoded, report.Corrupt)
				}
				if report.NextCursor != "" {
					token, err := report.NextCursor.Token()
					if err != nil {
						fmt.Fprintln(os.Stderr, "error: render next cursor:", err)
					} else {
						fmt.Fprintf(os.Stderr, "next cursor: --after %s\n", token)
					}
				}
			}
			if !yes {
				if has("--json") {
					// JSON-only output: the human preamble would corrupt
					// downstream parsing.
					printJSON(report)
					return
				}
				fmt.Printf("dry-run: %d candidate(s) scanned; would adjudicate %d by disposition; %d already closed", report.Scanned, report.Adjudicated, report.Skipped)
				if report.BasisDeferred > 0 {
					fmt.Printf("; %d basis candidate(s) left proposed for per-id promotion", report.BasisDeferred)
				}
				fmt.Println()
				for v, c := range report.VerdictCounts {
					fmt.Printf("  would %s: %d\n", v, c)
				}
				if len(report.Errors) > 0 {
					fmt.Println("errors:")
					for _, e := range report.Errors {
						identity := e.CandidateID
						if identity == "" && e.EntryCursor != "" {
							if token, err := e.EntryCursor.Token(); err == nil {
								identity = token
							}
						}
						fmt.Printf("  %s: %s\n", identity, e.Err)
					}
				}
				return
			}
			fmt.Printf("adjudicated: scanned=%d adjudicated=%d skipped=%d", report.Scanned, report.Adjudicated, report.Skipped)
			if report.BasisDeferred > 0 {
				fmt.Printf(" basis-deferred=%d", report.BasisDeferred)
			}
			fmt.Println()
			for v, c := range report.VerdictCounts {
				fmt.Printf("  %s: %d\n", v, c)
			}
			if len(report.Errors) > 0 {
				fmt.Println("errors:")
				for _, e := range report.Errors {
					identity := e.CandidateID
					if identity == "" && e.EntryCursor != "" {
						if token, err := e.EntryCursor.Token(); err == nil {
							identity = token
						}
					}
					fmt.Printf("  %s: %s\n", identity, e.Err)
				}
			}
			if has("--json") {
				printJSON(report)
			}
			return
		}
		candidateID := firstPos(map[string]bool{"--verdict": true, "--work": true, "--confirmation-ref": true, "--by": true})
		verdict := flag("--verdict")
		confirmationRef := flag("--confirmation-ref")
		if has("--confirmation-ref") && confirmationRef == "" {
			fmt.Fprintln(os.Stderr, "usage: --confirmation-ref requires a content-addressed evidence id")
			os.Exit(2)
		}
		if confirmationRef != "" && verdict != candidates.Promote {
			fmt.Fprintln(os.Stderr, "usage: --confirmation-ref is valid only with --verdict promote")
			os.Exit(2)
		}
		// Falsifier 1 (work_3c70f011f6ba): empty id exits 2 with usage, never the store.
		if candidateID == "" {
			fmt.Fprintln(os.Stderr, memoryUsage("adjudicate"))
			os.Exit(2)
		}
		var res mem.AdjudicateResult
		if confirmationRef != "" {
			res, err = store.AdjudicateExplicitConfirmation(candidateID, confirmationRef, flag("--work"), time.Now())
		} else {
			// The CLI verb is the operator ratification door: it always stamps
			// AdjudicatedBy/AdjudicatedAt (actor defaults to "operator") so a
			// candidate the operator has ruled on — including a re-park, "Keep
			// aside" — leaves the needs-your-call queue while staying parked as
			// evidence. See mem.Candidate.AdjudicatedBy.
			res, err = store.AdjudicateAsOperator(candidateID, verdict, flag("--work"), flag("--by"), time.Now())
		}
		// Falsifier 2 (work_3c70f011f6ba): wrap "not found: <id>.json" with the searched location + door hint.
		if err != nil {
			if msg := err.Error(); strings.HasPrefix(msg, "not found: ") {
				mem.PrintAdjudicateUnknownID(os.Stderr, candidateID, msg)
				os.Exit(1)
			}
			die(err)
		}
		// Loop piece 2/4 (work_57f80593eb2d): a freshly promoted Claim becomes
		// a validated_by edge on the matching capability-graph node so the
		// next orient offer scoring (Surface) re-orders matching offers by it.
		// Resolved through the candidate's explicit `capability` qualifier, or
		// inferred from the candidate's Predicate / Subject so a wide range of
		// promoted candidates enrich the latent space without each call having
		// to know its target node id. A clean no-match is silent (false): a
		// Claim about a topic with no matching capability must NOT mint a
		// phantom node. Enrichment errors are NOT fatal — the ledger promote
		// already committed, so we log a stderr note and let the verb return
		// the adjudicate result the operator asked for.
		if res.Claim != nil && strings.TrimSpace(homePath) != "" {
			hint, token := memoryAdjudicateEnrichHints(res.Candidate, *res.Claim)
			if hint != "" || token != "" {
				enrichHome := homePath
				if _, wrote, enrichErr := capgraph.EnrichFromPromotedClaim(
					enrichHome,
					res.Claim.ID,
					capgraph.ProvenanceAdjudicateMemEnrich,
					hint,
					token,
					time.Now(),
				); enrichErr != nil {
					fmt.Fprintf(os.Stderr, "capgraph enrichment error for %s: %v (adjudicate result still stands)\n", res.Claim.ID, enrichErr)
				} else if wrote {
					fmt.Fprintf(os.Stderr, "capgraph enrichment: %s -> validated_by edge\n", res.Claim.ID)
				}
			}
		}
		if res.Claim != nil {
			line := fmt.Sprintf("%s: promoted claim %s (seq %d)", res.Candidate.ID, res.Claim.ID, res.Claim.Seq)
			if res.Superseded != "" {
				line += " superseding " + res.Superseded
			}
			fmt.Println(line)
			if res.Transition != nil {
				fmt.Printf("  transition %s — %s\n", res.Transition.ID, res.Transition.StateDelta)
			}
		} else {
			fmt.Printf("%s: %s%s\n", res.Candidate.ID, res.Verdict, dashNote(res.Note))
		}
	case "recall":
		cmdMemoryRecall(homePath, store, rest)
	case "bakeoff":
		cmdMemoryBakeoff(store, rest, memBreadthEngine)
	case "concur":
		cmdMemoryConcur(store, rest)
	case "parity-replay":
		cmdMemoryParityReplay(homePath, store, rest)
	case "parity-freeze":
		cmdMemoryParityFreeze(homePath, rest)
	case "search":
		if flag("--lane") == "" {
			fmt.Fprintln(os.Stderr, "usage: compound memory search --lane L <query> [--k N] [--json]")
			os.Exit(2)
		}
		cmdMemoryCorpusSearch(homePath, rest)
	case "timeline":
		if err := validateKnownFlags(rest, nil, nil); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, memoryUsage("timeline"))
			os.Exit(2)
		}
		claims, err := store.Timeline(pos())
		die(err)
		for _, c := range claims {
			arrow := "  +"
			if c.Supersedes != "" {
				arrow = "  ->"
			}
			line := fmt.Sprintf("%s seq %-3d %s %s.%s = %s", arrow, c.Seq, c.ID, c.Entity, c.Predicate, c.Object)
			if c.Supersedes != "" {
				line += " supersedes=" + c.Supersedes
			}
			if len(c.EvidenceRefs) > 0 {
				line += " evidence=" + strings.Join(c.EvidenceRefs, ",")
			}
			fmt.Println(line)
		}
	case "fold":
		cmdMemoryFold(store, rest)
	case "position":
		cmdMemoryPosition(store, rest)
	case "positions":
		// The spine-native Position: current durable stance per narrow fact slot WITH the
		// supersession chain (old→new); topic remains the broader aggregation axis.
		ps, err := store.Positions()
		die(err)
		for _, p := range ps {
			fmt.Printf("%-28s = %-12s [%s]  chain:%d\n", p.ID, p.ActiveObject, p.Status, len(p.ClaimChain))
		}
	case "patterns":
		// recurrence hypotheses: (entity, topic) the ledger returns to across ≥2 claims/sessions.
		// Convergence of Gen-1 internal/pattern onto the spine.
		pats, err := store.Patterns()
		die(err)
		for _, p := range pats {
			fmt.Printf("%-22s sessions:%d claims:%d  %s\n", p.ID, p.Sessions, len(p.SupportClaims), p.Status)
		}
	case "entity":
		// the routing backbone: a canonical identity resolvable by slug/identifier/alias/path, so
		// project identity survives a folder move. Convergence of Gen-1 memory.Entity + scope onto the spine.
		split := func(s string) []string {
			if s == "" {
				return nil
			}
			return strings.Split(s, ",")
		}
		switch {
		case len(rest) >= 3 && rest[0] == "register":
			e, err := store.RegisterEntity(mem.Entity{
				Slug: rest[1], Kind: rest[2],
				Aliases: split(flag("--alias")), Identifiers: split(flag("--id")), Parent: flag("--parent"),
			}, time.Now())
			die(err)
			fmt.Printf("registered %s (%s) aliases=%v ids=%v\n", e.Slug, e.Kind, e.Aliases, e.Identifiers)
		case len(rest) >= 2 && rest[0] == "resolve":
			r, err := store.ResolveEntity(rest[1])
			die(err)
			if r.Status == "resolved" {
				fmt.Printf("%s  (via %s)\n", r.Slug, r.Via)
			} else {
				fmt.Printf("%s %s\n", r.Status, r.Slug)
			}
		case len(rest) >= 1 && rest[0] == "list":
			ents, err := store.Entities()
			die(err)
			for _, e := range ents {
				fmt.Printf("%-24s %-10s aliases=%v ids=%v\n", e.Slug, e.Kind, e.Aliases, e.Identifiers)
			}
		case len(rest) >= 1 && rest[0] == "fold":
			// Rewrites the registry to the rows its readers already select.
			// Refuses to apply unless both production readers agree across the
			// old and new encoding and every surviving row is a verbatim row of
			// the original; the pre-fold bytes are archived first. Defaults to a
			// dry run.
			apply := has("--apply")
			report, err := store.FoldEntities(time.Now(), apply)
			die(err)
			if has("--json") {
				printJSON(report)
				return
			}
			fmt.Printf("rows %d -> %d (%d slugs, %d unreadable rows skipped)\n",
				report.RowsBefore, report.RowsAfter, report.RowsAfter, report.SkippedRows)
			fmt.Printf("bytes %d -> %d\n", report.BytesBefore, report.BytesAfter)
			fmt.Printf("verified=%v applied=%v\n", report.Verified, report.Applied)
			if report.ArchivePath != "" {
				fmt.Printf("archive %s sha256=%s\n", report.ArchivePath, report.ArchiveSHA256)
			}
			if !apply {
				fmt.Println("dry run; re-run with --apply to rewrite the registry")
			}
		default:
			fmt.Fprintln(os.Stderr, "usage: compound memory entity <register <slug> <kind> [--alias a,b] [--id x,y] [--parent p]|resolve <token>|list|fold [--apply]>")
			os.Exit(2)
		}
	case "evidence":
		// stored, citable, content-addressed raw material — the convergence of Gen-1 memory.EvidenceItem.
		switch {
		case len(rest) >= 1 && rest[0] == "record":
			e, err := store.RecordEvidence(mem.EvidenceItem{
				SourceKind: flag("--source-kind"), SourceID: flag("--source-id"),
				QuoteText: flag("--quote"), Actor: flag("--actor"),
			}, time.Now())
			die(err)
			fmt.Printf("recorded %s (%s) %q\n", e.ID, e.SourceKind, e.QuoteText)
		case len(rest) >= 1 && rest[0] == "list":
			items, err := store.Evidence()
			die(err)
			for _, e := range items {
				fmt.Printf("%-18s %-12s %q\n", e.ID, e.SourceKind, e.QuoteText)
			}
		default:
			fmt.Fprintln(os.Stderr, `usage: compound memory evidence <record --source-kind k --quote "..." [--source-id id] [--actor user]|list>`)
			os.Exit(2)
		}
	case "write-plan":
		candidateID := flag("--candidate")
		targetPath := flag("--target")
		if candidateID == "" || targetPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound memory write-plan --candidate <candidate_id> --target <markdown>")
			os.Exit(2)
		}
		plan, err := store.BuildMeaningWritePlan(candidateID, targetPath, time.Now())
		die(err)
		paths, err := mem.RecordMeaningWritePlan(homePath, plan)
		die(err)
		fmt.Printf("# memory write-plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n", plan.Status)
		fmt.Printf("- candidate: %s\n", plan.CandidateID)
		fmt.Printf("- target: %s\n", plan.TargetPath)
		fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n", paths.PlanPath)
		fmt.Printf("- review: %s\n", paths.ReviewPath)
	case "query":
		// breadth via the RENTED engine, relevance-gated by the ledger. With COMPOUND_MEM_ENGINE_BIN
		// set, drive the sidecar (e.g. turbovec) over the protocol via a warm held coprocess;
		// otherwise the in-process lexical engine. A model swap is a HARD refuse, not a degrade.
		eng, closeEng := memBreadthEngine()
		retrievalStarted := time.Now()
		claims, err := store.QueryBreadth(pos(), 10, eng)
		closeEng() // tear down the held engine BEFORE any exit/return (os.Exit skips defers)
		retrievalparity.Record(homePath, retrievalparity.NewRow(retrievalparity.ConsumerQuery, pos(), parityClaimRefs(claims), time.Since(retrievalStarted), err == nil, time.Now()))
		switch {
		case errors.Is(err, mem.ErrUnsupported), errors.Is(err, mem.ErrEngineUnavailable):
			fmt.Println("# no breadth engine — exact/as-of recall available via `compound memory recall`")
			return
		case errors.Is(err, mem.ErrEmbedderMismatch), errors.Is(err, mem.ErrProtocolVersion):
			fmt.Fprintln(os.Stderr, "error: breadth engine refused (not degrading):", err)
			os.Exit(1)
		}
		die(err)
		fmt.Printf("# %d breadth hit(s) for %q (relevance-gated)\n", len(claims), pos())
		for _, c := range claims {
			fmt.Printf("%s.%s = %s  (seq %d, %s)\n", c.Entity, c.Predicate, c.Object, c.Seq, c.Visibility)
		}
	case "engine-doctor":
		k := 10
		if v := flag("--k"); v != "" {
			n, err := strconv.Atoi(v)
			die(err)
			k = n
		}
		eng, closeEng := memBreadthEngine()
		report, err := store.EngineDoctor(firstPos(map[string]bool{"--k": true}), k, eng)
		closeEng()
		fmt.Println("# memory engine-doctor")
		fmt.Printf("- query: %s\n", report.Query)
		fmt.Printf("- engine: %s\n", report.Engine.Name)
		fmt.Printf("- ledger_gen: %d\n", report.LedgerGen)
		fmt.Printf("- ledger_gen_after: %d\n", report.LedgerGenAfter)
		fmt.Printf("- engine_gen: %d\n", report.Engine.CorpusGen)
		fmt.Printf("- ledger_before: %s\n", report.LedgerHashBefore)
		fmt.Printf("- ledger_after: %s\n", report.LedgerHashAfter)
		fmt.Printf("- ledger_unchanged: %t\n", report.LedgerUnchanged)
		fmt.Printf("- owner_ledger_advanced: %t\n", report.OwnerLedgerAdvanced)
		fmt.Printf("- retry_count: %d\n", report.RetryCount)
		fmt.Printf("- advanced_rows: %d\n", report.AdvancedRows)
		if report.Note != "" {
			fmt.Printf("- note: %s\n", report.Note)
		}
		if report.Degraded {
			fmt.Printf("- degraded: %s\n", report.Error)
		}
		fmt.Printf("- hits: %d\n", len(report.Hits))
		for _, h := range report.Hits {
			fmt.Printf("  - %s: %s.%s = %s (seq %d)\n", h.ClaimID, h.Entity, h.Predicate, h.Object, h.Seq)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory engine-doctor:", err)
			os.Exit(1)
		}
	case "engine-compare":
		if strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_BIN")) == "" {
			fmt.Fprintln(os.Stderr, "memory engine-compare requires COMPOUND_MEM_ENGINE_BIN (a rented sidecar command)")
			os.Exit(2)
		}
		allowExpansion := has("--allow-expansion")
		k := 10
		if v := flag("--k"); v != "" {
			n, err := strconv.Atoi(v)
			die(err)
			k = n
		}
		eng, closeEng := memBreadthEngine()
		report, err := store.EngineCompareWithOptions(firstPos(map[string]bool{"--k": true}), k, eng, mem.EngineCompareOptions{AllowExpansion: allowExpansion})
		closeEng()
		fmt.Println("# memory engine-compare")
		fmt.Printf("- query: %s\n", report.Query)
		fmt.Printf("- compare_mode: %s\n", report.CompareMode)
		fmt.Printf("- reference_engine: %s\n", report.Reference.Engine.Name)
		fmt.Printf("- candidate_engine: %s\n", report.Candidate.Engine.Name)
		fmt.Printf("- ledger_unchanged: %t\n", report.LedgerUnchanged)
		fmt.Printf("- reference_covered: %t\n", report.ReferenceCovered)
		fmt.Printf("- reference_hits: %d\n", len(report.ReferenceClaimIDs))
		fmt.Printf("- candidate_hits: %d\n", len(report.CandidateClaimIDs))
		fmt.Printf("- missing: %d\n", len(report.MissingClaimIDs))
		for _, id := range report.MissingClaimIDs {
			fmt.Printf(" - missing %s\n", id)
		}
		fmt.Printf("- extra: %d\n", len(report.ExtraClaimIDs))
		for _, id := range report.ExtraClaimIDs {
			fmt.Printf(" - extra %s\n", id)
		}
		fmt.Printf("- expansion: %d\n", len(report.ExpansionClaimIDs))
		for _, id := range report.ExpansionClaimIDs {
			fmt.Printf(" - expansion %s\n", id)
		}
		fmt.Printf("- match: %t\n", report.Match)
		fmt.Printf("- acceptable: %t\n", report.Acceptable)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory engine-compare:", err)
			os.Exit(1)
		}
		if !report.Acceptable {
			os.Exit(1)
		}
	case "engine-config":
		if has("--clear") && flag("--profile") != "" {
			fmt.Fprintln(os.Stderr, memoryUsage("engine-config"))
			os.Exit(2)
		}
		var report memoryEngineConfigReport
		var err error
		switch {
		case has("--clear"):
			report, err = clearMemoryEngineConfig(homePath)
		case flag("--profile") != "":
			report, err = writeMemoryEngineConfig(homePath, flag("--profile"))
		default:
			cfg, path, loadErr := loadMemoryEngineConfig(homePath)
			err = loadErr
			report = memoryEngineConfigReport{
				SchemaVersion: memoryEngineConfigSchemaVersion,
				ConfigPath:    path,
				Configured:    cfg.Profile != "",
				Profile:       cfg.Profile,
				CorpusLanes:   len(cfg.CorpusLanes),
			}
		}
		die(err)
		if has("--json") {
			printJSON(report)
			return
		}
		fmt.Println("# memory engine-config")
		fmt.Printf("- config: %s\n", report.ConfigPath)
		fmt.Printf("- configured: %t\n", report.Configured)
		if report.Profile != "" {
			fmt.Printf("- profile: %s\n", report.Profile)
		}
		if report.CorpusLanes > 0 {
			fmt.Printf("- corpus_lanes: %d\n", report.CorpusLanes)
		}
		if report.Cleared {
			fmt.Println("- cleared: true")
		}
	case "show-output":
		// C3: retrieve the full externalized tool-output body for a content id captured at the
		// PostToolUse seam. The body lives only in the rebuildable blob store (never the ledger,
		// never ambient recall); this is the sole v1 door back to it. A missing/malformed cid is an
		// explicit error, never a silent empty.
		cid := pos()
		if strings.TrimSpace(cid) == "" {
			fmt.Fprintln(os.Stderr, "usage: compound memory show-output <cid>")
			os.Exit(2)
		}
		body, err := store.ReadToolOutput(cid)
		die(err)
		if _, err := os.Stdout.Write(body); err != nil {
			die(err)
		}
	case "render-harness-digest":
		cmdMemoryRenderHarnessDigest(store, rest)
	case "digest-doctor":
		cmdMemoryDigestDoctor(rest)
	case "doctor":
		var r mem.Report
		var eng mem.Engine
		// A selected or malformed external store is a categorical refusal;
		// otherwise an empty home lets doctor report the supported file lane as
		// healthy without ever invoking the refusing engine.
		if externalStoreErr := memoryExternalStoreRefusal(); externalStoreErr != nil {
			if has("--json") {
				printJSON(map[string]any{"ok": false, "error": externalStoreErr.Error()})
			} else {
				fmt.Fprintln(os.Stderr, "memory doctor:", externalStoreErr)
			}
			// Doctor exit-code contract (internal/doctor/exitcode): the
			// doctor itself could not run, which is exit 2 — distinct from
			// "ran and found problems" (1) so callers tell a broken doctor
			// apart from a broken estate.
			os.Exit(exitcode.CouldNotRun)
		}
		if strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_BIN")) != "" {
			var closeEng func()
			eng, closeEng = memBreadthEngine()
			r = store.DoctorWithEngine(eng)
			closeEng()
		} else {
			r = store.Doctor()
		}
		r.SemanticReadiness = memorySemanticReadiness(homePath, eng)
		// Doctor exit-code contract (internal/doctor/exitcode): a failed
		// hop is a problem (exit 1 + final count line); a warned hop is
		// info-only (exit 0) but MUST be visible — text prints it under a
		// `warnings:` heading, JSON carries the `warnings` array — never
		// silently buried inside "ok": true.
		doc := exitcode.Doctor{
			Verb:     "memory doctor",
			Warnings: r.Warnings,
			Note:     "fix the failed hop(s) listed above",
		}
		for _, h := range r.Hops {
			if h.Status == "fail" {
				doc.Problems = append(doc.Problems, h.Name+" hop failed — "+h.Detail)
			}
		}
		if has("--json") {
			printJSON(r)
			if doc.Exit() != exitcode.OK {
				os.Exit(exitcode.ProblemFound)
			}
			return
		}
		for _, h := range r.Hops {
			fmt.Printf("%-15s %-8s %s\n", h.Name, h.Status, h.Detail)
		}
		if r.SemanticReadiness != nil {
			fmt.Printf("%-15s %-8s %s\n", "semantic", r.SemanticReadiness.Status, r.SemanticReadiness.Detail)
		}
		fmt.Printf("verdict: %s — %s\n", okStr(r.OK), r.Note)
		// Exit 0 is the absence of an exit call (cmdMemory's callers rely
		// on a clean return; in-process tests would panic on os.Exit(0)).
		// Only a found problem or a broken doctor exits explicitly.
		if code := doc.ExitWith(os.Stdout); code != exitcode.OK {
			os.Exit(code)
		}
	case "promote-driver":
		os.Exit(promotedriver.RunCLI(rest))
	default:
		fmt.Fprintln(os.Stderr, memoryUsage(""))
		os.Exit(2)
	}
}

// memoryAdjudicateEnrichHints derives the (capabilityHint, contextToken)
// pair the capgraph enrichment seam consumes after a successful promote.
//
// Resolution rules (first non-empty hit wins for the hint):
//  1. Candidate.Qualifiers["capability"] (explicit node id / name /
//     capability:// URI the caller already knows the target of).
//  2. Candidate.Qualifiers["node_id"] (canonical node id when the
//     candidate's author knew exactly which node the claim should land
//     on).
//  3. Candidate.Qualifiers["capability_kind"] + ":" + Candidate.Entity
//     fallback (the kind+subject shape used by distiller-source
//     CapabilityObservation candidates).
//
// The context token is the Claim.Entity when set, falling back to the
// Candidate.Predicate — either is a stable, surface-tokenizer-friendly
// piece of evidence that maps to a future orient task.
//
// Both hint and token may be empty, in which case the caller skips the
// enrichment call (a candidate about a topic with no capability hint and
// no usable predicate is intentionally left alone).
func memoryAdjudicateEnrichHints(c mem.Candidate, claim mem.Claim) (string, string) {
	hint := firstNonEmpty(
		strings.TrimSpace(c.Qualifiers["capability"]),
		strings.TrimSpace(c.Qualifiers["node_id"]),
	)
	if hint == "" {
		kind := strings.TrimSpace(c.Qualifiers["capability_kind"])
		entity := strings.TrimSpace(c.Entity)
		if kind != "" && entity != "" {
			hint = kind + ":" + entity
		} else if kind != "" {
			hint = kind
		} else if entity != "" {
			hint = entity
		}
	}
	token := firstNonEmpty(claim.Entity, c.Predicate, c.Entity)
	return hint, token
}
