//go:build ignore

// release-artifacts builds the complete public release candidate into a fresh
// directory. It deliberately uses Go's archive writers rather than a host tar
// dialect, so the same bytes are produced on GNU-tar and BSD-tar hosts.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var stableSemVer = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

var releaseEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

type releasePlatform struct {
	os   string
	arch string
}

var releasePlatforms = []releasePlatform{
	{os: "darwin", arch: "arm64"},
	{os: "linux", arch: "amd64"},
}

type builtArtifact struct {
	platform     releasePlatform
	archive      string
	compound     string
	breadth      string
	compoundSBOM string
	breadthSBOM  string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "release-artifacts:", err)
		os.Exit(1)
	}
}

func run() error {
	version := strings.TrimSpace(os.Getenv("VERSION"))
	if !stableSemVer.MatchString(version) {
		return fmt.Errorf("VERSION=%q is not strict stable SemVer (MAJOR.MINOR.PATCH, no leading zeros, prerelease, or build metadata)", version)
	}
	root := strings.TrimSpace(os.Getenv("SOURCE_ROOT"))
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve source root: %w", err)
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve source root: %w", err)
	}
	dist := strings.TrimSpace(os.Getenv("DIST"))
	if dist == "" {
		dist = "dist"
	}
	if !filepath.IsAbs(dist) {
		dist = filepath.Join(root, dist)
	}
	dist = filepath.Clean(dist)
	if err := requireEmptyDestination(dist); err != nil {
		return err
	}

	buildGo := envOr("BUILD_GO", "go")
	buildGit := envOr("BUILD_GIT", "git")
	sbomTool := envOr("SBOM_TOOL", "cyclonedx-gomod")
	if err := requireCleanSource(root, buildGit); err != nil {
		return err
	}
	commit, err := commandOutput(root, nil, buildGit, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve release commit: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if !fullCommit.MatchString(commit) {
		return fmt.Errorf("git HEAD %q is not a full 40-character lowercase commit", commit)
	}
	releaseEnv := withEnv(os.Environ(),
		"COMPOUND_RELEASE_VERSION="+version,
		"COMPOUND_RELEASE_COMMIT="+commit,
	)
	reported, err := commandOutput(root, releaseEnv, buildGo, "run", "./cmd/compound", "version")
	if err != nil {
		return fmt.Errorf("probe source version: %w", err)
	}
	if strings.TrimSpace(reported) != "compound "+version+" (build dev)" {
		return fmt.Errorf("VERSION=%s does not match source version output %q", version, strings.TrimSpace(reported))
	}
	if _, err := exec.LookPath(sbomTool); err != nil && filepath.Base(sbomTool) == sbomTool {
		return fmt.Errorf("SBOM tool not found: %s", sbomTool)
	}

	parent := filepath.Dir(dist)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create release parent: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".compound-release-stage-*")
	if err != nil {
		return fmt.Errorf("create release stage: %w", err)
	}
	stageLive := true
	defer func() {
		if stageLive {
			_ = os.RemoveAll(stage)
		}
	}()

	var artifacts []builtArtifact
	for _, platform := range releasePlatforms {
		artifact, err := buildPlatform(root, stage, buildGo, sbomTool, version, commit, platform, releaseEnv)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, artifact)
	}
	if err := copyFile(filepath.Join(root, "scripts", "install-release.sh"), filepath.Join(stage, "install-release.sh"), 0o755, "installer source"); err != nil {
		return fmt.Errorf("stage installer: %w", err)
	}
	if _, err := requireRegularFile(filepath.Join(stage, "install-release.sh"), "staged installer"); err != nil {
		return err
	}
	if err := os.Chtimes(filepath.Join(stage, "install-release.sh"), releaseEpoch, releaseEpoch); err != nil {
		return fmt.Errorf("normalize installer time: %w", err)
	}
	manifest := releaseManifest(version, commit, artifacts)
	if err := writeFreshRegular(filepath.Join(stage, "RELEASE"), []byte(manifest), 0o644, "RELEASE output"); err != nil {
		return fmt.Errorf("write RELEASE: %w", err)
	}

	expected := expectedReleaseFiles(version)
	withoutSums := make([]string, 0, len(expected)-1)
	for _, name := range expected {
		if name != "SHA256SUMS" {
			withoutSums = append(withoutSums, name)
		}
	}
	if err := requireExactFiles(stage, withoutSums); err != nil {
		return err
	}
	var sums strings.Builder
	for _, name := range withoutSums {
		digest, err := sha256File(filepath.Join(stage, name))
		if err != nil {
			return fmt.Errorf("hash %s: %w", name, err)
		}
		fmt.Fprintf(&sums, "%s  %s\n", digest, name)
	}
	if err := writeFreshRegular(filepath.Join(stage, "SHA256SUMS"), []byte(sums.String()), 0o644, "SHA256SUMS output"); err != nil {
		return fmt.Errorf("write SHA256SUMS: %w", err)
	}
	if err := requireExactFiles(stage, expected); err != nil {
		return err
	}
	if err := publishFreshDirectory(stage, dist); err != nil {
		return err
	}
	stageLive = false
	fmt.Printf("release-artifacts: wrote exact %d-file release candidate to %s\n", len(expected), dist)
	return nil
}

func buildPlatform(root, stage, buildGo, sbomTool, version, commit string, platform releasePlatform, baseEnv []string) (builtArtifact, error) {
	bundle := fmt.Sprintf("compound_%s_%s_%s", version, platform.os, platform.arch)
	buildDir, err := os.MkdirTemp(stage, ".build-"+platform.os+"-"+platform.arch+"-")
	if err != nil {
		return builtArtifact{}, fmt.Errorf("create %s/%s build stage: %w", platform.os, platform.arch, err)
	}
	defer os.RemoveAll(buildDir)
	compoundPath := filepath.Join(buildDir, "compound")
	breadthPath := filepath.Join(buildDir, "compound-breadth-engine")
	env := withEnv(baseEnv,
		"GOOS="+platform.os,
		"GOARCH="+platform.arch,
		"CGO_ENABLED=0",
	)
	compoundLDFlags := "-s -w -buildid= -X main.buildSHA=" + commit
	if _, err := commandOutput(root, env, buildGo, "build", "-trimpath", "-buildvcs=false", "-ldflags", compoundLDFlags, "-o", compoundPath, "./cmd/compound"); err != nil {
		return builtArtifact{}, fmt.Errorf("build compound for %s/%s: %w", platform.os, platform.arch, err)
	}
	if _, err := commandOutput(root, env, buildGo, "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -buildid=", "-o", breadthPath, "./cmd/compound-breadth-engine"); err != nil {
		return builtArtifact{}, fmt.Errorf("build breadth engine for %s/%s: %w", platform.os, platform.arch, err)
	}
	for _, path := range []string{compoundPath, breadthPath} {
		if _, err := requireRegularFile(path, "build output"); err != nil {
			return builtArtifact{}, err
		}
		if err := os.Chmod(path, 0o755); err != nil {
			return builtArtifact{}, fmt.Errorf("chmod %s: %w", filepath.Base(path), err)
		}
		if err := os.Chtimes(path, releaseEpoch, releaseEpoch); err != nil {
			return builtArtifact{}, fmt.Errorf("normalize %s timestamp: %w", filepath.Base(path), err)
		}
	}
	compoundDigest, err := sha256File(compoundPath)
	if err != nil {
		return builtArtifact{}, err
	}
	breadthDigest, err := sha256File(breadthPath)
	if err != nil {
		return builtArtifact{}, err
	}
	identity := strings.Join([]string{
		"schema compound.release_archive.v0.1",
		"version " + version,
		"commit " + commit,
		"platform " + platform.os + " " + platform.arch,
		"binary compound " + compoundDigest,
		"binary compound-breadth-engine " + breadthDigest,
		"",
	}, "\n")
	archiveName := bundle + ".tar.gz"
	if err := writeDeterministicArchive(filepath.Join(stage, archiveName), bundle, compoundPath, breadthPath, []byte(identity)); err != nil {
		return builtArtifact{}, fmt.Errorf("archive %s/%s: %w", platform.os, platform.arch, err)
	}
	if _, err := requireRegularFile(filepath.Join(stage, archiveName), "release archive output"); err != nil {
		return builtArtifact{}, err
	}
	compoundSBOM := bundle + ".cdx.json"
	breadthSBOM := bundle + "_compound-breadth-engine.cdx.json"
	// Inspect the packaged executables. Source Git layouts and later tag creation
	// must not change the SBOM identity of an already built release candidate.
	if _, err := commandOutput(root, env, sbomTool, "bin", "-json", "-noserial", "-notimestamp", "-output", filepath.Join(stage, compoundSBOM), "-version", version, compoundPath); err != nil {
		return builtArtifact{}, fmt.Errorf("generate compound SBOM for %s/%s: %w", platform.os, platform.arch, err)
	}
	if _, err := requireRegularFile(filepath.Join(stage, compoundSBOM), "SBOM output"); err != nil {
		return builtArtifact{}, err
	}
	if _, err := commandOutput(root, env, sbomTool, "bin", "-json", "-noserial", "-notimestamp", "-output", filepath.Join(stage, breadthSBOM), "-version", version, breadthPath); err != nil {
		return builtArtifact{}, fmt.Errorf("generate breadth-engine SBOM for %s/%s: %w", platform.os, platform.arch, err)
	}
	if _, err := requireRegularFile(filepath.Join(stage, breadthSBOM), "SBOM output"); err != nil {
		return builtArtifact{}, err
	}
	return builtArtifact{
		platform:     platform,
		archive:      archiveName,
		compound:     compoundPath,
		breadth:      breadthPath,
		compoundSBOM: compoundSBOM,
		breadthSBOM:  breadthSBOM,
	}, nil
}

func writeDeterministicArchive(target, bundle, compoundPath, breadthPath string, identity []byte) (returnErr error) {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.Header.ModTime = releaseEpoch
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	closeWriters := func() error {
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: bundle + "/", Mode: 0o755, Typeflag: tar.TypeDir,
		ModTime: releaseEpoch, Format: tar.FormatUSTAR,
	}); err != nil {
		return err
	}
	for _, item := range []struct {
		name string
		path string
	}{
		{name: "compound", path: compoundPath},
		{name: "compound-breadth-engine", path: breadthPath},
	} {
		src, opened, err := openRegularFile(item.path, "archive input")
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name: bundle + "/" + item.name, Mode: 0o755, Size: opened.Size(), Typeflag: tar.TypeReg,
			ModTime: releaseEpoch, Format: tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = src.Close()
			return err
		}
		written, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		if written != opened.Size() {
			return fmt.Errorf("archive input size changed while reading")
		}
		if closeErr != nil {
			return closeErr
		}
		if err := verifyRegularIdentity(item.path, "archive input", opened); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: bundle + "/RELEASE-IDENTITY", Mode: 0o644, Size: int64(len(identity)), Typeflag: tar.TypeReg,
		ModTime: releaseEpoch, Format: tar.FormatUSTAR,
	}); err != nil {
		return err
	}
	if _, err := tw.Write(identity); err != nil {
		return err
	}
	return closeWriters()
}

func releaseManifest(version, commit string, artifacts []builtArtifact) string {
	var body strings.Builder
	fmt.Fprintln(&body, "schema compound.release.v0.2")
	fmt.Fprintln(&body, "version "+version)
	fmt.Fprintln(&body, "commit "+commit)
	fmt.Fprintln(&body, "installer install-release.sh")
	for _, artifact := range artifacts {
		fmt.Fprintf(&body, "artifact %s %s %s %s %s\n",
			artifact.platform.os, artifact.platform.arch, artifact.archive, artifact.compoundSBOM, artifact.breadthSBOM)
	}
	return body.String()
}

func expectedReleaseFiles(version string) []string {
	names := []string{"RELEASE", "SHA256SUMS", "install-release.sh"}
	for _, platform := range releasePlatforms {
		bundle := fmt.Sprintf("compound_%s_%s_%s", version, platform.os, platform.arch)
		names = append(names,
			bundle+".tar.gz",
			bundle+".cdx.json",
			bundle+"_compound-breadth-engine.cdx.json",
		)
	}
	sort.Strings(names)
	return names
}

func requireCleanSource(root, git string) error {
	out, err := commandOutput(root, nil, git, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect source state: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return errors.New("refuses dirty source; commit and verify the exact release tree first")
	}
	return nil
}

func requireEmptyDestination(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect DIST: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("DIST %s must be empty or absent; refusing stale artifact publication", path)
	}
	return nil
}

func publishFreshDirectory(stage, dist string) error {
	if err := requireEmptyDestination(dist); err != nil {
		return err
	}
	if err := os.Remove(dist); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty DIST before publication: %w", err)
	}
	if err := os.Rename(stage, dist); err != nil {
		return fmt.Errorf("publish release candidate: %w", err)
	}
	return nil
}

func requireExactFiles(dir string, expected []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, err := requireRegularFile(filepath.Join(dir, entry.Name()), "release stage entry "+entry.Name()); err != nil {
			return err
		}
		got = append(got, entry.Name())
	}
	sort.Strings(got)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		return fmt.Errorf("release inventory mismatch: got %v, want exactly %v", got, want)
	}
	return nil
}

func commandOutput(dir string, env []string, executable string, args ...string) (string, error) {
	cmd := exec.Command(executable, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	body, err := cmd.CombinedOutput()
	if err != nil {
		return string(body), fmt.Errorf("%s %s: %w\n%s", executable, strings.Join(args, " "), err, body)
	}
	return string(body), nil
}

func withEnv(base []string, values ...string) []string {
	replacements := make(map[string]string, len(values))
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		replacements[key] = value
	}
	result := make([]string, 0, len(base)+len(values))
	for _, value := range base {
		key, _, _ := strings.Cut(value, "=")
		if _, replace := replacements[key]; !replace {
			result = append(result, value)
		}
	}
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func sha256File(path string) (string, error) {
	f, opened, err := openRegularFile(path, "hash input")
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if err := verifyRegularIdentity(path, "hash input", opened); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(source, target string, mode os.FileMode, sourcePurpose string) error {
	src, opened, err := openRegularFile(source, sourcePurpose)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return verifyRegularIdentity(source, sourcePurpose, opened)
}

func requireRegularFile(path, purpose string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", purpose, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular non-symlink file", purpose)
	}
	return info, nil
}

func openRegularFile(path, purpose string) (*os.File, os.FileInfo, error) {
	before, err := requireRegularFile(path, purpose)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s changed identity while opening", purpose)
	}
	return f, opened, nil
}

func verifyRegularIdentity(path, purpose string, opened os.FileInfo) error {
	after, err := requireRegularFile(path, purpose)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, after) {
		return fmt.Errorf("%s changed identity while reading", purpose)
	}
	return nil
}

func writeFreshRegular(path string, body []byte, mode os.FileMode, purpose string) (returnErr error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	if _, err := f.Write(body); err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular non-symlink file", purpose)
	}
	return verifyRegularIdentity(path, purpose, opened)
}
