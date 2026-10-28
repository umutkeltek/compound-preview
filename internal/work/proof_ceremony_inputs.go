package work

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ProofCeremonyOwnedIgnoredInputs are the gitignored paths a sealed proof checkout
// needs but a git clone cannot carry into it.
var ProofCeremonyOwnedIgnoredInputs = []string{
	"docs_private/leak-tokens.txt",
	"docs_private/source-leak-tokens.txt",
	"docs_private/plan",
	"console/node_modules",
	"docs_private/profiles",
}

// CopyProofCeremonyInputs snapshots each ceremony input from sourceRepo into
// sealedRepo. A fresh linked worktree carries no ignored files, so an input it
// lacks comes from the repository's main checkout; an input neither has is
// skipped. command builds each subprocess so the caller's execution barrier and
// cancellation bind it. Copied regular files get fresh access times so temporary
// cleanup does not mistake APFS clones with inherited timestamps for old inputs.
func CopyProofCeremonyInputs(
	ctx context.Context,
	sourceRepo string,
	sealedRepo string,
	command func(context.Context, string, ...string) *exec.Cmd,
) error {
	mainCheckout := proofMainCheckout(ctx, sourceRepo, command)
	for _, relativePath := range ProofCeremonyOwnedIgnoredInputs {
		sourcePath := ""
		for _, root := range []string{sourceRepo, mainCheckout} {
			if root == "" {
				continue
			}
			candidate := filepath.Join(root, filepath.FromSlash(relativePath))
			if _, err := os.Lstat(candidate); err == nil {
				sourcePath = candidate
				break
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect proof ceremony input %s: %w", relativePath, err)
			}
		}
		if sourcePath == "" {
			continue
		}
		if err := validateProofCeremonyInputSnapshot(sourcePath); err != nil {
			return fmt.Errorf("validate proof ceremony input %s: %w", relativePath, err)
		}
		destinationPath := filepath.Join(sealedRepo, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
			return fmt.Errorf("prepare proof ceremony input %s: %w", relativePath, err)
		}
		args := []string{"-R"}
		if runtime.GOOS == "darwin" {
			args = []string{"-Rc"}
		}
		args = append(args, sourcePath, destinationPath)
		if output, err := command(ctx, "cp", args...).CombinedOutput(); err != nil {
			return fmt.Errorf(
				"copy proof ceremony input %s: %w: %s",
				relativePath,
				err,
				strings.TrimSpace(string(output)),
			)
		}
		if err := validateProofCeremonyInputSnapshot(destinationPath); err != nil {
			return fmt.Errorf("validate copied proof ceremony input %s: %w", relativePath, err)
		}
		if err := refreshProofCeremonyInputAccess(ctx, destinationPath); err != nil {
			return fmt.Errorf("refresh copied proof ceremony input %s: %w", relativePath, err)
		}
	}
	return nil
}

func refreshProofCeremonyInputAccess(ctx context.Context, root string) error {
	now := time.Now()
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		// WalkDir does not follow symlinks. Refresh only the copied regular files,
		// preserving their modification times and never touching link targets.
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chtimes(path, now, info.ModTime())
	})
}

// proofMainCheckout returns the main checkout of the repository sourceRepo belongs
// to, or "" when git cannot say or the repository is bare.
func proofMainCheckout(
	ctx context.Context,
	sourceRepo string,
	command func(context.Context, string, ...string) *exec.Cmd,
) string {
	output, err := command(ctx, "git", "-C", sourceRepo, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	commonDir := strings.TrimSpace(string(output))
	if filepath.Base(commonDir) != ".git" {
		return ""
	}
	return filepath.Dir(commonDir)
}

func validateProofCeremonyInputSnapshot(root string) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("top-level ceremony input is a symlink")
	}
	if !rootInfo.IsDir() {
		if !rootInfo.Mode().IsRegular() {
			return fmt.Errorf("top-level ceremony input is not a regular file or directory")
		}
		return nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve ceremony input root: %w", err)
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if info.IsDir() || info.Mode().IsRegular() {
				return nil
			}
			return fmt.Errorf("%s is not a regular file, directory, or rooted symlink", path)
		}
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			return fmt.Errorf("%s is an absolute symlink", path)
		}
		resolvedTarget, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", path, err)
		}
		relativeTarget, err := filepath.Rel(resolvedRoot, resolvedTarget)
		if err != nil {
			return fmt.Errorf("relativize symlink %s: %w", path, err)
		}
		if filepath.IsAbs(relativeTarget) ||
			relativeTarget == ".." ||
			strings.HasPrefix(relativeTarget, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("%s is a symlink escaping ceremony input root", path)
		}
		return nil
	})
}
