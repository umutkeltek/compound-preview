package mem

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

func TestSynthesisSweepFindsEchoAndContradictionProposals(t *testing.T) {
	s := newStore(t)

	first := cand("project:api", "runtime", "Go", "")
	first.ClaimType = "decision"
	first.Qualifiers = map[string]string{"domain": "language", "decision_id": "api-language"}
	second := cand("project:api", "implementation_language", " go ", "")
	second.ClaimType = "decision"
	second.Qualifiers = map[string]string{"domain": "language", "decision_id": "api-language"}
	if _, err := s.Capture([]Candidate{first, second}, t0); err != nil {
		t.Fatal(err)
	}

	if err := appendJSONL(s.claimsLog(), &Claim{
		ID:         "claim_go",
		Seq:        1,
		Entity:     "project:api",
		Predicate:  "runtime",
		Object:     "Go",
		ClaimType:  "decision",
		Qualifiers: map[string]string{"domain": "language", "decision_id": "api-language"},
		Confidence: 0.9,
		Visibility: "public",
		CreatedAt:  rfc3339(t0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONL(s.claimsLog(), &Claim{
		ID:         "claim_rust",
		Seq:        2,
		Entity:     "project:api",
		Predicate:  "implementation_language",
		Object:     "Rust",
		ClaimType:  "decision",
		Qualifiers: map[string]string{"domain": "language", "decision_id": "api-language"},
		Confidence: 0.8,
		Visibility: "public",
		CreatedAt:  rfc3339(t0),
	}); err != nil {
		t.Fatal(err)
	}

	report, err := s.SynthesisSweep([]string{"cluster", "contradict"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.ClusterCount != 1 {
		t.Fatalf("ClusterCount = %d, want 1", report.ClusterCount)
	}
	if report.EchoFoldProposalCount != 1 {
		t.Fatalf("EchoFoldProposalCount = %d, want 1: %+v", report.EchoFoldProposalCount, report.EchoFoldProposals)
	}
	if report.ContradictionProposalCount != 1 {
		t.Fatalf("ContradictionProposalCount = %d, want 1: %+v", report.ContradictionProposalCount, report.ContradictionProposals)
	}
}

func TestSynthesisSweepReopensPromotedClaimOnContradictingEvidence(t *testing.T) {
	s := newStore(t)

	original := cand("project:api", "runtime", "Go", "")
	original.ClaimType = "decision"
	original.Qualifiers = map[string]string{"domain": "language", "decision_id": "api-language"}
	promotedCandidates, err := s.Capture([]Candidate{original}, t0)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := promoteForTest(t, s, promotedCandidates[0].ID, "work_promote", t0)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Claim == nil {
		t.Fatal("promote should write claim")
	}

	contradicting := cand("project:api", "implementation_language", "Rust", "")
	contradicting.ClaimType = "decision"
	contradicting.Qualifiers = map[string]string{"domain": "language", "decision_id": "api-language"}
	contradicting.Confidence = 0.55
	contradictingCandidates, err := s.Capture([]Candidate{contradicting}, t0)
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.SynthesisSweep([]string{"contradict"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.ContradictionProposalCount != 1 {
		t.Fatalf("ContradictionProposalCount = %d, want 1: %+v", report.ContradictionProposalCount, report.ContradictionProposals)
	}
	if report.ReviewDebtCount != 1 || len(report.ReopenedClaims) != 1 {
		t.Fatalf("review debt = %d reopened=%+v, want one reopened claim", report.ReviewDebtCount, report.ReopenedClaims)
	}
	if report.ContradictionProposals[0].CandidateIDs[0] != contradictingCandidates[0].ID {
		t.Fatalf("contradiction should link candidate %q: %+v", contradictingCandidates[0].ID, report.ContradictionProposals[0])
	}
	if report.ReopenedClaims[0].State != ClaimStateNeedsReview ||
		report.ReopenedClaims[0].ReviewRef != report.ContradictionProposals[0].ID ||
		len(report.ReopenedClaims[0].CandidateIDs) != 1 ||
		report.ReopenedClaims[0].CandidateIDs[0] != contradictingCandidates[0].ID {
		t.Fatalf("reopened claim marker incomplete: %+v", report.ReopenedClaims[0])
	}

	claims, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}
	var reopened Claim
	for _, claim := range claims {
		if claim.ID == promoted.Claim.ID {
			reopened = claim
			break
		}
	}
	if reopened.ID == "" {
		t.Fatalf("missing promoted claim %s in %+v", promoted.Claim.ID, claims)
	}
	if reopened.State != ClaimStateNeedsReview {
		t.Fatalf("claim state = %q, want %q", reopened.State, ClaimStateNeedsReview)
	}
	if len(reopened.ReviewRefs) != 1 || reopened.ReviewRefs[0] != report.ContradictionProposals[0].ID {
		t.Fatalf("claim review refs = %+v, want contradiction %s", reopened.ReviewRefs, report.ContradictionProposals[0].ID)
	}
	if reopened.ConfidenceDelta < -0.351 || reopened.ConfidenceDelta > -0.349 {
		t.Fatalf("confidence delta = %.2f, want -0.35", reopened.ConfidenceDelta)
	}

	defaultRecall, err := s.Recall("project:api", RecallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultRecall.Claims) != 0 {
		t.Fatalf("default recall should not keep needs_review claim authoritative: %+v", defaultRecall.Claims)
	}
	reviewRecall, err := s.Recall("project:api", RecallOpts{IncludeNeedsReview: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewRecall.Claims) != 1 || reviewRecall.Claims[0].State != ClaimStateNeedsReview {
		t.Fatalf("IncludeNeedsReview recall = %+v, want reopened claim visible", reviewRecall.Claims)
	}

	timeline, err := s.Timeline("project:api")
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 1 ||
		timeline[0].State != ClaimStateNeedsReview ||
		len(timeline[0].ReviewRefs) != 1 ||
		timeline[0].ReviewRefs[0] != report.ContradictionProposals[0].ID {
		t.Fatalf("timeline should expose reopened claim review state: %+v", timeline)
	}

	again, err := s.SynthesisSweep([]string{"contradict"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if again.ReviewDebtCount != 0 || len(again.ReopenedClaims) != 0 {
		t.Fatalf("second contradiction sweep should not duplicate review markers: %+v", again.ReopenedClaims)
	}
}

func TestSynthesisSweepRaisesParkedSeedOnLaterCorroboratingClaim(t *testing.T) {
	s := newStore(t)

	seedID := parkSeed(t, s, CandidateExpectation{
		Entity:    "project:alpha",
		Predicate: "runtime_outcome",
		Object:    "integration risk materialized",
	})
	promoted := promoteOutcome(t, s, "integration risk materialized", t0.AddDate(0, 0, 1))

	report, err := s.SynthesisSweep([]string{"corroborate"}, t0.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if report.CorroborationProposalCount != 1 || len(report.CorroborationProposals) != 1 {
		t.Fatalf("corroboration count = %d proposals=%+v, want one", report.CorroborationProposalCount, report.CorroborationProposals)
	}
	proposal := report.CorroborationProposals[0]
	if proposal.SeedCandidateID != seedID || proposal.EvidenceClaimID != promoted.Claim.ID {
		t.Fatalf("proposal refs = seed %q claim %q, want %q/%q", proposal.SeedCandidateID, proposal.EvidenceClaimID, seedID, promoted.Claim.ID)
	}
	if len(proposal.EvidenceRefs) == 0 || proposal.EvidenceRefs[0] != "claim:"+promoted.Claim.ID {
		t.Fatalf("proposal evidence refs = %+v, want claim evidence", proposal.EvidenceRefs)
	}
	if report.ReviewDebtCount != 1 || len(report.RaisedSeeds) != 1 {
		t.Fatalf("review debt = %d raised=%+v, want one raised seed", report.ReviewDebtCount, report.RaisedSeeds)
	}
	if report.RaisedSeeds[0].CandidateID != seedID || report.RaisedSeeds[0].State != ClaimStateNeedsReview || report.RaisedSeeds[0].ReviewRef != proposal.ID {
		t.Fatalf("raised seed marker incomplete: %+v", report.RaisedSeeds[0])
	}

	candidates, err := s.ListCandidates(t0.AddDate(0, 0, 1), CandidateListOptions{State: ClaimStateNeedsReview, IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if candidates.Counts.Matched != 1 || candidates.Items[0].ID != seedID || candidates.Items[0].ReviewState != ClaimStateNeedsReview {
		t.Fatalf("needs_review candidate list = %+v, want raised seed %s", candidates.Items, seedID)
	}
}

func TestSynthesisSweepDoesNotRaiseSeedWithoutSatisfiedExpectation(t *testing.T) {
	s := newStore(t)

	seedID := parkSeed(t, s, CandidateExpectation{
		Entity:    "project:alpha",
		Predicate: "runtime_outcome",
		Object:    "integration risk materialized",
	})
	promoteOutcome(t, s, "deployment remained smooth", t0.AddDate(0, 0, 1))

	report, err := s.SynthesisSweep([]string{"corroborate"}, t0.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if report.CorroborationProposalCount != 0 || len(report.RaisedSeeds) != 0 || report.ReviewDebtCount != 0 {
		t.Fatalf("unsatisfied seed raised unexpectedly: report=%+v", report)
	}
	candidates, err := s.ListCandidates(t0.AddDate(0, 0, 1), CandidateListOptions{State: ClaimStateNeedsReview})
	if err != nil {
		t.Fatal(err)
	}
	if candidates.Counts.Matched != 0 {
		t.Fatalf("seed %s should not be needs_review without satisfied expectation: %+v", seedID, candidates.Items)
	}
}

func TestCorroborationRaisesSeedWithoutAutoPromoting(t *testing.T) {
	s := newStore(t)

	seedID := parkSeed(t, s, CandidateExpectation{
		Entity:    "project:alpha",
		Predicate: "runtime_outcome",
		Object:    "integration risk materialized",
	})
	if _, err := s.Adjudicate(seedID, "promote", "work_seed_promote", t0); err == nil || !strings.Contains(err.Error(), "without valid promotion_basis") {
		t.Fatalf("basis-less seed promote error = %v, want promotion_basis refusal", err)
	}
	promoteOutcome(t, s, "integration risk materialized", t0.AddDate(0, 0, 1))
	beforeClaims, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.SynthesisSweep([]string{"corroborate"}, t0.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RaisedSeeds) != 1 {
		t.Fatalf("expected one raised seed, got %+v", report.RaisedSeeds)
	}
	afterClaims, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterClaims) != len(beforeClaims) {
		t.Fatalf("corroboration sweep auto-promoted a seed: claims before=%d after=%d", len(beforeClaims), len(afterClaims))
	}
}

func TestDecaySweepFilesExpiredParkedCandidates(t *testing.T) {
	s := newStore(t)
	// Un-evidenced capture stamps ExpiresAt = t0+30d; advance clock past it.
	got, err := s.Capture([]Candidate{{
		Entity: "project:alpha", Predicate: "note", Object: "ephemeral observation",
		Confidence: 0.4, Derivation: "extracted",
		SourceTraceRef: "trace:ephemeral", SnippetHash: "h_ephemeral",
		Visibility: "operational",
	}}, t0)
	if err != nil || len(got) != 1 {
		t.Fatalf("capture: %v %+v", err, got)
	}
	if got[0].State != "parked" || got[0].ExpiresAt == "" {
		t.Fatalf("want decay_parked candidate, got %+v", got[0])
	}
	past := t0.AddDate(0, 0, 31)
	report, err := s.SynthesisSweep([]string{"decay"}, past)
	if err != nil {
		t.Fatal(err)
	}
	if report.DecayedCount != 1 || len(report.Decayed) != 1 {
		t.Fatalf("decayed = %d actions=%+v, want 1", report.DecayedCount, report.Decayed)
	}
	if report.Decayed[0].CandidateID != got[0].ID || report.Decayed[0].Reason != DecayReasonExpired {
		t.Fatalf("action = %+v", report.Decayed[0])
	}
	c, err := s.getCandidate(got[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "no_promote" || c.Reason != DecayReasonExpired {
		t.Fatalf("candidate after decay = %+v", c)
	}
	// Idempotent: second run decays nothing.
	again, err := s.SynthesisSweep([]string{"decay"}, past)
	if err != nil {
		t.Fatal(err)
	}
	if again.DecayedCount != 0 {
		t.Fatalf("second decay should be no-op, got %d", again.DecayedCount)
	}
}

func TestDecaySweepNeverExpiresCorroboratedCandidate(t *testing.T) {
	s := newStore(t)
	// Park seed while still unexpired so corroborate can raise it.
	seedID := parkSeed(t, s, CandidateExpectation{
		Entity:    "project:alpha",
		Predicate: "runtime_outcome",
		Object:    "integration risk materialized",
	})
	promoteOutcome(t, s, "integration risk materialized", t0.AddDate(0, 0, 1))
	// Corroborate while unexpired — raises seed, clears ExpiresAt.
	reportRaise, err := s.SynthesisSweep([]string{"corroborate"}, t0.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(reportRaise.RaisedSeeds) != 1 {
		t.Fatalf("expected one raised seed, got %+v", reportRaise.RaisedSeeds)
	}
	seed, err := s.getCandidate(seedID)
	if err != nil {
		t.Fatal(err)
	}
	if seed.ExpiresAt != "" {
		t.Fatalf("raise should clear ExpiresAt, got %q", seed.ExpiresAt)
	}
	// Re-stamp expired to prove the corroboration mark (not just cleared ExpiresAt) is the guard.
	seed.ExpiresAt = rfc3339(t0)
	if err := candidates.WriteCandidate(s.home, candidates.Memory, seed.ID, &seed); err != nil {
		t.Fatal(err)
	}
	report, err := s.SynthesisSweep([]string{"decay"}, t0.AddDate(0, 0, 40))
	if err != nil {
		t.Fatal(err)
	}
	if report.DecayedCount != 0 {
		t.Fatalf("corroborated candidate must NEVER decay: %+v", report.Decayed)
	}
	c, err := s.getCandidate(seedID)
	if err != nil {
		t.Fatal(err)
	}
	if c.State == "no_promote" {
		t.Fatalf("corroborated seed was filed: %+v", c)
	}
}

func TestDecaySweepSkipsParkedWithoutExpiry(t *testing.T) {
	s := newStore(t)
	// knowledge_import parks without ExpiresAt — not landfill fuel.
	got, err := s.Capture([]Candidate{{
		Entity: "knowledge:src", Predicate: "title", Object: "Doc",
		Confidence: 0.8, Derivation: "deterministic",
		Qualifiers:     map[string]string{"source": "knowledge_import"},
		SourceTraceRef: "ev_src", SnippetHash: "h_src",
		Visibility: "candidate",
	}}, t0)
	if err != nil || len(got) != 1 {
		t.Fatalf("capture: %v", err)
	}
	if got[0].ExpiresAt != "" {
		t.Fatalf("knowledge_import should not stamp ExpiresAt: %+v", got[0])
	}
	report, err := s.SynthesisSweep([]string{"decay"}, t0.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if report.DecayedCount != 0 {
		t.Fatalf("source-index parks without expiry must not decay: %+v", report.Decayed)
	}
}

func TestDecaySweepSkipsOperatorPermanentCandidateEvenWhenExpired(t *testing.T) {
	s := newStore(t)
	c := cand("compound", "operator_decision", "keep human approval for publishing", rfc3339(t0))
	c.ID = "cand_operator_permanent_expired"
	c.ClaimType = "decision"
	c.State = "parked"
	c.ExpiresAt = rfc3339(t0.AddDate(0, 0, -1))
	c.CreatedAt = rfc3339(t0.AddDate(0, 0, -2))
	if err := candidates.WriteCandidate(s.home, candidates.Memory, c.ID, &c); err != nil {
		t.Fatal(err)
	}

	report, err := s.SynthesisSweep([]string{"decay"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.DecayedCount != 0 {
		t.Fatalf("operator-permanent candidate must never decay: %+v", report.Decayed)
	}
	got, err := s.getCandidate(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "parked" || got.Reason == DecayReasonExpired {
		t.Fatalf("operator-permanent candidate was filed: %+v", got)
	}
}

func TestRaiseSeedsRechecksCandidateAfterStaleProposal(t *testing.T) {
	s := newStore(t)
	seed := cand("project:api", "status", "integration risk will matter later", "")
	seed.PromotionBasis = ""
	seed.ClaimType = "status"
	seed.Salience = "low"
	seed.ExpiresAt = t0.Add(24 * time.Hour).Format(time.RFC3339)
	seed.Expectation = &CandidateExpectation{Entity: "project:api", Predicate: "runtime_outcome", Object: "risk materialized"}
	captured, err := s.Capture([]Candidate{seed}, t0)
	if err != nil || len(captured) != 1 {
		t.Fatalf("capture = %+v, err=%v", captured, err)
	}
	if _, err := s.Adjudicate(captured[0].ID, "park", "work_corroboration_race", t0); err != nil {
		t.Fatal(err)
	}
	proposal := SynthesisCorroborationProposal{
		ID:                "synth_corroboration_stale",
		SeedCandidateID:   captured[0].ID,
		EvidenceClaimID:   "claim_runtime_outcome",
		EvidenceSummary:   "project:api.runtime_outcome = risk materialized",
		EvidenceRefs:      []string{"claim:claim_runtime_outcome", "candidate:" + captured[0].ID},
		EvidenceCreatedAt: rfc3339(t0),
	}
	if _, err := s.Adjudicate(captured[0].ID, "no_promote", "work_corroboration_race", t0); err != nil {
		t.Fatal(err)
	}

	raised, err := s.raiseSeedsForCorroborations([]SynthesisCorroborationProposal{proposal}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(raised) != 0 {
		t.Fatalf("stale proposal raised terminal candidate: %+v", raised)
	}
	marks, err := s.candidateReviewMarksAsOf(0)
	if err != nil {
		t.Fatal(err)
	}
	if mark, ok := marks[captured[0].ID]; ok && len(mark.Refs) > 0 {
		t.Fatalf("stale proposal minted corroboration mark: %+v", mark)
	}
}

func TestRaiseSeedsFailsBeforeEdgeWhenCandidateIsMissing(t *testing.T) {
	s := newStore(t)
	proposal := SynthesisCorroborationProposal{
		ID:                "synth_corroboration_missing",
		SeedCandidateID:   "cand_missing",
		EvidenceClaimID:   "claim_runtime_outcome",
		EvidenceSummary:   "project:api.runtime_outcome = risk materialized",
		EvidenceRefs:      []string{"claim:claim_runtime_outcome", "candidate:cand_missing"},
		EvidenceCreatedAt: rfc3339(t0),
	}
	if _, err := s.raiseSeedsForCorroborations([]SynthesisCorroborationProposal{proposal}, t0); err == nil {
		t.Fatal("missing candidate produced a successful raised review")
	}
	marks, err := s.candidateReviewMarksAsOf(0)
	if err != nil {
		t.Fatal(err)
	}
	if mark, ok := marks[proposal.SeedCandidateID]; ok && len(mark.Refs) > 0 {
		t.Fatalf("missing candidate minted corroboration mark: %+v", mark)
	}
}

func TestRaiseSeedsFailsBeforeEdgeWhenCandidateProjectionWriteFails(t *testing.T) {
	s := newStore(t)
	seed := cand("project:api", "status", "integration risk will matter later", "")
	seed.PromotionBasis = ""
	seed.ClaimType = "status"
	seed.Salience = "low"
	seed.ExpiresAt = t0.Add(24 * time.Hour).Format(time.RFC3339)
	seed.Expectation = &CandidateExpectation{Entity: "project:api", Predicate: "runtime_outcome", Object: "risk materialized"}
	captured, err := s.Capture([]Candidate{seed}, t0)
	if err != nil || len(captured) != 1 {
		t.Fatalf("capture = %+v, err=%v", captured, err)
	}
	if _, err := s.Adjudicate(captured[0].ID, "park", "work_corroboration_write_failure", t0); err != nil {
		t.Fatal(err)
	}
	before, err := s.getCandidate(captured[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	proposal := SynthesisCorroborationProposal{
		ID:                "synth_corroboration_write_failure",
		SeedCandidateID:   captured[0].ID,
		EvidenceClaimID:   "claim_runtime_outcome",
		EvidenceSummary:   "project:api.runtime_outcome = risk materialized",
		EvidenceRefs:      []string{"claim:claim_runtime_outcome", "candidate:" + captured[0].ID},
		EvidenceCreatedAt: rfc3339(t0),
	}

	candidatePath := filepath.Join(s.candDir(), captured[0].ID+".json")
	info, err := os.Stat(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	originalMode := info.Mode().Perm()
	if err := os.Chmod(candidatePath, 0o400); err != nil {
		t.Fatal(err)
	}
	restoreMode := func() {
		if err := os.Chmod(candidatePath, originalMode); err != nil {
			t.Errorf("restore candidate mode: %v", err)
		}
	}
	t.Cleanup(restoreMode)
	if _, err := s.getCandidate(captured[0].ID); err != nil {
		t.Fatalf("candidate unreadable before projection failure: %v", err)
	}

	raised, err := s.raiseSeedsForCorroborations([]SynthesisCorroborationProposal{proposal}, t0)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("candidate projection error = %v, want permission failure", err)
	}
	if len(raised) != 0 {
		t.Fatalf("failed candidate projection raised review: %+v", raised)
	}
	restoreMode()

	edges, err := s.loadEdges()
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind == EdgeKindCorroboration && edge.To == captured[0].ID {
			t.Fatalf("failed candidate projection minted corroboration edge: %+v", edge)
		}
	}
	marks, err := s.candidateReviewMarksAsOf(0)
	if err != nil {
		t.Fatal(err)
	}
	if mark, ok := marks[captured[0].ID]; ok && len(mark.Refs) > 0 {
		t.Fatalf("failed candidate projection minted corroboration mark: %+v", mark)
	}
	after, err := s.getCandidate(captured[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || after.ExpiresAt != before.ExpiresAt {
		t.Fatalf("candidate changed after failed projection: before=%+v after=%+v", before, after)
	}
}

func parkSeed(t *testing.T, s *Store, expectation CandidateExpectation) string {
	t.Helper()
	seed := cand("project:alpha", "operator_stance", "integration risk will matter later", "")
	seed.Confidence = 0.25
	seed.PromotionBasis = ""
	seed.Visibility = "contextual"
	seed.Tags = []string{"seed"}
	seed.Salience = "low"
	seed.ExpiresAt = rfc3339(t0.AddDate(0, 1, 0))
	seed.Expectation = &expectation
	got, err := s.Capture([]Candidate{seed}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(got[0].ID, "park", "work_seed_park", t0); err != nil {
		t.Fatal(err)
	}
	return got[0].ID
}

func promoteOutcome(t *testing.T, s *Store, object string, now time.Time) AdjudicateResult {
	t.Helper()
	outcome := cand("project:alpha", "runtime_outcome", object, rfc3339(now))
	outcome.SourceTraceRef = "receipt:runtime-outcome"
	outcome.SnippetHash = "hash_runtime_outcome"
	got, err := s.Capture([]Candidate{outcome}, now)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := promoteForTest(t, s, got[0].ID, "work_runtime_outcome", now)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Claim == nil {
		t.Fatal("outcome should promote to claim")
	}
	return promoted
}
