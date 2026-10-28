//go:build linux

package eventstore

import (
	"fmt"
	"syscall"
)

const (
	linuxNFSMagic  = uint64(0x6969)
	linuxSMBMagic  = uint64(0x517B)
	linuxCIFSMagic = uint64(0xFF534D42)
	linuxSMB2Magic = uint64(0xFE534D42)
)

func checkLocalFS(path string) (string, bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", false, err
	}
	magic := uint64(stat.Type)
	switch magic {
	case linuxNFSMagic:
		return "nfs", false, nil
	case linuxSMBMagic:
		return "smb", false, nil
	case linuxCIFSMagic:
		return "cifs", false, nil
	case linuxSMB2Magic:
		return "smb2", false, nil
	default:
		return fmt.Sprintf("0x%x", magic), true, nil
	}
}
