package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

func TestContinuePreviewPublicDoorPreservesLifecycle(t *testing.T) {
	for _, state := range []string{"proposed", "ready", "claimed", "verifying", "stale_detached", "admission"} {
		t.Run(state, func(t *testing.T) {
			homePath := initializedExecutionHome(t)
			s, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Add(-time.Minute)
			id := "work_preview_" + state
			in := work.CreateInput{WorkID: id, Owner: "codex", Actor: "codex", State: "ready", SuccessPredicate: "preview leaves lifecycle unchanged"}
			if state == "proposed" || state == "admission" {
				in.State = "proposed"
			}
			if state == "admission" {
				_, err = s.CreatePlan(in, now, work.CreatePlanOptions{Record: true, Capability: "product.action_review", Action: "plan_work_from_reviewed_product_action", TargetKind: "product_action_review", TargetID: "preview_fixture", MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local"})
			} else {
				_, err = s.Create(in, now)
			}
			if err != nil {
				t.Fatal(err)
			}
			if state == "claimed" || state == "verifying" || state == "stale_detached" {
				if _, err := s.Claim(id, "codex", now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if state == "verifying" {
				if _, err := s.Advance(id, work.AdvanceInput{To: "running", Actor: "codex"}, now.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Advance(id, work.AdvanceInput{To: "verifying", Actor: "codex", VerificationID: "check:preview-fixture"}, now.Add(3*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if state == "stale_detached" {
				const missingProcessID = 1<<31 - 1
				if err := work.WriteDetachedLaunch(homePath, work.DetachedLaunch{WorkID: id, Actor: "codex", Lane: "fixture", PID: missingProcessID, PGID: missingProcessID, StartedAt: now.Add(-time.Hour).Format(time.RFC3339), Outcome: work.DetachedOutcomeRunning, LogPath: filepath.Join(t.TempDir(), "door.log")}); err != nil {
					t.Fatal(err)
				}
			}
			before := continueLifecycleSnapshot(t, homePath)
			for _, untilWait := range []bool{false, true} {
				args := []string{"work", "continue", "--actor", "codex", "--expect-work", id, "--max-execute-steps", "1", "--dry-run", "--json"}
				if untilWait {
					args = append(args, "--until-wait")
				}
				cmd := exec.Command(compoundTestBinary(t), args...)
				cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("preview: %v\n%s", err, out)
				}
				var result work.ContinueResult
				if untilWait {
					var run work.ContinueRunResult
					if err := json.Unmarshal(out, &run); err != nil {
						t.Fatalf("decode run: %v\n%s", err, out)
					}
					if len(run.Steps) != 1 || run.CanContinue {
						t.Fatalf("preview loop must stop after one observation: %+v", run)
					}
					result = run.Steps[0]
				} else if err := json.Unmarshal(out, &result); err != nil {
					t.Fatalf("decode: %v\n%s", err, out)
				}
				if result.Preview == nil || result.Preview.WorkID != id || result.Preview.Action == "none" || result.Action != "none" {
					t.Errorf("preview did not report exact proposed-only target: %+v", result)
				}
				if result.Preview != nil && (len(result.Preview.ChecksPerformed) == 0 || len(result.Preview.ChecksDeferred) == 0) {
					t.Error("preview omitted observation limits")
				}
				if result.Execute != nil && result.Execute.WouldExecute != "" {
					t.Errorf("missing grant reported would_execute: %+v", result.Execute)
				}
				after := continueLifecycleSnapshot(t, homePath)
				if !reflect.DeepEqual(before, after) {
					for path, digest := range after {
						if before[path] != digest {
							t.Errorf("preview changed lifecycle file %s (until-wait=%v)", path, untilWait)
						}
					}
					for path := range before {
						if _, ok := after[path]; !ok {
							t.Errorf("preview removed lifecycle file %s", path)
						}
					}
				}
			}
		})
	}
}

func TestContinueZeroExecuteBudgetRefusesWithoutMutation(t *testing.T) {
	homePath := initializedExecutionHome(t)
	before := continueLifecycleSnapshot(t, homePath)
	cmd := exec.Command(compoundTestBinary(t), "work", "continue", "--max-execute-steps", "0", "--dry-run", "--json")
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("zero budget accepted: %s", out)
	}
	if !reflect.DeepEqual(before, continueLifecycleSnapshot(t, homePath)) {
		t.Fatal("invalid zero budget mutated home")
	}
}

// Diagnostic Continue reports and lock files are not lifecycle authority. All
// other bytes (including events, leases, attempts and detached records) count.
func continueLifecycleSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(rel, ".lock") || strings.HasPrefix(rel, "continues/") || strings.HasPrefix(rel, "continue-runs/") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[rel] = fmt.Sprintf("%x", sha256.Sum256(body))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
