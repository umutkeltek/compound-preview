package activity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/policy"
)

// ByteCapBytes is the hard recursive size cap for observed/activity/. At or above it, drain REFUSES
// the whole run (no partial write); `activity sweep` is the remedy, drain never silently truncates.
const ByteCapBytes = 256 * 1024 * 1024 // 256 MiB

// CursorSchemaVersion versions the idempotency cursor.
const CursorSchemaVersion = "compound.activity_cursor.v0.1"

// FileCursor records how far a spool file has been consumed. Offset is the byte position after the
// last COMPLETE line ingested; LastSeq is the highest event seq seen from that file.
type FileCursor struct {
	Offset  int64 `json:"offset"`
	LastSeq int64 `json:"last_seq"`
}

// Cursor makes re-drain idempotent: same spool input -> same spans, no duplicate segment lines.
type Cursor struct {
	SchemaVersion string                `json:"schema_version"`
	Files         map[string]FileCursor `json:"files"`
}

// DrainReport is the honest outcome of a drain. Refused=true with a Reason means ZERO rows were
// written (fail-closed); the CLI maps it to a non-zero exit. RefusedLines counts strict-decode
// rejections (unknown field/kind/forbidden meta) — an outage or bad line never reads as clean input.
type DrainReport struct {
	Refused           bool     `json:"refused"`
	Reason            string   `json:"reason,omitempty"`
	EventsIngested    int      `json:"events_ingested"`
	SegmentLinesAdded int      `json:"segment_lines_added"`
	RefusedLines      int      `json:"refused_lines"`
	DaysWritten       []string `json:"days_written"`
	BytesUnderRoot    int64    `json:"bytes_under_root"`
}

// Drain is the membrane: the sole reader of the collector's NDJSON spool. It is fail-closed on both
// gates (decision 9): (a) the config key must be enabled, AND (b) a live allowing PolicyDecision for
// DrainCapability must exist. Missing/expired grant, disabled key, or a non-allow outcome writes ZERO
// rows and returns Refused with an honest reason (F2, F3). It refuses at the byte cap with no partial
// write (F7). Ingestion is cursor-bounded, so a re-drain over the same spool is idempotent (F12).
func Drain(home, spoolDir string, cfg policy.Config, workID string, now time.Time) (DrainReport, error) {
	// Gate 1: config enabled.
	enabled, err := Enabled(home)
	if err != nil {
		return DrainReport{}, err
	}
	if !enabled {
		return DrainReport{Refused: true, Reason: "activity sensor disabled (config key enabled!=true)"}, nil
	}
	// Gate 2: live allowing PolicyDecision for the drain capability (fail-closed on missing/expired).
	ai := buildDrainIntent(workID, home, now)
	pd := policy.Evaluate(ai, cfg, now)
	if gateErr := act.Gate(ai, pd); gateErr != nil {
		return DrainReport{Refused: true, Reason: "drain not granted: " + gateErr.Error()}, nil
	}

	// Byte cap: refuse the WHOLE run before any write (no partial write).
	size, err := dirSize(ActivityRoot(home))
	if err != nil {
		return DrainReport{}, err
	}
	if size >= ByteCapBytes {
		return DrainReport{Refused: true, Reason: "byte_cap_exceeded", BytesUnderRoot: size}, nil
	}

	cursor, err := loadCursor(home)
	if err != nil {
		return DrainReport{}, err
	}

	spoolFiles, err := listSpool(spoolDir)
	if err != nil {
		return DrainReport{}, err
	}

	report := DrainReport{DaysWritten: []string{}}
	touchedDays := map[string]bool{}
	for _, sf := range spoolFiles {
		info, statErr := os.Stat(sf)
		if statErr != nil {
			return DrainReport{}, statErr
		}
		key := filepath.Base(sf)
		fc := cursor.Files[key]
		if fc.Offset >= info.Size() {
			continue // already consumed to EOF — idempotent no-op
		}
		newBytes, err := readRange(sf, fc.Offset, info.Size())
		if err != nil {
			return DrainReport{}, err
		}
		// Only consume up to the last COMPLETE line, so a spool file mid-write never has a partial
		// line ingested. Bytes after the final newline wait for the next drain.
		lastNL := bytes.LastIndexByte(newBytes, '\n')
		if lastNL < 0 {
			continue // no complete line yet
		}
		complete := newBytes[:lastNL+1]
		dec := DecodeStrict(complete, 1)
		report.RefusedLines += len(dec.Refused)

		// Group accepted events by their UTC day and append to that day's segment.
		byDay := map[string][]Event{}
		for _, ev := range dec.Accepted {
			day := utcDay(ev.TS)
			byDay[day] = append(byDay[day], ev)
			if ev.Seq > fc.LastSeq {
				fc.LastSeq = ev.Seq
			}
		}
		for day, evs := range byDay {
			added, err := appendSegment(home, day, evs)
			if err != nil {
				return DrainReport{}, err
			}
			report.SegmentLinesAdded += added
			report.EventsIngested += added
			touchedDays[day] = true
		}
		fc.Offset += int64(lastNL + 1)
		cursor.Files[key] = fc
	}

	// Re-segment every touched day from its full segment file and atomically rewrite its spans.
	for day := range touchedDays {
		if err := rewriteSpans(home, day, now); err != nil {
			return DrainReport{}, err
		}
		report.DaysWritten = append(report.DaysWritten, day)
	}
	sort.Strings(report.DaysWritten)

	if err := saveCursor(home, cursor); err != nil {
		return DrainReport{}, err
	}
	report.BytesUnderRoot, _ = dirSize(ActivityRoot(home))
	return report, nil
}

func buildDrainIntent(workID, home string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actID("act_", workID, DrainCapability, ActivityRoot(home)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "activity",
		Capability:    DrainCapability,
		Action:        "drain_activity_spool",
		Target:        envelope.ActionTarget{Kind: "evidence_dir", ID: "observed/activity", Path: ActivityRoot(home)},
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "ingest validated activity events into observed/activity segments+spans",
		RollbackPlan:  "delete segment/span files written by this drain",
	}
}

// appendSegment appends canonical, validated events to <day>.jsonl. It writes compound's OWN
// canonical marshaling (never the raw spool bytes), so a rejected line can never reach a segment and
// the segment stays content-free. Returns the number of lines appended.
func appendSegment(home, day string, events []Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	path := SegmentPath(home, day)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	for _, ev := range events {
		line, err := json.Marshal(&ev)
		if err != nil {
			return 0, err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return 0, err
		}
	}
	return len(events), f.Sync()
}

// rewriteSpans re-reads a day's full segment file, re-segments it deterministically, and atomically
// rewrites <day>.json. Deterministic input -> byte-identical output (F12).
func rewriteSpans(home, day string, now time.Time) error {
	segPath := SegmentPath(home, day)
	data, err := os.ReadFile(segPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dec := DecodeStrict(data, 1) // segment lines are already validated; decode defensively
	spans := Segment(dec.Accepted)
	spanFile := SpanFile{
		SchemaVersion: SpansSchemaVersion,
		Date:          day,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Spans:         spans,
	}
	body, err := json.MarshalIndent(&spanFile, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(SpanPath(home, day), body)
}

// --- cursor + helpers ---

func loadCursor(home string) (Cursor, error) {
	data, err := os.ReadFile(CursorPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return Cursor{SchemaVersion: CursorSchemaVersion, Files: map[string]FileCursor{}}, nil
		}
		return Cursor{}, err
	}
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return Cursor{}, fmt.Errorf("activity cursor parse: %w", err)
	}
	if c.Files == nil {
		c.Files = map[string]FileCursor{}
	}
	c.SchemaVersion = CursorSchemaVersion
	return c, nil
}

func saveCursor(home string, c Cursor) error {
	if err := os.MkdirAll(filepath.Dir(CursorPath(home)), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(CursorPath(home), body)
}

func listSpool(spoolDir string) ([]string, error) {
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ndjson") {
			continue
		}
		out = append(out, filepath.Join(spoolDir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

func readRange(path string, from, to int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, to-from)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

// utcDay returns the YYYY-MM-DD UTC day of an RFC3339 timestamp (falls back to the raw prefix).
func utcDay(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if len(ts) >= 10 {
			return ts[:10]
		}
		return ts
	}
	return t.UTC().Format("2006-01-02")
}

func atomicWrite(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dirSize sums the recursive byte size under root (0 if absent).
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}
