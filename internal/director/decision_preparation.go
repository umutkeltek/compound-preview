package director

import (
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

// decisionPreparationStaleAfter is the default age a needs-you review group
// must exceed before the director proposes preparing a decision brief for it.
// "Older than" is STRICT (see evaluateBriefableGroup): a group whose freshest
// member sits exactly at the threshold is not yet stale. Two reasons: it matches
// the operator's "sitting past a threshold" intent, and it is load-bearing —
// the existing class-fairness fixtures seed cards exactly at the 2h mark, so a
// non-strict comparison would inject spurious decision_preparation candidates
// into those runs.
const decisionPreparationStaleAfter = 2 * time.Hour

// decisionPreparationCandidates is the director's decision_preparation scanner
// (preparation flow, slice B). It reads the generic review-queue projection and
// proposes — propose-only — one "prepare a decision brief" WorkEnvelope per
// needs-you group that has (a) been sitting longer than the threshold, (b) no
// wired one-click action, and (c) no decision-brief sidecar on any member yet.
// The work chain each envelope points at is the existing `compound reviews
// brief-plan --item <id>` instruction packet (slice A). Silence is the default:
// a home with no briefable needs-you group yields zero candidates.
func decisionPreparationCandidates(homePath string, now time.Time) ([]candidate, error) {
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{}, now)
	if err != nil {
		return nil, err
	}
	return briefableCandidates(report, now, decisionPreparationStaleAfter), nil
}

// briefableCandidates is the pure core over an already-built queue projection so
// the trigger logic is unit-testable without touching disk. It emits one
// decision_preparation candidate per eligible group, in the queue's own group
// order (the director's class-fair round-robin re-sorts within the class).
func briefableCandidates(report reviewartifact.QueueReport, now time.Time, staleAfter time.Duration) []candidate {
	itemByAreaID := make(map[string]reviewartifact.QueueItem, len(report.Items))
	for _, item := range report.Items {
		itemByAreaID[item.Area+"\x00"+item.ID] = item
	}
	var out []candidate
	for _, group := range report.Groups {
		info, ok := evaluateBriefableGroup(group, itemByAreaID, now, staleAfter)
		if !ok {
			continue
		}
		out = append(out, decisionPreparationCandidate(info))
	}
	return out
}

// briefableGroup is the resolved, deterministic description of one eligible
// needs-you group: enough to mint a stable trigger, cite evidence, and phrase
// the operator card. Every field is stable across runs for an unchanged group —
// no elapsed-time value leaks in (the age anchor is the freshest member's
// created_at TIMESTAMP, not a live duration), so the director trigger ref stays
// stable and open-work / prior-proposal dedupe holds across runs.
type briefableGroup struct {
	Area           string
	Key            string
	ContextLabel   string
	MemberIDs      []string
	StaleSince     string
	NoActionReason string
	EvidenceRefs   []string
}

// evaluateBriefableGroup applies the three deterministic triggers to one group
// and returns its resolved description when all pass:
//
//	band     — only needs-you groups (the operator's own decision surface).
//	no act   — no safe batch action AND no per-member action; nothing the
//	           operator can already one-click (the console renders copy-mode).
//	no brief — no member carries a decision-brief sidecar yet; an already-briefed
//	           group is evidence, not another envelope.
//	stale    — the freshest member is STRICTLY older than staleAfter. A group
//	           whose age is unknown (no parseable timestamp) is never eligible.
func evaluateBriefableGroup(group reviewartifact.QueueGroup, itemByAreaID map[string]reviewartifact.QueueItem, now time.Time, staleAfter time.Duration) (briefableGroup, bool) {
	if group.Band != reviewartifact.BandNeedsYou {
		return briefableGroup{}, false
	}
	members := make([]reviewartifact.QueueItem, 0, len(group.ItemIDs))
	for _, id := range group.ItemIDs {
		if item, ok := itemByAreaID[group.Area+"\x00"+id]; ok {
			members = append(members, item)
		}
	}
	if len(members) == 0 {
		return briefableGroup{}, false
	}
	if groupHasWiredAction(group, members) {
		return briefableGroup{}, false
	}
	for _, m := range members {
		if m.Brief != nil {
			return briefableGroup{}, false
		}
	}
	freshest, ok := freshestCreatedAt(members)
	if !ok || now.Sub(freshest) <= staleAfter {
		return briefableGroup{}, false
	}

	memberIDs := make([]string, 0, len(members))
	for _, m := range members {
		memberIDs = append(memberIDs, m.ID)
	}
	sort.Strings(memberIDs)
	memberIDs = reviewartifact.Unique(memberIDs)

	noAction := firstNonEmpty(group.TerminalReason, "owner has not wired a one-click action for this group")
	staleSince := freshest.UTC().Format(time.RFC3339)

	refs := []string{
		"review_group:" + group.Area + ":" + group.Key,
		"decision_preparation_stale_since:" + staleSince,
		"decision_preparation_threshold:" + staleAfter.String(),
		"decision_preparation_no_action:" + slug(noAction),
	}
	for _, id := range memberIDs {
		refs = append(refs, "review:"+reviewRefForAreaID(group.Area, id))
	}
	refs = append(refs, group.EvidenceRefs...)

	return briefableGroup{
		Area:           group.Area,
		Key:            group.Key,
		ContextLabel:   firstNonEmpty(group.ContextLabel, group.Area, "reviews"),
		MemberIDs:      memberIDs,
		StaleSince:     staleSince,
		NoActionReason: noAction,
		EvidenceRefs:   compactRefs(refs),
	}, true
}

// groupHasWiredAction reports whether the operator already has a one-click act
// on this group — a safe batch action, or any per-member action. Both are
// checked because the console's copy-mode fallback (which this class targets)
// only appears when NEITHER exists.
func groupHasWiredAction(group reviewartifact.QueueGroup, members []reviewartifact.QueueItem) bool {
	if group.SafeBatchAction != nil {
		return true
	}
	for _, m := range members {
		if len(m.Actions) > 0 {
			return true
		}
	}
	return false
}

// freshestCreatedAt returns the most recent parseable created_at across a
// group's members — the group's age anchor. A group is only stale when even its
// freshest member has been sitting past the threshold, which prevents
// re-proposing preparation the moment a fresh member joins an old group.
func freshestCreatedAt(members []reviewartifact.QueueItem) (time.Time, bool) {
	var freshest time.Time
	found := false
	for _, m := range members {
		ts := strings.TrimSpace(m.CreatedAt)
		if ts == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if !found || parsed.After(freshest) {
			freshest = parsed
			found = true
		}
	}
	return freshest, found
}

func reviewRefForAreaID(area, id string) string {
	if strings.TrimSpace(area) == "" {
		return id
	}
	return area + "/" + id
}

// decisionPreparationCandidate turns a resolved briefable group into a
// propose-only director candidate. CapabilityClass is a preparation class
// (classify-extract) so the dispatch-plan lane router ranks preparation lanes by
// fit/speed/cost instead of hardcoding an executor. The candidate never carries
// a TargetSurface — this is not a harness edit — so it inherits the standard
// propose-only non-goals and the suggestion-only write allowlist.
func decisionPreparationCandidate(info briefableGroup) candidate {
	consequence := "Admitting this prepares a decision brief so this card arrives translated before you next look at it"
	memberList := strings.Join(info.MemberIDs, ", ")
	planItem := info.MemberIDs[0]
	groupRef := info.Area + "/" + info.Key

	return candidate{
		Kind:             "decision_preparation",
		SourceID:         info.Area + ":" + info.Key,
		Title:            "Prepare a decision brief for " + info.ContextLabel,
		Consequence:      consequence,
		CapabilityClass:  "classify-extract",
		Owner:            "compound-reviews",
		CanonicalSurface: "director:decision-preparation:" + info.Area + ":" + info.Key,
		SuccessPredicate: "Prepare a compound.decision_brief.v0.1 sidecar for each member of needs-you group " + groupRef + " via `compound reviews brief-plan`, without resolving, applying, or mutating any underlying review artifact",
		DesiredOutcome:   consequence,
		Command:          []string{"compound", "reviews", "brief-plan", "--item", planItem, "--json"},
		RequiredInputs: []string{
			"needs-you group " + groupRef + " member ids: " + memberList,
			"run `compound reviews brief-plan --item <member-id> --json` for EACH member to get its exact write path, evidence refs, and the compound.decision_brief.v0.1 schema",
			"the decision-brief schema compound.decision_brief.v0.1 (headline, findings[] each with an evidence_ref, options[], recommendation, prepared_by)",
		},
		ExpectedValidation: []string{
			"`compound reviews --json` shows a valid <id>.brief.json sidecar for every member of " + groupRef,
			"the underlying artifacts' status and band are unchanged — the brief is propose-only",
		},
		StopConditions: []string{
			"a member already carries a valid decision brief",
			"the group gained a wired one-click action",
			"an underlying artifact was resolved, archived, or superseded",
		},
		Boundary:     "prepare propose-only decision-brief sidecars only; never resolve, apply, adjudicate, or mutate any underlying review artifact, its status, or its band",
		Proof:        "a valid compound.decision_brief.v0.1 sidecar exists for each member of " + groupRef + ", and the console card renders the translation with a 'prepared by <lane>' provenance chip",
		EvidenceRefs: info.EvidenceRefs,
		CreatedAt:    info.StaleSince,
	}
}
