package harvest

import "strings"

// Kind is the operational decision shape a classified operator utterance carries.
// It maps 1:1 onto the decision-record kinds `compound decisions record` writes
// (grant | freeze | ruling), so a harvested candidate flows through the exact
// same lifecycle as a hand-recorded one.
type Kind string

const (
	KindRuling Kind = "ruling"
	KindGrant  Kind = "grant"
	KindFreeze Kind = "freeze"
)

// Classification is the gate's verdict on one utterance: the decision kind plus
// the marker phrases that fired, kept for explainability (the operator sees WHY
// a sentence was proposed as standing law).
type Classification struct {
	Kind    Kind     `json:"kind"`
	Signals []string `json:"signals"`
}

// standingLawMarkers are the normative/standing-law phrasings that mark an
// utterance as an imperative about how things WILL be, not a one-off request.
// Precision over recall: an utterance with none of these never classifies.
var standingLawMarkers = []string{
	"from now on", "from here on", "going forward", "moving forward", "henceforth",
	"by default", "as a rule", "the rule is", "rule:", "settled", "decided",
	"never again", "no longer", "always", "never", "must never", "must always",
	"should always", "should never", "don't ever", "do not ever", "stop ",
	"only when", "only inside", "only within", "prefer ", " over ",
	"ruling:", "policy:", "authori", "grant ", "freeze", "from now",
}

// systemBehaviorMarkers mark the SUBJECT as the system / agents / harness — i.e.
// how the machine should behave — as opposed to a concrete artifact or task.
var systemBehaviorMarkers = []string{
	"agent", "agents", "subagent", "executor", "execution", "execute",
	"lane", "lanes", "codex", "opus", "sonnet", "fable", "model", "models",
	"route", "routing", "orchestrat", "memory", "recall", "orient", "harness",
	"the system", "doctrine", "policy", "promote", "promotion", "decision",
	"judge", "judging", "autonomy", "envelope", "permission", "spine",
	"gbrain", "brain", "compound", "training", "batch", "fedora", "premium",
	"subscription", "provider", "cron", "skill", "skills", "capability",
}

// taskVerbHeads are imperative verbs that head a concrete task instruction. They
// only veto when NO standing-law marker is present (a real ruling like "never run
// training on this Mac" legitimately contains a task-ish verb).
var taskVerbHeads = []string{
	"fix", "add", "implement", "refactor", "run", "write", "create", "build",
	"debug", "test", "rename", "delete", "remove", "update", "change", "make",
	"install", "deploy", "merge", "commit", "revert", "check", "review", "open",
}

// maxRulingLen caps how long a classifiable ruling can be. A standing-law
// imperative is a crisp sentence; a multi-hundred-character block is an essay, a
// skill body, or a strategy dump — classifying those produced coincidental
// false grants in the live 2026-07-11 dry-run. Precision over recall: an operator
// ruling buried in an essay is missed (costs a restatement), never asserted.
const maxRulingLen = 360

// Classify is the deterministic ruling gate. It returns (Classification, true)
// ONLY for an operator utterance that is a standing-law imperative about how the
// SYSTEM should behave — a ruling / grant / freeze — and never for a task
// instruction. No LLM is in this path (binding constraint 2): the shape signals
// are encoded as testable rules, conjunctive so a false positive needs BOTH a
// standing-law phrasing AND a system-behavior subject.
func Classify(text string) (Classification, bool) {
	collapsed := strings.Join(strings.Fields(text), " ")
	if len([]rune(collapsed)) > maxRulingLen {
		return Classification{}, false
	}
	lower := " " + strings.ToLower(collapsed) + " "

	var lawSignals []string
	for _, m := range standingLawMarkers {
		if strings.Contains(lower, m) {
			lawSignals = append(lawSignals, strings.TrimSpace(m))
		}
	}
	var sysSignals []string
	for _, m := range systemBehaviorMarkers {
		if strings.Contains(lower, m) {
			sysSignals = append(sysSignals, m)
		}
	}
	if len(lawSignals) == 0 || len(sysSignals) == 0 {
		return Classification{}, false
	}
	// A bare task instruction ("fix the sync bug") has no standing-law marker and
	// is already rejected above; the head-verb veto only matters for an utterance
	// that tripped a weak law marker but is still just a task. Since a real ruling
	// carries a standing-law marker, we veto only when the utterance is head-verb
	// task-shaped AND its ONLY law signal is a weak one.
	if isPureTaskInstruction(lower, lawSignals) {
		return Classification{}, false
	}

	signals := append(append([]string{}, lawSignals...), sysSignals...)
	return Classification{Kind: classifyKind(lower), Signals: signals}, true
}

func isPureTaskInstruction(lower string, lawSignals []string) bool {
	// Only weak law signals (a bare "stop ", "make", " over ") can be a false
	// trip; strong standing-law phrasings below are never task-shaped.
	for _, s := range lawSignals {
		switch s {
		case "from now on", "from here on", "going forward", "moving forward",
			"henceforth", "by default", "as a rule", "the rule is", "rule:",
			"settled", "decided", "never again", "no longer", "always", "never",
			"must never", "must always", "should always", "should never",
			"ruling:", "policy:", "only when", "only inside", "only within":
			return false
		}
	}
	first := firstWord(lower)
	for _, v := range taskVerbHeads {
		if first == v {
			return true
		}
	}
	return false
}

func firstWord(lower string) string {
	fields := strings.Fields(lower)
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], ".,:;!?")
}

func classifyKind(lower string) Kind {
	switch {
	case strings.Contains(lower, "freeze") || strings.Contains(lower, "do not touch") ||
		strings.Contains(lower, "don't touch") || strings.Contains(lower, "hold off") ||
		strings.Contains(lower, "pause ") || strings.Contains(lower, "halt "):
		return KindFreeze
	case strings.Contains(lower, "grant") || strings.Contains(lower, "authori") ||
		strings.Contains(lower, "allow ") || strings.Contains(lower, "permit "):
		return KindGrant
	default:
		return KindRuling
	}
}
