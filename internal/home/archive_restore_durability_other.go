//go:build !darwin

package home

import "os"

func syncStreamedRestoreFile(file *os.File) error                                   { return file.Sync() }
func requireStreamedRestoreDevice(_, _ os.FileInfo) error                           { return nil }
func syncStreamedRestoreDirectory(*os.Root, string, os.FileInfo, os.FileInfo) error { return nil }
func syncStreamedRestoreBarrier(*archiveRestoreTarget) error                        { return nil }
func syncStreamedRestoreParent(*os.File, os.FileInfo) error                         { return nil }
