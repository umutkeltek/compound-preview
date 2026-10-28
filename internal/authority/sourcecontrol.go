package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/candidates"
	"compound/internal/mem"
)

const SourceControlSchemaVersion = "compound.source_control_snapshot.v0.1"

type SourceControlSnapshot struct {
	SchemaVersion           string `json:"schema_version"`
	RepoID                  string `json:"repo_id"`
	RequestedPath           string `json:"requested_path,omitempty"`
	GitRoot                 string `json:"git_root"`
	MainWorktreeRoot        string `json:"main_worktree_root,omitempty"`
	Remote                  string `json:"remote,omitempty"`
	RepositoryFingerprint   string `json:"repository_fingerprint,omitempty"`
	Branch                  string `json:"branch,omitempty"`
	Head                    string `json:"head,omitempty"`
	Upstream                string `json:"upstream,omitempty"`
	Ahead                   int    `json:"ahead"`
	Behind                  int    `json:"behind"`
	Status                  string `json:"status"` // clean | dirty
	Staged                  int    `json:"staged"`
	Unstaged                int    `json:"unstaged"`
	Untracked               int    `json:"untracked"`
	ChangedPaths            int    `json:"changed_paths"`
	TargetPath              string `json:"target_path,omitempty"`
	TargetStatus            string `json:"target_status,omitempty"` // clean | dirty | unknown
	ChangedStateFingerprint string `json:"changed_state_fingerprint,omitempty"`
	EvidenceRef             string `json:"evidence_ref"`
}

type SourceControlReport struct {
	EntityRef               string `json:"entity_ref"`
	Status                  string `json:"status"`
	GitRoot                 string `json:"git_root"`
	Branch                  string `json:"branch,omitempty"`
	Head                    string `json:"head,omitempty"`
	Remote                  string `json:"remote,omitempty"`
	RepositoryFingerprint   string `json:"repository_fingerprint,omitempty"`
	Upstream                string `json:"upstream,omitempty"`
	Ahead                   int    `json:"ahead"`
	Behind                  int    `json:"behind"`
	Staged                  int    `json:"staged"`
	Unstaged                int    `json:"unstaged"`
	Untracked               int    `json:"untracked"`
	ChangedPaths            int    `json:"changed_paths"`
	ChangedStateFingerprint string `json:"changed_state_fingerprint,omitempty"`
	CandidateCount          int    `json:"candidate_count"`
	EvidenceRef             string `json:"evidence_ref"`
}

// ImportGitSnapshot reads live git state with read-only git commands and turns
// it into deterministic authority candidates. It never stages, cleans, fetches,
// pushes, or edits the inspected repository.
func ImportGitSnapshot(path string, existing []mem.Entity) (SourceControlSnapshot, mem.Entity, []mem.Candidate, SourceControlReport, error) {
	snap, err := ReadGitSnapshot(path)
	if err != nil {
		return SourceControlSnapshot{}, mem.Entity{}, nil, SourceControlReport{Status: "fail"}, err
	}
	entity := mergeSourceControlEntity(snap, existing)
	facts := sourceControlCandidates(snap)
	report := SourceControlReport{
		EntityRef:               entity.Slug,
		Status:                  snap.Status,
		GitRoot:                 snap.GitRoot,
		Branch:                  snap.Branch,
		Head:                    snap.Head,
		Remote:                  snap.Remote,
		RepositoryFingerprint:   snap.RepositoryFingerprint,
		Upstream:                snap.Upstream,
		Ahead:                   snap.Ahead,
		Behind:                  snap.Behind,
		Staged:                  snap.Staged,
		Unstaged:                snap.Unstaged,
		Untracked:               snap.Untracked,
		ChangedPaths:            snap.ChangedPaths,
		ChangedStateFingerprint: snap.ChangedStateFingerprint,
		CandidateCount:          len(facts),
		EvidenceRef:             snap.EvidenceReference(),
	}
	return snap, entity, facts, report, nil
}

func ReadGitSnapshot(path string) (SourceControlSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requestedPath, inspectDir, err := gitInspectionPath(path)
	if err != nil {
		return SourceControlSnapshot{}, err
	}
	root, err := gitOutput(ctx, inspectDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return SourceControlSnapshot{}, fmt.Errorf("git root: %w", err)
	}
	root = strings.TrimSpace(root)
	if canonical, canonicalErr := filepath.EvalSymlinks(root); canonicalErr == nil {
		root = canonical
	}
	repositoryIdentity, err := ReadGitRepositoryIdentity(root, "HEAD")
	if err != nil {
		return SourceControlSnapshot{}, fmt.Errorf("git repository identity: %w", err)
	}
	remote, _ := gitOutput(ctx, root, "config", "--get", "remote.origin.url")
	branch, _ := gitOutput(ctx, root, "branch", "--show-current")
	head, _ := gitOutput(ctx, root, "rev-parse", "HEAD")
	upstream, _ := gitOutput(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	ahead, behind := 0, 0
	if strings.TrimSpace(upstream) != "" {
		if counts, err := gitOutput(ctx, root, "rev-list", "--left-right", "--count", "HEAD...@{u}"); err == nil {
			ahead, behind = parseAheadBehind(counts)
		}
	}
	porcelain, err := gitOutput(ctx, root, "status", "--porcelain=v1", "-b")
	if err != nil {
		return SourceControlSnapshot{}, fmt.Errorf("git status: %w", err)
	}
	staged, unstaged, untracked := parsePorcelainCounts(porcelain)
	changed := staged + unstaged + untracked
	status := "clean"
	if changed > 0 {
		status = "dirty"
	}
	targetPath, targetErr := CanonicalSourceControlMutationTarget(root, requestedPath)
	targetStatus := "clean"
	if targetErr != nil {
		targetStatus = "unknown"
	} else if status == "dirty" {
		targetStatus, err = readTargetSourceControlStatus(ctx, root, targetPath)
		if err != nil {
			targetStatus = "unknown"
		}
	}
	fingerprint := ""
	if changed > 0 {
		fingerprint, err = readChangedStateFingerprint(ctx, root)
		if err != nil {
			return SourceControlSnapshot{}, fmt.Errorf("git changed-state fingerprint: %w", err)
		}
	}
	repositoryID := repoID(strings.TrimSpace(remote), root)
	snap := SourceControlSnapshot{
		SchemaVersion:           SourceControlSchemaVersion,
		RepoID:                  repositoryID,
		RequestedPath:           requestedPath,
		GitRoot:                 root,
		MainWorktreeRoot:        mainWorktreeRoot(ctx, root),
		Remote:                  strings.TrimSpace(remote),
		RepositoryFingerprint:   repositoryIdentity.Fingerprint,
		Branch:                  strings.TrimSpace(branch),
		Head:                    strings.TrimSpace(head),
		Upstream:                strings.TrimSpace(upstream),
		Ahead:                   ahead,
		Behind:                  behind,
		Status:                  status,
		Staged:                  staged,
		Unstaged:                unstaged,
		Untracked:               untracked,
		ChangedPaths:            changed,
		TargetPath:              targetPath,
		TargetStatus:            targetStatus,
		ChangedStateFingerprint: fingerprint,
		EvidenceRef:             sourceControlEvidenceRef(repositoryID, strings.TrimSpace(head), fingerprint),
	}
	return snap, nil
}

func readTargetSourceControlStatus(ctx context.Context, root, targetPath string) (string, error) {
	rel, err := filepath.Rel(root, targetPath)
	if err != nil {
		return "unknown", err
	}
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "unknown", fmt.Errorf("target path escapes git root")
	}
	pathspec := filepath.ToSlash(rel)
	if pathspec == "" {
		pathspec = "."
	}
	porcelain, err := gitOutput(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", pathspec)
	if err != nil {
		return "unknown", err
	}
	if porcelain == "" {
		return "clean", nil
	}
	return "dirty", nil
}

type GitRepositoryIdentity = gitrepository.Identity

func ReadGitRepositoryIdentity(path, lineageRef string) (GitRepositoryIdentity, error) {
	return gitrepository.ReadIdentity(path, lineageRef)
}

func (s SourceControlSnapshot) EvidenceReference() string {
	if strings.TrimSpace(s.EvidenceRef) != "" {
		return s.EvidenceRef
	}
	return sourceControlEvidenceRef(s.RepoID, s.Head, s.ChangedStateFingerprint)
}

func sourceControlEvidenceRef(repoID, head, fingerprint string) string {
	head = strings.TrimSpace(head)
	if head == "" {
		head = "unknown-head"
	}
	ref := "source-control:" + repoID + ":" + head
	if fingerprint = strings.TrimSpace(fingerprint); fingerprint != "" {
		ref += ":" + fingerprint
	}
	return ref
}

// readChangedStateFingerprint binds the complete dirty state, not only counters:
// porcelain-v2 paths/status, every index stage/blob, and the current bytes for
// modified/deleted/untracked paths. A staged/unstaged flip therefore changes the
// digest even when the working-tree bytes do not.
func readChangedStateFingerprint(ctx context.Context, root string) (string, error) {
	status, err := gitOutput(ctx, root, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	index, err := gitOutput(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	changed, err := gitOutput(ctx, root, "ls-files", "-m", "-d", "-o", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}

	h := sha256.New()
	writeFingerprintFrame(h, "schema", []byte("compound.git_changed_state.v0.1"))
	writeFingerprintFrame(h, "status", []byte(status))
	writeFingerprintFrame(h, "index", []byte(index))
	paths := uniqueNULPaths(changed)
	for _, path := range paths {
		state, stateErr := changedPathState(ctx, root, path)
		if stateErr != nil {
			return "", fmt.Errorf("path %q: %w", path, stateErr)
		}
		writeFingerprintFrame(h, "path", []byte(filepath.ToSlash(path)))
		writeFingerprintFrame(h, "worktree", state)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func uniqueNULPaths(raw string) []string {
	seen := map[string]struct{}{}
	for _, path := range strings.Split(raw, "\x00") {
		if path != "" {
			seen[path] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func changedPathState(ctx context.Context, root, path string) ([]byte, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path escapes git root")
	}
	abs := filepath.Join(root, clean)
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return []byte("missing"), nil
	}
	if err != nil {
		return nil, err
	}
	prefix := []byte(info.Mode().String() + "\x00")
	switch {
	case info.Mode().IsRegular():
		digest, digestErr := fileContentSHA256(ctx, abs)
		if digestErr != nil {
			return nil, digestErr
		}
		return append(prefix, []byte(digest)...), nil
	case info.Mode()&os.ModeSymlink != 0:
		target, linkErr := os.Readlink(abs)
		if linkErr != nil {
			return nil, linkErr
		}
		return append(prefix, []byte("symlink:"+target)...), nil
	case info.IsDir():
		nestedRoot, rootErr := gitOutput(ctx, abs, "rev-parse", "--show-toplevel")
		if rootErr == nil && strings.TrimSpace(nestedRoot) != "" && strings.TrimSpace(nestedRoot) != root {
			nested, nestedErr := readChangedStateFingerprint(ctx, strings.TrimSpace(nestedRoot))
			if nestedErr != nil {
				return nil, nestedErr
			}
			return append(prefix, []byte("nested-git:"+nested)...), nil
		}
		return append(prefix, []byte("directory")...), nil
	default:
		return append(prefix, []byte("special")...), nil
	}
}

// fileContentSHA256 hashes abs's CURRENT bytes on every call. This digest feeds
// ChangedStateFingerprint, which is matched against reviewed source-control
// approval — so content identity must come from the bytes themselves, never
// from a stat tuple: a same-size, same-mtime rewrite that reused a cached hash
// would let approval bind to bytes nobody reviewed (external delta audit
// 2026-09-16). A stat-keyed cache is therefore not permitted on this path; the
// ctx-aborting read is the only allowed shortcut: it bounds hashing by the
// snapshot deadline and fails closed.
func fileContentSHA256(ctx context.Context, abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, &ctxAbortingReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

// ctxAbortingReader makes the snapshot's context deadline bound content
// hashing too, not only the git subprocess calls.
type ctxAbortingReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxAbortingReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func writeFingerprintFrame(h hash.Hash, label string, value []byte) {
	fmt.Fprintf(h, "%s:%d:", label, len(value))
	_, _ = h.Write(value)
}

func gitInspectionPath(path string) (requestedPath, inspectDir string, err error) {
	requestedPath, err = filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(requestedPath)
	if err == nil {
		if info.IsDir() {
			return requestedPath, requestedPath, nil
		}
		return requestedPath, filepath.Dir(requestedPath), nil
	}
	if !os.IsNotExist(err) {
		return "", "", err
	}
	parent, err := nearestExistingParent(requestedPath)
	if err != nil {
		return "", "", err
	}
	return requestedPath, parent, nil
}

func nearestExistingParent(path string) (string, error) {
	p := path
	for {
		info, err := os.Stat(p)
		if err == nil {
			if info.IsDir() {
				return p, nil
			}
			return filepath.Dir(p), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(p)
		if next == p {
			return "", fmt.Errorf("no existing parent for %s", path)
		}
		p = next
	}
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s: %s", strings.Join(args, " "), msg)
	}
	return string(out), nil
}

func parsePorcelainCounts(text string) (staged, unstaged, untracked int) {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "##") {
			continue
		}
		if strings.HasPrefix(line, "??") {
			untracked++
			continue
		}
		if len(line) < 2 {
			continue
		}
		if line[0] != ' ' {
			staged++
		}
		if line[1] != ' ' {
			unstaged++
		}
	}
	return staged, unstaged, untracked
}

func parseAheadBehind(text string) (ahead, behind int) {
	parts := strings.Fields(text)
	if len(parts) >= 2 {
		ahead, _ = strconv.Atoi(parts[0])
		behind, _ = strconv.Atoi(parts[1])
	}
	return ahead, behind
}

func repoID(remote, root string) string {
	base := strings.TrimSpace(remote)
	if base == "" {
		base = filepath.Base(root)
	} else {
		base = strings.TrimSuffix(base, ".git")
		base = filepath.Base(strings.ReplaceAll(base, ":", "/"))
	}
	return "repo:" + strings.ReplaceAll(slugToken(base), "_", "-")
}

func slugToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

// mergeSourceControlEntity records only identifiers that outlive the invocation
// that observed them. Identifiers are never evicted, so anything ephemeral put
// here accumulates forever inside a row that is re-appended in full on every
// change -- a set stored across a time axis, which costs sum(n) rather than n.
//
// GitRoot is the toplevel of whichever worktree was inspected, so from a linked
// worktree it names a directory that is deleted when the branch lands, and
// RequestedPath is whatever path the caller passed, down to a single file. Both
// are observations of one invocation, not identity, and both already reach the
// evidence layer: sourceControlCandidates emits git_root as a candidate fact on
// every import, so nothing is lost by keeping them out of the entity.
func mergeSourceControlEntity(snap SourceControlSnapshot, existing []mem.Entity) mem.Entity {
	root := snap.MainWorktreeRoot
	if root == "" {
		root = snap.GitRoot
	}
	ids := []string{root}
	// Inside a linked worktree every path observed is scoped to a checkout that
	// outlives nothing, so the main root is the only path worth recording.
	//
	// Otherwise the requested path is kept only when it is another name for the
	// root itself -- the root is canonicalized on read, so a repo reached
	// through a symlinked prefix (anything under /var on macOS) is named by a
	// string the canonical root does not match, and identifier matching is
	// literal. That alias is bounded at one per spelling of the root, and
	// because matching is prefix-aware it also resolves every path beneath it.
	// A requested path that merely lives inside the repo is not kept: the root,
	// in either spelling, already covers it.
	inLinkedWorktree := snap.MainWorktreeRoot != "" && snap.MainWorktreeRoot != snap.GitRoot
	if !inLinkedWorktree && snap.RequestedPath != "" && canonicalPath(snap.RequestedPath) == root {
		ids = append(ids, snap.RequestedPath)
	}
	if snap.Remote != "" {
		ids = append(ids, snap.Remote, "git_remote:"+snap.Remote)
	}
	return mergeEntity(existing, snap.RepoID, "repo", []string{strings.TrimPrefix(snap.RepoID, "repo:")}, ids)
}

func canonicalPath(path string) string {
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(path))
	if err != nil {
		return path
	}
	return resolved
}

// mainWorktreeRoot resolves the repository's main worktree. A linked worktree's
// git dir points back at the main checkout's .git, which is what makes the main
// root stable across the worktrees that come and go against it.
func mainWorktreeRoot(ctx context.Context, root string) string {
	common, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	common = strings.TrimSpace(common)
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	if canonical, canonicalErr := filepath.EvalSymlinks(common); canonicalErr == nil {
		common = canonical
	}
	if filepath.Base(common) != ".git" {
		// Bare repository or a layout with no main worktree to name.
		return ""
	}
	return filepath.Dir(common)
}

func pathCoveredBy(root, candidate string) bool {
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	candidate = strings.TrimSpace(candidate)
	if root == "" || candidate == "" {
		return false
	}
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func sourceControlCandidates(snap SourceControlSnapshot) []mem.Candidate {
	src := snap.EvidenceReference()
	add := func(out *[]mem.Candidate, pred, object string) {
		object = strings.TrimSpace(object)
		if object == "" {
			return
		}
		*out = append(*out, mem.Candidate{
			Entity:         snap.RepoID,
			Predicate:      pred,
			Object:         object,
			Confidence:     1.0,
			Derivation:     "deterministic",
			ClaimType:      "status",
			Qualifiers:     map[string]string{"domain": pred},
			PromotionBasis: candidates.BasisTrustedDeterministic,
			SourceTraceRef: src,
			SnippetHash:    src + ":" + pred + ":" + object,
			Visibility:     "operational",
		})
	}
	var facts []mem.Candidate
	add(&facts, "git_root", snap.GitRoot)
	add(&facts, "source_repo", snap.Remote)
	add(&facts, "current_branch", snap.Branch)
	add(&facts, "head", snap.Head)
	add(&facts, "upstream", snap.Upstream)
	add(&facts, "source_control_status", snap.Status)
	add(&facts, "ahead_count", strconv.Itoa(snap.Ahead))
	add(&facts, "behind_count", strconv.Itoa(snap.Behind))
	add(&facts, "staged_count", strconv.Itoa(snap.Staged))
	add(&facts, "unstaged_count", strconv.Itoa(snap.Unstaged))
	add(&facts, "untracked_count", strconv.Itoa(snap.Untracked))
	add(&facts, "changed_path_count", strconv.Itoa(snap.ChangedPaths))
	return facts
}
