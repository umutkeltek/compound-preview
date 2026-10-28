package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// executeStageMaxPickAttempts bounds how many distinct ready envelopes ONE
// stage call may examine when earlier picks keep refusing (ineligibility or a
// recognized preflight refusal). The launch budget is untouched and stays
// exactly one per call (executeStageMaxLaunchesPerRun in continue.go); this
// bound only stops a bench of permanently refused heads from burning the
// whole tick. Unexamined candidates stay ready for the next tick.
const executeStageMaxPickAttempts = 5

// executeStageMaxRouteCandidates bounds route/capacity admissions in one
// tick. It is independent of the one-launch ceiling and keeps a malformed or
// very large route authority from turning the heartbeat into an unbounded
// polling pass.
const executeStageMaxRouteCandidates = 5

// ExecuteRefusal records one ready envelope the stage declined to even try,
// in pick (workbench) order. Reason is "<family>:<detail>" — either
// "ineligible:<why>" (decided before the gates; see unattendedEligible) or a
// recognized preflight refusal family ("workspace_unresolvable:<detail>",
// "lane_unready:<detail>", "adapter_missing:<detail>"). The envelope behind
// a refusal is never mutated by the stage.
type ExecuteRefusal struct {
	WorkID string `json:"work_id"`
	Reason string `json:"reason"`
}

// executeStage is the grant-gated, fail-closed seam that drives ONE ready
// envelope through claim -> running -> verifying. It is called from Continue
// ONLY when the existing lifecycle branches return status="waiting",
// action="none" AND there is at least one ready envelope on the bench. Each
// gate below is a separate refusal reason — see the named constants
// (skipNoGrant, skipRisk, skipNoRoute, skipCapacityUnknown, skipPolicyHold,
// skipLaunchFailed) for the wire values.
//
// The seam never lands, closes, pushes or retires anything: land/close stay
// human/orchestrator-only. Failure isolation is per-envelope: one bad launch
// marks that envelope failed and returns; the next Continue picks the next
// envelope.
//
// R6E (2026-09-10): the stage no longer strands the tick on ONE refused
// head. Candidates are taken in workbench order; an INELIGIBLE candidate
// (another executor's work — see unattendedEligible, decided BEFORE gates
// (b)-(e)) or a candidate whose preflight refuses with a recognized reason
// is recorded in the refused list (no envelope mutation) and the NEXT ready
// item is tried, up to executeStageMaxPickAttempts examinations. The launch
// budget stays exactly one per call. A non-refusal launch error still closes
// THAT envelope failed and ends the stage, exactly as before.
//
// The five gates in order:
//
//	a. standing grant grant_unattended_work_continue_v1 present in registry
//	b. envelope risk ∈ {tier0, tier1} AND privacy == private
//	c. route plan names an eligible harness lane for the capability
//	d. capacity snapshot fresher than the freshness window AND usedPercent
//	   strictly less than the ceiling
//	e. policy evaluation of work.continue.unattended returns allow or
//	   allow_with_obligations; hold/deny -> skipped=policy_hold
//
// (c) and (d) are per-lane, not per-candidate, so they run once before the
// candidate loop; eligibility needs the resolved lane's harness family.
func (s *Store) executeStage(now time.Time, opts ContinueOptions, actor string) (result ContinueResult, err error) {
	result = ContinueResult{
		Status:  "waiting",
		Action:  "none",
		Actor:   actor,
		WorkID:  "",
		Skipped: "",
		Human:   "",
	}
	// Every refusal recorded below is stamped onto the result at exit so
	// the Human line and the Refused list can never disagree.
	var refused []ExecuteRefusal
	defer func() {
		if len(refused) > 0 {
			result.Refused = append([]ExecuteRefusal(nil), refused...)
		}
	}()
	// (a) standing grant present?
	hasGrant, grantErr := s.executeStageHasGrant(s.home)
	if grantErr != nil {
		result.Human = "did nothing: could not read standing-grants registry: " + grantErr.Error()
		result.Skipped = "no_grant"
		return result, nil
	}
	if !hasGrant {
		result.Skipped = "no_grant"
		result.Human = fmt.Sprintf("did nothing: no grant named %s; import one to allow unattended execution", executeUnattendedGrantID)
		return result, nil
	}

	// (c,d) resolve the route authority's ordered candidates, then admit
	// fresh capacity for each one. A stale, unknown, or exhausted lane is a
	// refusal for that lane only; it must not strand a later eligible lane.
	routeCandidates := executeStageRouteCandidatesFn(s.home, executeUnattendedCapability)
	if len(routeCandidates) == 0 {
		result.Skipped = "no_route"
		result.Human = fmt.Sprintf("did nothing: route plan has no eligible lane for %s", executeUnattendedCapability)
		return result, nil
	}

	// Candidates in workbench order. We do NOT pre-filter to specific
	// IDs — the seam walks ready items by Workbench rank so the existing
	// lifecycle ordering is preserved. A pinned ExpectWorkID narrows the
	// list to that id, preserving the pin the pre-R6E stage had.
	candidates := s.findReadyForExecuteIDs(now, actor, opts.ExpectWorkID)
	if len(candidates) == 0 {
		// No ready envelope on the bench — this is the existing "no safe
		// lifecycle step is ready" path, not an execute refusal. Return
		// without setting Skipped so callers can tell "no work to do"
		// apart from "gates refused".
		result.Human = "did nothing: no ready envelope to execute"
		return result, nil
	}

	type usableLane struct {
		candidate ExecutionRouteCandidate
		used      float64
	}
	usable := make([]usableLane, 0, len(routeCandidates))
	capacityRefusals := make([]string, 0, len(routeCandidates))
	seenLanes := make(map[string]bool, len(routeCandidates))
	routeChecks := 0
	for _, candidate := range routeCandidates {
		candidate.LaunchProfileID = strings.TrimSpace(candidate.LaunchProfileID)
		candidate.Harness = strings.TrimSpace(candidate.Harness)
		candidate.CapacityLane = strings.TrimSpace(candidate.CapacityLane)
		if candidate.CapacityLane == "" {
			candidate.CapacityLane = candidate.Harness
		}
		key := normalizeRouteLane(candidate.LaunchProfileID)
		if candidate.LaunchProfileID == "" || candidate.Harness == "" || seenLanes[key] {
			continue
		}
		if routeChecks >= executeStageMaxRouteCandidates {
			break
		}
		routeChecks++
		seenLanes[key] = true
		usedPercent, fresh, capErr := executeStageCapacityFn(s.home, candidate.CapacityLane, now)
		if capErr != nil {
			capacityRefusals = append(capacityRefusals, fmt.Sprintf("%s: capacity read failed: %v", candidate.LaunchProfileID, capErr))
			continue
		}
		if !fresh {
			capacityRefusals = append(capacityRefusals, fmt.Sprintf("%s: capacity snapshot missing or older than %s", candidate.LaunchProfileID, executeStageCapacityFreshnessWindow))
			continue
		}
		if usedPercent >= executeStageCapacityCeilingPercent {
			capacityRefusals = append(capacityRefusals, fmt.Sprintf("%s: capacity %.0f%% (ceiling %.0f%%)", candidate.LaunchProfileID, usedPercent, executeStageCapacityCeilingPercent))
			continue
		}
		usable = append(usable, usableLane{candidate: candidate, used: usedPercent})
	}
	if len(capacityRefusals) > 0 {
		defer func() { result.Human = "route refusals: " + strings.Join(capacityRefusals, "; ") + "; " + result.Human }()
	}
	if len(usable) == 0 {
		result.Skipped = "capacity_unknown"
		result.Human = "did nothing: no route lane has fresh capacity below the ceiling"
		return result, nil
	}

	workChecks := 0
	for _, selectedLane := range usable {
		candidate, usedPercent := selectedLane.candidate, selectedLane.used
		lane := candidate.Harness
		result.Lane = candidate.LaunchProfileID
		attempts := 0
		for _, ready := range candidates {
			if attempts >= executeStageMaxPickAttempts {
				break
			}
			attempts++
			workChecks++
			result.WorkID = ready

			w, loadErr := s.Get(ready)
			if loadErr != nil {
				return result, fmt.Errorf("load ready envelope %s: %w", ready, loadErr)
			}

			// R6E eligibility, decided BEFORE the gates (b)-(e): work that
			// belongs to another executor (foreign origin harness, foreign
			// owner) or carries an unresolvable workspace is skipped with
			// reason ineligible:<why> and never mutated.
			// Saved profiles may name a tuple alias rather than its model-entry
			// key. Carry that exact profile only when its stored tuple matches;
			// production preflight must still resolve it through the owner before
			// any claim. Reset from the route for every work item.
			candidate = executionCandidateForSelection(selectedLane.candidate, w.ExecutionSelection)
			result.Lane = candidate.LaunchProfileID
			if eligible, why := unattendedEligibleCandidate(w, candidate); !eligible {
				refused = append(refused, ExecuteRefusal{WorkID: ready, Reason: "ineligible:" + why})
				continue
			}
			candidate.SelectionRevision = 0
			if w.ExecutionSelection != nil {
				candidate.SelectionRevision = w.ExecutionSelection.Revision
			}

			// (b) risk/privacy gate.
			if !executeRiskAllowed(w.Risk) || !executePrivacyAllowed(w.Privacy) {
				result.Skipped = "risk"
				result.Human = executeRefusedSummary(refused) + fmt.Sprintf("did nothing: envelope %s risk=%s privacy=%s is outside the unattended envelope (tier0/tier1, private)", ready, w.Risk, w.Privacy)
				return result, nil
			}

			// (e) policy evaluation of work.continue.unattended.
			pd := executeStagePolicyEvalFn(
				policyActionIntentShim{
					ID:         "ai_unattended_" + ts(now),
					Capability: executeUnattendedCapability,
					WorkID:     ready,
					Risk:       w.Risk,
					Privacy:    w.Privacy,
				},
				policyConfigShim{HasGrant: true, Home: s.home},
				now,
			)
			result.PolicyDecisionID = pd.ID
			switch pd.Outcome {
			case "allow", "allow_with_obligations":
				// proceed to launch
			default:
				result.Skipped = "policy_hold"
				result.Human = executeRefusedSummary(refused) + fmt.Sprintf("did nothing: policy decision %s for %s is %s", pd.ID, executeUnattendedCapability, pd.Outcome)
				return result, nil
			}

			// DEFECT 1 fix (AUT-P0-001 2026-09-04): preflight MUST run BEFORE the
			// envelope is claimed or advanced. Production wires this hook to
			// executewire.PreflightViaActuator, which resolves adapter enrollment,
			// workspace resolvability, and lane readiness — every prerequisite that
			// can fail before any state mutation. A recognized refusal leaves the
			// envelope, its lease, the event log and the RunAttempt set byte-
			// identical to before, so the next Continue tick can retry safely.
			// R6E: a recognized refusal no longer strands the tick — it is
			// recorded and the NEXT ready item is tried (bounded above).
			var preflightErr error
			if candidate.SourceRevision != "" && executeStageCandidatePreflightFn == nil {
				preflightErr = errors.New("lane_unready:owner tuple preflight is not installed")
			} else if executeStageCandidatePreflightFn != nil {
				preflightErr = executeStageCandidatePreflightFn(s, ready, candidate, now)
			} else {
				preflightErr = executeStagePreflightFn(s, ready, lane, now)
			}
			if preflightErr != nil {
				skipReason, refuseDetail := parseLaunchRefusal(preflightErr)
				if skipReason != "" {
					refused = append(refused, ExecuteRefusal{WorkID: ready, Reason: skipReason + ":" + refuseDetail})
					continue
				}
				// Bare preflight error (infra failure inside the preflight itself):
				// also fail closed without mutation. The seam never claims or
				// advances the envelope under an unrecognized preflight error —
				// that would be exactly the bug the fix prevents.
				result.Skipped = "preflight_failed"
				result.Human = executeRefusedSummary(refused) + fmt.Sprintf("did nothing: preflight failed: %v", preflightErr)
				result.Action = "none"
				result.RunAttemptIDs = nil
				return result, nil
			}

			// Dry-run probe: every gate opened, but the operator asked us NOT to
			// claim, advance, launch, or finalize. Return a verdict that names
			// the work, the lane, the capacity reading, and the policy decision
			// id so the operator can verify the unattended envelope end-to-end
			// without minting a RunAttempt. The envelope stays in its current
			// state (ready) for the next live tick to act on.
			if opts.DryRun {
				result.Skipped = "would_execute"
				result.executionChoice = &candidate
				result.Human = executeRefusedSummary(refused) + fmt.Sprintf("would execute %s on %s (capacity %.0f%%, policy %s)", ready, lane, usedPercent, pd.ID)
				result.PolicyDecisionID = pd.ID
				result.Lane = candidate.LaunchProfileID
				result.WorkID = ready
				return result, nil
			}

			// All gates pass — claim + advance to running + launch + advance to
			// verifying, reusing the existing lifecycle seams (`Claim`, `Advance`,
			// `Close`) rather than forking them. The launch stops at verifying;
			// the envelope is NEVER closed by the seam on a successful launch.
			workerPrompt := strings.TrimSpace(opts.Summary)
			if workerPrompt == "" {
				workerPrompt = "unattended execute from work continue: " + ready
			}
			var sourceAction string
			if n := len(w.ActionIntentIDs); n > 0 {
				sourceAction = w.ActionIntentIDs[n-1]
			}
			if _, claimErr := s.claimExpected(ready, actor, w.State, w.PolicyDecisionID, sourceAction, now); claimErr != nil {
				// Claim can fail (e.g. policy hold). Treat that as a stage-level
				// refusal; the envelope stays in its current state so the next
				// Continue can retry.
				result.Skipped = "claim_failed"
				if errors.Is(claimErr, ErrExecutionReservationChanged) {
					result.Skipped = "target_changed"
				}
				result.Human = executeRefusedSummary(refused) + fmt.Sprintf("did nothing: claim of %s refused: %v", ready, claimErr)
				result.NoAction = result.Human
				return result, nil
			}
			// R6P: detached mode hands the CLAIMED envelope to a separate governed
			// door process and returns; the door owns running -> verifying/failed
			// (see execute_detached.go). A spawn failure fails only this envelope.
			if executeStageDetachedCandidateLaunchFn != nil || executeStageDetachedLaunchFn != nil {
				var launch DetachedLaunch
				var spawnErr error
				if executeStageDetachedCandidateLaunchFn != nil {
					launch, spawnErr = executeStageDetachedCandidateLaunchFn(s, ready, candidate, workerPrompt, actor, now)
				} else {
					launch, spawnErr = executeStageDetachedLaunchFn(s, ready, lane, workerPrompt, actor, now)
				}
				if spawnErr != nil {
					// 2026-09-15 start/record contract: a door that STARTED is
					// never a failed-to-spawn result. The typed record error
					// carries the started identity; the door owns the envelope
					// and finalizes it, so the envelope stays claimed;
					// reconciliation adopts matching evidence or retains uncertainty.
					var recErr *DetachedLaunchRecordError
					if errors.As(spawnErr, &recErr) && recErr != nil {
						result.Action = "execute"
						result.Status = "claimed"
						result.Lane = candidate.LaunchProfileID
						result.Detached = &recErr.Launch
						result.Skipped = "launch_record_recovery"
						result.RunAttemptIDs = nil
						result.Human = executeRefusedSummary(refused) + fmt.Sprintf("launched envelope %s on lane %s in a detached governed door (pid %d) but start evidence could not be durably recorded: %v; door retains ownership; reconciliation adopts matching identity when available or reports recovery_required; log %s", ready, lane, recErr.Launch.PID, recErr.Err, recErr.Launch.LogPath)
						return result, nil
					}
					closeSummary := "unattended detached launch failed to spawn: " + spawnErr.Error()
					if closeErr := s.failClaimedUnattendedEnvelope(ready, actor, closeSummary, "execute_stage_detached_spawn_failed", now); closeErr != nil {
						result.Human = executeRefusedSummary(refused) + fmt.Sprintf("claimed %s on lane %s but the detached door failed to spawn: %v; recording the failure was refused too: %v", ready, lane, spawnErr, closeErr)
					} else {
						result.Human = executeRefusedSummary(refused) + fmt.Sprintf("claimed %s on lane %s but the detached door failed to spawn: %v; envelope marked failed, heartbeat continues", ready, lane, spawnErr)
					}
					result.Action = "execute"
					result.Skipped = "launch_failed"
					result.RunAttemptIDs = nil
					return result, nil
				}
				result.Action = "execute"
				result.Status = "claimed"
				result.Lane = candidate.LaunchProfileID
				result.Detached = &launch
				result.RunAttemptIDs = nil
				result.Human = executeRefusedSummary(refused) + fmt.Sprintf("launched envelope %s on lane %s in a detached governed door (pid %d, budget %ds, log %s); the door owns running -> verifying/failed; later ticks reconcile it", ready, lane, launch.PID, launch.BudgetSeconds, launch.LogPath)
				return result, nil
			}
			if _, advErr := s.Advance(ready, AdvanceInput{To: "running", Actor: actor, Note: "execute stage: running"}, now); advErr != nil {
				result.Skipped = "claim_failed"
				result.Human = executeRefusedSummary(refused) + fmt.Sprintf("did nothing: advance to running of %s refused: %v", ready, advErr)
				result.NoAction = result.Human
				return result, nil
			}

			var rattID string
			var launchErr error
			if executeStageCandidateLaunchFn != nil {
				rattID, launchErr = executeStageCandidateLaunchFn(s, ready, candidate, workerPrompt, actor, now)
			} else {
				rattID, launchErr = launchStub(s, ready, lane, lane, workerPrompt, actor, now)
			}
			if launchErr != nil {
				// The launch wire can return a refused execution WITHOUT an
				// adapter having been invoked: e.g. the readiness guard reports
				// not-ready, or the workspace is unresolvable. Those refusals
				// must NOT close the envelope "failed" — the launch never
				// happened, the envelope is still safe to retry next tick. The
				// wire signals refusal-by-skip-reason with a known prefix; bare
				// errors (subprocess crash, adapter panic) get the standard
				// "failed" close.
				skipReason, refuseDetail := parseLaunchRefusal(launchErr)
				if skipReason != "" {
					result.Skipped = skipReason
					result.Human = executeRefusedSummary(refused) + fmt.Sprintf("did nothing: %s", refuseDetail)
					result.Action = "none"
					result.RunAttemptIDs = nil
					return result, nil
				}
				// One bad launch fails THAT envelope only. Mark the work as
				// failed via Close (the only seam that emits a Receipt + state
				// transition with a failed outcome). The stage still returns
				// Action=execute so the heartbeat pass can keep going — the next
				// Continue picks the next envelope.
				//
				// A crashed adapter still mints a RunAttempt holding the workspace,
				// command and transcript of the attempt. The launch wire forwards
				// out + runErr to FinalizeGovernedHarnessRun which resolves the
				// attempt via the sealed launch-authority event and binds the id
				// onto envelope.RunAttemptIDs, so the failure closes the envelope
				// as failed AND keeps the evidence on the chain. The result summary
				// here names the id for the operator and result.RunAttemptIDs
				// records it for any downstream consumer that polls the tick result.
				//
				// work_r6m_seam_posture_20260911 (defect 2): the R6A canonical
				// finalize already closes the envelope as failed WITH the attempt
				// attached. The stage's job is to record the outcome, not to
				// attempt a second Close — a second Close with different proof
				// refs would refuse (idempotent re-close rejects new proof refs)
				// and the stage would report the refusal as if the failure were
				// unrecorded. Detect that the envelope has moved past `running`
				// BEFORE attempting Close; if it has, record the outcome as a
				// launch_failed skip and surface the minted ratt id without
				// attempting the second Close.
				closeSummary := "unattended execute launch failed: " + launchErr.Error()
				if rattID != "" {
					closeSummary += " (run attempt " + rattID + ")"
				}
				if after, afterErr := s.Get(ready); afterErr == nil && after.State != "running" {
					// The canonical finalize (or some other writer) has
					// already moved the envelope past `running` — do NOT
					// attempt a second Close. The stage records the outcome
					// truthfully: the envelope is already in its terminal
					// state with the attempt attached, the heartbeat
					// continues.
					result.Human = executeRefusedSummary(refused) + fmt.Sprintf("executed envelope %s on lane %s but launch failed: %v; envelope already %s with attempt attached (canonical finalize owns the close), heartbeat continues", ready, lane, launchErr, after.State)
					if rattID != "" {
						result.Human += fmt.Sprintf(" (partial evidence in run attempt %s)", rattID)
					}
					result.Action = "execute"
					result.Skipped = "launch_failed"
					result.RunAttemptIDs = nil
					if rattID != "" {
						result.RunAttemptIDs = []string{rattID}
					}
					return result, nil
				}
				if _, closeErr := s.Close(ready, CloseInput{
					Outcome:   "failed",
					MoveType:  "audit_only",
					Actor:     actor,
					Summary:   closeSummary,
					ProofRefs: []string{"execute_stage_launch_failed"},
				}, now); closeErr != nil {
					result.Human = executeRefusedSummary(refused) + fmt.Sprintf("executed but failed to record failure on %s: launch=%v close=%v", ready, launchErr, closeErr)
				} else {
					result.Human = executeRefusedSummary(refused) + fmt.Sprintf("executed envelope %s on lane %s but launch failed: %v; envelope marked failed, heartbeat continues", ready, lane, launchErr)
				}
				if rattID != "" {
					result.Human += fmt.Sprintf(" (partial evidence in run attempt %s; bound to the envelope via the canonical finalize)", rattID)
				}
				result.Action = "execute"
				result.Skipped = "launch_failed"
				// R6A 2026-09-10: slice 2's canonical finalize now closes the envelope
				// as failed AND attaches the minted ratt id, so the operator's view
				// (ContinueResult.RunAttemptIDs) and the envelope's own chain agree.
				// The pre-replay "named but not bound" gap closed: the launch wire
				// forwards out + runErr to FinalizeGovernedHarnessRun, which uses the
				// sealed same-state event MintLaunchAuthority appended to resolve the
				// attempt's authority and bind the id onto envelope.RunAttemptIDs.
				result.RunAttemptIDs = nil
				if rattID != "" {
					result.RunAttemptIDs = []string{rattID}
				}
				return result, nil
			}

			// Successful launch — advance to verifying and stamp the ratt id. The
			// execute stage NEVER lands, closes, pushes or retires anything; verifying
			// is the hand-off point for human/orchestrator review. The production
			// launch wire calls FinalizeGovernedHarnessRun inside launchStub which
			// already moves the envelope from running to verifying — re-running the
			// advance here would refuse (wrong state). Skip it when launchStub has
			// already taken the envelope past running.
			if after, afterErr := s.Get(ready); afterErr == nil {
				switch after.State {
				case "verifying", "completed", "failed":
					result.Action = "execute"
					result.Status = after.State
					result.RunAttemptIDs = []string{rattID}
					result.Human = executeRefusedSummary(refused) + fmt.Sprintf("executed envelope %s on lane %s (ratt=%s); stopped at %s for human/orchestrator review", ready, lane, rattID, after.State)
					return result, nil
				}
			}
			if _, verifyErr := s.Advance(ready, AdvanceInput{To: "verifying", Actor: actor, Note: "execute stage: verifying"}, now); verifyErr != nil {
				// The launch succeeded but the verifying advance failed — surface
				// the launch + the verify error so the operator can intervene.
				result.Action = "execute"
				result.RunAttemptIDs = []string{rattID}
				result.Human = executeRefusedSummary(refused) + fmt.Sprintf("executed envelope %s on lane %s (ratt=%s) but verifying advance failed: %v; envelope remains in running for review", ready, lane, rattID, verifyErr)
				result.NoAction = result.Human
				return result, nil
			}
			result.Action = "execute"
			result.Status = "verifying"
			result.RunAttemptIDs = []string{rattID}
			result.Human = executeRefusedSummary(refused) + fmt.Sprintf("executed envelope %s on lane %s (ratt=%s); stopped at verifying for human/orchestrator review", ready, lane, rattID)
			return result, nil
		}

	}

	// The stage examined every compatible route/candidate pair it was allowed
	// to and none of them launched. All entries remain ready and unmodified.
	// Skipped carries the HEAD's refusal family (the head is what
	// the DispatchStep fall-through would otherwise claim); the Human line
	// names every skipped id and reason, then states the outcome truthfully
	// (design decision 3).
	if len(refused) == 0 {
		// Defensive: every examined candidate either returns from the loop
		// or records a refusal, so this is unreachable today. Keep a
		// truthful line in case a future path forgets to record.
		result.Human = "did nothing: no eligible ready envelope"
		return result, nil
	}
	result.WorkID = refused[0].WorkID
	result.Skipped = refusalFamily(refused[0].Reason)
	result.Human = "did nothing: " + executeRefusedSummary(refused)
	result.Human += fmt.Sprintf("no launch; stopped after %d pick attempts across %d usable routes (bounds: %d routes, %d work per route)", workChecks, len(usable), executeStageMaxRouteCandidates, executeStageMaxPickAttempts)
	result.Action = "none"
	result.RunAttemptIDs = nil
	return result, nil
}

// findReadyForExecuteIDs returns every ready envelope id in workbench order
// (state priority, then id — the same rank findFirstReadyForExecute used to
// pick the head from). When ExpectWorkID is set the list is pinned to that
// id, preserving the pin semantics the pre-R6E stage had. The Recent fallback
// keeps surfacing ready items the Open count gate excluded.
func (s *Store) findReadyForExecuteIDs(now time.Time, actor, expectWorkID string) []string {
	report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 0, Actor: actor})
	if err != nil {
		return nil
	}
	expectWorkID = strings.TrimSpace(expectWorkID)
	seen := map[string]bool{}
	var out []string
	add := func(items []WorkbenchItem) {
		for _, item := range items {
			if item.State != "ready" || seen[item.WorkID] {
				continue
			}
			if expectWorkID != "" && item.WorkID != expectWorkID {
				continue
			}
			seen[item.WorkID] = true
			out = append(out, item.WorkID)
		}
	}
	add(report.Open)
	add(report.Recent)
	return out
}

// unattendedEligible decides, BEFORE the gates (b)-(e) run, whether the
// envelope may be executed unattended by the lane that would run it. An
// ineligible envelope is skipped with reason "ineligible:<why>" and never
// mutated. The rule (R6E settled design):
//
//	i.   origin.harness is empty or equals the route lane's harness family
//	     (claude_code for every claude_* lane) — the loop must never run
//	     another executor's work on this lane;
//	ii.  a saved execution_selection whose harness family differs from the
//	     route lane's is ineligible too (2026-09-15): the work's own saved
//	     choice is the plan, and the detached door would otherwise pass the
//	     global route as --harness and override it. Route/capacity for the
//	     wrong lane cannot attest this candidate either. An explicit
//	     policy-visible override is the escape, never a silent launch;
//	iii. owner is empty, claude_code:orchestrator, or starts with
//	     "orchestrator:" — the seam's own dispatchers are the only owners
//	     whose intent unattended execution may consume;
//	iv.  the git_delivery target root exists on disk, or there is no
//	     git_delivery at all (the launch resolves the workspace then).
//
// A nil Origin (legacy envelopes) reads as an empty harness and passes (i).
func unattendedEligible(w envelope.WorkEnvelope, lane string) (bool, string) {
	return unattendedEligibleCandidate(w, ExecutionRouteCandidate{LaunchProfileID: lane, Harness: lane})
}

func unattendedEligibleCandidate(w envelope.WorkEnvelope, candidate ExecutionRouteCandidate) (bool, string) {
	lane := candidate.LaunchProfileID
	harness := ""
	if w.Origin != nil {
		harness = strings.TrimSpace(w.Origin.Harness)
	}
	if w.ExecutionSelection != nil {
		if !savedSelectionCompatible(*w.ExecutionSelection, candidate) {
			return false, fmt.Sprintf("saved execution_selection is not selected profile %s", candidate.LaunchProfileID)
		}
	} else if harness != "" && !originHarnessCompatible(harness, candidate.Harness) {
		// Provenance protects unselected work from being picked up by a
		// foreign executor. An explicit saved selection is the later,
		// operator-visible choice of executor identity and is checked above.
		return false, fmt.Sprintf("harness=%s is not the route lane harness %s", harness, routeLaneHarness(lane))
	}
	owner := strings.TrimSpace(w.Owner)
	if (owner != "" || w.ExecutionSelection != nil) && owner != "claude_code:orchestrator" && !strings.HasPrefix(owner, "orchestrator:") {
		return false, "owner=" + owner + " is not an unattended dispatcher"
	}
	if w.GitDelivery != nil {
		root := strings.TrimSpace(w.GitDelivery.TargetWorktreeRoot)
		if root == "" {
			return false, "git_delivery has no target_worktree_root"
		}
		if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
			return false, "workspace_missing=" + root
		}
	}
	return true, ""
}

// routeLaneHarness returns the exact normalized lane identity. Hyphen and
// underscore are spelling aliases only; provider/profile identity stays part
// of the comparison and is never collapsed to a generic Claude harness.
func routeLaneHarness(lane string) string {
	return normalizeRouteLane(lane)
}

func normalizeRouteLane(lane string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(lane)), "-", "_")
}

func originHarnessCompatible(harness, lane string) bool {
	harness = normalizeRouteLane(harness)
	lane = routeLaneHarness(lane)
	if harness == lane {
		return true
	}
	// Origin records older Claude-family provenance as claude_code. Preserve
	// that trusted family guard while requiring saved selections to name an
	// exact launch profile below.
	return harness == "claude_code" && strings.HasPrefix(lane, "claude_")
}

func savedSelectionCompatible(selection envelope.ExecutionSelection, candidate ExecutionRouteCandidate) bool {
	lane := routeLaneHarness(candidate.LaunchProfileID)
	profile := normalizeRouteLane(selection.LaunchProfileID)
	if profile == "" || profile != lane {
		return false
	}
	if model := strings.TrimSpace(selection.RequestedModelID); model != "" && model != candidate.RequestedModelID {
		return false
	}
	if provider := strings.TrimSpace(selection.ProviderRoute); provider != "" && provider != candidate.ProviderRoute {
		return false
	}
	harness := normalizeRouteLane(selection.Harness)
	selectedHarness := normalizeRouteLane(candidate.Harness)
	return harness == selectedHarness
}

// This only carries a saved spelling into owner preflight; it never establishes
// alias authority. SourceRevision requires the tuple-aware production path to
// re-resolve model, provider, harness and manifest before claim and launch.
func executionCandidateForSelection(candidate ExecutionRouteCandidate, selection *envelope.ExecutionSelection) ExecutionRouteCandidate {
	if selection != nil && candidate.SourceRevision != "" &&
		strings.TrimSpace(selection.LaunchProfileID) != "" &&
		candidate.RequestedModelID != "" && selection.RequestedModelID == candidate.RequestedModelID &&
		candidate.ProviderRoute != "" && selection.ProviderRoute == candidate.ProviderRoute &&
		normalizeRouteLane(selection.Harness) == normalizeRouteLane(candidate.Harness) {
		candidate.LaunchProfileID = selection.LaunchProfileID
	}
	return candidate
}

// refusalFamily strips a refusal reason to its machine-readable family —
// the text before the first ":" ("ineligible:harness=codex ..." ->
// "ineligible"; a bare "risk" stays "risk").
func refusalFamily(reason string) string {
	family, _, _ := strings.Cut(reason, ":")
	return strings.TrimSpace(family)
}

// executeRefusedSummary renders the refused list as the Human-line prefix
// "<id> (<reason>), <id> (<reason>); " — empty when nothing was refused, so
// single-refusal messages read exactly as they did before R6E.
func executeRefusedSummary(refused []ExecuteRefusal) string {
	if len(refused) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refused))
	for _, r := range refused {
		parts = append(parts, fmt.Sprintf("%s (%s)", r.WorkID, r.Reason))
	}
	return strings.Join(parts, ", ") + "; "
}

// executeStageRefusedHeadUntouchable reports whether the stage ended without
// a launch while the bench HEAD (Refused[0] — refusals are recorded in pick
// order) was refused for a reason that must leave it byte-identical:
// ineligibility (another executor's work) or a recognized preflight refusal.
// Continue consults this before the DispatchStep fall-through, which would
// otherwise CLAIM the first ready item — mutating an envelope the execute
// stage just declined to touch. Preflight families are normally visible
// through stage.Skipped already; this closes the gap for stage endings whose
// terminal Skipped came from a later candidate's gate, and for ineligible
// heads.
func executeStageRefusedHeadUntouchable(stage ContinueResult) bool {
	if len(stage.Refused) == 0 {
		return false
	}
	switch refusalFamily(stage.Refused[0].Reason) {
	case "ineligible", "adapter_missing", "workspace_unresolvable", "lane_unready":
		return true
	default:
		return false
	}
}

// executeRiskAllowed reports whether the envelope risk is inside the
// unattended envelope (tier0/tier1). Anything else fails closed.
func executeRiskAllowed(risk string) bool {
	switch strings.ToLower(strings.TrimSpace(risk)) {
	case "tier0", "tier1":
		return true
	default:
		return false
	}
}

// executePrivacyAllowed reports whether the envelope privacy is inside the
// unattended envelope (private). Anything else fails closed.
func executePrivacyAllowed(privacy string) bool {
	return strings.ToLower(strings.TrimSpace(privacy)) == "private"
}

// executeStageHasGrant reads the standing-grants registry from
// `~/.compound/registries/standing-grants.json` and reports whether the
// unattended grant is present and not expired. A missing registry file is
// fail-closed (no grant).
func (s *Store) executeStageHasGrant(homePath string) (bool, error) {
	if strings.TrimSpace(homePath) == "" {
		return false, nil
	}
	path := filepath.Join(homePath, "registries", "standing-grants.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var registry struct {
		Grants []struct {
			ID         string `json:"id"`
			Capability string `json:"capability"`
			ExpiresAt  string `json:"expires_at"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return false, fmt.Errorf("parse standing-grants registry: %w", err)
	}
	for _, g := range registry.Grants {
		if g.ID == executeUnattendedGrantID && g.Capability == executeUnattendedCapability {
			if g.ExpiresAt == "" {
				continue
			}
			exp, err := time.Parse(time.RFC3339, g.ExpiresAt)
			if err != nil {
				return false, fmt.Errorf("grant %s has invalid expires_at %q: %w", g.ID, g.ExpiresAt, err)
			}
			if time.Now().UTC().After(exp) {
				return false, nil
			}
			return true, nil
		}
	}
	_ = sort.Slice // keep sort import used; registry reads are unordered but sort keeps the file stable.
	return false, nil
}

// ExecuteStageCapacityFreshness reports whether a capacity observation
// stamped at lastProven is fresh enough for unattended execution at now.
// Fail-closed: a zero observation time is never fresh, and neither is a
// stamp from the future.
func ExecuteStageCapacityFreshness(lastProven, now time.Time) bool {
	if lastProven.IsZero() {
		return false
	}
	age := now.Sub(lastProven)
	return age >= 0 && age <= executeStageCapacityFreshnessWindow
}

// defaultExecuteStageRoutePlanFn, defaultExecuteStageCapacityFn, and
// defaultExecuteStagePolicyEvalFn are the production defaults for the
// execute-stage hooks. They are intentionally minimal: read the home's
// route plan, capacity projection, and policy, and translate the result
// into the seam's narrow types. cmd/compound wires a richer default on
// startup if the production path needs real adapter launch.

// defaultExecuteStageRoutePlanFn returns no eligible lane — production must
// wire this to routeevidence.LoadModelCapabilitiesSnapshot. The default is
// fail-closed; any production caller that forgets to wire the real resolver
// sees every execute attempt as `skipped=no_route`.
func defaultExecuteStageRoutePlanFn(homePath, capability string) (string, bool) {
	return "", false
}

func defaultExecuteStageRouteCandidatesFn(homePath, capability string) []ExecutionRouteCandidate {
	lane, ok := executeStageRoutePlanFn(homePath, capability)
	if !ok || strings.TrimSpace(lane) == "" {
		return nil
	}
	return []ExecutionRouteCandidate{{LaunchProfileID: lane, Harness: lane, CapacityLane: lane}}
}

// defaultExecuteStageCapacityFn returns fresh=false — production must wire
// this to routeevidence.ReadLaneCapacityWithConfig. The default is
// fail-closed; any production caller that forgets to wire the real reader
// sees every execute attempt as `skipped=capacity_unknown`.
func defaultExecuteStageCapacityFn(homePath, lane string, now time.Time) (float64, bool, error) {
	return 0, false, nil
}

// defaultExecuteStagePolicyEvalFn is the REAL evaluator: the same door
// `work advance --execute` consumes (policy.LoadConfig + policy.Evaluate on
// an envelope.ActionIntent for capability work.continue.unattended). It fails
// closed twice: an unreadable config is a hold, and an allow that is not
// backed by the unattended standing grant (StandingGrantID mismatch — e.g.
// the local_reversible class baseline allowing on its own) is downgraded to
// hold. The operator's named grant is the only authority that opens this
// gate; the class baseline alone never does.
func defaultExecuteStagePolicyEvalFn(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim {
	intent := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            ai.ID,
			Privacy:       ai.Privacy,
			Risk:          ai.Risk,
		},
		Actor:         "compound-unattended-execute",
		Capability:    ai.Capability,
		Action:        "execute_unattended",
		Target:        envelope.ActionTarget{Kind: "work", ID: ai.WorkID, Path: filepath.Join(cfg.Home, "work", "work", ai.WorkID+".json")},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "execute ready work " + ai.WorkID + " unattended via work continue",
		RollbackPlan:  "advance work back to ready if the launch produced no side effects",
	}
	config, _, err := policy.LoadConfig(cfg.Home)
	if err != nil {
		return policyDecisionShim{ID: "pol_unattended_config_unreadable", Outcome: "hold"}
	}
	pd := policy.Evaluate(intent, config, now)
	switch pd.Outcome {
	case "allow", "allow_with_obligations":
		if pd.StandingGrantID != executeUnattendedGrantID {
			return policyDecisionShim{ID: pd.ID, Outcome: "hold"}
		}
	}
	return policyDecisionShim{ID: pd.ID, Outcome: pd.Outcome}
}

// defaultExecuteStageLaunch returns an explicit error — production must wire
// this to the actuator's ExecuteGovernedHarnessAdvance seam. The default is
// fail-closed; any production caller that forgets to wire the real launcher
// sees every execute attempt as `skipped=launch_failed`.
func defaultExecuteStageLaunch(_ *Store, _ string, _ string, _ string, _ string, _ string, _ time.Time) (string, error) {
	return "", errors.New("execute stage launch not wired; production must bind launchStub to the actuator seam")
}

// defaultExecuteStagePreflight is the no-op preflight default. Production
// must wire this to executewire.PreflightViaActuator (which resolves adapter,
// workspace and lane readiness) so a recognized refusal can leave the
// envelope untouched. The default returns nil so test fixtures that only
// override the launchStub keep flowing through Claim → Advance → Launch
// the way they always did; the explicit preflight override (DEFECT 1 test)
// drives the recognized-refusal paths.
func defaultExecuteStagePreflight(_ *Store, _ string, _ string, _ time.Time) error {
	return nil
}

// parseLaunchRefusal inspects the error returned by launchStub and reports
// whether it carries a refusal-prefix the seam should surface as a stage
// skip reason rather than a hard launch failure. Returns ("", "") for bare
// errors, which the seam treats as an adapter-level failure and closes the
// envelope as failed.
//
// Wire contract: the launch function can prefix its error with
// "<skip_reason>:<detail>" when it refuses the launch without invoking the
// adapter. Today that is "lane_unready:<details>" (the readiness guard
// reported not-ready), "workspace_unresolvable:<reason>", and
// "adapter_missing:<harness>". Bare errors are infrastructure failures that
// the envelope "failed" close should record. Adding a new refusal kind is
// safe: it must be listed in executeStageLaunchRefusalReasons.
func parseLaunchRefusal(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	msg := err.Error()
	for _, prefix := range executeStageLaunchRefusalReasons {
		if strings.HasPrefix(msg, prefix+":") {
			detail := strings.TrimSpace(strings.TrimPrefix(msg, prefix+":"))
			return prefix, detail
		}
	}
	return "", ""
}

// executeStageLaunchRefusalReasons lists every skip reason the launch wire
// can emit on its own (without invoking the adapter). The seam reads this
// list to distinguish a refused launch (envelope stays in its current
// state, no Close) from a launched-and-failed launch (envelope is
// terminalized "failed"). Order matters only for prefix matching — none of
// the listed prefixes is a prefix of another.
var executeStageLaunchRefusalReasons = []string{
	"lane_unready",
	"workspace_unresolvable",
	"adapter_missing",
	// selection_unsupported:<harness>:<field> -- the saved/override selection
	// asks for a model or effort the harness cannot apply (internal/harness
	// SelectionCapability). Deterministic: the envelope is not claimed or burned.
	"selection_unsupported",
}
