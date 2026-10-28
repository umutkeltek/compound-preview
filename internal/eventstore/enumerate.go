package eventstore

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// EnumerateIDs returns the stable ids of every stored event record whose id
// begins with prefix, in ascending lexical order. It exists for callers that
// need the exact id SET behind a prefix (one proof reservation's refresh
// outcomes) without paying for unrelated record content: sealed segments are
// screened through their validated sidecar index KEYS, and only a segment
// whose complete inventory holds matching ids has its bytes read (one rooted,
// strict parse validating every matching frame). Segments with no matching
// index keys are skipped after their first complete validation. Enumeration
// still reads the family's index inventory. The hot (still-written)
// segment has no sidecar index yet and is parsed directly, bounded by
// rotation. While the migration completion gate is open, legacy loose records
// matching the prefix are unioned in, with the same fail-closed rule Scan
// applies to an invalid loose filename that matches the requested prefix.
//
// Completeness contract: the enumeration-specific inventory marker binds the
// complete id/offset/length map to validated sealed frames. Source-only
// .verified markers are insufficient. Missing or changed inventory proof
// triggers a full strict validation and repairs incomplete indexes. There is
// no cached result set: older writers and changed segments remain visible.
//
// Newest-segment precedence: like Scan, the id set is the union across
// segments; per-id byte-level precedence (newest segment wins) remains owned
// by Read/segmentFrame, and duplicate ids are reported once.
//
// An empty prefix enumerates every id (the meaning Scan gives empty Prefixes)
// and walks every family; a prefix that maps onto the idPrefixFamilies
// vocabulary restricts the walk to those families. RecentSegments is not
// honored: enumeration is exact, never truncated.
func (s *Store) EnumerateIDs(prefix string, regularOnly bool) ([]string, error) {
	return s.engine.enumeratePrefixIDs(prefix, regularOnly)
}

func (e *engine) enumeratePrefixIDs(prefix string, regularOnly bool) ([]string, error) {
	regularKind := regularKindForScan(regularOnly)
	restriction := scanFamilyRestriction([]string{prefix})
	families, err := e.segmentFamilies()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, family := range families {
		if restriction != nil && !restriction[family] {
			continue
		}
		segments, err := e.familySegments(family)
		if err != nil {
			return nil, err
		}
		for index, segment := range segments {
			if index == 0 {
				// Hot (still-written) segment: no sidecar index exists yet,
				// so parse it. This is the only unconditionally parsed
				// segment, and it is bounded by rotation.
				frames, err := e.readSegment(segment.path, true, regularKind)
				if err != nil {
					return nil, err
				}
				notifyScanParse(segment.path, len(frames), 0)
				for i := len(frames) - 1; i >= 0; i-- {
					id := frames[i].Frame.ID
					if matchesPrefix(id, []string{prefix}) {
						seen[id] = true
					}
				}
				continue
			}
			// Sealed segment: screen by index keys; only parse when at least
			// one candidate key matches.
			segIndex, err := e.loadCompleteEnumerationIndex(family, segment, regularKind)
			if err != nil {
				return nil, err
			}
			candidates := make([]string, 0, 4)
			for id := range segIndex.Entries {
				if matchesPrefix(id, []string{prefix}) && !seen[id] {
					candidates = append(candidates, id)
				}
			}
			if len(candidates) == 0 {
				continue
			}
			present, err := e.validateIndexedIDs(family, segment, candidates, regularKind)
			if err != nil {
				return nil, err
			}
			for id, exists := range present {
				if exists && matchesPrefix(id, []string{prefix}) {
					seen[id] = true
				}
			}
		}
	}

	// Loose union, only while the migration completion gate is open — the
	// same layer Scan unions. A legacy writer's loose record stays authority
	// until the gate closes, so skipping it would silently drop old writers.
	if e.legacyFallbackEnabled() {
		dirEntries, err := os.ReadDir(e.eventsDir)
		if err != nil {
			return nil, err
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
			if !matchesPrefix(id, []string{prefix}) {
				continue
			}
			if _, err := ReferenceForID(id); err != nil {
				return nil, fmt.Errorf("invalid event enumeration entry %q: %w", name, err)
			}
			seen[id] = true
		}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// validateIndexedIDs proves that each candidate id from a sealed segment's
// sidecar index is really present in that segment's bytes. The parse is
// strict (a sealed segment must not carry a torn tail) and — under
// regularOnly — rooted, so the same symlink/stability defense Scan applies to
// segment content covers the one read. If the index disagrees with segment
// truth for any candidate, the index is rebuilt from that truth and the check
// repeats, mirroring readIndexedFrame's contract for point reads.
func (e *engine) validateIndexedIDs(family string, segment segmentFile, candidates []string, regularKind string) (map[string]bool, error) {
	present, err := e.segmentContainsIDs(segment, regularKind, candidates)
	if err != nil {
		return nil, err
	}
	disagrees := false
	for _, id := range candidates {
		if !present[id] {
			disagrees = true
			break
		}
	}
	if !disagrees {
		return present, nil
	}
	if _, err := e.rebuildIndex(family, segment, "enumeration candidate missing from segment bytes"); err != nil {
		return nil, err
	}
	return e.segmentContainsIDs(segment, regularKind, candidates)
}

func (e *engine) segmentContainsIDs(segment segmentFile, regularKind string, candidates []string) (map[string]bool, error) {
	frames, err := e.readSegment(segment.path, false, regularKind)
	if err != nil {
		return nil, err
	}
	notifyScanParse(segment.path, len(frames), 0)
	present := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		present[id] = false
	}
	for _, frame := range frames {
		present[frame.Frame.ID] = true
	}
	return present, nil
}
