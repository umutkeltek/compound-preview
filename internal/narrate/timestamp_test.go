package narrate_test

import (
	"strconv"
	"testing"
	"time"

	"compound/internal/attention"
	"compound/internal/narrate"
	"compound/internal/work"
)

// Attention preserves the oldest claim's original timestamp representation.
// A mixed-format group must keep its warning when the older native claim wins.
func TestNarrateMixedClaimFormatsPreserveWarning(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 123456789, time.UTC)
	old, recent := now.Add(-7*24*time.Hour), now.Add(-3*24*time.Hour)
	native := func(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 10) }
	for _, tc := range []struct{ name, older, newer string }{
		{"native_rfc", native(old), recent.Format(time.RFC3339Nano)},
		{"rfc_native", old.Format(time.RFC3339Nano), native(recent)},
		{"native_native", native(old), native(recent)},
		{"rfc_rfc", old.Format(time.RFC3339Nano), recent.Format(time.RFC3339Nano)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reversed := range []bool{false, true} {
				items := []work.WorkbenchItem{
					{WorkID: "work_old", State: "held", ClaimedAt: tc.older},
					{WorkID: "work_recent", State: "held", ClaimedAt: tc.newer},
				}
				if reversed {
					items[0], items[1] = items[1], items[0]
				}
				report := attention.Build(attention.Inputs{Workbench: &work.WorkbenchReport{
					GeneratedAt: now.Format(time.RFC3339Nano), Open: items,
				}}, now)
				got := buildOrDie(t, narrate.Inputs{Attention: report, Now: now}, narrate.Options{})
				if got.Script.Counts.WarnCount != 1 || len(got.Script.Beats) != 1 ||
					got.Script.Beats[0].Act != narrate.ActWarn || got.Script.Beats[0].ThreadID != "workbench:blocked-on-you" {
					t.Fatalf("reversed=%v: oldest=%q, want one blocked-work warning; got counts=%+v beats=%+v",
						reversed, report.Groups[0].OldestCreatedAt, got.Script.Counts, got.Script.Beats)
				}
			}
		})
	}
}

func TestNarrateClaimTimestampCompatibility(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 123456789, time.UTC)
	native := func(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 10) }
	for _, tc := range []struct {
		name, stamp string
		wantWarn    int
	}{
		{"native_old", native(now.Add(-72 * time.Hour)), 1},
		{"native_boundary", native(now.Add(-48 * time.Hour)), 0},
		{"native_just_past_boundary", native(now.Add(-48*time.Hour - time.Nanosecond)), 1},
		{"native_recent", native(now.Add(-time.Hour)), 0},
		{"native_future", native(now.Add(time.Hour)), 0},
		{"native_whitespace", " " + native(now.Add(-72*time.Hour)) + " ", 1},
		{"rfc_offset", now.Add(-72 * time.Hour).In(time.FixedZone("test", 7200)).Format(time.RFC3339Nano), 1},
		{"rfc_fractional", now.Add(-72 * time.Hour).Format(time.RFC3339Nano), 1},
		{"malformed", "not-a-time", 0},
		{"overflow", "9223372036854775808", 0},
		{"negative", "-1", 0},
		{"zero", "0", 0},
		{"empty", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := attention.Build(attention.Inputs{Workbench: &work.WorkbenchReport{
				GeneratedAt: now.Format(time.RFC3339Nano),
				Open:        []work.WorkbenchItem{{WorkID: "work_waiting", State: "held", ClaimedAt: tc.stamp}},
			}}, now)
			got := buildOrDie(t, narrate.Inputs{Attention: report, Now: now}, narrate.Options{})
			if got.Script.Counts.WarnCount != tc.wantWarn {
				t.Fatalf("warn count=%d, want %d", got.Script.Counts.WarnCount, tc.wantWarn)
			}
		})
	}
}
