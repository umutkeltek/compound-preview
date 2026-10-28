// Package lanes refreshes per-harness binding state during
// `./install.sh` and `compound doctor`. R3B 2026-09-04 closes the gap
// where the installer's "fresh" probe ran only `instruction-pack`
// (which writes latest.json) and then declared the lane fresh, while
// the operator's later `instruction-pack --drift` run on the SAME
// harness returned `status: drift` — because (a) the installer's loop
// used the alias form (`claude-minimax`) while the registry and the
// pack's on-disk path use the canonical id (`claude_minimax`), so the
// canonical path was never touched and stayed drifted; and (b) the
// installer's probe never ran `--drift` itself.
//
// The fix is a single owner for the verdict: RefreshLane runs the
// full ladder (hook install → contract compile if a canonical cwd/task
// is recorded → instruction-pack render → `--drift`) and returns a
// LaneVerdict that install.sh, doctor, and any other caller render
// from. The CLI verb `compound harness refresh-lane` is a thin shell
// over RefreshLane so the doctor's binding-freshness section and the
// installer's per-lane line print the SAME verdict for the SAME state.
//
// One owner, one verdict line, no aliasing, no probe-then-divergence.
package lanes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"compound/internal/contract"
	"compound/internal/harness"
	"compound/internal/hookplan"
)

// refreshLaneHookInstall re-runs the per-harness hook install against
// opts.BaseDir / opts.BinaryPath and publishes the install report back
// into the installed-hooks manifest. The install.sh-level `hook install
// --harness all` already updated the manifest once; this per-lane call
// is idempotent — its only purpose is to make sure a manifest entry for
// this exact harness is present and points at opts.BinaryPath, so the
// harness's `binary:<path>` revision in the manifest matches what
// contract compute binding freshness will compare against.
//
// `publish` is the SAME closure `compound hook install` uses:
// `contract.ImportHookInstallReport(homePath, base, report, now)`. A
// nil publish (e.g. from a check-only invocation) would leave the
// manifest entry stale — the very bug R3B fixes — so this function
// always supplies the publish callback for non-check installs.
func refreshLaneHookInstall(opts RefreshLaneOptions, harnessName string) error {
	publish := func(report hookplan.HookInstallReport) error {
		_, err := contract.ImportHookInstallReport(opts.HomePath, opts.BaseDir, report, opts.Now)
		return err
	}
	_, err := hookplan.InstallHooksWithRoster(
		opts.BaseDir,
		opts.BinaryPath,
		"global",
		[]string{hookInstallHarness(harnessName)},
		"",
		nil,
		false,
		publish,
	)
	return err
}

// refreshLaneContractCompile compiles the effective contract for the
// lane's canonical (cwd, task), seals the runtime, and records the
// materialization so contracts/effective/latest.json exists on disk
// before the pack renders. Without this step the instruction pack's
// effective_contract layer renders Absent and any drift check that
// needs the contract layer to land clean will report drift.
//
// The orientation policy is the home-resolved one (loadOrientationPolicy
// semantics) — `contract.LoadPolicy(homePath)` returns the built-in
// default when the home has no orientation.yaml, which is the right
// behavior for an installer whose operator has not customized it.
func refreshLaneContractCompile(opts RefreshLaneOptions, harnessName string, defaultLane LaneDefault) error {
	pol, _, err := contract.LoadPolicy(opts.HomePath)
	if err != nil {
		return fmt.Errorf("load orientation policy: %w", err)
	}
	in := contract.Input{
		Harness: harnessName,
		Cwd:     defaultLane.Cwd,
		Task:    defaultLane.Task,
	}
	in, err = contract.ResolveOrientationIdentity(in, harness.DefaultRegistry())
	if err != nil {
		return fmt.Errorf("resolve orientation identity: %w", err)
	}
	in.RepoKind, in.RepoKindVia, err = contract.ResolveRepoKindWithHome(opts.HomePath, "", in.Cwd)
	if err != nil {
		return fmt.Errorf("resolve repository kind: %w", err)
	}
	runtime, err := contract.MaterializeRuntime(in, pol, opts.HomePath, opts.Now)
	if err != nil {
		return fmt.Errorf("materialize runtime: %w", err)
	}
	sealed, err := contract.SealRuntimeMaterialization(runtime)
	if err != nil {
		return fmt.Errorf("seal runtime materialization: %w", err)
	}
	if _, err := contract.RecordRuntimeMaterialization(opts.HomePath, sealed); err != nil {
		return fmt.Errorf("record runtime materialization: %w", err)
	}
	if _, err := contract.RenderInstructionPackForContext(opts.HomePath, harnessName, "", sealed.ContextPack.ID, opts.Now); err != nil {
		return fmt.Errorf("bind instruction-pack context: %w", err)
	}
	return nil
}

// Status vocabulary. The drift side mirrors InstructionPackDriftReport
// (`clean` / `drift`) so a verdict line prints the SAME word the
// operator would see running `instruction-pack --drift` directly.
// `skipped` is the R3B-owned state for "no canonical cwd/task
// recorded; the lane cannot be re-compiled" and is loud on purpose —
// the operator must seed config/lane-defaults.yaml or run contract
// compile by hand for that lane.
const (
	StatusClean   = "clean"
	StatusDrifted = "drifted"
	StatusSkipped = "skipped"
	StatusError   = "error"
)

// LaneVerdict is the single owner of "did the lane ladder land clean?".
// install.sh prints one line per lane from this struct; doctor renders
// the binding-freshness section from this struct's verdict text.
// RepairCommand is non-empty only for `drifted` and is the exact shell
// command an operator can paste to recover without reading source.
type LaneVerdict struct {
	Harness       string `json:"harness"`
	Status        string `json:"status"`
	Detail        string `json:"detail,omitempty"`
	RepairCommand string `json:"repair_command,omitempty"`
}

// RefreshLaneOptions carries every input RefreshLane needs. Now is
// exposed so the test pin uses a deterministic clock (the drift
// comparator explicitly excludes GeneratedAt, but the on-disk pack's
// generated_at still gets written — a stable clock keeps the bytes
// stable for fixtures).
type RefreshLaneOptions struct {
	HomePath   string
	BinaryPath string
	BaseDir    string
	Now        time.Time
}

// LaneDefault is the canonical (cwd, task) pair a lane uses when
// contract compile runs as part of the refresh ladder. Seeded by
// `compound contract compile --harness <h> --cwd <d> --task <t>` (a
// future slice wires that seed step into the contract compile path;
// for now the operator writes config/lane-defaults.yaml by hand).
type LaneDefault struct {
	Harness string `yaml:"harness" json:"harness"`
	Cwd     string `yaml:"cwd" json:"cwd"`
	Task    string `yaml:"task" json:"task"`
}

// laneDefaultsFile is the on-disk YAML one record per harness, only
// the (cwd, task) pair used by RefreshLane. Mirrors the
// installed-hooks manifest shape (a yaml with a small typed list of
// records) so the doctor can render its presence in the same breath
// as the binding freshness section.
type laneDefaultsFile struct {
	Lanes []LaneDefault `yaml:"lanes"`
}

// LaneDefaultsPath is the on-disk home-relative file RefreshLane
// reads to decide whether a lane has a canonical cwd/task recorded.
// Exported because install.sh and doctor both reference it in their
// repair hints.
func LaneDefaultsPath(homePath string) string {
	return filepath.Join(homePath, "config", "lane-defaults.yaml")
}

// hookInstallHarness translates a canonical registry id (the form the
// manifest, the instruction-pack paths, and RefreshLanes callers use)
// into the alias form `compound hook install` and its hookplan
// internals expect. The two forms coexist on purpose — the registry
// uses underscores for consistency with go package ids, the native
// hook config files use dashes — but a refresh pass that takes one
// form MUST hand the other to the hook installer or the install
// report comes back empty and the manifest stays drifted.
func hookInstallHarness(canonical string) string {
	switch strings.TrimSpace(canonical) {
	case "claude_minimax":
		return "claude-minimax"
	case "claude_zai":
		return "claude-zai"
	case "claude_qwen":
		return "claude-qwen"
	case "claude_code":
		return "claude-code"
	case "claude_neuralwatt", "claude_kimi", "claude_gpt56_sol_wpcom":
		return canonical // no native alias; hookplan skips
	}
	return canonical
}

// InstalledHarnesses returns the harness ids in the installed-hooks
// manifest, sorted for stable iteration order (test pin + install.sh
// per-lane loop). An empty / missing manifest returns an empty slice
// without error — that is the cold-start case and the installer's
// loop should print nothing per-lane, not fail.
func InstalledHarnesses(homePath string) ([]string, error) {
	manifest, err := contract.LoadInstalledHooksManifest(homePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("load installed-hooks manifest: %w", err)
	}
	seen := map[string]struct{}{}
	for _, hook := range manifest.Hooks {
		h := strings.TrimSpace(hook.Harness)
		if h == "" {
			continue
		}
		seen[h] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out, nil
}

// LoadLaneDefault returns the canonical cwd/task for a single harness.
// A missing file or a missing lane is `("", "", nil)` so the caller
// can distinguish "not recorded" from "read error" without parsing
// error strings. The install loop uses `not recorded` to print the
// loud skip line; doctor uses it to render "(no canonical cwd/task
// recorded)" in its binding-freshness section.
func LoadLaneDefault(homePath, harnessName string) (LaneDefault, error) {
	harnessName = strings.TrimSpace(harnessName)
	path := LaneDefaultsPath(homePath)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LaneDefault{Harness: harnessName}, nil
		}
		return LaneDefault{}, fmt.Errorf("read lane defaults: %w", err)
	}
	var doc laneDefaultsFile
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return LaneDefault{}, fmt.Errorf("parse lane defaults: %w", err)
	}
	for _, lane := range doc.Lanes {
		if strings.TrimSpace(lane.Harness) == harnessName {
			return LaneDefault{
				Harness: harnessName,
				Cwd:     strings.TrimSpace(lane.Cwd),
				Task:    strings.TrimSpace(lane.Task),
			}, nil
		}
	}
	return LaneDefault{Harness: harnessName}, nil
}

// RefreshLane runs the per-lane ladder and returns the verdict.
// It is the SINGLE OWNER of the verdict text: install.sh, doctor,
// and the CLI verb all print the result of this function so the
// operator can never see `clean` and `drifted` from two different
// callers at the same instant on the same home.
//
// Order: hook install → contract compile (if recorded) → pack render
// → drift check. Each step's error collapses to a verdict with that
// status and a per-step repair command, so a failing ladder still
// produces a single printable line per lane.
func RefreshLane(opts RefreshLaneOptions, harnessName string) (LaneVerdict, error) {
	harnessName = strings.TrimSpace(harnessName)
	v := LaneVerdict{Harness: harnessName}
	if harnessName == "" {
		v.Status = StatusError
		v.Detail = "harness name is required"
		return v, errors.New("refresh lane: harness name is required")
	}
	if strings.TrimSpace(opts.HomePath) == "" {
		v.Status = StatusError
		v.Detail = "home path is required"
		return v, errors.New("refresh lane: home path is required")
	}
	if strings.TrimSpace(opts.BinaryPath) == "" {
		v.Status = StatusError
		v.Detail = "binary path is required"
		return v, errors.New("refresh lane: binary path is required")
	}
	if strings.TrimSpace(opts.BaseDir) == "" {
		v.Status = StatusError
		v.Detail = "base dir is required"
		return v, errors.New("refresh lane: base dir is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}

	if preflight := preflightLaneRefresh(opts, harnessName); preflight.Status != statusRefreshReady {
		return preflight, nil
	}

	// Step 1: hook install for THIS lane. The installer used to call
	// `hook install --harness all` once and then re-run per-lane; the
	// per-lane call here is idempotent and keeps each lane's verdict
	// line honest (a hook-install failure on one lane does not skip
	// the others).
	if err := refreshLaneHookInstall(opts, harnessName); err != nil {
		v.Status = StatusError
		v.Detail = "hook install: " + err.Error()
		v.RepairCommand = fmt.Sprintf("compound hook install --harness %s", harnessName)
		return v, nil
	}

	// Step 2: contract compile. Skipped loudly if no canonical cwd/task
	// is recorded; the operator must seed config/lane-defaults.yaml or
	// run contract compile by hand. This is the R3B "loud skip" branch.
	//
	// The named canonical context is selected explicitly. Other orientations may
	// advance their own projections without rebinding this installed consumer.
	defaultLane, err := LoadLaneDefault(opts.HomePath, harnessName)
	if err != nil {
		v.Status = StatusError
		v.Detail = "read lane defaults: " + err.Error()
		return v, nil
	}
	if defaultLane.Cwd == "" || defaultLane.Task == "" {
		v.Status = StatusSkipped
		v.Detail = "no canonical cwd/task recorded for this lane"
		v.RepairCommand = fmt.Sprintf(
			"echo 'lanes:\\n  %s:\\n    cwd: <dir>\\n    task: <task>' >> %s; compound contract compile --harness %s --cwd <dir> --task <task>",
			harnessName, LaneDefaultsPath(opts.HomePath), harnessName,
		)
		return v, nil
	}
	if err := refreshLaneContractCompile(opts, harnessName, defaultLane); err != nil {
		v.Status = StatusError
		v.Detail = "contract compile: " + err.Error()
		v.RepairCommand = fmt.Sprintf("compound contract compile --harness %s --cwd %s --task %s", harnessName, defaultLane.Cwd, defaultLane.Task)
		return v, nil
	}

	// Step 3: render the instruction pack so the on-disk
	// instruction-packs/<harness>/latest.json matches the manifest's
	// new binary revision. Without this step the next drift check
	// would still report drift on the installed_hooks layer.
	if _, err := harness.RenderInstructionPack(opts.HomePath, harnessName, "", opts.Now); err != nil {
		v.Status = StatusError
		v.Detail = "instruction-pack render: " + err.Error()
		v.RepairCommand = fmt.Sprintf("compound harness instruction-pack --harness %s", harnessName)
		return v, nil
	}

	// Step 4: drift check. The verdict of THIS call is what the
	// installer and doctor both print — single owner.
	report, err := harness.InstructionPackDrift(opts.HomePath, harnessName, "", opts.Now)
	if err != nil {
		v.Status = StatusError
		v.Detail = "drift check: " + err.Error()
		v.RepairCommand = fmt.Sprintf("compound harness instruction-pack --harness %s --drift", harnessName)
		return v, nil
	}
	if report.Drifted {
		v.Status = StatusDrifted
		v.Detail = strings.Join(report.Details, "; ")
		v.RepairCommand = fmt.Sprintf("compound harness instruction-pack --harness %s", harnessName)
		return v, nil
	}
	v.Status = StatusClean
	return v, nil
}

// CheckLaneDrift is the read-only half of RefreshLane, used by
// `compound doctor` so the installer's verdict and the doctor's
// binding-freshness section read from the SAME verdict struct.
// doctor never writes; the only step it cannot reuse from
// RefreshLane is `hook install`, so this skips it and goes straight
// to the on-disk drift check. A lane with no manifest entry (i.e.
// `installed_hooks layer absent`) renders `absent`, matching the
// pre-R3B binding-freshness vocabulary so doctor text stays
// readable.
//
// The verdict text uses the SAME `instruction-pack: <h> <status>`
// shape install.sh prints — single owner of the line.
func CheckLaneDrift(opts RefreshLaneOptions, harnessName string) (LaneVerdict, error) {
	harnessName = strings.TrimSpace(harnessName)
	v := LaneVerdict{Harness: harnessName}
	if harnessName == "" {
		v.Status = StatusError
		v.Detail = "harness name is required"
		return v, errors.New("check lane drift: harness name is required")
	}
	if strings.TrimSpace(opts.HomePath) == "" {
		v.Status = StatusError
		v.Detail = "home path is required"
		return v, errors.New("check lane drift: home path is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	manifest, err := contract.LoadInstalledHooksManifest(opts.HomePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			v.Status = StatusError
			v.Detail = "no installed hooks manifest"
			return v, nil
		}
		v.Status = StatusError
		v.Detail = "load manifest: " + err.Error()
		return v, nil
	}
	hasEntry := false
	for _, h := range manifest.Hooks {
		if strings.TrimSpace(h.Harness) == harnessName {
			hasEntry = true
			break
		}
	}
	if !hasEntry {
		v.Status = StatusError
		v.Detail = "lane has no installed-hooks manifest entry"
		v.RepairCommand = fmt.Sprintf("compound hook install --harness %s", harnessName)
		return v, nil
	}
	report, err := harness.InstructionPackDrift(opts.HomePath, harnessName, "", opts.Now)
	if err != nil {
		v.Status = StatusError
		v.Detail = "drift check: " + err.Error()
		v.RepairCommand = fmt.Sprintf("compound harness instruction-pack --harness %s --drift", harnessName)
		return v, nil
	}
	if report.Drifted {
		v.Status = StatusDrifted
		v.Detail = strings.Join(report.Details, "; ")
		v.RepairCommand = fmt.Sprintf("compound harness instruction-pack --harness %s", harnessName)
		return v, nil
	}
	v.Status = StatusClean
	return v, nil
}

// RenderVerdict writes ONE line per verdict in the install.sh /
// refresh-lane CLI shape:
//
//	"instruction-pack: <harness> <status>[: <detail>] [(run: <cmd>)]"
//
// Single owner of the line — doctor and installer call this too.
func RenderVerdict(w io.Writer, v LaneVerdict) {
	fmt.Fprintf(w, "instruction-pack: %s %s", v.Harness, v.Status)
	if v.Detail != "" {
		fmt.Fprintf(w, ": %s", v.Detail)
	}
	if v.RepairCommand != "" {
		fmt.Fprintf(w, " (run: %s)", v.RepairCommand)
	}
	fmt.Fprintln(w)
}

// RefreshAllInstalledHarnesses runs the per-lane ladder over every
// harness in the installed-hooks manifest, in sorted order, and
// returns the verdicts. This is the function `compound harness
// refresh-lane` (no --harness flag) and `compound harness
// refresh-lane --harness X` both call from cmd/compound; install.sh
// invokes the same code path through `compound harness
// refresh-lane`. Keeping the iteration body in internal/lanes means
// the per-lane loop never has to live in cmd/compound (which is
// line-budgeted).
//
// Hook installation changes a shared manifest, so it completes for all lanes
// before any pack renders. Compile and bind every recorded canonical context;
// never render all lanes against the first lane's contract.
func RefreshAllInstalledHarnesses(opts RefreshLaneOptions) ([]LaneVerdict, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	preflight, err := PreflightInstalledRefresh(opts)
	if err != nil {
		return nil, err
	}
	if !refreshPreflightReady(preflight) {
		return preflight, nil
	}
	// An externally-owned lane is carried through to the report untouched: the
	// native writer owns neither its hooks nor its pack, so refreshing it here
	// would claim an activation this code cannot perform.
	names := make([]string, 0, len(preflight))
	externalVerdicts := make(map[string]LaneVerdict, len(preflight))
	for _, v := range preflight {
		if v.Status == statusRefreshExternal {
			externalVerdicts[v.Harness] = v
			continue
		}
		names = append(names, v.Harness)
	}
	// Stabilize the shared hook manifest before rendering any pack.
	hookErrors := make(map[string]error)
	for _, name := range names {
		if err := refreshLaneHookInstall(opts, name); err != nil {
			hookErrors[name] = err
		}
	}
	out := make([]LaneVerdict, 0, len(preflight))
	for _, p := range preflight {
		if external, ok := externalVerdicts[p.Harness]; ok {
			out = append(out, external)
			continue
		}
		name := p.Harness
		if err := hookErrors[name]; err != nil {
			out = append(out, LaneVerdict{Harness: name, Status: StatusError, Detail: "hook install: " + err.Error(),
				RepairCommand: fmt.Sprintf("compound hook install --harness %s", name)})
			continue
		}
		laneDefault, err := LoadLaneDefault(opts.HomePath, name)
		if err == nil && laneDefault.Cwd != "" && laneDefault.Task != "" {
			err = refreshLaneContractCompile(opts, name, laneDefault)
		}
		if err != nil {
			out = append(out, LaneVerdict{Harness: name, Status: StatusError, Detail: "canonical context: " + err.Error()})
			continue
		}
		v, _ := RefreshLaneNoCompile(opts, name)
		out = append(out, v)
	}
	return out, nil
}

// RefreshLaneNoCompile is the per-lane render + drift check WITHOUT
// the hook-install and contract-compile steps. The caller (the
// `RefreshAllInstalledHarnesses` loop body, or a test) is
// responsible for having already run hook install + contract
// compile once before iterating lanes; running them per-lane races
// on shared state. Single-lane callers who DO want a fresh install
// + compile use RefreshLane instead.
func RefreshLaneNoCompile(opts RefreshLaneOptions, harnessName string) (LaneVerdict, error) {
	harnessName = strings.TrimSpace(harnessName)
	v := LaneVerdict{Harness: harnessName}
	if harnessName == "" {
		v.Status = StatusError
		v.Detail = "harness name is required"
		return v, errors.New("refresh lane (no compile): harness name is required")
	}
	if strings.TrimSpace(opts.HomePath) == "" {
		v.Status = StatusError
		v.Detail = "home path is required"
		return v, errors.New("refresh lane (no compile): home path is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if _, err := harness.RenderInstructionPack(opts.HomePath, harnessName, "", opts.Now); err != nil {
		v.Status = StatusError
		v.Detail = "instruction-pack render: " + err.Error()
		v.RepairCommand = fmt.Sprintf("compound harness instruction-pack --harness %s --context-pack <ctx_id>", harnessName)
		return v, nil
	}
	report, err := harness.InstructionPackDrift(opts.HomePath, harnessName, "", opts.Now)
	if err != nil {
		v.Status = StatusError
		v.Detail = "drift check: " + err.Error()
		v.RepairCommand = fmt.Sprintf("compound harness instruction-pack --harness %s --context-pack <ctx_id>", harnessName)
		return v, nil
	}
	if report.Drifted {
		v.Status = StatusDrifted
		v.Detail = strings.Join(report.Details, "; ")
		v.RepairCommand = fmt.Sprintf("compound harness instruction-pack --harness %s", harnessName)
		return v, nil
	}
	v.Status = StatusClean
	return v, nil
}

// CheckAllInstalledHarnesses is the read-only twin, used by
// `compound doctor`. It runs CheckLaneDrift per installed harness
// and returns the verdicts. Verdict text uses the same `LaneVerdict`
// struct, so doctor and install.sh share one line shape.
func CheckAllInstalledHarnesses(opts RefreshLaneOptions) ([]LaneVerdict, error) {
	names, err := InstalledHarnesses(opts.HomePath)
	if err != nil {
		return nil, err
	}
	out := make([]LaneVerdict, 0, len(names))
	for _, name := range names {
		v, _ := CheckLaneDrift(opts, name)
		out = append(out, v)
	}
	return out, nil
}

// VerdictLine is the on-disk single-line representation of a
// LaneVerdict. The install.sh install report, the refresh-lane CLI,
// and doctor all print this exact string so the operator never sees
// two different wordings for the same state. Use RenderVerdict to
// write it to an io.Writer; this helper is for callers that want
// the bytes.
func VerdictLine(v LaneVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "instruction-pack: %s %s", v.Harness, v.Status)
	if v.Detail != "" {
		fmt.Fprintf(&b, ": %s", v.Detail)
	}
	if v.RepairCommand != "" {
		fmt.Fprintf(&b, " (run: %s)", v.RepairCommand)
	}
	return b.String()
}

// CLIRefreshOpts is what `compound harness refresh-lane` parses from
// its own flag set; cmd/compound only forwards parsed values, so the
// arg-parsing logic stays in this package (cmd/compound is line
// budgeted and must not grow with this verb's plumbing).
type CLIRefreshOpts struct {
	HomePath    string
	BinaryPath  string
	BaseDir     string
	HarnessName string
	JSON        bool
	Now         time.Time
}

// RunCLIRefresh is the verb body. It returns the verdicts AND the
// formatted string the CLI should write — `Print` chooses text vs
// JSON. cmd/compound calls this from the harness refresh-lane
// dispatch (one line per verdict). Errors here are verdict-shaped,
// not fatal: a failing lane prints its error verdict and the loop
// moves on so the operator sees every lane in one pass.
func RunCLIRefresh(opts CLIRefreshOpts) ([]LaneVerdict, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	refreshOpts := RefreshLaneOptions{
		HomePath:   opts.HomePath,
		BinaryPath: opts.BinaryPath,
		BaseDir:    opts.BaseDir,
		Now:        now,
	}
	if opts.HarnessName != "" {
		v, err := RefreshLane(refreshOpts, opts.HarnessName)
		return []LaneVerdict{v}, err
	}
	return RefreshAllInstalledHarnesses(refreshOpts)
}

// ParseCLIRefreshArgs builds CLIRefreshOpts from a raw argv slice.
// Centralizes flag parsing in this package so the cmd/compound
// dispatch can stay under cmd/compound's line budget. The boolean
// returned is true when the caller should print usage and exit 0.
func ParseCLIRefreshArgs(args []string, defaultHome, defaultBinary, defaultBase string, now time.Time) (CLIRefreshOpts, bool, error) {
	// Validate before help/default resolution so malformed input cannot hide
	// behind a successful no-op or change the raw --preflight admission path.
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--home", "--binary", "--base", "--harness":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(strings.TrimSpace(args[i+1]), "-") {
				return CLIRefreshOpts{}, false, fmt.Errorf("%s requires a value", args[i])
			}
			i++
		case "--json", "--preflight", "--help", "-h":
		default:
			return CLIRefreshOpts{}, false, fmt.Errorf("unknown refresh-lane argument %q", args[i])
		}
	}
	if HasUsageFlag(args) {
		return CLIRefreshOpts{}, true, nil
	}
	homePath := strings.TrimSpace(ValueFlag(args, "--home"))
	if homePath == "" {
		homePath = defaultHome
	}
	binaryPath := strings.TrimSpace(ValueFlag(args, "--binary"))
	if binaryPath == "" {
		binaryPath = defaultBinary
	}
	baseDir := strings.TrimSpace(ValueFlag(args, "--base"))
	if baseDir == "" {
		baseDir = defaultBase
	}
	if homePath == "" || binaryPath == "" || baseDir == "" {
		return CLIRefreshOpts{}, false, errors.New("--home, --binary, --base are required when defaults are not set")
	}
	return CLIRefreshOpts{
		HomePath:    homePath,
		BinaryPath:  binaryPath,
		BaseDir:     baseDir,
		HarnessName: strings.TrimSpace(ValueFlag(args, "--harness")),
		JSON:        HasFlag(args, "--json"),
		Now:         now,
	}, false, nil
}

// refreshIncompleteVerdict picks the verdict that should fail the refresh CLI,
// or nil when none should. An externally-owned lane is never `clean`, because
// the native writer deliberately does not refresh it; counting it as incomplete
// would fail the installer's activation on a lane publication cannot affect --
// the same unsatisfiable-gate shape the preflight once carried, one exit code
// later. A `ready` verdict is weaker evidence than a real per-lane failure, so a
// later genuine failure replaces it.
func refreshIncompleteVerdict(verdicts []LaneVerdict) *LaneVerdict {
	var incomplete *LaneVerdict
	for i := range verdicts {
		verdict := &verdicts[i]
		if verdict.Status == statusRefreshExternal {
			continue
		}
		if verdict.Status != StatusClean && (incomplete == nil || incomplete.Status == statusRefreshReady) {
			incomplete = verdict
		}
	}
	return incomplete
}

// RunCLIFromArgs is the single entry point cmd/compound should use
// for `compound harness refresh-lane`. It owns the entire CLI body:
// arg parsing, home resolution, verb dispatch, exit-code mapping,
// usage printing. cmd/compound calls this in one line from the
// target dispatcher and dies on the returned error.
func RunCLIFromArgs(args []string, now time.Time) error {
	exe, _ := os.Executable()
	homePath, hErr := osUserHome()
	if hErr != nil {
		return hErr
	}
	defaultBase := "."
	if exe != "" {
		defaultBase = filepath.Dir(exe)
	}
	opts, printUsage, pErr := ParseCLIRefreshArgs(args, homePath, exe, defaultBase, now)
	if pErr != nil {
		return pErr
	}
	if printUsage {
		fmt.Fprintln(os.Stderr, "usage: compound harness refresh-lane [--preflight] [--harness <name>] [--json] [--base <dir>] [--binary <path>] [--home <dir>]")
		return nil
	}
	if HasFlag(args, "--preflight") {
		return runCLIRefreshPreflight(opts)
	}
	verdicts, refreshErr := RunCLIRefresh(opts)
	if err := PrintVerdicts(os.Stdout, verdicts, opts.JSON); err != nil {
		return err
	}
	if refreshErr != nil {
		return refreshErr
	}
	if incomplete := refreshIncompleteVerdict(verdicts); incomplete != nil {
		return fmt.Errorf("refresh incomplete for %s: %s (%s)", incomplete.Harness, incomplete.Status, incomplete.Detail)
	}
	return nil
}

// osUserHome is a small indirection so RunCLIFromArgs can be unit
// tested in environments where os.UserHomeDir returns errors.
// Honors $COMPOUND_HOME first so install.sh's temp-home proofs
// route to the temp home, not to /Users/<name>/.compound.
var osUserHome = func() (string, error) {
	if v := strings.TrimSpace(os.Getenv("COMPOUND_HOME")); v != "" {
		return v, nil
	}
	return os.UserHomeDir()
}

// RunCLICheck is the verb body for the read-only check used by
// doctor (and any future read-only caller). It returns verdicts
// produced by CheckLaneDrift per installed harness so the doctor's
// binding-freshness section reads from the same LaneVerdict struct
// as the installer's per-lane line.
func RunCLICheck(opts CLIRefreshOpts) ([]LaneVerdict, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	checkOpts := RefreshLaneOptions{
		HomePath:   opts.HomePath,
		BinaryPath: opts.BinaryPath,
		BaseDir:    opts.BaseDir,
		Now:        now,
	}
	if opts.HarnessName != "" {
		v, err := CheckLaneDrift(checkOpts, opts.HarnessName)
		return []LaneVerdict{v}, err
	}
	return CheckAllInstalledHarnesses(checkOpts)
}

// PrintVerdicts writes verdicts as JSON or text per opts.JSON.
// Keeping it here keeps cmd/compound's render path one line; the
// line shape is owned by RenderVerdict / VerdictLine above.
func PrintVerdicts(w io.Writer, verdicts []LaneVerdict, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(verdicts)
	}
	for _, v := range verdicts {
		RenderVerdict(w, v)
	}
	return nil
}

// HasUsageFlag reports whether args contains --help or -h. Mirrors
// cmd/compound.hasUsageFlag so the parsing logic stays in one place.
func HasUsageFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// HasFlag reports whether args contains a flag.
func HasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// ValueFlag returns the value following the given flag in args, or "".
func ValueFlag(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// Exit writes v to stderr as a JSON error and returns exit code 1.
func Exit(v interface{}) int {
	b, err := json.MarshalIndent(map[string]interface{}{"error": fmt.Sprintf("%v", v)}, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", v)
		return 1
	}
	fmt.Fprintln(os.Stderr, string(b))
	return 1
}

// RenderInstalledLaneDriftVerdicts writes the R3B 2026-09-04 doctor
// section that mirrors the installer's per-lane line. Implemented in
// this package so cmd/compound's call site is one line — doctor's
// binding-freshness and installer's per-lane line read from the same
// LaneVerdict struct, satisfying the "one owner" packet requirement.
// An empty / missing manifest is a no-op (no section, no error)
// because doctor must keep rendering cleanly on a cold-start home.
func RenderInstalledLaneDriftVerdicts(w io.Writer, homePath, binaryPath string, now time.Time) {
	if strings.TrimSpace(homePath) == "" {
		return
	}
	names, err := InstalledHarnesses(homePath)
	if err != nil || len(names) == 0 {
		return
	}
	fmt.Fprintln(w, "\n## lane drift verdicts (read-only mirror of installer)")
	opts := RefreshLaneOptions{HomePath: homePath, BinaryPath: binaryPath, Now: now}
	for _, n := range names {
		v, _ := CheckLaneDrift(opts, n)
		RenderVerdict(w, v)
	}
}
