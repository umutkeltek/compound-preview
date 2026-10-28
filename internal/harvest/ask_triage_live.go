package harvest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/hook"
	"compound/internal/mem"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

const (
	AskTriageReviewArea          = "ask-triage"
	AskTriageReviewSchemaVersion = "compound.ask_triage_review.v0.1"
)

type CodexStopInput struct {
	SessionID            string
	TurnID               string
	TranscriptPath       string
	LastAssistantMessage string
	Cwd                  string
	StopHookActive       bool
	JobsStateDir         string
}

type CodexStopOutcome struct {
	WorkID           string
	Handled          bool
	Decision         string
	Reason           string
	Disposition      string
	ActionIntentID   string
	PolicyDecisionID string
	ReceiptID        string
	TransitionID     string
	ReviewID         string
	SideEffects      []string
}

type AskTriageReview struct {
	SchemaVersion  string   `json:"schema_version"`
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	Band           string   `json:"band"`
	Readiness      string   `json:"readiness"`
	CreatedAt      string   `json:"created_at"`
	Disposition    string   `json:"disposition"`
	Ask            string   `json:"ask"`
	Context        string   `json:"context,omitempty"`
	WorkID         string   `json:"work_id,omitempty"`
	SessionID      string   `json:"session_id"`
	TurnID         string   `json:"turn_id,omitempty"`
	TranscriptPath string   `json:"transcript_path,omitempty"`
	TranscriptLine int      `json:"transcript_line,omitempty"`
	SourceKind     string   `json:"source_kind"`
	EvidenceRefs   []string `json:"evidence_refs"`
	BlockedReason  string   `json:"blocked_reason,omitempty"`
	NextAction     string   `json:"next_action"`
	NotToDo        string   `json:"not_to_do"`

	OperatorSummary string `json:"operator_summary"`
	ContextLabel    string `json:"context_label"`
	GroupKey        string `json:"group_key"`
	OperatorUnlock  string `json:"operator_unlock,omitempty"`
}

type codexTrackedJob struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PID           int    `json:"pid"`
	ThreadID      string `json:"threadId"`
	TurnID        string `json:"turnId"`
	Summary       string `json:"summary"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspaceRoot"`
	Request       struct {
		Cwd    string `json:"cwd"`
		Prompt string `json:"prompt"`
	} `json:"request"`
}

type codexStopBinding struct {
	Job         codexTrackedJob
	Work        envelope.WorkEnvelope
	JobRef      string
	BindingRefs []string
}

// AskLawsFromClaims admits only operator-provenanced, operational decision
// claims. Generic recalled meaning may inform review, but it can never become
// permission to continue a lane.
func AskLawsFromClaims(claims []mem.Claim) []AskLaw {
	active := mem.AuthoritativeActiveOperationalDecisions(claims)
	laws := make([]AskLaw, 0, len(active))
	for _, claim := range active {
		lifecycle := decisions.LifecycleFromClaim(claim)
		parts := []string{claim.Object, lifecycle.Scope, claim.Qualifiers["freeze_keywords"], claim.Qualifiers["domain"]}
		termSet := significantTokens(strings.Join(parts, " "))
		terms := make([]string, 0, len(termSet))
		for term := range termSet {
			terms = append(terms, term)
		}
		sort.Strings(terms)
		if len(terms) < 2 {
			continue
		}
		laws = append(laws, AskLaw{ClaimID: claim.ID, Statement: strings.TrimSpace(claim.Object), Terms: terms, MinTerms: 2})
	}
	return laws
}

// HandleCodexStop turns one real Stop event into either a governed continuation
// on the existing WorkEnvelope or a prepared review. It never closes the parent:
// task success remains owned by the eventual Receipt and Transition.
func HandleCodexStop(homePath string, in CodexStopInput, now time.Time) (CodexStopOutcome, error) {
	var out CodexStopOutcome
	if now.IsZero() {
		now = time.Now()
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		return out, err
	}
	binding, bindingReason, err := resolveCodexStopBinding(workStore, in)
	if err != nil {
		return out, err
	}
	if binding != nil {
		out.WorkID = binding.Work.ID
	}
	if in.StopHookActive {
		return out, nil
	}

	ask, err := stopBlockingAsk(in)
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(ask.Text) == "" {
		return out, nil
	}

	memStore, err := mem.Open(homePath)
	if err != nil {
		return out, err
	}
	claims, err := memStore.AllCurrentOperationalDecisions()
	if err != nil {
		return out, err
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		return out, err
	}
	laws := AskLawsFromClaims(claims)
	riskAssessment := assessAskRisk(ask.Text, cfg)
	riskFloor := riskAssessment.Reason != ""
	if binding != nil && (binding.Work.Risk == "tier2" || binding.Work.Risk == "tier3") {
		riskFloor = true
	}
	if riskFloor {
		laws = nil
	}
	triage := TriageAsk(AskTriageInput{
		Ask:          ask.Text,
		Context:      ask.Context,
		LaneID:       in.SessionID + ":" + in.TurnID,
		WorkID:       out.WorkID,
		Now:          now,
		StandingLaw:  laws,
		PolicyConfig: &cfg,
	})

	if triage.ResumeLane {
		if binding == nil {
			return writeStopReview(homePath, out, in, ask, triage, "delivery_unavailable", bindingReason, now)
		}
		if !askContinuableWorkState(binding.Work.State) {
			return writeStopReview(homePath, out, in, ask, triage, "delivery_unavailable", "bound work is not in a continuable state", now)
		}
		return recordStopContinuation(homePath, workStore, *binding, in, ask, triage, now)
	}

	disposition := "novel"
	blockedReason := "no current operator-provenanced standing law covers this ask"
	if riskFloor {
		disposition = "risk_floor"
		blockedReason = firstAskNonEmpty(riskAssessment.Reason, "the bound work is tier2/tier3 and requires operator review")
	}
	return writeStopReview(homePath, out, in, ask, triage, disposition, blockedReason, now)
}

func stopBlockingAsk(in CodexStopInput) (hook.BlockingAsk, error) {
	var transcriptAsk hook.BlockingAsk
	var transcriptErr error
	path := strings.TrimSpace(in.TranscriptPath)
	if path != "" {
		transcriptAsk, transcriptErr = hook.LastBlockingAsk(path)
	}
	if latest := strings.TrimSpace(in.LastAssistantMessage); latest != "" {
		question := hook.BlockingQuestion(latest)
		if question == "" {
			return hook.BlockingAsk{}, nil
		}
		if transcriptErr == nil && transcriptAsk.Text == question {
			return transcriptAsk, nil
		}
		return hook.BlockingAsk{Text: question, Context: transcriptAsk.Context}, nil
	}
	return transcriptAsk, transcriptErr
}

func resolveCodexStopBinding(store *work.Store, in CodexStopInput) (*codexStopBinding, string, error) {
	root := strings.TrimSpace(in.JobsStateDir)
	if root == "" {
		if resolved, ok := work.DefaultCodexJobsStateDir(); ok {
			root = resolved
		}
	}
	if root == "" || strings.TrimSpace(in.SessionID) == "" || strings.TrimSpace(in.TurnID) == "" {
		return nil, "Stop payload lacks an exact companion thread and turn binding", nil
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "jobs", "*.json"))
	if err != nil {
		return nil, "", err
	}
	var matches []codexStopBinding
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var job codexTrackedJob
		// The live Stop payload's exact thread/turn pair is the delivery identity.
		// PID liveness is only a negative filter for stale running job records.
		if json.Unmarshal(data, &job) != nil || !strings.EqualFold(strings.TrimSpace(job.Status), "running") || !work.PIDAlive(job.PID) || strings.TrimSpace(job.ThreadID) != strings.TrimSpace(in.SessionID) || strings.TrimSpace(job.TurnID) != strings.TrimSpace(in.TurnID) {
			continue
		}
		matches = append(matches, codexStopBinding{Job: job, JobRef: "codex-job:" + firstAskNonEmpty(job.ID, filepath.Base(path))})
	}
	if len(matches) != 1 {
		return nil, fmt.Sprintf("expected one exact companion thread/turn/work binding, found %d", len(matches)), nil
	}
	binding := &matches[0]
	workIDs := work.LegacyJobWorkReferences(binding.Job.Summary, binding.Job.Title, binding.Job.Request.Prompt)
	if len(workIDs) > 1 {
		return nil, "companion job contains conflicting legacy work references", nil
	}
	recipient, owned, err := store.ResolveCollaborationRecipient(work.CollaborationRecipientQuery{
		Harness: "codex", SessionID: in.SessionID, Cwd: in.Cwd,
		CheckPriorBindings: len(workIDs) == 1,
	})
	if err != nil {
		return nil, "", err
	}
	if !owned && recipient.UnresolvedReason != "" {
		return nil, recipient.UnresolvedReason, nil
	}
	workID := recipient.WorkID
	if owned {
		if len(workIDs) == 1 && workIDs[0] != workID {
			return nil, "owned session work conflicts with the companion job's legacy reference", nil
		}
		binding.BindingRefs = []string{"work-binding:collaboration_recipient"}
		if recipient.RunAttemptID != "" {
			binding.BindingRefs = append(binding.BindingRefs, recipient.RunAttemptID)
		}
	} else {
		if len(workIDs) != 1 {
			return nil, "no owned session work or unique legacy companion work reference", nil
		}
		workID = workIDs[0]
		binding.BindingRefs = []string{"work-binding:legacy_codex_job_reference"}
	}
	// Get resolves a path; reject path-shaped references before consulting it,
	// then require the complete embedded identity rather than trusting a prefix.
	if filepath.Base(workID) != workID || strings.ContainsAny(workID, `/\`) {
		return nil, "companion work reference is not a single work identity", nil
	}
	w, err := store.Get(workID)
	if err != nil || w.ID != workID || w.Validate() != nil {
		return nil, "companion work reference does not resolve to the exact valid envelope", nil
	}
	binding.Work = w
	return binding, "", nil
}

func askContinuableWorkState(state string) bool {
	switch strings.TrimSpace(state) {
	case "claimed", "running", "verifying":
		return true
	default:
		return false
	}
}

func recordStopContinuation(homePath string, store *work.Store, binding codexStopBinding, in CodexStopInput, ask hook.BlockingAsk, triage AskTriageResult, now time.Time) (CodexStopOutcome, error) {
	evidenceRefs := askStopEvidenceRefs(binding.Work.ID, binding.JobRef, in, ask, triage.CitedRecordID)
	evidenceRefs = append(evidenceRefs, binding.BindingRefs...)
	actionID := reviewartifact.ID("act_", "ask-triage", binding.Work.ID, in.SessionID, in.TurnID, ask.Text, triage.CitedRecordID)
	action := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actionID,
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound", LaneID: "codex"},
			IDs:           envelope.IDs{WorkID: binding.Work.ID, SessionID: in.SessionID, ContextPackID: binding.Work.IDs.ContextPackID},
			Scope:         envelope.Scope{Surface: strings.TrimSpace(in.Cwd)},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       firstAskNonEmpty(binding.Work.Privacy, "private"),
			Risk:          firstAskNonEmpty(binding.Work.Risk, "tier1"),
			EvidenceRefs:  evidenceRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "codex",
		Capability:    "harness.stop_feedback",
		Action:        "resume_lane",
		Target:        envelope.ActionTarget{Kind: "harness_turn", ID: in.SessionID + ":" + in.TurnID},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "continue the current Codex turn with a cited standing-law answer",
		RollbackPlan:  "retain the Stop event and route any operator contradiction through ruling harvest",
	}

	result, err := store.RecordReceiptedAction(binding.Work.ID, work.ReceiptedActionInput{
		Action:                     action,
		OperationalDecisionClaimID: triage.CitedRecordID,
		ExpectedWorkState:          binding.Work.State,
		Summary:                    "auto-answered a blocking ask from current standing law and resumed the bound Codex turn",
		MoveType:                   "control_plane_repair",
		ProofRefs:                  evidenceRefs,
	}, now)
	if err != nil {
		triage.ResumeLane = false
		return writeStopReview(homePath, CodexStopOutcome{WorkID: binding.Work.ID}, in, ask, triage, "policy_hold", err.Error(), now)
	}
	sideEffects, err := logicalEventRefs(result.Action.ID, result.Decision.ID, result.Receipt.ID, result.Transition.ID)
	if err != nil {
		return CodexStopOutcome{}, err
	}
	return CodexStopOutcome{
		WorkID: binding.Work.ID, Handled: true, Decision: "block", Reason: triage.Answer, Disposition: string(AskAutoAnswered),
		ActionIntentID: result.Action.ID, PolicyDecisionID: result.Decision.ID, ReceiptID: result.Receipt.ID, TransitionID: result.Transition.ID,
		SideEffects: sideEffects,
	}, nil
}

func logicalEventRefs(ids ...string) ([]string, error) {
	refs := make([]string, 0, len(ids))
	for _, id := range ids {
		ref, err := eventstore.ReferenceForID(id)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref.String())
	}
	return refs, nil
}

func writeStopReview(homePath string, out CodexStopOutcome, in CodexStopInput, ask hook.BlockingAsk, triage AskTriageResult, disposition, blockedReason string, now time.Time) (CodexStopOutcome, error) {
	id := reviewartifact.ID("ask_", in.SessionID, in.TurnID, ask.Text, disposition)
	evidenceRefs := askStopEvidenceRefs(out.WorkID, "", in, ask, triage.CitedRecordID)
	plainAsk := reviewartifact.ShortLabel(reviewartifact.PlainLabel(strings.TrimSuffix(ask.Text, "?")), 104)
	summary := "Should the paused lane continue?"
	if plainAsk != "" {
		summary = "Should the paused lane continue: " + plainAsk + "?"
	}
	card, err := reviewartifact.NewOperatorCard(summary, "Paused Codex question", AskTriageReviewArea+"|"+reviewartifact.Digest12(id), "Resolving this question determines whether the lane continues.")
	if err != nil {
		return out, err
	}
	review := AskTriageReview{
		SchemaVersion: AskTriageReviewSchemaVersion,
		ID:            id, Status: "review_required", Band: reviewartifact.BandNeedsYou, Readiness: "needs_review",
		CreatedAt: now.UTC().Format(time.RFC3339), Disposition: disposition, Ask: ask.Text, Context: ask.Context,
		WorkID: out.WorkID, SessionID: in.SessionID, TurnID: in.TurnID, TranscriptPath: in.TranscriptPath, TranscriptLine: ask.LineNo,
		SourceKind: "codex_stop", EvidenceRefs: evidenceRefs, BlockedReason: strings.TrimSpace(blockedReason),
		NextAction:      "Review the prepared options and record the operator decision.",
		NotToDo:         "Do not resume the lane without a current law match and an exact delivery binding.",
		OperatorSummary: card.Summary, ContextLabel: card.ContextLabel, GroupKey: card.GroupKey, OperatorUnlock: card.Unlock,
	}
	briefSource := firstAskNonEmpty(transcriptAskEvidence(in.TranscriptPath, ask.LineNo), "session:"+in.SessionID+":turn:"+in.TurnID)
	brief := reviewartifact.DecisionBrief{
		SchemaVersion: reviewartifact.DecisionBriefSchemaVersion,
		Headline:      ask.Text,
		WhatThisIs:    "A paused Codex lane needs a decision because Compound classified the ask as " + disposition + ".",
		Findings: []reviewartifact.DecisionBriefFinding{
			{Finding: "The lane ended its turn with this blocking question.", EvidenceRef: briefSource},
			{Finding: firstAskNonEmpty(blockedReason, "No safe automatic continuation was available."), EvidenceRef: firstAskNonEmpty(askBindingEvidence(out.WorkID, in), briefSource)},
		},
		Options: []reviewartifact.DecisionBriefOption{
			{Value: "allow", Label: "Allow this continuation", ConsequenceApprove: "The lane may continue this specific request.", ConsequenceHold: "The lane remains paused."},
			{Value: "hold", Label: "Keep the lane paused", ConsequenceApprove: "The envelope stays unchanged.", ConsequenceHold: "No continuation is sent."},
		},
		Recommendation: reviewartifact.DecisionBriefRecommendation{Option: "hold", Confidence: 0.8, Rationale: "Hold until the ask is covered by current law and exact delivery evidence."},
		PreparedBy:     "compound ask-triage", PreparedAt: now.UTC().Format(time.RFC3339), ProposeOnly: true,
	}
	briefBytes, err := json.Marshal(brief)
	if err != nil {
		return out, err
	}
	if parsed, warnings := reviewartifact.ParseDecisionBrief(briefBytes); parsed == nil || len(warnings) > 0 {
		return out, fmt.Errorf("ask-triage decision brief invalid: %v", warnings)
	}
	paths, err := reviewartifact.Write(homePath, AskTriageReviewArea, id, review, "  ", renderAskReviewMarkdown(review))
	if err != nil {
		return out, err
	}
	briefPath, err := reviewartifact.WriteJSON(homePath, AskTriageReviewArea, reviewartifact.DecisionBriefSidecarName(id), brief, "  ")
	if err != nil {
		return out, err
	}
	out.Disposition = disposition
	out.Handled = true
	out.ReviewID = id
	out.SideEffects = askHomeRelativeRefs(homePath, paths.PlanPath, paths.ReviewPath, briefPath)
	return out, nil
}

func renderAskReviewMarkdown(review AskTriageReview) string {
	return fmt.Sprintf("# Ask triage\n\n- Status: %s\n- Disposition: %s\n- Ask: %s\n- Reason: %s\n", review.Status, review.Disposition, review.Ask, review.BlockedReason)
}

func askStopEvidenceRefs(workID, jobRef string, in CodexStopInput, ask hook.BlockingAsk, claimID string) []string {
	refs := []string{jobRef, askBindingEvidence(workID, in), transcriptAskEvidence(in.TranscriptPath, ask.LineNo)}
	if strings.TrimSpace(claimID) != "" {
		refs = append(refs, "claim:"+strings.TrimSpace(claimID))
	}
	return askUniqueStrings(refs)
}

func askBindingEvidence(workID string, in CodexStopInput) string {
	parts := []string{"session:" + strings.TrimSpace(in.SessionID), "turn:" + strings.TrimSpace(in.TurnID)}
	if strings.TrimSpace(workID) != "" {
		parts = append([]string{"work:" + strings.TrimSpace(workID)}, parts...)
	}
	return strings.Join(parts, ":")
}

func transcriptAskEvidence(path string, line int) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if line > 0 {
		return fmt.Sprintf("transcript:%s#L%d", path, line)
	}
	return "transcript:" + path
}

func askHomeRelativeRefs(homePath string, paths ...string) []string {
	refs := make([]string, 0, len(paths))
	for _, path := range paths {
		rel, err := filepath.Rel(homePath, path)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			refs = append(refs, rel)
		}
	}
	return askUniqueStrings(refs)
}

func askUniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func firstAskNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
