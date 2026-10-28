package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const PublicExportSchemaVersion = "compound.release_public_export.v0.1"

type PublicExportOptions struct {
	RepoPath   string
	TargetPath string
	Prefix     string
}

type PublicExportReport struct {
	SchemaVersion string   `json:"schema_version"`
	Status        string   `json:"status"`
	GeneratedAt   string   `json:"generated_at"`
	Repo          string   `json:"repo"`
	Commit        string   `json:"commit"`
	TargetPath    string   `json:"target_path"`
	Prefix        string   `json:"prefix"`
	Files         int      `json:"files"`
	Bytes         int64    `json:"bytes"`
	ProofRefs     []string `json:"proof_refs"`
}

type PublicArchiveScan struct {
	Files int
	Bytes int64
}

func ExportPublic(ctx context.Context, opts PublicExportOptions, now time.Time) (PublicExportReport, error) {
	repo, err := gitTopLevel(ctx, opts.RepoPath)
	if err != nil {
		return PublicExportReport{}, err
	}
	commit, err := gitTrimmed(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return PublicExportReport{}, err
	}
	target, err := normalizeTarget(opts.TargetPath)
	if err != nil {
		return PublicExportReport{}, err
	}
	prefix := normalizePrefix(opts.Prefix)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return PublicExportReport{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".compound-public-export-*.tar.gz")
	if err != nil {
		return PublicExportReport{}, err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()

	cmd := exec.CommandContext(ctx, "git", "-C", repo, "archive", "--format=tar", "--prefix", prefix, "HEAD")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return PublicExportReport{}, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return PublicExportReport{}, err
	}
	scan, copyErr := copyCheckedTarGzip(stdout, tmp, prefix)
	if copyErr != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return PublicExportReport{}, copyErr
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		return PublicExportReport{}, fmt.Errorf("git archive failed: %w%s", waitErr, stderrSuffix(stderr.String()))
	}
	if err := tmp.Close(); err != nil {
		return PublicExportReport{}, err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return PublicExportReport{}, err
	}
	ok = true
	return PublicExportReport{
		SchemaVersion: PublicExportSchemaVersion,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Repo:          repo,
		Commit:        commit,
		TargetPath:    target,
		Prefix:        prefix,
		Files:         scan.Files,
		Bytes:         scan.Bytes,
		ProofRefs:     []string{"git-archive-head", "public-export-private-path-gate"},
	}, nil
}

func ValidatePublicArchive(r io.Reader, prefix string) (PublicArchiveScan, error) {
	tr := tar.NewReader(r)
	var scan PublicArchiveScan
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return scan, nil
		}
		if err != nil {
			return scan, err
		}
		if err := ValidatePublicExportPath(hdr.Name, prefix); err != nil {
			return scan, err
		}
		if tarRegularFile(hdr) {
			scan.Files++
			scan.Bytes += hdr.Size
		}
	}
}

func ValidatePublicExportPath(name, prefix string) error {
	payload, parts, err := payloadPathParts(name, prefix)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return nil
	}
	switch parts[0] {
	case "docs_private", ".compound-private", ".claude", "Plans", "memory", "bin":
		return fmt.Errorf("public export contains private/runtime path %q", payload)
	}
	if len(parts) == 1 && parts[0] == "compound" {
		return fmt.Errorf("public export contains built binary %q", payload)
	}
	for _, part := range parts {
		switch part {
		case ".git":
			return fmt.Errorf("public export contains nested git metadata path %q", payload)
		case ".DS_Store":
			return fmt.Errorf("public export contains Finder metadata path %q", payload)
		}
	}
	if strings.HasSuffix(parts[len(parts)-1], ".private.md") {
		return fmt.Errorf("public export contains private markdown path %q", payload)
	}
	return nil
}

func copyCheckedTarGzip(src io.Reader, dst io.Writer, prefix string) (PublicArchiveScan, error) {
	tr := tar.NewReader(src)
	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)
	var scan PublicArchiveScan
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return scan, err
		}
		if err := ValidatePublicExportPath(hdr.Name, prefix); err != nil {
			return scan, err
		}
		hdrCopy := *hdr
		if err := tw.WriteHeader(&hdrCopy); err != nil {
			return scan, err
		}
		if hdr.Size > 0 {
			n, err := io.CopyN(tw, tr, hdr.Size)
			if err != nil {
				return scan, err
			}
			if tarRegularFile(hdr) {
				scan.Files++
				scan.Bytes += n
			}
		}
	}
	// tar.Reader stops at the logical end marker, before producers such as
	// git archive necessarily finish writing record padding to their pipe.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return scan, fmt.Errorf("drain archive source: %w", err)
	}
	if err := tw.Close(); err != nil {
		return scan, err
	}
	if err := gz.Close(); err != nil {
		return scan, err
	}
	return scan, nil
}

func payloadPathParts(name, prefix string) (string, []string, error) {
	raw := strings.TrimSpace(name)
	if raw == "" || strings.HasPrefix(raw, "/") {
		return "", nil, fmt.Errorf("invalid archive path %q", name)
	}
	clean := path.Clean(raw)
	if clean == "." {
		return "", nil, nil
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if part == ".." {
			return "", nil, fmt.Errorf("invalid archive path %q", name)
		}
	}
	prefixRoot := strings.TrimSuffix(normalizePrefix(prefix), "/")
	if len(parts) > 0 && parts[0] == prefixRoot {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return "", nil, nil
	}
	return strings.Join(parts, "/"), parts, nil
}

func gitTopLevel(ctx context.Context, repoPath string) (string, error) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		repoPath = "."
	}
	top, err := gitTrimmed(ctx, repoPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.Abs(top)
}

func gitTrimmed(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %w%s", strings.Join(args, " "), err, stderrSuffix(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func normalizeTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("target path required")
	}
	return filepath.Abs(target)
}

func normalizePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = "compound-public"
	}
	return prefix + "/"
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return ": " + stderr
}

func tarRegularFile(hdr *tar.Header) bool {
	return hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA
}
