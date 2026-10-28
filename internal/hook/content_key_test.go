package hook

import (
	"reflect"
	"testing"
)

// Falsifier (slice 1): DeriveContentKey is a pure, versioned function of a claim's structured
// entity/predicate/object fields — the preferred entry point (mirrors mem.Claim's shape).
func TestDeriveContentKeyStructuredFieldsBasic(t *testing.T) {
	key := DeriveContentKey("source_host", "pricing_region", "germany_frankfurt datacenter")
	if key.Version != ContentKeyDerivationVersion {
		t.Fatalf("key not version-stamped: got %q want %q", key.Version, ContentKeyDerivationVersion)
	}
	if !reflect.DeepEqual(key.EntityTokens, []string{"source_host"}) {
		t.Fatalf("entity tokens: %v", key.EntityTokens)
	}
	if !reflect.DeepEqual(key.PredicateTokens, []string{"pricing_region"}) {
		t.Fatalf("predicate tokens: %v", key.PredicateTokens)
	}
	if !reflect.DeepEqual(key.SalientObjectTokens, []string{"datacenter", "germany_frankfurt"}) {
		t.Fatalf("salient object tokens: %v", key.SalientObjectTokens)
	}
	if !key.Eligible {
		t.Fatalf("expected eligible key, got ineligible: %+v", key)
	}
}

// Falsifier: the rendered-text fallback parses "entity.predicate = object" into the SAME key the
// structured path would derive from the same three fields — the packet's "prefer structured
// fields where the code has them; else split on ' = ' / last '.'" rule must round-trip exactly.
func TestDeriveContentKeyFromLineMatchesStructuredPath(t *testing.T) {
	line := "source_host.pricing_region = germany_frankfurt datacenter"
	fromLine, ok := DeriveContentKeyFromLine(line)
	if !ok {
		t.Fatalf("expected line to parse, got ok=false")
	}
	structured := DeriveContentKey("source_host", "pricing_region", "germany_frankfurt datacenter")
	if !reflect.DeepEqual(fromLine, structured) {
		t.Fatalf("fallback parse diverged from structured derivation:\nfromLine=%+v\nstructured=%+v", fromLine, structured)
	}
}

// Falsifier: an entity-path with an embedded dot (nested qualifier) splits on the LAST dot, not
// the first — "billing.invoice.state = paid" must yield entity-path "billing.invoice", predicate
// "state", never entity "billing" + predicate "invoice.state" (which would embed a stray dot into
// the predicate token stream).
func TestDeriveContentKeyFromLineSplitsOnLastDot(t *testing.T) {
	key, ok := DeriveContentKeyFromLine("billing.invoice.state = paid in full amount")
	if !ok {
		t.Fatalf("expected line to parse")
	}
	if !reflect.DeepEqual(key.EntityTokens, []string{"billing", "invoice"}) {
		t.Fatalf("entity tokens (expected billing+invoice from entity-path billing.invoice): %v", key.EntityTokens)
	}
	if !reflect.DeepEqual(key.PredicateTokens, []string{"state"}) {
		t.Fatalf("predicate tokens: %v", key.PredicateTokens)
	}
}

// Falsifier: a line with no " = " cannot be parsed — ineligible, not an error, not a panic.
func TestDeriveContentKeyFromLineNoEqualsIsIneligible(t *testing.T) {
	key, ok := DeriveContentKeyFromLine("source_host pricing_region germany_frankfurt")
	if ok {
		t.Fatalf("expected ok=false for a line with no ' = ', got %+v", key)
	}
	if key.Eligible {
		t.Fatalf("zero-value key must never be eligible")
	}
}

// Falsifier: an lhs with no "." (no entity/predicate split point) cannot be parsed — ineligible.
func TestDeriveContentKeyFromLineNoDotInLHSIsIneligible(t *testing.T) {
	key, ok := DeriveContentKeyFromLine("pricingregion = germany_frankfurt datacenter")
	if ok {
		t.Fatalf("expected ok=false for an lhs with no '.', got %+v", key)
	}
	if key.Eligible {
		t.Fatalf("zero-value key must never be eligible")
	}
}

// Falsifier (eligibility gate, low specificity): all three slots present but too thin (short
// tokens, low summed length, no qualifying long/digit token) must be INELIGIBLE — the packet's
// fail-safe floor, not a judgment call left to the matcher.
func TestContentKeyEligibilityLowSpecificityIsIneligible(t *testing.T) {
	key := DeriveContentKey("ab", "cd", "ef")
	if key.Eligible {
		t.Fatalf("expected ineligible (all tokens < min length), got %+v", key)
	}

	// Two short-but-valid (>=3 byte) tokens totalling under the summed-length floor, with no
	// token >= 6 bytes or containing a digit.
	key2 := DeriveContentKey("app", "run", "log")
	if key2.Eligible {
		t.Fatalf("expected ineligible (no qualifying long/digit token, thin summed length), got %+v", key2)
	}
}

// Falsifier (eligibility gate, meets floor): a realistically specific claim clears every bar.
func TestContentKeyEligibilityMeetsFloorIsEligible(t *testing.T) {
	key := DeriveContentKey("checkout_flow", "conversion_rate", "increased to 12 percent")
	if !key.Eligible {
		t.Fatalf("expected eligible, got ineligible: %+v", key)
	}
}

// Falsifier: an empty slot (no salient object tokens once cross-field-deduped away) is
// ineligible even if entity/predicate look specific enough alone — ALL three slots are load
// bearing, never any-2-of-3.
func TestContentKeyEligibilityRequiresAllThreeSlotsNonEmpty(t *testing.T) {
	// Object text is entirely composed of tokens already in entity/predicate -- cross-field
	// dedupe leaves salient-object empty.
	key := DeriveContentKey("checkout_flow", "conversion_rate", "checkout_flow conversion_rate")
	if key.Eligible {
		t.Fatalf("expected ineligible (salient-object empty after cross-field dedupe), got %+v", key)
	}
	if len(key.SalientObjectTokens) != 0 {
		t.Fatalf("expected empty salient object tokens, got %v", key.SalientObjectTokens)
	}
}

// Falsifier: cross-field dedupe — an object token that merely repeats the entity or predicate
// contributes no salient signal and must be excluded from SalientObjectTokens.
func TestDeriveContentKeyCrossFieldDedupe(t *testing.T) {
	key := DeriveContentKey("checkout_flow", "conversion_rate", "checkout_flow now measured at 12 percent")
	for _, tok := range key.SalientObjectTokens {
		if tok == "checkout_flow" {
			t.Fatalf("salient object tokens must exclude entity token, got %v", key.SalientObjectTokens)
		}
	}
}

// Falsifier: stop-words and sub-3-byte tokens are dropped from every slot, not just the object.
func TestDeriveContentKeyDropsStopWordsAndShortTokens(t *testing.T) {
	key := DeriveContentKey("the_flow", "is_at", "was measured at 12 percent for the checkout")
	for _, tok := range key.AllTokens() {
		if contentKeyStopWords[tok] {
			t.Fatalf("stopword %q survived normalization: %v", tok, key.AllTokens())
		}
		if len(tok) < contentKeyMinTokenLen {
			t.Fatalf("sub-minimum token %q survived normalization: %v", tok, key.AllTokens())
		}
	}
}

// Falsifier: a snake_case token survives WHOLE (never split on the underscore).
func TestDeriveContentKeySnakeCaseSurvivesWhole(t *testing.T) {
	key := DeriveContentKey("source_host", "connection_state", "healthy and responsive")
	if !reflect.DeepEqual(key.EntityTokens, []string{"source_host"}) {
		t.Fatalf("expected source_host to survive whole, got %v", key.EntityTokens)
	}
}

// Falsifier: purity/determinism — the same three fields always derive a byte-identical key.
func TestDeriveContentKeyDeterministic(t *testing.T) {
	a := DeriveContentKey("checkout_flow", "conversion_rate", "increased to 12 percent this week")
	b := DeriveContentKey("checkout_flow", "conversion_rate", "increased to 12 percent this week")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same input produced different keys:\na=%+v\nb=%+v", a, b)
	}
}

// Falsifier: NFC normalization treats a precomposed accented letter as "other" (dropped), not
// silently decomposed into its bare Latin letter — a multilingual token must NOT survive
// normalization as if it were plain ASCII. This is what makes the held-out multilingual/paraphrase
// fixture (slice 3) an ACCEPTED miss rather than an accidental match.
func TestDeriveContentKeyNFCDropsAccentedLetterRatherThanDecomposing(t *testing.T) {
	// "café_host" (precomposed é, U+00E9) must NOT normalize to a token containing "cafe".
	toks := normalizeTokens("café_host")
	for _, tok := range toks {
		if tok == "cafe_host" || tok == "cafehost" {
			t.Fatalf("accented letter was decomposed to bare ascii, got token %q (want it dropped/split instead)", tok)
		}
	}
}
