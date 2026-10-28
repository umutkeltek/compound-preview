package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/policy"
)

const LegacyCreatePlanSchemaVersion = "compound.work_create_plan.v0.1"
const CreatePlanSchemaVersion = "compound.work_create_plan.v0.2"
const AdmitCreatePlanReportSchemaVersion = "compound.work_create_plan_admission.v0.1"

type CreatePlanOptions struct {
	Actor         string
	Capability    string
	Action        string
	MutationClass string
	Reversibility string
	BlastRadius   string
	TargetKind    string
	TargetID      string
	TargetPath    string
	ActionContext *WorkActionContext
	Record        bool
}

type CreatePolicySummary struct {
	ActionIntentID   string   `json:"action_intent_id"`
	PolicyDecisionID string   `json:"policy_decision_id"`
	PolicySource     string   `json:"policy_source,omitempty"`
	Outcome          string   `json:"outcome"`
	Allowed          bool     `json:"allowed"`
	StandingGrantID  string   `json:"standing_grant_id,omitempty"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
	Reasons          []string `json:"reasons,omitempty"`
	Obligations      []string `json:"obligations,omitempty"`
	EvidenceRefs     []string `json:"evidence_refs,omitempty"`
}

type WorkActionContext struct {
	Kind               string   `json:"kind,omitempty"`
	SourceRef          string   `json:"source_ref,omitempty"`
	SourceID           string   `json:"source_id,omitempty"`
	Title              string   `json:"title,omitempty"`
	ActionSummary      string   `json:"action_summary,omitempty"`
	Command            []string `json:"command,omitempty"`
	RequiredInputs     []string `json:"required_inputs,omitempty"`
	ExpectedValidation []string `json:"expected_validation,omitempty"`
	StopConditions     []string `json:"stop_conditions,omitempty"`
	Boundary           string   `json:"boundary,omitempty"`
	Proof              string   `json:"proof,omitempty"`
	RequiresOperator   bool     `json:"requires_operator,omitempty"`
	EvidenceRefs       []string `json:"evidence_refs,omitempty"`
}

type CreatePlan struct {
	SchemaVersion string                  `json:"schema_version"`
	ID            string                  `json:"id"`
	GeneratedAt   string                  `json:"generated_at"`
	Status        string                  `json:"status"`
	Mode          string                  `json:"mode"`
	Actor         string                  `json:"actor"`
	WorkID        string                  `json:"work_id"`
	WorkState     string                  `json:"work_state"`
	WorkOwner     string                  `json:"work_owner,omitempty"`
	PlanPath      string                  `json:"plan_path,omitempty"`
	CreateCommand []string                `json:"create_command,omitempty"`
	Policy        CreatePolicySummary     `json:"policy"`
	ActionIntent  envelope.ActionIntent   `json:"action_intent"`
	Decision      envelope.PolicyDecision `json:"policy_decision"`
	Work          envelope.WorkEnvelope   `json:"work"`
	ActionContext *WorkActionContext      `json:"action_context,omitempty"`
	// MeaningResolution records the work-owned discriminated promoted-meaning status
	// (none|ambiguous|resolved) the private resolver returned. Empty on plans
	// produced before this field existed; admissions treat the empty value as
	// "resolved" because the legacy path could only emit a resolved context.
	MeaningResolution string `json:"meaning_resolution,omitempty"`
	// AmbiguousRefs lists the recorded decision refs that cleared the
	// eligibility filter under the same ContextPack. Non-empty only when
	// MeaningResolution == "ambiguous"; admissions require it to be sorted
	// and de-duplicated.
	AmbiguousRefs []string `json:"ambiguous_refs,omitempty"`
	NoAction      string   `json:"no_action,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type AdmitCreatePlanOptions struct {
	Actor string
	Note  string
}

type AdmitCreatePlanReport struct {
	SchemaVersion    string                `json:"schema_version"`
	ID               string                `json:"id"`
	GeneratedAt      string                `json:"generated_at"`
	Status           string                `json:"status"`
	Actor            string                `json:"actor,omitempty"`
	PlanID           string                `json:"plan_id"`
	PlanPath         string                `json:"plan_path,omitempty"`
	WorkID           string                `json:"work_id"`
	WorkState        string                `json:"work_state"`
	WorkOwner        string                `json:"work_owner,omitempty"`
	ActionIntentID   string                `json:"action_intent_id,omitempty"`
	PolicyDecisionID string                `json:"policy_decision_id,omitempty"`
	PolicyOutcome    string                `json:"policy_outcome,omitempty"`
	PolicyAllowed    bool                  `json:"policy_allowed"`
	AdmissionNote    string                `json:"admission_note,omitempty"`
	NoAction         string                `json:"no_action,omitempty"`
	Work             envelope.WorkEnvelope `json:"work"`
	ActionContext    *WorkActionContext    `json:"action_context,omitempty"`
	Warnings         []string              `json:"warnings,omitempty"`
}

func (s *Store) CreatePlan(in CreateInput, now time.Time, opts CreatePlanOptions) (CreatePlan, error) {
	w := BuildWork(in, now)
	if err := s.normalizeGitDeliveryForPlan(&w); err != nil {
		return CreatePlan{}, err
	}
	if strings.TrimSpace(w.State) == "running" {
		return CreatePlan{}, fmt.Errorf("create at ready, then claim and advance to preserve lease proof")
	}
	if strings.TrimSpace(opts.Actor) == "" {
		opts.Actor = in.Actor
	}
	return s.createPlanForWork(w, now, opts)
}

func LoadCreatePlan(path string) (CreatePlan, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return CreatePlan{}, fmt.Errorf("create-plan path required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CreatePlan{}, err
	}
	var plan CreatePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return CreatePlan{}, fmt.Errorf("parse create-plan %s: %w", path, err)
	}
	if strings.TrimSpace(plan.PlanPath) == "" {
		plan.PlanPath = path
	}
	return plan, nil
}

func (s *Store) AdmitCreatePlanPath(path string, now time.Time, opts AdmitCreatePlanOptions) (AdmitCreatePlanReport, error) {
	plan, err := LoadCreatePlan(path)
	if err != nil {
		return AdmitCreatePlanReport{}, err
	}
	if filepath.Base(path) != plan.ID+".json" {
		return AdmitCreatePlanReport{}, fmt.Errorf("create-plan path identity mismatch: %s does not contain %s", filepath.Base(path), plan.ID)
	}
	return s.AdmitCreatePlan(plan, path, now, opts)
}

func (s *Store) AdmitCreatePlan(plan CreatePlan, planPath string, now time.Time, opts AdmitCreatePlanOptions) (AdmitCreatePlanReport, error) {
	if err := validateCreatePlanForAdmission(plan); err != nil {
		return AdmitCreatePlanReport{}, err
	}
	if err := s.validateBoundGitDelivery(plan.Work); err != nil {
		return AdmitCreatePlanReport{}, fmt.Errorf("create-plan %s git delivery admission: %w", plan.ID, err)
	}
	var report AdmitCreatePlanReport
	err := s.withCreatePlanAdmissionLock(plan.WorkID, func() error {
		var admitErr error
		report, admitErr = s.admitCreatePlanLocked(plan, planPath, now, opts)
		return admitErr
	})
	return report, err
}

func (s *Store) admitCreatePlanLocked(plan CreatePlan, planPath string, now time.Time, opts AdmitCreatePlanOptions) (AdmitCreatePlanReport, error) {
	planPath = strings.TrimSpace(firstNonEmpty(planPath, plan.PlanPath))
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = firstNonEmpty(plan.Actor, plan.Work.Owner, "compound")
	}
	note := strings.TrimSpace(opts.Note)
	if note == "" {
		note = "admitted_from_create_plan"
	}
	report := admitCreatePlanReport(plan, planPath, now, actor, note)
	if existing, err := s.Get(plan.WorkID); err == nil {
		if !sameCreatePlanWork(existing, plan.Work) {
			return report, fmt.Errorf("work %s already exists but does not match create-plan %s", plan.WorkID, plan.ID)
		}
		existingContext, warnings := s.actionContextForWork(existing)
		if len(warnings) > 0 {
			return report, fmt.Errorf("work %s admitted action context unreadable: %s", plan.WorkID, strings.Join(warnings, "; "))
		}
		if !sameWorkActionContext(existingContext, plan.ActionContext) {
			return report, fmt.Errorf("work %s already exists but action context does not match admitted snapshot", plan.WorkID)
		}
		report.Status = "already_admitted"
		report.NoAction = "work already exists; create-plan admission left lifecycle unchanged"
		report.Work = existing
		report.WorkState = existing.State
		report.WorkOwner = existing.Owner
		report.ActionContext = cloneWorkActionContext(existingContext)
		return report, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return report, err
	}
	if _, err := s.admissionEventsFor(plan.WorkID); err != nil {
		return report, err
	}
	w, err := s.admitWork(plan, now, note, []string{"create_plan:" + plan.ID})
	if err != nil {
		return report, err
	}
	report.Status = "admitted"
	report.Work = w
	report.WorkState = w.State
	report.WorkOwner = w.Owner
	return report, nil
}

// admissionEventsFor uses the same strict, locked reader as every lifecycle
// projection so admission cannot append through corruption that readers hide.
func (s *Store) admissionEventsFor(workID string) ([]Event, error) {
	return s.eventsFor(workID)
}

// decodeAdmissionEvents is the single strict parser shared by locked lifecycle
// readers and the controlled event-log publisher.
func decodeAdmissionEvents(b []byte, workID string) ([]Event, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if b[len(b)-1] != '\n' {
		return nil, fmt.Errorf("work event log has an incomplete trailing record")
	}
	var out []Event
	for lineNumber, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("work event log malformed at line %d: %w", lineNumber+1, err)
		}
		if strings.TrimSpace(event.WorkID) == "" {
			return nil, fmt.Errorf("work event log malformed at line %d: work_id required", lineNumber+1)
		}
		if workID == "" || event.WorkID == workID {
			out = append(out, event)
		}
	}
	return out, nil
}

func (s *Store) withCreatePlanAdmissionLock(workID string, fn func() error) error {
	if strings.TrimSpace(workID) == "" || filepath.Base(workID) != workID {
		return fmt.Errorf("invalid create-plan work id %q", workID)
	}
	// Keep the original lifecycle lock path so old and new binaries serialize
	// against the same inode during an installed-runtime transition.
	path := filepath.Join(s.home, "lock", "capability-lifecycle-work-admission", workID+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func validateCreatePlanForAdmission(plan CreatePlan) error {
	if plan.SchemaVersion != CreatePlanSchemaVersion {
		return fmt.Errorf("create-plan schema_version must be %s, got %q", CreatePlanSchemaVersion, plan.SchemaVersion)
	}
	if strings.TrimSpace(plan.ID) == "" || strings.TrimSpace(plan.WorkID) == "" {
		return fmt.Errorf("create-plan id and work_id required")
	}
	expectedID, err := createPlanContentID(plan)
	if err != nil {
		return fmt.Errorf("create-plan %s content digest: %w", plan.ID, err)
	}
	if plan.ID != expectedID {
		return fmt.Errorf("create-plan %s content digest mismatch (expected %s)", plan.ID, expectedID)
	}
	if plan.Status != "create_policy_allowed" {
		return fmt.Errorf("create-plan %s status must be create_policy_allowed, got %q", plan.ID, plan.Status)
	}
	switch plan.MeaningResolution {
	case "": // legacy v0.2 plans predate the additive discriminator
	case string(meaningNone):
		if plan.ActionContext != nil || len(plan.AmbiguousRefs) != 0 {
			return fmt.Errorf("create-plan %s none meaning resolution is inconsistent", plan.ID)
		}
	case string(meaningResolved):
		if plan.ActionContext == nil || len(plan.AmbiguousRefs) != 0 {
			return fmt.Errorf("create-plan %s resolved meaning resolution is inconsistent", plan.ID)
		}
	case string(meaningAmbiguous):
		if plan.ActionContext != nil || len(plan.AmbiguousRefs) == 0 || !sameStringSlice(plan.AmbiguousRefs, uniqueMeaningRefs(plan.AmbiguousRefs...)) {
			return fmt.Errorf("create-plan %s ambiguous meaning resolution is inconsistent", plan.ID)
		}
		return fmt.Errorf("create-plan %s has ambiguous promoted meaning; review the named refs before admission", plan.ID)
	default:
		return fmt.Errorf("create-plan %s has unknown meaning resolution %q", plan.ID, plan.MeaningResolution)
	}
	if !plan.Policy.Allowed {
		return fmt.Errorf("create-plan %s policy is not allowed: %s", plan.ID, plan.Policy.Outcome)
	}
	if strings.TrimSpace(plan.Work.ID) != plan.WorkID {
		return fmt.Errorf("create-plan %s work.id %q does not match work_id %q", plan.ID, plan.Work.ID, plan.WorkID)
	}
	if strings.TrimSpace(plan.WorkState) != "" && plan.Work.State != plan.WorkState {
		return fmt.Errorf("create-plan %s work.state %q does not match work_state %q", plan.ID, plan.Work.State, plan.WorkState)
	}
	if terminalStates[plan.Work.State] {
		return fmt.Errorf("create-plan %s: %w", plan.ID, terminalStateRequiresClose("create-plan admission", plan.Work.State))
	}
	if err := plan.Work.Validate(); err != nil {
		return fmt.Errorf("create-plan %s work invalid: %w", plan.ID, err)
	}
	if err := validateNewGitDeliveryDeclaration(plan.Work); err != nil {
		return fmt.Errorf("create-plan %s work invalid: %w", plan.ID, err)
	}
	if err := validateDirectorProposalInitialState(plan.Work); err != nil {
		return fmt.Errorf("create-plan %s: %w", plan.ID, err)
	}
	if err := plan.ActionIntent.Validate(); err != nil {
		return fmt.Errorf("create-plan %s action_intent invalid: %w", plan.ID, err)
	}
	if err := plan.Decision.Validate(); err != nil {
		return fmt.Errorf("create-plan %s policy_decision invalid: %w", plan.ID, err)
	}
	if plan.ActionIntent.IDs.WorkID != plan.WorkID || plan.Decision.IDs.WorkID != plan.WorkID {
		return fmt.Errorf("create-plan %s admission envelopes are not bound to work %s", plan.ID, plan.WorkID)
	}
	if plan.Policy.ActionIntentID != plan.ActionIntent.ID || plan.Policy.PolicyDecisionID != plan.Decision.ID {
		return fmt.Errorf("create-plan %s policy summary does not match admission envelopes", plan.ID)
	}
	if plan.Decision.ActionIntentID != plan.ActionIntent.ID {
		return fmt.Errorf("create-plan %s policy decision does not reference action intent %s", plan.ID, plan.ActionIntent.ID)
	}
	return nil
}

func admitCreatePlanReport(plan CreatePlan, planPath string, now time.Time, actor, note string) AdmitCreatePlanReport {
	return AdmitCreatePlanReport{
		SchemaVersion:    AdmitCreatePlanReportSchemaVersion,
		ID:               idFor("wcar_", plan.ID, plan.WorkID, ts(now)),
		GeneratedAt:      now.UTC().Format(time.RFC3339),
		Status:           "pending",
		Actor:            actor,
		PlanID:           plan.ID,
		PlanPath:         planPath,
		WorkID:           plan.WorkID,
		WorkState:        plan.WorkState,
		WorkOwner:        plan.WorkOwner,
		ActionIntentID:   plan.ActionIntent.ID,
		PolicyDecisionID: plan.Decision.ID,
		PolicyOutcome:    plan.Policy.Outcome,
		PolicyAllowed:    plan.Policy.Allowed,
		AdmissionNote:    note,
		Work:             plan.Work,
		ActionContext:    cloneWorkActionContext(plan.ActionContext),
	}
}

func sameCreatePlanWork(existing, planned envelope.WorkEnvelope) bool {
	return existing.ID == planned.ID &&
		existing.Owner == planned.Owner &&
		existing.State == planned.State &&
		existing.SuccessPredicate == planned.SuccessPredicate &&
		existing.DesiredOutcome == planned.DesiredOutcome &&
		existing.IDs.ContextPackID == planned.IDs.ContextPackID &&
		existing.Risk == planned.Risk &&
		existing.Privacy == planned.Privacy &&
		existing.CanonicalSurface == planned.CanonicalSurface &&
		existing.ExpectedBaseSHA == planned.ExpectedBaseSHA &&
		sameGitDeliveryContract(existing.GitDelivery, planned.GitDelivery) &&
		existing.RouteDecisionID == planned.RouteDecisionID &&
		sameStringSlice(existing.NonGoals, planned.NonGoals) &&
		sameStringSlice(existing.EvidenceRefs, planned.EvidenceRefs) &&
		sameWorkOrigin(existing.Origin, planned.Origin)
}

func sameWorkOrigin(a, b *envelope.WorkOrigin) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Store) createPlanForWork(w envelope.WorkEnvelope, now time.Time, opts CreatePlanOptions) (CreatePlan, error) {
	if terminalStates[w.State] {
		return CreatePlan{}, terminalStateRequiresClose("create", w.State)
	}
	if err := w.Validate(); err != nil {
		return CreatePlan{}, err
	}
	if err := validateNewGitDeliveryDeclaration(w); err != nil {
		return CreatePlan{}, err
	}
	if err := s.validateBoundGitDelivery(w); err != nil {
		return CreatePlan{}, err
	}
	if err := validateDirectorProposalInitialState(w); err != nil {
		return CreatePlan{}, err
	}
	if opts.ActionContext == nil {
		resolution, err := s.promotedMeaningActionContext(w.IDs.ContextPackID)
		if err != nil {
			return CreatePlan{}, fmt.Errorf("context_pack_id %q meaning authority: %w", w.IDs.ContextPackID, err)
		}
		opts.ActionContext = resolution.Context
		return s.finishCreatePlan(w, now, opts, resolution)
	}
	return s.finishCreatePlan(w, now, opts, meaningResolution{Status: meaningResolved, Context: opts.ActionContext})
}

func (s *Store) finishCreatePlan(w envelope.WorkEnvelope, now time.Time, opts CreatePlanOptions, resolution meaningResolution) (CreatePlan, error) {
	// The resolved context is work-owned derivation evidence. Preserve its exact
	// source chain on the proposed WorkEnvelope before policy/admission artifacts
	// are built so a later admission cannot separate the work from the meaning
	// revision that selected it.
	if resolution.Context != nil {
		w.EvidenceRefs = uniqueMeaningRefs(append(w.EvidenceRefs, resolution.Context.EvidenceRefs...)...)
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = firstNonEmpty(w.Owner, "compound")
	}
	ai := s.createActionIntent(w, actor, now, opts)
	if err := ai.Validate(); err != nil {
		return CreatePlan{}, err
	}
	if err := s.validateSourceActionAdmission(w, ai); err != nil {
		return CreatePlan{}, err
	}
	cfg, source, err := policy.LoadConfig(s.home)
	if err != nil {
		return CreatePlan{}, err
	}
	pd := policy.Evaluate(ai, cfg, now)
	if err := pd.Validate(); err != nil {
		return CreatePlan{}, err
	}
	plan := CreatePlan{
		SchemaVersion: CreatePlanSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "create_policy_blocked",
		Mode:          "proposal_only",
		Actor:         actor,
		WorkID:        w.ID,
		WorkState:     w.State,
		WorkOwner:     w.Owner,
		CreateCommand: createCommandForWork(w, actor),
		Policy:        summarizeCreatePolicy(pd, source),
		ActionIntent:  ai,
		Decision:      pd,
		Work:          w,
		ActionContext: normalizeWorkActionContext(opts.ActionContext),
		NoAction:      "create lifecycle-ready but policy does not allow it",
		Warnings: []string{
			"does not create work",
			"does not write lifecycle event",
			"does not record action intent",
			"does not enforce policy; it reports create gate decision",
		},
	}
	// Preserve the pre-discriminator wire shape for context-free create-plans.
	// Expressive intent always carries a recorded ContextPack, so all three
	// none|ambiguous|resolved outcomes remain explicit on that boundary.
	if resolution.Status != meaningNone || strings.TrimSpace(w.IDs.ContextPackID) != "" {
		plan.MeaningResolution = string(resolution.Status)
	}
	plan.AmbiguousRefs = append([]string{}, resolution.AmbiguousRefs...)
	if policy.Allowed(pd) {
		plan.Status = "create_policy_allowed"
		plan.NoAction = ""
	}
	plan.ID, err = createPlanContentID(plan)
	if err != nil {
		return CreatePlan{}, err
	}
	if opts.Record {
		path, err := s.RecordCreatePlan(plan)
		if err != nil {
			return CreatePlan{}, err
		}
		plan.PlanPath = path
	}
	return plan, nil
}

func validateDirectorProposalInitialState(w envelope.WorkEnvelope) error {
	if directorProposedWork(w) && strings.TrimSpace(w.State) != "proposed" {
		return fmt.Errorf("director-proposed work must start in proposed state; got %q", w.State)
	}
	return nil
}

func (s *Store) admitWork(plan CreatePlan, now time.Time, note string, extraAttached []string) (envelope.WorkEnvelope, error) {
	var out envelope.WorkEnvelope
	var admitErr error
	err := s.withLifecycleMutation(func() error {
		out, admitErr = s.admitWorkLocked(plan, now, note, extraAttached)
		return admitErr
	})
	return out, err
}

func (s *Store) admitWorkLocked(plan CreatePlan, now time.Time, note string, extraAttached []string) (envelope.WorkEnvelope, error) {
	w := plan.Work
	if terminalStates[w.State] {
		return w, terminalStateRequiresClose("create-plan admission", w.State)
	}
	if err := validateDirectorProposalInitialState(w); err != nil {
		return w, err
	}
	if err := s.validateBoundGitDelivery(w); err != nil {
		return w, err
	}
	if err := s.validateSourceActionAdmission(w, plan.ActionIntent); err != nil {
		return w, err
	}
	// The existence check must live inside the same critical section as the
	// write. A CreatePlan can be prepared before another process creates and
	// auto-admits the same work id; replaying that stale plan must never write
	// its older proposed state over the admitted envelope.
	if existing, err := s.Get(w.ID); err == nil {
		return existing, fmt.Errorf("work %s already exists; refusing stale create-plan %s overwrite (current state %s)", w.ID, plan.ID, existing.State)
	} else if !errors.Is(err, os.ErrNotExist) {
		return w, err
	}
	if !plan.Policy.Allowed {
		return w, fmt.Errorf("cannot create work %s: policy %s (decision %s)", w.ID, plan.Policy.Outcome, plan.Policy.PolicyDecisionID)
	}
	if err := s.validateContextPackRef(w.IDs.ContextPackID); err != nil {
		return w, err
	}
	if err := s.validateReworkOfRef(w); err != nil {
		return w, err
	}
	actionContextRef, err := s.recordActionContextSnapshot(plan)
	if err != nil {
		return w, err
	}
	if actionContextRef != "" {
		extraAttached = append(extraAttached, actionContextRef)
	}
	// Slice 3 (product-loop-momentum 2026-09-08): persist the
	// transition event BEFORE the candidate envelope, so a crash
	// between the two writes leaves either (a) neither — recoverable
	// by re-running admit, or (b) the event with no envelope —
	// the work proof-chain doctor flags its action intent and policy
	// decision as orphans (nothing rebuilds the envelope
	// automatically). The previous order
	// persisted the envelope first; a crash there left a fully
	// formed candidate with no transition record, which recovery
	// could not detect or repair because there was nothing to mark
	// the gap.
	state := w.State
	w.ActionIntentIDs = append(w.ActionIntentIDs, plan.ActionIntent.ID)
	w.PolicyDecisionID = plan.Decision.ID
	attached := []string{plan.ActionIntent.ID, plan.Decision.ID}
	attached = append(attached, extraAttached...)
	if strings.TrimSpace(note) == "" {
		note = "created"
	}
	event := Event{TS: ts(now), WorkID: w.ID, From: "", To: state, Note: note, Attached: attached}
	if err := s.writeEventJSON(plan.ActionIntent.ID, &plan.ActionIntent); err != nil {
		return w, err
	}
	if err := s.writeEventJSON(plan.Decision.ID, &plan.Decision); err != nil {
		return w, err
	}
	if err := s.appendEvent(event); err != nil {
		return w, err
	}
	if err := s.writeJSON(s.workPath(w.ID), &w); err != nil {
		return w, err
	}
	return w, nil
}

func createCommandForWork(w envelope.WorkEnvelope, actor string) []string {
	cmd := []string{"compound", "work", "create", "--work-id", w.ID, "--owner", w.Owner, "--predicate", w.SuccessPredicate, "--by", actor}
	if strings.TrimSpace(w.CanonicalSurface) != "" {
		cmd = append(cmd, "--canonical-surface", w.CanonicalSurface)
	}
	if strings.TrimSpace(w.DesiredOutcome) != "" {
		cmd = append(cmd, "--outcome", w.DesiredOutcome)
	}
	for _, nonGoal := range w.NonGoals {
		if strings.TrimSpace(nonGoal) != "" {
			cmd = append(cmd, "--non-goal", nonGoal)
		}
	}
	for _, evidenceRef := range w.EvidenceRefs {
		if strings.TrimSpace(evidenceRef) != "" {
			cmd = append(cmd, "--evidence-ref", evidenceRef)
		}
	}
	if strings.TrimSpace(w.IDs.ContextPackID) != "" {
		cmd = append(cmd, "--context-pack", w.IDs.ContextPackID)
	}
	if strings.TrimSpace(w.RouteDecisionID) != "" {
		cmd = append(cmd, "--route-decision-id", w.RouteDecisionID)
	}
	if w.GitDelivery != nil {
		cmd = append(cmd,
			"--target-worktree-root", w.GitDelivery.TargetWorktreeRoot,
			"--target-local-ref", w.GitDelivery.TargetLocalRef,
			"--expected-base-sha", w.GitDelivery.ExpectedBaseSHA,
		)
		if w.GitDelivery.PublicationMode == "local_only" {
			cmd = append(cmd, "--local-only")
		} else {
			cmd = append(cmd, "--publication-ref", w.GitDelivery.PublicationRef)
		}
	}
	if w.Origin != nil {
		cmd = append(cmd,
			"--creator-harness", w.Origin.Harness,
			"--creator-model", w.Origin.RequestedModel,
			"--creator-lane", w.Origin.LaneID,
		)
	}
	if strings.TrimSpace(w.State) != "" {
		cmd = append(cmd, "--state", w.State)
	}
	if strings.TrimSpace(w.Risk) != "" {
		cmd = append(cmd, "--risk", w.Risk)
	}
	if strings.TrimSpace(w.Privacy) != "" {
		cmd = append(cmd, "--privacy", w.Privacy)
	}
	if strings.TrimSpace(w.PlanClass) != "" {
		cmd = append(cmd, "--plan-class", w.PlanClass)
	}
	if strings.TrimSpace(w.ReworkOfWorkID) != "" {
		cmd = append(cmd, "--rework-of", w.ReworkOfWorkID)
	}
	return cmd
}

func sameGitDeliveryContract(a, b *envelope.GitDeliveryContract) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func gitDeliveryRequiresRepositoryIdentityMigration(contract *envelope.GitDeliveryContract) bool {
	return contract != nil && contract.RepositoryFingerprint == ""
}

func repositoryIdentityMigrationError(w envelope.WorkEnvelope) error {
	return fmt.Errorf("work %s git_delivery repository fingerprint is unbound; run the governed operator same-state repository identity migration", w.ID)
}

func sameGitDeliveryAuthorityExceptFingerprint(a, b envelope.GitDeliveryContract) bool {
	a.RepositoryFingerprint = ""
	b.RepositoryFingerprint = ""
	return a == b
}

func validateNewGitDeliveryDeclaration(w envelope.WorkEnvelope) error {
	if strings.TrimSpace(w.ExpectedBaseSHA) != "" {
		return fmt.Errorf("legacy expected_base_sha cannot admit new work; declare a complete git_delivery contract")
	}
	return nil
}

func (s *Store) normalizeGitDeliveryForPlan(w *envelope.WorkEnvelope) error {
	if w == nil || w.GitDelivery == nil {
		if w != nil {
			return validateNewGitDeliveryDeclaration(*w)
		}
		return nil
	}
	root, err := canonicalLandingRepo(w.GitDelivery.TargetWorktreeRoot)
	if err != nil {
		return fmt.Errorf("git_delivery.target_worktree_root: %w", err)
	}
	_, localRef, err := normalizeLandingTargetRef(root, w.GitDelivery.TargetLocalRef)
	if err != nil {
		return fmt.Errorf("git_delivery.target_local_ref: %w", err)
	}
	w.GitDelivery.TargetWorktreeRoot = root
	w.GitDelivery.TargetLocalRef = localRef
	w.GitDelivery.ExpectedBaseSHA = strings.TrimSpace(w.GitDelivery.ExpectedBaseSHA)
	w.GitDelivery.PublicationMode = strings.TrimSpace(w.GitDelivery.PublicationMode)
	w.GitDelivery.PublicationRef = strings.TrimSpace(w.GitDelivery.PublicationRef)
	identity, err := gitrepository.ReadIdentity(root, w.GitDelivery.ExpectedBaseSHA)
	if err != nil {
		return fmt.Errorf("git_delivery.repository_fingerprint: %w", err)
	}
	// Repository identity is always derived from the bound repository. A caller
	// cannot choose or preserve a supplied fingerprint.
	w.GitDelivery.RepositoryFingerprint = identity.Fingerprint
	if err := w.Validate(); err != nil {
		return err
	}
	return s.validateBoundGitDelivery(*w)
}

// validateBoundGitDelivery proves the bound Git delivery contract against the
// repository with fresh Git reads. The governed mutation paths (create, close,
// admission, landing) always take this form.
func (s *Store) validateBoundGitDelivery(w envelope.WorkEnvelope) error {
	return s.validateBoundGitDeliveryWithFacts(w, nil)
}

// validateBoundGitDeliveryWithFacts is the session-aware form: a non-nil facts
// session reuses the repository reads this validation shares with the rest of
// one projection request, while every contract check stays per-Work.
func (s *Store) validateBoundGitDeliveryWithFacts(w envelope.WorkEnvelope, facts *landingGitFacts) error {
	if w.GitDelivery == nil {
		return validateNewGitDeliveryDeclaration(w)
	}
	if gitDeliveryRequiresRepositoryIdentityMigration(w.GitDelivery) {
		return repositoryIdentityMigrationError(w)
	}
	contract := *w.GitDelivery
	root, err := facts.canonicalRepo(contract.TargetWorktreeRoot)
	if err != nil {
		return fmt.Errorf("git_delivery.target_worktree_root: %w", err)
	}
	if root != contract.TargetWorktreeRoot {
		return fmt.Errorf("git_delivery.target_worktree_root %q is not the exact canonical root %q", contract.TargetWorktreeRoot, root)
	}
	_, localRef, err := facts.normalizeTargetRef(root, contract.TargetLocalRef)
	if err != nil {
		return fmt.Errorf("git_delivery.target_local_ref: %w", err)
	}
	if localRef != contract.TargetLocalRef {
		return fmt.Errorf("git_delivery.target_local_ref %q is not canonical %q", contract.TargetLocalRef, localRef)
	}
	identity, err := facts.repositoryIdentity(root, contract.ExpectedBaseSHA)
	if err != nil {
		return fmt.Errorf("git_delivery.repository_fingerprint: %w", err)
	}
	if identity.Fingerprint != contract.RepositoryFingerprint {
		return fmt.Errorf("git_delivery repository fingerprint %s no longer matches bound fingerprint %s", identity.Fingerprint, contract.RepositoryFingerprint)
	}
	if _, err := facts.refCommit(root, localRef); err != nil {
		return fmt.Errorf("git_delivery.target_local_ref does not resolve: %w", err)
	}
	baseExists, err := facts.commitExists(root, contract.ExpectedBaseSHA)
	if err != nil {
		return err
	}
	if !baseExists {
		return fmt.Errorf("git_delivery.expected_base_sha %s does not exist in bound repository", contract.ExpectedBaseSHA)
	}
	if contract.PublicationMode == "required" {
		if _, err := facts.refCommit(root, contract.PublicationRef); err != nil {
			return fmt.Errorf("git_delivery.publication_ref does not resolve: %w", err)
		}
	}
	return nil
}

func normalizeWorkActionContext(ctx *WorkActionContext) *WorkActionContext {
	if ctx == nil {
		return nil
	}
	out := cloneWorkActionContext(ctx)
	out.Kind = strings.TrimSpace(out.Kind)
	out.SourceRef = strings.TrimSpace(out.SourceRef)
	out.SourceID = strings.TrimSpace(out.SourceID)
	out.Title = strings.TrimSpace(out.Title)
	out.Boundary = strings.TrimSpace(out.Boundary)
	out.Proof = strings.TrimSpace(out.Proof)
	out.Command = compactStrings(out.Command...)
	out.ActionSummary = strings.TrimSpace(out.ActionSummary)
	out.RequiredInputs = compactStrings(out.RequiredInputs...)
	out.ExpectedValidation = compactStrings(out.ExpectedValidation...)
	out.StopConditions = compactStrings(out.StopConditions...)
	out.EvidenceRefs = compactStrings(out.EvidenceRefs...)
	if out.Kind == "" && out.SourceRef == "" && out.SourceID == "" && out.Title == "" && out.ActionSummary == "" &&
		len(out.Command) == 0 && len(out.RequiredInputs) == 0 && len(out.ExpectedValidation) == 0 &&
		len(out.StopConditions) == 0 && out.Boundary == "" && out.Proof == "" && len(out.EvidenceRefs) == 0 {
		return nil
	}
	return out
}

func cloneWorkActionContext(ctx *WorkActionContext) *WorkActionContext {
	if ctx == nil {
		return nil
	}
	out := *ctx
	out.Command = append([]string{}, ctx.Command...)
	out.RequiredInputs = append([]string{}, ctx.RequiredInputs...)
	out.ExpectedValidation = append([]string{}, ctx.ExpectedValidation...)
	out.StopConditions = append([]string{}, ctx.StopConditions...)
	out.EvidenceRefs = append([]string{}, ctx.EvidenceRefs...)
	return &out
}

func (s *Store) RecordCreatePlan(plan CreatePlan) (string, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return "", os.ErrInvalid
	}
	expectedID, err := createPlanContentID(plan)
	if err != nil {
		return "", err
	}
	if plan.ID != expectedID {
		return "", fmt.Errorf("create-plan %s content digest mismatch (expected %s)", plan.ID, expectedID)
	}
	dir := filepath.Join(s.home, "create-plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, plan.ID+".json")
	if err := s.writeJSON(path, &plan); err != nil {
		return "", err
	}
	return path, nil
}

func createPlanContentID(plan CreatePlan) (string, error) {
	canonical := plan
	canonical.ID = ""
	canonical.PlanPath = ""
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return idFor("wcrp_", string(b)), nil
}

func (s *Store) createActionIntent(w envelope.WorkEnvelope, actor string, now time.Time, opts CreatePlanOptions) envelope.ActionIntent {
	targetID := strings.TrimSpace(opts.TargetID)
	if targetID == "" {
		targetID = w.ID
	}
	targetPath := strings.TrimSpace(opts.TargetPath)
	if targetPath == "" {
		targetPath = s.workPath(w.ID)
	}
	capability := orDefault(opts.Capability, "work.create")
	action := orDefault(opts.Action, "create_work")
	targetKind := orDefault(opts.TargetKind, "work")
	mutationClass := orDefault(opts.MutationClass, "local_reversible")
	reversibility := orDefault(opts.Reversibility, "easy")
	blastRadius := orDefault(opts.BlastRadius, "local")
	idParts := []string{capability, action, targetKind, targetID, targetPath, mutationClass, reversibility, blastRadius, actor}
	preview := fmt.Sprintf("create work %s state %s", w.ID, w.State)
	if w.GitDelivery != nil {
		contract := w.GitDelivery
		idParts = append(idParts, contract.TargetWorktreeRoot, contract.RepositoryFingerprint, contract.TargetLocalRef, contract.ExpectedBaseSHA, contract.PublicationMode, contract.PublicationRef)
		preview = fmt.Sprintf("create work %s state %s with Git delivery target %s repository fingerprint %s ref %s expected base %s publication mode %s publication ref %s", w.ID, w.State, contract.TargetWorktreeRoot, contract.RepositoryFingerprint, contract.TargetLocalRef, contract.ExpectedBaseSHA, contract.PublicationMode, contract.PublicationRef)
	}
	return envelope.ActionIntent{
		Header: newHeader(
			envelope.SVActionIntent,
			idFor("act_", append(idParts, ts(now))...),
			now,
			w.ID,
			w.IDs.ContextPackID,
			w.Privacy,
			w.Risk,
		),
		Actor:         actor,
		Capability:    capability,
		Action:        action,
		Target:        envelope.ActionTarget{Kind: targetKind, ID: targetID, Path: targetPath},
		MutationClass: mutationClass,
		Reversibility: reversibility,
		BlastRadius:   blastRadius,
		Preview:       preview,
		RollbackPlan:  "delete created WorkEnvelope and matching lifecycle event before any claim/action starts",
	}
}

func summarizeCreatePolicy(pd envelope.PolicyDecision, source string) CreatePolicySummary {
	if strings.TrimSpace(source) == "" {
		source = "builtin:default"
	}
	return CreatePolicySummary{
		ActionIntentID:   pd.ActionIntentID,
		PolicyDecisionID: pd.ID,
		PolicySource:     source,
		Outcome:          pd.Outcome,
		Allowed:          policy.Allowed(pd),
		StandingGrantID:  pd.StandingGrantID,
		ExpiresAt:        pd.ExpiresAt,
		Reasons:          append([]string{}, pd.Reasons...),
		Obligations:      append([]string{}, pd.Obligations...),
		EvidenceRefs:     append([]string{}, pd.EvidenceRefs...),
	}
}
