package act

import (
	"bytes"
	"compound/internal/policy"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed credential_password_align_program.py
var credentialPasswordAlignmentExtension string

func credentialPasswordAlignmentProgram() string {
	i := strings.LastIndex(credentialVerificationProgram, "if __name__ == \"__main__\":")
	if i < 0 {
		panic("credential alignment embedding contract")
	}
	return credentialVerificationProgram[:i] + "\n" + credentialPasswordAlignmentExtension
}
func CredentialPasswordAlignmentProgramSHA256() string {
	sum := sha256.Sum256([]byte(credentialPasswordAlignmentProgram()))
	return hex.EncodeToString(sum[:])
}

type credentialPasswordAlignAdmissionGuard = credentialVerifyAdmissionGuard
type credentialPasswordAlignNativeTransport struct{}

type credentialPasswordAlignStageError struct{ stage string }

func (e credentialPasswordAlignStageError) Error() string {
	return "credential_password_align:operation_failed"
}

func credentialPasswordAlignKnownFailureStage(stage string) bool {
	switch stage {
	case "request_validation", "runtime_preconditions", "inspection_negative_auth",
		"inspection_session", "inspection_catalog_read", "inspection_catalog_shape",
		"inspection_catalog_scram", "inspection_closed_state", "inspection_base_catalog",
		"inspection_postchecks", "password_operation", "deadline", "cleanup", "observation_encoding":
		return true
	}
	return false
}

type credentialPasswordAlignNativeChild struct{ *credentialVerifyNativeChild }

func (c *credentialPasswordAlignNativeChild) Wait() ([]byte, error) {
	raw, err := c.credentialVerifyNativeChild.Wait()
	if err == nil || !c.failed {
		return raw, err
	}
	c.stdout.mu.Lock()
	empty := !c.stdout.overflow && len(c.stdout.data) == 0
	c.stdout.mu.Unlock()
	c.stderr.mu.Lock()
	defer c.stderr.mu.Unlock()
	const prefix = "credential_password_align:refused:"
	marker := string(c.stderr.data)
	stage := strings.TrimSuffix(strings.TrimPrefix(marker, prefix), "\n")
	if empty && !c.stderr.invalid && credentialPasswordAlignKnownFailureStage(stage) && marker == prefix+stage+"\n" {
		return nil, credentialPasswordAlignStageError{stage: stage}
	}
	return nil, err
}

func credentialPasswordAlignOriginalAction(r policy.CredentialPasswordAlignmentRequest, action string) string {
	if r.Origin != nil {
		return r.Origin.ActionID
	}
	return action
}
func (t credentialPasswordAlignNativeTransport) Start(ctx context.Context, r policy.CredentialPasswordAlignmentRequest, action string, deadline time.Time, guard credentialPasswordAlignAdmissionGuard) (credentialPasswordAlignChild, error) {
	if ctx.Err() != nil || !time.Now().Before(deadline) || guard == nil {
		return nil, credentialVerifyError("transport_deadline")
	}
	recheck, err := credentialVerifyTransportPreflight(r.CredentialVerificationRequest)
	if err != nil {
		return nil, err
	}
	wire := struct {
		Action         string                                    `json:"action_intent_id"`
		OriginalAction string                                    `json:"original_action_intent_id"`
		Request        policy.CredentialPasswordAlignmentRequest `json:"request"`
		IssuedNS       string                                    `json:"issued_ns"`
		DeadlineNS     string                                    `json:"deadline_ns"`
	}{Action: action, OriginalAction: credentialPasswordAlignOriginalAction(r, action), Request: r, IssuedNS: strconv.FormatInt(time.Now().UnixNano(), 10), DeadlineNS: strconv.FormatInt(deadline.UnixNano(), 10)}
	payload, err := json.Marshal(wire)
	if err != nil || len(payload) > 32768 {
		return nil, credentialVerifyError("transport_request")
	}
	args := credentialVerifySSHArgs(r.CredentialVerificationRequest)
	args[len(args)-1] = "/usr/bin/sudo -n -- /usr/bin/python3 -I -B -c " + credentialVerifyShellQuote(credentialPasswordAlignmentProgram()) + " " + credentialVerifyShellQuote(CredentialPasswordAlignmentProgramSHA256())
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "LANG=C"}
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(payload)
	child := &credentialPasswordAlignNativeChild{&credentialVerifyNativeChild{cmd: cmd, done: make(chan struct{})}}
	cmd.Stdout = &child.stdout
	// Only a finite singleton marker can become failed-only metadata. Raw remote
	// diagnostics never become an error message, observation, or artifact.
	cmd.Stderr = &child.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	started := false
	// The owner admits this exact start callback while holding native admission.
	// Reap immediately after successful Start, including later evidence failure.
	err = guard(func() error {
		if err := recheck(); err != nil {
			return err
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return credentialVerifyError("transport_deadline")
		}
		if err := cmd.Start(); err != nil {
			return credentialVerifyError("transport_start")
		}
		started = true
		go func() { child.failed = cmd.Wait() != nil; close(child.done) }()
		return nil
	})
	if err != nil {
		if started {
			return child, credentialVerifyError("transport_launch")
		}
		return nil, err
	}
	return child, nil
}
