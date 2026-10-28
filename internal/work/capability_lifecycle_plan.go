package work

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
)

type CapabilityLifecycleCreatePlanOptions struct {
	Actor  string
	Record bool
}

// BuildCapabilityLifecycleCreatePlan translates an immutable owner proposal
// snapshot into proposed work. It does not execute the proposal, mutate the
// capability graph, or trust decision-brief argv.
func (s *Store) BuildCapabilityLifecycleCreatePlan(
	snapshot capgraph.CapabilityLifecycleProposalSnapshot,
	now time.Time,
	opts CapabilityLifecycleCreatePlanOptions,
) (CreatePlan, error) {
	proposal := snapshot.Proposal
	if strings.TrimSpace(proposal.ID) == "" || strings.TrimSpace(snapshot.Hash) == "" {
		return CreatePlan{}, fmt.Errorf("capability lifecycle proposal snapshot id/hash required")
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "operator-console"
	}
	workID := capabilityLifecycleWorkID(snapshot)
	sourceRef := "compound://reviews/" + capgraph.CapabilityLifecycleProposalReviewArea + "/" + proposal.ID
	evidenceRefs := compactStrings(
		append(
			[]string{"capability_lifecycle_proposal:" + proposal.ID, "sha256:" + snapshot.Hash, sourceRef},
			proposal.EvidenceRefs...,
		)...,
	)
	successPredicate := fmt.Sprintf(
		"Resolve capability lifecycle proposal %s through a reviewed owner change; proof: a terminal Receipt/Transition cites capability_lifecycle_proposal:%s and any CapabilityGraph, skill, adapter, or routing delta is separately governed",
		proposal.ID,
		proposal.ID,
	)
	desiredOutcome := fmt.Sprintf("Capability owner evaluates and, only with the required evidence, implements or retires: %s", proposal.Statement)
	ctx := WorkActionContext{
		Kind:          "capability_lifecycle_proposal",
		SourceRef:     sourceRef,
		SourceID:      proposal.ID,
		Title:         proposal.Subject,
		ActionSummary: proposal.Statement,
		RequiredInputs: compactStrings(
			append([]string{proposal.Detail}, proposal.Draft.RequiredEvidence...)...,
		),
		ExpectedValidation: compactStrings(proposal.Draft.RequiredEvidence...),
		StopConditions: []string{
			"the proposal revision no longer matches sha256:" + snapshot.Hash,
			"the claimed owner or replacement relationship is ambiguous",
			"the change requires public, financial, destructive, credential, or security-sensitive action without a separate policy decision",
		},
		Boundary:         proposal.Draft.Guardrail,
		Proof:            successPredicate,
		RequiresOperator: false,
		EvidenceRefs:     evidenceRefs,
	}
	return s.CreatePlan(CreateInput{
		WorkID:           workID,
		Owner:            "capability-graph",
		Actor:            actor,
		CanonicalSurface: sourceRef,
		State:            "proposed",
		SuccessPredicate: successPredicate,
		DesiredOutcome:   desiredOutcome,
		NonGoals: []string{
			"creating this work does not mutate CapabilityGraph, skill files, adapters, or routing defaults",
			"do not execute decision_brief command_argv or proposal text as shell authority",
			"do not advance, claim, dispatch, execute, or close follow-on work from this disposition",
		},
		EvidenceRefs: evidenceRefs,
		Risk:         "tier1",
		Privacy:      "private",
	}, now, CreatePlanOptions{
		Actor:         actor,
		Capability:    "work.capability_lifecycle_proposal",
		Action:        "plan_work_from_capability_lifecycle_proposal",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		TargetKind:    "capability_lifecycle_proposal",
		TargetID:      proposal.ID,
		TargetPath:    snapshot.Path,
		ActionContext: &ctx,
		Record:        opts.Record,
	})
}

// PrepareCapabilityLifecycleCreatePlan reuses the first recorded plan for an
// exact proposal/actor pair. This keeps crash retries bound to the ActionIntent,
// PolicyDecision, and action-context snapshot that may already be attached to
// the deterministic WorkEnvelope.
func (s *Store) PrepareCapabilityLifecycleCreatePlan(
	snapshot capgraph.CapabilityLifecycleProposalSnapshot,
	now time.Time,
	opts CapabilityLifecycleCreatePlanOptions,
) (CreatePlan, error) {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "operator-console"
	}
	existing, ok, err := s.findCapabilityLifecycleCreatePlan(snapshot, actor)
	if err != nil {
		return CreatePlan{}, err
	}
	if ok {
		return existing, nil
	}
	opts.Actor = actor
	return s.BuildCapabilityLifecycleCreatePlan(snapshot, now, opts)
}

// AdmitCapabilityLifecycleCreatePlan closes the one recoverable gap in the
// generic create-plan admission sequence: the WorkEnvelope is durable but its
// initial lifecycle event was not appended. Recovery is deliberately narrow
// and requires every already-written artifact to match the original plan.
func (s *Store) AdmitCapabilityLifecycleCreatePlan(
	plan CreatePlan,
	planPath string,
	now time.Time,
	opts AdmitCreatePlanOptions,
) (AdmitCreatePlanReport, error) {
	if err := validateCreatePlanForAdmission(plan); err != nil {
		return AdmitCreatePlanReport{}, err
	}
	var report AdmitCreatePlanReport
	err := s.withCreatePlanAdmissionLock(plan.WorkID, func() error {
		existing, err := s.Get(plan.WorkID)
		if errors.Is(err, os.ErrNotExist) {
			var admitErr error
			report, admitErr = s.admitCreatePlanLocked(plan, planPath, now, opts)
			return admitErr
		}
		if err != nil {
			return err
		}
		events, err := s.admissionEventsFor(plan.WorkID)
		if err != nil {
			return err
		}
		if len(events) != 0 {
			var admitErr error
			report, admitErr = s.admitCreatePlanLocked(plan, planPath, now, opts)
			return admitErr
		}
		var recoverErr error
		report, recoverErr = s.recoverCapabilityLifecycleAdmission(existing, plan, planPath, now, opts)
		return recoverErr
	})
	return report, err
}

func (s *Store) findCapabilityLifecycleCreatePlan(snapshot capgraph.CapabilityLifecycleProposalSnapshot, actor string) (CreatePlan, bool, error) {
	dir := filepath.Join(s.home, "create-plans")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return CreatePlan{}, false, nil
	}
	if err != nil {
		return CreatePlan{}, false, err
	}
	wantWorkID := capabilityLifecycleWorkID(snapshot)
	candidates := make([]CreatePlan, 0, 1)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		plan, loadErr := LoadCreatePlan(filepath.Join(dir, entry.Name()))
		if loadErr != nil || plan.WorkID != wantWorkID || strings.TrimSpace(plan.Actor) != actor {
			continue
		}
		if err := validateCreatePlanForAdmission(plan); err != nil {
			return CreatePlan{}, false, fmt.Errorf("recorded capability lifecycle create-plan %s invalid: %w", entry.Name(), err)
		}
		matches, matchErr := s.capabilityLifecycleCreatePlanMatches(plan, snapshot)
		if matchErr != nil {
			return CreatePlan{}, false, matchErr
		}
		if !matches {
			return CreatePlan{}, false, fmt.Errorf("recorded create-plan %s collides with capability lifecycle work %s", plan.ID, wantWorkID)
		}
		candidates = append(candidates, plan)
	}
	if len(candidates) == 0 {
		if _, getErr := s.Get(wantWorkID); getErr == nil {
			return CreatePlan{}, false, fmt.Errorf("work %s exists without its recorded capability lifecycle create-plan", wantWorkID)
		} else if !errors.Is(getErr, os.ErrNotExist) {
			return CreatePlan{}, false, getErr
		}
		return CreatePlan{}, false, nil
	}
	if existing, getErr := s.Get(wantWorkID); getErr == nil {
		for _, candidate := range candidates {
			if existing.PolicyDecisionID == candidate.Decision.ID && capabilityLifecycleContains(existing.ActionIntentIDs, candidate.ActionIntent.ID) {
				return candidate, true, nil
			}
		}
		return CreatePlan{}, false, fmt.Errorf("work %s exists without its recorded capability lifecycle create-plan", wantWorkID)
	} else if !errors.Is(getErr, os.ErrNotExist) {
		return CreatePlan{}, false, getErr
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].GeneratedAt != candidates[j].GeneratedAt {
			return candidates[i].GeneratedAt < candidates[j].GeneratedAt
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], true, nil
}

func (s *Store) capabilityLifecycleCreatePlanMatches(plan CreatePlan, snapshot capgraph.CapabilityLifecycleProposalSnapshot) (bool, error) {
	generatedAt, err := time.Parse(time.RFC3339, plan.GeneratedAt)
	if err != nil {
		return false, fmt.Errorf("capability lifecycle create-plan %s generated_at invalid: %w", plan.ID, err)
	}
	expected, err := s.BuildCapabilityLifecycleCreatePlan(snapshot, generatedAt, CapabilityLifecycleCreatePlanOptions{Actor: plan.Actor})
	if err != nil {
		return false, fmt.Errorf("rebuild capability lifecycle create-plan semantics: %w", err)
	}
	expectedAction := expected.ActionIntent
	// ActionIntent ids include the original UnixNano timestamp while the plan
	// persists only an RFC3339 second. Compare every semantic/header field and
	// preserve the already-validated content-bound action id from the plan.
	expectedAction.ID = plan.ActionIntent.ID
	return plan.Mode == expected.Mode &&
		plan.Actor == expected.Actor &&
		plan.WorkID == expected.WorkID &&
		plan.WorkState == expected.WorkState &&
		plan.WorkOwner == expected.WorkOwner &&
		reflect.DeepEqual(plan.CreateCommand, expected.CreateCommand) &&
		reflect.DeepEqual(plan.Work, expected.Work) &&
		reflect.DeepEqual(plan.ActionIntent, expectedAction) &&
		sameWorkActionContext(plan.ActionContext, expected.ActionContext), nil
}

func (s *Store) recoverCapabilityLifecycleAdmission(
	existing envelope.WorkEnvelope,
	plan CreatePlan,
	planPath string,
	now time.Time,
	opts AdmitCreatePlanOptions,
) (AdmitCreatePlanReport, error) {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = firstNonEmpty(plan.Actor, plan.Work.Owner, "compound")
	}
	note := strings.TrimSpace(opts.Note)
	if note == "" {
		note = "admitted_from_create_plan"
	}
	report := admitCreatePlanReport(plan, planPath, now, actor, note)
	if err := validateCreatePlanForAdmission(plan); err != nil {
		return report, err
	}
	expectedWork := plan.Work
	expectedWork.ActionIntentIDs = append(expectedWork.ActionIntentIDs, plan.ActionIntent.ID)
	expectedWork.PolicyDecisionID = plan.Decision.ID
	if !reflect.DeepEqual(existing, expectedWork) {
		return report, fmt.Errorf("work %s exists without a lifecycle event but does not match create-plan %s", plan.WorkID, plan.ID)
	}
	var action envelope.ActionIntent
	if err := s.readEventJSON(plan.ActionIntent.ID, &action); err != nil || !reflect.DeepEqual(action, plan.ActionIntent) {
		return report, fmt.Errorf("work %s torn admission action intent does not match create-plan %s", plan.WorkID, plan.ID)
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(plan.Decision.ID, &decision); err != nil || !reflect.DeepEqual(decision, plan.Decision) {
		return report, fmt.Errorf("work %s torn admission policy decision does not match create-plan %s", plan.WorkID, plan.ID)
	}
	actionContextRef, err := s.recordActionContextSnapshot(plan)
	if err != nil {
		return report, err
	}
	attached := []string{plan.ActionIntent.ID, plan.Decision.ID, "create_plan:" + plan.ID, actionContextRef}
	if err := s.appendEvent(Event{TS: ts(now), WorkID: existing.ID, From: "", To: existing.State, Note: note, Attached: attached}); err != nil {
		return report, err
	}
	report.Status = "recovered_admission"
	report.Work = existing
	report.WorkState = existing.State
	report.WorkOwner = existing.Owner
	report.ActionContext = cloneWorkActionContext(plan.ActionContext)
	return report, nil
}

func capabilityLifecycleWorkID(snapshot capgraph.CapabilityLifecycleProposalSnapshot) string {
	return idFor("work_", "capability_lifecycle_proposal", snapshot.Proposal.ID, snapshot.Hash)
}

func capabilityLifecycleContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
