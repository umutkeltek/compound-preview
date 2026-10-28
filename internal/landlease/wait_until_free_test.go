package landlease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The waiter must wake on the holder's release through the watch, and must NOT take
// the lease: the proof reservation that calls it acquires on its own terms afterward.
func TestWaitUntilFreeWakesOnReleaseWithoutTakingTheLease(t *testing.T) {
	home := t.TempDir()
	if _, err := Acquire(home, laneAlpha, time.Hour, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &fakeClock{t: baseNow}
	fw := &fakeWatcher{onWait: func(time.Duration) error {
		_, err := Release(home, laneAlpha, clk.Now())
		return err
	}}
	factory := func(string) (leaseWatcher, error) { return fw, nil }

	status, err := waitUntilFree(context.Background(), home, clk, 30*time.Second, 25*time.Second, factory)
	if err != nil {
		t.Fatalf("waiter should return once A releases: %v", err)
	}
	if status.Held {
		t.Fatalf("returned status still held: %+v", status)
	}
	if advanced := clk.Now().Sub(baseNow); advanced != 0 {
		t.Fatalf("clock advanced %s: the wake came from the poll, not the watch", advanced)
	}
	after, err := Status(home, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if after.Held {
		t.Fatalf("waiting took the lease: %+v", after)
	}
	if !fw.closed {
		t.Fatal("watcher must be closed on return")
	}
}

func TestWaitUntilFreeTimesOutAndLeavesTheHolder(t *testing.T) {
	home := t.TempDir()
	if _, err := Acquire(home, laneAlpha, time.Hour, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	clk := &countingClock{t: baseNow}
	status, err := waitUntilFree(context.Background(), home, clk, 30*time.Second, 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), laneAlpha) {
		t.Fatalf("want a timeout naming the holder, got status=%+v err=%v", status, err)
	}
	if clk.sleeps != 6 {
		t.Fatalf("slept %d times, want 6 bounded 5s sleeps across the 30s window", clk.sleeps)
	}
	after, err := Status(home, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !after.Held || after.Holder != laneAlpha {
		t.Fatalf("a timed-out wait must not disturb the holder: %+v", after)
	}
}

func TestWaitUntilFreeReturnsAtOnceWhenNothingIsHeld(t *testing.T) {
	clk := &countingClock{t: baseNow}
	status, err := waitUntilFree(context.Background(), t.TempDir(), clk, time.Minute, time.Second, nil)
	if err != nil || status.Held {
		t.Fatalf("free lease: status=%+v err=%v", status, err)
	}
	if clk.sleeps != 0 {
		t.Fatalf("slept %d times on a free lease", clk.sleeps)
	}
}

func TestWaitUntilFreeStopsWhenCancelled(t *testing.T) {
	home := t.TempDir()
	if _, err := Acquire(home, laneAlpha, time.Hour, baseNow); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clk := &countingClock{t: baseNow}
	_, err := waitUntilFree(ctx, home, clk, time.Hour, 5*time.Second, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v, want context.Canceled", err)
	}
	if clk.sleeps != 0 {
		t.Fatalf("cancelled wait slept %d times", clk.sleeps)
	}
}

func TestWaitUntilFreeCancelledVacancy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clk := &countingClock{t: baseNow}
	_, err := waitUntilFree(ctx, t.TempDir(), clk, time.Minute, time.Second, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("free lease admitted cancelled waiter: %v", err)
	}
	if clk.sleeps != 0 {
		t.Fatal("cancelled waiter slept")
	}
}

func TestWaitUntilFreeDeadlineAtRelease(t *testing.T) {
	home := t.TempDir()
	if _, err := Acquire(home, laneAlpha, time.Hour, baseNow); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: baseNow}
	fw := &fakeWatcher{onWait: func(time.Duration) error {
		clk.t = baseNow.Add(30 * time.Second)
		_, err := Release(home, laneAlpha, clk.Now())
		return err
	}}
	factory := func(string) (leaseWatcher, error) { return fw, nil }
	_, err := waitUntilFree(context.Background(), home, clk, 30*time.Second, time.Second, factory)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release at deadline admitted expired waiter: %v", err)
	}
	status, err := StatusReadOnly(home, clk.Now())
	if err != nil || status.Held {
		t.Fatalf("test release did not occur: %+v %v", status, err)
	}
}
