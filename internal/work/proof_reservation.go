package work

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

const ProofResourceCoordinatedReservationSchemaVersion = "compound.work_proof_reservation.v0.3"

const (
	ProofResourceReservationSchemaVersion           = "compound.work_proof_reservation.v0.2"
	ProofResourceRefreshSchemaVersion               = "compound.work_proof_refresh.v0.1"
	ProofResourceRefreshPreparationSchemaVersion    = "compound.work_proof_refresh_preparation.v0.1"
	ProofResourceResultSchemaVersion                = "compound.work_proof_result.v0.1"
	ProofResourceCompletionSchemaVersion            = "compound.work_proof_completion.v0.1"
	ProofResourceLegacyRecoverySchemaVersion        = "compound.work_proof_legacy_recovery.v0.1"
	proofResourceLegacyRecoveryPendingSchemaVersion = "compound.work_proof_legacy_recovery_pending.v0.1"
	ProofResourceTerminationSchemaVersion           = "compound.work_proof_termination.v0.1"
	ProofResourceTerminationEvidenceSchemaVersion   = "compound.work_proof_termination_evidence.v0.1"
	ProofResourceName                               = "compound-proof-battery"

	ProofResourceAcquired = "acquired"
	ProofResourceRefused  = "refused"
	ProofResourceVerified = "verified"
	ProofResourceFailed   = "failed"
	ProofResourceInvalid  = "invalid"

	ProofResourceProfileCanonicalFull = "canonical_full"

	proofResourceHarness = "proof-check"

	proofResourceActionReserve               = "reserve_full_proof_resource"
	proofResourceActionSteal                 = "steal_full_proof_resource"
	proofResourceActionStealAfterTermination = "steal_full_proof_resource_after_termination_evidence"
)

// ProofResourceReserveInput requests the existing process-wide landlease mutex for a governed
// full proof run. Workspace and Command bind the real proof-owning RunAttempt before any gate
// subprocess can begin. Steal is exceptional and requires an explicit audited reason.
type ProofResourceReserveInput struct {
	Actor                  string
	TTL                    time.Duration
	Workspace              string
	Command                string
	Profile                ProofResourceProfile
	Steal                  bool
	StealReason            string
	TerminatedRunAttemptID string
	TerminationEvidenceRef string
	// busyWhenHeld makes a live lease under another holder a quiet busy result, checked
	// under the admission lock before anything is minted. Only the queued path sets it.
	busyWhenHeld bool
	// waitCheck revalidates a queued caller after admission-lock contention and
	// immediately before creating its RunAttempt. It is never persisted.
	waitCheck        func() error
	coordination     *landlease.ProofCoordinationIdentity
	resource         *landlease.Resource
	admission        *landlease.ProofAdmission
	admissionContext context.Context
}

// ProofResourceResumeInput identifies the canonical command boundary that may finish an existing
// retained completion. It never authorizes a new reservation or proof execution.
type ProofResourceResumeInput struct {
	Actor     string
	Workspace string
	Command   string
	Profile   ProofResourceProfile
	// PrepareOwnedReport is the projection-publication barrier for a retained report. Resume calls
	// it only after exact pending-input validation and before completion may release exclusivity.
	PrepareOwnedReport func(ProofResourceResult) error
	// ReservationID pins the exact retained reservation this resume may finish. Empty keeps the
	// ordinary behavior, which resolves the reservation from the live proof holder.
	ReservationID string
	// ExpectedPendingSHA256 fences the retained completion journal by its exact bytes. It is
	// checked at admission and again inside the native mutation guards, before any effect.
	ExpectedPendingSHA256 string
	// RequireFailedResult restricts this resume to an owned, exact failed result. A successful,
	// unknown, inconsistent or unowned result is refused.
	RequireFailedResult bool
}

// ProofResourceGateProfile is the immutable identity of one canonical full-proof gate.
// Runtime-only fields such as status, duration, exit code, and output are intentionally absent.
type ProofResourceGateProfile struct {
	Name          string   `json:"name"`
	Command       []string `json:"command"`
	Dir           string   `json:"dir,omitempty"`
	SkipIfMissing string   `json:"skip_if_missing,omitempty"`
	ProofRef      string   `json:"proof_ref"`
}

// ProofResourceProfile binds admission to a typed proof inventory. Only canonical_full may later
// become governed verification. Digest covers Kind, Repo, BaseRef, and the ordered gate identities.
type ProofResourceProfile struct {
	Kind           string                     `json:"kind"`
	Repo           string                     `json:"repo"`
	BaseRef        string                     `json:"base_ref,omitempty"`
	SourceSnapshot *gitrepository.Snapshot    `json:"source_snapshot,omitempty"`
	Gates          []ProofResourceGateProfile `json:"gates"`
	Digest         string                     `json:"digest"`
	RemoteGoTest   *RemoteProofDescriptor     `json:"remote_go_test,omitempty"`
}

// NewProofResourceProfile validates and seals one typed proof inventory.
func NewProofResourceProfile(kind, repo, baseRef string, gates []ProofResourceGateProfile) (ProofResourceProfile, error) {
	return newProofResourceProfile(kind, repo, baseRef, nil, gates)
}

// NewProofResourceProfileWithSource validates and seals one certifying proof
// inventory against its immutable repository source identity.
func NewProofResourceProfileWithSource(
	kind, repo string,
	source gitrepository.Snapshot,
	gates []ProofResourceGateProfile,
) (ProofResourceProfile, error) {
	return newProofResourceProfile(kind, repo, source.BaseSHA, &source, gates)
}

func newProofResourceProfile(
	kind, repo, baseRef string,
	source *gitrepository.Snapshot,
	gates []ProofResourceGateProfile,
	remote ...*RemoteProofDescriptor,
) (ProofResourceProfile, error) {
	profile := ProofResourceProfile{
		Kind:           strings.TrimSpace(kind),
		Repo:           strings.TrimSpace(repo),
		BaseRef:        strings.TrimSpace(baseRef),
		SourceSnapshot: source,
		Gates:          append([]ProofResourceGateProfile(nil), gates...),
	}
	if len(remote) > 0 {
		profile.RemoteGoTest = remote[0]
	}
	for i := range profile.Gates {
		profile.Gates[i].Name = strings.TrimSpace(profile.Gates[i].Name)
		profile.Gates[i].Command = append([]string(nil), profile.Gates[i].Command...)
		profile.Gates[i].Dir = strings.TrimSpace(profile.Gates[i].Dir)
		profile.Gates[i].SkipIfMissing = strings.TrimSpace(profile.Gates[i].SkipIfMissing)
		profile.Gates[i].ProofRef = strings.TrimSpace(profile.Gates[i].ProofRef)
	}
	if err := validateProofResourceProfileShape(profile); err != nil {
		return ProofResourceProfile{}, err
	}
	digest, err := proofResourceProfileDigest(profile)
	if err != nil {
		return ProofResourceProfile{}, err
	}
	profile.Digest = digest
	return profile, nil
}

func normalizeProofResourceProfile(profile ProofResourceProfile) (ProofResourceProfile, error) {
	return newProofResourceProfile(
		profile.Kind,
		profile.Repo,
		profile.BaseRef,
		profile.SourceSnapshot,
		profile.Gates,
		profile.RemoteGoTest,
	)
}

// ProofResourceReservation is work-owned evidence around the existing landlease mutex. It creates
// no second scheduler, lock file, or authority: Holder is the exact landlease holder, RunAttemptID
// is the concrete proof execution, and work_id remains the multiplayer root.
type ProofResourceReservation struct {
	SchemaVersion             string                               `json:"schema_version"`
	ID                        string                               `json:"id"`
	Kind                      string                               `json:"kind"`
	Status                    string                               `json:"status"`
	CreatedAt                 string                               `json:"created_at"`
	WorkID                    string                               `json:"work_id"`
	Actor                     string                               `json:"actor"`
	Resource                  string                               `json:"resource"`
	Holder                    string                               `json:"holder"`
	RunAttemptID              string                               `json:"run_attempt_id,omitempty"`
	Workspace                 string                               `json:"workspace"`
	Command                   string                               `json:"command"`
	Profile                   ProofResourceProfile                 `json:"proof_profile"`
	ProofCoordination         *landlease.ProofCoordinationIdentity `json:"proof_coordination,omitempty"`
	AcquiredAt                string                               `json:"acquired_at,omitempty"`
	ExpiresAt                 string                               `json:"expires_at,omitempty"`
	TTLSeconds                int64                                `json:"ttl_seconds,omitempty"`
	ConflictingHolder         string                               `json:"conflicting_holder,omitempty"`
	RecoveredHolder           string                               `json:"recovered_holder,omitempty"`
	RecoveredExpiresAt        string                               `json:"recovered_expires_at,omitempty"`
	RecoveredRunAttemptID     string                               `json:"recovered_run_attempt_id,omitempty"`
	PriorHolder               string                               `json:"prior_holder,omitempty"`
	PriorRunAttemptID         string                               `json:"prior_run_attempt_id,omitempty"`
	StealReason               string                               `json:"steal_reason,omitempty"`
	StolenAt                  string                               `json:"stolen_at,omitempty"`
	TerminatedRunAttemptID    string                               `json:"terminated_run_attempt_id,omitempty"`
	TerminationEvidenceRef    string                               `json:"termination_evidence_ref,omitempty"`
	TerminationConfirmedAt    string                               `json:"termination_confirmed_at,omitempty"`
	TerminationConfirmationID string                               `json:"termination_confirmation_id,omitempty"`
	FailureReason             string                               `json:"failure_reason,omitempty"`
	ActionIntentID            string                               `json:"action_intent_id"`
	PolicyDecisionID          string                               `json:"policy_decision_id"`
}

type ProofResourceReserveResult struct {
	ExecutionBarrier *landlease.ExecutionBarrier `json:"-"`
	SchemaVersion    string                      `json:"schema_version"`
	Reservation      ProofResourceReservation    `json:"reservation"`
	Work             envelope.WorkEnvelope       `json:"work"`
}

// ProofResourceRefresh is append-only evidence that the original governed reservation remained
// continuously live and its exact RunAttempt heartbeat extended the exclusive window.
type ProofResourceRefresh struct {
	SchemaVersion    string `json:"schema_version"`
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	CreatedAt        string `json:"created_at"`
	WorkID           string `json:"work_id"`
	Actor            string `json:"actor"`
	ReservationID    string `json:"reservation_id"`
	RunAttemptID     string `json:"run_attempt_id"`
	Holder           string `json:"holder"`
	WindowOpenedAt   string `json:"window_opened_at"`
	RefreshedAt      string `json:"refreshed_at"`
	ExpiresAt        string `json:"expires_at"`
	TTLSeconds       int64  `json:"ttl_seconds"`
	PreparationID    string `json:"preparation_id,omitempty"`
	ActionIntentID   string `json:"action_intent_id"`
	PolicyDecisionID string `json:"policy_decision_id"`
}

// ProofResourceRefreshPreparation is the per-heartbeat pre-image that proves an
// allowing decision existed before the scarce lease mutation. Its reservation-
// scoped id family makes every preparation reachable without appending another
// row to the global Work lifecycle. A preparation without a matching refresh is
// a fail-closed interrupted attempt, never evidence of an extended window.
type ProofResourceRefreshPreparation struct {
	SchemaVersion    string `json:"schema_version"`
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	CreatedAt        string `json:"created_at"`
	WorkID           string `json:"work_id"`
	Actor            string `json:"actor"`
	ReservationID    string `json:"reservation_id"`
	RunAttemptID     string `json:"run_attempt_id"`
	Holder           string `json:"holder"`
	WindowOpenedAt   string `json:"window_opened_at"`
	RequestedAt      string `json:"requested_at"`
	TTLSeconds       int64  `json:"ttl_seconds"`
	ActionIntentID   string `json:"action_intent_id"`
	PolicyDecisionID string `json:"policy_decision_id"`
}

// ProofResourceTerminationConfirmation is the governed, immutable bridge between external
// process-supervisor evidence and a predecessor RunAttempt terminal state. It carries no PID:
// identity is the non-reusable ratt_* plus a content-addressed evidence ref.
type ProofResourceTerminationConfirmation struct {
	SchemaVersion          string `json:"schema_version"`
	ID                     string `json:"id"`
	Kind                   string `json:"kind"`
	CreatedAt              string `json:"created_at"`
	WorkID                 string `json:"work_id"`
	Actor                  string `json:"actor"`
	ReservationID          string `json:"reservation_id"`
	PriorHolder            string `json:"prior_holder"`
	TerminatedRunAttemptID string `json:"terminated_run_attempt_id"`
	TerminationEvidenceRef string `json:"termination_evidence_ref"`
	ActionIntentID         string `json:"action_intent_id"`
	PolicyDecisionID       string `json:"policy_decision_id"`
}

// ProofResourceTerminationEvidence is the immutable supervisor receipt required to recover a
// predecessor that could not terminalize itself. RunAttempt identity is stable and non-reusable;
// process IDs are deliberately absent.
type ProofResourceTerminationEvidence struct {
	Producer      string `json:"producer"`
	BarrierID     string `json:"barrier_id"`
	Holder        string `json:"holder"`
	SchemaVersion string `json:"schema_version"`
	RunAttemptID  string `json:"run_attempt_id"`
	Status        string `json:"status"`
	ObservedAt    string `json:"observed_at"`
	Observer      string `json:"observer"`
}

// ProofResourceHeldError is returned only after the refused attempt has been terminalized and
// attached to its work lifecycle. The caller can report ReservationID without inventing a second
// coordination message, and its runner callback must remain unentered.
type ProofResourceHeldError struct {
	WorkID        string
	ReservationID string
	Holder        string
	ExpiresAt     string
	Remaining     int64
}

func (e *ProofResourceHeldError) Error() string {
	return fmt.Sprintf(
		"proof resource %s held by %q until %s (%ds remaining); refusal recorded as %s on %s; release via --terminated-run RATT_ID --termination-evidence ARTIFACT_REF after predecessor RunAttempt %s supplies termination evidence",
		ProofResourceName,
		e.Holder,
		e.ExpiresAt,
		e.Remaining,
		e.ReservationID,
		e.WorkID,
		e.Holder,
	)
}

// ProofResourceCompleteInput records an internally-observed proof producer outcome. The immutable
// report artifact is parsed by CompleteProofResource; caller-supplied status/owned booleans are not
// trusted. ProducerExitKnown prevents piped or otherwise exit-ambiguous output from becoming proof.
// FailureReason is the mutually-exclusive infrastructure-failure path when no report was produced.
type ProofResourceCompleteInput struct {
	Actor             string
	ReservationID     string
	ReportArtifactRef string
	ProducerExitKnown bool
	ProducerExitCode  int
	FailureReason     string
}

type ProofResourceResult struct {
	SchemaVersion     string                  `json:"schema_version"`
	ID                string                  `json:"id"`
	Kind              string                  `json:"kind"`
	Status            string                  `json:"status"`
	CreatedAt         string                  `json:"created_at"`
	WorkID            string                  `json:"work_id"`
	Actor             string                  `json:"actor"`
	Resource          string                  `json:"resource"`
	ReservationID     string                  `json:"reservation_id"`
	RunAttemptID      string                  `json:"run_attempt_id"`
	RunAttemptStatus  string                  `json:"run_attempt_status"`
	ReportArtifactRef string                  `json:"report_artifact_ref,omitempty"`
	ReportStatus      string                  `json:"report_status,omitempty"`
	OwnedReport       bool                    `json:"owned_report"`
	SourceSnapshot    *gitrepository.Snapshot `json:"source_snapshot,omitempty"`
	ProducerExitKnown bool                    `json:"producer_exit_known"`
	ProducerExitCode  int                     `json:"producer_exit_code,omitempty"`
	FailureReason     string                  `json:"failure_reason,omitempty"`
	VerificationRef   string                  `json:"verification_ref,omitempty"`
	CompletionID      string                  `json:"completion_id"`
	ActionIntentID    string                  `json:"action_intent_id"`
	PolicyDecisionID  string                  `json:"policy_decision_id"`
}

// ProofResourceCompletion is the authoritative finalizer marker. A prepared result record is
// never valid proof on its own: the terminal RunAttempt, this marker, and the Work verification
// link must all converge before the scarce holder is released.
type ProofResourceCompletion struct {
	SchemaVersion    string `json:"schema_version"`
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	CreatedAt        string `json:"created_at"`
	WorkID           string `json:"work_id"`
	Actor            string `json:"actor"`
	ReservationID    string `json:"reservation_id"`
	ResultID         string `json:"result_id"`
	RunAttemptID     string `json:"run_attempt_id"`
	RunAttemptStatus string `json:"run_attempt_status"`
	VerificationRef  string `json:"verification_ref,omitempty"`
	ActionIntentID   string `json:"action_intent_id"`
	PolicyDecisionID string `json:"policy_decision_id"`
}

type ProofResourceCompleteResult struct {
	SchemaVersion           string                `json:"schema_version"`
	Result                  ProofResourceResult   `json:"result"`
	VerificationRef         string                `json:"verification_ref,omitempty"`
	ResourceReleased        bool                  `json:"resource_released"`
	RecoveryJournalRetained bool                  `json:"recovery_journal_retained,omitempty"`
	ReleaseWarning          string                `json:"release_warning,omitempty"`
	Work                    envelope.WorkEnvelope `json:"work"`
}

// ProofResourceLegacyRecoveryInput identifies the exact retained pre-v0.3
// completion invocation. Recovery is release-only: it never executes gates or
// converts the retained report into verification.
type ProofResourceLegacyRecoveryInput struct {
	Actor     string
	Workspace string
	Command   string
}

// ProofResourceLegacyRecovery is durable evidence that an otherwise valid
// pre-v0.3 completion journal was retired without certifying its report.
type ProofResourceLegacyRecovery struct {
	SchemaVersion        string `json:"schema_version"`
	ID                   string `json:"id"`
	Kind                 string `json:"kind"`
	Status               string `json:"status"`
	CreatedAt            string `json:"created_at"`
	WorkID               string `json:"work_id"`
	Actor                string `json:"actor"`
	Resource             string `json:"resource"`
	ReservationID        string `json:"reservation_id"`
	RunAttemptID         string `json:"run_attempt_id"`
	ReportArtifactRef    string `json:"report_artifact_ref"`
	DiscardedProofRef    string `json:"discarded_proof_ref,omitempty"`
	FreshBatteryRequired bool   `json:"fresh_battery_required"`
	ActionIntentID       string `json:"action_intent_id"`
	PolicyDecisionID     string `json:"policy_decision_id"`
}

type ProofResourceLegacyRecoveryResult struct {
	SchemaVersion        string                      `json:"schema_version"`
	Recovery             ProofResourceLegacyRecovery `json:"recovery"`
	ResourceReleased     bool                        `json:"resource_released"`
	FreshBatteryRequired bool                        `json:"fresh_battery_required"`
	VerificationRef      string                      `json:"verification_ref,omitempty"`
	Work                 envelope.WorkEnvelope       `json:"work"`
}

type proofResourceLegacyRecoveryPending struct {
	SchemaVersion      string                         `json:"schema_version"`
	ID                 string                         `json:"id"`
	WorkID             string                         `json:"work_id"`
	ReservationID      string                         `json:"reservation_id"`
	Holder             string                         `json:"holder"`
	RunAttemptID       string                         `json:"run_attempt_id"`
	InputDigest        string                         `json:"input_digest"`
	CompletionDigest   string                         `json:"completion_digest"`
	CreatedAt          string                         `json:"created_at"`
	GovernanceSeal     string                         `json:"governance_seal"`
	Action             envelope.ActionIntent          `json:"action"`
	Decision           envelope.PolicyDecision        `json:"decision"`
	Completion         proofResourceCompletionPending `json:"completion"`
	Recovery           ProofResourceLegacyRecovery    `json:"recovery"`
	WorkBefore         envelope.WorkEnvelope          `json:"work_before"`
	WorkAfter          envelope.WorkEnvelope          `json:"work_after"`
	LifecycleEvent     Event                          `json:"lifecycle_event"`
	FreshBatteryReason string                         `json:"fresh_battery_reason"`
	TerminalStatus     string                         `json:"terminal_status"`
	TerminalReason     string                         `json:"terminal_reason"`
}

// ProofResourceReleasePendingError means the authoritative proof commit is visible, but the
// scarce holder and exact recovery journal are intentionally retained. Callers must not present
// this state as an ordinary green proof; retrying the exact completion releases idempotently.
type ProofResourceReleasePendingError struct {
	ReservationID string
	Holder        string
	Cause         error
}

func (e *ProofResourceReleasePendingError) Error() string {
	return fmt.Sprintf(
		"proof resource %s committed but release remains pending for holder %q: %v",
		e.ReservationID,
		e.Holder,
		e.Cause,
	)
}

func (e *ProofResourceReleasePendingError) Unwrap() error {
	return e.Cause
}

const proofResourceCompletionPendingSchemaVersion = "compound.work_proof_completion_pending.v0.1"

type proofResourceCompletionPending struct {
	SchemaVersion   string                  `json:"schema_version"`
	WorkID          string                  `json:"work_id"`
	ReservationID   string                  `json:"reservation_id"`
	InputDigest     string                  `json:"input_digest"`
	GovernanceSeal  string                  `json:"governance_seal,omitempty"`
	CreatedAt       string                  `json:"created_at"`
	Action          envelope.ActionIntent   `json:"action"`
	Decision        envelope.PolicyDecision `json:"decision"`
	Result          ProofResourceResult     `json:"result"`
	Completion      ProofResourceCompletion `json:"completion"`
	VerificationRef string                  `json:"verification_ref,omitempty"`
	TerminalStatus  string                  `json:"terminal_status"`
	TerminalReason  string                  `json:"terminal_reason"`
	ProofRefs       []string                `json:"proof_refs"`
	LifecycleEvent  Event                   `json:"lifecycle_event"`
}

type proofResourceCompletionExpectation struct {
	InputDigest     string
	ResultStatus    string
	ReportStatus    string
	OwnedReport     bool
	SourceSnapshot  *gitrepository.Snapshot
	VerificationRef string
	TerminalStatus  string
	TerminalReason  string
}

var proofResourceWriteEventRecord = func(s *Store, stage, id string, value any) error {
	return s.writeEventJSON(id, value)
}

var proofResourceWriteWorkRecord = func(s *Store, stage string, w *envelope.WorkEnvelope) error {
	return s.writeJSON(s.workPath(w.ID), w)
}

var proofResourceAppendLifecycleRecord = func(s *Store, event Event) error {
	return s.appendEventControlled(event, nil, nil)
}

var proofResourceSetRunAttemptProofRefs = func(s *Store, id string, refs []string) error {
	_, err := s.runAttempts().SetProofRefs(id, refs)
	return err
}

var proofResourceTerminalizeRunAttempt = func(s *Store, id, status, reason string, now time.Time) (runattempt.RunAttempt, error) {
	return s.runAttempts().CompleteWithReason(id, status, reason, now)
}

var proofResourceAcquireWithExecutionBarrierPrepared = func(home, holder string, ttl time.Duration, now time.Time, prepare func(landlease.AcquireOutcome) error) (landlease.AcquireOutcome, *landlease.ExecutionBarrier, error) {
	return landlease.ProofResource(home).AcquireWithExecutionBarrierPrepared(holder, ttl, now, prepare)
}
var proofResourceAfterScarceMutation = func(phase string) {}

// proofResourceBeforeRetainedMutation is a deterministic test boundary immediately before the
// expected-input fence inside the native mutation guard.
var proofResourceBeforeRetainedMutation = func(reservationID string) {}
var proofResourceWriteLegacyRecoveryJournal = func(
	s *Store,
	path string,
	pending proofResourceLegacyRecoveryPending,
) error {
	return s.writeProofResourceLegacyRecoveryPending(path, pending)
}
var proofResourceReleaseLegacyRecoveryAfter = func(home, holder string, now time.Time, finalize func() error) (landlease.ReleaseResult, error) {
	return landlease.ProofResource(home).ReleaseAfter(holder, now, finalize)
}
var proofResourceAfterLegacyRecoveryRelease = func(proofResourceLegacyRecoveryPending) error {
	return nil
}
var proofResourceClearLegacyCompletionPending = func(
	s *Store,
	path string,
	pending proofResourceCompletionPending,
) error {
	return s.clearLegacyProofResourceCompletionPending(path, pending)
}
var proofResourceClearLegacyRecoveryJournal = func(
	s *Store,
	path string,
	pending proofResourceLegacyRecoveryPending,
) error {
	return s.clearProofResourceLegacyRecoveryPending(path, pending)
}

// proofResourceAfterCompletionRetryMint is the deterministic hard-crash test boundary after a
// retry is fully bound but before its pending journal or Work reachability may advance.
var proofResourceAfterCompletionRetryMint = func(runattempt.RunAttempt) {}
var proofResourceWriteCompletionRetryJournal = func(
	s *Store,
	path string,
	pending proofResourceCompletionPending,
	_ runattempt.RunAttempt,
) error {
	return s.writeProofResourceCompletionPending(path, pending)
}
var proofResourceAfterCompletionRetryJournal = func(runattempt.RunAttempt, proofResourceCompletionPending) {}

type ownedProofReport struct {
	SchemaVersion  string                  `json:"schema_version"`
	ProfileKind    string                  `json:"profile_kind"`
	Status         string                  `json:"status"`
	GeneratedAt    string                  `json:"generated_at"`
	Repo           string                  `json:"repo"`
	BaseRef        string                  `json:"base_ref,omitempty"`
	SourceSnapshot *gitrepository.Snapshot `json:"source_snapshot,omitempty"`
	DryRun         bool                    `json:"dry_run"`
	CIOnly         bool                    `json:"ci_only,omitempty"`
	CIConsole      bool                    `json:"ci_console,omitempty"`
	ProofRefs      []string                `json:"proof_refs"`
	Steps          []struct {
		Name                  string                     `json:"name"`
		Command               []string                   `json:"command"`
		Dir                   string                     `json:"dir,omitempty"`
		SkipIfMissing         string                     `json:"skip_if_missing,omitempty"`
		ProofRef              string                     `json:"proof_ref"`
		Status                string                     `json:"status"`
		FailureFacts          []string                   `json:"failure_facts,omitempty"`
		ConsumerEvidence      *CanonicalConsumerEvidence `json:"consumer_evidence,omitempty"`
		RedactionEvidence     *PublicRedactionEvidence   `json:"redaction_evidence,omitempty"`
		PrivateSourceEvidence *PrivateSourceEvidence     `json:"private_source_evidence,omitempty"`
		RemoteEvidence        *RemoteProofEvidence       `json:"remote_evidence,omitempty"`
		HomeFixtureEvidence   *HomeFixtureEvidence       `json:"home_fixture_evidence,omitempty"`
	} `json:"steps"`
}

// ReserveProofResource evaluates and persists one ActionIntent/PolicyDecision around acquisition
// of the existing landlease mutex. A competing holder is refused before this function returns.
func (s *Store) ReserveProofResource(workID string, in ProofResourceReserveInput, now time.Time) (result ProofResourceReserveResult, err error) {
	observedStart := time.Now() // observation only; authority continues to use injected now
	defer func() {
		if err == nil {
			observedAt := time.Now()
			s.observeProofAdmission(result.Reservation, observedAt.Sub(observedStart), observedAt, in.busyWhenHeld)
		}
	}()
	owned, prepareErr := s.prepareProofAdmission(&in)
	if prepareErr != nil {
		return ProofResourceReserveResult{}, prepareErr
	}
	if owned {
		defer func() { err = errors.Join(err, in.admission.Cancel()) }()
	}
	err = s.withLifecycleMutation(func() error {
		if in.admission == nil {
			in.resource = landlease.ProofResource(s.home)
			result, err = s.reserveProofResourceLocked(strings.TrimSpace(workID), in, now)
			return err
		}
		ctx := in.admissionContext
		if ctx == nil {
			ctx = context.Background()
		}
		return in.admission.WithTurnFor(ctx, in.Steal, preferTerminatedRun(in.TerminatedRunAttemptID), func(resource *landlease.Resource) error {
			in.resource = resource
			in.coordination = resource.ProofIdentity()
			in.busyWhenHeld = true
			result, err = s.reserveProofResourceLocked(strings.TrimSpace(workID), in, time.Now())
			return err
		})
	})
	if err != nil {
		return result, err
	}
	predecessorHolders, lineageErr := proofReservationPredecessorHolders(result.Reservation)
	if lineageErr != nil {
		barrierErr := s.retainProofTakeoverBarrier(result.Reservation, lineageErr, now)
		return result, errors.Join(lineageErr, barrierErr)
	}
	if len(predecessorHolders) == 0 {
		return result, nil
	}
	waitStarted := time.Now()
	grace := 3 * time.Second
	if ttlGrace := in.TTL / 3; ttlGrace > 0 && ttlGrace < grace {
		grace = ttlGrace
	}
	if grace < 100*time.Millisecond {
		grace = 100 * time.Millisecond
	}
	waitErr := s.awaitRecoveredProofAttempt(predecessorHolders[0], grace)
	checkAt := now.Add(time.Since(waitStarted))
	if waitErr == nil {
		status, statusErr := landlease.ProofResourceForIdentity(s.home, result.Reservation.ProofCoordination).Status(checkAt)
		if statusErr != nil {
			waitErr = statusErr
		} else if !status.Held || status.Holder != result.Reservation.Holder {
			waitErr = fmt.Errorf(
				"proof resource holder %q changed while awaiting predecessor termination; current holder %q",
				result.Reservation.Holder,
				status.Holder,
			)
		}
	}
	if waitErr != nil {
		barrierErr := s.retainProofTakeoverBarrier(result.Reservation, waitErr, checkAt)
		return result, errors.Join(waitErr, barrierErr)
	}
	return result, err
}

func (s *Store) retainProofTakeoverBarrier(
	reservation ProofResourceReservation,
	cause error,
	now time.Time,
) error {
	return s.withLifecycleMutation(func() error {
		reason := "proof resource takeover retained as a non-executable barrier: " + cause.Error()
		if err := s.completeProofAttempt(
			reservation.RunAttemptID,
			runattempt.StatusAbandoned,
			reason,
			now,
		); err != nil {
			return err
		}
		state := ""
		if w, err := s.Get(reservation.WorkID); err == nil {
			state = w.State
		}
		attached := []string{reservation.ID, reservation.RunAttemptID}
		if reservation.RecoveredRunAttemptID != "" {
			attached = append(attached, reservation.RecoveredRunAttemptID)
		}
		if reservation.PriorRunAttemptID != "" {
			attached = append(attached, reservation.PriorRunAttemptID)
		}
		return s.appendEvent(Event{
			TS:       ts(now),
			WorkID:   reservation.WorkID,
			From:     state,
			To:       state,
			By:       reservation.Actor,
			Note:     reason,
			Attached: attached,
		})
	})
}

func (s *Store) awaitRecoveredProofAttempt(holder string, grace time.Duration) error {
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		blocker, err := s.proofRecoveryOldestNonterminal(holder)
		if err != nil {
			return err
		}
		if blocker.RunAttemptID == "" {
			return nil
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return fmt.Errorf(
				"proof resource predecessor %s did not confirm termination; expiry or a synthetic terminal barrier cannot make a successor executable",
				blocker.RunAttemptID,
			)
		}
	}
}

// RecoverLegacyProofResourceCompletion releases one exact retained proof holder
// whose report predates source-bound proof schema v0.3. It intentionally does
// not resume completion: the old result is retired and a fresh battery remains
// mandatory.
func (s *Store) RecoverLegacyProofResourceCompletion(
	workID string,
	in ProofResourceLegacyRecoveryInput,
	now time.Time,
) (result ProofResourceLegacyRecoveryResult, found bool, err error) {
	err = s.withLifecycleMutation(func() error {
		workID = strings.TrimSpace(workID)
		in.Actor = strings.TrimSpace(in.Actor)
		in.Workspace = strings.TrimSpace(in.Workspace)
		in.Command = strings.TrimSpace(in.Command)
		if workID == "" || in.Actor == "" || in.Workspace == "" || in.Command == "" {
			return fmt.Errorf("legacy proof recovery requires work, actor, workspace, and original command")
		}

		journal, journalPresent, journalPath, err := s.readProofResourceLegacyRecoveryPending(workID)
		if err != nil {
			return err
		}
		if journalPresent {
			found = true
			result, err = s.resumeLegacyProofResourceRecovery(journal, journalPath, in, now)
			return err
		}

		status, err := s.proofStatusForWork(workID, now)
		if err != nil {
			return err
		}
		expiredExact := !status.Held && status.IsExpired
		if !status.Held && !expiredExact {
			return nil
		}
		reservationID, runAttemptID, heldWorkID, proofHolder := parseProofResourceHolder(status.Holder)
		if !proofHolder || heldWorkID != workID {
			return nil
		}
		found = true

		var reservation ProofResourceReservation
		if err := s.readProofReservationInto(reservationID, &reservation); err != nil {
			return fmt.Errorf("read retained legacy proof reservation %s: %w", reservationID, err)
		}
		pending, present, _, err := s.readProofResourceCompletionPending(reservationID)
		if err != nil {
			return err
		}
		if !present {
			found = false
			return nil
		}
		if !proofReservationSchemaValid(reservation) ||
			reservation.ID != reservationID ||
			reservation.Kind != "work.proof_resource.reservation" ||
			reservation.Status != ProofResourceAcquired ||
			reservation.WorkID != workID ||
			reservation.Actor != in.Actor ||
			reservation.Resource != ProofResourceName ||
			reservation.Holder != status.Holder ||
			reservation.RunAttemptID != runAttemptID ||
			reservation.Workspace != in.Workspace ||
			reservation.Command != in.Command {
			return fmt.Errorf("retained legacy proof reservation identity changed")
		}
		if reservation.Profile.SourceSnapshot != nil {
			return fmt.Errorf("legacy proof recovery refuses source-bound reservation")
		}

		w, err := s.Get(workID)
		if err != nil {
			return err
		}
		if len(w.VerificationIDs) == 0 {
			w.VerificationIDs = nil
		}
		claim, err := s.readLease(workID)
		if err != nil || claim.By != in.Actor {
			return fmt.Errorf("legacy proof recovery requires work lease held by %q", in.Actor)
		}
		expectedReservationAction, err := proofReservationExpectedAction(reservation)
		if err != nil {
			return fmt.Errorf("legacy proof reservation governance state: %w", err)
		}
		if err := s.validateStoredProofGovernance(
			w,
			in.Actor,
			reservation.ActionIntentID,
			reservation.PolicyDecisionID,
			expectedReservationAction,
		); err != nil {
			return fmt.Errorf("legacy proof reservation governance: %w", err)
		}

		completeInput := ProofResourceCompleteInput{
			Actor:             in.Actor,
			ReservationID:     reservation.ID,
			ReportArtifactRef: pending.Result.ReportArtifactRef,
			ProducerExitKnown: pending.Result.ProducerExitKnown,
			ProducerExitCode:  pending.Result.ProducerExitCode,
			FailureReason:     pending.Result.FailureReason,
		}
		expect, err := s.deriveProofResourceCompletionExpectation(workID, reservation, completeInput)
		if err != nil {
			return err
		}
		if pending.InputDigest != expect.InputDigest {
			return fmt.Errorf("retained legacy proof completion input digest changed")
		}
		attempt, err := s.runAttempts().Get(pending.Result.RunAttemptID)
		if err != nil {
			return fmt.Errorf("retained legacy proof RunAttempt: %w", err)
		}
		_, migrated, err := s.validateAndNormalizeProofResourceCompletionPending(
			w,
			reservation,
			pending,
			attempt,
			completeInput,
			expect,
		)
		if err != nil {
			return err
		}
		if migrated {
			return fmt.Errorf("legacy proof recovery refuses unsealed completion governance")
		}

		if pending.Result.SourceSnapshot != nil ||
			oneOf(pending.VerificationRef, w.VerificationIDs) {
			return fmt.Errorf("legacy proof recovery refuses source-bound or already-promoted proof")
		}
		if completeInput.ReportArtifactRef == "" {
			if strings.TrimSpace(completeInput.FailureReason) == "" ||
				expect.VerificationRef != "" ||
				expect.ResultStatus != ProofResourceFailed ||
				pending.Result.Status != ProofResourceFailed ||
				pending.Result.ReportStatus != "" ||
				pending.Result.OwnedReport ||
				pending.Result.VerificationRef != "" ||
				pending.TerminalStatus != runattempt.StatusFailed {
				return fmt.Errorf("legacy proof recovery refuses reportless completion unless it is unequivocally failed and non-verifying")
			}
		} else {
			artifacts, err := home.NewArtifactStore(s.home)
			if err != nil {
				return err
			}
			reportBody, err := artifacts.Get(completeInput.ReportArtifactRef)
			if err != nil {
				return fmt.Errorf("legacy proof report artifact: %w", err)
			}
			report, err := decodeOwnedProofReport(reportBody)
			if err != nil {
				return fmt.Errorf("legacy proof report decode: %w", err)
			}
			if report.SchemaVersion != "compound.proof_check_report.v0.1" &&
				report.SchemaVersion != "compound.proof_check_report.v0.2" {
				return fmt.Errorf("legacy proof recovery requires a pre-v0.3 owned report")
			}
			if report.SourceSnapshot != nil {
				return fmt.Errorf("legacy proof recovery refuses a source-bound report")
			}
		}

		journal, err = s.newProofResourceLegacyRecoveryPending(
			w,
			reservation,
			pending,
			attempt,
			in,
			now,
		)
		if err != nil {
			return err
		}
		if err := proofResourceWriteLegacyRecoveryJournal(s, journalPath, journal); err != nil {
			return fmt.Errorf("seal legacy proof recovery journal: %w", err)
		}
		result, err = s.resumeLegacyProofResourceRecovery(journal, journalPath, in, now)
		return err
	})
	return result, found, err
}

// ResumeProofResourceCompletion finishes an exact pending completion for the current live proof
// holder. The caller uses this before ReserveProofResource: a retained post-commit lease is released
// through its existing reservation, pending input, governance, and RunAttempt without entering the
// proof runner or minting a conflicting holder. found=false means normal admission should continue.
func (s *Store) ResumeProofResourceCompletion(
	workID string,
	in ProofResourceResumeInput,
	now time.Time,
) (result ProofResourceCompleteResult, found bool, err error) {
	workID = strings.TrimSpace(workID)
	in.Actor = strings.TrimSpace(in.Actor)
	in.Workspace = strings.TrimSpace(in.Workspace)
	in.Command = strings.TrimSpace(in.Command)
	status, err := s.proofStatusForWork(workID, now)
	if err != nil {
		return ProofResourceCompleteResult{}, false, err
	}
	expiredExact := !status.Held && status.IsExpired
	if !status.Held && !expiredExact {
		return ProofResourceCompleteResult{}, false, nil
	}
	reservationID, runAttemptID, heldWorkID, proofHolder := parseProofResourceHolder(status.Holder)
	if !proofHolder || heldWorkID != workID {
		return ProofResourceCompleteResult{}, false, nil
	}
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(reservationID, &reservation); err != nil {
		return ProofResourceCompleteResult{}, true, fmt.Errorf("read retained proof reservation %s: %w", reservationID, err)
	}
	if in.ReservationID != "" && reservation.ID != in.ReservationID {
		return ProofResourceCompleteResult{}, true, fmt.Errorf(
			"retained proof completion does not match the requested reservation %s",
			in.ReservationID,
		)
	}
	pending, present, _, err := s.readProofResourceCompletionPending(reservationID)
	if err != nil {
		return ProofResourceCompleteResult{}, true, err
	}
	if !present {
		return ProofResourceCompleteResult{}, false, nil
	}
	if err := s.verifyRetainedPendingSHA256(reservation.ID, in.ExpectedPendingSHA256); err != nil {
		return ProofResourceCompleteResult{}, true, err
	}
	if in.RequireFailedResult {
		if err := authenticateRetainedFailedProofResult(workID, in.Actor, reservation, pending); err != nil {
			return ProofResourceCompleteResult{}, true, err
		}
	}
	profile, profileErr := normalizeProofResourceProfile(in.Profile)
	if profileErr == nil && strings.TrimSpace(in.Profile.Digest) != "" && in.Profile.Digest != profile.Digest {
		profileErr = fmt.Errorf("proof resource profile digest %q does not match canonical %q", in.Profile.Digest, profile.Digest)
	}
	if profileErr != nil {
		return ProofResourceCompleteResult{}, true, fmt.Errorf("resume proof resource profile: %w", profileErr)
	}
	if workID == "" ||
		in.Actor == "" ||
		in.Workspace == "" ||
		in.Command == "" ||
		reservation.WorkID != workID ||
		reservation.Actor != in.Actor ||
		reservation.Workspace != in.Workspace ||
		reservation.Command != in.Command ||
		reservation.Status != ProofResourceAcquired ||
		reservation.Holder != status.Holder ||
		reservation.RunAttemptID != runAttemptID ||
		!reflect.DeepEqual(reservation.Profile, profile) ||
		pending.WorkID != workID ||
		pending.ReservationID != reservation.ID ||
		pending.Result.Actor != in.Actor {
		return ProofResourceCompleteResult{}, true, fmt.Errorf("retained proof completion does not match the exact work, actor, command, workspace, profile, and holder")
	}
	completeInput := ProofResourceCompleteInput{
		Actor:             in.Actor,
		ReservationID:     reservation.ID,
		ReportArtifactRef: pending.Result.ReportArtifactRef,
		ProducerExitKnown: pending.Result.ProducerExitKnown,
		ProducerExitCode:  pending.Result.ProducerExitCode,
		FailureReason:     pending.Result.FailureReason,
	}
	inputDigest, err := proofResourceCompletionInputDigest(workID, completeInput)
	if err != nil {
		return ProofResourceCompleteResult{}, true, err
	}
	if inputDigest != pending.InputDigest {
		return ProofResourceCompleteResult{}, true, fmt.Errorf("retained proof completion input digest changed")
	}
	// The expected-input fence lives inside the guard that performs the recovery effects: a
	// wrapper-only precheck is racy, and the recovery may not begin on journal bytes the
	// operator did not expect. The fence is never a standalone empty lock window.
	proofResourceBeforeRetainedMutation(reservation.ID)
	if strings.TrimSpace(in.ExpectedPendingSHA256) != "" {
		// ONE guard covers the fence, the rearm/reconcile and the owned-report publication, so
		// changed bytes refuse before any report, custody, rearm or attempt mutation. The
		// guard is entered once: the native completion takes its own guard afterwards, and the
		// lifecycle lock is never reacquired while it is held.
		if err := s.withLifecycleMutation(func() error {
			if err := s.verifyRetainedPendingSHA256(reservation.ID, in.ExpectedPendingSHA256); err != nil {
				return err
			}
			if in.RequireFailedResult {
				// The dedicated failure route requires an EXISTING governance seal before any
				// effect. The ordinary legacy reconstruction of a missing seal stays untouched
				// for every other path.
				if err := s.requireRetainedFailedProofGovernanceSeal(reservation.ID); err != nil {
					return err
				}
			}
			// The dedicated failure route authenticates EVERYTHING it needs BEFORE it writes
			// anything: the current Work lease owner, the immutable retry ancestry and the
			// native execution fence. The authentication is READ-ONLY and runs under the SAME
			// effects guard as the effects it protects, so an expired holder with a corrupt
			// ancestry refuses BEFORE its rearm and before any reachability repair.
			var retainedAuth retainedFailedProofAuthentication
			if in.RequireFailedResult {
				auth, err := s.authenticateRetainedFailedProofCompletionLocked(
					workID,
					reservation,
					completeInput,
				)
				if err != nil {
					return err
				}
				retainedAuth = auth
				// The exclusive proof resource must already belong to the exact reservation
				// holder before ANY effect of the recovery. A stale holder refuses here,
				// before the rearm preparation, the rearm and the reachability repair.
				if err := s.requireProofResourceHolderForRearm(reservation, now); err != nil {
					return fmt.Errorf("retained failed proof recovery: %w", err)
				}
			}
			if expiredExact {
				if err := s.rearmExpiredProofResourceCompletionLocked(
					workID,
					in,
					reservation,
					pending,
					completeInput,
					status,
					now,
				); err != nil {
					return err
				}
			} else if err := s.reconcileRearmedProofResourceCompletionLocked(
				workID,
				in,
				reservation,
				pending,
				completeInput,
				status,
				now,
			); err != nil {
				return err
			}
			if in.RequireFailedResult {
				// The native finalizer rule still holds for the dedicated route: only the exact
				// current holder of the exclusive proof resource may publish the owned report.
				// The ACTUAL current status is read HERE, under the effects guard, so a
				// legitimate own rearm is preserved and a stale holder refuses before the
				// output.
				if err := s.requireProofResourceHolder(reservation, now); err != nil {
					return fmt.Errorf("retained failed proof recovery: %w", err)
				}
				// Work reachability is the last native fact this route needs. An interrupted
				// retry may have been minted and journaled before its forward link into the
				// Work envelope was persisted: that is a legitimate native recovery state, so
				// the immutable ancestry is authenticated FIRST and the existing native
				// reachability recovery then repairs the missing link.
				if err := s.repairRetainedFailedProofRetryReachability(
					workID,
					reservation,
					retainedAuth.Pending,
					retainedAuth.Chain,
					retainedAuth.Terminal,
					now,
				); err != nil {
					return err
				}
			}
			if pending.Result.ReportArtifactRef != "" {
				if in.PrepareOwnedReport == nil {
					return fmt.Errorf("retained proof completion requires owned report publication before release")
				}
				if err := in.PrepareOwnedReport(pending.Result); err != nil {
					return fmt.Errorf("prepare retained owned proof report: %w", err)
				}
			}
			return nil
		}); err != nil {
			return ProofResourceCompleteResult{}, true, err
		}
		result, err = s.completeProofResourceFenced(workID, completeInput, in.ExpectedPendingSHA256, now)
		return result, true, err
	}
	if expiredExact {
		if err := s.rearmExpiredProofResourceCompletion(
			workID,
			in,
			reservation,
			pending,
			completeInput,
			status,
			now,
		); err != nil {
			return ProofResourceCompleteResult{}, true, err
		}
	} else if err := s.reconcileRearmedProofResourceCompletion(
		workID,
		in,
		reservation,
		pending,
		completeInput,
		status,
		now,
	); err != nil {
		return ProofResourceCompleteResult{}, true, err
	}
	if pending.Result.ReportArtifactRef != "" {
		if in.PrepareOwnedReport == nil {
			return ProofResourceCompleteResult{}, true, fmt.Errorf("retained proof completion requires owned report publication before release")
		}
		if err := in.PrepareOwnedReport(pending.Result); err != nil {
			return ProofResourceCompleteResult{}, true, fmt.Errorf("prepare retained owned proof report: %w", err)
		}
	}
	result, err = s.CompleteProofResource(workID, completeInput, now)
	return result, true, err
}

func (s *Store) deriveProofResourceCompletionExpectation(
	workID string,
	reservation ProofResourceReservation,
	in ProofResourceCompleteInput,
) (proofResourceCompletionExpectation, error) {
	inputDigest, err := proofResourceCompletionInputDigest(workID, in)
	if err != nil {
		return proofResourceCompletionExpectation{}, err
	}
	reportStatus := ""
	ownedReport := false
	var sourceSnapshot *gitrepository.Snapshot
	if in.ReportArtifactRef != "" {
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			return proofResourceCompletionExpectation{}, err
		}
		body, err := artifacts.Get(in.ReportArtifactRef)
		if err != nil {
			return proofResourceCompletionExpectation{}, fmt.Errorf("proof report artifact: %w", err)
		}
		report, err := decodeOwnedProofReport(body)
		if err != nil {
			return proofResourceCompletionExpectation{}, fmt.Errorf("proof report decode: %w", err)
		}
		if err := s.validateRemoteProofArtifacts(report); err != nil {
			return proofResourceCompletionExpectation{}, err
		}
		if err := validateOwnedProofReportAgainstReservation(report, reservation); err != nil {
			return proofResourceCompletionExpectation{}, fmt.Errorf("proof report profile: %w", err)
		}
		if err := s.validateHomeFixtureReport(workID, report); err != nil {
			return proofResourceCompletionExpectation{}, fmt.Errorf("home fixture proof: %w", err)
		}
		reportStatus = report.Status
		ownedReport = true
		sourceSnapshot = report.SourceSnapshot
	}
	resultID := idFor("pfrs_", workID, reservation.ID, in.ReportArtifactRef, in.FailureReason)
	resultStatus := ProofResourceFailed
	verificationRef := ""
	if reportStatus == "ok" {
		if in.ProducerExitKnown && in.ProducerExitCode == 0 {
			resultStatus = ProofResourceVerified
			verificationRef = "proof-check:" + resultID
		} else {
			resultStatus = ProofResourceInvalid
		}
	}
	if reportStatus == "failed" && in.ProducerExitKnown && in.ProducerExitCode == 0 {
		return proofResourceCompletionExpectation{}, fmt.Errorf("proof report failed but producer exit code was zero")
	}
	terminalStatus := runattempt.StatusFailed
	if resultStatus == ProofResourceVerified {
		terminalStatus = runattempt.StatusSucceeded
	}
	return proofResourceCompletionExpectation{
		InputDigest:     inputDigest,
		ResultStatus:    resultStatus,
		ReportStatus:    reportStatus,
		OwnedReport:     ownedReport,
		SourceSnapshot:  sourceSnapshot,
		VerificationRef: verificationRef,
		TerminalStatus:  terminalStatus,
		TerminalReason:  completionReason(resultStatus, in.FailureReason),
	}, nil
}

// requireProofWorkLeaseOwner is the single owner of the native rule that a proof completion may
// only run for the actor that currently owns the Work lease. The stage names the caller so the
// refusal keeps naming the exact native step that refused it.
func (s *Store) requireProofWorkLeaseOwner(workID, actor, stage string) error {
	claim, err := s.readLease(workID)
	if err != nil || claim.By != actor {
		return fmt.Errorf("%s requires work lease held by %q", stage, actor)
	}
	return nil
}

// proofResourceHolderStatus reads the ACTUAL current status of the coordinated proof resource.
// Every holder rule of a proof completion reads the status through this one owner, so a caller
// that already holds the effects guard observes the status as it is NOW and not the status it
// captured before it took the guard.
func (s *Store) proofResourceHolderStatus(reservation ProofResourceReservation, now time.Time) (landlease.StatusResult, error) {
	status, err := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).Status(now)
	if err != nil {
		return landlease.StatusResult{}, fmt.Errorf("proof resource status: %w", err)
	}
	return status, nil
}

// requireProofResourceHolder is the single owner of the native finalizer rule that a proof
// completion may only run while its own reservation holds the exclusive proof resource.
//
// A caller that legitimately rearmed its own expired holder observes its own live lease here, so
// the rearmed generation is preserved and never compared with the expired status it replaced.
func (s *Store) requireProofResourceHolder(reservation ProofResourceReservation, now time.Time) error {
	status, err := s.proofResourceHolderStatus(reservation, now)
	if err != nil {
		return err
	}
	if !status.Held || status.Holder != reservation.Holder {
		if status.Holder == reservation.Holder && status.IsExpired {
			return fmt.Errorf("proof reservation %s expired at %s", reservation.ID, status.ExpiresAt)
		}
		return fmt.Errorf("proof reservation %s no longer holds exclusive resource; current holder %q", reservation.ID, status.Holder)
	}
	return nil
}

// requireProofResourceHolderForRearm is the same native rule at its PRE-EFFECT strictness: the
// exclusive proof resource must already belong to the exact reservation holder. The holder need
// not be live yet, because the rearm is what makes an expired holder of ours live again. The
// expired status of a holder that is about to be rearmed is therefore never compared with the
// lease it rearmed.
func (s *Store) requireProofResourceHolderForRearm(reservation ProofResourceReservation, now time.Time) error {
	status, err := s.proofResourceHolderStatus(reservation, now)
	if err != nil {
		return err
	}
	if status.Holder != reservation.Holder {
		return fmt.Errorf("proof reservation %s no longer holds exclusive resource; current holder %q", reservation.ID, status.Holder)
	}
	return nil
}

func (s *Store) rearmExpiredProofResourceCompletion(
	workID string,
	in ProofResourceResumeInput,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
	completeInput ProofResourceCompleteInput,
	expiredStatus landlease.StatusResult,
	now time.Time,
) error {
	return s.withLifecycleMutation(func() error {
		return s.rearmExpiredProofResourceCompletionLocked(
			workID,
			in,
			reservation,
			pending,
			completeInput,
			expiredStatus,
			now,
		)
	})
}

// rearmExpiredProofResourceCompletionLocked is the rearm body for a caller that already holds
// the lifecycle mutation guard. It never takes that guard again.
func (s *Store) rearmExpiredProofResourceCompletionLocked(
	workID string,
	in ProofResourceResumeInput,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
	completeInput ProofResourceCompleteInput,
	expiredStatus landlease.StatusResult,
	now time.Time,
) error {
	w, err := s.Get(workID)
	if err != nil {
		return err
	}
	if err := s.requireProofWorkLeaseOwner(workID, in.Actor, "expired proof completion rearm"); err != nil {
		return err
	}
	var currentReservation ProofResourceReservation
	if err := s.readProofReservationInto(reservation.ID, &currentReservation); err != nil {
		return err
	}
	currentPending, present, pendingPath, err := s.readProofResourceCompletionPending(reservation.ID)
	if err != nil {
		return err
	}
	if !present ||
		!reflect.DeepEqual(currentReservation, reservation) ||
		!reflect.DeepEqual(currentPending, pending) {
		return fmt.Errorf("expired proof completion identity changed before rearm")
	}
	// The expected-input fence is re-checked inside the native mutation guard, before the
	// scarce mutation and before the journal may be reseeded by a legitimate retry.
	if err := s.verifyRetainedPendingSHA256(reservation.ID, in.ExpectedPendingSHA256); err != nil {
		return err
	}
	expect, err := s.deriveProofResourceCompletionExpectation(workID, currentReservation, completeInput)
	if err != nil {
		return err
	}
	ra, err := s.runAttempts().Get(currentPending.Result.RunAttemptID)
	if err != nil {
		return err
	}
	normalized, migrated, err := s.validateAndNormalizeProofResourceCompletionPending(
		w,
		currentReservation,
		currentPending,
		ra,
		completeInput,
		expect,
	)
	if err != nil {
		return err
	}
	if migrated {
		if err := s.writeProofResourceCompletionPending(pendingPath, normalized); err != nil {
			return fmt.Errorf("seal expired proof completion recovery journal: %w", err)
		}
		currentPending = normalized
	}
	action := s.proofResourceAction(
		w,
		currentReservation.ID,
		in.Actor,
		"completion_rearm",
		"",
		"",
		"",
		"",
		now,
	)
	decision, err := s.evaluateProofResourceAction(action, now)
	if err != nil {
		return err
	}
	if !policy.Allowed(decision) {
		return fmt.Errorf("expired proof completion rearm policy %s", decision.Outcome)
	}
	if err := s.persistProofScarceMutationPreparation(
		&w,
		action,
		decision,
		currentReservation,
		currentPending.Result.RunAttemptID,
		"completion_rearm",
		now,
	); err != nil {
		return err
	}
	rearmed, err := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).RearmExpiredExact(expiredStatus, now)
	if err != nil {
		return fmt.Errorf("rearm exact expired proof completion holder: %w", err)
	}
	proofResourceAfterScarceMutation("completion_rearm")
	refresh := ProofResourceRefresh{
		SchemaVersion:    ProofResourceRefreshSchemaVersion,
		ID:               idFor("pfrh_", currentReservation.ID, currentReservation.RunAttemptID, rearmed.ExpiresAt),
		Kind:             "work.proof_resource.refresh",
		CreatedAt:        now.UTC().Format(time.RFC3339Nano),
		WorkID:           workID,
		Actor:            in.Actor,
		ReservationID:    currentReservation.ID,
		RunAttemptID:     currentReservation.RunAttemptID,
		Holder:           currentReservation.Holder,
		WindowOpenedAt:   currentReservation.AcquiredAt,
		RefreshedAt:      rearmed.AcquiredAt,
		ExpiresAt:        rearmed.ExpiresAt,
		TTLSeconds:       rearmed.TTLSeconds,
		ActionIntentID:   action.ID,
		PolicyDecisionID: decision.ID,
	}
	return s.persistProofRefresh(&w, action, decision, refresh, now)
}

func (s *Store) reconcileRearmedProofResourceCompletion(
	workID string,
	in ProofResourceResumeInput,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
	completeInput ProofResourceCompleteInput,
	liveStatus landlease.StatusResult,
	now time.Time,
) error {
	if !liveStatus.Held ||
		liveStatus.Holder != reservation.Holder ||
		liveStatus.AcquiredAt == reservation.AcquiredAt {
		return nil
	}
	return s.withLifecycleMutation(func() error {
		return s.reconcileRearmedProofResourceCompletionLocked(
			workID,
			in,
			reservation,
			pending,
			completeInput,
			liveStatus,
			now,
		)
	})
}

// reconcileRearmedProofResourceCompletionLocked is the reconciliation body for a caller that
// already holds the lifecycle mutation guard. It never takes that guard again.
func (s *Store) reconcileRearmedProofResourceCompletionLocked(
	workID string,
	in ProofResourceResumeInput,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
	completeInput ProofResourceCompleteInput,
	liveStatus landlease.StatusResult,
	now time.Time,
) error {
	if !liveStatus.Held ||
		liveStatus.Holder != reservation.Holder ||
		liveStatus.AcquiredAt == reservation.AcquiredAt {
		return nil
	}
	currentStatus, err := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).Status(now)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(currentStatus, liveStatus) {
		return fmt.Errorf("rearmed proof completion lease changed before refresh reconciliation")
	}
	w, err := s.Get(workID)
	if err != nil {
		return err
	}
	if err := s.requireProofWorkLeaseOwner(workID, in.Actor, "rearmed proof completion reconciliation"); err != nil {
		return err
	}
	var currentReservation ProofResourceReservation
	if err := s.readProofReservationInto(reservation.ID, &currentReservation); err != nil {
		return err
	}
	currentPending, present, _, err := s.readProofResourceCompletionPending(reservation.ID)
	if err != nil {
		return err
	}
	if !present ||
		!reflect.DeepEqual(currentReservation, reservation) ||
		!reflect.DeepEqual(currentPending, pending) {
		return fmt.Errorf("rearmed proof completion identity changed before refresh reconciliation")
	}
	// The expected-input fence is re-checked inside the native mutation guard, before the
	// scarce mutation and before the journal may be reseeded by a legitimate retry.
	if err := s.verifyRetainedPendingSHA256(reservation.ID, in.ExpectedPendingSHA256); err != nil {
		return err
	}
	latestReservation, err := s.proofReservationWithLatestRefresh(currentReservation)
	if err != nil {
		return err
	}
	if latestReservation.ExpiresAt == liveStatus.ExpiresAt &&
		latestReservation.TTLSeconds == liveStatus.TTLSeconds {
		return nil
	}
	expect, err := s.deriveProofResourceCompletionExpectation(workID, currentReservation, completeInput)
	if err != nil {
		return err
	}
	ra, err := s.runAttempts().Get(currentPending.Result.RunAttemptID)
	if err != nil {
		return err
	}
	_, migrated, err := s.validateAndNormalizeProofResourceCompletionPending(
		w,
		currentReservation,
		currentPending,
		ra,
		completeInput,
		expect,
	)
	if err != nil {
		return err
	}
	if migrated {
		return fmt.Errorf("rearmed proof completion journal was not sealed before scarce mutation")
	}

	var decision envelope.PolicyDecision
	if err := s.readEventJSON(w.PolicyDecisionID, &decision); err != nil {
		return fmt.Errorf("read prepared rearm PolicyDecision: %w", err)
	}
	var action envelope.ActionIntent
	if err := s.readEventJSON(decision.ActionIntentID, &action); err != nil {
		return fmt.Errorf("read prepared rearm ActionIntent: %w", err)
	}
	actionTime, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		return fmt.Errorf("prepared rearm ActionIntent created_at: %w", err)
	}
	expectedAction := s.proofResourceAction(
		w,
		currentReservation.ID,
		in.Actor,
		"completion_rearm",
		"",
		"",
		"",
		"",
		actionTime,
	)
	expectedDecision, err := s.evaluateProofResourceAction(expectedAction, actionTime)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(action, expectedAction) ||
		!reflect.DeepEqual(decision, expectedDecision) ||
		!policy.Allowed(decision) {
		return fmt.Errorf("prepared rearm governance does not match canonical exact-holder recovery")
	}
	preparedAttached := []string{
		action.ID,
		decision.ID,
		currentReservation.ID,
		currentPending.Result.RunAttemptID,
	}
	prepared := false
	events, err := s.eventsFor(workID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.WorkID == workID &&
			event.By == in.Actor &&
			event.Note == "proof resource completion_rearm governance persisted before scarce mutation" &&
			reflect.DeepEqual(event.Attached, preparedAttached) {
			prepared = true
			break
		}
	}
	if !prepared {
		return fmt.Errorf("prepared rearm governance has no reachable pre-mutation event")
	}
	if _, err := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).AttestExecutionTerminated(currentReservation.Holder); err != nil {
		return fmt.Errorf("reconcile rearmed proof execution termination: %w", err)
	}
	refresh := ProofResourceRefresh{
		SchemaVersion:    ProofResourceRefreshSchemaVersion,
		ID:               idFor("pfrh_", currentReservation.ID, currentReservation.RunAttemptID, liveStatus.ExpiresAt),
		Kind:             "work.proof_resource.refresh",
		CreatedAt:        action.CreatedAt,
		WorkID:           workID,
		Actor:            in.Actor,
		ReservationID:    currentReservation.ID,
		RunAttemptID:     currentReservation.RunAttemptID,
		Holder:           currentReservation.Holder,
		WindowOpenedAt:   currentReservation.AcquiredAt,
		RefreshedAt:      liveStatus.AcquiredAt,
		ExpiresAt:        liveStatus.ExpiresAt,
		TTLSeconds:       liveStatus.TTLSeconds,
		ActionIntentID:   action.ID,
		PolicyDecisionID: decision.ID,
	}
	return s.persistProofRefresh(&w, action, decision, refresh, actionTime)
}

func (s *Store) reserveProofResourceLocked(workID string, in ProofResourceReserveInput, now time.Time) (ProofResourceReserveResult, error) {
	if err := checkProofWaitAdmission(in); err != nil {
		return ProofResourceReserveResult{}, err
	}
	in.Actor = strings.TrimSpace(in.Actor)
	in.Workspace = strings.TrimSpace(in.Workspace)
	in.Command = strings.TrimSpace(in.Command)
	in.StealReason = strings.TrimSpace(in.StealReason)
	in.TerminatedRunAttemptID = strings.TrimSpace(in.TerminatedRunAttemptID)
	in.TerminationEvidenceRef = strings.TrimSpace(in.TerminationEvidenceRef)
	profile, profileErr := normalizeProofResourceProfile(in.Profile)
	if profileErr == nil && strings.TrimSpace(in.Profile.Digest) != "" && in.Profile.Digest != profile.Digest {
		profileErr = fmt.Errorf("proof resource profile digest %q does not match canonical %q", in.Profile.Digest, profile.Digest)
	}
	in.Profile = profile
	switch {
	case workID == "":
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource work_id is required")
	case in.Actor == "":
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource actor is required")
	case in.Workspace == "":
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource workspace is required")
	case in.Command == "":
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource command is required")
	case profileErr != nil:
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource profile: %w", profileErr)
	case in.Profile.Repo != in.Workspace:
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource profile repo %q does not match workspace %q", in.Profile.Repo, in.Workspace)
	case in.TTL < time.Second:
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource ttl must be at least one second")
	case in.Steal && in.StealReason == "":
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource --steal requires a non-empty reason")
	case !in.Steal && in.StealReason != "":
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource steal reason requires --steal")
	case in.TerminationEvidenceRef != "" && in.TerminatedRunAttemptID == "":
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource termination evidence requires its terminated RunAttempt")
	case in.TerminationEvidenceRef != "" && !in.Steal:
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource termination evidence requires exceptional --steal governance")
	case in.TerminatedRunAttemptID != "" && !runattempt.Valid(in.TerminatedRunAttemptID):
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource terminated RunAttempt %q is invalid", in.TerminatedRunAttemptID)
	}

	w, err := s.recoverPendingClaimBeforeMutation(workID)
	if err != nil {
		return ProofResourceReserveResult{}, err
	}
	if w.GitDelivery != nil {
		if in.Profile.SourceSnapshot == nil {
			return ProofResourceReserveResult{}, fmt.Errorf("source-controlled proof requires sealed source snapshot")
		}
		wantFingerprint := strings.TrimSpace(w.GitDelivery.RepositoryFingerprint)
		gotFingerprint := strings.TrimSpace(in.Profile.SourceSnapshot.RepositoryFingerprint)
		if gotFingerprint != wantFingerprint {
			return ProofResourceReserveResult{}, fmt.Errorf(
				"proof repository fingerprint %s does not match WorkEnvelope git_delivery repository fingerprint %s",
				gotFingerprint,
				wantFingerprint,
			)
		}
	}
	if terminalStates[w.State] {
		return ProofResourceReserveResult{}, fmt.Errorf("work %s %s (terminal); cannot reserve proof resource", workID, w.State)
	}
	claim, err := s.readLease(workID)
	if err != nil {
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource requires a claimed work lease: %w", err)
	}
	if claim.By != in.Actor {
		return ProofResourceReserveResult{}, fmt.Errorf("proof resource work %s lease held by %q, actor is %q", workID, claim.By, in.Actor)
	}
	if err := s.validatePublicRedactionProfileAuthority(workID, in.Actor, in.Profile, now); err != nil {
		return ProofResourceReserveResult{}, fmt.Errorf("public redaction review admission: %w", err)
	}
	if err := s.validatePrivateSourceProfileAuthority(workID, in.Actor, in.Profile, now); err != nil {
		return ProofResourceReserveResult{}, err
	}

	if in.busyWhenHeld && !in.Steal {
		status, statusErr := in.resource.AdmissionStatus(now)
		if statusErr != nil {
			return ProofResourceReserveResult{}, statusErr
		}
		if status.Held {
			return ProofResourceReserveResult{}, &proofResourceBusyError{holder: status.Holder}
		}
	}

	if in.coordination != nil {
		predecessor, err := in.resource.AdmissionStatus(now)
		if err != nil {
			return ProofResourceReserveResult{}, err
		}
		if predecessor.Holder != "" {
			blocker, err := s.proofRecoveryOldestNonterminal(predecessor.Holder)
			if err != nil {
				return ProofResourceReserveResult{}, &landlease.ProofAdmissionBusyError{Holder: predecessor.Holder, Reason: "exact predecessor owner recovery required: " + err.Error()}
			}
			if !in.Steal && blocker.RunAttemptID != "" {
				return ProofResourceReserveResult{}, &landlease.ProofAdmissionBusyError{Holder: predecessor.Holder, Reason: "predecessor run requires owner recovery"}
			}
		}
	}
	reservationID := idFor("pfr_", workID, in.Actor, ts(now))
	priorHolder := ""
	if in.Steal {
		status, statusErr := in.resource.AdmissionStatus(now)
		if statusErr != nil {
			return ProofResourceReserveResult{}, statusErr
		}
		priorHolder = strings.TrimSpace(status.Holder)
	}
	var preparedTerminationEvidence []byte
	if in.TerminatedRunAttemptID != "" {
		if in.TerminationEvidenceRef == "" {
			var evidenceErr error
			in.TerminationEvidenceRef, preparedTerminationEvidence, evidenceErr = s.prepareTrustedProofTerminationEvidence(
				priorHolder,
				in.TerminatedRunAttemptID,
				in.Actor,
				now,
			)
			if evidenceErr != nil {
				return ProofResourceReserveResult{}, evidenceErr
			}
		}
		if len(preparedTerminationEvidence) == 0 {
			if err := s.validateProofTerminationEvidence(in.TerminationEvidenceRef, in.TerminatedRunAttemptID, priorHolder, now); err != nil {
				return ProofResourceReserveResult{}, err
			}
		}
		blocker, err := s.proofRecoveryOldestNonterminal(priorHolder)
		if err != nil {
			return ProofResourceReserveResult{}, err
		}
		if blocker.RunAttemptID == "" {
			return ProofResourceReserveResult{}, fmt.Errorf(
				"proof resource termination evidence targets %s, but predecessor lineage is already terminal",
				in.TerminatedRunAttemptID,
			)
		}
		if blocker.RunAttemptID != in.TerminatedRunAttemptID {
			return ProofResourceReserveResult{}, fmt.Errorf(
				"proof resource termination evidence targets %s, but oldest nonterminal predecessor is %s",
				in.TerminatedRunAttemptID,
				blocker.RunAttemptID,
			)
		}
	}
	action := s.proofResourceAction(
		w,
		reservationID,
		in.Actor,
		"reserve",
		priorHolder,
		in.StealReason,
		in.TerminatedRunAttemptID,
		in.TerminationEvidenceRef,
		now,
	)
	decision, err := s.evaluateProofResourceAction(action, now)
	if err != nil {
		return ProofResourceReserveResult{}, err
	}
	reservation := ProofResourceReservation{
		SchemaVersion:          ProofResourceReservationSchemaVersion,
		ID:                     reservationID,
		Kind:                   "work.proof_resource.reservation",
		Status:                 ProofResourceAcquired,
		CreatedAt:              now.UTC().Format(time.RFC3339Nano),
		WorkID:                 workID,
		Actor:                  in.Actor,
		Resource:               ProofResourceName,
		Workspace:              in.Workspace,
		Command:                in.Command,
		Profile:                in.Profile,
		ProofCoordination:      in.coordination,
		PriorHolder:            priorHolder,
		StealReason:            in.StealReason,
		TerminatedRunAttemptID: in.TerminatedRunAttemptID,
		TerminationEvidenceRef: in.TerminationEvidenceRef,
		ActionIntentID:         action.ID,
		PolicyDecisionID:       decision.ID,
	}
	if in.coordination != nil {
		reservation.SchemaVersion = ProofResourceCoordinatedReservationSchemaVersion
	}
	if err := checkProofWaitAdmission(in); err != nil {
		return ProofResourceReserveResult{}, err
	}
	existingRunIDs := append([]string(nil), w.RunAttemptIDs...)
	proofRun, err := s.prepareProofRunAttempt(w, action.ID, in, now)
	if proofRun.ID != "" {
		reservation.RunAttemptID = proofRun.ID
		reservation.Holder = proofResourceHolder(reservation.ID, proofRun.ID, workID, in.Actor)
	}
	if err != nil {
		return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, err, now)
	}
	if !policy.Allowed(decision) {
		reservation.Status = ProofResourceRefused
		terminalErr := s.completeProofAttempt(proofRun.ID, runattempt.StatusAbandoned, "proof resource policy refused before execution", now)
		persistErr := s.persistProofReservation(&w, action, decision, reservation, "proof resource policy refused before execution", now)
		result := ProofResourceReserveResult{SchemaVersion: ProofResourceReservationSchemaVersion, Reservation: reservation, Work: w}
		if terminalErr != nil || persistErr != nil {
			return result, errors.Join(fmt.Errorf("proof resource policy %s", decision.Outcome), terminalErr, persistErr)
		}
		return result, fmt.Errorf("proof resource policy %s", decision.Outcome)
	}
	if err := s.persistProofScarceMutationPreparation(
		&w,
		action,
		decision,
		reservation,
		proofRun.ID,
		"reserve",
		now,
	); err != nil {
		return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, err, now)
	}
	if len(preparedTerminationEvidence) != 0 {
		if err := s.persistTrustedProofTerminationEvidence(
			in.TerminationEvidenceRef,
			preparedTerminationEvidence,
		); err != nil {
			return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, err, now)
		}
	}
	if in.TerminationEvidenceRef != "" {
		reason := fmt.Sprintf(
			"governed termination evidence %s confirms predecessor %s terminated before exceptional proof takeover",
			in.TerminationEvidenceRef,
			in.TerminatedRunAttemptID,
		)
		if err := s.terminalizeGovernedProofPredecessor(in.TerminatedRunAttemptID, reason, now); err != nil {
			return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, err, now)
		}
		reservation.TerminationConfirmedAt = now.UTC().Format(time.RFC3339Nano)
		reservation.TerminationConfirmationID = idFor(
			"pfrt_",
			reservation.ID,
			in.TerminatedRunAttemptID,
			in.TerminationEvidenceRef,
			ts(now),
		)
		if err := s.persistProofTerminationConfirmation(&w, reservation, now); err != nil {
			return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, err, now)
		}
	}

	var lease landlease.Lease
	var recovered *landlease.Lease
	var executionBarrier *landlease.ExecutionBarrier
	barrierTransferred := false
	defer func() {
		if executionBarrier != nil && !barrierTransferred {
			_ = executionBarrier.Close()
		}
	}()
	prepareExactReservation := func(preparedLease landlease.Lease, preparedRecovered *landlease.Lease) error {
		if !reflect.DeepEqual(preparedLease.ProofCoordination, reservation.ProofCoordination) {
			return fmt.Errorf("proof lease coordination changed before reservation preparation")
		}

		applyProofResourceLease(&reservation, preparedLease, preparedRecovered)
		return nil
	}
	if in.Steal && priorHolder != "" {
		if in.TerminatedRunAttemptID == "" {
			predecessorResource, resolveErr := s.proofResourceForHolder(priorHolder)
			if resolveErr != nil {
				return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, resolveErr, now)
			}
			if _, requestErr := predecessorResource.TryRequestExecutionTermination(priorHolder,
				reservation.Holder,
				in.StealReason,
				now,
			); requestErr != nil {
				return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, requestErr, now)
			}
		}
		lease, executionBarrier, err = in.resource.StealExpectedWithExecutionBarrierPrepared(reservation.Holder,
			priorHolder,
			in.StealReason,
			now,
			func(prepared landlease.Lease) error {
				return prepareExactReservation(prepared, nil)
			},
		)
	} else {
		var outcome landlease.AcquireOutcome
		acquire := proofResourceAcquireWithExecutionBarrierPrepared
		if in.coordination != nil {
			acquire = func(_ string, holder string, ttl time.Duration, at time.Time, prepare func(landlease.AcquireOutcome) error) (landlease.AcquireOutcome, *landlease.ExecutionBarrier, error) {
				return in.resource.AcquireWithExecutionBarrierPrepared(holder, ttl, at, prepare)
			}
		}
		outcome, executionBarrier, err = acquire(
			s.home,
			reservation.Holder,
			in.TTL,
			now,
			func(prepared landlease.AcquireOutcome) error {
				return prepareExactReservation(prepared.Lease, prepared.ExpiredPredecessor)
			},
		)
		lease = outcome.Lease
		recovered = outcome.ExpiredPredecessor
	}
	if err != nil {
		var held *landlease.HeldError
		var executionPending *landlease.ExecutionPendingError
		if errors.As(err, &executionPending) {
			status, statusErr := in.resource.AdmissionStatus(now)
			if statusErr != nil {
				return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, errors.Join(err, statusErr), now)
			}
			held = &landlease.HeldError{
				Holder:    status.Holder,
				ExpiresAt: status.ExpiresAt,
				Remaining: status.RemainingSeconds,
			}
		}
		if held == nil && !errors.As(err, &held) {
			return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, err, now)
		}
		reservation.Status = ProofResourceRefused
		reservation.ConflictingHolder = held.Holder
		reservation.ExpiresAt = held.ExpiresAt
		if termErr := s.completeProofAttempt(proofRun.ID, runattempt.StatusAbandoned, "proof resource refused before execution", now); termErr != nil {
			return ProofResourceReserveResult{}, termErr
		}
		if persistErr := s.persistProofReservation(&w, action, decision, reservation, "proof resource refused before execution", now); persistErr != nil {
			return ProofResourceReserveResult{}, persistErr
		}
		return ProofResourceReserveResult{SchemaVersion: ProofResourceReservationSchemaVersion, Reservation: reservation, Work: w},
			&ProofResourceHeldError{
				WorkID:        workID,
				ReservationID: reservation.ID,
				Holder:        held.Holder,
				ExpiresAt:     held.ExpiresAt,
				Remaining:     held.Remaining,
			}
	}
	proofResourceAfterScarceMutation("reserve")

	applyProofResourceLease(&reservation, lease, recovered)
	if _, err := s.runAttempts().MarkRunning(proofRun.ID, now); err != nil {
		if executionBarrier != nil {
			_ = executionBarrier.Close()
		}
		_, _ = in.resource.Release(reservation.Holder, now)
		return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, err, now)
	}
	if err := s.persistProofReservation(&w, action, decision, reservation, "proof resource acquired before execution", now); err != nil {
		if executionBarrier != nil {
			_ = executionBarrier.Close()
		}
		_, _ = in.resource.Release(reservation.Holder, now)
		return s.failProofResourceAcquisition(&w, action, decision, reservation, proofRun, err, now)
	}
	s.supersedeClaimReservations(existingRunIDs, []string{proofRun.ID}, now)
	barrierTransferred = true
	return ProofResourceReserveResult{
		ExecutionBarrier: executionBarrier,
		SchemaVersion:    ProofResourceReservationSchemaVersion,
		Reservation:      reservation,
		Work:             w,
	}, nil
}

func applyProofResourceLease(
	reservation *ProofResourceReservation,
	lease landlease.Lease,
	recovered *landlease.Lease,
) {
	reservation.Holder = lease.Holder
	reservation.AcquiredAt = lease.AcquiredAt
	reservation.ExpiresAt = lease.ExpiresAt
	reservation.TTLSeconds = lease.TTLSeconds
	reservation.PriorHolder = lease.PriorHolder
	reservation.StealReason = lease.StealReason
	reservation.StolenAt = lease.StolenAt
	if recovered != nil {
		reservation.RecoveredHolder = recovered.Holder
		reservation.RecoveredExpiresAt = recovered.ExpiresAt
		_, recoveredRun, _, ok := parseProofResourceHolder(recovered.Holder)
		if ok {
			reservation.RecoveredRunAttemptID = recoveredRun
		}
	}
	if lease.PriorHolder != "" {
		_, priorRun, _, ok := parseProofResourceHolder(lease.PriorHolder)
		if ok {
			reservation.PriorRunAttemptID = priorRun
		}
	}
}

func (s *Store) prepareProofRunAttempt(w envelope.WorkEnvelope, actionID string, in ProofResourceReserveInput, now time.Time) (runattempt.RunAttempt, error) {
	store := s.runAttempts()
	ra, err := store.Mint(w.ID, in.Actor, proofResourceHarness, now)
	if err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("proof resource mint RunAttempt: %w", err)
	}
	mintedID := ra.ID
	fail := func(step string, cause error) (runattempt.RunAttempt, error) {
		terminalErr := s.completeProofAttempt(mintedID, runattempt.StatusFailed, "proof resource "+step+" failed: "+cause.Error(), now)
		failedRun, getErr := store.Get(mintedID)
		return failedRun, errors.Join(fmt.Errorf("proof resource %s: %w", step, cause), terminalErr, getErr)
	}
	if ra, err = store.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		return fail("stamp RunAttempt origin", err)
	}
	if w.IDs.ContextPackID != "" {
		if ra, err = store.SetContextPack(ra.ID, w.IDs.ContextPackID); err != nil {
			return fail("bind RunAttempt context", err)
		}
	}
	if ra, err = store.SetWorkspace(ra.ID, in.Workspace); err != nil {
		return fail("bind RunAttempt workspace", err)
	}
	if ra, err = store.SetCommand(ra.ID, in.Command); err != nil {
		return fail("bind RunAttempt command", err)
	}
	if err = store.AttachActionIntent(ra.ID, actionID); err != nil {
		return fail("bind RunAttempt action", err)
	}
	if len(w.RunAttemptIDs) > 0 {
		if ra, err = store.SetPredecessor(ra.ID, w.RunAttemptIDs[len(w.RunAttemptIDs)-1]); err != nil {
			return fail("bind RunAttempt predecessor", err)
		}
	}
	return ra, nil
}

func (s *Store) failProofResourceAcquisition(
	w *envelope.WorkEnvelope,
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
	reservation ProofResourceReservation,
	proofRun runattempt.RunAttempt,
	cause error,
	now time.Time,
) (ProofResourceReserveResult, error) {
	if proofRun.ID == "" {
		return ProofResourceReserveResult{}, cause
	}
	reservation.Status = ProofResourceFailed
	reservation.RunAttemptID = proofRun.ID
	if reservation.Holder == "" {
		reservation.Holder = proofResourceHolder(reservation.ID, proofRun.ID, reservation.WorkID, reservation.Actor)
	}
	reservation.FailureReason = strings.TrimSpace(cause.Error())
	terminalErr := s.completeProofAttempt(
		proofRun.ID,
		runattempt.StatusFailed,
		"proof resource acquisition failed before execution: "+reservation.FailureReason,
		now,
	)
	reservationExists, existenceErr := s.events.Exists(reservation.ID)
	var persistErr error
	if existenceErr == nil && reservationExists {
		var stored ProofResourceReservation
		if err := s.readProofReservationInto(reservation.ID, &stored); err != nil {
			persistErr = err
		} else if stored.WorkID != reservation.WorkID ||
			stored.Actor != reservation.Actor ||
			stored.RunAttemptID != reservation.RunAttemptID {
			persistErr = fmt.Errorf("persisted proof reservation %s conflicts with failed acquisition", reservation.ID)
		} else {
			persistErr = s.persistProofAcquisitionFailureReachability(w, action, decision, stored, now)
		}
	} else if existenceErr == nil {
		persistErr = s.persistProofReservation(
			w,
			action,
			decision,
			reservation,
			"proof resource acquisition failed before execution",
			now,
		)
	}
	result := ProofResourceReserveResult{
		SchemaVersion: ProofResourceReservationSchemaVersion,
		Reservation:   reservation,
		Work:          *w,
	}
	if terminalErr != nil || existenceErr != nil || persistErr != nil {
		return result, errors.Join(cause, terminalErr, existenceErr, persistErr)
	}
	return result, cause
}

// RefreshProofResource extends an already-governed reservation only while its work lease and exact
// running RunAttempt remain live. The landlease refresh itself refuses resurrection after expiry.
func (s *Store) RefreshProofResource(workID, reservationID, actor string, ttl time.Duration, now time.Time) (ProofResourceReservation, error) {
	var reservation ProofResourceReservation
	err := home.WithAutonomyAdmissionLock(s.home, func() error {
		workID = strings.TrimSpace(workID)
		reservationID = strings.TrimSpace(reservationID)
		actor = strings.TrimSpace(actor)
		if ttl < time.Second {
			return fmt.Errorf("proof resource ttl must be at least one second")
		}
		w, err := s.recoverPendingClaimBeforeMutation(workID)
		if err != nil {
			return err
		}
		if terminalStates[w.State] {
			return fmt.Errorf("work %s is terminal", workID)
		}
		claim, err := s.readLease(workID)
		if err != nil || claim.By != actor {
			return fmt.Errorf("proof refresh requires work lease held by %q", actor)
		}
		if err := s.readProofReservationInto(reservationID, &reservation); err != nil {
			return err
		}
		ra, err := s.activeProofRunAttempt(reservation, workID, actor)
		if err != nil {
			return err
		}
		actionTime, err := proofRefreshActionTime(reservation)
		if err != nil {
			return err
		}
		action := s.proofResourceAction(w, reservation.ID, actor, "refresh", "", "", "", "", actionTime)
		decision, err := s.evaluateProofResourceAction(action, now)
		if err != nil {
			return err
		}
		if !policy.Allowed(decision) {
			return fmt.Errorf("proof refresh policy %s", decision.Outcome)
		}
		preparation := ProofResourceRefreshPreparation{
			SchemaVersion:    ProofResourceRefreshPreparationSchemaVersion,
			ID:               proofRefreshPreparationID(reservation.ID, ra.ID, now),
			Kind:             "work.proof_resource.refresh_preparation",
			CreatedAt:        now.UTC().Format(time.RFC3339Nano),
			WorkID:           workID,
			Actor:            actor,
			ReservationID:    reservation.ID,
			RunAttemptID:     ra.ID,
			Holder:           reservation.Holder,
			WindowOpenedAt:   reservation.AcquiredAt,
			RequestedAt:      now.UTC().Format(time.RFC3339Nano),
			TTLSeconds:       int64(ttl / time.Second),
			ActionIntentID:   action.ID,
			PolicyDecisionID: decision.ID,
		}
		if err := s.persistProofHeartbeatPreparation(&w, action, decision, preparation, now); err != nil {
			return err
		}
		windowOpenedAt := reservation.AcquiredAt
		lease, err := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).Refresh(reservation.Holder, ttl, now)
		if err != nil {
			return err
		}
		proofResourceAfterScarceMutation("refresh")
		if _, err := s.runAttempts().Heartbeat(ra.ID, now); err != nil {
			return err
		}
		reservation.ExpiresAt = lease.ExpiresAt
		reservation.TTLSeconds = lease.TTLSeconds
		refresh := ProofResourceRefresh{
			SchemaVersion:    ProofResourceRefreshSchemaVersion,
			ID:               proofRefreshPreparedOutcomeID(reservation.ID, ra.ID, lease.ExpiresAt, preparation.ID),
			Kind:             "work.proof_resource.refresh",
			CreatedAt:        now.UTC().Format(time.RFC3339Nano),
			WorkID:           workID,
			Actor:            actor,
			ReservationID:    reservation.ID,
			RunAttemptID:     ra.ID,
			Holder:           reservation.Holder,
			WindowOpenedAt:   windowOpenedAt,
			RefreshedAt:      lease.AcquiredAt,
			ExpiresAt:        lease.ExpiresAt,
			TTLSeconds:       lease.TTLSeconds,
			PreparationID:    preparation.ID,
			ActionIntentID:   action.ID,
			PolicyDecisionID: decision.ID,
		}
		return s.writeEventJSON(refresh.ID, &refresh)
	})
	return reservation, err
}

// CompleteProofResource persists the governed result/work/event while the exact LIVE holder is
// still exclusive, terminalizes the proof RunAttempt, and only then releases. An expired, stolen,
// malformed, fabricated, piped, or exit-ambiguous result cannot enter VerificationIDs.
func (s *Store) CompleteProofResource(workID string, in ProofResourceCompleteInput, now time.Time) (ProofResourceCompleteResult, error) {
	return s.completeProofResourceFenced(workID, in, "", now)
}

// completeProofResourceFenced completes an exact retained proof completion under the initial
// expected pending-byte fence. The fence is evaluated inside the SAME lifecycle guard that
// performs the completion, before any mutation, and a fence refusal never terminalizes the
// attempt: it is a refusal, not a failed proof completion. A hash the recovery itself
// generates later (a legitimate retry reseed) is produced after this check and is allowed.
func (s *Store) completeProofResourceFenced(
	workID string,
	in ProofResourceCompleteInput,
	expectedPendingSHA256 string,
	now time.Time,
) (ProofResourceCompleteResult, error) {
	var result ProofResourceCompleteResult
	err := s.withLifecycleMutation(func() error {
		if strings.TrimSpace(expectedPendingSHA256) != "" {
			if err := s.verifyRetainedPendingSHA256(in.ReservationID, expectedPendingSHA256); err != nil {
				return err
			}
		}
		var err error
		workID = strings.TrimSpace(workID)
		result, err = s.completeProofResourceLocked(workID, in, now)
		if err == nil {
			return nil
		}
		return errors.Join(err, s.terminalizeProofCompletionErrorLocked(workID, in, err, now))
	})
	return result, err
}

func (s *Store) terminalizeProofCompletionErrorLocked(
	workID string,
	in ProofResourceCompleteInput,
	cause error,
	now time.Time,
) error {
	if workID == "" || strings.TrimSpace(in.Actor) == "" || strings.TrimSpace(in.ReservationID) == "" {
		return nil
	}
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(strings.TrimSpace(in.ReservationID), &reservation); err != nil {
		return nil
	}
	if reservation.WorkID != workID || reservation.Actor != strings.TrimSpace(in.Actor) {
		return nil
	}
	runID := reservation.RunAttemptID
	if pending, present, _, err := s.readProofResourceCompletionPending(reservation.ID); err == nil && present && pending.Result.RunAttemptID != "" {
		runID = pending.Result.RunAttemptID
	}
	if runID == "" {
		return nil
	}
	ra, err := s.runAttempts().Get(runID)
	if err != nil || runattempt.IsTerminal(ra.Status) {
		return err
	}
	reason := "proof completion returned before authoritative commit: " + strings.TrimSpace(cause.Error())
	if err := s.completeProofAttempt(runID, runattempt.StatusFailed, reason, now); err != nil {
		return err
	}
	state := ""
	if w, err := s.Get(workID); err == nil {
		state = w.State
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   workID,
		From:     state,
		To:       state,
		By:       reservation.Actor,
		Note:     reason,
		Attached: []string{reservation.ID, runID},
	})
}

func (s *Store) completeProofResourceLocked(workID string, in ProofResourceCompleteInput, now time.Time) (ProofResourceCompleteResult, error) {
	in.Actor = strings.TrimSpace(in.Actor)
	in.ReservationID = strings.TrimSpace(in.ReservationID)
	in.ReportArtifactRef = strings.TrimSpace(in.ReportArtifactRef)
	in.FailureReason = strings.TrimSpace(in.FailureReason)
	switch {
	case workID == "" || in.Actor == "" || in.ReservationID == "":
		return ProofResourceCompleteResult{}, fmt.Errorf("proof completion requires work_id, actor, and reservation_id")
	case in.ReportArtifactRef == "" && in.FailureReason == "":
		return ProofResourceCompleteResult{}, fmt.Errorf("proof completion requires report artifact or failure reason")
	case in.ReportArtifactRef != "" && in.FailureReason != "":
		return ProofResourceCompleteResult{}, fmt.Errorf("proof completion report artifact and failure reason are mutually exclusive")
	}
	w, err := s.recoverPendingClaimBeforeMutation(workID)
	if err != nil {
		return ProofResourceCompleteResult{}, err
	}
	if err := s.requireProofWorkLeaseOwner(workID, in.Actor, "proof completion"); err != nil {
		return ProofResourceCompleteResult{}, err
	}
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(in.ReservationID, &reservation); err != nil {
		return ProofResourceCompleteResult{}, err
	}
	reservation, err = s.proofReservationWithLatestRefresh(reservation)
	if err != nil {
		return ProofResourceCompleteResult{}, err
	}
	if err := s.requireProofResourceHolder(reservation, now); err != nil {
		return ProofResourceCompleteResult{}, err
	}

	expect, err := s.deriveProofResourceCompletionExpectation(workID, reservation, in)
	if err != nil {
		return ProofResourceCompleteResult{}, err
	}
	resultID := idFor("pfrs_", workID, reservation.ID, in.ReportArtifactRef, in.FailureReason)
	status := expect.ResultStatus
	reportStatus := expect.ReportStatus
	ownedReport := expect.OwnedReport
	verificationRef := expect.VerificationRef
	terminalStatus := expect.TerminalStatus
	inputDigest := expect.InputDigest
	pending, pendingPresent, pendingPath, err := s.readProofResourceCompletionPending(reservation.ID)
	if err != nil {
		return ProofResourceCompleteResult{}, err
	}
	var ra runattempt.RunAttempt
	if pendingPresent {
		if pending.WorkID != workID || pending.ReservationID != reservation.ID || pending.InputDigest != inputDigest {
			return ProofResourceCompleteResult{}, fmt.Errorf("proof completion pending transaction conflicts with requested input")
		}
		ra, pending, err = s.proofRunAttemptForCompletionRetry(
			&w,
			reservation,
			pending,
			pendingPath,
			in,
			expect,
			now,
		)
		if err != nil {
			return ProofResourceCompleteResult{}, err
		}
	} else {
		if status == ProofResourceVerified {
			if err := s.validatePublicRedactionProfileAuthority(workID, in.Actor, reservation.Profile, now); err != nil {
				return ProofResourceCompleteResult{}, fmt.Errorf("public redaction review finalization: %w", err)
			}
		}
		ra, err = s.activeProofRunAttempt(reservation, workID, in.Actor)
		if err != nil {
			return ProofResourceCompleteResult{}, err
		}
		action, decision, governanceSeal, err := s.proofResourceCompletionGovernance(
			w,
			reservation,
			in.Actor,
			now,
		)
		if err != nil {
			return ProofResourceCompleteResult{}, err
		}
		if !policy.Allowed(decision) {
			return ProofResourceCompleteResult{}, fmt.Errorf("proof completion policy %s", decision.Outcome)
		}
		completionID := idFor("pfrc_", workID, reservation.ID, resultID)
		proofResult := ProofResourceResult{
			SchemaVersion:     ProofResourceResultSchemaVersion,
			ID:                resultID,
			Kind:              "work.proof_resource.result",
			Status:            status,
			CreatedAt:         now.UTC().Format(time.RFC3339Nano),
			WorkID:            workID,
			Actor:             in.Actor,
			Resource:          ProofResourceName,
			ReservationID:     reservation.ID,
			RunAttemptID:      ra.ID,
			RunAttemptStatus:  terminalStatus,
			ReportArtifactRef: in.ReportArtifactRef,
			ReportStatus:      reportStatus,
			OwnedReport:       ownedReport,
			SourceSnapshot:    expect.SourceSnapshot,
			ProducerExitKnown: in.ProducerExitKnown,
			ProducerExitCode:  in.ProducerExitCode,
			FailureReason:     in.FailureReason,
			VerificationRef:   verificationRef,
			CompletionID:      completionID,
			ActionIntentID:    action.ID,
			PolicyDecisionID:  decision.ID,
		}
		completion := ProofResourceCompletion{
			SchemaVersion:    ProofResourceCompletionSchemaVersion,
			ID:               completionID,
			Kind:             "work.proof_resource.completion",
			CreatedAt:        now.UTC().Format(time.RFC3339Nano),
			WorkID:           workID,
			Actor:            in.Actor,
			ReservationID:    reservation.ID,
			ResultID:         proofResult.ID,
			RunAttemptID:     ra.ID,
			RunAttemptStatus: terminalStatus,
			VerificationRef:  verificationRef,
			ActionIntentID:   action.ID,
			PolicyDecisionID: decision.ID,
		}
		proofRefs := []string{reservation.ID, proofResult.ID, completion.ID}
		if in.ReportArtifactRef != "" {
			proofRefs = append(proofRefs, in.ReportArtifactRef)
		}
		if verificationRef != "" {
			proofRefs = append(proofRefs, verificationRef)
		}
		pending = proofResourceCompletionPending{
			SchemaVersion:   proofResourceCompletionPendingSchemaVersion,
			WorkID:          workID,
			ReservationID:   reservation.ID,
			InputDigest:     inputDigest,
			GovernanceSeal:  governanceSeal,
			CreatedAt:       now.UTC().Format(time.RFC3339Nano),
			Action:          action,
			Decision:        decision,
			Result:          proofResult,
			Completion:      completion,
			VerificationRef: verificationRef,
			TerminalStatus:  terminalStatus,
			TerminalReason:  completionReason(status, in.FailureReason),
			ProofRefs:       proofRefs,
			LifecycleEvent: Event{
				TS:       ts(now),
				WorkID:   workID,
				From:     w.State,
				To:       w.State,
				By:       in.Actor,
				Note:     "proof resource finalizer committed; verification publishes after terminal RunAttempt",
				Attached: append([]string{action.ID, decision.ID, reservation.ID, proofResult.ID, completion.ID, ra.ID}, proofRefs[3:]...),
			},
		}
		if err := s.writeProofResourceCompletionPending(pendingPath, pending); err != nil {
			return ProofResourceCompleteResult{}, err
		}
	}
	if err := s.validatePrivateSourceCompletionAuthority(workID, in.Actor, reservation.Profile, pending.Result, now); err != nil {
		return ProofResourceCompleteResult{}, err
	}

	finalWork := w
	var terminal runattempt.RunAttempt
	released, releaseErr := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).ReleaseAfter(reservation.Holder, now, func() error {
		// ReleaseAfter checks the inherited execution barrier under the same
		// resource lock before entering this finalizer.
		if err := s.persistPreparedProofCompletion(&finalWork, pending); err != nil {
			return err
		}
		var err error
		terminal, err = s.terminalizePreparedProofCompletion(ra, pending, now)
		if err != nil {
			return err
		}
		if err := proofResourceWriteEventRecord(s, "completion", pending.Completion.ID, &pending.Completion); err != nil {
			return err
		}
		if pending.VerificationRef != "" && !oneOf(pending.VerificationRef, finalWork.VerificationIDs) {
			finalWork.VerificationIDs = append(finalWork.VerificationIDs, pending.VerificationRef)
		}
		if err := proofResourceWriteWorkRecord(s, "work_final", &finalWork); err != nil {
			return err
		}
		return nil
	})
	if releaseErr != nil {
		committedWork, committed := s.visibleProofCompletionCommit(workID, pending, terminal)
		if !committed {
			return ProofResourceCompleteResult{}, fmt.Errorf("proof completion retained exclusive holder: %w", releaseErr)
		}
		finalWork = committedWork
		finalResult := pending.Result
		finalResult.RunAttemptStatus = terminal.Status
		finalResult.VerificationRef = pending.VerificationRef
		return ProofResourceCompleteResult{
			SchemaVersion:           ProofResourceResultSchemaVersion,
			Result:                  finalResult,
			VerificationRef:         pending.VerificationRef,
			ResourceReleased:        false,
			RecoveryJournalRetained: true,
			ReleaseWarning:          releaseErr.Error(),
			Work:                    finalWork,
		}, &ProofResourceReleasePendingError{
			ReservationID: reservation.ID,
			Holder:        reservation.Holder,
			Cause:         releaseErr,
		}
	}
	if released.Freed {
		s.observeProofRelease(reservation.ID, time.Now())
	}
	cleanupErr := s.clearProofResourceCompletionPending(pendingPath, pending)
	finalResult := pending.Result
	finalResult.RunAttemptStatus = terminal.Status
	finalResult.VerificationRef = pending.VerificationRef
	result := ProofResourceCompleteResult{
		SchemaVersion:    ProofResourceResultSchemaVersion,
		Result:           finalResult,
		VerificationRef:  pending.VerificationRef,
		ResourceReleased: released.Freed,
		Work:             finalWork,
	}
	if cleanupErr != nil {
		result.RecoveryJournalRetained = true
		result.ReleaseWarning = "proof completion committed; recovery journal cleanup deferred: " + cleanupErr.Error()
	}
	return result, nil
}

func (s *Store) visibleProofCompletionCommit(
	workID string,
	pending proofResourceCompletionPending,
	terminal runattempt.RunAttempt,
) (envelope.WorkEnvelope, bool) {
	if terminal.ID == "" || terminal.Status != pending.TerminalStatus {
		return envelope.WorkEnvelope{}, false
	}
	w, err := s.Get(workID)
	if err != nil ||
		!oneOf(pending.Action.ID, w.ActionIntentIDs) ||
		w.PolicyDecisionID != pending.Decision.ID {
		return envelope.WorkEnvelope{}, false
	}
	var result ProofResourceResult
	if err := s.readEventJSON(pending.Result.ID, &result); err != nil ||
		result.WorkID != workID ||
		result.ReservationID != pending.ReservationID ||
		result.RunAttemptID != terminal.ID ||
		result.RunAttemptStatus != pending.TerminalStatus ||
		result.VerificationRef != pending.VerificationRef ||
		result.CompletionID != pending.Completion.ID ||
		result.ActionIntentID != pending.Action.ID ||
		result.PolicyDecisionID != pending.Decision.ID ||
		(result.Status == ProofResourceVerified) != (pending.VerificationRef != "") {
		return envelope.WorkEnvelope{}, false
	}
	var completion ProofResourceCompletion
	if err := s.readEventJSON(pending.Completion.ID, &completion); err != nil ||
		completion.WorkID != workID ||
		completion.ReservationID != pending.ReservationID ||
		completion.ResultID != pending.Result.ID ||
		completion.VerificationRef != pending.VerificationRef ||
		completion.RunAttemptID != terminal.ID ||
		completion.RunAttemptStatus != pending.TerminalStatus ||
		completion.ActionIntentID != pending.Action.ID ||
		completion.PolicyDecisionID != pending.Decision.ID {
		return envelope.WorkEnvelope{}, false
	}
	if pending.VerificationRef != "" && !oneOf(pending.VerificationRef, w.VerificationIDs) {
		return envelope.WorkEnvelope{}, false
	}
	return w, true
}

func (s *Store) validateCommittedProofResourceChain(
	w envelope.WorkEnvelope,
	result ProofResourceResult,
	report ownedProofReport,
) error {
	return s.validateCommittedProofResourceChainStatus(w, result, report, ProofResourceVerified)
}

// committedVerifiedProofRunAttempt distinguishes the proof subprocess outcome
// from the Work outcome. A successful canonical battery proves that exact
// proof-check execution succeeded; it does not by itself mean the governed
// task later closed completed. The exception is accepted only through the
// complete committed proof-resource finalizer chain, never from harness name,
// status, or caller-authored refs alone.
func (s *Store) committedVerifiedProofRunAttempt(w envelope.WorkEnvelope, attempt runattempt.RunAttempt) (bool, error) {
	if attempt.Status != runattempt.StatusSucceeded {
		return false, nil
	}
	for _, ref := range w.VerificationIDs {
		ref = strings.TrimSpace(ref)
		if !strings.HasPrefix(ref, "proof-check:pfrs_") {
			continue
		}
		resultID := strings.TrimPrefix(ref, "proof-check:")
		var result ProofResourceResult
		if err := s.readEventJSON(resultID, &result); err != nil {
			return false, fmt.Errorf("proof verification %s result: %w", ref, err)
		}
		if result.RunAttemptID != attempt.ID {
			continue
		}
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			return false, err
		}
		body, err := artifacts.Get(result.ReportArtifactRef)
		if err != nil {
			return false, fmt.Errorf("proof verification %s report artifact: %w", ref, err)
		}
		report, err := decodeOwnedProofReport(body)
		if err != nil {
			return false, fmt.Errorf("proof verification %s report: %w", ref, err)
		}
		if err := s.validateCommittedProofResourceChain(w, result, report); err != nil {
			return false, fmt.Errorf("proof verification %s finalizer chain: %w", ref, err)
		}
		return true, nil
	}
	return false, nil
}

func (s *Store) validateCommittedFailedProofResourceChain(
	w envelope.WorkEnvelope,
	result ProofResourceResult,
	report ownedProofReport,
) error {
	return s.validateCommittedProofResourceChainStatus(w, result, report, ProofResourceFailed)
}

func (s *Store) validateCommittedProofResourceChainStatus(
	w envelope.WorkEnvelope,
	result ProofResourceResult,
	report ownedProofReport,
	expectedStatus string,
) error {
	if expectedStatus != ProofResourceVerified && expectedStatus != ProofResourceFailed {
		return fmt.Errorf("unsupported committed proof result status %q", expectedStatus)
	}
	verified := expectedStatus == ProofResourceVerified
	wantRunStatus := runattempt.StatusFailed
	wantReportStatus := "failed"
	wantVerificationRef := ""
	if verified {
		wantRunStatus = runattempt.StatusSucceeded
		wantReportStatus = "ok"
		wantVerificationRef = "proof-check:" + result.ID
	}
	if result.SchemaVersion != ProofResourceResultSchemaVersion ||
		result.Kind != "work.proof_resource.result" ||
		result.Status != expectedStatus ||
		result.WorkID != w.ID ||
		result.Actor == "" ||
		result.ReservationID == "" ||
		result.RunAttemptID == "" ||
		result.RunAttemptStatus != wantRunStatus ||
		result.ReportArtifactRef == "" ||
		result.ReportStatus != wantReportStatus ||
		!result.OwnedReport ||
		!result.ProducerExitKnown ||
		(verified && result.ProducerExitCode != 0) ||
		(!verified && result.ProducerExitCode == 0) ||
		result.FailureReason != "" ||
		result.VerificationRef != wantVerificationRef ||
		result.CompletionID == "" ||
		result.ActionIntentID == "" ||
		result.PolicyDecisionID == "" {
		return fmt.Errorf("result identity or terminal projection is incomplete for status %s", expectedStatus)
	}
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(result.ReservationID, &reservation); err != nil {
		return fmt.Errorf("reservation: %w", err)
	}
	if !proofReservationSchemaValid(reservation) ||
		reservation.ID != result.ReservationID ||
		reservation.Kind != "work.proof_resource.reservation" ||
		reservation.Status != ProofResourceAcquired ||
		reservation.WorkID != w.ID ||
		reservation.Actor != result.Actor ||
		reservation.Resource != ProofResourceName ||
		reservation.RunAttemptID == "" ||
		reservation.Workspace == "" ||
		reservation.Command == "" ||
		reservation.ActionIntentID == "" ||
		reservation.PolicyDecisionID == "" {
		return fmt.Errorf("reservation identity does not bind result")
	}
	if err := s.validateRemoteProofArtifacts(report); err != nil {
		return err
	}
	if err := validateOwnedProofReportAgainstReservation(report, reservation); err != nil {
		return fmt.Errorf("report/reservation: %w", err)
	}
	if err := s.validateHomeFixtureReport(w.ID, report); err != nil {
		return fmt.Errorf("home fixture proof: %w", err)
	}
	if reservation.Profile.SourceSnapshot == nil ||
		result.SourceSnapshot == nil ||
		!reflect.DeepEqual(reservation.Profile.SourceSnapshot, result.SourceSnapshot) {
		return fmt.Errorf("reservation source snapshot does not bind result")
	}
	retry := result.RunAttemptID != reservation.RunAttemptID
	wantResultID := idFor("pfrs_", w.ID, reservation.ID, result.ReportArtifactRef, "")
	if retry {
		wantResultID = idFor(
			"pfrs_",
			w.ID,
			reservation.ID,
			result.ReportArtifactRef,
			"",
			result.RunAttemptID,
		)
	}
	if result.ID != wantResultID {
		return fmt.Errorf("result id %s does not match committed proof inputs", result.ID)
	}
	wantCompletionID := idFor("pfrc_", w.ID, reservation.ID, result.ID)
	if result.CompletionID != wantCompletionID {
		return fmt.Errorf("completion id %s does not match result %s", result.CompletionID, result.ID)
	}

	var completion ProofResourceCompletion
	if err := s.readEventJSON(result.CompletionID, &completion); err != nil {
		return fmt.Errorf("completion marker: %w", err)
	}
	if completion.SchemaVersion != ProofResourceCompletionSchemaVersion ||
		completion.ID != result.CompletionID ||
		completion.Kind != "work.proof_resource.completion" ||
		completion.WorkID != w.ID ||
		completion.Actor != result.Actor ||
		completion.ReservationID != reservation.ID ||
		completion.ResultID != result.ID ||
		completion.RunAttemptID != result.RunAttemptID ||
		completion.RunAttemptStatus != wantRunStatus ||
		completion.VerificationRef != result.VerificationRef ||
		completion.ActionIntentID != result.ActionIntentID ||
		completion.PolicyDecisionID != result.PolicyDecisionID {
		return fmt.Errorf("completion marker does not commit result")
	}

	attempt, err := s.runAttempts().Get(result.RunAttemptID)
	if err != nil {
		return fmt.Errorf("terminal RunAttempt: %w", err)
	}
	wantProofRefs := []string{
		reservation.ID,
		result.ID,
		completion.ID,
		result.ReportArtifactRef,
	}
	if result.VerificationRef != "" {
		wantProofRefs = append(wantProofRefs, result.VerificationRef)
	}
	wantAttemptActionID := reservation.ActionIntentID
	if retry {
		wantAttemptActionID = result.ActionIntentID
	}
	if attempt.WorkID != w.ID ||
		attempt.Actor != result.Actor ||
		attempt.Harness != proofResourceHarness ||
		attempt.Origin != runattempt.OriginHarnessExecution ||
		attempt.ActionIntentID != wantAttemptActionID ||
		attempt.Workspace != reservation.Workspace ||
		attempt.Command != reservation.Command ||
		attempt.Status != wantRunStatus ||
		attempt.CompletedAt.IsZero() ||
		attempt.SweepReason != completionReason(expectedStatus, "") ||
		!reflect.DeepEqual(attempt.ProofRefs, wantProofRefs) {
		return fmt.Errorf("RunAttempt is not the terminal %s finalizer commitment", expectedStatus)
	}
	if err := s.validateProofRunAttemptLineage(w, reservation, result, attempt); err != nil {
		return err
	}
	expectedReservationAction, err := proofReservationExpectedAction(reservation)
	if err != nil {
		return fmt.Errorf("reservation governance state: %w", err)
	}
	if err := s.validateStoredProofGovernance(
		w,
		result.Actor,
		reservation.ActionIntentID,
		reservation.PolicyDecisionID,
		expectedReservationAction,
	); err != nil {
		return fmt.Errorf("reservation governance: %w", err)
	}
	if err := s.validateStoredProofGovernance(
		w,
		result.Actor,
		result.ActionIntentID,
		result.PolicyDecisionID,
		"complete_full_proof_resource",
	); err != nil {
		return fmt.Errorf("completion governance: %w", err)
	}
	return nil
}

// proofRunAttemptAncestry authenticates the immutable retry ancestry of a terminal proof
// RunAttempt and returns the chain of attempts from the terminal attempt back to the reserved
// original attempt.
//
// The walk is IDENTITY-only: every attempt must be the reserved same-work execution, a retry
// must be governed by the proof completion action, and its predecessor must be terminal. Work
// REACHABILITY is a separate native fact and is checked by the caller, because an interrupted
// retry may have been minted and journaled before its forward link into the Work envelope was
// persisted — the existing native reachability recovery owns that repair.
func (s *Store) proofRunAttemptAncestry(
	w envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	result ProofResourceResult,
	terminal runattempt.RunAttempt,
) ([]runattempt.RunAttempt, error) {
	seen := map[string]bool{}
	chain := []runattempt.RunAttempt{}
	current := terminal
	for {
		if current.ID == "" || seen[current.ID] {
			return nil, fmt.Errorf("RunAttempt retry lineage is empty or cyclic")
		}
		seen[current.ID] = true
		chain = append(chain, current)
		if current.WorkID != w.ID ||
			current.Actor != result.Actor ||
			current.Harness != proofResourceHarness ||
			current.Origin != runattempt.OriginHarnessExecution ||
			current.Workspace != reservation.Workspace ||
			current.Command != reservation.Command {
			return nil, fmt.Errorf("RunAttempt retry lineage leaves reserved proof execution")
		}
		if current.ID == reservation.RunAttemptID {
			if current.ActionIntentID != reservation.ActionIntentID {
				return nil, fmt.Errorf("original proof RunAttempt governance identity changed")
			}
			return chain, nil
		}
		if current.ActionIntentID != result.ActionIntentID ||
			current.PredecessorID == "" {
			return nil, fmt.Errorf("retry RunAttempt is not governed by proof completion action")
		}
		predecessor, err := s.runAttempts().Get(current.PredecessorID)
		if err != nil {
			return nil, fmt.Errorf("proof retry predecessor %s: %w", current.PredecessorID, err)
		}
		if !runattempt.IsTerminal(predecessor.Status) ||
			predecessor.CompletedAt.IsZero() {
			return nil, fmt.Errorf("proof retry predecessor %s is not terminal", predecessor.ID)
		}
		current = predecessor
	}
}

// validateProofRunAttemptLineage authenticates the FULL retry ancestry of a terminal proof
// RunAttempt and requires every attempt in it to be reachable from the Work envelope.
func (s *Store) validateProofRunAttemptLineage(
	w envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	result ProofResourceResult,
	terminal runattempt.RunAttempt,
) error {
	chain, err := s.proofRunAttemptAncestry(w, reservation, result, terminal)
	if err != nil {
		return err
	}
	for _, attempt := range chain {
		if !oneOf(attempt.ID, w.RunAttemptIDs) {
			return fmt.Errorf("RunAttempt retry lineage leaves reserved proof execution")
		}
	}
	return nil
}

// proofReservationExpectedAction derives one exact governance action from the immutable
// reservation state. Takeover actions are not aliases for reserve: every field written by the
// corresponding scarce-lease transfer must be present and internally consistent, while partial
// takeover state fails closed instead of broadening the accepted ActionIntent set.
func proofReservationExpectedAction(reservation ProofResourceReservation) (string, error) {
	priorHolder := strings.TrimSpace(reservation.PriorHolder)
	priorRunAttemptID := strings.TrimSpace(reservation.PriorRunAttemptID)
	stealReason := strings.TrimSpace(reservation.StealReason)
	stolenAt := strings.TrimSpace(reservation.StolenAt)
	terminatedRunAttemptID := strings.TrimSpace(reservation.TerminatedRunAttemptID)
	terminationEvidenceRef := strings.TrimSpace(reservation.TerminationEvidenceRef)
	terminationConfirmedAt := strings.TrimSpace(reservation.TerminationConfirmedAt)
	terminationConfirmationID := strings.TrimSpace(reservation.TerminationConfirmationID)
	if priorHolder != reservation.PriorHolder ||
		priorRunAttemptID != reservation.PriorRunAttemptID ||
		stealReason != reservation.StealReason ||
		stolenAt != reservation.StolenAt ||
		terminatedRunAttemptID != reservation.TerminatedRunAttemptID ||
		terminationEvidenceRef != reservation.TerminationEvidenceRef ||
		terminationConfirmedAt != reservation.TerminationConfirmedAt ||
		terminationConfirmationID != reservation.TerminationConfirmationID {
		return "", fmt.Errorf("proof reservation %s takeover fields are not canonical", reservation.ID)
	}
	if _, err := proofReservationPredecessorHolders(reservation); err != nil {
		return "", err
	}

	hasStealState := priorHolder != "" || priorRunAttemptID != "" || stealReason != "" || stolenAt != ""
	hasTerminationState := terminatedRunAttemptID != "" ||
		terminationEvidenceRef != "" ||
		terminationConfirmedAt != "" ||
		terminationConfirmationID != ""
	if hasTerminationState {
		if priorHolder == "" ||
			priorRunAttemptID == "" ||
			stealReason == "" ||
			stolenAt == "" ||
			terminatedRunAttemptID == "" ||
			terminationEvidenceRef == "" ||
			terminationConfirmedAt == "" ||
			terminationConfirmationID == "" {
			return "", fmt.Errorf("termination-evidenced proof reservation %s is incomplete", reservation.ID)
		}
		if !runattempt.Valid(terminatedRunAttemptID) {
			return "", fmt.Errorf("termination-evidenced proof reservation %s has invalid terminated RunAttempt %q", reservation.ID, terminatedRunAttemptID)
		}
		if !strings.HasPrefix(terminationEvidenceRef, "artifact:sha256:") ||
			!strings.HasPrefix(terminationConfirmationID, "pfrt_") {
			return "", fmt.Errorf("termination-evidenced proof reservation %s has invalid evidence identity", reservation.ID)
		}
		if _, err := time.Parse(time.RFC3339Nano, terminationConfirmedAt); err != nil {
			return "", fmt.Errorf("termination-evidenced proof reservation %s confirmed_at: %w", reservation.ID, err)
		}
		if err := validateProofReservationStealState(reservation, priorHolder, priorRunAttemptID, stolenAt); err != nil {
			return "", err
		}
		return proofResourceActionStealAfterTermination, nil
	}
	if hasStealState {
		if priorHolder == "" || priorRunAttemptID == "" || stealReason == "" || stolenAt == "" {
			return "", fmt.Errorf("ordinary proof-steal reservation %s is incomplete", reservation.ID)
		}
		if err := validateProofReservationStealState(reservation, priorHolder, priorRunAttemptID, stolenAt); err != nil {
			return "", err
		}
		return proofResourceActionSteal, nil
	}
	return proofResourceActionReserve, nil
}

func validateProofReservationStealState(
	reservation ProofResourceReservation,
	priorHolder, priorRunAttemptID, stolenAt string,
) error {
	_, holderRunAttemptID, _, ok := parseProofResourceHolder(priorHolder)
	if !ok || holderRunAttemptID != priorRunAttemptID || !runattempt.Valid(priorRunAttemptID) {
		return fmt.Errorf("proof reservation %s prior holder does not bind RunAttempt %q", reservation.ID, priorRunAttemptID)
	}
	if _, err := time.Parse(time.RFC3339, stolenAt); err != nil {
		return fmt.Errorf("proof reservation %s stolen_at: %w", reservation.ID, err)
	}
	return nil
}

func (s *Store) validateStoredProofGovernance(
	w envelope.WorkEnvelope,
	actor, actionID, decisionID, expectedAction string,
) error {
	var action envelope.ActionIntent
	if err := s.readEventJSON(actionID, &action); err != nil {
		return fmt.Errorf("action %s: %w", actionID, err)
	}
	if err := action.Validate(); err != nil {
		return fmt.Errorf("action %s: %w", actionID, err)
	}
	if action.ID != actionID ||
		action.IDs.WorkID != w.ID ||
		action.Actor != actor ||
		action.Capability != "proof.resource" ||
		action.Action != expectedAction ||
		action.Target.Kind != "work" ||
		action.Target.ID != w.ID ||
		filepath.Clean(action.Target.Path) != filepath.Clean(s.workPath(w.ID)) ||
		!oneOf(action.ID, w.ActionIntentIDs) {
		return fmt.Errorf("action %s identity does not bind proof work", actionID)
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(decisionID, &decision); err != nil {
		return fmt.Errorf("decision %s: %w", decisionID, err)
	}
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("decision %s: %w", decisionID, err)
	}
	if decision.ID != decisionID ||
		decision.IDs.WorkID != w.ID ||
		decision.ActionIntentID != action.ID ||
		!policy.Allowed(decision) {
		return fmt.Errorf("decision %s does not allow action %s", decisionID, actionID)
	}
	return nil
}

func proofResourceCompletionInputDigest(workID string, in ProofResourceCompleteInput) (string, error) {
	body, err := json.Marshal(struct {
		WorkID string                     `json:"work_id"`
		Input  ProofResourceCompleteInput `json:"input"`
	}{WorkID: workID, Input: in})
	if err != nil {
		return "", fmt.Errorf("marshal proof completion input: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func proofResourceCompletionGovernanceSeal(
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
) (string, error) {
	body, err := json.Marshal(struct {
		Action   envelope.ActionIntent   `json:"action"`
		Decision envelope.PolicyDecision `json:"decision"`
	}{Action: action, Decision: decision})
	if err != nil {
		return "", fmt.Errorf("marshal proof completion governance: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) proofResourceCompletionGovernance(
	w envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	actor string,
	now time.Time,
) (envelope.ActionIntent, envelope.PolicyDecision, string, error) {
	action := s.proofResourceAction(w, reservation.ID, actor, "complete", "", "", "", "", now)
	decision, err := s.evaluateProofResourceAction(action, now)
	if err != nil {
		return envelope.ActionIntent{}, envelope.PolicyDecision{}, "", err
	}
	seal, err := proofResourceCompletionGovernanceSeal(action, decision)
	if err != nil {
		return envelope.ActionIntent{}, envelope.PolicyDecision{}, "", err
	}
	return action, decision, seal, nil
}

func (s *Store) proofResourceCompletionPendingPath(reservationID string) (string, error) {
	if filepath.Base(reservationID) != reservationID || !strings.HasPrefix(reservationID, "pfr_") {
		return "", fmt.Errorf("invalid proof reservation id %q", reservationID)
	}
	return filepath.Join(s.home, "work", "proof-resource-completions", reservationID+".json"), nil
}

func (s *Store) readProofResourceCompletionPending(reservationID string) (proofResourceCompletionPending, bool, string, error) {
	var pending proofResourceCompletionPending
	path, err := s.proofResourceCompletionPendingPath(reservationID)
	if err != nil {
		return pending, false, "", err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, path, nil
	}
	if err != nil {
		return pending, false, path, err
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		return pending, false, path, fmt.Errorf("proof completion pending decode: %w", err)
	}
	if pending.SchemaVersion != proofResourceCompletionPendingSchemaVersion ||
		pending.ReservationID != reservationID ||
		pending.WorkID == "" ||
		pending.InputDigest == "" ||
		pending.Action.ID == "" ||
		pending.Decision.ID == "" ||
		pending.Result.ID == "" ||
		pending.Completion.ID == "" ||
		pending.TerminalStatus == "" ||
		pending.LifecycleEvent.WorkID != pending.WorkID {
		return pending, false, path, fmt.Errorf("proof completion pending transaction is incomplete")
	}
	return pending, true, path, nil
}

func (s *Store) writeProofResourceCompletionPending(path string, pending proofResourceCompletionPending) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return s.writeJSON(path, &pending)
}

func (s *Store) clearProofResourceCompletionPending(path string, expected proofResourceCompletionPending) error {
	actual, present, actualPath, err := s.readProofResourceCompletionPending(expected.ReservationID)
	if err != nil {
		return err
	}
	if !present || actualPath != path || !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("proof completion pending transaction changed before commit")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncReceiptedActionDir(filepath.Dir(path))
}

func (s *Store) proofResourceLegacyRecoveryPendingPath(workID string) (string, error) {
	if filepath.Base(workID) != workID || !strings.HasPrefix(workID, "work_") {
		return "", fmt.Errorf("invalid proof legacy recovery work id %q", workID)
	}
	return filepath.Join(s.home, "work", "proof-resource-legacy-recoveries", workID+".json"), nil
}

func (s *Store) readProofResourceLegacyRecoveryPending(
	workID string,
) (proofResourceLegacyRecoveryPending, bool, string, error) {
	var pending proofResourceLegacyRecoveryPending
	path, err := s.proofResourceLegacyRecoveryPendingPath(workID)
	if err != nil {
		return pending, false, "", err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, path, nil
	}
	if err != nil {
		return pending, false, path, err
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		return pending, false, path, fmt.Errorf("proof legacy recovery pending decode: %w", err)
	}
	if pending.SchemaVersion != proofResourceLegacyRecoveryPendingSchemaVersion ||
		pending.ID == "" ||
		pending.WorkID != workID ||
		pending.ReservationID == "" ||
		pending.Holder == "" ||
		pending.RunAttemptID == "" ||
		pending.InputDigest == "" ||
		pending.CompletionDigest == "" ||
		pending.CreatedAt == "" ||
		pending.GovernanceSeal == "" ||
		pending.Action.ID == "" ||
		pending.Decision.ID == "" ||
		pending.Recovery.ID == "" ||
		pending.WorkBefore.ID != workID ||
		pending.WorkAfter.ID != workID ||
		pending.LifecycleEvent.WorkID != workID ||
		pending.FreshBatteryReason == "" ||
		pending.TerminalStatus == "" ||
		pending.TerminalReason == "" {
		return pending, false, path, fmt.Errorf("proof legacy recovery pending transaction is incomplete")
	}
	return pending, true, path, nil
}

func (s *Store) writeProofResourceLegacyRecoveryPending(
	path string,
	pending proofResourceLegacyRecoveryPending,
) error {
	actual, present, actualPath, err := s.readProofResourceLegacyRecoveryPending(pending.WorkID)
	if err != nil {
		return err
	}
	if present {
		if actualPath == path && reflect.DeepEqual(actual, pending) {
			return nil
		}
		return fmt.Errorf("proof legacy recovery pending identity conflict")
	}
	if actualPath != path {
		return fmt.Errorf("proof legacy recovery pending path changed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return s.writeJSON(path, &pending)
}

func (s *Store) clearProofResourceLegacyRecoveryPending(
	path string,
	expected proofResourceLegacyRecoveryPending,
) error {
	actual, present, actualPath, err := s.readProofResourceLegacyRecoveryPending(expected.WorkID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if actualPath != path || !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("proof legacy recovery pending transaction changed before cleanup")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncReceiptedActionDir(filepath.Dir(path))
}

func (s *Store) clearLegacyProofResourceCompletionPending(
	path string,
	expected proofResourceCompletionPending,
) error {
	actual, present, actualPath, err := s.readProofResourceCompletionPending(expected.ReservationID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if actualPath != path || !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("legacy proof completion pending transaction changed before cleanup")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncReceiptedActionDir(filepath.Dir(path))
}

func proofResourceLegacyCompletionDigest(pending proofResourceCompletionPending) (string, error) {
	body, err := json.Marshal(pending)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func proofResourceLegacyRecoverySeal(pending proofResourceLegacyRecoveryPending) (string, error) {
	pending.GovernanceSeal = ""
	body, err := json.Marshal(pending)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) newProofResourceLegacyRecoveryPending(
	w envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	completion proofResourceCompletionPending,
	attempt runattempt.RunAttempt,
	in ProofResourceLegacyRecoveryInput,
	now time.Time,
) (proofResourceLegacyRecoveryPending, error) {
	completionDigest, err := proofResourceLegacyCompletionDigest(completion)
	if err != nil {
		return proofResourceLegacyRecoveryPending{}, err
	}
	action := s.proofResourceAction(
		w,
		reservation.ID,
		in.Actor,
		"legacy_recovery",
		"",
		"",
		"",
		"",
		now,
	)
	decision, err := s.evaluateProofResourceAction(action, now)
	if err != nil {
		return proofResourceLegacyRecoveryPending{}, err
	}
	if !policy.Allowed(decision) {
		return proofResourceLegacyRecoveryPending{}, fmt.Errorf("legacy proof recovery policy %s", decision.Outcome)
	}
	reason := "retained pre-v0.3 proof released without verification; fresh source-bound v0.3 battery required"
	terminalStatus := runattempt.StatusAbandoned
	terminalReason := reason
	if completion.Result.ReportArtifactRef == "" {
		terminalStatus = runattempt.StatusFailed
		terminalReason = strings.TrimSpace(completion.Result.FailureReason)
	}
	if terminalReason == "" {
		return proofResourceLegacyRecoveryPending{}, fmt.Errorf("legacy proof recovery terminal reason is empty")
	}
	workBefore := w
	workBefore.ActionIntentIDs = cloneProofStringSlice(w.ActionIntentIDs)
	workBefore.RunAttemptIDs = cloneProofStringSlice(w.RunAttemptIDs)
	workBefore.VerificationIDs = cloneProofStringSlice(w.VerificationIDs)
	workAfter := workBefore
	workAfter.ActionIntentIDs = cloneProofStringSlice(workBefore.ActionIntentIDs)
	workAfter.RunAttemptIDs = cloneProofStringSlice(workBefore.RunAttemptIDs)
	workAfter.VerificationIDs = cloneProofStringSlice(workBefore.VerificationIDs)
	if !oneOf(action.ID, workAfter.ActionIntentIDs) {
		workAfter.ActionIntentIDs = append(workAfter.ActionIntentIDs, action.ID)
	}
	if !oneOf(attempt.ID, workAfter.RunAttemptIDs) {
		workAfter.RunAttemptIDs = append(workAfter.RunAttemptIDs, attempt.ID)
	}
	workAfter.PolicyDecisionID = decision.ID
	recovery := ProofResourceLegacyRecovery{
		SchemaVersion:        ProofResourceLegacyRecoverySchemaVersion,
		ID:                   idFor("pflr_", w.ID, reservation.ID, completion.Result.ID),
		Kind:                 "work.proof_resource.legacy_recovery",
		Status:               "released_without_verification",
		CreatedAt:            now.UTC().Format(time.RFC3339Nano),
		WorkID:               w.ID,
		Actor:                in.Actor,
		Resource:             ProofResourceName,
		ReservationID:        reservation.ID,
		RunAttemptID:         attempt.ID,
		ReportArtifactRef:    completion.Result.ReportArtifactRef,
		DiscardedProofRef:    completion.VerificationRef,
		FreshBatteryRequired: true,
		ActionIntentID:       action.ID,
		PolicyDecisionID:     decision.ID,
	}
	lifecycle := Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     workAfter.State,
		To:       workAfter.State,
		By:       in.Actor,
		Note:     reason,
		Attached: []string{action.ID, decision.ID, reservation.ID, attempt.ID, recovery.ID},
	}
	pending := proofResourceLegacyRecoveryPending{
		SchemaVersion:      proofResourceLegacyRecoveryPendingSchemaVersion,
		ID:                 idFor("pflj_", w.ID, reservation.ID, completionDigest),
		WorkID:             w.ID,
		ReservationID:      reservation.ID,
		Holder:             reservation.Holder,
		RunAttemptID:       attempt.ID,
		InputDigest:        completion.InputDigest,
		CompletionDigest:   completionDigest,
		CreatedAt:          now.UTC().Format(time.RFC3339Nano),
		Action:             action,
		Decision:           decision,
		Completion:         completion,
		Recovery:           recovery,
		WorkBefore:         workBefore,
		WorkAfter:          workAfter,
		LifecycleEvent:     lifecycle,
		FreshBatteryReason: reason,
		TerminalStatus:     terminalStatus,
		TerminalReason:     terminalReason,
	}
	pending.GovernanceSeal, err = proofResourceLegacyRecoverySeal(pending)
	if err != nil {
		return proofResourceLegacyRecoveryPending{}, err
	}
	return pending, nil
}

func cloneProofStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string{}, in...)
}

func (s *Store) validateProofResourceLegacyRecoveryPending(
	pending proofResourceLegacyRecoveryPending,
	reservation ProofResourceReservation,
	in ProofResourceLegacyRecoveryInput,
) error {
	createdAt, err := time.Parse(time.RFC3339Nano, pending.CreatedAt)
	if err != nil {
		return fmt.Errorf("legacy proof recovery journal created_at: %w", err)
	}
	if !proofReservationSchemaValid(reservation) ||
		reservation.ID != pending.ReservationID ||
		reservation.Kind != "work.proof_resource.reservation" ||
		reservation.Status != ProofResourceAcquired ||
		reservation.WorkID != pending.WorkID ||
		reservation.Actor != in.Actor ||
		reservation.Resource != ProofResourceName ||
		reservation.Holder != pending.Holder ||
		reservation.Workspace != in.Workspace ||
		reservation.Command != in.Command ||
		reservation.Profile.SourceSnapshot != nil {
		return fmt.Errorf("legacy proof recovery journal reservation identity changed")
	}
	completionDigest, err := proofResourceLegacyCompletionDigest(pending.Completion)
	if err != nil {
		return err
	}
	if completionDigest != pending.CompletionDigest ||
		pending.InputDigest != pending.Completion.InputDigest ||
		pending.Completion.WorkID != pending.WorkID ||
		pending.Completion.ReservationID != pending.ReservationID ||
		pending.Completion.Result.RunAttemptID != pending.RunAttemptID {
		return fmt.Errorf("legacy proof recovery journal completion identity changed")
	}
	attempt, err := s.runAttempts().Get(pending.RunAttemptID)
	if err != nil {
		return fmt.Errorf("legacy proof recovery journal RunAttempt: %w", err)
	}
	completeInput := ProofResourceCompleteInput{
		Actor:             in.Actor,
		ReservationID:     reservation.ID,
		ReportArtifactRef: pending.Completion.Result.ReportArtifactRef,
		ProducerExitKnown: pending.Completion.Result.ProducerExitKnown,
		ProducerExitCode:  pending.Completion.Result.ProducerExitCode,
		FailureReason:     pending.Completion.Result.FailureReason,
	}
	expect, err := s.deriveProofResourceCompletionExpectation(pending.WorkID, reservation, completeInput)
	if err != nil {
		return err
	}
	_, migrated, err := s.validateAndNormalizeProofResourceCompletionPending(
		pending.WorkBefore,
		reservation,
		pending.Completion,
		attempt,
		completeInput,
		expect,
	)
	if err != nil {
		return err
	}
	if migrated {
		return fmt.Errorf("legacy proof recovery journal embeds unsealed completion governance")
	}
	if pending.Completion.Result.SourceSnapshot != nil ||
		oneOf(pending.Completion.VerificationRef, pending.WorkBefore.VerificationIDs) {
		return fmt.Errorf("legacy proof recovery journal embeds source-bound or promoted proof")
	}
	if completeInput.ReportArtifactRef == "" {
		if strings.TrimSpace(completeInput.FailureReason) == "" ||
			expect.VerificationRef != "" ||
			expect.ResultStatus != ProofResourceFailed ||
			pending.Completion.Result.Status != ProofResourceFailed ||
			pending.Completion.Result.ReportStatus != "" ||
			pending.Completion.Result.OwnedReport ||
			pending.Completion.Result.VerificationRef != "" ||
			pending.Completion.TerminalStatus != runattempt.StatusFailed {
			return fmt.Errorf("legacy proof recovery journal reportless completion is not unequivocally failed and non-verifying")
		}
	} else {
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			return err
		}
		reportBody, err := artifacts.Get(completeInput.ReportArtifactRef)
		if err != nil {
			return fmt.Errorf("legacy proof recovery journal report artifact: %w", err)
		}
		report, err := decodeOwnedProofReport(reportBody)
		if err != nil {
			return fmt.Errorf("legacy proof recovery journal report decode: %w", err)
		}
		if (report.SchemaVersion != "compound.proof_check_report.v0.1" &&
			report.SchemaVersion != "compound.proof_check_report.v0.2") ||
			report.SourceSnapshot != nil {
			return fmt.Errorf("legacy proof recovery journal report is not retained pre-v0.3 evidence")
		}
	}
	expected, err := s.newProofResourceLegacyRecoveryPending(
		pending.WorkBefore,
		reservation,
		pending.Completion,
		attempt,
		in,
		createdAt,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, pending) {
		return fmt.Errorf("legacy proof recovery journal differs canonical construction")
	}
	seal, err := proofResourceLegacyRecoverySeal(pending)
	if err != nil {
		return err
	}
	if seal != pending.GovernanceSeal {
		return fmt.Errorf("legacy proof recovery governance seal changed")
	}
	if !reflect.DeepEqual(pending.WorkBefore.VerificationIDs, pending.WorkAfter.VerificationIDs) {
		return fmt.Errorf("legacy proof recovery journal changes work verification ids")
	}
	return nil
}

func (s *Store) persistProofResourceLegacyRecovery(
	pending proofResourceLegacyRecoveryPending,
) error {
	for _, item := range []struct {
		stage string
		id    string
		value any
	}{
		{"legacy_recovery_prepare_action", pending.Action.ID, &pending.Action},
		{"legacy_recovery_prepare_decision", pending.Decision.ID, &pending.Decision},
		{"legacy_recovery", pending.Recovery.ID, &pending.Recovery},
	} {
		if err := proofResourceWriteEventRecord(s, item.stage, item.id, item.value); err != nil {
			return fmt.Errorf("persist %s: %w", item.stage, err)
		}
	}
	current, err := s.Get(pending.WorkID)
	if err != nil {
		return err
	}
	normalizedCurrent := current
	if len(normalizedCurrent.VerificationIDs) == 0 {
		normalizedCurrent.VerificationIDs = nil
	}
	switch {
	case reflect.DeepEqual(normalizedCurrent, pending.WorkAfter):
	case reflect.DeepEqual(normalizedCurrent, pending.WorkBefore):
		if err := proofResourceWriteWorkRecord(s, "legacy_recovery_work", &pending.WorkAfter); err != nil {
			return fmt.Errorf("persist legacy recovery work: %w", err)
		}
	default:
		return fmt.Errorf("legacy proof recovery work differs sealed before/after images")
	}
	events, err := s.eventsFor(pending.WorkID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if reflect.DeepEqual(event, pending.LifecycleEvent) {
			return nil
		}
		if event.WorkID == pending.LifecycleEvent.WorkID &&
			event.TS == pending.LifecycleEvent.TS &&
			event.Note == pending.LifecycleEvent.Note {
			return fmt.Errorf("legacy proof recovery lifecycle identity conflict")
		}
	}
	if err := proofResourceAppendLifecycleRecord(s, pending.LifecycleEvent); err != nil {
		return fmt.Errorf("persist legacy recovery lifecycle: %w", err)
	}
	return nil
}

func (s *Store) proofResourceLegacyRecoveryEvidenceExact(
	pending proofResourceLegacyRecoveryPending,
) error {
	for _, item := range []struct {
		name  string
		id    string
		value any
	}{
		{"ActionIntent", pending.Action.ID, &envelope.ActionIntent{}},
		{"PolicyDecision", pending.Decision.ID, &envelope.PolicyDecision{}},
		{"recovery evidence", pending.Recovery.ID, &ProofResourceLegacyRecovery{}},
	} {
		if err := s.readEventJSON(item.id, item.value); err != nil {
			return fmt.Errorf("read legacy proof recovery %s: %w", item.name, err)
		}
		switch value := item.value.(type) {
		case *envelope.ActionIntent:
			if !reflect.DeepEqual(*value, pending.Action) {
				return fmt.Errorf("legacy proof recovery ActionIntent differs sealed journal")
			}
		case *envelope.PolicyDecision:
			if !reflect.DeepEqual(*value, pending.Decision) {
				return fmt.Errorf("legacy proof recovery PolicyDecision differs sealed journal")
			}
		case *ProofResourceLegacyRecovery:
			if !reflect.DeepEqual(*value, pending.Recovery) {
				return fmt.Errorf("legacy proof recovery evidence differs sealed journal")
			}
		}
	}
	current, err := s.Get(pending.WorkID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, pending.WorkAfter) {
		return fmt.Errorf("legacy proof recovery work is not sealed after image")
	}
	events, err := s.eventsFor(pending.WorkID)
	if err != nil {
		return err
	}
	count := 0
	for _, event := range events {
		if reflect.DeepEqual(event, pending.LifecycleEvent) {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("legacy proof recovery lifecycle count = %d, want 1", count)
	}
	attempt, err := s.runAttempts().Get(pending.RunAttemptID)
	if err != nil {
		return err
	}
	if !runattempt.IsTerminal(attempt.Status) {
		return fmt.Errorf("legacy proof recovery RunAttempt %s is not terminal", attempt.ID)
	}
	if pending.Completion.Result.ReportArtifactRef == "" &&
		attempt.Status != runattempt.StatusFailed {
		return fmt.Errorf("reportless legacy proof recovery RunAttempt status = %s, want failed", attempt.Status)
	}
	return nil
}

func (s *Store) resumeLegacyProofResourceRecovery(
	pending proofResourceLegacyRecoveryPending,
	journalPath string,
	in ProofResourceLegacyRecoveryInput,
	now time.Time,
) (ProofResourceLegacyRecoveryResult, error) {
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(pending.ReservationID, &reservation); err != nil {
		return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("read legacy proof recovery reservation: %w", err)
	}
	if err := s.validateProofResourceLegacyRecoveryPending(pending, reservation, in); err != nil {
		return ProofResourceLegacyRecoveryResult{}, err
	}
	claim, err := s.readLease(pending.WorkID)
	if err != nil || claim.By != in.Actor {
		return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("legacy proof recovery requires work lease held by %q", in.Actor)
	}
	completion, completionPresent, completionPath, err := s.readProofResourceCompletionPending(pending.ReservationID)
	if err != nil {
		return ProofResourceLegacyRecoveryResult{}, err
	}
	if completionPresent && !reflect.DeepEqual(completion, pending.Completion) {
		return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("legacy proof recovery completion differs sealed journal")
	}
	status, err := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).Status(now)
	if err != nil {
		return ProofResourceLegacyRecoveryResult{}, err
	}
	exactHolder := status.Holder == pending.Holder
	released := false
	if exactHolder {
		if !completionPresent {
			return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("legacy proof recovery completion was removed before exact holder release")
		}
		if !status.Held {
			if !status.IsExpired {
				return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("legacy proof recovery exact holder is neither held nor expired")
			}
			if _, err := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).RearmExpiredExact(status, now); err != nil {
				return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("rearm exact expired legacy proof holder: %w", err)
			}
			proofResourceAfterScarceMutation("legacy_recovery_rearm")
		}
		attempt, err := s.runAttempts().Get(pending.RunAttemptID)
		if err != nil {
			return ProofResourceLegacyRecoveryResult{}, err
		}
		createdAt, err := time.Parse(time.RFC3339Nano, pending.CreatedAt)
		if err != nil {
			return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("legacy proof recovery created_at: %w", err)
		}
		if !runattempt.IsTerminal(attempt.Status) {
			if err := s.completeProofAttempt(
				attempt.ID,
				pending.TerminalStatus,
				pending.TerminalReason,
				createdAt,
			); err != nil {
				return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("terminalize retained legacy proof RunAttempt: %w", err)
			}
		}
		if _, err := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).AttestExecutionTerminated(pending.Holder); err != nil {
			return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("legacy proof recovery execution termination: %w", err)
		}
		releaseAfter := proofResourceReleaseLegacyRecoveryAfter
		if reservation.ProofCoordination != nil {
			releaseAfter = func(_ string, holder string, at time.Time, finalize func() error) (landlease.ReleaseResult, error) {
				return landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination).ReleaseAfter(holder, at, finalize)
			}
		}
		release, err := releaseAfter(
			s.home,
			pending.Holder,
			now,
			func() error {
				return s.persistProofResourceLegacyRecovery(pending)
			},
		)
		if err != nil {
			return ProofResourceLegacyRecoveryResult{}, err
		}
		if !release.Freed {
			return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("legacy proof recovery did not release exact holder")
		}
		released = true
		if err := proofResourceAfterLegacyRecoveryRelease(pending); err != nil {
			return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("legacy proof recovery post-release boundary: %w", err)
		}
	} else {
		if err := s.proofResourceLegacyRecoveryEvidenceExact(pending); err != nil {
			return ProofResourceLegacyRecoveryResult{}, fmt.Errorf(
				"legacy proof holder disappeared without exact durable recovery evidence: %w",
				err,
			)
		}
		released = true
	}
	if completionPresent {
		if err := proofResourceClearLegacyCompletionPending(s, completionPath, pending.Completion); err != nil {
			return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("clear retained legacy proof completion: %w", err)
		}
	}
	if err := proofResourceClearLegacyRecoveryJournal(s, journalPath, pending); err != nil {
		return ProofResourceLegacyRecoveryResult{}, fmt.Errorf("clear legacy proof recovery journal: %w", err)
	}
	current, err := s.Get(pending.WorkID)
	if err != nil {
		return ProofResourceLegacyRecoveryResult{}, err
	}
	return ProofResourceLegacyRecoveryResult{
		SchemaVersion:        ProofResourceLegacyRecoverySchemaVersion,
		Recovery:             pending.Recovery,
		ResourceReleased:     released,
		FreshBatteryRequired: true,
		Work:                 current,
	}, nil
}

func (s *Store) validateAndNormalizeProofResourceCompletionPending(
	w envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
	ra runattempt.RunAttempt,
	in ProofResourceCompleteInput,
	expect proofResourceCompletionExpectation,
) (proofResourceCompletionPending, bool, error) {
	fail := func(reason string) (proofResourceCompletionPending, bool, error) {
		return pending, false, fmt.Errorf("proof completion pending identity is corrupt: %s", reason)
	}
	workID := w.ID
	if pending.SchemaVersion != proofResourceCompletionPendingSchemaVersion ||
		pending.WorkID != workID ||
		pending.ReservationID != reservation.ID ||
		pending.InputDigest != expect.InputDigest {
		return fail("work, reservation, input, or schema binding changed")
	}
	if reservation.WorkID != workID ||
		reservation.Actor != in.Actor ||
		reservation.Status != ProofResourceAcquired {
		return fail("current reservation is not the acquired same-work actor binding")
	}
	if err := pending.Action.Validate(); err != nil {
		return fail("completion ActionIntent is invalid: " + err.Error())
	}
	if err := pending.Decision.Validate(); err != nil {
		return fail("completion PolicyDecision is invalid: " + err.Error())
	}
	seal, err := proofResourceCompletionGovernanceSeal(pending.Action, pending.Decision)
	if err != nil {
		return fail("completion governance seal cannot be computed: " + err.Error())
	}
	normalized := pending
	governanceMigrated := false
	governanceCreatedAt := pending.CreatedAt
	if ra.ID != reservation.RunAttemptID {
		originalAttempt, err := s.runAttempts().Get(reservation.RunAttemptID)
		if err != nil {
			return fail("completion governance original RunAttempt cannot be read: " + err.Error())
		}
		if originalAttempt.CompletedAt.IsZero() {
			return fail("completion governance original RunAttempt has no completion time")
		}
		governanceCreatedAt = originalAttempt.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, governanceCreatedAt)
	if err != nil {
		return fail("completion governance creation time is invalid: " + err.Error())
	}
	expectedAction := s.proofResourceAction(w, reservation.ID, in.Actor, "complete", "", "", "", "", createdAt)
	if !reflect.DeepEqual(pending.Action, expectedAction) {
		return fail("completion ActionIntent differs from canonical construction")
	}
	if pending.GovernanceSeal == "" {
		expectedAction, expectedDecision, expectedSeal, err := s.proofResourceCompletionGovernance(
			w,
			reservation,
			in.Actor,
			createdAt,
		)
		if err != nil {
			return fail("legacy completion governance cannot be reconstructed: " + err.Error())
		}
		if !reflect.DeepEqual(pending.Action, expectedAction) ||
			!reflect.DeepEqual(pending.Decision, expectedDecision) ||
			seal != expectedSeal {
			return fail("legacy completion governance differs from canonical reconstruction")
		}
		normalized.GovernanceSeal = expectedSeal
		governanceMigrated = true
	} else if pending.GovernanceSeal != seal {
		return fail("completion governance seal does not match the complete ActionIntent and PolicyDecision")
	}
	if !policy.Allowed(pending.Decision) {
		return fail("completion PolicyDecision is not allowing")
	}
	if ra.WorkID != workID ||
		ra.Actor != in.Actor ||
		ra.Harness != proofResourceHarness ||
		ra.Origin != runattempt.OriginHarnessExecution ||
		(ra.ActionIntentID != reservation.ActionIntentID && ra.ActionIntentID != pending.Action.ID) ||
		ra.Workspace != reservation.Workspace ||
		ra.Command != reservation.Command {
		return fail("pending RunAttempt is not the reserved same-work execution")
	}

	expectedResultID := idFor(
		"pfrs_",
		workID,
		reservation.ID,
		in.ReportArtifactRef,
		in.FailureReason,
	)
	retry := ra.ID != reservation.RunAttemptID
	if retry {
		expectedResultID = idFor(
			"pfrs_",
			workID,
			reservation.ID,
			in.ReportArtifactRef,
			in.FailureReason,
			ra.ID,
		)
		if ra.PredecessorID == "" {
			return fail("retry RunAttempt has no predecessor")
		}
	}
	expectedVerificationRef := expect.VerificationRef
	if retry && expectedVerificationRef != "" {
		expectedVerificationRef = "proof-check:" + expectedResultID
	}
	expectedCompletionID := idFor("pfrc_", workID, reservation.ID, expectedResultID)
	if pending.Result.SchemaVersion != ProofResourceResultSchemaVersion ||
		pending.Result.ID != expectedResultID ||
		pending.Result.Kind != "work.proof_resource.result" ||
		pending.Result.WorkID != workID ||
		pending.Result.Actor != in.Actor ||
		pending.Result.Resource != ProofResourceName ||
		pending.Result.ReservationID != reservation.ID ||
		pending.Result.RunAttemptID != ra.ID ||
		pending.Result.ReportArtifactRef != in.ReportArtifactRef ||
		pending.Result.ReportStatus != expect.ReportStatus ||
		pending.Result.OwnedReport != expect.OwnedReport ||
		!reflect.DeepEqual(pending.Result.SourceSnapshot, expect.SourceSnapshot) ||
		pending.Result.ProducerExitKnown != in.ProducerExitKnown ||
		pending.Result.ProducerExitCode != in.ProducerExitCode ||
		pending.Result.FailureReason != in.FailureReason ||
		pending.Result.Status != expect.ResultStatus ||
		pending.Result.CompletionID != expectedCompletionID ||
		pending.Result.ActionIntentID != pending.Action.ID ||
		pending.Result.PolicyDecisionID != pending.Decision.ID {
		return fail("ProofResourceResult immutable fields or cross-object references changed")
	}
	if pending.TerminalStatus != expect.TerminalStatus ||
		pending.TerminalReason != expect.TerminalReason ||
		pending.VerificationRef != expectedVerificationRef ||
		(pending.Result.Status == ProofResourceVerified) != (pending.VerificationRef != "") ||
		(pending.TerminalStatus == runattempt.StatusSucceeded) != (pending.VerificationRef != "") {
		return fail("terminal status, reason, result status, or verification reference is inconsistent")
	}
	if pending.Completion.SchemaVersion != ProofResourceCompletionSchemaVersion ||
		pending.Completion.ID != expectedCompletionID ||
		pending.Completion.Kind != "work.proof_resource.completion" ||
		pending.Completion.WorkID != workID ||
		pending.Completion.Actor != in.Actor ||
		pending.Completion.ReservationID != reservation.ID ||
		pending.Completion.ResultID != pending.Result.ID ||
		pending.Completion.RunAttemptID != ra.ID ||
		pending.Completion.RunAttemptStatus != pending.TerminalStatus ||
		pending.Completion.VerificationRef != pending.VerificationRef ||
		pending.Completion.ActionIntentID != pending.Action.ID ||
		pending.Completion.PolicyDecisionID != pending.Decision.ID {
		return fail("ProofResourceCompletion immutable fields or cross-object references changed")
	}
	if pending.CreatedAt == "" ||
		pending.Result.CreatedAt != pending.CreatedAt ||
		pending.Completion.CreatedAt != pending.CreatedAt {
		return fail("pending, result, and completion creation identity changed")
	}

	expectedProofRefs := []string{reservation.ID, pending.Result.ID, pending.Completion.ID}
	if in.ReportArtifactRef != "" {
		expectedProofRefs = append(expectedProofRefs, in.ReportArtifactRef)
	}
	if pending.VerificationRef != "" {
		expectedProofRefs = append(expectedProofRefs, pending.VerificationRef)
	}
	if !reflect.DeepEqual(pending.ProofRefs, expectedProofRefs) {
		return fail("proof references changed")
	}
	expectedNote := "proof resource finalizer committed; verification publishes after terminal RunAttempt"
	expectedAttached := append(
		[]string{
			pending.Action.ID,
			pending.Decision.ID,
			reservation.ID,
			pending.Result.ID,
			pending.Completion.ID,
			ra.ID,
		},
		expectedProofRefs[3:]...,
	)
	if retry {
		expectedNote = "proof resource finalizer retry committed; verification publishes after terminal RunAttempt"
		expectedAttached = append(
			[]string{
				pending.Action.ID,
				pending.Decision.ID,
				reservation.ID,
				pending.Result.ID,
				pending.Completion.ID,
				ra.PredecessorID,
				ra.ID,
			},
			expectedProofRefs[3:]...,
		)
	}
	if pending.LifecycleEvent.TS == "" ||
		pending.LifecycleEvent.WorkID != workID ||
		pending.LifecycleEvent.From != w.State ||
		pending.LifecycleEvent.To != w.State ||
		pending.LifecycleEvent.By != in.Actor ||
		pending.LifecycleEvent.Note != expectedNote ||
		!reflect.DeepEqual(pending.LifecycleEvent.Attached, expectedAttached) {
		return fail("lifecycle event identity or attachments changed")
	}

	exactResultProjection := pending.Result.RunAttemptStatus == pending.TerminalStatus &&
		pending.Result.VerificationRef == pending.VerificationRef
	legacyResultProjection := (pending.Result.RunAttemptStatus == runattempt.StatusPending ||
		pending.Result.RunAttemptStatus == runattempt.StatusRunning) &&
		pending.Result.VerificationRef == ""
	switch {
	case exactResultProjection:
		return normalized, governanceMigrated, nil
	case legacyResultProjection:
		normalized.Result.RunAttemptStatus = normalized.TerminalStatus
		normalized.Result.VerificationRef = normalized.VerificationRef
		return normalized, true, nil
	default:
		return fail("ProofResourceResult terminal projection is neither exact nor a known v0.1 pending-or-running/empty shape")
	}
}

func (s *Store) proofRunAttemptForCompletionRetry(
	w *envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
	pendingPath string,
	in ProofResourceCompleteInput,
	expect proofResourceCompletionExpectation,
	now time.Time,
) (runattempt.RunAttempt, proofResourceCompletionPending, error) {
	workID := w.ID
	actor := in.Actor
	ra, err := s.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		return runattempt.RunAttempt{}, pending, err
	}
	normalizedPending, migrated, err := s.validateAndNormalizeProofResourceCompletionPending(
		*w,
		reservation,
		pending,
		ra,
		in,
		expect,
	)
	if err != nil {
		return runattempt.RunAttempt{}, pending, err
	}
	if migrated {
		if err := s.writeProofResourceCompletionPending(pendingPath, normalizedPending); err != nil {
			return runattempt.RunAttempt{}, pending, fmt.Errorf("normalize legacy proof completion retry journal: %w", err)
		}
		pending = normalizedPending
	}
	pending, err = s.adoptProofCompletionRetryDescendant(
		w,
		reservation,
		pending,
		pendingPath,
		workID,
		actor,
		now,
	)
	if err != nil {
		return runattempt.RunAttempt{}, pending, err
	}
	ra, err = s.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		return runattempt.RunAttempt{}, pending, err
	}
	if reservation.WorkID != workID ||
		reservation.Actor != actor ||
		reservation.Status != ProofResourceAcquired ||
		ra.WorkID != workID ||
		ra.Actor != actor ||
		ra.Harness != proofResourceHarness ||
		ra.Origin != runattempt.OriginHarnessExecution ||
		(ra.ActionIntentID != reservation.ActionIntentID && ra.ActionIntentID != pending.Action.ID) ||
		ra.Workspace != reservation.Workspace ||
		ra.Command != reservation.Command ||
		pending.Action.IDs.WorkID != workID ||
		pending.Decision.ActionIntentID != pending.Action.ID ||
		!policy.Allowed(pending.Decision) {
		return runattempt.RunAttempt{}, pending, fmt.Errorf("proof completion retry is not bound to the reserved same-work RunAttempt and allowing decision")
	}
	if ra.ID != reservation.RunAttemptID {
		if err := s.persistProofCompletionRetryReachability(w, pending, ra, now); err != nil {
			return runattempt.RunAttempt{}, pending, err
		}
		if ra.Status == runattempt.StatusPending {
			ra, err = s.runAttempts().MarkRunning(ra.ID, now)
			if err != nil {
				return runattempt.RunAttempt{}, pending, err
			}
		}
	}
	if proofRunAttemptMatchesPreparedCompletion(ra, pending) {
		return ra, pending, nil
	}
	if ra.Status == runattempt.StatusRunning {
		return ra, pending, nil
	}
	if !runattempt.IsTerminal(ra.Status) {
		return runattempt.RunAttempt{}, pending, fmt.Errorf("proof completion retry RunAttempt status %q is not recoverable", ra.Status)
	}

	retry, retryErr := s.prepareProofRunAttempt(*w, pending.Action.ID, ProofResourceReserveInput{
		Actor:     actor,
		Workspace: reservation.Workspace,
		Command:   reservation.Command,
	}, now)
	failRetry := func(cause error) (runattempt.RunAttempt, proofResourceCompletionPending, error) {
		if retry.ID != "" {
			cause = errors.Join(cause, s.completeProofAttempt(
				retry.ID,
				runattempt.StatusFailed,
				"proof completion retry preparation failed: "+cause.Error(),
				now,
			))
		}
		return runattempt.RunAttempt{}, pending, cause
	}
	if retryErr != nil {
		return failRetry(retryErr)
	}
	if retry.PredecessorID != ra.ID {
		return failRetry(fmt.Errorf("proof completion retry predecessor %q does not match terminal attempt %q", retry.PredecessorID, ra.ID))
	}
	proofResourceAfterCompletionRetryMint(retry)
	nextPending := reseedProofResourceCompletionPending(pending, retry, now)
	if err := proofResourceWriteCompletionRetryJournal(s, pendingPath, nextPending, retry); err != nil {
		reachabilityErr := s.persistProofCompletionRetryReachability(w, nextPending, retry, now)
		return failRetry(errors.Join(
			fmt.Errorf("persist proof completion retry journal: %w", err),
			reachabilityErr,
		))
	}
	proofResourceAfterCompletionRetryJournal(retry, nextPending)
	pending = nextPending
	if err := s.persistProofCompletionRetryReachability(w, pending, retry, now); err != nil {
		return failRetry(err)
	}
	retry, err = s.runAttempts().MarkRunning(retry.ID, now)
	if err != nil {
		return failRetry(err)
	}
	return retry, pending, nil
}

func (s *Store) adoptProofCompletionRetryDescendant(
	w *envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	pending proofResourceCompletionPending,
	pendingPath, workID, actor string,
	now time.Time,
) (proofResourceCompletionPending, error) {
	currentID := pending.Result.RunAttemptID
	adopted := false
	seen := map[string]bool{currentID: true}
	candidateIDs := append([]string(nil), w.RunAttemptIDs...)
	allAttempts, err := s.runAttempts().List()
	if err != nil {
		return pending, fmt.Errorf("list proof completion retry descendants: %w", err)
	}
	for _, candidate := range allAttempts {
		if candidate.WorkID == workID && !oneOf(candidate.ID, candidateIDs) {
			candidateIDs = append(candidateIDs, candidate.ID)
		}
	}
	for {
		var descendants []runattempt.RunAttempt
		for _, candidateID := range candidateIDs {
			if seen[candidateID] {
				continue
			}
			candidate, err := s.runAttempts().Get(candidateID)
			if err != nil {
				return pending, fmt.Errorf("read proof completion retry descendant %s: %w", candidateID, err)
			}
			if candidate.PredecessorID != currentID ||
				candidate.WorkID != workID ||
				candidate.Actor != actor ||
				candidate.Harness != proofResourceHarness ||
				candidate.Origin != runattempt.OriginHarnessExecution ||
				candidate.ActionIntentID != pending.Action.ID ||
				candidate.Workspace != reservation.Workspace ||
				candidate.Command != reservation.Command {
				continue
			}
			descendants = append(descendants, candidate)
		}
		if len(descendants) == 0 {
			break
		}
		if len(descendants) > 1 {
			var ambiguityErrs []error
			ids := make([]string, 0, len(descendants))
			for _, descendant := range descendants {
				ids = append(ids, descendant.ID)
				if err := s.persistProofCompletionRetryReachability(w, pending, descendant, now); err != nil {
					ambiguityErrs = append(ambiguityErrs, fmt.Errorf("persist ambiguous retry %s reachability: %w", descendant.ID, err))
				}
				if !runattempt.IsTerminal(descendant.Status) {
					if err := s.completeProofAttempt(
						descendant.ID,
						runattempt.StatusAbandoned,
						"ambiguous proof completion retry lineage",
						now,
					); err != nil {
						ambiguityErrs = append(ambiguityErrs, fmt.Errorf("terminalize ambiguous retry %s: %w", descendant.ID, err))
					}
				}
			}
			return pending, errors.Join(
				fmt.Errorf(
					"ambiguous proof completion retry descendants of %s: %s",
					currentID,
					strings.Join(ids, ","),
				),
				errors.Join(ambiguityErrs...),
			)
		}
		descendant := descendants[0]
		pending = reseedProofResourceCompletionPending(pending, descendant, now)
		currentID = descendant.ID
		seen[currentID] = true
		adopted = true
	}
	if !adopted {
		return pending, nil
	}
	if err := s.writeProofResourceCompletionPending(pendingPath, pending); err != nil {
		return pending, fmt.Errorf("adopt reachable proof completion retry %s: %w", currentID, err)
	}
	return pending, nil
}

func (s *Store) persistProofCompletionRetryReachability(
	w *envelope.WorkEnvelope,
	pending proofResourceCompletionPending,
	retry runattempt.RunAttempt,
	now time.Time,
) error {
	if err := proofResourceWriteEventRecord(s, "completion_retry_action", pending.Action.ID, &pending.Action); err != nil {
		return err
	}
	if err := proofResourceWriteEventRecord(s, "completion_retry_decision", pending.Decision.ID, &pending.Decision); err != nil {
		return err
	}
	if !oneOf(pending.Action.ID, w.ActionIntentIDs) {
		w.ActionIntentIDs = append(w.ActionIntentIDs, pending.Action.ID)
	}
	if !oneOf(retry.ID, w.RunAttemptIDs) {
		w.RunAttemptIDs = append(w.RunAttemptIDs, retry.ID)
	}
	w.PolicyDecisionID = pending.Decision.ID
	if err := proofResourceWriteWorkRecord(s, "completion_retry_work", w); err != nil {
		return err
	}
	retryEvent := Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       pending.Result.Actor,
		Note:     "proof completion retry RunAttempt prepared from terminal predecessor",
		Attached: []string{pending.Action.ID, pending.Decision.ID, pending.ReservationID, retry.PredecessorID, retry.ID},
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.WorkID == retryEvent.WorkID &&
			event.By == retryEvent.By &&
			event.Note == retryEvent.Note &&
			reflect.DeepEqual(event.Attached, retryEvent.Attached) {
			return nil
		}
	}
	return proofResourceAppendLifecycleRecord(s, retryEvent)
}

func reseedProofResourceCompletionPending(
	pending proofResourceCompletionPending,
	retry runattempt.RunAttempt,
	now time.Time,
) proofResourceCompletionPending {
	resultID := idFor("pfrs_", pending.Result.WorkID, pending.ReservationID, pending.Result.ReportArtifactRef, pending.Result.FailureReason, retry.ID)
	completionID := idFor("pfrc_", pending.Result.WorkID, pending.ReservationID, resultID)
	verificationRef := ""
	if pending.VerificationRef != "" {
		verificationRef = "proof-check:" + resultID
	}
	pending.CreatedAt = now.UTC().Format(time.RFC3339Nano)
	pending.Result.ID = resultID
	pending.Result.CreatedAt = pending.CreatedAt
	pending.Result.RunAttemptID = retry.ID
	pending.Result.RunAttemptStatus = pending.TerminalStatus
	pending.Result.VerificationRef = verificationRef
	pending.Result.CompletionID = completionID
	pending.Completion.ID = completionID
	pending.Completion.CreatedAt = pending.CreatedAt
	pending.Completion.ResultID = resultID
	pending.Completion.RunAttemptID = retry.ID
	pending.Completion.VerificationRef = verificationRef
	pending.VerificationRef = verificationRef
	pending.ProofRefs = []string{pending.ReservationID, resultID, completionID}
	if pending.Result.ReportArtifactRef != "" {
		pending.ProofRefs = append(pending.ProofRefs, pending.Result.ReportArtifactRef)
	}
	if verificationRef != "" {
		pending.ProofRefs = append(pending.ProofRefs, verificationRef)
	}
	pending.LifecycleEvent = Event{
		TS:       ts(now),
		WorkID:   pending.WorkID,
		From:     pending.LifecycleEvent.From,
		To:       pending.LifecycleEvent.To,
		By:       pending.Result.Actor,
		Note:     "proof resource finalizer retry committed; verification publishes after terminal RunAttempt",
		Attached: append([]string{pending.Action.ID, pending.Decision.ID, pending.ReservationID, resultID, completionID, retry.PredecessorID, retry.ID}, pending.ProofRefs[3:]...),
	}
	return pending
}

func (s *Store) persistPreparedProofCompletion(w *envelope.WorkEnvelope, pending proofResourceCompletionPending) error {
	for _, item := range []struct {
		stage string
		id    string
		value any
	}{
		{stage: "action", id: pending.Action.ID, value: &pending.Action},
		{stage: "decision", id: pending.Decision.ID, value: &pending.Decision},
		{stage: "result", id: pending.Result.ID, value: &pending.Result},
	} {
		if err := proofResourceWriteEventRecord(s, item.stage, item.id, item.value); err != nil {
			return fmt.Errorf("persist %s: %w", item.stage, err)
		}
	}
	if !oneOf(pending.Action.ID, w.ActionIntentIDs) {
		w.ActionIntentIDs = append(w.ActionIntentIDs, pending.Action.ID)
	}
	w.PolicyDecisionID = pending.Decision.ID
	if err := proofResourceWriteWorkRecord(s, "work", w); err != nil {
		return fmt.Errorf("persist work: %w", err)
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if reflect.DeepEqual(event, pending.LifecycleEvent) {
			return nil
		}
	}
	if err := proofResourceAppendLifecycleRecord(s, pending.LifecycleEvent); err != nil {
		return fmt.Errorf("persist lifecycle: %w", err)
	}
	return nil
}

func (s *Store) terminalizePreparedProofCompletion(
	ra runattempt.RunAttempt,
	pending proofResourceCompletionPending,
	now time.Time,
) (runattempt.RunAttempt, error) {
	if runattempt.IsTerminal(ra.Status) {
		if !proofRunAttemptMatchesPreparedCompletion(ra, pending) {
			return runattempt.RunAttempt{}, fmt.Errorf("terminal proof RunAttempt conflicts with pending completion")
		}
		return ra, nil
	}
	if err := proofResourceSetRunAttemptProofRefs(s, ra.ID, pending.ProofRefs); err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("persist RunAttempt proof refs: %w", err)
	}
	terminal, err := proofResourceTerminalizeRunAttempt(s, ra.ID, pending.TerminalStatus, pending.TerminalReason, now)
	if err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("persist terminal RunAttempt: %w", err)
	}
	return terminal, nil
}

func proofRunAttemptMatchesPreparedCompletion(
	ra runattempt.RunAttempt,
	pending proofResourceCompletionPending,
) bool {
	return runattempt.IsTerminal(ra.Status) &&
		ra.Status == pending.TerminalStatus &&
		ra.SweepReason == pending.TerminalReason &&
		reflect.DeepEqual(ra.ProofRefs, pending.ProofRefs)
}

func (s *Store) activeProofRunAttempt(reservation ProofResourceReservation, workID, actor string) (runattempt.RunAttempt, error) {
	if reservation.WorkID != workID ||
		reservation.Actor != actor ||
		reservation.Status != ProofResourceAcquired ||
		reservation.RunAttemptID == "" {
		return runattempt.RunAttempt{}, fmt.Errorf("proof reservation %s does not authorize actor %q on work %s", reservation.ID, actor, workID)
	}
	ra, err := s.runAttempts().Get(reservation.RunAttemptID)
	if err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("proof reservation %s active RunAttempt: %w", reservation.ID, err)
	}
	if ra.WorkID != workID ||
		ra.Actor != actor ||
		ra.Harness != proofResourceHarness ||
		ra.Origin != runattempt.OriginHarnessExecution ||
		ra.ActionIntentID != reservation.ActionIntentID ||
		ra.Workspace != reservation.Workspace ||
		ra.Command != reservation.Command ||
		ra.Status != runattempt.StatusRunning {
		return runattempt.RunAttempt{}, fmt.Errorf("proof reservation %s requires active RunAttempt bound to same work/actor/action, got %+v", reservation.ID, ra)
	}
	return ra, nil
}

func decodeOwnedProofReport(body []byte) (ownedProofReport, error) {
	var report ownedProofReport
	if err := consumerUniqueJSON(body); err != nil {
		return ownedProofReport{}, err
	}
	if err := json.Unmarshal(body, &report); err != nil {
		return ownedProofReport{}, err
	}
	if report.SchemaVersion != "compound.proof_check_report.v0.1" &&
		report.SchemaVersion != "compound.proof_check_report.v0.2" &&
		report.SchemaVersion != "compound.proof_check_report.v0.3" &&
		report.SchemaVersion != "compound.proof_check_report.v0.4" &&
		report.SchemaVersion != "compound.proof_check_report.v0.5" && report.SchemaVersion != ProofCheckSchemaVersion && report.SchemaVersion != RemoteProofReportSchemaVersion {
		return ownedProofReport{}, fmt.Errorf("unsupported schema_version %q", report.SchemaVersion)
	}
	if report.SchemaVersion == "compound.proof_check_report.v0.3" || report.SchemaVersion == "compound.proof_check_report.v0.4" || report.SchemaVersion == "compound.proof_check_report.v0.5" || report.SchemaVersion == ProofCheckSchemaVersion || report.SchemaVersion == RemoteProofReportSchemaVersion {
		if report.SourceSnapshot != nil {
			if err := gitrepository.ValidateSnapshotShape(*report.SourceSnapshot); err != nil {
				return ownedProofReport{}, fmt.Errorf("source_snapshot: %w", err)
			}
			if !report.SourceSnapshot.Clean {
				return ownedProofReport{}, fmt.Errorf("source_snapshot must be clean")
			}
		}
	}
	if err := validateRemoteReportSchema(report); err != nil {
		return ownedProofReport{}, err
	}
	if report.Status != "ok" && report.Status != "failed" {
		return ownedProofReport{}, fmt.Errorf("status must be ok or failed")
	}
	if report.DryRun || report.CIOnly || report.CIConsole {
		return ownedProofReport{}, fmt.Errorf("lightweight/dry-run report is not a full proof battery")
	}
	if strings.TrimSpace(report.GeneratedAt) == "" {
		return ownedProofReport{}, fmt.Errorf("generated_at required")
	}
	if _, err := time.Parse(time.RFC3339, report.GeneratedAt); err != nil {
		return ownedProofReport{}, fmt.Errorf("generated_at: %w", err)
	}
	if strings.TrimSpace(report.Repo) == "" {
		return ownedProofReport{}, fmt.Errorf("repo required")
	}
	if len(report.Steps) == 0 {
		return ownedProofReport{}, fmt.Errorf("steps required")
	}
	failed := false
	passed := false
	for i, step := range report.Steps {
		if strings.TrimSpace(step.Name) == "" || strings.TrimSpace(step.ProofRef) == "" {
			return ownedProofReport{}, fmt.Errorf("step %d name and proof_ref required", i)
		}
		switch step.Status {
		case "ok":
			passed = true
		case "skipped":
		case "failed":
			failed = true
		default:
			return ownedProofReport{}, fmt.Errorf("step %d invalid status %q", i, step.Status)
		}
	}
	if report.Status == "ok" && (failed || !passed) {
		return ownedProofReport{}, fmt.Errorf("ok report must contain a passed gate and no failed gate")
	}
	if report.Status == "failed" && !failed {
		return ownedProofReport{}, fmt.Errorf("failed report must contain a failed gate")
	}
	return report, nil
}

func validateProofResourceProfileShape(profile ProofResourceProfile) error {
	if d := profile.RemoteGoTest; d != nil {
		if profile.Kind != ProofResourceProfileCanonicalFull || profile.SourceSnapshot == nil || !reflect.DeepEqual(*profile.SourceSnapshot, d.Source) {
			return fmt.Errorf("remote descriptor differs from native profile")
		}
		if err := validateRemoteProofDescriptor(*d); err != nil {
			return err
		}
		found := false
		for _, gate := range profile.Gates {
			if gate.Name == "go-test" {
				found = true
				if gate.ProofRef != "go-test-count-1-general-release" || !reflect.DeepEqual(gate.Command, remoteGateCommand(d.Source.HeadSHA)) {
					return fmt.Errorf("remote profile gate mismatch")
				}
			}
		}
		if !found {
			return fmt.Errorf("remote profile missing go-test")
		}
	}

	if profile.Kind == "" {
		return fmt.Errorf("kind required")
	}
	if profile.Repo == "" {
		return fmt.Errorf("repo required")
	}
	if profile.SourceSnapshot != nil {
		if err := gitrepository.ValidateSnapshotShape(*profile.SourceSnapshot); err != nil {
			return fmt.Errorf("source_snapshot: %w", err)
		}
		if !profile.SourceSnapshot.Clean {
			return fmt.Errorf("source_snapshot must be clean")
		}
		if profile.BaseRef != profile.SourceSnapshot.BaseSHA {
			return fmt.Errorf("base_ref must equal source_snapshot.base_sha")
		}
	}
	if len(profile.Gates) == 0 {
		return fmt.Errorf("full gate inventory required")
	}
	names := make(map[string]bool, len(profile.Gates))
	refs := make(map[string]bool, len(profile.Gates))
	for i, gate := range profile.Gates {
		if gate.Name == "" {
			return fmt.Errorf("gate %d name required", i)
		}
		if len(gate.Command) == 0 {
			return fmt.Errorf("gate %d command required", i)
		}
		for j, part := range gate.Command {
			if strings.TrimSpace(part) == "" {
				return fmt.Errorf("gate %d command part %d required", i, j)
			}
		}
		if gate.ProofRef == "" {
			return fmt.Errorf("gate %d proof_ref required", i)
		}
		if names[gate.Name] {
			return fmt.Errorf("gate inventory repeats name %q", gate.Name)
		}
		if refs[gate.ProofRef] {
			return fmt.Errorf("gate inventory repeats proof_ref %q", gate.ProofRef)
		}
		names[gate.Name] = true
		refs[gate.ProofRef] = true
	}
	return nil
}

func proofResourceProfileDigest(profile ProofResourceProfile) (string, error) {
	profile.Digest = ""
	body, err := json.Marshal(profile)
	if err != nil {
		return "", fmt.Errorf("marshal proof resource profile: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func validateOwnedProofReportAgainstReservation(report ownedProofReport, reservation ProofResourceReservation) error {
	if err := validateRemoteReportReservation(report, reservation); err != nil {
		return err
	}
	profile, err := normalizeProofResourceProfile(reservation.Profile)
	if err != nil {
		return fmt.Errorf("reservation profile invalid: %w", err)
	}
	if profile.Digest != reservation.Profile.Digest {
		return fmt.Errorf("reservation profile digest mismatch: stored=%q canonical=%q", reservation.Profile.Digest, profile.Digest)
	}
	if profile.Kind != ProofResourceProfileCanonicalFull {
		return fmt.Errorf("reservation profile_kind %q is not certifying; want %q", profile.Kind, ProofResourceProfileCanonicalFull)
	}
	if strings.TrimSpace(report.ProfileKind) != ProofResourceProfileCanonicalFull {
		return fmt.Errorf("report profile_kind %q is not certifying; want %q", report.ProfileKind, ProofResourceProfileCanonicalFull)
	}
	if reservation.Workspace != profile.Repo || report.Repo != profile.Repo {
		return fmt.Errorf("repo mismatch: report=%q profile=%q workspace=%q", report.Repo, profile.Repo, reservation.Workspace)
	}
	if strings.TrimSpace(report.BaseRef) != profile.BaseRef {
		return fmt.Errorf("base_ref mismatch: report=%q profile=%q", report.BaseRef, profile.BaseRef)
	}
	if !reflect.DeepEqual(report.SourceSnapshot, profile.SourceSnapshot) {
		return fmt.Errorf("source_snapshot mismatch between report and reservation profile")
	}
	if profile.SourceSnapshot != nil && report.SchemaVersion != "compound.proof_check_report.v0.3" && report.SchemaVersion != "compound.proof_check_report.v0.4" && report.SchemaVersion != "compound.proof_check_report.v0.5" && report.SchemaVersion != ProofCheckSchemaVersion && report.SchemaVersion != RemoteProofReportSchemaVersion {
		return fmt.Errorf("source-bound proof requires compound.proof_check_report.v0.3, v0.4, v0.5 or v0.6")
	}
	if len(report.Steps) != len(profile.Gates) {
		return fmt.Errorf("gate inventory mismatch: report=%d profile=%d", len(report.Steps), len(profile.Gates))
	}
	reportProofRefs := make([]string, 0, len(report.Steps))
	nativeRemaining := CanonicalConsumerEvidenceBudget
	for _, step := range report.Steps {
		if step.ConsumerEvidence.size() > nativeRemaining {
			return fmt.Errorf("aggregate native evidence exceeds report budget")
		}
		nativeRemaining -= step.ConsumerEvidence.size()
	}
	failedGates := 0
	for i, gate := range profile.Gates {
		step := report.Steps[i]
		switch {
		case step.Name != gate.Name:
			return fmt.Errorf("gate inventory mismatch at %d: name=%q want=%q", i, step.Name, gate.Name)
		case !reflect.DeepEqual(step.Command, gate.Command):
			return fmt.Errorf("gate %q command mismatch: report=%v profile=%v", gate.Name, step.Command, gate.Command)
		case strings.TrimSpace(step.Dir) != gate.Dir:
			return fmt.Errorf("gate %q dir mismatch: report=%q profile=%q", gate.Name, step.Dir, gate.Dir)
		case strings.TrimSpace(step.SkipIfMissing) != gate.SkipIfMissing:
			return fmt.Errorf("gate %q skip_if_missing mismatch: report=%q profile=%q", gate.Name, step.SkipIfMissing, gate.SkipIfMissing)
		case step.ProofRef != gate.ProofRef:
			return fmt.Errorf("gate %q proof_ref mismatch: report=%q profile=%q", gate.Name, step.ProofRef, gate.ProofRef)
		}
		redactionStep := ProofCheckStep{
			Name: step.Name, Command: step.Command, ProofRef: step.ProofRef,
			Status: step.Status, RedactionEvidence: step.RedactionEvidence, PrivateSourceEvidence: step.PrivateSourceEvidence,
		}
		_, reviewed, reviewErr := PublicRedactionBindingForStep(redactionStep)
		if reviewErr != nil {
			return fmt.Errorf("gate %q review binding: %w", gate.Name, reviewErr)
		}
		_, privatelyReviewed, privateReviewErr := PrivateSourceBindingForStep(redactionStep)
		if privateReviewErr != nil {
			return fmt.Errorf("gate %q private review binding: %w", gate.Name, privateReviewErr)
		}
		if (reviewed || privatelyReviewed) && report.SchemaVersion != "compound.proof_check_report.v0.5" && report.SchemaVersion != ProofCheckSchemaVersion && report.SchemaVersion != RemoteProofReportSchemaVersion {
			return fmt.Errorf("reviewed redaction requires proof report v0.5 or v0.6")
		}
		if err := ValidatePublicRedactionEvidence(redactionStep, profile.SourceSnapshot, reservation.WorkID); err != nil {
			return fmt.Errorf("gate %q redaction evidence: %w", gate.Name, err)
		}
		if err := ValidatePrivateSourceEvidence(redactionStep, profile.SourceSnapshot, reservation.WorkID); err != nil {
			return fmt.Errorf("gate %q private source evidence: %w", gate.Name, err)
		}

		if strings.HasPrefix(gate.Name, "consumer:") && step.Status == "ok" {
			if err := ValidateCanonicalConsumerEvidence(ProofCheckStep{
				Name: step.Name, Command: step.Command, Dir: step.Dir, SkipIfMissing: step.SkipIfMissing,
				ProofRef: step.ProofRef, ConsumerEvidence: step.ConsumerEvidence,
			}); err != nil {
				return fmt.Errorf("gate %q native evidence: %w", gate.Name, err)
			}
		}
		switch step.Status {
		case "ok":
			reportProofRefs = append(reportProofRefs, step.ProofRef)
		case "failed":
			if report.Status != "failed" {
				return fmt.Errorf("canonical_full ok report contains failed gate %q", gate.Name)
			}
			failedGates++
		default:
			return fmt.Errorf(
				"canonical_full gate %q status %q is incomplete; every reserved gate must finish ok or failed",
				gate.Name,
				step.Status,
			)
		}
	}
	if report.Status == "ok" && failedGates != 0 {
		return fmt.Errorf("canonical_full ok report contains %d failed gates", failedGates)
	}
	if report.Status == "failed" && failedGates == 0 {
		return fmt.Errorf("canonical_full failed report contains no failed gate")
	}
	if !slices.Equal(report.ProofRefs, reportProofRefs) {
		return fmt.Errorf("proof_ref inventory mismatch: report=%v successful_gates=%v", report.ProofRefs, reportProofRefs)
	}
	generatedAt, err := time.Parse(time.RFC3339, report.GeneratedAt)
	if err != nil {
		return fmt.Errorf("generated_at: %w", err)
	}
	acquiredAt, err := time.Parse(time.RFC3339, reservation.AcquiredAt)
	if err != nil {
		return fmt.Errorf("reservation acquired_at: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339, reservation.ExpiresAt)
	if err != nil {
		return fmt.Errorf("reservation expires_at: %w", err)
	}
	if generatedAt.Before(acquiredAt) || !generatedAt.Before(expiresAt) {
		return fmt.Errorf("generated_at %s outside reservation window [%s,%s)", report.GeneratedAt, reservation.AcquiredAt, reservation.ExpiresAt)
	}
	return nil
}

func proofRefreshReservationSuffix(reservationID string) string {
	return strings.TrimPrefix(strings.TrimSpace(reservationID), "pfr_")
}

func proofRefreshPreparationPrefix(reservationID string) string {
	return "pfrp_" + proofRefreshReservationSuffix(reservationID) + "_"
}

func proofRefreshOutcomePrefix(reservationID string) string {
	return "pfrh_" + proofRefreshReservationSuffix(reservationID) + "_"
}

func proofRefreshPreparationID(reservationID, runAttemptID string, now time.Time) string {
	return idFor(proofRefreshPreparationPrefix(reservationID), runAttemptID, ts(now))
}

// proofRefreshLegacyOutcomeID preserves reads of persisted v0.1 evidence whose
// identity used the whole-second expiry and could therefore collide.
func proofRefreshLegacyOutcomeID(reservationID, runAttemptID, expiresAt string) string {
	return idFor(proofRefreshOutcomePrefix(reservationID), runAttemptID, expiresAt)
}

func proofRefreshPreparedOutcomeID(reservationID, runAttemptID, expiresAt, preparationID string) string {
	return idFor(proofRefreshOutcomePrefix(reservationID), runAttemptID, expiresAt, preparationID)
}

func proofRefreshActionTime(reservation ProofResourceReservation) (time.Time, error) {
	createdAt, err := time.Parse(time.RFC3339Nano, reservation.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("proof reservation %s created_at: %w", reservation.ID, err)
	}
	return createdAt, nil
}

func (s *Store) proofRefreshRefs(reservation ProofResourceReservation) ([]string, bool, error) {
	prefix := proofRefreshOutcomePrefix(reservation.ID)
	scopedRefs, err := s.events.EnumerateIDs(prefix, true)
	if err != nil {
		return nil, false, err
	}
	if len(scopedRefs) > 0 {
		return scopedRefs, true, nil
	}
	// Compatibility path for reservations refreshed by pre-bounded runtimes.
	// Once one reservation-scoped outcome exists it is necessarily newer than
	// those historical lifecycle attachments, so the hot path never re-scans
	// the global work log.
	events, err := s.eventsFor(reservation.WorkID)
	if err != nil {
		return nil, false, err
	}
	refs := make([]string, 0)
	seen := map[string]bool{}
	for _, event := range events {
		for _, ref := range event.Attached {
			if !strings.HasPrefix(ref, "pfrh_") || seen[ref] {
				continue
			}
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	return refs, false, nil
}

func (s *Store) proofReservationWithLatestRefresh(reservation ProofResourceReservation) (ProofResourceReservation, error) {
	refs, scoped, err := s.proofRefreshRefs(reservation)
	if err != nil {
		return ProofResourceReservation{}, err
	}
	latest := ProofResourceRefresh{}
	for _, ref := range refs {
		var refresh ProofResourceRefresh
		if err := s.readEventJSON(ref, &refresh); err != nil {
			return ProofResourceReservation{}, fmt.Errorf("proof refresh evidence %s: %w", ref, err)
		}
		if refresh.ReservationID != reservation.ID {
			if scoped {
				return ProofResourceReservation{}, fmt.Errorf("proof refresh evidence %s escaped reservation-scoped family", ref)
			}
			continue
		}
		if scoped {
			preparedID := proofRefreshPreparedOutcomeID(
				reservation.ID,
				refresh.RunAttemptID,
				refresh.ExpiresAt,
				refresh.PreparationID,
			)
			legacyID := proofRefreshLegacyOutcomeID(reservation.ID, refresh.RunAttemptID, refresh.ExpiresAt)
			if refresh.ID != preparedID && refresh.ID != legacyID {
				return ProofResourceReservation{}, fmt.Errorf("proof refresh evidence %s has invalid reservation-scoped identity", ref)
			}
		}
		if refresh.SchemaVersion != ProofResourceRefreshSchemaVersion ||
			refresh.ID != ref ||
			refresh.WorkID != reservation.WorkID ||
			refresh.Actor != reservation.Actor ||
			refresh.RunAttemptID != reservation.RunAttemptID ||
			refresh.Holder != reservation.Holder ||
			refresh.WindowOpenedAt != reservation.AcquiredAt ||
			refresh.ActionIntentID == "" ||
			refresh.PolicyDecisionID == "" {
			return ProofResourceReservation{}, fmt.Errorf("proof refresh evidence %s is not bound to the reserved work/actor/RunAttempt/window", ref)
		}
		if scoped {
			var preparation ProofResourceRefreshPreparation
			if err := s.readEventJSON(refresh.PreparationID, &preparation); err != nil {
				return ProofResourceReservation{}, fmt.Errorf("proof refresh %s preparation: %w", ref, err)
			}
			preparedAt, err := time.Parse(time.RFC3339Nano, preparation.CreatedAt)
			if err != nil {
				return ProofResourceReservation{}, fmt.Errorf("proof refresh %s preparation created_at: %w", ref, err)
			}
			if preparation.SchemaVersion != ProofResourceRefreshPreparationSchemaVersion ||
				preparation.ID != refresh.PreparationID ||
				preparation.ID != proofRefreshPreparationID(reservation.ID, refresh.RunAttemptID, preparedAt) ||
				preparation.WorkID != refresh.WorkID ||
				preparation.Actor != refresh.Actor ||
				preparation.ReservationID != refresh.ReservationID ||
				preparation.RunAttemptID != refresh.RunAttemptID ||
				preparation.Holder != refresh.Holder ||
				preparation.WindowOpenedAt != refresh.WindowOpenedAt ||
				preparation.RequestedAt != refresh.RefreshedAt ||
				preparation.TTLSeconds != refresh.TTLSeconds ||
				preparation.ActionIntentID != refresh.ActionIntentID ||
				preparation.PolicyDecisionID != refresh.PolicyDecisionID {
				return ProofResourceReservation{}, fmt.Errorf("proof refresh %s lacks its exact governed preparation", ref)
			}
		}
		var action envelope.ActionIntent
		if err := s.readEventJSON(refresh.ActionIntentID, &action); err != nil {
			return ProofResourceReservation{}, fmt.Errorf("proof refresh %s ActionIntent: %w", ref, err)
		}
		var decision envelope.PolicyDecision
		if err := s.readEventJSON(refresh.PolicyDecisionID, &decision); err != nil {
			return ProofResourceReservation{}, fmt.Errorf("proof refresh %s PolicyDecision: %w", ref, err)
		}
		if action.IDs.WorkID != reservation.WorkID ||
			action.Actor != reservation.Actor ||
			(action.Action != "refresh_full_proof_resource" &&
				action.Action != "rearm_expired_proof_completion") ||
			decision.ActionIntentID != action.ID ||
			!policy.Allowed(decision) {
			return ProofResourceReservation{}, fmt.Errorf("proof refresh evidence %s lacks its exact allowing governance chain", ref)
		}
		refreshedAt, err := time.Parse(time.RFC3339, refresh.RefreshedAt)
		if err != nil {
			return ProofResourceReservation{}, fmt.Errorf("proof refresh %s refreshed_at: %w", ref, err)
		}
		expiresAt, err := time.Parse(time.RFC3339, refresh.ExpiresAt)
		if err != nil || !expiresAt.After(refreshedAt) {
			return ProofResourceReservation{}, fmt.Errorf("proof refresh %s has invalid expires_at %q", ref, refresh.ExpiresAt)
		}
		if latest.ID == "" || refresh.RefreshedAt > latest.RefreshedAt {
			latest = refresh
		}
	}
	if latest.ID != "" {
		reservation.ExpiresAt = latest.ExpiresAt
		reservation.TTLSeconds = latest.TTLSeconds
	}
	return reservation, nil
}

func (s *Store) persistProofReservation(
	w *envelope.WorkEnvelope,
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
	reservation ProofResourceReservation,
	note string,
	now time.Time,
) error {
	if err := s.writeEventJSON(action.ID, &action); err != nil {
		return err
	}
	if err := s.writeEventJSON(decision.ID, &decision); err != nil {
		return err
	}
	if err := s.writeEventJSON(reservation.ID, &reservation); err != nil {
		return err
	}
	if !oneOf(action.ID, w.ActionIntentIDs) {
		w.ActionIntentIDs = append(w.ActionIntentIDs, action.ID)
	}
	if reservation.RunAttemptID != "" && !oneOf(reservation.RunAttemptID, w.RunAttemptIDs) {
		w.RunAttemptIDs = append(w.RunAttemptIDs, reservation.RunAttemptID)
	}
	w.PolicyDecisionID = decision.ID
	if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
		return err
	}
	attached := []string{action.ID, decision.ID, reservation.ID}
	if reservation.RunAttemptID != "" {
		attached = append(attached, reservation.RunAttemptID)
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       reservation.Actor,
		Note:     note,
		Attached: attached,
	})
}

func (s *Store) persistProofScarceMutationPreparation(
	w *envelope.WorkEnvelope,
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
	reservation ProofResourceReservation,
	runAttemptID, phase string,
	now time.Time,
) error {
	if err := proofResourceWriteEventRecord(s, phase+"_prepare_action", action.ID, &action); err != nil {
		return fmt.Errorf("persist proof %s preparation action: %w", phase, err)
	}
	if err := proofResourceWriteEventRecord(s, phase+"_prepare_decision", decision.ID, &decision); err != nil {
		return fmt.Errorf("persist proof %s preparation decision: %w", phase, err)
	}
	if !oneOf(action.ID, w.ActionIntentIDs) {
		w.ActionIntentIDs = append(w.ActionIntentIDs, action.ID)
	}
	if runAttemptID != "" && !oneOf(runAttemptID, w.RunAttemptIDs) {
		w.RunAttemptIDs = append(w.RunAttemptIDs, runAttemptID)
	}
	w.PolicyDecisionID = decision.ID
	if err := proofResourceWriteWorkRecord(s, phase+"_prepare_work", w); err != nil {
		return fmt.Errorf("persist proof %s preparation work: %w", phase, err)
	}
	attached := []string{action.ID, decision.ID}
	if phase == "refresh" || phase == "completion_rearm" || phase == "legacy_recovery" {
		attached = append(attached, reservation.ID)
	}
	if runAttemptID != "" {
		attached = append(attached, runAttemptID)
	}
	return proofResourceAppendLifecycleRecord(s, Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       reservation.Actor,
		Note:     "proof resource " + phase + " governance persisted before scarce mutation",
		Attached: attached,
	})
}

func (s *Store) persistProofHeartbeatPreparation(
	w *envelope.WorkEnvelope,
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
	preparation ProofResourceRefreshPreparation,
	now time.Time,
) error {
	if err := proofResourceWriteEventRecord(s, "refresh_prepare_action", action.ID, &action); err != nil {
		return fmt.Errorf("persist proof refresh preparation action: %w", err)
	}
	if err := proofResourceWriteEventRecord(s, "refresh_prepare_decision", decision.ID, &decision); err != nil {
		return fmt.Errorf("persist proof refresh preparation decision: %w", err)
	}
	if err := proofResourceWriteEventRecord(s, "refresh_prepare_record", preparation.ID, &preparation); err != nil {
		return fmt.Errorf("persist proof refresh preparation: %w", err)
	}
	if oneOf(action.ID, w.ActionIntentIDs) {
		return nil
	}

	// Root the recurring refresh operation exactly once. Every later current
	// decision and outcome remains reservation-scoped evidence, so neither the
	// WorkEnvelope nor its terminal Transition grows with battery duration.
	w.ActionIntentIDs = append(w.ActionIntentIDs, action.ID)
	w.PolicyDecisionID = decision.ID
	if err := proofResourceWriteWorkRecord(s, "refresh_root_work", w); err != nil {
		return fmt.Errorf("persist proof refresh governance root: %w", err)
	}
	return proofResourceAppendLifecycleRecord(s, Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       preparation.Actor,
		Note:     "proof resource heartbeat governance rooted for the reservation",
		Attached: []string{action.ID, decision.ID, preparation.ReservationID, preparation.ID, preparation.RunAttemptID},
	})
}

func (s *Store) persistProofRefresh(
	w *envelope.WorkEnvelope,
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
	refresh ProofResourceRefresh,
	now time.Time,
) error {
	note := "proof resource heartbeat refreshed the exclusive expiry"
	if action.Action == "rearm_expired_proof_completion" {
		note = "exact expired proof completion holder rearmed after journal validation and execution attestation"
	}
	if err := s.writeEventJSON(action.ID, &action); err != nil {
		return err
	}
	if err := s.writeEventJSON(decision.ID, &decision); err != nil {
		return err
	}
	if err := s.writeEventJSON(refresh.ID, &refresh); err != nil {
		return err
	}
	if !oneOf(action.ID, w.ActionIntentIDs) {
		w.ActionIntentIDs = append(w.ActionIntentIDs, action.ID)
	}
	w.PolicyDecisionID = decision.ID
	if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
		return err
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       refresh.Actor,
		Note:     note,
		Attached: []string{action.ID, decision.ID, refresh.ReservationID, refresh.ID, refresh.RunAttemptID},
	})
}

func (s *Store) prepareTrustedProofTerminationEvidence(
	holder, runAttemptID, actor string,
	now time.Time,
) (string, []byte, error) {
	resource, resolveErr := s.proofResourceForHolder(holder)
	if resolveErr != nil {
		return "", nil, resolveErr
	}
	attestation, err := resource.TryAttestHistoricalExecutionTerminated(holder)
	if err != nil {
		return "", nil, fmt.Errorf("proof resource trusted termination producer: %w", err)
	}
	body, err := json.Marshal(ProofResourceTerminationEvidence{
		SchemaVersion: ProofResourceTerminationEvidenceSchemaVersion,
		Producer:      attestation.Producer,
		BarrierID:     attestation.BarrierID,
		Holder:        attestation.Holder,
		RunAttemptID:  runAttemptID,
		Status:        "terminated_and_waited",
		ObservedAt:    now.UTC().Format(time.RFC3339Nano),
		Observer:      strings.TrimSpace(actor),
	})
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(body)
	ref := home.ArtifactRefPrefix + hex.EncodeToString(sum[:])
	if err := s.validateProofTerminationEvidenceBody(ref, body, runAttemptID, holder, now); err != nil {
		return "", nil, err
	}
	return ref, body, nil
}

func (s *Store) persistTrustedProofTerminationEvidence(expectedRef string, body []byte) error {
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return err
	}
	ref, err := artifacts.Put(body)
	if err != nil {
		return err
	}
	if ref != expectedRef {
		return fmt.Errorf("proof resource trusted termination evidence ref %q, want %q", ref, expectedRef)
	}
	return nil
}

func (s *Store) validateProofTerminationEvidence(ref, runAttemptID, holder string, now time.Time) error {
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return err
	}
	body, err := artifacts.Get(ref)
	if err != nil {
		return fmt.Errorf("proof resource termination evidence %q is not in immutable Compound custody: %w", ref, err)
	}
	return s.validateProofTerminationEvidenceBody(ref, body, runAttemptID, holder, now)
}

func (s *Store) validateProofTerminationEvidenceBody(
	ref string,
	body []byte,
	runAttemptID, holder string,
	now time.Time,
) error {
	var evidence ProofResourceTerminationEvidence
	if err := json.Unmarshal(body, &evidence); err != nil {
		return fmt.Errorf("proof resource termination evidence %q is malformed: %w", ref, err)
	}
	resource, resolveErr := s.proofResourceForHolder(holder)
	if resolveErr != nil {
		return resolveErr
	}
	attestation, err := resource.TryAttestHistoricalExecutionTerminated(holder)
	if err != nil {
		return fmt.Errorf("proof resource termination evidence %q cannot be re-attested: %w", ref, err)
	}
	if evidence.Producer != attestation.Producer ||
		evidence.BarrierID != attestation.BarrierID ||
		evidence.Holder != attestation.Holder {
		return fmt.Errorf("proof resource termination evidence %q is not from the trusted execution-barrier producer", ref)
	}
	if evidence.SchemaVersion != ProofResourceTerminationEvidenceSchemaVersion ||
		evidence.RunAttemptID != runAttemptID ||
		evidence.Status != "terminated_and_waited" ||
		strings.TrimSpace(evidence.Observer) == "" {
		return fmt.Errorf(
			"proof resource termination evidence %q is not a terminated-and-waited receipt for %s",
			ref,
			runAttemptID,
		)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, evidence.ObservedAt)
	if err != nil {
		return fmt.Errorf("proof resource termination evidence %q observed_at: %w", ref, err)
	}
	ra, err := s.runAttempts().Get(runAttemptID)
	if err != nil {
		return fmt.Errorf("proof resource termination evidence %q RunAttempt: %w", ref, err)
	}
	if observedAt.Before(ra.CreatedAt) || observedAt.After(now) {
		return fmt.Errorf(
			"proof resource termination evidence %q observed_at %s is outside RunAttempt interval [%s,%s]",
			ref,
			evidence.ObservedAt,
			ra.CreatedAt.UTC().Format(time.RFC3339Nano),
			now.UTC().Format(time.RFC3339Nano),
		)
	}
	return nil
}

func (s *Store) persistProofTerminationConfirmation(
	w *envelope.WorkEnvelope,
	reservation ProofResourceReservation,
	now time.Time,
) error {
	confirmation := ProofResourceTerminationConfirmation{
		SchemaVersion:          ProofResourceTerminationSchemaVersion,
		ID:                     reservation.TerminationConfirmationID,
		Kind:                   "work.proof_resource.termination",
		CreatedAt:              reservation.TerminationConfirmedAt,
		WorkID:                 reservation.WorkID,
		Actor:                  reservation.Actor,
		ReservationID:          reservation.ID,
		PriorHolder:            reservation.PriorHolder,
		TerminatedRunAttemptID: reservation.TerminatedRunAttemptID,
		TerminationEvidenceRef: reservation.TerminationEvidenceRef,
		ActionIntentID:         reservation.ActionIntentID,
		PolicyDecisionID:       reservation.PolicyDecisionID,
	}
	if err := s.writeEventJSON(confirmation.ID, &confirmation); err != nil {
		return fmt.Errorf("persist proof termination confirmation: %w", err)
	}
	if !oneOf(reservation.TerminationEvidenceRef, w.ArtifactIDs) {
		w.ArtifactIDs = append(w.ArtifactIDs, reservation.TerminationEvidenceRef)
		if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
			return fmt.Errorf("persist proof termination evidence reachability: %w", err)
		}
	}
	return s.appendEvent(Event{
		TS:     ts(now),
		WorkID: w.ID,
		From:   w.State,
		To:     w.State,
		By:     reservation.Actor,
		Note: fmt.Sprintf(
			"proof resource predecessor %s termination confirmed by governed immutable evidence before exceptional takeover",
			reservation.TerminatedRunAttemptID,
		),
		Attached: []string{
			reservation.ActionIntentID,
			reservation.PolicyDecisionID,
			reservation.ID,
			confirmation.ID,
			reservation.TerminatedRunAttemptID,
			reservation.TerminationEvidenceRef,
		},
	})
}

func (s *Store) persistProofAcquisitionFailureReachability(
	w *envelope.WorkEnvelope,
	action envelope.ActionIntent,
	decision envelope.PolicyDecision,
	reservation ProofResourceReservation,
	now time.Time,
) error {
	if !oneOf(action.ID, w.ActionIntentIDs) {
		w.ActionIntentIDs = append(w.ActionIntentIDs, action.ID)
	}
	if !oneOf(reservation.RunAttemptID, w.RunAttemptIDs) {
		w.RunAttemptIDs = append(w.RunAttemptIDs, reservation.RunAttemptID)
	}
	w.PolicyDecisionID = decision.ID
	if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
		return err
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       reservation.Actor,
		Note:     "proof resource acquisition failed after reservation persistence; lease released before execution",
		Attached: []string{action.ID, decision.ID, reservation.ID, reservation.RunAttemptID},
	})
}

func (s *Store) proofResourceAction(
	w envelope.WorkEnvelope,
	reservationID, actor, phase, priorHolder, reason, terminatedRunAttemptID, terminationEvidenceRef string,
	now time.Time,
) envelope.ActionIntent {
	action := proofResourceActionReserve
	preview := "reserve full proof resource before any gate subprocess"
	rollback := "release the exclusive proof resource or let its heartbeat-backed TTL expire"
	mutationClass := "local_reversible"
	reversibility := "easy"
	blastRadius := "local"
	switch {
	case phase == "complete":
		action = "complete_full_proof_resource"
		preview = "persist governed full proof result while exclusive, then release"
		rollback = "result remains evidence; no successor holder is released by a failed finalizer"
	case phase == "refresh":
		action = "refresh_full_proof_resource"
		preview = "heartbeat the active proof RunAttempt and extend its exact exclusive reservation"
		rollback = "stop refreshing; the last evidenced TTL expires without reopening a stale holder"
	case phase == "completion_rearm":
		action = "rearm_expired_proof_completion"
		preview = "rearm the exact expired proof holder after complete journal validation and execution termination"
		rollback = "the exact holder remains auditable and its short rearmed TTL expires without successor release"
	case phase == "legacy_recovery":
		action = "recover_retained_legacy_proof_resource"
		preview = "release exact retained pre-v0.3 proof holder without verification"
		rollback = "the retained report remains evidence and a fresh source-bound v0.3 battery remains required"
	case strings.TrimSpace(terminationEvidenceRef) != "":
		action = proofResourceActionStealAfterTermination
		preview = fmt.Sprintf(
			"exceptionally steal full proof resource from %q after terminated predecessor %s; reason: %s; immutable termination evidence: %s",
			priorHolder,
			terminatedRunAttemptID,
			reason,
			terminationEvidenceRef,
		)
		rollback = "termination evidence and the predecessor terminal state remain auditable; the scarce holder stays closed on transfer failure"
		mutationClass = "security"
		reversibility = "manual"
		blastRadius = "repo"
	case strings.TrimSpace(reason) != "":
		action = proofResourceActionSteal
		preview = fmt.Sprintf("exceptionally steal full proof resource from %q; reason: %s", priorHolder, reason)
		rollback = "stolen predecessor remains auditable through reservation and RunAttempt evidence"
		mutationClass = "security"
		reversibility = "manual"
		blastRadius = "repo"
	}
	return envelope.ActionIntent{
		Header: newHeader(
			envelope.SVActionIntent,
			idFor(
				"act_",
				"proof.resource",
				action,
				w.ID,
				reservationID,
				actor,
				priorHolder,
				reason,
				terminatedRunAttemptID,
				terminationEvidenceRef,
				ts(now),
			),
			now,
			w.ID,
			w.IDs.ContextPackID,
			w.Privacy,
			w.Risk,
		),
		Actor:         actor,
		Capability:    "proof.resource",
		Action:        action,
		Target:        envelope.ActionTarget{Kind: "work", ID: w.ID, Path: s.workPath(w.ID)},
		MutationClass: mutationClass,
		Reversibility: reversibility,
		BlastRadius:   blastRadius,
		Preview:       preview,
		RollbackPlan:  rollback,
	}
}

func (s *Store) evaluateProofResourceAction(action envelope.ActionIntent, now time.Time) (envelope.PolicyDecision, error) {
	if err := action.Validate(); err != nil {
		return envelope.PolicyDecision{}, err
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return envelope.PolicyDecision{}, err
	}
	decision := policy.Evaluate(action, cfg, now)
	if err := decision.Validate(); err != nil {
		return envelope.PolicyDecision{}, err
	}
	return decision, nil
}

func (s *Store) completeProofAttempt(id, status, reason string, now time.Time) error {
	ra, err := s.runAttempts().Get(id)
	if err != nil {
		return err
	}
	if runattempt.IsTerminal(ra.Status) {
		if ra.Status == status {
			return nil
		}
		return fmt.Errorf("proof RunAttempt %s already terminal as %s, want %s", id, ra.Status, status)
	}
	_, err = s.runAttempts().CompleteWithReason(id, status, reason, now)
	return err
}

func (s *Store) terminalizeGovernedProofPredecessor(id, reason string, now time.Time) error {
	ra, err := s.runAttempts().Get(id)
	if err != nil {
		return fmt.Errorf("recover proof RunAttempt %s: %w", id, err)
	}
	if runattempt.IsTerminal(ra.Status) {
		return nil
	}
	_, err = s.runAttempts().CompleteWithReason(id, runattempt.StatusAbandoned, reason, now)
	return err
}

type proofRecoveryBlocker struct {
	ReservationID string
	RunAttemptID  string
}

func proofReservationPredecessorHolders(reservation ProofResourceReservation) ([]string, error) {
	holders := make([]string, 0, 2)
	for _, holder := range []string{reservation.RecoveredHolder, reservation.PriorHolder} {
		holder = strings.TrimSpace(holder)
		if holder == "" || oneOf(holder, holders) {
			continue
		}
		holders = append(holders, holder)
	}
	if len(holders) > 1 {
		return nil, fmt.Errorf(
			"proof resource reservation %s has ambiguous predecessor holders %q and %q",
			reservation.ID,
			holders[0],
			holders[1],
		)
	}
	return holders, nil
}

func (s *Store) proofRecoveryOldestNonterminal(holder string) (proofRecoveryBlocker, error) {
	return s.proofRecoveryOldestNonterminalFrom(holder, map[string]bool{})
}

func (s *Store) proofRecoveryOldestNonterminalFrom(
	holder string,
	path map[string]bool,
) (proofRecoveryBlocker, error) {
	reservationID, runID, _, ok := parseProofResourceHolder(holder)
	if !ok {
		return proofRecoveryBlocker{}, fmt.Errorf("proof resource predecessor holder %q is invalid", holder)
	}
	node := reservationID + ":" + runID
	if path[node] {
		return proofRecoveryBlocker{}, fmt.Errorf("proof resource predecessor lineage cycle at %s", node)
	}
	path[node] = true
	defer delete(path, node)

	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(reservationID, &reservation); err != nil {
		exists, existsErr := s.events.Exists(reservationID)
		if existsErr != nil {
			return proofRecoveryBlocker{}, errors.Join(err, existsErr)
		}
		if !exists {
			// A crash after the lease rename but before reservation promotion
			// leaves the exact holder-bound RunAttempt reachable from Work. No
			// runner was returned to the caller, so the missing node has no
			// executable descendants; recover that exact run fail-closed.
			ra, runErr := s.runAttempts().Get(runID)
			if runErr != nil {
				return proofRecoveryBlocker{}, errors.Join(err, runErr)
			}
			if !runattempt.IsTerminal(ra.Status) {
				return proofRecoveryBlocker{ReservationID: reservationID, RunAttemptID: runID}, nil
			}
			return proofRecoveryBlocker{}, nil
		}
		return proofRecoveryBlocker{}, fmt.Errorf("read proof resource predecessor reservation %s: %w", reservationID, err)
	}
	if reservation.ID != reservationID || reservation.RunAttemptID != runID {
		return proofRecoveryBlocker{}, fmt.Errorf(
			"proof resource predecessor holder %q does not match reservation %s run %s",
			holder,
			reservation.ID,
			reservation.RunAttemptID,
		)
	}
	predecessorHolders, err := proofReservationPredecessorHolders(reservation)
	if err != nil {
		return proofRecoveryBlocker{}, err
	}
	if len(predecessorHolders) == 1 {
		blocker, err := s.proofRecoveryOldestNonterminalFrom(predecessorHolders[0], path)
		if err != nil {
			return proofRecoveryBlocker{}, err
		}
		if blocker.RunAttemptID != "" {
			return blocker, nil
		}
	}
	ra, err := s.runAttempts().Get(runID)
	if err != nil {
		return proofRecoveryBlocker{}, fmt.Errorf("inspect recovered proof RunAttempt %s: %w", runID, err)
	}
	if !runattempt.IsTerminal(ra.Status) {
		return proofRecoveryBlocker{ReservationID: reservationID, RunAttemptID: runID}, nil
	}
	return proofRecoveryBlocker{}, nil
}

func proofResourceHolder(reservationID, runAttemptID, workID, actor string) string {
	return strings.Join([]string{"proof", reservationID, runAttemptID, workID, actor}, ":")
}

func parseProofResourceHolder(holder string) (reservationID, runAttemptID, workID string, ok bool) {
	parts := strings.SplitN(holder, ":", 5)
	if len(parts) != 5 ||
		parts[0] != "proof" ||
		!strings.HasPrefix(parts[1], "pfr_") ||
		!runattempt.Valid(parts[2]) ||
		!strings.HasPrefix(parts[3], "work_") {
		return "", "", "", false
	}
	return parts[1], parts[2], parts[3], true
}

func completionReason(status, failureReason string) string {
	if strings.TrimSpace(failureReason) != "" {
		return "proof resource failed: " + strings.TrimSpace(failureReason)
	}
	return "proof resource result " + status
}
