package home

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ArtifactRefPrefix is the stable reference grammar for immutable bodies held in
// COMPOUND_HOME/artifacts/sha256.
const ArtifactRefPrefix = "artifact:sha256:"

// ArtifactStore owns immutable, content-addressed bytes. It is intentionally in
// home: physical custody is a property of COMPOUND_HOME, not of any one object
// family that happens to consume the bytes.
type ArtifactStore struct {
	dir        string
	privateDir string
	home       string
}

func NewArtifactStore(homePath string) (*ArtifactStore, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("artifact store requires COMPOUND_HOME")
	}
	dir := filepath.Join(homePath, "artifacts", "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &ArtifactStore{dir: dir, privateDir: filepath.Join(homePath, "private", "artifacts", "sha256"), home: homePath}, nil
}

func (s *ArtifactStore) Home() string { return s.home }

// Put stores body without ever replacing a previously-addressed object. A
// temporary file plus hard-link publication makes concurrent first writes safe;
// an existing object must already contain the exact same bytes.
func (s *ArtifactStore) Put(body []byte) (string, error) {
	return s.put(body, s.dir, 0o644)
}

// PutPrivate stores body in COMPOUND_HOME/private/artifacts/sha256 without ever
// entering the public artifact root.
func (s *ArtifactStore) PutPrivate(body []byte) (string, error) {
	if err := s.ensurePrivateArtifactRoot(); err != nil {
		return "", err
	}
	var ref string
	acquired, err := withPrivateArtifactDirectoryLock(s.home, false, func(root *os.Root) error {
		var putErr error
		ref, putErr = s.putPrivateLocked(root, body)
		return putErr
	})
	if err != nil {
		return "", err
	}
	if !acquired {
		return "", fmt.Errorf("private artifact writer failed to acquire custody lock")
	}
	return ref, nil
}

func (s *ArtifactStore) putPrivateLocked(root *os.Root, body []byte) (string, error) {
	sum := sha256.Sum256(body)
	hexDigest := hex.EncodeToString(sum[:])
	ref := ArtifactRefPrefix + hexDigest
	dirRel := "."
	digestRel := hexDigest
	if exists, err := verifiedPrivateBodyRooted(root, digestRel, hexDigest, body, 1); err != nil {
		return "", err
	} else if exists {
		if err := syncRootedDirectoryStrict(root, dirRel, s.privateDir, "private artifact writer"); err != nil {
			return "", fmt.Errorf("sync existing private artifact digest: %w", err)
		}
		return ref, nil
	}

	var tmp *os.File
	var tmpRel string
	for range 100 {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		tmpRel = fmt.Sprintf(".artifact-%d", binary.BigEndian.Uint64(nonce[:]))
		var err error
		tmp, err = root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		break
	}
	if tmp == nil {
		return "", fmt.Errorf("allocate private artifact stage")
	}
	stagedDurable := false
	defer func() {
		if !stagedDurable {
			_ = root.Remove(tmpRel)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	stagedDurable = true
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := root.Link(tmpRel, digestRel); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		exists, verifyErr := verifiedPrivateBodyRooted(root, digestRel, hexDigest, body, 1)
		if verifyErr != nil {
			return "", verifyErr
		}
		if !exists {
			return "", fmt.Errorf("private artifact digest disappeared after publication race: %s", ref)
		}
	} else {
		exists, err := verifiedPrivateBodyRooted(root, digestRel, hexDigest, body, 2)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", fmt.Errorf("private artifact digest disappeared after publication: %s", ref)
		}
	}
	if err := syncRootedDirectoryStrict(root, dirRel, s.privateDir, "private artifact writer"); err != nil {
		return "", fmt.Errorf("sync artifact directory: %w", err)
	}
	if exists, err := verifiedPrivateBodyRooted(root, digestRel, hexDigest, body, 2); err != nil {
		return "", err
	} else if !exists {
		return "", fmt.Errorf("private artifact digest disappeared before stage removal: %s", ref)
	}
	if err := root.Remove(tmpRel); err != nil {
		return "", fmt.Errorf("remove published private artifact stage: %w", err)
	}
	if err := syncRootedDirectoryStrict(root, dirRel, s.privateDir, "private artifact writer"); err != nil {
		return "", fmt.Errorf("sync removed private artifact stage: %w", err)
	}
	if exists, err := verifiedPrivateBodyRooted(root, digestRel, hexDigest, body, 1); err != nil {
		return "", err
	} else if !exists {
		return "", fmt.Errorf("private artifact digest disappeared after stage removal: %s", ref)
	}
	return ref, nil
}

func verifiedPrivateBodyRooted(root *os.Root, rel, digest string, body []byte, expectedLinks uint64) (bool, error) {
	snapshot, err := readStablePrivateRecoveryFile(root, rel, rel)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	links, ok := privateObjectLinkCount(snapshot.info)
	if !ok || links != expectedLinks || snapshot.digest != digest || snapshot.bytes != int64(len(body)) {
		return true, fmt.Errorf("private artifact digest identity, links, or hash differs: %s", rel)
	}
	file, err := root.Open(rel)
	if err != nil {
		return true, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(snapshot.info, opened) {
		return true, fmt.Errorf("private artifact digest changed before byte comparison: %s: %v", rel, err)
	}
	existing, err := io.ReadAll(io.LimitReader(file, int64(len(body))+1))
	if err != nil {
		return true, err
	}
	if !bytes.Equal(existing, body) {
		return true, fmt.Errorf("private artifact content-address collision: %s", rel)
	}
	final, err := lstatRootedRegularPath(root, rel, rel, "private artifact writer")
	if err != nil || !os.SameFile(snapshot.info, final[len(final)-1]) {
		return true, fmt.Errorf("private artifact digest changed after byte comparison: %s: %v", rel, err)
	}
	return true, nil
}

func (s *ArtifactStore) put(body []byte, dir string, mode os.FileMode) (string, error) {
	sum := sha256.Sum256(body)
	hexDigest := hex.EncodeToString(sum[:])
	ref := ArtifactRefPrefix + hexDigest
	path := filepath.Join(dir, hexDigest)
	if existing, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(existing, body) {
			return "", fmt.Errorf("artifact %s content-address collision", ref)
		}
		return ref, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, ".artifact-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		if !bytes.Equal(existing, body) {
			return "", fmt.Errorf("artifact %s content-address collision", ref)
		}
	}
	// The file bytes were synced before publication. Sync the containing
	// directory as well so a successful Put means the name-to-inode link is
	// durable across a crash, not merely visible to this process.
	if err := syncDirectory(dir); err != nil {
		return "", fmt.Errorf("sync artifact directory: %w", err)
	}
	return ref, nil
}

// PutFileExpected streams sourcePath into immutable custody and publishes it
// only when its bytes match expectedRef. The source and any existing artifact
// are hashed without loading the file into memory.
func (s *ArtifactStore) PutFileExpected(sourcePath, expectedRef string) (string, int64, error) {
	hexDigest, err := ArtifactHex(expectedRef)
	if err != nil {
		return "", 0, err
	}
	source, err := os.Open(strings.TrimSpace(sourcePath))
	if err != nil {
		return "", 0, err
	}
	defer source.Close()

	tmp, err := os.CreateTemp(s.dir, ".artifact-*")
	if err != nil {
		return "", 0, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", 0, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), source)
	if copyErr != nil {
		tmp.Close()
		return "", 0, copyErr
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	gotDigest := hex.EncodeToString(hash.Sum(nil))
	if gotDigest != hexDigest {
		return "", 0, fmt.Errorf("source %s content hash mismatch for %s: got sha256:%s", sourcePath, expectedRef, gotDigest)
	}

	path := filepath.Join(s.dir, hexDigest)
	if err := os.Link(tmpPath, path); err != nil {
		if !os.IsExist(err) {
			return "", 0, err
		}
		existingBytes, verifyErr := s.Verify(expectedRef)
		if verifyErr != nil {
			return "", 0, verifyErr
		}
		if existingBytes != written {
			return "", 0, fmt.Errorf("artifact %s byte count mismatch: existing=%d source=%d", expectedRef, existingBytes, written)
		}
	}
	if err := syncDirectory(s.dir); err != nil {
		return "", 0, fmt.Errorf("sync artifact directory: %w", err)
	}
	return expectedRef, written, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Get validates both the reference grammar and the bytes. A corrupt object is
// never served merely because a file exists at the expected path.
func (s *ArtifactStore) Get(ref string) ([]byte, error) {
	return s.get(ref, s.dir)
}

// GetPrivate validates and returns a body held only in private artifact custody.
func (s *ArtifactStore) GetPrivate(ref string) ([]byte, error) {
	if err := s.ensurePrivateArtifactRoot(); err != nil {
		return nil, err
	}
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		return nil, err
	}
	file, _, err := openVerifiedStablePrivateArtifactObject(s.privateDir, hexDigest, ref)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (s *ArtifactStore) get(ref, dir string) ([]byte, error) {
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(dir, hexDigest))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != hexDigest {
		return nil, fmt.Errorf("artifact %s content hash mismatch", ref)
	}
	return body, nil
}

// Verify checks an artifact's content hash using bounded memory and returns its
// byte count.
func (s *ArtifactStore) Verify(ref string) (int64, error) {
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		return 0, err
	}
	return verifyFileDigest(filepath.Join(s.dir, hexDigest), hexDigest, ref)
}

// VerifyPrivate checks a private artifact's content hash using bounded memory
// and returns its byte count.
func (s *ArtifactStore) VerifyPrivate(ref string) (int64, error) {
	if err := s.ensurePrivateArtifactRoot(); err != nil {
		return 0, err
	}
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		return 0, err
	}
	file, n, err := openVerifiedStablePrivateArtifactObject(s.privateDir, hexDigest, ref)
	if err != nil {
		return 0, err
	}
	return n, file.Close()
}

// Materialize streams an immutable artifact to a new path without overwriting
// an existing non-identical target. Both the stored artifact and the published
// target are verified against ref.
func (s *ArtifactStore) Materialize(ref, targetPath string, mode os.FileMode) (int64, bool, error) {
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		return 0, false, err
	}
	expectedBytes, err := s.Verify(ref)
	if err != nil {
		return 0, false, err
	}
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		return 0, false, fmt.Errorf("artifact materialize target required")
	}
	mode &= os.ModePerm
	if mode == 0 {
		return 0, false, fmt.Errorf("artifact materialize mode required")
	}
	if n, exists, err := verifyExistingMaterializedTarget(targetPath, hexDigest, ref, mode, expectedBytes); exists || err != nil {
		return n, exists && err == nil, err
	}

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, false, err
	}
	source, err := os.Open(filepath.Join(s.dir, hexDigest))
	if err != nil {
		return 0, false, err
	}
	defer source.Close()
	tmp, err := os.CreateTemp(dir, ".compound-materialize-*")
	if err != nil {
		return 0, false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return 0, false, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), source)
	if copyErr != nil {
		tmp.Close()
		return 0, false, copyErr
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, false, err
	}
	if err := tmp.Close(); err != nil {
		return 0, false, err
	}
	if written != expectedBytes || hex.EncodeToString(hash.Sum(nil)) != hexDigest {
		return 0, false, fmt.Errorf("artifact %s changed during materialization", ref)
	}
	if err := os.Link(tmpPath, targetPath); err != nil {
		if !os.IsExist(err) {
			return 0, false, err
		}
		return verifyExistingMaterializedTarget(targetPath, hexDigest, ref, mode, expectedBytes)
	}
	if err := syncDirectory(dir); err != nil {
		return 0, false, fmt.Errorf("sync materialized artifact directory: %w", err)
	}
	return written, false, nil
}

// MaterializePrivateRooted streams a private immutable artifact into private
// custody beneath COMPOUND_HOME. It reads only the private artifact root,
// creates target directories with private custody permissions, refuses symlinked
// paths, and publishes a new 0600 file without replacing an existing target.
func (s *ArtifactStore) MaterializePrivateRooted(ref, targetPath string) (int64, bool, error) {
	hexDigest, err := ArtifactHex(ref)
	if err != nil {
		return 0, false, err
	}
	if err := s.ensurePrivateArtifactRoot(); err != nil {
		return 0, false, err
	}
	root, relativePath, displayPath, err := openRootedPath(s.home, targetPath, "private artifact")
	if err != nil {
		return 0, false, err
	}
	defer root.Close()
	source, expectedBytes, err := openVerifiedStablePrivateArtifactObject(s.privateDir, hexDigest, ref)
	if err != nil {
		return 0, false, err
	}
	defer source.Close()
	return s.materializePrivateRooted(root, relativePath, displayPath, source, hexDigest, ref, expectedBytes)
}

func (s *ArtifactStore) materializePrivateRooted(root *os.Root, relativePath, displayPath string, source *os.File, hexDigest, ref string, expectedBytes int64) (int64, bool, error) {
	parentPath := filepath.Dir(relativePath)
	parents, err := lstatRootedDirectoryPath(root, parentPath, displayPath, "private artifact", true)
	if err != nil {
		return 0, false, err
	}
	if err := verifyPrivateRootedDirectoryModes(parents, parentPath, displayPath); err != nil {
		return 0, false, err
	}
	parents, err = ensureRootedDirectories(root, parentPath, displayPath, "private artifact", 0o700)
	if err != nil {
		return 0, false, err
	}
	if err := verifyPrivateRootedDirectoryModes(parents, parentPath, displayPath); err != nil {
		return 0, false, err
	}
	if n, exists, err := verifyExistingPrivateMaterializedTarget(root, relativePath, displayPath, hexDigest, ref, expectedBytes); exists || err != nil {
		return n, exists, err
	}

	tmpName, err := privateMaterializeTempName(root, parentPath)
	if err != nil {
		return 0, false, err
	}
	tmpRelativePath := filepath.Join(parentPath, tmpName)
	tmp, err := root.OpenFile(tmpRelativePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, false, err
	}
	defer root.Remove(tmpRelativePath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), source)
	if copyErr != nil {
		tmp.Close()
		return 0, false, copyErr
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, false, err
	}
	if err := tmp.Close(); err != nil {
		return 0, false, err
	}
	if written != expectedBytes || hex.EncodeToString(hash.Sum(nil)) != hexDigest {
		return 0, false, fmt.Errorf("artifact %s changed during private materialization", ref)
	}
	if err := root.Link(tmpRelativePath, relativePath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return verifyExistingPrivateMaterializedTarget(root, relativePath, displayPath, hexDigest, ref, expectedBytes)
		}
		return 0, false, err
	}
	n, exists, err := verifyExistingPrivateMaterializedTarget(root, relativePath, displayPath, hexDigest, ref, expectedBytes)
	if err != nil {
		return 0, false, err
	}
	if !exists {
		return 0, false, fmt.Errorf("private artifact target disappeared after publication: %s", displayPath)
	}
	if err := syncRootedDirectory(root, parentPath, displayPath, "private artifact"); err != nil {
		return 0, false, fmt.Errorf("sync private materialized artifact directory: %w", err)
	}
	return n, false, nil
}

func (s *ArtifactStore) ensurePrivateArtifactRoot() error {
	root, relativePath, displayPath, err := openRootedPath(s.home, s.privateDir, "private artifact root")
	if err != nil {
		return err
	}
	defer root.Close()
	parents, err := lstatRootedDirectoryPath(root, relativePath, displayPath, "private artifact root", true)
	if err != nil {
		return err
	}
	if err := verifyPrivateRootedDirectoryModes(parents, relativePath, displayPath); err != nil {
		return err
	}
	parents, err = ensureRootedDirectories(root, relativePath, displayPath, "private artifact root", 0o700)
	if err != nil {
		return err
	}
	if err := verifyPrivateRootedDirectoryModes(parents, relativePath, displayPath); err != nil {
		return err
	}
	return syncRootedDirectory(root, relativePath, displayPath, "private artifact root")
}

func verifyPrivateRootedDirectoryModes(infos []os.FileInfo, relativePath, displayPath string) error {
	if relativePath == "." {
		return nil
	}
	for _, info := range infos {
		if info.Mode().Perm() != 0o700 {
			return fmt.Errorf("refusing private artifact directory with non-private mode %s: got %04o want 0700", displayPath, info.Mode().Perm())
		}
	}
	return nil
}

func openVerifiedStablePrivateArtifactObject(dir, hexDigest, ref string) (*os.File, int64, error) {
	const mode os.FileMode = 0o600
	path := filepath.Join(dir, hexDigest)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("artifact %s object must be regular file", ref)
	}
	if before.Mode().Perm() != mode.Perm() {
		return nil, 0, fmt.Errorf("artifact %s object has non-private mode: got %04o want %04o", ref, before.Mode().Perm(), mode.Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		file.Close()
		return nil, 0, fmt.Errorf("artifact %s object must be stable regular file", ref)
	}
	if after.Mode().Perm() != mode.Perm() || opened.Mode().Perm() != mode.Perm() {
		file.Close()
		return nil, 0, fmt.Errorf("artifact %s object has non-private mode", ref)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != hexDigest {
		file.Close()
		return nil, 0, fmt.Errorf("artifact %s content hash mismatch", ref)
	}
	afterRead, err := os.Lstat(path)
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	openedAfterRead, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	if afterRead.Mode()&os.ModeSymlink != 0 || !afterRead.Mode().IsRegular() || !openedAfterRead.Mode().IsRegular() || !os.SameFile(before, afterRead) || !os.SameFile(before, openedAfterRead) {
		file.Close()
		return nil, 0, fmt.Errorf("artifact %s object must be stable regular file", ref)
	}
	if afterRead.Mode().Perm() != mode.Perm() || openedAfterRead.Mode().Perm() != mode.Perm() {
		file.Close()
		return nil, 0, fmt.Errorf("artifact %s object has non-private mode", ref)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, n, nil
}

func privateMaterializeTempName(root *os.Root, parentPath string) (string, error) {
	var nonce [16]byte
	for range 100 {
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		name := ".compound-private-materialize-" + hex.EncodeToString(nonce[:])
		if _, err := root.Lstat(filepath.Join(parentPath, name)); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("allocate private materialization temp name")
}

func verifyExistingPrivateMaterializedTarget(root *os.Root, relativePath, displayPath, hexDigest, ref string, expectedBytes int64) (int64, bool, error) {
	before, err := lstatRootedRegularPath(root, relativePath, displayPath, "private artifact")
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, true, err
	}
	leaf := before[len(before)-1]
	if leaf.Mode().Perm() != 0o600 {
		return 0, true, fmt.Errorf("refusing private artifact target with non-private mode %s: got %04o want 0600", displayPath, leaf.Mode().Perm())
	}
	file, err := root.Open(relativePath)
	if err != nil {
		return 0, true, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return 0, true, err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return 0, true, err
	}
	after, err := lstatRootedRegularPath(root, relativePath, displayPath, "private artifact")
	if err != nil {
		return 0, true, err
	}
	if !sameRootedPath(before, after) || !opened.Mode().IsRegular() || !os.SameFile(leaf, opened) {
		return 0, true, fmt.Errorf("private artifact file must be a stable regular file: %s", displayPath)
	}
	if n != expectedBytes || hex.EncodeToString(hash.Sum(nil)) != hexDigest {
		return 0, true, fmt.Errorf("refusing to overwrite non-identical private artifact target %s", displayPath)
	}
	if opened.Mode().Perm() != 0o600 {
		return 0, true, fmt.Errorf("refusing private artifact target with non-private mode %s: got %04o want 0600", displayPath, opened.Mode().Perm())
	}
	if after[len(after)-1].Mode().Perm() != 0o600 {
		return 0, true, fmt.Errorf("refusing private artifact target with non-private mode %s: got %04o want 0600", displayPath, after[len(after)-1].Mode().Perm())
	}
	return n, true, nil
}

func verifyFileDigest(path, hexDigest, ref string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil {
		return 0, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != hexDigest {
		return 0, fmt.Errorf("artifact %s content hash mismatch", ref)
	}
	return n, nil
}

func verifyExistingMaterializedTarget(targetPath, hexDigest, ref string, mode os.FileMode, expectedBytes int64) (int64, bool, error) {
	info, err := os.Lstat(targetPath)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, true, fmt.Errorf("refusing to overwrite non-regular target %s", targetPath)
	}
	n, err := verifyFileDigest(targetPath, hexDigest, ref)
	if err != nil || n != expectedBytes {
		return 0, true, fmt.Errorf("refusing to overwrite non-identical target %s", targetPath)
	}
	if info.Mode().Perm() != mode.Perm() {
		return 0, true, fmt.Errorf("refusing byte-identical target with non-identical mode %s: got %04o want %04o", targetPath, info.Mode().Perm(), mode.Perm())
	}
	return n, true, nil
}

func ArtifactHex(ref string) (string, error) {
	if !strings.HasPrefix(ref, ArtifactRefPrefix) {
		return "", fmt.Errorf("artifact ref must be %q-prefixed, got %q", ArtifactRefPrefix, ref)
	}
	hexDigest := strings.TrimPrefix(ref, ArtifactRefPrefix)
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("artifact ref must be %s<64 hex>, got %q", ArtifactRefPrefix, ref)
	}
	for _, r := range hexDigest {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return "", fmt.Errorf("artifact ref has a non-hex character: %q", ref)
		}
	}
	return hexDigest, nil
}
