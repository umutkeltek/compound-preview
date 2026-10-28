package runattempt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	homepkg "compound/internal/home"
)

// Status enumerates the lifecycle of a RunAttempt. S1 only ever produces "pending" /
// "running" / "succeeded" / "failed"; later slices add heartbeat-driven transitions and
// terminal proof attachment. C1 adds "abandoned" for claim-release (claimed→ready):
// the attempt was minted at claim but the work was released without execution.
// Abandoned is terminal (same as succeeded/failed) — a released attempt cannot be
// re-entered; a fresh claim mints a fresh RunAttempt.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusAbandoned = "abandoned"
	// StatusSuperseded is the terminal status of a claim_reservation placeholder
	// that a real harness_execution replaced (F2, supersede-at-attach). It is NOT
	// a failure: it is the ordinary success path of the reservation protocol.
	//
	// It exists because "abandoned" was carrying both meanings, and the benign one
	// is ~10x more common: on a live home 330 of 400 sampled abandoned attempts
	// were superseded placeholders and only 38 were harness_execution attempts
	// that actually died. A ledger that is 83% benign-labelled-as-failure trains
	// its readers to ignore the label, which is how a genuinely dead controller
	// sat unnoticed holding 183 leases.
	StatusSuperseded = "superseded"
)

// SweepClassification enumerates how a stale non-terminal RunAttempt was
// disposed of by the sweep (DEC-0027 build step 1, work_195771b63a58).
// Empty on normally-completed attempts and on attempts the sweep did not
// touch (i.e. still non-terminal, or pre-sweep-teeth).
const (
	// SweepClassificationSideEffectFree: the attempt produced no artifacts we
	// have to preserve — no transcript_ref, diff_ref, test_ref, and not an
	// observed-external origin. The sweep auto-closes; operator review is
	// not required.
	SweepClassificationSideEffectFree = "side_effect_free"
	// SweepClassificationAmbiguous: the attempt MAY have left state behind
	// that the runattempt package cannot prove is clean. Sweep terminalizes
	// the attempt as abandoned, but the operator MUST reconcile the
	// side effects. Surfaced via doctor's StuckSweptAmbiguous counter and
	// via proof_refs entries prefixed "sweep:needs_me:".
	SweepClassificationAmbiguous = "ambiguous"
)

// Origin records which of the three RunAttempt producers created this record:
// a claim-time placeholder reservation, a harness that actually executed, or
// a post-hoc observation of external evidence (e.g. a codex job file). Empty
// on pre-F2 records -- additive field, no migration (omitempty keeps old
// records marshaling byte-identical). F2 (supersede-at-attach) reads Origin
// to terminalize a stale claim_reservation attempt when a harness_execution
// attempt attaches to the same work envelope; attempts with empty Origin
// (legacy) are never touched by that logic.
const (
	OriginClaimReservation = "claim_reservation"
	OriginHarnessExecution = "harness_execution"
	OriginObservedExternal = "observed_external"
)

// HarnessPlaceholder is the stand-in harness identifier used by writers that do not yet have a
// real harness adapter (today: panel.Apply). It is a named constant rather than an open string so
// the future harness-adapter slice has a single replacement site; the deferred context_pack_ref /
// workspace / command / heartbeat / transcript_ref / diff_ref / test_ref / proof_refs fields
// (see the package doc in id.go) land with that adapter, not before.
const HarnessPlaceholder = "fake"

// ErrNotFound is returned when a Get lookup cannot resolve the requested id.
var ErrNotFound = errors.New("runattempt not found")

// ErrAlreadyBound is returned by AttachActionIntent when the RunAttempt is already bound to a
// DIFFERENT ActionIntent. Attaching the same intent twice is a no-op success (retry-safe); a
// mismatched rebind is a bug, not a refresh.
var ErrAlreadyBound = errors.New("runattempt already bound to a different action_intent_id")

// ExecutionPostureEvidence is one immutable authority revision that justified
// the permission posture selected for a governed harness execution. It carries
// references and revisions only; no provider credentials or prompt bodies.
type ExecutionPostureEvidence struct {
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
}

// ExecutionPosture records the resolved (not merely requested) permission
// mode and the actual PolicyDecision that advanced the work to running. For a
// mutation-capable posture, AuthorityEvidence contains the exact permission
// and blocking hook revisions checked before that advance.
type ExecutionPosture struct {
	PermissionMode    string                     `json:"permission_mode"`
	PolicyDecisionID  string                     `json:"policy_decision_id"`
	AuthorityEvidence []ExecutionPostureEvidence `json:"authority_evidence,omitempty"`
}

// LaunchLifecycle is the materialized, content-free boundary around starting
// an external harness process. Its immutable events are custodied separately
// beneath run_attempts/launch rather than in the v0.1 main JSON. PreparedAt is
// committed before the start callback. A successful callback is followed by
// CommittedAt; a returned start error is followed by StartErrorAt. A process
// crash or disposition-write failure can leave a durable prepared-only record
// for stale recovery to escalate.
type LaunchLifecycle struct {
	PreparedAt   *time.Time `json:"prepared_at,omitempty"`
	CommittedAt  *time.Time `json:"committed_at,omitempty"`
	StartErrorAt *time.Time `json:"start_error_at,omitempty"`
}

const (
	// TerminalizationIntentSchemaVersion is the durable prepare record written
	// before a WorkEnvelope close publishes its first authoritative byte.
	TerminalizationIntentSchemaVersion = "compound.runattempt_terminalization_intent.v0.1"
	TerminalizationOwnerWorkClose      = "work.close"
	TerminalizationOwnerWorkSupersede  = "work.close.supersede"
	TerminalizationOwnerPanelApply     = "panel.apply"
	// TerminalizationOwnerWorkAcknowledgeAbandoned is the work.close
	// caller-owned acknowledgement of an ALREADY-terminal abandoned
	// RunAttempt (close-gate-recovery door, lived wedge work_2742cffbeb5f).
	// The attempt's status is NOT rewritten by this owner; it exists only so
	// the receipt carries a structured acknowledgement under a name the audit
	// can branch on.
	TerminalizationOwnerWorkAcknowledgeAbandoned = "work.close.ack_abandoned"
	// TerminalizationOwnerWorkVerificationRejected acknowledges successful
	// execution rejected by verification. It never rewrites the RunAttempt.
	TerminalizationOwnerWorkVerificationRejected = "work.close.verification_rejected"
)

// TerminalizationIntent is the immutable prepare half of RunAttempt
// terminalization. It binds one attempt to both the exact pre-close work
// generation and the exact close authority/proof generation. A crash after
// terminal work publication can therefore be repaired without inferring from
// PID, elapsed time, an ID prefix, or the latest event.
type TerminalizationIntent struct {
	SchemaVersion              string    `json:"schema_version"`
	Owner                      string    `json:"owner"`
	WorkID                     string    `json:"work_id"`
	RunAttemptActionIntentID   string    `json:"run_attempt_action_intent_id,omitempty"`
	RunAttemptPolicyDecisionID string    `json:"run_attempt_policy_decision_id,omitempty"`
	AuthorityActionIntentID    string    `json:"authority_action_intent_id"`
	AuthorityPolicyDecisionID  string    `json:"authority_policy_decision_id"`
	ExpectedWorkState          string    `json:"expected_work_state"`
	ExpectedWorkSHA256         string    `json:"expected_work_sha256"`
	ExpectedActionIntentID     string    `json:"expected_action_intent_id,omitempty"`
	ExpectedPolicyDecisionID   string    `json:"expected_policy_decision_id,omitempty"`
	CloseInputSHA256           string    `json:"close_input_sha256"`
	CloseActionIntentID        string    `json:"close_action_intent_id"`
	ClosePolicyDecisionID      string    `json:"close_policy_decision_id"`
	Outcome                    string    `json:"outcome"`
	TargetStatus               string    `json:"target_status"`
	TerminalizationReason      string    `json:"terminalization_reason,omitempty"`
	ReceiptID                  string    `json:"receipt_id"`
	TransitionID               string    `json:"transition_id"`
	PreparedAt                 time.Time `json:"prepared_at"`
}

// Validate rejects partial or open-vocabulary terminalization intents. The
// optional run-attempt and expected-generation action/policy ids are absent
// only for legacy records that never carried those bindings.
func (in TerminalizationIntent) Validate() error {
	if in.SchemaVersion != TerminalizationIntentSchemaVersion {
		return fmt.Errorf("terminalization intent schema_version = %q, want %q", in.SchemaVersion, TerminalizationIntentSchemaVersion)
	}
	switch in.Owner {
	case TerminalizationOwnerWorkClose, TerminalizationOwnerWorkSupersede, TerminalizationOwnerPanelApply, TerminalizationOwnerWorkAcknowledgeAbandoned:
	default:
		return fmt.Errorf("terminalization intent owner %q is unsupported", in.Owner)
	}
	if strings.TrimSpace(in.WorkID) == "" {
		return errors.New("terminalization intent work_id is required")
	}
	for name, value := range map[string]string{
		"run_attempt_action_intent_id": in.RunAttemptActionIntentID,
		"authority_action_intent_id":   in.AuthorityActionIntentID,
		"expected_action_intent_id":    in.ExpectedActionIntentID,
		"close_action_intent_id":       in.CloseActionIntentID,
	} {
		if value != "" && !strings.HasPrefix(value, "act_") {
			return fmt.Errorf("terminalization intent %s %q must start act_", name, value)
		}
	}
	for name, value := range map[string]string{
		"run_attempt_policy_decision_id": in.RunAttemptPolicyDecisionID,
		"authority_policy_decision_id":   in.AuthorityPolicyDecisionID,
		"expected_policy_decision_id":    in.ExpectedPolicyDecisionID,
		"close_policy_decision_id":       in.ClosePolicyDecisionID,
	} {
		if value != "" && !strings.HasPrefix(value, "pol_") {
			return fmt.Errorf("terminalization intent %s %q must start pol_", name, value)
		}
	}
	if strings.TrimSpace(in.ExpectedWorkState) == "" {
		return errors.New("terminalization intent expected_work_state is required")
	}
	if in.AuthorityActionIntentID == "" || in.AuthorityPolicyDecisionID == "" {
		return errors.New("terminalization intent exact attempt authority action and policy ids are required")
	}
	if !validSHA256Ref(in.ExpectedWorkSHA256) {
		return fmt.Errorf("terminalization intent expected_work_sha256 %q is invalid", in.ExpectedWorkSHA256)
	}
	if !validSHA256Ref(in.CloseInputSHA256) {
		return fmt.Errorf("terminalization intent close_input_sha256 %q is invalid", in.CloseInputSHA256)
	}
	if in.CloseActionIntentID == "" || in.ClosePolicyDecisionID == "" {
		return errors.New("terminalization intent close action and policy ids are required")
	}
	switch in.Outcome {
	case "completed", "failed", "cancelled":
	default:
		return fmt.Errorf("terminalization intent outcome %q is invalid", in.Outcome)
	}
	if !IsTerminal(in.TargetStatus) {
		return fmt.Errorf("terminalization intent target_status %q is invalid", in.TargetStatus)
	}
	if in.Owner == TerminalizationOwnerWorkClose {
		want := map[string]string{"completed": StatusSucceeded, "failed": StatusFailed, "cancelled": StatusAbandoned}[in.Outcome]
		if in.TargetStatus != want {
			return fmt.Errorf("work.close terminalization target_status %q does not match outcome %q (want %q)", in.TargetStatus, in.Outcome, want)
		}
	}
	if in.Owner == TerminalizationOwnerWorkSupersede && in.TargetStatus != StatusSuperseded {
		return fmt.Errorf("work.close.supersede terminalization target_status %q must be %q", in.TargetStatus, StatusSuperseded)
	}
	if in.Owner == TerminalizationOwnerWorkSupersede {
		if !strings.HasPrefix(in.TerminalizationReason, "superseded by ratt_") {
			return errors.New("work.close.supersede terminalization requires a reason naming the replacing RunAttempt")
		}
	} else if in.Owner != TerminalizationOwnerPanelApply && in.TerminalizationReason != "" {
		return fmt.Errorf("terminalization intent owner %q cannot carry terminalization_reason", in.Owner)
	}
	if !strings.HasPrefix(in.ReceiptID, "rec_") || !strings.HasPrefix(in.TransitionID, "tr_") {
		return errors.New("terminalization intent requires rec_ receipt and tr_ transition ids")
	}
	if in.PreparedAt.IsZero() {
		return errors.New("terminalization intent prepared_at is required")
	}
	return nil
}

func validSHA256Ref(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, r := range value[len("sha256:"):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// RunAttempt is the S1 type. Fields are deliberately minimal: enough to bind a RunAttempt to
// a work_id and to record who/what ran and when. Transcript, diff, test, proof refs are not
// part of S1 -- they land when a real harness adapter actually produces them.
//
// S2 (Slice B) adds the eight references the reviewer's full RunAttempt shape called for:
// context_pack_ref / workspace / command / last_heartbeat_at / transcript_ref / diff_ref /
// test_ref / proof_refs. Every new field is omitempty so pre-S2 records marshal byte-identical
// when re-decoded and re-marshaled (pinned by TestRunAttemptHashNeutralityForPreS2Records).
// The setters refuse on a terminal RunAttempt (succeeded|failed) -- completed attempts cannot
// be retroactively rewritten. Today panel.Apply wires three of these (workspace, command,
// proof_refs); the rest wait for a real harness adapter to populate them.
type RunAttempt struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	WorkID        string `json:"work_id"`
	// ActionIntentID binds this RunAttempt to the ActionIntent that authorized and represents it,
	// closing the attempt→intent→policy-decision audit chain. Governed subprocesses bind it
	// atomically through MintWithActionIntent; claim-time placeholders may attach it after the
	// Advance that creates the ActionIntent. omitempty preserves pre-existing ratt_ records.
	ActionIntentID string `json:"action_intent_id,omitempty"`
	Actor          string `json:"actor"`
	Harness        string `json:"harness"`
	// Origin is the provenance of this attempt's mint: OriginClaimReservation,
	// OriginHarnessExecution, or OriginObservedExternal. Empty on pre-F2
	// records (backward compat; omitempty so they re-marshal byte-identical).
	Origin      string    `json:"origin,omitempty"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`

	// --- S2 fields (every one omitempty; setters refuse on terminal status) ---

	// ContextPackRef is the id of the ContextRequest envelope the harness loaded for this
	// attempt. Set before the first side effect so the audit can trace which context pack
	// produced which decision.
	ContextPackRef string `json:"context_pack_ref,omitempty"`
	// Workspace is the filesystem root the harness ran inside (repo path, scratch dir, etc.).
	Workspace string `json:"workspace,omitempty"`
	// Command is the actual command or function call the harness executed. Free-text by design;
	// the audit reads it as opaque context, not as a re-runnable spec.
	Command string `json:"command,omitempty"`
	// LastHeartbeatAt is the latest in-progress activity timestamp. Set by Heartbeat, which
	// advances only forward (a stale heartbeat is a no-op). Combined with Status it tells an
	// operator whether a still-"running" RunAttempt is genuinely progressing or has stalled.
	// Pointer so `omitempty` actually works -- encoding/json treats `time.Time{}` (zero value)
	// as non-empty because it is a struct. Caught by TestRunAttemptHashNeutralityForPreS2Records
	// when this was a value field.
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	// Progress is cumulative supervised-work progress. Completed buckets only
	// move upward and Remaining only moves downward, so out-of-order workers
	// cannot make an operator dashboard regress.
	Progress *RunProgress `json:"progress,omitempty"`
	// TranscriptRef is the path (relative to COMPOUND_HOME) of the harness's run transcript.
	// For panel.Apply this is the panel run record; for a real adapter it is the model's
	// output stream.
	TranscriptRef string `json:"transcript_ref,omitempty"`
	// DiffRef is the path of the diff artifact (if any) the attempt produced.
	DiffRef string `json:"diff_ref,omitempty"`
	// TestRef is the path of the test-result artifact (if any) the attempt produced.
	TestRef string `json:"test_ref,omitempty"`
	// ProofRefs are the paths or ids of proof artifacts (receipts, transitions, external
	// verifications) that corroborate the attempt's outcome. Empty slice clears (intentional,
	// see TestRunAttemptSetProofRefsClearsOnEmpty).
	ProofRefs []string `json:"proof_refs,omitempty"`

	// TerminalizationIntent is written before work close publication. It is
	// immutable once prepared and remains after completion so recovery can
	// prove the exact authority generation that justified the terminal status.
	TerminalizationIntent       *TerminalizationIntent `json:"terminalization_intent,omitempty"`
	TerminalizationOwner        string                 `json:"terminalization_owner,omitempty"`
	TerminalizationReceiptID    string                 `json:"terminalization_receipt_id,omitempty"`
	TerminalizationTransitionID string                 `json:"terminalization_transition_id,omitempty"`
	TerminalizationReason       string                 `json:"terminalization_reason,omitempty"`

	// SweepReason records WHY a sweep terminalized this attempt (C3). Set only by
	// SweepStale / CompleteWithReason; empty on normally-completed attempts.
	// omitempty so pre-C3 records re-marshal byte-identical.
	SweepReason string `json:"sweep_reason,omitempty"`

	// SweepClassification records the sweep's DISPOSITION of a terminalized
	// stale attempt — the bit the run-attempt sweep needs to gain teeth
	// (DEC-0027 build step 1, work_195771b63a58). Two values, both set only
	// by the sweep path:
	//
	//   "side_effect_free" → the attempt had no transcript, diff, test, or
	//                        observed-external evidence when the sweep
	//                        terminalized it. Auto-closed; no operator review
	//                        needed because the attempt itself produced
	//                        nothing worth preserving.
	//   "ambiguous"        → the attempt MAY have left state behind — a
	//                        transcript_ref, diff_ref, test_ref, or
	//                        observed_external origin. Sweep terminalizes
	//                        the attempt but the operator MUST reconcile
	//                        the side effects (the runattempt package
	//                        cannot prove they are clean). Surfaced via
	//                        the doctor's StuckSweptAmbiguous counter and
	//                        via proof_refs entries prefixed "sweep:needs_me:".
	//
	// Empty on normally-completed attempts and on pre-sweep-teeth records
	// (omitempty so they re-marshal byte-identical).
	SweepClassification string `json:"sweep_classification,omitempty"`

	// PredecessorID links this attempt to the one it retries (C4: retry
	// lineage). When a work is re-claimed after a failed/abandoned attempt,
	// the new attempt's PredecessorID points to the previous one, forming a
	// chain. Empty on the first attempt. omitempty so pre-C4 records
	// re-marshal byte-identical. The work state machine is untouched —
	// this is evidence, not enforcement.
	PredecessorID string `json:"predecessor_id,omitempty"`

	// --- S3 fields (every one omitempty; setters refuse on terminal status) ---

	// LaneID is the adapter lane / actor identity that drove this run (e.g.
	// "codex", "claude-zai"). Stamped by the dispatching context only where it
	// actually knows the lane (observe-run flags today). omitempty so pre-S3
	// records re-marshal byte-identical (pinned by the pre-S3 case of
	// TestRunAttemptHashNeutralityForPreS2Records).
	LaneID string `json:"lane_id,omitempty"`
	// RequestedModelID and RequestedProviderRoute capture the Compound lane
	// tuple selected before launch. They are dispatch intent, not evidence that
	// a concrete provider/model consumed the run.
	RequestedModelID       string `json:"requested_model_id,omitempty"`
	RequestedProviderRoute string `json:"requested_provider_route,omitempty"`
	// ExecutionProfilePlanID and the requested client/transport/effort fields
	// identify the exact semantic execution plan selected before launch. They
	// are intent, not observed route evidence, and therefore never populate
	// ModelID or ProviderRoute.
	ExecutionProfilePlanID   string `json:"execution_profile_plan_id,omitempty"`
	RequestedClientHarnessID string `json:"requested_client_harness_id,omitempty"`
	RequestedTransportID     string `json:"requested_transport_id,omitempty"`
	RequestedNativeEffort    string `json:"requested_native_effort,omitempty"`
	LaunchProfileID          string `json:"launch_profile_id,omitempty"`
	LaunchProfileRevision    string `json:"launch_profile_revision,omitempty"`
	RequestedClientProfileID string `json:"requested_client_profile_id,omitempty"`
	// ModelID is the concrete model the run consumed (e.g. "model-x"). Empty
	// when the dispatching context does not know the model -- never guessed.
	ModelID string `json:"model_id,omitempty"`
	// ProviderRoute is the provider route / endpoint family the run used (e.g.
	// "provider-y"). Empty when unknown. omitempty so pre-S3 records re-marshal
	// byte-identical.
	ProviderRoute string `json:"provider_route,omitempty"`
	// TokensIn is the prompt token usage. Zero ≡ unknown (the dispatching
	// context did not report it). Plain int64 (not pointer) is sufficient:
	// encoding/json omits the zero value for numbers, so omitempty works
	// without the *time.Time workaround LastHeartbeatAt needs.
	TokensIn int64 `json:"tokens_in,omitempty"`
	// TokensOut is the completion token usage. Zero ≡ unknown; see TokensIn.
	// omitempty so pre-S3 records re-marshal byte-identical.
	TokensOut int64 `json:"tokens_out,omitempty"`

	// ExecutionPosture is the governed permission decision bound before the
	// harness subprocess starts. Pointer + omitempty preserves byte identity
	// for pre-posture records.
	ExecutionPosture *ExecutionPosture `json:"execution_posture,omitempty"`
	// Launch materializes the exact prepared|committed|start_error external-start
	// boundary for API consumers. runattemptWriteFile deliberately omits it
	// from the v0.1 main JSON; immutable sidecars are the durable authority.
	Launch *LaunchLifecycle `json:"launch,omitempty"`
	// ExecutionSelection is the immutable per-run selection snapshot (what
	// this launch was asked to run and what the adapter could apply; see
	// execution_selection_snapshot.go). Pointer + omitempty preserves byte
	// identity for records minted before it existed.
	ExecutionSelection *ExecutionSelectionSnapshot `json:"execution_selection,omitempty"`
	// Observed is the append-once observed leg (observed_execution.go): what
	// the launch read back from argv, wrapper env, and hook config. Nil when
	// nothing was observable; it never rewrites ExecutionSelection.
	Observed *ObservedExecution `json:"observed,omitempty"`
}

// RunProgress is the reduced, content-free progress shape shared by
// delegated workers, the CLI, and projections. It deliberately carries no
// prompt, filenames, or model output.
type RunProgress struct {
	Done      int `json:"done"`
	Empty     int `json:"empty"`
	Failed    int `json:"failed"`
	Remaining int `json:"remaining"`
}

// Store persists RunAttempts under <home>/run_attempts/<id>.json with atomic rename and a
// per-process index cache so multiple Get calls in one tick don't re-read disk.
type Store struct {
	home string

	mu    sync.RWMutex
	cache map[string]RunAttempt
}

// Open returns a Store rooted at home. It does NOT create directories -- Get / Complete
// lazily mkdir on first write, matching the pattern used by internal/landlease and
// internal/work so a fresh home stays clean until something is actually persisted.

// Home returns the home path the Store was opened with. Useful for callers
// that need to colocate side artifacts (e.g. a transcript) with the run
// JSON files. Pinned by S3 (harness adapter) consumers.
func (s *Store) Home() string { return s.home }
func Open(home string) *Store {
	// Pin an explicitly selected home alias once. A later alias retarget must
	// not split the per-attempt lock, legacy JSON fence, sidecars, and record
	// writes across two physical homes.
	if canonical, err := canonicalRunAttemptHome(home); err == nil {
		home = canonical
	}
	return &Store{home: home, cache: map[string]RunAttempt{}}
}

// Mint creates and persists a new RunAttempt bound to workID, returning the stored artifact.
// The id format is enforced by Valid -- if Mint ever produces a non-conforming id (regression
// guard), the validator will reject it before it reaches the envelope.
func (s *Store) Mint(workID, actor, harness string, now time.Time) (RunAttempt, error) {
	return s.MintWithActionIntent(workID, actor, harness, "", now)
}

// MintWithActionIntent atomically creates a RunAttempt with its authorizing
// ActionIntent already bound. Governed subprocesses use this form so even an
// infrastructure failure on the first post-mint metadata write retains the
// attempt -> intent -> policy audit chain.
func (s *Store) MintWithActionIntent(workID, actor, harness, actionIntentID string, now time.Time) (RunAttempt, error) {
	if strings.TrimSpace(workID) == "" {
		return RunAttempt{}, fmt.Errorf("runattempt mint: work_id is required")
	}
	if strings.TrimSpace(actor) == "" {
		return RunAttempt{}, fmt.Errorf("runattempt mint: actor is required")
	}
	if strings.TrimSpace(harness) == "" {
		return RunAttempt{}, fmt.Errorf("runattempt mint: harness is required")
	}
	actionIntentID = strings.TrimSpace(actionIntentID)
	if actionIntentID != "" && !strings.HasPrefix(actionIntentID, "act_") {
		return RunAttempt{}, fmt.Errorf("runattempt mint: action_intent_id %q must start with act_", actionIntentID)
	}
	var out RunAttempt
	err := s.withWorkAdmissionLock(workID, func() error {
		id, err := Mint(now)
		if err != nil {
			return err
		}
		ra := RunAttempt{
			SchemaVersion:  SchemaVersion,
			ID:             id,
			WorkID:         workID,
			Actor:          actor,
			Harness:        harness,
			Status:         StatusPending,
			CreatedAt:      now.UTC(),
			ActionIntentID: actionIntentID,
		}
		if err := s.write(ra); err != nil {
			return err
		}
		out = ra
		return nil
	})
	return out, err
}

// MintWithID creates a RunAttempt with a caller-supplied id (must pass
// Valid). If a record with that id already exists on disk, it is returned
// unchanged (idempotent create-or-retrieve). This is the deterministic
// counterpart to Mint: callers that derive an id from a stable seed (e.g.
// MintDeterministic(jobID, now)) get idempotency without a dedup index.
func (s *Store) MintWithID(id, workID, actor, harness string, now time.Time) (RunAttempt, error) {
	if !Valid(id) {
		return RunAttempt{}, fmt.Errorf("runattempt mint: id %q does not match %s pattern", id, Prefix)
	}
	if strings.TrimSpace(workID) == "" {
		return RunAttempt{}, fmt.Errorf("runattempt mint: work_id is required")
	}
	if strings.TrimSpace(actor) == "" {
		return RunAttempt{}, fmt.Errorf("runattempt mint: actor is required")
	}
	if strings.TrimSpace(harness) == "" {
		return RunAttempt{}, fmt.Errorf("runattempt mint: harness is required")
	}
	ra := RunAttempt{
		SchemaVersion: SchemaVersion,
		ID:            id,
		WorkID:        workID,
		Actor:         actor,
		Harness:       harness,
		Status:        StatusPending,
		CreatedAt:     now.UTC(),
	}
	var out RunAttempt
	err := s.withWorkAdmissionLock(workID, func() error {
		return s.withAttemptLock(id, func() error {
			// Create-or-retrieve is one filesystem transaction. An independent
			// Store cannot create and mutate this deterministic attempt between an
			// optimistic miss and a stale whole-record write.
			existing, err := s.getFresh(id)
			if err == nil {
				if existing.WorkID != workID {
					return fmt.Errorf("run attempt %s already belongs to work %s (caller: %s)", id, existing.WorkID, workID)
				}
				if existing.Actor != actor {
					return fmt.Errorf("run attempt %s already belongs to actor %s (caller: %s)", id, existing.Actor, actor)
				}
				if existing.Harness != harness {
					return fmt.Errorf("run attempt %s already belongs to harness %s (caller: %s)", id, existing.Harness, harness)
				}
				out = existing
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
			if err := s.writeLocked(ra); err != nil {
				return err
			}
			out = ra
			return nil
		})
	})
	if err != nil {
		return RunAttempt{}, err
	}
	return cloneExecutionPosture(out), nil
}

// Get returns the RunAttempt with the given id, loading from disk if not yet cached.
func (s *Store) Get(id string) (RunAttempt, error) {
	if !Valid(id) {
		return RunAttempt{}, fmt.Errorf("runattempt get: id %q does not match %s pattern", id, Prefix)
	}
	s.mu.RLock()
	if ra, ok := s.cache[id]; ok {
		ra = cloneExecutionPosture(ra)
		s.mu.RUnlock()
		// Launch authority lives only in validated rooted sidecars. Never let a
		// cached view (including one decoded by an older process generation)
		// stand in for those immutable events.
		ra.Launch = nil
		if launch, err := loadLaunchLifecycle(s.home, id); err != nil {
			return RunAttempt{}, fmt.Errorf("runattempt get %s: launch evidence: %w", id, err)
		} else if launch != nil {
			ra.Launch = launch
		}
		if err := validateDecodedRunAttempt(ra); err != nil {
			return RunAttempt{}, fmt.Errorf("runattempt get %s: %w", id, err)
		}
		return ra, nil
	}
	s.mu.RUnlock()
	path := s.path(id)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RunAttempt{}, ErrNotFound
		}
		return RunAttempt{}, fmt.Errorf("runattempt get %s: %w", id, err)
	}
	var ra RunAttempt
	if err := json.Unmarshal(data, &ra); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt get %s: decode: %w", id, err)
	}
	// The v0.1 JSON is mutable compatibility state, not launch authority. A
	// planted inline field must never be observed or promoted into sidecars.
	ra.Launch = nil
	if launch, err := loadLaunchLifecycle(s.home, id); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt get %s: launch evidence: %w", id, err)
	} else if launch != nil {
		ra.Launch = launch
	}
	if err := validateDecodedRunAttempt(ra); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt get %s: %w", id, err)
	}
	s.mu.Lock()
	// A concurrent same-Store mutation may have published and cached a newer
	// main-record generation while this read validated launch sidecars. Cache
	// the observation only when no newer local generation appeared; launch
	// authority is deliberately reloaded from sidecars on every cache hit.
	if _, exists := s.cache[id]; !exists {
		cached := cloneExecutionPosture(ra)
		cached.Launch = nil
		s.cache[id] = cached
	}
	s.mu.Unlock()
	return ra, nil
}

func (s *Store) getFresh(id string) (RunAttempt, error) {
	ra, err := s.readFresh(id)
	if err != nil {
		return RunAttempt{}, err
	}
	s.mu.Lock()
	s.cache[id] = cloneExecutionPosture(ra)
	s.mu.Unlock()
	return ra, nil
}

// readFresh checks the current main record and rooted launch evidence without
// retaining a cache copy. List uses it for complete censuses; callers that
// need the store cache continue through getFresh.
func (s *Store) readFresh(id string) (RunAttempt, error) {
	if !Valid(id) {
		return RunAttempt{}, fmt.Errorf("runattempt get: id %q does not match %s pattern", id, Prefix)
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RunAttempt{}, ErrNotFound
		}
		return RunAttempt{}, fmt.Errorf("runattempt get %s: %w", id, err)
	}
	var ra RunAttempt
	if err := json.Unmarshal(data, &ra); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt get %s: decode: %w", id, err)
	}
	// See Get: launch state is reconstructed only from rooted sidecars.
	ra.Launch = nil
	if launch, err := loadLaunchLifecycle(s.home, id); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt get %s: launch evidence: %w", id, err)
	} else if launch != nil {
		ra.Launch = launch
	}
	if err := validateDecodedRunAttempt(ra); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt get %s: %w", id, err)
	}
	return ra, nil
}

// validateDecodedRunAttempt is the single compatibility gate shared by cache
// misses and mutation-authority reads. Nested intent validation must never
// differ between Get and getFresh: a cached read accepting a record that a
// mutator rejects (or the reverse) would split the durable lifecycle view.
func validateDecodedRunAttempt(ra RunAttempt) error {
	if strings.TrimSpace(ra.WorkID) == "" || strings.TrimSpace(ra.WorkID) != ra.WorkID || filepath.Base(ra.WorkID) != ra.WorkID || ra.WorkID == "." || ra.WorkID == ".." {
		return fmt.Errorf("invalid work_id %q: a nonempty single path element is required", ra.WorkID)
	}
	if !AcceptsSchemaVersion(ra.SchemaVersion) {
		return fmt.Errorf("unsupported schema_version %q", ra.SchemaVersion)
	}
	if ra.SchemaVersion != SchemaVersion {
		if ra.TerminalizationIntent != nil || ra.TerminalizationOwner != "" ||
			ra.TerminalizationReceiptID != "" || ra.TerminalizationTransitionID != "" ||
			ra.TerminalizationReason != "" {
			return fmt.Errorf("schema_version %q cannot carry v0.2 terminalization fields", ra.SchemaVersion)
		}
		return nil
	}
	if ra.TerminalizationIntent == nil {
		return nil
	}
	intent := ra.TerminalizationIntent
	if err := intent.Validate(); err != nil {
		return fmt.Errorf("terminalization_intent: %w", err)
	}
	if ra.WorkID != intent.WorkID {
		return fmt.Errorf("terminalization_intent work_id %q does not match RunAttempt work_id %q", intent.WorkID, ra.WorkID)
	}
	if ra.ActionIntentID != intent.RunAttemptActionIntentID {
		return fmt.Errorf("terminalization_intent action binding %q does not match RunAttempt %q", intent.RunAttemptActionIntentID, ra.ActionIntentID)
	}
	policyID := ""
	if ra.ExecutionPosture != nil {
		policyID = ra.ExecutionPosture.PolicyDecisionID
	}
	if policyID != intent.RunAttemptPolicyDecisionID {
		return fmt.Errorf("terminalization_intent policy binding %q does not match RunAttempt %q", intent.RunAttemptPolicyDecisionID, policyID)
	}
	return nil
}

// IsTerminal reports whether status is a terminal RunAttempt status
// (succeeded, failed, or abandoned). Terminal attempts cannot be mutated,
// re-completed, or re-entered.
func IsTerminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed ||
		status == StatusAbandoned || status == StatusSuperseded
}

// Complete transitions a RunAttempt to terminal status (succeeded, failed, or
// abandoned) and records the completion timestamp. Pending and running are not
// terminal and Complete refuses them. C1 adds abandoned for claim-release
// (claimed→ready): the attempt was minted at claim but the work was released
// without execution.
func (s *Store) Complete(id string, status string, now time.Time) (RunAttempt, error) {
	if !IsTerminal(status) {
		return RunAttempt{}, fmt.Errorf("runattempt complete %s: status %q is not terminal", id, status)
	}
	return s.mutate(id, func(ra *RunAttempt) (bool, error) {
		if ra.TerminalizationIntent != nil {
			return false, fmt.Errorf("runattempt complete %s: prepared terminalization must use CompletePreparedTerminalization", id)
		}
		if IsTerminal(ra.Status) {
			return false, fmt.Errorf("runattempt complete %s: already terminal (%s)", id, ra.Status)
		}
		ra.Status = status
		ra.CompletedAt = now.UTC()
		return true, nil
	})
}

// PrepareTerminalization durably installs the immutable intent that must be
// present before the owning work close publishes. Repeating the exact intent
// is idempotent; a different intent is a lost-reservation error, never an
// overwrite.
func (s *Store) PrepareTerminalization(id string, intent TerminalizationIntent) (RunAttempt, error) {
	if err := intent.Validate(); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt prepare terminalization %s: %w", id, err)
	}
	return s.mutate(id, func(ra *RunAttempt) (bool, error) {
		if ra.WorkID != intent.WorkID {
			return false, fmt.Errorf("runattempt prepare terminalization %s: work_id %s does not match %s", id, ra.WorkID, intent.WorkID)
		}
		if ra.ActionIntentID != intent.RunAttemptActionIntentID {
			return false, fmt.Errorf("runattempt prepare terminalization %s: action_intent_id %s changed from reserved %s", id, ra.ActionIntentID, intent.RunAttemptActionIntentID)
		}
		actualPolicyID := ""
		if ra.ExecutionPosture != nil {
			actualPolicyID = ra.ExecutionPosture.PolicyDecisionID
		}
		if actualPolicyID != intent.RunAttemptPolicyDecisionID {
			return false, fmt.Errorf("runattempt prepare terminalization %s: execution policy %s changed from reserved %s", id, actualPolicyID, intent.RunAttemptPolicyDecisionID)
		}
		if ra.TerminalizationIntent != nil {
			if reflect.DeepEqual(*ra.TerminalizationIntent, intent) {
				return false, nil
			}
			return false, fmt.Errorf("runattempt prepare terminalization %s: intent is already bound", id)
		}
		if IsTerminal(ra.Status) {
			return false, fmt.Errorf("runattempt prepare terminalization %s: already terminal (%s)", id, ra.Status)
		}
		copyIntent := intent
		ra.SchemaVersion = SchemaVersion
		ra.TerminalizationIntent = &copyIntent
		return true, nil
	})
}

// CompletePreparedTerminalization completes only the exact prepared intent.
// It is idempotent for an already-completed exact intent. Every other terminal
// record remains immutable, including a malformed swept-abandoned record that
// somehow carries an intent no production sweep is allowed to terminalize.
func (s *Store) CompletePreparedTerminalization(id string, expected TerminalizationIntent, now time.Time) (RunAttempt, error) {
	if err := expected.Validate(); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt complete prepared terminalization %s: %w", id, err)
	}
	if !Valid(id) {
		return RunAttempt{}, fmt.Errorf("runattempt mutation %s: id does not match %s pattern", id, Prefix)
	}
	var out RunAttempt
	err := s.withAttemptLock(id, func() error {
		ra, err := s.getFresh(id)
		if err != nil {
			return err
		}
		if ra.TerminalizationIntent == nil || !reflect.DeepEqual(*ra.TerminalizationIntent, expected) {
			return fmt.Errorf("runattempt complete prepared terminalization %s: exact intent is not installed", id)
		}
		if ra.WorkID != expected.WorkID || ra.ActionIntentID != expected.RunAttemptActionIntentID {
			return fmt.Errorf("runattempt complete prepared terminalization %s: work/action binding changed after prepare", id)
		}
		actualPolicyID := ""
		if ra.ExecutionPosture != nil {
			actualPolicyID = ra.ExecutionPosture.PolicyDecisionID
		}
		if actualPolicyID != expected.RunAttemptPolicyDecisionID {
			return fmt.Errorf("runattempt complete prepared terminalization %s: execution policy binding changed after prepare", id)
		}
		if ra.Status == expected.TargetStatus &&
			ra.TerminalizationOwner == expected.Owner &&
			ra.TerminalizationReceiptID == expected.ReceiptID &&
			ra.TerminalizationTransitionID == expected.TransitionID &&
			ra.TerminalizationReason == expected.TerminalizationReason {
			out = ra
			return nil
		}
		if IsTerminal(ra.Status) {
			return fmt.Errorf("runattempt complete prepared terminalization %s: conflicting terminal status %s", id, ra.Status)
		}
		ra.Status = expected.TargetStatus
		ra.CompletedAt = now.UTC()
		ra.TerminalizationOwner = expected.Owner
		ra.TerminalizationReceiptID = expected.ReceiptID
		ra.TerminalizationTransitionID = expected.TransitionID
		ra.TerminalizationReason = expected.TerminalizationReason
		if err := s.writeLocked(ra); err != nil {
			return err
		}
		out = ra
		return nil
	})
	if err != nil {
		return RunAttempt{}, err
	}
	return cloneExecutionPosture(out), nil
}

// CompleteWithOwnerProof terminalizes a RunAttempt while recording the
// component that owns the terminal write and its proof pair. It remains
// available for terminal harness-owned completions that have no prepared
// work-close intent.
func (s *Store) CompleteWithOwnerProof(id, status, owner, receiptID, transitionID string, now time.Time) (RunAttempt, error) {
	if !IsTerminal(status) {
		return RunAttempt{}, fmt.Errorf("runattempt complete %s: status %q is not terminal", id, status)
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return RunAttempt{}, fmt.Errorf("runattempt complete %s: terminalization owner required", id)
	}
	receiptID = strings.TrimSpace(receiptID)
	transitionID = strings.TrimSpace(transitionID)
	if receiptID != "" && !strings.HasPrefix(receiptID, "rec_") {
		return RunAttempt{}, fmt.Errorf("runattempt complete %s: receipt_id %q must start rec_", id, receiptID)
	}
	if transitionID != "" && !strings.HasPrefix(transitionID, "tr_") {
		return RunAttempt{}, fmt.Errorf("runattempt complete %s: transition_id %q must start tr_", id, transitionID)
	}
	return s.mutate(id, func(ra *RunAttempt) (bool, error) {
		if ra.TerminalizationIntent != nil {
			return false, fmt.Errorf("runattempt complete %s: prepared terminalization must use CompletePreparedTerminalization", id)
		}
		if IsTerminal(ra.Status) {
			if ra.Status == status && ra.TerminalizationOwner == owner && ra.TerminalizationReceiptID == receiptID && ra.TerminalizationTransitionID == transitionID {
				return false, nil
			}
			return false, fmt.Errorf("runattempt complete %s: already terminal (%s)", id, ra.Status)
		}
		ra.Status = status
		ra.CompletedAt = now.UTC()
		ra.SchemaVersion = SchemaVersion
		ra.TerminalizationOwner = owner
		ra.TerminalizationReceiptID = receiptID
		ra.TerminalizationTransitionID = transitionID
		return true, nil
	})
}

func (s *Store) CompleteForWorkClose(id, status, receiptID, transitionID string, now time.Time) (RunAttempt, error) {
	if !IsTerminal(status) {
		return RunAttempt{}, fmt.Errorf("runattempt complete-for-work-close %s: status %q is not terminal", id, status)
	}
	receiptID = strings.TrimSpace(receiptID)
	transitionID = strings.TrimSpace(transitionID)
	if !strings.HasPrefix(receiptID, "rec_") || !strings.HasPrefix(transitionID, "tr_") {
		return RunAttempt{}, fmt.Errorf("runattempt complete-for-work-close %s: rec_ receipt and tr_ transition are required", id)
	}
	return s.CompleteWithOwnerProof(id, status, TerminalizationOwnerWorkClose, receiptID, transitionID, now)
}

// CompleteWithReason transitions a RunAttempt to terminal status and records
// WHY. Used by SweepStale (C3) to leave an audit trail: a swept attempt
// carries sweep_reason="stale: no heartbeat for 30m" (or similar) so an
// operator reading the JSON can distinguish a normally-completed attempt from
// one the sweeper collected. The reason is recorded in SweepReason (omitempty,
// hash-neutral). Refuses if already terminal, same as Complete.
func (s *Store) CompleteWithReason(id, status, reason string, now time.Time) (RunAttempt, error) {
	if !IsTerminal(status) {
		return RunAttempt{}, fmt.Errorf("runattempt complete %s: status %q is not terminal", id, status)
	}
	return s.mutate(id, func(ra *RunAttempt) (bool, error) {
		if ra.TerminalizationIntent != nil {
			return false, fmt.Errorf("runattempt complete %s: prepared terminalization must use CompletePreparedTerminalization", id)
		}
		if IsTerminal(ra.Status) {
			return false, fmt.Errorf("runattempt complete %s: already terminal (%s)", id, ra.Status)
		}
		ra.Status = status
		ra.CompletedAt = now.UTC()
		ra.SweepReason = strings.TrimSpace(reason)
		return true, nil
	})
}

// CompleteWithReasonAndClassification is the low-level classified terminalizer
// (DEC-0027 build step 1, work_195771b63a58). It records both the WHY
// (sweep_reason) and the DISPOSITION (side_effect_free vs ambiguous) in one
// atomic write — a partial write that sets the reason but not the
// classification would leave doctor unable to tell a swept-but-cleared
// attempt from a swept-but-needs-review attempt.
//
// The classification is REQUIRED: an empty (or otherwise unrecognized) value
// is rejected — the sweep ALWAYS classifies, so a missing classification is a
// bug in the caller and should fail closed. Callers that don't classify (the
// existing CompleteWithReason path, work envelope supersession, etc.) never
// reach this entrypoint; they leave SweepClassification empty (omitempty) and
// get the same byte-identical record shape as before. SweepStale uses the
// stricter completeStaleSnapshot path so its liveness decision cannot race a
// newer heartbeat or evidence write.
func (s *Store) CompleteWithReasonAndClassification(id, status, reason, classification string, now time.Time) (RunAttempt, error) {
	if !IsTerminal(status) {
		return RunAttempt{}, fmt.Errorf("runattempt complete %s: status %q is not terminal", id, status)
	}
	if classification != SweepClassificationSideEffectFree && classification != SweepClassificationAmbiguous {
		return RunAttempt{}, fmt.Errorf("runattempt complete %s: classification %q is not a valid SweepClassification", id, classification)
	}
	return s.mutate(id, func(ra *RunAttempt) (bool, error) {
		if ra.TerminalizationIntent != nil {
			return false, fmt.Errorf("runattempt complete %s: prepared terminalization must use CompletePreparedTerminalization", id)
		}
		if IsTerminal(ra.Status) {
			return false, fmt.Errorf("runattempt complete %s: already terminal (%s)", id, ra.Status)
		}
		ra.Status = status
		ra.CompletedAt = now.UTC()
		ra.SweepReason = strings.TrimSpace(reason)
		ra.SweepClassification = classification
		return true, nil
	})
}

// ClassifyStaleRunAttempt returns the disposition of a stale non-terminal
// RunAttempt. Side-effect-free attempts can be auto-closed; ambiguous ones
// MUST surface to operator attention because we cannot prove they left no
// state behind (transcript, diff, test artifact, or external observation).
//
// Rules (in order; first match wins):
//
//  1. transcript_ref / diff_ref / test_ref present → ambiguous. The attempt
//     produced an artifact we should not lose.
//  2. origin == observed_external → ambiguous. Someone observed evidence
//     outside our control; we don't know what they saw or whether it was
//     acted on.
//  3. an exact prepared + start_error harness launch with no committed start
//     → side_effect_free. The start callback returned an error before a
//     successful external launch, and the content-free terminal disposition
//     is rooted authority rather than mutable inline JSON.
//  4. any other launch evidence → ambiguous. Prepared-only and committed
//     launches may have crossed an external side-effect boundary.
//  5. anything other than an explicit claim_reservation → ambiguous. This
//     includes harness executions without launch evidence, legacy/empty
//     origins, and unknown future origins.
//  6. an evidence-free claim_reservation is side_effect_free: it reserves
//     work but performs no execution itself.
//
// Exposed at package level (not method) so callers can dry-run classification
// without minting/loading a store — useful for tests and for the sweep CLI's
// --dry-run mode.
func ClassifyStaleRunAttempt(ra RunAttempt) string {
	if ra.TranscriptRef != "" || ra.DiffRef != "" || ra.TestRef != "" {
		return SweepClassificationAmbiguous
	}
	if ra.Origin == OriginObservedExternal {
		return SweepClassificationAmbiguous
	}
	if ra.Launch != nil && exactStartErrorLifecycle(ra) {
		return SweepClassificationSideEffectFree
	}
	if ra.Launch != nil {
		return SweepClassificationAmbiguous
	}
	if ra.Origin != OriginClaimReservation {
		return SweepClassificationAmbiguous
	}
	return SweepClassificationSideEffectFree
}

func exactStartErrorLifecycle(ra RunAttempt) bool {
	launch := ra.Launch
	return ra.Origin == OriginHarnessExecution &&
		launch != nil && launch.PreparedAt != nil && launch.StartErrorAt != nil &&
		launch.CommittedAt == nil && launch.PreparedAt.Equal(*launch.StartErrorAt)
}

// completeStaleSnapshot terminalizes only the exact non-terminal snapshot a
// sweep inspected. Probe execution happens outside the RunAttempt lock, so a
// heartbeat, progress update, or evidence attachment may land after the
// candidate scan. Any such change invalidates the stale/liveness verdict and
// is skipped for a later sweep. Classification and age are recomputed from
// the fresh record while holding the same lock as the terminal write.
func (s *Store) completeStaleSnapshot(expected RunAttempt, threshold time.Duration, now time.Time) (RunAttempt, string, bool, error) {
	if threshold <= 0 {
		threshold = DefaultSweepThreshold
	}
	var classification string
	var swept bool
	committed := false
	updated, acquired, err := s.tryMutate(expected.ID, func(current *RunAttempt) (bool, error) {
		if IsTerminal(current.Status) || !reflect.DeepEqual(*current, expected) {
			return false, nil
		}
		if current.TerminalizationIntent != nil {
			return false, nil
		}
		lastActivity := current.CreatedAt
		if current.LastHeartbeatAt != nil {
			lastActivity = *current.LastHeartbeatAt
		}
		age := now.Sub(lastActivity)
		if age < threshold {
			return false, nil
		}
		classification = classifyStaleRunAttemptForTest(*current)
		if classification != SweepClassificationSideEffectFree && classification != SweepClassificationAmbiguous {
			return false, fmt.Errorf("runattempt complete %s: classification %q is not a valid SweepClassification", current.ID, classification)
		}
		current.Status = StatusAbandoned
		current.CompletedAt = now.UTC()
		current.SweepReason = fmt.Sprintf("stale: %s — no activity for %s (threshold %s)",
			classification, age.Round(time.Second), threshold)
		current.SweepClassification = classification
		committed = true
		return true, nil
	})
	if err == nil && acquired {
		swept = committed
	}
	return updated, classification, swept, err
}

// classifyStaleRunAttemptForTest is the production ClassifyStaleRunAttempt by
// default. Tests override it to simulate classification regressions.
var classifyStaleRunAttemptForTest = ClassifyStaleRunAttempt

// MarkRunning transitions a RunAttempt from pending to running (F5). This is
// the setter observe-run's "running"/"in_progress" job-status branch was
// missing -- without it a running job's RunAttempt silently stayed pending
// forever, contradicting the Status doc comment. Terminal-guarded like every
// other setter (refuses if already succeeded/failed/abandoned); idempotent
// no-op if already running -- repeated "still running" observations of the
// same job must not error or reset CreatedAt/heartbeat state.
//
// The transition is a liveness pivot: the supplied `now` is bound exactly
// to LastHeartbeatAt so a sweeper or downstream consumer that observes the
// running generation can read the transition time without re-deriving it
// from terminal-state sidecars. A fresh-pending attempt gets the binding;
// an already-running attempt (idempotent re-mark) leaves the prior
// heartbeat intact.
func (s *Store) MarkRunning(id string, now time.Time) (RunAttempt, error) {
	// Peek the workID outside the work admission lock so the lock payload
	// can name it; the in-lock mutateMutable re-reads+revalidates, so a
	// concurrent rename (which WorkID itself forbids) would still surface
	// as a refusal.
	workID, err := s.workIDForAttempt(id)
	if err != nil {
		return RunAttempt{}, err
	}
	var out RunAttempt
	err = s.withWorkAdmissionLock(workID, func() error {
		var mErr error
		out, mErr = s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
			if ra.Status == StatusRunning {
				return false, nil
			}
			heartbeat := now.UTC()
			ra.Status = StatusRunning
			ra.LastHeartbeatAt = &heartbeat
			return true, nil
		})
		return mErr
	})
	return out, err
}

// workIDForAttempt returns the immutable WorkID stamped at mint. Read-only;
// gets the cached or disk-recorded attempt and returns its WorkID.
func (s *Store) workIDForAttempt(id string) (string, error) {
	ra, err := s.Get(id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(ra.WorkID) == "" {
		return "", fmt.Errorf("runattempt %s: work_id is empty", id)
	}
	return ra.WorkID, nil
}

// CommitLaunch is the one-shot pending -> running boundary for a real
// subprocess. Content-free launch evidence lives in immutable per-attempt
// sidecars, so a v0.1 writer that does not know the additive Launch field
// cannot erase it while rewriting the main RunAttempt JSON. The work
// admission lock and the per-attempt lock both span the external start
// callback; the legacy global lock is held only for the short main-JSON
// read-modify-write. Cross-work non-convoy during a parked callback is
// outside the v0.1 admission support contract.
func (s *Store) CommitLaunch(id string, now time.Time, start func() error) (RunAttempt, error) {
	if !Valid(id) {
		return RunAttempt{}, fmt.Errorf("runattempt launch %s: id does not match %s pattern", id, Prefix)
	}
	if start == nil {
		return RunAttempt{}, fmt.Errorf("runattempt launch %s: start callback required", id)
	}
	workID, err := s.workIDForAttempt(id)
	if err != nil {
		return RunAttempt{}, err
	}
	var out RunAttempt
	err = s.withWorkAdmissionLock(workID, func() error {
		return s.withPerAttemptLock(id, func() error {
			return s.commitLaunchUnderPerAttemptLock(id, now, start, &out)
		})
	})
	return cloneExecutionPosture(out), err
}

// commitLaunchUnderPerAttemptLock holds the per-attempt lock while CommitLaunch
// drives the pending -> running transition plus the external start callback.
// It is split out of CommitLaunch so the outer admission lock wraps the whole
// sequence without ambiguity around ownership of `out`.
func (s *Store) commitLaunchUnderPerAttemptLock(id string, now time.Time, start func() error, out *RunAttempt) error {
	var prepared RunAttempt
	legacyAcquired, prepareErr := s.withLegacyRunAttemptLockMode(false, func() error {
		ra, err := s.getFresh(id)
		if err != nil {
			return err
		}
		if IsTerminal(ra.Status) {
			*out = ra
			return fmt.Errorf("runattempt launch %s: %w (status=%s)", id, terminalRefusal, ra.Status)
		}
		if ra.Status != StatusPending {
			*out = ra
			return fmt.Errorf("runattempt launch %s: status %s is not pending", id, ra.Status)
		}
		if ra.Launch != nil {
			*out = ra
			return fmt.Errorf("runattempt launch %s: prepared launch evidence already exists", id)
		}
		launchAt := now.UTC()
		if launchAt.IsZero() || launchAt.Before(ra.CreatedAt) {
			launchAt = ra.CreatedAt
		}
		if ra.LastHeartbeatAt != nil && launchAt.Before(*ra.LastHeartbeatAt) {
			launchAt = *ra.LastHeartbeatAt
		}
		preparedAt := launchAt
		if err := runattemptWriteLaunchEvidence(s.home, launchEvidence{
			SchemaVersion: launchEvidenceSchemaVersion,
			AttemptID:     id,
			Kind:          launchEvidencePrepared,
			At:            preparedAt,
		}); err != nil {
			return fmt.Errorf("runattempt launch %s: publish prepared boundary: %w", id, err)
		}
		ra.Launch = &LaunchLifecycle{PreparedAt: timePointer(preparedAt)}
		*out = cloneExecutionPosture(ra)
		ra.Status = StatusRunning
		ra.LastHeartbeatAt = timePointer(launchAt)
		if err := s.writeLocked(ra); err != nil {
			return fmt.Errorf("runattempt launch %s: publish running generation: %w", id, err)
		}
		prepared = cloneExecutionPosture(ra)
		*out = prepared
		return nil
	})
	if prepareErr != nil {
		return prepareErr
	}
	if !legacyAcquired {
		return fmt.Errorf("runattempt launch %s: blocking legacy acquisition returned without lock", id)
	}

	launchAt := *prepared.Launch.PreparedAt
	startErr := start()
	if startErr != nil {
		writeErr := runattemptWriteLaunchEvidence(s.home, launchEvidence{
			SchemaVersion: launchEvidenceSchemaVersion,
			AttemptID:     id,
			Kind:          launchEvidenceStartError,
			At:            launchAt,
		})
		if writeErr != nil {
			*out = prepared
			return errors.Join(
				startErr,
				fmt.Errorf("runattempt launch %s: publish start_error disposition: %w", id, writeErr),
			)
		}
		prepared.Launch.StartErrorAt = timePointer(launchAt)
		*out = prepared
		return startErr
	}

	if err := runattemptWriteLaunchEvidence(s.home, launchEvidence{
		SchemaVersion: launchEvidenceSchemaVersion,
		AttemptID:     id,
		Kind:          launchEvidenceCommitted,
		At:            launchAt,
	}); err != nil {
		*out = prepared
		return fmt.Errorf("runattempt launch %s: publish committed start: %w", id, err)
	}
	prepared.Launch.CommittedAt = timePointer(launchAt)
	*out = prepared
	return nil
}

// AttachActionIntent binds a RunAttempt to the ActionIntent that authorized it, closing the
// attempt→intent→policy-decision audit chain (the RunAttempt otherwise binds to WorkID only).
//
// It is separate from Complete on purpose: binding is retry-idempotent and may run more than once
// (panel.Apply recovers the intent id from the advanced envelope's last ActionIntentIDs element),
// whereas Complete is a one-shot terminal transition. Mint stays before Advance (panel/apply.go),
// preserving the intentional ordering that puts the run id on the envelope before any artifact is
// written; AttachActionIntent is the post-Advance step that adds the binding.
//
// Behavior:
//   - id must be a Valid ratt_ id (else a wrapped error).
//   - actionIntentID must have the act_ prefix (mirrors the ratt_ rule philosophy).
//   - If the RunAttempt is already bound to the SAME actionIntentID, this is a no-op success.
//   - If it is bound to a DIFFERENT actionIntentID, returns ErrAlreadyBound.
//   - If the RunAttempt is already terminal (succeeded/failed), returns an error — binding after
//     terminalization is a bug.
//
// Known limitation: a single Advance carrying multiple RunAttemptIDs would bind all of them to
// the same act_ id. Today only panel.Apply writes RunAttempts and it mints one per Advance, so
// this is a non-issue; revisit when a real multi-attempt harness adapter lands.
func (s *Store) AttachActionIntent(id, actionIntentID string) error {
	if !Valid(id) {
		return fmt.Errorf("runattempt attach %s: id does not match %s pattern", id, Prefix)
	}
	if !strings.HasPrefix(actionIntentID, "act_") {
		return fmt.Errorf("runattempt attach %s: action_intent_id %q must start with act_", id, actionIntentID)
	}
	_, err := s.mutate(id, func(ra *RunAttempt) (bool, error) {
		if ra.TerminalizationIntent != nil {
			return false, fmt.Errorf("runattempt attach %s: prepared terminalization freezes action binding", id)
		}
		if IsTerminal(ra.Status) {
			return false, fmt.Errorf("runattempt attach %s: already terminal (%s); binding after terminalization is a bug", id, ra.Status)
		}
		if ra.ActionIntentID == actionIntentID {
			return false, nil // idempotent retry
		}
		if ra.ActionIntentID != "" {
			return false, fmt.Errorf("runattempt attach %s: %w (have %q, want %q)", id, ErrAlreadyBound, ra.ActionIntentID, actionIntentID)
		}
		ra.ActionIntentID = actionIntentID
		return true, nil
	})
	return err
}

// ---------------------------------------------------------------------------
// S2: RunAttempt full-shape accessors
//
// Every setter follows the same pattern: Get the record (which validates the
// id shape and returns ErrNotFound on miss), refuse if the RunAttempt is
// terminal (StatusSucceeded | StatusFailed), update the field, atomic write.
// A helper updateField carries the refusal logic so each setter stays a
// single line of intent.
// ---------------------------------------------------------------------------

// terminalRefusal is the error returned by every S2 setter when the target
// RunAttempt is already terminal (succeeded, failed, or abandoned). A terminal
// attempt cannot be retroactively rewritten -- that is the audit invariant the
// refusal pins.
var terminalRefusal = errors.New("runattempt is terminal; cannot mutate")

func (s *Store) mutate(id string, fn func(*RunAttempt) (bool, error)) (RunAttempt, error) {
	if !Valid(id) {
		return RunAttempt{}, fmt.Errorf("runattempt mutation %s: id does not match %s pattern", id, Prefix)
	}
	if fn == nil {
		return RunAttempt{}, fmt.Errorf("runattempt mutation %s: callback required", id)
	}
	var out RunAttempt
	err := s.withAttemptLock(id, func() error {
		var err error
		out, err = s.mutateLocked(id, fn)
		return err
	})
	if err != nil {
		return RunAttempt{}, err
	}
	return cloneExecutionPosture(out), nil
}

// tryMutate is the nonblocking sweep seam. A busy launch for one attempt is
// skipped so later stale attempts can still be reconciled in the same pass.
func (s *Store) tryMutate(id string, fn func(*RunAttempt) (bool, error)) (RunAttempt, bool, error) {
	if !Valid(id) {
		return RunAttempt{}, false, fmt.Errorf("runattempt mutation %s: id does not match %s pattern", id, Prefix)
	}
	if fn == nil {
		return RunAttempt{}, false, fmt.Errorf("runattempt mutation %s: callback required", id)
	}
	var out RunAttempt
	acquired, err := s.tryWithAttemptLock(id, func() error {
		var mutateErr error
		out, mutateErr = s.mutateLocked(id, fn)
		return mutateErr
	})
	if err != nil || !acquired {
		return RunAttempt{}, acquired, err
	}
	return cloneExecutionPosture(out), true, nil
}

func (s *Store) mutateLocked(id string, fn func(*RunAttempt) (bool, error)) (RunAttempt, error) {
	// The read is inside the per-attempt process and filesystem locks. Another
	// Store or process may have changed terminality or evidence since any prior
	// observation, so cached state is never mutation authority.
	ra, err := s.getFresh(id)
	if err != nil {
		return RunAttempt{}, err
	}
	changed, err := fn(&ra)
	if err != nil {
		return RunAttempt{}, err
	}
	if changed {
		if err := s.writeLocked(ra); err != nil {
			return RunAttempt{}, err
		}
	}
	return ra, nil
}

func (s *Store) mutateMutable(id string, fn func(*RunAttempt) (bool, error)) (RunAttempt, error) {
	return s.mutate(id, func(ra *RunAttempt) (bool, error) {
		if IsTerminal(ra.Status) {
			return false, fmt.Errorf("runattempt %s: %w (status=%s)", id, terminalRefusal, ra.Status)
		}
		return fn(ra)
	})
}

// SetOrigin stamps the provenance of an attempt's mint (F2 groundwork). Called
// once, immediately after Mint/MintWithID at each of the three producer call
// sites (claim, harness execution, observe-run), before any other state
// change. Refuses on terminal exactly like every other S2/S3 setter, via
// requireMutable, so provenance can never be rewritten after the fact.
func (s *Store) SetOrigin(id, origin string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.Origin = strings.TrimSpace(origin)
		return true, nil
	})
}

// SetContextPack records which ContextRequest envelope this attempt loaded.
// Pinned by TestRunAttemptRoundTripsNewFields/context_pack and refused on
// terminal attempts by TestRunAttemptSettersRefuseOnTerminal.
func (s *Store) SetContextPack(id, ref string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.ContextPackRef = strings.TrimSpace(ref)
		return true, nil
	})
}

// SetWorkspace records the filesystem root the attempt ran inside. Free-text
// by design; the audit reads it as opaque context.
func (s *Store) SetWorkspace(id, workspace string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.Workspace = strings.TrimSpace(workspace)
		return true, nil
	})
}

// SetCommand records the actual command or function call the harness
// executed. Treated as opaque context, not a re-runnable spec.
func (s *Store) SetCommand(id, command string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.Command = strings.TrimSpace(command)
		return true, nil
	})
}

// SetExecutionPosture atomically binds the resolved governed permission mode,
// actual running PolicyDecision, and checked authority revisions before a
// harness subprocess starts. Repeating the exact bind is idempotent; changing
// it is refused because execution authority is immutable evidence.
//
// prompt_only is gated by the registered OMP pair set: each pair binds one
// harness to exactly one authority marker triple. The OMP-native verifier
// pair is (omp-native-verifier, omp_native_verifier,
// harness:omp-native-verifier, v0.1). The OMP-prompt-only pair is
// (omp-prompt-only, omp_prompt_only, harness:omp-prompt-only, v0.1).
// The pre-validation stage rejects any prompt_only evidence item that is
// not one of the registered OMP markers; the inside-callback stage rejects
// any record whose harness is unregistered, whose marker does not match
// the harness pair (cross-pair rejection), whose origin is not
// harness_execution, or whose ActionIntentID is blank. Both stages are
// reached on every admit attempt, so an existing posture (already bound)
// cannot be retroactively relabeled.
func (s *Store) SetExecutionPosture(id string, posture ExecutionPosture) (RunAttempt, error) {
	posture.PermissionMode = strings.TrimSpace(posture.PermissionMode)
	switch posture.PermissionMode {
	case "default", "plan", "acceptEdits", "bypass", "prompt_only":
	default:
		return RunAttempt{}, fmt.Errorf("runattempt %s: execution posture permission_mode %q is invalid", id, posture.PermissionMode)
	}
	posture.PolicyDecisionID = strings.TrimSpace(posture.PolicyDecisionID)
	if !strings.HasPrefix(posture.PolicyDecisionID, "pol_") {
		return RunAttempt{}, fmt.Errorf("runattempt %s: execution posture policy_decision_id %q must start with pol_", id, posture.PolicyDecisionID)
	}

	var evidence []ExecutionPostureEvidence
	if len(posture.AuthorityEvidence) > 0 {
		evidence = make([]ExecutionPostureEvidence, 0, len(posture.AuthorityEvidence))
	}
	seenKinds := make(map[string]bool, len(posture.AuthorityEvidence))
	mutationCapable := posture.PermissionMode == "default" || posture.PermissionMode == "acceptEdits" || posture.PermissionMode == "bypass"
	ompNative := posture.PermissionMode == "prompt_only"
	requiredKinds := map[string]bool{
		"permission_authority":       true,
		"permission_posture":         true,
		"blocking_hook_plan":         true,
		"blocking_hook_installation": true,
	}
	for _, item := range posture.AuthorityEvidence {
		item.Kind = strings.TrimSpace(item.Kind)
		item.Ref = strings.TrimSpace(item.Ref)
		item.Revision = strings.TrimSpace(item.Revision)
		if item.Kind == "" || item.Ref == "" || item.Revision == "" {
			return RunAttempt{}, fmt.Errorf("runattempt %s: execution posture evidence requires kind, ref, and revision", id)
		}
		if seenKinds[item.Kind] {
			return RunAttempt{}, fmt.Errorf("runattempt %s: execution posture evidence kind %q is duplicated", id, item.Kind)
		}
		if mutationCapable && !requiredKinds[item.Kind] {
			return RunAttempt{}, fmt.Errorf("runattempt %s: %s execution posture has unexpected authority evidence kind %q", id, posture.PermissionMode, item.Kind)
		}
		if ompNative {
			if _, ok := ompPromptOnlyPosturePairByMarker(item.Kind, item.Ref, item.Revision); !ok {
				return RunAttempt{}, fmt.Errorf("runattempt %s: prompt_only execution posture authority evidence must be a registered OMP marker (omp_native_verifier@omp-native-verifier@v0.1 or omp_prompt_only@omp-prompt-only@v0.1); got kind=%q ref=%q revision=%q", id, item.Kind, item.Ref, item.Revision)
			}
		}
		seenKinds[item.Kind] = true
		evidence = append(evidence, item)
	}
	if mutationCapable {
		for _, kind := range []string{"permission_authority", "permission_posture", "blocking_hook_plan", "blocking_hook_installation"} {
			if !seenKinds[kind] {
				return RunAttempt{}, fmt.Errorf("runattempt %s: %s execution posture requires %s evidence", id, posture.PermissionMode, kind)
			}
		}
	}
	if ompNative && len(evidence) != 1 {
		return RunAttempt{}, fmt.Errorf("runattempt %s: prompt_only execution posture requires exactly one omp_native_verifier or omp_prompt_only marker", id)
	}
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].Kind != evidence[j].Kind {
			return evidence[i].Kind < evidence[j].Kind
		}
		if evidence[i].Ref != evidence[j].Ref {
			return evidence[i].Ref < evidence[j].Ref
		}
		return evidence[i].Revision < evidence[j].Revision
	})
	posture.AuthorityEvidence = evidence
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		if ra.TerminalizationIntent != nil {
			return false, fmt.Errorf("runattempt %s: prepared terminalization freezes execution posture binding", id)
		}
		if ompNative {
			pair, ok := ompPromptOnlyPosturePairByHarness(ra.Harness)
			if !ok {
				return false, fmt.Errorf("runattempt %s: prompt_only execution posture requires a registered OMP harness \"omp-native-verifier\" or \"omp-prompt-only\" (got %q)", id, ra.Harness)
			}
			if len(evidence) != 1 || evidence[0].Kind != pair.Kind || evidence[0].Ref != pair.Ref || evidence[0].Revision != pair.Revision {
				return false, fmt.Errorf("runattempt %s: prompt_only execution posture harness/marker cross-pair mismatch: harness %q requires marker %q/%q/%q (got %q/%q/%q)", id, pair.Harness, pair.Kind, pair.Ref, pair.Revision, evidence[0].Kind, evidence[0].Ref, evidence[0].Revision)
			}
			if ra.Origin != OriginHarnessExecution {
				return false, fmt.Errorf("runattempt %s: prompt_only execution posture requires origin %q (got %q)", id, OriginHarnessExecution, ra.Origin)
			}
			if strings.TrimSpace(ra.ActionIntentID) == "" {
				return false, fmt.Errorf("runattempt %s: prompt_only execution posture requires a bound action_intent_id", id)
			}
		}
		if ra.ExecutionPosture != nil {
			if reflect.DeepEqual(*ra.ExecutionPosture, posture) {
				return false, nil
			}
			return false, fmt.Errorf("runattempt %s: execution posture is already bound", id)
		}
		if ompNative && ra.Launch != nil {
			return false, fmt.Errorf("runattempt %s: prepared launch freezes prompt_only posture binding", id)
		}
		bound := posture
		bound.AuthorityEvidence = append([]ExecutionPostureEvidence(nil), evidence...)
		ra.ExecutionPosture = &bound
		return true, nil
	})
}

// ompPromptOnlyPosturePair binds one OMP harness to exactly one authority
// marker triple. The two registered pairs are listed in
// ompPromptOnlyPosturePairs and form the prompt_only carve-out's
// extension surface: every gate in SetExecutionPosture's prompt_only
// branch reads from this table.
type ompPromptOnlyPosturePair struct {
	Harness  string
	Kind     string
	Ref      string
	Revision string
}

// ompPromptOnlyPosturePairs is the canonical harness+marker pair set
// admitted by prompt_only. Adding a pair here is the only place that
// extends the carve-out; every other gate (cross-pair match, harness
// registration, origin, ActionIntentID, marker version, extra marker,
// post-launch freeze) reads from this table.
var ompPromptOnlyPosturePairs = []ompPromptOnlyPosturePair{
	{Harness: "omp-native-verifier", Kind: "omp_native_verifier", Ref: "harness:omp-native-verifier", Revision: "v0.1"},
	{Harness: "omp-prompt-only", Kind: "omp_prompt_only", Ref: "harness:omp-prompt-only", Revision: "v0.1"},
}

func ompPromptOnlyPosturePairByHarness(harness string) (ompPromptOnlyPosturePair, bool) {
	for _, p := range ompPromptOnlyPosturePairs {
		if p.Harness == harness {
			return p, true
		}
	}
	return ompPromptOnlyPosturePair{}, false
}

func ompPromptOnlyPosturePairByMarker(kind, ref, revision string) (ompPromptOnlyPosturePair, bool) {
	for _, p := range ompPromptOnlyPosturePairs {
		if p.Kind == kind && p.Ref == ref && p.Revision == revision {
			return p, true
		}
	}
	return ompPromptOnlyPosturePair{}, false
}

func cloneExecutionPosture(ra RunAttempt) RunAttempt {
	if ra.ExecutionPosture != nil {
		posture := *ra.ExecutionPosture
		posture.AuthorityEvidence = append([]ExecutionPostureEvidence(nil), posture.AuthorityEvidence...)
		ra.ExecutionPosture = &posture
	}
	if ra.Launch != nil {
		launch := *ra.Launch
		if launch.PreparedAt != nil {
			prepared := *launch.PreparedAt
			launch.PreparedAt = &prepared
		}
		if launch.CommittedAt != nil {
			committed := *launch.CommittedAt
			launch.CommittedAt = &committed
		}
		if launch.StartErrorAt != nil {
			startError := *launch.StartErrorAt
			launch.StartErrorAt = &startError
		}
		ra.Launch = &launch
	}
	if ra.TerminalizationIntent != nil {
		intent := *ra.TerminalizationIntent
		ra.TerminalizationIntent = &intent
	}
	return ra
}

// Heartbeat advances LastHeartbeatAt to now, but ONLY if now is strictly
// later than the current value. Stale heartbeats are no-ops. The monotonic
// rule is pinned by TestRunAttemptHeartbeatAdvancesOnlyForward. Heartbeat
// also refuses on terminal attempts -- a heartbeat on a "succeeded" run
// would imply the attempt is still running, contradicting the terminal status.
func (s *Store) Heartbeat(id string, now time.Time) (RunAttempt, error) {
	workID, err := s.workIDForAttempt(id)
	if err != nil {
		return RunAttempt{}, err
	}
	var out RunAttempt
	err = s.withWorkAdmissionLock(workID, func() error {
		var mErr error
		out, mErr = s.mutateMutable(id, heartbeatMutation(now))
		return mErr
	})
	return out, err
}

func heartbeatMutation(now time.Time) func(*RunAttempt) (bool, error) {
	return func(ra *RunAttempt) (bool, error) {
		t := now.UTC()
		if ra.LastHeartbeatAt != nil && !t.After(*ra.LastHeartbeatAt) {
			return false, nil
		}
		ra.LastHeartbeatAt = &t
		return true, nil
	}
}

// tryHeartbeat is the sweep's non-blocking renew: when either the work
// admission lock or the per-attempt launch boundary is busy, the candidate is
// skipped this pass (the caller treats !acquired exactly like a busy launch),
// so a sweep never convoys behind a parked launch callback.
func (s *Store) tryHeartbeat(id string, now time.Time) (RunAttempt, bool, error) {
	workID, err := s.workIDForAttempt(id)
	if err != nil {
		return RunAttempt{}, false, err
	}
	var out RunAttempt
	attemptAcquired := false
	admissionAcquired, err := s.tryWithWorkAdmissionLock(workID, func() error {
		var mErr error
		out, attemptAcquired, mErr = s.tryMutate(id, func(ra *RunAttempt) (bool, error) {
			if IsTerminal(ra.Status) {
				return false, fmt.Errorf("runattempt %s: %w (status=%s)", id, terminalRefusal, ra.Status)
			}
			return heartbeatMutation(now)(ra)
		})
		return mErr
	})
	if err != nil || !admissionAcquired || !attemptAcquired {
		return RunAttempt{}, false, err
	}
	return out, true, nil
}

// HeartbeatProgress advances liveness and cumulative progress atomically.
// Completed buckets may only increase and Remaining may only decrease.
func (s *Store) HeartbeatProgress(id string, now time.Time, progress RunProgress) (RunAttempt, error) {
	if progress.Done < 0 || progress.Empty < 0 || progress.Failed < 0 || progress.Remaining < 0 {
		return RunAttempt{}, errors.New("runattempt progress counts must be non-negative")
	}
	workID, err := s.workIDForAttempt(id)
	if err != nil {
		return RunAttempt{}, err
	}
	var out RunAttempt
	err = s.withWorkAdmissionLock(workID, func() error {
		var mErr error
		out, mErr = s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
			if previous := ra.Progress; previous != nil {
				if progress.Done < previous.Done || progress.Empty < previous.Empty || progress.Failed < previous.Failed || progress.Remaining > previous.Remaining {
					return false, fmt.Errorf("runattempt progress must be monotonic: previous=%+v next=%+v", *previous, progress)
				}
			}
			t := now.UTC()
			if ra.LastHeartbeatAt != nil && t.Before(*ra.LastHeartbeatAt) {
				return false, fmt.Errorf("runattempt progress heartbeat is stale: previous=%s next=%s", ra.LastHeartbeatAt.UTC().Format(time.RFC3339Nano), t.Format(time.RFC3339Nano))
			}
			ra.LastHeartbeatAt = &t
			copy := progress
			ra.Progress = &copy
			return true, nil
		})
		return mErr
	})
	return out, err
}

// SetTranscript records the run transcript path. Single string by design --
// a harness has one transcript.
func (s *Store) SetTranscript(id, ref string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.TranscriptRef = strings.TrimSpace(ref)
		return true, nil
	})
}

// SetDiff records the diff artifact path (if any).
func (s *Store) SetDiff(id, ref string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.DiffRef = strings.TrimSpace(ref)
		return true, nil
	})
}

// SetTest records the test-result artifact path (if any).
func (s *Store) SetTest(id, ref string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.TestRef = strings.TrimSpace(ref)
		return true, nil
	})
}

// SetProofRefs replaces the proof-refs slice. Pass nil to clear (pinned by
// TestRunAttemptSetProofRefsClearsOnEmpty). Pass a non-empty slice to
// overwrite. Order is preserved as-given; the audit does not sort.
func (s *Store) SetProofRefs(id string, refs []string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		if len(refs) == 0 {
			ra.ProofRefs = nil
		} else {
			out := make([]string, 0, len(refs))
			for _, r := range refs {
				r = strings.TrimSpace(r)
				if r != "" {
					out = append(out, r)
				}
			}
			ra.ProofRefs = out
		}
		return true, nil
	})
}

// ---------------------------------------------------------------------------
// S3: model / provider / lane / token attribution
//
// SetAttribution and SetTokenUsage follow the exact pattern of every S2
// setter above: Get (which validates the id), refuse on terminal via the
// shared requireMutable helper (same terminalRefusal error wording as
// SetContextPack), update, atomic write. A terminal attempt cannot be
// retroactively re-attributed -- that is the same audit invariant the S2
// setters pin.
// ---------------------------------------------------------------------------

// SetDispatchIntent records the known execution lane plus the model-router
// tuple requested before launch. It deliberately does not populate ModelID or
// ProviderRoute: only observed route evidence may attest those fields.
func (s *Store) SetDispatchIntent(id, laneID, requestedModelID, requestedProviderRoute string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.LaneID = strings.TrimSpace(laneID)
		ra.RequestedModelID = strings.TrimSpace(requestedModelID)
		ra.RequestedProviderRoute = strings.TrimSpace(requestedProviderRoute)
		return true, nil
	})
}

// SetExecutionProfileIntent records the exact requested semantic execution
// identity before the harness is spawned. It deliberately leaves observed
// ModelID and ProviderRoute empty; those fields require route evidence.
func (s *Store) SetExecutionProfileIntent(id, planID, clientHarnessID, transportID, nativeEffort string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.ExecutionProfilePlanID = strings.TrimSpace(planID)
		ra.RequestedClientHarnessID = strings.TrimSpace(clientHarnessID)
		ra.RequestedTransportID = strings.TrimSpace(transportID)
		ra.RequestedNativeEffort = strings.TrimSpace(nativeEffort)
		return true, nil
	})
}

// SetLaunchProfileIntent records the opaque executor-owner launch and account
// policy identity selected before spawn. It carries no account id or
// credential and does not populate observed provider/model fields.
func (s *Store) SetLaunchProfileIntent(id, profileID, revision, clientProfileID string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.LaunchProfileID = strings.TrimSpace(profileID)
		ra.LaunchProfileRevision = strings.TrimSpace(revision)
		ra.RequestedClientProfileID = strings.TrimSpace(clientProfileID)
		return true, nil
	})
}

// SetAttribution records which lane / model / provider route drove this run.
// Empty strings are valid (the dispatching context did not know that
// dimension); the omitempty tags drop them from the JSON, so passing all-empty
// changes nothing on a fresh mint. Stamped by the dispatching context only
// where it actually knows the values -- never guessed.
func (s *Store) SetAttribution(id, laneID, modelID, providerRoute string) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.LaneID = strings.TrimSpace(laneID)
		ra.ModelID = strings.TrimSpace(modelID)
		ra.ProviderRoute = strings.TrimSpace(providerRoute)
		return true, nil
	})
}

// SetTokenUsage records the prompt / completion token counts. Zero ≡ unknown
// (see the TokensIn field comment); pass the counts the dispatching context
// actually observed. Refuses on terminal, same as every S2/S3 setter.
func (s *Store) SetTokenUsage(id string, in, out int64) (RunAttempt, error) {
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		ra.TokensIn = in
		ra.TokensOut = out
		return true, nil
	})
}

// ---------------------------------------------------------------------------
// C4: retry lineage — PredecessorID
//
// When a work is re-claimed after a failed/abandoned attempt, the new
// attempt's PredecessorID points to the previous one, forming a chain.
// The work state machine is untouched — this is evidence, not enforcement.
// NO new edges, NO move_type on Transition (that lives on Receipt).
// ---------------------------------------------------------------------------

// SetPredecessor links this attempt to the one it retries. The predecessor
// must be a different, valid ratt_ id for the same work that exists on disk.
// Setting the same predecessor twice is idempotent (retry-safe). Changing to
// a DIFFERENT predecessor is refused (a retry chain is append-only). Refuses
// on terminal attempts (a swept/completed attempt cannot be retroactively
// linked). Pinned by TestSetPredecessorChains and TestSetPredecessorRefuses.
func (s *Store) SetPredecessor(id, predecessorID string) (RunAttempt, error) {
	predecessorID = strings.TrimSpace(predecessorID)
	if predecessorID == "" {
		return RunAttempt{}, fmt.Errorf("runattempt set-predecessor %s: predecessor_id is required", id)
	}
	if !Valid(predecessorID) {
		return RunAttempt{}, fmt.Errorf("runattempt set-predecessor %s: predecessor %q does not match %s pattern", id, predecessorID, Prefix)
	}
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		if predecessorID == ra.ID {
			return false, fmt.Errorf("runattempt set-predecessor %s: attempt and predecessor must differ", id)
		}
		// The predecessor must exist on disk — no phantom links. The global
		// RunAttempt lock is already held, so this read cannot race a create.
		predecessor, err := s.getFresh(predecessorID)
		if err != nil {
			return false, fmt.Errorf("runattempt set-predecessor %s: predecessor %s: %w", id, predecessorID, err)
		}
		if predecessor.WorkID != ra.WorkID {
			return false, fmt.Errorf("runattempt set-predecessor %s: predecessor %s must belong to the same work", id, predecessorID)
		}
		// Idempotent: same predecessor again is a no-op.
		if ra.PredecessorID == predecessorID {
			return false, nil
		}
		// Append-only: changing to a different predecessor is a bug.
		if ra.PredecessorID != "" {
			return false, fmt.Errorf("runattempt set-predecessor %s: already linked to %s (append-only chain)", id, ra.PredecessorID)
		}
		ra.PredecessorID = predecessorID
		return true, nil
	})
}

// ---------------------------------------------------------------------------
// C3: crash terminality — List, ListNonTerminal, SweepStale
//
// A RunAttempt whose producer is gone (PID dead, no heartbeat, job file
// terminal) must not stay pending/running forever. SweepStale walks the
// store, finds non-terminal attempts whose last activity (heartbeat or
// created_at) is older than the threshold, and terminalizes them to
// abandoned with a recorded sweep_reason. Doctor surfaces the stuck count
// until swept. RunAttempt is evidence, not enforcement: sweeping an attempt
// has NO effect on the work state machine.
// ---------------------------------------------------------------------------

// SweepReport is the outcome of one SweepStale pass. Pinned by C3 tests;
// extended in sweep-teeth (DEC-0027 build step 1) with classification
// counters so a single pass reports BOTH the headline totals AND the
// split between auto-closed and escalated dispositions.
//
// AutoClosed + Escalated sum to Swept when every candidate could be
// classified; a candidate whose snapshot changed or whose terminal write
// failed lands in Skipped instead. The three counters are independently
// counted so a partial sweep is still legible.
type SweepReport struct {
	Scanned    int      `json:"scanned"` // readable non-terminal attempts plus unreadable candidates examined
	Swept      int      `json:"swept"`   // terminalized this pass
	Skipped    int      `json:"skipped"` // fresh enough to keep
	SweptIDs   []string `json:"swept_ids,omitempty"`
	SkippedIDs []string `json:"skipped_ids,omitempty"`
	// Unreadable counts valid RunAttempt records whose main record or launch
	// evidence could not be validated. They are conservatively included in
	// Scanned and Skipped: the sweep cannot prove their lifecycle state, so it
	// must neither omit nor terminalize them.
	Unreadable    int      `json:"unreadable,omitempty"`
	UnreadableIDs []string `json:"unreadable_ids,omitempty"`
	// Protected counts stale candidates whose probe declared live=true
	// and which SweepStaleWithProbe heartbeated without terminalizing.
	// Always 0 for the no-probe SweepStale path; surfaced separately so
	// generic and probe-aware passes share the same on-disk JSON shape
	// for the headline counters.
	Protected int `json:"protected,omitempty"`
	// AutoClosed counts swept attempts classified side_effect_free — the
	// attempt produced no transcript/diff/test and was not observed
	// externally. Auto-closed by the sweep; operator review NOT required.
	AutoClosed    int      `json:"auto_closed,omitempty"`
	AutoClosedIDs []string `json:"auto_closed_ids,omitempty"`
	// Escalated counts swept attempts classified ambiguous — the attempt
	// MAY have left state behind. Sweep terminalized as abandoned, but the
	// operator MUST reconcile. Surfaced via doctor's StuckSweptAmbiguous
	// counter so a non-zero value shows up on the next doctor run.
	Escalated    int      `json:"escalated,omitempty"`
	EscalatedIDs []string `json:"escalated_ids,omitempty"`
}

// DefaultSweepThreshold is the staleness cutoff used when no explicit
// threshold is supplied. A pending/running attempt with no activity for this
// long is considered orphaned. 30 minutes matches the codex job heartbeat
// cadence (the plugin pings every ~5 minutes; six missed pings = dead).
const DefaultSweepThreshold = 30 * time.Minute

// RunAttemptReadIssue identifies one valid RunAttempt filename that could
// not be materialized. The cause is retained for local diagnosis; callers
// that surface operator health can use only ID and keep private paths out of
// public output.
type RunAttemptReadIssue struct {
	ID    string
	Cause error
}

// UnreadableRunAttemptsError reports every valid RunAttempt record that a
// directory walk could not materialize. List and ListNonTerminal still return
// every readable record alongside this error so recovery consumers can make
// progress without making corruption invisible.
type UnreadableRunAttemptsError struct {
	Issues []RunAttemptReadIssue
}

func (e *UnreadableRunAttemptsError) Error() string {
	if e == nil || len(e.Issues) == 0 {
		return "runattempt list: unreadable run attempt"
	}
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		parts = append(parts, issue.ID)
	}
	return fmt.Sprintf("runattempt list: %d unreadable run attempt(s): %s", len(e.Issues), strings.Join(parts, "; "))
}

// UnreadableRunAttemptIDs returns a stable copy of the ids carried by a
// typed list error. A non-list error returns nil.
func UnreadableRunAttemptIDs(err error) []string {
	var unreadable *UnreadableRunAttemptsError
	if !errors.As(err, &unreadable) {
		return nil
	}
	ids := make([]string, 0, len(unreadable.Issues))
	for _, issue := range unreadable.Issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

// List returns every RunAttempt on disk, sorted by CreatedAt ascending.
// A missing run_attempts/ directory is not an error (fresh home). Foreign
// filenames are skipped. A valid ratt_ filename whose record or launch
// evidence cannot be validated is returned as a typed error alongside every
// readable record; recovery and health consumers must never mistake corrupt
// authority evidence for an empty successful walk.
func (s *Store) List() ([]RunAttempt, error) {
	return s.list(nil)
}

// list walks run_attempts/ and freshly loads every valid id, except ids
// for which skip reports true; those are never read past skip itself.
func (s *Store) list(skip func(id string) bool) ([]RunAttempt, error) {
	dir := filepath.Join(s.home, "run_attempts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("runattempt list: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !Valid(id) {
			continue
		}
		if skip != nil && skip(id) {
			continue
		}
		ids = append(ids, id)
	}
	// File and sidecar reads are independent. Keep parallelism small so a
	// census does not overwhelm the home filesystem or unrelated callers.
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]RunAttempt, len(ids))
	readErrors := make([]error, len(ids))
	const maxReaders = 4
	readers := min(maxReaders, len(ids))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(readers)
	for range readers {
		go func() {
			defer workers.Done()
			for i := range jobs {
				out[i], readErrors[i] = s.readFresh(ids[i])
			}
		}()
	}
	for i := range ids {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	// A census must also retire older same-Store snapshots: Get after List
	// must not resurrect a record that List saw changed or unreadable.
	s.mu.Lock()
	for _, id := range ids {
		delete(s.cache, id)
	}
	s.mu.Unlock()
	var issues []RunAttemptReadIssue
	write := 0
	for i, id := range ids {
		if readErrors[i] != nil {
			issues = append(issues, RunAttemptReadIssue{ID: id, Cause: readErrors[i]})
			continue
		}
		if out[i].ID != id {
			issues = append(issues, RunAttemptReadIssue{ID: id, Cause: fmt.Errorf("embedded identity %q contradicts named record %q", out[i].ID, id)})
			continue
		}
		out[write] = out[i]
		write++
	}
	clear(out[write:])
	if write == 0 {
		out = nil
	} else {
		out = out[:write]
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if len(issues) > 0 {
		return out, &UnreadableRunAttemptsError{Issues: issues}
	}
	return out, nil
}

// ListNonTerminal returns only pending/running RunAttempts — the set a
// sweep would examine. Sorted by CreatedAt ascending (List order).
func (s *Store) ListNonTerminal() ([]RunAttempt, error) {
	all, err := s.List()
	return nonTerminal(all), err
}

// ListNonTerminalByStatus returns the same pending/running set as
// ListNonTerminal, but decides terminality from each main record's status
// before paying for launch-sidecar validation. Terminal attempts dominate a
// long-lived home and each Get re-validates up to three rooted sidecars, so a
// full walk costs seconds; per-tool-call hook paths cannot afford that inside
// their wall-clock budget. Live and undecodable records still go through Get,
// so their corruption is reported exactly as ListNonTerminal reports it. A
// terminal record's unreadable launch evidence is NOT reported here — sweep
// and health consumers keep ListNonTerminal, which validates every record.
func (s *Store) ListNonTerminalByStatus() ([]RunAttempt, error) {
	live, err := s.list(s.recordedTerminal)
	// A record terminalized between the status probe and Get arrives here
	// terminal; filter again so the result matches ListNonTerminal.
	return nonTerminal(live), err
}

// recordedTerminal reports whether id's main record decodes with a terminal
// status. A record that cannot be read or decoded reports false so it falls
// through to Get and surfaces as a read issue rather than vanishing.
func (s *Store) recordedTerminal(id string) bool {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return false
	}
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return IsTerminal(probe.Status)
}

func nonTerminal(attempts []RunAttempt) []RunAttempt {
	var out []RunAttempt
	for _, ra := range attempts {
		if !IsTerminal(ra.Status) {
			out = append(out, ra)
		}
	}
	return out
}

// SweepStale terminalizes every non-terminal RunAttempt whose last activity
// is older than threshold. "Last activity" is LastHeartbeatAt when present,
// otherwise CreatedAt (an attempt that never received a heartbeat is judged
// from its birth). Swept attempts transition to abandoned with a
// sweep_reason recording the staleness, AND a SweepClassification recording
// the disposition (side_effect_free vs ambiguous — see
// ClassifyStaleRunAttempt). Terminal attempts are untouched.
//
// threshold <= 0 uses DefaultSweepThreshold.
//
// The sweep is best-effort per attempt: a write failure on one attempt does
// not abort the pass (the next sweep retries it). RunAttempt is evidence,
// not enforcement — sweeping has NO effect on the work state machine.
func (s *Store) SweepStale(threshold time.Duration, now time.Time) (SweepReport, error) {
	if threshold <= 0 {
		threshold = DefaultSweepThreshold
	}
	stuck, listErr := s.ListNonTerminal()
	unreadableIDs := UnreadableRunAttemptIDs(listErr)
	if listErr != nil && len(unreadableIDs) == 0 {
		return SweepReport{}, listErr
	}
	rep := SweepReport{
		Scanned:       len(stuck) + len(unreadableIDs),
		Skipped:       len(unreadableIDs),
		SkippedIDs:    append([]string(nil), unreadableIDs...),
		Unreadable:    len(unreadableIDs),
		UnreadableIDs: append([]string(nil), unreadableIDs...),
	}
	for _, ra := range stuck {
		if ra.TerminalizationIntent != nil {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		lastActivity := ra.CreatedAt
		if ra.LastHeartbeatAt != nil {
			lastActivity = *ra.LastHeartbeatAt
		}
		age := now.Sub(lastActivity)
		if age < threshold {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		_, classification, swept, cerr := s.completeStaleSnapshot(ra, threshold, now)
		if cerr != nil || !swept {
			// Best-effort: a write failure on one attempt does not abort the
			// pass. A changed snapshot is also skipped: the next sweep retries
			// it with a fresh liveness/classification decision.
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		rep.Swept++
		rep.SweptIDs = append(rep.SweptIDs, ra.ID)
		switch classification {
		case SweepClassificationSideEffectFree:
			rep.AutoClosed++
			rep.AutoClosedIDs = append(rep.AutoClosedIDs, ra.ID)
		case SweepClassificationAmbiguous:
			rep.Escalated++
			rep.EscalatedIDs = append(rep.EscalatedIDs, ra.ID)
		}
	}
	return rep, listErr
}

// LivenessProbe abstracts the behind-the-scenes check a SweepStaleWithProbe
// pass performs before terminalizing a candidate. The contract:
//   - nil probe is byte-identical to SweepStale (see nilProbeMatchesSweepStale).
//   - probe(ra) returns:
//   - live=true → heartbeat refresh and protect (no terminalization).
//   - live=false (vanished/dead/unbound) → ordinary stale abandonment.
//     A prior DelegateSweep that already mapped observed exit would have
//     terminalized this attempt; the generic pass does not duplicate that.
//   - err != nil → SKIP the candidate. Do NOT abandon on a transient probe
//     fault -- the next sweep retries.
//
// The probe is consulted ONLY on stale candidates. Probe receives the
// full RunAttempt so callers can do work-owned binding lookup (the
// runattempt store never persists a SessionName field on disk).
type LivenessProbe func(ra RunAttempt) (live bool, err error)

// SweepStaleWithProbe is the probe-aware sweep. With probe == nil it falls
// through to SweepStale unchanged (same on-disk bytes; this is the assertion
// that preserves compatibility for direct store callers). The CLI always
// supplies the work-owned probe. With probe != nil:
//
//   - probe errors skip the candidate (no terminalization, no heartbeat),
//     so a transient probe fault does not strand an attempt.
//   - probe returns live=true → heartbeat refresh + protect (counts as
//     Skipped in the report, with a Protected++ side-count so callers can
//     distinguish kept-due-to-live from kept-due-to-fresh-heartbeat).
//   - probe returns live=false (panes have died or vanished, OR the
//     candidate has no bound pane -- in which case live defaults to
//     false) → ordinary stale abandonment, identical to SweepStale. A
//     DelegateSweep that has already mapped observed exit for this exact
//     attempt would have terminalized it earlier; here we just release the
//     attempt as abandoned, no kill, no heartbeat.
//
// Probe receives the full RunAttempt so callers can apply work-owned
// binding lookup (e.g. work/delegate.lookupSessionFor(ra)) instead of
// having runattempt guess at a SessionName field. The orchestrator owns
// the seam.
func (s *Store) SweepStaleWithProbe(threshold time.Duration, now time.Time, probe LivenessProbe) (SweepReport, error) {
	if probe == nil {
		return s.SweepStale(threshold, now)
	}
	if threshold <= 0 {
		threshold = DefaultSweepThreshold
	}
	stuck, listErr := s.ListNonTerminal()
	unreadableIDs := UnreadableRunAttemptIDs(listErr)
	if listErr != nil && len(unreadableIDs) == 0 {
		return SweepReport{}, listErr
	}
	rep := SweepReport{
		Scanned:       len(stuck) + len(unreadableIDs),
		Skipped:       len(unreadableIDs),
		SkippedIDs:    append([]string(nil), unreadableIDs...),
		Unreadable:    len(unreadableIDs),
		UnreadableIDs: append([]string(nil), unreadableIDs...),
	}
	for _, ra := range stuck {
		if ra.TerminalizationIntent != nil {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		lastActivity := ra.CreatedAt
		if ra.LastHeartbeatAt != nil {
			lastActivity = *ra.LastHeartbeatAt
		}
		age := now.Sub(lastActivity)
		if age < threshold {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		live, perr := probe(ra)
		if perr != nil {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		if live {
			if _, acquired, herr := s.tryHeartbeat(ra.ID, now); herr != nil || !acquired {
				// Heartbeat-refresh write failures are best-effort per
				// attempt (same shape as the probe-error branch above and
				// the CompleteWithReason branch below): skip this candidate
				// and let later stale candidates be processed by the same
				// pass. Aborting the sweep on a single transient failure
				// would strand every subsequent candidate.
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
				continue
			}
			rep.Protected++
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		// Vanished or dead (and not previously swept by DelegateSweep) →
		// ordinary stale abandonment, classified the same way SweepStale
		// would (DEC-0027 build step 1, work_195771b63a58: the probe-aware
		// pass must produce the same disposition per attempt as the generic
		// pass, so doctor reads a single source of truth).
		_, classification, swept, cerr := s.completeStaleSnapshot(ra, threshold, now)
		if cerr != nil || !swept {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		rep.Swept++
		rep.SweptIDs = append(rep.SweptIDs, ra.ID)
		switch classification {
		case SweepClassificationSideEffectFree:
			rep.AutoClosed++
			rep.AutoClosedIDs = append(rep.AutoClosedIDs, ra.ID)
		case SweepClassificationAmbiguous:
			rep.Escalated++
			rep.EscalatedIDs = append(rep.EscalatedIDs, ra.ID)
		}
	}
	return rep, listErr
}

// ObservedLivenessProbe is the work-owned liveness probe that returns the
// time of last observed per-attempt activity. It is the rich sibling of
// LivenessProbe -- the difference matters: LivenessProbe answers
// "is the bound pane alive in some sense (exists, not marked dead)?"
// while ObservedLivenessProbe answers "did I observe per-attempt
// activity at time observed-at?".
//
// Contract:
//   - live=false, observedAt=zero: pane is gone / dead / unbound.
//   - live=true, observedAt=zero: probe saw liveness but did not observe
//     per-attempt activity. Treated as "live but no activity evidence"
//     -- the sweep WILL terminalize (a stale candidate with no
//     observable activity is exactly the byte-identical-heartbeat bug
//     shape and must surface to doctor as stuck_past_ttl, not stay
//     hidden as Protected).
//   - live=true, observedAt=set: probe observed per-attempt activity at
//     observed-at. The sweep stamps observed-at as the heartbeat
//     (forward-only monotonic rule) and protects the attempt. A
//     genuinely live pane producing fresh output therefore keeps a
//     fresh heartbeat; a lingering pane for a dead lane carries a stale
//     observed-at and ages out on the next pass.
//   - err != nil: SKIP the candidate (transient probe fault -- the
//     next sweep retries). Identical to LivenessProbe error semantics.
//
// The probe receives the full RunAttempt so work-owned binding lookup
// stays in work/, not runattempt/.

// SweepStaleAttemptWithProbe applies the probe-aware stale recovery law to
// exactly one RunAttempt. It intentionally resolves the requested record with
// Get rather than listing candidates: an exact recovery must remain available
// when an unrelated record is corrupt, and must never inspect or rewrite it.
func (s *Store) SweepStaleAttemptWithProbe(id string, threshold time.Duration, now time.Time, probe LivenessProbe) (SweepReport, error) {
	if probe == nil {
		return SweepReport{}, errors.New("runattempt targeted sweep: liveness probe is required")
	}
	if threshold <= 0 {
		threshold = DefaultSweepThreshold
	}
	ra, err := s.Get(id)
	if err != nil {
		return SweepReport{}, err
	}
	rep := SweepReport{Scanned: 1}
	if IsTerminal(ra.Status) || ra.TerminalizationIntent != nil {
		rep.Skipped = 1
		rep.SkippedIDs = []string{ra.ID}
		return rep, nil
	}
	lastActivity := ra.CreatedAt
	if ra.LastHeartbeatAt != nil {
		lastActivity = *ra.LastHeartbeatAt
	}
	if now.Sub(lastActivity) < threshold {
		rep.Skipped = 1
		rep.SkippedIDs = []string{ra.ID}
		return rep, nil
	}
	live, probeErr := probe(ra)
	if probeErr != nil {
		rep.Skipped = 1
		rep.SkippedIDs = []string{ra.ID}
		return rep, nil
	}
	if live {
		if _, acquired, heartbeatErr := s.tryHeartbeat(ra.ID, now); heartbeatErr != nil {
			rep.Skipped = 1
			rep.SkippedIDs = []string{ra.ID}
			return rep, heartbeatErr
		} else if !acquired {
			rep.Skipped = 1
			rep.SkippedIDs = []string{ra.ID}
			return rep, nil
		}
		rep.Protected = 1
		rep.Skipped = 1
		rep.SkippedIDs = []string{ra.ID}
		return rep, nil
	}
	_, classification, swept, completeErr := s.completeStaleSnapshot(ra, threshold, now)
	if completeErr != nil {
		rep.Skipped = 1
		rep.SkippedIDs = []string{ra.ID}
		return rep, completeErr
	}
	if !swept {
		rep.Skipped = 1
		rep.SkippedIDs = []string{ra.ID}
		return rep, nil
	}
	rep.Swept = 1
	rep.SweptIDs = []string{ra.ID}
	switch classification {
	case SweepClassificationSideEffectFree:
		rep.AutoClosed = 1
		rep.AutoClosedIDs = []string{ra.ID}
	case SweepClassificationAmbiguous:
		rep.Escalated = 1
		rep.EscalatedIDs = []string{ra.ID}
	}
	return rep, nil
}

// ObservedLivenessProbe is a work-owned per-attempt liveness probe that also
// reports the latest observed per-attempt activity.
type ObservedLivenessProbe func(ra RunAttempt) (live bool, observedAt time.Time, err error)

// SweepStaleWithObservedProbe is the per-attempt-activity-aware sweep.
// It is the fix for the byte-identical-heartbeat defect (work_c772671637c7,
// verified 2026-08-17 20:02Z on main 18645ea7). The CLI probe in
// cmd/compound/runattempt_cli.go returns live=true for any pane that
// exists and is not marked dead, so a lingering tmux pane for a
// long-dead lane keeps satisfying the probe and the old SweepStaleWithProbe
// stamps sweep-time across every probe-live candidate. SweepStaleWithObservedProbe
// replaces sweep-time stamping with probe's observed-at stamping, so:
//
//   - live=true AND observed-at fresh (within threshold) → heartbeat
//     advances to observed-at (forward-only, monotonic), Protected++.
//     This is the safety property: genuinely live delegated panes
//     producing real output stay protected.
//   - live=true AND observed-at stale (past threshold) OR observed-at
//     zero → terminalize as abandoned (NOT protected). The lingering-
//     pane-no-actual-output shape surfaces to doctor as stuck_past_ttl,
//     not as a heartbeat that hides its 4-day-old age.
//   - live=false → ordinary stale abandonment, classified identically
//     to SweepStale. DelegateSweep that already mapped observed exit
//     would have terminalized earlier; this pass does not duplicate.
//   - err != nil → SKIP the candidate. A transient probe fault does
//     not strand an attempt; the next pass retries.
//
// Heartbeat stamping uses observed-at (NOT the sweep's `now`), so two
// candidates the probe observed at distinct times carry distinct
// heartbeat timestamps on disk -- the byte-identical heartbeat symptom
// is broken at the seam.
func (s *Store) SweepStaleWithObservedProbe(threshold time.Duration, now time.Time, probe ObservedLivenessProbe) (SweepReport, error) {
	if probe == nil {
		return SweepReport{}, errors.New("runattempt sweep: SweepStaleWithObservedProbe requires a non-nil probe")
	}
	if threshold <= 0 {
		threshold = DefaultSweepThreshold
	}
	stuck, err := s.ListNonTerminal()
	if err != nil {
		return SweepReport{}, err
	}
	var rep SweepReport
	rep.Scanned = len(stuck)
	for _, ra := range stuck {
		lastActivity := ra.CreatedAt
		if ra.LastHeartbeatAt != nil {
			lastActivity = *ra.LastHeartbeatAt
		}
		age := now.Sub(lastActivity)
		if age < threshold {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		live, observedAt, perr := probe(ra)
		if perr != nil {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		// Probe error path above already returned; here we have a verdict.
		if !live {
			// Vanished or dead → ordinary stale abandonment.
			_, classification, swept, cerr := s.completeStaleSnapshot(ra, threshold, now)
			if cerr != nil || !swept {
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
				continue
			}
			rep.Swept++
			rep.SweptIDs = append(rep.SweptIDs, ra.ID)
			switch classification {
			case SweepClassificationSideEffectFree:
				rep.AutoClosed++
				rep.AutoClosedIDs = append(rep.AutoClosedIDs, ra.ID)
			case SweepClassificationAmbiguous:
				rep.Escalated++
				rep.EscalatedIDs = append(rep.EscalatedIDs, ra.ID)
			}
			continue
		}
		// live=true. The probe vouches for liveness; the question now is
		// whether the probe ALSO observed recent per-attempt activity.
		// observedAt == zero means "live but no activity evidence" -- the
		// lingering-pane bug shape. Treat as dead for sweep purposes:
		// the candidate ages out and surfaces to doctor as stuck_past_ttl.
		if observedAt.IsZero() {
			_, classification, swept, cerr := s.completeStaleSnapshot(ra, threshold, now)
			if cerr != nil || !swept {
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
				continue
			}
			rep.Swept++
			rep.SweptIDs = append(rep.SweptIDs, ra.ID)
			switch classification {
			case SweepClassificationSideEffectFree:
				rep.AutoClosed++
				rep.AutoClosedIDs = append(rep.AutoClosedIDs, ra.ID)
			case SweepClassificationAmbiguous:
				rep.Escalated++
				rep.EscalatedIDs = append(rep.EscalatedIDs, ra.ID)
			}
			continue
		}
		// live=true AND observed-at set. Observed-at must be within
		// threshold of `now` for the safety property to hold (a genuinely
		// live pane is observed at recent times). If observed-at is itself
		// past threshold, the candidate is stale by definition -- the
		// lingering-pane-with-old-output bug shape. Terminalize without
		// stamping the heartbeat, so the on-disk heartbeat reflects the
		// last activity that actually happened (or nil if none). This
		// restores the run_attempts_stuck_past_ttl vs run_attempts_stuck_oldest
		// split the bug erased.
		if now.Sub(observedAt) >= threshold {
			_, classification, swept, cerr := s.completeStaleSnapshot(ra, threshold, now)
			if cerr != nil || !swept {
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
				continue
			}
			rep.Swept++
			rep.SweptIDs = append(rep.SweptIDs, ra.ID)
			switch classification {
			case SweepClassificationSideEffectFree:
				rep.AutoClosed++
				rep.AutoClosedIDs = append(rep.AutoClosedIDs, ra.ID)
			case SweepClassificationAmbiguous:
				rep.Escalated++
				rep.EscalatedIDs = append(rep.EscalatedIDs, ra.ID)
			}
			continue
		}
		// live=true AND observed-at within threshold. Stamp observed-at
		// as the heartbeat (NOT sweep's `now`). The Heartbeat call's
		// forward-only monotonic rule applies: if a more recent heartbeat
		// landed between ListNonTerminal() and this write, the observed-at
		// stamp is a no-op and the candidate stays current.
		if _, herr := s.Heartbeat(ra.ID, observedAt); herr != nil {
			// Heartbeat-refresh write failures are best-effort per
			// attempt: skip this candidate and let later stale
			// candidates be processed by the same pass. Aborting on a
			// single transient failure would strand every subsequent
			// candidate.
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
			continue
		}
		rep.Protected++
		rep.Skipped++
		rep.SkippedIDs = append(rep.SkippedIDs, ra.ID)
	}
	return rep, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.home, "run_attempts", id+".json")
}

type processAttemptLock struct {
	mu   sync.Mutex
	refs int
}

var processAttemptLockRegistry = struct {
	sync.Mutex
	locks map[string]*processAttemptLock
}{locks: map[string]*processAttemptLock{}}

func canonicalAttemptLockKey(home, id string) (string, error) {
	if !Valid(id) {
		return "", fmt.Errorf("runattempt lock: id %q does not match %s pattern", id, Prefix)
	}
	absolute, err := canonicalRunAttemptHome(home)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute) + "\x00" + id, nil
}

func acquireProcessAttemptLock(key string, nonblocking bool) (func(), bool) {
	processAttemptLockRegistry.Lock()
	entry := processAttemptLockRegistry.locks[key]
	if entry == nil {
		entry = &processAttemptLock{}
		processAttemptLockRegistry.locks[key] = entry
	}
	entry.refs++
	processAttemptLockRegistry.Unlock()

	acquired := true
	if nonblocking {
		acquired = entry.mu.TryLock()
	} else {
		entry.mu.Lock()
	}
	if !acquired {
		releaseProcessAttemptLockRef(key, entry)
		return nil, false
	}
	return func() {
		entry.mu.Unlock()
		releaseProcessAttemptLockRef(key, entry)
	}, true
}

func releaseProcessAttemptLockRef(key string, entry *processAttemptLock) {
	processAttemptLockRegistry.Lock()
	defer processAttemptLockRegistry.Unlock()
	entry.refs--
	if entry.refs == 0 && processAttemptLockRegistry.locks[key] == entry {
		delete(processAttemptLockRegistry.locks, key)
	}
}

// write persists ra atomically (write to .tmp, fsync, rename) so a partial write cannot
// leave a half-decoded file in place. Same shape as internal/work.eventLogWrites and
// internal/landlease.writeAtomic so a crash mid-write loses at most one RunAttempt.
// C5: the fsync is real now (previously the comment claimed it but the code did not).
// The store is an evidence organ — durability matters.
func (s *Store) withAttemptLock(id string, fn func() error) error {
	acquired, err := s.withAttemptLockMode(id, false, fn)
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("runattempt lock: blocking acquisition returned without lock")
	}
	return nil
}

func (s *Store) tryWithAttemptLock(id string, fn func() error) (bool, error) {
	return s.withAttemptLockMode(id, true, fn)
}

func (s *Store) withAttemptLockMode(id string, nonblocking bool, fn func() error) (bool, error) {
	if !Valid(id) {
		return false, fmt.Errorf("runattempt mutation: id %q does not match %s pattern", id, Prefix)
	}
	if fn == nil {
		return false, errors.New("runattempt mutation callback required")
	}
	return s.withAttemptMutationLocks(id, nonblocking, fn)
}

func (s *Store) write(ra RunAttempt) error {
	return s.withAttemptLock(ra.ID, func() error {
		if current, err := s.getFresh(ra.ID); err == nil {
			if reflect.DeepEqual(current, ra) {
				return nil
			}
			return fmt.Errorf("runattempt write %s: record already exists", ra.ID)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return s.writeLocked(ra)
	})
}

func (s *Store) writeLocked(ra RunAttempt) error {
	dir := filepath.Join(s.home, "run_attempts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("runattempt write: mkdir %s: %w", dir, err)
	}
	if current, err := s.getFresh(ra.ID); err == nil {
		schemaMigration := current.SchemaVersion == LegacySchemaVersionV01 && ra.SchemaVersion == SchemaVersion &&
			((current.TerminalizationIntent == nil && ra.TerminalizationIntent != nil) ||
				(current.TerminalizationOwner == "" && ra.TerminalizationOwner != ""))
		if (!schemaMigration && current.SchemaVersion != ra.SchemaVersion) || current.ID != ra.ID || current.WorkID != ra.WorkID ||
			current.Actor != ra.Actor || current.Harness != ra.Harness || !current.CreatedAt.Equal(ra.CreatedAt) {
			return fmt.Errorf("runattempt write %s: immutable identity changed", ra.ID)
		}
		if IsTerminal(current.Status) && !reflect.DeepEqual(current, ra) {
			return fmt.Errorf("runattempt %s: %w (status=%s)", ra.ID, terminalRefusal, current.Status)
		}
		if current.ExecutionPosture != nil {
			switch {
			case ra.ExecutionPosture == nil:
				posture := *current.ExecutionPosture
				posture.AuthorityEvidence = append([]ExecutionPostureEvidence(nil), posture.AuthorityEvidence...)
				ra.ExecutionPosture = &posture
			case !reflect.DeepEqual(*current.ExecutionPosture, *ra.ExecutionPosture):
				return fmt.Errorf("runattempt %s: execution posture is already bound", ra.ID)
			}
		}
		if current.TerminalizationIntent != nil {
			switch {
			case ra.TerminalizationIntent == nil:
				intent := *current.TerminalizationIntent
				ra.TerminalizationIntent = &intent
			case !reflect.DeepEqual(*current.TerminalizationIntent, *ra.TerminalizationIntent):
				return fmt.Errorf("runattempt %s: terminalization intent is already bound", ra.ID)
			}
		}
		if current.Status == StatusRunning && ra.Status == StatusPending {
			ra.Status = StatusRunning
		}
		if current.LastHeartbeatAt != nil && (ra.LastHeartbeatAt == nil || current.LastHeartbeatAt.After(*ra.LastHeartbeatAt)) {
			heartbeat := *current.LastHeartbeatAt
			ra.LastHeartbeatAt = &heartbeat
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := validateDecodedRunAttempt(ra); err != nil {
		return fmt.Errorf("runattempt write %s: %w", ra.ID, err)
	}
	if err := runattemptWriteFile(s.path(ra.ID), ra); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[ra.ID] = cloneExecutionPosture(ra)
	s.mu.Unlock()
	return nil
}

// withWorkAdmissionLock wraps fn in the home work admission lock for workID so
// every liveness-increasing RunAttempt write for that work is serialized both
// within the process and across processes. It materializes the authority root
// (<home>/work) through the home package's door on a fresh home before
// acquiring the lock — the lock core deliberately never materializes authority,
// and pinning inside an uncreated directory would deadlock on id-check failure.
//
// One helper, one place to change admission policy. Liveness-increasing paths
// (Mint, MintWithID, MarkRunning, Heartbeat, HeartbeatProgress, CommitLaunch)
// MUST call this helper; reads stay lock-free.
func (s *Store) withWorkAdmissionLock(workID string, fn func() error) error {
	if strings.TrimSpace(workID) == "" {
		return fmt.Errorf("runattempt write: work_id required for admission lock")
	}
	if fn == nil {
		return fmt.Errorf("runattempt write: callback required for admission lock")
	}
	if err := homepkg.EnsureWorkAdmissionAuthorityRoot(s.home); err != nil {
		return err
	}
	return homepkg.WithWorkAdmissionLock(s.home, workID, fn)
}

// tryWithWorkAdmissionLock is the non-blocking admission variant for sweep
// renews: a busy admission lock returns (false, nil) so the sweep can skip the
// candidate this pass instead of convoying behind a parked launch callback.
func (s *Store) tryWithWorkAdmissionLock(workID string, fn func() error) (bool, error) {
	if strings.TrimSpace(workID) == "" {
		return false, fmt.Errorf("runattempt write: work_id required for admission lock")
	}
	if fn == nil {
		return false, fmt.Errorf("runattempt write: callback required for admission lock")
	}
	if err := homepkg.EnsureWorkAdmissionAuthorityRoot(s.home); err != nil {
		return false, err
	}
	return homepkg.TryWithWorkAdmissionLock(s.home, workID, fn)
}

// runattemptWriteFile is the production atomic write implementation for a
// RunAttempt. Tests override it to inject write failures.
var runattemptWriteFile = func(path string, ra RunAttempt) error {
	// Launch evidence is independently custodied beneath run_attempts/launch.
	// Keeping it out of the main JSON makes an old whole-record decoder
	// unable to erase the external-start boundary.
	persisted := cloneExecutionPosture(ra)
	persisted.Launch = nil
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("runattempt write: encode %s: %w", ra.ID, err)
	}
	dir := filepath.Dir(path)
	// Refuse a planted run_attempts symlink before CreateTemp can publish
	// outside the pinned home. The post-rename sync below revalidates the same
	// directory identity and reports publication ambiguity if it changed.
	if err := syncRunAttemptDirectory(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("runattempt write: create temp for %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("runattempt write: write %s: %w", tmp, err)
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return fmt.Errorf("runattempt write: chmod %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("runattempt write: fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("runattempt write: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("runattempt write: rename %s: %w", tmp, err)
	}
	if err := runattemptSyncDirectory(dir); err != nil {
		return fmt.Errorf("runattempt write: sync containing directory %s: %w", dir, err)
	}
	return nil
}
