package executewire

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the detached door its own process-group leader so a
// signal aimed at the heartbeat's group (the tick's `timeout 600`) never
// reaches it, and so `kill -TERM -<pid>` cancels the door and everything it
// spawned as one unit.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
