package work

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

// Falsifiers for the ownerless-claimed repair predicate. One test per
// refusal dimension named in the predicate (predecessor, attachment,
// action, policy, current-ref, RunID, PolicyDecisionID). Each test:
//
//  1. seeds a wedge (ownerless claimed work + bare-advance lifecycle tail)
//  2. seeds a valid repair transaction in memory and persists its
//     action/policy/ratt_ envelopes on disk so the pending can reference
//     them
//  3. mutates ONE dimension of the pending transaction OR of the
//     supporting envelopes on disk
//  4. captures a post-tamper snapshot of every store (the state Claim
//     actually observes)
//  5. stages the pending on disk
//  6. calls Claim -- the recoverPendingClaim path must refuse the claim
//  7. asserts all four stores (work, event, lease, action) are byte-
//     identical to the snapshot, i.e. the refusal is atomic and never
//     partially writes
//
// Written BEFORE the fix. Some dimensions are already enforced today
// (the test passes pre-fix as a regression); others expose a real gap the
// fix must close (the test fails pre-fix and passes post-fix).

// repairSeed bundles a wedge work envelope with a fully-formed repair
// transaction so each falsifier can mutate one dimension and then stage
// the result. The original envelope IDs (action, policy, ratt_) are
// preserved here so post-tamper snapshots can address the canonical
// files; only the pending transaction struct carries the tampered values.
type repairSeed struct {
	wedge       envelope.WorkEnvelope
	pending     claimPending
	actionID    string
	policyID    string
	rattID      string
	actionPath  string
	policyPath  string
	rattPath    string
	pendingPath string
}

// storeSnapshot captures a byte-exact view of every store relevant to a
// repair claim so the falsifier can compare after the refused Claim.
type storeSnapshot struct {
	workBytes   []byte
	eventBytes  []byte
	actionBytes []byte
	policyBytes []byte
	rattBytes   []byte
}

// snapshotStores reads every store the falsifier cares about. Run this
// AFTER any tampering the test does -- it captures the state Claim will
// observe at the moment of the refused call. The IDs come from the seed
// (original action/policy/ratt_) so the snapshot survives pending-struct
// tampering.
func snapshotStores(t *testing.T, s *Store, seed repairSeed) storeSnapshot {
	t.Helper()
	workBytes, err := os.ReadFile(s.workPath(seed.wedge.ID))
	if err != nil {
		t.Fatalf("snapshot work: %v", err)
	}
	eventBytes, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatalf("snapshot events: %v", err)
	}
	actionBytes, err := s.eventBytes(seed.actionID)
	if err != nil {
		t.Fatalf("snapshot action: %v", err)
	}
	policyBytes, err := s.eventBytes(seed.policyID)
	if err != nil {
		t.Fatalf("snapshot policy: %v", err)
	}
	var rattBytes []byte
	if _, statErr := os.Stat(seed.rattPath); statErr == nil {
		rattBytes, err = os.ReadFile(seed.rattPath)
		if err != nil {
			t.Fatalf("snapshot ratt: %v", err)
		}
	}
	return storeSnapshot{
		workBytes:   workBytes,
		eventBytes:  eventBytes,
		actionBytes: actionBytes,
		policyBytes: policyBytes,
		rattBytes:   rattBytes,
	}
}

// assertByteIdentical fails the test if any of the four stores named in
// the predicate -- work, event, lease, action -- or the supporting
// ratt_ envelope differs from the pre-Claim snapshot. The lease store
// is asserted-absent: a refused claim must not publish one.
func (snap storeSnapshot) assertByteIdentical(t *testing.T, s *Store, seed repairSeed) {
	t.Helper()
	gotWork, err := os.ReadFile(s.workPath(seed.wedge.ID))
	if err != nil {
		t.Fatalf("read work after: %v", err)
	}
	if !bytes.Equal(snap.workBytes, gotWork) {
		t.Fatalf("work store changed on refused claim:\nbefore=%q\nafter=%q", snap.workBytes, gotWork)
	}
	gotEvents, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatalf("read events after: %v", err)
	}
	if !bytes.Equal(snap.eventBytes, gotEvents) {
		t.Fatalf("event store changed on refused claim:\nbefore=%q\nafter=%q", snap.eventBytes, gotEvents)
	}
	if gotLease, err := os.ReadFile(s.leasePath(seed.wedge.ID)); err == nil {
		t.Fatalf("refused claim published a lease: %s", gotLease)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read lease after: %v", err)
	}
	gotAction, err := s.eventBytes(seed.actionID)
	if err != nil {
		t.Fatalf("read action after: %v", err)
	}
	if !bytes.Equal(snap.actionBytes, gotAction) {
		t.Fatalf("action store changed on refused claim:\nbefore=%q\nafter=%q", snap.actionBytes, gotAction)
	}
	gotPolicy, err := s.eventBytes(seed.policyID)
	if err != nil {
		t.Fatalf("read policy after: %v", err)
	}
	if !bytes.Equal(snap.policyBytes, gotPolicy) {
		t.Fatalf("policy store changed on refused claim:\nbefore=%q\nafter=%q", snap.policyBytes, gotPolicy)
	}
	if snap.rattBytes != nil {
		gotRatt, err := os.ReadFile(seed.rattPath)
		if err != nil {
			t.Fatalf("read ratt after: %v", err)
		}
		if !bytes.Equal(snap.rattBytes, gotRatt) {
			t.Fatalf("ratt store changed on refused claim:\nbefore=%q\nafter=%q", snap.rattBytes, gotRatt)
		}
	}
}

// seedRepairTransaction sets up a valid ownerless-claim repair against a
// freshly-installed wedge. The action/policy/ratt_ envelopes are
// persisted to disk (Claim normally would write them) but the pending
// transaction itself is NOT staged -- the caller mutates one dimension
// and stages it.
func seedRepairTransaction(t *testing.T, s *Store) repairSeed {
	t.Helper()
	w := writeOwnerlessClaimWedge(t, s, fixedNow.Add(time.Minute))

	actor := "repair-actor"
	at := fixedNow.Add(2 * time.Minute)

	plan, err := s.ClaimPlan(w.ID, at, ClaimPlanOptions{Actor: actor})
	if err != nil {
		t.Fatalf("claim plan: %v", err)
	}
	if err := s.writeEventJSON(plan.ActionIntent.ID, &plan.ActionIntent); err != nil {
		t.Fatalf("write action: %v", err)
	}
	if err := s.writeEventJSON(plan.Decision.ID, &plan.Decision); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	ra, err := s.runAttemptsClaim().Mint(w.ID, actor, runattempt.HarnessPlaceholder, at)
	if err != nil {
		t.Fatalf("mint ratt: %v", err)
	}
	if _, err := s.runAttemptsClaim().SetOrigin(ra.ID, runattempt.OriginClaimReservation); err != nil {
		t.Fatalf("set origin: %v", err)
	}

	claimed := w
	claimed.State = "claimed"
	claimed.Authority.Owner = actor
	claimed.ActionIntentIDs = append([]string{}, w.ActionIntentIDs...)
	claimed.ActionIntentIDs = append(claimed.ActionIntentIDs, plan.ActionIntent.ID)
	claimed.PolicyDecisionID = plan.Decision.ID
	claimed.RunAttemptIDs = []string{ra.ID}

	previousWorkSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		t.Fatalf("hash wedge: %v", err)
	}
	claimedWorkSHA256, err := workEnvelopeSHA256(claimed)
	if err != nil {
		t.Fatalf("hash claimed: %v", err)
	}

	// Repair event is a same-state advance that retroactively binds
	// authority and proof to the wedge transaction. The seed uses
	// From="ready" (the wedge source) so canTransition("ready",
	// "claimed") passes today and the dimension-specific falsifiers
	// below fail at the dimension they are testing, not at the state
	// machine edge check. The shape test asserts the production repair
	// event uses From="claimed".
	event := Event{
		TS:                 ts(at),
		WorkID:             w.ID,
		From:               "ready",
		To:                 "claimed",
		By:                 actor,
		Note:               "ownerless-claim repair (policy " + plan.Policy.Outcome + ")",
		Attached:           []string{plan.ActionIntent.ID, plan.Decision.ID, ra.ID},
		PreviousWorkSHA256: previousWorkSHA256,
		WorkSHA256:         claimedWorkSHA256,
	}
	lease := Lease{WorkID: w.ID, By: actor, ClaimAt: event.TS, WorkSHA256: claimedWorkSHA256}

	pending := claimPending{
		SchemaVersion:      claimPendingSchemaVersion,
		WorkID:             w.ID,
		Actor:              actor,
		PreviousWorkSHA256: previousWorkSHA256,
		ClaimedWork:        claimed,
		ClaimedWorkSHA256:  claimedWorkSHA256,
		Lease:              lease,
		ActionIntentID:     plan.ActionIntent.ID,
		PolicyDecisionID:   plan.Decision.ID,
		RunAttemptID:       ra.ID,
		RepairOwnerless:    true,
		Event:              event,
	}

	rattPath := filepath.Join(s.home, "runattempts", ra.ID+".json")

	return repairSeed{
		wedge:       w,
		pending:     pending,
		actionID:    plan.ActionIntent.ID,
		policyID:    plan.Decision.ID,
		rattID:      ra.ID,
		actionPath:  s.envPath(plan.ActionIntent.ID),
		policyPath:  s.envPath(plan.Decision.ID),
		rattPath:    rattPath,
		pendingPath: s.claimPendingPath(w.ID),
	}
}

// stagePending writes the (possibly tampered) claimPending to disk so the
// next Claim call hits recoverPendingClaim's validation path.
func stagePending(t *testing.T, s *Store, pending claimPending) {
	t.Helper()
	if err := s.writeJSON(s.claimPendingPath(pending.WorkID), &pending); err != nil {
		t.Fatalf("stage pending: %v", err)
	}
}

// runRefusedClaim stages the pending, calls Claim, and asserts the call
// refused. The snapshot must be taken by the caller BEFORE this helper
// runs -- it captures whatever the test most recently put on disk.
func runRefusedClaim(t *testing.T, s *Store, seed repairSeed, pending claimPending) {
	t.Helper()
	stagePending(t, s, pending)
	if _, err := s.Claim(seed.wedge.ID, "repair-actor", fixedNow.Add(3*time.Minute)); err == nil {
		t.Fatal("Claim with tampered repair pending must refuse")
	}
}

// ---- Falsifiers ----

// Falsifier 1 -- predecessor mismatch refuses.
func TestRepairFalsifier_PredecessorMismatchRefuses(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seed := seedRepairTransaction(t, s)
	snap := snapshotStores(t, s, seed)

	tampered := seed.pending
	// The repair event's preimage hash must equal the wedge's claimed
	// hash. Force a wrong hash to simulate a missing/mismatched
	// predecessor binding.
	tampered.PreviousWorkSHA256 = "sha256:" + strings.Repeat("0", 64)

	runRefusedClaim(t, s, seed, tampered)
	snap.assertByteIdentical(t, s, seed)
}

// Falsifier 2 -- attachment missing refuses.
func TestRepairFalsifier_AttachmentMissingRefuses(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seed := seedRepairTransaction(t, s)
	snap := snapshotStores(t, s, seed)

	tampered := seed.pending
	// Drop the run-attachment from the event so the attachment triple
	// (action, policy, ratt_*) is incomplete.
	tampered.Event.Attached = []string{tampered.ActionIntentID, tampered.PolicyDecisionID}

	runRefusedClaim(t, s, seed, tampered)
	snap.assertByteIdentical(t, s, seed)
}

// Falsifier 3 -- action tampered refuses.
func TestRepairFalsifier_ActionTamperedRefuses(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seed := seedRepairTransaction(t, s)

	// Replace the action envelope's actor so the claim action no longer
	// agrees with the lifecycle event's By actor.
	var action envelope.ActionIntent
	if err := s.readEventJSON(seed.actionID, &action); err != nil {
		t.Fatalf("read action: %v", err)
	}
	action.Actor = "rogue-actor"
	if err := s.replaceEvent(seed.actionID, &action); err != nil {
		t.Fatalf("replace action: %v", err)
	}
	snap := snapshotStores(t, s, seed)

	runRefusedClaim(t, s, seed, seed.pending)
	snap.assertByteIdentical(t, s, seed)
}

// Falsifier 4 -- policy tampered refuses.
func TestRepairFalsifier_PolicyTamperedRefuses(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seed := seedRepairTransaction(t, s)

	// Re-point the policy decision at a different ActionIntentID so the
	// decision no longer authorises the action it is paired with.
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(seed.policyID, &decision); err != nil {
		t.Fatalf("read policy: %v", err)
	}
	decision.ActionIntentID = "act_unrelated"
	if err := s.replaceEvent(seed.policyID, &decision); err != nil {
		t.Fatalf("replace policy: %v", err)
	}
	snap := snapshotStores(t, s, seed)

	runRefusedClaim(t, s, seed, seed.pending)
	snap.assertByteIdentical(t, s, seed)
}

// Falsifier 5 -- current-ref mismatch refuses.
func TestRepairFalsifier_CurrentRefMismatchRefuses(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seed := seedRepairTransaction(t, s)
	snap := snapshotStores(t, s, seed)

	tampered := seed.pending
	// ClaimedWorkSHA256 is the current-ref binding into the published
	// WorkEnvelope. A wrong hash means the pending references a
	// WorkEnvelope that does not exist on disk -- the recoverPendingClaim
	// current-vs-pending check must refuse.
	tampered.ClaimedWorkSHA256 = "sha256:" + strings.Repeat("1", 64)

	runRefusedClaim(t, s, seed, tampered)
	snap.assertByteIdentical(t, s, seed)
}

// Falsifier 6 -- RunID mismatch refuses.
//
// Today this dimension is under-enforced: validateClaimEventArtifacts
// checks the ratt_ prefix on the event attachment but does not bind the
// pending.RunAttemptID against the envelope's RunAttemptIDs. After the
// fix this refusal fires.
func TestRepairFalsifier_RunIDMismatchRefuses(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seed := seedRepairTransaction(t, s)
	snap := snapshotStores(t, s, seed)

	tampered := seed.pending
	// Repoint the pending at a ratt_ id that does not exist on disk; the
	// claimed Work's RunAttemptIDs still point at the real ratt_, so the
	// binding between pending.RunAttemptID and ClaimedWork.RunAttemptIDs
	// is broken.
	tampered.RunAttemptID = "ratt_does_not_exist_0000000000000000"
	tampered.Event.Attached = []string{tampered.ActionIntentID, tampered.PolicyDecisionID, tampered.RunAttemptID}

	runRefusedClaim(t, s, seed, tampered)
	snap.assertByteIdentical(t, s, seed)
}

// Falsifier 7 -- PolicyDecisionID mismatch refuses.
func TestRepairFalsifier_PolicyDecisionIDMismatchRefuses(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seed := seedRepairTransaction(t, s)
	snap := snapshotStores(t, s, seed)

	tampered := seed.pending
	// pending.PolicyDecisionID is the policy decision that authorised
	// the repair claim; ClaimedWork.PolicyDecisionID is the same value
	// staked onto the envelope. Force the top-level field away from the
	// envelope's value to simulate a missing/mismatched binding.
	tampered.PolicyDecisionID = "pol_unrelated"

	runRefusedClaim(t, s, seed, tampered)
	snap.assertByteIdentical(t, s, seed)
}

// Shape -- the successful repair event is claimed-to-claimed, not
// ready-to-claimed. The repair event binds the wedge's claimed
// preimage hash and the new claimed postimage hash; the lifecycle
// From/To fields must agree (a same-state binding), not borrow the
// wedge's source state.
func TestRepairEventFromIsClaimedToClaimed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := writeOwnerlessClaimWedge(t, s, fixedNow.Add(time.Minute))

	repaired, err := s.Claim(w.ID, "repair-actor", fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("repair claim: %v", err)
	}
	events, err := s.eventsFor(repaired.ID)
	if err != nil {
		t.Fatalf("events for: %v", err)
	}
	var repairEvent Event
	for i := len(events) - 1; i >= 0; i-- {
		if strings.Contains(events[i].Note, "ownerless-claim repair") {
			repairEvent = events[i]
			break
		}
	}
	if repairEvent.WorkID == "" {
		t.Fatalf("repair lifecycle event missing: %+v", events)
	}
	if repairEvent.From != "claimed" || repairEvent.To != "claimed" {
		t.Fatalf("repair event From/To = %s/%s, want claimed/claimed", repairEvent.From, repairEvent.To)
	}
	previousHash, err := workEnvelopeSHA256(w)
	if err != nil {
		t.Fatalf("hash wedge: %v", err)
	}
	if repairEvent.PreviousWorkSHA256 != previousHash {
		t.Fatalf("repair preimage = %q, want %q", repairEvent.PreviousWorkSHA256, previousHash)
	}
	postimageHash, err := workEnvelopeSHA256(repaired)
	if err != nil {
		t.Fatalf("hash repaired: %v", err)
	}
	if repairEvent.WorkSHA256 != postimageHash {
		t.Fatalf("repair postimage = %q, want %q", repairEvent.WorkSHA256, postimageHash)
	}
}
