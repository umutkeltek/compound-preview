package work

import (
	"errors"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/runattempt"
)

// v0.3 adds the always-serialized RunAttempt outcome and exact
// terminalization-intent integrity dimensions.
const ProofChainDoctorSchemaVersion = "compound.work_proof_chain_doctor.v0.3"

type ProofChainDoctorOptions struct {
	IncludeArchived bool
}

type ProofChainDoctorReport struct {
	SchemaVersion string                    `json:"schema_version"`
	Status        string                    `json:"status"`
	GeneratedAt   string                    `json:"generated_at"`
	Counts        ProofChainDoctorCounts    `json:"counts"`
	Items         []ProofChainDoctorItem    `json:"items,omitempty"`
	Findings      []ProofChainDoctorFinding `json:"findings,omitempty"`
	ProofRefs     []string                  `json:"proof_refs"`
}

type ProofChainDoctorCounts struct {
	WorksChecked               int `json:"works_checked"`
	TerminalChecked            int `json:"terminal_checked"`
	Complete                   int `json:"complete"`
	MissingReceipt             int `json:"missing_receipt"`
	MissingTransition          int `json:"missing_transition"`
	MissingActionIntent        int `json:"missing_action_intent"`
	MissingPolicyDecision      int `json:"missing_policy_decision"`
	InvalidReceipt             int `json:"invalid_receipt"`
	InvalidTransition          int `json:"invalid_transition"`
	InvalidActionIntent        int `json:"invalid_action_intent"`
	InvalidPolicyDecision      int `json:"invalid_policy_decision"`
	MismatchedWorkID           int `json:"mismatched_work_id"`
	MismatchedLink             int `json:"mismatched_link"`
	MismatchedActionPolicy     int `json:"mismatched_action_policy"`
	MissingReceiptAfter        int `json:"missing_receipt_after_ref"`
	MissingTransitionActionRef int `json:"missing_transition_action_ref"`
	OrphanReceipt              int `json:"orphan_receipt"`
	OrphanTransition           int `json:"orphan_transition"`
	OrphanActionIntent         int `json:"orphan_action_intent"`
	OrphanPolicyDecision       int `json:"orphan_policy_decision"`
	// RunAttempt proof dimension (v0.2). Each counter records one
	// terminal-work RunAttempt evidence defect; the finding code under
	// report.Findings is the narrow reason (missing_run_attempt,
	// run_attempt_unreadable, run_attempt_invalid, run_attempt_work_mismatch,
	// run_attempt_action_mismatch, run_attempt_nonterminal).
	MissingRunAttempt                 int `json:"missing_run_attempt"`
	UnreadableRunAttempt              int `json:"unreadable_run_attempt"`
	InvalidRunAttempt                 int `json:"invalid_run_attempt"`
	RunAttemptWorkMismatch            int `json:"run_attempt_work_mismatch"`
	RunAttemptActionMismatch          int `json:"run_attempt_action_mismatch"`
	RunAttemptNonterminal             int `json:"run_attempt_nonterminal"`
	RunAttemptOutcomeMismatch         int `json:"run_attempt_outcome_mismatch"`
	RunAttemptTerminalizationMismatch int `json:"run_attempt_terminalization_mismatch"`
}

type ProofChainDoctorItem struct {
	WorkID           string   `json:"work_id"`
	State            string   `json:"state"`
	ActionIntentIDs  []string `json:"action_intent_ids,omitempty"`
	PolicyDecisionID string   `json:"policy_decision_id,omitempty"`
	ReceiptID        string   `json:"receipt_id,omitempty"`
	TransitionID     string   `json:"transition_id,omitempty"`
	RunAttemptIDs    []string `json:"run_attempt_ids,omitempty"`
	ProofComplete    bool     `json:"proof_complete"`
	Problems         []string `json:"problems,omitempty"`
}

type ProofChainDoctorFinding struct {
	WorkID  string `json:"work_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Store) ProofChainDoctor(now time.Time, opts ProofChainDoctorOptions) (ProofChainDoctorReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	works, err := s.List()
	if opts.IncludeArchived {
		works, err = s.ListWithArchived()
	}
	if err != nil {
		return ProofChainDoctorReport{}, err
	}
	report := ProofChainDoctorReport{
		SchemaVersion: ProofChainDoctorSchemaVersion,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		ProofRefs: []string{
			"work-proof-chain-terminal-receipt",
			"work-proof-chain-terminal-transition",
			"work-proof-chain-work-id-match",
			"work-proof-chain-action-intent",
			"work-proof-chain-policy-decision",
			"work-proof-chain-transition-action-refs",
			"work-proof-chain-orphan-envelope-scan",
			"work-proof-chain-run-attempt",
		},
	}
	report.Counts.WorksChecked = len(works)
	// One events/work.jsonl read for the whole doctor: eventsByWorkID decodes the
	// full lifecycle log once under the work-event lock and groups by work_id
	// (eventsByWork[id] is identical to a per-work eventsFor slice — same filter
	// and order, see decodeAdmissionEvents). The prior shape re-read+re-decoded
	// the whole log once per terminal item (proofChainItem) PLUS once per work
	// item (the orphan scan), so the doctor was O(works x log size). This mirrors
	// the workbench.go batch seam.
	eventsByWork, err := s.eventsByWorkID()
	if err != nil {
		return ProofChainDoctorReport{}, err
	}
	// One runattempt.Store opened per doctor pass, reused across terminal items.
	// The store is cheap (path-only); opening once keeps the proof dimension
	// scoped to the same home every terminal item inspects.
	raStore := runattempt.Open(s.home)
	census, err := s.proofRunAttemptCensus(works, &report)
	if err != nil {
		return ProofChainDoctorReport{}, err
	}
	censusIncomplete := false
	for _, finding := range report.Findings {
		if finding.Code == "run_attempt_census_incomplete" {
			censusIncomplete = true
			break
		}
	}
	for _, w := range works {
		if !terminalProofRequired(w.State) {
			continue
		}
		report.Counts.TerminalChecked++
		item := s.proofChainItem(w, eventsByWork[w.ID], &report)
		if censusIncomplete {
			// Unknown membership affects every terminal certification, even if
			// this item's listed records are individually readable.
			item.Problems = append(item.Problems, "run_attempt_census_incomplete")
		}
		if len(item.RunAttemptIDs) > 0 {
			s.checkRunAttemptProof(w, eventsByWork[w.ID], raStore, &item, &report)
		}
		addReverseRunAttemptFindings(w, census[w.ID], &item, &report)
		if len(item.Problems) == 0 {
			item.ProofComplete = true
			report.Counts.Complete++
		}
		report.Items = append(report.Items, item)
	}
	s.addOrphanEnvelopeFindings(works, eventsByWork, &report)
	if len(report.Findings) > 0 {
		report.Status = "fail"
	}
	return report, nil
}

func (s *Store) proofChainItem(w envelope.WorkEnvelope, events []Event, report *ProofChainDoctorReport) ProofChainDoctorItem {
	item := ProofChainDoctorItem{WorkID: w.ID, State: w.State}
	// events is the batched eventsByWorkID slice for this work_id; a malformed
	// log already failed the whole doctor at the batch read, so there is no
	// per-item events_unreadable path.
	item.ReceiptID = lastAttachedWithPrefix(events, "rec_")
	item.TransitionID = firstNonEmpty(w.IDs.TransitionID, lastAttachedWithPrefix(events, "tr_"))
	item.ActionIntentIDs = compactPrefix(w.ActionIntentIDs, "act_")
	item.PolicyDecisionID = strings.TrimSpace(w.PolicyDecisionID)
	// RunAttemptIDs: copy and dedupe the raw slot. compactPrefix would
	// silently drop malformed entries and turn a populated bad slot into a
	// legacy zero-ref work -- a silent corruption trap. The shape check
	// runs per-entry inside checkRunAttemptProof so the doctor can name the
	// exact bad reference.
	item.RunAttemptIDs = dedupeStrings(w.RunAttemptIDs)
	if len(item.ActionIntentIDs) == 0 {
		report.Counts.MissingActionIntent++
		addProofChainProblem(report, &item, "missing_action_intent", "terminal work has no action intent run ref")
	}
	if item.PolicyDecisionID == "" {
		report.Counts.MissingPolicyDecision++
		addProofChainProblem(report, &item, "missing_policy_decision", "terminal work has no policy decision ref")
	}
	if item.ReceiptID == "" {
		report.Counts.MissingReceipt++
		addProofChainProblem(report, &item, "missing_receipt", "terminal work has no closing receipt ref")
	}
	if item.TransitionID == "" {
		report.Counts.MissingTransition++
		addProofChainProblem(report, &item, "missing_transition", "terminal work has no closing transition ref")
	}

	var rec envelope.Receipt
	receiptValid := false
	if item.ReceiptID != "" {
		if err := s.readEventJSON(item.ReceiptID, &rec); err != nil {
			report.Counts.InvalidReceipt++
			addProofChainProblem(report, &item, "receipt_unreadable", "receipt "+item.ReceiptID+" unreadable: "+err.Error())
		} else if err := rec.Validate(); err != nil {
			report.Counts.InvalidReceipt++
			addProofChainProblem(report, &item, "receipt_invalid", "receipt "+item.ReceiptID+" invalid: "+err.Error())
		} else {
			receiptValid = true
			if rec.IDs.WorkID != w.ID {
				report.Counts.MismatchedWorkID++
				addProofChainProblem(report, &item, "receipt_work_mismatch", "receipt "+item.ReceiptID+" belongs to work "+rec.IDs.WorkID)
			}
		}
	}

	actionIDs := map[string]bool{}
	for _, actionID := range item.ActionIntentIDs {
		actionIDs[actionID] = true
		var ai envelope.ActionIntent
		if err := s.readEventJSON(actionID, &ai); err != nil {
			report.Counts.InvalidActionIntent++
			addProofChainProblem(report, &item, "action_intent_unreadable", "action intent "+actionID+" unreadable: "+err.Error())
		} else if err := ai.Validate(); err != nil {
			report.Counts.InvalidActionIntent++
			addProofChainProblem(report, &item, "action_intent_invalid", "action intent "+actionID+" invalid: "+err.Error())
		} else if ai.IDs.WorkID != w.ID {
			report.Counts.MismatchedWorkID++
			addProofChainProblem(report, &item, "action_intent_work_mismatch", "action intent "+actionID+" belongs to work "+ai.IDs.WorkID)
		}
	}

	if item.PolicyDecisionID != "" {
		var pd envelope.PolicyDecision
		if err := s.readEventJSON(item.PolicyDecisionID, &pd); err != nil {
			report.Counts.InvalidPolicyDecision++
			addProofChainProblem(report, &item, "policy_decision_unreadable", "policy decision "+item.PolicyDecisionID+" unreadable: "+err.Error())
		} else if err := pd.Validate(); err != nil {
			report.Counts.InvalidPolicyDecision++
			addProofChainProblem(report, &item, "policy_decision_invalid", "policy decision "+item.PolicyDecisionID+" invalid: "+err.Error())
		} else {
			if pd.IDs.WorkID != w.ID {
				report.Counts.MismatchedWorkID++
				addProofChainProblem(report, &item, "policy_decision_work_mismatch", "policy decision "+item.PolicyDecisionID+" belongs to work "+pd.IDs.WorkID)
			}
			if !actionIDs[pd.ActionIntentID] {
				report.Counts.MismatchedActionPolicy++
				addProofChainProblem(report, &item, "policy_action_mismatch", "policy decision "+item.PolicyDecisionID+" references action "+pd.ActionIntentID+" not present in work action intent ids")
			}
		}
	}

	if item.TransitionID != "" {
		var tr envelope.Transition
		if err := s.readEventJSON(item.TransitionID, &tr); err != nil {
			report.Counts.InvalidTransition++
			addProofChainProblem(report, &item, "transition_unreadable", "transition "+item.TransitionID+" unreadable: "+err.Error())
		} else if err := tr.Validate(); err != nil {
			report.Counts.InvalidTransition++
			addProofChainProblem(report, &item, "transition_invalid", "transition "+item.TransitionID+" invalid: "+err.Error())
		} else {
			if tr.IDs.WorkID != w.ID {
				report.Counts.MismatchedWorkID++
				addProofChainProblem(report, &item, "transition_work_mismatch", "transition "+item.TransitionID+" belongs to work "+tr.IDs.WorkID)
			}
			if strings.TrimSpace(w.IDs.TransitionID) != "" && w.IDs.TransitionID != item.TransitionID {
				report.Counts.MismatchedLink++
				addProofChainProblem(report, &item, "work_transition_link_mismatch", "work transition_id "+w.IDs.TransitionID+" differs from closing transition "+item.TransitionID)
			}
			if receiptValid && !proofChainContains(tr.AfterRefs, rec.ID) {
				report.Counts.MissingReceiptAfter++
				addProofChainProblem(report, &item, "transition_missing_receipt_ref", "transition "+item.TransitionID+" after_refs does not include receipt "+rec.ID)
			}
			for _, actionID := range item.ActionIntentIDs {
				if !proofChainContains(tr.ActionRefs, actionID) {
					report.Counts.MissingTransitionActionRef++
					addProofChainProblem(report, &item, "transition_missing_action_ref", "transition "+item.TransitionID+" action_refs does not include action "+actionID)
				}
			}
		}
	}
	return item
}

func (s *Store) addOrphanEnvelopeFindings(works []envelope.WorkEnvelope, eventsByWork map[string][]Event, report *ProofChainDoctorReport) {
	referenced := map[string]bool{}
	for _, w := range works {
		for _, ref := range w.ActionIntentIDs {
			addProofChainRef(referenced, ref)
		}
		addProofChainRef(referenced, w.PolicyDecisionID)
		addProofChainRef(referenced, w.IDs.TransitionID)
		for _, event := range eventsByWork[w.ID] {
			for _, ref := range event.Attached {
				addProofChainRef(referenced, ref)
			}
		}
	}

	entries, err := s.events.Scan(eventstore.ScanOptions{})
	if err != nil {
		report.Findings = append(report.Findings, ProofChainDoctorFinding{Code: "envelopes_unreadable", Message: "could not scan envelope root: " + err.Error()})
		return
	}
	for _, entry := range entries {
		id := entry.ID
		if referenced[id] {
			continue
		}
		switch {
		case strings.HasPrefix(id, "act_"):
			report.Counts.OrphanActionIntent++
			report.Findings = append(report.Findings, ProofChainDoctorFinding{WorkID: s.envelopeWorkID(id), Code: "orphan_action_intent", Message: "action intent " + id + " is not referenced by any work action intent ids or lifecycle event"})
		case strings.HasPrefix(id, "pol_"):
			report.Counts.OrphanPolicyDecision++
			report.Findings = append(report.Findings, ProofChainDoctorFinding{WorkID: s.envelopeWorkID(id), Code: "orphan_policy_decision", Message: "policy decision " + id + " is not referenced by any work policy_decision_id or lifecycle event"})
		case strings.HasPrefix(id, "rec_"):
			report.Counts.OrphanReceipt++
			report.Findings = append(report.Findings, ProofChainDoctorFinding{WorkID: s.envelopeWorkID(id), Code: "orphan_receipt", Message: "receipt " + id + " is not referenced by any terminal lifecycle event"})
		case strings.HasPrefix(id, "tr_"):
			report.Counts.OrphanTransition++
			report.Findings = append(report.Findings, ProofChainDoctorFinding{WorkID: s.envelopeWorkID(id), Code: "orphan_transition", Message: "transition " + id + " is not referenced by any work transition_id or lifecycle event"})
		default:
			continue
		}
	}
}

func addProofChainRef(refs map[string]bool, ref string) {
	ref = strings.TrimSpace(ref)
	if ref != "" {
		refs[ref] = true
	}
}

func (s *Store) envelopeWorkID(id string) string {
	switch {
	case strings.HasPrefix(id, "act_"):
		var ai envelope.ActionIntent
		if err := s.readEventJSON(id, &ai); err == nil {
			return ai.IDs.WorkID
		}
	case strings.HasPrefix(id, "pol_"):
		var pd envelope.PolicyDecision
		if err := s.readEventJSON(id, &pd); err == nil {
			return pd.IDs.WorkID
		}
	case strings.HasPrefix(id, "rec_"):
		var rec envelope.Receipt
		if err := s.readEventJSON(id, &rec); err == nil {
			return rec.IDs.WorkID
		}
	case strings.HasPrefix(id, "tr_"):
		var tr envelope.Transition
		if err := s.readEventJSON(id, &tr); err == nil {
			return tr.IDs.WorkID
		}
	}
	return ""
}

func compactPrefix(values []string, prefix string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || !strings.HasPrefix(value, prefix) || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// dedupeStrings dedupes a string slice while preserving every entry the
// caller put there -- including entries that trim to empty. Used for the
// RunAttemptIDs slot, where compactPrefix's prefix filter would silently
// turn a populated malformed slot into a legacy zero-ref work; checkRunAttemptProof
// then classifies each preserved entry on its own merits (whitespace-only,
// empty, padded, and non-ratt_ entries surface run_attempt_invalid).
func dedupeStrings(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func addProofChainProblem(report *ProofChainDoctorReport, item *ProofChainDoctorItem, code, message string) {
	item.Problems = append(item.Problems, code)
	report.Findings = append(report.Findings, ProofChainDoctorFinding{WorkID: item.WorkID, Code: code, Message: message})
}

func proofChainContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// checkRunAttemptProof validates every RunAttempt id referenced by a terminal
// work envelope against the live runattempt.Store. The slice order preserves
// schema v0.1 compatibility for terminal work with zero RunAttempt refs
// (legacy evidence) and for legacy RunAttempt records with empty
// action_intent_id (delegate evidence).
//
// Validation operates on the raw entry -- no trimming. Whitespace-only and
// padded canonical refs classify as run_attempt_invalid because
// runattempt.Valid only accepts the exact `ratt_<UTC>_<8 hex>` shape.
//
// Defect ordering matters: identity (embedded id / schema_version) is
// checked AFTER decode and BEFORE WorkID linkage so a corrupt record (empty
// WorkID, half-decoded bytes, multi-corrupt file) classifies as
// run_attempt_invalid instead of masking as run_attempt_work_mismatch. After
// an identity mismatch the loop continues to the next reference -- the
// work_id / action / terminal checks only run on records with valid
// embedded identity, because those checks would otherwise misattribute the
// half-decoded record's emptiness as additional defects.
func (s *Store) checkRunAttemptProof(w envelope.WorkEnvelope, events []Event, raStore *runattempt.Store, item *ProofChainDoctorItem, report *ProofChainDoctorReport) {
	actionSet := map[string]bool{}
	for _, actionID := range item.ActionIntentIDs {
		actionSet[actionID] = true
	}
	type historyEligibilityProblem struct {
		code    string
		message string
	}
	attachedAttempts := map[string]runattempt.RunAttempt{}
	eligibleIDs := []string{}
	historyProblems := map[string]historyEligibilityProblem{}
	for _, rattID := range item.RunAttemptIDs {
		if !runattempt.Valid(rattID) {
			continue
		}
		ra, err := raStore.Get(rattID)
		if err != nil || ra.ID != rattID || !runattempt.AcceptsSchemaVersion(ra.SchemaVersion) || ra.WorkID != w.ID {
			continue
		}
		if err := validateRunAttemptRecord(rattID, w, ra); err != nil {
			historyProblems[rattID] = historyEligibilityProblem{code: "run_attempt_invalid", message: err.Error()}
			continue
		}
		if _, err := s.resolveRunAttemptAuthorityFromEvents(w, ra, events); err != nil {
			historyProblems[rattID] = historyEligibilityProblem{code: "run_attempt_action_mismatch", message: err.Error()}
			continue
		}
		attachedAttempts[rattID] = ra
		eligibleIDs = append(eligibleIDs, rattID)
	}
	history, lineageErr := buildAttachedRunAttemptHistory(eligibleIDs, attachedAttempts)
	if lineageErr != nil {
		report.Counts.InvalidRunAttempt++
		addProofChainProblem(report, item, "run_attempt_invalid", "run attempt retry lineage is invalid: "+lineageErr.Error())
		history = attachedRunAttemptHistory{
			SuccessfulRetryPredecessors: map[string]bool{},
			ReplacedClaimReservations:   map[string][]string{},
		}
	}
	closeEvidence, closeEvidenceErr := s.loadExactTerminalCloseFromEvents(w, events)
	for _, rattID := range item.RunAttemptIDs {
		// Validate the EXACT raw entry. No trimming -- whitespace-only and
		// padded canonical refs are run_attempt_invalid.
		if !runattempt.Valid(rattID) {
			report.Counts.InvalidRunAttempt++
			addProofChainProblem(report, item, "run_attempt_invalid", "run attempt reference "+rattID+" is not a syntactically valid ratt_ id")
			continue
		}
		ra, err := raStore.Get(rattID)
		if err != nil {
			if errors.Is(err, runattempt.ErrNotFound) {
				report.Counts.MissingRunAttempt++
				addProofChainProblem(report, item, "missing_run_attempt", "run attempt "+rattID+" is referenced by work "+w.ID+" but no run_attempts/"+rattID+".json exists")
				continue
			}
			report.Counts.UnreadableRunAttempt++
			addProofChainProblem(report, item, "run_attempt_unreadable", "run attempt "+rattID+" unreadable: "+err.Error())
			continue
		}
		// Identity check: embedded id + schema_version. A mismatch
		// classifies as run_attempt_invalid and the loop continues to the
		// next reference -- the work/action/terminal checks would otherwise
		// misattribute the half-decoded record's emptiness.
		if ra.ID != rattID || !runattempt.AcceptsSchemaVersion(ra.SchemaVersion) {
			report.Counts.InvalidRunAttempt++
			addProofChainProblem(report, item, "run_attempt_invalid", "run attempt "+rattID+" embedded identity mismatches: embedded id="+ra.ID+" schema="+ra.SchemaVersion)
			continue
		}
		// Identity-valid record: orthogonal work/action/terminal checks.
		if ra.WorkID != w.ID {
			report.Counts.RunAttemptWorkMismatch++
			report.Counts.MismatchedWorkID++
			addProofChainProblem(report, item, "run_attempt_work_mismatch", "run attempt "+rattID+" belongs to work "+ra.WorkID+", not "+w.ID)
		}
		if !runattempt.IsTerminal(ra.Status) {
			if ra.ActionIntentID != "" && !actionSet[ra.ActionIntentID] {
				report.Counts.RunAttemptActionMismatch++
				addProofChainProblem(report, item, "run_attempt_action_mismatch", "run attempt "+rattID+" action_intent_id "+ra.ActionIntentID+" is not present in work "+w.ID+" action intent ids")
			}
			report.Counts.RunAttemptNonterminal++
			addProofChainProblem(report, item, "run_attempt_nonterminal", "run attempt "+rattID+" status "+ra.Status+" is not terminal while work "+w.ID+" is terminal")
			continue
		}
		if problem, ok := historyProblems[rattID]; ok {
			switch problem.code {
			case "run_attempt_invalid":
				report.Counts.InvalidRunAttempt++
			case "run_attempt_action_mismatch":
				report.Counts.RunAttemptActionMismatch++
			}
			addProofChainProblem(report, item, problem.code, "run attempt "+rattID+" is ineligible for retry history: "+problem.message)
			continue
		}
		if ra.ActionIntentID != "" && !actionSet[ra.ActionIntentID] {
			report.Counts.RunAttemptActionMismatch++
			addProofChainProblem(report, item, "run_attempt_action_mismatch", "run attempt "+rattID+" action_intent_id "+ra.ActionIntentID+" is not present in work "+w.ID+" action intent ids")
		}
		if runAttemptHasUncertainLaunch(ra) {
			report.Counts.InvalidRunAttempt++
			addProofChainProblem(report, item, "run_attempt_launch_uncertain", "run attempt "+rattID+" has active or uncertain launch effects; reconcile execution before terminal certification")
			continue
		}
		if ra.TerminalizationIntent != nil {
			intent := *ra.TerminalizationIntent
			intentErr := closeEvidenceErr
			if intentErr == nil {
				intentErr = s.validateTerminalizationIntentAgainstClose(w, ra, intent, closeEvidence)
			}
			if intentErr != nil || ra.TerminalizationOwner != intent.Owner || ra.TerminalizationReceiptID != intent.ReceiptID || ra.TerminalizationTransitionID != intent.TransitionID || ra.TerminalizationReason != intent.TerminalizationReason {
				report.Counts.RunAttemptTerminalizationMismatch++
				message := "run attempt " + rattID + " terminalization does not match its exact prepared intent and closing proof"
				if intentErr != nil {
					message += ": " + intentErr.Error()
				}
				addProofChainProblem(report, item, "run_attempt_terminalization_mismatch", message)
			}
			if ra.Status != intent.TargetStatus {
				report.Counts.RunAttemptOutcomeMismatch++
				addProofChainProblem(report, item, "run_attempt_outcome_mismatch", "run attempt "+rattID+" status "+ra.Status+" does not match prepared target "+intent.TargetStatus)
			}
			continue
		}
		want := runAttemptStatusForCloseOutcome(w.State)
		historicalOutcomeCompatibleAttempt := historicalRunAttemptOutcomeCompatible(w.State, rattID, history)
		verificationRejected := closeEvidenceErr == nil && verificationRejectedOutcomeCompatible(w, ra, closeEvidence)
		nonExecutedAttempt := w.State == "completed" && (runAttemptPolicyRefusedBeforeExecution(ra) ||
			runAttemptSweptSideEffectFreeReservation(ra) || runAttemptRefusedStartBeforeExecution(ra))
		if ra.Status != runattempt.StatusSuperseded && ra.Status != want &&
			!historicalOutcomeCompatibleAttempt && !nonExecutedAttempt && !verificationRejected {
			verifiedProof, proofErr := s.committedVerifiedProofRunAttempt(w, ra)
			if !verifiedProof {
				report.Counts.RunAttemptOutcomeMismatch++
				message := "run attempt " + rattID + " status " + ra.Status + " contradicts terminal work outcome " + w.State + " (want " + want + ")"
				if proofErr != nil {
					message += ": proof-resource exception invalid: " + proofErr.Error()
				}
				addProofChainProblem(report, item, "run_attempt_outcome_mismatch", message)
			}
		}
		if ra.TerminalizationOwner == runattempt.TerminalizationOwnerWorkClose &&
			(ra.TerminalizationReceiptID != item.ReceiptID || ra.TerminalizationTransitionID != item.TransitionID) {
			report.Counts.RunAttemptTerminalizationMismatch++
			addProofChainProblem(report, item, "run_attempt_terminalization_mismatch", "run attempt "+rattID+" work.close proof pair "+ra.TerminalizationReceiptID+"/"+ra.TerminalizationTransitionID+" does not match closing proof "+item.ReceiptID+"/"+item.TransitionID)
		}
	}
}
