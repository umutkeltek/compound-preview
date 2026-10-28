package gitrepository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Older Git versions lack --attr-source. Fall back only for its explicit
// unsupported-option refusal, never for a failed object read or diff check.
func unsupportedAttributeSource(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 129 &&
		strings.Contains(err.Error(), "unknown option: --attr-source=")
}

// Git uses index attributes when the worktree has no .gitattributes. Seed a
// private index from the reviewed head and use an empty private worktree so
// neither the caller's staged work nor its checkout can change that selection.
// read-tree writes only this temporary index; the repository and caller index
// remain untouched. All commands retain the caller's execution owner/deadline.
func committedDiffCheckOlderGit(ctx context.Context, repo, baseSHA, headSHA string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	gitDir, err := committedGitOutput(ctx, repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "compound-committed-attributes-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0700); err != nil {
		return nil, err
	}
	// Diff between trees need not chdir to GIT_WORK_TREE. Run from the
	// empty directory explicitly, selecting only the resolved repository.
	env := append(ReadEnvironment(),
		"GIT_DIR="+strings.TrimSuffix(string(gitDir), "\n"),
		"GIT_INDEX_FILE="+filepath.Join(root, "index"),
		"GIT_WORK_TREE="+worktree)
	// These are private-index reads, not caller index updates. Prevent
	// Git from invoking caller hooks or a filesystem-monitor callback.
	readPrivateIndex := func(args ...string) ([]byte, error) {
		options := []string{"-c", "core.hooksPath=" + worktree, "-c", "core.fsmonitor=false"}
		return committedGitOutputEnvironment(ctx, worktree, env, append(options, args...)...)
	}
	if _, err := readPrivateIndex("read-tree", headSHA); err != nil {
		return nil, err
	}
	// A tree-to-tree diff need not load an index. Compare the reviewed
	// private index to the same merge base instead, forcing Git to load its
	// versioned attributes as well as its content.
	mergeBase, err := readPrivateIndex("merge-base", baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(string(mergeBase))
	if !validObjectID(base) {
		return nil, fmt.Errorf("committed diff-check merge base is not a full Git object ID")
	}
	return readPrivateIndex("diff", "--cached", "--check", base, "--")
}
