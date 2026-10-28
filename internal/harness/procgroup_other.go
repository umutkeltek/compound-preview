//go:build !unix

package harness

import "os/exec"

// setProcessGroup is the non-unix fallback: without setpgid/killpg semantics
// there is no portable process group to create, so timeout enforcement stays
// direct-child-only (see procgroup_unix.go for the unix behavior).
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree is the non-unix fallback: with no process groups the best
// available timeout enforcement is killing the direct child.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
