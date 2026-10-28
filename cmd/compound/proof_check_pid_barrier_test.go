package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestProofCheckPIDPublicationReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		missing bool
		wantPID int
		ready   bool
		wantErr bool
	}{
		{name: "not created", missing: true},
		{name: "created but empty"},
		{name: "whitespace only", body: " \n"},
		{name: "published", body: "123\n", wantPID: 123, ready: true},
		{name: "malformed", body: "not-a-pid", wantErr: true},
		{name: "zero", body: "0", wantErr: true},
		{name: "negative", body: "-1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pid")
			if !tc.missing {
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			pid, ready, err := readProofCheckTestPID(path)
			if (err != nil) != tc.wantErr || ready != tc.ready {
				t.Fatalf("PID readiness = (%d, %v, %v), want ready=%v error=%v", pid, ready, err, tc.ready, tc.wantErr)
			}
			if ready && pid != tc.wantPID {
				t.Fatalf("published PID = %d, want %d", pid, tc.wantPID)
			}
		})
	}
}

func TestProofCheckSealedPreparationEarlyExitCleansHelper(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child-pid")
	t.Setenv("PROOF_EARLY_CHILD_PID", pidPath)
	proofCheckInstallFakeGit(t, `#!/bin/sh
sleep 60 &
echo $! > "$PROOF_EARLY_CHILD_PID"
wait
`)
	var cancel context.CancelFunc
	var finished <-chan struct{}
	var childPID int
	// This outer cleanup bounds the RED experiment too; only this fixture's
	// context owns and cancels the fake git process group.
	t.Cleanup(func() {
		if cancel == nil {
			return
		}
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("outer cleanup could not join owned preparation")
		}
		if childPID > 0 {
			waitForProofCheckTestProcessExit(t, childPID)
		}
	})
	t.Run("returns before explicit cancellation", func(t *testing.T) {
		cancel, _, finished = startProofCheckSealedWorkspaceTest(t)
		childPID = waitForProofCheckTestPID(t, pidPath)
		// A return and t.Fatal both run the registered t.Cleanup stack.
	})
	select {
	case <-finished:
	default:
		t.Fatal("early exit left owned preparation running")
	}
	waitForProofCheckTestProcessExit(t, childPID)
}

func TestProofCheckProcessExitProbeRecognizesLiveProcess(t *testing.T) {
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	exited, err := proofCheckTestProcessExited(child.Process.Pid)
	if err != nil || exited {
		t.Fatalf("live owned child reported exited=%v error=%v", exited, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	reaped = true
	exited, err = proofCheckTestProcessExited(child.Process.Pid)
	if err != nil || !exited {
		t.Fatalf("reaped owned child reported exited=%v error=%v", exited, err)
	}
}
