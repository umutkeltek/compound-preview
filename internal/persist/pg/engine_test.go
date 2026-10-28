package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/mem"
)

func TestUpsertWithEmbedderStampsEmbedder(t *testing.T) {
	store := testStore(t)

	ref := fmt.Sprintf("vec-emb-%d", time.Now().UnixNano())
	emb := []float32{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}
	identity := "ollama:nomic-embed-text:8"

	if err := store.UpsertWithEmbedder(ref, emb, map[string]string{"topic": "custody"}, identity); err != nil {
		t.Fatalf("UpsertWithEmbedder: %v", err)
	}

	embedders, err := store.VectorEmbedders(testCtx(t))
	if err != nil {
		t.Fatalf("VectorEmbedders: %v", err)
	}
	if len(embedders) != 1 || embedders[0] != identity {
		t.Errorf("stored embedders = %v, want [%s]", embedders, identity)
	}
}

func TestUpsertWithEmbedderRefusesMismatch(t *testing.T) {
	store := testStore(t)

	refA := fmt.Sprintf("vec-mismatch-a-%d", time.Now().UnixNano())
	refB := fmt.Sprintf("vec-mismatch-b-%d", time.Now().UnixNano())
	emb := []float32{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}

	if err := store.UpsertWithEmbedder(refA, emb, nil, "ollama:first:8"); err != nil {
		t.Fatalf("first embedder: %v", err)
	}

	err := store.UpsertWithEmbedder(refB, emb, nil, "ollama:second:8")
	if !errors.Is(err, mem.ErrEmbedderMismatch) {
		t.Fatalf("different embedder error = %v, want ErrEmbedderMismatch", err)
	}
}

// TestUpsertWithEmbedderPreventsConcurrentMixedEmbedders starts two goroutines
// that try to stamp vectors with different identities at the same time. Before
// the advisory-lock fix both could see an empty table and commit, mixing spaces;
// with the lock, exactly one identity wins and the other is refused.
func TestUpsertWithEmbedderPreventsConcurrentMixedEmbedders(t *testing.T) {
	store := testStore(t)

	const identityA = "ollama:model-a:8"
	const identityB = "ollama:model-b:8"
	emb := []float32{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}

	var wg sync.WaitGroup
	var mu sync.Mutex
	committed := map[string]int{}

	run := func(id int, identity string) {
		defer wg.Done()
		ref := fmt.Sprintf("vec-race-%d-%d", id, time.Now().UnixNano())
		err := store.UpsertWithEmbedder(ref, emb, nil, identity)
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			committed[identity]++
		} else if !errors.Is(err, mem.ErrEmbedderMismatch) {
			t.Errorf("unexpected error from %s: %v", identity, err)
		}
	}

	wg.Add(2)
	go run(1, identityA)
	go run(2, identityB)
	wg.Wait()

	if committed[identityA] > 0 && committed[identityB] > 0 {
		t.Fatalf("both embedders committed: mixed embedding spaces in one index = %v", committed)
	}
	if committed[identityA] == 0 && committed[identityB] == 0 {
		t.Fatalf("neither embedder committed: both failed = %v", committed)
	}
}

func testCtx(t *testing.T) context.Context {
	return context.Background()
}

// readSourceFile reads a sibling source file from the same package directory.
// Used for static assertions that would fail on the un-fixed code.
func readSourceFile(name string) (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot determine test file path")
	}
	path := filepath.Join(filepath.Dir(file), name)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestEmbedderLockUsesHardcodedNamespacedKey verifies that the embedder advisory
// lock is acquired with a hardcoded 64-bit namespaced key, not with hashtext.
// hashtext has a small collision space and could alias with unrelated keys;
// pg_advisory_xact_lock(8392753, 1) is an explicit, stable namespace.
func TestEmbedderLockUsesHardcodedNamespacedKey(t *testing.T) {
	source, err := readSourceFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	// The SQL expression "hashtext(" must not appear in the source; this catches the
	// previously used pg_advisory_xact_lock(hashtext('compound:vectors:embedder')).
	if strings.Contains(source, "hashtext(") {
		t.Fatalf("engine.go still uses hashtext(...) for the embedder advisory lock")
	}
	want := "pg_advisory_xact_lock($1, $2)"
	if !strings.Contains(source, want) {
		t.Fatalf("engine.go does not use a parameterized 2-key pg_advisory_xact_lock; want %q", want)
	}
	wantKey := "vectorEmbedderLockKey1 = 8392753"
	if !strings.Contains(source, wantKey) {
		t.Fatalf("engine.go missing hardcoded lock namespace key; want %q", wantKey)
	}
	wantKey2 := "vectorEmbedderLockKey2 = 1"
	if !strings.Contains(source, wantKey2) {
		t.Fatalf("engine.go missing hardcoded lock instance key; want %q", wantKey2)
	}
}
