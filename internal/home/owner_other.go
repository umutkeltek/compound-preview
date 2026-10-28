//go:build !unix

package home

import "os"

func fileOwnerUID(info os.FileInfo) (uint32, bool)           { return 0, false }
func ownedByCurrent(info os.FileInfo) bool                   { return false }
func singleLinkPrivateObject(info os.FileInfo) bool          { return false }
func privateObjectLinkCount(info os.FileInfo) (uint64, bool) { return 0, false }
