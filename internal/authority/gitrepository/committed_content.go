package gitrepository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Committed content selection is the one exact inventory every committed
// reader shares. It reads the object database at two immutable commits and
// never consults HEAD, the index or the checkout, so a dirty worktree, a moved
// ref or a mutated caller file cannot change what a sealed proof scanned.
const (
	committedContentMaxInventoryBytes = 8 << 20
	committedContentMaxPaths          = 200000
	committedContentMaxDiffCheckBytes = 1 << 20
)

// CommittedContentPaths returns the exact content-bearing paths selected between
// two immutable commits. Rename detection stays off, the diff filter is ACM
// only, and every path arrives raw and NUL-delimited, so leading/trailing
// whitespace and embedded newlines survive byte-for-byte. Both commits must be
// full object IDs that resolve to existing commits.
func CommittedContentPaths(ctx context.Context, repo, baseSHA, headSHA string) ([]string, error) {
	if err := requireCommittedCommits(ctx, repo, baseSHA, headSHA); err != nil {
		return nil, err
	}
	out, err := committedGitOutput(ctx, repo, "diff", "--no-renames", "--name-only", "--diff-filter=ACM", "-z", baseSHA+"..."+headSHA, "--")
	if err != nil {
		return nil, err
	}
	if len(out) > committedContentMaxInventoryBytes {
		return nil, fmt.Errorf("committed content inventory exceeds %d bytes", committedContentMaxInventoryBytes)
	}
	paths := make([]string, 0, 16)
	for _, part := range bytes.Split(out, []byte{0}) {
		if len(part) == 0 {
			continue
		}
		if len(paths) >= committedContentMaxPaths {
			return nil, fmt.Errorf("committed content inventory exceeds %d paths", committedContentMaxPaths)
		}
		paths = append(paths, string(part))
	}
	return paths, nil
}

// CommittedAncestor checks exact immutable commit ancestry with the same
// caller deadline, command factory and sanitized environment as content reads.
func CommittedAncestor(ctx context.Context, repo, ancestorSHA, descendantSHA string) (bool, error) {
	if err := requireCommittedCommits(ctx, repo, ancestorSHA, descendantSHA); err != nil {
		return false, err
	}
	_, err := committedGitOutput(ctx, repo, "merge-base", "--is-ancestor", ancestorSHA, descendantSHA)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// CommittedFileBody reads the exact committed body of one path at an immutable
// commit. The bytes come from the object database, so a checkout that no longer
// matches the commit can never change what a sealed proof scanned.
func CommittedFileBody(ctx context.Context, repo, headSHA, path string) ([]byte, error) {
	if !validObjectID(headSHA) {
		return nil, fmt.Errorf("committed content head must be a full Git object ID")
	}
	if path == "" || strings.ContainsRune(path, 0) {
		return nil, fmt.Errorf("committed content path is invalid")
	}
	body, err := committedGitOutput(ctx, repo, "show", headSHA+":"+path)
	if err != nil {
		return nil, fmt.Errorf("read committed body %q at %s: %w", path, headSHA, err)
	}
	return body, nil
}

// CommittedDiffCheck runs the canonical `git diff --check BASE...HEAD` over two
// immutable commits and refuses every whitespace error that range introduces.
// Git reports those problems on stdout with exit status 2, so the report is
// read from the inventory stream and never from stderr. Versioned attributes
// come from the reviewed head, not the mutable caller checkout.
func CommittedDiffCheck(ctx context.Context, repo, baseSHA, headSHA string) error {
	if err := requireCommittedCommits(ctx, repo, baseSHA, headSHA); err != nil {
		return err
	}
	out, err := committedGitOutput(ctx, repo, "--attr-source="+headSHA, "diff", "--check", baseSHA+"..."+headSHA, "--")
	if unsupportedAttributeSource(err) {
		out, err = committedDiffCheckOlderGit(ctx, repo, baseSHA, headSHA)
	}
	if len(out) > committedContentMaxDiffCheckBytes {
		return fmt.Errorf("committed diff-check report exceeds %d bytes", committedContentMaxDiffCheckBytes)
	}
	report := strings.TrimSpace(string(out))
	if err != nil {
		if report == "" {
			return fmt.Errorf("git diff --check %s...%s: %w", baseSHA, headSHA, err)
		}
		return fmt.Errorf("git diff --check %s...%s reported whitespace errors: %s", baseSHA, headSHA, report)
	}
	if report != "" {
		return fmt.Errorf("git diff --check %s...%s reported whitespace errors: %s", baseSHA, headSHA, report)
	}
	return nil
}

// requireCommittedCommits refuses anything but full object IDs that resolve to
// existing commits, so a moved ref or a truncated SHA can never be read as
// committed content.
func requireCommittedCommits(ctx context.Context, repo, baseSHA, headSHA string) error {
	for _, sha := range []string{baseSHA, headSHA} {
		if !validObjectID(sha) {
			return fmt.Errorf("committed content range must use full Git object IDs")
		}
		resolved, err := committedGitOutput(ctx, repo, "rev-parse", "--verify", sha+"^{commit}")
		if err != nil {
			return fmt.Errorf("resolve committed object %s: %w", sha, err)
		}
		if strings.TrimSpace(string(resolved)) != sha {
			return fmt.Errorf("committed object %s did not resolve to itself", sha)
		}
	}
	return nil
}

// committedGitOutput runs one read-only Git command in the repository through
// the caller's execution owner, so a governed proof keeps its child-process
// isolation and execution barrier. The read environment is sanitized so an
// ambient Git selector cannot substitute another index, object directory or
// worktree, and stderr is kept out of the inventory stream.
func committedGitOutput(ctx context.Context, repo string, args ...string) ([]byte, error) {
	return committedGitOutputEnvironment(ctx, repo, ReadEnvironment(), args...)
}

// env is built by this package from sanitized reads and, for compatibility,
// its own private attribute index. Caller Git selectors never enter this path.
func committedGitOutputEnvironment(ctx context.Context, repo string, env []string, args ...string) ([]byte, error) {
	cmd := CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
