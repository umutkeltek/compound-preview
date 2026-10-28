package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/activity"
	"compound/internal/attention"
	"compound/internal/work"
)

func writeActivitySpanDay(t *testing.T, homePath, day string, spans []activity.ActivitySpan) {
	t.Helper()
	sf := activity.SpanFile{
		SchemaVersion: activity.SpansSchemaVersion,
		Date:          day,
		GeneratedAt:   day + "T18:00:00Z",
		Spans:         spans,
	}
	body, err := json.Marshal(sf)
	if err != nil {
		t.Fatalf("marshal span file: %v", err)
	}
	if err := os.MkdirAll(activity.SpansRoot(homePath), 0o700); err != nil {
		t.Fatalf("mkdir spans root: %v", err)
	}
	if err := os.WriteFile(activity.SpanPath(homePath, day), body, 0o600); err != nil {
		t.Fatalf("write span file: %v", err)
	}
}

func attentionReportForHome(t *testing.T, homePath string) attention.Report {
	t.Helper()
	output := captureStdout(t, func() { cmdAttention([]string{"--json", "--home", homePath}) })
	var report attention.Report
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("parse attention JSON: %v\n%s", err, output)
	}
	return report
}

func activityGroupsOf(report attention.Report) []attention.Group {
	var out []attention.Group
	for _, g := range report.Groups {
		if g.Source == attention.SourceActivity {
			out = append(out, g)
		}
	}
	return out
}

// Decision 13 end-to-end: a fresh home with one >=20-min complete-coverage span and NO work
// timestamps inside it surfaces exactly ONE watching-band activity group on the attention CLI.
func TestAttentionCLISurfacesUnaccountedActivityGroup(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	day := time.Now().UTC().Format("2006-01-02")
	writeActivitySpanDay(t, homePath, day, []activity.ActivitySpan{{
		Start: day + "T09:00:00Z", End: day + "T09:47:00Z", ActiveSec: 45 * 60,
		BundleID: "com.example.browser", AppName: "Browser",
		Category: activity.CategoryBrowsing, Coverage: activity.CoverageComplete,
	}})

	groups := activityGroupsOf(attentionReportForHome(t, homePath))
	if len(groups) != 1 {
		t.Fatalf("want exactly one activity group, got %d", len(groups))
	}
	g := groups[0]
	if g.Band != attention.BandWatching {
		t.Fatalf("activity group must be watching-band, got %q", g.Band)
	}
	if !strings.Contains(g.OperatorSummary, "47 min unaccounted") || !strings.Contains(g.OperatorSummary, day) {
		t.Fatalf("summary must headline the block and its day: %q", g.OperatorSummary)
	}
}

// A real claim timestamp inside the block accounts for it: same day, but the envelope is claimed at
// 09:20Z — the CLI read reconciles against the workbench and yields zero activity groups.
func TestAttentionCLIAccountedBlockYieldsNoActivityGroup(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	day := time.Now().UTC().Format("2006-01-02")
	writeActivitySpanDay(t, homePath, day, []activity.ActivitySpan{{
		Start: day + "T09:00:00Z", End: day + "T09:47:00Z", ActiveSec: 45 * 60,
		BundleID: "com.example.browser", AppName: "Browser",
		Category: activity.CategoryBrowsing, Coverage: activity.CoverageComplete,
	}})
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID: "work_accounts_block", Owner: "compound-core", State: "ready",
		SuccessPredicate: "claimed inside the activity block",
	}, cliTestNow); err != nil {
		t.Fatalf("create work: %v", err)
	}
	claimAt, err := time.Parse(time.RFC3339, day+"T09:20:00Z")
	if err != nil {
		t.Fatalf("parse claim time: %v", err)
	}
	if _, err := store.Claim("work_accounts_block", "compound-core", claimAt); err != nil {
		t.Fatalf("claim work: %v", err)
	}

	if groups := activityGroupsOf(attentionReportForHome(t, homePath)); len(groups) != 0 {
		t.Fatalf("accounted block must yield zero activity groups, got %+v", groups)
	}
}

// A span day older than the staleness window never squats on the board: a sensor that stopped
// draining days ago yields zero activity groups even though its last day had unaccounted blocks.
func TestAttentionCLIStaleSpanDayYieldsNoActivityGroup(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	staleDay := time.Now().UTC().AddDate(0, 0, -(activityAttentionMaxAgeDays + 2)).Format("2006-01-02")
	writeActivitySpanDay(t, homePath, staleDay, []activity.ActivitySpan{{
		Start: staleDay + "T09:00:00Z", End: staleDay + "T09:47:00Z", ActiveSec: 45 * 60,
		BundleID: "com.example.browser", AppName: "Browser",
		Category: activity.CategoryBrowsing, Coverage: activity.CoverageComplete,
	}})

	if groups := activityGroupsOf(attentionReportForHome(t, homePath)); len(groups) != 0 {
		t.Fatalf("stale span day must yield zero activity groups, got %+v", groups)
	}
}

// workbenchWorkActivities pulls every rendered timestamp class: claim times plus run-observation and
// route-outcome times, across open and recent items; blank and malformed timestamps are skipped.
func TestWorkbenchWorkActivitiesExtractsRenderedTimestamps(t *testing.T) {
	report := &work.WorkbenchReport{
		Open: []work.WorkbenchItem{{
			WorkID:    "work_open",
			ClaimedAt: "2026-07-15T09:10:00Z",
			RunObservations: []work.RunObservationSummary{
				{ID: "obs_1", GeneratedAt: "2026-07-15T09:30:00Z"},
				{ID: "obs_blank", GeneratedAt: ""},
			},
			RouteOutcomes: []work.RouteOutcomeSummary{{ID: "route_1", CreatedAt: "2026-07-15T09:45:00Z"}},
		}},
		Recent: []work.WorkbenchItem{
			{WorkID: "work_recent", ClaimedAt: "not-a-time"},
			// Lease claimed_at renders as epoch NANOSECONDS (2026-07-16T09:20:00Z) — the premise
			// correction this extraction exists for; it must parse, not drop.
			{WorkID: "work_recent_nanos", ClaimedAt: "1784193600000000000"},
		},
	}
	got := workbenchWorkActivities(report)
	if len(got) != 4 {
		t.Fatalf("want 4 anchors (claim, observation, route, nanos-claim), got %d: %+v", len(got), got)
	}
	refs := map[string]time.Time{}
	for _, w := range got {
		refs[w.Ref] = w.At
	}
	for _, want := range []string{"work_open", "work_open:obs_1", "work_open:route_1", "work_recent_nanos"} {
		if _, present := refs[want]; !present {
			t.Fatalf("missing anchor ref %q in %+v", want, got)
		}
	}
	if at := refs["work_recent_nanos"].UTC(); !at.Equal(time.Date(2026, 7, 16, 9, 20, 0, 0, time.UTC)) {
		t.Fatalf("epoch-nanos claimed_at parsed to %v, want 2026-07-16T09:20:00Z", at)
	}
	if workbenchWorkActivities(nil) != nil {
		t.Fatal("nil report must yield nil anchors")
	}
}
