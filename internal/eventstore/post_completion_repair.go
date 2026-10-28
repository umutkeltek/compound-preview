package eventstore

// Governed post-completion repair for loose event records emitted by a retired
// writer after completion.json closed the legacy migration. The original
// migration plan/manifest/completion remain immutable. Each repair adds a
// sealed plan -> current PolicyDecision -> content-addressed repair manifest.
// Segment truth is never replaced: a loose-only id is appended through the normal
// frame format, while identical and divergent duplicates are archived and the
// existing segment body remains authoritative.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	homepkg "compound/internal/home"
)

var (
	ErrPostCompletionRepairRequired           = errors.New("post-completion loose records require governed event-store repair")
	postCompletionRepairSupersessionAfterFile = func() error { return nil }
)

const (
	postCompletionRepairDirName                 = "post-completion-repairs"
	postCompletionRepairArchiveGen              = "event-record-repairs-v1"
	postCompletionRepairPlanSchema              = "compound.eventstore.post_completion_repair_plan.v0.1"
	postCompletionRepairReviewSchema            = "compound.eventstore.post_completion_repair_review.v0.1"
	postCompletionRepairSupersessionSchema      = "compound.eventstore.post_completion_repair_supersession.v0.1"
	postCompletionRepairSupersessionProofSchema = "compound.eventstore.post_completion_repair_supersession_proof.v0.1"
	postCompletionRepairPendingSchema           = "compound.eventstore.post_completion_repair_pending.v0.1"
	postCompletionRepairProgressSchema          = "compound.eventstore.post_completion_repair_progress.v0.1"
	postCompletionRepairTransitionSchema        = "compound.eventstore.post_completion_repair_transition.v0.1"
	postCompletionRepairResultSchema            = "compound.eventstore.migration.repair_manifest.v0.1"
	postCompletionRepairProofSchema             = "compound.eventstore.post_completion_repair_proof.v0.1"
	postCompletionRepairRootSchema              = "compound.eventstore.post_completion_repair_root.v0.1"
	postCompletionRepairAnchorSchema            = "compound.eventstore.post_completion_repair_anchor.v0.1"
	postCompletionRepairPolicySchema            = "compound.policy_decision.v0.1"
	postCompletionRepairRootFile                = "root.json"
	postCompletionRepairAnchorFile              = "post-completion-repair-anchor.json"
	RepairImportAbsent                          = "import_absent"
	RepairArchiveIdentical                      = "archive_identical"
	RepairArchiveDivergent                      = "archive_divergent"
	postCompletionRepairVerificationRef         = "falsifier:internal/eventstore/post_completion_repair_test.go#mixed-drift-interruption-replay"
	repairPhasePlanned                          = "planned"
	repairPhaseAppendReserved                   = "append_reserved"
	repairPhaseAppended                         = "appended"
	repairPhaseFrameVerified                    = "frame_verified"
	repairPhaseRemoveReserved                   = "remove_reserved"
	repairPhaseClaimed                          = "claimed"
	repairPhaseRemoved                          = "removed"
	defaultPostCompletionObservation            = 250 * time.Millisecond
	maxPostCompletionRepairJournalLine          = 4096
)

// PostCompletionRepairEntry seals one loose body and the segment truth observed
// for its id. RawSHA256 binds every original byte; CanonicalSHA256 is the body
// identity used by frames. A divergent entry deliberately records both hashes.
type PostCompletionRepairEntry struct {
	ID              string `json:"id"`
	FileName        string `json:"file_name"`
	RawSHA256       string `json:"raw_sha256"`
	CanonicalSHA256 string `json:"canonical_sha256"`
	SchemaVersion   string `json:"schema_version"`
	Family          string `json:"family"`
	Disposition     string `json:"disposition"`
	SegmentSHA256   string `json:"segment_sha256,omitempty"`
	Segment         string `json:"segment,omitempty"`
	SegmentOffset   int64  `json:"segment_offset,omitempty"`
}

type PostCompletionRepairBaseline struct {
	MigrationPlanSHA256 string `json:"migration_plan_sha256"`
	ManifestSHA256      string `json:"manifest_sha256"`
	CompletionSHA256    string `json:"completion_sha256"`
}

type PostCompletionRepairPlan struct {
	SchemaVersion string                          `json:"schema_version"`
	ID            string                          `json:"id"`
	CreatedAt     string                          `json:"created_at"`
	Status        string                          `json:"status"`
	SourceDigest  string                          `json:"source_digest"`
	Revision      string                          `json:"revision"`
	PlanDigest    string                          `json:"plan_digest"`
	WriterFence   PostCompletionRepairWriterFence `json:"writer_fence"`
	Baseline      PostCompletionRepairBaseline    `json:"baseline"`
	Total         int                             `json:"total"`
	Counts        map[string]int                  `json:"counts"`
	Entries       []PostCompletionRepairEntry     `json:"entries"`
	EvidenceRefs  []string                        `json:"evidence_refs"`
}

// PostCompletionRepairWriterFence is bounded positive evidence, not a claim
// that an external process is permanently stopped. Planning reads the complete
// loose/compatibility-stream source twice around classification; review repeats
// that observation and the operator attests that the retired writer was
// stopped. Apply revalidates the reviewed digest before every new mutation.
type PostCompletionRepairWriterFence struct {
	StartedAt         string `json:"started_at"`
	CompletedAt       string `json:"completed_at"`
	BeforeDigest      string `json:"before_digest"`
	AfterDigest       string `json:"after_digest"`
	Stable            bool   `json:"stable"`
	ObservationMillis int64  `json:"observation_millis"`
}

type PostCompletionRepairReview struct {
	SchemaVersion         string `json:"schema_version"`
	ID                    string `json:"id"`
	CreatedAt             string `json:"created_at"`
	PlanID                string `json:"plan_id"`
	PlanDigest            string `json:"plan_digest"`
	PlanRevision          string `json:"plan_revision"`
	Reviewer              string `json:"reviewer"`
	Decision              string `json:"decision"`
	WriterStoppedAttested bool   `json:"writer_stopped_attested"`
	WriterFenceDigest     string `json:"writer_fence_digest"`
	ObservationMillis     int64  `json:"observation_millis"`
	ReviewDigest          string `json:"review_digest"`
}

type PostCompletionRepairReviewRecord struct {
	Review     PostCompletionRepairReview `json:"review"`
	ReviewPath string                     `json:"review_path"`
}

// PostCompletionRepairSupersession is the immutable terminal for a reviewed
// plan whose source set broadened before any progress boundary existed. It
// preserves the old plan and review as history while binding one exact broader
// successor through the same work/action/policy authorization seam.
type PostCompletionRepairSupersession struct {
	SchemaVersion         string                            `json:"schema_version"`
	ID                    string                            `json:"id"`
	CreatedAt             string                            `json:"created_at"`
	PlanID                string                            `json:"plan_id"`
	PlanDigest            string                            `json:"plan_digest"`
	ReviewID              string                            `json:"review_id"`
	ReviewDigest          string                            `json:"review_digest"`
	Authorization         PostCompletionRepairAuthorization `json:"authorization"`
	SuccessorPlanID       string                            `json:"successor_plan_id"`
	SuccessorPlanDigest   string                            `json:"successor_plan_digest"`
	SuccessorSourceDigest string                            `json:"successor_source_digest"`
	SupersessionDigest    string                            `json:"supersession_digest"`
}

// PostCompletionRepairSupersessionProof is published only after the work
// owner durably records the exact ActionIntent, PolicyDecision, Receipt, and
// Transition for a zero-progress supersession. The supersession artifact is
// physical terminal evidence; this sidecar is the governance completion gate.
type PostCompletionRepairSupersessionProof struct {
	SchemaVersion      string                            `json:"schema_version"`
	PlanID             string                            `json:"plan_id"`
	PlanDigest         string                            `json:"plan_digest"`
	SupersessionID     string                            `json:"supersession_id"`
	SupersessionDigest string                            `json:"supersession_digest"`
	Authorization      PostCompletionRepairAuthorization `json:"authorization"`
	ReceiptID          string                            `json:"receipt_id"`
	TransitionID       string                            `json:"transition_id"`
	RecordedAt         string                            `json:"recorded_at"`
	ProofDigest        string                            `json:"proof_digest"`
}

type PostCompletionRepairArtifact struct {
	Kind                        string `json:"kind"`
	ID                          string `json:"id"`
	Path                        string `json:"path"`
	SHA256                      string `json:"sha256"`
	AuthorizationCount          int    `json:"authorization_count,omitempty"`
	AuthorizationDigest         string `json:"authorization_digest,omitempty"`
	AuthorizationSequence       uint64 `json:"authorization_sequence,omitempty"`
	AuthorizationTransitionHead string `json:"authorization_transition_head,omitempty"`
}

// PostCompletionRepairRoot is the durable rollup owner. Directory discovery
// is only a falsifier; current repair identity comes from this digest-bound
// inventory, so deleting a result/proof cannot silently turn history into zero.
type PostCompletionRepairRoot struct {
	SchemaVersion     string                         `json:"schema_version"`
	Revision          uint64                         `json:"revision"`
	PreviousRoot      string                         `json:"previous_root_digest,omitempty"`
	PreviousChainHead string                         `json:"previous_chain_head,omitempty"`
	Artifacts         []PostCompletionRepairArtifact `json:"artifacts"`
	ChainHead         string                         `json:"chain_head"`
	RootDigest        string                         `json:"root_digest"`
}

// PostCompletionRepairAnchor lives beside the repair directory, under the
// existing migration authority. It prevents deletion of the entire repair
// subtree from making completed repair history appear to be an initial zero.
type PostCompletionRepairAnchor struct {
	SchemaVersion string `json:"schema_version"`
	Revision      uint64 `json:"revision"`
	ArtifactCount int    `json:"artifact_count"`
	RootDigest    string `json:"root_digest"`
	ChainHead     string `json:"chain_head"`
	AnchorDigest  string `json:"anchor_digest"`
}

type PostCompletionRepairPlanRecord struct {
	Plan     PostCompletionRepairPlan `json:"plan"`
	PlanPath string                   `json:"plan_path"`
}

// PostCompletionRepairAuthorization is the physical owner's opaque binding to
// the existing execution spine. cmd/compound supplies ids from the exact
// ActionIntent and allowing PolicyDecision reserved on the running work.
type PostCompletionRepairAuthorization struct {
	WorkID            string          `json:"work_id"`
	Actor             string          `json:"actor"`
	ActionIntentID    string          `json:"action_intent_id"`
	PolicyDecisionID  string          `json:"policy_decision_id"`
	AuthorityRevision string          `json:"authority_revision"`
	PlanDigest        string          `json:"plan_digest"`
	ReviewID          string          `json:"review_id"`
	ReviewDigest      string          `json:"review_digest"`
	WriterFenceDigest string          `json:"writer_fence_digest"`
	AuthorizedAt      string          `json:"authorized_at,omitempty"`
	SealedAction      json.RawMessage `json:"sealed_action_intent,omitempty"`
	SealedDecision    json.RawMessage `json:"sealed_policy_decision,omitempty"`
}

type PostCompletionRepairPending struct {
	SchemaVersion string                              `json:"schema_version"`
	PlanID        string                              `json:"plan_id"`
	PlanDigest    string                              `json:"plan_digest"`
	Authorization PostCompletionRepairAuthorization   `json:"authorization"`
	StartedAt     string                              `json:"started_at"`
	Entries       []PostCompletionRepairProgressEntry `json:"entries"`
	PendingDigest string                              `json:"pending_digest"`
}

type PostCompletionRepairProgressEntry struct {
	ID              string `json:"id"`
	Phase           string `json:"phase"`
	Segment         string `json:"segment,omitempty"`
	SegmentOffset   int64  `json:"segment_offset,omitempty"`
	CanonicalSHA256 string `json:"canonical_sha256,omitempty"`
}

// PostCompletionRepairProgressHeader is the immutable first journal record.
// The repair root binds these exact header bytes once; phase records extend
// the file without changing the rooted journal identity.
type PostCompletionRepairProgressHeader struct {
	SchemaVersion string `json:"schema_version"`
	PlanID        string `json:"plan_id"`
	PlanDigest    string `json:"plan_digest"`
	PendingDigest string `json:"pending_digest"`
	StartedAt     string `json:"started_at"`
	HeaderDigest  string `json:"header_digest"`
}

// PostCompletionRepairProgressTransition is one hash-chained phase move.
// Sequence is global to the repair so replay rejects reordering, gaps, and
// branching even when transitions concern different plan entries.
type PostCompletionRepairProgressTransition struct {
	SchemaVersion               string                             `json:"schema_version"`
	Kind                        string                             `json:"kind,omitempty"`
	Sequence                    uint64                             `json:"sequence"`
	PlanID                      string                             `json:"plan_id"`
	PendingDigest               string                             `json:"pending_digest"`
	EntryIndex                  int                                `json:"entry_index"`
	EntryID                     string                             `json:"entry_id"`
	FromPhase                   string                             `json:"from_phase"`
	ToPhase                     string                             `json:"to_phase"`
	Segment                     string                             `json:"segment,omitempty"`
	SegmentOffset               int64                              `json:"segment_offset,omitempty"`
	CanonicalSHA256             string                             `json:"canonical_sha256,omitempty"`
	PreviousDigest              string                             `json:"previous_digest"`
	PreviousAuthorizationDigest string                             `json:"previous_authorization_digest,omitempty"`
	Authorization               *PostCompletionRepairAuthorization `json:"authorization,omitempty"`
	TransitionDigest            string                             `json:"transition_digest"`
}

type postCompletionRepairProgressState struct {
	Snapshot             PostCompletionRepairPending
	Current              PostCompletionRepairPending
	Header               PostCompletionRepairProgressHeader
	LastSequence         uint64
	LastDigest           string
	JournalBytes         []byte
	TornTail             []byte
	AuthorizationHistory []PostCompletionRepairAuthorization
	AuthorizationHeads   []postCompletionRepairAuthorizationHead
}

type postCompletionRepairAuthorizationHead struct {
	Count          int
	Digest         string
	Sequence       uint64
	TransitionHead string
}

type PostCompletionRepairResultEntry struct {
	ID                     string `json:"id"`
	Disposition            string `json:"disposition"`
	ArchiveSHA256          string `json:"archive_sha256"`
	CanonicalSHA256        string `json:"canonical_sha256"`
	SegmentCanonicalSHA256 string `json:"segment_canonical_sha256"`
	Segment                string `json:"segment"`
	SegmentOffset          int64  `json:"segment_offset"`
}

type PostCompletionRepairResult struct {
	SchemaVersion        string                              `json:"schema_version"`
	ID                   string                              `json:"id"`
	CompletedAt          string                              `json:"completed_at"`
	PlanID               string                              `json:"plan_id"`
	PlanDigest           string                              `json:"plan_digest"`
	SourceDigest         string                              `json:"source_digest"`
	Authorization        PostCompletionRepairAuthorization   `json:"authorization"`
	AuthorizationHistory []PostCompletionRepairAuthorization `json:"authorization_history"`
	Total                int                                 `json:"total"`
	Counts               map[string]int                      `json:"counts"`
	Entries              []PostCompletionRepairResultEntry   `json:"entries"`
	VerificationRefs     []string                            `json:"verification_refs"`
	ResultDigest         string                              `json:"result_digest"`
	Changed              bool                                `json:"changed"`
	Replayed             bool                                `json:"replayed,omitempty"`
}

// PostCompletionRepairProof is published only after the work owner has
// durably recorded the exact ActionIntent, PolicyDecision, Receipt, and
// Transition. migrate --verify requires this sidecar, so physical completion
// can never masquerade as governance completion during a crash window.
type PostCompletionRepairProof struct {
	SchemaVersion string                            `json:"schema_version"`
	PlanID        string                            `json:"plan_id"`
	PlanDigest    string                            `json:"plan_digest"`
	ResultID      string                            `json:"result_id"`
	ResultDigest  string                            `json:"result_digest"`
	Authorization PostCompletionRepairAuthorization `json:"authorization"`
	ReceiptID     string                            `json:"receipt_id"`
	TransitionID  string                            `json:"transition_id"`
	RecordedAt    string                            `json:"recorded_at"`
	ProofDigest   string                            `json:"proof_digest"`
}

// PostCompletionRepairApplyOptions contains package-private fault injectors so
// tests exercise real durable boundaries without widening the public API.
type PostCompletionRepairApplyOptions struct {
	ValidateAuthority   func() error
	afterArchive        func(id string) error
	afterSegmentAppend  func(id string) error
	beforeLooseClaim    func(id string) error
	beforeLooseRemoval  func(id string) error
	afterLooseRemoval   func(id string) error
	afterResult         func() error
	verifyAppendedFrame func(entry PostCompletionRepairEntry, segment string, offset int64) error
	resultNow           func() time.Time
}

// PostCompletionRepairApplyOutcome separates per-call execution state from
// the immutable, digest-bound result. Result always preserves the exact sealed
// representation written by the first successful apply.
type PostCompletionRepairApplyOutcome struct {
	Result   PostCompletionRepairResult
	Changed  bool
	Replayed bool
}

type postCompletionRepairSourceCensus struct {
	Baseline           PostCompletionRepairBaseline
	WriterFenceDigest  string
	Loose              map[string]looseFile
	RawByID            map[string][]byte
	Presence           map[string]segmentLocation
	DirectorySeal      postCompletionRepairFileSeal
	StreamSeals        map[string]postCompletionRepairFileSeal
	CheckDirectorySeal bool
}

type postCompletionRepairFileSeal struct {
	Exists      bool
	Size        int64
	ModTimeNano int64
	SHA256      string
}

type postCompletionRepairPlanOptions struct {
	betweenWriterSnapshots func() error
	observationInterval    time.Duration
}

type postCompletionRepairReviewOptions struct {
	betweenWriterSnapshots func() error
	observationInterval    time.Duration
}

func (e *engine) postCompletionRepairRoot() string {
	return filepath.Join(e.migrationRoot(), postCompletionRepairDirName)
}

func (s *Store) postCompletionRepairPlanPath(id string) string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), id+".plan.json")
}

func (s *Store) postCompletionRepairPendingPath(id string) string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), id+".pending.json")
}

func (s *Store) postCompletionRepairProgressPath(id string) string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), id+".progress.jsonl")
}

func (s *Store) postCompletionRepairReviewPath(id string) string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), id+".review.json")
}

func (s *Store) postCompletionRepairSupersessionPath(id string) string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), id+".supersession.json")
}

func (s *Store) postCompletionRepairResultPath(id string) string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), id+".result.json")
}

func (s *Store) postCompletionRepairProofPath(id string) string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), id+".proof.json")
}

func (s *Store) postCompletionRepairArchivePath(rawSHA string) string {
	if len(rawSHA) < 2 {
		return filepath.Join(s.home, archiveTopDirName, postCompletionRepairArchiveGen, "sha256", "invalid")
	}
	return filepath.Join(s.home, archiveTopDirName, postCompletionRepairArchiveGen, "sha256", rawSHA[:2], rawSHA)
}

func (s *Store) postCompletionRepairClaimPath(planID string, entry PostCompletionRepairEntry) string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), "quarantine", planID, entry.FileName+"."+entry.RawSHA256)
}

func (s *Store) postCompletionRepairRootPath() string {
	return filepath.Join(s.engine.postCompletionRepairRoot(), postCompletionRepairRootFile)
}

func (s *Store) postCompletionRepairAnchorPath() string {
	return filepath.Join(s.engine.migrationRoot(), postCompletionRepairAnchorFile)
}

// CreatePostCompletionRepairPlan writes immutable source evidence only. It
// refuses before completion because ordinary migration remains the owner there.
func (s *Store) CreatePostCompletionRepairPlan(now time.Time, observationInterval ...time.Duration) (PostCompletionRepairPlanRecord, error) {
	interval, err := postCompletionRepairObservationInterval(observationInterval)
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	return s.createPostCompletionRepairPlan(now, postCompletionRepairPlanOptions{observationInterval: interval})
}

func postCompletionRepairObservationInterval(values []time.Duration) (time.Duration, error) {
	if len(values) > 1 || (len(values) == 1 && values[0] < 0) {
		return 0, fmt.Errorf("post-completion repair observation interval must be one non-negative duration")
	}
	if len(values) == 1 {
		return values[0], nil
	}
	return defaultPostCompletionObservation, nil
}

func (s *Store) createPostCompletionRepairPlan(now time.Time, opts postCompletionRepairPlanOptions) (PostCompletionRepairPlanRecord, error) {
	if s == nil || s.engine == nil {
		return PostCompletionRepairPlanRecord{}, fmt.Errorf("event store requires a home")
	}
	if !s.MigrationComplete() {
		return PostCompletionRepairPlanRecord{}, fmt.Errorf("post-completion repair requires a completed migration")
	}
	lockMigrator := &migrator{store: s, engine: s.engine}
	runLock, err := lockMigrator.acquireRunLockBlocking()
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	defer runLock.release()
	fenceStarted := time.Now().UTC()
	fenceBefore, err := s.postCompletionRepairWriterSnapshot(nil, false)
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	if opts.betweenWriterSnapshots != nil {
		if err := opts.betweenWriterSnapshots(); err != nil {
			return PostCompletionRepairPlanRecord{}, err
		}
	}
	if err := observePostCompletionRepairClock(s.engine.migrationRoot(), opts.observationInterval); err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	m := &migrator{store: s, engine: s.engine}
	loose, err := m.enumerateLoose()
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	if len(loose) == 0 {
		return PostCompletionRepairPlanRecord{}, fmt.Errorf("post-completion repair has no loose records to plan")
	}
	presence, err := s.engine.buildSegmentPresence()
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	baseline, err := s.postCompletionRepairBaseline()
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	entries := make([]PostCompletionRepairEntry, 0, len(loose))
	counts := newRepairCounts()
	for _, lf := range loose {
		entry, err := s.planPostCompletionRepairEntry(lf, presence)
		if err != nil {
			return PostCompletionRepairPlanRecord{}, err
		}
		entries = append(entries, entry)
		counts[entry.Disposition]++
	}
	sourceDigest, err := digestRepairValue(struct {
		Baseline PostCompletionRepairBaseline `json:"baseline"`
		Entries  []PostCompletionRepairEntry  `json:"entries"`
	}{baseline, entries})
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	fenceAfter, err := s.postCompletionRepairWriterSnapshot(nil, false)
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	if fenceBefore != fenceAfter {
		return PostCompletionRepairPlanRecord{}, fmt.Errorf("post-completion repair writer changed source during bounded quiescence observation")
	}
	fenceCompleted := time.Now().UTC()
	if now.IsZero() {
		now = fenceCompleted
	}
	plan := PostCompletionRepairPlan{
		SchemaVersion: postCompletionRepairPlanSchema,
		CreatedAt:     now.UTC().Format(time.RFC3339Nano),
		Status:        "ready",
		SourceDigest:  sourceDigest,
		Revision:      sourceDigest,
		WriterFence: PostCompletionRepairWriterFence{
			StartedAt: fenceStarted.Format(time.RFC3339Nano), CompletedAt: fenceCompleted.Format(time.RFC3339Nano),
			BeforeDigest: fenceBefore, AfterDigest: fenceAfter, Stable: true,
			ObservationMillis: opts.observationInterval.Milliseconds(),
		},
		Baseline:     baseline,
		Total:        len(entries),
		Counts:       counts,
		Entries:      entries,
		EvidenceRefs: []string{"eventstore-migration-plan:" + baseline.MigrationPlanSHA256, "eventstore-migration-manifest:" + baseline.ManifestSHA256, "eventstore-migration-completion:" + baseline.CompletionSHA256},
	}
	plan.ID, err = s.postCompletionRepairPlanIDForSource(sourceDigest)
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	planPath := s.postCompletionRepairPlanPath(plan.ID)
	if existing, loadErr := loadStrictRootedJSON[PostCompletionRepairPlan](s.home, planPath, "post-completion repair plan"); loadErr == nil {
		if err := validatePostCompletionRepairPlan(existing); err != nil {
			return PostCompletionRepairPlanRecord{}, err
		}
		if existing.SourceDigest != plan.SourceDigest || existing.Revision != plan.Revision || existing.WriterFence.AfterDigest != plan.WriterFence.AfterDigest || !reflect.DeepEqual(existing.Baseline, plan.Baseline) ||
			existing.Total != plan.Total || !reflect.DeepEqual(existing.Counts, plan.Counts) ||
			!reflect.DeepEqual(existing.Entries, plan.Entries) || !reflect.DeepEqual(existing.EvidenceRefs, plan.EvidenceRefs) {
			return PostCompletionRepairPlanRecord{}, fmt.Errorf("post-completion repair plan id collision or source drift")
		}
		if err := s.ensurePostCompletionRepairArtifact("plan", existing.ID, planPath); err != nil {
			return PostCompletionRepairPlanRecord{}, err
		}
		return PostCompletionRepairPlanRecord{Plan: existing, PlanPath: planPath}, nil
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return PostCompletionRepairPlanRecord{}, loadErr
	}
	plan.PlanDigest, err = postCompletionRepairPlanDigest(plan)
	if err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	if err := validatePostCompletionRepairPlan(plan); err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	if err := s.writePostCompletionRepairExclusiveOrEqualJSON(planPath, "post-completion repair plan", plan); err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	if err := s.ensurePostCompletionRepairArtifact("plan", plan.ID, planPath); err != nil {
		return PostCompletionRepairPlanRecord{}, err
	}
	return PostCompletionRepairPlanRecord{Plan: plan, PlanPath: planPath}, nil
}

func postCompletionRepairPlanID(sourceDigest string, occurrence uint64) string {
	base := "esrp_" + strings.TrimPrefix(sourceDigest, "sha256:")[:12]
	if occurrence == 0 {
		return base
	}
	return fmt.Sprintf("%s_o%d", base, occurrence)
}

// postCompletionRepairPlanIDForSource preserves the original generation-zero
// identity, reuses an in-flight occurrence, and advances only after the exact
// prior lifecycle is terminal, proof-backed, and fully finalized. The suffix
// is deterministic, so repeated planning remains idempotent without time as
// identity.
func (s *Store) postCompletionRepairPlanIDForSource(sourceDigest string) (string, error) {
	root, _, rootExists, err := s.loadPostCompletionRepairRoot()
	if err != nil {
		return "", err
	}
	anchor, anchorExists, err := s.loadPostCompletionRepairAnchor()
	if err != nil {
		return "", err
	}
	if err := verifyPostCompletionRepairRootAnchor(root, rootExists, anchor, anchorExists); err != nil {
		return "", err
	}
	if rootExists {
		if err := s.verifyPostCompletionRepairRootArtifactBytes(root); err != nil {
			return "", fmt.Errorf("post-completion repair occurrence authority invalid: immutable repair artifact drift: %w", err)
		}
	}
	for occurrence := uint64(0); ; occurrence++ {
		planID := postCompletionRepairPlanID(sourceDigest, occurrence)
		plan, loadErr := loadStrictRootedJSON[PostCompletionRepairPlan](s.home, s.postCompletionRepairPlanPath(planID), "post-completion repair plan")
		if errors.Is(loadErr, os.ErrNotExist) {
			return planID, nil
		}
		if loadErr != nil {
			return "", loadErr
		}
		if err := validatePostCompletionRepairPlan(plan); err != nil {
			return "", err
		}
		if plan.SourceDigest != sourceDigest {
			return "", fmt.Errorf("post-completion repair plan id collision or source drift")
		}
		completed, err := s.postCompletionRepairOccurrenceCompleted(root, rootExists, plan)
		if err != nil {
			return "", err
		}
		if !completed {
			return planID, nil
		}
		if occurrence == ^uint64(0) {
			return "", fmt.Errorf("post-completion repair occurrence identity exhausted")
		}
	}
}

func (s *Store) postCompletionRepairOccurrenceCompleted(root PostCompletionRepairRoot, rootExists bool, plan PostCompletionRepairPlan) (bool, error) {
	if !rootExists {
		return false, nil
	}
	kinds := map[string]bool{}
	for _, artifact := range root.Artifacts {
		if postCompletionRepairPlanIDFromArtifactPath(artifact.Path) == plan.ID {
			kinds[artifact.Kind] = true
		}
	}
	if kinds["proof"] && !kinds["result"] && !kinds["supersession"] {
		return false, fmt.Errorf("post-completion repair occurrence %s has proof without terminal owner", plan.ID)
	}
	if kinds["result"] && kinds["supersession"] {
		return false, fmt.Errorf("post-completion repair occurrence %s has conflicting terminal owners", plan.ID)
	}
	if !kinds["proof"] || kinds["pending"] || kinds["progress"] {
		return false, nil
	}
	if kinds["result"] {
		result, present, err := s.ReadPostCompletionRepairResult(plan.ID)
		if err != nil || !present {
			return false, errors.Join(fmt.Errorf("post-completion repair occurrence %s rooted result missing", plan.ID), err)
		}
		if err := s.validatePostCompletionRepairResult(result, plan, result.Authorization); err != nil {
			return false, err
		}
		proof, present, err := s.ReadPostCompletionRepairProof(plan.ID)
		if err != nil || !present {
			return false, errors.Join(fmt.Errorf("post-completion repair occurrence %s rooted proof missing", plan.ID), err)
		}
		if err := s.validatePostCompletionRepairProof(proof, result); err != nil {
			return false, err
		}
		return true, nil
	}
	if kinds["supersession"] {
		supersession, present, err := s.ReadPostCompletionRepairSupersession(plan.ID)
		if err != nil || !present {
			return false, errors.Join(fmt.Errorf("post-completion repair occurrence %s rooted supersession missing", plan.ID), err)
		}
		proof, present, err := s.ReadPostCompletionRepairSupersessionProof(plan.ID)
		if err != nil || !present {
			return false, errors.Join(fmt.Errorf("post-completion repair occurrence %s rooted supersession proof missing", plan.ID), err)
		}
		if err := s.validatePostCompletionRepairSupersessionProof(proof, supersession); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// ReviewPostCompletionRepair records the operator decision separately from
// policy. It binds the exact immutable plan and a second stable writer fence;
// it is never inferred from private scope or an allowing standing grant.
func (s *Store) ReviewPostCompletionRepair(planPath, reviewer, decision string, writerStoppedAttested bool, now time.Time, observationInterval ...time.Duration) (PostCompletionRepairReviewRecord, error) {
	interval, err := postCompletionRepairObservationInterval(observationInterval)
	if err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	return s.reviewPostCompletionRepair(planPath, reviewer, decision, writerStoppedAttested, now, postCompletionRepairReviewOptions{observationInterval: interval})
}

func (s *Store) reviewPostCompletionRepair(planPath, reviewer, decision string, writerStoppedAttested bool, now time.Time, opts postCompletionRepairReviewOptions) (PostCompletionRepairReviewRecord, error) {
	lockMigrator := &migrator{store: s, engine: s.engine}
	runLock, err := lockMigrator.acquireRunLockBlocking()
	if err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	defer runLock.release()
	plan, err := s.loadPostCompletionRepairPlan(planPath)
	if err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	if !validPostCompletionRepairReviewer(reviewer) {
		return PostCompletionRepairReviewRecord{}, fmt.Errorf("post-completion repair reviewer must be an operator identity")
	}
	if decision != "authorize" {
		return PostCompletionRepairReviewRecord{}, fmt.Errorf("post-completion repair review decision must be authorize")
	}
	if !writerStoppedAttested {
		return PostCompletionRepairReviewRecord{}, fmt.Errorf("post-completion repair review requires positive writer-stop attestation")
	}
	before, err := s.postCompletionRepairWriterSnapshot(&plan, false)
	if err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	if opts.betweenWriterSnapshots != nil {
		if err := opts.betweenWriterSnapshots(); err != nil {
			return PostCompletionRepairReviewRecord{}, err
		}
	}
	if err := observePostCompletionRepairClock(s.engine.migrationRoot(), opts.observationInterval); err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	after, err := s.postCompletionRepairWriterSnapshot(&plan, false)
	if err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	if before != after || after != plan.WriterFence.AfterDigest {
		return PostCompletionRepairReviewRecord{}, fmt.Errorf("post-completion repair writer fence is not quiescent at review")
	}
	if now.IsZero() {
		now = time.Now()
	}
	review := PostCompletionRepairReview{
		SchemaVersion: postCompletionRepairReviewSchema, CreatedAt: now.UTC().Format(time.RFC3339Nano),
		PlanID: plan.ID, PlanDigest: plan.PlanDigest, PlanRevision: plan.Revision,
		Reviewer: reviewer, Decision: decision, WriterStoppedAttested: true,
		WriterFenceDigest: after, ObservationMillis: opts.observationInterval.Milliseconds(),
	}
	review.ID = "esrr_" + strings.TrimPrefix(plan.PlanDigest, "sha256:")[:12]
	review.ReviewDigest, err = postCompletionRepairReviewDigest(review)
	if err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	if err := validatePostCompletionRepairReview(review, plan); err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	path := s.postCompletionRepairReviewPath(plan.ID)
	if existing, loadErr := loadStrictRootedJSON[PostCompletionRepairReview](s.home, path, "post-completion repair review"); loadErr == nil {
		candidate := review
		candidate.CreatedAt = existing.CreatedAt
		candidate.ReviewDigest, _ = postCompletionRepairReviewDigest(candidate)
		if !reflect.DeepEqual(existing, candidate) {
			return PostCompletionRepairReviewRecord{}, fmt.Errorf("post-completion repair review already exists with different content")
		}
		if err := s.ensurePostCompletionRepairArtifact("review", existing.ID, path); err != nil {
			return PostCompletionRepairReviewRecord{}, err
		}
		return PostCompletionRepairReviewRecord{Review: existing, ReviewPath: path}, nil
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return PostCompletionRepairReviewRecord{}, loadErr
	}
	if err := s.writePostCompletionRepairExclusiveOrEqualJSON(path, "post-completion repair review", review); err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	if err := s.ensurePostCompletionRepairArtifact("review", review.ID, path); err != nil {
		return PostCompletionRepairReviewRecord{}, err
	}
	return PostCompletionRepairReviewRecord{Review: review, ReviewPath: path}, nil
}

func (s *Store) planPostCompletionRepairEntry(lf looseFile, presence map[string]segmentLocation) (PostCompletionRepairEntry, error) {
	if regular, err := isRegularFile(lf.path); err != nil || !regular {
		return PostCompletionRepairEntry{}, errors.Join(fmt.Errorf("post-completion loose %s is not a regular file", lf.id), err)
	}
	if _, err := ReferenceForID(lf.id); err != nil {
		return PostCompletionRepairEntry{}, fmt.Errorf("post-completion loose id %q invalid: %w", lf.id, err)
	}
	raw, err := os.ReadFile(lf.path)
	if err != nil {
		return PostCompletionRepairEntry{}, err
	}
	compact, err := canonical(raw)
	if err != nil {
		return PostCompletionRepairEntry{}, fmt.Errorf("post-completion loose %s canonicalize: %w", lf.id, err)
	}
	var header frameRecordHeader
	if err := json.Unmarshal(compact, &header); err != nil {
		return PostCompletionRepairEntry{}, fmt.Errorf("post-completion loose %s header: %w", lf.id, err)
	}
	if header.ID != lf.id {
		return PostCompletionRepairEntry{}, fmt.Errorf("post-completion loose %s embedded id %q differs from filename", lf.id, header.ID)
	}
	entry := PostCompletionRepairEntry{
		ID: lf.id, FileName: lf.name, RawSHA256: recordHash(raw), CanonicalSHA256: recordHash(compact),
		SchemaVersion: header.SchemaVersion, Family: familyOf(compact, lf.id), Disposition: RepairImportAbsent,
	}
	if loc, ok := presence[lf.id]; ok {
		entry.SegmentSHA256, entry.Segment, entry.SegmentOffset = loc.sha256, loc.relPath, loc.offset
		if loc.sha256 == entry.CanonicalSHA256 {
			entry.Disposition = RepairArchiveIdentical
		} else {
			entry.Disposition = RepairArchiveDivergent
		}
	}
	return entry, nil
}

func (s *Store) postCompletionRepairBaseline() (PostCompletionRepairBaseline, error) {
	planHash, err := s.hashRequiredPostCompletionRepairFile(s.engine.planPath(), "event-store migration plan")
	if err != nil {
		return PostCompletionRepairBaseline{}, fmt.Errorf("read frozen migration plan: %w", err)
	}
	manifestHash, err := s.hashRequiredPostCompletionRepairFile(s.engine.manifestPath(), "event-store migration manifest")
	if err != nil {
		return PostCompletionRepairBaseline{}, fmt.Errorf("read frozen migration manifest: %w", err)
	}
	completionHash, err := s.hashRequiredPostCompletionRepairFile(s.engine.completionPath(), "event-store migration completion")
	if err != nil {
		return PostCompletionRepairBaseline{}, fmt.Errorf("read frozen migration completion: %w", err)
	}
	return PostCompletionRepairBaseline{MigrationPlanSHA256: planHash, ManifestSHA256: manifestHash, CompletionSHA256: completionHash}, nil
}

func (s *Store) postCompletionRepairWriterSnapshot(plan *PostCompletionRepairPlan, allowProgress bool) (string, error) {
	regrew, err := s.engine.legacyStreamRegrew()
	if err != nil {
		return "", err
	}
	if regrew {
		return "", fmt.Errorf("post-completion repair refuses while a retired compatibility writer is active")
	}
	baseline, err := s.postCompletionRepairBaseline()
	if err != nil {
		return "", err
	}
	type sourceRow struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	m := &migrator{store: s, engine: s.engine}
	loose, err := m.enumerateLoose()
	if err != nil {
		return "", err
	}
	rows := make([]sourceRow, 0, len(loose)+2)
	actual := map[string]looseFile{}
	for _, lf := range loose {
		actual[lf.id] = lf
	}
	if plan == nil {
		for _, lf := range loose {
			body, err := os.ReadFile(lf.path)
			if err != nil {
				return "", err
			}
			rows = append(rows, sourceRow{Name: "loose/" + lf.name, SHA256: recordHash(body)})
		}
	} else {
		planned := map[string]bool{}
		for _, entry := range plan.Entries {
			planned[entry.ID] = true
			sha := "missing"
			if lf, ok := actual[entry.ID]; ok {
				body, err := os.ReadFile(lf.path)
				if err != nil {
					return "", err
				}
				sha = recordHash(body)
			} else if allowProgress {
				body, err := s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID)
				if err == nil {
					sha = recordHash(body)
				} else if !errors.Is(err, os.ErrNotExist) {
					return "", err
				}
			}
			rows = append(rows, sourceRow{Name: "loose/" + entry.FileName, SHA256: sha})
		}
		for _, lf := range loose {
			if planned[lf.id] {
				continue
			}
			body, err := os.ReadFile(lf.path)
			if err != nil {
				return "", err
			}
			rows = append(rows, sourceRow{Name: "unplanned/" + lf.name, SHA256: recordHash(body)})
		}
	}
	for _, name := range []string{hookEventsStreamName, tracesStreamName} {
		body, err := os.ReadFile(filepath.Join(s.engine.eventsDir, name))
		if errors.Is(err, os.ErrNotExist) {
			rows = append(rows, sourceRow{Name: "stream/" + name, SHA256: "absent"})
			continue
		}
		if err != nil {
			return "", err
		}
		rows = append(rows, sourceRow{Name: "stream/" + name, SHA256: recordHash(body)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return digestRepairValue(struct {
		Baseline PostCompletionRepairBaseline `json:"baseline"`
		Sources  []sourceRow                  `json:"sources"`
	}{Baseline: baseline, Sources: rows})
}

func (s *Store) hashRequiredPostCompletionRepairFile(path, kind string) (string, error) {
	body, err := homepkg.ReadRootedRegularFile(s.home, path, kind)
	if err != nil {
		return "", err
	}
	return "sha256:" + recordHash(body), nil
}

func (s *Store) ApplyPostCompletionRepair(planPath string, authorization PostCompletionRepairAuthorization, opts PostCompletionRepairApplyOptions) (PostCompletionRepairResult, error) {
	outcome, err := s.applyPostCompletionRepairOutcome(planPath, authorization, opts, nil)
	return outcome.Result, err
}

// ApplyPostCompletionRepairOutcome reports whether this invocation performed
// the physical apply without projecting those call-local flags into Result.
func (s *Store) ApplyPostCompletionRepairOutcome(planPath string, authorization PostCompletionRepairAuthorization, opts PostCompletionRepairApplyOptions) (PostCompletionRepairApplyOutcome, error) {
	return s.applyPostCompletionRepairOutcome(planPath, authorization, opts, nil)
}

func (s *Store) applyPostCompletionRepair(planPath string, authorization PostCompletionRepairAuthorization, opts PostCompletionRepairApplyOptions, observeInspection func(string)) (PostCompletionRepairResult, error) {
	outcome, err := s.applyPostCompletionRepairOutcome(planPath, authorization, opts, observeInspection)
	return outcome.Result, err
}

func (s *Store) applyPostCompletionRepairOutcome(planPath string, authorization PostCompletionRepairAuthorization, opts PostCompletionRepairApplyOptions, observeInspection func(string)) (PostCompletionRepairApplyOutcome, error) {
	var zero PostCompletionRepairApplyOutcome
	if s == nil || s.engine == nil {
		return zero, fmt.Errorf("event store requires a home")
	}
	if !s.engine.localFS {
		return zero, fmt.Errorf("post-completion repair requires a local filesystem, detected %s", s.engine.fsType)
	}
	plan, err := s.loadPostCompletionRepairPlan(planPath)
	if err != nil {
		return zero, err
	}
	if err := validatePostCompletionRepairAuthorization(authorization, plan); err != nil {
		return zero, err
	}
	review, err := s.loadPostCompletionRepairReview(s.postCompletionRepairReviewPath(plan.ID), plan)
	if err != nil {
		return zero, fmt.Errorf("post-completion repair exact operator review required: %w", err)
	}
	if review.ID != authorization.ReviewID || review.ReviewDigest != authorization.ReviewDigest || review.WriterFenceDigest != authorization.WriterFenceDigest {
		return zero, fmt.Errorf("post-completion repair authorization does not bind exact operator review")
	}
	m := &migrator{store: s, engine: s.engine}
	runLock, err := m.acquireRunLockBlocking()
	if err != nil {
		return zero, err
	}
	defer runLock.release()
	root, _, rootExists, err := s.loadPostCompletionRepairRoot()
	if err != nil {
		return zero, err
	}
	if rootExists && postCompletionRepairRootOwns(root, "supersession", plan.ID) {
		return zero, fmt.Errorf("post-completion repair rooted superseded plan %s cannot be applied", plan.ID)
	}
	resultPath := s.postCompletionRepairResultPath(plan.ID)
	if existing, loadErr := loadStrictRootedJSON[PostCompletionRepairResult](s.home, resultPath, "post-completion repair result"); loadErr == nil {
		if err := s.ensurePostCompletionRepairArtifact("plan", plan.ID, s.postCompletionRepairPlanPath(plan.ID)); err != nil {
			return zero, err
		}
		if err := s.ensurePostCompletionRepairArtifact("review", review.ID, s.postCompletionRepairReviewPath(plan.ID)); err != nil {
			return zero, err
		}
		if err := s.validatePostCompletionRepairResult(existing, plan, authorization); err != nil {
			return zero, err
		}
		appendLock, err := acquireAppendLock(s.engine.segRoot)
		if err != nil {
			return zero, err
		}
		stateErr := s.verifyPostCompletionRepairResultState(existing, true)
		releaseErr := appendLock.release()
		if stateErr != nil || releaseErr != nil {
			return zero, errors.Join(stateErr, releaseErr)
		}
		if err := s.ensurePostCompletionRepairArtifact("result", existing.ID, resultPath); err != nil {
			return zero, err
		}
		if pending, present, err := s.ReadPostCompletionRepairPending(plan.ID); err != nil {
			return zero, err
		} else if present {
			if err := validatePostCompletionRepairPending(pending, plan, authorization); err != nil {
				return zero, err
			}
		} else if proof, present, err := s.ReadPostCompletionRepairProof(plan.ID); err != nil {
			return zero, err
		} else if !present {
			return zero, fmt.Errorf("post-completion repair result %s has neither pending journal nor work proof", existing.ID)
		} else if err := s.validatePostCompletionRepairProof(proof, existing); err != nil {
			return zero, err
		}
		return PostCompletionRepairApplyOutcome{Result: existing, Replayed: true}, nil
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return zero, loadErr
	}
	sealedFinalization, err := s.postCompletionRepairPhysicalCompletionForFinalization(plan, authorization)
	if err != nil {
		return zero, err
	}
	if !sealedFinalization {
		if opts.ValidateAuthority == nil {
			return zero, fmt.Errorf("post-completion repair current policy authority validator is required before physical mutation")
		}
		if err := opts.ValidateAuthority(); err != nil {
			return zero, fmt.Errorf("post-completion repair current policy authority changed before mutation: %w", err)
		}
	}
	if err := s.ensurePostCompletionRepairArtifact("plan", plan.ID, s.postCompletionRepairPlanPath(plan.ID)); err != nil {
		return zero, err
	}
	if err := s.ensurePostCompletionRepairArtifact("review", review.ID, s.postCompletionRepairReviewPath(plan.ID)); err != nil {
		return zero, err
	}

	pendingExists := false
	if _, statErr := homepkg.ReadRootedRegularFile(s.home, s.postCompletionRepairPendingPath(plan.ID), "post-completion repair pending snapshot"); statErr == nil {
		pendingExists = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return zero, statErr
	}
	census, err := s.buildPostCompletionRepairSourceCensus(plan, pendingExists, observeInspection)
	if err != nil {
		return zero, err
	}
	if census.WriterFenceDigest != authorization.WriterFenceDigest {
		return zero, fmt.Errorf("post-completion repair writer fence drifted after operator review")
	}
	var progressState postCompletionRepairProgressState
	if pendingExists {
		progressState, err = s.loadOrUpgradePostCompletionRepairProgress(plan, authorization)
		if err != nil {
			return zero, err
		}
	} else {
		if err := s.validatePostCompletionRepairSourceCensus(plan, false, census); err != nil {
			return zero, err
		}
		progressState, err = s.createPostCompletionRepairProgress(plan, authorization, time.Now())
		if err != nil {
			return zero, err
		}
	}
	if err := s.verifyPostCompletionRepairProgressPhysicalWithPresence(plan, progressState.Current, census.Presence); err != nil {
		return zero, err
	}
	pending := &progressState.Current
	// A pending journal is the boundary after which partial progress is tracked
	// and therefore resumable. Re-check all entries together before continuing;
	// one conflicting/drifted member prevents any new mutation in this attempt.
	if err := s.validatePostCompletionRepairSourceCensus(plan, true, census); err != nil {
		return zero, err
	}
	presence := clonePostCompletionRepairPresence(census.Presence)
	repairedFamilies := map[string]bool{}
	for index, entry := range plan.Entries {
		progress := &pending.Entries[index]
		raw, err := census.sourceBytes(entry)
		if err != nil {
			return zero, err
		}
		if repairPhaseRank(progress.Phase) < repairPhaseRank(repairPhaseFrameVerified) {
			if err := s.validatePostCompletionRepairMutationBoundary(census, opts); err != nil {
				return zero, err
			}
			if err := s.writePostCompletionRepairArchive(entry, raw); err != nil {
				return zero, err
			}
			if opts.afterArchive != nil {
				if err := opts.afterArchive(entry.ID); err != nil {
					return zero, err
				}
			}
		}
		if entry.Disposition == RepairImportAbsent && repairPhaseRank(progress.Phase) < repairPhaseRank(repairPhaseFrameVerified) {
			releaseAll, err := s.engine.lockAllFamilyShards()
			if err != nil {
				return zero, err
			}
			segmentErr := func() (retErr error) {
				defer func() {
					retErr = errors.Join(retErr, releaseAll())
				}()
				if repairPhaseRank(progress.Phase) < repairPhaseRank(repairPhaseAppendReserved) {
					if err := s.validatePostCompletionRepairMutationBoundary(census, opts); err != nil {
						return err
					}
					if state, _, err := s.engine.checkID(entry.Family, entry.ID, entry.CanonicalSHA256); err != nil {
						return err
					} else if state != idAbsent {
						return fmt.Errorf("post-completion repair source state drift: segment %s changed before reserved append", entry.ID)
					}
					if !repairedFamilies[entry.Family] {
						if last, exists, err := s.engine.lastWritten(entry.Family); err != nil {
							return err
						} else if exists {
							if err := s.engine.repairTail(last.path); err != nil {
								return err
							}
						}
						repairedFamilies[entry.Family] = true
					}
					segment, offset, err := s.reservePostCompletionRepairFrame(entry)
					if err != nil {
						return err
					}
					if err := s.advancePostCompletionRepairProgress(&progressState, plan, index, repairPhaseAppendReserved, segment, offset, entry.CanonicalSHA256); err != nil {
						return err
					}
					progress = &progressState.Current.Entries[index]
				}
				if repairPhaseRank(progress.Phase) < repairPhaseRank(repairPhaseAppended) {
					if err := s.validatePostCompletionRepairMutationBoundary(census, opts); err != nil {
						return err
					}
					frame, _, err := buildFrame(entry.ID, raw)
					if err != nil {
						return err
					}
					line, err := encodeFrame(frame)
					if err != nil {
						return err
					}
					found, recovered, err := s.recoverPostCompletionRepairReservedFrame(entry, progress.Segment, progress.SegmentOffset, line)
					if err != nil {
						return err
					}
					if !found {
						if err := s.appendPostCompletionRepairReservedFrame(progress.Segment, progress.SegmentOffset, line); err != nil {
							return err
						}
						recovered = true
					}
					if recovered && opts.afterSegmentAppend != nil {
						if err := opts.afterSegmentAppend(entry.ID); err != nil {
							return err
						}
					}
					if err := s.advancePostCompletionRepairProgress(&progressState, plan, index, repairPhaseAppended, progress.Segment, progress.SegmentOffset, entry.CanonicalSHA256); err != nil {
						return err
					}
					progress = &progressState.Current.Entries[index]
				}
				presence[entry.ID] = segmentLocation{family: entry.Family, relPath: progress.Segment, offset: progress.SegmentOffset, sha256: entry.CanonicalSHA256}
				found, err := s.verifyPostCompletionRepairFrame(entry, progress.Segment, progress.SegmentOffset)
				if err != nil || !found {
					return errors.Join(fmt.Errorf("post-completion repair appended frame %s failed exact verification", entry.ID), err)
				}
				if opts.verifyAppendedFrame != nil {
					if err := opts.verifyAppendedFrame(entry, progress.Segment, progress.SegmentOffset); err != nil {
						return err
					}
				}
				if err := s.advancePostCompletionRepairProgress(&progressState, plan, index, repairPhaseFrameVerified, progress.Segment, progress.SegmentOffset, progress.CanonicalSHA256); err != nil {
					return err
				}
				progress = &progressState.Current.Entries[index]
				return nil
			}()
			if segmentErr != nil {
				return zero, segmentErr
			}
		} else if entry.Disposition != RepairImportAbsent {
			loc, present := presence[entry.ID]
			want := entry.SegmentSHA256
			if !present || loc.sha256 != want {
				return zero, fmt.Errorf("post-completion repair source state drift: segment %s changed before apply", entry.ID)
			}
		}
		if entry.Disposition != RepairImportAbsent && repairPhaseRank(progress.Phase) < repairPhaseRank(repairPhaseFrameVerified) {
			if err := s.advancePostCompletionRepairProgress(&progressState, plan, index, repairPhaseFrameVerified, progress.Segment, progress.SegmentOffset, progress.CanonicalSHA256); err != nil {
				return zero, err
			}
			progress = &progressState.Current.Entries[index]
		}
		if repairPhaseRank(progress.Phase) < repairPhaseRank(repairPhaseRemoveReserved) {
			if err := s.advancePostCompletionRepairProgress(&progressState, plan, index, repairPhaseRemoveReserved, progress.Segment, progress.SegmentOffset, progress.CanonicalSHA256); err != nil {
				return zero, err
			}
			progress = &progressState.Current.Entries[index]
		}
		if repairPhaseRank(progress.Phase) < repairPhaseRank(repairPhaseClaimed) {
			if err := s.validatePostCompletionRepairMutationBoundary(census, opts); err != nil {
				return zero, err
			}
			directorySeal, err := s.claimPostCompletionRepairLoose(plan.ID, entry, opts.beforeLooseClaim, opts.beforeLooseRemoval)
			if err != nil {
				return zero, err
			}
			delete(census.Loose, entry.ID)
			census.DirectorySeal = directorySeal
			if err := s.advancePostCompletionRepairProgress(&progressState, plan, index, repairPhaseClaimed, progress.Segment, progress.SegmentOffset, progress.CanonicalSHA256); err != nil {
				return zero, err
			}
			progress = &progressState.Current.Entries[index]
		}
		if repairPhaseRank(progress.Phase) < repairPhaseRank(repairPhaseRemoved) {
			claimPath := s.postCompletionRepairClaimPath(plan.ID, entry)
			claimBefore, statErr := os.Lstat(claimPath)
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return zero, statErr
			}
			if err := s.validatePostCompletionRepairMutationBoundary(census, opts); err != nil {
				return zero, err
			}
			if claimBefore != nil {
				claimAfter, statErr := os.Lstat(claimPath)
				if statErr != nil || !os.SameFile(claimBefore, claimAfter) {
					return zero, fmt.Errorf("post-completion repair claim %s file changed after validation: removal would destroy bytes the final authority never reviewed", entry.ID)
				}
			}
			if err := s.cleanupPostCompletionRepairClaim(plan.ID, entry); err != nil {
				return zero, err
			}
			if opts.afterLooseRemoval != nil {
				if err := opts.afterLooseRemoval(entry.ID); err != nil {
					return zero, err
				}
			}
			if err := s.advancePostCompletionRepairProgress(&progressState, plan, index, repairPhaseRemoved, progress.Segment, progress.SegmentOffset, progress.CanonicalSHA256); err != nil {
				return zero, err
			}
		}
	}
	if err := census.validateFinalLooseSet(s, observeInspection); err != nil {
		return zero, err
	}
	if err := s.preflightPostCompletionRepairFinalWithPresence(plan, presence); err != nil {
		return zero, err
	}
	if !sealedFinalization {
		if err := s.validatePostCompletionRepairMutationBoundary(census, opts); err != nil {
			return zero, err
		}
	}
	resultNow := time.Now()
	if opts.resultNow != nil {
		resultNow = opts.resultNow()
	}
	result, err := s.buildPostCompletionRepairResultWithPresence(plan, authorization, progressState.AuthorizationHistory, resultNow, presence)
	if err != nil {
		return zero, err
	}
	if err := s.writePostCompletionRepairExclusiveOrEqualJSON(resultPath, "post-completion repair result", result); err != nil {
		return zero, err
	}
	if err := s.ensurePostCompletionRepairArtifact("result", result.ID, resultPath); err != nil {
		return zero, err
	}
	if opts.afterResult != nil {
		if err := opts.afterResult(); err != nil {
			return zero, err
		}
	}
	return PostCompletionRepairApplyOutcome{Result: result, Changed: true}, nil
}

func (s *Store) validatePostCompletionRepairMutationBoundary(census *postCompletionRepairSourceCensus, opts PostCompletionRepairApplyOptions) error {
	if err := census.validateSourceSeal(s); err != nil {
		return fmt.Errorf("post-completion repair writer fence drifted after operator review: %w", err)
	}
	if opts.ValidateAuthority == nil {
		return fmt.Errorf("post-completion repair current policy authority validator is required before physical mutation")
	}
	if err := opts.ValidateAuthority(); err != nil {
		return fmt.Errorf("post-completion repair current policy authority changed before mutation: %w", err)
	}
	return nil
}

func clonePostCompletionRepairPresence(source map[string]segmentLocation) map[string]segmentLocation {
	cloned := make(map[string]segmentLocation, len(source))
	for id, location := range source {
		cloned[id] = location
	}
	return cloned
}

func postCompletionRepairSeal(path string) (postCompletionRepairFileSeal, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return postCompletionRepairFileSeal{}, nil
	}
	if err != nil {
		return postCompletionRepairFileSeal{}, err
	}
	seal := postCompletionRepairFileSeal{Exists: true, Size: info.Size(), ModTimeNano: info.ModTime().UnixNano()}
	if info.IsDir() {
		return seal, nil
	}
	if !info.Mode().IsRegular() {
		return postCompletionRepairFileSeal{}, fmt.Errorf("post-completion repair seal requires a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return postCompletionRepairFileSeal{}, err
	}
	hash := sha256.New()
	n, readErr := io.Copy(hash, file)
	after, statErr := file.Stat()
	closeErr := file.Close()
	named, nameErr := os.Stat(path)
	if err := errors.Join(readErr, statErr, closeErr, nameErr); err != nil {
		return postCompletionRepairFileSeal{}, err
	}
	if !os.SameFile(info, after) || !os.SameFile(info, named) || n != info.Size() ||
		after.Size() != info.Size() || named.Size() != info.Size() ||
		!after.ModTime().Equal(info.ModTime()) || !named.ModTime().Equal(info.ModTime()) {
		return postCompletionRepairFileSeal{}, fmt.Errorf("post-completion repair source changed while sealing: %s", path)
	}
	seal.SHA256 = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return seal, nil
}

func (c *postCompletionRepairSourceCensus) sourceBytes(entry PostCompletionRepairEntry) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("post-completion repair source census required")
	}
	raw, ok := c.RawByID[entry.ID]
	if !ok || recordHash(raw) != entry.RawSHA256 {
		return nil, fmt.Errorf("post-completion repair tracked source %s unavailable", entry.ID)
	}
	return raw, nil
}

func (c *postCompletionRepairSourceCensus) validateSourceSeal(s *Store) error {
	if c == nil {
		return fmt.Errorf("source census missing")
	}
	if regrew, err := s.engine.legacyStreamRegrew(); err != nil {
		return err
	} else if regrew {
		return fmt.Errorf("retired compatibility stream regrew")
	}
	baseline, err := s.postCompletionRepairBaseline()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(baseline, c.Baseline) {
		return fmt.Errorf("frozen migration baseline changed")
	}
	if c.CheckDirectorySeal {
		current, err := postCompletionRepairSeal(s.rootPath())
		if err != nil {
			return err
		}
		if current != c.DirectorySeal {
			return fmt.Errorf("loose source directory changed")
		}
	}
	for name, sealed := range c.StreamSeals {
		current, err := postCompletionRepairSeal(filepath.Join(s.engine.eventsDir, name))
		if err != nil {
			return err
		}
		if current != sealed {
			return fmt.Errorf("compatibility stream %s changed", name)
		}
	}
	return nil
}

func (c *postCompletionRepairSourceCensus) validateFinalLooseSet(s *Store, observe func(string)) error {
	if c == nil {
		return fmt.Errorf("source census missing")
	}
	if observe != nil {
		observe("final_loose_census")
	}
	loose, err := (&migrator{store: s, engine: s.engine}).enumerateLoose()
	if err != nil {
		return err
	}
	actual := make(map[string]looseFile, len(loose))
	for _, file := range loose {
		actual[file.id] = file
		if _, expected := c.Loose[file.id]; !expected {
			return fmt.Errorf("post-completion repair source state drift: unplanned loose record %s appeared", file.id)
		}
	}
	for id, expected := range c.Loose {
		file, exists := actual[id]
		if !exists || file.name != expected.name || file.path != expected.path {
			return fmt.Errorf("post-completion repair source state drift: expected loose record %s changed", id)
		}
	}
	return nil
}

func validatePostCompletionRepairLooseRegular(path, id string) error {
	regular, err := isRegularFile(path)
	if err != nil {
		return fmt.Errorf("post-completion repair source state drift: loose %s state unreadable: %w", id, err)
	}
	if !regular {
		return fmt.Errorf("post-completion repair source state drift: loose %s is not a regular file", id)
	}
	return nil
}

func (s *Store) buildPostCompletionRepairSourceCensus(plan PostCompletionRepairPlan, allowProgress bool, observe func(string)) (*postCompletionRepairSourceCensus, error) {
	if observe != nil {
		observe("full_census")
	}
	if regrew, err := s.engine.legacyStreamRegrew(); err != nil {
		return nil, err
	} else if regrew {
		return nil, fmt.Errorf("post-completion repair refuses while a retired compatibility writer is active")
	}
	baseline, err := s.postCompletionRepairBaseline()
	if err != nil {
		return nil, err
	}
	directoryBefore, err := postCompletionRepairSeal(s.rootPath())
	if err != nil {
		return nil, err
	}
	streamBefore := map[string]postCompletionRepairFileSeal{}
	for _, name := range []string{hookEventsStreamName, tracesStreamName} {
		streamBefore[name], err = postCompletionRepairSeal(filepath.Join(s.engine.eventsDir, name))
		if err != nil {
			return nil, err
		}
	}
	m := &migrator{store: s, engine: s.engine}
	loose, err := m.enumerateLoose()
	if err != nil {
		return nil, err
	}
	actual := make(map[string]looseFile, len(loose))
	for _, file := range loose {
		actual[file.id] = file
	}
	type sourceRow struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	rows := make([]sourceRow, 0, len(loose)+2)
	rawByID := make(map[string][]byte, len(plan.Entries))
	planned := make(map[string]bool, len(plan.Entries))
	for _, entry := range plan.Entries {
		planned[entry.ID] = true
		if observe != nil {
			observe("loose_record")
		}
		sha := "missing"
		if file, exists := actual[entry.ID]; exists {
			if err := validatePostCompletionRepairLooseRegular(file.path, entry.ID); err != nil {
				return nil, err
			}
			raw, readErr := os.ReadFile(file.path)
			if readErr != nil {
				return nil, readErr
			}
			rawByID[entry.ID] = raw
			sha = recordHash(raw)
		} else if allowProgress {
			raw, readErr := s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID)
			if readErr == nil {
				rawByID[entry.ID] = raw
				sha = recordHash(raw)
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			}
		}
		if archive, archiveErr := s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID); archiveErr == nil {
			if recordHash(archive) != entry.RawSHA256 {
				return nil, fmt.Errorf("post-completion repair source state drift: archive for %s has different bytes", entry.ID)
			}
		} else if !errors.Is(archiveErr, os.ErrNotExist) {
			return nil, archiveErr
		}
		rows = append(rows, sourceRow{Name: "loose/" + entry.FileName, SHA256: sha})
	}
	for _, file := range loose {
		if planned[file.id] {
			continue
		}
		if err := validatePostCompletionRepairLooseRegular(file.path, file.id); err != nil {
			return nil, err
		}
		body, readErr := os.ReadFile(file.path)
		if readErr != nil {
			return nil, readErr
		}
		rows = append(rows, sourceRow{Name: "unplanned/" + file.name, SHA256: recordHash(body)})
	}
	streamSeals := map[string]postCompletionRepairFileSeal{}
	for _, name := range []string{hookEventsStreamName, tracesStreamName} {
		body, readErr := os.ReadFile(filepath.Join(s.engine.eventsDir, name))
		if errors.Is(readErr, os.ErrNotExist) {
			rows = append(rows, sourceRow{Name: "stream/" + name, SHA256: "absent"})
		} else if readErr != nil {
			return nil, readErr
		} else {
			rows = append(rows, sourceRow{Name: "stream/" + name, SHA256: recordHash(body)})
		}
		streamSeals[name], err = postCompletionRepairSeal(filepath.Join(s.engine.eventsDir, name))
		if err != nil {
			return nil, err
		}
		if streamSeals[name] != streamBefore[name] {
			return nil, fmt.Errorf("post-completion repair writer changed compatibility stream during source census")
		}
	}
	directoryAfter, err := postCompletionRepairSeal(s.rootPath())
	if err != nil {
		return nil, err
	}
	if directoryAfter != directoryBefore {
		return nil, fmt.Errorf("post-completion repair writer changed loose source during source census")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	writerFenceDigest, err := digestRepairValue(struct {
		Baseline PostCompletionRepairBaseline `json:"baseline"`
		Sources  []sourceRow                  `json:"sources"`
	}{Baseline: baseline, Sources: rows})
	if err != nil {
		return nil, err
	}
	if observe != nil {
		observe("segment_presence")
	}
	presence, err := s.engine.buildSegmentPresence()
	if err != nil {
		return nil, err
	}
	return &postCompletionRepairSourceCensus{
		Baseline: baseline, WriterFenceDigest: writerFenceDigest, Loose: actual, RawByID: rawByID,
		Presence: presence, DirectorySeal: directoryAfter, StreamSeals: streamSeals, CheckDirectorySeal: true,
	}, nil
}

func (s *Store) validatePostCompletionRepairSourceCensus(plan PostCompletionRepairPlan, allowProgress bool, census *postCompletionRepairSourceCensus) error {
	if census == nil || !reflect.DeepEqual(census.Baseline, plan.Baseline) {
		return fmt.Errorf("post-completion repair source state drift: frozen migration plan/manifest/completion changed")
	}
	planned := make(map[string]bool, len(plan.Entries))
	for _, entry := range plan.Entries {
		planned[entry.ID] = true
	}
	for id := range census.Loose {
		if !planned[id] {
			return fmt.Errorf("post-completion repair source state drift: unplanned loose record %s appeared", id)
		}
	}
	for _, entry := range plan.Entries {
		raw, sourceExists := census.RawByID[entry.ID]
		if !sourceExists || recordHash(raw) != entry.RawSHA256 {
			return fmt.Errorf("post-completion repair source state drift: loose %s bytes changed", entry.ID)
		}
		if _, looseExists := census.Loose[entry.ID]; !looseExists && !allowProgress {
			return fmt.Errorf("post-completion repair source state drift: loose %s vanished without tracked archive progress", entry.ID)
		}
		loc, present := census.Presence[entry.ID]
		switch entry.Disposition {
		case RepairImportAbsent:
			if present && (!allowProgress || loc.sha256 != entry.CanonicalSHA256) {
				return fmt.Errorf("post-completion repair source state drift: segment %s no longer matches planned absence", entry.ID)
			}
		case RepairArchiveIdentical, RepairArchiveDivergent:
			if !present || loc.sha256 != entry.SegmentSHA256 || loc.relPath != entry.Segment || loc.offset != entry.SegmentOffset {
				return fmt.Errorf("post-completion repair source state drift: segment authority %s changed", entry.ID)
			}
		default:
			return fmt.Errorf("post-completion repair plan %s has unknown disposition %q", entry.ID, entry.Disposition)
		}
	}
	return nil
}

func (s *Store) preflightPostCompletionRepair(plan PostCompletionRepairPlan, allowProgress bool) error {
	baseline, err := s.postCompletionRepairBaseline()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(baseline, plan.Baseline) {
		return fmt.Errorf("post-completion repair source state drift: frozen migration plan/manifest/completion changed")
	}
	m := &migrator{store: s, engine: s.engine}
	loose, err := m.enumerateLoose()
	if err != nil {
		return err
	}
	actual := map[string]looseFile{}
	for _, lf := range loose {
		actual[lf.id] = lf
	}
	planned := map[string]PostCompletionRepairEntry{}
	for _, entry := range plan.Entries {
		planned[entry.ID] = entry
	}
	for id := range actual {
		if _, ok := planned[id]; !ok {
			return fmt.Errorf("post-completion repair source state drift: unplanned loose record %s appeared", id)
		}
	}
	presence, err := s.engine.buildSegmentPresence()
	if err != nil {
		return err
	}
	for _, entry := range plan.Entries {
		lf, looseExists := actual[entry.ID]
		archiveRaw, archiveErr := s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID)
		archiveExists := archiveErr == nil
		if archiveErr != nil && !errors.Is(archiveErr, os.ErrNotExist) {
			return archiveErr
		}
		if archiveExists && (recordHash(archiveRaw) != entry.RawSHA256) {
			return fmt.Errorf("post-completion repair source state drift: archive for %s has different bytes", entry.ID)
		}
		if looseExists {
			if err := validatePostCompletionRepairLooseRegular(lf.path, entry.ID); err != nil {
				return err
			}
			raw, err := os.ReadFile(lf.path)
			if err != nil {
				return err
			}
			if recordHash(raw) != entry.RawSHA256 {
				return fmt.Errorf("post-completion repair source state drift: loose %s bytes changed", entry.ID)
			}
		} else if !allowProgress || !archiveExists {
			return fmt.Errorf("post-completion repair source state drift: loose %s vanished without tracked archive progress", entry.ID)
		}
		loc, present := presence[entry.ID]
		switch entry.Disposition {
		case RepairImportAbsent:
			if present && (!allowProgress || loc.sha256 != entry.CanonicalSHA256) {
				return fmt.Errorf("post-completion repair source state drift: segment %s no longer matches planned absence", entry.ID)
			}
		case RepairArchiveIdentical, RepairArchiveDivergent:
			if !present || loc.sha256 != entry.SegmentSHA256 || loc.relPath != entry.Segment || loc.offset != entry.SegmentOffset {
				return fmt.Errorf("post-completion repair source state drift: segment authority %s changed", entry.ID)
			}
		default:
			return fmt.Errorf("post-completion repair plan %s has unknown disposition %q", entry.ID, entry.Disposition)
		}
	}
	return nil
}

func (s *Store) reservePostCompletionRepairFrame(entry PostCompletionRepairEntry) (string, int64, error) {
	path, _, err := s.engine.appendTarget(entry.Family, ingestionDay(s.engine.clock()))
	if err != nil {
		return "", 0, err
	}
	offset := int64(0)
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", 0, err
	}
	rel, err := filepath.Rel(s.engine.eventsDir, path)
	if err != nil {
		return "", 0, err
	}
	return rel, offset, nil
}

// verifyPostCompletionRepairFrame rereads the exact reserved frame, including
// its canonical body. It distinguishes "not appended yet" (the file ends at
// the reservation) from drift/corruption after that coordinate.
func (s *Store) verifyPostCompletionRepairFrame(entry PostCompletionRepairEntry, rel string, offset int64) (bool, error) {
	path := filepath.Join(s.engine.eventsDir, filepath.Clean(rel))
	if !strings.HasPrefix(path, filepath.Clean(s.engine.segRoot)+string(os.PathSeparator)) {
		return false, fmt.Errorf("post-completion repair reserved segment escapes segment root")
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && offset == 0 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if int64(len(body)) == offset {
		return false, nil
	}
	if int64(len(body)) < offset {
		return false, fmt.Errorf("post-completion repair reserved segment shrank before append")
	}
	frames, err := parseSegmentBytes(path, body, true)
	if err != nil {
		return false, err
	}
	for _, parsed := range frames {
		if parsed.Offset != offset {
			continue
		}
		canonicalRecord, err := canonical(parsed.Frame.Record)
		if err != nil {
			return false, err
		}
		if parsed.Frame.ID != entry.ID || parsed.Frame.Family != entry.Family || parsed.Frame.SHA256 != entry.CanonicalSHA256 || recordHash(canonicalRecord) != entry.CanonicalSHA256 {
			return false, fmt.Errorf("post-completion repair reserved frame identity mismatch at %s:%d", rel, offset)
		}
		return true, nil
	}
	return false, fmt.Errorf("post-completion repair reserved coordinate %s:%d was consumed by different bytes", rel, offset)
}

// recoverPostCompletionRepairReservedFrame accepts only one crash shape: the
// bytes after the reserved coordinate are an exact, incomplete prefix of the
// sealed frame and nothing follows them. It truncates to the last verified
// boundary, re-appends the exact reserved bytes, fsyncs, and rereads. Any
// different or later byte refuses instead of guessing at segment history.
func (s *Store) recoverPostCompletionRepairReservedFrame(entry PostCompletionRepairEntry, rel string, offset int64, line []byte) (found, recovered bool, err error) {
	if found, verifyErr := s.verifyPostCompletionRepairFrame(entry, rel, offset); verifyErr == nil {
		return found, false, nil
	}
	path, err := s.postCompletionRepairReservedSegmentPath(rel)
	if err != nil {
		return false, false, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false, false, err
	}
	if offset < 0 || offset > int64(len(body)) {
		return false, false, fmt.Errorf("post-completion repair cannot safely recover reserved torn frame at %s:%d", rel, offset)
	}
	tail := body[offset:]
	if len(tail) == 0 {
		return false, false, nil
	}
	if len(tail) >= len(line) || !bytes.Equal(tail, line[:len(tail)]) {
		return false, false, fmt.Errorf("post-completion repair cannot safely recover reserved torn frame at %s:%d: later or different bytes follow reservation", rel, offset)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return false, false, err
	}
	if err := file.Truncate(offset); err != nil {
		_ = file.Close()
		return false, false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, false, err
	}
	if err := file.Close(); err != nil {
		return false, false, err
	}
	if err := s.appendPostCompletionRepairReservedFrame(rel, offset, line); err != nil {
		return false, false, err
	}
	found, err = s.verifyPostCompletionRepairFrame(entry, rel, offset)
	if err != nil || !found {
		return false, true, errors.Join(fmt.Errorf("post-completion repair recovered reserved frame failed exact reread"), err)
	}
	return true, true, nil
}

func (s *Store) postCompletionRepairReservedSegmentPath(rel string) (string, error) {
	path := filepath.Join(s.engine.eventsDir, filepath.Clean(rel))
	if !strings.HasPrefix(path, filepath.Clean(s.engine.segRoot)+string(os.PathSeparator)) {
		return "", fmt.Errorf("post-completion repair reserved segment escapes segment root")
	}
	return path, nil
}

func (s *Store) appendPostCompletionRepairReservedFrame(rel string, offset int64, line []byte) error {
	path, err := s.postCompletionRepairReservedSegmentPath(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	created := false
	if info, statErr := file.Stat(); statErr != nil {
		_ = file.Close()
		return statErr
	} else if info.Size() != offset {
		_ = file.Close()
		return fmt.Errorf("post-completion repair reserved segment size drift: got %d want %d", info.Size(), offset)
	} else {
		created = info.Size() == 0 && offset == 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		_ = file.Close()
		return err
	}
	if err := writeFull(file, line); err != nil {
		_ = file.Truncate(offset)
		_ = file.Sync()
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if created {
		return syncDirectory(filepath.Dir(path))
	}
	return nil
}

func (s *Store) preflightPostCompletionRepairFinal(plan PostCompletionRepairPlan) error {
	if err := s.preflightPostCompletionRepair(plan, true); err != nil {
		return err
	}
	presence, err := s.engine.buildSegmentPresence()
	if err != nil {
		return err
	}
	return s.preflightPostCompletionRepairFinalWithPresence(plan, presence)
}

func (s *Store) preflightPostCompletionRepairFinalWithPresence(plan PostCompletionRepairPlan, presence map[string]segmentLocation) error {
	for _, entry := range plan.Entries {
		loosePath := filepath.Join(s.rootPath(), entry.FileName)
		if info, err := os.Lstat(loosePath); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("post-completion repair final loose %s remains and is not a regular file", entry.ID)
			}
			return fmt.Errorf("post-completion repair final loose %s remains", entry.ID)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("post-completion repair final loose %s state unreadable: %w", entry.ID, err)
		}
		archive, err := s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID)
		if err != nil || recordHash(archive) != entry.RawSHA256 {
			return fmt.Errorf("post-completion repair final archive %s unverified: %w", entry.ID, err)
		}
		loc, ok := presence[entry.ID]
		want := entry.SegmentSHA256
		if entry.Disposition == RepairImportAbsent {
			want = entry.CanonicalSHA256
		}
		if !ok || loc.sha256 != want {
			return fmt.Errorf("post-completion repair final segment %s unverified", entry.ID)
		}
	}
	return nil
}

func (s *Store) postCompletionRepairSourceBytes(entry PostCompletionRepairEntry) ([]byte, error) {
	loosePath := filepath.Join(s.rootPath(), entry.FileName)
	raw, err := os.ReadFile(loosePath)
	if err == nil {
		if recordHash(raw) != entry.RawSHA256 {
			return nil, fmt.Errorf("post-completion repair source state drift: loose %s bytes changed", entry.ID)
		}
		return raw, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw, err = s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID)
	if err != nil || recordHash(raw) != entry.RawSHA256 {
		return nil, fmt.Errorf("post-completion repair tracked source %s unavailable: %w", entry.ID, err)
	}
	return raw, nil
}

func (s *Store) writePostCompletionRepairArchive(entry PostCompletionRepairEntry, raw []byte) error {
	if recordHash(raw) != entry.RawSHA256 {
		return fmt.Errorf("post-completion repair archive input %s hash mismatch", entry.ID)
	}
	path := s.postCompletionRepairArchivePath(entry.RawSHA256)
	if existing, err := s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID); err == nil {
		if !bytes.Equal(existing, raw) {
			return fmt.Errorf("post-completion repair archive %s already exists with different bytes", entry.ID)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	created, err := writeBytesExclusiveAtomic(path, raw)
	if err != nil {
		return err
	}
	if !created {
		existing, err := s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID)
		if err != nil || !bytes.Equal(existing, raw) {
			return fmt.Errorf("post-completion repair archive %s raced with different bytes: %w", entry.ID, err)
		}
	}
	return nil
}

func (s *Store) readPostCompletionRepairArchive(rawSHA, id string) ([]byte, error) {
	raw, err := homepkg.ReadRootedRegularFile(s.home, s.postCompletionRepairArchivePath(rawSHA), "post-completion repair archive")
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return raw, err
	}
	return nil, fmt.Errorf("post-completion repair archive custody for %s is not a regular file: %w", id, err)
}

func (s *Store) claimPostCompletionRepairLoose(planID string, entry PostCompletionRepairEntry, beforeRename, afterValidation func(string) error) (postCompletionRepairFileSeal, error) {
	var zero postCompletionRepairFileSeal
	sourcePath := filepath.Join(s.rootPath(), entry.FileName)
	claimPath := s.postCompletionRepairClaimPath(planID, entry)
	claimDir := filepath.Dir(claimPath)
	if err := s.ensurePostCompletionRepairClaimDir(claimDir); err != nil {
		return zero, err
	}
	claimed, err := s.readPostCompletionRepairClaim(claimPath, entry.ID)
	if errors.Is(err, os.ErrNotExist) {
		if err := validatePostCompletionRepairLooseRegular(sourcePath, entry.ID); err != nil {
			return zero, err
		}
		if beforeRename != nil {
			if err := beforeRename(entry.ID); err != nil {
				return zero, err
			}
		}
		if err := os.Rename(sourcePath, claimPath); err != nil {
			return zero, fmt.Errorf("post-completion repair claim loose %s: %w", entry.ID, err)
		}
		if err := syncDirectory(s.rootPath()); err != nil {
			return zero, err
		}
		if err := syncDirectory(claimDir); err != nil {
			return zero, err
		}
		claimed, err = s.readPostCompletionRepairClaim(claimPath, entry.ID)
		if err != nil {
			return zero, errors.Join(err, restorePostCompletionRepairClaim(claimPath, sourcePath))
		}
	}
	if err != nil {
		return zero, err
	}
	if recordHash(claimed) != entry.RawSHA256 {
		return zero, fmt.Errorf("post-completion repair source state drift: quarantined loose %s does not match planned bytes", entry.ID)
	}
	if err := s.writePostCompletionRepairArchive(entry, claimed); err != nil {
		return zero, err
	}
	expectedSeal, err := postCompletionRepairSeal(s.rootPath())
	if err != nil {
		return zero, err
	}
	if afterValidation != nil {
		if err := afterValidation(entry.ID); err != nil {
			return zero, err
		}
	}
	if _, err := os.Lstat(sourcePath); err == nil {
		return zero, fmt.Errorf("post-completion repair source state drift: loose %s pathname was recreated after atomic claim", entry.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return zero, err
	}
	currentSeal, err := postCompletionRepairSeal(s.rootPath())
	if err != nil {
		return zero, err
	}
	if currentSeal != expectedSeal {
		return zero, fmt.Errorf("post-completion repair loose source directory changed outside expected claim delta")
	}
	return currentSeal, nil
}

func (s *Store) readPostCompletionRepairClaim(path, id string) ([]byte, error) {
	raw, err := homepkg.ReadRootedRegularFile(s.home, path, "post-completion repair quarantine claim")
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return raw, err
	}
	return nil, fmt.Errorf("post-completion repair source state drift: quarantined loose %s is not a regular file: %w", id, err)
}

func restorePostCompletionRepairClaim(claimPath, sourcePath string) error {
	info, err := os.Lstat(claimPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// target must be hoisted: a := here would shadow the outer err, so a failed
		// Symlink would read as success at the check below and the quarantined claim
		// — possibly the only surviving copy — would be destroyed after syncDirectory.
		var target string
		target, err = os.Readlink(claimPath)
		if err != nil {
			return err
		}
		err = os.Symlink(target, sourcePath)
	} else if info.Mode().IsRegular() {
		err = os.Link(claimPath, sourcePath)
	} else {
		return fmt.Errorf("restore refused post-completion repair claim: unsupported custody mode %s", info.Mode())
	}
	if err != nil {
		return fmt.Errorf("restore refused post-completion repair claim: %w", err)
	}
	if err := syncDirectory(filepath.Dir(sourcePath)); err != nil {
		return err
	}
	if err := os.Remove(claimPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(claimPath))
}

func (s *Store) ensurePostCompletionRepairClaimDir(claimDir string) error {
	return homepkg.EnsureRootedDirectoryDurable(s.home, claimDir, "post-completion repair quarantine plan directory", 0o755)
}

func (s *Store) cleanupPostCompletionRepairClaim(planID string, entry PostCompletionRepairEntry) error {
	path := s.postCompletionRepairClaimPath(planID, entry)
	// Inode/hash-bound removal: the claim leaves custody only as the exact
	// planned bytes, and a foreign file swapped onto the claim pathname is
	// never deleted (rooted quarantine re-verifies identity through unlink).
	err := homepkg.RemoveRootedRegularFileDurableValidated(s.home, path, "post-completion repair quarantine claim", func(body []byte) error {
		if recordHash(body) != entry.RawSHA256 {
			return fmt.Errorf("post-completion repair claim %s does not match planned bytes", entry.ID)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	claimDir := filepath.Dir(path)
	quarantineDir := filepath.Dir(claimDir)
	if err := os.Remove(claimDir); err == nil {
		if err := syncDirectory(quarantineDir); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	if err := os.Remove(quarantineDir); err == nil {
		return syncDirectory(filepath.Dir(quarantineDir))
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
}

func (s *Store) buildPostCompletionRepairResult(plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization, history []PostCompletionRepairAuthorization, now time.Time) (PostCompletionRepairResult, error) {
	presence, err := s.engine.buildSegmentPresence()
	if err != nil {
		return PostCompletionRepairResult{}, err
	}
	return s.buildPostCompletionRepairResultWithPresence(plan, authorization, history, now, presence)
}

func (s *Store) buildPostCompletionRepairResultWithPresence(plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization, history []PostCompletionRepairAuthorization, now time.Time, presence map[string]segmentLocation) (PostCompletionRepairResult, error) {
	entries := make([]PostCompletionRepairResultEntry, 0, len(plan.Entries))
	for _, planned := range plan.Entries {
		loc, ok := presence[planned.ID]
		if !ok {
			return PostCompletionRepairResult{}, fmt.Errorf("post-completion repair result segment %s absent", planned.ID)
		}
		entries = append(entries, PostCompletionRepairResultEntry{
			ID: planned.ID, Disposition: planned.Disposition, ArchiveSHA256: planned.RawSHA256,
			CanonicalSHA256: planned.CanonicalSHA256, SegmentCanonicalSHA256: loc.sha256,
			Segment: loc.relPath, SegmentOffset: loc.offset,
		})
	}
	result := PostCompletionRepairResult{
		SchemaVersion: postCompletionRepairResultSchema,
		ID:            "esrm_" + strings.TrimPrefix(plan.PlanDigest, "sha256:")[:12],
		CompletedAt:   now.UTC().Format(time.RFC3339Nano),
		PlanID:        plan.ID, PlanDigest: plan.PlanDigest, SourceDigest: plan.SourceDigest,
		Authorization:        authorization,
		AuthorizationHistory: append([]PostCompletionRepairAuthorization(nil), history...),
		Total:                plan.Total, Counts: cloneRepairCounts(plan.Counts), Entries: entries,
		VerificationRefs: []string{postCompletionRepairVerificationRef}, Changed: true,
	}
	resultDigest, err := postCompletionRepairResultDigest(result)
	if err != nil {
		return PostCompletionRepairResult{}, err
	}
	result.ResultDigest = resultDigest
	if err := s.validatePostCompletionRepairResult(result, plan, authorization); err != nil {
		return PostCompletionRepairResult{}, err
	}
	return result, nil
}

func (s *Store) ReadPostCompletionRepairPending(planID string) (PostCompletionRepairPending, bool, error) {
	var pending PostCompletionRepairPending
	if filepath.Base(planID) != planID || !strings.HasPrefix(planID, "esrp_") {
		return pending, false, fmt.Errorf("post-completion repair plan id invalid")
	}
	pending, err := loadStrictRootedJSON[PostCompletionRepairPending](s.home, s.postCompletionRepairPendingPath(planID), "post-completion repair pending snapshot")
	if errors.Is(err, os.ErrNotExist) {
		return PostCompletionRepairPending{}, false, nil
	}
	if err != nil {
		return PostCompletionRepairPending{}, false, err
	}
	plan, err := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(planID))
	if err != nil {
		return PostCompletionRepairPending{}, false, err
	}
	state, err := s.loadPostCompletionRepairProgressCurrent(plan)
	if err != nil {
		// An unrooted mutable v0.1 snapshot is accepted only as upgrade input.
		// It is not current journal authority and migrate --verify still reports
		// it as an orphan until Apply converts it under the run lock.
		if errors.Is(err, os.ErrNotExist) {
			root, _, rootExists, rootErr := s.loadPostCompletionRepairRoot()
			if rootErr == nil {
				rootedPending := postCompletionRepairRootOwns(root, "pending", planID)
				rootedProgress := postCompletionRepairRootOwns(root, "progress", planID)
				if !rootedPending && !rootedProgress {
					if legacyErr := validatePostCompletionRepairSnapshot(pending, plan, pending.Authorization); legacyErr != nil {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair unrooted pending snapshot invalid: %w", legacyErr)
					}
					return pending, true, nil
				} else if rootExists && rootedPending && rootedProgress {
					if snapshotErr := validatePostCompletionRepairSnapshot(pending, plan, pending.Authorization); snapshotErr != nil {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair pending snapshot invalid during cleanup recovery: %w", snapshotErr)
					}
					result, resultExists, resultErr := s.ReadPostCompletionRepairResult(planID)
					if resultErr != nil {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair progress journal missing before valid durable result: %w", resultErr)
					}
					if !resultExists {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair progress journal missing before durable result")
					}
					if resultErr := s.validatePostCompletionRepairResult(result, plan, result.Authorization); resultErr != nil {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair progress journal missing before valid durable result: %w", resultErr)
					}
					if len(result.AuthorizationHistory) == 0 || !postCompletionRepairAuthorizationEqual(result.AuthorizationHistory[0], pending.Authorization) {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair progress journal missing with a result that does not start from the immutable pending authorization")
					}
					proof, proofExists, proofErr := s.ReadPostCompletionRepairProof(planID)
					if proofErr != nil {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair progress journal missing before valid work proof: %w", proofErr)
					}
					if !proofExists {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair progress journal missing before durable work proof")
					}
					if proofErr := s.validatePostCompletionRepairProof(proof, result); proofErr != nil {
						return PostCompletionRepairPending{}, false, fmt.Errorf("post-completion repair progress journal missing before valid work proof: %w", proofErr)
					}
					return pending, true, nil
				}
			}
		}
		return PostCompletionRepairPending{}, false, err
	}
	return state.Current, true, nil
}

// PostCompletionRepairPhysicalCompletionForFinalization is a read-only proof
// used by the CLI before selecting sealed historical authority. It returns
// true only when the rooted progress journal and current authorization agree,
// every physical repair phase is complete, and the durable result is the only
// missing artifact. ApplyPostCompletionRepair repeats the proof under its
// mutation locks before it permits result finalization without current policy.
func (s *Store) PostCompletionRepairPhysicalCompletionForFinalization(planPath string, authorization PostCompletionRepairAuthorization) (complete bool, retErr error) {
	if s == nil || s.engine == nil {
		return false, fmt.Errorf("event store requires a home")
	}
	plan, err := s.loadPostCompletionRepairPlan(planPath)
	if err != nil {
		return false, err
	}
	if err := validatePostCompletionRepairAuthorization(authorization, plan); err != nil {
		return false, err
	}
	review, err := s.loadPostCompletionRepairReview(s.postCompletionRepairReviewPath(plan.ID), plan)
	if err != nil {
		return false, fmt.Errorf("post-completion repair exact operator review required: %w", err)
	}
	if review.ID != authorization.ReviewID || review.ReviewDigest != authorization.ReviewDigest || review.WriterFenceDigest != authorization.WriterFenceDigest {
		return false, fmt.Errorf("post-completion repair authorization does not bind exact operator review")
	}
	m := &migrator{store: s, engine: s.engine}
	runLock, err := m.acquireRunLockBlocking()
	if err != nil {
		return false, err
	}
	defer runLock.release()
	releaseAll, err := s.engine.lockAllFamilyShards()
	if err != nil {
		return false, err
	}
	defer func() {
		retErr = errors.Join(retErr, releaseAll())
	}()
	return s.postCompletionRepairPhysicalCompletionForFinalization(plan, authorization)
}

func (s *Store) postCompletionRepairPhysicalCompletionForFinalization(plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization) (bool, error) {
	if _, err := homepkg.ReadRootedRegularFile(s.home, s.postCompletionRepairResultPath(plan.ID), "post-completion repair result"); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	state, err := s.loadPostCompletionRepairProgress(plan, authorization)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range state.Current.Entries {
		if entry.Phase != repairPhaseRemoved {
			return false, nil
		}
	}
	root, _, rootExists, err := s.loadPostCompletionRepairRoot()
	if err != nil {
		return false, err
	}
	anchor, anchorExists, err := s.loadPostCompletionRepairAnchor()
	if err != nil {
		return false, err
	}
	if !rootExists || !anchorExists {
		return false, fmt.Errorf("post-completion repair rooted progress authority is required for sealed finalization")
	}
	if err := verifyPostCompletionRepairRootAnchor(root, rootExists, anchor, anchorExists); err != nil {
		return false, err
	}
	if err := s.verifyPostCompletionRepairRootArtifactBytes(root); err != nil {
		return false, err
	}
	var planRooted, reviewRooted, pendingRooted, progressRooted bool
	for _, artifact := range root.Artifacts {
		switch {
		case artifact.Kind == "plan" && artifact.ID == plan.ID:
			planRooted = artifact.Path == filepath.Base(s.postCompletionRepairPlanPath(plan.ID))
		case artifact.Kind == "review" && artifact.ID == authorization.ReviewID:
			reviewRooted = artifact.Path == filepath.Base(s.postCompletionRepairReviewPath(plan.ID))
		case artifact.Kind == "pending" && artifact.ID == plan.ID:
			pendingRooted = artifact.Path == filepath.Base(s.postCompletionRepairPendingPath(plan.ID))
		case artifact.Kind == "progress" && artifact.ID == plan.ID:
			progressRooted = artifact.Path == filepath.Base(s.postCompletionRepairProgressPath(plan.ID)) && postCompletionRepairProgressAuthorizationHeadCurrent(artifact, state)
		}
	}
	if !planRooted || !reviewRooted || !pendingRooted || !progressRooted {
		return false, fmt.Errorf("post-completion repair plan, review, pending progress, and current authorization head must all be rooted so result is the only missing artifact for sealed finalization")
	}
	if err := s.verifyPostCompletionRepairProgressPhysical(plan, state.Current); err != nil {
		return false, err
	}
	if err := s.preflightPostCompletionRepairFinal(plan); err != nil {
		return false, err
	}
	return true, nil
}

// PostCompletionRepairPlanForApply resolves and validates the immutable plan
// before cmd/compound evaluates current policy for the concrete work target.
func (s *Store) PostCompletionRepairPlanForApply(planPath string) (PostCompletionRepairPlan, error) {
	return s.loadPostCompletionRepairPlan(planPath)
}

// ReadPostCompletionRepairPlans discovers immutable repair plans without
// creating the repair directory or consulting directory entries as authority.
// Status needs this narrow read because every existing lifecycle accessor is
// plan-addressed, while a caller may intentionally omit --plan. Each returned
// plan still passes the same rooted regular-file and canonical-path validation
// as PostCompletionRepairPlanForApply.
func (s *Store) ReadPostCompletionRepairPlans() ([]PostCompletionRepairPlanRecord, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("event store requires a home")
	}
	root := s.engine.postCompletionRepairRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []PostCompletionRepairPlanRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	plans := make([]PostCompletionRepairPlanRecord, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".plan.json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		plan, err := s.PostCompletionRepairPlanForApply(path)
		if err != nil {
			return nil, fmt.Errorf("read post-completion repair plan %s: %w", entry.Name(), err)
		}
		plans = append(plans, PostCompletionRepairPlanRecord{Plan: plan, PlanPath: path})
	}
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].Plan.CreatedAt != plans[j].Plan.CreatedAt {
			return plans[i].Plan.CreatedAt < plans[j].Plan.CreatedAt
		}
		return plans[i].Plan.ID < plans[j].Plan.ID
	})
	return plans, nil
}

// SupersedePostCompletionRepair terminalizes a reviewed plan only while it is
// still at zero progress. The successor must be an immutable broader plan that
// preserves every old entry exactly; current policy authority is validated
// immediately before the terminal artifact is written.
func (s *Store) SupersedePostCompletionRepair(oldPlanPath, successorPlanPath string, authorization PostCompletionRepairAuthorization, now time.Time, validateAuthority func() error) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("event store requires a home")
	}
	if validateAuthority == nil {
		return fmt.Errorf("post-completion repair supersession requires current policy authority")
	}
	m := &migrator{store: s, engine: s.engine}
	runLock, err := m.acquireRunLockBlocking()
	if err != nil {
		return err
	}
	defer runLock.release()

	oldPlan, err := s.loadPostCompletionRepairPlan(oldPlanPath)
	if err != nil {
		return err
	}
	successorPlan, err := s.loadPostCompletionRepairPlan(successorPlanPath)
	if err != nil {
		return err
	}
	review, err := s.loadPostCompletionRepairReview(s.postCompletionRepairReviewPath(oldPlan.ID), oldPlan)
	if err != nil {
		return fmt.Errorf("post-completion repair exact operator review required before supersession: %w", err)
	}
	if err := validatePostCompletionRepairAuthorization(authorization, oldPlan); err != nil {
		return err
	}
	if review.ID != authorization.ReviewID || review.ReviewDigest != authorization.ReviewDigest || review.WriterFenceDigest != authorization.WriterFenceDigest {
		return fmt.Errorf("post-completion repair supersession authorization does not bind exact operator review")
	}
	if err := validatePostCompletionRepairSuccessor(oldPlan, successorPlan); err != nil {
		return err
	}
	root, _, exists, err := s.loadPostCompletionRepairRoot()
	if err != nil || !exists {
		return errors.Join(fmt.Errorf("post-completion repair root required for supersession"), err)
	}
	if !postCompletionRepairRootOwns(root, "plan", oldPlan.ID) || !postCompletionRepairRootOwns(root, "review", review.ID) || !postCompletionRepairRootOwns(root, "plan", successorPlan.ID) {
		return fmt.Errorf("post-completion repair old plan, review, and successor plan must be rooted before supersession")
	}
	quarantinePlanPath := filepath.Join(s.engine.postCompletionRepairRoot(), "quarantine", oldPlan.ID)
	quarantinePlanExists, quarantineErr := homepkg.RootedDirectoryExists(s.home, quarantinePlanPath, "post-completion repair quarantine plan directory")
	if quarantineErr != nil {
		return quarantineErr
	}
	if quarantinePlanExists {
		return fmt.Errorf("post-completion repair supersession requires zero progress; unowned quarantine entry for %s already exists", oldPlan.ID)
	}
	for _, artifact := range root.Artifacts {
		if planID, ok := postCompletionRepairLifecycleArtifactPlanID(artifact); ok && planID == oldPlan.ID {
			return fmt.Errorf("post-completion repair supersession requires zero progress; %s boundary already exists", artifact.Kind)
		}
	}
	for _, artifact := range []struct {
		path string
		kind string
	}{
		{path: s.postCompletionRepairPendingPath(oldPlan.ID), kind: "pending snapshot"},
		{path: s.postCompletionRepairProgressPath(oldPlan.ID), kind: "progress journal"},
		{path: s.postCompletionRepairResultPath(oldPlan.ID), kind: "result"},
		{path: s.postCompletionRepairProofPath(oldPlan.ID), kind: "proof"},
	} {
		exists, custodyErr := s.postCompletionRepairControlArtifactExists(artifact.path, artifact.kind)
		if custodyErr != nil {
			return custodyErr
		}
		if exists {
			return fmt.Errorf("post-completion repair supersession requires zero progress; artifact %s already exists", filepath.Base(artifact.path))
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	supersession := PostCompletionRepairSupersession{
		SchemaVersion: postCompletionRepairSupersessionSchema,
		ID:            "esrs_" + strings.TrimPrefix(oldPlan.PlanDigest, "sha256:")[:12],
		CreatedAt:     now.UTC().Format(time.RFC3339Nano),
		PlanID:        oldPlan.ID, PlanDigest: oldPlan.PlanDigest,
		ReviewID: review.ID, ReviewDigest: review.ReviewDigest,
		Authorization:   authorization,
		SuccessorPlanID: successorPlan.ID, SuccessorPlanDigest: successorPlan.PlanDigest,
		SuccessorSourceDigest: successorPlan.SourceDigest,
	}
	supersession.SupersessionDigest, err = postCompletionRepairSupersessionDigest(supersession)
	if err != nil {
		return err
	}
	if err := validatePostCompletionRepairSupersession(supersession, oldPlan, review, successorPlan); err != nil {
		return err
	}
	path := s.postCompletionRepairSupersessionPath(oldPlan.ID)
	durableExact := false
	if existing, loadErr := loadStrictRootedJSON[PostCompletionRepairSupersession](s.home, path, "post-completion repair supersession"); loadErr == nil {
		candidate := supersession
		candidate.CreatedAt = existing.CreatedAt
		candidate.SupersessionDigest, _ = postCompletionRepairSupersessionDigest(candidate)
		if !reflect.DeepEqual(existing, candidate) {
			return fmt.Errorf("post-completion repair supersession already exists with different content")
		}
		if err := validatePostCompletionRepairSupersession(existing, oldPlan, review, successorPlan); err != nil {
			return fmt.Errorf("post-completion repair durable supersession invalid: %w", err)
		}
		supersession = existing
		durableExact = true
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	if !durableExact {
		if err := validateAuthority(); err != nil {
			return fmt.Errorf("post-completion repair current policy authority changed before supersession: %w", err)
		}
		if err := s.writePostCompletionRepairExclusiveOrEqualJSON(path, "post-completion repair supersession", supersession); err != nil {
			return err
		}
		if err := postCompletionRepairSupersessionAfterFile(); err != nil {
			return err
		}
	}
	return s.ensurePostCompletionRepairArtifact("supersession", oldPlan.ID, path)
}

func (s *Store) postCompletionRepairControlArtifactExists(path, kind string) (bool, error) {
	_, err := homepkg.ReadRootedRegularFile(s.home, path, "post-completion repair "+kind)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func validatePostCompletionRepairSuccessor(oldPlan, successor PostCompletionRepairPlan) error {
	if successor.ID == oldPlan.ID || successor.Total <= oldPlan.Total || !reflect.DeepEqual(successor.Baseline, oldPlan.Baseline) {
		return fmt.Errorf("post-completion repair successor must be a broader plan over the same frozen baseline")
	}
	successorEntries := make(map[string]PostCompletionRepairEntry, len(successor.Entries))
	for _, entry := range successor.Entries {
		successorEntries[entry.ID] = entry
	}
	for _, entry := range oldPlan.Entries {
		if successorEntry, ok := successorEntries[entry.ID]; !ok || !reflect.DeepEqual(successorEntry, entry) {
			return fmt.Errorf("post-completion repair successor does not preserve old entry %s exactly", entry.ID)
		}
	}
	return nil
}

func validatePostCompletionRepairSupersession(supersession PostCompletionRepairSupersession, oldPlan PostCompletionRepairPlan, review PostCompletionRepairReview, successor PostCompletionRepairPlan) error {
	if supersession.SchemaVersion != postCompletionRepairSupersessionSchema || supersession.ID != "esrs_"+strings.TrimPrefix(oldPlan.PlanDigest, "sha256:")[:12] || supersession.PlanID != oldPlan.ID || supersession.PlanDigest != oldPlan.PlanDigest || supersession.ReviewID != review.ID || supersession.ReviewDigest != review.ReviewDigest || supersession.SuccessorPlanID != successor.ID || supersession.SuccessorPlanDigest != successor.PlanDigest || supersession.SuccessorSourceDigest != successor.SourceDigest {
		return fmt.Errorf("post-completion repair supersession identity does not bind exact history and successor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, supersession.CreatedAt)
	if err != nil {
		return fmt.Errorf("post-completion repair supersession created_at invalid: %w", err)
	}
	if err := validatePostCompletionRepairAuthorization(supersession.Authorization, oldPlan); err != nil {
		return err
	}
	if supersession.Authorization.ReviewID != review.ID || supersession.Authorization.ReviewDigest != review.ReviewDigest || supersession.Authorization.WriterFenceDigest != review.WriterFenceDigest {
		return fmt.Errorf("post-completion repair supersession authorization does not bind exact review")
	}
	if err := validatePostCompletionRepairSuccessor(oldPlan, successor); err != nil {
		return err
	}
	predecessors := []struct {
		name, createdAt string
	}{
		{name: "old plan", createdAt: oldPlan.CreatedAt},
		{name: "operator review", createdAt: review.CreatedAt},
	}
	if authorizedAt := strings.TrimSpace(supersession.Authorization.AuthorizedAt); authorizedAt != "" {
		predecessors = append(predecessors, struct{ name, createdAt string }{name: "authorization", createdAt: authorizedAt})
	}
	predecessors = append(predecessors, struct{ name, createdAt string }{name: "successor plan", createdAt: successor.CreatedAt})
	for _, predecessor := range predecessors {
		predecessorAt, err := time.Parse(time.RFC3339Nano, predecessor.createdAt)
		if err != nil {
			return fmt.Errorf("post-completion repair %s created_at invalid: %w", predecessor.name, err)
		}
		if createdAt.Before(predecessorAt) {
			return fmt.Errorf("post-completion repair supersession created_at precedes %s", predecessor.name)
		}
	}
	if expected, err := postCompletionRepairSupersessionDigest(supersession); err != nil || expected != supersession.SupersessionDigest {
		return errors.Join(fmt.Errorf("post-completion repair supersession digest mismatch"), err)
	}
	return nil
}

func (s *Store) ReauthorizePostCompletionRepair(planPath string, previous, next PostCompletionRepairAuthorization, validateAuthority func() error) error {
	if validateAuthority == nil {
		return fmt.Errorf("post-completion repair reauthorization requires current policy authority")
	}
	plan, err := s.loadPostCompletionRepairPlan(planPath)
	if err != nil {
		return err
	}
	if err := validatePostCompletionRepairReauthorization(previous, next, plan); err != nil {
		return err
	}
	m := &migrator{store: s, engine: s.engine}
	runLock, err := m.acquireRunLockBlocking()
	if err != nil {
		return err
	}
	defer runLock.release()
	if err := validateAuthority(); err != nil {
		return err
	}
	state, err := s.loadPostCompletionRepairProgressCurrent(plan)
	if errors.Is(err, os.ErrNotExist) {
		state, err = s.loadOrUpgradePostCompletionRepairProgress(plan, previous)
	}
	if err != nil {
		return err
	}
	if len(state.TornTail) > 0 {
		if err := s.recoverPostCompletionRepairTornIntent(&state, plan, previous, next); err != nil {
			return err
		}
	}
	if postCompletionRepairAuthorizationEqual(state.Current.Authorization, next) {
		return s.ensurePostCompletionRepairProgressArtifact(plan.ID, state)
	}
	if !postCompletionRepairAuthorizationEqual(state.Current.Authorization, previous) {
		return fmt.Errorf("post-completion repair current authorization is not the expected predecessor")
	}
	if err := validateAuthority(); err != nil {
		return err
	}
	if err := s.advancePostCompletionRepairAuthorization(&state, plan, next); err != nil {
		return err
	}
	return s.ensurePostCompletionRepairProgressArtifact(plan.ID, state)
}

func (s *Store) recoverPostCompletionRepairTornIntent(state *postCompletionRepairProgressState, plan PostCompletionRepairPlan, requestedPrevious, requestedNext PostCompletionRepairAuthorization) error {
	if state == nil || len(state.TornTail) == 0 {
		return nil
	}
	current := state.Current.Authorization
	authorization := requestedNext
	if !postCompletionRepairAuthorizationEqual(current, requestedPrevious) {
		authorization = requestedPrevious
	}
	candidates := []PostCompletionRepairProgressTransition{}
	if transition, matched, err := postCompletionRepairTornAuthorizationCandidate(*state, plan, authorization); err != nil {
		return err
	} else if matched {
		if err := s.verifyPostCompletionRepairProgressPhysical(plan, state.Current); err != nil {
			return fmt.Errorf("post-completion repair torn reauthorization lacks durable physical evidence: %w", err)
		}
		candidates = append(candidates, transition)
	}
	if transition, err := nextDeterministicPostCompletionRepairPhaseTransition(*state, plan); err == nil {
		matched, matchErr := postCompletionRepairTornTransitionMatches(state.TornTail, transition)
		if matchErr != nil {
			return matchErr
		}
		if matched {
			nextState := state.Current
			nextState.Entries = append([]PostCompletionRepairProgressEntry(nil), state.Current.Entries...)
			if err := applyPostCompletionRepairTransition(&nextState, plan, transition); err != nil {
				return err
			}
			if err := s.verifyPostCompletionRepairProgressPhysical(plan, nextState); err != nil {
				return fmt.Errorf("post-completion repair torn phase lacks durable physical evidence: %w", err)
			}
			candidates = append(candidates, transition)
		}
	}
	if len(candidates) > 1 {
		return fmt.Errorf("post-completion repair torn journal suffix is ambiguous across %d recoverable transitions", len(candidates))
	}
	if len(candidates) == 0 {
		if !postCompletionRepairAuthorizationEqual(current, requestedPrevious) {
			return fmt.Errorf("post-completion repair torn journal suffix does not bind the requested authorization predecessor")
		}
		return fmt.Errorf("post-completion repair torn journal suffix is not the exact recoverable phase or authorization transition prefix")
	}
	return s.appendPostCompletionRepairProgressTransition(state, plan, candidates[0])
}

func postCompletionRepairTornAuthorizationCandidate(state postCompletionRepairProgressState, plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization) (PostCompletionRepairProgressTransition, bool, error) {
	if err := validatePostCompletionRepairReauthorization(state.Current.Authorization, authorization, plan); err != nil {
		return PostCompletionRepairProgressTransition{}, false, nil
	}
	previousDigest, err := digestRepairValue(state.Current.Authorization)
	if err != nil {
		return PostCompletionRepairProgressTransition{}, false, err
	}
	transition := PostCompletionRepairProgressTransition{
		SchemaVersion: postCompletionRepairTransitionSchema, Kind: "reauthorize", Sequence: state.LastSequence + 1,
		PlanID: plan.ID, PendingDigest: state.Snapshot.PendingDigest, EntryIndex: -1,
		PreviousDigest: state.LastDigest, PreviousAuthorizationDigest: previousDigest,
		Authorization: &authorization,
	}
	matched, err := postCompletionRepairTornTransitionMatches(state.TornTail, transition)
	return transition, matched, err
}

func nextDeterministicPostCompletionRepairPhaseTransition(state postCompletionRepairProgressState, plan PostCompletionRepairPlan) (PostCompletionRepairProgressTransition, error) {
	for index, progress := range state.Current.Entries {
		if progress.Phase == repairPhaseRemoved {
			continue
		}
		entry := plan.Entries[index]
		sequence := postCompletionRepairPhaseSequence(entry)
		for phaseIndex := 0; phaseIndex+1 < len(sequence); phaseIndex++ {
			if sequence[phaseIndex] != progress.Phase {
				continue
			}
			if entry.Disposition == RepairImportAbsent && progress.Phase == repairPhasePlanned {
				return PostCompletionRepairProgressTransition{}, fmt.Errorf("post-completion repair cannot independently reconstruct a torn append reservation before its segment coordinate is durable")
			}
			return PostCompletionRepairProgressTransition{
				SchemaVersion: postCompletionRepairTransitionSchema, Sequence: state.LastSequence + 1,
				PlanID: plan.ID, PendingDigest: state.Snapshot.PendingDigest, EntryIndex: index, EntryID: entry.ID,
				FromPhase: progress.Phase, ToPhase: sequence[phaseIndex+1], Segment: progress.Segment,
				SegmentOffset: progress.SegmentOffset, CanonicalSHA256: progress.CanonicalSHA256,
				PreviousDigest: state.LastDigest,
			}, nil
		}
		return PostCompletionRepairProgressTransition{}, fmt.Errorf("post-completion repair current phase %s has no deterministic successor", progress.Phase)
	}
	return PostCompletionRepairProgressTransition{}, fmt.Errorf("post-completion repair torn journal has no incomplete phase to recover")
}

func postCompletionRepairTornTransitionMatches(torn []byte, transition PostCompletionRepairProgressTransition) (bool, error) {
	transition.TransitionDigest, _ = postCompletionRepairTransitionDigest(transition)
	line, err := json.Marshal(transition)
	if err != nil {
		return false, err
	}
	line = append(line, '\n')
	return validatePostCompletionRepairTornTransitionPrefix(torn, line) == nil, nil
}

func (s *Store) ReadPostCompletionRepairResult(planID string) (PostCompletionRepairResult, bool, error) {
	var result PostCompletionRepairResult
	if filepath.Base(planID) != planID || !strings.HasPrefix(planID, "esrp_") {
		return result, false, fmt.Errorf("post-completion repair plan id invalid")
	}
	result, err := loadStrictRootedJSON[PostCompletionRepairResult](s.home, s.postCompletionRepairResultPath(planID), "post-completion repair result")
	if errors.Is(err, os.ErrNotExist) {
		return PostCompletionRepairResult{}, false, nil
	}
	return result, err == nil, err
}

func (s *Store) ReadPostCompletionRepairProof(planID string) (PostCompletionRepairProof, bool, error) {
	var proof PostCompletionRepairProof
	if filepath.Base(planID) != planID || !strings.HasPrefix(planID, "esrp_") {
		return proof, false, fmt.Errorf("post-completion repair plan id invalid")
	}
	proof, err := loadStrictRootedJSON[PostCompletionRepairProof](s.home, s.postCompletionRepairProofPath(planID), "post-completion repair proof")
	if errors.Is(err, os.ErrNotExist) {
		return PostCompletionRepairProof{}, false, nil
	}
	return proof, err == nil, err
}

func (s *Store) ReadPostCompletionRepairSupersession(planID string) (PostCompletionRepairSupersession, bool, error) {
	var supersession PostCompletionRepairSupersession
	if filepath.Base(planID) != planID || !strings.HasPrefix(planID, "esrp_") {
		return supersession, false, fmt.Errorf("post-completion repair plan id invalid")
	}
	supersession, err := loadStrictRootedJSON[PostCompletionRepairSupersession](s.home, s.postCompletionRepairSupersessionPath(planID), "post-completion repair supersession")
	if errors.Is(err, os.ErrNotExist) {
		return PostCompletionRepairSupersession{}, false, nil
	}
	if err != nil {
		return PostCompletionRepairSupersession{}, false, err
	}
	oldPlan, err := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(planID))
	if err != nil {
		return PostCompletionRepairSupersession{}, false, err
	}
	review, err := s.loadPostCompletionRepairReview(s.postCompletionRepairReviewPath(planID), oldPlan)
	if err != nil {
		return PostCompletionRepairSupersession{}, false, err
	}
	successor, err := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(supersession.SuccessorPlanID))
	if err != nil {
		return PostCompletionRepairSupersession{}, false, err
	}
	if err := validatePostCompletionRepairSupersession(supersession, oldPlan, review, successor); err != nil {
		return PostCompletionRepairSupersession{}, false, err
	}
	return supersession, true, nil
}

func (s *Store) ReadPostCompletionRepairSupersessionProof(planID string) (PostCompletionRepairSupersessionProof, bool, error) {
	var proof PostCompletionRepairSupersessionProof
	if filepath.Base(planID) != planID || !strings.HasPrefix(planID, "esrp_") {
		return proof, false, fmt.Errorf("post-completion repair plan id invalid")
	}
	proof, err := loadStrictRootedJSON[PostCompletionRepairSupersessionProof](s.home, s.postCompletionRepairProofPath(planID), "post-completion repair supersession proof")
	if errors.Is(err, os.ErrNotExist) {
		return PostCompletionRepairSupersessionProof{}, false, nil
	}
	return proof, err == nil, err
}

// FinalizePostCompletionRepairSupersession promotes a physical supersession to
// terminal completion only after its exact same-work Receipt and Transition
// exist. Replays preserve RecordedAt and the complete proof identity.
func (s *Store) FinalizePostCompletionRepairSupersession(planID string, authorization PostCompletionRepairAuthorization, receiptID, transitionID string, now time.Time) (PostCompletionRepairSupersessionProof, error) {
	m := &migrator{store: s, engine: s.engine}
	runLock, err := m.acquireRunLockBlocking()
	if err != nil {
		return PostCompletionRepairSupersessionProof{}, err
	}
	defer runLock.release()
	supersession, exists, err := s.ReadPostCompletionRepairSupersession(planID)
	if err != nil || !exists {
		return PostCompletionRepairSupersessionProof{}, errors.Join(fmt.Errorf("post-completion repair supersession %s required before proof finalization", planID), err)
	}
	if !postCompletionRepairAuthorizationEqual(supersession.Authorization, authorization) {
		return PostCompletionRepairSupersessionProof{}, fmt.Errorf("post-completion repair supersession authorization changed before work proof finalization")
	}
	root, _, rootExists, err := s.loadPostCompletionRepairRoot()
	if err != nil || !rootExists || !postCompletionRepairRootOwns(root, "supersession", planID) {
		return PostCompletionRepairSupersessionProof{}, errors.Join(fmt.Errorf("post-completion repair rooted supersession required before work proof finalization"), err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	proof := PostCompletionRepairSupersessionProof{
		SchemaVersion: postCompletionRepairSupersessionProofSchema,
		PlanID:        planID, PlanDigest: supersession.PlanDigest,
		SupersessionID: supersession.ID, SupersessionDigest: supersession.SupersessionDigest,
		Authorization: authorization, ReceiptID: receiptID, TransitionID: transitionID,
		RecordedAt: now.UTC().Format(time.RFC3339Nano),
	}
	proof.ProofDigest, err = postCompletionRepairSupersessionProofDigest(proof)
	if err != nil {
		return PostCompletionRepairSupersessionProof{}, err
	}
	proofPath := s.postCompletionRepairProofPath(planID)
	if existing, present, err := s.ReadPostCompletionRepairSupersessionProof(planID); err != nil {
		return PostCompletionRepairSupersessionProof{}, err
	} else if present {
		candidate := proof
		candidate.RecordedAt = existing.RecordedAt
		candidate.ProofDigest, _ = postCompletionRepairSupersessionProofDigest(candidate)
		if existing.ProofDigest != candidate.ProofDigest || !postCompletionRepairAuthorizationEqual(existing.Authorization, candidate.Authorization) {
			return PostCompletionRepairSupersessionProof{}, fmt.Errorf("post-completion repair supersession proof already exists with different content")
		}
		proof = existing
		if err := s.validatePostCompletionRepairSupersessionProof(proof, supersession); err != nil {
			return PostCompletionRepairSupersessionProof{}, err
		}
	} else {
		if err := s.validatePostCompletionRepairSupersessionProof(proof, supersession); err != nil {
			return PostCompletionRepairSupersessionProof{}, err
		}
		if err := s.writePostCompletionRepairExclusiveOrEqualJSON(proofPath, "post-completion repair supersession proof", proof); err != nil {
			return PostCompletionRepairSupersessionProof{}, err
		}
	}
	if err := s.ensurePostCompletionRepairArtifact("proof", proof.ProofDigest, proofPath); err != nil {
		return PostCompletionRepairSupersessionProof{}, err
	}
	return proof, nil
}

// FinalizePostCompletionRepair closes the physical pending journal only after
// all four execution-spine refs resolve and bind the same work/action chain.
func (s *Store) FinalizePostCompletionRepair(planID string, authorization PostCompletionRepairAuthorization, receiptID, transitionID string, now time.Time) (PostCompletionRepairProof, error) {
	m := &migrator{store: s, engine: s.engine}
	runLock, err := m.acquireRunLockBlocking()
	if err != nil {
		return PostCompletionRepairProof{}, err
	}
	defer runLock.release()
	result, exists, err := s.ReadPostCompletionRepairResult(planID)
	if err != nil || !exists {
		return PostCompletionRepairProof{}, errors.Join(fmt.Errorf("post-completion repair result %s required before proof finalization", planID), err)
	}
	plan, err := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(planID))
	if err != nil {
		return PostCompletionRepairProof{}, err
	}
	if err := s.validatePostCompletionRepairResult(result, plan, authorization); err != nil {
		return PostCompletionRepairProof{}, err
	}
	if err := s.verifyPostCompletionRepairResultState(result, true); err != nil {
		return PostCompletionRepairProof{}, fmt.Errorf("post-completion repair result state drifted before work proof finalization: %w", err)
	}
	if err := s.validatePostCompletionRepairEnvelopeChain(authorization, receiptID, transitionID); err != nil {
		return PostCompletionRepairProof{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	proof := PostCompletionRepairProof{
		SchemaVersion: postCompletionRepairProofSchema, PlanID: plan.ID, PlanDigest: plan.PlanDigest,
		ResultID: result.ID, ResultDigest: result.ResultDigest,
		Authorization: authorization, ReceiptID: receiptID, TransitionID: transitionID,
		RecordedAt: now.UTC().Format(time.RFC3339Nano),
	}
	proof.ProofDigest, err = postCompletionRepairProofDigest(proof)
	if err != nil {
		return PostCompletionRepairProof{}, err
	}
	proofPath := s.postCompletionRepairProofPath(plan.ID)
	if existing, present, err := s.ReadPostCompletionRepairProof(plan.ID); err != nil {
		return PostCompletionRepairProof{}, err
	} else if present {
		candidate := proof
		candidate.RecordedAt = existing.RecordedAt
		candidate.ProofDigest, _ = postCompletionRepairProofDigest(candidate)
		if existing.ProofDigest != candidate.ProofDigest || !postCompletionRepairAuthorizationEqual(existing.Authorization, candidate.Authorization) {
			return PostCompletionRepairProof{}, fmt.Errorf("post-completion repair proof already exists with different content")
		}
		proof = existing
		if err := s.validatePostCompletionRepairProof(proof, result); err != nil {
			return PostCompletionRepairProof{}, err
		}
	} else {
		if err := s.validatePostCompletionRepairProof(proof, result); err != nil {
			return PostCompletionRepairProof{}, err
		}
		if err := s.writePostCompletionRepairExclusiveOrEqualJSON(proofPath, "post-completion repair proof", proof); err != nil {
			return PostCompletionRepairProof{}, err
		}
	}
	if err := s.ensurePostCompletionRepairArtifact("proof", proof.ProofDigest, proofPath); err != nil {
		return PostCompletionRepairProof{}, err
	}
	if err := s.finalizePostCompletionRepairProgress(plan, authorization); err != nil {
		return PostCompletionRepairProof{}, fmt.Errorf("repair work proof is durable but progress finalization failed: %w", err)
	}
	return proof, nil
}

func (s *Store) finalizePostCompletionRepairProgress(plan PostCompletionRepairPlan, auth PostCompletionRepairAuthorization) error {
	pendingPath := s.postCompletionRepairPendingPath(plan.ID)
	progressPath := s.postCompletionRepairProgressPath(plan.ID)
	pendingBody, pendingErr := homepkg.ReadRootedRegularFile(s.home, pendingPath, "post-completion repair pending snapshot")
	progressBody, progressErr := homepkg.ReadRootedRegularFile(s.home, progressPath, "post-completion repair progress journal")
	pendingExists := pendingErr == nil
	progressExists := progressErr == nil
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return pendingErr
	}
	if progressErr != nil && !errors.Is(progressErr, os.ErrNotExist) {
		return progressErr
	}
	root, _, rootExists, err := s.loadPostCompletionRepairRoot()
	if err != nil || !rootExists {
		return errors.Join(fmt.Errorf("post-completion repair root required for progress finalization"), err)
	}
	rootedPending := postCompletionRepairRootOwns(root, "pending", plan.ID)
	rootedProgress := postCompletionRepairRootOwns(root, "progress", plan.ID)
	if pendingExists && progressExists {
		if !rootedPending || !rootedProgress {
			return fmt.Errorf("post-completion repair progress files are not both rooted")
		}
		state, err := s.loadPostCompletionRepairProgress(plan, auth)
		if err != nil {
			return err
		}
		if !bytes.Equal(progressBody, state.JournalBytes) {
			return fmt.Errorf("post-completion repair progress journal changed before finalization")
		}
		if err := s.removePostCompletionRepairExactBytes(progressPath, state.JournalBytes, "post-completion repair progress journal"); err != nil {
			return err
		}
		if err := s.removePostCompletionRepairExactJSON(pendingPath, state.Snapshot, "post-completion repair pending snapshot"); err != nil {
			return err
		}
	} else if pendingExists && !progressExists {
		// Cleanup always removes progress first. A valid proof plus both rooted
		// identities makes this the one resumable cleanup crash shape.
		if !rootedPending || !rootedProgress {
			return fmt.Errorf("post-completion repair progress history disappeared without rooted cleanup evidence")
		}
		snapshot, err := loadStrictRootedJSON[PostCompletionRepairPending](s.home, pendingPath, "post-completion repair pending snapshot")
		if err != nil || validatePostCompletionRepairSnapshot(snapshot, plan, snapshot.Authorization) != nil {
			return errors.Join(fmt.Errorf("post-completion repair pending snapshot invalid during cleanup recovery"), err)
		}
		if !bytes.Equal(pendingBody, mustRepairJSON(snapshot)) {
			return fmt.Errorf("post-completion repair pending snapshot changed during cleanup recovery")
		}
		if err := s.removePostCompletionRepairExactJSON(pendingPath, snapshot, "post-completion repair pending snapshot"); err != nil {
			return err
		}
	} else if !pendingExists && progressExists {
		return fmt.Errorf("post-completion repair pending snapshot disappeared before progress journal cleanup")
	}
	return s.removePostCompletionRepairProgressArtifacts(plan.ID)
}

func (s *Store) removePostCompletionRepairProgressArtifacts(planID string) error {
	return s.mutatePostCompletionRepairRoot(func(root *PostCompletionRepairRoot) error {
		kept := root.Artifacts[:0]
		for _, artifact := range root.Artifacts {
			if artifact.ID == planID && (artifact.Kind == "pending" || artifact.Kind == "progress") {
				continue
			}
			kept = append(kept, artifact)
		}
		root.Artifacts = kept
		return nil
	})
}

func newPostCompletionRepairPending(plan PostCompletionRepairPlan, authorization PostCompletionRepairAuthorization, now time.Time) PostCompletionRepairPending {
	entries := make([]PostCompletionRepairProgressEntry, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		entries = append(entries, PostCompletionRepairProgressEntry{ID: entry.ID, Phase: repairPhasePlanned})
	}
	pending := PostCompletionRepairPending{
		SchemaVersion: postCompletionRepairPendingSchema, PlanID: plan.ID, PlanDigest: plan.PlanDigest,
		Authorization: authorization,
		StartedAt:     now.UTC().Format(time.RFC3339Nano), Entries: entries,
	}
	pending.PendingDigest, _ = postCompletionRepairPendingDigest(pending)
	return pending
}

func (s *Store) loadPostCompletionRepairPlan(path string) (PostCompletionRepairPlan, error) {
	plan, err := loadStrictRootedJSON[PostCompletionRepairPlan](s.home, path, "post-completion repair plan")
	if err != nil {
		return plan, err
	}
	if err := validatePostCompletionRepairPlan(plan); err != nil {
		return plan, err
	}
	expected, err := filepath.Abs(s.postCompletionRepairPlanPath(plan.ID))
	if err != nil {
		return plan, err
	}
	actual, err := filepath.Abs(filepath.Clean(path))
	if err != nil || actual != expected {
		return plan, fmt.Errorf("post-completion repair plan path must be %s", expected)
	}
	return plan, nil
}

func validatePostCompletionRepairPlan(plan PostCompletionRepairPlan) error {
	if plan.SchemaVersion != postCompletionRepairPlanSchema || !strings.HasPrefix(plan.ID, "esrp_") || plan.Status != "ready" {
		return fmt.Errorf("post-completion repair plan identity/status invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, plan.CreatedAt); err != nil {
		return fmt.Errorf("post-completion repair plan created_at invalid: %w", err)
	}
	if !validRepairDigest(plan.SourceDigest) || plan.Revision != plan.SourceDigest || !validRepairDigest(plan.PlanDigest) || plan.Total != len(plan.Entries) || !repairCountsEqual(plan.Counts, plan.Entries) {
		return fmt.Errorf("post-completion repair plan count/digest shape invalid")
	}
	if !plan.WriterFence.Stable || !validRepairDigest(plan.WriterFence.BeforeDigest) || plan.WriterFence.BeforeDigest != plan.WriterFence.AfterDigest {
		return fmt.Errorf("post-completion repair plan lacks stable writer-quiescence evidence")
	}
	if _, err := time.Parse(time.RFC3339Nano, plan.WriterFence.StartedAt); err != nil {
		return fmt.Errorf("post-completion repair writer fence started_at invalid: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, plan.WriterFence.CompletedAt); err != nil {
		return fmt.Errorf("post-completion repair writer fence completed_at invalid: %w", err)
	}
	for _, digest := range []string{plan.Baseline.MigrationPlanSHA256, plan.Baseline.ManifestSHA256, plan.Baseline.CompletionSHA256} {
		if !validRepairDigest(digest) {
			return fmt.Errorf("post-completion repair plan baseline digest invalid")
		}
	}
	if expected, err := postCompletionRepairPlanDigest(plan); err != nil || expected != plan.PlanDigest {
		return errors.Join(fmt.Errorf("post-completion repair plan digest mismatch"), err)
	}
	baseID := postCompletionRepairPlanID(plan.SourceDigest, 0)
	validID := plan.ID == baseID
	if !validID && strings.HasPrefix(plan.ID, baseID+"_o") {
		occurrenceText := strings.TrimPrefix(plan.ID, baseID+"_o")
		occurrence, err := strconv.ParseUint(occurrenceText, 10, 64)
		validID = err == nil && occurrence > 0 && plan.ID == postCompletionRepairPlanID(plan.SourceDigest, occurrence)
	}
	if !validID {
		return fmt.Errorf("post-completion repair plan id does not match source digest")
	}
	seen := map[string]bool{}
	for _, entry := range plan.Entries {
		if entry.ID == "" || filepath.Base(entry.FileName) != entry.FileName || entry.FileName != entry.ID+".json" || seen[entry.ID] || len(entry.RawSHA256) != 64 || len(entry.CanonicalSHA256) != 64 || entry.Family == "" {
			return fmt.Errorf("post-completion repair plan entry %q invalid", entry.ID)
		}
		seen[entry.ID] = true
		if entry.Disposition == RepairImportAbsent {
			if entry.SegmentSHA256 != "" || entry.Segment != "" {
				return fmt.Errorf("post-completion repair absent entry %s unexpectedly binds segment", entry.ID)
			}
		} else if (entry.Disposition != RepairArchiveIdentical && entry.Disposition != RepairArchiveDivergent) || len(entry.SegmentSHA256) != 64 || entry.Segment == "" {
			return fmt.Errorf("post-completion repair duplicate entry %s lacks segment authority", entry.ID)
		}
	}
	return nil
}

func validatePostCompletionRepairReview(review PostCompletionRepairReview, plan PostCompletionRepairPlan) error {
	if review.SchemaVersion != postCompletionRepairReviewSchema || !strings.HasPrefix(review.ID, "esrr_") || review.PlanID != plan.ID || review.PlanDigest != plan.PlanDigest || review.PlanRevision != plan.Revision {
		return fmt.Errorf("post-completion repair review does not bind exact plan digest/revision")
	}
	if !validPostCompletionRepairReviewer(review.Reviewer) || review.Decision != "authorize" || !review.WriterStoppedAttested || review.WriterFenceDigest != plan.WriterFence.AfterDigest {
		return fmt.Errorf("post-completion repair review is not an authorized operator writer-stop decision")
	}
	if _, err := time.Parse(time.RFC3339Nano, review.CreatedAt); err != nil {
		return fmt.Errorf("post-completion repair review created_at invalid: %w", err)
	}
	if expected, err := postCompletionRepairReviewDigest(review); err != nil || expected != review.ReviewDigest {
		return errors.Join(fmt.Errorf("post-completion repair review digest mismatch"), err)
	}
	if review.ID != "esrr_"+strings.TrimPrefix(plan.PlanDigest, "sha256:")[:12] {
		return fmt.Errorf("post-completion repair review id does not match plan")
	}
	return nil
}

func validPostCompletionRepairReviewer(reviewer string) bool {
	if reviewer != strings.TrimSpace(reviewer) || !strings.HasPrefix(reviewer, "operator:") {
		return false
	}
	id := strings.TrimPrefix(reviewer, "operator:")
	return id != "" && strings.IndexFunc(id, unicode.IsSpace) < 0
}

func (s *Store) loadPostCompletionRepairReview(path string, plan PostCompletionRepairPlan) (PostCompletionRepairReview, error) {
	review, err := loadStrictRootedJSON[PostCompletionRepairReview](s.home, path, "post-completion repair review")
	if err != nil {
		return review, err
	}
	if err := validatePostCompletionRepairReview(review, plan); err != nil {
		return review, err
	}
	expected, err := filepath.Abs(s.postCompletionRepairReviewPath(plan.ID))
	if err != nil {
		return review, err
	}
	actual, err := filepath.Abs(filepath.Clean(path))
	if err != nil || actual != expected {
		return review, fmt.Errorf("post-completion repair review path must be %s", expected)
	}
	return review, nil
}

// PostCompletionRepairReviewForApply validates the exact immutable review and
// rechecks the bounded writer fence before the command reserves work.
func (s *Store) PostCompletionRepairReviewForApply(reviewPath string, plan PostCompletionRepairPlan, allowProgress bool) (PostCompletionRepairReview, error) {
	review, err := s.loadPostCompletionRepairReview(reviewPath, plan)
	if err != nil {
		return review, err
	}
	current, err := s.postCompletionRepairWriterSnapshot(&plan, allowProgress)
	if err != nil {
		return review, err
	}
	if current != review.WriterFenceDigest {
		return review, fmt.Errorf("post-completion repair writer fence drifted after operator review")
	}
	return review, nil
}

// PostCompletionRepairReviewForFinalization validates the sealed review bytes
// without demanding the pre-mutation writer snapshot. This is only for a
// physically completed result whose reserved authorization is being finalized.
func (s *Store) PostCompletionRepairReviewForFinalization(reviewPath string, plan PostCompletionRepairPlan) (PostCompletionRepairReview, error) {
	return s.loadPostCompletionRepairReview(reviewPath, plan)
}

func validatePostCompletionRepairAuthorization(auth PostCompletionRepairAuthorization, plan PostCompletionRepairPlan) error {
	// AuthorityRevision is an opaque policy-owned identity. The adapter validates
	// the complete AuthorityFace before crossing this physical boundary; the
	// event store only binds that exact revision into its journal and proof.
	if !strings.HasPrefix(auth.WorkID, "work_") || strings.TrimSpace(auth.Actor) == "" || !strings.HasPrefix(auth.ActionIntentID, "act_") || !strings.HasPrefix(auth.PolicyDecisionID, "pol_") || strings.TrimSpace(auth.AuthorityRevision) == "" || auth.PlanDigest != plan.PlanDigest || !strings.HasPrefix(auth.ReviewID, "esrr_") || !validRepairDigest(auth.ReviewDigest) || auth.WriterFenceDigest != plan.WriterFence.AfterDigest {
		return fmt.Errorf("post-completion repair authorization does not bind work/action/current policy authority and exact plan")
	}
	if len(auth.SealedAction) > 0 || len(auth.SealedDecision) > 0 || auth.AuthorizedAt != "" {
		if len(auth.SealedAction) == 0 || len(auth.SealedDecision) == 0 {
			return fmt.Errorf("post-completion repair sealed authorization tuple is incomplete")
		}
		if _, err := time.Parse(time.RFC3339Nano, auth.AuthorizedAt); err != nil {
			return fmt.Errorf("post-completion repair sealed authorization time invalid: %w", err)
		}
		var actionLink, decisionLink postCompletionRepairEnvelopeLink
		if err := json.Unmarshal(auth.SealedAction, &actionLink); err != nil || actionLink.ID != auth.ActionIntentID || actionLink.IDs.WorkID != auth.WorkID {
			return fmt.Errorf("post-completion repair sealed action does not bind authorization: %w", err)
		}
		if err := json.Unmarshal(auth.SealedDecision, &decisionLink); err != nil || decisionLink.ID != auth.PolicyDecisionID || decisionLink.IDs.WorkID != auth.WorkID || decisionLink.ActionIntentID != auth.ActionIntentID {
			return fmt.Errorf("post-completion repair sealed policy decision does not bind authorization: %w", err)
		}
	}
	return nil
}

func validatePostCompletionRepairPending(pending PostCompletionRepairPending, plan PostCompletionRepairPlan, auth PostCompletionRepairAuthorization) error {
	if pending.SchemaVersion != postCompletionRepairPendingSchema || pending.PlanID != plan.ID || pending.PlanDigest != plan.PlanDigest || !postCompletionRepairAuthorizationEqual(pending.Authorization, auth) {
		return fmt.Errorf("post-completion repair pending journal conflicts with policy authorization")
	}
	if _, err := time.Parse(time.RFC3339Nano, pending.StartedAt); err != nil {
		return err
	}
	if expected, err := postCompletionRepairPendingDigest(pending); err != nil || expected != pending.PendingDigest {
		return errors.Join(fmt.Errorf("post-completion repair pending digest mismatch"), err)
	}
	if len(pending.Entries) != len(plan.Entries) {
		return fmt.Errorf("post-completion repair progress entry count mismatch")
	}
	for index, progress := range pending.Entries {
		if progress.ID != plan.Entries[index].ID || repairPhaseRank(progress.Phase) < 0 {
			return fmt.Errorf("post-completion repair progress entry %d invalid", index)
		}
		if repairPhaseRank(progress.Phase) >= repairPhaseRank(repairPhaseAppendReserved) && plan.Entries[index].Disposition == RepairImportAbsent {
			if progress.Segment == "" || progress.CanonicalSHA256 != plan.Entries[index].CanonicalSHA256 {
				return fmt.Errorf("post-completion repair progress %s lacks reserved frame identity", progress.ID)
			}
		}
	}
	return nil
}

func repairPhaseRank(phase string) int {
	for index, candidate := range []string{repairPhasePlanned, repairPhaseAppendReserved, repairPhaseAppended, repairPhaseFrameVerified, repairPhaseRemoveReserved, repairPhaseClaimed, repairPhaseRemoved} {
		if phase == candidate {
			return index
		}
	}
	return -1
}

func (s *Store) createPostCompletionRepairProgress(plan PostCompletionRepairPlan, auth PostCompletionRepairAuthorization, now time.Time) (postCompletionRepairProgressState, error) {
	snapshot := newPostCompletionRepairPending(plan, auth, now)
	if err := validatePostCompletionRepairSnapshot(snapshot, plan, auth); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	if err := s.writePostCompletionRepairExclusiveOrEqualJSON(s.postCompletionRepairPendingPath(plan.ID), "post-completion repair pending snapshot", snapshot); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	header, body, err := newPostCompletionRepairProgressHeader(snapshot)
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	created, err := writeBytesExclusiveAtomic(s.postCompletionRepairProgressPath(plan.ID), body)
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	if !created {
		existing, readErr := homepkg.ReadRootedRegularFile(s.home, s.postCompletionRepairProgressPath(plan.ID), "post-completion repair progress journal")
		if readErr != nil || !bytes.Equal(existing, body) {
			return postCompletionRepairProgressState{}, errors.Join(fmt.Errorf("post-completion repair progress journal already exists with different history"), readErr)
		}
	}
	initialHead, err := newPostCompletionRepairAuthorizationHead(auth, 1, 0, header.HeaderDigest)
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	state := postCompletionRepairProgressState{
		Snapshot: snapshot, Current: snapshot, Header: header, LastDigest: header.HeaderDigest, JournalBytes: body,
		AuthorizationHistory: []PostCompletionRepairAuthorization{auth}, AuthorizationHeads: []postCompletionRepairAuthorizationHead{initialHead},
	}
	if err := s.ensurePostCompletionRepairArtifact("pending", plan.ID, s.postCompletionRepairPendingPath(plan.ID)); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	if err := s.ensurePostCompletionRepairProgressArtifact(plan.ID, state); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	return state, nil
}

func (s *Store) loadOrUpgradePostCompletionRepairProgress(plan PostCompletionRepairPlan, auth PostCompletionRepairAuthorization) (postCompletionRepairProgressState, error) {
	state, err := s.loadPostCompletionRepairProgress(plan, auth)
	if err == nil {
		if err := s.ensurePostCompletionRepairArtifact("pending", plan.ID, s.postCompletionRepairPendingPath(plan.ID)); err != nil {
			return postCompletionRepairProgressState{}, err
		}
		if err := s.ensurePostCompletionRepairProgressArtifact(plan.ID, state); err != nil {
			return postCompletionRepairProgressState{}, err
		}
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return postCompletionRepairProgressState{}, err
	}
	legacy, loadErr := loadStrictRootedJSON[PostCompletionRepairPending](s.home, s.postCompletionRepairPendingPath(plan.ID), "post-completion repair pending snapshot")
	if loadErr != nil {
		return postCompletionRepairProgressState{}, loadErr
	}
	if err := validatePostCompletionRepairSnapshot(legacy, plan, auth); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	root, _, _, err := s.loadPostCompletionRepairRoot()
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	if postCompletionRepairRootOwns(root, "pending", plan.ID) || postCompletionRepairRootOwns(root, "progress", plan.ID) {
		return postCompletionRepairProgressState{}, fmt.Errorf("post-completion repair indexed progress journal is missing: %w", os.ErrNotExist)
	}
	startedAt, err := time.Parse(time.RFC3339Nano, legacy.StartedAt)
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	snapshot := newPostCompletionRepairPending(plan, auth, startedAt)
	body, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	if err := writeBytesAtomic(s.postCompletionRepairPendingPath(plan.ID), append(body, '\n')); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	return s.createPostCompletionRepairProgress(plan, auth, startedAt)
}

func (s *Store) loadPostCompletionRepairProgress(plan PostCompletionRepairPlan, auth PostCompletionRepairAuthorization) (postCompletionRepairProgressState, error) {
	return s.loadPostCompletionRepairProgressExpected(plan, &auth)
}

func (s *Store) loadPostCompletionRepairProgressCurrent(plan PostCompletionRepairPlan) (postCompletionRepairProgressState, error) {
	return s.loadPostCompletionRepairProgressExpected(plan, nil)
}

func (s *Store) loadPostCompletionRepairProgressExpected(plan PostCompletionRepairPlan, expectedAuth *PostCompletionRepairAuthorization) (postCompletionRepairProgressState, error) {
	snapshot, err := loadStrictRootedJSON[PostCompletionRepairPending](s.home, s.postCompletionRepairPendingPath(plan.ID), "post-completion repair pending snapshot")
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	path := s.postCompletionRepairProgressPath(plan.ID)
	if err := validatePostCompletionRepairSnapshot(snapshot, plan, snapshot.Authorization); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	maxBytes := int64((1 + postCompletionRepairMaxJournalTransitions(plan)) * maxPostCompletionRepairJournalLine)
	body, err := homepkg.ReadRootedRegularFile(s.home, path, "post-completion repair progress journal")
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	if len(body) <= 0 || int64(len(body)) > maxBytes {
		return postCompletionRepairProgressState{}, fmt.Errorf("post-completion repair progress journal size %d exceeds bounded history", len(body))
	}
	if len(body) == 0 {
		return postCompletionRepairProgressState{}, fmt.Errorf("post-completion repair progress journal is empty")
	}
	completeBody := body
	var tornTail []byte
	if body[len(body)-1] != '\n' {
		lastComplete := bytes.LastIndexByte(body, '\n')
		if lastComplete < 0 {
			return postCompletionRepairProgressState{}, fmt.Errorf("post-completion repair progress journal header is torn")
		}
		completeBody = body[:lastComplete+1]
		tornTail = append([]byte(nil), body[lastComplete+1:]...)
		if len(tornTail) == 0 || len(tornTail) >= maxPostCompletionRepairJournalLine {
			return postCompletionRepairProgressState{}, fmt.Errorf("post-completion repair progress journal torn tail exceeds transition bound")
		}
	}
	lines := bytes.Split(completeBody[:len(completeBody)-1], []byte{'\n'})
	if len(lines) == 0 || len(lines)-1 > postCompletionRepairMaxJournalTransitions(plan) {
		return postCompletionRepairProgressState{}, fmt.Errorf("post-completion repair progress journal exceeds transition bound")
	}
	var header PostCompletionRepairProgressHeader
	if err := decodePostCompletionRepairJournalLine(lines[0], &header); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	if err := validatePostCompletionRepairProgressHeader(header, snapshot); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	current := snapshot
	current.Entries = append([]PostCompletionRepairProgressEntry(nil), snapshot.Entries...)
	initialHead, err := newPostCompletionRepairAuthorizationHead(snapshot.Authorization, 1, 0, header.HeaderDigest)
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	state := postCompletionRepairProgressState{
		Snapshot: snapshot, Current: current, Header: header, LastDigest: header.HeaderDigest, JournalBytes: append([]byte(nil), completeBody...), TornTail: tornTail,
		AuthorizationHistory: []PostCompletionRepairAuthorization{snapshot.Authorization}, AuthorizationHeads: []postCompletionRepairAuthorizationHead{initialHead},
	}
	for index, line := range lines[1:] {
		var transition PostCompletionRepairProgressTransition
		if err := decodePostCompletionRepairJournalLine(line, &transition); err != nil {
			return postCompletionRepairProgressState{}, fmt.Errorf("post-completion repair progress record %d invalid: %w", index+1, err)
		}
		if transition.SchemaVersion != postCompletionRepairTransitionSchema || transition.Sequence != uint64(index+1) || transition.PlanID != plan.ID || transition.PendingDigest != snapshot.PendingDigest || transition.PreviousDigest != state.LastDigest {
			return postCompletionRepairProgressState{}, fmt.Errorf("post-completion repair progress record %d identity or chain invalid", index+1)
		}
		if expected, digestErr := postCompletionRepairTransitionDigest(transition); digestErr != nil || expected != transition.TransitionDigest {
			return postCompletionRepairProgressState{}, errors.Join(fmt.Errorf("post-completion repair progress record %d digest mismatch", index+1), digestErr)
		}
		if err := applyPostCompletionRepairTransition(&state.Current, plan, transition); err != nil {
			return postCompletionRepairProgressState{}, err
		}
		if transition.Kind == "reauthorize" && transition.Authorization != nil {
			state.AuthorizationHistory = append(state.AuthorizationHistory, *transition.Authorization)
			head, headErr := newPostCompletionRepairAuthorizationHead(*transition.Authorization, len(state.AuthorizationHistory), transition.Sequence, transition.TransitionDigest)
			if headErr != nil {
				return postCompletionRepairProgressState{}, headErr
			}
			state.AuthorizationHeads = append(state.AuthorizationHeads, head)
		}
		state.LastSequence = transition.Sequence
		state.LastDigest = transition.TransitionDigest
	}
	state.Current.PendingDigest, err = postCompletionRepairPendingDigest(state.Current)
	if err != nil {
		return postCompletionRepairProgressState{}, err
	}
	currentAuth := state.Current.Authorization
	if expectedAuth != nil {
		currentAuth = *expectedAuth
	}
	if err := validatePostCompletionRepairPending(state.Current, plan, currentAuth); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	if err := s.validatePostCompletionRepairProgressAuthorizationHead(plan.ID, state); err != nil {
		return postCompletionRepairProgressState{}, err
	}
	return state, nil
}

func (s *Store) advancePostCompletionRepairProgress(state *postCompletionRepairProgressState, plan PostCompletionRepairPlan, index int, phase, segment string, offset int64, canonicalSHA string) error {
	if state == nil || index < 0 || index >= len(state.Current.Entries) || index >= len(plan.Entries) {
		return fmt.Errorf("post-completion repair progress state required")
	}
	transition := PostCompletionRepairProgressTransition{
		SchemaVersion: postCompletionRepairTransitionSchema, Sequence: state.LastSequence + 1,
		PlanID: plan.ID, PendingDigest: state.Snapshot.PendingDigest, EntryIndex: index,
		FromPhase: state.Current.Entries[index].Phase, ToPhase: phase,
		Segment: segment, SegmentOffset: offset, CanonicalSHA256: canonicalSHA,
		PreviousDigest: state.LastDigest,
	}
	if index >= 0 && index < len(plan.Entries) {
		transition.EntryID = plan.Entries[index].ID
	}
	return s.appendPostCompletionRepairProgressTransition(state, plan, transition)
}

func (s *Store) advancePostCompletionRepairAuthorization(state *postCompletionRepairProgressState, plan PostCompletionRepairPlan, nextAuthorization PostCompletionRepairAuthorization) error {
	previousDigest, err := digestRepairValue(state.Current.Authorization)
	if err != nil {
		return err
	}
	transition := PostCompletionRepairProgressTransition{
		SchemaVersion: postCompletionRepairTransitionSchema, Kind: "reauthorize", Sequence: state.LastSequence + 1,
		PlanID: plan.ID, PendingDigest: state.Snapshot.PendingDigest, EntryIndex: -1,
		PreviousDigest: state.LastDigest, PreviousAuthorizationDigest: previousDigest,
		Authorization: &nextAuthorization,
	}
	return s.appendPostCompletionRepairProgressTransition(state, plan, transition)
}

func (s *Store) appendPostCompletionRepairProgressTransition(state *postCompletionRepairProgressState, plan PostCompletionRepairPlan, transition PostCompletionRepairProgressTransition) error {
	next := state.Current
	next.Entries = append([]PostCompletionRepairProgressEntry(nil), state.Current.Entries...)
	if err := applyPostCompletionRepairTransition(&next, plan, transition); err != nil {
		return err
	}
	transition.TransitionDigest, _ = postCompletionRepairTransitionDigest(transition)
	line, err := json.Marshal(transition)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if len(line) > maxPostCompletionRepairJournalLine || int(state.LastSequence)+1 > postCompletionRepairMaxJournalTransitions(plan) {
		return fmt.Errorf("post-completion repair progress transition exceeds journal bound")
	}
	path := s.postCompletionRepairProgressPath(plan.ID)
	suffix, err := recoverPostCompletionRepairTornTransition(s.home, path, state.JournalBytes, state.TornTail, line)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	writeErr := writePostCompletionRepairProgressSuffix(file, suffix)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	next.PendingDigest, err = postCompletionRepairPendingDigest(next)
	if err != nil {
		return err
	}
	state.Current = next
	state.LastSequence = transition.Sequence
	state.LastDigest = transition.TransitionDigest
	state.JournalBytes = append(state.JournalBytes, line...)
	state.TornTail = nil
	if transition.Kind == "reauthorize" && transition.Authorization != nil {
		state.AuthorizationHistory = append(state.AuthorizationHistory, *transition.Authorization)
		head, headErr := newPostCompletionRepairAuthorizationHead(*transition.Authorization, len(state.AuthorizationHistory), transition.Sequence, transition.TransitionDigest)
		if headErr != nil {
			return headErr
		}
		state.AuthorizationHeads = append(state.AuthorizationHeads, head)
	}
	return nil
}

type postCompletionRepairSyncWriter interface {
	Write([]byte) (int, error)
	Sync() error
}

func writePostCompletionRepairProgressSuffix(file postCompletionRepairSyncWriter, suffix []byte) error {
	for len(suffix) > 0 {
		n, err := file.Write(suffix)
		if n < 0 || n > len(suffix) {
			return errors.Join(fmt.Errorf("post-completion repair progress writer returned invalid count %d", n), file.Sync())
		}
		suffix = suffix[n:]
		if err != nil {
			return errors.Join(err, file.Sync())
		}
		if n == 0 {
			return errors.Join(io.ErrShortWrite, file.Sync())
		}
	}
	return file.Sync()
}

func recoverPostCompletionRepairTornTransition(homePath, path string, complete, torn, expected []byte) ([]byte, error) {
	body, err := homepkg.ReadRootedRegularFile(homePath, path, "post-completion repair progress journal")
	if err != nil {
		return nil, err
	}
	if len(body) != len(complete)+len(torn) || !bytes.Equal(body[:len(complete)], complete) || !bytes.Equal(body[len(complete):], torn) {
		return nil, fmt.Errorf("post-completion repair progress append coordinate drift")
	}
	if len(torn) == 0 {
		return expected, nil
	}
	if err := validatePostCompletionRepairTornTransitionPrefix(torn, expected); err != nil {
		return nil, err
	}
	return expected[len(torn):], nil
}

func validatePostCompletionRepairTornTransitionPrefix(torn, expected []byte) error {
	if len(torn) == 0 || len(torn) >= len(expected) || !bytes.Equal(torn, expected[:len(torn)]) {
		return fmt.Errorf("post-completion repair torn journal suffix is not the exact expected next transition prefix")
	}
	return nil
}

func applyPostCompletionRepairTransition(current *PostCompletionRepairPending, plan PostCompletionRepairPlan, transition PostCompletionRepairProgressTransition) error {
	if current == nil {
		return fmt.Errorf("post-completion repair progress state required")
	}
	if transition.Kind == "reauthorize" {
		if transition.EntryIndex != -1 || transition.EntryID != "" || transition.FromPhase != "" || transition.ToPhase != "" || transition.Segment != "" || transition.SegmentOffset != 0 || transition.CanonicalSHA256 != "" || transition.Authorization == nil {
			return fmt.Errorf("post-completion repair reauthorization transition shape invalid")
		}
		previousDigest, err := digestRepairValue(current.Authorization)
		if err != nil || previousDigest != transition.PreviousAuthorizationDigest {
			return errors.Join(fmt.Errorf("post-completion repair reauthorization predecessor mismatch"), err)
		}
		if err := validatePostCompletionRepairReauthorization(current.Authorization, *transition.Authorization, plan); err != nil {
			return err
		}
		current.Authorization = *transition.Authorization
		return nil
	}
	if transition.Kind != "" && transition.Kind != "phase" {
		return fmt.Errorf("post-completion repair transition kind %q invalid", transition.Kind)
	}
	if current == nil || transition.EntryIndex < 0 || transition.EntryIndex >= len(current.Entries) || transition.EntryIndex >= len(plan.Entries) {
		return fmt.Errorf("post-completion repair progress entry index invalid")
	}
	entry := plan.Entries[transition.EntryIndex]
	progress := &current.Entries[transition.EntryIndex]
	if transition.EntryID != entry.ID || progress.ID != entry.ID || transition.FromPhase != progress.Phase || !postCompletionRepairTransitionAllowed(entry, transition.FromPhase, transition.ToPhase) {
		return fmt.Errorf("post-completion repair progress for %s is nonlinear: %s -> %s", entry.ID, transition.FromPhase, transition.ToPhase)
	}
	if entry.Disposition == RepairImportAbsent {
		if repairPhaseRank(transition.ToPhase) >= repairPhaseRank(repairPhaseAppendReserved) {
			if transition.Segment == "" || transition.SegmentOffset < 0 || transition.CanonicalSHA256 != entry.CanonicalSHA256 {
				return fmt.Errorf("post-completion repair progress %s lacks reserved frame identity", entry.ID)
			}
			if progress.Segment != "" && (progress.Segment != transition.Segment || progress.SegmentOffset != transition.SegmentOffset || progress.CanonicalSHA256 != transition.CanonicalSHA256) {
				return fmt.Errorf("post-completion repair progress %s changed reserved frame identity", entry.ID)
			}
			progress.Segment, progress.SegmentOffset, progress.CanonicalSHA256 = transition.Segment, transition.SegmentOffset, transition.CanonicalSHA256
		}
	} else if transition.Segment != "" || transition.SegmentOffset != 0 || transition.CanonicalSHA256 != "" {
		return fmt.Errorf("post-completion repair duplicate progress %s unexpectedly binds a new frame", entry.ID)
	}
	progress.Phase = transition.ToPhase
	return nil
}

func postCompletionRepairTransitionAllowed(entry PostCompletionRepairEntry, from, to string) bool {
	sequence := postCompletionRepairPhaseSequence(entry)
	for index := 0; index+1 < len(sequence); index++ {
		if sequence[index] == from && sequence[index+1] == to {
			return true
		}
	}
	return false
}

func postCompletionRepairPhaseSequence(entry PostCompletionRepairEntry) []string {
	if entry.Disposition == RepairImportAbsent {
		return []string{repairPhasePlanned, repairPhaseAppendReserved, repairPhaseAppended, repairPhaseFrameVerified, repairPhaseRemoveReserved, repairPhaseClaimed, repairPhaseRemoved}
	}
	return []string{repairPhasePlanned, repairPhaseFrameVerified, repairPhaseRemoveReserved, repairPhaseClaimed, repairPhaseRemoved}
}

func postCompletionRepairMaxTransitions(plan PostCompletionRepairPlan) int {
	total := 0
	for _, entry := range plan.Entries {
		total += len(postCompletionRepairPhaseSequence(entry)) - 1
	}
	return total
}

func postCompletionRepairMaxJournalTransitions(plan PostCompletionRepairPlan) int {
	progressTransitions := postCompletionRepairMaxTransitions(plan)
	return progressTransitions + progressTransitions + 1
}

func newPostCompletionRepairProgressHeader(snapshot PostCompletionRepairPending) (PostCompletionRepairProgressHeader, []byte, error) {
	header := PostCompletionRepairProgressHeader{
		SchemaVersion: postCompletionRepairProgressSchema, PlanID: snapshot.PlanID,
		PlanDigest: snapshot.PlanDigest, PendingDigest: snapshot.PendingDigest, StartedAt: snapshot.StartedAt,
	}
	var err error
	header.HeaderDigest, err = postCompletionRepairProgressHeaderDigest(header)
	if err != nil {
		return header, nil, err
	}
	body, err := json.Marshal(header)
	if err != nil {
		return header, nil, err
	}
	body = append(body, '\n')
	if len(body) > maxPostCompletionRepairJournalLine {
		return header, nil, fmt.Errorf("post-completion repair progress header exceeds journal line bound")
	}
	return header, body, nil
}

func validatePostCompletionRepairProgressHeader(header PostCompletionRepairProgressHeader, snapshot PostCompletionRepairPending) error {
	if header.SchemaVersion != postCompletionRepairProgressSchema || header.PlanID != snapshot.PlanID || header.PlanDigest != snapshot.PlanDigest || header.PendingDigest != snapshot.PendingDigest || header.StartedAt != snapshot.StartedAt {
		return fmt.Errorf("post-completion repair progress header does not bind immutable pending snapshot")
	}
	if expected, err := postCompletionRepairProgressHeaderDigest(header); err != nil || expected != header.HeaderDigest {
		return errors.Join(fmt.Errorf("post-completion repair progress header digest mismatch"), err)
	}
	return nil
}

func validatePostCompletionRepairSnapshot(snapshot PostCompletionRepairPending, plan PostCompletionRepairPlan, auth PostCompletionRepairAuthorization) error {
	if err := validatePostCompletionRepairPending(snapshot, plan, auth); err != nil {
		return err
	}
	for _, entry := range snapshot.Entries {
		if entry.Phase != repairPhasePlanned || entry.Segment != "" || entry.SegmentOffset != 0 || entry.CanonicalSHA256 != "" {
			return fmt.Errorf("post-completion repair pending snapshot is not immutable initial state")
		}
	}
	return nil
}

func decodePostCompletionRepairJournalLine(line []byte, target any) error {
	if len(line) == 0 || len(line) > maxPostCompletionRepairJournalLine {
		return fmt.Errorf("post-completion repair progress journal line length invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("post-completion repair progress journal line has trailing content")
	}
	return nil
}

func postCompletionRepairRootOwns(root PostCompletionRepairRoot, kind, id string) bool {
	for _, artifact := range root.Artifacts {
		if artifact.Kind == kind && artifact.ID == id {
			return true
		}
	}
	return false
}

func postCompletionRepairLifecycleArtifactPlanID(artifact PostCompletionRepairArtifact) (string, bool) {
	suffix, ok := map[string]string{
		"pending":  ".pending.json",
		"progress": ".progress.jsonl",
		"result":   ".result.json",
		"proof":    ".proof.json",
	}[artifact.Kind]
	if !ok || filepath.Base(artifact.Path) != artifact.Path || !strings.HasSuffix(artifact.Path, suffix) {
		return "", false
	}
	planID := strings.TrimSuffix(artifact.Path, suffix)
	if filepath.Base(planID) != planID || !strings.HasPrefix(planID, "esrp_") {
		return "", false
	}
	return planID, true
}

func (s *Store) validatePostCompletionRepairResult(result PostCompletionRepairResult, plan PostCompletionRepairPlan, auth PostCompletionRepairAuthorization) error {
	if result.SchemaVersion != postCompletionRepairResultSchema || !strings.HasPrefix(result.ID, "esrm_") || result.PlanID != plan.ID || result.PlanDigest != plan.PlanDigest || result.SourceDigest != plan.SourceDigest || !postCompletionRepairAuthorizationEqual(result.Authorization, auth) || result.Total != plan.Total || !reflect.DeepEqual(result.Counts, plan.Counts) || len(result.Entries) != plan.Total || !result.Changed || result.Replayed || len(result.AuthorizationHistory) == 0 || !postCompletionRepairAuthorizationEqual(result.AuthorizationHistory[len(result.AuthorizationHistory)-1], auth) {
		return fmt.Errorf("post-completion repair result does not bind exact plan and policy authorization")
	}
	for index, historical := range result.AuthorizationHistory {
		if err := validatePostCompletionRepairAuthorization(historical, plan); err != nil {
			return fmt.Errorf("post-completion repair authorization history %d invalid: %w", index, err)
		}
		if index > 0 {
			if err := validatePostCompletionRepairReauthorization(result.AuthorizationHistory[index-1], historical, plan); err != nil {
				return fmt.Errorf("post-completion repair authorization history %d is not linear: %w", index, err)
			}
		}
	}
	completedAt, err := time.Parse(time.RFC3339Nano, result.CompletedAt)
	if err != nil {
		return err
	}
	if authorizedAt := strings.TrimSpace(auth.AuthorizedAt); authorizedAt != "" {
		latestAuthorizationAt, err := time.Parse(time.RFC3339Nano, authorizedAt)
		if err != nil {
			return fmt.Errorf("post-completion repair latest authorization time invalid: %w", err)
		}
		if completedAt.Before(latestAuthorizationAt) {
			return fmt.Errorf("post-completion repair result completed_at precedes latest authorization")
		}
	}
	if expected, err := postCompletionRepairResultDigest(result); err != nil || expected != result.ResultDigest {
		return errors.Join(fmt.Errorf("post-completion repair result digest mismatch"), err)
	}
	return nil
}

func validatePostCompletionRepairReauthorization(previous, next PostCompletionRepairAuthorization, plan PostCompletionRepairPlan) error {
	if err := validatePostCompletionRepairAuthorization(previous, plan); err != nil {
		return err
	}
	if err := validatePostCompletionRepairAuthorization(next, plan); err != nil {
		return err
	}
	if previous.WorkID != next.WorkID || previous.Actor != next.Actor || previous.ActionIntentID != next.ActionIntentID || previous.PlanDigest != next.PlanDigest || previous.ReviewID != next.ReviewID || previous.ReviewDigest != next.ReviewDigest || previous.WriterFenceDigest != next.WriterFenceDigest {
		return fmt.Errorf("post-completion repair reauthorization changed immutable work/plan/review scope")
	}
	if !postCompletionRepairSealedJSONEqual(previous.SealedAction, next.SealedAction) {
		return fmt.Errorf("post-completion repair reauthorization changed sealed action scope")
	}
	previousAt, previousErr := time.Parse(time.RFC3339Nano, previous.AuthorizedAt)
	nextAt, nextErr := time.Parse(time.RFC3339Nano, next.AuthorizedAt)
	if previousErr != nil || nextErr != nil || nextAt.Before(previousAt) {
		return errors.Join(fmt.Errorf("post-completion repair reauthorization time is not monotonic"), previousErr, nextErr)
	}
	if postCompletionRepairAuthorizationEqual(previous, next) || previous.AuthorityRevision == next.AuthorityRevision {
		return fmt.Errorf("post-completion repair reauthorization requires a new current authority revision")
	}
	return nil
}

func postCompletionRepairSealedJSONEqual(left, right []byte) bool {
	decode := func(body []byte) (any, bool) {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, false
		}
		return value, true
	}
	leftValue, leftOK := decode(left)
	rightValue, rightOK := decode(right)
	return leftOK && rightOK && reflect.DeepEqual(leftValue, rightValue)
}

type postCompletionRepairEnvelopeLink struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	IDs           struct {
		WorkID string `json:"work_id"`
	} `json:"ids"`
	ActionIntentID string   `json:"action_intent_id,omitempty"`
	ActionRefs     []string `json:"action_refs,omitempty"`
	AfterRefs      []string `json:"after_refs,omitempty"`
	ProofRefs      []string `json:"proof_refs,omitempty"`
}

func (s *Store) postCompletionRepairEnvelopeCreatedAt(id, kind string) (time.Time, error) {
	record, err := s.Read(id)
	if err != nil {
		return time.Time{}, fmt.Errorf("post-completion repair %s proof %s unresolved: %w", kind, id, err)
	}
	var link postCompletionRepairEnvelopeLink
	if err := json.Unmarshal(record.Bytes, &link); err != nil {
		return time.Time{}, fmt.Errorf("post-completion repair %s proof %s invalid: %w", kind, id, err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, link.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("post-completion repair %s proof created_at invalid: %w", kind, err)
	}
	return createdAt, nil
}

func (s *Store) validatePostCompletionRepairEnvelopeChain(auth PostCompletionRepairAuthorization, receiptID, transitionID string, requiredReceiptRefs ...string) error {
	expected := []struct {
		id, schema string
	}{
		{auth.ActionIntentID, "compound.action_intent.v0.1"},
		{auth.PolicyDecisionID, postCompletionRepairPolicySchema},
		{receiptID, "compound.receipt.v0.1"},
		{transitionID, "compound.transition.v0.1"},
	}
	links := make([]postCompletionRepairEnvelopeLink, 0, len(expected))
	for _, want := range expected {
		if want.id == "" {
			return fmt.Errorf("post-completion repair work proof id required")
		}
		record, err := s.Read(want.id)
		if err != nil {
			return fmt.Errorf("post-completion repair work proof %s unresolved: %w", want.id, err)
		}
		var link postCompletionRepairEnvelopeLink
		if err := json.Unmarshal(record.Bytes, &link); err != nil {
			return fmt.Errorf("post-completion repair work proof %s invalid: %w", want.id, err)
		}
		if link.ID != want.id || link.SchemaVersion != want.schema || link.IDs.WorkID != auth.WorkID {
			return fmt.Errorf("post-completion repair work proof %s does not bind schema/id/work", want.id)
		}
		links = append(links, link)
	}
	if links[1].ActionIntentID != auth.ActionIntentID {
		return fmt.Errorf("post-completion repair policy proof does not bind action intent")
	}
	if !containsRepairString(links[2].ProofRefs, "eventstore-repair-plan:"+auth.PlanDigest) || !containsRepairString(links[2].ProofRefs, "eventstore-repair-review:"+auth.ReviewDigest) || !containsRepairString(links[2].ProofRefs, "policy-authority:"+auth.AuthorityRevision) {
		return fmt.Errorf("post-completion repair receipt proof does not bind plan, operator review, and policy authority")
	}
	for _, requiredRef := range requiredReceiptRefs {
		if !containsRepairString(links[2].ProofRefs, requiredRef) {
			return fmt.Errorf("post-completion repair receipt proof does not bind %s", requiredRef)
		}
	}
	if !containsRepairString(links[3].ActionRefs, auth.ActionIntentID) || !containsRepairString(links[3].AfterRefs, receiptID) {
		return fmt.Errorf("post-completion repair transition proof does not bind action and receipt")
	}
	return nil
}

func (s *Store) validatePostCompletionRepairProof(proof PostCompletionRepairProof, result PostCompletionRepairResult) error {
	if proof.SchemaVersion != postCompletionRepairProofSchema || proof.PlanID != result.PlanID || proof.PlanDigest != result.PlanDigest || proof.ResultID != result.ID || proof.ResultDigest != result.ResultDigest || !postCompletionRepairAuthorizationEqual(proof.Authorization, result.Authorization) || !strings.HasPrefix(proof.ReceiptID, "rec_") || !strings.HasPrefix(proof.TransitionID, "tr_") {
		return fmt.Errorf("post-completion repair work proof does not bind exact result")
	}
	recordedAt, err := time.Parse(time.RFC3339Nano, proof.RecordedAt)
	if err != nil {
		return fmt.Errorf("post-completion repair work proof recorded_at invalid: %w", err)
	}
	completedAt, err := time.Parse(time.RFC3339Nano, result.CompletedAt)
	if err != nil {
		return fmt.Errorf("post-completion repair result completed_at invalid: %w", err)
	}
	latestAuthorizationAt := completedAt
	if authorizedAt := strings.TrimSpace(proof.Authorization.AuthorizedAt); authorizedAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, authorizedAt)
		if err != nil {
			return fmt.Errorf("post-completion repair latest authorization time invalid: %w", err)
		}
		if parsed.After(latestAuthorizationAt) {
			latestAuthorizationAt = parsed
		}
	}
	if recordedAt.Before(latestAuthorizationAt) {
		return fmt.Errorf("post-completion repair proof recorded_at precedes latest authorization or result completion")
	}
	if expected, err := postCompletionRepairProofDigest(proof); err != nil || expected != proof.ProofDigest {
		return errors.Join(fmt.Errorf("post-completion repair work proof digest mismatch"), err)
	}
	if err := s.validatePostCompletionRepairEnvelopeChain(proof.Authorization, proof.ReceiptID, proof.TransitionID); err != nil {
		return err
	}
	receiptAt, err := s.postCompletionRepairEnvelopeCreatedAt(proof.ReceiptID, "receipt")
	if err != nil {
		return err
	}
	transitionAt, err := s.postCompletionRepairEnvelopeCreatedAt(proof.TransitionID, "transition")
	if err != nil {
		return err
	}
	if receiptAt.Before(completedAt) {
		return fmt.Errorf("post-completion repair receipt created_at precedes result completion")
	}
	if transitionAt.Before(receiptAt) {
		return fmt.Errorf("post-completion repair transition created_at precedes receipt")
	}
	for _, predecessor := range []struct {
		name string
		at   time.Time
	}{
		{name: "receipt", at: receiptAt},
		{name: "transition", at: transitionAt},
	} {
		if recordedAt.Before(predecessor.at) {
			return fmt.Errorf("post-completion repair proof recorded_at precedes %s", predecessor.name)
		}
	}
	return nil
}

func (s *Store) validatePostCompletionRepairSupersessionProof(proof PostCompletionRepairSupersessionProof, supersession PostCompletionRepairSupersession) error {
	if proof.SchemaVersion != postCompletionRepairSupersessionProofSchema || proof.PlanID != supersession.PlanID || proof.PlanDigest != supersession.PlanDigest || proof.SupersessionID != supersession.ID || proof.SupersessionDigest != supersession.SupersessionDigest || !postCompletionRepairAuthorizationEqual(proof.Authorization, supersession.Authorization) || !strings.HasPrefix(proof.ReceiptID, "rec_") || !strings.HasPrefix(proof.TransitionID, "tr_") {
		return fmt.Errorf("post-completion repair supersession work proof does not bind exact terminal artifact")
	}
	recordedAt, err := time.Parse(time.RFC3339Nano, proof.RecordedAt)
	if err != nil {
		return fmt.Errorf("post-completion repair supersession work proof recorded_at invalid: %w", err)
	}
	supersessionAt, err := time.Parse(time.RFC3339Nano, supersession.CreatedAt)
	if err != nil {
		return fmt.Errorf("post-completion repair supersession created_at invalid: %w", err)
	}
	if expected, err := postCompletionRepairSupersessionProofDigest(proof); err != nil || expected != proof.ProofDigest {
		return errors.Join(fmt.Errorf("post-completion repair supersession work proof digest mismatch"), err)
	}
	if err := s.validatePostCompletionRepairEnvelopeChain(
		proof.Authorization,
		proof.ReceiptID,
		proof.TransitionID,
		"eventstore-repair-successor-plan:"+supersession.SuccessorPlanDigest,
		"eventstore-repair-supersession:"+supersession.ID,
	); err != nil {
		return err
	}
	receiptAt, err := s.postCompletionRepairEnvelopeCreatedAt(proof.ReceiptID, "receipt")
	if err != nil {
		return err
	}
	transitionAt, err := s.postCompletionRepairEnvelopeCreatedAt(proof.TransitionID, "transition")
	if err != nil {
		return err
	}
	if receiptAt.Before(supersessionAt) {
		return fmt.Errorf("post-completion repair supersession receipt created_at precedes supersession")
	}
	if transitionAt.Before(receiptAt) {
		return fmt.Errorf("post-completion repair supersession transition created_at precedes receipt")
	}
	for _, predecessor := range []struct {
		name string
		at   time.Time
	}{
		{name: "supersession", at: supersessionAt},
		{name: "receipt", at: receiptAt},
		{name: "transition", at: transitionAt},
	} {
		if recordedAt.Before(predecessor.at) {
			return fmt.Errorf("post-completion repair supersession proof recorded_at precedes %s", predecessor.name)
		}
	}
	return nil
}

func postCompletionRepairAuthorizationEqual(left, right PostCompletionRepairAuthorization) bool {
	leftAction, rightAction := left.SealedAction, right.SealedAction
	leftDecision, rightDecision := left.SealedDecision, right.SealedDecision
	left.SealedAction, right.SealedAction = nil, nil
	left.SealedDecision, right.SealedDecision = nil, nil
	return reflect.DeepEqual(left, right) && repairRawJSONEqual(leftAction, rightAction) && repairRawJSONEqual(leftDecision, rightDecision)
}

func repairRawJSONEqual(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == len(right)
	}
	leftCanonical, leftErr := canonical(left)
	rightCanonical, rightErr := canonical(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func containsRepairString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (s *Store) verifyPostCompletionRepairResultState(result PostCompletionRepairResult, requireNoNewLoose bool) error {
	presence, err := s.engine.buildSegmentPresence()
	if err != nil {
		return err
	}
	plannedIDs := map[string]bool{}
	for _, entry := range result.Entries {
		plannedIDs[entry.ID] = true
		archive, err := s.readPostCompletionRepairArchive(entry.ArchiveSHA256, entry.ID)
		if err != nil || recordHash(archive) != entry.ArchiveSHA256 {
			return fmt.Errorf("post-completion repair result archive %s does not verify: %w", entry.ID, err)
		}
		loc, ok := presence[entry.ID]
		if !ok || loc.sha256 != entry.SegmentCanonicalSHA256 || loc.relPath != entry.Segment || loc.offset != entry.SegmentOffset {
			return fmt.Errorf("post-completion repair result segment %s does not verify", entry.ID)
		}
		loosePath := filepath.Join(s.rootPath(), entry.ID+".json")
		if info, err := os.Lstat(loosePath); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("post-completion repair result loose %s reappeared without exact archived bytes", entry.ID)
			}
			loose, readErr := os.ReadFile(loosePath)
			if readErr != nil || recordHash(loose) != entry.ArchiveSHA256 || !bytes.Equal(loose, archive) {
				return errors.Join(fmt.Errorf("post-completion repair result loose %s reappeared without exact archived bytes", entry.ID), readErr)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("post-completion repair result loose %s state unreadable: %w", entry.ID, err)
		}
	}
	if requireNoNewLoose {
		m := &migrator{store: s, engine: s.engine}
		loose, err := m.enumerateLoose()
		if err != nil {
			return err
		}
		for _, lf := range loose {
			if !plannedIDs[lf.id] {
				return fmt.Errorf("post-completion repair source state drift: unplanned loose record %s appeared", lf.id)
			}
		}
	}
	return nil
}

func postCompletionRepairPlanDigest(plan PostCompletionRepairPlan) (string, error) {
	plan.PlanDigest = ""
	return digestRepairValue(plan)
}

func postCompletionRepairReviewDigest(review PostCompletionRepairReview) (string, error) {
	review.ReviewDigest = ""
	return digestRepairValue(review)
}

func postCompletionRepairSupersessionDigest(supersession PostCompletionRepairSupersession) (string, error) {
	supersession.SupersessionDigest = ""
	return digestRepairValue(supersession)
}

func postCompletionRepairSupersessionProofDigest(proof PostCompletionRepairSupersessionProof) (string, error) {
	proof.ProofDigest = ""
	return digestRepairValue(proof)
}

func postCompletionRepairPendingDigest(pending PostCompletionRepairPending) (string, error) {
	pending.PendingDigest = ""
	return digestRepairValue(pending)
}

func postCompletionRepairProgressHeaderDigest(header PostCompletionRepairProgressHeader) (string, error) {
	header.HeaderDigest = ""
	return digestRepairValue(header)
}

func postCompletionRepairTransitionDigest(transition PostCompletionRepairProgressTransition) (string, error) {
	transition.TransitionDigest = ""
	return digestRepairValue(transition)
}

func postCompletionRepairResultDigest(result PostCompletionRepairResult) (string, error) {
	result.ResultDigest = ""
	result.Replayed = false
	return digestRepairValue(result)
}

func postCompletionRepairProofDigest(proof PostCompletionRepairProof) (string, error) {
	proof.ProofDigest = ""
	return digestRepairValue(proof)
}

func digestRepairValue(value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return "sha256:" + digestRepairHex(body), nil
}

func digestRepairHex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validRepairDigest(value string) bool {
	raw := strings.TrimPrefix(value, "sha256:")
	if raw == value || len(raw) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size
}

func postCompletionRepairRootDigest(root PostCompletionRepairRoot) (string, error) {
	root.RootDigest = ""
	return digestRepairValue(root)
}

func postCompletionRepairChainHead(root PostCompletionRepairRoot) (string, error) {
	return digestRepairValue(struct {
		Revision          uint64                         `json:"revision"`
		PreviousRoot      string                         `json:"previous_root_digest,omitempty"`
		PreviousChainHead string                         `json:"previous_chain_head,omitempty"`
		Artifacts         []PostCompletionRepairArtifact `json:"artifacts"`
	}{root.Revision, root.PreviousRoot, root.PreviousChainHead, root.Artifacts})
}

func postCompletionRepairAnchorDigest(anchor PostCompletionRepairAnchor) (string, error) {
	anchor.AnchorDigest = ""
	return digestRepairValue(anchor)
}

func validatePostCompletionRepairRoot(root PostCompletionRepairRoot) error {
	if root.SchemaVersion != postCompletionRepairRootSchema || root.Revision == 0 || !validRepairDigest(root.ChainHead) || !validRepairDigest(root.RootDigest) {
		return fmt.Errorf("post-completion repair root identity invalid")
	}
	if root.Revision == 1 {
		if root.PreviousRoot != "" || root.PreviousChainHead != "" {
			return fmt.Errorf("post-completion repair root genesis linkage invalid")
		}
	} else if !validRepairDigest(root.PreviousRoot) || !validRepairDigest(root.PreviousChainHead) {
		return fmt.Errorf("post-completion repair root predecessor linkage invalid")
	}
	if expected, err := postCompletionRepairChainHead(root); err != nil || expected != root.ChainHead {
		return errors.Join(fmt.Errorf("post-completion repair root chain head mismatch"), err)
	}
	if expected, err := postCompletionRepairRootDigest(root); err != nil || expected != root.RootDigest {
		return errors.Join(fmt.Errorf("post-completion repair root digest mismatch"), err)
	}
	seen := map[string]bool{}
	seenPath := map[string]bool{}
	last := ""
	for _, artifact := range root.Artifacts {
		key := artifact.Kind + ":" + artifact.ID
		wantSuffix := map[string]string{"plan": ".plan.json", "review": ".review.json", "supersession": ".supersession.json", "pending": ".pending.json", "progress": ".progress.jsonl", "result": ".result.json", "proof": ".proof.json"}[artifact.Kind]
		if wantSuffix == "" || artifact.ID == "" || filepath.Base(artifact.Path) != artifact.Path || !strings.HasSuffix(artifact.Path, wantSuffix) || !validRepairDigest(artifact.SHA256) || seen[key] || seenPath[artifact.Path] || (last != "" && key < last) {
			return fmt.Errorf("post-completion repair root artifact %q invalid or unsorted", key)
		}
		hasAuthorizationHead := artifact.AuthorizationCount != 0 || artifact.AuthorizationDigest != "" || artifact.AuthorizationSequence != 0 || artifact.AuthorizationTransitionHead != ""
		if artifact.Kind != "progress" && hasAuthorizationHead {
			return fmt.Errorf("post-completion repair non-progress artifact %q carries authorization head", key)
		}
		if artifact.Kind == "progress" && hasAuthorizationHead && (artifact.AuthorizationCount < 1 || !validRepairDigest(artifact.AuthorizationDigest) || !validRepairDigest(artifact.AuthorizationTransitionHead)) {
			return fmt.Errorf("post-completion repair progress artifact %q authorization head invalid", key)
		}
		seen[key], seenPath[artifact.Path], last = true, true, key
	}
	return nil
}

func validatePostCompletionRepairAnchor(anchor PostCompletionRepairAnchor) error {
	if anchor.SchemaVersion != postCompletionRepairAnchorSchema || anchor.Revision == 0 || anchor.ArtifactCount < 0 || !validRepairDigest(anchor.RootDigest) || !validRepairDigest(anchor.ChainHead) || !validRepairDigest(anchor.AnchorDigest) {
		return fmt.Errorf("post-completion repair authority anchor identity invalid")
	}
	if expected, err := postCompletionRepairAnchorDigest(anchor); err != nil || expected != anchor.AnchorDigest {
		return errors.Join(fmt.Errorf("post-completion repair authority anchor digest mismatch"), err)
	}
	return nil
}

func (s *Store) loadPostCompletionRepairRoot() (PostCompletionRepairRoot, []byte, bool, error) {
	path := s.postCompletionRepairRootPath()
	body, err := homepkg.ReadRootedRegularFile(s.home, path, "post-completion repair root")
	if errors.Is(err, os.ErrNotExist) {
		return PostCompletionRepairRoot{SchemaVersion: postCompletionRepairRootSchema, Artifacts: []PostCompletionRepairArtifact{}}, nil, false, nil
	}
	if err != nil {
		return PostCompletionRepairRoot{}, nil, false, err
	}
	root, err := decodeStrictJSON[PostCompletionRepairRoot](body)
	if err != nil {
		return PostCompletionRepairRoot{}, nil, true, err
	}
	if err := validatePostCompletionRepairRoot(root); err != nil {
		return PostCompletionRepairRoot{}, nil, true, err
	}
	return root, body, true, nil
}

func (s *Store) loadPostCompletionRepairAnchor() (PostCompletionRepairAnchor, bool, error) {
	anchor, err := loadStrictRootedJSON[PostCompletionRepairAnchor](s.home, s.postCompletionRepairAnchorPath(), "post-completion repair authority anchor")
	if errors.Is(err, os.ErrNotExist) {
		return PostCompletionRepairAnchor{}, false, nil
	}
	if err != nil {
		return PostCompletionRepairAnchor{}, false, err
	}
	if err := validatePostCompletionRepairAnchor(anchor); err != nil {
		return PostCompletionRepairAnchor{}, true, err
	}
	return anchor, true, nil
}

func verifyPostCompletionRepairRootAnchor(root PostCompletionRepairRoot, rootExists bool, anchor PostCompletionRepairAnchor, anchorExists bool) error {
	if !rootExists && !anchorExists {
		return nil
	}
	if !rootExists {
		return fmt.Errorf("post-completion repair authority anchor exists but repair root is missing")
	}
	if !anchorExists {
		return fmt.Errorf("post-completion repair root exists but authority anchor is missing")
	}
	if anchor.Revision != root.Revision || anchor.ArtifactCount != len(root.Artifacts) || anchor.RootDigest != root.RootDigest || anchor.ChainHead != root.ChainHead {
		return fmt.Errorf("post-completion repair authority anchor/root mismatch")
	}
	return nil
}

// mutatePostCompletionRepairRoot is an atomic compare-and-swap rollup update.
// Callers already serialize repair operations with the migration run lock;
// the byte comparison still refuses a foreign writer instead of overwriting it.
func (s *Store) mutatePostCompletionRepairRoot(mutate func(*PostCompletionRepairRoot) error) error {
	root, expected, existed, err := s.loadPostCompletionRepairRoot()
	if err != nil {
		return err
	}
	anchor, anchorExists, err := s.loadPostCompletionRepairAnchor()
	if err != nil {
		return err
	}
	if pairErr := verifyPostCompletionRepairRootAnchor(root, existed, anchor, anchorExists); pairErr != nil {
		anchor, anchorExists, err = s.resumePostCompletionRepairAnchor(root, existed, anchor, anchorExists)
		if err != nil {
			return errors.Join(pairErr, err)
		}
		if err := verifyPostCompletionRepairRootAnchor(root, existed, anchor, anchorExists); err != nil {
			return err
		}
	}
	before := append([]PostCompletionRepairArtifact(nil), root.Artifacts...)
	if err := mutate(&root); err != nil {
		return err
	}
	sort.Slice(root.Artifacts, func(i, j int) bool {
		left := root.Artifacts[i].Kind + ":" + root.Artifacts[i].ID
		right := root.Artifacts[j].Kind + ":" + root.Artifacts[j].ID
		return left < right
	})
	if reflect.DeepEqual(before, root.Artifacts) {
		return nil
	}
	previousRoot, previousChain := root.RootDigest, root.ChainHead
	root.Revision++
	root.PreviousRoot, root.PreviousChainHead = previousRoot, previousChain
	if root.Revision == 1 {
		root.PreviousRoot, root.PreviousChainHead = "", ""
	}
	root.ChainHead, err = postCompletionRepairChainHead(root)
	if err != nil {
		return err
	}
	root.RootDigest, err = postCompletionRepairRootDigest(root)
	if err != nil {
		return err
	}
	if err := validatePostCompletionRepairRoot(root); err != nil {
		return err
	}
	current, readErr := homepkg.ReadRootedRegularFile(s.home, s.postCompletionRepairRootPath(), "post-completion repair root")
	if existed {
		if readErr != nil || !bytes.Equal(current, expected) {
			return fmt.Errorf("post-completion repair root CAS drift")
		}
	} else if readErr == nil || !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("post-completion repair root CAS creation drift: %w", readErr)
	}
	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := writeBytesAtomic(s.postCompletionRepairRootPath(), append(body, '\n')); err != nil {
		return err
	}
	anchor = PostCompletionRepairAnchor{
		SchemaVersion: postCompletionRepairAnchorSchema,
		Revision:      root.Revision,
		ArtifactCount: len(root.Artifacts),
		RootDigest:    root.RootDigest,
		ChainHead:     root.ChainHead,
	}
	anchor.AnchorDigest, err = postCompletionRepairAnchorDigest(anchor)
	if err != nil {
		return err
	}
	anchorBody, err := json.MarshalIndent(anchor, "", "  ")
	if err != nil {
		return err
	}
	return writeBytesAtomic(s.postCompletionRepairAnchorPath(), append(anchorBody, '\n'))
}

func (s *Store) resumePostCompletionRepairAnchor(root PostCompletionRepairRoot, rootExists bool, anchor PostCompletionRepairAnchor, anchorExists bool) (PostCompletionRepairAnchor, bool, error) {
	if !rootExists {
		return anchor, anchorExists, fmt.Errorf("post-completion repair root missing during anchor recovery")
	}
	if anchorExists {
		if root.Revision != anchor.Revision+1 || root.PreviousRoot != anchor.RootDigest || root.PreviousChainHead != anchor.ChainHead {
			return anchor, true, fmt.Errorf("post-completion repair root is not the single anchored successor")
		}
	} else if root.Revision != 1 || root.PreviousRoot != "" || root.PreviousChainHead != "" {
		return anchor, false, fmt.Errorf("post-completion repair root is not recoverable genesis")
	}
	if err := s.verifyPostCompletionRepairRootArtifactBytes(root); err != nil {
		return anchor, anchorExists, fmt.Errorf("post-completion repair root artifacts do not support anchor recovery: %w", err)
	}
	recovered := PostCompletionRepairAnchor{
		SchemaVersion: postCompletionRepairAnchorSchema,
		Revision:      root.Revision,
		ArtifactCount: len(root.Artifacts),
		RootDigest:    root.RootDigest,
		ChainHead:     root.ChainHead,
	}
	var err error
	recovered.AnchorDigest, err = postCompletionRepairAnchorDigest(recovered)
	if err != nil {
		return anchor, anchorExists, err
	}
	body, err := json.MarshalIndent(recovered, "", "  ")
	if err != nil {
		return anchor, anchorExists, err
	}
	if err := writeBytesAtomic(s.postCompletionRepairAnchorPath(), append(body, '\n')); err != nil {
		return anchor, anchorExists, err
	}
	return recovered, true, nil
}

func (s *Store) verifyPostCompletionRepairRootArtifactBytes(root PostCompletionRepairRoot) error {
	for _, artifact := range root.Artifacts {
		body, err := homepkg.ReadRootedRegularFile(s.home, filepath.Join(s.engine.postCompletionRepairRoot(), artifact.Path), "post-completion repair indexed "+artifact.Kind+" artifact")
		if err != nil {
			return err
		}
		if artifact.Kind == "progress" {
			body, err = postCompletionRepairProgressHeaderBytes(body)
			if err != nil {
				return err
			}
		}
		if got := "sha256:" + recordHash(body); got != artifact.SHA256 {
			return fmt.Errorf("artifact %s digest mismatch", artifact.Path)
		}
	}
	return nil
}

func (s *Store) ensurePostCompletionRepairArtifact(kind, id, path string) error {
	body, err := homepkg.ReadRootedRegularFile(s.home, path, "post-completion repair "+kind+" artifact")
	if err != nil {
		return err
	}
	digestBody := body
	if kind == "progress" {
		digestBody, err = postCompletionRepairProgressHeaderBytes(body)
		if err != nil {
			return err
		}
	}
	want := PostCompletionRepairArtifact{Kind: kind, ID: id, Path: filepath.Base(path), SHA256: "sha256:" + recordHash(digestBody)}
	return s.mutatePostCompletionRepairRoot(func(root *PostCompletionRepairRoot) error {
		for _, artifact := range root.Artifacts {
			if artifact.Kind == kind && artifact.ID == id {
				if reflect.DeepEqual(artifact, want) {
					return nil
				}
				return fmt.Errorf("immutable repair artifact drift for %s:%s", kind, id)
			}
		}
		root.Artifacts = append(root.Artifacts, want)
		return nil
	})
}

func newPostCompletionRepairAuthorizationHead(auth PostCompletionRepairAuthorization, count int, sequence uint64, transitionHead string) (postCompletionRepairAuthorizationHead, error) {
	digest, err := digestRepairValue(auth)
	if err != nil {
		return postCompletionRepairAuthorizationHead{}, err
	}
	if count < 1 || !validRepairDigest(transitionHead) {
		return postCompletionRepairAuthorizationHead{}, fmt.Errorf("post-completion repair authorization head identity invalid")
	}
	return postCompletionRepairAuthorizationHead{Count: count, Digest: digest, Sequence: sequence, TransitionHead: transitionHead}, nil
}

func postCompletionRepairArtifactAuthorizationHead(artifact PostCompletionRepairArtifact) postCompletionRepairAuthorizationHead {
	return postCompletionRepairAuthorizationHead{
		Count: artifact.AuthorizationCount, Digest: artifact.AuthorizationDigest,
		Sequence: artifact.AuthorizationSequence, TransitionHead: artifact.AuthorizationTransitionHead,
	}
}

func setPostCompletionRepairArtifactAuthorizationHead(artifact *PostCompletionRepairArtifact, head postCompletionRepairAuthorizationHead) {
	artifact.AuthorizationCount = head.Count
	artifact.AuthorizationDigest = head.Digest
	artifact.AuthorizationSequence = head.Sequence
	artifact.AuthorizationTransitionHead = head.TransitionHead
}

func (s *Store) validatePostCompletionRepairProgressAuthorizationHead(planID string, state postCompletionRepairProgressState) error {
	root, _, exists, err := s.loadPostCompletionRepairRoot()
	if err != nil || !exists {
		return err
	}
	for _, artifact := range root.Artifacts {
		if artifact.Kind != "progress" || artifact.ID != planID || artifact.AuthorizationCount == 0 {
			continue
		}
		if artifact.AuthorizationCount > len(state.AuthorizationHeads) {
			return fmt.Errorf("post-completion repair authorization history was truncated behind its rooted head")
		}
		want := state.AuthorizationHeads[artifact.AuthorizationCount-1]
		if !reflect.DeepEqual(postCompletionRepairArtifactAuthorizationHead(artifact), want) {
			return fmt.Errorf("post-completion repair rooted authorization head does not match journal history")
		}
	}
	return nil
}

func postCompletionRepairProgressAuthorizationHeadCurrent(artifact PostCompletionRepairArtifact, state postCompletionRepairProgressState) bool {
	if len(state.AuthorizationHeads) == 0 {
		return false
	}
	return reflect.DeepEqual(postCompletionRepairArtifactAuthorizationHead(artifact), state.AuthorizationHeads[len(state.AuthorizationHeads)-1])
}

func (s *Store) ensurePostCompletionRepairProgressArtifact(planID string, state postCompletionRepairProgressState) error {
	if len(state.AuthorizationHeads) == 0 {
		return fmt.Errorf("post-completion repair progress authorization head required")
	}
	path := s.postCompletionRepairProgressPath(planID)
	body, err := homepkg.ReadRootedRegularFile(s.home, path, "post-completion repair progress journal")
	if err != nil {
		return err
	}
	headerBody, err := postCompletionRepairProgressHeaderBytes(body)
	if err != nil {
		return err
	}
	want := PostCompletionRepairArtifact{Kind: "progress", ID: planID, Path: filepath.Base(path), SHA256: "sha256:" + recordHash(headerBody)}
	setPostCompletionRepairArtifactAuthorizationHead(&want, state.AuthorizationHeads[len(state.AuthorizationHeads)-1])
	return s.mutatePostCompletionRepairRoot(func(root *PostCompletionRepairRoot) error {
		for index, artifact := range root.Artifacts {
			if artifact.Kind != "progress" || artifact.ID != planID {
				continue
			}
			if artifact.Path != want.Path || artifact.SHA256 != want.SHA256 {
				return fmt.Errorf("immutable repair artifact drift for progress:%s", planID)
			}
			if artifact.AuthorizationCount > len(state.AuthorizationHeads) {
				return fmt.Errorf("post-completion repair authorization history was truncated behind its rooted head")
			}
			if artifact.AuthorizationCount > 0 {
				anchored := state.AuthorizationHeads[artifact.AuthorizationCount-1]
				if !reflect.DeepEqual(postCompletionRepairArtifactAuthorizationHead(artifact), anchored) {
					return fmt.Errorf("post-completion repair rooted authorization head does not match journal history")
				}
			}
			if reflect.DeepEqual(artifact, want) {
				return nil
			}
			root.Artifacts[index] = want
			return nil
		}
		root.Artifacts = append(root.Artifacts, want)
		return nil
	})
}

func (s *Store) removePostCompletionRepairArtifact(kind, id string) error {
	return s.mutatePostCompletionRepairRoot(func(root *PostCompletionRepairRoot) error {
		for index, artifact := range root.Artifacts {
			if artifact.Kind == kind && artifact.ID == id {
				root.Artifacts = append(root.Artifacts[:index], root.Artifacts[index+1:]...)
				return nil
			}
		}
		return nil
	})
}

func postCompletionRepairProgressHeaderBytes(body []byte) ([]byte, error) {
	newline := bytes.IndexByte(body, '\n')
	if newline < 0 || newline+1 > maxPostCompletionRepairJournalLine {
		return nil, fmt.Errorf("post-completion repair progress journal header missing or oversized")
	}
	var header PostCompletionRepairProgressHeader
	if err := decodePostCompletionRepairJournalLine(body[:newline], &header); err != nil {
		return nil, err
	}
	if header.SchemaVersion != postCompletionRepairProgressSchema {
		return nil, fmt.Errorf("post-completion repair progress journal schema invalid")
	}
	if expected, err := postCompletionRepairProgressHeaderDigest(header); err != nil || expected != header.HeaderDigest {
		return nil, errors.Join(fmt.Errorf("post-completion repair progress header digest mismatch"), err)
	}
	return body[:newline+1], nil
}

func newRepairCounts() map[string]int {
	return map[string]int{RepairImportAbsent: 0, RepairArchiveIdentical: 0, RepairArchiveDivergent: 0}
}

func cloneRepairCounts(in map[string]int) map[string]int {
	out := newRepairCounts()
	for key, value := range in {
		out[key] = value
	}
	return out
}

func repairCountsEqual(counts map[string]int, entries []PostCompletionRepairEntry) bool {
	want := newRepairCounts()
	for _, entry := range entries {
		want[entry.Disposition]++
	}
	return reflect.DeepEqual(counts, want)
}

func (s *Store) writePostCompletionRepairExclusiveOrEqualJSON(path, kind string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if existing, err := homepkg.ReadRootedRegularFile(s.home, path, kind); err == nil {
		if !bytes.Equal(existing, body) {
			return fmt.Errorf("immutable artifact %s already exists with different bytes", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	created, err := writeBytesExclusiveAtomic(path, body)
	if err != nil {
		return err
	}
	if !created {
		existing, err := homepkg.ReadRootedRegularFile(s.home, path, kind)
		if err != nil || !bytes.Equal(existing, body) {
			return fmt.Errorf("immutable artifact %s raced with different bytes: %w", path, err)
		}
	}
	return nil
}

func (s *Store) removePostCompletionRepairExactJSON(path string, expected any, kind string) error {
	want, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		return err
	}
	want = append(want, '\n')
	// Required-exists semantics: absence is an error. The rooted validated
	// removal closes the read/compare→path-remove race — the compared bytes
	// and the unlinked inode are the same custody.
	if err := homepkg.RemoveRootedRegularFileDurableValidated(s.home, path, kind, func(actual []byte) error {
		if !bytes.Equal(actual, want) {
			return fmt.Errorf("pending journal %s changed before cleanup", path)
		}
		return nil
	}); err != nil {
		return err
	}
	claimDir := filepath.Dir(path)
	quarantineDir := filepath.Dir(claimDir)
	if err := os.Remove(claimDir); err == nil {
		if err := syncDirectory(quarantineDir); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	if err := os.Remove(quarantineDir); err == nil {
		return syncDirectory(filepath.Dir(quarantineDir))
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
}

func (s *Store) removePostCompletionRepairExactBytes(path string, expected []byte, kind string) error {
	// ENOENT-success semantics preserved; the rooted validated removal closes
	// the read/compare→path-remove race for present files.
	err := homepkg.RemoveRootedRegularFileDurableValidated(s.home, path, kind, func(actual []byte) error {
		if !bytes.Equal(actual, expected) {
			return fmt.Errorf("refuse to remove changed file %s", path)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func mustRepairJSON(value any) []byte {
	body, _ := json.MarshalIndent(value, "", "  ")
	return append(body, '\n')
}

func loadStrictJSON[T any](path string) (T, error) {
	var value T
	body, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	return decodeStrictJSON[T](body)
}

func loadStrictRootedJSON[T any](homePath, path, kind string) (T, error) {
	var value T
	body, err := homepkg.ReadRootedRegularFile(homePath, path, kind)
	if err != nil {
		return value, err
	}
	return decodeStrictJSON[T](body)
}

func decodeStrictJSON[T any](body []byte) (T, error) {
	var value T
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return value, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return value, err
	}
	return value, nil
}

// verifyPostCompletionRepairManifests is folded into migrate --verify. A
// result is the additive repair-baseline/manifest; the immutable v0 plan and
// manifest stay independently hash-bound by every repair plan.
func (s *Store) verifyPostCompletionRepairManifests() (plans, records, divergent int, reasons []string, err error) {
	root := s.engine.postCompletionRepairRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		if _, exists, anchorErr := s.loadPostCompletionRepairAnchor(); anchorErr != nil {
			return 0, 0, 0, []string{"post-completion repair authority anchor invalid: " + anchorErr.Error()}, nil
		} else if exists {
			return 0, 0, 0, []string{"post-completion repair authority anchor exists but repair root is missing"}, nil
		}
		return 0, 0, 0, nil, nil
	}
	if err != nil {
		return 0, 0, 0, nil, err
	}
	inventoryReasons, indexed, inventoryErr := s.verifyPostCompletionRepairRoot(entries)
	if inventoryErr != nil {
		return 0, 0, 0, nil, inventoryErr
	}
	reasons = append(reasons, inventoryReasons...)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == postCompletionRepairRootFile {
			continue
		}
		if isPostCompletionRepairArtifactName(name) && !indexed[name] {
			// The root verifier already reports this orphan. Do not allow a
			// directory-discovered result to contribute authoritative counts.
			continue
		}
		if strings.HasSuffix(name, ".pending.json") {
			reasons = append(reasons, "post-completion repair finalization pending: "+name)
			continue
		}
		if strings.HasSuffix(name, ".progress.jsonl") {
			continue
		}
		if !strings.HasSuffix(name, ".result.json") {
			continue
		}
		resultPath := filepath.Join(root, name)
		result, loadErr := loadStrictRootedJSON[PostCompletionRepairResult](s.home, resultPath, "post-completion repair result")
		if loadErr != nil {
			reasons = append(reasons, fmt.Sprintf("post-completion repair result %s unreadable: %v", name, loadErr))
			continue
		}
		plan, loadErr := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(result.PlanID))
		if loadErr != nil {
			reasons = append(reasons, fmt.Sprintf("post-completion repair result %s plan invalid: %v", name, loadErr))
			continue
		}
		if loadErr = s.validatePostCompletionRepairResult(result, plan, result.Authorization); loadErr != nil {
			reasons = append(reasons, fmt.Sprintf("post-completion repair result %s invalid: %v", name, loadErr))
			continue
		}
		if loadErr = s.verifyPostCompletionRepairResultState(result, false); loadErr != nil {
			reasons = append(reasons, fmt.Sprintf("post-completion repair result %s state invalid: %v", name, loadErr))
			continue
		}
		proof, present, loadErr := s.ReadPostCompletionRepairProof(result.PlanID)
		if loadErr != nil || !present {
			reasons = append(reasons, fmt.Sprintf("post-completion repair result %s work proof missing: %v", name, loadErr))
			continue
		}
		if loadErr = s.validatePostCompletionRepairProof(proof, result); loadErr != nil {
			reasons = append(reasons, fmt.Sprintf("post-completion repair result %s work proof invalid: %v", name, loadErr))
			continue
		}
		plans++
		records += result.Total
		divergent += result.Counts[RepairArchiveDivergent]
	}
	sort.Strings(reasons)
	return plans, records, divergent, reasons, nil
}

func isPostCompletionRepairArtifactName(name string) bool {
	for _, suffix := range []string{".plan.json", ".review.json", ".supersession.json", ".pending.json", ".progress.jsonl", ".result.json", ".proof.json"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func (s *Store) verifyPostCompletionRepairRoot(entries []os.DirEntry) ([]string, map[string]bool, error) {
	reasons := []string{}
	indexed := map[string]bool{}
	root, _, exists, err := s.loadPostCompletionRepairRoot()
	if err != nil {
		return []string{"post-completion repair root invalid: " + err.Error()}, indexed, nil
	}
	anchor, anchorExists, anchorErr := s.loadPostCompletionRepairAnchor()
	if anchorErr != nil {
		return []string{"post-completion repair authority anchor invalid: " + anchorErr.Error()}, indexed, nil
	}
	if anchorErr := verifyPostCompletionRepairRootAnchor(root, exists, anchor, anchorExists); anchorErr != nil {
		return []string{anchorErr.Error()}, indexed, nil
	}
	quarantineReasons, quarantineErr := s.verifyPostCompletionRepairQuarantineInventory(root)
	if quarantineErr != nil {
		return []string{"post-completion repair quarantine inventory invalid: " + quarantineErr.Error()}, indexed, nil
	}
	reasons = append(reasons, quarantineReasons...)
	hasArtifacts := false
	for _, entry := range entries {
		if !entry.IsDir() && isPostCompletionRepairArtifactName(entry.Name()) {
			hasArtifacts = true
		}
	}
	if !exists {
		if hasArtifacts {
			reasons = append(reasons, "post-completion repair root missing while artifacts exist")
		}
		return reasons, indexed, nil
	}
	for _, artifact := range root.Artifacts {
		indexed[artifact.Path] = true
		path := filepath.Join(s.engine.postCompletionRepairRoot(), artifact.Path)
		body, err := homepkg.ReadRootedRegularFile(s.home, path, "post-completion repair indexed "+artifact.Kind+" artifact")
		if errors.Is(err, os.ErrNotExist) {
			reasons = append(reasons, "post-completion repair indexed artifact missing: "+artifact.Path)
			continue
		}
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("post-completion repair indexed artifact unreadable: %s: %v", artifact.Path, err))
			continue
		}
		digestBody := body
		if artifact.Kind == "progress" {
			digestBody, err = postCompletionRepairProgressHeaderBytes(body)
			if err != nil {
				reasons = append(reasons, fmt.Sprintf("post-completion repair indexed progress journal invalid: %s: %v", artifact.Path, err))
				continue
			}
		}
		if artifact.Kind == "supersession" {
			supersession, loadErr := decodeStrictJSON[PostCompletionRepairSupersession](body)
			if loadErr != nil {
				reasons = append(reasons, fmt.Sprintf("post-completion repair indexed supersession unreadable: %s: %v", artifact.Path, loadErr))
				continue
			}
			oldPlan, oldErr := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(supersession.PlanID))
			review, reviewErr := s.loadPostCompletionRepairReview(s.postCompletionRepairReviewPath(supersession.PlanID), oldPlan)
			successor, successorErr := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(supersession.SuccessorPlanID))
			if oldErr != nil || reviewErr != nil || successorErr != nil {
				reasons = append(reasons, fmt.Sprintf("post-completion repair indexed supersession dependencies invalid: %s: %v", artifact.Path, errors.Join(oldErr, reviewErr, successorErr)))
				continue
			}
			if loadErr := validatePostCompletionRepairSupersession(supersession, oldPlan, review, successor); loadErr != nil {
				reasons = append(reasons, fmt.Sprintf("post-completion repair indexed supersession invalid: %s: %v", artifact.Path, loadErr))
			} else if proof, present, proofErr := s.ReadPostCompletionRepairSupersessionProof(supersession.PlanID); proofErr != nil || !present {
				reasons = append(reasons, fmt.Sprintf("post-completion repair supersession work proof missing: %s: %v", artifact.Path, proofErr))
			} else if proofErr := s.validatePostCompletionRepairSupersessionProof(proof, supersession); proofErr != nil {
				reasons = append(reasons, fmt.Sprintf("post-completion repair supersession work proof invalid: %s: %v", artifact.Path, proofErr))
			}
		}
		if got := "sha256:" + recordHash(digestBody); got != artifact.SHA256 {
			reasons = append(reasons, "post-completion repair indexed artifact digest mismatch: "+artifact.Path)
			continue
		}
		if artifact.Kind == "progress" {
			plan, planErr := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(artifact.ID))
			if planErr != nil {
				reasons = append(reasons, fmt.Sprintf("post-completion repair indexed progress plan invalid: %s: %v", artifact.Path, planErr))
				continue
			}
			state, stateErr := s.loadPostCompletionRepairProgressCurrent(plan)
			if stateErr == nil && len(state.TornTail) > 0 {
				stateErr = fmt.Errorf("torn final transition awaits exact retry")
			}
			if stateErr == nil {
				stateErr = s.verifyPostCompletionRepairProgressPhysical(plan, state.Current)
			}
			if stateErr != nil {
				reasons = append(reasons, fmt.Sprintf("post-completion repair indexed progress history invalid: %s: %v", artifact.Path, stateErr))
			} else if !postCompletionRepairProgressAuthorizationHeadCurrent(artifact, state) {
				reasons = append(reasons, fmt.Sprintf("post-completion repair indexed progress authorization head is not anchored: %s", artifact.Path))
			}
		}
	}
	lifecycle := map[string]map[string]bool{}
	for _, artifact := range root.Artifacts {
		planID := postCompletionRepairPlanIDFromArtifactPath(artifact.Path)
		if planID == "" {
			continue
		}
		if lifecycle[planID] == nil {
			lifecycle[planID] = map[string]bool{}
		}
		lifecycle[planID][artifact.Kind] = true
	}
	for planID, kinds := range lifecycle {
		if kinds["supersession"] && (kinds["pending"] || kinds["progress"] || kinds["result"]) {
			reasons = append(reasons, "post-completion repair supersession conflicts with progressed lifecycle: "+planID)
		} else if kinds["proof"] && !kinds["supersession"] && !kinds["result"] {
			reasons = append(reasons, "post-completion repair proof lacks terminal owner: "+planID)
		} else if kinds["plan"] && !kinds["supersession"] && !kinds["pending"] && !kinds["progress"] && !kinds["result"] {
			reasons = append(reasons, "post-completion repair indexed plan/review has no pending or result: "+planID)
		}
	}
	for _, entry := range entries {
		if entry.IsDir() || !isPostCompletionRepairArtifactName(entry.Name()) {
			continue
		}
		if !indexed[entry.Name()] {
			reasons = append(reasons, "post-completion repair orphan artifact not indexed: "+entry.Name())
		}
	}
	sort.Strings(reasons)
	return reasons, indexed, nil
}

func (s *Store) verifyPostCompletionRepairQuarantineInventory(root PostCompletionRepairRoot) ([]string, error) {
	repairRoot := s.engine.postCompletionRepairRoot()
	quarantineRoot := filepath.Join(repairRoot, "quarantine")
	expected := map[string]bool{}
	for _, artifact := range root.Artifacts {
		if artifact.Kind != "progress" {
			continue
		}
		planID := postCompletionRepairPlanIDFromArtifactPath(artifact.Path)
		plan, err := s.loadPostCompletionRepairPlan(s.postCompletionRepairPlanPath(planID))
		if err != nil {
			continue
		}
		state, err := s.loadPostCompletionRepairProgressCurrent(plan)
		if err != nil {
			continue
		}
		for index, progress := range state.Current.Entries {
			rank := repairPhaseRank(progress.Phase)
			if rank < repairPhaseRank(repairPhaseRemoveReserved) || rank >= repairPhaseRank(repairPhaseRemoved) {
				continue
			}
			expected["quarantine"] = true
			expected[filepath.Join("quarantine", plan.ID)] = true
			claimPath := s.postCompletionRepairClaimPath(plan.ID, plan.Entries[index])
			if _, err := s.readPostCompletionRepairClaim(claimPath, plan.Entries[index].ID); err == nil {
				rel, relErr := filepath.Rel(repairRoot, claimPath)
				if relErr != nil {
					return nil, relErr
				}
				expected[rel] = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	quarantineExists, err := homepkg.RootedDirectoryExists(s.home, quarantineRoot, "post-completion repair quarantine directory")
	if err != nil {
		return nil, err
	}
	if !quarantineExists {
		return nil, nil
	}
	reasons := []string{}
	err = filepath.WalkDir(quarantineRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(repairRoot, path)
		if err != nil {
			return err
		}
		if rel == "quarantine" || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			exists, custodyErr := homepkg.RootedDirectoryExists(s.home, path, "post-completion repair quarantine directory")
			if custodyErr != nil {
				return custodyErr
			}
			if !exists {
				return fmt.Errorf("post-completion repair quarantine directory disappeared during verification: %s", rel)
			}
		}
		if !expected[rel] {
			reasons = append(reasons, "post-completion repair unowned quarantine entry: "+rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(reasons)
	return reasons, nil
}

func postCompletionRepairPlanIDFromArtifactPath(path string) string {
	for _, suffix := range []string{".plan.json", ".review.json", ".supersession.json", ".pending.json", ".progress.jsonl", ".result.json", ".proof.json"} {
		if strings.HasSuffix(path, suffix) {
			return strings.TrimSuffix(path, suffix)
		}
	}
	return ""
}

func (s *Store) verifyPostCompletionRepairProgressPhysical(plan PostCompletionRepairPlan, current PostCompletionRepairPending) error {
	presence, err := s.engine.buildSegmentPresence()
	if err != nil {
		return err
	}
	return s.verifyPostCompletionRepairProgressPhysicalWithPresence(plan, current, presence)
}

func (s *Store) verifyPostCompletionRepairProgressPhysicalWithPresence(plan PostCompletionRepairPlan, current PostCompletionRepairPending, presence map[string]segmentLocation) error {
	for index, entry := range plan.Entries {
		progress := current.Entries[index]
		rank := repairPhaseRank(progress.Phase)
		if entry.Disposition == RepairImportAbsent && rank < repairPhaseRank(repairPhaseAppendReserved) {
			if loc, exists := presence[entry.ID]; exists && loc.sha256 == entry.CanonicalSHA256 {
				return fmt.Errorf("post-completion repair progress history for %s is behind physical segment state", entry.ID)
			}
		}
		archiveRequired := (entry.Disposition == RepairImportAbsent && rank >= repairPhaseRank(repairPhaseAppendReserved)) || (entry.Disposition != RepairImportAbsent && rank >= repairPhaseRank(repairPhaseFrameVerified))
		if archiveRequired {
			body, err := s.readPostCompletionRepairArchive(entry.RawSHA256, entry.ID)
			if err != nil || recordHash(body) != entry.RawSHA256 {
				return errors.Join(fmt.Errorf("progress %s lacks exact archive evidence", entry.ID), err)
			}
		}
		if entry.Disposition == RepairImportAbsent && rank >= repairPhaseRank(repairPhaseAppended) {
			found, err := s.verifyPostCompletionRepairFrame(entry, progress.Segment, progress.SegmentOffset)
			if err != nil || !found {
				return errors.Join(fmt.Errorf("progress %s lacks exact appended frame evidence", entry.ID), err)
			}
		}
		_, looseErr := os.Stat(filepath.Join(s.rootPath(), entry.FileName))
		looseExists := looseErr == nil
		if looseErr != nil && !errors.Is(looseErr, os.ErrNotExist) {
			return looseErr
		}
		if !looseExists && rank < repairPhaseRank(repairPhaseRemoveReserved) {
			return fmt.Errorf("progress %s is missing removal-reservation history", entry.ID)
		}
		claim, claimErr := s.readPostCompletionRepairClaim(s.postCompletionRepairClaimPath(plan.ID, entry), entry.ID)
		claimExists := claimErr == nil
		if claimErr != nil && !errors.Is(claimErr, os.ErrNotExist) {
			return claimErr
		}
		if claimExists && recordHash(claim) != entry.RawSHA256 {
			return fmt.Errorf("progress %s quarantine bytes do not match the plan", entry.ID)
		}
		if rank == repairPhaseRank(repairPhaseRemoveReserved) && !looseExists && !claimExists {
			return fmt.Errorf("progress %s has removal reservation without loose or quarantine body", entry.ID)
		}
		if rank >= repairPhaseRank(repairPhaseClaimed) && looseExists {
			return fmt.Errorf("progress %s claims source pathname while loose source remains", entry.ID)
		}
		if progress.Phase == repairPhaseRemoved && claimExists {
			return fmt.Errorf("progress %s claims removal while quarantine remains", entry.ID)
		}
	}
	return nil
}
