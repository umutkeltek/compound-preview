// memory_cli_hints.go holds the operator-facing message constructors that
// `compound memory` verbs append to their normal output. They live in
// internal/mem (not in cmd/compound) so the cmd package stays thin and the
// LOC budget for cmd/compound keeps ratcheting down; the cobra/glue side
// only knows how to write the lines these helpers return.
//
// The packet that introduced these is work_3c70f011f6ba; the user-facing
// behavior is documented in cmd/compound/memory_cli_test.go (the end-to-end
// subprocess assertions). Unit tests for these helpers live in
// memory_cli_hints_test.go.
package mem

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"compound/internal/candidates"
)

// DoorCommand maps a candidates.Spec.Consumer value (a string that may be
// a Go package symbol like "capgraph.Consume") to the operator-facing CLI
// command. The translation is narrow on purpose: every entry must be tied
// to a real producer elsewhere in the codebase, and an unknown symbol
// falls through unchanged so the operator sees the string we actually have,
// not a guess. The cmd/compound packet replaces this with `compound
// capability consume` — which is the only entry today; a fresh entry is
// cheap to add when another arrives.
func DoorCommand(consumer string) string {
	switch consumer {
	case "capgraph.Consume":
		return "compound capability consume"
	default:
		return consumer
	}
}

// IdFromCandidatePrefix attempts to recover the candidate Kind from a
// minted-id prefix (e.g. "src_cand_xyz" -> capability_observation).
// Returns the empty kind when the prefix is unknown; callers must fall
// back to a generic note rather than fabricate a door.
//
// The mapping is intentionally narrow — every entry should be tied to a
// documented producer elsewhere in the codebase. Today the only known
// producer of cross-namespace ids is `compound source observe` (it mints
// capability_observation ids into inbox/capability-observations).
func IdFromCandidatePrefix(id string) candidates.Kind {
	switch {
	case strings.HasPrefix(id, "src_cand_"):
		return candidates.CapabilityObs
	default:
		return ""
	}
}

// KindCrossNamespacePointer returns a one-line hint pointing at the
// adjudication door for a kind that does NOT live in the memory inbox.
// Returns "" when kind is the memory kind itself, unknown, or already
// lives in the memory inbox (no cross-namespace hint needed).
//
// This is the operator's aid for packet work_3c70f011f6ba falsifier 3:
// a `--kind capability_observation` filter against the memory inbox
// always reports 0/N — the kind is real, it just doesn't live here.
// We never refuse the request; we surface the pointer and let the
// listing print.
func KindCrossNamespacePointer(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" || string(candidates.Memory) == kind {
		return ""
	}
	spec, ok := candidates.SpecFor(candidates.Kind(kind))
	if !ok {
		return ""
	}
	if spec.InboxDir == candidates.InboxDir(candidates.Memory) {
		return "" // same inbox, no cross-namespace hint needed
	}
	return fmt.Sprintf("# --kind %s is adjudicated via %s (inbox: %s, %d verdict(s) allowed)", kind, DoorCommand(spec.Consumer), spec.InboxDir, len(spec.Verdicts))
}

// MaybePrintCrossKindPointer writes the cross-namespace pointer line to
// w when KindCrossNamespacePointer returns a non-empty string. Output is
// intentionally NOT prefixed with a newline; the caller controls line
// ordering. Empty/blank kindFilter prints nothing (no hint, no noise).
func MaybePrintCrossKindPointer(w io.Writer, kindFilter string) {
	msg := KindCrossNamespacePointer(kindFilter)
	if msg == "" {
		return
	}
	fmt.Fprintln(w, msg)
}

// PrintAdjudicateUnknownID writes the operator-facing stderr block for the
// "operator asked to adjudicate an id the store could not resolve" error
// class. The store surfaces this as "not found: <id>.json" from readJSON
// at mem.go (internal/mem) for both the explicit-confirmation and the
// operator-ratification paths, and the CLI's job is to wrap that bare
// store message with enough context for an operator to act.
//
// The first line echoes the store's verbatim "not found: ..." message;
// the second names the searched inbox dir; the third points at the lookup
// aid; the fourth prints either the cross-namespace door (when the id's
// prefix matches a known producer) or a generic note about where to look
// next. This function does NOT call os.Exit — the cobra-side caller
// (cmdMemory's adjudicate case) decides the exit code (always 1 today:
// operator-facing fail, distinct from the empty-id usage class).
func PrintAdjudicateUnknownID(w io.Writer, id, storeErr string) {
	dir := filepath.Join("$COMPOUND_HOME", candidates.InboxDir(candidates.Memory))
	fmt.Fprintf(w, "error: %s\n", storeErr)
	fmt.Fprintf(w, "  searched: %s/%s.json\n", dir, id)
	fmt.Fprintln(w, "  try: `compound memory candidates --state proposed` to list current ids")
	hint := KindCrossNamespacePointer(string(IdFromCandidatePrefix(id)))
	if hint != "" {
		fmt.Fprintf(w, "  note: %s\n", hint)
	} else {
		fmt.Fprintln(w, "  note: ids minted by other producers (e.g. `compound source observe` -> `src_cand_*`) live in other inboxes; run `compound memory candidates` with no --kind for the cross-namespace list footer")
	}
}
