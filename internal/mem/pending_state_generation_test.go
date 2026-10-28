package mem

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/candidates"
)

func TestPendingStateGenerationDetectsSameCountCorpusReplacement(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	write := func(id string) {
		t.Helper()
		candidate := Candidate{ID: id, Entity: id, Predicate: "value", Object: id, State: "proposed", Visibility: "operational", Derivation: "extracted", CreatedAt: "2026-08-26T00:00:00Z"}
		if err := candidates.WriteCandidate(home, candidates.Memory, id, &candidate); err != nil {
			t.Fatal(err)
		}
	}
	write("cand_a")
	write("cand_b")
	state, err := store.ReconcilePendingState("test", t0)
	if err != nil {
		t.Fatal(err)
	}
	if state.Files != 2 || state.Generation == "" {
		t.Fatalf("state = %+v, want two files and generation", state)
	}

	if err := os.Remove(filepath.Join(store.candDir(), "cand_a.json")); err != nil {
		t.Fatal(err)
	}
	write("cand_c")
	names, generation, _, err := store.candidateInboxMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || generation == state.Generation {
		t.Fatalf("replacement generation did not move: names=%v before=%s after=%s", names, state.Generation, generation)
	}
	if pendingStateFresh(state, len(names), generation, t0.Add(time.Minute)) {
		t.Fatal("same file count with a different candidate set must be stale")
	}
}

func TestReconcilePendingStateRefusesCandidateCustodyCorruption(t *testing.T) {
	tests := map[string]func(t *testing.T, candidateDir string){
		"invalid filename": func(t *testing.T, candidateDir string) {
			if err := os.WriteFile(filepath.Join(candidateDir, "scratch.tmp"), []byte("not a canonical candidate"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"blank id": func(t *testing.T, candidateDir string) {
			if err := os.WriteFile(filepath.Join(candidateDir, "blank.json"), []byte(`{"id":"","state":"proposed"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"id mismatch": func(t *testing.T, candidateDir string) {
			if err := os.WriteFile(filepath.Join(candidateDir, "named.json"), []byte(`{"id":"other","state":"proposed"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, candidateDir string) {
			outside := filepath.Join(t.TempDir(), "outside.json")
			if err := os.WriteFile(outside, []byte(`{"id":"linked","state":"proposed"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(candidateDir, "linked.json")); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			store, err := Open(home)
			if err != nil {
				t.Fatal(err)
			}
			valid := Candidate{ID: "valid", Entity: "valid", Predicate: "p", Object: "o", State: "proposed", Visibility: "operational", Derivation: "extracted", CreatedAt: rfc3339(t0)}
			if err := candidates.WriteCandidate(home, candidates.Memory, valid.ID, &valid); err != nil {
				t.Fatal(err)
			}
			baseline, err := store.ReconcilePendingState("baseline", t0)
			if err != nil {
				t.Fatal(err)
			}

			corrupt(t, store.candDir())
			if _, err := store.ReconcilePendingState("custody-check", t0.Add(time.Minute)); err == nil {
				t.Fatal("reconcile accepted a noncanonical candidate entry")
			}
			if _, _, _, err := store.candidateInboxMetadata(); err == nil {
				t.Fatal("candidate inbox metadata accepted a noncanonical candidate entry")
			}
			after, err := store.ReadPendingState()
			if err != nil {
				t.Fatal(err)
			}
			if after.Generation != baseline.Generation || after.Pending != baseline.Pending || after.Files != baseline.Files {
				t.Fatalf("failed reconcile changed durable state: after=%+v baseline=%+v", after, baseline)
			}
		})
	}
}

func TestPendingMetadataRejectsNonCanonicalEntries(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{ID: "cand_a", Entity: "a", Predicate: "value", Object: "a", State: "proposed", Visibility: "operational", Derivation: "extracted", CreatedAt: "2026-08-26T00:00:00Z"}
	if err := candidates.WriteCandidate(home, candidates.Memory, candidate.ID, &candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.candDir(), "scratch.tmp"), []byte("not a candidate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.candDir(), "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.candidateInboxMetadata(); err == nil {
		t.Fatal("candidate metadata silently ignored noncanonical candidate-root entries")
	}
	if _, err := store.ReconcilePendingState("custody-check", t0); err == nil {
		t.Fatal("pending reconciliation silently ignored noncanonical candidate-root entries")
	}
}
