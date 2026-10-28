package activity

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RetentionDays is the span/segment retention window. Files for a day older than this are swept.
// Note: home.Compact() provably never reaches observed/* children (habitat_catalog skips prefixes
// containing '/'), so this is a dedicated sweep, not Compact reuse.
const RetentionDays = 30

// SweepReport is the outcome of a retention sweep.
type SweepReport struct {
	FilesDeleted   int      `json:"files_deleted"`
	BytesReclaimed int64    `json:"bytes_reclaimed"`
	Deleted        []string `json:"deleted,omitempty"`
}

// Sweep deletes segment (.jsonl) and span (.json) files whose UTC day is older than RetentionDays
// before now, reporting bytes reclaimed. It is the remedy for the byte cap; drain never silently
// truncates. Re-running Sweep is idempotent (a second run finds nothing older and deletes nothing).
func Sweep(home string, now time.Time) (SweepReport, error) {
	var report SweepReport
	cutoff := now.UTC().AddDate(0, 0, -RetentionDays)
	for _, dir := range []string{SegmentsRoot(home), SpansRoot(home)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return SweepReport{}, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			day, ok := dayFromFilename(e.Name())
			if !ok {
				continue // never delete a file we cannot date — fail-safe
			}
			d, err := time.Parse("2006-01-02", day)
			if err != nil || !d.Before(cutoff) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			info, statErr := os.Stat(path)
			if statErr != nil {
				return SweepReport{}, statErr
			}
			if err := os.Remove(path); err != nil {
				return SweepReport{}, err
			}
			report.FilesDeleted++
			report.BytesReclaimed += info.Size()
			report.Deleted = append(report.Deleted, e.Name())
		}
	}
	return report, nil
}

// dayFromFilename extracts YYYY-MM-DD from "<day>.jsonl" or "<day>.json".
func dayFromFilename(name string) (string, bool) {
	base := name
	for _, ext := range []string{".jsonl", ".json"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			if len(base) == 10 {
				return base, true
			}
		}
	}
	return "", false
}
