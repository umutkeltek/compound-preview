package mem

import (
	"strings"
	"testing"
	"time"
)

func TestSynthesisReconcileDigestTiersAndUndoRestoresReviewMarker(t *testing.T) {
	s := newStore(t)
	oldDecision := reconcileClaim("claim_digest_old", 1, "project:digest", "runtime", "Go", rfc3339(t0))
	oldDecision.CanonClass = "decision"
	newDecision := reconcileClaim("claim_digest_new", 2, "project:digest", "runtime", "Rust", rfc3339(t0.AddDate(0, 0, 1)))
	newDecision.CanonClass = "decision"
	used := reconcileClaim("claim_digest_used", 3, "project:digest", "owner", "team-a", rfc3339(t0))
	low := reconcileClaim("claim_digest_low", 4, "project:digest", "owner", "team-b", rfc3339(t0))
	low.Confidence = 0.2
	for _, claim := range []Claim{oldDecision, newDecision, used, low} {
		appendReconcileClaim(t, s, claim)
	}
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_digest_reopen",
		CreatedAt: rfc3339(t0.AddDate(0, 0, 1).Add(time.Hour)),
		Scope:     newDecision.Entity,
		ClaimRefs: []string{newDecision.ID},
	})
	writeReconcileCloseReceipt(t, s, "rec_digest_usage", "tr_digest_usage", rfc3339(t0.AddDate(0, 0, 1).Add(2*time.Hour)), []string{"claim://" + used.ID})
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_digest_low",
		CreatedAt: rfc3339(t0.AddDate(0, 0, 1).Add(3 * time.Hour)),
		Scope:     low.Entity,
		ClaimRefs: []string{low.ID},
	})

	if _, err := s.SynthesisSweep([]string{"reconcile"}, t0.AddDate(0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	beforeUndo := claimByID(mustLoadClaims(t, s), oldDecision.ID)
	if beforeUndo.State != ClaimStateNeedsReview {
		t.Fatalf("fixture did not create review marker: %+v", beforeUndo)
	}

	digest, err := s.SynthesisReconcileDigest(t0.AddDate(0, 0, 2).Add(time.Hour), SynthesisReconcileDigestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if digest.ZeroAction {
		t.Fatalf("digest should not be zero-action: %+v", digest)
	}
	decisionLine := findDigestLine(digest.Tier1, "reopened", oldDecision.ID)
	if decisionLine == nil || !strings.Contains(decisionLine.Summary, "decision") || decisionLine.UndoCommand == "" {
		t.Fatalf("missing tier-1 decision reopen line: %+v", digest.Tier1)
	}
	for _, want := range []string{"reopened", "decay_routed", "parked"} {
		if line := findDigestLine(digest.Tier2, want, ""); line == nil || line.Count != 1 || line.UndoCommand == "" {
			t.Fatalf("missing tier-2 %s count line in %+v", want, digest.Tier2)
		}
	}
	if parked := findDigestLine(digest.Tier2, "parked", ""); parked == nil || !strings.Contains(parked.Summary, reconcileParkLowConfidence) {
		t.Fatalf("parked line should include top reason %q: %+v", reconcileParkLowConfidence, parked)
	}
	if err := s.ApplySynthesisReconcileDigestUndoCommand(decisionLine.UndoCommand, t0.AddDate(0, 0, 2).Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	afterUndo := claimByID(mustLoadClaims(t, s), oldDecision.ID)
	if afterUndo.State == ClaimStateNeedsReview || len(afterUndo.ReviewRefs) != 0 {
		t.Fatalf("undo command did not restore claim review state: %+v", afterUndo)
	}
}

func findDigestLine(lines []SynthesisReconcileDigestLine, kind, contains string) *SynthesisReconcileDigestLine {
	for i := range lines {
		if lines[i].Kind != kind {
			continue
		}
		if contains == "" || strings.Contains(lines[i].Summary, contains) || strings.Contains(strings.Join(lines[i].Refs, ","), contains) {
			return &lines[i]
		}
	}
	return nil
}
