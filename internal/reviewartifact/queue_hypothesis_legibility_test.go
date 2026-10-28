package reviewartifact

import (
	"strings"
	"testing"
)

// TestQueueGroupActionProductHypothesisLegibility is the W5 J9-R3G
// read-for-the-operator red falsifier: when a queue group is the
// product-hypothesis area, the generic "owner has not exposed a safe action
// for this decision group" copy is illegible. The queue must surface a plain
// "approve to give the loop real evidence" sentence instead.
// Other areas keep the original fail-closed copy so we don't drift.
func TestQueueGroupActionProductHypothesisLegibility(t *testing.T) {
	report := QueueReport{Items: []QueueItem{
		systemicReviewItem("product-hypotheses", "product-hypotheses|hungry", "hungry_review"),
		systemicReviewItem("decisions", "decisions|legacy", "legacy_review"),
	}}
	Recount(&report)
	if len(report.Groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(report.Groups))
	}
	hypReason, legacyReason := "", ""
	for _, group := range report.Groups {
		switch group.Area {
		case "product-hypotheses":
			hypReason = group.TerminalReason
		case "decisions":
			legacyReason = group.TerminalReason
		}
	}
	// The product-hypotheses line is the one a tired operator sees on a
	// starved loop; it must read as plain language, not jargon.
	if hypReason == "" {
		t.Fatal("product-hypotheses group must surface a TerminalReason")
	}
	if strings.Contains(hypReason, "owner has not exposed") {
		t.Fatalf("product-hypotheses group must NOT carry the illegible fail-closed copy, got %q", hypReason)
	}
	if !strings.Contains(strings.ToLower(hypReason), "hypothesis") {
		t.Fatalf("product-hypotheses group must name what the operator is being asked to review, got %q", hypReason)
	}
	if !strings.Contains(strings.ToLower(hypReason), "approve") {
		t.Fatalf("product-hypotheses group must give a concrete operator move, got %q", hypReason)
	}
	if !Readable(hypReason) {
		t.Fatalf("the new sentence must stay at or below eighth grade (%.2f): %q", GradeLevel(hypReason), hypReason)
	}
	// Other areas keep the legacy copy; this test is a regression guard
	// against a too-broad swap that drifts every queue area.
	if !strings.Contains(legacyReason, "owner has not exposed") {
		t.Fatalf("non-product-hypothesis area must keep the legacy copy, got %q", legacyReason)
	}
}
