package contract

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"golang.org/x/sys/unix"
)

// This identity describes the actual checkout, never a remote URL or repo name.
type linkedWorktreeIdentity struct{ TopLevel, GitDir, CommonDir string }

// One aggregate deadline covers candidate discovery and every registered root.
const gitMetadataTimeout = 5 * time.Second

func canonicalGitPath(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(abs)
	if os.IsNotExist(err) {
		return filepath.Clean(abs), nil
	}
	if err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}

// readGitCheckoutIdentity uses one bounded Git query, then verifies reciprocal
// linked-worktree metadata. Primary checkouts return their identity too, so
// registry matching never repeats discovery.
func readGitCheckoutIdentity(ctx context.Context, cwd string) (linkedWorktreeIdentity, bool, error) {
	var zero linkedWorktreeIdentity
	cwd, err := canonicalGitPath(cwd)
	if err != nil {
		return zero, false, err
	}
	hasGit := false
	for dir := cwd; ; dir = filepath.Dir(dir) {
		path := filepath.Join(dir, ".git")
		info, statErr := os.Lstat(path)
		if statErr == nil {
			hasGit = true
			if !info.IsDir() {
				if _, err := readRegularGitMetadata(path); err != nil {
					return zero, false, fmt.Errorf("repo kind: invalid gitfile: %w", err)
				}
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return zero, false, statErr
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	cmd.Env = gitrepository.ReadEnvironment()
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return zero, false, fmt.Errorf("repo kind: metadata deadline: %w", ctx.Err())
	}
	if err != nil {
		if hasGit {
			return zero, false, fmt.Errorf("repo kind: unreadable Git metadata: %w", err)
		}
		return zero, false, nil
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(parts) != 3 {
		return zero, false, fmt.Errorf("repo kind: unexpected Git identity")
	}
	for i := range parts {
		parts[i], err = canonicalGitPath(parts[i])
		if err != nil {
			return zero, false, err
		}
	}
	id := linkedWorktreeIdentity{TopLevel: parts[0], GitDir: parts[1], CommonDir: parts[2]}
	if id.GitDir == id.CommonDir {
		return id, false, nil
	}
	gitfile := filepath.Join(id.TopLevel, ".git")
	data, err := readRegularGitMetadata(gitfile)
	if err != nil {
		return zero, false, err
	}
	pointer, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok || strings.TrimSpace(pointer) == "" {
		return zero, false, fmt.Errorf("repo kind: malformed gitfile")
	}
	pointer = strings.TrimSpace(pointer)
	if !filepath.IsAbs(pointer) {
		pointer = filepath.Join(id.TopLevel, pointer)
	}
	declared, err := canonicalGitPath(pointer)
	if err != nil || declared != id.GitDir {
		return zero, false, fmt.Errorf("repo kind: gitfile disagrees with Git identity")
	}
	if filepath.Dir(id.GitDir) != filepath.Join(id.CommonDir, "worktrees") {
		return zero, false, fmt.Errorf("repo kind: administrative directory is outside common worktrees")
	}
	data, err = readRegularGitMetadata(filepath.Join(id.GitDir, "gitdir"))
	if err != nil {
		return zero, false, err
	}
	back := strings.TrimSpace(string(data))
	if !filepath.IsAbs(back) {
		back = filepath.Join(id.GitDir, back)
	}
	back, err = canonicalGitPath(back)
	if err != nil || back != gitfile {
		return zero, false, fmt.Errorf("repo kind: administrative back-pointer does not name this checkout")
	}
	return id, true, nil
}

func detectLinkedWorktree(cwd string) (linkedWorktreeIdentity, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitMetadataTimeout)
	defer cancel()
	return readGitCheckoutIdentity(ctx, cwd)
}

// Refuse symlinks, special files, replacements and oversized metadata.
// NONBLOCK prevents a raced FIFO from hanging the reader.
func readRegularGitMetadata(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("metadata is not a regular file: %s", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("metadata identity changed: %s", path)
	}
	const maxMetadata = 16384
	data, err := io.ReadAll(io.LimitReader(f, maxMetadata+1))
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, after) || !after.Mode().IsRegular() || len(data) > maxMetadata {
		return nil, fmt.Errorf("metadata changed or exceeds size limit: %s", path)
	}
	return data, nil
}

func resolveRepoKindFromLinkedWorktree(reg RepoKindRegistry, registryPath, cwd string) (string, string, error) {
	if len(reg.Repos) == 0 {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitMetadataTimeout)
	defer cancel()
	wt, linked, err := readGitCheckoutIdentity(ctx, cwd)
	if err != nil {
		return "", "", err
	}
	if !linked {
		return "", "", nil
	}
	kinds := map[string][]string{}
	// A cached empty identity represents a root that was checked and did not match.
	checked := map[string]linkedWorktreeIdentity{}
	for _, repo := range reg.Repos {
		for _, rawRoot := range repo.Roots {
			root := expandPath(rawRoot)
			if !filepath.IsAbs(root) && registryPath != "" {
				root = filepath.Join(filepath.Dir(registryPath), root)
			}
			root, err = canonicalContainmentPath(root)
			if err != nil {
				continue
			}
			id, seen := checked[root]
			if !seen {
				// A subtree declaration must not widen to the repository. Actual checkout
				// roots have their own Git entry; unrelated subdirectories need no subprocess.
				if _, statErr := os.Lstat(filepath.Join(root, ".git")); statErr == nil {
					id, _, err = readGitCheckoutIdentity(ctx, root)
					if ctx.Err() != nil {
						return "", "", fmt.Errorf("repo kind: registry metadata deadline: %w", ctx.Err())
					}
					if err != nil {
						id = linkedWorktreeIdentity{}
					}
				}
				checked[root] = id
			}
			if id.TopLevel == root && id.CommonDir == wt.CommonDir {
				kinds[repo.Kind] = append(kinds[repo.Kind], root)
			}
		}
	}
	names := make([]string, 0, len(kinds))
	for kind := range kinds {
		names = append(names, kind)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", "", nil
	}
	if len(names) > 1 {
		return "", "", fmt.Errorf("repo kind: linked worktree matches conflicting registered kinds %s", strings.Join(names, ", "))
	}
	kind := names[0]
	sort.Strings(kinds[kind])
	return kind, "repo-kind-linked-worktree:" + registryPath + " -> " + kinds[kind][0], nil
}
