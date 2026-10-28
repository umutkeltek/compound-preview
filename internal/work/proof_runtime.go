package work

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProofRuntimeResolver maps a logical proof command to the executable that
// belongs to the certifying runtime. Tests may inject a resolver, while the
// production resolver keeps logical Compound children on the current binary.
type ProofRuntimeResolver func(string) (string, error)

type proofRuntimeExecutableContextKey struct{}

// ResolveProofRuntimeCommand is the default resolver for proof subprocesses.
func ResolveProofRuntimeCommand(logical string) (string, error) {
	if logical == "compound" {
		return os.Executable()
	}
	return logical, nil
}

// PinProofRuntimeExecutable copies a temporary self executable into private,
// synced custody before any proof gate can outlive the source build-cache path.
func PinProofRuntimeExecutable(ctx context.Context, resolve ProofRuntimeResolver) (context.Context, func(), error) {
	if ctx.Err() != nil {
		return ctx, func() {}, context.Cause(ctx)
	}
	if resolve == nil {
		return ctx, func() {}, fmt.Errorf("proof runtime resolver is required")
	}
	executable, err := resolve("compound")
	if err != nil {
		return ctx, func() {}, err
	}
	executable = filepath.Clean(strings.TrimSpace(executable))
	if executable == "." || executable == "" {
		return ctx, func() {}, fmt.Errorf("compound runtime executable is empty")
	}
	if ctx.Err() != nil {
		return ctx, func() {}, context.Cause(ctx)
	}
	if !proofRuntimeExecutableIsTemporary(executable) {
		return context.WithValue(ctx, proofRuntimeExecutableContextKey{}, executable), func() {}, nil
	}

	source, err := os.Open(executable)
	if err != nil {
		return ctx, func() {}, fmt.Errorf("open temporary compound runtime executable: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return ctx, func() {}, fmt.Errorf("stat temporary compound runtime executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ctx, func() {}, fmt.Errorf("temporary compound runtime executable is not a regular file: %s", executable)
	}

	dir, err := os.MkdirTemp("", "compound-proofcheck-bin-*")
	if err != nil {
		return ctx, func() {}, fmt.Errorf("create stable compound runtime directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	target := filepath.Join(dir, "compound")
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		cleanup()
		return ctx, func() {}, fmt.Errorf("create stable compound runtime executable: %w", err)
	}
	if _, err := copyProofContext(ctx, destination, source); err != nil {
		_ = destination.Close()
		cleanup()
		return ctx, func() {}, fmt.Errorf("copy stable compound runtime executable: %w", err)
	}
	if err := destination.Sync(); err != nil {
		_ = destination.Close()
		cleanup()
		return ctx, func() {}, fmt.Errorf("sync stable compound runtime executable: %w", err)
	}
	if err := destination.Close(); err != nil {
		cleanup()
		return ctx, func() {}, fmt.Errorf("close stable compound runtime executable: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		cleanup()
		return ctx, func() {}, fmt.Errorf("open stable compound runtime directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		cleanup()
		return ctx, func() {}, fmt.Errorf("sync stable compound runtime directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		cleanup()
		return ctx, func() {}, fmt.Errorf("close stable compound runtime directory: %w", err)
	}
	if ctx.Err() != nil {
		cleanup()
		return ctx, func() {}, context.Cause(ctx)
	}
	return context.WithValue(ctx, proofRuntimeExecutableContextKey{}, target), cleanup, nil
}

// ProofRuntimeExecutable resolves a logical command against the pinned runtime
// when one is present, and otherwise delegates to the supplied resolver.
func ProofRuntimeExecutable(ctx context.Context, logical string, resolve ProofRuntimeResolver) (string, error) {
	if logical == "compound" && ctx != nil {
		if executable, ok := ctx.Value(proofRuntimeExecutableContextKey{}).(string); ok && strings.TrimSpace(executable) != "" {
			return executable, nil
		}
	}
	if resolve == nil {
		return "", fmt.Errorf("proof runtime resolver is required")
	}
	return resolve(logical)
}

func proofRuntimeExecutableIsTemporary(path string) bool {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "." || cleaned == "" {
		return false
	}
	for _, root := range []string{filepath.Clean(os.TempDir()), string(filepath.Separator) + "tmp"} {
		if cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
			return true
		}
	}
	return strings.Contains(cleaned, string(filepath.Separator)+"go-build")
}

// ProofGoCache gives each worktree a stable Go cache outside the repository.
// Sibling proof lanes then cannot evict one another's live cache entries or
// leak build-cache bytes into the public-redact surface. Each call stamps the
// cache's owner-path and last-used markers so PruneProofGoCaches can bound
// the caches; a retired worktree's cache becomes an orphan the next run removes.
func ProofGoCache(repo string) (string, bool) {
	dir, owner, ok := proofGoCacheDir(repo)
	if !ok {
		return "", false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	markProofGoCacheUse(dir, owner, time.Now())
	return dir, true
}

// proofGoCacheDir names repo's per-worktree cache without creating it.
func proofGoCacheDir(repo string) (dir string, owner string, ok bool) {
	if strings.TrimSpace(os.Getenv("COMPOUND_PROOF_SHARED_GOCACHE")) != "" || strings.TrimSpace(repo) == "" {
		return "", "", false
	}
	resolved, err := filepath.Abs(repo)
	if err != nil {
		return "", "", false
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", "", false
	}
	sum := sha256.Sum256([]byte(resolved))
	return filepath.Join(base, "compound-proof-gocache", hex.EncodeToString(sum[:8])), resolved, true
}
