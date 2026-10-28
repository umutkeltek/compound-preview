package eventstore

import (
	"errors"
	"fmt"
	"strings"
)

const requireLocalEnv = "COMPOUND_EVENTSTORE_REQUIRE_LOCAL"

var ErrNonLocalFS = errors.New("event segment append requires a local filesystem")

type fsProbeFunc func(path string) (fsType string, local bool, err error)

func knownNonLocalFS(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{"nfs", "smb", "cifs", "afp", "webdav"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func nonLocalFSError(fsType string) error {
	return fmt.Errorf("%w: detected %s", ErrNonLocalFS, strings.TrimSpace(fsType))
}
