package work

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// TestExecuteStageActorDistinctFromLane — DEFECT 2 red test. The claimant
// actor (authority holder) and the lane (execution attribution) are
// DIFFERENT concepts. Today execute_stage.go claims and advances with the
// caller's actor but then calls
//
//	launchStub(s, ready, lane, lane, prompt, now)
//
// which DROPS the actor. wire.go's LaunchViaActuator then finalizes with
//
//	FinalizeGovernedHarnessRun{...Actor: lane, ...}
//
// replacing the actor with the lane. Leases compare exact strings and the
// heartbeat uses --actor claude-minimax (hyphen) while the canonical lane is
// claude_minimax (underscore) — so a production wire where actor != lane
// either fails the lease-holder advance gate OR finalizes under the wrong
// identity.
//
// The test wires the seam with deliberately DIFFERENT actor and lane
// strings. On the current buggy code this fails because the launchStub
// signature does not carry the actor — the seam-level call passes the
// lane as the actor. The test stubs launchStub to REFUSE if it is called
// without the actor parameter. The red output therefore surfaces as a
// signature mismatch in this test file (compile failure) until the
// launchStub signature is widened to carry the actor.
func TestExecuteStageActorDistinctFromLane(t *testing.T) {
	s := newStore(t)
	const workID = "work_defect2_actor_vs_lane"
	const actor = "actor:test/claude-minimax" // hyphen + slash — NOT a lane
	const lane = "claude_minimax"             // canonical lane

	// Seed a ready envelope with git_delivery so the seam's preflight
	// (DEFECT 1 fix) passes.
	seedReadyExecuteWorkWithGitDelivery(t, s, workID, s.home)

	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)

	executeStageRoutePlanFn = func(home, capability string) (string, bool) {
		return lane, true
	}
	executeStageCapacityFn = func(home, ln string, now time.Time) (float64, bool, error) {
		return 30, true, nil
	}
	executeStagePolicyEvalFn = func(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_defect2_allow"}
	}
	// Preflight (DEFECT 1) accepts everything.
	restorePreflight := SwapExecuteStagePreflight(func(*Store, string, string, time.Time) error { return nil })
	t.Cleanup(restorePreflight)

	// The launchStub signature after the fix carries the actor. We emulate
	// the production wire by advancing the envelope to verifying under the
	// actor and returning a RunAttempt id. The current buggy signature does
	// NOT receive the actor, so the test fails to compile until the seam
	// passes actor through.
	called := 0
	launchStub = func(store *Store, wid, ln, harnessName, prompt, actorArg string, now time.Time) (string, error) {
		called++
		if actorArg != actor {
			return "", errors.New("launchStub received actor=" + actorArg + ", want " + actor)
		}
		// Finalize under the actor (NOT the lane). The bug: execute_stage.go
		// passed lane here, breaking the lease-holder advance gate.
		if _, advErr := store.Advance(wid, AdvanceInput{
			To: "verifying", Actor: actorArg, Note: "defect2 test finalize",
		}, now); advErr != nil {
			return "", advErr
		}
		_ = ln
		_ = harnessName
		_ = prompt
		return "ratt_defect2_actor", nil
	}

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: actor})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if res.Skipped != "" {
		t.Fatalf("skipped = %q, want empty on the green path; result=%+v", res.Skipped, res)
	}
	if res.Action != "execute" {
		t.Fatalf("action = %q, want execute; result=%+v", res.Action, res)
	}
	if res.Status != "verifying" {
		t.Fatalf("status = %q, want verifying (the launch must terminalize under the actor, not the lane)", res.Status)
	}
	if called != 1 {
		t.Fatalf("launch calls = %d, want exactly 1", called)
	}
	if len(res.RunAttemptIDs) != 1 || res.RunAttemptIDs[0] != "ratt_defect2_actor" {
		t.Fatalf("run_attempt_ids = %v, want [ratt_defect2_actor]", res.RunAttemptIDs)
	}

	after, err := s.Get(workID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.State != "verifying" {
		t.Fatalf("envelope state = %q, want verifying", after.State)
	}
}

// seedReadyExecuteWorkWithGitDelivery creates a ready envelope with
// git_delivery attached. Mirrors executewire.seedExecuteStageReadyWorkWithDelivery
// (kept local so the seam package has no executewire dependency).
func seedReadyExecuteWorkWithGitDelivery(t *testing.T, s *Store, id, targetRoot string) {
	t.Helper()
	sha := initDefect2GitRepoOrSkip(t, targetRoot)
	w, err := s.Create(CreateInput{
		WorkID:           id,
		Owner:            "orchestrator:claude_zai",
		Actor:            "claude_code:orchestrator",
		State:            "ready",
		Risk:             "tier1",
		Privacy:          "private",
		SuccessPredicate: "execute stage drives the ready envelope (defect 2)",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: targetRoot,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    sha,
			PublicationMode:    "local_only",
		},
	}, fixedNow)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if w.State != "ready" {
		t.Fatalf("seeded work state = %q, want ready", w.State)
	}
}

// initDefect2GitRepoOrSkip turns targetRoot into a minimal git repo so
// validateBoundGitDelivery accepts it as a workspace. Mirrors
// executewire.initGitRepoOrSkip without pulling the executewire test
// helper into the work package.
func initDefect2GitRepoOrSkip(t *testing.T, targetRoot string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH; cannot init a workspace repo: %v", err)
	}
	for _, args := range [][]string{
		// Pin the fixture branch to refs/heads/main regardless of the
		// machine's init.defaultBranch (CI runners default to master).
		{"init", "--quiet"},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
		{"commit", "--allow-empty", "--quiet", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = targetRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	shaCmd := exec.Command("git", "rev-parse", "HEAD")
	shaCmd.Dir = targetRoot
	out, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}
