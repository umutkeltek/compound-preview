package jobprobe

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CommandRunner runs one external command and reports its stdout+stderr, its
// exit code, and a start error. A non-zero exit is a valid OBSERVATION (returned
// with err == nil and the code); a non-nil error means the command could not
// start at all (e.g. the tool is absent) and is surfaced as a probe error, not a
// dead unit. Injecting this interface is what lets SystemProbeRunner be tested
// without ever probing the real machine.
type CommandRunner interface {
	Run(name string, args ...string) (output string, exitCode int, err error)
}

// SystemProbeRunner is the real probe runner: it shells out to launchctl/crontab
// for local units and to a declared command for remote hosts, all through an
// injectable CommandRunner. It hardcodes no host or unit — every host/unit/argv
// comes from the operator's private targets file.
type SystemProbeRunner struct {
	Cmd CommandRunner
}

// NewSystemProbeRunner returns a SystemProbeRunner backed by os/exec.
func NewSystemProbeRunner() SystemProbeRunner {
	return SystemProbeRunner{Cmd: execCommandRunner{}}
}

// Probe observes the current liveness of one target. launchctl/crontab exit
// status and crontab content are the observed truth for local units; a declared
// remote probe command's exit code is the observed truth for remote hosts.
func (r SystemProbeRunner) Probe(target ProbeTarget, _ time.Time) (ProbeResult, error) {
	cmd := r.Cmd
	if cmd == nil {
		cmd = execCommandRunner{}
	}
	switch target.Kind {
	case KindLocalLaunchd:
		_, code, err := cmd.Run("launchctl", "list", target.Unit)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("launchctl probe failed to run: %w", err)
		}
		if code == 0 {
			return ProbeResult{Alive: true, Detail: "launchctl reports the unit loaded"}, nil
		}
		return ProbeResult{Alive: false, Detail: "launchctl reports the unit not loaded"}, nil
	case KindLocalCron:
		out, code, err := cmd.Run("crontab", "-l")
		if err != nil {
			return ProbeResult{}, fmt.Errorf("crontab probe failed to run: %w", err)
		}
		if code == 0 && strings.Contains(out, target.Match) {
			return ProbeResult{Alive: true, Detail: "crontab entry present"}, nil
		}
		return ProbeResult{Alive: false, Detail: "crontab entry absent"}, nil
	case KindRemote:
		if len(target.ProbeCommand) == 0 {
			return ProbeResult{}, fmt.Errorf("remote probe target missing probe_command")
		}
		_, code, err := cmd.Run(target.ProbeCommand[0], target.ProbeCommand[1:]...)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("remote probe failed to run: %w", err)
		}
		if code == 0 {
			return ProbeResult{Alive: true, Detail: "declared remote probe exited 0"}, nil
		}
		return ProbeResult{Alive: false, Detail: fmt.Sprintf("declared remote probe exited %d", code)}, nil
	default:
		return ProbeResult{}, fmt.Errorf("unknown probe kind %q", target.Kind)
	}
}

type execCommandRunner struct{}

func (execCommandRunner) Run(name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return out.String(), exitErr.ExitCode(), nil
		}
		return out.String(), -1, err
	}
	return out.String(), 0, nil
}
