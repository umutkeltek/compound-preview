//go:build !darwin

package home

import (
	"fmt"
	"io/fs"
	"os"
)

func requireArchiveEventSnapshotSupport() error {
	return fmt.Errorf("streamed-v3-event-snapshot requires native immutable event clones on a supported filesystem")
}

func writeRootedArchiveEventSnapshot(_ archiveEntryWriter, _ *os.Root, _ string, _ fs.FileInfo) (ArchiveEntry, error) {
	return ArchiveEntry{}, requireArchiveEventSnapshotSupport()
}
