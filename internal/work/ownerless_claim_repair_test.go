package work

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

const claimedAdvanceRefusal = "claimed is reached only via 'compound work claim' (claim-before-you-touch); bare advance cannot bind authority owner+lease"

func TestAdvanceToClaimedRefusedWithoutBinding(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "bare advance cannot claim work"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	_, beforeEvents, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Advance(w.ID, AdvanceInput{To: "claimed", Actor: "actor-a"}, fixedNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), claimedAdvanceRefusal) {
		t.Fatalf("Advance ready -> claimed error = %v, want actionable claim-verb refusal", err)
	}

	after, afterEvents, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "ready" || after.Authority.Owner != "" {
		t.Fatalf("refused advance mutated work: %+v", after)
	}
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("refused advance appended lifecycle events: before=%+v after=%+v", beforeEvents, afterEvents)
	}
	if _, err := s.readLease(w.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused advance lease error = %v, want not-exist", err)
	}
}

func TestAdvancePlanRefusesCrossStateClaimed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "advance plan cannot propose bare claim"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AdvancePlan(w.ID, fixedNow.Add(time.Minute), AdvancePlanOptions{Actor: "actor-a", To: "claimed"}); err == nil || !strings.Contains(err.Error(), claimedAdvanceRefusal) {
		t.Fatalf("AdvancePlan ready -> claimed error = %v, want actionable claim-verb refusal", err)
	}

	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "ready" || after.Authority.Owner != "" {
		t.Fatalf("refused advance plan mutated work: %+v", after)
	}
	if _, err := s.readLease(w.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused advance plan lease error = %v, want not-exist", err)
	}
}

func TestSameStateAdvanceOnClaimedStillAllowed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "claimed work accepts delivery attachments"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "actor-a", fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	advanced, err := s.Advance(w.ID, AdvanceInput{
		To:         "claimed",
		Actor:      "actor-a",
		ArtifactID: "artifact_delivery",
		Note:       "attach delivery proof",
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("same-state advance on claimed work: %v", err)
	}
	if advanced.State != "claimed" || advanced.Authority.Owner != "actor-a" || !contains(advanced.ArtifactIDs, "artifact_delivery") {
		t.Fatalf("same-state claimed advance = %+v", advanced)
	}
	if len(advanced.ActionIntentIDs) != len(claimed.ActionIntentIDs)+1 {
		t.Fatalf("same-state advance action chain = %v, claimed=%v", advanced.ActionIntentIDs, claimed.ActionIntentIDs)
	}
	lease, err := s.readLease(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.By != "actor-a" {
		t.Fatalf("same-state advance changed lease holder: %+v", lease)
	}
}

func TestClaimRepairsOwnerlessClaimedEnvelope(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := writeOwnerlessClaimWedge(t, s, fixedNow.Add(time.Minute))
	beforeRuns := len(w.ActionIntentIDs)

	repaired, err := s.Claim(w.ID, "repair-actor", fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("repair ownerless claimed work: %v", err)
	}
	if repaired.State != "claimed" || repaired.Authority.Owner != "repair-actor" {
		t.Fatalf("repaired work = %+v", repaired)
	}
	if repaired.PolicyDecisionID == "" || len(repaired.ActionIntentIDs) != beforeRuns+1 {
		t.Fatalf("repair must record policy/action chain: runs=%v policy=%q", repaired.ActionIntentIDs, repaired.PolicyDecisionID)
	}
	if exists, err := s.eventExists(repaired.ActionIntentIDs[beforeRuns]); err != nil || !exists {
		t.Fatalf("repair claim action envelope missing: %v", err)
	}
	if exists, err := s.eventExists(repaired.PolicyDecisionID); err != nil || !exists {
		t.Fatalf("repair claim policy envelope missing: %v", err)
	}
	lease, err := s.readLease(w.ID)
	if err != nil {
		t.Fatalf("read repaired lease: %v", err)
	}
	if lease.By != "repair-actor" || lease.WorkID != w.ID || lease.WorkSHA256 == "" {
		t.Fatalf("repaired lease = %+v", lease)
	}
	_, events, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1].Note, "ownerless-claim repair") {
		t.Fatalf("repair lifecycle tail = %+v", events)
	}

	running, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "repair-actor"}, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("advance repaired claim to running: %v", err)
	}
	if running.State != "running" {
		t.Fatalf("repaired advance state = %q, want running", running.State)
	}
}

func TestClaimOnOwnedClaimedStillRefused(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "owned claim remains exclusive"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Claim(w.ID, "actor-b", fixedNow.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), `already claimed by "actor-a"`) {
		t.Fatalf("second claim error = %v, want already-claimed-by refusal", err)
	}
}

func writeOwnerlessClaimWedge(t *testing.T, s *Store, at time.Time) envelope.WorkEnvelope {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "ownerless claimed work can be repaired"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	previousWorkSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		t.Fatal(err)
	}
	w.State = "claimed"
	w.Authority.Owner = ""
	claimedWorkSHA256, err := workEnvelopeSHA256(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.appendEvent(Event{
		TS:                 ts(at),
		WorkID:             w.ID,
		From:               "ready",
		To:                 "claimed",
		By:                 "legacy-advance",
		Note:               "advanced",
		PreviousWorkSHA256: previousWorkSHA256,
		WorkSHA256:         claimedWorkSHA256,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeJSON(s.workPath(w.ID), &w); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readLease(w.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wedge fixture lease error = %v, want not-exist", err)
	}
	return w
}
