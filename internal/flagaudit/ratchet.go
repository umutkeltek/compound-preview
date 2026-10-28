// Package flagaudit runs the flag-validation ratchet for the compound CLI.
//
// The defect class under census: a first-party command path that silently
// accepts an unknown flag instead of refusing it. `compound work list --json`
// printed the fixed-width table while swallowing the flag — and any other
// typo'd flag — then exited 0, so an operator asking for machine output got
// the wrong shape with success semantics and no complaint (fixed by
// internal/work.ValidateWorkListFlags). The generalization is behavioral, not
// per-command: enumerate every command path the CLI runtime registry exposes
// (cmd/compound/command_registry.go is the enumeration seam), run each one in
// a hermetic subprocess with one obviously-unknown flag appended, and record
// whether the path still completes successfully.
//
// A path silently accepts the flag when BOTH loud signals are absent: it
// exits 0 (success semantics for scripts and operators) AND its combined
// output never mentions the probe flag. Either signal alone counts as
// refusing: a non-zero exit refuses the invocation (this codebase's
// convention is exit 2 for usage errors, exit 1 for runtime errors), and a
// path that names the unknown flag while still exiting 0 has a wrong-exit
// defect but did not swallow the flag silently. The paths that silently
// accept form the census, and EnforceCeiling pins that census as a ratchet
// ceiling: it may only shrink.
package flagaudit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/home"
)

// DefaultProbeFlag is the unknown flag every census run appends to each
// probed argv. Deliberately namespaced so it cannot collide with a real flag,
// and deliberately not --help-shaped so hasUsageFlag-style helpers cannot
// short-circuit it into a help path that never reaches the handler.
const DefaultProbeFlag = "--flag-ratchet-unknown-probe"

// Child contract: the cmd/compound child test (flag_validation_ratchet_
// test.go) is selected by ChildTestPattern when the parent census run re-execs
// the test binary; it decodes ChildEnvArgs (base64 JSON argv) and calls
// main(), so a probe travels the REAL dispatch path — global-arg preparation,
// registry lookup, handler Run — exactly as an operator's shell would.
const (
	ChildTestPattern = "^TestFlagValidationRatchetChild$"
	ChildEnvActivate = "COMPOUND_TEST_FLAG_AUDIT_CHILD"
	ChildEnvArgs     = "COMPOUND_TEST_FLAG_AUDIT_ARGS"
)

// Probe is one first-party command path to interrogate. Args is the argv the
// binary should see after its own name, WITHOUT the probe flag — RunCensus
// appends it. Name is the canonical census key, e.g. "work" or "route
// promotion-apply".
type Probe struct {
	Name string
	Args []string
}

// Options tunes a census run; zero values fall back to the defaults.
type Options struct {
	ProbeFlag string        // default DefaultProbeFlag
	Timeout   time.Duration // per-probe subprocess budget, default 15s
	Parallel  int           // concurrent probes, default 8
	Now       time.Time     // timestamp for fresh per-probe homes, default now
}

func (o Options) probeFlag() string {
	if o.ProbeFlag != "" {
		return o.ProbeFlag
	}
	return DefaultProbeFlag
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 15 * time.Second
}

func (o Options) parallel() int {
	if o.Parallel > 0 {
		return o.Parallel
	}
	return 8
}

func (o Options) now() time.Time {
	if !o.Now.IsZero() {
		return o.Now
	}
	return time.Now().UTC()
}

// verdict is one probe's observed behavior.
type verdict struct {
	silentlyAccepts bool
	exitCode        int
	timedOut        bool
	namedFlag       bool // combined output mentions the probe flag (a loud refusal)
	probeErr        error
	output          string
}

// RunCensus probes every given command path in a hermetic subprocess and
// returns the sorted census: names of paths that silently accept the unknown
// flag. Hermeticity: each probe gets a freshly initialized COMPOUND_HOME, an
// empty HOME, an empty PATH, a private cwd, and a strict environment
// allowlist, so census membership depends only on the code under test — never
// on the operator's machine (no real home, no harness configs, no network
// credentials, no spawnable binaries). A probe that neither refuses nor
// settles (timeout) fails the run: an incomplete census cannot back a
// ceiling.
func RunCensus(t *testing.T, probes []Probe, opts Options) []string {
	t.Helper()

	root := t.TempDir()
	for _, dir := range []string{"path-empty", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("flag audit: mkdir %s: %v", dir, err)
		}
	}

	verdicts := make([]verdict, len(probes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.parallel())
	for i := range probes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			verdicts[i] = runProbe(root, probes[i], i, opts)
		}(i)
	}
	wg.Wait()

	census := make([]string, 0, len(verdicts))
	for i, v := range verdicts {
		name := probes[i].Name
		switch {
		case v.probeErr != nil:
			t.Errorf("flag audit: probe %q could not run: %v; output: %s", name, v.probeErr, v.output)
		case v.timedOut:
			t.Errorf("flag audit: probe %q never settled within %s — the census is incomplete; resolve the hang before trusting the ceiling", name, opts.timeout())
		case v.silentlyAccepts:
			census = append(census, name)
			t.Logf("flag audit: %q SILENTLY ACCEPTS %s (exit 0)", name, opts.probeFlag())
		default:
			t.Logf("flag audit: %q refuses %s (exit %d, names flag: %t)", name, opts.probeFlag(), v.exitCode, v.namedFlag)
		}
	}
	sort.Strings(census)
	return census
}

// runProbe executes one command path. Only ever called from RunCensus's
// worker goroutines, so it reports failure through the verdict instead of
// failing the test directly.
func runProbe(root string, probe Probe, index int, opts Options) verdict {
	slug := fmt.Sprintf("%03d-%s", index, probeSlug(probe.Name))
	homePath := filepath.Join(root, "homes", slug)
	if err := home.Init(homePath, "test", opts.now()); err != nil {
		return verdict{probeErr: fmt.Errorf("init probe home: %w", err)}
	}
	osHome := filepath.Join(root, "oshome", slug)
	cwd := filepath.Join(root, "cwd", slug)
	for _, dir := range []string{osHome, cwd} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return verdict{probeErr: fmt.Errorf("mkdir probe dir: %w", err)}
		}
	}

	argv := append(append([]string{}, probe.Args...), opts.probeFlag())
	raw, err := json.Marshal(argv)
	if err != nil {
		return verdict{probeErr: fmt.Errorf("marshal probe argv: %w", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+ChildTestPattern)
	cmd.Dir = cwd
	cmd.Env = []string{
		ChildEnvActivate + "=1",
		ChildEnvArgs + "=" + base64.RawStdEncoding.EncodeToString(raw),
		"COMPOUND_HOME=" + homePath,
		"HOME=" + osHome,
		"PATH=" + filepath.Join(root, "path-empty"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
	}
	out, err := cmd.CombinedOutput()
	v := verdict{
		output:    truncate(string(out), 400),
		namedFlag: strings.Contains(string(out), opts.probeFlag()),
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		v.timedOut = true
	case err == nil && !v.namedFlag:
		v.silentlyAccepts = true
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			v.exitCode = exitErr.ExitCode() // non-zero: the path refused the invocation
		} else {
			v.probeErr = err
		}
	}
	return v
}

// EnforceCeiling is the ratchet. It fails when the census grew — a new
// unvalidated command path silently accepts an unknown flag — and when it
// shrank — a path gained flag validation, so the ceiling must be lowered to
// lock the improvement in. Growth without a pinned entry is how an
// unvalidated command lands silently; a stale pin above the census is how a
// regression lands silently. Both are test failures.
func EnforceCeiling(census, pinned []string) error {
	censusSet, pinnedSet := sliceSet(census), sliceSet(pinned)
	var added, removed []string
	for name := range censusSet {
		if _, pinned := pinnedSet[name]; !pinned {
			added = append(added, name)
		}
	}
	for name := range pinnedSet {
		if _, live := censusSet[name]; !live {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	switch {
	case len(added) > 0:
		return fmt.Errorf("census GREW — new command path(s) silently accepting an unknown flag: [%s]. Every new first-party command must refuse unknown flags (pattern: internal/work.ValidateWorkListFlags wired into the work list case of cmd/compound/work_cli.go). If a path genuinely cannot validate flags yet, add it to PinnedSilentAcceptors in internal/flagaudit/ratchet.go WITH a justification comment — never let the ceiling grow silently", strings.Join(added, ", "))
	case len(removed) > 0:
		return fmt.Errorf("census SHRANK — [%s] now refuse(s) unknown flags; lower the ceiling by removing them from PinnedSilentAcceptors in internal/flagaudit/ratchet.go so the improvement is pinned and cannot regress", strings.Join(removed, ", "))
	}
	return nil
}

// PinnedSilentAcceptors is the ratchet ceiling: the census of first-party
// compound command paths that silently accept an unknown flag instead of
// refusing it, measured at the landing of the work-list flag-validation fix
// (commit "fix(work-list): validate flags, honour --json, name valid set").
// The ratchet fails when the live census grows past this list. Shrinking it
// is the intended direction of travel: fix a path's flag validation (the
// internal/work/list_flags.go pattern — canonical flag table shared by usage
// string, validator, and handler), then remove the entry here. Entries carry
// the reason they are still on the wrong side of the ratchet.
var PinnedSilentAcceptors = []string{
	// cmdVersion answers only --json/--challenge; every other flag is ignored.
	"--version", // alias row for cmdVersion
	"-v",        // alias row for cmdVersion
	"version",   // same handler: `compound version --typo` prints version, exit 0
	// cmdDecisions treats an unknown first token as the default report path.
	"decisions",
	// cmdDoctor (cmd/compound/doctor_cli.go) prints its report regardless of argv.
	"doctor",
	// cmdHome's arg loop explicitly pockets every "-*" token it does not know
	// ("command-specific boolean flags are read below") — the whole home family
	// shares that loop.
	"home",
	"home doctor",
	// "home init" no longer pinned: it validates its own args before the cmdHome loop pockets them.
	"home verify",
	"home where",
	// cmdHookInstall scans only its known flags, then installs with defaults —
	// a typo'd flag silently selects the default posture instead of stopping.
	"hook install",
	// cmdMCPTransport enters the stdio JSON-RPC loop without looking at argv.
	"mcp-transport",
	// route evidence-authority reports and swallows any unrecognized flag.
	"route evidence-authority",
	// "schedule doctor" no longer pinned: it validates argv through
	// validateScheduleFlags on every platform (darwin and the not-darwin
	// stub), so the dialect cannot split across platforms.
	// "store migrate" no longer pinned: it validates its own flags before migration.
}

func sliceSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}

func probeSlug(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
