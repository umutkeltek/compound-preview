// orient_adjacent_catalog.go owns the table-driven mapping that the orient
// operator report uses to surface "adjacent_progress" preflight/review
// actions on held work. It replaces the inline substring-scan block that
// used to live in orient_operator.go's orientAdjacentActionIDsForOpenWork.
//
// Why a typed catalog (vs. inline strings.Contains):
//   - The three clusters (asc/app-store, conversation/chatgpt,
//     second-opinion/oracle) map to six pinned action ids. The six ids are
//     CLI surface -- tests in orient_operator_test.go assert them by
//     substring (e.g. ascNext, conversationNext, secondOpinionNext). A
//     table makes the mapping auditable: one row per cluster, the action
//     id next to its keyword phrases, no branching tree to keep in sync.
//   - Future clusters add a row, not a new `if strings.Contains(haystack, ...)`
//     stanza. The dispatch loop is single-pass and order-preserving so
//     the output is byte-identical to the previous implementation.
//   - Candidate 2 (orient_operator.go -> internal/operatorreport/) will
//     lift this file to a typed sub-package alongside the orient report
//     types. Keeping the catalog in its own file gives a clean lift
//     target that doesn't drag the rest of the report builder.
//
// What stays the same:
//   - The exported function name (orientAdjacentActionIDsForOpenWork) and
//     its signature. The single caller in orient_operator.go (the
//     held-adjacent-progress next-action builder) does not change.
//   - The case-insensitivity: the haystack is still lowercased before
//     scan, and the catalog's keyword phrases are lowercase strings.
//   - The addIfPresent semantics: an action id is appended to the
//     selected list only if the caller supplied it. The catalog
//     identifies which actions are eligible, not which are present.
package main

import (
	"slices"
	"strings"

	"compound/internal/work"
)

// orientAdjacentActionEntry is one row of the catalog: a set of
// lowercase keyword phrases that match against the lowercased workbench
// haystack, and the action id to surface when at least one phrase hits.
type orientAdjacentActionEntry struct {
	// Keywords is a set of lowercase substrings. The haystack (the
	// join of SuccessPredicate + CanonicalSurface + ObservatoryTags,
	// lowercased) is checked for any-of membership; the first hit is
	// enough to surface the action id. Phrases are matched verbatim --
	// no tokenization, no word boundaries. That mirrors the prior
	// strings.Contains calls byte-for-byte.
	Keywords []string
	// ActionID is the candidate action id the orient report's
	// running.adjacent_progress list should highlight. It is added to
	// the selected slice only if the caller-supplied `actionIDs`
	// already contains it (so the catalog does not invent actions).
	ActionID string
}

// orientAdjacentActionCatalog is the typed replacement for the three
// `if strings.Contains(haystack, ...)` blocks in the old
// orientAdjacentActionIDsForOpenWork. Order matches the previous
// implementation (asc -> conversation -> second-opinion) so the
// generated next-action text is byte-identical and the pinning tests
// (TestOrientOperatorSurfacesHeldValveAdjacentProgress et al.) keep
// passing without churn.
var orientAdjacentActionCatalog = []orientAdjacentActionEntry{
	{
		// ASC + app-store keywords. Two eligible actions because the
		// held-work preflight and the sales-series bridge are both
		// needed on asc/app-store surfaces.
		Keywords: []string{"asc", "app-store", "app store"},
		ActionID: "asc-preflight-private-config",
	},
	{
		Keywords: []string{"asc", "app-store", "app store"},
		ActionID: "asc-sales-series-bridge-proof",
	},
	{
		// ChatGPT / conversation keywords. Both the private-export
		// preflight and the memory-review gate apply to held
		// conversation work.
		Keywords: []string{"conversation", "chatgpt"},
		ActionID: "conversation-preflight-private-export",
	},
	{
		Keywords: []string{"conversation", "chatgpt"},
		ActionID: "conversation-memory-review-gate",
	},
	{
		// Second-opinion / oracle keyword. Single action today; if a
		// second one lands, add another row, do not extend the
		// Keyword slice.
		Keywords: []string{"second-opinion", "oracle"},
		ActionID: "second-opinion-evidence-recovery",
	},
}

// orientAdjacentActionIDsForOpenWork returns the subset of the caller-
// supplied actionIDs that the orient operator report should surface as
// adjacent-progress preflight/review options for the given work item.
//
// The selection is driven by orientAdjacentActionCatalog: each row
// declares a set of keyword phrases; if any phrase is found in the
// lowercased haystack built from the workbench item's identity
// fields, the row's ActionID is added to the result (if the caller
// already supplied it via actionIDs).
//
// The haystack is the join (single space separator) of:
//   - item.SuccessPredicate
//   - item.CanonicalSurface
//   - item.ObservatoryTags (also single-space joined)
//
// This is the typed replacement for the previous
// strings.Contains-based implementation. Output ordering and content
// are byte-identical: the catalog is walked top-to-bottom, and
// duplicates are filtered via the `seen` map (the prior code did not
// explicitly dedupe because each block targeted a different action
// id, but the catalog form makes dedupe mandatory since multiple
// rows can share a keyword cluster).
func orientAdjacentActionIDsForOpenWork(item work.WorkbenchItem, actionIDs []string) []string {
	haystack := strings.ToLower(strings.Join([]string{
		item.SuccessPredicate,
		item.CanonicalSurface,
		strings.Join(item.ObservatoryTags, " "),
	}, " "))
	var selected []string
	seen := make(map[string]struct{})
	addIfPresent := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		if slices.Contains(actionIDs, id) {
			seen[id] = struct{}{}
			selected = append(selected, id)
		}
	}
	for _, entry := range orientAdjacentActionCatalog {
		if haystackContainsAny(haystack, entry.Keywords) {
			addIfPresent(entry.ActionID)
		}
	}
	return selected
}

// haystackContainsAny reports whether haystack contains any of the
// given literal substrings. Empty `phrases` returns false (an empty
// keyword cluster never matches -- the previous code's three
// conditions were non-empty strings.Contains checks, so the catalog
// preserves that).
func haystackContainsAny(haystack string, phrases []string) bool {
	for _, phrase := range phrases {
		if phrase == "" {
			continue
		}
		if strings.Contains(haystack, phrase) {
			return true
		}
	}
	return false
}
