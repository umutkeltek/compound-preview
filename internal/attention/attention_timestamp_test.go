package attention

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"compound/internal/work"
)

// Work claims stamp Unix nanoseconds, which Workbench carries unchanged.
// Exercise the public projection with that producer format, not only RFC3339.
func TestAttentionNativeClaimAge(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 123456789, time.UTC)
	old := now.Add(-7 * 24 * time.Hour)
	rfc := old.Format(time.RFC3339Nano)
	native := strconv.FormatInt(old.UnixNano(), 10)
	project := func(stamp string) Group {
		t.Helper()
		report := Build(Inputs{Workbench: &work.WorkbenchReport{
			GeneratedAt: now.Format(time.RFC3339Nano),
			Open:        []work.WorkbenchItem{{WorkID: "work_waiting", State: "held", ClaimedAt: stamp}},
		}}, now)
		return groupByKey(t, report, "workbench:blocked-on-you")
	}
	want := project(rfc)
	got := project(native)
	if got.Leverage != 10.9375 {
		t.Errorf("seven-day native claim leverage = %v, want 10.9375 (age and stale bump)", got.Leverage)
	}
	if got.OldestCreatedAt != native {
		t.Errorf("claim provenance = %q, want original %q", got.OldestCreatedAt, native)
	}
	// Timestamp representation must not change permission or action semantics.
	got.OldestCreatedAt = want.OldestCreatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("native and RFC3339 projections differ:\nnative=%+v\nRFC3339=%+v", got, want)
	}
}

func TestAttentionMixedClaimFormatsKeepOldest(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 123456789, time.UTC)
	old := now.Add(-7 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	native := func(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 10) }
	for _, tc := range []struct{ name, older, newer string }{
		{"native_then_rfc", native(old), recent.Format(time.RFC3339Nano)},
		{"rfc_then_native", old.Format(time.RFC3339Nano), native(recent)},
		{"both_native", native(old), native(recent)},
		{"both_rfc", old.Format(time.RFC3339Nano), recent.Format(time.RFC3339Nano)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reversed := range []bool{false, true} {
				items := []work.WorkbenchItem{
					{WorkID: "work_old", State: "held", ClaimedAt: tc.older},
					{WorkID: "work_new", State: "held", ClaimedAt: tc.newer},
				}
				if reversed {
					items[0], items[1] = items[1], items[0]
				}
				report := Build(Inputs{Workbench: &work.WorkbenchReport{
					GeneratedAt: now.Format(time.RFC3339Nano), Open: items,
				}}, now)
				group := groupByKey(t, report, "workbench:blocked-on-you")
				if group.OldestCreatedAt != tc.older || group.Leverage != 21.875 {
					t.Errorf("reversed=%v: oldest=%q leverage=%v; want %q / 21.875", reversed, group.OldestCreatedAt, group.Leverage, tc.older)
				}
			}
		})
	}
}

func TestAttentionClaimTimestampCompatibility(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	native := func(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 10) }
	for _, tc := range []struct {
		name, stamp string
		want        float64
	}{
		{"native_boundary_48h", native(now.Add(-48 * time.Hour)), 2.5},
		{"native_past_48h", native(now.Add(-72 * time.Hour)), 4.6875},
		{"native_whitespace", " " + native(now.Add(-72*time.Hour)) + " ", 4.6875},
		{"future_native", native(now.Add(time.Hour)), 1.25},
		{"rfc3339_offset", now.Add(-72 * time.Hour).In(time.FixedZone("test", 2*60*60)).Format(time.RFC3339), 4.6875},
		{"rfc3339_nano", now.Add(-72 * time.Hour).Format(time.RFC3339Nano), 4.6875},
		{"malformed", "not-a-time", 1.25},
		{"overflow", "9223372036854775808", 1.25},
		{"zero", "0", 1.25},
		{"negative", "-1", 1.25},
		{"empty", "", 1.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := Build(Inputs{Workbench: &work.WorkbenchReport{
				GeneratedAt: now.Format(time.RFC3339Nano),
				Open:        []work.WorkbenchItem{{WorkID: "work_waiting", State: "held", ClaimedAt: tc.stamp}},
			}}, now)
			group := groupByKey(t, report, "workbench:blocked-on-you")
			if group.Leverage != tc.want {
				t.Fatalf("leverage=%v, want %v", group.Leverage, tc.want)
			}
		})
	}
}
