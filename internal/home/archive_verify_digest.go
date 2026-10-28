package home

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
)

func verifiedArchiveSourceSHA256(file *os.File, before fs.FileInfo, path string) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	bytesRead, err := io.Copy(hash, newBoundedArchiveSource(file, defaultArchiveReadLimits.MaxSourceBytes))
	if err != nil {
		return "", err
	}
	end, err := file.Stat()
	if err != nil {
		return "", err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(before, end) || bytesRead != before.Size() || end.Size() != before.Size() || !end.ModTime().Equal(before.ModTime()) {
		return "", fmt.Errorf("verified archive source identity or bytes changed before checksum completion")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
