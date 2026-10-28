package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

const (
	proofActorAlpha = "codex:proof-alpha"
	proofActorBeta  = "codex:proof-beta"
)

func TestProofResourceReservationExclusiveUntilReleaseAndGovernedResultReachable(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "first proof work", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "second proof work", proofActorBeta, fixedNow.Add(time.Second))

	acquired, err := reserveProofResource(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if acquired.Reservation.Status != ProofResourceAcquired || acquired.Reservation.Holder == "" {
		t.Fatalf("first reservation = %+v, want acquired holder", acquired.Reservation)
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusRunning)
	assertGovernedProofReservation(t, s, acquired.Reservation)

	refused, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
	}, fixedNow.Add(3*time.Second))
	var held *ProofResourceHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second concurrent reserve = %v, want *ProofResourceHeldError", err)
	}
	if held.Holder != acquired.Reservation.Holder || held.ReservationID == "" {
		t.Fatalf("held refusal = %+v, want current holder and recorded reservation id", held)
	}
	if refused.Reservation.RunAttemptID == "" {
		t.Fatal("refused admission omitted its terminal RunAttempt")
	}
	assertProofRunAttempt(t, s, refused.Reservation, runattempt.StatusAbandoned)
	assertLifecycleAttachment(t, s, second.ID, held.ReservationID)

	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteProofResource(first.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("complete first proof: %v", err)
	}
	if completed.VerificationRef == "" || completed.Result.Status != ProofResourceVerified {
		t.Fatalf("completed result = %+v, want governed verification", completed)
	}
	var persistedResult ProofResourceResult
	if err := s.readEventJSON(completed.Result.ID, &persistedResult); err != nil {
		t.Fatal(err)
	}
	if persistedResult.RunAttemptStatus != runattempt.StatusSucceeded ||
		persistedResult.VerificationRef != completed.VerificationRef {
		t.Fatalf(
			"persisted result run status/ref = %q/%q, want %q/%q",
			persistedResult.RunAttemptStatus,
			persistedResult.VerificationRef,
			runattempt.StatusSucceeded,
			completed.VerificationRef,
		)
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusSucceeded)
	afterFirst, err := s.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(afterFirst.VerificationIDs, completed.VerificationRef) {
		t.Fatalf("work verification_ids = %v, want %q", afterFirst.VerificationIDs, completed.VerificationRef)
	}
	if !contains(afterFirst.ActionIntentIDs, completed.Result.ActionIntentID) ||
		afterFirst.PolicyDecisionID != completed.Result.PolicyDecisionID {
		t.Fatalf("completion governance lost from final Work: actions=%v policy=%q result=%+v", afterFirst.ActionIntentIDs, afterFirst.PolicyDecisionID, completed.Result)
	}
	assertLifecycleAttachment(t, s, first.ID, acquired.Reservation.ID)
	assertLifecycleAttachment(t, s, first.ID, completed.Result.ID)

	successor, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
	}, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("successor reserve after release: %v", err)
	}
	if successor.Reservation.Status != ProofResourceAcquired {
		t.Fatalf("successor status = %q, want %q", successor.Reservation.Status, ProofResourceAcquired)
	}
}

func TestCloseFailedWorkPreservesCommittedVerifiedProofRunAttemptOutcome(t *testing.T) {
	t.Parallel()
	fixture := newSourceBoundProofCompletionFixture(t, fixedNow)
	completed, err := fixture.store.CompleteProofResource(fixture.item.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     fixture.reserved.Reservation.ID,
		ReportArtifactRef: fixture.reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Advance(fixture.item.ID, AdvanceInput{To: "running", Actor: proofActorAlpha}, fixedNow.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	closed, err := fixture.store.Close(fixture.item.ID, CloseInput{
		Outcome:  "failed",
		MoveType: "audit_only",
		Actor:    proofActorAlpha,
		Summary:  "task failed after its certifying subprocess completed",
	}, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("close failed work with committed verified proof: %v", err)
	}
	if closed.Work.State != "failed" {
		t.Fatalf("closed work state = %q, want failed", closed.Work.State)
	}
	attempt, err := fixture.store.runAttempts().Get(completed.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("verified proof RunAttempt status = %q, want succeeded execution evidence", attempt.Status)
	}
	proof, err := fixture.store.ProofChainForWork(fixture.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.ProofComplete || len(proof.Problems) != 0 {
		t.Fatalf("failed work proof chain rejected typed verified proof RunAttempt: %+v", proof)
	}
}

func TestCloseFailedWorkRejectsProofShapedSucceededAttemptWithoutCommittedFinalizer(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "reject proof-shaped attempt without finalizer", proofActorAlpha, fixedNow)
	attempt, err := s.runAttempts().Mint(w.ID, proofActorAlpha, proofResourceHarness, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.runAttempts().SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runAttempts().Complete(attempt.ID, runattempt.StatusSucceeded, fixedNow.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{
		Actor:          proofActorAlpha,
		To:             "running",
		RunAttemptIDs:  []string{attempt.ID},
		VerificationID: "proof-check:pfrs_missing_finalizer",
	}, fixedNow.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}

	_, err = s.Close(w.ID, CloseInput{
		Outcome:  "failed",
		MoveType: "audit_only",
		Actor:    proofActorAlpha,
		Summary:  "must not trust a proof-shaped harness attempt",
	}, fixedNow.Add(5*time.Second))
	if err == nil || !strings.Contains(err.Error(), "proof-resource exception invalid") {
		t.Fatalf("close with proof-shaped uncommitted attempt error = %v, want fail-closed proof-resource error", err)
	}
	after, getErr := s.Get(w.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.State != "running" {
		t.Fatalf("failed close mutated work state = %q, want running", after.State)
	}
}

func TestProofResourceCompleteCanonicalAllFailedNullProofRefsReleasesWithoutVerification(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "complete failed proof work", proofActorAlpha, fixedNow)
	successorWork := claimedProofWork(t, s, "proof after complete failure", proofActorBeta, fixedNow.Add(time.Second))

	acquired, err := reserveProofResource(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatalf("reserve failed proof: %v", err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(allFailedOwnedProofReportJSONWithNullRefs())
	if err != nil {
		t.Fatal(err)
	}

	completed, err := s.CompleteProofResource(first.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  1,
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("complete canonical failed report: %v", err)
	}
	if completed.Result.Status != ProofResourceFailed ||
		completed.Result.ReportStatus != "failed" ||
		!completed.Result.OwnedReport ||
		!completed.Result.ProducerExitKnown ||
		completed.Result.ProducerExitCode != 1 ||
		completed.VerificationRef != "" ||
		!completed.ResourceReleased {
		t.Fatalf("failed completion = %+v, want owned non-certifying result and released resource", completed)
	}
	var persistedResult ProofResourceResult
	if err := s.readEventJSON(completed.Result.ID, &persistedResult); err != nil {
		t.Fatal(err)
	}
	if persistedResult.RunAttemptStatus != runattempt.StatusFailed ||
		persistedResult.VerificationRef != "" {
		t.Fatalf(
			"persisted failed result run status/ref = %q/%q, want %q/empty",
			persistedResult.RunAttemptStatus,
			persistedResult.VerificationRef,
			runattempt.StatusFailed,
		)
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusFailed)
	after, err := s.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.VerificationIDs) != 0 {
		t.Fatalf("failed proof entered verification_ids: %v", after.VerificationIDs)
	}
	pending, err := os.ReadDir(filepath.Join(s.home, "work", "proof-resource-completions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("failed proof retained completion journal: %v", pending)
	}

	successor, err := reserveProofResource(t, s, successorWork.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
	}, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("successor reserve after complete failed proof: %v", err)
	}
	if successor.Reservation.Status != ProofResourceAcquired {
		t.Fatalf("successor reservation = %+v, want acquired", successor.Reservation)
	}
}

func TestProofResourceReservationExpiryRetainsBarrierUntilTerminationIsGoverned(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "expiring proof work", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "recovery proof work", proofActorBeta, fixedNow.Add(time.Second))
	predecessor := startStoppedProofProcess(t)
	writeProofResourceStealGrant(t, s.home)

	acquired, err := reserveProofResourceWithLiveBarrier(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
	}, fixedNow.Add(31*time.Second)); err == nil {
		t.Fatal("successor acquired before predecessor expiry")
	}

	barrier, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
	}, fixedNow.Add(33*time.Second))
	if err == nil {
		t.Fatalf("successor after expiry = result=%+v err=%v, want non-executable execution barrier", barrier, err)
	}
	if barrier.Reservation.Status != ProofResourceRefused ||
		barrier.Reservation.ConflictingHolder != acquired.Reservation.Holder {
		t.Fatalf("expiry barrier refusal = %+v", barrier.Reservation)
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusRunning)
	assertProofRunAttempt(t, s, barrier.Reservation, runattempt.StatusAbandoned)
	if barrier.Reservation.PriorHolder != "" || barrier.Reservation.StealReason != "" {
		t.Fatalf("expiry barrier masqueraded as steal: %+v", barrier.Reservation)
	}
	assertLifecycleAttachment(t, s, second.ID, barrier.Reservation.ID)
	if predecessor.terminated() {
		t.Fatal("state-only expiry recovery reported the stopped predecessor process terminated")
	}

	status, err := landlease.Status(s.home, fixedNow.Add(33*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Holder != acquired.Reservation.Holder {
		t.Fatalf("expiry refusal did not retain the non-executable barrier: %+v", status)
	}

	predecessor.terminate(t)
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	recovered, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:                  proofActorBeta,
		TTL:                    30 * time.Second,
		Workspace:              "/synthetic/repo-beta",
		Command:                "compound proof-check --out /synthetic/report-beta.json --steal",
		Steal:                  true,
		StealReason:            "predecessor process group was terminated and waited",
		TerminatedRunAttemptID: acquired.Reservation.RunAttemptID,
	}, fixedNow.Add(34*time.Second))
	if err != nil {
		t.Fatalf("governed stale recovery after process termination: %v", err)
	}
	if recovered.Reservation.TerminatedRunAttemptID != acquired.Reservation.RunAttemptID ||
		recovered.Reservation.TerminationEvidenceRef == "" ||
		recovered.Reservation.TerminationConfirmedAt == "" {
		t.Fatalf("termination evidence was not bound to recovered reservation: %+v", recovered.Reservation)
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusAbandoned)
	assertLifecycleAttachment(t, s, second.ID, recovered.Reservation.TerminationEvidenceRef)
	var action envelope.ActionIntent
	if err := s.readEventJSON(recovered.Reservation.ActionIntentID, &action); err != nil {
		t.Fatal(err)
	}
	if action.Action != "steal_full_proof_resource_after_termination_evidence" ||
		!strings.Contains(action.Preview, acquired.Reservation.RunAttemptID) ||
		!strings.Contains(action.Preview, recovered.Reservation.TerminationEvidenceRef) {
		t.Fatalf("termination-evidenced steal ActionIntent = %+v", action)
	}
}

func TestProofResourceChainedStealWaitsForOldestNonterminalPredecessor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "stopped original proof", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "first takeover barrier", proofActorBeta, fixedNow.Add(time.Second))
	thirdActor := "codex:proof-gamma"
	third := claimedProofWork(t, s, "second takeover barrier", thirdActor, fixedNow.Add(2*time.Second))
	predecessor := startStoppedProofProcess(t)
	writeProofResourceStealGrant(t, s.home)

	acquired, err := reserveProofResourceWithLiveBarrier(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	firstTakeoverDone := make(chan error, 1)
	var firstTakeoverRunnerStarts atomic.Int32
	go func() {
		_, reserveErr := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
			Actor:       proofActorBeta,
			TTL:         time.Second,
			Workspace:   "/synthetic/repo-beta",
			Command:     "compound proof-check --out /synthetic/report-beta.json --steal",
			Steal:       true,
			StealReason: "first governed takeover waits on stopped predecessor",
		}, fixedNow.Add(4*time.Second))
		if reserveErr == nil {
			firstTakeoverRunnerStarts.Add(1)
		}
		firstTakeoverDone <- reserveErr
	}()
	// Shared execution barrier prevents the intermediate holder from ever
	// publishing, so the next contender still observes the original holder.

	secondTakeoverDone := make(chan error, 1)
	var secondTakeoverRunnerStarts atomic.Int32
	go func() {
		_, reserveErr := reserveProofResource(t, s, third.ID, ProofResourceReserveInput{
			Actor:       thirdActor,
			TTL:         3 * time.Second,
			Workspace:   "/synthetic/repo-gamma",
			Command:     "compound proof-check --out /synthetic/report-gamma.json --steal",
			Steal:       true,
			StealReason: "second governed takeover must retain the original barrier",
		}, fixedNow.Add(5*time.Second))
		if reserveErr == nil {
			secondTakeoverRunnerStarts.Add(1)
		}
		secondTakeoverDone <- reserveErr
	}()

	if err := <-firstTakeoverDone; err == nil {
		t.Fatal("first takeover became executable while original predecessor was stopped")
	}
	if err := <-secondTakeoverDone; err == nil {
		t.Fatal("second takeover bypassed the original stopped predecessor")
	}
	if firstTakeoverRunnerStarts.Load() != 0 || secondTakeoverRunnerStarts.Load() != 0 {
		t.Fatalf(
			"takeover runner starts = first:%d second:%d, want zero before original termination",
			firstTakeoverRunnerStarts.Load(),
			secondTakeoverRunnerStarts.Load(),
		)
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusRunning)
	if predecessor.terminated() {
		t.Fatal("chained state-only steals reported the stopped original process terminated")
	}

	predecessor.terminate(t)
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	recovered, err := reserveProofResource(t, s, third.ID, ProofResourceReserveInput{
		Actor:                  thirdActor,
		TTL:                    3 * time.Second,
		Workspace:              "/synthetic/repo-gamma",
		Command:                "compound proof-check --out /synthetic/report-gamma.json --steal",
		Steal:                  true,
		StealReason:            "oldest predecessor process group was terminated and waited",
		TerminatedRunAttemptID: acquired.Reservation.RunAttemptID,
	}, fixedNow.Add(7*time.Second))
	if err != nil {
		t.Fatalf("chained recovery after original termination evidence: %v", err)
	}
	if recovered.Reservation.TerminatedRunAttemptID != acquired.Reservation.RunAttemptID {
		t.Fatalf("chained recovery targeted %q, want oldest predecessor %q", recovered.Reservation.TerminatedRunAttemptID, acquired.Reservation.RunAttemptID)
	}
	secondTakeoverRunnerStarts.Add(1)
	if secondTakeoverRunnerStarts.Load() != 1 {
		t.Fatalf("successor runner starts after governed original termination = %d, want one", secondTakeoverRunnerStarts.Load())
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusAbandoned)
}

func TestProofResourceRecoveryLineageFailsClosedOnAmbiguityAndCycles(t *testing.T) {
	t.Parallel()
	t.Run("ambiguous predecessor branches", func(t *testing.T) {
		s := newStore(t)
		left := writeTestProofRecoveryNode(t, s, "pfr_lineage_left", "", "", fixedNow)
		right := writeTestProofRecoveryNode(t, s, "pfr_lineage_right", "", "", fixedNow.Add(time.Second))
		root := writeTestProofRecoveryNode(
			t,
			s,
			"pfr_lineage_root",
			left,
			right,
			fixedNow.Add(2*time.Second),
		)
		if _, err := s.proofRecoveryOldestNonterminal(root); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("ambiguous recovery lineage error = %v, want fail-closed ambiguity", err)
		}
	})

	t.Run("predecessor cycle", func(t *testing.T) {
		s := newStore(t)
		workA := claimedProofWork(t, s, "cycle a", proofActorAlpha, fixedNow)
		workB := claimedProofWork(t, s, "cycle b", proofActorBeta, fixedNow.Add(time.Second))
		runA, err := s.runAttempts().Mint(workA.ID, proofActorAlpha, proofResourceHarness, fixedNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		runB, err := s.runAttempts().Mint(workB.ID, proofActorBeta, proofResourceHarness, fixedNow.Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		holderA := proofResourceHolder("pfr_cycle_a", runA.ID, workA.ID, proofActorAlpha)
		holderB := proofResourceHolder("pfr_cycle_b", runB.ID, workB.ID, proofActorBeta)
		for _, reservation := range []ProofResourceReservation{
			{
				SchemaVersion: ProofResourceReservationSchemaVersion,
				ID:            "pfr_cycle_a",
				WorkID:        workA.ID,
				Actor:         proofActorAlpha,
				RunAttemptID:  runA.ID,
				PriorHolder:   holderB,
			},
			{
				SchemaVersion: ProofResourceReservationSchemaVersion,
				ID:            "pfr_cycle_b",
				WorkID:        workB.ID,
				Actor:         proofActorBeta,
				RunAttemptID:  runB.ID,
				PriorHolder:   holderA,
			},
		} {
			if err := s.writeEventJSON(reservation.ID, &reservation); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.proofRecoveryOldestNonterminal(holderA); err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("cyclic recovery lineage error = %v, want fail-closed cycle", err)
		}
	})
}

func TestProofResourceTerminationEvidenceMustBindExactRunAttempt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "termination evidence target", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "termination evidence verifier", proofActorBeta, fixedNow.Add(time.Second))
	writeProofResourceStealGrant(t, s.home)
	acquired, err := reserveProofResourceWithLiveBarrier(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body []byte
	}{
		{name: "malformed", body: []byte(`not-json`)},
		{name: "different RunAttempt", body: mustJSON(t, ProofResourceTerminationEvidence{
			SchemaVersion: ProofResourceTerminationEvidenceSchemaVersion,
			RunAttemptID:  "ratt_20260724T120000Z_different",
			Status:        "terminated_and_waited",
			ObservedAt:    fixedNow.Format(time.RFC3339Nano),
			Observer:      "test-supervisor",
		})},
		{name: "not waited", body: mustJSON(t, ProofResourceTerminationEvidence{
			SchemaVersion: ProofResourceTerminationEvidenceSchemaVersion,
			RunAttemptID:  acquired.Reservation.RunAttemptID,
			Status:        "signal_sent",
			ObservedAt:    fixedNow.Format(time.RFC3339Nano),
			Observer:      "test-supervisor",
		})},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := artifacts.Put(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			_, err = reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
				Actor:                  proofActorBeta,
				TTL:                    time.Minute,
				Workspace:              "/synthetic/repo-beta",
				Command:                "compound proof-check --out /synthetic/report-beta.json --steal",
				Steal:                  true,
				StealReason:            "test invalid external termination evidence",
				TerminatedRunAttemptID: acquired.Reservation.RunAttemptID,
				TerminationEvidenceRef: ref,
			}, fixedNow.Add(time.Duration(3+i)*time.Second))
			if err == nil || !strings.Contains(err.Error(), "termination evidence") {
				t.Fatalf("invalid termination evidence error = %v", err)
			}
		})
	}
	after, err := s.Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RunAttemptIDs) != len(before.RunAttemptIDs) ||
		len(after.ActionIntentIDs) != len(before.ActionIntentIDs) {
		t.Fatalf("invalid termination evidence mutated governed work: before=%+v after=%+v", before, after)
	}
	status, err := landlease.Status(s.home, fixedNow.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Holder != acquired.Reservation.Holder {
		t.Fatalf("invalid termination evidence changed scarce holder: %+v", status)
	}
}

func TestProofResourceProducesTrustedTerminationEvidenceFromReleasedBarrier(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "trusted termination predecessor", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "trusted termination successor", proofActorBeta, fixedNow.Add(time.Second))
	writeProofResourceStealGrant(t, s.home)
	acquired, err := reserveProofResourceWithLiveBarrier(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	recovered, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:                  proofActorBeta,
		TTL:                    time.Minute,
		Workspace:              "/synthetic/repo-beta",
		Command:                "compound proof-check --out /synthetic/report-beta.json --steal",
		Steal:                  true,
		StealReason:            "trusted predecessor execution barrier released",
		TerminatedRunAttemptID: acquired.Reservation.RunAttemptID,
	}, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Reservation.TerminationEvidenceRef == "" {
		t.Fatal("governed recovery omitted trusted termination evidence")
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	body, err := artifacts.Get(recovered.Reservation.TerminationEvidenceRef)
	if err != nil {
		t.Fatal(err)
	}
	var evidence ProofResourceTerminationEvidence
	if err := json.Unmarshal(body, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Producer != "compound.landlease.execution-barrier" ||
		evidence.BarrierID == "" ||
		evidence.Holder != acquired.Reservation.Holder {
		t.Fatalf("trusted termination evidence = %+v", evidence)
	}
}

func TestProofResourceTerminationEvidencePublicationRequiresPolicyAllow(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "governed termination evidence predecessor", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "governed termination evidence successor", proofActorBeta, fixedNow.Add(time.Second))
	acquired, err := reserveProofResourceWithLiveBarrier(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(s.home, "artifacts", "sha256")
	before, err := os.ReadDir(artifactDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	blocked, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:                  proofActorBeta,
		TTL:                    time.Minute,
		Workspace:              "/synthetic/repo-beta",
		Command:                "compound proof-check --out /synthetic/report-beta.json --steal",
		Steal:                  true,
		StealReason:            "recover only after trusted termination attestation",
		TerminatedRunAttemptID: acquired.Reservation.RunAttemptID,
	}, fixedNow.Add(3*time.Second))
	if err == nil || blocked.Reservation.PolicyDecisionID == "" {
		t.Fatalf("default policy did not govern termination-evidenced steal: result=%+v err=%v", blocked, err)
	}
	after, readErr := os.ReadDir(artifactDir)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	if len(after) != len(before) {
		t.Fatalf("policy-refused termination evidence mutated artifact custody: before=%d after=%d", len(before), len(after))
	}
}

func TestProofResourceReservationHeartbeatExtendsOnlyTheCurrentHolder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "heartbeat proof work", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "heartbeat successor work", proofActorBeta, fixedNow.Add(time.Second))

	acquired, err := reserveProofResource(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := s.RefreshProofResource(
		first.ID,
		acquired.Reservation.ID,
		proofActorAlpha,
		30*time.Second,
		fixedNow.Add(20*time.Second),
	)
	if err != nil {
		t.Fatalf("refresh current holder: %v", err)
	}
	if refreshed.ExpiresAt == acquired.Reservation.ExpiresAt {
		t.Fatalf("refresh did not extend expiry: before=%s after=%s", acquired.Reservation.ExpiresAt, refreshed.ExpiresAt)
	}
	refreshRefs, scoped, err := s.proofRefreshRefs(acquired.Reservation)
	if err != nil {
		t.Fatal(err)
	}
	if !scoped || len(refreshRefs) != 1 {
		t.Fatalf("refresh expiry evidence = refs:%v scoped:%t, want one reservation-scoped outcome", refreshRefs, scoped)
	}

	if _, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
	}, fixedNow.Add(33*time.Second)); err == nil {
		t.Fatal("successor acquired after original expiry even though live holder refreshed")
	}

	report := canonicalOwnedProofReport()
	report["base_ref"] = ""
	report["generated_at"] = fixedNow.Add(35 * time.Second).Format(time.RFC3339)
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteProofResource(first.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(36*time.Second))
	if err != nil {
		t.Fatalf("completion inside refreshed window: %v", err)
	}
	if completed.Result.Status != ProofResourceVerified || completed.VerificationRef == "" {
		t.Fatalf("refreshed reservation did not certify exact canonical report: %+v", completed)
	}
}

func TestProofResourceRepeatedHeartbeatsKeepWorkLifecycleBounded(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "bounded heartbeat proof work", proofActorAlpha, fixedNow)
	w, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: proofActorAlpha}, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	readsAfterRoot := int64(0)
	for i, offset := range []time.Duration{5, 10, 15} {
		if _, err := s.RefreshProofResource(
			w.ID,
			acquired.Reservation.ID,
			proofActorAlpha,
			30*time.Second,
			fixedNow.Add(offset*time.Second),
		); err != nil {
			t.Fatalf("refresh at +%s: %v", offset, err)
		}
		if i == 0 {
			readsAfterRoot = s.eventLogReadCount()
		}
	}
	if got := s.eventLogReadCount() - readsAfterRoot; got != 0 {
		t.Fatalf("heartbeats after the first governance root performed %d global lifecycle-log reads, want zero", got)
	}

	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(after.ActionIntentIDs), len(before.ActionIntentIDs)+1; got != want {
		t.Fatalf("three heartbeats grew work action refs to %d, want one stable refresh root (%d)", got, want)
	}
	if got, want := len(afterEvents)-len(beforeEvents), 1; got != want {
		t.Fatalf("three heartbeats appended %d lifecycle rows, want one stable refresh root", got)
	}

	entries, err := s.events.Scan(eventstore.ScanOptions{Prefixes: []string{"pfrh_"}, RegularOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			t.Fatal(err)
		}
		var refresh ProofResourceRefresh
		if err := json.Unmarshal(record.Bytes, &refresh); err != nil {
			t.Fatal(err)
		}
		if refresh.ReservationID == acquired.Reservation.ID {
			refreshes++
		}
	}
	if refreshes != 3 {
		t.Fatalf("reservation refresh evidence count = %d, want 3", refreshes)
	}
	latest, err := s.proofReservationWithLatestRefresh(acquired.Reservation)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ExpiresAt != fixedNow.Add(45*time.Second).UTC().Format(time.RFC3339) {
		t.Fatalf("latest refresh expiry = %s, want %s", latest.ExpiresAt, fixedNow.Add(45*time.Second).UTC().Format(time.RFC3339))
	}
}

func TestProofResourceSameSecondRefreshesKeepDistinctOutcomeEvidence(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "same-second heartbeat proof work", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	for _, refreshedAt := range []time.Time{
		fixedNow.Add(10*time.Second + 11*time.Millisecond),
		fixedNow.Add(10*time.Second + 771*time.Millisecond),
	} {
		if _, err := s.RefreshProofResource(
			w.ID,
			acquired.Reservation.ID,
			proofActorAlpha,
			30*time.Second,
			refreshedAt,
		); err != nil {
			t.Fatalf("same-second refresh at %s: %v", refreshedAt.Format(time.RFC3339Nano), err)
		}
	}

	refs, scoped, err := s.proofRefreshRefs(acquired.Reservation)
	if err != nil {
		t.Fatal(err)
	}
	if !scoped || len(refs) != 2 || refs[0] == refs[1] {
		t.Fatalf("same-second refresh refs = %v scoped=%t, want two distinct scoped outcomes", refs, scoped)
	}
	preparations := map[string]bool{}
	for _, ref := range refs {
		var refresh ProofResourceRefresh
		if err := s.readEventJSON(ref, &refresh); err != nil {
			t.Fatal(err)
		}
		if refresh.PreparationID == "" {
			t.Fatalf("same-second refresh %s omitted preparation_id", ref)
		}
		preparations[refresh.PreparationID] = true
	}
	if len(preparations) != 2 {
		t.Fatalf("same-second refresh preparations = %v, want two distinct governed preparations", preparations)
	}
	if _, err := s.proofReservationWithLatestRefresh(acquired.Reservation); err != nil {
		t.Fatalf("same-second refresh evidence did not validate: %v", err)
	}
}

func TestProofResourceScopedRefreshRequiresExactPreparation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "refresh preparation proof work", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshProofResource(
		w.ID,
		acquired.Reservation.ID,
		proofActorAlpha,
		30*time.Second,
		fixedNow.Add(10*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	refs, scoped, err := s.proofRefreshRefs(acquired.Reservation)
	if err != nil || !scoped || len(refs) != 1 {
		t.Fatalf("scoped refresh refs = %v scoped=%t err=%v", refs, scoped, err)
	}
	var refresh ProofResourceRefresh
	if err := s.readEventJSON(refs[0], &refresh); err != nil {
		t.Fatal(err)
	}
	if refresh.PreparationID == "" {
		t.Fatal("scoped refresh omitted preparation_id")
	}
	legacy := refresh
	legacy.ID = proofRefreshLegacyOutcomeID(legacy.ReservationID, legacy.RunAttemptID, legacy.ExpiresAt)
	if err := s.writeEventJSON(legacy.ID, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.proofReservationWithLatestRefresh(acquired.Reservation); err != nil {
		t.Fatalf("legacy expiry-only refresh identity was not accepted: %v", err)
	}
	refresh.RefreshedAt = fixedNow.Add(11 * time.Second).UTC().Format(time.RFC3339)
	refresh.ExpiresAt = fixedNow.Add(41 * time.Second).UTC().Format(time.RFC3339)
	refresh.CreatedAt = fixedNow.Add(11 * time.Second).UTC().Format(time.RFC3339Nano)
	refresh.PreparationID = "pfrp_missing_preparation"
	refresh.ID = proofRefreshLegacyOutcomeID(refresh.ReservationID, refresh.RunAttemptID, refresh.ExpiresAt)
	if err := s.writeEventJSON(refresh.ID, &refresh); err != nil {
		t.Fatal(err)
	}
	if _, err := s.proofReservationWithLatestRefresh(acquired.Reservation); err == nil || !strings.Contains(err.Error(), "preparation") {
		t.Fatalf("missing preparation was not rejected: %v", err)
	}
}

func TestProofResourceScopedRefreshRejectsArbitraryOutcomeIdentity(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "arbitrary refresh identity proof work", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshProofResource(
		w.ID,
		acquired.Reservation.ID,
		proofActorAlpha,
		30*time.Second,
		fixedNow.Add(10*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	refs, scoped, err := s.proofRefreshRefs(acquired.Reservation)
	if err != nil || !scoped || len(refs) != 1 {
		t.Fatalf("scoped refresh refs = %v scoped=%t err=%v", refs, scoped, err)
	}
	var refresh ProofResourceRefresh
	if err := s.readEventJSON(refs[0], &refresh); err != nil {
		t.Fatal(err)
	}
	refresh.ID = proofRefreshOutcomePrefix(acquired.Reservation.ID) + "arbitrary"
	if err := s.writeEventJSON(refresh.ID, &refresh); err != nil {
		t.Fatal(err)
	}
	if _, err := s.proofReservationWithLatestRefresh(acquired.Reservation); err == nil || !strings.Contains(err.Error(), "invalid reservation-scoped identity") {
		t.Fatalf("arbitrary refresh identity was not rejected: %v", err)
	}
}

func TestProofReservationWithLatestRefreshIgnoresHistoricalReservationLineage(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "historical refresh lineage", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	// A work item may contain refresh evidence from an older reservation.
	// Select the current lineage before applying its strict validation.
	historical := ProofResourceRefresh{
		SchemaVersion:    ProofResourceRefreshSchemaVersion,
		ID:               "pfrh_historical_reservation",
		Kind:             "work.proof_resource.refresh",
		CreatedAt:        fixedNow.Add(3 * time.Second).UTC().Format(time.RFC3339Nano),
		WorkID:           w.ID,
		Actor:            proofActorAlpha,
		ReservationID:    "pfr_historical_reservation",
		RunAttemptID:     "ratt_historical_reservation",
		Holder:           "proof:pfr_historical_reservation:ratt_historical_reservation",
		WindowOpenedAt:   fixedNow.Add(-time.Minute).UTC().Format(time.RFC3339),
		RefreshedAt:      fixedNow.Add(-30 * time.Second).UTC().Format(time.RFC3339),
		ExpiresAt:        fixedNow.Add(time.Minute).UTC().Format(time.RFC3339),
		TTLSeconds:       90,
		ActionIntentID:   acquired.Reservation.ActionIntentID,
		PolicyDecisionID: acquired.Reservation.PolicyDecisionID,
	}
	if err := s.writeEventJSON(historical.ID, &historical); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.appendEvent(Event{
		TS:       ts(fixedNow.Add(3 * time.Second)),
		WorkID:   w.ID,
		From:     current.State,
		To:       current.State,
		By:       proofActorAlpha,
		Note:     "historical proof resource refresh",
		Attached: []string{historical.ID},
	}); err != nil {
		t.Fatal(err)
	}

	refreshed, err := s.RefreshProofResource(
		w.ID,
		acquired.Reservation.ID,
		proofActorAlpha,
		30*time.Second,
		fixedNow.Add(20*time.Second),
	)
	if err != nil {
		t.Fatalf("refresh current reservation: %v", err)
	}
	evidenced, err := s.proofReservationWithLatestRefresh(acquired.Reservation)
	if err != nil {
		t.Fatalf("resolve latest current refresh: %v", err)
	}
	if evidenced.ExpiresAt != refreshed.ExpiresAt || evidenced.TTLSeconds != refreshed.TTLSeconds {
		t.Fatalf("latest current refresh = expiry %s ttl %d, want expiry %s ttl %d",
			evidenced.ExpiresAt, evidenced.TTLSeconds, refreshed.ExpiresAt, refreshed.TTLSeconds)
	}
}

func TestProofResourceReservationExceptionalStealIsExplicitAndAudited(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "steal source proof", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "steal successor proof", proofActorBeta, fixedNow.Add(time.Second))

	acquired, err := reserveProofResourceWithLiveBarrier(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
		Steal:     true,
	}, fixedNow.Add(3*time.Second)); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("reasonless steal = %v, want explicit refusal", err)
	}

	const reason = "predecessor process is gone; operator inspected its stale execution"
	blocked, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:       proofActorBeta,
		TTL:         5 * time.Minute,
		Workspace:   "/synthetic/repo-beta",
		Command:     "compound proof-check --out /synthetic/report-beta.json",
		Steal:       true,
		StealReason: reason,
	}, fixedNow.Add(4*time.Second))
	if err == nil || !strings.Contains(err.Error(), "hold") {
		t.Fatalf("default policy permitted exceptional steal: result=%+v err=%v", blocked, err)
	}
	var blockedAction envelope.ActionIntent
	if err := s.readEventJSON(blocked.Reservation.ActionIntentID, &blockedAction); err != nil {
		t.Fatal(err)
	}
	var blockedPolicy envelope.PolicyDecision
	if err := s.readEventJSON(blocked.Reservation.PolicyDecisionID, &blockedPolicy); err != nil {
		t.Fatal(err)
	}
	if blockedAction.Action != "steal_full_proof_resource" ||
		blockedAction.MutationClass != "security" ||
		blockedAction.Reversibility != "manual" ||
		blockedPolicy.Outcome != "hold" ||
		!strings.Contains(blockedAction.Preview, acquired.Reservation.Holder) ||
		!strings.Contains(blockedAction.Preview, reason) {
		t.Fatalf("steal did not take the stricter policy path: action=%+v decision=%+v", blockedAction, blockedPolicy)
	}
	if blocked.Reservation.RunAttemptID == "" {
		t.Fatal("policy-refused exceptional steal omitted ratt_* evidence")
	}
	assertProofRunAttempt(t, s, blocked.Reservation, runattempt.StatusAbandoned)
	assertLifecycleAttachment(t, s, second.ID, blocked.Reservation.RunAttemptID)

	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	policyYAML := []byte(`standing_grants:
  - id: operator_proof_resource_steal
    capability: proof.resource
    mutation_class: security
    reversibility: manual
    blast_radius: repo
    expires_at: "` + fixedNow.Add(365*24*time.Hour).Format(time.RFC3339) + `"
`)
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), policyYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	stolen, err := reserveProofWithTakeoverObserver(func() (ProofResourceReserveResult, error) {
		return reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
			Actor:       proofActorBeta,
			TTL:         5 * time.Minute,
			Workspace:   "/synthetic/repo-beta",
			Command:     "compound proof-check --out /synthetic/report-beta.json",
			Steal:       true,
			StealReason: reason,
		}, fixedNow.Add(5*time.Second))
	}, func() (bool, error) {
		status, statusErr := landlease.Status(s.home, fixedNow.Add(5*time.Second))
		if statusErr != nil {
			return false, statusErr
		}
		if status.Holder == acquired.Reservation.Holder {
			return false, nil
		}
		_, completeErr := s.CompleteProofResource(first.ID, ProofResourceCompleteInput{
			Actor:         proofActorAlpha,
			ReservationID: acquired.Reservation.ID,
			FailureReason: "predecessor observed exceptional lease loss and stopped",
		}, fixedNow.Add(5*time.Second))
		if completeErr == nil {
			return false, errors.New("stolen predecessor completion unexpectedly succeeded")
		}
		ra, readErr := s.runAttempts().Get(acquired.Reservation.RunAttemptID)
		if readErr != nil {
			return false, readErr
		}
		if !runattempt.IsTerminal(ra.Status) {
			return false, errors.New("stolen predecessor remained nonterminal")
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("explicitly granted audited steal: %v", err)
	}
	if stolen.Reservation.PriorHolder != acquired.Reservation.Holder ||
		stolen.Reservation.PriorRunAttemptID != acquired.Reservation.RunAttemptID ||
		stolen.Reservation.StealReason != reason ||
		stolen.Reservation.StolenAt == "" {
		t.Fatalf("steal audit = %+v", stolen.Reservation)
	}
	var stealAction envelope.ActionIntent
	if err := s.readEventJSON(stolen.Reservation.ActionIntentID, &stealAction); err != nil {
		t.Fatal(err)
	}
	if stealAction.Action != "steal_full_proof_resource" ||
		!strings.Contains(stealAction.Preview, acquired.Reservation.Holder) ||
		!strings.Contains(stealAction.Preview, reason) {
		t.Fatalf("steal ActionIntent did not expose exceptional transfer: %+v", stealAction)
	}
	var stealPolicy envelope.PolicyDecision
	if err := s.readEventJSON(stolen.Reservation.PolicyDecisionID, &stealPolicy); err != nil {
		t.Fatal(err)
	}
	if stealPolicy.ActionIntentID != stealAction.ID {
		t.Fatalf("steal policy %s does not govern steal action %s", stealPolicy.ID, stealAction.ID)
	}
	if stealPolicy.StandingGrantID != "operator_proof_resource_steal" || !policy.Allowed(stealPolicy) {
		t.Fatalf("steal policy did not cite exact operator grant: %+v", stealPolicy)
	}
	priorAttempt, err := s.runAttempts().Get(acquired.Reservation.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !runattempt.IsTerminal(priorAttempt.Status) {
		t.Fatalf("exceptionally stolen predecessor remained %q", priorAttempt.Status)
	}
	assertLifecycleAttachment(t, s, second.ID, stolen.Reservation.ID)
}

func TestProofResourceFailedStealRetainsTerminationBarrierAcrossExpiry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := claimedProofWork(t, s, "active proof ignores takeover", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "failed takeover barrier", proofActorBeta, fixedNow.Add(time.Second))
	third := claimedProofWork(t, s, "contender after barrier expiry", "codex:proof-gamma", fixedNow.Add(2*time.Second))
	acquired, err := reserveProofResourceWithLiveBarrier(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), []byte(`standing_grants:
  - id: operator_proof_resource_steal
    capability: proof.resource
    mutation_class: security
    reversibility: manual
    blast_radius: repo
    expires_at: "`+fixedNow.Add(365*24*time.Hour).Format(time.RFC3339)+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	barrier, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:       proofActorBeta,
		TTL:         time.Second,
		Workspace:   "/synthetic/repo-beta",
		Command:     "compound proof-check --out /synthetic/report-beta.json",
		Steal:       true,
		StealReason: "operator requested takeover but predecessor did not stop",
	}, fixedNow.Add(4*time.Second))
	if err == nil {
		t.Fatalf("failed takeover = result=%+v err=%v, want execution-barrier refusal", barrier, err)
	}
	assertProofRunAttempt(t, s, barrier.Reservation, runattempt.StatusAbandoned)
	status, err := landlease.Status(s.home, fixedNow.Add(4500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Held || status.Holder != acquired.Reservation.Holder {
		t.Fatalf("failed takeover opened scarce resource instead of retaining barrier: %+v", status)
	}

	nextBarrier, err := reserveProofResource(t, s, third.ID, ProofResourceReserveInput{
		Actor:     "codex:proof-gamma",
		TTL:       time.Second,
		Workspace: "/synthetic/repo-gamma",
		Command:   "compound proof-check --out /synthetic/report-gamma.json",
	}, fixedNow.Add(6*time.Minute))
	if err == nil {
		t.Fatalf("post-expiry contender = result=%+v err=%v, want original termination barrier", nextBarrier, err)
	}
	assertProofRunAttempt(t, s, nextBarrier.Reservation, runattempt.StatusAbandoned)
	original, err := s.runAttempts().Get(acquired.Reservation.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if original.Status != runattempt.StatusRunning {
		t.Fatalf("barrier recovery rewrote unconfirmed original as %q", original.Status)
	}
	status, err = landlease.Status(s.home, fixedNow.Add(6*time.Minute+500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if status.Holder != acquired.Reservation.Holder {
		t.Fatalf("post-expiry refusal reopened scarce resource: %+v", status)
	}
}

func TestProofResourceAcquisitionFailureAfterMintIsTerminalAndReachable(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "non-contention acquisition failure", proofActorAlpha, fixedNow)
	before, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalAcquire := proofResourceAcquireWithExecutionBarrierPrepared
	t.Cleanup(func() { proofResourceAcquireWithExecutionBarrierPrepared = originalAcquire })
	proofResourceAcquireWithExecutionBarrierPrepared = func(home, holder string, ttl time.Duration, now time.Time, prepare func(landlease.AcquireOutcome) error) (landlease.AcquireOutcome, *landlease.ExecutionBarrier, error) {
		if err := os.MkdirAll(filepath.Join(home, "state"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "state", "land-lease.json"), []byte("{malformed"), 0644); err != nil {
			t.Fatal(err)
		}
		return originalAcquire(home, holder, ttl, now, prepare)
	}
	failed, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err == nil || !strings.Contains(err.Error(), "parse land-lease state") {
		t.Fatalf("non-contention acquisition error = %v, want malformed-state refusal", err)
	}
	if failed.Reservation.Status != ProofResourceFailed ||
		failed.Reservation.RunAttemptID == "" {
		t.Fatalf("acquisition failure omitted governed reservation evidence: %+v", failed.Reservation)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RunAttemptIDs) != len(before.RunAttemptIDs)+1 ||
		after.RunAttemptIDs[len(after.RunAttemptIDs)-1] != failed.Reservation.RunAttemptID {
		t.Fatalf("failed ratt_* not attached to work: before=%v after=%v reservation=%+v", before.RunAttemptIDs, after.RunAttemptIDs, failed.Reservation)
	}
	assertProofRunAttempt(t, s, failed.Reservation, runattempt.StatusFailed)
	assertLifecycleAttachment(t, s, w.ID, failed.Reservation.RunAttemptID)
	assertLifecycleAttachment(t, s, w.ID, failed.Reservation.ID)
}

func TestProofResourceLeasePublishFailureCannotLeaveAcquiredReservation(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "lease publish failure", proofActorAlpha, fixedNow)
	originalAcquire := proofResourceAcquireWithExecutionBarrierPrepared
	t.Cleanup(func() { proofResourceAcquireWithExecutionBarrierPrepared = originalAcquire })
	publishErr := errors.New("synthetic lease rename failure after prepared callback")
	proofResourceAcquireWithExecutionBarrierPrepared = func(
		homePath, holder string,
		ttl time.Duration,
		now time.Time,
		prepare func(landlease.AcquireOutcome) error,
	) (landlease.AcquireOutcome, *landlease.ExecutionBarrier, error) {
		outcome := landlease.AcquireOutcome{
			Lease: landlease.Lease{
				SchemaVersion: landlease.SchemaVersion,
				Holder:        holder,
				AcquiredAt:    now.UTC().Format(time.RFC3339),
				ExpiresAt:     now.Add(ttl).UTC().Format(time.RFC3339),
				TTLSeconds:    int64(ttl / time.Second),
			},
		}
		if err := prepare(outcome); err != nil {
			return landlease.AcquireOutcome{}, nil, err
		}
		return landlease.AcquireOutcome{}, nil, publishErr
	}
	failed, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if !errors.Is(err, publishErr) {
		t.Fatalf("reserve error = %v, want %v", err, publishErr)
	}
	var stored ProofResourceReservation
	if err := s.readEventJSON(failed.Reservation.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status != ProofResourceFailed || stored.FailureReason == "" {
		t.Fatalf("lease publish failure persisted false acquired reservation: %+v", stored)
	}
	assertProofRunAttempt(t, s, stored, runattempt.StatusFailed)
	status, err := landlease.Status(s.home, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Held {
		t.Fatalf("failed lease publication exposed holder: %+v", status)
	}
}

func TestProofResourceScarceMutationCrashIsGovernedAndRecoverable(t *testing.T) {
	t.Run("reserve mutation", func(t *testing.T) {
		s := newStore(t)
		first := claimedProofWork(t, s, "reserve crash holder", proofActorAlpha, fixedNow)
		second := claimedProofWork(t, s, "reserve crash recovery", proofActorBeta, fixedNow.Add(time.Second))
		before, err := s.Get(first.ID)
		if err != nil {
			t.Fatal(err)
		}
		originalHook := proofResourceAfterScarceMutation
		t.Cleanup(func() { proofResourceAfterScarceMutation = originalHook })
		proofResourceAfterScarceMutation = func(phase string) {
			if phase == "reserve" {
				panic("synthetic crash after reserve mutation")
			}
		}
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("reserve mutation did not reach crash boundary")
				}
			}()
			_, _ = reserveProofResource(t, s, first.ID, ProofResourceReserveInput{
				Actor:     proofActorAlpha,
				TTL:       5 * time.Second,
				Workspace: "/synthetic/repo-alpha",
				Command:   "compound proof-check --out /synthetic/report-alpha.json",
			}, fixedNow.Add(2*time.Second))
		}()
		proofResourceAfterScarceMutation = originalHook

		status, err := landlease.Status(s.home, fixedNow.Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !status.Held {
			t.Fatal("synthetic reserve crash did not leave the scarce mutation to recover")
		}
		_, runID, workID, ok := parseProofResourceHolder(status.Holder)
		if !ok || workID != first.ID || runID == "" {
			t.Fatalf("crashed holder is not auditable: %+v", status)
		}
		after, err := s.Get(first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !contains(after.RunAttemptIDs, runID) ||
			len(after.ActionIntentIDs) != len(before.ActionIntentIDs)+1 ||
			after.PolicyDecisionID == before.PolicyDecisionID {
			t.Fatalf("reserve mutation outran governed Work reachability: before=%+v after=%+v holder=%s", before, after, status.Holder)
		}
		var action envelope.ActionIntent
		if err := s.readEventJSON(after.ActionIntentIDs[len(after.ActionIntentIDs)-1], &action); err != nil {
			t.Fatal(err)
		}
		var decision envelope.PolicyDecision
		if err := s.readEventJSON(after.PolicyDecisionID, &decision); err != nil {
			t.Fatal(err)
		}
		if action.Action != "reserve_full_proof_resource" ||
			decision.ActionIntentID != action.ID ||
			!policy.Allowed(decision) {
			t.Fatalf("crashed reserve governance = action=%+v decision=%+v", action, decision)
		}

		writeProofResourceStealGrant(t, s.home)
		recovered, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
			Actor:                  proofActorBeta,
			TTL:                    time.Minute,
			Workspace:              "/synthetic/repo-beta",
			Command:                "compound proof-check --out /synthetic/report-beta.json --steal",
			Steal:                  true,
			StealReason:            "crashed predecessor process was confirmed terminated",
			TerminatedRunAttemptID: runID,
		}, fixedNow.Add(8*time.Second))
		if err != nil {
			t.Fatalf("recover expired crashed reserve with governed termination evidence: %v", err)
		}
		if recovered.Reservation.PriorRunAttemptID != runID ||
			recovered.Reservation.TerminatedRunAttemptID != runID ||
			recovered.Reservation.TerminationEvidenceRef == "" {
			t.Fatalf("recovered crashed RunAttempt evidence = %+v, want trusted evidence for %q", recovered.Reservation, runID)
		}
		recoveredRun, err := s.runAttempts().Get(runID)
		if err != nil {
			t.Fatal(err)
		}
		if recoveredRun.Status != runattempt.StatusAbandoned {
			t.Fatalf("recovered crashed RunAttempt status = %q, want abandoned", recoveredRun.Status)
		}
	})

	t.Run("refresh mutation", func(t *testing.T) {
		s := newStore(t)
		w := claimedProofWork(t, s, "refresh crash holder", proofActorAlpha, fixedNow)
		acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
			Actor:     proofActorAlpha,
			TTL:       10 * time.Second,
			Workspace: "/synthetic/repo-alpha",
			Command:   "compound proof-check --out /synthetic/report-alpha.json",
		}, fixedNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		before, err := s.Get(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		originalHook := proofResourceAfterScarceMutation
		t.Cleanup(func() { proofResourceAfterScarceMutation = originalHook })
		proofResourceAfterScarceMutation = func(phase string) {
			if phase == "refresh" {
				panic("synthetic crash after refresh mutation")
			}
		}
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("refresh mutation did not reach crash boundary")
				}
			}()
			_, _ = s.RefreshProofResource(w.ID, acquired.Reservation.ID, proofActorAlpha, 30*time.Second, fixedNow.Add(5*time.Second))
		}()
		proofResourceAfterScarceMutation = originalHook

		status, err := landlease.Status(s.home, fixedNow.Add(13*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !status.Held {
			t.Fatal("crashed refresh did not retain its governed extended holder")
		}
		after, err := s.Get(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.ActionIntentIDs) != len(before.ActionIntentIDs)+1 ||
			after.PolicyDecisionID == before.PolicyDecisionID {
			t.Fatalf("refresh mutation outran governed Work reachability: before=%+v after=%+v", before, after)
		}
		var action envelope.ActionIntent
		if err := s.readEventJSON(after.ActionIntentIDs[len(after.ActionIntentIDs)-1], &action); err != nil {
			t.Fatal(err)
		}
		var decision envelope.PolicyDecision
		if err := s.readEventJSON(after.PolicyDecisionID, &decision); err != nil {
			t.Fatal(err)
		}
		if action.Action != "refresh_full_proof_resource" ||
			decision.ActionIntentID != action.ID ||
			!policy.Allowed(decision) {
			t.Fatalf("crashed refresh governance = action=%+v decision=%+v", action, decision)
		}
		evidenced, err := s.proofReservationWithLatestRefresh(acquired.Reservation)
		if err != nil {
			t.Fatal(err)
		}
		if evidenced.ExpiresAt != acquired.Reservation.ExpiresAt {
			t.Fatalf("unevidenced crashed refresh changed certifying window: got=%s want=%s", evidenced.ExpiresAt, acquired.Reservation.ExpiresAt)
		}

		if _, err := s.RefreshProofResource(w.ID, acquired.Reservation.ID, proofActorAlpha, 30*time.Second, fixedNow.Add(6*time.Second)); err != nil {
			t.Fatalf("retry refresh after crash: %v", err)
		}
		report := canonicalOwnedProofReport()
		report["base_ref"] = ""
		report["generated_at"] = fixedNow.Add(20 * time.Second).Format(time.RFC3339)
		body, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			t.Fatal(err)
		}
		reportRef, err := artifacts.Put(body)
		if err != nil {
			t.Fatal(err)
		}
		completed, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
			Actor:             proofActorAlpha,
			ReservationID:     acquired.Reservation.ID,
			ReportArtifactRef: reportRef,
			ProducerExitKnown: true,
			ProducerExitCode:  0,
		}, fixedNow.Add(21*time.Second))
		if err != nil {
			t.Fatalf("complete after recovered refresh: %v", err)
		}
		if completed.VerificationRef == "" {
			t.Fatalf("recovered refresh did not certify: %+v", completed)
		}
	})
}

func TestProofResourceUnknownProducerExitNeverBecomesVerification(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "ambiguous producer proof", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: false,
	}, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if completed.VerificationRef != "" || completed.Result.Status != ProofResourceInvalid {
		t.Fatalf("ambiguous producer result counted as proof: %+v", completed)
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusFailed)
}

func TestProofResourceCompletionRejectsInvalidOwnedReportAndExpiredLease(t *testing.T) {
	t.Parallel()
	t.Run("invalid JSON bytes", func(t *testing.T) {
		s := newStore(t)
		w := claimedProofWork(t, s, "invalid report proof", proofActorAlpha, fixedNow)
		acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
			Actor:     proofActorAlpha,
			TTL:       time.Minute,
			Workspace: "/synthetic/repo-alpha",
			Command:   "compound proof-check --out /synthetic/report-alpha.json",
		}, fixedNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			t.Fatal(err)
		}
		reportRef, err := artifacts.Put([]byte("battery: ok"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
			Actor:             proofActorAlpha,
			ReservationID:     acquired.Reservation.ID,
			ReportArtifactRef: reportRef,
			ProducerExitKnown: true,
			ProducerExitCode:  0,
		}, fixedNow.Add(3*time.Second)); err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("invalid report completion = %v, want decode refusal", err)
		}
		status, err := landlease.Status(s.home, fixedNow.Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !status.Held || status.Holder != acquired.Reservation.Holder {
			t.Fatalf("invalid report released reservation: %+v", status)
		}
		assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusFailed)
	})

	t.Run("fabricated report shape", func(t *testing.T) {
		s := newStore(t)
		w := claimedProofWork(t, s, "fabricated report proof", proofActorAlpha, fixedNow)
		acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
			Actor:     proofActorAlpha,
			TTL:       time.Minute,
			Workspace: "/synthetic/repo-alpha",
			Command:   "compound proof-check --out /synthetic/report-alpha.json",
		}, fixedNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			t.Fatal(err)
		}
		reportRef, err := artifacts.Put([]byte(`{"schema_version":"compound.proof_check_report.v0.2","status":"ok","generated_at":"2026-07-24T12:00:00Z","repo":"/synthetic/repo","dry_run":false,"steps":[{"status":"ok"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
			Actor:             proofActorAlpha,
			ReservationID:     acquired.Reservation.ID,
			ReportArtifactRef: reportRef,
			ProducerExitKnown: true,
			ProducerExitCode:  0,
		}, fixedNow.Add(3*time.Second)); err == nil || !strings.Contains(err.Error(), "name and proof_ref") {
			t.Fatalf("fabricated report completion = %v, want structural refusal", err)
		}
		assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusFailed)
	})

	t.Run("lease expired without successor", func(t *testing.T) {
		s := newStore(t)
		w := claimedProofWork(t, s, "expired completion proof", proofActorAlpha, fixedNow)
		acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
			Actor:     proofActorAlpha,
			TTL:       30 * time.Second,
			Workspace: "/synthetic/repo-alpha",
			Command:   "compound proof-check --out /synthetic/report-alpha.json",
		}, fixedNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		artifacts, err := home.NewArtifactStore(s.home)
		if err != nil {
			t.Fatal(err)
		}
		reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
			Actor:             proofActorAlpha,
			ReservationID:     acquired.Reservation.ID,
			ReportArtifactRef: reportRef,
			ProducerExitKnown: true,
			ProducerExitCode:  0,
		}, fixedNow.Add(33*time.Second)); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expired completion = %v, want expiry refusal", err)
		}
		after, err := s.Get(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.VerificationIDs) != 0 {
			t.Fatalf("expired result entered verification_ids: %v", after.VerificationIDs)
		}
	})
}

func TestProofResourceCompletionRejectsCanonicalProfileDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name: "well shaped one fake gate",
			mutate: func(report map[string]any) {
				report["steps"] = []map[string]any{{
					"name":      "fake-green-gate",
					"command":   []string{"true"},
					"proof_ref": "fake-green-proof",
					"status":    "ok",
				}}
				report["proof_refs"] = []string{"fake-green-proof"}
			},
			wantErr: "inventory",
		},
		{
			name: "gate command mismatch",
			mutate: func(report map[string]any) {
				steps := report["steps"].([]map[string]any)
				steps[0]["command"] = []string{"go", "test", "./..."}
			},
			wantErr: "command",
		},
		{
			name: "proof ref inventory mismatch",
			mutate: func(report map[string]any) {
				report["proof_refs"] = []string{"synthetic-proof-a"}
			},
			wantErr: "proof_ref inventory mismatch",
		},
		{
			name: "gate proof ref mismatch",
			mutate: func(report map[string]any) {
				steps := report["steps"].([]map[string]any)
				steps[1]["proof_ref"] = "synthetic-proof-b-tampered"
				report["proof_refs"] = []string{"synthetic-proof-a", "synthetic-proof-b-tampered"}
			},
			wantErr: "proof_ref",
		},
		{
			name: "base ref mismatch",
			mutate: func(report map[string]any) {
				report["base_ref"] = "refs/heads/unexpected"
			},
			wantErr: "base_ref",
		},
		{
			name: "repo mismatch",
			mutate: func(report map[string]any) {
				report["repo"] = "/synthetic/other-repo"
			},
			wantErr: "repo",
		},
		{
			name: "noncanonical profile kind",
			mutate: func(report map[string]any) {
				report["profile_kind"] = "development_fast"
			},
			wantErr: "profile_kind",
		},
		{
			name: "canonical gate skipped",
			mutate: func(report map[string]any) {
				steps := report["steps"].([]map[string]any)
				steps[1]["status"] = "skipped"
				report["proof_refs"] = []string{"synthetic-proof-a"}
			},
			wantErr: "status",
		},
		{
			name: "generated before reservation",
			mutate: func(report map[string]any) {
				report["generated_at"] = fixedNow.Format(time.RFC3339)
			},
			wantErr: "generated_at",
		},
		{
			name: "generated after reservation",
			mutate: func(report map[string]any) {
				report["generated_at"] = fixedNow.Add(10 * time.Minute).Format(time.RFC3339)
			},
			wantErr: "generated_at",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w := claimedProofWork(t, s, "canonical profile "+tc.name, proofActorAlpha, fixedNow)
			acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
				Actor:     proofActorAlpha,
				TTL:       time.Minute,
				Workspace: "/synthetic/repo-alpha",
				Command:   "compound proof-check --base refs/heads/main --out /synthetic/report-alpha.json",
			}, fixedNow.Add(2*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			report := canonicalOwnedProofReport()
			tc.mutate(report)
			body, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			artifacts, err := home.NewArtifactStore(s.home)
			if err != nil {
				t.Fatal(err)
			}
			reportRef, err := artifacts.Put(body)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
				Actor:             proofActorAlpha,
				ReservationID:     acquired.Reservation.ID,
				ReportArtifactRef: reportRef,
				ProducerExitKnown: true,
				ProducerExitCode:  0,
			}, fixedNow.Add(3*time.Second)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("profile drift completion error = %v, want %q refusal", err, tc.wantErr)
			}
			after, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.VerificationIDs) != 0 {
				t.Fatalf("profile drift entered verification_ids: %v", after.VerificationIDs)
			}
		})
	}
}

func TestProofResourceProfileKindIsSealedAndNoncanonicalCannotVerify(t *testing.T) {
	t.Parallel()
	gates := []ProofResourceGateProfile{
		{
			Name:     "synthetic-gate-a",
			Command:  []string{"go", "test", "./internal/synthetic-a"},
			ProofRef: "synthetic-proof-a",
		},
		{
			Name:     "synthetic-gate-b",
			Command:  []string{"go", "test", "./internal/synthetic-b"},
			ProofRef: "synthetic-proof-b",
		},
	}
	canonical, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, "/synthetic/repo-alpha", "", gates)
	if err != nil {
		t.Fatal(err)
	}
	development, err := NewProofResourceProfile("development_fast", "/synthetic/repo-alpha", "", gates)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Digest == development.Digest {
		t.Fatalf("profile kind is absent from digest: canonical=%s development=%s", canonical.Digest, development.Digest)
	}

	s := newStore(t)
	w := claimedProofWork(t, s, "development profile cannot certify", proofActorAlpha, fixedNow)
	acquired, err := s.ReserveProofResource(w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
		Profile:   development,
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(3*time.Second)); err == nil || !strings.Contains(err.Error(), "profile_kind") {
		t.Fatalf("noncanonical reservation completion error = %v, want profile_kind refusal", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.VerificationIDs) != 0 {
		t.Fatalf("noncanonical reservation entered verification_ids: %v", after.VerificationIDs)
	}
}

func TestProofResourceCompletionRequiresActiveSameWorkRunAttempt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "terminal run proof", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.runAttempts().CompleteWithReason(
		acquired.Reservation.RunAttemptID,
		runattempt.StatusAbandoned,
		"synthetic producer disappeared",
		fixedNow.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(4*time.Second)); err == nil || !strings.Contains(err.Error(), "active RunAttempt") {
		t.Fatalf("terminal RunAttempt completion = %v, want active-attempt refusal", err)
	}
	status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Held {
		t.Fatal("terminal RunAttempt refusal released scarce resource")
	}
}

func TestProofResourceCompletionPersistenceFailureKeepsSuccessorOut(t *testing.T) {
	s := newStore(t)
	first := claimedProofWork(t, s, "persisting proof work", proofActorAlpha, fixedNow)
	second := claimedProofWork(t, s, "waiting proof work", proofActorBeta, fixedNow.Add(time.Second))
	acquired, err := reserveProofResource(t, s, first.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}

	originalWrite := writeWorkEvent
	writeWorkEvent = func(_ *os.File, _ []byte) (int, error) {
		return 0, errors.New("injected proof event persistence failure")
	}
	_, completeErr := s.CompleteProofResource(first.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(3*time.Second))
	writeWorkEvent = originalWrite
	if completeErr == nil {
		t.Fatal("completion unexpectedly succeeded through injected persistence failure")
	}
	status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Held || status.Holder != acquired.Reservation.Holder {
		t.Fatalf("completion failure released scarce resource: %+v", status)
	}
	afterFailure, err := s.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFailure.VerificationIDs) != 0 {
		t.Fatalf("failed completion exposed verification_ids: %v", afterFailure.VerificationIDs)
	}
	assertProofRunAttempt(t, s, acquired.Reservation, runattempt.StatusFailed)
	if _, err := reserveProofResource(t, s, second.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
	}, fixedNow.Add(4*time.Second)); err == nil {
		t.Fatal("successor began after completion persistence failure")
	}
	retried, err := s.CompleteProofResource(first.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("retry exact completion: %v", err)
	}
	if retried.VerificationRef == "" || retried.Result.Status != ProofResourceVerified {
		t.Fatalf("retried completion = %+v, want governed verification", retried)
	}
	if retried.Result.RunAttemptID == acquired.Reservation.RunAttemptID {
		t.Fatalf("retry reused failed RunAttempt %s", acquired.Reservation.RunAttemptID)
	}
	retryAttempt, err := s.runAttempts().Get(retried.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if retryAttempt.PredecessorID != acquired.Reservation.RunAttemptID {
		t.Fatalf("retry predecessor = %q, want %q", retryAttempt.PredecessorID, acquired.Reservation.RunAttemptID)
	}
}

func TestProofResourceFailedCompletionRetryMintsPredecessorAfterPrecommitFault(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "retry failed proof after precommit fault", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("failed"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  1,
	}

	originalWriteEvent := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("injected failed-result persistence fault")
		}
		return originalWriteEvent(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWriteEvent
	if firstErr == nil {
		t.Fatal("failed completion unexpectedly crossed pre-authoritative-commit fault")
	}
	predecessor, err := s.runAttempts().Get(acquired.Reservation.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if predecessor.Status != runattempt.StatusFailed {
		t.Fatalf("predecessor status = %q, want failed terminal error record", predecessor.Status)
	}

	retried, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("exact failed completion retry remained wedged: %v", err)
	}
	if retried.Result.Status != ProofResourceFailed ||
		retried.Result.RunAttemptStatus != runattempt.StatusFailed ||
		retried.VerificationRef != "" ||
		!retried.ResourceReleased {
		t.Fatalf("retried failed completion = %+v, want terminal non-certifying release", retried)
	}
	if retried.Result.RunAttemptID == predecessor.ID {
		t.Fatalf("failed completion retry reused mismatched terminal predecessor %s", predecessor.ID)
	}
	retryAttempt, err := s.runAttempts().Get(retried.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if retryAttempt.PredecessorID != predecessor.ID {
		t.Fatalf("retry predecessor = %q, want %q", retryAttempt.PredecessorID, predecessor.ID)
	}
}

func TestProofResourceCompletionRetryNormalizesLegacyPendingResultProjection(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "normalize legacy pending proof result", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}

	originalWriteEvent := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "completion" {
			return errors.New("injected post-terminal completion persistence fault")
		}
		return originalWriteEvent(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWriteEvent
	if firstErr == nil {
		t.Fatal("completion unexpectedly crossed post-terminal persistence fault")
	}

	pending, present, pendingPath, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("completion fault did not retain pending journal")
	}
	pending.GovernanceSeal = ""
	pending.Result.RunAttemptStatus = runattempt.StatusRunning
	pending.Result.VerificationRef = ""
	if err := s.writeProofResourceCompletionPending(pendingPath, pending); err != nil {
		t.Fatal(err)
	}

	recovered, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("legacy pending completion retry remained wedged: %v", err)
	}
	if !recovered.ResourceReleased || recovered.VerificationRef == "" {
		t.Fatalf("legacy pending completion recovery = %+v", recovered)
	}
	var persistedResult ProofResourceResult
	if err := s.readEventJSON(recovered.Result.ID, &persistedResult); err != nil {
		t.Fatal(err)
	}
	if persistedResult.RunAttemptStatus != runattempt.StatusSucceeded ||
		persistedResult.VerificationRef != recovered.VerificationRef {
		t.Fatalf(
			"persisted recovered result status/ref = %q/%q, want %q/%q",
			persistedResult.RunAttemptStatus,
			persistedResult.VerificationRef,
			runattempt.StatusSucceeded,
			recovered.VerificationRef,
		)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy pending journal not cleared after recovery: %v", err)
	}
}

func TestProofResourceCompletionRetryRejectsForeignPendingBeforeDescendantAdoption(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "reject foreign pending before retry adoption", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}
	originalWriteEvent := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("injected initial result persistence fault")
		}
		return originalWriteEvent(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWriteEvent
	if firstErr == nil {
		t.Fatal("initial completion unexpectedly crossed result persistence fault")
	}

	pending, present, pendingPath, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("initial fault did not retain pending completion")
	}
	currentWork, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	descendant, err := s.prepareProofRunAttempt(currentWork, pending.Action.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		Workspace: acquired.Reservation.Workspace,
		Command:   acquired.Reservation.Command,
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if descendant.PredecessorID != pending.Result.RunAttemptID {
		t.Fatalf("retry descendant predecessor = %q, want %q", descendant.PredecessorID, pending.Result.RunAttemptID)
	}
	pending.Action.IDs.WorkID = "work_foreign"
	if err := s.writeProofResourceCompletionPending(pendingPath, pending); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(5*time.Second)); err == nil {
		t.Fatal("foreign pending completion unexpectedly reached descendant adoption")
	}
	after, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("foreign pending completion mutated its journal before rejection")
	}
	afterDescendant, err := s.runAttempts().Get(descendant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterDescendant.Status != runattempt.StatusPending {
		t.Fatalf("foreign pending completion adopted/terminalized descendant as %q", afterDescendant.Status)
	}
}

func TestProofResourceCompletionRetryRejectsNonLegacyResultProjectionByteIdentical(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "reject nonlegacy pending result projection", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}
	originalWriteEvent := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "completion" {
			return errors.New("injected post-terminal completion persistence fault")
		}
		return originalWriteEvent(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWriteEvent
	if firstErr == nil {
		t.Fatal("completion unexpectedly crossed post-terminal persistence fault")
	}

	pending, present, pendingPath, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("completion fault did not retain pending journal")
	}
	pending.Result.RunAttemptStatus = runattempt.StatusFailed
	pending.Result.VerificationRef = "proof-check:foreign"
	if err := s.writeProofResourceCompletionPending(pendingPath, pending); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(4*time.Second)); err == nil {
		t.Fatal("nonlegacy pending result mismatch unexpectedly normalized")
	}
	after, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("nonlegacy pending result mismatch mutated its journal before rejection")
	}
}

func TestProofResourceCompletionFaultsAreAtomicAndRetryable(t *testing.T) {
	tests := []struct {
		name          string
		install       func() func()
		wantRunStatus string
	}{
		{
			name: "result persistence",
			install: func() func() {
				original := proofResourceWriteEventRecord
				proofResourceWriteEventRecord = func(s *Store, stage, id string, value any) error {
					if stage == "result" {
						return errors.New("injected result persistence failure")
					}
					return original(s, stage, id, value)
				}
				return func() { proofResourceWriteEventRecord = original }
			},
			wantRunStatus: runattempt.StatusFailed,
		},
		{
			name: "work persistence",
			install: func() func() {
				original := proofResourceWriteWorkRecord
				proofResourceWriteWorkRecord = func(s *Store, stage string, w *envelope.WorkEnvelope) error {
					if stage == "work" {
						return errors.New("injected work persistence failure")
					}
					return original(s, stage, w)
				}
				return func() { proofResourceWriteWorkRecord = original }
			},
			wantRunStatus: runattempt.StatusFailed,
		},
		{
			name: "lifecycle persistence",
			install: func() func() {
				original := proofResourceAppendLifecycleRecord
				proofResourceAppendLifecycleRecord = func(_ *Store, _ Event) error {
					return errors.New("injected lifecycle persistence failure")
				}
				return func() { proofResourceAppendLifecycleRecord = original }
			},
			wantRunStatus: runattempt.StatusFailed,
		},
		{
			name: "RunAttempt proof persistence",
			install: func() func() {
				original := proofResourceSetRunAttemptProofRefs
				proofResourceSetRunAttemptProofRefs = func(_ *Store, _ string, _ []string) error {
					return errors.New("injected RunAttempt proof persistence failure")
				}
				return func() { proofResourceSetRunAttemptProofRefs = original }
			},
			wantRunStatus: runattempt.StatusFailed,
		},
		{
			name: "RunAttempt terminal persistence",
			install: func() func() {
				original := proofResourceTerminalizeRunAttempt
				proofResourceTerminalizeRunAttempt = func(_ *Store, _, _, _ string, _ time.Time) (runattempt.RunAttempt, error) {
					return runattempt.RunAttempt{}, errors.New("injected RunAttempt terminal persistence failure")
				}
				return func() { proofResourceTerminalizeRunAttempt = original }
			},
			wantRunStatus: runattempt.StatusFailed,
		},
		{
			name: "final work verification persistence",
			install: func() func() {
				original := proofResourceWriteWorkRecord
				proofResourceWriteWorkRecord = func(s *Store, stage string, w *envelope.WorkEnvelope) error {
					if stage == "work_final" {
						return errors.New("injected final work persistence failure")
					}
					return original(s, stage, w)
				}
				return func() { proofResourceWriteWorkRecord = original }
			},
			wantRunStatus: runattempt.StatusSucceeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w := claimedProofWork(t, s, "atomic completion "+tc.name, proofActorAlpha, fixedNow)
			acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
				Actor:     proofActorAlpha,
				TTL:       time.Minute,
				Workspace: "/synthetic/repo-alpha",
				Command:   "compound proof-check --out /synthetic/report-alpha.json",
			}, fixedNow.Add(2*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			artifacts, err := home.NewArtifactStore(s.home)
			if err != nil {
				t.Fatal(err)
			}
			reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
			if err != nil {
				t.Fatal(err)
			}
			input := ProofResourceCompleteInput{
				Actor:             proofActorAlpha,
				ReservationID:     acquired.Reservation.ID,
				ReportArtifactRef: reportRef,
				ProducerExitKnown: true,
				ProducerExitCode:  0,
			}
			restore := tc.install()
			_, completeErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
			restore()
			if completeErr == nil {
				t.Fatal("completion unexpectedly crossed injected persistence fault")
			}
			status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if !status.Held || status.Holder != acquired.Reservation.Holder {
				t.Fatalf("persistence fault released exclusive holder: %+v", status)
			}
			afterFailure, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(afterFailure.VerificationIDs) != 0 {
				t.Fatalf("persistence fault exposed verification_ids: %v", afterFailure.VerificationIDs)
			}
			assertProofRunAttempt(t, s, acquired.Reservation, tc.wantRunStatus)

			retried, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(5*time.Second))
			if err != nil {
				t.Fatalf("retry exact transaction: %v", err)
			}
			if retried.VerificationRef == "" || retried.Result.RunAttemptStatus != runattempt.StatusSucceeded {
				t.Fatalf("retried completion = %+v, want committed verification", retried)
			}
			if tc.wantRunStatus == runattempt.StatusFailed {
				if retried.Result.RunAttemptID == acquired.Reservation.RunAttemptID {
					t.Fatalf("retry reused terminal failed RunAttempt %s", acquired.Reservation.RunAttemptID)
				}
				retryAttempt, err := s.runAttempts().Get(retried.Result.RunAttemptID)
				if err != nil {
					t.Fatal(err)
				}
				if retryAttempt.PredecessorID != acquired.Reservation.RunAttemptID {
					t.Fatalf("retry predecessor = %q, want %q", retryAttempt.PredecessorID, acquired.Reservation.RunAttemptID)
				}
			}
			status, err = landlease.Status(s.home, fixedNow.Add(5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if status.Held {
				t.Fatalf("successful retry retained holder: %+v", status)
			}
		})
	}
}

func TestProofResourceCompletionRetryReachabilityRecoversAppendFault(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "retry reachability recovery", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}

	originalWriteEvent := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("injected initial completion failure")
		}
		return originalWriteEvent(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWriteEvent
	if firstErr == nil {
		t.Fatal("initial completion unexpectedly crossed result persistence fault")
	}

	originalAppend := proofResourceAppendLifecycleRecord
	proofResourceAppendLifecycleRecord = func(store *Store, event Event) error {
		if event.Note == "proof completion retry RunAttempt prepared from terminal predecessor" {
			return errors.New("injected completion retry lifecycle append failure")
		}
		return originalAppend(store, event)
	}
	_, secondErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(4*time.Second))
	proofResourceAppendLifecycleRecord = originalAppend
	if secondErr == nil {
		t.Fatal("second completion unexpectedly crossed retry lifecycle append fault")
	}

	recovered, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("third exact completion did not recover retry reachability: %v", err)
	}
	if recovered.VerificationRef == "" || !recovered.ResourceReleased {
		t.Fatalf("recovered completion = %+v, want verified released result", recovered)
	}
	pendingPath, err := s.proofResourceCompletionPendingPath(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered completion retained pending journal: %v", err)
	}
	status, err := landlease.Status(s.home, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Held {
		t.Fatalf("recovered completion retained lease: %+v", status)
	}
	finalWork, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := s.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.WorkID != w.ID {
			continue
		}
		if !contains(finalWork.RunAttemptIDs, attempt.ID) {
			t.Fatalf("retry attempt %s is not reachable from Work: %v", attempt.ID, finalWork.RunAttemptIDs)
		}
		if !runattempt.IsTerminal(attempt.Status) {
			t.Fatalf("retry attempt %s remained %q after recovery", attempt.ID, attempt.Status)
		}
	}
}

func TestProofResourceCompletionRetryRejectsAmbiguousDescendants(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "ambiguous retry descendants", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}
	originalWriteEvent := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("injected initial completion failure")
		}
		return originalWriteEvent(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWriteEvent
	if firstErr == nil {
		t.Fatal("initial completion unexpectedly succeeded")
	}

	pending, present, pendingPath, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("initial completion failure did not retain retry journal")
	}
	currentWork, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	retryInput := ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		Workspace: acquired.Reservation.Workspace,
		Command:   acquired.Reservation.Command,
	}
	firstChild, err := s.prepareProofRunAttempt(currentWork, pending.Action.ID, retryInput, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	secondChild, err := s.prepareProofRunAttempt(currentWork, pending.Action.ID, retryInput, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if firstChild.PredecessorID != pending.Result.RunAttemptID ||
		secondChild.PredecessorID != pending.Result.RunAttemptID {
		t.Fatalf(
			"ambiguous children do not share pending predecessor: first=%+v second=%+v pending=%s",
			firstChild,
			secondChild,
			pending.Result.RunAttemptID,
		)
	}

	_, err = s.adoptProofCompletionRetryDescendant(
		&currentWork,
		acquired.Reservation,
		pending,
		pendingPath,
		w.ID,
		proofActorAlpha,
		fixedNow.Add(6*time.Second),
	)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("two matching retry descendants error = %v, want fail-closed ambiguity", err)
	}
	finalWork, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range []runattempt.RunAttempt{firstChild, secondChild} {
		got, err := s.runAttempts().Get(child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !contains(finalWork.RunAttemptIDs, child.ID) {
			t.Fatalf("ambiguous child %s is not reachable from Work: %v", child.ID, finalWork.RunAttemptIDs)
		}
		if !runattempt.IsTerminal(got.Status) {
			t.Fatalf("ambiguous child %s remained %q", child.ID, got.Status)
		}
	}
	if len(finalWork.VerificationIDs) != 0 {
		t.Fatalf("ambiguous retry lineage minted verification: %v", finalWork.VerificationIDs)
	}
}

func TestProofResourceCompletionRetryRecoversCrashAfterMintBeforeJournal(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "retry mint crash recovery", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}

	originalWriteEvent := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("injected initial completion failure")
		}
		return originalWriteEvent(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWriteEvent
	if firstErr == nil {
		t.Fatal("initial completion unexpectedly crossed result persistence fault")
	}

	originalAfterMint := proofResourceAfterCompletionRetryMint
	t.Cleanup(func() {
		proofResourceWriteEventRecord = originalWriteEvent
		proofResourceAfterCompletionRetryMint = originalAfterMint
	})
	proofResourceAfterCompletionRetryMint = func(runattempt.RunAttempt) {
		panic("synthetic process death after completion retry mint")
	}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("completion retry did not reach mint crash boundary")
			}
		}()
		_, _ = s.CompleteProofResource(w.ID, input, fixedNow.Add(4*time.Second))
	}()
	proofResourceAfterCompletionRetryMint = originalAfterMint

	afterCrashWork, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterCrashAttempts, err := s.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	crashedRetryID := ""
	for _, attempt := range afterCrashAttempts {
		if attempt.WorkID != w.ID || attempt.PredecessorID != acquired.Reservation.RunAttemptID {
			continue
		}
		if crashedRetryID != "" {
			t.Fatalf("mint crash created multiple retry descendants: %s and %s", crashedRetryID, attempt.ID)
		}
		crashedRetryID = attempt.ID
		if attempt.Status != runattempt.StatusPending {
			t.Fatalf("mint-crash retry %s became %q before journal/Work reachability", attempt.ID, attempt.Status)
		}
	}
	if crashedRetryID == "" {
		t.Fatal("mint crash did not leave a recoverable retry descendant")
	}
	if contains(afterCrashWork.RunAttemptIDs, crashedRetryID) {
		t.Fatalf("mint-crash retry unexpectedly entered Work before crash boundary: %v", afterCrashWork.RunAttemptIDs)
	}

	recovered, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("exact retry did not recover mint crash: %v", err)
	}
	if recovered.VerificationRef == "" || !recovered.ResourceReleased || recovered.RecoveryJournalRetained {
		t.Fatalf("mint crash recovery = %+v, want verified released result", recovered)
	}
	status, err := landlease.Status(s.home, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Held {
		t.Fatalf("mint crash recovery retained lease: %+v", status)
	}
	finalWork, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := s.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	retryCount := 0
	for _, attempt := range attempts {
		if attempt.WorkID != w.ID {
			continue
		}
		if attempt.PredecessorID == acquired.Reservation.RunAttemptID {
			retryCount++
		}
		if !contains(finalWork.RunAttemptIDs, attempt.ID) {
			t.Fatalf("mint-crash attempt %s not reachable from Work: %v", attempt.ID, finalWork.RunAttemptIDs)
		}
		if !runattempt.IsTerminal(attempt.Status) {
			t.Fatalf("mint-crash attempt %s remained %q after recovery", attempt.ID, attempt.Status)
		}
	}
	if retryCount != 1 {
		t.Fatalf("mint crash recovery minted %d retry attempts, want one adopted retry", retryCount)
	}
}

func TestProofResourceCommittedVerificationReturnsReleasePendingUntilRecovered(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "truthful retained release", proofActorAlpha, fixedNow)
	successorWork := claimedProofWork(t, s, "successor after release recovery", proofActorBeta, fixedNow.Add(time.Second))
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}

	stateDir := filepath.Join(s.home, "state")
	originalWriteWork := proofResourceWriteWorkRecord
	proofResourceWriteWorkRecord = func(store *Store, stage string, current *envelope.WorkEnvelope) error {
		if err := originalWriteWork(store, stage, current); err != nil {
			return err
		}
		if stage == "work_final" {
			return os.Chmod(stateDir, 0o555)
		}
		return nil
	}
	completed, completeErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteWorkRecord = originalWriteWork
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if completeErr == nil {
		t.Fatal("committed verification with retained lease returned ordinary success")
	}
	var releasePending *ProofResourceReleasePendingError
	if !errors.As(completeErr, &releasePending) ||
		releasePending.ReservationID != acquired.Reservation.ID ||
		releasePending.Holder != acquired.Reservation.Holder {
		t.Fatalf("retained release error = %v, want typed exact-holder recovery error", completeErr)
	}
	if completed.VerificationRef == "" ||
		completed.ResourceReleased ||
		!completed.RecoveryJournalRetained ||
		completed.ReleaseWarning == "" {
		t.Fatalf("retained release result did not expose recovery state: %+v", completed)
	}
	afterCommit, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(afterCommit.VerificationIDs, completed.VerificationRef) {
		t.Fatalf("authoritative commit lost verification %q: %v", completed.VerificationRef, afterCommit.VerificationIDs)
	}
	status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Held || status.Holder != acquired.Reservation.Holder {
		t.Fatalf("release failure did not retain exact holder: %+v", status)
	}

	samples, timingErr := ReadProofResourceTimings(s.home, "", 20)
	if timingErr != nil || len(samples) != 1 || samples[0].ObservedReservedMS != nil || samples[0].ReleasedAt != "" {
		t.Fatalf("retained holder must not report release timing: %+v, %v", samples, timingErr)
	}
	recovered, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("exact completion retry did not release retained holder: %v", err)
	}
	if !recovered.ResourceReleased || recovered.RecoveryJournalRetained {
		t.Fatalf("release recovery = %+v, want released and journal cleared", recovered)
	}
	samples, timingErr = ReadProofResourceTimings(s.home, "", 20)
	if timingErr != nil || len(samples) != 1 || samples[0].ObservedReservedMS == nil || samples[0].ReleasedAt == "" {
		t.Fatalf("recovered release must complete the same timing row: %+v, %v", samples, timingErr)
	}
	pendingPath, err := s.proofResourceCompletionPendingPath(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release recovery retained pending journal: %v", err)
	}
	successor, err := reserveProofResource(t, s, successorWork.ID, ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-beta",
		Command:   "compound proof-check --out /synthetic/report-beta.json",
	}, fixedNow.Add(6*time.Second))
	if err != nil {
		t.Fatalf("successor remained wedged after release recovery: %v", err)
	}
	if successor.Reservation.Status != ProofResourceAcquired {
		t.Fatalf("successor reservation = %+v, want acquired", successor.Reservation)
	}
}

func TestProofResourceCommittedVerificationDoesNotReturnCleanupError(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "truthful cleanup commit", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	completionDir := filepath.Join(s.home, "work", "proof-resource-completions")
	originalWrite := proofResourceWriteWorkRecord
	proofResourceWriteWorkRecord = func(store *Store, stage string, current *envelope.WorkEnvelope) error {
		if err := originalWrite(store, stage, current); err != nil {
			return err
		}
		if stage == "work_final" {
			return os.Chmod(completionDir, 0o555)
		}
		return nil
	}
	defer func() {
		proofResourceWriteWorkRecord = originalWrite
		_ = os.Chmod(completionDir, 0o755)
	}()

	completed, completeErr := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(3*time.Second))
	if completeErr != nil {
		t.Fatalf("verification became visible but completion returned error: %v", completeErr)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.VerificationRef == "" || !contains(after.VerificationIDs, completed.VerificationRef) {
		t.Fatalf("successful commit omitted verification: result=%+v work=%v", completed, after.VerificationIDs)
	}
}

func claimedProofWork(t *testing.T, s *Store, predicate, actor string, now time.Time) envelope.WorkEnvelope {
	t.Helper()
	w, err := s.Create(CreateInput{
		Owner:            "proof-admission",
		Actor:            actor,
		SuccessPredicate: predicate,
	}, now)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	w, err = s.Claim(w.ID, actor, now.Add(time.Second))
	if err != nil {
		t.Fatalf("claim work: %v", err)
	}
	return w
}

type stoppedProofProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startStoppedProofProcess(t *testing.T) *stoppedProofProcess {
	t.Helper()
	startedPath := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command("sh", "-c", `printf started > "$1"; while :; do sleep 1; done`, "proof-predecessor", startedPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start predecessor process group: %v", err)
	}
	process := &stoppedProofProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		process.err = cmd.Wait()
		close(process.done)
	}()
	waitForTestFile(t, startedPath, 3*time.Second)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stop predecessor process group: %v", err)
	}
	t.Cleanup(func() {
		if !process.terminated() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-process.done:
			case <-time.After(3 * time.Second):
				t.Errorf("predecessor process group did not terminate during cleanup")
			}
		}
	})
	return process
}

func (p *stoppedProofProcess) terminated() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *stoppedProofProcess) terminate(t *testing.T) {
	t.Helper()
	if p.terminated() {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("terminate predecessor process group: %v", err)
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("predecessor process group did not confirm termination")
	}
}

func recordTestProofTerminationEvidence(
	t *testing.T,
	homePath, runAttemptID string,
	observedAt time.Time,
) string {
	t.Helper()
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(ProofResourceTerminationEvidence{
		SchemaVersion: ProofResourceTerminationEvidenceSchemaVersion,
		RunAttemptID:  runAttemptID,
		Status:        "terminated_and_waited",
		ObservedAt:    observedAt.UTC().Format(time.RFC3339Nano),
		Observer:      "test-owned-process-supervisor",
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(append(body, '\n'))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func waitForTestFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", filepath.Base(path))
		}
	}
}

func waitForProofLeaseWork(t *testing.T, homePath, workID string, now time.Time) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := landlease.Status(homePath, now)
		if err != nil {
			t.Fatal(err)
		}
		_, _, heldWorkID, ok := parseProofResourceHolder(status.Holder)
		if ok && heldWorkID == workID {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for proof lease work %s; status=%+v", workID, status)
		}
	}
}

func writeProofResourceStealGrant(t *testing.T, homePath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(homePath, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte(`standing_grants:
  - id: operator_proof_resource_steal
    capability: proof.resource
    mutation_class: security
    reversibility: manual
    blast_radius: repo
    expires_at: "`+fixedNow.Add(365*24*time.Hour).Format(time.RFC3339)+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestProofRecoveryNode(
	t *testing.T,
	s *Store,
	reservationID, recoveredHolder, priorHolder string,
	now time.Time,
) string {
	t.Helper()
	actor := "codex:" + strings.TrimPrefix(reservationID, "pfr_")
	w := claimedProofWork(t, s, reservationID, actor, now)
	ra, err := s.runAttempts().Mint(w.ID, actor, proofResourceHarness, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	reservation := ProofResourceReservation{
		SchemaVersion:   ProofResourceReservationSchemaVersion,
		ID:              reservationID,
		WorkID:          w.ID,
		Actor:           actor,
		RunAttemptID:    ra.ID,
		RecoveredHolder: recoveredHolder,
		PriorHolder:     priorHolder,
	}
	if err := s.writeEventJSON(reservation.ID, &reservation); err != nil {
		t.Fatal(err)
	}
	return proofResourceHolder(reservation.ID, ra.ID, w.ID, actor)
}

func TestResumeProofResourceCompletionRearmsExactExpiredHolderWithoutNewReservation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "resume exact expired proof completion", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResourceWithLiveBarrier(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       3 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	completeInput := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}
	if _, err := s.CompleteProofResource(w.ID, completeInput, fixedNow.Add(2*time.Second)); err == nil {
		t.Fatal("completion released while the execution barrier was live")
	}
	expiredAt := fixedNow.Add(5 * time.Second)
	resumeInput := ProofResourceResumeInput{
		Actor:              proofActorAlpha,
		Workspace:          acquired.Reservation.Workspace,
		Command:            acquired.Reservation.Command,
		Profile:            acquired.Reservation.Profile,
		PrepareOwnedReport: func(ProofResourceResult) error { return nil },
	}
	if _, found, err := s.ResumeProofResourceCompletion(w.ID, resumeInput, expiredAt); !found || err == nil {
		t.Fatalf("live descendant expired resume = found:%t err:%v, want found refusal", found, err)
	}
	status, err := landlease.Status(s.home, expiredAt)
	if err != nil {
		t.Fatal(err)
	}
	if !status.IsExpired || status.Holder != acquired.Reservation.Holder {
		t.Fatalf("live-descendant refusal changed expired holder: %+v", status)
	}
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}

	resumed, found, err := s.ResumeProofResourceCompletion(w.ID, resumeInput, expiredAt)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !resumed.ResourceReleased || resumed.VerificationRef == "" {
		t.Fatalf("expired exact resume = found:%t result:%+v", found, resumed)
	}
	attempts, err := s.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	proofAttempts := 0
	for _, attempt := range attempts {
		if attempt.WorkID == w.ID && attempt.Harness == proofResourceHarness {
			proofAttempts++
		}
	}
	if proofAttempts != 2 {
		t.Fatalf("proof RunAttempts = %d, want original plus completion-retry only", proofAttempts)
	}
}

func TestResumeProofResourceCompletionReconcilesCrashAfterExactRearm(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "reconcile exact rearm crash", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResourceWithLiveBarrier(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       3 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	completeInput := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}
	if _, err := s.CompleteProofResource(w.ID, completeInput, fixedNow.Add(2*time.Second)); err == nil {
		t.Fatal("completion released while execution barrier remained live")
	}
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	prepareCalls := 0
	resumeInput := ProofResourceResumeInput{
		Actor:     proofActorAlpha,
		Workspace: acquired.Reservation.Workspace,
		Command:   acquired.Reservation.Command,
		Profile:   acquired.Reservation.Profile,
		PrepareOwnedReport: func(ProofResourceResult) error {
			prepareCalls++
			if prepareCalls == 1 {
				return errors.New("injected failure after rearm outcome publication")
			}
			return nil
		},
	}
	expiredAt := fixedNow.Add(5 * time.Second)
	originalHook := proofResourceAfterScarceMutation
	t.Cleanup(func() { proofResourceAfterScarceMutation = originalHook })
	crashed := false
	proofResourceAfterScarceMutation = func(phase string) {
		if phase == "completion_rearm" {
			crashed = true
			panic("synthetic crash after exact rearm")
		}
		originalHook(phase)
	}
	func() {
		defer func() { _ = recover() }()
		_, _, _ = s.ResumeProofResourceCompletion(w.ID, resumeInput, expiredAt)
	}()
	proofResourceAfterScarceMutation = originalHook
	if !crashed {
		t.Fatal("resume did not reach post-rearm crash boundary")
	}
	rearmedStatus, err := landlease.Status(s.home, expiredAt)
	if err != nil {
		t.Fatal(err)
	}
	if !rearmedStatus.Held || rearmedStatus.Holder != acquired.Reservation.Holder {
		t.Fatalf("post-crash exact holder = %+v", rearmedStatus)
	}

	if _, found, err := s.ResumeProofResourceCompletion(w.ID, resumeInput, expiredAt.Add(time.Second)); !found || err == nil {
		t.Fatalf("post-rearm reconciliation did not stop at injected publication failure: found=%t err=%v", found, err)
	}
	heldAfterReconciliation, err := landlease.Status(s.home, expiredAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !heldAfterReconciliation.Held || heldAfterReconciliation.Holder != acquired.Reservation.Holder {
		t.Fatalf("post-reconciliation failure released exact holder: %+v", heldAfterReconciliation)
	}

	resumed, found, err := s.ResumeProofResourceCompletion(w.ID, resumeInput, expiredAt.Add(2*time.Second))
	if err != nil {
		t.Fatalf("exact retry did not reuse reconciled rearm outcome: %v", err)
	}
	if !found || !resumed.ResourceReleased || resumed.VerificationRef == "" {
		t.Fatalf("post-rearm crash recovery = found:%t result:%+v", found, resumed)
	}
	if resumed.Result.ReservationID != acquired.Reservation.ID {
		t.Fatalf("post-rearm recovery changed reservation %q, want %q", resumed.Result.ReservationID, acquired.Reservation.ID)
	}

	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	refreshRefs := map[string]struct{}{}
	rearmPreparations := 0
	rearmOutcomes := 0
	for _, event := range events {
		if event.Note == "proof resource completion_rearm governance persisted before scarce mutation" {
			rearmPreparations++
		}
		if event.Note == "exact expired proof completion holder rearmed after journal validation and execution attestation" {
			rearmOutcomes++
		}
		for _, ref := range event.Attached {
			if strings.HasPrefix(ref, "pfrh_") {
				refreshRefs[ref] = struct{}{}
			}
		}
	}
	if rearmPreparations != 1 || rearmOutcomes != 1 || len(refreshRefs) != 1 {
		t.Fatalf(
			"rearm evidence preparations/outcomes/refreshes = %d/%d/%d, want 1/1/1",
			rearmPreparations,
			rearmOutcomes,
			len(refreshRefs),
		)
	}
}

func TestProofResourceCompletionRetryRecoversLegacyPendingJournalCrashBeforeMarkRunning(t *testing.T) {
	s, w, acquired, input, _, pendingPath := proofCompletionPendingFixture(t, "legacy pending retry journal crash")
	originalWriter := proofResourceWriteCompletionRetryJournal
	originalAfterJournal := proofResourceAfterCompletionRetryJournal
	t.Cleanup(func() {
		proofResourceWriteCompletionRetryJournal = originalWriter
		proofResourceAfterCompletionRetryJournal = originalAfterJournal
	})
	proofResourceWriteCompletionRetryJournal = func(
		store *Store,
		path string,
		pending proofResourceCompletionPending,
		retry runattempt.RunAttempt,
	) error {
		pending.GovernanceSeal = ""
		pending.Result.RunAttemptStatus = retry.Status
		pending.Result.VerificationRef = ""
		return store.writeProofResourceCompletionPending(path, pending)
	}
	crashed := false
	proofResourceAfterCompletionRetryJournal = func(retry runattempt.RunAttempt, pending proofResourceCompletionPending) {
		crashed = true
		panic("synthetic crash after legacy retry journal write")
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = s.CompleteProofResource(w.ID, input, fixedNow.Add(4*time.Second))
	}()
	proofResourceWriteCompletionRetryJournal = originalWriter
	proofResourceAfterCompletionRetryJournal = originalAfterJournal
	if !crashed {
		t.Fatal("retry did not reach post-journal crash boundary")
	}
	pending, present, _, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil || !present {
		t.Fatalf("read crash journal: present=%t err=%v", present, err)
	}
	if pending.Result.RunAttemptStatus != runattempt.StatusPending || pending.Result.VerificationRef != "" {
		t.Fatalf("crash journal projection = %q/%q", pending.Result.RunAttemptStatus, pending.Result.VerificationRef)
	}
	before, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.ResourceReleased || recovered.VerificationRef == "" {
		t.Fatalf("legacy pending crash recovery = %+v", recovered)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered pending journal remains (before=%d bytes): %v", len(before), err)
	}
}

func TestProofResourceCompletionLegacyGovernanceMutationRejectedByteIdentical(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*proofResourceCompletionPending)
	}{
		{name: "action id", mutate: func(p *proofResourceCompletionPending) { p.Action.ID = "act_changed" }},
		{name: "action preview", mutate: func(p *proofResourceCompletionPending) { p.Action.Preview += " changed" }},
		{name: "action rollback", mutate: func(p *proofResourceCompletionPending) { p.Action.RollbackPlan += " changed" }},
		{name: "action arguments", mutate: func(p *proofResourceCompletionPending) { p.Action.ArgumentsHash = "sha256:changed" }},
		{name: "action source", mutate: func(p *proofResourceCompletionPending) {
			p.Action.InputSources = []envelope.SourceBinding{{
				SourceID:        "src_changed",
				ContentHash:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				TrustClass:      envelope.SourceTrustAuthenticatedOperator,
				Confidentiality: envelope.ConfidentialityInternal,
			}}
		}},
		{name: "action security", mutate: func(p *proofResourceCompletionPending) {
			p.Action.SecurityFloorOutcome = "hold"
			p.Action.SecurityReasons = []string{"changed"}
		}},
		{name: "decision id", mutate: func(p *proofResourceCompletionPending) { p.Decision.ID = "pol_changed" }},
		{name: "decision outcome", mutate: func(p *proofResourceCompletionPending) {
			p.Decision.Outcome = "allow_with_obligations"
			p.Decision.Obligations = []string{"changed"}
		}},
		{name: "decision reasons", mutate: func(p *proofResourceCompletionPending) { p.Decision.Reasons = append(p.Decision.Reasons, "changed") }},
		{name: "decision obligations", mutate: func(p *proofResourceCompletionPending) {
			p.Decision.Obligations = append(p.Decision.Obligations, "changed")
		}},
		{name: "decision grant", mutate: func(p *proofResourceCompletionPending) { p.Decision.StandingGrantID = "grant_changed" }},
		{name: "decision expiry", mutate: func(p *proofResourceCompletionPending) {
			p.Decision.ExpiresAt = fixedNow.Add(365 * 24 * time.Hour).Format(time.RFC3339)
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			s, w, _, input, pending, pendingPath := proofCompletionPendingFixture(t, "governance mutation "+tc.name)
			pending.GovernanceSeal = ""
			tc.mutate(&pending)
			if err := s.writeProofResourceCompletionPending(pendingPath, pending); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(pendingPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CompleteProofResource(w.ID, input, fixedNow.Add(4*time.Second)); err == nil {
				t.Fatal("changed legacy governance was accepted")
			}
			after, err := os.ReadFile(pendingPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("governance rejection mutated pending journal")
			}
		})
	}
}

func TestRecoverLegacyProofResourceCompletionReleasesWithoutVerification(t *testing.T) {
	s, w, acquired, _, _, _ := proofCompletionPendingFixture(
		t,
		"legacy recovery-only release",
	)
	beforeVerification := append([]string(nil), w.VerificationIDs...)
	recovered, found, err := s.RecoverLegacyProofResourceCompletion(
		w.ID,
		ProofResourceLegacyRecoveryInput{
			Actor:     proofActorAlpha,
			Workspace: acquired.Reservation.Workspace,
			Command:   acquired.Reservation.Command,
		},
		fixedNow.Add(4*time.Second),
	)
	if err != nil || !found {
		t.Fatalf("legacy recovery: found=%t result=%+v err=%v", found, recovered, err)
	}
	if !recovered.ResourceReleased ||
		!recovered.FreshBatteryRequired ||
		recovered.VerificationRef != "" {
		t.Fatalf("legacy recovery promoted old proof: %+v", recovered)
	}
	status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Held || status.Holder != "" {
		t.Fatalf("legacy recovery retained shared proof holder: %+v", status)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.VerificationIDs, beforeVerification) {
		t.Fatalf("legacy recovery emitted verification: before=%v after=%v", beforeVerification, current.VerificationIDs)
	}
	_, present, _, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("legacy recovery retained completion journal after release")
	}
	attempt, err := s.runAttempts().Get(acquired.Reservation.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !runattempt.IsTerminal(attempt.Status) {
		t.Fatalf("legacy recovery left RunAttempt active: %+v", attempt)
	}
}

func TestRecoverLegacyProofResourceCompletionRearmsExpiredExactHolder(t *testing.T) {
	s, w, acquired, _, _, _ := proofCompletionPendingFixture(
		t,
		"expired exact legacy recovery-only release",
	)
	expiredAt := fixedNow.Add(2*time.Second + time.Minute)
	recovered, found, err := s.RecoverLegacyProofResourceCompletion(
		w.ID,
		ProofResourceLegacyRecoveryInput{
			Actor:     proofActorAlpha,
			Workspace: acquired.Reservation.Workspace,
			Command:   acquired.Reservation.Command,
		},
		expiredAt.Add(time.Second),
	)
	if err != nil || !found {
		t.Fatalf("expired exact legacy recovery: found=%t result=%+v err=%v", found, recovered, err)
	}
	if !recovered.ResourceReleased ||
		!recovered.FreshBatteryRequired ||
		recovered.VerificationRef != "" {
		t.Fatalf("expired exact legacy recovery promoted old proof: %+v", recovered)
	}
	status, err := landlease.Status(s.home, expiredAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Held || status.Holder != "" {
		t.Fatalf("expired exact legacy recovery retained holder: %+v", status)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.VerificationIDs) != 0 {
		t.Fatalf("expired exact legacy recovery emitted verification: %v", current.VerificationIDs)
	}
}

func TestRecoverLegacyProofResourceCompletionReusesGovernanceAfterActionPersistenceCrash(t *testing.T) {
	s, w, acquired, _, _, _ := proofCompletionPendingFixture(
		t,
		"legacy recovery action persistence crash",
	)
	in := ProofResourceLegacyRecoveryInput{
		Actor:     proofActorAlpha,
		Workspace: acquired.Reservation.Workspace,
		Command:   acquired.Reservation.Command,
	}
	originalWrite := proofResourceWriteEventRecord
	t.Cleanup(func() {
		proofResourceWriteEventRecord = originalWrite
	})
	var actions []envelope.ActionIntent
	failed := false
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "legacy_recovery_prepare_action" {
			action, ok := value.(*envelope.ActionIntent)
			if !ok {
				t.Fatalf("legacy recovery action value = %T", value)
			}
			actions = append(actions, *action)
			if !failed {
				failed = true
				return errors.New("synthetic legacy recovery action persistence crash")
			}
		}
		return originalWrite(store, stage, id, value)
	}

	_, found, err := s.RecoverLegacyProofResourceCompletion(
		w.ID,
		in,
		fixedNow.Add(4*time.Second),
	)
	if err == nil || !found {
		t.Fatalf("first recovery = found=%t err=%v, want injected failure", found, err)
	}
	recovered, found, err := s.RecoverLegacyProofResourceCompletion(
		w.ID,
		in,
		fixedNow.Add(9*time.Second),
	)
	if err != nil || !found || !recovered.ResourceReleased {
		t.Fatalf("retry recovery: found=%t result=%+v err=%v", found, recovered, err)
	}
	if len(actions) != 2 || !reflect.DeepEqual(actions[0], actions[1]) {
		t.Fatalf("recovery regenerated ActionIntent after crash: %+v", actions)
	}
}

func TestRecoverLegacyProofResourceCompletionJournalFailurePrecedesEveryMutation(t *testing.T) {
	s, w, acquired, _, pending, completionPath := proofCompletionPendingFixture(
		t,
		"legacy recovery journal pre-mutation boundary",
	)
	beforeWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	beforeCompletion, err := os.ReadFile(completionPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeAttempt, err := s.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	originalWriteJournal := proofResourceWriteLegacyRecoveryJournal
	t.Cleanup(func() {
		proofResourceWriteLegacyRecoveryJournal = originalWriteJournal
	})
	proofResourceWriteLegacyRecoveryJournal = func(
		_ *Store,
		_ string,
		_ proofResourceLegacyRecoveryPending,
	) error {
		return errors.New("synthetic legacy recovery journal persistence failure")
	}
	_, found, err := s.RecoverLegacyProofResourceCompletion(
		w.ID,
		ProofResourceLegacyRecoveryInput{
			Actor:     proofActorAlpha,
			Workspace: acquired.Reservation.Workspace,
			Command:   acquired.Reservation.Command,
		},
		fixedNow.Add(4*time.Second),
	)
	if err == nil || !found {
		t.Fatalf("journal persistence failure: found=%t err=%v", found, err)
	}
	if _, present, _, err := s.readProofResourceLegacyRecoveryPending(w.ID); err != nil || present {
		t.Fatalf("failed journal write became visible: present=%t err=%v", present, err)
	}
	afterWork, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	afterCompletion, err := os.ReadFile(completionPath)
	if err != nil {
		t.Fatal(err)
	}
	afterAttempt, err := s.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeWork, afterWork) ||
		!bytes.Equal(beforeCompletion, afterCompletion) ||
		!reflect.DeepEqual(beforeAttempt, afterAttempt) {
		t.Fatal("legacy recovery mutated work, completion, or RunAttempt before journal commit")
	}
	status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Held || status.Holder != acquired.Reservation.Holder {
		t.Fatalf("legacy recovery mutated holder before journal commit: %+v", status)
	}
}

func TestRecoverLegacyProofResourceCompletionConvergesAcrossCrashBoundaries(t *testing.T) {
	stages := []string{
		"action persistence",
		"decision persistence",
		"recovery persistence",
		"work persistence",
		"lifecycle persistence",
		"release",
		"after release",
		"completion cleanup",
		"recovery journal cleanup",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			s, w, acquired, _, _, _ := proofCompletionPendingFixture(
				t,
				"legacy recovery crash "+stage,
			)
			in := ProofResourceLegacyRecoveryInput{
				Actor:     proofActorAlpha,
				Workspace: acquired.Reservation.Workspace,
				Command:   acquired.Reservation.Command,
			}
			originalWriteEvent := proofResourceWriteEventRecord
			originalWriteWork := proofResourceWriteWorkRecord
			originalAppendLifecycle := proofResourceAppendLifecycleRecord
			originalRelease := proofResourceReleaseLegacyRecoveryAfter
			originalAfterRelease := proofResourceAfterLegacyRecoveryRelease
			originalClearCompletion := proofResourceClearLegacyCompletionPending
			originalClearRecovery := proofResourceClearLegacyRecoveryJournal
			t.Cleanup(func() {
				proofResourceWriteEventRecord = originalWriteEvent
				proofResourceWriteWorkRecord = originalWriteWork
				proofResourceAppendLifecycleRecord = originalAppendLifecycle
				proofResourceReleaseLegacyRecoveryAfter = originalRelease
				proofResourceAfterLegacyRecoveryRelease = originalAfterRelease
				proofResourceClearLegacyCompletionPending = originalClearCompletion
				proofResourceClearLegacyRecoveryJournal = originalClearRecovery
			})

			injected := false
			inject := func() error {
				if injected {
					return nil
				}
				injected = true
				return errors.New("synthetic legacy recovery " + stage + " crash")
			}
			freedCount := 0
			switch stage {
			case "action persistence", "decision persistence", "recovery persistence":
				target := map[string]string{
					"action persistence":   "legacy_recovery_prepare_action",
					"decision persistence": "legacy_recovery_prepare_decision",
					"recovery persistence": "legacy_recovery",
				}[stage]
				proofResourceWriteEventRecord = func(store *Store, gotStage, id string, value any) error {
					if gotStage == target {
						if err := inject(); err != nil {
							return err
						}
					}
					return originalWriteEvent(store, gotStage, id, value)
				}
			case "work persistence":
				proofResourceWriteWorkRecord = func(store *Store, gotStage string, work *envelope.WorkEnvelope) error {
					if gotStage == "legacy_recovery_work" {
						if err := inject(); err != nil {
							return err
						}
					}
					return originalWriteWork(store, gotStage, work)
				}
			case "lifecycle persistence":
				proofResourceAppendLifecycleRecord = func(store *Store, event Event) error {
					if strings.Contains(event.Note, "fresh source-bound v0.3 battery required") {
						if err := inject(); err != nil {
							return err
						}
					}
					return originalAppendLifecycle(store, event)
				}
			case "release":
				proofResourceReleaseLegacyRecoveryAfter = func(
					homePath string,
					holder string,
					at time.Time,
					finalize func() error,
				) (landlease.ReleaseResult, error) {
					if err := inject(); err != nil {
						return landlease.ReleaseResult{}, err
					}
					release, err := originalRelease(homePath, holder, at, finalize)
					if release.Freed {
						freedCount++
					}
					return release, err
				}
			case "after release":
				proofResourceAfterLegacyRecoveryRelease = func(proofResourceLegacyRecoveryPending) error {
					return inject()
				}
			case "completion cleanup":
				proofResourceClearLegacyCompletionPending = func(
					store *Store,
					path string,
					pending proofResourceCompletionPending,
				) error {
					if err := inject(); err != nil {
						return err
					}
					return originalClearCompletion(store, path, pending)
				}
			case "recovery journal cleanup":
				proofResourceClearLegacyRecoveryJournal = func(
					store *Store,
					path string,
					pending proofResourceLegacyRecoveryPending,
				) error {
					if err := inject(); err != nil {
						return err
					}
					return originalClearRecovery(store, path, pending)
				}
			}
			if stage != "release" {
				wrappedRelease := proofResourceReleaseLegacyRecoveryAfter
				proofResourceReleaseLegacyRecoveryAfter = func(
					homePath string,
					holder string,
					at time.Time,
					finalize func() error,
				) (landlease.ReleaseResult, error) {
					release, err := wrappedRelease(homePath, holder, at, finalize)
					if release.Freed {
						freedCount++
					}
					return release, err
				}
			}

			_, found, err := s.RecoverLegacyProofResourceCompletion(
				w.ID,
				in,
				fixedNow.Add(4*time.Second),
			)
			if err == nil || !found || !injected {
				t.Fatalf("first recovery = found=%t injected=%t err=%v, want injected crash", found, injected, err)
			}
			journal, present, journalPath, err := s.readProofResourceLegacyRecoveryPending(w.ID)
			if err != nil || !present {
				t.Fatalf("retained recovery journal: present=%t err=%v", present, err)
			}
			journalBytes, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			retryClearRecovery := proofResourceClearLegacyRecoveryJournal
			proofResourceClearLegacyRecoveryJournal = func(
				store *Store,
				path string,
				pending proofResourceLegacyRecoveryPending,
			) error {
				retryBytes, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if !bytes.Equal(retryBytes, journalBytes) {
					return errors.New("legacy recovery journal bytes changed across retry")
				}
				return retryClearRecovery(store, path, pending)
			}
			current, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(current.VerificationIDs) != 0 {
				t.Fatalf("crash stage %q emitted verification: %v", stage, current.VerificationIDs)
			}
			statusAfterCrash, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			_, completionPresent, _, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if statusAfterCrash.Holder == acquired.Reservation.Holder && !completionPresent {
				t.Fatalf("crash stage %q removed completion before exact holder release", stage)
			}

			recovered, found, err := s.RecoverLegacyProofResourceCompletion(
				w.ID,
				in,
				fixedNow.Add(9*time.Second),
			)
			if err != nil || !found || !recovered.ResourceReleased {
				t.Fatalf("retry recovery: found=%t result=%+v err=%v", found, recovered, err)
			}
			if recovered.Recovery.ID != journal.Recovery.ID ||
				recovered.Recovery.ActionIntentID != journal.Action.ID ||
				recovered.Recovery.PolicyDecisionID != journal.Decision.ID {
				t.Fatalf("retry changed sealed recovery identity: journal=%+v result=%+v", journal, recovered)
			}
			if len(journalBytes) == 0 {
				t.Fatal("sealed recovery journal was empty")
			}
			if _, present, _, err := s.readProofResourceLegacyRecoveryPending(w.ID); err != nil || present {
				t.Fatalf("recovery journal cleanup: present=%t err=%v", present, err)
			}
			if _, present, _, err := s.readProofResourceCompletionPending(acquired.Reservation.ID); err != nil || present {
				t.Fatalf("completion journal cleanup: present=%t err=%v", present, err)
			}
			status, err := landlease.Status(s.home, fixedNow.Add(9*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if status.Holder == acquired.Reservation.Holder {
				t.Fatalf("retry retained exact proof holder: %+v", status)
			}
			if freedCount != 1 {
				t.Fatalf("release freed count = %d, want 1", freedCount)
			}
			events, err := s.eventsFor(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			lifecycleCount := 0
			for _, event := range events {
				if reflect.DeepEqual(event, journal.LifecycleEvent) {
					lifecycleCount++
				}
			}
			if lifecycleCount != 1 {
				t.Fatalf("recovery lifecycle count = %d, want 1", lifecycleCount)
			}
			current, err = s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(current.VerificationIDs) != 0 {
				t.Fatalf("retry stage %q emitted verification: %v", stage, current.VerificationIDs)
			}
		})
	}
}

func TestRecoverLegacyProofResourceCompletionReleasesReportlessInfrastructureFailure(t *testing.T) {
	s, w, acquired, _, pending, _ := proofReportlessCompletionPendingFixture(
		t,
		"reportless retained infrastructure failure",
	)
	recovered, found, err := s.RecoverLegacyProofResourceCompletion(
		w.ID,
		ProofResourceLegacyRecoveryInput{
			Actor:     proofActorAlpha,
			Workspace: acquired.Reservation.Workspace,
			Command:   acquired.Reservation.Command,
		},
		fixedNow.Add(4*time.Second),
	)
	if err != nil || !found {
		t.Fatalf("reportless legacy recovery: found=%t result=%+v err=%v", found, recovered, err)
	}
	if !recovered.ResourceReleased ||
		!recovered.FreshBatteryRequired ||
		recovered.VerificationRef != "" ||
		recovered.Recovery.ReportArtifactRef != "" {
		t.Fatalf("reportless legacy recovery certified failure: %+v", recovered)
	}
	attempt, err := s.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != runattempt.StatusFailed {
		t.Fatalf("reportless legacy recovery RunAttempt = %s, want failed", attempt.Status)
	}
	status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Holder == acquired.Reservation.Holder {
		t.Fatalf("reportless legacy recovery retained holder: %+v", status)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.VerificationIDs) != 0 {
		t.Fatalf("reportless legacy recovery emitted verification: %v", current.VerificationIDs)
	}
}

func TestRecoverLegacyProofResourceCompletionReconcilesReleasedReportlessFailure(t *testing.T) {
	s, w, acquired, _, pending, _ := proofReportlessCompletionPendingFixture(
		t,
		"reportless released reconciliation",
	)
	in := ProofResourceLegacyRecoveryInput{
		Actor:     proofActorAlpha,
		Workspace: acquired.Reservation.Workspace,
		Command:   acquired.Reservation.Command,
	}
	originalAfterRelease := proofResourceAfterLegacyRecoveryRelease
	t.Cleanup(func() {
		proofResourceAfterLegacyRecoveryRelease = originalAfterRelease
	})
	failed := false
	proofResourceAfterLegacyRecoveryRelease = func(proofResourceLegacyRecoveryPending) error {
		if !failed {
			failed = true
			return errors.New("synthetic reportless post-release crash")
		}
		return nil
	}
	_, found, err := s.RecoverLegacyProofResourceCompletion(
		w.ID,
		in,
		fixedNow.Add(4*time.Second),
	)
	if err == nil || !found || !failed {
		t.Fatalf("reportless post-release crash: found=%t failed=%t err=%v", found, failed, err)
	}
	status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Holder == acquired.Reservation.Holder {
		t.Fatalf("reportless post-release crash retained holder: %+v", status)
	}
	recovered, found, err := s.RecoverLegacyProofResourceCompletion(
		w.ID,
		in,
		fixedNow.Add(9*time.Second),
	)
	if err != nil || !found || !recovered.ResourceReleased {
		t.Fatalf("reportless released reconciliation: found=%t result=%+v err=%v", found, recovered, err)
	}
	attempt, err := s.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != runattempt.StatusFailed {
		t.Fatalf("reconciled reportless RunAttempt = %s, want failed", attempt.Status)
	}
	if len(recovered.Work.VerificationIDs) != 0 || recovered.VerificationRef != "" {
		t.Fatalf("reconciled reportless recovery emitted verification: %+v", recovered)
	}
}

func TestRecoverLegacyProofResourceCompletionRefusesUnsafeReportlessShapesByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*proofResourceCompletionPending)
	}{
		{
			name: "empty failure reason",
			mutate: func(pending *proofResourceCompletionPending) {
				pending.Result.FailureReason = ""
				pending.TerminalReason = ""
			},
		},
		{
			name: "verification shaped",
			mutate: func(pending *proofResourceCompletionPending) {
				pending.VerificationRef = "proof-check:" + pending.Result.ID
				pending.Result.VerificationRef = pending.VerificationRef
				pending.Completion.VerificationRef = pending.VerificationRef
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w, acquired, _, pending, pendingPath := proofReportlessCompletionPendingFixture(
				t,
				"unsafe reportless "+tc.name,
			)
			tc.mutate(&pending)
			if err := s.writeProofResourceCompletionPending(pendingPath, pending); err != nil {
				t.Fatal(err)
			}
			beforeJournal, err := os.ReadFile(pendingPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			_, found, err := s.RecoverLegacyProofResourceCompletion(
				w.ID,
				ProofResourceLegacyRecoveryInput{
					Actor:     proofActorAlpha,
					Workspace: acquired.Reservation.Workspace,
					Command:   acquired.Reservation.Command,
				},
				fixedNow.Add(4*time.Second),
			)
			if err == nil || !found {
				t.Fatalf("unsafe reportless recovery: found=%t err=%v", found, err)
			}
			afterJournal, err := os.ReadFile(pendingPath)
			if err != nil {
				t.Fatal(err)
			}
			afterWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeJournal, afterJournal) || !bytes.Equal(beforeWork, afterWork) {
				t.Fatal("unsafe reportless recovery mutated completion journal or work")
			}
			if _, present, _, err := s.readProofResourceLegacyRecoveryPending(w.ID); err != nil || present {
				t.Fatalf("unsafe reportless recovery created recovery journal: present=%t err=%v", present, err)
			}
			status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if !status.Held || status.Holder != acquired.Reservation.Holder {
				t.Fatalf("unsafe reportless recovery released holder: %+v", status)
			}
		})
	}
}

func TestRecoverLegacyProofResourceCompletionRefusesCorruptRetainedRecoveryJournalByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(
			t *testing.T,
			s *Store,
			journal *proofResourceLegacyRecoveryPending,
			journalPath string,
			completionPath string,
			in *ProofResourceLegacyRecoveryInput,
		)
	}{
		{
			name: "wrong actor",
			mutate: func(
				_ *testing.T,
				_ *Store,
				_ *proofResourceLegacyRecoveryPending,
				_ string,
				_ string,
				in *ProofResourceLegacyRecoveryInput,
			) {
				in.Actor = proofActorBeta
			},
		},
		{
			name: "wrong command",
			mutate: func(
				_ *testing.T,
				_ *Store,
				_ *proofResourceLegacyRecoveryPending,
				_ string,
				_ string,
				in *ProofResourceLegacyRecoveryInput,
			) {
				in.Command += " --changed"
			},
		},
		{
			name: "corrupt governance seal",
			mutate: func(
				t *testing.T,
				s *Store,
				journal *proofResourceLegacyRecoveryPending,
				journalPath string,
				_ string,
				_ *ProofResourceLegacyRecoveryInput,
			) {
				journal.GovernanceSeal = strings.Repeat("0", 64)
				if err := s.writeJSON(journalPath, journal); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "completion differs sealed journal",
			mutate: func(
				t *testing.T,
				s *Store,
				journal *proofResourceLegacyRecoveryPending,
				_ string,
				completionPath string,
				_ *ProofResourceLegacyRecoveryInput,
			) {
				changed := journal.Completion
				changed.Result.FailureReason = "changed after recovery seal"
				if err := s.writeProofResourceCompletionPending(completionPath, changed); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w, acquired, _, _, completionPath := proofCompletionPendingFixture(
				t,
				"retained recovery journal "+tc.name,
			)
			in := ProofResourceLegacyRecoveryInput{
				Actor:     proofActorAlpha,
				Workspace: acquired.Reservation.Workspace,
				Command:   acquired.Reservation.Command,
			}
			originalWrite := proofResourceWriteEventRecord
			failed := false
			proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
				if stage == "legacy_recovery_prepare_action" && !failed {
					failed = true
					return errors.New("synthetic retained recovery action crash")
				}
				return originalWrite(store, stage, id, value)
			}
			_, found, err := s.RecoverLegacyProofResourceCompletion(
				w.ID,
				in,
				fixedNow.Add(4*time.Second),
			)
			proofResourceWriteEventRecord = originalWrite
			if err == nil || !found || !failed {
				t.Fatalf("seed retained recovery journal: found=%t failed=%t err=%v", found, failed, err)
			}
			journal, present, journalPath, err := s.readProofResourceLegacyRecoveryPending(w.ID)
			if err != nil || !present {
				t.Fatalf("read retained recovery journal: present=%t err=%v", present, err)
			}
			tc.mutate(t, s, &journal, journalPath, completionPath, &in)
			beforeJournal, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			_, found, err = s.RecoverLegacyProofResourceCompletion(
				w.ID,
				in,
				fixedNow.Add(9*time.Second),
			)
			if err == nil || !found {
				t.Fatalf("corrupt retained recovery: found=%t err=%v", found, err)
			}
			afterJournal, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			afterWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeJournal, afterJournal) || !bytes.Equal(beforeWork, afterWork) {
				t.Fatal("corrupt retained recovery mutated journal or work")
			}
			status, err := landlease.Status(s.home, fixedNow.Add(9*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if !status.Held || status.Holder != acquired.Reservation.Holder {
				t.Fatalf("corrupt retained recovery released holder: %+v", status)
			}
		})
	}
}

func TestRecoverLegacyProofResourceCompletionRefusesForeignOrCorruptIdentityByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(
			t *testing.T,
			s *Store,
			acquired ProofResourceReserveResult,
			pending proofResourceCompletionPending,
			path string,
			in *ProofResourceLegacyRecoveryInput,
		)
	}{
		{
			name: "wrong actor",
			mutate: func(
				_ *testing.T,
				_ *Store,
				_ ProofResourceReserveResult,
				_ proofResourceCompletionPending,
				_ string,
				in *ProofResourceLegacyRecoveryInput,
			) {
				in.Actor = proofActorBeta
			},
		},
		{
			name: "wrong command",
			mutate: func(
				_ *testing.T,
				_ *Store,
				_ ProofResourceReserveResult,
				_ proofResourceCompletionPending,
				_ string,
				in *ProofResourceLegacyRecoveryInput,
			) {
				in.Command += " --changed"
			},
		},
		{
			name: "wrong report",
			mutate: func(
				t *testing.T,
				s *Store,
				_ ProofResourceReserveResult,
				pending proofResourceCompletionPending,
				path string,
				_ *ProofResourceLegacyRecoveryInput,
			) {
				artifacts, err := home.NewArtifactStore(s.home)
				if err != nil {
					t.Fatal(err)
				}
				otherRef, err := artifacts.Put(validOwnedProofReportJSON("failed"))
				if err != nil {
					t.Fatal(err)
				}
				pending.Result.ReportArtifactRef = otherRef
				if err := s.writeProofResourceCompletionPending(path, pending); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w, acquired, _, pending, path := proofCompletionPendingFixture(
				t,
				"legacy recovery refusal "+tc.name,
			)
			in := ProofResourceLegacyRecoveryInput{
				Actor:     proofActorAlpha,
				Workspace: acquired.Reservation.Workspace,
				Command:   acquired.Reservation.Command,
			}
			tc.mutate(t, s, acquired, pending, path, &in)
			beforeJournal, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}

			_, found, err := s.RecoverLegacyProofResourceCompletion(
				w.ID,
				in,
				fixedNow.Add(4*time.Second),
			)
			if err == nil || !found {
				t.Fatalf("foreign/corrupt recovery: found=%t err=%v", found, err)
			}
			afterJournal, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			afterWork, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeJournal, afterJournal) || !bytes.Equal(beforeWork, afterWork) {
				t.Fatal("foreign/corrupt legacy recovery mutated journal or work")
			}
			status, err := landlease.Status(s.home, fixedNow.Add(4*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if !status.Held || status.Holder != acquired.Reservation.Holder {
				t.Fatalf("foreign/corrupt legacy recovery released holder: %+v", status)
			}
		})
	}
}

func proofCompletionPendingFixture(
	t *testing.T,
	title string,
) (*Store, envelope.WorkEnvelope, ProofResourceReserveResult, ProofResourceCompleteInput, proofResourceCompletionPending, string) {
	t.Helper()
	s := newStore(t)
	w := claimedProofWork(t, s, title, proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(validOwnedProofReportJSON("ok"))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}
	originalWrite := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("synthetic prepared-completion persistence fault")
		}
		return originalWrite(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWrite
	if firstErr == nil {
		t.Fatal("fixture completion did not retain pending journal")
	}
	pending, present, pendingPath, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil || !present {
		t.Fatalf("fixture pending journal: present=%t err=%v", present, err)
	}
	if pending.GovernanceSeal == "" {
		t.Fatal("new completion journal omitted governance seal")
	}
	return s, w, acquired, input, pending, pendingPath
}

func proofReportlessCompletionPendingFixture(
	t *testing.T,
	title string,
) (*Store, envelope.WorkEnvelope, ProofResourceReserveResult, ProofResourceCompleteInput, proofResourceCompletionPending, string) {
	t.Helper()
	s := newStore(t)
	w := claimedProofWork(t, s, title, proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ProducerExitKnown: false,
		FailureReason:     "proof runner infrastructure failed before report publication",
	}
	originalWrite := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("synthetic reportless prepared-completion persistence fault")
		}
		return originalWrite(store, stage, id, value)
	}
	_, firstErr := s.CompleteProofResource(w.ID, input, fixedNow.Add(3*time.Second))
	proofResourceWriteEventRecord = originalWrite
	if firstErr == nil {
		t.Fatal("reportless fixture completion did not retain pending journal")
	}
	pending, present, pendingPath, err := s.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil || !present {
		t.Fatalf("reportless fixture pending journal: present=%t err=%v", present, err)
	}
	if pending.GovernanceSeal == "" {
		t.Fatal("reportless completion journal omitted governance seal")
	}
	return s, w, acquired, input, pending, pendingPath
}

func TestProofFixtureBarrierCloseWaitsForForkToExecWindow(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "fixture fork-to-exec barrier", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResourceWithLiveBarrier(t, s, w.ID, ProofResourceReserveInput{
		Actor: proofActorAlpha, TTL: time.Minute, Workspace: "/synthetic/repo-alpha",
		Command: "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = acquired.ExecutionBarrier.Close() })
	inherited, err := acquired.ExecutionBarrier.DuplicateForChild()
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	// Model a forked child holding the same open-file description until exec.
	// Go holds ForkLock for writing during that interval. No subprocess is
	// started while this deterministic fixture holds the lock.
	syscall.ForkLock.Lock()
	locked := true
	defer func() {
		if locked {
			syscall.ForkLock.Unlock()
		}
	}()
	closing := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(closing)
		done <- closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier)
	}()
	<-closing
	closedBeforeExec := false
	select {
	case err = <-done:
		closedBeforeExec = true
	case <-time.After(50 * time.Millisecond):
	}
	// An actually inherited descriptor must still block the trusted producer;
	// fixture synchronization is not authority to declare a live tree dead.
	_, attestErr := landlease.AttestExecutionTerminated(s.home, acquired.Reservation.Holder)
	var pending *landlease.ExecutionPendingError
	if !errors.As(attestErr, &pending) {
		_ = inherited.Close()
		syscall.ForkLock.Unlock()
		locked = false
		t.Fatalf("inherited descriptor attestation = %v, want execution pending", attestErr)
	}
	if closeErr := inherited.Close(); closeErr != nil {
		syscall.ForkLock.Unlock()
		locked = false
		t.Fatal(closeErr)
	}
	syscall.ForkLock.Unlock()
	locked = false
	if !closedBeforeExec {
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("fixture barrier close did not finish after exec window")
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if closedBeforeExec {
		t.Fatal("fixture barrier close completed before inherited fork-to-exec descriptor was released")
	}
	if _, err := landlease.AttestExecutionTerminated(s.home, acquired.Reservation.Holder); err != nil {
		t.Fatalf("closed fixture barrier retained execution tree: %v", err)
	}
}

func closeProofFixtureExecutionBarrier(barrier *landlease.ExecutionBarrier) error {
	// A concurrent Linux test subprocess can inherit even an O_CLOEXEC
	// descriptor between fork and exec. Wait out that window and exclude new
	// forks only during close, never during launch, store work or attestation.
	syscall.ForkLock.RLock()
	err := barrier.Close()
	syscall.ForkLock.RUnlock()
	return err
}

func reserveProofResource(
	t *testing.T,
	s *Store,
	workID string,
	in ProofResourceReserveInput,
	now time.Time,
) (ProofResourceReserveResult, error) {
	t.Helper()
	result, err := reserveProofResourceWithLiveBarrier(t, s, workID, in, now)
	if err != nil {
		return result, err
	}
	if result.ExecutionBarrier == nil {
		t.Fatal("successful proof reservation omitted execution barrier")
	}
	if err := closeProofFixtureExecutionBarrier(result.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	return result, nil
}

func reserveProofResourceWithLiveBarrier(
	t *testing.T,
	s *Store,
	workID string,
	in ProofResourceReserveInput,
	now time.Time,
) (ProofResourceReserveResult, error) {
	t.Helper()
	baseRef := ""
	if strings.Contains(in.Command, "--base refs/heads/main") {
		baseRef = "refs/heads/main"
	}
	profile, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, in.Workspace, baseRef, []ProofResourceGateProfile{
		{
			Name:     "synthetic-gate-a",
			Command:  []string{"go", "test", "./internal/synthetic-a"},
			ProofRef: "synthetic-proof-a",
		},
		{
			Name:     "synthetic-gate-b",
			Command:  []string{"go", "test", "./internal/synthetic-b"},
			ProofRef: "synthetic-proof-b",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	in.Profile = profile
	return s.ReserveProofResource(workID, in, now)
}

func assertGovernedProofReservation(t *testing.T, s *Store, reservation ProofResourceReservation) {
	t.Helper()
	var action envelope.ActionIntent
	if err := s.readEventJSON(reservation.ActionIntentID, &action); err != nil {
		t.Fatalf("read reservation action: %v", err)
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(reservation.PolicyDecisionID, &decision); err != nil {
		t.Fatalf("read reservation policy: %v", err)
	}
	if action.IDs.WorkID != reservation.WorkID || decision.ActionIntentID != action.ID || !policy.Allowed(decision) {
		t.Fatalf("reservation governance chain invalid: action=%+v decision=%+v", action, decision)
	}
}

func validOwnedProofReportJSON(status string) []byte {
	firstStatus := "ok"
	if status == "failed" {
		firstStatus = "failed"
	}
	report := canonicalOwnedProofReport()
	report["base_ref"] = ""
	report["status"] = status
	report["generated_at"] = fixedNow.Add(3 * time.Second).Format(time.RFC3339)
	steps := report["steps"].([]map[string]any)
	steps[0]["status"] = firstStatus
	if status == "failed" {
		report["proof_refs"] = []string{"synthetic-proof-b"}
	}
	body, err := json.Marshal(report)
	if err != nil {
		panic(err)
	}
	return body
}

func allFailedOwnedProofReportJSONWithNullRefs() []byte {
	report := canonicalOwnedProofReport()
	report["base_ref"] = ""
	report["status"] = "failed"
	report["generated_at"] = fixedNow.Add(3 * time.Second).Format(time.RFC3339)
	steps := report["steps"].([]map[string]any)
	for _, step := range steps {
		step["status"] = "failed"
	}
	report["proof_refs"] = nil
	body, err := json.Marshal(report)
	if err != nil {
		panic(err)
	}
	return body
}

func canonicalOwnedProofReport() map[string]any {
	return map[string]any{
		"schema_version": "compound.proof_check_report.v0.2",
		"profile_kind":   ProofResourceProfileCanonicalFull,
		"status":         "ok",
		"generated_at":   fixedNow.Add(3 * time.Second).Format(time.RFC3339),
		"repo":           "/synthetic/repo-alpha",
		"base_ref":       "refs/heads/main",
		"dry_run":        false,
		"proof_refs":     []string{"synthetic-proof-a", "synthetic-proof-b"},
		"steps": []map[string]any{
			{
				"name":      "synthetic-gate-a",
				"command":   []string{"go", "test", "./internal/synthetic-a"},
				"proof_ref": "synthetic-proof-a",
				"status":    "ok",
			},
			{
				"name":      "synthetic-gate-b",
				"command":   []string{"go", "test", "./internal/synthetic-b"},
				"proof_ref": "synthetic-proof-b",
				"status":    "ok",
			},
		},
	}
}

func assertProofRunAttempt(t *testing.T, s *Store, reservation ProofResourceReservation, wantStatus string) {
	t.Helper()
	if reservation.RunAttemptID == "" {
		t.Fatal("proof reservation omitted run_attempt_id")
	}
	ra, err := s.runAttempts().Get(reservation.RunAttemptID)
	if err != nil {
		t.Fatalf("read proof RunAttempt: %v", err)
	}
	if ra.WorkID != reservation.WorkID ||
		ra.Actor != reservation.Actor ||
		ra.ActionIntentID != reservation.ActionIntentID ||
		ra.Status != wantStatus {
		t.Fatalf("proof RunAttempt = %+v, want work=%s actor=%s action=%s status=%s", ra, reservation.WorkID, reservation.Actor, reservation.ActionIntentID, wantStatus)
	}
	w, err := s.Get(reservation.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(w.RunAttemptIDs, ra.ID) {
		t.Fatalf("work run_attempt_ids = %v, want %s", w.RunAttemptIDs, ra.ID)
	}
}

func assertLifecycleAttachment(t *testing.T, s *Store, workID, ref string) {
	t.Helper()
	events, err := s.eventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		for _, attached := range event.Attached {
			if attached == ref {
				return
			}
		}
	}
	t.Fatalf("work %s lifecycle does not attach %s", workID, ref)
}
