package decisions

// grant_mint turns the auto-admit autonomy grant into a condition-gated,
// plain-language Decide card (operator correction of record, 2026-07-11): a
// governance decision must read as a calm card a human acts on in one click,
// not a CLI command to copy. The card is SYNTHESIZED from live state — there is
// no on-disk review artifact — and appears only when BOTH (a) >=1 non-terminal
// director-authored envelope of an auto-admit-capable class is waiting AND
// (b) no valid active autonomy grant for that class exists. A card resting in
// operator attention with no live decision behind it is a defect (two-class
// law), so absent either condition this annotator emits nothing.
//
// It mutates only the projection: it reads decision claims (mem) and work
// envelopes (work store list), writes no owner file and promotes no claim. The
// governed mint happens through the SAME `decisions record` admission path the
// CLI uses; this only makes it a legible card.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

// autonomyGrantMintArea is the condition-gated Decide area that surfaces the
// "let proven work enter the queue on its own" grant as a plain-language card.
const autonomyGrantMintArea = "autonomy-grant-mint"

// autoAdmitCapableClasses mirrors the single enabled evidence-autonomy slice
// literal at internal/work/auto_admit.go (`class != "job_health"` is rejected
// there). This card only ever offers a grant for a class the runtime can
// actually auto-admit; widen this list in lockstep when that literal widens.
var autoAdmitCapableClasses = []string{"job_health"}

// AnnotateAutonomyGrantMintQueue appends one grant-mint card per auto-admit
// class that has waiting director work and no valid active grant. See the
// package doc above for the two gating conditions.
func AnnotateAutonomyGrantMintQueue(report *reviewartifact.QueueReport, homePath string) {
	if report == nil {
		return
	}
	// This annotator SYNTHESIZES items with no backing artifact, so it must
	// respect an area-scoped read (the Queue's own on-disk filter runs before
	// annotators) — otherwise the card would leak into an unrelated --area query.
	if report.Area != "" && report.Area != autonomyGrantMintArea {
		return
	}
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return
	}
	granted, ok := activeAutonomyGrantClasses(homePath)
	if !ok {
		// Fail closed: if grant records cannot be read we cannot prove a grant is
		// absent, so we do not prompt the operator to record one that may exist.
		return
	}
	waiting := waitingDirectorClassCounts(homePath)
	for _, class := range autoAdmitCapableClasses {
		if granted[class] {
			continue
		}
		n := waiting[class]
		if n <= 0 {
			continue
		}
		item, err := autonomyGrantMintItem(class, n)
		if err != nil {
			continue
		}
		report.Items = append(report.Items, item)
	}
}

// activeAutonomyGrantClasses returns the set of classes that already hold a
// valid active autonomy grant. It mirrors the match semantics of
// internal/director.resolveAutonomyGrant (kind=grant, scope=autonomy-evidence-
// threshold, reviewed_by present, complete+safe thresholds) using only
// decisions-package exports, but treats an invalid/superseded record as ABSENT
// (so the card emits) rather than erroring. The bool is false only when the
// grant records could not be read at all.
func activeAutonomyGrantClasses(homePath string) (map[string]bool, bool) {
	// OpenReadOnly: this annotates a read-only queue projection off the memory
	// ledger and must never scaffold candidates/ or ledger/ as a side effect.
	// An ABSENT ledger reads as "no grants" (empty map, ok) — exactly the
	// fresh-home semantics mem.Open used to provide by mkdir-ing the ledger
	// before reading it empty. Only a genuine open failure fails closed.
	store, err := mem.OpenReadOnly(homePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, true
		}
		return nil, false
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		return nil, false
	}
	granted := map[string]bool{}
	for _, claim := range AuthoritativeActiveDecisions(claims, KindGrant) {
		lc := LifecycleFromClaim(claim)
		if lc.Scope != AutonomyGrantScope {
			continue
		}
		class := strings.TrimSpace(lc.AutoAdmitClass)
		if class == "" {
			continue
		}
		if strings.TrimSpace(lc.ReviewedBy) == "" {
			continue // missing operator provenance ⇒ not a valid grant
		}
		if lc.MinTerminalSamples <= 0 || lc.MinWindowDays <= 0 || lc.MaxFailures < 0 || lc.MaxFailures >= lc.MinTerminalSamples {
			continue // incomplete/unsafe thresholds ⇒ not a valid grant
		}
		granted[class] = true
	}
	return granted, true
}

// waitingDirectorClassCounts counts non-terminal director-authored work per
// autonomy class, keyed off the `director_trigger:<class>:<digest>` evidence
// ref. It mirrors internal/director.loadDuplicateIndex's open-work scan using
// the work store's own List API; a read failure yields an empty map (emit
// nothing) rather than a phantom card.
func waitingDirectorClassCounts(homePath string) map[string]int {
	counts := map[string]int{}
	// OpenReadOnly: a read failure yields an empty map per the contract above;
	// counting director work must not materialize work/ or events/.
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return counts
	}
	works, err := store.List()
	if err != nil {
		return counts
	}
	for _, w := range works {
		if workStateTerminalForGrantMint(w.State) {
			continue
		}
		if class := directorTriggerClass(w.EvidenceRefs); class != "" {
			counts[class]++
		}
	}
	return counts
}

// directorTriggerClass extracts the autonomy class from a director_trigger
// evidence ref (director_trigger:<class>:<digest>), mirroring
// internal/work.autonomyClassOf. Work without a director trigger ref is not
// director-authored and never counts.
func directorTriggerClass(refs []string) string {
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if !strings.HasPrefix(ref, "director_trigger:") {
			continue
		}
		rest := strings.TrimPrefix(ref, "director_trigger:")
		if i := strings.Index(rest, ":"); i > 0 {
			return rest[:i]
		}
		if rest != "" {
			return rest
		}
	}
	return ""
}

// workStateTerminalForGrantMint mirrors the work state machine's terminal set
// (internal/work.transitions comment: completed/failed/cancelled are terminal).
// Duplicated deliberately: the internal/work helper is unexported and this
// read-only annotator must not reach into a mutation seam.
func workStateTerminalForGrantMint(state string) bool {
	switch strings.TrimSpace(state) {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// autonomyGrantMintItem synthesizes the operator-facing queue item: an operator
// card (plain summary + group key) plus a read-only decision card carrying the
// what/why/never-changes copy and the exact governed record command.
func autonomyGrantMintItem(class string, waiting int) (reviewartifact.QueueItem, error) {
	plain := plainAutonomyClass(class)
	item := reviewartifact.QueueItem{
		Area:       autonomyGrantMintArea,
		ID:         autonomyGrantMintID(class),
		Status:     "review_required",
		Readiness:  "needs_review",
		NextAction: "grant autonomy for " + plain + " admission, or keep clicking each one",
	}
	card := reviewartifact.OperatorCard{
		Summary:      "Let proven " + plain + " work enter the queue on its own?",
		ContextLabel: "Autonomy grant",
		GroupKey:     autonomyGrantMintArea + "|" + class,
		Unlock:       "Granting lets proven " + plain + " work enter the queue without a per-item ask.",
	}
	if err := reviewartifact.SetOperatorCard(&item, card, reviewartifact.SummarySourceAnnotator, reviewartifact.BandNeedsYou); err != nil {
		return reviewartifact.QueueItem{}, err
	}
	item.DecisionCard = autonomyGrantMintDecisionCard(class, waiting)
	item.Actions = []reviewartifact.Action{autonomyGrantMintAction(class)}
	return item, nil
}

func autonomyGrantMintDecisionCard(class string, waiting int) *reviewartifact.DecisionCard {
	plain := plainAutonomyClass(class)
	return &reviewartifact.DecisionCard{
		Claim: "Let proven " + plain + " work enter the queue on its own?",
		Why: []reviewartifact.DecisionCardReason{
			{
				Label:    "why now",
				Question: "Why is this waiting for you?",
				Detail:   autonomyGrantMintWhy(plain, waiting),
			},
			{
				Label:    "what changes",
				Question: "What changes if you grant it?",
				Detail: "After the system has closed enough " + plain + " items cleanly (you set the bar below), " +
					"new ones enter the queue without your click — nothing runs outside the normal executor and proof path. Every entry writes a receipt, and one switch halts it.",
			},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{
				ID:    "grant",
				Label: "Grant it",
				Effect: "Records an autonomy grant so proven " + plain + " work enters the queue on its own; " +
					"every entry still writes a receipt and one switch halts it.",
			},
		},
		Boundary: []string{
			"Anything external, destructive, irreversible, financial, or security-related still always comes to you. No grant can override that.",
		},
		Meta: map[string]string{
			"auto_admit_class": class,
			"waiting_count":    fmt.Sprintf("%d", waiting),
		},
		// The machine terms (scope, class, thresholds) stay in this exact-command
		// disclosure — never in the plain card copy above (glossary law).
		CanonicalActionHint: "compound decisions record --kind grant --scope " + AutonomyGrantScope +
			" --auto-admit-class " + class + " --min-terminal-samples <n> --min-window-days <n>" +
			" --max-failures <n> --reviewed-by <operator>",
	}
}

// autonomyGrantMintWhy renders the dynamic evidence line. The card only emits
// when waiting >= 1, so the singular branch is a grammar nicety, not a state.
func autonomyGrantMintWhy(plain string, waiting int) string {
	if waiting == 1 {
		return "1 " + plain + " item is waiting; each currently needs your click."
	}
	return fmt.Sprintf("%d %s items are waiting; each currently needs your click.", waiting, plain)
}

// autonomyGrantMintAction carries the governed record command with the card's
// default thresholds. The console's bespoke registry rebuilds this argv from
// the operator's edited fields; this default-valued copy keeps the item
// actionable (Recount) and gives attention a clean argv[0]=="compound".
func autonomyGrantMintAction(class string) reviewartifact.Action {
	argv := []string{
		"compound", "decisions", "record",
		"--kind", "grant",
		"--scope", AutonomyGrantScope,
		"--auto-admit-class", class,
		"--min-terminal-samples", "5",
		"--min-window-days", "7",
		"--max-failures", "0",
		"--reviewed-by", "operator-console",
		"--title", autonomyGrantMintRecordTitle(class),
		"--json",
	}
	return reviewartifact.Action{
		ID:             "record_autonomy_grant",
		Label:          "Grant it",
		Command:        shellCommand(argv),
		Argv:           argv,
		RequiresReview: true,
	}
}

// autonomyGrantMintRecordTitle is the --title the record carries. The console
// buildArgv must produce the identical title so the two mint paths agree.
func autonomyGrantMintRecordTitle(class string) string {
	return "Let proven " + plainAutonomyClass(class) + " work enter the queue on its own"
}

func autonomyGrantMintID(class string) string {
	return "agm_" + class
}

// plainAutonomyClass renders a class token for operator copy (job_health →
// job-health). Underscores are a machine convention; hyphens read as English.
func plainAutonomyClass(class string) string {
	return strings.ReplaceAll(strings.TrimSpace(class), "_", "-")
}
