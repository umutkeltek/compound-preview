// Package-level governed execution substrate (slice 1): `compound work
// delegate|steer|attach`. The pure orchestrator lives here; the thin real
// tmux adapter lives in tmux_runner.go behind the SessionRunner seam.
//
// Design rules pinned by the packet "2026-07-20-governed-execution-delegate-
// steer-attach.md":
//   - NO new gate. Delegate routes through the same LaneCapacityGuard seam
//     dispatch_plan.go uses. A walled lane REFUSES even with --confirm; a
//     confirmation-required lane proceeds only with --confirm. A nil guard
//     fails CLOSED.
//   - tmux, wrapped. SessionRunner is an interface; only tmux_runner.go (in
//     this same allowed-file set) shells out. Pure orchestrator tests never
//     touch a real tmux server.
//   - One meaning owner. The binding (work ↔ tmux session) lives in the
//     EXISTING eventstore as a structured work.delegate.spawned record, NOT
//     as a sidecar file under COMPOUND_HOME. Recovery walks the work's
//     lifecycle attachments newest-first and reads the attached eventstore
//     objects through the same eventstore API. A later work.delegate.exited
//     record kills the binding; steer/attach probe SessionRunner.HasSession
//     regardless. A delegate RunAttempt is minted before the first spawn side
//     effect; its per-attempt launch boundary publishes running+prepared before
//     Create and committed after success. It is then heartbeated while live and
//     terminalized from an observed pane exit or disappearance.
//   - One live session per work. A second delegate on a live session
//     REFUSES with reason=session_live and the existing session returned.
//   - Lifecycle events, not logs. Each verb emits a structured eventstore
//     record under the current compound.work_delegate_event schema with kind ∈
//     work.delegate.{spawned,refused,steered,exited}. Event payloads NEVER
//     carry prompt text or command stdout (privacy law of the packet).
//   - Dry-run runs the guard + binding + tmux-available checks, prints the
//     decision, and writes NO event record (preview-only).
//   - Named lane aliases resolve through Compound's injected LaneTuple
//     resolver. Governed activation admits only an executable attested by the
//     readiness adapter; a raw override remains parseable for compatibility
//     but fails closed unless an authority can attest it.
package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"compound/internal/runattempt"
)

const (
	delegateEventSchemaVersion        = "compound.work_delegate_event.v0.2"
	delegateLegacyEventSchemaVersion  = "compound.work_delegate_event.v0.1"
	delegateSpawnFailureNote          = "session runner could not create the governed session"
	delegateAmbiguousNote             = "Create failed but the named session exists; classified as spawn_ambiguous (the session may be a stranger's); no kill performed"
	delegateProbeUnknownNote          = "Create failed and session ownership could not be determined; classified as spawn_ambiguous; no kill performed"
	delegateLaunchEvidenceFailureNote = "Create succeeded but committed launch evidence could not be published; the definitely-owned session was stopped and the attempt was terminalized"
	ExperimentalDelegateMutationsFlag = "--experimental-delegate-mutations"
)

var ErrExperimentalDelegateMutationsDisabled = fmt.Errorf(
	"delegate mutation is experimental and disabled; pass %s at the CLI composition root to opt in",
	ExperimentalDelegateMutationsFlag,
)

// EnableExperimentalDelegateMutations opts this Store instance into the
// unsupported delegated-mutation surface. The supported v0.1 execution path is
// work advance --execute; status, attach, and dry-run do not require this opt-in.
// The flag is process-local and must be selected explicitly for each CLI run.
func (s *Store) EnableExperimentalDelegateMutations() {
	if s != nil {
		s.experimentalDelegateMutations.Store(true)
	}
}

func (s *Store) requireExperimentalDelegateMutations() error {
	if s == nil || !s.experimentalDelegateMutations.Load() {
		return ErrExperimentalDelegateMutationsDisabled
	}
	return nil
}

type delegateAuthorityChangedError struct {
	detail string
}

func (e *delegateAuthorityChangedError) Error() string {
	return "delegate authority changed during admission: " + e.detail
}

func (s *Store) withDelegateLifecycleMutation(workID string, fn func() error) error {
	return s.withLifecycleMutation(func() error {
		if err := s.fenceClosePendingMutation(workID); err != nil {
			return &delegateAuthorityChangedError{detail: err.Error()}
		}
		if err := s.fenceSweepStalePendingMutation(workID); err != nil {
			return &delegateAuthorityChangedError{detail: err.Error()}
		}
		return fn()
	})
}

func (s *Store) fenceDelegateLifecycleMutation(workID string) error {
	return s.withDelegateLifecycleMutation(workID, func() error { return nil })
}

func delegateLifecycleMutationResult(out DelegateResult, err error) (DelegateResult, error) {
	var authorityChanged *delegateAuthorityChangedError
	if errors.As(err, &authorityChanged) {
		out.EventID = ""
		out.Refusal = &DelegateRefusal{
			Reason:  "authority_changed",
			Message: authorityChanged.Error(),
		}
		return out, nil
	}
	return out, err
}

type delegateExecutionProfileIncompatibleError struct {
	detail string
}

func (e *delegateExecutionProfileIncompatibleError) Error() string {
	return "delegate execution profile is incompatible: " + e.detail
}

type delegateLaunchProfileIncompatibleError struct {
	detail string
}

func (e *delegateLaunchProfileIncompatibleError) Error() string {
	return "delegate launch profile is incompatible: " + e.detail
}

// SessionStatus captures the supervised pane's liveness and (when
// available) its exit code/signal. The orchestrator treats this -- not bare
// Has -- as the source of truth for whether a previously-spawned session
// is still running, because tmux keeps dead sessions around with
// remain-on-exit.
//
// ExitCode is the observed exit status; -1 means the runner has no
// observable value yet (server crash, pane_dead set without metadata, the
// pane is live, etc). The orchestrator treats -1 as "unknown / cannot
// conclude exit" rather than guessing the value 0.
type SessionStatus struct {
	Exists   bool   `json:"exists"`           // session exists in the server's namespace
	PaneDead bool   `json:"pane_dead"`        // pane process is dead (remain-on-exit set)
	ExitCode int    `json:"exit_code"`        // observed exit status; -1 means unknown / not observed
	Signal   string `json:"signal,omitempty"` // non-empty when killed by a signal (e.g. "TERM")
	// UnknownReason is set when the runner cannot determine the status
	// (exec failure, malformed output, permissions, etc). The orchestrator
	// propagates the uncertainty; it never guesses a terminal status.
	UnknownReason string `json:"unknown_reason,omitempty"`
}

// SessionRunner is the seam between the orchestrator and any backing
// supervised-execution backend (tmux in production; a fake in tests).
type SessionRunner interface {
	Available() error
	Has(name string) (bool, error)
	SessionStatus(name string) (SessionStatus, error)
	Create(name, dir, command string, args []string) error
	SendKeys(name, text string, enter bool) error
	Kill(name string) error
	AttachArgv(name string) []string
}

// SessionEnvironmentRunner is the supervised-launch extension used when a
// delegated run carries canonical execution identity. Values remain argv/data
// at the runner boundary and are never rendered into a shell command.
type SessionEnvironmentRunner interface {
	CreateWithEnvironment(name, dir, command string, args []string, environment map[string]string) error
}

type DelegateOptions struct {
	WorkID           string
	Harness          string
	ExecutionProfile DelegateExecutionProfileIntent
	Session          string // optional override; default = delegateSessionName(WorkID)
	Actor            string
	Command          string
	Args             []string
	Workspace        string
	ExpectedMinutes  int
	Confirm          bool
	DryRun           bool
	Now              time.Time
}

type DelegateHarnessResolveRequest struct {
	HomePath        string
	Harness         string
	CommandOverride string
	Now             time.Time
}

type DelegateHarnessResolution struct {
	Harness                    string
	Command                    string
	LaneID                     string
	RequestedModelID           string
	RequestedProviderRoute     string
	RequestedProviderRouteKind string
	SelectedTransportID        string
	CapacityLane               string
	TupleBacked                bool
	CapacitySource             *LaneCapacitySource
}

type DelegateHarnessResolverFunc func(DelegateHarnessResolveRequest) (DelegateHarnessResolution, error)

// DelegateHarnessResolver is installed by routeevidence, the owner of the
// Compound model-router authority. Injection avoids a work↔routeevidence
// import cycle while keeping a single tuple resolver.
var DelegateHarnessResolver DelegateHarnessResolverFunc

// DelegateExecutionProfileIntent is the narrow selector DTO accepted by the
// work orchestrator. routeevidence owns validation and semantic resolution;
// this package intentionally does not import it.
type DelegateExecutionProfileIntent struct {
	ProfileID           string `json:"profile_id"`
	LaunchProfileID     string `json:"launch_profile_id,omitempty"`
	Role                string `json:"role,omitempty"`
	CapabilityClass     string `json:"capability_class,omitempty"`
	ClientHarnessID     string `json:"client_harness_id,omitempty"`
	TransportID         string `json:"transport_id,omitempty"`
	RouteAlias          string `json:"route_alias,omitempty"`
	ModelConstraint     string `json:"model_constraint,omitempty"`
	SemanticEffort      string `json:"semantic_effort,omitempty"`
	NativeEffort        string `json:"native_effort,omitempty"`
	OrchestrationPolicy string `json:"orchestration_policy,omitempty"`
	PrivacyCeiling      string `json:"privacy_ceiling,omitempty"`
}

const (
	delegateExecutionProfilePlanSchema       = "compound.execution_profile_plan.v0.1"
	delegateExecutionProfileSnapshotMaxBytes = 256 << 10
	delegateLaunchProfilePlanSchema          = "compound.delegate_launch_profile_plan.v0.2"
	delegateLegacyLaunchProfilePlanSchema    = "compound.delegate_launch_profile_plan.v0.1"
	delegateLaunchProfileSnapshotMaxBytes    = 64 << 10

	DelegateExecutionProfileCompatible   = "compatible"
	DelegateExecutionProfileIncompatible = "incompatible"
	DelegateLaunchProfileCompatible      = "compatible"
	DelegateLaunchProfileIncompatible    = "incompatible"
)

// DelegateExecutionProfilePlan is derived from the validated canonical
// routeevidence snapshot. Its flattened fields are the bounded requested
// identity projection used for bindings and RunAttempt stamping; callers may
// not supply them independently from Snapshot.
type DelegateExecutionProfilePlan struct {
	SchemaVersion           string          `json:"schema_version"`
	ID                      string          `json:"id"`
	ProfileID               string          `json:"profile_id"`
	Role                    string          `json:"role"`
	CapabilityClass         string          `json:"capability_class"`
	ClientHarnessID         string          `json:"client_harness_id"`
	TransportID             string          `json:"transport_id"`
	RouteAlias              string          `json:"route_alias,omitempty"`
	ModelConstraint         string          `json:"model_constraint,omitempty"`
	SemanticEffort          string          `json:"semantic_effort"`
	RequestedNativeEffort   string          `json:"requested_native_effort"`
	NativeEffortDisposition string          `json:"native_effort_disposition"`
	NativeEffortReason      string          `json:"native_effort_reason"`
	OrchestrationPolicy     string          `json:"orchestration_policy"`
	PrivacyCeiling          string          `json:"privacy_ceiling"`
	Budget                  json.RawMessage `json:"budget,omitempty"`
	Checkpoint              json.RawMessage `json:"checkpoint,omitempty"`
	ParentPlanID            string          `json:"parent_plan_id,omitempty"`
	RemainingBudget         json.RawMessage `json:"remaining_budget,omitempty"`
	InheritedCeilings       []string        `json:"inherited_ceilings,omitempty"`
	Warnings                []string        `json:"warnings,omitempty"`
	Snapshot                json.RawMessage `json:"snapshot"`
}

type DelegateExecutionProfileResolveRequest struct {
	Intent                DelegateExecutionProfileIntent
	SelectedHarness       string
	SelectedLaneID        string
	SelectedModelID       string
	SelectedProviderRoute string
	SelectedTransportID   string
}

type DelegateExecutionProfileCompatibility struct {
	Disposition           string `json:"disposition"`
	SelectedHarness       string `json:"selected_harness"`
	SelectedLaneID        string `json:"selected_lane_id"`
	SelectedModelID       string `json:"selected_model_id"`
	SelectedProviderRoute string `json:"selected_provider_route"`
	SelectedTransportID   string `json:"selected_transport_id"`
	PlanID                string `json:"plan_id"`
	ClientHarnessID       string `json:"client_harness_id"`
	TransportID           string `json:"transport_id"`
	RequestedNativeEffort string `json:"requested_native_effort"`
}

type DelegateExecutionProfileResolution struct {
	Snapshot      json.RawMessage                       `json:"snapshot"`
	Compatibility DelegateExecutionProfileCompatibility `json:"compatibility"`
}

type DelegateExecutionProfileResolverFunc func(DelegateExecutionProfileResolveRequest) (*DelegateExecutionProfileResolution, error)

// DelegateExecutionProfileResolver is installed by routeevidence, the owner of
// ExecutionProfile definitions and resolution. The seam avoids an import
// cycle and prevents a parallel profile registry in internal/work.
var DelegateExecutionProfileResolver DelegateExecutionProfileResolverFunc

type DelegateLaunchProfileResolveRequest struct {
	ProfileID                 string
	SelectedHarness           string
	SelectedLaneID            string
	SelectedModelID           string
	SelectedProviderRoute     string
	SelectedProviderRouteKind string
	SelectedTransportID       string
	RequestedNativeEffort     string
	SelectedCommand           string
}

type DelegateLaunchProfilePlan struct {
	SchemaVersion     string          `json:"schema_version"`
	SourceSchema      string          `json:"source_schema"`
	SourceDigest      string          `json:"source_digest"`
	AuthorityRevision string          `json:"authority_revision"`
	Revision          string          `json:"revision"`
	ID                string          `json:"id"`
	Harness           string          `json:"harness"`
	ProviderLane      string          `json:"provider_lane"`
	ProviderRouteKind string          `json:"provider_route_kind"`
	Route             string          `json:"route"`
	RequestedModel    string          `json:"requested_model"`
	RequestedEffort   string          `json:"requested_effort"`
	TransportID       string          `json:"transport_id"`
	OwnerTransport    string          `json:"owner_transport"`
	ClientProfileID   string          `json:"client_profile_id"`
	CaptureMode       string          `json:"capture_mode"`
	ExecutableID      string          `json:"executable_id"`
	ExecutablePath    string          `json:"-"`
	Snapshot          json.RawMessage `json:"snapshot,omitempty"`
}

type DelegateLaunchProfileCompatibility struct {
	Disposition               string `json:"disposition"`
	SelectedHarness           string `json:"selected_harness"`
	SelectedLaneID            string `json:"selected_lane_id"`
	SelectedModelID           string `json:"selected_model_id"`
	SelectedProviderRoute     string `json:"selected_provider_route"`
	SelectedProviderRouteKind string `json:"selected_provider_route_kind"`
	SelectedTransportID       string `json:"selected_transport_id"`
	RequestedNativeEffort     string `json:"requested_native_effort"`
	PlanID                    string `json:"plan_id"`
	ClientHarnessID           string `json:"client_harness_id"`
	ClientProfileID           string `json:"client_profile_id"`
	Revision                  string `json:"revision"`
	SelectedExecutableID      string `json:"selected_executable_id"`
	ExecutableID              string `json:"executable_id"`
}

type DelegateLaunchProfileResolution struct {
	Snapshot       json.RawMessage                    `json:"snapshot"`
	Compatibility  DelegateLaunchProfileCompatibility `json:"compatibility"`
	ExecutablePath string                             `json:"-"`
}

type DelegateLaunchProfileResolverFunc func(DelegateLaunchProfileResolveRequest) (*DelegateLaunchProfileResolution, error)

// DelegateLaunchProfileResolver is installed by the CLI edge that can ask the
// executor owner for `sb profile plan --json`. internal/work never reads or
// copies the executor's private launch-profile registry.
var DelegateLaunchProfileResolver DelegateLaunchProfileResolverFunc

type DelegateHarnessReadinessRequest struct {
	LaunchProfileID        string
	HomePath               string
	HarnessHome            string
	WorkID                 string
	ContextPackID          string
	Workspace              string
	Harness                string
	LaneID                 string
	RequestedModelID       string
	RequestedProviderRoute string
	Command                string
	Args                   []string
	CommandOverridden      bool
	Timeout                time.Duration // bounded run horizon, preserved through start revalidation
	Now                    time.Time
}

type DelegateHarnessReadinessEvidence struct {
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Revision string `json:"revision,omitempty"`
}

type DelegateExecutionProfileReadiness struct {
	Ref      string          `json:"ref"`
	Revision string          `json:"revision"`
	Snapshot json.RawMessage `json:"snapshot"`
}

type DelegateLaunchProfileReadiness struct {
	Ref      string          `json:"ref"`
	Revision string          `json:"revision"`
	Snapshot json.RawMessage `json:"snapshot"`
}

type DelegateHarnessReadinessResult struct {
	Ready            bool                               `json:"ready"`
	SnapshotRevision string                             `json:"snapshot_revision,omitempty"`
	Details          []string                           `json:"details,omitempty"`
	Evidence         []DelegateHarnessReadinessEvidence `json:"evidence,omitempty"`
	ExecutionProfile *DelegateExecutionProfileReadiness `json:"execution_profile,omitempty"`
	LaunchProfile    *DelegateLaunchProfileReadiness    `json:"launch_profile,omitempty"`
}

type DelegateHarnessReadinessFunc func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error)

// DelegateHarnessReadiness is installed by the executable composition root.
// It reads the permission, hook, and instruction authorities without mutating
// them. A missing guard fails closed: tuple/executable/capacity evidence alone
// is not proof that a harness is safe to activate.
var DelegateHarnessReadiness DelegateHarnessReadinessFunc

type DelegateResult struct {
	SchemaVersion    string                          `json:"schema_version"`
	WorkID           string                          `json:"work_id"`
	Lane             string                          `json:"lane,omitempty"`
	Status           string                          `json:"status"` // spawned|refused|preview
	Binding          *DelegateBinding                `json:"binding,omitempty"`
	Refusal          *DelegateRefusal                `json:"refusal,omitempty"`
	Readiness        *DelegateHarnessReadinessResult `json:"readiness,omitempty"`
	Decision         *LaneCapacityGuardResult        `json:"decision,omitempty"`
	ExecutionProfile *DelegateExecutionProfilePlan   `json:"execution_profile,omitempty"`
	LaunchProfile    *DelegateLaunchProfilePlan      `json:"launch_profile,omitempty"`
	// RunAttemptID is the ratt_<UTC>_<hex> that was minted before Create
	// and marked running after the supervised spawn succeeded. Always
	// populated on Status="spawned"; older callers that recover a
	// pre-slice binding decode this as empty.
	RunAttemptID string `json:"run_attempt_id,omitempty"`
	EventID      string `json:"event_id,omitempty"`
	GeneratedAt  string `json:"generated_at"`
}

// DelegateBinding is the SYNTHESIZED view of the bound session. It is built
// from the newest attached work.delegate.spawned eventstore record (not
// from a sidecar file). The orchestrator returns it to the verb layer so
// the operator can print a single "session: <name>" hint.
type DelegateBinding struct {
	WorkID                    string                          `json:"work_id"`
	Session                   string                          `json:"session"`
	Harness                   string                          `json:"harness"`
	Lane                      string                          `json:"lane,omitempty"`
	Holder                    string                          `json:"holder,omitempty"`
	CapacityState             string                          `json:"capacity_state,omitempty"`
	RecordedAt                string                          `json:"recorded_at,omitempty"`
	WorkSHA256                string                          `json:"work_sha256,omitempty"`
	SelectedProviderRoute     string                          `json:"selected_provider_route,omitempty"`
	SelectedProviderRouteKind string                          `json:"selected_provider_route_kind,omitempty"`
	Readiness                 *DelegateHarnessReadinessResult `json:"readiness,omitempty"`
	ExecutionProfile          *DelegateExecutionProfilePlan   `json:"execution_profile,omitempty"`
	LaunchProfile             *DelegateLaunchProfilePlan      `json:"launch_profile,omitempty"`
	// RunAttemptID is the ratt_<UTC>_<hex> the orchestrator minted BEFORE
	// the supervised spawn. Older bindings (pre-slice) leave it empty
	// and remain backward-compatible: recovery still returns the
	// binding, sweep falls back to "no probe target".
	RunAttemptID string `json:"run_attempt_id,omitempty"`
}

type DelegateRefusal struct {
	Reason  string `json:"reason"` // readiness_unmet|authority_changed|walled|dead|expiring_unconfirmed|not_claimed|session_live|tmux_absent|gate_absent|harness_unknown|mint_failed|spawn_failed
	Message string `json:"message,omitempty"`
	Session string `json:"session,omitempty"` // session_live: the existing session name
}

func delegateSessionName(workID string) string { return "compound-" + workID }

func resolveDelegateSessionName(workID, override string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	return delegateSessionName(workID)
}

// delegateEvent is the structured record stored in the existing eventstore
// under the versioned compound.work_delegate_event schema. It is the slice-1 single
// meaning owner for the binding (work ↔ tmux session).
//
// RunAttemptID carries the exact ratt_<UTC>_<hex> the orchestrator minted
// BEFORE the first supervised spawn side effect, so a later recovery or
// sweep can resolve from this binding back to its attempt without a side
// index. Older records (no RunAttemptID field) decode as empty string and
// remain backward-compatible: recovery still returns the binding, and
// sweep falls back to "no probe target".
//
// ExitCode / Signal carry the OBSERVED exit when present on a
// work.delegate.exited event. Older exited events (no fields) still
// serialize cleanly: the orchestrator accepts either shape.
type delegateEvent struct {
	SchemaVersion             string                          `json:"schema_version"`
	ID                        string                          `json:"id"`
	Kind                      string                          `json:"kind"`
	TS                        string                          `json:"ts"`
	WorkID                    string                          `json:"work_id"`
	Harness                   string                          `json:"harness"`
	Lane                      string                          `json:"lane,omitempty"`
	Holder                    string                          `json:"holder,omitempty"`
	CapacityState             string                          `json:"capacity_state,omitempty"`
	Reason                    string                          `json:"reason,omitempty"`
	Message                   string                          `json:"message,omitempty"`
	Confirm                   bool                            `json:"confirm,omitempty"`
	Session                   string                          `json:"session,omitempty"`
	WorkSHA256                string                          `json:"work_sha256,omitempty"`
	SelectedProviderRoute     string                          `json:"selected_provider_route,omitempty"`
	SelectedProviderRouteKind string                          `json:"selected_provider_route_kind,omitempty"`
	RunAttemptID              string                          `json:"run_attempt_id,omitempty"`
	Readiness                 *DelegateHarnessReadinessResult `json:"readiness,omitempty"`
	ExecutionProfile          *DelegateExecutionProfilePlan   `json:"execution_profile,omitempty"`
	LaunchProfile             *DelegateLaunchProfilePlan      `json:"launch_profile,omitempty"`
	ExitCode                  *int                            `json:"exit_code,omitempty"`
	Signal                    string                          `json:"signal,omitempty"`
	ObservedExit              bool                            `json:"observed_exit,omitempty"`
}

func delegateEventID(kind, workID string, now time.Time) string {
	h := sha256.Sum256([]byte(strings.Join([]string{kind, workID, ts(now)}, "|")))
	return "wde_" + hex.EncodeToString(h[:])[:16]
}

func delegateBindingEventID(kind, workID string, b *DelegateBinding, now time.Time) string {
	identity := ""
	if b != nil {
		identity = strings.Join([]string{b.Session, b.RunAttemptID}, "|")
	}
	h := sha256.Sum256([]byte(strings.Join([]string{kind, workID, identity, ts(now)}, "|")))
	return "wde_" + hex.EncodeToString(h[:])[:16]
}

// latestDelegateBinding walks the work's lifecycle events oldest-first,
// reading every attached wde_* record through the existing eventstore API,
// and synthesizes the latest binding. A work.delegate.spawned sets the
// binding (and copies RunAttemptID so terminalization is keyed to the
// exact ratt); a work.delegate.exited retains that identity but marks it
// inactive so a partial cleanup can be retried without inventing a second
// exit event. recoverBoundSession wraps this helper and exposes only active
// bindings. Callers still probe SessionRunner.SessionStatus because the
// runner is the process-liveness source of truth.
//
// Returns (nil, false, nil) when no binding has ever existed. The second
// return is the eventlog-alive flag; SessionStatus at use time may still
// differ (pane dead under remain-on-exit).
func (s *Store) latestDelegateBinding(workID string) (*DelegateBinding, bool, error) {
	events, err := s.eventsFor(workID)
	if err != nil {
		return nil, false, err
	}
	return s.latestDelegateBindingFromEvents(workID, events, nil)
}

// latestDelegateBindingFromEvents folds an already-owned lifecycle snapshot.
// Batch callers must not reread the entire work log once per candidate. The
// optional observer sees validated spawned identities, including superseded
// ones; it cannot alter which binding the lifecycle fold makes current.
func (s *Store) latestDelegateBindingFromEvents(workID string, events []Event, observeSpawn func(DelegateBinding)) (*DelegateBinding, bool, error) {
	var binding *DelegateBinding
	alive := false
	for _, e := range events {
		for _, att := range e.Attached {
			if !strings.HasPrefix(att, "wde_") {
				continue
			}
			var dev delegateEvent
			if rerr := s.readEventJSON(att, &dev); rerr != nil {
				return nil, false, fmt.Errorf("recover delegate binding %s: %w", att, rerr)
			}
			if verr := validateAttachedDelegateEvent(att, workID, dev); verr != nil {
				return nil, false, fmt.Errorf("recover delegate binding %s: %w", att, verr)
			}
			switch dev.Kind {
			case "work.delegate.spawned":
				binding = &DelegateBinding{
					WorkID:                    dev.WorkID,
					Session:                   dev.Session,
					Harness:                   dev.Harness,
					Lane:                      dev.Lane,
					Holder:                    dev.Holder,
					CapacityState:             dev.CapacityState,
					RecordedAt:                dev.TS,
					WorkSHA256:                dev.WorkSHA256,
					SelectedProviderRoute:     dev.SelectedProviderRoute,
					SelectedProviderRouteKind: dev.SelectedProviderRouteKind,
					Readiness:                 dev.Readiness,
					ExecutionProfile:          cloneDelegateExecutionProfilePlan(dev.ExecutionProfile),
					LaunchProfile:             cloneDelegateLaunchProfilePlan(dev.LaunchProfile),
					// Older spawned events (pre-slice) decode with empty
					// RunAttemptID; the rest of the orchestrator treats
					// empty as "no exact attempt" and stays backward-
					// compatible (terminalization is skipped; console
					// shows an empty ratt).
					RunAttemptID: dev.RunAttemptID,
				}
				if observeSpawn != nil {
					observeSpawn(*binding)
				}
				alive = true
			case "work.delegate.exited":
				alive = false
			case "work.delegate.refused", "work.delegate.steered":
				// These are binding-neutral but still valid delegate events.
			}
		}
	}
	return binding, alive, nil
}

func (s *Store) recoverBoundSession(workID string) (*DelegateBinding, bool, error) {
	binding, alive, err := s.latestDelegateBinding(workID)
	if err != nil || !alive {
		return nil, false, err
	}
	return binding, true, nil
}

// recordedDelegateExit returns the latest exit attached after the matching
// spawned binding. Matching the exact RunAttemptID prevents an old exit from
// a reused session name from making a new generation look finalized. Legacy
// bindings without a ratt fall back to session identity and reset on the next
// matching spawn.
func (s *Store) recordedDelegateExit(workID string, b *DelegateBinding) (*delegateEvent, error) {
	events, err := s.eventsFor(workID)
	if err != nil {
		return nil, err
	}
	var matchedSpawn bool
	var found *delegateEvent
	for _, e := range events {
		for _, att := range e.Attached {
			if !strings.HasPrefix(att, "wde_") {
				continue
			}
			var dev delegateEvent
			if err := s.readEventJSON(att, &dev); err != nil {
				return nil, fmt.Errorf("recover delegate exit %s: %w", att, err)
			}
			if err := validateAttachedDelegateEvent(att, workID, dev); err != nil {
				return nil, fmt.Errorf("recover delegate exit %s: %w", att, err)
			}
			identityMatches := dev.Session == b.Session && (b.RunAttemptID == "" || dev.RunAttemptID == b.RunAttemptID)
			switch dev.Kind {
			case "work.delegate.spawned":
				if identityMatches {
					matchedSpawn = true
					found = nil
				}
			case "work.delegate.exited":
				if matchedSpawn && identityMatches {
					copy := dev
					found = &copy
				}
			}
		}
	}
	return found, nil
}

func validateAttachedDelegateEvent(eventID, workID string, event delegateEvent) error {
	if !validDelegateEventSchemaVersion(event.SchemaVersion) || event.ID != eventID || event.WorkID != workID {
		return errors.New("invalid delegate event identity")
	}
	switch event.Kind {
	case "work.delegate.spawned":
		if strings.TrimSpace(event.Session) == "" || strings.TrimSpace(event.Harness) == "" {
			return errors.New("spawned delegate event requires session and harness")
		}
		if err := validateDelegateLaunchProfileBinding(event.LaunchProfile, event.Readiness); err != nil {
			return fmt.Errorf("invalid launch profile proof: %w", err)
		}
		if err := validateDelegateExecutionProfileBinding(event.ExecutionProfile, event.Readiness); err != nil {
			return fmt.Errorf("invalid execution profile proof: %w", err)
		}
		if event.LaunchProfile != nil &&
			event.ExecutionProfile != nil &&
			strings.TrimSpace(event.LaunchProfile.Harness) != strings.TrimSpace(event.ExecutionProfile.ClientHarnessID) {
			return fmt.Errorf(
				"launch profile client harness %q does not match execution profile client harness %q",
				event.LaunchProfile.Harness,
				event.ExecutionProfile.ClientHarnessID,
			)
		}
		if event.LaunchProfile != nil && event.SchemaVersion == delegateLegacyEventSchemaVersion {
			if event.LaunchProfile.SchemaVersion != delegateLegacyLaunchProfilePlanSchema {
				return fmt.Errorf(
					"legacy delegate event launch profile schema %q is invalid",
					event.LaunchProfile.SchemaVersion,
				)
			}
			if strings.TrimSpace(event.SelectedProviderRoute) != "" ||
				strings.TrimSpace(event.SelectedProviderRouteKind) != "" {
				return errors.New("legacy delegate event cannot carry independent provider route facts")
			}
		}
		if event.LaunchProfile != nil && event.SchemaVersion == delegateEventSchemaVersion {
			if event.LaunchProfile.SchemaVersion != delegateLaunchProfilePlanSchema {
				return fmt.Errorf(
					"current delegate event launch profile schema %q is invalid",
					event.LaunchProfile.SchemaVersion,
				)
			}
			selectedProviderRoute := strings.TrimSpace(event.SelectedProviderRoute)
			selectedProviderRouteKind := strings.TrimSpace(event.SelectedProviderRouteKind)
			if !validDelegateProviderRouteKind(selectedProviderRouteKind) {
				return fmt.Errorf(
					"launch profile selected provider route kind %q is invalid",
					event.SelectedProviderRouteKind,
				)
			}
			if strings.TrimSpace(event.LaunchProfile.ProviderRouteKind) != selectedProviderRouteKind {
				return fmt.Errorf(
					"launch profile provider route kind %q does not match selected provider route kind %q",
					event.LaunchProfile.ProviderRouteKind,
					event.SelectedProviderRouteKind,
				)
			}
			if !DelegateLaunchProfileRouteConforms(
				selectedProviderRouteKind,
				selectedProviderRoute,
				event.LaunchProfile.ProviderLane,
				event.LaunchProfile.Route,
			) {
				return fmt.Errorf(
					"launch profile selected provider route %q does not match sealed %s axis",
					event.SelectedProviderRoute,
					event.SelectedProviderRouteKind,
				)
			}
		}
	case "work.delegate.exited", "work.delegate.steered":
		if strings.TrimSpace(event.Session) == "" {
			return fmt.Errorf("%s delegate event requires session", event.Kind)
		}
	case "work.delegate.refused":
		if strings.TrimSpace(event.Reason) == "" {
			return errors.New("refused delegate event requires reason")
		}
	default:
		return fmt.Errorf("unknown kind %q", event.Kind)
	}
	return nil
}

func validDelegateEventSchemaVersion(version string) bool {
	switch version {
	case delegateEventSchemaVersion, delegateLegacyEventSchemaVersion:
		return true
	default:
		return false
	}
}

// runLaneCapacityGuard is the nil-safe wrapper. A nil guard returns a
// synthetic gate_absent verdict (D3 contract); any panic inside the
// underlying guard is converted to the same refusal envelope.
func runLaneCapacityGuard(guard LaneCapacityGuardFunc, req LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
	if guard == nil {
		return LaneCapacityGuardResult{
			Capacity: DispatchLaneCapacity{State: "absent"},
			Refused:  true,
			Message:  "no capacity gate installed (LaneCapacityGuard is nil)",
		}, nil
	}
	res, err := guard(req)
	if err != nil {
		return res, err
	}
	if res.Capacity.State == "" {
		res.Capacity.State = "unknown"
	}
	return res, nil
}

// completeDelegateReadinessSnapshot binds the harness-readiness evidence and
// the exact capacity verdict into one canonical revision. The resulting
// snapshot is persisted on the delegate binding; a second evaluation must
// produce the same revision immediately before RunAttempt mint.
//
// Only the capacity VERDICT is folded in — state, refusal, whether
// confirmation is required, and the reason. The live measurements behind it
// (remaining share, reset time, last-proven stamp, data ref) are deliberately
// excluded: they are re-read from usage history on every evaluation and move
// on their own while the orchestrator spends tokens, so hashing them made the
// admission-time and pre-mint revisions differ for a lane that never changed
// its verdict. Every spawn on an actively-used account then refused as
// `authority_changed`. The measurements stay on the evidence entry for
// observability; they are evidence, not authority.
//
// The guard's Message is excluded for the same reason and is NOT a weaker
// claim than excluding the fields directly: capacityDecisionMessage formats
// `data_row_timestamp=<LastProven>` and `data_ref=<...updated_at=...>` into the
// prose, so hashing the string smuggles the identical live meter back in. For a
// lane whose capacity comes from a live probe that restamps per second, that
// re-breaks the spawn on exactly the states where Message is populated —
// `expiring` and `unknown`, which are the states where --confirm is load-bearing
// and the run still proceeds to mint. State, refusal and confirmation-required
// already carry the whole actionable verdict; the prose only explains it.
func completeDelegateReadinessSnapshot(readiness DelegateHarnessReadinessResult, lane string, decision LaneCapacityGuardResult) (DelegateHarnessReadinessResult, error) {
	capacityMaterial, err := json.Marshal(struct {
		Lane                 string `json:"lane"`
		State                string `json:"state"`
		Refused              bool   `json:"refused"`
		ConfirmationRequired bool   `json:"confirmation_required"`
	}{
		Lane:                 strings.TrimSpace(lane),
		State:                strings.TrimSpace(decision.Capacity.State),
		Refused:              decision.Refused,
		ConfirmationRequired: decision.ConfirmationRequired,
	})
	if err != nil {
		return DelegateHarnessReadinessResult{}, fmt.Errorf("delegate: encode capacity readiness: %w", err)
	}
	capacitySum := sha256.Sum256(capacityMaterial)
	evidence := append([]DelegateHarnessReadinessEvidence(nil), readiness.Evidence...)
	evidence = append(evidence, DelegateHarnessReadinessEvidence{
		Kind:     "lane_capacity",
		Ref:      "lane-capacity://" + strings.TrimSpace(lane) + "/" + strings.TrimSpace(decision.Capacity.State),
		Revision: "sha256:" + hex.EncodeToString(capacitySum[:]),
	})
	readiness.Evidence = evidence
	return finalizeDelegateReadinessSnapshot(readiness)
}

func finalizeDelegateReadinessSnapshot(readiness DelegateHarnessReadinessResult) (DelegateHarnessReadinessResult, error) {
	evidence := append([]DelegateHarnessReadinessEvidence(nil), readiness.Evidence...)
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].Kind != evidence[j].Kind {
			return evidence[i].Kind < evidence[j].Kind
		}
		if evidence[i].Ref != evidence[j].Ref {
			return evidence[i].Ref < evidence[j].Ref
		}
		return evidence[i].Revision < evidence[j].Revision
	})
	details := append([]string(nil), readiness.Details...)
	sort.Strings(details)
	var executionProfile *DelegateExecutionProfileReadiness
	if readiness.ExecutionProfile != nil {
		copied := *readiness.ExecutionProfile
		copied.Snapshot = append(json.RawMessage(nil), readiness.ExecutionProfile.Snapshot...)
		executionProfile = &copied
	}
	var launchProfile *DelegateLaunchProfileReadiness
	if readiness.LaunchProfile != nil {
		copied := *readiness.LaunchProfile
		copied.Snapshot = append(json.RawMessage(nil), readiness.LaunchProfile.Snapshot...)
		launchProfile = &copied
	}
	snapshotMaterial, err := json.Marshal(struct {
		Ready            bool                               `json:"ready"`
		Details          []string                           `json:"details,omitempty"`
		Evidence         []DelegateHarnessReadinessEvidence `json:"evidence"`
		ExecutionProfile *DelegateExecutionProfileReadiness `json:"execution_profile,omitempty"`
		LaunchProfile    *DelegateLaunchProfileReadiness    `json:"launch_profile,omitempty"`
	}{
		Ready:            readiness.Ready,
		Details:          details,
		Evidence:         evidence,
		ExecutionProfile: executionProfile,
		LaunchProfile:    launchProfile,
	})
	if err != nil {
		return DelegateHarnessReadinessResult{}, fmt.Errorf("delegate: encode readiness snapshot: %w", err)
	}
	snapshotSum := sha256.Sum256(snapshotMaterial)
	readiness.Details = details
	readiness.Evidence = evidence
	readiness.ExecutionProfile = executionProfile
	readiness.LaunchProfile = launchProfile
	readiness.SnapshotRevision = "sha256:" + hex.EncodeToString(snapshotSum[:])
	return readiness, nil
}

// Delegate is the governed execution entry: routes through the existing
// LaneCapacityGuard (or refuses closed when no guard is installed),
// refuses on walled/death, requires --confirm on expiring/unknown, then
// records a work.delegate.spawned eventstore record + a work lifecycle
// event that attaches it. Dry-run runs the gate without spawning and
// writes no event.
func delegateExecutionProfileSelected(intent DelegateExecutionProfileIntent) bool {
	return strings.TrimSpace(intent.ProfileID) != "" ||
		strings.TrimSpace(intent.Role) != "" ||
		strings.TrimSpace(intent.CapabilityClass) != "" ||
		strings.TrimSpace(intent.ClientHarnessID) != "" ||
		strings.TrimSpace(intent.TransportID) != "" ||
		strings.TrimSpace(intent.RouteAlias) != "" ||
		strings.TrimSpace(intent.ModelConstraint) != "" ||
		strings.TrimSpace(intent.SemanticEffort) != "" ||
		strings.TrimSpace(intent.NativeEffort) != "" ||
		strings.TrimSpace(intent.OrchestrationPolicy) != "" ||
		strings.TrimSpace(intent.PrivacyCeiling) != ""
}

func cloneDelegateExecutionProfilePlan(plan *DelegateExecutionProfilePlan) *DelegateExecutionProfilePlan {
	if plan == nil {
		return nil
	}
	cloned := *plan
	cloned.Budget = append(json.RawMessage(nil), plan.Budget...)
	cloned.Checkpoint = append(json.RawMessage(nil), plan.Checkpoint...)
	cloned.RemainingBudget = append(json.RawMessage(nil), plan.RemainingBudget...)
	cloned.InheritedCeilings = append([]string(nil), plan.InheritedCeilings...)
	cloned.Warnings = append([]string(nil), plan.Warnings...)
	cloned.Snapshot = append(json.RawMessage(nil), plan.Snapshot...)
	return &cloned
}

func cloneDelegateLaunchProfilePlan(plan *DelegateLaunchProfilePlan) *DelegateLaunchProfilePlan {
	if plan == nil {
		return nil
	}
	cloned := *plan
	cloned.Snapshot = append(json.RawMessage(nil), plan.Snapshot...)
	return &cloned
}

type delegateExecutionProfileCanonicalBody struct {
	SchemaVersion       string                                        `json:"schema_version"`
	ID                  string                                        `json:"id"`
	ProfileID           string                                        `json:"profile_id"`
	Role                string                                        `json:"role"`
	CapabilityClass     string                                        `json:"capability_class"`
	ClientHarnessID     string                                        `json:"client_harness_id"`
	TransportID         string                                        `json:"transport_id"`
	RouteAlias          string                                        `json:"route_alias,omitempty"`
	ModelConstraint     string                                        `json:"model_constraint,omitempty"`
	SemanticEffort      string                                        `json:"semantic_effort"`
	NativeEffort        delegateExecutionProfileCanonicalNativeEffort `json:"native_effort"`
	OrchestrationPolicy string                                        `json:"orchestration_policy"`
	PrivacyCeiling      string                                        `json:"privacy_ceiling"`
	Budget              json.RawMessage                               `json:"budget,omitempty"`
	Checkpoint          json.RawMessage                               `json:"checkpoint,omitempty"`
	ParentPlanID        string                                        `json:"parent_plan_id,omitempty"`
	RemainingBudget     json.RawMessage                               `json:"remaining_budget,omitempty"`
	InheritedCeilings   []string                                      `json:"inherited_ceilings,omitempty"`
	Warnings            []string                                      `json:"warnings,omitempty"`
}

type delegateExecutionProfileCanonicalNativeEffort struct {
	Requested   string `json:"requested"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

func delegateExecutionProfilePlanFromSnapshot(snapshot json.RawMessage) (*DelegateExecutionProfilePlan, error) {
	if len(snapshot) == 0 {
		return nil, errors.New("canonical execution profile snapshot is required")
	}
	if len(snapshot) > delegateExecutionProfileSnapshotMaxBytes {
		return nil, fmt.Errorf("canonical execution profile snapshot exceeds %d bytes", delegateExecutionProfileSnapshotMaxBytes)
	}
	if !json.Valid(snapshot) {
		return nil, errors.New("canonical execution profile snapshot is invalid JSON")
	}
	var canonical delegateExecutionProfileCanonicalBody
	if err := json.Unmarshal(snapshot, &canonical); err != nil {
		return nil, fmt.Errorf("decode canonical execution profile snapshot: %w", err)
	}
	if canonical.SchemaVersion != delegateExecutionProfilePlanSchema {
		return nil, fmt.Errorf("canonical execution profile schema_version = %q, want %q", canonical.SchemaVersion, delegateExecutionProfilePlanSchema)
	}
	for name, value := range map[string]string{
		"id":                      canonical.ID,
		"profile_id":              canonical.ProfileID,
		"client_harness_id":       canonical.ClientHarnessID,
		"transport_id":            canonical.TransportID,
		"native_effort.requested": canonical.NativeEffort.Requested,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("canonical execution profile %s is required", name)
		}
	}
	return &DelegateExecutionProfilePlan{
		SchemaVersion:           canonical.SchemaVersion,
		ID:                      canonical.ID,
		ProfileID:               canonical.ProfileID,
		Role:                    canonical.Role,
		CapabilityClass:         canonical.CapabilityClass,
		ClientHarnessID:         canonical.ClientHarnessID,
		TransportID:             canonical.TransportID,
		RouteAlias:              canonical.RouteAlias,
		ModelConstraint:         canonical.ModelConstraint,
		SemanticEffort:          canonical.SemanticEffort,
		RequestedNativeEffort:   canonical.NativeEffort.Requested,
		NativeEffortDisposition: canonical.NativeEffort.Disposition,
		NativeEffortReason:      canonical.NativeEffort.Reason,
		OrchestrationPolicy:     canonical.OrchestrationPolicy,
		PrivacyCeiling:          canonical.PrivacyCeiling,
		Budget:                  append(json.RawMessage(nil), canonical.Budget...),
		Checkpoint:              append(json.RawMessage(nil), canonical.Checkpoint...),
		ParentPlanID:            canonical.ParentPlanID,
		RemainingBudget:         append(json.RawMessage(nil), canonical.RemainingBudget...),
		InheritedCeilings:       append([]string(nil), canonical.InheritedCeilings...),
		Warnings:                append([]string(nil), canonical.Warnings...),
		Snapshot:                append(json.RawMessage(nil), snapshot...),
	}, nil
}

func validateDelegateExecutionProfileResolution(
	resolution *DelegateExecutionProfileResolution,
	selectedHarness string,
	selectedLaneID string,
	selectedModelID string,
	selectedProviderRoute string,
	selectedTransportID string,
) (*DelegateExecutionProfilePlan, error) {
	if resolution == nil {
		return nil, errors.New("execution profile resolver returned no resolution")
	}
	plan, err := delegateExecutionProfilePlanFromSnapshot(resolution.Snapshot)
	if err != nil {
		return nil, err
	}
	compatibility := resolution.Compatibility
	if compatibility.SelectedHarness != selectedHarness {
		return nil, fmt.Errorf("compatibility selected_harness = %q, want %q", compatibility.SelectedHarness, selectedHarness)
	}
	if compatibility.SelectedLaneID != selectedLaneID {
		return nil, fmt.Errorf("compatibility selected_lane_id = %q, want %q", compatibility.SelectedLaneID, selectedLaneID)
	}
	for _, field := range []struct {
		name string
		got  string
		want string
	}{
		{name: "selected_model_id", got: compatibility.SelectedModelID, want: selectedModelID},
		{name: "selected_provider_route", got: compatibility.SelectedProviderRoute, want: selectedProviderRoute},
		{name: "selected_transport_id", got: compatibility.SelectedTransportID, want: selectedTransportID},
		{name: "plan_id", got: compatibility.PlanID, want: plan.ID},
		{name: "client_harness_id", got: compatibility.ClientHarnessID, want: plan.ClientHarnessID},
		{name: "transport_id", got: compatibility.TransportID, want: plan.TransportID},
		{name: "requested_native_effort", got: compatibility.RequestedNativeEffort, want: plan.RequestedNativeEffort},
	} {
		if field.got != field.want {
			return nil, fmt.Errorf("compatibility %s = %q, canonical snapshot has %q", field.name, field.got, field.want)
		}
	}
	switch compatibility.Disposition {
	case DelegateExecutionProfileCompatible:
		return plan, nil
	case DelegateExecutionProfileIncompatible:
		return nil, &delegateExecutionProfileIncompatibleError{
			detail: fmt.Sprintf("selected harness/model/route/transport tuple is not compatible with execution profile %q", plan.ID),
		}
	default:
		return nil, fmt.Errorf("compatibility disposition = %q", compatibility.Disposition)
	}
}

func delegateLaunchProfilePlanFromSnapshot(snapshot json.RawMessage) (*DelegateLaunchProfilePlan, error) {
	if len(snapshot) == 0 {
		return nil, errors.New("canonical launch profile snapshot is required")
	}
	if len(snapshot) > delegateLaunchProfileSnapshotMaxBytes {
		return nil, fmt.Errorf("canonical launch profile snapshot exceeds %d bytes", delegateLaunchProfileSnapshotMaxBytes)
	}
	if !json.Valid(snapshot) {
		return nil, errors.New("canonical launch profile snapshot is invalid JSON")
	}
	var canonical DelegateLaunchProfilePlan
	if err := json.Unmarshal(snapshot, &canonical); err != nil {
		return nil, fmt.Errorf("decode canonical launch profile snapshot: %w", err)
	}
	switch canonical.SchemaVersion {
	case delegateLaunchProfilePlanSchema, delegateLegacyLaunchProfilePlanSchema:
	default:
		return nil, fmt.Errorf(
			"canonical launch profile schema_version = %q, want %q or %q",
			canonical.SchemaVersion,
			delegateLaunchProfilePlanSchema,
			delegateLegacyLaunchProfilePlanSchema,
		)
	}
	for name, value := range map[string]string{
		"source_schema":      canonical.SourceSchema,
		"source_digest":      canonical.SourceDigest,
		"authority_revision": canonical.AuthorityRevision,
		"revision":           canonical.Revision,
		"id":                 canonical.ID,
		"harness":            canonical.Harness,
		"provider_lane":      canonical.ProviderLane,
		"route":              canonical.Route,
		"requested_model":    canonical.RequestedModel,
		"requested_effort":   canonical.RequestedEffort,
		"transport_id":       canonical.TransportID,
		"owner_transport":    canonical.OwnerTransport,
		"client_profile_id":  canonical.ClientProfileID,
		"capture_mode":       canonical.CaptureMode,
		"executable_id":      canonical.ExecutableID,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("canonical launch profile %s is required", name)
		}
	}
	if canonical.SchemaVersion == delegateLaunchProfilePlanSchema &&
		!validDelegateProviderRouteKind(canonical.ProviderRouteKind) {
		return nil, fmt.Errorf(
			"canonical launch profile provider_route_kind %q is unknown",
			canonical.ProviderRouteKind,
		)
	}
	if canonical.SchemaVersion == delegateLegacyLaunchProfilePlanSchema &&
		strings.TrimSpace(canonical.ProviderRouteKind) != "" {
		return nil, errors.New("legacy canonical launch profile cannot carry provider_route_kind")
	}
	for name, value := range map[string]string{
		"source_digest":      canonical.SourceDigest,
		"authority_revision": canonical.AuthorityRevision,
		"revision":           canonical.Revision,
	} {
		if !validDelegateSHA256Revision(value) {
			return nil, fmt.Errorf("canonical launch profile %s is not a sha256 revision", name)
		}
	}
	canonical.Snapshot = append(json.RawMessage(nil), snapshot...)
	return &canonical, nil
}

func validDelegateSHA256Revision(value string) bool {
	digest := strings.TrimPrefix(strings.TrimSpace(value), "sha256:")
	return len(digest) == 64 && strings.IndexFunc(digest, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'))
	}) == -1
}

const (
	delegateProviderRouteKindProviderClass = "provider_class"
	delegateProviderRouteKindDirectModel   = "direct_model"
)

func validDelegateProviderRouteKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case delegateProviderRouteKindProviderClass, delegateProviderRouteKindDirectModel:
		return true
	default:
		return false
	}
}

// DelegateLaunchProfileRouteConforms applies only the route axis explicitly
// declared by Compound's lane tuple. It never infers the kind from a slash,
// requested_model, or either owner field.
func DelegateLaunchProfileRouteConforms(selectedProviderRouteKind, selectedProviderRoute, ownerProviderLane, ownerRoute string) bool {
	selectedProviderRoute = strings.TrimSpace(selectedProviderRoute)
	if selectedProviderRoute == "" {
		return false
	}
	switch strings.TrimSpace(selectedProviderRouteKind) {
	case delegateProviderRouteKindProviderClass:
		return selectedProviderRoute == strings.TrimSpace(ownerProviderLane)
	case delegateProviderRouteKindDirectModel:
		return selectedProviderRoute == strings.TrimSpace(ownerRoute)
	default:
		return false
	}
}

func validateDelegateLaunchProfileResolution(
	resolution *DelegateLaunchProfileResolution,
	executionClientHarnessID,
	selectedHarness,
	selectedLaneID,
	selectedModelID,
	selectedProviderRoute,
	selectedProviderRouteKind,
	selectedTransportID,
	requestedNativeEffort,
	selectedCommand string,
) (*DelegateLaunchProfilePlan, error) {
	if resolution == nil {
		return nil, errors.New("launch profile resolver returned no resolution")
	}
	plan, err := delegateLaunchProfilePlanFromSnapshot(resolution.Snapshot)
	if err != nil {
		return nil, err
	}
	if plan.SchemaVersion != delegateLaunchProfilePlanSchema {
		return nil, &delegateLaunchProfileIncompatibleError{
			detail: fmt.Sprintf(
				"launch profile schema %q cannot admit a new delegated run",
				plan.SchemaVersion,
			),
		}
	}
	compatibility := resolution.Compatibility
	for _, field := range []struct {
		name string
		got  string
		want string
	}{
		{name: "selected_harness", got: compatibility.SelectedHarness, want: selectedHarness},
		{name: "selected_lane_id", got: compatibility.SelectedLaneID, want: selectedLaneID},
		{name: "selected_model_id", got: compatibility.SelectedModelID, want: selectedModelID},
		{name: "selected_provider_route", got: compatibility.SelectedProviderRoute, want: selectedProviderRoute},
		{name: "selected_provider_route_kind", got: compatibility.SelectedProviderRouteKind, want: selectedProviderRouteKind},
		{name: "selected_transport_id", got: compatibility.SelectedTransportID, want: selectedTransportID},
		{name: "requested_native_effort", got: compatibility.RequestedNativeEffort, want: requestedNativeEffort},
		{name: "plan_id", got: compatibility.PlanID, want: plan.ID},
		{name: "client_harness_id", got: compatibility.ClientHarnessID, want: plan.Harness},
		{name: "client_profile_id", got: compatibility.ClientProfileID, want: plan.ClientProfileID},
		{name: "revision", got: compatibility.Revision, want: plan.Revision},
		{name: "selected_executable_id", got: compatibility.SelectedExecutableID, want: filepath.Base(strings.TrimSpace(selectedCommand))},
		{name: "executable_id", got: compatibility.ExecutableID, want: plan.ExecutableID},
	} {
		if strings.TrimSpace(field.got) != strings.TrimSpace(field.want) {
			return nil, fmt.Errorf(
				"launch compatibility %s = %q, canonical snapshot has %q",
				field.name,
				field.got,
				field.want,
			)
		}
	}
	if strings.TrimSpace(plan.Harness) != strings.TrimSpace(executionClientHarnessID) {
		return nil, &delegateLaunchProfileIncompatibleError{
			detail: fmt.Sprintf(
				"owner client harness %q does not match execution profile client harness %q",
				plan.Harness,
				executionClientHarnessID,
			),
		}
	}
	if strings.TrimSpace(plan.ProviderRouteKind) != strings.TrimSpace(selectedProviderRouteKind) {
		return nil, &delegateLaunchProfileIncompatibleError{
			detail: fmt.Sprintf(
				"sealed provider route kind %q does not match selected provider route kind %q",
				plan.ProviderRouteKind,
				selectedProviderRouteKind,
			),
		}
	}
	if !DelegateLaunchProfileRouteConforms(selectedProviderRouteKind, selectedProviderRoute, plan.ProviderLane, plan.Route) {
		return nil, &delegateLaunchProfileIncompatibleError{
			detail: fmt.Sprintf(
				"selected provider route %q does not match owner %s axis (provider lane %q, concrete route %q)",
				selectedProviderRoute,
				selectedProviderRouteKind,
				plan.ProviderLane,
				plan.Route,
			),
		}
	}
	for _, field := range []struct {
		name string
		got  string
		want string
	}{
		{name: "transport_id", got: plan.TransportID, want: selectedTransportID},
		{name: "requested_effort", got: plan.RequestedEffort, want: requestedNativeEffort},
		{name: "executable_id", got: plan.ExecutableID, want: filepath.Base(strings.TrimSpace(selectedCommand))},
	} {
		if strings.TrimSpace(field.got) != strings.TrimSpace(field.want) {
			return nil, &delegateLaunchProfileIncompatibleError{
				detail: fmt.Sprintf("%s %q does not match selected tuple %q", field.name, field.got, field.want),
			}
		}
	}
	executablePath := strings.TrimSpace(resolution.ExecutablePath)
	if executablePath == "" || executablePath != resolution.ExecutablePath ||
		!filepath.IsAbs(executablePath) || filepath.Base(executablePath) != plan.ExecutableID {
		return nil, errors.New("launch profile resolver returned no exact owner executable")
	}
	plan.ExecutablePath = executablePath
	switch compatibility.Disposition {
	case DelegateLaunchProfileCompatible:
		return plan, nil
	case DelegateLaunchProfileIncompatible:
		return nil, &delegateLaunchProfileIncompatibleError{
			detail: fmt.Sprintf("selected tuple is not compatible with launch profile %q", plan.ID),
		}
	default:
		return nil, fmt.Errorf("launch compatibility disposition = %q", compatibility.Disposition)
	}
}

func addDelegateExecutionProfileReadinessEvidence(readiness DelegateHarnessReadinessResult, plan *DelegateExecutionProfilePlan) (DelegateHarnessReadinessResult, error) {
	if plan == nil {
		return readiness, nil
	}
	validated, err := delegateExecutionProfilePlanFromSnapshot(plan.Snapshot)
	if err != nil {
		return DelegateHarnessReadinessResult{}, err
	}
	if !reflect.DeepEqual(plan, validated) {
		return DelegateHarnessReadinessResult{}, errors.New("execution profile projection disagrees with canonical snapshot")
	}
	sum := sha256.Sum256(plan.Snapshot)
	readiness.ExecutionProfile = &DelegateExecutionProfileReadiness{
		Ref:      "execution-profile://" + strings.TrimSpace(plan.ID),
		Revision: "sha256:" + hex.EncodeToString(sum[:]),
		Snapshot: append(json.RawMessage(nil), plan.Snapshot...),
	}
	return readiness, nil
}

func addDelegateLaunchProfileReadinessEvidence(readiness DelegateHarnessReadinessResult, plan *DelegateLaunchProfilePlan) (DelegateHarnessReadinessResult, error) {
	if plan == nil {
		return readiness, nil
	}
	validated, err := delegateLaunchProfilePlanFromSnapshot(plan.Snapshot)
	if err != nil {
		return DelegateHarnessReadinessResult{}, err
	}
	projection := cloneDelegateLaunchProfilePlan(plan)
	projection.ExecutablePath = ""
	if !reflect.DeepEqual(projection, validated) {
		return DelegateHarnessReadinessResult{}, errors.New("launch profile projection disagrees with canonical snapshot")
	}
	sum := sha256.Sum256(plan.Snapshot)
	readiness.LaunchProfile = &DelegateLaunchProfileReadiness{
		Ref:      "launch-profile://" + strings.TrimSpace(plan.ID),
		Revision: "sha256:" + hex.EncodeToString(sum[:]),
		Snapshot: append(json.RawMessage(nil), plan.Snapshot...),
	}
	return readiness, nil
}

func validateDelegateExecutionProfileBinding(plan *DelegateExecutionProfilePlan, readiness *DelegateHarnessReadinessResult) error {
	var proof *DelegateExecutionProfileReadiness
	if readiness != nil {
		proof = readiness.ExecutionProfile
		for _, evidence := range readiness.Evidence {
			if evidence.Kind == "execution_profile" {
				return errors.New("execution profile proof must use the typed readiness field")
			}
		}
	}
	if plan == nil {
		if proof != nil {
			return errors.New("execution profile readiness proof has no bound plan")
		}
		return nil
	}
	if proof == nil {
		return errors.New("bound execution profile has no typed readiness proof")
	}
	validated, err := delegateExecutionProfilePlanFromSnapshot(plan.Snapshot)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(plan, validated) {
		return errors.New("bound execution profile projection disagrees with canonical snapshot")
	}
	if !bytes.Equal(proof.Snapshot, plan.Snapshot) {
		return errors.New("execution profile readiness snapshot disagrees with bound plan")
	}
	if _, err := delegateExecutionProfilePlanFromSnapshot(proof.Snapshot); err != nil {
		return fmt.Errorf("execution profile readiness snapshot: %w", err)
	}
	wantRef := "execution-profile://" + strings.TrimSpace(plan.ID)
	if proof.Ref != wantRef {
		return fmt.Errorf("execution profile readiness ref = %q, want %q", proof.Ref, wantRef)
	}
	sum := sha256.Sum256(proof.Snapshot)
	wantRevision := "sha256:" + hex.EncodeToString(sum[:])
	if proof.Revision != wantRevision {
		return fmt.Errorf("execution profile readiness revision = %q, want %q", proof.Revision, wantRevision)
	}
	canonicalReadiness, err := finalizeDelegateReadinessSnapshot(*readiness)
	if err != nil {
		return err
	}
	if readiness.SnapshotRevision != canonicalReadiness.SnapshotRevision {
		return fmt.Errorf("execution profile readiness snapshot_revision = %q, want %q", readiness.SnapshotRevision, canonicalReadiness.SnapshotRevision)
	}
	return nil
}

func validateDelegateLaunchProfileBinding(plan *DelegateLaunchProfilePlan, readiness *DelegateHarnessReadinessResult) error {
	var proof *DelegateLaunchProfileReadiness
	if readiness != nil {
		proof = readiness.LaunchProfile
		for _, evidence := range readiness.Evidence {
			if evidence.Kind == "launch_profile" {
				return errors.New("launch profile proof must use typed readiness field")
			}
		}
	}
	if plan == nil {
		if proof != nil {
			return errors.New("launch profile readiness proof has no bound plan")
		}
		return nil
	}
	if proof == nil {
		return errors.New("bound launch profile has no typed readiness proof")
	}
	validated, err := delegateLaunchProfilePlanFromSnapshot(plan.Snapshot)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(plan, validated) {
		return errors.New("bound launch profile projection disagrees with canonical snapshot")
	}
	if !bytes.Equal(proof.Snapshot, plan.Snapshot) {
		return errors.New("launch profile readiness snapshot disagrees with bound plan")
	}
	if _, err := delegateLaunchProfilePlanFromSnapshot(proof.Snapshot); err != nil {
		return fmt.Errorf("launch profile readiness snapshot: %w", err)
	}
	wantRef := "launch-profile://" + strings.TrimSpace(plan.ID)
	if proof.Ref != wantRef {
		return fmt.Errorf("launch profile readiness ref = %q, want %q", proof.Ref, wantRef)
	}
	sum := sha256.Sum256(proof.Snapshot)
	wantRevision := "sha256:" + hex.EncodeToString(sum[:])
	if proof.Revision != wantRevision {
		return fmt.Errorf("launch profile readiness revision = %q, want %q", proof.Revision, wantRevision)
	}
	canonicalReadiness, err := finalizeDelegateReadinessSnapshot(*readiness)
	if err != nil {
		return err
	}
	if readiness.SnapshotRevision != canonicalReadiness.SnapshotRevision {
		return fmt.Errorf(
			"launch profile readiness snapshot_revision = %q, want %q",
			readiness.SnapshotRevision,
			canonicalReadiness.SnapshotRevision,
		)
	}
	return nil
}

func (s *Store) Delegate(opts DelegateOptions, runner SessionRunner) (DelegateResult, error) {
	out := DelegateResult{
		SchemaVersion: delegateEventSchemaVersion,
		WorkID:        strings.TrimSpace(opts.WorkID),
		Status:        "refused",
		GeneratedAt:   opts.Now.UTC().Format(time.RFC3339),
	}
	if out.WorkID == "" {
		return out, errors.New("delegate: WorkID is required")
	}
	if strings.TrimSpace(opts.Harness) == "" {
		return out, errors.New("delegate: Harness is required")
	}
	if runner == nil {
		return out, errors.New("delegate: SessionRunner is nil")
	}
	if !opts.DryRun {
		if err := s.requireExperimentalDelegateMutations(); err != nil {
			out.Refusal = &DelegateRefusal{Reason: "experimental_disabled", Message: err.Error()}
			return out, nil
		}
		if err := s.fenceDelegateLifecycleMutation(out.WorkID); err != nil {
			return delegateLifecycleMutationResult(out, err)
		}
	}
	resolution := DelegateHarnessResolution{
		Harness:      strings.TrimSpace(opts.Harness),
		Command:      strings.TrimSpace(opts.Command),
		LaneID:       strings.TrimSpace(opts.Harness),
		CapacityLane: strings.TrimSpace(opts.Harness),
	}
	harnessResolver := DelegateHarnessResolver
	harnessResolveRequest := DelegateHarnessResolveRequest{
		HomePath: s.home, Harness: opts.Harness, CommandOverride: opts.Command, Now: opts.Now,
	}
	if harnessResolver != nil {
		resolved, resolveErr := harnessResolver(harnessResolveRequest)
		if resolveErr != nil {
			out.Refusal = &DelegateRefusal{Reason: "harness_unknown", Message: resolveErr.Error()}
			return out, nil
		}
		resolution = resolved
	}
	resolvedHarness := strings.TrimSpace(resolution.Harness)
	command := strings.TrimSpace(resolution.Command)
	laneForGuard := strings.TrimSpace(resolution.CapacityLane)
	selectedModelID := strings.TrimSpace(resolution.RequestedModelID)
	selectedProviderRoute := strings.TrimSpace(resolution.RequestedProviderRoute)
	selectedProviderRouteKind := strings.TrimSpace(resolution.RequestedProviderRouteKind)
	selectedTransportID := strings.TrimSpace(resolution.SelectedTransportID)
	if laneForGuard == "" {
		laneForGuard = strings.TrimSpace(resolution.LaneID)
	}
	if resolvedHarness == "" || command == "" || laneForGuard == "" {
		out.Refusal = &DelegateRefusal{
			Reason:  "harness_unknown",
			Message: fmt.Sprintf("harness %q did not resolve a complete executable tuple", opts.Harness),
		}
		return out, nil
	}
	if resolution.TupleBacked && selectedProviderRoute == "" {
		out.Refusal = &DelegateRefusal{
			Reason:  "harness_unknown",
			Message: fmt.Sprintf("harness %q resolved a tuple without a provider route", opts.Harness),
		}
		return out, nil
	}
	if resolution.TupleBacked &&
		selectedProviderRouteKind != "" &&
		!validDelegateProviderRouteKind(selectedProviderRouteKind) {
		out.Refusal = &DelegateRefusal{
			Reason: "harness_unknown",
			Message: fmt.Sprintf(
				"harness %q resolved provider route %q with invalid provider route kind %q",
				opts.Harness,
				selectedProviderRoute,
				selectedProviderRouteKind,
			),
		}
		return out, nil
	}
	if strings.TrimSpace(opts.ExecutionProfile.LaunchProfileID) != "" &&
		!validDelegateProviderRouteKind(selectedProviderRouteKind) {
		out.Refusal = &DelegateRefusal{
			Reason: "harness_unknown",
			Message: fmt.Sprintf(
				"harness %q resolved provider route %q with invalid provider route kind %q",
				opts.Harness,
				selectedProviderRoute,
				selectedProviderRouteKind,
			),
		}
		return out, nil
	}
	out.Lane = strings.TrimSpace(resolution.LaneID)
	if out.Lane == "" {
		out.Lane = laneForGuard
	}
	executionProfileResolver := DelegateExecutionProfileResolver
	var executionProfile *DelegateExecutionProfilePlan
	if delegateExecutionProfileSelected(opts.ExecutionProfile) {
		if strings.TrimSpace(opts.ExecutionProfile.ProfileID) == "" {
			out.Refusal = &DelegateRefusal{
				Reason:  "execution_profile_unresolved",
				Message: "execution profile selectors require a profile_id",
			}
			return out, nil
		}
		if executionProfileResolver == nil {
			out.Refusal = &DelegateRefusal{
				Reason:  "execution_profile_unresolved",
				Message: "no execution profile resolver installed",
			}
			return out, nil
		}
		resolvedProfile, profileErr := executionProfileResolver(DelegateExecutionProfileResolveRequest{
			Intent:                opts.ExecutionProfile,
			SelectedHarness:       resolvedHarness,
			SelectedLaneID:        out.Lane,
			SelectedModelID:       selectedModelID,
			SelectedProviderRoute: selectedProviderRoute,
			SelectedTransportID:   selectedTransportID,
		})
		if profileErr != nil {
			out.Refusal = &DelegateRefusal{
				Reason:  "execution_profile_unresolved",
				Message: profileErr.Error(),
			}
			return out, nil
		}
		executionProfile, profileErr = validateDelegateExecutionProfileResolution(
			resolvedProfile,
			resolvedHarness,
			out.Lane,
			selectedModelID,
			selectedProviderRoute,
			selectedTransportID,
		)
		var incompatible *delegateExecutionProfileIncompatibleError
		if errors.As(profileErr, &incompatible) {
			out.Refusal = &DelegateRefusal{
				Reason:  "execution_profile_incompatible",
				Message: incompatible.Error(),
			}
			return out, nil
		}
		if profileErr != nil {
			out.Refusal = &DelegateRefusal{
				Reason:  "execution_profile_unresolved",
				Message: profileErr.Error(),
			}
			return out, nil
		}
		executionProfile = cloneDelegateExecutionProfilePlan(executionProfile)
		out.ExecutionProfile = cloneDelegateExecutionProfilePlan(executionProfile)
	}
	launchProfileResolver := DelegateLaunchProfileResolver
	var launchProfile *DelegateLaunchProfilePlan
	if strings.TrimSpace(opts.ExecutionProfile.LaunchProfileID) != "" {
		if executionProfile == nil {
			out.Refusal = &DelegateRefusal{
				Reason:  "execution_profile_unresolved",
				Message: "launch profile selection requires a resolved execution profile",
			}
			return out, nil
		}
		if launchProfileResolver == nil {
			out.Refusal = &DelegateRefusal{
				Reason:  "launch_profile_unresolved",
				Message: "no launch profile owner resolver installed",
			}
			return out, nil
		}
		resolvedLaunchProfile, launchErr := launchProfileResolver(DelegateLaunchProfileResolveRequest{
			ProfileID:                 strings.TrimSpace(opts.ExecutionProfile.LaunchProfileID),
			SelectedHarness:           resolvedHarness,
			SelectedLaneID:            out.Lane,
			SelectedModelID:           selectedModelID,
			SelectedProviderRoute:     selectedProviderRoute,
			SelectedProviderRouteKind: selectedProviderRouteKind,
			SelectedTransportID:       executionProfile.TransportID,
			RequestedNativeEffort:     executionProfile.RequestedNativeEffort,
			SelectedCommand:           command,
		})
		if launchErr != nil {
			out.Refusal = &DelegateRefusal{
				Reason:  "launch_profile_unresolved",
				Message: launchErr.Error(),
			}
			return out, nil
		}
		launchProfile, launchErr = validateDelegateLaunchProfileResolution(
			resolvedLaunchProfile,
			executionProfile.ClientHarnessID,
			resolvedHarness,
			out.Lane,
			selectedModelID,
			selectedProviderRoute,
			selectedProviderRouteKind,
			executionProfile.TransportID,
			executionProfile.RequestedNativeEffort,
			command,
		)
		var incompatible *delegateLaunchProfileIncompatibleError
		if errors.As(launchErr, &incompatible) {
			out.Refusal = &DelegateRefusal{
				Reason:  "launch_profile_incompatible",
				Message: incompatible.Error(),
			}
			return out, nil
		}
		if launchErr != nil {
			out.Refusal = &DelegateRefusal{
				Reason:  "launch_profile_unresolved",
				Message: launchErr.Error(),
			}
			return out, nil
		}
		launchProfile = cloneDelegateLaunchProfilePlan(launchProfile)
		command = launchProfile.ExecutablePath
		out.LaunchProfile = cloneDelegateLaunchProfilePlan(launchProfile)
	}

	w, err := s.Get(out.WorkID)
	if err != nil {
		return out, fmt.Errorf("delegate: load work %s: %w", out.WorkID, err)
	}
	if w.State != "claimed" && w.State != "running" {
		out.Refusal = &DelegateRefusal{
			Reason:  "not_claimed",
			Message: fmt.Sprintf("work is in state %q, must be claimed or running", w.State),
		}
		return out, nil
	}
	lease, err := s.readLease(out.WorkID)
	if err != nil {
		out.Refusal = &DelegateRefusal{Reason: "not_claimed", Message: err.Error()}
		return out, nil
	}
	leaseHolder := strings.TrimSpace(lease.By)
	holder := strings.TrimSpace(opts.Actor)
	if holder == "" {
		holder = leaseHolder
	}
	if holder != leaseHolder {
		message := fmt.Sprintf("work lease is held by %q; actor %q cannot delegate it", leaseHolder, holder)
		if opts.DryRun {
			out.Refusal = &DelegateRefusal{Reason: "lease_held", Message: message}
			return out, nil
		}
		evID, eerr := s.recordAndAttachDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, "lease_held", message, opts.Now)
		if eerr != nil {
			return delegateLifecycleMutationResult(out, eerr)
		}
		out.EventID = evID
		out.Refusal = &DelegateRefusal{Reason: "lease_held", Message: message}
		return out, nil
	}
	authorizedWorkSHA, shaErr := workEnvelopeSHA256(w)
	if shaErr != nil {
		return out, shaErr
	}
	authorizedLease := lease

	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" && w.GitDelivery != nil {
		workspace = strings.TrimSpace(w.GitDelivery.TargetWorktreeRoot)
	}
	if workspace == "" {
		workspace, err = delegateOsGetwd()
		if err != nil {
			out.Refusal = &DelegateRefusal{
				Reason:  "workspace_unresolvable",
				Message: "no workspace resolved from --workspace, git_delivery.target_worktree_root, or cwd: " + err.Error(),
			}
			return out, nil
		}
	}
	if strings.TrimSpace(workspace) == "" {
		out.Refusal = &DelegateRefusal{
			Reason:  "workspace_unresolvable",
			Message: "no workspace resolved from --workspace, git_delivery.target_worktree_root, or cwd",
		}
		return out, nil
	}
	// COMPOUND_HOME itself is never a valid workspace. Delegating into the
	// compound home would write transcript/session evidence alongside lifecycle
	// state, making attribution and cleanup impossible.
	if sameWorkspacePath(workspace, s.home) {
		out.Refusal = &DelegateRefusal{
			Reason:  "workspace_occupied",
			Message: fmt.Sprintf("workspace %s is COMPOUND_HOME; choose a distinct working directory", workspace),
		}
		return out, nil
	}
	// The workspace is the attribution key: TranscriptDirFor derives a lane's
	// session store from it, so two live lanes sharing one workspace interleave
	// into a single JSONL with nothing marking which passage belongs to which
	// work. That cannot be separated afterward, so refuse here rather than let a
	// later harvest read one lane's transcript as another's.
	occupant, occupied, occErr := s.workspaceLiveOccupant(workspace, out.WorkID)
	if occErr != nil {
		return out, occErr
	}
	if occupied {
		message := fmt.Sprintf(
			"workspace %s is already bound to live lane work %s (run attempt %s, status %s); "+
				"give this lane its own workspace, or terminalize that attempt first",
			workspace, occupant.WorkID, occupant.ID, occupant.Status)
		out.Refusal = &DelegateRefusal{Reason: "workspace_occupied", Message: message}
		return out, nil
	}
	activationArgs := append([]string(nil), opts.Args...)
	readinessRequest := DelegateHarnessReadinessRequest{
		HomePath:               s.home,
		WorkID:                 out.WorkID,
		ContextPackID:          w.IDs.ContextPackID,
		Workspace:              workspace,
		Harness:                resolvedHarness,
		LaneID:                 out.Lane,
		RequestedModelID:       resolution.RequestedModelID,
		RequestedProviderRoute: resolution.RequestedProviderRoute,
		Command:                command,
		Args:                   append([]string(nil), activationArgs...),
		CommandOverridden:      strings.TrimSpace(opts.Command) != "",
		Now:                    opts.Now,
	}
	readinessGuard := DelegateHarnessReadiness

	readiness := DelegateHarnessReadinessResult{}
	var readinessErr error
	if readinessGuard == nil {
		readiness.Details = []string{"no delegate harness readiness guard is installed"}
	} else {
		readiness, readinessErr = readinessGuard(readinessRequest)
	}
	if readinessErr != nil {
		readiness.Ready = false
		readiness.Details = append(readiness.Details, "readiness probe failed: "+readinessErr.Error())
	}
	out.Readiness = &readiness
	if !readiness.Ready {
		if len(readiness.Details) == 0 {
			readiness.Details = []string{"harness readiness was not established"}
		}
		out.Refusal = &DelegateRefusal{
			Reason:  "readiness_unmet",
			Message: strings.Join(readiness.Details, "; "),
		}
		return out, nil
	}

	capacitySource := resolution.CapacitySource
	if capacitySource != nil {
		copied := *capacitySource
		capacitySource = &copied
	}
	capacityRequest := LaneCapacityGuardRequest{
		HomePath:        s.home,
		Lane:            laneForGuard,
		WorkID:          out.WorkID,
		ExpectedMinutes: dispatchExpectedMinutes(opts.ExpectedMinutes),
		Confirm:         opts.Confirm,
		ReadOnly:        opts.DryRun,
		Now:             opts.Now,
		TupleBacked:     resolution.TupleBacked,
		CapacitySource:  capacitySource,
	}
	capacityGuard := LaneCapacityGuard
	decision, err := runLaneCapacityGuard(capacityGuard, capacityRequest)
	if err != nil {
		return out, fmt.Errorf("delegate: guard lane capacity: %w", err)
	}
	out.Decision = &decision
	readiness, err = addDelegateExecutionProfileReadinessEvidence(readiness, executionProfile)
	if err != nil {
		return out, err
	}
	readiness, err = addDelegateLaunchProfileReadinessEvidence(readiness, launchProfile)
	if err != nil {
		return out, err
	}
	readiness, err = completeDelegateReadinessSnapshot(readiness, laneForGuard, decision)
	if err != nil {
		return out, err
	}
	out.Readiness = &readiness

	// Dry-run: surface the verdict WITHOUT refusing and WITHOUT persisting
	// any event. The decision carries enough for the operator to decide
	// whether to re-run with --confirm.
	if opts.DryRun {
		out.Status = "preview"
		return out, nil
	}

	// D3: when the guard is nil, runLaneCapacityGuard synthesizes a
	// gate_absent verdict (Refused=true). Treat that as the documented
	// gate_absent refusal so the audit trail carries the canonical reason.
	if decision.Refused && strings.TrimSpace(decision.Capacity.State) == "absent" {
		evID, eerr := s.recordAndAttachDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, "gate_absent", decision.Message, opts.Now)
		if eerr != nil {
			return delegateLifecycleMutationResult(out, eerr)
		}
		out.EventID = evID
		out.Refusal = &DelegateRefusal{Reason: "gate_absent", Message: decision.Message}
		return out, nil
	}
	if decision.Refused {
		reason := strings.TrimSpace(decision.Capacity.State)
		if reason == "" {
			reason = "walled"
		}
		evID, eerr := s.recordAndAttachDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, reason, decision.Message, opts.Now)
		if eerr != nil {
			return delegateLifecycleMutationResult(out, eerr)
		}
		out.EventID = evID
		out.Refusal = &DelegateRefusal{Reason: reason, Message: decision.Message}
		return out, nil
	}
	if decision.ConfirmationRequired && !opts.Confirm {
		evID, eerr := s.recordAndAttachDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, "expiring_unconfirmed", decision.Message, opts.Now)
		if eerr != nil {
			return delegateLifecycleMutationResult(out, eerr)
		}
		out.EventID = evID
		out.Refusal = &DelegateRefusal{Reason: "expiring_unconfirmed", Message: decision.Message}
		return out, nil
	}

	sessionName := resolveDelegateSessionName(out.WorkID, opts.Session)
	// Existing binding? Probe liveness via SessionStatus (NOT Has, which
	// is false evidence once a pane dies under remain-on-exit); if
	// exists+pane_dead, record .exited and proceed with a fresh spawn on
	// the SAME session name (deterministic so a steer-while-dead can
	// re-attach); if exists+alive, refuse session_live.
	// The eventlog-latest binding is read (not the alive-only view) so a
	// binding whose exit is ALREADY recorded is still reachable here. That
	// binding is exactly the leak case: a prior teardown recorded the exit but
	// failed to remove the pane, and every later spawn on the same
	// deterministic session name then hit Create -> spawn_ambiguous forever
	// with no path back except a manual `tmux kill-session`.
	latest, latestAlive, rerr := s.latestDelegateBinding(out.WorkID)
	if rerr != nil {
		return out, rerr
	}
	var existing *DelegateBinding
	if latestAlive {
		existing = latest
	} else if latest != nil && latest.Session == sessionName {
		if err := s.reapExitedDelegateSession(latest, runner); err != nil {
			return out, err
		}
	}
	if existing != nil {
		st, sessErr := runner.SessionStatus(existing.Session)
		if sessErr != nil {
			return out, fmt.Errorf("delegate: probe session %s: %w", existing.Session, sessErr)
		}
		if st.Exists && !st.PaneDead {
			evID, eerr := s.recordAndAttachDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, "session_live", "a live bound session already exists for this work", opts.Now)
			if eerr != nil {
				return delegateLifecycleMutationResult(out, eerr)
			}
			out.EventID = evID
			out.Refusal = &DelegateRefusal{
				Reason:  "session_live",
				Message: "a live bound session already exists for this work",
				Session: existing.Session,
			}
			return out, nil
		}
		// Dead or gone: finalize via the unified DelegateSweep helper
		// (terminalize the attempt, record the exited artifact, kill the
		// finished tmux pane if any). This MUST happen BEFORE respawning
		// on the same session name -- otherwise the next Create sees the
		// orphan pane and classifies spawn_ambiguous.
		if _, err := s.DelegateSweep(existing, st, runner, holder, opts.Now); err != nil {
			return delegateLifecycleMutationResult(out, err)
		}
	}

	if availErr := runner.Available(); availErr != nil {
		evID, eerr := s.recordAndAttachDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, "tmux_absent", availErr.Error(), opts.Now)
		if eerr != nil {
			return delegateLifecycleMutationResult(out, eerr)
		}
		out.EventID = evID
		out.Refusal = &DelegateRefusal{Reason: "tmux_absent", Message: availErr.Error()}
		return out, nil
	}

	// MINT BEFORE CREATE: the orchestrator mints the delegate RunAttempt
	// with origin/lane/workspace/wrapper command only -- NEVER prompt or
	// argv, which may contain operator secrets. RunAttempt persistence is
	// independent of the spawn side effect, so a Mint failure is a
	// refuse -- ZERO Create side effects, ZERO session exists.
	raStore := runAttemptsFactory(s.home)
	var minted runattempt.RunAttempt
	mintErr := s.withDelegateLifecycleMutation(out.WorkID, func() error {
		current, err := s.Get(out.WorkID)
		if err != nil {
			return &delegateAuthorityChangedError{detail: "work envelope is no longer readable"}
		}
		currentSHA, err := workEnvelopeSHA256(current)
		if err != nil {
			return err
		}
		if currentSHA != authorizedWorkSHA {
			return &delegateAuthorityChangedError{detail: "work envelope changed"}
		}
		if current.State != "claimed" && current.State != "running" {
			return &delegateAuthorityChangedError{detail: "work state is no longer claimed or running"}
		}
		currentLease, err := s.readLease(out.WorkID)
		if err != nil {
			return &delegateAuthorityChangedError{detail: "work lease is no longer readable"}
		}
		if currentLease != authorizedLease || strings.TrimSpace(currentLease.By) != holder {
			return &delegateAuthorityChangedError{detail: "work lease changed"}
		}
		if harnessResolver != nil {
			currentResolution, resolveErr := harnessResolver(harnessResolveRequest)
			if resolveErr != nil {
				return &delegateAuthorityChangedError{detail: "model-router harness tuple revalidation failed"}
			}
			if !reflect.DeepEqual(currentResolution, resolution) {
				return &delegateAuthorityChangedError{detail: "model-router harness tuple changed"}
			}
		}
		revalidatedExecutionProfile := executionProfile
		if executionProfile != nil {
			currentProfile, profileErr := executionProfileResolver(DelegateExecutionProfileResolveRequest{
				Intent:                opts.ExecutionProfile,
				SelectedHarness:       resolvedHarness,
				SelectedLaneID:        out.Lane,
				SelectedModelID:       selectedModelID,
				SelectedProviderRoute: selectedProviderRoute,
				SelectedTransportID:   selectedTransportID,
			})
			if profileErr != nil {
				return &delegateAuthorityChangedError{detail: "execution profile revalidation failed"}
			}
			revalidatedExecutionProfile, profileErr = validateDelegateExecutionProfileResolution(
				currentProfile,
				resolvedHarness,
				out.Lane,
				selectedModelID,
				selectedProviderRoute,
				selectedTransportID,
			)
			if profileErr != nil {
				return &delegateAuthorityChangedError{detail: "execution profile revalidation failed: " + profileErr.Error()}
			}
		}
		revalidatedLaunchProfile := launchProfile
		if launchProfile != nil {
			currentLaunchProfile, launchErr := launchProfileResolver(DelegateLaunchProfileResolveRequest{
				ProfileID:                 strings.TrimSpace(opts.ExecutionProfile.LaunchProfileID),
				SelectedHarness:           resolvedHarness,
				SelectedLaneID:            out.Lane,
				SelectedModelID:           selectedModelID,
				SelectedProviderRoute:     selectedProviderRoute,
				SelectedProviderRouteKind: selectedProviderRouteKind,
				SelectedTransportID:       revalidatedExecutionProfile.TransportID,
				RequestedNativeEffort:     revalidatedExecutionProfile.RequestedNativeEffort,
				SelectedCommand:           command,
			})
			if launchErr != nil {
				return &delegateAuthorityChangedError{detail: "launch profile revalidation failed"}
			}
			revalidatedLaunchProfile, launchErr = validateDelegateLaunchProfileResolution(
				currentLaunchProfile,
				revalidatedExecutionProfile.ClientHarnessID,
				resolvedHarness,
				out.Lane,
				selectedModelID,
				selectedProviderRoute,
				selectedProviderRouteKind,
				revalidatedExecutionProfile.TransportID,
				revalidatedExecutionProfile.RequestedNativeEffort,
				command,
			)
			if launchErr != nil {
				return &delegateAuthorityChangedError{detail: "launch profile revalidation failed: " + launchErr.Error()}
			}
		}
		revalidatedReadiness, err := readinessGuard(readinessRequest)
		if err != nil {
			return &delegateAuthorityChangedError{detail: "harness readiness revalidation failed"}
		}
		if !revalidatedReadiness.Ready {
			return &delegateAuthorityChangedError{detail: "harness readiness is no longer satisfied"}
		}
		recheckCapacity := capacityRequest
		// Revalidation is evidence-only: it must not emit a duplicate
		// capacity-refusal observation while deciding whether the original
		// activation snapshot still holds.
		recheckCapacity.ReadOnly = true
		revalidatedCapacity, err := runLaneCapacityGuard(capacityGuard, recheckCapacity)
		if err != nil {
			return &delegateAuthorityChangedError{detail: "lane capacity revalidation failed"}
		}
		revalidatedReadiness, err = addDelegateExecutionProfileReadinessEvidence(revalidatedReadiness, revalidatedExecutionProfile)
		if err != nil {
			return err
		}
		revalidatedReadiness, err = addDelegateLaunchProfileReadinessEvidence(revalidatedReadiness, revalidatedLaunchProfile)
		if err != nil {
			return err
		}
		revalidatedSnapshot, err := completeDelegateReadinessSnapshot(revalidatedReadiness, laneForGuard, revalidatedCapacity)
		if err != nil {
			return err
		}
		if revalidatedSnapshot.SnapshotRevision != readiness.SnapshotRevision {
			return &delegateAuthorityChangedError{
				detail: fmt.Sprintf(
					"readiness snapshot changed (admitted=%s current=%s)",
					readiness.SnapshotRevision,
					revalidatedSnapshot.SnapshotRevision,
				),
			}
		}
		minted, err = raStore.Mint(out.WorkID, holder, resolvedHarness, opts.Now)
		return err
	})
	var authorityChanged *delegateAuthorityChangedError
	if errors.As(mintErr, &authorityChanged) {
		out.Refusal = &DelegateRefusal{
			Reason:  "authority_changed",
			Message: authorityChanged.Error(),
		}
		return out, nil
	}
	if mintErr != nil {
		evID, eerr := s.recordDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, "mint_failed", "RunAttempt mint failed before any spawn side effect", opts.Now)
		if eerr != nil {
			return out, eerr
		}
		if err := s.appendDelegateLifecycleEvent(out.WorkID, w.State, holder, evID, "delegate refused reason=mint_failed", opts.Now); err != nil {
			return out, err
		}
		out.EventID = evID
		out.Refusal = &DelegateRefusal{Reason: "mint_failed", Message: "RunAttempt mint failed before any spawn side effect"}
		return out, nil
	}
	runAttemptID := minted.ID
	// Stamp the run-attempt envelope with origin / lane / workspace /
	// wrapper command only -- prompt text / args must NEVER reach
	// RunAttempt on disk. Stamp failures terminalize the mint (the
	// attempt is the only thing that exists at this point) and refuse.
	// Best-effort context (origin/workspace/command) and the failure reason
	// are persisted BEFORE terminalizing so the failed attempt remains auditable.
	stamp := func(field, msg string) (DelegateResult, error) {
		var bestEffortDetails []string
		if detail := bestEffortStamp(raStore, runAttemptID, "origin", func() (runattempt.RunAttempt, error) {
			return raStore.SetOrigin(runAttemptID, runattempt.OriginHarnessExecution)
		}); detail != "" {
			bestEffortDetails = append(bestEffortDetails, detail)
		}
		if detail := bestEffortStamp(raStore, runAttemptID, "workspace", func() (runattempt.RunAttempt, error) {
			return raStore.SetWorkspace(runAttemptID, workspace)
		}); detail != "" {
			bestEffortDetails = append(bestEffortDetails, detail)
		}
		if detail := bestEffortStamp(raStore, runAttemptID, "command", func() (runattempt.RunAttempt, error) {
			return raStore.SetCommand(runAttemptID, command)
		}); detail != "" {
			bestEffortDetails = append(bestEffortDetails, detail)
		}
		if detail := bestEffortStamp(raStore, runAttemptID, "dispatch_intent", func() (runattempt.RunAttempt, error) {
			return raStore.SetDispatchIntent(runAttemptID, resolution.LaneID, resolution.RequestedModelID, resolution.RequestedProviderRoute)
		}); detail != "" {
			bestEffortDetails = append(bestEffortDetails, detail)
		}
		if executionProfile != nil {
			if detail := bestEffortStamp(raStore, runAttemptID, "execution_profile", func() (runattempt.RunAttempt, error) {
				return raStore.SetExecutionProfileIntent(runAttemptID, executionProfile.ID, executionProfile.ClientHarnessID, executionProfile.TransportID, executionProfile.RequestedNativeEffort)
			}); detail != "" {
				bestEffortDetails = append(bestEffortDetails, detail)
			}
		}
		if launchProfile != nil {
			if detail := bestEffortStamp(raStore, runAttemptID, "launch_profile", func() (runattempt.RunAttempt, error) {
				return raStore.SetLaunchProfileIntent(runAttemptID, launchProfile.ID, launchProfile.Revision, launchProfile.ClientProfileID)
			}); detail != "" {
				bestEffortDetails = append(bestEffortDetails, detail)
			}
		}
		reason := "stamp " + field + ": " + msg
		if len(bestEffortDetails) > 0 {
			reason += "; " + strings.Join(bestEffortDetails, "; ")
		}
		if _, terr := raStore.CompleteWithReason(runAttemptID, runattempt.StatusFailed, reason, opts.Now); terr != nil {
			return out, terr
		}
		evID, eerr := s.recordDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, "mint_failed", msg, opts.Now)
		if eerr != nil {
			return out, eerr
		}
		if err := s.appendDelegateLifecycleEvent(out.WorkID, w.State, holder, evID, "delegate refused reason=mint_failed ("+field+")", opts.Now); err != nil {
			return out, err
		}
		out.EventID = evID
		out.Refusal = &DelegateRefusal{Reason: "mint_failed", Message: msg}
		return out, nil
	}
	if _, settErr := raStore.SetOrigin(runAttemptID, runattempt.OriginHarnessExecution); settErr != nil {
		return stamp("origin", "RunAttempt origin stamp failed")
	}
	if _, settErr := raStore.SetWorkspace(runAttemptID, workspace); settErr != nil {
		return stamp("workspace", "RunAttempt workspace stamp failed")
	}
	if _, settErr := raStore.SetCommand(runAttemptID, command); settErr != nil {
		return stamp("command", "RunAttempt command stamp failed")
	}
	if _, settErr := raStore.SetDispatchIntent(runAttemptID, resolution.LaneID, resolution.RequestedModelID, resolution.RequestedProviderRoute); settErr != nil {
		return stamp("attribution", "RunAttempt attribution stamp failed")
	}
	if executionProfile != nil {
		if _, settErr := raStore.SetExecutionProfileIntent(
			runAttemptID,
			executionProfile.ID,
			executionProfile.ClientHarnessID,
			executionProfile.TransportID,
			executionProfile.RequestedNativeEffort,
		); settErr != nil {
			return stamp("execution_profile", "RunAttempt execution profile stamp failed")
		}
	}
	if launchProfile != nil {
		if _, settErr := raStore.SetLaunchProfileIntent(
			runAttemptID,
			launchProfile.ID,
			launchProfile.Revision,
			launchProfile.ClientProfileID,
		); settErr != nil {
			return stamp("launch_profile", "RunAttempt launch profile stamp failed")
		}
	}

	launchEnvironment := map[string]string{
		"COMPOUND_WORK_ID":        out.WorkID,
		"COMPOUND_RUN_ATTEMPT_ID": runAttemptID,
	}
	binding := &DelegateBinding{
		WorkID:                    out.WorkID,
		Session:                   sessionName,
		Harness:                   resolvedHarness,
		Lane:                      laneForGuard,
		Holder:                    holder,
		CapacityState:             decision.Capacity.State,
		RecordedAt:                ts(opts.Now),
		WorkSHA256:                authorizedWorkSHA,
		SelectedProviderRoute:     selectedProviderRoute,
		SelectedProviderRouteKind: selectedProviderRouteKind,
		Readiness:                 out.Readiness,
		ExecutionProfile:          cloneDelegateExecutionProfilePlan(executionProfile),
		LaunchProfile:             cloneDelegateLaunchProfilePlan(launchProfile),
		RunAttemptID:              runAttemptID,
	}
	createInvoked := false
	createSucceeded := false
	_, createErr := raStore.CommitLaunch(runAttemptID, opts.Now, func() error {
		createInvoked = true
		var err error
		if environmentRunner, ok := runner.(SessionEnvironmentRunner); ok {
			if executionProfile != nil {
				launchEnvironment["COMPOUND_EXECUTION_PROFILE_PLAN_ID"] = executionProfile.ID
				launchEnvironment["COMPOUND_EXECUTION_PROFILE_REVISION"] = readiness.ExecutionProfile.Revision
			}
			if launchProfile != nil {
				launchEnvironment["SB_LAUNCH_PROFILE_ID"] = launchProfile.ID
				launchEnvironment["SB_LAUNCH_PROFILE_REVISION"] = launchProfile.Revision
				launchEnvironment["SB_LAUNCH_HARNESS"] = launchProfile.Harness
				launchEnvironment["SB_LAUNCH_CAPTURE_POLICY"] = launchProfile.CaptureMode
				launchEnvironment["SB_LAUNCH_CLIENT_PROFILE"] = launchProfile.ClientProfileID
				launchEnvironment["COMPOUND_CLIENT_PROFILE_ID"] = launchProfile.ClientProfileID
			}
			err = environmentRunner.CreateWithEnvironment(
				sessionName,
				workspace,
				command,
				activationArgs,
				launchEnvironment,
			)
		} else if executionProfile != nil {
			err = errors.New("session runner cannot bind supervised launch environment")
		} else {
			err = runner.Create(sessionName, workspace, command, activationArgs)
		}
		createSucceeded = err == nil
		return err
	})
	if createErr != nil {
		if !createInvoked {
			// Prepared launch evidence could not be published, so the external
			// callback never ran. Terminalize the mint if possible and surface
			// the infrastructure error without persisting its text.
			if _, terminalErr := raStore.Complete(runAttemptID, runattempt.StatusFailed, opts.Now); terminalErr != nil {
				return out, errors.Join(createErr, terminalErr)
			}
			return out, createErr
		}
		if createSucceeded {
			// Create returned success but committed launch evidence did not.
			// The session is definitely ours; it must not remain live without
			// a durable binding. The cleanup helper preserves a binding instead
			// of terminalizing if it cannot prove the session stopped.
			return s.finalizeLaunchEvidenceFailure(out, runner, binding, holder, w.State, createErr, opts.Confirm, opts.Now)
		}
		// Create failed. Probe to detect partial / ambiguous: if the
		// session exists on the server we DO NOT kill it (it may be a
		// stranger's session) and we DO NOT emit a spawned binding. We
		// terminalize the mint as failed and record
		// work.delegate.refused(spawn_ambiguous).
		probe, probeErr := runner.SessionStatus(sessionName)
		reason := "spawn_failed"
		msg := delegateSpawnFailureNote
		if probeErr != nil || probe.UnknownReason != "" {
			reason = "spawn_ambiguous"
			msg = delegateProbeUnknownNote
		} else if probe.Exists {
			reason = "spawn_ambiguous"
			msg = delegateAmbiguousNote
		}
		if _, terr := raStore.Complete(runAttemptID, runattempt.StatusFailed, opts.Now); terr != nil {
			return out, terr
		}
		evID, eerr := s.recordDelegateRefused(out.WorkID, resolvedHarness, laneForGuard, holder, reason, msg, opts.Now)
		if eerr != nil {
			return out, eerr
		}
		if err := s.appendDelegateLifecycleEvent(out.WorkID, w.State, holder, evID, "delegate refused reason="+reason, opts.Now); err != nil {
			return out, err
		}
		out.EventID = evID
		out.Refusal = &DelegateRefusal{Reason: reason, Message: msg}
		return out, nil
	}

	// APPROVED ORDER: committed Create success -> record+attach spawned
	// binding. Any failure AFTER the spawn + binding is a
	// finalize-spawn-failure: kill the definitely-owned session,
	// terminalize the attempt, and record a truthful exited artifact.
	evID, evErr := s.recordDelegateSpawned(out.WorkID, binding, opts.Confirm, runAttemptID, opts.Now)
	if evErr != nil {
		// recordDelegateSpawned failed: no recoverable binding yet.
		// Kill definitely-owned session + terminalize attempt + return.
		// Do NOT write a work.delegate.exited (that would create an
		// exit-without-spawn).
		return out, s.finalizeSpawnFailureAfterBinding(&out, runner, out.WorkID, binding, holder, w.State, evErr, false, opts.Confirm, opts.Now)
	}
	if err := s.appendEvent(Event{
		TS:       ts(opts.Now),
		WorkID:   out.WorkID,
		From:     w.State,
		To:       w.State,
		By:       holder,
		Note:     fmt.Sprintf("delegate spawned session %s harness=%s lane=%s", binding.Session, binding.Harness, binding.Lane),
		Attached: []string{evID},
	}); err != nil {
		// Lifecycle append failed: same as recordDelegateSpawned failure
		// -- no recoverable binding, no exited artifact.
		return out, s.finalizeSpawnFailureAfterBinding(&out, runner, out.WorkID, binding, holder, w.State, err, false, opts.Confirm, opts.Now)
	}
	// Forward-link the exact delegate attempt from the WorkEnvelope through
	// the existing governed same-state Advance seam. The binding is already
	// durable, so an attach failure is a post-spawn failure: finalize the
	// definitely-owned session and the attempt rather than leaving a ratt
	// reachable only from run_attempts/ and the delegate event.
	if _, advanceErr := s.Advance(out.WorkID, AdvanceInput{
		To:            w.State,
		Actor:         holder,
		RunAttemptIDs: []string{runAttemptID},
		Note:          "delegate run attempt attached",
	}, opts.Now); advanceErr != nil {
		return out, s.finalizeSpawnFailureAfterBinding(&out, runner, out.WorkID, binding, holder, w.State, advanceErr, true, opts.Confirm, opts.Now)
	}
	out.Status = "spawned"
	out.Binding = binding
	out.EventID = evID
	out.RunAttemptID = runAttemptID
	return out, nil
}

// finalizeSpawnFailureAfterBinding is the post-Create failure-finalization
// helper. If cleanup cannot prove the session stopped, the attempt stays
// nonterminal, its worktree lock stays held, and a missing spawned binding is
// attached so a later sweep can recover the definitely-owned live session.
// The cause's Error() is NEVER persisted -- adapter errors may echo prompt argv.
func (s *Store) finalizeSpawnFailureAfterBinding(out *DelegateResult, runner SessionRunner, workID string, b *DelegateBinding, holder, state string, cause error, bindingAttached, confirm bool, now time.Time) error {
	if b == nil {
		return cause
	}
	cleanupErrs := []error{cause}
	killed := false
	if runner == nil {
		cleanupErrs = append(cleanupErrs, errors.New("delegate: definitely-owned session cleanup runner is unavailable"))
	} else if killErr := runner.Kill(b.Session); killErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delegate: kill definitely-owned session %s: %w", b.Session, killErr))
	} else {
		killed = true
	}
	if !killed {
		if out != nil {
			out.Status = "spawned"
			out.Binding = b
			out.RunAttemptID = b.RunAttemptID
		}
		if preserveErr := s.preserveLiveDelegateBinding(workID, b, holder, state, bindingAttached, confirm, now); preserveErr != nil {
			cleanupErrs = append(cleanupErrs, preserveErr)
		}
		return errors.Join(cleanupErrs...)
	}

	terminalized := false
	if _, terminalErr := runattempt.Open(s.home).Complete(b.RunAttemptID, runattempt.StatusFailed, now); terminalErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delegate: terminalize spawned attempt %s: %w", b.RunAttemptID, terminalErr))
	} else {
		terminalized = true
	}
	if terminalized {
		if _, lockErr := s.reapDelegateWorktreeLock(b); lockErr != nil {
			cleanupErrs = append(cleanupErrs, lockErr)
		}
	}
	if !bindingAttached {
		return errors.Join(cleanupErrs...)
	}
	ev := delegateEvent{
		SchemaVersion: delegateEventSchemaVersion,
		ID:            delegateBindingEventID("work.delegate.exited", workID, b, now),
		Kind:          "work.delegate.exited",
		TS:            ts(now),
		WorkID:        workID,
		Harness:       b.Harness,
		Lane:          b.Lane,
		Holder:        holder,
		Session:       b.Session,
		WorkSHA256:    b.WorkSHA256,
		RunAttemptID:  b.RunAttemptID,
		Message:       "spawn_finalize_failure",
		Reason:        "spawn_finalize_failure",
	}
	if err := s.writeEventJSON(ev.ID, ev); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delegate: record spawn-finalize exit: %w", err))
	} else if err := s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   workID,
		From:     state,
		To:       state,
		By:       holder,
		Note:     fmt.Sprintf("delegate exited session %s (spawn_finalize_failure)", b.Session),
		Attached: []string{ev.ID},
	}); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delegate: attach spawn-finalize exit: %w", err))
	}
	return errors.Join(cleanupErrs...)
}

// finalizeLaunchEvidenceFailure handles the narrow ambiguity where Create
// returned success but CommitLaunch could not publish the committed boundary.
// A stopped session produces a fixed-content refusal; a session that cannot be
// stopped stays nonterminal and gets a recoverable binding. The live adapter
// error is returned to the caller but never persisted.
func (s *Store) finalizeLaunchEvidenceFailure(out DelegateResult, runner SessionRunner, b *DelegateBinding, holder, state string, cause error, confirm bool, now time.Time) (DelegateResult, error) {
	if b == nil {
		return out, cause
	}
	cleanupErrs := []error{cause}
	killed := false
	if runner == nil {
		cleanupErrs = append(cleanupErrs, errors.New("delegate: definitely-owned session cleanup runner is unavailable"))
	} else if killErr := runner.Kill(b.Session); killErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delegate: kill definitely-owned session %s: %w", b.Session, killErr))
	} else {
		killed = true
	}
	if !killed {
		out.Status = "spawned"
		out.Binding = b
		out.RunAttemptID = b.RunAttemptID
		if preserveErr := s.preserveLiveDelegateBinding(out.WorkID, b, holder, state, false, confirm, now); preserveErr != nil {
			cleanupErrs = append(cleanupErrs, preserveErr)
		}
		return out, errors.Join(cleanupErrs...)
	}

	terminalized := false
	if _, terminalErr := runattempt.Open(s.home).Complete(b.RunAttemptID, runattempt.StatusFailed, now); terminalErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delegate: terminalize ambiguous launch attempt %s: %w", b.RunAttemptID, terminalErr))
	} else {
		terminalized = true
	}
	if terminalized {
		if _, lockErr := s.reapDelegateWorktreeLock(b); lockErr != nil {
			cleanupErrs = append(cleanupErrs, lockErr)
		}
	}

	evID, eventErr := s.recordDelegateRefused(out.WorkID, b.Harness, b.Lane, holder, "spawn_ambiguous", delegateLaunchEvidenceFailureNote, now)
	if eventErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delegate: record launch-evidence refusal: %w", eventErr))
	} else if attachErr := s.appendDelegateLifecycleEvent(out.WorkID, state, holder, evID, "delegate refused reason=spawn_ambiguous", now); attachErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delegate: attach launch-evidence refusal: %w", attachErr))
	} else {
		out.EventID = evID
		out.RunAttemptID = b.RunAttemptID
		out.Refusal = &DelegateRefusal{Reason: "spawn_ambiguous", Message: delegateLaunchEvidenceFailureNote}
	}
	return out, errors.Join(cleanupErrs...)
}

func (s *Store) preserveLiveDelegateBinding(workID string, b *DelegateBinding, holder, state string, bindingAttached, confirm bool, now time.Time) error {
	if !bindingAttached {
		evID, err := s.recordDelegateSpawned(workID, b, confirm, b.RunAttemptID, now)
		if err != nil {
			return fmt.Errorf("delegate: preserve live session binding: %w", err)
		}
		if err := s.appendEvent(Event{
			TS:       ts(now),
			WorkID:   workID,
			From:     state,
			To:       state,
			By:       holder,
			Note:     fmt.Sprintf("delegate preserved live session %s after cleanup failure", b.Session),
			Attached: []string{evID},
		}); err != nil {
			return fmt.Errorf("delegate: attach preserved live session binding: %w", err)
		}
	}
	if _, err := s.Advance(workID, AdvanceInput{
		To:            state,
		Actor:         holder,
		RunAttemptIDs: []string{b.RunAttemptID},
		Note:          "preserved live delegate run attempt attached",
	}, now); err != nil {
		return fmt.Errorf("delegate: forward-link preserved live run attempt %s: %w", b.RunAttemptID, err)
	}
	return nil
}

// bestEffortStamp writes audit context to a RunAttempt during a failure closure
// and captures any error in the reason string so operators can see that the
// backfill failed. The actual stamp error is preserved as a non-fatal audit
// signal rather than shadowing the terminalization error.
func bestEffortStamp(raStore runAttemptStore, runAttemptID, field string, setter func() (runattempt.RunAttempt, error)) string {
	_, err := setter()
	if err != nil {
		return fmt.Sprintf("%s stamp failed: %v", field, err)
	}
	return ""
}

// reapExitedDelegateSession removes a pane left behind by a delegate whose
// exit is ALREADY recorded in the eventlog -- the retry path for a teardown
// whose Kill failed (DelegateSweep records the exit before it kills, so a kill
// failure makes the binding non-alive while the pane survives).
//
// Ownership follows the spawn_ambiguous principle rather than working around
// it: this delegate's own work.delegate.spawned record proves it created a
// session with this exact name, so a DEAD pane under that name is ours to
// remove. A LIVE pane under a name whose exit we already recorded is precisely
// the ownership uncertainty spawn_ambiguous exists for -- it may be a
// stranger's -- so it is left untouched and the following Create refuses on it.
// An unreadable probe concludes nothing and also leaves the pane alone.
func (s *Store) reapExitedDelegateSession(b *DelegateBinding, runner SessionRunner) error {
	if b == nil || runner == nil {
		return nil
	}
	st, err := runner.SessionStatus(b.Session)
	if err != nil {
		return fmt.Errorf("delegate: probe exited session %s: %w", b.Session, err)
	}
	if st.UnknownReason != "" || !st.Exists || !st.PaneDead {
		return nil
	}
	if err := runner.Kill(b.Session); err != nil {
		return fmt.Errorf("delegate: remove orphaned pane %s left by a recorded exit: %w", b.Session, err)
	}
	return nil
}

// --- record helpers (writeEventJSON, D7 contract) ---

func (s *Store) recordDelegateSpawned(workID string, b *DelegateBinding, confirm bool, runAttemptID string, now time.Time) (string, error) {
	id := delegateBindingEventID("work.delegate.spawned", workID, b, now)
	if err := s.writeEventJSON(id, delegateEvent{
		SchemaVersion:             delegateEventSchemaVersion,
		ID:                        id,
		Kind:                      "work.delegate.spawned",
		TS:                        ts(now),
		WorkID:                    workID,
		Harness:                   b.Harness,
		Lane:                      b.Lane,
		Holder:                    b.Holder,
		CapacityState:             b.CapacityState,
		Confirm:                   confirm,
		Session:                   b.Session,
		WorkSHA256:                b.WorkSHA256,
		SelectedProviderRoute:     b.SelectedProviderRoute,
		SelectedProviderRouteKind: b.SelectedProviderRouteKind,
		RunAttemptID:              runAttemptID,
		Readiness:                 b.Readiness,
		ExecutionProfile:          cloneDelegateExecutionProfilePlan(b.ExecutionProfile),
		LaunchProfile:             cloneDelegateLaunchProfilePlan(b.LaunchProfile),
	}); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) recordDelegateRefused(workID, harness, lane, holder, reason, message string, now time.Time) (string, error) {
	id := delegateEventID("work.delegate.refused", workID, now)
	if err := s.writeEventJSON(id, delegateEvent{
		SchemaVersion: delegateEventSchemaVersion,
		ID:            id,
		Kind:          "work.delegate.refused",
		TS:            ts(now),
		WorkID:        workID,
		Harness:       harness,
		Lane:          lane,
		Holder:        holder,
		Reason:        reason,
		Message:       message,
	}); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) recordAndAttachDelegateRefused(workID, harness, lane, holder, reason, message string, now time.Time) (string, error) {
	var id string
	err := s.withDelegateLifecycleMutation(workID, func() error {
		current, err := s.Get(workID)
		if err != nil {
			return &delegateAuthorityChangedError{detail: "work envelope is no longer readable"}
		}
		id, err = s.recordDelegateRefused(workID, harness, lane, holder, reason, message, now)
		if err != nil {
			return err
		}
		return s.appendDelegateLifecycleEvent(
			workID,
			current.State,
			holder,
			id,
			"delegate refused reason="+reason,
			now,
		)
	})
	return id, err
}

func (s *Store) recordDelegateSteered(workID string, b *DelegateBinding, holder, text string, enter bool, now time.Time) (string, error) {
	id := delegateEventID("work.delegate.steered", workID, now)
	if err := s.writeEventJSON(id, delegateEvent{
		SchemaVersion: delegateEventSchemaVersion,
		ID:            id,
		Kind:          "work.delegate.steered",
		TS:            ts(now),
		WorkID:        workID,
		Harness:       b.Harness,
		Lane:          b.Lane,
		Holder:        holder,
		CapacityState: b.CapacityState,
		Session:       b.Session,
		WorkSHA256:    b.WorkSHA256,
		Message:       fmt.Sprintf("steered len=%d enter=%v", len(text), enter),
	}); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) recordDelegateExited(workID string, b *DelegateBinding, observed *SessionStatus, now time.Time) (string, error) {
	id := delegateBindingEventID("work.delegate.exited", workID, b, now)
	ev := delegateEvent{
		SchemaVersion: delegateEventSchemaVersion,
		ID:            id,
		Kind:          "work.delegate.exited",
		TS:            ts(now),
		WorkID:        workID,
		Harness:       b.Harness,
		Lane:          b.Lane,
		Holder:        b.Holder,
		Session:       b.Session,
		WorkSHA256:    b.WorkSHA256,
		Message:       "session no longer live at delegate-time; orchestrator observed death",
	}
	// Carry observed exit when known. Older exited events without
	// observed fields still serialize cleanly (omitempty / nil), so old
	// records decode unchanged.
	//
	// ExitCode pointer is set ONLY when an observed value >= 0 exists
	// -- a signal-only exit must NOT serialize -1 (that would lie).
	if observed != nil {
		if observed.ExitCode >= 0 {
			code := observed.ExitCode
			ev.ExitCode = &code
			ev.ObservedExit = true
		}
		if observed.Signal != "" {
			ev.Signal = observed.Signal
			ev.ObservedExit = true
		}
	}
	// Carry the exact ratt so the exit artifact is keyed to its
	// attempt. Older bindings (empty RunAttemptID) decode unchanged.
	if b != nil {
		ev.RunAttemptID = b.RunAttemptID
	}
	if err := s.writeEventJSON(id, ev); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) recordAndAttachDelegateExited(workID, state string, b *DelegateBinding, observer string, observed SessionStatus, now time.Time) (string, error) {
	id, err := s.recordDelegateExited(workID, b, &observed, now)
	if err != nil {
		return "", err
	}
	if err := s.appendDelegateLifecycleEvent(workID, state, observer, id, fmt.Sprintf("delegate exited session %s", b.Session), now); err != nil {
		return "", err
	}
	return id, nil
}

// --- steer ---

type SteerOptions struct {
	WorkID string
	Actor  string
	Text   string
	Enter  bool
	Now    time.Time
}

type SteerResult struct {
	SchemaVersion string `json:"schema_version"`
	WorkID        string `json:"work_id"`
	Status        string `json:"status"`
	Session       string `json:"session"`
	EventID       string `json:"event_id,omitempty"`
	GeneratedAt   string `json:"generated_at"`
}

func (s *Store) Steer(opts SteerOptions, runner SessionRunner) (SteerResult, error) {
	out := SteerResult{
		SchemaVersion: delegateEventSchemaVersion,
		WorkID:        strings.TrimSpace(opts.WorkID),
		GeneratedAt:   opts.Now.UTC().Format(time.RFC3339),
	}
	if out.WorkID == "" {
		return out, errors.New("steer: WorkID is required")
	}
	if strings.TrimSpace(opts.Text) == "" {
		return out, errors.New("steer: --text is required")
	}
	if runner == nil {
		return out, errors.New("steer: SessionRunner is nil")
	}
	if err := s.requireExperimentalDelegateMutations(); err != nil {
		return out, err
	}
	if err := s.fenceDelegateLifecycleMutation(out.WorkID); err != nil {
		return out, err
	}
	binding, _, err := s.recoverBoundSession(out.WorkID)
	if err != nil {
		return out, err
	}
	if binding == nil {
		return out, fmt.Errorf("steer: work %s has no bound session (experimental delegate mutation must be opted in first)", out.WorkID)
	}
	holder := strings.TrimSpace(opts.Actor)
	if holder == "" {
		holder = binding.Holder
	}
	if holder == "" {
		holder = "codex"
	}
	st, stErr := runner.SessionStatus(binding.Session)
	if stErr != nil {
		return out, fmt.Errorf("steer: probe session %s: %w", binding.Session, stErr)
	}
	if !st.Exists || st.PaneDead {
		if _, sweepErr := s.DelegateSweep(binding, st, runner, holder, opts.Now); sweepErr != nil {
			return out, sweepErr
		}
		return out, fmt.Errorf("steer: session %s for work %s is no longer live; spawn a new delegate", binding.Session, out.WorkID)
	}
	if err := runner.SendKeys(binding.Session, opts.Text, opts.Enter); err != nil {
		return out, fmt.Errorf("steer: send-keys %s: %w", binding.Session, err)
	}
	evID, evErr := s.recordDelegateSteered(out.WorkID, binding, holder, opts.Text, opts.Enter, opts.Now)
	if evErr != nil {
		return out, evErr
	}
	w, werr := s.Get(out.WorkID)
	if werr != nil {
		return out, werr
	}
	if err := s.appendEvent(Event{
		TS:       ts(opts.Now),
		WorkID:   out.WorkID,
		From:     w.State,
		To:       w.State,
		By:       holder,
		Note:     fmt.Sprintf("delegate steered session %s (len=%d)", binding.Session, len(opts.Text)),
		Attached: []string{evID},
	}); err != nil {
		return out, err
	}
	out.Status = "steered"
	out.Session = binding.Session
	out.EventID = evID
	return out, nil
}

// --- attach ---

type AttachOptions struct {
	WorkID string
	Actor  string
}

type AttachResult struct {
	SchemaVersion string   `json:"schema_version"`
	WorkID        string   `json:"work_id"`
	Session       string   `json:"session"`
	AttachArgv    []string `json:"attach_argv"`
	Live          bool     `json:"live"`
}

func (s *Store) Attach(opts AttachOptions, runner SessionRunner) (AttachResult, error) {
	out := AttachResult{
		SchemaVersion: delegateEventSchemaVersion,
		WorkID:        strings.TrimSpace(opts.WorkID),
	}
	if out.WorkID == "" {
		return out, errors.New("attach: WorkID is required")
	}
	if runner == nil {
		return out, errors.New("attach: SessionRunner is nil")
	}
	binding, _, err := s.recoverBoundSession(out.WorkID)
	if err != nil {
		return out, err
	}
	if binding == nil {
		return out, fmt.Errorf("attach: work %s has no bound session (only an explicitly opted-in experimental delegate can create one)", out.WorkID)
	}
	out.Session = binding.Session
	out.AttachArgv = runner.AttachArgv(binding.Session)
	st, stErr := runner.SessionStatus(binding.Session)
	if stErr != nil {
		return out, fmt.Errorf("attach: probe session %s: %w", binding.Session, stErr)
	}
	out.Live = st.Exists && !st.PaneDead
	if !out.Live {
		return out, fmt.Errorf("attach: session %s for work %s is no longer live; spawn a new delegate", binding.Session, out.WorkID)
	}
	return out, nil
}

// appendDelegateLifecycleEvent appends a no-state-change work lifecycle
// event whose Attached[] carries one delegate event id. Slice 1 contract:
// every structured delegate event (spawned, refused, steered, exited) MUST
// attach to a lifecycle event so the audit chain points at it.
func (s *Store) appendDelegateLifecycleEvent(workID, fromState, by, evID, note string, now time.Time) error {
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   workID,
		From:     fromState,
		To:       fromState,
		By:       by,
		Note:     note,
		Attached: []string{evID},
	})
}

// DelegateSweep finalizes an exact binding: maps the observed exit
// status to the right RunAttempt terminal status, kills the finished tmux
// pane (NOT a live one -- bare Has is false evidence once remain-on-exit
// keeps a dead pane alive; SessionStatus is the unified source of truth
// for any pane that has actually died), and records work.delegate.exited.
// Used by:
//   - the explicit `compound work delegate sweep` path,
//   - Steer when the bound pane is dead (instead of just appending an
//     exited lifecycle note, the exact ratt terminalizes and the pane
//     disappears too),
//   - the existing-dead-binding respawn path (BEFORE same-name respawn,
//     otherwise Create deterministically becomes spawn_ambiguous).
//
// Exit mapping (from observed SessionStatus):
//   - observed.PaneDead + ExitCode == 0 -> succeeded
//   - observed.PaneDead + ExitCode > 0 OR signal != "" -> failed
//   - observed.UnknownReason != "" -> UNCERTAIN: surface as a sweep
//     error; do NOT claim failed/abandoned/succeeded; the probe did
//     not conclude. Caller propagates the error.
//   - observed.Exists == false (no probe error) -> abandoned
//
// The RunAttemptID is what makes terminalization exact: only THIS ratt_
// is completed, no sibling claim-reservation attempt is touched. When
// the binding is older than slice-1 (RunAttemptID empty) terminalization
// is skipped, but the killed pane and exited artifact are still emitted.
// The lifecycle-self-transition uses the work envelope's CURRENT state
// (loaded via s.Get), not "".
func (s *Store) DelegateSweep(b *DelegateBinding, observed SessionStatus, runner SessionRunner, observer string, now time.Time) (DelegateSweepOutcome, error) {
	if b == nil {
		return DelegateSweepOutcome{}, fmt.Errorf("delegate sweep: nil binding")
	}
	out := DelegateSweepOutcome{Session: b.Session, WorkID: b.WorkID, RunAttemptID: b.RunAttemptID}
	if err := s.requireExperimentalDelegateMutations(); err != nil {
		return out, err
	}
	if err := s.fenceDelegateLifecycleMutation(b.WorkID); err != nil {
		return out, err
	}
	if observed.UnknownReason != "" {
		// Probe error: do NOT claim an exit. Operator must re-run after
		// the runner is fixed.
		return out, fmt.Errorf("delegate sweep: probe uncertainty: %s", observed.UnknownReason)
	}
	recordedExit, err := s.recordedDelegateExit(b.WorkID, b)
	if err != nil {
		return out, err
	}
	// LIVE pane -- heartbeat the exact ratt (when present) and return
	// WITHOUT writing an event, terminalizing the attempt, or killing.
	// The sweep was not designed to abandon a healthy binding; the
	// generic stale sweep protects it elsewhere.
	if observed.Exists && !observed.PaneDead {
		if recordedExit != nil {
			return out, fmt.Errorf("delegate sweep: session %s is live after its exit was already recorded; refusing to touch a potentially foreign pane", b.Session)
		}
		if b.RunAttemptID != "" {
			raStore := runattempt.Open(s.home)
			if _, herr := raStore.Heartbeat(b.RunAttemptID, now); herr != nil {
				return out, herr
			}
			if cur, gerr := raStore.Get(b.RunAttemptID); gerr == nil {
				out.RunAttemptStatus = cur.Status
			}
		}
		out.ObservedExit = "live"
		out.TerminalStatus = "live"
		return out, nil
	}
	var status, observedLabel string
	if recordedExit != nil {
		switch {
		case recordedExit.Reason == "spawn_finalize_failure":
			status, observedLabel = runattempt.StatusFailed, "failed"
		case recordedExit.ExitCode != nil && *recordedExit.ExitCode == 0:
			status, observedLabel = runattempt.StatusSucceeded, "succeeded"
		case recordedExit.ExitCode != nil && *recordedExit.ExitCode > 0:
			status, observedLabel = runattempt.StatusFailed, "failed"
		case recordedExit.Signal != "":
			status, observedLabel = runattempt.StatusFailed, "failed"
		default:
			status, observedLabel = runattempt.StatusAbandoned, "vanished"
		}
	} else {
		switch {
		case observed.Exists && observed.PaneDead && observed.ExitCode == 0:
			status = runattempt.StatusSucceeded
			observedLabel = "succeeded"
		case observed.Exists && observed.PaneDead && observed.ExitCode > 0:
			status = runattempt.StatusFailed
			observedLabel = "failed"
		case observed.Exists && observed.PaneDead && observed.Signal != "":
			status = runattempt.StatusFailed
			observedLabel = "failed"
		case observed.Exists && observed.PaneDead:
			// Pane dead but no observable exit value: be honest with "unknown"
			// rather than guessing succeeded / failed.
			return out, fmt.Errorf("delegate sweep: pane is dead but observed exit is unknown (ExitCode=%d)", observed.ExitCode)
		default:
			status = runattempt.StatusAbandoned
			observedLabel = "vanished"
		}
	}
	// Lifecycle self-transition needs the envelope's CURRENT state. A
	// failed load is propagated -- we never append state="" (that would
	// silently corrupt the eventlog).
	if recordedExit == nil {
		w, gerr := s.Get(b.WorkID)
		if gerr != nil {
			return out, fmt.Errorf("delegate sweep: load work %s: %w", b.WorkID, gerr)
		}
		if _, err := s.recordAndAttachDelegateExited(b.WorkID, w.State, b, observer, observed, now); err != nil {
			return out, err
		}
	}
	if b.RunAttemptID != "" {
		raStore := runattempt.Open(s.home)
		cur, gerr := raStore.Get(b.RunAttemptID)
		if gerr != nil {
			return out, gerr
		}
		if runattempt.IsTerminal(cur.Status) {
			if cur.Status != status {
				return out, fmt.Errorf("delegate sweep: attempt %s already terminal as %s, observed %s", b.RunAttemptID, cur.Status, status)
			}
		} else if _, terr := raStore.Complete(b.RunAttemptID, status, now); terr != nil {
			return out, terr
		}
		out.RunAttemptStatus = status
	}
	// Teardown reaps BOTH artifacts this delegate created, not just the pane:
	// the git worktree lock over its workspace is released first, because a
	// kill failure below returns early and a lock left behind blocks
	// `git worktree remove` long after the pane is irrelevant. Reaping is
	// reported, never fatal -- the attempt is already terminal, and refusing
	// to return the outcome because an advisory lock could not be released
	// would strand the caller with no way to observe what happened.
	out.WorktreeLock, err = s.reapDelegateWorktreeLock(b)
	if err != nil {
		out.WorktreeLockError = err.Error()
	}
	if runner != nil && observed.PaneDead {
		if err := runner.Kill(b.Session); err != nil {
			out.KillError = err.Error()
			return out, fmt.Errorf("delegate sweep: remove finished pane %s: %w", b.Session, err)
		}
	}
	out.ObservedExit = observedLabel
	out.TerminalStatus = status
	return out, nil
}

// RecoverBoundSession is the exported form of recoverBoundSession. It
// returns the current binding (or nil + no error for "no binding
// exists") plus the eventlog-derived alive flag. Callers that need to
// probe the runner themselves use this to look up the binding; it does
// NOT touch tmux.
func (s *Store) RecoverBoundSession(workID string) (*DelegateBinding, bool, error) {
	return s.recoverBoundSession(workID)
}

// DelegateSweepResolve is a convenience wrapper that returns the current
// binding (or nil + no error for "no binding exists") for the explicit
// sweep CLI verb. It does NOT probe the runner; the caller owns the
// session-status read so a missing pane surfaces immediately.
func (s *Store) DelegateSweepResolve(workID string) (*DelegateBinding, error) {
	b, _, err := s.latestDelegateBinding(workID)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// DelegateStatus returns a read-only, write-free observation of the
// binding and its RunAttempt. It performs no kill, no event write, and
// no terminalization -- the dry-run-capable sibling of DelegateSweep.
func (s *Store) DelegateStatus(workID string, runner SessionRunner) (DelegateStatusObservation, error) {
	out := DelegateStatusObservation{WorkID: workID}
	b, _, err := s.recoverBoundSession(workID)
	if err != nil {
		return out, err
	}
	if b == nil {
		return out, nil
	}
	out.Bound = true
	out.Session = b.Session
	out.Harness = b.Harness
	out.Lane = b.Lane
	out.RunAttemptID = b.RunAttemptID
	if runner != nil {
		st, stErr := runner.SessionStatus(b.Session)
		if stErr != nil {
			out.ProbeError = stErr.Error()
		} else {
			out.PaneStatus = st
		}
	}
	if b.RunAttemptID != "" {
		raStore := runattempt.Open(s.home)
		ra, gerr := raStore.Get(b.RunAttemptID)
		if gerr != nil {
			return out, fmt.Errorf("delegate status: load run attempt %s: %w", b.RunAttemptID, gerr)
		}
		out.RunAttemptStatus = ra.Status
		out.Workspace = ra.Workspace
		// Resolve where this run's transcript lives while we already hold the
		// binding and the workspace. Without this the operator is left to guess
		// which profile home holds it, and a wrong guess reports a completed run
		// as having produced nothing.
		if DelegateSessionStoreResolver != nil {
			root, rErr := DelegateSessionStoreResolver(b.Harness, b.Lane)
			if rErr != nil {
				out.TranscriptError = rErr.Error()
				// Prose alone is blindness with an explanation. When the
				// resolver can say WHAT is missing, the status carries the
				// repair so the keeper can act instead of guess.
				var unknown *SessionStoreRootUnknownError
				if errors.As(rErr, &unknown) {
					repair := unknown.Repair()
					out.SessionStoreRepair = &repair
				}
			} else {
				dir, files, exists, tErr := TranscriptsFor(root, ra.Workspace)
				out.TranscriptDir = dir
				out.Transcripts = files
				out.TranscriptDirExists = exists
				if tErr != nil {
					out.TranscriptError = tErr.Error()
				}
				if newest := newestTranscriptMtime(files); !newest.IsZero() {
					out.TranscriptWriteAge = time.Since(newest).Truncate(time.Second).String()
				}
			}
		} else {
			out.TranscriptError = ErrSessionStoreRootUnknown.Error() + " (no resolver installed)"
		}
	}
	out.LivenessVerdict = classifyLivenessVerdict(out)
	return out, nil
}

// newestTranscriptMtime returns the most recent mtime across files, or
// the zero Time when the slice is empty or every stat fails. Stale runs
// still produce a verdict (stalled), so a stat error on every file does
// not panic -- it just leaves the age field unset.
func newestTranscriptMtime(files []string) time.Time {
	var newest time.Time
	for _, p := range files {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// classifyLivenessVerdict reduces a DelegateStatusObservation to the
// single word a keeper reads from --json. Order is load-bearing: probe
// failure first so a broken prober never manufactures ghosts, unbound
// before pane state so an unbound attempt cannot be mis-classified by
// a stale PaneStatus from a prior run, and starting before stalled so a
// brand-new lane never pages on its first few seconds.
func classifyLivenessVerdict(obs DelegateStatusObservation) string {
	switch {
	case obs.ProbeError != "":
		return "unknown"
	case !obs.Bound:
		return "unbound"
	case !obs.PaneStatus.Exists || obs.PaneStatus.PaneDead:
		return "exited"
	case obs.TranscriptWriteAge == "":
		return "starting"
	default:
		age, err := time.ParseDuration(obs.TranscriptWriteAge)
		if err != nil || age < livenessStallThreshold {
			return "working"
		}
		return "stalled"
	}
}

// livenessStallThreshold is the silence after which a live pane with a
// stale transcript reads as stalled. 90s is short enough that a frozen
// permission dialog surfaces in the next status poll and long enough
// that normal "thinking" pauses do not page.
const livenessStallThreshold = 90 * time.Second

// DelegateStatusObservation is the read-only, JSON-friendly payload of
// `compound work delegate status`.
type DelegateStatusObservation struct {
	WorkID           string        `json:"work_id"`
	Bound            bool          `json:"bound"`
	Session          string        `json:"session,omitempty"`
	Harness          string        `json:"harness,omitempty"`
	Lane             string        `json:"lane,omitempty"`
	RunAttemptID     string        `json:"run_attempt_id,omitempty"`
	RunAttemptStatus string        `json:"run_attempt_status,omitempty"`
	PaneStatus       SessionStatus `json:"pane_status"`
	ProbeError       string        `json:"probe_error,omitempty"`
	// Workspace is the filesystem root the run used; it is also what keys the
	// harness's per-project transcript directory.
	Workspace string `json:"workspace,omitempty"`
	// TranscriptDir / Transcripts answer "where did this delegated run's output
	// go" as a lookup. TranscriptDirExists separates "ran and wrote nothing"
	// from "we are looking in the wrong place"; TranscriptError carries the
	// reason when the lane's profile home cannot be named, because guessing one
	// misreports a completed run as empty.
	TranscriptDir       string   `json:"transcript_dir,omitempty"`
	Transcripts         []string `json:"transcripts,omitempty"`
	TranscriptDirExists bool     `json:"transcript_dir_exists"`
	TranscriptError     string   `json:"transcript_error,omitempty"`
	// SessionStoreRepair is set exactly when the lane's session store root
	// could not be resolved: the actionable instruction (lane, launch profiles
	// consulted, exact missing artifact) instead of a blind false. A blind
	// enrolled lane is a verification hole, never a clean "no transcript".
	SessionStoreRepair *SessionStoreRepair `json:"session_store_repair,omitempty"`
	// TranscriptWriteAge is the time since the newest transcript file was
	// modified, formatted as a Go duration string ("30s", "5m12s"). Empty
	// when there are no transcripts -- a lane that has never produced output
	// has nothing to age. Source of truth: newest mtime across
	// Transcripts; a keeper reading --json sees "this lane has been silent
	// for N" without stat'ing a file.
	TranscriptWriteAge string `json:"transcript_write_age,omitempty"`
	// LivenessVerdict is the keeper-visible single word: working, stalled,
	// starting, exited, unbound, or unknown. Distinguishes a lane that is
	// actively progressing from one frozen on an unanswered permission
	// dialog (the failure mode this field exists to surface). Order in
	// classifyLivenessVerdict is load-bearing; see its doc.
	LivenessVerdict string `json:"liveness_verdict"`
}

// DelegateProgressResult is the content-free progress receipt returned after
// resolving a work's active delegated binding to its exact RunAttempt.
type DelegateProgressResult struct {
	WorkID           string                 `json:"work_id"`
	RunAttemptID     string                 `json:"run_attempt_id"`
	Progress         runattempt.RunProgress `json:"progress"`
	HeartbeatAt      string                 `json:"heartbeat_at"`
	RunAttemptStatus string                 `json:"run_attempt_status"`
}

// DelegateProgress records cumulative progress on the active bound attempt.
// Callers identify work, never a raw ratt id, so sibling claim reservations
// cannot receive a delegated worker's heartbeat.
func (s *Store) DelegateProgress(workID string, progress runattempt.RunProgress, now time.Time) (DelegateProgressResult, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return DelegateProgressResult{}, errors.New("delegate progress: WorkID is required")
	}
	if err := s.requireExperimentalDelegateMutations(); err != nil {
		return DelegateProgressResult{}, err
	}
	if err := s.fenceDelegateLifecycleMutation(workID); err != nil {
		return DelegateProgressResult{}, err
	}
	binding, alive, err := s.recoverBoundSession(workID)
	if err != nil {
		return DelegateProgressResult{}, err
	}
	if !alive || binding == nil || strings.TrimSpace(binding.RunAttemptID) == "" {
		return DelegateProgressResult{}, fmt.Errorf("delegate progress: no active bound RunAttempt for work %s", workID)
	}
	raStore := runattempt.Open(s.home)
	ra, err := raStore.Get(binding.RunAttemptID)
	if err != nil {
		return DelegateProgressResult{}, fmt.Errorf("delegate progress: load RunAttempt %s: %w", binding.RunAttemptID, err)
	}
	if ra.Status != runattempt.StatusRunning {
		return DelegateProgressResult{}, fmt.Errorf("delegate progress: RunAttempt %s is %s, want running", ra.ID, ra.Status)
	}
	updated, err := raStore.HeartbeatProgress(ra.ID, now, progress)
	if err != nil {
		return DelegateProgressResult{}, err
	}
	return DelegateProgressResult{
		WorkID:           workID,
		RunAttemptID:     updated.ID,
		Progress:         *updated.Progress,
		HeartbeatAt:      updated.LastHeartbeatAt.UTC().Format(time.RFC3339Nano),
		RunAttemptStatus: updated.Status,
	}, nil
}

// DelegateSweepOutcome is the per-binding result of DelegateSweep.
//
// WorktreeLock reports what teardown did with the `git worktree lock` over the
// delegate's workspace, as one of the WorktreeLock* vocabulary constants. It is
// always populated on a terminal sweep so "nothing was released" is legible as
// a reason (no reaper installed, no workspace evidence, another live attempt
// still holds it) instead of silence.
type DelegateSweepOutcome struct {
	WorkID            string `json:"work_id"`
	Session           string `json:"session"`
	RunAttemptID      string `json:"run_attempt_id,omitempty"`
	RunAttemptStatus  string `json:"run_attempt_status,omitempty"`
	ObservedExit      string `json:"observed_exit"`
	TerminalStatus    string `json:"terminal_status"`
	KillError         string `json:"kill_error,omitempty"`
	WorktreeLock      string `json:"worktree_lock,omitempty"`
	WorktreeLockError string `json:"worktree_lock_error,omitempty"`
}

// delegateOsGetwd is the runtime seam for workspace resolution. Tests override
// it to simulate an unresolvable cwd without changing the process working
// directory. Production delegates always use os.Getwd.
var delegateOsGetwd = os.Getwd

// workspaceLiveOccupant reports whether a workspace is already bound to a
// non-terminal run attempt belonging to a DIFFERENT work. It guards the
// transcript-attribution boundary described at the call site in Delegate.
//
// Non-terminal is deliberately the whole bar. An idle-but-unterminalized attempt
// still owns the transcript directory, so admitting a second lane beside it
// collides their reports just as thoroughly as a busy one would. Over-refusing
// costs one explicit message naming the attempt to terminalize; under-refusing
// costs a report that can never be attributed.
func (s *Store) workspaceLiveOccupant(workspace, selfWorkID string) (runattempt.RunAttempt, bool, error) {
	if strings.TrimSpace(workspace) == "" {
		return runattempt.RunAttempt{}, false, nil
	}
	attempts, err := runattempt.Open(s.home).ListNonTerminal()
	if err != nil {
		return runattempt.RunAttempt{}, false, fmt.Errorf("delegate: list non-terminal run attempts: %w", err)
	}
	for _, ra := range attempts {
		if ra.WorkID == selfWorkID || strings.TrimSpace(ra.Workspace) == "" {
			continue
		}
		if sameWorkspacePath(ra.Workspace, workspace) {
			return ra, true, nil
		}
	}
	return runattempt.RunAttempt{}, false, nil
}
