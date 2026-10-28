package work

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Hash the bounded real installed tree, not a caller-selected executable sample.
// Internal links are recorded, never followed; escaping links and nested source
// repositories cannot become package evidence. File reads use the rooted,
// race-checked readback already used by private-runtime delivery.
func installedPackageTree(path string) (string, int, int64, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer root.Close()
	h := sha256.New()
	count := 0
	var size int64
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > 200000 {
			return fmt.Errorf("package tree entry limit exceeded")
		}
		if entry.Name() == ".git" {
			return fmt.Errorf("package tree contains nested source repository")
		}
		info, e := root.Lstat(name)
		if e != nil {
			return e
		}
		fmt.Fprintf(h, "%d:%s:%o:", len(name), name, info.Mode())
		switch {
		case info.IsDir():
			fmt.Fprintln(h, "directory")
		case info.Mode().IsRegular():
			size += info.Size()
			if size > 2<<30 {
				return fmt.Errorf("package tree byte limit exceeded")
			}
			digest, e := privateDeliveryDigest(root, name)
			if e != nil {
				return e
			}
			fmt.Fprintf(h, "%d:%s\n", info.Size(), digest)
		case info.Mode()&os.ModeSymlink != 0:
			link, e := root.Readlink(name)
			if e != nil {
				return e
			}
			resolved, e := filepath.EvalSymlinks(filepath.Join(path, filepath.FromSlash(name)))
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(path, resolved)
			if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("package symlink escapes installed tree")
			}
			fmt.Fprintf(h, "%d:%s\n", len(link), link)
		default:
			return fmt.Errorf("package tree contains special file")
		}
		return nil
	})
	if err == nil {
		err = validatePrivateDeliveryRoot(root)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), count, size, err
}
