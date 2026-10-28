package harvest

import (
	"strings"

	"compound/internal/decisions"
	"compound/internal/mem"
)

// Outcome is how a classified utterance reconciles against the current accepted
// decision records — the three doors the harvest packet names.
type Outcome string

const (
	// OutcomeCorroboration: the utterance restates an accepted record. Evidence is
	// appended to that record; no card, no duplicate (decisions-down).
	OutcomeCorroboration Outcome = "corroboration"
	// OutcomeConflict: the utterance topically matches an accepted record but
	// reverses it (e.g. lifting a freeze). Surfaces loudly for the operator; never
	// a silent record.
	OutcomeConflict Outcome = "conflict"
	// OutcomeNovel: no accepted record matches. Becomes a proposed-only decision
	// candidate carrying the verbatim quote + provenance.
	OutcomeNovel Outcome = "novel"
)

// Match is the accepted record a classified utterance reconciled against.
type Match struct {
	ClaimID    string  `json:"claim_id,omitempty"`
	DecisionID string  `json:"decision_id,omitempty"`
	Object     string  `json:"object,omitempty"`
	Kind       string  `json:"kind,omitempty"`
	Score      float64 `json:"score,omitempty"`
	Anchors    int     `json:"anchors,omitempty"`
}

const (
	// matchCoverage is the fraction of an accepted record's distinctive tokens an
	// utterance must restate to count as the same standing law. Tuned precision-first.
	matchCoverage = 0.5
	// matchMinAnchors guards against a coincidental single-token overlap.
	matchMinAnchors = 2
)

// reversalMarkers flip a matched record: an utterance that topically matches an
// accepted record but carries one of these is a CONFLICT, not a corroboration.
var reversalMarkers = []string{
	"resume", "unfreeze", "un-freeze", "unblock", "lift the freeze", "lift freeze",
	"no longer frozen", "go ahead", "you can now", "reverse that", "changed my mind",
	"actually do", "scrap that", "ignore that rule", "reverse the",
}

// Reconcile maps a classified utterance to one of the three doors by meaning
// overlap against the current accepted decision records. Decision records are
// slot-keyed by decision_id (mem cluster machinery), and a fresh utterance has no
// id — so cross-record identity is resolved by distinctive-token overlap over
// each record's choice + falsifier + keywords, normalized through the same
// mem.NormalizeClaimValue the ledger uses (the vocabulary-mapping discipline).
// Conservative: an ambiguous utterance defaults to NOVEL, and a corroboration is
// never re-labelled a conflict unless an explicit reversal marker is present.
func Reconcile(text string, kind Kind, active []mem.Claim) (Outcome, Match) {
	uttTokens := significantTokens(text)
	if len(uttTokens) == 0 {
		return OutcomeNovel, Match{}
	}
	var best Match
	for _, claim := range active {
		if claim.ClaimType != "decision" {
			continue
		}
		// Coverage is measured against the decision CHOICE (object) only — the
		// operator's actual words. Explicit scope keywords (freeze_keywords/scope)
		// widen the anchor pool but never inflate the denominator, so the record's
		// boilerplate falsifier/review_trigger can't dilute a real restatement.
		coreTokens := significantTokens(claim.Object)
		if len(coreTokens) == 0 {
			continue
		}
		keyTokens := significantTokens(recordKeywords(claim))
		anchorPool := union(coreTokens, keyTokens)
		anchors := intersectionCount(anchorPool, uttTokens)
		if anchors == 0 {
			continue
		}
		coverage := float64(intersectionCount(coreTokens, uttTokens)) / float64(len(coreTokens))
		if anchors < matchMinAnchors || coverage < matchCoverage {
			continue
		}
		if coverage > best.Score {
			lc := decisions.LifecycleFromClaim(claim)
			best = Match{
				ClaimID:    claim.ID,
				DecisionID: lc.ID,
				Object:     claim.Object,
				Kind:       lc.Kind,
				Score:      coverage,
				Anchors:    anchors,
			}
		}
	}
	if best.ClaimID == "" {
		return OutcomeNovel, Match{}
	}
	if hasReversal(text, best) {
		return OutcomeConflict, best
	}
	return OutcomeCorroboration, best
}

// recordKeywords are a decision record's operator-authored scope tokens (freeze
// keywords + scope), used as extra match anchors — NOT its boilerplate lifecycle
// prose (falsifier/review_trigger), which would dilute a genuine restatement.
func recordKeywords(claim mem.Claim) string {
	if claim.Qualifiers == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	for _, k := range []string{"freeze_keywords", "scope"} {
		if v := strings.TrimSpace(claim.Qualifiers[k]); v != "" {
			parts = append(parts, strings.ReplaceAll(v, ",", " "))
		}
	}
	return strings.Join(parts, " ")
}

func union(a, b map[string]bool) map[string]bool {
	if len(b) == 0 {
		return a
	}
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func hasReversal(text string, m Match) bool {
	lower := strings.ToLower(text)
	for _, marker := range reversalMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// significantTokens normalizes through mem.NormalizeClaimValue (the ledger's own
// identity normalizer) then keeps distinctive tokens: length >= 4, minus common
// stopwords, plus a small keep-set of short-but-meaningful terms.
func significantTokens(text string) map[string]bool {
	norm := mem.NormalizeClaimValue(text)
	out := map[string]bool{}
	for _, raw := range strings.FieldsFunc(norm, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if tokenStopwords[raw] {
			continue
		}
		if len(raw) >= 4 || tokenKeep[raw] {
			out[raw] = true
		}
	}
	return out
}

func intersectionCount(a, b map[string]bool) int {
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	n := 0
	for tok := range small {
		if large[tok] {
			n++
		}
	}
	return n
}

// tokenKeep are short tokens that carry meaning in this domain.
var tokenKeep = map[string]bool{
	"db": true, "os": true, "ml": true, "ai": true, "ci": true, "cd": true,
	"pr": true, "ui": true, "ux": true, "go": true,
}

// tokenStopwords are high-frequency words that carry no identity, so they never
// count as an overlap anchor. Length->=4 words that are still noise are listed.
var tokenStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true, "with": true,
	"from": true, "into": true, "must": true, "will": true, "should": true, "shall": true,
	"never": true, "always": true, "only": true, "when": true, "then": true, "than": true,
	"over": true, "them": true, "they": true, "have": true, "been": true, "such": true,
	"onto": true, "your": true, "yours": true, "were": true, "what": true, "which": true,
	"there": true, "here": true, "make": true, "made": true, "keep": true, "kept": true,
	"stays": true, "stay": true, "gets": true, "get": true, "now": true, "not": true,
	"use": true, "used": true, "using": true, "some": true, "more": true, "most": true,
	"very": true, "just": true, "also": true, "each": true, "every": true, "any": true,
}
