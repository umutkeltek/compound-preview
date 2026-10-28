package work

import (
	"bytes"
	"compound/internal/authority/gitrepository"
	"compound/internal/landlease"
	"compound/internal/runattempt"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCanonicalProofCapacityRequiresExactNativeInventory(t *testing.T) {
	repo, source := pytestRepo(t, nil)
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	class, err := CanonicalProofCapacityClass(profile)
	if err != nil || class != landlease.ProofCapacityNativeConsumer {
		t.Fatalf("native class = %s: %v", class, err)
	}
	for _, kind := range []string{"omitted-gate", "changed-command", "reordered", "forged-repository", "missing-source", "stale-digest", "changed-kind"} {
		t.Run(kind, func(t *testing.T) {
			changed := profile
			changed.Gates = append([]ProofResourceGateProfile(nil), profile.Gates...)
			sourceCopy := *profile.SourceSnapshot
			changed.SourceSnapshot = &sourceCopy
			changed.Digest = ""
			switch kind {
			case "omitted-gate":
				changed.Gates = changed.Gates[1:]
			case "changed-command":
				changed.Gates[0].Command = []string{"true"}
			case "reordered":
				changed.Gates[0], changed.Gates[1] = changed.Gates[1], changed.Gates[0]
			case "forged-repository":
				changed.SourceSnapshot.RepositoryFingerprint = strings.Repeat("a", 64)
			case "missing-source":
				changed.SourceSnapshot = nil
			case "stale-digest":
				changed.Digest = strings.Repeat("b", 64)
			case "changed-kind":
				changed.Kind = "diagnostic"
			}
			if class, err := CanonicalProofCapacityClass(changed); err == nil {
				t.Fatalf("forged profile admitted as %s", class)
			}
		})
	}
}

func coordinatedStoreFixture(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	backing, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := landlease.MigrateLegacyLayout(s.home, backing, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := landlease.MigrateProofDomain(backing, []string{canonical}, func() error { return nil }, nil, landlease.ProofDomainFencedV3); err != nil {
		t.Fatal(err)
	}
	return s
}
func capacitySystemProfile(t *testing.T, rust bool) ProofResourceProfile {
	t.Helper()
	return capacitySystemProfileForOrigin(t, rust, "")
}

func capacitySystemProfileForOrigin(t *testing.T, rust bool, origin string) ProofResourceProfile {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{"go.mod": "module compound\n\ngo 1.24\n"}
	if rust {
		files = map[string]string{"Cargo.toml": "[package]\nname = \"synthetic\"\nversion = \"0.1.0\"\nedition = \"2021\"\n", "Cargo.lock": "version = 4\n"}
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	nodeGit(t, repo, "init", "-q")
	nodeGit(t, repo, "add", ".")
	nodeGit(t, repo, "commit", "-qm", "fixture")
	if origin != "" {
		nodeGit(t, repo, "remote", "add", "origin", origin)
	}
	source, err := gitrepository.CaptureCleanSnapshot(repo, nodeGit(t, repo, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewProofResourceProfileWithSource(ProofResourceProfileCanonicalFull, repo, source, []ProofResourceGateProfile{{Name: "fixture", Command: []string{"true"}, ProofRef: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
func TestCanonicalProofCapacityCoreAndRustRemainSystemFull(t *testing.T) {
	for _, rust := range []bool{false, true} {
		class, err := CanonicalProofCapacityClass(capacitySystemProfile(t, rust))
		if err != nil || class != landlease.ProofCapacitySystemFull {
			t.Fatalf("system profile class=%s err=%v", class, err)
		}
	}
}
func TestCoordinatedProofStoreRoutesAdmissionHeartbeatAndCompletion(t *testing.T) {
	s := coordinatedStoreFixture(t)
	now := time.Now().UTC()
	nativeWork := claimedProofWork(t, s, "native proof", proofActorAlpha, now)
	systemWork := claimedProofWork(t, s, "system proof", proofActorAlpha, now)
	blockedWork := claimedProofWork(t, s, "queued native proof", proofActorAlpha, now)
	repo, source := pytestRepo(t, nil)
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: repo, Command: "compound proof-check", Profile: profile}
	native, err := s.ReserveProofResource(nativeWork.ID, input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.ExecutionBarrier.Close() })
	if native.Reservation.ProofCoordination == nil || native.Reservation.ProofCoordination.Class != landlease.ProofCapacityNativeConsumer {
		t.Fatal("native route identity missing")
	}
	systemProfile := capacitySystemProfile(t, false)
	system, err := s.ReserveProofResource(systemWork.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: systemProfile.Repo, Command: "compound proof-check", Profile: systemProfile}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.ExecutionBarrier.Close() })
	if system.Reservation.ProofCoordination.Class != landlease.ProofCapacitySystemFull {
		t.Fatal("core route not system capacity")
	}
	before := runAttemptCount(t, s, blockedWork.ID)
	refused, err := s.ReserveProofResource(blockedWork.ID, input, time.Now())
	if !proofAdmissionBusy(err) || refused.Reservation.ID != "" || runAttemptCount(t, s, blockedWork.ID) != before {
		t.Fatalf("busy capacity minted attempt: %+v %v", refused.Reservation, err)
	}
	if _, err := s.RefreshProofResource(nativeWork.ID, native.Reservation.ID, proofActorAlpha, time.Minute, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, item := range []ProofResourceReserveResult{native, system} {
		if err := item.ExecutionBarrier.Close(); err != nil {
			t.Fatal(err)
		}
		done, err := s.CompleteProofResource(item.Reservation.WorkID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: item.Reservation.ID, FailureReason: "fixture stopped before gates", ProducerExitKnown: true, ProducerExitCode: 1}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !done.ResourceReleased || done.VerificationRef != "" {
			t.Fatalf("failed proof did not release exact class without certification: %+v", done)
		}
	}
	rows, err := landlease.ProofResourceStatuses(s.home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Status.Holder != "" {
			t.Fatalf("class retained after completion: %+v", row)
		}
	}
}
func TestCoordinatedProofStoreRejectsForgedNativeBeforeAttempt(t *testing.T) {
	s := coordinatedStoreFixture(t)
	w := claimedProofWork(t, s, "reject forged profile", proofActorAlpha, time.Now())
	repo, source := pytestRepo(t, nil)
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	profile.Gates = profile.Gates[1:]
	profile.Digest = ""
	before := runAttemptCount(t, s, w.ID)
	_, err = s.ReserveProofResource(w.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: repo, Command: "compound proof-check", Profile: profile}, time.Now())
	if err == nil || runAttemptCount(t, s, w.ID) != before {
		t.Fatalf("forged profile created attempt: %v", err)
	}
}
func TestCoordinatedProofCancelledWaitMintsNoAttempt(t *testing.T) {
	s := coordinatedStoreFixture(t)
	w := claimedProofWork(t, s, "cancel queued native", proofActorAlpha, time.Now())
	repo, source := pytestRepo(t, nil)
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := runAttemptCount(t, s, w.ID)
	_, err = s.ReserveProofResourceWhenFree(ctx, w.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: repo, Command: "compound proof-check", Profile: profile}, time.Minute, landlease.RealClock{}, nil)
	if err == nil || runAttemptCount(t, s, w.ID) != before {
		t.Fatalf("cancelled waiter created attempt: %v", err)
	}
}

func TestCoordinatedProofMissingIdentityCannotBecomeHistorical(t *testing.T) {
	old := ProofResourceReservation{SchemaVersion: ProofResourceReservationSchemaVersion}
	if err := validateProofCoordination(old); err != nil {
		t.Fatal(err)
	}
	changed := old
	changed.SchemaVersion = ProofResourceCoordinatedReservationSchemaVersion
	if err := validateProofCoordination(changed); err == nil {
		t.Fatal("new schema silently downgraded to singleton")
	}
	identity, err := landlease.NewProofCoordinationIdentity(strings.Repeat("1", 64), landlease.ProofCapacityNativeConsumer, strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	changed.ProofCoordination = &identity
	if err := validateProofCoordination(changed); err == nil {
		t.Fatal("identity without sealed repository accepted")
	}
	old.ProofCoordination = &identity
	if err := validateProofCoordination(old); err == nil {
		t.Fatal("historical record reinterpreted by attached identity")
	}
}
func TestCoordinatedProofForeignExpiredPredecessorPreservesLease(t *testing.T) {
	s, other, _, in := proofMigrationStoreFixture(t)
	root, err := landlease.LayoutStatus(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := landlease.MigrateProofDomain(root.BackingRoot, in.ProofHomes, func() error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	firstWork := claimedProofWork(t, s, "first-home proof", proofActorAlpha, time.Now())
	secondWork := claimedProofWork(t, other, "second-home proof", proofActorAlpha, time.Now())
	repo, source := pytestRepo(t, nil)
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	input := ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Second, Workspace: repo, Command: "compound proof-check", Profile: profile}
	first, err := s.ReserveProofResource(firstWork.ID, input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	// Expire the lease deterministically by observing its real TTL, then try the
	// other home while the predecessor's durable attempt is still nonterminal.
	time.Sleep(1100 * time.Millisecond)
	before := runAttemptCount(t, other, secondWork.ID)
	second, err := other.ReserveProofResource(secondWork.ID, input, time.Now())
	if !proofAdmissionBusy(err) || second.Reservation.ID != "" || runAttemptCount(t, other, secondWork.ID) != before {
		t.Fatalf("foreign predecessor replaced: %+v %v", second.Reservation, err)
	}
	resource := landlease.ProofResourceForIdentity(s.home, first.Reservation.ProofCoordination)
	status, err := resource.StatusReadOnly(time.Now())
	if err != nil || status.Holder != first.Reservation.Holder {
		t.Fatalf("original owner lost exact lease: %+v %v", status, err)
	}
}

func TestCoordinatedProofStatusReportsBothClasses(t *testing.T) {
	s := coordinatedStoreFixture(t)
	var out, diagnostics bytes.Buffer
	if code := RunLandingStatus(s.home, LandingCLISelection{Proof: true, Status: true}, true, &out, &diagnostics, time.Now()); code != 0 {
		t.Fatalf("status exit=%d %s", code, diagnostics.String())
	}
	var result struct {
		Generation string
		Resources  []landlease.ProofClassStatus
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	// Operator-approved capacity: two system_full slots and one native_consumer.
	if result.Generation == "" || len(result.Resources) != 3 ||
		result.Resources[0].Class != landlease.ProofCapacitySystemFull || result.Resources[0].Slot != 0 ||
		result.Resources[1].Class != landlease.ProofCapacitySystemFull || result.Resources[1].Slot != 1 ||
		result.Resources[2].Class != landlease.ProofCapacityNativeConsumer || result.Resources[2].Slot != 0 {
		t.Fatalf("missing class slot statuses: %+v", result)
	}
	out.Reset()
	if code := RunLandingStatus(s.home, LandingCLISelection{Proof: true, Status: true}, false, &out, &diagnostics, time.Now()); code != 0 {
		t.Fatal(diagnostics.String())
	}
	for _, class := range []string{"system_full[0]", "system_full[1]", "native_consumer[0]"} {
		if !strings.Contains(out.String(), class) {
			t.Fatalf("text status omitted %s", class)
		}
	}
}

func TestCoordinatedProofStealDoesNotHoldDomainWhileFinalizerRuns(t *testing.T) {
	for _, terminated := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "attestation"}[terminated], func(t *testing.T) {
			s := coordinatedStoreFixture(t)
			grant := map[string]any{"standing_grants": []map[string]string{{
				"id": "fixture_proof_steal", "capability": "proof.resource", "mutation_class": "security",
				"reversibility": "manual", "blast_radius": "repo", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			}}}
			body, err := json.Marshal(grant)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(s.home, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), body, 0600); err != nil {
				t.Fatal(err)
			}
			firstWork := claimedProofWork(t, s, "native producer", proofActorAlpha, time.Now())
			nextWork := claimedProofWork(t, s, "native successor", proofActorAlpha, time.Now())
			systemWork := claimedProofWork(t, s, "independent system", proofActorAlpha, time.Now())
			repo, source := pytestRepo(t, nil)
			plan, err := ResolveCanonicalProofPlan(repo, source, nil)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := ProofResourceProfileForCanonicalPlan(plan)
			if err != nil {
				t.Fatal(err)
			}
			input := ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: repo, Command: "compound proof-check", Profile: profile}
			first, err := s.ReserveProofResource(firstWork.ID, input, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := first.ExecutionBarrier.Close(); err != nil {
				t.Fatal(err)
			}
			resource := landlease.ProofResourceForIdentity(s.home, first.Reservation.ProofCoordination)
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				_, err := resource.ReleaseAfter(first.Reservation.Holder, time.Now(), func() error { close(entered); <-release; return nil })
				finished <- err
			}()
			<-entered
			var once sync.Once
			unlock := func() {
				once.Do(func() {
					close(release)
					if err := <-finished; err != nil {
						t.Error(err)
					}
				})
			}
			defer unlock()
			input.Steal = true
			input.StealReason = "fixture exact takeover"
			if terminated {
				input.TerminatedRunAttemptID = first.Reservation.RunAttemptID
			}
			result := make(chan error, 1)
			go func() { _, err := s.ReserveProofResource(nextWork.ID, input, time.Now()); result <- err }()
			select {
			case err := <-result:
				if !proofAdmissionBusy(err) {
					t.Fatalf("held finalizer should defer admission: %v", err)
				}
			case <-time.After(5 * time.Second):
				unlock()
				<-result
				t.Fatal("admission blocked on finalizer while holding domain")
			}
			systemProfile := capacitySystemProfile(t, false)
			independent, err := s.ReserveProofResource(systemWork.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: systemProfile.Repo, Command: "compound proof-check", Profile: systemProfile}, time.Now())
			if err != nil {
				t.Fatalf("independent class stalled behind finalizer: %v", err)
			}
			if err := independent.ExecutionBarrier.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = s.CompleteProofResource(systemWork.ID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: independent.Reservation.ID, FailureReason: "fixture", ProducerExitKnown: true, ProducerExitCode: 1}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

type proofCancelOnSleepClock struct{ cancel context.CancelFunc }

func (c proofCancelOnSleepClock) Now() time.Time      { return time.Now() }
func (c proofCancelOnSleepClock) Sleep(time.Duration) { c.cancel() }

// Occupied capacity must not fold the global lifecycle log on every poll.
func TestCoordinatedProofBusyWaitDoesNotReadLifecycleLog(t *testing.T) {
	// Git timestamps define fixture lineage, not proof freshness. Pin them so
	// the foreign-repository case cannot accidentally depend on crossing a second.
	t.Setenv("GIT_AUTHOR_DATE", "2001-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2001-01-01T00:00:00Z")
	for _, foreign := range []bool{false, true} {
		name := "same-repository"
		if foreign {
			name = "other-repository"
		}
		t.Run(name, func(t *testing.T) {
			s := coordinatedStoreFixture(t)
			firstWork := claimedProofWork(t, s, "active proof", proofActorAlpha, time.Now())
			waiter := claimedProofWork(t, s, "waiting proof", proofActorAlpha, time.Now())
			profile := capacitySystemProfile(t, false)
			input := ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: profile.Repo, Command: "compound proof-check", Profile: profile}
			first, err := s.ReserveProofResource(firstWork.ID, input, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer first.ExecutionBarrier.Close()
			if foreign {
				// Operator-approved capacity: system_full has two slots, so a
				// foreign waiter is only busy once both slots hold other repositories.
				secondWork := claimedProofWork(t, s, "second active proof", proofActorAlpha, time.Now())
				secondProfile := capacitySystemProfileForOrigin(t, false, "https://example.invalid/proof-slot-second.git")
				second, err := s.ReserveProofResource(secondWork.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: secondProfile.Repo, Command: "compound proof-check", Profile: secondProfile}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				defer second.ExecutionBarrier.Close()
				if second.Reservation.ProofCoordination == nil || second.Reservation.ProofCoordination.Slot != 1 {
					t.Fatalf("second repository not in slot 1: %+v", second.Reservation.ProofCoordination)
				}
				input.Profile = capacitySystemProfileForOrigin(t, false, "https://example.invalid/proof-fixture.git")
				input.Workspace = input.Profile.Repo
				if input.Profile.SourceSnapshot.RepositoryFingerprint == profile.SourceSnapshot.RepositoryFingerprint || input.Profile.SourceSnapshot.RepositoryFingerprint == secondProfile.SourceSnapshot.RepositoryFingerprint {
					t.Fatal("fixture repositories are not distinct")
				}
			}
			before := runAttemptCount(t, s, waiter.ID)
			reads := s.eventLogReads.Load()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := s.ReserveProofResourceWhenFree(ctx, waiter.ID, input, time.Minute, proofCancelOnSleepClock{cancel}, nil)
			if result.ExecutionBarrier != nil {
				_ = result.ExecutionBarrier.Close()
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("busy cancellation = %v", err)
			}
			if added := s.eventLogReads.Load() - reads; added != 0 {
				t.Fatalf("occupied capacity read lifecycle log %d times", added)
			}
			if after := runAttemptCount(t, s, waiter.ID); after != before {
				t.Fatalf("busy wait minted attempt: %d -> %d", before, after)
			}
		})
	}
}

// A foreign repository joins the second system_full slot without waiting, and
// its refresh and completion route to that exact slot root.
func TestCoordinatedProofSecondRepositoryUsesSecondSlot(t *testing.T) {
	t.Setenv("GIT_AUTHOR_DATE", "2001-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2001-01-01T00:00:00Z")
	s := coordinatedStoreFixture(t)
	firstWork := claimedProofWork(t, s, "first system proof", proofActorAlpha, time.Now())
	secondWork := claimedProofWork(t, s, "second system proof", proofActorAlpha, time.Now())
	firstProfile := capacitySystemProfile(t, false)
	first, err := s.ReserveProofResource(firstWork.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: firstProfile.Repo, Command: "compound proof-check", Profile: firstProfile}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.ExecutionBarrier.Close() })
	secondProfile := capacitySystemProfileForOrigin(t, false, "https://example.invalid/proof-slot-fixture.git")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second, err := s.ReserveProofResourceWhenFree(ctx, secondWork.ID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: secondProfile.Repo, Command: "compound proof-check", Profile: secondProfile}, time.Minute, proofCancelOnSleepClock{cancel}, nil)
	if err != nil {
		t.Fatalf("second repository waited for capacity: %v", err)
	}
	t.Cleanup(func() { _ = second.ExecutionBarrier.Close() })
	identity := second.Reservation.ProofCoordination
	if identity == nil || identity.Slot != 1 || identity.SchemaVersion != landlease.ProofCoordinationSlotIdentitySchema {
		t.Fatalf("second repository identity = %+v, want slot 1", identity)
	}
	if first.Reservation.ProofCoordination == nil || first.Reservation.ProofCoordination.Slot != 0 {
		t.Fatalf("first repository identity = %+v, want slot 0", first.Reservation.ProofCoordination)
	}
	slotStatus := func(slot int) landlease.StatusResult {
		t.Helper()
		rows, err := landlease.ProofResourceStatuses(s.home, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Class == landlease.ProofCapacitySystemFull && row.Slot == slot {
				return row.Status
			}
		}
		t.Fatalf("no system_full slot %d row", slot)
		return landlease.StatusResult{}
	}
	if status := slotStatus(1); !status.Held || status.Holder != second.Reservation.Holder {
		t.Fatalf("slot 1 status = %+v, want %s", status, second.Reservation.Holder)
	}
	if _, err := s.RefreshProofResource(secondWork.ID, second.Reservation.ID, proofActorAlpha, time.Minute, time.Now()); err != nil {
		t.Fatalf("slot-1 refresh: %v", err)
	}
	if err := second.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	done, err := s.CompleteProofResource(secondWork.ID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: second.Reservation.ID, FailureReason: "fixture stopped before gates", ProducerExitKnown: true, ProducerExitCode: 1}, time.Now())
	if err != nil || !done.ResourceReleased {
		t.Fatalf("slot-1 completion: %+v %v", done, err)
	}
	if status := slotStatus(1); status.Holder != "" {
		t.Fatalf("slot 1 retained after completion: %+v", status)
	}
	if status := slotStatus(0); !status.Held || status.Holder != first.Reservation.Holder {
		t.Fatalf("slot 0 disturbed by slot-1 completion: %+v", status)
	}
}

// An audited steal prefers only the slot whose holder is the exact terminated run.
func TestCoordinatedProofStealPrefersExactTerminatedRun(t *testing.T) {
	if preferTerminatedRun(" ") != nil {
		t.Fatal("empty run id must keep lowest-slot choice")
	}
	run, err := runattempt.Mint(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other, err := runattempt.Mint(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	prefer := preferTerminatedRun(" " + run + " ")
	if !prefer(proofResourceHolder("pfr_fixture", run, "work_fixture", proofActorAlpha)) {
		t.Fatal("exact terminated run holder not preferred")
	}
	for _, holder := range []string{proofResourceHolder("pfr_fixture", other, "work_fixture", proofActorAlpha), "capacity-first", ""} {
		if prefer(holder) {
			t.Fatalf("holder %q preferred for run %s", holder, run)
		}
	}
}
