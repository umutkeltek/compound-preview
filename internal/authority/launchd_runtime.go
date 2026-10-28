package authority

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LaunchdCommandRunner is the bounded process seam. An OS exit is data; launch
// and context failures are errors. Only fixed launchctl/plutil paths are used.
type LaunchdCommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}
type LaunchdCommandRunner func(context.Context, string, ...string) (LaunchdCommandResult, error)
type LaunchdEnvironment struct {
	UserHome string
	UID      int
	Run      LaunchdCommandRunner
	Now      func() time.Time
	// Checkpoint is a fault-injection seam, never installed by the CLI.
	Checkpoint func(string) error
}
type LaunchdRuntime struct {
	Domain           string               `json:"domain"`
	Label            string               `json:"label"`
	OverrideState    string               `json:"override_state"` // disabled, enabled, absent, unknown
	LoadedState      string               `json:"loaded_state"`   // loaded, absent, unknown
	ServicePath      string               `json:"service_path,omitempty"`
	Program          string               `json:"program,omitempty"`
	Arguments        []string             `json:"arguments,omitempty"`
	ForeignOverrides map[string]bool      `json:"foreign_overrides,omitempty"`
	DisabledProbe    LaunchdCommandResult `json:"disabled_probe"`
	ServiceProbe     LaunchdCommandResult `json:"service_probe"`
	Error            string               `json:"error,omitempty"`
}

// limitedLaunchdOutput bounds both process memory and persisted evidence.
type limitedLaunchdOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedLaunchdOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 1024*1024 - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// ExecuteLaunchdCommand exposes read-only process observations to schedule.
// Mutating launchctl verbs are reachable only inside the reviewed phase owner.
func ExecuteLaunchdCommand(ctx context.Context, name string, args ...string) (LaunchdCommandResult, error) {
	read := name == "/bin/launchctl" && len(args) == 2 && (args[0] == "print" || args[0] == "print-disabled")
	read = read || (name == "/usr/bin/plutil" && len(args) == 5 && args[0] == "-convert" && args[1] == "json" && args[2] == "-o" && args[3] == "-")
	if !read {
		return LaunchdCommandResult{}, fmt.Errorf("unreviewed launchd mutation refused")
	}
	return executeLaunchdCommand(ctx, name, args...)
}
func executeLaunchdCommand(ctx context.Context, name string, args ...string) (LaunchdCommandResult, error) {
	if name != "/bin/launchctl" && name != "/usr/bin/plutil" {
		return LaunchdCommandResult{}, fmt.Errorf("unsupported launchd executable")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	var out, errout limitedLaunchdOutput
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err := cmd.Run()
	r := LaunchdCommandResult{Stdout: out.String(), Stderr: errout.String()}
	if ctx.Err() != nil {
		return r, ctx.Err()
	}
	if out.overflow || errout.overflow {
		return r, fmt.Errorf("launchd output exceeds evidence bound")
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			r.ExitCode = exit.ExitCode()
			return r, nil
		}
		return r, err
	}
	return r, nil
}
func launchdRun(ctx context.Context, env LaunchdEnvironment, name string, args ...string) (LaunchdCommandResult, error) {
	run := env.Run
	if run == nil {
		run = executeLaunchdCommand
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return run(ctx, name, args...)
}
func launchdNow(env LaunchdEnvironment) time.Time {
	if env.Now != nil {
		return env.Now().UTC()
	}
	return time.Now().UTC()
}

var launchdLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,239}$`)
var launchdOverridePattern = regexp.MustCompile(`^"([^"\r\n]+)"\s*=>\s*(enabled|disabled|true|false)$`)

func parseLaunchdOverrides(body string) (map[string]bool, error) {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "disabled services = {" || strings.TrimSpace(lines[len(lines)-1]) != "}" {
		return nil, fmt.Errorf("unrecognized disabled override response")
	}
	out := map[string]bool{}
	for _, line := range lines[1 : len(lines)-1] {
		m := launchdOverridePattern.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			return nil, fmt.Errorf("partial or malformed disabled override response")
		}
		if _, ok := out[m[1]]; ok {
			return nil, fmt.Errorf("duplicate disabled override")
		}
		out[m[1]] = m[2] == "disabled" || m[2] == "true"
	}
	return out, nil
}
func launchdPrintedValue(v string) (string, error) {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) {
		return strconv.Unquote(v)
	}
	if v == "" || strings.ContainsAny(v, "\r\n\x00") {
		return "", fmt.Errorf("invalid service value")
	}
	return v, nil
}
func parseLaunchdService(body, domain, label string) (string, string, []string, error) {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != domain+"/"+label+" = {" || strings.TrimSpace(lines[len(lines)-1]) != "}" {
		return "", "", nil, fmt.Errorf("service response has wrong identity or incomplete body")
	}
	path, program, serviceDomain := "", "", ""
	var args []string
	inArgs := false
	depth := 1
	for _, raw := range lines[1 : len(lines)-1] {
		line := strings.TrimSpace(raw)
		if inArgs {
			if line == "}" {
				inArgs = false
				depth--
				continue
			}
			v, err := launchdPrintedValue(line)
			if err != nil {
				return "", "", nil, err
			}
			args = append(args, v)
			continue
		}
		if depth == 1 {
			if line == "arguments = {" {
				if args != nil {
					return "", "", nil, fmt.Errorf("duplicate service arguments")
				}
				args = []string{}
				inArgs = true
				depth++
				continue
			}
			for _, field := range []struct {
				prefix string
				dst    *string
			}{{"path = ", &path}, {"program = ", &program}, {"domain = ", &serviceDomain}} {
				if strings.HasPrefix(line, field.prefix) {
					if *field.dst != "" {
						return "", "", nil, fmt.Errorf("duplicate service identity")
					}
					v, err := launchdPrintedValue(strings.TrimPrefix(line, field.prefix))
					if err != nil {
						return "", "", nil, err
					}
					*field.dst = v
				}
			}
		}
		if strings.HasSuffix(line, "{") {
			depth++
		}
		if line == "}" {
			depth--
		}
		if depth < 1 {
			return "", "", nil, fmt.Errorf("malformed service nesting")
		}
	}
	if depth != 1 || inArgs || path == "" || program == "" || !strings.HasPrefix(serviceDomain, domain+" [") {
		return "", "", nil, fmt.Errorf("service identity unreadable")
	}
	if args == nil {
		args = []string{program}
	}
	return path, program, args, nil
}

// ProbeLaunchdRuntime distinguishes unknown from absent. A nonzero launchctl
// exit is absent only for the exact documented service-not-found response.
func ProbeLaunchdRuntime(ctx context.Context, env LaunchdEnvironment, label string) LaunchdRuntime {
	r := LaunchdRuntime{Domain: fmt.Sprintf("gui/%d", env.UID), Label: label, OverrideState: "unknown", LoadedState: "unknown"}
	if env.UID < 1 || !launchdLabelPattern.MatchString(label) {
		r.Error = "invalid current-user domain or label"
		return r
	}
	disabled, err := launchdRun(ctx, env, "/bin/launchctl", "print-disabled", r.Domain)
	r.DisabledProbe = disabled
	if err != nil || disabled.ExitCode != 0 || strings.TrimSpace(disabled.Stderr) != "" {
		r.Error = fmt.Sprintf("disabled override probe failed: %v", err)
		return r
	}
	overrides, err := parseLaunchdOverrides(disabled.Stdout)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.OverrideState = "absent"
	if v, ok := overrides[label]; ok {
		r.OverrideState = "enabled"
		if v {
			r.OverrideState = "disabled"
		}
	}
	delete(overrides, label)
	r.ForeignOverrides = overrides
	service, err := launchdRun(ctx, env, "/bin/launchctl", "print", r.Domain+"/"+label)
	r.ServiceProbe = service
	if err != nil {
		r.Error = err.Error()
		return r
	}
	absent := fmt.Sprintf("Bad request.\nCould not find service %q in domain for user gui: %d", label, env.UID)
	if service.ExitCode == 113 && strings.TrimSpace(service.Stdout) == "" && strings.TrimSpace(service.Stderr) == absent {
		r.LoadedState = "absent"
		return r
	}
	if service.ExitCode != 0 || strings.TrimSpace(service.Stderr) != "" {
		r.Error = "service probe failed without exact absence evidence"
		return r
	}
	r.ServicePath, r.Program, r.Arguments, err = parseLaunchdService(service.Stdout, r.Domain, label)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.LoadedState = "loaded"
	return r
}
