package eventstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/home"
)

type storedRecord struct {
	id             string
	bytes          []byte
	diagnosticPath string
	fromSegment    bool
}

// segmentHit is a resolved segment record: the authoritative frame plus the
// physical coordinate the migration seam records in a manifest row. offset is
// only populated when the record is resolved by a full segment parse (hot
// segments and presence scans); an index-resolved sealed hit leaves it zero
// because the completion gate keys record identity on canonical hash, not offset.
type segmentHit struct {
	frame   Frame
	relPath string
	offset  int64
}

// segmentFrame resolves an id against segment truth ONLY (never the loose layer
// or the archive). A corrupt segment surfaces its error here so every read path
// fails closed rather than silently falling back to another representation.
func (e *engine) segmentFrame(id, regularKind string) (segmentHit, bool, error) {
	family := familyOf(nil, id)
	segments, err := e.familySegments(family)
	if err != nil {
		return segmentHit{}, false, err
	}
	for index, segment := range segments {
		rel, relErr := filepath.Rel(e.eventsDir, segment.path)
		if relErr != nil {
			rel = segment.path
		}
		if index > 0 {
			frame, found, err := e.readIndexedFrame(family, segment, id, regularKind)
			if err != nil {
				return segmentHit{}, false, err
			}
			if found {
				return segmentHit{frame: frame, relPath: rel}, true, nil
			}
			continue
		}

		// Hot (last-written) segment: resolve through the per-process memo so a
		// projection's thousands of point reads cost one parse, not one parse
		// each (the post-migration quadratic — see hotcache.go). regularKind
		// reads keep the direct rooted-validation parse.
		if regularKind == "" {
			notifyHotSegmentAcquire(segment.path)
			byID, err := e.hotFrames(segment.path)
			if err != nil {
				return segmentHit{}, false, err
			}
			if hit, ok := byID[id]; ok {
				return segmentHit{frame: hit.Frame, relPath: rel, offset: hit.Offset}, true, nil
			}
			continue
		}
		notifyHotSegmentAcquire(segment.path)
		notifyHotParse(segment.path)
		frames, err := e.readSegment(segment.path, true, regularKind)
		if err != nil {
			return segmentHit{}, false, err
		}
		for i := len(frames) - 1; i >= 0; i-- {
			if frames[i].Frame.ID == id {
				return segmentHit{frame: frames[i].Frame, relPath: rel, offset: frames[i].Offset}, true, nil
			}
		}
	}
	return segmentHit{}, false, nil
}

func (e *engine) getByID(id, regularKind string) (storedRecord, error) {
	hit, found, err := e.segmentFrame(id, regularKind)
	if err != nil {
		return storedRecord{}, err
	}
	if found {
		return storedRecord{
			id:             id,
			bytes:          append(append([]byte(nil), hit.frame.Record...), '\n'),
			diagnosticPath: logicalReferencePrefix + id,
			fromSegment:    true,
		}, nil
	}
	// After the migration completion gate closes, the loose layer is dropped from
	// the hot read path: a migrated id resolves in a segment above, and anything
	// not in a segment is genuinely absent (fail closed, never the archive copy).
	if !e.legacyFallbackEnabled() {
		return storedRecord{}, fmt.Errorf("event %s not found in segments (migration complete): %w", id, os.ErrNotExist)
	}
	legacyPath := filepath.Join(e.eventsDir, id+".json")
	var data []byte
	if regularKind == "" {
		data, err = os.ReadFile(legacyPath)
	} else {
		data, err = home.ReadRootedRegularFile(e.home, legacyPath, regularKind)
	}
	if err != nil {
		return storedRecord{}, err
	}
	return storedRecord{id: id, bytes: data, diagnosticPath: legacyPath}, nil
}

func (e *engine) readSegment(path string, tolerateFinalTorn bool, regularKind string) ([]parsedFrame, error) {
	if regularKind == "" {
		return parseSegment(path, tolerateFinalTorn)
	}
	data, err := home.ReadRootedRegularFile(e.home, path, regularKind+" segment")
	if err != nil {
		return nil, err
	}
	return parseSegmentBytes(path, data, tolerateFinalTorn)
}

// scanSegmentObserver is a test-only seam fired once per segment ACTUALLY
// parsed during a Scan/ScanFrom/EnumerateIDs, reporting the frame count and
// byte count decoded. It is nil in production and never surfaces in the
// public API. A falsifier installs it to prove an incremental ScanFrom
// decodes ONLY frames appended since its cursor while a full Scan re-decodes
// everything (counter-seam precedent: segmentParseObserver in hotcache.go),
// and that warm prefix enumeration skips segments whose complete inventory
// has no matching keys. A skipped fully-consumed segment never fires it — 0 work is 0 events.
var scanSegmentObserver func(path string, frames int, bytes int64)

func notifyScanParse(path string, frames int, bytes int64) {
	if scanSegmentObserver != nil {
		scanSegmentObserver(path, frames, bytes)
	}
}

// invalidLooseEntry is a loose events/ file whose name is not a valid record
// reference. scanAll reports these instead of skipping them: whether an
// invalid name is ignorable depends on the caller's prefix filter (Scan fails
// closed when one MATCHES the requested prefixes), and the engine does not
// know the prefixes.
type invalidLooseEntry struct {
	name string
	err  error
}

// scanAll walks every segment family directory and unions legacy loose
// records. restrictFamilies, when non-nil, is the fail-safe-derived set of
// families opts.Prefixes could possibly match (scanFamilyRestriction,
// store.go); families outside that set are skipped without a readdir or
// parse. nil means no restriction -- walk every family, the same behavior as
// before routing existed.
func (e *engine) scanAll(regularOnly bool, recentSegments int, restrictFamilies map[string]bool) (map[string]storedRecord, []invalidLooseEntry, error) {
	records := map[string]storedRecord{}
	var invalid []invalidLooseEntry

	if familyEntries, err := os.ReadDir(e.segRoot); err == nil {
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
				return nil, nil, err
			}
			if recentSegments > 0 && len(segments) > recentSegments {
				segments = segments[:recentSegments]
			}
			for index, segment := range segments {
				frames, err := e.readSegment(segment.path, index == 0, regularKindForScan(regularOnly))
				if err != nil {
					return nil, nil, err
				}
				notifyScanParse(segment.path, len(frames), 0)
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
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	// Loose records are unioned only while the migration completion gate is open.
	// Once completion.json exists the scan is segment-authoritative: the loose
	// readdir is skipped entirely (no O(records) directory of point files to walk),
	// and a post-completion loose file is a legacy-writer regression for the
	// doctor, not scan authority. A missing events dir before completion still
	// surfaces os.ErrNotExist here, preserving the read-only Open contract.
	if e.legacyFallbackEnabled() {
		dirEntries, err := os.ReadDir(e.eventsDir)
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range dirEntries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			if regularOnly && !entry.Type().IsRegular() {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			if _, exists := records[id]; exists {
				continue
			}
			if _, err := ReferenceForID(id); err != nil {
				invalid = append(invalid, invalidLooseEntry{name: name, err: err})
				continue
			}
			records[id] = storedRecord{id: id, diagnosticPath: filepath.Join(e.eventsDir, name)}
		}
	}
	return records, invalid, nil
}

// scanFingerprint computes a cheap identity token over exactly the content scanAll
// would read under the same options: the restricted families' segment files plus —
// only while the migration completion gate is open — the loose record layer. It
// stats files (path:size:mtime) instead of parsing them, so a caller can invalidate
// a read-side cache without re-scanning. Segments are append-only and immutable once
// sealed, so an identical token means Scan would return identical entries, while any
// append/seal/rotate or loose-record change shifts it. It never reads record bytes,
// keeping the events physical layout owned solely by this package. The token format
// is opaque: callers compare tokens, never parse them.
func (e *engine) scanFingerprint(regularOnly bool, restrictFamilies map[string]bool) string {
	var b strings.Builder
	if familyEntries, err := os.ReadDir(e.segRoot); err == nil {
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
				fmt.Fprintf(&b, "seg|%s|<err>;", family)
				continue
			}
			for _, segment := range segments {
				info, statErr := os.Stat(segment.path)
				if statErr != nil {
					fmt.Fprintf(&b, "seg|%s|<absent>;", filepath.Base(segment.path))
					continue
				}
				fmt.Fprintf(&b, "seg|%s:%d:%d;", segment.path, info.Size(), info.ModTime().UnixNano())
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(&b, "segroot|<err>;%v;", err)
	}
	if e.legacyFallbackEnabled() {
		if dirEntries, err := os.ReadDir(e.eventsDir); err == nil {
			for _, entry := range dirEntries {
				name := entry.Name()
				if entry.IsDir() || !strings.HasSuffix(name, ".json") {
					continue
				}
				if regularOnly && !entry.Type().IsRegular() {
					continue
				}
				info, infoErr := entry.Info()
				if infoErr != nil {
					continue
				}
				fmt.Fprintf(&b, "loose|%s:%d:%d;", name, info.Size(), info.ModTime().UnixNano())
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(&b, "loose|<err>;%v;", err)
		}
	}
	if b.Len() == 0 {
		return "empty"
	}
	return b.String()
}

func regularKindForScan(regularOnly bool) string {
	if regularOnly {
		return "event record"
	}
	return ""
}

func (e *engine) remove(id string) (retErr error) {
	if !e.localFS {
		return os.Remove(filepath.Join(e.eventsDir, id+".json"))
	}
	// remove may rewrite any family's hot segment, so it locks every family
	// shard (sorted, deadlock-free) — mutually exclusive with put's per-family
	// appends on the families it touches.
	releaseAll, err := e.lockAllFamilyShards()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.Remove(filepath.Join(e.eventsDir, id+".json"))
		}
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, releaseAll())
	}()
	// A serialized predecessor may have created a family while Remove waited for
	// the all-family boundary. Census only after acquisition so the set inspected
	// is the same generation the lock protects.
	families, err := e.segmentFamilies()
	if err != nil {
		return err
	}

	preferred := familyOf(nil, id)
	sort.SliceStable(families, func(i, j int) bool {
		return families[i] == preferred && families[j] != preferred
	})
	for _, family := range families {
		last, exists, err := e.lastWritten(family)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := e.repairTail(last.path); err != nil {
			return err
		}
		frames, err := parseSegment(last.path, false)
		if err != nil {
			return err
		}
		for i := len(frames) - 1; i >= 0; i-- {
			if frames[i].Frame.ID != id {
				continue
			}
			if i != len(frames)-1 {
				return fmt.Errorf("cannot remove non-tail event %s from append-only segment", id)
			}
			if err := truncateAndSync(last.path, frames[i].Offset, true); err != nil {
				return err
			}
			return nil
		}
	}
	if err := os.Remove(filepath.Join(e.eventsDir, id+".json")); err != nil {
		return err
	}
	return nil
}

func (e *engine) segmentFamilies() ([]string, error) {
	entries, err := os.ReadDir(e.segRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var families []string
	for _, entry := range entries {
		if entry.IsDir() {
			families = append(families, entry.Name())
		}
	}
	sort.Strings(families)
	return families, nil
}
