//go:build darwin

package home

import "golang.org/x/sys/unix"

func exchangeRootedCASAt(parentFD int, leftName, rightName string) error {
	return unix.RenameatxNp(parentFD, leftName, parentFD, rightName, unix.RENAME_SWAP)
}
