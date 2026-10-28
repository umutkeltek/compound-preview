package work

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/mem"
)

// meaningResolutionStatus is the private discriminator for promoted meaning.
// The production resolver remains package-private; only CreatePlan carries it out.
// resolution. The existing call sites collapse "no eligible directive" and "more
// than one eligible directive" into the same nil result; the discriminator reports
// all three outcomes so the intent coordinator can park ambiguity instead of
// silently picking one.
type meaningResolutionStatus string

const (
	meaningNone      meaningResolutionStatus = "none"
	meaningAmbiguous meaningResolutionStatus = "ambiguous"
	meaningResolved  meaningResolutionStatus = "resolved"
)

// meaningResolution is the private typed outcome of promoted meaning resolution.
// It carries the resolved WorkActionContext only when Status is meaningResolved;
// AmbiguousRefs lists every recorded decision ref that cleared the same eligibility
// filter under the same ContextPack, deterministically ordered, so the intent
// coordinator can park a named question across them.
type meaningResolution struct {
	Status        meaningResolutionStatus
	Context       *WorkActionContext
	AmbiguousRefs []string
}

// promotedMeaningActionContext keeps
// the existing semantics (no eligible directive → nil/none; unresolved revision →
// none; ambiguous → ambiguous with refs) but returns the full discriminator so the
// intent coordinator can park ambiguity without re-reading the ContextPack.
// Existing create-plan callers still consume only the resolved context through
// createPlanForWork; the discriminator remains private to this package.
func (s *Store) promotedMeaningActionContext(contextPackID string) (meaningResolution, error) {
	contextPackID = strings.TrimSpace(contextPackID)
	if contextPackID == "" {
		return meaningResolution{Status: meaningNone}, nil
	}

	var pack envelope.ContextPack
	if err := s.readJSON(filepath.Join(s.home, "context", "packs", contextPackID+".json"), &pack); err != nil {
		return meaningResolution{}, fmt.Errorf("read ContextPack %s for promoted meaning: %w", contextPackID, err)
	}
	if err := pack.Validate(); err != nil {
		return meaningResolution{}, fmt.Errorf("validate ContextPack %s for promoted meaning: %w", contextPackID, err)
	}
	active := make([]string, 0, len(pack.ActiveDecisions))
	for _, ref := range pack.ActiveDecisions {
		if ref = strings.TrimSpace(ref); ref != "" {
			active = append(active, ref)
		}
	}
	if len(active) == 0 {
		return meaningResolution{Status: meaningNone}, nil
	}

	memory, err := mem.Open(s.home)
	if err != nil {
		return meaningResolution{}, fmt.Errorf("open promoted meaning authority: %w", err)
	}
	claims, err := memory.AllCurrent()
	if err != nil {
		return meaningResolution{}, fmt.Errorf("read promoted meaning authority: %w", err)
	}
	currentByRef := currentActiveMeaningDecisions(claims)

	type candidate struct {
		ref   string
		claim mem.Claim
	}
	var eligible []candidate
	for _, ref := range active {
		claimID, err := recordedDecisionClaimID(pack, ref)
		if err != nil {
			return meaningResolution{}, err
		}
		claim, ok := currentByRef[ref]
		if !ok || claim.ID != claimID {
			// A recorded decision revision that is no longer current cannot steer
			// newly-created work. Work already admitted from it keeps its snapshot.
			continue
		}
		if actionDirectiveDecisionRef(claim) != ref || !eligibleActionDirective(claim) {
			continue
		}
		eligible = append(eligible, candidate{ref: ref, claim: claim})
	}
	if len(eligible) == 0 {
		return meaningResolution{Status: meaningNone}, nil
	}
	if len(eligible) > 1 {
		refs := make([]string, 0, len(eligible))
		for _, e := range eligible {
			refs = append(refs, e.ref)
		}
		return meaningResolution{Status: meaningAmbiguous, AmbiguousRefs: uniqueMeaningRefs(refs...)}, nil
	}
	matched := eligible[0]
	decisionID := strings.TrimPrefix(matched.ref, "decision://")
	evidenceRefs := uniqueMeaningRefs(append([]string{matched.ref, "claim://" + matched.claim.ID}, matched.claim.EvidenceRefs...)...)
	return meaningResolution{
		Status: meaningResolved,
		Context: &WorkActionContext{
			Kind:          "promoted_action_directive",
			SourceRef:     matched.ref,
			SourceID:      decisionID,
			Title:         "Promoted action directive",
			ActionSummary: strings.TrimSpace(matched.claim.Object),
			RequiredInputs: []string{
				"recorded ContextPack " + contextPackID,
				"current promoted decision " + matched.ref,
			},
			ExpectedValidation: []string{
				"a future Receipt and Transition prove the WorkEnvelope state delta",
			},
			StopConditions: []string{
				"the directive is absent from the recorded ContextPack",
				"the directive is no longer a current promoted claim",
				"the action would leave the WorkEnvelope policy grant",
			},
			Boundary:     "propose-only; promoted meaning selects work context but does not execute, admit work, or bypass PolicyDecision",
			Proof:        "the recorded create plan preserves decision and claim evidence refs; Receipt and Transition prove execution",
			EvidenceRefs: evidenceRefs,
		},
	}, nil
}

// currentActiveMeaningDecisions mirrors the accepted/superseded decision wire
// contract without importing internal/decisions (which would create the
// decisions -> writeback -> work import cycle). If multiple accepted claims
// carry one decision id, the newest ledger revision is the current one, matching
// ContextPack construction.
func currentActiveMeaningDecisions(claims []mem.Claim) map[string]mem.Claim {
	superseded := map[string]bool{}
	for _, claim := range claims {
		if claim.ClaimType != "decision" || claim.Qualifiers == nil ||
			strings.ToLower(strings.TrimSpace(claim.Qualifiers["lifecycle_status"])) != "accepted" {
			continue
		}
		for _, id := range strings.Split(claim.Qualifiers["supersedes"], ",") {
			if id = strings.TrimSpace(id); id != "" {
				superseded[id] = true
			}
		}
	}
	byRef := map[string]mem.Claim{}
	for _, claim := range claims {
		if claim.ClaimType != "decision" || claim.Qualifiers == nil ||
			strings.ToLower(strings.TrimSpace(claim.Qualifiers["lifecycle_status"])) != "accepted" {
			continue
		}
		decisionID := strings.TrimSpace(claim.Qualifiers["decision_id"])
		if decisionID == "" || superseded[decisionID] {
			continue
		}
		ref := "decision://" + decisionID
		if prior, ok := byRef[ref]; !ok || claim.Seq > prior.Seq || (claim.Seq == prior.Seq && claim.ID > prior.ID) {
			byRef[ref] = claim
		}
	}
	return byRef
}

func eligibleActionDirective(claim mem.Claim) bool {
	if claim.ClaimType != "decision" || claim.Visibility != "operational" || claim.Qualifiers == nil ||
		strings.ToLower(strings.TrimSpace(claim.Qualifiers["lifecycle_status"])) != "accepted" ||
		mem.NormalizeClaimValue(claim.Predicate) != "action_directive" || strings.TrimSpace(claim.Object) == "" {
		return false
	}
	switch claim.PromotionBasis {
	case candidates.BasisExplicitConfirmation, candidates.BasisDecisionRecord, candidates.BasisRuntimeProof:
		return true
	default:
		return false
	}
}

func recordedDecisionClaimID(pack envelope.ContextPack, decisionRef string) (string, error) {
	var claimID string
	for _, revision := range pack.SourceRevisions {
		if strings.TrimSpace(revision.Source) != decisionRef {
			continue
		}
		rev := strings.TrimSpace(revision.Rev)
		if !strings.HasPrefix(rev, "claim:") || strings.TrimSpace(strings.TrimPrefix(rev, "claim:")) == "" {
			return "", fmt.Errorf("ContextPack %s decision %s has invalid claim revision %q", pack.ID, decisionRef, rev)
		}
		candidateID := strings.TrimSpace(strings.TrimPrefix(rev, "claim:"))
		if claimID != "" && claimID != candidateID {
			return "", fmt.Errorf("ContextPack %s decision %s has conflicting claim revisions", pack.ID, decisionRef)
		}
		claimID = candidateID
	}
	if claimID == "" {
		return "", fmt.Errorf("ContextPack %s decision %s is missing its exact claim revision", pack.ID, decisionRef)
	}
	return claimID, nil
}

func actionDirectiveDecisionRef(claim mem.Claim) string {
	decisionID := strings.TrimSpace(claim.Qualifiers["decision_id"])
	if decisionID == "" {
		return ""
	}
	return "decision://" + decisionID
}

func uniqueMeaningRefs(values ...string) []string {
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
	sort.Strings(out)
	return out
}
