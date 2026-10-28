//go:build darwin

// Compound `browser` surface: read-only projection of headless / automation
// chrome processes spawned (typically) by agent-browser binaries under
// ~/.hermes and elsewhere. Designed to surface the "stuck swiftshader
// zombie" class that we have hit in real incidents (PID 7180 / 26526
// burned 300%+ CPU combined before a forced reboot, survived 3+ days
// after their orchestration was lost).
//
// Doctrinal anchors:
//  - Read-only by default; `doctor --fix` is the only mutation and is
//    scoped to the orphan-idle swiftshader class.
//  - One row per browser INSTANCE (grouped by --user-data-dir uuid), not
//    per PID, so the doctor result is legible at the level a human acts
//    on (kill the instance, not "is PID 7180 a problem? probably no").
//
// Privacy / scope:
//  This surface is a local operator diagnostic. Every projection row carries
//  local paths (--user-data-dir, cwd) and full process command lines, which
//  is useful locally and unsafe to pipe into public audit docs, MCP
//  responses, or general agent context. Treat output as you would `ps`:
//  inspect it on the box, redact before sharing.
//
//  `browser doctor --fix` is the only mutation. It is governed at the
//  command-registry layer (FirstPartyCommandGoverned for `--fix`), scoped
//  to the orphan-idle-swiftshader class, and prompts per-target by
//  default (`--yes-each` skips the prompt).

package browser

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const browserUsage = "usage: compound browser list [--json] [--instance UUID] [--swiftshader-only]\n" +
	"       compound browser show <pid|uuid> [--json]\n" +
	"       compound browser doctor [--json] [--stale-min M] [--idle-cpu-pct PCT]\n" +
	"       compound browser doctor --fix [--dry-run] [--json] [--yes-each] scoped reap: orphan swiftshader only; per-target confirmation by default\n" +
	"\n" +
	"local operator diagnostic only — output contains local paths and process args; do not pipe into public audit / MCP / general agent context.\n" + portableBrowserUsage + "\n" + selfHealUsage

// browserFlagSets enumerates the accepted flags per subcommand. Mirrors the
// shape used by `scheduleFlagSets` (see cmd/compound/schedule_cli.go) so
// adding a new flag touches exactly two places: this table, and the consume
// site in the matching handler. Unknown flags fail closed — `compound browser
// doctor --stale-mins 30 --json` is rejected with exit 2, not silently
// treated as default.
var browserFlagSets = map[string]map[string]bool{
	"list": {
		"--json":             true,
		"--instance":         true,
		"--swiftshader-only": true,
	},
	"show": {
		"--json": true,
	},
	"doctor": {
		"--json":         true,
		"--stale-min":    true,
		"--idle-cpu-pct": true,
		"--fix":          true,
		"--dry-run":      true,
		"--yes-each":     true,
	},
}

// validateBrowserFlags fails closed on unknown flags and on too many
// positional args (the `show` verb takes exactly one — pid or uuid — and
// anything beyond that is a usage error). The unknown-flag scan is inline
// (self-contained like schedule's validateScheduleFlags) so the organ owns
// its validation end-to-end; the positional cap exists because positional
// args are not gated by the flag table.
func validateBrowserFlags(sub string, args []string, maxPos int) error {
	known, ok := browserFlagSets[sub]
	if !ok {
		return fmt.Errorf("unknown subcommand %q", sub)
	}
	positional := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			positional++
			continue
		}
		name := a
		if eq := strings.Index(a, "="); eq >= 0 {
			name = a[:eq]
		}
		if !known[name] {
			return fmt.Errorf("unknown flag %q", a)
		}
		// Value-requiring flags consume the next arg when it isn't a flag.
		// --instance and --stale-min take values; --json/--fix/--dry-run/
		// --yes-each/swiftshader-only are bare switches.
		if (name == "--instance" || name == "--stale-min" || name == "--idle-cpu-pct") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			i++
		}
	}
	if positional > maxPos {
		return fmt.Errorf("too many positional args (got %d, max %d)", positional, maxPos)
	}
	return nil
}

// browserProcess is one PID row from ps, plus the user-data-dir uuid (when
// present) and the agent-browser parent PID (when reachable via ppid walk).
type browserProcess struct {
	PID            int    `json:"pid"`
	PPID           int    `json:"ppid"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
	Command        string `json:"command"`
	Argv0          string `json:"argv0"`
	HelperKind     string `json:"helper_kind,omitempty"` // "main","gpu","renderer","other"
	UserDataUUID   string `json:"user_data_uuid,omitempty"`
	ParentABPID    int    `json:"parent_ab_pid,omitempty"`
	ParentABAlive  bool   `json:"parent_ab_alive"`
	ParentABCPU    string `json:"parent_ab_cpu,omitempty"`
}

// browserInstance is one user-data-dir group: the chrome "main" process
// plus its helpers. The doctor reports findings at the instance level.
type browserInstance struct {
	UUID          string           `json:"uuid"`
	MainPID       int              `json:"main_pid"`
	HelperPIDs    []int            `json:"helper_pids"`
	ParentABPID   int              `json:"parent_ab_pid"`
	ParentABCmd   string           `json:"parent_ab_cmd,omitempty"`
	ParentABAlive bool             `json:"parent_ab_alive"`
	ParentABCPU   string           `json:"parent_ab_cpu,omitempty"`
	Elapsed       string           `json:"elapsed"`
	StartedAt     string           `json:"started_at"`
	Swiftshader   bool             `json:"swiftshader"`
	UserDataDir   string           `json:"user_data_dir,omitempty"`
	Cwd           string           `json:"cwd,omitempty"`
	Procs         []browserProcess `json:"procs,omitempty"`
}

type browserDoctorFinding struct {
	UUID       string `json:"uuid,omitempty"`
	MainPID    int    `json:"main_pid,omitempty"`
	Code       string `json:"code"` // parent-missing, orphan-idle-swiftshader, stale-instance, non-orchestrated
	Detail     string `json:"detail"`
	ReapTarget []int  `json:"reap_target_pids,omitempty"` // only set when code expects --fix
}

type browserDoctorReport struct {
	Schema    string                 `json:"schema"` // compound/browser-doctor.v0.1
	Generated string                 `json:"generated_at"`
	StaleMin  int                    `json:"stale_minutes"`
	IdlePct   int                    `json:"idle_cpu_pct"`
	Instances int                    `json:"instances"`
	Codes     map[string]int         `json:"codes"`
	Findings  []browserDoctorFinding `json:"findings"`
	SelfHeal  *selfHealDoctorReport  `json:"self_heal,omitempty"`
}

const browserDoctorSchema = "compound/browser-doctor.v0.1"

// Run is the organ entry point behind `compound browser` (the cmd-budget
// ratchet keeps cmd/compound a dispatch layer; the surface lives here).
func Run(args []string) {
	if handled, code := runSelfHealCLI(args, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if handled, err := runPortableBrowser(args, os.Stdout); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, browserUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		cmdBrowserList(args[1:])
	case "show":
		cmdBrowserShow(args[1:])
	case "doctor":
		cmdBrowserDoctor(args[1:])
	case "credential-provider":
		os.Exit(runCredentialProvider(args[1:], os.Stdin, os.Stdout, os.Stderr))
	case "credential":
		os.Exit(runCredentialCLI(args[1:], os.Stdout, os.Stderr))
	default:
		fmt.Fprintln(os.Stderr, browserUsage)
		os.Exit(2)
	}
}

func cmdBrowserList(args []string) {
	if err := validateBrowserFlags("list", args, 0); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, browserUsage)
		os.Exit(2)
	}
	jsonOut := hasFlag(args, "--json")
	swiftOnly := hasFlag(args, "--swiftshader-only")
	instFilter := strings.TrimSpace(valueFlag(args, "--instance"))

	instances, err := collectBrowserInstances()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	instances = filterInstances(instances, instFilter, swiftOnly)
	if jsonOut {
		out := struct {
			Schema    string            `json:"schema"`
			Generated string            `json:"generated_at"`
			Instances []browserInstance `json:"instances"`
			RawProcs  []browserProcess  `json:"raw_procs"`
		}{
			Schema:    "compound/browser-list.v0.1",
			Generated: time.Now().UTC().Format(time.RFC3339Nano),
			RawProcs:  allProcs(instances),
		}
		for _, inst := range instances {
			out.Instances = append(out.Instances, inst)
		}
		printJSONBrowser(out)
		return
	}
	if len(instances) == 0 {
		fmt.Println("# no browser instances found")
		return
	}
	fmt.Printf("# browser list (%d instances)\n", len(instances))
	for _, inst := range instances {
		printBrowserInstance(inst)
	}
}

func cmdBrowserShow(args []string) {
	if err := validateBrowserFlags("show", args, 1); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, browserUsage)
		os.Exit(2)
	}
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
		fmt.Fprintln(os.Stderr, "usage: compound browser show <pid|uuid> [--json]")
		os.Exit(2)
	}
	instances, err := collectBrowserInstances()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if pid, err := strconv.Atoi(target); err == nil {
		for _, inst := range instances {
			if inst.MainPID == pid {
				browserShowInstance(inst, jsonOut)
				return
			}
			for _, hp := range inst.HelperPIDs {
				if hp == pid {
					browserShowInstance(inst, jsonOut)
					return
				}
			}
		}
		fmt.Fprintf(os.Stderr, "browser show: no proc/pid %d in any instance\n", pid)
		os.Exit(1)
	}
	for _, inst := range instances {
		if inst.UUID == target {
			browserShowInstance(inst, jsonOut)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "browser show: no instance with uuid %s\n", target)
	os.Exit(1)
}

func cmdBrowserDoctor(args []string) {
	if err := validateBrowserFlags("doctor", args, 0); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, browserUsage)
		os.Exit(2)
	}
	jsonOut := hasFlag(args, "--json")
	staleMin := 60
	if v := strings.TrimSpace(valueFlag(args, "--stale-min")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fmt.Fprintln(os.Stderr, "error: --stale-min must be a non-negative integer")
			os.Exit(2)
		}
		staleMin = n
	}
	idlePct := 5
	if v := strings.TrimSpace(valueFlag(args, "--idle-cpu-pct")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			fmt.Fprintln(os.Stderr, "error: --idle-cpu-pct must be 0..100")
			os.Exit(2)
		}
		idlePct = n
	}
	instances, err := collectBrowserInstances()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	report := browserDoctorReport{
		Schema:    browserDoctorSchema,
		Generated: time.Now().UTC().Format(time.RFC3339Nano),
		StaleMin:  staleMin,
		IdlePct:   idlePct,
		Instances: len(instances),
		Codes:     map[string]int{},
	}
	for _, inst := range instances {
		addInstanceFindings(&report, inst)
	}
	for _, f := range report.Findings {
		report.Codes[f.Code]++
	}
	fix := hasFlag(args, "--fix")
	dryRun := hasFlag(args, "--dry-run")
	yesEach := hasFlag(args, "--yes-each")
	if fix {
		cmdBrowserDoctorFix(report, dryRun, yesEach, jsonOut)
		return
	}
	report.SelfHeal = selfHealDoctorSection()
	if jsonOut {
		printJSONBrowser(report)
		return
	}
	printBrowserDoctorText(report)
	renderSelfHealDoctorText(os.Stdout, *report.SelfHeal)
}

// cmdBrowserDoctorFix reaps ONLY the orphan-idle-swiftshader findings. Per
// the B-scope envelope, no plists, no PIDs not flagged in the report, no
// helpers that belong to a parent that is alive and non-idle. dryRun lists
// targets without killing. yesEach skips per-target confirmation; without
// it the command prompts before each reap.
func cmdBrowserDoctorFix(report browserDoctorReport, dryRun, yesEach, jsonOut bool) {
	var targets []browserDoctorFinding
	for _, f := range report.Findings {
		if f.Code == "orphan-idle-swiftshader" {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		fmt.Println("# browser doctor --fix: no reap targets")
		return
	}
	type reapResult struct {
		UUID   string `json:"uuid"`
		PID    int    `json:"pid"`
		Action string `json:"action"` // "would-kill" | "skipped" | "killed" | "kill-failed"
		Error  string `json:"error,omitempty"`
	}
	results := []reapResult{}
	for _, t := range targets {
		for _, pid := range t.ReapTarget {
			r := reapResult{UUID: t.UUID, PID: pid}
			if dryRun {
				r.Action = "would-kill"
				results = append(results, r)
				continue
			}
			if !yesEach {
				fmt.Printf("reap pid %d (uuid=%s)? [y/N] ", pid, t.UUID)
				var resp string
				_, _ = fmt.Scanln(&resp)
				if strings.ToLower(strings.TrimSpace(resp)) != "y" {
					r.Action = "skipped"
					results = append(results, r)
					continue
				}
			}
			if err := killProcess(pid); err != nil {
				r.Action = "kill-failed"
				r.Error = err.Error()
				results = append(results, r)
				continue
			}
			r.Action = "killed"
			results = append(results, r)
		}
	}
	if jsonOut {
		printJSONBrowser(struct {
			Schema  string       `json:"schema"`
			DryRun  bool         `json:"dry_run"`
			Results []reapResult `json:"results"`
		}{Schema: "compound/browser-doctor-fix.v0.1", DryRun: dryRun, Results: results})
		return
	}
	fmt.Printf("# browser doctor --fix (dry_run=%v)\n", dryRun)
	for _, r := range results {
		fmt.Printf("- %s uuid=%s pid=%d %s\n", r.Action, r.UUID, r.PID, r.Error)
	}
}

func addInstanceFindings(report *browserDoctorReport, inst browserInstance) {
	// Always emit per-instance metadata even if no finding fires (operators
	// want to see chrome instances when scanning), so we don't suppress here.
	// 1) parent missing
	if inst.ParentABPID != 0 && !inst.ParentABAlive {
		report.Findings = append(report.Findings, browserDoctorFinding{
			UUID:    inst.UUID,
			MainPID: inst.MainPID,
			Code:    "parent-missing",
			Detail:  fmt.Sprintf("agent-browser parent pid %d is gone; instance %s has no orchestrator", inst.ParentABPID, inst.UUID),
		})
	}
	// 2) non-orchestrated: chrome with no agent-browser ancestor at all
	if inst.ParentABPID == 0 {
		report.Findings = append(report.Findings, browserDoctorFinding{
			UUID:    inst.UUID,
			MainPID: inst.MainPID,
			Code:    "non-orchestrated",
			Detail:  fmt.Sprintf("no agent-browser ancestor found in ppid chain (main pid %d)", inst.MainPID),
		})
	}
	// 3) orphan-idle-swiftshader: parent is idle (CPU < threshold) AND swiftshader on
	// AND no live descendants beyond the instance helpers. Reap target = the
	// full instance (main + helpers).
	if inst.ParentABAlive && inst.Swiftshader {
		cpuPct, _ := strconv.ParseFloat(strings.TrimSuffix(inst.ParentABCPU, "%"), 64)
		if cpuPct < float64(report.IdlePct) {
			target := []int{inst.MainPID}
			target = append(target, inst.HelperPIDs...)
			report.Findings = append(report.Findings, browserDoctorFinding{
				UUID:       inst.UUID,
				MainPID:    inst.MainPID,
				Code:       "orphan-idle-swiftshader",
				Detail:     fmt.Sprintf("agent-browser pid %d at %s CPU; swiftshader on; reap target [%s]", inst.ParentABPID, inst.ParentABCPU, strings.Trim(fmt.Sprint(target), "[]")),
				ReapTarget: target,
			})
		}
	}
	// 4) stale-instance: instance has been up >= staleMin minutes regardless of state.
	ageSeconds, err := parseElapsed(inst.Elapsed)
	if err == nil {
		if report.StaleMin > 0 && ageSeconds >= int64(report.StaleMin)*60 {
			report.Findings = append(report.Findings, browserDoctorFinding{
				UUID:    inst.UUID,
				MainPID: inst.MainPID,
				Code:    "stale-instance",
				Detail:  fmt.Sprintf("instance age %s exceeds %d min", inst.Elapsed, report.StaleMin),
			})
		}
	}
}

// collectBrowserInstances pulls all chrome-family processes from `ps`,
// groups them by --user-data-dir uuid, enriches each instance with parent
// agent-browser state (walking the ppid chain), and returns sorted by uuid.
func collectBrowserInstances() ([]browserInstance, error) {
	psRows, err := runPS()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	abPIDs := map[int]bool{}
	for _, r := range psRows {
		if strings.Contains(r.Command, "agent-browser") {
			abPIDs[r.PID] = true
		}
	}
	procMap := map[int]browserProcess{}
	for _, r := range psRows {
		procMap[r.PID] = r
	}
	// Gather chrome rows + attach agent-browser parent via ppid walk.
	var chrome []browserProcess
	for _, r := range psRows {
		if !strings.Contains(r.Command, "Chrome") && !strings.Contains(r.Command, "chrome") {
			continue
		}
		if strings.Contains(r.Command, "codeql") {
			continue
		}
		if isChromeShape(r) {
			r.HelperKind = classifyHelper(r.Command)
			r.UserDataUUID = extractUserDataUUID(r.Command)
			r.ParentABPID, r.ParentABAlive, r.ParentABCPU = walkToAgentBrowser(r.PPID, procMap, abPIDs)
			chrome = append(chrome, r)
		}
	}
	// Group by uuid (instances whose --user-data-dir was not set use a
	// synthesized "no-uuid:<main-pid>" bucket so they never collide).
	groups := map[string][]browserProcess{}
	order := []string{}
	for _, p := range chrome {
		key := p.UserDataUUID
		if key == "" {
			key = fmt.Sprintf("no-uuid:%d", pickMainPIDForOrphan(chrome, p))
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], p)
	}
	now := time.Now()
	instances := make([]browserInstance, 0, len(order))
	for _, key := range order {
		members := groups[key]
		main := pickMainProcess(members)
		elapsedSec := int64(0)
		if main.ElapsedSeconds > 0 {
			elapsedSec = main.ElapsedSeconds
		} else {
			for _, m := range members {
				if m.ElapsedSeconds > elapsedSec {
					elapsedSec = m.ElapsedSeconds
				}
			}
		}
		inst := browserInstance{
			UUID:          key,
			MainPID:       main.PID,
			Swiftshader:   anySwiftshader(members),
			Elapsed:       formatElapsed(elapsedSec),
			StartedAt:     now.Add(-time.Duration(elapsedSec) * time.Second).UTC().Format(time.RFC3339Nano),
			UserDataDir:   extractUserDataDir(main.Command),
			ParentABPID:   main.ParentABPID,
			ParentABAlive: main.ParentABAlive,
			ParentABCPU:   main.ParentABCPU,
			ParentABCmd:   parentCmdName(procMap, main.ParentABPID),
			Procs:         members,
		}
		for _, m := range members {
			if m.PID != main.PID {
				inst.HelperPIDs = append(inst.HelperPIDs, m.PID)
			}
		}
		sort.Ints(inst.HelperPIDs)
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].UUID < instances[j].UUID })
	return instances, nil
}

// runPS captures the fields we need without depending on column ordering.
func runPS() ([]browserProcess, error) {
	cmd := exec.Command("ps", "-axo", "pid=,ppid=,etime=,command=")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var rows []browserProcess
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		pidStr, rest, ok := cutColumns(line)
		if !ok {
			continue
		}
		ppidStr, rest, ok := cutColumns(rest)
		if !ok {
			continue
		}
		etimeStr, command, ok := cutColumns(rest)
		if !ok {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(pidStr))
		ppid, _ := strconv.Atoi(strings.TrimSpace(ppidStr))
		rows = append(rows, browserProcess{
			PID:            pid,
			PPID:           ppid,
			ElapsedSeconds: parseEtime(strings.TrimSpace(etimeStr)),
			Command:        command,
			Argv0:          argv0(command),
		})
	}
	return rows, scanner.Err()
}

// cutColumns splits off the next whitespace-delimited token from a line and
// returns (token, rest, ok). Empty lines and lines starting with whitespace
// (ps header-less output trims leading whitespace) return ok=false.
func cutColumns(s string) (string, string, bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", false
	}
	fields := strings.SplitN(s, " ", 2)
	if len(fields) == 1 {
		return fields[0], "", true
	}
	return fields[0], fields[1], true
}

// parseEtime handles [[DD-]HH:]MM:SS or just MM:SS forms. Values of unknown
// shape (e.g., "-") return 0 and we rely on parent-elapsed as a fallback.
// Day-prefixed forms (DD-HH:MM:SS) are matched BEFORE the colon-split
// dispatch because they contain colons internally.
func parseEtime(s string) int64 {
	if s == "" || s == "-" {
		return 0
	}
	// Negative elapseds are not something ps emits; treat them as 0 rather
	// than guessing, so callers fall back to parent-elapsed heuristics.
	if strings.HasPrefix(s, "-") {
		return 0
	}
	if dash := strings.Index(s, "-"); dash > 0 {
		if d, err := strconv.Atoi(s[:dash]); err == nil {
			secs := int64(d) * 86400
			secs += parseEtime(s[dash+1:])
			return secs
		}
	}
	parts := strings.Split(s, ":")
	var secs int64
	switch len(parts) {
	case 2:
		m, _ := strconv.Atoi(parts[0])
		sec, _ := strconv.Atoi(parts[1])
		secs = int64(m*60 + sec)
	case 3:
		h, _ := strconv.Atoi(parts[0])
		m, _ := strconv.Atoi(parts[1])
		sec, _ := strconv.Atoi(parts[2])
		secs = int64(h*3600 + m*60 + sec)
	}
	return secs
}

// formatElapsed turns seconds back into a compact human form (upper bound
// 99d). We deliberately do NOT use time.Duration.String because that yields
// "72h45m3s" which the projection renders elsewhere; here we want "3d0h45m".
func formatElapsed(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	days := secs / 86400
	rem := secs % 86400
	hours := rem / 3600
	rem = rem % 3600
	minutes := rem / 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh%dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm%ds", minutes, secs%60)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

func parseElapsed(s string) (int64, error) {
	// Accepts forms produced by formatElapsed: "Xd", "XdYhZm", "YhZm", "Yh", "XmYs".
	if s == "" {
		return 0, errors.New("empty elapsed")
	}
	var days, hours, minutes int
	if d := strings.Index(s, "d"); d > 0 {
		v, err := strconv.Atoi(s[:d])
		if err != nil {
			return 0, err
		}
		days = v
		s = s[d+1:]
	}
	if h := strings.Index(s, "h"); h > 0 {
		v, err := strconv.Atoi(s[:h])
		if err != nil {
			return 0, err
		}
		hours = v
		s = s[h+1:]
	}
	if m := strings.Index(s, "m"); m > 0 {
		v, err := strconv.Atoi(s[:m])
		if err != nil {
			return 0, err
		}
		minutes = v
		s = s[m+1:]
	}
	if s != "" && s != "s" {
		return 0, errors.New("unrecognized elapsed form: " + s)
	}
	return int64(days)*86400 + int64(hours)*3600 + int64(minutes)*60, nil
}

func argv0(cmd string) string {
	// ps output collapses argv whitespace ambiguously; we keep the full
	// command string and let downstream helpers slice it.
	fields := strings.SplitN(cmd, " ", 2)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

var (
	chromeMainRe   = regexp.MustCompile(`MacOS/Google Chrome for Testing`)
	helperMarkerRe = regexp.MustCompile(`--type=(gpu-process|renderer|utility|service|network)`)
	userDataRe     = regexp.MustCompile(`--user-data-dir=[^ ]+`)
	abPathRe       = regexp.MustCompile(`agent-browser(?:-[a-z0-9-]+)?`)
)

// isChromeShape filters to processes that look like a headless Chrome under
// Google Chrome for Testing, which is the agent-browser default. Personal
// Chrome (Google Chrome.app/Contents/MacOS/Google Chrome) is intentionally
// not flagged.
func isChromeShape(p browserProcess) bool {
	if strings.Contains(p.Command, "Google Chrome for Testing") {
		return true
	}
	if strings.Contains(p.Command, "AgentBrowser") || strings.Contains(p.Command, "Puppeteer") || strings.Contains(p.Command, "playwright") {
		return true
	}
	return false
}

func classifyHelper(cmd string) string {
	m := helperMarkerRe.FindStringSubmatch(cmd)
	if len(m) >= 2 {
		return m[1]
	}
	if strings.Contains(cmd, "Google Chrome for Testing Helper (") {
		// No --type flag (rare for older agent-browser builds) — heuristic by
		// the parenthesized suffix.
		if strings.Contains(cmd, "(GPU)") {
			return "gpu"
		}
		if strings.Contains(cmd, "(Renderer)") {
			return "renderer"
		}
	}
	if chromeMainRe.MatchString(cmd) {
		return "main"
	}
	return "other"
}

func anySwiftshader(members []browserProcess) bool {
	for _, m := range members {
		if strings.Contains(m.Command, "--enable-unsafe-swiftshader") {
			return true
		}
	}
	return false
}

func extractUserDataDir(cmd string) string {
	m := userDataRe.FindString(cmd)
	if m == "" {
		return ""
	}
	return strings.TrimPrefix(m, "--user-data-dir=")
}

func extractUserDataUUID(cmd string) string {
	dir := extractUserDataDir(cmd)
	if dir == "" {
		return ""
	}
	base := dir
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if strings.HasPrefix(base, "agent-browser-chrome-") {
		return strings.TrimPrefix(base, "agent-browser-chrome-")
	}
	return base
}

// pickMainProcess prefers a process whose --type is missing (the chrome
// "main" for the user-data-dir). Falls back to the lowest PID in the
// members list so the choice is deterministic.
func pickMainProcess(members []browserProcess) browserProcess {
	for _, m := range members {
		if m.HelperKind == "main" || m.HelperKind == "" {
			return m
		}
	}
	min := members[0]
	for _, m := range members[1:] {
		if m.PID < min.PID {
			min = m
		}
	}
	return min
}

// pickMainPIDForOrphan picks the lowest-pid member of the group so an
// orphan process without a uuid gets a stable group key.
func pickMainPIDForOrphan(all []browserProcess, target browserProcess) int {
	min := target.PID
	for _, m := range all {
		if strings.Contains(m.Command, extractUserDataDir(target.Command)) || m.PID == target.PID {
			if m.PID < min {
				min = m.PID
			}
		}
	}
	return min
}

// walkToAgentBrowser climbs the ppid chain looking for a process whose
// argv0 contains "agent-browser". Returns (pid, alive, cpu%) for the
// found ancestor; pid=0 if no agent-browser ancestor is reachable.
func walkToAgentBrowser(startPPID int, procMap map[int]browserProcess, abPIDs map[int]bool) (int, bool, string) {
	guard := 0
	cur := startPPID
	lastAB := 0
	abCPU := ""
	for cur > 1 && guard < 32 {
		guard++
		p, ok := procMap[cur]
		if !ok {
			if lastAB != 0 {
				return lastAB, false, ""
			}
			return 0, false, ""
		}
		if abPIDs[p.PID] || abPathRe.MatchString(p.Command) {
			lastAB = p.PID
			cpu, err := readProcessCPU(p.PID)
			if err != nil {
				abCPU = "?"
			} else {
				abCPU = cpu
			}
		}
		cur = p.PPID
	}
	if lastAB == 0 {
		return 0, false, ""
	}
	alive := false
	if _, ok := procMap[lastAB]; ok {
		alive = true
	}
	return lastAB, alive, abCPU
}

func parentCmdName(procMap map[int]browserProcess, ppid int) string {
	if ppid == 0 {
		return ""
	}
	p, ok := procMap[ppid]
	if !ok {
		return ""
	}
	return p.Argv0
}

func readProcessCPU(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pcpu=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)) + "%", nil
}

func killProcess(pid int) error {
	cmd := exec.Command("kill", "-TERM", strconv.Itoa(pid))
	return cmd.Run()
}

func filterInstances(in []browserInstance, uuidFilter string, swiftOnly bool) []browserInstance {
	var out []browserInstance
	for _, inst := range in {
		if uuidFilter != "" && inst.UUID != uuidFilter {
			continue
		}
		if swiftOnly && !inst.Swiftshader {
			continue
		}
		out = append(out, inst)
	}
	return out
}

func allProcs(insts []browserInstance) []browserProcess {
	var out []browserProcess
	for _, inst := range insts {
		out = append(out, inst.Procs...)
	}
	return out
}

func printBrowserInstance(inst browserInstance) {
	state := "ok"
	switch {
	case !inst.ParentABAlive && inst.ParentABPID != 0:
		state = "parent-gone"
	case inst.ParentABPID == 0:
		state = "no-orchestrator"
	case inst.Swiftshader:
		state = "swiftshader"
	}
	fmt.Printf("- uuid=%s  main_pid=%d  age=%s  state=%s\n", inst.UUID, inst.MainPID, inst.Elapsed, state)
	if inst.UserDataDir != "" {
		fmt.Printf("    user_data_dir=%s\n", inst.UserDataDir)
	}
	if inst.Cwd != "" {
		fmt.Printf("    cwd=%s\n", inst.Cwd)
	}
	if inst.ParentABPID != 0 {
		alive := "alive"
		if !inst.ParentABAlive {
			alive = "gone"
		}
		fmt.Printf("    parent_ab=%d (%s, %s, %s)\n", inst.ParentABPID, inst.ParentABCmd, alive, inst.ParentABCPU)
	}
	if len(inst.HelperPIDs) > 0 {
		fmt.Printf("    helpers=%v\n", inst.HelperPIDs)
	}
}

func browserShowInstance(inst browserInstance, jsonOut bool) {
	if jsonOut {
		printJSONBrowser(struct {
			Schema    string          `json:"schema"`
			Generated string          `json:"generated_at"`
			Instance  browserInstance `json:"instance"`
		}{Schema: "compound/browser-show.v0.1", Generated: time.Now().UTC().Format(time.RFC3339Nano), Instance: inst})
		return
	}
	fmt.Printf("# browser show uuid=%s\n", inst.UUID)
	fmt.Printf("main_pid:    %d\n", inst.MainPID)
	fmt.Printf("elapsed:     %s (started %s)\n", inst.Elapsed, inst.StartedAt)
	fmt.Printf("swiftshader: %v\n", inst.Swiftshader)
	if inst.UserDataDir != "" {
		fmt.Printf("user_data:   %s\n", inst.UserDataDir)
	}
	if inst.Cwd != "" {
		fmt.Printf("cwd:         %s\n", inst.Cwd)
	}
	if inst.ParentABPID != 0 {
		fmt.Printf("parent_ab:   %d (%s, alive=%v, cpu=%s)\n", inst.ParentABPID, inst.ParentABCmd, inst.ParentABAlive, inst.ParentABCPU)
	}
	fmt.Printf("helpers:     %d\n", len(inst.HelperPIDs))
	for _, p := range inst.Procs {
		fmt.Printf("  pid=%d ppid=%d kind=%s elapsed=%s\n", p.PID, p.PPID, p.HelperKind, formatElapsed(p.ElapsedSeconds))
	}
}

func printBrowserDoctorText(report browserDoctorReport) {
	fmt.Printf("# browser doctor (%s)\n", report.Schema)
	fmt.Printf("# generated=%s instances=%d idle_pct=%d stale_min=%d findings=%d\n",
		report.Generated, report.Instances, report.IdlePct, report.StaleMin, len(report.Findings))
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
			target := ""
			if len(f.ReapTarget) > 0 {
				target = fmt.Sprintf(" reap=[%s]", strings.Trim(fmt.Sprint(f.ReapTarget), "[]"))
			}
			fmt.Printf("- uuid=%s pid=%d%s %s\n", f.UUID, f.MainPID, target, f.Detail)
		}
	}
}

func printJSONBrowser(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
