package capgraph

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

var lifecycleReviewNow = time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

func lifecycleReviewFixture(t *testing.T, homePath string) CapabilityLifecycleProposalSnapshot {
	t.Helper()
	proposal, err := BuildCapabilityLifecycleProposal(CapabilityLifecycleProposalInput{
		ProposalKind:  "revise",
		Subject:       "session-mined skill update",
		Statement:     "teach the existing skill one verified failure pattern",
		Detail:        "The source session found a repeated failure with a concrete regression check.",
		Confidence:    0.82,
		EvidenceRefs:  []string{"session:fixture", "check:fixture"},
		CapabilityRef: "skill:fixture",
	}, lifecycleReviewNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordCapabilityLifecycleProposal(homePath, proposal); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadCapabilityLifecycleProposalSnapshot(homePath, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCapabilityLifecycleDispositionRejectsStaleProjectionAndRequiresSemanticInputs(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)

	_, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID:   snapshot.Proposal.ID,
		ExpectedHash: strings.Repeat("0", 64),
		Decision:     CapabilityLifecycleOpenWork,
		Rationale:    "This needs bounded implementation work.",
		Actor:        "operator-test",
	}, lifecycleReviewNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "changed since preview") {
		t.Fatalf("stale proposal should be rejected, got %v", err)
	}

	_, err = PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID:   snapshot.Proposal.ID,
		ExpectedHash: snapshot.Hash,
		Decision:     CapabilityLifecycleReject,
		Actor:        "operator-test",
	}, lifecycleReviewNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "rationale required") {
		t.Fatalf("empty rationale should be rejected, got %v", err)
	}

	_, err = PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID:   snapshot.Proposal.ID,
		ExpectedHash: snapshot.Hash,
		Decision:     CapabilityLifecycleDefer,
		Rationale:    "Wait for the named evaluation.",
		Actor:        "operator-test",
	}, lifecycleReviewNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "defer_until required") {
		t.Fatalf("defer without trigger should be rejected, got %v", err)
	}
}

func TestCapabilityLifecycleDispositionIsIdempotentAndDrivesQueueBand(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	plan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID:   snapshot.Proposal.ID,
		ExpectedHash: snapshot.Hash,
		Decision:     CapabilityLifecycleOpenWork,
		Rationale:    "Create bounded owner work; do not mutate the capability graph here.",
		Actor:        "operator-test",
	}, lifecycleReviewNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	links := CapabilityLifecycleDispositionLinks{
		WorkID:           "work_fixture",
		WorkState:        "proposed",
		WorkPlanID:       "wcrp_fixture",
		ActionIntentID:   "act_fixture",
		PolicyDecisionID: "pol_fixture",
	}
	first, err := RecordCapabilityLifecycleDisposition(homePath, plan, links)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.AfterBand != reviewartifact.BandFiled || first.WorkID != links.WorkID {
		t.Fatalf("unexpected first disposition: %+v", first)
	}
	second, err := RecordCapabilityLifecycleDisposition(homePath, plan, links)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed || second.Status != "already_recorded" || second.ID != first.ID {
		t.Fatalf("idempotent replay should report no delta: first=%+v second=%+v", first, second)
	}

	conflict, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID:   snapshot.Proposal.ID,
		ExpectedHash: snapshot.Hash,
		Decision:     CapabilityLifecycleReject,
		Rationale:    "The evidence does not justify owner work.",
		Actor:        "operator-test",
	}, lifecycleReviewNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordCapabilityLifecycleDisposition(homePath, conflict, CapabilityLifecycleDispositionLinks{}); err == nil || !strings.Contains(err.Error(), "already has a different disposition") {
		t.Fatalf("conflicting replay should fail, got %v", err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, lifecycleReviewNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityLifecycleProposalQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Items) != 1 {
		t.Fatalf("queue items = %d, want 1", len(report.Items))
	}
	item := report.Items[0]
	if item.Band != reviewartifact.BandFiled || item.Status != "recorded" || item.Readiness != "no_action" {
		t.Fatalf("recorded open_work should file the proposal: %+v", item)
	}
	if item.DecisionCard == nil || item.DecisionCard.Meta["work_id"] != links.WorkID || item.DecisionCard.Meta["decision_rationale"] != plan.Input.Rationale || len(item.DecisionCard.Choices) != 0 {
		t.Fatalf("filed card should preserve linked work: %+v", item.DecisionCard)
	}
	if len(item.Actions) != 0 {
		t.Fatalf("filed proposal must expose no action: %+v", item.Actions)
	}
	if report.Counts.ReviewRequired != 0 || report.Counts.OK != 1 || len(report.Groups) != 0 {
		t.Fatalf("filed proposal should leave active review accounting: counts=%+v groups=%+v", report.Counts, report.Groups)
	}

	core, err := os.ReadFile(snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(core), `"status": "review_required"`) {
		t.Fatalf("decision must not rewrite proposal evidence:\n%s", core)
	}
	if _, err := os.Stat(filepath.Join(homePath, "capability", "nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("disposition must not mutate CapabilityGraph, stat err=%v", err)
	}
}

func TestApplyCapabilityLifecycleDispositionSerializesWorkCreation(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	planFor := func(rationale string) CapabilityLifecycleDispositionPlan {
		plan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
			ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
			Decision: CapabilityLifecycleOpenWork, Rationale: rationale, Actor: "operator-test",
		}, lifecycleReviewNow.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	firstPlan := planFor("Create the first bounded work item.")
	secondPlan := planFor("Create a conflicting bounded work item.")
	firstLinks := CapabilityLifecycleDispositionLinks{
		WorkID: "work_first", WorkState: "proposed", WorkPlanID: "wcrp_first",
		ActionIntentID: "act_first", PolicyDecisionID: "pol_first",
	}
	secondLinks := CapabilityLifecycleDispositionLinks{
		WorkID: "work_second", WorkState: "proposed", WorkPlanID: "wcrp_second",
		ActionIntentID: "act_second", PolicyDecisionID: "pol_second",
	}

	type applyResult struct {
		disposition CapabilityLifecycleDisposition
		err         error
	}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	results := make(chan applyResult, 2)
	var firstCreates, secondCreates atomic.Int32

	go func() {
		disposition, err := ApplyCapabilityLifecycleDisposition(homePath, firstPlan, func() (CapabilityLifecycleDispositionLinks, error) {
			firstCreates.Add(1)
			close(firstStarted)
			<-releaseFirst
			return firstLinks, nil
		})
		results <- applyResult{disposition: disposition, err: err}
	}()
	<-firstStarted
	go func() {
		close(secondEntered)
		disposition, err := ApplyCapabilityLifecycleDisposition(homePath, secondPlan, func() (CapabilityLifecycleDispositionLinks, error) {
			secondCreates.Add(1)
			return secondLinks, nil
		})
		results <- applyResult{disposition: disposition, err: err}
	}()
	<-secondEntered
	close(releaseFirst)

	first := <-results
	second := <-results
	if first.err != nil && second.err == nil {
		first, second = second, first
	}
	if first.err != nil || first.disposition.WorkID != firstLinks.WorkID {
		t.Fatalf("winning disposition = %+v, err=%v", first.disposition, first.err)
	}
	if second.err == nil || !strings.Contains(second.err.Error(), "already has a different disposition") {
		t.Fatalf("conflicting disposition should lose before work creation: %+v, err=%v", second.disposition, second.err)
	}
	if firstCreates.Load() != 1 || secondCreates.Load() != 0 {
		t.Fatalf("work creators called first=%d second=%d, want 1/0", firstCreates.Load(), secondCreates.Load())
	}

	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		t.Fatal(err)
	}
	if history, err := capabilityLifecycleDispositionHistory(dir, snapshot.Proposal.ID, ""); err != nil || len(history) != 1 {
		t.Fatalf("disposition history = %+v, err=%v, want exactly one", history, err)
	}
}

func TestApplyCapabilityLifecycleDispositionReservationSurvivesFinalWriteFailure(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	planFor := func(rationale string) CapabilityLifecycleDispositionPlan {
		plan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
			ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
			Decision: CapabilityLifecycleOpenWork, Rationale: rationale, Actor: "operator-test",
		}, lifecycleReviewNow.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	winner := planFor("Create the crash-recoverable bounded work item.")
	conflict := planFor("Create a different work item after the failed final write.")
	links := CapabilityLifecycleDispositionLinks{
		WorkID: "work_reserved", WorkState: "proposed", WorkPlanID: "wcrp_reserved",
		ActionIntentID: "act_reserved", PolicyDecisionID: "pol_reserved",
	}
	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(dir, snapshot.Proposal.ID+"."+capabilityLifecycleDispositionID(winner)+".decision.json")
	if err := os.Mkdir(finalPath, 0o755); err != nil {
		t.Fatal(err)
	}

	winnerCalls := 0
	_, err = ApplyCapabilityLifecycleDisposition(homePath, winner, func() (CapabilityLifecycleDispositionLinks, error) {
		winnerCalls++
		return links, nil
	})
	if err == nil {
		t.Fatal("sabotaged final disposition write should fail")
	}

	conflictCalls := 0
	_, conflictErr := ApplyCapabilityLifecycleDisposition(homePath, conflict, func() (CapabilityLifecycleDispositionLinks, error) {
		conflictCalls++
		return CapabilityLifecycleDispositionLinks{
			WorkID: "work_conflict", WorkState: "proposed", WorkPlanID: "wcrp_conflict",
			ActionIntentID: "act_conflict", PolicyDecisionID: "pol_conflict",
		}, nil
	})
	if conflictErr == nil || CapabilityLifecycleReviewErrorCode(conflictErr) != CapabilityLifecycleErrorDispositionConflict {
		t.Fatalf("different semantics should lose to durable reservation, got %v", conflictErr)
	}
	if conflictCalls != 0 {
		t.Fatalf("conflicting work creator called %d times, want 0", conflictCalls)
	}
	rejectPlan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
		Decision: CapabilityLifecycleReject, Rationale: "Reject after the reserved work callback.", Actor: "operator-test",
	}, lifecycleReviewNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyCapabilityLifecycleDisposition(homePath, rejectPlan, nil); err == nil || CapabilityLifecycleReviewErrorCode(err) != CapabilityLifecycleErrorDispositionConflict {
		t.Fatalf("reject must not contradict a reserved open_work disposition, got %v", err)
	}

	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, lifecycleReviewNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityLifecycleProposalQueue(&queue)
	reviewartifact.Recount(&queue)
	if got := queue.Items[0]; got.Band != reviewartifact.BandNeedsYou || got.Status != "held" || got.Readiness != "ready_to_apply" || len(got.Actions) != 1 || got.Actions[0].ID != "resume_open_work" {
		t.Fatalf("reserved partial disposition must expose only exact recovery: %+v", got)
	}

	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	recovered, err := ApplyCapabilityLifecycleDisposition(homePath, winner, func() (CapabilityLifecycleDispositionLinks, error) {
		winnerCalls++
		return links, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.WorkID != links.WorkID || winnerCalls != 2 {
		t.Fatalf("exact retry did not recover reserved winner: disposition=%+v calls=%d", recovered, winnerCalls)
	}
	if history, err := capabilityLifecycleDispositionHistory(dir, snapshot.Proposal.ID, ""); err != nil || len(history) != 1 {
		t.Fatalf("recovered history = %+v, err=%v, want exactly one final disposition", history, err)
	}
	reservations, err := filepath.Glob(filepath.Join(dir, snapshot.Proposal.ID+".*.reservation.decision.json"))
	if err != nil || len(reservations) != 1 {
		t.Fatalf("winner reservations = %v, err=%v", reservations, err)
	}
}

func TestApplyCapabilityLifecycleDispositionFailsClosedOnMalformedDispositionEvidence(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	plan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
		Decision: CapabilityLifecycleReject, Rationale: "Reject only if no disposition already exists.", Actor: "operator-test",
	}, lifecycleReviewNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		t.Fatal(err)
	}
	malformedPath := filepath.Join(dir, snapshot.Proposal.ID+".cld_ambiguous.decision.json")
	malformed := []byte(`{"schema_version":`)
	if err := os.WriteFile(malformedPath, malformed, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ApplyCapabilityLifecycleDisposition(homePath, plan, nil)
	if err == nil || CapabilityLifecycleReviewErrorCode(err) != CapabilityLifecycleErrorDispositionConflict {
		t.Fatalf("malformed matching disposition evidence must block a new disposition, got %v", err)
	}
	if got, readErr := os.ReadFile(malformedPath); readErr != nil || string(got) != string(malformed) {
		t.Fatalf("malformed disposition evidence changed: bytes=%q err=%v", got, readErr)
	}
	newPath := filepath.Join(dir, snapshot.Proposal.ID+"."+capabilityLifecycleDispositionID(plan)+".decision.json")
	if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
		t.Fatalf("contradictory disposition was published despite ambiguous evidence: %v", statErr)
	}
}

func TestApplyCapabilityLifecycleDispositionHandlesPoisonedCanonicalSidecars(t *testing.T) {
	planFor := func(t *testing.T, homePath string, snapshot CapabilityLifecycleProposalSnapshot) CapabilityLifecycleDispositionPlan {
		t.Helper()
		plan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
			ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
			Decision: CapabilityLifecycleOpenWork, Rationale: "Create one crash-recoverable bounded work item.", Actor: "operator-test",
		}, lifecycleReviewNow.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	links := CapabilityLifecycleDispositionLinks{
		WorkID: "work_repaired", WorkState: "proposed", WorkPlanID: "wcrp_repaired",
		ActionIntentID: "act_repaired", PolicyDecisionID: "pol_repaired",
	}

	t.Run("reservation", func(t *testing.T) {
		homePath := t.TempDir()
		snapshot := lifecycleReviewFixture(t, homePath)
		plan := planFor(t, homePath, snapshot)
		dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
		if err != nil {
			t.Fatal(err)
		}
		reservationPath := capabilityLifecycleReservationPath(dir, snapshot.Proposal.ID, "root")
		poison := []byte(`{"schema_version":`)
		if err := os.WriteFile(reservationPath, poison, 0o644); err != nil {
			t.Fatal(err)
		}
		rejectPlan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
			ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
			Decision: CapabilityLifecycleReject, Rationale: "Do not seize an ambiguous open-work reservation.", Actor: "operator-test",
		}, lifecycleReviewNow.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyCapabilityLifecycleDisposition(homePath, rejectPlan, nil); err == nil {
			t.Fatal("non-open disposition must not replace a poisoned reservation")
		}
		if stillPoisoned, err := os.ReadFile(reservationPath); err != nil || string(stillPoisoned) != string(poison) {
			t.Fatalf("non-open disposition changed poisoned reservation: bytes=%q err=%v", stillPoisoned, err)
		}

		creatorCalls := 0
		_, err = ApplyCapabilityLifecycleDisposition(homePath, plan, func() (CapabilityLifecycleDispositionLinks, error) {
			creatorCalls++
			return links, nil
		})
		if err == nil {
			t.Fatal("open_work must not guess the winner of a poisoned reservation")
		}
		if creatorCalls != 0 {
			t.Fatalf("poisoned reservation invoked work creator %d times", creatorCalls)
		}
		if stillPoisoned, err := os.ReadFile(reservationPath); err != nil || string(stillPoisoned) != string(poison) {
			t.Fatalf("open_work changed ambiguous poisoned reservation: bytes=%q err=%v", stillPoisoned, err)
		}
		assertNoCapabilityLifecyclePublicationTemps(t, dir)
	})

	t.Run("final disposition", func(t *testing.T) {
		homePath := t.TempDir()
		snapshot := lifecycleReviewFixture(t, homePath)
		plan := planFor(t, homePath, snapshot)
		dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
		if err != nil {
			t.Fatal(err)
		}
		finalPath := filepath.Join(dir, snapshot.Proposal.ID+"."+capabilityLifecycleDispositionID(plan)+".decision.json")
		if err := os.WriteFile(finalPath, []byte(`{"schema_version":`), 0o644); err != nil {
			t.Fatal(err)
		}

		created, err := ApplyCapabilityLifecycleDisposition(homePath, plan, func() (CapabilityLifecycleDispositionLinks, error) {
			return links, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.WorkID != links.WorkID {
			t.Fatalf("repaired final disposition = %+v", created)
		}
		persisted, ok := loadCapabilityLifecycleDisposition(finalPath)
		if !ok || persisted.ID != created.ID || persisted.WorkID != links.WorkID {
			t.Fatalf("persisted repaired disposition = %+v, ok=%t", persisted, ok)
		}
		assertNoCapabilityLifecyclePublicationTemps(t, dir)
	})
}

func assertNoCapabilityLifecyclePublicationTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("capability lifecycle publication temps leaked: %v", matches)
	}
}

func TestCapabilityLifecycleOpenCardExposesServerOwnedChoicesAndHash(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, lifecycleReviewNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityLifecycleProposalQueue(&report)
	reviewartifact.Recount(&report)
	item := report.Items[0]
	if item.DecisionCard == nil {
		t.Fatal("open proposal should expose a decision card")
	}
	if got := item.DecisionCard.Meta["proposal_hash"]; got != snapshot.Hash {
		t.Fatalf("proposal_hash = %q, want %q", got, snapshot.Hash)
	}
	if len(item.DecisionCard.Choices) != 3 {
		t.Fatalf("choices = %+v", item.DecisionCard.Choices)
	}
	for _, choice := range item.DecisionCard.Choices {
		if len(choice.Argv) != 0 || choice.Command != "" {
			t.Fatalf("semantic choice must not carry executable argv before rationale/hash revalidation: %+v", choice)
		}
	}
}

func TestCapabilityLifecycleDeferResurfacesAtTriggerAndCanBeSuperseded(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	deferUntil := lifecycleReviewNow.Add(2 * time.Hour).Format(time.RFC3339)
	deferPlan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
		Decision: CapabilityLifecycleDefer, Rationale: "Wait for the named evaluation result.",
		Actor: "operator-test", DeferUntil: deferUntil,
	}, lifecycleReviewNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := RecordCapabilityLifecycleDisposition(homePath, deferPlan, CapabilityLifecycleDispositionLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if deferred.AfterBand != reviewartifact.BandWatching {
		t.Fatalf("deferred = %+v", deferred)
	}

	beforeDue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, lifecycleReviewNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityLifecycleProposalQueue(&beforeDue)
	reviewartifact.Recount(&beforeDue)
	if beforeDue.Items[0].Band != reviewartifact.BandWatching || beforeDue.Items[0].Status != "held" {
		t.Fatalf("defer should watch before trigger: %+v", beforeDue.Items[0])
	}

	afterDueAt := lifecycleReviewNow.Add(3 * time.Hour)
	afterDue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: CapabilityLifecycleProposalReviewArea}, afterDueAt)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityLifecycleProposalQueue(&afterDue)
	reviewartifact.Recount(&afterDue)
	if afterDue.Items[0].Band != reviewartifact.BandNeedsYou || afterDue.Items[0].Status != "review_required" || afterDue.Items[0].Readiness != "needs_review" {
		t.Fatalf("due defer should resurface: %+v", afterDue.Items[0])
	}

	rejectPlan, err := PlanCapabilityLifecycleDisposition(homePath, CapabilityLifecycleDispositionInput{
		ProposalID: snapshot.Proposal.ID, ExpectedHash: snapshot.Hash,
		Decision: CapabilityLifecycleReject, Rationale: "The evaluation disproved the proposal.", Actor: "operator-test",
	}, afterDueAt)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := RecordCapabilityLifecycleDisposition(homePath, rejectPlan, CapabilityLifecycleDispositionLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.SupersedesDecisionID != deferred.ID || rejected.BeforeBand != reviewartifact.BandWatching {
		t.Fatalf("superseding decision = %+v", rejected)
	}
}

func TestRecordCapabilityLifecycleProposalPreservesHashAndRejectsSemanticCollision(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)
	before, err := os.ReadFile(snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	replay := snapshot.Proposal
	replay.CreatedAt = lifecycleReviewNow.Add(24 * time.Hour).Format(time.RFC3339)
	if _, err := RecordCapabilityLifecycleProposal(homePath, replay); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("semantic replay must preserve proposal bytes and rendered hash")
	}
	collision := snapshot.Proposal
	collision.Statement = "different semantics under the same proposal id"
	if _, err := RecordCapabilityLifecycleProposal(homePath, collision); err == nil || !strings.Contains(err.Error(), "semantic collision") {
		t.Fatalf("semantic collision should be rejected, got %v", err)
	}
}
