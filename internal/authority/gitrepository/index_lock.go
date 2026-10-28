package gitrepository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const IndexLockMinimumAge = 15 * time.Minute
const indexLockMaxBytes = 64 << 20

// IndexLockFileIdentity deliberately excludes access time: inspection itself
// may change it. Directory identity uses only device/inode/ownership/mode.
type IndexLockFileIdentity struct {
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
	UID        uint32 `json:"uid"`
	Links      uint64 `json:"links,omitempty"`
	Mode       uint32 `json:"mode"`
	Size       int64  `json:"size,omitempty"`
	ModifiedNS int64  `json:"modified_ns,omitempty"`
}

type IndexLockSnapshot struct {
	Repo           string                `json:"repo"`
	Root           IndexLockFileIdentity `json:"root"`
	GitDir         IndexLockFileIdentity `json:"git_dir"`
	Lock           IndexLockFileIdentity `json:"lock"`
	Index          IndexLockFileIdentity `json:"index"`
	Head           string                `json:"head"`
	HeadRef        string                `json:"head_ref"`
	IndexSHA256    string                `json:"index_sha256"`
	StagedSHA256   string                `json:"staged_sha256"`
	UnstagedSHA256 string                `json:"unstaged_sha256"`
	StatusSHA256   string                `json:"status_sha256"`
	WorktreeSHA256 string                `json:"worktree_sha256"`
}

// IndexLockState contains private recovery bytes; never print patches in CLI
// summaries or receipts. A snapshot is evidence, not permission to move a lock.
type IndexLockState struct {
	Snapshot      IndexLockSnapshot `json:"snapshot"`
	Index         []byte            `json:"index"`
	StagedPatch   []byte            `json:"staged_patch"`
	UnstagedPatch []byte            `json:"unstaged_patch"`
	Status        []byte            `json:"status"`
}

type IndexLockLiveness func(context.Context, ...string) error

// InspectIndexLock supports only primary standard checkouts and preserves both
// kinds of tracked WIP. Unknown liveness, special index modes and in-progress
// Git operations fail closed. It never refreshes the index or removes the lock.
func InspectIndexLock(ctx context.Context, repo string, now time.Time, idle IndexLockLiveness) (IndexLockState, error) {
	return inspectIndexLock(ctx, repo, now, idle, false)
}

func inspectIndexLock(ctx context.Context, repo string, now time.Time, idle IndexLockLiveness, absent bool) (IndexLockState, error) {
	var state IndexLockState
	if idle == nil || now.IsZero() {
		return state, fmt.Errorf("clock and liveness probe required")
	}
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return state, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return state, err
	}
	gitDir := filepath.Join(root, ".git")
	state.Snapshot.Repo = root
	if state.Snapshot.Root, err = indexLockIdentity(root, true); err != nil {
		return state, err
	}
	if state.Snapshot.GitDir, err = indexLockIdentity(gitDir, true); err != nil {
		return state, fmt.Errorf("primary checkout required: %w", err)
	}
	probePaths := []string{filepath.Join(gitDir, "index")}
	if !absent {
		probePaths = append(probePaths, filepath.Join(gitDir, "index.lock"))
	}
	if err = idle(ctx, probePaths...); err != nil {
		return state, fmt.Errorf("Git liveness unresolved: %w", err)
	}
	if err = indexLockIdleMetadata(gitDir); err != nil {
		return state, err
	}
	lockBytes, lockID, err := readIndexLockFile(filepath.Join(gitDir, "index.lock"))
	if absent && !os.IsNotExist(err) {
		return state, fmt.Errorf("replacement lock present or unreadable")
	}
	if err != nil && !(absent && os.IsNotExist(err)) {
		return state, err
	}
	state.Snapshot.Lock = lockID
	if !absent && (len(lockBytes) != 0 || now.Sub(time.Unix(0, lockID.ModifiedNS)) < IndexLockMinimumAge) {
		return state, fmt.Errorf("index lock must be empty and at least %s old", IndexLockMinimumAge)
	}
	state.Index, state.Snapshot.Index, err = readIndexLockFile(filepath.Join(gitDir, "index"))
	if err != nil {
		return state, err
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		cmd.Env = append(ReadEnvironment(), "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
		out := indexLockBoundedOutput{limit: indexLockMaxBytes}
		stderr := indexLockBoundedOutput{limit: 8192}
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil || stderr.Len() != 0 {
			return "", fmt.Errorf("Git index recovery inspection failed: %s", args[0])
		}
		return out.String(), nil
	}
	keys, err := git("config", "--name-only", "--list")
	if err != nil {
		return state, err
	}
	for _, key := range strings.Split(strings.ToLower(keys), "\n") {
		if strings.HasPrefix(key, "filter.") || key == "extensions.partialclone" {
			return state, fmt.Errorf("filtered or partial-clone checkout unsupported for read-only recovery")
		}
	}
	actualRoot, err := git("rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(actualRoot) != root {
		return state, fmt.Errorf("Git worktree root mismatch")
	}
	actualDir, err := git("rev-parse", "--absolute-git-dir")
	if err != nil || strings.TrimSpace(actualDir) != gitDir {
		return state, fmt.Errorf("Git directory mismatch")
	}
	head, err := git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !validObjectID(strings.TrimSpace(head)) {
		return state, fmt.Errorf("committed HEAD required")
	}
	state.Snapshot.Head = strings.TrimSpace(head)
	ref, err := git("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return state, fmt.Errorf("attached HEAD required")
	}
	state.Snapshot.HeadRef = strings.TrimSpace(ref)
	flags, err := git("ls-files", "-v", "-z")
	if err != nil {
		return state, err
	}
	for _, entry := range strings.Split(flags, "\x00") {
		if entry != "" && !strings.HasPrefix(entry, "H ") {
			return state, fmt.Errorf("unsupported index flags")
		}
	}
	entries, err := git("ls-files", "--stage", "-z")
	if err != nil {
		return state, err
	}
	for _, entry := range strings.Split(entries, "\x00") {
		if entry == "" {
			continue
		}
		meta, _, ok := strings.Cut(entry, "\t")
		parts := strings.Fields(meta)
		if !ok || len(parts) != 3 || (parts[0] != "100644" && parts[0] != "100755") || parts[2] != "0" {
			return state, fmt.Errorf("unsupported or unmerged index entry")
		}
	}
	state.Snapshot.WorktreeSHA256, err = indexLockWorktreeHash(root, entries)
	if err != nil {
		return state, err
	}
	untracked, err := git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || untracked != "" {
		return state, fmt.Errorf("untracked files or unreadable inventory; preserve them before recovery")
	}
	staged, err := git("diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if err != nil {
		return state, err
	}
	unstaged, err := git("diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--")
	if err != nil {
		return state, err
	}
	status, err := git("status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return state, err
	}
	state.StagedPatch, state.UnstagedPatch, state.Status = []byte(staged), []byte(unstaged), []byte(status)
	state.Snapshot.IndexSHA256 = indexLockHash(state.Index)
	state.Snapshot.StagedSHA256 = indexLockHash(state.StagedPatch)
	state.Snapshot.UnstagedSHA256 = indexLockHash(state.UnstagedPatch)
	state.Snapshot.StatusSHA256 = indexLockHash(state.Status)
	// Detect drift caused by another process while Git was reading. This check
	// is not exclusion: apply additionally requires an exclusive writer window.
	indexAfter, idAfter, err := readIndexLockFile(filepath.Join(gitDir, "index"))
	if err != nil || idAfter != state.Snapshot.Index || indexLockHash(indexAfter) != state.Snapshot.IndexSHA256 {
		return state, fmt.Errorf("index changed during inspection")
	}
	lockAfter, err := indexLockIdentity(filepath.Join(gitDir, "index.lock"), false)
	if (!absent && (err != nil || lockAfter != lockID)) || (absent && !os.IsNotExist(err)) {
		return state, fmt.Errorf("lock changed during inspection")
	}
	worktreeAfter, err := indexLockWorktreeHash(root, entries)
	if err != nil || worktreeAfter != state.Snapshot.WorktreeSHA256 {
		return state, fmt.Errorf("worktree changed during inspection")
	}
	headAfter, err := git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || headAfter != head {
		return state, fmt.Errorf("HEAD changed during inspection")
	}
	for path, expected := range map[string]IndexLockFileIdentity{root: state.Snapshot.Root, gitDir: state.Snapshot.GitDir} {
		actual, err := indexLockIdentity(path, true)
		if err != nil || actual != expected {
			return state, fmt.Errorf("repository directory changed during inspection")
		}
	}
	if _, err := marshalIndexLockBackup(state, indexLockMaxBytes); err != nil {
		return state, err
	}
	return state, nil
}

type indexLockBoundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *indexLockBoundedOutput) Len() int       { return b.buffer.Len() }
func (b *indexLockBoundedOutput) String() string { return b.buffer.String() }

func (b *indexLockBoundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("Git recovery output exceeds bound")
	}
	return b.buffer.Write(p)
}

func marshalIndexLockBackup(state IndexLockState, limit int) ([]byte, error) {
	body, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, fmt.Errorf("serialized Git recovery preimage exceeds reader bound; lock untouched")
	}
	return body, nil
}

func indexLockWorktreeHash(root, entries string) (string, error) {
	h := sha256.New()
	for _, entry := range strings.Split(entries, "\x00") {
		if entry == "" {
			continue
		}
		_, name, _ := strings.Cut(entry, "\t")
		if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return "", fmt.Errorf("unsafe tracked path")
		}
		path := filepath.Join(root, name)
		resolved, err := filepath.EvalSymlinks(path)
		if os.IsNotExist(err) {
			fmt.Fprintf(h, "%s\x00missing\x00", name)
			continue
		}
		if err != nil || resolved != path {
			return "", fmt.Errorf("unsupported working path")
		}
		data, id, err := readIndexLockFile(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", name, id.Mode, indexLockHash(data))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func indexLockHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func indexLockIdentity(path string, directory bool) (IndexLockFileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return IndexLockFileIdentity{}, err
	}
	return indexLockInfo(info, directory)
}

func indexLockInfo(info os.FileInfo, directory bool) (IndexLockFileIdentity, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Getuid()) || (directory && !info.IsDir()) || (!directory && (!info.Mode().IsRegular() || s.Nlink != 1)) {
		return IndexLockFileIdentity{}, fmt.Errorf("unsupported file type, owner or link count")
	}
	id := IndexLockFileIdentity{Device: uint64(s.Dev), Inode: uint64(s.Ino), UID: s.Uid, Mode: uint32(info.Mode())}
	if !directory {
		id.Links, id.Size, id.ModifiedNS = uint64(s.Nlink), info.Size(), info.ModTime().UnixNano()
	}
	return id, nil
}

func readIndexLockFile(path string) ([]byte, IndexLockFileIdentity, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, IndexLockFileIdentity{}, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, IndexLockFileIdentity{}, err
	}
	id, err := indexLockInfo(info, false)
	if err != nil {
		return nil, id, err
	}
	data, err := io.ReadAll(io.LimitReader(f, indexLockMaxBytes+1))
	if err == nil && len(data) > indexLockMaxBytes {
		err = fmt.Errorf("Git recovery file exceeds size bound")
	}
	after, statErr := indexLockIdentity(path, false)
	if statErr != nil || after != id {
		return nil, id, fmt.Errorf("file changed during read")
	}
	return data, id, err
}

func indexLockIdleMetadata(dir string) error {
	for _, name := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG", "rebase-merge", "rebase-apply", "sequencer", "shallow", "commondir"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			return fmt.Errorf("active or unsupported Git metadata: %s", name)
		}
	}
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		if entry.IsDir() && (entry.Name() == "objects" || entry.Name() == "worktrees" || entry.Name() == "hooks" || entry.Name() == "logs") {
			return filepath.SkipDir
		}
		if strings.HasSuffix(entry.Name(), ".lock") && path != filepath.Join(dir, "index.lock") {
			return fmt.Errorf("other Git lock present")
		}
		return nil
	})
}
