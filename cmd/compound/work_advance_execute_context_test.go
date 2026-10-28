package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/contract"
	"compound/internal/envelope"
)

// Include unlinked events, policies, actions and attempts, not just envelope IDs.
func executeContextHomeSnapshot(t *testing.T, home string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		files[relative] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func executeContextPack(t *testing.T, fixture claimedExecutionFixture, label string) string {
	t.Helper()
	now := time.Now().UTC()
	task := envelope.TaskContract{
		Intent: "attach synthetic context " + label, Mode: "execute",
		DesiredOutcome: "current context is persisted", SuccessPredicate: "same-state attachment preserves state",
	}
	effective := contract.Compile(contract.Input{
		Harness: "codex", Cwd: t.TempDir(), TaskContract: task,
	}, contract.DefaultPolicy(), now)
	pack := validCLIContextPack()
	pack.CreatedAt = now.Format(time.RFC3339)
	pack.IDs = envelope.IDs{WorkID: fixture.workID, ContractID: effective.ID}
	pack.TaskContract = task
	runtime, err := contract.SealRuntimeMaterialization(contract.RuntimeMaterialization{
		EffectiveContract: effective, ContextPack: pack,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.RecordRuntimeMaterialization(fixture.homePath, runtime); err != nil {
		t.Fatal(err)
	}
	return runtime.ContextPack.ID
}

func TestWorkAdvanceExecuteRejectsParentContextBeforeEffects(t *testing.T) {
	for _, state := range []string{"claimed", "verifying"} {
		t.Run(state, func(t *testing.T) {
			fixture := newResumeFixture(t, "parent context is never silently discarded")
			contextID := executeContextPack(t, fixture, "existing")
			captureStdout(t, func() {
				cmdWork([]string{"advance", fixture.workID, "--to", "claimed", "--by", "codex", "--context-pack", contextID})
			})
			if state == "verifying" {
				out, code := runExecuteCapturingExit(t, []string{
					"advance", fixture.workID, "--to", "running", "--by", "codex", "--execute",
					"--harness", "os-harness-entrypoints", "--workspace", t.TempDir(), "--", "sh", "-c", "true",
				})
				if code != 0 {
					t.Fatalf("prepare verifying state: exit=%d\n%s", code, out)
				}
			}
			for _, supplied := range []struct{ name, value string }{
				{"same ID", contextID}, {"nonexistent", "ctx_missing"}, {"malformed", "not-a-context"}, {"empty", ""},
			} {
				t.Run(supplied.name, func(t *testing.T) {
					marker := filepath.Join(t.TempDir(), "child-launched")
					before := executeContextHomeSnapshot(t, fixture.homePath)
					args := []string{"advance", fixture.workID, "--to", "running", "--by", "codex", "--execute",
						"--harness", "os-harness-entrypoints", "--context-pack", supplied.value}
					if state == "verifying" {
						args = append(args, "--resume-verifying")
					}
					args = append(args, "--", "sh", "-c", `printf launched > "$1"`, "child", marker)
					var code int
					stderr := captureStderr(t, func() { _, code = runExecuteCapturingExit(t, args) })
					if code != 2 {
						t.Errorf("exit=%d, want usage exit 2; stderr=%s", code, stderr)
					}
					for _, want := range []string{"--context-pack cannot be combined with --execute", "separate same-state advance", "--to " + state, fixture.workID} {
						if !strings.Contains(stderr, want) {
							t.Errorf("missing guidance %q in %s", want, stderr)
						}
					}
					if after := executeContextHomeSnapshot(t, fixture.homePath); !reflect.DeepEqual(before, after) {
						t.Error("parent context refusal changed durable home bodies or their inventory")
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatalf("child marker must be absent: %v", err)
					}
				})
			}
		})
	}
}

func TestWorkAdvanceExecuteChildContextArgumentPassthrough(t *testing.T) {
	fixture := newResumeFixture(t, "child owns context arguments after separator")
	args := []string{"advance", fixture.workID, "--to", "running", "--by", "codex", "--execute",
		"--harness", "os-harness-entrypoints", "--workspace", t.TempDir(), "--", "sh", "-c", `printf '<%s>\n' "$@"`, "child",
		"--context-pack", "ctx_child with spaces", "--context-pack", "", "--", "--resume-verifying"}
	out, code := runExecuteCapturingExit(t, args)
	if code != 0 {
		t.Fatalf("child context flag refused: exit=%d\n%s", code, out)
	}
	attempt := fixture.executionAttempt(t)
	body, err := os.ReadFile(attempt.TranscriptRef)
	if err != nil {
		t.Fatal(err)
	}
	want := "stdout: <--context-pack>\nstdout: <ctx_child with spaces>\nstdout: <--context-pack>\nstdout: <>\nstdout: <-->\nstdout: <--resume-verifying>"
	if strings.TrimSpace(string(body)) != want {
		t.Fatalf("child arguments changed; want %q in transcript:\n%s", want, body)
	}
}

func TestWorkAdvanceSameStateContextAttachment(t *testing.T) {
	for _, state := range []string{"claimed", "verifying"} {
		t.Run(state, func(t *testing.T) {
			fixture := newResumeFixture(t, "context attachment remains an explicit same-state advance")
			if state == "verifying" {
				out, code := runExecuteCapturingExit(t, []string{
					"advance", fixture.workID, "--to", "running", "--by", "codex", "--execute",
					"--harness", "os-harness-entrypoints", "--workspace", t.TempDir(), "--", "sh", "-c", "true",
				})
				if code != 0 {
					t.Fatalf("prepare verifying state: exit=%d\n%s", code, out)
				}
			}
			for _, label := range []string{"original", "replacement"} {
				contextID := executeContextPack(t, fixture, label)
				before, err := fixture.store.Get(fixture.workID)
				if err != nil {
					t.Fatal(err)
				}
				captureStdout(t, func() {
					cmdWork([]string{"advance", fixture.workID, "--to", before.State, "--by", "codex", "--context-pack", contextID})
				})
				after, err := fixture.store.Get(fixture.workID)
				if err != nil {
					t.Fatal(err)
				}
				if after.State != before.State || after.IDs.ContextPackID != contextID || after.IDs.ContextPackID == before.IDs.ContextPackID {
					t.Fatalf("same-state context attachment did not replace context: before=%+v after=%+v", before, after)
				}
			}
		})
	}
}
