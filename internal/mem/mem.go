// Package mem is P6: memory re-bound through the spine. It is the OWNED ledger — the
// authority — built on the patterns the external-systems audit converged on:
//   - MVCC-by-fold: an append-only, immutable claim log + a monotonic seq; "current" and
//     "as of T" are DERIVED by folding, never by mutating a record. Claims are never deleted.
//   - candidate-first: extracted facts land as untrusted Candidates and are adjudicated
//     reject | park | promote by deterministic code (the LLM only proposes).
//   - contradiction over delete: a promotion that changes a fact SUPERSEDES the prior claim
//     via an edge (the version chain); the old claim stays as history.
//   - provenance everywhere: every claim carries how it was derived + its evidence refs.
//
// A promotion emits a Transition (memory_delta) — memory is one projection of the spine.
// No engine here: the ledger alone answers exact, delta, and as-of queries.
// See docs/memory-architecture.md "Adopted Patterns".
package mem

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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"compound/internal/candidates"
	"compound/internal/eventstore"
	"compound/internal/policy"

	"compound/internal/envelope"
)

// Derivation tags how a claim/candidate was produced (provenance). ambiguous auto-surfaces.
var Derivations = []string{"deterministic", "extracted", "inferred", "ambiguous"}

// ClaimTypes is the claim taxonomy — what KIND of statement a claim is. It sharpens the topic axis
// (Position/Pattern group by Topic(), which prefers qualifiers["domain"] over the bare predicate).
// The spine-native convergence of the Gen-1 memory.ClaimType. Empty is allowed (untyped).
var ClaimTypes = []string{"preference", "decision", "architecture", "status", "constraint"}

// Verdicts are the adjudication outcomes (candidate-first).
var Verdicts = []string{"reject", "park", "promote", "no_promote"}

// Visibilities are the temperature tiers. retired is hidden from default recall but kept;
// volatile is promoted-but-clocked (useful now, expires unless reinforced) and never ambient.
var Visibilities = []string{"ambient", "contextual", "candidate", "volatile", "private", "operational", "retired"}

const (
	ClaimStatePromoted    = "promoted"
	ClaimStateNeedsReview = "needs_review"

	// OperationalDecisionAuthorityQualifier is reserved for accepted policy
	// decisions imported through the dedicated, locked decisions authority
	// boundary. Generic memory candidates may describe grants, freezes, or
	// revocations as evidence, but they cannot mint this stamp through
	// Adjudicate and therefore cannot become runtime admission authority.
	OperationalDecisionAuthorityQualifier = "_compound_operational_decision_authority"
	OperationalDecisionAuthorityValue     = "policy-import-v1"

	EdgeKindSupersedes    = "supersedes"
	EdgeKindReinforces    = "reinforces"
	EdgeKindNeedsReview   = "needs_review"
	EdgeKindReviewCleared = "needs_review_cleared"
	EdgeKindCorroboration = "corroboration"
)

// CandidateExpectation is the structured later fact that would make a parked seed matter.
type CandidateExpectation struct {
	Entity    string `json:"entity,omitempty"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	Detail    string `json:"detail,omitempty"`
}

// Candidate is an untrusted, proposed fact awaiting adjudication (lives in the inbox).
type Candidate struct {
	ID              string                `json:"id"`
	Entity          string                `json:"entity"`
	Predicate       string                `json:"predicate"`
	Object          string                `json:"object"`
	ObservationDate string                `json:"observation_date"` // temporal grounding (when the fact was true)
	Confidence      float64               `json:"confidence"`
	Derivation      string                `json:"derivation"`
	ProvenanceLane  string                `json:"provenance_lane,omitempty"` // provenance: producing model/lane when known
	ClaimType       string                `json:"claim_type,omitempty"`      // the claim taxonomy (mem.ClaimTypes); empty = untyped
	CanonClass      string                `json:"canon_class,omitempty"`     // kind-of-truth tier (mem.CanonClasses); empty = derive from ClaimType
	Qualifiers      map[string]string     `json:"qualifiers,omitempty"`      // e.g. {"domain": "languages"} → sharpens Topic()
	TopicRefs       []string              `json:"topic_refs,omitempty"`      // optional concept taxonomy refs; old rows omit this safely
	Tags            []string              `json:"tags,omitempty"`
	Salience        string                `json:"salience,omitempty"`
	Expectation     *CandidateExpectation `json:"expectation,omitempty"`
	ExpiresAt       string                `json:"expires_at,omitempty"`
	PromotionBasis  string                `json:"promotion_basis,omitempty"` // WHY it may promote (candidates.PromotionBases); empty = none
	SourceTraceRef  string                `json:"source_trace_ref"`          // provenance: where it came from
	EvidenceRefs    []string              `json:"evidence_refs,omitempty"`   // bounded source and adjudication evidence
	SnippetHash     string                `json:"snippet_hash"`              // bounded snippet, not the raw body
	Visibility      string                `json:"visibility"`
	State           string                `json:"state"` // proposed | parked | promoted | rejected
	Reason          string                `json:"reason,omitempty"`
	// AdjudicatedBy/AdjudicatedAt are stamped ONLY by the CLI's operator
	// ratification door (AdjudicateAsOperator, i.e. `compound memory
	// adjudicate`) — never by an automated caller (panel apply, decisions
	// import, harvest). A raw harvested "parked" candidate carries neither
	// field; once the operator has explicitly ruled on it (even a re-park —
	// "Keep aside"), both are set so the candidates-list projection (and the
	// console standing-law queue) can tell "seen and kept aside" from "never
	// looked at" and stop re-asking about the same candidate.
	AdjudicatedBy string `json:"adjudicated_by,omitempty"`
	AdjudicatedAt string `json:"adjudicated_at,omitempty"`
	// AdjudicationOperationKey binds a terminal candidate projection to the
	// exact frozen operation that produced it. Generic/operator adjudication
	// clears it; governed promotion-driver replay uses it only to distinguish
	// its own committed write from a concurrent terminal writer.
	AdjudicationOperationKey string                `json:"adjudication_operation_key,omitempty"`
	Annotations              []CandidateAnnotation `json:"annotations,omitempty"`
	CreatedAt                string                `json:"created_at"`
}

// Claim is a durable, IMMUTABLE assertion in the ledger. It is never mutated or deleted;
// supersession and validity are derived from the seq order + supersedes edges (MVCC-by-fold).
type CandidateAnnotation struct {
	ID               string                    `json:"id"`
	Kind             string                    `json:"kind"`
	Decision         string                    `json:"decision"`
	OriginalDecision string                    `json:"original_decision,omitempty"`
	RunID            string                    `json:"run_id,omitempty"`
	VerdictArtifact  string                    `json:"verdict_artifact,omitempty"`
	Rationale        string                    `json:"rationale,omitempty"`
	Confidence       float64                   `json:"confidence,omitempty"`
	EvidenceRefs     []string                  `json:"evidence_refs,omitempty"`
	LensFindings     []CandidateAnnotationLens `json:"lens_findings,omitempty"`
	CreatedAt        string                    `json:"created_at"`
}

type CandidateAnnotationLens struct {
	LensRef          string                       `json:"lens_ref"`
	Status           string                       `json:"status,omitempty"`
	Confidence       float64                      `json:"confidence,omitempty"`
	RouteEvidenceRef string                       `json:"route_evidence_ref,omitempty"`
	Findings         []CandidateAnnotationFinding `json:"findings,omitempty"`
}

type CandidateAnnotationFinding struct {
	Claim        string   `json:"claim,omitempty"`
	Severity     string   `json:"severity,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type Claim struct {
	ID              string            `json:"id"`
	Seq             int64             `json:"seq"` // monotonic version clock
	Entity          string            `json:"entity"`
	Predicate       string            `json:"predicate"`
	Object          string            `json:"object"`
	ValidFrom       string            `json:"valid_from"` // observation date (immutable)
	Confidence      float64           `json:"confidence"`
	Derivation      string            `json:"derivation"`                // provenance: how derived
	ProvenanceLane  string            `json:"provenance_lane,omitempty"` // provenance: producing model/lane when known
	ClaimType       string            `json:"claim_type,omitempty"`      // the claim taxonomy (mem.ClaimTypes)
	CanonClass      string            `json:"canon_class,omitempty"`     // kind-of-truth tier (mem.CanonClasses); empty = derive from ClaimType
	Qualifiers      map[string]string `json:"qualifiers,omitempty"`      // e.g. {"domain": "languages"} → Topic()
	TopicRefs       []string          `json:"topic_refs,omitempty"`      // optional concept taxonomy refs; additive for older rows
	PromotionBasis  string            `json:"promotion_basis,omitempty"` // WHY it was promoted (the recallable "why" of canon)
	EvidenceRefs    []string          `json:"evidence_refs"`             // provenance: source candidate/trace
	Supersedes      string            `json:"supersedes,omitempty"`
	Visibility      string            `json:"visibility"`
	ExpiresAt       string            `json:"expires_at,omitempty"`       // volatile-only claim clock ("useful now, expires unless reinforced"); empty on every other visibility
	CooledReason    string            `json:"cooled_reason,omitempty"`    // why a version was cooled (volatile_expired_unreinforced); NOT a Qualifier — that map is identity-bearing (Topic/dup-detection)
	State           string            `json:"state,omitempty"`            // derived: promoted | needs_review
	ReviewRefs      []string          `json:"review_refs,omitempty"`      // append-only review marker refs
	ConfidenceDelta float64           `json:"confidence_delta,omitempty"` // latest contradicting evidence delta
	CreatedAt       string            `json:"created_at"`
}

// Topic is the axis Position/Pattern group by: the claim's domain qualifier if set, else the
// predicate. So domain-tagged claims cluster by domain while plain ones stay predicate-grouped.
func (c Claim) Topic() string {
	return topicFromQualifiers(c.Predicate, c.Qualifiers)
}

// Edge is an append-only relation between claims. Its seq is when the relation took effect,
// so "current as of T" can fold supersession deterministically.
type Edge struct {
	Seq             int64    `json:"seq"`
	Kind            string   `json:"kind"` // supersedes | reinforces | needs_review | relation kinds
	From            string   `json:"from"`
	To              string   `json:"to"`
	Confidence      float64  `json:"confidence"`
	ConfidenceDelta float64  `json:"confidence_delta,omitempty"`
	EvidenceRefs    []string `json:"evidence_refs,omitempty"`
	CreatedAt       string   `json:"created_at"`
}

// Position is the CURRENT durable stance per fact slot. Topic remains the broad domain axis, while
// predicate and claim type keep independent facts in that domain from sharing a version chain. The
// active claim is the current (non-superseded) version; the chain is the FULL supersession history
// (MVCC-by-fold), so a Position carries old→new, not just the latest value.
type Position struct {
	SchemaVersion         string   `json:"schema_version"`
	ID                    string   `json:"id"` // readable entity::topic prefix plus a fact-slot hash
	Entity                string   `json:"entity"`
	Topic                 string   `json:"topic"` // mem's topic axis = domain qualifier, else predicate
	Predicate             string   `json:"predicate,omitempty"`
	ClaimType             string   `json:"claim_type,omitempty"`
	ActiveClaimID         string   `json:"active_claim_id"`
	ActiveObject          string   `json:"active_object"` // the current value (convenience)
	Status                string   `json:"status"`        // active | retired | contested
	SupportingClaimRefs   []string `json:"supporting_claim_refs,omitempty"`
	SupersededHistoryRefs []string `json:"superseded_history_refs,omitempty"`
	ClaimChain            []string `json:"claim_chain"` // all versions oldest→newest (the supersession history)
	ReviewRefs            []string `json:"review_refs,omitempty"`
	EvidenceRefs          []string `json:"evidence_refs,omitempty"`
	TopicRefs             []string `json:"topic_refs,omitempty"`
	FoldedAt              string   `json:"folded_at,omitempty"`
	AsOfSeq               int64    `json:"as_of_seq,omitempty"`
}

// Store is the memory ledger rooted at a COMPOUND_HOME.
type Store struct {
	home       string
	eventStore *eventstore.Store

	// entityLoader is a test seam for proving that cluster folds do not reload
	// the append-only entity registry once per claim. Production stores leave it
	// nil and read the registry from disk through loadEntities.
	entityLoader func() ([]Entity, error)

	// beforeLockObserver is a test seam called immediately before the advisory
	// lock attempt. Production stores leave it nil.
	beforeLockObserver func()

	// currentViewAfterClaimsRead is a test seam for pausing a CurrentView while
	// it still holds the shared memory lock. Production stores leave it nil.
	currentViewAfterClaimsRead func()

	// ledgerDoctorSnapshotter is a test seam for proving snapshot failures are
	// propagated instead of being misclassified as ledger mutation. Production
	// stores leave it nil and snapshot the owned ledger from disk.
	ledgerDoctorSnapshotter func() (ledgerDoctorSnapshot, error)

	// adjudicationCandidateWriter is a test seam for deterministic failure at
	// the final candidate-projection write. Production stores leave it nil and
	// persist through the candidates package's single owned write door.
	adjudicationCandidateWriter func(id string, payload any) error

	// pendingCandidateReplacer is the exact candidate compare-and-swap used by
	// bounded pending sweeps. Production stores leave it nil.
	pendingCandidateReplacer func(string, candidates.Record, any) (candidates.ReplaceCandidateResult, error)

	// candidatePointReader is a test seam for proving batch walks never fall
	// back to per-candidate point reads. Production stores leave it nil and
	// read through the candidates package's single rooted read door.
	candidatePointReader func(id string) (Candidate, error)
}

func Open(homePath string) (*Store, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	for _, d := range []string{candidates.InboxDir(candidates.Memory), "ledger"} {
		if err := os.MkdirAll(filepath.Join(homePath, d), 0o755); err != nil {
			return nil, err
		}
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return nil, err
	}
	if err := events.Ensure(); err != nil {
		return nil, err
	}
	return &Store{home: homePath, eventStore: events}, nil
}

// OpenReadOnly opens an existing memory ledger without creating runtime
// directories. Read-only authority proofs use it so observing an absent or
// partial home never changes that home.
func OpenReadOnly(homePath string) (*Store, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	ledger := filepath.Join(homePath, "ledger")
	info, err := os.Stat(ledger)
	if err != nil {
		return nil, fmt.Errorf("open memory ledger read-only: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("open memory ledger read-only: %s is not a directory", ledger)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return nil, err
	}
	return &Store{home: homePath, eventStore: events}, nil
}

// OpenScan opens the store for bounded walks and dry-run gates without
// scaffolding: unlike Open it creates no ledger/ or events/ directories,
// and unlike OpenReadOnly an ABSENT ledger is a legitimate empty view, not
// an error — a home that captured candidates but never promoted has no
// claims.jsonl yet, and plan passes and dry-run gates must still walk its
// inbox. Callers that must distinguish "no ledger" from "empty ledger"
// (authority proofs) stay on OpenReadOnly.
func OpenScan(homePath string) (*Store, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return nil, err
	}
	return &Store{home: homePath, eventStore: events}, nil
}

// HomePath exposes the runtime root to sibling authority owners that must
// coordinate one cross-object critical section. It is not an authority read;
// callers still use the owning package for ledger operations.
func (s *Store) HomePath() string {
	if s == nil {
		return ""
	}
	return s.home
}

func (s *Store) candDir() string {
	return filepath.Join(s.home, candidates.InboxDir(candidates.Memory))
}

func (s *Store) writeAdjudicatedCandidate(id string, payload any) error {
	if s.adjudicationCandidateWriter != nil {
		return s.adjudicationCandidateWriter(id, payload)
	}
	return candidates.WriteCandidate(s.home, candidates.Memory, id, payload)
}

func (s *Store) claimsLog() string { return filepath.Join(s.home, "ledger", "claims.jsonl") }
func (s *Store) edgesLog() string  { return filepath.Join(s.home, "ledger", "edges.jsonl") }
func (s *Store) seqFile() string   { return filepath.Join(s.home, "ledger", "seq") }

// --- capture (candidate-first) ---

// Capture writes proposed candidates to the inbox. The LLM-extraction step is upstream; this
// is the deterministic intake — nothing here is canon until adjudicated.
func (s *Store) Capture(cands []Candidate, now time.Time) ([]Candidate, error) {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Derivation == "" {
			c.Derivation = "extracted"
		}
		if !oneOf(c.Derivation, Derivations) {
			return out, fmt.Errorf("candidate %s: bad derivation %q", c.Entity, c.Derivation)
		}
		if c.ClaimType != "" && !oneOf(c.ClaimType, ClaimTypes) {
			return out, fmt.Errorf("candidate %s: bad claim_type %q (allowed: %v)", c.Entity, c.ClaimType, ClaimTypes)
		}
		if c.CanonClass != "" {
			if c.CanonClass == CanonClassUnclassified {
				return out, fmt.Errorf("candidate %s: canon_class %q is derived-only and never storable (allowed: %v)", c.Entity, c.CanonClass, CanonClasses)
			}
			if !oneOf(c.CanonClass, CanonClasses) {
				return out, fmt.Errorf("candidate %s: bad canon_class %q (allowed: %v)", c.Entity, c.CanonClass, CanonClasses)
			}
		}
		c.TopicRefs = normalizeStringRefs(c.TopicRefs)
		c.EvidenceRefs = uniqueStrings(c.EvidenceRefs)
		if c.Visibility == "" {
			c.Visibility = "operational"
		}
		if err := validateVolatileWrite("candidate "+c.Entity, c.Visibility, c.ExpiresAt, c.ClaimType, c.CanonClass); err != nil {
			return out, err
		}
		candidateID, err := captureCandidateID(c, now)
		if err != nil {
			return out, fmt.Errorf("candidate %s: derive id: %w", c.Entity, err)
		}
		c.ID = candidateID
		echo, err := s.reinforceIfEcho(c, now)
		if err != nil {
			return out, err
		}
		if echo {
			continue
		}
		c.State = "proposed"
		c.CreatedAt = rfc3339(now)
		// Decisions-down write seam: knowledge tiers + disposition (decay_park / auto_file / operator).
		c, _ = AdmitCandidateAt(c, now)
		created, err := s.publishCapturedCandidate(c)
		if err != nil {
			return out, err
		}
		if !created {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Store) publishCapturedCandidate(candidate Candidate) (bool, error) {
	return candidates.CreateCandidateIfAbsent(s.home, candidates.Memory, candidate.ID, &candidate)
}

// captureCandidateID distinguishes separate observations of the same fact when
// their provenance or other evidence-bearing fields differ. Lifecycle fields
// are excluded because Capture and adjudication own them after intake.
func captureCandidateID(c Candidate, now time.Time) (string, error) {
	c.ID = ""
	c.State = ""
	c.Reason = ""
	c.AdjudicatedBy = ""
	c.AdjudicatedAt = ""
	c.AdjudicationOperationKey = ""
	c.Annotations = nil
	c.CreatedAt = ""
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return idFor("cand_", string(payload), ts(now)), nil
}

func (s *Store) getCandidate(id string) (Candidate, error) {
	if s.candidatePointReader != nil {
		return s.candidatePointReader(id)
	}
	var c Candidate
	err := candidates.ReadCandidate(s.home, candidates.Memory, id, &c)
	if errors.Is(err, fs.ErrNotExist) {
		return Candidate{}, fmt.Errorf("not found: %s.json: %w", id, fs.ErrNotExist)
	}
	return c, err
}

// CandidatesByID returns the readable inbox candidates for the requested IDs.
// Missing or unreadable candidates are omitted so one damaged file cannot break
// lookups for its siblings.
func (s *Store) CandidatesByID(ids []string) (map[string]Candidate, error) {
	out := make(map[string]Candidate, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		candidate, err := s.getCandidate(id)
		if err != nil {
			continue
		}
		out[id] = candidate
	}
	return out, nil
}

func (s *Store) mutateCandidateLocked(candID string, mutate func(*Candidate) (bool, error)) (Candidate, bool, error) {
	c, err := s.getCandidate(candID)
	if err != nil {
		return Candidate{}, false, err
	}
	changed, err := mutate(&c)
	if err != nil || !changed {
		return c, changed, err
	}
	if err := candidates.WriteCandidate(s.home, candidates.Memory, c.ID, &c); err != nil {
		return Candidate{}, false, err
	}
	return c, true, nil
}

func (s *Store) mutateCandidate(candID string, mutate func(*Candidate) (bool, error)) (Candidate, bool, error) {
	var (
		candidate Candidate
		changed   bool
	)
	err := s.withLock(func() error {
		var err error
		candidate, changed, err = s.mutateCandidateLocked(candID, mutate)
		return err
	})
	return candidate, changed, err
}

func (s *Store) AnnotateCandidate(candID string, ann CandidateAnnotation, now time.Time) (Candidate, error) {
	if strings.TrimSpace(ann.ID) == "" {
		ann.ID = idFor("ann_", candID, ann.Kind, ann.Decision, ts(now))
	}
	if strings.TrimSpace(ann.Kind) == "" {
		ann.Kind = "panel_verdict"
	}
	if strings.TrimSpace(ann.Decision) == "" {
		ann.Decision = "annotate"
	}
	if strings.TrimSpace(ann.CreatedAt) == "" {
		ann.CreatedAt = rfc3339(now)
	}
	c, _, err := s.mutateCandidate(candID, func(c *Candidate) (bool, error) {
		if c.State == "promoted" {
			return false, fmt.Errorf("candidate %s already promoted", candID)
		}
		if !candidateHasAnnotation(c.Annotations, ann.ID) {
			c.Annotations = append(c.Annotations, ann)
		}
		c.State = "parked"
		if strings.TrimSpace(c.Reason) == "" {
			c.Reason = "annotated evidence; not promoted"
		}
		return true, nil
	})
	return c, err
}

func candidateHasAnnotation(annotations []CandidateAnnotation, id string) bool {
	for _, ann := range annotations {
		if ann.ID == id {
			return true
		}
	}
	return false
}

type candidateInboxEntry struct {
	Candidate Candidate
	RawKind   string
	Modified  time.Time
}

// candidateInboxSnapshot reads every canonical memory candidate from one
// descriptor-rooted page. Corrupt .json entries stay named and never become a
// candidate through filename fallback.
func (s *Store) candidateInboxSnapshot() ([]candidateInboxEntry, []*candidates.Corruption, error) {
	page, err := candidates.ReadPage(s.home, candidates.Memory, "", 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	out := make([]candidateInboxEntry, 0, len(page.Entries))
	var corruptions []*candidates.Corruption
	for _, entry := range page.Entries {
		if entry.Corruption != nil {
			corruptions = append(corruptions, entry.Corruption)
			continue
		}
		var c Candidate
		if err := json.Unmarshal(entry.Record.Body, &c); err != nil {
			corruptions = append(corruptions, &candidates.Corruption{EntryCursor: entry.EntryCursor, Code: candidates.CorruptionInvalidJSON, Detail: "decode memory candidate: " + err.Error()})
			continue
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(entry.Record.Body, &meta); err != nil {
			corruptions = append(corruptions, &candidates.Corruption{EntryCursor: entry.EntryCursor, Code: candidates.CorruptionInvalidJSON, Detail: "decode memory candidate kind: " + err.Error()})
			continue
		}
		out = append(out, candidateInboxEntry{Candidate: c, RawKind: strings.TrimSpace(meta.Kind), Modified: entry.Record.ModifiedAt()})
	}
	return out, corruptions, nil
}

// AllCandidates returns inbox candidates in deterministic id order. This is a
// decision/mutation seam, so named custody corruption fails the snapshot
// closed rather than returning a partial set as if it were complete.
func (s *Store) AllCandidates() ([]Candidate, error) {
	snapshot, corruptions, err := s.candidateInboxSnapshot()
	if err != nil {
		return nil, err
	}
	if len(corruptions) > 0 {
		errs := make([]error, 0, len(corruptions))
		for _, corruption := range corruptions {
			errs = append(errs, corruption)
		}
		return nil, fmt.Errorf("candidate inbox custody: %w", errors.Join(errs...))
	}
	out := make([]Candidate, 0, len(snapshot))
	for _, entry := range snapshot {
		out = append(out, entry.Candidate)
	}
	return out, nil
}

// --- adjudicate (code decides) ---

type AdjudicateResult struct {
	Verdict    string               `json:"verdict"`
	Candidate  Candidate            `json:"candidate"`
	Claim      *Claim               `json:"claim,omitempty"`
	Superseded string               `json:"superseded,omitempty"`
	Transition *envelope.Transition `json:"transition,omitempty"`
	Note       string               `json:"note,omitempty"`
}

// Adjudicate applies a verdict to a candidate. promote writes an immutable Claim (next seq,
// provenance), supersedes the prior current claim if the value changed (edge, not delete),
// and emits a Transition (memory_delta). A duplicate (same current value) is rejected.
//
// This is the UNSTAMPED door: automated callers (panel apply, decisions import,
// authority imports, ...) use this and leave AdjudicatedBy/AdjudicatedAt empty —
// an automated verdict is not the same event as a human ratifying the queue.
// The CLI's `compound memory adjudicate` verb uses AdjudicateAsOperator instead.
func (s *Store) Adjudicate(candID, verdict, workID string, now time.Time) (AdjudicateResult, error) {
	return s.adjudicate(candID, verdict, workID, now, false, "", "")
}

const (
	FrozenAdjudicationApplied           = "applied"
	FrozenAdjudicationAlreadyApplied    = "already_applied"
	FrozenAdjudicationStalePrecondition = "stale_precondition"
)

// FrozenCandidatePrecondition is the exact persisted candidate generation a
// planned adjudication was made from. State is echoed for receipt clarity and
// verified against the digest-pinned body before mutation.
type FrozenCandidatePrecondition struct {
	CandidateID string
	BodyDigest  string
	State       string
}

// FrozenAdjudication binds one target and an optional dedupe keeper to an
// idempotency key. The keeper is a companion precondition only; it is never
// mutated by this operation.
type FrozenAdjudication struct {
	OperationKey string
	Target       FrozenCandidatePrecondition
	Companion    *FrozenCandidatePrecondition
	CurrentClaim *FrozenClaimPrecondition

	outcome              string
	mismatchCandidateID  string
	preconditionMismatch string
	afterDigest          string
}

type FrozenClaimPrecondition struct {
	ID     string
	Absent bool
}

type FrozenAdjudicationResult struct {
	Outcome              string
	MismatchCandidateID  string
	PreconditionMismatch string
	BeforeDigest         string
	AfterDigest          string
	Adjudication         AdjudicateResult
}

// FrozenPromotionClaimPrecondition captures the exact current claim whose
// presence or absence makes a planned promotion deterministic.
func (s *Store) FrozenPromotionClaimPrecondition(candidate Candidate) (FrozenClaimPrecondition, error) {
	claim, found, err := s.currentClusterClaim(candidate, 0)
	if err != nil {
		return FrozenClaimPrecondition{}, err
	}
	if !found {
		return FrozenClaimPrecondition{Absent: true}, nil
	}
	return FrozenClaimPrecondition{ID: claim.ID}, nil
}

// AdjudicateFrozen executes one already-decided verdict without re-reading or
// re-deciding it. The memory lock is acquired before candidate custody; both
// target and companion digests remain pinned through every claim/transition
// write and the final candidate replacement.
func (s *Store) AdjudicateFrozen(plan FrozenAdjudication, verdict, workID string, now time.Time) (FrozenAdjudicationResult, error) {
	if strings.TrimSpace(plan.OperationKey) == "" {
		return FrozenAdjudicationResult{}, fmt.Errorf("frozen adjudication operation key required")
	}
	if strings.TrimSpace(plan.Target.CandidateID) == "" || strings.TrimSpace(plan.Target.BodyDigest) == "" {
		return FrozenAdjudicationResult{}, fmt.Errorf("frozen adjudication target id and digest required")
	}
	if verdict == candidates.Promote {
		if plan.CurrentClaim == nil || plan.CurrentClaim.Absent == (strings.TrimSpace(plan.CurrentClaim.ID) != "") {
			return FrozenAdjudicationResult{}, fmt.Errorf("frozen promotion current-claim precondition must name one claim or exact absence")
		}
	} else if plan.CurrentClaim != nil {
		return FrozenAdjudicationResult{}, fmt.Errorf("non-promotion frozen adjudication cannot carry a current-claim precondition")
	}
	adjudication, err := s.adjudicateWithFrozen(
		plan.Target.CandidateID,
		verdict,
		workID,
		now,
		false,
		"",
		"",
		&plan,
	)
	return FrozenAdjudicationResult{
		Outcome:              plan.outcome,
		MismatchCandidateID:  plan.mismatchCandidateID,
		PreconditionMismatch: plan.preconditionMismatch,
		BeforeDigest:         plan.Target.BodyDigest,
		AfterDigest:          plan.afterDigest,
		Adjudication:         adjudication,
	}, err
}

type frozenClaimPreconditionMismatch struct {
	observed string
}

func (e *frozenClaimPreconditionMismatch) Error() string {
	return "frozen current-claim precondition changed to " + e.observed
}

func frozenClaimReference(claim Claim, found bool) string {
	if !found {
		return "claim:absent"
	}
	return "claim:" + claim.ID
}

func frozenClaimMatches(expected FrozenClaimPrecondition, claim Claim, found bool) bool {
	if expected.Absent {
		return !found
	}
	return found && expected.ID == claim.ID
}

// adjudicatePendingProposed applies a non-promote batch verdict only when the
// candidate is still proposed under the memory mutation lock. A concurrent
// terminal writer wins; the pending sweep observes that as an idempotent skip
// instead of overwriting a newer verdict selected after the page walk.
type pendingMutationOutcome uint8

const (
	pendingMutationRetry pendingMutationOutcome = iota
	pendingMutationApplied
	pendingMutationSkipped
)

func (s *Store) adjudicatePendingProposed(expected candidates.Record, verdict, workID string, now time.Time) (AdjudicateResult, pendingMutationOutcome, error) {
	candID := expected.ID
	res := AdjudicateResult{Verdict: verdict}
	if verdict == candidates.Promote {
		return res, pendingMutationRetry, fmt.Errorf("pending adjudication cannot promote candidate %s", candID)
	}
	outcome := pendingMutationRetry
	err := s.withLock(func() error {
		var latest Candidate
		if err := json.Unmarshal(expected.Body, &latest); err != nil {
			return fmt.Errorf("decode expected candidate %s: %w", candID, err)
		}
		res.Candidate = latest
		if strings.TrimSpace(latest.State) != "proposed" {
			outcome = pendingMutationSkipped
			return nil
		}
		if err := validateAdjudicationIntent(latest, AdjudicationIntent{Verdict: verdict, WorkID: workID}); err != nil {
			return err
		}
		latest.State = nonPromoteState(verdict)
		replacer := s.pendingCandidateReplacer
		if replacer == nil {
			replacer = candidates.ReplaceCandidateIfCurrent
		}
		replaceResult, err := replacer(s.home, expected, &latest)
		if err != nil {
			return err
		}
		if !replaceResult.Replaced {
			var current Candidate
			if err := json.Unmarshal(replaceResult.Current.Body, &current); err != nil {
				return fmt.Errorf("decode current candidate %s after stale generation: %w", candID, err)
			}
			res.Candidate = current
			if strings.TrimSpace(current.State) != "proposed" {
				outcome = pendingMutationSkipped
			}
			return nil
		}
		res.Candidate = latest
		outcome = pendingMutationApplied
		return nil
	})
	return res, outcome, err
}

// AdjudicateAsOperator is the operator ratification door — the CLI's
// `compound memory adjudicate` verb. It stamps AdjudicatedBy/AdjudicatedAt
// provenance onto the resulting candidate so the candidates-list projection
// (and the console standing-law queue built on it) can tell a candidate the
// operator has already ruled on — even a re-park, "Keep aside" — from one
// still sitting untouched since ingestion. actor defaults to "operator" when
// blank and must carry operator provenance (the same admission rule
// RecordOperatorConfirmation applies): a rented executor identity must not
// mint operator ratification stamps through this door.
func (s *Store) AdjudicateAsOperator(candID, verdict, workID, actor string, now time.Time) (AdjudicateResult, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "operator"
	}
	if !policy.HasOperatorProvenance(actor) {
		return AdjudicateResult{}, fmt.Errorf("operator adjudication requires operator provenance, got %q", actor)
	}
	return s.adjudicate(candID, verdict, workID, now, false, "", actor)
}

// AdjudicatePendingReport summarises one batch adjudicate pass. The VerdictCounts
// map keys are the verbatim verdict strings ("reject", "no_promote", "park",
// "promote"); FirstID and LastID name the inboxes walked for repeatable log
// runs; Errors is the slice of per-candidate errors (adjudicate halts on a
// hard failure but tolerates per-candidate errors so a single corrupt file
// does not strand the whole batch).
type AdjudicatePendingReport struct {
	Walked        int            `json:"walked"`
	Decoded       int            `json:"decoded"`
	Corrupt       int            `json:"corrupt"`
	Scanned       int            `json:"scanned"`
	Adjudicated   int            `json:"adjudicated"`
	Skipped       int            `json:"skipped"`
	Deferred      int            `json:"deferred"`
	Failed        int            `json:"failed"`
	VerdictCounts map[string]int `json:"verdict_counts"`
	FirstID       string         `json:"first_id,omitempty"`
	LastID        string         `json:"last_id,omitempty"`
	// LastWalkedCursor is the LAST DIRECTORY ENTRY the pass walked, valid or
	// corrupt — the true continuation point. NextCursor derives from it (or
	// from the last completed decision on a write-cap stop): a cursor that only advanced on
	// successfully decoded candidates trapped a pass on a page ending in an
	// unreadable file, re-reading it forever.
	LastWalkedCursor candidates.EntryCursor `json:"last_walked_cursor,omitempty"`
	// AtEnd reports whether the walk reached the directory's final entry —
	// answered by the walker from the directory itself, never derived from
	// a pre-cursor total.
	AtEnd        bool `json:"at_end,omitempty"`
	Limit        int  `json:"limit,omitempty"`
	LimitReached bool `json:"limit_reached,omitempty"`
	// ScanLimit mirrors the pass's scan budget (0 = uncapped) and ScanCapHit
	// reports whether the pass stopped READING because of it. A cap on
	// writes alone is not an operational budget: `--limit 1` used to
	// enumerate, decode, and sort the whole 132k inbox to perform one write.
	ScanLimit  int  `json:"scan_limit,omitempty"`
	ScanCapHit bool `json:"scan_cap_hit,omitempty"`
	// NextCursor is the exact directory-entry cursor to resume after. The CLI
	// renders it as a versioned token for copy-safe --after continuation.
	// Set only when the pass stopped early (apply limit or scan cap), so an
	// absent cursor means "the inbox was walked to the end".
	NextCursor candidates.EntryCursor `json:"next_cursor,omitempty"`
	// ChangedIDs names every candidate this pass actually adjudicated, so a
	// pass that reports `adjudicated: N` can be reconciled file by file.
	ChangedIDs []string `json:"changed_ids,omitempty"`
	// CorruptIDs names inbox files that could not be decoded. A walker that
	// silently skipped unreadable JSON made the scan count a fiction.
	CorruptIDs []candidates.EntryCursor  `json:"corrupt_ids,omitempty"`
	Errors     []AdjudicatePendingError  `json:"errors,omitempty"`
	Actions    []AdjudicatePendingAction `json:"actions,omitempty"`
	// BasisDeferred counts proposed candidates the disposition pass LEFT
	// proposed because they carry a promotable basis: disposition must not
	// pre-empt the per-id promotion gate (design decision 3's corollary —
	// the batch neither promotes nor forecloses promotion). Fixed-verdict
	// passes (--verdict park) are explicit operator intent and still apply.
	BasisDeferred int `json:"basis_deferred,omitempty"`
}

// AdjudicatePendingError names one candidate the batch could not adjudicate.
// Reported inline so the operator can re-drive the failure without re-walking
// the whole inbox.
type AdjudicatePendingError struct {
	CandidateID string                 `json:"candidate_id,omitempty"`
	EntryCursor candidates.EntryCursor `json:"entry_cursor,omitempty"`
	Err         string                 `json:"error"`
}

// AdjudicatePendingAction is the exact per-candidate operation a dry-run
// planned or an apply pass attempted. It lets higher-level drivers freeze a
// reconstructible intent before mutation instead of journaling only counts.
type AdjudicatePendingAction struct {
	CandidateID    string                 `json:"candidate_id"`
	Verdict        string                 `json:"verdict"`
	EntryCursor    candidates.EntryCursor `json:"entry_cursor,omitempty"`
	ExpectedDigest string                 `json:"expected_digest,omitempty"`
	ExpectedState  string                 `json:"expected_state,omitempty"`
}

// AdjudicatePending walks the inbox in stable id order and applies a verdict
// per candidate. With VerdictPolicy=Disposition, each candidate's own
// admission-class disposition drives its verdict (auto no_promote/park for
// non-memory kinds, leave proposed-for-operator otherwise). With
// VerdictPolicy=Fixed, the caller-supplied verdict is applied uniformly.
// Promotion stays per-id: an explicit fixed "promote" verdict is REFUSED,
// forcing the operator to drive promotion through the single-id door.
//
// The function does NOT recurse into candidates the inbox write already
// closed (State != "proposed"); they are counted in Skipped. This is what
// makes the verb idempotent — running it twice produces the same report
// modulo the Errors slice.
func (s *Store) AdjudicatePending(opts AdjudicatePendingOptions, now time.Time) AdjudicatePendingReport {
	r := AdjudicatePendingReport{VerdictCounts: map[string]int{}, Limit: opts.Limit, ScanLimit: opts.ScanLimit}
	page, err := s.ListCandidatesLite(opts.AfterCursor, opts.ScanLimit)
	if err != nil {
		r.Failed++
		r.Errors = append(r.Errors, AdjudicatePendingError{CandidateID: "*", Err: err.Error()})
		return r
	}
	r.Walked = page.Walked
	r.LastWalkedCursor = page.LastCursor

	type pendingDecision struct {
		cursor      candidates.EntryCursor
		record      candidates.Record
		candidateID string
		state       string
		verdict     string
		outcome     string
		err         error
	}
	decisions := make([]pendingDecision, 0, len(page.Entries))
	for _, entry := range page.Entries {
		decision := pendingDecision{cursor: entry.Cursor}
		if entry.Corruption != nil {
			r.Corrupt++
			r.CorruptIDs = append(r.CorruptIDs, entry.Cursor)
			r.Errors = append(r.Errors, AdjudicatePendingError{EntryCursor: entry.Cursor, Err: entry.Corruption.Error()})
			decision.outcome = "corrupt"
			decisions = append(decisions, decision)
			continue
		}

		r.Decoded++
		r.Scanned++ // compatibility: scanned historically meant decoded candidates.
		lite := *entry.Candidate
		decision.record = *entry.Record
		decision.candidateID = lite.ID
		decision.state = lite.State
		if r.FirstID == "" {
			r.FirstID = lite.ID
		}
		r.LastID = lite.ID
		if strings.TrimSpace(lite.State) != "proposed" {
			decision.outcome = "skipped"
			decisions = append(decisions, decision)
			continue
		}
		if opts.ReservedIDs[lite.ID] {
			decision.outcome = "skipped"
			decisions = append(decisions, decision)
			continue
		}
		projected := Candidate{PromotionBasis: lite.PromotionBasis, Qualifiers: lite.Qualifiers}
		if opts.Verdict == "" && basisDefers(projected) {
			decision.outcome = "basis_deferred"
			decisions = append(decisions, decision)
			continue
		}
		decision.verdict = opts.Verdict
		if decision.verdict == "" {
			decision.verdict = dispositionVerdict(projected)
		}
		if decision.verdict == "promote" {
			decision.outcome = "failed"
			decision.err = fmt.Errorf("batch mode refuses --verdict promote; promote per-id with `compound memory adjudicate <id> --verdict promote`")
		} else {
			decision.outcome = "mutate"
		}
		decisions = append(decisions, decision)
	}

	safeCursor := opts.AfterCursor
	blocked := false
	plannedOrApplied := 0
	for _, decision := range decisions {
		if blocked {
			break
		}
		switch decision.outcome {
		case "corrupt":
			safeCursor = decision.cursor
		case "skipped":
			r.Skipped++
			safeCursor = decision.cursor
		case "basis_deferred":
			r.Deferred++
			r.BasisDeferred++
			safeCursor = decision.cursor
		case "failed":
			r.Failed++
			r.Errors = append(r.Errors, AdjudicatePendingError{CandidateID: decision.candidateID, EntryCursor: decision.cursor, Err: decision.err.Error()})
			blocked = true
		case "mutate":
			if opts.Limit > 0 && plannedOrApplied >= opts.Limit {
				r.Deferred++
				r.LimitReached = true
				blocked = true
				continue
			}
			action := AdjudicatePendingAction{
				CandidateID:    decision.candidateID,
				Verdict:        decision.verdict,
				EntryCursor:    decision.cursor,
				ExpectedDigest: decision.record.Digest,
				ExpectedState:  decision.state,
			}
			if opts.DryRun {
				r.Actions = append(r.Actions, action)
				plannedOrApplied++
				safeCursor = decision.cursor
				continue
			} else {
				_, outcome, err := s.adjudicatePendingProposed(decision.record, decision.verdict, opts.WorkID, now)
				if err != nil {
					r.Failed++
					r.Errors = append(r.Errors, AdjudicatePendingError{CandidateID: decision.candidateID, EntryCursor: decision.cursor, Err: err.Error()})
					blocked = true
					continue
				}
				switch outcome {
				case pendingMutationSkipped:
					r.Skipped++
					safeCursor = decision.cursor
					continue
				case pendingMutationRetry:
					r.Deferred++
					blocked = true
					continue
				case pendingMutationApplied:
					// Continue below and receipt the committed mutation.
				default:
					r.Failed++
					r.Errors = append(r.Errors, AdjudicatePendingError{CandidateID: decision.candidateID, EntryCursor: decision.cursor, Err: "candidate mutation returned an unknown outcome"})
					blocked = true
					continue
				}
				r.Actions = append(r.Actions, action)
			}
			r.VerdictCounts[decision.verdict]++
			r.Adjudicated++
			plannedOrApplied++
			r.ChangedIDs = append(r.ChangedIDs, decision.candidateID)
			safeCursor = decision.cursor
		}
	}

	r.AtEnd = page.AtEnd
	r.ScanCapHit = !page.AtEnd && opts.ScanLimit > 0
	if blocked {
		r.NextCursor = safeCursor
		return r
	}
	if !page.AtEnd {
		r.NextCursor = page.LastCursor
	}
	return r
}

// AdjudicatePendingOptions configures one AdjudicatePending pass. With Verdict
// empty, each candidate's own admission-class disposition drives its verdict.
// DryRun=true scans the inbox and reports the plan WITHOUT invoking Adjudicate
// per candidate — used by the --dry-run CLI flag so a 132k backlog is
// previewable in seconds, not the per-candidate lock cost.
type AdjudicatePendingOptions struct {
	Verdict string // empty -> per-candidate disposition; fixed verdict applied uniformly
	WorkID  string // optional --work binding for the batch receipts
	DryRun  bool   // scan + plan only; do not mutate
	Limit   int    // 0 = uncapped. >0 stops the pass after N candidates are ADJUDICATED; already-closed (skipped) candidates never count toward the cap, so the cap bounds writes, not scanning.
	// ScanLimit bounds how many inbox entries one pass READS (0 = uncapped).
	// It is the operational budget: without it, `--limit 1` enumerates,
	// decodes, and sorts the entire inbox to perform one write.
	ScanLimit int
	// AfterCursor resumes the walk after this exact directory entry (the
	// previous pass's NextCursor). Entries at or before it are not read, so a multi-pass
	// drain over a saturated inbox costs O(total) across all passes, not
	// O(total) per pass.
	AfterCursor candidates.EntryCursor
	// ReservedIDs excludes candidates already owned by an earlier phase in
	// the same planned driver pass. It prevents a failed promote/dedupe
	// operation from silently falling through to a different sweep verdict.
	ReservedIDs map[string]bool
}

// dispositionVerdict returns the verdict a candidate with no explicit
// --verdict flag would receive. source_metadata / annotated_evidence are
// not memory; they auto-reject (no_promote). A bare proposed candidate
// without a definite disposition parks itself so the operator can rule.
// basisDefers reports whether a candidate carries a promotable promotion
// basis that disposition must leave alone. explicit_confirmation is the
// exception: it is self-attested and only the governed confirmation door
// (AdjudicateExplicitConfirmation) can make it real, so disposition parks
// it like any other operator-material candidate.
func basisDefers(c Candidate) bool {
	basis := strings.TrimSpace(c.PromotionBasis)
	return candidates.ValidBasis(basis) && basis != candidates.BasisExplicitConfirmation
}

func dispositionVerdict(c Candidate) string {
	switch strings.TrimSpace(c.Qualifiers["source"]) {
	case "knowledge_import":
		return "no_promote"
	case "knowledge_extraction":
		return "no_promote"
	default:
		return "park"
	}
}

// AdjudicateExplicitConfirmation is the only ordinary-memory door that may
// attach an explicit_confirmation basis to an already-parked candidate. The
// caller must cite a content-addressed EvidenceItem bound to this candidate
// and attributed to an operator/human actor; model output and ordinary
// Adjudicate callers cannot mint this basis.
func (s *Store) AdjudicateExplicitConfirmation(candID, confirmationEvidenceID, workID string, now time.Time) (AdjudicateResult, error) {
	confirmationEvidenceID = strings.TrimSpace(confirmationEvidenceID)
	if confirmationEvidenceID == "" {
		return AdjudicateResult{}, fmt.Errorf("explicit confirmation evidence ref required")
	}
	return s.adjudicate(candID, "promote", workID, now, false, confirmationEvidenceID, "")
}

// AdjudicateOperationalDecision is the dedicated claim-append door used by
// the policy decision importer. It stamps promoted decision claims as
// operational authority after the importer has parsed operator-owned policy
// input under the shared autonomy-admission lock. Callers with ordinary
// extracted/inferred memory must use Adjudicate instead.
func (s *Store) AdjudicateOperationalDecision(candID, verdict, workID string, now time.Time) (AdjudicateResult, error) {
	return s.adjudicate(candID, verdict, workID, now, true, "", "")
}

func (s *Store) adjudicate(candID, verdict, workID string, now time.Time, operationalDecision bool, explicitConfirmationRef string, actor string) (AdjudicateResult, error) {
	return s.adjudicateWithFrozen(candID, verdict, workID, now, operationalDecision, explicitConfirmationRef, actor, nil)
}

func (s *Store) adjudicateWithFrozen(candID, verdict, workID string, now time.Time, operationalDecision bool, explicitConfirmationRef string, actor string, frozen *FrozenAdjudication) (AdjudicateResult, error) {
	res := AdjudicateResult{Verdict: verdict}
	intent := AdjudicationIntent{
		Verdict:                 verdict,
		WorkID:                  workID,
		OperationalDecision:     operationalDecision,
		ExplicitConfirmationRef: explicitConfirmationRef,
		Actor:                   actor,
	}
	var c Candidate
	if frozen == nil {
		var err error
		c, err = s.getCandidate(candID)
		if err != nil {
			return res, err
		}
		res.Candidate = c
		if err := validateAdjudicationIntent(c, intent); err != nil {
			return res, err
		}
	}

	preconditions := func() []candidates.DigestPrecondition {
		values := []candidates.DigestPrecondition{{ID: frozen.Target.CandidateID, Digest: frozen.Target.BodyDigest}}
		if frozen.Companion != nil {
			values = append(values, candidates.DigestPrecondition{ID: frozen.Companion.CandidateID, Digest: frozen.Companion.BodyDigest})
		}
		return values
	}
	handleMiss := func(result candidates.ConditionalMutationResult) error {
		frozen.mismatchCandidateID = result.MismatchID
		frozen.outcome = FrozenAdjudicationStalePrecondition
		frozen.afterDigest = result.Target.Digest
		if result.MismatchID != frozen.Target.CandidateID || len(result.Target.Body) == 0 {
			return nil
		}
		var current Candidate
		if err := json.Unmarshal(result.Target.Body, &current); err != nil {
			return fmt.Errorf("decode current frozen candidate %s: %w", candID, err)
		}
		res.Candidate = current
		if current.AdjudicationOperationKey != frozen.OperationKey {
			return nil
		}
		frozen.outcome = FrozenAdjudicationAlreadyApplied
		frozen.mismatchCandidateID = ""
		frozen.preconditionMismatch = ""
		if verdict == candidates.Promote {
			recovered, err := s.frozenAppliedPromotionResult(current, workID, now)
			if err != nil {
				return err
			}
			res = recovered
		}
		return nil
	}

	if verdict != candidates.Promote {
		if frozen != nil {
			err := s.withLock(func() error {
				result, err := candidates.MutateCandidateIfDigests(s.home, candidates.Memory, candID, preconditions(), func(record candidates.Record) (any, error) {
					var latest Candidate
					if err := json.Unmarshal(record.Body, &latest); err != nil {
						return nil, fmt.Errorf("decode frozen candidate %s: %w", candID, err)
					}
					if latest.State != frozen.Target.State {
						return nil, fmt.Errorf("frozen candidate %s state %q does not match planned %q", candID, latest.State, frozen.Target.State)
					}
					if err := validateAdjudicationIntent(latest, intent); err != nil {
						return nil, err
					}
					if latest.State == "promoted" {
						return nil, fmt.Errorf("candidate %s already promoted", candID)
					}
					latest.State = nonPromoteState(verdict)
					latest.AdjudicationOperationKey = frozen.OperationKey
					res.Candidate = latest
					return &latest, nil
				})
				if err != nil {
					return err
				}
				if !result.Applied {
					if err := handleMiss(result); err != nil {
						return err
					}
					return nil
				}
				frozen.outcome = FrozenAdjudicationApplied
				frozen.afterDigest = result.Target.Digest
				return nil
			})
			return res, err
		}

		latest, _, lockErr := s.mutateCandidate(candID, func(latest *Candidate) (bool, error) {
			if latest.State == "promoted" {
				return false, fmt.Errorf("candidate %s already promoted", candID)
			}
			latest.State = nonPromoteState(verdict)
			latest.AdjudicationOperationKey = ""
			if actor != "" {
				latest.AdjudicatedBy = actor
				latest.AdjudicatedAt = rfc3339(now)
			}
			return true, nil
		})
		if lockErr != nil {
			return res, lockErr
		}
		res.Candidate = latest
		return res, nil
	}

	// Promotion holds the memory lock across claim/edge/transition writes.
	// Frozen execution additionally nests candidate custody inside it, pinning
	// producer-visible bytes until the terminal projection is published.
	var (
		claim      Claim
		prior      Claim
		transition envelope.Transition
		hasPrior   bool
		dup        bool
	)
	applyPromotion := func(latest Candidate, writeCandidate func(Candidate) error) error {
		c = latest
		if err := validateAdjudicationIntent(c, intent); err != nil {
			return err
		}
		if c.State == "promoted" {
			return fmt.Errorf("candidate %s already promoted", candID)
		}
		if c.Qualifiers != nil && strings.TrimSpace(c.Qualifiers[OperationalDecisionAuthorityQualifier]) != "" && !operationalDecision {
			return fmt.Errorf("candidate %s uses reserved operational decision authority qualifier; import it through the policy decisions authority", candID)
		}
		if !operationalDecision && isOperationalDecisionKind(c.Qualifiers["kind"]) {
			return fmt.Errorf("candidate %s uses reserved operational decision kind %q; import it through the policy decisions authority", candID, c.Qualifiers["kind"])
		}
		if operationalDecision {
			if c.ClaimType != "decision" {
				return fmt.Errorf("candidate %s cannot receive operational decision authority: claim_type must be decision", candID)
			}
			c.Qualifiers = cloneStringMap(c.Qualifiers)
			c.Qualifiers[OperationalDecisionAuthorityQualifier] = OperationalDecisionAuthorityValue
		}
		if explicitConfirmationRef != "" {
			if c.State != "parked" {
				return fmt.Errorf("explicit confirmation requires parked candidate %s, got state %q", candID, c.State)
			}
			evidence, ok, err := s.GetEvidence(explicitConfirmationRef)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("explicit confirmation evidence %q not found", explicitConfirmationRef)
			}
			if err := validateEvidenceContentAddress(evidence); err != nil {
				return fmt.Errorf("explicit confirmation: %w", err)
			}
			if evidence.SourceKind != OperatorConfirmationSourceKind {
				return fmt.Errorf("explicit confirmation evidence %q has source_kind %q, want %s", explicitConfirmationRef, evidence.SourceKind, OperatorConfirmationSourceKind)
			}
			if strings.TrimSpace(evidence.SourceID) != strings.TrimSpace(candID) {
				return fmt.Errorf("explicit confirmation evidence %q targets %q, want candidate %q", explicitConfirmationRef, evidence.SourceID, candID)
			}
			if !policy.HasOperatorProvenance(evidence.Actor) {
				return fmt.Errorf("explicit confirmation evidence %q requires operator provenance, got %q", explicitConfirmationRef, evidence.Actor)
			}
			c.PromotionBasis = candidates.BasisExplicitConfirmation
			c.EvidenceRefs = uniqueStrings(append(c.EvidenceRefs, evidence.ID))
		} else if c.PromotionBasis == candidates.BasisExplicitConfirmation {
			return fmt.Errorf("candidate %s explicit confirmation requires governed confirmation evidence", candID)
		}
		if !candidates.ValidBasis(c.PromotionBasis) {
			basis, err := s.corroborationPromotionBasis(c.ID)
			if err != nil {
				return err
			}
			c.PromotionBasis = basis
		}
		if !candidates.ValidBasis(c.PromotionBasis) {
			return fmt.Errorf("candidate %s cannot promote without valid promotion_basis (got %q; allowed: %v)", candID, c.PromotionBasis, candidates.PromotionBases)
		}
		// Defense-in-depth for the single claim-append door: a clockless or
		// doctrine-class volatile candidate that predates the Capture guard (or was
		// hand-written) must still never become a ledger claim.
		if err := validateVolatileWrite("candidate "+c.ID, c.Visibility, c.ExpiresAt, c.ClaimType, c.CanonClass); err != nil {
			return err
		}
		if !operationalDecision && c.ClaimType == "decision" {
			protected, err := s.protectedOperationalDecisionInSlot(c)
			if err != nil {
				return err
			}
			if protected.ID != "" {
				return fmt.Errorf("candidate %s targets reserved operational decision slot held by %s", c.ID, protected.ID)
			}
		}
		claims, err := s.loadClaimsRaw()
		if err != nil {
			return err
		}
		for _, existing := range claims {
			if !oneOf(c.ID, existing.EvidenceRefs) || !isDuplicatePromotion(c, existing, true) {
				continue
			}
			if claim.ID != "" && claim.ID != existing.ID {
				return fmt.Errorf("candidate %s has multiple promotion claims (%s, %s)", c.ID, claim.ID, existing.ID)
			}
			claim = existing
		}
		if claim.ID != "" {
			// A prior attempt may have appended the immutable claim before a
			// later Transition or candidate projection write failed. Recover by
			// candidate provenance across the full claim history: a newer claim
			// may already have superseded this one before the retry arrives.
			var transitionPrior Claim
			transitionHasPrior := false
			if claim.Supersedes != "" {
				for _, existing := range claims {
					if existing.ID == claim.Supersedes {
						transitionPrior = existing
						transitionHasPrior = true
						break
					}
				}
				if !transitionHasPrior {
					return fmt.Errorf("candidate %s recovery missing superseded claim %s", c.ID, claim.Supersedes)
				}
				edgeExists, err := s.hasEdge(EdgeKindSupersedes, claim.ID, transitionPrior.ID)
				if err != nil {
					return err
				}
				if !edgeExists {
					edge := supersedesEdge(claim, transitionPrior, c, claim.Seq, now)
					if err := appendJSONL(s.edgesLog(), &edge); err != nil {
						return err
					}
				}
			}
			expectedTransition := s.memoryTransition(claim, transitionPrior, transitionHasPrior, workID, now)
			existingTransition, transitionExists, err := s.persistedPromotionTransition(expectedTransition)
			if err != nil {
				return err
			}
			if transitionExists {
				transition = existingTransition
			} else {
				transition = expectedTransition
				if err := transition.Validate(); err != nil {
					return fmt.Errorf("emitted transition invalid: %w", err)
				}
				if err := s.writePromotionTransition(&transition); err != nil {
					return err
				}
			}
			c.State = "promoted"
			if actor != "" {
				c.AdjudicatedBy = actor
				c.AdjudicatedAt = rfc3339(now)
			}
			if err := writeCandidate(c); err != nil {
				return err
			}
			prior = transitionPrior
			hasPrior = transitionHasPrior
			return nil
		}
		p, hp, err := s.currentClusterClaim(c, 0)
		if err != nil {
			return err
		}
		prior, hasPrior = p, hp
		if isDuplicatePromotion(c, prior, hasPrior) {
			dup = true
			c.State = "rejected"
			c.Reason = "duplicate of current claim " + prior.ID
			if actor != "" {
				c.AdjudicatedBy = actor
				c.AdjudicatedAt = rfc3339(now)
			}
			return writeCandidate(c)
		}
		seq, err := s.nextSeq()
		if err != nil {
			return err
		}
		claim = promotedClaim(c, prior, hasPrior, seq, now)
		if err := appendJSONL(s.claimsLog(), &claim); err != nil {
			return err
		}
		if hasPrior {
			edge := supersedesEdge(claim, prior, c, seq, now)
			if err := appendJSONL(s.edgesLog(), &edge); err != nil {
				return err
			}
		}
		transition = s.memoryTransition(claim, prior, hasPrior, workID, now)
		if err := transition.Validate(); err != nil {
			return fmt.Errorf("emitted transition invalid: %w", err)
		}
		if err := s.writePromotionTransition(&transition); err != nil {
			return err
		}
		c.State = "promoted"
		if actor != "" {
			c.AdjudicatedBy = actor
			c.AdjudicatedAt = rfc3339(now)
		}
		return writeCandidate(c)
	}

	lockErr := s.withLock(func() error {
		if frozen == nil {
			latest, err := s.getCandidate(candID)
			if err != nil {
				return err
			}
			return applyPromotion(latest, func(updated Candidate) error {
				updated.AdjudicationOperationKey = ""
				c = updated
				return s.writeAdjudicatedCandidate(updated.ID, &updated)
			})
		}
		result, err := candidates.MutateCandidateIfDigests(s.home, candidates.Memory, candID, preconditions(), func(record candidates.Record) (any, error) {
			var latest Candidate
			if err := json.Unmarshal(record.Body, &latest); err != nil {
				return nil, fmt.Errorf("decode frozen candidate %s: %w", candID, err)
			}
			if latest.State != frozen.Target.State {
				return nil, fmt.Errorf("frozen candidate %s state %q does not match planned %q", candID, latest.State, frozen.Target.State)
			}
			currentClaim, found, err := s.currentClusterClaim(latest, 0)
			if err != nil {
				return nil, err
			}
			if !frozenClaimMatches(*frozen.CurrentClaim, currentClaim, found) {
				res.Candidate = latest
				return nil, &frozenClaimPreconditionMismatch{observed: frozenClaimReference(currentClaim, found)}
			}
			var payload Candidate
			applyErr := applyPromotion(latest, func(updated Candidate) error {
				updated.AdjudicationOperationKey = frozen.OperationKey
				payload = updated
				c = updated
				return nil
			})
			return &payload, applyErr
		})
		if err != nil {
			var mismatch *frozenClaimPreconditionMismatch
			if errors.As(err, &mismatch) {
				frozen.outcome = FrozenAdjudicationStalePrecondition
				frozen.preconditionMismatch = mismatch.observed
				frozen.afterDigest = frozen.Target.BodyDigest
				return nil
			}
			return err
		}
		if !result.Applied {
			if err := handleMiss(result); err != nil {
				return err
			}
			return nil
		}
		frozen.outcome = FrozenAdjudicationApplied
		frozen.afterDigest = result.Target.Digest
		return nil
	})
	if lockErr != nil {
		return res, lockErr
	}
	if frozen != nil && frozen.outcome != FrozenAdjudicationApplied {
		return res, nil
	}
	if dup {
		res.Verdict, res.Candidate, res.Note = "reject", c, "already current: "+prior.ID
		return res, nil
	}
	if hasPrior {
		res.Superseded = prior.ID
	}
	res.Candidate, res.Claim, res.Transition = c, &claim, &transition
	return res, nil
}

func (s *Store) frozenAppliedPromotionResult(candidate Candidate, workID string, now time.Time) (AdjudicateResult, error) {
	result := AdjudicateResult{Verdict: candidates.Promote, Candidate: candidate}
	if candidate.State == "rejected" {
		const duplicatePrefix = "duplicate of current claim "
		claimID := strings.TrimSpace(strings.TrimPrefix(candidate.Reason, duplicatePrefix))
		if !strings.HasPrefix(candidate.Reason, duplicatePrefix) || claimID == "" {
			return result, fmt.Errorf("frozen operation key marks rejected candidate %s without duplicate provenance", candidate.ID)
		}
		result.Verdict = candidates.Reject
		result.Note = "already current: " + claimID
		return result, nil
	}
	if candidate.State != "promoted" {
		return result, fmt.Errorf("frozen promotion operation key marks candidate %s in state %q", candidate.ID, candidate.State)
	}

	claims, err := s.loadClaimsRaw()
	if err != nil {
		return result, err
	}
	var promoted Claim
	for _, existing := range claims {
		if !oneOf(candidate.ID, existing.EvidenceRefs) || !isDuplicatePromotion(candidate, existing, true) {
			continue
		}
		if promoted.ID != "" && promoted.ID != existing.ID {
			return result, fmt.Errorf("candidate %s has multiple promotion claims (%s, %s)", candidate.ID, promoted.ID, existing.ID)
		}
		promoted = existing
	}
	if promoted.ID == "" {
		return result, fmt.Errorf("candidate %s frozen promotion projection has no durable claim", candidate.ID)
	}

	var prior Claim
	hasPrior := promoted.Supersedes != ""
	if hasPrior {
		for _, existing := range claims {
			if existing.ID == promoted.Supersedes {
				prior = existing
				break
			}
		}
		if prior.ID == "" {
			return result, fmt.Errorf("candidate %s frozen promotion missing superseded claim %s", candidate.ID, promoted.Supersedes)
		}
	}
	expectedTransition := s.memoryTransition(promoted, prior, hasPrior, workID, now)
	transition, exists, err := s.persistedPromotionTransition(expectedTransition)
	if err != nil {
		return result, err
	}
	if !exists {
		return result, fmt.Errorf("candidate %s frozen promotion has no durable transition %s", candidate.ID, expectedTransition.ID)
	}
	if hasPrior {
		result.Superseded = prior.ID
	}
	result.Claim = &promoted
	result.Transition = &transition
	return result, nil
}

// HasOperationalDecisionAuthority reports whether qualifiers carry the exact
// reserved stamp minted by AdjudicateOperationalDecision. A source string or
// a grant-shaped set of ordinary qualifiers is not authority.
func HasOperationalDecisionAuthority(qualifiers map[string]string) bool {
	return qualifiers != nil && qualifiers[OperationalDecisionAuthorityQualifier] == OperationalDecisionAuthorityValue
}

func isOperationalDecisionKind(kind string) bool {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case "grant", "freeze", "ruling":
		return true
	default:
		return false
	}
}

// protectedOperationalDecisionClaim includes the reserved-stamp format and
// the narrow legacy format written by `compound decisions record` before the
// stamp existed. The legacy branch keeps already-recorded operator grants and
// freezes live across the migration; generic promotion can no longer create
// this shape because operational kinds are reserved above.
func protectedOperationalDecisionClaim(claim Claim) bool {
	if claim.ClaimType != "decision" {
		return false
	}
	q := claim.Qualifiers
	if q == nil || !policy.HasOperatorProvenance(q["reviewed_by"]) || !isOperationalDecisionKind(q["kind"]) {
		return false
	}
	if HasOperationalDecisionAuthority(q) {
		return true
	}
	return q["source"] == "decisions-repo" && q["lifecycle_status"] == "accepted"
}

// HasProtectedOperationalDecisionAuthority recognizes both the current
// reserved-stamp format and the narrow pre-stamp operator record retained for
// migration. Non-operator legacy/stamped claims are never authority.
func HasProtectedOperationalDecisionAuthority(claim Claim) bool {
	return protectedOperationalDecisionClaim(claim)
}

func (s *Store) protectedOperationalDecisionInSlot(candidate Candidate) (Claim, error) {
	want, err := s.candidateClusterKey(candidate)
	if err != nil {
		return Claim{}, err
	}
	claims, err := s.loadClaimsRaw()
	if err != nil {
		return Claim{}, err
	}
	var latest Claim
	for _, claim := range claims {
		if !protectedOperationalDecisionClaim(claim) {
			continue
		}
		got, err := s.claimClusterKey(claim)
		if err != nil {
			return Claim{}, err
		}
		if sameClusterKey(got, want) && claim.Seq > latest.Seq {
			latest = claim
		}
	}
	return latest, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
}

// The promotion decision is pure: nonPromoteState / isDuplicatePromotion /
// promotedClaim / supersedesEdge compute the verdict's effect from a candidate
// and the prior current claim, with no I/O. Adjudicate is the thin apply: it
// reads the candidate + prior, allocates the seq under the lock, then writes what
// these decide. The split lets the decision be table-tested without a
// COMPOUND_HOME (see TestPromotionDecisionIsPure).

// nonPromoteState maps a non-promote verdict to the candidate's resulting state.
func nonPromoteState(verdict string) string {
	return map[string]string{"reject": "rejected", "park": "parked", "no_promote": "no_promote"}[verdict]
}

func (s *Store) corroborationPromotionBasis(candidateID string) (string, error) {
	marks, err := s.candidateReviewMarksAsOf(0)
	if err != nil {
		return "", err
	}
	if mark, ok := marks[candidateID]; ok && len(mark.Refs) > 0 {
		return candidates.BasisRuntimeProof, nil
	}
	return "", nil
}

// isDuplicatePromotion reports whether promoting c would only restate the current
// claim (same object and qualifiers), so no new version is needed.
func isDuplicatePromotion(c Candidate, prior Claim, hasPrior bool) bool {
	if !hasPrior {
		return false
	}
	if c.Entity != "" || c.Predicate != "" || c.ClaimType != "" {
		candidateKey := ClaimSlotKey(c.Entity, c.Predicate, c.ClaimType, c.Qualifiers, "")
		priorKey := ClaimSlotKey(prior.Entity, prior.Predicate, prior.ClaimType, prior.Qualifiers, prior.ID)
		if !sameClusterKey(candidateKey, priorKey) {
			return false
		}
	}
	return SameClaimObject(prior.Object, c.Object) && sameQualifiers(prior.Qualifiers, c.Qualifiers)
}

func sameQualifiers(a, b map[string]string) bool { return SameClaimQualifiers(a, b) }

// promotedClaim builds the durable Claim a promotion appends at the allocated
// seq: the candidate's value + taxonomy, superseding the prior claim when one
// exists.
func promotedClaim(c Candidate, prior Claim, hasPrior bool, seq int64, now time.Time) Claim {
	claim := Claim{
		ID:             idFor("claim_", c.Entity, c.Predicate, c.Object, strconv.FormatInt(seq, 10)),
		Seq:            seq,
		Entity:         c.Entity,
		Predicate:      c.Predicate,
		Object:         c.Object,
		ValidFrom:      orDef(c.ObservationDate, rfc3339(now)),
		Confidence:     c.Confidence,
		Derivation:     c.Derivation,
		ProvenanceLane: c.ProvenanceLane,
		ClaimType:      c.ClaimType,  // carry the taxonomy + topic qualifiers onto the durable claim
		CanonClass:     c.CanonClass, // carry explicit kind-of-truth; empty rows derive via EffectiveCanonClass
		Qualifiers:     c.Qualifiers,
		TopicRefs:      normalizeStringRefs(c.TopicRefs),
		PromotionBasis: c.PromotionBasis, // carry the "why" of promotion onto the durable claim
		EvidenceRefs:   uniqueStrings(append([]string{c.ID, c.SourceTraceRef}, c.EvidenceRefs...)),
		Visibility:     orDef(c.Visibility, "operational"),
		CreatedAt:      rfc3339(now),
	}
	// The clock crosses onto the durable claim ONLY for volatile: a decay-parked
	// candidate's ExpiresAt is candidate-lifecycle state and must not leak onto
	// claims of any other visibility.
	if isVolatileVisibility(claim.Visibility) {
		claim.ExpiresAt = strings.TrimSpace(c.ExpiresAt)
	}
	if hasPrior {
		claim.Supersedes = prior.ID
	}
	return claim
}

// supersedesEdge builds the supersedes edge from a promoted claim to the prior.
func supersedesEdge(claim, prior Claim, c Candidate, seq int64, now time.Time) Edge {
	return Edge{Seq: seq, Kind: EdgeKindSupersedes, From: claim.ID, To: prior.ID, Confidence: c.Confidence, CreatedAt: rfc3339(now)}
}

func reinforcesEdge(from string, prior Claim, c Candidate, seq int64, now time.Time) Edge {
	return Edge{Seq: seq, Kind: EdgeKindReinforces, From: from, To: prior.ID, Confidence: c.Confidence, CreatedAt: rfc3339(now)}
}

func (s *Store) reinforceIfEcho(c Candidate, now time.Time) (bool, error) {
	var reinforced bool
	err := s.withLock(func() error {
		entities, err := s.Entities()
		if err != nil {
			return err
		}
		resolver := newEntityResolver(entities)
		prior, hasPrior, err := s.currentClusterClaimWithResolver(c, 0, resolver)
		if err != nil {
			return err
		}
		if hasPrior {
			// An exact fact slot wins over a broad domain echo. A changed value
			// must proceed to adjudication and supersede its own prior version.
			if !isDuplicatePromotion(c, prior, true) {
				return nil
			}
		} else {
			prior, hasPrior, err = s.currentEchoClaimWithResolver(c, 0, resolver)
			if err != nil {
				return err
			}
			if !hasPrior {
				return nil
			}
		}
		evidence, err := s.RecordEvidence(EvidenceItem{
			SourceKind: "memory-candidate",
			SourceID:   firstNonEmpty(c.SourceTraceRef, c.ID),
			QuoteText:  c.Entity + " " + c.Predicate + " " + c.Object,
			EventTime:  c.ObservationDate,
			Actor:      "agent",
			Visibility: c.Visibility,
		}, now)
		if err != nil {
			return err
		}
		seq, err := s.nextSeq()
		if err != nil {
			return err
		}
		edge := reinforcesEdge(evidence.ID, prior, c, seq, now)
		if err := appendJSONL(s.edgesLog(), &edge); err != nil {
			return err
		}
		reinforced = true
		return nil
	})
	return reinforced, err
}

func (s *Store) memoryTransition(claim Claim, prior Claim, hasPrior bool, workID string, now time.Time) envelope.Transition {
	wid := workID
	if wid == "" {
		wid = "work_mem_" + shortHash(claim.ID) // the adjudication is the unit of work
	}
	before := []string{}
	delta := "claim added: " + claim.Entity + "." + claim.Predicate + " = " + claim.Object
	if hasPrior {
		before = []string{prior.ID}
		delta = "claim superseded: " + claim.Entity + "." + claim.Predicate + ": " + prior.Object + " -> " + claim.Object
	}
	return envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            idFor("tr_", claim.ID),
			CreatedAt:     rfc3339(now),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: wid},
			Scope:         envelope.Scope{EntityID: claim.Entity},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  claim.EvidenceRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		BeforeRefs: before,
		ActionRefs: []string{},
		AfterRefs:  []string{claim.ID},
		StateDelta: delta,
		Projections: envelope.Projections{
			Memory: []string{claim.ID}, Skill: []string{}, Product: []string{}, Cost: []string{}, Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{},
			Typed: []envelope.ProjectionRef{{Kind: "memory", Ref: claim.ID, Role: "promoted", EvidenceRef: strings.Join(claim.EvidenceRefs, ",")}},
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
	}
}

func (s *Store) persistedPromotionTransition(expected envelope.Transition) (envelope.Transition, bool, error) {
	record, err := s.eventStore.Read(expected.ID)
	if errors.Is(err, os.ErrNotExist) {
		return envelope.Transition{}, false, nil
	}
	if err != nil {
		return envelope.Transition{}, false, err
	}
	data := record.Bytes
	var transition envelope.Transition
	if err := json.Unmarshal(data, &transition); err != nil {
		return envelope.Transition{}, false, fmt.Errorf("persisted promotion transition %s unreadable: %w", expected.ID, err)
	}
	if err := transition.Validate(); err != nil {
		return envelope.Transition{}, false, fmt.Errorf("persisted promotion transition %s invalid: %w", expected.ID, err)
	}
	if transition.ID != expected.ID ||
		transition.StateDelta != expected.StateDelta ||
		transition.Scope.EntityID != expected.Scope.EntityID ||
		!sameStrings(transition.BeforeRefs, expected.BeforeRefs) ||
		!sameStrings(transition.AfterRefs, expected.AfterRefs) ||
		!sameStrings(transition.Projections.Memory, expected.Projections.Memory) {
		return envelope.Transition{}, false, fmt.Errorf("persisted promotion transition %s does not match claim projection", expected.ID)
	}
	return transition, true, nil
}

func (s *Store) writePromotionTransition(transition *envelope.Transition) error {
	_, err := s.eventStore.PutJSONAtomic(transition.ID, transition, "  ", eventstore.AtomicOptions{
		SyncDirectory: false,
		Write: func(file *os.File, data []byte) error {
			_, err := file.Write(data)
			return err
		},
	})
	return err
}

// --- recall (relevance-gated, as-of-aware fold) ---

type RecallOpts struct {
	AsOfSeq            int64    // 0 = now (latest)
	IncludeRetired     bool     // default false
	IncludeNeedsReview bool     // default false: held claims do not keep ruling recall
	IncludeEvidence    bool     // include annotated candidates; default false
	MinConfidence      float64  // hard floor
	Visibilities       []string // allowed tiers; empty = all (minus retired unless IncludeRetired)
	Topic              string   // optional topic subtree root; filters claims by topic_refs
}

type RecallResult struct {
	Entity             string      `json:"entity"`
	AsOfSeq            int64       `json:"as_of_seq"`
	Topic              string      `json:"topic,omitempty"`
	TopicSubtree       []string    `json:"topic_subtree,omitempty"`
	Claims             []Claim     `json:"claims"`
	EvidenceCandidates []Candidate `json:"evidence_candidates,omitempty"`
	Filtered           int         `json:"filtered"`
	Coverage           string      `json:"coverage"`
}

// Recall returns the current claims for an entity AS OF a seq, after HARD filters
// (visibility/retired/confidence) run BEFORE the fold — never a post-filter. It returns a
// coverage verdict and never fabricates: an empty result says so plainly.
func (s *Store) Recall(entity string, opts RecallOpts) (RecallResult, error) {
	claims, err := s.loadClaimsAsOf(opts.AsOfSeq)
	if err != nil {
		return RecallResult{Entity: entity}, err
	}
	entity = strings.TrimSpace(entity)
	if entity != "" {
		entity, err = s.canonicalRecallEntity(entity, claims, opts.IncludeEvidence)
		if err != nil {
			return RecallResult{Entity: entity}, err
		}
	}
	topicSet, topicSubtree, err := s.topicScopeSet(opts.Topic)
	if err != nil {
		return RecallResult{Entity: entity}, err
	}
	res := RecallResult{Entity: entity, Topic: strings.TrimSpace(opts.Topic), TopicSubtree: topicSubtree}
	asOf := opts.AsOfSeq
	if asOf == 0 {
		asOf = s.maxSeq(claims)
	}
	res.AsOfSeq = asOf

	// 1. HARD gates first (scope/time/visibility/confidence).
	var gated []Claim
	for _, c := range claims {
		if entity != "" && c.Entity != entity {
			continue
		}
		if c.Seq > asOf {
			continue
		}
		if len(topicSet) > 0 && !claimHasTopicRef(c, topicSet) {
			continue
		}
		if !opts.IncludeRetired && c.Visibility == "retired" {
			res.Filtered++
			continue
		}
		if !opts.IncludeNeedsReview && c.State == ClaimStateNeedsReview {
			res.Filtered++
			continue
		}
		if opts.MinConfidence > 0 && c.Confidence < opts.MinConfidence {
			res.Filtered++
			continue
		}
		if len(opts.Visibilities) > 0 && !oneOf(c.Visibility, opts.Visibilities) {
			res.Filtered++
			continue
		}
		gated = append(gated, c)
	}

	// 2. Fold supersession as-of seq: a claim superseded by an edge with seq <= asOf is not current.
	superseded, err := s.supersededAsOf(asOf)
	if err != nil {
		return res, err
	}
	for _, c := range gated {
		if !superseded[c.ID] {
			res.Claims = append(res.Claims, c)
		}
	}
	sort.Slice(res.Claims, func(i, j int) bool { return res.Claims[i].Seq < res.Claims[j].Seq })

	if opts.IncludeEvidence {
		evidence, err := s.annotatedEvidenceCandidates(entity, opts)
		if err != nil {
			return res, err
		}
		res.EvidenceCandidates = evidence
	}

	switch {
	case len(res.Claims) > 0 && len(res.EvidenceCandidates) > 0:
		res.Coverage = fmt.Sprintf("%d current claim(s) for %q as seq %d (%d gated); %d annotated evidence candidate(s)", len(res.Claims), entity, asOf, res.Filtered, len(res.EvidenceCandidates))
	case len(res.EvidenceCandidates) > 0:
		res.Coverage = fmt.Sprintf("no current claims recorded for %q as seq %d; %d annotated evidence candidate(s)", entity, asOf, len(res.EvidenceCandidates))
	case len(res.Claims) > 0:
		res.Coverage = fmt.Sprintf("%d current claim(s) for %q as of seq %d (%d gated)", len(res.Claims), entity, asOf, res.Filtered)
	case res.Filtered > 0:
		res.Coverage = fmt.Sprintf("no claims pass the gate for %q (%d filtered by visibility/confidence) — not fabricating", entity, res.Filtered)
	default:
		res.Coverage = fmt.Sprintf("no claims recorded for %q as of seq %d", entity, asOf)
	}
	return res, nil
}

// Timeline walks the supersession chain for an entity, oldest→newest — the "what changed"
// view (delta-aware memory on the owned ledger, no engine).
func (s *Store) annotatedEvidenceCandidates(entity string, opts RecallOpts) ([]Candidate, error) {
	candidates, err := s.AllCandidates()
	if err != nil {
		return nil, err
	}
	topicSet, _, err := s.topicScopeSet(opts.Topic)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0)
	for _, c := range candidates {
		if c.Entity != entity || len(c.Annotations) == 0 {
			continue
		}
		if len(topicSet) > 0 && !candidateHasTopicRef(c, topicSet) {
			continue
		}
		if !opts.IncludeRetired && c.Visibility == "retired" {
			continue
		}
		if opts.MinConfidence > 0 && c.Confidence < opts.MinConfidence {
			continue
		}
		if len(opts.Visibilities) > 0 && !oneOf(c.Visibility, opts.Visibilities) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out, nil
}

func candidateHasTopicRef(candidate Candidate, topicSet map[string]bool) bool {
	for _, ref := range candidate.TopicRefs {
		if topicSet[ref] {
			return true
		}
	}
	return false
}

func (s *Store) Timeline(entity string) ([]Claim, error) {
	claims, err := s.loadClaims()
	if err != nil {
		return nil, err
	}
	entity, err = s.canonicalRecallEntity(entity, claims, false)
	if err != nil {
		return nil, err
	}
	var out []Claim
	for _, c := range claims {
		if c.Entity == entity {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (s *Store) canonicalRecallEntity(entity string, claims []Claim, includeEvidence bool) (string, error) {
	entity = strings.TrimSpace(entity)
	if entity == "" {
		return entity, nil
	}
	for _, c := range claims {
		if c.Entity == entity {
			return entity, nil
		}
	}
	matches := map[string]struct{}{}
	for _, c := range claims {
		if strings.EqualFold(c.Entity, entity) {
			matches[c.Entity] = struct{}{}
		}
	}
	if len(matches) == 1 {
		for match := range matches {
			return match, nil
		}
	}
	if len(matches) > 1 {
		return entity, nil
	}
	if r, err := s.ResolveEntity(entity); err != nil {
		return "", err
	} else if r.Status == "resolved" {
		return r.Slug, nil
	} else if r.Status == "multi" {
		return entity, nil
	}

	if includeEvidence {
		candidates, err := s.AllCandidates()
		if err != nil {
			return "", err
		}
		for _, c := range candidates {
			if c.Entity == entity {
				return entity, nil
			}
			if strings.EqualFold(c.Entity, entity) {
				matches[c.Entity] = struct{}{}
			}
		}
	}
	if len(matches) != 1 {
		return entity, nil
	}
	for match := range matches {
		return match, nil
	}
	return entity, nil
}

// CurrentClaim is the exported read door onto currentClaim for callers
// outside this package (e.g. internal/authority resolvers) that need the
// current (highest-seq, non-superseded) promoted claim for one entity+
// predicate pair "as of now". It never mutates the store.
func (s *Store) CurrentClaim(entity, predicate string) (Claim, bool, error) {
	return s.currentClaim(entity, predicate, 0)
}

// currentClaim returns the current claim for entity+predicate as of asOf (0 = now), if any.
func (s *Store) currentClaim(entity, predicate string, asOf int64) (Claim, bool, error) {
	claims, err := s.loadClaimsAsOf(asOf)
	if err != nil {
		return Claim{}, false, err
	}
	if asOf == 0 {
		asOf = s.maxSeq(claims)
	}
	superseded, err := s.supersededAsOf(asOf)
	if err != nil {
		return Claim{}, false, err
	}
	var best Claim
	found := false
	for _, c := range claims {
		if c.Entity == entity && c.Predicate == predicate && c.Seq <= asOf && !superseded[c.ID] {
			if !found || c.Seq > best.Seq {
				best, found = c, true
			}
		}
	}
	return best, found, nil
}

func (s *Store) currentClusterClaim(cand Candidate, asOf int64) (Claim, bool, error) {
	entities, err := s.Entities()
	if err != nil {
		return Claim{}, false, err
	}
	return s.currentClusterClaimWithResolver(cand, asOf, newEntityResolver(entities))
}

func (s *Store) currentClusterClaimWithResolver(cand Candidate, asOf int64, resolver *entityResolver) (Claim, bool, error) {
	claims, err := s.loadClaimsAsOf(asOf)
	if err != nil {
		return Claim{}, false, err
	}
	if asOf == 0 {
		asOf = s.maxSeq(claims)
	}
	want := candidateClusterKeyWithResolver(resolver, cand)
	superseded, err := s.supersededAsOf(asOf)
	if err != nil {
		return Claim{}, false, err
	}
	var best Claim
	found := false
	for _, claim := range claims {
		if claim.Seq > asOf || superseded[claim.ID] {
			continue
		}
		got := claimClusterKeyWithResolver(resolver, claim)
		if sameClusterKey(got, want) && (!found || claim.Seq > best.Seq) {
			best, found = claim, true
		}
	}
	return best, found, nil
}

func (s *Store) currentEchoClaim(cand Candidate, asOf int64) (Claim, bool, error) {
	entities, err := s.Entities()
	if err != nil {
		return Claim{}, false, err
	}
	return s.currentEchoClaimWithResolver(cand, asOf, newEntityResolver(entities))
}

func (s *Store) currentEchoClaimWithResolver(cand Candidate, asOf int64, resolver *entityResolver) (Claim, bool, error) {
	claims, err := s.loadClaimsAsOf(asOf)
	if err != nil {
		return Claim{}, false, err
	}
	if asOf == 0 {
		asOf = s.maxSeq(claims)
	}
	want := candidateEchoKeyWithResolver(resolver, cand)
	superseded, err := s.supersededAsOf(asOf)
	if err != nil {
		return Claim{}, false, err
	}
	var best Claim
	found := false
	for _, claim := range claims {
		if claim.Seq > asOf || superseded[claim.ID] || !SameClaimObject(claim.Object, cand.Object) || !sameQualifiers(claim.Qualifiers, cand.Qualifiers) {
			continue
		}
		got := claimEchoKeyWithResolver(resolver, claim)
		if sameClusterKey(got, want) && (!found || claim.Seq > best.Seq) {
			best, found = claim, true
		}
	}
	return best, found, nil
}

// --- doctor freshness thresholds ---
//
// A stalled pipeline produces an "ok" doctor with stale evidence: the inbox
// grows while claim writes are flat. The thresholds below surface that as
// warn/fail so the operator sees the stall signature, not just totals.
// Quiet inbox (no candidates) is fine — quiet != stalled.
// memDoctorThresholds holds the freshness gates. Vars (not consts) so
// tests can shrink them; production callers use the defaults.
var memDoctorThresholds = struct {
	ClaimStaleAfter     time.Duration
	CandidateStaleAfter time.Duration
	// PendingStateFreshFor bounds how long a reconciled pending count stays
	// authoritative. Past it (or once the inbox generation moves) the doctor
	// reports the count as stale instead of silently re-serving it.
	PendingStateFreshFor time.Duration
	InboxBacklogWarn     int
	InboxBacklogFail     int
}{
	ClaimStaleAfter:      72 * time.Hour,
	CandidateStaleAfter:  168 * time.Hour,
	PendingStateFreshFor: 24 * time.Hour,
	InboxBacklogWarn:     5_000,
	InboxBacklogFail:     50_000,
}

// --- doctor (which hop) ---

type Hop struct {
	Name              string `json:"name"`
	Status            string `json:"status"` // ok | empty | skipped | warn | fail
	Detail            string `json:"detail"`
	NewestClaimAt     string `json:"newest_claim_at,omitempty"`
	NewestCandidateAt string `json:"newest_candidate_at,omitempty"`
	InboxBacklog      int    `json:"inbox_backlog,omitempty"`
}
type Report struct {
	Hops              []Hop              `json:"hops"`
	OK                bool               `json:"ok"`
	Note              string             `json:"note"`
	SemanticReadiness *SemanticReadiness `json:"semantic_readiness,omitempty"`
	// Warnings is the list of hops with status="warn" rolled into a
	// single field so a downstream consumer can grep warnings without
	// walking the hops array. Each entry is "<hop-name>: <status-detail>"
	// — short enough to fit a one-line warn row, and parallel to the
	// OK field's binary shape: false means at least one hop failed;
	// len(Warnings) > 0 means at least one hop warned. The memory
	// doctor in cmd/compound surfaces this array in JSON and under a
	// `warnings:` heading in text mode; per the exit-code contract it
	// NEVER causes a non-zero exit on its own.
	Warnings []string `json:"warnings"`
}

type SemanticReadiness struct {
	Status               string `json:"status"`
	Profile              string `json:"profile,omitempty"`
	ConfigPath           string `json:"config_path,omitempty"`
	Sidecar              string `json:"sidecar"`
	Engine               string `json:"engine,omitempty"`
	EmbedderConfigured   bool   `json:"embedder_configured"`
	EmbedderIdentity     string `json:"embedder_identity,omitempty"`
	EmbedderGuard        string `json:"embedder_guard,omitempty"`
	VectorCacheStatus    string `json:"vector_cache_status"`
	VectorCachePath      string `json:"vector_cache_path,omitempty"`
	VectorCacheCorpusGen int64  `json:"vector_cache_corpus_gen,omitempty"`
	VectorCacheClaims    int    `json:"vector_cache_claims,omitempty"`
	VectorCacheEmbedded  int    `json:"vector_cache_embedded,omitempty"`
	Detail               string `json:"detail"`
}

// Doctor reports WHICH hop is healthy or failed, not just "missing" — the fix differs per hop.
func (s *Store) Doctor() Report {
	return s.DoctorAt(nil, time.Now().UTC())
}

// DoctorWithEngine is Doctor plus an optional rented breadth engine. The clock is
// time.Now().UTC(); tests inject a fixed timestamp via DoctorAt.
func (s *Store) DoctorWithEngine(eng Engine) Report {
	return s.DoctorAt(eng, time.Now().UTC())
}

// DoctorAt is DoctorWithEngine plus a caller-supplied clock. The freshness gates
// depend on wall-clock comparisons and would silently red days after the fixture
// date ages past the staleness window — pass time.Now() in production, a fixed
// timestamp in tests so the staleness rule stays deterministic.
func (s *Store) DoctorAt(eng Engine, now time.Time) Report {
	r := Report{OK: true}
	now = now.UTC()
	add := func(name, status, detail string, extras ...hopExtras) {
		h := Hop{Name: name, Status: status, Detail: detail}
		for _, e := range extras {
			if e.newestClaimAt != "" {
				h.NewestClaimAt = e.newestClaimAt
			}
			if e.newestCandidateAt != "" {
				h.NewestCandidateAt = e.newestCandidateAt
			}
			if e.inboxBacklog > 0 {
				h.InboxBacklog = e.inboxBacklog
			}
		}
		r.Hops = append(r.Hops, h)
		if status == "fail" {
			r.OK = false
		}
	}

	// --- capture hop ---
	// Inbox backlog + newest-candidate age are the stall signature: a flat
	// claim count + a growing inbox + an old newest-candidate means
	// capture writes still flow but adjudication is wedged.
	//
	// The backlog is the PENDING (proposed) count, never the raw file count:
	// closed candidates (parked/promoted/rejected/no_promote) stay on disk,
	// and counting them as backlog fails the hop forever (lived 2026-08-25:
	// 133,542 files reported as backlog while 34 were actually pending).
	// The pending count comes from the reconciled state the promote-driver
	// sweep writes after a full-budget walk — and it is authoritative ONLY
	// while fresh: reconciled recently enough AND against the same inbox
	// generation (the file count has not moved since). A stale or
	// generation-mismatched count can lie in BOTH directions (thousands of
	// new candidates arrived after reconciliation), so it reads as "stale /
	// unknown", never as a confident number. Never reconciled means the hop
	// cannot prove the backlog small, so it warns instead of guessing.
	candidateNames, generation, newestCandidateAt, _ := s.candidateInboxMetadata()
	fileCount := len(candidateNames)
	captureExtras := hopExtras{}
	if !newestCandidateAt.IsZero() {
		captureExtras.newestCandidateAt = rfc3339(newestCandidateAt)
	}
	captureStatus := statusOf(fileCount > 0)
	captureDetail := fmt.Sprintf("%d candidate file(s) in inbox", fileCount)
	// pendingPressure is the stall signal the promotion hop consumes: the
	// reconciled pending count when authoritative, -1 when unknown (never
	// reconciled, stale, or generation-mismatched).
	pendingPressure := -1
	if pendingState, err := s.ReadPendingState(); err == nil {
		if pendingStateFresh(pendingState, fileCount, generation, now) {
			pendingPressure = pendingState.Pending
			captureExtras.inboxBacklog = pendingState.Pending
			captureDetail = fmt.Sprintf("%d pending / %d candidate file(s) (reconciled %s)",
				pendingState.Pending, fileCount, pendingState.AsOf)
			if pendingState.Pending >= memDoctorThresholds.InboxBacklogFail {
				captureStatus = "fail"
			} else if pendingState.Pending >= memDoctorThresholds.InboxBacklogWarn {
				captureStatus = "warn"
			}
		} else {
			captureDetail += fmt.Sprintf("; reconciled pending count stale (as of %s, inbox now %d file(s)) — run the memory promote-driver sweep",
				pendingState.AsOf, fileCount)
			if captureStatus == "ok" {
				captureStatus = "warn"
			}
		}
	} else if fileCount > 0 {
		captureDetail += "; pending count not reconciled (run the memory promote-driver sweep)"
		if captureStatus == "ok" {
			captureStatus = "warn"
		}
	}
	add("capture", captureStatus, captureDetail, captureExtras)

	// --- promotion hop ---
	// Newest claim-ledger WRITE age vs inbox: the promotion pipeline stopping
	// while capture still flows is the stall. The signal is the ledger's
	// append evidence (claims.jsonl mtime), NEVER a claim's declared
	// created_at — imports, backfills, and test fixtures legitimately carry
	// old declared dates on rows appended seconds ago (lived false-positive
	// 2026-08-24: TestMemoryDoctorUsesConfiguredSidecarEngine exited silently
	// because a 55d-old fixture claim read as a stalled pipeline).
	claims, err := s.loadClaims()
	if err != nil {
		add("promotion", "fail", "cannot read claims: "+err.Error())
	} else {
		newestClaimAt := ledgerAppendTime(s.claimsLog())
		extras := hopExtras{}
		if !newestClaimAt.IsZero() {
			extras.newestClaimAt = rfc3339(newestClaimAt)
		}
		detail := fmt.Sprintf("%d claim(s) promoted to ledger", len(claims))
		status := statusOf(len(claims) > 0)
		// Stall signature: the ledger stopped being appended to while LIVE
		// promotion pressure exists. Pressure is the reconciled pending count
		// when authoritative (>0), otherwise the capture-write signal — a
		// fresh candidate mtime — and NEVER the mere existence of candidate
		// files: closed candidates stay on disk forever, so a directory of
		// parked/promoted/no_promote files with zero pending must not read
		// as a backlog (the drive-down to zero would leave doctor red
		// forever). An old declared created_at alone is also not a fail.
		stalled := false
		if !newestClaimAt.IsZero() && now.Sub(newestClaimAt) > memDoctorThresholds.ClaimStaleAfter {
			if pendingPressure >= 0 {
				stalled = pendingPressure > 0
			} else {
				stalled = fileCount > 0 && !newestCandidateAt.IsZero() &&
					now.Sub(newestCandidateAt) <= memDoctorThresholds.CandidateStaleAfter
			}
		}
		if stalled {
			detail += fmt.Sprintf(" (newest claim write %s old; live promotion pressure remains)",
				truncateAge(now.Sub(newestClaimAt)))
			status = "fail"
		}
		add("promotion", status, detail, extras)
	}
	if eng == nil {
		add("sync/embedding", "skipped", "no engine wired (ledger answers exact/as-of without one)")
	} else {
		s.doctorEngineHop(eng, add)
	}

	// retrieval: do a recall on an arbitrary recorded entity and confirm it returns.
	if len(claims) > 0 {
		rr, err := s.Recall(claims[0].Entity, RecallOpts{})
		if err != nil {
			add("retrieval", "fail", err.Error())
		} else {
			add("retrieval", statusOf(len(rr.Claims) > 0), rr.Coverage)
		}
	} else {
		add("retrieval", "empty", "no claims to retrieve")
	}
	s.doctorRecallQualityHop(add)

	// authority framing: every claim must carry provenance (derivation + evidence).
	missing := 0
	for _, c := range claims {
		if c.Derivation == "" || len(c.EvidenceRefs) == 0 {
			missing++
		}
	}
	if missing > 0 {
		add("authority", "fail", fmt.Sprintf("%d claim(s) missing provenance", missing))
	} else {
		add("authority", statusOf(len(claims) > 0), "all claims carry derivation + evidence")
	}
	// Roll warnings up AFTER every hop is added so no warn hop can be
	// missed by ordering (the exit-code contract in internal/doctor/
	// exitcode: warnings never flip OK, but they must never be silently
	// buried inside "ok": true either).
	for _, h := range r.Hops {
		if h.Status == "warn" {
			r.Warnings = append(r.Warnings, h.Name+": "+h.Detail)
		}
	}
	if r.OK {
		r.Note = "memory hops healthy (or empty/skipped); no failed hop"
	} else {
		r.Note = "a memory hop FAILED — fix at that hop, not 'memory missing'"
	}
	return r
}

func (s *Store) doctorRecallQualityHop(add func(name, status, detail string, extras ...hopExtras)) {
	cur, err := s.currentVisible()
	if err != nil {
		add("recall-quality", "fail", "cannot read current claims: "+err.Error())
		return
	}
	if len(cur) == 0 {
		add("recall-quality", "empty", "no current visible claims to probe")
		return
	}
	foldCounts := map[string]int{}
	for _, c := range cur {
		if alternateCaseProbe(c.Entity) == "" {
			continue
		}
		foldCounts[strings.ToLower(c.Entity)]++
	}
	for _, c := range cur {
		probe := alternateCaseProbe(c.Entity)
		if probe == "" || foldCounts[strings.ToLower(c.Entity)] != 1 {
			continue
		}
		rr, err := s.Recall(probe, RecallOpts{})
		if err != nil {
			add("recall-quality", "fail", fmt.Sprintf("%q probe failed: %v", probe, err))
			return
		}
		for _, got := range rr.Claims {
			if got.Entity == c.Entity {
				add("recall-quality", "ok", fmt.Sprintf("%q resolved %q with %d current claim(s)", probe, c.Entity, len(rr.Claims)))
				return
			}
		}
		add("recall-quality", "fail", fmt.Sprintf("%q did not resolve canonical entity %q: %s", probe, c.Entity, rr.Coverage))
		return
	}
	add("recall-quality", "empty", "no unambiguous alternate-case entity to probe")
}

func alternateCaseProbe(entity string) string {
	runes := []rune(strings.TrimSpace(entity))
	for i, r := range runes {
		switch {
		case 'a' <= r && r <= 'z':
			runes[i] = r - ('a' - 'A')
			return string(runes)
		case 'A' <= r && r <= 'Z':
			runes[i] = r + ('a' - 'A')
			return string(runes)
		}
	}
	return ""
}

func (s *Store) doctorEngineHop(eng Engine, add func(name, status, detail string, extras ...hopExtras)) {
	caps := eng.Capabilities()
	cur, err := s.currentVisible()
	if err != nil {
		add("sync/embedding", "fail", "cannot read current claims: "+err.Error())
		return
	}
	if len(cur) == 0 {
		add("sync/embedding", "empty", fmt.Sprintf("%s configured; no current visible claims to index", doctorEngineName(caps)))
		return
	}

	report, err := s.EngineDoctor(ClaimIndexText(cur[0]), 1, eng)
	detail := fmt.Sprintf("%s ledger_unchanged=%t hits=%d ledger_gen=%d engine_gen=%d", doctorEngineName(report.Engine), report.LedgerUnchanged, len(report.Hits), report.LedgerGen, report.Engine.CorpusGen)
	if report.OwnerLedgerAdvanced {
		detail += fmt.Sprintf(" owner_ledger_advanced=true retry_count=%d advanced_rows=%d", report.RetryCount, report.AdvancedRows)
	}
	if report.Note != "" {
		detail += " (" + report.Note + ")"
	}
	if err != nil {
		if errors.Is(err, ErrLedgerBusy) {
			add("sync/embedding", "skipped", detail+": current synchronization not certified while governed owner writes remain active")
			return
		}
		add("sync/embedding", "fail", detail+": "+err.Error())
		return
	}
	if report.Degraded {
		add("sync/embedding", "fail", detail+": degraded "+report.Error)
		return
	}
	if len(report.Hits) == 0 {
		add("sync/embedding", "fail", detail+": configured engine returned no hit for a current claim")
		return
	}
	add("sync/embedding", "ok", detail)
}

func doctorEngineName(caps EngineCapabilities) string {
	if strings.TrimSpace(caps.Name) != "" {
		return caps.Name
	}
	return "configured-engine"
}

// --- ledger internals (MVCC-by-fold) ---

// withLock serializes the seq-allocate + append critical section with an exclusive advisory
// file lock under COMPOUND_HOME/lock. The lock is released when the fd closes, so a crashed
// writer never wedges the ledger. (Darwin/Linux; compound's targets.)
func (s *Store) withLock(fn func() error) error {
	lockPath := filepath.Join(s.home, "lock", "mem.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if s.beforeLockObserver != nil {
		s.beforeLockObserver()
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// nextSeq mints the next monotonic-unique seq under the caller's lock. It FAILS LOUD on a corrupt
// counter (never silently resets to 1), SELF-HEALS from the immutable logs (the counter must exceed
// every seq already on disk — claims and edges share the seq space), and persists atomically
// (temp + rename) so an O_TRUNC-then-crash can't leave an empty counter.
func (s *Store) nextSeq() (int64, error) {
	cur := int64(0)
	if b, err := os.ReadFile(s.seqFile()); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			v, perr := strconv.ParseInt(t, 10, 64)
			if perr != nil {
				return 0, fmt.Errorf("corrupt seq counter %q: %w", t, perr)
			}
			cur = v
		}
	}
	hi, err := s.maxLedgerSeq()
	if err != nil {
		return 0, err
	}
	if hi > cur {
		cur = hi // a lost/zeroed counter heals to above everything on disk
	}
	cur++
	tmp := s.seqFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(cur, 10)), 0o644); err != nil {
		return 0, err
	}
	return cur, os.Rename(tmp, s.seqFile())
}

// maxLedgerSeq is the highest seq across claims AND edges (they share the seq space: relate-edges
// and supersedes edges both draw from nextSeq).
func (s *Store) maxLedgerSeq() (int64, error) {
	claims, err := s.loadClaimsRaw()
	if err != nil {
		return 0, err
	}
	hi := s.maxSeq(claims)
	err = readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if json.Unmarshal(line, &e) == nil && e.Seq > hi {
			hi = e.Seq
		}
		return nil
	})
	return hi, err
}

func (s *Store) loadClaims() ([]Claim, error) {
	return s.loadClaimsAsOf(0)
}

func (s *Store) loadClaimsAsOf(asOf int64) ([]Claim, error) {
	claims, err := s.loadClaimsRaw()
	if err != nil {
		return nil, err
	}
	return s.decorateClaimReviewStates(claims, asOf)
}

func (s *Store) loadClaimsRaw() ([]Claim, error) {
	var out []Claim
	err := readJSONL(s.claimsLog(), func(line []byte) error {
		var c Claim
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

func (s *Store) hasEdge(kind, from, to string) (bool, error) {
	exists := false
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var edge Edge
		if err := json.Unmarshal(line, &edge); err != nil {
			return err
		}
		if edge.Kind == kind && edge.From == from && edge.To == to {
			exists = true
		}
		return nil
	})
	return exists, err
}

type claimReviewMark struct {
	Refs            []string
	ConfidenceDelta float64
}

type candidateReviewMark struct {
	Refs []string
}

func (s *Store) decorateClaimReviewStates(claims []Claim, asOf int64) ([]Claim, error) {
	marks, err := s.claimReviewMarksAsOf(asOf)
	if err != nil {
		return nil, err
	}
	out := make([]Claim, len(claims))
	for i, claim := range claims {
		claim.State = ClaimStatePromoted
		if mark, ok := marks[claim.ID]; ok {
			claim.State = ClaimStateNeedsReview
			claim.ReviewRefs = append([]string(nil), mark.Refs...)
			claim.ConfidenceDelta = mark.ConfidenceDelta
		}
		out[i] = claim
	}
	return out, nil
}

func (s *Store) claimReviewMarksAsOf(asOf int64) (map[string]claimReviewMark, error) {
	type sourceState struct {
		markSeq         int64
		clearSeq        int64
		confidenceDelta float64
	}
	byClaim := map[string]map[string]*sourceState{}
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Kind != EdgeKindNeedsReview && e.Kind != EdgeKindReviewCleared {
			return nil
		}
		if asOf > 0 && e.Seq > asOf {
			return nil
		}
		sources := byClaim[e.To]
		if sources == nil {
			sources = map[string]*sourceState{}
			byClaim[e.To] = sources
		}
		state := sources[e.From]
		if state == nil {
			state = &sourceState{}
			sources[e.From] = state
		}
		switch e.Kind {
		case EdgeKindNeedsReview:
			if e.Seq >= state.markSeq {
				state.markSeq = e.Seq
				state.confidenceDelta = e.ConfidenceDelta
			}
		case EdgeKindReviewCleared:
			if e.Seq >= state.clearSeq {
				state.clearSeq = e.Seq
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]claimReviewMark{}
	for claimID, sources := range byClaim {
		var mark claimReviewMark
		var latestSeq int64
		for source, state := range sources {
			if state.markSeq == 0 || state.clearSeq > state.markSeq {
				continue
			}
			if source != "" {
				mark.Refs = append(mark.Refs, source)
			}
			if state.markSeq >= latestSeq {
				latestSeq = state.markSeq
				mark.ConfidenceDelta = state.confidenceDelta
			}
		}
		sort.Strings(mark.Refs)
		if latestSeq > 0 {
			out[claimID] = mark
		}
	}
	return out, nil
}

func (s *Store) candidateReviewMarksAsOf(asOf int64) (map[string]candidateReviewMark, error) {
	marks := map[string]candidateReviewMark{}
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Kind != EdgeKindCorroboration {
			return nil
		}
		if asOf > 0 && e.Seq > asOf {
			return nil
		}
		mark := marks[e.To]
		mark.Refs = append(mark.Refs, e.From)
		marks[e.To] = mark
		return nil
	})
	for id, mark := range marks {
		sort.Strings(mark.Refs)
		marks[id] = mark
	}
	return marks, err
}

func (s *Store) supersededAsOf(asOf int64) (map[string]bool, error) {
	out := map[string]bool{}
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Kind == EdgeKindSupersedes && e.Seq <= asOf {
			out[e.To] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Also honor each claim's own Supersedes pointer, so a lost/torn supersedes-edge append can
	// never leave two "current" claims for one entity+predicate (the new claim records what it replaced).
	claims, err := s.loadClaimsRaw()
	if err != nil {
		return nil, err
	}
	for _, c := range claims {
		if c.Supersedes != "" && c.Seq <= asOf {
			out[c.Supersedes] = true
		}
	}
	return out, nil
}

func (s *Store) maxSeq(claims []Claim) int64 {
	var m int64
	for _, c := range claims {
		if c.Seq > m {
			m = c.Seq
		}
	}
	return m
}

// --- small io helpers ---

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("not found: %s", filepath.Base(path))
		}
		return err
	}
	return json.Unmarshal(b, v)
}

// appendJSONL is the one append door for the owned ledger logs (claims, edges,
// evidence, entities, positions) — every authority write funnels through it.
// It mirrors the eventstore segment discipline (segment.go appendFrameAt): a
// torn final line is repaired first (a crash between Write and fsync can leave
// a fragment, and appending onto it would glue the new record to the fragment
// into one permanently unparseable line), then the record is written in full
// and fsynced before returning. The ledger is the meaning authority: a nil
// error must mean the bytes are durable, and a short write must never pass as
// success.
func appendJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := repairJSONLTail(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	line := append(b, '\n')
	n, err := f.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// tornFinalLine reports whether a ledger log ends in a torn final line: a
// crash-torn append shows up either as an unterminated tail (the newline never
// landed) or as a terminated-but-unparseable final line (the record body was
// partially flushed). Anything else — including syntactically valid JSON that
// fails a typed decode — is genuine corruption, not a tear.
func tornFinalLine(b []byte) bool {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return false
	}
	if b[len(b)-1] != '\n' {
		return true
	}
	last := trimmed[bytes.LastIndexByte(trimmed, '\n')+1:]
	return !json.Valid(last)
}

// repairJSONLTail drops a torn final line before an append (the write-path
// half of the eventstore repairTail pattern). Only the FINAL line is
// repairable: the kept prefix is re-validated first, so mid-file corruption
// fails loud and the file is left untouched for forensics.
func repairJSONLTail(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 || !tornFinalLine(data) {
		return nil
	}
	body := data
	if body[len(body)-1] == '\n' {
		body = body[:len(body)-1] // ignore the terminator; the final line itself is torn
	}
	keep := bytes.LastIndexByte(body, '\n') + 1 // 0 when no complete line precedes the fragment
	for i, line := range bytes.Split(data[:keep], []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(bytes.TrimSpace(line)) {
			return fmt.Errorf("repair %s: mid-file corruption at line %d; refusing to append", filepath.Base(path), i+1)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(keep)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readJSONL folds a ledger log line by line. A torn FINAL line is tolerated as
// never-written — the append did not complete — and the write path truncates
// it on the next append (repairJSONLTail). Any other unparseable line is
// mid-file corruption and fails loud.
func readJSONL(path string, fn func([]byte) error) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			if i == len(lines)-1 && tornFinalLine(b) {
				continue
			}
			return err
		}
	}
	return nil
}

func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(h[:])[:12]
}
func shortHash(s string) string    { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:])[:12] }
func ts(now time.Time) string      { return strconv.FormatInt(now.UTC().UnixNano(), 10) }
func rfc3339(now time.Time) string { return now.UTC().Format(time.RFC3339) }

// hopExtras carries the optional freshness fields a Doctor hop may stamp.
// Zero-value fields are omitted from JSON.
type hopExtras struct {
	newestClaimAt     string
	newestCandidateAt string
	inboxBacklog      int
}

// newestClaimCreatedAt returns the most recent CreatedAt across the loaded
// claims. Zero when claims are empty or all carry an unparseable timestamp.
// NOTE: declared claim dates are NOT pipeline evidence (see the promotion
// hop comment); this exists for display surfaces, never for stall detection.
func newestClaimCreatedAt(claims []Claim) time.Time {
	var newest time.Time
	for _, c := range claims {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(c.CreatedAt))
		if err != nil {
			continue
		}
		if t.After(newest) {
			newest = t
		}
	}
	return newest
}

// ledgerAppendTime returns the mtime of a ledger file — the append evidence
// of the pipeline that owns it. Zero when the file is missing or unreadable.
// Stall detection keys on THIS (the pipeline stopped writing), never on the
// declared timestamps of the rows inside (fixtures/imports can be arbitrarily
// old on rows appended seconds ago).
func ledgerAppendTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// truncateAge renders a time.Duration as a terse human-readable string for
// doctor output ("13d", "4h12m", "0s"). Rounds to days for >=24h, hours
// otherwise, so the freshness field reads "newest claim 13d old" not
// "newest claim 312h0m0s old".
func truncateAge(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}

func statusOf(ok bool) string {
	if ok {
		return "ok"
	}
	return "empty"
}
func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}
func orDef(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
