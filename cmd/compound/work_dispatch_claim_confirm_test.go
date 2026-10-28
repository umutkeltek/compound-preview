package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

const dispatchClaimConfirmChildEnv = "COMPOUND_DISPATCH_CLAIM_CONFIRM_CHILD"

func isolateDispatchCapacityGuard(t *testing.T) {
	t.Helper()
	previous := work.LaneCapacityGuard
	work.LaneCapacityGuard = nil
	t.Cleanup(func() { work.LaneCapacityGuard = previous })
}

func TestWorkDispatchClaimCLIChild(t *testing.T) {
	mode := os.Getenv(dispatchClaimConfirmChildEnv)
	if mode == "" {
		t.Skip("subprocess helper")
	}
	// CLI boundary tests must not inherit the operator's live CodexBar/quota state.
	// A confirmed command still exercises the complete claim mutation below this adapter.
	if mode == "walled" {
		work.LaneCapacityGuard = routeevidence.WorkLaneCapacityGuard(routeevidence.LaneCapacityConfig{
			Paths: routeevidence.LaneCapacityPaths{UsageHistoryPath: os.Getenv("COMPOUND_TEST_CAPACITY_PATH")},
		})
	} else {
		work.LaneCapacityGuard = nil
	}
	args := []string{"dispatch-claim", "--actor", "codex", "--json"}
	switch mode {
	case "argless":
	case "next":
		args = append(args, "--next")
	case "explicit":
		args = append(args, os.Getenv("COMPOUND_TEST_WORK_ID"))
	case "confirm":
		args = append(args, "--next", "--confirm")
	case "both":
		args = append(args, "--next", os.Getenv("COMPOUND_TEST_WORK_ID"))
	case "help-example":
		example := strings.Fields(workVerbHelpByName["dispatch-claim"].Example)
		if len(example) < 3 || strings.Join(example[:3], " ") != "compound work dispatch-claim" {
			t.Fatalf("dispatch-claim help example is not a work command: %q", example)
		}
		args = example[2:]
	case "missing-actor-value":
		args = []string{"dispatch-claim", "--next", "--actor", "--confirm", "--json"}
	case "walled":
		args = append(args, "--next")
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
	cmdWork(args)
	// Successful JSON commands return normally; exit here so the go test child does not append
	// its own PASS line to the CLI bytes being asserted by the parent process.
	os.Exit(0)
}

func TestWorkDispatchClaimUnconfirmedPreviewsWithoutMutating(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"argless", "next", "explicit", "help-example"} {
		t.Run(target, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatalf("init home: %v", err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatalf("open work: %v", err)
			}
			ready, err := store.Create(work.CreateInput{
				Owner:            "compound-core",
				SuccessPredicate: "unconfirmed dispatch-claim previews without mutation",
			}, cliTestNow)
			if err != nil {
				t.Fatalf("create work: %v", err)
			}
			before := snapshotDispatchClaimHome(t, homePath)

			output, runErr := runDispatchClaimChild(t, target, homePath, ready.ID, "")
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("unconfirmed %s dispatch-claim exit = %v, want 1\n%s", target, runErr, output)
			}
			var preview work.DispatchPlan
			if err := json.Unmarshal(output, &preview); err != nil {
				t.Fatalf("parse %s preview: %v\n%s", target, err, output)
			}
			if preview.Selected == nil || preview.Selected.WorkID != ready.ID {
				t.Fatalf("%s preview selected %+v, want %s", target, preview.Selected, ready.ID)
			}
			if preview.Actor != "codex" {
				t.Fatalf("%s preview actor = %q, want codex", target, preview.Actor)
			}

			after, err := store.Get(ready.ID)
			if err != nil {
				t.Fatalf("get work after preview: %v", err)
			}
			if after.State != "ready" {
				t.Fatalf("unconfirmed %s preview changed state to %s", target, after.State)
			}
			if afterSnapshot := snapshotDispatchClaimHome(t, homePath); !reflect.DeepEqual(before, afterSnapshot) {
				t.Fatalf("home mutated by unconfirmed %s dispatch-claim:\nbefore=%+v\nafter=%+v", target, before, afterSnapshot)
			}
		})
	}
}

func TestWorkDispatchClaimConfirmPerformsMutation(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "confirmed dispatch-claim performs the claim",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}

	output, runErr := runDispatchClaimChild(t, "confirm", homePath, ready.ID, "")
	if runErr != nil {
		t.Fatalf("confirmed dispatch-claim failed: %v\n%s", runErr, output)
	}
	var result work.DispatchClaimResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("parse confirmed result: %v\n%s", err, output)
	}
	if result.ClaimedWorkID != ready.ID || result.Status != "claimed" {
		t.Fatalf("confirmed result = %+v, want claimed %s", result, ready.ID)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get work after confirmed claim: %v", err)
	}
	if after.State != "claimed" || after.Authority.Owner != "codex" {
		t.Fatalf("confirmed dispatch-claim did not mutate through claim path: %+v", after)
	}
}

func TestWorkDispatchClaimRejectsNextAndExplicitIDTogether(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "reject ambiguous target"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	output, runErr := runDispatchClaimChild(t, "both", homePath, ready.ID, "")
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("ambiguous target exit = %v, want 2\n%s", runErr, output)
	}
	if !strings.Contains(string(output), "either --next or <work_id>") {
		t.Fatalf("ambiguous target error is not actionable:\n%s", output)
	}
}

func TestWorkDispatchClaimRejectsConfirmAsMissingActorValue(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	if _, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "missing actor value must not swallow confirm",
	}, cliTestNow); err != nil {
		t.Fatalf("create work: %v", err)
	}
	before := snapshotDispatchClaimHome(t, homePath)

	output, runErr := runDispatchClaimChild(t, "missing-actor-value", homePath, "", "")
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("missing actor value exit = %v, want usage exit 2\n%s", runErr, output)
	}
	if !strings.Contains(string(output), "usage: compound work dispatch-claim") {
		t.Fatalf("missing actor value did not print dispatch-claim usage:\n%s", output)
	}
	if strings.Contains(string(output), `"actor": "--confirm"`) {
		t.Fatalf("--confirm was swallowed as the actor value:\n%s", output)
	}
	if after := snapshotDispatchClaimHome(t, homePath); !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid dispatch-claim mutated home:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestDispatchCLIIsolationIgnoresAndRestoresAmbientCapacityGuard(t *testing.T) {
	previousGuard := work.LaneCapacityGuard
	ambientCalls := 0
	work.LaneCapacityGuard = func(work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		ambientCalls++
		return work.LaneCapacityGuardResult{
			Capacity: work.DispatchLaneCapacity{State: "walled", DataRef: "ambient test sentinel"},
			Refused:  true,
			Message:  "ambient capacity must not enter hermetic CLI tests",
		}, nil
	}
	t.Cleanup(func() { work.LaneCapacityGuard = previousGuard })

	t.Run("direct CLI harness", func(t *testing.T) {
		isolateDispatchCapacityGuard(t)
		homePath := t.TempDir()
		t.Setenv("COMPOUND_HOME", homePath)
		if err := home.Init(homePath, "test", cliTestNow); err != nil {
			t.Fatalf("init home: %v", err)
		}
		store, err := work.Open(homePath)
		if err != nil {
			t.Fatalf("open work: %v", err)
		}
		ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "ambient capacity is isolated"}, cliTestNow)
		if err != nil {
			t.Fatalf("create work: %v", err)
		}

		out := captureStdout(t, func() {
			cmdWork([]string{"dispatch-plan", "--actor", "codex", "--json"})
		})
		var plan work.DispatchPlan
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatalf("decode dispatch plan: %v\n%s", err, out)
		}
		if plan.Status != "ready_to_claim" || plan.Selected == nil || plan.Selected.WorkID != ready.ID {
			t.Fatalf("ambient capacity changed hermetic dispatch result: %+v", plan)
		}
		if plan.LaneCapacity != nil {
			t.Fatalf("ambient capacity leaked into hermetic dispatch result: %+v", plan.LaneCapacity)
		}
	})

	if ambientCalls != 0 {
		t.Fatalf("ambient capacity guard called during isolated CLI run: %d", ambientCalls)
	}
	if _, err := work.LaneCapacityGuard(work.LaneCapacityGuardRequest{}); err != nil {
		t.Fatalf("restored ambient guard: %v", err)
	}
	if ambientCalls != 1 {
		t.Fatalf("ambient capacity guard was not restored after isolated CLI run: calls=%d", ambientCalls)
	}
}

func TestWorkDispatchClaimWalledPreviewPreservesStatusWithoutEvidenceWrite(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "walled preview stays read-only"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	now := time.Now()
	capacityPath := filepath.Join(t.TempDir(), "usage-history.jsonl")
	row, err := json.Marshal(map[string]any{
		"provider":      "codex",
		"windowKind":    "5h",
		"windowMinutes": 300,
		"usedPercent":   100,
		"resetsAt":      now.Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"sampledAt":     now.Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capacityPath, append(row, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotDispatchClaimHome(t, homePath)

	output, runErr := runDispatchClaimChild(t, "walled", homePath, ready.ID, capacityPath)
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("walled preview exit = %v, want 1\n%s", runErr, output)
	}
	var preview work.DispatchPlan
	if err := json.Unmarshal(output, &preview); err != nil {
		t.Fatalf("parse walled preview: %v\n%s", err, output)
	}
	if preview.Status != "capacity_refused" || preview.LaneCapacity == nil || preview.LaneCapacity.State != "walled" {
		t.Fatalf("walled capacity disappeared from preview: %+v", preview)
	}
	if after := snapshotDispatchClaimHome(t, homePath); !reflect.DeepEqual(before, after) {
		t.Fatalf("walled preview wrote refusal evidence:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestWorkDispatchClaimPreviewCapacityGuardIsPerRequest(t *testing.T) {
	makeStore := func(predicate string) *work.Store {
		t.Helper()
		homePath := t.TempDir()
		if err := home.Init(homePath, "test", cliTestNow); err != nil {
			t.Fatalf("init home: %v", err)
		}
		store, err := work.Open(homePath)
		if err != nil {
			t.Fatalf("open work: %v", err)
		}
		if _, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: predicate}, cliTestNow); err != nil {
			t.Fatalf("create work: %v", err)
		}
		return store
	}
	previewStore := makeStore("read-only request keeps guard installed")
	mutableStore := makeStore("ordinary request keeps its own capacity semantics")

	previousGuard := work.LaneCapacityGuard
	requests := make(chan bool, 2)
	release := make(chan struct{})
	released := false
	work.LaneCapacityGuard = func(req work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		requests <- req.ReadOnly
		<-release
		return work.LaneCapacityGuardResult{
			Capacity: work.DispatchLaneCapacity{State: "walled", DataRef: "concurrency falsifier"},
			Refused:  true,
			Message:  "capacity refused",
		}, nil
	}
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		work.LaneCapacityGuard = previousGuard
	})

	type planResult struct {
		plan work.DispatchPlan
		err  error
	}
	results := make(chan planResult, 2)
	go func() {
		plan, err := previewDispatchClaim(previewStore, cliTestNow, work.DispatchPlanOptions{Actor: "codex"})
		results <- planResult{plan: plan, err: err}
	}()
	go func() {
		plan, err := mutableStore.DispatchPlan(cliTestNow, work.DispatchPlanOptions{Actor: "codex"})
		results <- planResult{plan: plan, err: err}
	}()

	seenReadOnly, seenMutable := false, false
	for range 2 {
		select {
		case readOnly := <-requests:
			if readOnly {
				seenReadOnly = true
			} else {
				seenMutable = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent preview removed the process-global capacity guard")
		}
	}
	close(release)
	released = true
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("dispatch plan: %v", result.err)
		}
		if result.plan.Status != "capacity_refused" {
			t.Fatalf("concurrent plan lost capacity status: %+v", result.plan)
		}
	}
	if !seenReadOnly || !seenMutable {
		t.Fatalf("capacity requests = read_only:%v mutable:%v, want one of each", seenReadOnly, seenMutable)
	}
}

func runDispatchClaimChild(t *testing.T, mode, homePath, workID, capacityPath string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkDispatchClaimCLIChild$")
	cmd.Env = []string{
		dispatchClaimConfirmChildEnv + "=" + mode,
		"COMPOUND_HOME=" + homePath,
		"COMPOUND_TEST_WORK_ID=" + workID,
		"COMPOUND_TEST_CAPACITY_PATH=" + capacityPath,
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"TZ=UTC",
	}
	return cmd.CombinedOutput()
}

func snapshotDispatchClaimHome(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			snapshot[rel] = "dir"
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		snapshot[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot home: %v", err)
	}
	return snapshot
}
