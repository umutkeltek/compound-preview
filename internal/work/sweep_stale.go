// Package work — sweep_stale.go is the work-envelope counterpart to the
// run-attempt sweep (internal/runattempt/runattempt.go). The run-attempt sweep
// is audit-only by design: terminalizing a stale RunAttempt has NO effect on the
// work state machine. Consequently, envelopes stuck in non-terminal states
// (claimed, running, verifying) with no liveness evidence have nothing to move
// them — `running` fossilized into a write-only state. sweep-stale reclaims
// those envelopes under audit.
//
// Design contract (pinned by docs_private/plan/packets/2026-08-24-work-sweep-stale.md):
//   - An envelope is a candidate ONLY IF:
//     1. state in {claimed, running, verifying}, AND
//     2. no lifecycle event newer than threshold (last event -> RFC3339), AND
//     3. no non-terminal run attempt bound to it with LastHeartbeatAt within
//     threshold, AND
//     4. no live delegate pane (work-owned probe — reuses RecoverBoundSession
//   - SessionStatus, mirrors buildDelegateLivenessProbe in runattempt_cli.go),
//     AND
//     5. no held land-lease naming it.
//   - Fail CLOSED: any probe error excludes the envelope (never reclaim on doubt).
//   - Reclaim action: advance to failed (running/verifying) or cancelled (claimed),
//     emit one lifecycle Event with the sweep_reason naming the evidence, release
//     the claim lease. NEVER touch run_attempts — that is the run-attempt sweep's
//     job.
//   - Dry-run by default; --yes applies. The packet default threshold is 72h
//     (the run-attempt default is 30m; an envelope is a much longer-lived
//     object, so the default is correspondingly larger).
package work

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/landlease"
	"compound/internal/runattempt"
)

// sweepStaleEnvelopeStates is the closed set of states that sweep-stale may
// reclaim. Terminal states are no-ops; held / blocked / admitted / ready
// require operator judgment (not silent reclaim). ready->claimed is normal
// claim, not a sweep.
var sweepStaleEnvelopeStates = map[string]bool{
	"claimed":   true,
	"running":   true,
	"verifying": true,
}

// sweepStaleTargetState maps a reclaim-eligible envelope to the terminal
// state the sweep lands it in. chosen so the audit tells a real failure
// apart from a never-started work: claimed -> cancelled (no execution),
// running/verifying -> failed (an execution happened and went silent).
func sweepStaleTargetState(from string) string {
	if from == "claimed" {
		return "cancelled"
	}
	return "failed"
}

func sweepStaleReason(candidate SweepStaleCandidate, threshold time.Duration) string {
	return fmt.Sprintf(
		"sweep-stale: state=%s target=%s last_event_age=%s threshold=%s live_attempts=%d delegate_live=%t lease_held=%t",
		candidate.State,
		candidate.TargetState,
		candidate.LastEventAge,
		threshold,
		candidate.LiveAttempts,
		candidate.DelegateLive,
		candidate.LeaseHeld,
	)
}

// DefaultSweepStaleThreshold is the envelope staleness cutoff when the operator
// does not supply --threshold. 72h matches the packet's acceptance threshold
// (claimed 30d-old envelopes and verifying 36d-old envelopes both qualify; a
// fresh running claim does not).
const DefaultSweepStaleThreshold = 72 * time.Hour

// SweepStaleCandidate is one non-terminal envelope the sweep decided to
// reclaim (dry-run or apply). The struct is the operator-facing dry-run row
// AND the per-envelope entry of SweepStaleReport — same shape, one source
// of truth, so JSON consumers do not need to reconcile two projections.
type SweepStaleCandidate struct {
	WorkID           string `json:"work_id"`
	SourceWorkSHA256 string `json:"source_work_sha256,omitempty"`
	State            string `json:"state"`
	TargetState      string `json:"target_state"`
	LastEventTS      string `json:"last_event_ts,omitempty"`
	LastEventAge     string `json:"last_event_age"`
	LastHeartbeatAt  string `json:"last_heartbeat_at,omitempty"`
	LiveAttempts     int    `json:"live_attempts"` // non-terminal attempts bound to this work
	DelegateLive     bool   `json:"delegate_live"` // true iff a live delegate pane is bound
	LeaseHeld        bool   `json:"lease_held"`    // true iff a land-lease names this work
	SweepReason      string `json:"sweep_reason"`  // evidence recorded on the reclaim event
	Reclaimed        bool   `json:"reclaimed"`     // true when Apply actually terminalized the envelope
	ReclaimError     string `json:"reclaim_error,omitempty"`
}

// SweepStaleReport is the operator-visible outcome of one sweep-stale pass.
// Scanned counts the eligible non-terminal envelopes (claimed+running+verifying
// whose last event is older than threshold); Safe counts the protected ones
// (a live attempt / live delegate / held lease; or a probe error, since the
// sweep fails closed). Swept lists the envelopes the sweep actually
// reclaimed. Hard boundaries: --yes mutates; --dry-run (the default) does not.
type SweepStaleReport struct {
	Threshold     time.Duration         `json:"threshold"`
	Scanned       int                   `json:"scanned"`
	Safe          int                   `json:"safe"`
	Reclaimed     int                   `json:"reclaimed"`
	Skipped       int                   `json:"skipped"`
	Unreadable    int                   `json:"unreadable,omitempty"`
	Candidates    []SweepStaleCandidate `json:"candidates"`
	SkippedIDs    []string              `json:"skipped_ids,omitempty"`
	UnreadableIDs []string              `json:"unreadable_ids,omitempty"`
}

// SweepStaleProbe is the seam through which SweepStalePlan/Apply consult liveness
// evidence the work package would otherwise have to re-discover. Production
// wires the run-attempt store + delegate bindings + land-lease status; tests
// substitute a probe that returns canned answers so the sweep logic is
// exercised end-to-end without driving tmux or the run-attempt store.
//
// Each method returns (live bool, err error). err != nil EXCLUDES the
// envelope (fail-closed); live=true also excludes it. Only live=false, err=nil
// is a reclaim candidate.
type SweepStaleProbe interface {
	// LiveRunAttempt reports whether any non-terminal RunAttempt bound to
	// workID carries activity (heartbeat or claim) within threshold. The
	// probe MUST check each bound attempt: a stale envelope with one live
	// attempt and one abandoned attempt is still live, not reclaimable.
	LiveRunAttempt(workID string) (bool, error)
	// LiveDelegate reports whether a live tmux pane is bound to workID.
	LiveDelegate(workID string) (bool, error)
	// LeaseHeld reports whether a currently-held land-lease names workID.
	LeaseHeld(workID string) (bool, error)
}

// DefaultSweepStaleProbe is the production probe. It consults run_attempts/
// via the runattempt store, delegate bindings via the work-owned recovery
// path (RecoverBoundSession + tmux SessionStatus), and the land-lease state
// via landlease.StatusReadOnly. Probe errors FAIL CLOSED — the envelope is excluded
// from reclaim on doubt, never swept.
//
// Mirrors the safety contract the run-attempt sweep relies on (see
// buildDelegateLivenessProbe in cmd/compound/runattempt_cli.go). A probe that
// cannot prove live=false never claims the candidate is reclaimable.
type DefaultSweepStaleProbe struct {
	Home   string
	Runner SessionRunner
	Now    func() time.Time
	// AttemptLivenessThreshold is the run-attempt liveness window for
	// LiveRunAttempt. Zero falls back to runattempt.DefaultSweepThreshold.
	// The envelope sweep threads its own --threshold here so one coherent
	// staleness definition governs the whole pass: an operator asking for
	// `--threshold 72h` must not have a 30m-hot attempt counted live
	// against a 72h sweep, and vice versa.
	AttemptLivenessThreshold time.Duration
}

// NewDefaultSweepStaleProbe returns the production probe rooted at home, with
// tmux and time hooks defaulted to the standard constructor and time.Now. The
// constructor exists so tests can build a probe against a fresh home without
// having to remember every field shape.
func NewDefaultSweepStaleProbe(home string) *DefaultSweepStaleProbe {
	return &DefaultSweepStaleProbe{
		Home:   home,
		Runner: NewTmuxRunner(),
		Now:    func() time.Time { return time.Now().UTC() },
	}
}

// LiveRunAttempt scans run_attempts/ for attempts bound to workID. A bound
// attempt with status in {pending, running} AND last activity (LastHeartbeatAt
// or CreatedAt) within threshold is live. Any probe error is propagated so the
// sweep fails closed.
func (p *DefaultSweepStaleProbe) LiveRunAttempt(workID string) (bool, error) {
	store := runattempt.Open(p.Home)
	attempts, err := store.List()
	if err != nil {
		return false, fmt.Errorf("sweep-stale probe: list run attempts: %w", err)
	}
	now := p.Now()
	for _, ra := range attempts {
		if ra.WorkID != workID {
			continue
		}
		if runattempt.IsTerminal(ra.Status) {
			continue
		}
		lastActivity := ra.CreatedAt
		if ra.LastHeartbeatAt != nil {
			lastActivity = *ra.LastHeartbeatAt
		}
		// The liveness window comes from the sweep's own threshold when the
		// caller threaded it (the CLI does), falling back to the run-attempt
		// default so the probe stays usable standalone. One pass, one
		// definition of stale.
		threshold := p.AttemptLivenessThreshold
		if threshold <= 0 {
			threshold = runattempt.DefaultSweepThreshold
		}
		if now.Sub(lastActivity) < threshold {
			return true, nil
		}
	}
	return false, nil
}

// LiveDelegate uses the work-owned binding lookup + tmux SessionStatus to
// decide whether a live pane is bound to workID. Mirrors the run-attempt
// sweep's buildDelegateLivenessProbe: a candidate with no binding is
// ordinary-stale (live=false); a probe error is propagated.
func (p *DefaultSweepStaleProbe) LiveDelegate(workID string) (bool, error) {
	wstore, err := OpenReadOnly(p.Home)
	if err != nil {
		return false, fmt.Errorf("sweep-stale probe: open work store: %w", err)
	}
	binding, _, err := wstore.RecoverBoundSession(workID)
	if err != nil {
		return false, fmt.Errorf("sweep-stale probe: recover bound session for %s: %w", workID, err)
	}
	if binding == nil {
		return false, nil
	}
	if p.Runner == nil {
		return false, errors.New("sweep-stale probe: delegate runner unavailable")
	}
	st, err := p.Runner.SessionStatus(binding.Session)
	if err != nil {
		return false, fmt.Errorf("sweep-stale probe: session status for %s: %w", binding.Session, err)
	}
	return st.Exists && !st.PaneDead, nil
}

// LeaseHeld checks both proof capacity and the work's authoritative landing
// slot. A held lease is liveness evidence even when no delegate is visible.
func (p *DefaultSweepStaleProbe) LeaseHeld(workID string) (bool, error) {
	statuses, err := landlease.ProofResourceStatuses(p.Home, p.Now())
	if err != nil {
		return false, fmt.Errorf("sweep-stale proof resource: %w", err)
	}
	for _, row := range statuses {
		status := row.Status
		if status.Held || (status.ProofCoordination != nil && status.IsExpired) {
			_, _, heldWork, ok := parseProofResourceHolder(status.Holder)
			if ok && heldWork == workID {
				return true, nil
			}
			if holderNamesWork(status.Holder, workID) {
				return true, nil
			}
		}
	}

	layout, err := landlease.LayoutStatus(p.Home)
	if err != nil {
		return false, err
	}
	if layout.Mode != "scoped" {
		return false, nil
	}
	store, err := OpenReadOnly(p.Home)
	if err != nil {
		return false, err
	}
	w, err := store.Get(workID)
	if err != nil {
		return false, err
	}
	if w.GitDelivery == nil {
		return false, nil
	}
	key, err := store.LandingResourceKey(workID)
	if err != nil {
		return false, err
	}
	resource, err := landlease.OpenResource(p.Home, key)
	if err != nil {
		return false, err
	}
	status, err := resource.StatusReadOnly(p.Now())
	if err != nil {
		return false, err
	}
	return status.Held && holderNamesWork(status.Holder, workID), nil
}

func holderNamesWork(holder, workID string) bool {
	for _, part := range strings.Split(holder, ":") {
		if part == workID {
			return true
		}
	}
	return false
}

// SweepStalePlan diagnoses every reclaim candidate and returns the operator-
// facing report WITHOUT mutating anything. Use this as the read-only
// preview; --dry-run calls it. Like SweepStaleApply it takes the caller's clock
// (zero = real clock): a plan that silently re-derived time.Now() while apply
// took `now` let a test pinned to a fixed date expire 72h later and turn
// main red for every lane (lived 2026-08-27).
func (s *Store) SweepStalePlan(threshold time.Duration, probe SweepStaleProbe, now time.Time) (SweepStaleReport, error) {
	return sweepStaleEvaluate(s, threshold, probe, false, now)
}

// SweepStaleApply diagnoses every reclaim candidate and APPLIES the reclaim
// for those that fail the liveness probes. --yes calls this. Probe faults
// are surfaced (not retried inside the sweep); the next pass retries.
func (s *Store) SweepStaleApply(threshold time.Duration, probe SweepStaleProbe, now time.Time) (SweepStaleReport, error) {
	return sweepStaleEvaluate(s, threshold, probe, true, now)
}

// sweepStaleEvaluate is the shared read/apply spine. apply=false returns the
// plan only; apply=true mutates the envelopes that pass the liveness probes.
// Per-envelope apply errors do NOT abort the pass — a single terminal-write
// failure on one envelope leaves the others reclaimable, and the failed
// envelope is surfaced via SweepStaleCandidate.ReclaimError so the operator
// can reconcile.
func sweepStaleEvaluate(s *Store, threshold time.Duration, probe SweepStaleProbe, apply bool, now time.Time) (SweepStaleReport, error) {
	if threshold <= 0 {
		threshold = DefaultSweepStaleThreshold
	}
	if probe == nil {
		return SweepStaleReport{}, errors.New("sweep-stale: probe is required")
	}
	// The caller's clock is the sweep's clock: tests pin determinism through
	// it, and a sweep that silently re-derived time.Now() could disagree with
	// the CLI's --threshold parse and the probe's liveness window within one
	// pass. Zero means the caller wants the real clock (CLI default).
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rep := SweepStaleReport{Threshold: threshold}

	// Audit every work envelope currently non-terminal. List skips
	// archived items; an archived envelope cannot be in a non-terminal
	// state because terminalize-published envelopes cannot be re-opened
	// via archive (the archive seam is one-way).
	envelopes, err := s.List()
	if err != nil {
		return rep, fmt.Errorf("sweep-stale: list work envelopes: %w", err)
	}
	// Canonical ordering: by work_id so dry-run output is stable across
	// runs (the underlying directory enumeration is not).
	sort.Slice(envelopes, func(i, j int) bool { return envelopes[i].ID < envelopes[j].ID })

	for _, w := range envelopes {
		_, pendingPresent, pendingErr := s.loadSweepStalePending(w.ID)
		if pendingErr != nil {
			if pendingPresent {
				candidate := SweepStaleCandidate{
					WorkID:       w.ID,
					ReclaimError: sweepStaleRecoveryRequired(w.ID, pendingErr).Error(),
				}
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, w.ID)
				rep.Candidates = append(rep.Candidates, candidate)
				continue
			}
			rep.Unreadable++
			rep.UnreadableIDs = append(rep.UnreadableIDs, w.ID)
			continue
		}
		if pendingPresent {
			candidate := SweepStaleCandidate{WorkID: w.ID}
			if !apply {
				candidate.ReclaimError = sweepStaleRecoveryRequired(w.ID, errors.New("frozen reclaim awaits replay")).Error()
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, w.ID)
				rep.Candidates = append(rep.Candidates, candidate)
				continue
			}
			var recoveredCandidate SweepStaleCandidate
			reconcileErr := s.withLifecycleMutation(func() error {
				return s.withCreatePlanAdmissionLock(w.ID, func() error {
					var err error
					recoveredCandidate, err = s.reconcileSweepStalePendingLocked(w.ID)
					return err
				})
			})
			if reconcileErr != nil {
				candidate.ReclaimError = reconcileErr.Error()
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, w.ID)
				rep.Candidates = append(rep.Candidates, candidate)
				continue
			}
			candidate = recoveredCandidate
			rep.Reclaimed++
			rep.Candidates = append(rep.Candidates, candidate)
			continue
		}
		if !sweepStaleEnvelopeStates[w.State] {
			continue
		}
		candidate, ok, err := evaluateEnvelopeCandidate(s, w, threshold, probe, now)
		if err != nil {
			// Probe error: the envelope is unreadable from a sweep
			// perspective. Surface it but NEVER reclaim — fail closed.
			rep.Unreadable++
			rep.UnreadableIDs = append(rep.UnreadableIDs, w.ID)
			continue
		}
		if !ok {
			// Protected by a live attempt, live delegate, held
			// lease, or fresh lifecycle event. Not a candidate.
			rep.Safe++
			continue
		}
		if apply {
			if reclaimErr := reclaimEnvelopeWithProbe(s, w, candidate, threshold, probe, now); reclaimErr != nil {
				candidate.ReclaimError = reclaimErr.Error()
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, w.ID)
				rep.Candidates = append(rep.Candidates, candidate)
				continue
			}
			candidate.Reclaimed = true
			rep.Reclaimed++
		} else {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, w.ID)
		}
		rep.Candidates = append(rep.Candidates, candidate)
	}
	rep.Scanned = rep.Safe + rep.Reclaimed + rep.Skipped + rep.Unreadable
	return rep, nil
}

// evaluateEnvelopeCandidate decides whether envelope w is reclaimable under
// the packet's five-rule contract. Returns (candidate, true, nil) for a
// reclaim candidate with its sweep_reason filled in; (zero, false, nil) for
// a protected envelope; (zero, false, err) for an unreadable envelope (probe
// failure — fail closed).
func evaluateEnvelopeCandidate(s *Store, w envelope.WorkEnvelope, threshold time.Duration, probe SweepStaleProbe, now time.Time) (SweepStaleCandidate, bool, error) {
	target := sweepStaleTargetState(w.State)

	// Rule 2: last lifecycle event must be older than threshold. An
	// envelope whose most recent event is fresh has a live operator or
	// harness driving it; sweep must NOT race them.
	lastEventTS, lastEventTime, err := lastLifecycleEventTime(s, w.ID)
	if err != nil {
		return SweepStaleCandidate{}, false, err
	}
	ageSeconds := now.Sub(lastEventTime)
	if ageSeconds < threshold {
		// Fresh enough — protected, not a candidate. We do not surface
		// this case in the dry-run report (it would dilute the list);
		// Safe counter is sufficient.
		return SweepStaleCandidate{}, false, nil
	}

	// Rule 3: live run attempt bound to this work? A live attempt is
	// evidence the work is still in flight, regardless of envelope
	// silence (a harness may be running without a recent event).
	liveAttempt, err := probe.LiveRunAttempt(w.ID)
	if err != nil {
		return SweepStaleCandidate{}, false, fmt.Errorf("work %s: %w", w.ID, err)
	}
	if liveAttempt {
		return SweepStaleCandidate{}, false, nil
	}

	// Rule 4: live delegate pane? Reuses the run-attempt probe's
	// delegate seam (RecoverBoundSession + SessionStatus).
	liveDelegate, err := probe.LiveDelegate(w.ID)
	if err != nil {
		return SweepStaleCandidate{}, false, fmt.Errorf("work %s: %w", w.ID, err)
	}
	if liveDelegate {
		return SweepStaleCandidate{}, false, nil
	}

	// Rule 5: held land-lease naming this work? A steal-under-way
	// would be silently lost by reclaiming the work out from under it.
	leaseHeld, err := probe.LeaseHeld(w.ID)
	if err != nil {
		return SweepStaleCandidate{}, false, fmt.Errorf("work %s: %w", w.ID, err)
	}
	if leaseHeld {
		return SweepStaleCandidate{}, false, nil
	}

	sourceWorkSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		return SweepStaleCandidate{}, false, fmt.Errorf("work %s: seal sweep candidate: %w", w.ID, err)
	}

	candidate := SweepStaleCandidate{
		WorkID:           w.ID,
		SourceWorkSHA256: sourceWorkSHA256,
		State:            w.State,
		TargetState:      target,
		LastEventTS:      lastEventTS,
		LastEventAge:     ageSeconds.Round(time.Second).String(),
		LiveAttempts:     0,
		DelegateLive:     false,
		LeaseHeld:        false,
	}
	candidate.SweepReason = sweepStaleReason(candidate, threshold)
	return candidate, true, nil
}

// lastLifecycleEventTime returns the timestamp of the most recent lifecycle
// event for workID, or (zero, zero-time, nil) for an envelope with no events.
// A fresh envelope that has never recorded an event has no "last event" to
// date against — treat that envelope as "born now" (caller compares against
// now.Sub(zero) = a very large age, which always exceeds threshold; this is
// the desired behavior because an envelope with no recorded activity IS stale).
//
// Events are read through the lifecycle log under eventsFor. A missing log
// returns an empty slice, never an error, matching OpenReadOnly's read-class
// contract.
func lastLifecycleEventTime(s *Store, workID string) (string, time.Time, error) {
	events, err := s.eventsFor(workID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sweep-stale: read lifecycle events for %s: %w", workID, err)
	}
	if len(events) == 0 {
		return "", time.Time{}, nil
	}
	// eventsFor returns the chronological log; the most recent event
	// is the last element.
	last := events[len(events)-1]
	t, perr := parseLifecycleTS(last.TS)
	if perr != nil {
		return last.TS, time.Time{}, fmt.Errorf("sweep-stale: parse last event ts %q for %s: %w", last.TS, workID, perr)
	}
	return last.TS, t, nil
}

// parseLifecycleTS accepts the formats compound actually writes on the wire.
// Today ts(now) emits UnixNano as a decimal string (e.g. "1754400000000000000"),
// and the sweep-stale backdating test seam reuses the same producer so
// fixture events round-trip the same shape. Older writers (pre-ts-now
// refactors) and any external tooling that emits RFC3339Nano / RFC3339 strings
// are accepted too so the sweep never refuses to evaluate an envelope whose
// history happens to be in either shape.
//
// Returns an error on parse failure so a corrupt event timestamp does not
// silently regress into a never-stale envelope.
func parseLifecycleTS(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("empty lifecycle timestamp")
	}
	// Primary: UnixNano decimal string (the shape ts(now) writes today).
	if nano, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(0, nano).UTC(), nil
	}
	// Fallback: RFC3339Nano / RFC3339 for older writers and external tooling.
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized lifecycle timestamp format %q", value)
}

// reclaimEnvelope terminalizes w to its target state, appends one lifecycle
// event with the sweep_reason on it, and releases the claim lease (only
// relevant for claimed-state reclaims; for running/verifying the lease may
// have already been released by Advance, in which case the remove is a no-op
// via os.IsNotExist).
//
// The reclaim is NOT a full Close — there is no Receipt, no Transition, no
// PolicyDecision. The packet is explicit: the envelope is reclaimable
// precisely because no live agent can produce those proofs. An event with
// from=w.State, to=target, and a sweep_reason naming the evidence is the
// audit hook a downstream reconciliation can match on. The audit reclaimer
// itself is the operator running this command, not a third-party tool.
func reclaimEnvelope(s *Store, w envelope.WorkEnvelope, candidate SweepStaleCandidate, now time.Time) error {
	return reclaimEnvelopeWithProbe(s, w, candidate, 0, nil, now)
}

func reclaimEnvelopeWithProbe(s *Store, w envelope.WorkEnvelope, candidate SweepStaleCandidate, threshold time.Duration, probe SweepStaleProbe, now time.Time) error {
	return s.withLifecycleMutation(func() error {
		// This existing work-keyed flock is the cross-process critical
		// section for one reclaim generation. The outer lifecycle lock still
		// serializes against Claim, Advance, and Close in this binary.
		return s.withCreatePlanAdmissionLock(w.ID, func() error {
			return s.reclaimEnvelopeLocked(w, candidate, threshold, probe, now)
		})
	})
}

func (s *Store) reclaimEnvelopeLocked(w envelope.WorkEnvelope, candidate SweepStaleCandidate, threshold time.Duration, probe SweepStaleProbe, now time.Time) error {
	if err := s.fenceClosePendingMutation(w.ID); err != nil {
		return err
	}
	if err := s.fenceSweepStalePendingMutation(w.ID); err != nil {
		return err
	}
	if !sweepStaleEnvelopeStates[w.State] {
		return fmt.Errorf("work %s: state %q is not reclaimable", w.ID, w.State)
	}
	target := sweepStaleTargetState(w.State)

	// CAS-style precondition: the candidate was diagnosed against a
	// snapshot taken at pass start, and a live agent can advance the work
	// between the probes and this write. Re-read the envelope and refuse
	// when its state moved — reclaiming a work that is no longer in the
	// diagnosed state is a mutation without a diagnosis.
	var current envelope.WorkEnvelope
	if err := s.readJSON(s.workPath(w.ID), &current); err != nil {
		return fmt.Errorf("recheck envelope %s before reclaim: %w", w.ID, err)
	}
	if current.State != w.State {
		return fmt.Errorf("work %s state changed from %q to %q between sweep diagnosis and reclaim; refusing (next pass re-diagnoses)", w.ID, w.State, current.State)
	}
	currentWorkSHA256, err := workEnvelopeSHA256(current)
	if err != nil {
		return fmt.Errorf("seal current envelope %s before reclaim: %w", w.ID, err)
	}
	expectedWorkSHA256 := strings.TrimSpace(candidate.SourceWorkSHA256)
	if expectedWorkSHA256 == "" {
		expectedWorkSHA256, err = workEnvelopeSHA256(w)
		if err != nil {
			return fmt.Errorf("seal diagnosed envelope %s before reclaim: %w", w.ID, err)
		}
	}
	if currentWorkSHA256 != expectedWorkSHA256 {
		return fmt.Errorf("work %s generation changed from %s to %s between sweep diagnosis and reclaim; refusing (next pass re-diagnoses)", w.ID, expectedWorkSHA256, currentWorkSHA256)
	}
	if probe != nil {
		rechecked, ok, recheckErr := evaluateEnvelopeCandidate(s, current, threshold, probe, now)
		if recheckErr != nil {
			return fmt.Errorf("work %s liveness recheck inside reclaim boundary: %w", w.ID, recheckErr)
		}
		if !ok {
			return fmt.Errorf("work %s liveness changed inside reclaim boundary; refusing (next pass re-diagnoses)", w.ID)
		}
		if rechecked.SourceWorkSHA256 != currentWorkSHA256 {
			return fmt.Errorf("work %s generation changed during in-boundary liveness recheck", w.ID)
		}
	}

	// Mutate the on-disk envelope state. Reuse writeJSON's atomic
	// temp+rename seam so a partial write cannot corrupt work.json.
	updated := current
	updated.State = target

	// CUSTODY: a writer that MUTATES a work envelope must RE-SEAL it.
	//
	// The autonomy director verifies a terminal work by re-hashing its envelope and
	// comparing that to the most recent work_sha256 recorded on the work's lifecycle
	// events. Close and Advance have always carried that seal (see the Event built in
	// work.go). sweep-stale did not: it rewrote the envelope's state and appended an
	// UNSEALED event, so the newest recorded hash still described the pre-sweep bytes.
	// Every work this reclaimed therefore failed the director's custody check, and
	// because that check fail-closes on the whole cohort rather than excluding one
	// sample, a single swept envelope silently vetoed `director propose` — i.e. one
	// governed subsystem disabled another, with no error surfaced at the sweep.
	//
	// Hash BEFORE the write so a hashing failure cannot leave a mutated envelope on
	// disk with no matching seal.
	previousWorkSHA256 := currentWorkSHA256
	sweptWorkSHA256, err := workEnvelopeSHA256(updated)
	if err != nil {
		return fmt.Errorf("seal swept envelope %s: %w", w.ID, err)
	}

	if err := s.injectSweepStaleFault(sweepStaleFaultBeforePendingPersist); err != nil {
		return err
	}
	pending, err := s.newSweepStalePending(current, updated, candidate, threshold, now)
	if err != nil {
		return fmt.Errorf("freeze sweep-stale reclaim for %s: %w", w.ID, err)
	}
	if pending.SourceWorkSHA256 != previousWorkSHA256 || pending.FinalWorkSHA256 != sweptWorkSHA256 {
		return fmt.Errorf("frozen sweep-stale digests drifted for %s", w.ID)
	}
	if err := s.persistSweepStalePending(pending); err != nil {
		if _, present, loadErr := s.loadSweepStalePending(w.ID); loadErr == nil && present {
			return sweepStaleRecoveryRequired(w.ID, err)
		}
		return fmt.Errorf("persist sweep-stale intent for %s: %w", w.ID, err)
	}
	if err := s.injectSweepStaleFault(sweepStaleFaultAfterPendingPersist); err != nil {
		return sweepStaleRecoveryRequired(w.ID, err)
	}
	if err := s.completeSweepStalePending(pending); err != nil {
		return sweepStaleRecoveryRequired(w.ID, err)
	}
	return nil
}
