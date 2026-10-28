package eventstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ReadResult is the record or native read error for one original reference.
type ReadResult struct {
	Record Record
	Err    error
}

// ReadMany batches exact references using request-local family inventories,
// hot frames and sealed indexes. On a stable store, each result matches Read,
// including reference parsing, newest-occurrence rules and native errors.
// Existing owner validation and index repair remain in use.
//
// Concurrent reads are nontransactional: results need not describe one instant
// or the state at return. A later call reacquires facts through the same
// stat/hash freshness checks as Read. No batch verdict survives this call.
// ReadRegular retains its separate rooted validation path.
func (s *Store) ReadMany(refs []string) map[string]ReadResult {
	results := make(map[string]ReadResult, len(refs))
	ids := make([]string, 0, len(refs))
	keysForID := make(map[string][]string, len(refs))
	seenRaw := make(map[string]bool, len(refs))
	for _, raw := range refs {
		if seenRaw[raw] {
			continue
		}
		seenRaw[raw] = true
		ref, err := ParseReference(raw)
		if err != nil {
			results[raw] = ReadResult{Err: err}
			continue
		}
		id := ref.ID()
		if _, known := keysForID[id]; !known {
			ids = append(ids, id)
		}
		keysForID[id] = append(keysForID[id], raw)
	}
	if len(ids) == 0 {
		return results
	}

	stored, errs := s.engine.resolveBatch(ids)
	for _, id := range ids {
		var result ReadResult
		if err, failed := errs[id]; failed {
			result.Err = err
		} else {
			rec := stored[id]
			result.Record = Record{
				ID: id, Reference: Reference{id: id},
				Bytes: rec.bytes, DiagnosticPath: rec.diagnosticPath,
			}
		}
		for _, raw := range keysForID[id] {
			results[raw] = result
		}
	}
	return results
}

func (e *engine) resolveBatch(ids []string) (map[string]storedRecord, map[string]error) {
	stored := make(map[string]storedRecord, len(ids))
	errs := make(map[string]error)
	pending := make(map[string]bool, len(ids))
	byFamily := make(map[string][]string)
	for _, id := range ids {
		pending[id] = true
		family := familyOf(nil, id)
		byFamily[family] = append(byFamily[family], id)
	}
	families := make([]string, 0, len(byFamily))
	for family := range byFamily {
		families = append(families, family)
	}
	sort.Strings(families)
	for _, family := range families {
		familyIDs := byFamily[family]
		segments, err := e.familySegments(family)
		if err != nil {
			failPendingFamily(familyIDs, pending, errs, err)
			continue
		}
		e.resolveFamily(family, segments, familyIDs, pending, stored, errs)
	}
	if len(pending) == 0 {
		return stored, errs
	}

	// Match getByID's loose fallback, sharing the completion-gate observation.
	looseEnabled := e.legacyFallbackEnabled()
	for _, id := range ids {
		if !pending[id] {
			continue
		}
		if !looseEnabled {
			errs[id] = fmt.Errorf("event %s not found in segments (migration complete): %w", id, os.ErrNotExist)
			continue
		}
		path := filepath.Join(e.eventsDir, id+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			errs[id] = err
			continue
		}
		stored[id] = storedRecord{id: id, bytes: data, diagnosticPath: path}
	}
	return stored, errs
}

// resolveFamily uses the point reader's newest-first traversal and validation.
// Only unresolved IDs reach an older segment or receive its native error.
func (e *engine) resolveFamily(family string, segments []segmentFile, ids []string, pending map[string]bool, stored map[string]storedRecord, errs map[string]error) {
	for position, segment := range segments {
		if !anyPending(ids, pending) {
			return
		}
		if position == 0 {
			notifyHotSegmentAcquire(segment.path)
			byID, err := e.hotFrames(segment.path)
			if err != nil {
				failPendingFamily(ids, pending, errs, err)
				return
			}
			for _, id := range ids {
				if !pending[id] {
					continue
				}
				hit, found := byID[id]
				if !found {
					continue
				}
				stored[id] = storedRecord{
					id: id, bytes: append(append([]byte(nil), hit.Frame.Record...), '\n'),
					diagnosticPath: logicalReferencePrefix + id, fromSegment: true,
				}
				delete(pending, id)
			}
			continue
		}

		index, err := e.loadOrBuildIndex(family, segment)
		if err != nil {
			failPendingFamily(ids, pending, errs, err)
			return
		}
		for _, id := range ids {
			if !pending[id] {
				continue
			}
			frame, found, err := e.resolveIndexedFrame(family, segment, &index, id, "")
			if err != nil {
				errs[id] = err
				delete(pending, id)
				continue
			}
			if !found {
				continue
			}
			stored[id] = storedRecord{
				id: id, bytes: append(append([]byte(nil), frame.Record...), '\n'),
				diagnosticPath: logicalReferencePrefix + id, fromSegment: true,
			}
			delete(pending, id)
		}
	}
}

func anyPending(ids []string, pending map[string]bool) bool {
	for _, id := range ids {
		if pending[id] {
			return true
		}
	}
	return false
}

func failPendingFamily(ids []string, pending map[string]bool, errs map[string]error, err error) {
	for _, id := range ids {
		if pending[id] {
			errs[id] = err
			delete(pending, id)
		}
	}
}

// hotSegmentObserver counts acquisitions in point and batch reads for tests.
// Production leaves it nil.
var hotSegmentObserver func(path string)

func notifyHotSegmentAcquire(path string) {
	if hotSegmentObserver != nil {
		hotSegmentObserver(path)
	}
}
