package harness

import (
	"context"
	"os"
	"os/exec"
	"time"

	"compound/internal/runattempt"
)

// defaultHeartbeatInterval is the wall-clock cadence the adapter calls
// store.Heartbeat on the minted RunAttempt. Tunable per-adapter via
// NewOSEntrypointsAdapterWithInterval (tests).
const defaultHeartbeatInterval = 5 * time.Second

// OSEntrypointsAdapter is the first real Adapter: it drives an OS-level
// subprocess (the entrypoint declared in OSEntrypointsFile) and wraps
// the execution in a RunAttempt. The actual subprocess execution is
// shared with the Claude and Codex adapters via runWithSubprocess in
// subprocess_runner.go; this type only supplies the harness-specific
// bits (name, argv passthrough, heartbeat cadence).
//
// The adapter is intentionally simple:
//   - one subprocess per Run call
//   - transcript captured line-by-line to <home>/run_attempts/<id>.transcript
//   - heartbeats on a fixed interval while the subprocess is running
//   - terminal status derived from exit code (0 -> succeeded, else failed)
//   - timeout (when set) kills the subprocess and marks TimedOut=true
type OSEntrypointsAdapter struct {
	store             *runattempt.Store
	heartbeatInterval time.Duration
}

// NewOSEntrypointsAdapter returns the production adapter. Heartbeats
// every 5s. For tests, use NewOSEntrypointsAdapterWithInterval.
func NewOSEntrypointsAdapter(store *runattempt.Store) *OSEntrypointsAdapter {
	return &OSEntrypointsAdapter{store: store, heartbeatInterval: defaultHeartbeatInterval}
}

// NewOSEntrypointsAdapterWithInterval returns an adapter with a custom
// heartbeat cadence. Test-only: production code uses the 5s default.
func NewOSEntrypointsAdapterWithInterval(store *runattempt.Store, interval time.Duration) *OSEntrypointsAdapter {
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	return &OSEntrypointsAdapter{store: store, heartbeatInterval: interval}
}

// Name returns the manifest adapter id. Matches OSEntrypointsAdapterID
// and OSEntrypointsFile.AdapterID so the Registry slice (C.3) can key
// adapters to manifest rows.
func (a *OSEntrypointsAdapter) Name() string { return "os-harness-entrypoints" }

// Run drives the subprocess end-to-end. Delegates to runWithSubprocess
// with a buildCmd that passes Command+Args through verbatim -- the
// os-entrypoints adapter does no argv transformation, it just exec's
// whatever the caller named in RunInput.Command.
func (a *OSEntrypointsAdapter) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	return runWithSubprocess(ctx, a.store, a.Name(), in, a.buildCmd, a.heartbeatInterval)
}

func (a *OSEntrypointsAdapter) buildCmd(ctx context.Context, in RunInput) *exec.Cmd {
	// Timeout is owned by runWithSubprocess (a kill timer enforces
	// RunInput.Timeout regardless of how the command is built); buildCmd
	// only wires the caller's ctx so an externally-cancelled context still
	// propagates. Deliberately NOT adding a second context.WithTimeout
	// here -- two independent deadlines race and the loser masks the
	// TimedOut signal.
	cmd := exec.CommandContext(ctx, in.Command, in.Args...)
	cmd.Dir = in.Workspace
	cmd.Env = append(os.Environ(), in.Env...)
	return cmd
}
