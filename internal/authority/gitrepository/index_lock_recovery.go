package gitrepository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"time"

	"golang.org/x/sys/unix"
)

type IndexLockRecoveryResult struct {
	RecoveryDir string `json:"recovery_dir"`
	Phase       string `json:"phase"`
	Changed     bool   `json:"changed"`
}

// LSOFIndexLockLiveness treats warnings, unavailable probes and unfamiliar
// output as unknown, not idle. It reports no process command lines.
func LSOFIndexLockLiveness(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return fmt.Errorf("exact Git paths required")
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("absolute Git path required")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", append([]string{"-n", "-P", "-F", "p", "--"}, paths...)...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 && out.Len() == 0 && stderr.Len() == 0 {
		return nil
	}
	return fmt.Errorf("Git file holder present or liveness probe inconclusive")
}

// InspectRecoveredIndexLock proves absence of a replacement lock and exact
// preservation of HEAD, index and tracked working bytes. It never retries a move.
func InspectRecoveredIndexLock(ctx context.Context, expected IndexLockSnapshot, idle IndexLockLiveness) (IndexLockState, error) {
	state, err := inspectIndexLock(ctx, expected.Repo, time.Now(), idle, true)
	if err != nil {
		return state, err
	}
	expected.Lock = IndexLockFileIdentity{}
	if state.Snapshot != expected {
		return state, fmt.Errorf("repository changed after lock recovery")
	}
	return state, nil
}

// RecoverIndexLock is an effect primitive, not an authorization door. Its sole
// production caller is the authority adapter, after reviewed exclusive-window
// admission and a durable work reservation. There is no inode-CAS rename on
// POSIX: unrelated writers MUST remain paused for the entire recovery window.
func RecoverIndexLock(ctx context.Context, state IndexLockState, idle IndexLockLiveness, authorize func() error) (IndexLockRecoveryResult, error) {
	return recoverIndexLock(ctx, state, idle, authorize, nil)
}

// RecoverIndexLockJournaled requires the owner to persist the recovery locator
// before any lock movement. Restart reconciles that attempt; it never moves again.
func RecoverIndexLockJournaled(ctx context.Context, state IndexLockState, idle IndexLockLiveness, authorize func() error, journal func(IndexLockRecoveryResult) error) (IndexLockRecoveryResult, error) {
	if journal == nil {
		return IndexLockRecoveryResult{}, fmt.Errorf("durable recovery locator required")
	}
	return recoverIndexLockOwned(ctx, state, idle, authorize, journal, nil)
}

func recoverIndexLock(ctx context.Context, state IndexLockState, idle IndexLockLiveness, authorize func() error, afterMove func() error) (IndexLockRecoveryResult, error) {
	return recoverIndexLockOwned(ctx, state, idle, authorize, nil, afterMove)
}

func recoverIndexLockOwned(ctx context.Context, state IndexLockState, idle IndexLockLiveness, authorize func() error, journal func(IndexLockRecoveryResult) error, afterMove func() error) (IndexLockRecoveryResult, error) {
	var result IndexLockRecoveryResult
	if authorize == nil {
		return result, fmt.Errorf("fresh recovery authorization required")
	}
	if err := authorize(); err != nil {
		return result, err
	}
	current, err := InspectIndexLock(ctx, state.Snapshot.Repo, time.Now(), idle)
	if err != nil {
		return result, err
	}
	if current.Snapshot != state.Snapshot || !bytes.Equal(current.Index, state.Index) || !bytes.Equal(current.StagedPatch, state.StagedPatch) || !bytes.Equal(current.UnstagedPatch, state.UnstagedPatch) {
		return result, fmt.Errorf("reviewed repository snapshot changed")
	}
	root, err := openIndexLockDirectory(unix.AT_FDCWD, state.Snapshot.Repo)
	if err != nil {
		return result, err
	}
	defer root.Close()
	gitDir, err := openIndexLockDirectory(int(root.Fd()), ".git")
	if err != nil {
		return result, err
	}
	defer gitDir.Close()
	for f, expected := range map[*os.File]IndexLockFileIdentity{root: state.Snapshot.Root, gitDir: state.Snapshot.GitDir} {
		info, err := f.Stat()
		if err != nil {
			return result, err
		}
		actual, err := indexLockInfo(info, true)
		if err != nil || actual != expected {
			return result, fmt.Errorf("pinned Git directory identity changed")
		}
	}
	const area = "compound-index-lock-recovery"
	if err := unix.Mkdirat(int(gitDir.Fd()), area, 0700); err != nil && err != unix.EEXIST {
		return result, err
	}
	parent, err := openIndexLockDirectory(int(gitDir.Fd()), area)
	if err != nil {
		return result, err
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil {
		return result, err
	}
	if _, err := indexLockInfo(info, true); err != nil || info.Mode().Perm() != 0700 {
		return result, fmt.Errorf("private recovery directory required")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return result, err
	}
	name := hex.EncodeToString(nonce[:])
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return result, err
	}
	dest, err := openIndexLockDirectory(int(parent.Fd()), name)
	if err != nil {
		return result, err
	}
	defer dest.Close()
	result.RecoveryDir = filepath.Join(state.Snapshot.Repo, ".git", area, name)
	result.Phase = "prepared"
	if err := syncIndexLockJSON(dest, "preimage.json", state); err != nil {
		return result, err
	}
	if err := parent.Sync(); err != nil {
		return result, err
	}
	if err := gitDir.Sync(); err != nil {
		return result, err
	}
	if journal != nil {
		if err := journal(result); err != nil {
			return result, err
		}
	}
	// Reauthorize after potentially slow backup I/O. An identical reservation
	// alone is not a fresh policy, lease or exclusive-window check.
	if err := authorize(); err != nil {
		return result, err
	}
	current, err = InspectIndexLock(ctx, state.Snapshot.Repo, time.Now(), idle)
	if err != nil {
		return result, err
	}
	if current.Snapshot != state.Snapshot {
		return result, fmt.Errorf("repository drift before lock move")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := syncIndexLockJSON(dest, "move-started.json", state.Snapshot); err != nil {
		return result, err
	}
	if err := idle(ctx, filepath.Join(state.Snapshot.Repo, ".git/index"), filepath.Join(state.Snapshot.Repo, ".git/index.lock")); err != nil {
		return result, err
	}
	if err := authorize(); err != nil {
		return result, err
	}
	last, err := indexLockIdentity(filepath.Join(state.Snapshot.Repo, ".git/index.lock"), false)
	if err != nil || last != state.Snapshot.Lock {
		return result, fmt.Errorf("lock identity changed at final boundary")
	}
	// Directory FDs keep the rename inside the already-pinned checkout. The
	// destination is a new private directory; never overwrite an old recovery.
	if err := renameIndexLockExclusive(int(gitDir.Fd()), "index.lock", int(dest.Fd()), "lock.preimage"); err != nil {
		return result, err
	}
	result.Changed = true
	result.Phase = "moved"
	if err := dest.Sync(); err != nil {
		return result, err
	}
	if err := gitDir.Sync(); err != nil {
		return result, err
	}
	if afterMove != nil {
		if err := afterMove(); err != nil {
			return result, err
		}
	}
	// Post-move mismatch is a partial failure, never an automatic rollback:
	// another writer may already have installed a new index.lock or index.
	fd, err := unix.Openat(int(dest.Fd()), "lock.preimage", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return result, err
	}
	moved := os.NewFile(uintptr(fd), "lock.preimage")
	defer moved.Close()
	movedInfo, err := moved.Stat()
	if err != nil {
		return result, err
	}
	id, err := indexLockInfo(movedInfo, false)
	if err != nil || id != state.Snapshot.Lock {
		return result, fmt.Errorf("displaced lock identity mismatch; retained for operator reconciliation")
	}
	if _, err := InspectRecoveredIndexLock(ctx, state.Snapshot, idle); err != nil {
		return result, err
	}
	if err := syncIndexLockJSON(dest, "verified.json", state.Snapshot); err != nil {
		return result, err
	}
	result.Phase = "verified"
	return result, nil
}

// VerifyIndexLockRecoveryEvidence is a read-only crash-reconciliation check.
// It never removes a new lock, restores an old index, or repeats a rename.
func VerifyIndexLockRecoveryEvidence(ctx context.Context, state IndexLockState, result IndexLockRecoveryResult, idle IndexLockLiveness) error {
	if err := validateIndexLockRecoveryPreimages(state, result); err != nil {
		return err
	}
	_, err := InspectRecoveredIndexLock(ctx, state.Snapshot, idle)
	return err
}

// ValidateIndexLockRecoveryEvidence checks durable historical verification
// without asserting that the repository is still unchanged at replay time.
func ValidateIndexLockRecoveryEvidence(state IndexLockState, result IndexLockRecoveryResult) error {
	if err := validateIndexLockRecoveryPreimages(state, result); err != nil {
		return err
	}
	body, _, err := readIndexLockFile(filepath.Join(result.RecoveryDir, "verified.json"))
	if err != nil {
		return err
	}
	var verified IndexLockSnapshot
	if err := json.Unmarshal(body, &verified); err != nil {
		return err
	}
	if verified != state.Snapshot {
		return fmt.Errorf("retained recovery verification mismatch")
	}
	return nil
}

func validateIndexLockRecoveryPreimages(state IndexLockState, result IndexLockRecoveryResult) error {
	base := filepath.Join(state.Snapshot.Repo, ".git", "compound-index-lock-recovery")
	if filepath.Dir(result.RecoveryDir) != base || len(filepath.Base(result.RecoveryDir)) != 32 {
		return fmt.Errorf("invalid recovery locator")
	}
	resolved, err := filepath.EvalSymlinks(result.RecoveryDir)
	if err != nil || resolved != result.RecoveryDir {
		return fmt.Errorf("recovery directory changed")
	}
	body, _, err := readIndexLockFile(filepath.Join(result.RecoveryDir, "preimage.json"))
	if err != nil {
		return err
	}
	var saved IndexLockState
	if err := json.Unmarshal(body, &saved); err != nil {
		return err
	}
	if !reflect.DeepEqual(saved, state) {
		return fmt.Errorf("recovery preimage mismatch")
	}
	lock, id, err := readIndexLockFile(filepath.Join(result.RecoveryDir, "lock.preimage"))
	if err != nil || len(lock) != 0 || id != state.Snapshot.Lock {
		return fmt.Errorf("displaced lock evidence mismatch")
	}
	return nil
}

func openIndexLockDirectory(parent int, path string) (*os.File, error) {
	fd, err := unix.Openat(parent, path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func syncIndexLockJSON(dir *os.File, name string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return dir.Sync()
}
