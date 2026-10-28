package capgraph

import (
	"sync"
	"testing"
	"time"
)

// TestGraphWriteLockSerializesMutations: the graph's mutations are
// load -> decide -> rewrite/append critical sections. Without a
// cross-process lock two writers can load the same old state and the later
// rename erases the earlier writer's changes (the capability graph is an
// authority/routing registry — a lost update is a silently wrong system
// belief). Falsifier: an externally held graph write lock must BLOCK Add
// until released; pre-lock, Add completed underneath.
func TestGraphWriteLockSerializesMutations(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	completedWhileHeld := false
	blocked := s.withGraphWriteLock(func() error {
		go func() {
			done <- s.Add(Node{ID: "tool:lock-probe", Kind: "tool", Name: "Lock probe"})
		}()
		select {
		case err := <-done:
			completedWhileHeld = true
			return err
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	})
	if blocked != nil {
		t.Fatalf("Add returned an error while the graph write lock was held: %v", blocked)
	}
	if completedWhileHeld {
		t.Fatal("Add completed while the graph write lock was held (no serialization)")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Add failed after the graph write lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Add stayed blocked after the graph write lock was released")
	}
}

// TestCompactionDoesNotEatConcurrentAppends: compaction rewrites the nodes
// log from a snapshot; an Add that lands between the snapshot and the rename
// must survive. Pre-lock this raced: compaction silently deleted the
// appended node. Deterministic without scheduling games because both sides
// now serialize on the same flock.
func TestCompactionDoesNotEatConcurrentAppends(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// Seed enough duplicate rows that compaction has real work.
	for i := 0; i < 5; i++ {
		if err := s.Add(Node{ID: "tool:compaction-fixture", Kind: "tool", Name: "Compaction fixture", Weight: float64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := s.CompactNodes(); err != nil {
			errs <- err
		}
	}()
	go func() {
		defer wg.Done()
		if err := s.Add(Node{ID: "tool:late-arrival", Kind: "tool", Name: "Late arrival"}); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	nodes, err := s.loadNodes()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, n := range nodes {
		found[n.ID] = true
	}
	if !found["tool:late-arrival"] {
		t.Fatalf("compaction ate a concurrently appended node; survivors: %+v", found)
	}
	if !found["tool:compaction-fixture"] {
		t.Fatalf("compaction lost the fixture node entirely; survivors: %+v", found)
	}
}
