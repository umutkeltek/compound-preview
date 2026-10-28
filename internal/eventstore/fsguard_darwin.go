//go:build darwin

package eventstore

import (
	"strings"
	"syscall"
)

func checkLocalFS(path string) (string, bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", false, err
	}
	name := make([]byte, 0, len(stat.Fstypename))
	for _, char := range stat.Fstypename {
		if char == 0 {
			break
		}
		name = append(name, byte(char))
	}
	fsType := strings.TrimSpace(string(name))
	return fsType, !knownNonLocalFS(fsType), nil
}
