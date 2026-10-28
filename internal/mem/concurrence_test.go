package mem

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// concurrenceClaim seeds and promotes one assumption onto the ledger, returning
// its claim id — the shared fixture every concurrence test concurs/parks.
func concurrenceClaim(t *testing.T, s *Store) string {
	t.Helper()
	captured, err := s.Capture([]Candidate{cand("operator", "assumption.deploy", "safe to deploy", "2026-07-10T00:00:00Z")}, t0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	res, err := promoteForTest(t, s, captured[0].ID, "work_concurrence", t0)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if res.Claim == nil {
		t.Fatalf("no claim promoted: %+v", res)
	}
	return res.Claim.ID
}

func needsReviewEdgesOn(t *testing.T, s *Store, claimID string) int {
	t.Helper()
	count := 0
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Kind == EdgeKindNeedsReview && e.To == claimID {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	return count
}

func concurrenceProbeForTest(claimID string, lanes []string, threshold int) ConcurrenceProbe {
	return ConcurrenceProbe{
		SchemaVersion: ConcurrenceProbeSchemaVersion,
		ID:            "probe_deploy_safety",
		ClaimID:       claimID,
		Content:       "Is it safe to deploy the payments service now?",
		Lanes:         lanes,
		Threshold:     threshold,
	}
}

// TestConcurrenceConvergesConcurs: uncontested, threshold-met, independent
// agreement appends reinforces edges and opens the gate.
func TestConcurrenceConvergesConcurs(t *testing.T) {
	s := newStore(t)
	claimID := concurrenceClaim(t, s)
	answers := []ConcurrenceAnswer{
		{Lane: "zai", Verdict: ConcurrenceVerdictAgree, Summary: "yes, migrations clean", ModelProvider: "zai"},
		{Lane: "minimax", Verdict: ConcurrenceVerdictAgree, Summary: "safe, tests green", ModelProvider: "minimax"},
		{Lane: "qwen", Verdict: ConcurrenceVerdictAgree, Summary: "no blockers", ModelProvider: "qwen"},
	}
	report, err := s.ReduceConcurrence(concurrenceProbeForTest(claimID, []string{"zai", "minimax", "qwen"}, 2), answers, t0)
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if report.Decision != ConcurrenceDecisionConcur || !report.CertaintyGate {
		t.Fatalf("expected concur + gate, got %+v", report)
	}
	if report.IndependentAgreementCount != 3 {
		t.Fatalf("expected 3 independent agreements, got %d", report.IndependentAgreementCount)
	}
	refs, err := s.CorroborationEvidence(claimID)
	if err != nil {
		t.Fatalf("corroboration evidence: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("expected 3 reinforces evidence refs, got %d (%v)", len(refs), refs)
	}
	if report.ReportPath == "" {
		t.Fatal("expected report path to be written")
	}
	if !strings.Contains(report.ReportPath, "reviews/memory-concurrences") {
		t.Fatalf("report must live under reviews/memory-concurrences, got %q", report.ReportPath)
	}
	if _, err := os.Stat(report.ReportPath); err != nil {
		t.Fatalf("report file missing: %v", err)
	}
}

// TestConcurrenceSingleDissentParksClaim is the depth-probe: even with a majority
// agreeing, ONE dissenting lane must park the claim, mark it needs_review, and add
// NO reinforce edges. Agreement under contest is not certainty — that is the
// entire safety contract.
func TestConcurrenceSingleDissentParksClaim(t *testing.T) {
	s := newStore(t)
	claimID := concurrenceClaim(t, s)
	answers := []ConcurrenceAnswer{
		{Lane: "zai", Verdict: ConcurrenceVerdictAgree, Summary: "looks fine"},
		{Lane: "minimax", Verdict: ConcurrenceVerdictAgree, Summary: "looks fine"},
		{Lane: "qwen", Verdict: ConcurrenceVerdictDisagree, Summary: "no: migration 0042 not applied on replica"},
	}
	report, err := s.ReduceConcurrence(concurrenceProbeForTest(claimID, []string{"zai", "minimax", "qwen"}, 2), answers, t0)
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if report.Decision != ConcurrenceDecisionPark || report.CertaintyGate {
		t.Fatalf("expected park + gate-closed on dissent, got %+v", report)
	}
	if len(report.Dissenters) != 1 || report.Dissenters[0] != "qwen" {
		t.Fatalf("expected dissenter qwen, got %v", report.Dissenters)
	}
	// No reinforce edges may have been written under contest.
	refs, err := s.CorroborationEvidence(claimID)
	if err != nil {
		t.Fatalf("corroboration evidence: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("dissent must not add reinforce edges, got %v", refs)
	}
	// The claim must be flagged for human review.
	if got := needsReviewEdgesOn(t, s, claimID); got != 1 {
		t.Fatalf("expected exactly 1 needs_review edge, got %d", got)
	}
	if !strings.Contains(report.Reason, "dissented") {
		t.Fatalf("reason should name the dissent: %q", report.Reason)
	}
}

// TestConcurrenceInsufficientAgreementHolds: not enough witnesses and no dissent
// means hold — no edges either way (absence of agreement is "not yet", not
// evidence against).
func TestConcurrenceInsufficientAgreementHolds(t *testing.T) {
	s := newStore(t)
	claimID := concurrenceClaim(t, s)
	answers := []ConcurrenceAnswer{
		{Lane: "zai", Verdict: ConcurrenceVerdictAgree, Summary: "ok"},
		{Lane: "minimax", Verdict: ConcurrenceVerdictAbstain, Summary: "uncertain"},
		{Lane: "qwen", Verdict: ConcurrenceVerdictAbstain, Summary: "uncertain"},
	}
	report, err := s.ReduceConcurrence(concurrenceProbeForTest(claimID, []string{"zai", "minimax", "qwen"}, 3), answers, t0)
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if report.Decision != ConcurrenceDecisionHold || report.CertaintyGate {
		t.Fatalf("expected hold + gate-closed, got %+v", report)
	}
	refs, _ := s.CorroborationEvidence(claimID)
	if len(refs) != 0 {
		t.Fatalf("hold must not add reinforce edges, got %v", refs)
	}
	if got := needsReviewEdgesOn(t, s, claimID); got != 0 {
		t.Fatalf("hold must not flag needs_review, got %d", got)
	}
}

// TestConcurrenceDuplicateLaneCannotInflate: the same lane voting twice cannot
// manufacture the agreement count, and a threshold beyond the distinct lane count
// is rejected up front. Independence is by distinct lane.
func TestConcurrenceDuplicateLaneCannotInflate(t *testing.T) {
	s := newStore(t)
	claimID := concurrenceClaim(t, s)
	answers := []ConcurrenceAnswer{
		{Lane: "zai", Verdict: ConcurrenceVerdictAgree, Summary: "ok"},
		{Lane: "zai", Verdict: ConcurrenceVerdictAgree, Summary: "ok again"}, // duplicate lane
	}
	_, err := s.ReduceConcurrence(concurrenceProbeForTest(claimID, []string{"zai"}, 2), answers, t0)
	if err == nil || !strings.Contains(err.Error(), "threshold") {
		t.Fatalf("expected threshold-exceeds-distinct-lanes rejection, got %v", err)
	}
}

// TestConcurrenceRefusesUnknownClaim: a concurrence can never attach to a dangling
// claim id.
func TestConcurrenceRefusesUnknownClaim(t *testing.T) {
	s := newStore(t)
	answers := []ConcurrenceAnswer{{Lane: "zai", Verdict: ConcurrenceVerdictAgree}}
	if _, err := s.ReduceConcurrence(concurrenceProbeForTest("claim_does_not_exist", []string{"zai"}, 1), answers, t0); err == nil {
		t.Fatal("expected error on an unknown claim")
	}
}

// TestConcurrenceThresholdExceedsLanesRejected: a gate that can never pass is a
// configuration error, surfaced up front.
func TestConcurrenceThresholdExceedsLanesRejected(t *testing.T) {
	s := newStore(t)
	claimID := concurrenceClaim(t, s)
	_, err := s.ReduceConcurrence(concurrenceProbeForTest(claimID, []string{"zai", "minimax"}, 3), []ConcurrenceAnswer{}, t0)
	if err == nil || !strings.Contains(err.Error(), "threshold") {
		t.Fatalf("expected threshold-exceeds-lanes error, got %v", err)
	}
}

// TestRunConcurrenceFanOutViaRunner: the orchestration path fans out through the
// ConcurrenceRunner; a lane that errors abstains rather than crashing the sweep,
// and the receipt is still written.
func TestRunConcurrenceFanOutViaRunner(t *testing.T) {
	s := newStore(t)
	claimID := concurrenceClaim(t, s)
	p := concurrenceProbeForTest(claimID, []string{"zai", "minimax", "flaky"}, 2)
	runner := ConcurrenceRunnerFunc(func(probe ConcurrenceProbe, lane string) (ConcurrenceAnswer, error) {
		switch lane {
		case "flaky":
			return ConcurrenceAnswer{}, os.ErrDeadlineExceeded // simulate a rate-limited free lane
		default:
			return ConcurrenceAnswer{Lane: lane, Verdict: ConcurrenceVerdictAgree, Summary: lane + " agrees"}, nil
		}
	})
	report, err := s.RunConcurrence(p, runner, t0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Decision != ConcurrenceDecisionConcur || !report.CertaintyGate {
		t.Fatalf("expected concur despite one abstaining lane, got %+v", report)
	}
	if len(report.Abstainers) != 1 || report.Abstainers[0] != "flaky" {
		t.Fatalf("expected flaky lane recorded as abstainer, got %v", report.Abstainers)
	}
	if report.IndependentAgreementCount != 2 {
		t.Fatalf("expected 2 agreements (flaky abstained), got %d", report.IndependentAgreementCount)
	}
	if _, err := os.Stat(report.ReportPath); err != nil {
		t.Fatalf("report file missing: %v", err)
	}
}
