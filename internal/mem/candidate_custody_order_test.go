package mem

import (
	"reflect"
	"testing"

	"compound/internal/candidates"
)

func candidateCustodyOrderStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "a-", "z"} {
		candidate := Candidate{ID: id, Entity: "entity:" + id, Predicate: "order", Object: id, State: "proposed", Visibility: "candidate"}
		if err := candidates.WriteCandidate(store.home, candidates.Memory, id, candidate); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// The canonical candidate order is exact filename-byte order (the custody
// contract: cursors are opaque directory-entry names, not decoded ids), so the
// prefix-adjacent pair "a"/"a-" walks "a-.json" first. Resuming after that
// entry must return "a" — never skip it for "z".
func TestListCandidatesLiteResumesAfterPrefixEntryWithoutGap(t *testing.T) {
	store := candidateCustodyOrderStore(t)
	page, err := store.ListCandidatesLite(candidates.EntryCursor("a-.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Walked != 1 || len(page.Entries) != 1 {
		t.Fatalf("resume after a-.json walked %+v, want exactly one entry", page)
	}
	entry := page.Entries[0]
	if entry.Corruption != nil {
		t.Fatalf("resume after a-.json returned corruption: %+v", entry.Corruption)
	}
	if entry.Candidate == nil || entry.Candidate.ID != "a" {
		t.Fatalf("resume after a-.json returned %+v, want a before z", entry)
	}
	if page.LastCursor != "a.json" || page.AtEnd {
		t.Fatalf("progress = last %q atEnd=%v, want a.json with work remaining", page.LastCursor, page.AtEnd)
	}
}

func TestListCandidatesLiteBoundedWalkPreservesCanonicalFilenameOrder(t *testing.T) {
	store := candidateCustodyOrderStore(t)
	var got []string
	var cursor candidates.EntryCursor
	for step := 0; step < 4; step++ {
		page, err := store.ListCandidatesLite(cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Entries {
			if entry.Corruption != nil {
				t.Fatalf("corrupt = %+v", entry.Corruption)
			}
			got = append(got, entry.Candidate.ID)
		}
		if page.AtEnd {
			break
		}
		if page.LastCursor == "" || page.LastCursor == cursor {
			t.Fatalf("cursor did not advance: before=%q after=%q", cursor, page.LastCursor)
		}
		cursor = page.LastCursor
	}
	if want := []string{"a-", "a", "z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bounded walk IDs = %v, want canonical filename order %v", got, want)
	}
}

func TestAllCandidatesReturnsCanonicalFilenameOrder(t *testing.T) {
	store := candidateCustodyOrderStore(t)
	all, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(all))
	for _, candidate := range all {
		got = append(got, candidate.ID)
	}
	if want := []string{"a-", "a", "z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AllCandidates IDs = %v, want canonical filename order %v", got, want)
	}
}
