package home

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// The serializer consumes either rooted live entries or sealed private captures.
// All path selection, chunking, hashing, and publication remain in one writer.
type archiveStreamItem struct {
	rel   string
	info  fs.FileInfo
	write func(archiveEntryWriter) (ArchiveEntry, error)
	link  func() (archiveLink, error)
}
type archiveStreamSource struct {
	manifest   []byte
	homeSchema string
	walk       func(func(archiveStreamItem) error) error
	close      func() error
}

func openLiveArchiveStreamSource(homePath, schema string, snapshotEvents bool) (_ *archiveStreamSource, resultErr error) {
	if schema == ArchiveStreamSchemaV3 {
		if err := validateArchiveLinkSourceHome(homePath); err != nil {
			return nil, err
		}
	}
	root, err := openStableArchiveRoot(homePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			root.Close()
		}
	}()
	if err := archiveStreamRootOpened(homePath); err != nil {
		return nil, err
	}
	if err := validateStreamedSourceHealth(root); err != nil {
		return nil, err
	}

	pinnedManifestBytes, err := readRootedHomeManifestBounded(root)
	if err != nil {
		return nil, fmt.Errorf("pin home manifest for archive: %w", err)
	}
	pinnedManifest, err := decodeHomeManifest(pinnedManifestBytes)
	if err != nil {
		return nil, err
	}
	if err := archiveAfterHomeManifestPinned(filepath.Join(homePath, ManifestName)); err != nil {
		return nil, err
	}

	return &archiveStreamSource{
		manifest:   pinnedManifestBytes,
		homeSchema: pinnedManifest.SchemaVersion,
		close:      root.Close,
		walk: func(visit func(archiveStreamItem) error) error {
			return walkArchiveRootBatched(root, ".", func(rel string, info fs.FileInfo) error {
				return visit(archiveStreamItem{
					rel: rel, info: info,
					link: func() (archiveLink, error) { return captureArchiveLink(root, homePath, rel, info) },
					write: func(tw archiveEntryWriter) (ArchiveEntry, error) {
						if snapshotEvents && strings.HasPrefix(rel, "events/") {
							return writeRootedArchiveEventSnapshot(tw, root, rel, info)
						}
						return writeRootedArchiveFile(tw, root, rel, info)
					},
				})
			})
		},
	}, nil
}
