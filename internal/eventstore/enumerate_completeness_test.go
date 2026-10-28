package eventstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestEnumerateIDsRepairsIncompleteInventory(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "subset"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
			day := seedProofEnumerationFixture(t, store, now)
			if _, err := store.EnumerateIDs("pfrh_", true); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.rootPath(), "index", "other", day+".idx")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var index segmentIndex
			if err := json.Unmarshal(body, &index); err != nil {
				t.Fatal(err)
			}
			if empty {
				index.Entries = map[string]indexEntry{}
			} else {
				delete(index.Entries, "pfrh_seed0001_outcome_b")
			}
			// Keep the exact correct source hash/stat metadata. This is a valid
			// index shape that the old source-only verification marker accepts.
			if err := writeIndex(path, index); err != nil {
				t.Fatal(err)
			}
			seg := segmentFile{path: segmentPath(store.engine.segRoot, "other", day, 0), day: day}
			if _, err := store.engine.loadOrBuildIndex("other", seg); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(store.engine.home)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.EnumerateIDs("pfrh_", true)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"pfrh_seed0001_outcome_a", "pfrh_seed0001_outcome_b", "pfrh_seed0001_outcome_c"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("incomplete index hid authority: got %v, want %v", got, want)
			}
		})
	}
}

func TestEnumerateIDsRevalidatesMissingOrCorruptInventoryMarker(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "missing"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
			day := seedProofEnumerationFixture(t, store, now)
			if _, err := store.EnumerateIDs("pfrh_absent_", true); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.rootPath(), "index", "other", day+".idx.inventory-verified")
			if corrupt {
				if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			parsed := observedSegmentPaths(t)
			reopened, err := Open(store.engine.home)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.EnumerateIDs("pfrh_absent_", true)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("unexpected ids: %v", got)
			}
			sealed := segmentPath(store.engine.segRoot, "other", day, 0)
			found := false
			for _, path := range parsed() {
				if path == sealed {
					found = true
				}
			}
			if !found {
				t.Fatal("missing inventory authority did not revalidate sealed truth")
			}
		})
	}
}
