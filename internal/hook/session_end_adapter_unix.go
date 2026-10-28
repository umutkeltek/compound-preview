//go:build unix

package hook

import (
	"fmt"
	"os/exec"
	"syscall"
)

func setSessionEndAdapterProcessGroup(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}

func killSessionEndAdapterProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		if processErr := command.Process.Kill(); processErr != nil {
			return fmt.Errorf("kill process group: %v; kill worker: %w", err, processErr)
		}
	}
	return nil
}
