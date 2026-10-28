package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/runattempt"
	"compound/internal/work"
)

var delegateCLITestNow = time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)

type delegateCLIFakeRunner struct {
	mu        sync.Mutex
	live      map[string]bool
	panes     map[string]work.SessionStatus
	creates   []delegateCLICreate
	keys      []delegateCLIKeys
	kills     []string
	createErr error
}

type delegateCLICreate struct {
	name, dir, command string
	args               []string
}

type delegateCLIKeys struct {
	name, text string
	enter      bool
}

func newDelegateCLIFakeRunner() *delegateCLIFakeRunner {
	return &delegateCLIFakeRunner{live: map[string]bool{}, panes: map[string]work.SessionStatus{}}
}

func (f *delegateCLIFakeRunner) Available() error { return nil }
func (f *delegateCLIFakeRunner) Has(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[name], nil
}
func (f *delegateCLIFakeRunner) SessionStatus(name string) (work.SessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ps, ok := f.panes[name]; ok {
		return ps, nil
	}
	if f.live[name] {
		return work.SessionStatus{Exists: true, ExitCode: -1}, nil
	}
	return work.SessionStatus{Exists: false}, nil
}
func (f *delegateCLIFakeRunner) Create(name, dir, command string, args []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.creates = append(f.creates, delegateCLICreate{name: name, dir: dir, command: command, args: append([]string(nil), args...)})
	f.live[name] = true
	return nil
}
func (f *delegateCLIFakeRunner) SendKeys(name, text string, enter bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.live[name] {
		return fmt.Errorf("session %s is dead", name)
	}
	f.keys = append(f.keys, delegateCLIKeys{name: name, text: text, enter: enter})
	return nil
}
func (f *delegateCLIFakeRunner) Kill(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, name)
	delete(f.live, name)
	delete(f.panes, name)
	return nil
}
func (f *delegateCLIFakeRunner) AttachArgv(name string) []string {
	return []string{"tmux", "attach-session", "-t", name}
}

func claimedDelegateCLIWork(t *testing.T, holder string) (*work.Store, string) {
	t.Helper()
	store, workID, _ := claimedDelegateCLIWorkAt(t, holder)
	return store, workID
}

func claimedDelegateCLIWorkAt(t *testing.T, holder string) (*work.Store, string, string) {
	t.Helper()
	home := t.TempDir()
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner: holder, SuccessPredicate: "governed session remains observable and steerable", State: "ready",
	}, delegateCLITestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, holder, delegateCLITestNow); err != nil {
		t.Fatal(err)
	}
	return store, w.ID, home
}

func allowDelegateCLIGuard(t *testing.T, confirmationRequired bool) {
	t.Helper()
	orig := work.LaneCapacityGuard
	origResolver := work.DelegateHarnessResolver
	origReadiness := work.DelegateHarnessReadiness
	work.LaneCapacityGuard = func(req work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		return work.LaneCapacityGuardResult{
			Capacity:             work.DispatchLaneCapacity{State: "ok", Remaining: 0.8},
			ConfirmationRequired: confirmationRequired,
			Message:              "lane has headroom",
		}, nil
	}
	work.DelegateHarnessResolver = func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		command := strings.ReplaceAll(req.Harness, "_", "-")
		if req.CommandOverride != "" {
			command = req.CommandOverride
		}
		return work.DelegateHarnessResolution{
			Harness: req.Harness, Command: command, LaneID: req.Harness,
			RequestedModelID: "fixture-model", RequestedProviderRoute: strings.TrimPrefix(command, "claude-"),
			RequestedProviderRouteKind: "provider_class",
			CapacityLane:               req.Harness, TupleBacked: true,
		}, nil
	}
	work.DelegateHarnessReadiness = func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() {
		work.LaneCapacityGuard = orig
		work.DelegateHarnessResolver = origResolver
		work.DelegateHarnessReadiness = origReadiness
	})
}

func runDelegateCLIForTest(t *testing.T, sub string, store *work.Store, runner work.SessionRunner, attach func([]string) error, args ...string) (string, string, int) {
	t.Helper()
	args = explicitExperimentalDelegateArgsForTest(sub, args)
	var stdout, stderr bytes.Buffer
	code := runWorkDelegateCLI(sub, args, store, runner, attach, delegateCLITestNow, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

// Most historical delegate fixtures exercise the now-unsupported mutation
// engine deliberately. Keep that opt-in visible at their shared composition
// seam while dedicated boundary tests below call runWorkDelegateCLI directly
// to prove production remains default-off.
func explicitExperimentalDelegateArgsForTest(sub string, args []string) []string {
	out := append([]string(nil), args...)
	mutation := sub == "steer"
	if sub == "delegate" {
		if len(out) > 0 && out[0] == "status" {
			return out
		}
		for _, arg := range out {
			if arg == "--dry-run" {
				return out
			}
		}
		mutation = true
	}
	if !mutation {
		return out
	}
	for _, arg := range out {
		if arg == work.ExperimentalDelegateMutationsFlag {
			return out
		}
	}
	insertAt := len(out)
	for i, arg := range out {
		if arg == "--" {
			insertAt = i
			break
		}
	}
	out = append(out, "")
	copy(out[insertAt+1:], out[insertAt:])
	out[insertAt] = work.ExperimentalDelegateMutationsFlag
	return out
}

func runDelegateCLIWithoutExperimentalGateForTest(t *testing.T, sub string, store *work.Store, runner work.SessionRunner, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWorkDelegateCLI(sub, args, store, runner, nil, delegateCLITestNow, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestWorkDelegateMutationCLIGateDefaultsOffWithoutSideEffects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sub  string
		args func(string) []string
	}{
		{name: "spawn", sub: "delegate", args: func(workID string) []string {
			return []string{workID, "--harness", "claude-zai"}
		}},
		{name: "steer", sub: "steer", args: func(workID string) []string {
			return []string{workID, "--text", "do not send"}
		}},
		{name: "progress", sub: "delegate", args: func(workID string) []string {
			return []string{"progress", workID, "--done", "0", "--empty", "0", "--failed", "0", "--remaining", "1"}
		}},
		{name: "sweep", sub: "delegate", args: func(workID string) []string {
			return []string{"sweep", workID}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, workID, homePath := claimedDelegateCLIWorkAt(t, "orchestrator:test")
			runner := newDelegateCLIFakeRunner()
			before := snapshotFiles(t, homePath)

			_, errOut, code := runDelegateCLIWithoutExperimentalGateForTest(t, tc.sub, store, runner, tc.args(workID)...)
			if code != 2 || !strings.Contains(errOut, work.ExperimentalDelegateMutationsFlag) || !strings.Contains(errOut, "unsupported in v0.1") {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			after := snapshotFiles(t, homePath)
			if !reflect.DeepEqual(before, after) || len(runner.creates) != 0 || len(runner.keys) != 0 || len(runner.kills) != 0 {
				t.Fatalf("default-off %s mutated state: files_equal=%v creates=%v keys=%v kills=%v", tc.name, reflect.DeepEqual(before, after), runner.creates, runner.keys, runner.kills)
			}
		})
	}
}

func TestWorkDelegateCLIPromptUsesNamedWrapperWithoutPersistingSpeech(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID := claimedDelegateCLIWork(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	workspace := t.TempDir()
	const speech = "let it move like tidewater around the hard edges"
	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude_zai", "--prompt", speech, "--workspace", workspace, "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	if len(runner.creates) != 1 {
		t.Fatalf("creates = %+v", runner.creates)
	}
	created := runner.creates[0]
	if created.command != "claude-zai" || created.dir != workspace || len(created.args) != 1 || created.args[0] != speech {
		t.Fatalf("create = %+v", created)
	}
	if strings.Contains(out, speech) || strings.Contains(errOut, speech) {
		t.Fatalf("CLI output persisted expressive prompt: stdout=%q stderr=%q", out, errOut)
	}
	var result work.DelegateResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, out)
	}
	if result.Status != "spawned" || result.Binding == nil || result.Binding.Harness != "claude_zai" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWorkDelegateCLIRawCommandAfterDashDash(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID := claimedDelegateCLIWork(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	_, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude-qwen", "--", "worker-tool", "--mode", "gentle")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(runner.creates) != 1 || runner.creates[0].command != "worker-tool" || strings.Join(runner.creates[0].args, " ") != "--mode gentle" {
		t.Fatalf("raw create = %+v", runner.creates)
	}
}

func TestWorkDelegateCLIDryRunPrintsGuardDecisionAndDoesNotSpawn(t *testing.T) {
	allowDelegateCLIGuard(t, true)
	store, workID := claimedDelegateCLIWork(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude-minimax", "--dry-run")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "status=preview") || !strings.Contains(out, "lane=claude-minimax") || !strings.Contains(out, "capacity=ok") || !strings.Contains(out, "confirmation_required=true") {
		t.Fatalf("dry-run hid guard decision: %q", out)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("dry-run spawned: %+v", runner.creates)
	}
}

func TestWorkDelegateCLIRefusesUnreadyHarnessBeforeAnyMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalGuard := work.LaneCapacityGuard
	originalResolver := work.DelegateHarnessResolver
	capacityCalls := 0
	work.LaneCapacityGuard = func(req work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		capacityCalls++
		return work.LaneCapacityGuardResult{
			Capacity: work.DispatchLaneCapacity{State: "ok", Remaining: 0.8},
			Message:  "lane has headroom",
		}, nil
	}
	work.DelegateHarnessResolver = func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		return work.DelegateHarnessResolution{
			Harness:                    "claude_minimax",
			Command:                    "claude-minimax",
			LaneID:                     "claude_minimax",
			RequestedModelID:           "claude-minimax (token plan)",
			RequestedProviderRoute:     "minimax",
			RequestedProviderRouteKind: "provider_class",
			CapacityLane:               "minimax",
			TupleBacked:                true,
		}, nil
	}
	t.Cleanup(func() {
		work.LaneCapacityGuard = originalGuard
		work.DelegateHarnessResolver = originalResolver
	})

	store, workID, homePath := claimedDelegateCLIWorkAt(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	before := snapshotFiles(t, homePath)

	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude_minimax", "--prompt", "do the bounded task", "--json")

	if code != 1 || errOut != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	var result work.DelegateResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode refusal: %v\n%s", err, out)
	}
	if result.Status != "refused" || result.Refusal == nil || result.Refusal.Reason != "readiness_unmet" {
		t.Fatalf("result = %+v, want structured readiness_unmet refusal", result)
	}
	if result.Readiness == nil || result.Readiness.Ready {
		t.Fatalf("readiness = %+v, want explicit unready evidence", result.Readiness)
	}
	for _, want := range []string{"permission_authority:", "blocking_hook:", "instruction_pack:"} {
		if !strings.Contains(strings.Join(result.Readiness.Details, "\n"), want) {
			t.Fatalf("readiness details = %v, want %q component", result.Readiness.Details, want)
		}
	}
	if capacityCalls != 0 {
		t.Fatalf("capacity guard calls = %d, want 0 before readiness admission", capacityCalls)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("session creates = %+v, want none", runner.creates)
	}
	if after := snapshotFiles(t, homePath); !reflect.DeepEqual(before, after) {
		t.Fatalf("unready refusal mutated Compound state\nbefore=%v\nafter=%v", before, after)
	}
}

func TestWorkDelegateCLISteerAndAttachUseRecoveredSession(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID := claimedDelegateCLIWork(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	if _, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude-zai"); code != 0 {
		t.Fatalf("delegate code=%d stderr=%q", code, errOut)
	}

	const direction = "follow the surprising thread"
	if _, errOut, code := runDelegateCLIForTest(t, "steer", store, runner, nil,
		workID, "--text", direction, "--enter"); code != 0 {
		t.Fatalf("steer code=%d stderr=%q", code, errOut)
	}
	if len(runner.keys) != 1 || runner.keys[0].text != direction || !runner.keys[0].enter {
		t.Fatalf("steer calls = %+v", runner.keys)
	}

	var attached []string
	attach := func(argv []string) error {
		attached = append([]string(nil), argv...)
		return nil
	}
	if _, errOut, code := runDelegateCLIForTest(t, "attach", store, runner, attach, workID); code != 0 {
		t.Fatalf("attach code=%d stderr=%q", code, errOut)
	}
	wantSession := "compound-" + workID
	if strings.Join(attached, " ") != "tmux attach-session -t "+wantSession {
		t.Fatalf("attach argv = %q", attached)
	}
}

func TestWorkDelegateStatusIsReachableAndWritesNothing(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID, home := claimedDelegateCLIWorkAt(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	spawnOut, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude-zai", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("spawn code=%d stderr=%q", code, errOut)
	}
	var spawned work.DelegateResult
	if err := json.Unmarshal([]byte(spawnOut), &spawned); err != nil || spawned.Binding == nil {
		t.Fatalf("decode spawn: %+v %v", spawned, err)
	}
	runner.panes[spawned.Binding.Session] = work.SessionStatus{Exists: true, PaneDead: true, ExitCode: 1}
	before := snapshotFiles(t, home)
	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil, "status", workID, "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("status code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	var observed work.DelegateStatusObservation
	if err := json.Unmarshal([]byte(out), &observed); err != nil {
		t.Fatalf("decode status: %v\n%s", err, out)
	}
	if !observed.Bound || !observed.PaneStatus.PaneDead || observed.RunAttemptID != spawned.RunAttemptID || observed.RunAttemptStatus != "running" {
		t.Fatalf("status observation = %+v", observed)
	}
	after := snapshotFiles(t, home)
	if !reflect.DeepEqual(before, after) || len(runner.kills) != 0 {
		t.Fatalf("status mutated state: files_equal=%v kills=%v", reflect.DeepEqual(before, after), runner.kills)
	}
}

// TestWorkDelegateProgressUpdatesBoundRunAttempt proves an operator or worker
// can report cumulative batch progress against the exact supervised attempt
// already bound to the work. The command never accepts a caller-supplied
// ratt id, so progress cannot drift onto a sibling claim reservation.
func TestWorkDelegateProgressUpdatesBoundRunAttempt(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID, home := claimedDelegateCLIWorkAt(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	spawnOut, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude-minimax", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("spawn code=%d stderr=%q stdout=%q", code, errOut, spawnOut)
	}
	var spawned work.DelegateResult
	if err := json.Unmarshal([]byte(spawnOut), &spawned); err != nil || spawned.Binding == nil {
		t.Fatalf("decode spawn: %+v %v", spawned, err)
	}

	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		"progress", workID,
		"--done", "3", "--empty", "1", "--failed", "0", "--remaining", "6", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("progress code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	var result struct {
		RunAttemptID string `json:"run_attempt_id"`
		Progress     struct {
			Done      int `json:"done"`
			Empty     int `json:"empty"`
			Failed    int `json:"failed"`
			Remaining int `json:"remaining"`
		} `json:"progress"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode progress: %v\n%s", err, out)
	}
	if result.RunAttemptID != spawned.RunAttemptID || result.Progress.Done != 3 || result.Progress.Empty != 1 || result.Progress.Failed != 0 || result.Progress.Remaining != 6 {
		t.Fatalf("progress result = %+v, spawned=%+v", result, spawned)
	}

	ra, err := runattempt.Open(home).Get(spawned.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(home, "run_attempts", ra.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"done": 3`, `"empty": 1`, `"failed": 0`, `"remaining": 6`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("persisted RunAttempt missing %s:\n%s", want, body)
		}
	}
}

func TestWorkDelegateSweepDryRunWritesAndKillsNothing(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID, home := claimedDelegateCLIWorkAt(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	spawnOut, _, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude-minimax", "--json")
	if code != 0 {
		t.Fatalf("spawn code=%d", code)
	}
	var spawned work.DelegateResult
	if err := json.Unmarshal([]byte(spawnOut), &spawned); err != nil || spawned.Binding == nil {
		t.Fatalf("decode spawn: %+v %v", spawned, err)
	}
	runner.panes[spawned.Binding.Session] = work.SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	before := snapshotFiles(t, home)
	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		"sweep", workID, "--dry-run", "--json")
	if code != 0 || errOut != "" || !strings.Contains(out, spawned.RunAttemptID) {
		t.Fatalf("dry-run code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	if after := snapshotFiles(t, home); !reflect.DeepEqual(before, after) || len(runner.kills) != 0 {
		t.Fatalf("dry-run mutated state: files_equal=%v kills=%v", reflect.DeepEqual(before, after), runner.kills)
	}
}

func TestWorkDelegateSweepTerminalizesAndRemovesDeadPane(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID := claimedDelegateCLIWork(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	spawnOut, _, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude-qwen", "--json")
	if code != 0 {
		t.Fatalf("spawn code=%d", code)
	}
	var spawned work.DelegateResult
	if err := json.Unmarshal([]byte(spawnOut), &spawned); err != nil || spawned.Binding == nil {
		t.Fatalf("decode spawn: %+v %v", spawned, err)
	}
	runner.panes[spawned.Binding.Session] = work.SessionStatus{Exists: true, PaneDead: true, ExitCode: 1}
	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		"sweep", workID, "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("sweep code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	var swept work.DelegateSweepOutcome
	if err := json.Unmarshal([]byte(out), &swept); err != nil {
		t.Fatalf("decode sweep: %v\n%s", err, out)
	}
	if swept.RunAttemptID != spawned.RunAttemptID || swept.TerminalStatus != "failed" || len(runner.kills) != 1 {
		t.Fatalf("sweep=%+v kills=%v", swept, runner.kills)
	}
}

func TestRunAttemptSweepProbeUsesExactDelegateBinding(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID, home := claimedDelegateCLIWorkAt(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	spawnOut, _, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude-zai", "--json")
	if code != 0 {
		t.Fatalf("spawn code=%d", code)
	}
	var spawned work.DelegateResult
	if err := json.Unmarshal([]byte(spawnOut), &spawned); err != nil || spawned.Binding == nil {
		t.Fatalf("decode spawn: %+v %v", spawned, err)
	}
	all, err := runattempt.Open(home).List()
	if err != nil {
		t.Fatal(err)
	}
	var delegated, sibling runattempt.RunAttempt
	for _, ra := range all {
		switch ra.ID {
		case spawned.RunAttemptID:
			delegated = ra
		default:
			if ra.WorkID == workID {
				sibling = ra
			}
		}
	}
	probe := buildDelegateLivenessProbeWithRunner(home, runner)
	if live, err := probe(delegated); err != nil || !live {
		t.Fatalf("live exact delegate probe = %v, %v", live, err)
	}
	if live, err := probe(sibling); err != nil || live {
		t.Fatalf("sibling claim reservation probe = %v, %v", live, err)
	}
	runner.panes[spawned.Binding.Session] = work.SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	if live, err := probe(delegated); err != nil || live {
		t.Fatalf("dead exact delegate probe = %v, %v", live, err)
	}
}

func TestWorkDelegateCLIUsageErrors(t *testing.T) {
	t.Parallel()
	store, workID := claimedDelegateCLIWork(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	cases := []struct {
		sub  string
		args []string
	}{
		{sub: "delegate", args: []string{workID}},
		{sub: "delegate", args: []string{workID, "--harness", "claude-zai", "--prompt", "x", "--", "worker"}},
		{sub: "delegate", args: []string{workID, "--harness", "claude-zai", "--expected-minutes", "never"}},
		{sub: "steer", args: []string{workID}},
		{sub: "attach", args: []string{workID, "--json"}},
	}
	for _, tc := range cases {
		t.Run(tc.sub+strings.Join(tc.args, "_"), func(t *testing.T) {
			_, errOut, code := runDelegateCLIForTest(t, tc.sub, store, runner, nil, tc.args...)
			if code != 2 || !strings.Contains(errOut, "usage: compound work "+tc.sub) {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
		})
	}
}

func TestWorkDelegateHelpInventory(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"delegate", "steer", "attach"} {
		if !strings.Contains(workUsage(""), verb) || !strings.Contains(workUsage(verb), "usage: compound work "+verb) {
			t.Fatalf("work help does not expose %s: root=%q sub=%q", verb, workUsage(""), workUsage(verb))
		}
	}
	for _, verb := range []string{"delegate", "steer"} {
		help := workUsage(verb)
		if !strings.Contains(help, work.ExperimentalDelegateMutationsFlag) || !strings.Contains(help, "unsupported in v0.1") {
			t.Fatalf("experimental mutation boundary missing from %s help: %q", verb, help)
		}
	}
}

func TestWorkDelegateWritableStoreClassificationUsesExactParsers(t *testing.T) {
	t.Parallel()
	const workID = "work_delegate_parser_classification"
	tests := []struct {
		name string
		sub  string
		args []string
		want bool
	}{
		{name: "status", sub: "delegate", args: []string{"status", workID}},
		{name: "attach", sub: "attach", args: []string{workID}},
		{name: "default off", sub: "delegate", args: []string{workID, "--harness", "claude-zai"}},
		{name: "dry run", sub: "delegate", args: []string{workID, "--harness", "claude-zai", "--dry-run"}},
		{name: "gate is harness value", sub: "delegate", args: []string{workID, "--harness", work.ExperimentalDelegateMutationsFlag}},
		{name: "gate is raw command", sub: "delegate", args: []string{workID, "--harness", "claude-zai", "--", work.ExperimentalDelegateMutationsFlag}},
		{name: "gate is steer text", sub: "steer", args: []string{workID, "--text", work.ExperimentalDelegateMutationsFlag}},
		{name: "gate is progress count", sub: "delegate", args: []string{"progress", workID, "--done", work.ExperimentalDelegateMutationsFlag, "--empty", "0", "--failed", "0", "--remaining", "0"}},
		{name: "invalid sweep", sub: "delegate", args: []string{"sweep", workID, work.ExperimentalDelegateMutationsFlag, "--unknown"}},
		{name: "delegate mutation", sub: "delegate", args: []string{workID, "--harness", "claude-zai", work.ExperimentalDelegateMutationsFlag}, want: true},
		{name: "raw dry run token is command data", sub: "delegate", args: []string{workID, "--harness", "claude-zai", work.ExperimentalDelegateMutationsFlag, "--", "worker", "--dry-run"}, want: true},
		{name: "steer mutation", sub: "steer", args: []string{workID, "--text", "send", work.ExperimentalDelegateMutationsFlag}, want: true},
		{name: "sweep mutation", sub: "delegate", args: []string{"sweep", workID, work.ExperimentalDelegateMutationsFlag}, want: true},
		{name: "progress mutation", sub: "delegate", args: []string{"progress", workID, "--done", "0", "--empty", "0", "--failed", "0", "--remaining", "0", work.ExperimentalDelegateMutationsFlag}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := workCommandNeedsWritableStore(tc.sub, tc.args); got != tc.want {
				t.Fatalf("workCommandNeedsWritableStore(%q, %q) = %v, want %v", tc.sub, tc.args, got, tc.want)
			}
		})
	}
}

func TestExperimentalDelegateMutationEnablementHasOneProductionOwner(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, top), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			count := strings.Count(string(body), ".EnableExperimentalDelegateMutations()")
			if count == 0 {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			calls[filepath.ToSlash(rel)] = count
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int{"cmd/compound/work_delegate.go": 4}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("experimental delegate mutation production call sites = %v, want %v", calls, want)
	}
}

func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = string(body)
		return nil
	}); err != nil {
		t.Fatalf("snapshot files: %v", err)
	}
	return out
}

// Falsifier B6: the --instruction-block opt-in flag renders the executor
// variant and appends it to the dispatch prompt that the delegate forwards
// to the harness lane. The original prompt is preserved; the variant lives
// after a blank line; the executor charter is in the forwarded args; the
// preset tag is in the body; the begin/end markers are present. The repo
// file (CLAUDE.md / AGENTS.md) is NOT touched.
func TestWorkDelegateInstructionBlockAppendsExecutorVariantToDispatchPrompt(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID := claimedDelegateCLIWork(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	workspace := t.TempDir()
	// Plant a real instruction target so the (unused by the variant) target
	// resolution has a file to walk past if a regression runs the default
	// branch. The variant path skips drift and never reads the file.
	target := filepath.Join(workspace, "CLAUDE.md")
	before := "# untouched\n"
	if err := os.WriteFile(target, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	const speech = "audit the executor branch and report a falsifier"
	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude_code", "--prompt", speech, "--workspace", workspace,
		"--instruction-block", "executor", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	if len(runner.creates) != 1 {
		t.Fatalf("creates = %+v", runner.creates)
	}
	forwarded := runner.creates[0].args
	if len(forwarded) != 1 {
		t.Fatalf("args len = %d, want 1", len(forwarded))
	}
	prompt := forwarded[0]
	if !strings.HasPrefix(prompt, speech) {
		t.Fatalf("forwarded prompt must start with the original --prompt text; got head: %q", prompt[:min(120, len(prompt))])
	}
	// The executor variant lives after a blank line.
	wantCharter := []string{
		"Work only inside the dispatched envelope/worktree",
		"Commit incrementally on the dispatched branch",
		"do not push, do not check out main",
		"Report back in the handoff format",
		"Escalate instead of improvising when blocked",
		"preset=" + "`executor`",
		"<!-- BEGIN COMPOUND GENERATED INSTRUCTIONS harness=claude_code -->",
		"<!-- END COMPOUND GENERATED INSTRUCTIONS -->",
	}
	for _, want := range wantCharter {
		if !strings.Contains(prompt, want) {
			t.Errorf("forwarded prompt missing %q", want)
		}
	}
	// Repo file untouched.
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatalf("instruction-block mutated repo file:\nbefore=%q\nafter=%q", before, string(after))
	}
	// Task text does not leak into the appended variant.
	if strings.Contains(prompt[strings.Index(prompt, "<!-- BEGIN"):], "audit the executor branch") {
		t.Fatalf("task text leaked into the appended variant block:\n%s", prompt)
	}
}

// Falsifier B6 (negative): without --instruction-block, the prompt is
// forwarded verbatim — the wire is opt-in. This is the contract surface for
// "do not invent plumbing": absent the flag, the dispatch path is unchanged.
func TestWorkDelegateWithoutInstructionBlockForwardsPromptVerbatim(t *testing.T) {
	allowDelegateCLIGuard(t, false)
	store, workID := claimedDelegateCLIWork(t, "orchestrator:test")
	runner := newDelegateCLIFakeRunner()
	workspace := t.TempDir()
	const speech = "verify the existing dispatch path is byte-identical"
	out, errOut, code := runDelegateCLIForTest(t, "delegate", store, runner, nil,
		workID, "--harness", "claude_zai", "--prompt", speech, "--workspace", workspace, "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, errOut, out)
	}
	if len(runner.creates) != 1 {
		t.Fatalf("creates = %+v", runner.creates)
	}
	if len(runner.creates[0].args) != 1 || runner.creates[0].args[0] != speech {
		t.Fatalf("args = %+v, want [speech]", runner.creates[0].args)
	}
}
