// Package work is the lifecycle store that threads ONE work_id through the whole
// chain: intent (WorkEnvelope) -> context (pack id) -> action (run ids) -> close,
// where close emits the Receipt and the apex Transition that prove the chain
// connected. State lives in COMPOUND_HOME (the runtime body), never the repo.
// See docs/architecture/transition.md and docs/architecture/contracts/README.md.
package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"compound/internal/capgraph"
	"slices"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

// transitions is the work-state machine. Terminal states (completed/failed/cancelled)
// have no outgoing edges. `claimed` is reached only via Claim (claim-before-you-touch).
var transitions = map[string][]string{
	"proposed":  {"admitted", "ready", "held", "cancelled"},
	"admitted":  {"ready", "held", "cancelled"},
	"ready":     {"claimed", "held", "cancelled"},
	"claimed":   {"running", "ready", "held", "cancelled"},
	"running":   {"verifying", "blocked", "held", "failed", "completed"},
	"verifying": {"completed", "running", "failed"},
	"blocked":   {"running", "cancelled", "failed"},
	"held":      {"ready", "cancelled"},
}

// terminalStates may not advance further; close lands a work in one of them.
var terminalStates = map[string]bool{"completed": true, "failed": true, "cancelled": true}

// ErrExecutionReservationChanged is returned when a governed executor tries
// to start or finalize against a running state/policy/action generation that
// another lifecycle mutation has superseded.
var ErrExecutionReservationChanged = errors.New("work execution reservation changed")

const crossStateClaimedAdvanceRefusal = "claimed is reached only via 'compound work claim' (claim-before-you-touch); bare advance cannot bind authority owner+lease"

func terminalStateRequiresClose(action, state string) error {
	return fmt.Errorf("%s cannot enter terminal state %q; use Close so Receipt and Transition proof are emitted", action, state)
}

func crossStateClaimedAdvanceError(from, to string) error {
	if to == "claimed" && from != "claimed" {
		return errors.New(crossStateClaimedAdvanceRefusal)
	}
	return nil
}

func validateExpectedExecutionReservation(w envelope.WorkEnvelope, expectedState, expectedPolicyDecisionID, expectedActionIntentID string) error {
	expectedState = strings.TrimSpace(expectedState)
	expectedPolicyDecisionID = strings.TrimSpace(expectedPolicyDecisionID)
	expectedActionIntentID = strings.TrimSpace(expectedActionIntentID)
	var currentActionIntentID string
	if n := len(w.ActionIntentIDs); n > 0 {
		currentActionIntentID = w.ActionIntentIDs[n-1]
	}
	if expectedState != "" && w.State != expectedState && expectedPolicyDecisionID == "" && expectedActionIntentID == "" {
		return fmt.Errorf("%w: work %s state %q no longer matches expected state %q", ErrExecutionReservationChanged, w.ID, w.State, expectedState)
	}
	if (expectedState != "" && w.State != expectedState) ||
		(expectedPolicyDecisionID != "" && w.PolicyDecisionID != expectedPolicyDecisionID) ||
		(expectedActionIntentID != "" && currentActionIntentID != expectedActionIntentID) {
		return fmt.Errorf(
			"%w: work %s state=%q policy=%q action_intent=%q; expected state=%q policy=%q action_intent=%q",
			ErrExecutionReservationChanged,
			w.ID,
			w.State,
			w.PolicyDecisionID,
			currentActionIntentID,
			expectedState,
			expectedPolicyDecisionID,
			expectedActionIntentID,
		)
	}
	return nil
}

func canTransition(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// claimEventFromIsLegal returns whether `from` is a legal lifecycle From
// for a transaction that lands the work in "claimed". A normal claim
// requires a real state-machine edge (canTransition). The ownerless-claim
// repair is a same-state binding (claimed -> claimed) -- the state
// machine has no self-loop, so the canTransition check is loosened for
// that case to keep the binding explicit and refused-if-claimed-to-
// something-else.
func claimEventFromIsLegal(from string, repairOwnerless bool) bool {
	if repairOwnerless && from == "claimed" {
		return true
	}
	return canTransition(from, "claimed")
}

// Store is the work lifecycle store rooted at a COMPOUND_HOME.
type Store struct {
	// advanceClock is a test clock; nil always samples real admission time.
	// Its argument lets deterministic fixtures preserve their explicit logical time.
	advanceClock  func(time.Time) time.Time
	home          string
	events        *eventstore.Store
	readOnly      bool
	eventLogReads atomic.Int64
	// Summary-projection reads only; hold evidence has its own observation pass.
	runObservationFileReads       atomic.Int64
	holdEvidenceFileReads         atomic.Int64
	experimentalDelegateMutations atomic.Bool
	sweepStaleFault               func(stage string) error
	resourceMigrationFault        func(stage string) error
}

// HomePath returns the COMPOUND_HOME root the store reads from and writes
// to. Composition roots (cmd/compound) need it once per CLI invocation to
// bind resolvers that take a homePath string (adapter registry, runattempt
// store, routeevidence reader). The accessor is the only sanctioned way to
// obtain the home; reading the unexported field would create a second,
// untyped access path that bypasses the rest of the seam's contracts.
func (s *Store) HomePath() string { return s.home }

func (s *Store) eventLogReadCount() int64 {
	return s.eventLogReads.Load()
}

// runAttemptStore is the RunAttempt persistence surface that the claim
// path depends on. *runattempt.Store satisfies it in production -- every
// method here is a verbatim signature of the concrete store -- so the
// runtime behavior is unchanged. The interface exists purely as a test
// seam: claim_runattempt_test substitutes a fault-injecting wrapper to
// exercise infra-failure return paths -- in particular a store whose
// SetOrigin fails while every other write (including the terminalizing
// Complete/CompleteWithReason) still lands, so the test can assert the
// freshly minted attempt is terminalized failed on disk rather than left
// stranded pending. The mirror of this seam in internal/harness
// (subprocess_runner.go) catches the same contract violation on the
// harness-execution lane.
type runAttemptStore interface {
	Mint(workID, actor, harness string, now time.Time) (runattempt.RunAttempt, error)
	SetOrigin(id, origin string) (runattempt.RunAttempt, error)
	SetWorkspace(id, workspace string) (runattempt.RunAttempt, error)
	SetCommand(id, command string) (runattempt.RunAttempt, error)
	SetDispatchIntent(id, laneID, requestedModelID, requestedProviderRoute string) (runattempt.RunAttempt, error)
	SetExecutionProfileIntent(id, planID, clientHarnessID, transportID, nativeEffort string) (runattempt.RunAttempt, error)
	SetLaunchProfileIntent(id, profileID, revision, clientProfileID string) (runattempt.RunAttempt, error)
	SetPredecessor(id, predecessorID string) (runattempt.RunAttempt, error)
	Get(id string) (runattempt.RunAttempt, error)
	Complete(id, status string, now time.Time) (runattempt.RunAttempt, error)
	CompleteWithReason(id, status, reason string, now time.Time) (runattempt.RunAttempt, error)
	CommitLaunch(id string, now time.Time, start func() error) (runattempt.RunAttempt, error)
	MarkRunning(id string, now time.Time) (runattempt.RunAttempt, error)
}

// runAttemptsFactory is the test seam through which the claim path
// obtains its RunAttempt store. Production: runattempt.Open(home).
// Tests inject a wrapper that fails individual writes (e.g., SetOrigin)
// while letting terminalizing writes land.
var runAttemptsFactory = func(home string) runAttemptStore {
	return runattempt.Open(home)
}

// prepareRunAttemptTerminalization is the deterministic failure-injection seam
// for the recoverable close boundary. Production delegates byte-for-byte to the
// RunAttempt store; tests can refuse one preparation without relying on host
// filesystem permissions.
var prepareRunAttemptTerminalization = func(store *runattempt.Store, id string, intent runattempt.TerminalizationIntent) (runattempt.RunAttempt, error) {
	return store.PrepareTerminalization(id, intent)
}

// runAttempts returns the RunAttempt evidence store rooted at the same
// COMPOUND_HOME. C1 mints a RunAttempt on every claim so the execution
// organ fires in production, not just in panel.Apply tests. The store is
// cheap to construct (path only, no I/O); no caching needed. The
// run-observation sibling uses the concrete *runattempt.Store directly
// for its own typed test seams (see run_observation.go); the claim path
// reaches the same store through the runAttemptStore interface seam
// instead, so claim_runattempt_test can inject a fault-injecting
// wrapper for the SetOrigin-failure return path.
func (s *Store) runAttempts() *runattempt.Store {
	return runattempt.Open(s.home)
}

// runAttemptsClaim is the claim-path entry point for the RunAttempt
// store. It returns the runAttemptStore interface so the test seam can
// swap in a fault-injecting wrapper; production behavior is byte-
// identical to runAttempts() (the factory returns the same concrete
// store). Only the claim path uses this; the run-observation sibling
// continues to use the typed *runattempt.Store directly.
func (s *Store) runAttemptsClaim() runAttemptStore {
	return runAttemptsFactory(s.home)
}

func (s *Store) holdEvidenceFileReadCount() int64 {
	return s.holdEvidenceFileReads.Load()
}

// withLifecycleMutation is the outer transaction boundary for authoritative
// work writers. The lock order is always autonomy-admission -> work-event:
// validate the complete shared event log while holding admission, release the
// event lock, then let the mutation append under that same order. No caller may
// enter this helper while already holding the work-event lock.
func (s *Store) withLifecycleMutation(fn func() error) error {
	if s.readOnly {
		return errors.New("work lifecycle mutation requires a writable store")
	}
	return home.WithAutonomyAdmissionLock(s.home, func() error {
		if _, err := s.eventsFor(""); err != nil {
			return fmt.Errorf("work lifecycle event log preflight: %w", err)
		}
		return fn()
	})
}

// withCloseAdmissionLock serializes close terminalization through the same
// shared work-admission inode every RunAttempt liveness write already holds
// (v3). Close is non-reentrant: the runattempt writes it triggers
// (PrepareTerminalization / CompletePreparedTerminalization) take only the
// per-attempt inode, never the work-admission lock, so the outer holder here
// cannot recurse into itself. ONE helper, never a second lock implementation:
// every site that needs the shared work-admission lock for workID goes through
// home.WithWorkAdmissionLock. The legacy capability-lifecycle path
// (withCreatePlanAdmissionLock) targets the same inode and is preserved for
// the installed-runtime cutover; close uses this helper so the close seam
// `closeAfterTerminalizationPrepare` runs strictly under the same authority.
func (s *Store) withCloseAdmissionLock(workID string, fn func() error) error {
	if strings.TrimSpace(workID) == "" || filepath.Base(workID) != workID {
		return fmt.Errorf("invalid close-admission work id %q", workID)
	}
	if err := home.EnsureWorkAdmissionAuthorityRoot(s.home); err != nil {
		return err
	}
	return home.WithWorkAdmissionLock(s.home, workID, fn)
}

// Open roots a store at homePath and ensures the work/event habitat dirs exist.
func Open(homePath string) (*Store, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	for _, d := range []string{"work/work", "work/leases", "work/land-checks"} {
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
	return &Store{home: homePath, events: events}, nil
}

// OpenReadOnly roots a store at homePath WITHOUT materializing any runtime
// state: it creates no directories and never calls events.Ensure. It exists so
// read-classified commands (presence, workbench, attention, coordination,
// narrate, executors, doctor) can honor the registry's read-only contract —
// inspecting the home must not itself leave footprints in COMPOUND_HOME.
// The read methods OpenReadOnly serves (List/ListWithArchived, ListLeases,
// eventsFor/eventsByWorkID, readWorkLogTail, ListPresenceSessions) all treat
// missing dirs/files as "empty", so a fresh or stripped home yields an empty
// projection rather than an error. Any mutating method called on a Store
// opened this way fails at its own write boundary, exactly as it would
// against any unprepared home.
func OpenReadOnly(homePath string) (*Store, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("no COMPOUND_HOME (run `compound home init`)")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return nil, err
	}
	return &Store{home: homePath, events: events, readOnly: true}, nil
}

func (s *Store) workPath(id string) string  { return filepath.Join(s.home, "work/work", id+".json") }
func (s *Store) leasePath(id string) string { return filepath.Join(s.home, "work/leases", id+".json") }

// Event is one append-only lifecycle record.
type Event struct {
	TS                               string                            `json:"ts"`
	WorkID                           string                            `json:"work_id"`
	From                             string                            `json:"from"`
	To                               string                            `json:"to"`
	By                               string                            `json:"by,omitempty"`
	Note                             string                            `json:"note,omitempty"`
	Attached                         []string                          `json:"attached,omitempty"`
	PreviousWorkSHA256               string                            `json:"previous_work_sha256,omitempty"`
	WorkSHA256                       string                            `json:"work_sha256,omitempty"`
	ReceiptSHA256                    string                            `json:"receipt_sha256,omitempty"`
	RunAttemptTerminalizationDigests []RunAttemptTerminalizationDigest `json:"run_attempt_terminalization_digests,omitempty"`
}

// RunAttemptTerminalizationDigest binds the full immutable per-attempt intent
// into the closing lifecycle event. Shared close-generation fields alone are
// insufficient: owner, target, reason, or attempt authority could otherwise
// be rewritten after close and accepted by reconciliation.
type RunAttemptTerminalizationDigest struct {
	RunAttemptID string `json:"run_attempt_id"`
	IntentSHA256 string `json:"intent_sha256"`
}

// Lease records who holds a claimed work (claim-before-you-touch / D7).
type Lease struct {
	WorkID  string `json:"work_id"`
	By      string `json:"by"`
	ClaimAt string `json:"claimed_at"`
	// LaneID is the DECLARED lane's canonical agent-lane node id (resolved via
	// capgraph.ResolveLaneRef at claim/advance time), or empty when no lane was declared.
	// It is the single authoritative source for the lane stamped onto close-minted
	// receipts/transitions — never derived from actor/owner/harness (no-guessing, B0).
	LaneID     string `json:"lane_id,omitempty"`
	WorkSHA256 string `json:"work_sha256,omitempty"`
}

const claimPendingSchemaVersion = "compound.work_claim_pending.v0.1"

// claimPending is the durable transaction intent for a claim. It binds the
// pre/post WorkEnvelope versions, exact lease, exact lifecycle event, and the
// action/policy identities before any authoritative claim publication.
type claimPending struct {
	SchemaVersion      string                `json:"schema_version"`
	WorkID             string                `json:"work_id"`
	Actor              string                `json:"actor"`
	PreviousWorkSHA256 string                `json:"previous_work_sha256"`
	ClaimedWork        envelope.WorkEnvelope `json:"claimed_work"`
	ClaimedWorkSHA256  string                `json:"claimed_work_sha256"`
	Lease              Lease                 `json:"lease"`
	ActionIntentID     string                `json:"action_intent_id"`
	PolicyDecisionID   string                `json:"policy_decision_id"`
	// RunAttemptID is the ratt_* id minted at claim time (C1). Carried in the
	// pending so crash recovery can verify the attempt exists and matches the
	// envelope's RunAttemptIDs. Empty for pre-C1 pending files (backcompat).
	RunAttemptID    string `json:"run_attempt_id,omitempty"`
	RepairOwnerless bool   `json:"repair_ownerless,omitempty"`
	Event           Event  `json:"event"`
}

// --- create ---

type CreateInput struct {
	WorkID           string
	Owner            string
	Actor            string
	CanonicalSurface string
	CreatorHarness   string
	CreatorModel     string
	CreatorLaneID    string
	TargetSurface    string
	SuccessPredicate string
	DesiredOutcome   string
	NonGoals         []string
	EvidenceRefs     []string
	ContextPackID    string
	// ProposalKind classifies what kind of system-originated proposal this work carries
	// (see envelope.ProposalKinds). EMPTY is reserved for "unknown" and triggers an
	// EVOLVE-BLOCK guard denial at admission for autonomous proposals; ordinary work
	// and harness_edit work are explicit declarations the producer stamps. Plumbed
	// verbatim onto the envelope; unrecognized non-empty values are refused at
	// envelope.WorkEnvelope.Validate, BEFORE persistence.
	ProposalKind    string
	RouteDecisionID string
	GitDelivery     *envelope.GitDeliveryContract
	// ExpectedBaseSHA is retained only so legacy callers fail with an explicit
	// incomplete-contract error instead of silently losing their old field.
	ExpectedBaseSHA string
	State           string // default "ready"
	Risk            string // default "tier1"
	Privacy         string // default "private"
	// Uncertainty is the bounded derivation uncertainty preserved on a
	// proposed WorkEnvelope. A zero value keeps the established known default.
	Uncertainty envelope.Uncertainty
	// Prediction is an optional stake of how this work is expected to CLOSE (see
	// envelope.WorkPrediction). Staked at create time only -- there is no restaking at
	// Claim/Advance in v1.
	Prediction *envelope.WorkPrediction
	// RouteDeviation, when set, marks this work as an explicit deviation from the served policy
	// line and carries the risk grade admission binds on (see envelope.RouteDeviation). A
	// judge-grade (or ungraded) deviation is REFUSED at admission unless Prediction is also staked;
	// the refusal is fail-closed in envelope.WorkEnvelope.Validate, which every create/admission
	// path funnels through.
	RouteDeviation *envelope.RouteDeviation
	// PlanClass declares how much alignment this work owes before it may run (see
	// envelope.PlanClasses). Copied VERBATIM onto the envelope -- empty stays empty rather than
	// being defaulted to "oneshot", because the semantics "empty means oneshot" are enforced at
	// the guard, not baked into stored bytes (mirroring freeze's "absent means nothing gated").
	// An unrecognized value is refused by envelope.WorkEnvelope.Validate before persistence.
	PlanClass string
	// ReworkOfWorkID records that this work exists because an earlier landed work broke
	// something (see envelope.WorkEnvelope.ReworkOfWorkID). Copied VERBATIM onto the envelope.
	// A malformed or self-referential value is refused by envelope.WorkEnvelope.Validate, and a
	// value that does not resolve in this store is refused by validateReworkOfRef at admission
	// -- both BEFORE anything persists.
	ReworkOfWorkID string
	// ExecutionSelection is the saved work-level execution selection
	// (harness + route + model + effort + source + revision + actor +
	// timestamp), recorded at create time so a follow-up launch uses
	// THIS combination rather than whatever the route plan / preset
	// currently says. When set, Source MUST be explicit (project /
	// system defaults do not need to be persisted — they would only
	// survive until the next default change), Revision MUST be 1, and
	// the field is validated by envelope.WorkEnvelope.Validate before
	// anything persists. A subsequent edit goes through
	// SaveExecutionSelection (revision +1 + lifecycle event + CAS), never
	// through Create.
	//
	// work_r7a_execution_selection_20260911.
	ExecutionSelection *envelope.ExecutionSelection
}

// BuildWork constructs a WorkEnvelope WITHOUT persisting it — used both by Create (which
// then persists) and by callers that need a candidate (e.g. a UserPromptSubmit hook).
func BuildWork(in CreateInput, now time.Time) envelope.WorkEnvelope {
	workID := strings.TrimSpace(in.WorkID)
	if workID == "" {
		workID = idFor("work_", in.Owner, in.SuccessPredicate, ts(now))
	}
	var gitDelivery *envelope.GitDeliveryContract
	if in.GitDelivery != nil {
		copy := *in.GitDelivery
		gitDelivery = &copy
	}
	var prediction *envelope.WorkPrediction
	if in.Prediction != nil {
		copy := *in.Prediction
		prediction = &copy
	}
	var routeDeviation *envelope.RouteDeviation
	if in.RouteDeviation != nil {
		copy := *in.RouteDeviation
		routeDeviation = &copy
	}
	var executionSelection *envelope.ExecutionSelection
	if in.ExecutionSelection != nil {
		copy := *in.ExecutionSelection
		// Belt-and-suspenders: the contract says a create-time selection is
		// explicit + revision 1. Force those here so a caller cannot slip a
		// non-explicit copy through BuildWork (the validator catches it too,
		// but this short-circuits to a clearer error path).
		if copy.Source != "explicit" {
			copy.Source = "explicit"
		}
		if copy.Revision < 1 {
			copy.Revision = 1
		}
		executionSelection = &copy
	}
	origin := deriveWorkOrigin(in)
	w := envelope.WorkEnvelope{
		Header:             newHeader(envelope.SVWork, workID, now, "", in.ContextPackID, orDefault(in.Privacy, "private"), orDefault(in.Risk, "tier1")),
		Owner:              in.Owner,
		CanonicalSurface:   strings.TrimSpace(in.CanonicalSurface),
		TargetSurface:      strings.TrimSpace(in.TargetSurface),
		ProposalKind:       strings.TrimSpace(in.ProposalKind),
		State:              orDefault(in.State, "ready"),
		DesiredOutcome:     in.DesiredOutcome,
		SuccessPredicate:   in.SuccessPredicate,
		NonGoals:           orEmpty(in.NonGoals),
		ExpectedBaseSHA:    strings.TrimSpace(in.ExpectedBaseSHA),
		GitDelivery:        gitDelivery,
		RouteDecisionID:    strings.TrimSpace(in.RouteDecisionID),
		ActionIntentIDs:    []string{},
		ArtifactIDs:        []string{},
		VerificationIDs:    []string{},
		MemoryCandidateIDs: []string{},
		Prediction:         prediction,
		RouteDeviation:     routeDeviation,
		PlanClass:          in.PlanClass,
		ReworkOfWorkID:     strings.TrimSpace(in.ReworkOfWorkID),
		Origin:             &origin,
		ExecutionSelection: executionSelection,
	}
	if strings.TrimSpace(in.Uncertainty.Level) != "" || strings.TrimSpace(in.Uncertainty.Reason) != "" {
		w.Uncertainty = in.Uncertainty
	}
	w.EvidenceRefs = orEmpty(in.EvidenceRefs)
	return w
}

func deriveWorkOrigin(in CreateInput) envelope.WorkOrigin {
	actor := firstNonEmpty(strings.TrimSpace(in.Actor), strings.TrimSpace(in.Owner), "compound")
	harness := strings.TrimSpace(in.CreatorHarness)
	requestedModel := strings.TrimSpace(in.CreatorModel)
	laneID := strings.TrimSpace(in.CreatorLaneID)
	identitySource := "explicit_or_environment"
	if harness == "" && requestedModel == "" && laneID == "" {
		identitySource = "derived"
	}
	if harness == "" {
		harness = inferCreatorHarness(actor)
	}
	if requestedModel == "" {
		switch harness {
		case "compound", "operator", "script", "scheduler":
			requestedModel = "none"
		default:
			requestedModel = "unknown"
		}
	}
	if laneID == "" {
		laneID = actor
	}
	return envelope.WorkOrigin{
		AccountableOwner: accountableOwnerRef(in.Owner),
		Actor:            actor,
		Harness:          harness,
		RequestedModel:   requestedModel,
		LaneID:           laneID,
		IdentitySource:   identitySource,
	}
}

func accountableOwnerRef(owner string) string {
	owner = strings.TrimSpace(owner)
	if before, after, ok := strings.Cut(owner, ":"); ok && strings.TrimSpace(before) != "" && strings.TrimSpace(after) != "" {
		return owner
	}
	normalized := slug(owner)
	if normalized == "" {
		return "owner:unknown"
	}
	lower := strings.ToLower(owner)
	switch lower {
	case "codex", "claude", "claude_code", "oh_my_pi", "hermes", "opencode", "minimax", "claude-minimax", "claude-qwen":
		return "harness:" + normalized
	case "compound":
		return "compound:core"
	case "operator":
		return "operator:unknown"
	}
	if strings.HasPrefix(lower, "compound ") {
		return "compound:" + slug(strings.TrimSpace(owner[len("compound "):]))
	}
	if strings.ContainsAny(owner, `/\`) {
		return "surface:" + normalized
	}
	return "owner:" + normalized
}

func inferCreatorHarness(actor string) string {
	for _, token := range strings.FieldsFunc(strings.ToLower(actor), func(r rune) bool {
		return r == ':' || r == '/' || r == ' '
	}) {
		switch {
		case token == "codex":
			return "codex"
		case token == "claude_code" || token == "claude-code":
			return "claude_code"
		case token == "claude":
			return "claude"
		case strings.HasPrefix(token, "claude-"):
			return token
		case token == "oh_my_pi" || token == "omp":
			return "oh_my_pi"
		case token == "hermes":
			return "hermes"
		case token == "opencode":
			return "opencode"
		case token == "operator":
			return "operator"
		case token == "compound":
			return "compound"
		}
	}
	return "unknown"
}

// Create writes a new WorkEnvelope and its first lifecycle event only after the
// admission ActionIntent is allowed by policy.
func (s *Store) Create(in CreateInput, now time.Time) (envelope.WorkEnvelope, error) {
	plan, err := s.CreatePlan(in, now, CreatePlanOptions{Actor: in.Actor})
	if err != nil {
		return plan.Work, err
	}
	attached := []string(nil)
	if in.ContextPackID != "" {
		attached = append(attached, in.ContextPackID)
	}
	return s.admitWork(plan, now, "created", attached)
}

func (s *Store) validateContextPackRef(contextPackID string) error {
	contextPackID = strings.TrimSpace(contextPackID)
	if contextPackID == "" {
		return nil
	}
	var pack envelope.ContextPack
	path := filepath.Join(s.home, "context", "packs", contextPackID+".json")
	if err := s.readJSON(path, &pack); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("context_pack_id %q does not resolve to %s", contextPackID, filepath.ToSlash(filepath.Join("context", "packs", contextPackID+".json")))
		}
		return fmt.Errorf("read context_pack_id %q: %w", contextPackID, err)
	}
	if pack.ID != contextPackID {
		return fmt.Errorf("context_pack_id %q does not match artifact id %q", contextPackID, pack.ID)
	}
	if err := pack.Validate(); err != nil {
		return fmt.Errorf("context_pack_id %q invalid: %w", contextPackID, err)
	}
	if err := s.validateContextPackContractRef(contextPackID, pack.IDs.ContractID); err != nil {
		return err
	}
	return nil
}

// validateReworkOfRef proves the rework lineage edge RESOLVES in this store -- no phantom links,
// mirroring runattempt.SetPredecessor. The envelope validator already fixed the SHAPE (and
// refused self-reference); this is the existence half, and it is fail-closed at admission: an
// edge to a work id that does not resolve refuses the create BEFORE anything persists, because a
// rework-rate fold that counts edges to works that never existed reports a number nobody can
// trace back.
//
// The check is intentionally NOT transitive: the target being itself a rework is fine, and no
// cycle walk is needed -- the edge is create-time-only against an already-existing target, so a
// cycle would require an id collision the store already refuses.
func (s *Store) validateReworkOfRef(w envelope.WorkEnvelope) error {
	reworkOf := strings.TrimSpace(w.ReworkOfWorkID)
	if reworkOf == "" {
		return nil
	}
	if _, err := s.Get(reworkOf); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rework_of_work_id %q does not resolve to an existing work", reworkOf)
		}
		return fmt.Errorf("read rework_of_work_id %q: %w", reworkOf, err)
	}
	return nil
}

func (s *Store) validateContextPackContractRef(contextPackID, contractID string) error {
	contractID = strings.TrimSpace(contractID)
	if contractID == "" {
		return nil
	}
	pattern := filepath.Join(s.home, "contracts", "effective", "*", contractID+".json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("context_pack_id %q contract_id %q lookup failed: %w", contextPackID, contractID, err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("context_pack_id %q references contract_id %q but no %s artifact exists", contextPackID, contractID, filepath.ToSlash(filepath.Join("contracts", "effective", "*", contractID+".json")))
	}
	sort.Strings(matches)
	var ec envelope.EffectiveContract
	if err := s.readJSON(matches[len(matches)-1], &ec); err != nil {
		return fmt.Errorf("read context_pack_id %q contract_id %q: %w", contextPackID, contractID, err)
	}
	if ec.ID != contractID {
		return fmt.Errorf("context_pack_id %q contract_id %q does not match artifact id %q", contextPackID, contractID, ec.ID)
	}
	if err := ec.Validate(); err != nil {
		return fmt.Errorf("context_pack_id %q contract_id %q invalid: %w", contextPackID, contractID, err)
	}
	return nil
}

// --- show / list ---

// MintLaunchAuthority mints a fresh (ActionIntent, PolicyDecision) pair for
// a single unattended launch and stamps it onto the running-reservation's
// WorkEnvelope. DEFECT 3 fix (AUT-P0-003 2026-09-04): every harness launch
// needs a persisted authority tuple bound to the WorkEnvelope generation
// that approved it, so the chain on disk reads:
//
//	frozen WorkEnvelope generation
//	-> persisted unattended ActionIntent
//	-> persisted PolicyDecision
//	-> those exact ids on the running reservation
//	-> RunInput.ActionIntentID
//	-> RunAttempt
//	-> verifying transition comparing the SAME ids
//
// The launch wire (executewire.LaunchViaActuator) calls this method AFTER
// readiness and BEFORE the adapter mints its RunAttempt. The returned
// action-intent id becomes RunInput.ActionIntentID; the returned policy-
// decision id is forwarded to FinalizeGovernedHarnessRun as
// RunningPolicyID. The wenv's tail action/policy become the
// ExpectedActionIntentID / ExpectedPolicyDecisionID the verifying
// transition compares against; an attacker or a race that mutates the
// wenv between Mint and Finalize surfaces as ErrExecutionReservationChanged.
//
// The pair is persisted via the event store and the wenv is rewritten
// atomically under the lifecycle mutation lock; a crash between the
// event-store write and the wenv rewrite leaves both halves on disk so
// the next reconcile can re-pair them, instead of an unattached live
// RunAttempt. Both halves pass envelope.Validate() so the close-time
// authority gate (decideRunAttemptAuthorityFromLifecycle) can resolve
// the chain without further synthesis.
//
// `actor` is the operator/agent under whose authority the launch is run
// (the seam's per-tick actor). `lane` is the resolved executable lane.
// `prompt` is the worker prompt the wire forwards; it lands in the
// ActionIntent's Preview field verbatim so audit readers can see what
// was actually launched.
func (s *Store) MintLaunchAuthority(workID, actor, lane, prompt string, now time.Time) (actionIntentID, policyDecisionID string, err error) {
	workID = strings.TrimSpace(workID)
	actor = strings.TrimSpace(actor)
	lane = strings.TrimSpace(lane)
	if workID == "" {
		return "", "", errors.New("MintLaunchAuthority: workID is required")
	}
	if actor == "" {
		return "", "", errors.New("MintLaunchAuthority: actor is required")
	}
	if lane == "" {
		return "", "", errors.New("MintLaunchAuthority: lane is required")
	}
	var minted envelope.WorkEnvelope
	txErr := s.withLifecycleMutation(func() error {
		w, loadErr := s.Get(workID)
		if loadErr != nil {
			return fmt.Errorf("MintLaunchAuthority: load work %s: %w", workID, loadErr)
		}
		if !strings.HasPrefix(w.State, "claimed") && w.State != "running" && w.State != "verifying" {
			return fmt.Errorf("MintLaunchAuthority: work %s is %s; refuse to mint launch authority outside claimed/running/verifying", workID, w.State)
		}
		// Freeze the running-reservation identity in scope-captured locals so
		// the AI's Scope.ProductID and the Authority.Owner reflect the
		// generation the launch bound to.
		createdAt := now.UTC().Format(time.RFC3339)
		actionID := idFor("act_", "launch", workID, lane, actor, ts(now))
		ai := envelope.ActionIntent{
			Header: envelope.Header{
				SchemaVersion: envelope.SVActionIntent,
				ID:            actionID,
				CreatedAt:     createdAt,
				Producer:      envelope.Producer{Kind: "cli", Name: "compound", LaneID: lane},
				IDs:           envelope.IDs{WorkID: workID},
				Scope:         envelope.Scope{},
				Authority:     envelope.Authority{SourceClass: "runtime", Owner: actor, Status: "current"},
				Privacy:       stringOrDefault(w.Privacy, "private"),
				Risk:          stringOrDefault(w.Risk, "tier1"),
				EvidenceRefs:  nil,
				Uncertainty:   envelope.Uncertainty{Level: "known"},
			},
			Actor:                  actor,
			Capability:             executeUnattendedCapability,
			Action:                 "execute_unattended",
			Target:                 envelope.ActionTarget{Kind: "work", ID: workID, Path: filepath.Join(s.home, "work", "work", workID+".json")},
			MutationClass:          "local_reversible",
			Reversibility:          "easy",
			BlastRadius:            "local",
			Preview:                "execute work " + workID + " unattended on lane " + lane,
			RollbackPlan:           "advance work back to ready if the launch produced no side effects",
			ArgumentsHash:          "",
			CapturesWorkInProgress: w.GitDelivery != nil,
		}
		if strings.TrimSpace(prompt) != "" {
			ai.Preview = ai.Preview + " — prompt: " + truncateForPreview(prompt, 256)
		}
		if err := ai.Validate(); err != nil {
			return fmt.Errorf("MintLaunchAuthority: ActionIntent %s invalid: %w", actionID, err)
		}
		policyID := idFor("pol_", "launch", actionID, actor, ts(now))
		// Slice 2 (aut-launch-canonical 2026-09-07): evaluate the freshly
		// minted ActionIntent through the canonical policy evaluator so
		// the PolicyDecision records the OBLIGATIONS and reasons the
		// evaluator actually derived for THIS action — not a fabricated
		// "standing grant covers it" string. The standing-grant id, when
		// matched, becomes evidence on the decision; when no grant is
		// matched, the baseline outcome / obligations / reasons stand on
		// their own. The exact (ActionIntentID, PolicyDecisionID) pair
		// rides the verifying transition regardless of which path chose it.
		cfg, _, cfgErr := policy.LoadConfig(s.home)
		if cfgErr != nil {
			return fmt.Errorf("MintLaunchAuthority: load policy config: %w", cfgErr)
		}
		evaluated := policy.Evaluate(ai, cfg, now)
		pd := envelope.PolicyDecision{
			Header: envelope.Header{
				SchemaVersion: envelope.SVPolicyDecision,
				ID:            policyID,
				CreatedAt:     createdAt,
				Producer:      envelope.Producer{Kind: "cli", Name: "compound", LaneID: lane},
				IDs:           envelope.IDs{WorkID: workID},
				Scope:         envelope.Scope{},
				Authority:     envelope.Authority{SourceClass: "runtime", Owner: actor, Status: "current"},
				Privacy:       ai.Privacy,
				Risk:          ai.Risk,
				EvidenceRefs:  nil,
				Uncertainty:   envelope.Uncertainty{Level: "known"},
			},
			ActionIntentID:  actionID,
			Outcome:         evaluated.Outcome,
			Reasons:         append([]string(nil), evaluated.Reasons...),
			Obligations:     append([]string(nil), evaluated.Obligations...),
			StandingGrantID: evaluated.StandingGrantID,
			ExpiresAt:       evaluated.ExpiresAt,
		}
		// Allow-with-obligations MUST carry at least one obligation; if
		// the evaluator produced an empty obligations slice under that
		// outcome, the launch has no recorded duty and we MUST refuse
		// rather than mint a hollow allow.
		if pd.Outcome == "allow_with_obligations" && len(pd.Obligations) == 0 {
			return fmt.Errorf("MintLaunchAuthority: policy.Evaluate returned %s with no obligations for action %s", pd.Outcome, actionID)
		}
		if pd.Outcome == "deny" || pd.Outcome == "hold" {
			return fmt.Errorf("MintLaunchAuthority: policy.Evaluate refused action %s (%s): %v", actionID, pd.Outcome, pd.Reasons)
		}
		if err := pd.Validate(); err != nil {
			return fmt.Errorf("MintLaunchAuthority: PolicyDecision %s invalid: %w", policyID, err)
		}
		// Persist the (ai, pd) pair first: if the wenv rewrite fails, the
		// next reconcile can re-stamp the wenv from the (ai, pd) on disk.
		if err := s.writeEventJSON(actionID, &ai); err != nil {
			return fmt.Errorf("MintLaunchAuthority: persist ActionIntent %s: %w", actionID, err)
		}
		if err := s.writeEventJSON(policyID, &pd); err != nil {
			return fmt.Errorf("MintLaunchAuthority: persist PolicyDecision %s: %w", policyID, err)
		}
		// Stamp the new pair onto the running reservation BEFORE the
		// adapter mint so the wenv carries the ids the verifying
		// transition compares against. Order matters: this is a wenv
		// mutation that must land before the wire calls RunInput so the
		// close-time guard sees the SAME ids.
		previousWorkSHA256, shaErr := workEnvelopeSHA256(w)
		if shaErr != nil {
			return fmt.Errorf("MintLaunchAuthority: seal work %s before mint: %w", workID, shaErr)
		}
		w.ActionIntentIDs = append(w.ActionIntentIDs, actionID)
		w.PolicyDecisionID = policyID
		if err := w.Validate(); err != nil {
			return fmt.Errorf("MintLaunchAuthority: refusing to persist invalid work %s: %w", workID, err)
		}
		workSHA256, shaErr := workEnvelopeSHA256(w)
		if shaErr != nil {
			return fmt.Errorf("MintLaunchAuthority: seal work %s after mint: %w", workID, shaErr)
		}
		if err := s.writeJSON(s.workPath(workID), &w); err != nil {
			return fmt.Errorf("MintLaunchAuthority: rewrite work %s: %w", workID, err)
		}
		// Slice 2 (aut-launch-canonical 2026-09-07): the mint IS a lifecycle
		// mutation, so it is recorded as a sealed same-state event attaching
		// the exact (ActionIntent, PolicyDecision) pair. Two consumers depend
		// on it: the custody chain (every envelope writer re-seals, or the
		// newest seal describes stale bytes) and the close-time authority
		// gate, which binds an attempt that carries only its ActionIntent —
		// every non-Claude adapter, whose RunInput gets no execution posture
		// — to this generation by scanning the events that attached the
		// pair. Without the event a failed unattended launch could not be
		// closed ("authority generation is missing") and the envelope stayed
		// running with an unrecorded attempt.
		if err := s.appendEvent(Event{
			TS:                 ts(now),
			WorkID:             workID,
			From:               w.State,
			To:                 w.State,
			By:                 actor,
			Note:               "launch authority minted for lane " + lane + " (policy " + pd.Outcome + ")",
			Attached:           []string{actionID, policyID},
			PreviousWorkSHA256: previousWorkSHA256,
			WorkSHA256:         workSHA256,
		}); err != nil {
			return fmt.Errorf("MintLaunchAuthority: record launch authority event for %s: %w", workID, err)
		}
		minted = w
		actionIntentID = actionID
		policyDecisionID = policyID
		return nil
	})
	_ = minted // reserved for future terminalization-intent binding
	if txErr != nil {
		return "", "", txErr
	}
	return actionIntentID, policyDecisionID, nil
}

func stringOrDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func truncateForPreview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (s *Store) Get(workID string) (envelope.WorkEnvelope, error) {
	var w envelope.WorkEnvelope
	err := s.readJSON(s.workPath(workID), &w)
	if errors.Is(err, os.ErrNotExist) {
		if archivedPath, ok := s.archivedWorkPath(workID); ok {
			err = s.readJSON(archivedPath, &w)
		}
	}
	return w, err
}

// getWithArchiveIndex is Get for batch callers that already loaded the
// operational archive index once (e.g. the workbench report build) and must
// not trigger another LoadOperationalArchiveIndex per work item.
func (s *Store) getWithArchiveIndex(workID string, archiveIndex []home.OperationalArchiveEntry) (envelope.WorkEnvelope, error) {
	var w envelope.WorkEnvelope
	err := s.readJSON(s.workPath(workID), &w)
	if errors.Is(err, os.ErrNotExist) {
		if archivedPath, ok := s.archivedWorkPathWithIndex(workID, archiveIndex); ok {
			err = s.readJSON(archivedPath, &w)
		}
	}
	return w, err
}

// Show returns a work and its lifecycle log (oldest first).
func (s *Store) Show(workID string) (envelope.WorkEnvelope, []Event, error) {
	w, err := s.Get(workID)
	if err != nil {
		return w, nil, err
	}
	evs, err := s.eventsFor(workID)
	return w, evs, err
}

func (s *Store) List() ([]envelope.WorkEnvelope, error) {
	out, _, err := s.list(false)
	return out, err
}

func (s *Store) ListWithArchived() ([]envelope.WorkEnvelope, error) {
	out, _, err := s.list(true)
	return out, err
}

// list reads every work/work/<id>.json (plus archived entries when
// includeArchived is set) and returns the parsed envelopes alongside one
// integrity warning per item that failed to parse. A parse failure never
// aborts the read (callers like Workbench build a degrade-and-report
// projection over a directory of independently-written files, so one
// corrupt item must not blank the rest) but it must not vanish either: the
// old shape (`if s.readJSON(...) == nil { append }`) silently dropped
// corrupt items from every downstream count with no trace. The warning
// slice is the "we could not read N items" channel List/ListWithArchived
// callers don't need; Workbench threads it into report.Integrity, the
// existing degradation channel, rather than inventing a parallel one.
func (s *Store) list(includeArchived bool) ([]envelope.WorkEnvelope, []string, error) {
	dir := filepath.Join(s.home, "work/work")
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing work/work dir reads as "no work" — the contract
		// OpenReadOnly's doc states for the read methods it serves, and the
		// precedent ListLeases already sets: a fresh or stripped home projects
		// empty, never an error, and never a mkdir.
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var out []envelope.WorkEnvelope
	var warnings []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var w envelope.WorkEnvelope
		if err := s.readJSON(filepath.Join(dir, e.Name()), &w); err != nil {
			warnings = append(warnings, fmt.Sprintf("work/work/%s unreadable: %s", e.Name(), err.Error()))
			continue
		}
		out = append(out, w)
	}
	if includeArchived {
		archived, archivedWarnings, err := s.archivedWorks()
		if err != nil {
			return nil, nil, err
		}
		out = append(out, archived...)
		warnings = append(warnings, archivedWarnings...)
	}
	return out, warnings, nil
}

func (s *Store) archivedWorks() ([]envelope.WorkEnvelope, []string, error) {
	entries, err := home.LoadOperationalArchiveIndex(s.home)
	if err != nil {
		return nil, nil, err
	}
	var out []envelope.WorkEnvelope
	var warnings []string
	for _, entry := range entries {
		if entry.Kind != "work" || !strings.HasPrefix(entry.OriginalPath, "work/work/") || !strings.HasSuffix(entry.OriginalPath, ".json") {
			continue
		}
		var w envelope.WorkEnvelope
		if err := s.readJSON(filepath.Join(s.home, filepath.FromSlash(entry.ArchivePath)), &w); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s unreadable: %s", entry.OriginalPath, err.Error()))
			continue
		}
		out = append(out, w)
	}
	return out, warnings, nil
}

func (s *Store) archivedWorkPath(workID string) (string, bool) {
	entries, err := home.LoadOperationalArchiveIndex(s.home)
	if err != nil {
		return "", false
	}
	return s.archivedWorkPathWithIndex(workID, entries)
}

// archivedWorkPathWithIndex is archivedWorkPath against a caller-supplied,
// already-loaded archive index instead of re-reading it from disk.
func (s *Store) archivedWorkPathWithIndex(workID string, entries []home.OperationalArchiveEntry) (string, bool) {
	want := filepath.ToSlash(filepath.Join("work", "work", workID+".json"))
	for _, entry := range entries {
		if entry.Kind == "work" && entry.OriginalPath == want {
			return filepath.Join(s.home, filepath.FromSlash(entry.ArchivePath)), true
		}
	}
	return "", false
}

// --- claim ---

// Claim moves a ready work to claimed only after policy allows the claim
// ActionIntent, then writes the claim envelopes and lease. A second claimant is
// rejected (claim-before-you-touch prevents parallel agents colliding on the
// same surface).
func (s *Store) Claim(workID, by string, now time.Time) (envelope.WorkEnvelope, error) {
	return s.ClaimWithLane(workID, by, "", now)
}

// claimExpected shares Claim's transaction but refuses a stale dispatch source
// before claim recovery, lease publication, action/policy writes or RunAttempts.
func (s *Store) claimExpected(workID, by, state, policyID, actionID string, now time.Time) (envelope.WorkEnvelope, error) {
	var out envelope.WorkEnvelope
	err := s.withLifecycleMutation(func() error {
		w, err := s.Get(workID)
		if err != nil {
			return err
		}
		if err := validateExpectedExecutionReservation(w, state, policyID, actionID); err != nil {
			return err
		}
		out, err = s.claimLocked(workID, by, "", now)
		return err
	})
	return out, err
}

// ClaimWithLane claims a work and, when laneRef is non-empty, declares its lane: the ref must
// resolve via capgraph.ResolveLaneRef (fail-closed at the gate, before any state mutation) and
// the resolved canonical node id is recorded on the lease. Claim is the laneRef=="" case; every
// existing Claim caller is byte-identical.
func (s *Store) ClaimWithLane(workID, by, laneRef string, now time.Time) (envelope.WorkEnvelope, error) {
	laneID, err := s.resolveDeclaredLane(laneRef)
	if err != nil {
		w, _ := s.Get(workID)
		return w, err
	}
	var out envelope.WorkEnvelope
	var claimErr error
	err = s.withLifecycleMutation(func() error {
		out, claimErr = s.claimLocked(workID, by, laneID, now)
		return claimErr
	})
	return out, err
}

// resolveDeclaredLane maps a declared --lane ref to its canonical agent-lane node id via the
// single no-guessing resolver (capgraph.ResolveLaneRef). Empty ref → ("", nil): no declaration.
// A non-empty ref that does not resolve is a fail-closed error naming the valid ref forms, so
// typos and bare-harness/actor guesses die at the write gate, never in the belief fold.
func (s *Store) resolveDeclaredLane(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	cg, err := capgraph.Open(s.home)
	if err != nil {
		return "", fmt.Errorf("resolve --lane %q: %w", ref, err)
	}
	nodeID, ok := cg.ResolveLaneRef(ref)
	if !ok {
		return "", fmt.Errorf("--lane %q does not resolve to an imported agent-lane node; pass an agent-lane node id or a short lane id (e.g. provider/model), not a bare harness or an actor string", ref)
	}
	return nodeID, nil
}

// recordDeclaredLaneOnLease writes the resolved canonical lane id onto workID's existing lease.
// The caller has already passed the holder-scoped advance gate, so this only reaches a lease the
// acting holder is entitled to mutate. An absent lease is an error: a lane can only be declared
// on a work that is currently claimed.
func (s *Store) recordDeclaredLaneOnLease(workID, laneID string) error {
	lease, err := s.readLease(workID)
	if err != nil {
		return fmt.Errorf("advance --lane requires a claimed work with a live lease: %w", err)
	}
	if strings.TrimSpace(lease.By) == "" {
		return fmt.Errorf("advance --lane requires a claimed work (no lease holder)")
	}
	lease.LaneID = laneID
	return s.publishClaimJSON(s.leasePath(workID), &lease)
}

func (s *Store) claimLocked(workID, by, laneID string, now time.Time) (envelope.WorkEnvelope, error) {
	if err := s.fenceClosePendingMutation(workID); err != nil {
		return envelope.WorkEnvelope{}, err
	}
	if err := s.fenceSweepStalePendingMutation(workID); err != nil {
		return envelope.WorkEnvelope{}, err
	}
	w, err := s.Get(workID)
	if err != nil {
		return w, err
	}
	by = strings.TrimSpace(by)
	if by == "" {
		return w, fmt.Errorf("claim requires --by <actor>")
	}
	if recovered, ok, err := s.recoverPendingClaim(w); err != nil {
		return w, err
	} else if ok {
		holder := recovered.Authority.Owner
		if holder != by {
			return recovered, fmt.Errorf("work %s already claimed by %q", workID, holder)
		}
		return recovered, nil
	}
	from := w.State
	repairOwnerless := false
	if w.State == "claimed" {
		repairFrom, repair, err := s.ownerlessClaimRepairSource(w)
		if err != nil {
			return w, fmt.Errorf("work %s inspect ownerless-claim repair: %w", workID, err)
		}
		if repair {
			from = repairFrom
			repairOwnerless = true
		} else {
			holder, err := s.validateClaimAuthority(w, false)
			if err != nil {
				return w, fmt.Errorf("work %s claimed authority is inconsistent: %w", workID, err)
			}
			if holder != by {
				return w, fmt.Errorf("work %s already claimed by %q", workID, holder)
			}
			return w, nil
		}
	}
	if !repairOwnerless && !canTransition(w.State, "claimed") {
		return w, fmt.Errorf("cannot claim from state %q (must be ready)", w.State)
	}
	plan, err := s.ClaimPlan(workID, now, ClaimPlanOptions{Actor: by})
	if err != nil {
		return w, err
	}
	if !plan.Policy.Allowed {
		return w, fmt.Errorf("cannot claim work %s: policy %s (decision %s)", workID, plan.Policy.Outcome, plan.Policy.PolicyDecisionID)
	}
	previousWorkSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		return w, err
	}
	w.State = "claimed"
	w.Authority.Owner = plan.Actor
	w.ActionIntentIDs = append(w.ActionIntentIDs, plan.ActionIntent.ID)
	w.PolicyDecisionID = plan.Decision.ID
	if err := s.writeEventJSON(plan.ActionIntent.ID, &plan.ActionIntent); err != nil {
		return w, err
	}
	if err := s.writeEventJSON(plan.Decision.ID, &plan.Decision); err != nil {
		return w, err
	}
	// C1: mint a RunAttempt on every claim so the execution organ fires in
	// production. The attempt is minted BEFORE the claimed-work hash is
	// computed so the ratt_ id is part of the canonical envelope bytes.
	// Harness is the resolved lane id when declared, else the placeholder.
	raHarness := laneID
	if raHarness == "" {
		raHarness = runattempt.HarnessPlaceholder
	}
	ra, raErr := s.runAttemptsClaim().Mint(workID, plan.Actor, raHarness, now)
	if raErr != nil {
		return w, fmt.Errorf("claim %s: mint run attempt: %w", workID, raErr)
	}
	// F2: stamp provenance -- this is a claim-time placeholder reservation,
	// not a real execution. supersede-at-attach (in Advance) reads this to
	// terminalize the reservation once a harness_execution attempt attaches.
	//
	// Audit invariant (E14 twin): on a SetOrigin failure, the freshly
	// minted reservation attempt MUST be terminalized as `failed` with a
	// reason naming the origin-stamp failure -- never left `pending`. The
	// same invariant is enforced on the harness-execution lane by
	// subprocess_runner.go (slice/audit-e14-setorigin-20260724). A pending
	// ratt_ record stranded here would either be swept-as-stale by the
	// C3 sweeper (mislabeling a real infra failure as "no heartbeat"),
	// or worse, attach to a later envelope and propagate as a phantom
	// provenance. If the terminalization ITSELF fails, best-effort: we
	// return the original SetOrigin error (do not mask it).
	if stamped, oErr := s.runAttemptsClaim().SetOrigin(ra.ID, runattempt.OriginClaimReservation); oErr != nil {
		if _, tErr := s.runAttemptsClaim().CompleteWithReason(ra.ID, runattempt.StatusFailed, "stamp origin: "+oErr.Error(), now); tErr != nil {
			// best-effort: the terminalization failure is not reported --
			// the original SetOrigin error is the truth we want to surface.
			_ = tErr
		}
		return w, fmt.Errorf("claim %s: stamp run attempt origin: %w", workID, oErr)
	} else {
		ra = stamped
	}
	// C4: retry lineage — if this work already has attempts, the new one
	// chains to the most recent. Best-effort: a link failure does not abort
	// the claim (the attempt exists, lineage is evidence, not enforcement).
	if len(w.RunAttemptIDs) > 0 {
		prev := w.RunAttemptIDs[len(w.RunAttemptIDs)-1]
		if _, linkErr := s.runAttemptsClaim().SetPredecessor(ra.ID, prev); linkErr != nil {
			// Best-effort: log nothing, the claim proceeds.
			_ = linkErr
		}
	}
	w.RunAttemptIDs = append(w.RunAttemptIDs, ra.ID)
	claimedWorkSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		return w, err
	}
	lease := Lease{WorkID: workID, By: plan.Actor, ClaimAt: ts(now), LaneID: laneID, WorkSHA256: claimedWorkSHA256}
	note := "claimed (policy " + plan.Policy.Outcome + ")"
	// Repair is a claimed-to-claimed same-state advance that retroactively
	// binds authority and proof to an ownerless wedge. The lifecycle
	// From/To fields must agree (a same-state binding), not borrow the
	// wedge's source state, and the note must carry the repair marker so
	// validatePendingClaim's RepairOwnerless<->note parity holds.
	eventFrom := from
	if repairOwnerless {
		eventFrom = "claimed"
		note = "ownerless-claim repair (policy " + plan.Policy.Outcome + ")"
	}
	event := Event{
		TS:                 lease.ClaimAt,
		WorkID:             workID,
		From:               eventFrom,
		To:                 "claimed",
		By:                 plan.Actor,
		Note:               note,
		Attached:           []string{plan.ActionIntent.ID, plan.Decision.ID, ra.ID},
		PreviousWorkSHA256: previousWorkSHA256,
		WorkSHA256:         claimedWorkSHA256,
	}
	pending := claimPending{
		SchemaVersion:      claimPendingSchemaVersion,
		WorkID:             workID,
		Actor:              plan.Actor,
		PreviousWorkSHA256: previousWorkSHA256,
		ClaimedWork:        w,
		ClaimedWorkSHA256:  claimedWorkSHA256,
		Lease:              lease,
		ActionIntentID:     plan.ActionIntent.ID,
		PolicyDecisionID:   plan.Decision.ID,
		RunAttemptID:       ra.ID,
		RepairOwnerless:    repairOwnerless,
		Event:              event,
	}
	if err := s.validatePendingClaim(pending); err != nil {
		return w, err
	}
	pendingPath := s.claimPendingPath(workID)
	stagedPending, err := s.stageJSON(pendingPath, &pending)
	if err != nil {
		return w, err
	}
	if err := commitStagedJSONExclusive(stagedPending, pendingPath); err != nil {
		_ = os.Remove(stagedPending)
		return w, err
	}
	return s.completePendingClaim(pending, true)
}

// ownerlessClaimRepairSource recognizes only the historical wedge minted by a bare
// advance: claimed state, no authority owner, no lease, and a current-hash-bound
// lifecycle tail that crossed a legal state-machine edge into claimed. The returned
// source state lets the repair claim append a historically replayable claim event.
func (s *Store) ownerlessClaimRepairSource(w envelope.WorkEnvelope) (string, bool, error) {
	if w.State != "claimed" || strings.TrimSpace(w.Authority.Owner) != "" {
		return "", false, nil
	}
	if _, err := s.readLease(w.ID); err == nil {
		return "", false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	workSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		return "", false, err
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return "", false, err
	}
	if len(events) == 0 {
		return "", false, nil
	}
	tail := events[len(events)-1]
	if tail.To != "claimed" || !canTransition(tail.From, "claimed") || tail.WorkSHA256 != workSHA256 {
		return "", false, nil
	}
	return tail.From, true, nil
}

// These synchronization seams model process death after each authoritative
// file publication. Production leaves them as no-ops.
var claimAfterWorkPublish = func() {}
var claimAfterLeasePublish = func() {}

func (s *Store) claimPendingPath(workID string) string {
	return s.workPath(workID) + ".claim-pending"
}

func (s *Store) recoverPendingClaim(current envelope.WorkEnvelope) (envelope.WorkEnvelope, bool, error) {
	var pending claimPending
	if err := s.readJSON(s.claimPendingPath(current.ID), &pending); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return current, false, nil
		}
		return current, false, fmt.Errorf("read pending claim for %s: %w", current.ID, err)
	}
	if err := s.validatePendingClaim(pending); err != nil {
		return current, false, fmt.Errorf("pending claim for %s invalid: %w", current.ID, err)
	}
	currentSHA256, err := workEnvelopeSHA256(current)
	if err != nil {
		return current, false, err
	}
	if currentSHA256 != pending.PreviousWorkSHA256 && currentSHA256 != pending.ClaimedWorkSHA256 {
		return current, false, fmt.Errorf("pending claim for %s matches neither current nor claimed WorkEnvelope", current.ID)
	}
	recovered, err := s.completePendingClaim(pending, false)
	if err != nil {
		return current, false, err
	}
	return recovered, true, nil
}

func (s *Store) recoverPendingClaimBeforeMutation(workID string) (envelope.WorkEnvelope, error) {
	return s.recoverPendingClaimBeforeLifecycleMutation(workID, false)
}

// recoverPendingClaimBeforeClose permits only the exact Close retry to enter
// while a durable RunAttempt terminalization reservation exists. Every other
// lifecycle mutation is held by recoverPendingClaimBeforeMutation so a crash
// between prepare and terminal work publication cannot be bypassed by an
// unrelated advance, claim, or close generation.
func (s *Store) recoverPendingClaimBeforeClose(workID string) (envelope.WorkEnvelope, error) {
	return s.recoverPendingClaimBeforeLifecycleMutation(workID, true)
}

func (s *Store) recoverPendingClaimBeforeLifecycleMutation(workID string, allowPreparedClose bool) (envelope.WorkEnvelope, error) {
	if err := s.fenceSweepStalePendingMutation(workID); err != nil {
		return envelope.WorkEnvelope{}, err
	}
	if !allowPreparedClose {
		if err := s.fenceClosePendingMutation(workID); err != nil {
			return envelope.WorkEnvelope{}, err
		}
	}
	current, err := s.Get(workID)
	if err != nil {
		return current, err
	}
	recovered, ok, err := s.recoverPendingClaim(current)
	if err != nil {
		return current, err
	}
	if ok {
		current = recovered
	}
	if current.State == "claimed" {
		if _, err := s.validateClaimAuthority(current, true); err != nil {
			return current, fmt.Errorf("work %s claimed authority is inconsistent: %w", workID, err)
		}
	}
	if !allowPreparedClose && !terminalStates[current.State] {
		intent, err := s.preparedCloseIntentForWork(workID)
		if err != nil {
			return current, err
		}
		if intent != nil {
			return current, fmt.Errorf("work %s has prepared close %s/%s from %s; retry the exact work close before another lifecycle mutation", workID, intent.CloseActionIntentID, intent.ClosePolicyDecisionID, intent.PreparedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	return current, nil
}

func (s *Store) preparedCloseIntentForWork(workID string) (*runattempt.TerminalizationIntent, error) {
	attempts, err := s.runAttempts().ForWork(workID)
	if err != nil {
		return nil, fmt.Errorf("scan prepared close for work %s: %w", workID, err)
	}
	var found *runattempt.TerminalizationIntent
	for _, ra := range attempts {
		if ra.WorkID != workID || ra.TerminalizationIntent == nil {
			continue
		}
		intent := *ra.TerminalizationIntent
		if err := intent.Validate(); err != nil {
			return nil, fmt.Errorf("prepared close on runattempt %s is invalid: %w", ra.ID, err)
		}
		if found == nil {
			copyIntent := intent
			found = &copyIntent
			continue
		}
		if !samePreparedCloseGeneration(*found, intent) {
			return nil, fmt.Errorf("work %s has conflicting prepared close generations", workID)
		}
	}
	return found, nil
}

func (s *Store) completePendingClaim(pending claimPending, runCrashSeams bool) (envelope.WorkEnvelope, error) {
	if err := s.validatePendingClaim(pending); err != nil {
		return pending.ClaimedWork, fmt.Errorf("pending claim for %s invalid: %w", pending.WorkID, err)
	}
	err := s.withWorkEventLock(func() error {
		if err := s.ensureExactClaimEventUnlocked(pending); err != nil {
			return err
		}

		current, err := s.Get(pending.WorkID)
		if err != nil {
			return err
		}
		currentSHA256, err := workEnvelopeSHA256(current)
		if err != nil {
			return err
		}
		switch currentSHA256 {
		case pending.PreviousWorkSHA256:
			if pending.RepairOwnerless {
				if current.State != "claimed" || strings.TrimSpace(current.Authority.Owner) != "" {
					return fmt.Errorf("pending ownerless-claim repair for %s no longer matches an ownerless claimed WorkEnvelope", pending.WorkID)
				}
			} else if current.State != pending.Event.From {
				return fmt.Errorf("pending claim for %s previous state is %q, want %q", pending.WorkID, current.State, pending.Event.From)
			}
			if err := s.publishClaimJSON(s.workPath(pending.WorkID), &pending.ClaimedWork); err != nil {
				return err
			}
		case pending.ClaimedWorkSHA256:
			if !reflect.DeepEqual(current, pending.ClaimedWork) {
				return fmt.Errorf("pending claim for %s claimed WorkEnvelope bytes disagree", pending.WorkID)
			}
		default:
			return fmt.Errorf("pending claim for %s conflicts with current WorkEnvelope", pending.WorkID)
		}
		if runCrashSeams {
			claimAfterWorkPublish()
		}

		lease, leaseErr := s.readLease(pending.WorkID)
		if leaseErr != nil || !reflect.DeepEqual(lease, pending.Lease) {
			if err := s.publishClaimJSON(s.leasePath(pending.WorkID), &pending.Lease); err != nil {
				return err
			}
		}
		lease, err = s.readLease(pending.WorkID)
		if err != nil {
			return fmt.Errorf("read published claim lease for %s: %w", pending.WorkID, err)
		}
		if !reflect.DeepEqual(lease, pending.Lease) {
			return fmt.Errorf("published claim lease for %s does not match pending transaction", pending.WorkID)
		}
		if runCrashSeams {
			claimAfterLeasePublish()
		}
		if err := removeFileAndSyncDir(s.claimPendingPath(pending.WorkID)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return pending.ClaimedWork, err
	}
	return pending.ClaimedWork, nil
}

func (s *Store) ensureExactClaimEventUnlocked(pending claimPending) error {
	events, err := s.readEventsStrictUnlocked(pending.WorkID)
	if err != nil {
		return err
	}
	exact := 0
	for _, event := range events {
		if event.To != "claimed" || event.WorkSHA256 != pending.ClaimedWorkSHA256 {
			continue
		}
		if !reflect.DeepEqual(event, pending.Event) {
			return fmt.Errorf("pending claim for %s conflicts with durable claim event", pending.WorkID)
		}
		exact++
	}
	if exact > 1 {
		return fmt.Errorf("pending claim for %s has %d exact durable events, want one", pending.WorkID, exact)
	}
	if exact == 0 {
		before := len(events)
		if err := s.appendEventControlledUnlocked(pending.Event, nil, nil); err != nil {
			return err
		}
		events, err = s.readEventsStrictUnlocked(pending.WorkID)
		if err != nil {
			return err
		}
		if len(events) != before+1 || !reflect.DeepEqual(events[len(events)-1], pending.Event) {
			return fmt.Errorf("work event log did not preserve the exact claim event for %s", pending.WorkID)
		}
	}
	return nil
}

func (s *Store) publishClaimJSON(path string, value any) error {
	staged, err := s.stageJSON(path, value)
	if err != nil {
		return err
	}
	if err := commitStagedJSON(staged, path); err != nil {
		_ = os.Remove(staged)
		return err
	}
	return nil
}

func (s *Store) validatePendingClaim(pending claimPending) error {
	if pending.SchemaVersion != claimPendingSchemaVersion {
		return fmt.Errorf("schema_version must be %q", claimPendingSchemaVersion)
	}
	if strings.TrimSpace(pending.WorkID) == "" || pending.ClaimedWork.ID != pending.WorkID {
		return fmt.Errorf("pending Work identity mismatch")
	}
	if strings.TrimSpace(pending.Actor) == "" || pending.ClaimedWork.State != "claimed" || pending.ClaimedWork.Authority.Owner != pending.Actor {
		return fmt.Errorf("pending claimed Work owner/state mismatch")
	}
	if err := pending.ClaimedWork.Validate(); err != nil {
		return fmt.Errorf("claimed WorkEnvelope invalid: %w", err)
	}
	claimedSHA256, err := workEnvelopeSHA256(pending.ClaimedWork)
	if err != nil {
		return err
	}
	if strings.TrimSpace(pending.PreviousWorkSHA256) == "" || pending.ClaimedWorkSHA256 != claimedSHA256 {
		return fmt.Errorf("pending WorkEnvelope hashes do not bind the claimed Work")
	}
	if pending.Lease.WorkID != pending.WorkID || pending.Lease.By != pending.Actor || strings.TrimSpace(pending.Lease.ClaimAt) == "" ||
		pending.Lease.WorkSHA256 != pending.ClaimedWorkSHA256 {
		return fmt.Errorf("pending lease identity mismatch")
	}
	if strings.TrimSpace(pending.ActionIntentID) == "" || strings.TrimSpace(pending.PolicyDecisionID) == "" {
		return fmt.Errorf("pending claim must bind action and policy ids")
	}
	event := pending.Event
	// C1: the Attached list carries [action, policy] for pre-C1 pending
	// files and [action, policy, ratt_*] for C1+ files. Validate both
	// shapes so crash recovery works across the boundary.
	wantAttached := []string{pending.ActionIntentID, pending.PolicyDecisionID}
	if pending.RunAttemptID != "" {
		wantAttached = append(wantAttached, pending.RunAttemptID)
	}
	if event.WorkID != pending.WorkID || !claimEventFromIsLegal(event.From, pending.RepairOwnerless) || event.To != "claimed" || event.By != pending.Actor ||
		event.TS != pending.Lease.ClaimAt || event.PreviousWorkSHA256 != pending.PreviousWorkSHA256 || event.WorkSHA256 != pending.ClaimedWorkSHA256 ||
		!reflect.DeepEqual(event.Attached, wantAttached) {
		return fmt.Errorf("pending lifecycle event does not bind exact claim transaction")
	}
	if pending.RepairOwnerless != strings.HasPrefix(event.Note, "ownerless-claim repair (policy ") {
		return fmt.Errorf("pending ownerless-claim repair marker does not match lifecycle event")
	}
	if len(pending.ClaimedWork.ActionIntentIDs) == 0 ||
		pending.ClaimedWork.ActionIntentIDs[len(pending.ClaimedWork.ActionIntentIDs)-1] != pending.ActionIntentID ||
		pending.ClaimedWork.PolicyDecisionID != pending.PolicyDecisionID {
		return fmt.Errorf("pending claimed Work does not bind exact action and policy")
	}
	// RunAttemptID binding: the ratt_ the claim mints must land on the
	// envelope's RunAttemptIDs. A missing/mismatched ratt_ dimension is a
	// refused claim -- validate before any authoritative publication.
	if pending.RunAttemptID != "" {
		if !oneOf(pending.RunAttemptID, pending.ClaimedWork.RunAttemptIDs) {
			return fmt.Errorf("pending claimed Work does not bind its claim RunAttempt %s", pending.RunAttemptID)
		}
		if event.Attached[2] != pending.RunAttemptID {
			return fmt.Errorf("pending repair RunAttempt %s does not match event attachment", pending.RunAttemptID)
		}
	}
	return s.validateClaimEventArtifacts(event, pending.ClaimedWork)
}

func (s *Store) validateClaimAuthority(claimed envelope.WorkEnvelope, allowLegacy bool) (string, error) {
	if err := claimed.Validate(); err != nil {
		return "", fmt.Errorf("claimed WorkEnvelope invalid: %w", err)
	}
	holder := strings.TrimSpace(claimed.Authority.Owner)
	if claimed.State != "claimed" || holder == "" {
		return "", fmt.Errorf("claimed WorkEnvelope has no exact authority owner")
	}
	lease, err := s.readLease(claimed.ID)
	if err != nil {
		return "", fmt.Errorf("read exact claim lease: %w", err)
	}
	events, err := s.eventsFor(claimed.ID)
	if err != nil {
		return "", err
	}
	matches := make([]Event, 0, 1)
	if strings.TrimSpace(lease.WorkSHA256) == "" {
		if !allowLegacy {
			return "", fmt.Errorf("lease does not bind a claimed WorkEnvelope hash")
		}
		for _, event := range events {
			if event.To == "claimed" && canTransition(event.From, "claimed") && event.TS == lease.ClaimAt &&
				event.WorkSHA256 == "" && event.PreviousWorkSHA256 == "" {
				matches = append(matches, event)
			}
		}
	} else {
		for _, event := range events {
			if event.To == "claimed" && event.WorkSHA256 == lease.WorkSHA256 {
				matches = append(matches, event)
			}
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("found %d durable events for current claimed WorkEnvelope, want exactly one", len(matches))
	}
	event := matches[0]
	repairFrom := strings.HasPrefix(event.Note, "ownerless-claim repair (policy ")
	if event.WorkID != claimed.ID || !claimEventFromIsLegal(event.From, repairFrom) ||
		event.TS != lease.ClaimAt || event.By != holder {
		return "", fmt.Errorf("durable claim event identity mismatch")
	}
	if lease.WorkSHA256 != "" && strings.TrimSpace(event.PreviousWorkSHA256) == "" {
		return "", fmt.Errorf("durable claim event does not bind the previous WorkEnvelope")
	}
	if lease.WorkID != claimed.ID || lease.By != holder {
		return "", fmt.Errorf("lease does not agree with claimed Work and event")
	}
	if err := s.validateClaimEventArtifacts(event, claimed); err != nil {
		return "", err
	}
	return holder, nil
}

func (s *Store) validateClaimEventArtifacts(event Event, claimed envelope.WorkEnvelope) error {
	// C1: claim events carry [action, policy] (pre-C1) or [action, policy,
	// ratt_*] (C1+). The first two are validated as artifacts; the third is
	// a RunAttempt id (evidence, not an envelope artifact — no readEventJSON).
	if len(event.Attached) < 2 || len(event.Attached) > 3 {
		return fmt.Errorf("claim event must bind exactly one action and policy")
	}
	if len(event.Attached) == 3 && !strings.HasPrefix(event.Attached[2], "ratt_") {
		return fmt.Errorf("claim event third attached id %q is not a ratt_ RunAttempt", event.Attached[2])
	}
	actionID, policyID := event.Attached[0], event.Attached[1]
	var action envelope.ActionIntent
	if err := s.readEventJSON(actionID, &action); err != nil {
		return fmt.Errorf("read claim action %s: %w", actionID, err)
	}
	if err := action.Validate(); err != nil {
		return fmt.Errorf("claim action %s invalid: %w", actionID, err)
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(policyID, &decision); err != nil {
		return fmt.Errorf("read claim policy %s: %w", policyID, err)
	}
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("claim policy %s invalid: %w", policyID, err)
	}
	if action.ID != actionID || action.IDs.WorkID != claimed.ID || action.Actor != event.By ||
		action.Capability != "work.claim" || action.Action != "claim_work" ||
		action.Target.Kind != "work" || action.Target.ID != claimed.ID ||
		filepath.Clean(action.Target.Path) != filepath.Clean(s.workPath(claimed.ID)) {
		return fmt.Errorf("claim action %s conflicts with Work/event identity", actionID)
	}
	if decision.ID != policyID || decision.IDs.WorkID != claimed.ID || decision.ActionIntentID != actionID ||
		(decision.Outcome != "allow" && decision.Outcome != "allow_with_obligations") {
		return fmt.Errorf("claim policy %s conflicts with Work/event identity", policyID)
	}
	if !oneOf(actionID, claimed.ActionIntentIDs) {
		return fmt.Errorf("claimed Work does not retain its claim action")
	}
	normalNote := "claimed (policy " + decision.Outcome + ")"
	repairNote := "ownerless-claim repair (policy " + decision.Outcome + ")"
	if event.Note != normalNote && event.Note != repairNote {
		return fmt.Errorf("claim event policy outcome note mismatch")
	}
	return nil
}

// --- advance ---

type AdvanceInput struct {
	proofAdoptionAdmission bool
	proofManifestAdmission bool
	proofReviewAdmission   bool
	To                     string
	Actor                  string
	NonSourceDelivery      *ActionDeliveryRequest
	// ExpectedWorkState is an optional compare-and-set precondition evaluated
	// under the lifecycle admission lock. Governed execution uses it to reserve
	// claimed -> running exactly once while ordinary same-state advances remain
	// available for proof and attachment updates.
	ExpectedWorkState string
	// These two optional preconditions distinguish same-state generations:
	// every lifecycle mutation mints a new PolicyDecision + ActionIntent.
	ExpectedPolicyDecisionID string
	ExpectedActionIntentID   string
	// AutonomyClass optionally requires the live class kill-switch to be clear
	// at the same locked commit that changes lifecycle state.
	AutonomyClass string
	// LaneRef optionally declares or corrects the work's lane mid-lifecycle. When set it must
	// resolve via capgraph.ResolveLaneRef (fail-closed) and is recorded as the canonical node
	// id on the holder's lease, under the same holder-scoped rules as any advance mutation.
	LaneRef     string
	GitDelivery *envelope.GitDeliveryContract
	// RunAttemptIDs attaches one or more RunAttempt ids (ratt_*) to the work envelope. S1
	// retires the pre-S1 `RunID` field, which silently appended non-`act_*` ids into
	// ActionIntentIDs. RunAttempt ids land on the dedicated slot so the validator can reject
	// non-ratt_ entries (envelope.validateRunAttemptIDs, pinned by
	// TestRunAttemptIDsRequireRattPrefix). Empty slice is a no-op.
	RunAttemptIDs  []string
	ArtifactID     string
	VerificationID string
	ContextPackID  string
	// EvidenceRefs are immutable proof attachments for a specialized admitted
	// transition (currently evidence-threshold autonomy). Generic CLI advances
	// leave this empty. applyAdvance persists them on both the WorkEnvelope and
	// the exact lifecycle event so replay does not depend on a mutable sidecar.
	EvidenceRefs []string
	Note         string
	// Steal and StealReason mirror CloseInput's lease escape hatch (see
	// leaseHolderAdvanceGate in lease_advance.go): advancing a work held by a different
	// actor is refused unless Steal is true AND StealReason is non-empty.
	Steal       bool
	StealReason string
}

// Advance moves a work along a legal edge of the state machine, optionally attaching
// linked envelope ids (runs = executed actions, artifacts = outputs).
func (s *Store) Advance(workID string, in AdvanceInput, now time.Time) (envelope.WorkEnvelope, error) {
	var out envelope.WorkEnvelope
	var advanceErr error
	err := s.withLifecycleMutation(func() error {
		admissionTime := time.Now()
		if s.advanceClock != nil {
			admissionTime = s.advanceClock(now)
		}
		out, advanceErr = s.advanceLocked(workID, in, admissionTime)
		return advanceErr
	})
	return out, err
}

func (s *Store) advanceLocked(workID string, in AdvanceInput, now time.Time) (envelope.WorkEnvelope, error) {
	// Recovery is a mutation too: reject stale callers before replaying it.
	w, err := s.Get(workID)
	if err != nil {
		return w, err
	}
	if err := validateExpectedExecutionReservation(w, in.ExpectedWorkState, in.ExpectedPolicyDecisionID, in.ExpectedActionIntentID); err != nil {
		return w, err
	}
	w, err = s.recoverPendingClaimBeforeMutation(workID)
	if err != nil {
		return w, err
	}
	if err := validateExpectedExecutionReservation(w, in.ExpectedWorkState, in.ExpectedPolicyDecisionID, in.ExpectedActionIntentID); err != nil {
		return w, err
	}
	pendingActionID, err := s.pendingReceiptedActionID(workID)
	if err != nil {
		return w, err
	}
	if pendingActionID != "" {
		return w, fmt.Errorf("work %s has pending receipted action %s; recover it before another lifecycle mutation", workID, pendingActionID)
	}
	if terminalStates[w.State] {
		return w, fmt.Errorf("work %s is %s (terminal); cannot advance", workID, w.State)
	}
	if in.To == "" {
		return w, fmt.Errorf("advance requires --to <state>")
	}
	if terminalStates[in.To] {
		return w, terminalStateRequiresClose("advance", in.To)
	}
	if err := crossStateClaimedAdvanceError(w.State, in.To); err != nil {
		return w, err
	}
	if in.To != w.State && !canTransition(w.State, in.To) {
		return w, fmt.Errorf("illegal transition %s -> %s (allowed: %s)", w.State, in.To, strings.Join(transitions[w.State], ", "))
	}
	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		actor = "codex"
	}
	if class := strings.TrimSpace(in.AutonomyClass); class != "" {
		halted, reason, err := policy.AutonomyHalted(s.home, class)
		if err != nil {
			return w, fmt.Errorf("check autonomy halt (%s): %w", class, err)
		}
		if halted {
			return w, fmt.Errorf("autonomy halted (%s): %s", class, reason)
		}
	}
	// Kill-switch (W9-J4), defense-in-depth: an autonomous (non-operator) admission of a
	// director-proposed work out of `proposed` is denied while the policy-owned autonomy
	// halt is engaged, even through this generic seam. Manual operator admit, normal
	// claim, and normal advance never enter this branch; when the halt is unset it is a
	// no-op, so behavior is unchanged.
	if err := s.autonomyHaltGuard(w, in.To, actor, now); err != nil {
		return w, err
	}
	// Editable-surfaces (EVOLVE-BLOCK), defense-in-depth: a system-originated admission of
	// a director-proposed work that declares a target_surface is denied through this generic
	// seam too, unless that surface is actively whitelisted. The halt guard above runs first,
	// so a halt wins; work without a target_surface is unaffected.
	if err := s.editableSurfaceGuard(w, in.To, actor, now); err != nil {
		return w, err
	}
	// Design-sketch gate (alignment depth scales with risk): a work that declared
	// plan_class=phased may not ENTER running until the sketch it promised is attached
	// to the envelope. Placed at this generic seam so every path into running -- plain
	// advance, --execute, dispatch-claim auto-advance -- passes it; work of any other
	// plan class (including empty, which means oneshot) is unaffected.
	if err := s.designSketchGuard(&w, in.To, actor, now); err != nil {
		return w, err
	}
	// Freeze (grants-as-gates, 2026-07-10), defense-in-depth: a system-originated
	// admission matching an active freeze decision record is denied through this
	// generic seam too, mirroring the halt/editable-surface guards above.
	// Operator-provenance advances are excluded by isAutonomousAdmission the same
	// way (Law 6: the operator can always act; the system cannot ignore its own
	// freeze).
	if err := s.freezeGuard(w, in.To, actor, now); err != nil {
		return w, err
	}
	if err := heldReleaseOperatorError(w.State, in.To, actor); err != nil {
		return w, err
	}
	if err := directorProposedReleaseOperatorError(w, in.To, actor); err != nil {
		return w, err
	}
	if in.GitDelivery != nil {
		normalized, err := s.preflightGitDeliveryBinding(w, actor, in.To, in.GitDelivery)
		if err != nil {
			return w, err
		}
		in.GitDelivery = normalized
	}
	if in.NonSourceDelivery != nil {
		if in.LaneRef != "" || len(in.RunAttemptIDs) != 0 || in.ArtifactID != "" || in.VerificationID != "" || in.ContextPackID != "" || len(in.EvidenceRefs) != 0 {
			return w, fmt.Errorf("non-source delivery binding cannot attach unrelated execution or proof state")
		}
		if in.ExpectedWorkState == "" || in.ExpectedPolicyDecisionID == "" || in.ExpectedActionIntentID == "" {
			return w, fmt.Errorf("non-source delivery binding requires exact expected state, policy and action generation")
		}
		if in.GitDelivery != nil {
			return w, fmt.Errorf("cannot combine Git and non-source authority binding")
		}
		if _, err := s.preflightActionDeliveryBinding(w, actor, in.To, in.NonSourceDelivery); err != nil {
			return w, err
		}
	}
	if ref := strings.TrimSpace(in.VerificationID); strings.HasPrefix(ref, DifferentialProofVerificationPrefix) {
		if _, err := s.validateDifferentialProofRef(w, ref); err != nil {
			return w, fmt.Errorf("differential proof verification: %w", err)
		}
	}
	if ref := strings.TrimSpace(in.VerificationID); strings.HasPrefix(ref, ProofAdoptionVerificationPrefix) {
		if !in.proofAdoptionAdmission {
			return w, fmt.Errorf("exact proof admission requires work proof-adopt")
		}
		if _, err := s.validateProofAdoptionRef(w, ref, false); err != nil {
			return w, fmt.Errorf("exact proof adoption: %w", err)
		}
	}
	if ref := strings.TrimSpace(in.ArtifactID); strings.HasPrefix(ref, ProofManifestArtifactPrefix) && !in.proofManifestAdmission {
		return w, fmt.Errorf("proof membership custody requires work proof-manifest")
	}
	// Resolve a declared/corrected lane fail-closed BEFORE any state mutation: a bad --lane ref
	// aborts the advance untouched (never records a refusal, never mutates the envelope).
	declaredLane, err := s.resolveDeclaredLane(in.LaneRef)
	if err != nil {
		return w, err
	}
	if err := s.leaseHolderAdvanceGate(w, actor, in, now); err != nil {
		return w, err
	}
	advanced, err := s.applyAdvance(w, in, now)
	if err != nil {
		return advanced, err
	}
	// C1: abandon terminality. When a claimed work is released back to ready
	// (the only path that produces claimed→ready), every RunAttempt minted at
	// claim time is terminalized as "abandoned". The advance is already
	// durable at this point; terminalization is best-effort evidence — a
	// failure here is a doctor-visible gap, not a rollback (C3 sweeps stuck
	// attempts).
	if w.State == "claimed" && in.To == "ready" {
		s.abandonRunAttempts(advanced.RunAttemptIDs, now)
	}
	// F2: supersede-at-attach. When an incoming harness_execution attempt
	// attaches to the envelope, any still-pending claim_reservation attempt
	// already on the envelope (w.RunAttemptIDs, captured before this
	// advance) is now stale -- a real execution superseded the claim-time
	// placeholder -- and is terminalized abandoned. Same best-effort
	// durability posture as abandonRunAttempts above.
	if len(in.RunAttemptIDs) > 0 {
		s.supersedeClaimReservations(w.RunAttemptIDs, in.RunAttemptIDs, now)
	}
	// The advance passed the holder-scoped gate; record the declared lane on the holder's lease.
	if strings.TrimSpace(in.LaneRef) != "" {
		if err := s.recordDeclaredLaneOnLease(w.ID, declaredLane); err != nil {
			return advanced, err
		}
	}
	return advanced, nil
}

// abandonRunAttempts terminalizes every non-terminal RunAttempt in ids as
// "abandoned". Best-effort: errors are silently dropped because the advance
// is already durable and the RunAttempt is evidence, not enforcement. C3
// (crash terminality) sweeps any attempts this path misses.
func (s *Store) abandonRunAttempts(ids []string, now time.Time) {
	store := s.runAttempts()
	for _, id := range ids {
		ra, err := store.Get(id)
		if err != nil {
			continue // not found or invalid — nothing to abandon
		}
		if runattempt.IsTerminal(ra.Status) {
			continue // already terminal (succeeded/failed/abandoned)
		}
		_, _ = store.Complete(id, runattempt.StatusAbandoned, now)
	}
}

// supersedeClaimReservations terminalizes every still-pending
// claim_reservation-origin attempt in existingIDs as "abandoned" once
// incomingIDs attaches at least one harness_execution-origin attempt (F2,
// settled design decision 2). Attempts with empty Origin (legacy, pre-F2)
// or any origin other than claim_reservation are never touched -- only a
// claim-time placeholder can be superseded, and only a real execution can
// supersede it. Best-effort: errors are silently dropped, same posture as
// abandonRunAttempts (the advance is already durable; terminalization here
// is evidence, not enforcement).
func (s *Store) supersedeClaimReservations(existingIDs, incomingIDs []string, now time.Time) {
	store := s.runAttempts()
	var execIDs []string
	for _, id := range incomingIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ra, err := store.Get(id)
		if err != nil {
			continue
		}
		if ra.Origin == runattempt.OriginHarnessExecution {
			execIDs = append(execIDs, id)
		}
	}
	if len(execIDs) == 0 {
		return
	}
	reason := "superseded by " + strings.Join(execIDs, ",")
	for _, id := range existingIDs {
		ra, err := store.Get(id)
		if err != nil {
			continue
		}
		if ra.Origin != runattempt.OriginClaimReservation {
			continue // legacy (empty origin) or non-claim origin — not touched
		}
		if runattempt.IsTerminal(ra.Status) {
			continue // already terminal
		}
		_, _ = store.CompleteWithReason(id, runattempt.StatusSuperseded, reason, now)
	}
}

// applyAdvance performs the state write for a legal, gate-cleared advance. It is shared
// by Advance (after its provenance/halt guards) and AdmitAuto (after its kill-switch and
// eligibility gates) so both mutate through one audited path.
// advanceBeforeWorkCommit is a test synchronization seam for the crash window
// after the lifecycle event is durable and before the WorkEnvelope rename.
var advanceBeforeWorkCommit = func() {}

// closeBeforeLeaseRelease is a test synchronization seam for the crash window
// after the terminal WorkEnvelope, receipt, transition and lifecycle event are
// durable and before the claim lease is released (see releaseLease).
var closeBeforeLeaseRelease = func() {}

// closeAfterTerminalizationPrepare models process death after every exact
// RunAttempt intent is durable but before the first close envelope is written.
// Production leaves it as a no-op.
var closeAfterTerminalizationPrepare = func() {}

// closeAfterLeaseStealDecision models process death after the final close
// lease-override decision and before the first close envelope is written.
var closeAfterLeaseStealDecision = func() {}

// closeAfterFirstEnvelopeWrite models a crash after the closing ActionIntent
// is published. Exact retry must reuse that generation rather than minting a
// new action/policy/proof chain.
var closeAfterFirstEnvelopeWrite = func() {}

func (s *Store) applyAdvance(w envelope.WorkEnvelope, in AdvanceInput, now time.Time) (envelope.WorkEnvelope, error) {
	workID := w.ID
	if recovered, ok, err := s.recoverPendingAdvance(w, in.To, in.NonSourceDelivery); err != nil {
		return w, err
	} else if ok {
		return recovered, nil
	}
	proofAttachmentRef := ""
	if in.proofManifestAdmission || in.proofReviewAdmission {
		proofAttachmentRef = in.ArtifactID
	}
	if in.proofAdoptionAdmission {
		proofAttachmentRef = in.VerificationID
	}
	plan, err := s.AdvancePlan(workID, now, AdvancePlanOptions{Actor: in.Actor, To: in.To, GitDelivery: in.GitDelivery, NonSourceDelivery: in.NonSourceDelivery, proofAttachmentRef: proofAttachmentRef})
	if err != nil {
		return w, err
	}
	if !plan.Policy.Allowed {
		return w, fmt.Errorf("cannot advance work %s: policy %s (decision %s)", workID, plan.Policy.Outcome, plan.Policy.PolicyDecisionID)
	}
	if err := s.validateContextPackRef(in.ContextPackID); err != nil {
		return w, err
	}
	previousWorkSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		return w, err
	}
	from := w.State
	w.State = in.To
	if plan.GitDelivery != nil {
		contract := *plan.GitDelivery
		w.GitDelivery = &contract
		w.ExpectedBaseSHA = ""
	}
	w.ActionIntentIDs = append(w.ActionIntentIDs, plan.ActionIntent.ID)
	w.PolicyDecisionID = plan.Decision.ID
	attached := []string{plan.ActionIntent.ID, plan.Decision.ID}
	if plan.NonSourceDelivery != nil {
		w.NonSourceDelivery = append(w.NonSourceDelivery, *plan.NonSourceDelivery)
		w.EvidenceRefs = mergeUnique(w.EvidenceRefs, []string{plan.NonSourceDelivery.EvidenceRef})
	}
	if in.NonSourceDelivery != nil {
		attached = append(attached, in.NonSourceDelivery.EvidenceRef)
	}
	if len(in.EvidenceRefs) > 0 {
		w.EvidenceRefs = mergeUnique(w.EvidenceRefs, in.EvidenceRefs)
		attached = append(attached, in.EvidenceRefs...)
	}
	if len(in.RunAttemptIDs) > 0 {
		attachedAttempts, err := s.runAttemptAttachmentIndex(w)
		if err != nil {
			return envelope.WorkEnvelope{}, err
		}
		for _, rid := range in.RunAttemptIDs {
			rid = strings.TrimSpace(rid)
			if rid == "" {
				continue
			}
			if !strings.HasPrefix(rid, "ratt_") {
				return envelope.WorkEnvelope{}, fmt.Errorf("advance RunAttemptIDs contains non-ratt_ id %q (settled in S1; see internal/runattempt.Prefix)", rid)
			}
			// WEDGE PREVENTION: if a prior lifecycle event already attached this
			// ratt_, do NOT attach it again — the close-time authority gate builds
			// one (act_, pol_) pair per attaching event, and a second attach with a
			// fresh pair wedges the envelope forever (lived 2026-08-23,
			// work_1d968c3f0522). The attach is idempotent-dropped (the advance
			// itself still proceeds: same run, no new information); a genuinely new
			// execution must mint a fresh ratt_ via runattempt.Mint.
			if !attachedAttempts[rid] {
				attached = append(attached, rid)
			}
			w.RunAttemptIDs = mergeUnique(w.RunAttemptIDs, []string{rid})
		}
	}
	if in.ArtifactID != "" {
		w.ArtifactIDs = append(w.ArtifactIDs, in.ArtifactID)
		attached = append(attached, in.ArtifactID)
	}
	if in.VerificationID != "" {
		w.VerificationIDs = append(w.VerificationIDs, in.VerificationID)
		attached = append(attached, in.VerificationID)
	}
	if in.ContextPackID != "" {
		w.IDs.ContextPackID = in.ContextPackID
		attached = append(attached, in.ContextPackID)
	}
	if err := s.writeEventJSON(plan.ActionIntent.ID, &plan.ActionIntent); err != nil {
		return w, err
	}
	if err := s.writeEventJSON(plan.Decision.ID, &plan.Decision); err != nil {
		return w, err
	}
	workSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		return w, err
	}
	pendingPath := s.advancePendingPath(workID)
	// Never stage an envelope that fails the current schema: a hand edit or a
	// rule added after the last write must not ride the lifecycle into a
	// persisted invalid spine object.
	if err := w.Validate(); err != nil {
		return w, fmt.Errorf("refusing to persist invalid advanced envelope: %w", err)
	}
	stagedWork, err := s.stageJSON(pendingPath, &w)
	if err != nil {
		return w, err
	}
	if err := commitStagedJSONExclusive(stagedWork, pendingPath); err != nil {
		_ = os.Remove(stagedWork)
		return w, err
	}
	note := strings.TrimSpace(in.Note)
	if note == "" {
		note = "advanced (policy " + plan.Policy.Outcome + ")"
	} else {
		note += " (policy " + plan.Policy.Outcome + ")"
	}
	event := Event{
		TS:                 ts(now),
		WorkID:             workID,
		From:               from,
		To:                 in.To,
		By:                 plan.Actor,
		Note:               note,
		Attached:           attached,
		PreviousWorkSHA256: previousWorkSHA256,
		WorkSHA256:         workSHA256,
	}
	err = s.withWorkEventLock(func() error {
		before, err := s.readEventsStrictUnlocked(workID)
		if err != nil {
			return err
		}
		if err := s.appendEventUnlocked(event); err != nil {
			return err
		}
		after, err := s.readEventsStrictUnlocked(workID)
		if err != nil {
			return err
		}
		if len(after) != len(before)+1 || !reflect.DeepEqual(after[len(after)-1], event) {
			return fmt.Errorf("work event log did not preserve the exact advance event for %s", workID)
		}
		advanceBeforeWorkCommit()
		return commitStagedJSON(pendingPath, s.workPath(workID))
	})
	if err != nil {
		return w, err
	}
	return w, nil
}

func (s *Store) advancePendingPath(workID string) string {
	return s.workPath(workID) + ".advance-pending"
}

func (s *Store) recoverPendingAdvance(current envelope.WorkEnvelope, to string, delivery *ActionDeliveryRequest) (envelope.WorkEnvelope, bool, error) {
	currentSHA256, err := workEnvelopeSHA256(current)
	if err != nil {
		return current, false, err
	}
	pendingPath := s.advancePendingPath(current.ID)
	var recovered envelope.WorkEnvelope
	var didRecover bool
	err = s.withWorkEventLock(func() error {
		events, err := s.readEventsStrictUnlocked(current.ID)
		if err != nil {
			return err
		}
		var pending envelope.WorkEnvelope
		if err := s.readJSON(pendingPath, &pending); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read pending advance for %s: %w", current.ID, err)
			}
			for _, event := range events {
				if event.From == current.State && event.PreviousWorkSHA256 == currentSHA256 && event.WorkSHA256 != "" {
					return fmt.Errorf("work %s has a durable advance event to %s but no pending WorkEnvelope", current.ID, event.To)
				}
			}
			return nil
		}
		if err := pending.Validate(); err != nil {
			return fmt.Errorf("pending advance for %s invalid: %w", current.ID, err)
		}
		if pending.ID != current.ID || pending.State != to {
			return fmt.Errorf("pending advance for %s conflicts with requested transition %s -> %s", current.ID, current.State, to)
		}
		pendingSHA256, err := workEnvelopeSHA256(pending)
		if err != nil {
			return err
		}
		postMatches := make([]Event, 0, 1)
		exactMatches := make([]Event, 0, 1)
		for _, event := range events {
			if event.To != pending.State || event.WorkSHA256 != pendingSHA256 {
				continue
			}
			postMatches = append(postMatches, event)
			if event.From == current.State && event.PreviousWorkSHA256 == currentSHA256 {
				exactMatches = append(exactMatches, event)
			}
		}
		if currentSHA256 == pendingSHA256 {
			if len(postMatches) != 1 {
				return fmt.Errorf("pending advance for %s has %d matching durable events, want exactly one", current.ID, len(postMatches))
			}
			if err := s.validatePendingAdvance(postMatches[0], pending); err != nil {
				return err
			}
			if err := s.validatePendingDeliveryRequest(postMatches[0], current, pending, delivery); err != nil {
				return err
			}
			if err := removeFileAndSyncDir(pendingPath); err != nil {
				return err
			}
			recovered = current
			didRecover = true
			return nil
		}
		switch len(exactMatches) {
		case 0:
			if len(postMatches) > 0 {
				return fmt.Errorf("pending advance for %s does not match the current WorkEnvelope", current.ID)
			}
			return removeFileAndSyncDir(pendingPath)
		case 1:
			if err := s.validatePendingAdvance(exactMatches[0], pending); err != nil {
				return err
			}
			if err := s.validatePendingDeliveryRequest(exactMatches[0], current, pending, delivery); err != nil {
				return err
			}
			if err := commitStagedJSON(pendingPath, s.workPath(current.ID)); err != nil {
				return err
			}
			recovered = pending
			didRecover = true
			return nil
		default:
			return fmt.Errorf("pending advance for %s has %d matching durable events, want exactly one", current.ID, len(exactMatches))
		}
	})
	if err != nil {
		return current, false, err
	}
	return recovered, didRecover, nil
}

func (s *Store) validatePendingAdvance(event Event, pending envelope.WorkEnvelope) error {
	if event.PreviousWorkSHA256 == "" || event.WorkSHA256 == "" {
		return fmt.Errorf("pending advance event for %s does not bind both WorkEnvelope versions", pending.ID)
	}
	if event.WorkID != pending.ID || event.To != pending.State {
		return fmt.Errorf("pending advance event for %s conflicts with WorkEnvelope identity", pending.ID)
	}
	if len(event.Attached) < 2 {
		return fmt.Errorf("pending advance event for %s does not bind action and policy envelopes", pending.ID)
	}
	actionID, policyID := event.Attached[0], event.Attached[1]
	var action envelope.ActionIntent
	if err := s.readEventJSON(actionID, &action); err != nil {
		return fmt.Errorf("read pending advance action %s: %w", actionID, err)
	}
	if err := action.Validate(); err != nil {
		return fmt.Errorf("pending advance action %s invalid: %w", actionID, err)
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(policyID, &decision); err != nil {
		return fmt.Errorf("read pending advance policy %s: %w", policyID, err)
	}
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("pending advance policy %s invalid: %w", policyID, err)
	}
	if strings.TrimSpace(event.By) == "" || action.ID != actionID || action.IDs.WorkID != pending.ID || action.Actor != event.By {
		return fmt.Errorf("pending advance action %s conflicts with lifecycle event identity", actionID)
	}
	if action.Capability != "work.advance" || action.Action != "advance_work_to_"+event.To ||
		action.Target.Kind != "work" || action.Target.ID != pending.ID ||
		filepath.Clean(action.Target.Path) != filepath.Clean(s.workPath(pending.ID)) {
		return fmt.Errorf("pending advance action %s does not describe transition to %s", actionID, event.To)
	}
	if decision.ID != policyID || decision.IDs.WorkID != pending.ID ||
		decision.ActionIntentID != actionID || pending.PolicyDecisionID != policyID {
		return fmt.Errorf("pending advance policy %s conflicts with lifecycle event identity", policyID)
	}
	if decision.Outcome != "allow" && decision.Outcome != "allow_with_obligations" {
		return fmt.Errorf("pending advance policy %s does not allow publication", policyID)
	}
	if !oneOf(actionID, pending.ActionIntentIDs) {
		return fmt.Errorf("pending WorkEnvelope %s does not attach action %s", pending.ID, actionID)
	}
	return nil
}

// --- act (governed mutation) ---

// RecordAction records a performed ActionIntent against a work — but ONLY if its
// PolicyDecision allows it. This is the enforcement seam: a mutating adapter cannot
// record an ungoverned mutation, so no model bypasses policy by prompt obedience.
// (Decoupled from the policy package on purpose: the store enforces on the decision's
// own outcome, so the gate holds even if a caller skips the evaluator.)
func (s *Store) RecordAction(workID string, ai envelope.ActionIntent, pd envelope.PolicyDecision, now time.Time) (envelope.WorkEnvelope, error) {
	var out envelope.WorkEnvelope
	var recordErr error
	err := s.withLifecycleMutation(func() error {
		out, recordErr = s.recordActionLocked(workID, ai, pd, now)
		return recordErr
	})
	return out, err
}

func (s *Store) recordActionLocked(workID string, ai envelope.ActionIntent, pd envelope.PolicyDecision, now time.Time) (envelope.WorkEnvelope, error) {
	w, err := s.recoverPendingClaimBeforeMutation(workID)
	if err != nil {
		return w, err
	}
	pendingActionID, err := s.pendingReceiptedActionID(workID)
	if err != nil {
		return w, err
	}
	if pendingActionID != "" {
		return w, fmt.Errorf("work %s has pending receipted action %s; recover it before another lifecycle mutation", workID, pendingActionID)
	}
	if terminalStates[w.State] {
		return w, fmt.Errorf("work %s is %s (terminal); cannot record an action", workID, w.State)
	}
	if ai.IDs.WorkID != workID {
		return w, fmt.Errorf("action work_id %q does not match work %q", ai.IDs.WorkID, workID)
	}
	if pd.ActionIntentID != ai.ID {
		return w, fmt.Errorf("policy decision is for action %q, not %q", pd.ActionIntentID, ai.ID)
	}
	if err := ai.Validate(); err != nil {
		return w, fmt.Errorf("action intent invalid: %w", err)
	}
	if err := pd.Validate(); err != nil {
		return w, fmt.Errorf("policy decision invalid: %w", err)
	}
	// THE GATE: refuse to record a performed action unless policy allows it.
	if pd.Outcome != "allow" && pd.Outcome != "allow_with_obligations" {
		return w, fmt.Errorf("cannot record action %s: policy %s (a mutating adapter requires an allowing PolicyDecision)", ai.ID, pd.Outcome)
	}
	if err := s.validateSourceActionAdmission(w, ai); err != nil {
		return w, err
	}
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		return w, err
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		return w, err
	}
	w.ActionIntentIDs = append(w.ActionIntentIDs, ai.ID) // the performed action is an action ref in the chain
	w.PolicyDecisionID = pd.ID
	if err := s.writeJSON(s.workPath(workID), &w); err != nil {
		return w, err
	}
	if err := s.appendEvent(Event{TS: ts(now), WorkID: workID, From: w.State, To: w.State, Note: "action recorded: " + ai.Action + " (policy " + pd.Outcome + ")", Attached: []string{ai.ID, pd.ID}}); err != nil {
		return w, err
	}
	// PLACEMENT_OVERRIDE observability: policy.go fail-opens the placement-guard
	// input on PLACEMENT_OVERRIDE (operator-ratified; see docs/architecture/runtime.md),
	// and already appends a visible reason string to the PolicyDecision. That
	// reason lives only inside this one decision unless we persist it here — this
	// is the smallest honest seam: RecordAction is the consumer that already
	// durably persists every PolicyDecision, so an override use gets one
	// additional observation Event, auditable beyond the in-decision reasons.
	if reason := placementOverrideReason(pd); reason != "" {
		if err := s.appendEvent(Event{TS: ts(now), WorkID: workID, From: w.State, To: w.State, Note: "placement_override observed: " + reason, Attached: []string{pd.ID}}); err != nil {
			return w, err
		}
	}
	return w, nil
}

// placementOverrideObservationMarker matches the exact reason suffix
// internal/policy appends to a PolicyDecision when PLACEMENT_OVERRIDE (or an
// override-flagged PlacementGuardContext) fail-opens the placement guard.
const placementOverrideObservationMarker = "PLACEMENT_OVERRIDE active, fail-open"

// placementOverrideReason returns the matching reason string if pd carries
// evidence that a placement-guard fail-open fired via PLACEMENT_OVERRIDE, or
// "" if it did not fire (so RecordAction persists nothing extra).
func placementOverrideReason(pd envelope.PolicyDecision) string {
	for _, reason := range pd.Reasons {
		if strings.Contains(reason, placementOverrideObservationMarker) {
			return reason
		}
	}
	return ""
}

// --- close ---

type CloseInput struct {
	Outcome  string // completed | failed | cancelled
	MoveType string // outward_delta | memory_delta | rule_delta | control_plane_repair | audit_only
	Actor    string
	// Optional compare-and-set preconditions for governed execution
	// finalization. They are evaluated under the lifecycle lock.
	ExpectedWorkState        string
	ExpectedPolicyDecisionID string
	ExpectedActionIntentID   string
	LaneID                   string
	Summary                  string
	Changed                  bool
	// Steal and StealReason are the close-gate lease escape hatch: closing a work held by a
	// different actor is refused unless Steal is true AND StealReason is non-empty. A steal
	// records the prior holder + reason + stealing actor on the envelope trail (see
	// leaseHolderStatusFor / recordLeaseSteal in lease_close.go). Coordination is
	// advisory-with-teeth, not a deadlock machine — a dead lane must never brick an envelope.
	Steal                      bool
	StealReason                string
	UsedCapabilities           []string
	MissedCapabilities         []string
	FalseTriggeredCapabilities []string
	ProofRefs                  []string
	MemoryCandidates           []string
	MemoryPromoted             []string
	MemoryParked               []string
	MemoryRejected             []string
	FailureSurfaceOutcome      string
	FailureMechanism           string
	// UsedDeclared preserves historical/typed declarations. Claim scoring needs
	// separate application provenance; absence never implies non-use.
	UsedDeclared bool
	// ConsumedRefs is a legacy name for transcript matches. It is merged into
	// Skills.Referenced and never establishes application.
	ConsumedRefs       []string
	ReferencedRefs     []string
	ObservationMethod  string
	ObservationQuality string
	ApplicationMethod  string
	NonUseMethod       string
	ExplicitNonUse     []string
	NonUseEvidence     string
	// RunAttemptIDs attaches one or more RunAttempt ids (ratt_*) to the work
	// envelope at close -- the failed-harness-run forward link. Close is the
	// terminal transition for failed/cancelled work whose RunAttempt would
	// otherwise be orphaned (Advance already carries RunAttemptIDs for the
	// running->verifying path; this mirrors it for the close path so a failed
	// run is reachable from the envelope, not only from the run_attempts dir).
	// Same ratt_ prefix rule as AdvanceInput.RunAttemptIDs (enforced below).
	RunAttemptIDs []string
	// RunAttemptTerminalizations declares caller-owned terminal statuses that
	// intentionally differ from the work outcome. Close prepares these exact
	// requests before publishing any close byte, but leaves completion to the
	// named owner so it can attach post-close proof first.
	RunAttemptTerminalizations []RunAttemptTerminalizationRequest
	// OpensBranches is the executor's declared future-branch set (the model
	// PROPOSES; envelope validation decides; the BranchCoverage read model
	// verifies over later flow). Empty means this close opens no checkable
	// branch — which, combined with an empty next_consumer, makes a
	// future_behavior_changed claim unverifiable by construction.
	// Additive; validated as part of the emitted Transition.
	OpensBranches []envelope.BranchRef
	// SkipRunAttemptTerminalization opts out of close-time terminalization of
	// bound non-terminal RunAttempts (see closeLocked). A caller sets this
	// when it completes its RunAttempt itself AFTER the close — the panel
	// apply flow binds the close's own receipt/transition ids as proof refs
	// and completes the attempt with its honest status (failed on target
	// failures even though the envelope closes completed, fail-soft).
	SkipRunAttemptTerminalization bool
}

// RunAttemptTerminalizationRequest is the closed-vocabulary caller-owned
// terminalization reservation accepted by Close.
type RunAttemptTerminalizationRequest struct {
	RunAttemptID string `json:"run_attempt_id"`
	Owner        string `json:"owner"`
	TargetStatus string `json:"target_status"`
	Reason       string `json:"reason,omitempty"`
}

// CloseResult is the connected chain proof: the final work plus the Receipt and the
// apex Transition emitted at close, all carrying the same work_id.
type CloseResult struct {
	Work                        envelope.WorkEnvelope `json:"work"`
	Receipt                     envelope.Receipt      `json:"receipt"`
	Transition                  envelope.Transition   `json:"transition"`
	UnterminalizedRunAttemptIDs []string              `json:"unterminalized_run_attempt_ids,omitempty"`
}

var receiptOutcome = map[string]string{"completed": "completed", "failed": "failed", "cancelled": "no_op"}

func runAttemptStatusForCloseOutcome(outcome string) string {
	return map[string]string{
		"completed": runattempt.StatusSucceeded,
		"failed":    runattempt.StatusFailed,
		"cancelled": runattempt.StatusAbandoned,
	}[outcome]
}

type runAttemptTerminalizationPlan struct {
	Attempt    runattempt.RunAttempt
	Intent     runattempt.TerminalizationIntent
	CallerOwns bool
}

type runAttemptAuthority struct {
	ActionIntentID   string
	PolicyDecisionID string
}

func closeInputSHA256(in CloseInput) (string, error) {
	return canonicalEnvelopeSHA256(in)
}

// samePreparedCloseGeneration compares only the fields shared by every
// attempt participating in one close. Per-attempt owner/status/bindings are
// compared against the newly derived full intent in
// buildRunAttemptTerminalizationPlans before any close mutation.
func samePreparedCloseGeneration(a, b runattempt.TerminalizationIntent) bool {
	return a.SchemaVersion == b.SchemaVersion &&
		a.WorkID == b.WorkID &&
		a.ExpectedWorkState == b.ExpectedWorkState &&
		a.ExpectedWorkSHA256 == b.ExpectedWorkSHA256 &&
		a.ExpectedActionIntentID == b.ExpectedActionIntentID &&
		a.ExpectedPolicyDecisionID == b.ExpectedPolicyDecisionID &&
		a.CloseInputSHA256 == b.CloseInputSHA256 &&
		a.CloseActionIntentID == b.CloseActionIntentID &&
		a.ClosePolicyDecisionID == b.ClosePolicyDecisionID &&
		a.Outcome == b.Outcome &&
		a.ReceiptID == b.ReceiptID &&
		a.TransitionID == b.TransitionID &&
		a.PreparedAt.Equal(b.PreparedAt)
}

func (s *Store) closeGenerationTime(reserved envelope.WorkEnvelope, reservedSHA, inputSHA string, runAttemptIDs []string, requested time.Time) (time.Time, *runattempt.TerminalizationIntent, error) {
	var found *runattempt.TerminalizationIntent
	store := s.runAttempts()
	for _, id := range dedupeStrings(runAttemptIDs) {
		if !runattempt.Valid(id) {
			continue
		}
		ra, err := store.Get(id)
		if err != nil || ra.TerminalizationIntent == nil {
			continue
		}
		intent := *ra.TerminalizationIntent
		if err := intent.Validate(); err != nil {
			return time.Time{}, nil, fmt.Errorf("runattempt %s prepared close invalid: %w", id, err)
		}
		if intent.WorkID != reserved.ID || intent.ExpectedWorkState != reserved.State || intent.ExpectedWorkSHA256 != reservedSHA ||
			intent.ExpectedActionIntentID != latestActionIntentID(reserved) || intent.ExpectedPolicyDecisionID != reserved.PolicyDecisionID {
			return time.Time{}, nil, fmt.Errorf("runattempt %s prepared close no longer matches current work generation", id)
		}
		if intent.CloseInputSHA256 != inputSHA {
			return time.Time{}, nil, fmt.Errorf("runattempt %s prepared close input %s differs from retry close input %s", id, intent.CloseInputSHA256, inputSHA)
		}
		if found == nil {
			copyIntent := intent
			found = &copyIntent
			continue
		}
		if !samePreparedCloseGeneration(*found, intent) {
			return time.Time{}, nil, fmt.Errorf("work %s has conflicting prepared close generations", reserved.ID)
		}
	}
	if found != nil {
		return found.PreparedAt.UTC(), found, nil
	}
	return requested.UTC(), nil, nil
}

func validateRunAttemptRecord(id string, w envelope.WorkEnvelope, ra runattempt.RunAttempt) error {
	if !runattempt.Valid(id) || ra.ID != id || !runattempt.AcceptsSchemaVersion(ra.SchemaVersion) {
		return fmt.Errorf("runattempt %s has invalid embedded identity id=%q schema=%q", id, ra.ID, ra.SchemaVersion)
	}
	if ra.WorkID != w.ID {
		return fmt.Errorf("runattempt %s belongs to work %s, not work %s", ra.ID, ra.WorkID, w.ID)
	}
	switch ra.Status {
	case runattempt.StatusPending, runattempt.StatusRunning,
		runattempt.StatusSucceeded, runattempt.StatusFailed,
		runattempt.StatusAbandoned, runattempt.StatusSuperseded:
	default:
		return fmt.Errorf("runattempt %s status %q is invalid", ra.ID, ra.Status)
	}
	if ra.TerminalizationOwner == "" && (ra.TerminalizationReceiptID != "" || ra.TerminalizationTransitionID != "" || ra.TerminalizationReason != "") {
		return fmt.Errorf("runattempt %s has terminalization evidence without an owner", ra.ID)
	}
	if ra.TerminalizationOwner != "" && ((ra.TerminalizationReceiptID == "") != (ra.TerminalizationTransitionID == "")) {
		return fmt.Errorf("runattempt %s has a partial terminalization proof pair", ra.ID)
	}
	if !runattempt.IsTerminal(ra.Status) && (ra.TerminalizationOwner != "" ||
		ra.TerminalizationReceiptID != "" || ra.TerminalizationTransitionID != "" ||
		ra.TerminalizationReason != "") {
		return fmt.Errorf("runattempt %s is nonterminal but already carries terminalization completion evidence", ra.ID)
	}
	return nil
}

func attachedHas(values []string, want string) bool {
	return slices.Contains(values, want)
}

// findRunAttemptAttach walks the lifecycle event log for the FIRST event
// that attached rattID for this work, returning the (act_, pol_) pair
// carried by that attaching event plus the event's TS + Note for use in
// operator-facing diagnostics. found=false + err==nil means no prior
// attaching event exists; an empty rattID or one not matching the ratt_
// prefix is a graceful not-found.
//
// The close-time authority gate (resolveRunAttemptAuthorityFromEvents)
// builds one (act_, pol_) pair per attaching event, so a SECOND attaching
// event for the same ratt_ under a DIFFERENT pair wedges the envelope
// forever (lived 2026-08-23, work_1d968c3f0522). applyAdvance calls this
// once per ratt_ in RunAttemptIDs and REFUSES the advance if the new
// pair would not equal the existing pair; a same-pair re-attach is an
// idempotent no-op (the attach is dropped, the advance still proceeds).
// The claim -> running flow is structurally distinct because the claim
// reservation and the execution attempts carry DIFFERENT ratt_ ids.
func (s *Store) findRunAttemptAttach(w envelope.WorkEnvelope, rattID string) (eventTS string, eventNote string, existingActionID string, existingPolicyID string, found bool, err error) {
	if rattID == "" || !strings.HasPrefix(rattID, "ratt_") {
		return "", "", "", "", false, nil
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return "", "", "", "", false, err
	}
	for _, event := range events {
		if !attachedHas(event.Attached, rattID) {
			continue
		}
		var actionID, policyID string
		for _, ref := range event.Attached {
			switch {
			case strings.HasPrefix(ref, "act_"):
				if actionID == "" {
					actionID = ref
				}
			case strings.HasPrefix(ref, "pol_"):
				if policyID == "" {
					policyID = ref
				}
			}
		}
		return event.TS, event.Note, actionID, policyID, true, nil
	}
	return "", "", "", "", false, nil
}

// quoteForError wraps a string in double quotes for inclusion in operator-
// facing error messages (the wedge-prevention rejection at advance time).
// We don't use %q because the format string is shared with other %s/%d
// verbs and Go's %q escapes non-printable runes we want to leave visible
// in the note (operator-facing output stays human-readable).
func quoteForError(s string) string {
	return `"` + s + `"`
}

// runAttemptAttachmentIndex reads the strictly validated canonical lifecycle
// history once for the entire incoming attempt batch. It is a mutation-local
// projection under autonomy admission, never a persisted membership authority.
// Existing attachments retain their original action/policy pair.
func (s *Store) runAttemptAttachmentIndex(w envelope.WorkEnvelope) (map[string]bool, error) {
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return nil, err
	}
	attached := make(map[string]bool)
	for _, event := range events {
		for _, ref := range event.Attached {
			if strings.HasPrefix(ref, "ratt_") {
				attached[ref] = true
			}
		}
	}
	return attached, nil
}

// resolveRunAttemptAuthority binds one attempt to one exact allowed
// ActionIntent/PolicyDecision generation. Modern governed attempts carry the
// pair directly; claim reservations and legacy panel attempts recover the
// unique pair from the lifecycle event that attached their ratt_ id.
func (s *Store) resolveRunAttemptAuthority(w envelope.WorkEnvelope, ra runattempt.RunAttempt) (runAttemptAuthority, error) {
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return runAttemptAuthority{}, err
	}
	return s.resolveRunAttemptAuthorityFromEvents(w, ra, events)
}

func (s *Store) resolveRunAttemptAuthorityFromEvents(w envelope.WorkEnvelope, ra runattempt.RunAttempt, events []Event) (runAttemptAuthority, error) {
	actionID := strings.TrimSpace(ra.ActionIntentID)
	policyID := ""
	if ra.ExecutionPosture != nil {
		policyID = strings.TrimSpace(ra.ExecutionPosture.PolicyDecisionID)
	}
	type pair struct{ action, policy string }
	pairs := map[pair]bool{}
	if actionID != "" && policyID != "" {
		pairs[pair{actionID, policyID}] = true
	} else {
		for _, event := range events {
			if actionID == "" && !attachedHas(event.Attached, ra.ID) {
				continue
			}
			if ra.Origin == runattempt.OriginClaimReservation &&
				(event.To != "claimed" || !attachedHas(event.Attached, ra.ID)) {
				continue
			}
			var actions, policies []string
			for _, ref := range event.Attached {
				switch {
				case strings.HasPrefix(ref, "act_"):
					if actionID == "" || ref == actionID {
						actions = append(actions, ref)
					}
				case strings.HasPrefix(ref, "pol_"):
					if policyID == "" || ref == policyID {
						policies = append(policies, ref)
					}
				}
			}
			for _, candidateAction := range actions {
				for _, candidatePolicy := range policies {
					var decision envelope.PolicyDecision
					if err := s.readEventJSON(candidatePolicy, &decision); err == nil && decision.ActionIntentID == candidateAction {
						pairs[pair{candidateAction, candidatePolicy}] = true
					}
				}
			}
		}
	}
	if len(pairs) != 1 {
		// DETERMINISTIC TIEBREAK (lived 2026-08-23, work_1d968c3f0522):
		// when the close-time authority gate sees >1 (act_, pol_) pair for
		// the same ratt_ (a wedged history), the pair attached by the
		// lifecycle event that transitioned THIS attempt into `running` is
		// the authoritative one — the lifecycle that actually executed the
		// attempt was governed by that event's PolicyDecisionID. The lookup
		// is bound to the exact ratt_ and takes the LATEST running
		// generation: a work that was claimed, run, retried, and run again
		// has running events from every generation, and closing the later
		// attempt under the earlier generation's policy is confidently
		// wrong rather than loudly ambiguous. The tiebreak only resolves
		// when the running event's full (act_, pol_) pair is among the
		// candidates; anything else falls through to the loud error.
		if len(pairs) > 1 {
			if actionID, policyID, ok := findRunningAuthorityForAttempt(events, ra.ID); ok {
				if pairs[pair{actionID, policyID}] {
					pairs = map[pair]bool{{actionID, policyID}: true}
				}
			}
		}
		if len(pairs) == 0 {
			return runAttemptAuthority{}, fmt.Errorf("runattempt %s authority generation is missing (pairs=0)", ra.ID)
		}
		if len(pairs) > 1 {
			// Pair ambiguity is the lived 2026-08-23 wedge. Name EVERY pair
			// and the events that attached it so an operator can audit the
			// wedge without diffing events files by hand. The prevention at
			// applyAdvance stops new wedges; this error is defense-in-depth
			// for histories already on disk.
			var pairDescs []string
			for p := range pairs {
				pairDescs = append(pairDescs, fmt.Sprintf("(act=%s, pol=%s)", p.action, p.policy))
			}
			attachingEvents := []string{}
			for _, event := range events {
				if !attachedHas(event.Attached, ra.ID) {
					continue
				}
				attachingEvents = append(attachingEvents, fmt.Sprintf("event@%s note=%q", event.TS, event.Note))
			}
			return runAttemptAuthority{}, fmt.Errorf(
				"runattempt %s authority generation is ambiguous (pairs=%d): %s; attached by lifecycle events: %s. remediation: run compound work reconcile-run-attempts %s, or fix the source that wrote the second attach",
				ra.ID, len(pairs), strings.Join(pairDescs, " vs "), strings.Join(attachingEvents, "; "), w.ID,
			)
		}
		// len(pairs) == 1: tiebreak resolved a >1 case to a single pair; fall
		// through to the resolution path below.
	}
	var resolved pair
	for candidate := range pairs {
		resolved = candidate
	}
	if !slices.Contains(w.ActionIntentIDs, resolved.action) {
		return runAttemptAuthority{}, fmt.Errorf("runattempt %s authority action %s is not in work %s action chain", ra.ID, resolved.action, w.ID)
	}
	var action envelope.ActionIntent
	if err := s.readEventJSON(resolved.action, &action); err != nil {
		return runAttemptAuthority{}, fmt.Errorf("runattempt %s authority action %s unreadable: %w", ra.ID, resolved.action, err)
	}
	if err := action.Validate(); err != nil || action.IDs.WorkID != w.ID {
		return runAttemptAuthority{}, fmt.Errorf("runattempt %s authority action %s is invalid or belongs to another work", ra.ID, resolved.action)
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(resolved.policy, &decision); err != nil {
		return runAttemptAuthority{}, fmt.Errorf("runattempt %s authority policy %s unreadable: %w", ra.ID, resolved.policy, err)
	}
	// Identity and validation are unconditional; the ONE non-allowing
	// exception is the pre-execution refusal shape (lived close wedge,
	// work_7a5836372f5e) — see runAttemptPolicyRefusedBeforeExecution.
	if err := decision.Validate(); err != nil || decision.IDs.WorkID != w.ID || decision.ActionIntentID != action.ID {
		return runAttemptAuthority{}, fmt.Errorf("runattempt %s authority policy %s is invalid, work-mismatched, or not bound to action %s for work %s", ra.ID, resolved.policy, action.ID, w.ID)
	}
	if !policy.Allowed(decision) && !runAttemptPolicyRefusedBeforeExecution(ra) {
		return runAttemptAuthority{}, fmt.Errorf("runattempt %s authority policy %s does not allow action %s for work %s", ra.ID, resolved.policy, action.ID, w.ID)
	}
	return runAttemptAuthority{ActionIntentID: resolved.action, PolicyDecisionID: resolved.policy}, nil
}

// findRunningAuthorityForAttempt resolves the (act_, pol_) authority pair from
// the lifecycle event that transitioned the work into `running` while attaching
// exactly this run attempt. Used by the deterministic tiebreak in
// resolveRunAttemptAuthorityFromEvents when a wedged history presents >1
// candidate (act_, pol_) pair (lived 2026-08-23, work_1d968c3f0522).
//
// Binding rules, each closing a way the old first-policy-anywhere lookup could
// select an authority the attempt never ran under:
//
//   - only `running` events that attach the exact ratt_ count — a running event
//     from a different attempt of the same work must never decide this one;
//   - when several lifecycle generations attached the same ratt_ (claim, run,
//     retry, run again), the LATEST running generation wins, because that is
//     the lifecycle the attempt actually executed last;
//   - the chosen event must carry exactly one act_ and exactly one pol_ —
//     zero or several on the governing event is ambiguity, not a preference.
//
// ok=false means no governing pair could be established; the caller must then
// fail loudly instead of picking silently.
func findRunningAuthorityForAttempt(events []Event, runAttemptID string) (actionID string, policyID string, ok bool) {
	if !strings.HasPrefix(runAttemptID, "ratt_") {
		return "", "", false
	}
	var latest *Event
	for i := range events {
		event := &events[i]
		if event.To != "running" || !attachedHas(event.Attached, runAttemptID) {
			continue
		}
		if latest == nil || event.TS >= latest.TS {
			latest = event
		}
	}
	if latest == nil {
		return "", "", false
	}
	var actions, policies []string
	for _, ref := range latest.Attached {
		switch {
		case strings.HasPrefix(ref, "act_"):
			actions = append(actions, ref)
		case strings.HasPrefix(ref, "pol_"):
			policies = append(policies, ref)
		}
	}
	if len(actions) != 1 || len(policies) != 1 {
		return "", "", false
	}
	return actions[0], policies[0], true
}

func latestActionIntentID(w envelope.WorkEnvelope) string {
	for i := len(w.ActionIntentIDs) - 1; i >= 0; i-- {
		if strings.HasPrefix(w.ActionIntentIDs[i], "act_") {
			return w.ActionIntentIDs[i]
		}
	}
	return ""
}

type attachedRunAttemptHistory struct {
	SuccessfulRetryPredecessors map[string]bool
	ReplacedClaimReservations   map[string][]string
}

func runAttemptHasExecuted(ra runattempt.RunAttempt) bool {
	if ra.Origin != runattempt.OriginHarnessExecution {
		return false
	}
	if ra.Launch != nil {
		return ra.Launch.CommittedAt != nil
	}
	if ra.LastHeartbeatAt == nil || !ra.LastHeartbeatAt.After(ra.CreatedAt) {
		return false
	}
	return ra.CompletedAt.IsZero() || !ra.LastHeartbeatAt.After(ra.CompletedAt)
}

// runAttemptOwnsHarnessOutcome recognizes either a proved process launch or
// the one exact pre-launch failure that still belongs to the harness attempt.
// Origin alone is never enough: the start-error case must carry the exact
// rooted prepared+start_error lifecycle with no conflicting commit.
func runAttemptOwnsHarnessOutcome(ra runattempt.RunAttempt) bool {
	launch := ra.Launch
	if launch != nil && launch.CommittedAt != nil && launch.StartErrorAt != nil {
		return false
	}
	if runAttemptHasExecuted(ra) {
		return true
	}
	return ra.Origin == runattempt.OriginHarnessExecution &&
		launch != nil && launch.PreparedAt != nil && launch.StartErrorAt != nil &&
		launch.CommittedAt == nil && launch.PreparedAt.Equal(*launch.StartErrorAt)
}

// proofResourcePolicyRefusedBeforeExecution is the exact refusal reason the
// proof-resource reservation stamps when its policy denies before any
// execution (proof_reservation.go). It must stay byte-identical to that
// writer's literal: near-miss reasons must keep rejecting.
const proofResourcePolicyRefusedBeforeExecution = "proof resource policy refused before execution"

// runAttemptPolicyRefusedBeforeExecution reports whether ra carries the exact
// closed shape of a pre-execution policy refusal — the ONE non-allowing
// authority pair the close-time gate may bind (lived close wedge,
// work_7a5836372f5e). The refusal happened before any side effect, so the
// refusing (act_, pol_) pair is the attempt's legitimate authority record.
// Every field is an exact-match condition; a near-miss on any dimension
// rejects.
func runAttemptPolicyRefusedBeforeExecution(ra runattempt.RunAttempt) bool {
	if ra.Status != runattempt.StatusAbandoned || ra.Origin != runattempt.OriginHarnessExecution {
		return false
	}
	if ra.SweepReason != proofResourcePolicyRefusedBeforeExecution {
		return false
	}
	if ra.Launch != nil || ra.LastHeartbeatAt != nil {
		return false
	}
	if ra.CompletedAt.IsZero() || !ra.CompletedAt.Equal(ra.CreatedAt) {
		return false
	}
	return !runAttemptHasExecuted(ra)
}

const staleSideEffectFreeSweepPrefix = "stale: " + runattempt.SweepClassificationSideEffectFree + " — no activity for "

// runAttemptSweptSideEffectFreeReservation recognizes a stale claim
// reservation that the sweep proved never crossed an execution boundary.
func runAttemptSweptSideEffectFreeReservation(ra runattempt.RunAttempt) bool {
	if ra.Status != runattempt.StatusAbandoned || ra.Origin != runattempt.OriginClaimReservation {
		return false
	}
	if ra.SweepClassification != runattempt.SweepClassificationSideEffectFree ||
		!strings.HasPrefix(ra.SweepReason, staleSideEffectFreeSweepPrefix) {
		return false
	}
	if ra.CompletedAt.IsZero() || !ra.CompletedAt.After(ra.CreatedAt) || ra.TerminalizationIntent != nil {
		return false
	}
	return runattempt.ClassifyStaleRunAttempt(ra) == runattempt.SweepClassificationSideEffectFree &&
		!runAttemptHasExecuted(ra)
}

func buildAttachedRunAttemptHistory(ids []string, attempts map[string]runattempt.RunAttempt) (attachedRunAttemptHistory, error) {
	history := attachedRunAttemptHistory{
		SuccessfulRetryPredecessors: map[string]bool{},
		ReplacedClaimReservations:   map[string][]string{},
	}
	orderedIDs := dedupeStrings(ids)
	positions := make(map[string]int, len(orderedIDs))
	for index, id := range orderedIDs {
		if _, ok := attempts[id]; ok {
			positions[id] = index
		}
	}
	parents := map[string]string{}
	for _, id := range orderedIDs {
		child, ok := attempts[id]
		if !ok || child.PredecessorID == "" {
			continue
		}
		predecessorID := child.PredecessorID
		if !runattempt.Valid(predecessorID) {
			return attachedRunAttemptHistory{}, fmt.Errorf("runattempt %s retry predecessor %q is not a canonical ratt_ id", id, predecessorID)
		}
		if predecessorID == id {
			return attachedRunAttemptHistory{}, fmt.Errorf("runattempt retry lineage contains a cycle at %s", id)
		}
		predecessor, ok := attempts[predecessorID]
		if !ok {
			return attachedRunAttemptHistory{}, fmt.Errorf("runattempt %s retry predecessor %s is not attached to the work", id, predecessorID)
		}
		if positions[predecessorID] >= positions[id] {
			return attachedRunAttemptHistory{}, fmt.Errorf("runattempt %s retry predecessor %s is not earlier in attachment order", id, predecessorID)
		}
		if !child.CreatedAt.After(predecessor.CreatedAt) {
			return attachedRunAttemptHistory{}, fmt.Errorf("runattempt %s retry predecessor %s is not earlier in creation order", id, predecessorID)
		}
		parents[id] = predecessorID
	}
	for _, id := range orderedIDs {
		ra, ok := attempts[id]
		if !ok || ra.Status != runattempt.StatusSucceeded || !runAttemptHasExecuted(ra) {
			continue
		}
		for predecessorID := parents[id]; predecessorID != ""; predecessorID = parents[predecessorID] {
			history.SuccessfulRetryPredecessors[predecessorID] = true
		}
	}
	for claimIndex, claimID := range orderedIDs {
		claim, ok := attempts[claimID]
		if !ok || claim.Origin != runattempt.OriginClaimReservation {
			continue
		}
		for executionIndex := claimIndex + 1; executionIndex < len(orderedIDs); executionIndex++ {
			executionID := orderedIDs[executionIndex]
			execution, ok := attempts[executionID]
			if !ok || !execution.CreatedAt.After(claim.CreatedAt) || !runAttemptOwnsHarnessOutcome(execution) {
				continue
			}
			history.ReplacedClaimReservations[claimID] = append(history.ReplacedClaimReservations[claimID], executionID)
		}
	}
	return history, nil
}

// historicalRunAttemptOutcomeCompatible classifies terminal attempts whose
// status does not represent the terminal work outcome. A failed predecessor is
// compatible only when a successful retry completed the work. A claim
// reservation replaced by any later harness execution attempt is always
// outcome-neutral: the reservation did not execute the task, including when a
// stale sweep made it terminal before the harness could supersede it.
func historicalRunAttemptOutcomeCompatible(outcome, id string, history attachedRunAttemptHistory) bool {
	if len(history.ReplacedClaimReservations[id]) > 0 {
		return true
	}
	return outcome == "completed" && history.SuccessfulRetryPredecessors[id]
}

func (s *Store) buildRunAttemptTerminalizationPlans(
	reserved envelope.WorkEnvelope,
	reservedSHA string,
	closing envelope.WorkEnvelope,
	plan ClosePlan,
	receipt *envelope.Receipt,
	transition envelope.Transition,
	in CloseInput,
	inputSHA string,
	now time.Time,
) ([]runAttemptTerminalizationPlan, error) {
	if in.SkipRunAttemptTerminalization && len(in.RunAttemptTerminalizations) == 0 {
		return nil, errors.New("SkipRunAttemptTerminalization requires explicit RunAttemptTerminalizations")
	}
	if !in.SkipRunAttemptTerminalization && len(in.RunAttemptTerminalizations) > 0 {
		// Caller-owned terminalization is allowed WITHOUT SkipRunAttemptTerminalization
		// only for terminal abandoned or explicitly verification-rejected
		// succeeded attempts. Both doors acknowledge rather than rewrite.
		// Non-terminal targets still require SkipRunAttemptTerminalization so the
		// gate owns the terminalization of bound attempts.
		store := s.runAttempts()
		for _, req := range in.RunAttemptTerminalizations {
			ra, err := store.Get(req.RunAttemptID)
			if err != nil {
				return nil, fmt.Errorf("caller terminalization run_attempt_id %s unreadable: %w", req.RunAttemptID, err)
			}
			if strings.TrimSpace(req.Owner) == runattempt.TerminalizationOwnerWorkVerificationRejected && ra.Status == runattempt.StatusSucceeded {
				continue // Already validated; acknowledgement never terminalizes.
			}
			if !runattempt.IsTerminal(ra.Status) || ra.Status != runattempt.StatusAbandoned {
				return nil, fmt.Errorf("RunAttemptTerminalizations requires SkipRunAttemptTerminalization (only acknowledge-abandoned is allowed without it)")
			}
		}
	}
	requests := map[string]RunAttemptTerminalizationRequest{}
	for _, request := range in.RunAttemptTerminalizations {
		request.RunAttemptID = strings.TrimSpace(request.RunAttemptID)
		request.Owner = strings.TrimSpace(request.Owner)
		request.TargetStatus = strings.TrimSpace(request.TargetStatus)
		request.Reason = strings.TrimSpace(request.Reason)
		if !runattempt.Valid(request.RunAttemptID) {
			return nil, fmt.Errorf("caller terminalization run_attempt_id %q is invalid", request.RunAttemptID)
		}
		if _, duplicate := requests[request.RunAttemptID]; duplicate {
			return nil, fmt.Errorf("caller terminalization duplicates %s", request.RunAttemptID)
		}
		if request.Owner == runattempt.TerminalizationOwnerWorkClose || request.Owner == runattempt.TerminalizationOwnerWorkSupersede {
			return nil, fmt.Errorf("caller terminalization %s cannot claim work-owned terminalization owner %s", request.RunAttemptID, request.Owner)
		}
		// acknowledge-abandoned is a record-only sub-owner of work.close for
		// already-terminal abandoned attempts (no status rewrite);
		// the receipt proof_ref carries the acknowledgement under that owner.
		if request.Owner == runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned &&
			request.TargetStatus != runattempt.StatusAbandoned {
			return nil, fmt.Errorf("caller terminalization %s owner %q requires target_status %q", request.RunAttemptID, request.Owner, runattempt.StatusAbandoned)
		}
		requests[request.RunAttemptID] = request
	}

	workTarget := runAttemptStatusForCloseOutcome(in.Outcome)
	preparedAt := now.UTC()
	var plans []runAttemptTerminalizationPlan
	seen := map[string]bool{}
	store := s.runAttempts()
	attempts := map[string]runattempt.RunAttempt{}
	for _, id := range dedupeStrings(closing.RunAttemptIDs) {
		if id == "" || !runattempt.Valid(id) {
			return nil, fmt.Errorf("close RunAttemptIDs contains invalid id %q", id)
		}
		ra, err := store.Get(id)
		if err != nil {
			return nil, fmt.Errorf("close runattempt %s unreadable: %w", id, err)
		}
		if err := validateRunAttemptRecord(id, closing, ra); err != nil {
			return nil, err
		}
		attempts[id] = ra
	}
	history, err := buildAttachedRunAttemptHistory(closing.RunAttemptIDs, attempts)
	if err != nil {
		return nil, err
	}
	events, err := s.eventsFor(reserved.ID)
	if err != nil {
		return nil, err
	}
	for _, id := range dedupeStrings(closing.RunAttemptIDs) {
		ra := attempts[id]
		request, callerOwns := requests[id]
		if callerOwns {
			seen[id] = true
		}
		if runattempt.IsTerminal(ra.Status) {
			if callerOwns {
				// Both acknowledgement owners are record-only. Verification
				// rejection was validated against the exact pre-close state.
				verificationRejected := request.Owner == runattempt.TerminalizationOwnerWorkVerificationRejected
				if !verificationRejected && (ra.Status != runattempt.StatusAbandoned || request.TargetStatus != runattempt.StatusAbandoned) {
					return nil, fmt.Errorf("caller terminalization %s targets an already-terminal attempt", id)
				}
				if strings.TrimSpace(request.Reason) == "" {
					return nil, fmt.Errorf("caller terminalization %s (acknowledge-abandoned) requires a non-empty reason", id)
				}
				if _, err := s.resolveRunAttemptAuthorityFromEvents(reserved, ra, events); err != nil {
					return nil, err
				}
				if ra.TerminalizationOwner == runattempt.TerminalizationOwnerWorkClose &&
					(ra.TerminalizationReceiptID != receipt.ID || ra.TerminalizationTransitionID != transition.ID) {
					return nil, fmt.Errorf("terminal runattempt %s work.close proof pair %s/%s does not match closing proof %s/%s", id, ra.TerminalizationReceiptID, ra.TerminalizationTransitionID, receipt.ID, transition.ID)
				}
				if ra.TerminalizationIntent != nil {
					return nil, fmt.Errorf("terminal runattempt %s already carries a prepared intent while work %s is nonterminal", id, reserved.ID)
				}
				// Stamp the acknowledgement on the receipt's proof_refs so
				// the operator-visible audit carries it (no Receipt schema
				// change — proof refs are already the structured-evidence
				// surface; `abandon_ack:` and `verification_rejected:`
				// document acknowledgement without rewriting execution history).
				namespace := "abandon_ack"
				if verificationRejected {
					namespace = "verification_rejected"
				}
				receipt.ProofRefs = append(receipt.ProofRefs,
					fmt.Sprintf("%s:%s:%s", namespace, id, strings.TrimSpace(request.Reason)))
				continue
			}
			historicalOutcomeCompatibleAttempt := historicalRunAttemptOutcomeCompatible(in.Outcome, id, history)
			// Non-executed attempts have no task outcome. Completed close may
			// bind only an exact pre-execution refusal or a sweep-proven,
			// side-effect-free claim reservation.
			nonExecutedAttempt := in.Outcome == "completed" &&
				(runAttemptPolicyRefusedBeforeExecution(ra) || runAttemptSweptSideEffectFreeReservation(ra) ||
					runAttemptRefusedStartBeforeExecution(ra))
			if ra.Status != runattempt.StatusSuperseded && ra.Status != workTarget &&
				!historicalOutcomeCompatibleAttempt && !nonExecutedAttempt {
				verifiedProof, err := s.committedVerifiedProofRunAttempt(closing, ra)
				if err != nil {
					return nil, fmt.Errorf("terminal runattempt %s proof-resource exception invalid: %w", id, err)
				}
				if !verifiedProof {
					return nil, fmt.Errorf("terminal runattempt %s status %s contradicts close outcome %s (want %s)", id, ra.Status, in.Outcome, workTarget)
				}
			}
			if _, err := s.resolveRunAttemptAuthorityFromEvents(reserved, ra, events); err != nil {
				return nil, err
			}
			if ra.TerminalizationOwner == runattempt.TerminalizationOwnerWorkClose &&
				(ra.TerminalizationReceiptID != receipt.ID || ra.TerminalizationTransitionID != transition.ID) {
				return nil, fmt.Errorf("terminal runattempt %s work.close proof pair %s/%s does not match closing proof %s/%s", id, ra.TerminalizationReceiptID, ra.TerminalizationTransitionID, receipt.ID, transition.ID)
			}
			if ra.TerminalizationIntent != nil {
				return nil, fmt.Errorf("terminal runattempt %s already carries a prepared intent while work %s is nonterminal", id, reserved.ID)
			}
			continue
		}
		authority, err := s.resolveRunAttemptAuthorityFromEvents(reserved, ra, events)
		if err != nil {
			return nil, err
		}
		owner, target := runattempt.TerminalizationOwnerWorkClose, workTarget
		if ra.Origin == runattempt.OriginClaimReservation && len(history.ReplacedClaimReservations[id]) > 0 {
			owner, target = runattempt.TerminalizationOwnerWorkSupersede, runattempt.StatusSuperseded
		}
		if callerOwns {
			owner, target = request.Owner, request.TargetStatus
		}
		intent := runattempt.TerminalizationIntent{
			SchemaVersion:             runattempt.TerminalizationIntentSchemaVersion,
			Owner:                     owner,
			WorkID:                    reserved.ID,
			RunAttemptActionIntentID:  ra.ActionIntentID,
			AuthorityActionIntentID:   authority.ActionIntentID,
			AuthorityPolicyDecisionID: authority.PolicyDecisionID,
			ExpectedWorkState:         reserved.State,
			ExpectedWorkSHA256:        reservedSHA,
			ExpectedActionIntentID:    latestActionIntentID(reserved),
			ExpectedPolicyDecisionID:  reserved.PolicyDecisionID,
			CloseInputSHA256:          inputSHA,
			CloseActionIntentID:       plan.ActionIntent.ID,
			ClosePolicyDecisionID:     plan.Decision.ID,
			Outcome:                   in.Outcome,
			TargetStatus:              target,
			ReceiptID:                 receipt.ID,
			TransitionID:              transition.ID,
			PreparedAt:                preparedAt,
		}
		if owner == runattempt.TerminalizationOwnerWorkSupersede {
			intent.TerminalizationReason = "superseded by " + strings.Join(history.ReplacedClaimReservations[id], ",")
		} else if callerOwns {
			intent.TerminalizationReason = request.Reason
		}
		if ra.ExecutionPosture != nil {
			intent.RunAttemptPolicyDecisionID = ra.ExecutionPosture.PolicyDecisionID
		}
		if err := intent.Validate(); err != nil {
			return nil, fmt.Errorf("runattempt %s terminalization intent invalid: %w", id, err)
		}
		// This exact comparison is deliberately before leaseHolderCloseGate.
		// A prepared retry may reuse only the complete immutable intent; a
		// conflict in owner, target, reason, authority, proof ids, digests, or
		// prepared timestamp must leave the entire home byte-identical.
		if ra.TerminalizationIntent != nil && !reflect.DeepEqual(*ra.TerminalizationIntent, intent) {
			return nil, fmt.Errorf("runattempt %s prepared terminalization differs from the exact requested close", id)
		}
		plans = append(plans, runAttemptTerminalizationPlan{Attempt: ra, Intent: intent, CallerOwns: callerOwns})
	}
	for id := range requests {
		if !seen[id] {
			return nil, fmt.Errorf("caller terminalization %s is not a non-terminal RunAttempt bound to work %s", id, reserved.ID)
		}
	}
	return plans, nil
}

func (s *Store) prepareRunAttemptTerminalizations(plans []runAttemptTerminalizationPlan) error {
	store := s.runAttempts()
	for _, plan := range plans {
		if _, err := prepareRunAttemptTerminalization(store, plan.Attempt.ID, plan.Intent); err != nil {
			return err
		}
	}
	return nil
}

func terminalizationIntentSHA256(intent runattempt.TerminalizationIntent) (string, error) {
	return canonicalEnvelopeSHA256(intent)
}

func terminalizationDigestsForPlans(plans []runAttemptTerminalizationPlan) ([]RunAttemptTerminalizationDigest, error) {
	out := make([]RunAttemptTerminalizationDigest, 0, len(plans))
	for _, plan := range plans {
		digest, err := terminalizationIntentSHA256(plan.Intent)
		if err != nil {
			return nil, fmt.Errorf("runattempt %s terminalization digest: %w", plan.Attempt.ID, err)
		}
		out = append(out, RunAttemptTerminalizationDigest{
			RunAttemptID: plan.Attempt.ID,
			IntentSHA256: digest,
		})
	}
	return out, nil
}

// Close lands a work in a terminal state and emits the Receipt + Transition that tie the
// whole chain (context -> runs -> outputs) to this work_id.
func (s *Store) Close(workID string, in CloseInput, now time.Time) (CloseResult, error) {
	var out CloseResult
	var closeErr error
	err := s.withLifecycleMutation(func() error {
		out, closeErr = s.closeLocked(workID, in, now)
		return closeErr
	})
	return out, err
}

func (s *Store) closeLocked(workID string, in CloseInput, now time.Time) (CloseResult, error) {
	var res CloseResult
	if pending, present, err := s.loadClosePending(workID); err != nil {
		return res, err
	} else if present {
		return s.completeClosePending(pending, in, now)
	}
	w, err := s.Get(workID)
	if err != nil {
		return res, err
	}
	if err := validateExpectedExecutionReservation(w, in.ExpectedWorkState, in.ExpectedPolicyDecisionID, in.ExpectedActionIntentID); err != nil {
		return res, err
	}
	w, err = s.recoverPendingClaimBeforeClose(workID)
	if err != nil {
		return res, err
	}
	if err := validateExpectedExecutionReservation(w, in.ExpectedWorkState, in.ExpectedPolicyDecisionID, in.ExpectedActionIntentID); err != nil {
		return res, err
	}
	pendingActionID, err := s.pendingReceiptedActionID(workID)
	if err != nil {
		return res, err
	}
	if pendingActionID != "" {
		return res, fmt.Errorf("work %s has pending receipted action %s; recover it before another lifecycle mutation", workID, pendingActionID)
	}
	if !terminalStates[in.Outcome] {
		return res, fmt.Errorf("close --outcome must be completed|failed|cancelled, got %q", in.Outcome)
	}
	if err := s.validateVerificationRejectedRequests(w, in); err != nil {
		return res, err
	}
	if w.State != in.Outcome && !canTransition(w.State, in.Outcome) {
		return res, fmt.Errorf("cannot close %s from state %q to %q", workID, w.State, in.Outcome)
	}
	// Idempotent re-close: a work already terminal with the SAME outcome returns
	// its existing terminal proof pair — "the closing receipt" is not
	// last-writer-wins, and re-minting a fresh receipt+transition would strand
	// the original pair unreachable from the envelope.
	if w.State == in.Outcome {
		events, err := s.eventsFor(workID)
		if err != nil {
			return res, err
		}
		for i := len(events) - 1; i >= 0; i-- {
			ev := events[i]
			if ev.To != in.Outcome {
				continue
			}
			var recID, trID string
			for _, id := range ev.Attached {
				if strings.HasPrefix(id, "rec_") {
					recID = id
				}
				if strings.HasPrefix(id, "tr_") {
					trID = id
				}
			}
			if recID == "" || trID == "" {
				break // terminal event without its pair: fall through to a fresh close
			}
			var rec envelope.Receipt
			if err := s.readEventJSON(recID, &rec); err != nil {
				return res, fmt.Errorf("re-close: read existing closing receipt %s: %w", recID, err)
			}
			var tr envelope.Transition
			if err := s.readEventJSON(trID, &tr); err != nil {
				return res, fmt.Errorf("re-close: read existing closing transition %s: %w", trID, err)
			}
			// A re-close carrying NEW information is not a replay: refuse with a
			// pointer to the existing terminal receipt rather than silently
			// discarding the caller's evidence.
			if in.MoveType != rec.MoveType {
				return res, fmt.Errorf("work %s is already %s (receipt %s, move_type %s); a re-close with move_type %s is not a replay — attach new evidence through new work", workID, in.Outcome, rec.ID, rec.MoveType, in.MoveType)
			}
			for _, ref := range in.ProofRefs {
				if !slices.Contains(rec.ProofRefs, ref) {
					return res, fmt.Errorf("work %s is already %s (receipt %s); re-close carries new proof ref %q not on that receipt — attach new evidence through new work", workID, in.Outcome, rec.ID, ref)
				}
			}
			return CloseResult{Work: w, Receipt: rec, Transition: tr}, nil
		}
	}
	var landingProof *LandCheckReport
	deliveryProofRefs := mergeUnique(w.ArtifactIDs, w.VerificationIDs, in.ProofRefs, landCheckEvidenceRefs(w.EvidenceRefs))
	landingProof, err = s.gitDeliveryProofForClose(w, in.Outcome, in.MoveType, deliveryProofRefs, now)
	if err != nil {
		return res, err
	}
	// The lane stamped onto close-minted receipts is the DECLARED lane on the lease (a canonical
	// node id recorded at claim/advance time) or an explicit, resolver-gated in.LaneID override —
	// never the actor/owner/harness (no-guessing, B0). No declared lane → empty.
	producerLaneID, err := s.closeLaneID(w.ID, in.LaneID)
	if err != nil {
		return res, err
	}
	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		actor = "codex"
	}
	if err := heldReleaseOperatorError(w.State, in.Outcome, actor); err != nil {
		return res, err
	}
	if !oneOf(in.MoveType, envelope.MoveTypes) {
		return res, fmt.Errorf("close --move-type must be one of %v, got %q", envelope.MoveTypes, in.MoveType)
	}
	reservedWork := w
	reservedWorkSHA256, err := workEnvelopeSHA256(reservedWork)
	if err != nil {
		return res, err
	}
	inputSHA256, err := closeInputSHA256(in)
	if err != nil {
		return res, err
	}
	// Assemble and validate the complete final RunAttempt union before the
	// lease gate can record a denial/steal or transfer the live lease.
	for _, rid := range in.RunAttemptIDs {
		rid = strings.TrimSpace(rid)
		if rid == "" {
			continue
		}
		if !runattempt.Valid(rid) {
			return res, fmt.Errorf("close RunAttemptIDs contains invalid id %q", rid)
		}
		w.RunAttemptIDs = mergeUnique(w.RunAttemptIDs, []string{rid})
	}
	// Include explicit caller references before discovery: their launch-free
	// lifecycle still passes through the existing exact terminalization rules.
	// Discovery never repairs an earlier attachment by rotating newer authority.
	w.RunAttemptIDs, err = s.closeRunAttemptCensus(w)
	if err != nil {
		return res, err
	}
	closeNow, preparedGeneration, err := s.closeGenerationTime(reservedWork, reservedWorkSHA256, inputSHA256, w.RunAttemptIDs, now)
	if err != nil {
		return res, err
	}
	frozenCloseActionID := ""
	if preparedGeneration != nil {
		frozenCloseActionID = preparedGeneration.CloseActionIntentID
	}
	plan, err := s.ClosePlan(workID, closeNow, ClosePlanOptions{
		Actor:                in.Actor,
		Outcome:              in.Outcome,
		MoveType:             in.MoveType,
		ProofRefs:            in.ProofRefs,
		Steal:                in.Steal,
		StealReason:          in.StealReason,
		FrozenActionIntentID: frozenCloseActionID,
	})
	if err != nil {
		return res, err
	}
	if !plan.Policy.Allowed {
		return res, fmt.Errorf("cannot close work %s: policy %s (decision %s)", workID, plan.Policy.Outcome, plan.Policy.PolicyDecisionID)
	}
	w.ActionIntentIDs = append(w.ActionIntentIDs, plan.ActionIntent.ID)
	w.PolicyDecisionID = plan.Decision.ID
	capabilityRefs, err := s.capabilityRefsForContextPack(w.IDs.ContextPackID)
	if err != nil {
		return res, err
	}
	usedCapabilitiesForRuns, err := s.usedCapabilitiesForRuns(w.ActionIntentIDs, capabilityRefs)
	if err != nil {
		return res, err
	}
	usedCapabilities := mergeUnique(normalizeCapabilityRefs(in.UsedCapabilities), usedCapabilitiesForRuns)
	missedCapabilities := normalizeCapabilityRefs(in.MissedCapabilities)
	falseTriggeredCapabilities := normalizeCapabilityRefs(in.FalseTriggeredCapabilities)
	skillProjectionRefs := mergeUnique(usedCapabilities, capabilityRefs, missedCapabilities, falseTriggeredCapabilities)
	proofRefs := mergeUnique(in.ProofRefs, w.ArtifactIDs, w.VerificationIDs, landCheckEvidenceRefs(w.EvidenceRefs))
	if err := validateProofMedium(w.ProofMedium, in.Outcome, proofRefs); err != nil {
		return res, err
	}
	memoryCandidates := mergeUnique(w.MemoryCandidateIDs, in.MemoryCandidates)
	surprise := surpriseOnClose(w, in)

	// Receipt — how the run is learned from.
	rOut := receiptOutcome[in.Outcome]
	rec := envelope.Receipt{
		Header:   newCloseHeaderWithLane(envelope.SVReceipt, idFor("rec_", workID, ts(closeNow)), closeNow, workID, w.IDs.ContextPackID, w.Privacy, w.Risk, producerLaneID),
		Outcome:  rOut,
		MoveType: in.MoveType,
		Surprise: surprise,
		StateDelta: envelope.StateDelta{
			Changed:      in.Changed,
			Summary:      in.Summary,
			FilesChanged: []string{},
			Artifacts:    orEmpty(w.ArtifactIDs),
			Verification: orEmpty(w.VerificationIDs),
		},
		Cost: envelope.ReceiptCost{ModelCalls: []string{}},
		Skills: envelope.ReceiptSkills{
			Used:                usedCapabilities,
			Missed:              missedCapabilities,
			FalseTriggered:      falseTriggeredCapabilities,
			Candidate:           capabilityRefs,
			CapabilitiesOffered: capabilityRefs,
			UsedDeclared:        in.UsedDeclared,
			Referenced:          in.ReferencedRefs,
			ObservationMethod:   in.ObservationMethod,
			ObservationQuality:  in.ObservationQuality,
			ApplicationMethod:   in.ApplicationMethod,
			NonUseMethod:        in.NonUseMethod,
			NonUseActor:         in.Actor,
			ExplicitNonUse:      in.ExplicitNonUse,
			NonUseEvidence:      in.NonUseEvidence,
		},
		Memory: envelope.ReceiptMemory{
			Candidates: memoryCandidates,
			Promoted:   orEmpty(in.MemoryPromoted),
			Parked:     orEmpty(in.MemoryParked),
			Rejected:   orEmpty(in.MemoryRejected),
		},
		Next:      envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: proofRefs,
	}
	if fc := failureCause(in.FailureSurfaceOutcome, in.FailureMechanism); fc != nil {
		rec.FailureCause = fc
	}
	if len(in.ExplicitNonUse) > 0 && in.NonUseEvidence != "" {
		for _, proof := range rec.ProofRefs {
			if proof == in.NonUseEvidence {
				rec.EvidenceRefs = mergeUnique(rec.EvidenceRefs, []string{in.NonUseEvidence})
				break
			}
		}
	}
	// Legacy transcript matches must be validated with the receipt's observation
	// method and quality before any close artifact or recovery frame is persisted.
	if len(in.ConsumedRefs) > 0 {
		rec.Skills.Referenced = mergeUnique(rec.Skills.Referenced, in.ConsumedRefs)
	}
	rec.StateDelta.Changed = rec.EffectiveChanged()
	if err := rec.Validate(); err != nil {
		return res, fmt.Errorf("emitted receipt invalid: %w", err)
	}

	// Transition — the apex object: what changed across spaces, tied to this work_id.
	before := []string{}
	if w.IDs.ContextPackID != "" {
		before = []string{w.IDs.ContextPackID}
	}
	after := append(append([]string{}, orEmpty(w.ArtifactIDs)...), rec.ID)
	typedProjectionRefs := closeProjectionRefs(rec, w.IDs.ContextPackID)
	// The apex action_refs carry only ActionIntent ids: legacy envelopes still
	// hold prun_* run refs in ActionIntentIDs (see the field comment in
	// envelope.go), and those are run history, not actions.
	actionRefs := make([]string, 0, len(w.ActionIntentIDs))
	for _, id := range w.ActionIntentIDs {
		if strings.HasPrefix(id, "act_") {
			actionRefs = append(actionRefs, id)
		}
	}
	tr := envelope.Transition{
		Header:     newCloseHeaderWithLane(envelope.SVTransition, idFor("tr_", workID, ts(closeNow)), closeNow, workID, w.IDs.ContextPackID, w.Privacy, w.Risk, producerLaneID),
		BeforeRefs: before,
		ActionRefs: orEmpty(actionRefs),
		AfterRefs:  after,
		StateDelta: in.Summary,
		Projections: envelope.Projections{
			Memory: []string{}, Skill: skillProjectionRefs, Product: []string{}, Cost: []string{},
			Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{},
			Typed: typedProjectionRefs,
		},
		SecondOrder: envelope.SecondOrder{
			// Derived from the effective change, not the move class: a no-op
			// close (no effective change) must not claim future behavior
			// changed, or every branchless close ranks as changed and the
			// coverage rule ranks nothing.
			FutureBehaviorChanged: in.MoveType != "audit_only" && rec.StateDelta.Changed,
			ShouldHarden:          shouldHardenOnClose(w.TargetSurface, in.Outcome, in.ProofRefs),
		},
		OpensBranches: in.OpensBranches,
	}
	if err := tr.Validate(); err != nil {
		return res, fmt.Errorf("emitted transition invalid: %w", err)
	}
	terminalizationPlans, err := s.buildRunAttemptTerminalizationPlans(
		reservedWork, reservedWorkSHA256, w, plan, &rec, tr, in, inputSHA256, closeNow,
	)
	if err != nil {
		return res, err
	}
	terminalizationDigests, err := terminalizationDigestsForPlans(terminalizationPlans)
	if err != nil {
		return res, err
	}
	var frozenLandingProof *LandCheckReport
	var referenceRepositoryPath string
	var referenceExpectations []gitReferenceExpectation
	if landingProof != nil {
		currentProof, err := s.revalidateLandedProof(w, *landingProof, now)
		if err != nil {
			return res, err
		}
		proofCopy := *landingProof
		frozenLandingProof = &proofCopy
		referenceRepositoryPath = currentProof.RepoPath
		referenceExpectations = []gitReferenceExpectation{{
			Ref: currentProof.TargetBranchRef,
			SHA: currentProof.TargetSHA,
		}}
		if currentProof.PublicationMode == "required" {
			referenceExpectations = append(referenceExpectations, gitReferenceExpectation{
				Ref: currentProof.PublicationRef,
				SHA: currentProof.PublicationSHA,
			})
		}
		// Freeze repository identity before the pending frame. Recovery repeats
		// this validation and re-prepares the exact ref expectations.
		if err := s.validateBoundGitDelivery(w); err != nil {
			return res, fmt.Errorf("final repository identity revalidation: %w", err)
		}
	}
	// Decide lease authority read-only before preparing terminalization intents.
	// A denied close must keep its existing early audit trail and must never wedge
	// a RunAttempt behind a prepared intent. A valid steal is retained but does
	// not transfer: terminal override authorization belongs after the durable
	// recovery boundary and success is published with the terminal close event.
	closeLeaseStatus := s.leaseHolderStatusFor(w.ID, actor, in.Steal, in.StealReason, "close")
	if closeLeaseStatus.Err != nil {
		return res, closeLeaseStatus.Err
	}
	if closeLeaseStatus.Blocked {
		gateErr := s.recordLeaseHolderDeny(
			w,
			actor,
			closeLeaseStatus.Holder,
			closeLeaseStatus.Message,
			"work.close",
			"close_denied_not_lease_holder",
			now,
		)
		if gateErr == nil {
			gateErr = fmt.Errorf("cannot close %s: %s", w.ID, closeLeaseStatus.Message)
		}
		return res, gateErr
	}

	from := w.State
	w.State = in.Outcome
	w.IDs.TransitionID = tr.ID
	// Never persist an envelope that fails the current schema: the lifecycle
	// must not mint valid receipts/transitions citing an invalid spine object.
	if err := w.Validate(); err != nil {
		return res, fmt.Errorf("refusing to persist invalid terminal envelope: %w", err)
	}
	workSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		return res, err
	}
	receiptSHA256, err := receiptEnvelopeSHA256(rec)
	if err != nil {
		return res, err
	}
	closeEvent := Event{
		TS:                               ts(closeNow),
		WorkID:                           workID,
		From:                             from,
		To:                               in.Outcome,
		Note:                             "closed: " + in.MoveType + " (policy " + plan.Policy.Outcome + ")",
		Attached:                         []string{plan.ActionIntent.ID, plan.Decision.ID, rec.ID, tr.ID},
		PreviousWorkSHA256:               reservedWorkSHA256,
		WorkSHA256:                       workSHA256,
		ReceiptSHA256:                    receiptSHA256,
		RunAttemptTerminalizationDigests: terminalizationDigests,
	}
	if closeLeaseStatus.Stolen {
		closeEvent.By = actor
		closeEvent.Note = fmt.Sprintf(
			"lease stolen from %q: %s; %s",
			closeLeaseStatus.Holder,
			strings.TrimSpace(in.StealReason),
			closeEvent.Note,
		)
	}
	leaseSnapshot, capturedLease, err := s.captureClosePendingLease(workID)
	if err != nil {
		return res, err
	}
	frozenLeaseHolder := strings.TrimSpace(capturedLease.By)
	if closeLeaseStatus.Holder != "" && closeLeaseStatus.Holder != frozenLeaseHolder {
		return res, fmt.Errorf("close lease authority changed before recovery frame: expected holder %q, got holder %q", closeLeaseStatus.Holder, capturedLease.By)
	}
	pending, err := s.newClosePending(
		reservedWork, w, inputSHA256, in,
		plan.ActionIntent, plan.Decision, rec, tr, closeEvent,
		leaseSnapshot, frozenLeaseHolder, closeLeaseStatus.Stolen,
		terminalizationPlans, frozenLandingProof,
		referenceRepositoryPath, referenceExpectations,
	)
	if err != nil {
		return res, fmt.Errorf("build close recovery frame: %w", err)
	}
	if err := s.persistClosePending(pending); err != nil {
		return res, fmt.Errorf("persist close recovery frame: %w", err)
	}
	return s.completeClosePending(pending, in, now)
}

// terminalizePreparedRunAttempts writes only work-owned exact intents.
// Caller-owned plans remain mutable for post-close proof attachment. Failures
// remain recoverable from the durable intent and terminal close chain, and the
// returned ids make that non-fatal recovery obligation visible to the caller.
func (s *Store) terminalizePreparedRunAttempts(plans []runAttemptTerminalizationPlan, now time.Time) []string {
	store := s.runAttempts()
	var unterminalized []string
	for _, plan := range plans {
		if plan.CallerOwns {
			continue
		}
		if _, err := completePreparedTerminalization(store, plan.Attempt.ID, plan.Intent, now); err != nil {
			unterminalized = append(unterminalized, plan.Attempt.ID)
		}
	}
	return unterminalized
}

// completePreparedTerminalization is the production completion call and a
// deterministic test seam for post-publication failure and CAS-loss paths.
var completePreparedTerminalization = func(store *runattempt.Store, id string, intent runattempt.TerminalizationIntent, now time.Time) (runattempt.RunAttempt, error) {
	return store.CompletePreparedTerminalization(id, intent, now)
}

const RunAttemptReconcileReportSchemaVersion = "compound.runattempt_reconcile_report.v0.1"

type RunAttemptReconcileIssue struct {
	RunAttemptID string `json:"run_attempt_id,omitempty"`
	Code         string `json:"code"`
	Disposition  string `json:"disposition"`
	Message      string `json:"message"`
}

type RunAttemptReconcileReport struct {
	SchemaVersion        string                     `json:"schema_version"`
	Status               string                     `json:"status"`
	WorkID               string                     `json:"work_id"`
	Outcome              string                     `json:"outcome"`
	ReceiptID            string                     `json:"receipt_id,omitempty"`
	TransitionID         string                     `json:"transition_id,omitempty"`
	TargetStatus         string                     `json:"target_status"`
	ReconciledIDs        []string                   `json:"reconciled_ids,omitempty"`
	AlreadyConsistentIDs []string                   `json:"already_consistent_ids,omitempty"`
	HeldIDs              []string                   `json:"held_ids,omitempty"`
	Issues               []RunAttemptReconcileIssue `json:"issues,omitempty"`
}

type exactTerminalClose struct {
	Event      Event
	Events     []Event
	Action     envelope.ActionIntent
	Decision   envelope.PolicyDecision
	Receipt    envelope.Receipt
	Transition envelope.Transition
	WorkSHA256 string
}

func oneAttachedWithPrefix(values []string, prefix string) (string, error) {
	var found string
	for _, value := range values {
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		if found != "" && found != value {
			return "", fmt.Errorf("multiple %s attachments", prefix)
		}
		found = value
	}
	if found == "" {
		return "", fmt.Errorf("missing %s attachment", prefix)
	}
	return found, nil
}

func (s *Store) loadExactTerminalClose(w envelope.WorkEnvelope) (exactTerminalClose, error) {
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return exactTerminalClose{}, err
	}
	return s.loadExactTerminalCloseFromEvents(w, events)
}

func (s *Store) loadExactTerminalCloseFromEvents(w envelope.WorkEnvelope, events []Event) (exactTerminalClose, error) {
	var out exactTerminalClose
	workSHA, err := workEnvelopeSHA256(w)
	if err != nil {
		return out, err
	}
	var matches []Event
	for _, event := range events {
		if event.To == w.State && event.WorkSHA256 == workSHA {
			matches = append(matches, event)
		}
	}
	if len(matches) != 1 {
		return out, fmt.Errorf("terminal work %s has %d exact closing lifecycle events", w.ID, len(matches))
	}
	event := matches[0]
	if event.PreviousWorkSHA256 == "" {
		return out, fmt.Errorf("terminal work %s close event lacks previous_work_sha256", w.ID)
	}
	actionID, err := oneAttachedWithPrefix(event.Attached, "act_")
	if err != nil {
		return out, err
	}
	policyID, err := oneAttachedWithPrefix(event.Attached, "pol_")
	if err != nil {
		return out, err
	}
	receiptID, err := oneAttachedWithPrefix(event.Attached, "rec_")
	if err != nil {
		return out, err
	}
	transitionID, err := oneAttachedWithPrefix(event.Attached, "tr_")
	if err != nil {
		return out, err
	}
	if w.IDs.TransitionID != transitionID || latestActionIntentID(w) != actionID || w.PolicyDecisionID != policyID {
		return out, fmt.Errorf("terminal work %s does not retain exact close action/policy/transition generation", w.ID)
	}
	if err := s.readEventJSON(actionID, &out.Action); err != nil {
		return out, err
	}
	if err := s.readEventJSON(policyID, &out.Decision); err != nil {
		return out, err
	}
	if err := s.readEventJSON(receiptID, &out.Receipt); err != nil {
		return out, err
	}
	if err := s.readEventJSON(transitionID, &out.Transition); err != nil {
		return out, err
	}
	if err := out.Action.Validate(); err != nil || out.Action.IDs.WorkID != w.ID {
		return out, fmt.Errorf("closing action %s invalid or work-mismatched", actionID)
	}
	if err := out.Decision.Validate(); err != nil || out.Decision.IDs.WorkID != w.ID || out.Decision.ActionIntentID != actionID || !policy.Allowed(out.Decision) {
		return out, fmt.Errorf("closing policy %s invalid, denied, or action-mismatched", policyID)
	}
	if err := out.Receipt.Validate(); err != nil || out.Receipt.IDs.WorkID != w.ID || out.Receipt.Outcome != receiptOutcome[w.State] {
		return out, fmt.Errorf("closing receipt %s invalid, work-mismatched, or outcome-mismatched", receiptID)
	}
	if err := out.Transition.Validate(); err != nil || out.Transition.IDs.WorkID != w.ID || !slices.Contains(out.Transition.ActionRefs, actionID) || !slices.Contains(out.Transition.AfterRefs, receiptID) {
		return out, fmt.Errorf("closing transition %s invalid or does not bind action/receipt", transitionID)
	}
	receiptSHA, err := receiptEnvelopeSHA256(out.Receipt)
	if err != nil || event.ReceiptSHA256 != receiptSHA {
		return out, fmt.Errorf("closing event receipt digest does not match receipt %s", receiptID)
	}
	out.Event = event
	out.Events = append([]Event(nil), events...)
	out.WorkSHA256 = workSHA
	return out, nil
}

func exactTerminalizationDigest(event Event, runAttemptID string) (string, error) {
	var found string
	for _, binding := range event.RunAttemptTerminalizationDigests {
		if !runattempt.Valid(binding.RunAttemptID) || binding.IntentSHA256 == "" {
			return "", fmt.Errorf("closing event carries invalid RunAttempt terminalization digest binding for %q", binding.RunAttemptID)
		}
		if binding.RunAttemptID != runAttemptID {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("closing event duplicates terminalization digest for %s", runAttemptID)
		}
		found = binding.IntentSHA256
	}
	if found == "" {
		return "", fmt.Errorf("closing event lacks terminalization digest for %s", runAttemptID)
	}
	return found, nil
}

func (s *Store) validateTerminalizationIntentAgainstClose(w envelope.WorkEnvelope, ra runattempt.RunAttempt, intent runattempt.TerminalizationIntent, close exactTerminalClose) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if intent.WorkID != w.ID || intent.Outcome != w.State || intent.ReceiptID != close.Receipt.ID || intent.TransitionID != close.Transition.ID ||
		intent.CloseActionIntentID != close.Action.ID || intent.ClosePolicyDecisionID != close.Decision.ID || intent.ExpectedWorkSHA256 != close.Event.PreviousWorkSHA256 {
		return errors.New("terminalization intent does not bind the exact terminal close generation")
	}
	if ra.ActionIntentID != intent.RunAttemptActionIntentID {
		return fmt.Errorf("runattempt action binding %q differs from prepared %q", ra.ActionIntentID, intent.RunAttemptActionIntentID)
	}
	actualRunPolicy := ""
	if ra.ExecutionPosture != nil {
		actualRunPolicy = ra.ExecutionPosture.PolicyDecisionID
	}
	if actualRunPolicy != intent.RunAttemptPolicyDecisionID {
		return fmt.Errorf("runattempt policy binding %q differs from prepared %q", actualRunPolicy, intent.RunAttemptPolicyDecisionID)
	}
	authority, err := s.resolveRunAttemptAuthorityFromEvents(w, ra, close.Events)
	if err != nil {
		return err
	}
	if authority.ActionIntentID != intent.AuthorityActionIntentID || authority.PolicyDecisionID != intent.AuthorityPolicyDecisionID {
		return errors.New("runattempt authority generation differs from prepared intent")
	}
	wantDigest, err := exactTerminalizationDigest(close.Event, ra.ID)
	if err != nil {
		return err
	}
	gotDigest, err := terminalizationIntentSHA256(intent)
	if err != nil {
		return err
	}
	if gotDigest != wantDigest {
		return fmt.Errorf("runattempt terminalization intent digest %s differs from closing event %s", gotDigest, wantDigest)
	}
	precloseMatches := 0
	for _, event := range close.Events {
		if event.WorkSHA256 != intent.ExpectedWorkSHA256 || event.To != intent.ExpectedWorkState {
			continue
		}
		if intent.ExpectedActionIntentID != "" && !attachedHas(event.Attached, intent.ExpectedActionIntentID) {
			continue
		}
		if intent.ExpectedPolicyDecisionID != "" && !attachedHas(event.Attached, intent.ExpectedPolicyDecisionID) {
			continue
		}
		precloseMatches++
	}
	if precloseMatches != 1 {
		return fmt.Errorf("prepared pre-close work/action/policy generation has %d exact lifecycle matches", precloseMatches)
	}
	return nil
}

func reconcileIssue(report *RunAttemptReconcileReport, id, code, message string) {
	report.Status = "held"
	report.Issues = append(report.Issues, RunAttemptReconcileIssue{
		RunAttemptID: id,
		Code:         code,
		Disposition:  "manual_review",
		Message:      message,
	})
	if id != "" && !slices.Contains(report.HeldIDs, id) {
		report.HeldIDs = append(report.HeldIDs, id)
	}
}

// ReconcileRunAttemptsForTerminalWork repairs only attempts carrying an exact
// pre-close terminalization intent. Missing, malformed, foreign, stale, or
// contradictory evidence is held for manual review and never inferred.
func (s *Store) ReconcileRunAttemptsForTerminalWork(workID string, now time.Time) (RunAttemptReconcileReport, error) {
	report := RunAttemptReconcileReport{
		SchemaVersion: RunAttemptReconcileReportSchemaVersion,
		Status:        "ok",
		WorkID:        workID,
	}
	w, err := s.Get(workID)
	if err != nil {
		return report, err
	}
	report.Outcome = w.State
	report.TargetStatus = runAttemptStatusForCloseOutcome(w.State)
	if !terminalStates[w.State] {
		return report, fmt.Errorf("work %s is %q, not terminal", workID, w.State)
	}
	closeEvidence, err := s.loadExactTerminalClose(w)
	if err != nil {
		reconcileIssue(&report, "", "close_evidence_mismatch", err.Error())
		return report, nil
	}
	report.ReceiptID = closeEvidence.Receipt.ID
	report.TransitionID = closeEvidence.Transition.ID

	type eligible struct {
		id     string
		intent runattempt.TerminalizationIntent
	}
	var eligibleAttempts []eligible
	store := s.runAttempts()
	for _, id := range dedupeStrings(w.RunAttemptIDs) {
		if !runattempt.Valid(id) {
			reconcileIssue(&report, id, "run_attempt_invalid", "invalid RunAttempt reference")
			continue
		}
		ra, err := store.Get(id)
		if err != nil {
			reconcileIssue(&report, id, "run_attempt_unreadable", err.Error())
			continue
		}
		if err := validateRunAttemptRecord(id, w, ra); err != nil {
			reconcileIssue(&report, id, "run_attempt_mismatch", err.Error())
			continue
		}
		if ra.TerminalizationIntent == nil {
			if runattempt.IsTerminal(ra.Status) && (ra.Status == runattempt.StatusSuperseded || ra.Status == report.TargetStatus) {
				report.AlreadyConsistentIDs = append(report.AlreadyConsistentIDs, id)
				continue
			}
			reconcileIssue(&report, id, "terminalization_intent_missing", "no exact pre-close terminalization intent; automatic repair is forbidden")
			continue
		}
		intent := *ra.TerminalizationIntent
		if err := s.validateTerminalizationIntentAgainstClose(w, ra, intent, closeEvidence); err != nil {
			reconcileIssue(&report, id, "terminalization_intent_mismatch", err.Error())
			continue
		}
		if ra.Status == intent.TargetStatus && ra.TerminalizationOwner == intent.Owner && ra.TerminalizationReceiptID == intent.ReceiptID && ra.TerminalizationTransitionID == intent.TransitionID && ra.TerminalizationReason == intent.TerminalizationReason {
			report.AlreadyConsistentIDs = append(report.AlreadyConsistentIDs, id)
			continue
		}
		if runattempt.IsTerminal(ra.Status) {
			reconcileIssue(&report, id, "terminal_status_conflict", fmt.Sprintf("terminal status %s conflicts with prepared %s", ra.Status, intent.TargetStatus))
			continue
		}
		eligibleAttempts = append(eligibleAttempts, eligible{id: id, intent: intent})
	}
	if report.Status == "held" {
		return report, nil
	}
	for _, candidate := range eligibleAttempts {
		if _, err := completePreparedTerminalization(store, candidate.id, candidate.intent, now); err != nil {
			reconcileIssue(&report, candidate.id, "terminalization_cas_lost", err.Error())
			continue
		}
		report.ReconciledIDs = append(report.ReconciledIDs, candidate.id)
	}
	if len(report.ReconciledIDs) > 0 && len(report.Issues) > 0 {
		report.Status = "partially_reconciled"
	}
	if report.Status == "ok" {
		if err := s.finishReconciledClosePending(workID); err != nil {
			return report, err
		}
	}
	return report, nil
}

// releaseLease deletes workID's claim lease, if one is still held. Called only
// from closeLocked, after the terminal state is durable (see the invariant
// comment there).
//
// An absent lease is the normal no-op: close has always been legal for work
// that was never claimed, and re-closing already-terminal work must stay
// idempotent.
//
// The result is deliberately dropped rather than returned, mirroring
// abandonRunAttempts: the close is already committed by the time this runs, so
// surfacing a removal failure would report a close that actually succeeded as
// failed. The failure is not silent — a lease that survives its terminal work
// is exactly what home.OperationalHygiene counts in StaleLeases (doctor gate
// hygiene:no-stale-leases) and what `home archive-history --kind lease` sweeps
// as the orphan backstop.
func (s *Store) releaseLease(workID string) {
	_ = removeFileAndSyncDir(s.leasePath(workID))
}

// WriteResidualLeaseForTerminalWork plants the lease residue a terminal work can
// still carry after I1, for callers that need to construct that state rather than
// wait for it to occur.
//
// It exists because I1 made the state unreachable through the public API: Advance
// refuses terminal states (terminalStateRequiresClose) and Close now releases the
// lease in the same publish, so no ordinary sequence leaves a terminal work holding
// one. The residue is still REAL — a home written before that release existed, a
// crash inside the close's release window, and work archived out from under its
// lease all produce it — and the sweep and the proof-check agents gate both have to
// exercise a non-empty residue population to assert anything at all. Without this,
// a fixture that used to get residue from a plain close silently gets an empty set,
// and the gates compare nothing.
//
// The lease layout stays owned here; callers pass intent, not paths.
func (s *Store) WriteResidualLeaseForTerminalWork(workID, by string, at time.Time) error {
	w, err := s.Get(workID)
	if err != nil {
		return err
	}
	if !terminalStates[w.State] {
		return fmt.Errorf("residual-lease fixture needs terminal work, %s is %q", workID, w.State)
	}
	lease := Lease{WorkID: workID, By: by, ClaimAt: ts(at)}
	return s.writeJSON(s.leasePath(workID), &lease)
}

func landCheckEvidenceRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if _, ok := landCheckIDFromEvidenceRef(ref); ok {
			out = append(out, ref)
		}
	}
	return out
}

// --- persistence helpers ---

func (s *Store) writeJSON(path string, v any) error {
	if strings.HasPrefix(path, "compound://events/") {
		ref, err := eventstore.ParseReference(path)
		if err != nil {
			return err
		}
		return s.writeEventJSON(ref.ID(), v)
	}
	stagedPath, err := s.stageJSON(path, v)
	if err != nil {
		return err
	}
	if err := commitStagedJSON(stagedPath, path); err != nil {
		_ = os.Remove(stagedPath)
		return err
	}
	return nil
}

func (s *Store) writeEventJSON(id string, v any) error {
	_, err := s.events.PutJSON(id, v, "  ")
	return err
}

var writeStagedJSONFile = func(file *os.File, data []byte) error {
	_, err := file.Write(data)
	return err
}

func (s *Store) stageJSON(path string, v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".stage-")
	if err != nil {
		return "", err
	}
	stagedPath := file.Name()
	cleanup := func(cause error) (string, error) {
		_ = file.Close()
		_ = os.Remove(stagedPath)
		return "", cause
	}
	if err := file.Chmod(0o644); err != nil {
		return cleanup(err)
	}
	if err := writeStagedJSONFile(file, append(data, '\n')); err != nil {
		return cleanup(err)
	}
	if err := file.Sync(); err != nil {
		return cleanup(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(stagedPath)
		return "", err
	}
	return stagedPath, nil
}

func commitStagedJSON(stagedPath, targetPath string) error {
	if err := os.Rename(stagedPath, targetPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(targetPath))
}

func commitStagedJSONExclusive(stagedPath, targetPath string) error {
	if err := os.Link(stagedPath, targetPath); err != nil {
		return err
	}
	if err := os.Remove(stagedPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(targetPath))
}

func removeFileAndSyncDir(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) {
		return syncErr
	}
	return closeErr
}

func workEnvelopeSHA256(work envelope.WorkEnvelope) (string, error) {
	return canonicalEnvelopeSHA256(work)
}

func receiptEnvelopeSHA256(receipt envelope.Receipt) (string, error) {
	return canonicalEnvelopeSHA256(receipt)
}

func canonicalEnvelopeSHA256(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (s *Store) capabilityRefsForContextPack(contextPackID string) ([]string, error) {
	if strings.TrimSpace(contextPackID) == "" {
		return []string{}, nil
	}
	path := filepath.Join(s.home, "context", "packs", contextPackID+".json")
	var pack envelope.ContextPack
	if err := s.readJSON(path, &pack); err != nil {
		if strings.HasPrefix(err.Error(), "not found:") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("read context pack %s for capability refs: %w", contextPackID, err)
	}
	if err := pack.Validate(); err != nil {
		return nil, fmt.Errorf("context pack %s invalid: %w", contextPackID, err)
	}
	seen := map[string]bool{}
	refs := []string{}
	for _, offer := range pack.CapabilityOffers {
		ref := normalizeCapabilityRef(offer.Ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	for _, included := range pack.Included {
		ref := normalizeCapabilityRef(included.Ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs, nil
}

func (s *Store) usedCapabilitiesForRuns(runIDs, offeredRefs []string) ([]string, error) {
	seen := map[string]bool{}
	used := []string{}
	offeredByNode := capabilityRefsByNodeID(offeredRefs)
	for _, runID := range runIDs {
		var ai envelope.ActionIntent
		if err := s.readEventJSON(runID, &ai); err != nil {
			if strings.HasPrefix(err.Error(), "not found:") {
				continue
			}
			return nil, fmt.Errorf("read action intent %s for used capabilities: %w", runID, err)
		}
		if err := ai.Validate(); err != nil {
			return nil, fmt.Errorf("action intent %s invalid: %w", runID, err)
		}
		if strings.TrimSpace(ai.Capability) == "" || internalLifecycleCapability(ai.Capability) {
			continue
		}
		ref := normalizeCapabilityRef(ai.Capability)
		if ref == "" {
			continue
		}
		if offeredRef := offeredByNode[capabilityNodeID(ref)]; offeredRef != "" {
			ref = offeredRef
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		used = append(used, ref)
	}
	return used, nil
}

func normalizeCapabilityRefs(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		ref := normalizeCapabilityRef(value)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

func normalizeCapabilityRef(value string) string {
	nodeID := capabilityNodeID(value)
	if !realCapabilityNodeID(nodeID) {
		return ""
	}
	return "capability://" + nodeID
}

func capabilityRefsByNodeID(refs []string) map[string]string {
	out := map[string]string{}
	for _, ref := range refs {
		normalized := normalizeCapabilityRef(ref)
		if normalized == "" {
			continue
		}
		out[capabilityNodeID(normalized)] = normalized
	}
	return out
}

func capabilityNodeID(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "capability://")
	return strings.TrimSpace(ref)
}

func realCapabilityNodeID(nodeID string) bool {
	for _, prefix := range []string{
		"lens:",
		"procedure:",
		"script:",
		"tool:",
		"mcp:",
		"model_profile:",
		"harness:",
		"eval:",
		"workflow:",
		"adapter:",
		"doctrine:",
		"taste_standard:",
		"skill:",
		"instruction_rule:",
	} {
		if strings.HasPrefix(nodeID, prefix) && strings.TrimSpace(strings.TrimPrefix(nodeID, prefix)) != "" {
			return true
		}
	}
	return false
}

func internalLifecycleCapability(capability string) bool {
	switch capability {
	case "work.create", "work.claim", "work.advance", "work.close":
		return true
	default:
		return false
	}
}

func validateProofMedium(medium, outcome string, proofRefs []string) error {
	medium = strings.TrimSpace(strings.ToLower(medium))
	if medium == "" || medium == "none" || outcome != "completed" {
		return nil
	}
	switch medium {
	case "web-ui":
		if !containsProofText(proofRefs, "screenshot") || !containsAnyProofText(proofRefs, []string{"design-review", "design_review"}) {
			return fmt.Errorf("proof_medium web-ui requires screenshot artifact and design-review verdict proof refs")
		}
	case "ios":
		if !containsProofText(proofRefs, "screenshot") {
			return fmt.Errorf("proof_medium ios requires screenshot proof ref")
		}
	case "cli", "api":
		if !containsAnyProofText(proofRefs, []string{"command-output", "command_output", "test", "tests"}) {
			return fmt.Errorf("proof_medium %s requires command-output or test proof ref", medium)
		}
	default:
		return fmt.Errorf("proof_medium %q is not supported", medium)
	}
	return nil
}

func containsProofText(values []string, needle string) bool {
	return containsAnyProofText(values, []string{needle})
}

func containsAnyProofText(values []string, needles []string) bool {
	for _, value := range values {
		lower := strings.ToLower(value)
		for _, needle := range needles {
			if strings.Contains(lower, strings.ToLower(needle)) {
				return true
			}
		}
	}
	return false
}

func mergeUnique(groups ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, group := range groups {
		for _, v := range group {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func closeProjectionRefs(rec envelope.Receipt, contextPackID string) []envelope.ProjectionRef {
	var out []envelope.ProjectionRef
	add := func(kind, ref, role, evidence string) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return
		}
		out = append(out, envelope.ProjectionRef{Kind: kind, Ref: ref, Role: role, EvidenceRef: evidence})
	}
	for _, ref := range rec.Skills.Candidate {
		add("skill", ref, "offered", contextPackID)
	}
	for _, ref := range rec.Skills.Used {
		add("skill", ref, "used", rec.ID)
	}
	for _, ref := range rec.Skills.Missed {
		add("skill", ref, "missed", rec.ID)
	}
	for _, ref := range rec.Skills.FalseTriggered {
		add("skill", ref, "false_triggered", rec.ID)
	}
	for _, ref := range rec.Memory.Candidates {
		add("memory", ref, "candidate", rec.ID)
	}
	for _, ref := range rec.Memory.Promoted {
		add("memory", ref, "promoted", rec.ID)
	}
	for _, ref := range rec.Memory.Parked {
		add("memory", ref, "parked", rec.ID)
	}
	for _, ref := range rec.Memory.Rejected {
		add("memory", ref, "rejected", rec.ID)
	}
	for _, ref := range rec.ProofRefs {
		if strings.HasPrefix(ref, "failure:") || strings.HasPrefix(ref, "failure_pattern:") || strings.HasPrefix(ref, "rule:") || strings.HasPrefix(ref, "check:") || strings.HasPrefix(ref, "guard:") {
			add("rule", ref, "proof", rec.ID)
			continue
		}
		add("control_plane", ref, "proof", rec.ID)
	}
	return out
}

func (s *Store) readJSON(path string, v any) error {
	if strings.HasPrefix(path, "compound://events/") {
		return s.readEventJSON(path, v)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("not found: %s: %w", filepath.Base(path), err)
		}
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Store) readEventJSON(ref string, v any) error {
	record, err := s.events.Read(ref)
	return decodeEventJSON(ref, record, err, v)
}

// decodeEventJSON preserves the same error and JSON semantics for point and
// request-local batch readers.
func decodeEventJSON(ref string, record eventstore.Record, err error, v any) error {
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			id := ref
			if parsed, parseErr := eventstore.ParseReference(ref); parseErr == nil {
				id = parsed.ID()
			}
			return fmt.Errorf("not found: %s.json: %w", id, err)
		}
		return err
	}
	return json.Unmarshal(record.Bytes, v)
}

func (s *Store) readLease(workID string) (Lease, error) {
	var l Lease
	err := s.readJSON(s.leasePath(workID), &l)
	return l, err
}

// ListLeases returns the read-only lease projection for claimed work. The work
// package owns the lease files; operator surfaces should consume this projection
// instead of reading COMPOUND_HOME/work/leases directly.
func (s *Store) ListLeases() ([]Lease, error) {
	dir := filepath.Join(s.home, "work", "leases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	leases := make([]Lease, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var lease Lease
		if err := s.readJSON(filepath.Join(dir, entry.Name()), &lease); err != nil {
			return nil, err
		}
		if strings.TrimSpace(lease.WorkID) == "" {
			lease.WorkID = strings.TrimSuffix(entry.Name(), ".json")
		}
		leases = append(leases, lease)
	}
	sort.Slice(leases, func(i, j int) bool {
		if leases[i].ClaimAt == leases[j].ClaimAt {
			return leases[i].WorkID < leases[j].WorkID
		}
		return leases[i].ClaimAt < leases[j].ClaimAt
	})
	return leases, nil
}

var writeWorkEvent = func(file *os.File, data []byte) (int, error) { return file.Write(data) }

func (s *Store) appendEvent(e Event) error {
	return s.withWorkEventLock(func() error {
		if _, err := s.readEventsStrictUnlocked(""); err != nil {
			return err
		}
		return s.appendEventUnlocked(e)
	})
}

func (s *Store) appendEventUnlocked(e Event) error {
	b, err := marshalWorkEvent(e)
	if err != nil {
		return err
	}
	// The caller holds the stable sidecar lock before eventstore opens the
	// stream, so controlled publication cannot replace the inode while waiting.
	return s.events.AppendWorkLog(b, writeWorkEventThrough)
}

// appendEventControlled publishes one JSONL append by atomically replacing the
// canonical log. The receipted-action transaction uses this rarer O(n) path so
// a killed writer can leave only a disposable temp file, never a torn canonical
// tail. beforePublish runs after the temp file is durable but before rename;
// afterPublish runs after rename and directory sync.
func (s *Store) appendEventControlled(e Event, beforePublish, afterPublish func() error) error {
	return s.withWorkEventLock(func() error {
		return s.appendEventControlledUnlocked(e, beforePublish, afterPublish)
	})
}

func (s *Store) appendEventControlledUnlocked(e Event, beforePublish, afterPublish func() error) error {
	b, err := marshalWorkEvent(e)
	if err != nil {
		return err
	}
	current, err := s.events.ReadWorkLog()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := decodeAdmissionEvents(current, ""); err != nil {
		return err
	}
	return s.events.ReplaceWorkLog(current, b, eventstore.WorkLogReplaceOptions{
		WriteSuffix:   writeWorkEventThrough,
		BeforePublish: beforePublish,
		AfterPublish:  afterPublish,
	})
}

func marshalWorkEvent(e Event) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (s *Store) withWorkEventLock(fn func() error) error {
	if s.readOnly {
		return s.events.WithWorkLogReadLock(fn)
	}
	return s.events.WithWorkLogLock(fn)
}

func writeWorkEventThrough(writer io.Writer, data []byte) (int, error) {
	file, ok := writer.(*os.File)
	if !ok {
		return 0, fmt.Errorf("work event writer is %T, want *os.File", writer)
	}
	return writeWorkEvent(file, data)
}

func (s *Store) eventsFor(workID string) ([]Event, error) {
	// Read-only fast path: when the lifecycle log does not exist there are no
	// events to return and no concurrent append to serialize against, so the
	// read returns empty WITHOUT taking the work-event lock. Taking that lock
	// materializes runtime state (eventstore.Ensure creates events/, and the
	// lock file lands inside it) — a footprint work.OpenReadOnly exists to
	// forbid, and the read-classified commands (workbench, attention,
	// coordination, narrate, executors, doctor) reach eventsFor through every
	// lifecycle projection. readEventsStrictUnlocked below already maps an
	// absent log to empty, so a Store opened for mutation sees identical
	// behavior either way; the log-exists branch keeps the lock because a
	// concurrent append may be mid-flight and the strict read demands a
	// stable tail.
	if exists, err := s.events.WorkLogExists(); err != nil || !exists {
		return nil, err
	}
	var events []Event
	err := s.withWorkEventLock(func() error {
		var err error
		events, err = s.readEventsStrictUnlocked(workID)
		return err
	})
	return events, err
}

func (s *Store) readEventsStrictUnlocked(workID string) ([]Event, error) {
	s.eventLogReads.Add(1)
	b, err := s.events.ReadWorkLog()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return decodeAdmissionEvents(b, workID)
}

// eventsByWorkID reads and strictly decodes the whole event log ONCE, under
// the same work-event lock as eventsFor, and groups the result by work_id.
// It exists for batch callers (the workbench report build) that would
// otherwise call eventsFor once per work item and pay a full log
// re-read+re-decode per item; eventsFor stays the correct call for existing
// single-work callers (dispatch, continue, show) and is untouched.
func (s *Store) eventsByWorkID() (map[string][]Event, error) {
	// Same read-only fast path as eventsFor: no log means no events and no
	// concurrent writer, so the batch read returns empty without taking the
	// lock — taking it would materialize events/ under work.OpenReadOnly.
	if exists, err := s.events.WorkLogExists(); err != nil || !exists {
		return map[string][]Event{}, err
	}
	var all []Event
	err := s.withWorkEventLock(func() error {
		var err error
		all, err = s.readAllEventsStrictUnlocked()
		return err
	})
	if err != nil {
		return nil, err
	}
	byWorkID := make(map[string][]Event, len(all))
	for _, e := range all {
		byWorkID[e.WorkID] = append(byWorkID[e.WorkID], e)
	}
	return byWorkID, nil
}

func (s *Store) readAllEventsStrictUnlocked() ([]Event, error) {
	s.eventLogReads.Add(1)
	b, err := s.events.ReadWorkLog()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return decodeAdmissionEvents(b, "")
}

// --- small helpers ---

func newHeader(sv, id string, now time.Time, workID, contextPackID, privacy, risk string) envelope.Header {
	return newHeaderWithLane(sv, id, now, workID, contextPackID, privacy, risk, "")
}

func newCloseHeader(sv, id string, now time.Time, workID, contextPackID, privacy, risk string) envelope.Header {
	return newCloseHeaderWithLane(sv, id, now, workID, contextPackID, privacy, risk, "")
}

func newCloseHeaderWithLane(sv, id string, now time.Time, workID, contextPackID, privacy, risk, laneID string) envelope.Header {
	header := newHeaderWithLane(sv, id, now, workID, contextPackID, privacy, risk, laneID)
	header.CreatedAt = now.UTC().Format(time.RFC3339Nano)
	return header
}

// newHeaderWithLane is newHeader plus a declared lane id stamped into Producer.LaneID. laneID
// is a canonical agent-lane node id (resolved via capgraph.ResolveLaneRef) or empty. With
// laneID=="" the Producer is byte-identical to newHeader's (lane_id is omitempty), so every
// existing newHeader call site is unchanged; only the close-minted Receipt/Transition pass a
// non-empty lane, and only when one was declared on the lifecycle (no-guessing, B0).
func newHeaderWithLane(sv, id string, now time.Time, workID, contextPackID, privacy, risk, laneID string) envelope.Header {
	return envelope.Header{
		SchemaVersion: sv,
		ID:            id,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Producer:      envelope.Producer{Kind: "cli", Name: "compound", LaneID: laneID},
		IDs:           envelope.IDs{WorkID: workID, ContextPackID: contextPackID},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       orDefault(privacy, "private"),
		Risk:          orDefault(risk, "tier1"),
		EvidenceRefs:  []string{},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}

// closeLaneID resolves the lane stamped onto close-minted receipts/transitions. Precedence:
// an explicit in.LaneID override (resolver-gated, fail-closed) wins; otherwise the DECLARED
// lane recorded on the work's lease at claim/advance time (already a canonical node id). No
// override and no lease-declared lane → empty. Never derived from actor/owner/harness.
func (s *Store) closeLaneID(workID, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return s.resolveDeclaredLane(override)
	}
	lease, err := s.readLease(workID)
	if err != nil {
		return "", nil // no lease = no declared lane = empty (no-guessing)
	}
	return strings.TrimSpace(lease.LaneID), nil
}

func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(h[:])[:12]
}

func ts(now time.Time) string { return strconv.FormatInt(now.UTC().UnixNano(), 10) }

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// surpriseOnClose diffs a staked WorkPrediction (envelope.WorkEnvelope.Prediction) against the
// actual close, mirroring shouldHardenOnClose as a small pure close-time helper. Nil when the
// work staked no prediction. When staked, it ALWAYS returns a signal -- Diverged=false (a
// confirmed prediction) is signal too, not merely the absence of a divergence flag. A prediction
// dimension left unstaked (empty MoveType) is not compared and can never itself cause
// Diverged=true.
func surpriseOnClose(w envelope.WorkEnvelope, in CloseInput) *envelope.SurpriseSignal {
	p := w.Prediction
	if p == nil {
		return nil
	}
	diverged := p.Outcome != in.Outcome
	if !diverged && p.MoveType != "" {
		diverged = p.MoveType != in.MoveType
	}
	return &envelope.SurpriseSignal{
		PredictedOutcome:  p.Outcome,
		PredictedMoveType: p.MoveType,
		ActualOutcome:     in.Outcome,
		ActualMoveType:    in.MoveType,
		Diverged:          diverged,
	}
}

// shouldHardenOnClose reports whether a close should stamp
// SecondOrder.ShouldHarden=true. It is true only on an ACCEPTED close (outcome
// "completed") of an explicit harness-edit envelope — one that declared a
// TargetSurface (EVOLVE-BLOCK) — AND that close carries a bench-compare ACCEPT
// proof ref, so "completed" cannot self-certify a harness edit. A rejected/failed/
// cancelled close of the same envelope stamps false but still emits its Receipt +
// Transition, so "rejections are logged without changing the active harness." An
// ordinary work (no TargetSurface) never hardens on close. Derived, not a new
// CloseInput flag: the close outcome already carries whether the harness edit was
// accepted; the proof ref carries whether the gate actually accepted it.
func shouldHardenOnClose(targetSurface, outcome string, proofRefs []string) bool {
	return strings.TrimSpace(targetSurface) != "" &&
		outcome == "completed" &&
		hasBenchCompareAcceptProof(proofRefs)
}

// hasBenchCompareAcceptProof reports whether the close's proof refs contain a
// bench-compare accept record. Accepted spellings include bench_compare_accept:<id>
// and bench_compare:accept:<id>; matched case-insensitively on a ref that contains
// both "bench_compare" and "accept". A plain bench_report:... ref does not qualify —
// the harness edit must prove the gate accepted, not merely that a report exists.
func hasBenchCompareAcceptProof(refs []string) bool {
	for _, r := range refs {
		r = strings.ToLower(strings.TrimSpace(r))
		if strings.Contains(r, "bench_compare") && strings.Contains(r, "accept") {
			return true
		}
	}
	return false
}

// failureCause builds the optional causal-failure annotation; nil when both
// fields are empty so the receipt stays byte-identical to pre-field behavior.
func failureCause(surfaceOutcome, mechanism string) *envelope.FailureCause {
	surfaceOutcome = strings.TrimSpace(surfaceOutcome)
	mechanism = strings.TrimSpace(mechanism)
	if surfaceOutcome == "" && mechanism == "" {
		return nil
	}
	return &envelope.FailureCause{SurfaceOutcome: surfaceOutcome, Mechanism: mechanism}
}
