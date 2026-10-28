package director

// Director-apply grant consumption (grants-as-gates, 2026-07-10): director
// --apply now requires an active operator grant decision record (kind=grant,
// status=accepted, scope=director-apply, reviewed_by present) before it
// files anything. That broad grant authorizes filing only. A separate,
// class-scoped autonomy grant carries explicit sample/window/failure
// thresholds and is sealed into a content-addressed eligibility artifact;
// neither grant can bypass the kill-switch or operator-only risk frontier.
//
// The read path is decisions.ActiveDecisions over mem.Store claims ONLY —
// never markdown, worklog, or brain prose (falsifier e): a worklog line or a
// brain paragraph ratifying a grant binds nothing here.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/decisions"
	"compound/internal/mem"
	"compound/internal/policy"
	autonomyproof "compound/internal/routeevidence/autonomy"
)

// directorApplyGrantScope is the fixed scope director --apply resolves.
const directorApplyGrantScope = "director-apply"

// resolvedApplyGrant is the active broad director-apply grant
// Propose(apply=true) resolves before filing anything. It never authorizes
// proposed -> admitted.
type resolvedApplyGrant struct {
	DecisionID string
	ClaimID    string
	ReviewedBy string
	GrantRef   string
}

type resolvedAutonomyGrant struct {
	Proof autonomyproof.GrantSnapshot
}

// HasActiveAutonomyGrant reports whether a complete, safe, operator-reviewed
// autonomy-evidence-threshold grant is active for the given auto-admit class.
// It wraps the same resolveAutonomyGrant authority the job_health evidence
// slice uses, so callers outside the director (e.g. the experiential-smoke
// circuit deciding whether it may autonomously file ux_friction fix-work) can
// gate on a real active grant without re-deriving the match semantics or
// touching markdown/prose. A missing grant returns (false, nil); a malformed
// or ambiguous grant returns a non-nil error.
func HasActiveAutonomyGrant(homePath, class string) (bool, error) {
	_, ok, err := resolveAutonomyGrant(homePath, class)
	return ok, err
}

// resolveAutonomyGrant is deliberately stricter than resolveApplyGrant. The
// broad director-apply grant authorizes filing suggestions; this grant may
// move exactly one class across proposed -> admitted, so zero or multiple
// active records are both non-authority. Thresholds must be complete and
// safe before any evidence compiler sees them.
func resolveAutonomyGrant(homePath, class string) (resolvedAutonomyGrant, bool, error) {
	store, err := mem.Open(homePath)
	if err != nil {
		return resolvedAutonomyGrant{}, false, err
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		return resolvedAutonomyGrant{}, false, err
	}
	class = strings.TrimSpace(class)
	var matches []mem.Claim
	for _, claim := range decisions.AuthoritativeActiveDecisions(claims, decisions.KindGrant) {
		lifecycle := decisions.LifecycleFromClaim(claim)
		if lifecycle.Scope != decisions.AutonomyGrantScope || lifecycle.AutoAdmitClass != class {
			continue
		}
		if strings.TrimSpace(lifecycle.ReviewedBy) == "" {
			return resolvedAutonomyGrant{}, false, fmt.Errorf("autonomy grant %q has no reviewed_by provenance", lifecycle.ID)
		}
		if lifecycle.MinTerminalSamples <= 0 || lifecycle.MinWindowDays <= 0 || lifecycle.MaxFailures < 0 || lifecycle.MaxFailures >= lifecycle.MinTerminalSamples {
			return resolvedAutonomyGrant{}, false, fmt.Errorf("autonomy grant %q has an incomplete or unsafe threshold", lifecycle.ID)
		}
		matches = append(matches, claim)
	}
	if len(matches) == 0 {
		return resolvedAutonomyGrant{}, false, nil
	}
	if len(matches) != 1 {
		return resolvedAutonomyGrant{}, false, fmt.Errorf("autonomy grant for class %q is ambiguous: %d active records", class, len(matches))
	}
	claim := matches[0]
	lifecycle := decisions.LifecycleFromClaim(claim)
	return resolvedAutonomyGrant{Proof: autonomyproof.GrantSnapshot{
		DecisionID:         lifecycle.ID,
		ClaimID:            claim.ID,
		GrantRef:           "grant:" + lifecycle.ID,
		ClaimRef:           "claim:" + claim.ID,
		ReviewedBy:         lifecycle.ReviewedBy,
		Class:              lifecycle.AutoAdmitClass,
		MinTerminalSamples: lifecycle.MinTerminalSamples,
		MinWindowDays:      lifecycle.MinWindowDays,
		MaxFailures:        lifecycle.MaxFailures,
	}}, true, nil
}

// resolveApplyGrant reads ONLY decision records (mem claims) for an active
// grant scoped to director-apply. No matching record => a named refusal;
// more than one => the most recently promoted wins (deterministic by claim
// seq), matching decisions' own "latest edit wins" semantics elsewhere.
func resolveApplyGrant(homePath string, now time.Time) (resolvedApplyGrant, error) {
	store, err := mem.Open(homePath)
	if err != nil {
		return resolvedApplyGrant{}, err
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		return resolvedApplyGrant{}, err
	}
	active := decisions.ActiveDecisions(claims, decisions.KindGrant)
	var candidates []mem.Claim
	for _, c := range active {
		l := decisions.LifecycleFromClaim(c)
		if l.Scope != directorApplyGrantScope {
			continue
		}
		if !policy.HasOperatorProvenance(l.ReviewedBy) {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return resolvedApplyGrant{}, fmt.Errorf(
			"director --apply refused: no active grant record for scope %q "+
				"(record one: `compound decisions record --kind grant --scope %s --reviewed-by <operator>`)",
			directorApplyGrantScope, directorApplyGrantScope)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Seq < candidates[j].Seq })
	latest := candidates[len(candidates)-1]
	l := decisions.LifecycleFromClaim(latest)
	return resolvedApplyGrant{
		DecisionID: l.ID,
		ClaimID:    latest.ID,
		ReviewedBy: l.ReviewedBy,
		GrantRef:   "grant:" + l.ID,
	}, nil
}
