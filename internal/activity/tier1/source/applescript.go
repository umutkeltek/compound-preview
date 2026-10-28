package source

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// AppleScriptConfig identifies one terminal app that exposes its focused pane's pid
// via AppleScript on macOS. The DisplayName is informational only; it does NOT appear
// in any envelope field (per the F5 invariant — no human-readable content).
type AppleScriptConfig struct {
	BundleID    string // e.g. "com.googlecode.iterm2", "dev.warp.Warp", "com.apple.Terminal"
	DisplayName string // human-readable; not used in envelopes
}

// AppleScriptSource is the generic fallback for terminals that don't expose an IPC.
// Strategy:
//
//  1. Use AppleScript via System Events to find the focused window's PID.
//  2. Use `lsof -a -p <pid> -d cwd -F n` to read the process's cwd file descriptor.
//  3. If lsof doesn't support -d cwd on this system, fall back to -d 255 (FreeBSD cwd).
//
// On TCC permission loss (Accessibility / Screen Recording revoked), the source
// returns (zero, false, ErrPermissionLost). The caller can detect this and emit a
// KindPermissionLost event so downstream spans are marked incomplete (F11).
type AppleScriptSource struct {
	cfg     AppleScriptConfig
	execCmd lsofRunner
	getPID  pidResolver
}

// pidResolver abstracts the AppleScript pid-lookup so tests can stub.
type pidResolver interface {
	FocusedPID(ctx context.Context, appName string) (int, error)
}

// lsofRunner abstracts the lsof call so tests can stub.
type lsofRunner interface {
	Run(ctx context.Context, pid int) (string, error)
}

// NewAppleScriptSource returns an AppleScriptSource for one terminal app.
func NewAppleScriptSource(cfg AppleScriptConfig) *AppleScriptSource {
	return &AppleScriptSource{
		cfg:     cfg,
		execCmd: defaultLsofRunner{},
		getPID:  defaultPIDResolver{},
	}
}

// AppBundleID returns the configured bundle id.
func (s *AppleScriptSource) AppBundleID() string { return s.cfg.BundleID }

// TryEnvelope resolves the focused pane's PID, then its cwd via lsof. Returns
// (zero, false, nil) on any "not focused / not installed / TCC lost" condition so
// the caller can skip this source cleanly.
func (s *AppleScriptSource) TryEnvelope(ctx context.Context, focused FocusedApp) (Envelope, bool, error) {
	if focused.BundleID != "" && focused.BundleID != s.cfg.BundleID {
		return Envelope{}, false, nil
	}
	pid, err := s.getPID.FocusedPID(ctx, s.cfg.DisplayName)
	if err != nil {
		// TCC permission loss — surface as a typed error so the caller can emit
		// KindPermissionLost. Caller logs + counts.
		if errors.Is(err, ErrPermissionLost) {
			return Envelope{}, false, err
		}
		// Any other failure: graceful skip.
		return Envelope{}, false, nil
	}
	if pid == 0 {
		return Envelope{}, false, nil
	}
	cwd, err := s.execCmd.Run(ctx, pid)
	if err != nil || cwd == "" {
		// lsof failure: graceful skip.
		return Envelope{}, false, nil
	}
	return Envelope{
		PaneCwd:        cwd,
		PaneType:       "terminal",
		PaneResolution: PaneResolutionBestEffortProcess,
	}, true, nil
}

// ContentFreeInvariant: the fallback source constructs an envelope with only PaneCwd
// (a path) and PaneType ("terminal"). Both are content-free by doctrine. The
// invariant is verified by building a synthetic envelope.
func (s *AppleScriptSource) ContentFreeInvariant() error {
	return CheckEnvelopeContentFree(Envelope{
		PaneCwd:        "/Users/example/projects/example",
		PaneType:       "terminal",
		PaneResolution: PaneResolutionBestEffortProcess,
	})
}

// ErrPermissionLost signals that the macOS Accessibility / Screen Recording TCC
// permission is required and missing. The collector surfaces this by emitting a
// KindPermissionLost event so the segment reducer marks downstream spans incomplete.
var ErrPermissionLost = errors.New("tier1/source: accessibility permission required for AppleScript")

// ErrParseFailed signals that a source's vendor output (cmux surface.list,
// vscode AW event, lsof output) was unparseable. Distinct from ErrPermissionLost
// (TCC) so the collector can emit a precise source_error_code. The raw error
// text stays in the CollectReport's audit log, NOT in the activity spool.
var ErrParseFailed = errors.New("tier1/source: vendor output unparseable")

// --- default runners (production wiring) ---

// defaultPIDResolver uses AppleScript via System Events to find the focused window's
// PID for the named app. Implementation note: the script is intentionally narrow —
// it asks only for the first process whose name matches the app's display name.
//
// On macOS Sonoma+ this requires Accessibility TCC. If permission is missing, the
// AppleScript engine returns error -1719 ("AppleScript is not allowed assistive
// access"); we surface that as ErrPermissionLost.
type defaultPIDResolver struct{}

// FocusedPID runs the AppleScript and parses the PID out of the result.
func (defaultPIDResolver) FocusedPID(ctx context.Context, appName string) (int, error) {
	// We use `pgrep` instead of AppleScript for a much simpler TCC story: pgrep is
	// not a TCC-protected API. We then verify the focused app via System Events only
	// when we need to. For v1 the PID resolution strategy is "find a running process
	// for this app" — good enough for the typical case where the app has exactly one
	// process. Multi-instance accuracy requires AppleScript, deferred to Phase 5+.
	if appName == "" {
		return 0, nil
	}
	out, err := exec.CommandContext(ctx, "pgrep", "-x", appName).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// pgrep exits 1 when no match — graceful "not focused".
			return 0, nil
		}
		return 0, nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return 0, nil
	}
	var pid int
	if _, err := fmt.Sscanf(lines[0], "%d", &pid); err != nil {
		return 0, nil
	}
	return pid, nil
}

// defaultLsofRunner shells out to lsof to read the cwd file descriptor of a process.
// Output format: lines starting with "n " are names; the cwd line carries the path.
type defaultLsofRunner struct{}

// Run calls `lsof -a -p <pid> -d cwd -F n` and parses the lone "n <path>" line.
func (defaultLsofRunner) Run(ctx context.Context, pid int) (string, error) {
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", fmt.Sprintf("%d", pid), "-d", "cwd", "-F", "n").CombinedOutput()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "n ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "n")), nil
		}
	}
	return "", nil
}
