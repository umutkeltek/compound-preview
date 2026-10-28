package runattempt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRunAttemptLockUsesCanonicalHabitat(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	ra, err := store.Mint("work_lock_habitat", "codex", "claude", time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "lock", "runattempts", ra.ID+".lock")); err != nil {
		t.Fatalf("canonical RunAttempt lock is absent: %v", err)
	}
	legacy, err := os.Stat(filepath.Join(home, "lock", "runattempts.lock"))
	if err != nil {
		t.Fatalf("v0.1 legacy compatibility fence is absent: %v", err)
	}
	if legacy.Mode().Perm() != 0o600 {
		t.Fatalf("legacy compatibility fence mode = %04o, want 0600", legacy.Mode().Perm())
	}
	perAttemptDir, err := os.Stat(filepath.Join(home, "lock", "runattempts"))
	if err != nil {
		t.Fatal(err)
	}
	if perAttemptDir.Mode().Perm() != 0o700 {
		t.Fatalf("per-attempt lock directory mode = %04o, want 0700", perAttemptDir.Mode().Perm())
	}
	perAttempt, err := os.Stat(filepath.Join(home, "lock", "runattempts", ra.ID+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	if perAttempt.Mode().Perm() != 0o600 {
		t.Fatalf("per-attempt lock mode = %04o, want 0600", perAttempt.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(home, "locks")); !os.IsNotExist(err) {
		t.Fatalf("RunAttempt mutation materialized legacy locks habitat: %v", err)
	}
}

func TestConcurrentMintWithIDAndEvidenceSettersDoNotOverwriteCurrentRecord(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		home := t.TempDir()
		now := time.Date(2026, 8, 4, 8, 0, iteration, 0, time.UTC)
		id := MintDeterministic(fmt.Sprintf("cross-store-mint-%d", iteration), now)
		stores := []*Store{Open(home), Open(home)}
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := stores[0].MintWithID(id, "work_deterministic", "codex", "claude", now); err != nil {
				errs <- err
				return
			}
			_, err := stores[0].SetTranscript(id, "transcript://deterministic")
			errs <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := stores[1].MintWithID(id, "work_deterministic", "codex", "claude", now); err != nil {
				errs <- err
				return
			}
			_, err := stores[1].SetProofRefs(id, []string{"proof://deterministic"})
			errs <- err
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("deterministic mint iteration %d: %v", iteration, err)
			}
		}
		got, err := Open(home).Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.TranscriptRef != "transcript://deterministic" || !reflect.DeepEqual(got.ProofRefs, []string{"proof://deterministic"}) {
			t.Fatalf("deterministic mint overwrote current evidence at iteration %d: transcript=%q proof=%v", iteration, got.TranscriptRef, got.ProofRefs)
		}
	}
}

func TestAttachActionIntentIsImmutableAcrossIndependentStores(t *testing.T) {
	home := t.TempDir()
	seed := Open(home)
	ra, err := seed.Mint("work_cross_store", "codex", "claude", time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	stale := Open(home)
	if _, err := stale.Get(ra.ID); err != nil {
		t.Fatal(err)
	}
	if err := Open(home).AttachActionIntent(ra.ID, "act_first"); err != nil {
		t.Fatal(err)
	}
	if err := stale.AttachActionIntent(ra.ID, "act_second"); !errors.Is(err, ErrAlreadyBound) {
		t.Fatalf("stale cross-store rebind error = %v, want ErrAlreadyBound", err)
	}
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActionIntentID != "act_first" {
		t.Fatalf("persisted action_intent_id = %q, want immutable first bind", got.ActionIntentID)
	}
}

func TestConcurrentIndependentStoreMutationsPreserveHeterogeneousEvidence(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		home := t.TempDir()
		seed := Open(home)
		ra, err := seed.Mint("work_cross_store", "codex", "claude", time.Date(2026, 8, 4, 8, 0, iteration, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		stores := []*Store{Open(home), Open(home), Open(home)}
		start := make(chan struct{})
		errs := make(chan error, len(stores))
		var wg sync.WaitGroup
		wg.Add(len(stores))
		go func() {
			defer wg.Done()
			<-start
			_, err := stores[0].SetTranscript(ra.ID, "transcript://cross-store")
			errs <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := stores[1].SetProofRefs(ra.ID, []string{"proof://cross-store"})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := stores[2].HeartbeatProgress(ra.ID, time.Date(2026, 8, 4, 8, 1, iteration, 0, time.UTC), RunProgress{Done: 1, Remaining: 2})
			errs <- err
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent mutation iteration %d: %v", iteration, err)
			}
		}

		got, err := Open(home).Get(ra.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.TranscriptRef != "transcript://cross-store" || !reflect.DeepEqual(got.ProofRefs, []string{"proof://cross-store"}) ||
			got.Progress == nil || *got.Progress != (RunProgress{Done: 1, Remaining: 2}) {
			t.Fatalf("heterogeneous evidence lost at iteration %d: %s", iteration, fmt.Sprintf("transcript=%q proof=%v progress=%+v", got.TranscriptRef, got.ProofRefs, got.Progress))
		}
	}
}

func TestStaleIndependentStoreSetterCannotResurrectTerminalAttempt(t *testing.T) {
	home := t.TempDir()
	seed := Open(home)
	ra, err := seed.Mint("work_terminal_race", "codex", "claude", time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	stale := Open(home)
	if _, err := stale.Get(ra.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home).Complete(ra.ID, StatusFailed, time.Date(2026, 8, 4, 8, 1, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.SetTranscript(ra.ID, "transcript://too-late"); !errors.Is(err, terminalRefusal) {
		t.Fatalf("stale setter after terminalization error = %v, want terminal refusal", err)
	}
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.TranscriptRef != "" {
		t.Fatalf("terminal attempt was resurrected or rewritten: status=%s transcript=%q", got.Status, got.TranscriptRef)
	}
}

func TestStaleIndependentStoreCompletePreservesNewerEvidence(t *testing.T) {
	home := t.TempDir()
	seed := Open(home)
	ra, err := seed.Mint("work_terminal_race", "codex", "claude", time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	stale := Open(home)
	if _, err := stale.Get(ra.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home).SetTranscript(ra.ID, "transcript://preserve-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Complete(ra.ID, StatusSucceeded, time.Date(2026, 8, 4, 8, 1, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || got.TranscriptRef != "transcript://preserve-me" {
		t.Fatalf("stale completion lost newer evidence: status=%s transcript=%q", got.Status, got.TranscriptRef)
	}
}
