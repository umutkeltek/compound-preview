package reviewartifact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReviewResolutionRevisionBindsCompleteRenderedQueueItem(t *testing.T) {
	base := QueueItem{
		Area:            "parity",
		ID:              "parity_revision_fixture",
		Status:          "unknown",
		OperatorSummary: "Review the private parity evidence.",
		ContextLabel:    "parity evidence",
		GroupKey:        "parity|evidence",
		OperatorUnlock:  "Choose whether this evidence should remain visible.",
		Band:            BandNeedsYou,
		SummarySource:   SummarySourceAnnotator,
		JSONPath:        "/private/reviews/parity/parity_revision_fixture.json",
		Readiness:       "needs_review",
		BlockedReason:   "owner action unavailable",
		NextAction:      "review the evidence",
		DecisionCard: &DecisionCard{
			Claim:   "The parity evidence needs an operator disposition.",
			Why:     []DecisionCardReason{{Label: "Evidence", Detail: "Owner evidence is inconclusive."}},
			Choices: []DecisionCardChoice{{ID: "park", Label: "Park", Effect: "Keep it visible later."}},
		},
		Warnings: []string{"fixture warning"},
	}

	want := ReviewResolutionRevision(base.Area, base)
	if !strings.HasPrefix(want, reviewResolutionRevisionPrefix) || len(want) != len(reviewResolutionRevisionPrefix)+64 {
		t.Fatalf("revision = %q, want privacy-safe sha256 token", want)
	}
	for _, privateText := range []string{base.OperatorSummary, base.JSONPath, base.DecisionCard.Claim} {
		if strings.Contains(want, privateText) {
			t.Fatalf("revision leaked rendered content %q: %s", privateText, want)
		}
	}

	tests := []struct {
		name   string
		mutate func(*QueueItem)
	}{
		{"actions", func(item *QueueItem) {
			item.Actions = []Action{{ID: "owner_action", Argv: []string{"compound", "owner", "act"}}}
		}},
		{"decision card", func(item *QueueItem) { item.DecisionCard.Claim = "Owner truth changed." }},
		{"readiness", func(item *QueueItem) { item.Readiness = "ready_to_apply" }},
		{"band", func(item *QueueItem) { item.Band = BandWatching }},
		{"blocked reason", func(item *QueueItem) { item.BlockedReason = "a different blocker" }},
		{"next action", func(item *QueueItem) { item.NextAction = "take a different next action" }},
		{"operator summary", func(item *QueueItem) { item.OperatorSummary = "A different rendered summary." }},
		{"context label", func(item *QueueItem) { item.ContextLabel = "changed context" }},
		{"operator unlock", func(item *QueueItem) { item.OperatorUnlock = "A different unlock." }},
		{"warnings", func(item *QueueItem) { item.Warnings = append(item.Warnings, "new warning") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := cloneQueueItemForRevisionTest(t, base)
			tt.mutate(&changed)
			if got := ReviewResolutionRevision(changed.Area, changed); got == want {
				t.Fatalf("revision did not change after %s changed", tt.name)
			}
		})
	}
	if got := ReviewResolutionRevision("source", base); got == want {
		t.Fatal("revision did not change when the scoped area changed")
	}
	withOldRevision := base
	withOldRevision.ReviewResolutionRevision = "review-resolution-v2:sha256:old"
	if got := ReviewResolutionRevision(base.Area, withOldRevision); got != want {
		t.Fatalf("revision field must be excluded from its own preimage: got %s want %s", got, want)
	}
}

func TestRecountStampsItemsAndGroupSamplesAfterDerivedState(t *testing.T) {
	report := QueueReport{Items: []QueueItem{{
		Area:            "parity",
		ID:              "parity_stamp_fixture",
		Status:          "review_required",
		OperatorSummary: "Review this parity fixture.",
		ContextLabel:    "parity fixture",
		Band:            BandNeedsYou,
		Readiness:       "needs_review",
		JSONPath:        "/private/parity_stamp_fixture.json",
	}}}
	Recount(&report)
	if len(report.Items) != 1 || report.Items[0].ReviewResolutionRevision == "" {
		t.Fatalf("recount did not stamp item revision: %+v", report.Items)
	}
	if len(report.Groups) != 1 || report.Groups[0].Sample == nil {
		t.Fatalf("recount did not build group sample: %+v", report.Groups)
	}
	if got, want := report.Groups[0].Sample.ReviewResolutionRevision, report.Items[0].ReviewResolutionRevision; got != want {
		t.Fatalf("group sample revision = %q, want item revision %q", got, want)
	}
}

func cloneQueueItemForRevisionTest(t *testing.T, item QueueItem) QueueItem {
	t.Helper()
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var clone QueueItem
	if err := json.Unmarshal(body, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}
