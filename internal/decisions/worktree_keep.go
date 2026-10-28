package decisions

// worktree_keep is the fail-closed reader for worktree-keep decision records
// (worktree-tracking slice 2). A keep is a KindGrant record whose scope is
// WorktreeKeepScope; it pins a worktree branch against retirement cleanup.
//
// This reader mirrors activeAutonomyGrantClasses (grant_mint.go) exactly:
//   - mem.Open(homePath) -> AllCurrentOperationalDecisions -> AuthoritativeActiveDecisions(KindGrant)
//   - the bool is false ONLY when records could not be read at all (fail closed:
//     a caller cannot prove the absence of a keep it could not read, so it must
//     block retirement, never treat unreadable as "no keeps").
//   - a record without operator ReviewedBy provenance is inert (the no-self-keep
//     law: an agent-authored keep exists on disk but is NOT an active keep).
//
// It is read-only: it mutates nothing and promotes no claim.

import (
	"strings"

	"compound/internal/mem"
)

// WorktreeKeep is one active operator keep, keyed in the returned map by its
// protected branch ref (refs/heads/<name>).
type WorktreeKeep struct {
	DecisionID string
	BranchRef  string
	Reason     string
	Revisit    string
	ReviewedBy string
}

// ActiveWorktreeKeeps returns the active operator-reviewed worktree-keep records
// keyed by protected branch ref. The bool is false only when the decision
// records could not be read at all — callers MUST fail closed on false (block
// retirement / report keeps unreadable), never interpret it as "no keeps".
func ActiveWorktreeKeeps(homePath string) (map[string]WorktreeKeep, bool) {
	store, err := mem.Open(strings.TrimSpace(homePath))
	if err != nil {
		return nil, false
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		return nil, false
	}
	return worktreeKeepsFromClaims(claims), true
}

// worktreeKeepsFromClaims is the pure projection used by ActiveWorktreeKeeps.
// Keeping it pure makes the exclusion rules (scope, operator provenance, valid
// branch ref) unit-testable without a real store.
func worktreeKeepsFromClaims(claims []mem.Claim) map[string]WorktreeKeep {
	keeps := map[string]WorktreeKeep{}
	// AuthoritativeActiveDecisions already requires the protected authority stamp
	// AND operator provenance; the explicit ReviewedBy re-check below mirrors the
	// autonomy-grant reader belt-and-suspenders so the no-self-keep law is visible
	// at this seam and survives an upstream relaxation.
	for _, claim := range AuthoritativeActiveDecisions(claims, KindGrant) {
		lc := LifecycleFromClaim(claim)
		if lc.Scope != WorktreeKeepScope {
			continue
		}
		branchRef := strings.TrimSpace(lc.WorktreeBranchRef)
		if !strings.HasPrefix(branchRef, "refs/heads/") {
			continue
		}
		if strings.TrimSpace(lc.ReviewedBy) == "" {
			continue // no operator provenance ⇒ inert (no self-keep)
		}
		keeps[branchRef] = WorktreeKeep{
			DecisionID: lc.ID,
			BranchRef:  branchRef,
			Reason:     lc.Choice,
			Revisit:    lc.ReviewTrigger,
			ReviewedBy: lc.ReviewedBy,
		}
	}
	return keeps
}
