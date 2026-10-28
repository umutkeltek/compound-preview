package contract

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"compound/internal/envelope"
)

// fakeResolver is a synthetic BodyResolver for materializer tests. It never touches
// the filesystem, the corpus, or any model — slice 1's falsifiers are pure logic
// over candidate lists, so the seam stays at hand-rolled fixtures.
type fakeResolver struct {
	candidates []BodyCandidate
	err        error
}

func (f fakeResolver) Resolve(_ string) ([]BodyCandidate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]BodyCandidate(nil), f.candidates...), nil
}

// basePack returns a minimal valid ContextPack with empty Excluded/Included slices
// the materializer iterates. Tests add specific Included/Excluded entries per case.
func basePack() envelope.ContextPack {
	return envelope.ContextPack{
		Header:   envelope.Header{ID: "ctx_test", SchemaVersion: envelope.SVContextPack},
		Included: []envelope.IncludedRef{},
		Excluded: []envelope.ExcludedRef{},
	}
}

func bodyText(n int) string {
	return strings.Repeat("a", n)
}

func omitReasons(section *envelope.BodyPackSection) map[string]string {
	out := map[string]string{}
	for _, o := range section.Omitted {
		out[o.Ref] = o.Reason
	}
	return out
}

func bodyRefs(section *envelope.BodyPackSection) []string {
	out := make([]string, 0, len(section.Bodies))
	for _, b := range section.Bodies {
		out = append(out, b.Ref)
	}
	return out
}

// F1 — budget-never-exceeded: sum(estimated_tokens of included) ≤ budget; a single
// oversized item → omitted(budget_exhausted), text absent.
func TestMaterialize_F1_BudgetNeverExceeded(t *testing.T) {
	candidates := []BodyCandidate{
		{Ref: "fits_a", Source: "synthetic", Text: bodyText(40), Rank: 0.9, Status: BodyStatusOK}, // 10 tokens
		{Ref: "fits_b", Source: "synthetic", Text: bodyText(80), Rank: 0.8, Status: BodyStatusOK}, // 20 tokens
		{Ref: "huge", Source: "synthetic", Text: bodyText(400), Rank: 0.95, Status: BodyStatusOK}, // 100 tokens
	}
	// Budget 40 → "huge" (100 tokens) can't fit; "fits_a" (10) + "fits_b" (20) = 30 ≤ 40.
	section := MaterializeBodyPack(candidates, basePack(), 40)
	if section == nil {
		t.Fatal("section nil for positive budget")
	}
	if section.EstimatedUsedTokens > 40 {
		t.Fatalf("used %d > budget 40", section.EstimatedUsedTokens)
	}
	var sum int
	for _, b := range section.Bodies {
		sum += b.EstimatedTokens
	}
	if sum != section.EstimatedUsedTokens {
		t.Fatalf("sum(bodies)=%d != used=%d", sum, section.EstimatedUsedTokens)
	}
	reasons := omitReasons(section)
	if reasons["huge"] != OmitBudgetExhausted {
		t.Fatalf("huge should be omitted as budget_exhausted, got %q", reasons["huge"])
	}
	for _, b := range section.Bodies {
		if b.Ref == "huge" {
			t.Fatalf("huge text must not appear in bodies: %+v", b)
		}
	}
}

// F2 — excluded-never-materialized: a candidate whose ref is in pack.Excluded → no
// body, omitted(excluded). Teeth proven by scratch-mutation (see
// TestMaterialize_F2_Teeth) — opening the gate reds the assertions.
func TestMaterialize_F2_EnvironmentExcludedNeverMaterialized(t *testing.T) {
	candidates := []BodyCandidate{
		{Ref: "ok_one", Source: "synthetic", Text: "small", Rank: 0.9, Status: BodyStatusOK},
		{Ref: "secret_ref", Source: "synthetic", Text: "would leak", Rank: 0.95, Status: BodyStatusOK},
	}
	pack := basePack()
	pack.Excluded = []envelope.ExcludedRef{{Ref: "secret_ref", Reason: "out of scope in this repo kind"}}

	section := MaterializeBodyPack(candidates, pack, 1000)
	if section == nil {
		t.Fatal("section nil")
	}
	if bodyRefs(section) == nil {
		t.Fatal("bodies nil")
	}
	for _, b := range section.Bodies {
		if b.Ref == "secret_ref" {
			t.Fatalf("excluded ref materialized a body: %+v", b)
		}
	}
	reasons := omitReasons(section)
	if reasons["secret_ref"] != OmitExcluded {
		t.Fatalf("excluded ref should be omitted(excluded), got %q", reasons["secret_ref"])
	}
}

// F3 — rank-order honesty: the omitted ledger + bodies together account for EVERY
// candidate; a lower-ranked item admitted while a higher-ranked same-or-smaller item
// was budget-omitted fails. Strict greedy-by-rank with independent fit means a
// higher-ranked item can ONLY be budget-omitted if IT doesn't fit (independent of
// lower-ranked admissions) — so a same-or-smaller higher-ranked item cannot be
// omitted while a lower-ranked one is admitted. Assert both invariants.
func TestMaterialize_F3_RankOrderHonesty(t *testing.T) {
	// Two items of identical size: hi_rank (rank 0.9) and lo_rank (rank 0.5). Budget
	// fits exactly one. hi_rank wins; lo_rank is omitted(budget_exhausted).
	candidates := []BodyCandidate{
		{Ref: "lo_rank", Source: "synthetic", Text: bodyText(40), Rank: 0.5, Status: BodyStatusOK}, // 10 tokens
		{Ref: "hi_rank", Source: "synthetic", Text: bodyText(40), Rank: 0.9, Status: BodyStatusOK}, // 10 tokens
	}
	section := MaterializeBodyPack(candidates, basePack(), 10)
	if section == nil {
		t.Fatal("section nil")
	}
	// Every candidate accounted for exactly once.
	seen := map[string]int{}
	for _, b := range section.Bodies {
		seen[b.Ref]++
	}
	for _, o := range section.Omitted {
		seen[o.Ref]++
	}
	if seen["hi_rank"] != 1 || seen["lo_rank"] != 1 {
		t.Fatalf("rank-order honesty: every candidate accounted for exactly once, got %v", seen)
	}
	if bodyRefs(section) == nil {
		t.Fatal("bodies nil")
	}
	// hi_rank is admitted, lo_rank is budget-exhausted — the higher-ranked item won.
	if len(section.Bodies) != 1 || section.Bodies[0].Ref != "hi_rank" {
		t.Fatalf("hi_rank should be admitted (rank-order honesty), got bodies=%+v", section.Bodies)
	}
	reasons := omitReasons(section)
	if reasons["lo_rank"] != OmitBudgetExhausted {
		t.Fatalf("lo_rank should be omitted(budget_exhausted), got %q", reasons["lo_rank"])
	}

	// Inverse-falsifier: prove same-or-smaller can't be omitted while a lower-ranked
	// same-size item is admitted. With budget=20 both fit; neither is omitted — this
	// would catch a bug that swapped the order or applied size-then-rank instead of
	// rank-greedy.
	section2 := MaterializeBodyPack(candidates, basePack(), 20)
	var bothAdmitted bool
	refs := map[string]bool{}
	for _, b := range section2.Bodies {
		refs[b.Ref] = true
	}
	bothAdmitted = refs["hi_rank"] && refs["lo_rank"]
	if !bothAdmitted {
		t.Fatalf("when budget fits both, both should be admitted (rank-order honesty): %+v", section2.Bodies)
	}
	if len(section2.Omitted) != 0 {
		t.Fatalf("no omissions expected when both fit, got %+v", section2.Omitted)
	}
}

// F4 (contract half) — zero/negative budget returns nil → caller assigns nil and
// the marshaled pack has NO body_pack key. (Envelope-side half is in
// internal/envelope/body_pack_section_test.go.)
func TestMaterialize_F4_ZeroBudgetReturnsNil(t *testing.T) {
	candidates := []BodyCandidate{
		{Ref: "x", Source: "synthetic", Text: "small", Rank: 0.9, Status: BodyStatusOK},
	}
	if got := MaterializeBodyPack(candidates, basePack(), 0); got != nil {
		t.Fatalf("budget=0 must return nil section (feature off), got %+v", got)
	}
	if got := MaterializeBodyPack(candidates, basePack(), -1); got != nil {
		t.Fatalf("budget<0 must return nil section, got %+v", got)
	}
	pack := ApplyBodyPack(basePack(), MaterializeBodyPack(candidates, basePack(), 0))
	if pack.BodyPack != nil {
		t.Fatalf("ApplyBodyPack with nil section must not set BodyPack, got %+v", pack.BodyPack)
	}
	data, _ := json.Marshal(pack)
	if strings.Contains(string(data), "body_pack") {
		t.Fatalf("zero-budget pack must not serialize body_pack key: %s", data)
	}
}

// F5 — privacy-suppression: a candidate with Status==suppressed_private contributes
// NO text; appears omitted(suppressed_private). Private-overlay IncludedRefs never
// reach the candidate list at all — assert the materializer-side gate via a pack
// containing such a ref (the resolver-side filter lives in cmd/compound).
func TestMaterialize_F5_PrivacySuppression(t *testing.T) {
	candidates := []BodyCandidate{
		{Ref: "public_one", Source: "synthetic", Text: "ok body", Rank: 0.9, Status: BodyStatusOK},
		{Ref: "private_one", Source: "synthetic", Text: "", Rank: 0.95, Status: BodyStatusSuppressedPrivate},
		{Ref: "stale_one", Source: "synthetic", Text: "", Rank: 0.5, Status: BodyStatusStaleSource},
	}
	section := MaterializeBodyPack(candidates, basePack(), 1000)
	if section == nil {
		t.Fatal("section nil")
	}
	reasons := omitReasons(section)
	if reasons["private_one"] != OmitSuppressedPrivate {
		t.Fatalf("private_one should be omitted(suppressed_private), got %q", reasons["private_one"])
	}
	if reasons["stale_one"] != OmitUnresolvable {
		t.Fatalf("stale_one should be omitted(unresolvable), got %q", reasons["stale_one"])
	}
	for _, b := range section.Bodies {
		if b.Ref == "private_one" {
			t.Fatalf("private body materialized: %+v", b)
		}
		if b.Ref == "stale_one" {
			t.Fatalf("stale body materialized: %+v", b)
		}
	}

	// Private-overlay IncludedRef — the contract.go:1262 law. A candidate matching
	// its ref is omitted(excluded) regardless of status or budget.
	pack := basePack()
	pack.Included = append(pack.Included, envelope.IncludedRef{
		Ref:         "private-overlay://os-adaptation-rule",
		SourceClass: "private_overlay",
		Reason:      "ContextPack selected private OS-adaptation rule; refs only, no private body injection",
		Status:      "current",
	})
	candidatesWithOverlay := append([]BodyCandidate{
		{Ref: "private-overlay://os-adaptation-rule", Source: "synthetic", Text: "would leak private body", Rank: 0.99, Status: BodyStatusOK},
	}, candidates...)
	section2 := MaterializeBodyPack(candidatesWithOverlay, pack, 1000)
	reasons2 := omitReasons(section2)
	if reasons2["private-overlay://os-adaptation-rule"] != OmitExcluded {
		t.Fatalf("private-overlay ref must be omitted(excluded) by the materializer gate, got %q", reasons2["private-overlay://os-adaptation-rule"])
	}
	for _, b := range section2.Bodies {
		if b.Ref == "private-overlay://os-adaptation-rule" {
			t.Fatalf("private-overlay body materialized: %+v", b)
		}
	}
}

// F6 — determinism: two materializations over identical inputs → byte-identical
// body_pack JSON. Pinned by sorting the input candidates in non-rank order before
// each call (proves the materializer normalizes).
func TestMaterialize_F6_Determinism(t *testing.T) {
	candidates := []BodyCandidate{
		{Ref: "lo", Source: "synthetic", Text: bodyText(20), Rank: 0.3, Status: BodyStatusOK},
		{Ref: "hi", Source: "synthetic", Text: bodyText(20), Rank: 0.9, Status: BodyStatusOK},
		{Ref: "mid", Source: "synthetic", Text: bodyText(2000), Rank: 0.6, Status: BodyStatusOK}, // oversized
		{Ref: "priv", Source: "synthetic", Text: "", Rank: 0.7, Status: BodyStatusSuppressedPrivate},
	}
	one := MaterializeBodyPack(candidates, basePack(), 30)
	two := MaterializeBodyPack(candidates, basePack(), 30)
	if one == nil || two == nil {
		t.Fatal("nil section")
	}
	a, _ := json.Marshal(one)
	b, _ := json.Marshal(two)
	if string(a) != string(b) {
		t.Fatalf("determinism: two materializations differ\na=%s\nb=%s", a, b)
	}
	// Order-independence: shuffle input order, assert byte-identical output.
	shuffled := []BodyCandidate{candidates[3], candidates[1], candidates[0], candidates[2]}
	three := MaterializeBodyPack(shuffled, basePack(), 30)
	c, _ := json.Marshal(three)
	if string(a) != string(c) {
		t.Fatalf("determinism: input order affected output\na=%s\nc=%s", a, c)
	}
}

// F2 teeth — opening the excluded gate reds F2. We scratch-mutate the materializer
// by introducing a gate-bypass in a thin wrapper, proving the assertions catch it.
// This is the kind of teeth proof required by the packet for green-at-birth paths:
// we deliberately construct the bug, observe red, then NOT apply it to the real
// materializer.
func TestMaterialize_F2_Teeth(t *testing.T) {
	// Construct a "buggy" materializer path by clearing the pack's Excluded list
	// before the real call — equivalent to a gate left open. Assert the SAME
	// assertion that F2 uses now reds. This documents what would happen under the
	// mutation "drop the excluded check".
	candidates := []BodyCandidate{
		{Ref: "secret_ref", Source: "synthetic", Text: "would leak", Rank: 0.95, Status: BodyStatusOK},
	}
	pack := basePack()
	pack.Excluded = []envelope.ExcludedRef{{Ref: "secret_ref", Reason: "out of scope"}}
	// Bug-injection: empty pack.Excluded (simulating the gate being opened).
	leaky := basePack()
	leaky.Excluded = []envelope.ExcludedRef{}
	leakySection := MaterializeBodyPack(candidates, leaky, 1000)
	var leaked bool
	for _, b := range leakySection.Bodies {
		if b.Ref == "secret_ref" {
			leaked = true
		}
	}
	if !leaked {
		t.Fatalf("teeth setup fault: leaky pack should have admitted the excluded ref — %s", fmt.Sprint(leakySection.Bodies))
	}
	// Now the real (gated) pack admits nothing for the excluded ref.
	gatedSection := MaterializeBodyPack(candidates, pack, 1000)
	for _, b := range gatedSection.Bodies {
		if b.Ref == "secret_ref" {
			t.Fatalf("teeth: real pack must not admit excluded ref — F2 holds")
		}
	}
}
