package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
)

// claimFixture is one injected claim's structured fields, used to build both a real
// ledger-claim-shaped IncludedRef.Reason (see claimLedgerReason) and, where a test wants it, the
// expected DeriveContentKey directly.
type claimFixture struct {
	Ref       string
	Entity    string
	Predicate string
	Object    string
}

// claimLedgerReason renders a claim exactly like internal/contract's memory-ref builder does
// (contract.go's IncludedRef.Reason for a ledger claim: the "Compound memory ledger claim; ...:
// entity.predicate = object" convention) -- the shape content_key.go's fallback parser expects,
// and the shape that ends up embedded in the rendered injection markdown's "reason=" attribute
// (hook.go's writeIncludedList) and Active Memory line (hook.go's activeMemoryLine), so a
// transcript that reproduces this text verbatim IS the injection echo the detector must exclude.
func claimLedgerReason(entity, predicate, object string) string {
	return "Compound memory ledger claim; source stores and indexes are evidence, not authority: " + entity + "." + predicate + " = " + object
}

// claimContentLineText is the bare "entity.predicate = object" text claimLedgerReason wraps --
// what a test builds transcript content around to prove the echo-exclusion span.
func claimContentLineText(entity, predicate, object string) string {
	return entity + "." + predicate + " = " + object
}

// writeConsumePackWithClaims writes a ContextPack whose included[] carries claim:// refs with
// REAL ledger-claim-shaped Reason text (see claimLedgerReason) -- the input content-key
// derivation actually parses, as opposed to writeConsumePack's bare "recency" stub reason.
func writeConsumePackWithClaims(t *testing.T, home, packID string, claims ...claimFixture) {
	t.Helper()
	included := make([]envelope.IncludedRef, 0, len(claims))
	for _, c := range claims {
		included = append(included, envelope.IncludedRef{
			Ref:         c.Ref,
			SourceClass: "generated",
			Reason:      claimLedgerReason(c.Entity, c.Predicate, c.Object),
			Confidence:  1,
			Status:      "current",
		})
	}
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            packID,
			CreatedAt:     fixedNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "hook"},
			Authority:     envelope.Authority{SourceClass: "generated", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD:          "consume pack",
		Included:           included,
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("context pack fixture invalid: %v", err)
	}
	path := filepath.Join(home, "context", "packs", packID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(pack, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Falsifier a (paraphrase-now-counts): a session that references a claim's content (entity +
// predicate + salient object, no id) is detected consumed under v2 -- RED under v1 alone (the
// exact famine the packet exists to fix), GREEN once the content-key OR-path lands.
func TestSessionEndContentKeyParaphraseWithoutIDCounts(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	claim := claimFixture{
		Ref: "claim://claim_paraphrase01", Entity: "checkout_flow", Predicate: "conversion_rate",
		Object: "increased to 12 percent this week",
	}
	writeConsumePackWithClaims(t, home, "ctx_paraphrase", claim)
	wid := closeableWork(t, store, "ctx_paraphrase")

	// Injection echo present (as it always is); the assistant references the claim's CONTENT --
	// entity + predicate + salient object tokens -- but never its id nor its scheme-qualified ref.
	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claim.Ref + "`" +
		` source=generated reason=` + claimLedgerReason(claim.Entity, claim.Predicate, claim.Object) + `"}` + "\n" +
		`{"role":"assistant","content":"I confirmed checkout_flow conversion_rate increased after the deploy this week."}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.Skills.ObservationQuality != "readable" {
		t.Fatalf("detector ran => used_declared must be true, got %+v", rec.Skills)
	}
	if !containsString(rec.Skills.Referenced, claim.Ref) {
		t.Fatalf("content-key paraphrase must be credited as consumed: %v", rec.Skills.Referenced)
	}
}

// Falsifier c (self-echo still safe, content-key form): a claim whose content-key text appears
// ONLY inside its own rendered injection block -- never independently referenced -- must count as
// IGNORED, not consumed. Echo exclusion must extend to the content-key form, not just the id.
func TestSessionEndContentKeySelfEchoNotConsumed(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	claim := claimFixture{
		Ref: "claim://claim_echo_ck01", Entity: "source_host", Predicate: "deploy_status",
		Object: "rolled back due to failing health checks",
	}
	writeConsumePackWithClaims(t, home, "ctx_echo_ck", claim)
	wid := closeableWork(t, store, "ctx_echo_ck")

	// The content-key text (entity.predicate = object) appears ONLY inside the injected block;
	// the assistant's own words never repeat entity/predicate/salient-object tokens.
	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claim.Ref + "`" +
		` source=generated reason=` + claimLedgerReason(claim.Entity, claim.Predicate, claim.Object) + `"}` + "\n" +
		`{"role":"assistant","content":"Everything looks fine, no further action needed."}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.Skills.ObservationQuality != "readable" {
		t.Fatalf("detector ran => used_declared must be true, got %+v", rec.Skills)
	}
	if containsString(rec.Skills.Referenced, claim.Ref) {
		t.Fatalf("content-key self-echo must not count as consumed: %v", rec.Skills.Referenced)
	}
}

// Falsifier c, requote variant: the injected content-key text is byte-exactly REQUOTED elsewhere
// in the transcript (e.g. a later message reflows/repeats the injected digest verbatim) -- echo
// exclusion applies at EVERY transcript location, not just the original injection offset.
func TestSessionEndContentKeyRequotedDigestNotConsumed(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	claim := claimFixture{
		Ref: "claim://claim_requote01", Entity: "billing_export", Predicate: "retry_policy",
		Object: "backs off exponentially after repeated failures",
	}
	writeConsumePackWithClaims(t, home, "ctx_requote", claim)
	wid := closeableWork(t, store, "ctx_requote")

	line := claimContentLineText(claim.Entity, claim.Predicate, claim.Object)
	// The exact rendered claim line is requoted in a LATER message (not the injection offset) --
	// still byte-exact, still must be excluded.
	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claim.Ref + "`" +
		` source=generated reason=` + claimLedgerReason(claim.Entity, claim.Predicate, claim.Object) + `"}` + "\n" +
		`{"role":"assistant","content":"Recapping context: ` + line + `"}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if containsString(rec.Skills.Referenced, claim.Ref) {
		t.Fatalf("byte-exact requote of the injected digest must not count as consumed: %v", rec.Skills.Referenced)
	}
}

// Falsifier d (no cross-credit, sibling negative): two claims share the SAME entity and the SAME
// object value but differ in predicate. The transcript references only the FIRST claim's
// predicate; the sibling's predicate is never mentioned. The sibling must NOT be credited even
// though its entity and salient-object tokens both appear nearby (three-slot conjunction, not
// any-2-of-3).
func TestSessionEndContentKeyNoCrossCreditSiblingClaim(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	referenced := claimFixture{
		Ref: "claim://claim_sibling_region01", Entity: "db_host", Predicate: "region",
		Object: "frankfurt datacenter primary",
	}
	sibling := claimFixture{
		Ref: "claim://claim_sibling_capacity01", Entity: "db_host", Predicate: "capacity_tier",
		Object: "frankfurt datacenter primary",
	}
	writeConsumePackWithClaims(t, home, "ctx_sibling", referenced, sibling)
	wid := closeableWork(t, store, "ctx_sibling")

	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + referenced.Ref + "`" +
		` source=generated reason=` + claimLedgerReason(referenced.Entity, referenced.Predicate, referenced.Object) + `\n- ` +
		"`" + sibling.Ref + "`" + ` source=generated reason=` + claimLedgerReason(sibling.Entity, sibling.Predicate, sibling.Object) + `"}` + "\n" +
		`{"role":"assistant","content":"Confirmed db_host region is the frankfurt datacenter primary site."}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if !containsString(rec.Skills.Referenced, referenced.Ref) {
		t.Fatalf("referenced claim must be credited: %v", rec.Skills.Referenced)
	}
	if containsString(rec.Skills.Referenced, sibling.Ref) {
		t.Fatalf("unreferenced sibling (shared entity+object, different predicate never mentioned) must NOT be credited: %v", rec.Skills.Referenced)
	}
}

// Falsifier d / ambient-scatter negative: the entity is mentioned dozens of times, and the
// predicate/salient-object tokens are BOTH present in the transcript, but never within one
// <=200-char/one-line window of each other -- must NOT be credited (window bound enforced).
func TestSessionEndContentKeyAmbientScatterNotConsumed(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	claim := claimFixture{
		Ref: "claim://claim_scatter01", Entity: "lookup_cache", Predicate: "hit_ratio",
		Object: "improved substantially after tuning",
	}
	writeConsumePackWithClaims(t, home, "ctx_scatter", claim)
	wid := closeableWork(t, store, "ctx_scatter")

	padding := ""
	for i := 0; i < 40; i++ {
		padding += "lookup_cache warmed up fine and lookup_cache stayed healthy throughout. "
	}
	// hit_ratio and "improved"/"tuning" sit on the FAR side of >200 chars of padding from any
	// lookup_cache mention and from each other -- same line, never the same window.
	scattered := padding + "meanwhile hit_ratio was reported separately. " + padding + "tuning notes were filed elsewhere and improved later."
	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claim.Ref + "`" +
		` source=generated reason=` + claimLedgerReason(claim.Entity, claim.Predicate, claim.Object) + `"}` + "\n" +
		`{"role":"assistant","content":"` + scattered + `"}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if containsString(rec.Skills.Referenced, claim.Ref) {
		t.Fatalf("scattered mentions beyond the window bound must NOT be credited: %v", rec.Skills.Referenced)
	}
}

// Falsifier f-adjacent (ineligible claim, fixture F): a low-specificity claim whose content key
// fails the eligibility gate must fall back to exact-id ONLY -- content-key never fires for it,
// but v1 keeps working unmodified when the id IS present as a bare token.
func TestSessionEndIneligibleContentKeyFallsBackToExactIDOnly(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	claim := claimFixture{Ref: "claim://claim_thin01", Entity: "ab", Predicate: "cd", Object: "ef"}
	writeConsumePackWithClaims(t, home, "ctx_thin", claim)
	wid := closeableWork(t, store, "ctx_thin")

	// The assistant repeats the claim's (too-short-to-tokenize) words but never its bare id --
	// content-key must stay dead (ineligible), so this must NOT be credited.
	noIDTranscript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claim.Ref + "`" +
		` source=generated reason=` + claimLedgerReason(claim.Entity, claim.Predicate, claim.Object) + `"}` + "\n" +
		`{"role":"assistant","content":"ab cd ef, all good."}` + "\n"
	path := writeTranscript(t, home, noIDTranscript)
	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if containsString(rec.Skills.Referenced, claim.Ref) {
		t.Fatalf("ineligible content-key must never credit a bare-word (non-id) reference: %v", rec.Skills.Referenced)
	}

	// A SEPARATE session where the bare id IS present must still be credited via v1 -- ineligible
	// content-key must never suppress the untouched exact-id path.
	writeConsumePackWithClaims(t, home, "ctx_thin_exact_id", claim)
	wid2 := closeableWork(t, store, "ctx_thin_exact_id")
	idTranscript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claim.Ref + "`" +
		` source=generated reason=` + claimLedgerReason(claim.Entity, claim.Predicate, claim.Object) + `"}` + "\n" +
		`{"role":"assistant","content":"applied claim_thin01 as requested."}` + "\n"
	path2 := writeTranscript(t, home, idTranscript)
	res2, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid2,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path2,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec2 := res2.Produced[0].(*envelope.Receipt)
	if !containsString(rec2.Skills.Referenced, claim.Ref) {
		t.Fatalf("v1 exact-id path must still work for an ineligible-content-key claim: %v", rec2.Skills.Referenced)
	}
}

// Boundary H: a paraphrase that uses genuinely different vocabulary (synonyms / a different
// language) rather than the claim's own entity/predicate/object tokens is an ACCEPTED MISS, not a
// bug -- the detector is deterministic token matching, not semantic/LLM understanding.
func TestSessionEndContentKeyMultilingualParaphraseIsAcceptedMiss(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	claim := claimFixture{
		Ref: "claim://claim_multilingual01", Entity: "onboarding_flow", Predicate: "completion_rate",
		Object: "dropped sharply after the redesign",
	}
	writeConsumePackWithClaims(t, home, "ctx_multilingual", claim)
	wid := closeableWork(t, store, "ctx_multilingual")

	// Turkish paraphrase of the SAME fact, sharing no entity/predicate/object tokens with the
	// claim at all -- this MUST miss under the deterministic detector (accepted boundary).
	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claim.Ref + "`" +
		` source=generated reason=` + claimLedgerReason(claim.Entity, claim.Predicate, claim.Object) + `"}` + "\n" +
		`{"role":"assistant","content":"Kullanıcı tamamlama oranı yeniden tasarımdan sonra ciddi şekilde düştü."}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.Skills.ObservationQuality != "readable" {
		t.Fatalf("detector ran => used_declared must be true, got %+v", rec.Skills)
	}
	if containsString(rec.Skills.Referenced, claim.Ref) {
		t.Fatalf("accepted miss: a genuinely different-vocabulary/multilingual paraphrase must NOT be credited: %v", rec.Skills.Referenced)
	}
}
