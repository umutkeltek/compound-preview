package eventstore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"compound/internal/home"
)

// scanCursorVersion is bumped when the on-the-wire cursor shape changes so a
// stale cursor from an older layout decodes to the zero value and converges to a
// full rescan rather than skipping work.
const scanCursorVersion = 1

// scanCursor is the opaque, engine-independent resume token for an incremental
// Scan. It records a per-segment BYTE high-water mark (the end offset of the
// last complete frame the caller has already consumed), keyed by the segment
// path relative to the events dir. It is a CACHE, never authority: a lost,
// stale, or unparseable cursor decodes to the zero value and the next scan
// falls back to a fuller rescan — it can never produce a wrong (skipped) result.
//
// The map is bounded by families x day-segments (a handful), never by record
// count, so it stays small even as the store grows. It deliberately reuses the
// byte-offset cursor vocabulary already established by `compound stream`'s
// work.jsonl tail (stream.FollowOptions.Cursor), generalized from one file to
// the multi-segment layout.
type scanCursor struct {
	Version  int              `json:"v"`
	Segments map[string]int64 `json:"seg"`
}

func encodeScanCursor(c scanCursor) string {
	if c.Segments == nil {
		c.Segments = map[string]int64{}
	}
	c.Version = scanCursorVersion
	data, err := json.Marshal(c)
	if err != nil {
		// A cursor that cannot be encoded is simply not issued; the caller then
		// does a full rescan next run. Never fail the scan over a cache token.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// decodeScanCursor returns the zero cursor (and a full-rescan intent) for any
// empty, malformed, or version-mismatched token. ok reports whether a usable
// cursor was recovered; callers treat !ok as "start from the beginning".
func decodeScanCursor(raw string) (scanCursor, bool) {
	if raw == "" {
		return scanCursor{Segments: map[string]int64{}}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return scanCursor{Segments: map[string]int64{}}, false
	}
	var c scanCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return scanCursor{Segments: map[string]int64{}}, false
	}
	if c.Version != scanCursorVersion || c.Segments == nil {
		return scanCursor{Segments: map[string]int64{}}, false
	}
	return c, true
}

// scanFrom is the incremental sibling of scanAll. It returns only the records
// appended since cursorStr, the next cursor to persist, and whether this call
// actually resumed from a usable cursor on the segment-authoritative path
// (see the `resumed` return). Semantics:
//
//   - While the migration completion gate is OPEN (legacy fallback), the loose
//     layer is unioned and has no stable ordering a byte cursor can ride, so
//     scanFrom delegates to a full scanAll and issues an EMPTY cursor. Callers
//     converge to full-rescan correctness automatically on un-migrated homes.
//     resumed is always false here.
//   - Once segment-authoritative, each present segment is parsed only from its
//     recorded high-water to EOF; a fully-consumed sealed segment is skipped
//     without an open. New segments (rotation seq++, day-seal) are absent from
//     the cursor and parse in full exactly once. rm -rf index/ is irrelevant —
//     the cursor is byte offsets into segment truth, never index state.
//     resumed reports whether cursorStr itself decoded into a usable cursor
//     (decodeScanCursor ok) — an empty, garbage, or version-mismatched token
//     still runs this path (so a fresh cursor is issued) but resumed is false,
//     matching a full scan's semantics for anything layered on top (e.g. a
//     LastEventTime dedup filter that must stay authoritative on that call).
//
// restrictFamilies is the fail-safe-derived set of families opts.Prefixes
// could possibly match (scanFamilyRestriction, store.go), or nil for no
// restriction. It only decides which family directories this call walks; the
// per-segment byte-cursor map (next.Segments) it produces simply has no entry
// for a family it did not visit, and a later call reading that missing key
// falls back to a full reparse of that segment via the zero value (see
// alignedStart / the `start < 0 || start > size` reparse branch below) --
// the existing cache-not-authority law already makes any intent change (a
// different Prefixes/RegularOnly between calls) converge to a fuller scan,
// never a narrower one that could skip a record.
func (e *engine) scanFrom(regularOnly bool, cursorStr string, restrictFamilies map[string]bool) (map[string]storedRecord, []invalidLooseEntry, string, bool, error) {
	if e.legacyFallbackEnabled() {
		records, invalid, err := e.scanAll(regularOnly, 0, restrictFamilies)
		return records, invalid, "", false, err
	}

	prev, usable := decodeScanCursor(cursorStr)
	// resumed starts as "the token decoded" and is downgraded the moment any
	// segment degrades to a whole-file reparse: such a page can re-serve
	// already-consumed frames (a superset), and a caller that trusts
	// ScanPage.Resumed to skip its own dedup filter would fold them twice.
	resumed := usable
	next := scanCursor{Version: scanCursorVersion, Segments: map[string]int64{}}
	records := map[string]storedRecord{}

	familyEntries, err := os.ReadDir(e.segRoot)
	if errors.Is(err, os.ErrNotExist) {
		return records, nil, encodeScanCursor(next), resumed, nil
	}
	if err != nil {
		return nil, nil, "", false, err
	}
	families := make([]string, 0, len(familyEntries))
	for _, entry := range familyEntries {
		if !entry.IsDir() {
			continue
		}
		if restrictFamilies != nil && !restrictFamilies[entry.Name()] {
			continue
		}
		families = append(families, entry.Name())
	}
	sort.Strings(families)

	for _, family := range families {
		segments, err := e.familySegments(family)
		if err != nil {
			return nil, nil, "", false, err
		}
		for index, segment := range segments {
			rel, relErr := filepath.Rel(e.eventsDir, segment.path)
			if relErr != nil {
				rel = segment.path
			}
			frames, highWater, degraded, err := e.scanSegmentFrom(segment.path, prev.Segments[rel], index == 0, regularKindForScan(regularOnly))
			if err != nil {
				return nil, nil, "", false, err
			}
			if degraded {
				resumed = false
			}
			next.Segments[rel] = highWater
			// Newest-occurrence-wins within the returned delta, matching scanAll's
			// reverse walk. Records already below the high-water are intentionally
			// not returned — a monotonic fold has already consumed them.
			for i := len(frames) - 1; i >= 0; i-- {
				id := frames[i].Frame.ID
				if _, exists := records[id]; exists {
					continue
				}
				records[id] = storedRecord{
					id:             id,
					bytes:          append(append([]byte(nil), frames[i].Frame.Record...), '\n'),
					diagnosticPath: logicalReferencePrefix + id,
					fromSegment:    true,
				}
			}
		}
	}
	return records, nil, encodeScanCursor(next), resumed, nil
}

// scanSegmentFrom parses one segment starting at the caller's recorded byte
// high-water and returns the new frames plus the updated high-water (the end
// offset of the last COMPLETE frame — a tolerated torn tail on the hot segment
// is excluded so it is re-read once completed). Every reconciliation hazard —
// a non-boundary offset, a cursor past EOF, a shrunk file — degrades to a full
// reparse of that segment, never a skip; degraded reports exactly that case
// (the parse started before the caller's recorded offset), so scanFrom can
// withhold ScanPage.Resumed from a page that may re-serve consumed frames.
// regularKind, when set, keeps the rooted symlink/traversal guard by reading
// through home.ReadRootedRegularFile.
func (e *engine) scanSegmentFrom(path string, consumed int64, tolerateFinalTorn bool, regularKind string) (_ []parsedFrame, _ int64, degraded bool, _ error) {
	if regularKind != "" {
		data, err := home.ReadRootedRegularFile(e.home, path, regularKind+" segment")
		if err != nil {
			return nil, 0, false, err
		}
		start := alignedStart(data, consumed)
		degraded = start != consumed
		frames, highWater, err := parseSegmentSuffix(path, data, start, tolerateFinalTorn)
		if err != nil {
			return nil, 0, degraded, err
		}
		if start < int64(len(data)) {
			notifyScanParse(path, len(frames), int64(len(data))-start)
		}
		return frames, highWater, degraded, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	size := info.Size()
	start := consumed
	if start < 0 || start > size {
		start = 0 // shrunk/garbage cursor -> reparse the whole segment
	}
	if start > 0 && start < size {
		var one [1]byte
		if _, err := file.ReadAt(one[:], start-1); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, false, err
		} else if one[0] != '\n' {
			start = 0 // not a frame boundary -> reparse the whole segment
		}
	}
	degraded = start != consumed
	if start >= size {
		return nil, size, degraded, nil // nothing new since the cursor
	}
	buf := make([]byte, size-start)
	n, err := file.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, degraded, err
	}
	buf = buf[:n]
	frames, highWater, err := rebaseFrames(path, buf, start, tolerateFinalTorn)
	if err != nil {
		return nil, 0, degraded, err
	}
	notifyScanParse(path, len(frames), int64(len(buf)))
	return frames, highWater, degraded, nil
}

// alignedStart returns the byte offset to begin an in-memory suffix parse at.
// It falls back to 0 (full reparse) unless consumed points exactly at a frame
// boundary (the byte before it is a newline) within the segment.
func alignedStart(data []byte, consumed int64) int64 {
	if consumed <= 0 || consumed > int64(len(data)) {
		return 0
	}
	if consumed < int64(len(data)) && data[consumed-1] != '\n' {
		return 0
	}
	return consumed
}

// parseSegmentSuffix parses data[start:] and rebases frame offsets to absolute.
func parseSegmentSuffix(path string, data []byte, start int64, tolerateFinalTorn bool) ([]parsedFrame, int64, error) {
	if start >= int64(len(data)) {
		return nil, int64(len(data)), nil
	}
	return rebaseFrames(path, data[start:], start, tolerateFinalTorn)
}

// rebaseFrames parses a segment tail buffer whose first byte sits at absolute
// offset start, rebases every frame's Offset/End to absolute, and reports the
// new high-water as the end of the last complete frame (start when the tail
// holds none, e.g. a lone torn line).
func rebaseFrames(path string, buf []byte, start int64, tolerateFinalTorn bool) ([]parsedFrame, int64, error) {
	frames, err := parseSegmentBytes(path, buf, tolerateFinalTorn)
	if err != nil {
		return nil, 0, err
	}
	highWater := start
	for i := range frames {
		frames[i].Offset += start
		frames[i].End += start
		highWater = frames[i].End
	}
	return frames, highWater, nil
}
