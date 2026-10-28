package act

import (
	"bytes"
	"compound/internal/policy"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

//go:embed source_marker_retirement_program.py
var sourceMarkerRetirementProgram string

func SourceMarkerRetirementProgramSHA256() string {
	return runtimeConfigBytesHash([]byte(sourceMarkerRetirementProgram))
}

type sourceMarkerRetirementAdmissionGuard = runtimeConfigInstallAdmissionGuard
type sourceMarkerRetirementNativeTransport struct{}

func (sourceMarkerRetirementNativeTransport) Start(ctx context.Context, r policy.SourceMarkerRetirementRequest, _ map[string]string, operation string, deadline time.Time, guard sourceMarkerRetirementAdmissionGuard) (sourceMarkerRetirementChild, error) {
	if guard == nil {
		return nil, sourceMarkerRetirementError("native_guard_required")
	}
	// Reuse only the bounded SSH custody reader, never backup admission/effects.
	ssh := policy.BackupCredentialStageRequest{Resource: r.Resource, SSH: r.SSH}
	recheck, e := backupCredentialStageTransportPins(ssh)
	if e != nil {
		return nil, sourceMarkerRetirementError("ssh_custody")
	}
	args := runtimeConfigInstallSSHArgs(backupCredentialStageSSHRequest(ssh))
	args[len(args)-1] = "/usr/bin/sudo -n -- /usr/bin/python3 -I -B -c " + runtimeConfigInstallShellQuote(sourceMarkerRetirementProgram) + " " + runtimeConfigInstallShellQuote(SourceMarkerRetirementProgramSHA256())
	wire := struct {
		Request    policy.SourceMarkerRetirementRequest `json:"request"`
		Operation  string                               `json:"operation"`
		DeadlineNS string                               `json:"deadline_ns"`
	}{r, operation, strconv.FormatInt(deadline.UnixNano(), 10)}
	payload, e := json.Marshal(wire)
	if e != nil || len(payload) > 65536 {
		return nil, sourceMarkerRetirementError("transport_request")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "LANG=C"}
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(payload)
	child := &runtimeConfigInstallNativeChild{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &child.stdout
	cmd.Stderr = &child.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(e, syscall.ESRCH) {
			return nil
		}
		return e
	}
	started := false
	e = guard(func() error {
		if recheck() != nil || ctx.Err() != nil || !time.Now().Before(deadline) {
			return sourceMarkerRetirementError("transport_precondition")
		}
		if e = cmd.Start(); e != nil {
			return sourceMarkerRetirementError("transport_start")
		}
		started = true
		go func() { child.failed = cmd.Wait() != nil; close(child.done) }()
		return nil
	})
	if e != nil {
		if started {
			return child, e
		}
		return nil, e
	}
	return child, nil
}
