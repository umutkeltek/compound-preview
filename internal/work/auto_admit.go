package work

// Auto-admit chokepoint. AdmitAuto is the ONLY path that releases
// director-proposed work from proposed -> admitted without operator
// provenance. The first enabled slice is hard-limited to job_health and
// requires a current typed grant plus a content-addressed eligibility proof
// replayed from terminal Receipts and route evidence. The kill-switch runs
// first, and the generic Advance seam retains a defense-in-depth halt check.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/policy"
	autonomyproof "compound/internal/routeevidence/autonomy"
)

// AutoAdmitInput carries a typed auto-admit request. EligibilityRef identifies
// the artifact to load and fully revalidate; no caller assertion establishes
// authority. The kill-switch overrides valid eligibility unconditionally.
type AutoAdmitInput struct {
	By             string // autonomous actor requesting admission (default "director")
	EligibilityRef string // required content-addressed autonomy eligibility artifact
	Note           string
	// Deprecated compatibility fields decode old saved reports but are never
	// consulted. A caller string or grant ref cannot establish eligibility.
	EligibilityReason string `json:"eligibility_reason,omitempty"`
	GrantRef          string `json:"grant_ref,omitempty"`
}

// AutoAdmitResult reports the outcome of an auto-admit attempt. On a halt or an
// ineligible request Admitted is false and the work stays proposed.
type AutoAdmitResult struct {
	WorkID         string   `json:"work_id"`
	WorkState      string   `json:"work_state"`
	Class          string   `json:"class"`
	Admitted       bool     `json:"admitted"`
	Halted         bool     `json:"halted"`
	Reason         string   `json:"reason,omitempty"`
	EligibilityRef string   `json:"eligibility_ref,omitempty"`
	GrantRef       string   `json:"grant_ref,omitempty"`
	EvidenceRefs   []string `json:"evidence_refs,omitempty"`
}

// autoAdmitBeforeCommit is a test synchronization seam. Production keeps the
// no-op; tests use it to prove concurrent authority writers cannot enter
// between final eligibility validation and the admitted transition.
var autoAdmitBeforeCommit = func() {}

// AdmitAuto attempts autonomous admission of director-proposed work. Its fixed
// precedence is load-and-shape -> halt -> editable surface -> freeze ->
// operator-only risk -> typed eligibility -> admit. It never changes work
// state on a blocked, halted, or ineligible request; denials still leave an
// auditable policy/event trail where the corresponding guard owns one.
func (s *Store) AdmitAuto(workID string, in AutoAdmitInput, now time.Time) (AutoAdmitResult, error) {
	var out AutoAdmitResult
	var admitErr error
	err := s.withLifecycleMutation(func() error {
		out, admitErr = s.admitAutoLocked(workID, in, now)
		return admitErr
	})
	return out, err
}

func (s *Store) admitAutoLocked(workID string, in AutoAdmitInput, now time.Time) (AutoAdmitResult, error) {
	w, err := s.recoverPendingClaimBeforeMutation(workID)
	if err != nil {
		return AutoAdmitResult{}, err
	}
	if w.State != "proposed" {
		return AutoAdmitResult{WorkID: w.ID, WorkState: w.State}, fmt.Errorf("auto-admit requires proposed work, got %q", w.State)
	}
	if !directorProposedWork(w) {
		return AutoAdmitResult{WorkID: w.ID, WorkState: w.State}, fmt.Errorf("auto-admit only applies to director-proposed work")
	}
	class, err := autonomyClassOf(w)
	if err != nil {
		return AutoAdmitResult{WorkID: w.ID, WorkState: w.State, Class: "director", Reason: err.Error()}, err
	}
	if recovered, ok, err := s.recoverPendingAdvance(w, "admitted", nil); err != nil {
		return AutoAdmitResult{WorkID: w.ID, WorkState: w.State, Class: class, Reason: err.Error()}, err
	} else if ok {
		result := AutoAdmitResult{
			WorkID:       recovered.ID,
			WorkState:    recovered.State,
			Class:        class,
			Admitted:     true,
			Reason:       "recovered durable admission",
			EvidenceRefs: append([]string{}, recovered.EvidenceRefs...),
		}
		for _, ref := range recovered.EvidenceRefs {
			switch {
			case strings.HasPrefix(ref, "eligibility:"):
				result.EligibilityRef = ref
			case strings.HasPrefix(ref, "grant:"):
				result.GrantRef = ref
			}
		}
		return result, nil
	}
	actor := autoAdmitActor(in.By)

	// 1. Kill-switch (W9-J4): a halt overrides any eligibility. Records a deny, stays proposed.
	halted, reason, err := policy.AutonomyHalted(s.home, class)
	if err != nil {
		return AutoAdmitResult{WorkID: w.ID, WorkState: w.State, Class: class}, err
	}
	if halted {
		if recErr := s.recordAutonomyHalt(w, actor, "admitted", reason, now); recErr != nil {
			return AutoAdmitResult{WorkID: w.ID, WorkState: w.State, Class: class}, recErr
		}
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class, Halted: true, Reason: reason},
			fmt.Errorf("autonomy halted (%s): %s", class, reason)
	}

	// 1.5 Editable-surfaces (EVOLVE-BLOCK): if this proposal declares a target harness
	// surface, it may be admitted only while that surface is actively whitelisted. Runs
	// AFTER the kill-switch (so a halt wins) and BEFORE eligibility. A proposal with no
	// target_surface is ordinary work and passes untouched.
	if err := s.editableSurfaceGuard(w, "admitted", actor, now); err != nil {
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class, Reason: err.Error()}, err
	}

	// 1.6 Freeze (grants-as-gates, 2026-07-10): a director-proposed admission
	// matching an active freeze decision record is denied, naming the freeze.
	// Runs AFTER the kill-switch and editable-surfaces guard, BEFORE
	// eligibility. An envelope matching no active freeze passes untouched.
	if err := s.freezeGuard(w, "admitted", actor, now); err != nil {
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class, Reason: err.Error()}, err
	}

	// 1.7 Operator-only risk frontier. This is deliberately outside ordinary
	// standing-grant evaluation: no policy grant can override it.
	action, err := s.autonomyAdmissionAction(w)
	if err != nil {
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class, Reason: err.Error()}, err
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class}, err
	}
	if reason := policy.AutonomyOperatorOnlyReason(action, cfg); reason != "" {
		if recErr := s.recordAutonomyRiskDeny(w, actor, "admitted", reason, now); recErr != nil {
			return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class}, recErr
		}
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class, Reason: reason}, fmt.Errorf("auto-admit operator-only risk: %s", reason)
	}

	// 2. Typed evidence eligibility. job_health is the only enabled class in
	// this first vertical slice. The artifact is loaded by content id and fully
	// recompiled from current receipts, raw route-row hashes, and router source
	// revision; caller-provided strings and GrantRef are ignored.
	if class != "job_health" {
		reason := fmt.Sprintf("class %q has no enabled evidence-autonomy slice", class)
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class, Reason: reason}, fmt.Errorf("auto-admit not eligible: %s", reason)
	}
	eligibilityRef := strings.TrimSpace(in.EligibilityRef)
	if eligibilityRef == "" {
		reason := "typed eligibility artifact required; caller assertions are not authority"
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class, Reason: reason}, fmt.Errorf("auto-admit not eligible for class %q: %s", class, reason)
	}
	eligibility, err := autonomyproof.LoadAndRevalidate(s.home, eligibilityRef, class, w.RouteDecisionID)
	if err != nil {
		return AutoAdmitResult{WorkID: w.ID, WorkState: "proposed", Class: class, EligibilityRef: eligibilityRef, Reason: err.Error()}, err
	}
	refs := eligibility.AdmissionRefs()
	resultBase := AutoAdmitResult{
		WorkID:         w.ID,
		WorkState:      "proposed",
		Class:          class,
		EligibilityRef: eligibility.Ref(),
		GrantRef:       eligibility.Grant.GrantRef,
		EvidenceRefs:   append([]string{}, refs...),
	}
	admissionInstant := now.UTC().Format(time.RFC3339)
	if eligibility.GeneratedAt != admissionInstant {
		reason := fmt.Sprintf("eligibility evaluation instant %q does not match admission instant %q", eligibility.GeneratedAt, admissionInstant)
		resultBase.Reason = reason
		if recErr := s.recordAutonomyEligibilityDeny(w, actor, "admitted", reason, refs, now); recErr != nil {
			return resultBase, recErr
		}
		return resultBase, fmt.Errorf("auto-admit not eligible for class %q: %s", class, reason)
	}
	if err := s.validateActiveAutonomyGrant(eligibility); err != nil {
		resultBase.Reason = err.Error()
		if recErr := s.recordAutonomyEligibilityDeny(w, actor, "admitted", err.Error(), refs, now); recErr != nil {
			return resultBase, recErr
		}
		return resultBase, err
	}
	if eligibility.Status != "eligible" {
		reason := strings.Join(eligibility.Reasons, "; ")
		if reason == "" {
			reason = "eligibility threshold not met"
		}
		resultBase.Reason = reason
		if recErr := s.recordAutonomyEligibilityDeny(w, actor, "admitted", reason, refs, now); recErr != nil {
			return resultBase, recErr
		}
		return resultBase, fmt.Errorf("auto-admit not eligible for class %q: %s", class, reason)
	}

	// 3. Cleared: admit through the shared advance write path.
	autoAdmitBeforeCommit()
	note := strings.TrimSpace(in.Note)
	if note == "" {
		note = "auto-admitted (" + class + "): " + eligibility.Ref()
	}
	advanced, err := s.applyAdvance(w, AdvanceInput{
		Actor:          actor,
		To:             "admitted",
		VerificationID: eligibility.ID,
		EvidenceRefs:   refs,
		Note:           note,
	}, now)
	if err != nil {
		return resultBase, err
	}
	resultBase.WorkID = advanced.ID
	resultBase.WorkState = advanced.State
	resultBase.Admitted = true
	resultBase.Reason = eligibility.Ref()
	return resultBase, nil
}

func (s *Store) autonomyAdmissionAction(w envelope.WorkEnvelope) (envelope.ActionIntent, error) {
	for _, ref := range w.ActionIntentIDs {
		if !strings.HasPrefix(ref, "act_") {
			continue
		}
		var action envelope.ActionIntent
		if err := s.readEventJSON(ref, &action); err != nil {
			return envelope.ActionIntent{}, fmt.Errorf("autonomy admission action %s unreadable: %w", ref, err)
		}
		if err := action.Validate(); err != nil {
			return envelope.ActionIntent{}, fmt.Errorf("autonomy admission action %s invalid: %w", ref, err)
		}
		if action.IDs.WorkID != w.ID {
			return envelope.ActionIntent{}, fmt.Errorf("autonomy admission action %s belongs to work %s", ref, action.IDs.WorkID)
		}
		return action, nil
	}
	return envelope.ActionIntent{}, fmt.Errorf("autonomy admission requires a recorded create ActionIntent")
}

func (s *Store) validateActiveAutonomyGrant(artifact autonomyproof.Artifact) error {
	store, err := mem.Open(s.home)
	if err != nil {
		return err
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		return err
	}
	superseded := map[string]bool{}
	for _, claim := range claims {
		if claim.ClaimType != "decision" ||
			!mem.HasOperationalDecisionAuthority(claim.Qualifiers) ||
			!policy.HasOperatorProvenance(claim.Qualifiers["reviewed_by"]) ||
			claim.Qualifiers["lifecycle_status"] != "accepted" {
			continue
		}
		for _, id := range splitFreezeCSV(claim.Qualifiers["supersedes"]) {
			superseded[id] = true
		}
	}
	var classClaims []mem.Claim
	for _, claim := range claims {
		q := claim.Qualifiers
		if claim.ClaimType != "decision" ||
			!mem.HasOperationalDecisionAuthority(q) ||
			!policy.HasOperatorProvenance(q["reviewed_by"]) ||
			q["kind"] != "grant" || q["lifecycle_status"] != "accepted" || q["scope"] != "autonomy-evidence-threshold" {
			continue
		}
		if superseded[q["decision_id"]] || q["auto_admit_class"] != artifact.Class {
			continue
		}
		classClaims = append(classClaims, claim)
	}
	if len(classClaims) != 1 {
		return fmt.Errorf("active class-scoped autonomy grant for %q is missing or ambiguous (found %d)", artifact.Class, len(classClaims))
	}
	claim := classClaims[0]
	q := claim.Qualifiers
	grant := artifact.Grant
	if claim.ID != grant.ClaimID || q["decision_id"] != grant.DecisionID || grant.GrantRef != "grant:"+q["decision_id"] || grant.ClaimRef != "claim:"+claim.ID {
		return fmt.Errorf("eligibility grant ref does not match the active grant claim")
	}
	if q["reviewed_by"] != grant.ReviewedBy || qualifierInt(q, "min_terminal_samples") != grant.MinTerminalSamples || qualifierInt(q, "min_window_days") != grant.MinWindowDays || qualifierInt(q, "max_failures") != grant.MaxFailures {
		return fmt.Errorf("eligibility threshold does not match the active grant")
	}
	return nil
}

func qualifierInt(q map[string]string, key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(q[key]))
	return n
}

func (s *Store) recordAutonomyRiskDeny(w envelope.WorkEnvelope, actor, to, reason string, now time.Time) error {
	ai := s.advanceActionIntent(w, actor, to, now, AdvancePlanOptions{Actor: actor, To: to})
	pd := policy.AutonomyRiskDenyDecision(ai, reason, now)
	return s.recordAutonomyDeny(w, actor, "autonomy risk deny: "+reason, ai, pd, []string{policy.AutonomyRiskAuthorityRef}, now)
}

func (s *Store) recordAutonomyEligibilityDeny(w envelope.WorkEnvelope, actor, to, reason string, refs []string, now time.Time) error {
	ai := s.advanceActionIntent(w, actor, to, now, AdvancePlanOptions{Actor: actor, To: to})
	pd := policy.AutonomyEligibilityDenyDecision(ai, reason, now)
	pd.EvidenceRefs = mergeUnique(pd.EvidenceRefs, refs)
	return s.recordAutonomyDeny(w, actor, "autonomy eligibility deny: "+reason, ai, pd, refs, now)
}

func (s *Store) recordAutonomyDeny(w envelope.WorkEnvelope, actor, note string, ai envelope.ActionIntent, pd envelope.PolicyDecision, refs []string, now time.Time) error {
	if err := ai.Validate(); err != nil {
		return err
	}
	if err := pd.Validate(); err != nil {
		return err
	}
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		return err
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		return err
	}
	attached := mergeUnique([]string{ai.ID, pd.ID}, refs)
	return s.appendEvent(Event{TS: ts(now), WorkID: w.ID, From: w.State, To: w.State, By: actor, Note: note, Attached: attached})
}

// autonomyHaltGuard is the defense-in-depth kill-switch check inside the generic Advance
// seam. It denies an autonomous (non-operator) admission of director-proposed work while
// the policy-owned halt is engaged, and records a deny PolicyDecision. It is a no-op for
// operator admits, non-proposed work, and an unset halt.
func (s *Store) autonomyHaltGuard(w envelope.WorkEnvelope, to, actor string, now time.Time) error {
	if !isAutonomousAdmission(w, to, actor) {
		return nil
	}
	class, err := autonomyClassOf(w)
	if err != nil {
		return err
	}
	halted, reason, err := policy.AutonomyHalted(s.home, class)
	if err != nil {
		return err
	}
	if !halted {
		return nil
	}
	if err := s.recordAutonomyHalt(w, actor, to, reason, now); err != nil {
		return err
	}
	return fmt.Errorf("autonomy halted (%s): %s", class, reason)
}

// recordAutonomyHalt writes the ActionIntent and a deny PolicyDecision for a halted
// autonomous admission, plus a lifecycle event, so the halt leaves an auditable trail
// while the work stays in place.
func (s *Store) recordAutonomyHalt(w envelope.WorkEnvelope, actor, to, reason string, now time.Time) error {
	ai := s.advanceActionIntent(w, actor, to, now, AdvancePlanOptions{Actor: actor, To: to})
	if err := ai.Validate(); err != nil {
		return err
	}
	pd := policy.AutonomyHaltDecision(ai, reason, now)
	if err := pd.Validate(); err != nil {
		return err
	}
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		return err
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		return err
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       actor,
		Note:     "autonomy halt: " + reason,
		Attached: []string{ai.ID, pd.ID},
	})
}

// editableSurfaceGuard is the editable-surfaces (EVOLVE-BLOCK) check at the admission seam.
// It denies a system-originated (director-proposed, non-operator) admission whose
// ProposalKind classification makes it a harness-edit proposal, or whose classification
// is empty/unknown (no longer silently exempt as ordinary). The discrimination is
// stamped by the trusted producer (internal/director.proposalKindFor) onto WorkEnvelope.
// ProposalKind; missing/unknown classification cannot grant the ordinary-work exemption.
// The registry check still applies for ordinary proposals that declare a TargetSurface
// for any other reason. Operator admits and non-proposed work remain no-ops.
//
// Decision table (after the non-autonomous early return):
//
//	ProposalKind        TargetSurface   Result
//	empty/unknown       any             recorded deny ("unclassified autonomous proposal")
//	harness_edit        empty/whitespace recorded deny ("harness_edit missing target")
//	harness_edit        whitelisted     pass
//	harness_edit        not whitelisted recorded deny (existing whitelist check)
//	ordinary            empty           pass (the legitimate ordinary-work path)
//	ordinary            nonempty        existing whitelist check (pass or deny)
func (s *Store) editableSurfaceGuard(w envelope.WorkEnvelope, to, actor string, now time.Time) error {
	if !isAutonomousAdmission(w, to, actor) {
		return nil
	}
	kind := strings.TrimSpace(w.ProposalKind)
	surface := strings.TrimSpace(w.TargetSurface)
	switch kind {
	case "":
		if err := s.recordEditableSurfaceDeny(w, actor, to, "unclassified autonomous proposal: ProposalKind required (ordinary|harness_edit)", now); err != nil {
			return err
		}
		return fmt.Errorf("editable surface denied: unclassified autonomous proposal (ProposalKind empty)")
	case envelope.ProposalKindHarnessEdit:
		if surface == "" {
			if err := s.recordEditableSurfaceDeny(w, actor, to, "harness_edit proposal missing target_surface", now); err != nil {
				return err
			}
			return fmt.Errorf("editable surface denied: harness_edit missing target_surface")
		}
		allowed, reason, err := policy.EditableSurfaceAllowed(s.home, surface)
		if err != nil {
			return err
		}
		if allowed {
			return nil
		}
		if err := s.recordEditableSurfaceDeny(w, actor, to, reason, now); err != nil {
			return err
		}
		return fmt.Errorf("editable surface denied (%s): %s", surface, reason)
	case envelope.ProposalKindOrdinary:
		if surface == "" {
			return nil
		}
		allowed, reason, err := policy.EditableSurfaceAllowed(s.home, surface)
		if err != nil {
			return err
		}
		if allowed {
			return nil
		}
		if err := s.recordEditableSurfaceDeny(w, actor, to, reason, now); err != nil {
			return err
		}
		return fmt.Errorf("editable surface denied (%s): %s", surface, reason)
	default:
		// envelope.WorkEnvelope.Validate already refuses unrecognized ProposalKind values
		// before persistence, so reaching this branch requires a producer that bypassed
		// Validate. Fail closed.
		return fmt.Errorf("editable surface denied: unknown ProposalKind %q", kind)
	}
}

// recordEditableSurfaceDeny writes the ActionIntent and a deny PolicyDecision for a
// system-originated proposal blocked by the editable-surfaces registry, plus a lifecycle
// event, so the denial leaves an auditable trail while the work stays in place.
func (s *Store) recordEditableSurfaceDeny(w envelope.WorkEnvelope, actor, to, reason string, now time.Time) error {
	ai := s.advanceActionIntent(w, actor, to, now, AdvancePlanOptions{Actor: actor, To: to})
	if err := ai.Validate(); err != nil {
		return err
	}
	pd := policy.EditableSurfaceDenyDecision(ai, reason, now)
	if err := pd.Validate(); err != nil {
		return err
	}
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		return err
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		return err
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       actor,
		Note:     "editable-surface deny: " + reason,
		Attached: []string{ai.ID, pd.ID},
	})
}

// isAutonomousAdmission is true only for a non-operator release of director-proposed work
// out of `proposed` — the exact shape a trust-dial auto-admit or director-originated
// advance takes. Operator admits, claims, and ordinary advances are excluded.
func isAutonomousAdmission(w envelope.WorkEnvelope, to, actor string) bool {
	return w.State == "proposed" && to != "" && to != "proposed" && directorProposedWork(w) && !hasOperatorProvenance(actor)
}

// autonomyClassOf derives the autonomy class of a director-proposed work from its
// director_trigger evidence ref (e.g. director_trigger:job_health:<digest> -> job_health).
// Director work without a trigger ref falls back to the generic class "director".
func autonomyClassOf(w envelope.WorkEnvelope) (string, error) {
	var triggers []string
	for _, ref := range w.EvidenceRefs {
		ref = strings.TrimSpace(ref)
		if !strings.HasPrefix(ref, "director_trigger:") {
			continue
		}
		triggers = append(triggers, ref)
	}
	if len(triggers) == 0 {
		return "director", nil
	}
	if len(triggers) != 1 {
		return "", fmt.Errorf("work %s has ambiguous director_trigger identity (%d refs)", w.ID, len(triggers))
	}
	rest := strings.TrimPrefix(triggers[0], "director_trigger:")
	index := strings.Index(rest, ":")
	if index <= 0 || index == len(rest)-1 {
		return "", fmt.Errorf("work %s has invalid director_trigger %q", w.ID, triggers[0])
	}
	return rest[:index], nil
}

// autoAdmitActor stamps an autonomous-admission provenance so the actor is never mistaken
// for an operator. hasOperatorProvenance is false for it, so the kill-switch always applies.
func autoAdmitActor(by string) string {
	by = strings.TrimSpace(by)
	if by == "" {
		by = "director"
	}
	if hasAutoAdmitProvenance(by) {
		return by
	}
	return "auto-admit:" + by
}

func hasAutoAdmitProvenance(actor string) bool {
	actor = strings.ToLower(strings.TrimSpace(actor))
	return actor == "auto-admit" || strings.HasPrefix(actor, "auto-admit:") || strings.HasPrefix(actor, "auto-admit-")
}

// --- Freeze admission guard (grants-as-gates, 2026-07-10) ---
//
// The sibling of autonomyHaltGuard/editableSurfaceGuard at the same
// admission seam. Its source of truth is a decision RECORD (an accepted
// kind=freeze claim in the decisions store), never markdown/worklog/brain
// prose (falsifier e). Matching is literal (policy.FreezeMatch): an owner
// glob or a keyword substring against the canonical surface/evidence refs,
// never inferred.
//
// This reads mem.Claim qualifiers directly instead of going through
// internal/decisions' ActiveDecisions/LifecycleFromClaim helpers: decisions
// imports internal/writeback, which imports internal/work, so internal/work
// importing internal/decisions would be an import cycle. The qualifier keys
// (kind, lifecycle_status, decision_id, freeze_owner_globs, freeze_keywords,
// supersedes, reviewed_by) mirror internal/decisions' schema exactly —
// `compound decisions record --kind freeze` is the only writer of these
// qualifiers, so the two stay in lockstep by construction.
const (
	freezeDecisionKind            = "freeze"
	freezeDecisionStatusAccepted  = "accepted"
	freezeDecisionSupersedesField = "supersedes"
)

// freezeGuard is the enforcement seam AdmitAuto and Advance both call: a
// no-op for operator admits, non-proposed work, and when nothing active
// matches. A matching freeze refuses the admission, recording a deny.
func (s *Store) freezeGuard(w envelope.WorkEnvelope, to, actor string, now time.Time) error {
	if !isAutonomousAdmission(w, to, actor) {
		return nil
	}
	matched, rec, err := s.matchingFreeze(w)
	if err != nil {
		return err
	}
	if !matched {
		return nil
	}
	if err := s.recordFreezeDeny(w, actor, to, rec, now); err != nil {
		return err
	}
	return fmt.Errorf("freeze active (%s): envelope matches an active freeze scope", rec.ID)
}

// matchingFreeze loads active freeze decision records from the decisions
// store (mem claims) — never markdown/brain paths — and reports whether the
// envelope matches one.
func (s *Store) matchingFreeze(w envelope.WorkEnvelope) (bool, policy.FreezeRecord, error) {
	memStore, err := mem.Open(s.home)
	if err != nil {
		return false, policy.FreezeRecord{}, err
	}
	claims, err := memStore.AllCurrentOperationalDecisions()
	if err != nil {
		return false, policy.FreezeRecord{}, err
	}
	records := activeFreezeRecords(claims)
	matched, rec := policy.FreezeMatch(w.Owner, w.CanonicalSurface, w.EvidenceRefs, records)
	return matched, rec, nil
}

// activeFreezeRecords projects current decision claims into policy.FreezeRecord
// values: accepted kind=freeze claims not named by an authoritative current
// record's supersedes qualifier (undo = supersede, falsifier c). Legacy
// recorded freezes remain fail-safe active; only the new locked-import stamp
// may release them, so generic memory cannot forge an unfreeze.
func activeFreezeRecords(claims []mem.Claim) []policy.FreezeRecord {
	superseded := map[string]bool{}
	for _, c := range claims {
		if c.ClaimType != "decision" || !mem.HasOperationalDecisionAuthority(c.Qualifiers) || !policy.HasOperatorProvenance(c.Qualifiers["reviewed_by"]) {
			continue
		}
		for _, id := range splitFreezeCSV(c.Qualifiers[freezeDecisionSupersedesField]) {
			superseded[id] = true
		}
	}
	var out []policy.FreezeRecord
	for _, c := range claims {
		if c.ClaimType != "decision" {
			continue
		}
		q := c.Qualifiers
		if q["kind"] != freezeDecisionKind || q["lifecycle_status"] != freezeDecisionStatusAccepted || !policy.HasOperatorProvenance(q["reviewed_by"]) {
			continue
		}
		id := q["decision_id"]
		if id != "" && superseded[id] {
			continue
		}
		out = append(out, policy.FreezeRecord{
			ID:         id,
			Title:      c.Object,
			ReviewedBy: q["reviewed_by"],
			OwnerGlobs: splitFreezeCSV(q["freeze_owner_globs"]),
			Keywords:   splitFreezeCSV(q["freeze_keywords"]),
		})
	}
	return out
}

func splitFreezeCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// recordFreezeDeny writes the ActionIntent and a deny PolicyDecision for an
// autonomous admission blocked by an active freeze, plus a lifecycle event
// naming the freeze record, so the deny leaves an auditable trail while the
// work stays in place.
func (s *Store) recordFreezeDeny(w envelope.WorkEnvelope, actor, to string, rec policy.FreezeRecord, now time.Time) error {
	reason := fmt.Sprintf("freeze active (%s): %s matches an active freeze scope; operator must record a superseding decision to unfreeze", rec.ID, w.ID)
	ai := s.advanceActionIntent(w, actor, to, now, AdvancePlanOptions{Actor: actor, To: to})
	if err := ai.Validate(); err != nil {
		return err
	}
	pd := policy.FreezeDenyDecision(ai, reason, now)
	if err := pd.Validate(); err != nil {
		return err
	}
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		return err
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		return err
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       actor,
		Note:     "freeze deny: " + reason,
		Attached: []string{ai.ID, pd.ID},
	})
}
