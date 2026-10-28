package hook

// Falsifier for work_3820d2b91248 — consumption detector v2 content-key over-credit on the
// preserved dogfood repro transcript.
//
// PROVENANCE (byte-exact, nothing reconstructed):
//   - Receipt rec_396a4e3ccee2 / transition tr_396a4e3ccee2 (2026-07-10T22:45:45Z, lane
//     claude_code) closed work_6fa53aab92bb through the REAL `compound hook session-end` door
//     with the observed session's true transcript, used_declared=true, and declared all five
//     injected claims of pack ctx_61592fc04708 consumed.
//   - The observed session genuinely referenced ONE claim: claim_6a6fb980bf59 (bare id in its
//     final report's CLAIMS CONSUMED section; see also the corresponding operator-private
//     dogfood diagnosis note). The other four claims appear ONLY inside the injected block,
//     which sits entirely on ONE transcript line (a single JSONL record).
//   - Fixture testdata/dogfood_repro_rec_396a4e3ccee2.jsonl holds the two detector-relevant
//     records of that transcript byte-exact (the injection-echo record and the final-report
//     record; the 90 intermediate records contain none of the five claim ids — verified by
//     count: the four echo-only ids occur exactly once each, scheme-qualified, on line 1).
//   - Fixture testdata/ctx_61592fc04708_included_claims.json holds the five included claim refs
//     with their Reason strings byte-exact as pack ctx_61592fc04708 stores them — including the
//     "..." truncation the pack carries, which is load-bearing for the flaw (below).
//
// THE FLAW (filed, not fixed — no detector change lands while the harness_edit freeze
// claim_35c64adbc231 stands): v1 exact-id correctly scores only claim_6a6fb980bf59 (all other
// occurrences are scheme-qualified injection echo). v2 content-key over-credits all FOUR echo-only
// claims, because the whole injected block is on one line and each claim's byte-exact echo mask
// misses:
//   - contentKeyMatch masks renderedLine = claimContentLine(pack reason), which ends in the
//     pack's "..." truncation; the transcript's injection block carries the FULL claim text.
//     Truncated != full, so maskByteExact finds no byte-exact occurrence and masks nothing.
//   - With nothing masked, each claim's entity/predicate/salient-object tokens are all present
//     within one <=200-byte window of that same line — partly from its own unmasked text, partly
//     from its neighbors' (cross-claim token soup) — so the three-slot conjunction fires and every
//     echo-only claim self-credits.
//
// RED-FIRST: this test encodes the CORRECT verdict (exactly claim_6a6fb980bf59 consumed, declared
// true). It is red on the detector as shipped and goes green only when the one-line-echo
// over-credit is removed — without touching v1's verdicts or the held-out precision floor.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// dogfoodReproClaim is one included claim ref of pack ctx_61592fc04708 with its byte-exact Reason.
type dogfoodReproClaim struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// dogfoodReproEnv opts this falsifier in. It is OFF by default on purpose.
//
// This is a RED-FIRST test: it encodes the CORRECT verdict against a detector flaw that is
// filed, not fixed, so it FAILS whenever it runs. Gating it behind an explicit opt-in keeps
// main and every branch green; without the gate, merely having the private fixtures on disk
// would make the whole suite red for anyone who has them.
//
// To watch the flaw:
//
//	COMPOUND_DOGFOOD_REPRO=1 go test ./internal/hook/ -run TestDetectSessionConsumptionDogfoodRepro
const dogfoodReproEnv = "COMPOUND_DOGFOOD_REPRO"

// dogfoodReproFixture resolves a preserved-transcript fixture under the operator-private,
// gitignored docs_private/hook-fixtures/ tree.
//
// These fixtures are byte-exact captures of a REAL product session, so they carry a product
// name, pricing, and private source paths that must never enter this public-destined repo —
// the architecture doctor's doc-leak gate blocks them on sight. Byte-exactness is load-bearing
// for the flaw under test (a <=200-byte content-key window over one JSONL line, plus the
// pack's own "..." truncation), so sanitizing the bytes would destroy the reproduction.
// Keeping them private and skipping by default is the only option that preserves both.
func dogfoodReproFixture(name string) string {
	return filepath.Join("..", "..", "docs_private", "hook-fixtures", name)
}

// requireDogfoodReproOptIn skips unless the operator explicitly asked for the red falsifier.
func requireDogfoodReproOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv(dogfoodReproEnv) == "" {
		t.Skipf("red-first falsifier is opt-in; set %s=1 with the operator-private fixtures present to reproduce the over-credit", dogfoodReproEnv)
	}
}

// loadDogfoodReproPackClaims reads the five included claim refs + reasons exactly as the real
// pack ctx_61592fc04708 stores them (truncation included).
func loadDogfoodReproPackClaims(t *testing.T) []dogfoodReproClaim {
	t.Helper()
	path := dogfoodReproFixture("ctx_61592fc04708_included_claims.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("operator-private pack-claims fixture absent (%s); skipping preserved-transcript falsifier", path)
	}
	var claims []dogfoodReproClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("parse preserved pack claims: %v", err)
	}
	if len(claims) != 5 {
		t.Fatalf("preserved pack claims = %d, want 5", len(claims))
	}
	return claims
}

// writeDogfoodReproPack writes a ContextPack whose included[] carries the preserved refs with
// their byte-exact Reason strings — the detector input exactly as the real close resolved it.
func writeDogfoodReproPack(t *testing.T, home, packID string, claims []dogfoodReproClaim) {
	t.Helper()
	included := make([]envelope.IncludedRef, 0, len(claims))
	for _, c := range claims {
		included = append(included, envelope.IncludedRef{
			Ref:         c.Ref,
			SourceClass: "generated",
			Reason:      c.Reason,
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
		SummaryMD:          "preserved dogfood repro pack (included[] byte-exact from ctx_61592fc04708)",
		Included:           included,
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("dogfood repro pack fixture invalid: %v", err)
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

// TestDetectSessionConsumptionDogfoodRepro replays the preserved rec_396a4e3ccee2 transcript
// through detectSessionConsumption and demands the honest verdict: exactly the genuinely
// referenced claim (claim_6a6fb980bf59) consumed, the four echo-only claims not-consumed.
// RED-FIRST: red until the one-line-echo content-key over-credit is fixed.
func TestDetectSessionConsumptionDogfoodRepro(t *testing.T) {
	requireDogfoodReproOptIn(t)
	deps, home, store := consumptionEnv(t)

	claims := loadDogfoodReproPackClaims(t)
	const packID = "ctx_dogfood_repro"
	writeDogfoodReproPack(t, home, packID, claims)
	wid := closeableWork(t, store, packID)

	transcriptPath := dogfoodReproFixture("dogfood_repro_rec_396a4e3ccee2.jsonl")
	if _, err := os.Stat(transcriptPath); err != nil {
		t.Skipf("operator-private repro transcript absent (%s); skipping preserved-transcript falsifier", transcriptPath)
	}

	// The genuinely consumed claim, per the observed session's final report (bare id, no
	// claim:// prefix) and diagnosis.md's CLAIMS CONSUMED line.
	const wantConsumed = "claim://claim_6a6fb980bf59"
	echoOnly := []string{
		"claim://claim_0574f4ee1940",
		"claim://claim_3460e5bae87d",
		"claim://claim_75a8402ec249",
		"claim://claim_e3388649a155",
	}

	consumed, declared := detectSessionConsumption(deps, Fixture{WorkID: wid, TranscriptPath: transcriptPath})

	if !declared {
		t.Fatalf("declared = false; the preserved transcript is readable, so the detector must declare")
	}

	got := map[string]bool{}
	for _, ref := range consumed {
		got[ref] = true
	}

	if !got[wantConsumed] {
		t.Errorf("consumed = %v; the session genuinely referenced %s (bare id in its final report) — it MUST score consumed", consumed, wantConsumed)
	}
	for _, ref := range echoOnly {
		if got[ref] {
			id := strings.TrimPrefix(ref, claimScheme)
			v1Fired := false
			data, err := os.ReadFile(transcriptPath)
			if err == nil {
				v1Fired = bareClaimReference(string(data), id)
			}
			if v1Fired {
				t.Errorf("%s scored consumed via v1 exact-id, but the transcript has no bare occurrence of %s — v1 regression", ref, id)
			} else {
				t.Errorf("%s scored consumed with NO bare id anywhere in the transcript — this is the v2 content-key one-line-echo over-credit: its only occurrence is the scheme-qualified injection echo on the single-line injected block", ref)
			}
		}
	}

	if len(consumed) != 1 || !got[wantConsumed] {
		t.Fatalf("consumed = %v; want exactly [%s] — the four claims present ONLY inside the one-line injected block must score not-consumed (work_3820d2b91248 success predicate)", consumed, wantConsumed)
	}
}
