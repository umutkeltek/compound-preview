package authority

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"compound/internal/work"
)

// LaunchdSourceReattestation binds a historical source to its explicitly
// supplied current successor proof; the transition requires exact review.
type LaunchdSourceReattestation struct {
	Historical LaunchdSourceProof `json:"historical"`
	Current    LaunchdSourceProof `json:"current"`
}

// LaunchdTerminalHandoff binds a successful predecessor's immutable native
// outputs and authenticated terminal close. It is part of the successor's exact
// authorization digest, never an implicit permission to take another owner.
type LaunchdTerminalHandoff struct {
	FromPlanID           string                       `json:"from_plan_id"`
	PlanDigest           string                       `json:"plan_digest"`
	CloseProof           work.TerminalCloseProof      `json:"close_proof"`
	TerminalSHA256       string                       `json:"terminal_sha256"`
	PhaseChainSHA256     string                       `json:"phase_chain_sha256"`
	Owner                []byte                       `json:"owner"`
	SourceReattestations []LaunchdSourceReattestation `json:"source_reattestations,omitempty"`
}

// LaunchdNoEffectContinuation admits one new observation, not an effect retry.
// It binds all failed native observations that remain immutable before it.
type LaunchdNoEffectContinuation struct {
	Index        int    `json:"index"`
	PrefixSHA256 string `json:"prefix_sha256"`
}

func launchdNoEffectPrefixDigest(home string, p LaunchdRepairPlan, prior []launchdPhaseCommit) string {
	var forward launchdPhaseCommit
	// Every caller first authenticates launchdUnchangedHandoff.
	if readLaunchdArtifact(home, launchdPhaseStem(p, "apply", 3)+".committed.json", &forward) != nil {
		return ""
	}
	return launchdDigest(struct {
		PlanID   string
		Forward  launchdPhaseCommit
		Rollback []launchdPhaseCommit
	}{p.ID, forward, prior})
}

func launchdContinuationContainsPrefix(home string, p LaunchdRepairPlan, req LaunchdReviewRequest, prior []launchdPhaseCommit) bool {
	if len(prior) == 0 || req.BeforeWork.ID != p.WorkID || req.BeforeWork.State != "running" {
		return false
	}
	var last launchdPhaseStart
	if readLaunchdArtifact(home, launchdPhaseStem(p, "rollback", len(prior)-1)+".started.json", &last) != nil {
		return false
	}
	for _, id := range req.BeforeWork.ActionIntentIDs {
		if id == last.Pending.Action.ID {
			return true
		}
	}
	return false
}

// Only exact phase journals and the owner's review artifacts are recognized.
// A gap, pending prefix, unknown suffix, or later phase cannot be skipped.
func launchdNoEffectJournalCount(home string, p LaunchdRepairPlan, limit int) (int, error) {
	path, err := launchdArtifactPath(home, p.ID+".plan.json")
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	indices := map[int]bool{}
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(entry.Name(), p.ID+".rollback.")
		if !ok {
			continue
		}
		if rest == "terminal.json" {
			if limit < 0 {
				return 0, fmt.Errorf("no-effect rollback already has terminal evidence")
			}
			continue
		}
		parts := strings.Split(rest, ".")
		if len(parts) == 3 && parts[0] == "exact-v2" && (parts[1] == "review-request" || parts[1] == "review-proposal" || parts[1] == "review-intent") && parts[2] == "json" {
			continue
		}
		if len(parts) == 4 && strings.HasPrefix(parts[0], "no-effect-") && len(strings.TrimPrefix(parts[0], "no-effect-")) == 24 && parts[1] == "exact-v2" && (parts[2] == "review-request" || parts[2] == "review-proposal" || parts[2] == "review-intent") && parts[3] == "json" {
			continue
		}
		i, e := strconv.Atoi(parts[0])
		if e != nil || i < 0 || strconv.Itoa(i) != parts[0] || len(parts) != 3 || (parts[1] != "started" && parts[1] != "outcome" && parts[1] != "committed") || parts[2] != "json" || (limit >= 0 && i > limit) {
			return 0, fmt.Errorf("unknown or later no-effect rollback journal")
		}
		indices[i] = true
	}
	if limit >= 0 {
		return limit, nil
	}
	count := 0
	for indices[count] {
		count++
	}
	if count == 0 || count != len(indices) {
		return 0, fmt.Errorf("no-effect rollback prefix is missing or noncontiguous")
	}
	return count, nil
}

func readLaunchdNoEffectRollbackPhase(home string, p LaunchdRepairPlan, index int, prior []launchdPhaseCommit) (launchdPhaseCommit, error) {
	var zero launchdPhaseCommit
	stem := launchdPhaseStem(p, "rollback", index)
	var start launchdPhaseStart
	var outcome launchdPhaseOutcome
	var commit launchdPhaseCommit
	if readLaunchdArtifact(home, stem+".started.json", &start) != nil || readLaunchdArtifact(home, stem+".outcome.json", &outcome) != nil || readLaunchdArtifact(home, stem+".committed.json", &commit) != nil {
		return zero, fmt.Errorf("incomplete no-effect rollback prefix")
	}
	if start.Direction != "rollback" || start.Index != index || start.Name != "restore_owner" || !bytesEqual(start.BeforeOwner, p.Handoff.Owner) || !bytesEqual(start.BeforeBytes, p.Original) || outcome.Final.Changed || outcome.OwnerSHA256 != launchdBytesDigest(p.Handoff.Owner) || outcome.FileSHA256 != launchdBytesDigest(p.Original) || launchdStateMatches(p, p.Original, outcome.Runtime, p.Original, "disabled", "absent") != nil || !reflect.DeepEqual(outcome.Runtime.ForeignOverrides, p.Runtime.ForeignOverrides) {
		return zero, fmt.Errorf("rollback prefix is not an exact unchanged-owner observation")
	}
	if err := verifyLaunchdCommittedPhase(home, p, start, outcome, commit); err != nil {
		return zero, err
	}
	var review LaunchdRepairReview
	if readLaunchdArtifact(home, start.ReviewID+".review.json", &review) != nil || review.ID != start.ReviewID || validateLaunchdReviewEvidence(home, p, review) != nil || review.Request.Purpose != "rollback" || launchdWorkDigest(start.BeforeWork) != review.WorkSHA256 {
		return zero, fmt.Errorf("no-effect rollback prefix lacks authentic consumed review")
	}
	if index == 0 {
		anchor, sha, err := launchdRollbackAnchor(home, p)
		if err != nil || review.Request.NoEffectContinuation != nil || review.Request.RollbackAnchor != anchor || launchdWorkDigest(review.Request.BeforeWork) != sha {
			return zero, fmt.Errorf("initial no-effect rollback lineage changed")
		}
	} else {
		c := review.Request.NoEffectContinuation
		if c == nil || c.Index != index || len(prior) != index || c.PrefixSHA256 != launchdNoEffectPrefixDigest(home, p, prior) || review.Request.RollbackAnchor != c.PrefixSHA256 || !launchdContinuationContainsPrefix(home, p, review.Request, prior) {
			return zero, fmt.Errorf("no-effect continuation does not bind exact failed prefix")
		}
	}
	return commit, nil
}

func launchdNoEffectPrefix(home string, p LaunchdRepairPlan, count int) ([]launchdPhaseCommit, error) {
	if err := launchdUnchangedHandoff(home, p); err != nil {
		return nil, err
	}
	n, err := launchdNoEffectJournalCount(home, p, count)
	if err != nil {
		return nil, err
	}
	var prior []launchdPhaseCommit
	for i := 0; i < n; i++ {
		commit, err := readLaunchdNoEffectRollbackPhase(home, p, i, prior)
		if err != nil {
			return nil, err
		}
		if commit.Outcome != "failed" {
			return nil, fmt.Errorf("only failed no-effect observations admit fresh continuation")
		}
		prior = append(prior, commit)
	}
	return prior, nil
}

func validateLaunchdNoEffectContinuation(home string, p LaunchdRepairPlan, review LaunchdRepairReview) ([]launchdPhaseCommit, error) {
	c := review.Request.NoEffectContinuation
	if c == nil || c.Index < 1 || p.Handoff == nil {
		return nil, fmt.Errorf("explicit no-effect continuation required")
	}
	prior, err := launchdNoEffectPrefix(home, p, c.Index)
	if err != nil {
		return nil, err
	}
	if len(prior) != c.Index || c.PrefixSHA256 != launchdNoEffectPrefixDigest(home, p, prior) || review.Request.RollbackAnchor != c.PrefixSHA256 || !launchdContinuationContainsPrefix(home, p, review.Request, prior) {
		return nil, fmt.Errorf("no-effect continuation checkpoint changed")
	}
	return prior, nil
}

func launchdActiveName(p LaunchdRepairPlan) string {
	return "active-" + launchdDigest(p.Domain + "/" + p.Label)[:24] + ".json"
}

func launchdOwnerBytes(planID string) []byte {
	b, _ := json.MarshalIndent(struct {
		PlanID string `json:"plan_id"`
	}{planID}, "", "  ")
	return b
}

func readLaunchdOwner(home string, p LaunchdRepairPlan) ([]byte, error) {
	path, err := launchdArtifactPath(home, launchdActiveName(p))
	if err != nil {
		return nil, err
	}
	b, _, err := launchdOwnedFile(path, p.UID)
	return b, err
}

// Existing planning keeps its disabled/unloaded precondition. The caller must
// quiesce through separately admitted authority; this builder never stops a job.
func BuildLaunchdRepairPlanWithHandoff(ctx context.Context, home string, env LaunchdEnvironment, workID, actor, plistPath, candidatePath string, proofs []LaunchdSourceProof, from string) (LaunchdRepairPlan, error) {
	p, err := BuildLaunchdRepairPlan(ctx, env, workID, actor, plistPath, candidatePath, proofs)
	if err != nil {
		return p, err
	}
	unlock, err := lockLaunchdLabel(home, p.Domain+"/"+p.Label)
	if err != nil {
		return p, err
	}
	defer unlock()
	b, runtime, err := launchdCurrent(ctx, env, p)
	if err != nil {
		return p, err
	}
	if err = launchdStateMatches(p, b, runtime, p.Original, "disabled", "absent"); err != nil {
		return p, err
	}
	handoff, err := captureLaunchdTerminalHandoff(home, p, from, false)
	if err != nil {
		return p, err
	}
	p.Handoff = &handoff
	p.ID, p.Digest, p.AuthorizationDigest = "", "", ""
	p.AuthorizationDigest = launchdDigest(p)
	p.Digest = launchdDigest(p)
	p.ID = "ldrp_" + p.Digest[:24]
	return p, nil
}

// Historical source bytes remain evidence of the completed predecessor, not a
// requirement to keep old executable versions installed forever. Any drift must
// be explicitly covered by the successor's current source proofs and sealed in
// its handoff, so the exact grant and consumed review authorize that transition.
func launchdHistoricalSourceReattestations(historical, successor []LaunchdSourceProof) ([]LaunchdSourceReattestation, error) {
	currentByPath := make(map[string]LaunchdSourceProof, len(successor))
	for _, proof := range successor {
		if !filepath.IsAbs(proof.Path) || filepath.Clean(proof.Path) != proof.Path {
			return nil, fmt.Errorf("canonical successor source proof path required")
		}
		if _, duplicate := currentByPath[proof.Path]; duplicate {
			return nil, fmt.Errorf("duplicate successor source proof path")
		}
		currentByPath[proof.Path] = proof
	}
	var changes []LaunchdSourceReattestation
	seen := make(map[string]bool, len(historical))
	for _, old := range historical {
		if !filepath.IsAbs(old.Path) || filepath.Clean(old.Path) != old.Path || seen[old.Path] {
			return nil, fmt.Errorf("unique canonical historical source proof path required")
		}
		seen[old.Path] = true
		current, err := inspectLaunchdSource(LaunchdSourceProof{Path: old.Path})
		if err != nil {
			return nil, err
		}
		if current == old {
			continue
		}
		if supplied, ok := currentByPath[old.Path]; !ok || supplied != current {
			return nil, fmt.Errorf("historical source drift requires exact current successor proof: %s", old.Path)
		}
		changes = append(changes, LaunchdSourceReattestation{Historical: old, Current: current})
	}
	return changes, nil
}

func captureLaunchdTerminalHandoff(home string, p LaunchdRepairPlan, from string, successorOwns bool) (LaunchdTerminalHandoff, error) {
	var h LaunchdTerminalHandoff
	old, err := loadLaunchdPlan(home, from)
	if err != nil {
		return h, err
	}
	if old.WorkID == p.WorkID || old.UserHome != p.UserHome || old.UID != p.UID || old.Domain != p.Domain || old.Label != p.Label || old.PlistPath != p.PlistPath || old.Mode != p.Mode || !bytesEqual(old.Candidate, p.Original) || !reflect.DeepEqual(old.CandidateArgs, p.OriginalArgs) {
		return h, fmt.Errorf("terminal handoff requires the exact predecessor candidate and a different work")
	}
	owner, err := readLaunchdOwner(home, p)
	expectedOwner := launchdOwnerBytes(from)
	if successorOwns {
		expectedOwner = launchdOwnerBytes(p.ID)
	}
	if err != nil || !bytesEqual(owner, expectedOwner) {
		return h, fmt.Errorf("terminal handoff predecessor is not the exact active owner")
	}
	current, mode, err := launchdOwnedFile(p.PlistPath, p.UID)
	if err != nil || uint32(mode) != p.Mode || !bytesEqual(current, p.Original) {
		return h, fmt.Errorf("terminal handoff current predecessor bytes or mode changed")
	}
	store, err := work.OpenReadOnly(home)
	if err != nil {
		return h, err
	}
	next, err := store.Get(p.WorkID)
	if err != nil || next.State != "running" {
		return h, fmt.Errorf("running successor work required for terminal handoff")
	}
	closed, err := store.ReadTerminalCloseProof(old.WorkID)
	if err != nil || closed.State != "completed" {
		return h, fmt.Errorf("authenticated completed predecessor work required: %v", err)
	}
	reattestations, err := launchdHistoricalSourceReattestations(old.SourceProofs, p.SourceProofs)
	if err != nil {
		return h, err
	}
	indices := []int{0, 1, 2}
	if old.Handoff != nil {
		indices = append([]int{3}, indices...)
	}
	allowed := map[string]bool{}
	var commits []launchdPhaseCommit
	for _, i := range indices {
		stem := launchdPhaseStem(old, "apply", i)
		var start launchdPhaseStart
		var outcome launchdPhaseOutcome
		var commit launchdPhaseCommit
		for _, item := range []struct {
			name  string
			value any
		}{{stem + ".started.json", &start}, {stem + ".outcome.json", &outcome}, {stem + ".committed.json", &commit}} {
			allowed[item.name] = true
			if err = readLaunchdArtifact(home, item.name, item.value); err != nil {
				return h, fmt.Errorf("incomplete predecessor phase: %w", err)
			}
		}
		name := map[int]string{0: "replace", 1: "enable", 2: "bootstrap", 3: "handoff"}[i]
		if start.Direction != "apply" || start.Index != i || start.Name != name || commit.Outcome != "completed" || outcome.Final.Outcome != "completed" {
			return h, fmt.Errorf("predecessor phase is not a completed owned apply")
		}
		if len(commits) > 0 && launchdWorkDigest(start.BeforeWork) != commits[len(commits)-1].WorkSHA256 {
			return h, fmt.Errorf("predecessor native phase chain changed")
		}
		if err = verifyLaunchdCommittedPhase(home, old, start, outcome, commit); err != nil {
			return h, err
		}
		var review LaunchdRepairReview
		if err = readLaunchdArtifact(home, start.ReviewID+".review.json", &review); err != nil {
			return h, err
		}
		if review.ID != start.ReviewID || validateLaunchdReviewEvidence(home, old, review) != nil {
			return h, fmt.Errorf("predecessor phase lacks authentic sealed review")
		}
		if len(commits) == 0 && launchdWorkDigest(start.BeforeWork) != review.WorkSHA256 {
			return h, fmt.Errorf("predecessor first phase does not follow its sealed review")
		}
		expectedBytes := old.Candidate
		if name == "handoff" {
			expectedBytes = old.Original
			if outcome.OwnerSHA256 != launchdBytesDigest(launchdOwnerBytes(old.ID)) || launchdStateMatches(old, expectedBytes, outcome.Runtime, expectedBytes, "disabled", "absent") != nil {
				return h, fmt.Errorf("predecessor ownership outcome is not proven")
			}
		} else if err = launchdPostcondition(old, name, expectedBytes, outcome.Runtime); err != nil {
			return h, err
		}
		if outcome.FileSHA256 != launchdBytesDigest(expectedBytes) {
			return h, fmt.Errorf("predecessor phase does not bind exact resulting bytes")
		}
		commits = append(commits, commit)
	}
	// A terminal success cannot hide a later partial rollback or extra phase.
	dir, err := launchdArtifactPath(home, old.ID+".apply.terminal.json")
	if err != nil {
		return h, err
	}
	entries, err := os.ReadDir(strings.TrimSuffix(dir, old.ID+".apply.terminal.json"))
	if err != nil {
		return h, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "ldrp_") && strings.HasSuffix(name, ".plan.json") && name != p.ID+".plan.json" {
			later, err := loadLaunchdPlan(home, strings.TrimSuffix(name, ".plan.json"))
			if err != nil {
				return h, err
			}
			if later.Handoff != nil && later.Handoff.FromPlanID == old.ID {
				if err = verifyLaunchdRestoredOwnership(home, later); err != nil {
					return h, err
				}
			}
		}
		journal := strings.HasSuffix(name, ".started.json") || strings.HasSuffix(name, ".outcome.json") || strings.HasSuffix(name, ".committed.json") || strings.HasSuffix(name, ".terminal.json")
		if (strings.HasPrefix(name, old.ID+".rollback.") && journal) || name == old.ID+".rollback-manifest.json" || (strings.HasPrefix(name, old.ID+".apply.") && journal && name != old.ID+".apply.terminal.json" && !allowed[name]) {
			return h, fmt.Errorf("ambiguous or later predecessor phase evidence")
		}
	}
	var terminal LaunchdRepairResult
	if err = readLaunchdArtifact(home, old.ID+".apply.terminal.json", &terminal); err != nil {
		return h, err
	}
	if terminal.PlanID != old.ID || terminal.Direction != "apply" || terminal.Outcome != "completed" || terminal.Failure != "" || !reflect.DeepEqual(terminal.Phases, commits) || terminal.Runtime.Error != "" || !reflect.DeepEqual(terminal.Runtime.ForeignOverrides, p.Runtime.ForeignOverrides) {
		return h, fmt.Errorf("sealed predecessor success does not bind the native phase chain")
	}
	if err = launchdStateMatches(old, old.Candidate, terminal.Runtime, old.Candidate, "enabled", "loaded"); err != nil {
		return h, err
	}
	terminalBytes, _, err := launchdOwnedFile(dir, p.UID)
	if err != nil {
		return h, err
	}
	return LaunchdTerminalHandoff{FromPlanID: from, PlanDigest: old.Digest, CloseProof: closed,
		TerminalSHA256: launchdBytesDigest(terminalBytes), PhaseChainSHA256: launchdDigest(commits), Owner: launchdOwnerBytes(from), SourceReattestations: reattestations}, nil
}

// An old pointer can become visible before a restoring CAS has its receipt.
// It becomes eligible again only after that successor's native rollback seals.
// Planned successors with no started handoff have never acquired ownership.
func verifyLaunchdRestoredOwnership(home string, p LaunchdRepairPlan) error {
	var first launchdPhaseStart
	err := readLaunchdArtifact(home, launchdPhaseStem(p, "apply", 3)+".started.json", &first)
	if os.IsNotExist(err) {
		path, pathErr := launchdArtifactPath(home, p.ID+".plan.json")
		if pathErr != nil {
			return pathErr
		}
		entries, readErr := os.ReadDir(filepath.Dir(path))
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			name := entry.Name()
			journal := strings.HasSuffix(name, ".started.json") || strings.HasSuffix(name, ".outcome.json") || strings.HasSuffix(name, ".committed.json") || strings.HasSuffix(name, ".terminal.json")
			if ((strings.HasPrefix(name, p.ID+".apply.") || strings.HasPrefix(name, p.ID+".rollback.")) && journal) || name == p.ID+".rollback-manifest.json" {
				return fmt.Errorf("later ownership journals remain without their handoff start")
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	stem := launchdPhaseStem(p, "apply", 3)
	var firstOutcome launchdPhaseOutcome
	var firstCommit launchdPhaseCommit
	if first.PlanID != p.ID || first.Direction != "apply" || first.Index != 3 || first.Name != "handoff" || readLaunchdArtifact(home, stem+".outcome.json", &firstOutcome) != nil || readLaunchdArtifact(home, stem+".committed.json", &firstCommit) != nil {
		return fmt.Errorf("later ownership handoff lacks complete native evidence")
	}
	if err = verifyLaunchdCommittedPhase(home, p, first, firstOutcome, firstCommit); err != nil {
		return err
	}
	var firstReview LaunchdRepairReview
	if readLaunchdArtifact(home, first.ReviewID+".review.json", &firstReview) != nil || validateLaunchdReviewEvidence(home, p, firstReview) != nil || firstReview.Request.Purpose != "apply" || launchdWorkDigest(first.BeforeWork) != firstReview.WorkSHA256 {
		return fmt.Errorf("later ownership handoff lacks authentic consumed apply review")
	}
	var terminal LaunchdRepairResult
	if err = readLaunchdArtifact(home, p.ID+".rollback.terminal.json", &terminal); err != nil {
		return fmt.Errorf("later ownership handoff requires proven terminal rollback: %w", err)
	}
	if terminal.PlanID != p.ID || terminal.Direction != "rollback" || terminal.Outcome != "completed" || terminal.Failure != "" || len(terminal.Phases) == 0 || launchdStateMatches(p, p.Original, terminal.Runtime, p.Original, "disabled", "absent") != nil {
		return fmt.Errorf("later ownership rollback is not a completed restoration")
	}
	for _, phase := range terminal.Phases {
		if phase.Outcome != "completed" {
			last := len(terminal.Phases) - 1
			prior, err := launchdNoEffectPrefix(home, p, last)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(prior, terminal.Phases[:last]) {
				return fmt.Errorf("terminal no-effect recovery prefix changed")
			}
			tail, err := readLaunchdNoEffectRollbackPhase(home, p, last, prior)
			if err != nil {
				return err
			}
			if tail.Outcome != "completed" || !reflect.DeepEqual(tail, terminal.Phases[last]) {
				return fmt.Errorf("no-effect recovery requires successful fresh observation after failed prefix")
			}
			return nil
		}
	}
	for i, commit := range terminal.Phases {
		stem := launchdPhaseStem(p, "rollback", i)
		var start launchdPhaseStart
		var outcome launchdPhaseOutcome
		var sealed launchdPhaseCommit
		if readLaunchdArtifact(home, stem+".started.json", &start) != nil || readLaunchdArtifact(home, stem+".outcome.json", &outcome) != nil || readLaunchdArtifact(home, stem+".committed.json", &sealed) != nil || !reflect.DeepEqual(sealed, commit) || commit.Outcome != "completed" || start.Direction != "rollback" || start.Index != i {
			return fmt.Errorf("later ownership rollback phase is incomplete")
		}
		if i > 0 && launchdWorkDigest(start.BeforeWork) != terminal.Phases[i-1].WorkSHA256 {
			return fmt.Errorf("later ownership rollback chain changed")
		}
		if err = verifyLaunchdCommittedPhase(home, p, start, outcome, commit); err != nil {
			return err
		}
		var review LaunchdRepairReview
		if readLaunchdArtifact(home, start.ReviewID+".review.json", &review) != nil || validateLaunchdReviewEvidence(home, p, review) != nil || review.Request.Purpose != "rollback" || (i == 0 && launchdWorkDigest(start.BeforeWork) != review.WorkSHA256) {
			return fmt.Errorf("later ownership restoration lacks authentic rollback review")
		}
		if i == len(terminal.Phases)-1 && (start.Name != "restore_owner" || outcome.OwnerSHA256 != launchdBytesDigest(p.Handoff.Owner) || outcome.FileSHA256 != launchdBytesDigest(p.Original) || launchdStateMatches(p, p.Original, outcome.Runtime, p.Original, "disabled", "absent") != nil) {
			return fmt.Errorf("later ownership rollback did not receipt exact predecessor restoration")
		}
	}
	return nil
}

func validateLaunchdHandoff(home string, p LaunchdRepairPlan) error {
	if p.Handoff == nil {
		return fmt.Errorf("explicit terminal predecessor required")
	}
	current, err := captureLaunchdTerminalHandoff(home, p, p.Handoff.FromPlanID, false)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, *p.Handoff) {
		return fmt.Errorf("terminal predecessor evidence changed after review")
	}
	return nil
}

// A failed handoff can truthfully leave the predecessor unchanged. A fresh
// rollback may receipt that no-effect state; it never relabels the failure.
func launchdUnchangedHandoff(home string, p LaunchdRepairPlan) error {
	if p.Handoff == nil {
		return fmt.Errorf("explicit handoff required")
	}
	stem := launchdPhaseStem(p, "apply", 3)
	var start launchdPhaseStart
	var outcome launchdPhaseOutcome
	var commit launchdPhaseCommit
	if readLaunchdArtifact(home, stem+".started.json", &start) != nil || readLaunchdArtifact(home, stem+".outcome.json", &outcome) != nil || readLaunchdArtifact(home, stem+".committed.json", &commit) != nil || start.PlanID != p.ID || start.Direction != "apply" || start.Index != 3 || start.Name != "handoff" {
		return fmt.Errorf("no-effect rollback requires complete failed handoff evidence")
	}
	if err := verifyLaunchdCommittedPhase(home, p, start, outcome, commit); err != nil {
		return err
	}
	var review LaunchdRepairReview
	if readLaunchdArtifact(home, start.ReviewID+".review.json", &review) != nil || validateLaunchdReviewEvidence(home, p, review) != nil || review.Request.Purpose != "apply" || launchdWorkDigest(start.BeforeWork) != review.WorkSHA256 {
		return fmt.Errorf("no-effect rollback requires authentic consumed handoff review")
	}
	if commit.Outcome != "failed" || outcome.Final.Changed || !bytesEqual(start.BeforeOwner, p.Handoff.Owner) || !bytesEqual(start.BeforeBytes, p.Original) || outcome.OwnerSHA256 != launchdBytesDigest(p.Handoff.Owner) || outcome.FileSHA256 != launchdBytesDigest(p.Original) || launchdStateMatches(p, p.Original, outcome.Runtime, p.Original, "disabled", "absent") != nil || !reflect.DeepEqual(outcome.Runtime.ForeignOverrides, p.Runtime.ForeignOverrides) {
		return fmt.Errorf("failed handoff does not prove unchanged predecessor ownership and runtime")
	}
	path, err := launchdArtifactPath(home, p.ID+".plan.json")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		journal := strings.HasSuffix(name, ".started.json") || strings.HasSuffix(name, ".outcome.json") || strings.HasSuffix(name, ".committed.json") || strings.HasSuffix(name, ".terminal.json")
		if strings.HasPrefix(name, p.ID+".apply.") && journal && name != stem+".started.json" && name != stem+".outcome.json" && name != stem+".committed.json" {
			return fmt.Errorf("later forward phase blocks no-effect handoff rollback")
		}
	}
	return nil
}

func observeLaunchdUnchangedOwnership(home string, p LaunchdRepairPlan) error {
	if err := launchdUnchangedHandoff(home, p); err != nil {
		return err
	}
	owner, err := readLaunchdOwner(home, p)
	if err != nil || !bytesEqual(owner, p.Handoff.Owner) {
		return fmt.Errorf("no-effect rollback predecessor owner changed")
	}
	current, err := captureLaunchdTerminalHandoff(home, p, p.Handoff.FromPlanID, false)
	if err != nil || !reflect.DeepEqual(current, *p.Handoff) {
		return fmt.Errorf("no-effect rollback predecessor proof changed: %v", err)
	}
	return nil
}

func launchdOwnerEffect(home string, p LaunchdRepairPlan, name string) error {
	if p.Handoff == nil || (name != "handoff" && name != "restore_owner") {
		return fmt.Errorf("explicit reviewed ownership phase required")
	}
	path, err := launchdArtifactPath(home, launchdActiveName(p))
	if err != nil {
		return err
	}
	before, after := p.Handoff.Owner, launchdOwnerBytes(p.ID)
	if name == "handoff" {
		if err = validateLaunchdHandoff(home, p); err != nil {
			return err
		}
	} else {
		owner, ownerErr := readLaunchdOwner(home, p)
		unchanged := ownerErr == nil && bytesEqual(owner, p.Handoff.Owner)
		if unchanged {
			return observeLaunchdUnchangedOwnership(home, p)
		}
		current, verifyErr := captureLaunchdTerminalHandoff(home, p, p.Handoff.FromPlanID, true)
		if verifyErr != nil || !reflect.DeepEqual(current, *p.Handoff) {
			return fmt.Errorf("predecessor proof changed before ownership restoration: %v", verifyErr)
		}
		before, after = after, before
	}
	return launchdAtomicReplace(path, before, after, 0600)
}

func validateLaunchdPhaseOwner(home string, p LaunchdRepairPlan, name string) error {
	if name == "apply_handoff" {
		return validateLaunchdHandoff(home, p)
	}
	owner, err := readLaunchdOwner(home, p)
	if err != nil || !bytesEqual(owner, launchdOwnerBytes(p.ID)) {
		if err == nil && name == "rollback_restore_owner" && p.Handoff != nil && bytesEqual(owner, p.Handoff.Owner) {
			return launchdUnchangedHandoff(home, p)
		}
		return fmt.Errorf("launchd phase active owner changed")
	}
	return nil
}

func makeLaunchdPhaseOutcome(home string, p LaunchdRepairPlan, start launchdPhaseStart, b []byte, runtime LaunchdRuntime, cause error) launchdPhaseOutcome {
	if start.BeforeOwner == nil {
		return makeLaunchdOutcome(start, b, runtime, cause)
	}
	owner, err := readLaunchdOwner(home, p)
	if cause == nil && err != nil {
		cause = err
	}
	expected := launchdOwnerBytes(p.ID)
	if start.Name == "restore_owner" && p.Handoff != nil {
		expected = p.Handoff.Owner
	}
	if cause == nil && !bytesEqual(owner, expected) {
		cause = fmt.Errorf("ownership phase readback does not match exact expected owner")
	}
	out := makeLaunchdOutcome(start, b, runtime, cause)
	if out.Final.Outcome == "completed" && start.Name == "restore_owner" && p.Handoff != nil && bytesEqual(start.BeforeOwner, p.Handoff.Owner) && bytesEqual(owner, p.Handoff.Owner) {
		out.Final.Summary = "observed exact predecessor ownership unchanged; no ownership effect"
	}
	if err == nil {
		out.OwnerSHA256 = launchdBytesDigest(owner)
		out.Final.Changed = out.Final.Changed || !bytesEqual(owner, start.BeforeOwner)
		out.Final.AfterRefs = append(out.Final.AfterRefs, "launchd-owner:sha256:"+out.OwnerSHA256)
	}
	out.Final.ProofRefs = append(append([]string{}, start.Pending.ProofRefs...), "launchd-phase-start:sha256:"+start.Digest, "launchd-phase-observed:sha256:"+launchdObservedPhaseDigest(out))
	out.Digest = ""
	out.Digest = launchdDigest(out)
	return out
}

// A restored pointer may be replayed only to reconcile its already-started
// metadata phase. Every earlier rollback phase must already have a receipt.
func launchdOwnerRestoreStarted(home string, p LaunchdRepairPlan) bool {
	var manifest struct {
		Names []string `json:"names"`
	}
	if readLaunchdArtifact(home, p.ID+".rollback-manifest.json", &manifest) != nil || len(manifest.Names) == 0 || manifest.Names[len(manifest.Names)-1] != "restore_owner" {
		return false
	}
	for i := range manifest.Names[:len(manifest.Names)-1] {
		var committed launchdPhaseCommit
		if readLaunchdArtifact(home, launchdPhaseStem(p, "rollback", i)+".committed.json", &committed) != nil {
			return false
		}
	}
	var start launchdPhaseStart
	return readLaunchdArtifact(home, launchdPhaseStem(p, "rollback", len(manifest.Names)-1)+".started.json", &start) == nil && start.PlanID == p.ID && start.Name == "restore_owner" && start.Direction == "rollback"
}
