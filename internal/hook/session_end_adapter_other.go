//go:build !unix

package hook

import (
	"fmt"
	"os/exec"
)

func setSessionEndAdapterProcessGroup(_ *exec.Cmd) {}

func killSessionEndAdapterProcessGroup(command *exec.Cmd) error {
	if command.Process != nil {
		if err := command.Process.Kill(); err != nil {
			return fmt.Errorf("kill worker: %w", err)
		}
	}
	return nil
}
