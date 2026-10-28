package home

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ArtifactReader exposes only content-addressed reads from an existing
// canonical COMPOUND_HOME. It never initializes or repairs custody paths.
type ArtifactReader struct {
	home       string
	publicDir  string
	privateDir string
}

// OpenArtifactReader opens a read-only view over an existing canonical home.
func OpenArtifactReader(homePath string) (*ArtifactReader, error) {
	if strings.TrimSpace(homePath) == "" || strings.TrimSpace(homePath) != homePath ||
		!filepath.IsAbs(homePath) || filepath.Clean(homePath) != homePath {
		return nil, fmt.Errorf("artifact reader requires canonical absolute COMPOUND_HOME")
	}
	canonical, err := filepath.EvalSymlinks(homePath)
	if err != nil {
		return nil, err
	}
	if canonical != homePath {
		return nil, fmt.Errorf("artifact reader requires canonical absolute COMPOUND_HOME")
	}
	root, err := openStableOwnedHomeRoot(homePath, privateCustodyHooks{})
	if err != nil {
		return nil, err
	}
	if err := root.Close(); err != nil {
		return nil, err
	}
	return &ArtifactReader{
		home:       homePath,
		publicDir:  filepath.Join(homePath, "artifacts", "sha256"),
		privateDir: filepath.Join(homePath, "private", "artifacts", "sha256"),
	}, nil
}

// Get returns a verified public artifact without creating or repairing paths.
func (r *ArtifactReader) Get(ref string) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("artifact reader is nil")
	}
	if err := r.validateHome(); err != nil {
		return nil, err
	}
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		return nil, err
	}
	body, found, err := ReadRootedRegularFileCanonicalOnly(
		r.home,
		filepath.Join(r.publicDir, hexDigest),
		"artifact",
	)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("artifact %s: %w", ref, os.ErrNotExist)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != hexDigest {
		return nil, fmt.Errorf("artifact %s content hash mismatch", ref)
	}
	return body, nil
}

// GetPrivate validates and returns a body held only in private artifact
// custody. Missing or unsafe custody paths are refused without repair.
func (r *ArtifactReader) GetPrivate(ref string) ([]byte, error) {
	return r.getPrivate(ref, 0)
}

// GetPrivateBounded applies a caller-owned byte limit before body allocation.
// It retains the same custody, content-address and rooted-identity checks as
// GetPrivate. Nonpositive and overflowing limits are refused, not unbounded.
func (r *ArtifactReader) GetPrivateBounded(ref string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes == 1<<63-1 {
		return nil, fmt.Errorf("private artifact byte limit must be positive and bounded")
	}
	return r.getPrivate(ref, maxBytes)
}

func (r *ArtifactReader) getPrivate(ref string, maxBytes int64) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("artifact reader is nil")
	}
	if err := r.validateHome(); err != nil {
		return nil, err
	}
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		return nil, err
	}
	before, err := r.inspectPrivateArtifact(hexDigest, ref)
	if err != nil {
		return nil, err
	}
	body, found, err := readRootedRegularFileCanonicalOnlyWithLimit(
		r.home,
		filepath.Join(r.privateDir, hexDigest),
		"private artifact", maxBytes,
	)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("artifact %s not found: %w", ref, os.ErrNotExist)
	}
	after, err := r.inspectPrivateArtifact(hexDigest, ref)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("artifact %s object must be stable regular file", ref)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != hexDigest {
		return nil, fmt.Errorf("artifact %s content hash mismatch", ref)
	}
	return body, nil
}

func (r *ArtifactReader) validateHome() error {
	root, err := openStableOwnedHomeRoot(r.home, privateCustodyHooks{})
	if err != nil {
		return err
	}
	return root.Close()
}

func (r *ArtifactReader) inspectPrivateArtifact(hexDigest, ref string) (os.FileInfo, error) {
	path := filepath.Join(r.privateDir, hexDigest)
	root, relativePath, displayPath, err := openRootedPath(r.home, path, "private artifact")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	infos, err := lstatRootedRegularPath(root, relativePath, displayPath, "private artifact")
	if err != nil {
		return nil, err
	}
	if len(infos) < 2 {
		return nil, fmt.Errorf("artifact %s private custody path incomplete", ref)
	}
	parentPath := filepath.Dir(relativePath)
	if err := verifyPrivateRootedDirectoryModes(infos[:len(infos)-1], parentPath, displayPath); err != nil {
		return nil, err
	}
	leaf := infos[len(infos)-1]
	if leaf.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("artifact %s object has non-private mode: got %04o want 0600", ref, leaf.Mode().Perm())
	}
	return leaf, nil
}
