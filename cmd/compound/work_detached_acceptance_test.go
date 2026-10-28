package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
	"compound/internal/work"
	"compound/internal/work/executewire"
)

// The detached child is the freshly built Compound CLI. Only the enrolled
// Codex adapter's executable is synthetic; its governed lifecycle is real.
func TestDetachedGovernedDoorCompletesAndReconciles(t *testing.T) {
	if _, err := executewire.DetachedTimeoutBinary(); err != nil {
		t.Fatal(err)
	}
	compoundBinary := compoundTestBinary(t)
	for _, exitCode := range []int{0, 23} {
		t.Run(fmt.Sprintf("adapter_exit_%d", exitCode), func(t *testing.T) {
			homePath := initializedExecutionHome(t)
			workspace, base, _ := initLandingCLIRepo(t)
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			w, err := store.Create(work.CreateInput{
				Owner: "codex", SuccessPredicate: "synthetic detached adapter has one terminal attempt",
				DesiredOutcome: "verify detached goal delivery",
				GitDelivery: &envelope.GitDeliveryContract{
					TargetWorktreeRoot: workspace, TargetLocalRef: "refs/heads/main",
					ExpectedBaseSHA: base, PublicationMode: "local_only",
				},
			}, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(w.ID, "codex", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			binDir := t.TempDir()
			fakeCodex := filepath.Join(binDir, "codex")
			script := fmt.Sprintf("#!/bin/sh\nprintf 'synthetic adapter executable: %%s\\n' \"$0\"\nprintf 'synthetic adapter consumed: %%s\\n' \"$*\"\nexit %d\n", exitCode)
			if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			previousDoor, previousBudget := executewire.DetachedDoorBinary, executewire.DefaultDetachedLaunchBudget
			wasInstalled := executewire.AssertLaunchContextInstalled() == nil
			executewire.DetachedDoorBinary = func() (string, error) { return compoundBinary, nil }
			executewire.DefaultDetachedLaunchBudget = 30 * time.Second
			executewire.SetLaunchContextInstalled(true)
			t.Cleanup(func() {
				executewire.DetachedDoorBinary, executewire.DefaultDetachedLaunchBudget = previousDoor, previousBudget
				executewire.SetLaunchContextInstalled(wasInstalled)
			})
			launch, err := executewire.LaunchDetachedViaDoor(store, w.ID, "codex", "detached acceptance prompt", "codex", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			joined := false
			done := make(chan error, 1)
			go func() {
				var status syscall.WaitStatus
				_, err := syscall.Wait4(launch.PID, &status, 0, nil)
				done <- err
			}()
			t.Cleanup(func() {
				if !joined {
					_ = syscall.Kill(-launch.PGID, syscall.SIGKILL)
					<-done
				}
			})
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Fatalf("join detached door: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("actual governed door did not terminate within fixture bound")
			}
			logBody, err := os.ReadFile(launch.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			final, err := fresh.Get(w.ID)
			wantState, wantStatus := "verifying", runattempt.StatusSucceeded
			if exitCode != 0 {
				wantState, wantStatus = "failed", runattempt.StatusFailed
			}
			if err != nil || final.State != wantState || len(final.RunAttemptIDs) != 2 {
				t.Fatalf("governed door did not finalize real lifecycle: %+v err=%v\n%s", final, err, logBody)
			}
			attempt, err := runattempt.Open(homePath).Get(final.RunAttemptIDs[1])
			if err != nil || attempt.Status != wantStatus || attempt.WorkID != w.ID || attempt.Command != "codex" || attempt.TranscriptRef == "" {
				t.Fatalf("actual adapter did not leave bound terminal evidence: %+v err=%v\n%s", attempt, err, logBody)
			}
			transcript, err := os.ReadFile(attempt.TranscriptRef)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"synthetic adapter executable: " + fakeCodex,
				"synthetic adapter consumed: exec # Current governed work " + w.ID,
				"stdout: ## Desired outcome\nstdout: verify detached goal delivery",
				"stdout: ## Success predicate\nstdout: synthetic detached adapter has one terminal attempt",
				"stdout: ## Latest explicit request\nstdout: detached acceptance prompt",
			} {
				if !strings.Contains(string(transcript), want) {
					t.Fatalf("actual adapter transcript does not prove governed prompt delivery; missing %q:\n%s", want, transcript)
				}
			}
			if !strings.Contains(string(logBody), "run_attempt_id: "+attempt.ID) || !strings.Contains(string(logBody), "status: "+wantStatus) {
				t.Fatalf("actual governed door did not report its terminal attempt:\n%s", logBody)
			}
			beforeBody, _ := json.Marshal(final)
			got := fresh.ReconcileDetachedLaunches(time.Now().UTC())
			if len(got) != 1 || got[0].Outcome != work.DetachedOutcomeDoorFinalized {
				t.Fatalf("completed governed door was not finalized by recovery: %+v", got)
			}
			after, err := fresh.Get(w.ID)
			afterBody, _ := json.Marshal(after)
			if err != nil || !bytes.Equal(beforeBody, afterBody) {
				t.Fatalf("reconciliation rewrote finalized lifecycle: %+v err=%v", after, err)
			}
			if again := fresh.ReconcileDetachedLaunches(time.Now().UTC()); len(again) != 0 {
				t.Fatalf("finalized door was handled twice: %+v", again)
			}
		})
	}
}
