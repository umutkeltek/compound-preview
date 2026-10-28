// Package eventstore is the sole physical owner of COMPOUND_HOME/events record
// layout. New records use framed append-only segments; legacy loose records
// remain a read fallback until the separately governed migration slice.
package eventstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"compound/internal/home"
)

const (
	eventsDirName              = "events"
	workLogName                = "work.jsonl"
	hookEventsStreamName       = "hook-events.ndjson"
	tracesStreamName           = "traces.ndjson"
	retrievalTrafficStreamName = "retrieval-traffic.ndjson"
)

// Store owns event records and the named compatibility streams under one home.
// Open validates configuration only; read-only callers do not create state.
type Store struct {
	home   string
	engine *engine
}

// Record is one event read through the seam. DiagnosticPath is intentionally
// available only after resolving an actual record; it is not a path builder.
type Record struct {
	ID             string
	Reference      Reference
	Bytes          []byte
	DiagnosticPath string
}

// PutResult reports whether a writer created/published a record. A skipped
// if-absent write returns Created=false and the same logical identity.
type PutResult struct {
	Reference      Reference
	Created        bool
	DiagnosticPath string
}

// AtomicOptions preserves the existing durability and injected-failure seams
// used by work and memory writers without exposing loose object paths.
type AtomicOptions struct {
	SyncDirectory bool
	Write         func(*os.File, []byte) error
	BeforePublish func() error
	AfterPublish  func() error
}

// ScanOptions controls filename filtering before any matching entry is opened.
type ScanOptions struct {
	Prefixes    []string
	RegularOnly bool
	// Cursor is reserved for the engine-independent scan contract. The loose
	// engine returns a complete deterministic scan when it is empty; segmented
	// engines may issue and consume opaque cursors without changing callers.
	Cursor string
	// RecentSegments caps each family to its N most-recent segment files
	// (day DESC, then sequence DESC). 0 (default) reads every segment.
	// Used by callers that only need the freshest records in a family
	// (e.g. the workbench hook-events scan keeps only the latest event per
	// harness, so 41 days × ~12MB is wasteful when the last 2 days cover
	// the live executors).
	RecentSegments int
}

// Entry is an event discovered by Scan. Segment scans already decode each
// record while validating frame truth, so Entry retains those bytes and Read
// does not perform a second point lookup. Legacy loose entries remain lazy: a
// matching unreadable loose file is still surfaced only when Read is called.
type Entry struct {
	ID             string
	Name           string
	Reference      Reference
	cachedBytes    []byte
	diagnosticPath string
	store          *Store
}

func Open(homePath string) (*Store, error) {
	return openWithConfig(homePath, engineConfig{})
}

func openWithConfig(homePath string, config engineConfig) (*Store, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	clean := filepath.Clean(homePath)
	engine, err := newEngine(clean, config)
	if err != nil {
		return nil, err
	}
	return &Store{home: clean, engine: engine}, nil
}

// Ensure creates the current physical event root for a writer.
func (s *Store) Ensure() error {
	if s == nil || strings.TrimSpace(s.home) == "" {
		return fmt.Errorf("event store requires a home")
	}
	return os.MkdirAll(s.rootPath(), 0o755)
}

// Preflight proves the selected engine can lock, create, sync, and remove a
// probe before a governed mutation reaches its post-write persistence phase.
func (s *Store) Preflight() error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("event store requires a home")
	}
	return s.engine.preflight()
}

func (s *Store) rootPath() string { return filepath.Join(s.home, eventsDirName) }

func (s *Store) recordPath(ref Reference) string {
	return filepath.Join(s.rootPath(), ref.ID()+".json")
}

func (s *Store) resolve(raw string) (Reference, string, error) {
	ref, err := ParseReference(raw)
	if err != nil {
		return Reference{}, "", err
	}
	return ref, s.recordPath(ref), nil
}

// Read resolves a bare, logical, or historical reference. Segments are
// authoritative when both representations contain the same id.
func (s *Store) Read(raw string) (Record, error) {
	ref, err := ParseReference(raw)
	if err != nil {
		return Record{}, err
	}
	stored, err := s.engine.getByID(ref.ID(), "")
	if err != nil {
		return Record{}, err
	}
	return Record{ID: ref.ID(), Reference: ref, Bytes: stored.bytes, DiagnosticPath: stored.diagnosticPath}, nil
}

// ReadRegular is the fail-closed rooted variant used where a symlinked record
// or events ancestor must never qualify as authority evidence.
func (s *Store) ReadRegular(raw, kind string) (Record, error) {
	ref, err := ParseReference(raw)
	if err != nil {
		return Record{}, err
	}
	stored, err := s.engine.getByID(ref.ID(), kind)
	if err != nil {
		return Record{}, err
	}
	return Record{ID: ref.ID(), Reference: ref, Bytes: stored.bytes, DiagnosticPath: stored.diagnosticPath}, nil
}

func (s *Store) Exists(raw string) (bool, error) {
	_, err := s.Read(raw)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) Remove(raw string) error {
	ref, err := ParseReference(raw)
	if err != nil {
		return err
	}
	return s.engine.remove(ref.ID())
}

// PutJSON keeps the slice-1 signature while the segment engine makes same-id
// retries idempotent and refuses same-id/different-record conflicts.
func (s *Store) PutJSON(id string, value any, indent string) (PutResult, error) {
	return s.putJSON(id, value, indent, putIdempotent, AtomicOptions{})
}

// PutJSONExclusive retains the caller-visible os.ErrExist contract.
func (s *Store) PutJSONExclusive(id string, value any, indent string) (PutResult, error) {
	return s.putJSON(id, value, indent, putExclusive, AtomicOptions{})
}

// PutJSONIfAbsent silently keeps either existing representation.
func (s *Store) PutJSONIfAbsent(id string, value any, indent string) (PutResult, error) {
	return s.putJSON(id, value, indent, putIfAbsent, AtomicOptions{})
}

// PutJSONAtomic retains the injected publication boundaries while delegating
// durability to the append engine.
func (s *Store) PutJSONAtomic(id string, value any, indent string, opts AtomicOptions) (PutResult, error) {
	return s.putJSON(id, value, indent, putAtomic, opts)
}

func (s *Store) putJSON(id string, value any, indent string, mode putMode, opts AtomicOptions) (PutResult, error) {
	ref, err := ReferenceForID(id)
	if err != nil {
		return PutResult{}, err
	}
	data, err := marshalJSONLine(value, indent)
	if err != nil {
		return PutResult{}, err
	}
	outcome, err := s.engine.put(id, data, mode, opts)
	result := PutResult{
		Reference:      ref,
		Created:        outcome.created,
		DiagnosticPath: outcome.diagnosticPath,
	}
	if err != nil {
		return result, err
	}
	if result.DiagnosticPath == "" {
		result.DiagnosticPath = ref.String()
	}
	return result, nil
}

func marshalJSONLine(value any, indent string) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", indent)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeFull(file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func writeOnce(writer io.Writer, data []byte) error {
	_, err := writer.Write(data)
	return err
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	syncErr := dir.Sync()
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) {
		return syncErr
	}
	return nil
}

// scanFamilyRestriction derives the exact set of segment family directories
// worth walking for opts.Prefixes, or nil (no restriction -- walk every
// family) when Prefixes is empty or any prefix fails to map cleanly onto the
// familyOf vocabulary (familyForPrefix, frame.go). Routing is a pure I/O
// optimization: the in-memory matchesPrefix post-filter below remains the
// sole authority on which records a caller actually receives, byte-for-byte
// identical to an unrestricted scan. nil is the fail-safe superset -- never
// guess-narrow.
func scanFamilyRestriction(prefixes []string) map[string]bool {
	if len(prefixes) == 0 {
		return nil
	}
	families := make(map[string]bool, len(prefixes))
	for _, prefix := range prefixes {
		family, ok := familyForPrefix(prefix)
		if !ok {
			return nil
		}
		families[family] = true
	}
	return families
}

// Scan validates segment truth, unions legacy loose records, and returns stable
// logical entries. Segment records win duplicate ids.
func (s *Store) Scan(opts ScanOptions) ([]Entry, error) {
	records, invalid, err := s.engine.scanAll(opts.RegularOnly, opts.RecentSegments, scanFamilyRestriction(opts.Prefixes))
	if err != nil {
		return nil, err
	}
	// Fail closed on an invalid loose filename that MATCHES the requested
	// prefixes: the caller asked for those records, so a name it cannot
	// reference is an error to surface, never an entry to silently skip.
	// Nonmatching invalid names stay ignorable (the caller never asked).
	for _, bad := range invalid {
		if matchesPrefix(strings.TrimSuffix(bad.name, ".json"), opts.Prefixes) {
			return nil, fmt.Errorf("invalid event scan entry %q: %w", bad.name, bad.err)
		}
	}
	entries := make([]Entry, 0, len(records))
	for id := range records {
		if !matchesPrefix(id, opts.Prefixes) {
			continue
		}
		ref, err := ReferenceForID(id)
		if err != nil {
			return nil, fmt.Errorf("invalid event scan entry %q: %w", id, err)
		}
		stored := records[id]
		entries = append(entries, Entry{
			ID:             id,
			Name:           id + ".json",
			Reference:      ref,
			cachedBytes:    stored.bytes,
			diagnosticPath: stored.diagnosticPath,
			store:          s,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// ScanPage is one incremental Scan result: the entries visible since the input
// cursor plus the opaque cursor to persist and pass back next run. A consumer
// binds a cursor to a fixed scan intent (Prefixes/RegularOnly); changing the
// query shape invalidates the cursor and it must be discarded (start empty).
type ScanPage struct {
	Entries []Entry
	Cursor  string
	// Resumed is true ONLY when opts.Cursor decoded into a usable cursor AND
	// the segment-authoritative scanFrom path consumed every segment strictly
	// from its recorded byte boundary — i.e. Entries is exactly the delta
	// since that cursor, never a superset. It is false for an
	// empty/garbage/version-mismatched cursor, for legacy-fallback delegation
	// to a full scan (migration gate open), and for a DECODABLE cursor any of
	// whose offsets forced a whole-segment reparse (offset past EOF, offset
	// mid-frame, segment replaced by a shorter file): those pages can
	// re-serve already-consumed frames. All of them still return a correct
	// (if less precise) result, so Resumed's zero value (false) is the
	// fail-safe default a caller can treat as "full rescan." A caller MUST
	// NOT skip its own dedup filter on Entries unless Resumed is true.
	Resumed bool
}

// ScanFrom returns the records appended since opts.Cursor and the next cursor.
// An empty (or unrecoverable) cursor yields a full deterministic scan — the
// cursor is a cache, never authority, so a lost or stale token always converges
// to the full-rescan answer rather than skipping records. On a home whose
// migration completion gate is still open the loose layer is unioned and no
// byte cursor is issued (Cursor comes back ""), preserving correctness while
// un-migrated. Existing full-snapshot callers keep using Scan unchanged.
func (s *Store) ScanFrom(opts ScanOptions) (ScanPage, error) {
	records, invalid, cursor, resumed, err := s.engine.scanFrom(opts.RegularOnly, opts.Cursor, scanFamilyRestriction(opts.Prefixes))
	if err != nil {
		return ScanPage{}, err
	}
	for _, bad := range invalid {
		if matchesPrefix(strings.TrimSuffix(bad.name, ".json"), opts.Prefixes) {
			return ScanPage{}, fmt.Errorf("invalid event scan entry %q: %w", bad.name, bad.err)
		}
	}
	entries := make([]Entry, 0, len(records))
	for id := range records {
		if !matchesPrefix(id, opts.Prefixes) {
			continue
		}
		ref, err := ReferenceForID(id)
		if err != nil {
			return ScanPage{}, fmt.Errorf("invalid event scan entry %q: %w", id, err)
		}
		stored := records[id]
		entries = append(entries, Entry{
			ID:             id,
			Name:           id + ".json",
			Reference:      ref,
			cachedBytes:    stored.bytes,
			diagnosticPath: stored.diagnosticPath,
			store:          s,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return ScanPage{Entries: entries, Cursor: cursor, Resumed: resumed}, nil
}

func matchesPrefix(id string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

func (e Entry) Read() (Record, error) {
	if e.store == nil {
		return Record{}, fmt.Errorf("event scan entry has no store")
	}
	if e.cachedBytes != nil {
		return Record{
			ID:             e.ID,
			Reference:      e.Reference,
			Bytes:          append([]byte(nil), e.cachedBytes...),
			DiagnosticPath: e.diagnosticPath,
		}, nil
	}
	return e.store.Read(e.ID)
}

func (s *Store) Count(opts ScanOptions) (int, error) {
	entries, err := s.Scan(opts)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// ScanFingerprint returns an opaque token identifying the physical state of the
// records Scan(opts) would read. A caller may use it to cheaply invalidate a
// read-side cache: while the token is unchanged, Scan(opts) returns identical
// entries; any append/seal/rotate or loose-record change yields a new token. It
// stats files rather than parsing them, so it is far cheaper than Scan, and —
// because it lives here, in the sole physical-layout owner — lets other packages
// build scan caches without ever learning the events layout. The token's format is
// opaque and must be compared, never parsed or persisted.
func (s *Store) ScanFingerprint(opts ScanOptions) string {
	return s.engine.scanFingerprint(opts.RegularOnly, scanFamilyRestriction(opts.Prefixes))
}

// Named compatibility streams remain physically separate from event records.
// Their paths stay private so callers cannot accidentally become a second
// physical owner while the work lifecycle retains its distinct semantics.
func (s *Store) workLogPath() string { return filepath.Join(s.rootPath(), workLogName) }

func (s *Store) workLogLockPath() string { return s.workLogPath() + ".lock" }

func (s *Store) hookEventsPath() string {
	return filepath.Join(s.rootPath(), hookEventsStreamName)
}

func (s *Store) tracesPath() string { return filepath.Join(s.rootPath(), tracesStreamName) }

func (s *Store) retrievalTrafficPath() string {
	return filepath.Join(s.rootPath(), retrievalTrafficStreamName)
}

// WithWorkLogReadLock serializes a read with active lifecycle writers without
// materializing the event habitat. A missing root or lock means there is no
// writer-established stream to coordinate with yet, so the callback reads the
// empty/legacy state directly.
func (s *Store) WithWorkLogReadLock(fn func() error) error {
	if fn == nil {
		return fmt.Errorf("work log lock callback required")
	}
	lock, err := os.OpenFile(s.workLogLockPath(), os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fn()
		}
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

// WithWorkLogLock serializes authoritative lifecycle readers and writers on
// the stable sidecar lock. Autonomy admission remains an outer concern of the
// work package, preserving admission -> work-log lock ordering.
func (s *Store) WithWorkLogLock(fn func() error) error {
	if fn == nil {
		return fmt.Errorf("work log lock callback required")
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.workLogLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

// AppendWorkLog performs one caller-supplied JSONL append and syncs the file.
// write is injectable so work's existing short-write and crash falsifiers keep
// observing the exact publication boundary without owning the physical path.
func (s *Store) AppendWorkLog(data []byte, write func(io.Writer, []byte) (int, error)) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	file, err := os.OpenFile(s.workLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if write == nil {
		write = func(w io.Writer, data []byte) (int, error) { return w.Write(data) }
	}
	n, err := write(file, data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	return file.Sync()
}

// WorkLogReplaceOptions preserves work's controlled append failpoints. Prefix
// bytes are copied completely before WriteSuffix writes the new event.
type WorkLogReplaceOptions struct {
	WriteSuffix   func(io.Writer, []byte) (int, error)
	BeforePublish func() error
	AfterPublish  func() error
}

// ReplaceWorkLog atomically publishes prefix+suffix via a synced sibling temp,
// rename, and directory sync. The caller is responsible for holding the work
// log lock and validating prefix before invoking this operation.
func (s *Store) ReplaceWorkLog(prefix, suffix []byte, opts WorkLogReplaceOptions) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	path := s.workLogPath()
	tmp, err := os.CreateTemp(s.rootPath(), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := writeFull(tmp, prefix); err != nil {
		return err
	}
	writeSuffix := opts.WriteSuffix
	if writeSuffix == nil {
		writeSuffix = func(w io.Writer, data []byte) (int, error) { return w.Write(data) }
	}
	n, err := writeSuffix(tmp, suffix)
	if err == nil && n != len(suffix) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if opts.BeforePublish != nil {
		if err := opts.BeforePublish(); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := syncDirectory(s.rootPath()); err != nil {
		return err
	}
	if opts.AfterPublish != nil {
		return opts.AfterPublish()
	}
	return nil
}

func (s *Store) ReadWorkLog() ([]byte, error) { return os.ReadFile(s.workLogPath()) }

func (s *Store) ReadWorkLogRegular(kind string) ([]byte, error) {
	return home.ReadRootedRegularFile(s.home, s.workLogPath(), kind)
}

// OpenWorkLog returns an O_RDONLY handle for scanners. It never creates the
// stream and never takes the authoritative writer lock.
func (s *Store) OpenWorkLog() (*os.File, error) { return os.Open(s.workLogPath()) }

func (s *Store) WorkLogSize() (int64, error) {
	info, err := os.Stat(s.workLogPath())
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *Store) ReadWorkLogAt(off, length int64) ([]byte, error) {
	if off < 0 || length < 0 {
		return nil, fmt.Errorf("work log read range must be non-negative")
	}
	if length == 0 {
		return nil, nil
	}
	file, err := s.OpenWorkLog()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buf := make([]byte, length)
	n, err := file.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

func (s *Store) OpenHookEvents() (*os.File, error) { return os.Open(s.hookEventsPath()) }

func (s *Store) HookEventsExists() (bool, error) {
	return regularFileExists(s.hookEventsPath())
}

func (s *Store) WorkLogExists() (bool, error) {
	return regularFileExists(s.workLogPath())
}

func (s *Store) OpenTraces() (*os.File, error) { return os.Open(s.tracesPath()) }

func (s *Store) OpenRetrievalTraffic() (*os.File, error) {
	return os.Open(s.retrievalTrafficPath())
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}

func (s *Store) AppendHookEvent(value any) error {
	return s.appendFrozenCompatibilityJSONL(s.hookEventsPath(), value)
}

func (s *Store) AppendTrace(value any) error {
	return s.appendFrozenCompatibilityJSONL(s.tracesPath(), value)
}

func (s *Store) AppendRetrievalTraffic(value any) error {
	return s.appendJSONL(s.retrievalTrafficPath(), value)
}

func (s *Store) appendJSONL(path string, value any) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return appendJSONLBytes(path, data)
}

func (s *Store) appendFrozenCompatibilityJSONL(path string, value any) (retErr error) {
	// completion.json is write-once. Once visible, the stream is permanently
	// retired and even invalid compatibility payloads are irrelevant.
	if s.engine.migrationComplete() {
		return nil
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	lock, err := s.engine.acquireRebaselineLockBlocking()
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.release(); retErr == nil && releaseErr != nil {
			retErr = releaseErr
		}
	}()
	if s.engine.migrationComplete() {
		return nil
	}
	return appendJSONLBytes(path, data)
}

func appendJSONLBytes(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(data, '\n'))
	return err
}
