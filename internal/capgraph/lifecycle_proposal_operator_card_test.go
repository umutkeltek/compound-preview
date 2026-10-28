package capgraph

import (
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

// TestAnnotateCapabilityLifecycleProposalQueueFillsFallback covers gap #1's
// capability-lifecycle-proposals area: no annotator existed for it at all,
// so every legacy proposal rendered the generic fallback sentence.
func TestAnnotateCapabilityLifecycleProposalQueueFillsFallback(t *testing.T) {
	homePath := t.TempDir()
	proposal := CapabilityLifecycleProposal{
		SchemaVersion: CapabilityLifecycleProposalSchemaVersion,
		ID:            "clp_test",
		CreatedAt:     "2026-07-01T00:00:00Z",
		Status:        "review_required",
		ProposalKind:  "revise",
		Subject:       "route promotion criteria",
		Statement:     "loosen the minimum sample bar for classify-extract",
		EvidenceRefs:  []string{"compound://route-promotions"},
		Draft: CapabilityLifecycleProposalDraft{
			RequiredEvidence: []string{"a", "b", "c", "d", "e"},
		},
	}
	if _, err := reviewartifact.WriteJSON(homePath, CapabilityLifecycleProposalReviewArea, proposal.ID+".json", proposal, "  "); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy proposal should start as fallback, got %+v", report.Items[0])
	}
	AnnotateCapabilityLifecycleProposalQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "Capability screening proposes a lifecycle change touching 5 evidence hooks. Review the proposal?"
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandNeedsYou {
		t.Fatalf("Band = %q, want needs_you (still open)", item.Band)
	}
	if item.SummarySource != reviewartifact.SummarySourceAnnotator {
		t.Fatalf("SummarySource = %q, want annotator", item.SummarySource)
	}

	// Once recorded, the card must file, not keep asking.
	proposal.Status = "recorded"
	if _, err := reviewartifact.WriteJSON(homePath, CapabilityLifecycleProposalReviewArea, proposal.ID+".json", proposal, "  "); err != nil {
		t.Fatal(err)
	}
	report, err = reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityLifecycleProposalQueue(&report)
	reviewartifact.Recount(&report)
	if report.Items[0].Band != reviewartifact.BandFiled {
		t.Fatalf("recorded proposal Band = %q, want filed", report.Items[0].Band)
	}
}
