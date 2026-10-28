//go:build linux

package home

import "golang.org/x/sys/unix"

func exchangeRootedCASAt(parentFD int, leftName, rightName string) error {
	return unix.Renameat2(parentFD, leftName, parentFD, rightName, unix.RENAME_EXCHANGE)
}
