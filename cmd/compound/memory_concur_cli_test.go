package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/mem"
)

// fakeLaneServer returns an httptest server that answers every chat-completion
// call with a fixed concurrence verdict, shaped as a real OpenAI-compatible
// response (the content field is a JSON-encoded verdict object).
func fakeLaneServer(t *testing.T, verdict, summary string) *httptest.Server {
	t.Helper()
	verdictJSON := `{"verdict":` + strconv.Quote(verdict) + `,"summary":` + strconv.Quote(summary) + `}`
	body := `{"model":"fake-lane","choices":[{"message":{"role":"assistant","content":` + strconv.Quote(verdictJSON) + `}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// concurSeedClaim initializes a home and promotes one assumption onto the ledger,
// returning (homePath, store, claimID). It mirrors the canon CLI test's seeding.
func concurSeedClaim(t *testing.T) (string, *mem.Store, string) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := store.Capture([]mem.Candidate{{
		Entity:         "operator",
		Predicate:      "assumption.deploy",
		Object:         "safe to deploy",
		Confidence:     0.9,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		PromotionBasis: candidates.BasisCuratedPage,
		SourceTraceRef: "ev_concur",
		SnippetHash:    "sha256:concur",
		Visibility:     "contextual",
	}}, cliTestNow)
	if err != nil || len(got) != 1 {
		t.Fatalf("capture: %v", err)
	}
	res, err := store.Adjudicate(got[0].ID, "promote", "test", cliTestNow)
	if err != nil || res.Claim == nil {
		t.Fatalf("promote: %v", err)
	}
	return homePath, store, res.Claim.ID
}

// TestMemoryConcurCLIConcursAndWritesReceipt proves the full pipeline ALIVE:
// CLI verb -> distiller runner -> real HTTP to two lanes -> mem reduce ->
// reinforces edges + receipt under reviews/memory-concurrences.
func TestMemoryConcurCLIConcursAndWritesReceipt(t *testing.T) {
	_, store, claimID := concurSeedClaim(t)
	alfa := fakeLaneServer(t, "agree", "migrations clean")
	beta := fakeLaneServer(t, "agree", "tests green")
	t.Setenv("COMPOUND_CONCUR_ALFA_BASE_URL", alfa.URL)
	t.Setenv("COMPOUND_CONCUR_ALFA_MODEL", "fake/alfa")
	t.Setenv("COMPOUND_CONCUR_BETA_BASE_URL", beta.URL)
	t.Setenv("COMPOUND_CONCUR_BETA_MODEL", "fake/beta")

	out := captureStdout(t, func() {
		cmdMemory([]string{"concur", "--claim", claimID, "--content", "Is it safe to deploy?", "--lanes", "alfa,beta", "--threshold", "2", "--json"})
	})
	var rep mem.ConcurrenceReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if rep.Decision != mem.ConcurrenceDecisionConcur || !rep.CertaintyGate {
		t.Fatalf("expected concur + gate, got %+v", rep)
	}
	if rep.IndependentAgreementCount != 2 {
		t.Fatalf("expected 2 agreements, got %d", rep.IndependentAgreementCount)
	}
	refs, err := store.CorroborationEvidence(claimID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("expected 2 reinforces edges, got %v (err %v)", refs, err)
	}
	if !strings.Contains(rep.ReportPath, "reviews/memory-concurrences") {
		t.Fatalf("receipt must live under reviews/memory-concurrences, got %q", rep.ReportPath)
	}
	if _, err := os.Stat(rep.ReportPath); err != nil {
		t.Fatalf("receipt missing: %v", err)
	}
}

// TestMemoryConcurCLISingleDissentParks proves the safety contract through the CLI:
// one dissenting lane parks the claim, closes the gate, and writes NO reinforce
// edges — even though the other lane agreed.
func TestMemoryConcurCLISingleDissentParks(t *testing.T) {
	_, store, claimID := concurSeedClaim(t)
	alfa := fakeLaneServer(t, "agree", "looks fine")
	beta := fakeLaneServer(t, "disagree", "migration 0042 not applied on replica")
	t.Setenv("COMPOUND_CONCUR_ALFA_BASE_URL", alfa.URL)
	t.Setenv("COMPOUND_CONCUR_ALFA_MODEL", "fake/alfa")
	t.Setenv("COMPOUND_CONCUR_BETA_BASE_URL", beta.URL)
	t.Setenv("COMPOUND_CONCUR_BETA_MODEL", "fake/beta")

	out := captureStdout(t, func() {
		cmdMemory([]string{"concur", "--claim", claimID, "--content", "Is it safe to deploy?", "--lanes", "alfa,beta", "--threshold", "2", "--json"})
	})
	var rep mem.ConcurrenceReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if rep.Decision != mem.ConcurrenceDecisionPark || rep.CertaintyGate {
		t.Fatalf("expected park + gate-closed, got %+v", rep)
	}
	if len(rep.Dissenters) != 1 || rep.Dissenters[0] != "beta" {
		t.Fatalf("expected dissenter beta, got %v", rep.Dissenters)
	}
	refs, _ := store.CorroborationEvidence(claimID)
	if len(refs) != 0 {
		t.Fatalf("dissent must not add reinforce edges, got %v", refs)
	}
}

// TestMemoryConcurCLIUnconfiguredLaneAbstains proves a missing/free-tier-dead lane
// abstains instead of crashing the sweep or faking agreement.
func TestMemoryConcurCLIUnconfiguredLaneAbstains(t *testing.T) {
	_, _, claimID := concurSeedClaim(t)
	alfa := fakeLaneServer(t, "agree", "ok")
	t.Setenv("COMPOUND_CONCUR_ALFA_BASE_URL", alfa.URL)
	t.Setenv("COMPOUND_CONCUR_ALFA_MODEL", "fake/alfa")
	// gamma deliberately left unconfigured.

	out := captureStdout(t, func() {
		cmdMemory([]string{"concur", "--claim", claimID, "--content", "?", "--lanes", "alfa,gamma", "--threshold", "1", "--json"})
	})
	var rep mem.ConcurrenceReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if rep.Decision != mem.ConcurrenceDecisionConcur || !rep.CertaintyGate {
		t.Fatalf("threshold 1 with one agreeing lane should concur, got %+v", rep)
	}
	found := false
	for _, a := range rep.Abstainers {
		if a == "gamma" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unconfigured gamma must be recorded as abstainer, got %v", rep.Abstainers)
	}
}

// TestMemoryConcurCLIMissingClaimExitsUsage proves dispatcher wiring + validation
// via the subprocess re-exec pattern (the verb os.Exit(2)s on missing args).
func TestMemoryConcurCLIMissingClaimExitsUsage(t *testing.T) {
	if os.Getenv("COMPOUND_CONCUR_CHILD") == "1" {
		homePath := t.TempDir()
		os.Setenv("COMPOUND_HOME", homePath)
		if err := home.Init(homePath, "test", cliTestNow); err != nil {
			t.Fatalf("init home: %v", err)
		}
		cmdMemory([]string{"concur", "--content", "x", "--lanes", "alfa"}) // no --claim
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMemoryConcurCLIMissingClaimExitsUsage$")
	cmd.Env = append(os.Environ(), "COMPOUND_CONCUR_CHILD=1")
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit 2 on missing --claim, got err=%v", err)
	}
}

// TestMemoryConcurCLIParkExitsNonZero proves the gate-closed exit contract (the
// human path os.Exit(1)s so a caller can branch on it), via subprocess re-exec
// with a disagreeing lane stood up inside the child.
func TestMemoryConcurCLIParkExitsNonZero(t *testing.T) {
	if os.Getenv("COMPOUND_CONCUR_CHILD") == "1" {
		homePath := t.TempDir()
		os.Setenv("COMPOUND_HOME", homePath)
		if err := home.Init(homePath, "test", cliTestNow); err != nil {
			t.Fatalf("init home: %v", err)
		}
		store, err := mem.Open(homePath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		got, _ := store.Capture([]mem.Candidate{{
			Entity: "operator", Predicate: "assumption.deploy", Object: "safe",
			Confidence: 0.9, Derivation: "deterministic", ClaimType: "decision",
			PromotionBasis: candidates.BasisCuratedPage, SourceTraceRef: "ev", SnippetHash: "sha256:x",
		}}, cliTestNow)
		res, _ := store.Adjudicate(got[0].ID, "promote", "test", cliTestNow)
		srv := fakeLaneServer(t, "disagree", "blocked")
		os.Setenv("COMPOUND_CONCUR_ALFA_BASE_URL", srv.URL)
		os.Setenv("COMPOUND_CONCUR_ALFA_MODEL", "fake/alfa")
		cmdMemory([]string{"concur", "--claim", res.Claim.ID, "--content", "?", "--lanes", "alfa", "--threshold", "1"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMemoryConcurCLIParkExitsNonZero$")
	cmd.Env = append(os.Environ(), "COMPOUND_CONCUR_CHILD=1")
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exit 1 on closed gate, got err=%v", err)
	}
}
