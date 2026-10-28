//go:build darwin || linux

package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func jevProcessSupported() bool { return true }

// uv and Python share an owned process group. On timeout, SIGTERM lets the
// Python context manager close its Chrome and Browser Harness daemon before
// CommandContext's hard WaitDelay expires.
func configureJevProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
}

func newJevSession() (string, error) {
	return os.MkdirTemp("/tmp", "cj-")
}

// The controller owns detached Chrome and Browser Harness processes even when
// uv/Python exits without running the bridge's finally block.
func cleanupJevSession(dir string) error {
	if filepath.Dir(dir) != "/tmp" || !strings.HasPrefix(filepath.Base(dir), "cj-") {
		return errors.New("refusing to clean unowned Jev session")
	}
	var failures []error
	for _, entry := range []struct{ path, marker string }{
		{filepath.Join(dir, "chrome.pid"), "--user-data-dir=" + filepath.Join(dir, "chrome")},
	} {
		if err := reapJevPID(entry.path, entry.marker); err != nil {
			failures = append(failures, err)
		}
	}
	runtimeDir := filepath.Join(dir, "runtime")
	entries, readErr := os.ReadDir(runtimeDir)
	if readErr != nil && !os.IsNotExist(readErr) {
		failures = append(failures, readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		if err := reapJevPID(filepath.Join(runtimeDir, entry.Name()), "browser_harness.daemon"); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	return os.RemoveAll(dir)
}

func reapJevPID(path, marker string) error {
	return reapJevPIDWith(path, marker, jevReaperOps{
		ps: func(pid int, field string) ([]byte, error) {
			return exec.Command("ps", "-p", strconv.Itoa(pid), "-o", field+"=").Output()
		},
		groupInspect: jevInspectGroup,
		kill:         syscall.Kill,
		pgid:         syscall.Getpgid,
		sleep:        time.Sleep,
	})
}

type jevReaperOps struct {
	ps           func(int, string) ([]byte, error)
	groupInspect func(int, string) (bool, bool, error)
	kill         func(int, syscall.Signal) error
	pgid         func(int) (int, error)
	sleep        func(time.Duration)
}

// A group leader can disappear while its descendants remain. ps reports
// zombies too, but they cannot execute and need not keep the session alive.
func jevInspectGroup(groupID int, marker string) (bool, bool, error) {
	output, err := exec.Command("ps", "-axo", "pid=,pgid=,stat=,command=").Output()
	if err != nil {
		return false, false, err
	}
	alive, matched := false, false
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return false, false, fmt.Errorf("malformed process-group inspection")
		}
		pgid, err := strconv.Atoi(fields[1])
		if err != nil {
			return false, false, fmt.Errorf("malformed process-group identity: %w", err)
		}
		if pgid == groupID && !strings.HasPrefix(fields[2], "Z") {
			alive = true
			matched = matched || strings.Contains(strings.Join(fields[3:], " "), marker)
		}
	}
	return alive, matched, nil
}

func jevLiveGroupMembers(groupID int) (bool, error) {
	alive, _, err := jevInspectGroup(groupID, "")
	return alive, err
}

func reapJevPIDWith(path, marker string, ops jevReaperOps) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	value := strings.TrimSpace(string(data))
	var record struct {
		PID    int    `json:"pid"`
		PGID   int    `json:"pgid"`
		Marker string `json:"marker"`
	}
	if strings.HasPrefix(value, "{") {
		if err := json.Unmarshal(data, &record); err != nil || record.PID <= 1 {
			return fmt.Errorf("invalid owned process binding in %s", path)
		}
		value = strconv.Itoa(record.PID)
	} else {
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return fmt.Errorf("empty owned process PID in %s", path)
		}
		value = fields[0]
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 1 {
		return fmt.Errorf("invalid owned process PID in %s", path)
	}
	boundGroup := record.PGID == pid && record.Marker == marker
	state, err := ops.ps(pid, "stat")
	if err != nil {
		if boundGroup {
			return reapAbsentJevGroup(pid, marker, ops)
		}
		return fmt.Errorf("inspect owned process %d state: %w", pid, err)
	}
	if strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
		if boundGroup {
			return reapAbsentJevGroup(pid, marker, ops)
		}
		return fmt.Errorf("owned process %d is a zombie; group ownership cannot be verified", pid)
	}
	command, err := ops.ps(pid, "command")
	if err != nil {
		if boundGroup {
			return reapAbsentJevGroup(pid, marker, ops)
		}
		return fmt.Errorf("inspect owned process %d command: %w", pid, err)
	}
	if len(command) == 0 {
		return fmt.Errorf("owned process %d has no observable command", pid)
	}
	if !strings.Contains(string(command), marker) {
		return fmt.Errorf("owned process %d identity no longer matches %q", pid, marker)
	}
	groupID, err := ops.pgid(pid)
	if errors.Is(err, syscall.ESRCH) {
		if boundGroup {
			return reapAbsentJevGroup(pid, marker, ops)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect owned process %d group: %w", pid, err)
	}
	target := pid
	if groupID == pid {
		target = -pid
	}
	if err := ops.kill(target, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate owned process %d: %w", pid, err)
	}
	for i := 0; i < 20; i++ {
		if gone, err := jevProcessGone(pid, groupID == pid, ops); err != nil {
			return err
		} else if gone {
			return nil
		}
		ops.sleep(100 * time.Millisecond)
	}
	if groupID == pid {
		// If the leader exited, the still-live group retains its ID. If that
		// PID was reused, refuse to signal a group we no longer own.
		if currentGroup, groupErr := ops.pgid(pid); groupErr == nil {
			if currentGroup != pid {
				return fmt.Errorf("owned process %d group changed before force stop", pid)
			}
			command, err = ops.ps(pid, "command")
			if err != nil || !strings.Contains(string(command), marker) {
				state, stateErr := ops.ps(pid, "stat")
				if stateErr != nil || !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
					return fmt.Errorf("owned process %d identity changed before force stop", pid)
				}
				// An unreaped zombie retains its PID and group, but ps no
				// longer exposes its original command. Recheck a live owned
				// descendant before signaling the still-owned group.
				alive, matched, inspectErr := ops.groupInspect(pid, marker)
				if inspectErr != nil {
					return fmt.Errorf("inspect zombie leader group %d: %w", pid, inspectErr)
				}
				if !alive {
					return nil
				}
				if !matched {
					return fmt.Errorf("zombie leader group %d lacks session identity", pid)
				}
			}
		} else if !errors.Is(groupErr, syscall.ESRCH) {
			return fmt.Errorf("recheck owned process %d group: %w", pid, groupErr)
		} else if _, matched, inspectErr := ops.groupInspect(pid, marker); inspectErr != nil || !matched {
			return fmt.Errorf("owned process %d group identity unverified before force stop: %v", pid, inspectErr)
		}
	} else {
		command, err = ops.ps(pid, "command")
		if err != nil || !strings.Contains(string(command), marker) {
			return fmt.Errorf("owned process %d identity changed before force stop", pid)
		}
	}
	if err := ops.kill(target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("force stop owned process %d: %w", pid, err)
	}
	for i := 0; i < 20; i++ {
		if gone, err := jevProcessGone(pid, groupID == pid, ops); err != nil {
			return err
		} else if gone {
			return nil
		}
		ops.sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("owned process %d survived SIGKILL", pid)
}

func jevProcessGone(pid int, ownedGroup bool, ops jevReaperOps) (bool, error) {
	if ownedGroup {
		if ops.groupInspect == nil {
			return false, errors.New("owned process-group inspection unavailable")
		}
		alive, _, err := ops.groupInspect(pid, "")
		if err != nil {
			return false, fmt.Errorf("inspect owned process group %d: %w", pid, err)
		}
		return !alive, nil
	}
	if err := ops.kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("probe owned process %d: %w", pid, err)
	}
	state, err := ops.ps(pid, "stat")
	if err != nil {
		return false, fmt.Errorf("inspect owned process %d state: %w", pid, err)
	}
	return strings.HasPrefix(strings.TrimSpace(string(state)), "Z"), nil
}

// Only a session-bound record plus a live matching member can authorize
// signaling a group whose leader was already reaped. A plain PID cannot.
func reapAbsentJevGroup(pid int, marker string, ops jevReaperOps) error {
	if ops.groupInspect == nil {
		return errors.New("owned process-group inspection unavailable")
	}
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		alive, matched, err := ops.groupInspect(pid, marker)
		if err != nil {
			return fmt.Errorf("inspect absent leader group %d: %w", pid, err)
		}
		if !alive {
			return nil
		}
		if !matched {
			return fmt.Errorf("absent leader group %d lacks session identity", pid)
		}
		if err := ops.kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stop absent leader group %d: %w", pid, err)
		}
		for i := 0; i < 20; i++ {
			if gone, err := jevProcessGone(pid, true, ops); err != nil {
				return err
			} else if gone {
				return nil
			}
			ops.sleep(100 * time.Millisecond)
		}
	}
	return fmt.Errorf("absent leader group %d survived SIGKILL", pid)
}
