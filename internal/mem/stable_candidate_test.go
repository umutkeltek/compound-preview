package mem

import (
	"os"
	"path/filepath"
	"testing"

	"compound/internal/candidates"
)

func TestRecordStableCandidateReusesExistingCandidate(t *testing.T) {
	store := newStore(t)
	candidate := Candidate{
		ID:             "cand_stable_test",
		Entity:         "knowledge:source:item",
		Predicate:      "reference_metadata",
		Object:         "title=Stable",
		Confidence:     0.8,
		Derivation:     "deterministic",
		SourceTraceRef: "ev_stable",
		SnippetHash:    "abc123",
		Visibility:     "candidate",
		State:          "proposed",
		CreatedAt:      rfc3339(t0),
	}
	created, err := store.RecordStableCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first stable candidate write should create")
	}
	reused, err := store.RecordStableCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("second stable candidate write should reuse existing record")
	}
	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != candidate.ID {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestRecordStableCandidateRejectsUnsafeExistingCustody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "filename body id mismatch",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(`{"id":"cand_other","state":"proposed"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "outside.json")
				if err := os.WriteFile(target, []byte(`{"id":"cand_stable_unsafe","state":"proposed"}`), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			candidate := Candidate{
				ID:         "cand_stable_unsafe",
				Entity:     "knowledge:source:item",
				Predicate:  "reference_metadata",
				Object:     "title=Unsafe",
				State:      "proposed",
				Visibility: "candidate",
				Derivation: "deterministic",
				CreatedAt:  rfc3339(t0),
			}
			path := filepath.Join(store.home, candidates.InboxDir(candidates.Memory), candidate.ID+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, path)

			created, err := store.RecordStableCandidate(candidate)
			if err == nil {
				t.Fatal("stable candidate reuse accepted unsafe existing custody")
			}
			if created {
				t.Fatal("unsafe existing custody reported a new candidate")
			}
		})
	}
}
