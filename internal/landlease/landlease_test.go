package landlease

import (
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// synthetic holders/reasons — no personal data (public repo).
const (
	laneAlpha = "lane-alpha"
	laneBeta  = "lane-beta"
	laneGamma = "lane-gamma"
)

// baseNow is a fixed injected instant; all tests do TTL/expiry math off this, never the wall clock.
var baseNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeClock is a deterministic Clock for the wait loop: each Sleep advances virtual time by exactly
// the requested poll, so a blocking AcquireWait is testable single-threaded with no real sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time        { return c.t }
func (c *fakeClock) Sleep(d time.Duration) { c.t = c.t.Add(d) }

// Falsifier 1: two DIFFERENT holders cannot hold at once. Acquire(A) then Acquire(B) is refused while
// A's lease is live; after Release(A) Acquire(B) succeeds.
func TestTwoDifferentHoldersCannotHoldAtOnce(t *testing.T) {
	home := t.TempDir()
	ttl := 60 * time.Second

	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	// B is refused while A's lease is live, and told who holds it.
	_, err := Acquire(home, laneBeta, ttl, baseNow.Add(10*time.Second))
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("B acquire on A's live lease must be refused with *HeldError, got %v", err)
	}
	if held.Holder != laneAlpha {
		t.Fatalf("HeldError.Holder = %q, want %q", held.Holder, laneAlpha)
	}

	// After Release(A), B acquires.
	if _, err := Release(home, laneAlpha, baseNow.Add(11*time.Second)); err != nil {
		t.Fatalf("release A: %v", err)
	}
	lease, err := Acquire(home, laneBeta, ttl, baseNow.Add(12*time.Second))
	if err != nil {
		t.Fatalf("B acquire after A released: %v", err)
	}
	if lease.Holder != laneBeta {
		t.Fatalf("after release, holder = %q, want %q", lease.Holder, laneBeta)
	}
}

// Falsifier 2: TTL expiry lets B acquire WITHOUT a steal, and the record is a CLEAN acquire (no steal
// fields). Before expiry B is refused; only crossing expiry flips it.
func TestExpiredLeaseAllowsCleanAcquire(t *testing.T) {
	home := t.TempDir()
	ttl := 30 * time.Second

	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	// One second before expiry: still A's — B refused. Proves it is EXPIRY that flips it, not time alone.
	beforeExpiry := baseNow.Add(29 * time.Second)
	if _, err := Acquire(home, laneBeta, ttl, beforeExpiry); err == nil {
		t.Fatalf("B must be refused while A's lease is still live (1s before expiry)")
	}

	// One second past expiry: B acquires cleanly.
	afterExpiry := baseNow.Add(31 * time.Second)
	lease, err := Acquire(home, laneBeta, ttl, afterExpiry)
	if err != nil {
		t.Fatalf("B acquire after A expired: %v", err)
	}
	if lease.Holder != laneBeta {
		t.Fatalf("holder = %q, want %q", lease.Holder, laneBeta)
	}
	if lease.PriorHolder != "" || lease.StealReason != "" || lease.StolenAt != "" {
		t.Fatalf("expired takeover must be a CLEAN acquire, not a steal: %+v", lease)
	}

	// Status confirms the persisted record carries no steal audit.
	st, err := Status(home, afterExpiry)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.PriorHolder != "" || st.StealReason != "" || st.StolenAt != "" {
		t.Fatalf("status shows steal fields on a clean expired-takeover acquire: %+v", st)
	}
	if !st.Held || st.Holder != laneBeta {
		t.Fatalf("status after clean acquire: held=%v holder=%q", st.Held, st.Holder)
	}
}

// A scarce-resource consumer must be able to distinguish an ordinary free acquire from safe
// stale-owner recovery without racing Status against Acquire. The predecessor is returned from
// the same locked read-modify-write that installs the successor, while the persisted successor
// remains a clean acquire (expiry is not exceptional stealing).
func TestAcquireWithOutcomeReturnsExpiredPredecessorAtomically(t *testing.T) {
	home := t.TempDir()
	ttl := 30 * time.Second

	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("alpha acquire: %v", err)
	}

	outcome, err := AcquireWithOutcome(home, laneBeta, ttl, baseNow.Add(31*time.Second))
	if err != nil {
		t.Fatalf("beta acquire after expiry: %v", err)
	}
	if outcome.Lease.Holder != laneBeta {
		t.Fatalf("successor holder = %q, want %q", outcome.Lease.Holder, laneBeta)
	}
	if outcome.ExpiredPredecessor == nil {
		t.Fatal("expired predecessor missing; stale recovery would be unauditable")
	}
	if outcome.ExpiredPredecessor.Holder != laneAlpha {
		t.Fatalf("expired predecessor holder = %q, want %q", outcome.ExpiredPredecessor.Holder, laneAlpha)
	}
	if outcome.Lease.PriorHolder != "" || outcome.Lease.StealReason != "" || outcome.Lease.StolenAt != "" {
		t.Fatalf("expiry recovery must remain a clean acquire, got %+v", outcome.Lease)
	}
}

func TestPreparedAcquireAndStealPublishOnlyAfterDurabilityBarrier(t *testing.T) {
	t.Run("acquire prepare failure preserves expired predecessor", func(t *testing.T) {
		home := t.TempDir()
		if _, err := Acquire(home, laneAlpha, 30*time.Second, baseNow); err != nil {
			t.Fatal(err)
		}
		prepareErr := errors.New("synthetic reservation persistence fault")
		_, err := AcquireWithOutcomePrepared(
			home,
			laneBeta,
			30*time.Second,
			baseNow.Add(31*time.Second),
			func(outcome AcquireOutcome) error {
				if outcome.ExpiredPredecessor == nil || outcome.ExpiredPredecessor.Holder != laneAlpha {
					t.Fatalf("prepare did not observe exact expired predecessor: %+v", outcome)
				}
				return prepareErr
			},
		)
		if !errors.Is(err, prepareErr) {
			t.Fatalf("prepared acquire error = %v, want %v", err, prepareErr)
		}
		status, err := Status(home, baseNow.Add(31*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if status.Holder != laneAlpha || !status.IsExpired {
			t.Fatalf("prepare failure published successor lease: %+v", status)
		}
	})

	t.Run("steal prepare failure preserves exact live holder", func(t *testing.T) {
		home := t.TempDir()
		if _, err := Acquire(home, laneAlpha, time.Minute, baseNow); err != nil {
			t.Fatal(err)
		}
		prepareErr := errors.New("synthetic governed transfer persistence fault")
		_, err := StealExpectedPrepared(
			home,
			laneBeta,
			laneAlpha,
			"test governed transfer",
			baseNow.Add(time.Second),
			func(lease Lease) error {
				if lease.PriorHolder != laneAlpha || lease.Holder != laneBeta {
					t.Fatalf("prepare did not observe exact transfer: %+v", lease)
				}
				return prepareErr
			},
		)
		if !errors.Is(err, prepareErr) {
			t.Fatalf("prepared steal error = %v, want %v", err, prepareErr)
		}
		status, err := Status(home, baseNow.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !status.Held || status.Holder != laneAlpha {
			t.Fatalf("prepare failure published stolen lease: %+v", status)
		}
	})
}

// Falsifier 3: Steal is recorded and gated. A plain Acquire never transfers A's LIVE lease; only Steal
// does, only with a reason, and it records prior_holder + reason + stolen_at.
func TestStealIsRecordedAndPlainAcquireNeverTransfers(t *testing.T) {
	home := t.TempDir()
	ttl := 120 * time.Second

	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	// A plain Acquire(B) on A's LIVE lease must REFUSE (never a silent transfer).
	_, err := Acquire(home, laneBeta, ttl, baseNow.Add(5*time.Second))
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("plain Acquire on a live lease must refuse, got %v", err)
	}
	// ...and the persisted record still shows A with no steal fields.
	st, err := Status(home, baseNow.Add(5*time.Second))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Holder != laneAlpha || st.PriorHolder != "" || st.StealReason != "" {
		t.Fatalf("a live lease was silently altered by a plain Acquire: %+v", st)
	}

	// Steal transfers to B and records the audit trail.
	reason := "alpha wedged past its window; beta taking the land slot"
	stolen, err := Steal(home, laneBeta, reason, baseNow.Add(6*time.Second))
	if err != nil {
		t.Fatalf("steal: %v", err)
	}
	if stolen.Holder != laneBeta {
		t.Fatalf("stolen holder = %q, want %q", stolen.Holder, laneBeta)
	}
	if stolen.PriorHolder != laneAlpha {
		t.Fatalf("stolen prior_holder = %q, want %q", stolen.PriorHolder, laneAlpha)
	}
	if stolen.StealReason != reason {
		t.Fatalf("stolen steal_reason = %q, want %q", stolen.StealReason, reason)
	}
	if stolen.StolenAt == "" {
		t.Fatalf("steal must record stolen_at")
	}
	// The stolen lease inherited A's TTL.
	if stolen.TTLSeconds != 120 {
		t.Fatalf("stolen ttl = %d, want inherited 120", stolen.TTLSeconds)
	}

	// Steal WITHOUT a reason is refused (the gate).
	if _, err := Steal(home, laneGamma, "   ", baseNow.Add(7*time.Second)); err == nil {
		t.Fatalf("steal without a reason must be refused")
	}
	// The refused steal changed nothing: B still holds.
	st2, err := Status(home, baseNow.Add(7*time.Second))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st2.Holder != laneBeta {
		t.Fatalf("refused reasonless steal altered the lease: holder=%q", st2.Holder)
	}
}

// Falsifier 4: Release is holder-scoped. Release(B) on A's lease is refused (A untouched); Release(A)
// frees it.
func TestReleaseIsHolderScoped(t *testing.T) {
	home := t.TempDir()
	ttl := 60 * time.Second

	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	// Non-holder release is refused and never frees A's lease.
	if _, err := Release(home, laneBeta, baseNow.Add(1*time.Second)); err == nil {
		t.Fatalf("non-holder release must be refused")
	}
	st, err := Status(home, baseNow.Add(1*time.Second))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Held || st.Holder != laneAlpha {
		t.Fatalf("non-holder release freed or altered the lease: %+v", st)
	}

	// Holder release frees it.
	res, err := Release(home, laneAlpha, baseNow.Add(2*time.Second))
	if err != nil {
		t.Fatalf("holder release: %v", err)
	}
	if !res.Freed {
		t.Fatalf("holder release should report Freed=true: %+v", res)
	}
	st2, err := Status(home, baseNow.Add(2*time.Second))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st2.Held {
		t.Fatalf("lease should be free after holder release: %+v", st2)
	}

	// A release with nothing held is an idempotent no-op, not an error.
	res2, err := Release(home, laneBeta, baseNow.Add(3*time.Second))
	if err != nil {
		t.Fatalf("release on empty lease should be a no-op, got %v", err)
	}
	if res2.Freed {
		t.Fatalf("release on empty lease must not report Freed")
	}
}

// A governed proof must persist its terminal evidence while it still owns the scarce resource.
// If that finalizer fails, ReleaseAfter leaves the exact lease in place, so no successor can begin
// against a partially-persisted result.
func TestReleaseAfterKeepsLeaseWhenFinalizerFails(t *testing.T) {
	home := t.TempDir()
	ttl := 60 * time.Second

	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	finalizeErr := errors.New("persist proof result")
	called := false
	if _, err := ReleaseAfter(home, laneAlpha, baseNow.Add(time.Second), func() error {
		called = true
		return finalizeErr
	}); !errors.Is(err, finalizeErr) {
		t.Fatalf("ReleaseAfter error = %v, want %v", err, finalizeErr)
	}
	if !called {
		t.Fatal("ReleaseAfter did not invoke finalizer")
	}
	status, err := Status(home, baseNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Held || status.Holder != laneAlpha {
		t.Fatalf("failed finalizer released or transferred lease: %+v", status)
	}
	if _, err := Acquire(home, laneBeta, ttl, baseNow.Add(2*time.Second)); err == nil {
		t.Fatal("successor acquired after failed finalizer")
	}

	released, err := ReleaseAfter(home, laneAlpha, baseNow.Add(3*time.Second), func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("successful ReleaseAfter: %v", err)
	}
	if !released.Freed {
		t.Fatalf("successful ReleaseAfter = %+v, want Freed", released)
	}
	if _, err := Acquire(home, laneBeta, ttl, baseNow.Add(4*time.Second)); err != nil {
		t.Fatalf("successor after finalized release: %v", err)
	}
}

// AcquireWait (bounded-poll fallback, nil watcher): A holds; the injected clock advances past A's expiry
// during the poll loop, and B acquires CLEANLY (no steal) within the timeout. This exercises the SAFETY-NET
// path deterministically (a nil factory ⇒ no OS watch, pure clock-driven poll) — the same path a platform
// with no supported watcher, or a failed watcher build, degrades to. The real watch fast path is proven by
// TestAcquireWaitWatchWakesInstantly* below.
func TestRearmExpiredExactRequiresUnchangedHolderAndTerminatedExecution(t *testing.T) {
	t.Run("exact expired holder", func(t *testing.T) {
		home := t.TempDir()
		acquired, barrier, err := AcquireWithExecutionBarrierPrepared(
			home,
			laneAlpha,
			time.Second,
			baseNow,
			func(AcquireOutcome) error { return nil },
		)
		if err != nil {
			t.Fatal(err)
		}
		status, err := Status(home, baseNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !status.IsExpired || status.Holder != acquired.Lease.Holder {
			t.Fatalf("expired status = %+v", status)
		}
		if _, err := RearmExpiredExact(home, status, baseNow.Add(2*time.Second)); err == nil {
			t.Fatal("rearmed expired holder while execution barrier was still pending")
		}
		if err := barrier.Close(); err != nil {
			t.Fatal(err)
		}

		rearmed, err := RearmExpiredExact(home, status, baseNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if rearmed.Holder != laneAlpha || !rearmed.live(baseNow.Add(2*time.Second)) {
			t.Fatalf("rearmed lease = %+v", rearmed)
		}
	})

	t.Run("replacement refused", func(t *testing.T) {
		home := t.TempDir()
		if _, err := Acquire(home, laneAlpha, time.Second, baseNow); err != nil {
			t.Fatal(err)
		}
		expired, err := Status(home, baseNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Acquire(home, laneBeta, time.Minute, baseNow.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := RearmExpiredExact(home, expired, baseNow.Add(2*time.Second)); err == nil {
			t.Fatal("rearmed replaced expired holder")
		}
		current, err := Status(home, baseNow.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !current.Held || current.Holder != laneBeta {
			t.Fatalf("replacement changed after refused rearm: %+v", current)
		}
	})

	t.Run("status snapshot mutations refused", func(t *testing.T) {
		home := t.TempDir()
		if _, err := Acquire(home, laneAlpha, time.Second, baseNow); err != nil {
			t.Fatal(err)
		}
		expiredAt := baseNow.Add(2 * time.Second)
		expected, err := Status(home, expiredAt)
		if err != nil {
			t.Fatal(err)
		}
		mutations := []struct {
			name   string
			mutate func(*StatusResult)
		}{
			{name: "schema", mutate: func(s *StatusResult) { s.SchemaVersion = "changed" }},
			{name: "holder", mutate: func(s *StatusResult) { s.Holder = laneGamma }},
			{name: "acquired", mutate: func(s *StatusResult) { s.AcquiredAt = baseNow.Add(time.Second).Format(time.RFC3339) }},
			{name: "expires", mutate: func(s *StatusResult) { s.ExpiresAt = baseNow.Add(3 * time.Second).Format(time.RFC3339) }},
			{name: "ttl", mutate: func(s *StatusResult) { s.TTLSeconds++ }},
			{name: "prior holder", mutate: func(s *StatusResult) { s.PriorHolder = laneBeta }},
			{name: "steal reason", mutate: func(s *StatusResult) { s.StealReason = "changed" }},
			{name: "stolen at", mutate: func(s *StatusResult) { s.StolenAt = baseNow.Format(time.RFC3339) }},
			{name: "termination requester", mutate: func(s *StatusResult) { s.TerminationRequestedBy = laneBeta }},
			{name: "termination reason", mutate: func(s *StatusResult) { s.TerminationRequestReason = "changed" }},
			{name: "termination time", mutate: func(s *StatusResult) { s.TerminationRequestedAt = baseNow.Format(time.RFC3339Nano) }},
			{name: "held", mutate: func(s *StatusResult) { s.Held = true }},
			{name: "expired", mutate: func(s *StatusResult) { s.IsExpired = false }},
			{name: "remaining", mutate: func(s *StatusResult) { s.RemainingSeconds = 1 }},
		}
		for _, tc := range mutations {
			t.Run(tc.name, func(t *testing.T) {
				changed := expected
				tc.mutate(&changed)
				if _, err := RearmExpiredExact(home, changed, expiredAt); err == nil {
					t.Fatal("rearmed from changed status snapshot")
				}
				current, err := Status(home, expiredAt)
				if err != nil {
					t.Fatal(err)
				}
				want, _ := json.Marshal(expected)
				got, _ := json.Marshal(current)
				if string(got) != string(want) {
					t.Fatalf("refused rearm mutated lease: got %s want %s", got, want)
				}
			})
		}
	})
}

func TestAcquireWaitAcquiresAfterHolderExpires(t *testing.T) {
	home := t.TempDir()
	ttl := 10 * time.Second

	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &fakeClock{t: baseNow}
	lease, err := acquireWait(home, laneBeta, ttl, clk, 60*time.Second, 2*time.Second, nil)
	if err != nil {
		t.Fatalf("AcquireWait should succeed once A expires within the timeout: %v", err)
	}
	if lease.Holder != laneBeta {
		t.Fatalf("waiter holder = %q, want %q", lease.Holder, laneBeta)
	}
	if lease.PriorHolder != "" || lease.StealReason != "" {
		t.Fatalf("waiter should CLEAN-acquire an expired lease, not steal: %+v", lease)
	}
}

// AcquireWait (bounded-poll fallback, nil watcher): A holds far longer than the wait window; B times out
// instead of stealing or blocking forever — the bound guarantees the fallback can never hang past timeout.
func TestAcquireWaitTimesOut(t *testing.T) {
	home := t.TempDir()
	ttl := 3600 * time.Second // A holds for an hour

	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &fakeClock{t: baseNow}
	_, err := acquireWait(home, laneBeta, ttl, clk, 30*time.Second, 5*time.Second, nil)
	if err == nil {
		t.Fatalf("AcquireWait must time out while A holds well past the wait window")
	}
	// A still holds — the timed-out waiter changed nothing.
	st, err := Status(home, clk.Now())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Holder != laneAlpha {
		t.Fatalf("a timed-out wait must not disturb the holder: %+v", st)
	}
}

// Same-holder Acquire is an idempotent refresh that extends the window without a steal.
func TestSameHolderAcquireRefreshesWindow(t *testing.T) {
	home := t.TempDir()
	ttl := 60 * time.Second

	first, err := Acquire(home, laneAlpha, ttl, baseNow)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	refreshed, err := Acquire(home, laneAlpha, ttl, baseNow.Add(30*time.Second))
	if err != nil {
		t.Fatalf("same-holder refresh must succeed: %v", err)
	}
	if refreshed.PriorHolder != "" || refreshed.StealReason != "" {
		t.Fatalf("refresh must be a clean record, not a steal: %+v", refreshed)
	}
	firstExp, _ := time.Parse(time.RFC3339, first.ExpiresAt)
	refExp, _ := time.Parse(time.RFC3339, refreshed.ExpiresAt)
	if !refExp.After(firstExp) {
		t.Fatalf("refresh should extend expiry: first=%s refreshed=%s", first.ExpiresAt, refreshed.ExpiresAt)
	}
}

// The persisted lease is well-formed JSON with the schema version and derived fields consistent.
func TestPersistedLeaseShape(t *testing.T) {
	home := t.TempDir()
	if _, err := Acquire(home, laneAlpha, 45*time.Second, baseNow); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "state", "land-lease.json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		t.Fatalf("state must be valid JSON: %v", err)
	}
	if lease.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %q, want %q", lease.SchemaVersion, SchemaVersion)
	}
	if lease.TTLSeconds != 45 {
		t.Fatalf("ttl_seconds = %d, want 45", lease.TTLSeconds)
	}
	// expires_at must equal acquired_at + ttl_seconds (they can never disagree).
	acq, err := time.Parse(time.RFC3339, lease.AcquiredAt)
	if err != nil {
		t.Fatalf("acquired_at not RFC3339: %v", err)
	}
	exp, err := time.Parse(time.RFC3339, lease.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at not RFC3339: %v", err)
	}
	if got := exp.Sub(acq); got != 45*time.Second {
		t.Fatalf("expires_at - acquired_at = %s, want 45s", got)
	}
}

// ---- INSTANT-NOTIFY-TAP falsifiers (watch-then-retry with a bounded-poll safety net) ----

// fakeWatcher is a deterministic leaseWatcher: wait invokes an injected hook so a single-threaded test can
// model an event (return without advancing the clock ⇒ instant wake), a timeout (advance the clock by the
// bound ⇒ the bounded wait elapsed), or a broken watch (return an error ⇒ the loop must degrade to poll).
type fakeWatcher struct {
	waits  int
	closed bool
	onWait func(bound time.Duration) error
}

func (w *fakeWatcher) wait(bound time.Duration) error {
	w.waits++
	if w.onWait != nil {
		return w.onWait(bound)
	}
	return nil
}
func (w *fakeWatcher) close() error { w.closed = true; return nil }

// countingClock is a fakeClock that counts Sleep calls, so a test can prove the wait BLOCKS a bounded
// number of times (no busy-spin, no sub-poll tight loop) rather than spinning.
type countingClock struct {
	t      time.Time
	sleeps int
}

func (c *countingClock) Now() time.Time { return c.t }
func (c *countingClock) Sleep(d time.Duration) {
	c.sleeps++
	c.t = c.t.Add(d)
}

// Falsifier 1 (INSTANT WAKE — deterministic): a waiter blocked on a lease that A holds for an HOUR (so
// expiry can never be the cause) acquires the instant the watch fires on A's Release — WITHOUT the poll
// bound elapsing. The fake watcher models the holder releasing while the waiter blocks, then returns
// (event, not timeout) without advancing the clock. If the loop ignored the watcher, onWait would never
// run, A would hold the full hour, and the waiter would time out — so this fails RED without the tap.
func TestAcquireWaitWatchWakesInstantly(t *testing.T) {
	home := t.TempDir()
	ttl := 3600 * time.Second // A holds for an hour: expiry cannot be the wake cause
	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &fakeClock{t: baseNow}
	fw := &fakeWatcher{}
	released := false
	fw.onWait = func(bound time.Duration) error {
		// Model the holder releasing WHILE we block: the watch fires immediately (no clock advance).
		if !released {
			if _, err := Release(home, laneAlpha, clk.Now()); err != nil {
				return err
			}
			released = true
		}
		return nil
	}
	factory := func(stateDir string) (leaseWatcher, error) { return fw, nil }

	// Poll bound is 25s and timeout 30s: if the wake came from the poll (not the watch), the clock would
	// have advanced by at least one 25s bound.
	start := clk.Now()
	lease, err := acquireWait(home, laneBeta, ttl, clk, 30*time.Second, 25*time.Second, factory)
	if err != nil {
		t.Fatalf("waiter should wake and acquire the instant the watch fires: %v", err)
	}
	if advanced := clk.Now().Sub(start); advanced >= 25*time.Second {
		t.Fatalf("waiter waited the full %s poll bound (clock advanced %s) — the watch did NOT wake it", 25*time.Second, advanced)
	}
	if lease.Holder != laneBeta {
		t.Fatalf("waiter holder = %q, want %q", lease.Holder, laneBeta)
	}
	if lease.PriorHolder != "" || lease.StealReason != "" {
		t.Fatalf("a released lease must be CLEAN-acquired, not stolen: %+v", lease)
	}
	if fw.waits != 1 {
		t.Fatalf("watch should have been consulted exactly once, got %d", fw.waits)
	}
	if !fw.closed {
		t.Fatalf("watcher must be closed on return")
	}
}

// Falsifier 1 (INSTANT WAKE — REAL OS watch, real timing): the headline claim proven end-to-end through
// the EXPORTED AcquireWait with RealClock and a real kqueue/inotify watcher. A holds for an hour; a
// goroutine Releases after a short real delay; the waiter — with a DELIBERATELY HIGH poll (30s, larger
// than the whole test budget) — must acquire within a few hundred ms. Because the poll bound is 30s, a
// sub-second wake can ONLY come from the OS watch, not the next poll tick. This is the falsifier that
// would go RED if AcquireWait still slept a fixed interval.
func TestAcquireWaitWatchWakesInstantlyIntegration(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("real file-watch integration test needs kqueue/inotify; GOOS=%s degrades to poll", runtime.GOOS)
	}
	home := t.TempDir()
	ttl := 3600 * time.Second // A holds for an hour: expiry cannot be the wake cause
	if _, err := Acquire(home, laneAlpha, ttl, time.Now()); err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	const releaseAfter = 150 * time.Millisecond
	const highPoll = 30 * time.Second // >> releaseAfter, so a fast wake must be the watch, not the poll
	go func() {
		time.Sleep(releaseAfter)
		if _, err := Release(home, laneAlpha, time.Now()); err != nil {
			t.Errorf("background release: %v", err)
		}
	}()

	start := time.Now()
	lease, err := AcquireWait(home, laneBeta, ttl, RealClock{}, 30*time.Second, highPoll)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waiter should acquire soon after the real Release: %v (elapsed %s)", err, elapsed)
	}
	if lease.Holder != laneBeta {
		t.Fatalf("waiter holder = %q, want %q", lease.Holder, laneBeta)
	}
	if lease.PriorHolder != "" || lease.StealReason != "" {
		t.Fatalf("a released lease must be CLEAN-acquired, not stolen: %+v", lease)
	}
	// The wake must beat the poll interval by a wide margin: releaseAfter=150ms, poll=30s. A generous 5s
	// ceiling still cleanly separates "watch woke it (~150ms)" from "poll woke it (~30s)".
	if elapsed >= 5*time.Second {
		t.Fatalf("waiter took %s — with a %s poll, only the OS watch can wake it fast; it appears to have polled", elapsed, highPoll)
	}
	t.Logf("instant wake: acquired %s after start (release at %s, poll bound %s) — watch beat the poll", elapsed, releaseAfter, highPoll)
}

// Falsifier 2a (FALLBACK when watcher build FAILS): a factory that returns an error must degrade cleanly
// to the clock-driven bounded poll and still acquire after A expires — never hang. Proves the watch is an
// optimization, not the sole liveness.
func TestAcquireWaitDegradesToPollWhenWatcherUnavailable(t *testing.T) {
	home := t.TempDir()
	ttl := 10 * time.Second
	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &fakeClock{t: baseNow}
	factoryCalls := 0
	factory := func(stateDir string) (leaseWatcher, error) {
		factoryCalls++
		return nil, errors.New("watch unavailable on this box")
	}
	lease, err := acquireWait(home, laneBeta, ttl, clk, 60*time.Second, 2*time.Second, factory)
	if err != nil {
		t.Fatalf("must degrade to poll and acquire after A expires: %v", err)
	}
	if lease.Holder != laneBeta {
		t.Fatalf("waiter holder = %q, want %q", lease.Holder, laneBeta)
	}
	if factoryCalls != 1 {
		t.Fatalf("factory should be tried exactly once (lazily, on first contention), got %d", factoryCalls)
	}
}

// Falsifier 2b (FALLBACK when watcher breaks MID-FLIGHT): a watcher whose wait errors must be dropped and
// the loop must fall back to the clock poll for the rest of the wait — still acquiring after A expires,
// never hanging on the broken watch.
func TestAcquireWaitWatcherErrorMidFlightFallsBackToPoll(t *testing.T) {
	home := t.TempDir()
	ttl := 10 * time.Second
	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &fakeClock{t: baseNow}
	fw := &fakeWatcher{onWait: func(bound time.Duration) error { return errors.New("watch broke mid-flight") }}
	factory := func(stateDir string) (leaseWatcher, error) { return fw, nil }

	lease, err := acquireWait(home, laneBeta, ttl, clk, 60*time.Second, 2*time.Second, factory)
	if err != nil {
		t.Fatalf("a broken watcher must fall back to poll and still acquire: %v", err)
	}
	if lease.Holder != laneBeta {
		t.Fatalf("waiter holder = %q, want %q", lease.Holder, laneBeta)
	}
	if fw.waits != 1 {
		t.Fatalf("a broken watcher should be consulted once then dropped, got %d waits", fw.waits)
	}
	if !fw.closed {
		t.Fatalf("a dropped watcher must be closed")
	}
}

// The watch fast path ALSO honors the clock-injected bound: a fake watcher that models a pure timeout
// (advance the clock by the bound, no event) must still terminate deterministically and acquire once A's
// virtual expiry passes. Proves the bound is real even on the watch path (belt-and-suspenders no-hang).
func TestAcquireWaitWatchPathRespectsBound(t *testing.T) {
	home := t.TempDir()
	ttl := 10 * time.Second
	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &fakeClock{t: baseNow}
	fw := &fakeWatcher{onWait: func(bound time.Duration) error {
		clk.t = clk.t.Add(bound) // model "the bound elapsed with no event"
		return nil
	}}
	factory := func(stateDir string) (leaseWatcher, error) { return fw, nil }

	lease, err := acquireWait(home, laneBeta, ttl, clk, 60*time.Second, 2*time.Second, factory)
	if err != nil {
		t.Fatalf("watch path must still terminate via the bound and acquire after expiry: %v", err)
	}
	if lease.Holder != laneBeta {
		t.Fatalf("waiter holder = %q, want %q", lease.Holder, laneBeta)
	}
	if fw.waits == 0 {
		t.Fatalf("the watch path should have been used")
	}
}

// Falsifier 3a (NO BUSY-SPIN): a timed-out fallback wait must BLOCK a BOUNDED number of times
// (~ceil(timeout/poll)), never tight-loop. A busy-spin would either never Sleep (sleeps==0) or Sleep far
// more than the bound implies.
func TestAcquireWaitFallbackDoesNotBusySpin(t *testing.T) {
	home := t.TempDir()
	ttl := 3600 * time.Second
	if _, err := Acquire(home, laneAlpha, ttl, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &countingClock{t: baseNow}
	if _, err := acquireWait(home, laneBeta, ttl, clk, 30*time.Second, 5*time.Second, nil); err == nil {
		t.Fatalf("should time out while A holds for an hour")
	}
	if clk.sleeps == 0 {
		t.Fatalf("the wait never blocked — that is a busy-spin")
	}
	// 30s timeout / 5s poll ⇒ ~6 bounded sleeps; allow a small margin. Thousands would be a spin.
	if clk.sleeps > 8 {
		t.Fatalf("slept %d times for a 30s/5s wait — expected ~ceil(timeout/poll); possible sub-poll spin", clk.sleeps)
	}
}

// Falsifier 3b (NO NETWORK): the landlease package (including the platform watch files) must import NO
// network package — the coordination hint is pure local file+syscall I/O, never a provider/LLM/net call.
func TestNoNetworkImports(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	forbidden := []string{"net", "net/http", "net/url", "net/rpc", "crypto/tls"}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// ImportsOnly parses regardless of build tags, so watch_darwin.go / watch_linux.go /
		// watch_other.go are all covered even though only one compiles on this GOOS.
		f, perr := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		checked++
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad {
					t.Fatalf("%s imports forbidden network package %q — land-lease is a local coordination hint, no net call", name, path)
				}
			}
		}
	}
	if checked < 4 {
		t.Fatalf("expected to scan landlease.go + the 3 platform watch files, only scanned %d", checked)
	}
}
