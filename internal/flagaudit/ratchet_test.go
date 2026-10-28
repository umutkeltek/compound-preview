package flagaudit

import (
	"strings"
	"testing"
)

// TestEnforceCeilingFailsOnGrowth is the RED half of the ratchet contract: a
// census containing a path beyond the pinned ceiling must fail, naming the
// path and pointing at the fix pattern.
func TestEnforceCeilingFailsOnGrowth(t *testing.T) {
	t.Parallel()

	pinned := []string{"alpha", "beta"}
	err := EnforceCeiling([]string{"alpha", "beta", "gamma-new"}, pinned)
	if err == nil {
		t.Fatal("census with unpinned path must fail the ceiling")
	}
	if !strings.Contains(err.Error(), "gamma-new") || !strings.Contains(err.Error(), "GREW") {
		t.Fatalf("growth failure must name the new path and the direction: %v", err)
	}
}

// TestEnforceCeilingFailsOnShrink is the ratchet-tightening half: a path that
// left the census must be removed from the pin, so improvements are locked in
// and regressions cannot hide behind a stale ceiling.
func TestEnforceCeilingFailsOnShrink(t *testing.T) {
	t.Parallel()

	err := EnforceCeiling([]string{"alpha"}, []string{"alpha", "beta"})
	if err == nil {
		t.Fatal("census smaller than the pin must fail the ceiling")
	}
	if !strings.Contains(err.Error(), "beta") || !strings.Contains(err.Error(), "SHRANK") {
		t.Fatalf("shrink failure must name the stale pin entry and the direction: %v", err)
	}
}

// TestEnforceCeilingPassesAtPinnedCensus pins the steady state: equal sets —
// in any order — hold the ceiling.
func TestEnforceCeilingPassesAtPinnedCensus(t *testing.T) {
	t.Parallel()

	pinned := []string{"alpha", "beta", "gamma"}
	if err := EnforceCeiling([]string{"gamma", "alpha", "beta"}, pinned); err != nil {
		t.Fatalf("census equal to the pin must hold: %v", err)
	}
}

// TestEnforceCeilingPassesOnExactEmptyPinnedSet documents that an empty
// census with an empty pin is the end state the ratchet drives toward.
func TestEnforceCeilingPassesOnExactEmptyPinnedSet(t *testing.T) {
	t.Parallel()

	if err := EnforceCeiling(nil, nil); err != nil {
		t.Fatalf("empty census with empty pin must hold: %v", err)
	}
}
