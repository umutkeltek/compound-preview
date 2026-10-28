package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/activity"
	"compound/internal/activity/tier1"
	"compound/internal/home"
	"compound/internal/policy"
)

// cmdActivity is the operator door onto the Tier-0 ambient-activity family (internal/activity):
// enable/disable the sensor through the governed act seam, inspect status, drain the collector's
// NDJSON spool into segments+spans (grant-gated, fail-closed), and sweep old files. It NEVER runs a
// daemon (a blind cron is a liar with a schedule) — the collector is an external launchd agent; drain
// is event-driven and operator/agent-invoked.
//
// Phase 4 adds two Tier-1 verbs:
//
//   - compound activity aw materialize  : reads AW across hostnames and writes Tier-0 spool events.
//   - compound activity tier1 collect   : bounded collect of Tier-1 envelopes into the spool.
func cmdActivity(args []string) {
	if code := runActivity(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// enforceKnownFlags checks every flag in args against a closed set. Returns a
// (badFlag, error) pair if an unknown flag is present — used by every activity
// verb to satisfy the audit's bug 7: "compound activity tier1 collect
// --duraton 5s" must NOT silently use the default. The audit's point: silent
// flag typos are especially bad on verbs that write spool data.
//
// A "flag" here is any arg starting with "--". Positional args (no leading
// "--") are NOT checked — sub-verb dispatch handles those.
func enforceKnownFlags(args []string, known map[string]bool) (string, error) {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		// Strip "=value" suffix — both "--foo=bar" and "--foo bar" appear; the
		// former is a single arg, the latter is two. Either way the flag name
		// is everything up to the first '='.
		name := a
		if eq := strings.Index(a, "="); eq >= 0 {
			name = a[:eq]
		}
		if !known[name] {
			return name, fmt.Errorf("unknown flag %q", name)
		}
	}
	return "", nil
}

func activityUsage() string {
	return "usage:\n" +
		"  compound activity status [--json] [--home P]                      sensor enabled state + storage summary\n" +
		"  compound activity enable  [--work W] [--home P]                   governed enable (needs a matching standing grant)\n" +
		"  compound activity disable [--work W] [--home P]                   governed disable\n" +
		"  compound activity drain   [--spool DIR] [--work W] [--json] [--home P]  ingest spool -> segments+spans (grant-gated, fail-closed)\n" +
		"  compound activity sweep   [--json] [--home P]                     delete spans/segments older than 30d, report reclaimed bytes\n" +
		"  compound activity aw materialize [--work W] [--home P] [--json]   read AW across hostnames -> spool (Tier-0, grant-gated)\n" +
		"  compound activity tier1 collect  [--lookback 30s] [--timeout 5s] [--work W] [--home P] [--json]  bounded collect of Tier-1 envelopes -> spool (grant-gated)"
}

func runActivity(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(stderr, activityUsage())
		return 2
	}
	switch args[0] {
	case "status":
		return runActivityStatus(args[1:], stdout, stderr)
	case "enable":
		return runActivityToggle(args[1:], stdout, stderr, true)
	case "disable":
		return runActivityToggle(args[1:], stdout, stderr, false)
	case "drain":
		return runActivityDrain(args[1:], stdout, stderr)
	case "sweep":
		return runActivitySweep(args[1:], stdout, stderr)
	case "aw":
		return runActivityAW(args[1:], stdout, stderr)
	case "tier1":
		return runActivityTier1(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, activityUsage())
		return 2
	}
}

func runActivityStatus(args []string, stdout, stderr io.Writer) int {
	if bad, err := enforceKnownFlags(args, map[string]bool{"--json": true, "--home": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, activityUsage())
		_ = bad
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// Read-only: never write config from status. If config is missing,
	// report config_status="missing" and enabled=false; the operator runs
	// `compound activity enable --work W` to materialize config through the
	// governed path.
	enabled, err := activity.Enabled(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	configStatus := "present"
	if _, statErr := os.Stat(activity.ConfigPath(homePath)); statErr != nil {
		if os.IsNotExist(statErr) {
			configStatus = "missing"
		} else {
			fmt.Fprintln(stderr, "error:", statErr)
			return 1
		}
	}
	out := map[string]any{
		"enabled":       enabled,
		"config_status": configStatus,
		"config_path":   activity.ConfigPath(homePath),
		"activity_root": activity.ActivityRoot(homePath),
		"spool_dir":     defaultSpoolDir(),
	}
	if hasFlag(args, "--json") {
		return writeJSON(stdout, stderr, out)
	}
	fmt.Fprintf(stdout, "activity sensor enabled=%v config_status=%s\n", enabled, configStatus)
	fmt.Fprintf(stdout, "  config: %s\n", activity.ConfigPath(homePath))
	fmt.Fprintf(stdout, "  store:  %s\n", activity.ActivityRoot(homePath))
	fmt.Fprintf(stdout, "  spool:  %s\n", defaultSpoolDir())
	if configStatus == "missing" {
		fmt.Fprintln(stdout, "  hint: run `compound activity enable --work W` to initialize config through the governed path")
	}
	return 0
}

func runActivityToggle(args []string, stdout, stderr io.Writer, enable bool) int {
	if bad, err := enforceKnownFlags(args, map[string]bool{"--work": true, "--home": true, "--json": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, activityUsage())
		_ = bad
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	work := activityWorkID(args, "toggle")
	res, err := activity.SetEnabled(homePath, cfg, work, enable, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if res.Refused {
		fmt.Fprintf(stderr, "refused: %s (needs a standing grant for %q)\n", res.Reason, activity.ToggleCapability)
		return 1
	}
	if hasFlag(args, "--json") {
		return writeJSON(stdout, stderr, res)
	}
	verb := "disabled"
	if enable {
		verb = "enabled"
	}
	fmt.Fprintf(stdout, "activity sensor %s (receipt=%s transition=%s)\n", verb, res.ReceiptID, res.TransitionID)
	return 0
}

func runActivityDrain(args []string, stdout, stderr io.Writer) int {
	if bad, err := enforceKnownFlags(args, map[string]bool{"--spool": true, "--work": true, "--json": true, "--home": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, activityUsage())
		_ = bad
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	spool := strings.TrimSpace(flagValue(args, "--spool"))
	if spool == "" {
		spool = defaultSpoolDir()
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	work := activityWorkID(args, "drain")
	rep, err := activity.Drain(homePath, spool, cfg, work, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		code := 0
		if rep.Refused {
			code = 1
		}
		if writeJSON(stdout, stderr, rep) != 0 {
			return 1
		}
		return code
	}
	if rep.Refused {
		fmt.Fprintf(stderr, "drain refused: %s\n", rep.Reason)
		return 1
	}
	fmt.Fprintf(stdout, "drain: ingested=%d refused_lines=%d days=%s bytes=%d\n",
		rep.SegmentLinesAdded, rep.RefusedLines, strings.Join(rep.DaysWritten, ","), rep.BytesUnderRoot)
	return 0
}

func runActivitySweep(args []string, stdout, stderr io.Writer) int {
	if bad, err := enforceKnownFlags(args, map[string]bool{"--json": true, "--home": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, activityUsage())
		_ = bad
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	rep, err := activity.Sweep(homePath, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		return writeJSON(stdout, stderr, rep)
	}
	fmt.Fprintf(stdout, "sweep: deleted=%d reclaimed_bytes=%d\n", rep.FilesDeleted, rep.BytesReclaimed)
	return 0
}

// activityWorkID reads --work or synthesizes a deterministic-enough default (a governed act belongs
// to a work; a standalone operator toggle/drain still needs one).
func activityWorkID(args []string, verb string) string {
	if w := strings.TrimSpace(flagValue(args, "--work")); w != "" {
		return w
	}
	return "work_activity_" + verb
}

// defaultSpoolDir is the collector's NDJSON output dir: a generic application-support path under the
// user's home (never a hardcoded personal path). The Swift collector writes here; drain reads here.
func defaultSpoolDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("Library", "Application Support", "compound-activity", "spool")
	}
	return filepath.Join(h, "Library", "Application Support", "compound-activity", "spool")
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// runActivityAW dispatches `compound activity aw ...`. Today only `materialize` is
// implemented; future sub-verbs (e.g., `aw buckets` for listing the joined virtual
// stream) land here.
func runActivityAW(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: compound activity aw materialize [--work W] [--home P] [--json]")
		return 2
	}
	switch args[0] {
	case "materialize":
		return runActivityAWMaterialize(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown aw sub-verb: %s\n", args[0])
		return 2
	}
}

// runActivityAWMaterialize reads AW buckets across hostnames, normalizes them via
// awstream.Normalize, and writes Tier-0 spool events. Useful when AW is the only
// activity source on this host (no Swift launchd collector running). Governance:
// the same Enable gate as drain. Tier-1 envelope extraction is a SEPARATE verb
// (`tier1 collect`).
//
// Phase 4 ships the verb as a working surface; the underlying AW materialize
// routine (`tier1.AWMaterialize`) is implemented as a thin wrapper around the
// awstream reader + the existing spool writer. The Tier-1 envelope collector
// (`tier1.Collect`) is a distinct verb.
func runActivityAWMaterialize(args []string, stdout, stderr io.Writer) int {
	if bad, err := enforceKnownFlags(args, map[string]bool{"--start": true, "--end": true, "--work": true, "--home": true, "--json": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, activityUsage())
		_ = bad
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	work := activityWorkID(args, "aw_materialize")
	now := time.Now()
	start, end, ok := parseAWMaterializeWindow(args, now, stderr)
	if !ok {
		return 2
	}
	rep, err := tier1.AWMaterialize(homePath, defaultSpoolDir(), cfg, work, start, end, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// Both a governance refusal and an operational failure (AW down, spool
	// unwritable, every bucket read errored) are nonzero. They stay distinct in
	// the message so an operator can tell "not allowed" from "did not work".
	if hasFlag(args, "--json") {
		code := 0
		if rep.Refused || rep.Failed {
			code = 1
		}
		if writeJSON(stdout, stderr, rep) != 0 {
			return 1
		}
		return code
	}
	if rep.Failed {
		fmt.Fprintf(stderr, "aw materialize failed: %s\n", rep.Reason)
		return 1
	}
	if rep.Refused {
		fmt.Fprintf(stderr, "aw materialize refused: %s\n", rep.Reason)
		return 1
	}
	if rep.BucketsFailed > 0 {
		fmt.Fprintf(stdout, "aw materialize: events_written=%d buckets=%d buckets_failed=%d bytes=%d\n",
			rep.EventsWritten, rep.BucketsRead, rep.BucketsFailed, rep.BytesUnderRoot)
		return 0
	}
	fmt.Fprintf(stdout, "aw materialize: events_written=%d buckets=%d bytes=%d\n",
		rep.EventsWritten, rep.BucketsRead, rep.BytesUnderRoot)
	return 0
}

// awWindowFlag reads a window flag in BOTH "--name value" and "--name=value"
// forms. enforceKnownFlags accepts the "=" form, so valueFlag alone would
// silently drop it — and silently ignoring a bounding flag is exactly the bug
// this verb is fixing.
func awWindowFlag(args []string, name string) string {
	if v := strings.TrimSpace(flagValue(args, name)); v != "" {
		return v
	}
	prefix := name + "="
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(a, prefix))
		}
	}
	return ""
}

// parseAWMaterializeWindow resolves the [start,end] AW data window from
// --start/--end (RFC3339), defaulting to the trailing 7 days ending at now.
// It returns ok=false (CLI exits 2) on a malformed timestamp or an inverted
// window, so an operator who thinks they bounded the query really did — the
// previous behavior accepted the flags and then always fetched 7 days.
func parseAWMaterializeWindow(args []string, now time.Time, stderr io.Writer) (start, end time.Time, ok bool) {
	end = now
	start = now.Add(-7 * 24 * time.Hour)
	if s := awWindowFlag(args, "--end"); s != "" {
		parsed, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			fmt.Fprintf(stderr, "invalid --end %q: expected RFC3339 (e.g. 2026-07-31T00:00:00Z): %v\n", s, perr)
			return time.Time{}, time.Time{}, false
		}
		end = parsed
	}
	if s := awWindowFlag(args, "--start"); s != "" {
		parsed, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			fmt.Fprintf(stderr, "invalid --start %q: expected RFC3339 (e.g. 2026-07-30T00:00:00Z): %v\n", s, perr)
			return time.Time{}, time.Time{}, false
		}
		start = parsed
	}
	if !start.Before(end) {
		fmt.Fprintf(stderr, "invalid window: --start %s must be before --end %s\n",
			start.Format(time.RFC3339), end.Format(time.RFC3339))
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

// runActivityTier1 dispatches `compound activity tier1 ...`. Today only `collect`
// is implemented; future sub-verbs (e.g., `tier1 status` for last-run summary)
// land here.
func runActivityTier1(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: compound activity tier1 collect [--lookback 30s] [--timeout 5s] [--work W] [--home P] [--json]")
		return 2
	}
	switch args[0] {
	case "collect":
		return runActivityTier1Collect(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown tier1 sub-verb: %s\n", args[0])
		return 2
	}
}

// runActivityTier1Collect runs a bounded Tier-1 collect.
//
// Two distinct timing flags (the audit's bug 4):
//
//   - --lookback: the AW data window to fetch events from. Default 30s.
//   - --timeout: the wall-clock budget for the run. Default 5s. The wrapper
//     applies context.WithTimeout so a hung source (lsof, pgrep, cmux IPC)
//     cannot keep the collector alive past this budget.
//
// Governance: Tier1CollectCapability grant + Enabled config + byte cap.
func runActivityTier1Collect(args []string, stdout, stderr io.Writer) int {
	if bad, err := enforceKnownFlags(args, map[string]bool{"--lookback": true, "--timeout": true, "--work": true, "--home": true, "--json": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, activityUsage())
		_ = bad
		return 2
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	lookback := 30 * time.Second
	if d := strings.TrimSpace(flagValue(args, "--lookback")); d != "" {
		parsed, perr := time.ParseDuration(d)
		if perr != nil {
			fmt.Fprintf(stderr, "invalid --lookback %q: %v\n", d, perr)
			return 1
		}
		lookback = parsed
	}
	timeout := 5 * time.Second
	if d := strings.TrimSpace(flagValue(args, "--timeout")); d != "" {
		parsed, perr := time.ParseDuration(d)
		if perr != nil {
			fmt.Fprintf(stderr, "invalid --timeout %q: %v\n", d, perr)
			return 1
		}
		timeout = parsed
	}
	work := activityWorkID(args, "tier1_collect")
	rep, err := tier1.Collect(homePath, defaultSpoolDir(), cfg, work, lookback, timeout, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		code := 0
		if rep.Refused || rep.Failed {
			code = 1
		}
		if writeJSON(stdout, stderr, rep) != 0 {
			return 1
		}
		return code
	}
	if rep.Refused {
		fmt.Fprintf(stderr, "tier1 collect refused: %s\n", rep.Reason)
		return 1
	}
	if rep.Failed {
		fmt.Fprintf(stderr, "tier1 collect failed: %s\n", rep.Reason)
		return 1
	}
	fmt.Fprintf(stdout, "tier1 collect: events_written=%d sources_called=%d sources_failed=%d buckets_failed=%d banned_refused=%d lookback=%s timeout=%s\n",
		rep.EventsWritten, rep.SourcesCalled, rep.SourcesFailed, rep.BucketsFailed, rep.BannedRefused, lookback, timeout)
	return 0
}
