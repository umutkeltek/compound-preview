package envelope

import (
	"sort"
	"strings"
	"testing"
)

// Falsifier F1: every MoveTypes entry has a MoveTypePlain entry.
// Proves teeth: scratch-add a MoveTypes value, or scratch-delete a
// MoveTypePlain entry, and this test reds. Revert with a targeted edit.

func TestGlossaryMoveTypePlainCoversMoveTypes(t *testing.T) {
	covered := map[string]bool{}
	for _, k := range MoveTypes {
		plain, ok := MoveTypePlain[k]
		if !ok {
			t.Errorf("MoveTypePlain missing entry for MoveTypes value %q", k)
			continue
		}
		if strings.TrimSpace(plain) == "" {
			t.Errorf("MoveTypePlain[%q] is empty", k)
		}
		covered[k] = true
	}
	for k := range MoveTypePlain {
		if !covered[k] {
			t.Errorf("MoveTypePlain has extra key %q not in MoveTypes", k)
		}
	}
}

// Sibling of MoveTypePlain: the canonical display label per MoveTypes entry.
// Drift smoke checks this byte-equal against the console; a label-only
// scratch-mutation in the console reds the smoke (F2 teeth).

func TestGlossaryMoveTypeLabelCoversMoveTypes(t *testing.T) {
	covered := map[string]bool{}
	for _, k := range MoveTypes {
		label, ok := MoveTypeLabel[k]
		if !ok {
			t.Errorf("MoveTypeLabel missing entry for MoveTypes value %q", k)
			continue
		}
		if strings.TrimSpace(label) == "" {
			t.Errorf("MoveTypeLabel[%q] is empty", k)
		}
		covered[k] = true
	}
	for k := range MoveTypeLabel {
		if !covered[k] {
			t.Errorf("MoveTypeLabel has extra key %q not in MoveTypes", k)
		}
	}
}

// Pin the five places + their order. The README's "What It Is" thesis line
// orders them door / ledger / edge / gate / receipt; the console's place
// walk-through inherits this order; the test fails if a future edit reorders
// or drops a place, or pads a sixth one.

func TestGlossaryPlainPlacesOrderedFive(t *testing.T) {
	want := []string{"door", "ledger", "edge", "gate", "receipt"}
	if len(PlainPlaces) != len(want) {
		t.Fatalf("PlainPlaces has %d entries, want %d", len(PlainPlaces), len(want))
	}
	for i, p := range PlainPlaces {
		if p.Name != want[i] {
			t.Errorf("PlainPlaces[%d].Name = %q, want %q", i, p.Name, want[i])
		}
		if strings.TrimSpace(p.Plain) == "" {
			t.Errorf("PlainPlaces[%d].Plain is empty", i)
		}
	}
}

// PlainPlaceNames must mirror the ordered slice — the helper is the surface
// callers index on, so a divergence here is a real bug.

func TestGlossaryPlainPlaceNamesMatchesSlice(t *testing.T) {
	fromSlice := make([]string, 0, len(PlainPlaces))
	for _, p := range PlainPlaces {
		fromSlice = append(fromSlice, p.Name)
	}
	fromHelper := PlainPlaceNames()
	if len(fromHelper) != len(fromSlice) {
		t.Fatalf("PlainPlaceNames length = %d, want %d", len(fromHelper), len(fromSlice))
	}
	for i := range fromSlice {
		if fromHelper[i] != fromSlice[i] {
			t.Errorf("PlainPlaceNames[%d] = %q, want %q", i, fromHelper[i], fromSlice[i])
		}
	}
}

// SortedMoveTypes must be a stable, sorted copy of MoveTypes — the smoke
// depends on a deterministic ordering for byte-equal comparisons.

func TestGlossarySortedMoveTypesIsSortedAndIsomorphic(t *testing.T) {
	got := SortedMoveTypes()
	if len(got) != len(MoveTypes) {
		t.Fatalf("SortedMoveTypes length = %d, want %d", len(got), len(MoveTypes))
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("SortedMoveTypes = %v, want sorted ascending", got)
	}
	seen := map[string]int{}
	for _, k := range got {
		seen[k]++
	}
	for _, k := range MoveTypes {
		seen[k]--
	}
	for k, n := range seen {
		if n != 0 {
			t.Errorf("SortedMoveTypes membership mismatch for key %q (delta %d)", k, n)
		}
	}
}

// TermPlain is the per-term one-liner the console renders. The test pins
// exact wording so the byte-equal check in the drift smoke has a single
// source of truth — the false teeth would be a future edit that drops a
// term or rewrites the wording under the console's feet.

func TestGlossaryTermPlainHasEveryExpectedTerm(t *testing.T) {
	want := []string{"ContextPack", "WorkEnvelope", "ActionIntent", "PolicyDecision", "Receipt", "Transition"}
	for _, term := range want {
		plain, ok := TermPlain[term]
		if !ok {
			t.Errorf("TermPlain missing entry for term %q", term)
			continue
		}
		if strings.TrimSpace(plain) == "" {
			t.Errorf("TermPlain[%q] is empty", term)
		}
	}
}
