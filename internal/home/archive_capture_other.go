//go:build !darwin

package home

import "time"

func backupCapturedStreamedArchive(_, _ string, _ time.Time, _ string) (ArchiveReport, error) {
	return ArchiveReport{}, requireArchiveEventSnapshotSupport()
}
