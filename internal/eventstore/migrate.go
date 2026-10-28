package eventstore

// Legacy migration (write-shape slice 4). Folds the loose one-record
// events/<id>.json files that predate the segment engine into framed segments
// through the NORMAL append path, records a verifiable manifest of what moved,
// archives every original (convert-don't-delete), and closes a fail-closed
// completion gate. After the gate closes the hot read/scan path drops the loose
// fallback entirely (see legacyFallbackEnabled): migrated ids resolve in
// segments, a reappearing loose file is a legacy-writer regression the doctor
// surfaces, and a corrupt segment record fails closed rather than silently
// falling back to the archived copy.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	migrationDirName    = "migration"
	manifestFileName    = "manifest.jsonl"
	planFileName        = "plan.json"
	completionFileName  = "completion.json"
	migrateLockName     = ".migrate.lock"
	conflictsDirName    = "conflicts"
	archiveTopDirName   = "archives"
	archiveGeneration   = "event-record-files-v0"
	archiveFilesDirName = "files"
	defaultMigrateBatch = 500

	planSchema           = "compound.eventstore.migration.plan.v0.1"
	manifestSchema       = "compound.eventstore.migration.manifest_row.v0.1"
	completionSchema     = "compound.eventstore.migration.completion.v0.1"
	conflictSchema       = "compound.eventstore.migration.conflict.v0.1"
	streamBaselineSchema = "compound.eventstore.migration.stream_baseline.v0.1"

	statusMigrated       = "migrated"
	statusAlreadyPresent = "already_present"
	statusRejected       = "rejected"

	// streamBaselineFileName is the sidecar next to completion.json that records
	// each frozen compatibility stream's byte size at the moment it was trusted
	// (migration completion, or a later governed re-baseline). completion.json
	// itself stays write-once (see writeCompletion); this file is the ONE thing a
	// governed verb is allowed to rewrite.
	streamBaselineFileName = "stream-baseline.json"
	// rebaselineLockName guards every sidecar/journal transaction and the narrow
	// initial-baseline + completion publication boundary. It remains distinct
	// from migrateLockName's whole migration-run lock.
	rebaselineLockName        = ".rebaseline.lock"
	rebaselinePendingFileName = "rebaseline-pending.json"
	// RebaselinePendingSchema is the physical journal contract eventstore
	// validates before any rebaseline mutation can use pending bytes as authority.
	RebaselinePendingSchema = "compound.eventstore.rebaseline_pending.v0.1"

	streamBaselineReasonCompletion = "completion"
	streamBaselineReasonRebaseline = "rebaseline"
)

var (
	ErrRebaselineFinalizationPending = errors.New("event-store rebaseline finalization pending")
	ErrRebaselineStateConflict       = errors.New("event-store rebaseline state conflict")
	ErrRebaselineTransactionClosed   = errors.New("event-store rebaseline transaction is closed")
)

var syncRebaselineDirectoryFn = syncDirectory

// ErrMigrationConflict halts a run when a loose record and a segment record
// share an id but differ in content. Both bodies are preserved for the operator;
// the migration never guesses which one wins.
var ErrMigrationConflict = errors.New("event migration id conflict: same id, different content")

// MigrateMode selects the migration surface. Only Apply mutates state.
type MigrateMode int

const (
	ModeDryRun MigrateMode = iota
	ModeApply
	ModeVerify
)

// MigrateOptions parameterizes a single migration invocation. Unexported hooks
// are test-only fault injectors and phase observers so crash/race falsifiers can
// exercise exact durable boundaries.
type MigrateOptions struct {
	Mode                  MigrateMode
	BatchSize             int
	beforeManifest        func(id string) error
	beforeArchive         func(id string) error
	beforeInitialBaseline func() error
	afterRetirementCensus func() error
	beforeFinalization    func() error
	afterFinalPresence    func() error
}

// MigrateRejection names a loose file that could not be folded and why. A
// non-empty rejection set blocks completion until the operator resolves it.
type MigrateRejection struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// MigrateReport is the caller-facing outcome of one invocation.
type MigrateReport struct {
	Mode           string             `json:"mode"`
	Discovered     int                `json:"discovered"`
	Migrated       int                `json:"migrated"`
	AlreadyPresent int                `json:"already_present"`
	Rejected       int                `json:"rejected"`
	Archived       int                `json:"archived"`
	Conflicts      int                `json:"conflicts,omitempty"`
	ConflictIDs    []string           `json:"conflict_ids,omitempty"`
	PerFamily      map[string]int     `json:"per_family"`
	Rejections     []MigrateRejection `json:"rejections,omitempty"`
	Completion     CompletionReport   `json:"completion"`
}

// CompletionReport is the fail-closed gate. completion.json is written only when
// Complete is true. LegacyWriterActive is orthogonal to Complete: it reports a
// loose record that appeared AFTER the gate closed (a legacy-writer regression).
type CompletionReport struct {
	Schema             string         `json:"schema,omitempty"`
	Complete           bool           `json:"complete"`
	DiscoveredExpected int            `json:"legacy_discovered"`
	Migrated           int            `json:"migrated"`
	AlreadyPresent     int            `json:"already_present"`
	Rejected           int            `json:"rejected"`
	RemainingLoose     int            `json:"remaining_loose"`
	PerFamily          map[string]int `json:"per_family"`
	RollupDigest       string         `json:"rollup_digest"`
	ExpectedDigest     string         `json:"expected_digest"`
	Reasons            []string       `json:"reasons,omitempty"`
	LegacyWriterActive bool           `json:"legacy_writer_active"`
	RepairManifests    int            `json:"repair_manifests,omitempty"`
	RepairedRecords    int            `json:"repaired_records,omitempty"`
	DivergentArchived  int            `json:"divergent_archived,omitempty"`
	PostPlanRows       int            `json:"post_plan_rows,omitempty"`
	CompletedAt        string         `json:"completed_at,omitempty"`
}

type manifestRow struct {
	Schema          string `json:"schema"`
	ID              string `json:"id"`
	SourceSHA256    string `json:"source_sha256"`
	CanonicalSHA256 string `json:"canonical_sha256,omitempty"`
	Family          string `json:"family,omitempty"`
	Segment         string `json:"segment,omitempty"`
	Offset          int64  `json:"offset"`
	SchemaVersion   string `json:"schema_version,omitempty"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	RecordedAt      string `json:"recorded_at"`
}

type migratePlan struct {
	Schema          string         `json:"schema"`
	DiscoveredCount int            `json:"discovered_count"`
	PerFamily       map[string]int `json:"per_family"`
	ExpectedDigest  string         `json:"expected_digest"`
	CreatedAt       string         `json:"created_at"`
}

type conflictReport struct {
	Schema        string `json:"schema"`
	ID            string `json:"id"`
	LooseSHA256   string `json:"loose_canonical_sha256"`
	SegmentSHA256 string `json:"segment_canonical_sha256"`
	Segment       string `json:"segment"`
	DetectedAt    string `json:"detected_at"`
}

// segmentLocation is one id's authoritative physical coordinate + canonical hash
// as read from segment truth. It backs the O(1) presence/conflict decision so a
// bulk fold stays linear instead of re-scanning segments per record.
type segmentLocation struct {
	family  string
	relPath string
	offset  int64
	sha256  string
}

type looseFile struct {
	id   string
	name string
	path string
}

type migrator struct {
	store  *Store
	engine *engine
	opts   MigrateOptions
}

func (e *engine) migrationRoot() string  { return filepath.Join(e.eventsDir, migrationDirName) }
func (e *engine) manifestPath() string   { return filepath.Join(e.migrationRoot(), manifestFileName) }
func (e *engine) planPath() string       { return filepath.Join(e.migrationRoot(), planFileName) }
func (e *engine) completionPath() string { return filepath.Join(e.migrationRoot(), completionFileName) }
func (e *engine) streamBaselinePath() string {
	return filepath.Join(e.migrationRoot(), streamBaselineFileName)
}

func (e *engine) archiveRoot() string {
	return filepath.Join(e.home, archiveTopDirName, archiveGeneration, archiveFilesDirName)
}

// migrationComplete reports whether the fail-closed completion gate has closed.
// A stat error other than not-exist is treated as "not complete" so the loose
// fallback stays live rather than dropping records on a transient fault.
func (e *engine) migrationComplete() bool {
	_, err := os.Stat(e.completionPath())
	return err == nil
}

func (e *engine) legacyFallbackEnabled() bool { return !e.migrationComplete() }

// buildSegmentPresence indexes every segment record by id → coordinate, matching
// getByID resolution (newest segment wins; within a segment the last frame wins).
// A corrupt sealed segment surfaces its error here, so every gate built on this
// index fails closed. Torn hot tails are tolerated exactly as the read path does.
func (e *engine) buildSegmentPresence() (map[string]segmentLocation, error) {
	presence := map[string]segmentLocation{}
	setBy := map[string]int{}
	families, err := e.segmentFamilies()
	if err != nil {
		return nil, err
	}
	for _, family := range families {
		segments, err := e.familySegments(family) // newest-first
		if err != nil {
			return nil, err
		}
		for index, segment := range segments {
			frames, err := parseSegment(segment.path, index == 0)
			if err != nil {
				return nil, err
			}
			rel, relErr := filepath.Rel(e.eventsDir, segment.path)
			if relErr != nil {
				rel = segment.path
			}
			for _, pf := range frames {
				id := pf.Frame.ID
				if prev, ok := setBy[id]; ok && prev < index {
					continue // a newer segment already owns this id
				}
				presence[id] = segmentLocation{family: family, relPath: rel, offset: pf.Offset, sha256: pf.Frame.SHA256}
				setBy[id] = index
			}
		}
	}
	return presence, nil
}

// Migrate folds the loose legacy records under one home. It is the single entry
// point behind `compound event-store migrate`.
func (s *Store) Migrate(opts MigrateOptions) (MigrateReport, error) {
	if s == nil || s.engine == nil {
		return MigrateReport{}, fmt.Errorf("event store requires a home")
	}
	m := &migrator{store: s, engine: s.engine, opts: opts}
	if m.opts.BatchSize <= 0 {
		m.opts.BatchSize = defaultMigrateBatch
	}
	switch opts.Mode {
	case ModeVerify:
		return m.verify()
	case ModeDryRun:
		return m.dryRun()
	case ModeApply:
		return m.apply()
	default:
		return MigrateReport{}, fmt.Errorf("unknown migrate mode %d", int(opts.Mode))
	}
}

// MigrationComplete reports whether this home's completion gate has closed.
func (s *Store) MigrationComplete() bool {
	if s == nil || s.engine == nil {
		return false
	}
	return s.engine.migrationComplete()
}

// LegacyWriterActive is the post-completion doctor/status check: after the gate
// closes it is true when a legacy writer is still emitting through a retired
// path — either a loose events/<id>.json record has reappeared, or a frozen
// compatibility stream (hook-events.ndjson / traces.ndjson) was appended to
// after the cutover. Both classes are regressions the doctor surfaces.
func (s *Store) LegacyWriterActive() (bool, error) {
	if s == nil || s.engine == nil {
		return false, fmt.Errorf("event store requires a home")
	}
	if !s.engine.migrationComplete() {
		return false, nil
	}
	m := &migrator{store: s, engine: s.engine}
	loose, err := m.enumerateLoose()
	if err != nil {
		return false, err
	}
	if len(loose) > 0 {
		return true, nil
	}
	return s.engine.legacyStreamRegrew()
}

// legacyStreamRegrew reports whether a frozen compatibility stream was written
// after the completion gate closed. Post-migration, hook-events.ndjson and
// traces.ndjson are read through Scan and never appended again (writeHookEvent /
// recordTrace skip the append once migrationComplete).
//
// When a stream-baseline.json sidecar exists AND recorded a size for a given
// stream, that stream is judged by BYTE SIZE against the recorded baseline —
// any difference, growth or shrink, is a regression, and an unchanged size is
// never flagged regardless of mtime (a legitimate mtime bump, e.g. a backup
// restore or filesystem migration, no longer false-reds the doctor). A stream
// the sidecar has no entry for, or a legacy home without a sidecar, falls back
// to the mtime boundary. Only a timestamp strictly before completion proves
// that a stream was frozen: equality is ambiguous on coarse filesystem clocks
// and fails closed. Absent streams remain healthy.
func (e *engine) legacyStreamRegrew() (bool, error) {
	if err := e.failIfRebaselinePending(); err != nil {
		return false, err
	}
	marker, err := os.Stat(e.completionPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	boundary := marker.ModTime()
	baseline, hasBaseline, err := readStreamBaseline(e.streamBaselinePath())
	if err != nil {
		return false, err
	}
	for _, name := range []string{hookEventsStreamName, tracesStreamName} {
		info, err := os.Stat(filepath.Join(e.eventsDir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		if hasBaseline {
			if want, ok := baseline.Streams[name]; ok {
				if info.Size() != want {
					return true, nil
				}
				continue
			}
		}
		if !info.ModTime().Before(boundary) {
			return true, nil
		}
	}
	return false, nil
}

func (e *engine) failIfRebaselinePending() error {
	path := filepath.Join(e.migrationRoot(), rebaselinePendingFileName)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %s", ErrRebaselineFinalizationPending, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// StreamBaseline is the schema-stamped sidecar recording each frozen
// compatibility stream's byte size at the moment it was trusted: either the
// migration's own completion (Reason=="completion") or a later governed
// `compound event-store rebaseline --confirm` (Reason=="rebaseline"). Streams
// maps stream file name -> byte size; a stream absent from the map did not
// exist on disk when this baseline was recorded.
type StreamBaseline struct {
	Schema     string           `json:"schema"`
	RecordedAt string           `json:"recorded_at"`
	Reason     string           `json:"reason"`
	Streams    map[string]int64 `json:"streams"`
}

// currentStreamSizes stats the frozen compatibility streams right now. A
// stream that does not exist is simply absent from the returned map (never an
// error) — the same "absent is healthy" rule legacyStreamRegrew already
// applies for the mtime path.
func (e *engine) currentStreamSizes() (map[string]int64, error) {
	sizes := map[string]int64{}
	for _, name := range []string{hookEventsStreamName, tracesStreamName} {
		info, err := os.Stat(filepath.Join(e.eventsDir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		sizes[name] = info.Size()
	}
	return sizes, nil
}

// readStreamBaseline reads the sidecar if present. A missing file is reported
// as (zero value, false, nil) — never an error — since most homes (any legacy
// home completed before this change) legitimately have no sidecar yet.
func readStreamBaseline(path string) (StreamBaseline, bool, error) {
	pendingPath := filepath.Join(filepath.Dir(path), rebaselinePendingFileName)
	if _, err := os.Stat(pendingPath); err == nil {
		return StreamBaseline{}, false, fmt.Errorf("%w: %s", ErrRebaselineFinalizationPending, pendingPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return StreamBaseline{}, false, err
	}
	baseline, ok, err := readStreamBaselineRaw(path)
	if err != nil {
		return StreamBaseline{}, false, err
	}
	// The marker may have appeared between the first check and sidecar read.
	if _, statErr := os.Stat(pendingPath); statErr == nil {
		return StreamBaseline{}, false, fmt.Errorf("%w: %s", ErrRebaselineFinalizationPending, pendingPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return StreamBaseline{}, false, statErr
	}
	return baseline, ok, nil
}

func readStreamBaselineRaw(path string) (StreamBaseline, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StreamBaseline{}, false, nil
		}
		return StreamBaseline{}, false, err
	}
	var baseline StreamBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return StreamBaseline{}, false, fmt.Errorf("parse stream baseline %s: %w", path, err)
	}
	return baseline, true, nil
}

func writeBytesAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := writeFull(tmp, data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func writeBytesExclusiveAtomic(path string, data []byte) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if err := writeFull(tmp, data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	if err := syncDirectory(dir); err != nil {
		return false, err
	}
	return true, nil
}

// writeStreamBaselineAtomic writes the sidecar via tmp+rename (never O_EXCL —
// unlike completion.json this file IS meant to be rewritten, only ever through
// a governed writer) so a reader never observes a partially written file.
func writeStreamBaselineAtomic(path string, baseline StreamBaseline) error {
	data, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeBytesAtomic(path, data)
}

// acquireRebaselineLock guards the sidecar/journal transaction and the narrow
// initial-baseline + completion publication boundary. It never spans migration
// batches, but it prevents completion and a governed rebaseline from interleaving.
func (e *engine) acquireRebaselineLock() (*appendLock, error) {
	return e.acquireRebaselineLockWithMode(true)
}

// acquireRebaselineLockBlocking is used by compatibility-stream writers at
// the completion boundary. Live hooks wait for the short publication window,
// then recheck completion under the lock and no-op rather than failing.
func (e *engine) acquireRebaselineLockBlocking() (*appendLock, error) {
	return e.acquireRebaselineLockWithMode(false)
}

func (e *engine) acquireRebaselineLockWithMode(nonBlocking bool) (*appendLock, error) {
	if err := os.MkdirAll(e.migrationRoot(), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(e.migrationRoot(), rebaselineLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_EX
	if nonBlocking {
		mode |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), mode); err != nil {
		_ = file.Close()
		if !nonBlocking {
			return nil, fmt.Errorf("lock event-store rebaseline boundary: %w", err)
		}
		return nil, fmt.Errorf("another event-store re-baseline is already running: %w", err)
	}
	return &appendLock{file: file}, nil
}

// RebaselineTransaction is a lock-scoped handle. Callers cannot construct one,
// so journal, sidecar, and recovery operations share the same rebaseline lock.
type rebaselineTransactionState struct {
	mu      sync.Mutex
	active  bool
	pending []byte
	journal *rebaselinePhysicalJournal
}

type rebaselinePhysicalJournal struct {
	PriorExisted bool
	PreImageRef  string
	PostImageRef string
	Proofs       []rebaselinePhysicalProof
}

type rebaselinePhysicalProof struct {
	ID        string
	Canonical []byte
}

type rebaselinePhysicalJournalWire struct {
	SchemaVersion   string                           `json:"schema_version"`
	SidecarPath     string                           `json:"sidecar_path"`
	PriorExisted    bool                             `json:"prior_existed"`
	PreImageRef     string                           `json:"pre_image_ref"`
	PostImageRef    string                           `json:"post_image_ref"`
	AuthorityBundle RebaselinePendingAuthorityBundle `json:"authority_bundle"`
	Proofs          []RebaselinePendingProof         `json:"proofs"`
}

// RebaselinePendingAuthorityBundle is an opaque physical binding for the
// governance records an adapter needs to recover a rebaseline. Eventstore owns
// only the exact canonical JSON bytes and their digest; the adapter owns every
// schema and relationship inside Record.
type RebaselinePendingAuthorityBundle struct {
	RecordSHA256 string          `json:"record_sha256"`
	Record       json.RawMessage `json:"record"`
}

// NewRebaselinePendingAuthorityBundle binds an opaque authority record to its
// exact canonical JSON bytes without interpreting its contents.
func NewRebaselinePendingAuthorityBundle(record []byte) (RebaselinePendingAuthorityBundle, error) {
	canonicalRecord, err := canonical(record)
	if err != nil {
		return RebaselinePendingAuthorityBundle{}, fmt.Errorf("authority bundle record: %w", err)
	}
	return RebaselinePendingAuthorityBundle{RecordSHA256: rebaselineProofRecordHash(canonicalRecord), Record: canonicalRecord}, nil
}

// CanonicalRecord validates the physical hash binding and returns a copy of
// the canonical opaque record for the adapter that owns its meaning.
func (b RebaselinePendingAuthorityBundle) CanonicalRecord() ([]byte, error) {
	canonicalRecord, err := canonical(b.Record)
	if err != nil {
		return nil, fmt.Errorf("authority bundle record: %w", err)
	}
	if strings.TrimSpace(b.RecordSHA256) == "" {
		return nil, fmt.Errorf("authority bundle record_sha256 is required")
	}
	if b.RecordSHA256 != rebaselineProofRecordHash(canonicalRecord) {
		return nil, fmt.Errorf("authority bundle record_sha256 does not match canonical record bytes")
	}
	return append([]byte(nil), canonicalRecord...), nil
}

// RebaselinePendingProof is the eventstore-owned physical proof binding. The
// record body is deliberately opaque here: the adapter that owns its meaning
// validates the envelope. Eventstore only binds one generic event id to exact
// canonical JSON bytes and their digest.
type RebaselinePendingProof struct {
	ID           string          `json:"id"`
	RecordSHA256 string          `json:"record_sha256"`
	Record       json.RawMessage `json:"record"`
}

// NewRebaselinePendingProof builds an opaque physical proof binding without
// interpreting the record's schema or relationships.
func NewRebaselinePendingProof(id string, record []byte) (RebaselinePendingProof, error) {
	if _, err := ReferenceForID(id); err != nil {
		return RebaselinePendingProof{}, fmt.Errorf("proof id: %w", err)
	}
	canonicalRecord, err := canonical(record)
	if err != nil {
		return RebaselinePendingProof{}, fmt.Errorf("proof record: %w", err)
	}
	return RebaselinePendingProof{ID: id, RecordSHA256: rebaselineProofRecordHash(canonicalRecord), Record: canonicalRecord}, nil
}

type RebaselineTransaction struct {
	store *Store
	state *rebaselineTransactionState
}

func (s *Store) WithRebaselineTransaction(fn func(*RebaselineTransaction) error) (retErr error) {
	if s == nil || s.engine == nil {
		return fmt.Errorf("event store requires a home")
	}
	if fn == nil {
		return fmt.Errorf("rebaseline transaction callback required")
	}
	lock, err := s.engine.acquireRebaselineLock()
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.release(); retErr == nil && releaseErr != nil {
			retErr = releaseErr
		}
	}()
	tx := &RebaselineTransaction{store: s, state: &rebaselineTransactionState{active: true}}
	defer tx.deactivate()
	return fn(tx)
}

func (tx *RebaselineTransaction) deactivate() {
	if tx == nil || tx.state == nil {
		return
	}
	tx.state.mu.Lock()
	defer tx.state.mu.Unlock()
	tx.state.active = false
}

func (tx *RebaselineTransaction) lockActive() error {
	if tx == nil || tx.state == nil {
		return ErrRebaselineTransactionClosed
	}
	tx.state.mu.Lock()
	if !tx.state.active {
		tx.state.mu.Unlock()
		return ErrRebaselineTransactionClosed
	}
	return nil
}

func (tx *RebaselineTransaction) PendingPath() string {
	return filepath.Join(tx.store.engine.migrationRoot(), rebaselinePendingFileName)
}

func (tx *RebaselineTransaction) SidecarPath() string { return tx.store.engine.streamBaselinePath() }

func (s *Store) ReadRebaselinePending() ([]byte, bool, error) {
	if s == nil || s.engine == nil {
		return nil, false, fmt.Errorf("event store requires a home")
	}
	return readRebaselinePendingRaw(filepath.Join(s.engine.migrationRoot(), rebaselinePendingFileName))
}

func readRebaselinePendingRaw(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func (tx *RebaselineTransaction) ReadPending() ([]byte, bool, error) {
	if err := tx.lockActive(); err != nil {
		return nil, false, err
	}
	defer tx.state.mu.Unlock()
	pending, exists, err := readRebaselinePendingRaw(tx.PendingPath())
	if err != nil {
		return nil, false, err
	}
	tx.state.pending = nil
	tx.state.journal = nil
	if !exists {
		return pending, false, nil
	}
	journal, err := parseRebaselinePhysicalJournal(pending, tx.SidecarPath())
	if err != nil {
		return pending, true, err
	}
	tx.state.pending = append([]byte(nil), pending...)
	tx.state.journal = &journal
	return pending, true, nil
}

func (tx *RebaselineTransaction) WritePending(pending []byte) error {
	if err := tx.lockActive(); err != nil {
		return err
	}
	defer tx.state.mu.Unlock()
	journal, err := parseRebaselinePhysicalJournal(pending, tx.SidecarPath())
	if err != nil {
		return err
	}
	if _, exists, err := readRebaselinePendingRaw(tx.PendingPath()); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: durable rebaseline pending journal already exists", ErrRebaselineStateConflict)
	}
	created, err := writeBytesExclusiveAtomic(tx.PendingPath(), pending)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("%w: durable rebaseline pending journal already exists", ErrRebaselineStateConflict)
	}
	tx.state.pending = append([]byte(nil), pending...)
	tx.state.journal = &journal
	return nil
}

func (tx *RebaselineTransaction) ClearPending() error {
	if err := tx.lockActive(); err != nil {
		return err
	}
	defer tx.state.mu.Unlock()
	marker, journal, err := tx.requireExactPendingLocked()
	if err != nil {
		return err
	}
	if err := tx.requireClearTerminalStateLocked(journal); err != nil {
		return err
	}
	if err := os.Remove(tx.PendingPath()); err != nil {
		return err
	}
	if err := syncRebaselineDirectoryFn(filepath.Dir(tx.PendingPath())); err != nil {
		if restoreErr := writeBytesAtomic(tx.PendingPath(), marker); restoreErr != nil {
			return fmt.Errorf("sync pending removal: %v; restore exact pending marker: %w", err, restoreErr)
		}
		return fmt.Errorf("sync pending removal (exact marker restored): %w", err)
	}
	tx.state.pending = nil
	tx.state.journal = nil
	return nil
}

func (tx *RebaselineTransaction) ReadSidecar() ([]byte, bool, error) {
	if err := tx.lockActive(); err != nil {
		return nil, false, err
	}
	defer tx.state.mu.Unlock()
	return tx.readSidecarRaw()
}

func (tx *RebaselineTransaction) readSidecarRaw() ([]byte, bool, error) {
	data, err := os.ReadFile(tx.SidecarPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func (tx *RebaselineTransaction) FreshPreview() (StreamBaselinePreview, error) {
	if err := tx.lockActive(); err != nil {
		return StreamBaselinePreview{}, err
	}
	defer tx.state.mu.Unlock()
	return tx.store.previewStreamBaselineRaw()
}

func (tx *RebaselineTransaction) Plan(now time.Time) (StreamBaseline, []byte, error) {
	if err := tx.lockActive(); err != nil {
		return StreamBaseline{}, nil, err
	}
	defer tx.state.mu.Unlock()
	if !tx.store.engine.migrationComplete() {
		return StreamBaseline{}, nil, fmt.Errorf("event-store rebaseline: migration is not complete; nothing to re-baseline (run `compound event-store migrate --apply` first)")
	}
	sizes, err := tx.store.engine.currentStreamSizes()
	if err != nil {
		return StreamBaseline{}, nil, err
	}
	baseline := StreamBaseline{Schema: streamBaselineSchema, RecordedAt: now.UTC().Format(time.RFC3339Nano), Reason: streamBaselineReasonRebaseline, Streams: sizes}
	data, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return StreamBaseline{}, nil, err
	}
	return baseline, append(data, '\n'), nil
}

func (tx *RebaselineTransaction) WriteSidecarCAS(expected []byte, expectedExisted bool, replacement []byte) error {
	if err := tx.lockActive(); err != nil {
		return err
	}
	defer tx.state.mu.Unlock()
	_, journal, err := tx.requireExactPendingLocked()
	if err != nil {
		return err
	}
	if err := journal.requireImageBinding(expected, expectedExisted, replacement); err != nil {
		return err
	}
	if err := tx.requireProofsAbsentLocked(journal); err != nil {
		return err
	}
	current, existed, err := tx.readSidecarRaw()
	if err != nil {
		return err
	}
	if existed != expectedExisted || !bytes.Equal(current, expected) {
		return fmt.Errorf("%w: sidecar changed before write", ErrRebaselineStateConflict)
	}
	return writeBytesAtomic(tx.SidecarPath(), replacement)
}

// requireExactPendingLocked binds every physical rebaseline mutation to the
// exact durable journal bytes this active transaction wrote or read. The
// caller must hold tx.state.mu through lockActive.
func (tx *RebaselineTransaction) requireExactPendingLocked() ([]byte, *rebaselinePhysicalJournal, error) {
	if len(tx.state.pending) == 0 || tx.state.journal == nil {
		return nil, nil, fmt.Errorf("%w: validated durable rebaseline pending journal required before mutation", ErrRebaselineStateConflict)
	}
	pending, exists, err := readRebaselinePendingRaw(tx.PendingPath())
	if err != nil {
		return nil, nil, err
	}
	if !exists || !bytes.Equal(pending, tx.state.pending) {
		return nil, nil, fmt.Errorf("%w: rebaseline pending marker changed before mutation", ErrRebaselineStateConflict)
	}
	return pending, tx.state.journal, nil
}

func (tx *RebaselineTransaction) RollbackSidecarCAS(expectedPost, prior []byte, priorExisted bool) error {
	if err := tx.lockActive(); err != nil {
		return err
	}
	defer tx.state.mu.Unlock()
	_, journal, err := tx.requireExactPendingLocked()
	if err != nil {
		return err
	}
	if err := journal.requireImageBinding(prior, priorExisted, expectedPost); err != nil {
		return err
	}
	if err := tx.requireProofsAbsentLocked(journal); err != nil {
		return err
	}
	current, existed, err := tx.readSidecarRaw()
	if err != nil {
		return err
	}
	if !existed || !bytes.Equal(current, expectedPost) {
		return fmt.Errorf("%w: sidecar no longer matches planned post-image", ErrRebaselineStateConflict)
	}
	if priorExisted {
		return writeBytesAtomic(tx.SidecarPath(), prior)
	}
	if err := os.Remove(tx.SidecarPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(tx.SidecarPath()))
}

func (tx *RebaselineTransaction) VerifySidecarCAS(expected []byte) error {
	if err := tx.lockActive(); err != nil {
		return err
	}
	defer tx.state.mu.Unlock()
	current, exists, err := tx.readSidecarRaw()
	if err != nil {
		return err
	}
	if !exists || !bytes.Equal(current, expected) {
		return fmt.Errorf("%w: sidecar does not match expected post-image", ErrRebaselineStateConflict)
	}
	return nil
}

const rebaselineArtifactRefPrefix = "artifact:sha256:"

func parseRebaselinePhysicalJournal(raw []byte, expectedSidecar string) (rebaselinePhysicalJournal, error) {
	var wire rebaselinePhysicalJournalWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return rebaselinePhysicalJournal{}, fmt.Errorf("parse physical rebaseline journal: %w", err)
	}
	if wire.SchemaVersion != RebaselinePendingSchema {
		return rebaselinePhysicalJournal{}, fmt.Errorf("physical rebaseline journal schema_version=%q, want %q", wire.SchemaVersion, RebaselinePendingSchema)
	}
	if wire.SidecarPath != expectedSidecar || strings.TrimSpace(wire.SidecarPath) == "" {
		return rebaselinePhysicalJournal{}, fmt.Errorf("physical rebaseline journal sidecar_path=%q, want %q", wire.SidecarPath, expectedSidecar)
	}
	if err := validateRebaselineArtifactRef(wire.PreImageRef); err != nil {
		return rebaselinePhysicalJournal{}, fmt.Errorf("physical rebaseline journal pre_image_ref: %w", err)
	}
	if err := validateRebaselineArtifactRef(wire.PostImageRef); err != nil {
		return rebaselinePhysicalJournal{}, fmt.Errorf("physical rebaseline journal post_image_ref: %w", err)
	}
	if _, err := wire.AuthorityBundle.CanonicalRecord(); err != nil {
		return rebaselinePhysicalJournal{}, fmt.Errorf("physical rebaseline journal authority_bundle: %w", err)
	}
	if len(wire.Proofs) != 2 {
		return rebaselinePhysicalJournal{}, fmt.Errorf("physical rebaseline journal requires exactly two opaque proofs")
	}
	proofs := make([]rebaselinePhysicalProof, 0, len(wire.Proofs))
	seen := make(map[string]bool, len(wire.Proofs))
	for i := range wire.Proofs {
		proof := wire.Proofs[i]
		canonicalRecord, err := proof.validateAndCanonicalize()
		if err != nil {
			return rebaselinePhysicalJournal{}, fmt.Errorf("physical rebaseline journal proofs[%d]: %w", i, err)
		}
		if seen[proof.ID] {
			return rebaselinePhysicalJournal{}, fmt.Errorf("physical rebaseline journal duplicate proof id %q", proof.ID)
		}
		seen[proof.ID] = true
		proofs = append(proofs, rebaselinePhysicalProof{ID: proof.ID, Canonical: canonicalRecord})
	}
	return rebaselinePhysicalJournal{PriorExisted: wire.PriorExisted, PreImageRef: wire.PreImageRef, PostImageRef: wire.PostImageRef, Proofs: proofs}, nil
}

func (p RebaselinePendingProof) validateAndCanonicalize() ([]byte, error) {
	if _, err := ReferenceForID(p.ID); err != nil {
		return nil, fmt.Errorf("proof id: %w", err)
	}
	canonicalRecord, err := canonical(p.Record)
	if err != nil {
		return nil, fmt.Errorf("proof record: %w", err)
	}
	if strings.TrimSpace(p.RecordSHA256) == "" {
		return nil, fmt.Errorf("proof record_sha256 is required")
	}
	if p.RecordSHA256 != rebaselineProofRecordHash(canonicalRecord) {
		return nil, fmt.Errorf("proof record_sha256 does not match canonical record bytes")
	}
	return canonicalRecord, nil
}

func rebaselineProofRecordHash(canonicalRecord []byte) string {
	sum := sha256.Sum256(canonicalRecord)
	return fmt.Sprintf("sha256:%x", sum)
}

func validateRebaselineArtifactRef(ref string) error {
	if !strings.HasPrefix(ref, rebaselineArtifactRefPrefix) {
		return fmt.Errorf("artifact ref must start with %q", rebaselineArtifactRefPrefix)
	}
	digest := strings.TrimPrefix(ref, rebaselineArtifactRefPrefix)
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("artifact ref must carry a 64-character digest")
	}
	for _, r := range digest {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("artifact ref digest must be lowercase hexadecimal")
		}
	}
	return nil
}

func rebaselineArtifactRef(body []byte) string {
	sum := sha256.Sum256(body)
	return rebaselineArtifactRefPrefix + hex.EncodeToString(sum[:])
}

func (j *rebaselinePhysicalJournal) requireImageBinding(prior []byte, priorExisted bool, post []byte) error {
	if j == nil || j.PriorExisted != priorExisted || (!priorExisted && len(prior) != 0) || j.PreImageRef != rebaselineArtifactRef(prior) || j.PostImageRef != rebaselineArtifactRef(post) {
		return fmt.Errorf("%w: rebaseline mutation arguments do not match journaled pre/post images", ErrRebaselineStateConflict)
	}
	return nil
}

type rebaselinePhysicalProofPresence uint8

const (
	rebaselineProofAbsent rebaselinePhysicalProofPresence = iota
	rebaselineProofExact
	rebaselineProofConflict
)

func (tx *RebaselineTransaction) physicalProofPresenceLocked(proof rebaselinePhysicalProof) (rebaselinePhysicalProofPresence, error) {
	record, err := tx.store.Read(proof.ID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rebaselineProofAbsent, nil
		}
		return rebaselineProofConflict, err
	}
	compact, err := canonical(record.Bytes)
	if err != nil {
		return rebaselineProofConflict, nil
	}
	if bytes.Equal(compact, proof.Canonical) {
		return rebaselineProofExact, nil
	}
	return rebaselineProofConflict, nil
}

func (tx *RebaselineTransaction) journalProofPresenceLocked(journal *rebaselinePhysicalJournal) (rebaselinePhysicalProofPresence, error) {
	presence := rebaselineProofAbsent
	for i, proof := range journal.Proofs {
		current, err := tx.physicalProofPresenceLocked(proof)
		if err != nil {
			return rebaselineProofConflict, err
		}
		if current == rebaselineProofConflict {
			return rebaselineProofConflict, nil
		}
		if i == 0 {
			presence = current
			continue
		}
		if current != presence {
			return rebaselineProofConflict, nil
		}
	}
	return presence, nil
}

func (tx *RebaselineTransaction) requireProofsAbsentLocked(journal *rebaselinePhysicalJournal) error {
	presence, err := tx.journalProofPresenceLocked(journal)
	if err != nil {
		return err
	}
	if presence != rebaselineProofAbsent {
		return fmt.Errorf("%w: canonical rebaseline proof exists before physical mutation", ErrRebaselineStateConflict)
	}
	return nil
}

func (tx *RebaselineTransaction) requireClearTerminalStateLocked(journal *rebaselinePhysicalJournal) error {
	current, currentExisted, err := tx.readSidecarRaw()
	if err != nil {
		return err
	}
	proofs, err := tx.journalProofPresenceLocked(journal)
	if err != nil {
		return err
	}
	currentIsPre := currentExisted == journal.PriorExisted && ((!currentExisted && len(current) == 0) || (currentExisted && journal.PreImageRef == rebaselineArtifactRef(current)))
	currentIsPost := currentExisted && journal.PostImageRef == rebaselineArtifactRef(current)
	if currentIsPre && proofs == rebaselineProofAbsent {
		return nil
	}
	if currentIsPost && proofs == rebaselineProofExact {
		return nil
	}
	return fmt.Errorf("%w: rebaseline journal is not at an allowed terminal physical state (pre requires no proof; post requires all exact journal proofs)", ErrRebaselineStateConflict)
}

// StreamBaselinePreview is the current-vs-proposed projection for `compound
// event-store rebaseline` without --confirm. Current is nil when no sidecar
// exists yet; Proposed is what re-baselining right now would record. Building
// a preview performs no write.
type StreamBaselinePreview struct {
	HomeComplete bool
	SidecarPath  string
	HasSidecar   bool
	Current      map[string]int64
	Proposed     map[string]int64
}

// PreviewStreamBaseline computes the preview without mutating anything.
func (s *Store) PreviewStreamBaseline() (StreamBaselinePreview, error) {
	if s == nil || s.engine == nil {
		return StreamBaselinePreview{}, fmt.Errorf("event store requires a home")
	}
	if _, err := os.Stat(filepath.Join(s.engine.migrationRoot(), rebaselinePendingFileName)); err == nil {
		return StreamBaselinePreview{}, fmt.Errorf("%w: %s", ErrRebaselineFinalizationPending, filepath.Join(s.engine.migrationRoot(), rebaselinePendingFileName))
	} else if !errors.Is(err, os.ErrNotExist) {
		return StreamBaselinePreview{}, err
	}
	preview, err := s.previewStreamBaselineRaw()
	if err != nil {
		return StreamBaselinePreview{}, err
	}
	if _, err := os.Stat(filepath.Join(s.engine.migrationRoot(), rebaselinePendingFileName)); err == nil {
		return StreamBaselinePreview{}, fmt.Errorf("%w: %s", ErrRebaselineFinalizationPending, filepath.Join(s.engine.migrationRoot(), rebaselinePendingFileName))
	} else if !errors.Is(err, os.ErrNotExist) {
		return StreamBaselinePreview{}, err
	}
	return preview, nil
}

func (s *Store) previewStreamBaselineRaw() (StreamBaselinePreview, error) {
	if s == nil || s.engine == nil {
		return StreamBaselinePreview{}, fmt.Errorf("event store requires a home")
	}
	proposed, err := s.engine.currentStreamSizes()
	if err != nil {
		return StreamBaselinePreview{}, err
	}
	preview := StreamBaselinePreview{
		HomeComplete: s.engine.migrationComplete(),
		SidecarPath:  s.engine.streamBaselinePath(),
		Proposed:     proposed,
	}
	baseline, ok, err := readStreamBaselineRaw(preview.SidecarPath)
	if err != nil {
		return StreamBaselinePreview{}, err
	}
	if ok {
		preview.HasSidecar = true
		preview.Current = baseline.Streams
	}
	return preview, nil
}

func (m *migrator) now() string { return m.engine.clock().UTC().Format(time.RFC3339Nano) }

// enumerateLoose lists ONLY loose events/<id>.json record files. work.jsonl,
// hook-events.ndjson, traces.ndjson and the segments/index/migration/archives
// subdirectories are excluded by construction (subdirs and non-.json names),
// and dotfiles (in-flight atomic temps) are skipped.
func (m *migrator) enumerateLoose() ([]looseFile, error) {
	entries, err := os.ReadDir(m.engine.eventsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var files []looseFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		files = append(files, looseFile{id: id, name: name, path: filepath.Join(m.engine.eventsDir, name)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })
	return files, nil
}

func (m *migrator) apply() (MigrateReport, error) {
	if !m.engine.localFS {
		return MigrateReport{}, fmt.Errorf("legacy migration requires a local filesystem, detected %s", m.engine.fsType)
	}
	if err := m.store.Ensure(); err != nil {
		return MigrateReport{}, err
	}
	runLock, err := m.acquireRunLock()
	if err != nil {
		return MigrateReport{}, err
	}
	defer runLock.release()
	retired := m.engine.migrationComplete()
	if retired {
		loose, err := m.enumerateLoose()
		if err != nil {
			return MigrateReport{}, err
		}
		if len(loose) > 0 {
			return MigrateReport{Mode: "apply", Discovered: len(loose), PerFamily: map[string]int{}},
				fmt.Errorf("%w: %d loose records found; use `compound event-store repair plan`", ErrPostCompletionRepairRequired, len(loose))
		}
		if m.opts.afterRetirementCensus != nil {
			if err := m.opts.afterRetirementCensus(); err != nil {
				return MigrateReport{}, err
			}
		}
	}

	plan, err := m.loadOrCreatePlan()
	if err != nil {
		return MigrateReport{}, err
	}
	manifest, err := m.loadManifest()
	if err != nil {
		return MigrateReport{}, err
	}
	loose, err := m.enumerateLoose()
	if err != nil {
		return MigrateReport{}, err
	}
	if retired && len(loose) > 0 {
		return MigrateReport{Mode: "apply", Discovered: len(loose), PerFamily: map[string]int{}},
			fmt.Errorf("%w: %d loose records found; use `compound event-store repair plan`", ErrPostCompletionRepairRequired, len(loose))
	}

	report := MigrateReport{Mode: "apply", Discovered: plan.DiscoveredCount, PerFamily: map[string]int{}}
	for start := 0; start < len(loose); start += m.opts.BatchSize {
		end := min(start+m.opts.BatchSize, len(loose))
		if err := m.processBatch(loose[start:end], manifest, &report); err != nil {
			if errors.Is(err, ErrMigrationConflict) {
				report.Conflicts++
			}
			return report, err
		}
	}

	if m.opts.beforeFinalization != nil {
		if err := m.opts.beforeFinalization(); err != nil {
			return report, err
		}
	}
	comp, err := m.evaluateAndPublishCompletion(plan)
	if err != nil {
		return report, err
	}
	report.Completion = comp
	return report, nil
}

// processBatch holds the append writer lock for one bounded batch only (never
// the whole run) so live writers interleave between batches. It locks every
// family shard so its segment folds stay mutually exclusive with put's
// per-family appends.
func (m *migrator) processBatch(batch []looseFile, manifest map[string]manifestRow, report *MigrateReport) (retErr error) {
	releaseAll, err := m.engine.lockAllFamilyShards()
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, releaseAll())
	}()
	// The run-level presence map was built without the family mutation lock so
	// a cooperating Remove may have committed after that census. Rebuild only
	// after every family is locked; otherwise stale "already present" evidence
	// can archive the last surviving loose body after segment truth disappeared.
	presence, err := m.engine.buildSegmentPresence()
	if err != nil {
		return err
	}
	repaired := map[string]bool{}
	today := ingestionDay(m.engine.clock())
	for _, lf := range batch {
		if err := m.processRecord(lf, presence, manifest, repaired, today, report); err != nil {
			return err
		}
	}
	return nil
}

// processRecord folds one loose file. Ordering is segment-commit → manifest →
// archive, so every crash window is a resumable point: a segment-committed but
// manifest-absent record is seen already-present on retry and its manifest row
// is written; a manifest-present but unmoved original is archived on retry; an
// already-archived original is simply not re-enumerated.
func (m *migrator) processRecord(lf looseFile, presence map[string]segmentLocation, manifest map[string]manifestRow, repaired map[string]bool, today string, report *MigrateReport) error {
	data, err := os.ReadFile(lf.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // archived/removed by a prior run — nothing to do
		}
		return err
	}
	sourceSHA := recordHash(data)

	if regular, err := isRegularFile(lf.path); err != nil {
		return err
	} else if !regular {
		return m.recordRejected(lf, sourceSHA, manifest, report, "loose entry is not a regular file")
	}
	if _, err := ReferenceForID(lf.id); err != nil {
		return m.recordRejected(lf, sourceSHA, manifest, report, fmt.Sprintf("invalid event id: %v", err))
	}
	compact, err := canonical(data)
	if err != nil {
		return m.recordRejected(lf, sourceSHA, manifest, report, fmt.Sprintf("canonicalize: %v", err))
	}
	var header frameRecordHeader
	if err := json.Unmarshal(compact, &header); err != nil {
		return m.recordRejected(lf, sourceSHA, manifest, report, fmt.Sprintf("decode record header: %v", err))
	}
	if header.ID != lf.id {
		return m.recordRejected(lf, sourceSHA, manifest, report, fmt.Sprintf("embedded id %q does not match filename id %q", header.ID, lf.id))
	}
	canonSHA := recordHash(compact)
	family := familyOf(compact, lf.id)

	var segRel string
	var offset int64
	var status string
	if loc, ok := presence[lf.id]; ok {
		if loc.sha256 != canonSHA {
			return m.haltConflict(lf, data, canonSHA, loc)
		}
		segRel, offset, status = loc.relPath, loc.offset, statusAlreadyPresent
	} else {
		frame, _, err := buildFrame(lf.id, data)
		if err != nil {
			return m.recordRejected(lf, sourceSHA, manifest, report, fmt.Sprintf("build frame: %v", err))
		}
		line, err := encodeFrame(frame)
		if err != nil {
			return m.recordRejected(lf, sourceSHA, manifest, report, fmt.Sprintf("encode frame: %v", err))
		}
		if !repaired[family] {
			if last, exists, err := m.engine.lastWritten(family); err != nil {
				return err
			} else if exists {
				if err := m.engine.repairTail(last.path); err != nil {
					return err
				}
			}
			repaired[family] = true
		}
		rel, off, err := m.engine.appendFrameAt(family, today, line)
		if err != nil {
			return err
		}
		segRel, offset, status = rel, off, statusMigrated
		presence[lf.id] = segmentLocation{family: family, relPath: rel, offset: off, sha256: canonSHA}
	}

	row := manifestRow{
		Schema:          manifestSchema,
		ID:              lf.id,
		SourceSHA256:    sourceSHA,
		CanonicalSHA256: canonSHA,
		Family:          family,
		Segment:         segRel,
		Offset:          offset,
		SchemaVersion:   header.SchemaVersion,
		Status:          status,
		RecordedAt:      m.now(),
	}
	if existing, ok := manifest[lf.id]; !ok || existing.Status == statusRejected || existing.CanonicalSHA256 != canonSHA {
		if m.opts.beforeManifest != nil { // crash window (a): segment committed, manifest absent
			if err := m.opts.beforeManifest(lf.id); err != nil {
				return err
			}
		}
		if err := m.appendManifest(row); err != nil {
			return err
		}
		manifest[lf.id] = row
	}

	if m.opts.beforeArchive != nil { // crash window (b): manifest written, original not moved
		if err := m.opts.beforeArchive(lf.id); err != nil {
			return err
		}
	}
	if err := m.archiveLoose(lf, sourceSHA); err != nil {
		return err
	}

	switch status {
	case statusMigrated:
		report.Migrated++
		report.Archived++
		report.PerFamily[family]++
	case statusAlreadyPresent:
		report.AlreadyPresent++
		report.Archived++
		report.PerFamily[family]++
	}
	return nil
}

func (m *migrator) recordRejected(lf looseFile, sourceSHA string, manifest map[string]manifestRow, report *MigrateReport, reason string) error {
	row := manifestRow{
		Schema:       manifestSchema,
		ID:           lf.id,
		SourceSHA256: sourceSHA,
		Status:       statusRejected,
		Reason:       reason,
		RecordedAt:   m.now(),
	}
	if existing, ok := manifest[lf.id]; !ok || existing.Status != statusRejected || existing.Reason != reason {
		if err := m.appendManifest(row); err != nil {
			return err
		}
		manifest[lf.id] = row
	}
	report.Rejected++
	report.Rejections = append(report.Rejections, MigrateRejection{ID: lf.id, Reason: reason})
	// Rejected originals are deliberately left in place (not archived): they block
	// completion until the operator resolves them.
	return nil
}

// haltConflict preserves both bodies and stops the run. The loose original stays
// in place; the segment record and a conflict descriptor are copied beside it.
func (m *migrator) haltConflict(lf looseFile, looseData []byte, looseCanonSHA string, loc segmentLocation) error {
	dir := filepath.Join(m.engine.migrationRoot(), conflictsDirName, lf.id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(dir, "loose.json"), looseData); err != nil {
		return err
	}
	if hit, found, err := m.engine.segmentFrame(lf.id, ""); err == nil && found {
		if err := writeFileSync(filepath.Join(dir, "segment.json"), append(append([]byte(nil), hit.frame.Record...), '\n')); err != nil {
			return err
		}
	}
	detail := conflictReport{
		Schema:        conflictSchema,
		ID:            lf.id,
		LooseSHA256:   looseCanonSHA,
		SegmentSHA256: loc.sha256,
		Segment:       loc.relPath,
		DetectedAt:    m.now(),
	}
	payload, err := json.MarshalIndent(detail, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(dir, "conflict.json"), append(payload, '\n')); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s (loose canonical %s vs segment %s); both bodies preserved at %s",
		ErrMigrationConflict, lf.id, looseCanonSHA, loc.sha256, dir)
}

// archiveLoose atomically renames the original into the sharded archive. It is
// idempotent across every crash window: a vanished original with a present
// archive is a completed move; an identical archive already in place removes the
// original; a different archive is a hard error.
func (m *migrator) archiveLoose(lf looseFile, sourceSHA string) error {
	shard := recordHash([]byte(lf.id))[:2]
	dir := filepath.Join(m.engine.archiveRoot(), shard)
	dest := filepath.Join(dir, lf.id+".json")
	if _, err := os.Stat(lf.path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, derr := os.Stat(dest); derr == nil {
				return nil // already archived by a prior run
			}
			return fmt.Errorf("loose record %s vanished with no archive at %s", lf.id, dest)
		}
		return err
	}
	if existing, err := os.ReadFile(dest); err == nil {
		if recordHash(existing) == sourceSHA {
			return os.Remove(lf.path) // identical archive already present
		}
		return fmt.Errorf("archive %s already exists with different bytes", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Rename(lf.path, dest); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func (m *migrator) appendManifest(row manifestRow) error {
	if err := os.MkdirAll(m.engine.migrationRoot(), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(m.engine.manifestPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := writeFull(f, data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (m *migrator) loadManifest() (map[string]manifestRow, error) {
	rows := map[string]manifestRow{}
	data, err := os.ReadFile(m.engine.manifestPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rows, nil
		}
		return nil, err
	}
	trailingNewline := len(data) > 0 && data[len(data)-1] == '\n'
	parts := strings.Split(string(data), "\n")
	lastNonEmpty := -1
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			lastNonEmpty = i
			break
		}
	}
	for i, part := range parts {
		if part == "" {
			continue
		}
		var row manifestRow
		if err := json.Unmarshal([]byte(part), &row); err != nil {
			// A torn FINAL line (no trailing newline) is a crash mid-append and is
			// tolerated; a torn interior line is manifest corruption and surfaces.
			if i == lastNonEmpty && !trailingNewline {
				break
			}
			return nil, fmt.Errorf("event migration manifest line %d: %w", i+1, err)
		}
		if row.ID != "" {
			rows[row.ID] = row
		}
	}
	return rows, nil
}

func (m *migrator) loadOrCreatePlan() (migratePlan, error) {
	if plan, ok, err := m.loadPlan(); err != nil {
		return migratePlan{}, err
	} else if ok {
		return plan, nil
	}
	plan, err := m.computePlan()
	if err != nil {
		return migratePlan{}, err
	}
	if err := m.writePlanIfAbsent(plan); err != nil {
		return migratePlan{}, err
	}
	if stored, ok, rerr := m.loadPlan(); rerr == nil && ok {
		return stored, nil
	}
	return plan, nil
}

func (m *migrator) loadPlan() (migratePlan, bool, error) {
	data, err := os.ReadFile(m.engine.planPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return migratePlan{}, false, nil
		}
		return migratePlan{}, false, err
	}
	var plan migratePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return migratePlan{}, false, fmt.Errorf("decode migration plan: %w", err)
	}
	return plan, true, nil
}

// computePlan captures the immutable "what must end up migrated" contract from
// the full loose enumeration: discovered_count anchors the count gate and
// expected_digest anchors the sorted (id, canonical_sha256) rollup. Invalid /
// non-record files count toward discovered_count (they become rejected) but are
// excluded from the digest.
func (m *migrator) computePlan() (migratePlan, error) {
	loose, err := m.enumerateLoose()
	if err != nil {
		return migratePlan{}, err
	}
	perFamily := map[string]int{}
	var pairs []string
	for _, lf := range loose {
		data, err := os.ReadFile(lf.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return migratePlan{}, err
		}
		if regular, err := isRegularFile(lf.path); err != nil || !regular {
			continue
		}
		if _, err := ReferenceForID(lf.id); err != nil {
			continue
		}
		compact, err := canonical(data)
		if err != nil {
			continue
		}
		var header frameRecordHeader
		if err := json.Unmarshal(compact, &header); err != nil || header.ID != lf.id {
			continue
		}
		pairs = append(pairs, lf.id+" "+recordHash(compact))
		perFamily[familyOf(compact, lf.id)]++
	}
	sort.Strings(pairs)
	return migratePlan{
		Schema:          planSchema,
		DiscoveredCount: len(loose),
		PerFamily:       perFamily,
		ExpectedDigest:  recordHash([]byte(strings.Join(pairs, "\n"))),
		CreatedAt:       m.now(),
	}, nil
}

func (m *migrator) writePlanIfAbsent(plan migratePlan) error {
	if err := os.MkdirAll(m.engine.migrationRoot(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(m.engine.planPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil // a concurrent apply already wrote the plan
		}
		return err
	}
	if err := writeFull(f, data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDirectory(m.engine.migrationRoot())
}

// evaluateCompletion runs the fail-closed gate against durable state. It never
// writes. Every migrated/already-present manifest row must resolve to a segment
// record with a matching canonical hash (a missing or corrupt segment record
// refuses completion), the accounted count must equal the plan's discovered
// count, no loose files may remain, and the manifest rollup digest must equal
// the plan's expected digest.
func (m *migrator) evaluateCompletion(plan migratePlan, presence map[string]segmentLocation) (CompletionReport, error) {
	manifest, err := m.loadManifest()
	if err != nil {
		return CompletionReport{}, err
	}
	comp := CompletionReport{
		PerFamily:          map[string]int{},
		DiscoveredExpected: plan.DiscoveredCount,
		ExpectedDigest:     plan.ExpectedDigest,
	}
	// The frozen census ends at the published completion instant. A manifest
	// row recorded after it is runtime growth a pre-refusal binary folded in
	// (find_b642a87db247's class): it never joins the plan comparison, and it
	// verifies only while segment authority AND its archived original survive.
	var completedAt time.Time
	if data, readErr := os.ReadFile(m.engine.completionPath()); readErr == nil {
		var published CompletionReport
		if json.Unmarshal(data, &published) == nil && published.CompletedAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339Nano, published.CompletedAt); parseErr == nil {
				completedAt = parsed
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return CompletionReport{}, readErr
	}
	var pairs []string
	for id, row := range manifest {
		if !completedAt.IsZero() && row.RecordedAt != "" {
			if recordedAt, parseErr := time.Parse(time.RFC3339Nano, row.RecordedAt); parseErr == nil && recordedAt.After(completedAt) {
				comp.PostPlanRows++
				if row.Status != statusMigrated && row.Status != statusAlreadyPresent {
					comp.Reasons = append(comp.Reasons, fmt.Sprintf("post-plan manifest row %s has unresolved status %q", id, row.Status))
					continue
				}
				loc, ok := presence[id]
				if !ok {
					comp.Reasons = append(comp.Reasons, fmt.Sprintf("post-plan record %s is not present in any segment", id))
					continue
				}
				if loc.sha256 != row.CanonicalSHA256 {
					comp.Reasons = append(comp.Reasons, fmt.Sprintf("post-plan segment record %s canonical hash %s differs from manifest %s", id, loc.sha256, row.CanonicalSHA256))
					continue
				}
				shard := recordHash([]byte(id))[:2]
				original, archiveErr := os.ReadFile(filepath.Join(m.engine.archiveRoot(), shard, id+".json"))
				if archiveErr != nil {
					comp.Reasons = append(comp.Reasons, fmt.Sprintf("post-plan record %s has no readable archived original", id))
					continue
				}
				if recordHash(original) != row.SourceSHA256 {
					comp.Reasons = append(comp.Reasons, fmt.Sprintf("archived original for post-plan record %s does not match its source hash", id))
				}
				continue
			}
		}
		switch row.Status {
		case statusRejected:
			comp.Rejected++
			continue
		case statusMigrated:
			comp.Migrated++
		case statusAlreadyPresent:
			comp.AlreadyPresent++
		default:
			comp.Reasons = append(comp.Reasons, fmt.Sprintf("manifest row %s has unknown status %q", id, row.Status))
			continue
		}
		comp.PerFamily[row.Family]++
		pairs = append(pairs, id+" "+row.CanonicalSHA256)
		loc, ok := presence[id]
		if !ok {
			comp.Reasons = append(comp.Reasons, fmt.Sprintf("migrated record %s is not present in any segment", id))
			continue
		}
		if loc.sha256 != row.CanonicalSHA256 {
			comp.Reasons = append(comp.Reasons, fmt.Sprintf("segment record %s canonical hash %s differs from manifest %s", id, loc.sha256, row.CanonicalSHA256))
		}
	}
	sort.Strings(pairs)
	comp.RollupDigest = recordHash([]byte(strings.Join(pairs, "\n")))

	loose, err := m.enumerateLoose()
	if err != nil {
		return CompletionReport{}, err
	}
	comp.RemainingLoose = len(loose)

	if comp.Rejected != 0 {
		comp.Reasons = append(comp.Reasons, fmt.Sprintf("%d rejected records must be resolved", comp.Rejected))
	}
	accounted := comp.Migrated + comp.AlreadyPresent + comp.Rejected
	if accounted != plan.DiscoveredCount {
		comp.Reasons = append(comp.Reasons, fmt.Sprintf("accounted %d != legacy_discovered %d", accounted, plan.DiscoveredCount))
	}
	if comp.RemainingLoose != 0 {
		comp.Reasons = append(comp.Reasons, fmt.Sprintf("%d loose record files remain unmigrated", comp.RemainingLoose))
	}
	if comp.RollupDigest != plan.ExpectedDigest {
		comp.Reasons = append(comp.Reasons, "manifest (id, canonical_sha256) rollup digest does not equal the plan digest")
	}
	for family, want := range plan.PerFamily {
		if comp.PerFamily[family] != want {
			comp.Reasons = append(comp.Reasons, fmt.Sprintf("family %s: manifest count %d != plan count %d", family, comp.PerFamily[family], want))
		}
	}
	for family, got := range comp.PerFamily {
		if _, ok := plan.PerFamily[family]; !ok {
			comp.Reasons = append(comp.Reasons, fmt.Sprintf("family %s: %d migrated records absent from plan", family, got))
		}
	}
	repairManifests, repairedRecords, divergentArchived, repairReasons, err := m.store.verifyPostCompletionRepairManifests()
	if err != nil {
		return CompletionReport{}, err
	}
	comp.RepairManifests = repairManifests
	comp.RepairedRecords = repairedRecords
	comp.DivergentArchived = divergentArchived
	comp.Reasons = append(comp.Reasons, repairReasons...)
	comp.Complete = len(comp.Reasons) == 0
	// A legacy writer is active post-gate when a loose record reappeared OR a
	// frozen compatibility stream (hook-events.ndjson / traces.ndjson) was
	// appended to after the gate closed. Both classes surface degraded state on
	// `migrate --verify`. During the FIRST apply the gate is not yet closed, so
	// legacyStreamRegrew short-circuits to false.
	comp.LegacyWriterActive = m.engine.migrationComplete() && comp.RemainingLoose > 0
	if m.engine.migrationComplete() && !comp.LegacyWriterActive {
		regrew, err := m.engine.legacyStreamRegrew()
		if err != nil {
			return CompletionReport{}, err
		}
		comp.LegacyWriterActive = regrew
	}
	if comp.Complete {
		comp.CompletedAt = m.now()
	}
	return comp, nil
}

func (m *migrator) verify() (MigrateReport, error) {
	plan, ok, err := m.loadPlan()
	if err != nil {
		return MigrateReport{}, err
	}
	presence, err := m.engine.buildSegmentPresence()
	if err != nil {
		return MigrateReport{}, err
	}
	comp, err := m.evaluateCompletion(plan, presence)
	if err != nil {
		return MigrateReport{}, err
	}
	if !ok {
		comp.Complete = false
		comp.Reasons = append([]string{"no migration plan found; run `compound event-store migrate --apply` first"}, comp.Reasons...)
	}
	return MigrateReport{
		Mode:           "verify",
		Discovered:     comp.DiscoveredExpected,
		Migrated:       comp.Migrated,
		AlreadyPresent: comp.AlreadyPresent,
		Rejected:       comp.Rejected,
		PerFamily:      comp.PerFamily,
		Completion:     comp,
	}, nil
}

func (m *migrator) dryRun() (MigrateReport, error) {
	loose, err := m.enumerateLoose()
	if err != nil {
		return MigrateReport{}, err
	}
	presence, err := m.engine.buildSegmentPresence()
	if err != nil {
		return MigrateReport{}, err
	}
	report := MigrateReport{Mode: "dry-run", Discovered: len(loose), PerFamily: map[string]int{}}
	for _, lf := range loose {
		data, err := os.ReadFile(lf.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return MigrateReport{}, err
		}
		reason := ""
		var compact []byte
		var header frameRecordHeader
		if regular, rerr := isRegularFile(lf.path); rerr != nil {
			return MigrateReport{}, rerr
		} else if !regular {
			reason = "loose entry is not a regular file"
		} else if _, err := ReferenceForID(lf.id); err != nil {
			reason = fmt.Sprintf("invalid event id: %v", err)
		} else if compact, err = canonical(data); err != nil {
			reason = fmt.Sprintf("canonicalize: %v", err)
		} else if err := json.Unmarshal(compact, &header); err != nil {
			reason = fmt.Sprintf("decode record header: %v", err)
		} else if header.ID != lf.id {
			reason = fmt.Sprintf("embedded id %q does not match filename id %q", header.ID, lf.id)
		}
		if reason != "" {
			report.Rejected++
			report.Rejections = append(report.Rejections, MigrateRejection{ID: lf.id, Reason: reason})
			continue
		}
		canonSHA := recordHash(compact)
		family := familyOf(compact, lf.id)
		if loc, ok := presence[lf.id]; ok {
			if loc.sha256 != canonSHA {
				report.Conflicts++
				report.ConflictIDs = append(report.ConflictIDs, lf.id)
				continue
			}
			report.AlreadyPresent++
			report.PerFamily[family]++
			continue
		}
		report.Migrated++
		report.PerFamily[family]++
	}
	return report, nil
}

// evaluateAndPublishCompletion preserves the process-wide lock order shared
// with WithRebaselineTransaction: rebaseline first, then all event families.
// The final segment census, completion evaluation, initial stream baseline,
// and completion.json publication are one exclusive boundary.
func (m *migrator) evaluateAndPublishCompletion(plan migratePlan) (comp CompletionReport, retErr error) {
	if err := os.MkdirAll(m.engine.migrationRoot(), 0o755); err != nil {
		return CompletionReport{}, err
	}
	rebaseline, err := m.engine.acquireRebaselineLock()
	if err != nil {
		return CompletionReport{}, err
	}
	defer func() {
		retErr = errors.Join(retErr, rebaseline.release())
	}()
	releaseAll, err := m.engine.lockAllFamilyShards()
	if err != nil {
		return CompletionReport{}, err
	}
	defer func() {
		retErr = errors.Join(retErr, releaseAll())
	}()

	finalPresence, err := m.engine.buildSegmentPresence()
	if err != nil {
		return CompletionReport{}, err
	}
	if m.opts.afterFinalPresence != nil {
		if err := m.opts.afterFinalPresence(); err != nil {
			return CompletionReport{}, err
		}
	}
	comp, err = m.evaluateCompletion(plan, finalPresence)
	if err != nil {
		return CompletionReport{}, err
	}
	if comp.Complete && !m.engine.migrationComplete() {
		if err := m.writeCompletionLocked(comp); err != nil {
			return CompletionReport{}, err
		}
	}
	return comp, nil
}

// writeCompletionLocked publishes the baseline and completion marker while
// its caller holds both the rebaseline lock and every family mutation lock.
func (m *migrator) writeCompletionLocked(comp CompletionReport) error {
	if err := os.MkdirAll(m.engine.migrationRoot(), 0o755); err != nil {
		return err
	}
	if _, exists, err := readRebaselinePendingRaw(filepath.Join(m.engine.migrationRoot(), rebaselinePendingFileName)); err != nil {
		return err
	} else if exists {
		return ErrRebaselineFinalizationPending
	}
	if m.opts.beforeInitialBaseline != nil {
		if err := m.opts.beforeInitialBaseline(); err != nil {
			return err
		}
	}
	if err := m.engine.writeInitialStreamBaseline(m.now()); err != nil {
		return err
	}
	// completion.json is the externally visible gate and is deliberately
	// published LAST, after the initial baseline is durable under the same lock.
	comp.Schema = completionSchema
	data, err := json.MarshalIndent(comp, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = writeBytesExclusiveAtomic(m.engine.completionPath(), data)
	return err
}

// writeInitialStreamBaseline records the frozen streams' sizes AT completion
// time. writeCompletion calls it while holding both the migration run lock and
// the rebaseline boundary lock, with completion.json still absent. A prior
// reason=completion sidecar is therefore a provisional image from an interrupted
// publication and is refreshed to current sizes. Any other reason belongs to a
// governed writer (or is unexpected) and must never be overwritten here.
func (e *engine) writeInitialStreamBaseline(recordedAt string) error {
	sizes, err := e.currentStreamSizes()
	if err != nil {
		return err
	}
	baseline := StreamBaseline{
		Schema:     streamBaselineSchema,
		RecordedAt: recordedAt,
		Reason:     streamBaselineReasonCompletion,
		Streams:    sizes,
	}
	current, exists, err := readStreamBaselineRaw(e.streamBaselinePath())
	if err != nil {
		return err
	}
	if exists {
		if current.Schema != streamBaselineSchema {
			return fmt.Errorf("%w: completion refuses existing stream baseline schema %q", ErrRebaselineStateConflict, current.Schema)
		}
		if current.Reason != streamBaselineReasonCompletion {
			return fmt.Errorf("%w: completion refuses existing stream baseline reason %q", ErrRebaselineStateConflict, current.Reason)
		}
		return writeStreamBaselineAtomic(e.streamBaselinePath(), baseline)
	}
	data, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return err
	}
	created, err := writeBytesExclusiveAtomic(e.streamBaselinePath(), append(data, '\n'))
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("%w: stream baseline appeared during completion publication", ErrRebaselineStateConflict)
	}
	return nil
}

// acquireRunLock guards against two concurrent migrations. It is a whole-run
// lock distinct from the per-batch append writer lock, so it never blocks live
// writers — only a second migrator.
func (m *migrator) acquireRunLock() (*appendLock, error) {
	return m.acquireRunLockWithMode(true)
}

func (m *migrator) acquireRunLockBlocking() (*appendLock, error) {
	return m.acquireRunLockWithMode(false)
}

func (m *migrator) acquireRunLockWithMode(nonBlocking bool) (*appendLock, error) {
	if err := os.MkdirAll(m.engine.migrationRoot(), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(m.engine.migrationRoot(), migrateLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_EX
	if nonBlocking {
		mode |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), mode); err != nil {
		_ = file.Close()
		if nonBlocking {
			return nil, fmt.Errorf("another event-store migration is already running: %w", err)
		}
		return nil, fmt.Errorf("lock event-store migration owner: %w", err)
	}
	return &appendLock{file: file}, nil
}

func writeFileSync(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := writeFull(f, data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func isRegularFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}
