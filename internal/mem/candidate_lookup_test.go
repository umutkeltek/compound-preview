package mem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCandidatesByIDOmitsMissingAndCorruptCandidates(t *testing.T) {
	store := newStore(t)
	for _, candidate := range []Candidate{
		{ID: "cand_lookup_alpha", Entity: "knowledge:lookup:alpha", Predicate: "status", Object: "alpha", State: "parked"},
		{ID: "cand_lookup_beta", Entity: "knowledge:lookup:beta", Predicate: "status", Object: "beta", State: "parked"},
		{ID: "cand_lookup_unrequested", Entity: "knowledge:lookup:unrequested", Predicate: "status", Object: "unrequested", State: "parked"},
	} {
		if _, err := store.RecordStableCandidate(candidate); err != nil {
			t.Fatalf("record stable candidate %s: %v", candidate.ID, err)
		}
	}
	if err := os.WriteFile(filepath.Join(store.candDir(), "cand_lookup_corrupt.json"), []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write corrupt candidate: %v", err)
	}

	got, err := store.CandidatesByID([]string{
		"cand_lookup_alpha",
		"cand_lookup_missing",
		"cand_lookup_corrupt",
		"cand_lookup_beta",
		"cand_lookup_alpha",
	})
	if err != nil {
		t.Fatalf("candidates by id: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want only the two readable requested candidates", got)
	}
	for _, id := range []string{"cand_lookup_alpha", "cand_lookup_beta"} {
		if got[id].ID != id {
			t.Fatalf("candidate %s = %+v, want matching candidate", id, got[id])
		}
	}
	for _, id := range []string{"cand_lookup_missing", "cand_lookup_corrupt", "cand_lookup_unrequested"} {
		if _, ok := got[id]; ok {
			t.Fatalf("candidate %s unexpectedly returned", id)
		}
	}
}
