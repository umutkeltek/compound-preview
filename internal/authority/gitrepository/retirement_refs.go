package gitrepository

import (
	"sort"
	"strings"
)

func WorktreeRetirementEvidenceRefs(plan WorktreeRetirementPlan) []string {
	refs := []string{
		"worktree-retirement-plan:" + plan.ID + ":" + plan.PlanDigest,
		"work:" + plan.OwnerProof.WorkID,
		"work:" + plan.SourceProof.WorkID,
		plan.SourceProof.CommitArtifactRef,
		plan.SourceProof.LandCheckEvidenceRef,
		plan.SourceProof.LandCheckFileDigest,
		"repo-fingerprint:" + plan.RepositoryFingerprint,
	}
	refs = append(refs, plan.RecoveryRefs...)
	return sortedRetirementRefs(refs)
}

func WorktreeRetirementApplyProofRefs(plan WorktreeRetirementPlan, report WorktreeRetirementReviewReport) []string {
	refs := WorktreeRetirementEvidenceRefs(plan)
	refs = append(refs, report.SourceDigest, report.AuthorizationDigest, "worktree-retirement-review:"+report.ID)
	return sortedRetirementRefs(refs)
}

func sortedRetirementRefs(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = true
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
