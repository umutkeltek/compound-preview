//go:build unix

package harness

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the subprocess in its own process group so the timeout
// kill can reach every descendant that STAYS in that group, not just the
// direct child. Without it an orphaned grandchild that inherited the
// stdout/stderr pipes keeps them open after the child dies: capture never
// sees EOF, capWG.Wait blocks forever, and the fresh heartbeats hide the
// stall from SweepStale.
//
// LIMIT: the group signal reaches the direct child and the descendants that
// inherited its process group. It does NOT reach a descendant that starts its
// own process group or session (setsid, a new pgid), nor work handed to a
// remote or external job. Such a descendant can outlive the group kill.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree SIGKILLs the subprocess's whole process group. With
// Setpgid the child is its group leader (pgid == pid), so the negative-pid
// kill reaches every descendant that remained in the group and holds the
// capture pipes. Falls back to killing only the direct child when the group
// signal fails (e.g. the group is already gone because the child exited
// between the timer firing and the kill).
//
// The name says "tree", but the guarantee is group-scoped: a descendant that
// left the group (its own pgid/session) is NOT signalled, and neither is a
// remote/external job. See setProcessGroup's limit note.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
