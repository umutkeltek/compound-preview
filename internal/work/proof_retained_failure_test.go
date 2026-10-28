package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
)

// retainedFailedProofFixture leaves an authentic owned failed completion journal behind: the
// producer already produced the failed report and the commit was interrupted before the
// terminal result became durable.
func retainedFailedProofFixture(t *testing.T) (privateSourceFixture, ProofResourceReserveResult, ProofResourceCompleteInput, string, time.Time) {
	t.Helper()
	f, acquired, input, at := privateFailedCompletionFixture(t)
	original := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(s *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("injected failed-result persistence fault")
		}
		return original(s, stage, id, value)
	}
	t.Cleanup(func() { proofResourceWriteEventRecord = original })
	if _, err := f.store.CompleteProofResource(f.workID, input, at.Add(time.Second)); err == nil {
		t.Fatal("fixture must retain a genuine failed completion before commit")
	}
	proofResourceWriteEventRecord = original
	sum, err := f.store.proofResourceCompletionPendingSHA256(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, acquired, input, sum, at
}

func advanceProofSourceRepo(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "advanced.txt"), []byte("synthetic later source commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runProofSourceGit(t, repo, "add", "advanced.txt")
	runProofSourceGit(t, repo, "commit", "-m", "synthetic later source commit")
}

// retainedFailedProofLiveBarrierFixture leaves an authentic owned failed completion journal
// behind AND keeps the NATIVE execution barrier pending, exactly as a live producer would.
//
// The barrier is a native fact, not an operator claim: the fixture keeps the exact inherited
// barrier descriptors locked, so the native quiescence probe observes an active execution. The
// descriptors are the ones the reservation installed — the fixture never re-implements the
// barrier, it only holds what the native owner created.
func retainedFailedProofLiveBarrierFixture(t *testing.T) (privateSourceFixture, ProofResourceReserveResult, ProofResourceCompleteInput, string, time.Time) {
	t.Helper()
	f, profile := privateNativeConsumerProfile(t)
	at := time.Now().UTC()
	acquired, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{
		Actor: proofActorAlpha, TTL: 20 * time.Minute, Workspace: f.repo,
		Command: "compound proof-check --out /synthetic/failed-proof.json", Profile: profile,
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	// The inherited barrier descriptors are captured before the reservation releases them: the
	// retained completion must be produced while the execution is quiescent, and only then is
	// the execution made pending again for the falsifier.
	barrierPaths := make([]string, 0, 4)
	for _, file := range acquired.ExecutionBarrier.InheritableFiles() {
		barrierPaths = append(barrierPaths, file.Name())
	}
	if len(barrierPaths) == 0 {
		t.Fatal("the retained proof reservation installed no execution barrier")
	}
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	steps := make([]map[string]any, 0, len(profile.Gates))
	for _, gate := range profile.Gates {
		steps = append(steps, map[string]any{"name": gate.Name, "command": gate.Command, "dir": gate.Dir, "skip_if_missing": gate.SkipIfMissing, "proof_ref": gate.ProofRef, "status": "failed"})
	}
	report := map[string]any{
		"schema_version": "compound.proof_check_report.v0.5", "profile_kind": ProofResourceProfileCanonicalFull,
		"status": "failed", "generated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"repo": profile.Repo, "base_ref": profile.BaseRef, "source_snapshot": profile.SourceSnapshot,
		"dry_run": false, "proof_refs": []string{}, "steps": steps,
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: ref,
		ProducerExitKnown: true,
		ProducerExitCode:  1,
	}
	original := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(s *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("injected failed-result persistence fault")
		}
		return original(s, stage, id, value)
	}
	t.Cleanup(func() { proofResourceWriteEventRecord = original })
	if _, err := f.store.CompleteProofResource(f.workID, input, at.Add(time.Second)); err == nil {
		t.Fatal("fixture must retain a genuine failed completion before commit")
	}
	proofResourceWriteEventRecord = original
	sum, err := f.store.proofResourceCompletionPendingSHA256(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The execution is live again: the exact inherited barrier descriptors are held, and the
	// NATIVE probe proves the execution is pending before the falsifier relies on it.
	for _, path := range barrierPaths {
		file, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			t.Fatalf("open the native execution barrier %s: %v", path, err)
		}
		t.Cleanup(func() { _ = file.Close() })
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("hold the native execution barrier %s: %v", path, err)
		}
	}
	resource := landlease.ProofResourceForIdentity(f.store.home, acquired.Reservation.ProofCoordination)
	pending, err := resource.ExecutionPending(acquired.Reservation.Holder)
	if err != nil {
		t.Fatalf("native execution quiescence probe: %v", err)
	}
	if !pending {
		t.Fatal("the fixture did not leave a pending native execution")
	}
	return f, acquired, input, sum, at
}

// retainedFailedProofRetryAncestorFixture leaves an authentic retained failed completion whose
// current attempt is a RETRY that extends the reserved original attempt: the interrupted native
// recovery already minted and journaled the retry before its commit was interrupted.
func retainedFailedProofRetryAncestorFixture(t *testing.T) (privateSourceFixture, ProofResourceReserveResult, string, time.Time, string) {
	t.Helper()
	f, acquired, input, _, at := retainedFailedProofFixture(t)
	// The second completion mints and journals the retry, then the completion record write
	// fails: the retained journal now points at the minted retry.
	original := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(s *Store, stage, id string, value any) error {
		if stage == "completion" {
			return errors.New("injected completion-record persistence fault")
		}
		return original(s, stage, id, value)
	}
	t.Cleanup(func() { proofResourceWriteEventRecord = original })
	if _, err := f.store.CompleteProofResource(f.workID, input, at.Add(2*time.Second)); err == nil {
		t.Fatal("fixture must interrupt the completion after the retry was journaled")
	}
	proofResourceWriteEventRecord = original
	pending, present, _, err := f.store.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("the interrupted completion did not retain the completion journal")
	}
	retryID := pending.Result.RunAttemptID
	if retryID == acquired.Reservation.RunAttemptID {
		t.Fatal("the fixture did not extend the retained completion with a retry attempt")
	}
	sum, err := f.store.proofResourceCompletionPendingSHA256(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, acquired, sum, at, retryID
}

// retainedFailedProofInterruptedRetryFixture leaves an authentic retained failed completion whose
// current attempt is a RETRY whose forward link into the Work envelope was never persisted: the
// interrupted native recovery minted and journaled the retry and then died before its Work
// reachability was written.
//
// The missing forward link is the ONE legitimate reachability exception of an authenticated retry
// ancestry, and the existing native reachability recovery owns its repair.
func retainedFailedProofInterruptedRetryFixture(t *testing.T) (privateSourceFixture, ProofResourceReserveResult, string, time.Time, string) {
	t.Helper()
	f, acquired, input, _, at := retainedFailedProofFixture(t)
	original := proofResourceAfterCompletionRetryJournal
	crashed := false
	proofResourceAfterCompletionRetryJournal = func(runattempt.RunAttempt, proofResourceCompletionPending) {
		crashed = true
		panic("synthetic crash after the retry journal write and before its Work reachability")
	}
	t.Cleanup(func() { proofResourceAfterCompletionRetryJournal = original })
	func() {
		defer func() { _ = recover() }()
		_, _ = f.store.CompleteProofResource(f.workID, input, at.Add(2*time.Second))
	}()
	proofResourceAfterCompletionRetryJournal = original
	if !crashed {
		t.Fatal("the fixture did not reach the crash after the retry journal write")
	}
	pending, present, _, err := f.store.readProofResourceCompletionPending(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("the interrupted completion did not retain the completion journal")
	}
	retryID := pending.Result.RunAttemptID
	if retryID == acquired.Reservation.RunAttemptID {
		t.Fatal("the fixture did not extend the retained completion with a retry attempt")
	}
	w, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if oneOf(retryID, w.RunAttemptIDs) {
		t.Fatal("the fixture did not leave the retry forward link unrepaired")
	}
	sum, err := f.store.proofResourceCompletionPendingSHA256(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, acquired, sum, at, retryID
}

// staleRetainedProofHolder moves the exclusive proof resource to a foreign holder by rewriting the
// durable land-lease record of the coordinated resource.
//
// The fixture proves the move is REAL: the NATIVE status of the coordinated resource then names
// the foreign holder, so the route observes a stale holder rather than a claim about one.
func staleRetainedProofHolder(t *testing.T, f privateSourceFixture, reservation ProofResourceReservation) {
	t.Helper()
	layout, err := landlease.LayoutStatus(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	roots := []string{f.store.home}
	if layout.Mode == "scoped" && layout.BackingRoot != "" {
		roots = append(roots, layout.BackingRoot)
	}
	found := ""
	for _, root := range roots {
		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || entry.Name() != "land-lease.json" {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			var probe struct {
				Holder string `json:"holder"`
			}
			if json.Unmarshal(body, &probe) != nil {
				return nil
			}
			if probe.Holder == reservation.Holder {
				found = path
			}
			return nil
		})
		if walkErr != nil {
			t.Fatal(walkErr)
		}
		if found != "" {
			break
		}
	}
	if found == "" {
		t.Fatalf("the fixture home has no durable lease record for holder %q", reservation.Holder)
	}
	body, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	var lease map[string]any
	if err := json.Unmarshal(body, &lease); err != nil {
		t.Fatal(err)
	}
	foreign := proofResourceHolder(reservation.ID, reservation.RunAttemptID, reservation.WorkID, "codex:foreign-holder")
	lease["holder"] = foreign
	forged, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(found, forged, 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := landlease.ProofResourceForIdentity(f.store.home, reservation.ProofCoordination).Status(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if status.Holder != foreign {
		t.Fatalf("the durable lease record did not move to the foreign holder: %+v", status)
	}
}

// corruptRetainedProofAncestor tampers with the immutable audit record of the reserved original
// attempt, so the retry ancestry no longer matches the reservation it was minted under.
func corruptRetainedProofAncestor(t *testing.T, attempts *runattempt.Store, attemptID string) {
	t.Helper()
	path := filepath.Join(attempts.Home(), "run_attempts", attemptID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["command"] = "forged ancestor command"
	forged, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, forged, 0o600); err != nil {
		t.Fatal(err)
	}
}

// transferRetainedProofWorkLease changes the CURRENT Work lease owner to a foreign actor while
// keeping the claim authority CONSISTENT: the lease, the Work envelope and the durable claim
// event all name the new owner.
//
// Only the owner changed. The retained proof, its reservation, its attempt and its completion
// journal are untouched, so the recovery can still authenticate everything else about the
// retained failure: the refusal must come from the lease owner alone.
func transferRetainedProofWorkLease(t *testing.T, f privateSourceFixture, foreign string) {
	t.Helper()
	logPath := filepath.Join(f.store.home, "events", "work.jsonl")
	lease, err := f.store.readLease(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	changed := 0
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("parse the durable work event %d: %v", index, err)
		}
		// The lease binds exactly one durable claim event through its Work hash.
		if event.WorkID != f.workID || event.To != "claimed" || event.WorkSHA256 != lease.WorkSHA256 {
			continue
		}
		event.By = foreign
		forged, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines[index] = string(forged)
		changed++
	}
	if changed != 1 {
		t.Fatalf("the retained work fixture has %d durable claim events, want exactly one", changed)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	w.Authority.Owner = foreign
	if err := f.store.writeJSON(f.store.workPath(f.workID), &w); err != nil {
		t.Fatal(err)
	}
	lease.By = foreign
	if err := f.store.writeJSON(f.store.leasePath(f.workID), &lease); err != nil {
		t.Fatal(err)
	}
}

// assertRetainedFailedRecoveryRefusedWithoutEffects asserts that a refused retained recovery left
// NO owned-report output, NO mutated journal bytes, NO minted or terminalized attempt and NO
// lifecycle effect behind.
func assertRetainedFailedRecoveryRefusedWithoutEffects(
	t *testing.T,
	f privateSourceFixture,
	acquired ProofResourceReserveResult,
	sum string,
	restored int,
	attemptsBefore []runattempt.RunAttempt,
	eventsBefore []Event,
) {
	t.Helper()
	if restored != 0 {
		t.Fatalf("a refused retained recovery reached the owned report barrier: %d calls", restored)
	}
	sumNow, err := f.store.proofResourceCompletionPendingSHA256(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sumNow != sum {
		t.Fatal("a refused retained recovery mutated the retained completion journal")
	}
	attemptsAfter, err := f.store.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	if len(attemptsAfter) != len(attemptsBefore) {
		t.Fatalf("a refused retained recovery changed the attempt ledger: %d -> %d", len(attemptsBefore), len(attemptsAfter))
	}
	for _, before := range attemptsBefore {
		after, err := f.store.runAttempts().Get(before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != before.Status || !after.CompletedAt.Equal(before.CompletedAt) {
			t.Fatalf("a refused retained recovery terminalized attempt %s: %+v -> %+v", before.ID, before, after)
		}
	}
	eventsAfter, err := f.store.eventsFor(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("a refused retained recovery wrote lifecycle events: %d -> %d", len(eventsBefore), len(eventsAfter))
	}
}

func assertRetainedFailedRecovery(t *testing.T, f privateSourceFixture, acquired ProofResourceReserveResult, outcome RetainedFailedProofOutcome) {
	t.Helper()
	if outcome.ReportStatus != "failed" || outcome.VerificationRef != "" || !outcome.ResourceReleased {
		t.Fatalf("retained failed proof recovery must stay a non-certifying audit completion: %+v", outcome)
	}
	if outcome.ReportArtifactRef == "" {
		t.Fatal("retained failed proof recovery lost the original owned report")
	}
	w, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.VerificationIDs) != 0 {
		t.Fatalf("retained failed proof recovery published a verification: %+v", w.VerificationIDs)
	}
	if w.State == "completed" {
		t.Fatal("retained failed report completed the source Work")
	}
	original, err := f.store.runAttempts().Get(acquired.Reservation.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !runattempt.IsTerminal(original.Status) || original.Status != runattempt.StatusFailed {
		t.Fatalf("the original failed attempt was not preserved: %+v", original)
	}
}

func TestRetainedFailedProofRecoveryFinishesMovedCheckout(t *testing.T) {
	f, acquired, input, sum, at := retainedFailedProofFixture(t)
	// The source checkout advanced after the retained proof was produced.
	advanceProofSourceRepo(t, f.repo)
	restored := 0
	outcome, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error {
			restored++
			if result.ReportArtifactRef != input.ReportArtifactRef {
				t.Fatal("recovery changed the original owned report")
			}
			return nil
		},
	}, at.Add(6*time.Minute))
	if err != nil || !found {
		t.Fatalf("retained failed proof recovery was blocked after the source checkout moved: found=%v err=%v", found, err)
	}
	if restored != 1 {
		t.Fatalf("owned report publication barrier calls=%d", restored)
	}
	assertRetainedFailedRecovery(t, f, acquired, outcome)
	artifacts, err := home.NewArtifactStore(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	body, err := artifacts.Get(input.ReportArtifactRef)
	if err != nil {
		t.Fatalf("the original owned report artifact was not preserved: %v", err)
	}
	var report ProofCheckReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "failed" {
		t.Fatalf("the preserved report is no longer the failed battery verdict: %q", report.Status)
	}
}

func TestRetainedFailedProofRecoveryRearmsExpiredLeaseWithMissingCheckout(t *testing.T) {
	f, acquired, _, sum, at := retainedFailedProofFixture(t)
	// The original source checkout disappeared entirely.
	if err := os.RemoveAll(f.repo); err != nil {
		t.Fatal(err)
	}
	outcome, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error { return nil },
	}, at.Add(21*time.Minute))
	if err != nil || !found {
		t.Fatalf("retained failed proof recovery did not rearm the expired holder: found=%v err=%v", found, err)
	}
	assertRetainedFailedRecovery(t, f, acquired, outcome)
}

// The combined realistic recovery: an expired source review, an expired reservation lease, a
// terminal attempt whose retry lineage the recovery itself must extend, and a source checkout
// that advanced after the retained proof was produced.
func TestRetainedFailedProofRecoveryCombinesExpiredLeaseExpiredReviewTerminalRetryAndAdvancedCheckout(t *testing.T) {
	f, acquired, input, sum, at := retainedFailedProofFixture(t)
	// The source checkout advanced after the retained proof was produced.
	advanceProofSourceRepo(t, f.repo)
	later := at.Add(21 * time.Minute)
	if err := f.store.validatePrivateSourceProfileAuthority(f.workID, proofActorAlpha, acquired.Reservation.Profile, later); err == nil {
		t.Fatal("the fixture must carry a genuinely expired source review")
	}
	restored := 0
	outcome, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error {
			restored++
			if result.ReportArtifactRef != input.ReportArtifactRef {
				t.Fatal("recovery changed the original owned report")
			}
			return nil
		},
	}, later)
	if err != nil || !found {
		t.Fatalf("the combined retained recovery was blocked: found=%v err=%v", found, err)
	}
	if restored != 1 {
		t.Fatalf("owned report publication barrier calls=%d", restored)
	}
	assertRetainedFailedRecovery(t, f, acquired, outcome)
	// The original terminal attempt is preserved and the retry lineage extends it: the
	// recovery never replaces an original attempt.
	original, err := f.store.runAttempts().Get(acquired.Reservation.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !runattempt.IsTerminal(original.Status) || original.Status != runattempt.StatusFailed {
		t.Fatalf("the original terminal attempt was not preserved: %+v", original)
	}
	attempts, err := f.store.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, attempt := range attempts {
		if attempt.PredecessorID == original.ID {
			retries++
		}
	}
	if retries == 0 {
		t.Fatalf("the retained recovery did not extend the terminal retry lineage: %+v", attempts)
	}
}

func TestRetainedFailedProofRecoveryRefusesMismatchedExpectations(t *testing.T) {
	f, acquired, _, sum, at := retainedFailedProofFixture(t)
	cases := []struct {
		name   string
		workID string
		in     ProofResourceRetainedFailureInput
	}{
		{"wrong_actor", f.workID, ProofResourceRetainedFailureInput{Actor: "codex:someone-else", ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum}},
		{"wrong_work", "work_other", ProofResourceRetainedFailureInput{Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum}},
		{"wrong_reservation", f.workID, ProofResourceRetainedFailureInput{Actor: proofActorAlpha, ReservationID: "pfr_not_this_one", ExpectedPendingSHA256: sum}},
		{"wrong_pending_sha", f.workID, ProofResourceRetainedFailureInput{Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: strings.Repeat("a", 64)}},
		{"malformed_pending_sha", f.workID, ProofResourceRetainedFailureInput{Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: "not-a-digest"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			before, err := f.store.runAttempts().List()
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := f.store.ResumeRetainedFailedProofCompletion(testCase.workID, testCase.in, at.Add(6*time.Minute)); err == nil || found {
				t.Fatalf("mismatched retained recovery was accepted: found=%v err=%v", found, err)
			}
			after, err := f.store.runAttempts().List()
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("a refused retained recovery minted an attempt: %d -> %d", len(before), len(after))
			}
			sumNow, err := f.store.proofResourceCompletionPendingSHA256(acquired.Reservation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if sumNow != sum {
				t.Fatal("a refused retained recovery changed the retained completion journal")
			}
		})
	}
}

func TestRetainedFailedProofRecoveryFencesPendingBytesInsideTheNativeGuard(t *testing.T) {
	f, acquired, _, sum, at := retainedFailedProofFixture(t)
	path, err := f.store.proofResourceCompletionPendingPath(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	tamper := func() {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var decoded map[string]any
		if unmarshalErr := json.Unmarshal(body, &decoded); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		decoded["injected_field"] = "forged"
		forged, marshalErr := json.Marshal(decoded)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := os.WriteFile(path, forged, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	attemptsBefore, err := f.store.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := f.store.eventsFor(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	// The journal changes between the initial read and the protected mutation. The fence lives
	// inside the guard that performs the recovery effects, so the changed bytes refuse there.
	original := proofResourceBeforeRetainedMutation
	proofResourceBeforeRetainedMutation = func(reservationID string) { tamper() }
	t.Cleanup(func() { proofResourceBeforeRetainedMutation = original })
	_, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error {
			t.Fatal("a fenced retained recovery reached the owned report barrier")
			return nil
		},
	}, at.Add(6*time.Minute))
	if err == nil {
		t.Fatalf("changed retained completion bytes were accepted: found=%v err=%v", found, err)
	}
	if !strings.Contains(err.Error(), "expected pending sha256") {
		t.Fatalf("the changed pending bytes were not refused by the expected-hash fence: %v", err)
	}
	// A fenced refusal has ZERO callback, output and native effects.
	attemptsAfter, err := f.store.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	if len(attemptsAfter) != len(attemptsBefore) {
		t.Fatalf("a fenced retained recovery mutated the attempt ledger: %d -> %d", len(attemptsBefore), len(attemptsAfter))
	}
	eventsAfter, err := f.store.eventsFor(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("a fenced retained recovery wrote lifecycle events: %d -> %d", len(eventsBefore), len(eventsAfter))
	}
}

// The dedicated failure route requires an EXISTING, valid completion governance seal before any
// effect. The refusal must be the SEAL refusal, not the expected-hash fence: the operator's
// expected bytes are re-derived from the mutated journal, so the fence admits them and the seal
// rule is the only thing left that can refuse.
func TestRetainedFailedProofRecoveryRefusesMissingOrMalformedSeal(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"missing_seal":   func(body map[string]any) { delete(body, "governance_seal") },
		"malformed_seal": func(body map[string]any) { body["governance_seal"] = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			f, acquired, _, _, at := retainedFailedProofFixture(t)
			path, err := f.store.proofResourceCompletionPendingPath(acquired.Reservation.ID)
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			mutate(decoded)
			forged, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, forged, 0o600); err != nil {
				t.Fatal(err)
			}
			// The operator expects the CURRENT journal bytes: the hash fence must admit them,
			// so the seal rule is the only remaining refusal.
			mutatedSum, err := f.store.proofResourceCompletionPendingSHA256(acquired.Reservation.ID)
			if err != nil {
				t.Fatal(err)
			}
			attemptsBefore, err := f.store.runAttempts().List()
			if err != nil {
				t.Fatal(err)
			}
			eventsBefore, err := f.store.eventsFor(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			restored := 0
			_, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
				Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: mutatedSum,
				PrepareOwnedReport: func(result ProofResourceResult) error {
					restored++
					return nil
				},
			}, at.Add(6*time.Minute))
			if err == nil {
				t.Fatalf("the dedicated failure route accepted a missing or malformed governance seal: found=%v", found)
			}
			if !strings.Contains(err.Error(), "governance seal") {
				t.Fatalf("the refusal was not the governance seal refusal: %v", err)
			}
			if restored != 0 {
				t.Fatal("a refused seal reached the owned report barrier")
			}
			attemptsAfter, err := f.store.runAttempts().List()
			if err != nil {
				t.Fatal(err)
			}
			if len(attemptsAfter) != len(attemptsBefore) {
				t.Fatalf("a refused seal mutated the attempt ledger: %d -> %d", len(attemptsBefore), len(attemptsAfter))
			}
			eventsAfter, err := f.store.eventsFor(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			if len(eventsAfter) != len(eventsBefore) {
				t.Fatalf("a refused seal wrote lifecycle events: %d -> %d", len(eventsBefore), len(eventsAfter))
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, forged) {
				t.Fatal("a refused seal rewrote the retained completion journal")
			}
		})
	}
}

func TestRetainedFailedProofRecoveryRefusesForgedOwnedReport(t *testing.T) {
	f, acquired, input, sum, at := retainedFailedProofFixture(t)
	hexDigest, err := home.ArtifactHex(input.ReportArtifactRef)
	if err != nil {
		t.Fatal(err)
	}
	forged := filepath.Join(f.store.home, "artifacts", "sha256", hexDigest)
	if err := os.WriteFile(forged, []byte(`{"schema_version":"compound.proof_check_report.v0.6","status":"ok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error { return nil },
	}, at.Add(6*time.Minute)); err == nil || found {
		t.Fatalf("a forged owned report was accepted: found=%v err=%v", found, err)
	}
}

func TestRetainedFailedProofRecoveryRefusesNonFailedResults(t *testing.T) {
	// The dedicated failure route accepts only an owned, exact failed result. The ordinary
	// successful-proof authority is untouched: a certifying result keeps its own route.
	reservation := ProofResourceReservation{ID: "pfr_synthetic", WorkID: "work_synthetic", Actor: proofActorAlpha, Status: ProofResourceAcquired}
	for name, result := range map[string]ProofResourceResult{
		"verified":     {WorkID: "work_synthetic", Actor: proofActorAlpha, ReservationID: "pfr_synthetic", Status: ProofResourceVerified, RunAttemptStatus: runattempt.StatusSucceeded, OwnedReport: true, ReportArtifactRef: "artifact:sha256:synthetic", ReportStatus: "ok", ProducerExitKnown: true, ProducerExitCode: 0},
		"unknown":      {WorkID: "work_synthetic", Actor: proofActorAlpha, ReservationID: "pfr_synthetic", Status: ProofResourceFailed, RunAttemptStatus: runattempt.StatusFailed, OwnedReport: true, ReportArtifactRef: "artifact:sha256:synthetic", ReportStatus: "failed"},
		"zero_exit":    {WorkID: "work_synthetic", Actor: proofActorAlpha, ReservationID: "pfr_synthetic", Status: ProofResourceFailed, RunAttemptStatus: runattempt.StatusFailed, OwnedReport: true, ReportArtifactRef: "artifact:sha256:synthetic", ReportStatus: "failed", ProducerExitKnown: true, ProducerExitCode: 0},
		"unowned":      {WorkID: "work_synthetic", Actor: proofActorAlpha, ReservationID: "pfr_synthetic", Status: ProofResourceFailed, RunAttemptStatus: runattempt.StatusFailed, ReportStatus: "failed", ProducerExitKnown: true, ProducerExitCode: 1},
		"no_artifact":  {WorkID: "work_synthetic", Actor: proofActorAlpha, ReservationID: "pfr_synthetic", Status: ProofResourceFailed, RunAttemptStatus: runattempt.StatusFailed, OwnedReport: true, ReportStatus: "failed", ProducerExitKnown: true, ProducerExitCode: 1},
		"verified_ref": {WorkID: "work_synthetic", Actor: proofActorAlpha, ReservationID: "pfr_synthetic", Status: ProofResourceFailed, RunAttemptStatus: runattempt.StatusFailed, OwnedReport: true, ReportArtifactRef: "artifact:sha256:synthetic", ReportStatus: "failed", ProducerExitKnown: true, ProducerExitCode: 1, VerificationRef: "proof-check:synthetic"},
	} {
		t.Run(name, func(t *testing.T) {
			pending := proofResourceCompletionPending{Result: result, TerminalStatus: runattempt.StatusFailed}
			if err := authenticateRetainedFailedProofResult("work_synthetic", proofActorAlpha, reservation, pending); err == nil {
				t.Fatal("the dedicated failure route accepted a result that is not an owned failure")
			}
		})
	}
	failed := ProofResourceResult{WorkID: "work_synthetic", Actor: proofActorAlpha, ReservationID: "pfr_synthetic", Status: ProofResourceFailed, RunAttemptStatus: runattempt.StatusFailed, OwnedReport: true, ReportArtifactRef: "artifact:sha256:synthetic", ReportStatus: "failed", ProducerExitKnown: true, ProducerExitCode: 1}
	if err := authenticateRetainedFailedProofResult("work_synthetic", proofActorAlpha, reservation, proofResourceCompletionPending{Result: failed, TerminalStatus: runattempt.StatusFailed}); err != nil {
		t.Fatalf("an owned failed result was refused: %v", err)
	}
}

func TestRetainedFailedProofRecoveryRepetitionRefusesWithoutStateDelta(t *testing.T) {
	f, acquired, _, sum, at := retainedFailedProofFixture(t)
	outcome, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error { return nil },
	}, at.Add(6*time.Minute))
	if err != nil || !found {
		t.Fatalf("the first retained recovery failed: found=%v err=%v", found, err)
	}
	if outcome.ReportArtifactRef == "" {
		t.Fatal("the first retained recovery lost the original owned report")
	}
	attempts, err := f.store.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.store.eventsFor(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	// The consumed journal is gone. A repetition must refuse clearly: it must not require the
	// consumed pending file, must not recreate consumed evidence, and must not admit a battery.
	repeat, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error {
			t.Fatal("a repeated retained recovery restored consumed evidence")
			return nil
		},
	}, at.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("a repeated retained recovery errored: %v", err)
	}
	if found {
		t.Fatalf("a repeated retained recovery claimed a retained completion: %+v", repeat)
	}
	if repeat.ReportArtifactRef != "" || repeat.VerificationRef != "" || repeat.ResourceReleased {
		t.Fatalf("a refused repetition reported a completion: %+v", repeat)
	}
	after, err := f.store.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(attempts) {
		t.Fatalf("a refused repetition invented an attempt: %d -> %d", len(attempts), len(after))
	}
	afterEvents, err := f.store.eventsFor(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(events) {
		t.Fatalf("a refused repetition wrote lifecycle events: %d -> %d", len(events), len(afterEvents))
	}
}

func retainedProofCLIRestorer(t *testing.T, restored *int) func(string, string, string) (ProofCheckReport, error) {
	t.Helper()
	return func(homePath, artifactRef, outPath string) (ProofCheckReport, error) {
		*restored++
		artifacts, err := home.NewArtifactStore(homePath)
		if err != nil {
			return ProofCheckReport{}, err
		}
		body, err := artifacts.Get(artifactRef)
		if err != nil {
			return ProofCheckReport{}, err
		}
		if err := os.WriteFile(outPath, body, 0o600); err != nil {
			return ProofCheckReport{}, err
		}
		return readRetainedProofCheckReport(homePath, artifactRef)
	}
}

func TestRetainedFailedProofRecoveryCLIKeepsFailedVerdictAndNeverAdmitsBattery(t *testing.T) {
	f, acquired, _, sum, _ := retainedFailedProofFixture(t)
	outPath := filepath.Join(t.TempDir(), "failed-proof.json")
	restored := 0
	var stdout bytes.Buffer
	code := RunRetainedFailedProofRecoveryCLI([]string{
		"--resume-retained", acquired.Reservation.ID,
		"--work", f.workID,
		"--actor", proofActorAlpha,
		"--expected-pending-sha256", sum,
		"--out", outPath,
		"--home", f.store.home,
	}, &stdout, io.Discard, retainedProofCLIRestorer(t, &restored))
	if code != 1 {
		t.Fatalf("retained failed proof recovery must keep the failed battery exit code: %d (%s)", code, stdout.String())
	}
	if restored != 1 {
		t.Fatalf("the owned report was not restored through the output custody: %d", restored)
	}
	verdict := stdout.String()
	if !strings.Contains(verdict, "battery: FAILED") || !strings.Contains(verdict, "retained-failed-proof=recovered") {
		t.Fatalf("the retained recovery verdict is not a truthful failed audit completion: %q", verdict)
	}
	if strings.Contains(verdict, "battery: ok") {
		t.Fatalf("the retained recovery read as a passed proof: %q", verdict)
	}
	published, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var report ProofCheckReport
	if err := json.Unmarshal(published, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "failed" {
		t.Fatalf("the restored report is not the original failed verdict: %q", report.Status)
	}
}

func TestRetainedFailedProofRecoveryCLIRefusesNoMatchWithoutWritingOrAdmitting(t *testing.T) {
	f, acquired, _, sum, _ := retainedFailedProofFixture(t)
	// The retained journal is gone and no completion was ever committed: there is nothing to
	// finish, so the route must refuse instead of admitting a battery.
	path, err := f.store.proofResourceCompletionPendingPath(acquired.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "never-written.json")
	restored := 0
	var stderr bytes.Buffer
	code := RunRetainedFailedProofRecoveryCLI([]string{
		"--resume-retained", acquired.Reservation.ID,
		"--work", f.workID,
		"--actor", proofActorAlpha,
		"--expected-pending-sha256", sum,
		"--out", outPath,
		"--home", f.store.home,
	}, io.Discard, &stderr, retainedProofCLIRestorer(t, &restored))
	if code != 2 {
		t.Fatalf("a retained recovery without a matching completion must refuse: %d", code)
	}
	if restored != 0 {
		t.Fatal("a refused retained recovery restored a report")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatal("a refused retained recovery wrote the --out path")
	}
	if !strings.Contains(stderr.String(), "no exact retained failed proof completion") {
		t.Fatalf("the refusal did not name the missing retained completion: %q", stderr.String())
	}
}

func TestRetainedFailedProofRecoveryCLIRequiresExactInputs(t *testing.T) {
	for name, args := range map[string][]string{
		"missing_reservation": {"--work", "work_x", "--actor", "codex:proof-alpha", "--expected-pending-sha256", strings.Repeat("a", 64), "--out", "/tmp/x.json"},
		"missing_work":        {"--resume-retained", "pfr_x", "--actor", "codex:proof-alpha", "--expected-pending-sha256", strings.Repeat("a", 64), "--out", "/tmp/x.json"},
		"missing_actor":       {"--resume-retained", "pfr_x", "--work", "work_x", "--expected-pending-sha256", strings.Repeat("a", 64), "--out", "/tmp/x.json"},
		"missing_sha":         {"--resume-retained", "pfr_x", "--work", "work_x", "--actor", "codex:proof-alpha", "--out", "/tmp/x.json"},
		"missing_out":         {"--resume-retained", "pfr_x", "--work", "work_x", "--actor", "codex:proof-alpha", "--expected-pending-sha256", strings.Repeat("a", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if code := RunRetainedFailedProofRecoveryCLI(args, io.Discard, io.Discard, nil); code != 2 {
				t.Fatalf("incomplete retained recovery inputs must refuse: %d", code)
			}
		})
	}
}

// retainedFailedProofRecoveryWindows are the two native holder windows of the retained route: the
// LIVE holder (the 20 minute lease is still held) and the EXPIRED holder (the lease expired and
// the route must rearm it before it may publish anything).
//
// Every refusal fixture runs against BOTH windows, because the expired window is the one where an
// effect could otherwise precede the refusal.
var retainedFailedProofRecoveryWindows = []struct {
	name string
	at   time.Duration
}{
	{"live_holder", 6 * time.Minute},
	{"expired_holder", 21 * time.Minute},
}

// The native execution barrier is a native fact, not an operator claim. While the inherited
// execution descriptor is still held, the dedicated route must refuse BEFORE the owned report
// is restored: the refusal has no callback, no output, no mutated journal bytes, no minted or
// terminalized attempt and no lifecycle effect.
//
// The falsifier is CAUSAL: without the pre-output quiescence probe the route reaches the owned
// report barrier and only then fails at the final ReleaseAfter, so the callback assertion and
// the effect assertions fail.
//
// The EXPIRED holder window is the load-bearing one: the quiescence probe has to refuse BEFORE
// the rearm, or the expired window writes a rearm preparation before it refuses.
func TestRetainedFailedProofRecoveryRefusesActiveNativeExecutionBeforeOutput(t *testing.T) {
	for _, window := range retainedFailedProofRecoveryWindows {
		t.Run(window.name, func(t *testing.T) {
			f, acquired, _, sum, at := retainedFailedProofLiveBarrierFixture(t)
			attemptsBefore, err := f.store.runAttempts().List()
			if err != nil {
				t.Fatal(err)
			}
			eventsBefore, err := f.store.eventsFor(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			restored := 0
			_, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
				Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
				PrepareOwnedReport: func(result ProofResourceResult) error {
					restored++
					return nil
				},
			}, at.Add(window.at))
			if err == nil {
				t.Fatalf("an active native execution was accepted by the retained route: found=%v", found)
			}
			assertRetainedFailedRecoveryRefusedWithoutEffects(t, f, acquired, sum, restored, attemptsBefore, eventsBefore)
			if !strings.Contains(err.Error(), "active or ambiguous execution") {
				t.Fatalf("the refusal was not the native quiescence refusal: %v", err)
			}
		})
	}
}

// A corrupt retry ancestor is invisible to the CURRENT-attempt validation: the current attempt
// still matches the reservation, so only the FULL ancestry authentication refuses. The refusal
// happens before the owned report is restored and leaves no native effect behind.
//
// The falsifier is CAUSAL: without the ancestry authentication the route completes the retained
// failure and publishes the owned report, so the callback assertion fails.
//
// The EXPIRED holder window is the load-bearing one: the ancestry has to be authenticated BEFORE
// the rearm, or the expired window rearms an expired holder before it refuses.
func TestRetainedFailedProofRecoveryRefusesCorruptRetryAncestorBeforeOutput(t *testing.T) {
	for _, window := range retainedFailedProofRecoveryWindows {
		t.Run(window.name, func(t *testing.T) {
			f, acquired, sum, at, retryID := retainedFailedProofRetryAncestorFixture(t)
			// The reserved original attempt is the ancestor of the retained retry. Tampering with its
			// immutable command record breaks the ancestry without touching the current attempt.
			corruptRetainedProofAncestor(t, f.store.runAttempts(), acquired.Reservation.RunAttemptID)
			attemptsBefore, err := f.store.runAttempts().List()
			if err != nil {
				t.Fatal(err)
			}
			eventsBefore, err := f.store.eventsFor(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			restored := 0
			_, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
				Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
				PrepareOwnedReport: func(result ProofResourceResult) error {
					restored++
					return nil
				},
			}, at.Add(window.at))
			if err == nil {
				t.Fatalf("a corrupt retry ancestor was accepted by the retained route: found=%v", found)
			}
			assertRetainedFailedRecoveryRefusedWithoutEffects(t, f, acquired, sum, restored, attemptsBefore, eventsBefore)
			if !strings.Contains(err.Error(), "retry ancestry is not authentic") {
				t.Fatalf("the refusal was not the retry ancestry refusal: %v", err)
			}
			retry, err := f.store.runAttempts().Get(retryID)
			if err != nil {
				t.Fatal(err)
			}
			if retry.PredecessorID != acquired.Reservation.RunAttemptID {
				t.Fatalf("the fixture retry does not descend from the reserved original attempt: %+v", retry)
			}
		})
	}
}

// The Work lease owner is a native fact: only the actor that currently owns the Work lease may
// finish a retained completion. A foreign lease owner must be refused BEFORE the owned report is
// restored, with no native effect behind.
//
// The falsifier is CAUSAL: without the pre-output lease-owner check the route reaches the owned
// report barrier and only then fails in the native completion, so the callback assertion fails.
func TestRetainedFailedProofRecoveryRefusesForeignWorkLeaseOwnerBeforeOutput(t *testing.T) {
	for _, window := range retainedFailedProofRecoveryWindows {
		t.Run(window.name, func(t *testing.T) {
			f, acquired, _, sum, at := retainedFailedProofFixture(t)
			lease, err := f.store.readLease(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			if lease.By != proofActorAlpha {
				t.Fatalf("the fixture does not hold the work lease as %q: %+v", proofActorAlpha, lease)
			}
			// The Work lease owner changes to a foreign actor and the claim authority stays consistent,
			// so everything else about the retained failure still authenticates: only the lease owner
			// refuses the recovery.
			transferRetainedProofWorkLease(t, f, "codex:foreign-owner")
			attemptsBefore, err := f.store.runAttempts().List()
			if err != nil {
				t.Fatal(err)
			}
			eventsBefore, err := f.store.eventsFor(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			restored := 0
			_, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
				Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
				PrepareOwnedReport: func(result ProofResourceResult) error {
					restored++
					return nil
				},
			}, at.Add(window.at))
			if err == nil {
				t.Fatalf("a foreign work lease owner was accepted by the retained route: found=%v", found)
			}
			assertRetainedFailedRecoveryRefusedWithoutEffects(t, f, acquired, sum, restored, attemptsBefore, eventsBefore)
			if !strings.Contains(err.Error(), "requires work lease held by") {
				t.Fatalf("the refusal was not the work lease owner refusal: %v", err)
			}
		})
	}
}

// The exclusive proof resource is a native fact: only the exact CURRENT holder may publish the
// owned report. When the resource moved to a foreign holder after the route read the current
// status, the route must refuse BEFORE the owned report is restored, with no native effect behind.
//
// The falsifier is CAUSAL: without the pre-effect holder rule the route reaches the owned report
// barrier and only then fails in the native finalizer, so the callback assertion fails.
//
// The EXPIRED holder window is the load-bearing one: the stale holder has to refuse BEFORE the
// rearm preparation, or the expired window writes governance for a lease it does not own.
func TestRetainedFailedProofRecoveryRefusesStaleProofResourceHolderBeforeOutput(t *testing.T) {
	for _, window := range retainedFailedProofRecoveryWindows {
		t.Run(window.name, func(t *testing.T) {
			f, acquired, _, sum, at := retainedFailedProofFixture(t)
			attemptsBefore, err := f.store.runAttempts().List()
			if err != nil {
				t.Fatal(err)
			}
			eventsBefore, err := f.store.eventsFor(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			// The exclusive proof resource moves to a foreign holder after the route read the
			// current status and before it takes the effects guard.
			original := proofResourceBeforeRetainedMutation
			proofResourceBeforeRetainedMutation = func(reservationID string) {
				staleRetainedProofHolder(t, f, acquired.Reservation)
			}
			t.Cleanup(func() { proofResourceBeforeRetainedMutation = original })
			restored := 0
			_, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
				Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
				PrepareOwnedReport: func(result ProofResourceResult) error {
					restored++
					return nil
				},
			}, at.Add(window.at))
			if err == nil {
				t.Fatalf("a stale proof resource holder was accepted by the retained route: found=%v", found)
			}
			assertRetainedFailedRecoveryRefusedWithoutEffects(t, f, acquired, sum, restored, attemptsBefore, eventsBefore)
			if !strings.Contains(err.Error(), "no longer holds exclusive resource") {
				t.Fatalf("the refusal was not the native proof resource holder refusal: %v", err)
			}
		})
	}
}

// An interrupted retry that was minted and journaled before its forward link into the Work
// envelope was persisted is a LEGITIMATE native recovery state, not corruption.
//
// The immutable ancestry is authenticated first, the ONE permitted missing forward link is repaired
// by the EXISTING native reachability recovery, and only then is the owned report published. The
// original terminal attempt and the retry both stay in the Work lineage.
func TestRetainedFailedProofRecoveryRepairsInterruptedRetryForwardLinkBeforeOutput(t *testing.T) {
	f, acquired, sum, at, retryID := retainedFailedProofInterruptedRetryFixture(t)
	restored := 0
	outcome, found, err := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error {
			restored++
			return nil
		},
	}, at.Add(6*time.Minute))
	if err != nil || !found {
		t.Fatalf("the interrupted retry reachability was not recovered: found=%v err=%v", found, err)
	}
	if restored != 1 {
		t.Fatalf("owned report publication barrier calls=%d", restored)
	}
	assertRetainedFailedRecovery(t, f, acquired, outcome)
	w, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if !oneOf(retryID, w.RunAttemptIDs) {
		t.Fatalf("the interrupted retry forward link was not repaired: %+v", w.RunAttemptIDs)
	}
	retry, err := f.store.runAttempts().Get(retryID)
	if err != nil {
		t.Fatal(err)
	}
	if !runattempt.IsTerminal(retry.Status) || retry.Status != runattempt.StatusFailed {
		t.Fatalf("the repaired retry attempt is not a preserved terminal failure: %+v", retry)
	}
}

// The strict pre-output holder rule is the FINAL native fence of the dedicated route: the route
// rearmed its OWN expired holder, and the exclusive resource moved to a foreign holder right
// after that rearm. The route must observe the ACTUAL current status under the effects guard and
// refuse BEFORE the owned report is published.
//
// The falsifier is CAUSAL: without the strict pre-output holder rule the route publishes the owned
// report and only then fails in the native finalizer, so the callback assertion fails.
func TestRetainedFailedProofRecoveryRefusesHolderLostAfterOwnRearmBeforeOutput(t *testing.T) {
	f, acquired, _, sum, at := retainedFailedProofFixture(t)
	attemptsBefore, err := f.store.runAttempts().List()
	if err != nil {
		t.Fatal(err)
	}
	// The exclusive proof resource moves to a foreign holder right after the route's own rearm of
	// its expired holder: the rearmed lease is a legitimate own generation, the foreign takeover
	// is not.
	original := proofResourceAfterScarceMutation
	proofResourceAfterScarceMutation = func(phase string) {
		if phase == "completion_rearm" {
			staleRetainedProofHolder(t, f, acquired.Reservation)
		}
	}
	t.Cleanup(func() { proofResourceAfterScarceMutation = original })
	restored := 0
	_, found, refusal := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ExpectedPendingSHA256: sum,
		PrepareOwnedReport: func(result ProofResourceResult) error {
			restored++
			return nil
		},
	}, at.Add(21*time.Minute))
	if refusal == nil {
		t.Fatalf("a proof resource holder lost after the own rearm was accepted by the retained route: found=%v", found)
	}
	// The rearm's own governance was legitimately written before the holder was lost, so only the
	// output, the retained journal bytes and the attempt ledger are asserted here.
	if restored != 0 {
		t.Fatalf("a refused retained recovery reached the owned report barrier: %d calls", restored)
	}
	sumNow, sumErr := f.store.proofResourceCompletionPendingSHA256(acquired.Reservation.ID)
	if sumErr != nil {
		t.Fatal(sumErr)
	}
	if sumNow != sum {
		t.Fatal("a refused retained recovery mutated the retained completion journal")
	}
	attemptsAfter, listErr := f.store.runAttempts().List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(attemptsAfter) != len(attemptsBefore) {
		t.Fatalf("a refused retained recovery changed the attempt ledger: %d -> %d", len(attemptsBefore), len(attemptsAfter))
	}
	if !strings.Contains(refusal.Error(), "no longer holds exclusive resource") {
		t.Fatalf("the refusal was not the native proof resource holder refusal: %v", refusal)
	}
}

func TestRetainedFailedProofRecoveryRefusesUnreachableAncestorBeforeEffects(t *testing.T) {
	for _, window := range retainedFailedProofRecoveryWindows {
		for _, currentLinked := range []bool{true, false} {
			name := window.name + "/current_unlinked"
			if currentLinked {
				name = window.name + "/current_linked"
			}
			t.Run(name, func(t *testing.T) {
				fixture := retainedFailedProofInterruptedRetryFixture
				if currentLinked {
					fixture = retainedFailedProofRetryAncestorFixture
				}
				f, acquired, sum, at, _ := fixture(t)
				w, err := f.store.Get(f.workID)
				if err != nil {
					t.Fatal(err)
				}
				ids := w.RunAttemptIDs[:0]
				for _, id := range w.RunAttemptIDs {
					if id != acquired.Reservation.RunAttemptID {
						ids = append(ids, id)
					}
				}
				w.RunAttemptIDs = ids
				if err := f.store.writeJSON(f.store.workPath(f.workID), &w); err != nil {
					t.Fatal(err)
				}
				snapshot := func() []byte {
					t.Helper()
					records := map[string][]byte{}
					err := filepath.WalkDir(f.store.home, func(path string, entry fs.DirEntry, walkErr error) error {
						if walkErr != nil {
							return walkErr
						}
						if entry.IsDir() || !strings.HasSuffix(path, ".json") {
							return nil
						}
						body, err := os.ReadFile(path)
						if err != nil {
							return err
						}
						records[path] = body
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					body, err := json.Marshal(records)
					if err != nil {
						t.Fatal(err)
					}
					return body
				}
				checkAt := at.Add(window.at)
				resource := landlease.ProofResourceForIdentity(f.store.home, acquired.Reservation.ProofCoordination)
				holderBefore, err := resource.Status(checkAt)
				if err != nil {
					t.Fatal(err)
				}
				stateBefore := snapshot()
				restored := 0
				_, _, refusal := f.store.ResumeRetainedFailedProofCompletion(f.workID, ProofResourceRetainedFailureInput{
					Actor:                 proofActorAlpha,
					ReservationID:         acquired.Reservation.ID,
					ExpectedPendingSHA256: sum,
					PrepareOwnedReport: func(ProofResourceResult) error {
						restored++
						return nil
					},
				}, checkAt)
				if refusal == nil || !strings.Contains(refusal.Error(), "not reachable from work") {
					t.Fatalf("unreachable ancestor must refuse through native membership validation: %v", refusal)
				}
				if restored != 0 {
					t.Fatalf("unreachable ancestor restored a report: %d", restored)
				}
				if !bytes.Equal(stateBefore, snapshot()) {
					t.Fatal("changed native records before refusing unreachable ancestry")
				}
				holderAfter, err := resource.Status(checkAt)
				if err != nil {
					t.Fatal(err)
				}
				beforeJSON, err := json.Marshal(holderBefore)
				if err != nil {
					t.Fatal(err)
				}
				afterJSON, err := json.Marshal(holderAfter)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(beforeJSON, afterJSON) {
					t.Fatal("changed proof holder before refusing unreachable ancestry")
				}
			})
		}
	}
}
