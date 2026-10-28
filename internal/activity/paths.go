package activity

import (
	"path/filepath"
)

// Storage layout under COMPOUND_HOME/observed/activity/ — day-rotated files, NEVER per-event files
// (the eventstore 53k-file lesson). Segments are validated raw events (append); spans are the
// deterministic reduction (atomic rewrite per drain); cursor.json makes re-drain idempotent.
const (
	activityDir  = "observed/activity"
	segmentsDir  = "segments"
	spansDir     = "spans"
	cursorFile   = "cursor.json"
	configDir    = "config"
	configFile   = "activity-sensor.json"
	actionLedger = "activity-sensor-actions.jsonl"
)

// ActivityRoot is <home>/observed/activity.
func ActivityRoot(home string) string { return filepath.Join(home, activityDir) }

// SegmentsRoot is <home>/observed/activity/segments.
func SegmentsRoot(home string) string { return filepath.Join(ActivityRoot(home), segmentsDir) }

// SpansRoot is <home>/observed/activity/spans.
func SpansRoot(home string) string { return filepath.Join(ActivityRoot(home), spansDir) }

// SegmentPath is the append-only validated-event file for a UTC day (YYYY-MM-DD).
func SegmentPath(home, day string) string {
	return filepath.Join(SegmentsRoot(home), day+".jsonl")
}

// SpanPath is the atomic-rewrite span reduction for a UTC day.
func SpanPath(home, day string) string {
	return filepath.Join(SpansRoot(home), day+".json")
}

// CursorPath records {spool file -> consumed byte offset, last seq} so re-drain is idempotent.
func CursorPath(home string) string {
	return filepath.Join(ActivityRoot(home), cursorFile)
}

// ConfigPath is <home>/config/activity-sensor.json — the governed enable/disable key store.
func ConfigPath(home string) string {
	return filepath.Join(home, configDir, configFile)
}

// ActionLedgerPath is the append-only governed-toggle action record ledger under the activity home.
func ActionLedgerPath(home string) string {
	return filepath.Join(ActivityRoot(home), actionLedger)
}
