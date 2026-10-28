// Command compound is the CLI for the compound information kernel / transition substrate.
// Built envelope-first: the proven spine is P0-P6 first slice (home, envelopes, contract/orient, work,
// policy, harness/hook, memory ledger). Memory is one slice on top (P6, re-bound through the spine), not the
// system boundary. Gen-1 memory-era commands are retired tombstones; canonical memory lives
// under `compound memory ...`.
// Architecture: docs/architecture/{substrate,runtime,transition}.md, docs/roadmap/phases.md.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	hookfixtures "compound/fixtures/hooks"
	"compound/internal/arch"
	"compound/internal/authority"
	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/contract"
	"compound/internal/decisions"
	"compound/internal/distiller"
	"compound/internal/envelope"
	"compound/internal/eventsource"
	"compound/internal/eventstore"
	"compound/internal/finding"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/hook"
	"compound/internal/hookplan"
	"compound/internal/knowledge"
	"compound/internal/lanes"
	"compound/internal/mem"
	"compound/internal/policy"
	"compound/internal/product"
	"compound/internal/projection"
	"compound/internal/retrievalparity"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
	routeevidencecli "compound/internal/routeevidence/cli"
	"compound/internal/runtimebinding"
	"compound/internal/schema"
	"compound/internal/sourceobs"
	storecli "compound/internal/store"
	"compound/internal/work"
	"compound/internal/writeback"
)

const version = "0.1.0"

// buildSHA is embedded at build time via
//
//	-ldflags "-X main.buildSHA=$(git rev-parse --short HEAD)"
//
// (see install.sh, the entry point `make install` calls). Empty when built without
// ldflags (e.g. a plain `go build`/`go run` during dev) — `compound version` then
// reports "dev" rather than silently claiming a build provenance it doesn't have.
// This is the freshness signal consumed by internal/arch's build-freshness check:
// a hook/consumer running a stale installed binary is otherwise undetectable.
var buildSHA string

func buildSHAOrDev() string {
	if strings.TrimSpace(buildSHA) == "" {
		return "dev"
	}
	return strings.TrimSpace(buildSHA)
}

// versionString is what `compound version`/`--version`/`-v` prints. Split out from the
// switch so tests can assert build-SHA embedding without shelling out to a built binary.
func versionString() string {
	return fmt.Sprintf("compound %s (build %s)", version, buildSHAOrDev())
}

// versionReport is what `compound version --json` emits: the same identity as versionString, in
// a shape a caller can PARSE rather than string-match, plus an answer to a nonce the caller
// minted. Install verification uses it to prove the artifact at the install path is this binary
// and not something that merely knows what this binary prints (internal/arch/installverify.go).
func versionReport(challenge string) arch.VersionReport {
	challenge = strings.TrimSpace(challenge)
	report := arch.VersionReport{
		SchemaVersion: arch.VersionReportSchema,
		Version:       version,
		BuildSHA:      buildSHAOrDev(),
	}
	if challenge != "" {
		report.Challenge = challenge
		report.ChallengeDigest = arch.VersionChallengeDigest(challenge, report.Version, report.BuildSHA)
	}
	return report
}

func main() {
	rawArgs, err := prepareCompoundEntrypointArgs(os.Args[0], os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "os:", err)
		fmt.Fprintln(os.Stderr, osAliasUsage)
		os.Exit(2)
	}
	args, err := home.PrepareGlobalCLIArgs(rawArgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		usage()
		os.Exit(2)
	}
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	os.Args = append([]string{os.Args[0]}, args...)
	command, ok := compoundCommandRegistry()[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	die(firstPartyCommandCatalog().RequireHomeCompatibility(os.Args[1], os.Args[2:]))
	command.Run(os.Args[2:])
}

const osAliasUsage = "usage: os [status|doctor|capability <task>|skills [query]|route <class>] [options]"

func prepareCompoundEntrypointArgs(argv0 string, args []string) ([]string, error) {
	if filepath.Base(argv0) != "os" {
		return append([]string(nil), args...), nil
	}
	return rewriteOSAliasArgs(args)
}

func rewriteOSAliasArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return []string{"system", "status"}, nil
	}
	switch args[0] {
	case "status":
		return append([]string{"system", "status"}, args[1:]...), nil
	case "doctor":
		return append([]string{"doctor"}, args[1:]...), nil
	case "capability":
		if len(args) < 2 {
			return nil, fmt.Errorf("capability requires a task")
		}
		rewritten, err := rewriteOSAliasFlags(args[1:], false)
		if err != nil {
			return nil, err
		}
		return append([]string{"capability", "resolve"}, rewritten...), nil
	case "skills":
		rewritten, positional, err := rewriteOSSkillsArgs(args[1:])
		if err != nil {
			return nil, err
		}
		subcommand := "surface"
		if positional == 0 {
			subcommand = "list"
		}
		return append([]string{"capability", subcommand}, rewritten...), nil
	case "route":
		if len(args) < 2 || args[1] == "--list" || strings.HasPrefix(args[1], "-") {
			return nil, fmt.Errorf("route requires a capability class; --list is retired")
		}
		class := canonicalOSRouteClass(args[1])
		rewritten, err := rewriteOSAliasFlags(args[2:], false)
		if err != nil {
			return nil, err
		}
		out := []string{"route", "plan", "--capability-class", class}
		return append(out, rewritten...), nil
	default:
		return nil, fmt.Errorf("legacy command %q is retired", args[0])
	}
}

func rewriteOSAliasFlags(args []string, skills bool) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--privacy":
			out = append(out, "--privacy-sensitive")
		case "--write-packet":
			return nil, fmt.Errorf("--write-packet is retired; use Compound work packet doors")
		case "--host":
			if !skills || i+1 >= len(args) {
				return nil, fmt.Errorf("--host requires a value and is supported only by os skills")
			}
			i++
			out = append(out, "--harness", canonicalOSHarness(args[i]))
		default:
			out = append(out, args[i])
		}
	}
	return out, nil
}

func rewriteOSSkillsArgs(args []string) ([]string, int, error) {
	rewritten, err := rewriteOSAliasFlags(args, true)
	if err != nil {
		return nil, 0, err
	}
	positional := 0
	for i := 0; i < len(rewritten); i++ {
		if rewritten[i] == "--harness" {
			i++
			continue
		}
		if !strings.HasPrefix(rewritten[i], "-") {
			positional++
		}
	}
	return rewritten, positional, nil
}

func canonicalOSHarness(value string) string {
	switch strings.TrimSpace(value) {
	case "claude", "claude-code":
		return "claude_code"
	default:
		return strings.TrimSpace(value)
	}
}

func canonicalOSRouteClass(value string) string {
	switch strings.TrimSpace(value) {
	case "extraction", "extract", "classify":
		return "classify-extract"
	case "architecture", "architect", "planning":
		return "plan-decompose"
	default:
		return strings.TrimSpace(value)
	}
}

// The pg recall-engine factory is installed by the internal/store organ (ADR-0005:
// the spine stays dependency-free; backend construction lives at the composition
// root, and the organ owns the mechanics).
func init() {
	storecli.InstallPgvectorEngineFactory()
}

func sortedMapKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hasUsageFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func routeUsage(sub string) string {
	switch sub {
	case "cost-doctor":
		return "usage: compound route cost-doctor <provider-calls.ndjson> [--limit N|--tail N] [--record]"
	case "model-capabilities":
		return routeevidencecli.ModelCapabilitiesUsage()
	case "promotion-apply":
		return routeevidencecli.PromotionApplyUsage()
	case "promotion-revert":
		return routeevidencecli.PromotionRevertUsage()
	case "promotion-record":
		return "usage: compound route promotion-record --review <route_promotion.json> --rationale <text> [--json]"
	case "execution-profile":
		return routeevidencecli.ExecutionProfileUsage()
	case "ev":
		return routeEvUsage
	default:
		return "usage: compound route <execution-profile (--list|--profile ID) [--json]|evidence-authority [--json] [--recent N]|lane-projection [--harness H] [--json]|model-capabilities [--json|--yaml|--check-view FILE]|import <execution-route-evidence.json>|import-model-capabilities <model-capabilities.yaml>|import-promotion-criteria <promotion-criteria.yaml>|promotion-review [--lane ID] [--json]|promotion-record --review <route_promotion.json> --rationale <text> [--json]|promotion-apply --review <route_promotion.json> --rationale <text> --work W [--confirm] [--json]|promotion-revert --applied <route_promotion.applied.json> --work W [--confirm] [--json]|import-provider-ledger <provider-calls.ndjson> [--limit N|--tail N]|import-account-projection <admin-accounts.json>|cost-doctor <provider-calls.ndjson> [--limit N|--tail N] [--record]|speed (observe [--from <probe.json>]|list) [--json]|outcome-review [(--route-id ID|--route-evidence file)] (--receipt file|--receipt-id ID|--work-id ID) [--target file] [--record]|plan (--candidate C [--target file]|--capability-class C [--privacy-sensitive] [--latency-class interactive|batch|overnight] [--read-only])|ev --capability-class C --deviation-lane L [--risk-grade gate|judge|tidy] [--class-bucket unknown] [--json]>"
	}
}

func decisionSourceKind(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "note"
	}
	return raw
}

func importDecisionSource(store *mem.Store, path, sourceKind string, now time.Time) (decisions.BrainDecisionReport, error) {
	switch sourceKind {
	case "note":
		return decisions.ImportNoteDecision(store, path, now)
	default:
		return decisions.BrainDecisionReport{}, fmt.Errorf("unsupported decision source kind %q (supported: note)", sourceKind)
	}
}

func importDecisionSourceDir(store *mem.Store, root, sourceKind string, now time.Time) ([]decisions.BrainDecisionReport, error) {
	switch sourceKind {
	case "note":
		return decisions.ImportNoteDecisionDir(store, root, now)
	default:
		return nil, fmt.Errorf("unsupported decision source kind %q (supported: note)", sourceKind)
	}
}

func eligibleDecisionSourceFiles(root, sourceKind string) ([]string, error) {
	switch sourceKind {
	case "note":
		return decisions.EligibleNoteDecisionFiles(root)
	default:
		return nil, fmt.Errorf("unsupported decision source kind %q (supported: note)", sourceKind)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `compound — local meaning + execution substrate

usage:
  compound version
compound home <init|doctor|where|backup|export|verify|restore|archive-history|restore-history|compact|private-custody> [--home P] [--local] COMPOUND_HOME runtime body + archive/restore; private-custody repair is explicit permission repair; archive-history requires --kind work|lease|review|comparison|create-plan|memory-candidate to archive (or --dry-run to preview) and accepts --path <rel> and --max-files <N>
  compound envelope <validate <file>...|kinds>                  typed runtime envelopes
  compound schema <list|export|validate>                        JSON Schema export + contract-aware validation
  compound contract <compile|import-hooks|hooks-authority> ...  EffectiveContract and installed hook authority
  compound orient --harness H [--cwd D] [--task T] [--topic S]  ContextPack orient from records excluded; --topic narrows ambient recall
  compound mcp-transport                                      stdio MCP server for initialized clients: bounded finding evidence + orient recall; no memory promotion/authority
  compound console [dev|build|serve] --operator                  own the canonical local console lifecycle on :3777
  compound console status [--json]                               verify console identity, HTTP, and static-asset health
compound work <create-plan|admit-plan|review-card-plan|create|import-packet|show|list|leases|active-packet-proof|proof-chain-doctor|reconcile-run-attempts|continue|dispatch-plan|dispatch-claim|dispatch-execute|dispatch-advance|dispatch-close-plan|dispatch-close|dispatch-step|claim-plan|claim|advance-plan|advance|observe-run|action-plan|act|close-plan|close> one work_id: intent -> context -> action -> receipt -> transition
compound workbench [--json] [--recent N] [--actor A] [--active-packet-surface S] [--source-packet packet.md] [--packet-dir D] [--capability-task T] [--capability-harness H] [--capability-limit N] [--route-class C] [--route-privacy-sensitive] read-only open work / receipt / transition board; optional packet offers, bounded capability offers, and route plan; no dispatch
compound attention [--json] [--home P] ranked operator-attention projection from reviews, workbench, and executors; no mutation
compound activity <status|enable|disable|drain|sweep|aw materialize|tier1 collect> Tier-0 + Tier-1 ambient-activity family (compound.activity_observation.v0.2): content-free app/idle/lock exhaust -> day-rotated observed/activity segments+spans; AW sidecar reader joins events across hostnames (aw materialize); Tier-1 envelope collector captures pane_cwd/git_remote/agent_kind (tier1 collect); all writes are grant-gated + fail-closed; zero auto memory candidates
compound narrate [--json|--text] [--mood decide|wander|worry] [--recent N] [--home P] deterministic correspondent script (compound.narrate.v0.1) composed over attention/orient-changed/receipts; also embedded in orient --operator; reads the orient cursor, never advances it
compound executors [--json] read-only executor projection from Workbench, leases, and profile guide; no health overclaim
compound lanes matrix [--json] [--projection-dir DIR] [--harness H] [--route R] read-only (harness x lane) projection over the launch owner's published profile-conformance; enumerates from the registry, never a directory listing; an unbound combination reports unknown, never possible, because the projection publishes no wire protocol or tap binding
compound presence [--json] [--ttl D] [--include-stale] [--tail-bytes N] read-only "who is here now" projection from lease holders + recent work-log actors; presence follows activity, never spawn provenance, so an agent this runtime never launched still appears; writes nothing
compound stream (--once [--backlog N] | --follow [--cursor BYTE]) [--json] [--home P] read-only byte-cursor tail over events/work.jsonl + ledger/seq; --once is a bounded snapshot (last N + end-of-log cursor + seq + generated_at), --follow is a live JSONL tail; never takes the work-event write lock
compound behavioral-lens <list|run|compose|signals> composable behavioral-lens framework (compound.behavioral_lens_signal.v0.1): uniform observe->InferredSignal plug + visibility-gated composition; sycophancy (paired-stance shift) is the first lens; evidence-only, never a gate; operator_private signals stay under $COMPOUND_HOME
compound lens-modulate <arm|thresholds|modulate|revert> governed modulator: turns a crossing behavioral-lens signal into a REVERSIBLE, PolicyDecision-gated route/role demote through act; default off + calibration-guarded; modulate is dry-run unless --confirm; operator_private modulations stay operator-local
compound act repoint --target-path P --target-id KEY --value V --work W [--confirm] [--capability CAP] generic governed OUTBOUND write through the act seam (config-key repoint); dry-run unless --confirm; requires an allowing PolicyDecision; pre/post images + receipt land in $COMPOUND_HOME/events
compound act dns-record --service S --host H --address A --remote-host RH --work W [--confirm] governed ADDITIVE DNS record on a router over an injectable ssh transport; external_side_effect held by default; preflight-resolves the service:* entity
compound act vhost --service S --server-name N --upstream U --remote-host RH --work W [--confirm] governed ADDITIVE NPM vhost block on an apps host over the same seam; additive-only, held by default, pre/post images + receipt in $COMPOUND_HOME/events
compound land-lease <acquire|release|status|steal> TTL-bounded single-holder landing mutex under $COMPOUND_HOME; steal records a reason + prior holder
compound finding <record|list|show|doctor> private direct-write home for durable operational findings (evidence-class, never ledger); record accepts bounded --body / --body-file / --stdin and returns a minimal body-free JSON ack with status=recorded|duplicate; doctor flags superseded/stale
compound capture --source S [--text T] [--work W] | capture show <cid> gesture-fired capture front door: content-addressed body under observed/capture/ (never ledger), mints ONE observed-class proposed candidate (never auto-promotable); day-salted dedup
compound intent <plan|review|apply> expressive utterance -> task-scoped ContextPack -> reviewable existing work create-plan; ambiguity parks and apply requires an explicit reviewer
compound placement <resolve|target> where a write belongs: positive canonical-target redirect for a blocked path (guidance, not a gate)
compound schedule <list|show|doctor> [--json] [--source user|system|crontab|all] [--namespace N] [--stale-days N] read-only launchd + crontab projection; no --fix
  compound browser <list|show|doctor|settings|route|goal> [--json] [--instance UUID] [--swiftshader-only] [--stale-min M] [--idle-cpu-pct PCT] [--fix|--dry-run|--yes-each] process doctor, governed backend switch, bounded public goal; see docs/architecture/execution-plane.md
  compound doctor [--json] [--base DIR] [--binary PATH] one legible operator health surface: hooks, leases, findings, placement freshness, retention; writes nothing
compound store <doctor|init|status|migrate|sync|embed|recall> [--json]  EXPERIMENTAL/unsupported Postgres+pgvector; requires COMPOUND_STORE=postgres + COMPOUND_EXPERIMENTAL_POSTGRES=1 + COMPOUND_POSTGRES_PROFILE + COMPOUND_DSN; DSNs are never rendered/persisted
compound retention <classes|scan|plan> [--json]                 declare and inspect artifact retention; plans only, never reclaims
compound proof-check [--json] [--dry-run] [--dev] [--repo P] [--out FILE] deterministic verification bundle; --dev is fast, cache-enabled, non-certifying feedback while canonical proof stays fresh
  compound director propose [--dry-run|--apply] --json          suggested work; apply may evidence-admit job_health but never executes it
  compound release export-public [--repo P] [--target archive.tar.gz] [--prefix NAME] [--json] public source archive from git archive HEAD with private/runtime path gate
  compound reviews [--json] [--area A] [--include-archived] read-only owner-review artifact queue; no apply
  compound reviews resolve --item ID --area AREA --expected-revision REVISION --decision reviewed|parked|dismissed --reason TEXT [--json]; add --undo --reason TEXT to restore visibility
  compound reviews binding-plan --item ID --area AREA [--source-work W] [--source-run-kind K --source-run ID] [--fallback-work W] [--json]
  compound reviews binding-record --item ID --area AREA --expected-review-sha256 SHA --governed-work W [--source-work W] [--source-run-kind K --source-run ID] [--fallback-work W] [--json]
  compound reviews record-finding (--work-id W|--auto-work) --target state/runs/.../finding.json --summary S [--root DIR] [--capability review.finding] [--source-artifact state/runs/.../artifact.json] [--source-trace trace_id] [--evidence-ref REF...]
  compound policy <evaluate|import-placement-rules|placement-authority|import-standing-grants|standing-grants-authority|import-operator-state|operator-authority|import-operator-prompts|operator-prompts-authority|import-operator-approval-frontier|operator-approval-frontier-authority|import-release-goal-holds|release-goal-holds-authority|autonomy-halt> ActionIntent -> PolicyDecision; import policy authorities / mutate halt through its locked command
  compound hook <name> [--enforce] <fixture.json>               run lifecycle hook over fixture; --enforce exits nonzero on pre-tool holds
  compound harness <list|NAME|observe [registry.yaml|--os-entrypoints file]|posture-plan --harness H|drift|generate|permission-mode|instruction-pack>
  compound transition show <id>                                 inspect a Transition or envelope
	  compound route lane-projection [--harness H] [--json]          read-only executor lane input from Compound route authority
	  compound route import <execution-route-evidence.json>          import route metadata -> Transition projection
	  compound route model-capabilities [--json|--yaml|--check-view FILE] read/project Compound model-router authority; drift check exits nonzero
	  compound route import-model-capabilities <model-capabilities.yaml> additive migration/backfill bridge into Compound route authority
	  compound route import-promotion-criteria <promotion-criteria.yaml> Promotion criteria YAML -> Compound route review authority
	  compound route promotion-review [--lane ID] [--json] Review model-lane claim promotion evidence, no authority mutation
	  compound route promotion-record --review route_promotion.json --rationale TEXT [--json] Record reviewed hold decision, no authority mutation
	  compound route promotion-apply --review route_promotion.json --rationale TEXT --work W [--confirm] [--json] governed one-claim authority mutation; dry-run by default
	  compound route promotion-revert --applied route_promotion.applied.json --work W [--confirm] [--json] governed exact-pre-image undo; dry-run by default
  compound route evidence-authority [--json] [--recent N] read-only route evidence authority
  compound route import-provider-ledger <provider-calls.ndjson> [--limit N|--tail N] provider-call NDJSON -> route/cost projections
  compound route import-account-projection <admin-accounts.json>  switchback/admin-accounts@1 -> projections/account-projection.json (lane admission gate)
  compound route execution-profile (--list|--profile ID ...) [--json] provider-neutral resolved intent; no provider call or home mutation
  compound route cost-doctor <provider-calls.ndjson> [--limit N|--tail N] [--record] provider cost coverage -> route observation/no-promote
  compound route outcome-review (--route-id ID|--route-evidence file) --receipt file [--target file] [--record] join route evidence to work outcome
  compound route plan --candidate C [--target file]              route_observation candidate -> reviewed routing-policy draft
  compound route plan --capability-class C [--privacy-sensitive] planned lane chain from Compound route authority
  compound route ev --capability-class C --deviation-lane L [--risk-grade gate|judge|tidy] [--class-bucket unknown] [--json] price a deviation against the served policy line (compound.route.evcard.v0.1); advisory, never auto-blocks
	compound memory <candidate|candidates|import-page|import-topics|adjudicate|recall|timeline|fold|position|query|search --lane L|engine-doctor|engine-compare [--allow-expansion]|bakeoff <init|run>|doctor>
	              spine memory: candidates, claims, positions, patterns, evidence
  compound corpus <index --lane L [--rebuild]|doctor --lane L [--json]> rebuildable corpus vector indexes
  compound synthesis sweep [--stage cluster,contradict,corroborate,decay,reconcile] [--json]
                memory synthesis reports; contradictions append needs_review markers only; decay files expired parks
  compound distiller eval <case.json|cases-dir> [--record]    Offline request/response/adjudication eval corpus, no model call
  compound distiller eval-plan --candidate C [--corpus dir]   eval_case candidate -> reviewed eval fixture draft, no corpus mutation
  compound distiller improvement-plan --candidate C [--target file] method_upgrade/policy_patch -> reviewed distiller improvement draft
  compound distiller improvement-review <review.yaml>           validate reviewed distiller improvement decision, no source mutation
  compound distiller rule-plan --candidate C [--target file]  failure_pattern candidate -> reviewed rule draft, no doctrine mutation
  compound distiller taste-plan --candidate C [--target file] taste candidate -> reviewed taste standard draft, no doctrine mutation
  compound distiller request --trace-ref T [--work-id W] [--raw f] [--context-pack f] [--work f] [--receipt f] [--envelope f]... [--event-trace trace_id]... [--record]
  TraceDistiller LLM request packet, no model call
  compound distiller extract --request <request.json|distiller_request:id> --eval-corpus dir --openai-compatible-url URL --model M [--api-key-env ENV]
  gated rented model extraction; provider output stays candidate-only
  compound distiller import-conversation-coverage <summary.json>
                                                                 conversation corpus coverage -> SourceObservation/no-promote; no row import, Dream execution, or memory promotion
  compound distiller import-conversation-mining <report.json> [--mined-tags rows.jsonl] [--lossiness lossiness.json]
                                                                 mined conversation source quality -> SourceObservation/no-promote; no Dream execution or memory promotion
  compound distiller import-session-lenses <multi-lens.json>
                                                                 existing session distill lenses -> SourceObservation/no-promote; no row import or memory promotion
  compound distiller run [--llm-response response.json [--request request.json]|trace.json] | doctor [--writeback] | prior-state Trace -> candidate fan-out, adjudication, prior state
compound capability <add|resolve|surface|observe|consume|consume-receipts|usage|list|graph|authority|lifecycle-review|external-intake|outbound-distribution-plan|outbound-distribution-verdict-plan|outbound-distribution-verdict-review|outbound-distribution-dry-run-receipt|outbound-distribution-dry-run-review|external-intake-review-plan|external-intake-evaluation-plan|external-intake-import-reviewed|external-intake-review|tool-boundary-doctor|skill-source-doctor|import-topology|import-connections|import-gateway|import-skills|import-evaluations|import-surfacing-misses|evaluation-plan|evaluation-review|skill-plan|skill-candidate-review|skill-candidate-decision> capability graph: auto-surface, never auto-execute; usage folds offers + Skill fires; lifecycle-review records hash-bound operator disposition …
 compound panel <import-lenses|lenses|plan|run|verdict-record|show> perspective panel planning and dry-run verdicts
  compound local-models advise [--lm-studio-url U] [--required MODEL]... [--min-context MODEL=TOKENS]... [--host-context-limit N] [--idle-ttl DURATION] [--json] local model context/load/unload advice with lifecycle intents; reads configured local provider, no mutation
	compound authority import [f] | path <f> | path-registry <f> | git <repo> [--json] | dirty-plan <repo> [--work-id id] [--action-intent id ...] [--json] | dirty-review <review.yaml> [--json] | worktree-retire <plan|review|apply> | git-index-lock <plan|review|apply> | import-os-absorption <matrix.json> | os-absorption [--json] | os-absorption-doctor [--json] | import-infra-database <database.yaml> | import-infra-services <services.yaml> | import-model-catalog <provider-registry.json> | model-catalog [--family F] [--model M] [--json] | model-catalog-doctor [--source P] [--json] | import-job-roster <f> | import-job-token-governance <f> | job-roster-authority [--json] | job-health-plan | job-health-review <review.yaml> [--decision <enum>] [--rationale <text>] [--confidence <0..1>] [--next-action <text>] [--json] | resolve <token> | <entity> import/query authority through spine memory
  compound decisions show <decision://ref|decision_id|claim_id> [--json] | decisions import [--source <md>] [--source-dir <dir>] [--source-kind note] [--brain <md>] [--policy <DEC.md>] [--policy-dir <dir>] | decisions authority [--source <md>] [--source-dir <dir>] [--source-kind note] [--policy <DEC.md>] [--policy-dir <dir>] [--json] | decisions candidate-plan --candidate C [--target file] | decisions [--kind=K] | decisions doctor | decisions source-doctor --template <md> [--format-doc <md>] | decisions repair-plan [--policy-dir <dir>] [--source <md>] [--source-dir <dir>] [--source-kind note] [--brain <md>] | decisions repair-apply --plan <json> [--write-reviewed]
                                                                  import/query decisions through spine memory; verify source format; emit/apply lifecycle repair artifacts
compound runtime doctor [--project-contract <yaml> | --switchback-pulse <json> | <yaml>] [--live] [--writeback] | runtime repair-plan [--project-contract <yaml> | --switchback-pulse <json> | <yaml>] [--live] | runtime repair-review <review.yaml> RuntimeBinding health/live evidence + gated writeback/review artifacts
compound projection doctor <freshness.yaml> [--writeback] [--source-observe] | projection dashboard-read-model <read-model.json> [--max-age-hours N] [--writeback] [--source-observe] | projection repair-plan <freshness.yaml> | projection repair-review <review.yaml>
                                                               ProjectionFreshness + gated writeback/source observation/regeneration review
compound product import <product.yaml>|--project-contract <project.yaml> [--release <release.yaml>] | signal <signal.yaml>|--asc-datapoints <json> --product <id> --app-id <asc_id> [--metric <metric>]|--asc-sales-series <jsonl> --product <id> --app-id <asc_id> --before <range> --after <range> --change-event <text> [--metric <metric>]|--gsc-report <json> --product <id> --site <site> [--metric <metric>]|--ga4-report <json> --product <id> --property <id> [--metric <metric>]|--adsense-report <json> --product <id> --account <id> [--metric <metric>]|--posthog-report <json> --product <id> --project <id> [--metric <metric>] | loops [--product <id>] [--json] [--recent N] [--stale-days D] | canonical-metrics <json> --product <id> | hypothesis-plan --candidate C [--target file] | observation-verdict-plan --verdict <panel-verdict.json> [--target file] | observation-verdict-review <review.yaml> | source-issue-plan --product P --issue I [--target file] | review <review.yaml> --reviewed-by <actor> | source-issue-review <review.yaml> | timeline <product> | doctor <product> [--writeback]
                                                               Product registry + signal/hypothesis loop
compound source observe <source-observation.yaml> | observe-batch <batch.json> (--dry-run|--apply-product-signals|--apply-source-observations|--apply-knowledge|--apply-route-evidence|--apply-events|--review-candidates) [--json] | intake [--limit N] [--json] | intake-work-plan --card <card_id> [--actor A] [--json] SourceObservation or generic ObservationBatch intake
compound event import-os <events.ndjson> | event show <trace_id> [--json] | event list [--stream NAME] [--limit N] [--json] | event authority [--json] OS-style append-only events -> TraceEnvelope evidence; forensic reader served from COMPOUND_HOME; optional trace-backed review finding producer
compound event-store migrate (--dry-run | --apply | --verify) [--batch N] [--json] [--home P] fold legacy loose events/<id>.json records into segments; manifest counts+hashes, originals archived, fail-closed completion gate; --verify reports the gate + degraded:legacy_writer_active
compound event-store rebaseline [--home P] [--work W] [--confirm] [--json]     governed re-baseline of the frozen-stream doctor's byte-size baseline; dry-run preview by default, --confirm requires an allowing PolicyDecision
compound event-store repair (plan | review | supersede | apply) ...             governed post-completion loose-record repair; exact operator review + current running-work PolicyDecision, content-addressed recovery, segment truth preserved
compound writeback <gate|no-promote>                           OwnerWritebackGate + NoPromoteDecision

retired Gen-1 tombstones:
  compound distill|scope|promote|build|reconcile|timeline|recall|pack|ledger
  These exit 2 and point to canonical compound memory/distiller surfaces.
`)
}

// buildCapabilitySurfaceReport adapts CapabilityGraph offers into the stable
// machine surface consumed by CLI/read-model callers. It only reads graph state.
func buildCapabilitySurfaceReport(store *capgraph.Store, homePath, task, harness string) (capgraph.SurfaceReport, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return capgraph.SurfaceReport{}, fmt.Errorf("capability task must be non-empty")
	}
	harness = strings.TrimSpace(harness)
	revisionBefore, err := capgraph.AuthorityRevision(homePath)
	if err != nil {
		return capgraph.SurfaceReport{}, err
	}
	var offers []capgraph.Surfaced
	if harness != "" {
		offers, err = store.SurfaceForHarness(task, harness)
	} else {
		offers, err = store.Surface(task)
	}
	if err != nil {
		return capgraph.SurfaceReport{}, err
	}
	stats, err := store.Stats()
	if err != nil {
		return capgraph.SurfaceReport{}, err
	}
	records, err := capgraph.LoadSourceAuthority(homePath)
	if err != nil {
		return capgraph.SurfaceReport{}, err
	}
	report := capgraph.NewSurfaceReport(task, harness, offers, stats, len(records))
	revisionAfter, err := capgraph.AuthorityRevision(homePath)
	if err != nil {
		return capgraph.SurfaceReport{}, err
	}
	if revisionBefore != revisionAfter {
		return capgraph.SurfaceReport{}, fmt.Errorf("capability graph authority changed during resolution: before=%s after=%s", revisionBefore, revisionAfter)
	}
	report.AuthorityRevision = revisionBefore
	matchedEvidence, err := capgraph.ExternalRepoMatchedEvidenceForTask(homePath, task, 5)
	if err != nil {
		return capgraph.SurfaceReport{}, err
	}
	report.MatchedEvidence = matchedEvidence
	return report, nil
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" || arg == "help" {
			return true
		}
	}
	return false
}

func cmdEvent(args []string) {
	if len(args) >= 1 && args[0] == "authority" {
		jsonOut := false
		for _, arg := range args[1:] {
			switch arg {
			case "--json":
				jsonOut = true
			default:
				fmt.Fprintln(os.Stderr, "event authority: unknown arg", arg)
				os.Exit(2)
			}
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		report, err := eventsource.LoadAuthority(homePath, 5)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# event trace authority\n\n")
		fmt.Printf("- status: %s\n- authority: %s\n- traces: %d\n", report.Status, report.AuthorityRef, report.TraceCount)
		if len(report.SourceAuthority) > 0 {
			latest := report.SourceAuthority[len(report.SourceAuthority)-1]
			fmt.Printf("- source_authority: %s\n- source: %s\n", latest.ID, latest.SourcePath)
		}
		for _, trace := range report.RecentTraces {
			fmt.Printf("- trace: %s %s %s\n", trace.ID, trace.SourceKind, trace.Summary)
		}
		return
	}
	if len(args) >= 1 && args[0] == "show" {
		// runEventShow (cmd/compound/event_show.go) resolves ANY event-store
		// record kind — trace_ ids stay on eventsource.ShowTrace unchanged;
		// rec_/hook_/act_/pol_/tr_ ids resolve through the shared eventstore
		// read seam. cmdEvent only calls os.Exit on a non-zero code (matching
		// every other branch in this function): the success path must return
		// normally so existing tests can call cmdEvent directly in-process.
		if code := runEventShow(args[1:], os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return
	}
	if len(args) >= 1 && args[0] == "list" {
		jsonOut := false
		stream := ""
		limit := 20
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--json":
				jsonOut = true
			case "--stream":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "event list: --stream needs value")
					os.Exit(2)
				}
				i++
				stream = args[i]
			case "--limit":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "event list: --limit needs value")
					os.Exit(2)
				}
				i++
				n, err := strconv.Atoi(args[i])
				if err != nil || n < 0 {
					fmt.Fprintln(os.Stderr, "event list: --limit must be non-negative integer (0 = all)")
					os.Exit(2)
				}
				limit = n
			default:
				fmt.Fprintln(os.Stderr, "event list: unknown arg", args[i])
				os.Exit(2)
			}
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		traces, err := eventsource.ListTraces(homePath, stream, limit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "event list:", err)
			os.Exit(1)
		}
		report := eventsource.TraceListReport{
			SchemaVersion: eventsource.TraceListReportSchema,
			Status:        "ok",
			AuthorityRef:  eventsource.TraceEventsAuthorityRef,
			Stream:        stream,
			Limit:         limit,
			Count:         len(traces),
			Traces:        traces,
		}
		if jsonOut {
			printJSON(report)
			return
		}
		fmt.Printf("# event traces\n\n")
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		if stream != "" {
			fmt.Printf("- stream: %s\n", stream)
		}
		fmt.Printf("- count: %d\n", report.Count)
		for _, trace := range traces {
			fmt.Printf("- %s %s %s %s\n", trace.ID, trace.CreatedAt, trace.Stream, trace.Summary)
		}
		return
	}
	if len(args) < 2 || args[0] != "import-os" {
		fmt.Fprintln(os.Stderr, "usage: compound event import-os <events.ndjson> [--limit N] [--stream NAME] [--review-root DIR] [--review-summary TEXT] | event show <id> [--json] | event list [--stream NAME] [--limit N] [--json] | event authority [--json]")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	path := args[1]
	opts := eventsource.ImportOptions{}
	reviewRoot := ""
	reviewSummary := ""
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "event import-os: --limit needs value")
				os.Exit(2)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "event import-os: --limit must be non-negative integer")
				os.Exit(2)
			}
			opts.Limit = n
		case "--stream":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "event import-os: --stream needs value")
				os.Exit(2)
			}
			i++
			opts.Stream = args[i]
		case "--review-root":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "event import-os: --review-root needs value")
				os.Exit(2)
			}
			i++
			reviewRoot = args[i]
		case "--review-summary":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "event import-os: --review-summary needs value")
				os.Exit(2)
			}
			i++
			reviewSummary = args[i]
		default:
			fmt.Fprintln(os.Stderr, "event import-os: unknown arg", args[i])
			os.Exit(2)
		}
	}
	if strings.TrimSpace(reviewSummary) != "" && strings.TrimSpace(reviewRoot) == "" {
		fmt.Fprintln(os.Stderr, "event import-os: --review-summary requires --review-root")
		os.Exit(2)
	}
	report, err := eventsource.ImportOSEvents(homePath, path, opts, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "event import-os:", err)
		os.Exit(1)
	}
	fmt.Println("# event import")
	fmt.Printf("- status: %s\n- stream: %s\n", report.Status, report.Stream)
	if report.AuthorityRef != "" {
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
	}
	if report.SourceAuthorityID != "" {
		fmt.Printf("- source_authority: %s\n", report.SourceAuthorityID)
	}
	fmt.Printf("- imported: %d\n", report.Imported)
	fmt.Printf("- skipped: %d\n", report.Skipped)
	for _, id := range report.TraceIDs {
		fmt.Printf("- trace: %s\n", id)
	}
	if strings.TrimSpace(reviewRoot) != "" {
		fmt.Printf("- review_findings: %d\n", len(report.TraceIDs))
		for _, id := range report.TraceIDs {
			cmdReviewsRecordFinding([]string{
				"--auto-work",
				"--target", eventReviewFindingTarget(report.Stream, id),
				"--root", reviewRoot,
				"--summary", eventReviewFindingSummary(report.Stream, id, reviewSummary),
				"--source-trace", id,
				"--evidence-ref", eventsource.TraceEventsAuthorityRef,
			})
		}
	}
}

func eventReviewFindingTarget(stream, traceID string) string {
	streamSlug := reviewartifact.Slugify(reviewartifact.FirstNonEmpty(stream, "event-import"), 80)
	if streamSlug == "" {
		streamSlug = "event-import"
	}
	traceSlug := reviewartifact.Slugify(traceID, 96)
	if traceSlug == "" {
		traceSlug = reviewartifact.Digest12(traceID)
	}
	return filepath.ToSlash(filepath.Join("state", "runs", "compound-event-trace-reviews", streamSlug, traceSlug+".json"))
}

func eventReviewFindingSummary(stream, traceID, override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	return fmt.Sprintf("Imported OS event trace %s from %s; record trace-backed review finding.", traceID, reviewartifact.FirstNonEmpty(stream, "event-import"))
}

func readTypedEnvelopeOrDie(path string) envelope.Envelope {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read envelope %s: %v\n", path, err)
		os.Exit(1)
	}
	var probe struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		fmt.Fprintf(os.Stderr, "decode envelope %s: %v\n", path, err)
		os.Exit(1)
	}
	env := envelope.New(probe.SchemaVersion)
	if env == nil {
		fmt.Fprintf(os.Stderr, "decode envelope %s: unknown schema_version %q\n", path, probe.SchemaVersion)
		os.Exit(1)
	}
	if err := json.Unmarshal(data, env); err != nil {
		fmt.Fprintf(os.Stderr, "decode envelope %s: %v\n", path, err)
		os.Exit(1)
	}
	if err := env.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid envelope %s: %v\n", path, err)
		os.Exit(1)
	}
	return env
}

// cmdMemory is P6: the owned memory ledger re-bound through the spine. candidate-first
// (reject|park|promote), MVCC-by-fold (as-of recall), provenance + contradiction-over-delete.
func cmdKnowledge(args []string) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("usage: compound knowledge <import|extract> ...")
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound knowledge <import|extract> ...")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	store, err := mem.Open(homePath)
	die(err)
	switch args[0] {
	case "import":
		rest := args[1:]
		if err := validateKnownFlags(rest, nil, map[string]bool{"--json": true}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, "usage: compound knowledge import <export.json> [--json]")
			os.Exit(2)
		}
		path := ""
		for _, arg := range rest {
			if !strings.HasPrefix(arg, "-") {
				path = arg
				break
			}
		}
		if path == "" {
			fmt.Fprintln(os.Stderr, "usage: compound knowledge import <export.json> [--json]")
			os.Exit(2)
		}
		report, err := knowledge.ImportFile(homePath, store, path, time.Now())
		die(err)
		if hasFlag(rest, "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# knowledge import %s\n", path)
		fmt.Printf("- items: %d\n- imported: %d\n- echoes: %d\n- reused: %d\n", report.Items, report.Imported, report.Echoes, report.Reused)
		for _, id := range report.EvidenceIDs {
			fmt.Printf(" - evidence %s\n", id)
		}
		for _, id := range report.CandidateIDs {
			fmt.Printf(" - candidate %s\n", id)
		}
		for _, id := range report.ExtractionIDs {
			fmt.Printf(" - extraction %s\n", id)
		}
	case "extract":
		rest := args[1:]
		extractUsage := "usage: compound knowledge extract (--pending|--item <id>) (--fixtures dir | --openai-compatible-url U --model M --eval-corpus dir [--eval-root dir] [--api-key-env E]) [--source-kind K] [--limit N] [--json]"
		limit, err := intFlag(rest, "--limit")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintln(os.Stderr, extractUsage)
			os.Exit(2)
		}
		opts := knowledge.ExtractOptions{
			Pending:             hasFlag(rest, "--pending"),
			ItemID:              valueFlag(rest, "--item"),
			Limit:               limit,
			SourceKind:          valueFlag(rest, "--source-kind"),
			FixturesDir:         valueFlag(rest, "--fixtures"),
			OpenAICompatibleURL: valueFlag(rest, "--openai-compatible-url"),
			Model:               valueFlag(rest, "--model"),
			APIKeyEnv:           valueFlag(rest, "--api-key-env"),
			EvalCorpusDir:       valueFlag(rest, "--eval-corpus"),
			EvalRoot:            valueFlag(rest, "--eval-root"),
		}
		if !opts.Pending && strings.TrimSpace(opts.ItemID) == "" {
			fmt.Fprintln(os.Stderr, extractUsage)
			os.Exit(2)
		}
		report, err := knowledge.Extract(homePath, store, opts, time.Now())
		// For a --pending bulk-drain pass, partial failures (some items succeed
		// while others fail) are the NORMAL case: Extract returns both a
		// populated report AND a non-nil error. Printing the JSON report FIRST
		// — then exiting non-zero — preserves the report as the authoritative
		// record of what did and did not get processed, instead of suppressing
		// the entire batch behind a single error line. Non-partial failures
		// (setup errors like a missing --fixtures dir) still fail closed
		// without a useful report to emit.
		if hasFlag(rest, "--json") {
			printJSON(report)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
		die(err)
		fmt.Println("# knowledge extract")
		fmt.Printf("- processed: %d\n", report.Processed)
		fmt.Printf("- created: %d\n", report.Created)
		fmt.Printf("- skipped: %d\n", report.Skipped)
		fmt.Printf("- failed: %d\n", report.Failed)
		for _, id := range report.CandidateIDs {
			fmt.Printf(" - candidate %s\n", id)
		}
		for _, id := range report.ExtractionIDs {
			fmt.Printf(" - extraction %s\n", id)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: compound knowledge <import|extract> ...")
		os.Exit(2)
	}
}

// cmdTransition gives proof-by-transition a first-class read path: inspect a Transition
// (or any emitted envelope) by id under COMPOUND_HOME/events, validated through the spine.
func cmdTransition(args []string) {
	if len(args) < 2 || args[0] != "show" {
		fmt.Fprintln(os.Stderr, "usage: compound transition show <id>")
		os.Exit(2)
	}
	id := args[1]
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	record, err := store.Read(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: not found under COMPOUND_HOME/events:", id)
		os.Exit(1)
	}
	kind, verr := envelope.ValidateBytes(record.Bytes)
	fmt.Println(string(record.Bytes))
	if verr != nil {
		fmt.Fprintf(os.Stderr, "[%s] INVALID: %v\n", kind, verr)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[%s] ok\n", kind)
}

func cmdRoute(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, routeUsage(""))
		os.Exit(2)
	}
	if hasUsageFlag(args) {
		fmt.Println(routeUsage(args[0]))
		return
	}
	if tryRouteExecutionProfileCommand(args) {
		return
	}
	if tryRouteExperimentPlanCommand(args) {
		return
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	if tryRoutePromotionCommand(args, homePath) {
		return
	}
	if tryRouteSpeedCommand(args, homePath) {
		return
	}
	if tryRouteEvCommand(args, homePath) {
		return
	}
	if args[0] == "evidence-authority" {
		recent := 6
		if v := valueFlag(args[1:], "--recent"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "route evidence-authority: --recent must be a non-negative integer")
				os.Exit(2)
			}
			recent = n
		}
		report, err := routeevidence.LoadAuthority(homePath, recent)
		if err != nil {
			fmt.Fprintln(os.Stderr, "route evidence-authority:", err)
			os.Exit(1)
		}
		if hasFlag(args[1:], "--json") {
			printJSON(report)
			return
		}
		fmt.Println("# route evidence-authority")
		fmt.Printf("- status: %s\n- authority: %s\n- authority_path: %s\n", report.Status, report.AuthorityRef, report.AuthorityPath)
		fmt.Printf("- route_evidence_count: %d\n", report.RouteEvidenceCount)
		fmt.Printf("- provider_ledger_evidence_count: %d\n", report.ProviderLedgerEvidenceCount)
		fmt.Printf("- token_usage_rows: %d\n", report.TokenUsageRows)
		fmt.Printf("- cost_known_rows: %d\n", report.CostKnownRows)
		if len(report.Recent) > 0 {
			last := report.Recent[len(report.Recent)-1]
			fmt.Printf("- latest_route_evidence: %s\n", last.ID)
			fmt.Printf("- latest_selected_target: %s/%s (%s)\n", last.Provider, last.Model, last.Protocol)
		}
		fmt.Println("- verdict: metadata_only_not_task_success")
		return
	}
	if args[0] == "plan" {
		candidateID := valueFlag(args[1:], "--candidate")
		capabilityClass := valueFlag(args[1:], "--capability-class")
		if hasFlag(args[1:], "--read-only") && (capabilityClass == "" || candidateID != "") {
			fmt.Fprintln(os.Stderr, "route plan: --read-only requires --capability-class without --candidate")
			os.Exit(2)
		}
		targetPath := valueFlag(args[1:], "--target")
		if (candidateID == "" && capabilityClass == "") || (candidateID != "" && capabilityClass != "") {
			fmt.Fprintln(os.Stderr, "usage: compound route plan (--candidate <candidate_id> [--target <file>]|--capability-class <class> [--privacy-sensitive])")
			os.Exit(2)
		}
		if capabilityClass != "" {
			plan, err := routeevidence.PlanCapabilityRoute(homePath, capabilityClass, routeevidence.CapabilityRoutePlanOptions{
				PrivacySensitive:            hasFlag(args[1:], "--privacy-sensitive"),
				LatencyClass:                valueFlag(args[1:], "--latency-class"),
				SuppressRefusalEvidence:     hasFlag(args[1:], "--read-only"),
				SuppressPendingObservations: hasFlag(args[1:], "--read-only"),
			}, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "route plan:", err)
				os.Exit(1)
			}
			if hasFlag(args[1:], "--json") {
				printJSON(plan)
				return
			}
			fmt.Printf("# route capability-plan %s\n", plan.ID)
			fmt.Printf("- status: %s\n- authority: %s\n- authority_path: %s\n", plan.Status, plan.AuthorityRef, plan.AuthorityPath)
			if plan.SourceAuthorityID != "" {
				fmt.Printf("- source_authority: %s\n", plan.SourceAuthorityID)
			}
			fmt.Printf("- capability_class: %s\n", plan.CapabilityClass)
			fmt.Printf("- privacy_sensitive: %t\n", plan.PrivacySensitive)
			if plan.RequiredLatencyClass != "" {
				fmt.Printf("- required_latency_class: %s (budget=%s)\n", plan.RequiredLatencyClass, emptyDash(plan.LatencyBudgetSource))
			}
			fmt.Printf("- routing_rule: %s\n", plan.RoutingRule)
			fmt.Printf("- chain_count: %d\n", len(plan.Chain))
			for _, lane := range plan.Chain {
				fmt.Printf("- lane: %d %s tier=%s cost_class=%s role=%s claim=%s harness=%s provider_route=%s effort=%s\n",
					lane.Position,
					lane.ModelID,
					lane.Tier,
					emptyDash(lane.CostClass),
					lane.GateRole,
					lane.ClaimLevel,
					emptyDash(lane.Harness),
					emptyDash(lane.ProviderRoute),
					emptyDash(lane.EffortDefault),
				)
				fmt.Printf("  lane_justification: %s\n", lane.Justification)
				if adj := lane.SpeedAdjustment; adj != nil {
					fmt.Printf("  speed_adjustment: declared=%s measured=%s (%.2f tok/s, ttft=%dms) role %s -> %s probe=%s\n",
						adj.DeclaredLatencyClass, adj.MeasuredLatencyClass, adj.MeasuredTokensPerSec, adj.MeasuredTTFTms, adj.PriorGateRole, adj.AdjustedGateRole, adj.ProbeID)
				}
			}
			if len(plan.Omitted) > 0 {
				fmt.Printf("- omitted_count: %d\n", len(plan.Omitted))
				for _, lane := range plan.Omitted {
					fmt.Printf("- omitted: %d %s reason=%s\n", lane.Position, lane.ModelID, lane.OmitReason)
				}
			}
			if len(plan.PendingObservations) > 0 {
				fmt.Printf("- pending_route_observations: %d\n", len(plan.PendingObservations))
				for _, observation := range plan.PendingObservations {
					fmt.Printf("- pending_observation: %s match=%s review=\"%s\"\n", observation.CandidateID, strings.Join(observation.MatchReasons, ","), observation.ReviewCommand)
				}
			}
			for _, warning := range plan.Warnings {
				fmt.Printf("- warning: %s\n", warning)
			}
			fmt.Println("- verdict: metadata_only_not_task_success")
			return
		}
		plan, err := routeevidence.BuildRoutePolicyPlan(homePath, candidateID, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "route plan:", err)
			os.Exit(1)
		}
		paths, err := routeevidence.RecordRoutePolicyPlan(homePath, plan)
		if err != nil {
			fmt.Fprintln(os.Stderr, "route plan:", err)
			os.Exit(1)
		}
		fmt.Printf("# route plan %s\n", plan.ID)
		fmt.Printf("- status: %s\n- candidate: %s\n", plan.Status, plan.CandidateID)
		fmt.Printf("- route_key: %s\n", plan.RouteKey)
		fmt.Printf("- policy_id: %s\n", plan.PolicyDraft.PolicyID)
		if plan.TargetPath != "" {
			fmt.Printf("- target: %s\n", plan.TargetPath)
			fmt.Printf("- target_exists: %t\n", plan.TargetExists)
		}
		if plan.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", plan.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n", len(plan.PolicyDraft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(plan.EvidenceRefs))
		fmt.Printf("- plan: %s\n- review: %s\n", paths.PlanPath, paths.ReviewPath)
		return
	}
	if args[0] == "lane-projection" {
		harnessFilter := valueFlag(args[1:], "--harness")
		projection, err := routeevidence.LaneProjectionFromHome(homePath, harnessFilter)
		if err != nil {
			fmt.Fprintln(os.Stderr, "route lane-projection:", err)
			os.Exit(1)
		}
		if hasFlag(args[1:], "--json") {
			printJSON(projection)
			return
		}
		fmt.Println("# route lane-projection")
		fmt.Printf("- authority: %s\n", projection.AuthorityRef)
		fmt.Printf("- source_revision: %s\n", projection.SourceRevision)
		if projection.HarnessFilter != "" {
			fmt.Printf("- harness_filter: %s\n", projection.HarnessFilter)
		}
		fmt.Printf("- lanes: %d\n", projection.LaneCount)
		fmt.Printf("- executor_owned (NOT emitted): %s\n", strings.Join(projection.ExecutorOwned, ", "))
		for _, lane := range projection.Lanes {
			effort, route := lane.NativeEffort, lane.Route
			if effort == "" {
				effort = "-"
			}
			if route == "" {
				route = "-"
			}
			fmt.Printf("  %-22s harness=%-14s effort=%-7s route=%s\n",
				lane.Lane, lane.Harness, effort, route)
		}
		return
	}
	if args[0] == "import-model-capabilities" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound route import-model-capabilities <model-capabilities.yaml>")
			os.Exit(2)
		}
		report, err := routeevidence.ImportModelCapabilitiesYAML(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "route import-model-capabilities:", err)
			os.Exit(1)
		}
		fmt.Println("# route import-model-capabilities")
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- authority_path: %s\n", report.AuthorityPath)
		fmt.Printf("- source_authority: %s\n", report.SourceAuthorityID)
		fmt.Printf("- models: %d\n", report.ModelCount)
		fmt.Printf("- capability_classes: %d\n", report.CapabilityClassCount)
		fmt.Printf("- routing_defaults: %d\n", report.RoutingDefaultCount)
		fmt.Println("- verdict: compound_primary")
		return
	}
	if args[0] == "outcome-review" {
		routeID := valueFlag(args[1:], "--route-id")
		routePath := valueFlag(args[1:], "--route-evidence")
		receiptPath := valueFlag(args[1:], "--receipt")
		receiptID := valueFlag(args[1:], "--receipt-id")
		workID := valueFlag(args[1:], "--work-id")
		targetPath := valueFlag(args[1:], "--target")
		ev, rec, err := routeevidence.ResolveOutcomeReviewInputs(homePath, routeevidence.OutcomeReviewSelector{
			RouteID:           routeID,
			RouteEvidencePath: routePath,
			ReceiptPath:       receiptPath,
			ReceiptID:         receiptID,
			WorkID:            workID,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "route outcome-review:", err)
			os.Exit(1)
		}
		review, err := routeevidence.BuildRouteOutcomeReview(ev, rec, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "route outcome-review:", err)
			os.Exit(1)
		}
		if hasFlag(args[1:], "--record") {
			review, err = routeevidence.RecordRouteOutcomeObservation(homePath, review, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "route outcome-review:", err)
				os.Exit(1)
			}
		}
		paths, err := routeevidence.RecordRouteOutcomeReview(homePath, review)
		if err != nil {
			fmt.Fprintln(os.Stderr, "route outcome-review:", err)
			os.Exit(1)
		}
		target := review.SelectedTarget
		fmt.Printf("# route outcome-review %s\n", review.ID)
		fmt.Printf("- status: %s\n", review.Status)
		fmt.Printf("- route_evidence: %s\n", review.RouteEvidenceID)
		fmt.Printf("- receipt: %s\n", review.ReceiptID)
		fmt.Printf("- selected_target: %s/%s (%s)\n", target.Provider, target.Model, target.Protocol)
		if len(review.CapabilityClasses) > 0 {
			fmt.Printf("- capability_classes: %s\n", strings.Join(review.CapabilityClasses, ","))
		}
		fmt.Printf("- receipt_outcome: %s\n", review.ReceiptOutcome)
		fmt.Printf("- state_changed: %t\n", review.ReceiptStateChanged)
		fmt.Printf("- assessment_class: %s\n", review.Assessment.Class)
		fmt.Printf("- should_review_policy: %t\n", review.Assessment.ShouldReviewPolicy)
		if review.CandidateID != "" {
			fmt.Printf("- candidate: %s\n", review.CandidateID)
			fmt.Printf("- no_promote: %s\n", review.NoPromoteID)
		}
		if review.TargetPath != "" {
			fmt.Printf("- target: %s\n", review.TargetPath)
			fmt.Printf("- target_exists: %t\n", review.TargetExists)
		}
		if review.TargetHash != "" {
			fmt.Printf("- target_hash: %s\n", review.TargetHash)
		}
		fmt.Printf("- required_evidence: %d\n", len(review.ReviewDraft.RequiredEvidence))
		fmt.Printf("- evidence_refs: %d\n", len(review.EvidenceRefs))
		fmt.Printf("- plan: %s\n- review: %s\n", paths.PlanPath, paths.ReviewPath)
		fmt.Println("- verdict: route_outcome_review_not_policy")
		return
	}
	if args[0] == "import-provider-ledger" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound route import-provider-ledger <provider-calls.ndjson> [--limit N|--tail N]")
			os.Exit(2)
		}
		limit, err := intFlag(args[2:], "--limit")
		if err != nil {
			fmt.Fprintln(os.Stderr, "route import-provider-ledger:", err)
			os.Exit(2)
		}
		tail, err := intFlag(args[2:], "--tail")
		if err != nil {
			fmt.Fprintln(os.Stderr, "route import-provider-ledger:", err)
			os.Exit(2)
		}
		summary, err := routeevidence.ImportProviderLedgerFile(homePath, args[1], routeevidence.ProviderLedgerImportOptions{Limit: limit, Tail: tail})
		if err != nil {
			fmt.Fprintln(os.Stderr, "route import-provider-ledger:", err)
			os.Exit(1)
		}
		fmt.Println("# route import-provider-ledger")
		fmt.Printf("- rows_read: %d\n", summary.RowsRead)
		fmt.Printf("- imported: %d\n", summary.Imported)
		if summary.FirstID != "" {
			fmt.Printf("- first_route_evidence: %s\n", summary.FirstID)
			fmt.Printf("- last_route_evidence: %s\n", summary.LastID)
		}
		if len(summary.Results) > 0 {
			last := summary.Results[len(summary.Results)-1]
			target := last.Evidence.SelectedTarget
			fmt.Printf("- last_request: %s\n", last.Evidence.RequestID)
			fmt.Printf("- last_selected_target: %s/%s (%s)\n", target.Provider, target.Model, target.Protocol)
			fmt.Printf("- last_transition: %s\n", last.Transition.ID)
		}
		fmt.Println("- verdict: metadata_only_not_task_success")
		return
	}
	if args[0] == "import-account-projection" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: compound route import-account-projection <admin-accounts.json>")
			os.Exit(2)
		}
		report, err := routeevidence.ImportAccountProjectionFile(homePath, args[1], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "route import-account-projection:", err)
			os.Exit(1)
		}
		fmt.Println("# route import-account-projection")
		fmt.Printf("- schema_version: %s\n", report.SchemaVersion)
		if report.SourceRevision != "" {
			fmt.Printf("- source_revision: %s\n", report.SourceRevision)
		}
		fmt.Printf("- imported_at: %s\n", report.ImportedAt)
		fmt.Printf("- providers: %d\n", report.ProviderCount)
		if len(report.Providers) > 0 {
			fmt.Printf("- provider_keys: %s\n", strings.Join(report.Providers, ","))
		}
		fmt.Printf("- projection: %s\n", report.ProjectionPath)
		fmt.Println("- verdict: projection_not_governed_registry")
		return
	}
	if args[0] == "cost-doctor" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound route cost-doctor <provider-calls.ndjson> [--limit N|--tail N] [--record]")
			os.Exit(2)
		}
		limit, err := intFlag(args[2:], "--limit")
		if err != nil {
			fmt.Fprintln(os.Stderr, "route cost-doctor:", err)
			os.Exit(2)
		}
		tail, err := intFlag(args[2:], "--tail")
		if err != nil {
			fmt.Fprintln(os.Stderr, "route cost-doctor:", err)
			os.Exit(2)
		}
		report, err := routeevidence.DoctorProviderLedgerCosts(args[1], routeevidence.ProviderLedgerImportOptions{Limit: limit, Tail: tail})
		if err != nil {
			fmt.Fprintln(os.Stderr, "route cost-doctor:", err)
			os.Exit(1)
		}
		if hasFlag(args[2:], "--record") && report.Status != "ok" {
			report, err = routeevidence.RecordCostDoctorObservation(homePath, report, time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "route cost-doctor:", err)
				os.Exit(1)
			}
		}
		fmt.Println("# route cost-doctor")
		fmt.Printf("- status: %s\n", report.Status)
		fmt.Printf("- rows_read: %d\n", report.RowsRead)
		fmt.Printf("- rows_checked: %d\n", report.RowsChecked)
		fmt.Printf("- token_usage_rows: %d\n", report.TokenUsageRows)
		fmt.Printf("- cost_known_rows: %d\n", report.CostKnownRows)
		fmt.Printf("- token_usage_missing_cost: %d\n", report.TokenUsageMissingCost)
		fmt.Printf("- unknown_project_rows: %d\n", report.UnknownProjectRows)
		fmt.Printf("- partial_metadata_rows: %d\n", report.PartialMetadataRows)
		if report.CandidateID != "" {
			fmt.Printf("- candidate: %s\n", report.CandidateID)
			fmt.Printf("- no_promote: %s\n", report.NoPromoteID)
		}
		fmt.Println("- verdict: route_cost_observation_not_policy")
		return
	}
	if len(args) != 2 || args[0] != "import" {
		fmt.Fprintln(os.Stderr, routeUsage(""))
		os.Exit(2)
	}
	result, err := routeevidence.ImportFile(homePath, args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "route import:", err)
		os.Exit(1)
	}
	target := result.Evidence.SelectedTarget
	fmt.Println("# route import")
	fmt.Printf("- route_evidence: %s\n", result.Evidence.ID)
	fmt.Printf("- route_transition: %s\n", result.Transition.ID)
	fmt.Printf("- request: %s\n", result.Evidence.RequestID)
	fmt.Printf("- selected_target: %s/%s (%s)\n", target.Provider, target.Model, target.Protocol)
	fmt.Printf("- status: %s\n", result.Evidence.Status)
	fmt.Println("- verdict: metadata_only_not_task_success")
}

// cmdHarness prints the HarnessRegistry — the spine's record of what each harness
// verifiably supports and where it is weak (docs/architecture/runtime.md §3).
func cmdHarness(args []string) {
	target := "list"
	if len(args) > 0 {
		target = args[0]
	}
	// Slice C.4: the AdapterRegistry-backed subcommands (run, executors)
	// live in harness_exec_cli.go and dispatch before the capabilities
	// registry is loaded -- they don't need it.
	if target == "capability-drift" {
		// Compares the DECLARED registry against the lane's real files. Dispatches
		// before the capabilities registry load below for the same reason run and
		// executors do: it reads the declaration itself, it is not a consumer of it.
		cmdHarnessCapabilityDrift(args[1:])
		return
	}
	if target == "settings-matrix" {
		if code := runHarnessSettingsMatrix(args[1:], os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return
	}
	if target == "posture-plan" {
		if code := runHarnessPosturePlan(args[1:], os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return
	}
	if target == "readiness" {
		if code := runHarnessReadiness(args[1:], os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return
	}
	if target == "run" {
		cmdHarnessRun(args[1:])
		return
	}
	if target == "executors" {
		cmdHarnessExecutors(args[1:])
		return
	}
	reg := harness.DefaultRegistry()
	if target == "permission-mode" {
		harnessName := valueFlag(args[1:], "--harness")
		if harnessName == "" {
			fmt.Fprintln(os.Stderr, "usage: compound harness permission-mode --harness <name> [--json]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(set COMPOUND_HOME)")
			os.Exit(1)
		}
		policy, revision, err := harness.LoadPermissionAuthority(homePath, harnessName)
		die(err)
		if hasFlag(args[1:], "--json") {
			die(json.NewEncoder(os.Stdout).Encode(map[string]string{
				"harness":            harnessName,
				"permission_mode":    policy.Permissions.DefaultMode,
				"authority_ref":      policy.AuthorityRef,
				"authority_revision": revision,
			}))
			return
		}
		fmt.Println(policy.Permissions.DefaultMode)
		return
	}
	if target == "generate" || target == "drift" {
		if hasFlag(args[1:], "--apply") {
			if target != "generate" {
				fmt.Fprintln(os.Stderr, "compound harness drift does not accept --apply")
				os.Exit(2)
			}
			if code := runHarnessGenerateApply(args[1:], os.Stdout, os.Stderr, time.Now()); code != 0 {
				os.Exit(code)
			}
			return
		}
		harnessName := valueFlag(args[1:], "--harness")
		if harnessName == "" {
			fmt.Fprintf(os.Stderr, "usage: compound harness %s --harness <name> [--json] [--apply --work <work_id>]\n", target)
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(set COMPOUND_HOME)")
			os.Exit(1)
		}
		if target == "generate" {
			render, err := harness.RenderPermissions(homePath, harnessName)
			die(err)
			if hasFlag(args[1:], "--json") {
				die(json.NewEncoder(os.Stdout).Encode(render))
				return
			}
			fmt.Println("# harness generate (dry-run)")
			fmt.Printf("- harness: %s\n", harnessName)
			fmt.Printf("- target: %s\n", render.TargetPath)
			fmt.Printf("- changed: %t\n", render.Changed)
			fmt.Printf("- source_authority: %s\n", render.Artifact.SourceAuthorityRef)
			fmt.Printf("- source_revision: %s\n", render.Artifact.SourceRevision)
			fmt.Printf("- intended_sha256: %s\n", render.Artifact.ContentSHA256)
			for _, detail := range render.Details {
				fmt.Printf("- delta: %s\n", detail)
			}
			if render.Changed {
				fmt.Printf("\n--- %s\n+++ intended:%s\n%s", render.TargetPath, render.Artifact.SourceAuthorityRef, render.Content)
			}
			return
		}
		report, err := harness.PermissionDrift(homePath, harnessName)
		die(err)
		// --emit-divergence bridges a drifted report into the generic observation
		// intake (packet work_0fc6126d8fa2): ONE tool_health observation batch,
		// applied only when drifted (silence-on-match), so the existing
		// source-intake edge can declare a divergence_declared card. Without the
		// flag this stays exactly the prior display-only behavior.
		var emission permissionDivergenceEmission
		if hasFlag(args[1:], "--emit-divergence") {
			emission, err = emitPermissionPostureDivergence(homePath, report, time.Now())
			die(err)
		}
		if hasFlag(args[1:], "--json") {
			if emission.Applied {
				die(json.NewEncoder(os.Stdout).Encode(struct {
					harness.PermissionDriftReport
					permissionDivergenceEmission
				}{report, emission}))
			} else {
				die(json.NewEncoder(os.Stdout).Encode(report))
			}
		} else {
			fmt.Println("# harness drift")
			fmt.Printf("- harness: %s\n", report.Harness)
			fmt.Printf("- target: %s\n", report.TargetPath)
			fmt.Printf("- status: %s\n", map[bool]string{true: "drift", false: "clean"}[report.Drifted])
			for _, detail := range report.Details {
				fmt.Printf("- detail: %s\n", detail)
			}
			if emission.Applied {
				fmt.Printf("- observation_applied: %s\n", emission.ObservationID)
				fmt.Printf("- observation_source: %s\n", emission.SourcePath)
			}
		}
		if report.Drifted {
			os.Exit(1)
		}
		return
	}
	if target == "instruction-pack" {
		harnessName := valueFlag(args[1:], "--harness")
		if harnessName == "" {
			fmt.Fprintln(os.Stderr, "usage: compound harness instruction-pack --harness <name> [--preset <name>] [--context-pack <id>] [--json] [--drift [--emit-divergence]]")
			os.Exit(2)
		}
		// --preset is the optional second identity axis: two switchback
		// presets over one harness render to distinct preset-scoped packs.
		// Omitted → today's harness-only path and pack identity, unchanged.
		presetName := valueFlag(args[1:], "--preset")
		registered, ok := harness.DefaultRegistry().Get(harnessName)
		if !ok {
			die(fmt.Errorf("unknown instruction-pack harness %q", harnessName))
		}
		harnessName = registered.Name
		if hasFlag(args[1:], "--drift") && valueFlag(args[1:], "--context-pack") != "" {
			fmt.Fprintln(os.Stderr, "--drift verifies the installed binding; --context-pack is render-only")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		die(err)
		// --drift compares on-disk latest.json against a fresh render
		// (READ-ONLY: never writes latest.json, never mutates inputs).
		// --emit-divergence bridges a drifted report into the generic
		// observation intake as ONE tool_health observation — silence on
		// match, exactly as `compound harness drift --emit-divergence`
		// does for the permission-posture seam. The packet calls this
		// out as the S5b extension that closes the cold-start loop from
		// the S5 deviation (work_e9a494d5cf58).
		if hasFlag(args[1:], "--drift") {
			report, err := harness.InstructionPackDrift(homePath, harnessName, presetName, time.Now().UTC())
			die(err)
			var emission instructionPackDivergenceEmission
			if hasFlag(args[1:], "--emit-divergence") {
				emission, err = emitInstructionPackDivergence(homePath, report, time.Now())
				die(err)
			}
			if hasFlag(args[1:], "--json") {
				if emission.Applied {
					die(json.NewEncoder(os.Stdout).Encode(struct {
						harness.InstructionPackDriftReport
						instructionPackDivergenceEmission
					}{report, emission}))
				} else {
					die(json.NewEncoder(os.Stdout).Encode(report))
				}
			} else {
				fmt.Println("# harness instruction-pack drift")
				fmt.Printf("- harness: %s\n", report.Harness)
				fmt.Printf("- path: %s\n", report.Path)
				fmt.Printf("- status: %s\n", map[bool]string{true: "drift", false: "clean"}[report.Drifted])
				if report.Preset != "" {
					fmt.Printf("- preset: %s\n", report.Preset)
				}
				for _, detail := range report.Details {
					fmt.Printf("- detail: %s\n", detail)
				}
				if emission.Applied {
					fmt.Printf("- observation_id: %s\n", emission.ObservationID)
					fmt.Printf("- observation_path: %s\n", emission.SourcePath)
				}
			}
			if report.Drifted {
				os.Exit(1)
			}
			return
		}
		var pack harness.InstructionPack
		if contextID := valueFlag(args[1:], "--context-pack"); contextID != "" {
			pack, err = contract.RenderInstructionPackForContext(homePath, harnessName, presetName, contextID, time.Now().UTC())
		} else {
			pack, err = harness.RenderInstructionPack(homePath, harnessName, presetName, time.Now().UTC())
		}
		die(err)
		if hasFlag(args[1:], "--json") {
			die(json.NewEncoder(os.Stdout).Encode(pack))
			return
		}
		fmt.Println("# harness instruction-pack")
		fmt.Printf("- harness: %s\n", pack.Harness)
		if pack.Preset != "" {
			fmt.Printf("- preset: %s\n", pack.Preset)
		}
		fmt.Printf("- schema_version: %s\n", pack.SchemaVersion)
		fmt.Printf("- policy_core: ref=%s absent=%v\n", pack.PolicyCore.AuthorityRef, pack.PolicyCore.Absent)
		fmt.Printf("- posture: ref=%s absent=%v\n", pack.Posture.AuthorityRef, pack.Posture.Absent)
		fmt.Printf("- installed_hooks: ref=%s absent=%v\n", pack.Roster.InstalledHooks.Ref, pack.Roster.InstalledHooks.Absent)
		fmt.Printf("- effective_contract: ref=%s absent=%v\n", pack.Roster.EffectiveContract.Ref, pack.Roster.EffectiveContract.Absent)
		fmt.Printf("- registry_revision: %s\n", pack.Roster.RegistryRevision)
		fmt.Printf("- generated_at: %s\n", pack.GeneratedAt.Format(time.RFC3339))
		fmt.Printf("- content_sha256: %s\n", pack.ContentSHA256)
		fmt.Printf("- written_to: %s\n", harness.InstructionPackPath(homePath, harnessName, pack.Preset))
		return
	}
	if target == "refresh-lane" {
		die(lanes.RunCLIFromArgs(args[1:], time.Now().UTC()))
		return
	}
	if target == "observe" {
		if len(args) > 3 {
			fmt.Fprintln(os.Stderr, "usage: compound harness observe [harness-registry.yaml|--os-entrypoints file]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
			os.Exit(1)
		}
		now := time.Now()
		obs := sourceobs.FromHarnessRegistry(reg, now)
		switch {
		case len(args) == 2:
			manifest, err := harness.LoadRegistryManifest(args[1])
			die(err)
			obs, err = sourceobs.FromHarnessRegistryManifest(manifest, now)
			die(err)
		case len(args) == 3 && args[1] == "--os-entrypoints":
			manifest, err := harness.LoadOSEntrypointsManifest(args[2])
			die(err)
			obs, err = sourceobs.FromHarnessRegistryManifest(manifest, now)
			die(err)
		case len(args) == 3:
			fmt.Fprintln(os.Stderr, "usage: compound harness observe [harness-registry.yaml|--os-entrypoints file]")
			os.Exit(2)
		}
		report, err := sourceobs.Apply(homePath, obs, now)
		die(err)
		fmt.Println("# harness observe")
		fmt.Printf("- source_observation: %s\n", report.ObservationID)
		fmt.Printf("- status: %s\n", report.Status)
		for _, id := range report.CandidateIDs {
			fmt.Printf("- source_candidate: %s\n", id)
		}
		for _, id := range report.NoPromoteIDs {
			fmt.Printf("- source_no_promote: %s\n", id)
		}
		fmt.Println("- verdict: capability_evidence_only_not_authority")
		return
	}
	print1 := func(h harness.Harness) {
		c := h.Capabilities
		fmt.Printf("%s  (%s)%s\n", h.Name, h.InstructionMechanism, fallbackTag(h.Fallback))
		fmt.Printf("  hooks: session_start=%v user_prompt=%v pre_tool_block=%v post_tool=%v session_end=%v | skills=%v mcp=%v\n",
			c.SessionStartContext, c.UserPromptContext, c.PreToolUseBlock, c.PostToolUseEvent, c.SessionEndReceipt, c.NativeSkills, c.MCP)
		for _, l := range h.Limitations {
			fmt.Printf("  limitation: %s\n", l)
		}
	}
	if target == "list" {
		for _, n := range reg.Names() {
			h, _ := reg.Get(n)
			print1(h)
		}
		return
	}
	if harness.RunBrowserOwnerSubcommand(target, args[1:]) {
		return
	}
	h, ok := reg.Get(target)
	if !ok {
		fmt.Printf("(unknown harness %q — falling back to generic)\n", target)
	}
	print1(h)
}

func fallbackTag(b bool) string {
	if b {
		return "  [fallback]"
	}
	return ""
}

// cmdHook runs fixture/live lifecycle hooks and dispatches managed hook installation.
// Each path is capability-aware per harness.
func cmdHook(args []string) {
	if len(args) > 0 && args[0] == "install" {
		cmdHookInstall(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "session-end-adapter" {
		adapterArgs := args[1:]
		if hasFlag(adapterArgs, "--live") {
			homePath, compatibilityErr := home.Resolve("")
			if compatibilityErr == nil {
				compatibilityErr = home.RequireMutationCompatible(homePath)
			}
			if compatibilityErr != nil {
				hook.WriteSessionEndAdapterHomeUnavailable(os.Stdout, os.Stderr, compatibilityErr)
				return
			}
		}
		if code := hook.RunSessionEndAdapterCommand(adapterArgs, os.Stdin, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return
	}
	if code := runHookCommand(args); code != 0 {
		os.Exit(code)
	}
}

// cmdHookInstall reconciles each compiled default posture without clobbering unrelated settings.
// Interactive Codex and Claude Code observe actions without publishing native permission
// decisions; isolated Claude-family profiles retain explicit mutation enforcement.
// `--lane-file` derives install harness + hook roster from a lane manifest; `--roster`
// selects the roster explicitly or overrides the manifest's declaration.
func cmdHookInstall(args []string) {
	if hasUsageFlag(args) {
		fmt.Println("usage: compound hook install [--lane-file lane.yaml | --harness claude-code|claude-zai|claude-minimax|claude-qwen|codex|oh-my-pi|all] [--roster minimal|full] [--scope global|project] [--config-dir DIR] [--base DIR] [--binary PATH] [--check] [--json]")
		return
	}
	check := hasFlag(args, "--check")
	jsonOut := hasFlag(args, "--json")

	scope := strings.TrimSpace(valueFlag(args, "--scope"))
	if scope == "" {
		scope = "global"
	}
	if scope != "global" && scope != "project" {
		fmt.Fprintln(os.Stderr, "error: --scope must be global or project")
		os.Exit(2)
	}

	req, err := hookplan.ResolveInstallRequest(strings.TrimSpace(valueFlag(args, "--lane-file")), strings.TrimSpace(valueFlag(args, "--harness")), strings.TrimSpace(valueFlag(args, "--roster")), strings.TrimSpace(valueFlag(args, "--config-dir")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	base := strings.TrimSpace(valueFlag(args, "--base"))
	if base == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		base = h
	}
	binary := strings.TrimSpace(valueFlag(args, "--binary"))
	if binary == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		binary = exe
	}

	homePath := ""
	var publish func(hookplan.HookInstallReport) error
	if !check {
		resolvedHome, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		homePath = resolvedHome
		publish = func(report hookplan.HookInstallReport) error {
			_, err := contract.ImportHookInstallReport(homePath, base, report, time.Now())
			return err
		}
	}
	var rep hookplan.HookInstallReport
	install := func() error {
		var err error
		if len(req.Harnesses) == 1 && req.Harnesses[0] == "oh-my-pi" {
			rep, err = hookplan.InstallOhMyPiShimWithConfigDir(base, req.ConfigDir, check)
			return err
		}
		rep, err = hookplan.InstallHooksWithRoster(
			base,
			binary,
			scope,
			req.Harnesses,
			req.ConfigDir,
			req.AllowedEvents,
			check,
			publish,
		)
		return err
	}
	err = func() error {
		if check {
			return install()
		}
		return withInstalledHookAuthorityLock(homePath, install)
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(rep)
		return
	}
	head := "installed"
	if check {
		head = "check"
	}
	fmt.Printf("hook %s (scope=%s):\n", head, rep.Scope)
	for _, e := range rep.Entries {
		fmt.Printf("- %s: %s -> %s [%s]\n", e.Harness, e.Action, e.ConfigPath, e.Matcher)
	}
	if !check {
		fmt.Println("\ninteractive Codex/Claude PreToolUse is observation-only; isolated executor profiles use explicit enforcement.")
	}
}

// withInstalledHookAuthorityLock serializes Compound CLI writers that can replace the managed
// native-hook roster or its installed-hooks authority. Native config preimage checks still defend
// against editors that do not cooperate with this advisory lock.
func withInstalledHookAuthorityLock(homePath string, mutate func() error) error {
	if mutate == nil {
		return fmt.Errorf("installed-hook authority mutation required")
	}
	return home.WithAutonomyAdmissionLock(homePath, mutate)
}

func runHookCommand(args []string) int {
	parsed, err := parseHookArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	now := time.Now()
	// Arm the wall-clock budget BEFORE any work: the harness kills a slow
	// hook with no output at all, which reads as a broken hook. The watchdog
	// publishes a truthful fail-open answer before that can happen.
	hookGate := newHookAnswerGate()
	stopBudget := startHookBudgetWatchdog(parsed, hookGate, nil)
	defer stopBudget()
	homePath, compatibilityErr := home.Resolve("")
	if compatibilityErr == nil {
		compatibilityErr = home.RequireMutationCompatible(homePath)
	}
	if compatibilityErr != nil {
		return runHookCommandWithoutHome(parsed, now, compatibilityErr, hookGate)
	}
	orientation, err := loadOrientationPolicy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}
	policyCfg, err := loadRuntimePolicyConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}
	deps := hook.Deps{Registry: harness.DefaultRegistry(), Orient: orientation, Policy: policyCfg, HomePath: homePath}
	if store, err := work.Open(homePath); err == nil {
		deps.Store = store // session-end needs it; others tolerate nil
	}
	// Resolve the memory-engine profile so a session-start / user-prompt-submit hook injects
	// ambient recall from the SAME owned engine `memory query` uses (else it stays lexical).
	if restoreEngine, err := configureMemoryEngineFromHome(homePath, ""); err == nil {
		defer restoreEngine()
	}
	fx, err := loadHookFixtureForHome(parsed, now, homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}
	fx.Enforced = parsed.Enforce
	stopOutcome, err := prepareLiveStopTriage(parsed, homePath, &fx, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}

	res, err := hook.Run(fx, deps, now)
	if err != nil {
		if parsed.Live && parsed.HookName == "pre-tool-use" &&
			res.CoordinationDelivery != nil && res.CoordinationDelivery.ExecutionHeld {
			// The governed hold and HookEvent are already durable, but a later
			// delivery projection write failed. Enforced mode publishes only the
			// native deny; observation mode emits no permission decision. Keep the
			// semantic card undisclosed so retry can deliver it exactly once.
			failClosed := res
			failClosed.ContextOutput = ""
			hookGate.mainAnswer(func() {
				printJSON(liveHookResponseWithEnforcement(
					deps.Registry,
					parsed.Harness,
					parsed.HookName,
					failClosed,
					parsed.Enforce,
					stopOutcome,
				))
			})
			fmt.Fprintln(os.Stderr, "error:", err)
			return preToolDeliveryFailureExit(parsed.Enforce, failClosed)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}
	recordLiveHookRetrieval(homePath, parsed, res, time.Now())
	if parsed.Live && strings.TrimSpace(parsed.ParityLogDir) != "" {
		if err := writeLiveHookParityLog(parsed.ParityLogDir, res, now); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return hookRuntimeErrorExit(parsed)
		}
	}
	if parsed.Live {
		hookGate.mainAnswer(func() {
			printJSON(liveHookResponseWithEnforcement(
				deps.Registry,
				parsed.Harness,
				parsed.HookName,
				res,
				parsed.Enforce,
				stopOutcome,
			))
		})
	} else {
		printJSON(res)
	}
	if parsed.HookName == "session-end" {
		autoConsumeCapabilityFeedback(homePath)
	}
	fmt.Fprintf(os.Stderr, "[%s/%s] %s", fx.Harness, res.Event.Status, res.Event.HookName)
	if res.Event.FailOpenReason != "" {
		fmt.Fprintf(os.Stderr, " (%s)", res.Event.FailOpenReason)
	}
	fmt.Fprintf(os.Stderr, " — %s\n", res.Summary)
	if parsed.Enforce {
		return hookEnforceExit(parsed.HookName, res)
	}
	return 0
}

// runHookCommandWithoutHome is the protocol-only recovery path for a missing
// or incompatible home. It uses built-in policy, never enters a home-aware
// fixture/resolver seam, and returns before every persistence/parity/feedback
// helper in the normal hook path.
func runHookCommandWithoutHome(parsed hookCommandArgs, now time.Time, compatibilityErr error, gate *hookAnswerGate) int {
	orientation, _, err := contract.LoadPolicy("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}
	policyCfg, _, err := policy.LoadConfig("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}
	if parsed.HookName == "pre-tool-use" {
		// Without compatible home authority, reads may remain observable but no
		// mutation may inherit the built-in local-reversible allowance. All other
		// mutation classes already hold (or deny) in the built-in policy.
		policyCfg.ClassOutcome["local_reversible"] = policy.ClassRule{Outcome: "hold"}
	}
	fx, err := loadHookFixture(parsed, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}
	fx.Enforced = parsed.Enforce
	res, err := hook.Run(fx, hook.Deps{
		Registry: harness.DefaultRegistry(),
		Orient:   orientation,
		Policy:   policyCfg,
	}, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return hookRuntimeErrorExit(parsed)
	}
	if parsed.Live {
		gate.mainAnswer(func() {
			printJSON(liveHookResponseWithEnforcement(harness.DefaultRegistry(), parsed.Harness, parsed.HookName, res, parsed.Enforce, nil))
		})
	} else {
		printJSON(res)
	}
	fmt.Fprintf(os.Stderr, "[%s/%s] %s — %s\n", fx.Harness, res.Event.Status, res.Event.HookName, res.Summary)
	fmt.Fprintln(os.Stderr, "home unavailable; hook response was not persisted:", compatibilityErr)
	if parsed.Enforce {
		return hookEnforceExit(parsed.HookName, res)
	}
	return 0
}

func recordLiveHookRetrieval(homePath string, parsed hookCommandArgs, res hook.Result, now time.Time) {
	if !parsed.Live || res.BreakerTripped || res.Context == nil ||
		(parsed.HookName != "session-start" && parsed.HookName != "user-prompt-submit") {
		return
	}
	in := res.Context.Input
	retrievalparity.Record(homePath, retrievalparity.NewOrientRow(
		in.Task,
		parityOrientInput(in),
		res.Context.Pack.ActiveMemory,
		res.Context.RetrievalLatency,
		true,
		now,
	))
}

func parseHookArgs(args []string) (hookCommandArgs, error) {
	var parsed hookCommandArgs
	if len(args) < 2 {
		return parsed, fmt.Errorf(hookUsage)
	}
	parsed.HookName = args[0]
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--enforce" {
			parsed.Enforce = true
			continue
		}
		if arg == "--live" {
			parsed.Live = true
			continue
		}
		if arg == "--harness" {
			if i+1 >= len(args) {
				return parsed, fmt.Errorf(hookUsage)
			}
			i++
			parsed.Harness = args[i]
			continue
		}
		if arg == "--parity-log" {
			if i+1 >= len(args) {
				return parsed, fmt.Errorf(hookUsage)
			}
			i++
			parsed.ParityLogDir = args[i]
			continue
		}
		if parsed.FixturePath != "" {
			return parsed, fmt.Errorf(hookUsage)
		}
		parsed.FixturePath = arg
	}
	if parsed.Live {
		if strings.TrimSpace(parsed.Harness) == "" || parsed.FixturePath != "" {
			return parsed, fmt.Errorf(hookUsage)
		}
		return parsed, nil
	}
	if parsed.FixturePath == "" {
		return parsed, fmt.Errorf(hookUsage)
	}
	if parsed.ParityLogDir != "" {
		return parsed, fmt.Errorf(hookUsage)
	}
	return parsed, nil
}

func hookEnforceExit(hookName string, res hook.Result) int {
	if hookName != "pre-tool-use" {
		return 0
	}
	for _, produced := range res.Produced {
		if pd, ok := produced.(*envelope.PolicyDecision); ok && !policy.Allowed(*pd) {
			return gateExit(*pd)
		}
	}
	if res.Event.Status == "blocked" {
		return 1
	}
	if res.Event.Status == "fail_open" {
		if res.Event.FailOpenReason == envelope.FailOpenContextUnavailable {
			return 4
		}
		return 1
	}
	return 0
}

func hookRuntimeErrorExit(parsed hookCommandArgs) int {
	if parsed.Live && parsed.HookName == "pre-tool-use" && !parsed.Enforce {
		return 0
	}
	return 1
}

// preToolDeliveryFailureExit keeps observation-only hooks from becoming a
// process-exit gate when a held coordination delivery cannot be projected.
func preToolDeliveryFailureExit(enforce bool, res hook.Result) int {
	if !enforce {
		return 0
	}
	if code := hookEnforceExit("pre-tool-use", res); code != 0 {
		return code
	}
	return 4
}

// gateExit maps a policy outcome to a scriptable exit code (0 allow, 3 hold, 4 deny).
func gateExit(pd envelope.PolicyDecision) int {
	switch pd.Outcome {
	case "allow", "allow_with_obligations":
		return 0
	case "hold":
		return 3
	case "deny":
		return 4
	default:
		return 1
	}
}

func policyConfig() policy.Config {
	cfg, err := loadRuntimePolicyConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return cfg
}

func loadRuntimePolicyConfig() (policy.Config, error) {
	homePath, _ := home.Resolve("") // empty if none; LoadConfig falls back to the built-in default
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		return policy.Config{}, err
	}
	return cfg, nil
}

func loadActionIntent(path string) envelope.ActionIntent {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var ai envelope.ActionIntent
	if err := json.Unmarshal(data, &ai); err != nil {
		fmt.Fprintln(os.Stderr, "error: parse action intent:", err)
		os.Exit(1)
	}
	return ai
}

// cmdWriteback governs candidate updates to owner surfaces.
func cmdWriteback(args []string) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println(writebackUsageText())
		return
	}
	if len(args) == 0 {
		writebackUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "gate":
		cmdWritebackGate(args[1:])
	case "no-promote":
		cmdWritebackNoPromote(args[1:])
	default:
		writebackUsage()
		os.Exit(2)
	}
}

func cmdWritebackGate(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: compound writeback gate <gate.json> --policy <policy-decision.json> [--record]")
		os.Exit(2)
	}
	gatePath := args[0]
	rest := args[1:]
	policyPath := valueFlag(rest, "--policy")
	record := hasFlag(rest, "--record")
	gate := loadWritebackGate(gatePath)
	var pd *envelope.PolicyDecision
	if policyPath != "" {
		loaded := loadPolicyDecision(policyPath)
		pd = &loaded
	}
	checked, err := writeback.EvaluateGate(gate, pd, time.Now())
	die(err)
	if record {
		homePath, err := home.Resolve("")
		die(err)
		store, err := writeback.Open(homePath)
		die(err)
		die(store.RecordGate(checked))
	}
	printJSON(checked)
	os.Exit(writebackGateExit(checked.Status))
}

func cmdWritebackNoPromote(args []string) {
	candidate := valueFlag(args, "--candidate")
	kind := valueFlag(args, "--kind")
	basis := valueFlag(args, "--basis")
	evidence := splitCSV(valueFlag(args, "--evidence"))
	ownerKind := valueFlag(args, "--owner-kind")
	ownerRef := valueFlag(args, "--owner-ref")
	expiry := valueFlag(args, "--expiry")
	policyVersion := valueFlag(args, "--policy-version")
	if candidate == "" || basis == "" || len(evidence) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound writeback no-promote --candidate C --basis B --evidence E1[,E2] [--kind K] [--owner-kind K --owner-ref R] [--expiry RFC3339]")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	die(err)
	store, err := writeback.Open(homePath)
	die(err)
	decision, created, err := store.RecordNoPromote(writeback.NoPromoteDecision{
		CandidateRef:  candidate,
		CandidateKind: kind,
		OwnerTarget:   writeback.OwnerTarget{Kind: ownerKind, Ref: ownerRef},
		Basis:         basis,
		Expiry:        expiry,
		EvidenceRefs:  evidence,
		PolicyVersion: policyVersion,
	}, time.Now())
	die(err)
	out := struct {
		writeback.NoPromoteDecision
		Created bool `json:"created"`
	}{NoPromoteDecision: decision, Created: created}
	printJSON(out)
}

func writebackUsageText() string {
	return `usage:
compound writeback gate <gate.json> --policy <policy-decision.json> [--record]
compound writeback no-promote --candidate C --basis B --evidence E1[,E2] [--kind K] [--owner-kind K --owner-ref R] [--expiry RFC3339]`
}

func writebackUsage() {
	fmt.Fprintln(os.Stderr, writebackUsageText())
}

func loadWritebackGate(path string) writeback.OwnerWritebackGate {
	data, err := os.ReadFile(path)
	die(err)
	var g writeback.OwnerWritebackGate
	if err := json.Unmarshal(data, &g); err != nil {
		die(fmt.Errorf("parse owner writeback gate: %w", err))
	}
	return g
}

func loadPolicyDecision(path string) envelope.PolicyDecision {
	data, err := os.ReadFile(path)
	die(err)
	var pd envelope.PolicyDecision
	if err := json.Unmarshal(data, &pd); err != nil {
		die(fmt.Errorf("parse policy decision: %w", err))
	}
	return pd
}

func writebackGateExit(status string) int {
	switch status {
	case writeback.GateAllowed:
		return 0
	case writeback.GateDenied:
		return 4
	case writeback.GateHeld, writeback.GateNoPolicy:
		return 3
	default:
		return 1
	}
}

func valueFlag(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func valuesFlag(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

// findingEvidenceRefs collects EVERY --evidence occurrence (the flag is
// repeatable) and also splits any comma-joined value, so `--evidence a
// --evidence b` and `--evidence a,b` are equivalent. Whitespace-only refs are
// dropped. The previous single valueFlag read silently kept only the first
// --evidence and discarded the rest (observed dropping 3 of 4 refs live).
func findingEvidenceRefs(args []string) []string {
	var refs []string
	for _, ev := range valuesFlag(args, "--evidence") {
		for _, ref := range strings.Split(ev, ",") {
			if ref = strings.TrimSpace(ref); ref != "" {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// flagValue was a byte-for-byte behavioral duplicate of valueFlag (same first-occurrence
// scan, only the loop bound differed) — H7 (2026-07-16/17 B2/B3 CLI seam packet) consolidates
// the two into one implementation. Every in-scope caller (this file, work_cli.go, and the
// finding/capture/placement/knowledge/memory CLI surfaces) now calls valueFlag directly; this
// name survives only as a compatibility forward for callers in files outside that packet's
// touch boundary (activity_cli.go, doctor_cli.go, act_cli.go, landlease_cli.go,
// executorlens_cli.go, latenttax_cli.go, capability.go, bench.go, and others — see the packet
// handoff for the full list). Follow-up: migrate those callers to valueFlag, then delete this.
func flagValue(args []string, name string) string {
	return valueFlag(args, name)
}

func intFlag(args []string, name string) (int, error) {
	raw := valueFlag(args, name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be non-negative", name)
	}
	return n, nil
}

// parseBodyBudget parses `--body-budget <n>` for `compound orient`. Absent or 0 →
// feature OFF (the materializer returns a nil section and the pack stays
// byte-identical to today). Negative or non-integer → usage error (exit 2, the
// repo's parse-error convention). The flag controls whether `compound orient`
// materializes a body_pack section into the ContextPack; it is CLI-transient and
// does not enter contract.Input (it is not part of the task's meaning).
func parseBodyBudget(args []string) (int, error) {
	raw := valueFlag(args, "--body-budget")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("--body-budget must be a non-negative integer")
	}
	if n < 0 {
		return 0, fmt.Errorf("--body-budget must be a non-negative integer")
	}
	return n, nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func flagCSVValues(args []string, names ...string) []string {
	nameSet := make(map[string]bool, len(names))
	for _, name := range names {
		nameSet[name] = true
	}
	var out []string
	for i := 0; i < len(args); i++ {
		if !nameSet[args[i]] || i+1 >= len(args) {
			continue
		}
		out = append(out, splitCSV(args[i+1])...)
		i++
	}
	return out
}

func validateKnownFlags(args []string, valueFlags, boolFlags map[string]bool) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		if boolFlags[arg] {
			continue
		}
		if valueFlags[arg] {
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", arg)
			}
			i++
			continue
		}
		return fmt.Errorf("unknown flag %s", arg)
	}
	return nil
}

// cmdFinding is the direct-write home for operational findings. It is the ordinary agent's
// positive counterpart to the placement guard: governed place to drop an evidence-class note,
// NEVER a ledger byte, NEVER auto-promoted to a Claim. Bare `compound finding` prints the
// discoverable help an unfamiliar agent needs to land one finding — including the three body
// forms and the evidence-only/no-promotion boundary.
func cmdFinding(args []string) {
	const help = "" +
		"usage: compound finding <record|list|show|doctor> ...\n" +
		"\n" +
		"Operational finding: a durable, evidence-class note written under\n" +
		"  COMPOUND_HOME/observed/findings. It is the positive counterpart to the placement\n" +
		"  guard and the direct-write door for a runbook fact an ordinary agent wants to remember.\n" +
		"\n" +
		"Verbs:\n" +
		"  record  record one finding (--topic T with one of --body B | --body-file F | --stdin;\n" +
		"          body input is bounded at 1 MiB; records default privacy=private and\n" +
		"          visibility=operational;\n" +
		"          --work-id W, --produced-by P, repeatable --evidence REF optional; --json returns\n" +
		"          body-free ack with status=recorded|duplicate)\n" +
		"  list    list finding ids and topics\n" +
		"  show    show one finding by id (JSON or human; the body IS printed on retrieval)\n" +
		"  doctor  read-only lifecycle hygiene: superseded + stale (older than --stale-days)\n" +
		"\n" +
		"Boundary:\n" +
		"  - First writer wins. An exact (topic, body) retry is a no-op duplicate, never an\n" +
		"    overwrite of the first record's provenance or created_at.\n" +
		"  - Evidence-only, never a promoted Claim: recording is not adjudication and never\n" +
		"    appears in ambient recall. Promoting a durable finding happens through the SEPARATE\n" +
		"    governed memory door.\n" +
		"  - Never a ledger byte: the ledger tree and digest stay byte-identical across record.\n" +
		"\n" +
		"Discoverability:\n" +
		"  compound placement target --kind operational-finding --json  (returns target,\n" +
		"                                                                source=finding-home,\n" +
		"                                                                consumer, and the\n" +
		"                                                                canonical record command)"
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println(help)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, help)
		os.Exit(2)
	}
	switch args[0] {
	case "record":
		cmdFindingRecord(args[1:])
	case "list":
		cmdFindingList(args[1:])
	case "show":
		cmdFindingShow(args[1:])
	case "doctor":
		cmdFindingDoctor(args[1:])
	default:
		fmt.Fprintln(os.Stderr, help)
		os.Exit(2)
	}
}

// cmdFindingDoctor is the read-only lifecycle surface for the direct-write finding home: it flags
// superseded records (an id revised by a later finding) and stale live records (older than
// --stale-days), so the "junk drawer" risk of a convenient direct-write home stays visible. It never
// promotes, archives, or deletes.
func cmdFindingDoctor(args []string) {
	jsonOut := hasFlag(args, "--json")
	staleDays := 0
	if v := strings.TrimSpace(valueFlag(args, "--stale-days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fmt.Fprintln(os.Stderr, "error: --stale-days must be a non-negative integer")
			os.Exit(2)
		}
		staleDays = n
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	rep, err := finding.Doctor(homePath, time.Now().UTC(), staleDays)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(rep)
		return
	}
	fmt.Printf("# finding doctor: %d total (%d live, %d superseded)\n", rep.Total, rep.Live, len(rep.Superseded))
	for _, d := range rep.Superseded {
		fmt.Printf("- superseded %s topic=%q by=%s\n", d.ID, d.Topic, d.SupersededBy)
	}
	if rep.StaleDays > 0 {
		fmt.Printf("# stale live findings (>= %d days)\n", rep.StaleDays)
		if len(rep.Stale) == 0 {
			fmt.Println("- none")
		}
		for _, d := range rep.Stale {
			fmt.Printf("- stale %s topic=%q age=%dd\n", d.ID, d.Topic, d.AgeDays)
		}
	}
}

func cmdFindingRecord(args []string) {
	const usage = "usage: compound finding record --topic T (--body B | --body-file F | --stdin) [--work-id W] [--produced-by P] [--supersedes ID] [--evidence REF]... [--json]"
	valueFlags := map[string]bool{
		"--topic": true, "--body": true, "--body-file": true, "--work-id": true,
		"--produced-by": true, "--supersedes": true, "--evidence": true,
	}
	boolFlags := map[string]bool{
		"--json": true, "--stdin": true,
	}
	if err := validateKnownFlags(args, valueFlags, boolFlags); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := rejectFlagUsedAsValue(args, valueFlags); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := requireExactlyOneBodySource(args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	for i, a := range args {
		if a == "--body" || a == "--body-file" || a == "--stdin" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		// A bare argument is a positional argument only if it is NOT the value of a known
		// value-flag. The previous arg, if it is a value-flag in our schema, "consumes" this
		// token as its value.
		if i > 0 && valueFlags[args[i-1]] {
			continue
		}
		fmt.Fprintln(os.Stderr, "error: unexpected positional argument:", a)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	topic := valueFlag(args, "--topic")
	if strings.TrimSpace(topic) == "" {
		fmt.Fprintln(os.Stderr, "error: --topic required")
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	body, err := resolveBodySource(args)
	if err != nil {
		// The bound is a gate (this body belongs somewhere else); an unreadable path is a
		// fault (fix the path). Selected by the error's type so the two never share a remedy.
		var bound *findingBodyBoundError
		if errors.As(err, &bound) {
			findingBodyBoundRefusal(bound, topic).writeTo(os.Stderr)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	jsonOut := hasFlag(args, "--json")
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	f := finding.Finding{
		Topic:      topic,
		Body:       body,
		WorkID:     valueFlag(args, "--work-id"),
		ProducedBy: valueFlag(args, "--produced-by"),
		Supersedes: valueFlag(args, "--supersedes"),
	}
	f.EvidenceRefs = findingEvidenceRefs(args)
	res, err := finding.RecordWithStatus(homePath, f, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(findingRecordAck(res))
		return
	}
	if res.Status == finding.StatusDuplicate {
		fmt.Printf("duplicate finding %s (topic=%q cid=%s) — preserved first record at observed/findings/%s.json\n", res.Finding.ID, res.Finding.Topic, res.Finding.CID, res.Finding.ID)
		return
	}
	fmt.Printf("recorded finding %s (topic=%q cid=%s) -> observed/findings/%s.json\n", res.Finding.ID, res.Finding.Topic, res.Finding.CID, res.Finding.ID)
}

// findingRecordAck is the minimal, body-free typed acknowledgement returned by `compound finding
// record --json`. Known consumers read the top-level id; the remaining fields expose only the
// disposition, path, and safe classification defaults. Body-derived and provenance metadata stay
// behind the explicit show surface.
func findingRecordAck(res finding.RecordResult) map[string]any {
	privacy := strings.TrimSpace(res.Finding.Privacy)
	if privacy == "" {
		privacy = finding.PrivacyPrivate
	}
	visibility := strings.TrimSpace(res.Finding.Visibility)
	if visibility == "" {
		visibility = finding.VisibilityOperational
	}
	return map[string]any{
		"schema":     "compound.finding.record-ack.v1",
		"status":     res.Status,
		"id":         res.Finding.ID,
		"path":       filepath.Join("observed/findings", res.Finding.ID+".json"),
		"privacy":    privacy,
		"visibility": visibility,
	}
}

// rejectFlagUsedAsValue rejects the case where a value-flag's next token is itself a flag
// (e.g. `--topic --stdin`). Treating a flag as another flag's value silently accepts nonsense
// inputs, so this is a hard usage error.
func rejectFlagUsedAsValue(args []string, valueFlags map[string]bool) error {
	for i, a := range args {
		if !valueFlags[a] {
			continue
		}
		if i+1 >= len(args) {
			// Trailing flag without value — let validateKnownFlags handle it (its error
			// message names the specific flag).
			continue
		}
		next := args[i+1]
		if strings.HasPrefix(next, "-") {
			return fmt.Errorf("flag %s requires a value, got another flag %q", a, next)
		}
	}
	return nil
}

// requireExactlyOneBodySource enforces that exactly one of --body, --body-file, --stdin is
// supplied. Missing any body source OR supplying more than one is a usage error.
func requireExactlyOneBodySource(args []string) error {
	seen := 0
	for _, a := range args {
		if a == "--body" || a == "--body-file" || a == "--stdin" {
			seen++
		}
	}
	switch {
	case seen == 0:
		return fmt.Errorf("one of --body, --body-file, or --stdin is required")
	case seen > 1:
		return fmt.Errorf("only one of --body, --body-file, or --stdin may be supplied")
	}
	return nil
}

// resolveBodySource reads the body from the single body source the caller supplied.
func resolveBodySource(args []string) (string, error) {
	if hasFlag(args, "--body") {
		return readBoundedFindingBody(strings.NewReader(valueFlag(args, "--body")), "--body")
	}
	if hasFlag(args, "--body-file") {
		path := valueFlag(args, "--body-file")
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("--body-file requires a value")
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read --body-file: %w", err)
		}
		defer file.Close()
		return readBoundedFindingBody(file, "--body-file")
	}
	if hasFlag(args, "--stdin") {
		return readBoundedFindingBody(os.Stdin, "--stdin")
	}
	return "", fmt.Errorf("one of --body, --body-file, or --stdin is required")
}

// findingBodyBoundError is the typed refusal of the direct-write finding home's body bound.
// It is typed rather than a plain fmt.Errorf so the CLI can tell an oversize body (a gate:
// the body belongs somewhere else) apart from an unreadable one (a fault: fix the path) and
// render a different remedy for each, without matching error text.
type findingBodyBoundError struct {
	Source string // the flag that supplied the body: --body, --body-file, or --stdin
	Bound  int    // finding.MaxBodyBytes, carried so the message cannot drift from the check
	Got    int    // bytes observed before the read was cut off; >= Bound+1
}

func (e *findingBodyBoundError) Error() string {
	return fmt.Sprintf("%s exceeds the %s limit", e.Source, findingBodyBoundLabel(e.Bound))
}

// findingBodyBoundLabel renders the bound in the unit it is declared in. Derived from the
// constant rather than written out, so raising the bound cannot leave the refusal quoting
// the old number -- the exact drift that makes a refusal untrustworthy.
func findingBodyBoundLabel(bound int) string {
	if bound >= 1<<20 && bound%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", bound>>20)
	}
	return fmt.Sprintf("%d byte", bound)
}

func readBoundedFindingBody(reader io.Reader, source string) (string, error) {
	data, err := io.ReadAll(io.LimitReader(reader, finding.MaxBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", source, err)
	}
	if len(data) > finding.MaxBodyBytes {
		return "", &findingBodyBoundError{Source: source, Bound: finding.MaxBodyBytes, Got: len(data)}
	}
	return string(data), nil
}

// findingBodyBoundRefusal renders the body-bound gate.
//
// The bound is not an arbitrary size limit: the finding home is the DIRECT-WRITE home for
// operational notes, and a body past the bound is nearly always a transcript, a log, or a
// dump -- content whose home is an artifact the finding should CITE, not inline. So the
// refusal has to name the shape of the fix ("cite it, do not paste it"), because the obvious
// reading of a size error is "split it in half and record it twice", which produces two
// findings that each say less than one.
func findingBodyBoundRefusal(bound *findingBodyBoundError, topic string) gateRefusal {
	label := findingBodyBoundLabel(bound.Bound)
	return gateRefusal{
		Command: "compound finding record",
		Gate:    "finding_body_bound",
		Refused: fmt.Sprintf("the body supplied via %s is larger than the %s bound, so nothing was recorded", bound.Source, label),
		Tried: []string{
			fmt.Sprintf("read %s: at least %d bytes, over the %s bound", bound.Source, bound.Got, label),
			"record it anyway: not attempted — the bound is checked before the finding is minted, so no partial body was written",
		},
		DoThis: []string{
			fmt.Sprintf("compound finding record --topic %s --body-file <path>   # a trimmed body: the finding is the OBSERVATION, not the transcript", firstNonEmptyRefusalValue(topic, "<topic>")),
			fmt.Sprintf("compound finding record --topic %s --body \"<the observation>\" --evidence <ref>   # cite the bulk as evidence instead of inlining it", firstNonEmptyRefusalValue(topic, "<topic>")),
			"compound placement resolve <path-to-the-large-file>   # if the bulk is really a file, ask where it is allowed to live",
		},
		Prevents: "the direct-write home turning into a log sink. A finding is read by an operator deciding what to do next; a body that large is a transcript, and inlining it makes the finding unreadable while leaving the real artifact with no home of its own.",
	}
}

func cmdFindingList(args []string) {
	jsonOut := hasFlag(args, "--json")
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	items, err := finding.List(homePath, valueFlag(args, "--topic"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(items)
		return
	}
	if len(items) == 0 {
		fmt.Println("no findings")
		return
	}
	for _, f := range items {
		fmt.Printf("%s  %s  %s\n", f.ID, f.CreatedAt, f.Topic)
	}
}

func cmdFindingShow(args []string) {
	if err := validateKnownFlags(args, nil, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "usage: compound finding show <finding-id> [--json]")
		os.Exit(2)
	}
	jsonOut := hasFlag(args, "--json")
	id := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			id = a
			break
		}
	}
	if strings.TrimSpace(id) == "" {
		fmt.Fprintln(os.Stderr, "usage: compound finding show <finding-id> [--json]")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	f, err := finding.Show(homePath, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(f)
		return
	}
	fmt.Printf("# %s\n", f.Topic)
	fmt.Printf("id=%s cid=%s created=%s", f.ID, f.CID, f.CreatedAt)
	if f.WorkID != "" {
		fmt.Printf(" work=%s", f.WorkID)
	}
	if f.Supersedes != "" {
		fmt.Printf(" supersedes=%s", f.Supersedes)
	}
	fmt.Print("\n")
	if len(f.EvidenceRefs) > 0 {
		fmt.Printf("evidence: %s\n", strings.Join(f.EvidenceRefs, ", "))
	}
	fmt.Print("\n")
	fmt.Println(f.Body)
}

const captureUsage = "usage: compound capture --source <label> [--text <body>] [--work <work_id>] | compound capture show <cid>"

// cmdCapture is the top-level front door for claim_933958b8cad4
// (compound.conversation_capture_posture): a gesture-fired text body (stdin or --text) becomes a
// content-addressed blob under COMPOUND_HOME/observed/capture/ (never the ledger) plus exactly
// ONE observed-class 'proposed' memory candidate carrying a bounded pointer. See
// internal/mem/capture.go (Store.CaptureExhaust) for the write-door contract.
func cmdCapture(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, captureUsage)
		os.Exit(2)
	}
	if args[0] == "show" {
		cmdCaptureShow(args[1:])
		return
	}
	if hasUsageFlag(args) {
		fmt.Println(captureUsage)
		return
	}
	cmdCaptureRecord(args)
}

func cmdCaptureRecord(args []string) {
	source := strings.TrimSpace(valueFlag(args, "--source"))
	if source == "" {
		fmt.Fprintln(os.Stderr, captureUsage)
		os.Exit(2)
	}
	workID := valueFlag(args, "--work")

	var body string
	if hasFlag(args, "--text") {
		body = valueFlag(args, "--text")
	} else {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: read stdin:", err)
			os.Exit(1)
		}
		body = string(data)
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(os.Stderr, "error: capture body is empty")
		fmt.Fprintln(os.Stderr, captureUsage)
		os.Exit(2)
	}

	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	now := time.Now().UTC()
	// Check candidate pre-existence BEFORE the write so the success line can distinguish a fresh
	// mint from CaptureExhaust's skip-if-exists day-salted dedup, without CaptureExhaust itself
	// needing to widen its settled (cid, pointer, err) return shape.
	candID := mem.CaptureCandidateID(mem.ToolOutputCID([]byte(body)), now)
	var existing mem.Candidate
	preExisted := candidates.ReadCandidate(homePath, candidates.Memory, candID, &existing) == nil

	cid, _, err := store.CaptureExhaust(source, workID, body, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	state := "proposed"
	if preExisted {
		state = "skipped-existing"
	}
	fmt.Printf("captured %s source=%s bytes=%d candidate=%s (%s)\n", cid, source, len(body), candID, state)
}

func cmdCaptureShow(args []string) {
	if err := validateKnownFlags(args, nil, nil); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "usage: compound capture show <cid>")
		os.Exit(2)
	}
	cid := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			cid = a
			break
		}
	}
	if strings.TrimSpace(cid) == "" {
		fmt.Fprintln(os.Stderr, "usage: compound capture show <cid>")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	body, err := store.ReadCapture(cid)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(body); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdPlacement(args []string) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("usage: compound placement <target|resolve> ...")
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound placement <target|resolve> ...")
		os.Exit(2)
	}
	switch args[0] {
	case "target":
		cmdPlacementTarget(args[1:])
	case "resolve":
		cmdPlacementResolve(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "usage: compound placement <target|resolve> ...")
		os.Exit(2)
	}
}

func cmdPlacementTarget(args []string) {
	jsonOut := hasFlag(args, "--json")
	kind := valueFlag(args, "--kind")
	owner := valueFlag(args, "--owner")
	var (
		res policy.ResolvedTarget
		err error
	)
	switch {
	case strings.TrimSpace(kind) != "":
		res, err = policy.ResolveKind(kind)
	case strings.TrimSpace(owner) != "":
		homePath, herr := home.Resolve("")
		if herr != nil {
			fmt.Fprintln(os.Stderr, "error:", herr, "(run `compound home init`)")
			os.Exit(1)
		}
		res, err = policy.ResolveOwner(homePath, owner)
	default:
		fmt.Fprintln(os.Stderr, "usage: compound placement target (--kind K | --owner O) [--json]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(res)
		return
	}
	label := res.Kind
	if label == "" {
		label = res.Owner
	}
	fmt.Printf("%s -> %s (mode=%s source=%s)\n", label, res.Target, res.Mode, res.Source)
	if cmd := strings.TrimSpace(res.Command); cmd != "" {
		fmt.Printf("  command: %s\n", cmd)
	}
}

func cmdPlacementResolve(args []string) {
	if err := validateKnownFlags(args, nil, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "usage: compound placement resolve <path> [--json]")
		os.Exit(2)
	}
	jsonOut := hasFlag(args, "--json")
	path := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			path = a
			break
		}
	}
	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(os.Stderr, "usage: compound placement resolve <path> [--json]")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	res, err := policy.ResolvePath(homePath, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(res)
		return
	}
	if res.Allowed {
		fmt.Printf("allowed: %s\n", res.Path)
		return
	}
	fmt.Printf("blocked: %s (%s) -> %s\n", res.Path, res.Reason, res.Redirect)
}

func cmdReviews(args []string) {
	if len(args) > 0 && args[0] == "binding-plan" {
		cmdReviewsBinding(args[1:], false)
		return
	}
	if len(args) > 0 && args[0] == "binding-record" {
		cmdReviewsBinding(args[1:], true)
		return
	}
	if len(args) > 0 && args[0] == "resolve" {
		cmdReviewsResolve(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "record-finding" {
		cmdReviewsRecordFinding(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "brief-plan" {
		cmdReviewsBriefPlan(args[1:])
		return
	}
	jsonOut := hasFlag(args, "--json")
	area := valueFlag(args, "--area")
	includeArchived := hasFlag(args, "--include-archived")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
		case "--include-archived":
		case "--area":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: compound reviews [--json] [--area A] [--include-archived]")
				os.Exit(2)
			}
			i++
		default:
			fmt.Fprintln(os.Stderr, "usage: compound reviews [--json] [--area A] [--include-archived]")
			os.Exit(2)
		}
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	report, err := buildReviewQueueReport(homePath, area, includeArchived)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(report)
		return
	}
	fmt.Printf("reviews total=%d review_required=%d ready_to_apply=%d ok=%d\n", report.Counts.Total, report.Counts.ReviewRequired, report.Counts.ReadyToApply, report.Counts.OK)
	if report.Counts.DecisionCardCount > 0 || report.Counts.ActionableCount > 0 || report.Counts.ReviewRequiredMissingDecisionCard > 0 || report.Counts.ReviewRequiredMissingAction > 0 || report.Counts.FallbackSummaryCount > 0 || report.Counts.NeedsYouFallbackGroupCount > 0 {
		fmt.Printf("review_surface decision_cards=%d actionable=%d missing_cards=%d missing_actions=%d fallback_summaries=%d needs_you_fallback_groups=%d\n", report.Counts.DecisionCardCount, report.Counts.ActionableCount, report.Counts.ReviewRequiredMissingDecisionCard, report.Counts.ReviewRequiredMissingAction, report.Counts.FallbackSummaryCount, report.Counts.NeedsYouFallbackGroupCount)
	}
	if len(report.Counts.ByArea) > 0 {
		fmt.Println("areas:")
		for _, area := range sortedMapKeys(report.Counts.ByArea) {
			fmt.Printf("- %s: %d\n", area, report.Counts.ByArea[area])
		}
	}
	if len(report.Counts.ByStatus) > 0 {
		fmt.Println("statuses:")
		for _, status := range sortedMapKeys(report.Counts.ByStatus) {
			fmt.Printf("- %s: %d\n", status, report.Counts.ByStatus[status])
		}
	}
	if len(report.Groups) > 0 {
		fmt.Println("groups:")
		for _, group := range report.Groups {
			fmt.Printf("- %s count=%d area=%s", group.Key, group.Count, group.Area)
			if group.ContextLabel != "" {
				fmt.Printf(" context=%q", group.ContextLabel)
			}
			if group.SafeBatchAction != nil {
				fmt.Printf(" action=%s", group.SafeBatchAction.Command)
			} else if group.TerminalReason != "" {
				fmt.Printf(" reason=%q", group.TerminalReason)
			}
			fmt.Println()
		}
	}
	if len(report.Items) == 0 {
		fmt.Println("review_required: none")
		return
	}
	fmt.Println("items:")
	for _, item := range report.Items {
		fmt.Printf("- %s/%s status=%s repairs=%d", item.Area, item.ID, item.Status, item.RepairCount)
		if item.Readiness != "" {
			fmt.Printf(" readiness=%s", item.Readiness)
		}
		fmt.Println()
		fmt.Printf(" json: %s\n", item.JSONPath)
		if item.MarkdownPath != "" {
			fmt.Printf(" review: %s\n", item.MarkdownPath)
		}
		for _, action := range item.Actions {
			fmt.Printf(" action:%s %s", action.ID, action.Command)
			if action.RequiresReview {
				fmt.Print(" (requires review)")
			}
			fmt.Println()
		}
	}
}

// reviewsBriefPlanSchemaVersion versions the `compound reviews brief-plan`
// packet SHAPE itself — distinct from reviewartifact.DecisionBriefSchemaVersion,
// which is the schema of the brief a preparing session WRITES BACK. This is
// the read-only instruction packet pointing at where/how to write one.
const reviewsBriefPlanSchemaVersion = "compound.reviews_brief_plan.v0.1"

// reviewsBriefPlan is the bounded, harness-agnostic instruction packet
// `compound reviews brief-plan --item <id> --json` emits for a preparing
// agent session (claude code / codex / claude-zai / opencode / switchback —
// plain JSON in/out, nothing CLI-specific per this packet's boundaries).
// Building the plan is read-only: it never writes anything, including the
// brief itself.
type reviewsBriefPlan struct {
	SchemaVersion       string         `json:"schema_version"`
	ItemID              string         `json:"item_id"`
	Area                string         `json:"area"`
	ArtifactPath        string         `json:"artifact_path"`
	MarkdownPath        string         `json:"markdown_path,omitempty"`
	EvidenceRefs        []string       `json:"evidence_refs"`
	MemberIDs           []string       `json:"member_ids"`
	WritePath           string         `json:"write_path"`
	BriefSchema         map[string]any `json:"brief_schema"`
	ProposeOnlyWarnings []string       `json:"propose_only_warnings"`
}

// cmdReviewsBriefPlan is the `compound reviews brief-plan --item <id>
// [--json]` sub-verb (dispatched from cmdReviews, no new top-level CLI case —
// per this packet's file boundaries). It resolves the item across every area
// via the SAME queue projection `compound reviews` builds (so a preparing
// session sees exactly what the operator console does), finds the decision
// group the item belongs to (if any) so member_ids/evidence_refs cover the
// WHOLE group rather than just the queried item, and emits the bounded,
// read-only plan. It never writes the brief itself.
func cmdReviewsBriefPlan(args []string) {
	itemID := valueFlag(args, "--item")
	jsonOut := hasFlag(args, "--json")
	usage := "usage: compound reviews brief-plan --item ID [--json]"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
		case "--item":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			i++
		default:
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	}
	if itemID == "" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	report, err := buildReviewQueueReport(homePath, "", false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var item *reviewartifact.QueueItem
	for i := range report.Items {
		if report.Items[i].ID == itemID {
			item = &report.Items[i]
			break
		}
	}
	if item == nil {
		fmt.Fprintln(os.Stderr, "error: no review item with id", itemID)
		os.Exit(1)
	}
	evidenceRefs := append([]string{}, item.EvidenceRefs...)
	memberIDs := []string{item.ID}
	for _, group := range report.Groups {
		if group.Area != item.Area {
			continue
		}
		belongs := false
		for _, id := range group.ItemIDs {
			if id == item.ID {
				belongs = true
				break
			}
		}
		if !belongs {
			continue
		}
		memberIDs = reviewartifact.Unique(append([]string{}, group.ItemIDs...))
		evidenceRefs = reviewartifact.UniqueSorted(append(evidenceRefs, group.EvidenceRefs...))
		break
	}
	plan := reviewsBriefPlan{
		SchemaVersion:       reviewsBriefPlanSchemaVersion,
		ItemID:              item.ID,
		Area:                item.Area,
		ArtifactPath:        item.JSONPath,
		MarkdownPath:        item.MarkdownPath,
		EvidenceRefs:        evidenceRefs,
		MemberIDs:           memberIDs,
		WritePath:           reviewartifact.DecisionBriefWritePath(item.JSONPath, item.ID),
		BriefSchema:         reviewartifact.DecisionBriefJSONSchema(),
		ProposeOnlyWarnings: reviewartifact.DecisionBriefProposeOnlyWarnings,
	}
	if jsonOut {
		printJSON(plan)
		return
	}
	fmt.Printf("brief-plan item=%s area=%s\n", plan.ItemID, plan.Area)
	fmt.Printf(" artifact: %s\n", plan.ArtifactPath)
	fmt.Printf(" write: %s\n", plan.WritePath)
	if len(plan.MemberIDs) > 0 {
		fmt.Printf(" members: %s\n", strings.Join(plan.MemberIDs, ", "))
	}
	for _, ref := range plan.EvidenceRefs {
		fmt.Printf(" evidence: %s\n", ref)
	}
	for _, w := range plan.ProposeOnlyWarnings {
		fmt.Printf(" warning: %s\n", w)
	}
}

type reviewFindingRecord struct {
	SchemaVersion    string                        `json:"schema_version"`
	ID               string                        `json:"id"`
	CreatedAt        string                        `json:"created_at"`
	Status           string                        `json:"status"`
	WorkID           string                        `json:"work_id"`
	Capability       string                        `json:"capability"`
	Summary          string                        `json:"summary"`
	TargetPath       string                        `json:"target_path"`
	ArtifactPath     string                        `json:"artifact_path"`
	MarkdownPath     string                        `json:"markdown_path,omitempty"`
	ActionIntentID   string                        `json:"action_intent_id"`
	PolicyDecisionID string                        `json:"policy_decision_id"`
	PolicyOutcome    string                        `json:"policy_outcome"`
	StandingGrantID  string                        `json:"standing_grant_id,omitempty"`
	SourceArtifacts  []reviewFindingSourceArtifact `json:"source_artifacts,omitempty"`
	SourceTraces     []reviewFindingSourceTrace    `json:"source_traces,omitempty"`
	EvidenceRefs     []string                      `json:"evidence_refs,omitempty"`
	Warnings         []string                      `json:"warnings,omitempty"`
}

type reviewFindingSourceArtifact struct {
	Path         string `json:"path"`
	ArtifactPath string `json:"artifact_path"`
	Hash         string `json:"hash"`
}

type reviewFindingSourceTrace struct {
	ID            string   `json:"id"`
	SourceSystem  string   `json:"source_system,omitempty"`
	SourceKind    string   `json:"source_kind,omitempty"`
	MutationClass string   `json:"mutation_class,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
}

func cmdReviewsRecordFinding(args []string) {
	workID := valueFlag(args, "--work-id")
	targetPath := valueFlag(args, "--target")
	summary := valueFlag(args, "--summary")
	root := valueFlag(args, "--root")
	capability := reviewartifact.FirstNonEmpty(valueFlag(args, "--capability"), "review.finding")
	evidenceRefs := valuesFlag(args, "--evidence-ref")
	sourceArtifactPaths := valuesFlag(args, "--source-artifact")
	sourceTraceIDs := valuesFlag(args, "--source-trace")
	autoWork := hasFlag(args, "--auto-work")
	usage := "usage: compound reviews record-finding (--work-id W|--auto-work) --target state/runs/.../finding.json --summary S [--root DIR] [--capability review.finding] [--source-artifact state/runs/.../artifact.json] [--source-trace trace_id] [--evidence-ref REF...]"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--auto-work":
		case "--work-id", "--target", "--summary", "--root", "--capability", "--source-artifact", "--source-trace", "--evidence-ref":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			i++
		default:
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	}
	if (workID == "" && !autoWork) || targetPath == "" || summary == "" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if workID != "" && autoWork {
		fmt.Fprintln(os.Stderr, "error: --work-id and --auto-work are mutually exclusive")
		os.Exit(2)
	}
	if autoWork && len(sourceArtifactPaths) == 0 && len(sourceTraceIDs) == 0 {
		fmt.Fprintln(os.Stderr, "error: --auto-work requires --source-artifact or --source-trace")
		os.Exit(2)
	}
	if capability != "review.finding" && capability != "review.summary" {
		fmt.Fprintln(os.Stderr, "error: --capability must be review.finding or review.summary")
		os.Exit(2)
	}
	rec, err := recordReviewFinding(recordReviewFindingInput{
		WorkID:              workID,
		AutoWork:            autoWork,
		TargetPath:          targetPath,
		Summary:             summary,
		Root:                root,
		Capability:          capability,
		EvidenceRefs:        evidenceRefs,
		SourceArtifactPaths: sourceArtifactPaths,
		SourceTraceIDs:      sourceTraceIDs,
	}, time.Now())
	if err != nil {
		var denied *reviewFindingDeniedError
		if errors.As(err, &denied) {
			printJSON(denied.Decision)
			os.Exit(gateExit(denied.Decision))
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	printJSON(rec)
}

// recordReviewFindingInput is the parsed, validated request for the governed
// review-finding door. It is shared by the `reviews record-finding` CLI wrapper
// and by any in-process caller (e.g. the experiential-smoke circuit) that must
// emit a review card through the SAME governed path rather than re-implement it.
type recordReviewFindingInput struct {
	WorkID              string
	AutoWork            bool
	TargetPath          string
	Summary             string
	Root                string
	Capability          string
	EvidenceRefs        []string
	SourceArtifactPaths []string
	SourceTraceIDs      []string
}

// reviewFindingDeniedError carries the PolicyDecision when the governed door
// refuses the write, so a CLI caller can reproduce the exact `printJSON(pd)` +
// gate-exit behavior while a library caller can treat denial as a plain error.
type reviewFindingDeniedError struct {
	Decision envelope.PolicyDecision
}

func (e *reviewFindingDeniedError) Error() string {
	return "review finding refused by policy: outcome " + e.Decision.Outcome
}

// reviewFindingWorkStore is the minimal work-store surface used by
// recordReviewFinding. The concrete *work.Store satisfies it; the variable
// below exists only so tests can inject a fault wrapper to prove rollback
// behavior when lifecycle transitions fail after the artifact is written.
type reviewFindingWorkStore interface {
	Create(in work.CreateInput, now time.Time) (envelope.WorkEnvelope, error)
	Get(workID string) (envelope.WorkEnvelope, error)
	RecordAction(workID string, ai envelope.ActionIntent, pd envelope.PolicyDecision, now time.Time) (envelope.WorkEnvelope, error)
	Claim(workID string, actor string, now time.Time) (envelope.WorkEnvelope, error)
	Advance(workID string, in work.AdvanceInput, now time.Time) (envelope.WorkEnvelope, error)
	Close(workID string, in work.CloseInput, now time.Time) (work.CloseResult, error)
}

// reviewFindingStoreOpener is a test seam; production delegates to work.Open.
var reviewFindingStoreOpener = func(homePath string) (reviewFindingWorkStore, error) { return work.Open(homePath) }

// recordReviewFinding is the governed review-finding door extracted as a
// returning function. It evaluates policy, records the ActionIntent and
// allowing PolicyDecision on the WorkEnvelope FIRST, and only then writes the
// finding artifact and its markdown sibling and advances/closes the work.
// On any failure after the artifact files are written, the files are rolled
// back so no orphaned artifact remains while the work is stuck mid-lifecycle.
// The CLI wrapper adds the os.Exit / printJSON shell.
func recordReviewFinding(in recordReviewFindingInput, now time.Time) (reviewFindingRecord, error) {
	capability := reviewartifact.FirstNonEmpty(in.Capability, "review.finding")
	target, err := normalizeRunReviewTarget(in.TargetPath)
	if err != nil {
		return reviewFindingRecord{}, err
	}
	root := in.Root
	if root == "" {
		root, err = os.Getwd()
		if err != nil {
			return reviewFindingRecord{}, err
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return reviewFindingRecord{}, err
	}
	artifactPath := filepath.Join(root, filepath.FromSlash(target))
	markdownPath := strings.TrimSuffix(artifactPath, filepath.Ext(artifactPath)) + ".md"
	sourceArtifacts, sourceEvidenceRefs, err := reviewFindingSourceArtifacts(root, in.SourceArtifactPaths)
	if err != nil {
		return reviewFindingRecord{}, err
	}
	if _, err := os.Stat(artifactPath); err == nil {
		return reviewFindingRecord{}, fmt.Errorf("review finding target already exists: %s", artifactPath)
	} else if !os.IsNotExist(err) {
		return reviewFindingRecord{}, err
	}
	if _, err := os.Stat(markdownPath); err == nil {
		return reviewFindingRecord{}, fmt.Errorf("review finding markdown target already exists: %s", markdownPath)
	} else if !os.IsNotExist(err) {
		return reviewFindingRecord{}, err
	}
	homePath, err := home.Resolve("")
	if err != nil {
		return reviewFindingRecord{}, fmt.Errorf("%w (run `compound home init`)", err)
	}
	sourceTraces, traceEvidenceRefs, err := reviewFindingSourceTraces(homePath, in.SourceTraceIDs)
	if err != nil {
		return reviewFindingRecord{}, err
	}
	store, err := reviewFindingStoreOpener(homePath)
	if err != nil {
		return reviewFindingRecord{}, err
	}
	createdAt := reviewartifact.Timestamp(now)
	workID := in.WorkID
	evidenceRefs := append([]string{}, in.EvidenceRefs...)
	evidenceRefs = append(evidenceRefs, sourceEvidenceRefs...)
	evidenceRefs = append(evidenceRefs, traceEvidenceRefs...)
	if in.AutoWork {
		created, err := store.Create(work.CreateInput{
			Owner:            "compound reviews",
			SuccessPredicate: "governed review finding recorded at " + target,
			DesiredOutcome:   strings.TrimSpace(in.Summary),
			EvidenceRefs:     reviewartifact.Unique(append(append([]string{}, evidenceRefs...), target)),
			Risk:             "tier1",
			Privacy:          "private",
		}, now)
		if err != nil {
			return reviewFindingRecord{}, err
		}
		workID = created.ID
	} else {
		w, err := store.Get(workID)
		if err != nil {
			return reviewFindingRecord{}, err
		}
		if w.State == "completed" || w.State == "failed" || w.State == "cancelled" {
			return reviewFindingRecord{}, fmt.Errorf("work %s is %s (terminal); cannot record review finding", workID, w.State)
		}
	}
	// evidenceRefs collects the upstream evidence that exists before this action.
	// The target artifact path is an OUTPUT of this action, not evidence for it,
	// and must not appear in the ActionIntent's EvidenceRefs before the file is
	// written. The final report (written after the artifact exists) can link it.
	actionEvidenceRefs := reviewartifact.Unique(append([]string{}, evidenceRefs...))
	actionEvidenceRefs = reviewartifact.Unique(append(actionEvidenceRefs, "work://"+workID))
	actionID := reviewartifact.ID("act_", workID, capability, target, in.Summary, createdAt)
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actionID,
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "cli", Name: "compound reviews"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  actionEvidenceRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "compound-cli",
		Capability:    capability,
		Action:        "record_review_finding",
		Target:        envelope.ActionTarget{Kind: "path", Path: target},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "repo",
		Preview:       in.Summary,
		RollbackPlan:  "remove the newly written review finding artifact and markdown sibling",
	}
	pd := policy.Evaluate(ai, policyConfig(), now)
	if !policy.Allowed(pd) {
		return reviewFindingRecord{}, &reviewFindingDeniedError{Decision: pd}
	}
	report := reviewFindingRecord{
		SchemaVersion:    "compound.review_finding_record.v0.1",
		ID:               reviewartifact.ID("rvf_", workID, capability, target, in.Summary),
		CreatedAt:        createdAt,
		Status:           "recorded",
		WorkID:           workID,
		Capability:       capability,
		Summary:          strings.TrimSpace(in.Summary),
		TargetPath:       target,
		ArtifactPath:     artifactPath,
		MarkdownPath:     markdownPath,
		ActionIntentID:   ai.ID,
		PolicyDecisionID: pd.ID,
		PolicyOutcome:    pd.Outcome,
		StandingGrantID:  pd.StandingGrantID,
		SourceArtifacts:  sourceArtifacts,
		SourceTraces:     sourceTraces,
		EvidenceRefs:     actionEvidenceRefs,
		Warnings: []string{
			"writes only under state/runs/**",
			"requires allowing PolicyDecision before artifact write",
			"records ActionIntent on WorkEnvelope before artifact write",
		},
	}
	// POLICY-ENFORCEMENT SEAM: record the ActionIntent + PolicyDecision before
	// any irreversible artifact write or lifecycle transition. If this fails, no
	// files are on disk and the work has not been advanced/closed.
	if _, err := store.RecordAction(workID, ai, pd, now); err != nil {
		return reviewFindingRecord{}, err
	}
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		return reviewFindingRecord{}, err
	}
	// Now that the governed policy decision is durable, include the target path
	// in the written report's evidence refs -- the file is about to exist.
	report.EvidenceRefs = reviewartifact.Unique(append(append([]string{}, actionEvidenceRefs...), target))
	data, err := json.MarshalIndent(report, "", " ")
	if err != nil {
		return reviewFindingRecord{}, err
	}
	if err := os.WriteFile(artifactPath, append(data, '\n'), 0o644); err != nil {
		return reviewFindingRecord{}, err
	}
	artifactFilesWritten := false
	rollbackArtifacts := func() {
		if artifactFilesWritten {
			_ = os.Remove(artifactPath)
			_ = os.Remove(markdownPath)
		}
	}
	if err := os.WriteFile(markdownPath, []byte(renderReviewFindingMarkdown(report)), 0o644); err != nil {
		_ = os.Remove(artifactPath)
		return reviewFindingRecord{}, err
	}
	artifactFilesWritten = true
	if in.AutoWork {
		if _, err := store.Claim(workID, "compound reviews", now); err != nil {
			rollbackArtifacts()
			return reviewFindingRecord{}, err
		}
		if _, err := store.Advance(workID, work.AdvanceInput{To: "running", Actor: "compound reviews", Note: "review finding recorded"}, now); err != nil {
			rollbackArtifacts()
			return reviewFindingRecord{}, err
		}
		if _, err := store.Advance(workID, work.AdvanceInput{To: "verifying", Actor: "compound reviews", VerificationID: target, Note: "review finding artifact written"}, now); err != nil {
			rollbackArtifacts()
			return reviewFindingRecord{}, err
		}
		if _, err := store.Close(workID, work.CloseInput{
			Outcome:   "completed",
			MoveType:  "control_plane_repair",
			Actor:     "compound reviews",
			Summary:   "Review finding recorded through StandingGrant",
			Changed:   true,
			ProofRefs: []string{target},
		}, now); err != nil {
			rollbackArtifacts()
			return reviewFindingRecord{}, err
		}
	}
	return report, nil
}

func reviewFindingSourceTraces(homePath string, values []string) ([]reviewFindingSourceTrace, []string, error) {
	envs, err := distiller.LoadTraceEnvelopes(homePath, values)
	if err != nil {
		return nil, nil, err
	}
	var traces []reviewFindingSourceTrace
	var evidenceRefs []string
	for _, env := range envs {
		trace, ok := env.(*envelope.TraceEnvelope)
		if !ok {
			return nil, nil, fmt.Errorf("source trace is %T, want TraceEnvelope", env)
		}
		traceRef := "trace_envelope:" + trace.ID
		refs := reviewartifact.Unique(append([]string{traceRef}, trace.EvidenceRefs...))
		traces = append(traces, reviewFindingSourceTrace{
			ID:            trace.ID,
			SourceSystem:  trace.Source.System,
			SourceKind:    trace.Source.SourceKind,
			MutationClass: trace.MutationClass,
			Summary:       trace.Summary,
			EvidenceRefs:  refs,
		})
		evidenceRefs = append(evidenceRefs, refs...)
	}
	return traces, reviewartifact.Unique(evidenceRefs), nil
}

func reviewFindingSourceArtifacts(root string, values []string) ([]reviewFindingSourceArtifact, []string, error) {
	var artifacts []reviewFindingSourceArtifact
	var evidenceRefs []string
	seen := map[string]bool{}
	for _, value := range values {
		source, err := normalizeRunReviewSourceArtifact(value)
		if err != nil {
			return nil, nil, err
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		artifactPath := filepath.Join(root, filepath.FromSlash(source))
		exists, hash, err := reviewartifact.HashFileTarget(artifactPath)
		if err != nil {
			return nil, nil, fmt.Errorf("source artifact %q: %w", source, err)
		}
		if !exists {
			return nil, nil, fmt.Errorf("source artifact does not exist: %s", source)
		}
		hashRef := "sha256:" + hash
		artifacts = append(artifacts, reviewFindingSourceArtifact{
			Path:         source,
			ArtifactPath: artifactPath,
			Hash:         hashRef,
		})
		evidenceRefs = append(evidenceRefs, source, hashRef)
	}
	return artifacts, reviewartifact.Unique(evidenceRefs), nil
}

func normalizeRunReviewSourceArtifact(source string) (string, error) {
	source = filepath.ToSlash(strings.TrimSpace(source))
	if source == "" {
		return "", fmt.Errorf("source artifact path required")
	}
	if filepath.IsAbs(source) || strings.HasPrefix(source, "~") {
		return "", fmt.Errorf("source artifact must be relative state/runs path, got %q", source)
	}
	clean := filepath.ToSlash(filepath.Clean(source))
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("source artifact must not escape root, got %q", source)
	}
	if !strings.HasPrefix(clean, "state/runs/") {
		return "", fmt.Errorf("source artifact must be under state/runs/**, got %q", clean)
	}
	return clean, nil
}

func normalizeRunReviewTarget(target string) (string, error) {
	target = filepath.ToSlash(strings.TrimSpace(target))
	if target == "" {
		return "", fmt.Errorf("target required")
	}
	if filepath.IsAbs(target) || strings.HasPrefix(target, "~") {
		return "", fmt.Errorf("review finding target must be relative state/runs path, got %q", target)
	}
	clean := filepath.ToSlash(filepath.Clean(target))
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("review finding target must not escape root, got %q", target)
	}
	if !strings.HasPrefix(clean, "state/runs/") {
		return "", fmt.Errorf("review finding target must be under state/runs/**, got %q", clean)
	}
	if !strings.HasSuffix(clean, ".json") {
		return "", fmt.Errorf("review finding target must be a .json artifact, got %q", clean)
	}
	return clean, nil
}

func renderReviewFindingMarkdown(report reviewFindingRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Review Finding: %s\n\n", report.ID)
	fmt.Fprintf(&b, "- status: %s\n", report.Status)
	fmt.Fprintf(&b, "- work_id: %s\n", report.WorkID)
	fmt.Fprintf(&b, "- capability: %s\n", report.Capability)
	fmt.Fprintf(&b, "- target: %s\n", report.TargetPath)
	fmt.Fprintf(&b, "- policy_decision: %s (%s)\n", report.PolicyDecisionID, report.PolicyOutcome)
	if report.StandingGrantID != "" {
		fmt.Fprintf(&b, "- standing_grant: %s\n", report.StandingGrantID)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Summary")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, report.Summary)
	if len(report.SourceArtifacts) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Source Artifacts")
		fmt.Fprintln(&b)
		for _, artifact := range report.SourceArtifacts {
			fmt.Fprintf(&b, "- %s (%s)\n", artifact.Path, artifact.Hash)
		}
	}
	if len(report.SourceTraces) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Source Traces")
		fmt.Fprintln(&b)
		for _, trace := range report.SourceTraces {
			fmt.Fprintf(&b, "- %s", trace.ID)
			if trace.SourceKind != "" {
				fmt.Fprintf(&b, " [%s]", trace.SourceKind)
			}
			if trace.Summary != "" {
				fmt.Fprintf(&b, ": %s", trace.Summary)
			}
			fmt.Fprintln(&b)
		}
	}
	if len(report.EvidenceRefs) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Evidence")
		fmt.Fprintln(&b)
		for _, ref := range report.EvidenceRefs {
			fmt.Fprintf(&b, "- %s\n", ref)
		}
	}
	return b.String()
}

// cmdWork is the lifecycle store: one work_id threads intent -> context -> action ->
// receipt -> transition. State lives in COMPOUND_HOME (docs/architecture/transition.md).
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func emptyDash(s string) string {
	if s == "" {
		return "(new)"
	}
	return s
}

func parseOrientArgsWithHome(args []string, homePath string) (contract.Input, error) {
	var harnessName, cwd, task, repoKind, mode, topic string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--harness":
			if i+1 < len(args) {
				i++
				harnessName = args[i]
			}
		case "--cwd":
			if i+1 < len(args) {
				i++
				cwd = args[i]
			}
		case "--task":
			if i+1 < len(args) {
				i++
				task = args[i]
			}
		case "--repo-kind":
			if i+1 < len(args) {
				i++
				repoKind = args[i]
			}
		case "--topic":
			if i+1 < len(args) {
				i++
				topic = args[i]
			}
		case "--mode":
			if i+1 < len(args) {
				i++
				mode = args[i]
			}
		}
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return contract.Input{}, fmt.Errorf("resolve orientation cwd: %w", err)
		}
	}
	resolved, err := contract.ResolveOrientationIdentity(contract.Input{
		Harness: harnessName,
		Cwd:     cwd,
		Task:    task,
		Mode:    mode,
		Topic:   topic,
	}, harness.DefaultRegistry())
	if err != nil {
		return contract.Input{}, err
	}
	kind, via, err := contract.ResolveRepoKindWithHome(homePath, repoKind, resolved.Cwd)
	if err != nil {
		return contract.Input{}, err
	}
	resolved.RepoKind = kind
	resolved.RepoKindVia = via
	return resolved, nil
}

func orientPolicy() contract.OrientationPolicy {
	pol, err := loadOrientationPolicy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return pol
}

func loadOrientationPolicy() (contract.OrientationPolicy, error) {
	homePath, _ := home.Resolve("") // empty if none; LoadPolicy falls back to the built-in default
	pol, _, err := contract.LoadPolicy(homePath)
	if err != nil {
		return contract.OrientationPolicy{}, err
	}
	return pol, nil
}

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func autoConsumeCapabilityFeedback(homePath string) {
	report, ok, err := capgraph.ConsumeReceiptsIfGraphExists(homePath, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "capability feedback:", err)
		os.Exit(1)
	}
	if !ok || report.AppliedEdges == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "[capability-feedback] applied_edges=%d validated=%d declared_use=%d missed=%d false=%d\n",
		report.AppliedEdges, report.AppliedValidatedEdges, report.AppliedDeclaredUseEdges, report.AppliedMissedEdges, report.AppliedFalseEdges)
}

func autoConsumeDistillerCapabilityFeedback(homePath string) {
	if !capgraph.Exists(homePath) {
		return
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capability feedback:", err)
		os.Exit(1)
	}
	applied, err := store.Consume(homePath, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "capability feedback:", err)
		os.Exit(1)
	}
	if applied == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "[capability-feedback] applied_distiller_observations=%d\n", applied)
}

func cliHeader(schemaVersion, id, workID string, now time.Time, evidenceRefs []string) envelope.Header {
	return envelope.Header{
		SchemaVersion: schemaVersion,
		ID:            id,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
		IDs:           envelope.IDs{WorkID: workID},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       "private",
		Risk:          "tier1",
		EvidenceRefs:  evidenceRefs,
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}

func cliID(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(h[:])[:16]
}

func cliTS(now time.Time) string {
	return strconv.FormatInt(now.UTC().UnixNano(), 10)
}

// cmdContract compiles the EffectiveContract — the instructions/hooks/policies in force
// for a harness+repo+task (docs/architecture/contracts/README.md).
func cmdContract(args []string) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("usage: compound contract <compile|import-hooks|hooks-authority> ...")
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound contract <compile|import-hooks|hooks-authority> ...")
		os.Exit(2)
	}
	switch args[0] {
	case "compile":
		renderInstructions := hasFlag(args[1:], "--instructions")
		checkInstructions := hasFlag(args[1:], "--check-instructions")
		materializeInstructions := hasFlag(args[1:], "--materialize-instructions")
		bootstrapReplace := hasFlag(args[1:], "--bootstrap-replace")
		if (checkInstructions || materializeInstructions) && !renderInstructions {
			fmt.Fprintln(os.Stderr, "--check-instructions and --materialize-instructions require --instructions")
			os.Exit(2)
		}
		if checkInstructions && materializeInstructions {
			fmt.Fprintln(os.Stderr, "--check-instructions and --materialize-instructions are mutually exclusive")
			os.Exit(2)
		}
		if bootstrapReplace && !materializeInstructions {
			fmt.Fprintln(os.Stderr, "--bootstrap-replace requires --materialize-instructions")
			os.Exit(2)
		}
		preset := strings.TrimSpace(valueFlag(args[1:], "--preset"))
		if preset != "" && (!renderInstructions || checkInstructions || materializeInstructions) {
			fmt.Fprintln(os.Stderr, "--preset requires --instructions and is not supported with instruction check/materialization")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		die(err)
		in, err := parseOrientArgsWithHome(args[1:], homePath)
		die(err)
		if in.Harness == "" {
			fmt.Fprintln(os.Stderr, "usage: compound contract compile --harness <h> --cwd <dir> --task <t> [--repo-kind <k>] [--mode <m>] [--instructions [--check-instructions|--materialize-instructions [--bootstrap-replace]]] [--preset <name>]")
			os.Exit(2)
		}
		runtime, err := contract.MaterializeRuntime(in, orientPolicy(), homePath, time.Now())
		die(err)
		runtime, err = contract.SealRuntimeMaterialization(runtime)
		die(err)
		_, err = contract.RecordRuntimeMaterialization(homePath, runtime)
		die(err)
		ec := runtime.EffectiveContract
		if renderInstructions {
			render, err := contract.RenderInstructionBlockVariant(ec, in.Cwd, preset)
			die(err)
			if materializeInstructions {
				report, err := contract.MaterializeInstructionBlock(render, contract.InstructionMaterializationOptions{BootstrapTakeover: bootstrapReplace})
				die(err)
				printJSON(report)
				return
			}
			printJSON(render)
			if checkInstructions && render.Drifted {
				os.Exit(1)
			}
			return
		}
		printJSON(ec)
	case "import-hooks":
		sourcePath := valueFlag(args[1:], "--codex-hooks-json")
		registrationPath := valueFlag(args[1:], "--hook-registration-ts")
		if sourcePath == "" && registrationPath == "" && len(args) == 2 {
			sourcePath = args[1]
		}
		if (sourcePath == "" && registrationPath == "") || (sourcePath != "" && registrationPath != "") {
			fmt.Fprintln(os.Stderr, "usage: compound contract import-hooks (--codex-hooks-json <codex-hooks-json.json>|--hook-registration-ts <hook-registration.ts>)")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		die(err)
		var report contract.InstalledHooksImportReport
		err = withInstalledHookAuthorityLock(homePath, func() error {
			if registrationPath != "" {
				report, err = contract.ImportHookRegistrationTS(homePath, registrationPath, time.Now())
			} else {
				report, err = contract.ImportCodexHooksJSON(homePath, sourcePath, time.Now())
			}
			return err
		})
		die(err)
		printJSON(report)
	case "hooks-authority":
		observeLive := len(args) == 2 && args[1] == "--observe-live"
		if len(args) != 1 && !observeLive {
			fmt.Fprintln(os.Stderr, "usage: compound contract hooks-authority [--observe-live]")
			os.Exit(2)
		}
		homePath, err := home.Resolve("")
		die(err)
		report := contract.HooksAuthorityReport{
			Privacy:          "private",
			AuthorityRef:     contract.InstalledHooksAuthorityRef,
			AuthorityPath:    contract.InstalledHooksPath(homePath),
			SourceRecordPath: contract.InstalledHooksSourceAuthorityPath(homePath),
		}
		manifest, err := contract.LoadInstalledHooksManifest(homePath)
		if err != nil {
			if !os.IsNotExist(err) {
				die(err)
			}
		} else {
			report.Installed = true
			report.HookCount = len(manifest.Hooks)
			report.Harnesses = hookManifestHarnesses(manifest)
			report.PreToolUseReadiness = contract.InstalledHookGateReadiness(manifest, nativePreToolUseBlockMap(harness.DefaultRegistry()))
			report.HookReadiness = contract.InstalledHookRuntimeReadinessRows(manifest, installedHookRuntimeSupport())
		}
		records, err := contract.LoadInstalledHooksSourceAuthority(homePath)
		die(err)
		sourceSummary := contract.SummarizeInstalledHooksSourceAuthority(records)
		report.SourceRoleCounts = sourceSummary.SourceRoleCounts
		report.RetiredSourceAuthorityCount = sourceSummary.RetiredSourceAuthorityCount
		report.RetiredSourceAuthorityRefs = sourceSummary.RetiredSourceAuthorityRefs
		report.SourceAuthority = records
		if observeLive {
			harnessHome, err := os.UserHomeDir()
			die(err)
			die(contract.PopulateLiveHookReconciliation(&report, homePath, harnessHome))
		}
		printJSON(report)
	default:
		fmt.Fprintln(os.Stderr, "usage: compound contract <compile|import-hooks|hooks-authority> ...")
		os.Exit(2)
	}
}

func hookManifestHarnesses(manifest contract.InstalledHooksManifest) []string {
	seen := map[string]bool{}
	for _, hook := range manifest.Hooks {
		h := strings.TrimSpace(hook.Harness)
		if h != "" {
			seen[h] = true
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func installedHookRuntimeSupport() []contract.HookRuntimeSupport {
	supports := []contract.HookRuntimeSupport{}
	for _, harnessName := range []string{"claude_code", "claude_zai", "claude_minimax", "claude_qwen"} {
		supports = append(supports,
			hookRuntimeSupport(harnessName, "SessionStart", "fixtures/hooks/session-start.json", true),
			hookRuntimeSupport(harnessName, "UserPromptSubmit", "fixtures/hooks/user-prompt-submit.json", true),
			hookRuntimeSupport(harnessName, "PreToolUse", "fixtures/hooks/pre-tool-use.claude.json", true),
			hookRuntimeSupport(harnessName, "PostToolUse", "fixtures/hooks/post-tool-use.json", true),
			hookRuntimeSupport(harnessName, "SessionEnd", "fixtures/hooks/session-end.json", true))
	}
	return append(supports,
		hookRuntimeSupport("codex", "SessionStart", "fixtures/hooks/session-start.json", true),
		hookRuntimeSupport("codex", "UserPromptSubmit", "fixtures/hooks/user-prompt-submit.json", true),
		hookRuntimeSupport("codex", "PreToolUse", "fixtures/hooks/pre-tool-use.codex.json", true),
		hookRuntimeSupport("codex", "PostToolUse", "fixtures/hooks/post-tool-use.codex.json", true),
		hookRuntimeSupport("codex", "SessionEnd", "fixtures/hooks/session-end.json", true),
		hookRuntimeSupport("codex", "Stop", "fixtures/hooks/stop.codex.json", true),
		hookRuntimeSupport("opencode", "SessionStart", "fixtures/hooks/session-start.opencode.json", true),
		hookRuntimeSupport("opencode", "UserPromptSubmit", "fixtures/hooks/user-prompt-submit.opencode.json", true),
		hookRuntimeSupport("opencode", "PostToolUse", "fixtures/hooks/post-tool-use.opencode.json", true),
	)
}

func hookRuntimeSupport(harnessName, hookName, fixtureRef string, liveAdapter bool) contract.HookRuntimeSupport {
	return contract.HookRuntimeSupport{
		Harness:        harnessName,
		Hook:           hookName,
		FixtureRef:     fixtureRef,
		FixturePresent: hookfixtures.HasReference(fixtureRef),
		LiveAdapter:    liveAdapter,
		LiveCommand:    liveHookCommand(harnessName, hookName, liveAdapter),
	}
}

func liveHookCommand(harnessName, hookName string, liveAdapter bool) string {
	if !liveAdapter {
		return ""
	}
	if strings.TrimSpace(hookName) == "SessionEnd" && strings.HasPrefix(harnessName, "claude") {
		return fmt.Sprintf("compound hook session-end-adapter --live --harness %s", harnessName)
	}
	return fmt.Sprintf("compound hook %s --live --harness %s", hookCommandName(hookName), harnessName)
}

func hookCommandName(hookName string) string {
	switch strings.TrimSpace(hookName) {
	case "SessionStart":
		return "session-start"
	case "UserPromptSubmit":
		return "user-prompt-submit"
	case "PreToolUse":
		return "pre-tool-use"
	case "PostToolUse":
		return "post-tool-use"
	case "SessionEnd":
		return "session-end"
	case "Stop":
		return "stop"
	default:
		return strings.ToLower(strings.TrimSpace(hookName))
	}
}

// nativePreToolUseBlockMap exposes harness-native gate facts to installed-hook authority reporting.
func nativePreToolUseBlockMap(reg harness.Registry) map[string]bool {
	out := map[string]bool{}
	for name, h := range reg.Harnesses {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out[name] = h.Capabilities.PreToolUseBlock
	}
	return out
}

// cmdOrient assembles ContextPack an agent orients from. The excluded list records what was not loaded and why.
func cmdOrient(args []string) {
	homePath, err := home.Resolve("")
	die(err)
	if hasFlag(args, "--operator") {
		// Operator orientation also consumes memory-backed projections. Preserve
		// the same configured engine posture as the task-scoped pack path below.
		restoreEngine, err := configureMemoryEngineFromHome(homePath, "")
		die(err)
		defer restoreEngine()
		cmdOrientOperator(homePath, args)
		return
	}
	in, err := parseOrientArgsWithHome(args, homePath)
	die(err)
	if in.Harness == "" {
		fmt.Fprintln(os.Stderr, "usage: compound orient --harness <h> [--cwd <dir>] [--task <t>] [--repo-kind <k>] [--mode <m>] [--topic <topic-slug>] [--body-budget <estimated-tokens>]")
		os.Exit(2)
	}
	bodyBudget, err := parseBodyBudget(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	now := time.Now()
	pack, err := materializeOrientContextPack(homePath, in, bodyBudget, now)
	die(err)
	printJSON(pack)
}

// cmdHome manages COMPOUND_HOME — the runtime body (docs/architecture/runtime.md).
// The first envelope-first brick: generated state lives here, never in the repo.
// cmdPresence and cmdPresenceAnnounce are thin shims over the work organ's
// presence surface (the cmd-budget ratchet keeps cmd/compound a dispatch layer).
func cmdPresence(args []string) {
	if code := work.RunPresence(args); code != 0 {
		os.Exit(code)
	}
}

func cmdPresenceAnnounce(args []string) {
	if code := work.RunPresenceAnnounce(args); code != 0 {
		os.Exit(code)
	}
}

// cmdStore is the thin shell over the internal/store organ (the cmd-budget
// ratchet keeps cmd/compound a dispatch layer; the surface lives in the organ).
func cmdStore(args []string) {
	os.Exit(storecli.Run(args, os.Stdout, os.Stderr))
}

func cmdHome(args []string) {
	parsed := home.ParseCLIArgs(args)
	sub, kind, restPos := parsed.Subcommand, parsed.HomeKind, parsed.RemainingPositionals
	if hasUsageFlag(args) {
		switch sub {
		case "archive-history":
			fmt.Println("usage: compound home archive-history --kind work|lease|review|comparison|create-plan|memory-candidate [--path rel] [--max-files N] [--dry-run]; --kind is required to archive; --max-files bounds the batch (durable cursor resumes the next pass); --dry-run previews every kind without moving anything")
		case "restore-history":
			fmt.Println("usage: compound home restore-history (--all|--id id[,id...]) [--dry-run]")
		case "private-custody":
			fmt.Print(home.PrivateCustodyUsage)
		case "backup":
			fmt.Println("usage: compound home backup --target <new-archive.tar.gz> [--format v1|streamed-v2|streamed-v3|streamed-v3-event-snapshot] [--home <path>]")
		default:
			fmt.Println("usage: compound home <init|doctor|where|backup|export|verify|restore|archive-history|restore-history|compact|private-custody> [--home <path>] [--local|--global]")
		}
		return
	}
	resolve := func() string {
		p, err := parsed.ResolveHome()
		die(err)
		return p
	}
	switch sub {
	case "init":
		hp := resolve()
		if err := home.Init(hp, kind, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Printf("compound home initialized: %s (%s)\n", hp, kind)
	case "where":
		fmt.Println(resolve())
	case "doctor", "":
		report := home.Doctor(resolve())
		if hasFlag(args, "--json") {
			jsonReport := buildHomeDoctorReport(report)
			printJSON(jsonReport)
			if jsonReport.Verdict != "ok" {
				os.Exit(1)
			}
			return
		}
		printHomeDoctor(report)
	case "backup":
		if hasFlag(args, "--format") && valueFlag(args, "--format") == "" {
			fmt.Fprintln(os.Stderr, "home backup: --format requires v1, streamed-v2, streamed-v3 or streamed-v3-event-snapshot")
			os.Exit(2)
		}
		report, err := home.BackupWithFormat(resolve(), valueFlag(args, "--target"), valueFlag(args, "--format"), time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "home backup:", err)
			os.Exit(1)
		}
		printHomeArchiveReport("backup", report)
	case "export":
		report, err := home.ExportPublicFixture(resolve(), valueFlag(args, "--target"), time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "home export:", err)
			os.Exit(1)
		}
		printHomeArchiveReport("export", report)
	case "verify":
		archivePath := valueFlag(args, "--archive")
		if archivePath == "" && len(restPos) > 0 {
			archivePath = restPos[0]
		}
		if archivePath == "" {
			printHomeDoctor(home.Doctor(resolve()))
			return
		}
		report, err := home.VerifyArchive(archivePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "home verify:", err)
			os.Exit(1)
		}
		if hasFlag(args, "--json") {
			printJSON(report)
		} else {
			printHomeVerifyReport(report)
		}
	case "restore":
		archivePath := valueFlag(args, "--archive")
		if archivePath == "" && len(restPos) > 0 {
			archivePath = restPos[0]
		}
		targetPath := valueFlag(args, "--target")
		if archivePath == "" || targetPath == "" {
			fmt.Fprintln(os.Stderr, "usage: compound home restore <archive.tar.gz> --target <new-absent-home-path>")
			os.Exit(2)
		}
		report, err := home.RestoreArchive(archivePath, targetPath, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "home restore:", err)
			os.Exit(1)
		}
		printHomeRestoreReport(report)
	case "archive-history":
		dryRun := hasFlag(args, "--dry-run")
		kinds := valuesFlag(args, "--kind")
		maxFiles, err := intFlag(args, "--max-files")
		if err != nil {
			fmt.Fprintln(os.Stderr, "home archive-history:", err)
			os.Exit(2)
		}
		// A mutation with no selection is a footgun, not a default. Invoked
		// bare -- as an agent probing for usage does -- this swept EVERY kind
		// and relocated 329 terminal entries nobody asked to move. Selection
		// is now explicit for any mutation: no --kind, no archive. --dry-run
		// keeps its full-preview semantics, because a preview that writes
		// nothing is exactly how you find out what a sweep would take.
		if len(kinds) == 0 && !dryRun {
			fmt.Fprintln(os.Stderr, "usage: compound home archive-history --kind work|lease|review|comparison|create-plan|memory-candidate [--path rel] [--max-files N] [--dry-run]")
			fmt.Fprintln(os.Stderr, "archive-history mutates: it needs an explicit --kind. To see what a full sweep would move, add --dry-run.")
			os.Exit(2)
		}
		report, err := home.ArchiveOperationalHistory(resolve(), home.OperationalArchiveOptions{
			DryRun:   dryRun,
			Kinds:    kinds,
			Paths:    valuesFlag(args, "--path"),
			MaxFiles: maxFiles,
		}, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "home archive-history:", err)
			os.Exit(1)
		}
		printHomeOperationalArchiveReport("archive-history", report)
	case "restore-history":
		report, err := home.RestoreOperationalArchive(resolve(), home.OperationalRestoreOptions{
			DryRun: hasFlag(args, "--dry-run"),
			All:    hasFlag(args, "--all"),
			IDs:    valuesFlag(args, "--id"),
		}, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "home restore-history:", err)
			os.Exit(1)
		}
		printHomeOperationalArchiveReport("restore-history", report)
	case "compact":
		report, err := home.Compact(resolve(), home.CompactOptions{
			DryRun:      hasFlag(args, "--dry-run"),
			Rebuildable: hasFlag(args, "--rebuildable"),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "home compact:", err)
			os.Exit(1)
		}
		printHomeCompactReport(report)
	case "private-custody":
		if code := home.RunPrivateCustodyCLI(args, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
	case "route-swap-repair":
		os.Exit(mem.RouteEvidenceSwapRepairCLI(resolve(), args, os.Stdout, os.Stderr))
	default:
		fmt.Fprintln(os.Stderr, "usage: compound home <init|doctor|where|backup|export|verify|restore|archive-history|restore-history|compact|private-custody|route-swap-repair> [--home <path>] [--local|--global]; archive-history --kind work|lease|review|comparison|create-plan|memory-candidate [--path rel] [--max-files N] [--dry-run]")
		os.Exit(2)
	}
}

type editableSurfaceEntryReport struct {
	Surface       string `json:"surface"`
	Description   string `json:"description,omitempty"`
	ProposalClass string `json:"proposal_class,omitempty"`
	Status        string `json:"status"`
}

type editableSurfacesReport struct {
	AuthorityRef     string                       `json:"authority_ref"`
	AuthorityPath    string                       `json:"authority_path"`
	Source           string                       `json:"source"`
	Installed        bool                         `json:"installed"`
	Valid            bool                         `json:"valid"`
	NothingEditable  bool                         `json:"nothing_editable"`
	HardDenyCore     []string                     `json:"hard_deny_core"`
	Entries          []editableSurfaceEntryReport `json:"entries,omitempty"`
	EntryCount       int                          `json:"entry_count"`
	ActiveCount      int                          `json:"active_count"`
	ValidationErrors []string                     `json:"validation_errors,omitempty"`
}

// buildEditableSurfacesReport mirrors the autonomy-halt report shape (authority_ref,
// authority_path, source, installed, ...) so both governance switches read alike.
func buildEditableSurfacesReport(reg policy.EditableSurfaces, source, authorityPath string) editableSurfacesReport {
	report := editableSurfacesReport{
		AuthorityRef:     policy.EditableSurfacesAuthorityRef,
		AuthorityPath:    authorityPath,
		Source:           source,
		Installed:        source != "unset",
		Valid:            reg.Valid(),
		NothingEditable:  reg.NothingEditable(),
		HardDenyCore:     policy.HardDenyCoreSurfaces(),
		ActiveCount:      reg.ActiveCount(),
		ValidationErrors: reg.ValidationErrors,
	}
	for _, e := range reg.Entries {
		report.Entries = append(report.Entries, editableSurfaceEntryReport{
			Surface:       e.Surface,
			Description:   e.Description,
			ProposalClass: e.ProposalClass,
			Status:        e.Status,
		})
	}
	report.EntryCount = len(report.Entries)
	return report
}

// homeDoctorGovernanceReport co-locates the two policy-owned governance switches read live
// from COMPOUND_HOME/config: the autonomy kill-switch and the editable-surfaces registry.
type homeDoctorGovernanceReport struct {
	AutonomyHalt     homeDoctorAutonomyHaltState     `json:"autonomy_halt"`
	EditableSurfaces homeDoctorEditableSurfacesState `json:"editable_surfaces"`
}

type homeDoctorAutonomyHaltState struct {
	Installed          bool `json:"installed"`
	GlobalHalt         bool `json:"global_halt"`
	DisabledClassCount int  `json:"disabled_class_count"`
}

type homeDoctorEditableSurfacesState struct {
	Installed        bool `json:"installed"`
	Valid            bool `json:"valid"`
	NothingEditable  bool `json:"nothing_editable"`
	EntryCount       int  `json:"entry_count"`
	ActiveCount      int  `json:"active_count"`
	ValidationErrors int  `json:"validation_errors"`
}

func buildHomeGovernanceReport(homePath string) homeDoctorGovernanceReport {
	var gov homeDoctorGovernanceReport
	if halt, source, err := policy.LoadAutonomyHalt(homePath); err == nil {
		gov.AutonomyHalt = homeDoctorAutonomyHaltState{
			Installed:          source != "unset",
			GlobalHalt:         halt.Global,
			DisabledClassCount: len(halt.DisabledClasses),
		}
	}
	if reg, source, err := policy.LoadEditableSurfaces(homePath); err == nil {
		gov.EditableSurfaces = homeDoctorEditableSurfacesState{
			Installed:        source != "unset",
			Valid:            reg.Valid(),
			NothingEditable:  reg.NothingEditable(),
			EntryCount:       len(reg.Entries),
			ActiveCount:      reg.ActiveCount(),
			ValidationErrors: len(reg.ValidationErrors),
		}
	}
	return gov
}

func printHomeGovernanceDoctor(homePath string) {
	gov := buildHomeGovernanceReport(homePath)
	fmt.Printf("autonomy_halt: installed=%t global=%t disabled_classes=%d\n",
		gov.AutonomyHalt.Installed, gov.AutonomyHalt.GlobalHalt, gov.AutonomyHalt.DisabledClassCount)
	fmt.Printf("editable_surfaces: installed=%t valid=%t nothing_editable=%t entries=%d active=%d validation_errors=%d\n",
		gov.EditableSurfaces.Installed, gov.EditableSurfaces.Valid, gov.EditableSurfaces.NothingEditable,
		gov.EditableSurfaces.EntryCount, gov.EditableSurfaces.ActiveCount, gov.EditableSurfaces.ValidationErrors)
}

func homeOKStr(b bool) string {
	if b {
		return "ok"
	}
	return "MISSING"
}

func homeCleanStr(b bool) string {
	if b {
		return "ok"
	}
	return "DIRTY"
}

type homeDoctorJSONReport struct {
	SchemaVersion              string                     `json:"schema_version"`
	GeneratedAt                string                     `json:"generated_at"`
	Home                       string                     `json:"home"`
	Kind                       string                     `json:"kind,omitempty"`
	ManifestOK                 bool                       `json:"manifest_ok"`
	ManifestError              string                     `json:"manifest_error,omitempty"`
	ManifestRecovery           string                     `json:"manifest_recovery,omitempty"`
	DirsOK                     int                        `json:"dirs_ok"`
	DirsTotal                  int                        `json:"dirs_total"`
	Habitat                    home.HabitatInventory      `json:"habitat"`
	InsideGitRepo              bool                       `json:"inside_git_repo"`
	GeneratedStateInRepoSource bool                       `json:"generated_state_in_repo_source"`
	OperationalHygiene         homeDoctorHygieneReport    `json:"operational_hygiene"`
	OperationalHygieneActions  []homeDoctorHygieneAction  `json:"operational_hygiene_actions,omitempty"`
	BackupWarnings             []home.CheckItem           `json:"backup_warnings,omitempty"`
	Governance                 homeDoctorGovernanceReport `json:"governance"`
	Setup                      home.SetupReport           `json:"setup"`
	SetupOK                    bool                       `json:"setup_ok"`
	// CapacityFeedWarnings is the non-fatal capacity.blackout:<family> watchdog (work_9928ccb3621b):
	// a family with capacity rows of ANY age (CodexBar's usage-history.jsonl or Compound's own
	// usage-supplement.jsonl) but nothing fresh in 4x its own freshness window. Additive only —
	// never folded into Verdict below.
	CapacityFeedWarnings []routeevidence.CapacityFeedWarning `json:"capacity_feed_warnings,omitempty"`
	PrivateCustody       home.PrivateCustodyReport           `json:"private_custody"`
	Verdict              string                              `json:"verdict"`
}

// homeDoctorCapacityFeedWarnings computes the capacity-feed-blackout watchdog for a home doctor
// report. It is deliberately NOT threaded through buildHomeDoctorReport's/printHomeDoctor's
// signatures: both functions already have out-of-fence callers
// (cmd/compound/home_doctor_generated_at_test.go, cmd/compound/home_habitat_cli_test.go) that
// invoke them with today's single home.Report argument, so adding a parameter would break those
// files mechanically for a change this packet does not own. Computing from r.Home keeps every
// existing call site source-compatible.
func homeDoctorCapacityFeedWarnings(homePath string) []routeevidence.CapacityFeedWarning {
	if strings.TrimSpace(homePath) == "" {
		return nil
	}
	return routeevidence.CapacityFeedWarnings(routeevidence.DefaultLaneCapacityConfig(homePath), time.Now())
}

type homeDoctorHygieneReport struct {
	OK                   bool     `json:"ok"`
	TerminalHotWorkCount int      `json:"terminal_hot_work_count"`
	StaleLeaseCount      int      `json:"stale_lease_count"`
	TerminalHotWork      []string `json:"terminal_hot_work,omitempty"`
	StaleLeases          []string `json:"stale_leases,omitempty"`
}

type homeDoctorHygieneAction struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	Title              string   `json:"title"`
	Command            []string `json:"command"`
	DryRunCommand      []string `json:"dry_run_command,omitempty"`
	ExpectedValidation []string `json:"expected_validation,omitempty"`
	StopConditions     []string `json:"stop_conditions,omitempty"`
	Boundary           string   `json:"boundary"`
	Proof              string   `json:"proof"`
}

func buildHomeDoctorReport(r home.Report) homeDoctorJSONReport {
	smell := r.InsideGitRepo && r.Manifest.HomeKind != "local"
	operational := homeDoctorHygieneReport{OK: r.HygieneOK}
	if hygiene, err := home.OperationalHygiene(r.Home); err == nil {
		operational.TerminalHotWork = append([]string{}, hygiene.TerminalHotWork...)
		operational.StaleLeases = append([]string{}, hygiene.StaleLeases...)
	}
	operational.TerminalHotWorkCount = len(operational.TerminalHotWork)
	operational.StaleLeaseCount = len(operational.StaleLeases)
	verdict := "ok"
	if !r.OK || smell || !r.PrivateCustody.OK {
		verdict = "fail"
	}
	return homeDoctorJSONReport{
		SchemaVersion:              "compound.home_doctor_report.v0.1",
		GeneratedAt:                time.Now().UTC().Format(time.RFC3339),
		Home:                       r.Home,
		Kind:                       r.Manifest.HomeKind,
		ManifestOK:                 r.ManifestOK,
		ManifestError:              r.ManifestError,
		ManifestRecovery:           r.ManifestRecovery,
		DirsOK:                     r.DirsOK,
		DirsTotal:                  len(home.HabitatDirs()),
		Habitat:                    r.Habitat,
		InsideGitRepo:              r.InsideGitRepo,
		GeneratedStateInRepoSource: smell,
		OperationalHygiene:         operational,
		OperationalHygieneActions:  buildHomeDoctorHygieneActions(operational),
		BackupWarnings:             r.BackupWarnings,
		Governance:                 buildHomeGovernanceReport(r.Home),
		Setup:                      r.Setup,
		SetupOK:                    r.SetupOK,
		CapacityFeedWarnings:       homeDoctorCapacityFeedWarnings(r.Home),
		PrivateCustody:             r.PrivateCustody,
		Verdict:                    verdict,
	}
}

func homeDoctorHabitatLines(r home.Report) []string {
	lines := []string{"habitat: " + homeCleanStr(r.HabitatOK)}
	for _, root := range r.Habitat.UnknownRoots {
		lines = append(lines, "habitat_unknown_root: "+root)
	}
	for _, entry := range r.Habitat.UnknownEntries {
		lines = append(lines, "habitat_unknown_entry: "+entry)
	}
	for _, root := range r.Habitat.BridgeRoots {
		lines = append(lines, "habitat_bridge_root: "+root)
	}
	return lines
}

func buildHomeDoctorHygieneActions(h homeDoctorHygieneReport) []homeDoctorHygieneAction {
	var actions []homeDoctorHygieneAction
	if h.TerminalHotWorkCount > 0 {
		actions = append(actions, homeDoctorHygieneAction{
			ID:            "archive-terminal-work",
			Kind:          "operational_hygiene_repair",
			Title:         "Archive terminal WorkEnvelope history",
			Command:       []string{"compound", "home", "archive-history", "--kind", "work"},
			DryRunCommand: []string{"compound", "home", "archive-history", "--kind", "work", "--dry-run"},
			ExpectedValidation: []string{
				"terminal_hot_work_count becomes 0",
				"receipts, transitions, and archive index remain resolvable",
				"held/open WorkEnvelope state remains visible in workbench",
			},
			StopConditions: []string{
				"dry-run includes non-terminal work",
				"archive-history reports unexpected kind",
				"workbench held/open work would be cleared",
			},
			Boundary: "reversible COMPOUND_HOME operational archive only; no held/open work clearing, no receipts/transitions deletion, no external side effects",
			Proof:    "home doctor --json reports terminal_hot_work_count=0 and workbench still shows held valves after archive-history",
		})
	}
	if h.StaleLeaseCount > 0 {
		actions = append(actions, homeDoctorHygieneAction{
			ID:            "archive-stale-leases",
			Kind:          "operational_hygiene_repair",
			Title:         "Archive stale WorkEnvelope leases",
			Command:       []string{"compound", "home", "archive-history", "--kind", "lease"},
			DryRunCommand: []string{"compound", "home", "archive-history", "--kind", "lease", "--dry-run"},
			ExpectedValidation: []string{
				"stale_lease_count becomes 0",
				"leases for live non-terminal work remain hot",
				"held WorkEnvelope state remains visible in workbench",
			},
			StopConditions: []string{
				"dry-run includes a lease for live non-terminal work",
				"archive-history reports unexpected kind",
				"workbench held/open work would be cleared",
			},
			Boundary: "reversible COMPOUND_HOME lease archive only; no held/open work clearing, no work state mutation, no external side effects",
			Proof:    "home doctor --json reports stale_lease_count=0 and workbench still shows held valves after archive-history",
		})
	}
	return actions
}

func printHomeDoctor(r home.Report) {
	// generated state must not be tracked: a global home inside a git repo is the
	// smell; a --local home inside a repo is fine if it is gitignored.
	smell := r.InsideGitRepo && r.Manifest.HomeKind != "local"
	fmt.Printf("home: %s\n", r.Home)
	fmt.Printf("kind: %s\n", r.Manifest.HomeKind)
	fmt.Printf("manifest: %s\n", homeOKStr(r.ManifestOK))
	if !r.ManifestOK {
		fmt.Printf("manifest_error: %s\nmanifest_recovery: %s\n", r.ManifestError, r.ManifestRecovery)
	}
	fmt.Printf("dirs: %d/%d ok\n", r.DirsOK, len(home.HabitatDirs()))
	for _, line := range homeDoctorHabitatLines(r) {
		fmt.Println(line)
	}
	fmt.Printf("inside_git_repo: %v\n", r.InsideGitRepo)
	fmt.Printf("generated_state_in_repo_source: %v\n", smell)
	fmt.Printf("operational_hygiene: %s\n", homeCleanStr(r.HygieneOK))
	for _, item := range r.Hygiene {
		fmt.Printf("%s: %s\n", item.Name, homeCleanStr(item.OK))
	}
	for _, item := range r.BackupWarnings {
		fmt.Printf("%s: %s\n", item.Name, homeCleanStr(item.OK))
	}
	for _, w := range homeDoctorCapacityFeedWarnings(r.Home) {
		fmt.Printf("%s: %s — %s\n", w.Name, homeCleanStr(w.OK), w.Detail)
	}
	_ = home.WritePrivateCustodyReport(os.Stdout, r.PrivateCustody)
	for _, action := range buildHomeDoctorReport(r).OperationalHygieneActions {
		fmt.Printf("hygiene_action: %s kind=%s\n", action.ID, action.Kind)
		fmt.Printf("  command: %s\n", strings.Join(action.Command, " "))
		if len(action.DryRunCommand) > 0 {
			fmt.Printf("  dry_run: %s\n", strings.Join(action.DryRunCommand, " "))
		}
		if len(action.ExpectedValidation) > 0 {
			fmt.Printf("  expected_validation: %s\n", strings.Join(action.ExpectedValidation, "; "))
		}
		if len(action.StopConditions) > 0 {
			fmt.Printf("  stop_conditions: %s\n", strings.Join(action.StopConditions, "; "))
		}
		fmt.Printf("  boundary: %s\n", action.Boundary)
		fmt.Printf("  proof: %s\n", action.Proof)
	}
	fmt.Printf("setup: %s\n", homeOKStr(r.SetupOK))
	fmt.Printf("setup.install_dir: %s\n", r.Setup.InstallDir)
	if r.Setup.RequiredGoVersion != "" {
		fmt.Printf("setup.required_go: %s\n", r.Setup.RequiredGoVersion)
	}
	for _, item := range r.Setup.Checks {
		fmt.Printf("%s: %s — %s\n", item.Name, homeOKStr(item.OK), item.Detail)
		if !item.OK && item.Fix != "" {
			fmt.Printf("%s.fix: %s\n", item.Name, item.Fix)
		}
	}
	printHomeGovernanceDoctor(r.Home)
	fmt.Printf("verdict: %s\n", homeOKStr(r.OK && !smell && r.PrivateCustody.OK))
	if !r.OK || !r.PrivateCustody.OK {
		os.Exit(1)
	}
}

func printHomeArchiveReport(action string, r home.ArchiveReport) {
	fmt.Printf("# home %s\n", action)
	fmt.Printf("- kind: %s\n", r.Kind)
	fmt.Printf("- archive: %s\n", r.ArchivePath)
	fmt.Printf("- files: %d\n", r.Files)
	fmt.Printf("- bytes: %d\n", r.Bytes)
	for _, skipped := range r.Skipped {
		fmt.Printf("- skipped_root: %s\n", skipped)
	}
}

func printHomeVerifyReport(r home.VerifyReport) {
	fmt.Println("# home verify")
	fmt.Printf("- archive: %s\n", r.ArchivePath)
	fmt.Printf("- kind: %s\n", r.Kind)
	fmt.Printf("- files: %d\n", r.Files)
	fmt.Printf("- bytes: %d\n- archive_sha256: %s\n", r.Bytes, r.ArchiveSHA256)
	fmt.Printf("- verdict: %s\n", homeOKStr(r.OK))
}

func printHomeRestoreReport(r home.RestoreReport) {
	fmt.Println("# home restore")
	fmt.Printf("- archive: %s\n", r.ArchivePath)
	fmt.Printf("- target_home: %s\n", r.TargetHome)
	fmt.Printf("- kind: %s\n", r.Kind)
	fmt.Printf("- files: %d\n", r.Files)
	fmt.Printf("- bytes: %d\n", r.Bytes)
	fmt.Println("- verdict: ok")
}

func printHomeOperationalArchiveReport(action string, r home.OperationalArchiveReport) {
	fmt.Printf("# home %s\n", action)
	fmt.Printf("- home: %s\n", r.Home)
	fmt.Printf("- dry_run: %v\n", r.DryRun)
	if r.ArchivedAt != "" {
		fmt.Printf("- archived_at: %s\n", r.ArchivedAt)
	}
	if r.RestoredAt != "" {
		fmt.Printf("- restored_at: %s\n", r.RestoredAt)
	}
	if r.ArchivedCount > 0 {
		fmt.Printf("- archived: %d\n", r.ArchivedCount)
	}
	if r.RestoredCount > 0 {
		fmt.Printf("- restored: %d\n", r.RestoredCount)
	}
	if r.MaxFiles > 0 {
		fmt.Printf("- max_files: %d\n", r.MaxFiles)
	}
	for _, kind := range sortedStringKeys(r.ResumedFrom) {
		fmt.Printf("- resumed_from: %s %s\n", kind, r.ResumedFrom[kind])
	}
	for _, kind := range sortedMapKeys(r.Counts) {
		fmt.Printf("- %s: %d\n", kind, r.Counts[kind])
	}
	for _, entry := range r.Entries {
		fmt.Printf("- entry: %s %s %s -> %s\n", entry.ID, entry.Kind, entry.OriginalPath, entry.ArchivePath)
	}
	for _, kept := range r.Kept {
		fmt.Printf("- kept: %s %s status=%s kept_reason=%s\n", kept.Kind, kept.OriginalPath, kept.Status, kept.KeptReason)
	}
}

func printHomeCompactReport(r home.CompactReport) {
	fmt.Println("# home compact")
	fmt.Printf("- home: %s\n", r.Home)
	fmt.Printf("- dry_run: %v\n", r.DryRun)
	fmt.Printf("- removed_files: %d\n", r.RemovedFiles)
	fmt.Printf("- removed_bytes: %d\n", r.RemovedBytes)
	for _, dir := range r.ClearedDirs {
		fmt.Printf("- cleared_dir: %s\n", dir)
	}
}

// cmdEnvelope validates typed runtime envelopes (docs/architecture/runtime.md).
func cmdEnvelope(args []string) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("usage: compound envelope <validate <file>...|kinds>")
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound envelope <validate <file>...|kinds>")
		os.Exit(2)
	}
	switch args[0] {
	case "kinds":
		for _, k := range envelope.Kinds() {
			fmt.Println(k)
		}
	case "validate":
		files := args[1:]
		if len(files) == 0 {
			fmt.Fprintln(os.Stderr, "usage: compound envelope validate <file>...")
			os.Exit(2)
		}
		bad := 0
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				fmt.Printf("%s: read error: %v\n", f, err)
				bad++
				continue
			}
			kind, err := envelope.ValidateBytes(data)
			if err != nil {
				fmt.Printf("%s: INVALID (%s): %v\n", f, kind, err)
				bad++
				continue
			}
			fmt.Printf("%s: ok (%s)\n", f, kind)
		}
		if bad > 0 {
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: compound envelope <validate <file>...|kinds>")
		os.Exit(2)
	}
}

func cmdSchema(args []string) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("usage: compound schema <list|export|validate> ...")
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: compound schema <list|export|validate> ...")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		for _, c := range schema.Contracts() {
			fmt.Printf("%-20s %s\n", c.Name, c.Description)
		}
	case "export":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: compound schema export <name>|--all [--dir <dir>]")
			os.Exit(2)
		}
		if args[1] == "--all" {
			dir := valueFlag(args[2:], "--dir")
			if dir == "" {
				all := map[string]any{}
				for _, c := range schema.Contracts() {
					doc, err := schema.Export(c.Name)
					die(err)
					all[c.Name] = doc
				}
				printJSON(all)
				return
			}
			die(os.MkdirAll(dir, 0o755))
			for _, c := range schema.Contracts() {
				doc, err := schema.Export(c.Name)
				die(err)
				b, err := json.MarshalIndent(doc, "", "  ")
				die(err)
				path := filepath.Join(dir, c.Name+".schema.json")
				die(os.WriteFile(path, append(b, '\n'), 0o644))
				fmt.Printf("- %s\n", path)
			}
			return
		}
		doc, err := schema.Export(args[1])
		die(err)
		printJSON(doc)
	case "validate":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: compound schema validate <name> <file>")
			os.Exit(2)
		}
		die(schema.Validate(args[1], args[2]))
		fmt.Printf("%s: ok (%s)\n", args[2], args[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: compound schema <list|export|validate> ...")
		os.Exit(2)
	}
}

func cmdRetiredGen1(verb string) {
	replacements := map[string]string{
		"distill":   "compound distiller run <trace.json>",
		"scope":     "compound memory entity resolve <token>",
		"promote":   "compound memory adjudicate <candidate-id> --verdict promote",
		"build":     "compound memory positions / compound memory patterns",
		"reconcile": "compound memory adjudicate (supersession is spine-native)",
		"timeline":  "compound memory timeline <entity>",
		"recall":    "compound memory recall <query>",
		"pack":      "compound memory query <query>",
		"ledger":    "compound memory doctor",
	}
	fmt.Fprintf(os.Stderr, "compound %s retired with the Gen-1 memory stack. Use %s.\n", verb, replacements[verb])
	os.Exit(2)
}

func openMemStore() (*mem.Store, string) {
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run compound home init)")
		os.Exit(1)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return store, homePath
}

func loadSourceControlMutationBinding(homePath string, actionIntentIDs []string) (authority.SourceControlMutationBinding, error) {
	if strings.TrimSpace(homePath) == "" {
		return authority.SourceControlMutationBinding{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if len(actionIntentIDs) == 0 {
		return authority.SourceControlMutationBinding{}, fmt.Errorf("at least one persisted action_intent required")
	}
	binding := authority.SourceControlMutationBinding{}
	seen := map[string]struct{}{}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return authority.SourceControlMutationBinding{}, err
	}
	for _, rawID := range actionIntentIDs {
		id := strings.TrimSpace(rawID)
		if id == "" || filepath.Base(id) != id || !strings.HasPrefix(id, "act_") {
			return authority.SourceControlMutationBinding{}, fmt.Errorf("action_intent id %q must be a bare act_ id", rawID)
		}
		if _, duplicate := seen[id]; duplicate {
			return authority.SourceControlMutationBinding{}, fmt.Errorf("duplicate action_intent %s", id)
		}
		seen[id] = struct{}{}
		record, err := events.Read(id)
		if err != nil {
			return authority.SourceControlMutationBinding{}, fmt.Errorf("read persisted action_intent %s: %w", id, err)
		}
		var action envelope.ActionIntent
		if err := json.Unmarshal(record.Bytes, &action); err != nil {
			return authority.SourceControlMutationBinding{}, fmt.Errorf("decode persisted action_intent %s: %w", id, err)
		}
		if action.ID != id {
			return authority.SourceControlMutationBinding{}, fmt.Errorf("persisted action_intent %s contains id %q", id, action.ID)
		}
		if err := action.Validate(); err != nil {
			return authority.SourceControlMutationBinding{}, fmt.Errorf("persisted action_intent %s invalid: %w", id, err)
		}
		inputHash, err := persistedMutationInputHash(action)
		if err != nil {
			return authority.SourceControlMutationBinding{}, fmt.Errorf("persisted action_intent %s: %w", id, err)
		}
		if len(binding.ActionIntentIDs) == 0 {
			binding.ActionWorkID = strings.TrimSpace(action.IDs.WorkID)
			binding.MutationInputHash = inputHash
			binding.Capability = strings.TrimSpace(action.Capability)
			binding.Action = strings.TrimSpace(action.Action)
			binding.MutationClass = strings.TrimSpace(action.MutationClass)
		} else {
			if strings.TrimSpace(action.IDs.WorkID) != binding.ActionWorkID {
				return authority.SourceControlMutationBinding{}, fmt.Errorf("persisted ActionIntents must share action work_id")
			}
			if inputHash != binding.MutationInputHash {
				return authority.SourceControlMutationBinding{}, fmt.Errorf("persisted ActionIntents must share mutation input hash")
			}
			if strings.TrimSpace(action.Capability) != binding.Capability {
				return authority.SourceControlMutationBinding{}, fmt.Errorf("persisted ActionIntents must share capability")
			}
			if strings.TrimSpace(action.Action) != binding.Action {
				return authority.SourceControlMutationBinding{}, fmt.Errorf("persisted ActionIntents must share action")
			}
			if strings.TrimSpace(action.MutationClass) != binding.MutationClass {
				return authority.SourceControlMutationBinding{}, fmt.Errorf("persisted ActionIntents must share mutation class")
			}
		}
		binding.TargetPaths = append(binding.TargetPaths, action.Target.Path)
		binding.ActionIntentIDs = append(binding.ActionIntentIDs, action.ID)
	}
	return binding, nil
}

func persistedMutationInputHash(action envelope.ActionIntent) (string, error) {
	value := ""
	for _, ref := range action.EvidenceRefs {
		if !strings.HasPrefix(ref, "mutation-input:sha256:") {
			continue
		}
		if value != "" && value != ref {
			return "", fmt.Errorf("multiple mutation-input hashes")
		}
		value = ref
	}
	if value == "" {
		return "", fmt.Errorf("session-bound mutation-input hash required")
	}
	return value, nil
}

// writebackSpec is the per-doctor parameterization of the one writeback
// choreography (runDoctorWriteback). Each doctor differs only in these values
// plus how it builds its OwnerWritebackGate and summaries; the spine
// (work create→claim→advance→ActionIntent→policy→gate→receipt→transition) is shared.
type writebackSpec struct {
	Owner            string
	DesiredOutcome   string
	SuccessPredicate string
	NonGoals         []string
	Risk             string
	Privacy          string
	Actor            string
	IDParts          []string // cliID middle parts; cliTS(now) is appended
	Capability       string
	Action           string
	TargetKind       string
	TargetID         string
	TargetPath       string
	Preview          string
	RollbackPlan     string
	RunningNote      string
	CheckedNote      string
	EvidenceRefs     []string
	Verification     string // post-check VerificationID; "" => the checked gate's CandidateRef
	MoveType         string // success-close move_type
	BlockedSummary   func(status, gateID string) string
	AllowedSummary   func(status, gateID string) string
	GateFor          func(ai envelope.ActionIntent, pd envelope.PolicyDecision) writeback.OwnerWritebackGate
}

// runDoctorWriteback runs the one governed owner-writeback choreography shared by
// every doctor: open work + writeback stores, create/claim a work envelope,
// propose the ActionIntent, evaluate policy and the writeback gate, then close
// the work (control_plane_repair/outward_delta, or audit_only when the gate
// blocks) while recording the gate and the receipt/transition refs.
// runDoctorWriteback is the thin CLI adapter: it resolves the home and hands the
// per-domain spec to writeback.RunGatedWriteback, which owns the spine. The CLI
// keeps only what it owns — the id/header scheme (ActionIntentFor) and policy
// config (Evaluate) — while the work→ActionIntent→policy→gate→receipt→transition
// choreography lives behind one internal seam.
func runDoctorWriteback(spec writebackSpec, now time.Time) (writebackWorkResult, error) {
	homePath, err := home.Resolve("")
	if err != nil {
		return writebackWorkResult{}, err
	}
	res, err := writeback.RunGatedWriteback(writeback.GatedWriteback{
		Home: homePath,
		Now:  now,
		Work: work.CreateInput{
			Owner:            spec.Owner,
			DesiredOutcome:   spec.DesiredOutcome,
			SuccessPredicate: spec.SuccessPredicate,
			NonGoals:         spec.NonGoals,
			State:            "ready",
			Risk:             spec.Risk,
			Privacy:          spec.Privacy,
		},
		Actor:        spec.Actor,
		RunningNote:  spec.RunningNote,
		CheckedNote:  spec.CheckedNote,
		Verification: spec.Verification,
		MoveType:     spec.MoveType,
		ActionIntentFor: func(workID string) envelope.ActionIntent {
			idParts := append(append([]string{}, spec.IDParts...), cliTS(now))
			return envelope.ActionIntent{
				Header:        cliHeader(envelope.SVActionIntent, cliID("act_", idParts...), workID, now, spec.EvidenceRefs),
				Actor:         spec.Actor,
				Capability:    spec.Capability,
				Action:        spec.Action,
				Target:        envelope.ActionTarget{Kind: spec.TargetKind, ID: spec.TargetID, Path: spec.TargetPath},
				MutationClass: "local_reversible",
				Reversibility: "easy",
				BlastRadius:   "local",
				Preview:       spec.Preview,
				RollbackPlan:  spec.RollbackPlan,
			}
		},
		Evaluate: func(ai envelope.ActionIntent) envelope.PolicyDecision {
			return policy.Evaluate(ai, policyConfig(), now)
		},
		GateFor:        spec.GateFor,
		BlockedSummary: spec.BlockedSummary,
		AllowedSummary: spec.AllowedSummary,
	})
	return writebackWorkResult{
		WorkID:           res.WorkID,
		ActionIntentID:   res.ActionIntentID,
		PolicyDecisionID: res.PolicyDecisionID,
		GateID:           res.GateID,
		GateStatus:       res.GateStatus,
		ReceiptID:        res.ReceiptID,
		TransitionID:     res.TransitionID,
	}, err
}

func runDecisionWriteback(report decisions.DoctorReport, now time.Time) (writebackWorkResult, error) {
	if !decisions.NeedsWriteback(report) {
		return writebackWorkResult{}, nil
	}
	gapCount := len(report.LifecycleGaps)
	return runDoctorWriteback(writebackSpec{
		Owner:            "decisions:lifecycle",
		DesiredOutcome:   "Route decision lifecycle gaps through governed owner writeback",
		SuccessPredicate: "decision writeback gate recorded with receipt/transition refs",
		NonGoals:         []string{"mutate decision owner files", "promote decision truth"},
		Risk:             "tier1",
		Privacy:          "private",
		Actor:            "compound decisions doctor",
		IDParts:          []string{"decision-writeback", strconv.Itoa(gapCount)},
		Capability:       "decisions.writeback",
		Action:           "record_decision_lifecycle_writeback_gate",
		TargetKind:       "decision_lifecycle",
		TargetID:         "spine-memory-decisions",
		Preview:          fmt.Sprintf("decisions doctor found %d lifecycle gap(s); record owner writeback gate", gapCount),
		RollbackPlan:     "remove COMPOUND_HOME writeback gate/work records before any owner file is changed",
		RunningNote:      "decisions doctor running writeback",
		CheckedNote:      "decision writeback gate checked",
		EvidenceRefs:     decisions.EvidenceRefs(report),
		MoveType:         "control_plane_repair",
		BlockedSummary: func(status, _ string) string {
			return fmt.Sprintf("decision lifecycle writeback blocked for %d gap(s): gate %s", gapCount, status)
		},
		AllowedSummary: func(_, gateID string) string {
			return fmt.Sprintf("%d decision lifecycle gap(s) routed through owner writeback gate %s", gapCount, gateID)
		},
		GateFor: func(ai envelope.ActionIntent, pd envelope.PolicyDecision) writeback.OwnerWritebackGate {
			return decisions.WritebackGate(report, ai.ID, pd.ID, now)
		},
	}, now)
}

func runRuntimeWriteback(report runtimebinding.DoctorReport, now time.Time) (writebackWorkResult, error) {
	if !runtimebinding.NeedsWriteback(report) {
		return writebackWorkResult{}, nil
	}
	return runDoctorWriteback(writebackSpec{
		Owner:            "runtime:" + report.ServiceID,
		DesiredOutcome:   "Route failing runtime doctor report through governed owner writeback",
		SuccessPredicate: "runtime writeback gate recorded with receipt/transition refs",
		NonGoals:         []string{"mutate runtime owner files", "promote runtime truth"},
		Risk:             "tier1",
		Privacy:          "private",
		Actor:            "compound runtime doctor",
		IDParts:          []string{"runtime-writeback", report.ServiceID, report.Status},
		Capability:       "runtime.writeback",
		Action:           "record_runtime_writeback_gate",
		TargetKind:       "runtime_binding",
		TargetID:         report.ServiceID,
		Preview:          fmt.Sprintf("runtime doctor for %s is %s; record owner writeback gate", report.ServiceID, report.Status),
		RollbackPlan:     "remove COMPOUND_HOME writeback gate/work records before any owner file is changed",
		RunningNote:      "runtime doctor running writeback",
		CheckedNote:      "runtime writeback gate checked",
		EvidenceRefs:     runtimebinding.EvidenceRefs(report),
		Verification:     "runtime:" + report.ServiceID + ":" + report.Status,
		MoveType:         "control_plane_repair",
		BlockedSummary: func(status, _ string) string {
			return fmt.Sprintf("runtime writeback blocked for %s: gate %s", report.ServiceID, status)
		},
		AllowedSummary: func(_, gateID string) string {
			return fmt.Sprintf("runtime %s %s routed through owner writeback gate %s", report.ServiceID, report.Status, gateID)
		},
		GateFor: func(ai envelope.ActionIntent, pd envelope.PolicyDecision) writeback.OwnerWritebackGate {
			return runtimebinding.WritebackGate(report, ai.ID, pd.ID, now)
		},
	}, now)
}

func runDistillerWriteback(report distiller.DoctorReport, now time.Time) (writebackWorkResult, error) {
	if !distiller.NeedsWriteback(report) {
		return writebackWorkResult{}, nil
	}
	debtCount := report.SelfImprovementDebt
	return runDoctorWriteback(writebackSpec{
		Owner:            "distiller:policy",
		DesiredOutcome:   "Route distiller method/policy debt through governed owner writeback",
		SuccessPredicate: "distiller writeback gate recorded with receipt/transition refs",
		NonGoals:         []string{"mutate distillation policy", "call model extractor", "promote memory truth"},
		Risk:             "tier1",
		Privacy:          "private",
		Actor:            "compound distiller doctor",
		IDParts:          []string{"distiller-writeback", strconv.Itoa(report.MethodUpgradeDebt), strconv.Itoa(report.PolicyPatchDebt)},
		Capability:       "distiller.writeback",
		Action:           "record_distillation_policy_writeback_gate",
		TargetKind:       "distillation_policy",
		TargetID:         distiller.DefaultPolicy().Stamp(),
		Preview:          fmt.Sprintf("distiller doctor found %d method/policy debt item(s); record owner writeback gate", debtCount),
		RollbackPlan:     "remove COMPOUND_HOME writeback gate/work records before any distillation policy is changed",
		RunningNote:      "distiller doctor running writeback",
		CheckedNote:      "distiller writeback gate checked",
		EvidenceRefs:     distiller.EvidenceRefs(report),
		MoveType:         "control_plane_repair",
		BlockedSummary: func(status, _ string) string {
			return fmt.Sprintf("distiller method/policy writeback blocked for %d debt item(s): gate %s", debtCount, status)
		},
		AllowedSummary: func(_, gateID string) string {
			return fmt.Sprintf("%d distiller method/policy debt item(s) routed through owner writeback gate %s", debtCount, gateID)
		},
		GateFor: func(ai envelope.ActionIntent, pd envelope.PolicyDecision) writeback.OwnerWritebackGate {
			return distiller.WritebackGate(report, ai.ID, pd.ID, now)
		},
	}, now)
}

func cmdRuntime(args []string) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println(runtimeUsage())
		return
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, runtimeUsage())
		os.Exit(2)
	}
	switch args[0] {
	case "doctor":
		cmdRuntimeDoctor(args[1:])
	case "repair-plan":
		cmdRuntimeRepairPlan(args[1:])
	case "repair-review":
		cmdRuntimeRepairReview(args[1:])
	default:
		fmt.Fprintln(os.Stderr, runtimeUsage())
		os.Exit(2)
	}
}

func runtimeUsage() string {
	return "usage: compound runtime doctor [--project-contract <yaml> | --switchback-pulse <json> | <yaml>] [--live] [--writeback] | runtime repair-plan [--project-contract <yaml> | --switchback-pulse <json> | <yaml>] [--live] | runtime repair-review <review.yaml>"
}

func cmdRuntimeDoctor(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: compound runtime doctor [--project-contract <yaml> | --switchback-pulse <json> | <yaml>] [--live] [--writeback]")
		os.Exit(2)
	}
	now := time.Now()
	report := loadRuntimeReportOrExit(args, "runtime doctor", hasFlag(args, "--live"), now)
	printRuntimeDoctorReport(report)
	if report.Status != "ok" {
		if hasFlag(args, "--writeback") {
			result, err := runRuntimeWriteback(report, now)
			if err != nil {
				fmt.Fprintln(os.Stderr, "runtime writeback:", err)
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
			return
		}
		os.Exit(1)
	}
}

func cmdRuntimeRepairPlan(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: compound runtime repair-plan [--project-contract <yaml> | --switchback-pulse <json> | <yaml>] [--live]")
		os.Exit(2)
	}
	now := time.Now()
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime repair-plan:", err)
		os.Exit(1)
	}
	report := loadRuntimeReportOrExit(args, "runtime repair-plan", hasFlag(args, "--live"), now)
	if !runtimebinding.NeedsWriteback(report) {
		fmt.Fprintf(os.Stderr, "runtime repair-plan: runtime %s ok; no repair plan needed\n", report.ServiceID)
		os.Exit(1)
	}
	writebackResult, err := runRuntimeWriteback(report, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime repair-plan:", err)
		os.Exit(1)
	}
	plan, err := runtimebinding.BuildRepairPlan(report, runtimebinding.RepairRefs{
		WritebackGateID: writebackResult.GateID,
		WorkID:          writebackResult.WorkID,
		ReceiptID:       writebackResult.ReceiptID,
		TransitionID:    writebackResult.TransitionID,
	}, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime repair-plan:", err)
		os.Exit(1)
	}
	paths, err := runtimebinding.RecordRepairPlan(homePath, plan)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime repair-plan:", err)
		os.Exit(1)
	}
	fmt.Printf("# runtime repair-plan %s\n", plan.ID)
	fmt.Printf("- service: %s\n", plan.ServiceID)
	fmt.Printf("- status: %s\n", plan.Status)
	fmt.Printf("- failed_checks: %d\n", plan.FailedCount)
	fmt.Printf("- writeback_gate: %s\n", plan.WritebackGateID)
	fmt.Printf("- work: %s\n", plan.WorkID)
	fmt.Printf("- receipt: %s\n", plan.ReceiptID)
	fmt.Printf("- transition: %s\n", plan.TransitionID)
	fmt.Printf("- plan: %s\n", paths.PlanPath)
	fmt.Printf("- review: %s\n", paths.ReviewPath)
	fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
}

func cmdRuntimeRepairReview(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: compound runtime repair-review <review.yaml>")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime repair-review:", err)
		os.Exit(1)
	}
	review, err := runtimebinding.LoadRepairReview(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime repair-review:", err)
		os.Exit(1)
	}
	report, err := runtimebinding.RecordRepairReview(homePath, review, args[0], time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime repair-review:", err)
		os.Exit(1)
	}
	fmt.Printf("# runtime repair-review %s\n\n", report.ID)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- plan: %s\n", report.PlanID)
	fmt.Printf("- service: %s\n", report.ServiceID)
	if report.Owner != "" {
		fmt.Printf("- owner: %s\n", report.Owner)
	}
	fmt.Printf("- decision: %s\n", report.Decision)
	if report.NextAction != "" {
		fmt.Printf("- next_action: %s\n", report.NextAction)
	}
	fmt.Printf("- reviewed: %s\n", report.ReviewPath)
}

func loadRuntimeReportOrExit(args []string, label string, live bool, now time.Time) runtimebinding.DoctorReport {
	path := ""
	sourceKind := "bundle"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project-contract":
			if sourceKind != "bundle" || path != "" {
				fmt.Fprintf(os.Stderr, "%s: choose exactly one runtime evidence source\n", label)
				os.Exit(2)
			}
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "usage: compound %s --project-contract <project.yaml>\n", label)
				os.Exit(2)
			}
			sourceKind = "project-contract"
			path = args[i+1]
			i++
		case "--switchback-pulse":
			if sourceKind != "bundle" || path != "" {
				fmt.Fprintf(os.Stderr, "%s: choose exactly one runtime evidence source\n", label)
				os.Exit(2)
			}
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "usage: compound %s --switchback-pulse <pulse.json>\n", label)
				os.Exit(2)
			}
			sourceKind = "switchback-pulse"
			path = args[i+1]
			i++
		case "--live", "--writeback":
			continue
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "%s: unknown option %s\n", label, args[i])
				os.Exit(2)
			}
			if path == "" {
				path = args[i]
			} else {
				fmt.Fprintf(os.Stderr, "%s: choose exactly one runtime evidence source\n", label)
				os.Exit(2)
			}
		}
	}
	if path == "" {
		fmt.Fprintf(os.Stderr, "usage: compound %s [--project-contract <project.yaml> | --switchback-pulse <pulse.json> | <bundle.yaml>]\n", label)
		os.Exit(2)
	}
	if sourceKind == "switchback-pulse" {
		if live {
			fmt.Fprintf(os.Stderr, "%s: --switchback-pulse cannot be combined with --live\n", label)
			os.Exit(2)
		}
		report, err := runtimebinding.LoadSwitchbackPulseReport(path, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, label+":", err)
			os.Exit(1)
		}
		return report
	}

	load := runtimebinding.Load
	if sourceKind == "project-contract" {
		load = runtimebinding.LoadProjectContract
	}
	bundle, err := load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, label+":", err)
		os.Exit(1)
	}
	if live {
		return runtimebinding.DoctorWithLiveProbes(context.Background(), bundle, now)
	}
	return runtimebinding.Doctor(bundle)
}

func printRuntimeDoctorReport(report runtimebinding.DoctorReport) {
	fmt.Printf("# runtime doctor\n\n")
	fmt.Printf("- service: %s\n", report.ServiceID)
	fmt.Printf("- owner: %s\n", report.Owner)
	fmt.Printf("- status: %s\n", report.Status)
	for _, check := range report.Checks {
		fmt.Printf("- %s: %s", check.Name, check.Status)
		if check.Detail != "" {
			fmt.Printf(" (%s)", check.Detail)
		}
		fmt.Println()
	}
}

type writebackWorkResult struct {
	WorkID           string `json:"work_id"`
	ActionIntentID   string `json:"action_intent_id"`
	PolicyDecisionID string `json:"policy_decision_id"`
	GateID           string `json:"gate_id"`
	GateStatus       string `json:"gate_status"`
	ReceiptID        string `json:"receipt_id,omitempty"`
	TransitionID     string `json:"transition_id,omitempty"`
}

func runProductWriteback(report product.DoctorReport, now time.Time) (writebackWorkResult, error) {
	if !product.NeedsWriteback(report) {
		return writebackWorkResult{}, nil
	}
	return runDoctorWriteback(writebackSpec{
		Owner:            "product_strategy:" + report.ProductID,
		DesiredOutcome:   "Route product feedback review through governed owner writeback",
		SuccessPredicate: "product feedback writeback gate recorded with receipt/transition refs",
		NonGoals:         []string{"mutate product pages", "declare product causality", "execute next product action"},
		Risk:             "tier1",
		Privacy:          "private",
		Actor:            "compound product doctor",
		IDParts:          []string{"product-writeback", report.ProductID, strconv.Itoa(report.PendingReviewCount), strconv.Itoa(report.SourceIssueCount)},
		Capability:       "product.writeback",
		Action:           "record_product_feedback_writeback_gate",
		TargetKind:       "product_strategy",
		TargetID:         report.ProductID,
		Preview:          fmt.Sprintf("product doctor found %d pending hypothesis review(s) and %d source issue(s); record owner writeback gate", report.PendingReviewCount, report.SourceIssueCount),
		RollbackPlan:     "remove COMPOUND_HOME writeback gate/work records before any product owner file or external surface is changed",
		RunningNote:      "product doctor running writeback",
		CheckedNote:      "product hypothesis writeback gate checked",
		EvidenceRefs:     product.EvidenceRefs(report),
		MoveType:         "outward_delta",
		BlockedSummary: func(_, gateID string) string {
			return fmt.Sprintf("product feedback writeback blocked for %s: gate %s", report.ProductID, gateID)
		},
		AllowedSummary: func(_, gateID string) string {
			return fmt.Sprintf("product %s feedback review routed through owner writeback gate %s", report.ProductID, gateID)
		},
		GateFor: func(ai envelope.ActionIntent, pd envelope.PolicyDecision) writeback.OwnerWritebackGate {
			return product.WritebackGate(report, ai.ID, pd.ID, now)
		},
	}, now)
}

func runProjectionWriteback(report projection.Freshness, now time.Time) (writebackWorkResult, error) {
	if !projection.NeedsWriteback(report) {
		return writebackWorkResult{}, nil
	}
	return runDoctorWriteback(writebackSpec{
		Owner:            "projection:" + report.ProjectionID,
		DesiredOutcome:   "Route stale/drifted projection through governed owner writeback",
		SuccessPredicate: "writeback gate recorded with receipt/transition refs",
		NonGoals:         []string{"regenerate the projection output", "promote projection truth"},
		Risk:             "tier1",
		Privacy:          "private",
		Actor:            "compound projection doctor",
		IDParts:          []string{"projection-writeback", report.ProjectionID, report.Status},
		Capability:       "projection.writeback",
		Action:           "record_projection_writeback_gate",
		TargetKind:       "generated_view",
		TargetID:         report.ProjectionID,
		TargetPath:       report.ViewRef,
		Preview:          fmt.Sprintf("projection %s is %s; record owner writeback gate for %s", report.ProjectionID, report.Status, report.ViewRef),
		RollbackPlan:     "remove the COMPOUND_HOME writeback gate/work records before any owner file is changed",
		RunningNote:      "projection doctor running writeback",
		CheckedNote:      "projection writeback gate checked",
		EvidenceRefs:     projection.EvidenceRefs(report),
		Verification:     "projection:" + report.ProjectionID + ":" + report.Status,
		MoveType:         "control_plane_repair",
		BlockedSummary: func(status, _ string) string {
			return fmt.Sprintf("projection writeback blocked for %s: gate %s", report.ProjectionID, status)
		},
		AllowedSummary: func(_, gateID string) string {
			return fmt.Sprintf("projection %s %s routed through owner writeback gate %s", report.ProjectionID, report.Status, gateID)
		},
		GateFor: func(ai envelope.ActionIntent, pd envelope.PolicyDecision) writeback.OwnerWritebackGate {
			return projection.WritebackGate(report, ai.ID, pd.ID, now)
		},
	}, now)
}

func runProjectionSourceObservation(report projection.Freshness, now time.Time) (sourceobs.Report, error) {
	var result sourceobs.Report
	if !projection.NeedsWriteback(report) {
		return result, nil
	}
	homePath, err := home.Resolve("")
	if err != nil {
		return result, err
	}
	obs := sourceobs.FromProjectionFreshness(report, now)
	return sourceobs.Apply(homePath, obs, now)
}

func runProjectionContextRecord(report projection.Freshness, repoKindArg string, now time.Time) (projection.ContextRecord, string, error) {
	homePath, err := home.Resolve("")
	if err != nil {
		return projection.ContextRecord{}, "", err
	}
	return projection.RecordContext(homePath, report, splitCSV(repoKindArg), now)
}

func cmdProjectionRepairPlan(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: compound projection repair-plan <freshness.yaml>")
		os.Exit(2)
	}
	now := time.Now()
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-plan:", err)
		os.Exit(1)
	}
	report, err := projection.DoctorFile(args[0], now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-plan:", err)
		os.Exit(1)
	}
	if !projection.NeedsWriteback(report) {
		fmt.Fprintf(os.Stderr, "projection repair-plan: projection %s is fresh; no repair plan needed\n", report.ProjectionID)
		os.Exit(1)
	}
	writebackResult, err := runProjectionWriteback(report, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-plan:", err)
		os.Exit(1)
	}
	sourceReport, err := runProjectionSourceObservation(report, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-plan:", err)
		os.Exit(1)
	}
	plan, err := projection.BuildRegenerationPlan(report, projection.RegenerationRefs{
		WritebackGateID:     writebackResult.GateID,
		WorkID:              writebackResult.WorkID,
		ReceiptID:           writebackResult.ReceiptID,
		TransitionID:        writebackResult.TransitionID,
		SourceObservationID: sourceReport.ObservationID,
		CandidateIDs:        sourceReport.CandidateIDs,
		NoPromoteIDs:        sourceReport.NoPromoteIDs,
	}, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-plan:", err)
		os.Exit(1)
	}
	paths, err := projection.RecordRegenerationPlan(homePath, plan)
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-plan:", err)
		os.Exit(1)
	}
	fmt.Printf("# projection repair-plan\n\n")
	fmt.Printf("- plan: %s\n", plan.ID)
	fmt.Printf("- projection: %s\n", plan.ProjectionID)
	fmt.Printf("- status: %s\n", plan.Status)
	fmt.Printf("- freshness: %s\n", plan.FreshnessStatus)
	fmt.Printf("- writeback_gate: %s %s\n", writebackResult.GateID, writebackResult.GateStatus)
	fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
	for _, id := range sourceReport.CandidateIDs {
		fmt.Printf("- source_candidate: %s\n", id)
	}
	for _, id := range sourceReport.NoPromoteIDs {
		fmt.Printf("- source_no_promote: %s\n", id)
	}
	fmt.Printf("- review: %s\n", paths.ReviewPath)
	if paths.ReviewYAMLPath != "" {
		fmt.Printf("- review_yaml: %s\n", paths.ReviewYAMLPath)
	}
	fmt.Printf("- artifact: %s\n", paths.PlanPath)
}

func cmdProjectionRepairReview(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: compound projection repair-review <review.yaml>")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-review:", err)
		os.Exit(1)
	}
	review, err := projection.LoadRegenerationReview(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-review:", err)
		os.Exit(1)
	}
	report, err := projection.RecordRegenerationReview(homePath, review, args[0], time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection repair-review:", err)
		os.Exit(1)
	}
	fmt.Printf("# projection repair-review %s\n\n", report.ID)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- plan: %s\n", report.PlanID)
	fmt.Printf("- projection: %s\n", report.ProjectionID)
	fmt.Printf("- view: %s\n", report.ViewRef)
	fmt.Printf("- decision: %s\n", report.Decision)
	if report.NextAction != "" {
		fmt.Printf("- next_action: %s\n", report.NextAction)
	}
	fmt.Printf("- reviewed: %s\n", report.ReviewPath)
}

func projectionUsage() string {
	return "usage: compound projection doctor <freshness.yaml> [--writeback] [--source-observe] [--record-context --repo-kind K] | projection dashboard-read-model <read-model.json> [--max-age-hours N] [--writeback] [--source-observe] [--record-context --repo-kind K] | projection repair-plan <freshness.yaml> | projection repair-review <review.yaml>"
}

func cmdProjectionDashboardReadModel(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: compound projection dashboard-read-model <read-model.json> [--max-age-hours N] [--writeback] [--source-observe] [--record-context --repo-kind K]")
		os.Exit(2)
	}
	maxAgeHours := projection.DefaultDashboardMaxAgeHours
	if v := valueFlag(args[1:], "--max-age-hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "projection dashboard-read-model: --max-age-hours must be a positive integer")
			os.Exit(2)
		}
		maxAgeHours = n
	}
	now := time.Now()
	report, err := projection.DoctorDashboardReadModel(args[0], maxAgeHours, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection dashboard-read-model:", err)
		os.Exit(1)
	}
	printProjectionDoctorReport(report)
	finishProjectionDoctor(report, hasFlag(args[1:], "--writeback"), hasFlag(args[1:], "--source-observe"), hasFlag(args[1:], "--record-context"), valueFlag(args[1:], "--repo-kind"), now)
}

func printProjectionDoctorReport(report projection.Freshness) {
	fmt.Printf("# projection doctor\n\n")
	fmt.Printf("- projection: %s\n", report.ProjectionID)
	fmt.Printf("- view: %s\n", report.ViewRef)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- source_ledger_seq: %d\n", report.SourceLedgerSeq)
	if report.Staleness != "" {
		fmt.Printf("- staleness: %s\n", report.Staleness)
	}
	fmt.Printf("- output_hash: %s\n", report.OutputHash)
	maxPrintedSources := 20
	if len(report.Sources) > maxPrintedSources {
		fmt.Printf("- sources: %d total (showing first %d)\n", len(report.Sources), maxPrintedSources)
	}
	for i, source := range report.Sources {
		if i >= maxPrintedSources {
			fmt.Printf("- source: ... %d more\n", len(report.Sources)-maxPrintedSources)
			break
		}
		fmt.Printf("- source: %s %s", source.SourceRef, source.Status)
		if source.CurrentHash != "" {
			fmt.Printf(" (%s)", source.CurrentHash)
		}
		fmt.Println()
	}
	for _, check := range report.Checks {
		fmt.Printf("- check:%s %s", check.Name, check.Status)
		if check.Detail != "" {
			fmt.Printf(" (%s)", check.Detail)
		}
		fmt.Println()
	}
}

func finishProjectionDoctor(report projection.Freshness, writebackEnabled, sourceObserveEnabled, recordContextEnabled bool, contextRepoKind string, now time.Time) {
	if recordContextEnabled {
		record, path, err := runProjectionContextRecord(report, contextRepoKind, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "projection context record:", err)
			os.Exit(1)
		}
		fmt.Printf("- context_record: %s\n", record.ID)
		fmt.Printf("- context_repo_kinds: %s\n", strings.Join(record.RepoKinds, ","))
		fmt.Printf("- context_path: %s\n", path)
	}
	if report.Status != projection.StatusFresh {
		if writebackEnabled {
			result, err := runProjectionWriteback(report, now)
			if err != nil {
				fmt.Fprintln(os.Stderr, "projection doctor writeback:", err)
				os.Exit(1)
			}
			fmt.Printf("- writeback_work: %s\n", result.WorkID)
			fmt.Printf("- writeback_gate: %s %s\n", result.GateID, result.GateStatus)
			if result.ReceiptID != "" {
				fmt.Printf("- receipt: %s\n", result.ReceiptID)
			}
			if result.TransitionID != "" {
				fmt.Printf("- transition: %s\n", result.TransitionID)
			}
		}
		if sourceObserveEnabled {
			sourceReport, err := runProjectionSourceObservation(report, now)
			if err != nil {
				fmt.Fprintln(os.Stderr, "projection doctor source-observe:", err)
				os.Exit(1)
			}
			fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
			for _, id := range sourceReport.CandidateIDs {
				fmt.Printf("- source_candidate: %s\n", id)
			}
			for _, id := range sourceReport.NoPromoteIDs {
				fmt.Printf("- source_no_promote: %s\n", id)
			}
		}
		if writebackEnabled || sourceObserveEnabled || recordContextEnabled {
			return
		}
		os.Exit(1)
	}
}

func cmdProjection(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, projectionUsage())
		os.Exit(2)
	}
	if args[0] == "repair-plan" {
		cmdProjectionRepairPlan(args[1:])
		return
	}
	if args[0] == "repair-review" {
		cmdProjectionRepairReview(args[1:])
		return
	}
	if args[0] == "dashboard-read-model" {
		cmdProjectionDashboardReadModel(args[1:])
		return
	}
	if args[0] != "doctor" {
		fmt.Fprintln(os.Stderr, projectionUsage())
		os.Exit(2)
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: compound projection doctor <freshness.yaml> [--writeback] [--source-observe] [--record-context --repo-kind K]")
		os.Exit(2)
	}
	now := time.Now()
	writebackEnabled := hasFlag(args[2:], "--writeback")
	sourceObserveEnabled := hasFlag(args[2:], "--source-observe")
	recordContextEnabled := hasFlag(args[2:], "--record-context")
	contextRepoKind := valueFlag(args[2:], "--repo-kind")
	report, err := projection.DoctorFile(args[1], now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection doctor:", err)
		os.Exit(1)
	}
	fmt.Printf("# projection doctor\n\n")
	fmt.Printf("- projection: %s\n", report.ProjectionID)
	fmt.Printf("- view: %s\n", report.ViewRef)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- source_ledger_seq: %d\n", report.SourceLedgerSeq)
	if report.Staleness != "" {
		fmt.Printf("- staleness: %s\n", report.Staleness)
	}
	fmt.Printf("- output_hash: %s\n", report.OutputHash)
	for _, source := range report.Sources {
		fmt.Printf("- source: %s %s", source.SourceRef, source.Status)
		if source.CurrentHash != "" {
			fmt.Printf(" (%s)", source.CurrentHash)
		}
		fmt.Println()
	}
	for _, check := range report.Checks {
		fmt.Printf("- check:%s %s", check.Name, check.Status)
		if check.Detail != "" {
			fmt.Printf(" (%s)", check.Detail)
		}
		fmt.Println()
	}
	if recordContextEnabled {
		record, path, err := runProjectionContextRecord(report, contextRepoKind, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, "projection context record:", err)
			os.Exit(1)
		}
		fmt.Printf("- context_record: %s\n", record.ID)
		fmt.Printf("- context_repo_kinds: %s\n", strings.Join(record.RepoKinds, ","))
		fmt.Printf("- context_path: %s\n", path)
	}
	if report.Status != projection.StatusFresh {
		if writebackEnabled {
			result, err := runProjectionWriteback(report, now)
			if err != nil {
				fmt.Fprintln(os.Stderr, "projection writeback:", err)
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
		if sourceObserveEnabled {
			sourceReport, err := runProjectionSourceObservation(report, now)
			if err != nil {
				fmt.Fprintln(os.Stderr, "projection source observe:", err)
				os.Exit(1)
			}
			fmt.Printf("- source_observation: %s\n", sourceReport.ObservationID)
			for _, id := range sourceReport.CandidateIDs {
				fmt.Printf("- source_candidate: %s\n", id)
			}
			for _, id := range sourceReport.NoPromoteIDs {
				fmt.Printf("- source_no_promote: %s\n", id)
			}
		}
		if writebackEnabled || sourceObserveEnabled || recordContextEnabled {
			return
		}
		os.Exit(1)
	}
}
