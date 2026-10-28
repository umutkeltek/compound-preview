package attention

import (
	"testing"
	"time"
)

func activityInput(blocks ...ActivityBlock) *ActivityDayInput {
	total := 0
	for _, b := range blocks {
		total += b.DurationMin
	}
	return &ActivityDayInput{
		Date:                "2026-07-15",
		GeneratedAt:         "2026-07-16T00:00:00Z",
		TotalUnaccountedMin: total,
		UnaccountedBlocks:   blocks,
	}
}

// TestFalsifier8_TwoBlocksOneGroupSummaryCountsBoth: two >=20-min unaccounted blocks yield exactly
// ONE attention group whose summary counts both blocks.
func TestFalsifier8_TwoBlocksOneGroupSummaryCountsBoth(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	in := activityInput(
		ActivityBlock{Start: "2026-07-15T09:00:00Z", End: "2026-07-15T09:47:00Z", DurationMin: 47, ActiveMin: 45, CategoryMix: "browsing/design"},
		ActivityBlock{Start: "2026-07-15T14:00:00Z", End: "2026-07-15T14:25:00Z", DurationMin: 25, ActiveMin: 24, CategoryMix: "coding"},
	)
	report := Build(Inputs{Activity: in}, now)

	activityGroups := groupsWithSource(report.Groups, SourceActivity)
	if len(activityGroups) != 1 {
		t.Fatalf("want exactly ONE activity group, got %d: %+v", len(activityGroups), activityGroups)
	}
	g := activityGroups[0]
	if g.Band != BandWatching {
		t.Fatalf("activity group must be watching-band, got %q", g.Band)
	}
	if g.Count != 2 {
		t.Fatalf("group count must be 2 (both blocks), got %d", g.Count)
	}
	// The 47-min block is the headline; the summary must acknowledge the second.
	if !contains(g.OperatorSummary, "47 min unaccounted") {
		t.Fatalf("summary must headline the largest block: %q", g.OperatorSummary)
	}
	if !contains(g.OperatorSummary, "+1 more") {
		t.Fatalf("summary must count the second block: %q", g.OperatorSummary)
	}
	if g.BlastRadius != BlastRead || g.Reversibility != Reversible {
		t.Fatalf("activity evidence group must be read/reversible, got %s/%s", g.BlastRadius, g.Reversibility)
	}
}

// TestFalsifier8_NoUnaccountedBlocksNoGroup: an activity input with zero unaccounted blocks (the
// accounted-only or incomplete-coverage day, filtered upstream) yields zero attention groups.
func TestFalsifier8_NoUnaccountedBlocksNoGroup(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	report := Build(Inputs{Activity: activityInput()}, now)
	if got := len(groupsWithSource(report.Groups, SourceActivity)); got != 0 {
		t.Fatalf("no unaccounted blocks must yield zero activity groups, got %d", got)
	}
}

// TestActivityBuildNilSafe: the other four Build call sites pass a nil Activity — Build must not panic
// and must produce no activity group.
func TestActivityBuildNilSafe(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	report := Build(Inputs{}, now) // all inputs nil
	if got := len(groupsWithSource(report.Groups, SourceActivity)); got != 0 {
		t.Fatalf("nil Activity must yield no activity group, got %d", got)
	}
}

func groupsWithSource(groups []Group, source string) []Group {
	var out []Group
	for _, g := range groups {
		if g.Source == source {
			out = append(out, g)
		}
	}
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
