package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type sourcePackageInventory struct {
	All, Build []string
}

var sourceCommit = regexp.MustCompile("^(?:[0-9a-f]{40}|[0-9a-f]{64})$")

// The caller's existing sealed HEAD is the authority, not a new manifest. A
// test-created source package or edited index cannot change later gate inputs
// silently: each gate must start and finish against that same clean commit.
func runBoundSourceGate(ctx context.Context, mode, sourceHead string, stdout, stderr io.Writer) error {
	if mode == credentialAlignmentProofMode && sourceHead == "" {
		return errors.New("native credential-alignment proof requires an exact sealed source head")
	}
	if sourceHead == "" {
		return runSourceGate(ctx, mode, discoverSourcePackages, runGo, stdout, stderr)
	}
	if !sourceCommit.MatchString(sourceHead) {
		return errors.New("source snapshot requires a full commit SHA")
	}
	if err := verifySourceSnapshot(ctx, sourceHead, stderr); err != nil {
		return err
	}
	runErr := runSourceGate(ctx, mode, discoverSourcePackages, runGo, stdout, stderr)
	return errors.Join(runErr, verifySourceSnapshot(ctx, sourceHead, stderr))
}

func verifySourceSnapshot(ctx context.Context, sourceHead string, stderr io.Writer) error {
	head, err := runSourceGit(ctx, stderr, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if string(head) != sourceHead+"\n" {
		return errors.New("source snapshot drift: HEAD no longer matches the sealed commit")
	}
	status, err := runSourceGit(ctx, stderr, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("source snapshot drift: tracked, staged or nonignored untracked files changed")
	}
	return nil
}

func runSourceGit(ctx context.Context, stderr io.Writer, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Like runGo: inherit the owning gate's group, never add a child killer.
	cmd := exec.Command("git", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("source inventory git %s: %w", args[0], err)
	}
	return out.Bytes(), ctx.Err()
}

// discoverSourcePackages resolves Git-owned directories through Go. The Build
// projection excludes test-only packages just as go build ./... does; All keeps
// them for both tests and vet. Every projection is derived from this one query.
func discoverSourcePackages(ctx context.Context, stderr io.Writer) (result sourcePackageInventory, err error) {
	dirs, err := sourceDirectories(ctx, stderr)
	if err != nil {
		return result, err
	}
	var listed bytes.Buffer
	if err := runGo(ctx, append([]string{"list", "-e", "-json"}, dirs...), &listed, stderr); err != nil {
		return result, fmt.Errorf("resolve source packages: %w", err)
	}
	decoder := json.NewDecoder(&listed)
	seenDirs := make(map[string]bool)
	wanted := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return result, err
		}
		wanted[abs] = true
	}
	var packages, buildPackages []string
	for {
		var pkg struct {
			Dir, ImportPath                                              string
			GoFiles, CgoFiles, TestGoFiles, XTestGoFiles, IgnoredGoFiles []string
			Error                                                        *struct{ Err string }
		}
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return result, fmt.Errorf("decode source package: %w", err)
		}
		if !wanted[pkg.Dir] || seenDirs[pkg.Dir] {
			return result, fmt.Errorf("unexpected or duplicate source package directory %q", pkg.Dir)
		}
		seenDirs[pkg.Dir] = true
		if pkg.Error != nil {
			// Match Go's normal ./... platform/tag exclusion, not arbitrary
			// errors. Broken syntax, imports and missing files remain failures.
			if pkg.Error.Err == "build constraints exclude all Go files in "+pkg.Dir &&
				len(pkg.IgnoredGoFiles) > 0 &&
				len(pkg.GoFiles)+len(pkg.CgoFiles)+len(pkg.TestGoFiles)+len(pkg.XTestGoFiles) == 0 {
				continue
			}
			return result, fmt.Errorf("source package %s: %s", pkg.ImportPath, pkg.Error.Err)
		}
		packages = append(packages, pkg.ImportPath)
		if len(pkg.GoFiles)+len(pkg.CgoFiles) > 0 {
			buildPackages = append(buildPackages, pkg.ImportPath)
		}
	}
	if len(seenDirs) != len(wanted) {
		return result, errors.New("Go did not resolve every source directory")
	}
	sort.Strings(packages)
	inventory := strings.Join(packages, "\n") + "\n"
	if _, err := partitionPackages(inventory); err != nil {
		return result, err
	}
	sort.Strings(buildPackages)
	return sourcePackageInventory{All: packages, Build: buildPackages}, nil
}

func sourceDirectories(ctx context.Context, stderr io.Writer) ([]string, error) {
	runGit := func(args ...string) ([]byte, error) {
		return runSourceGit(ctx, stderr, args...)
	}
	prefix, err := runGit("rev-parse", "--show-prefix")
	if err != nil {
		return nil, err
	}
	if string(prefix) != "\n" {
		return nil, errors.New("source inventory must run at the Git repository root")
	}
	files, err := runGit("ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	if len(files) == 0 || files[len(files)-1] != 0 {
		return nil, errors.New("empty or malformed Git source inventory")
	}
	dirs := make(map[string]bool)
	for _, name := range strings.Split(string(files[:len(files)-1]), "\x00") {
		if name == "" || path.Clean(name) != name || path.IsAbs(name) ||
			name == ".." || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("invalid Git source path %q", name)
		}
		parts := strings.Split(name, "/")
		excluded := false
		for _, part := range parts[:len(parts)-1] {
			if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") ||
				part == "testdata" || part == "vendor" {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		base := path.Base(name)
		if base == "go.mod" && name != "go.mod" {
			return nil, fmt.Errorf("unsupported nested source module %q; requires its own proof", name)
		}
		if !strings.HasSuffix(base, ".go") || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
			continue
		}
		info, err := os.Lstat(filepath.FromSlash(name))
		if err != nil {
			return nil, fmt.Errorf("source file %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("source file %q is not a regular file", name)
		}
		dir := path.Dir(name)
		if !dirs[dir] {
			for parent := dir; parent != "."; parent = path.Dir(parent) {
				info, err := os.Lstat(filepath.FromSlash(parent))
				if err != nil {
					return nil, fmt.Errorf("source directory %q: %w", parent, err)
				}
				if !info.IsDir() {
					return nil, fmt.Errorf("source directory %q is not a real directory", parent)
				}
			}
			dirs[dir] = true
		}
	}
	if len(dirs) == 0 {
		return nil, errors.New("Git inventory contains no Go source packages")
	}
	result := make([]string, 0, len(dirs))
	for dir := range dirs {
		if dir == "." {
			result = append(result, ".")
		} else {
			result = append(result, "./"+dir)
		}
	}
	sort.Strings(result)
	return result, nil
}

func runSourceGate(ctx context.Context, mode string, discover func(context.Context, io.Writer) (sourcePackageInventory, error), run commandRunner, stdout, stderr io.Writer) error {
	if mode == credentialAlignmentProofMode {
		return runCredentialAlignmentProof(ctx, credentialAlignmentCommand, stdout, stderr)
	}
	if mode != "test" && mode != "general-release" && mode != "home" && mode != "vet" && mode != "build" {
		return fmt.Errorf("unsupported source gate %q", mode)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	inventory, err := discover(ctx, stderr)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mode == "test" || mode == "general-release" || mode == "home" {
		// Adapt the resolved inventory to the existing phase driver; its launch,
		// aggregation and cancellation behavior stays unchanged.
		return runBatteryPhase(ctx, mode, func(ctx context.Context, args []string, out, errOut io.Writer) error {
			if args[0] == "list" {
				_, err := io.WriteString(out, strings.Join(inventory.All, "\n")+"\n")
				return err
			}
			return run(ctx, args, out, errOut)
		}, stdout, stderr)
	}
	packages := inventory.All
	if mode == "build" {
		packages = inventory.Build
	}
	if len(packages) == 0 {
		return errors.New("source gate has no packages")
	}
	return run(ctx, append([]string{mode}, packages...), stdout, stderr)
}
