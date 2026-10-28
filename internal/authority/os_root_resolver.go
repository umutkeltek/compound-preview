package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/mem"
	"golang.org/x/sys/unix"
)

// AbsorbedOSRegistryKey is the os-path-authority.yaml canonical_roots key an
// operator uses to declare where their absorbed operating-system control
// plane lives on disk. This is the single-owner seam: the operator declares
// WHERE the absorbed OS lives (root path = data, via
// `compound authority path-registry`); compound owns the internal layout
// below that root (the AbsorbedOS* sub-path constants below). No absorbed-OS
// path is ever hardcoded into product source.
const (
	AbsorbedOSRegistryKey   = "absorbed_os"
	InstalledHarnessHomeKey = "installed_harness_home"

	InstalledHarnessEntrypointRevisionPrefix = "installed-entrypoint://"
)

// absorbedOSEntityRef mirrors pathAuthorityFromOSRoot's entity_ref minting
// ("source:" + key with underscores replaced by hyphens) for the
// AbsorbedOSRegistryKey key.
var (
	absorbedOSEntityRef     = "source:" + strings.ReplaceAll(AbsorbedOSRegistryKey, "_", "-")
	installedHarnessHomeRef = "source:" + strings.ReplaceAll(InstalledHarnessHomeKey, "_", "-")
)

// Compound-owned sub-paths beneath an absorbed OS root. These are CODE (the
// internal layout compound expects), not operator data — only the root
// itself is operator-declared.
const (
	AbsorbedOSExecLaneEntrypoint = "exec-lane/os.ts"
	AbsorbedOSSteeringConfig     = "spine/steering.config.ts"
	AbsorbedOSSkillProposals     = "state/skill-surfacing-proposals.json"
	AbsorbedOSEventsDir          = "events"
)

// ResolveAbsorbedOSRoot resolves the operator-declared absorbed-OS root from
// the promoted current_path claim on the absorbed_os path-authority entity.
// It returns found=false (never an error) whenever there is nothing to
// resolve: no COMPOUND_HOME ledger yet, or no current_path claim for the
// absorbed_os key. That is the public-clone skip case, not a fault — a fresh
// clone with no operator os-path-authority.yaml imported must resolve
// found=false, not fail.
func ResolveAbsorbedOSRoot(compoundHome string) (string, bool, error) {
	return resolveDeclaredOSPath(compoundHome, absorbedOSEntityRef)
}

// ResolveInstalledHarnessHome resolves the live harness installation home
// from Compound's promoted path-authority claim. Unlike os.UserHomeDir, this
// binding cannot be redirected by a process-local HOME override.
func ResolveInstalledHarnessHome(compoundHome string) (string, bool, error) {
	return resolveDeclaredOSPath(compoundHome, installedHarnessHomeRef)
}

// InstalledHarnessEntrypointBinding returns the artifact revision that
// contract.MaterializeRuntime binds into both the ContextPack and
// EffectiveContract. A public clone without an installed-harness marker has
// no binding. Once the marker exists, supported harness entrypoints fail
// closed when their exact installed file is missing, unreadable, or not a
// regular file.
func InstalledHarnessEntrypointBinding(compoundHome, harness string) (string, string, bool, error) {
	return installedHarnessEntrypointBindingWithOps(compoundHome, harness, installedEntrypointReadOps{})
}

type installedEntrypointSnapshot struct {
	path   string
	body   []byte
	digest string
	info   os.FileInfo
}

type installedEntrypointReadOps struct {
	beforeRootOpen func(string)
	beforeOpen     func(string)
}

func installedHarnessEntrypointBindingWithOps(
	compoundHome, harness string,
	ops installedEntrypointReadOps,
) (string, string, bool, error) {
	snapshot, found, err := readInstalledHarnessEntrypoint(compoundHome, harness, ops)
	if err != nil || !found {
		return "", "", found, err
	}
	family, _, _ := installedHarnessFamily(harness)
	return InstalledHarnessEntrypointRevisionPrefix + family, "sha256:" + snapshot.digest, true, nil
}

func installedHarnessFamily(harness string) (string, string, bool) {
	harness = strings.TrimSpace(strings.ToLower(harness))
	switch {
	case harness == "codex" || strings.HasPrefix(harness, "codex-") || strings.HasPrefix(harness, "codex_"):
		return "codex", osAbsorptionServedCodexEntrypoint, true
	case harness == "claude" || strings.HasPrefix(harness, "claude-") || strings.HasPrefix(harness, "claude_"):
		return "claude", osAbsorptionServedClaudeEntrypoint, true
	default:
		return "", "", false
	}
}

// readInstalledHarnessEntrypoint returns one descriptor-backed snapshot of
// the installed entrypoint. Every path component is opened relative to its
// already-open parent with O_NOFOLLOW, and the body, digest, and metadata all
// come from the final descriptor. Callers must not reopen snapshot.path.
func readInstalledHarnessEntrypoint(
	compoundHome, harness string,
	ops installedEntrypointReadOps,
) (installedEntrypointSnapshot, bool, error) {
	family, relativePath, supported := installedHarnessFamily(harness)
	if !supported {
		return installedEntrypointSnapshot{}, false, nil
	}

	installedHome, found, err := ResolveInstalledHarnessHome(compoundHome)
	if err != nil || !found {
		return installedEntrypointSnapshot{}, found, err
	}

	relativePath = filepath.Clean(filepath.FromSlash(relativePath))
	if relativePath == "." || filepath.IsAbs(relativePath) {
		return installedEntrypointSnapshot{}, false, fmt.Errorf(
			"installed %s entrypoint path must be a non-empty relative path: %q",
			family,
			relativePath,
		)
	}
	components := strings.Split(relativePath, string(os.PathSeparator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return installedEntrypointSnapshot{}, false, fmt.Errorf(
				"installed %s entrypoint path has unsafe component %q",
				family,
				component,
			)
		}
	}

	entrypointPath := filepath.Join(installedHome, relativePath)
	if ops.beforeRootOpen != nil {
		ops.beforeRootOpen(installedHome)
	}
	rootFD, err := unix.Open(
		string(os.PathSeparator),
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY,
		0,
	)
	if err != nil {
		return installedEntrypointSnapshot{}, false, fmt.Errorf(
			"open filesystem root for installed %s harness home %s: %w",
			family,
			installedHome,
			err,
		)
	}
	openedDirectories := []int{rootFD}
	defer func() {
		for i := len(openedDirectories) - 1; i >= 0; i-- {
			_ = unix.Close(openedDirectories[i])
		}
	}()

	dirFD := rootFD
	walkedPath := string(os.PathSeparator)
	homeRelativePath := strings.TrimPrefix(installedHome, string(os.PathSeparator))
	if homeRelativePath != "" {
		for _, component := range strings.Split(homeRelativePath, string(os.PathSeparator)) {
			if component == "" || component == "." || component == ".." {
				return installedEntrypointSnapshot{}, false, fmt.Errorf(
					"installed %s harness home has unsafe component %q",
					family,
					component,
				)
			}
			nextFD, openErr := unix.Openat(
				dirFD,
				component,
				unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY,
				0,
			)
			walkedPath = filepath.Join(walkedPath, component)
			if openErr != nil {
				return installedEntrypointSnapshot{}, false, fmt.Errorf(
					"open installed %s harness home without symlink traversal %s: %w",
					family,
					walkedPath,
					openErr,
				)
			}
			openedDirectories = append(openedDirectories, nextFD)
			dirFD = nextFD
		}
	}

	for index, component := range components[:len(components)-1] {
		nextFD, openErr := unix.Openat(
			dirFD,
			component,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY,
			0,
		)
		if openErr != nil {
			return installedEntrypointSnapshot{}, false, fmt.Errorf(
				"open installed %s entrypoint directory without symlink traversal %s: %w",
				family,
				filepath.Join(installedHome, filepath.Join(components[:index+1]...)),
				openErr,
			)
		}
		openedDirectories = append(openedDirectories, nextFD)
		dirFD = nextFD
	}

	if ops.beforeOpen != nil {
		ops.beforeOpen(entrypointPath)
	}
	entrypointFD, err := unix.Openat(
		dirFD,
		components[len(components)-1],
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return installedEntrypointSnapshot{}, false, fmt.Errorf(
			"open installed %s entrypoint without symlink traversal %s: %w",
			family,
			entrypointPath,
			err,
		)
	}
	entrypointFile := os.NewFile(uintptr(entrypointFD), entrypointPath)
	if entrypointFile == nil {
		_ = unix.Close(entrypointFD)
		return installedEntrypointSnapshot{}, false, fmt.Errorf(
			"open installed %s entrypoint descriptor %s",
			family,
			entrypointPath,
		)
	}
	defer entrypointFile.Close()

	info, err := entrypointFile.Stat()
	if err != nil {
		return installedEntrypointSnapshot{}, false, fmt.Errorf(
			"inspect installed %s entrypoint descriptor %s: %w",
			family,
			entrypointPath,
			err,
		)
	}
	if !info.Mode().IsRegular() {
		return installedEntrypointSnapshot{}, false, fmt.Errorf(
			"installed %s entrypoint is not a regular file: %s",
			family,
			entrypointPath,
		)
	}
	body, err := io.ReadAll(entrypointFile)
	if err != nil {
		return installedEntrypointSnapshot{}, false, fmt.Errorf(
			"read installed %s entrypoint descriptor %s: %w",
			family,
			entrypointPath,
			err,
		)
	}
	digest := sha256.Sum256(body)
	return installedEntrypointSnapshot{
		path:   entrypointPath,
		body:   body,
		digest: hex.EncodeToString(digest[:]),
		info:   info,
	}, true, nil
}

func resolveDeclaredOSPath(compoundHome, entityRef string) (string, bool, error) {
	store, err := mem.OpenReadOnly(compoundHome)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			ledgerPath := filepath.Join(compoundHome, "ledger")
			if _, lstatErr := os.Lstat(ledgerPath); errors.Is(lstatErr, os.ErrNotExist) {
				// No ledger yet is the public-clone case: nothing declared.
				// Lstat deliberately distinguishes absence from a dangling
				// ledger symlink, which is malformed state and must fail closed.
				return "", false, nil
			}
		}
		return "", false, fmt.Errorf("resolve declared path authority %s: %w", entityRef, err)
	}
	claim, ok, err := store.CurrentClaim(entityRef, "current_path")
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	root := strings.TrimSpace(claim.Object)
	if root == "" {
		return "", false, nil
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return "", false, fmt.Errorf("declared path authority %s current_path must be absolute, got %q", entityRef, root)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			if entityRef == installedHarnessHomeRef {
				return "", false, fmt.Errorf("declared installed harness home must be an existing directory: %s", root)
			}
			return root, true, nil
		}
		return "", false, fmt.Errorf("canonicalize declared path authority %s current_path %s: %w", entityRef, root, err)
	}
	canonicalRoot = filepath.Clean(canonicalRoot)
	if canonicalRoot != root {
		return "", false, fmt.Errorf(
			"declared path authority %s current_path is no longer canonical: claim=%s resolved=%s",
			entityRef,
			root,
			canonicalRoot,
		)
	}
	info, err := os.Stat(canonicalRoot)
	if err != nil {
		return "", false, fmt.Errorf("inspect declared path authority %s current_path %s: %w", entityRef, canonicalRoot, err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("declared path authority %s current_path is not a directory: %s", entityRef, canonicalRoot)
	}
	return canonicalRoot, true, nil
}

// ResolveAbsorbedOSEntrypoint resolves subPath (one of the compound-owned
// AbsorbedOS* sub-path constants, forward-slash form) beneath the
// operator-declared absorbed-OS root. It returns found=false whenever the
// root is not declared OR the declared root is absent on disk (a
// stale/moved registry entry skips exactly like an absent registry — a
// declared-but-missing path is never treated as a hard failure).
func ResolveAbsorbedOSEntrypoint(compoundHome, subPath string) (string, bool, error) {
	root, ok, err := ResolveAbsorbedOSRoot(compoundHome)
	if err != nil {
		return "", false, err
	}
	if !ok || !absorbedOSRootPresentOnDisk(root) {
		return "", false, nil
	}
	return filepath.Join(root, filepath.FromSlash(subPath)), true, nil
}

// absorbedOSRootPresentOnDisk reports whether a declared absorbed-OS root
// actually exists as a directory. A declared-but-absent root (stale or moved
// registry entry) is treated identically to an absent declaration.
func absorbedOSRootPresentOnDisk(root string) bool {
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

// osAbsorptionAbsorbedOSSkipReason is the standard proof_detail every
// OS-absorption wrapper proof reports when it declines to run because no
// absorbed-OS root is configured on this machine (the public-clone case).
const osAbsorptionAbsorbedOSSkipReason = "no absorbed-OS root declared (os-path-authority key '" +
	AbsorbedOSRegistryKey + "' absent or path missing) — OS-absorption proofs skipped"

// resolveAbsorbedOSRootForProof is the shared entry check every OS-absorption
// wrapper proof (os_capability, os_skills, os_status) calls first: it
// resolves the declared root and confirms it is present on disk, folding
// "not declared" and "declared but absent" into the same skipped outcome
// (returned via skip, never via err) so a public clone with no absorbed-OS
// root imported skips these proofs instead of failing them. err is reserved
// for a genuine resolver fault (e.g. an unreadable ledger).
func resolveAbsorbedOSRootForProof(compoundHome string) (root string, skip *osAbsorptionProofResult, err error) {
	root, found, err := ResolveAbsorbedOSRoot(compoundHome)
	if err != nil {
		return "", nil, err
	}
	if !found || !absorbedOSRootPresentOnDisk(root) {
		return "", &osAbsorptionProofResult{skipped: true, reason: osAbsorptionAbsorbedOSSkipReason}, nil
	}
	return root, nil, nil
}
