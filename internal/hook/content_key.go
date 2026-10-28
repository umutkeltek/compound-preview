// content_key.go — the v2 additive detector's SECOND deterministic match key (see
// consumption.go's header for the OR-composition this feeds: exact-id OR content-key, never a
// replacement). A content key is a stable, versioned, order-independent token set derived from a
// claim's entity/predicate/object — a PURE function of the claim, no LLM, no fuzz: the same claim
// always derives the same key. Matching (window scan + echo exclusion) lives in consumption.go;
// this file only derives the key and gates its eligibility so a low-specificity claim never
// becomes a content-key match candidate at all — an ineligible claim's consumption falls back to
// v1 exact-id alone, the packet's fail-safe boundary.
//
// Derivation:
//  1. STRUCTURED input preferred: DeriveContentKey(entity, predicate, object) is the primary pure
//     function, taking a claim's three fields directly (the mem.Claim shape) when the caller
//     already has them structured.
//  2. RENDERED-TEXT fallback: DeriveContentKeyFromLine(line) parses a claim's rendered
//     "entity.predicate = object" text — exactly what contract.memoryLine renders and what
//     consumption.go reads back out of ContextPack.Included[].Reason, since that is the only form
//     consumption.go's own data flow ever has in hand — by splitting on the FIRST " = " into
//     lhs/rhs, then splitting lhs on its LAST "." into entity-path/predicate, then deferring to
//     DeriveContentKey. A line with no " = " (or an lhs with no ".") cannot be parsed and returns
//     an ineligible zero key, never an error: parse failure is just another way to be ineligible.
//  3. Normalization: Unicode NFC first (so a precomposed vs. decomposed accented character
//     normalizes identically — "same claim -> same key" holds across Unicode representations),
//     then lowercase, then keep [a-z0-9_] runs whole (snake_case tokens like source_host survive
//     intact) and turn every other run into a single space, then split on whitespace. NFC
//     deliberately does not decompose-and-strip accents down to their bare Latin letter: a
//     precomposed accented letter falls outside the ascii-only keep-set and is dropped like any
//     other punctuation, not silently rewritten into a token that could then match — a
//     multilingual/paraphrased reference is an accepted MISS (see the held-out fixture pinning
//     this), never a loophole into passing token matches.
//  4. Stop-words and tokens shorter than 3 bytes are dropped from every slot. Salient-object
//     tokens are object tokens minus stop-words minus entity/predicate tokens (cross-field
//     dedupe) — an object token that only repeats the entity or predicate carries no independent
//     signal and must not count twice toward eligibility/matching.
//
// Eligibility gate (fail-safe, named constants below): a claim short of ANY bar here is
// content-key-DEAD — matchTargetHitsLine (consumption.go) then relies on exact-id alone for it.
package hook

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ContentKeyDerivationVersion names this file's derivation rules (mirrors the versioned-converter
// pattern of bench.IDMapV1/IDMapV2 — see internal/bench/convert.go). Bump it, and add a new
// versioned block alongside this one, if the token/eligibility rules ever change; existing
// fixtures pin the CURRENT version's output as a byte-stable falsifier, so changing behavior
// in place under the same version string would silently invalidate that guarantee.
const ContentKeyDerivationVersion = "content_key_v1"

// Eligibility gate thresholds (named constants, not magic numbers — see the doc block above).
const (
	contentKeyMinDistinctTokens = 3  // >= this many distinct tokens across all three slots
	contentKeyMinSummedTokenLen = 12 // summed length of those distinct tokens
	contentKeyMinLongTokenLen   = 6  // >= 1 token this long (or containing a digit) is required
	contentKeyMinTokenLen       = 3  // shorter tokens are dropped from every slot outright
)

// contentKeyStopWords are dropped from every slot before eligibility/matching — generic English
// function words carry no claim-specific signal and would otherwise inflate cross-claim
// collisions (the packet's falsifier d: no cross-credit between claims sharing common words).
var contentKeyStopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true, "to": true,
	"in": true, "on": true, "is": true, "are": true, "was": true, "were": true, "be": true,
	"for": true, "with": true, "at": true, "by": true, "from": true, "as": true, "it": true,
	"this": true, "that": true, "has": true, "have": true, "had": true, "not": true, "no": true,
	"but": true, "if": true, "into": true, "than": true, "then": true, "so": true, "its": true,
	"can": true, "will": true, "would": true, "should": true, "could": true, "any": true,
}

// ContentKey is a claim's derived, order-independent content key: three deduped+sorted token
// slots plus the eligibility verdict. An ineligible key (Eligible == false) carries no matching
// power at all — see contentKeyEligible.
type ContentKey struct {
	Version             string
	EntityTokens        []string
	PredicateTokens     []string
	SalientObjectTokens []string
	Eligible            bool
}

// AllTokens returns the sorted, deduped union of all three slots — the set the eligibility
// gate's distinct-token-count/summed-length checks run over.
func (k ContentKey) AllTokens() []string {
	set := map[string]bool{}
	for _, t := range k.EntityTokens {
		set[t] = true
	}
	for _, t := range k.PredicateTokens {
		set[t] = true
	}
	for _, t := range k.SalientObjectTokens {
		set[t] = true
	}
	return sortedKeys(set)
}

// DeriveContentKey is the pure, versioned content-key derivation over a claim's STRUCTURED fields
// — the preferred entry point when the caller already has them (mirrors mem.Claim's
// Entity/Predicate/Object). Same three strings -> byte-identical ContentKey, always.
func DeriveContentKey(entity, predicate, object string) ContentKey {
	entityTokens := dedupeSortedTokens(normalizeTokens(entity))
	predicateTokens := dedupeSortedTokens(normalizeTokens(predicate))

	entitySet := tokenSet(entityTokens)
	predicateSet := tokenSet(predicateTokens)

	var salient []string
	for _, t := range normalizeTokens(object) {
		if entitySet[t] || predicateSet[t] {
			continue // cross-field dedupe: no independent signal beyond entity/predicate
		}
		salient = append(salient, t)
	}

	key := ContentKey{
		Version:             ContentKeyDerivationVersion,
		EntityTokens:        entityTokens,
		PredicateTokens:     predicateTokens,
		SalientObjectTokens: dedupeSortedTokens(salient),
	}
	key.Eligible = contentKeyEligible(key)
	return key
}

// DeriveContentKeyFromLine parses a claim's rendered "entity.predicate = object" text (the
// fallback path — see the file doc block) and derives its content key. ok=false means the line
// could not be parsed into entity/predicate/object at all (no " = ", or no "." in the lhs); the
// returned key is the zero value and MUST be treated as ineligible, never matched.
func DeriveContentKeyFromLine(line string) (key ContentKey, ok bool) {
	lhs, rhs, found := strings.Cut(line, " = ")
	if !found {
		return ContentKey{}, false
	}
	lastDot := strings.LastIndex(lhs, ".")
	if lastDot < 0 {
		return ContentKey{}, false
	}
	entity := lhs[:lastDot]
	predicate := lhs[lastDot+1:]
	return DeriveContentKey(entity, predicate, rhs), true
}

// contentKeyEligible is the fail-safe eligibility gate (packet-named floor): a key with anything
// less specific than every one of these bars stays content-key-dead so the detector's OR degrades
// to exact-id alone for that claim.
func contentKeyEligible(k ContentKey) bool {
	if len(k.EntityTokens) == 0 || len(k.PredicateTokens) == 0 || len(k.SalientObjectTokens) == 0 {
		return false
	}
	all := k.AllTokens()
	if len(all) < contentKeyMinDistinctTokens {
		return false
	}
	summed := 0
	hasQualifyingToken := false
	for _, t := range all {
		summed += len(t)
		if len(t) >= contentKeyMinLongTokenLen || containsDigit(t) {
			hasQualifyingToken = true
		}
	}
	if summed < contentKeyMinSummedTokenLen {
		return false
	}
	return hasQualifyingToken
}

// containsDigit reports whether s contains an ASCII digit (part of the eligibility gate's
// "length >= 6 OR contains a digit" qualifying-token test).
func containsDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

// isKeyTokenRune reports whether r survives normalization whole ([a-z0-9_] — input is already
// lowercased by the time this runs). Every other rune, ASCII or not, becomes a token-breaking
// space — see the file doc block's NFC-then-ascii-filter rationale.
func isKeyTokenRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
}

// normalizeTokens implements derivation steps 3+4 from the file doc block: NFC -> lowercase ->
// keep [a-z0-9_] runs whole, other runs -> space -> split on whitespace -> drop stopwords/tokens
// shorter than contentKeyMinTokenLen. Returned tokens are NOT yet deduped/sorted (callers that
// need a set do that themselves — DeriveContentKey needs the raw object token list before
// cross-field dedupe, so deduping here would be premature).
func normalizeTokens(s string) []string {
	s = norm.NFC.String(s)
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isKeyTokenRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	fields := strings.Fields(b.String())
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < contentKeyMinTokenLen || contentKeyStopWords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// tokenSet turns a token slice into a membership set.
func tokenSet(xs []string) map[string]bool {
	set := make(map[string]bool, len(xs))
	for _, x := range xs {
		set[x] = true
	}
	return set
}

// dedupeSortedTokens dedupes and sorts a token slice, reusing session_transcript.go's sortedKeys
// so this package has one canonical set->sorted-slice helper.
func dedupeSortedTokens(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	return sortedKeys(tokenSet(xs))
}
