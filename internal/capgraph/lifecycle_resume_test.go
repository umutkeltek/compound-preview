package capgraph

import (
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

func TestCapabilityLifecycleReservationProjectsOneExactResumeAction(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	plan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
		Decision: CapabilityLifecycleOpenWork, Rationale: "Resume only this reserved rationale.", Actor: "operator-test",
	}, lifecycleReviewNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := reserveCapabilityLifecycleDisposition(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, lifecycleReviewNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityLifecycleProposalQueue(&report)
	reviewartifact.Recount(&report)
	item := report.Items[0]
	if item.Band != reviewartifact.BandNeedsYou || item.Status != "held" || item.Readiness != "ready_to_apply" {
		t.Fatalf("reserved item must expose recovery authority: %+v", item)
	}
	if len(item.Actions) != 1 || item.Actions[0].ID != "resume_open_work" || !item.Actions[0].RequiresReview {
		t.Fatalf("reserved actions = %+v", item.Actions)
	}
	if item.DecisionCard == nil || len(item.DecisionCard.Choices) != 1 || item.DecisionCard.Choices[0].ID != "resume_open_work" {
		t.Fatalf("reserved decision card = %+v", item.DecisionCard)
	}
	if item.DecisionCard.Meta["reservation_id"] != reservation.ID || item.DecisionCard.Meta["reserved_disposition_id"] != reservation.DispositionID || item.DecisionCard.Meta["proposal_hash"] != snapshot.Hash {
		t.Fatalf("reserved decision metadata = %+v", item.DecisionCard.Meta)
	}
	argv := item.Actions[0].Argv
	for flag, want := range map[string]string{
		"--proposal":      snapshot.Proposal.ID,
		"--expected-hash": snapshot.Hash,
		"--decision":      CapabilityLifecycleOpenWork,
		"--rationale":     reservation.Rationale,
		"--actor":         reservation.Actor,
	} {
		if got := lifecycleResumeArg(argv, flag); got != want {
			t.Fatalf("resume argv %s = %q, want %q; argv=%+v", flag, got, want, argv)
		}
	}
	for _, forbidden := range []string{CapabilityLifecycleDefer, CapabilityLifecycleReject} {
		for _, choice := range item.DecisionCard.Choices {
			if choice.ID == forbidden {
				t.Fatalf("reserved card reoffered competing choice %q: %+v", forbidden, item.DecisionCard.Choices)
			}
		}
	}
}

func TestCapabilityLifecycleDueDeferReservationProjectsExactResumeUntilFinalDisposition(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	deferUntil := lifecycleReviewNow.Add(2 * time.Hour)
	deferPlan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
		Decision: CapabilityLifecycleDefer, Rationale: "Wait for the bounded evaluation window.", Actor: "operator-test",
		DeferUntil: deferUntil.Format(time.RFC3339),
	}, lifecycleReviewNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordCapabilityLifecycleDisposition(homePath, deferPlan, CapabilityLifecycleDispositionLinks{}); err != nil {
		t.Fatal(err)
	}

	openPlan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
		Decision: CapabilityLifecycleOpenWork, Rationale: "Resume only this due-generation work decision.", Actor: "operator-test",
	}, deferUntil.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := reserveCapabilityLifecycleDisposition(homePath, openPlan)
	if err != nil {
		t.Fatal(err)
	}

	queueAt := deferUntil.Add(2 * time.Minute)
	item := lifecycleQueuedProposalItem(t, homePath, queueAt)
	if item.Band != reviewartifact.BandNeedsYou || item.Status != "held" || item.Readiness != "ready_to_apply" {
		t.Fatalf("due defer with unresolved reservation must expose recovery authority: %+v", item)
	}
	if len(item.Actions) != 1 || item.Actions[0].ID != "resume_open_work" {
		t.Fatalf("due defer with unresolved reservation actions = %+v", item.Actions)
	}
	if item.DecisionCard == nil || len(item.DecisionCard.Choices) != 1 || item.DecisionCard.Choices[0].ID != "resume_open_work" {
		t.Fatalf("due defer with unresolved reservation card = %+v", item.DecisionCard)
	}
	if item.DecisionCard.Meta["reservation_id"] != reservation.ID {
		t.Fatalf("projected reservation = %+v, want %s", item.DecisionCard.Meta, reservation.ID)
	}

	links := CapabilityLifecycleDispositionLinks{
		WorkID: "work_after_defer", WorkState: "proposed", WorkPlanID: "wcrp_after_defer",
		ActionIntentID: "act_after_defer", PolicyDecisionID: "pol_after_defer",
	}
	if _, err := RecordCapabilityLifecycleDisposition(homePath, openPlan, links); err != nil {
		t.Fatal(err)
	}
	item = lifecycleQueuedProposalItem(t, homePath, queueAt)
	if item.Status != "recorded" || item.Readiness != "no_action" || len(item.Actions) != 0 {
		t.Fatalf("matching final disposition must resolve the reservation: %+v", item)
	}
	if item.DecisionCard == nil || item.DecisionCard.Meta["decision_id"] != reservation.DispositionID || len(item.DecisionCard.Choices) != 0 {
		t.Fatalf("matching final disposition card = %+v", item.DecisionCard)
	}
}

func lifecycleQueuedProposalItem(t *testing.T, homePath string, now time.Time) reviewartifact.QueueItem {
	t.Helper()
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, now)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityLifecycleProposalQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Items) != 1 {
		t.Fatalf("queue items = %d, want 1", len(report.Items))
	}
	return report.Items[0]
}

func lifecycleResumeArg(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}
