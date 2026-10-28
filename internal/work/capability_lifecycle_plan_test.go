package work

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/capgraph"
)

func TestCapabilityLifecycleOpenWorkUsesCreatePlanAndCarriesImmutableSourceContext(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	proposal, err := capgraph.BuildCapabilityLifecycleProposal(capgraph.CapabilityLifecycleProposalInput{
		ProposalKind:  "revise",
		Subject:       "session-mined skill update",
		Statement:     "teach an existing skill a verified failure pattern",
		Detail:        "Bounded implementation remains subject to the normal work lifecycle.",
		EvidenceRefs:  []string{"session:fixture", "check:fixture"},
		CapabilityRef: "skill:fixture",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.RecordCapabilityLifecycleProposal(homePath, proposal); err != nil {
		t.Fatal(err)
	}
	snapshot, err := capgraph.LoadCapabilityLifecycleProposalSnapshot(homePath, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := store.BuildCapabilityLifecycleCreatePlan(snapshot, now.Add(time.Minute), CapabilityLifecycleCreatePlanOptions{
		Actor:  "operator-test",
		Record: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Work.State != "proposed" || plan.Work.Owner != "capability-graph" {
		t.Fatalf("work = %+v", plan.Work)
	}
	if plan.ActionContext == nil || plan.ActionContext.SourceID != proposal.ID || plan.ActionContext.Kind != "capability_lifecycle_proposal" {
		t.Fatalf("action context = %+v", plan.ActionContext)
	}
	if len(plan.ActionContext.Command) != 0 {
		t.Fatalf("decision-brief or proposal argv must never become work execution authority: %+v", plan.ActionContext.Command)
	}
	if !containsCapabilityLifecycleRef(plan.Work.EvidenceRefs, "capability_lifecycle_proposal:"+proposal.ID) || !containsCapabilityLifecycleRef(plan.Work.EvidenceRefs, "sha256:"+snapshot.Hash) {
		t.Fatalf("work evidence refs = %+v", plan.Work.EvidenceRefs)
	}
	if !strings.Contains(strings.Join(plan.Work.NonGoals, " "), "does not mutate CapabilityGraph") {
		t.Fatalf("work non-goals = %+v", plan.Work.NonGoals)
	}

	admitted, err := store.AdmitCreatePlan(plan, plan.PlanPath, now.Add(2*time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Status != "admitted" || admitted.WorkState != "proposed" || admitted.WorkID != plan.WorkID {
		t.Fatalf("admission = %+v", admitted)
	}
	replayed, err := store.AdmitCreatePlan(plan, plan.PlanPath, now.Add(3*time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != "already_admitted" || replayed.WorkID != plan.WorkID {
		t.Fatalf("replayed admission = %+v", replayed)
	}
}

func TestCapabilityLifecycleCreatePlanPreparationReusesRecordedPlan(t *testing.T) {
	t.Parallel()
	homePath, store, snapshot, now := capabilityLifecyclePlanFixture(t)
	first, err := store.BuildCapabilityLifecycleCreatePlan(snapshot, now, CapabilityLifecycleCreatePlanOptions{
		Actor:  "operator-test",
		Record: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	replayed, err := store.PrepareCapabilityLifecycleCreatePlan(snapshot, now.Add(time.Hour), CapabilityLifecycleCreatePlanOptions{
		Actor: "operator-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != first.ID || replayed.PlanPath != first.PlanPath || replayed.ActionIntent.ID != first.ActionIntent.ID || replayed.Decision.ID != first.Decision.ID {
		t.Fatalf("recorded create plan was not reused: first=%+v replayed=%+v", first, replayed)
	}
	entries, err := os.ReadDir(filepath.Join(homePath, "create-plans"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("create plans = %d, want one exact reusable plan", len(entries))
	}
}

func TestCapabilityLifecycleCreatePlanAdmissionRepairsTornLifecycleEvent(t *testing.T) {
	t.Parallel()
	_, store, snapshot, now := capabilityLifecyclePlanFixture(t)
	plan, err := store.BuildCapabilityLifecycleCreatePlan(snapshot, now, CapabilityLifecycleCreatePlanOptions{
		Actor:  "operator-test",
		Record: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	actionContextRef, err := store.recordActionContextSnapshot(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writeJSON(store.envPath(plan.ActionIntent.ID), &plan.ActionIntent); err != nil {
		t.Fatal(err)
	}
	if err := store.writeJSON(store.envPath(plan.Decision.ID), &plan.Decision); err != nil {
		t.Fatal(err)
	}
	tornWork := plan.Work
	tornWork.ActionIntentIDs = append(tornWork.ActionIntentIDs, plan.ActionIntent.ID)
	tornWork.PolicyDecisionID = plan.Decision.ID
	if err := store.writeJSON(store.workPath(tornWork.ID), &tornWork); err != nil {
		t.Fatal(err)
	}
	if events, err := store.eventsFor(plan.WorkID); err != nil || len(events) != 0 {
		t.Fatalf("torn fixture events = %+v, err=%v", events, err)
	}

	recovered, err := store.AdmitCapabilityLifecycleCreatePlan(plan, plan.PlanPath, now.Add(time.Minute), AdmitCreatePlanOptions{
		Actor: "operator-test",
		Note:  "capability_lifecycle_disposition_open_work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "recovered_admission" || recovered.WorkID != plan.WorkID {
		t.Fatalf("recovered admission = %+v", recovered)
	}
	_, events, err := store.Show(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !containsCapabilityLifecycleRef(events[0].Attached, "create_plan:"+plan.ID) || !containsCapabilityLifecycleRef(events[0].Attached, actionContextRef) {
		t.Fatalf("recovered lifecycle event = %+v", events)
	}
}

func TestCapabilityLifecycleCreatePlanAdmissionRefusesMalformedEventTail(t *testing.T) {
	t.Parallel()
	_, store, snapshot, now := capabilityLifecyclePlanFixture(t)
	plan, err := store.BuildCapabilityLifecycleCreatePlan(snapshot, now, CapabilityLifecycleCreatePlanOptions{
		Actor:  "operator-test",
		Record: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTornCapabilityLifecycleAdmission(store, plan); err != nil {
		t.Fatal(err)
	}
	partial := []byte(`{"ts":"2026-07-10T09:00:00Z","work_id":"` + plan.WorkID + `"`)
	if err := os.WriteFile(store.eventsLog(), partial, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AdmitCapabilityLifecycleCreatePlan(plan, plan.PlanPath, now.Add(time.Minute), AdmitCreatePlanOptions{
		Actor: "operator-test",
		Note:  "capability_lifecycle_disposition_open_work",
	}); err == nil || !strings.Contains(err.Error(), "incomplete trailing record") {
		t.Fatalf("malformed lifecycle tail must fail closed, got %v", err)
	}
	after, err := os.ReadFile(store.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, partial) {
		t.Fatalf("failed admission appended onto malformed event tail: %q", after)
	}
}

func TestCapabilityLifecycleCreatePlanAdmissionSerializesGenericAndRecoveryPaths(t *testing.T) {
	t.Parallel()
	_, store, snapshot, now := capabilityLifecyclePlanFixture(t)
	plan, err := store.BuildCapabilityLifecycleCreatePlan(snapshot, now, CapabilityLifecycleCreatePlanOptions{
		Actor:  "operator-test",
		Record: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(useLifecyclePath bool) {
			defer wg.Done()
			<-start
			if useLifecyclePath {
				_, err := store.AdmitCapabilityLifecycleCreatePlan(plan, plan.PlanPath, now.Add(time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
				errs <- err
				return
			}
			_, err := store.AdmitCreatePlan(plan, plan.PlanPath, now.Add(time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
			errs <- err
		}(i%2 == 0)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent admission failed: %v", err)
		}
	}
	events, err := store.eventsFor(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("concurrent generic/lifecycle admission wrote %d initial events: %+v", len(events), events)
	}
}

func TestCapabilityLifecycleCreatePlanPreparationRefusesReplacementForExistingWork(t *testing.T) {
	t.Parallel()
	_, store, snapshot, now := capabilityLifecyclePlanFixture(t)
	plan, err := store.BuildCapabilityLifecycleCreatePlan(snapshot, now, CapabilityLifecycleCreatePlanOptions{
		Actor:  "operator-test",
		Record: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	existing := plan.Work
	existing.ActionIntentIDs = append(existing.ActionIntentIDs, plan.ActionIntent.ID)
	existing.PolicyDecisionID = plan.Decision.ID
	if err := store.writeJSON(store.workPath(existing.ID), &existing); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(plan.PlanPath); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PrepareCapabilityLifecycleCreatePlan(snapshot, now.Add(time.Hour), CapabilityLifecycleCreatePlanOptions{Actor: "operator-test"}); err == nil || !strings.Contains(err.Error(), "without its recorded") {
		t.Fatalf("existing work without original plan must fail closed, got %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.home, "create-plans"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("replacement create plans were written: %+v", entries)
	}
}

func TestCapabilityLifecycleCreatePlanPreparationRejectsAlteredRecordedSemantics(t *testing.T) {
	t.Parallel()
	_, store, snapshot, now := capabilityLifecyclePlanFixture(t)
	altered, err := store.BuildCapabilityLifecycleCreatePlan(snapshot, now, CapabilityLifecycleCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	altered.ActionContext.Command = []string{"compound", "work", "advance", altered.WorkID, "--to", "running"}
	altered.ID, err = createPlanContentID(altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCreatePlan(altered); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PrepareCapabilityLifecycleCreatePlan(snapshot, now.Add(time.Hour), CapabilityLifecycleCreatePlanOptions{Actor: "operator-test"}); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("altered recorded action context must not become resume authority, got %v", err)
	}
}

func capabilityLifecyclePlanFixture(t *testing.T) (string, *Store, capgraph.CapabilityLifecycleProposalSnapshot, time.Time) {
	t.Helper()
	homePath := t.TempDir()
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	proposal, err := capgraph.BuildCapabilityLifecycleProposal(capgraph.CapabilityLifecycleProposalInput{
		ProposalKind:  "revise",
		Subject:       "session-mined skill update",
		Statement:     "teach an existing skill a verified failure pattern",
		Detail:        "Bounded implementation remains subject to the normal work lifecycle.",
		EvidenceRefs:  []string{"session:fixture", "check:fixture"},
		CapabilityRef: "skill:fixture",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.RecordCapabilityLifecycleProposal(homePath, proposal); err != nil {
		t.Fatal(err)
	}
	snapshot, err := capgraph.LoadCapabilityLifecycleProposalSnapshot(homePath, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	return homePath, store, snapshot, now
}

func writeTornCapabilityLifecycleAdmission(store *Store, plan CreatePlan) error {
	if _, err := store.recordActionContextSnapshot(plan); err != nil {
		return err
	}
	if err := store.writeJSON(store.envPath(plan.ActionIntent.ID), &plan.ActionIntent); err != nil {
		return err
	}
	if err := store.writeJSON(store.envPath(plan.Decision.ID), &plan.Decision); err != nil {
		return err
	}
	tornWork := plan.Work
	tornWork.ActionIntentIDs = append(tornWork.ActionIntentIDs, plan.ActionIntent.ID)
	tornWork.PolicyDecisionID = plan.Decision.ID
	return store.writeJSON(store.workPath(tornWork.ID), &tornWork)
}

func containsCapabilityLifecycleRef(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
