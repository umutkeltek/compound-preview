package act

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"compound/internal/envelope"
)

// ExecTransportAdapter delivers an outbound body through one named, home-configured
// executable transport. Its unexported write method keeps execution behind Execute's
// PolicyDecision gate.
type ExecTransportAdapter struct {
	Name           string
	Home           string
	lastOutput     []byte
	contentBinding *ContentTransportBinding
	requestDigest  string
	admissionCheck func() error
}

var _ Adapter = (*ExecTransportAdapter)(nil)

func NewExecTransportAdapter(name, home string) Adapter {
	return &ExecTransportAdapter{Name: name, Home: home}
}

// ReadCurrent returns the successful transport's bounded stdout after a write,
// making a remote id or verification object the governed post-image. Before a
// write, the outbound state has no remote pre-image and remains the empty sentinel.
func (a *ExecTransportAdapter) ReadCurrent(envelope.ActionTarget) ([]byte, error) {
	if len(a.lastOutput) == 0 {
		return []byte("{}"), nil
	}
	return append([]byte(nil), a.lastOutput...), nil
}

// A transport creates an external effect; its empty pre-image is not a request
// that can undo it. Cancellation needs its own exact, authorized contract.
func (a *ExecTransportAdapter) restorePreImage(envelope.ActionTarget, []byte) error {
	return fmt.Errorf("transport %q has no pre-image restore contract; reconcile the original attempt before another request", a.Name)
}

type execTransportUncertainError struct{ err error }

func (e *execTransportUncertainError) Error() string               { return e.err.Error() }
func (e *execTransportUncertainError) Unwrap() error               { return e.err }
func (*execTransportUncertainError) MutationMayHaveOccurred() bool { return true }

type execTransportConfig struct {
	Argv            []string `json:"argv"`
	TimeoutS        int      `json:"timeout_s,omitempty"`
	ContentContract string   `json:"content_contract,omitempty"`
}

const (
	defaultExecTransportTimeout = 60 * time.Second
	maxExecTransportOutput      = 64 * 1024
	maxExecErrorOutputSummary   = 200
)

// execTransportAllowedNames is the allowlist of executable basenames a transport
// config may invoke without an absolute path. It keeps the "named, home-configured
// executable transport" model intentional: a transport is a deliberately enrolled
// local program, not a shell or arbitrary command lookup.
var execTransportAllowedNames = map[string]bool{
	"claude":   true,
	"codex":    true,
	"compound": true,
	"zai":      true,
	"qwen":     true,
	"minimax":  true,
}

// shellUnsafeRe rejects arguments that look like shell control operators, redirects,
// command separators, or variable/command substitutions. The adapter does not use a
// shell, but the config file is operator-edited, so we fail closed on characters that
// could be misinterpreted by a downstream shell or by exec.Command's argument splitting.
var shellUnsafeRe = regexp.MustCompile(`[;|&<>(){}\[\]\$\` + "`" + `*?\\\n\r]`)

// shellExecutableDenylist rejects well-known interactive shells and interpreters even
// when they are referenced by an absolute path. A transport is a deliberately enrolled
// executable, not a shell or interpreter invocation. The map intentionally omits
// version suffixes; isShellExecutable strips trailing version fragments like "3.9" or
// "2.7" so that "/usr/local/bin/python3.9" is rejected as readily as "python".
var shellExecutableDenylist = map[string]bool{
	"sh":      true,
	"bash":    true,
	"zsh":     true,
	"dash":    true,
	"csh":     true,
	"tcsh":    true,
	"ksh":     true,
	"fish":    true,
	"python":  true,
	"python3": true,
	"perl":    true,
	"ruby":    true,
	"node":    true,
	"nodejs":  true,
}

// shellExecutableVersionRe captures a base interpreter name followed by a trailing
// version suffix (e.g. "python3.9", "ruby2.7", "perl5.34", "node18"). The non-greedy
// group ensures we peel the smallest matching base, then check that base against the
// denylist.
var shellExecutableVersionRe = regexp.MustCompile(`^(.+?)[0-9][0-9.]*$`)

// isShellExecutable reports whether a path's basename is a denied shell or interpreter,
// including versioned variants like "python3.9".
func isShellExecutable(path string) bool {
	base := filepath.Base(path)
	if shellExecutableDenylist[base] {
		return true
	}
	if m := shellExecutableVersionRe.FindStringSubmatch(base); m != nil {
		return shellExecutableDenylist[m[1]]
	}
	return false
}

type cappedTransportOutput struct {
	// Do not embed bytes.Buffer: its promoted ReadFrom lets io.Copy bypass
	// Write, silently defeating the size limit for exec's output pipe.
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (w *cappedTransportOutput) Write(p []byte) (int, error) {
	remaining := w.limit - w.buffer.Len()
	if len(p) > remaining {
		w.truncated = true
	}
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.buffer.Write(p[:remaining])
	}
	return len(p), nil
}

func (a ExecTransportAdapter) loadConfig() (execTransportConfig, error) {
	name := strings.TrimSpace(a.Name)
	if name == "" || filepath.Base(name) != name || name == "." {
		return execTransportConfig{}, fmt.Errorf("transport %q config: invalid transport name", a.Name)
	}
	if strings.TrimSpace(a.Home) == "" {
		return execTransportConfig{}, fmt.Errorf("transport %q config: home is required", name)
	}
	path := filepath.Join(a.Home, "actuator", "transports", name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return execTransportConfig{}, fmt.Errorf("transport %q config: read %s: %w", name, path, err)
	}
	var cfg execTransportConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return execTransportConfig{}, fmt.Errorf("transport %q config: parse %s: %w", name, path, err)
	}
	if len(cfg.Argv) == 0 || strings.TrimSpace(cfg.Argv[0]) == "" {
		return execTransportConfig{}, fmt.Errorf("transport %q config: argv must contain an executable", name)
	}
	if err := validateExecArgv(name, cfg.Argv); err != nil {
		return execTransportConfig{}, err
	}
	if cfg.TimeoutS < 0 {
		return execTransportConfig{}, fmt.Errorf("transport %q config: timeout_s must be non-negative", name)
	}
	return cfg, nil
}

// validateExecArgv enforces the executable allowlist / absolute-path + argument
// validation rule. argv[0] must be either a clean absolute path to a non-shell
// executable, or one of the allowed basenames. Every argument must be free of shell
// metacharacters.
func validateExecArgv(transportName string, argv []string) error {
	return ValidateEnrolledArgv("transport", transportName, argv, execTransportAllowedNames)
}

// ValidateEnrolledArgv is the ONE owner of "may this operator-enrolled config
// invoke this program?". It is exported because a second enrolled-executable
// surface exists (product media generators) and a security rule with two copies
// is a rule that drifts — the same failure class as a platform list that lived in
// three places while claiming to be one.
//
// kind and name only shape the error text. allowedBareNames is the caller's
// allowlist for non-absolute argv[0]; pass nil to require an absolute path.
//
// The rules, unchanged: an absolute argv[0] must be clean and must not be a shell
// or interpreter (which would turn an enrolled program into arbitrary command
// execution); a bare argv[0] must be a plain basename on the caller's allowlist;
// and no argument may carry shell control characters, because the config is
// operator-edited and may be read by a downstream shell even though this adapter
// never uses one.
func ValidateEnrolledArgv(kind, name string, argv []string, allowedBareNames map[string]bool) error {
	if len(argv) == 0 {
		return fmt.Errorf("%s %q config: argv is empty", kind, name)
	}
	exe := argv[0]
	if strings.TrimSpace(exe) == "" {
		return fmt.Errorf("%s %q config: argv[0] executable is empty", kind, name)
	}
	if filepath.IsAbs(exe) {
		if clean := filepath.Clean(exe); clean != exe || clean == "/" || strings.Contains(exe, "..") {
			return fmt.Errorf("%s %q config: argv[0] must be a clean absolute path", kind, name)
		}
		if isShellExecutable(exe) {
			return fmt.Errorf("%s %q config: argv[0] is a shell/interpreter and cannot be used as a %s", kind, name, kind)
		}
	} else {
		if filepath.Base(exe) != exe || filepath.Dir(exe) != "." || !allowedBareNames[exe] {
			return fmt.Errorf("%s %q config: argv[0] %q is not an allowed %s executable", kind, name, exe, kind)
		}
	}
	for i, arg := range argv {
		if shellUnsafeRe.MatchString(arg) {
			return fmt.Errorf("%s %q config: argv[%d] contains unsafe characters", kind, name, i)
		}
	}
	return nil
}

// summarizeOutput returns a redacted, bounded summary of combined stdout/stderr for
// error messages. The full output is retained in the cappedTransportOutput buffer for
// separate recording if needed, but it is never echoed verbatim to callers.
func summarizeOutput(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return "<no output>"
	}
	// Collapse runs of whitespace and strip control characters for a compact summary.
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > maxExecErrorOutputSummary {
		return out[:maxExecErrorOutputSummary] + "... [output redacted]"
	}
	return out
}

func (a *ExecTransportAdapter) write(_ envelope.ActionTarget, newValue []byte) error {
	a.lastOutput = nil
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if a.contentBinding != nil {
		cfg.Argv = append([]string(nil), a.contentBinding.Argv...)
		cfg.TimeoutS = a.contentBinding.TimeoutS
	}
	timeout := defaultExecTransportTimeout
	if cfg.TimeoutS > 0 {
		timeout = time.Duration(cfg.TimeoutS) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Argv[0], cfg.Argv[1:]...)
	cmd.Stdin = bytes.NewReader(newValue)
	output := &cappedTransportOutput{limit: maxExecTransportOutput}
	diagnostics := &cappedTransportOutput{limit: maxExecTransportOutput}
	cmd.Stdout = output
	cmd.Stderr = diagnostics
	if a.contentBinding != nil {
		if ContentTransportDigest(newValue) != a.requestDigest {
			return fmt.Errorf("content transport request changed")
		}
		if a.admissionCheck != nil {
			if err := a.admissionCheck(); err != nil {
				return err
			}
		}
		if err := CheckContentTransport(a.Home, *a.contentBinding); err != nil {
			return err
		}
		cmd.Path = a.contentBinding.Executable
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("transport %q start: %w", strings.TrimSpace(a.Name), err)
	}
	err = cmd.Wait()
	// Stdout is the response contract. Stderr is diagnostic only. Retain
	// partial response bytes even on error: the external operation may exist.
	if len(bytes.TrimSpace(output.buffer.Bytes())) > 0 {
		a.lastOutput = append([]byte(nil), output.buffer.Bytes()...)
	}
	summary := summarizeOutput(output.buffer.String() + "\n" + diagnostics.buffer.String())
	if ctx.Err() == context.DeadlineExceeded {
		return &execTransportUncertainError{fmt.Errorf("transport %q timed out after %s; output: %s", strings.TrimSpace(a.Name), timeout, summary)}
	}
	if err != nil {
		return &execTransportUncertainError{fmt.Errorf("transport %q exec: %w; output: %s", strings.TrimSpace(a.Name), err, summary)}
	}
	if output.truncated {
		return &execTransportUncertainError{fmt.Errorf("transport %q response exceeded %d bytes; retained prefix is not a complete observation", strings.TrimSpace(a.Name), maxExecTransportOutput)}
	}
	return nil
}
