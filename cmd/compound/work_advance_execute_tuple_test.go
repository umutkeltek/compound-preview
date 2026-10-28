package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
	"compound/internal/runattempt"
	"compound/internal/work"
	"compound/internal/work/executewire"
)

// Two owner tuples deliberately share one adapter. The default tuple's model,
// provider and executable differ, so a harness-only handoff cannot pass.
func adaptiveTupleFixture(t *testing.T) (governedClaudeExecutionFixture, []work.ExecutionRouteCandidate) {
	t.Helper()
	f := newGovernedClaudeLaneExecutionFixture(t, "exact detached tuple reaches adapter", "claude_qwen", "claude-qwen")
	bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	wrapper := filepath.Join(bin, "fixture-qwen-quality")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf '%s\\n' quality > \"$COMPOUND_TEST_CLAUDE_ARGV\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	body := `version: 1
capabilities: [build-implement, work.continue.unattended]
models:
  claude-qwen:
    lane: default
    harness: claude_qwen
    provider_route: fixture-default
    tier: cheap
    packet_ceiling: multi-file
    aliases: [claude-qwen]
    capabilities:
      build-implement: {level: capable, evidence: fixture}
      work.continue.unattended: {level: capable, evidence: fixture}
  fixture model (quality):
    lane: quality
    harness: claude_qwen
    provider_route: fixture-quality
    tier: cheap
    packet_ceiling: multi-file
    aliases: [fixture-qwen-quality]
    capabilities:
      build-implement: {level: capable, evidence: fixture}
      work.continue.unattended: {level: capable, evidence: fixture}
routing_defaults:
  build-implement: [claude-qwen, fixture model (quality)]
  work.continue.unattended: [claude-qwen, fixture model (quality)]
`
	path := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(f.homePath, path, time.Now()); err != nil {
		t.Fatal(err)
	}
	candidates := executewire.RouteCandidatesFromManifest(f.homePath, "work.continue.unattended")
	if len(candidates) != 2 || candidates[0].Harness != candidates[1].Harness || candidates[0].RequestedModelID == candidates[1].RequestedModelID {
		t.Fatalf("distinct same-harness tuples were lost: %+v", candidates)
	}
	return f, candidates
}

func adaptiveDetachedArgs(f governedClaudeExecutionFixture, candidate work.ExecutionRouteCandidate) []string {
	argv := executewire.DetachedCandidateLaunchArgv("timeout", "compound", time.Minute, f.workID, candidate, "selected tuple", "codex")
	return append(argv[6:], "--workspace", f.workspace)
}

func TestWorkAdvanceExecuteDetachedTupleReachesActualAdapter(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			f, candidates := adaptiveTupleFixture(t)
			candidate := candidates[1]
			if saved {
				candidate.LaunchProfileID = "fixture-qwen-quality"
				saveAdaptiveSelection(t, f, candidate)
				candidate.SelectionRevision = 1
			}
			before, err := f.store.Get(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			args := adaptiveDetachedArgs(f, candidate)
			if saved && strings.Contains(strings.Join(args, " "), "--launch-profile") {
				t.Fatal("saved selection replaced by override")
			}
			calls := 0
			stubDelegateHarnessReadiness(t, func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
				calls++
				if req.Harness != candidate.Harness || req.RequestedModelID != candidate.RequestedModelID || req.RequestedProviderRoute != candidate.ProviderRoute || filepath.Base(req.Command) != "fixture-qwen-quality" {
					t.Fatalf("readiness substituted selected tuple: %+v", req)
				}
				return work.DelegateHarnessReadinessResult{Ready: true, Evidence: governedClaudeReadyEvidenceFor("claude_qwen")}, nil
			})
			captureStdout(t, func() { cmdWork(args) })
			attempt := f.executionAttempt(t)
			if attempt.Harness != candidate.Harness || attempt.LaunchProfileID != candidate.LaunchProfileID || attempt.RequestedModelID != candidate.RequestedModelID || attempt.RequestedProviderRoute != candidate.ProviderRoute || filepath.Base(attempt.Command) != "fixture-qwen-quality" {
				t.Fatalf("actual attempt substituted tuple: %+v", attempt)
			}
			if f.argv(t) != "quality\n" || calls != 1 {
				t.Fatalf("selected wrapper/readiness not consumed: calls=%d argv=%q", calls, f.argv(t))
			}
			after, err := f.store.Get(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.ExecutionSelection, after.ExecutionSelection) {
				t.Fatal("detached execution rewrote saved selection")
			}
			source := "override"
			if saved {
				source = "saved"
			}
			if attempt.ExecutionSelection == nil || attempt.ExecutionSelection.Source != source || attempt.ExecutionSelection.SelectionRevision != candidate.SelectionRevision {
				t.Fatalf("wrong selection evidence: %+v", attempt.ExecutionSelection)
			}
		})
	}
}

func TestWorkAdvanceExecuteDetachedTupleDriftHasNoDelta(t *testing.T) {
	for _, drift := range []string{"digest", "manifest", "selection", "model", "provider", "capacity", "profile", "foreign-profile"} {
		t.Run(drift, func(t *testing.T) {
			f, candidates := adaptiveTupleFixture(t)
			candidate := candidates[1]
			switch drift {
			case "profile":
				candidate.LaunchProfileID = "fixture-missing-wrapper"
			case "foreign-profile":
				candidate.LaunchProfileID = candidates[0].LaunchProfileID
			case "model":
				candidate.RequestedModelID = "fixture-substitution"
			case "provider":
				candidate.ProviderRoute = "fixture-substitution"
			case "capacity":
				candidate.CapacityLane = "fixture-substitution"
			}
			args := adaptiveDetachedArgs(f, candidate)
			switch drift {
			case "digest":
				for i := range args {
					if args[i] == "--expected-execution-tuple" {
						args[i+1] = "sha256:stale"
					}
				}
			case "manifest":
				importQwenTupleFixture(t, f.homePath, "fixture-qwen-quality", "fixture-changed")
			case "selection":
				saveAdaptiveSelection(t, f, candidate)
			}
			before, err := f.store.Get(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			attemptsBefore, err := runattempt.Open(f.homePath).List()
			if err != nil {
				t.Fatal(err)
			}
			stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
				t.Fatal("drift reached readiness")
				return work.DelegateHarnessReadinessResult{}, nil
			})
			type exitSignal struct{ code int }
			prev := workAdvanceExecuteExit
			workAdvanceExecuteExit = func(code int) { panic(exitSignal{code}) }
			t.Cleanup(func() { workAdvanceExecuteExit = prev })
			code := 0
			if drift == "profile" {
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				childArgs := append([]string{"-test.run=^TestAdaptiveDetachedDoorHelper$", "--", "work"}, args...)
				child := exec.Command(executable, childArgs...)
				child.Env = append(os.Environ(), "COMPOUND_TEST_ADAPTIVE_DOOR=1")
				out, err := child.CombinedOutput()
				if err == nil || !strings.Contains(string(out), "no Compound lane tuple") {
					t.Fatalf("missing profile result: %v %s", err, out)
				}
				code = child.ProcessState.ExitCode()
			} else {
				func() {
					defer func() {
						if x := recover(); x != nil {
							if signal, ok := x.(exitSignal); ok {
								code = signal.code
							} else {
								panic(x)
							}
						}
					}()
					cmdWork(args)
				}()
			}
			wantCode := 1
			if drift == "profile" {
				wantCode = 2
			}
			if code != wantCode {
				t.Fatalf("drift exit=%d", code)
			}
			after, err := f.store.Get(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			attemptsAfter, err := runattempt.Open(f.homePath).List()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(attemptsBefore, attemptsAfter) {
				t.Fatal("drift minted or changed a RunAttempt")
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("drift changed envelope")
			}
			if _, err := os.Stat(f.argvLog); !os.IsNotExist(err) {
				t.Fatalf("drift launched child: %v", err)
			}
		})
	}
}

func saveAdaptiveSelection(t *testing.T, f governedClaudeExecutionFixture, c work.ExecutionRouteCandidate) {
	t.Helper()
	_, err := f.store.SaveExecutionSelection(f.workID, 0, envelope.ExecutionSelection{LaunchProfileID: c.LaunchProfileID, LaunchProfileRevision: c.SourceRevision, Harness: c.Harness, ProviderRoute: c.ProviderRoute, RequestedModelID: c.RequestedModelID, Source: "explicit", Revision: 1, SelectedBy: "codex", SelectedAt: cliTestNow.UTC().Format(time.RFC3339)}, "codex", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
}

// The helper is a new process running the real CLI dispatcher. Only external
// permission/hook evidence is synthetic; owner resolution, policy, reservation,
// adapter, subprocess and persisted RunAttempt use their production paths.
func TestAdaptiveDetachedDoorHelper(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_ADAPTIVE_DOOR") != "1" {
		return
	}
	stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true, Evidence: governedClaudeReadyEvidenceFor("claude_qwen")}, nil
	})
	i := 0
	for i < len(os.Args) && os.Args[i] != "--" {
		i++
	}
	if i+2 >= len(os.Args) || os.Args[i+1] != "work" {
		t.Fatal("missing detached work command")
	}
	cmdWork(os.Args[i+2:])
}

func TestWorkAdvanceExecuteDetachedProcessPreservesTuple(t *testing.T) {
	f, candidates := adaptiveTupleFixture(t)
	candidate := candidates[1]
	repo := initCompoundCommandGitRepo(t)
	output, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.store.Create(work.CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: "detached process uses selected tuple", GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: strings.TrimSpace(string(output)), PublicationMode: "local_only"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.workID = created.ID
	if _, err = f.store.Claim(f.workID, "codex", time.Now()); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "compound-test-door")
	body := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run='^TestAdaptiveDetachedDoorHelper$' -- \"$@\"\n"
	if err = os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	previous := executewire.DetachedDoorBinary
	executewire.DetachedDoorBinary = func() (string, error) { return script, nil }
	t.Cleanup(func() { executewire.DetachedDoorBinary = previous })
	t.Setenv("COMPOUND_TEST_ADAPTIVE_DOOR", "1")
	launch, err := executewire.LaunchDetachedCandidateViaDoor(f.store, f.workID, candidate, "selected tuple", "codex", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-launch.PGID, syscall.SIGTERM) })
	deadline := time.Now().Add(15 * time.Second)
	for {
		log, _ := os.ReadFile(launch.LogPath)
		if strings.Contains(string(log), "\nPASS\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached child did not complete: %s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	final, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	attempt := f.executionAttempt(t)
	if final.State != "verifying" || attempt.LaunchProfileID != candidate.LaunchProfileID || attempt.RequestedModelID != candidate.RequestedModelID || attempt.RequestedProviderRoute != candidate.ProviderRoute || filepath.Base(attempt.Command) != "fixture-qwen-quality" || f.argv(t) != "quality\n" {
		t.Fatalf("detached process substituted tuple: state=%s attempt=%+v argv=%q", final.State, attempt, f.argv(t))
	}
}

func TestWorkAdvanceExecuteSynchronousCandidatePreservesTuple(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprintf("drift=%t", drift), func(t *testing.T) {
			f, candidates := adaptiveTupleFixture(t)
			candidate := candidates[1]
			repo := initCompoundCommandGitRepo(t)
			output, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			created, err := f.store.Create(work.CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: "synchronous route uses selected tuple", GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: strings.TrimSpace(string(output)), PublicationMode: "local_only"}}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			f.workID = created.ID
			if _, err = f.store.Claim(f.workID, "codex", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.Advance(f.workID, work.AdvanceInput{To: "running", Actor: "codex"}, time.Now()); err != nil {
				t.Fatal(err)
			}
			previous := executewire.LaunchReadiness
			executewire.LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
				if req.LaunchProfileID != candidate.LaunchProfileID || req.RequestedModelID != candidate.RequestedModelID || req.RequestedProviderRoute != candidate.ProviderRoute {
					t.Fatalf("synchronous preflight substituted tuple: %+v", req)
				}
				if drift {
					importQwenTupleFixture(t, f.homePath, candidate.LaunchProfileID, "fixture-changed")
				}
				return work.DelegateHarnessReadinessResult{Ready: true, Evidence: governedClaudeReadyEvidenceFor("claude_qwen")}, nil
			}
			t.Cleanup(func() { executewire.LaunchReadiness = previous })
			id, err := executewire.LaunchCandidateViaActuator(f.store, f.workID, candidate, "selected tuple", "codex", time.Now())
			if drift {
				if err == nil || !strings.Contains(err.Error(), "execution tuple changed before launch") {
					t.Fatalf("drift result id=%s err=%v", id, err)
				}
				if _, err := os.Stat(f.argvLog); !os.IsNotExist(err) {
					t.Fatalf("drift launched: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			attempt := f.executionAttempt(t)
			if attempt.ID != id || attempt.LaunchProfileID != candidate.LaunchProfileID || attempt.RequestedModelID != candidate.RequestedModelID || attempt.RequestedProviderRoute != candidate.ProviderRoute || f.argv(t) != "quality\n" {
				t.Fatalf("synchronous attempt substituted tuple: %+v", attempt)
			}
		})
	}
}
