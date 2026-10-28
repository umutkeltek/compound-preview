package proofscope

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// MaxGoSourceBytes bounds the head Go source DeriveGit reads.
const MaxGoSourceBytes = 256 << 20

var fullSHA = regexp.MustCompile("^(?:[0-9a-f]{40}|[0-9a-f]{64})$")

// DeriveGit derives the scope from git objects in repoDir: never the worktree,
// never COMPOUND_HOME. It returns errors and lets the caller decide.
func DeriveGit(ctx context.Context, repoDir, baseSHA, headSHA string) (Scope, error) {
	for _, sha := range []string{baseSHA, headSHA} {
		if !fullSHA.MatchString(sha) {
			return Scope{}, fmt.Errorf("proofscope: %q is not a full commit SHA", sha)
		}
		out, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
		if err != nil {
			return Scope{}, err
		}
		if strings.TrimSpace(string(out)) != sha {
			return Scope{}, fmt.Errorf("proofscope: %s does not name a commit", sha)
		}
	}
	base, _, err := lsTree(ctx, repoDir, baseSHA)
	if err != nil {
		return Scope{}, err
	}
	head, blobs, err := lsTree(ctx, repoDir, headSHA)
	if err != nil {
		return Scope{}, err
	}
	diff, err := gitOutput(ctx, repoDir, nil, "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", baseSHA, headSHA, "--")
	if err != nil {
		return Scope{}, err
	}
	changed, err := splitZ(diff)
	if err != nil {
		return Scope{}, err
	}
	head.Source, err = readBlobs(ctx, repoDir, blobs)
	if err != nil {
		return Scope{}, err
	}
	scope := Derive(base, head, changed)
	scope.Base, scope.Head = baseSHA, headSHA
	return scope, nil
}

type blobRef struct {
	path, oid string
	size      int64
}

// lsTree lists every tracked path of a commit, plus the head blobs Derive reads.
func lsTree(ctx context.Context, repoDir, sha string) (Snapshot, []blobRef, error) {
	out, err := gitOutput(ctx, repoDir, nil, "ls-tree", "-r", "-z", "-l", "--full-tree", sha)
	if err != nil {
		return Snapshot{}, nil, err
	}
	entries, err := splitZ(out)
	if err != nil {
		return Snapshot{}, nil, err
	}
	var snap Snapshot
	var blobs []blobRef
	var total int64
	for _, entry := range entries {
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 || name == "" {
			return Snapshot{}, nil, fmt.Errorf("proofscope: malformed ls-tree entry %q", entry)
		}
		snap.Paths = append(snap.Paths, name)
		if fields[1] != "blob" || (name != "go.mod" && !strings.HasSuffix(name, ".go")) {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return Snapshot{}, nil, fmt.Errorf("proofscope: malformed blob size in %q", entry)
		}
		if total += size; total > MaxGoSourceBytes {
			return Snapshot{}, nil, fmt.Errorf("proofscope: head Go source exceeds %d bytes", MaxGoSourceBytes)
		}
		blobs = append(blobs, blobRef{path: name, oid: fields[2], size: size})
	}
	return snap, blobs, nil
}

func readBlobs(ctx context.Context, repoDir string, blobs []blobRef) (map[string][]byte, error) {
	source := make(map[string][]byte, len(blobs))
	if len(blobs) == 0 {
		return source, nil
	}
	var stdin bytes.Buffer
	for _, blob := range blobs {
		stdin.WriteString(blob.oid + "\n")
	}
	out, err := gitOutput(ctx, repoDir, &stdin, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for _, blob := range blobs {
		header, rest, ok := bytes.Cut(out, []byte("\n"))
		want := fmt.Sprintf("%s blob %d", blob.oid, blob.size)
		if !ok || string(header) != want || int64(len(rest)) < blob.size+1 || rest[blob.size] != '\n' {
			return nil, fmt.Errorf("proofscope: unexpected cat-file record for %s", blob.path)
		}
		source[blob.path] = rest[:blob.size:blob.size]
		out = rest[blob.size+1:]
	}
	if len(out) != 0 {
		return nil, errors.New("proofscope: trailing cat-file output")
	}
	return source, nil
}

func splitZ(out []byte) ([]string, error) {
	if len(out) == 0 {
		return nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, errors.New("proofscope: malformed NUL-terminated git output")
	}
	return strings.Split(string(out[:len(out)-1]), "\x00"), nil
}

// gitOutput inherits the caller's process group like the testbattery gates:
// no child killer, only a context check before and after.
func gitOutput(ctx context.Context, repoDir string, stdin *bytes.Buffer, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("proofscope: git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), ctx.Err()
}
