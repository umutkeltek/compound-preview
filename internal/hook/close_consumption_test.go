package hook

import (
	"testing"

	"compound/internal/envelope"
)

// Falsifier 1: the governed close path detects a consumed injected claim.
//
// This is the edge itself. Before it existed, `compound work close` had no transcript and could
// only ever report UNKNOWN, so this test reds with (nil, false) if ConsumptionForClose stops
// reaching the detector.
func TestConsumptionForCloseDetectsConsumedInjectedClaim(t *testing.T) {
	_, home, store := consumptionEnv(t)
	const claimRef = "claim://claim_closeedge01"
	writeConsumePack(t, home, "ctx_closeedge", claimRef)
	wid := closeableWork(t, store, "ctx_closeedge")

	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claimRef + "`" + ` recency"}` + "\n" +
		`{"role":"assistant","content":"I applied claim_closeedge01 to the pricing change."}` + "\n"
	path := writeTranscript(t, home, transcript)

	consumed, declared := ConsumptionForClose(store, home, wid, path)
	if !declared {
		t.Fatal("detector ran over a readable transcript => declared must be true")
	}
	if !containsString(consumed, claimRef) {
		t.Fatalf("consumed claim ref missing from governed-close detection: %v", consumed)
	}
}

// Falsifier 2: fail-open. An absent transcript, an empty path, and a nil store each yield
// (nil, false) — honestly UNKNOWN. None of them may fabricate a declaration, and none may error.
//
// This is the gate that keeps the edge from ever blocking a close. If a future change makes any
// of these return declared=true, injected refs would silently be scored IGNORED on closes that
// never actually looked at a transcript — which is precisely the dishonest-zero the injection
// meter was built to refuse.
func TestConsumptionForCloseFailsOpen(t *testing.T) {
	_, home, store := consumptionEnv(t)
	writeConsumePack(t, home, "ctx_failopen", "claim://claim_failopen01")
	wid := closeableWork(t, store, "ctx_failopen")

	cases := []struct {
		name       string
		workID     string
		transcript string
	}{
		{name: "empty transcript path", workID: wid, transcript: ""},
		{name: "absent transcript file", workID: wid, transcript: home + "/does-not-exist.jsonl"},
		{name: "empty work id", workID: "", transcript: home + "/whatever.jsonl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			consumed, declared := ConsumptionForClose(store, home, tc.workID, tc.transcript)
			if declared {
				t.Fatalf("fail-open violated: declared=true for %q", tc.name)
			}
			if len(consumed) != 0 {
				t.Fatalf("fail-open violated: consumed=%v for %q", consumed, tc.name)
			}
		})
	}

	// A nil store must also be unknown rather than a panic.
	if consumed, declared := ConsumptionForClose(nil, home, wid, home+"/x.jsonl"); declared || len(consumed) != 0 {
		t.Fatalf("nil store must be unknown, got consumed=%v declared=%v", consumed, declared)
	}
}

// Falsifier 3: hook-close and governed-close must score IDENTICALLY.
//
// This is the test that justifies the thin-wrapper design. The two paths exist for different
// reasons (one is driven by a harness SessionEnd event, one by an operator or agent running
// `compound work close`), and if they ever scored the same transcript differently the usage
// projection would be unreadable — a ref would be CONSUMED or IGNORED depending on which door
// the work happened to leave by. Two works, same pack, same transcript, one of each path.
//
// This reds the moment someone reimplements matching on the close side instead of sharing
// detectSessionConsumption.
func TestConsumptionForCloseMatchesSessionEndScoring(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	const claimRef = "claim://claim_parity01"
	// Two packs carrying the SAME injected ref: closeableWork derives its work id from the pack
	// id, so one pack cannot back two works. The refs are identical, which is what parity needs.
	writeConsumePack(t, home, "ctx_parity_hook", claimRef)
	writeConsumePack(t, home, "ctx_parity_close", claimRef)

	hookWork := closeableWork(t, store, "ctx_parity_hook")
	closeWork := closeableWork(t, store, "ctx_parity_close")

	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claimRef + "`" + ` recency"}` + "\n" +
		`{"role":"assistant","content":"Applying claim_parity01 as the basis for this change."}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: hookWork,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)

	consumed, declared := ConsumptionForClose(store, home, closeWork, path)

	if declared != (rec.Skills.ObservationQuality == "readable") {
		t.Fatalf("readability drift: governed-close=%v session-end=%s", declared, rec.Skills.ObservationQuality)
	}
	for _, ref := range consumed {
		if !containsString(rec.Skills.Referenced, ref) {
			t.Fatalf("governed-close credited %q that session-end did not: close=%v hook=%v", ref, consumed, rec.Skills.Referenced)
		}
	}
	if !containsString(consumed, claimRef) || !containsString(rec.Skills.Referenced, claimRef) {
		t.Fatalf("both paths must credit %q: close=%v hook=%v", claimRef, consumed, rec.Skills.Referenced)
	}
}
