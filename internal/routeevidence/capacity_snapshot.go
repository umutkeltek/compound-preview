package routeevidence

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/work"
)

type LaneCapacityState string

const (
	CapacityAlive    LaneCapacityState = "alive"
	CapacityWalled   LaneCapacityState = "walled"
	CapacityExpiring LaneCapacityState = "expiring"
	CapacityUnknown  LaneCapacityState = "unknown"
	CapacityDead     LaneCapacityState = "dead"

	defaultExpectedMinutes   = 60
	capacityWallFreshness    = 6 * time.Hour
	capacityRecentCluster    = 30 * time.Minute
	capacityClockSkew        = 5 * time.Minute
	codexCapacityFreshness   = 15 * time.Minute
	claudeCapacityFreshness  = 15 * time.Minute
	zaiCapacityFreshness     = 30 * time.Minute
	capacityEvidenceProtocol = "capacity-snapshot"
	capacityEvidenceProfile  = "compound.capacity-veto"
	capacityRefusalStatus    = "capacity_refused"

	// laneAliasesRelPath is the home-relative path to the operator-owned lane-alias override file.
	laneAliasesRelPath = "projections/lane-aliases.json"
	// laneAliasesSchema versions the lane-alias override projection body.
	laneAliasesSchema = "compound.lane_aliases.v0.1"

	// capacitySupplementRelPath is the home-relative path to Compound's OWN supplement usage
	// feed — claude/zai/etc capacity rows Compound writes and controls, merged alongside
	// CodexBar's own usage-history.jsonl so an external app relaunch that rewrites/prunes its
	// own file can never again silently blind a whole lane family (2026-07-23 blackout).
	capacitySupplementRelPath = "projections/usage-supplement.jsonl"

	// capacityBlackoutMultiplier is how many freshness windows of total silence (across BOTH
	// merged sources) turn a stale-but-once-enrolled capacity family into a doctor-visible
	// capacity.blackout:<family> warning. 4x the family's own freshness window (15m/15m/30m)
	// gives relaunch/restart jitter room before firing, while still catching a silently pruned
	// or stopped feed well before an operator would otherwise notice via a hard dispatch
	// refusal.
	capacityBlackoutMultiplier = 4
)

type LaneCapacity struct {
	Remaining  float64           `json:"remaining"`
	ResetsAt   time.Time         `json:"resets_at,omitempty"`
	LastProven time.Time         `json:"last_proven,omitempty"`
	State      LaneCapacityState `json:"state"`
	Source     string            `json:"source,omitempty"`
	DataRef    string            `json:"data_ref,omitempty"`
	// SourceInstalled reports whether ANY configured capacity source file exists. Unknown while
	// unwired fails OPEN (warning only) — a fresh adopter home without CodexBar/quota/projection
	// must not demand --confirm forever; unknown while wired stays confirmation-gated.
	SourceInstalled bool `json:"source_installed"`
}

type LaneCapacityPaths struct {
	UsageHistoryPath      string
	QuotaCachePath        string
	AccountProjectionPath string
	// AliasesPath is the operator-owned lane-alias override projection
	// (`<COMPOUND_HOME>/projections/lane-aliases.json`). It is NOT a capacity source — it only
	// renames a lane to a family before the structural rules run — so it is deliberately excluded
	// from capacitySourceInstalled.
	AliasesPath string
	// SupplementUsagePath is Compound's OWN supplement usage feed
	// (`<COMPOUND_HOME>/projections/usage-supplement.jsonl`) — same row shape as
	// UsageHistoryPath, merged at row level before any freshness/cluster logic runs. It IS a
	// capacity source (unlike AliasesPath) so it participates in capacitySourceInstalled.
	SupplementUsagePath string
}

type LaneCapacityConfig struct {
	Paths                   LaneCapacityPaths
	ExpectedMinutes         int
	SuppressRefusalEvidence bool
}

// DefaultLaneCapacityConfig points at the operator-owned read projections without embedding any
// personal absolute path. Tests use ReadLaneCapacityWithConfig with temp fixtures instead.
func DefaultLaneCapacityConfig(homePath string) LaneCapacityConfig {
	var paths LaneCapacityPaths
	if userHome, err := os.UserHomeDir(); err == nil {
		paths.UsageHistoryPath = filepath.Join(userHome, "Library", "Application Support", "CodexBar", "usage-history.jsonl")
		paths.QuotaCachePath = filepath.Join(userHome, ".codex", "multi-auth", "quota-cache.json")
	}
	if strings.TrimSpace(homePath) != "" {
		paths.AccountProjectionPath = AccountProjectionPath(homePath)
		paths.AliasesPath = LaneAliasesPath(homePath)
		paths.SupplementUsagePath = CapacitySupplementPath(homePath)
	}
	return LaneCapacityConfig{Paths: paths, ExpectedMinutes: defaultExpectedMinutes}
}

// LaneAliasesPath is the absolute path the operator-owned lane-alias override projection is read
// from (mirrors AccountProjectionPath's home-relative convention).
func LaneAliasesPath(homePath string) string {
	return filepath.Join(homePath, filepath.FromSlash(laneAliasesRelPath))
}

// CapacitySupplementPath is the absolute path Compound's own supplement usage feed is read from
// (mirrors LaneAliasesPath's home-relative convention).
func CapacitySupplementPath(homePath string) string {
	return filepath.Join(homePath, filepath.FromSlash(capacitySupplementRelPath))
}

// ReadLaneCapacity is the production front door. COMPOUND_HOME is used only to locate Compound's
// own reduced account projection; callers that need deterministic paths use the config variant.
func ReadLaneCapacity(lane string, now time.Time) (LaneCapacity, error) {
	return ReadLaneCapacityWithConfig(lane, now, DefaultLaneCapacityConfig(os.Getenv("COMPOUND_HOME")))
}

// ReadLaneCapacityWithConfig reads the capacity sources for one evaluation. It is the
// production front door: it re-reads every source on every call, so a decision always sees the
// current bytes.
func ReadLaneCapacityWithConfig(lane string, now time.Time, cfg LaneCapacityConfig) (LaneCapacity, error) {
	cfg = normalizeLaneCapacityConfig(cfg, "")
	return readLaneCapacityWithFacts(lane, now, cfg.ExpectedMinutes, freshLaneCapacityFacts(cfg))
}

// laneCapacityFacts is the seam the lane-capacity composition runs over: where the source facts
// come from. The fresh path reads them from disk on every evaluation; a request-scoped
// observation serves the bytes it already read for that request. Both routes run the SAME
// composition and the SAME resolver, so a decision can never differ between them.
type laneCapacityFacts struct {
	// aliases is the operator-owned lane-alias override map. Nil when the file is absent.
	aliases map[string]string
	// aliasErr is the alias-file read/parse failure. Non-nil means the file exists but could
	// not be read, which composes a note onto a final-unknown state.
	aliasErr error
	// installed reports whether ANY configured capacity source file exists.
	installed bool
	// usage answers the usage-feed question for one provider family.
	usage func(provider string, now time.Time, expectedMinutes int) (LaneCapacity, bool, error)
	// quotaWall answers the multi-auth quota-cache question for one provider family.
	quotaWall func(provider string, now time.Time) (LaneCapacity, bool, error)
	// deadAccount answers the account-projection question for one provider family.
	deadAccount func(provider string, now time.Time) (LaneCapacity, bool, error)
}

// freshLaneCapacityFacts reads every capacity source for one evaluation. This is the production
// path: it re-reads on every call, so a decision always sees the current bytes.
func freshLaneCapacityFacts(cfg LaneCapacityConfig) laneCapacityFacts {
	aliases, aliasErr := loadLaneCapacityAliases(cfg.Paths.AliasesPath)
	installed := capacitySourceInstalled(cfg.Paths)
	facts := laneCapacityFacts{aliases: aliases, aliasErr: aliasErr, installed: installed}
	facts.usage = func(provider string, now time.Time, expectedMinutes int) (LaneCapacity, bool, error) {
		return readCodexBarCapacity(cfg.Paths, provider, now, expectedMinutes, capacityFreshness(provider))
	}
	facts.quotaWall = func(provider string, now time.Time) (LaneCapacity, bool, error) {
		return readFreshQuotaWall(cfg.Paths.QuotaCachePath, provider, now)
	}
	facts.deadAccount = func(provider string, now time.Time) (LaneCapacity, bool, error) {
		return readDeadAccountProjection(cfg.Paths.AccountProjectionPath, provider, now)
	}
	return facts
}

// readLaneCapacityWithFacts is the ONE lane-capacity composition. It resolves the lane to a
// capacity family, then layers the usage feed, the quota-cache wall and the account-projection
// death in the same order and with the same precedence for every caller.
func readLaneCapacityWithFacts(lane string, now time.Time, expectedMinutes int, facts laneCapacityFacts) (LaneCapacity, error) {
	key, aliasNote := resolveLaneCapacityKeyFrom(lane, facts.aliases, facts.aliasErr)
	capacity := LaneCapacity{State: CapacityUnknown, DataRef: "no matching capacity row", SourceInstalled: facts.installed}
	if !facts.installed {
		capacity.DataRef = "no capacity source installed"
	}
	if key == "" {
		if aliasNote != "" {
			capacity.DataRef = capacity.DataRef + "; " + aliasNote
		}
		return capacity, nil
	}

	usage, ok, err := facts.usage(key, now, expectedMinutes)
	if err != nil {
		return LaneCapacity{}, err
	}
	if ok {
		capacity = usage
	} else if usage.DataRef != "" {
		// Annotated unknown: the usage question returns a non-empty LaneCapacity even when no
		// fresh row was found, so callers can tell WHY the lane is unknown — zero rows vs
		// only-stale rows. Carry the annotation forward; the wall/dead checks below may still
		// override if their own fresh evidence arrives.
		capacity.DataRef = usage.DataRef
		if !usage.LastProven.IsZero() {
			capacity.LastProven = usage.LastProven
		}
		if usage.Source != "" {
			capacity.Source = usage.Source
		}
	}

	wall, ok, err := facts.quotaWall(key, now)
	if err != nil {
		return LaneCapacity{}, err
	}
	if ok {
		wall.Remaining = capacity.Remaining
		wall.ResetsAt = capacity.ResetsAt
		capacity = wall
	}

	dead, ok, err := facts.deadAccount(key, now)
	if err != nil {
		return LaneCapacity{}, err
	}
	if ok {
		dead.Remaining = capacity.Remaining
		dead.ResetsAt = capacity.ResetsAt
		capacity = dead
	}
	capacity.SourceInstalled = facts.installed
	// Compose malformed-alias note onto any final-unknown state. Earlier branches already
	// appended the note when key==""; this covers the resolved-but-stale branch where the
	// lane resolves to a family but only stale rows are available.
	if capacity.State == CapacityUnknown && aliasNote != "" && !strings.Contains(capacity.DataRef, aliasNote) {
		capacity.DataRef = capacity.DataRef + "; " + aliasNote
	}
	return capacity, nil
}

func capacitySourceInstalled(paths LaneCapacityPaths) bool {
	return capacitySourceInstalledCounted(paths, nil)
}

// capacitySourceInstalledCounted is capacitySourceInstalled with an optional per-path observer,
// so a request-scoped observation can report how many existence probes it made. The probe
// sequence and the short-circuit are identical to the uncounted form: the result is "any
// configured capacity source file exists", and the count reflects only the paths actually
// probed (the first hit ends the walk, exactly as the production path does).
func capacitySourceInstalledCounted(paths LaneCapacityPaths, onStat func(string)) bool {
	for _, path := range []string{paths.UsageHistoryPath, paths.QuotaCachePath, paths.AccountProjectionPath, paths.SupplementUsagePath} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if onStat != nil {
			onStat(path)
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

type codexBarCapacityRow struct {
	Provider      string  `json:"provider"`
	WindowKind    string  `json:"windowKind"`
	WindowMinutes int     `json:"windowMinutes"`
	UsedPercent   float64 `json:"usedPercent"`
	ResetsAt      string  `json:"resetsAt"`
	SampledAt     string  `json:"sampledAt"`
	sampled       time.Time
	// source names the file this row was read from (e.g. "codexbar/usage-history" or
	// "compound/usage-supplement") — set post-parse, never part of the on-disk JSON shape.
	source string
}

// capacityRowSource pairs a usage-row JSONL file path with the human-readable source label
// recorded on a LaneCapacity result (Source/DataRef) whenever a row from that file constrains
// the decision.
type capacityRowSource struct {
	path  string
	label string
}

// capacityRowSourcesFor builds the merge set for a lookup: CodexBar's own usage-history file and
// Compound's own supplement feed. Iteration order does not decide the winner — the newest
// sampledAt row across BOTH always constrains, regardless of which source produced it — so an
// empty/absent SupplementUsagePath (every pre-supplement caller and fixture) degrades to exactly
// the single-source behavior that existed before this feed was added.
func capacityRowSourcesFor(paths LaneCapacityPaths) []capacityRowSource {
	return []capacityRowSource{
		{path: paths.UsageHistoryPath, label: "codexbar/usage-history"},
		{path: paths.SupplementUsagePath, label: "compound/usage-supplement"},
	}
}

// scanCapacityRows scans one usage-row JSONL file and retains only the rows the accept
// predicate approves. It is the shared parser for both capacity paths: the fresh reader accepts
// the one provider family it was asked about, and a request-scoped observation accepts every
// family it could evaluate. A blank path or a missing file is not an error — it contributes zero
// rows and reports installed=false so the caller can distinguish "nothing wired" from "wired but
// empty". An unreadable/corrupt file IS an error: silently treating "broken" as "empty" would
// hide a real misconfiguration from the freshness veto above it.
//
// The accept predicate runs on the PARSED row, after the malformed/NaN/sampledAt rejections, so
// no path can retain a row the decision could not use. Nothing that accept rejects is ever
// materialized: a broad multi-provider feed costs a lookup only its own rows.
func scanCapacityRows(src capacityRowSource, accept func(codexBarCapacityRow) bool) (rows []codexBarCapacityRow, installed bool, err error) {
	if strings.TrimSpace(src.path) == "" {
		return nil, false, nil
	}
	file, err := os.Open(src.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read capacity usage history (%s): %w", src.label, err)
	}
	defer file.Close()
	installed = true

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var row codexBarCapacityRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			continue
		}
		if math.IsNaN(row.UsedPercent) || math.IsInf(row.UsedPercent, 0) {
			continue
		}
		sampled, err := time.Parse(time.RFC3339, strings.TrimSpace(row.SampledAt))
		if err != nil {
			continue
		}
		row.sampled = sampled
		row.source = src.label
		if accept != nil && !accept(row) {
			continue
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, installed, fmt.Errorf("scan capacity usage history (%s): %w", src.label, err)
	}
	return rows, installed, nil
}

// readCapacityRowsFromSource scans a single usage-row JSONL file for ONE provider family at one
// instant. The family filter and the clock-skew allowance run inside the scan, so a broad
// multi-provider feed never materializes rows this lookup will not use.
func readCapacityRowsFromSource(src capacityRowSource, now time.Time, matches func(string) bool) (rows []codexBarCapacityRow, installed bool, err error) {
	return scanCapacityRows(src, func(row codexBarCapacityRow) bool {
		return matches(row.Provider) && !row.sampled.After(now.Add(capacityClockSkew))
	})
}

// readCapacityRowsFromSources merges matching rows from every source file that exists.
// installed reports whether ANY of the source files exist on disk at all — used to tell "zero
// rows because nothing is wired" apart from "zero rows because nothing matched this family".
func readCapacityRowsFromSources(sources []capacityRowSource, now time.Time, matches func(string) bool) (rows []codexBarCapacityRow, installed bool, err error) {
	for _, src := range sources {
		fileRows, fileInstalled, ferr := readCapacityRowsFromSource(src, now, matches)
		if ferr != nil {
			return nil, false, ferr
		}
		installed = installed || fileInstalled
		rows = append(rows, fileRows...)
	}
	return rows, installed, nil
}

func readCodexBarCapacity(paths LaneCapacityPaths, lane string, now time.Time, expectedMinutes int, freshness time.Duration) (LaneCapacity, bool, error) {
	return readCodexBarCapacityMatching(capacityRowSourcesFor(paths), lane, now, expectedMinutes, freshness, func(provider string) bool {
		return normalizeLaneCapacityKey(provider) == lane
	})
}

func readCodexBarCapacityExact(paths LaneCapacityPaths, provider string, now time.Time, expectedMinutes int, freshness time.Duration) (LaneCapacity, bool, error) {
	wanted := normalizeCodexBarProvider(provider)
	return readCodexBarCapacityMatching(capacityRowSourcesFor(paths), provider, now, expectedMinutes, freshness, func(candidate string) bool {
		return normalizeCodexBarProvider(candidate) == wanted
	})
}

// readCodexBarCapacityMatching merges rows from every configured source BEFORE any
// freshness/cluster logic runs — the newest-sampledAt row across all sources decides freshness,
// and the "recent cluster" window/veto/state logic below is unchanged from the single-source
// version, just fed a merged row set. Source/DataRef of the result always name the ACTUAL source
// of the constraining (or, on the stale path, the merged-newest) row.
func readCodexBarCapacityMatching(sources []capacityRowSource, provider string, now time.Time, expectedMinutes int, freshness time.Duration, matches func(string) bool) (LaneCapacity, bool, error) {
	rows, installed, err := readCapacityRowsFromSources(sources, now, matches)
	if err != nil {
		return LaneCapacity{}, false, err
	}
	return decideCodexBarCapacity(rows, installed, provider, now, expectedMinutes, freshness)
}

// decideCodexBarCapacity is the freshness/cluster/window decision over an ALREADY-READ row set.
// The row set is consumed in place (it is sorted here), so a caller that shares parsed rows
// between actors must hand over its own copy.
func decideCodexBarCapacity(rows []codexBarCapacityRow, installed bool, provider string, now time.Time, expectedMinutes int, freshness time.Duration) (LaneCapacity, bool, error) {
	if !installed {
		return LaneCapacity{}, false, nil
	}
	if len(rows) == 0 {
		return LaneCapacity{
			State:           CapacityUnknown,
			SourceInstalled: true,
			DataRef:         fmt.Sprintf("no usage rows for family: %s", provider),
		}, false, nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].sampled.After(rows[j].sampled) })
	newest := rows[0].sampled
	newestSource := rows[0].source
	if freshness <= 0 {
		freshness = capacityFreshness(provider)
	}
	if age := now.Sub(newest); age < 0 || age > freshness {
		return LaneCapacity{
			State:           CapacityUnknown,
			SourceInstalled: true,
			LastProven:      newest.UTC(),
			Source:          newestSource,
			DataRef: fmt.Sprintf("%s:no_fresh_rows,family=%s,newest=%s,age=%dm,freshness=%dm",
				newestSource, provider, newest.UTC().Format(time.RFC3339), int(age.Minutes()), int(freshness.Minutes())),
		}, false, nil
	}

	recentCutoff := newest.Add(-capacityRecentCluster)
	latestByWindow := map[string]codexBarCapacityRow{}
	for _, row := range rows {
		if row.sampled.Before(recentCutoff) {
			continue
		}
		group := fmt.Sprintf("%s|%d|%s", row.WindowKind, row.WindowMinutes, row.ResetsAt)
		if previous, ok := latestByWindow[group]; !ok || row.sampled.After(previous.sampled) {
			latestByWindow[group] = row
		}
	}
	// A window group whose ResetsAt is not after now is EXPIRED: its UsedPercent describes a
	// quota window that no longer exists, so it must never constrain a launch decision. Right
	// after a provider's window resets, the recent cluster holds BOTH the old window's last
	// row (higher usage, older sample) and the new window's fresh rows — without this
	// exclusion the dead window wins "highest UsedPercent", pins LastProven to its aging
	// sample, and the lane reads capacity_unknown for up to the whole cluster span while
	// fresh rows keep arriving. When EVERY group is expired, fall back to today's behaviour
	// (report the highest-used row, state unknown below) rather than inventing capacity.
	constraining, found := constrainingWindowRow(latestByWindow, now)
	allExpired := !found
	if allExpired {
		constraining, found = highestUsedWindowRow(latestByWindow)
	}
	if !found {
		return LaneCapacity{}, false, nil
	}

	remaining := 100 - constraining.UsedPercent
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 100 {
		remaining = 100
	}
	// dataRefPrefix keeps the pre-supplement DataRef format byte-identical when the constraining
	// row still comes from CodexBar's own file ("codexbar:provider=..."); a supplement-sourced
	// row instead names itself explicitly so an operator can tell which feed is load-bearing.
	dataRefPrefix := constraining.source
	if dataRefPrefix == "codexbar/usage-history" {
		dataRefPrefix = "codexbar"
	}
	out := LaneCapacity{
		Remaining:  remaining,
		LastProven: constraining.sampled.UTC(),
		State:      CapacityUnknown,
		Source:     constraining.source,
		DataRef: fmt.Sprintf(
			"%s:provider=%s,window=%s/%dm,sampled_at=%s",
			dataRefPrefix,
			provider,
			strings.TrimSpace(constraining.WindowKind),
			constraining.WindowMinutes,
			constraining.sampled.UTC().Format(time.RFC3339),
		),
	}
	reset, err := time.Parse(time.RFC3339, strings.TrimSpace(constraining.ResetsAt))
	if err != nil || !reset.After(now) {
		if allExpired {
			out.DataRef += "; all_windows_expired"
		}
		return out, true, nil
	}
	out.ResetsAt = reset.UTC()
	switch {
	case constraining.UsedPercent >= 100:
		out.State = CapacityWalled
	case !reset.After(now.Add(time.Duration(expectedMinutes) * time.Minute)):
		out.State = CapacityExpiring
	default:
		out.State = CapacityAlive
	}
	return out, true, nil
}

// windowGroupExpired reports whether a window group's reset deadline has already passed. An
// unparseable ResetsAt is NOT expired — the row stays eligible, and the parse check in
// readCodexBarCapacityMatching keeps reporting such a row as unknown exactly as before this
// helper existed.
func windowGroupExpired(row codexBarCapacityRow, now time.Time) bool {
	reset, err := time.Parse(time.RFC3339, strings.TrimSpace(row.ResetsAt))
	return err == nil && !reset.After(now)
}

// constrainingWindowRow picks the highest-UsedPercent NON-expired group representative — the
// live window with the least headroom is what a launch decision actually answers to, so a
// dead window's usage can never win the slot or supply LastProven.
func constrainingWindowRow(groups map[string]codexBarCapacityRow, now time.Time) (codexBarCapacityRow, bool) {
	var constraining codexBarCapacityRow
	found := false
	for _, row := range groups {
		if windowGroupExpired(row, now) {
			continue
		}
		if !found || row.UsedPercent > constraining.UsedPercent {
			constraining = row
			found = true
		}
	}
	return constraining, found
}

// highestUsedWindowRow is the pre-expiry-exclusion selection: highest UsedPercent across every
// group, expired or not. It backs only the all-groups-expired fallback, where no live window
// exists to constrain and the caller reports unknown off the expired winner.
func highestUsedWindowRow(groups map[string]codexBarCapacityRow) (codexBarCapacityRow, bool) {
	var constraining codexBarCapacityRow
	found := false
	for _, row := range groups {
		if !found || row.UsedPercent > constraining.UsedPercent {
			constraining = row
			found = true
		}
	}
	return constraining, found
}

// capacityWatchedFamilies enumerates every capacity family the blackout watchdog checks — the
// same families exactLaneCapacityFamily/normalizeLaneCapacityKey recognize.
var capacityWatchedFamilies = []string{"codex", "claude", "zai", "qwen", "minimax"}

// CapacityFeedWarning is a single non-fatal `compound home doctor` watchdog item. It never gates
// the doctor verdict — see cmd/compound's buildHomeDoctorReport, which folds this in as an
// additive, non-verdict-affecting field.
type CapacityFeedWarning struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// newestCapacitySampleAcrossSources returns the most recent sampledAt timestamp for `family`
// across every configured capacity row source (CodexBar's file + Compound's own supplement) with
// NO freshness veto — the blackout watchdog needs to see a row of ANY age to tell a
// stale-but-once-enrolled family (blackout) apart from a family that was never enrolled at all
// (silent by design, not a warning).
func newestCapacitySampleAcrossSources(paths LaneCapacityPaths, family string, now time.Time) (time.Time, bool, error) {
	rows, _, err := readCapacityRowsFromSources(capacityRowSourcesFor(paths), now, func(provider string) bool {
		return normalizeLaneCapacityKey(provider) == family
	})
	if err != nil {
		return time.Time{}, false, err
	}
	if len(rows) == 0 {
		return time.Time{}, false, nil
	}
	newest := rows[0].sampled
	for _, row := range rows[1:] {
		if row.sampled.After(newest) {
			newest = row.sampled
		}
	}
	return newest, true, nil
}

// CapacityFeedWarnings is the non-fatal capacity-feed-blackout watchdog folded into
// `compound home doctor`: a family with at least one row EVER (from either source) but nothing
// within capacityBlackoutMultiplier x its own freshness window produces a
// `capacity.blackout:<family>` item. A family with zero rows ever is not enrolled — not blacked
// out — and produces no item; a family with a fresh-enough row produces no item either. A source
// read error is swallowed here (mirrors capacitySourceInstalled's fail-open posture) — this is a
// doctor-only annotation, never a veto, so it must never itself panic or block doctor output.
func CapacityFeedWarnings(cfg LaneCapacityConfig, now time.Time) []CapacityFeedWarning {
	cfg = normalizeLaneCapacityConfig(cfg, "")
	var warnings []CapacityFeedWarning
	for _, family := range capacityWatchedFamilies {
		newest, found, err := newestCapacitySampleAcrossSources(cfg.Paths, family, now)
		if err != nil || !found {
			continue
		}
		threshold := capacityBlackoutMultiplier * capacityFreshness(family)
		age := now.Sub(newest)
		if age <= threshold {
			continue
		}
		warnings = append(warnings, CapacityFeedWarning{
			Name: "capacity.blackout:" + family,
			OK:   false,
			Detail: fmt.Sprintf("newest=%s age=%dm threshold=%dm",
				newest.UTC().Format(time.RFC3339), int(age.Minutes()), int(threshold.Minutes())),
		})
	}
	return warnings
}

// quotaCacheAccount is one multi-auth account entry in the Codex quota cache.
type quotaCacheAccount struct {
	Status int `json:"status"`
}

// quotaCacheDocument is the parsed shape of the Codex multi-auth quota cache.
type quotaCacheDocument struct {
	ByAccountID map[string]quotaCacheAccount `json:"byAccountId"`
}

// quotaWallApplies reports whether the multi-auth quota cache can answer for a provider family
// at all. The cache is a Codex-family artifact; no other family reads it.
func quotaWallApplies(provider, path string) bool {
	return provider == "codex" && strings.TrimSpace(path) != ""
}

// quotaCacheWithinFreshness reports whether the quota cache's observed mtime can constrain a
// decision. A cache observed beyond the clock-skew allowance in the future, or older than the
// wall-freshness window, is ignored.
func quotaCacheWithinFreshness(observed, now time.Time) bool {
	age := now.Sub(observed)
	return age >= -capacityClockSkew && age <= capacityWallFreshness
}

func readFreshQuotaWall(path, lane string, now time.Time) (LaneCapacity, bool, error) {
	if !quotaWallApplies(lane, path) {
		return LaneCapacity{}, false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LaneCapacity{}, false, nil
		}
		return LaneCapacity{}, false, fmt.Errorf("stat quota cache: %w", err)
	}
	observed := info.ModTime()
	if !quotaCacheWithinFreshness(observed, now) {
		return LaneCapacity{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return LaneCapacity{}, false, fmt.Errorf("read quota cache: %w", err)
	}
	var cache quotaCacheDocument
	if err := json.Unmarshal(data, &cache); err != nil {
		return LaneCapacity{}, false, fmt.Errorf("parse quota cache: %w", err)
	}
	return decideFreshQuotaWall(observed, cache)
}

// decideFreshQuotaWall turns a parsed quota cache into the wall verdict. A 429 on ANY account
// walls the codex family; the observed mtime is the provenance of that verdict.
func decideFreshQuotaWall(observed time.Time, cache quotaCacheDocument) (LaneCapacity, bool, error) {
	for _, account := range cache.ByAccountID {
		if account.Status == 429 {
			return LaneCapacity{
				State:      CapacityWalled,
				LastProven: observed.UTC(),
				Source:     "codex-multi-auth/quota-cache",
				DataRef:    "quota-cache:status=429,modified_at=" + observed.UTC().Format(time.RFC3339),
			}, true, nil
		}
	}
	return LaneCapacity{}, false, nil
}

// accountProjectionApplies reports whether an account projection is configured as a source.
func accountProjectionApplies(path string) bool {
	return strings.TrimSpace(path) != ""
}

func readDeadAccountProjection(path, lane string, now time.Time) (LaneCapacity, bool, error) {
	if !accountProjectionApplies(path) {
		return LaneCapacity{}, false, nil
	}
	projection, err := readAccountProjectionFile(path)
	if err != nil {
		return LaneCapacity{}, false, err
	}
	if projection == nil {
		return LaneCapacity{}, false, nil
	}
	return decideDeadAccountProjection(projection, lane, now)
}

// readAccountProjectionFile reads and parses the switchback account projection. A missing file is
// absent (nil, nil); an unreadable or malformed file is a hard error — the difference between
// "no projection" and "broken projection" must never collapse.
func readAccountProjectionFile(path string) (*AccountProjection, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read account projection: %w", err)
	}
	var projection AccountProjection
	if err := json.Unmarshal(data, &projection); err != nil {
		return nil, fmt.Errorf("parse account projection: %w", err)
	}
	return &projection, nil
}

// decideDeadAccountProjection reports a lane DEAD when the account projection says every account
// for that provider is unhealthy and the projection itself is fresh.
func decideDeadAccountProjection(projection *AccountProjection, lane string, now time.Time) (LaneCapacity, bool, error) {
	importedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(projection.ImportedAt))
	if err != nil {
		return LaneCapacity{}, false, nil
	}
	age := now.Sub(importedAt)
	if age < -capacityClockSkew || age > accountProjectionFreshnessWindow {
		return LaneCapacity{}, false, nil
	}
	entry, ok := projection.Providers[lane]
	if !ok || entry.TotalAccountCount <= 0 || entry.HealthyAccountCount > 0 {
		return LaneCapacity{}, false, nil
	}
	observed := importedAt
	if sampled, err := time.Parse(time.RFC3339, strings.TrimSpace(entry.FreshestSampledAt)); err == nil && !sampled.After(now.Add(capacityClockSkew)) {
		observed = sampled
	}
	return LaneCapacity{
		State:      CapacityDead,
		LastProven: observed.UTC(),
		Source:     "switchback/account-projection",
		DataRef: fmt.Sprintf(
			"account-projection:provider=%s,healthy=%d,total=%d,observed_at=%s",
			lane,
			entry.HealthyAccountCount,
			entry.TotalAccountCount,
			observed.UTC().Format(time.RFC3339),
		),
	}, true, nil
}

type LaneCapacityDecision struct {
	Capacity             LaneCapacity
	Refused              bool
	ConfirmationRequired bool
	Message              string
	AlternativeLadder    []string
}

func GuardLaneCapacity(homePath, lane, capabilityClass string, now time.Time, confirm bool, alternatives []string, cfg LaneCapacityConfig) (LaneCapacityDecision, error) {
	cfg = normalizeLaneCapacityConfig(cfg, homePath)
	capacity, err := ReadLaneCapacityWithConfig(lane, now, cfg)
	if err != nil {
		return LaneCapacityDecision{}, err
	}
	alternatives = capacityAlternativeLadder(capabilityClass, alternatives, freshRoutingDefaults(homePath))
	return decideLaneCapacity(homePath, lane, now, confirm, alternatives, cfg, capacity)
}

// GuardDeclaredLaneCapacity reads exactly the source declared on a LaneTuple.
// It never falls through to another provider family or another capacity feed:
// an absent/stale declared source stays unknown, and an unknown source kind is
// rejected. This keeps Compound from manufacturing quota truth by aliasing one
// provider's evidence onto another lane.
func GuardDeclaredLaneCapacity(homePath, lane, capabilityClass string, source LaneCapacitySourceRef, now time.Time, expectedMinutes int, confirm bool, alternatives []string, suppressRefusalEvidence bool) (LaneCapacityDecision, error) {
	return GuardDeclaredLaneCapacityWithConfig(homePath, lane, capabilityClass, source, now, expectedMinutes, confirm, alternatives, suppressRefusalEvidence, LaneCapacityConfig{})
}

// GuardDeclaredLaneCapacityWithConfig is the injectable variant used by route
// planning and the work guard. Production callers pass an empty config; tests
// and embedded consumers can point the same policy at deterministic sources.
func GuardDeclaredLaneCapacityWithConfig(homePath, lane, capabilityClass string, source LaneCapacitySourceRef, now time.Time, expectedMinutes int, confirm bool, alternatives []string, suppressRefusalEvidence bool, cfg LaneCapacityConfig) (LaneCapacityDecision, error) {
	return guardDeclaredLaneCapacityWithConfigAndProbe(homePath, lane, capabilityClass, source, now, expectedMinutes, confirm, alternatives, suppressRefusalEvidence, cfg, ProbeCodexBarCapacity)
}

type codexBarCapacityProbeFunc func(string, time.Time, time.Duration) (CodexBarCapacityEvidence, error)

func guardDeclaredLaneCapacityWithProbe(homePath, lane, capabilityClass string, source LaneCapacitySourceRef, now time.Time, expectedMinutes int, confirm bool, alternatives []string, suppressRefusalEvidence bool, probe codexBarCapacityProbeFunc) (LaneCapacityDecision, error) {
	return guardDeclaredLaneCapacityWithConfigAndProbe(homePath, lane, capabilityClass, source, now, expectedMinutes, confirm, alternatives, suppressRefusalEvidence, LaneCapacityConfig{}, probe)
}

func guardDeclaredLaneCapacityWithConfigAndProbe(homePath, lane, capabilityClass string, source LaneCapacitySourceRef, now time.Time, expectedMinutes int, confirm bool, alternatives []string, suppressRefusalEvidence bool, cfg LaneCapacityConfig, probe codexBarCapacityProbeFunc) (LaneCapacityDecision, error) {
	kind := strings.ToLower(strings.TrimSpace(source.Kind))
	if kind != "codexbar" {
		return LaneCapacityDecision{}, fmt.Errorf("unknown capacity source kind %q", source.Kind)
	}
	if source.FreshnessMinutes <= 0 {
		return LaneCapacityDecision{}, errors.New("declared capacity source freshness_minutes must be positive")
	}
	provider, err := codexBarProviderFromRef(source.Ref)
	if err != nil {
		return LaneCapacityDecision{}, err
	}
	cfg = normalizeLaneCapacityConfig(cfg, homePath)
	if expectedMinutes <= 0 {
		expectedMinutes = defaultExpectedMinutes
	}
	cfg.ExpectedMinutes = expectedMinutes
	cfg.SuppressRefusalEvidence = suppressRefusalEvidence
	installed := capacitySourceInstalled(LaneCapacityPaths{
		UsageHistoryPath:    cfg.Paths.UsageHistoryPath,
		SupplementUsagePath: cfg.Paths.SupplementUsagePath,
	})
	freshness := time.Duration(source.FreshnessMinutes) * time.Minute
	capacity, ok, err := readCodexBarCapacityExact(
		cfg.Paths,
		provider,
		now,
		cfg.ExpectedMinutes,
		freshness,
	)
	if err != nil {
		return LaneCapacityDecision{}, err
	}
	if !ok {
		history := capacity
		live, probeErr := probe(provider, now, freshness)
		if probeErr == nil {
			capacity = codexBarEvidenceCapacity(live, now, cfg.ExpectedMinutes)
			installed = true
		} else {
			capacity = LaneCapacity{
				State:           CapacityUnknown,
				SourceInstalled: true,
				LastProven:      history.LastProven,
				Source:          history.Source,
				DataRef:         strings.TrimSpace(history.DataRef) + "; live_probe=" + probeErr.Error(),
			}
		}
	}
	capacity.SourceInstalled = installed || capacity.SourceInstalled
	capacity.DataRef = "source_ref=" + strings.TrimSpace(source.Ref) + "; " + strings.TrimSpace(capacity.DataRef)
	alternatives = capacityAlternativeLadder(capabilityClass, alternatives, freshRoutingDefaults(homePath))
	return decideLaneCapacity(homePath, lane, now, confirm, alternatives, cfg, capacity)
}

func codexBarProviderFromRef(ref string) (string, error) {
	const prefix = "codexbar://provider/"
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("capacity source ref %q must match %s<provider>", ref, prefix)
	}
	provider := strings.TrimPrefix(ref, prefix)
	if provider == "" || strings.ContainsAny(provider, "/?#") || normalizeCodexBarProvider(provider) == "" {
		return "", fmt.Errorf("capacity source ref %q has invalid CodexBar provider", ref)
	}
	return normalizeCodexBarProvider(provider), nil
}

func decideLaneCapacity(homePath, lane string, now time.Time, confirm bool, alternatives []string, cfg LaneCapacityConfig, capacity LaneCapacity) (LaneCapacityDecision, error) {
	decision := LaneCapacityDecision{
		Capacity:          capacity,
		AlternativeLadder: alternatives,
	}
	switch capacity.State {
	case CapacityWalled, CapacityDead:
		decision.Refused = true
		decision.Message = capacityDecisionMessage(lane, capacity, alternatives, "refused")
		if !cfg.SuppressRefusalEvidence {
			if err := recordCapacityRefusal(homePath, lane, capacity, now); err != nil {
				return LaneCapacityDecision{}, err
			}
		}
	case CapacityUnknown, CapacityExpiring:
		if capacity.State == CapacityUnknown && !capacity.SourceInstalled {
			// Fail open when UNWIRED: no capacity source exists at all (fresh adopter home).
			// Annotate and warn; demanding --confirm forever would gate every dispatch on a
			// feed the home never had. Wired-but-blind lanes stay confirmation-gated below.
			decision.Message = capacityDecisionMessage(lane, capacity, alternatives, "unguarded (no capacity source installed)")
			break
		}
		if confirm {
			decision.Message = capacityDecisionMessage(lane, capacity, alternatives, "admitted by --confirm")
		} else {
			decision.ConfirmationRequired = true
			decision.Message = capacityDecisionMessage(lane, capacity, alternatives, "--confirm required")
		}
	}
	return decision, nil
}

func WorkLaneCapacityGuard(cfg LaneCapacityConfig) work.LaneCapacityGuardFunc {
	return func(req work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		if req.TupleBacked && req.CapacitySource == nil {
			return work.LaneCapacityGuardResult{
				Capacity: work.DispatchLaneCapacity{State: "gateway_owned"},
				Message:  "no Compound capacity source declared; runtime quota remains execution-gateway-owned",
			}, nil
		}
		effective := cfg
		effective.ExpectedMinutes = req.ExpectedMinutes
		effective.SuppressRefusalEvidence = req.ReadOnly
		var decision LaneCapacityDecision
		var err error
		if req.CapacitySource != nil {
			decision, err = GuardDeclaredLaneCapacityWithConfig(req.HomePath, req.Lane, req.CapabilityClass, LaneCapacitySourceRef{
				Kind:             req.CapacitySource.Kind,
				FreshnessMinutes: req.CapacitySource.FreshnessMinutes,
				Ref:              req.CapacitySource.Ref,
			}, req.Now, req.ExpectedMinutes, req.Confirm, req.Alternatives, req.ReadOnly, effective)
		} else {
			decision, err = GuardLaneCapacity(
				req.HomePath,
				req.Lane,
				req.CapabilityClass,
				req.Now,
				req.Confirm,
				req.Alternatives,
				effective,
			)
		}
		if err != nil {
			return work.LaneCapacityGuardResult{}, err
		}
		return laneCapacityGuardResult(decision), nil
	}
}

// laneCapacityGuardResult projects a capacity decision onto the work-package guard result. The
// production guard and the coordination observation share it so both speak the same wire shape.
func laneCapacityGuardResult(decision LaneCapacityDecision) work.LaneCapacityGuardResult {
	return work.LaneCapacityGuardResult{
		Capacity: work.DispatchLaneCapacity{
			Remaining:  decision.Capacity.Remaining,
			ResetsAt:   formatCapacityTime(decision.Capacity.ResetsAt),
			LastProven: formatCapacityTime(decision.Capacity.LastProven),
			State:      string(decision.Capacity.State),
			DataRef:    decision.Capacity.DataRef,
		},
		Refused:              decision.Refused,
		ConfirmationRequired: decision.ConfirmationRequired,
		Message:              decision.Message,
	}
}

func init() {
	work.LaneCapacityGuard = WorkLaneCapacityGuard(LaneCapacityConfig{})
}

func normalizeLaneCapacityConfig(cfg LaneCapacityConfig, homePath string) LaneCapacityConfig {
	defaults := DefaultLaneCapacityConfig(homePath)
	if strings.TrimSpace(cfg.Paths.UsageHistoryPath) == "" {
		cfg.Paths.UsageHistoryPath = defaults.Paths.UsageHistoryPath
	}
	if strings.TrimSpace(cfg.Paths.QuotaCachePath) == "" {
		cfg.Paths.QuotaCachePath = defaults.Paths.QuotaCachePath
	}
	if strings.TrimSpace(cfg.Paths.AccountProjectionPath) == "" {
		cfg.Paths.AccountProjectionPath = defaults.Paths.AccountProjectionPath
	}
	if strings.TrimSpace(cfg.Paths.AliasesPath) == "" {
		cfg.Paths.AliasesPath = defaults.Paths.AliasesPath
	}
	if strings.TrimSpace(cfg.Paths.SupplementUsagePath) == "" {
		cfg.Paths.SupplementUsagePath = defaults.Paths.SupplementUsagePath
	}
	cfg.ExpectedMinutes = capacityExpectedMinutes(cfg.ExpectedMinutes)
	return cfg
}

// capacityExpectedMinutes is the effective window for one evaluation: the requested duration
// wins, and an unset request falls back to the default window. The name carries the file's
// capacity* prefix so it can never be shadowed by an `expectedMinutes` parameter.
func capacityExpectedMinutes(requested int) int {
	if requested > 0 {
		return requested
	}
	return defaultExpectedMinutes
}

// exactLaneCapacityFamily maps a literal, already-lowercased/trimmed lane name to its capacity
// family. It returns "" for anything not one of the known exact names — the caller decides what
// to do next (retry on a stripped base, infer, or give up).
func exactLaneCapacityFamily(value string) string {
	switch value {
	case "codex":
		return "codex"
	case "claude", "claude-code", "anthropic":
		return "claude"
	case "zai", "z.ai", "glm":
		return "zai"
	case "qwen":
		return "qwen"
	case "minimax":
		return "minimax"
	default:
		return ""
	}
}

// inferLaneCapacityFamily applies structural family inference to an already sub-lane-stripped,
// lowercased base name. Precedence: zai/glm beats qwen/minimax beats generic codex/claude
// prefixes — so "claude-zai" resolves to zai, "claude-qwen" resolves to qwen, and
// "claude-minimax" resolves to minimax, never the generic claude row.
func inferLaneCapacityFamily(base string) string {
	switch {
	case strings.Contains(base, "zai") || strings.Contains(base, "glm"):
		return "zai"
	case strings.Contains(base, "minimax"):
		return "minimax"
	case strings.Contains(base, "qwen"):
		return "qwen"
	case strings.HasPrefix(base, "codex"):
		return "codex"
	case strings.HasPrefix(base, "claude"), strings.HasPrefix(base, "anthropic"):
		return "claude"
	default:
		return ""
	}
}

// normalizeLaneCapacityKey is a PURE function: structural rules only, no config/IO. Order:
//  1. exact switch on the full (lowercased, trimmed) value — unchanged legacy semantics;
//  2. strip everything from the first ':' (sub-lane suffix) and retry the exact switch on the base;
//  3. family inference (prefix/contains) on that stripped base.
//
// Real dispatch lane names carry sub-lane suffixes (`claude-main:sub-lane`) or operator-chosen
// prefixes (`claude-opus-x`) that the legacy 3-entry switch never recognized — this is the
// structural half of lane-capacity-alias resolution; resolveLaneCapacityKeyFrom layers the
// operator-owned alias override on top of it.
func normalizeLaneCapacityKey(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if key := exactLaneCapacityFamily(trimmed); key != "" {
		return key
	}
	base := trimmed
	if idx := strings.Index(base, ":"); idx >= 0 {
		base = base[:idx]
	}
	if key := exactLaneCapacityFamily(base); key != "" {
		return key
	}
	return inferLaneCapacityFamily(base)
}

// laneAliasesFile is the on-disk shape of the operator-owned lane-alias override projection
// (`<COMPOUND_HOME>/projections/lane-aliases.json`). It is config, not a governed registry: a
// missing file is a no-op (fail open); nothing here is append-only evidence.
type laneAliasesFile struct {
	SchemaVersion string            `json:"schema_version"`
	Aliases       map[string]string `json:"aliases"`
}

// loadLaneCapacityAliases reads and normalizes the lane-alias override file. A missing file
// returns (nil, nil) — fail open, no overrides. A malformed file returns a non-nil error; the
// caller decides whether to surface that (only when the lane would otherwise resolve unknown) —
// this function itself never swallows the distinction between "no file" and "bad file".
func loadLaneCapacityAliases(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lane aliases: %w", err)
	}
	var file laneAliasesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse lane aliases: %w", err)
	}
	aliases := make(map[string]string, len(file.Aliases))
	for lane, family := range file.Aliases {
		lane = strings.ToLower(strings.TrimSpace(lane))
		family = strings.ToLower(strings.TrimSpace(family))
		if lane == "" || family == "" {
			continue
		}
		aliases[lane] = family
	}
	return aliases, nil
}

// resolveLaneCapacityKeyFrom layers the operator-owned alias override on top of the pure
// structural normalizeLaneCapacityKey rules, using an ALREADY-loaded alias map. Precedence: an
// exact built-in switch hit (the literal family name, or its sub-lane-stripped base) is never
// overridden by an alias; short of that, an alias match (checked on the full lowercased/trimmed
// name, then the ':'-stripped base) beats family inference; a missing alias file is a no-op; a
// malformed alias file behaves like "no aliases" but is reported via the returned note so the
// final-unknown DataRef can surface it — composed onto the DataRef only when the final state is
// unknown, per the packet's "no silent swallow, no fleet-wide error either" rule.
//
// The caller loads the alias file eagerly — even when a structural match is found — so a
// malformed file can still surface on structurally-resolved lanes when downstream evidence
// leaves the state unknown.
func resolveLaneCapacityKeyFrom(lane string, aliases map[string]string, aliasErr error) (key string, unreadableAliasNote string) {
	trimmed := strings.ToLower(strings.TrimSpace(lane))
	if key := exactLaneCapacityFamily(trimmed); key != "" {
		return aliasResolved(key, aliasErr)
	}
	base := trimmed
	if idx := strings.Index(base, ":"); idx >= 0 {
		base = base[:idx]
	}
	if key := exactLaneCapacityFamily(base); key != "" {
		return aliasResolved(key, aliasErr)
	}

	if aliasErr == nil {
		if family, ok := aliases[trimmed]; ok {
			return family, ""
		}
		if family, ok := aliases[base]; ok {
			return family, ""
		}
	}

	inferred := inferLaneCapacityFamily(base)
	if aliasErr != nil {
		return inferred, "lane-aliases.json unreadable"
	}
	return inferred, ""
}

// aliasResolved keeps a structurally resolved family and attaches the unreadable-alias note when
// the alias file exists but could not be parsed. The note is returned only when the file exists
// but fails to parse; a missing file (IsNotExist) or a successfully-parsed file produces no note.
func aliasResolved(key string, aliasErr error) (string, string) {
	if aliasErr != nil {
		return key, "lane-aliases.json unreadable"
	}
	return key, ""
}

func capacityFreshness(lane string) time.Duration {
	switch lane {
	case "zai":
		return zaiCapacityFreshness
	case "claude":
		return claudeCapacityFreshness
	default:
		return codexCapacityFreshness
	}
}

// capacityAlternativeLadder is the ONE alternative-ladder rule: an explicit ladder wins, an
// empty capability class has no ladder, and a routing-defaults read that fails yields no ladder
// rather than an error. The fresh path supplies defaults read from the model-capabilities
// manifest; a request-scoped observation supplies the defaults it already read.
func capacityAlternativeLadder(capabilityClass string, provided []string, defaults func() (map[string][]string, error)) []string {
	if len(provided) > 0 {
		return append([]string(nil), provided...)
	}
	class := strings.TrimSpace(capabilityClass)
	if class == "" {
		return nil
	}
	routing, err := defaults()
	if err != nil {
		return nil
	}
	return append([]string(nil), routing[class]...)
}

// freshRoutingDefaults reads the model-capabilities routing defaults for a home.
func freshRoutingDefaults(homePath string) func() (map[string][]string, error) {
	return func() (map[string][]string, error) {
		manifest, _, _, err := loadModelCapabilitiesSnapshot(homePath)
		if err != nil {
			return nil, err
		}
		return manifest.RoutingDefaults, nil
	}
}

func capacityDecisionMessage(lane string, capacity LaneCapacity, alternatives []string, outcome string) string {
	timestamp := "unknown"
	if !capacity.LastProven.IsZero() {
		timestamp = capacity.LastProven.UTC().Format(time.RFC3339)
	}
	ladder := "none recorded"
	if len(alternatives) > 0 {
		ladder = strings.Join(alternatives, " -> ")
	}
	return fmt.Sprintf(
		"lane %s capacity %s: state=%s data_row_timestamp=%s data_ref=%s alternative_ladder=[%s]",
		strings.TrimSpace(lane),
		outcome,
		capacity.State,
		timestamp,
		capacity.DataRef,
		ladder,
	)
}

func recordCapacityRefusal(homePath, lane string, capacity LaneCapacity, now time.Time) error {
	key := normalizeLaneCapacityKey(lane)
	if key == "" {
		key = strings.TrimSpace(lane)
	}
	rowTimestamp := formatCapacityTime(capacity.LastProven)
	requestID := idFor("capacity_refusal_", key, string(capacity.State), rowTimestamp, now.UTC().Format(time.RFC3339))
	evidence, err := Build(BuildInput{
		CreatedAt:     now,
		RequestID:     requestID,
		ClientProfile: capacityEvidenceProfile,
		Protocol:      capacityEvidenceProtocol,
		Provider:      key,
		Model:         key,
		Status:        capacityRefusalStatus,
		EvidenceRefs: []string{
			"capacity-state:" + string(capacity.State),
			"capacity-row:" + orCapacityUnknown(rowTimestamp),
			"capacity-source:" + orCapacityUnknown(capacity.Source),
		},
	})
	if err != nil {
		return err
	}
	return RecordEvidence(homePath, evidence)
}

func formatCapacityTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func orCapacityUnknown(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}
