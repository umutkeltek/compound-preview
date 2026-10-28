package home

import (
	"archive/tar"
	"io/fs"
)

// Archive custody retains numeric ownership and file metadata without consulting
// a host account service, which may be unavailable or blocked by isolation.
type archiveNumericOwners struct{ fs.FileInfo }

func (archiveNumericOwners) Uname() (string, error) { return "", nil }
func (archiveNumericOwners) Gname() (string, error) { return "", nil }

func archiveFileInfoHeader(info fs.FileInfo, link string) (*tar.Header, error) {
	if info != nil {
		info = archiveNumericOwners{info}
	}
	return tar.FileInfoHeader(info, link)
}
