package eventstore

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	segmentsDirName = "segments"
	// maxBytes bounds ONE segment file before the append path rolls to the
	// next seq. It is first a HOT-PATH bound, not a disk-ergonomics bound:
	// every put in a short-lived process (each agent hook invocation is a
	// fresh process) fully parses the family's hot segment to prove
	// id-absence before appending. At 64 MiB that parse was the dominant
	// cost of a PreToolUse hook on a busy estate — measured 2026-08-24 at
	// 4.5–9.4s per tool call, occasionally past the harness's 60s hook
	// timeout, which kills the hook with NO output ("hook exited without a
	// status code"). At 8 MiB the per-put parse stays a fraction of that
	// for the whole day; rollover already existed (seq segments are in the
	// wild), so nothing else changes shape.
	maxBytes int64 = 8 << 20
)

type engineConfig struct {
	clock           func() time.Time
	probe           fsProbeFunc
	logf            func(string, ...any)
	segmentMaxBytes int64
}

type engine struct {
	home            string
	eventsDir       string
	segRoot         string
	indexRoot       string
	segmentMaxBytes int64
	clock           func() time.Time
	logf            func(string, ...any)
	localFS         bool
	fsType          string
	warnOnce        sync.Once
	sourceHashMu    sync.Mutex
	sourceHashes    map[string]cachedSourceHash
	appendShards    *shardedAppendLock
}

type putMode uint8

const (
	putIdempotent putMode = iota
	putExclusive
	putIfAbsent
	putAtomic
)

type putOutcome struct {
	created        bool
	diagnosticPath string
}

type idPresence uint8

const (
	idAbsent idPresence = iota
	idAlreadyPresent
	idConflict
)

var ErrIDConflict = errors.New("event id already exists with different content")

func newEngine(homePath string, config engineConfig) (*engine, error) {
	clock := config.clock
	if clock == nil {
		clock = time.Now
	}
	probe := config.probe
	if probe == nil {
		probe = checkLocalFS
	}
	logf := config.logf
	if logf == nil {
		logf = log.Printf
	}
	limit := config.segmentMaxBytes
	if limit <= 0 {
		limit = maxBytes
	}
	// Prefer the existing events directory when it is a distinct mount point;
	// otherwise walk to the nearest existing ancestor without creating state.
	probePath, err := nearestExistingPath(filepath.Join(homePath, eventsDirName))
	if err != nil {
		return nil, err
	}
	fsType, local, err := probe(probePath)
	if err != nil {
		return nil, fmt.Errorf("probe event store filesystem: %w", err)
	}
	if strings.TrimSpace(fsType) == "" {
		return nil, fmt.Errorf("probe event store filesystem: empty filesystem type")
	}
	return &engine{
		home:            homePath,
		eventsDir:       filepath.Join(homePath, eventsDirName),
		segRoot:         filepath.Join(homePath, eventsDirName, segmentsDirName),
		indexRoot:       filepath.Join(homePath, eventsDirName, indexDirName),
		segmentMaxBytes: limit,
		clock:           clock,
		logf:            logf,
		localFS:         local,
		fsType:          fsType,
		appendShards:    shardedAppendLocksFor(filepath.Join(homePath, eventsDirName, segmentsDirName)),
	}, nil
}

func nearestExistingPath(path string) (string, error) {
	current := filepath.Clean(path)
	for {
		if _, err := os.Stat(current); err == nil {
			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %s", path)
		}
		current = parent
	}
}

func ingestionDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

func segmentPath(segRoot, family, day string, seq int) string {
	name := day + ".jsonl"
	if seq > 0 {
		name = day + "." + strconv.Itoa(seq) + ".jsonl"
	}
	return filepath.Join(segRoot, family, name)
}

type segmentFile struct {
	path string
	day  string
	seq  int
}

// familyInventoryObserver is a test-only seam fired once per family
// directory ENUMERATION — the per-id cost the batch reader collapses to one
// enumeration per family. It is nil in production (counter-seam precedent:
// indexParseObserver in index.go).
var familyInventoryObserver func(family string)

func notifyFamilyInventory(family string) {
	if familyInventoryObserver != nil {
		familyInventoryObserver(family)
	}
}

func (e *engine) familySegments(family string) ([]segmentFile, error) {
	notifyFamilyInventory(family)
	dir := filepath.Join(e.segRoot, family)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	segments := make([]segmentFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		day, seq, ok := parseSegmentName(entry.Name())
		if !ok {
			continue
		}
		segments = append(segments, segmentFile{path: filepath.Join(dir, entry.Name()), day: day, seq: seq})
	}
	sort.Slice(segments, func(i, j int) bool {
		if segments[i].day == segments[j].day {
			return segments[i].seq > segments[j].seq
		}
		return segments[i].day > segments[j].day
	})
	return segments, nil
}

func parseSegmentName(name string) (string, int, bool) {
	if !strings.HasSuffix(name, ".jsonl") {
		return "", 0, false
	}
	stem := strings.TrimSuffix(name, ".jsonl")
	day := stem
	seq := 0
	if dot := strings.LastIndexByte(stem, '.'); dot >= 0 {
		day = stem[:dot]
		parsed, err := strconv.Atoi(stem[dot+1:])
		if err != nil || parsed <= 0 {
			return "", 0, false
		}
		seq = parsed
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", 0, false
	}
	return day, seq, true
}

func (e *engine) lastWritten(family string) (segmentFile, bool, error) {
	segments, err := e.familySegments(family)
	if err != nil {
		return segmentFile{}, false, err
	}
	if len(segments) == 0 {
		return segmentFile{}, false, nil
	}
	return segments[0], true, nil
}

func (e *engine) appendTarget(family, today string) (string, bool, error) {
	dir := filepath.Join(e.segRoot, family)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	segments, err := e.familySegments(family)
	if err != nil {
		return "", false, err
	}
	seq := 0
	foundToday := false
	for _, segment := range segments {
		if segment.day == today {
			seq = segment.seq
			foundToday = true
			break
		}
	}
	if foundToday {
		current := segmentPath(e.segRoot, family, today, seq)
		info, err := os.Stat(current)
		if err != nil {
			return "", false, err
		}
		if info.Size() >= e.segmentMaxBytes {
			seq++
			foundToday = false
		}
	}
	path := segmentPath(e.segRoot, family, today, seq)
	if foundToday {
		return path, false, nil
	}
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	return path, true, nil
}

func (e *engine) put(id string, record []byte, mode putMode, opts AtomicOptions) (putOutcome, error) {
	frame, _, err := buildFrame(id, record)
	if err != nil {
		return putOutcome{}, err
	}
	line, err := encodeFrame(frame)
	if err != nil {
		return putOutcome{}, err
	}
	if !e.localFS {
		if os.Getenv(requireLocalEnv) == "1" {
			return putOutcome{}, nonLocalFSError(e.fsType)
		}
		e.warnOnce.Do(func() {
			e.logf("eventstore: WARNING non-local filesystem %s; degrading to legacy loose writes", e.fsType)
		})
		return e.legacyPut(frame, record, mode, opts)
	}

	// Validate the current durable state under the family lock. Plain puts keep
	// this one acquisition through append. Atomic puts with pre-publication
	// callbacks release it because those callbacks are arbitrary and may re-enter
	// this store, then reacquire and revalidate before publication.
	lock, err := e.appendShards.acquire(frame.Family)
	if err != nil {
		return putOutcome{}, err
	}
	outcome, handled, checkErr := e.checkPutLocked(frame, mode)
	if checkErr != nil || handled {
		releaseErr := e.appendShards.release(frame.Family, lock)
		return outcome, errors.Join(checkErr, releaseErr)
	}

	if opts.Write != nil || opts.BeforePublish != nil {
		if releaseErr := e.appendShards.release(frame.Family, lock); releaseErr != nil {
			return putOutcome{}, releaseErr
		}
		if opts.Write != nil {
			if err := e.exerciseAtomicWriter(opts.Write, line); err != nil {
				return putOutcome{}, err
			}
		}
		if opts.BeforePublish != nil {
			if err := opts.BeforePublish(); err != nil {
				return putOutcome{}, err
			}
		}

		// The callbacks may have changed durable truth. Reacquire in canonical
		// order, repair the live tail again, and prove full id presence again.
		lock, err = e.appendShards.acquire(frame.Family)
		if err != nil {
			return putOutcome{}, err
		}
		outcome, handled, checkErr = e.checkPutLocked(frame, mode)
		if checkErr != nil || handled {
			releaseErr := e.appendShards.release(frame.Family, lock)
			return outcome, errors.Join(checkErr, releaseErr)
		}
	}
	// This held family lock is the publication linearization point.
	today := ingestionDay(e.clock())
	relPath, start, err := e.appendFrameAt(frame.Family, today, line)
	if err != nil {
		releaseErr := e.appendShards.release(frame.Family, lock)
		return putOutcome{}, errors.Join(err, releaseErr)
	}
	// Keep the hot-segment memo warm in place so the NEXT put's repairTail does
	// not reparse the whole hot segment: pre-append size == start, post-append
	// size/mtime from a stat. See hotSegmentAppend's freshness guard.
	absPath := filepath.Join(e.eventsDir, relPath)
	if info, statErr := os.Stat(absPath); statErr == nil {
		hotSegmentAppend(absPath, info.Size(), info.ModTime(), parsedFrame{
			Frame: frame, Offset: start, End: start + int64(len(line)),
		})
	}
	outcome = putOutcome{created: true, diagnosticPath: logicalReferencePrefix + id}
	releaseErr := e.appendShards.release(frame.Family, lock)
	if opts.AfterPublish != nil {
		return outcome, errors.Join(releaseErr, opts.AfterPublish())
	}
	return outcome, releaseErr
}

// checkPutLocked repairs the current family tail and resolves the complete id
// history. The caller must hold the family's process mutex and flock.
func (e *engine) checkPutLocked(frame Frame, mode putMode) (putOutcome, bool, error) {
	last, exists, err := e.lastWritten(frame.Family)
	if err != nil {
		return putOutcome{}, false, err
	}
	if exists {
		if err := e.repairTail(last.path); err != nil {
			return putOutcome{}, false, err
		}
	}
	presence, existingPath, err := e.checkID(frame.Family, frame.ID, frame.SHA256)
	if err != nil {
		return putOutcome{}, false, err
	}
	if presence == idAbsent {
		return putOutcome{}, false, nil
	}
	switch mode {
	case putExclusive:
		return putOutcome{}, true, fmt.Errorf("%s: %w", frame.ID, os.ErrExist)
	case putIfAbsent:
		return putOutcome{created: false, diagnosticPath: existingPath}, true, nil
	default:
		if presence == idConflict {
			return putOutcome{}, true, fmt.Errorf("%w: %s", ErrIDConflict, frame.ID)
		}
		return putOutcome{created: false, diagnosticPath: existingPath}, true, nil
	}
}

// appendFrameAt appends one encoded frame line to the family's current segment.
// The caller must already hold the append lock and must have repaired the target
// family's tail. It returns the segment path relative to the events dir and the
// byte offset the frame begins at, so the migration seam can record a precise
// {segment, offset} manifest coordinate for every folded record.
func (e *engine) appendFrameAt(family, today string, line []byte) (relPath string, offset int64, err error) {
	path, created, err := e.appendTarget(family, today)
	if err != nil {
		return "", 0, err
	}
	flags := os.O_WRONLY | os.O_APPEND
	if created {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return "", 0, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if created {
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return "", 0, err
		}
	}
	start, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return "", 0, err
	}
	if err := writeFull(file, line); err != nil {
		truncateErr := file.Truncate(start)
		syncErr := file.Sync()
		if truncateErr != nil {
			return "", 0, fmt.Errorf("append event frame: %v; rollback truncate: %w", err, truncateErr)
		}
		if syncErr != nil {
			return "", 0, fmt.Errorf("append event frame: %v; rollback sync: %w", err, syncErr)
		}
		return "", 0, err
	}
	if err := file.Sync(); err != nil {
		return "", 0, err
	}
	if err := file.Close(); err != nil {
		return "", 0, err
	}
	closed = true
	rel, relErr := filepath.Rel(e.eventsDir, path)
	if relErr != nil {
		rel = path
	}
	return rel, start, nil
}

func (e *engine) exerciseAtomicWriter(writer func(*os.File, []byte) error, line []byte) error {
	tmp, err := os.CreateTemp(e.segRoot, ".eventstore-atomic-write-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(path)
	}()
	if err := writer(tmp, line); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	written, err := io.ReadAll(tmp)
	if err != nil {
		return err
	}
	if string(written) != string(line) {
		return io.ErrShortWrite
	}
	return nil
}

func (e *engine) checkID(family, id, hash string) (idPresence, string, error) {
	// A loose record is authoritative for its id only until the migration
	// completion gate closes. Once completion.json exists the loose layer is no
	// longer consulted on the write path: migrated ids live in segments and any
	// reappearing loose file is a legacy-writer regression for the doctor, not
	// silent authority.
	if e.legacyFallbackEnabled() {
		legacyPath := filepath.Join(e.eventsDir, id+".json")
		if data, err := os.ReadFile(legacyPath); err == nil {
			compact, compactErr := canonical(data)
			if compactErr != nil {
				return idAbsent, "", corruptionAt(legacyPath, data, 1, 0, compactErr)
			}
			if recordHash(compact) == hash {
				return idAlreadyPresent, legacyPath, nil
			}
			return idConflict, legacyPath, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return idAbsent, "", err
		}
	}

	segments, err := e.familySegments(family)
	if err != nil {
		return idAbsent, "", err
	}
	presence := idAbsent
	// Sealed segments (index > 0): a negative-membership proof lets the common
	// brand-new id skip every sealed index load (membership.go). "Maybe
	// present" runs the exact per-segment scan below.
	coldPossible := len(segments) > 1 && e.coldMaybeContains(family, id, segments[1:])
	for index, segment := range segments {
		if index > 0 {
			if !coldPossible {
				continue
			}
			frame, found, err := e.readIndexedFrame(family, segment, id, "")
			if err != nil {
				return idAbsent, "", err
			}
			if !found {
				continue
			}
			if frame.SHA256 != hash {
				return idConflict, logicalReferencePrefix + id, nil
			}
			presence = idAlreadyPresent
			continue
		}

		// Hot (last-written, index-0) segment: resolve through the same
		// per-process memo the read path uses (hotcache.go) instead of a
		// full parse per put. repairTail's interior sweep just above
		// (repair.go) already warms this memo for the common case, so this
		// is a pure map lookup. divergent reproduces base checkID's
		// scan-every-occurrence semantics: conflict ⇔ (newest frame's hash
		// != incoming) OR (id has intra-divergent duplicate frames) — an id
		// in divergent means SOME occurrence's hash differs from every other,
		// so it can never equal the incoming hash for all occurrences, which
		// is exactly base checkID's per-frame mismatch scan collapsed to one
		// lookup (see hotFramesAndDivergent).
		byID, divergent, err := e.hotFramesAndDivergent(segment.path)
		if err != nil {
			return idAbsent, "", err
		}
		if _, ok := divergent[id]; ok {
			return idConflict, logicalReferencePrefix + id, nil
		}
		if frame, ok := byID[id]; ok {
			if frame.Frame.SHA256 != hash {
				return idConflict, logicalReferencePrefix + id, nil
			}
			presence = idAlreadyPresent
		}
	}
	if presence == idAlreadyPresent {
		return presence, logicalReferencePrefix + id, nil
	}
	return idAbsent, "", nil
}

func (e *engine) legacyPut(frame Frame, record []byte, mode putMode, opts AtomicOptions) (putOutcome, error) {
	if err := os.MkdirAll(e.eventsDir, 0o755); err != nil {
		return putOutcome{}, err
	}
	path := filepath.Join(e.eventsDir, frame.ID+".json")
	if existing, err := os.ReadFile(path); err == nil {
		switch mode {
		case putExclusive:
			return putOutcome{}, fmt.Errorf("%s: %w", frame.ID, os.ErrExist)
		case putIfAbsent:
			return putOutcome{diagnosticPath: path}, nil
		}
		compact, compactErr := canonical(existing)
		if compactErr != nil {
			return putOutcome{}, corruptionAt(path, existing, 1, 0, compactErr)
		}
		if recordHash(compact) == frame.SHA256 {
			return putOutcome{diagnosticPath: path}, nil
		}
		return putOutcome{}, fmt.Errorf("%w: %s", ErrIDConflict, frame.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return putOutcome{}, err
	}

	if mode == putAtomic {
		return e.legacyAtomicPut(frame.ID, path, record, opts)
	}
	if opts.BeforePublish != nil {
		if err := opts.BeforePublish(); err != nil {
			return putOutcome{}, err
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if mode == putExclusive {
		flags = os.O_CREATE | os.O_EXCL | os.O_WRONLY
	}
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return putOutcome{}, err
	}
	if err := writeFull(file, record); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return putOutcome{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return putOutcome{}, err
	}
	if err := file.Close(); err != nil {
		return putOutcome{}, err
	}
	if err := syncDirectory(e.eventsDir); err != nil {
		return putOutcome{}, err
	}
	outcome := putOutcome{created: true, diagnosticPath: path}
	if opts.AfterPublish != nil {
		if err := opts.AfterPublish(); err != nil {
			return outcome, err
		}
	}
	return outcome, nil
}

func (e *engine) legacyAtomicPut(id, path string, record []byte, opts AtomicOptions) (putOutcome, error) {
	tmp, err := os.CreateTemp(e.eventsDir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return putOutcome{}, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return putOutcome{}, err
	}
	writer := opts.Write
	if writer == nil {
		writer = writeFull
	}
	if err := writer(tmp, record); err != nil {
		return putOutcome{}, err
	}
	if err := tmp.Sync(); err != nil {
		return putOutcome{}, err
	}
	if err := tmp.Close(); err != nil {
		return putOutcome{}, err
	}
	if opts.BeforePublish != nil {
		if err := opts.BeforePublish(); err != nil {
			return putOutcome{}, err
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return putOutcome{}, err
	}
	if opts.SyncDirectory {
		if err := syncDirectory(e.eventsDir); err != nil {
			return putOutcome{}, err
		}
	}
	outcome := putOutcome{created: true, diagnosticPath: path}
	if opts.AfterPublish != nil {
		if err := opts.AfterPublish(); err != nil {
			return outcome, err
		}
	}
	return outcome, nil
}

// lockAllFamilyShards acquires every family shard that could be appended to —
// the existing segment directories plus the known id-prefix vocabulary — and
// returns a release func. Maintenance paths (migration apply, post-completion
// repair import) use it so their segment mutations stay mutually exclusive with
// put's per-family appends; locking all of them (≤ a dozen, sorted, deadlock-
// free) is cheap on these rare paths and never touches the hot per-tool-call
// put. Callers that rewrite one known family can lock just that shard instead.
func (e *engine) lockAllFamilyShards() (func() error, error) {
	fams, err := e.segmentFamilies()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range fams {
		seen[f] = true
	}
	for _, pf := range idPrefixFamilies {
		if !seen[pf.family] {
			seen[pf.family] = true
			fams = append(fams, pf.family)
		}
	}
	if !seen["other"] {
		fams = append(fams, "other")
	}
	return e.appendShards.acquireAll(fams)
}

func (e *engine) preflight() error {
	if !e.localFS && os.Getenv(requireLocalEnv) == "1" {
		return nonLocalFSError(e.fsType)
	}
	dir := e.segRoot
	if !e.localFS {
		dir = e.eventsDir
		e.warnOnce.Do(func() {
			e.logf("eventstore: WARNING non-local filesystem %s; degrading to legacy loose writes", e.fsType)
		})
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// No append lock here: this is a writability probe on a uniquely-named temp
	// file, independent of segment appends. Pre-2026-08-08 this took the global
	// .append.lock on EVERY store open (every tool call), adding a second global
	// serialization point on top of put's.
	probe, err := os.CreateTemp(dir, ".eventstore-preflight-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err := probe.Sync(); err != nil {
		_ = probe.Close()
		_ = os.Remove(name)
		return err
	}
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// SegmentsRelativeDir returns the home-relative physical path of the sealed
// event segments directory ("events/segments"). internal/eventstore is the
// only package that may know this layout (the arch eventstore-ownership
// gate); other packages must resolve it here rather than spell the literal.
func SegmentsRelativeDir() string {
	return filepath.Join(eventsDirName, segmentsDirName)
}
