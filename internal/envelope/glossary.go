package envelope

// EP-U1 plain-language register — the Go side owns the canonical plain
// register. The console's move-types module (console/src/lib/move-types.ts)
// re-exports the same one-liners, and console/scripts/move-type-drift-smoke.mjs
// guards Go <-> console divergence so neither side can drift silently.

import "sort"

// PlainPlace is one of the five places the runtime is held in (README "What It Is").
// Name is the canonical slug; Plain is the one-line definition an operator reads.
type PlainPlace struct {
	Name  string
	Plain string
}

// PlainPlaces is the canonical, ORDERED five places (door / ledger / edge /
// gate / receipt). Wording mirrors README.md "What It Is" so a place one-liner
// shown in the console never drifts from the front-door thesis line. The
// ordering is part of the contract: callers index by position (e.g. "the second
// place is the ledger") and the glossary_test pins the names + order.
var PlainPlaces = []PlainPlace{
	{
		Name:  "door",
		Plain: "Everything that enters — a task, an observation, a model's output — arrives as a typed envelope and is validated before it can matter.",
	},
	{
		Name:  "ledger",
		Plain: "Meaning lives in one adjudicated home: nothing becomes ambient truth without a recorded verdict, and supersession is a record, never a prose edit.",
	},
	{
		Name:  "edge",
		Plain: "Executors — models, harnesses, engines — are rented: they propose, extract, and execute, but never own truth, promotion, or policy.",
	},
	{
		Name:  "gate",
		Plain: "Every mutation asks policy first; destructive and irreversible actions stay with the human by construction.",
	},
	{
		Name:  "receipt",
		Plain: "After action, a receipt records what actually ran, and a transition records what it means for future behavior.",
	},
}

// MoveTypePlain is the one-liner per MoveTypes entry. Values MUST stay
// byte-equal to console/src/lib/move-types.ts MOVE_TYPE_GLOSSARY — the drift
// smoke asserts that. Wording is identical to the previous console
// MOVE_TYPE_GLOSSARY so the live transitions ticker shows the same text after
// the refactor. The display label is held separately in MoveTypeLabel so the
// canonical ("Control-plane repair" with a hyphen, not "Control plane
// repair") survives the Go <-> console round-trip.
var MoveTypePlain = map[string]string{
	"outward_delta":        "Shipped a real product change — the outward-facing move.",
	"memory_delta":         "Wrote new memory the system can recall later.",
	"rule_delta":           "Changed a rule, contract, or doctrine the system follows.",
	"control_plane_repair": "Fixed the control plane itself — no product or memory change.",
	"audit_only":           "Recorded for the audit trail; no state moved.",
}

// MoveTypeLabel is the canonical display label per MoveTypes entry. Values
// MUST stay byte-equal to console/src/lib/move-types.ts MOVE_TYPE_LABELS —
// the drift smoke asserts that. The labels are short headlines used in
// tables and selects; the one-liner in MoveTypePlain is the operator-readable
// line shown in tooltips and rollups.
var MoveTypeLabel = map[string]string{
	"outward_delta":        "Outward delta",
	"memory_delta":         "Memory delta",
	"rule_delta":           "Rule delta",
	"control_plane_repair": "Control-plane repair",
	"audit_only":           "Audit only",
}

// TermPlain is the one-liner per typed envelope / object term. Values match
// console/src/lib/glossary.ts TERM_GLOSSARY where entries overlap, so the
// console's client-side glossary stays aligned with the Go owner. The README
// typed-grammar block is the source for the new entries (ContextPack /
// WorkEnvelope / ActionIntent / PolicyDecision); the overlap entries
// (Receipt / Transition) keep the console's longer wording.
var TermPlain = map[string]string{
	"ContextPack":    "The briefing: what the agent is told, and what was left out.",
	"WorkEnvelope":   "The job: one stable identity for one unit of work.",
	"ActionIntent":   "The gate's input: what an executor wants to do and what it will cost.",
	"PolicyDecision": "The gate's answer: allow, deny, or hold.",
	"Receipt":        "Proof a job ran: what was touched, what it cost, what came out.",
	"Transition":     "The ledger entry: the system moved from state A to B because of this work.",
}

// PlainPlaceNames returns the ordered place slugs, a convenience for callers
// that only need the names (the drift smoke uses a similar idiom).
func PlainPlaceNames() []string {
	out := make([]string, 0, len(PlainPlaces))
	for _, p := range PlainPlaces {
		out = append(out, p.Name)
	}
	return out
}

// SortedMoveTypes returns MoveTypes in lexical order — a stable, sorted
// iteration surface for callers (tests, the drift smoke) that need to avoid
// map-iteration order.
func SortedMoveTypes() []string {
	out := make([]string, len(MoveTypes))
	copy(out, MoveTypes)
	sort.Strings(out)
	return out
}
