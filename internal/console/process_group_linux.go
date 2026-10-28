//go:build linux

package console

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// consoleProcessGroupAlive reports whether any NON-ZOMBIE process remains in
// the group. kill(-pgid, 0) alone is not sufficient on linux: a killed but
// unreaped child (zombie) still satisfies the signal-0 probe, so a supervisor
// running under a non-reaping pid 1 (docker, containerd sidecars) waits out
// its grace window and reports forced-shutdown failure for a group that is
// in fact dead. Zombies are dead; they are not alive. Found by the
// console-supervisor falsifiers in containers (CI follow-up, 2026-09-19).
func ProcessGroupAlive(pgid int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, fmt.Errorf("read /proc: %w", err)
	}
	alive := false
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		body, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			// Vanished between the readdir and this read (ENOENT/ESRCH):
			// the pid cannot be part of a live group.
			continue
		}
		closing := strings.LastIndexByte(string(body), ')')
		if closing < 0 {
			continue
		}
		fields := strings.Fields(string(body[closing+1:]))
		if len(fields) <= 2 {
			continue
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil || group != pgid {
			continue
		}
		switch fields[0] {
		case "Z", "X", "x":
			// zombie or exiting: dead, not alive
			continue
		}
		alive = true
	}
	return alive, nil
}
