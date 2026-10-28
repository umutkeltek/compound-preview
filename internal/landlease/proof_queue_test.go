package landlease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func proofQueueFixture(t *testing.T) *proofQueue {
	t.Helper()
	q, err := newProofQueue(t.TempDir(), strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return baseNow }
	return q
}
func queueFixtureTicket(t *testing.T, q *proofQueue, class ProofCapacityClass, repo string, deadline time.Time) *ProofTicket {
	t.Helper()
	identity, err := NewProofCoordinationIdentity(q.generation, class, strings.Repeat(repo, 64))
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := q.enqueue(identity, deadline)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ticket.Cancel(); err != nil {
			t.Error(err)
		}
	})
	return ticket
}
func TestProofQueueConflictFIFOAllowsIndependentClass(t *testing.T) {
	q := proofQueueFixture(t)
	older := queueFixtureTicket(t, q, ProofCapacitySystemFull, "2", baseNow.Add(time.Hour))
	sameClass := queueFixtureTicket(t, q, ProofCapacitySystemFull, "3", baseNow.Add(time.Hour))
	sameRepo := queueFixtureTicket(t, q, ProofCapacityNativeConsumer, "2", baseNow.Add(time.Hour))
	independent := queueFixtureTicket(t, q, ProofCapacityNativeConsumer, "4", baseNow.Add(time.Hour))
	for _, ticket := range []*ProofTicket{sameClass, sameRepo} {
		called := false
		err := q.withTurn(context.Background(), ticket, func() error { called = true; return nil })
		var busy *ProofQueueBusyError
		if !errors.As(err, &busy) || busy.EarlierTicket != older.ID() || called {
			t.Fatalf("younger conflicting ticket overtook older: %v called=%v", err, called)
		}
	}
	// Independent native work may pass the system head, but not the earlier
	// native waiter. Cancel that waiter before exercising independence.
	if err := sameRepo.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := q.withTurn(context.Background(), independent, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := older.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := q.withTurn(context.Background(), sameClass, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// A lease image that cannot be read keeps the later ticket waiting: the
// occupancy check fails closed and the callback never runs.
func TestProofQueueOccupancyReadErrorFailsClosed(t *testing.T) {
	backing := t.TempDir()
	q, err := newProofQueue(backing, strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return baseNow }
	queueFixtureTicket(t, q, ProofCapacitySystemFull, "2", baseNow.Add(time.Hour))
	later := queueFixtureTicket(t, q, ProofCapacitySystemFull, "3", baseNow.Add(time.Hour))
	path, err := statePath(proofSlotRoot(backing, q.generation, ProofCapacitySystemFull, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	err = q.withTurn(context.Background(), later, func() error { called = true; return nil })
	var busy *ProofQueueBusyError
	if called || err == nil || errors.As(err, &busy) || !strings.Contains(err.Error(), "parse land-lease state") {
		t.Fatalf("corrupt slot lease did not fail closed: called=%v err=%v", called, err)
	}
}
func TestProofQueuePrunesDeadAndExpiredHead(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "dead"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			q := proofQueueFixture(t)
			older := queueFixtureTicket(t, q, ProofCapacitySystemFull, "2", baseNow.Add(time.Second))
			younger := queueFixtureTicket(t, q, ProofCapacitySystemFull, "3", baseNow.Add(time.Hour))
			now := baseNow
			if expired {
				now = baseNow.Add(time.Second)
			} else {
				// Simulate descriptor loss on process death without orderly cancellation.
				if err := older.file.Close(); err != nil {
					t.Fatal(err)
				}
				older.file = nil
			}
			q.now = func() time.Time { return now }
			if err := q.withTurn(context.Background(), younger, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestProofQueueCancellationAndDeadlineRefuseBeforeCallback(t *testing.T) {
	q := proofQueueFixture(t)
	ticket := queueFixtureTicket(t, q, ProofCapacitySystemFull, "2", baseNow.Add(time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	fn := func() error { called = true; return nil }
	if err := q.withTurn(ctx, ticket, fn); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	q.now = func() time.Time { return baseNow.Add(time.Second) }
	if err := q.withTurn(context.Background(), ticket, fn); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if called {
		t.Fatal("stopped ticket entered admission")
	}
}

func TestProofQueueMissingIdentityMetadataRefuses(t *testing.T) {
	for _, body := range []string{"{}", `{"schema_version":"compound.proof_queue.v0.1"}`, `{"generation":"` + strings.Repeat("1", 64) + `"}`} {
		t.Run(body, func(t *testing.T) {
			q := proofQueueFixture(t)
			if err := os.MkdirAll(filepath.Dir(q.path()), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(q.path(), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := q.readLocked(); err == nil {
				t.Fatal("missing queue identity accepted")
			}
		})
	}
}
func TestProofQueueDeadlineAdvancesWhileWaitingForLock(t *testing.T) {
	for _, enqueue := range []bool{false, true} {
		name := "turn"
		if enqueue {
			name = "enqueue"
		}
		t.Run(name, func(t *testing.T) {
			q := proofQueueFixture(t)
			ticket := queueFixtureTicket(t, q, ProofCapacitySystemFull, "2", baseNow.Add(time.Second))
			var expired atomic.Bool
			sampled := make(chan struct{}, 1)
			q.now = func() time.Time {
				if expired.Load() {
					return baseNow.Add(time.Second)
				}
				select {
				case sampled <- struct{}{}:
				default:
				}
				return baseNow
			}
			done := make(chan error, 1)
			called := false
			if err := withLock(q.root, func() error {
				go func() {
					if enqueue {
						_, err := q.enqueue(ticket.record.Identity, baseNow.Add(time.Second))
						done <- err
					} else {
						done <- q.withTurn(context.Background(), ticket, func() error { called = true; return nil })
					}
				}()
				<-sampled
				expired.Store(true)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expired waiter result = %v", err)
			}
			if called {
				t.Fatal("expired waiter entered callback")
			}
		})
	}
}

func TestProofQueueCancellationReclaimsFilesAndUnblocksFailure(t *testing.T) {
	q := proofQueueFixture(t)
	first := queueFixtureTicket(t, q, ProofCapacitySystemFull, "2", baseNow.Add(time.Hour))
	next := queueFixtureTicket(t, q, ProofCapacitySystemFull, "3", baseNow.Add(time.Hour))
	failure := errors.New("preparation failed")
	if err := q.withTurn(context.Background(), first, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := first.Cancel(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(q.ticketPath(first.ID())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled liveness remains: %v", err)
	}
	if err := q.withTurn(context.Background(), next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestProofQueuePruningReclaimsFilesAndDeadOrphans(t *testing.T) {
	q := proofQueueFixture(t)
	first := queueFixtureTicket(t, q, ProofCapacitySystemFull, "2", baseNow.Add(time.Second))
	orphan := "pqt_" + strings.Repeat("a", 32)
	if err := os.WriteFile(q.ticketPath(orphan), nil, 0600); err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return baseNow.Add(time.Second) }
	next := queueFixtureTicket(t, q, ProofCapacitySystemFull, "3", baseNow.Add(time.Hour))
	for _, id := range []string{first.ID(), orphan} {
		if _, err := os.Lstat(q.ticketPath(id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dead liveness remains: %s: %v", id, err)
		}
	}
	if err := q.withTurn(context.Background(), next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
