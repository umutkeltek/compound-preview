//go:build darwin

// Compound `schedule` surface: read-only projection of the macOS launchd
// surface (user LaunchAgents, system LaunchAgents, system LaunchDaemons) and
// the user crontab, grouped by namespace inferred from each launchd label
// (see inferNamespace; unattributable entries land in "other"). Designed
// to make declared and observed runtime state legible. Exact reviewed repair
// delegates to authority; the projection itself never writes OS state.
//
// Doctrinal anchors:
//  - Read-only by default; every entry point emits a projection that can be
//    diffed against a previous run.
//  - Fail-closed on plutil / crontab errors per source so one broken plist
//    does not hide the rest of the surface.
//  - Namespace inference is heuristic (label second-segment or known list)
//    and exposed verbatim in JSON so callers can override.

package schedule

import (
	"bufio"
	"compound/internal/authority"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type scheduleSource string

const (
	scheduleSourceUser    scheduleSource = "user"
	scheduleSourceSystem  scheduleSource = "system"
	scheduleSourceCrontab scheduleSource = "crontab"
)

// ScheduleEntry is one row of the read-only projection. JSON-friendly and
// stable enough to diff across runs.
type ScheduleRuntimeState struct {
	Domain              string `json:"domain"`
	OverrideState       string `json:"override_state"`
	LoadedState         string `json:"loaded_state"`
	EffectiveEnablement string `json:"effective_enablement"`
	Readiness           string `json:"readiness"`
	Error               string `json:"error,omitempty"`
}

type ScheduleEntry struct {
	DeclaredDisabledState string                `json:"declared_disabled_state"`
	Runtime               *ScheduleRuntimeState `json:"runtime,omitempty"`
	Label                 string                `json:"label"`
	Path                  string                `json:"path"`
	Source                scheduleSource        `json:"source"`
	Namespace             string                `json:"namespace"`
	Disabled              bool                  `json:"disabled"`
	DisabledByExt         bool                  `json:"disabled_by_extension"` // *.plist.disabled
	RunAtLoad             bool                  `json:"run_at_load"`
	KeepAlive             bool                  `json:"keep_alive"`
	ProgramArgs           []string              `json:"program_args,omitempty"`
	EnvKeys               []string              `json:"env_keys,omitempty"` // sorted env var names; populated when Env is redacted out
	Calendar              *scheduleCalendar     `json:"calendar,omitempty"`
	Env                   map[string]string     `json:"env,omitempty"`
	StdOutPath            string                `json:"stdout_path,omitempty"`
	StdErrPath            string                `json:"stderr_path,omitempty"`
	MTime                 string                `json:"mtime,omitempty"`
	ModTimeAge            string                `json:"mod_age,omitempty"`
	Warnings              []string              `json:"warnings,omitempty"`
}

type scheduleCalendar struct {
	Hour    int `json:"hour,omitempty"`
	Minute  int `json:"minute,omitempty"`
	Weekday int `json:"weekday,omitempty"`
}

type scheduleDoctorFinding struct {
	Label  string         `json:"label"`
	Path   string         `json:"path"`
	Source scheduleSource `json:"source"`
	Code   string         `json:"code"` // disabled, disabled-by-extension, missing-binary, stale
	Detail string         `json:"detail"`
}

type scheduleDoctorReport struct {
	Schema    string                  `json:"schema"` // compound/schedule-doctor.v0.1
	Source    string                  `json:"source"`
	Generated string                  `json:"generated_at"`
	StaleDays int                     `json:"stale_days"`
	Codes     map[string]int          `json:"codes"`
	Findings  []scheduleDoctorFinding `json:"findings"`
}

const scheduleDoctorSchema = "compound/schedule-doctor.v0.1"

// Run is the organ entry point behind `compound schedule` (the cmd-budget
// ratchet keeps cmd/compound a dispatch layer; the surface lives here).
func Run(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, scheduleUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		cmdScheduleList(args[1:])
	case "show":
		cmdScheduleShow(args[1:])
	case "doctor":
		cmdScheduleDoctor(args[1:])
	case "repair":
		os.Exit(runScheduleRepair(args[1:]))
	default:
		fmt.Fprintln(os.Stderr, scheduleUsage)
		os.Exit(2)
	}
}

func cmdScheduleList(args []string) {
	if err := validateScheduleFlags("list", args, 0); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, scheduleUsage)
		os.Exit(2)
	}
	jsonOut := hasFlag(args, "--json")
	source := "all"
	if v := strings.TrimSpace(valueFlag(args, "--source")); v != "" {
		if err := validateScheduleSource(v); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, scheduleUsage)
			os.Exit(2)
		}
		source = v
	}
	nsFilter := strings.TrimSpace(valueFlag(args, "--namespace"))
	includeEnv := hasFlag(args, "--include-env-values")

	entries, errs := collectSchedule(source)
	entries = prepareScheduleEntries(entries, nsFilter, includeEnv)
	if jsonOut {
		out := struct {
			Schema    string          `json:"schema"`
			Generated string          `json:"generated_at"`
			Source    string          `json:"source_filter"`
			Entries   []ScheduleEntry `json:"entries"`
			Errors    []string        `json:"source_errors,omitempty"`
		}{
			Schema:    "compound/schedule-list.v0.1",
			Generated: time.Now().UTC().Format(time.RFC3339Nano),
			Source:    source,
			Entries:   entries,
		}
		for _, e := range errs {
			out.Errors = append(out.Errors, e.Error())
		}
		printJSON(out)
		return
	}
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "# %d source read error(s):\n", len(errs))
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "- %s\n", e.Error())
		}
	}
	grouped := groupByNamespace(entries)
	for _, ns := range sortedKeys(grouped) {
		fmt.Printf("# namespace: %s (%d)\n", ns, len(grouped[ns]))
		for _, e := range grouped[ns] {
			printScheduleEntry(e)
		}
	}
}

func cmdScheduleShow(args []string) {
	jsonOut := hasFlag(args, "--json")
	target := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		target = a
		break
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "usage: compound schedule show <label|path> [--json]")
		os.Exit(2)
	}
	entries, _ := collectSchedule("all")
	var hit *ScheduleEntry
	for i := range entries {
		e := entries[i]
		if e.Label == target || e.Path == target || filepath.Base(e.Path) == target {
			hit = &e
			break
		}
	}
	if hit == nil {
		fmt.Fprintf(os.Stderr, "schedule show: no entry matching %q\n", target)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(struct {
			Schema    string        `json:"schema"`
			Generated string        `json:"generated_at"`
			Entry     ScheduleEntry `json:"entry"`
		}{
			Schema:    "compound/schedule-show.v0.1",
			Generated: time.Now().UTC().Format(time.RFC3339Nano),
			Entry:     *hit,
		})
		return
	}
	printScheduleEntryVerbose(*hit)
}

func cmdScheduleDoctor(args []string) {
	if err := validateScheduleFlags("doctor", args, 0); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, scheduleUsage)
		os.Exit(2)
	}
	jsonOut := hasFlag(args, "--json")
	staleDays := 90
	if v := strings.TrimSpace(valueFlag(args, "--stale-days")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
			fmt.Fprintln(os.Stderr, "error: --stale-days must be a non-negative integer")
			os.Exit(2)
		}
		staleDays = n
	}
	entries, errs := collectSchedule("all")
	now := time.Now()
	report := scheduleDoctorReport{
		Schema:    scheduleDoctorSchema,
		Source:    "all",
		Generated: now.UTC().Format(time.RFC3339Nano),
		StaleDays: staleDays,
		Codes:     map[string]int{},
	}
	seen := map[string]int{}
	for _, e := range entries {
		if e.Runtime != nil {
			code, detail := "", ""
			switch {
			case e.Runtime.Error != "":
				code = "runtime-unknown"
				detail = e.Runtime.Error
			case e.Runtime.OverrideState == "disabled":
				code = "disabled-override"
				detail = "launchctl explicitly disables this label"
			case e.Runtime.LoadedState == "absent" && !e.Disabled && !e.DisabledByExt:
				code = "not-loaded"
				detail = "launchd has no loaded service; plist declaration alone is not runtime readiness"
			}
			if code != "" {
				report.Findings = append(report.Findings, scheduleDoctorFinding{Label: e.Label, Path: e.Path, Source: e.Source, Code: code, Detail: detail})
			}
		}
		seen[e.Label]++
		if e.Disabled {
			report.Findings = append(report.Findings, scheduleDoctorFinding{
				Label: e.Label, Path: e.Path, Source: e.Source,
				Code: "disabled", Detail: "plist declares Disabled=true",
			})
		}
		if e.DisabledByExt {
			report.Findings = append(report.Findings, scheduleDoctorFinding{
				Label: e.Label, Path: e.Path, Source: e.Source,
				Code: "disabled-by-extension", Detail: "plist filename ends in .disabled",
			})
		}
		if !e.Disabled && len(e.ProgramArgs) > 0 {
			statPath := expandEnvPath(e.ProgramArgs[0], e.Env)
			if _, err := os.Stat(statPath); err != nil {
				report.Findings = append(report.Findings, scheduleDoctorFinding{
					Label: e.Label, Path: e.Path, Source: e.Source,
					Code:   "missing-binary",
					Detail: fmt.Sprintf("ProgramArguments[0] %q (expanded: %q) not executable: %v", e.ProgramArgs[0], statPath, err),
				})
			}
		}
		if !e.Disabled && staleDays > 0 {
			if e.MTime != "" {
				if mt, err := time.Parse(time.RFC3339Nano, e.MTime); err == nil {
					age := now.Sub(mt)
					if age > time.Duration(staleDays)*24*time.Hour {
						report.Findings = append(report.Findings, scheduleDoctorFinding{
							Label: e.Label, Path: e.Path, Source: e.Source,
							Code:   "stale",
							Detail: fmt.Sprintf("mtime age %s exceeds %d days", age.Round(time.Hour), staleDays),
						})
					}
				}
			}
		}
	}
	for label, count := range seen {
		if count > 1 {
			report.Findings = append(report.Findings, scheduleDoctorFinding{
				Label: label, Source: scheduleSourceUser,
				Code:   "duplicate-label",
				Detail: fmt.Sprintf("label appears %d times", count),
			})
		}
	}
	for _, e := range errs {
		report.Findings = append(report.Findings, scheduleDoctorFinding{
			Source: scheduleSourceSystem, // best-effort
			Code:   "source-read-error",
			Detail: e.Error(),
		})
	}
	for _, f := range report.Findings {
		report.Codes[f.Code]++
	}
	if jsonOut {
		printJSON(report)
		return
	}
	fmt.Printf("# schedule doctor (%s)\n", scheduleDoctorSchema)
	fmt.Printf("# generated: %s  stale_days: %d  findings: %d\n", report.Generated, report.StaleDays, len(report.Findings))
	if len(report.Findings) == 0 {
		fmt.Println("- none")
		return
	}
	codes := make([]string, 0, len(report.Codes))
	for c := range report.Codes {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		fmt.Printf("## %s (%d)\n", c, report.Codes[c])
		for _, f := range report.Findings {
			if f.Code != c {
				continue
			}
			loc := string(f.Source) + ":" + f.Label
			if f.Path != "" {
				loc = string(f.Source) + ":" + f.Path
			}
			fmt.Printf("- %s %s\n", loc, f.Detail)
		}
	}
}

// collectSchedule gathers entries from the requested sources. Per-source
// errors are returned alongside the partial results so callers can show what
// worked and what didn't.
func collectSchedule(sourceFilter string) ([]ScheduleEntry, []error) {
	var entries []ScheduleEntry
	var errs []error
	wants := func(s scheduleSource) bool {
		switch sourceFilter {
		case "all", "":
			return true
		case "user":
			return s == scheduleSourceUser
		case "system":
			return s == scheduleSourceSystem
		case "crontab":
			return s == scheduleSourceCrontab
		default:
			return false
		}
	}
	if wants(scheduleSourceUser) {
		es, err := collectLaunchAgents(filepath.Join(homeUserHome(), "Library", "LaunchAgents"))
		if err != nil {
			errs = append(errs, fmt.Errorf("user launchd: %w", err))
		}
		projectLaunchdRuntime(es)
		for i := range es {
			es[i].Source = scheduleSourceUser
			entries = append(entries, es[i])
		}
	}
	if wants(scheduleSourceSystem) {
		esA, err := collectLaunchAgents("/Library/LaunchAgents")
		if err != nil && !errors.Is(err, errNotExist) {
			errs = append(errs, fmt.Errorf("system launchd agents: %w", err))
		}
		for i := range esA {
			esA[i].Source = scheduleSourceSystem
			entries = append(entries, esA[i])
		}
		esD, err := collectLaunchAgents("/Library/LaunchDaemons")
		if err != nil && !errors.Is(err, errNotExist) {
			errs = append(errs, fmt.Errorf("system launchd daemons: %w", err))
		}
		for i := range esD {
			esD[i].Source = scheduleSourceSystem
			entries = append(entries, esD[i])
		}
	}
	if wants(scheduleSourceCrontab) {
		cs, err := collectCrontab()
		if err != nil {
			errs = append(errs, fmt.Errorf("crontab: %w", err))
		}
		entries = append(entries, cs...)
	}
	for i := range entries {
		entries[i].Namespace = inferNamespace(entries[i].Label)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Source != entries[j].Source {
			return entries[i].Source < entries[j].Source
		}
		return entries[i].Label < entries[j].Label
	})
	return entries, errs
}

var errNotExist = errors.New("path not present")

func homeUserHome() string {
	if h := os.Getenv("HOME"); strings.TrimSpace(h) != "" {
		return h
	}
	return "/var/empty"
}

func collectLaunchAgents(dir string) ([]ScheduleEntry, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, errNotExist
		}
		return nil, err
	}
	// Match *.plist and *.plist.disabled (a launchd convention for soft-disabling
	// a job without uninstalling it). Files like *.plist.bak-* or *.plist.pre-*
	// are deliberately excluded — they are pre-edit snapshots, not live plists.
	matches, err := filepath.Glob(filepath.Join(dir, "*.plist"))
	if err != nil {
		return nil, err
	}
	disabled, err := filepath.Glob(filepath.Join(dir, "*.plist.disabled"))
	if err != nil {
		return nil, err
	}
	matches = append(matches, disabled...)
	var out []ScheduleEntry
	for _, path := range matches {
		e, err := readPlistEntry(path)
		if err != nil {
			e = ScheduleEntry{
				Path:     path,
				Label:    filepath.Base(path),
				Warnings: []string{"plist parse: " + err.Error()},
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// readPlistEntry reads a (possibly binary) plist file via `plutil -convert
// json -o -`, then parses the resulting JSON for the fields the projection
// exposes. JSON via plutil is preferred over XML because plutil's JSON
// output maps cleanly to encoding/json and handles every plist value type
// the launchd surface emits (string, int, bool, array, dict). Unknown keys
// are ignored on purpose so new launchd keys do not break the projection.
func readPlistEntry(path string) (ScheduleEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", path)
	out, err := cmd.Output()
	if err != nil {
		return ScheduleEntry{}, fmt.Errorf("plutil: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return ScheduleEntry{}, fmt.Errorf("plist json: %w", err)
	}
	e := ScheduleEntry{Path: path}
	if st, err := os.Stat(path); err == nil {
		e.MTime = st.ModTime().UTC().Format(time.RFC3339Nano)
		e.ModTimeAge = time.Since(st.ModTime()).Round(time.Second).String()
	}
	if strings.HasSuffix(path, ".plist.disabled") {
		e.DisabledByExt = true
	}
	e.Label = stringField(raw, "Label")
	e.Disabled = boolField(raw, "Disabled")
	e.DeclaredDisabledState = "absent"
	if v, present := raw["Disabled"]; present {
		if b, ok := v.(bool); ok {
			e.DeclaredDisabledState = "enabled"
			if b {
				e.DeclaredDisabledState = "disabled"
			}
		} else {
			e.DeclaredDisabledState = "unknown"
		}
	}
	e.RunAtLoad = boolField(raw, "RunAtLoad")
	e.KeepAlive = boolField(raw, "KeepAlive")
	e.ProgramArgs = stringArrayField(raw, "ProgramArguments")
	e.StdOutPath = stringField(raw, "StandardOutPath")
	e.StdErrPath = stringField(raw, "StandardErrorPath")
	if e.Label == "" {
		e.Label = strings.TrimSuffix(filepath.Base(path), ".plist.disabled")
	}
	if cal := dictField(raw, "StartCalendarInterval"); cal != nil {
		e.Calendar = &scheduleCalendar{
			Hour:    intField(cal, "Hour"),
			Minute:  intField(cal, "Minute"),
			Weekday: intField(cal, "Weekday"),
		}
	}
	if env := dictField(raw, "EnvironmentVariables"); env != nil {
		e.Env = stringMapField(env)
	}
	return e, nil
}

// parseCrontabLines parses crontab text into ScheduleEntry rows. Pure
// function: no exec, no filesystem — easy to unit-test against synthetic
// crontabs without `crontab -l` needing to exist.
//
// crontab semantics worth preserving: leading "NAME=VALUE" lines are env
// assignments inherited by every subsequent schedule line. We capture them
// per-entry so the doctor can expand $VAR before stat'ing the program path
// (cron's parser does this expansion; a literal stat does not).
func parseCrontabLines(text string) []ScheduleEntry {
	var entries []ScheduleEntry
	env := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if isCrontabEnvLine(line) {
			if k, v, ok := strings.Cut(line, "="); ok && k != "" && !strings.ContainsAny(k, " \t") {
				env[k] = v
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		entryEnv := make(map[string]string, len(env))
		for k, v := range env {
			entryEnv[k] = v
		}
		// Label carries the schedule prefix AND the command so identical
		// schedules invoking different commands are distinguishable in the
		// list/group output (e.g. "0 * * * * job-one" vs "job-two").
		// Different invocations of the same command+schedule get the same
		// label, which is the caller's signal that the duplicate is the
		// same job, not a coincidentally-coinciding rule.
		cmd := fields[5]
		if i := strings.LastIndex(cmd, "/"); i >= 0 {
			cmd = cmd[i+1:]
		}
		entries = append(entries, ScheduleEntry{
			Label:       "crontab:" + strings.Join(fields[:5], " ") + " " + cmd,
			Path:        "crontab",
			Source:      scheduleSourceCrontab,
			Namespace:   "other",
			ProgramArgs: fields[5:],
			Env:         entryEnv,
		})
	}
	return entries
}

// isCrontabEnvLine reports whether a trimmed crontab line is an env
// assignment rather than a schedule entry. cron treats leading "NAME=VALUE"
// lines as Bourne-shell-style exports; this helper is intentionally narrow —
// any line that begins with a cron schedule token (digit, "*", "@") is a
// schedule, not env, even if it happens to contain "=".
func isCrontabEnvLine(line string) bool {
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "@") {
		return false
	}
	first := line[0]
	if first == '*' || (first >= '0' && first <= '9') {
		return false
	}
	return strings.Contains(line, "=")
}

// collectCrontab shells out to `crontab -l` and delegates parsing to
// parseCrontabLines. Only the user crontab is consulted; system crontabs
// (/etc/cron.d) are out of scope for compound observation per the B-scope
// envelope.
func collectCrontab() ([]ScheduleEntry, error) {
	cmd := exec.Command("crontab", "-l")
	out, err := cmd.Output()
	if err != nil {
		// crontab returns non-zero when there is no crontab for the user.
		// Treat that as empty rather than an error so the projection is total.
		if exitErr, ok := err.(*exec.ExitError); ok && strings.Contains(string(exitErr.Stderr), "no crontab") {
			return nil, nil
		}
		return nil, fmt.Errorf("crontab -l: %w", err)
	}
	return parseCrontabLines(string(out)), nil
}

// printScheduleEntry renders one entry in compact human form.
func printScheduleEntry(e ScheduleEntry) {
	state := "declared"
	if e.Runtime != nil {
		state = e.Runtime.Readiness
	}
	switch {
	case e.DisabledByExt:
		state = "disabled-by-extension"
	case e.Disabled:
		state = "disabled"
	}
	fmt.Printf("- [%s] %s (%s)\n", e.Namespace, e.Label, state)
	if len(e.ProgramArgs) > 0 {
		fmt.Printf("    cmd: %s\n", strings.Join(e.ProgramArgs, " "))
	}
	if e.Calendar != nil && (e.Calendar.Hour != 0 || e.Calendar.Minute != 0) {
		fmt.Printf("    calendar: %02d:%02d weekday=%d\n", e.Calendar.Hour, e.Calendar.Minute, e.Calendar.Weekday)
	}
	if e.RunAtLoad {
		fmt.Printf("    run_at_load: true\n")
	}
	if e.KeepAlive {
		fmt.Printf("    keep_alive: true\n")
	}
	for _, w := range e.Warnings {
		fmt.Printf("    warning: %s\n", w)
	}
}

func printScheduleEntryVerbose(e ScheduleEntry) {
	fmt.Printf("# schedule show\n")
	fmt.Printf("label:    %s\n", e.Label)
	fmt.Printf("path:     %s\n", e.Path)
	fmt.Printf("source:   %s\n", e.Source)
	fmt.Printf("namespace:%s\n", e.Namespace)
	fmt.Printf("disabled: %v (by_extension=%v)\n", e.Disabled, e.DisabledByExt)
	if len(e.ProgramArgs) > 0 {
		fmt.Printf("cmd:      %s\n", strings.Join(e.ProgramArgs, " "))
	}
	if e.Calendar != nil {
		fmt.Printf("calendar: %02d:%02d weekday=%d\n", e.Calendar.Hour, e.Calendar.Minute, e.Calendar.Weekday)
	}
	if e.RunAtLoad {
		fmt.Printf("run_at_load: true\n")
	}
	if e.KeepAlive {
		fmt.Printf("keep_alive: true\n")
	}
	if e.MTime != "" {
		fmt.Printf("mtime:    %s (age %s)\n", e.MTime, e.ModTimeAge)
	}
	if len(e.Env) > 0 {
		keys := make([]string, 0, len(e.Env))
		for k := range e.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("env:      %s=%s\n", k, e.Env[k])
		}
	}
	for _, w := range e.Warnings {
		fmt.Printf("warning:  %s\n", w)
	}
}

func filterByNamespace(in []ScheduleEntry, ns string) []ScheduleEntry {
	var out []ScheduleEntry
	for _, e := range in {
		if e.Namespace == ns {
			out = append(out, e)
		}
	}
	return out
}

func groupByNamespace(in []ScheduleEntry) map[string][]ScheduleEntry {
	out := map[string][]ScheduleEntry{}
	for _, e := range in {
		out[e.Namespace] = append(out[e.Namespace], e)
	}
	return out
}

// scheduleFlagSets enumerates the accepted flags per subcommand. Keep this

// scheduleValidSources is the canonical set accepted by --source. Listed
// here, not in collectSchedule's switch, so the error message and the
// happy-path filter cannot drift: validateScheduleSource and collectSchedule
// both reference the same table. Adding a new source means one entry here
// + the switch arms in collectSchedule (failure mode: validate accepts a
// value the filter silently drops to zero entries, which is exactly the
// silent-empty bug clause (a) exists to prevent).
var scheduleValidSources = []string{"all", "user", "system", "crontab"}

// validateScheduleSource fails closed on an unknown --source value with an
// error message that names the canonical set. Called from cmdScheduleList
// before collectSchedule so a typo cannot silently produce an empty
// projection. The default ("all") is set at the call site and is not
// re-validated; this helper only runs when the user passed --source
// explicitly.
func validateScheduleSource(v string) error {
	for _, s := range scheduleValidSources {
		if v == s {
			return nil
		}
	}
	return fmt.Errorf("--source %q invalid; valid values: %s", v, strings.Join(scheduleValidSources, ", "))
}

// prepareScheduleEntries is the shared seam for list/doctor/anything that
// wants the projection ready for emission. It applies the namespace filter,
// and — to keep secrets out of operator-facing output by default — strips
// Env values when includeEnv is false, replacing them with a sorted
// EnvKeys list. Caller controls opt-in via --include-env-values.
func prepareScheduleEntries(in []ScheduleEntry, ns string, includeEnv bool) []ScheduleEntry {
	out := make([]ScheduleEntry, 0, len(in))
	for _, e := range in {
		if ns != "" && e.Namespace != ns {
			continue
		}
		keys := make([]string, 0, len(e.Env))
		for k := range e.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		cp := e
		cp.EnvKeys = keys
		if !includeEnv {
			cp.Env = nil
		}
		out = append(out, cp)
	}
	return out
}

func sortedKeys(m map[string][]ScheduleEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// inferNamespace extracts a stable namespace from a launchd label using
// known conventions: ai.<ns>.<...> -> ns, com.<ns>.<...> -> ns, else "other".
// The full label is also returned in the JSON entry so callers can override.
func inferNamespace(label string) string {
	if strings.HasPrefix(label, "crontab:") {
		return "crontab"
	}
	parts := strings.Split(label, ".")
	if len(parts) >= 3 && (parts[0] == "ai" || parts[0] == "com") {
		return parts[1]
	}
	if len(parts) >= 2 {
		return parts[0]
	}
	return "other"
}

// projectLaunchdRuntime shares one disabled-override snapshot and places a
// deadline on the whole projection. Each service probe remains exact-label.
func projectLaunchdRuntime(entries []ScheduleEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	env := authority.LaunchdEnvironment{UserHome: homeUserHome(), UID: os.Getuid()}
	disabledCtx, disabledCancel := context.WithTimeout(ctx, 8*time.Second)
	disabled, disabledErr := authority.ExecuteLaunchdCommand(disabledCtx, "/bin/launchctl", "print-disabled", fmt.Sprintf("gui/%d", env.UID))
	disabledCancel()
	env.Run = func(ctx context.Context, name string, args ...string) (authority.LaunchdCommandResult, error) {
		if len(args) > 0 && args[0] == "print-disabled" {
			return disabled, disabledErr
		}
		return authority.ExecuteLaunchdCommand(ctx, name, args...)
	}
	for i := range entries {
		e := &entries[i]
		r := authority.ProbeLaunchdRuntime(ctx, env, e.Label)
		state := &ScheduleRuntimeState{Domain: r.Domain, OverrideState: r.OverrideState, LoadedState: r.LoadedState, EffectiveEnablement: "unknown", Readiness: "unknown", Error: r.Error}
		if r.Error == "" && r.OverrideState != "unknown" && r.LoadedState != "unknown" && e.DeclaredDisabledState != "unknown" {
			state.EffectiveEnablement = "enabled"
			state.Readiness = "unloaded"
			if r.LoadedState == "loaded" {
				state.Readiness = "loaded"
			}
			if e.Disabled || e.DisabledByExt || r.OverrideState == "disabled" {
				state.EffectiveEnablement = "disabled"
				state.Readiness = "disabled"
			}
		}
		e.Runtime = state
	}
}

// ---------------- plist JSON helpers (darwin-only) ----------------

// stringField, boolField, intField, stringArrayField, dictField, stringMapField
// are narrow accessors over a map[string]any produced by encoding/json when
// the input was plutil's JSON conversion of a plist. Every value-type check is
// panic-free; an absent or wrong-type value yields the zero value.
func stringField(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// intField accepts JSON numbers as int or float64 (encoding/json's default
// for whole numbers it cannot prove is int). We round float64 to int so the
// launchd StartCalendarInterval integers survive.
func intField(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func stringArrayField(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func dictField(m map[string]any, key string) map[string]any {
	if v, ok := m[key]; ok {
		if d, ok := v.(map[string]any); ok {
			return d
		}
	}
	return nil
}

func stringMapField(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// printJSON lives in this organ's helpers.go, mirrored byte-compatible from
// cmd/compound/main.go (the cmd-budget ratchet keeps cmd/compound a thin
// dispatch shell, so the organ carries its own copy rather than importing the
// shell).

// expandEnvPath replaces $NAME and ${NAME} references in p with values from
// env. Used to bridge a stat() that does not know about cron's env semantics;
// cron's parser expands these references itself before exec, so a literal
// stat would falsely report a missing binary. Unknown $NAME tokens are left
// untouched so the resulting detail string still tells the operator which
// var matters.
func expandEnvPath(p string, env map[string]string) string {
	if len(env) == 0 || !strings.ContainsAny(p, "$") {
		return p
	}
	out := p
	for k, v := range env {
		if k == "" {
			continue
		}
		out = strings.ReplaceAll(out, "${"+k+"}", v)
		out = strings.ReplaceAll(out, "$"+k, v)
	}
	return out
}
