package gitrepository

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Snapshot seals the immutable Git source identity certified by a proof
// battery. BaseSHA is resolved once from the caller's ref; every later
// operation uses the sealed object ID rather than resolving that ref again.
type Snapshot struct {
	RepositoryFingerprint string `json:"repository_fingerprint"`
	BaseSHA               string `json:"base_sha"`
	HeadSHA               string `json:"head_sha"`
	TreeSHA               string `json:"tree_sha"`
	Clean                 bool   `json:"clean"`
}

// CaptureCleanSnapshot resolves baseRef once and refuses a candidate whose
// source is not entirely represented by its committed HEAD tree.
func CaptureCleanSnapshot(repo, baseRef string) (Snapshot, error) {
	return CaptureCleanSnapshotContext(context.Background(), repo, baseRef)
}

// CaptureCleanSnapshotContext binds all snapshot reads to the caller's cancellation.
func CaptureCleanSnapshotContext(ctx context.Context, repo, baseRef string) (Snapshot, error) {
	snapshot, err := readSnapshotContext(ctx, repo, baseRef)
	if err != nil {
		return Snapshot{}, err
	}
	if !snapshot.Clean {
		return Snapshot{}, fmt.Errorf("certifying proof requires a clean committed Git worktree")
	}
	return snapshot, nil
}

// RevalidateSnapshot proves the repository still presents the exact sealed
// source identity. It intentionally resolves the immutable BaseSHA, never the
// moving symbolic ref originally supplied by the caller.
func RevalidateSnapshot(repo string, expected Snapshot) error {
	return RevalidateSnapshotContext(context.Background(), repo, expected)
}

// RevalidateSnapshotContext retains the success fence without escaping the caller's deadline.
func RevalidateSnapshotContext(ctx context.Context, repo string, expected Snapshot) error {
	if err := ValidateSnapshotShape(expected); err != nil {
		return fmt.Errorf("sealed source snapshot: %w", err)
	}
	if !expected.Clean {
		return fmt.Errorf("sealed source snapshot is not clean")
	}
	current, err := readSnapshotContext(ctx, repo, expected.BaseSHA)
	if err != nil {
		return err
	}
	switch {
	case !current.Clean:
		return fmt.Errorf("source snapshot changed: worktree is dirty")
	case current.RepositoryFingerprint != expected.RepositoryFingerprint:
		return fmt.Errorf("source snapshot changed: repository fingerprint")
	case current.BaseSHA != expected.BaseSHA:
		return fmt.Errorf("source snapshot changed: base SHA")
	case current.HeadSHA != expected.HeadSHA:
		return fmt.Errorf("source snapshot changed: HEAD SHA")
	case current.TreeSHA != expected.TreeSHA:
		return fmt.Errorf("source snapshot changed: tree SHA")
	default:
		return nil
	}
}

func readSnapshot(repo, baseRef string) (Snapshot, error) {
	return readSnapshotContext(context.Background(), repo, baseRef)
}
func readSnapshotContext(parent context.Context, repo, baseRef string) (Snapshot, error) {
	repo = strings.TrimSpace(repo)
	baseRef = strings.TrimSpace(baseRef)
	if repo == "" {
		return Snapshot{}, fmt.Errorf("repository path required")
	}
	if baseRef == "" {
		return Snapshot{}, fmt.Errorf("repository base ref required")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	// Read these related object names together, then validate every field:
	// --revs-only may omit an invalid expression without returning an error.
	objects, err := gitOutput(ctx, repo, "rev-parse", "--show-object-format", "--revs-only", "--end-of-options", baseRef+"^{commit}", "HEAD^{commit}", "HEAD^{tree}")
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve Git source objects: %w", err)
	}
	fields := strings.Split(strings.TrimSuffix(objects, "\n"), "\n")
	if len(fields) != 4 {
		return Snapshot{}, fmt.Errorf("Git source object read returned %d fields, want object format and three full object IDs", len(fields))
	}
	objectFormat, baseSHA, headSHA, treeSHA := fields[0], fields[1], fields[2], fields[3]
	objectIDLength := 40
	switch objectFormat {
	case "sha1":
	case "sha256":
		objectIDLength = 64
	default:
		return Snapshot{}, fmt.Errorf("unsupported Git object format %q", objectFormat)
	}
	for _, id := range []string{baseSHA, headSHA, treeSHA} {
		if len(id) != objectIDLength || !validObjectID(id) {
			return Snapshot{}, fmt.Errorf("Git source identity did not resolve to full %s object IDs", objectFormat)
		}
	}
	identity, err := readIdentityWithObjectFormat(ctx, repo, baseSHA, objectFormat)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read repository identity: %w", err)
	}
	if err := verifyTrackedSourceClean(ctx, repo); err != nil {
		return Snapshot{}, err
	}
	status, err := gitOutput(
		ctx,
		repo,
		"status",
		"--porcelain=v1",
		"-z",
		"--untracked-files=normal",
		"--ignore-submodules=none",
	)
	if err != nil {
		return Snapshot{}, fmt.Errorf("inspect Git worktree status: %w", err)
	}
	snapshot := Snapshot{
		RepositoryFingerprint: strings.TrimSpace(identity.Fingerprint),
		BaseSHA:               baseSHA,
		HeadSHA:               headSHA,
		TreeSHA:               treeSHA,
		Clean:                 status == "",
	}
	if err := ValidateSnapshotShape(snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// verifyTrackedSourceClean refuses index flags and submodule configuration that
// can hide tracked bytes from ordinary status output.
func verifyTrackedSourceClean(ctx context.Context, repo string) error {
	tagged, err := gitOutput(ctx, repo, "ls-files", "-v", "-z")
	if err != nil {
		return fmt.Errorf("inspect tracked Git index flags: %w", err)
	}
	for _, entry := range strings.Split(tagged, "\x00") {
		if len(entry) < 3 || entry[1] != ' ' {
			continue
		}
		tag := entry[0]
		path := entry[2:]
		switch {
		case tag == 'S':
			return fmt.Errorf("certifying proof rejects skip-worktree tracked path %q", path)
		case tag >= 'a' && tag <= 'z':
			return fmt.Errorf("certifying proof rejects assume-unchanged tracked path %q", path)
		}
	}
	if _, err := gitOutput(
		ctx,
		repo,
		"diff-index",
		"--quiet",
		"--cached",
		"--ignore-submodules=none",
		"HEAD",
		"--",
	); err != nil {
		return fmt.Errorf("tracked Git index differs from HEAD: %w", err)
	}
	if _, err := gitOutput(
		ctx,
		repo,
		"diff-files",
		"--quiet",
		"--ignore-submodules=none",
		"--",
	); err != nil {
		return fmt.Errorf("tracked Git worktree or submodule differs from index: %w", err)
	}
	return nil
}

// ValidateSnapshotShape validates the sealed identity without reading a
// repository. Persistence owners use it before trusting decoded evidence.
func ValidateSnapshotShape(snapshot Snapshot) error {
	if len(strings.TrimSpace(snapshot.RepositoryFingerprint)) != 64 {
		return fmt.Errorf("repository fingerprint must be SHA-256")
	}
	if !validObjectID(strings.TrimSpace(snapshot.BaseSHA)) {
		return fmt.Errorf("base SHA must be a full Git object ID")
	}
	if !validObjectID(strings.TrimSpace(snapshot.HeadSHA)) {
		return fmt.Errorf("HEAD SHA must be a full Git object ID")
	}
	if !validObjectID(strings.TrimSpace(snapshot.TreeSHA)) {
		return fmt.Errorf("tree SHA must be a full Git object ID")
	}
	return nil
}
