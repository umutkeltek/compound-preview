package home

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRootedNewFileDurableSyncsEachCreatedDirectoryParentBeforePublishing(t *testing.T) {
	fault := errors.New("injected created-directory parent sync failure")
	for _, failAt := range []string{".", "ledger"} {
		t.Run(failAt, func(t *testing.T) {
			homePath := t.TempDir()
			claimPath := filepath.Join(homePath, "ledger", "execution-route-request-claims", "claim.json")
			previous := syncRootedDirectoryFile
			defer func() { syncRootedDirectoryFile = previous }()
			syncRootedDirectoryFile = func(relativePath string, file *os.File) error {
				if relativePath == failAt {
					return fault
				}
				return file.Sync()
			}

			err := WriteRootedNewFileDurable(homePath, claimPath, "request claim", []byte("{}\n"), 0o644)
			if !errors.Is(err, fault) {
				t.Errorf("created-directory parent sync error = %v, want %v", err, fault)
			}
			if _, err := os.Stat(claimPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("claim file published after parent sync failure: %v", err)
			}
		})
	}
}
