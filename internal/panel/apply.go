package panel

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/runattempt"
	"compound/internal/work"
	"compound/internal/writeback"

	"gopkg.in/yaml.v3"
)

const panelApplyActor = "compound-panel"

var mergeClaimRefRE = regexp.MustCompile(`claim_[A-Za-z0-9_:-]+`)

// Apply is the only mutating panel step. It maps recorded panel verdicts through
// policy and memory adjudication, then closes a WorkEnvelope proof chain.
//
// The returns are named so the deferred terminalizer registered right after
// Mint can read the error an early return is failing with and record it as the
// RunAttempt's terminalization reason (see the failTerm-style guard below).
func Apply(homePath string, run Run, now func() time.Time) (result Run, err error) {
	if now == nil {
		now = time.Now
	}
	if run.Status != "verdicts_recorded" && run.Status != "applied" {
		return run, fmt.Errorf("panel apply requires verdicts_recorded or applied run, got %q", run.Status)
	}
	if run.WorkBound {
		if err := validatePanelWorkBinding(homePath, run); err != nil {
			return run, err
		}
		return run, fmt.Errorf("bound panel apply requires a delegated work operation lease; no mutation performed")
	}
	store, err := Open(homePath)
	if err != nil {
		return run, err
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		return run, err
	}
	writebackStore, err := writeback.Open(homePath)
	if err != nil {
		return run, err
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		return run, err
	}
	policyConfig, _, err := policy.LoadConfig(homePath)
	if err != nil {
		return run, err
	}
	if run.Status == "applied" {
		skippedRun, allSkipped, err := markAllSkippableApplications(homePath, run)
		if err != nil {
			return run, err
		}
		if allSkipped {
			skippedRun.CountsAfter, err = countPanelTargetStates(homePath, run.TargetKind, memStore)
			if err != nil {
				return run, err
			}
			if err := store.SaveRun(skippedRun); err != nil {
				return run, err
			}
			return skippedRun, nil
		}
		run = skippedRun
	}

	runRecordPath := store.runPath(run.ID)
	proofRefs := panelApplyProofRefs(run, runRecordPath)
	workEnvelope, err := workStore.Create(work.CreateInput{
		Owner:            "compound panel",
		Actor:            panelApplyActor,
		SuccessPredicate: fmt.Sprintf("panel apply run %s target_kind %s records governed adjudication results", run.ID, run.TargetKind),
		DesiredOutcome:   fmt.Sprintf("Apply recorded panel verdicts for %s", run.ID),
		NonGoals:         []string{"call live models", "mutate owner files outside COMPOUND_HOME"},
		EvidenceRefs:     proofRefs,
		Risk:             "tier1",
		Privacy:          "private",
	}, now().UTC())
	if err != nil {
		return run, err
	}
	run.WorkID = workEnvelope.ID
	run.Status = "running"
	if err := store.SaveRun(run); err != nil {
		return run, err
	}
	if _, err := workStore.Claim(run.WorkID, panelApplyActor, now().UTC()); err != nil {
		return run, err
	}
	// S1: the panel apply records itself as a RunAttempt instead of stuffing the prun_*
	// panel run id into WorkEnvelope.ActionIntentIDs. The RunAttempt id lands on the
	// dedicated slot so envelope.validateRunAttemptIDs can enforce the ratt_* shape and
	// ActionIntentIDs stays act_*-only. Mint-before-Advance keeps the audit trail intact
	// (the RunAttempt id is on the envelope before any artifact is written).
	runAttemptStore := runattempt.Open(homePath)
	ra, err := runAttemptStore.Mint(run.WorkID, panelApplyActor, runattempt.HarnessPlaceholder, now().UTC())
	if err != nil {
		return run, err
	}
	// Terminalize on EVERY return path from here on -- the failTerm-style
	// backstop (see subprocess_runner.go's failTerm). Before this guard, an
	// error return below stranded the minted attempt pending AND left the
	// WorkEnvelope stuck in running. The explicit paths below still own the
	// honest terminal states (succeeded on a clean apply, failed when the
	// fail-soft loop saw target failures); this defer covers only returns
	// that happen BEFORE those Complete/Close calls: it fails the attempt
	// with the returned error as the reason and fail-closes the envelope
	// (audit_only), mirroring the governed execution path's failed-run close
	// in cmd/compound/work_advance_execute.go. All best-effort: the
	// function's own error return remains the truth we surface.
	attemptTerminal := false
	envelopeOpen := false
	completePreparedRunAttempt := func(receiptID, transitionID string, at time.Time) error {
		// Work.Close prepares through its own Store instance. Open a fresh
		// RunAttempt view so this caller never mistakes its pre-close cache for
		// the durable post-close intent.
		freshStore := runattempt.Open(homePath)
		current, getErr := freshStore.Get(ra.ID)
		if getErr != nil {
			return getErr
		}
		if current.TerminalizationIntent == nil {
			return fmt.Errorf("panel run attempt %s has no prepared terminalization intent", ra.ID)
		}
		fullProofRefs := append([]string(nil), proofRefs...)
		fullProofRefs = append(fullProofRefs, receiptID, transitionID)
		if _, setErr := freshStore.SetProofRefs(ra.ID, fullProofRefs); setErr != nil {
			return setErr
		}
		if _, completeErr := freshStore.CompletePreparedTerminalization(ra.ID, *current.TerminalizationIntent, at); completeErr != nil {
			return completeErr
		}
		attemptTerminal = true
		return nil
	}
	defer func() {
		reason := "panel apply aborted before completing the run attempt"
		if err != nil {
			reason = "panel apply aborted: " + err.Error()
		}
		if envelopeOpen {
			summary := "panel apply aborted before work close"
			if err != nil {
				summary = "panel apply aborted: " + err.Error()
			}
			closeResult, closeErr := workStore.Close(run.WorkID, work.CloseInput{
				Outcome:       "failed",
				MoveType:      "audit_only",
				Actor:         panelApplyActor,
				Summary:       summary,
				Changed:       true,
				ProofRefs:     proofRefs,
				RunAttemptIDs: []string{ra.ID},
				// panel owns its RunAttempt lifecycle end-to-end: the deferred
				// guard attaches the close proof and completes the exact prepared
				// intent after publication.
				SkipRunAttemptTerminalization: true,
				RunAttemptTerminalizations: []work.RunAttemptTerminalizationRequest{{
					RunAttemptID: ra.ID,
					Owner:        runattempt.TerminalizationOwnerPanelApply,
					TargetStatus: runattempt.StatusFailed,
					Reason:       reason,
				}},
			}, now().UTC())
			if closeErr == nil {
				envelopeOpen = false
				run.ReceiptID = closeResult.Receipt.ID
				run.TransitionID = closeResult.Transition.ID
				_ = completePreparedRunAttempt(closeResult.Receipt.ID, closeResult.Transition.ID, now().UTC())
			}
		}
		if !attemptTerminal {
			current, getErr := runattempt.Open(homePath).Get(ra.ID)
			if getErr == nil && current.TerminalizationIntent != nil {
				_ = completePreparedRunAttempt(current.TerminalizationIntent.ReceiptID, current.TerminalizationIntent.TransitionID, now().UTC())
				return
			}
			_, _ = runAttemptStore.CompleteWithReason(ra.ID, runattempt.StatusFailed, reason, now().UTC())
		}
	}()
	// F2: stamp provenance immediately after Mint, before any other side
	// effect. panel.Apply IS the execution -- it performs the adjudication
	// mutations synchronously -- so the attempt is harness_execution, never a
	// claim reservation or a post-hoc observation. The stamp lets
	// supersede-at-attach (internal/work) retire the pending claim_reservation
	// minted by workStore.Claim above when this attempt attaches at the
	// running-advance below. A stamp failure is a genuine infra failure: it
	// returns here and the deferred guard terminalizes the freshly minted
	// attempt instead of stranding it pending.
	if stamped, oErr := runAttemptStore.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); oErr != nil {
		return run, fmt.Errorf("stamp run attempt origin: %w", oErr)
	} else {
		ra = stamped
	}
	advancedEnv, err := workStore.Advance(run.WorkID, work.AdvanceInput{To: "running", Actor: panelApplyActor, RunAttemptIDs: []string{ra.ID}, Note: "panel apply started"}, now().UTC())
	if err != nil {
		return run, err
	}
	envelopeOpen = true
	// Bind the RunAttempt to the ActionIntent Advance just created (the last act_ id on the
	// advanced envelope), closing the attempt→intent→decision audit chain.
	if n := len(advancedEnv.ActionIntentIDs); n > 0 {
		if err := runAttemptStore.AttachActionIntent(ra.ID, advancedEnv.ActionIntentIDs[n-1]); err != nil {
			return run, err
		}
	}
	// S2: wire the RunAttempt's full shape with honest values from the apply context.
	// Workspace and Command are stable across the attempt and can be set immediately.
	// Transcript and ProofRefs depend on artifacts produced by the apply loop, so they
	// land later (after SaveRun/Close) and before Complete. All setters refuse on a
	// terminal RunAttempt, which is why these land between Mint and Complete.
	if _, err := runAttemptStore.SetWorkspace(ra.ID, homePath); err != nil {
		return run, err
	}
	if _, err := runAttemptStore.SetCommand(ra.ID, "compound panel apply"); err != nil {
		return run, err
	}
	// Terminal state for the RunAttempt is decided after the apply loop: succeeded when
	// every target applied, failed when at least one failed (the work envelope still
	// terminalizes successfully because the apply loop is fail-soft by design).
	runAttemptStatus := runattempt.StatusSucceeded

	closeState := panelApplyCloseState{proofRefs: proofRefs}
	applyFailures := 0
	for i := range run.Findings {
		finding := &run.Findings[i]
		result, err := applyTargetFinding(homePath, run, finding, memStore, writebackStore, workStore, policyConfig, now)
		if err != nil {
			// Fail-soft: one broken target must not strand the run at "running"
			// or leave the WorkEnvelope dangling; the envelope terminalizes below.
			applyFailures++
			finding.Application.Applied = false
			if strings.TrimSpace(finding.Application.Note) == "" {
				finding.Application.Note = "apply failed: " + err.Error()
			}
			if saveErr := store.SaveRun(run); saveErr != nil {
				return run, saveErr
			}
			continue
		}
		closeState.add(result)
	}

	countsAfter, err := countPanelTargetStates(homePath, run.TargetKind, memStore)
	if err != nil {
		return run, err
	}
	run.CountsAfter = countsAfter
	run.Status = "applied"
	run.StoppedReason = ""
	if err := store.SaveRun(run); err != nil {
		return run, err
	}
	// S2: bind the panel run record as the RunAttempt's transcript. Lands before
	// Complete so the terminal status reflects the full audit trail.
	if _, err := runAttemptStore.SetTranscript(ra.ID, runRecordPath); err != nil {
		return run, err
	}
	if _, err := workStore.Advance(run.WorkID, work.AdvanceInput{To: "verifying", Actor: panelApplyActor, VerificationID: runRecordPath, Note: "panel apply run record written"}, now().UTC()); err != nil {
		return run, err
	}
	moveType := "audit_only"
	if closeState.changed {
		moveType = "memory_delta"
	}
	runAttemptReason := ""
	if applyFailures > 0 {
		runAttemptStatus = runattempt.StatusFailed
		runAttemptReason = fmt.Sprintf("panel apply completed with %d target failure(s)", applyFailures)
	}
	closeResult, err := workStore.Close(run.WorkID, work.CloseInput{
		Outcome:          "completed",
		MoveType:         moveType,
		Actor:            panelApplyActor,
		Summary:          panelApplySummary(run.ID, closeState),
		Changed:          closeState.changed,
		ProofRefs:        proofRefs,
		MemoryCandidates: closeState.memoryCandidates,
		MemoryPromoted:   closeState.promoted,
		MemoryParked:     closeState.parked,
		MemoryRejected:   closeState.rejected,
		// panel completes the RunAttempt itself after the close (binding the
		// close's receipt/transition as proof refs first), with failed on
		// target failures even though the envelope closes completed (fail-soft).
		// Close-time terminalization would freeze the attempt before those
		// refs land and would mislabel target failures as succeeded.
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations: []work.RunAttemptTerminalizationRequest{{
			RunAttemptID: ra.ID,
			Owner:        runattempt.TerminalizationOwnerPanelApply,
			TargetStatus: runAttemptStatus,
			Reason:       runAttemptReason,
		}},
	}, now().UTC())
	if err != nil {
		return run, err
	}
	envelopeOpen = false // the envelope terminalized on the explicit path; the deferred guard must not re-close it
	run.ReceiptID = closeResult.Receipt.ID
	run.TransitionID = closeResult.Transition.ID
	if err := store.SaveRun(run); err != nil {
		return run, err
	}
	// Bind the work proof chain before completing the exact prepared intent.
	// Generic completion is forbidden once Close has reserved caller-owned
	// terminalization evidence.
	if err := completePreparedRunAttempt(closeResult.Receipt.ID, closeResult.Transition.ID, now().UTC()); err != nil {
		return run, err
	}
	if applyFailures > 0 {
		return run, fmt.Errorf("panel apply completed with %d target failure(s); run %s remains re-applyable for the failed targets", applyFailures, run.ID)
	}
	return run, nil
}

type applyTargetResult struct {
	candidateID string
	promoted    bool
	parked      bool
	rejected    bool
	changed     bool
}

type panelApplyCloseState struct {
	proofRefs        []string
	memoryCandidates []string
	promoted         []string
	parked           []string
	rejected         []string
	changed          bool
}

func (s *panelApplyCloseState) add(result applyTargetResult) {
	if result.candidateID == "" {
		return
	}
	if result.changed {
		s.changed = true
		s.memoryCandidates = appendUniqueString(s.memoryCandidates, result.candidateID)
	}
	if result.promoted {
		s.promoted = appendUniqueString(s.promoted, result.candidateID)
	}
	if result.parked {
		s.parked = appendUniqueString(s.parked, result.candidateID)
	}
	if result.rejected {
		s.rejected = appendUniqueString(s.rejected, result.candidateID)
	}
}

func applyTargetFinding(homePath string, run Run, finding *TargetFinding, memStore *mem.Store, writebackStore *writeback.Store, workStore *work.Store, cfg policy.Config, now func() time.Time) (applyTargetResult, error) {
	result := applyTargetResult{candidateID: finding.TargetRef}
	app := finding.Application
	if app.Applied {
		app.Note = "already applied; skipped"
		finding.Application = app
		return result, nil
	}
	if strings.TrimSpace(finding.Synthesis.Decision) == "" {
		app.Applied = false
		app.Note = "no recorded synthesis; skipped"
		finding.Application = app
		return result, nil
	}

	if run.TargetKind != "memory_candidate" {
		return applyNonMemoryTargetFinding(homePath, run, finding, writebackStore, workStore, cfg, now)
	}

	cand, err := readMemoryCandidate(homePath, finding.TargetRef)
	if err != nil {
		return result, err
	}
	result.candidateID = cand.ID
	app.CandidateRef = cand.ID
	if terminalCandidateState(cand.State) {
		app.Applied = false
		app.MappedVerdict = normalizePanelPosition(finding.Synthesis.Decision)
		app.Note = "candidate terminal state " + cand.State + "; skipped"
		finding.Application = app
		return result, nil
	}

	createdAt := now().UTC()
	evidenceRefs := panelApplyEvidenceRefs(run.ID, finding.VerdictArtifact)
	ai := panelAdjudicateAction(run.WorkID, run.ID, cand, finding.Synthesis.Decision, candidateRecordPath(homePath, cand.ID), evidenceRefs, createdAt)
	pd := policy.Evaluate(ai, cfg, createdAt)
	app.ActionIntentRef = ai.ID
	app.PolicyDecisionRef = pd.ID
	app.MappedVerdict = normalizePanelPosition(finding.Synthesis.Decision)
	if !policy.Allowed(pd) {
		app.Applied = false
		app.Note = "policy " + pd.Outcome + ": " + strings.Join(pd.Reasons, "; ")
		finding.Application = app
		return result, nil
	}
	if _, err := workStore.RecordAction(run.WorkID, ai, pd, createdAt); err != nil {
		return result, err
	}

	mapped, applied, note, adjudicated, err := applyMemoryCandidateVerdict(run, finding, cand, memStore, writebackStore, now)
	if err != nil {
		return result, err
	}
	app.MappedVerdict = mapped
	app.Applied = applied
	app.Note = note
	if adjudicated.Candidate.ID != "" {
		app.CandidateRef = adjudicated.Candidate.ID
	}
	if adjudicated.Claim != nil {
		app.ClaimRef = adjudicated.Claim.ID
	}
	if adjudicated.Transition != nil {
		app.TransitionRef = adjudicated.Transition.ID
	}
	if applied {
		switch mapped {
		case "promote":
			result.promoted = adjudicated.Claim != nil
		case "park", "annotate":
			result.parked = true
		case "reject", "merge_existing":
			result.rejected = true
		}
		result.changed = result.promoted || result.parked || result.rejected
	}
	if app.NoPromoteRef == "" && (mapped == "park" || mapped == "merge_existing") {
		app.NoPromoteRef = noPromoteIDForCandidate(writebackStore, cand.ID, now)
	}
	finding.Application = app
	return result, nil
}

func applyNonMemoryTargetFinding(homePath string, run Run, finding *TargetFinding, writebackStore *writeback.Store, workStore *work.Store, cfg policy.Config, now func() time.Time) (applyTargetResult, error) {
	result := applyTargetResult{candidateID: finding.TargetRef}
	app := finding.Application
	target, err := LoadTargetInput(homePath, run.TargetKind, finding.TargetRef)
	if err != nil {
		return result, err
	}
	createdAt := now().UTC()
	evidenceRefs := panelDecisionEvidenceRefs(run.ID, finding.VerdictArtifact)
	ai := panelAdjudicateTargetAction(run.WorkID, run.ID, target, finding.Synthesis.Decision, panelTargetPath(homePath, run.TargetKind, finding.TargetRef), evidenceRefs, createdAt)
	pd := policy.Evaluate(ai, cfg, createdAt)
	app.ActionIntentRef = ai.ID
	app.PolicyDecisionRef = pd.ID
	app.MappedVerdict = normalizePanelPosition(finding.Synthesis.Decision)
	if !policy.Allowed(pd) {
		app.Applied = false
		app.Note = "policy " + pd.Outcome + ": " + strings.Join(pd.Reasons, "; ")
		finding.Application = app
		return result, nil
	}
	if _, err := workStore.RecordAction(run.WorkID, ai, pd, createdAt); err != nil {
		return result, err
	}

	switch run.TargetKind {
	case "skill_candidate":
		mapped, applied, note, candidateRef, changed, err := applySkillCandidateVerdict(homePath, run, finding, now)
		if err != nil {
			return result, err
		}
		app.MappedVerdict = mapped
		app.Applied = applied
		app.Note = note
		app.CandidateRef = candidateRef
		result.candidateID = firstNonEmptyString(candidateRef, finding.TargetRef)
		result.changed = changed
	case "capability_observation":
		mapped, applied, note, noPromoteRef, changed, err := applyCapabilityObservationVerdict(homePath, run, finding, writebackStore, now)
		if err != nil {
			return result, err
		}
		app.MappedVerdict = mapped
		app.Applied = applied
		app.Note = note
		app.CandidateRef = finding.TargetRef
		app.NoPromoteRef = noPromoteRef
		result.changed = changed
	case "comparison":
		mapped, applied, note, err := applyComparisonVerdict(homePath, run, finding, now)
		if err != nil {
			return result, err
		}
		app.MappedVerdict = mapped
		app.Applied = applied
		app.Note = note
		app.CandidateRef = finding.TargetRef
	case "memory_candidate":
		return result, fmt.Errorf("memory_candidate reached non-memory apply path")
	default:
		return result, fmt.Errorf("unsupported target kind %q", run.TargetKind)
	}
	finding.Application = app
	return result, nil
}

func applySkillCandidateVerdict(homePath string, run Run, finding *TargetFinding, now func() time.Time) (mapped string, applied bool, note string, candidateRef string, changed bool, err error) {
	reviewPath := panelTargetPath(homePath, "skill_candidate", finding.TargetRef)
	review, err := capgraph.LoadSkillCandidateReview(reviewPath)
	if err != nil {
		return "", false, "", "", false, err
	}
	candidateRef = review.CandidateID
	mapped = mapSkillCandidateDecision(finding.Synthesis.Decision)
	switch mapped {
	case "needs_human", "needs_trial":
		return mapped, false, "no mutation; verdict remains in review queue", candidateRef, false, nil
	}
	decision := capgraph.SkillCandidateDecision{
		SchemaVersion:    capgraph.SkillCandidateDecisionSchemaVersion,
		ID:               "decision_" + review.ID,
		ReviewID:         review.ID,
		Decision:         mapped,
		Rationale:        finding.Synthesis.Rationale,
		Confidence:       finding.Synthesis.Confidence,
		CandidateVerdict: candidates.Park,
		EvidenceRefs:     panelDecisionEvidenceRefs(run.ID, finding.VerdictArtifact),
		ReviewedAt:       reviewartifact.Timestamp(now().UTC()),
	}
	data, err := yaml.Marshal(decision)
	if err != nil {
		return mapped, false, "", candidateRef, false, err
	}
	yamlPath, err := reviewartifact.WriteRaw(homePath, "skill-candidates", finding.TargetRef+".review.yaml", data)
	if err != nil {
		return mapped, false, "", candidateRef, false, err
	}
	report, err := capgraph.RecordSkillCandidateDecision(homePath, decision, yamlPath, now().UTC())
	if err != nil {
		return mapped, false, "record skill candidate decision failed: " + err.Error(), candidateRef, false, nil
	}
	if report.CandidateID != "" {
		candidateRef = report.CandidateID
	}
	return mapped, true, "skill candidate decision recorded: " + report.ID, candidateRef, true, nil
}

func applyCapabilityObservationVerdict(homePath string, run Run, finding *TargetFinding, writebackStore *writeback.Store, now func() time.Time) (mapped string, applied bool, note string, noPromoteRef string, changed bool, err error) {
	var obs capabilityObservationTarget
	if err := candidates.ReadCandidate(homePath, candidates.CapabilityObs, finding.TargetRef, &obs); err != nil {
		return "", false, "", "", false, err
	}
	mapped = normalizePanelPosition(finding.Synthesis.Decision)
	switch mapped {
	case "promote":
		obs.Verdict = candidates.Operationalize
		if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, finding.TargetRef, obs); err != nil {
			return mapped, false, "", "", false, err
		}
		store, err := capgraph.Open(homePath)
		if err != nil {
			return mapped, false, "", "", false, err
		}
		appliedEdges, err := store.Consume(homePath, now().UTC())
		if err != nil {
			return mapped, false, "", "", false, err
		}
		return mapped, true, fmt.Sprintf("capability observation operationalized; consumed=%d", appliedEdges), "", true, nil
	case "park", "reject", "merge_existing":
		obs.Verdict = candidates.Park
		if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, finding.TargetRef, obs); err != nil {
			return mapped, false, "", "", false, err
		}
		recorded, _, err := writebackStore.RecordNoPromote(writeback.NoPromoteDecision{CandidateRef: finding.TargetRef, CandidateKind: string(candidates.CapabilityObs), Basis: "panel_verdict_park", EvidenceRefs: panelDecisionEvidenceRefs(run.ID, finding.VerdictArtifact)}, now().UTC())
		if err != nil {
			return mapped, false, "", "", false, err
		}
		return mapped, true, "capability observation parked; no-promote recorded", recorded.ID, true, nil
	case "needs_human", "needs_trial":
		return mapped, false, "no mutation; verdict remains in review queue", "", false, nil
	default:
		return mapped, false, "", "", false, fmt.Errorf("unsupported panel synthesis decision %q", finding.Synthesis.Decision)
	}
}

func mapSkillCandidateDecision(decision string) string {
	switch normalizePanelPosition(decision) {
	case "promote":
		return "create_candidate"
	case "merge_existing":
		return "merge_existing"
	case "reject":
		return "reject"
	case "park":
		return "keep_needs_review"
	case "needs_human":
		return "needs_human"
	case "needs_trial":
		return "needs_trial"
	default:
		return normalizePanelPosition(decision)
	}
}

func applyMemoryCandidateVerdict(run Run, finding *TargetFinding, cand mem.Candidate, memStore *mem.Store, writebackStore *writeback.Store, now func() time.Time) (string, bool, string, mem.AdjudicateResult, error) {
	decision := normalizePanelPosition(finding.Synthesis.Decision)
	evidenceRefs := panelApplyEvidenceRefs(run.ID, finding.VerdictArtifact)
	switch decision {
	case "promote":
		if !candidates.ValidBasis(cand.PromotionBasis) {
			res, err := memStore.Adjudicate(cand.ID, "park", run.WorkID, now().UTC())
			if err != nil {
				return "park", false, "", res, err
			}
			nop, _, err := writebackStore.RecordNoPromote(writeback.NoPromoteDecision{CandidateRef: cand.ID, CandidateKind: string(candidates.Memory), Basis: "panel_verdict_without_promotion_basis", EvidenceRefs: evidenceRefs}, now().UTC())
			if err != nil {
				return "park", false, "", res, err
			}
			finding.Application.NoPromoteRef = nop.ID
			return "park", true, "downgraded promote to park: panel_verdict_without_promotion_basis", res, nil
		}
		res, err := memStore.Adjudicate(cand.ID, "promote", run.WorkID, now().UTC())
		if err == nil && res.Verdict == "promote" && res.Claim != nil {
			return "promote", true, res.Note, res, nil
		}
		note := "promotion rejected by memory adjudicator"
		if err != nil {
			note += ": " + err.Error()
		} else if res.Note != "" {
			note += ": " + res.Note
		}
		parked, parkErr := memStore.Adjudicate(cand.ID, "park", run.WorkID, now().UTC())
		if parkErr != nil {
			return "park", false, note, res, parkErr
		}
		return "park", true, note + "; fell through to park", parked, nil
	case "park":
		res, err := memStore.Adjudicate(cand.ID, "park", run.WorkID, now().UTC())
		if err != nil {
			return "park", false, "", res, err
		}
		nop, _, err := writebackStore.RecordNoPromote(writeback.NoPromoteDecision{CandidateRef: cand.ID, CandidateKind: string(candidates.Memory), Basis: "panel_verdict_park", EvidenceRefs: evidenceRefs}, now().UTC())
		if err != nil {
			return "park", false, "", res, err
		}
		finding.Application.NoPromoteRef = nop.ID
		return "park", true, res.Note, res, nil
	case "reject":
		res, err := memStore.Adjudicate(cand.ID, "reject", run.WorkID, now().UTC())
		return "reject", err == nil, res.Note, res, err
	case "merge_existing":
		res, err := memStore.Adjudicate(cand.ID, "reject", run.WorkID, now().UTC())
		if err != nil {
			return "merge_existing", false, "", res, err
		}
		nop := writeback.NoPromoteDecision{CandidateRef: cand.ID, CandidateKind: string(candidates.Memory), Basis: "panel_verdict_merge_existing", EvidenceRefs: evidenceRefs}
		if ref := mergeTargetClaimRef(finding.Synthesis); ref != "" {
			nop.OwnerTarget = writeback.OwnerTarget{Kind: "memory_claim", Ref: ref}
		}
		recorded, _, err := writebackStore.RecordNoPromote(nop, now().UTC())
		if err != nil {
			return "merge_existing", false, "", res, err
		}
		finding.Application.NoPromoteRef = recorded.ID
		return "merge_existing", true, res.Note, res, nil
	case "needs_human", "needs_trial":
		if knowledgeClassCandidate(cand) {
			appliedAt := now().UTC()
			annotated, err := memStore.AnnotateCandidate(cand.ID, knowledgePanelAnnotation(run, finding, decision, evidenceRefs, appliedAt), appliedAt)
			res := mem.AdjudicateResult{Candidate: annotated, Verdict: "park", Note: "knowledge-class panel verdict annotated and parked"}
			if err != nil {
				return "annotate", false, "", res, err
			}
			return "annotate", true, res.Note, res, nil
		}
		return decision, false, "no mutation; verdict remains in review queue", mem.AdjudicateResult{Candidate: cand, Verdict: decision}, nil
	default:
		return decision, false, "", mem.AdjudicateResult{Candidate: cand, Verdict: decision}, fmt.Errorf("unsupported panel synthesis decision %q", finding.Synthesis.Decision)
	}
}

func knowledgeClassCandidate(cand mem.Candidate) bool {
	if strings.HasPrefix(strings.TrimSpace(cand.Entity), "knowledge:") {
		return true
	}
	return strings.TrimSpace(cand.Qualifiers["source"]) == "knowledge_extraction"
}

func knowledgePanelAnnotation(run Run, finding *TargetFinding, originalDecision string, evidenceRefs []string, now time.Time) mem.CandidateAnnotation {
	return mem.CandidateAnnotation{
		ID:               reviewartifact.ID("ann_", run.ID, finding.TargetRef, originalDecision, finding.VerdictArtifact),
		Kind:             "panel_verdict",
		Decision:         "annotate",
		OriginalDecision: originalDecision,
		RunID:            run.ID,
		VerdictArtifact:  finding.VerdictArtifact,
		Rationale:        finding.Synthesis.Rationale,
		Confidence:       finding.Synthesis.Confidence,
		EvidenceRefs:     reviewartifact.Unique(evidenceRefs),
		LensFindings:     knowledgeAnnotationLensFindings(finding.LensFindings),
		CreatedAt:        reviewartifact.Timestamp(now),
	}
}

func knowledgeAnnotationLensFindings(findings []LensFinding) []mem.CandidateAnnotationLens {
	out := make([]mem.CandidateAnnotationLens, 0, len(findings))
	for _, finding := range findings {
		out = append(out, mem.CandidateAnnotationLens{
			LensRef:          finding.LensRef,
			Status:           finding.Status,
			Confidence:       finding.Confidence,
			RouteEvidenceRef: finding.RouteEvidenceRef,
			Findings:         knowledgeAnnotationFindings(finding.Findings),
		})
	}
	return out
}

func knowledgeAnnotationFindings(findings []Finding) []mem.CandidateAnnotationFinding {
	out := make([]mem.CandidateAnnotationFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, mem.CandidateAnnotationFinding{
			Claim:        finding.Claim,
			Severity:     finding.Severity,
			EvidenceRefs: append([]string(nil), finding.EvidenceRefs...),
		})
	}
	return out
}

func panelAdjudicateAction(workID, runID string, cand mem.Candidate, verdict, targetPath string, evidenceRefs []string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            reviewartifact.ID("act_", workID, runID, cand.ID, verdict, now.Format(time.RFC3339Nano)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "package", Name: "compound/internal/panel"},
			IDs:           envelope.IDs{WorkID: workID, RunID: runID},
			Authority:     envelope.Authority{SourceClass: "runtime", Owner: "compound panel", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  evidenceRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         panelApplyActor,
		Capability:    "panel.adjudicate",
		Action:        "adjudicate_memory_candidate",
		Target:        envelope.ActionTarget{Kind: "memory_candidate", ID: cand.ID, Path: targetPath},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("map panel verdict %q for memory candidate %s", verdict, cand.ID),
		RollbackPlan:  "restore candidate JSON state and remove appended ledger/event rows for this work_id before depending on them",
	}
}

func readMemoryCandidate(homePath, id string) (mem.Candidate, error) {
	var cand mem.Candidate
	if err := candidates.ReadCandidate(homePath, candidates.Memory, id, &cand); err != nil {
		return mem.Candidate{}, err
	}
	return cand, nil
}

func candidateRecordPath(homePath, id string) string {
	return filepath.Join(homePath, candidates.InboxDir(candidates.Memory), id+".json")
}

func countPanelTargetStates(homePath, targetKind string, memStore *mem.Store) (map[string]int, error) {
	switch targetKind {
	case "memory_candidate":
		return countMemoryCandidateStates(memStore)
	case "skill_candidate":
		inputs, err := LoadSkillCandidateTargets(homePath, 0)
		if err != nil {
			return nil, err
		}
		return countTargetInputStates(inputs), nil
	case "capability_observation":
		inputs, err := LoadCapabilityObservationTargets(homePath, 0)
		if err != nil {
			return nil, err
		}
		return countTargetInputStates(inputs), nil
	case "comparison":
		inputs, err := LoadComparisonTargets(homePath, 0)
		if err != nil {
			return nil, err
		}
		return countTargetInputStates(inputs), nil
	default:
		return nil, fmt.Errorf("unsupported target kind %q", targetKind)
	}
}

func countTargetInputStates(inputs []TargetInput) map[string]int {
	counts := map[string]int{}
	for _, input := range inputs {
		counts[targetState(input.Payload)]++
	}
	return counts
}

func countMemoryCandidateStates(store *mem.Store) (map[string]int, error) {
	cands, err := store.AllCandidates()
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, cand := range cands {
		state := strings.TrimSpace(cand.State)
		if state == "" {
			state = "unknown"
		}
		counts[state]++
	}
	return counts, nil
}

func terminalCandidateState(state string) bool {
	switch state {
	case "promoted", "rejected", "no_promote":
		return true
	default:
		return false
	}
}

func panelDecisionEvidenceRefs(runID, verdictArtifact string) []string {
	refs := []string{"panel_run:" + runID}
	if strings.TrimSpace(verdictArtifact) != "" {
		refs = append(refs, "panel_verdict:"+verdictArtifact)
	}
	return reviewartifact.Unique(refs)
}

func panelTargetPath(homePath, targetKind, ref string) string {
	switch targetKind {
	case "memory_candidate":
		return candidateRecordPath(homePath, ref)
	case "skill_candidate":
		return filepath.Join(homePath, "reviews", "skill-candidates", ref+".json")
	case "capability_observation":
		return filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs), ref+".json")
	case "comparison":
		return comparisonCandidatePath(homePath, ref)
	default:
		return ""
	}
}

func panelAdjudicateTargetAction(workID, runID string, target TargetInput, verdict, targetPath string, evidenceRefs []string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            reviewartifact.ID("act_", workID, runID, target.Kind, target.Ref, verdict, now.Format(time.RFC3339Nano)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "package", Name: "compound/internal/panel"},
			IDs:           envelope.IDs{WorkID: workID, RunID: runID},
			Authority:     envelope.Authority{SourceClass: "runtime", Owner: "compound panel", Status: "current"},
			EvidenceRefs:  evidenceRefs,
			Privacy:       "public",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         panelApplyActor,
		Capability:    "panel.adjudicate",
		Action:        "adjudicate_" + target.Kind,
		Target:        envelope.ActionTarget{Kind: target.Kind, ID: target.Ref, Path: targetPath},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("map panel verdict %q %s %s", verdict, target.Kind, target.Ref),
		RollbackPlan:  "restore target JSON/YAML sidecar and remove appended ledger/event rows for this work_id before depending on them",
	}
}

func panelApplyEvidenceRefs(runID, verdictArtifact string) []string {
	refs := []string{runID}
	if strings.TrimSpace(verdictArtifact) != "" {
		refs = append(refs, verdictArtifact)
	}
	return reviewartifact.Unique(refs)
}

func panelApplyProofRefs(run Run, runRecordPath string) []string {
	refs := []string{runRecordPath}
	for _, finding := range run.Findings {
		if strings.TrimSpace(finding.VerdictArtifact) != "" {
			refs = append(refs, finding.VerdictArtifact)
		}
		for _, lens := range finding.LensFindings {
			if strings.TrimSpace(lens.RouteEvidenceRef) != "" {
				refs = append(refs, lens.RouteEvidenceRef)
			}
		}
		if strings.TrimSpace(finding.Synthesis.JudgeRouteEvidenceRef) != "" {
			refs = append(refs, finding.Synthesis.JudgeRouteEvidenceRef)
		}
	}
	return reviewartifact.Unique(refs)
}

func panelApplySummary(runID string, state panelApplyCloseState) string {
	if !state.changed {
		return "Panel apply " + runID + " completed with no memory candidate mutations"
	}
	return fmt.Sprintf("Panel apply %s promoted=%d parked=%d rejected=%d", runID, len(state.promoted), len(state.parked), len(state.rejected))
}

func noPromoteIDForCandidate(store *writeback.Store, candidateID string, now func() time.Time) string {
	decision, ok, err := store.ActiveNoPromote(candidateID, now().UTC())
	if err != nil || !ok {
		return ""
	}
	return decision.ID
}

func mergeTargetClaimRef(s Synthesis) string {
	for _, value := range []string{s.Rationale} {
		if ref := mergeClaimRefRE.FindString(value); ref != "" {
			return ref
		}
	}
	for _, dissent := range s.Dissent {
		if ref := mergeClaimRefRE.FindString(dissent.Note); ref != "" {
			return ref
		}
	}
	return ""
}

func markAllSkippableNonMemoryApplications(run Run) (Run, bool) {
	allSkipped := len(run.Findings) > 0
	for i := range run.Findings {
		finding := &run.Findings[i]
		app := finding.Application
		mapped := normalizePanelPosition(finding.Synthesis.Decision)
		if run.TargetKind == "skill_candidate" {
			mapped = mapSkillCandidateDecision(finding.Synthesis.Decision)
		}
		if run.TargetKind == "comparison" {
			mapped = mapComparisonDecision(finding.Synthesis.Decision)
		}
		app.MappedVerdict = mapped
		switch {
		case app.Applied:
			app.Note = "already applied; skipped"
		case mapped == "needs_human" || mapped == "needs_trial":
			app.Note = "no mutation; verdict remains in review queue"
		case strings.TrimSpace(finding.Synthesis.Decision) == "":
			app.Note = "no recorded synthesis; skipped"
		default:
			// A recorded mutating verdict whose application never applied
			// (e.g. a prior apply failure) is retryable; the run must not
			// stay terminal or the target strands in awaiting_verdict.
			allSkipped = false
		}
		finding.Application = app
	}
	return run, allSkipped
}

func markAllSkippableApplications(homePath string, run Run) (Run, bool, error) {
	if run.TargetKind != "memory_candidate" {
		marked, allSkipped := markAllSkippableNonMemoryApplications(run)
		return marked, allSkipped, nil
	}
	allSkipped := len(run.Findings) > 0
	for i := range run.Findings {
		finding := &run.Findings[i]
		app := finding.Application
		switch {
		case app.Applied:
			app.Note = "already applied; skipped"
		case normalizePanelPosition(finding.Synthesis.Decision) == "needs_human" || normalizePanelPosition(finding.Synthesis.Decision) == "needs_trial":
			app.MappedVerdict = normalizePanelPosition(finding.Synthesis.Decision)
			app.Note = "no mutation; verdict remains in review queue"
		case finding.TargetRef != "":
			cand, err := readMemoryCandidate(homePath, finding.TargetRef)
			if err != nil {
				return run, false, err
			}
			if terminalCandidateState(cand.State) {
				app.CandidateRef = cand.ID
				app.MappedVerdict = normalizePanelPosition(finding.Synthesis.Decision)
				app.Note = "candidate terminal state " + cand.State + "; skipped"
			} else {
				allSkipped = false
			}
		default:
			allSkipped = false
		}
		finding.Application = app
	}
	return run, allSkipped, nil
}
