package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/readiness"
	"compound/internal/work"
)

// TestRunHarnessReadinessUnreadyPrintsRepair covers the read-only readiness
// face R3B 2026-09-04 introduced: --harness without a work id must
// surface Ready=false, the readiness details, and per-category repair
// commands — same vocabulary the execute-time refusal uses, so an
// operator can ask "can this lane run anything right now?" without
// claiming work first.
func TestRunHarnessReadinessUnreadyPrintsRepair(t *testing.T) {
	stubHarnessReadinessResolver(t)
	homePath := initializedExecutionHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{
			Ready: false,
			Details: []string{
				"permission_posture: managed settings differ from authority",
				"blocking_hook_live: mutation-covering binding status=drifted",
				"instruction_pack: work has no context_pack_id",
			},
		}, nil
	})
	var out, errOut bytes.Buffer
	code := runHarnessReadiness([]string{"--harness", "claude", "--workspace", t.TempDir()}, &out, &errOut)
	if code == 0 {
		t.Fatalf("unready readiness face returned 0; expected non-zero:\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
	body := out.String()
	if !strings.Contains(body, "status: unready") {
		t.Fatalf("readiness face did not print status: unready:\n%s", body)
	}
	for _, want := range []string{
		"permission_posture: managed settings differ from authority",
		"blocking_hook_live: mutation-covering binding status=drifted",
		"instruction_pack: work has no context_pack_id",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("readiness face omitted detail %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "compound harness readiness --harness claude") {
		t.Fatalf("readiness face did not include the self-referential repair line:\n%s", body)
	}
	for _, want := range []string{
		"compound harness permission-mode --harness claude",
		"compound hook install --harness claude",
		"compound harness instruction-pack --harness claude",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("readiness face did not include repair line %q:\n%s", want, body)
		}
	}
}

// TestRunHarnessReadinessJSONShape pins the JSON output the doctor
// harness section and dashboards consume. Every repair command line is
// a copy of the cmd-line, not a description.
func TestRunHarnessReadinessJSONShape(t *testing.T) {
	stubHarnessReadinessResolver(t)
	homePath := initializedExecutionHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{
			Ready:   false,
			Details: []string{"permission_posture: managed settings differ from authority"},
		}, nil
	})
	var out, errOut bytes.Buffer
	code := runHarnessReadiness([]string{"--harness", "claude", "--json"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("unready readiness JSON returned 0:\n%s", out.String())
	}
	var rep struct {
		Harness        string   `json:"harness"`
		Workspace      string   `json:"workspace,omitempty"`
		Ready          bool     `json:"ready"`
		Details        []string `json:"details"`
		RepairCommands []string `json:"repair_commands"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode readiness JSON: %v\n%s", err, out.String())
	}
	if rep.Harness != "claude" || rep.Ready {
		t.Fatalf("readiness JSON shape = %+v", rep)
	}
	if len(rep.Details) != 1 || rep.Details[0] != "permission_posture: managed settings differ from authority" {
		t.Fatalf("readiness JSON details = %v", rep.Details)
	}
	if !readinessRepairListHasPrefix(rep.RepairCommands, "compound harness permission-mode --harness claude") {
		t.Fatalf("readiness JSON repair_commands = %v", rep.RepairCommands)
	}
}

// TestRunHarnessReadinessReadyExitsZero pins that a Ready=true evaluation
// exits 0; the unready cases above exit 1 (with details + repair).
func TestRunHarnessReadinessReadyExitsZero(t *testing.T) {
	stubHarnessReadinessResolver(t)
	homePath := initializedExecutionHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	})
	var out, errOut bytes.Buffer
	code := runHarnessReadiness([]string{"--harness", "claude"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("ready readiness face returned %d; want 0:\nstdout=%s\nstderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "status: ready") {
		t.Fatalf("ready readiness face did not print status: ready:\n%s", out.String())
	}
}

// TestRunHarnessReadinessUsageMissingHarness pins the usage guard; it
// keeps the F3 falsifier contract for the new verb.
func TestRunHarnessReadinessUsageMissingHarness(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runHarnessReadiness(nil, &out, &errOut)
	if code != 2 {
		t.Fatalf("usage guard returned %d; want 2:\nstderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "usage: compound harness readiness") {
		t.Fatalf("usage guard stderr = %q; want usage line", errOut.String())
	}
}

// TestEvaluateForHarnessCallsRegisteredGuard makes the thin wrapper's
// contract explicit: it builds the request through the registered seam,
// so the cmd/compound face and the harness readiness face evaluate the
// SAME logic (and a missing guard fails closed).
func TestEvaluateForHarnessCallsRegisteredGuard(t *testing.T) {
	stubHarnessReadinessResolver(t)
	homePath := initializedExecutionHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	stubDelegateHarnessReadiness(t, func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		if req.Harness != "claude" {
			t.Fatalf("EvaluateForHarness harness = %q; want claude", req.Harness)
		}
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	})
	result, err := readiness.EvaluateForHarness(homePath, "claude", t.TempDir(), "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready {
		t.Fatalf("EvaluateForHarness Ready=false; want true")
	}
}

// TestEvaluateForHarnessMissingGuard pins the fail-closed path so a
// forgotten init() in a test harness (or a future binary that forgets
// to install the seam) cannot silently report Ready=true.
func TestEvaluateForHarnessMissingGuard(t *testing.T) {
	original := work.DelegateHarnessReadiness
	work.DelegateHarnessReadiness = nil
	t.Cleanup(func() { work.DelegateHarnessReadiness = original })
	if _, err := readiness.EvaluateForHarness(t.TempDir(), "claude", "", "", cliTestNow); err == nil {
		t.Fatalf("missing readiness guard returned no error; want fail-closed")
	}
}

func stubHarnessReadinessResolver(t *testing.T) {
	t.Helper()
	original := work.DelegateHarnessResolver
	work.DelegateHarnessResolver = func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		return work.DelegateHarnessResolution{Harness: req.Harness, Command: "/bin/echo", LaneID: "fixture", RequestedModelID: "fixture", RequestedProviderRoute: "fixture"}, nil
	}
	t.Cleanup(func() { work.DelegateHarnessResolver = original })
}

func TestHarnessReadinessBindsRealTupleAndWorkContext(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixtureAt(t, "work_readiness_cli", time.Now().UTC())
	t.Setenv("COMPOUND_HOME", fixture.compoundHome)
	store, err := work.Open(fixture.compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID: fixture.workID, Owner: "fixture", Actor: "fixture", State: "ready",
		SuccessPredicate: "readiness binds the exact work context", ContextPackID: fixture.contextPackID,
	}, fixture.now); err != nil {
		t.Fatal(err)
	}
	before := snapshotFiles(t, fixture.compoundHome)
	var out, errOut bytes.Buffer
	code := runHarnessReadiness([]string{"--harness", fixture.harness, "--workspace", fixture.workspace, "--work", fixture.workID, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("bound readiness exited %d: stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	var report struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Ready {
		t.Fatalf("real tuple and work context not ready: %s", out.String())
	}
	if after := snapshotFiles(t, fixture.compoundHome); !reflect.DeepEqual(before, after) {
		t.Fatal("readiness mutated home")
	}

	withoutWork, err := readiness.EvaluateForHarness(fixture.compoundHome, fixture.harness, fixture.workspace, "", fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if withoutWork.Ready || !strings.Contains(strings.Join(withoutWork.Details, "\n"), "work has no context_pack_id") {
		t.Fatalf("unbound readiness bypassed context guard: %+v", withoutWork)
	}
	if _, err := readiness.EvaluateForHarness(fixture.compoundHome, fixture.harness, fixture.workspace, "work_missing", fixture.now); err == nil {
		t.Fatal("nonexistent work did not fail closed")
	}
	if _, err := store.Create(work.CreateInput{WorkID: "work_wrong_context", Owner: "fixture", Actor: "fixture", State: "ready", SuccessPredicate: "wrong work is refused", ContextPackID: fixture.contextPackID}, fixture.now); err != nil {
		t.Fatal(err)
	}
	mismatch, err := readiness.EvaluateForHarness(fixture.compoundHome, fixture.harness, fixture.workspace, "work_wrong_context", fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch.Ready || !strings.Contains(strings.Join(mismatch.Details, "\n"), "work identity does not match") {
		t.Fatalf("wrong work accepted: %+v", mismatch)
	}
	if err := os.Remove(filepath.Join(fixture.compoundHome, "registries", "model-capabilities.yaml")); err != nil {
		t.Fatal(err)
	}
	missingTuple, err := readiness.EvaluateForHarness(fixture.compoundHome, fixture.harness, fixture.workspace, fixture.workID, fixture.now)
	if err == nil && missingTuple.Ready {
		t.Fatal("missing tuple reported ready")
	}
	before = snapshotFiles(t, fixture.compoundHome)
	_, _ = readiness.EvaluateForHarness(fixture.compoundHome, fixture.harness, fixture.workspace, fixture.workID, fixture.now)
	if after := snapshotFiles(t, fixture.compoundHome); !reflect.DeepEqual(before, after) {
		t.Fatal("failed readiness mutated home")
	}
}

func TestHarnessReadinessRejectsTupleRotationWithoutWriting(t *testing.T) {
	for _, change := range []string{"unchanged", "model", "provider", "command", "harness", "exhausted_to_available"} {
		t.Run(change, func(t *testing.T) {
			fixture := buildReadyDelegateHarnessFixture(t, "work_tuple_rotation")
			store, err := work.Open(fixture.compoundHome)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(work.CreateInput{
				WorkID: fixture.workID, Owner: "fixture", Actor: "fixture", State: "ready",
				SuccessPredicate: "readiness rejects tuple drift", ContextPackID: fixture.contextPackID,
			}, fixture.now); err != nil {
				t.Fatal(err)
			}
			if change == "exhausted_to_available" {
				writeDelegateCapacityManifest(t, fixture, true)
				usage := filepath.Join(fixture.harnessHome, "Library", "Application Support", "CodexBar", "usage-history.jsonl")
				body, err := json.Marshal(map[string]any{
					"provider": "zai", "windowKind": "5h", "windowMinutes": 300, "usedPercent": 100,
					"resetsAt": fixture.now.Add(2 * time.Hour).Format(time.RFC3339), "sampledAt": fixture.now.Format(time.RFC3339),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(usage), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(usage, append(body, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				unready, err := readiness.EvaluateForHarness(fixture.compoundHome, fixture.harness, fixture.workspace, fixture.workID, fixture.now)
				if err != nil || unready.Ready || !strings.Contains(strings.Join(unready.Details, " "), "capacity walled") {
					t.Fatalf("fixture A must be exhausted before rotation: result=%+v err=%v", unready, err)
				}
			}
			original := work.DelegateHarnessResolver
			t.Cleanup(func() { work.DelegateHarnessResolver = original })
			var before, harnessBefore map[string]string
			calls := 0
			work.DelegateHarnessResolver = func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
				resolved, err := original(req)
				if err != nil {
					return resolved, err
				}
				calls++
				// Rotate after the real resolver selected A, before the real adapter reads B.
				// This fixture mutation is not part of the read-only preview being measured.
				path := filepath.Join(fixture.compoundHome, "registries", "model-capabilities.yaml")
				var manifest map[string]any
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(body, &manifest); err != nil {
					t.Fatal(err)
				}
				models := manifest["models"].(map[string]any)
				model := models[resolved.RequestedModelID].(map[string]any)
				switch change {
				case "model", "exhausted_to_available":
					delete(models, resolved.RequestedModelID)
					models["fixture-next"] = model
					manifest["routing_defaults"] = map[string]any{"build-implement": []string{"fixture-next"}}
					if change == "exhausted_to_available" {
						model["provider_route"] = "fixture-route"
						delete(model, "capacity_source")
					}
				case "provider":
					model["provider_route"] = "fixture-route"
				case "command":
					model["aliases"] = []string{"/bin/sh"}
				case "harness":
					model["harness"] = "claude_minimax"
				}
				if change != "unchanged" {
					writeDelegateReadinessJSON(t, path, manifest)
				}
				before = snapshotFiles(t, fixture.compoundHome)
				harnessBefore = snapshotFiles(t, fixture.harnessHome)
				return resolved, nil
			}
			got, err := readiness.EvaluateForHarness(fixture.compoundHome, fixture.harness, fixture.workspace, fixture.workID, fixture.now)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("resolver calls=%d; want exactly one selected tuple", calls)
			}
			if after := snapshotFiles(t, fixture.compoundHome); !reflect.DeepEqual(before, after) {
				t.Fatal("readiness wrote home state, attempts, or refusal evidence")
			}
			if after := snapshotFiles(t, fixture.harnessHome); !reflect.DeepEqual(harnessBefore, after) {
				t.Fatal("readiness mutated harness authority")
			}
			if change == "unchanged" {
				if !got.Ready {
					t.Fatalf("unchanged tuple must be ready: %+v", got)
				}
			} else if change == "harness" {
				// An absent formerly selected harness already fails closed at resolution.
				if got.Ready || !strings.Contains(strings.Join(got.Details, " "), "adapter_preflight:") {
					t.Fatalf("rotated harness must be unready: %+v", got)
				}
			} else if got.Ready || !strings.Contains(strings.Join(got.Details, " "), "lane tuple changed") {
				t.Fatalf("rotated %s tuple must be unready with tuple-drift detail: %+v", change, got)
			}
		})
	}
}

func TestReadOnlyLaunchReadinessPreservesRawClaudeNormalization(t *testing.T) {
	home := t.TempDir()
	before := snapshotFiles(t, home)
	if details := harness.ReadOnlyLaunchReadiness(home, harness.RunInput{
		Harness: "claude", Command: "/bin/echo", PermissionMode: "plan", Now: cliTestNow,
	}); len(details) != 0 {
		t.Fatalf("raw Claude command normalization is not tuple drift: %v", details)
	}
	if after := snapshotFiles(t, home); !reflect.DeepEqual(before, after) {
		t.Fatal("raw Claude readiness mutated home")
	}
}

func TestHarnessReadinessMissingResolverFailsClosed(t *testing.T) {
	original := work.DelegateHarnessResolver
	work.DelegateHarnessResolver = nil
	t.Cleanup(func() { work.DelegateHarnessResolver = original })
	result, err := readiness.EvaluateForHarness(t.TempDir(), "claude", "", "", cliTestNow)
	if err == nil && result.Ready {
		t.Fatal("missing resolver reported ready")
	}
}

func readinessRepairListContains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func readinessRepairListHasPrefix(values []string, prefix string) bool {
	for _, v := range values {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// Suppress unused import warnings if all subcommand tests get trimmed.
var _ = exec.Command
var _ = filepath.Join
