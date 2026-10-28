package work

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// set-aside is the tool-owned scoped-stash play: it stashes a bounded set of
// FOREIGN uncommitted changes to an envelope's own files (a neighbor lane's WIP
// living in a shared checkout) so the landing ceremony's owned-path dirtiness
// gate can pass, then restores them token-for-token or fails loudly. It never
// discards work: begin records the exact working-tree content (SHA-256) and
// stashes under a random token; end matches that token, applies (never pops),
// re-verifies every byte, and only then drops the stash. A mismatch preserves
// both the stash entry and the record for manual recovery.
//
// This is a transient operational record, not a ledger byte: it does not touch
// the work event log, emits no Receipt/Transition, and requires no
// PolicyDecision. It imitates the land-lease / action-reservation precedent of
// per-work JSON custodied under COMPOUND_HOME with an atomic temp+rename write.
const (
	SetAsideSchemaVersion = "compound.work_set_aside.v0.1"
	// setAsideStashPrefix is the stable, searchable message prefix carried by
	// every set-aside stash entry. Identity is the token that follows it, never a
	// positional stash@{N} ref.
	setAsideStashPrefix = "compound-set-aside:"
	// setAsideUntrackedStatus is the porcelain-v1 XY code for an untracked path.
	setAsideUntrackedStatus = "??"
)

// SetAsideFile records one file's foreign working-tree content so end can prove
// the restore reproduced it exactly.
type SetAsideFile struct {
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	PorcelainStatus string `json:"porcelain_status"`
}

// SetAsideRecord is the per-work transient stash record written under
// $COMPOUND_HOME/work/set-aside/<workID>.json.
type SetAsideRecord struct {
	SchemaVersion string         `json:"schema_version"`
	WorkID        string         `json:"work_id"`
	Token         string         `json:"token"`
	Actor         string         `json:"actor"`
	Reason        string         `json:"reason"`
	CandidateSHA  string         `json:"candidate_sha"`
	Files         []SetAsideFile `json:"files"`
	CreatedAt     string         `json:"created_at"`
}

// SetAsideBeginOptions names every input to begin explicitly.
type SetAsideBeginOptions struct {
	WorkID       string
	CandidateSHA string
	Files        []string
	Actor        string
	Reason       string
}

// SetAsideEndOptions names every input to end explicitly.
type SetAsideEndOptions struct {
	WorkID string
	Actor  string
}

// SetAsideEndResult reports the outcome of a successful restore.
type SetAsideEndResult struct {
	WorkID   string   `json:"work_id"`
	Token    string   `json:"token"`
	StashRef string   `json:"stash_ref"`
	Restored []string `json:"restored"`
}

// SetAsideBegin verifies every fail-closed refusal BEFORE any mutation, then
// performs the single mutation (git stash push of the named files under a fresh
// token), verifies the stash landed and the paths are clean, and only then
// atomically writes the record. A refusal leaves the working tree, the stash
// list, and the home byte-identical.
func (s *Store) SetAsideBegin(opts SetAsideBeginOptions, now time.Time) (SetAsideRecord, error) {
	if now.IsZero() {
		now = time.Now()
	}
	workID := strings.TrimSpace(opts.WorkID)
	actor := strings.TrimSpace(opts.Actor)
	reason := strings.TrimSpace(opts.Reason)
	candidate := strings.TrimSpace(opts.CandidateSHA)

	if workID == "" {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin requires <work_id>")
	}
	if actor == "" {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin requires --by <actor>")
	}
	if reason == "" {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin requires --reason <why the dirt is foreign>")
	}
	if len(opts.Files) == 0 {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin requires at least one --file <path>")
	}

	w, err := s.Get(workID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SetAsideRecord{}, fmt.Errorf("set-aside begin: work %s not found", workID)
		}
		return SetAsideRecord{}, err
	}
	if terminalStates[w.State] {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: work %s is %s (terminal); nothing to set aside", workID, w.State)
	}
	if w.GitDelivery == nil {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: work %s has no git_delivery contract; set-aside is only for source-controlled work", workID)
	}
	contract := *w.GitDelivery
	repoPath := strings.TrimSpace(contract.TargetWorktreeRoot)
	expectedBase := strings.TrimSpace(contract.ExpectedBaseSHA)

	if !validLandingObjectID(candidate) {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: --candidate %q must be a full lowercase Git object id (40 or 64 hex characters)", candidate)
	}
	if _, err := setAsideGit(repoPath, "rev-parse", "--verify", "--quiet", candidate+"^{commit}"); err != nil {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: --candidate %s is not a commit resolvable in %s", candidate, repoPath)
	}

	recordPath, err := s.setAsideRecordPath(workID)
	if err != nil {
		return SetAsideRecord{}, err
	}
	if _, statErr := os.Stat(recordPath); statErr == nil {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: a set-aside record already exists for %s; run `compound work set-aside end %s` before beginning another", workID, workID)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: inspect existing record: %w", statErr)
	}

	// Owned set is single-sourced with the land-check gate: only files in the
	// envelope's base..candidate diff can block the ceremony, so only they are
	// worth setting aside.
	ownedPaths, err := landingOwnedPaths(repoPath, expectedBase, candidate)
	if err != nil {
		return SetAsideRecord{}, err
	}
	ownedSet := make(map[string]bool, len(ownedPaths))
	for _, p := range ownedPaths {
		ownedSet[p] = true
	}

	// Validate EVERY file before touching the working tree. Any failure returns
	// here, before the single stash mutation, so a refusal is a no-op.
	files := make([]SetAsideFile, 0, len(opts.Files))
	seen := make(map[string]bool, len(opts.Files))
	for _, raw := range opts.Files {
		rel, err := setAsideRepoRelative(repoPath, raw)
		if err != nil {
			return SetAsideRecord{}, fmt.Errorf("set-aside begin: --file %q escapes the target root %s", raw, repoPath)
		}
		if seen[rel] {
			return SetAsideRecord{}, fmt.Errorf("set-aside begin: --file %s given more than once", rel)
		}
		seen[rel] = true

		status, err := s.setAsideFileStatus(repoPath, rel)
		if err != nil {
			return SetAsideRecord{}, err
		}
		if status == setAsideUntrackedStatus {
			return SetAsideRecord{}, fmt.Errorf("set-aside begin: --file %s is untracked; set-aside preserves tracked dirty files only", rel)
		}
		if status == "" {
			return SetAsideRecord{}, fmt.Errorf("set-aside begin: --file %s is clean in the working tree; nothing to set aside", rel)
		}
		if !ownedSet[rel] {
			return SetAsideRecord{}, fmt.Errorf("set-aside begin: --file %s is not in work %s's owned set (%s..%s); it would not block the landing ceremony, so setting it aside is pointless", rel, workID, expectedBase, candidate)
		}
		workingBytes, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(rel)))
		if err != nil {
			return SetAsideRecord{}, fmt.Errorf("set-aside begin: read working-tree %s: %w", rel, err)
		}
		workingSHA := sha256Hex(workingBytes)
		// Owned-file refusal: if the dirt already equals what the candidate
		// committed, it is THIS envelope's own change -- commit it, do not hide it.
		if blobBytes, blobErr := setAsideGitBytes(repoPath, "cat-file", "blob", candidate+":"+rel); blobErr == nil {
			if sha256Hex(blobBytes) == workingSHA {
				return SetAsideRecord{}, fmt.Errorf("set-aside begin: --file %s already matches candidate %s's committed content; this is work %s's own change -- commit it, do not set it aside", rel, candidate, workID)
			}
		}
		files = append(files, SetAsideFile{Path: rel, SHA256: workingSHA, PorcelainStatus: status})
	}

	// All refusals passed. The single mutation follows.
	token, err := newSetAsideToken()
	if err != nil {
		return SetAsideRecord{}, err
	}
	pathspecs := make([]string, 0, len(files))
	for _, f := range files {
		pathspecs = append(pathspecs, f.Path)
	}
	stashArgs := append([]string{"stash", "push", "-m", setAsideStashPrefix + token, "--"}, pathspecs...)
	if _, err := setAsideGit(repoPath, stashArgs...); err != nil {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: git stash push failed, nothing recorded: %w", err)
	}

	// Verify the mutation landed exactly once and reverted the working tree.
	ref, count, err := s.matchSetAsideStash(repoPath, token)
	if err != nil {
		return SetAsideRecord{}, err
	}
	if count != 1 {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: expected exactly one stash entry for token %s after push, found %d; inspect `git -C %s stash list`", token, count, repoPath)
	}
	for _, f := range files {
		status, err := s.setAsideFileStatus(repoPath, f.Path)
		if err != nil {
			return SetAsideRecord{}, err
		}
		if status != "" {
			return SetAsideRecord{}, fmt.Errorf("set-aside begin: %s is still dirty after stash push (stash entry %s preserved); inspect `git -C %s stash list`", f.Path, ref, repoPath)
		}
	}

	record := SetAsideRecord{
		SchemaVersion: SetAsideSchemaVersion,
		WorkID:        workID,
		Token:         token,
		Actor:         actor,
		Reason:        reason,
		CandidateSHA:  candidate,
		Files:         files,
		CreatedAt:     now.UTC().Format(time.RFC3339),
	}
	if err := writeSetAsideRecordAtomic(recordPath, record); err != nil {
		return SetAsideRecord{}, fmt.Errorf("set-aside begin: write record (stash entry %s preserved -- restore it manually): %w", ref, err)
	}
	return record, nil
}

// SetAsideEnd restores a set-aside token-for-token. Apply-then-verify-then-drop
// is the safety spine: it never pops, it re-verifies every recorded byte after
// apply, and it drops the stash only once the restore is proven exact. Any
// divergence, or a recorded path that is dirty before restore, is a loud
// failure that preserves BOTH the stash entry and the record.
func (s *Store) SetAsideEnd(opts SetAsideEndOptions, now time.Time) (SetAsideEndResult, error) {
	if now.IsZero() {
		now = time.Now()
	}
	workID := strings.TrimSpace(opts.WorkID)
	if workID == "" {
		return SetAsideEndResult{}, fmt.Errorf("set-aside end requires <work_id>")
	}

	recordPath, err := s.setAsideRecordPath(workID)
	if err != nil {
		return SetAsideEndResult{}, err
	}
	record, err := readSetAsideRecord(recordPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SetAsideEndResult{}, fmt.Errorf("set-aside end: no set-aside record for %s; nothing to restore", workID)
		}
		return SetAsideEndResult{}, err
	}

	// The repo is authority-bound: resolve it from the envelope's contract, not
	// from any path stored in the transient record.
	w, err := s.Get(workID)
	if err != nil {
		return SetAsideEndResult{}, fmt.Errorf("set-aside end: load work %s: %w", workID, err)
	}
	if w.GitDelivery == nil {
		return SetAsideEndResult{}, fmt.Errorf("set-aside end: work %s has no git_delivery contract; cannot locate the target repo", workID)
	}
	repoPath := strings.TrimSpace(w.GitDelivery.TargetWorktreeRoot)

	ref, count, err := s.matchSetAsideStash(repoPath, record.Token)
	if err != nil {
		return SetAsideEndResult{}, err
	}
	if count != 1 {
		return SetAsideEndResult{}, fmt.Errorf("set-aside end: expected exactly one stash entry for token %s, found %d; record and stash left untouched", record.Token, count)
	}

	// A recorded path that is already dirty would be overwritten by the restore;
	// refuse before apply so no work is lost.
	for _, f := range record.Files {
		status, err := s.setAsideFileStatus(repoPath, f.Path)
		if err != nil {
			return SetAsideEndResult{}, err
		}
		if status != "" {
			return SetAsideEndResult{}, fmt.Errorf("set-aside end: %s is dirty in the working tree; resolve or discard it before restoring (stash %s and record preserved)", f.Path, ref)
		}
	}

	if _, err := setAsideGit(repoPath, "stash", "apply", ref); err != nil {
		return SetAsideEndResult{}, fmt.Errorf("set-aside end: git stash apply %s failed (stash entry and record preserved): %w", ref, err)
	}

	var diverged []string
	for _, f := range record.Files {
		workingBytes, readErr := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(f.Path)))
		if readErr != nil || sha256Hex(workingBytes) != f.SHA256 {
			diverged = append(diverged, f.Path)
		}
	}
	if len(diverged) > 0 {
		return SetAsideEndResult{}, fmt.Errorf("set-aside end: restored content diverged for %s; stash entry %s AND record %s are preserved for manual recovery -- nothing was dropped", strings.Join(diverged, ", "), ref, filepath.Base(recordPath))
	}

	if _, err := setAsideGit(repoPath, "stash", "drop", ref); err != nil {
		return SetAsideEndResult{}, fmt.Errorf("set-aside end: files restored but git stash drop %s failed (record preserved): %w", ref, err)
	}
	if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return SetAsideEndResult{}, fmt.Errorf("set-aside end: files restored and stash dropped but record cleanup failed: %w", err)
	}

	restored := make([]string, 0, len(record.Files))
	for _, f := range record.Files {
		restored = append(restored, f.Path)
	}
	return SetAsideEndResult{WorkID: workID, Token: record.Token, StashRef: ref, Restored: restored}, nil
}

// setAsideRecordPath is the per-work record location. The work id is guarded
// against path traversal so it can never escape the set-aside directory.
func (s *Store) setAsideRecordPath(workID string) (string, error) {
	if workID == "" || filepath.Base(workID) != workID || strings.ContainsAny(workID, `/\`) {
		return "", fmt.Errorf("invalid work id %q for set-aside record", workID)
	}
	return filepath.Join(s.home, "work", "set-aside", workID+".json"), nil
}

// setAsideFileStatus returns the porcelain-v1 XY status code for one repo-relative
// path, or "" when the path is clean. It captures raw bytes (no leading-space
// trimming) so the two-character code -- including the untracked "??" and a
// leading-space " M" -- survives intact.
func (s *Store) setAsideFileStatus(repoPath, rel string) (string, error) {
	out, err := setAsideGitBytes(repoPath, "--literal-pathspecs", "status", "--porcelain=v1", "--untracked-files=all", "--", rel)
	if err != nil {
		return "", fmt.Errorf("inspect worktree status for %s: %w", rel, err)
	}
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return "", nil
	}
	line := text
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		line = text[:idx]
	}
	if len(line) < 2 {
		return "", fmt.Errorf("unexpected porcelain status %q for %s", line, rel)
	}
	return line[:2], nil
}

// matchSetAsideStash finds every stash entry whose message carries the token and
// returns the resolved stash@{N} ref of the single match. The count is authority
// for the exactly-one requirement; the positional ref is used only immediately
// by the caller for apply/drop, never stored as identity.
func (s *Store) matchSetAsideStash(repoPath, token string) (string, int, error) {
	out, err := setAsideGit(repoPath, "stash", "list")
	if err != nil {
		return "", 0, fmt.Errorf("list stashes: %w", err)
	}
	marker := setAsideStashPrefix + token
	ref := ""
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" || !strings.Contains(line, marker) {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		ref = strings.TrimSpace(line[:idx])
		count++
	}
	return ref, count, nil
}

// setAsideRepoRelative resolves an operator-supplied file argument (absolute or
// relative) to a clean, forward-slash, repo-relative path and refuses anything
// that escapes the target root.
func setAsideRepoRelative(root, file string) (string, error) {
	file = strings.TrimSpace(file)
	if file == "" {
		return "", fmt.Errorf("empty file path")
	}
	abs := filepath.Join(root, filepath.FromSlash(file))
	if filepath.IsAbs(file) {
		abs = filepath.Clean(file)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path escapes root")
	}
	return rel, nil
}

// setAsideGitBytes runs git with repository hooks disabled and returns raw
// stdout. A reference-transaction or pre-commit hook is an unreviewed mutation
// surface, so this adapter never inherits repository hook behavior (the
// worktree-retirement precedent).
func setAsideGitBytes(repoPath string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"-c", "core.hooksPath=" + os.DevNull, "-C", repoPath}, args...)
	cmd := exec.Command("git", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// setAsideGit is setAsideGitBytes for callers that want a trimmed string (refs,
// rev-parse, stash list); never use it where exact bytes matter.
func setAsideGit(repoPath string, args ...string) (string, error) {
	out, err := setAsideGitBytes(repoPath, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func newSetAsideToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate set-aside token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeSetAsideRecordAtomic(path string, record SetAsideRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "set-aside-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed away; cleans up on any error path
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readSetAsideRecord(path string) (SetAsideRecord, error) {
	var record SetAsideRecord
	body, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, fmt.Errorf("set-aside record %s invalid: %w", filepath.Base(path), err)
	}
	if record.SchemaVersion != SetAsideSchemaVersion {
		return record, fmt.Errorf("set-aside record %s has unexpected schema_version %q", filepath.Base(path), record.SchemaVersion)
	}
	return record, nil
}
