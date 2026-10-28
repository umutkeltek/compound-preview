package home

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ReadRootedRegularFileCanonicalOnly reads a regular file only when the
// canonical pathname is its sole removal-chain name. It is the admission read
// for mutations that must not adopt or resurrect cleanup residue. Recovery and
// read-only inspection should use ReadRootedRegularFileRemovalChain instead.
func ReadRootedRegularFileCanonicalOnly(homePath, path, kind string) ([]byte, bool, error) {
	return readRootedRegularFileCanonicalOnlyWithLimit(homePath, path, kind, 0)
}

func readRootedRegularFileCanonicalOnlyWithLimit(homePath, path, kind string, maxBytes int64) ([]byte, bool, error) {
	root, relativePath, displayPath, err := openRootedPath(homePath, path, kind)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()

	parentPath := filepath.Dir(relativePath)
	parents, err := lstatRootedDirectoryPath(root, parentPath, displayPath, kind, true)
	if err != nil {
		return nil, false, err
	}
	if parentPath != "." && len(parents) != len(strings.Split(parentPath, string(filepath.Separator))) {
		return nil, false, nil
	}

	parent, chain, present, err := inspectRootedRemovalChainWithLimit(root, relativePath, displayPath, kind, maxBytes)
	if err != nil {
		return nil, present, err
	}
	defer parent.Close()

	stagingLeaf := rootedRemovalEvidenceStagingLeaf(filepath.Base(relativePath))
	stagingPresent, err := rootedRemovalEvidenceStagingPresent(parent, stagingLeaf)
	if err != nil {
		return nil, present, fmt.Errorf("inspect %s removal recovery staging: %w", kind, err)
	}
	if stagingPresent {
		return nil, present, fmt.Errorf("%s removal recovery staging pending: %s", kind, displayPath)
	}
	if len(chain) == 0 {
		return nil, false, nil
	}
	if len(chain) != 1 || chain[0].depth != 0 {
		return nil, true, fmt.Errorf("%s removal cleanup is in flight: %s", kind, displayPath)
	}
	return append([]byte(nil), chain[0].data...), true, nil
}
