package work

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
)

func quiescentRecoveryFixture(t *testing.T) (*Store, ProofResourceReserveResult, QuiescentProofRecoveryInput, time.Time) {
	t.Helper()
	s := coordinatedStoreFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	w := claimedProofWork(t, s, "recover exact proof", proofActorAlpha, now)
	profile := capacitySystemProfile(t, false)
	r, err := s.ReserveProofResource(w.ID, ProofResourceReserveInput{
		Actor: proofActorAlpha, TTL: 10 * time.Minute, Workspace: profile.Repo, Command: "compound proof-check", Profile: profile,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.ExecutionBarrier.Close() })
	return s, r, QuiescentProofRecoveryInput{Actor: proofActorAlpha, ReservationID: r.Reservation.ID, ExpectedRunAttemptID: r.Reservation.RunAttemptID}, time.Now().UTC()
}

// Follow the existing fixture's coordination symlinks, preserving every durable
// byte rather than merely checking that the Work state label stayed unchanged.
func quiescentRecoveryBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out, visited := map[string]string{}, map[string]bool{}
	var walk func(string)
	walk = func(path string) {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		if visited[real] {
			return
		}
		visited[real] = true
		st, err := os.Stat(real)
		if err != nil {
			t.Fatal(err)
		}
		if st.IsDir() {
			entries, err := os.ReadDir(real)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				walk(filepath.Join(real, e.Name()))
			}
		} else if st.Mode().IsRegular() {
			b, err := os.ReadFile(real)
			if err != nil {
				t.Fatal(err)
			}
			out[real] = fmt.Sprintf("%o:%s", st.Mode(), b)
		}
	}
	walk(root)
	layout, err := landlease.LayoutStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if layout.BackingRoot != "" {
		walk(layout.BackingRoot)
	}
	return out
}

func requireQuiescentRefusalUnchanged(t *testing.T, s *Store, workID string, in QuiescentProofRecoveryInput, now time.Time) error {
	t.Helper()
	before := quiescentRecoveryBytes(t, s.home)
	_, err := s.ReconcileQuiescentProofResource(workID, in, now)
	if err == nil {
		t.Fatal("recovery unexpectedly accepted")
	}
	if after := quiescentRecoveryBytes(t, s.home); !reflect.DeepEqual(before, after) {
		t.Fatalf("refusal changed durable state: %v", err)
	}
	return err
}

func TestQuiescentProofRecoveryFailsOriginalAndUnblocksEarlierWaiter(t *testing.T) {
	s, r, in, now := quiescentRecoveryFixture(t)
	w0, _ := s.Get(r.Reservation.WorkID)
	identity := *r.Reservation.ProofCoordination
	identity.Slot = 0
	waiter, err := landlease.BeginProofAdmission(s.home, identity, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Cancel()
	if err := waiter.WithTurn(context.Background(), false, func(*landlease.Resource) error { return fmt.Errorf("must be busy") }); err == nil {
		t.Fatal("occupied holder admitted")
	}
	if err := r.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	queueBefore := quiescentRecoveryQueueBytes(t, s.home)
	done, err := s.ReconcileQuiescentProofResource(w0.ID, in, now.Add(time.Second))
	if !reflect.DeepEqual(queueBefore, quiescentRecoveryQueueBytes(t, s.home)) {
		t.Fatal("recovery changed queued ticket state")
	}
	if err != nil {
		t.Fatal(err)
	}
	if done.Result.Status != ProofResourceFailed || !done.ResourceReleased || done.VerificationRef != "" ||
		done.Result.ProducerExitKnown || done.Result.ReportArtifactRef != "" || done.Result.RunAttemptID != in.ExpectedRunAttemptID ||
		done.Result.FailureReason != quiescentProofFailure {
		t.Fatalf("unexpected recovery: %+v", done)
	}
	w1, _ := s.Get(w0.ID)
	if w0.State != w1.State || !reflect.DeepEqual(w0.VerificationIDs, w1.VerificationIDs) || !reflect.DeepEqual(w0.RunAttemptIDs, w1.RunAttemptIDs) {
		t.Fatalf("recovery changed work lifecycle/proof credit")
	}
	ra, err := s.runAttempts().Get(in.ExpectedRunAttemptID)
	if err != nil || ra.Status != runattempt.StatusFailed {
		t.Fatalf("original not failed: %+v %v", ra, err)
	}
	// The original queued ticket survives unchanged and remains usable. Recovery
	// does not call admission or mint a successor reservation/attempt for itself.
	if err := waiter.WithTurn(context.Background(), false, func(resource *landlease.Resource) error {
		_, barrier, err := resource.AcquireWithExecutionBarrierPrepared("fixture-next-holder", time.Minute, time.Now(), func(landlease.AcquireOutcome) error { return nil })
		if err != nil {
			return err
		}
		defer barrier.Close()
		return nil
	}); err != nil {
		t.Fatalf("earlier waiter could not proceed: %v", err)
	}
	requireQuiescentRefusalUnchanged(t, s, w0.ID, in, now.Add(2*time.Second))
	status, err := landlease.ProofResourceForIdentity(s.home, r.Reservation.ProofCoordination).StatusReadOnly(now.Add(2 * time.Second))
	if err != nil || status.Holder != "fixture-next-holder" {
		t.Fatalf("repeat touched successor: %+v %v", status, err)
	}
}

func TestQuiescentProofRecoveryRefusalsPreserveState(t *testing.T) {
	for _, kind := range []string{"live-barrier", "expired", "wrong-work", "wrong-actor", "wrong-run", "pending-claim", "corrupt-journal", "pending-failure", "pending-success", "recorded-source-drift", "lease-window", "class-drift", "remote-profile", "missing-barrier"} {
		t.Run(kind, func(t *testing.T) {
			s, r, in, now := quiescentRecoveryFixture(t)
			wid := r.Reservation.WorkID
			if kind != "live-barrier" {
				if err := r.ExecutionBarrier.Close(); err != nil {
					t.Fatal(err)
				}
			}
			reservation := r.Reservation
			switch kind {
			case "expired":
				now = now.Add(11 * time.Minute)
			case "wrong-work":
				wid = "work_other"
			case "wrong-actor":
				in.Actor = "codex:other"
			case "wrong-run":
				in.ExpectedRunAttemptID = "ratt_20260102T030405Z_bbbbbbbb"
			case "pending-claim":
				if err := os.WriteFile(s.claimPendingPath(wid), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "pending-failure", "pending-success":
				quiescentPendingJournal(t, s, reservation, kind == "pending-success", now)
			case "corrupt-journal":
				path, err := s.proofResourceCompletionPendingPath(reservation.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}

			case "recorded-source-drift":
				source := *reservation.Profile.SourceSnapshot
				source.RepositoryFingerprint = strings.Repeat("b", 64)
				reservation.Profile.SourceSnapshot = &source
				if err := s.events.Remove(reservation.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.writeEventJSON(reservation.ID, reservation); err != nil {
					t.Fatal(err)
				}

			case "lease-window":
				reservation.ExpiresAt = now.Add(9 * time.Minute).Format(time.RFC3339)
				if err := func() error {
					if err := s.events.Remove(reservation.ID); err != nil {
						return err
					}
					return s.writeEventJSON(reservation.ID, reservation)
				}(); err != nil {
					t.Fatal(err)
				}
			case "class-drift":
				copyIdentity := *reservation.ProofCoordination
				copyIdentity.Class = landlease.ProofCapacityNativeConsumer
				reservation.ProofCoordination = &copyIdentity
				if err := func() error {
					if err := s.events.Remove(reservation.ID); err != nil {
						return err
					}
					return s.writeEventJSON(reservation.ID, reservation)
				}(); err != nil {
					t.Fatal(err)
				}
			case "remote-profile":
				reservation.Profile.RemoteGoTest = &RemoteProofDescriptor{}
				if err := func() error {
					if err := s.events.Remove(reservation.ID); err != nil {
						return err
					}
					return s.writeEventJSON(reservation.ID, reservation)
				}(); err != nil {
					t.Fatal(err)
				}
			case "missing-barrier":
				for path := range quiescentRecoveryBytes(t, s.home) {
					if strings.Contains(path, "execution") && strings.Contains(path, ".lock") {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			err := requireQuiescentRefusalUnchanged(t, s, wid, in, now)
			if kind == "expired" && !strings.Contains(err.Error(), "expired") {
				t.Fatalf("not explicit expiry refusal: %v", err)
			}
		})
	}
}

func TestQuiescentProofRecoveryRefusesInheritedLiveChild(t *testing.T) {
	const helper = "COMPOUND_QUIESCENT_RECOVERY_CHILD"
	if os.Getenv(helper) == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	s, r, in, now := quiescentRecoveryFixture(t)
	child := exec.Command(os.Args[0], "-test.run=^TestQuiescentProofRecoveryRefusesInheritedLiveChild$")
	child.Env = append(os.Environ(), helper+"=1")
	files, err := r.ExecutionBarrier.DuplicateFilesForChild()
	if err != nil {
		t.Fatal(err)
	}
	child.ExtraFiles = files
	pipe, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pipe.Close(); _ = child.Process.Kill() })
	for _, f := range files {
		_ = f.Close()
	}
	if err := r.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	requireQuiescentRefusalUnchanged(t, s, r.Reservation.WorkID, in, now)
	_ = pipe.Close()
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, now); err != nil {
		t.Fatal(err)
	}
}

func TestQuiescentProofRecoveryAndNormalCompletionKeepWinner(t *testing.T) {
	for _, recoveryFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(recoveryFirst), func(t *testing.T) {
			s, r, in, now := quiescentRecoveryFixture(t)
			if err := r.ExecutionBarrier.Close(); err != nil {
				t.Fatal(err)
			}
			normal := ProofResourceCompleteInput{Actor: in.Actor, ReservationID: in.ReservationID, ReportArtifactRef: quiescentRecoveryReport(t, s, r.Reservation, now), ProducerExitKnown: true}
			if recoveryFirst {
				if _, err := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, now); err != nil {
					t.Fatal(err)
				}
				before := quiescentRecoveryBytes(t, s.home)
				if _, err := s.CompleteProofResource(r.Reservation.WorkID, normal, now); err == nil {
					t.Fatal("late producer completion succeeded")
				}
				if !reflect.DeepEqual(before, quiescentRecoveryBytes(t, s.home)) {
					t.Fatal("late producer changed committed recovery")
				}
			} else {
				done, err := s.CompleteProofResource(r.Reservation.WorkID, normal, now)
				if err != nil {
					t.Fatal(err)
				}
				if !done.Result.ProducerExitKnown {
					t.Fatal("fixture completion missing known exit")
				}
				requireQuiescentRefusalUnchanged(t, s, r.Reservation.WorkID, in, now)
			}
		})
	}
}

func TestQuiescentProofRecoveryCLIIsStrict(t *testing.T) {
	base := []string{"--reconcile-quiescent", "pfr_exact", "--work", "work_exact", "--actor", "codex:alpha", "--expected-run", "ratt_20260102T030405Z_bbbbbbbb"}
	for _, extra := range [][]string{{"--wait", "1s"}, {"--execution-timeout", "1s"}, {"--out", "x"}, {"--report", "x"}, {"--producer-exit", "0"}, {"--steal"}, {"--repo", "x"}, {"--remote-reconcile", "pfr_other"}, {"--work", "work_again"}, {"positional"}} {
		var out, err bytes.Buffer
		if code := RunQuiescentProofRecoveryCLI(append(append([]string{}, base...), extra...), &out, &err); code != 2 || out.Len() != 0 {
			t.Fatalf("unsafe options accepted: %v code=%d out=%s err=%s", extra, code, &out, &err)
		}
	}
	s, r, in, _ := quiescentRecoveryFixture(t)
	_ = r.ExecutionBarrier.Close()
	var out, stderr bytes.Buffer
	code := RunQuiescentProofRecoveryCLI([]string{"--reconcile-quiescent", in.ReservationID, "--work", r.Reservation.WorkID, "--actor", in.Actor, "--expected-run", in.ExpectedRunAttemptID, "--home", s.home, "--json"}, &out, &stderr)
	var result ProofResourceCompleteResult
	if code != 0 || json.Unmarshal(out.Bytes(), &result) != nil || result.Result.Status != ProofResourceFailed {
		t.Fatalf("CLI failed code=%d out=%s err=%s", code, &out, &stderr)
	}
}

func quiescentRecoveryReport(t *testing.T, s *Store, r ProofResourceReservation, now time.Time) string {
	t.Helper()
	steps := []map[string]any{}
	refs := []string{}
	for _, g := range r.Profile.Gates {
		steps = append(steps, map[string]any{"name": g.Name, "command": g.Command, "proof_ref": g.ProofRef, "status": "ok"})
		refs = append(refs, g.ProofRef)
	}
	body, err := json.Marshal(map[string]any{"schema_version": "compound.proof_check_report.v0.3", "profile_kind": ProofResourceProfileCanonicalFull, "status": "ok", "generated_at": now.Format(time.RFC3339Nano), "repo": r.Profile.Repo, "base_ref": r.Profile.BaseRef, "source_snapshot": r.Profile.SourceSnapshot, "dry_run": false, "proof_refs": refs, "steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func quiescentPendingJournal(t *testing.T, s *Store, r ProofResourceReservation, success bool, now time.Time) {
	t.Helper()
	input := ProofResourceCompleteInput{Actor: r.Actor, ReservationID: r.ID, FailureReason: "different producer failure"}
	if success {
		input.FailureReason = ""
		input.ReportArtifactRef = quiescentRecoveryReport(t, s, r, now)
		input.ProducerExitKnown = true
	}
	previous := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "result" {
			return fmt.Errorf("synthetic finalizer stopped after journal")
		}
		return previous(store, stage, id, value)
	}
	err := s.withLifecycleMutation(func() error { _, err := s.completeProofResourceLocked(r.WorkID, input, now); return err })
	proofResourceWriteEventRecord = previous
	if err == nil {
		t.Fatal("journal fixture unexpectedly finalized")
	}
	pending, present, _, err := s.readProofResourceCompletionPending(r.ID)
	if err != nil || !present {
		t.Fatalf("missing valid pending journal: %v %v", present, err)
	}
	if (pending.Result.Status == ProofResourceVerified) != success {
		t.Fatalf("wrong journal fixture: %+v", pending.Result)
	}
}

func TestQuiescentProofRecoveryRefusesStalePrivateAuthorityBeforeJournal(t *testing.T) {
	f, profile := privateNativeConsumerProfile(t)
	backing, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := landlease.MigrateLegacyLayout(f.store.home, backing, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := landlease.MigrateProofDomain(backing, []string{canonical}, func() error { return nil }, nil, landlease.ProofDomainFencedV3); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: 10 * time.Minute, Workspace: f.repo, Command: "compound proof-check", Profile: profile}, now)
	if err != nil {
		t.Fatal(err)
	}
	defer r.ExecutionBarrier.Close()
	if err := r.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	stale := now.Add(6 * time.Minute)
	if err := f.store.validatePrivateSourceProfileAuthority(f.workID, proofActorAlpha, profile, stale); err == nil {
		t.Fatal("private review unexpectedly current")
	}
	requireQuiescentRefusalUnchanged(t, f.store, f.workID, QuiescentProofRecoveryInput{Actor: proofActorAlpha, ReservationID: r.Reservation.ID, ExpectedRunAttemptID: r.Reservation.RunAttemptID}, stale)
}

func TestQuiescentProofRecoveryAllowsRecordedSourceWhenCheckoutUnavailable(t *testing.T) {
	for _, kind := range []string{"dirty", "missing"} {
		t.Run(kind, func(t *testing.T) {
			s, r, in, now := quiescentRecoveryFixture(t)
			_ = r.ExecutionBarrier.Close()
			if kind == "dirty" {
				if err := os.WriteFile(filepath.Join(r.Reservation.Profile.Repo, "drift"), []byte("dirty"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				repo := r.Reservation.Profile.Repo
				moved := repo + "-unavailable"
				if err := os.Rename(repo, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Rename(moved, repo) })
			}
			if _, err := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, now); err != nil {
				t.Fatalf("failure cleanup required live checkout: %v", err)
			}
		})
	}
}
func TestQuiescentProofRecoveryPreservesNewerWorkGeneration(t *testing.T) {
	s, r, in, now := quiescentRecoveryFixture(t)
	_ = r.ExecutionBarrier.Close()
	if _, err := s.Advance(r.Reservation.WorkID, AdvanceInput{Actor: in.Actor, To: "running"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	err := requireQuiescentRefusalUnchanged(t, s, r.Reservation.WorkID, in, now.Add(2*time.Second))
	if !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("wrong generation refusal: %v", err)
	}
}
func TestQuiescentProofRecoveryAcceptsExactRootedRefresh(t *testing.T) {
	s, r, in, now := quiescentRecoveryFixture(t)
	_ = r.ExecutionBarrier.Close()
	for _, offset := range []time.Duration{time.Second, 2 * time.Second} {
		if _, err := s.RefreshProofResource(r.Reservation.WorkID, r.Reservation.ID, in.Actor, 10*time.Minute, now.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func quiescentRecoveryQueueBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for path, value := range quiescentRecoveryBytes(t, root) {
		if strings.Contains(path, "proof-queue") || strings.Contains(path, "proof-tickets") {
			out[path] = value
		}
	}
	if len(out) == 0 {
		t.Fatal("queue fixture has no observable ticket state")
	}
	return out
}

func TestQuiescentProofRecoveryResumesOnlyOwnExactFailureJournal(t *testing.T) {
	for _, stage := range []string{"result", "work", "completion", "work_final"} {
		t.Run(stage, func(t *testing.T) {
			s, r, in, now := quiescentRecoveryFixture(t)
			_ = r.ExecutionBarrier.Close()
			oldEvent, oldWork := proofResourceWriteEventRecord, proofResourceWriteWorkRecord
			proofResourceWriteEventRecord = func(store *Store, at, id string, value any) error {
				if at == stage {
					return fmt.Errorf("synthetic interrupted recovery at %s", stage)
				}
				return oldEvent(store, at, id, value)
			}
			proofResourceWriteWorkRecord = func(store *Store, at string, w *envelope.WorkEnvelope) error {
				if at == stage {
					return fmt.Errorf("synthetic interrupted recovery at %s", stage)
				}
				return oldWork(store, at, w)
			}
			_, err := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, now)
			proofResourceWriteEventRecord, proofResourceWriteWorkRecord = oldEvent, oldWork
			if err == nil {
				t.Fatal("fault did not retain exact recovery")
			}
			pending, present, _, err := s.readProofResourceCompletionPending(in.ReservationID)
			if err != nil || !present || pending.Result.FailureReason != quiescentProofFailure {
				t.Fatalf("wrong pending recovery: %+v %v", pending, err)
			}
			before, _ := s.Get(r.Reservation.WorkID)
			repo := r.Reservation.Profile.Repo
			moved := repo + "-unavailable"
			if err := os.Rename(repo, moved); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Rename(moved, repo) })
			done, err := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			after, _ := s.Get(r.Reservation.WorkID)
			if !done.ResourceReleased || done.Result.RunAttemptID != in.ExpectedRunAttemptID || done.VerificationRef != "" || !reflect.DeepEqual(before.RunAttemptIDs, after.RunAttemptIDs) || !reflect.DeepEqual(before.VerificationIDs, after.VerificationIDs) || before.State != after.State {
				t.Fatalf("retry changed original authority: %+v", done)
			}
		})
	}
}

func TestQuiescentProofRecoveryRefusesChangedOwnPending(t *testing.T) {
	for _, kind := range []string{"newer-generation", "different-terminal", "changed-marker", "legacy-seal", "retry-descendant"} {
		t.Run(kind, func(t *testing.T) {
			s, r, in, now := quiescentRecoveryFixture(t)
			_ = r.ExecutionBarrier.Close()
			previous := proofResourceWriteEventRecord
			proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
				if stage == "result" {
					return fmt.Errorf("synthetic pending recovery")
				}
				return previous(store, stage, id, value)
			}
			_, firstErr := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, now)
			proofResourceWriteEventRecord = previous
			if firstErr == nil {
				t.Fatal("expected retained journal")
			}
			pending, present, path, err := s.readProofResourceCompletionPending(in.ReservationID)
			if err != nil || !present {
				t.Fatalf("pending=%v err=%v", present, err)
			}
			switch kind {
			case "newer-generation":
				if _, err := s.Advance(r.Reservation.WorkID, AdvanceInput{Actor: in.Actor, To: "running"}, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			case "different-terminal":
				if _, err := s.runAttempts().CompleteWithReason(in.ExpectedRunAttemptID, runattempt.StatusFailed, "foreign terminal reason", now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			case "retry-descendant":
				w, err := s.Get(r.Reservation.WorkID)
				if err != nil {
					t.Fatal(err)
				}
				retry, err := s.prepareProofRunAttempt(w, pending.Action.ID, ProofResourceReserveInput{Actor: in.Actor, Workspace: r.Reservation.Workspace, Command: r.Reservation.Command}, now.Add(time.Second))
				if err != nil || retry.PredecessorID != in.ExpectedRunAttemptID {
					t.Fatalf("descendant fixture: %+v %v", retry, err)
				}

			case "changed-marker":
				pending.Result.FailureReason = "different failure"
				if err := s.writeJSON(path, pending); err != nil {
					t.Fatal(err)
				}
			case "legacy-seal":
				pending.GovernanceSeal = ""
				if err := s.writeJSON(path, pending); err != nil {
					t.Fatal(err)
				}
			}
			requireQuiescentRefusalUnchanged(t, s, r.Reservation.WorkID, in, now.Add(2*time.Second))
		})
	}
}

func quiescentReleasedJournalFixture(t *testing.T) (*Store, ProofResourceReserveResult, QuiescentProofRecoveryInput, time.Time, proofResourceCompletionPending, string) {
	t.Helper()
	s, r, in, now := quiescentRecoveryFixture(t)
	_ = r.ExecutionBarrier.Close()
	var retained proofResourceCompletionPending
	var path string
	previous := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(store *Store, stage, id string, value any) error {
		if stage == "completion" {
			var present bool
			var err error
			retained, present, path, err = store.readProofResourceCompletionPending(in.ReservationID)
			if err != nil || !present {
				return fmt.Errorf("capture native journal: present=%v err=%v", present, err)
			}
		}
		return previous(store, stage, id, value)
	}
	done, err := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, now)
	proofResourceWriteEventRecord = previous
	if err != nil || !done.ResourceReleased || path == "" {
		t.Fatalf("native release fixture: %+v %v", done, err)
	}
	// Recreate precisely the native durable image between holder release and the
	// following unlink. Result/completion/attempt/work evidence was never forged.
	if err := s.writeJSON(path, retained); err != nil {
		t.Fatal(err)
	}
	return s, r, in, now, retained, path
}
func TestQuiescentProofRecoveryCleansOwnCommittedJournalAfterRelease(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(fmt.Sprint(successor), func(t *testing.T) {
			s, r, in, now, pending, path := quiescentReleasedJournalFixture(t)
			if successor {
				w := claimedProofWork(t, s, "foreign successor proof", proofActorAlpha, now)
				profile := capacitySystemProfile(t, false)
				next, err := s.ReserveProofResource(w.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: profile.Repo, Command: "compound proof-check", Profile: profile}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				defer next.ExecutionBarrier.Close()
			}
			repo := r.Reservation.Profile.Repo
			moved := repo + "-unavailable"
			if err := os.Rename(repo, moved); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Rename(moved, repo) })
			before := quiescentRecoveryBytes(t, s.home)
			canonicalJournal, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatal(err)
			}
			delete(before, canonicalJournal)
			done, err := s.ReconcileQuiescentProofResource(r.Reservation.WorkID, in, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if done.ResourceReleased || done.RecoveryJournalRetained || done.Result.ID != pending.Result.ID || done.Result.Status != ProofResourceFailed || done.VerificationRef != "" || !strings.Contains(done.ReleaseWarning, "journal") {
				t.Fatalf("cleanup manufactured release: %+v", done)
			}
			if !reflect.DeepEqual(before, quiescentRecoveryBytes(t, s.home)) {
				t.Fatal("cleanup changed more than exact old journal, including successor")
			}
			requireQuiescentRefusalUnchanged(t, s, r.Reservation.WorkID, in, time.Now())
		})
	}
}
func TestQuiescentProofRecoveryRefusesUnprovenReleasedJournal(t *testing.T) {
	for _, kind := range []string{"missing-completion", "forged-result", "forged-decision", "newer-work"} {
		t.Run(kind, func(t *testing.T) {
			s, r, in, now, pending, _ := quiescentReleasedJournalFixture(t)
			switch kind {
			case "missing-completion":
				if err := s.events.Remove(pending.Completion.ID); err != nil {
					t.Fatal(err)
				}
			case "forged-result":
				changed := pending.Result
				changed.FailureReason = "forged failure"
				if err := s.events.Remove(pending.Completion.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.events.Remove(changed.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.writeEventJSON(changed.ID, changed); err != nil {
					t.Fatal(err)
				}
				if err := s.writeEventJSON(pending.Completion.ID, pending.Completion); err != nil {
					t.Fatal(err)
				}
			case "forged-decision":
				changed := pending.Decision
				changed.Reasons = []string{"different stored decision body"}
				if err := s.events.Remove(changed.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.writeEventJSON(changed.ID, changed); err != nil {
					t.Fatal(err)
				}
			case "newer-work":
				if _, err := s.Advance(r.Reservation.WorkID, AdvanceInput{Actor: in.Actor, To: "running"}, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			requireQuiescentRefusalUnchanged(t, s, r.Reservation.WorkID, in, time.Now())
		})
	}
}
