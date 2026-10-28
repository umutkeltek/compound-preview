package home

import (
	"os"
	"path/filepath"
)

// Test-injection hooks for the two syncs below, so a test can fail them and prove the
// write aborts before publication — the same pattern as syncRootedDirectoryFile in
// rooted_file.go.
var (
	atomicWriteTempFileSync = func(file *os.File) error { return file.Sync() }
	atomicWriteDirSync      = syncDirectory
)

// WriteFileAtomic writes body to path via a same-directory temp file and a rename,
// so a concurrent reader observes either the previous complete body or the new
// complete body — never a partial one.
//
// WHY THIS IS NOT A STYLE PREFERENCE: derived projections under projections/ are
// read on the live orient path on every prompt, and their readers fail OPEN on a
// parse error. internal/contract's loadMemoryTemperatures returns a nil map when the
// body will not parse, and a nil map is treated as all-neutral — so a torn write does
// not surface as an error, it silently disables the signal the body carries. A plain
// os.WriteFile truncates in place, so any reader arriving mid-write sees exactly that
// partial body. The window is small, and it widens with every additional writer.
//
// rename(2) is atomic within a filesystem, which is why the temp file is created in
// the SAME directory as the target rather than in a temp dir: a cross-device rename
// degrades to copy-then-delete and loses the guarantee.
//
// Atomicity alone is not durability: the temp file is synced before the rename and the
// containing directory after it, so a successful return means the new body also survives
// a crash — a rename of unsynced bytes can resurrect the previous body (or an empty
// file) after a power loss while WriteFileAtomic reported success. This mirrors the
// artifact store's publication (Put in artifact_store.go).
func WriteFileAtomic(path string, body []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on every failure path: a leftover dotfile in projections/
	// would show up as an unknown entry in the habitat inventory.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	// Chmod before the rename so the file is never briefly visible at the target path
	// with temp-file permissions (CreateTemp makes 0600).
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	// Sync before the rename so the bytes published at the target path are durable,
	// not merely visible to this process.
	if err := atomicWriteTempFileSync(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // renamed away; nothing to clean up
	// Sync the containing directory so the name-to-inode rename itself is durable
	// across a crash.
	return atomicWriteDirSync(dir)
}
