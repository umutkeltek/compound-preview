// Package work — sweep_stale_test.go holds the packet's falsifier battery for
// the work-envelope sweep-stale verb (docs_private/plan/packets/2026-08-24
// -work-sweep-stale.md §"Falsifiers").
//
// The three required cases:
//  1. claimed+stale envelope with an abandoned attempt and no pane ->
//     reclaimed to cancelled with the sweep_reason recorded on the event.
//  2. claimed+stale envelope WITH a live heartbeat -> excluded.
//  3. running+fresh envelope -> excluded (last event age < threshold).
//
// Plus four belt-and-suspenders gates the design implies:
//  4. probe error -> fail closed (unreadable).
//  5. live delegate / held lease -> excluded (safe, not unreadable).
//  6. verifying -> failed (NOT cancelled).
//  7. run_attempts are NEVER touched by sweep-stale.
//  8. terminal-state envelopes are never candidates.
//
// Each test uses an injected probe (SweepStaleProbe) so the sweep logic is
// exercised end-to-end without driving tmux, the run-attempt store, or the
// land-lease module. The probe seam is the same one the production code uses
// — the DefaultSweepStaleProbe just lives one layer deeper.
package work

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/runattempt"
)

// fakeProbe is a SweepStaleProbe that returns canned answers so the sweep
// spine can be exercised deterministically. Each method records the workID
// it was called for so tests can assert probe coverage.
//
// probeErrOnWorkIDs: when a workID appears in this set, the probe returns
// err rather than live=false — the sweep must fail closed on these.
type fakeProbe struct {
	liveAttempts      map[string]bool
	liveDelegates     map[string]bool
	heldLeases        map[string]bool
	probeErrOnWorkIDs map[string]error
	calls             []string
}

func newFakeProbe() *fakeProbe {
	return &fakeProbe{
		liveAttempts:      map[string]bool{},
		liveDelegates:     map[string]bool{},
		heldLeases:        map[string]bool{},
		probeErrOnWorkIDs: map[string]error{},
	}
}

func (p *fakeProbe) LiveRunAttempt(workID string) (bool, error) {
	p.calls = append(p.calls, "attempt:"+workID)
	if err, ok := p.probeErrOnWorkIDs[workID]; ok {
		return false, err
	}
	return p.liveAttempts[workID], nil
}

func (p *fakeProbe) LiveDelegate(workID string) (bool, error) {
	p.calls = append(p.calls, "delegate:"+workID)
	if err, ok := p.probeErrOnWorkIDs[workID]; ok {
		return false, err
	}
	return p.liveDelegates[workID], nil
}

func (p *fakeProbe) LeaseHeld(workID string) (bool, error) {
	p.calls = append(p.calls, "lease:"+workID)
	if err, ok := p.probeErrOnWorkIDs[workID]; ok {
		return false, err
	}
	return p.heldLeases[workID], nil
}

// staleEnvelope writes a work envelope to the desired non-terminal state and
// backdates every recorded lifecycle event by eventAge. Returns the envelope
// after backdating, so its LastEventAge (recomputed by the sweep at
// evaluation time) is always eventAge relative to `now`.
//
// The backdate is the wall-clock-freshness insurance: "tests never compare
// wall-clock against fixed fixture dates" (AGENTS.md). Without it, a
// fixture created at fixedNow=2026-06-28T12:00Z would age past the 72h
// threshold the moment 2026-07-01T12:00Z rolls around and would silently
// start failing its OWN falsifier.
func staleEnvelope(t *testing.T, s *Store, state string, eventAge time.Duration, now time.Time) envelope.WorkEnvelope {
	t.Helper()
	// The work_id is a deterministic hash of (Owner, SuccessPredicate, ts(now))
	// (see work.BuildWork), so two envelopes seeded with the same predicate
	// and the same `now` would collide. Append the state to the predicate —
	// every test seeds a single envelope, so collisions across tests are
	// impossible because each test uses a fresh t.TempDir() home.
	w, err := s.Create(CreateInput{Owner: "test-fixture", SuccessPredicate: "sweep-stale falsifier " + state}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	switch state {
	case "claimed":
		claimed, err := s.Claim(w.ID, "test-fixture", now)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		w = claimed
	case "running":
		if _, err := s.Claim(w.ID, "test-fixture", now); err != nil {
			t.Fatalf("claim: %v", err)
		}
		running, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "test-fixture"}, now)
		if err != nil {
			t.Fatalf("advance to running: %v", err)
		}
		w = running
	case "verifying":
		if _, err := s.Claim(w.ID, "test-fixture", now); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "test-fixture"}, now); err != nil {
			t.Fatalf("advance to running: %v", err)
		}
		verifying, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "test-fixture"}, now)
		if err != nil {
			t.Fatalf("advance to verifying: %v", err)
		}
		w = verifying
	default:
		t.Fatalf("staleEnvelope: unsupported state %q", state)
	}
	if err := backdateAllLifecycleEvents(s, now.Add(-eventAge)); err != nil {
		t.Fatalf("backdate events: %v", err)
	}
	return w
}

// backdateAllLifecycleEvents rewrites the TS field of every event on the
// work-event log to `stamp`. Tests call this once after creating a fixture
// envelope so the synthetic envelope ages in step with the test's `now`,
// not in step with the wall-clock.
//
// The implementation reads the entire JSONL log as raw text, decodes each
// line, rewrites the timestamp, and writes it back. That matches the
// established test-seam pattern (see tamperGovernedLifecycleEvent in
// action_outcome_governed_test.go) and avoids depending on the
// append-only-by-design eventstore API the production code uses.
func backdateAllLifecycleEvents(s *Store, stamp time.Time) error {
	path := s.eventsLog()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	bodyStr := strings.TrimSuffix(string(body), "\n")
	if bodyStr == "" {
		return nil
	}
	lines := strings.Split(bodyStr, "\n")
	stampStr := ts(stamp)
	for i, line := range lines {
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return err
		}
		event.TS = stampStr
		encoded, err := json.Marshal(&event)
		if err != nil {
			return err
		}
		lines[i] = string(encoded)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// seedRunAttemptBoundToWork plants a non-terminal RunAttempt bound to
// workID with the given LastHeartbeatAt. Used by the
// run-attempts-immutability falsifier (case 7) to assert that sweep-stale
// does not touch run_attempts/ even when reclaiming the envelope.
func seedRunAttemptBoundToWork(t *testing.T, home string, workID, attemptID string, now time.Time, lastHeartbeat time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "run_attempts"), 0o755); err != nil {
		t.Fatalf("mkdir run_attempts: %v", err)
	}
	ra := runattempt.RunAttempt{
		SchemaVersion:   runattempt.SchemaVersion,
		ID:              attemptID,
		WorkID:          workID,
		Actor:           "test-fixture",
		Harness:         "fake",
		Status:          runattempt.StatusRunning,
		CreatedAt:       now.Add(-1 * time.Hour),
		LastHeartbeatAt: &lastHeartbeat,
	}
	data, err := json.MarshalIndent(&ra, "", " ")
	if err != nil {
		t.Fatalf("marshal attempt: %v", err)
	}
	path := filepath.Join(home, "run_attempts", attemptID+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write attempt: %v", err)
	}
}

// Falsifier 1: claimed+stale envelope with an abandoned attempt and no
// pane -> reclaimed to cancelled with the sweep_reason recorded on the
// event.
func TestSweepStaleReclaimsClaimedEnvelopeWithAbandonedAttempt(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, now)

	probe := newFakeProbe()
	// All probe methods return live=false; envelope is reclaimable.

	rep, err := s.SweepStalePlan(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep.Scanned != 1 || rep.Safe != 0 || rep.Unreadable != 0 {
		t.Fatalf("plan counters wrong: scanned=%d safe=%d unreadable=%d", rep.Scanned, rep.Safe, rep.Unreadable)
	}
	if len(rep.Candidates) != 1 {
		t.Fatalf("plan candidates = %d, want 1", len(rep.Candidates))
	}
	cand := rep.Candidates[0]
	if cand.WorkID != w.ID || cand.State != "claimed" || cand.TargetState != "cancelled" {
		t.Fatalf("candidate wrong: %+v", cand)
	}
	if !strings.Contains(cand.SweepReason, "sweep-stale:") {
		t.Fatalf("sweep_reason missing prefix: %q", cand.SweepReason)
	}

	// Apply
	applyRep, err := s.SweepStaleApply(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applyRep.Reclaimed != 1 {
		t.Fatalf("apply reclaimed=%d, want 1; report=%+v", applyRep.Reclaimed, applyRep)
	}
	if !applyRep.Candidates[0].Reclaimed {
		t.Fatalf("apply candidate.reclaimed=false on success: %+v", applyRep.Candidates[0])
	}

	// Verify on-disk state matches the reclaim
	got, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get after reclaim: %v", err)
	}
	if got.State != "cancelled" {
		t.Fatalf("envelope state=%q after reclaim, want cancelled", got.State)
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatalf("events after reclaim: %v", err)
	}
	var reclaimEvent *Event
	for i := range events {
		if events[i].From == "claimed" && events[i].To == "cancelled" && strings.HasPrefix(events[i].Note, "sweep-stale:") {
			reclaimEvent = &events[i]
		}
	}
	if reclaimEvent == nil {
		t.Fatalf("reclaim event not found in lifecycle log; events=%+v", events)
	}
	if reclaimEvent.By != "sweep-stale" {
		t.Fatalf("reclaim event by=%q, want sweep-stale", reclaimEvent.By)
	}

	// Lease must be released (claimed->cancelled)
	if _, err := s.readLease(w.ID); err == nil {
		t.Fatalf("lease should be released after claimed->cancelled reclaim")
	}
}

// Falsifier 2: claimed+stale envelope WITH a live heartbeat -> excluded.
func TestSweepStaleExcludesEnvelopeWithLiveRunAttempt(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("bootstrap home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, now)

	probe := newFakeProbe()
	probe.liveAttempts[w.ID] = true // Live attempt bound to this work.

	rep, err := s.SweepStalePlan(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep.Safe != 1 || rep.Scanned != 1 {
		t.Fatalf("plan counters wrong: scanned=%d safe=%d (want safe=1)", rep.Scanned, rep.Safe)
	}
	if len(rep.Candidates) != 0 {
		t.Fatalf("plan candidates = %d, want 0 (live attempt must exclude)", len(rep.Candidates))
	}

	// Apply must NOT advance the envelope
	applyRep, err := s.SweepStaleApply(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applyRep.Reclaimed != 0 {
		t.Fatalf("apply reclaimed=%d, want 0 (live attempt)", applyRep.Reclaimed)
	}
	got, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "claimed" {
		t.Fatalf("envelope state=%q, want claimed (must be untouched)", got.State)
	}
}

// Falsifier 3: running+fresh envelope -> excluded (last event age < threshold).
func TestSweepStaleExcludesFreshRunningEnvelope(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("bootstrap home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	// 1h old envelope is FRESH relative to the 72h threshold.
	w := staleEnvelope(t, s, "running", 1*time.Hour, now)

	probe := newFakeProbe()

	rep, err := s.SweepStalePlan(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep.Safe != 1 {
		t.Fatalf("plan safe=%d, want 1 (fresh envelope must be protected)", rep.Safe)
	}
	if len(rep.Candidates) != 0 {
		t.Fatalf("plan candidates = %d, want 0 (fresh envelope must be excluded)", len(rep.Candidates))
	}

	applyRep, err := s.SweepStaleApply(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applyRep.Reclaimed != 0 {
		t.Fatalf("apply reclaimed=%d, want 0 (fresh envelope)", applyRep.Reclaimed)
	}
	got, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "running" {
		t.Fatalf("envelope state=%q, want running (must be untouched)", got.State)
	}
}

// Falsifier 4: probe error -> envelope is unreadable, sweep fails closed.
func TestSweepStaleFailClosedOnProbeError(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("bootstrap home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, now)

	probe := newFakeProbe()
	// Simulate a transient probe fault on the delegate lookup.
	probe.probeErrOnWorkIDs[w.ID] = errors.New("delegate runner unavailable")

	rep, err := s.SweepStalePlan(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep.Unreadable != 1 {
		t.Fatalf("plan unreadable=%d, want 1 (probe error must be unreadable, fail-closed)", rep.Unreadable)
	}
	if len(rep.Candidates) != 0 {
		t.Fatalf("plan candidates = %d, want 0 (probe error must exclude)", len(rep.Candidates))
	}
	if rep.UnreadableIDs[0] != w.ID {
		t.Fatalf("unreadable id=%q, want %q", rep.UnreadableIDs[0], w.ID)
	}

	// Apply must NOT advance the envelope when probe is faulting.
	applyRep, err := s.SweepStaleApply(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applyRep.Reclaimed != 0 {
		t.Fatalf("apply reclaimed=%d, want 0 (fail-closed)", applyRep.Reclaimed)
	}
	got, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "claimed" {
		t.Fatalf("envelope state=%q, want claimed (probe error must NOT reclaim)", got.State)
	}
}

// Falsifier 5: live delegate pane excludes; held lease excludes; both are
// reported as safe (not unreadable) and not surfaced in the candidates list
// — operators reading the dry-run output must see that these envelopes were
// alive, not that they were uncertain.
func TestSweepStaleExcludesOnLiveDelegateAndLease(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("bootstrap home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	delegateLive := staleEnvelope(t, s, "claimed", 30*24*time.Hour, now)
	leaseHeld := staleEnvelope(t, s, "running", 30*24*time.Hour, now)

	probe := newFakeProbe()
	probe.liveDelegates[delegateLive.ID] = true
	probe.heldLeases[leaseHeld.ID] = true

	rep, err := s.SweepStalePlan(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep.Safe != 2 {
		t.Fatalf("plan safe=%d, want 2 (delegate live + lease held)", rep.Safe)
	}
	if len(rep.Candidates) != 0 {
		t.Fatalf("plan candidates = %d, want 0", len(rep.Candidates))
	}
}

// Falsifier 6: applying from `verifying` lands in `failed` (not cancelled).
// The packet is explicit: claimed -> cancelled, running/verifying -> failed,
// so the audit can tell apart a never-started work from one whose
// execution went silent.
func TestSweepStaleVerifyingReclaimsToFailedNotCancelled(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("bootstrap home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	w := staleEnvelope(t, s, "verifying", 30*24*time.Hour, now)

	probe := newFakeProbe()

	rep, err := s.SweepStaleApply(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Reclaimed != 1 || rep.Candidates[0].TargetState != "failed" {
		t.Fatalf("expected target=failed, got %+v", rep.Candidates[0])
	}
	got, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "failed" {
		t.Fatalf("envelope state=%q, want failed (verifying -> failed)", got.State)
	}
}

// Falsifier 7: sweep must NEVER touch run_attempts. We plant a non-terminal
// RunAttempt bound to a reclaimed work, then apply a sweep, then assert the
// attempt is byte-identical to its pre-sweep state.
func TestSweepStaleNeverTouchesRunAttempts(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("bootstrap home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, now)

	attemptID := "ratt_test_sweep_stale_no_touch"
	hb := now.Add(-2 * time.Hour)
	seedRunAttemptBoundToWork(t, homePath, w.ID, attemptID, now, hb)

	preBytes, err := os.ReadFile(filepath.Join(homePath, "run_attempts", attemptID+".json"))
	if err != nil {
		t.Fatalf("read pre attempt: %v", err)
	}

	// The default probe would refuse to reclaim (attempt has a heartbeat
	// within runattempt.DefaultSweepThreshold=30m). Use a fake probe that
	// declares "no live attempt" explicitly to test the boundary
	// claim: sweep-stale MUST NOT touch the attempt even when the sweep
	// reclaims the envelope.
	probe := newFakeProbe()

	if _, err := s.SweepStaleApply(72*time.Hour, probe, now); err != nil {
		t.Fatalf("apply: %v", err)
	}

	postBytes, err := os.ReadFile(filepath.Join(homePath, "run_attempts", attemptID+".json"))
	if err != nil {
		t.Fatalf("read post attempt: %v", err)
	}
	if string(preBytes) != string(postBytes) {
		t.Fatalf("sweep-stale mutated run_attempts/%s.json (forbidden — owned by run-attempt sweep)", attemptID)
	}
}

// Falsifier 8: terminal-state envelopes are never candidates (sweep is a
// no-op for {completed, failed, cancelled}).
func TestSweepStaleIgnoresTerminalEnvelopes(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("bootstrap home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	// Create + claim + advance + close a work so it lands terminal.
	w, err := s.Create(CreateInput{Owner: "test-fixture", SuccessPredicate: "terminal sweep-stale falsifier completed"}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "test-fixture", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "test-fixture"}, now); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "outward_delta", Actor: "test-fixture", Changed: true}, now); err != nil {
		t.Fatalf("close: %v", err)
	}

	probe := newFakeProbe()
	rep, err := s.SweepStalePlan(72*time.Hour, probe, now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rep.Candidates) != 0 {
		t.Fatalf("candidates=%d, want 0 (terminal envelope must not surface)", len(rep.Candidates))
	}
}

// Falsifier: the candidate was diagnosed against a snapshot, and a live
// agent can advance the work between the liveness probes and the reclaim
// write. reclaimEnvelope must re-read the envelope and refuse when its state
// moved — reclaiming a work that is no longer in the diagnosed state is a
// mutation without a diagnosis (the CAS-shaped guard the apply path owes).
func TestReclaimEnvelopeRefusesWhenStateChangedUnderneath(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "sweep-cas", SuccessPredicate: "reclaim refuses moved state"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	// Stale snapshot: the envelope as the sweep saw it at diagnosis time.
	stale, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A live agent advances the work to running AFTER the sweep snapshotted it.
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	candidate := SweepStaleCandidate{WorkID: w.ID, State: stale.State, TargetState: sweepStaleTargetState(stale.State), SweepReason: "falsifier"}
	reclaimErr := reclaimEnvelope(s, stale, candidate, fixedNow.Add(2*time.Minute))
	if reclaimErr == nil {
		t.Fatal("reclaim must refuse when the live state moved underneath the diagnosis")
	}
	if !strings.Contains(reclaimErr.Error(), "state changed") {
		t.Fatalf("error should name the moved state, got: %v", reclaimErr)
	}
	// The envelope must still be running — untouched by the refused reclaim.
	live, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if live.State != "running" {
		t.Fatalf("live state = %q, want running (refused reclaim must not mutate)", live.State)
	}
}

func TestSweepStaleApplyRefusesFrozenCloseAuthority(t *testing.T) {
	s := newStore(t)
	workID := prepareClaimedWork(t, s, "test-fixture", "", "a frozen close fences stale reclaim")
	frozen := freezeClosePendingAfterPrepare(t, s, workID, CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "test-fixture",
		Summary:  "freeze close before stale reclaim",
	}, fixedNow.Add(time.Minute))
	if err := backdateAllLifecycleEvents(s, fixedNow.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("backdate frozen close fixture: %v", err)
	}
	beforeAuthority := snapshotLifecycleAuthority(t, s)
	beforeAttempts := runAttemptsByID(t, s)

	report, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sweep over frozen close: %v", err)
	}
	if report.Reclaimed != 0 || report.Skipped != 1 || len(report.Candidates) != 1 {
		t.Fatalf("frozen close sweep report = %+v, want one skipped recovery_required candidate", report)
	}
	candidate := report.Candidates[0]
	if candidate.Reclaimed ||
		!strings.Contains(candidate.ReclaimError, "close recovery_required") ||
		!strings.Contains(candidate.ReclaimError, frozen.Action.ID) {
		t.Fatalf("frozen close candidate = %+v, want non-reclaimed error bound to %s", candidate, frozen.Action.ID)
	}
	assertLifecycleAuthorityUnchanged(t, beforeAuthority, snapshotLifecycleAuthority(t, s))
	afterAttempts := runAttemptsByID(t, s)
	if !reflect.DeepEqual(beforeAttempts, afterAttempts) {
		t.Fatalf("sweep over frozen close changed RunAttempts\nbefore=%+v\nafter=%+v", beforeAttempts, afterAttempts)
	}
}

func TestReclaimEnvelopeRefusesWhenGenerationChangedUnderneath(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "sweep-generation-cas", SuccessPredicate: "reclaim refuses same-state generation drift"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "artifact_same_state_generation"
	advanced, err := s.Advance(w.ID, AdvanceInput{
		To:         "claimed",
		Actor:      "codex",
		ArtifactID: marker,
		Note:       "same-state generation advanced after sweep diagnosis",
	}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	candidate := SweepStaleCandidate{
		WorkID:      w.ID,
		State:       stale.State,
		TargetState: sweepStaleTargetState(stale.State),
		SweepReason: "same-state generation falsifier",
	}
	reclaimErr := reclaimEnvelope(s, stale, candidate, fixedNow.Add(2*time.Minute))
	if reclaimErr == nil {
		t.Fatal("reclaim must refuse when the exact WorkEnvelope generation changed underneath the diagnosis")
	}
	if !strings.Contains(reclaimErr.Error(), "generation changed") {
		t.Fatalf("error should name generation drift, got: %v", reclaimErr)
	}
	live, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, advanced) {
		t.Fatalf("refused reclaim changed the current generation\nwant=%+v\ngot=%+v", advanced, live)
	}
	if !contains(live.ArtifactIDs, marker) {
		t.Fatalf("same-state generation marker %q was clobbered: %+v", marker, live.ArtifactIDs)
	}
}

type liveOnRecheckProbe struct {
	attemptCalls int
}

func (p *liveOnRecheckProbe) LiveRunAttempt(string) (bool, error) {
	p.attemptCalls++
	return p.attemptCalls > 1, nil
}

func (*liveOnRecheckProbe) LiveDelegate(string) (bool, error) { return false, nil }
func (*liveOnRecheckProbe) LeaseHeld(string) (bool, error)    { return false, nil }

func TestSweepStaleApplyReprobesLivenessInsideMutationBoundary(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, fixedNow)
	probe := &liveOnRecheckProbe{}

	report, err := s.SweepStaleApply(72*time.Hour, probe, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if probe.attemptCalls < 2 {
		t.Fatalf("run-attempt liveness probes = %d, want diagnosis plus in-boundary recheck", probe.attemptCalls)
	}
	if report.Reclaimed != 0 || report.Skipped != 1 {
		t.Fatalf("report = %+v, want the newly-live work skipped", report)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "claimed" {
		t.Fatalf("work state = %q, want claimed after liveness wins the race", current.State)
	}
}

func TestSweepStaleEventFailureLeavesRecoverableTransaction(t *testing.T) {
	s := newStore(t)
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, fixedNow)
	pendingPath := s.workPath(w.ID) + ".sweep-pending"

	previousWriter := writeWorkEvent
	t.Cleanup(func() { writeWorkEvent = previousWriter })
	writeWorkEvent = func(_ *os.File, _ []byte) (int, error) {
		return 0, errors.New("injected sweep event write failure")
	}
	failed, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow)
	writeWorkEvent = previousWriter
	if err != nil {
		t.Fatal(err)
	}
	if failed.Reclaimed != 0 || failed.Skipped != 1 || len(failed.Candidates) != 1 {
		t.Fatalf("failed report = %+v, want one skipped recovery candidate", failed)
	}
	if !strings.Contains(failed.Candidates[0].ReclaimError, "sweep-stale recovery_required") {
		t.Fatalf("failed candidate = %+v, want native recovery_required", failed.Candidates[0])
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("durable sweep pending frame missing after event failure: %v", err)
	}
	if _, err := s.readLease(w.ID); err != nil {
		t.Fatalf("claim lease must remain until event/work publication succeeds: %v", err)
	}

	recovered, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Reclaimed != 1 || len(recovered.Candidates) != 1 || !recovered.Candidates[0].Reclaimed {
		t.Fatalf("recovery report = %+v, want the exact frozen reclaim completed", recovered)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "cancelled" {
		t.Fatalf("recovered state = %q, want cancelled", current.State)
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	exact := 0
	for _, event := range events {
		if event.By == "sweep-stale" && event.From == "claimed" && event.To == "cancelled" {
			exact++
		}
	}
	if exact != 1 {
		t.Fatalf("exact sweep events = %d, want 1: %+v", exact, events)
	}
	if _, err := s.readLease(w.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim lease after recovered terminal publication: %v", err)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sweep pending frame after exact recovery: %v", err)
	}
}

func TestDefaultSweepStaleProbeReadPathsDoNotMaterializeState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*DefaultSweepStaleProbe) (bool, error)
	}{
		{name: "run-attempt", call: func(probe *DefaultSweepStaleProbe) (bool, error) {
			return probe.LiveRunAttempt("work_missing")
		}},
		{name: "delegate", call: func(probe *DefaultSweepStaleProbe) (bool, error) {
			return probe.LiveDelegate("work_missing")
		}},
		{name: "land-lease", call: func(probe *DefaultSweepStaleProbe) (bool, error) {
			return probe.LeaseHeld("work_missing")
		}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			homePath := t.TempDir()
			probe := NewDefaultSweepStaleProbe(homePath)
			live, err := test.call(probe)
			if err != nil {
				t.Fatal(err)
			}
			if live {
				t.Fatalf("missing %s state reported live", test.name)
			}
			entries, err := os.ReadDir(homePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("%s read probe materialized state: %v", test.name, entries)
			}
		})
	}
}

func TestDefaultSweepStaleProbeLiveRunAttemptIsReportOnly(t *testing.T) {
	t.Parallel()

	homePath := t.TempDir()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	attemptStore := runattempt.Open(homePath)
	attempt, err := attemptStore.Mint("work_probe_report_only", "test-actor", "test-harness", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attemptStore.MarkRunning(attempt.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	heartbeatAt := now.Add(2 * time.Second)
	heartbeated, err := attemptStore.Heartbeat(attempt.ID, heartbeatAt)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeated.Status != runattempt.StatusRunning || heartbeated.LastHeartbeatAt == nil || !heartbeated.LastHeartbeatAt.Equal(heartbeatAt) {
		t.Fatalf("running heartbeat fixture = %+v", heartbeated)
	}
	attemptPath := filepath.Join(homePath, "run_attempts", attempt.ID+".json")
	before, err := os.ReadFile(attemptPath)
	if err != nil {
		t.Fatal(err)
	}

	probe := NewDefaultSweepStaleProbe(homePath)
	probe.Now = func() time.Time { return now.Add(time.Minute) }
	probe.AttemptLivenessThreshold = time.Hour
	live, err := probe.LiveRunAttempt("work_probe_report_only")
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatal("fresh pending RunAttempt was not reported live")
	}
	after, err := os.ReadFile(attemptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("LiveRunAttempt mutated RunAttempt:\nbefore=%s\nafter=%s", before, after)
	}
}
