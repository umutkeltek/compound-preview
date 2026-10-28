//go:build darwin || linux

package candidates

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

type candidateHome struct {
	displayPath string
	fd          int
	openedFDs   []int
	identity    candidateFileIdentity
}

type candidateDirectory struct {
	displayPath string
	fd          int
	openedFDs   []int
	identity    candidateFileIdentity
}

type candidateInbox struct {
	home      *candidateHome
	directory *candidateDirectory
}

type candidateFileIdentity struct {
	dev         uint64
	ino         uint64
	mode        uint32
	size        int64
	mtimeSecond int64
	mtimeNano   int64
	ctimeSecond int64
	ctimeNano   int64
}

var candidateReadDirectoryNames = readCandidateDirectoryNames

// Test seam for filesystems whose stat timestamps do not advance on every write.
var candidateIdentitySnapshot = candidateIdentityFromStat

func openCandidateInbox(homePath, relativePath string, create bool) (*candidateInbox, error) {
	home, err := openCandidateHome(homePath)
	if err != nil {
		return nil, err
	}
	directory, err := home.openDirectory(relativePath, create)
	if err != nil {
		_ = home.close()
		return nil, err
	}
	return &candidateInbox{home: home, directory: directory}, nil
}

func (inbox *candidateInbox) close() error {
	if inbox == nil {
		return nil
	}
	return errors.Join(inbox.directory.close(), inbox.home.close())
}

func openCandidateHome(homePath string) (*candidateHome, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("candidate home path required")
	}
	displayPath, err := filepath.Abs(filepath.Clean(homePath))
	if err != nil {
		return nil, err
	}
	if displayPath == string(os.PathSeparator) {
		return nil, fmt.Errorf("candidate home must not be the filesystem root")
	}
	physicalPath, err := candidatePhysicalHomePath(displayPath)
	if err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(string(os.PathSeparator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root for candidate home: %w", err)
	}
	opened := []int{rootFD}
	current := rootFD
	walked := string(os.PathSeparator)
	for _, component := range candidatePathComponents(physicalPath) {
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		walked = filepath.Join(walked, component)
		if openErr != nil {
			_ = closeCandidateFDs(opened)
			return nil, fmt.Errorf("open candidate home without symlink traversal %s: %w", walked, openErr)
		}
		opened = append(opened, next)
		current = next
	}
	identity, err := candidateIdentityForFD(current)
	if err != nil {
		_ = closeCandidateFDs(opened)
		return nil, fmt.Errorf("identify candidate home: %w", err)
	}
	return &candidateHome{displayPath: displayPath, fd: current, openedFDs: opened, identity: identity}, nil
}

// candidatePhysicalHomePath resolves only the operating-system root alias.
// macOS exposes /var beneath /private/var; every project-controlled component
// after that alias remains O_NOFOLLOW.
func candidatePhysicalHomePath(absolutePath string) (string, error) {
	parts := candidatePathComponents(absolutePath)
	if len(parts) == 0 {
		return absolutePath, nil
	}
	first := filepath.Join(string(os.PathSeparator), parts[0])
	info, err := os.Lstat(first)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return absolutePath, nil
	}
	if runtime.GOOS != "darwin" || first != "/var" {
		return "", fmt.Errorf("candidate home must not traverse symlinked root component %s", first)
	}
	resolved, err := filepath.EvalSymlinks(first)
	if err != nil {
		return "", fmt.Errorf("resolve operating-system root alias %s: %w", first, err)
	}
	if resolved != "/private/var" {
		return "", fmt.Errorf("candidate home root alias %s resolves to unsupported target %s", first, resolved)
	}
	if len(parts) == 1 {
		return resolved, nil
	}
	return filepath.Join(resolved, filepath.Join(parts[1:]...)), nil
}

func candidatePathComponents(path string) []string {
	relative := strings.TrimPrefix(filepath.Clean(path), string(os.PathSeparator))
	if relative == "" || relative == "." {
		return nil
	}
	return strings.Split(relative, string(os.PathSeparator))
}

func (home *candidateHome) openDirectory(relativePath string, create bool) (*candidateDirectory, error) {
	components := candidatePathComponents(relativePath)
	if len(components) == 0 || filepath.IsAbs(relativePath) {
		return nil, fmt.Errorf("candidate custody directory must be a non-empty relative path: %q", relativePath)
	}
	current := home.fd
	opened := make([]int, 0, len(components))
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			_ = closeCandidateFDs(opened)
			return nil, fmt.Errorf("candidate custody directory has unsafe component %q", component)
		}
		if create {
			mkdirErr := unix.Mkdirat(current, component, 0o755)
			if mkdirErr == nil {
				if syncErr := unix.Fsync(current); syncErr != nil {
					_ = closeCandidateFDs(opened)
					return nil, fmt.Errorf("sync candidate custody parent after mkdir: %w", syncErr)
				}
			} else if !errors.Is(mkdirErr, fs.ErrExist) {
				_ = closeCandidateFDs(opened)
				return nil, fmt.Errorf("create candidate custody directory %s: %w", filepath.Join(components[:index+1]...), mkdirErr)
			}
		}
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if err != nil {
			_ = closeCandidateFDs(opened)
			path := filepath.Join(home.displayPath, filepath.Join(components[:index+1]...))
			if errors.Is(err, fs.ErrNotExist) {
				return nil, &os.PathError{
					Op:   "open candidate custody directory without symlink traversal",
					Path: path,
					Err:  err,
				}
			}
			return nil, fmt.Errorf("open candidate custody directory without symlink traversal %s: %w", path, err)
		}
		opened = append(opened, next)
		current = next
	}
	identity, err := candidateIdentityForFD(current)
	if err != nil {
		_ = closeCandidateFDs(opened)
		return nil, fmt.Errorf("identify candidate custody directory: %w", err)
	}
	return &candidateDirectory{displayPath: filepath.Join(home.displayPath, relativePath), fd: current, openedFDs: opened, identity: identity}, nil
}

func (home *candidateHome) close() error {
	if home == nil {
		return nil
	}
	err := closeCandidateFDs(home.openedFDs)
	home.openedFDs = nil
	return err
}

func (directory *candidateDirectory) close() error {
	if directory == nil {
		return nil
	}
	err := closeCandidateFDs(directory.openedFDs)
	directory.openedFDs = nil
	return err
}

func closeCandidateFDs(fds []int) error {
	var result error
	for index := len(fds) - 1; index >= 0; index-- {
		if err := unix.Close(fds[index]); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func readCandidateDirectoryNames(directory *candidateDirectory) ([]string, error) {
	fd, err := unix.Openat(directory.fd, ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), directory.displayPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open candidate directory descriptor %s", directory.displayPath)
	}
	entries, readErr := file.ReadDir(-1)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func (directory *candidateDirectory) path(name string) string {
	return filepath.Join(directory.displayPath, name)
}

func (directory *candidateDirectory) lstat(name string) (candidateFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(directory.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return candidateFileIdentity{}, err
	}
	return candidateIdentitySnapshot(&stat), nil
}

func candidateIdentityForFD(fd int) (candidateFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return candidateFileIdentity{}, err
	}
	return candidateIdentitySnapshot(&stat), nil
}

func candidateIdentityForFile(file *os.File) (candidateFileIdentity, error) {
	return candidateIdentityForFD(int(file.Fd()))
}

func (identity candidateFileIdentity) isSymlink() bool {
	return identity.mode&uint32(unix.S_IFMT) == uint32(unix.S_IFLNK)
}

func (identity candidateFileIdentity) isRegular() bool {
	return identity.mode&uint32(unix.S_IFMT) == uint32(unix.S_IFREG)
}

func (identity candidateFileIdentity) same(other candidateFileIdentity) bool {
	return identity.sameObject(other) &&
		identity.size == other.size &&
		identity.mtimeSecond == other.mtimeSecond &&
		identity.mtimeNano == other.mtimeNano &&
		identity.ctimeSecond == other.ctimeSecond &&
		identity.ctimeNano == other.ctimeNano
}

func (identity candidateFileIdentity) sameObject(other candidateFileIdentity) bool {
	return identity.dev == other.dev && identity.ino == other.ino && identity.mode == other.mode
}

func (identity candidateFileIdentity) detail() string {
	return fmt.Sprintf(
		"mode=%#o dev=%d ino=%d size=%d mtime=%d.%09d ctime=%d.%09d",
		identity.mode,
		identity.dev,
		identity.ino,
		identity.size,
		identity.mtimeSecond,
		identity.mtimeNano,
		identity.ctimeSecond,
		identity.ctimeNano,
	)
}

func (identity candidateFileIdentity) tokenField() string {
	return fmt.Sprintf(
		"%d:%d:%d:%d:%d:%d:%d:%d",
		identity.dev,
		identity.ino,
		identity.mode,
		identity.size,
		identity.mtimeSecond,
		identity.mtimeNano,
		identity.ctimeSecond,
		identity.ctimeNano,
	)
}

func (directory *candidateDirectory) openRead(name string) (*os.File, error) {
	fd, err := unix.Openat(directory.fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), directory.path(name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open candidate file descriptor %s", directory.path(name))
	}
	return file, nil
}

func (directory *candidateDirectory) openPublicationLock() (*os.File, error) {
	const name = "candidates.lock"
	fd := -1
	var nameIdentity candidateFileIdentity
	haveNameIdentity := false
	for attempt := 0; attempt < 100; attempt++ {
		created, err := unix.Openat(directory.fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o644)
		switch {
		case err == nil:
			fd = created
			if err := directory.sync(); err != nil {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("sync candidate lock directory after create: %w", err)
			}
		case errors.Is(err, fs.ErrExist):
			identity, statErr := directory.lstat(name)
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			if statErr != nil {
				return nil, fmt.Errorf("inspect candidate publication lock: %w", statErr)
			}
			if !identity.isRegular() {
				return nil, fmt.Errorf("candidate publication lock is not a regular file (%s)", identity.detail())
			}
			nameIdentity = identity
			haveNameIdentity = true
			opened, openErr := unix.Openat(directory.fd, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if errors.Is(openErr, fs.ErrNotExist) {
				continue
			}
			if openErr != nil {
				return nil, fmt.Errorf("open candidate publication lock: %w", openErr)
			}
			fd = opened
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			return nil, fmt.Errorf("create candidate publication lock: %w", err)
		}
		break
	}
	if fd < 0 {
		return nil, fmt.Errorf("open candidate publication lock: create-or-open retry budget exhausted")
	}
	identity, err := candidateIdentityForFD(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("identify candidate publication lock: %w", err)
	}
	if !identity.isRegular() {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("candidate publication lock is not a regular file (%s)", identity.detail())
	}
	if haveNameIdentity && !nameIdentity.sameObject(identity) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("candidate publication lock changed while opening")
	}
	current, err := directory.lstat(name)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("candidate publication lock name changed while opening: %w", err)
	}
	if !current.sameObject(identity) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("candidate publication lock name changed while opening")
	}
	file := os.NewFile(uintptr(fd), directory.path(name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open candidate publication lock descriptor")
	}
	return file, nil
}

func (directory *candidateDirectory) createTempFile() (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, fmt.Errorf("generate candidate temp name: %w", err)
		}
		name := ".candidate-publish-" + hex.EncodeToString(random[:])
		fd, err := unix.Openat(directory.fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("create candidate temp: %w", err)
		}
		file := os.NewFile(uintptr(fd), directory.path(name))
		if file == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(directory.fd, name, 0)
			return "", nil, fmt.Errorf("open candidate temp descriptor")
		}
		return name, file, nil
	}
	return "", nil, fmt.Errorf("create candidate temp: name collision budget exhausted")
}

func (directory *candidateDirectory) openExistingWritable(name string) (*os.File, error) {
	fd, err := unix.Openat(directory.fd, name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), directory.path(name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open candidate replacement descriptor %s", directory.path(name))
	}
	return file, nil
}

func (directory *candidateDirectory) link(source, target string) error {
	return unix.Linkat(directory.fd, source, directory.fd, target, 0)
}

func (directory *candidateDirectory) rename(source, target string) error {
	return unix.Renameat(directory.fd, source, directory.fd, target)
}

func (directory *candidateDirectory) remove(name string) error {
	return unix.Unlinkat(directory.fd, name, 0)
}

func (directory *candidateDirectory) sync() error {
	return unix.Fsync(directory.fd)
}
