package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"compound/internal/doctor/exitcode"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/hook"
	"compound/internal/mem"
	"compound/internal/routeev"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func parseWorkDeliveryRequest(args []string) (*work.ActionDeliveryRequest, error) {
	front := args
	if separator := indexOf(args, "--"); separator >= 0 {
		front = args[:separator]
	}
	flags := []string{"--delivery-action", "--delivery-kind", "--delivery-evidence"}
	seen := 0
	for _, flag := range flags {
		if hasFlag(front, flag) {
			seen++
		}
	}
	if seen == 0 {
		return nil, nil
	}
	if seen != len(flags) {
		return nil, fmt.Errorf("delivery binding requires --delivery-action, --delivery-kind and --delivery-evidence together")
	}
	for _, flag := range flags {
		if strings.TrimSpace(valueFlag(front, flag)) == "" {
			return nil, fmt.Errorf("%s requires a nonempty value", flag)
		}
	}
	for _, flag := range []string{"--execute", "--resume-verifying", "--keep-verifying-on-failure", "--lane", "--launch-profile", "--effort", "--expected-selection-revision", "--expected-execution-tuple", "--expected-work-revision", "--target-worktree-root", "--target-local-ref", "--expected-base-sha", "--publication-ref", "--local-only", "--run-attempt", "--artifact", "--verification", "--context-pack", "--harness", "--prompt", "--workspace", "--permission-mode", "--timeout", "--"} {
		if hasFlag(args, flag) {
			return nil, fmt.Errorf("delivery binding cannot be combined with %s", flag)
		}
	}
	return &work.ActionDeliveryRequest{ActionIntentID: valueFlag(args, flags[0]), Kind: valueFlag(args, flags[1]), EvidenceRef: valueFlag(args, flags[2])}, nil
}

type workShowOutput struct {
	envelope.WorkEnvelope
	PacketPath   string       `json:"packet_path,omitempty"`
	PacketStatus string       `json:"packet_status,omitempty"`
	Lifecycle    []work.Event `json:"lifecycle"`
}

// packetEnrichmentStatus renders a packetPathForWork failure as an explicit,
// non-fatal enrichment note. Packet content is enrichment for `show`, not a
// load-bearing dependency: the envelope itself always renders, and a dangling
// packet pointer (or unconfigured workbench packet dir) becomes a visible
// packet_status note instead of a hard failure, so the operator still learns
// the pointer is dangling without losing the rest of the envelope.
func packetEnrichmentStatus(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("unavailable (%s)", err.Error())
}

func packetPathForWork(w envelope.WorkEnvelope) (string, error) {
	surface := strings.TrimSpace(w.CanonicalSurface)
	if !strings.HasPrefix(surface, "packet:") {
		return "", nil
	}
	packetDir := defaultWorkbenchPacketDir()
	if packetDir == "" {
		return "", fmt.Errorf("packet path unavailable for %s: workbench packet directory is not configured", w.ID)
	}
	offered, err := work.OfferedPackets(packetDir)
	if err != nil {
		return "", fmt.Errorf("resolve workbench packet directory: %w", err)
	}
	digest := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(w.Authority.SourceRevision), "sha256:"))
	matches := make([]work.OfferedPacketItem, 0, 1)
	for _, item := range offered.Items {
		if item.CanonicalSurface != surface {
			continue
		}
		if digest != "" && item.SourceDigest != digest {
			continue
		}
		matches = append(matches, item)
	}
	switch len(matches) {
	case 1:
		path, err := filepath.Abs(matches[0].Path)
		if err != nil {
			return "", fmt.Errorf("resolve current packet path for %s: %w", w.ID, err)
		}
		return path, nil
	case 0:
		return "", fmt.Errorf("packet path unavailable for %s: no current workbench packet matches %s", w.ID, surface)
	default:
		return "", fmt.Errorf("packet path ambiguous for %s: %d current workbench packets match %s", w.ID, len(matches), surface)
	}
}

type workVerbHelp struct {
	Usage   string
	Example string
}

const genericWorkUsage = "usage: compound work <create-plan|admit-plan|review-card-plan|create|import-packet|show|list|leases|active-packet-proof|proof-chain-doctor|reconcile-run-attempts|run-attempt-link|continue|dispatch-plan|dispatch-claim|dispatch-execute|dispatch-advance|dispatch-close-plan|dispatch-close|dispatch-step|claim-plan|claim|delivery-capture|advance-plan|advance|delegate|steer|attach|collaborate|observe-run|land-check|proof-review|proof-manifest|proof-adopt|salvage-plan|private-source-publish|action-plan|act|close-plan|close|set-aside|sweep-stale|reseal> ..."

var workVerbHelpByName = map[string]workVerbHelp{
	"proof-review":   {Usage: work.ProofAdoptionUsage, Example: "compound work proof-review work_example --actor executor --file review.md"},
	"proof-manifest": {Usage: work.ProofAdoptionUsage, Example: "compound work proof-manifest work_example --actor executor --file manifest.json"},
	"proof-adopt":    {Usage: work.ProofAdoptionUsage, Example: "compound work proof-adopt work_example --actor executor --proof-result pfrs_example --manifest artifact:sha256:DIGEST"},
	"private-source-publish": {
		Usage:   work.PrivateSourcePublishUsage,
		Example: "compound work private-source-publish --work work_example --actor codex:example --repo /clean/source --review-action act_example --json",
	},
	"delivery-capture": {
		Usage:   "usage: compound work delivery-capture <work_id> --request <file> --by <actor> [--json]",
		Example: "compound work delivery-capture work_example --request capture.json --by operator:example --json",
	},
	"create-plan": {
		Usage:   "usage: compound work create-plan [--work-id ID] --owner O --predicate P [--outcome O] [--by A] [--creator-harness H] [--creator-model M] [--creator-lane L] [--state proposed|ready] [--context-pack ctx] [--canonical-surface S] [--route-decision-id R] [--risk tier1] [--privacy private] [--target-worktree-root /abs/repo --target-local-ref refs/heads/main --expected-base-sha SHA (--publication-ref refs/remotes/origin/main|--local-only)] [--predicted-outcome completed|failed|cancelled --predicted-move-type type --prediction-note N] [--deviation-lane L [--deviation-class C] [--deviation-card REF] [--deviation-risk gate|judge|tidy]] [--plan-class oneshot|plan_doc|phased] [--rework-of work_id] [--json] [--dry-run]",
		Example: "compound work create-plan --owner codex --predicate \"tests pass\" --dry-run",
	},
	"admit-plan": {
		Usage:   "usage: compound work admit-plan --plan <create-plan.json> [--by A] [--json]",
		Example: "compound work admit-plan --plan create-plans/wcp_123.json --by codex",
	},
	"review-card-plan": {
		Usage:   "usage: compound work review-card-plan --card <workbench_review_card_id> [--actor A] [--json]",
		Example: "compound work review-card-plan --card wrc_abc123 --json",
	},
	"create": {
		Usage:   "usage: compound work create [--work-id ID] --owner O --predicate P [--outcome O] [--by A] [--creator-harness H] [--creator-model M] [--creator-lane L] [--state proposed|ready] [--context-pack ctx] [--canonical-surface S] [--route-decision-id R] [--risk tier1] [--privacy private] [--target-worktree-root /abs/repo --target-local-ref refs/heads/main --expected-base-sha SHA (--publication-ref refs/remotes/origin/main|--local-only)] [--predicted-outcome completed|failed|cancelled --predicted-move-type type --prediction-note N] [--deviation-lane L [--deviation-class C] [--deviation-card REF] [--deviation-risk gate|judge|tidy]] [--plan-class oneshot|plan_doc|phased] [--rework-of work_id] [--launch-profile ID [--effort LEVEL]]",
		Example: "compound work create --owner codex --predicate tests --launch-profile claude-minimax",
	},
	"import-packet": {
		Usage:   "usage: compound work import-packet <packet.md> [--owner O] [--by A] [--state proposed|ready] [--context-pack ctx] [--risk tier1] [--privacy private]",
		Example: "compound work import-packet docs_private/plan/packets/example.md --owner codex",
	},
	"show": {
		Usage:   "usage: compound work show <work_id> [--json]",
		Example: "compound work show work_abc123 --json",
	},
	"list": {
		Usage:   "usage: compound work list [--json]",
		Example: "compound work list --json",
	},
	"leases": {
		Usage:   "usage: compound work leases [--json]",
		Example: "compound work leases --json",
	},
	"active-packet-proof": {
		Usage:   "usage: compound work active-packet-proof [--surface canonical_surface] [--source-packet packet.md] [--json]",
		Example: "compound work active-packet-proof --json",
	},
	"proof-chain-doctor": {
		Usage:   "usage: compound work proof-chain-doctor [--include-archived] [--json]",
		Example: "compound work proof-chain-doctor --json",
	},
	"reconcile-run-attempts": {
		Usage:   "usage: compound work reconcile-run-attempts <work_id> [--json]",
		Example: "compound work reconcile-run-attempts work_abc123 --json",
	},
	"run-attempt-link": {
		Usage:   "usage: compound work run-attempt-link --attempt ratt_X --predecessor ratt_Y [--json]",
		Example: "compound work run-attempt-link --attempt ratt_20260823T120500Z_real --predecessor ratt_20260823T120000Z_abandoned",
	},
	"continue": {
		Usage:   "usage: compound work continue [--actor A] [--expect-work work_id] [--selected [--resume|--verification REF]] [--outcome completed|failed|cancelled] [--move-type type] [--summary S] [--until-wait] [--max-steps N] [--max-execute-steps N] [--dry-run] [--json]",
		Example: "compound work continue --actor codex --until-wait --max-execute-steps 1 --dry-run --json",
	},
	"dispatch-plan": {
		Usage:   "usage: compound work dispatch-plan [--actor A] [--capability-class classify-extract|draft-prose] [--source-packet packet.md] [--source-state proposed|ready] [--source-owner O] [--source-context-pack ctx] [--source-risk tier1] [--source-privacy private] [--json] [--dry-run]",
		Example: "compound work dispatch-plan --actor codex --dry-run --json",
	},
	"dispatch-claim": {
		Usage:   "usage: compound work dispatch-claim [--next|<work_id>] [--confirm] [--actor A] [--source-packet packet.md] [--source-state proposed|ready] [--source-owner O] [--source-context-pack ctx] [--source-risk tier1] [--source-privacy private] [--json]\n  without --confirm previews; with --confirm claims",
		Example: "compound work dispatch-claim --next --actor codex --json",
	},
	"dispatch-execute": {
		Usage:   "usage: compound work dispatch-execute <work_id> [--workspace PATH] [--permission-mode MODE] [--privacy-sensitive] [--confirm] [--json]\n  without --confirm previews the Compound-selected harness/model/lane and mutates nothing",
		Example: "compound work dispatch-execute work_abc123 --workspace /path/to/project --json",
	},
	"dispatch-advance": {
		Usage:   "usage: compound work dispatch-advance (--next|<work_id>) [--actor A] [--json]",
		Example: "compound work dispatch-advance --next --actor codex --json",
	},
	"dispatch-close-plan": {
		Usage:   "usage: compound work dispatch-close-plan (--next|<work_id>) [--actor A] [--outcome completed|failed|cancelled] [--move-type type] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--json]",
		Example: "compound work dispatch-close-plan --next --outcome completed --move-type outward_delta --json",
	},
	"dispatch-close": {
		Usage:   "usage: compound work dispatch-close (--next|<work_id>) [--actor A] [--outcome completed|failed|cancelled] [--move-type type] [--summary S] [--json]",
		Example: "compound work dispatch-close --next --outcome completed --move-type outward_delta",
	},
	"dispatch-step": {
		Usage:   "usage: compound work dispatch-step [--actor A] [--source-packet packet.md] [--source-state proposed|ready] [--source-owner O] [--source-context-pack ctx] [--source-risk tier1] [--source-privacy private] [--outcome completed|failed|cancelled] [--move-type type] [--summary S] [--until-wait] [--max-steps N] [--json]",
		Example: "compound work dispatch-step --actor codex --until-wait --json",
	},
	"claim-plan": {
		Usage:   "usage: compound work claim-plan <work_id> [--actor A] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--json] [--dry-run]",
		Example: "compound work claim-plan work_abc123 --actor codex --dry-run",
	},
	"claim": {
		Usage:   "usage: compound work claim <work_id> --by A [--lane <agent-lane node id | short lane id>]",
		Example: "compound work claim work_abc123 --by codex --lane auto/judge",
	},
	"advance-plan": {
		Usage:   "usage: compound work advance-plan <work_id> --to <state> [--actor A] [--steal --reason R] [--target-worktree-root /abs/repo --target-local-ref refs/heads/main --expected-base-sha SHA (--publication-ref refs/remotes/origin/main|--local-only)] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--json] [--dry-run] [--delivery-action ACT --delivery-kind private_runtime|worktree_retirement|installed_package --delivery-evidence REF]",
		Example: "compound work advance-plan work_abc123 --to running --actor codex --dry-run",
	},
	"advance": {
		Usage:   "usage: compound work advance <work_id> --to <state> [--by A|--actor A] [--lane <agent-lane node id | short lane id>] [--run-attempt RATT_ID] (repeatable) [--artifact A] [--verification V] [--context-pack ctx] [--note N] [--acknowledge-abandoned ratt_X --reason R] [--steal --reason R] [--target-worktree-root /abs/repo --target-local-ref refs/heads/main --expected-base-sha SHA (--publication-ref refs/remotes/origin/main|--local-only)] [--launch-profile ID [--effort LEVEL] [--expected-selection-revision N]] [--expected-execution-tuple DIGEST] [--execute [--resume-verifying] --harness <name> [--permission-mode MODE] [--timeout DUR (default 5m, max 45m)] [--prompt <text> | -- <command> [args...]]] [--delivery-action ACT --delivery-kind private_runtime|worktree_retirement|installed_package --delivery-evidence REF] [--expected-work-state S --expected-policy-decision POL --expected-action-intent ACT]",
		Example: "compound work advance work_abc123 --to running --by codex --lane auto/judge",
	},
	"delegate": {
		Usage:   "usage: compound work delegate <work_id> --harness <claude-zai|claude-qwen|claude-minimax|claude|codex> [--execution-profile ID [--launch-profile ID] [--role ROLE] [--capability-class CLASS] [--client-harness ID] [--transport ID] [--route-alias ALIAS] [--model MODEL] [--semantic-effort LEVEL] [--effort LEVEL] [--orchestration POLICY] [--privacy-ceiling LEVEL]] [--actor A] [--prompt <text> | -- <command> [args...]] [--workspace DIR] [--session NAME] [--expected-minutes N] [--confirm] [--experimental-delegate-mutations] [--dry-run] [--json]\n  mutation is experimental and unsupported in v0.1; dry-run/status/attach remain read-only\n  OR:  compound work delegate status <work_id> [--json]\n  OR:  compound work delegate progress <work_id> --done N --empty N --failed N --remaining N --experimental-delegate-mutations [--json]\n  OR:  compound work delegate sweep <work_id> [--experimental-delegate-mutations | --dry-run] [--json]",
		Example: "compound work delegate work_abc123 --harness claude-zai --experimental-delegate-mutations --prompt \"follow the surprising thread\"",
	},
	"steer": {
		Usage:   "usage: compound work steer <work_id> --text <instruction> --experimental-delegate-mutations [--actor A] [--enter] [--json] (experimental; unsupported in v0.1)",
		Example: "compound work steer work_abc123 --experimental-delegate-mutations --text \"look again at the boundary\" --enter",
	},
	"attach": {
		Usage:   "usage: compound work attach <work_id>",
		Example: "compound work attach work_abc123",
	},
	"collaborate": {
		Usage:   "usage: compound work collaborate <work_id> --actor A --idempotency-key K --body-ref artifact:sha256:<digest> [--session S] [--run-attempt RATT_ID] [--target-actor A] [--causal-parent WCO_ID]... [--visibility work] [--delivery-class silent_state|next_boundary|interrupt|human_attention] [--delta-kind K --delta-subject REF --delta-before V --delta-after V] [--affects-actor A]... [--affects-work W]... [--affects-run-attempt RATT_ID]... [--scope-ref REF]... [--dependency-ref REF]... [--policy-ref REF]... [--base-revision REF]... [--resource-ref REF]... [--json]\n  OR: compound work collaborate body <work_id> --event WCO_ID --actor A --run-attempt RATT_ID --session S [--json]",
		Example: "compound work collaborate work_abc123 --actor codex:session --idempotency-key turn-1 --body-ref artifact:sha256:<digest> --delivery-class next_boundary --delta-kind work --delta-subject work_abc123 --delta-after changed --affects-work work_abc123 --json",
	},
	"observe-run": {
		Usage:   "usage: compound work observe-run <work_id> --trace <trace_id> [--job-file <path>] [--actor A] [--note N] [--json]",
		Example: "compound work observe-run work_abc123 --trace trace_xyz --job-file <codex-job.json> --json",
	},
	"land-check": {
		Usage:   "usage: compound work land-check <work_id> --tip <full-sha> [--repo /bound/repo] [--target-ref refs/heads/main] [--actor A] [--steal --reason R] [--dry-run] [--json]",
		Example: "compound work land-check work_abc123 --tip 0123456789012345678901234567890123456789 --json",
	},
	"salvage-plan": {
		Usage:   "usage: compound work salvage-plan <work_id> --branch refs/heads/<branch> [--json]",
		Example: "compound work salvage-plan work_abc123 --branch refs/heads/stale-executor-branch --json",
	},
	"action-plan": {
		Usage:   "usage: compound work action-plan <work_id> [--actor A] [--move-type type] [--receipt-outcome completed|failed|blocked|no_op] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--json] [--dry-run]",
		Example: "compound work action-plan work_abc123 --dry-run --json",
	},
	"act": {
		Usage:   "usage: compound work act <work_id> (--intent <action-intent.json> | --plan <action-execution-plan.json>)",
		Example: "compound work act work_abc123 --plan action-plans/aep_123.json",
	},
	"close-plan": {
		Usage:   "usage: compound work close-plan <work_id> (--action-plan <action-execution-plan.json> | --outcome completed|failed|cancelled --move-type <type>) [--actor A] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--steal --reason R] [--json] [--dry-run]",
		Example: "compound work close-plan work_abc123 --outcome completed --move-type outward_delta --dry-run",
	},
	"close": {
		Usage:   "usage: compound work close <work_id> --outcome completed|failed|cancelled --move-type <type> [--summary S] [--changed] [--by A|--actor A] [--proof REF[,REF]...] [--proof-ref REF] [--used C] [--missed C] [--false-triggered C] [--failure-surface S] [--failure-mechanism M] [--memory-candidate C] [--memory-promoted C] [--memory-parked C] [--memory-rejected C] [--acknowledge-abandoned ratt_X --reason R]... [--verification-rejected ratt_X --reason R]... [--steal --reason R]",
		Example: "compound work close work_abc123 --outcome completed --move-type outward_delta --summary \"shipped\" --changed",
	},
	"set-aside": {
		Usage:   "usage: compound work set-aside <begin|end> <work_id> ...\n  begin <work_id> --candidate <full-sha> --file <path> [--file ...] --by <actor> --reason \"<why the dirt is foreign>\" [--json]\n  end <work_id> [--by <actor>] [--json]",
		Example: "compound work set-aside begin work_abc123 --candidate 0123456789012345678901234567890123456789 --file internal/foo.go --by codex --reason \"neighbor lane WIP\"",
	},
	"sweep-stale": {
		Usage:   "usage: compound work sweep-stale [--threshold 72h] [--dry-run | --yes --experimental-sweep-stale-mutations] [--json]\n  default is dry-run (prints candidate set + reasons, exits 0).\n  --yes mutation is experimental, unsupported in v0.1, and requires --experimental-sweep-stale-mutations.\n  Never touches run_attempts (compound run-attempt sweep owns that).",
		Example: "compound work sweep-stale --dry-run --threshold 48h",
	},
	"reseal": {
		// Same dry-run-default law as sweep-stale: the verb exists to audit
		// the custody-mismatch set, and --yes is the only mutation gate. The
		// repair itself is append-only (one sealed same-state lifecycle
		// event); envelope bytes and history are never rewritten.
		Usage:   "usage: compound work reseal [--dry-run|--yes] [--by <actor>] [--json]\n  default is dry-run (lists work id, stored seal, computed seal; exits 0).\n  --yes appends one sealed same-state lifecycle event per mismatching\n  terminal envelope so the autonomy director's custody check verifies again.\n  Never rewrites envelope bytes or history; non-terminal envelopes are skipped.",
		Example: "compound work reseal --yes --by orchestrator:claude_zai",
	},
}

func workUsage(sub string) string {
	if help, ok := workVerbHelpByName[sub]; ok {
		return help.Usage
	}
	return genericWorkUsage
}

func workHelpText(sub string) string {
	help, ok := workVerbHelpByName[sub]
	if !ok {
		return genericWorkUsage
	}
	return help.Usage + "\nexample: " + help.Example
}

func previewDispatchClaim(store *work.Store, now time.Time, opts work.DispatchPlanOptions) (work.DispatchPlan, error) {
	opts.Record = false
	opts.Confirm = false
	opts.ReadOnly = true
	return store.DispatchPlan(now, opts)
}

// workCommandNeedsWritableStore classifies commands before Store construction.
// The command owner parses any mixed read/write surface so invalid and read-only
// requests cannot materialize the work habitat.
func workCommandNeedsWritableStore(sub string, args []string) bool {
	if sub == "proof-manifest" || sub == "proof-adopt" || sub == "proof-review" {
		return work.ProofAdoptionCLIRequiresWritableStore(sub, args)
	}
	if sub == "private-source-publish" {
		return work.PrivateSourcePublishCLIRequiresWritableStore(args)
	}
	if sub == "sweep-stale" {
		return work.SweepStaleCLIRequiresWritableStore(args)
	}
	return delegateCommandNeedsWritableStore(sub, args)
}

func cmdWork(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, workUsage(""))
		os.Exit(2)
	}
	if hasUsageFlag(args) {
		fmt.Println(workHelpText(args[0]))
		return
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	sub, rest := args[0], args[1:]
	openStore := work.OpenReadOnly
	if workCommandNeedsWritableStore(sub, rest) {
		openStore = work.Open
	}
	store, err := openStore(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
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
	// pos returns the first positional argument in rest, skipping any token that is the VALUE
	// of a registered value-flag. The flag-blind idiom this replaces returned the first
	// non-dash token unconditionally, so e.g. `work show --home /tmp/x work_123` (or any other
	// value-flag whose value doesn't start with "-") silently fed the flag's value to the
	// command as if it were the positional argument (find_52e26e530829). Callers pass the same
	// value-flags map they use to validate that subcommand's flags, so extraction and
	// validation share one definition of "what is a flag here".
	pos := func(valueFlags map[string]bool) string {
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if valueFlags[a] {
				i++
				continue
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			return a
		}
		return ""
	}
	parseExplicitDispatchTarget := func(usage string, valueFlags, boolFlags map[string]bool) string {
		targetWorkID := ""
		next := false
		for i := 0; i < len(rest); i++ {
			arg := rest[i]
			switch {
			case arg == "--next":
				next = true
			case boolFlags[arg]:
			case valueFlags[arg]:
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				if strings.HasPrefix(arg, "-") {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				if targetWorkID != "" {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				targetWorkID = arg
			}
		}
		if targetWorkID == "" && !next {
			fmt.Fprintln(os.Stderr, "error: mutating dispatch requires --next or <work_id>")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if targetWorkID != "" && next {
			fmt.Fprintln(os.Stderr, "error: pass either --next or <work_id>, not both")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		return targetWorkID
	}
	parseDispatchClaimTarget := func(usage string) (targetWorkID string, confirmed bool) {
		valueFlags := map[string]bool{
			"--actor": true, "--source-packet": true, "--source-state": true,
			"--source-owner": true, "--source-context-pack": true,
			"--source-risk": true, "--source-privacy": true,
		}
		next := false
		for i := 0; i < len(rest); i++ {
			arg := rest[i]
			switch {
			case arg == "--next":
				next = true
			case arg == "--confirm":
				confirmed = true
			case arg == "--json":
			case valueFlags[arg]:
				if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				if strings.HasPrefix(arg, "-") || targetWorkID != "" {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				targetWorkID = arg
			}
		}
		if targetWorkID != "" && next {
			fmt.Fprintln(os.Stderr, "error: pass either --next or <work_id>, not both")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		return targetWorkID, confirmed
	}
	validateCreateState := func(usage string, allowTestRunning bool) {
		state := strings.TrimSpace(flag("--state"))
		if state == "" || state == "proposed" || state == "ready" {
			return
		}
		if state == "running" && allowTestRunning {
			manifest, manifestErr := home.LoadManifest(homePath)
			if manifestErr == nil && manifest.HomeKind == "test" {
				return
			}
			fmt.Fprintln(os.Stderr, "error: create at ready -> claim -> advance")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "error: initial work state must be proposed or ready; use lifecycle commands for later states")
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	die := func(e error) {
		if e != nil {
			fmt.Fprintln(os.Stderr, "error:", e)
			os.Exit(1)
		}
	}
	parseGitDelivery := func(usage string) *envelope.GitDeliveryContract {
		root := strings.TrimSpace(flag("--target-worktree-root"))
		localRef := strings.TrimSpace(flag("--target-local-ref"))
		base := strings.TrimSpace(flag("--expected-base-sha"))
		publicationRef := strings.TrimSpace(flag("--publication-ref"))
		localOnly := has("--local-only")
		declared := root != "" || localRef != "" || base != "" || publicationRef != "" || localOnly
		if !declared {
			return nil
		}
		if root == "" || localRef == "" || base == "" || (publicationRef == "" && !localOnly) || (publicationRef != "" && localOnly) {
			fmt.Fprintln(os.Stderr, "error: Git delivery requires --target-worktree-root, --target-local-ref, --expected-base-sha, and exactly one of --publication-ref or --local-only")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		mode := "required"
		if localOnly {
			mode = "local_only"
		}
		return &envelope.GitDeliveryContract{
			TargetWorktreeRoot: root,
			TargetLocalRef:     localRef,
			ExpectedBaseSHA:    base,
			PublicationMode:    mode,
			PublicationRef:     publicationRef,
		}
	}
	// parsePrediction is the grouped-optional-flags precedent for staking a WorkPrediction at
	// create time (mirrors parseGitDelivery): any of the three prediction flags declares intent
	// to stake, and staking without --predicted-outcome is a usage error, since Outcome is the
	// one required dimension (envelope.WorkEnvelope.Validate enforces the same rule for a
	// hand-written envelope). MoveType/Note are optional and enum/free-text validated downstream
	// by WorkEnvelope.Validate.
	parsePrediction := func(usage string) *envelope.WorkPrediction {
		outcome := strings.TrimSpace(flag("--predicted-outcome"))
		moveType := strings.TrimSpace(flag("--predicted-move-type"))
		note := strings.TrimSpace(flag("--prediction-note"))
		if outcome == "" && moveType == "" && note == "" {
			return nil
		}
		if outcome == "" {
			fmt.Fprintln(os.Stderr, "error: staking a prediction requires --predicted-outcome")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		return &envelope.WorkPrediction{Outcome: outcome, MoveType: moveType, Note: note}
	}

	// parseRouteDeviation mirrors parsePrediction: any of the deviation flags declares intent to
	// mark this work as an explicit route deviation, and --deviation-lane is then required (it is
	// the priced object). --deviation-card is advisory and never gates admission (a missing card is
	// not a bypass). --deviation-risk is left as stated (empty allowed): the fail-closed default of
	// judge for an unstated grade lives in ONE place, envelope.effectiveRouteDeviationGrade, so a
	// hand-written envelope and this CLI path get the identical rule. The judge-grade + no-prediction
	// refusal is enforced downstream by envelope.WorkEnvelope.Validate.
	parseRouteDeviation := func(usage string) *envelope.RouteDeviation {
		lane := strings.TrimSpace(flag("--deviation-lane"))
		card := strings.TrimSpace(flag("--deviation-card"))
		class := strings.TrimSpace(flag("--deviation-class"))
		risk := strings.TrimSpace(flag("--deviation-risk"))
		if lane == "" && card == "" && class == "" && risk == "" {
			return nil
		}
		if lane == "" {
			fmt.Fprintln(os.Stderr, "error: declaring a route deviation requires --deviation-lane")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		return &envelope.RouteDeviation{Lane: lane, CapabilityClass: class, CardRef: card, RiskGrade: risk}
	}
	createInputFromFlags := func(base work.CreateInput) work.CreateInput {
		base.WorkID = flag("--work-id")
		base.Owner = flag("--owner")
		base.CanonicalSurface = flag("--canonical-surface")
		base.CreatorHarness = firstNonEmpty(flag("--creator-harness"), os.Getenv("COMPOUND_HARNESS"))
		base.CreatorModel = firstNonEmpty(flag("--creator-model"), os.Getenv("COMPOUND_MODEL"))
		base.CreatorLaneID = firstNonEmpty(flag("--creator-lane"), os.Getenv("COMPOUND_LANE_ID"))
		base.SuccessPredicate = flag("--predicate")
		base.DesiredOutcome = flag("--outcome")
		base.NonGoals = flagCSVValues(rest, "--non-goal", "--non-goals")
		base.EvidenceRefs = flagCSVValues(rest, "--evidence-ref", "--evidence")
		base.ContextPackID = flag("--context-pack")
		base.RouteDecisionID = flag("--route-decision-id")
		base.Risk = flag("--risk")
		base.Privacy = flag("--privacy")
		base.PlanClass = flag("--plan-class")
		base.ReworkOfWorkID = flag("--rework-of")
		return base
	}

	switch sub {
	case "proof-manifest", "proof-adopt", "proof-review":
		if code := work.RunProofAdoptionCLI(store, sub, rest, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
	case "private-source-publish":
		if code := work.RunPrivateSourcePublishCLI(store, rest, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
	case "delegate", "steer", "attach":
		// Capture pane output. Without it a delegate's entire record dies
		// with its tmux pane, which is how a 55-session burst on
		// 2026-07-26 left zero artifacts and zero diagnosable cause.
		runner := work.NewTmuxRunnerWithLogDir(filepath.Join(homePath, "observed", "delegate-logs"))
		code := runWorkDelegateCLI(sub, rest, store, runner, productionDelegateAttach, time.Now(), os.Stdout, os.Stderr)
		if code != 0 {
			os.Exit(code)
		}
	case "create-plan":
		usage := workUsage("create-plan")
		allowed := map[string]bool{"--work-id": true, "--owner": true, "--predicate": true, "--outcome": true, "--by": true, "--actor": true, "--state": true, "--context-pack": true, "--canonical-surface": true, "--route-decision-id": true, "--risk": true, "--privacy": true, "--non-goal": true, "--non-goals": true, "--evidence-ref": true, "--evidence": true, "--capability": true, "--action": true, "--mutation-class": true, "--reversibility": true, "--blast-radius": true, "--target-kind": true, "--target-id": true, "--target-path": true, "--target-worktree-root": true, "--target-local-ref": true, "--expected-base-sha": true, "--publication-ref": true, "--predicted-outcome": true, "--predicted-move-type": true, "--prediction-note": true, "--deviation-lane": true, "--deviation-card": true, "--deviation-class": true, "--deviation-risk": true, "--plan-class": true, "--rework-of": true, "--creator-harness": true, "--creator-model": true, "--creator-lane": true, "--launch-profile": true, "--effort": true}
		if err := validateWorkArguments(rest, allowed, map[string]bool{"--json": true, "--dry-run": true, "--local-only": true}, 0); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		validateCreateState(usage, false)
		gitDelivery := parseGitDelivery(usage)
		prediction := parsePrediction(usage)
		routeDeviation := parseRouteDeviation(usage)
		selActor := firstNonEmpty(flag("--by"), flag("--actor"))
		executionSelection := selectionFlagsFromCLI(store, usage, selActor, flag("--launch-profile"), flag("--effort"))
		plan, err := store.CreatePlan(createInputFromFlags(work.CreateInput{Actor: selActor, GitDelivery: gitDelivery, State: flag("--state"), Prediction: prediction, RouteDeviation: routeDeviation, ExecutionSelection: executionSelection}), time.Now(), work.CreatePlanOptions{
			Actor:         firstNonEmpty(flag("--by"), flag("--actor")),
			Capability:    flag("--capability"),
			Action:        flag("--action"),
			MutationClass: flag("--mutation-class"),
			Reversibility: flag("--reversibility"),
			BlastRadius:   flag("--blast-radius"),
			TargetKind:    flag("--target-kind"),
			TargetID:      flag("--target-id"),
			TargetPath:    flag("--target-path"),
			Record:        !has("--dry-run"),
		})
		die(err)
		if has("--json") {
			printJSON(plan)
			return
		}
		fmt.Printf("create-plan %s status=%s work=%s policy=%s allowed=%v\n", plan.ID, plan.Status, plan.WorkID, plan.Policy.Outcome, plan.Policy.Allowed)
		if plan.Policy.StandingGrantID != "" {
			fmt.Printf("standing_grant: %s\n", plan.Policy.StandingGrantID)
		}
		if len(plan.CreateCommand) > 0 {
			fmt.Printf("command: %s\n", strings.Join(plan.CreateCommand, " "))
		}
		if plan.NoAction != "" {
			fmt.Printf("no_action: %s\n", plan.NoAction)
		}
		if plan.PlanPath != "" {
			fmt.Printf("plan: %s\n", plan.PlanPath)
		}
	case "admit-plan":
		admitPlanFlags := map[string]bool{"--plan": true, "--by": true, "--actor": true}
		if err := validateKnownFlags(rest, admitPlanFlags, map[string]bool{"--json": true}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, workUsage("admit-plan"))
			os.Exit(2)
		}
		planPath := firstNonEmpty(flag("--plan"), pos(admitPlanFlags))
		if planPath == "" {
			fmt.Fprintln(os.Stderr, workUsage("admit-plan"))
			os.Exit(2)
		}
		report, err := store.AdmitCreatePlanPath(planPath, time.Now(), work.AdmitCreatePlanOptions{
			Actor: firstNonEmpty(flag("--by"), flag("--actor")),
		})
		die(err)
		if has("--json") {
			printJSON(report)
			return
		}
		fmt.Printf("admit-plan %s status=%s work=%s state=%s policy=%s allowed=%v\n", report.PlanID, report.Status, report.WorkID, report.WorkState, report.PolicyOutcome, report.PolicyAllowed)
		if report.NoAction != "" {
			fmt.Printf("no_action: %s\n", report.NoAction)
		}
		if report.PlanPath != "" {
			fmt.Printf("plan: %s\n", report.PlanPath)
		}
	case "review-card-plan":
		reviewCardPlanFlags := map[string]bool{"--card": true, "--actor": true, "--by": true}
		if err := validateKnownFlags(rest, reviewCardPlanFlags, map[string]bool{"--json": true}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, workUsage("review-card-plan"))
			os.Exit(2)
		}
		cardID := firstNonEmpty(flag("--card"), pos(reviewCardPlanFlags))
		if cardID == "" {
			fmt.Fprintln(os.Stderr, workUsage("review-card-plan"))
			os.Exit(2)
		}
		report, err := store.BuildReviewCardWorkPlan(cardID, time.Now(), work.ReviewCardWorkPlanOptions{
			Actor: firstNonEmpty(flag("--actor"), flag("--by")),
		})
		die(err)
		if has("--json") {
			printJSON(report)
			return
		}
		fmt.Printf("review-card-plan %s status=%s work=%s card=%s kind=%s\n", report.WorkPlan.ID, report.Status, report.WorkPlan.WorkID, report.ReviewCard.CardID, report.ReviewCard.CardKind)
		if report.ReviewCard.ConditionStatus != "" {
			fmt.Printf("condition_status: %s evidence=%s\n", report.ReviewCard.ConditionStatus, report.ReviewCard.TriggeringEvidenceRef)
		}
		if report.ReviewCard.NextAction != "" {
			fmt.Printf("next_action: %s\n", report.ReviewCard.NextAction)
		}
		fmt.Printf("action_context: %s\n", strings.Join(report.ReviewCard.ActionCommand, " "))
		if len(report.ReviewCard.RequiredInputs) > 0 {
			fmt.Printf("required_inputs: %s\n", strings.Join(report.ReviewCard.RequiredInputs, " | "))
		}
		if report.WorkPlan.PlanPath != "" {
			fmt.Printf("plan: %s\n", report.WorkPlan.PlanPath)
		}
	case "create":
		usage := workUsage("create")
		if err := validateWorkArguments(rest, map[string]bool{
			"--work-id": true, "--owner": true, "--predicate": true, "--outcome": true, "--by": true, "--actor": true,
			"--state": true, "--context-pack": true, "--canonical-surface": true, "--route-decision-id": true, "--risk": true, "--privacy": true,
			"--non-goal": true, "--non-goals": true, "--evidence-ref": true, "--evidence": true,
			"--target-worktree-root": true, "--target-local-ref": true, "--expected-base-sha": true, "--publication-ref": true,
			"--predicted-outcome": true, "--predicted-move-type": true, "--prediction-note": true,
			"--deviation-lane": true, "--deviation-card": true, "--deviation-class": true, "--deviation-risk": true,
			"--plan-class": true, "--rework-of": true,
			"--creator-harness": true, "--creator-model": true, "--creator-lane": true,
			"--launch-profile": true, "--effort": true,
		}, map[string]bool{"--local-only": true}, 0); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		validateCreateState(usage, true)
		gitDelivery := parseGitDelivery(usage)
		prediction := parsePrediction(usage)
		routeDeviation := parseRouteDeviation(usage)
		requestedState := flag("--state")
		createState := requestedState
		actor := firstNonEmpty(flag("--by"), flag("--actor"))
		if requestedState == "running" {
			createState = "ready"
			actor = firstNonEmpty(actor, "test-fixture")
		}
		executionSelection := selectionFlagsFromCLI(store, usage, actor, flag("--launch-profile"), flag("--effort"))
		now := time.Now()
		w, err := store.Create(createInputFromFlags(work.CreateInput{Actor: actor, GitDelivery: gitDelivery, State: createState, Prediction: prediction, RouteDeviation: routeDeviation, ExecutionSelection: executionSelection}), now)
		die(err)
		if requestedState == "running" {
			w, err = store.Claim(w.ID, actor, now)
			die(err)
			w, err = store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: actor}, now)
			die(err)
		}
		fmt.Printf("created %s (%s)\n", w.ID, w.State)
		printJSON(w)
	case "import-packet":
		importPacketUsage := "usage: compound work import-packet <packet.md> [--owner O] [--by A] [--state proposed|ready] [--context-pack ctx] [--risk tier1] [--privacy private]"
		importPacketFlags := map[string]bool{
			"--owner": true, "--by": true, "--actor": true, "--state": true,
			"--context-pack": true, "--risk": true, "--privacy": true,
		}
		if err := validateKnownFlags(rest, importPacketFlags, nil); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, importPacketUsage)
			os.Exit(2)
		}
		validateCreateState(importPacketUsage, false)
		packetPath := pos(importPacketFlags)
		if packetPath == "" {
			fmt.Fprintln(os.Stderr, importPacketUsage)
			os.Exit(2)
		}
		report, err := store.ImportPacket(packetPath, work.PacketImportOptions{
			Owner:         flag("--owner"),
			Actor:         firstNonEmpty(flag("--by"), flag("--actor")),
			State:         flag("--state"),
			Risk:          flag("--risk"),
			Privacy:       flag("--privacy"),
			ContextPackID: flag("--context-pack"),
		}, time.Now())
		die(err)
		fmt.Printf("imported packet %q -> %s created=%v state=%s\n", report.PacketTitle, report.Work.ID, report.Created, report.Work.State)
		printJSON(report)
	case "show":
		if err := validateKnownFlags(rest, nil, map[string]bool{"--json": true}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, workUsage("show"))
			os.Exit(2)
		}
		w, evs, err := store.Show(pos(nil))
		die(err)
		packetPath, packetErr := packetPathForWork(w)
		packetStatus := packetEnrichmentStatus(packetErr)
		if has("--json") {
			printJSON(workShowOutput{WorkEnvelope: w, PacketPath: packetPath, PacketStatus: packetStatus, Lifecycle: evs})
			return
		}
		printJSON(w)
		// work_r7a_execution_selection_20260911.
		fmt.Print(work.RenderWorkSelection(w, &work.RunAttemptStoreLookup{Store: runattempt.Open(homePath)}))
		if packetPath != "" {
			fmt.Printf("packet_path: %s\n", packetPath)
		}
		if packetStatus != "" {
			fmt.Printf("packet_status: %s\n", packetStatus)
		}
		fmt.Println("--- lifecycle ---")
		for _, e := range evs {
			fmt.Printf("%s -> %s  %s %s\n", emptyDash(e.From), e.To, e.By, e.Note)
		}
	case "list":
		if err := work.ValidateWorkListFlags(rest); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, work.WorkListUsage)
			os.Exit(2)
		}
		ws, err := store.List()
		die(err)
		if has("--json") {
			printJSON(struct {
				SchemaVersion string                  `json:"schema_version"`
				AuthorityRef  string                  `json:"authority_ref"`
				Work          []envelope.WorkEnvelope `json:"work"`
			}{
				SchemaVersion: "compound.work_list_projection.v0.1",
				AuthorityRef:  "compound://work/list",
				Work:          ws,
			})
			return
		}
		for _, w := range ws {
			fmt.Printf("%s  %-10s %s\n", w.ID, w.State, w.SuccessPredicate)
		}
	case "leases":
		usage := "usage: compound work leases [--json]"
		for _, arg := range rest {
			if arg != "--json" {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
		}
		leases, err := store.ListLeases()
		die(err)
		if has("--json") {
			printJSON(struct {
				SchemaVersion string       `json:"schema_version"`
				AuthorityRef  string       `json:"authority_ref"`
				Leases        []work.Lease `json:"leases"`
			}{
				SchemaVersion: "compound.work_leases_projection.v0.1",
				AuthorityRef:  "compound://work/leases",
				Leases:        leases,
			})
			return
		}
		fmt.Printf("# work leases\n\n")
		for _, lease := range leases {
			fmt.Printf("- lease: %s by=%s claimed_at=%s\n", lease.WorkID, lease.By, lease.ClaimAt)
		}
	case "active-packet-proof":
		usage := "usage: compound work active-packet-proof [--surface canonical_surface] [--source-packet packet.md] [--json]"
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--json":
			case "--surface":
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			case "--source-packet":
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
		}
		activePacketSurface := flag("--surface")
		activePacketExpectedRef := ""
		if sourcePacket := flag("--source-packet"); sourcePacket != "" {
			proof, err := work.PacketSourceProofForPath(sourcePacket)
			die(err)
			activePacketExpectedRef = proof.EvidenceRef
			if strings.TrimSpace(activePacketSurface) == "" {
				activePacketSurface = proof.CanonicalSurface
			}
		}
		report, err := store.ActivePacketProof(time.Now(), activePacketSurface, activePacketExpectedRef)
		die(err)
		if has("--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# work active-packet-proof %s\n", report.Status)
		fmt.Printf("- authority: %s\n", report.AuthorityRef)
		fmt.Printf("- surface: %s\n", report.CanonicalSurface)
		if report.WorkID != "" {
			fmt.Printf("- work: %s state=%s\n", report.WorkID, report.State)
		}
		if report.ContextPackID != "" {
			fmt.Printf("- context_pack: %s\n", report.ContextPackID)
		}
		if report.ReceiptID != "" {
			fmt.Printf("- receipt: %s\n", report.ReceiptID)
		}
		if report.TransitionID != "" {
			fmt.Printf("- transition: %s\n", report.TransitionID)
		}
		for _, ref := range report.PacketEvidenceRefs {
			fmt.Printf("- packet_evidence: %s\n", ref)
		}
		for name, ok := range report.Checks {
			fmt.Printf("- check.%s: %v\n", name, ok)
		}
		for _, item := range report.Integrity {
			fmt.Printf("- integrity: %s\n", item)
		}
		fmt.Printf("- next: %s\n", report.NextAction)
	case "proof-chain-doctor":
		usage := "usage: compound work proof-chain-doctor [--include-archived] [--json]"
		for _, arg := range rest {
			switch arg {
			case "--include-archived", "--json":
			default:
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
		}
		report, err := store.ProofChainDoctor(time.Now(), work.ProofChainDoctorOptions{IncludeArchived: has("--include-archived")})
		if err != nil {
			// Exit-code contract: a store failure means the doctor itself
			// could not run (2), not "ran and found problems" (1). die()
			// exits 1 and is shared across verbs, so this branch decides
			// for itself.
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(exitcode.CouldNotRun)
		}
		// Doctor exit-code contract (internal/doctor/exitcode): findings
		// are problems, exit 1 when any exist. This doctor's documented
		// policy divergence: the count line prints even on a clean run —
		// it printed NOTHING before, and a doctor whose clean output is
		// silence cannot be distinguished from one that never ran.
		doc := exitcode.Doctor{Verb: "proof-chain-doctor", Note: "all terminal chains are intact"}
		for _, finding := range report.Findings {
			doc.Problems = append(doc.Problems, fmt.Sprintf("%s %s: %s", finding.WorkID, finding.Code, finding.Message))
		}
		if len(doc.Problems) > 0 {
			doc.Note = "run `compound work show <work_id>` for each finding above"
		}
		if has("--json") {
			printJSON(report)
			if doc.Exit() != exitcode.OK {
				os.Exit(exitcode.ProblemFound)
			}
			return
		}
		fmt.Printf("# work proof-chain-doctor %s\n\n", report.Status)
		fmt.Printf("- works_checked: %d\n", report.Counts.WorksChecked)
		fmt.Printf("- terminal_checked: %d\n", report.Counts.TerminalChecked)
		fmt.Printf("- complete: %d\n", report.Counts.Complete)
		if len(report.Findings) > 0 {
			fmt.Printf("- findings: %d\n", len(report.Findings))
			for _, finding := range report.Findings {
				fmt.Printf("- %s %s: %s\n", finding.WorkID, finding.Code, finding.Message)
			}
		}
		fmt.Println(doc.FinalLine())
		if doc.Exit() != exitcode.OK {
			os.Exit(doc.Exit())
		}
	case "reconcile-run-attempts":
		usage := workUsage("reconcile-run-attempts")
		if err := validateWorkArguments(rest, nil, map[string]bool{"--json": true}, 1); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		workID := pos(nil)
		report, err := store.ReconcileRunAttemptsForTerminalWork(workID, time.Now())
		die(err)
		if has("--json") {
			printJSON(report)
		} else {
			fmt.Printf("work run-attempt reconciliation %s: work=%s reconciled=%d consistent=%d held=%d\n",
				report.Status, report.WorkID, len(report.ReconciledIDs), len(report.AlreadyConsistentIDs), len(report.HeldIDs))
			for _, issue := range report.Issues {
				fmt.Printf("- %s %s: %s\n", issue.RunAttemptID, issue.Code, issue.Message)
			}
		}
		if report.Status != "ok" {
			os.Exit(1)
		}
	case "run-attempt-link":
		runAttemptLinkUsage := "usage: compound work run-attempt-link --attempt ratt_X --predecessor ratt_Y [--json]"
		runAttemptLinkValueFlags := map[string]bool{"--attempt": true, "--predecessor": true}
		if err := validateWorkArguments(rest, runAttemptLinkValueFlags, map[string]bool{"--json": true}, 0); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, runAttemptLinkUsage)
			os.Exit(2)
		}
		attemptID := flag("--attempt")
		predecessorID := flag("--predecessor")
		if attemptID == "" || predecessorID == "" {
			fmt.Fprintln(os.Stderr, "error: --attempt and --predecessor are required")
			fmt.Fprintln(os.Stderr, runAttemptLinkUsage)
			os.Exit(2)
		}
		ra, err := runattempt.Open(homePath).SetPredecessor(attemptID, predecessorID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: run-attempt-link:", err)
			os.Exit(1)
		}
		if has("--json") {
			printJSON(ra)
		} else {
			fmt.Printf("linked %s -> %s (work=%s, status=%s)\n", ra.ID, ra.PredecessorID, ra.WorkID, ra.Status)
		}
	case "continue":
		usage := "usage: compound work continue [--actor A] [--expect-work work_id] [--selected [--resume|--verification REF]] [--outcome completed|failed|cancelled] [--move-type type] [--summary S] [--until-wait] [--max-steps N] [--max-execute-steps N] [--dry-run] [--json]"
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--json", "--until-wait", "--dry-run", "--selected", "--resume":
			case "--actor", "--expect-work", "--outcome", "--move-type", "--summary", "--max-steps", "--max-execute-steps", "--verification":
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
		}
		continueOpts := work.ContinueOptions{
			Selected:        has("--selected"),
			Resume:          has("--resume"),
			VerificationRef: flag("--verification"),
			Actor:           flag("--actor"),
			ExpectWorkID:    flag("--expect-work"),
			Outcome:         flag("--outcome"),
			MoveType:        flag("--move-type"),
			Summary:         flag("--summary"),
			DryRun:          has("--dry-run"),
		}
		maxSteps := 0
		if raw := strings.TrimSpace(flag("--max-steps")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			maxSteps = n
		}
		if maxSteps > 0 && !has("--until-wait") {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if raw := strings.TrimSpace(flag("--max-execute-steps")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			continueOpts.MaxExecuteSteps = n
		}
		if has("--until-wait") {
			result, err := store.ContinueRun(time.Now(), work.ContinueRunOptions{ContinueOptions: continueOpts, MaxSteps: maxSteps})
			die(err)
			if has("--json") {
				printJSON(result)
				return
			}
			fmt.Printf("continue-run %s status=%s steps=%d", result.ID, result.Status, result.StepCount)
			if result.WaitReason != "" {
				fmt.Printf(" wait=%s", result.WaitReason)
			}
			fmt.Println()
			if result.ResultPath != "" {
				fmt.Printf("result: %s\n", result.ResultPath)
			}
			return
		}
		result, err := store.Continue(time.Now(), continueOpts)
		die(err)
		if has("--json") {
			printJSON(result)
			return
		}
		if result.WorkID != "" {
			fmt.Printf("continue %s status=%s action=%s work=%s\n", result.ID, result.Status, result.Action, result.WorkID)
		} else {
			fmt.Printf("continue %s status=%s no_action=%s\n", result.ID, result.Status, result.NoAction)
		}
		if result.ResultPath != "" {
			fmt.Printf("result: %s\n", result.ResultPath)
		}
	case "dispatch-plan":
		usage := "usage: compound work dispatch-plan [--actor A] [--capability-class classify-extract|draft-prose] [--source-packet packet.md preview-only] [--source-state proposed|ready] [--source-owner O] [--source-context-pack ctx] [--source-risk tier1] [--source-privacy private] [--json] [--dry-run]"
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--json", "--dry-run":
			case "--actor", "--capability-class", "--source-packet", "--source-state", "--source-owner", "--source-context-pack", "--source-risk", "--source-privacy":
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
		}
		plan, err := store.DispatchPlan(time.Now(), work.DispatchPlanOptions{
			Actor:                 flag("--actor"),
			CapabilityClass:       flag("--capability-class"),
			Record:                !has("--dry-run"),
			SourcePacketPath:      flag("--source-packet"),
			SourcePacketState:     flag("--source-state"),
			SourcePacketOwner:     flag("--source-owner"),
			SourcePacketRisk:      flag("--source-risk"),
			SourcePacketPrivacy:   flag("--source-privacy"),
			SourcePacketContextID: flag("--source-context-pack"),
		})
		die(err)
		if has("--json") {
			printJSON(plan)
			return
		}
		if plan.Selected != nil {
			fmt.Printf("dispatch-plan %s status=%s selected=%s action=%s\n", plan.ID, plan.Status, plan.Selected.WorkID, plan.Selected.Action)
			if len(plan.Selected.Command) > 0 {
				fmt.Printf("command: %s\n", strings.Join(plan.Selected.Command, " "))
			}
			if plan.Selected.ActionContext != nil && len(plan.Selected.ActionContext.Command) > 0 {
				fmt.Printf("action_context: %s proof=%s\n", strings.Join(plan.Selected.ActionContext.Command, " "), emptyDash(plan.Selected.ActionContext.Proof))
			}
		} else {
			fmt.Printf("dispatch-plan %s status=%s no_action=%s\n", plan.ID, plan.Status, plan.NoAction)
		}
		if plan.LaneSelection != nil {
			sel := plan.LaneSelection
			if sel.Winner != nil {
				fmt.Printf("lane_selection: class=%s winner=%s floor=%.1ft/s status=%s (%s)\n", sel.CapabilityClass, sel.Winner.Lane, sel.FloorTokensPerSecond, sel.Status, sel.Winner.Rationale)
			} else {
				fmt.Printf("lane_selection: class=%s status=%s (no eligible lane)\n", sel.CapabilityClass, sel.Status)
			}
		}
		if plan.PlanPath != "" {
			fmt.Printf("plan: %s\n", plan.PlanPath)
		}
	case "dispatch-claim":
		usage := workUsage("dispatch-claim") + "\n  (omit --confirm to preview only; --next and <work_id> are mutually exclusive)"
		targetWorkID, confirmed := parseDispatchClaimTarget(usage)
		dispatchOpts := work.DispatchPlanOptions{
			Actor:                 flag("--actor"),
			Confirm:               confirmed,
			WorkID:                targetWorkID,
			SourcePacketPath:      flag("--source-packet"),
			SourcePacketState:     flag("--source-state"),
			SourcePacketOwner:     flag("--source-owner"),
			SourcePacketRisk:      flag("--source-risk"),
			SourcePacketPrivacy:   flag("--source-privacy"),
			SourcePacketContextID: flag("--source-context-pack"),
		}
		if !confirmed {
			preview, err := previewDispatchClaim(store, time.Now(), dispatchOpts)
			die(err)
			if has("--json") {
				printJSON(preview)
			} else {
				fmt.Printf("dispatch-claim PREVIEW %s status=%s\n", preview.ID, preview.Status)
				if preview.Selected != nil {
					fmt.Printf("would_claim: %s by=%s\n", preview.Selected.WorkID, preview.Actor)
				}
				if preview.NoAction != "" {
					fmt.Printf("no_action: %s\n", preview.NoAction)
				}
				fmt.Println("nothing claimed; re-run with --confirm to perform the claim")
			}
			os.Exit(1)
		}
		result, err := store.DispatchClaim(time.Now(), dispatchOpts)
		die(err)
		if has("--json") {
			printJSON(result)
			return
		}
		if result.ClaimedWorkID != "" {
			fmt.Printf("dispatch-claim %s status=%s claimed=%s policy=%s\n", result.ID, result.Status, result.ClaimedWorkID, result.PolicyDecisionID)
		} else {
			fmt.Printf("dispatch-claim %s status=%s no_action=%s\n", result.ID, result.Status, result.NoAction)
		}
		if result.DispatchPlanPath != "" {
			fmt.Printf("plan: %s\n", result.DispatchPlanPath)
		}
		if result.ResultPath != "" {
			fmt.Printf("result: %s\n", result.ResultPath)
		}
	case "dispatch-execute":
		usage := workUsage("dispatch-execute")
		valueFlags := map[string]bool{"--workspace": true, "--permission-mode": true}
		if err := validateWorkArguments(rest, valueFlags, map[string]bool{"--privacy-sensitive": true, "--confirm": true, "--json": true}, 1); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		workID := pos(valueFlags)
		if workID == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		executeOpts := workDispatchExecuteOptions{
			Workspace:        flag("--workspace"),
			PrivacySensitive: has("--privacy-sensitive"),
			PermissionMode:   flag("--permission-mode"),
		}
		now := time.Now()
		plan, err := buildWorkDispatchExecutionPlan(homePath, store, workID, executeOpts, now)
		die(err)
		if has("--confirm") {
			result, err := executeWorkDispatchExecution(homePath, store, plan, executeOpts, now)
			die(err)
			if has("--json") {
				printJSON(result)
			} else {
				fmt.Printf("dispatch-execute %s status=%s adapter=%s model=%s run=%s\n", result.WorkID, result.FinalState, result.Adapter, result.Selected.ModelID, result.RunAttemptID)
			}
			if result.FinalState != "verifying" {
				os.Exit(1)
			}
			return
		}
		if has("--json") {
			printJSON(plan)
		} else {
			fmt.Printf("dispatch-execute PREVIEW work=%s adapter=%s model=%s lane=%s\n", plan.WorkID, plan.Adapter, plan.Selected.ModelID, plan.Selected.Lane)
			fmt.Println("nothing claimed or executed; re-run with --confirm to perform the governed launch")
		}
		os.Exit(1)
	case "dispatch-advance":
		usage := "usage: compound work dispatch-advance (--next|<work_id>) [--actor A] [--json]"
		targetWorkID := parseExplicitDispatchTarget(
			usage,
			map[string]bool{"--actor": true},
			map[string]bool{"--json": true},
		)
		result, err := store.DispatchAdvance(time.Now(), work.DispatchPlanOptions{Actor: flag("--actor"), WorkID: targetWorkID})
		die(err)
		if has("--json") {
			printJSON(result)
			return
		}
		if result.AdvancedWorkID != "" {
			fmt.Printf("dispatch-advance %s status=%s advanced=%s policy=%s\n", result.ID, result.Status, result.AdvancedWorkID, result.PolicyDecisionID)
		} else {
			fmt.Printf("dispatch-advance %s status=%s no_action=%s\n", result.ID, result.Status, result.NoAction)
		}
		if result.ResultPath != "" {
			fmt.Printf("result: %s\n", result.ResultPath)
		}
	case "dispatch-close-plan":
		usage := "usage: compound work dispatch-close-plan (--next|<work_id>) [--actor A] [--outcome completed|failed|cancelled] [--move-type type] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--json]"
		targetWorkID := parseExplicitDispatchTarget(
			usage,
			map[string]bool{"--actor": true, "--outcome": true, "--move-type": true, "--capability": true, "--action": true, "--mutation-class": true, "--reversibility": true, "--blast-radius": true, "--target-kind": true, "--target-id": true, "--target-path": true},
			map[string]bool{"--json": true},
		)
		result, err := store.DispatchClosePlan(time.Now(), work.DispatchClosePlanOptions{
			Actor:         flag("--actor"),
			WorkID:        targetWorkID,
			Outcome:       flag("--outcome"),
			MoveType:      flag("--move-type"),
			Capability:    flag("--capability"),
			Action:        flag("--action"),
			MutationClass: flag("--mutation-class"),
			Reversibility: flag("--reversibility"),
			BlastRadius:   flag("--blast-radius"),
			TargetKind:    flag("--target-kind"),
			TargetID:      flag("--target-id"),
			TargetPath:    flag("--target-path"),
		})
		die(err)
		if has("--json") {
			printJSON(result)
			return
		}
		if result.ClosePlanID != "" && result.Selected != nil {
			fmt.Printf("dispatch-close-plan %s status=%s selected=%s close_plan=%s\n", result.ID, result.Status, result.Selected.WorkID, result.ClosePlanID)
		} else {
			fmt.Printf("dispatch-close-plan %s status=%s no_action=%s\n", result.ID, result.Status, result.NoAction)
		}
		if result.ClosePlanPath != "" {
			fmt.Printf("close_plan: %s\n", result.ClosePlanPath)
		}
		if result.ResultPath != "" {
			fmt.Printf("result: %s\n", result.ResultPath)
		}
	case "dispatch-close":
		usage := "usage: compound work dispatch-close (--next|<work_id>) [--actor A] [--outcome completed|failed|cancelled] [--move-type type] [--summary S] [--json]"
		targetWorkID := parseExplicitDispatchTarget(
			usage,
			map[string]bool{"--actor": true, "--outcome": true, "--move-type": true, "--summary": true},
			map[string]bool{"--json": true},
		)
		result, err := store.DispatchClose(time.Now(), work.DispatchCloseOptions{
			Actor:    flag("--actor"),
			WorkID:   targetWorkID,
			Outcome:  flag("--outcome"),
			MoveType: flag("--move-type"),
			Summary:  flag("--summary"),
		})
		die(err)
		if has("--json") {
			printJSON(result)
			return
		}
		if result.ClosedWorkID != "" {
			fmt.Printf("dispatch-close %s status=%s closed=%s receipt=%s transition=%s\n", result.ID, result.Status, result.ClosedWorkID, result.ReceiptID, result.TransitionID)
		} else {
			fmt.Printf("dispatch-close %s status=%s no_action=%s\n", result.ID, result.Status, result.NoAction)
		}
		if result.DispatchClosePlanPath != "" {
			fmt.Printf("dispatch_close_plan: %s\n", result.DispatchClosePlanPath)
		}
		if result.ResultPath != "" {
			fmt.Printf("result: %s\n", result.ResultPath)
		}
	case "dispatch-step":
		usage := "usage: compound work dispatch-step [--actor A] [--source-packet packet.md] [--source-state proposed|ready] [--source-owner O] [--source-context-pack ctx] [--source-risk tier1] [--source-privacy private] [--outcome completed|failed|cancelled] [--move-type type] [--summary S] [--until-wait] [--max-steps N] [--json]"
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--json", "--until-wait":
			case "--actor", "--source-packet", "--source-state", "--source-owner", "--source-context-pack", "--source-risk", "--source-privacy", "--outcome", "--move-type", "--summary", "--max-steps":
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
		}
		stepOpts := work.DispatchStepOptions{
			Actor:                 flag("--actor"),
			Outcome:               flag("--outcome"),
			MoveType:              flag("--move-type"),
			Summary:               flag("--summary"),
			SourcePacketPath:      flag("--source-packet"),
			SourcePacketState:     flag("--source-state"),
			SourcePacketOwner:     flag("--source-owner"),
			SourcePacketRisk:      flag("--source-risk"),
			SourcePacketPrivacy:   flag("--source-privacy"),
			SourcePacketContextID: flag("--source-context-pack"),
		}
		maxSteps := 0
		if raw := strings.TrimSpace(flag("--max-steps")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			maxSteps = n
		}
		if maxSteps > 0 && !has("--until-wait") {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if has("--until-wait") {
			result, err := store.DispatchStepRun(time.Now(), work.DispatchStepRunOptions{DispatchStepOptions: stepOpts, MaxSteps: maxSteps})
			die(err)
			if has("--json") {
				printJSON(result)
				return
			}
			fmt.Printf("dispatch-step-run %s status=%s steps=%d", result.ID, result.Status, result.StepCount)
			if result.WaitReason != "" {
				fmt.Printf(" wait=%s", result.WaitReason)
			}
			fmt.Println()
			if result.ResultPath != "" {
				fmt.Printf("result: %s\n", result.ResultPath)
			}
			return
		}
		result, err := store.DispatchStep(time.Now(), stepOpts)
		die(err)
		if has("--json") {
			printJSON(result)
			return
		}
		if result.WorkID != "" {
			fmt.Printf("dispatch-step %s status=%s action=%s work=%s\n", result.ID, result.Status, result.Action, result.WorkID)
		} else {
			fmt.Printf("dispatch-step %s status=%s no_action=%s\n", result.ID, result.Status, result.NoAction)
		}
		if result.ResultPath != "" {
			fmt.Printf("result: %s\n", result.ResultPath)
		}
	case "claim-plan":
		usage := "usage: compound work claim-plan <work_id> [--actor A] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--json] [--dry-run]"
		claimPlanValueFlags := map[string]bool{
			"--actor": true, "--capability": true, "--action": true, "--mutation-class": true,
			"--reversibility": true, "--blast-radius": true, "--target-kind": true, "--target-id": true, "--target-path": true,
		}
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--json", "--dry-run":
			case "--actor", "--capability", "--action", "--mutation-class", "--reversibility", "--blast-radius", "--target-kind", "--target-id", "--target-path":
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				if strings.HasPrefix(rest[i], "-") {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
			}
		}
		if pos(claimPlanValueFlags) == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		plan, err := store.ClaimPlan(pos(claimPlanValueFlags), time.Now(), work.ClaimPlanOptions{
			Actor:         flag("--actor"),
			Capability:    flag("--capability"),
			Action:        flag("--action"),
			MutationClass: flag("--mutation-class"),
			Reversibility: flag("--reversibility"),
			BlastRadius:   flag("--blast-radius"),
			TargetKind:    flag("--target-kind"),
			TargetID:      flag("--target-id"),
			TargetPath:    flag("--target-path"),
			Record:        !has("--dry-run"),
		})
		die(err)
		if has("--json") {
			printJSON(plan)
			return
		}
		fmt.Printf("claim-plan %s status=%s work=%s policy=%s allowed=%v\n", plan.ID, plan.Status, plan.WorkID, plan.Policy.Outcome, plan.Policy.Allowed)
		if plan.Policy.StandingGrantID != "" {
			fmt.Printf("standing_grant: %s\n", plan.Policy.StandingGrantID)
		}
		if len(plan.ClaimCommand) > 0 {
			fmt.Printf("command: %s\n", strings.Join(plan.ClaimCommand, " "))
		}
		if plan.ActionContext != nil && len(plan.ActionContext.Command) > 0 {
			fmt.Printf("action_context: %s proof=%s\n", strings.Join(plan.ActionContext.Command, " "), emptyDash(plan.ActionContext.Proof))
		}
		if plan.NoAction != "" {
			fmt.Printf("no_action: %s\n", plan.NoAction)
		}
		if plan.PlanPath != "" {
			fmt.Printf("plan: %s\n", plan.PlanPath)
		}
	case "claim":
		claimFlags := map[string]bool{"--by": true, "--lane": true}
		if err := validateKnownFlags(rest, claimFlags, nil); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, workUsage("claim"))
			os.Exit(2)
		}
		// --lane optionally declares the lane: the ref must resolve to a canonical agent-lane
		// node id (fail-closed) or the claim errors. Absent --lane is byte-identical to before.
		claimID := pos(claimFlags)
		w, err := store.ClaimWithLane(claimID, flag("--by"), flag("--lane"), time.Now())
		if err != nil {
			// A live lease held by another actor is the ONE outcome here that is a gate
			// rather than a fault, and it is decided by re-reading the lease -- an
			// unresolvable --lane or a bad state keeps its own plain error.
			if holder, conflicted := workLeaseConflict(store, claimID, flag("--by")); conflicted {
				workLeaseHolderRefusal("claim", claimID, flag("--by"), holder, err).writeTo(os.Stderr)
				os.Exit(1)
			}
		}
		die(err)
		fmt.Printf("%s claimed by %s (%s) policy=%s\n", w.ID, w.Authority.Owner, w.State, w.PolicyDecisionID)
	case "delivery-capture":
		if code := work.RunWorkDeliveryCaptureCLI(rest, store, time.Now(), os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
	case "advance-plan":
		delivery, deliveryErr := parseWorkDeliveryRequest(rest)
		die(deliveryErr)
		usage := workUsage("advance-plan")
		advancePlanValueFlags := map[string]bool{
			"--delivery-action": true, "--delivery-kind": true, "--delivery-evidence": true,
			"--to": true, "--actor": true, "--reason": true, "--capability": true, "--action": true, "--mutation-class": true,
			"--reversibility": true, "--blast-radius": true, "--target-kind": true, "--target-id": true, "--target-path": true,
			"--target-worktree-root": true, "--target-local-ref": true, "--expected-base-sha": true, "--publication-ref": true,
		}
		if err := validateWorkArguments(rest, advancePlanValueFlags, map[string]bool{"--json": true, "--dry-run": true, "--local-only": true, "--steal": true}, 1); err != nil {
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		advancePlanSteal := false
		advancePlanReason := ""
		for i := 0; i < len(rest) && rest[i] != "--"; i++ {
			if advancePlanValueFlags[rest[i]] {
				if rest[i] == "--reason" {
					advancePlanReason = rest[i+1]
				}
				i++
			} else if rest[i] == "--steal" {
				advancePlanSteal = true
			}
		}
		if pos(advancePlanValueFlags) == "" || flag("--to") == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		gitDelivery := parseGitDelivery(usage)
		plan, err := store.AdvancePlan(pos(advancePlanValueFlags), time.Now(), work.AdvancePlanOptions{
			NonSourceDelivery: delivery,
			Actor:             flag("--actor"),
			To:                flag("--to"),
			GitDelivery:       gitDelivery,
			Capability:        flag("--capability"),
			Action:            flag("--action"),
			MutationClass:     flag("--mutation-class"),
			Reversibility:     flag("--reversibility"),
			BlastRadius:       flag("--blast-radius"),
			TargetKind:        flag("--target-kind"),
			TargetID:          flag("--target-id"),
			TargetPath:        flag("--target-path"),
			Record:            !has("--dry-run"),
			Steal:             advancePlanSteal,
			StealReason:       advancePlanReason,
		})
		die(err)
		if has("--json") {
			printJSON(plan)
			return
		}
		fmt.Printf("advance-plan %s status=%s work=%s to=%s policy=%s allowed=%v\n", plan.ID, plan.Status, plan.WorkID, plan.ToState, plan.Policy.Outcome, plan.Policy.Allowed)
		if plan.Policy.StandingGrantID != "" {
			fmt.Printf("standing_grant: %s\n", plan.Policy.StandingGrantID)
		}
		if len(plan.AdvanceCommand) > 0 {
			fmt.Printf("command: %s\n", strings.Join(plan.AdvanceCommand, " "))
		}
		if plan.ActionContext != nil && len(plan.ActionContext.Command) > 0 {
			fmt.Printf("action_context: %s proof=%s\n", strings.Join(plan.ActionContext.Command, " "), emptyDash(plan.ActionContext.Proof))
		}
		if plan.NoAction != "" {
			fmt.Printf("no_action: %s\n", plan.NoAction)
		}
		if plan.PlanPath != "" {
			fmt.Printf("plan: %s\n", plan.PlanPath)
		}
	case "advance":
		executeRest := rest
		if idx := indexOf(rest, "--"); idx >= 0 {
			rest = rest[:idx]
		}
		delivery, deliveryErr := parseWorkDeliveryRequest(rest)
		die(deliveryErr)
		usage := workUsage("advance")
		advanceValueFlags := map[string]bool{
			"--delivery-action": true, "--delivery-kind": true, "--delivery-evidence": true,
			"--expected-work-state": true, "--expected-policy-decision": true, "--expected-action-intent": true,
			"--to": true, "--by": true, "--actor": true, "--lane": true, "--run-attempt": true, "--artifact": true, "--verification": true,
			"--context-pack": true, "--note": true, "--reason": true,
			"--target-worktree-root": true, "--target-local-ref": true, "--expected-base-sha": true, "--publication-ref": true,
			"--harness": true, "--prompt": true, "--workspace": true, "--permission-mode": true, "--timeout": true,
			"--launch-profile": true, "--effort": true, "--expected-selection-revision": true, "--expected-execution-tuple": true, "--expected-work-revision": true,
		}
		// Validate only the pre-`--` portion of the args. The `--` is the
		// standard POSIX positional separator; everything after is the
		// subprocess argv and is not subject to flag validation. This is
		// the same convention `compound harness run` uses.
		validateSlice := rest
		if idx := indexOf(rest, "--"); idx >= 0 {
			validateSlice = rest[:idx]
		}
		if err := validateWorkArguments(validateSlice, advanceValueFlags, map[string]bool{"--steal": true, "--local-only": true, "--execute": true, "--resume-verifying": true, "--keep-verifying-on-failure": true}, 1); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if valueFlag(validateSlice, "--expected-execution-tuple") != "" && !hasFlag(validateSlice, "--execute") {
			fmt.Fprintln(os.Stderr, "error: --expected-execution-tuple requires --execute")
			os.Exit(2)
		}
		if valueFlag(validateSlice, "--expected-work-revision") != "" && !hasFlag(validateSlice, "--execute") {
			fmt.Fprintln(os.Stderr, "error: --expected-work-revision requires --execute")
			os.Exit(2)
		}
		if hasFlag(validateSlice, "--timeout") && !hasFlag(validateSlice, "--execute") {
			fmt.Fprintln(os.Stderr, "error: --timeout requires --execute")
			os.Exit(2)
		}
		if hasFlag(validateSlice, "--keep-verifying-on-failure") && !hasFlag(validateSlice, "--execute") {
			fmt.Fprintln(os.Stderr, "error: --keep-verifying-on-failure requires --execute")
			os.Exit(2)
		}
		if hasFlag(validateSlice, "--resume-verifying") && (!hasFlag(validateSlice, "--execute") || hasFlag(validateSlice, "--steal")) {
			fmt.Fprintln(os.Stderr, "error: --resume-verifying requires --execute and cannot transfer a lease with --steal")
			os.Exit(2)
		}
		if pos(advanceValueFlags) == "" || flag("--to") == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		// S1: --run-attempt replaces the pre-S1 --run flag. Each value must be a ratt_* id
		// minted by internal/runattempt. The validator in work.Advance rejects non-ratt_
		// ids, but pre-checking here gives the user a clearer error path than a later
		// internal envelope-validation failure.
		var runAttempts []string
		for _, rid := range valuesFlag(rest, "--run-attempt") {
			rid = strings.TrimSpace(rid)
			if rid == "" {
				continue
			}
			if !strings.HasPrefix(rid, "ratt_") {
				fmt.Fprintf(os.Stderr, "error: --run-attempt value %q must start with %q (minted via internal/runattempt.Mint)\n", rid, "ratt_")
				os.Exit(2)
			}
			runAttempts = append(runAttempts, rid)
		}
		gitDelivery := parseGitDelivery(usage)
		// work_r7a_execution_selection_20260911: explicit save; --expected-selection-revision is the CAS guard.
		saveSelection := selectionFlagsFromCLI(store, usage, firstNonEmpty(flag("--by"), flag("--actor")), flag("--launch-profile"), flag("--effort"))
		// Slice D: --execute runs a registered harness adapter inline as
		// part of the advance. The work advances running -> verifying
		// (or failed) in a single CLI invocation. The adapter mints a
		// RunAttempt; the verifying/failed advance carries the ratt_ id
		// on WorkEnvelope.RunAttemptIDs. Implementation lives in
		// work_advance_execute.go to keep this dispatch handler readable.
		if hasFlag(rest, "--execute") {
			// work_r7a_execution_selection_20260911 + 2026-09-15 contract:
			// the FULL resolved selection travels into the execute path —
			// not only the launch-profile id — so the run's snapshot
			// records the resolution (route, model, effort, revision) the
			// invocation asked for. The envelope's saved selection is not
			// rewritten by a one-run override.
			runWorkAdvanceExecute(homePath, store, pos(advanceValueFlags), executeRest, runAttempts, saveSelection, time.Now())
			return
		}
		w, err := store.Advance(pos(advanceValueFlags), work.AdvanceInput{
			NonSourceDelivery:        delivery,
			ExpectedWorkState:        flag("--expected-work-state"),
			ExpectedPolicyDecisionID: flag("--expected-policy-decision"),
			ExpectedActionIntentID:   flag("--expected-action-intent"),
			To:                       flag("--to"),
			Actor:                    firstNonEmpty(flag("--by"), flag("--actor")),
			LaneRef:                  flag("--lane"),
			GitDelivery:              gitDelivery,
			RunAttemptIDs:            runAttempts,
			ArtifactID:               flag("--artifact"),
			VerificationID:           flag("--verification"),
			ContextPackID:            flag("--context-pack"),
			Note:                     flag("--note"),
			Steal:                    has("--steal"),
			StealReason:              flag("--reason"),
		}, time.Now())
		die(err)
		if saveSelection != nil {
			saved, saveErr := work.RunCommandAfterAdvance(store, w, flag("--launch-profile"), flag("--effort"), flag("--expected-selection-revision"), firstNonEmpty(flag("--by"), flag("--actor")), time.Now())
			if saveErr != nil {
				fmt.Fprintf(os.Stderr, "error: save execution selection: %v\n", saveErr)
				os.Exit(1)
			}
			w = saved
		}
		fmt.Printf("%s -> %s policy=%s\n", w.ID, w.State, w.PolicyDecisionID)
	case "observe-run":
		observeRunUsage := "usage: compound work observe-run <work_id> --trace <trace_id> [--job-file <path>] [--actor A] [--note N] [--lane L] [--model M] [--provider-route P] [--tokens-in N] [--tokens-out N] [--json]"
		observeRunFlags := map[string]bool{
			"--trace": true, "--by": true, "--actor": true, "--note": true, "--job-file": true,
			"--lane": true, "--model": true, "--provider-route": true,
			"--tokens-in": true, "--tokens-out": true,
		}
		if err := validateKnownFlags(rest, observeRunFlags, map[string]bool{"--json": true}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, observeRunUsage)
			os.Exit(2)
		}
		workID := pos(observeRunFlags)
		if workID == "" || flag("--trace") == "" {
			fmt.Fprintln(os.Stderr, observeRunUsage)
			os.Exit(2)
		}
		// S3 attribution flags are optional. Token flags parse to int64
		// (zero ≡ unknown / absent); a non-integer value is a usage error.
		tokensIn, tokensOut := int64(0), int64(0)
		if v := flag("--tokens-in"); v != "" {
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "error: --tokens-in must be an integer, got %q\n", v)
				fmt.Fprintln(os.Stderr, observeRunUsage)
				os.Exit(2)
			}
			tokensIn = n
		}
		if v := flag("--tokens-out"); v != "" {
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "error: --tokens-out must be an integer, got %q\n", v)
				fmt.Fprintln(os.Stderr, observeRunUsage)
				os.Exit(2)
			}
			tokensOut = n
		}
		result, err := store.ObserveRun(workID, work.ObserveRunInput{
			Actor:         firstNonEmpty(flag("--by"), flag("--actor")),
			TraceID:       flag("--trace"),
			Note:          flag("--note"),
			JobFilePath:   flag("--job-file"),
			LaneID:        flag("--lane"),
			ModelID:       flag("--model"),
			ProviderRoute: flag("--provider-route"),
			TokensIn:      tokensIn,
			TokensOut:     tokensOut,
		}, time.Now())
		die(err)
		if has("--json") {
			printJSON(result)
			return
		}
		fmt.Printf("%s observed trace=%s state=%s policy=%s\n", result.WorkID, result.TraceID, result.State, result.PolicyDecisionID)
		if result.RunAttemptID != "" {
			fmt.Printf("run_attempt: %s\n", result.RunAttemptID)
		}
		if result.ActionContext != nil && len(result.ActionContext.Command) > 0 {
			fmt.Printf("action_context: %s proof=%s\n", strings.Join(result.ActionContext.Command, " "), emptyDash(result.ActionContext.Proof))
		}
		if result.ResultPath != "" {
			fmt.Printf("result: %s\n", result.ResultPath)
		}
	case "collaborate":
		runWorkCollaborate(store, rest, time.Now())
	case "land-check":
		usage := workUsage("land-check")
		landCheckValueFlags := map[string]bool{"--tip": true, "--repo": true, "--target-ref": true, "--actor": true, "--reason": true}
		if err := validateWorkArguments(rest,
			landCheckValueFlags,
			map[string]bool{"--steal": true, "--dry-run": true, "--json": true}, 1); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if strings.TrimSpace(flag("--tip")) == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		steal := has("--steal")
		stealReasonProvided := has("--reason")
		stealReason := strings.TrimSpace(flag("--reason"))
		switch {
		case steal && (!stealReasonProvided || stealReason == ""):
			fmt.Fprintln(os.Stderr, "error: --steal requires --reason <non-empty>")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		case !steal && stealReasonProvided:
			fmt.Fprintln(os.Stderr, "error: --reason requires --steal")
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		report, err := store.LandCheck(pos(landCheckValueFlags), work.LandCheckOptions{
			RepoPath:        flag("--repo"),
			CandidateTipSHA: flag("--tip"),
			TargetRef:       flag("--target-ref"),
			Actor:           flag("--actor"),
			Record:          !has("--dry-run"),
			Steal:           steal,
			StealReason:     stealReason,
		}, time.Now())
		die(err)
		if has("--json") {
			printJSON(report)
			return
		}
		fmt.Printf("land-check %s status=%s target=%s candidate=%s recorded=%v\n", report.WorkID, report.Status, report.TargetBranchRef, report.CandidateTipSHA, report.Recorded)
		if report.EvidenceRef != "" {
			fmt.Printf("evidence: %s\n", report.EvidenceRef)
		}
		if report.NextAction != "" {
			fmt.Printf("next_action: %s\n", report.NextAction)
		}
	case "salvage-plan":
		usage := workUsage("salvage-plan")
		salvagePlanValueFlags := map[string]bool{"--branch": true}
		if err := validateWorkArguments(rest, salvagePlanValueFlags, map[string]bool{"--json": true}, 1); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if strings.TrimSpace(flag("--branch")) == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		plan, err := store.SalvagePlan(pos(salvagePlanValueFlags), work.SalvagePlanOptions{
			Branch: flag("--branch"),
		}, time.Now())
		die(err)
		if has("--json") {
			printJSON(plan)
			return
		}
		fmt.Printf("salvage-plan %s target=%s branch=%s merge_base=%s stale_base=%v\n", plan.WorkID, plan.TargetBranchRef, plan.Branch, plan.MergeBase, plan.StaleBase)
		fmt.Printf("true_commits=%d true_files=%d naive_deletions=%d true_deletions=%d\n", len(plan.TrueCommits), len(plan.TrueFiles), plan.NaiveDeletions, plan.TrueDeletions)
		if plan.FabricatedDeletionLabel != "" {
			fmt.Printf("fabricated_deletion_label: %s\n", plan.FabricatedDeletionLabel)
		}
		fmt.Printf("teeth_reproof: %s\n", plan.TeethReproofNote)
		for _, command := range plan.SuccessorCommands {
			fmt.Printf("successor_command: %s\n", command)
		}
	case "action-plan":
		usage := "usage: compound work action-plan <work_id> [--actor A] [--move-type type] [--receipt-outcome completed|failed|blocked|no_op] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--json] [--dry-run]"
		actionPlanValueFlags := map[string]bool{
			"--actor": true, "--move-type": true, "--receipt-outcome": true, "--capability": true, "--action": true,
			"--mutation-class": true, "--reversibility": true, "--blast-radius": true, "--target-kind": true, "--target-id": true, "--target-path": true,
		}
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--json", "--dry-run":
			case "--actor", "--move-type", "--receipt-outcome", "--capability", "--action", "--mutation-class", "--reversibility", "--blast-radius", "--target-kind", "--target-id", "--target-path":
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				if strings.HasPrefix(rest[i], "-") {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
			}
		}
		if pos(actionPlanValueFlags) == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		plan, err := store.ActionExecutionPlan(pos(actionPlanValueFlags), time.Now(), work.ActionExecutionPlanOptions{
			Actor:          flag("--actor"),
			Capability:     flag("--capability"),
			Action:         flag("--action"),
			MutationClass:  flag("--mutation-class"),
			Reversibility:  flag("--reversibility"),
			BlastRadius:    flag("--blast-radius"),
			TargetKind:     flag("--target-kind"),
			TargetID:       flag("--target-id"),
			TargetPath:     flag("--target-path"),
			MoveType:       flag("--move-type"),
			ReceiptOutcome: flag("--receipt-outcome"),
			Record:         !has("--dry-run"),
		})
		die(err)
		if has("--json") {
			printJSON(plan)
			return
		}
		fmt.Printf("action-plan %s status=%s work=%s state=%s policy=%s allowed=%v\n", plan.ID, plan.Status, plan.WorkID, plan.WorkState, plan.Policy.Outcome, plan.Policy.Allowed)
		if plan.ActionSummary != "" {
			fmt.Printf("action_summary: %s\n", plan.ActionSummary)
		}
		if len(plan.ActionCommand) > 0 {
			fmt.Printf("action_command: %s\n", strings.Join(plan.ActionCommand, " "))
		}
		if len(plan.RequiredInputs) > 0 {
			fmt.Printf("required_inputs: %s\n", strings.Join(plan.RequiredInputs, ", "))
		}
		if len(plan.ExpectedValidation) > 0 {
			fmt.Printf("expected_validation: %s\n", strings.Join(plan.ExpectedValidation, ", "))
		}
		if len(plan.StopConditions) > 0 {
			fmt.Printf("stop_conditions: %s\n", strings.Join(plan.StopConditions, ", "))
		}
		if plan.Boundary != "" {
			fmt.Printf("boundary: %s\n", plan.Boundary)
		}
		if plan.Proof != "" {
			fmt.Printf("proof: %s\n", plan.Proof)
		}
		if len(plan.RecordCommand) > 0 {
			fmt.Printf("record_after_execution: %s\n", strings.Join(plan.RecordCommand, " "))
		}
		if len(plan.CloseCommand) > 0 {
			fmt.Printf("close_after_validation: %s\n", strings.Join(plan.CloseCommand, " "))
		}
		if plan.NoAction != "" {
			fmt.Printf("no_action: %s\n", plan.NoAction)
		}
		if plan.PlanPath != "" {
			fmt.Printf("plan: %s\n", plan.PlanPath)
		}
	case "act":
		actUsage := "usage: compound work act <work_id> (--intent <action-intent.json> | --plan <action-execution-plan.json>)"
		actFlags := map[string]bool{"--intent": true, "--plan": true}
		if err := validateKnownFlags(rest, actFlags, nil); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, actUsage)
			os.Exit(2)
		}
		workID := pos(actFlags)
		intentFile := flag("--intent")
		planFile := flag("--plan")
		if workID == "" || (intentFile == "" && planFile == "") || (intentFile != "" && planFile != "") {
			fmt.Fprintln(os.Stderr, actUsage)
			os.Exit(2)
		}
		var ai envelope.ActionIntent
		if planFile != "" {
			plan, err := work.LoadActionExecutionPlan(planFile)
			die(err)
			if plan.WorkID != workID {
				die(fmt.Errorf("action plan belongs to work %s, not %s", plan.WorkID, workID))
			}
			ai = plan.ActionIntent
		} else {
			ai = loadActionIntent(intentFile)
			if ai.IDs.WorkID == "" {
				ai.IDs.WorkID = workID
			}
		}
		result, err := store.EvaluateAndRecordAction(workID, ai, time.Now())
		die(err)
		printJSON(result.Decision)
		if !result.Recorded {
			fmt.Fprintf(os.Stderr, "BLOCKED: policy %s — proposal %s persisted; action NOT recorded\n", result.Decision.Outcome, ai.ID)
			if result.Decision.Outcome == "hold" && ai.SecurityFloorOutcome == "" {
				fmt.Fprintf(os.Stderr, "review: compound policy action-plan %s --ttl 15m --json\n", ai.ID)
			}
			os.Exit(gateExit(result.Decision))
		}
		fmt.Printf("%s: action %s recorded (policy %s, decision %s)\n", result.Work.ID, ai.ID, result.Decision.Outcome, result.Decision.ID)
	case "close-plan":
		usage := "usage: compound work close-plan <work_id> (--action-plan <action-execution-plan.json> | --outcome completed|failed|cancelled --move-type <type>) [--actor A] [--capability C] [--action A] [--mutation-class C] [--reversibility R] [--blast-radius B] [--target-kind K] [--target-id ID] [--target-path P] [--steal --reason R] [--json] [--dry-run]"
		closePlanValueFlags := map[string]bool{
			"--outcome": true, "--move-type": true, "--actor": true, "--action-plan": true, "--capability": true, "--action": true,
			"--mutation-class": true, "--reversibility": true, "--blast-radius": true, "--target-kind": true, "--target-id": true, "--target-path": true, "--reason": true,
		}
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--json", "--dry-run", "--steal":
			case "--outcome", "--move-type", "--actor", "--action-plan", "--capability", "--action", "--mutation-class", "--reversibility", "--blast-radius", "--target-kind", "--target-id", "--target-path", "--reason":
				if i+1 >= len(rest) {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
				i++
			default:
				if strings.HasPrefix(rest[i], "-") {
					fmt.Fprintln(os.Stderr, usage)
					os.Exit(2)
				}
			}
		}
		if pos(closePlanValueFlags) == "" || (flag("--action-plan") == "" && (flag("--outcome") == "" || flag("--move-type") == "")) {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		plan, err := store.ClosePlan(pos(closePlanValueFlags), time.Now(), work.ClosePlanOptions{
			Actor:          flag("--actor"),
			Outcome:        flag("--outcome"),
			MoveType:       flag("--move-type"),
			ActionPlanPath: flag("--action-plan"),
			Capability:     flag("--capability"),
			Action:         flag("--action"),
			MutationClass:  flag("--mutation-class"),
			Reversibility:  flag("--reversibility"),
			BlastRadius:    flag("--blast-radius"),
			TargetKind:     flag("--target-kind"),
			TargetID:       flag("--target-id"),
			TargetPath:     flag("--target-path"),
			Record:         !has("--dry-run"),
			Steal:          has("--steal"),
			StealReason:    flag("--reason"),
		})
		die(err)
		if has("--json") {
			printJSON(plan)
			return
		}
		fmt.Printf("close-plan %s status=%s work=%s outcome=%s move_type=%s policy=%s allowed=%v\n", plan.ID, plan.Status, plan.WorkID, plan.Outcome, plan.MoveType, plan.Policy.Outcome, plan.Policy.Allowed)
		if plan.Policy.StandingGrantID != "" {
			fmt.Printf("standing_grant: %s\n", plan.Policy.StandingGrantID)
		}
		if len(plan.CloseCommand) > 0 {
			fmt.Printf("command: %s\n", strings.Join(plan.CloseCommand, " "))
		}
		if len(plan.ProofRefs) > 0 {
			fmt.Printf("proof_refs: %s\n", strings.Join(plan.ProofRefs, ","))
		}
		if len(plan.RunObservations) > 0 {
			fmt.Printf("run_observations: %d\n", len(plan.RunObservations))
		}
		if plan.ActionExecutionPlanID != "" {
			fmt.Printf("action_execution_plan: %s\n", plan.ActionExecutionPlanID)
		}
		if len(plan.ExpectedValidation) > 0 {
			fmt.Printf("expected_validation: %s\n", strings.Join(plan.ExpectedValidation, ", "))
		}
		if plan.NoAction != "" {
			fmt.Printf("no_action: %s\n", plan.NoAction)
		}
		if plan.PlanPath != "" {
			fmt.Printf("plan: %s\n", plan.PlanPath)
		}
	case "close":
		closeUsage := "usage: compound work close <work_id> --outcome completed|failed|cancelled --move-type <type> [--summary S] [--changed] [--by A|--actor A] [--proof REF[,REF]...] [--proof-ref REF] [--used C] [--missed C] [--false-triggered C] [--failure-surface S] [--failure-mechanism M] [--memory-candidate C] [--memory-promoted C] [--memory-parked C] [--memory-rejected C] [--opens-branch slug:owner:kind]... [--acknowledge-abandoned ratt_X --reason R]... [--verification-rejected ratt_X --reason R]... [--steal --reason R] [--transcript PATH] [--non-use REF --non-use-evidence REF]\n  (passing --used at all — even --used \"\" for declared-none — stamps skills.used_declared=true, records an explicit use declaration; absence of other refs remains unknown)\n  (--transcript PATH, or COMPOUND_TRANSCRIPT_PATH, runs the same reference detector as SessionEnd and records matches as references only; an absent or unreadable transcript is fail-open — unknown, never ignored, never a blocked close)\n  (--opens-branch declares a future branch on the emitted transition: kind is opens|blocks|supersedes|validates|retires|routes_to; the owner may itself contain colons)\n  (a work held by another actor's lease refuses close unless --by/--actor matches the holder, or --steal --reason \"...\" is given)"
		closeValueFlags := map[string]bool{
			"--outcome":               true,
			"--move-type":             true,
			"--summary":               true,
			"--by":                    true,
			"--actor":                 true,
			"--used":                  true,
			"--missed":                true,
			"--false-triggered":       true,
			"--failure-surface":       true,
			"--failure-mechanism":     true,
			"--proof":                 true,
			"--proof-ref":             true,
			"--memory-candidate":      true,
			"--memory-promoted":       true,
			"--memory-parked":         true,
			"--memory-rejected":       true,
			"--opens-branch":          true,
			"--reason":                true,
			"--acknowledge-abandoned": true,
			"--verification-rejected": true,
			"--transcript":            true,
			"--non-use":               true,
			"--non-use-evidence":      true,
		}
		if err := validateKnownFlags(rest, closeValueFlags, map[string]bool{
			"--changed": true,
			"--steal":   true,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, closeUsage)
			os.Exit(2)
		}
		branchRefs, branchErr := parseOpensBranchFlags(flagCSVValues(rest, "--opens-branch"))
		if branchErr != nil {
			fmt.Fprintln(os.Stderr, "error:", branchErr)
			fmt.Fprintln(os.Stderr, closeUsage)
			os.Exit(2)
		}
		ackAttempts, ackErr := work.ParseAcknowledgeAbandonedFlagPairs(rest, flag("--reason"))
		if ackErr != nil {
			fmt.Fprintln(os.Stderr, "error:", ackErr)
			fmt.Fprintln(os.Stderr, closeUsage)
			os.Exit(2)
		}
		closeID := pos(closeValueFlags)
		closeActor := firstNonEmpty(flag("--by"), flag("--actor"))
		closeProofRefs := flagCSVValues(rest, "--proof", "--proof-ref")
		// A readable transcript yields reference evidence only. Missing input leaves
		// application and non-use unknown; neither path blocks close.
		closeTranscript := firstNonEmpty(flag("--transcript"), os.Getenv("COMPOUND_TRANSCRIPT_PATH"))
		closeConsumedRefs, closeTranscriptDeclared := hook.ConsumptionForClose(store, homePath, closeID, closeTranscript)
		if has("--non-use") && strings.TrimSpace(flag("--non-use-evidence")) == "" {
			fmt.Fprintln(os.Stderr, "error: --non-use requires --non-use-evidence")
			os.Exit(2)
		}
		res, err := store.Close(closeID, work.CloseInput{
			Outcome:          flag("--outcome"),
			MoveType:         flag("--move-type"),
			Actor:            closeActor,
			Summary:          flag("--summary"),
			Changed:          has("--changed"),
			UsedCapabilities: splitCSV(flag("--used")),
			// Declared iff --used was EXPLICITLY passed (even --used "" = declared-none), OR the
			// transcript detector actually ran. Omitting both leaves used_declared=false, so
			// injected refs stay unknown, not ignored.
			UsedDeclared:               has("--used"),
			ReferencedRefs:             closeConsumedRefs,
			ObservationMethod:          "transcript_reference_scan",
			ObservationQuality:         map[bool]string{true: "readable", false: "unobservable"}[closeTranscriptDeclared],
			ApplicationMethod:          map[bool]string{true: "explicit_declaration", false: ""}[has("--used")],
			NonUseMethod:               map[bool]string{true: "explicit_declaration", false: ""}[has("--non-use")],
			ExplicitNonUse:             splitCSV(flag("--non-use")),
			NonUseEvidence:             flag("--non-use-evidence"),
			MissedCapabilities:         splitCSV(flag("--missed")),
			FalseTriggeredCapabilities: splitCSV(flag("--false-triggered")),
			ProofRefs:                  closeProofRefs,
			MemoryCandidates:           splitCSV(flag("--memory-candidate")),
			MemoryPromoted:             splitCSV(flag("--memory-promoted")),
			MemoryParked:               splitCSV(flag("--memory-parked")),
			MemoryRejected:             splitCSV(flag("--memory-rejected")),
			FailureSurfaceOutcome:      flag("--failure-surface"),
			FailureMechanism:           flag("--failure-mechanism"),
			Steal:                      has("--steal"),
			StealReason:                flag("--reason"),
			OpensBranches:              branchRefs,
			RunAttemptTerminalizations: ackAttempts,
		}, time.Now())
		if err != nil {
			// Close has two fail-closed gates worth teaching. The lease gate is decided
			// structurally (re-read the lease); the git-delivery proof gate is not typed by
			// its enforcer, so it is matched at the seam documented in work_lease_refusal.go
			// and pinned by a test that drives the real gate.
			if holder, conflicted := workLeaseConflict(store, closeID, closeActor); conflicted {
				workLeaseHolderRefusal("close", closeID, closeActor, holder, err).writeTo(os.Stderr)
				os.Exit(1)
			}
			if isGitDeliveryProofRefusal(err) {
				gitDeliveryProofRefusal(closeID, flag("--outcome"), closeProofRefs, err).writeTo(os.Stderr)
				os.Exit(1)
			}
		}
		die(err)
		printWorkCloseResult(res)
		observeWorkSurpriseOnDivergedClose(homePath, res)
		observeEVCardCalibrationOnClose(homePath, res)
		offerStandingProofGraduationOnClose(homePath, res)
		autoConsumeCapabilityFeedback(homePath)
	case "sweep-stale":
		// sweep-stale is the work-state-machine counterpart to
		// `compound run-attempt sweep`: where the run-attempt sweep
		// terminalizes stuck RunAttempts (and explicitly does NOT touch the
		// work state machine), sweep-stale advances stuck WorkEnvelopes to
		// failed/cancelled and explicitly does NOT touch RunAttempts. The
		// two never run on the same byte; each owns its organ.
		os.Exit(work.RunWorkSweepStaleCLI(rest, store, homePath, time.Now(), os.Stdout, os.Stderr))
	case "reseal":
		// reseal is the governed repair door for terminal envelopes whose
		// recorded custody seal no longer matches their bytes (the 71
		// envelopes the 2026-08-28 sweep reclaimed before it learned to
		// re-seal). Dry-run by default; the repair is append-only.
		os.Exit(work.RunWorkResealCLI(rest, store, time.Now(), os.Stdout, os.Stderr))
	case "set-aside":
		usage := workUsage("set-aside")
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		// setAsidePositional extracts the single <work_id> positional from a
		// set-aside subcommand's args (the "begin"/"end" token already stripped),
		// skipping registered value-flags and any dashed token.
		setAsidePositional := func(args []string, valueFlags map[string]bool) string {
			for i := 0; i < len(args); i++ {
				a := args[i]
				if valueFlags[a] {
					i++
					continue
				}
				if strings.HasPrefix(a, "-") {
					continue
				}
				return a
			}
			return ""
		}
		switch rest[0] {
		case "begin":
			beginValueFlags := map[string]bool{"--candidate": true, "--file": true, "--by": true, "--reason": true}
			if err := validateWorkArguments(rest[1:], beginValueFlags, map[string]bool{"--json": true}, 1); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			workID := setAsidePositional(rest[1:], beginValueFlags)
			if workID == "" {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			record, err := store.SetAsideBegin(work.SetAsideBeginOptions{
				WorkID:       workID,
				CandidateSHA: flag("--candidate"),
				Files:        flagCSVValues(rest, "--file"),
				Actor:        flag("--by"),
				Reason:       flag("--reason"),
			}, time.Now())
			die(err)
			if has("--json") {
				printJSON(record)
				return
			}
			paths := make([]string, 0, len(record.Files))
			for _, f := range record.Files {
				paths = append(paths, f.Path)
			}
			fmt.Printf("set-aside begin %s token=%s files=%d (%s)\n", record.WorkID, record.Token, len(record.Files), strings.Join(paths, ", "))
			fmt.Println("restore with: compound work set-aside end " + record.WorkID)
		case "end":
			endValueFlags := map[string]bool{"--by": true}
			if err := validateWorkArguments(rest[1:], endValueFlags, map[string]bool{"--json": true}, 1); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			workID := setAsidePositional(rest[1:], endValueFlags)
			if workID == "" {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			result, err := store.SetAsideEnd(work.SetAsideEndOptions{WorkID: workID, Actor: flag("--by")}, time.Now())
			die(err)
			if has("--json") {
				printJSON(result)
				return
			}
			fmt.Printf("set-aside end %s token=%s restored=%d (%s)\n", result.WorkID, result.Token, len(result.Restored), strings.Join(result.Restored, ", "))
		default:
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	default:
		fmt.Fprintln(os.Stderr, workUsage(""))
		os.Exit(2)
	}
}

func printWorkCloseResult(res work.CloseResult) {
	fmt.Printf("%s closed: %s  move_type=%s  policy=%s  receipt=%s  transition=%s\n",
		res.Work.ID, res.Work.State, res.Receipt.MoveType, res.Work.PolicyDecisionID, res.Receipt.ID, res.Transition.ID)
	if len(res.UnterminalizedRunAttemptIDs) > 0 {
		fmt.Printf("warning: unterminalized_run_attempts=%s; recover with: compound work reconcile-run-attempts %s\n",
			strings.Join(res.UnterminalizedRunAttemptIDs, ","), res.Work.ID)
	}
	printJSON(res)
}

// observeWorkSurpriseOnDivergedClose is the cmd-layer wiring for the prediction/surprise
// exhaust door (see internal/mem/surprise.go, ObserveWorkSurprise): after a successful close, a
// DIVERGED staked prediction gets its Receipt.Surprise externalized into exactly one observed
// candidate. This call lives here, at the cmd layer, rather than inside internal/work's
// closeLocked, on purpose -- mirrors how CaptureExhaust is invoked from main.go rather than the
// spine -- so internal/work never imports internal/mem (Spine must not import MemTarget).
//
// A matching (non-diverged) close or a close with no staked prediction mints nothing (F6): this
// is a no-op. Exhaust failure (open store, marshal) must NEVER fail an already-successful close;
// it prints a warning line to stderr instead.
// parseOpensBranchFlags parses the repeatable --opens-branch flag values into
// validated BranchRefs before any close runs (fail-fast at the door).
func parseOpensBranchFlags(values []string) ([]envelope.BranchRef, error) {
	if len(values) == 0 {
		return nil, nil
	}
	refs := make([]envelope.BranchRef, 0, len(values))
	for _, v := range values {
		ref, err := parseOpensBranchFlag(v)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// parseOpensBranchFlag parses "slug:owner:kind". The owner is a consumer ref
// that may itself contain colons (e.g. claude:orchestrator), so the slug is
// taken up to the FIRST colon and the kind after the LAST; the middle is the
// owner. Kind is validated here; slug/owner shape is validated again by the
// emitted Transition's own validation inside Close (fail-closed mint).
func parseOpensBranchFlag(value string) (envelope.BranchRef, error) {
	var ref envelope.BranchRef
	first := strings.Index(value, ":")
	last := strings.LastIndex(value, ":")
	if first <= 0 || last <= first || last == len(value)-1 {
		return ref, fmt.Errorf("--opens-branch must be slug:owner:kind, got %q", value)
	}
	ref.Branch = value[:first]
	ref.Owner = value[first+1 : last]
	ref.Kind = value[last+1:]
	if ref.Owner == "" {
		return ref, fmt.Errorf("--opens-branch owner must be non-empty, got %q", value)
	}
	kindOK := false
	for _, k := range envelope.BranchKinds {
		if ref.Kind == k {
			kindOK = true
		}
	}
	if !kindOK {
		return ref, fmt.Errorf("--opens-branch kind must be one of %v, got %q", envelope.BranchKinds, ref.Kind)
	}
	return ref, nil
}

func observeWorkSurpriseOnDivergedClose(homePath string, res work.CloseResult) {
	surprise := res.Receipt.Surprise
	if surprise == nil || !surprise.Diverged {
		return
	}
	payload, err := json.Marshal(surprise)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: work-surprise exhaust skipped (marshal surprise):", err)
		return
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: work-surprise exhaust skipped (open mem store):", err)
		return
	}
	if _, _, err := memStore.ObserveWorkSurprise(res.Work.ID, res.Receipt.ID, payload, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "warning: work-surprise exhaust failed:", err)
	}
}

// observeEVCardCalibrationOnClose is the cmd-layer wiring for the EV-card calibration exhaust
// door (see internal/routeev/calibrate.go, Calibrate; internal/mem/evcard_calibration.go,
// ObserveEVCardCalibration -- spec docs_private/plan/ev-card-spec.md §5, §7 S4). It mirrors
// observeWorkSurpriseOnDivergedClose immediately above: lives here, at cmd layer, rather than
// inside internal/work's closeLocked, on purpose -- same reason CaptureExhaust is invoked from
// main.go rather than the spine -- so internal/work never imports internal/mem or
// internal/routeev (Spine must not import MemTarget).
//
// A close whose WorkEnvelope carries no RouteDeviation, or a RouteDeviation with an empty
// CardRef, has nothing to calibrate -- no-op. A missing or corrupt card blob must NEVER fail an
// already-successful close (calibration is exhaust, never a gate): it prints a warning line to
// stderr instead. Deliberate difference from observeWorkSurpriseOnDivergedClose: that helper
// pre-filters on SurpriseSignal.Diverged before calling its door, but ObserveEVCardCalibration
// checks CalibrationResult.Diverged ITSELF and mints nothing for a vindicated/unpriceable verdict
// (see its own doc comment), so this wiring hands it every computed result unfiltered and lets
// the door be the one place that guarantee is enforced.
func observeEVCardCalibrationOnClose(homePath string, res work.CloseResult) {
	dev := res.Work.RouteDeviation
	if dev == nil || strings.TrimSpace(dev.CardRef) == "" {
		return // nothing to calibrate
	}
	card, err := routeev.Read(homePath, dev.CardRef)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: evcard calibration skipped (read ev card):", err)
		return
	}
	result := routeev.Calibrate(card, res.Work.State)
	payload, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: evcard calibration skipped (marshal calibration):", err)
		return
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: evcard calibration skipped (open mem store):", err)
		return
	}
	if _, _, err := memStore.ObserveEVCardCalibration(res.Work.ID, res.Receipt.ID, dev.CardRef, payload, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "warning: evcard calibration failed:", err)
	}
}

// offerStandingProofGraduationOnClose is the cmd-layer wiring for the standing-proof graduation
// exhaust door (see internal/mem/standing_proof.go, OfferStandingProofGraduation): after a
// SUCCESSFUL (completed) close whose WorkEnvelope carries a machine-checkable, re-runnable proof
// command as its success predicate, compound mints exactly ONE observed-class PROPOSED candidate
// offering to graduate that command into a standing re-verified proof. It attacks "landed !=
// working": today close evidence is point-in-time and nothing re-runs the proof.
//
// This lives here, at the cmd layer -- NOT inside internal/work's closeLocked -- on purpose,
// mirroring observeWorkSurpriseOnDivergedClose (and CaptureExhaust from main.go): the mint path
// must never fail the close, and the close spine must not import the mem mint door. A non-completed
// close, or a completed close whose predicate is ordinary prose (the common case), mints nothing.
// Mint failure surfaces as a visible stderr warning while the already-successful close stands.
func offerStandingProofGraduationOnClose(homePath string, res work.CloseResult) {
	if res.Work.State != "completed" {
		return // only a successful close graduates a proof
	}
	command, ok := work.CommandShapedPredicate(res.Work.SuccessPredicate)
	if !ok {
		return // no machine-checkable proof command in the success predicate -- mint nothing
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: standing-proof graduation offer skipped (open mem store):", err)
		return
	}
	if _, _, err := memStore.OfferStandingProofGraduation(res.Work.ID, res.Receipt.ID, command, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "warning: standing-proof graduation offer failed:", err)
	}
}

func validateWorkArguments(args []string, valueFlags, boolFlags map[string]bool, positionalCount int) error {
	positions := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// `--` is the standard POSIX positional separator: everything
		// after is treated as positional arguments, no flag parsing.
		// This lets callers do `compound ... -- echo hello` where the
		// echo argv shouldn't be touched by flag validation.
		if arg == "--" {
			positions += len(args) - i - 1
			break
		}
		switch {
		case boolFlags[arg]:
		case valueFlags[arg]:
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return fmt.Errorf("%s requires a value", arg)
			}
			i++
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("unknown flag %s", arg)
		default:
			positions++
		}
	}
	if positions != positionalCount {
		return fmt.Errorf("expected %d positional argument(s), got %d", positionalCount, positions)
	}
	return nil
}

// selectionFlagsFromCLI resolves --launch-profile / --effort via the work resolver; nil when missing. (work_r7a_execution_selection_20260911.)
func selectionFlagsFromCLI(store *work.Store, usage, actor, lp, effort string) (sel *envelope.ExecutionSelection) {
	lp = strings.TrimSpace(lp)
	if lp == "" {
		return nil
	}
	sel, err := work.ResolveExecutionSelectionFromFlags(store, lp, effort, actor, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	return
}
