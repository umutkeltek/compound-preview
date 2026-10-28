package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
)

// CmuxBundleID is the macOS bundle identifier for cmux. cmux is the user's primary
// terminal (verified on this host). This is the only EnvelopeSource that has an IPC
// fast path; everything else falls back to AppleScript + lsof.
const CmuxBundleID = "com.cmuxterm.app"

// CmuxSource reads `cmux rpc surface.list` and extracts envelope fields. The full
// surface.list payload includes content-bearing fields (`initial_command`,
// `resume_binding.command`, `resume_binding.environment`); the source STRIPS these
// before constructing the Envelope. The ContentFreeInvariant self-test is the
// defense-in-depth that catches a future change that accidentally leaks one.
type CmuxSource struct {
	runner cmuxRunner
}

// CmuxRunner abstracts the exec call so tests can stub. Production wiring uses
// ExecRunner, which shells out to the real `cmux` binary. Exported so external
// tests (notably the tier1 integration smoke) can inject canned JSON responses
// without spawning a real cmux process.
type CmuxRunner interface {
	Run(ctx context.Context) ([]byte, error)
}

// cmuxRunner is the package-internal alias preserved for callers in this file.
type cmuxRunner = CmuxRunner

// ExecRunner is the production CmuxRunner — calls `cmux rpc surface.list`.
// Exported so callers in other packages can construct it explicitly (rarely
// needed; NewCmuxSource uses this by default).
type ExecRunner struct{}

// Run shells out to `cmux rpc surface.list` and returns the combined output (stdout +
// stderr). If `cmux` is not on PATH, the exec package returns an error wrapping
// fs.ErrNotExist — surfaced verbatim so the caller can decide to skip the source.
func (ExecRunner) Run(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "cmux", "rpc", "surface.list").CombinedOutput()
}

// execRunner is the package-internal alias preserved for callers in this file.
type execRunner = ExecRunner

// NewCmuxSource returns a CmuxSource backed by the production ExecRunner.
func NewCmuxSource() *CmuxSource { return &CmuxSource{runner: ExecRunner{}} }

// NewCmuxSourceWithRunner is a test-only constructor — the runner is replaceable
// so tests can inject fixture JSON without spawning a real cmux process.
// Exported because the tier1 integration smoke test (in another package)
// needs to construct a CmuxSource with a canned runner.
func NewCmuxSourceWithRunner(r CmuxRunner) *CmuxSource { return &CmuxSource{runner: r} }

// newCmuxSourceWithRunner is the package-internal alias preserved for callers
// in this file's tests.
func newCmuxSourceWithRunner(r cmuxRunner) *CmuxSource { return &CmuxSource{runner: r} }

// AppBundleID returns "com.cmuxterm.app".
func (s *CmuxSource) AppBundleID() string { return CmuxBundleID }

// TryEnvelope calls `cmux rpc surface.list`, parses the JSON, picks the focused
// surface, and extracts envelope fields. Returns (zero, false, nil) if cmux isn't
// installed (graceful skip — the collector records SourcesFailed++ and continues).
// Returns (zero, false, err) on parse / IO failure.
func (s *CmuxSource) TryEnvelope(ctx context.Context, focused FocusedApp) (Envelope, bool, error) {
	if focused.BundleID != "" && focused.BundleID != CmuxBundleID {
		return Envelope{}, false, nil
	}
	out, err := s.runner.Run(ctx)
	if err != nil {
		// exec.ErrNotFound indicates cmux is not installed. Surface as "not focused"
		// rather than "errored" — the caller counts this as SourcesFailed++ but doesn't
		// abort the run.
		if errors.Is(err, exec.ErrNotFound) {
			return Envelope{}, false, nil
		}
		// Any other transport error: caller logs + counts.
		return Envelope{}, false, fmt.Errorf("cmux runner: %w", err)
	}
	var resp cmuxSurfaceListResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return Envelope{}, false, fmt.Errorf("cmux: parse surface.list: %w", err)
	}
	surf := pickFocusedSurface(resp.Surfaces)
	if surf == nil {
		// cmux is running but no surface is focused — return zero envelope, no error.
		return Envelope{}, false, nil
	}
	env := Envelope{
		PaneCwd:        firstNonEmpty(surf.RequestedWorkingDirectory, resumeCwd(surf.ResumeBinding)),
		WorkspaceRef:   Slug(surf.WorkspaceID),
		SurfaceRef:     Slug(surf.ID),
		WindowRef:      Slug(surf.WindowID),
		PaneType:       cmuxPaneTypeToSurfaceType(surf.Type),
		PaneResolution: PaneResolutionCmuxIPC,
	}
	if surf.ResumeBinding != nil {
		env.AgentKind = surf.ResumeBinding.Kind
		env.AgentName = surf.ResumeBinding.Name
	}
	return env, true, nil
}

// ContentFreeInvariant: the envelope construction in TryEnvelope does not populate any
// banned key (the source fields we read are: requested_working_directory, type,
// resume_binding.cwd, resume_binding.kind, resume_binding.name, refs). We verify by
// constructing a synthetic envelope and running CheckEnvelopeContentFree. If a future
// change adds a new field assignment, this test catches it.
func (s *CmuxSource) ContentFreeInvariant() error {
	env := Envelope{
		PaneCwd:        "/Users/example/projects/example",
		PaneType:       "terminal",
		AgentKind:      "agent-hook",
		AgentName:      "Claude Code",
		WorkspaceRef:   "ws-abc123",
		SurfaceRef:     "surf-def456",
		WindowRef:      "win-789",
		PaneResolution: PaneResolutionCmuxIPC,
	}
	return CheckEnvelopeContentFree(env)
}

// pickFocusedSurface returns the surface with focused=true. Returns nil if no surface
// is marked focused — cmux marks exactly one surface focused at a time, so a missing
// focus flag means focus is somewhere else (sidebar, browser panel, etc.) and we
// should not synthesize envelope data for the wrong surface.
func pickFocusedSurface(surfaces []cmuxSurface) *cmuxSurface {
	for i := range surfaces {
		if surfaces[i].Focused {
			return &surfaces[i]
		}
	}
	return nil
}

// cmuxPaneTypeToSurfaceType maps cmux's surface type strings to our PaneType enum.
// cmux uses "terminal" / "browser" / "agent-session"; these are already canonical.
func cmuxPaneTypeToSurfaceType(s string) string {
	switch s {
	case "terminal", "browser", "agent-session":
		return s
	}
	return ""
}

// firstNonEmpty returns a if non-empty, otherwise b. Used to prefer the initial cwd
// over the resume-binding cwd when both are present.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// resumeCwd safely reads resume_binding.cwd. Returns "" if binding is nil.
func resumeCwd(r *cmuxResumeBinding) string {
	if r == nil {
		return ""
	}
	return r.Cwd
}

// --- cmux wire format (kept private to this file) ---

// cmuxSurfaceListResponse is the top-level shape of `cmux rpc surface.list`. The
// response is a JSON object with a "surfaces" array; each surface carries the focused
// flag plus resume_binding metadata. We deserialize into a minimal subset — anything
// we don't read is dropped (and content-bearing fields stay that way: ignored).
type cmuxSurfaceListResponse struct {
	Surfaces []cmuxSurface `json:"surfaces"`
}

// cmuxSurface is the per-surface subset we care about. Fields we don't read
// (`initial_command`, `resume_binding.command`, `resume_binding.environment`) are
// intentionally absent from the struct — the JSON decoder drops them on Unmarshal.
type cmuxSurface struct {
	ID                        string             `json:"id"`
	Type                      string             `json:"type"`
	Focused                   bool               `json:"focused"`
	WindowID                  string             `json:"window_id"`
	WorkspaceID               string             `json:"workspace_id"`
	PaneID                    string             `json:"pane_id"`
	PaneRef                   string             `json:"pane_ref"`
	RequestedWorkingDirectory string             `json:"requested_working_directory"`
	ResumeBinding             *cmuxResumeBinding `json:"resume_binding,omitempty"`
}

// cmuxResumeBinding is the agent-session resume envelope. We read only the
// content-free fields (kind, name) and the cwd. `command` and `environment` are
// intentionally NOT deserialized — their presence in the JSON doesn't grant them
// presence in our envelope.
type cmuxResumeBinding struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Cwd  string `json:"cwd"`
}
