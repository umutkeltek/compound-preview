package home

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// PrivateSpool is an unlinked retained descriptor. Under the private-root and
// process trust boundary, no caller or child receives a writable pathname;
// access is only through this descriptor or its one inherited duplicate.
type PrivateSpool struct {
	file     *os.File
	info     os.FileInfo
	consumed bool
	opened   bool
	removed  bool
}

func (s *ArtifactStore) CreatePrivateSpool() (*PrivateSpool, error) {
	if s == nil {
		return nil, fmt.Errorf("private spool requires artifact store")
	}
	if err := s.ensurePrivateArtifactRoot(); err != nil {
		return nil, err
	}
	root, err := openStableOwnedHomeRoot(s.home, privateCustodyHooks{})
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*PrivateSpool, error) { _ = root.Close(); return nil, e }
	parent := filepath.FromSlash("private/artifacts/sha256")
	if _, err := lstatRootedDirectoryPath(root, parent, filepath.Join(s.home, parent), "private spool root", false); err != nil {
		return fail(err)
	}
	for range 100 {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return fail(err)
		}
		rel := filepath.Join(parent, ".compound-private-spool-"+fmt.Sprintf("%x", nonce[:]))
		f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		info, err := f.Stat()
		if err == nil {
			err = validatePrivateSpoolLinked(info)
		}
		if err == nil {
			err = root.Remove(rel)
		}
		if err == nil {
			err = syncRootedDirectory(root, parent, filepath.Join(s.home, rel), "private spool")
		}
		if err != nil {
			_ = f.Close()
			_ = root.Remove(rel)
			return fail(err)
		}
		unlinked, err := f.Stat()
		if err != nil || !os.SameFile(info, unlinked) || unlinked.Mode().Perm() != 0o600 || !unlinked.Mode().IsRegular() {
			_ = f.Close()
			return fail(fmt.Errorf("private spool changed while unlinking"))
		}
		_ = root.Close()
		return &PrivateSpool{file: f, info: info}, nil
	}
	return fail(fmt.Errorf("private spool name collision limit exceeded"))
}

// OpenForChild returns the retained descriptor; exec.Cmd.ExtraFiles duplicates
// it for the child without re-opening a path.
func (s *PrivateSpool) OpenForChild() (*os.File, error) {
	if s.opened {
		return nil, fmt.Errorf("private spool child descriptor already opened")
	}
	if err := s.valid(); err != nil {
		return nil, err
	}
	s.opened = true
	return s.file, nil
}

func (s *PrivateSpool) Consume(max int64) ([]byte, error) {
	if max < 0 || max == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("private spool bound invalid")
	}
	if err := s.valid(); err != nil {
		return nil, err
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(s.file, max+1))
	if err != nil {
		return nil, err
	}
	after, err := s.file.Stat()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max || !os.SameFile(s.info, after) || after.Size() != int64(len(body)) || after.Mode().Perm() != 0o600 || !after.Mode().IsRegular() {
		return nil, fmt.Errorf("private spool changed while reading")
	}
	s.consumed = true
	return body, nil
}

// Remove is exact idempotent cleanup: its only mutation is closing the
// already-unlinked descriptor. Close errors therefore block certification.
func (s *PrivateSpool) Remove() error {
	if s == nil || s.file == nil {
		return fmt.Errorf("private spool unavailable")
	}
	if s.removed {
		return nil
	}
	if err := s.file.Close(); err != nil {
		return err
	}
	s.removed = true
	return nil
}
func (s *PrivateSpool) valid() error {
	if s == nil || s.file == nil || s.removed || s.consumed {
		return fmt.Errorf("private spool unavailable")
	}
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(s.info, info) || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		return fmt.Errorf("private spool identity changed")
	}
	return nil
}
func validatePrivateSpoolLinked(info os.FileInfo) error {
	if info == nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() || !singleLinkPrivateObject(info) {
		return fmt.Errorf("private spool must be single-link 0600 regular file")
	}
	return nil
}
