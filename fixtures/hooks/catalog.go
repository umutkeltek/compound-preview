// Package hookfixtures owns the synthetic lifecycle fixtures compiled into the
// Compound CLI. Installed hook readiness and fixture-mode execution therefore
// refer to the same bytes without depending on the caller's working directory.
package hookfixtures

import (
	"embed"
	"io/fs"
	"path"
	"strings"
)

const referencePrefix = "fixtures/hooks/"

//go:embed *.json
var catalog embed.FS

// ReadReference returns a built-in fixture for its public repository reference.
func ReadReference(ref string) ([]byte, error) {
	name, ok := referenceName(ref)
	if !ok {
		return nil, fs.ErrNotExist
	}
	return catalog.ReadFile(name)
}

// HasReference reports whether ref names a fixture compiled into this binary.
func HasReference(ref string) bool {
	name, ok := referenceName(ref)
	if !ok {
		return false
	}
	info, err := fs.Stat(catalog, name)
	return err == nil && !info.IsDir()
}

func referenceName(ref string) (string, bool) {
	trimmed := strings.TrimSpace(ref)
	if !strings.HasPrefix(trimmed, referencePrefix) {
		return "", false
	}
	name := strings.TrimPrefix(trimmed, referencePrefix)
	if name == "" || path.Clean(name) != name || path.Base(name) != name {
		return "", false
	}
	return name, true
}
