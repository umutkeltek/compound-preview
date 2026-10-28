package work

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// MultiplexerObservation is one session as the terminal multiplexer itself
// sees it, independent of any compound-recorded binding.
//
// It is the second observation source for `compound sessions live` so the
// report can answer "what panes is the operator actually running" — not
// only "what panes did compound spawn". CWD and LastActivity come straight
// from tmux; lane identity is left to the caller, which can read it from
// the session name or enrich it with domain knowledge.
type MultiplexerObservation struct {
	Name         string    // tmux session name
	CWD          string    // focused pane's working directory (pane_current_path)
	LastActivity time.Time // unix epoch from session_activity, second precision
}

// MultiplexerSource lists every supervised pane the multiplexer can see.
// Production wires this to the operator's tmux server; tests wire it to a
// private server through TmuxRunner.SetTestSocketPrefix.
//
// It is intentionally NOT a SessionRunner — the multiplexer source is
// read-only (no Create, no SendKeys, no Kill), and conflating the two
// would let a reaper that only wanted the runner pull multiplexer-only
// panes too.
type MultiplexerSource interface {
	// List returns every tmux session the source can see. A missing tmux
	// server is an error, NOT an empty list: a silent empty list is the
	// dangerous failure mode (operator sees \"nothing reported, must be
	// fine\") that motivated this whole observation source.
	List() ([]MultiplexerObservation, error)

	// ResumeArgv returns the argv an operator can paste into a shell to
	// put the named pane back on screen. For tmux this is
	// `tmux attach-session -t <name>`.
	ResumeArgv(name string) []string
}

// Multiplexer returns a MultiplexerSource backed by this runner's tmux
// server (operator's server in production; the test's private socket
// under the injected socketPrefix). Returns a non-nil source as long as
// the runner exists; the underlying tmux calls return errors at List
// time if the server is unreachable.
func (r *TmuxRunner) Multiplexer() MultiplexerSource {
	return &tmuxMultiplexer{r: r}
}

// tmuxMultiplexer is the tmux-backed MultiplexerSource. It reuses the
// runner's socketPrefix and tmuxCmd shims so a private-server test
// seam applies to multiplexer observation too — there is no parallel
// code path that could fall back to the operator's server when the
// test expected isolation.
type tmuxMultiplexer struct {
	r *TmuxRunner
}

// List drives `tmux list-sessions -F` against the runner's server and
// returns one MultiplexerObservation per session. Fields are read from
// tmux's documented formats:
//   - #{session_name}       -> session name
//   - #{pane_current_path}  -> focused pane cwd
//   - #{session_activity}   -> unix epoch seconds of last input
//
// The format string uses `||` as the field separator (a string tmux
// itself does not emit in any of these fields) so a malformed row that
// loses a field is detected by the field count, not silently truncated.
func (m *tmuxMultiplexer) List() ([]MultiplexerObservation, error) {
	r := m.r
	if r == nil {
		return nil, errors.New("tmux multiplexer source has no runner")
	}
	cmd := r.tmuxExec("list-sessions", "-F",
		"#{session_name}||#{pane_current_path}||#{session_activity}")
	cmd.Stderr = &bytes.Buffer{}
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := ""
			if b, ok := cmd.Stderr.(*bytes.Buffer); ok {
				stderr = strings.TrimSpace(b.String())
			}
			// "no server running" / "no sessions" — return an empty list,
			// not an error, so a fresh tmux server does not blow up the
			// report. A tmux server that crashed mid-call IS an error.
			if tmuxTargetMissing(stderr) {
				return nil, nil
			}
			return nil, fmt.Errorf("tmux list-sessions failed: exit %d: %s", exitErr.ExitCode(), stderr)
		}
		return nil, fmt.Errorf("tmux list-sessions execution failed: %w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var out2 []MultiplexerObservation
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "||")
		if len(parts) != 3 {
			return nil, fmt.Errorf("tmux list-sessions returned malformed row %q", line)
		}
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		cwd := strings.TrimSpace(parts[1])
		var lastActivity time.Time
		if raw := strings.TrimSpace(parts[2]); raw != "" {
			n, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("tmux list-sessions returned invalid session_activity %q", raw)
			}
			lastActivity = time.Unix(n, 0).UTC()
		}
		out2 = append(out2, MultiplexerObservation{
			Name:         name,
			CWD:          cwd,
			LastActivity: lastActivity,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan tmux list-sessions output: %w", err)
	}
	return out2, nil
}

// ResumeArgv returns `tmux attach-session -t <name>` so an operator can
// put a multiplexer-observed pane back on screen. The session name is
// trimmed to keep a pasted command from breaking on stray whitespace.
func (m *tmuxMultiplexer) ResumeArgv(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return []string{"tmux", "attach-session"}
	}
	return []string{"tmux", "attach-session", "-t", name}
}
