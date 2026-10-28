package work

import (
	"fmt"
	"strings"

	"compound/internal/authority/gitrepository"
)

// landingRead* name the Git fact kinds one projection request may reuse. They
// double as the deterministic read-count keys the focused tests pin: counting
// fact reads (not wall clock) keeps the amortization contract falsifiable.
const (
	landingReadCanonicalRepo = "canonical_repo"
	landingReadTargetRef     = "target_ref"
	landingReadRefCommit     = "ref_commit"
	landingReadCurrentBranch = "current_branch"
	landingReadStatus        = "status"
	landingReadWorktreeList  = "worktree_list"
	landingReadRefExists     = "ref_exists"
	landingReadCommitExists  = "commit_exists"
	landingReadAncestry      = "ancestry"
	landingReadIdentity      = "identity"
	landingReadOwnedPaths    = "owned_paths"
)

// landingFact is one cached read result: the value or the error, never both.
// Caching errors is the point — every Work affected by the same failed Git
// input must see that failure, not a silently empty fact.
type landingFact[T any] struct {
	value T
	err   error
}

type landingTargetRefFact struct {
	branch string
	ref    string
}

type landingOwnedDirtyFact struct {
	dirty bool
	paths []string
}

// landingGitFacts is a request-local read session over the Git facts that many
// Work delivery evaluations share. It exists so one projection request (one
// Workbench / coordination call) pays for each repository-scoped Git read once
// while every Work still evaluates its own envelope, contract, artifact,
// proof, publication, and lineage verdicts independently.
//
// It memoizes READ FACTS ONLY — never a LandCheckReport, never a verdict, and
// never anything Work-specific (proof resolution stays per-Work). Every key is
// the exact input tuple of the underlying read:
//
//   - canonical repository root, by asserted path;
//   - normalized target ref and ref→commit resolution, by repository + ref
//     (mutable refs keep first-read snapshot semantics inside one request);
//   - current branch, whole-tree status, and worktree list, by repository;
//   - ref existence, by repository + ref;
//   - commit existence and ancestry, by exact object ids;
//   - repository identity, by canonical root AND exact expected base — never
//     the root alone, because a different base can name a different lineage;
//   - owned-path dirt, by repository + exact base + candidate.
//
// A nil session is valid and means "read fresh on every call": the governed
// mutation paths (land-check, close, admission) never share one. The session
// dies with its request; it is never stored on the Store, never persisted, and
// never crosses processes, so a new request rereads every mutable fact (refs,
// status, origin identity, proofs) and every input that was missing before.
type landingGitFacts struct {
	canonicalRepoCache map[string]landingFact[string]
	targetRefCache     map[string]landingFact[landingTargetRefFact]
	refCommitCache     map[string]landingFact[string]
	currentBranchCache map[string]landingFact[string]
	statusCache        map[string]landingFact[string]
	worktreeListCache  map[string]landingFact[string]
	refExistsCache     map[string]landingFact[bool]
	commitExistsCache  map[string]landingFact[bool]
	ancestryCache      map[string]landingFact[bool]
	identityCache      map[string]landingFact[gitrepository.Identity]
	ownedDirtyCache    map[string]landingFact[landingOwnedDirtyFact]

	// reads counts, per fact kind, the real reads this session performed
	// (cache hits are free). Projection evidence only; never a gate.
	reads map[string]int
}

func newLandingGitFacts() *landingGitFacts {
	return &landingGitFacts{
		canonicalRepoCache: map[string]landingFact[string]{},
		targetRefCache:     map[string]landingFact[landingTargetRefFact]{},
		refCommitCache:     map[string]landingFact[string]{},
		currentBranchCache: map[string]landingFact[string]{},
		statusCache:        map[string]landingFact[string]{},
		worktreeListCache:  map[string]landingFact[string]{},
		refExistsCache:     map[string]landingFact[bool]{},
		commitExistsCache:  map[string]landingFact[bool]{},
		ancestryCache:      map[string]landingFact[bool]{},
		identityCache:      map[string]landingFact[gitrepository.Identity]{},
		ownedDirtyCache:    map[string]landingFact[landingOwnedDirtyFact]{},
		reads:              map[string]int{},
	}
}

func (f *landingGitFacts) recordRead(kind string) {
	if f.reads == nil {
		return
	}
	f.reads[kind]++
}

// readCount reports how many real Git reads of one fact kind this session
// performed. Deterministic projection evidence for the focused tests.
func (f *landingGitFacts) readCount(kind string) int {
	if f == nil {
		return 0
	}
	return f.reads[kind]
}

// canonicalRepo resolves an asserted path to the canonical Git worktree root
// (canonicalLandingRepo is the single authority for that definition).
func (f *landingGitFacts) canonicalRepo(path string) (string, error) {
	if f == nil {
		return canonicalLandingRepo(path)
	}
	if fact, ok := f.canonicalRepoCache[path]; ok {
		return fact.value, fact.err
	}
	value, err := canonicalLandingRepo(path)
	f.recordRead(landingReadCanonicalRepo)
	f.canonicalRepoCache[path] = landingFact[string]{value: value, err: err}
	return value, err
}

// normalizeTargetRef canonicalizes one target local ref inside one repository.
func (f *landingGitFacts) normalizeTargetRef(repoPath, value string) (string, string, error) {
	if f == nil {
		return normalizeLandingTargetRef(repoPath, value)
	}
	key := landingFactsKey(repoPath, value)
	if fact, ok := f.targetRefCache[key]; ok {
		return fact.value.branch, fact.value.ref, fact.err
	}
	branch, ref, err := normalizeLandingTargetRef(repoPath, value)
	f.recordRead(landingReadTargetRef)
	f.targetRefCache[key] = landingFact[landingTargetRefFact]{
		value: landingTargetRefFact{branch: branch, ref: ref},
		err:   err,
	}
	return branch, ref, err
}

// refCommit resolves one ref to its full commit object id. Refs are mutable:
// the first resolution inside a request is the snapshot the whole request
// uses, and a fresh request resolves again.
func (f *landingGitFacts) refCommit(repoPath, ref string) (string, error) {
	if f == nil {
		return runLandingGitOutput(repoPath, "rev-parse", "--verify", ref+"^{commit}")
	}
	key := landingFactsKey(repoPath, ref)
	if fact, ok := f.refCommitCache[key]; ok {
		return fact.value, fact.err
	}
	value, err := runLandingGitOutput(repoPath, "rev-parse", "--verify", ref+"^{commit}")
	f.recordRead(landingReadRefCommit)
	f.refCommitCache[key] = landingFact[string]{value: value, err: err}
	return value, err
}

// currentBranchRef reports the symbolic HEAD ref of one worktree.
func (f *landingGitFacts) currentBranchRef(repoPath string) (string, error) {
	if f == nil {
		return landingCurrentBranchRef(repoPath)
	}
	if fact, ok := f.currentBranchCache[repoPath]; ok {
		return fact.value, fact.err
	}
	value, err := landingCurrentBranchRef(repoPath)
	f.recordRead(landingReadCurrentBranch)
	f.currentBranchCache[repoPath] = landingFact[string]{value: value, err: err}
	return value, err
}

// statusPorcelain reads the whole-tree porcelain-v1 status of one worktree.
// The raw output is cached; callers keep their own error wrapping.
func (f *landingGitFacts) statusPorcelain(repoPath string) (string, error) {
	if f == nil {
		return runLandingGitOutput(repoPath, "status", "--porcelain=v1", "--untracked-files=all")
	}
	if fact, ok := f.statusCache[repoPath]; ok {
		return fact.value, fact.err
	}
	value, err := runLandingGitOutput(repoPath, "status", "--porcelain=v1", "--untracked-files=all")
	f.recordRead(landingReadStatus)
	f.statusCache[repoPath] = landingFact[string]{value: value, err: err}
	return value, err
}

// worktreeList reads the porcelain worktree list of one repository. The raw
// output is cached; callers keep their own error wrapping.
func (f *landingGitFacts) worktreeList(repoPath string) (string, error) {
	if f == nil {
		return runLandingGitOutput(repoPath, "worktree", "list", "--porcelain", "-z")
	}
	if fact, ok := f.worktreeListCache[repoPath]; ok {
		return fact.value, fact.err
	}
	value, err := runLandingGitOutput(repoPath, "worktree", "list", "--porcelain", "-z")
	f.recordRead(landingReadWorktreeList)
	f.worktreeListCache[repoPath] = landingFact[string]{value: value, err: err}
	return value, err
}

// refCheckedOut reports whether any worktree of one repository holds the exact
// branch ref, derived from the session's shared worktree list.
func (f *landingGitFacts) refCheckedOut(repoPath, targetBranchRef string) (bool, error) {
	if f == nil {
		return landingRefCheckedOut(repoPath, targetBranchRef)
	}
	out, err := f.worktreeList(repoPath)
	if err != nil {
		return false, fmt.Errorf("inspect worktrees for target ref %s: %w", targetBranchRef, err)
	}
	return landingWorktreeListHasBranch(out, targetBranchRef), nil
}

// refExists reports whether one exact ref exists in one repository.
func (f *landingGitFacts) refExists(repoPath, ref string) (bool, error) {
	if f == nil {
		return landingRefExists(repoPath, ref)
	}
	key := landingFactsKey(repoPath, ref)
	if fact, ok := f.refExistsCache[key]; ok {
		return fact.value, fact.err
	}
	value, err := landingRefExists(repoPath, ref)
	f.recordRead(landingReadRefExists)
	f.refExistsCache[key] = landingFact[bool]{value: value, err: err}
	return value, err
}

// commitExists reports whether one exact commit object id exists in one
// repository. Missing ids are a fact, not an error, so a repaired repository
// is reread by the next request.
func (f *landingGitFacts) commitExists(repoPath, sha string) (bool, error) {
	if f == nil {
		return landingCommitExists(repoPath, sha)
	}
	key := landingFactsKey(repoPath, sha)
	if fact, ok := f.commitExistsCache[key]; ok {
		return fact.value, fact.err
	}
	value, err := landingCommitExists(repoPath, sha)
	f.recordRead(landingReadCommitExists)
	f.commitExistsCache[key] = landingFact[bool]{value: value, err: err}
	return value, err
}

// isAncestor answers one exact ancestry question between two object ids.
// Callers resolve refs to immutable ids first, so the key never depends on a
// mutable ref name.
func (f *landingGitFacts) isAncestor(repoPath, ancestor, descendant string) (bool, error) {
	if f == nil {
		return landingIsAncestor(repoPath, ancestor, descendant)
	}
	key := landingFactsKey(repoPath, ancestor, descendant)
	if fact, ok := f.ancestryCache[key]; ok {
		return fact.value, fact.err
	}
	value, err := landingIsAncestor(repoPath, ancestor, descendant)
	f.recordRead(landingReadAncestry)
	f.ancestryCache[key] = landingFact[bool]{value: value, err: err}
	return value, err
}

// repositoryIdentity derives the repository identity at one exact base commit.
// Keyed by canonical root AND expected base — never the root alone — because
// the lineage roots and origin identity are read at that base. The
// gitrepository package stays the sole owner of the definition.
func (f *landingGitFacts) repositoryIdentity(root, expectedBase string) (gitrepository.Identity, error) {
	if f == nil {
		return gitrepository.ReadIdentity(root, expectedBase)
	}
	key := landingFactsKey(root, expectedBase)
	if fact, ok := f.identityCache[key]; ok {
		return fact.value, fact.err
	}
	value, err := gitrepository.ReadIdentity(root, expectedBase)
	f.recordRead(landingReadIdentity)
	f.identityCache[key] = landingFact[gitrepository.Identity]{value: value, err: err}
	return value, err
}

// ownedPathsDirty reports the uncommitted changes inside one exact
// base..candidate owned-path set of one repository.
func (f *landingGitFacts) ownedPathsDirty(repoPath, expectedBase, candidate string) (bool, []string, error) {
	if f == nil {
		return landingOwnedPathsDirty(repoPath, expectedBase, candidate)
	}
	key := landingFactsKey(repoPath, expectedBase, candidate)
	if fact, ok := f.ownedDirtyCache[key]; ok {
		return fact.value.dirty, append([]string(nil), fact.value.paths...), fact.err
	}
	dirty, paths, err := landingOwnedPathsDirty(repoPath, expectedBase, candidate)
	f.recordRead(landingReadOwnedPaths)
	f.ownedDirtyCache[key] = landingFact[landingOwnedDirtyFact]{
		value: landingOwnedDirtyFact{dirty: dirty, paths: append([]string(nil), paths...)},
		err:   err,
	}
	return dirty, append([]string(nil), paths...), err
}

// landingFactsKey joins exact read inputs into one cache key. NUL separates
// the fields because no Git ref name, path, or object id may contain one.
func landingFactsKey(fields ...string) string {
	return strings.Join(fields, "\x00")
}
