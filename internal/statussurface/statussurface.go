// Package statussurface reduces harness-native context and capacity metadata into one adaptive,
// body-free projection for terminal status surfaces. Harnesses remain authoritative for live
// context and routeevidence remains authoritative for capacity.
package statussurface

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	SchemaVersion = "compound.status_surface.v0.1"
	defaultTTL    = 2 * time.Minute

	ansiReset = "\x1b[0m"

	ThemePAINexus = "pai-nexus"
	ThemePAIFocus = "pai-focus"
	ThemePAIOps   = "pai-ops"
	ThemePAIMono  = "pai-mono"
	ThemeAuto     = "auto"
)

type Harness string

const (
	HarnessClaude Harness = "claude"
	HarnessCodex  Harness = "codex"
)

type MetricStatus string

const (
	StatusLive    MetricStatus = "live"
	StatusStale   MetricStatus = "stale"
	StatusUnknown MetricStatus = "unknown"
)

type Mode string

const (
	ModeFlow     Mode = "flow"
	ModeWatch    Mode = "watch"
	ModeConserve Mode = "conserve"
	ModeCritical Mode = "critical"
	ModeUnknown  Mode = "unknown"
)

type RouteGuidance string

const (
	GuidanceNormal           RouteGuidance = "normal"
	GuidancePreferEfficient  RouteGuidance = "prefer_efficient"
	GuidancePreservePrimary  RouteGuidance = "preserve_primary"
	GuidanceRouteAlternative RouteGuidance = "route_alternative"
	GuidanceConfirmCapacity  RouteGuidance = "confirm_capacity"
)

// InferenceClass selects an information profile, not merely a color palette. It is derived
// deterministically from current capacity state and can be overridden by choosing a concrete theme.
type InferenceClass string

const (
	InferenceFocus    InferenceClass = "focus"
	InferenceBalanced InferenceClass = "balanced"
	InferenceCapacity InferenceClass = "capacity"
	InferenceFallback InferenceClass = "fallback"
)

type LimitKind string

const (
	LimitFiveHour       LimitKind = "five_hour"
	LimitSevenDay       LimitKind = "seven_day"
	LimitSevenDayOpus   LimitKind = "seven_day_opus"
	LimitSevenDaySonnet LimitKind = "seven_day_sonnet"
)

type Metric struct {
	Status           MetricStatus `json:"status"`
	UsedPercent      float64      `json:"used_percent,omitempty"`
	RemainingPercent float64      `json:"remaining_percent,omitempty"`
	WindowTokens     int64        `json:"window_tokens,omitempty"`
	UsedTokens       int64        `json:"used_tokens,omitempty"`
	Source           string       `json:"source,omitempty"`
}

// compactionUsableFraction is the share of the advertised context window that is
// actually usable before Claude Code compacts; it reserves the remainder as
// compaction overhead. Reporting raw used_percentage reads optimistically -- a
// raw 83% means roughly nothing usable is left -- so the displayed context
// pressure is scaled against this fraction instead.
const compactionUsableFraction = 0.835

type LimitWindow struct {
	Kind             LimitKind    `json:"kind"`
	Status           MetricStatus `json:"status"`
	UsedPercent      float64      `json:"used_percent,omitempty"`
	RemainingPercent float64      `json:"remaining_percent,omitempty"`
	ResetsAt         time.Time    `json:"resets_at,omitempty"`
	Source           string       `json:"source,omitempty"`
}

type Snapshot struct {
	SchemaVersion    string         `json:"schema_version"`
	GeneratedAt      time.Time      `json:"generated_at"`
	FreshUntil       time.Time      `json:"fresh_until,omitempty"`
	Harness          Harness        `json:"harness"`
	Model            string         `json:"model,omitempty"`
	Context          Metric         `json:"context"`
	Limits           []LimitWindow  `json:"limits"`
	Mode             Mode           `json:"mode"`
	RouteGuidance    RouteGuidance  `json:"route_guidance"`
	InferenceClass   InferenceClass `json:"inference_class"`
	RecommendedTheme string         `json:"recommended_theme"`
	InputRetention   string         `json:"input_retention"`
}

type CapacityEvidence struct {
	Status           MetricStatus
	RemainingPercent float64
	ResetsAt         time.Time
	Source           string
}

// SelectMode applies entry thresholds plus exit hysteresis. The prior mode only delays a
// downgrade; fresh evidence can always escalate immediately.
func SelectMode(primary Metric, additional []Metric, previous Mode) Mode {
	worst := -1.0
	for _, metric := range append([]Metric{primary}, additional...) {
		if metric.Status == StatusLive && metric.UsedPercent > worst {
			worst = metric.UsedPercent
		}
	}
	if worst < 0 {
		return ModeUnknown
	}
	switch {
	case worst >= 90:
		return ModeCritical
	case previous == ModeCritical && worst >= 82:
		return ModeCritical
	case worst >= 75:
		return ModeConserve
	case previous == ModeConserve && worst >= 68:
		return ModeConserve
	case worst >= 50:
		return ModeWatch
	case previous == ModeWatch && worst >= 45:
		return ModeWatch
	default:
		return ModeFlow
	}
}

func guidanceForMode(mode Mode) RouteGuidance {
	switch mode {
	case ModeFlow:
		return GuidanceNormal
	case ModeWatch:
		return GuidancePreferEfficient
	case ModeConserve:
		return GuidancePreservePrimary
	case ModeCritical:
		return GuidanceRouteAlternative
	default:
		return GuidanceConfirmCapacity
	}
}

func InferDisplayClass(mode Mode) (InferenceClass, string) {
	switch mode {
	case ModeFlow:
		return InferenceFocus, ThemePAIFocus
	case ModeWatch:
		return InferenceBalanced, ThemePAINexus
	case ModeConserve, ModeCritical:
		return InferenceCapacity, ThemePAIOps
	default:
		return InferenceFallback, ThemePAIMono
	}
}

type claudeStatusInput struct {
	Model struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	ContextWindow *struct {
		WindowTokens int64 `json:"context_window_size"`
		// UsedPercent is a *float64 (not float64) so an absent
		// `used_percentage` field does not silently render as 0%.
		// finitePercent(0) is true (0 is not NaN, not Inf), so a plain
		// float64 here would let a payload without `used_percentage`
		// (only `total_input_tokens` + `context_window_size`) be
		// reported as StatusLive, UsedPercent=0 -- a false 'plenty of
		// context' signal. Pinned by
		// TestBuildClaudeSnapshotLeavesContextUnknownWhenNoUsageEvidence.
		UsedPercent *float64 `json:"used_percentage"`
		UsedTokens  int64    `json:"total_input_tokens"`
	} `json:"context_window"`
	RateLimits *struct {
		FiveHour       *nativeLimit `json:"five_hour"`
		SevenDay       *nativeLimit `json:"seven_day"`
		SevenDayOpus   *nativeLimit `json:"seven_day_opus"`
		SevenDaySonnet *nativeLimit `json:"seven_day_sonnet"`
	} `json:"rate_limits"`
}

type nativeLimit struct {
	UsedPercent *float64        `json:"used_percentage"`
	Utilization *float64        `json:"utilization"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

func BuildClaudeSnapshot(body []byte, now time.Time, previous Snapshot) (Snapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var input claudeStatusInput
	if err := decoder.Decode(&input); err != nil {
		return Snapshot{}, fmt.Errorf("decode Claude status input: %w", err)
	}

	snapshot := Snapshot{
		SchemaVersion:  SchemaVersion,
		GeneratedAt:    now.UTC(),
		Harness:        HarnessClaude,
		Model:          safeLabel(input.Model.DisplayName, 80),
		Context:        Metric{Status: StatusUnknown, Source: "claude/statusline.context_window"},
		Limits:         []LimitWindow{},
		InputRetention: "derived_metadata_only;raw_discarded",
	}
	// Context window resolution: prefer the native `used_percentage` when it is
	// present and finite; otherwise derive from `total_input_tokens` and
	// `context_window_size`; otherwise leave the context metric Unknown. The
	// previous code path fell into the first branch with UsedPercent=0 because
	// finitePercent(0) is true (0 is neither NaN nor Inf) -- a payload that
	// only carried total_input_tokens + context_window_size rendered as
	// StatusLive, UsedPercent=0, RemainingPercent=100, which is a false
	// 'plenty of context' signal. UsedPercent is now *float64 precisely so
	// the absent case can be detected and fall through to derivation.
	//
	// The outer nil check on input.ContextWindow is required because the JSON
	// unmarshaler leaves the pointer nil when context_window is entirely
	// absent from the payload; deriveClaudeContextUsage handles the
	// partial-evidence case (one field present, one absent) and the
	// no-evidence case (both absent) but cannot dereference a nil parent.
	var cwWindowTokens int64
	var cwUsedPercent *float64
	var cwUsedTokens int64
	if input.ContextWindow != nil {
		cwWindowTokens = input.ContextWindow.WindowTokens
		cwUsedPercent = input.ContextWindow.UsedPercent
		cwUsedTokens = input.ContextWindow.UsedTokens
	}
	if used, ok := deriveClaudeContextUsage(cwWindowTokens, cwUsedPercent, cwUsedTokens); ok {
		snapshot.Context = Metric{
			Status:           StatusLive,
			UsedPercent:      used,
			RemainingPercent: 100 - used,
			WindowTokens:     cwWindowTokens,
			UsedTokens:       cwUsedTokens,
			Source:           "claude/statusline.context_window",
		}
	}
	if input.RateLimits != nil {
		for _, pair := range []struct {
			kind  LimitKind
			value *nativeLimit
		}{
			{LimitFiveHour, input.RateLimits.FiveHour},
			{LimitSevenDay, input.RateLimits.SevenDay},
			{LimitSevenDayOpus, input.RateLimits.SevenDayOpus},
			{LimitSevenDaySonnet, input.RateLimits.SevenDaySonnet},
		} {
			if pair.value != nil {
				snapshot.Limits = append(snapshot.Limits, reduceNativeLimit(pair.kind, pair.value))
			}
		}
	}
	additional := make([]Metric, 0, len(snapshot.Limits))
	for _, limit := range snapshot.Limits {
		additional = append(additional, Metric{Status: limit.Status, UsedPercent: limit.UsedPercent, RemainingPercent: limit.RemainingPercent})
	}
	previousMode := ModeUnknown
	if previous.SchemaVersion == SchemaVersion && previous.Harness == HarnessClaude {
		previousMode = previous.Mode
	}
	snapshot.Mode = SelectMode(snapshot.Context, additional, previousMode)
	snapshot.RouteGuidance = guidanceForMode(snapshot.Mode)
	snapshot.InferenceClass, snapshot.RecommendedTheme = InferDisplayClass(snapshot.Mode)
	return snapshot, nil
}

// SupplementWeeklyCapacity fills the weekly window only when Claude's native status payload did
// not provide one. Native harness evidence always wins; unknown fallback evidence stays unknown.
func SupplementWeeklyCapacity(snapshot Snapshot, evidence CapacityEvidence, previous Snapshot) Snapshot {
	return supplementClaudeCapacity(snapshot, evidence, previous, LimitSevenDay)
}

// SupplementFiveHourCapacity fills the five-hour window only when Claude's native status payload
// did not provide one. Native harness evidence always wins; unknown fallback evidence stays unknown.
func SupplementFiveHourCapacity(snapshot Snapshot, evidence CapacityEvidence, previous Snapshot) Snapshot {
	return supplementClaudeCapacity(snapshot, evidence, previous, LimitFiveHour)
}

func supplementClaudeCapacity(snapshot Snapshot, evidence CapacityEvidence, previous Snapshot, kind LimitKind) Snapshot {
	if snapshot.Harness != HarnessClaude || evidence.Status != StatusLive {
		return snapshot
	}
	for _, limit := range snapshot.Limits {
		if limit.Kind == kind && limit.Status == StatusLive {
			return snapshot
		}
	}

	supplement := LimitWindow{
		Kind:             kind,
		Status:           StatusLive,
		RemainingPercent: clampPercent(evidence.RemainingPercent),
		ResetsAt:         evidence.ResetsAt,
		Source:           evidence.Source,
	}
	supplement.UsedPercent = 100 - supplement.RemainingPercent
	replaced := false
	for index := range snapshot.Limits {
		if snapshot.Limits[index].Kind == kind {
			snapshot.Limits[index] = supplement
			replaced = true
			break
		}
	}
	if !replaced {
		snapshot.Limits = append(snapshot.Limits, supplement)
	}

	additional := make([]Metric, 0, len(snapshot.Limits))
	for _, limit := range snapshot.Limits {
		additional = append(additional, Metric{Status: limit.Status, UsedPercent: limit.UsedPercent, RemainingPercent: limit.RemainingPercent})
	}
	previousMode := ModeUnknown
	if previous.SchemaVersion == SchemaVersion && previous.Harness == HarnessClaude {
		previousMode = previous.Mode
	}
	snapshot.Mode = SelectMode(snapshot.Context, additional, previousMode)
	snapshot.RouteGuidance = guidanceForMode(snapshot.Mode)
	snapshot.InferenceClass, snapshot.RecommendedTheme = InferDisplayClass(snapshot.Mode)
	return snapshot
}

// BuildCodexSnapshot adapts Compound's already-reduced capacity evidence. Exact Codex context and
// weekly values stay visible through the native footer; this projection never reads a session body.
func BuildCodexSnapshot(evidence CapacityEvidence, now time.Time, previous Snapshot) Snapshot {
	limit := LimitWindow{Kind: LimitSevenDay, Status: evidence.Status, ResetsAt: evidence.ResetsAt, Source: evidence.Source}
	if evidence.Status == StatusLive {
		limit.RemainingPercent = clampPercent(evidence.RemainingPercent)
		limit.UsedPercent = 100 - limit.RemainingPercent
	}
	previousMode := ModeUnknown
	if previous.SchemaVersion == SchemaVersion && previous.Harness == HarnessCodex {
		previousMode = previous.Mode
	}
	snapshot := Snapshot{
		SchemaVersion:  SchemaVersion,
		GeneratedAt:    now.UTC(),
		Harness:        HarnessCodex,
		Context:        Metric{Status: StatusUnknown, Source: "codex/native-footer"},
		Limits:         []LimitWindow{limit},
		InputRetention: "derived_metadata_only;no_session_input",
	}
	snapshot.Mode = SelectMode(snapshot.Context, []Metric{{Status: limit.Status, UsedPercent: limit.UsedPercent, RemainingPercent: limit.RemainingPercent}}, previousMode)
	snapshot.RouteGuidance = guidanceForMode(snapshot.Mode)
	snapshot.InferenceClass, snapshot.RecommendedTheme = InferDisplayClass(snapshot.Mode)
	return snapshot
}

func reduceNativeLimit(kind LimitKind, input *nativeLimit) LimitWindow {
	out := LimitWindow{Kind: kind, Status: StatusUnknown, Source: "claude/statusline.rate_limits"}
	value := input.UsedPercent
	if value == nil {
		value = input.Utilization
	}
	if value != nil && finitePercent(*value) {
		out.Status = StatusLive
		out.UsedPercent = clampPercent(*value)
		out.RemainingPercent = 100 - out.UsedPercent
	}
	out.ResetsAt = parseNativeReset(input.ResetsAt)
	return out
}

func parseNativeReset(raw json.RawMessage) time.Time {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return time.Time{}
	}
	if strings.HasPrefix(trimmed, "\"") {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
				return parsed.UTC()
			}
		}
		return time.Time{}
	}
	epoch, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if epoch > 10_000_000_000 {
		epoch /= 1000
	}
	return time.Unix(epoch, 0).UTC()
}

func finitePercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// deriveClaudeContextUsage decides what (if any) UsedPercent to report for the
// Claude context window. Three outcomes:
//
//  1. used_percentage present and finite: native percent wins. The status
//     payload is the canonical source, and any token-derived math could
//     disagree with whatever compaction policy Claude applied to compute
//     the reported percentage.
//  2. used_percentage absent, but both context_window_size and
//     total_input_tokens present: derive clampPercent(100 * used / window).
//     This is the common codexbar / proxy case where the upstream payload
//     only carries raw token counts. Capped at 100 (UsedTokens > Window
//     after compression artifacts is observable in practice).
//  3. no usage evidence at all: ok=false. The caller leaves the context
//     metric Unknown instead of falsely reporting 0% / 100% remaining.
//
// Pinned by:
//
//	TestBuildClaudeSnapshotNativeUsedPercentWinsOverTokenCounts
//	TestBuildClaudeSnapshotDerivesContextFromTokenCounts
//	TestBuildClaudeSnapshotLeavesContextUnknownWhenNoUsageEvidence
//	TestBuildClaudeSnapshotClampsTokenDerivedUsage
func deriveClaudeContextUsage(windowTokens int64, usedPercent *float64, usedTokens int64) (used float64, ok bool) {
	if usedPercent != nil && finitePercent(*usedPercent) {
		return clampPercent(*usedPercent), true
	}
	if windowTokens > 0 && usedTokens > 0 {
		return clampPercent(100 * float64(usedTokens) / float64(windowTokens)), true
	}
	return 0, false
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func safeLabel(value string, maxRunes int) string {
	var out []rune
	for _, r := range strings.TrimSpace(value) {
		if unicode.IsControl(r) {
			continue
		}
		out = append(out, r)
		if len(out) == maxRunes {
			break
		}
	}
	return string(out)
}

type Store struct {
	Home string
	TTL  time.Duration
}

func SnapshotPath(home string, harness Harness) string {
	return filepath.Join(home, "projections", "status-surface", string(harness)+".json")
}

func (s Store) Write(snapshot *Snapshot) error {
	if snapshot == nil {
		return errors.New("status surface snapshot required")
	}
	if snapshot.SchemaVersion != SchemaVersion {
		return fmt.Errorf("status surface schema = %q, want %q", snapshot.SchemaVersion, SchemaVersion)
	}
	if snapshot.Harness != HarnessClaude && snapshot.Harness != HarnessCodex {
		return fmt.Errorf("unsupported status surface harness %q", snapshot.Harness)
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	snapshot.FreshUntil = snapshot.GeneratedAt.Add(ttl).UTC()
	body, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := SnapshotPath(s.Home, snapshot.Harness)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (s Store) ReadFresh(harness Harness, now time.Time) (Snapshot, bool, error) {
	body, err := os.ReadFile(SnapshotPath(s.Home, harness))
	if errors.Is(err, os.ErrNotExist) {
		return unknownSnapshot(harness, now), false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return Snapshot{}, false, fmt.Errorf("decode status surface snapshot: %w", err)
	}
	if snapshot.SchemaVersion != SchemaVersion || snapshot.Harness != harness {
		return Snapshot{}, false, fmt.Errorf("status surface snapshot identity mismatch")
	}
	freshUntil := snapshot.FreshUntil
	if freshUntil.IsZero() {
		ttl := s.TTL
		if ttl <= 0 {
			ttl = defaultTTL
		}
		freshUntil = snapshot.GeneratedAt.Add(ttl)
	}
	if now.After(freshUntil) {
		snapshot.Mode = ModeUnknown
		snapshot.RouteGuidance = GuidanceConfirmCapacity
		snapshot.InferenceClass, snapshot.RecommendedTheme = InferDisplayClass(ModeUnknown)
		if snapshot.Context.Status == StatusLive {
			snapshot.Context.Status = StatusStale
		}
		for i := range snapshot.Limits {
			if snapshot.Limits[i].Status == StatusLive {
				snapshot.Limits[i].Status = StatusStale
			}
		}
		return snapshot, false, nil
	}
	return snapshot, true, nil
}

func unknownSnapshot(harness Harness, now time.Time) Snapshot {
	return Snapshot{SchemaVersion: SchemaVersion, GeneratedAt: now.UTC(), Harness: harness,
		Context: Metric{Status: StatusUnknown}, Limits: []LimitWindow{}, Mode: ModeUnknown,
		RouteGuidance: GuidanceConfirmCapacity, InferenceClass: InferenceFallback,
		RecommendedTheme: ThemePAIMono, InputRetention: "derived_metadata_only;raw_discarded"}
}

type RenderOptions struct {
	Theme string
	Width int
	Color bool
}

func ThemeIDs() []string {
	return []string{ThemePAINexus, ThemePAIFocus, ThemePAIOps, ThemePAIMono}
}

func Render(snapshot Snapshot, options RenderOptions) (string, error) {
	if options.Theme == "" || options.Theme == ThemeAuto {
		options.Theme = snapshot.RecommendedTheme
		if options.Theme == "" {
			_, options.Theme = InferDisplayClass(snapshot.Mode)
		}
	}
	if !validTheme(options.Theme) {
		return "", fmt.Errorf("unknown status surface theme %q", options.Theme)
	}
	ctx := percentText(snapshot.Context)
	five := limitText(snapshot.Limits, LimitFiveHour)
	week := limitText(snapshot.Limits, LimitSevenDay)
	mode := strings.ToUpper(string(snapshot.Mode))
	if mode == "" {
		mode = strings.ToUpper(string(ModeUnknown))
	}
	var line string
	if snapshot.Harness == HarnessClaude {
		if options.Theme != ThemePAIMono {
			return renderClaudeInfographic(snapshot, options, mode), nil
		}
		// pai-mono stays a single plain-ASCII, uncoloured line: it is the
		// fallback for terminals without truecolor or wide-glyph support.
		bar := remainingBar(snapshot.Context)
		ctxField := ctx
		if tokens := tokenText(snapshot.Context); tokens != "" {
			ctxField = ctx + " " + tokens
		}
		line = fmt.Sprintf(".. CLAUDE:%s .. left: ctx %s %s | 5h %s | wk %s",
			mode, bar, ctxField, five, week)
		if options.Width > 0 {
			line = truncateDisplay(line, options.Width)
		}
		return line, nil
	} else {
		switch options.Theme {
		case ThemePAINexus:
			line = fmt.Sprintf("< PAI:NEXUS > %s :: ctx %s :: 5h %s :: wk %s :: %s", strings.ToUpper(string(snapshot.Harness)), ctx, five, week, mode)
		case ThemePAIFocus:
			line = fmt.Sprintf("// PAI.FOCUS // ctx %s :: wk %s :: %s", ctx, week, mode)
		case ThemePAIOps:
			line = fmt.Sprintf("!! PAI.OPS !! [%s] ctx %s | 5h %s | wk %s", mode, ctx, five, week)
		case ThemePAIMono:
			line = fmt.Sprintf(".. PAI.MONO .. %s ctx %s 5h %s wk %s %s", strings.ToUpper(string(snapshot.Harness)), ctx, five, week, mode)
		}
	}
	if options.Width > 0 {
		line = truncateDisplay(line, options.Width)
	}
	if options.Color && options.Theme != ThemePAIMono {
		line = colorForMode(snapshot.Mode) + line + "\x1b[0m"
	}
	return line, nil
}

// ---------------------------------------------------------------------------
// Infographic rendering
//
// Three themes render a two-row instrument panel: an identity row (model, mode,
// inference class) and a meter row (context pressure with absolute token counts,
// then the capacity windows). pai-mono is deliberately excluded and stays a
// single plain-ASCII line as the no-truecolor fallback.
// ---------------------------------------------------------------------------

type themeGlyphs struct {
	full    string
	empty   string
	edge    string // leading identity marker
	sep     string
	barCols int
}

func glyphsForTheme(theme string) themeGlyphs {
	switch theme {
	case ThemePAINexus:
		return themeGlyphs{full: "▰", empty: "▱", edge: "◆", sep: "▏", barCols: 14}
	case ThemePAIFocus:
		return themeGlyphs{full: "━", empty: "─", edge: "◇", sep: "·", barCols: 16}
	default: // ThemePAIOps -- densest, dashboard styling
		return themeGlyphs{full: "█", empty: "░", edge: "▮", sep: "│", barCols: 18}
	}
}

func rgb(r, g, b int) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b) }

// heatColor maps 0..100 pressure onto green -> amber -> orange -> red.
func heatColor(pct float64) string {
	switch {
	case pct >= 90:
		return rgb(239, 68, 68)
	case pct >= 75:
		return rgb(251, 146, 60)
	case pct >= 50:
		return rgb(250, 204, 21)
	case pct >= 25:
		return rgb(163, 230, 53)
	default:
		return rgb(74, 222, 128)
	}
}

// effectiveUsed resolves consumed percentage from whichever side a producer
// populated. Supplemented capacity evidence carries only RemainingPercent, so
// reading UsedPercent alone would silently render those windows as 0%.
func effectiveUsed(used, remaining float64) float64 {
	if used > 0 {
		return clampPercent(used)
	}
	if remaining > 0 {
		return clampPercent(100 - remaining)
	}
	return 0
}

// contextPressure scales consumed context against the fraction of the window
// that is usable before compaction, so a full bar means "compacting now" rather
// than "plenty left". Returns 0..100.
func contextPressure(metric Metric) float64 {
	if metric.Status != StatusLive {
		return 0
	}
	used := effectiveUsed(metric.UsedPercent, metric.RemainingPercent)
	return clampPercent(used / compactionUsableFraction)
}

func formatTokenCount(n int64) string {
	switch {
	case n <= 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 100_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
}

// tokenText renders absolute context consumption, e.g. "142k/480k". Empty when
// the harness did not report token counts.
func tokenText(metric Metric) string {
	if metric.Status != StatusLive || metric.WindowTokens <= 0 || metric.UsedTokens <= 0 {
		return ""
	}
	return formatTokenCount(metric.UsedTokens) + "/" + formatTokenCount(metric.WindowTokens)
}

func meterBar(pct float64, g themeGlyphs, color bool) string {
	filled := int(math.Round(pct / 100 * float64(g.barCols)))
	if filled < 0 {
		filled = 0
	}
	if filled > g.barCols {
		filled = g.barCols
	}
	var b strings.Builder
	if color {
		b.WriteString(heatColor(pct))
	}
	b.WriteString(strings.Repeat(g.full, filled))
	if color {
		b.WriteString(rgb(71, 85, 105))
	}
	b.WriteString(strings.Repeat(g.empty, g.barCols-filled))
	if color {
		b.WriteString(ansiReset)
	}
	return b.String()
}

func renderClaudeInfographic(snapshot Snapshot, options RenderOptions, mode string) string {
	g := glyphsForTheme(options.Theme)
	color := options.Color

	paint := func(code, text string) string {
		if !color {
			return text
		}
		return code + text + ansiReset
	}
	label := func(text string) string { return paint(rgb(148, 163, 184), text) }
	value := func(text string) string { return paint(rgb(203, 213, 225), text) }
	sep := paint(rgb(71, 85, 105), " "+g.sep+" ")

	model := snapshot.Model
	if model == "" {
		model = "unknown model"
	}

	// Identity row.
	head := []string{
		paint(colorForMode(snapshot.Mode), g.edge+" "+strings.ToUpper(mode)),
		value(safeLabel(model, 34)),
	}
	if snapshot.InferenceClass != "" {
		head = append(head, label(string(snapshot.InferenceClass)))
	}
	// Route guidance is only worth a slot when it is actually steering.
	if snapshot.RouteGuidance != "" && snapshot.RouteGuidance != GuidanceNormal {
		head = append(head, label(strings.ReplaceAll(string(snapshot.RouteGuidance), "_", " ")))
	}

	// Meter row.
	meters := []string{}
	if snapshot.Context.Status == StatusLive {
		p := contextPressure(snapshot.Context)
		seg := label("ctx ") + meterBar(p, g, color) + " " +
			paint(heatColor(p), fmt.Sprintf("%d%%", int(math.Round(p))))
		if tokens := tokenText(snapshot.Context); tokens != "" {
			seg += " " + value(tokens)
		}
		meters = append(meters, seg)
	} else {
		meters = append(meters, label("ctx ")+meterBar(0, g, color)+" "+label("--"))
	}
	for _, kind := range []LimitKind{LimitFiveHour, LimitSevenDay} {
		name := "5h"
		if kind == LimitSevenDay {
			name = "wk"
		}
		for _, limit := range snapshot.Limits {
			if limit.Kind != kind {
				continue
			}
			if limit.Status != StatusLive {
				meters = append(meters, label(name+" --"))
				break
			}
			used := effectiveUsed(limit.UsedPercent, limit.RemainingPercent)
			meters = append(meters, label(name+" ")+
				paint(heatColor(used), fmt.Sprintf("%d%%", int(math.Round(used)))))
			break
		}
	}

	lines := []string{strings.Join(head, "  "), strings.Join(meters, sep)}
	if options.Width > 0 {
		for i := range lines {
			lines[i] = truncateDisplay(lines[i], options.Width)
		}
	}
	return strings.Join(lines, "\n")
}

func validTheme(theme string) bool {
	for _, candidate := range ThemeIDs() {
		if theme == candidate {
			return true
		}
	}
	return false
}

func percentText(metric Metric) string {
	if metric.Status != StatusLive {
		return "?"
	}
	return fmt.Sprintf("%.0f%%", math.Round(metric.RemainingPercent))
}

func remainingBar(metric Metric) string {
	if metric.Status != StatusLive {
		return "[??????????]"
	}
	filled := int(math.Round(clampPercent(metric.RemainingPercent) / 10))
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", 10-filled) + "]"
}

func limitText(limits []LimitWindow, kind LimitKind) string {
	for _, limit := range limits {
		if limit.Kind == kind {
			if limit.Status != StatusLive {
				return "?"
			}
			return fmt.Sprintf("%.0f%%", math.Round(limit.RemainingPercent))
		}
	}
	return "?"
}

func colorForMode(mode Mode) string {
	switch mode {
	case ModeFlow:
		return "\x1b[38;5;81m"
	case ModeWatch:
		return "\x1b[38;5;220m"
	case ModeConserve:
		return "\x1b[38;5;208m"
	case ModeCritical:
		return "\x1b[38;5;196m"
	default:
		return "\x1b[38;5;244m"
	}
}

// truncateDisplay clips to a visible-column budget. It is ANSI-aware: escape
// sequences are copied through without consuming width, and a reset is appended
// when the input carried any styling, so truncating a coloured line can never
// leak colour into the rest of the terminal.
func truncateDisplay(value string, width int) string {
	if width <= 0 || displayWidth(value) <= width {
		return value
	}
	if width == 1 {
		return "~"
	}
	runes := []rune(value)
	var b strings.Builder
	visible, styled := 0, false
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			styled = true
			b.WriteRune(runes[i])
			i++
			b.WriteRune(runes[i])
			for i+1 < len(runes) && runes[i] != 'm' {
				i++
				b.WriteRune(runes[i])
			}
			continue
		}
		if visible == width-1 {
			break
		}
		b.WriteRune(runes[i])
		visible++
	}
	b.WriteString("~")
	if styled {
		b.WriteString(ansiReset)
	}
	return b.String()
}

func displayWidth(value string) int {
	plain := make([]rune, 0, len(value))
	runes := []rune(value)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			i += 2
			for i < len(runes) && runes[i] != 'm' {
				i++
			}
			continue
		}
		plain = append(plain, runes[i])
	}
	return len(plain)
}

type codexPalette struct {
	Name, Background, Foreground, Accent, Muted, Warning string
}

func RenderCodexTheme(theme string) (string, error) {
	palettes := map[string]codexPalette{
		ThemePAINexus: {"PAI Nexus", "#08131A", "#D8F3F0", "#46D9C5", "#6F8F96", "#FFB454"},
		ThemePAIFocus: {"PAI Focus", "#111512", "#E5EBD8", "#A8D26D", "#77806E", "#E6C36A"},
		ThemePAIOps:   {"PAI Ops", "#0E1116", "#DDE5EF", "#63A9FF", "#778292", "#FF9D5C"},
		ThemePAIMono:  {"PAI Mono", "#101010", "#E6E6E6", "#FFFFFF", "#858585", "#C8C8C8"},
	}
	palette, ok := palettes[theme]
	if !ok {
		return "", fmt.Errorf("unknown Codex theme %q", theme)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>name</key><string>%s</string>
  <key>settings</key>
  <array>
    <dict><key>settings</key><dict><key>background</key><string>%s</string><key>foreground</key><string>%s</string><key>caret</key><string>%s</string><key>selection</key><string>%s</string></dict></dict>
    <dict><key>scope</key><string>comment, punctuation.definition.comment</string><key>settings</key><dict><key>foreground</key><string>%s</string></dict></dict>
    <dict><key>scope</key><string>keyword, storage, entity.name.function, markup.heading</string><key>settings</key><dict><key>foreground</key><string>%s</string></dict></dict>
    <dict><key>scope</key><string>invalid, message.error, markup.deleted</string><key>settings</key><dict><key>foreground</key><string>%s</string></dict></dict>
  </array>
</dict>
</plist>
`, palette.Name, palette.Background, palette.Foreground, palette.Foreground, palette.Accent+"55", palette.Muted, palette.Accent, palette.Warning), nil
}
