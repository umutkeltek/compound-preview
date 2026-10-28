// Package gitrepository implements the source-control primitives owned by the
// authority layer without importing the higher-level authority or work
// packages. This keeps repository identity and Git ref locking reusable at the
// WorkEnvelope completion boundary without creating an authority <-> work
// import cycle.
package gitrepository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Identity is stable for ordinary descendants of Ref while binding the
// repository's object format, root lineage, and canonical origin identity.
// The worktree path is deliberately excluded: paths locate a checkout, but do
// not prove which repository currently occupies it.
type Identity struct {
	Fingerprint    string   `json:"fingerprint"`
	ObjectFormat   string   `json:"object_format"`
	LineageRoots   []string `json:"lineage_roots"`
	RemoteIdentity string   `json:"remote_identity,omitempty"`
}

type identityContent struct {
	ObjectFormat   string   `json:"object_format"`
	LineageRoots   []string `json:"lineage_roots"`
	RemoteIdentity string   `json:"remote_identity"`
}

// ReadIdentity derives a collision-resistant logical repository identity at
// ref. Using the bound expected-base commit as ref makes the identity stable as
// branches advance, while a different lineage or canonical origin fails closed.
func ReadIdentity(repo, ref string) (Identity, error) {
	return ReadIdentityContext(context.Background(), repo, ref)
}

// ReadIdentityContext preserves the caller's cancellation and the existing local upper bound.
func ReadIdentityContext(parent context.Context, repo, ref string) (Identity, error) {
	repo = strings.TrimSpace(repo)
	ref = strings.TrimSpace(ref)
	if repo == "" {
		return Identity{}, fmt.Errorf("repository path required")
	}
	if ref == "" {
		return Identity{}, fmt.Errorf("repository identity ref required")
	}

	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	objectFormat, err := gitOutput(ctx, repo, "rev-parse", "--show-object-format")
	if err != nil {
		return Identity{}, fmt.Errorf("read Git object format: %w", err)
	}
	return readIdentityWithObjectFormat(ctx, repo, ref, strings.TrimSpace(objectFormat))
}

// readIdentityWithObjectFormat consumes the format just observed by this same
// request. Standalone identity reads still resolve their format freshly.
func readIdentityWithObjectFormat(ctx context.Context, repo, ref, objectFormat string) (Identity, error) {
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return Identity{}, fmt.Errorf("unsupported Git object format %q", objectFormat)
	}

	rootOutput, err := gitOutput(ctx, repo, "rev-list", "--max-parents=0", ref)
	if err != nil {
		return Identity{}, fmt.Errorf("read repository lineage at %s: %w", ref, err)
	}
	roots := compactLines(rootOutput)
	if len(roots) == 0 {
		return Identity{}, fmt.Errorf("repository lineage at %s has no root commit", ref)
	}
	for _, root := range roots {
		if !validObjectID(root) {
			return Identity{}, fmt.Errorf("repository lineage contains invalid root object id %q", root)
		}
	}
	sort.Strings(roots)

	remote, err := optionalGitOutput(ctx, repo, "config", "--get", "remote.origin.url")
	if err != nil {
		return Identity{}, fmt.Errorf("read canonical Git origin: %w", err)
	}
	content := identityContent{
		ObjectFormat:   objectFormat,
		LineageRoots:   roots,
		RemoteIdentity: strings.TrimSpace(remote),
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return Identity{}, err
	}
	sum := sha256.Sum256(encoded)
	return Identity{
		Fingerprint:    hex.EncodeToString(sum[:]),
		ObjectFormat:   content.ObjectFormat,
		LineageRoots:   append([]string{}, content.LineageRoots...),
		RemoteIdentity: content.RemoteIdentity,
	}, nil
}

func compactLines(value string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func optionalGitOutput(ctx context.Context, repo string, args ...string) (string, error) {
	out, err := gitOutput(ctx, repo, args...)
	if err == nil {
		return out, nil
	}
	var commandErr *gitCommandError
	if errors.As(err, &commandErr) && commandErr.exitCode == 1 {
		return "", nil
	}
	return "", err
}

type gitCommandError struct {
	args     []string
	exitCode int
	output   string
}

func (e *gitCommandError) Error() string {
	return strings.Join(e.args, " ") + ": " + e.output
}

func gitOutput(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Env = ReadEnvironment()
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	if err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return "", &gitCommandError{args: append([]string{}, args...), exitCode: exitCode, output: message}
	}
	return string(out), nil
}

// ContextWithCommandFactory retains the execution owner's cancellation and child isolation
// when repository reads are preparation for a governed operation.
func ContextWithCommandFactory(ctx context.Context, factory func(context.Context, string, ...string) *exec.Cmd) context.Context {
	return context.WithValue(ctx, commandFactoryKey{}, factory)
}

type commandFactoryKey struct{}

// CommandContext delegates to the caller's execution owner, or the standard direct-child command.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	if factory, ok := ctx.Value(commandFactoryKey{}).(func(context.Context, string, ...string) *exec.Cmd); ok {
		return factory(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}
