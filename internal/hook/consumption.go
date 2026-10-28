// consumption.go — the session-end consumption detector (the Reflector's food supply).
//
// A session injects claims (ContextPack.included[] carries claim://<id>). At session end this
// detector answers which injected claims the transcript referenced. A citation, quotation,
// or refutation is reference evidence only; it never proves application or task effect.
//
// Design rules (v1, deliberately narrow — still fully live, untouched by v2 below):
//   - JOIN PATH (single): the pack is resolved from the CLOSING WORK's context_pack_id. That is
//     exactly the id the close receipt stamps into ids.context_pack_id, which is the id the usage
//     projection (capgraph.BuildCapabilityUsage) joins its consumed/ignored attribution on — so
//     detection and attribution share ONE pack, never two. fx.SessionID is carried on the hook
//     event for session provenance; a session-scan fallback stays a regex-only, claim-text-free
//     path (transcriptInjectedClaimRefs, session_transcript.go) precisely to keep the single join
//     path — it never resolves a pack, so a v2 content-key never has claim text to work from
//     there and correctly stays ineligible.
//   - DETECTOR (exact-match, no fuzz): a claim counts CONSUMED iff its exact id token (the part
//     after claim://) appears in the transcript as a BARE reference — a token occurrence that is
//     NOT part of the scheme-qualified injected form "claim://<id>". The injection renderer always
//     writes the scheme-qualified ref, so scheme-qualified occurrences are injection echo and are
//     excluded; a bare <id> elsewhere is a genuine reference. This delimits the injection block by
//     FORM rather than byte-offset — position-independent yet strictly self-echo-safe (no credit
//     for the id merely appearing in the injection). Token boundaries are enforced so one claim id
//     that is a prefix of another cannot cross-credit. No LLM, no token-overlap heuristics.
//   - UNDER-DETECTION IS SAFE: a missed reference remains UNKNOWN. A readable transcript with
//     no match is not a declaration of non-use.
//   - FAIL-OPEN: a missing/unreadable transcript yields (nil, declared=false) and never errors, so
//     session end is never blocked by the detector.
//
// v2 ADDITIVE match (content-key, ORed with exact-id — see content_key.go for derivation):
//   - OR, never AND, never a replacement: a claim counts CONSUMED iff exact-id fires OR its
//     content key fires. Every v1 guarantee above holds unmodified; v2 can only ever RAISE
//     recall, never lower it, and never changes v1's own verdict for any claim.
//   - CONTENT-KEY MATCH: three-slot conjunction (never any-2-of-3) — at least one entity token,
//     one predicate token, and one salient-object token must all occur, each at isTokenByte
//     boundaries and case-folded, inside ONE window of <= contentKeyWindowBytes on a SINGLE
//     transcript line (a window never crosses the line boundary the transcript streams by, which
//     is itself one full harness message/JSONL record, not a rendered sub-line — see
//     session_transcript.go). An ineligible content key (content_key.go's fail-safe gate) never
//     matches at all; that claim's consumption then rests on exact-id alone, exactly as in v1.
//   - ECHO EXCLUSION EXTENDS to the content-key form: every BYTE-EXACT occurrence, anywhere in the
//     line (not just the original injection offset — a later verbatim requote of the injected
//     digest is excluded too), of the claim's rendered "entity.predicate = object" text is masked
//     out before token search, so a claim can never self-credit its own injected/requoted text.
//     KNOWN GAP (accepted, not silently swallowed): a REFLOWED, non-byte-exact requote (rewrapped
//     whitespace, re-punctuated, etc.) is NOT caught by this byte-exact mask and could in
//     principle self-credit — TestSessionEndContentKeyRequotedDigestNotConsumed pins the
//     byte-exact case as covered; there is no fixture for the reflowed case because it is an
//     accepted miss of the exclusion, not a guarantee.
package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/envelope"
)

// claimScheme is the ref scheme injected ledger claims carry in ContextPack.included[].
const claimScheme = "claim://"

func observationQuality(readable bool) string {
	if readable {
		return "readable"
	}
	return "unobservable"
}

// detectSessionConsumption reports injected claim refs mentioned in a readable transcript.
// The boolean means the transcript was readable; no match leaves application and non-use
// unknown. An absent or unreadable transcript yields (nil, false) without blocking close.
func detectSessionConsumption(deps Deps, fx Fixture) (consumed []string, declared bool) {
	path := strings.TrimSpace(fx.TranscriptPath)
	if path == "" {
		return nil, false
	}
	consumed, declared, err := detectTranscriptConsumption(path, injectedClaimRefs(deps, fx))
	if err != nil {
		return nil, false
	}
	return consumed, declared
}

func detectTranscriptConsumption(path string, injectedRefs []injectedClaim) (consumed []string, declared bool, err error) {
	refs := dedupeClaimRefs(injectedRefs)
	if len(refs) == 0 {
		ids, err := transcriptInjectedClaimRefs(path)
		if err != nil {
			return nil, false, err
		}
		if len(ids) == 0 {
			return nil, true, nil
		}
		// The regex-only fallback never resolves a pack, so it has no claim text: Line stays ""
		// and content-key derivation for these refs is ineligible by construction (see
		// content_key.go — DeriveContentKeyFromLine("") always returns ok=false). Exact-id, which
		// is how these ids were found in the first place, still applies unmodified.
		refs = make([]injectedClaim, 0, len(ids))
		for _, id := range ids {
			refs = append(refs, injectedClaim{Ref: id})
		}
	}
	consumed, err = matchConsumedClaimsFromFile(path, refs)
	if err != nil {
		return nil, false, err
	}
	return consumed, true, nil
}

// injectedClaim pairs an injected claim ref with the claim TEXT the pack carries for it (the
// "entity.predicate = object" line extracted from IncludedRef.Reason — see claimContentLine).
// Line is "" when the pack carries no such text for this ref (a non-memory-ledger included ref, a
// bare test-fixture stub reason, or the regex-only fallback path with no pack at all): an empty
// Line makes DeriveContentKeyFromLine fail to parse, so the derived content key is simply
// ineligible and this claim's consumption falls back to exact-id alone — never an error, never a
// panic, just the fail-safe eligibility gate doing its job.
type injectedClaim struct {
	Ref  string
	Line string
}

func dedupeClaimRefs(refs []injectedClaim) []injectedClaim {
	seen := map[string]bool{}
	out := make([]injectedClaim, 0, len(refs))
	for _, ic := range refs {
		ref := strings.TrimSpace(ic.Ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, injectedClaim{Ref: ref, Line: ic.Line})
	}
	return out
}

// claimLedgerReasonPrefixes are the IncludedRef.Reason prefixes internal/contract's memory-ref
// builder stamps on a ledger claim / folded position (see contract.memoryLine and its callers —
// the same convention hook.go's activeMemoryLine relies on for rendering). Duplicated here rather
// than called across files so consumption.go's content-key input stays self-contained and
// independently reviewable within this file's own scope.
var claimLedgerReasonPrefixes = []string{
	"Compound memory ledger claim;",
	"Compound memory folded position;",
}

// claimContentLine extracts a claim's rendered "entity.predicate = object" text from an
// IncludedRef.Reason string, iff reason carries one of the known ledger-claim/position prefixes.
// Any other reason (a non-memory included ref, or a bare stub like a v1 test fixture's "recency")
// yields "" — deliberately stricter than hook.go's activeMemoryLine (which falls back to the raw
// reason for display purposes): here, "" is not a rendering fallback, it is the honest signal
// that no content-key text exists for this ref, so content-key derivation stays ineligible.
func claimContentLine(reason string) string {
	reason = strings.TrimSpace(reason)
	for _, prefix := range claimLedgerReasonPrefixes {
		if strings.HasPrefix(reason, prefix) {
			if _, line, ok := strings.Cut(reason, ":"); ok {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// injectedClaimRefs resolves the injected claim:// refs for the closing work via its
// context_pack_id — the single join path shared with the usage projection. Any resolution miss
// (no store/home, work not found, no pack, unreadable/foreign pack) yields no refs, so the
// detector degrades to a declared-but-empty (honest all-ignored) signal rather than erroring.
func injectedClaimRefs(deps Deps, fx Fixture) []injectedClaim {
	if deps.Store == nil || strings.TrimSpace(deps.HomePath) == "" {
		return nil
	}
	w, err := deps.Store.Get(fx.WorkID)
	if err != nil {
		return nil
	}
	packID := strings.TrimSpace(w.IDs.ContextPackID)
	if packID == "" {
		return nil
	}
	pack := loadContextPackFile(deps.HomePath, packID)
	if pack == nil {
		return nil
	}
	seen := map[string]bool{}
	var refs []injectedClaim
	for _, inc := range pack.Included {
		ref := strings.TrimSpace(inc.Ref)
		if !strings.HasPrefix(ref, claimScheme) || seen[ref] {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(ref, claimScheme)) == "" {
			continue
		}
		seen[ref] = true
		refs = append(refs, injectedClaim{Ref: ref, Line: claimContentLine(inc.Reason)})
	}
	return refs
}

// loadContextPackFile decodes a ContextPack artifact by id. It only needs the included refs, so a
// decode failure (torn/foreign file) returns nil rather than bricking the close.
func loadContextPackFile(homePath, packID string) *envelope.ContextPack {
	path := filepath.Join(homePath, "context", "packs", packID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pack envelope.ContextPack
	if json.Unmarshal(data, &pack) != nil {
		return nil
	}
	return &pack
}

// matchConsumedClaims returns the injected refs consumed under v1 (exact-id) OR v2 (content-key)
// — see claimMatchTarget/matchTargetHitsLine below. A window never crosses a line boundary, so the
// transcript is scanned one line at a time; this whole-string entry point simply splits on "\n"
// (the streamed-file entry point, matchConsumedClaimsFromFile in session_transcript.go, shares the
// same per-target/per-line matching so the two never drift). Order follows injectedRefs;
// duplicates are impossible because callers dedupe first.
func matchConsumedClaims(injectedRefs []injectedClaim, transcript string) []string {
	if len(injectedRefs) == 0 || transcript == "" {
		return nil
	}
	targets := buildClaimMatchTargets(injectedRefs)
	consumedSet := map[string]bool{}
	for _, line := range strings.Split(transcript, "\n") {
		for i := range targets {
			if consumedSet[targets[i].Ref] {
				continue
			}
			if matchTargetHitsLine(targets[i], line) {
				consumedSet[targets[i].Ref] = true
			}
		}
	}
	var consumed []string
	for _, t := range targets {
		if consumedSet[t.Ref] {
			consumed = append(consumed, t.Ref)
		}
	}
	return consumed
}

// bareClaimReference reports whether id occurs in s as a whole token that is NOT the tail of the
// scheme-qualified injected form claim://<id>. Exact substring scan with word boundaries; the
// scheme prefix immediately before an occurrence marks it as injection echo (excluded).
func bareClaimReference(s, id string) bool {
	for from := 0; ; {
		i := strings.Index(s[from:], id)
		if i < 0 {
			return false
		}
		idx := from + i
		schemeStart := idx - len(claimScheme)
		injectionEcho := schemeStart >= 0 && s[schemeStart:idx] == claimScheme
		leftBoundary := idx == 0 || !isTokenByte(s[idx-1])
		rightBoundary := idx+len(id) >= len(s) || !isTokenByte(s[idx+len(id)])
		if !injectionEcho && leftBoundary && rightBoundary {
			return true
		}
		from = idx + 1
	}
}

// isTokenByte reports whether b is part of an identifier token ([A-Za-z0-9_]); token boundaries
// keep one claim id that is a prefix of another (claim_ab vs claim_abc) from cross-crediting.
func isTokenByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '_':
		return true
	default:
		return false
	}
}

// contentKeyWindowBytes bounds the co-occurrence window (packet: "<=200 chars (or one line)") —
// content-key match never credits tokens found further apart than this on the same line.
const contentKeyWindowBytes = 200

// claimMatchTarget precomputes everything one injected claim needs for BOTH match paths — exact-
// id (v1) and content-key (v2, additive) — once per session, so per-line scanning is pure lookup,
// no repeated derivation.
type claimMatchTarget struct {
	Ref          string
	ID           string     // exact-id path: claimScheme-stripped id (v1, unchanged)
	Key          ContentKey // content-key path; Key.Eligible == false means it is DEAD for this claim
	RenderedLine string     // this ref's injected claim text (echo-exclusion span); "" if none
}

// buildClaimMatchTargets derives each injected claim's match target ONCE. A ref whose id is empty
// is skipped (defensive; injectedClaimRefs never emits one). Content-key derivation failure
// (DeriveContentKeyFromLine ok=false — e.g. no claim text available) yields Key.Eligible == false,
// so that claim's OR simply degrades to its exact-id disjunct, per the packet's eligibility gate.
func buildClaimMatchTargets(injected []injectedClaim) []claimMatchTarget {
	targets := make([]claimMatchTarget, 0, len(injected))
	for _, ic := range injected {
		id := strings.TrimPrefix(ic.Ref, claimScheme)
		if id == "" {
			continue
		}
		key, _ := DeriveContentKeyFromLine(ic.Line)
		targets = append(targets, claimMatchTarget{Ref: ic.Ref, ID: id, Key: key, RenderedLine: ic.Line})
	}
	return targets
}

// matchTargetHitsLine reports whether t is consumed by line: v1 exact-id OR v2 content-key (OR
// semantics — the packet's additive contract). Exact-id is tried first purely because it is the
// cheaper check; either firing is sufficient.
func matchTargetHitsLine(t claimMatchTarget, line string) bool {
	if bareClaimReference(line, t.ID) {
		return true
	}
	return contentKeyMatch(t.Key, t.RenderedLine, line)
}

// contentKeyMatch reports whether key's three slots co-occur inside one <=contentKeyWindowBytes
// window of line, entirely outside any span where line contains renderedLine byte-exact (the
// injection-echo/requote exclusion — see this file's v2 header doc block). Every occurrence is
// token-boundary-checked (isTokenByte) and case-folded, mirroring bareClaimReference's discipline
// exactly: content-key match is exact-id's sibling, not a fuzzier cousin.
func contentKeyMatch(key ContentKey, renderedLine, line string) bool {
	if !key.Eligible {
		return false
	}
	masked := strings.ToLower(maskByteExact(line, renderedLine))

	entityPos := tokenPositions(masked, key.EntityTokens)
	if len(entityPos) == 0 {
		return false
	}
	predicatePos := tokenPositions(masked, key.PredicateTokens)
	if len(predicatePos) == 0 {
		return false
	}
	objectPos := tokenPositions(masked, key.SalientObjectTokens)
	if len(objectPos) == 0 {
		return false
	}
	return windowCoversThreeSlots(entityPos, predicatePos, objectPos, contentKeyWindowBytes)
}

// maskByteExact replaces every BYTE-EXACT occurrence of form in s with spaces of the same length
// (length-preserving, so real content on either side of a masked span is neither joined together
// nor pulled artificially closer for window-distance purposes). form == "" is a no-op — an empty
// RenderedLine (no claim text available) has nothing to exclude. This runs at every location in
// the line, not just an original injection offset, so a later byte-exact requote is masked too;
// see the file's v2 header doc block for the accepted reflowed-requote gap this does NOT cover.
func maskByteExact(s, form string) string {
	if form == "" {
		return s
	}
	return strings.ReplaceAll(s, form, strings.Repeat(" ", len(form)))
}

// tokenPositions returns every isTokenByte-bounded occurrence of any token in tokens within text
// (already lowercased by the caller), sorted ascending. Positions only — which token matched does
// not matter for the window-conjunction check below, only that some token from this slot did.
func tokenPositions(text string, tokens []string) []int {
	var positions []int
	for _, tok := range tokens {
		for from := 0; ; {
			i := strings.Index(text[from:], tok)
			if i < 0 {
				break
			}
			idx := from + i
			leftBoundary := idx == 0 || !isTokenByte(text[idx-1])
			rightBoundary := idx+len(tok) >= len(text) || !isTokenByte(text[idx+len(tok)])
			if leftBoundary && rightBoundary {
				positions = append(positions, idx)
			}
			from = idx + 1
		}
	}
	sort.Ints(positions)
	return positions
}

// slotHit is one token-position occurrence tagged with which of the three content-key slots it
// belongs to (0=entity, 1=predicate, 2=salient-object) — windowCoversThreeSlots's merge unit.
type slotHit struct {
	pos  int
	slot int
}

// windowCoversThreeSlots reports whether there exists a contiguous span of at most windowBytes
// containing at least one position from EACH of entityPos, predicatePos, objectPos — the packet's
// "three-slot conjunction, never any-2-of-3" rule. Classic sorted-merge + sliding window: O(n log n)
// in the total number of occurrences.
func windowCoversThreeSlots(entityPos, predicatePos, objectPos []int, windowBytes int) bool {
	hits := make([]slotHit, 0, len(entityPos)+len(predicatePos)+len(objectPos))
	for _, p := range entityPos {
		hits = append(hits, slotHit{p, 0})
	}
	for _, p := range predicatePos {
		hits = append(hits, slotHit{p, 1})
	}
	for _, p := range objectPos {
		hits = append(hits, slotHit{p, 2})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })

	var count [3]int
	left := 0
	for right := 0; right < len(hits); right++ {
		count[hits[right].slot]++
		for hits[right].pos-hits[left].pos > windowBytes {
			count[hits[left].slot]--
			left++
		}
		if count[0] > 0 && count[1] > 0 && count[2] > 0 {
			return true
		}
	}
	return false
}
