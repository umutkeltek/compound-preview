package statussurface

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

var testNow = time.Date(2026, 7, 17, 1, 15, 0, 0, time.UTC)

const claudeFixture = `{
  "session_id": "session-must-not-survive",
  "message": "BODY_MUST_NOT_SURVIVE",
  "model": {"display_name": "Opus 4.8"},
  "workspace": {"current_dir": "/private/project"},
  "context_window": {
    "context_window_size": 200000,
    "used_percentage": 41.5,
    "total_input_tokens": 83000
  },
  "rate_limits": {
    "five_hour": {"used_percentage": 34, "resets_at": "2026-07-17T04:00:00Z"},
    "seven_day": {"utilization": 82, "resets_at": 946684800},
    "seven_day_opus": {"used_percentage": 51},
    "seven_day_sonnet": {"used_percentage": 12}
  }
}`

func TestBuildClaudeSnapshotReducesNativeContextAndLimitsWithoutBody(t *testing.T) {
	snapshot, err := BuildClaudeSnapshot([]byte(claudeFixture), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != SchemaVersion || snapshot.Harness != HarnessClaude {
		t.Fatalf("identity = %+v", snapshot)
	}
	if snapshot.Context.Status != StatusLive || snapshot.Context.UsedPercent != 41.5 || snapshot.Context.RemainingPercent != 58.5 {
		t.Fatalf("context = %+v", snapshot.Context)
	}
	if snapshot.Context.WindowTokens != 200000 || snapshot.Model != "Opus 4.8" {
		t.Fatalf("model/context window = %q/%d", snapshot.Model, snapshot.Context.WindowTokens)
	}
	if got := limitByKind(t, snapshot, LimitFiveHour); got.UsedPercent != 34 || got.RemainingPercent != 66 || got.Status != StatusLive {
		t.Fatalf("five hour = %+v", got)
	}
	if got := limitByKind(t, snapshot, LimitSevenDay); got.UsedPercent != 82 || got.RemainingPercent != 18 || got.ResetsAt.IsZero() {
		t.Fatalf("seven day = %+v", got)
	}
	if snapshot.Mode != ModeConserve || snapshot.RouteGuidance != GuidancePreservePrimary {
		t.Fatalf("adaptive state = mode %q guidance %q", snapshot.Mode, snapshot.RouteGuidance)
	}
	if snapshot.InferenceClass != InferenceCapacity || snapshot.RecommendedTheme != ThemePAIOps {
		t.Fatalf("inference = class %q theme %q", snapshot.InferenceClass, snapshot.RecommendedTheme)
	}

	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"BODY_MUST_NOT_SURVIVE", "session-must-not-survive", "/private/project", "message", "session_id", "current_dir"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("derived snapshot retained forbidden input %q: %s", forbidden, body)
		}
	}
}

// TestBuildClaudeSnapshotDerivesContextFromTokenCounts pins the derivation
// branch of deriveClaudeContextUsage: a payload that only carries
// context_window_size and total_input_tokens (no used_percentage) must
// produce a StatusLive Context metric whose UsedPercent is the token-derived
// ratio, not the false 0% the pre-fix code path emitted.
//
// This is the common codexbar / proxy case where the upstream Claude
// status payload does not include the percentage Claude itself computes.
// Operators reading this metric need the derived value, not '0% used,
// 100% remaining'.
func TestBuildClaudeSnapshotDerivesContextFromTokenCounts(t *testing.T) {
	input := []byte(`{"context_window":{"context_window_size":200000,"total_input_tokens":83000}}`)
	snapshot, err := BuildClaudeSnapshot(input, testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Context.Status != StatusLive {
		t.Fatalf("Context.Status = %q, want %q (tokens were present, must derive)", snapshot.Context.Status, StatusLive)
	}
	// 83000 / 200000 = 0.415 = 41.5%; remaining = 58.5%.
	if !floatNear(snapshot.Context.UsedPercent, 41.5) {
		t.Fatalf("Context.UsedPercent = %v, want 41.5 (derived 83000/200000)", snapshot.Context.UsedPercent)
	}
	if !floatNear(snapshot.Context.RemainingPercent, 58.5) {
		t.Fatalf("Context.RemainingPercent = %v, want 58.5", snapshot.Context.RemainingPercent)
	}
	if snapshot.Context.WindowTokens != 200000 || snapshot.Context.UsedTokens != 83000 {
		t.Fatalf("Context.WindowTokens/UsedTokens = %d/%d, want 200000/83000", snapshot.Context.WindowTokens, snapshot.Context.UsedTokens)
	}
}

// TestBuildClaudeSnapshotLeavesContextUnknownWhenNoUsageEvidence pins the
// third branch of deriveClaudeContextUsage: when neither used_percentage
// nor token counts are present, the Context metric must be StatusUnknown.
// The pre-fix code path emitted StatusLive/0/100, which is a false
// 'plenty of context' signal. TestBuildClaudeSnapshotReducesNativeContext
// already pins the happy path with both fields.
func TestBuildClaudeSnapshotLeavesContextUnknownWhenNoUsageEvidence(t *testing.T) {
	input := []byte(`{"context_window":{"context_window_size":200000}}`)
	snapshot, err := BuildClaudeSnapshot(input, testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Context.Status != StatusUnknown {
		t.Fatalf("Context.Status = %q, want %q (no usage evidence; must not invent 0/100)", snapshot.Context.Status, StatusUnknown)
	}
	if snapshot.Context.UsedPercent != 0 || snapshot.Context.RemainingPercent != 0 {
		t.Fatalf("Context.UsedPercent/RemainingPercent = %v/%v, want 0/0", snapshot.Context.UsedPercent, snapshot.Context.RemainingPercent)
	}

	// Same expectation when context_window is entirely absent.
	noCtx, err := BuildClaudeSnapshot([]byte(`{}`), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if noCtx.Context.Status != StatusUnknown {
		t.Fatalf("Context.Status (no context_window) = %q, want %q", noCtx.Context.Status, StatusUnknown)
	}
}

// TestBuildClaudeSnapshotNativeUsedPercentWinsOverTokenCounts pins the
// priority order: native used_percentage wins over any token-derived value.
// The status payload is the canonical source; token-derived math could
// disagree with whatever compaction policy Claude applied to compute the
// reported percentage.
func TestBuildClaudeSnapshotNativeUsedPercentWinsOverTokenCounts(t *testing.T) {
	// 50% native; tokens (if the broken path were used) would yield 41.5%.
	// Native must win.
	input := []byte(`{"context_window":{"context_window_size":200000,"used_percentage":50,"total_input_tokens":83000}}`)
	snapshot, err := BuildClaudeSnapshot(input, testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !floatNear(snapshot.Context.UsedPercent, 50) {
		t.Fatalf("Context.UsedPercent = %v, want 50 (native wins over token-derived 41.5)", snapshot.Context.UsedPercent)
	}
}

// TestBuildClaudeSnapshotClampsTokenDerivedUsage pins the clamp boundary:
// token-derived usage that exceeds 100 (compression artifacts, scheduled
// compaction windows, etc.) must clamp at 100, not overflow the metric.
func TestBuildClaudeSnapshotClampsTokenDerivedUsage(t *testing.T) {
	// 250_000 / 200_000 = 1.25 = 125% before clamp; clamp to 100.
	input := []byte(`{"context_window":{"context_window_size":200000,"total_input_tokens":250000}}`)
	snapshot, err := BuildClaudeSnapshot(input, testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !floatNear(snapshot.Context.UsedPercent, 100) {
		t.Fatalf("Context.UsedPercent = %v, want 100 (clamped from 125%%)", snapshot.Context.UsedPercent)
	}
	if !floatNear(snapshot.Context.RemainingPercent, 0) {
		t.Fatalf("Context.RemainingPercent = %v, want 0", snapshot.Context.RemainingPercent)
	}
}

// floatNear is a small epsilon-aware equality helper for the percent tests.
func floatNear(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.0001
}

func TestAdaptiveModeUsesHysteresis(t *testing.T) {
	metric := Metric{Status: StatusLive, UsedPercent: 70, RemainingPercent: 30}
	if got := SelectMode(metric, nil, ModeFlow); got != ModeWatch {
		t.Fatalf("70%% from flow = %q, want watch", got)
	}
	if got := SelectMode(metric, nil, ModeConserve); got != ModeConserve {
		t.Fatalf("70%% from conserve = %q, want conserve hysteresis", got)
	}
	metric.UsedPercent, metric.RemainingPercent = 67, 33
	if got := SelectMode(metric, nil, ModeConserve); got != ModeWatch {
		t.Fatalf("67%% from conserve = %q, want watch after exit threshold", got)
	}
	metric.Status = StatusUnknown
	if got := SelectMode(metric, nil, ModeCritical); got != ModeUnknown {
		t.Fatalf("unknown evidence = %q", got)
	}
}

func TestSupplementWeeklyCapacityFillsMissingNativeLimitAndRecomputesInference(t *testing.T) {
	input := `{
	  "model": {"display_name": "Fable 5"},
	  "context_window": {"context_window_size": 1000000, "used_percentage": 20}
	}`
	snapshot, err := BuildClaudeSnapshot([]byte(input), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Mode != ModeFlow || len(snapshot.Limits) != 0 {
		t.Fatalf("native snapshot = mode %q limits %+v", snapshot.Mode, snapshot.Limits)
	}

	reset := testNow.Add(48 * time.Hour)
	merged := SupplementWeeklyCapacity(snapshot, CapacityEvidence{
		Status: StatusLive, RemainingPercent: 20, ResetsAt: reset, Source: "compound/routeevidence:codexbar/usage-history",
	}, Snapshot{})
	weekly := limitByKind(t, merged, LimitSevenDay)
	if weekly.Status != StatusLive || weekly.UsedPercent != 80 || weekly.RemainingPercent != 20 || !weekly.ResetsAt.Equal(reset) {
		t.Fatalf("supplemented weekly limit = %+v", weekly)
	}
	if weekly.Source != "compound/routeevidence:codexbar/usage-history" {
		t.Fatalf("weekly source = %q", weekly.Source)
	}
	if merged.Mode != ModeConserve || merged.RouteGuidance != GuidancePreservePrimary || merged.InferenceClass != InferenceCapacity || merged.RecommendedTheme != ThemePAIOps {
		t.Fatalf("adaptive state = mode %q guidance %q class %q theme %q", merged.Mode, merged.RouteGuidance, merged.InferenceClass, merged.RecommendedTheme)
	}
}

func TestSupplementWeeklyCapacityNeverOverridesNativeOrInventsUnknownLimit(t *testing.T) {
	native, err := BuildClaudeSnapshot([]byte(claudeFixture), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	merged := SupplementWeeklyCapacity(native, CapacityEvidence{
		Status: StatusLive, RemainingPercent: 90, Source: "compound/routeevidence:fallback",
	}, Snapshot{})
	weekly := limitByKind(t, merged, LimitSevenDay)
	if weekly.RemainingPercent != 18 || weekly.Source != "claude/statusline.rate_limits" {
		t.Fatalf("fallback replaced native weekly limit: %+v", weekly)
	}

	withoutNative, err := BuildClaudeSnapshot([]byte(`{"context_window":{"used_percentage":20}}`), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	unknown := SupplementWeeklyCapacity(withoutNative, CapacityEvidence{Status: StatusUnknown, RemainingPercent: 25}, Snapshot{})
	if len(unknown.Limits) != 0 || unknown.Mode != ModeFlow {
		t.Fatalf("unknown evidence invented capacity: mode %q limits %+v", unknown.Mode, unknown.Limits)
	}
}

func TestSupplementFiveHourCapacityFillsMissingNativeLimitWithoutOverridingIt(t *testing.T) {
	snapshot, err := BuildClaudeSnapshot([]byte(`{"context_window":{"used_percentage":20}}`), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	reset := testNow.Add(3 * time.Hour)
	merged := SupplementFiveHourCapacity(snapshot, CapacityEvidence{
		Status: StatusLive, RemainingPercent: 63, ResetsAt: reset, Source: "compound/codexbar-live",
	}, Snapshot{})
	five := limitByKind(t, merged, LimitFiveHour)
	if five.UsedPercent != 37 || five.RemainingPercent != 63 || !five.ResetsAt.Equal(reset) || five.Source != "compound/codexbar-live" {
		t.Fatalf("five-hour supplement = %+v", five)
	}

	native, err := BuildClaudeSnapshot([]byte(claudeFixture), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	native = SupplementFiveHourCapacity(native, CapacityEvidence{Status: StatusLive, RemainingPercent: 99, Source: "fallback"}, Snapshot{})
	if got := limitByKind(t, native, LimitFiveHour); got.RemainingPercent != 66 || got.Source != "claude/statusline.rate_limits" {
		t.Fatalf("fallback replaced native five-hour limit: %+v", got)
	}
}

func TestSnapshotStoreIsAtomicPrivateAndStaleAware(t *testing.T) {
	home := t.TempDir()
	snapshot, err := BuildClaudeSnapshot([]byte(claudeFixture), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Home: home, TTL: 2 * time.Minute}
	if err := store.Write(&snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.FreshUntil.Equal(testNow.Add(2 * time.Minute)) {
		t.Fatalf("write did not return the persisted freshness boundary: %s", snapshot.FreshUntil)
	}
	path := SnapshotPath(home, HarnessClaude)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %o, want 600", info.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "BODY_MUST_NOT_SURVIVE") {
		t.Fatalf("snapshot persisted an input body: %s", body)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("atomic write leaked temps: %v", temps)
	}
	if got, fresh, err := store.ReadFresh(HarnessClaude, testNow.Add(time.Minute)); err != nil || !fresh || got.Mode != ModeConserve {
		t.Fatalf("fresh read = %+v fresh=%v err=%v", got, fresh, err)
	}
	if got, fresh, err := store.ReadFresh(HarnessClaude, testNow.Add(3*time.Minute)); err != nil || fresh || got.Mode != ModeUnknown {
		t.Fatalf("stale read = %+v fresh=%v err=%v", got, fresh, err)
	}
}

func TestRenderThemesAndCodexThemeAreDeterministic(t *testing.T) {
	snapshot, err := BuildClaudeSnapshot([]byte(claudeFixture), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	// The three rich themes render a two-row instrument panel and are allowed
	// wide glyphs. They must still be deterministic and carry the context meter
	// with absolute token counts.
	for _, theme := range []string{ThemePAINexus, ThemePAIFocus, ThemePAIOps} {
		line, err := Render(snapshot, RenderOptions{Theme: theme, Width: 120, Color: false})
		if err != nil {
			t.Fatalf("render %s: %v", theme, err)
		}
		again, err := Render(snapshot, RenderOptions{Theme: theme, Width: 120, Color: false})
		if err != nil || again != line {
			t.Fatalf("render %s not deterministic: %q vs %q (err=%v)", theme, line, again, err)
		}
		rows := strings.Split(line, "\n")
		if len(rows) != 2 {
			t.Fatalf("render %s wanted 2 rows, got %d: %q", theme, len(rows), line)
		}
		if !strings.Contains(line, "CONSERVE") {
			t.Fatalf("render %s missing mode: %q", theme, line)
		}
		// ~41.75% raw used -> 41.75/0.835 = 50% compaction-aware pressure,
		// versus the 59%-remaining figure pai-mono reports.
		if !strings.Contains(line, "50%") {
			t.Fatalf("render %s missing compaction-aware context pressure: %q", theme, line)
		}
		if !strings.Contains(line, "83k/200k") {
			t.Fatalf("render %s missing absolute token counts: %q", theme, line)
		}
	}

	// pai-mono is the fallback contract: one row, pure ASCII, never coloured.
	// Terminals without truecolor or wide-glyph support depend on this holding.
	mono, err := Render(snapshot, RenderOptions{Theme: ThemePAIMono, Width: 120, Color: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mono, "\x1b[") {
		t.Fatalf("mono theme contains ANSI: %q", mono)
	}
	if strings.ContainsAny(mono, "\n\r") {
		t.Fatalf("mono theme is not one line: %q", mono)
	}
	if !strings.HasPrefix(mono, ".. CLAUDE:CONSERVE ..") || !strings.Contains(mono, "ctx [######----] 59%") {
		t.Fatalf("mono render = %q", mono)
	}
	for _, r := range mono {
		if r > unicode.MaxASCII {
			t.Fatalf("mono render contains non-ASCII rune %q: %q", r, mono)
		}
	}

	// Truncation must respect the visible-column budget on every row.
	short, err := Render(snapshot, RenderOptions{Theme: ThemePAINexus, Width: 34, Color: false})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(short, "\n") {
		if displayWidth(row) > 34 {
			t.Fatalf("render width = %d: %q", displayWidth(row), row)
		}
	}
	// Coloured truncation must not leak styling past the cut.
	shortColor, err := Render(snapshot, RenderOptions{Theme: ThemePAINexus, Width: 30, Color: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(shortColor, "\n") {
		if displayWidth(row) > 30 {
			t.Fatalf("coloured render width = %d: %q", displayWidth(row), row)
		}
		if strings.Contains(row, "\x1b[") && !strings.HasSuffix(row, ansiReset) {
			t.Fatalf("coloured row does not reset styling: %q", row)
		}
	}
	auto, err := Render(snapshot, RenderOptions{Theme: ThemeAuto, Width: 120, Color: false})
	if err != nil {
		t.Fatal(err)
	}
	// CONSERVE maps to the capacity information profile, so auto must resolve to
	// exactly what pai-ops renders.
	ops, err := Render(snapshot, RenderOptions{Theme: ThemePAIOps, Width: 120, Color: false})
	if err != nil {
		t.Fatal(err)
	}
	if auto != ops {
		t.Fatalf("auto theme did not select capacity information profile: %q, want %q", auto, ops)
	}

	first, err := RenderCodexTheme(ThemePAINexus)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderCodexTheme(ThemePAINexus)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.Contains(first, "<plist version=\"1.0\">") || !strings.Contains(first, "PAI Nexus") {
		t.Fatalf("codex theme is not deterministic TextMate XML: %q", first)
	}
}

func TestClaudeRenderHasNoPAIBranding(t *testing.T) {
	snapshot, err := BuildClaudeSnapshot([]byte(claudeFixture), testNow, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	for _, theme := range ThemeIDs() {
		line, err := Render(snapshot, RenderOptions{Theme: theme, Width: 120, Color: false})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToUpper(line), "PAI") {
			t.Fatalf("Claude render leaked retired branding: %q", line)
		}
	}
}

// Capacity supplemented from route evidence populates only RemainingPercent.
// Reading UsedPercent alone renders those windows as 0% -- a silent "plenty of
// headroom" lie exactly when capacity is scarce -- so consumed percentage must
// be derived from whichever side the producer filled in.
func TestClaudeRenderDerivesConsumedPercentFromRemainingOnlySnapshots(t *testing.T) {
	watch := Snapshot{
		Harness: HarnessClaude,
		Context: Metric{Status: StatusLive, RemainingPercent: 72},
		Limits: []LimitWindow{
			{Kind: LimitFiveHour, Status: StatusLive, RemainingPercent: 81},
			{Kind: LimitSevenDay, Status: StatusLive, RemainingPercent: 43},
		},
		Mode: ModeWatch,
	}
	line, err := Render(watch, RenderOptions{Theme: ThemePAINexus, Width: 120})
	if err != nil {
		t.Fatal(err)
	}
	// 72% remaining -> 28% used -> 28/0.835 ~= 34% compaction-aware pressure.
	// 5h: 81% remaining -> 19% used. wk: 43% remaining -> 57% used.
	for _, want := range []string{"34%", "5h ", "19%", "wk ", "57%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("watch render = %q, missing %q", line, want)
		}
	}
	if strings.Contains(line, " 0%") {
		t.Fatalf("remaining-only snapshot rendered a 0%% window: %q", line)
	}

	unknown := Snapshot{Harness: HarnessClaude, Context: Metric{Status: StatusUnknown}, Mode: ModeUnknown}
	line, err = Render(unknown, RenderOptions{Theme: ThemePAIMono, Width: 120})
	if err != nil {
		t.Fatal(err)
	}
	if want := ".. CLAUDE:UNKNOWN .. left: ctx [??????????] ? | 5h ? | wk ?"; line != want {
		t.Fatalf("unknown render = %q, want %q", line, want)
	}
}

func TestInferDisplayClassChangesThemeAndInformationProfile(t *testing.T) {
	for _, tc := range []struct {
		mode  Mode
		class InferenceClass
		theme string
	}{
		{ModeFlow, InferenceFocus, ThemePAIFocus},
		{ModeWatch, InferenceBalanced, ThemePAINexus},
		{ModeConserve, InferenceCapacity, ThemePAIOps},
		{ModeCritical, InferenceCapacity, ThemePAIOps},
		{ModeUnknown, InferenceFallback, ThemePAIMono},
	} {
		class, theme := InferDisplayClass(tc.mode)
		if class != tc.class || theme != tc.theme {
			t.Fatalf("mode %q = class %q theme %q, want %q/%q", tc.mode, class, theme, tc.class, tc.theme)
		}
	}
}

func limitByKind(t *testing.T, snapshot Snapshot, kind LimitKind) LimitWindow {
	t.Helper()
	for _, limit := range snapshot.Limits {
		if limit.Kind == kind {
			return limit
		}
	}
	t.Fatalf("missing limit %q in %+v", kind, snapshot.Limits)
	return LimitWindow{}
}
