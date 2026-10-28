package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

// I1 falsifiers: a lease must not outlive its work's terminal state.
//
// Before this slice the ONLY release path was the archive sweep
// (internal/home/archive.go staleLeaseCandidates), so every close left residue
// behind and work/leases grew without bound until an unrelated consumer broke.
// These tests fail against that behavior and pass only when close itself
// releases the lease.

// Falsifiers 1, 2 and 4 in one governed close: the closed work's lease file is
// gone, the hygiene doctor no longer names it, and a concurrently claimed LIVE
// work keeps its lease untouched (no over-broad deletion).
func TestCloseReleasesClaimLeaseAndLeavesLiveLeaseUntouched(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)
	homePath := os.Getenv("COMPOUND_HOME")

	closed, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "closed work releases its lease"}, now)
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "live claimed work keeps its lease"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(live.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	liveLease := filepath.Join(homePath, "work", "leases", live.ID+".json")
	closedLease := filepath.Join(homePath, "work", "leases", closed.ID+".json")

	closeWorkbenchTestWork(t, store, closed.ID, now, "closed with proof")

	// Falsifier 1: the lease is absent after close.
	if _, err := os.Stat(closedLease); !os.IsNotExist(err) {
		t.Fatalf("closed work lease must be released by close: stat err = %v", err)
	}
	// Falsifier 4: a lease for LIVE work is untouched.
	if _, err := os.Stat(liveLease); err != nil {
		t.Fatalf("live work lease must survive an unrelated close: %v", err)
	}
	if lease, err := store.readLease(live.ID); err != nil || lease.By != "codex" {
		t.Fatalf("live lease must be byte-intact: lease=%+v err=%v", lease, err)
	}

	// Falsifier 2: the hygiene report does not name the closed work.
	hygiene, err := home.OperationalHygiene(homePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range hygiene.StaleLeases {
		if strings.HasSuffix(stale, "/"+closed.ID+".json") {
			t.Fatalf("closed work must not appear in StaleLeases: %v", hygiene.StaleLeases)
		}
	}

	// The store's own lease projection agrees with the filesystem.
	leases, err := store.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].WorkID != live.ID {
		t.Fatalf("ListLeases should hold only the live work: %+v", leases)
	}
}

// Falsifier 1 for the other two terminal outcomes: release is a property of
// terminality, not of the "completed" happy path.
func TestCloseReleasesClaimLeaseForEveryTerminalOutcome(t *testing.T) {
	cases := []struct {
		outcome string
		via     []string // legal intermediate advances (claimed -> ... -> outcome)
	}{
		{outcome: "failed", via: []string{"running"}},
		{outcome: "cancelled"},
	}
	for _, tc := range cases {
		outcome := tc.outcome
		t.Run(outcome, func(t *testing.T) {
			now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
			store := newExecutorTestStore(t, now)
			homePath := os.Getenv("COMPOUND_HOME")

			w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "terminal " + outcome}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			for i, to := range tc.via {
				if _, err := store.Advance(w.ID, AdvanceInput{To: to}, now.Add(time.Duration(i+2)*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Close(w.ID, CloseInput{
				Outcome:  outcome,
				MoveType: "audit_only",
				Summary:  "closed " + outcome,
			}, now.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(homePath, "work", "leases", w.ID+".json")); !os.IsNotExist(err) {
				t.Fatalf("close --outcome %s must release the lease: stat err = %v", outcome, err)
			}
		})
	}
}

// Falsifier 4 (negative half): a close that is REFUSED must not release the
// lease. The holder gate denies a non-holder close without --steal; the lease
// is the thing doing the denying, so releasing it there would disarm the gate.
func TestRefusedCloseKeepsLease(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)
	homePath := os.Getenv("COMPOUND_HOME")

	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "refused close keeps the lease"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Close(w.ID, CloseInput{
		Actor:    "stranger",
		Outcome:  "completed",
		MoveType: "audit_only",
		Summary:  "not the holder",
	}, now.Add(2*time.Minute)); err == nil {
		t.Fatal("non-holder close without --steal must be refused")
	}
	if _, err := os.Stat(filepath.Join(homePath, "work", "leases", w.ID+".json")); err != nil {
		t.Fatalf("a refused close must leave the lease in place: %v", err)
	}
	current, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "claimed" {
		t.Fatalf("refused close must leave the work claimed, got %q", current.State)
	}
}

// Falsifier 3: crash/interrupt safety consistent with the package's staged
// publish posture. The release happens strictly AFTER the work envelope is
// durably terminal, so the only observable crash residue is the pair the
// archive sweep already backstops (terminal work + leftover lease). The
// forbidden inverse — a lease released while the work is still live/claimed —
// must be unreachable, because that silently disarms the close/advance holder
// gate for whoever runs next.
func TestCloseCrashBeforeLeaseReleaseLeavesSweepableResidueNeverALiveWorkWithNoLease(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)
	homePath := os.Getenv("COMPOUND_HOME")

	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "crash in the release window"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(homePath, "work", "leases", w.ID+".json")

	previous := closeBeforeLeaseRelease
	t.Cleanup(func() { closeBeforeLeaseRelease = previous })
	closeBeforeLeaseRelease = func() { panic("crash: close interrupted before lease release") }

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the injected crash seam to fire")
			}
		}()
		_, _ = store.Close(w.ID, CloseInput{
			Outcome:  "cancelled",
			MoveType: "audit_only",
			Summary:  "crash window",
		}, now.Add(2*time.Minute))
	}()
	closeBeforeLeaseRelease = previous

	// The work is durably terminal on disk — the crash is AFTER the point of
	// no return, never before it.
	reopened, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	crashed, err := reopened.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !terminalStates[crashed.State] {
		t.Fatalf("close must publish the terminal WorkEnvelope before releasing the lease, state = %q", crashed.State)
	}
	// The forbidden inverse never happens: the lease is not released ahead of
	// the terminal publish.
	if _, err := os.Stat(leasePath); err != nil {
		t.Fatalf("crash residue must be a leftover lease, not a missing one: %v", err)
	}

	// And the residue is exactly what the sweep already backstops.
	hygiene, err := home.OperationalHygiene(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if !hygieneNamesLease(hygiene, w.ID) {
		t.Fatalf("crash residue should be reported stale: %+v", hygiene.StaleLeases)
	}
	report, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"lease"}}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !sweepListsLease(report, w.ID) {
		t.Fatalf("orphan backstop should archive the crash residue: %+v", report)
	}
	if _, err := os.Stat(leasePath); !os.IsNotExist(err) {
		t.Fatalf("sweep should have archived the crash residue: %v", err)
	}
}

// Releasing a lease that was never taken is a no-op, not an error: close has
// always been legal for work that was never claimed.
func TestCloseWithoutLeaseIsUnaffected(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)

	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "never claimed"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, AdvanceInput{To: "ready"}, now.Add(time.Minute)); err != nil && w.State != "ready" {
		t.Fatal(err)
	}
	res, err := store.Close(w.ID, CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Summary:  "never claimed, closed anyway",
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("close of never-claimed work must still succeed: %v", err)
	}
	if res.Work.State != "cancelled" {
		t.Fatalf("close outcome not applied: %+v", res.Work)
	}
}

// A close-gate steal transfers the lease to the stealer and then closes — the
// transferred lease must be released too, or a steal would reintroduce exactly
// the residue this slice removes.
func TestStolenCloseAlsoReleasesTheTransferredLease(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)
	homePath := os.Getenv("COMPOUND_HOME")

	w, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "stolen close releases the lease"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Close(w.ID, CloseInput{
		Actor:       "operator",
		Outcome:     "cancelled",
		MoveType:    "audit_only",
		Summary:     "stolen and closed",
		Steal:       true,
		StealReason: "holder lane is dead",
	}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "work", "leases", w.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("a stolen close must release the transferred lease: stat err = %v", err)
	}
}

func hygieneNamesLease(report home.OperationalHygieneReport, workID string) bool {
	for _, stale := range report.StaleLeases {
		if strings.HasSuffix(stale, "/"+workID+".json") {
			return true
		}
	}
	return false
}
