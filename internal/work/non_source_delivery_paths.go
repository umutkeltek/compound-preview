package work

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
)

// These checks describe private-overlay bytes, never source delivery. A caller
// must additionally establish exact action ownership, operator binding, custody,
// rollback and consumed outcome evidence before using them at the close gate.
type privateRuntimeDeliveryFile struct {
	Path        string `json:"path"`
	AfterSHA256 string `json:"after_sha256"`
}

const maxPrivateRuntimeDeliveryFiles = 256
const maxPrivateRuntimeDeliveryFileBytes = 64 << 20

func validatePrivateRuntimeDeliveryPaths(repo, target string, files []privateRuntimeDeliveryFile) error {
	return visitPrivateRuntimeDeliveryPaths(repo, target, files, privateDeliveryDigest)
}

func visitPrivateRuntimeDeliveryPaths(repo, target string, files []privateRuntimeDeliveryFile, consume func(*os.Root, string) (string, error)) error {
	if len(files) == 0 || len(files) > maxPrivateRuntimeDeliveryFiles {
		return fmt.Errorf("private runtime delivery file count must be 1..%d", maxPrivateRuntimeDeliveryFiles)
	}
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return fmt.Errorf("private runtime repository must be canonical and absolute")
	}
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return err
	}
	if canonical != repo {
		return fmt.Errorf("private runtime repository must not contain a symlink")
	}
	actual, found, err := canonicalGitRootForPath(repo)
	if err != nil || !found || actual != repo {
		return fmt.Errorf("private runtime repository must be an exact Git worktree root")
	}
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return fmt.Errorf("private runtime target must be canonical and absolute")
	}
	targetRel, err := filepath.Rel(repo, target)
	if err != nil || !privateOverlayPath(filepath.ToSlash(targetRel)) {
		return fmt.Errorf("private runtime target must be inside a named private overlay")
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := privateDeliveryPathInfos(root, targetRel); err != nil {
		return err
	}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if filepath.IsAbs(file.Path) {
			return fmt.Errorf("private runtime file must be repository-relative")
		}
		if file.Path == "" || strings.TrimSpace(file.Path) != file.Path || filepath.ToSlash(filepath.Clean(file.Path)) != file.Path || strings.ContainsAny(file.Path, "\x00\r\n\\") {
			return fmt.Errorf("private runtime file path must be canonical")
		}
		if !privateOverlayPath(file.Path) {
			return fmt.Errorf("private runtime file must be inside a named private overlay")
		}
		if seen[file.Path] {
			return fmt.Errorf("duplicate private runtime file %q", file.Path)
		}
		seen[file.Path] = true
		path := filepath.FromSlash(file.Path)
		fileRepo, found, err := canonicalGitRootForPath(filepath.Join(repo, path))
		if err != nil || !found || fileRepo != repo {
			return fmt.Errorf("private runtime file must not enter a nested source repository")
		}
		within, err := filepath.Rel(target, filepath.Join(repo, path))
		if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
			return fmt.Errorf("private runtime file is outside the exact action target")
		}
		for _, args := range [][]string{{"ls-files", "--cached", "-z", "--", file.Path}, {"ls-tree", "-r", "--name-only", "-z", "HEAD", "--", file.Path}} {
			out, err := privateDeliveryGit(repo, args...)
			if err != nil {
				return err
			}
			if len(out) != 0 {
				return fmt.Errorf("private runtime file %q is tracked", file.Path)
			}
		}
		if _, err := privateDeliveryGit(repo, "check-ignore", "-q", "--", file.Path); err != nil {
			return fmt.Errorf("private runtime file %q must be ignored: %w", file.Path, err)
		}
		digest, err := consume(root, path)
		if err != nil {
			return err
		}
		if digest != file.AfterSHA256 {
			return fmt.Errorf("private runtime file %q hash differs from frozen after-image", file.Path)
		}
	}
	return nil
}

func privateOverlayPath(path string) bool {
	return path == "docs_private" || strings.HasPrefix(path, "docs_private/") || path == ".compound-private" || strings.HasPrefix(path, ".compound-private/")
}

func privateDeliveryGit(repo string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	global := []string{"-C", repo}
	// check-ignore consumes literal pathnames, not Git pathspecs, and rejects
	// pathspec magic. The index/tree queries do require literal-pathspec mode.
	if args[0] != "check-ignore" {
		global = append(global, "--literal-pathspecs")
	}
	cmd := exec.CommandContext(ctx, "git", append(global, args...)...)
	cmd.Env = gitrepository.ReadEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("inspect private runtime Git scope: %w", err)
	}
	return out, nil
}

func privateDeliveryPathInfos(root *os.Root, path string) ([]os.FileInfo, error) {
	var infos []os.FileInfo
	current := ""
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("private runtime path contains symlink")
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func privateDeliveryDigest(root *os.Root, path string) (string, error) {
	return privateDeliveryDigestTo(root, path, nil)
}

func privateDeliveryDigestTo(root *os.Root, path string, body io.Writer) (string, error) {
	if err := validatePrivateDeliveryRoot(root); err != nil {
		return "", err
	}
	before, err := privateDeliveryPathInfos(root, path)
	if err != nil {
		return "", err
	}
	leaf := before[len(before)-1]
	if !privateDeliverySingleLink(leaf) {
		return "", fmt.Errorf("private runtime body must be a single-link file")
	}
	if !leaf.Mode().IsRegular() || leaf.Size() > maxPrivateRuntimeDeliveryFileBytes {
		return "", fmt.Errorf("private runtime body must be a bounded regular file")
	}
	file, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(leaf, opened) || !privateDeliverySingleLink(opened) {
		return "", fmt.Errorf("private runtime file changed during open")
	}
	h := sha256.New()
	var output io.Writer = h
	if body != nil {
		output = io.MultiWriter(h, body)
	}
	n, err := io.Copy(output, io.LimitReader(file, maxPrivateRuntimeDeliveryFileBytes+1))
	if err != nil {
		return "", err
	}
	if n != leaf.Size() {
		return "", fmt.Errorf("private runtime file size changed or exceeded limit")
	}
	after, err := privateDeliveryPathInfos(root, path)
	if err != nil {
		return "", err
	}
	for i := range before {
		if !os.SameFile(before[i], after[i]) {
			return "", fmt.Errorf("private runtime path changed during read")
		}
	}
	if after[len(after)-1].Size() != leaf.Size() || !after[len(after)-1].ModTime().Equal(leaf.ModTime()) {
		return "", fmt.Errorf("private runtime file changed during read")
	}
	if !privateDeliverySingleLink(after[len(after)-1]) {
		return "", fmt.Errorf("private runtime body must remain a single-link file")
	}
	if err := validatePrivateDeliveryRoot(root); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func validatePrivateDeliveryRoot(root *os.Root) error {
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	named, err := os.Lstat(root.Name())
	if err != nil {
		return fmt.Errorf("private runtime root changed: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(root.Name())
	if err != nil || canonical != root.Name() || !named.IsDir() || !os.SameFile(opened, named) {
		return fmt.Errorf("private runtime root identity changed")
	}
	return nil
}
