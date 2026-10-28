//go:build !unix

package home

import "os"

func openManifestNoFollow(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateManifestIdentity(path, before, before, before); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = f.Close()
		}
	}()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := manifestOpenAfterStatHook(path); err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateManifestIdentity(path, before, opened, after); err != nil {
		return nil, err
	}
	success = true
	return f, nil
}
