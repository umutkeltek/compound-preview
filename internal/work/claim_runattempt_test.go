// C1 tests: every claim mints a RunAttempt; abandon (claimed→ready)
// terminalizes it. These are the falsifiers the continuation plan requires:
//
//   (a) state after claim+abandon DIFFERS from never-claimed (the attempt
//       file exists, terminal=abandoned)
//   (b) a claim mints exactly one attempt, idempotent on retry
//   (c) envelope hash continuity — the two guard tests stay green (pinned
//       in their own files; this file does not re-test them)

package work

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// TestClaimMintsRunAttempt proves that Claim mints exactly one RunAttempt,
// the ratt_ id lands on the envelope's RunAttemptIDs, and the attempt file
// exists on disk with status=pending.
func TestClaimMintsRunAttempt(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "c1-test", SuccessPredicate: "claim mints attempt"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "codex", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one RunAttemptID on the envelope.
	if len(claimed.RunAttemptIDs) != 1 {
		t.Fatalf("claimed RunAttemptIDs = %v, want exactly 1", claimed.RunAttemptIDs)
	}
	rattID := claimed.RunAttemptIDs[0]
	if !runattempt.Valid(rattID) {
		t.Fatalf("RunAttemptID %q is not a valid ratt_ id", rattID)
	}
	// The attempt file exists on disk.
	raPath := filepath.Join(s.home, "run_attempts", rattID+".json")
	if _, err := os.Stat(raPath); err != nil {
		t.Fatalf("RunAttempt file missing: %v", err)
	}
	// The attempt has status=pending and the right work_id.
	ra, err := runattempt.Open(s.home).Get(rattID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Status != runattempt.StatusPending {
		t.Fatalf("RunAttempt status = %q, want pending", ra.Status)
	}
	if ra.WorkID != w.ID {
		t.Fatalf("RunAttempt work_id = %q, want %q", ra.WorkID, w.ID)
	}
	if ra.Actor != "codex" {
		t.Fatalf("RunAttempt actor = %q, want codex", ra.Actor)
	}
	// The lifecycle event's Attached includes the ratt_ id.
	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	var claimEvent *Event
	for i := range events {
		if events[i].To == "claimed" {
			claimEvent = &events[i]
			break
		}
	}
	if claimEvent == nil {
		t.Fatal("no claim event found")
	}
	found := false
	for _, a := range claimEvent.Attached {
		if a == rattID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("claim event Attached %v does not include %q", claimEvent.Attached, rattID)
	}
}

// TestClaimIdempotentRetryDoesNotMintSecondAttempt proves that re-claiming
// an already-claimed work (same actor) returns the same envelope without
// minting a second RunAttempt.
func TestClaimIdempotentRetryDoesNotMintSecondAttempt(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "c1-test", SuccessPredicate: "idempotent claim"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Claim(w.ID, "codex", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.RunAttemptIDs) != 1 {
		t.Fatalf("first claim RunAttemptIDs = %v, want 1", first.RunAttemptIDs)
	}
	// Retry: same actor, same work.
	second, err := s.Claim(w.ID, "codex", fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.RunAttemptIDs) != 1 {
		t.Fatalf("retry claim RunAttemptIDs = %v, want still 1", second.RunAttemptIDs)
	}
	if second.RunAttemptIDs[0] != first.RunAttemptIDs[0] {
		t.Fatalf("retry minted a different attempt: %q vs %q", second.RunAttemptIDs[0], first.RunAttemptIDs[0])
	}
	// Only one ratt_ file on disk.
	entries, err := os.ReadDir(filepath.Join(s.home, "run_attempts"))
	if err != nil {
		t.Fatal(err)
	}
	rattCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && strings.HasPrefix(e.Name(), "ratt_") {
			rattCount++
		}
	}
	if rattCount != 1 {
		t.Fatalf("run_attempts dir has %d ratt_ files, want 1", rattCount)
	}
}

// TestClaimAbandonTerminalizesRunAttempt proves that advancing claimed→ready
// (abandon) terminalizes the claim-minted RunAttempt as "abandoned".
func TestClaimAbandonTerminalizesRunAttempt(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "c1-test", SuccessPredicate: "abandon terminalizes"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "codex", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	rattID := claimed.RunAttemptIDs[0]
	// Abandon: advance claimed→ready.
	released, err := s.Advance(w.ID, AdvanceInput{To: "ready", Actor: "codex", Steal: true, StealReason: "c1 test abandon"}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if released.State != "ready" {
		t.Fatalf("state = %q, want ready", released.State)
	}
	// The RunAttempt is now terminal=abandoned.
	ra, err := runattempt.Open(s.home).Get(rattID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Status != runattempt.StatusAbandoned {
		t.Fatalf("RunAttempt status = %q, want abandoned", ra.Status)
	}
	if ra.CompletedAt.IsZero() {
		t.Fatal("abandoned RunAttempt has zero CompletedAt")
	}
}

// TestAbandonedStateDiffersFromNeverClaimed is falsifier (a): state after
// claim+abandon DIFFERS from never-claimed. The attempt file exists and is
// terminal=abandoned; a never-claimed work has no run_attempts at all.
func TestAbandonedStateDiffersFromNeverClaimed(t *testing.T) {
	s := newStore(t)
	// Work A: claimed then abandoned.
	wA, err := s.Create(CreateInput{Owner: "c1-test", SuccessPredicate: "A"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimedA, err := s.Claim(wA.ID, "codex", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	rattA := claimedA.RunAttemptIDs[0]
	if _, err := s.Advance(wA.ID, AdvanceInput{To: "ready", Actor: "codex", Steal: true, StealReason: "abandon A"}, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Work B: never claimed.
	wB, err := s.Create(CreateInput{Owner: "c1-test", SuccessPredicate: "B"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// A has an attempt file; B does not.
	raPathA := filepath.Join(s.home, "run_attempts", rattA+".json")
	if _, err := os.Stat(raPathA); err != nil {
		t.Fatalf("abandoned work A: attempt file missing: %v", err)
	}
	ra, err := runattempt.Open(s.home).Get(rattA)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Status != runattempt.StatusAbandoned {
		t.Fatalf("A attempt status = %q, want abandoned", ra.Status)
	}
	// B has no RunAttemptIDs.
	gotB, err := s.Get(wB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB.RunAttemptIDs) != 0 {
		t.Fatalf("never-claimed B has RunAttemptIDs = %v, want empty", gotB.RunAttemptIDs)
	}
}

// TestDispatchClaimMintsRunAttempt proves that dispatch-claim also mints a
// RunAttempt (it calls Claim internally).
func TestDispatchClaimMintsRunAttempt(t *testing.T) {
	s := newStore(t)
	_, err := s.Create(CreateInput{Owner: "c1-test", SuccessPredicate: "dispatch mints"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.DispatchClaim(fixedNow, DispatchPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "claimed" {
		t.Fatalf("dispatch-claim status = %q, want claimed", result.Status)
	}
	claimed, err := s.Get(result.ClaimedWorkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed.RunAttemptIDs) != 1 {
		t.Fatalf("dispatch-claimed RunAttemptIDs = %v, want 1", claimed.RunAttemptIDs)
	}
	if !runattempt.Valid(claimed.RunAttemptIDs[0]) {
		t.Fatalf("dispatch-claimed RunAttemptID %q is not valid", claimed.RunAttemptIDs[0])
	}
	// The attempt file exists.
	raPath := filepath.Join(s.home, "run_attempts", claimed.RunAttemptIDs[0]+".json")
	if _, err := os.Stat(raPath); err != nil {
		t.Fatalf("dispatch-claimed attempt file missing: %v", err)
	}
}

// TestClaimWithLaneMintsRunAttemptWithLaneHarness proves that ClaimWithLane
// records the resolved lane id as the RunAttempt's harness.
func TestClaimWithLaneMintsRunAttemptWithLaneHarness(t *testing.T) {
	s := newStore(t)
	// Seed a lane node into the CapabilityGraph so ResolveLaneRef succeeds.
	_, nodeID := seedLane(t, s, "codex/gpt-5.6")
	w, err := s.Create(CreateInput{Owner: "c1-test", SuccessPredicate: "lane harness"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimWithLane(w.ID, "codex", "codex/gpt-5.6", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed.RunAttemptIDs) != 1 {
		t.Fatalf("RunAttemptIDs = %v, want 1", claimed.RunAttemptIDs)
	}
	ra, err := runattempt.Open(s.home).Get(claimed.RunAttemptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if ra.Harness != nodeID {
		t.Fatalf("RunAttempt harness = %q, want %q", ra.Harness, nodeID)
	}
}

// TestAbandonedRunAttemptRefusesSetters proves that after abandon, the S2
// setters refuse mutation (the audit invariant extends to abandoned).
func TestAbandonedRunAttemptRefusesSetters(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "c1-test", SuccessPredicate: "setter refusal"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "codex", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	rattID := claimed.RunAttemptIDs[0]
	if _, err := s.Advance(w.ID, AdvanceInput{To: "ready", Actor: "codex", Steal: true, StealReason: "abandon"}, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	store := runattempt.Open(s.home)
	if _, err := store.SetCommand(rattID, "should-fail"); err == nil {
		t.Fatal("SetCommand on abandoned attempt must fail")
	}
	if _, err := store.SetWorkspace(rattID, "/should-fail"); err == nil {
		t.Fatal("SetWorkspace on abandoned attempt must fail")
	}
	if err := store.AttachActionIntent(rattID, "act_12345678"); err == nil {
		t.Fatal("AttachActionIntent on abandoned attempt must fail")
	}
}

// --- C4: retry lineage through claim/release/re-claim ---

func TestClaimRetryChainsPredecessor(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "c4-test", SuccessPredicate: "retry lineage chains"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	// First claim mints attempt #1.
	claim1, err := s.Claim(w.ID, "codex", fixedNow)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(claim1.RunAttemptIDs) != 1 {
		t.Fatalf("first claim RunAttemptIDs = %v, want 1", claim1.RunAttemptIDs)
	}
	firstID := claim1.RunAttemptIDs[0]

	// Release (claimed→ready) abandons attempt #1.
	if _, err := s.Advance(w.ID, AdvanceInput{To: "ready", Actor: "codex"}, fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("release: %v", err)
	}
	ra1, _ := runattempt.Open(s.home).Get(firstID)
	if ra1.Status != runattempt.StatusAbandoned {
		t.Fatalf("first attempt status = %q, want abandoned", ra1.Status)
	}

	// Second claim mints attempt #2, chained to #1.
	claim2, err := s.Claim(w.ID, "codex", fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(claim2.RunAttemptIDs) != 2 {
		t.Fatalf("second claim RunAttemptIDs = %v, want 2", claim2.RunAttemptIDs)
	}
	secondID := claim2.RunAttemptIDs[1]
	if secondID == firstID {
		t.Fatal("second claim must mint a new attempt, not reuse the first")
	}

	// The second attempt's PredecessorID points to the first.
	ra2, err := runattempt.Open(s.home).Get(secondID)
	if err != nil {
		t.Fatal(err)
	}
	if ra2.PredecessorID != firstID {
		t.Fatalf("second attempt predecessor = %q, want %q", ra2.PredecessorID, firstID)
	}

	// The first attempt has no predecessor (it was the first).
	if ra1.PredecessorID != "" {
		t.Fatalf("first attempt predecessor = %q, want empty", ra1.PredecessorID)
	}
}

// failingOriginStore wraps a real *runattempt.Store but forces SetOrigin to
// fail, simulating a store-write fault on origin stamping immediately after
// a successful Mint. Every OTHER write (Mint, SetPredecessor, Complete,
// CompleteWithReason, Get, ...) delegates to the embedded real store, so
// the terminalizing write the claim path performs on the failure path
// still lands on disk. That isolation is the whole point: a blunt
// read-only-fault on the run_attempts directory would also break the
// terminalizing CompleteWithReason, so the test could never observe the
// attempt reaching terminal `failed`. mintedID captures the id Mint
// produced so the test can prove the OnDisk attempt is the freshly
// minted one.
type failingOriginStore struct {
	*runattempt.Store
	mintedID string
}

func (s *failingOriginStore) Mint(workID, actor, harness string, now time.Time) (runattempt.RunAttempt, error) {
	ra, err := s.Store.Mint(workID, actor, harness, now)
	s.mintedID = ra.ID
	return ra, err
}

func (s *failingOriginStore) SetOrigin(_, _ string) (runattempt.RunAttempt, error) {
	return runattempt.RunAttempt{}, errors.New("disk full")
}

// TestClaimSetOriginFailureTerminalizes (E14 twin): a store.SetOrigin
// failure immediately after a successful store.Mint in the claim path
// MUST terminalize the freshly minted reservation RunAttempt as `failed`
// (reason naming the origin-stamp failure) and STILL return a non-nil
// error to the caller. Falsifier: the old code did
// `return w, fmt.Errorf(...)`, leaving a `pending` ratt_ record stranded
// forever (the C3 sweeper would later mislabel it as
// "stale: no heartbeat for 30m", masking the real infra failure).
// Mirrors the harness-execution-lane fix in internal/harness/
// subprocess_runner.go (slice/audit-e14-setorigin-20260724).
func TestClaimSetOriginFailureTerminalizes(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "e14-twin", SuccessPredicate: "setorigin fail terminal"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	real := runattempt.Open(s.home)
	wrapper := &failingOriginStore{Store: real}
	prevFactory := runAttemptsFactory
	runAttemptsFactory = func(home string) runAttemptStore {
		return wrapper
	}
	defer func() { runAttemptsFactory = prevFactory }()

	// (1) Claim returns a non-nil error: a SetOrigin failure is a real
	// infra failure, not a swallowed one.
	_, err = s.Claim(w.ID, "actor-a", fixedNow)
	if err == nil {
		t.Fatal("Claim with SetOrigin failure must still return a non-nil error")
	}

	// (2) The OnDisk ratt_ record is terminal `failed`, NOT `pending`.
	// The returned WorkEnvelope does not advertise the new attempt (the
	// SetOrigin failure path returns early before w.RunAttemptIDs is
	// appended), so we read the record directly via the real store
	// against the wrapper's captured mintedID.
	if wrapper.mintedID == "" {
		t.Fatal("failingOriginStore never observed a Mint call; the claim path did not take the seam")
	}
	got, gerr := real.Get(wrapper.mintedID)
	if gerr != nil {
		t.Fatalf("Get(%s): %v", wrapper.mintedID, gerr)
	}
	if got.Status != runattempt.StatusFailed {
		t.Fatalf("RunAttempt status = %q, want failed (SetOrigin failure must terminalize, not leave pending)", got.Status)
	}

	// (3) The terminal record names the origin-stamp failure. The
	// terminalization write uses CompleteWithReason so the reason
	// lands in the persisted JSON (SweepReason field), readable via
	// either Get or the raw file body.
	if !strings.Contains(got.SweepReason, "stamp origin") {
		t.Fatalf("RunAttempt.SweepReason = %q, want it to name the origin-stamp failure", got.SweepReason)
	}
	body, rerr := os.ReadFile(filepath.Join(s.home, "run_attempts", wrapper.mintedID+".json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(body), "stamp origin") {
		t.Fatalf("persisted RunAttempt JSON must record a reason naming the origin-stamp failure; got:\n%s", body)
	}
}
