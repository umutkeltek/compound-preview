package routeevidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// lineRef locates one already-parsed JSONL row within the ledger file: the
// byte offset and length of the RAW (untrimmed) line, plus its 1-based
// absolute line number for error messages.
type lineRef struct {
	offset int64
	length int
	line   int
}

// requestIDIndex is an incremental, path-keyed memo of request_id -> line
// offsets over one execution-route-evidence.jsonl ledger. It exists so a
// lookup costs O(bytes appended since the last lookup), not O(whole ledger),
// while a single writer keeps appending rows under the shared admission lock.
//
// Freshness is never keyed on inode: recordEvidenceLocked (routeevidence.go)
// publishes every append via full copy-and-swap CAS (temp file + rename), so
// the ledger's inode changes on every write even though its content is
// strictly append-only (postimage = preimage + one line + "\n"). The only
// freshness signals that survive a normal CAS append are size and modTime.
type requestIDIndex struct {
	mu sync.Mutex

	// warm is false until this index has synchronized with an existing
	// ledger file at least once. It (not "offsets != nil") is the signal for
	// "never initialized", because a persistently empty ledger legitimately
	// never calls parseRangeLocked and must not be reset on every lookup.
	warm bool

	consumed  int64 // bytes of complete (\n-terminated) lines folded into offsets
	modTime   time.Time
	lineCount int // absolute count of complete lines folded into offsets
	offsets   map[string][]lineRef

	// trailingID/trailingRef describe a trailing line that lacks a
	// terminating "\n" as of the last refresh. It is parsed fresh on every
	// refresh (never persisted past it) because consumed deliberately does
	// not advance past it — the line may still be growing.
	trailingID  string
	trailingRef lineRef

	// Test-only depth-falsifier counters (F1). Guarded by mu; no production
	// caller reads them. fullRebuilds counts full-ledger (re)parses;
	// linesParsed counts every non-blank line actually run through
	// json.Unmarshal (the cost F1 bounds).
	fullRebuilds int64
	linesParsed  int64
}

var (
	requestIDIndexesMu sync.Mutex
	requestIDIndexes   = map[string]*requestIDIndex{}
)

func requestIDIndexKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func requestIDIndexFor(path string) *requestIDIndex {
	key := requestIDIndexKey(path)
	requestIDIndexesMu.Lock()
	defer requestIDIndexesMu.Unlock()
	idx, ok := requestIDIndexes[key]
	if !ok {
		idx = &requestIDIndex{}
		requestIDIndexes[key] = idx
	}
	return idx
}

// requestIDRefs refreshes the incremental index for the ledger at path and
// returns every line reference recorded under requestID. missing=true means
// the ledger file does not exist (mirrors the historical os.IsNotExist ->
// not-found contract; not an error).
func requestIDRefs(path, requestID string) ([]lineRef, bool, error) {
	idx := requestIDIndexFor(path)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	missing, err := idx.refreshLocked(path)
	if err != nil {
		return nil, false, err
	}
	if missing {
		return nil, true, nil
	}
	matched := idx.refsForLocked(requestID)
	if len(matched) == 0 {
		return nil, false, nil
	}
	out := make([]lineRef, len(matched))
	copy(out, matched)
	return out, false, nil
}

func (idx *requestIDIndex) refsForLocked(requestID string) []lineRef {
	refs := idx.offsets[requestID]
	if idx.trailingID == "" || idx.trailingID != requestID {
		return refs
	}
	combined := make([]lineRef, 0, len(refs)+1)
	combined = append(combined, refs...)
	combined = append(combined, idx.trailingRef)
	return combined
}

// refreshLocked brings idx up to date for the ledger at path via exactly one
// os.Stat, then parses only what changed:
//   - missing file -> drop the memo, report missing (not an error).
//   - size < consumed -> shrink/rewrite: full rebuild.
//   - size == consumed && modTime equal -> memo current, nothing to do.
//   - size == consumed && modTime differs -> same-size rewrite paranoia: full rebuild.
//   - size > consumed -> parse only the bytes appended since consumed.
func (idx *requestIDIndex) refreshLocked(path string) (missing bool, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			idx.resetLocked()
			return true, nil
		}
		return false, statErr
	}
	size := info.Size()
	modTime := info.ModTime()

	needsRebuild := !idx.warm ||
		size < idx.consumed ||
		(size == idx.consumed && !idx.modTime.Equal(modTime))
	if needsRebuild {
		idx.resetLocked()
		idx.fullRebuilds++
	}
	idx.warm = true

	if size == idx.consumed {
		idx.modTime = modTime
		return false, nil
	}
	if err := idx.parseRangeLocked(path); err != nil {
		return false, err
	}
	idx.modTime = modTime
	return false, nil
}

func (idx *requestIDIndex) resetLocked() {
	idx.warm = false
	idx.consumed = 0
	idx.lineCount = 0
	idx.offsets = nil
	idx.trailingID = ""
	idx.trailingRef = lineRef{}
}

// parseRangeLocked parses the ledger bytes from idx.consumed to EOF, folding
// every complete (\n-terminated) line into idx.offsets and advancing
// idx.consumed/idx.lineCount past it. A trailing unterminated chunk (the
// ledger's writer may legitimately not have appended "\n" yet, and today's
// full-scan implementation parsed it too) is parsed for THIS refresh only:
// it is recorded in idx.trailingID/trailingRef but idx.consumed stays before
// it, so the next refresh re-reads and re-validates it from scratch.
//
// A malformed line returns the same absolute-line-number error the prior
// full-scan implementation returned, and does NOT advance consumed/lineCount
// past it — retries before a fix reproduce the identical error deterministically;
// a retry after a fix resumes parsing exactly at the repaired line.
func (idx *requestIDIndex) parseRangeLocked(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(idx.consumed, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if idx.offsets == nil {
		idx.offsets = map[string][]lineRef{}
	}

	idx.trailingID = ""
	idx.trailingRef = lineRef{}

	base := idx.consumed
	pos := 0
	for {
		nl := bytes.IndexByte(data[pos:], '\n')
		if nl < 0 {
			break
		}
		lineNo := idx.lineCount + 1
		raw := data[pos : pos+nl]
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			pos += nl + 1
			idx.consumed = base + int64(pos)
			idx.lineCount = lineNo
			continue
		}
		idx.linesParsed++
		var ev Evidence
		if err := json.Unmarshal(trimmed, &ev); err != nil {
			return fmt.Errorf("parse execution route evidence line %d: %w", lineNo, err)
		}
		idx.offsets[ev.RequestID] = append(idx.offsets[ev.RequestID], lineRef{
			offset: base + int64(pos),
			length: nl,
			line:   lineNo,
		})
		pos += nl + 1
		idx.consumed = base + int64(pos)
		idx.lineCount = lineNo
	}

	if pos < len(data) {
		raw := data[pos:]
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 {
			lineNo := idx.lineCount + 1
			idx.linesParsed++
			var ev Evidence
			if err := json.Unmarshal(trimmed, &ev); err != nil {
				return fmt.Errorf("parse execution route evidence line %d: %w", lineNo, err)
			}
			idx.trailingID = ev.RequestID
			idx.trailingRef = lineRef{
				offset: base + int64(pos),
				length: len(raw),
				line:   lineNo,
			}
		}
	}
	return nil
}

// readLineRef re-reads and re-decodes one already-located row directly from
// the ledger file. The index deliberately never caches decoded Evidence
// values (only byte offsets), so memory stays O(distinct request_ids), not
// O(ledger).
func readLineRef(path string, ref lineRef) (Evidence, error) {
	file, err := os.Open(path)
	if err != nil {
		return Evidence{}, err
	}
	defer file.Close()
	buf := make([]byte, ref.length)
	if _, err := file.ReadAt(buf, ref.offset); err != nil {
		return Evidence{}, fmt.Errorf("re-read execution route evidence line %d: %w", ref.line, err)
	}
	var ev Evidence
	if err := json.Unmarshal(bytes.TrimSpace(buf), &ev); err != nil {
		return Evidence{}, fmt.Errorf("parse execution route evidence line %d: %w", ref.line, err)
	}
	return ev, nil
}

// requestIDIndexCountersForTest exposes the depth-falsifier counters for the
// ledger at homePath. Test-only seam; no production caller.
func requestIDIndexCountersForTest(homePath string) (fullRebuilds, linesParsed int64) {
	path := filepath.Join(homePath, "ledger", executionRouteEvidenceLedger)
	idx := requestIDIndexFor(path)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.fullRebuilds, idx.linesParsed
}

// resetRequestIDIndexForTest drops the in-memory memo for the ledger at
// homePath so a test can measure amortized cost over an isolated window.
// Test-only seam; no production caller.
func resetRequestIDIndexForTest(homePath string) {
	path := filepath.Join(homePath, "ledger", executionRouteEvidenceLedger)
	idx := requestIDIndexFor(path)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.warm = false
	idx.consumed = 0
	idx.lineCount = 0
	idx.modTime = time.Time{}
	idx.offsets = nil
	idx.trailingID = ""
	idx.trailingRef = lineRef{}
	idx.fullRebuilds = 0
	idx.linesParsed = 0
}
