package work

import (
	"strings"
	"testing"
	"time"
)

// The execute-stage budget and the truthfulness of what `work continue`
// tells the operator about it.
//
// ContinueRun loops `--max-steps N` calls to Continue, and Continue's
// execute stage can launch one governed harness per call. Nothing counted
// launches ACROSS steps, so the heartbeat's own invocation
// (`--max-steps 3 --max-execute-steps 1`) authorized one launch and
// permitted three. MaxExecuteSteps was documented as a bound and enforced
// nothing.

// countingLaunchStub installs a launch stub that records how many times the
// seam actually launched, and returns a distinct ratt id per call.
func countingLaunchStub(t *testing.T, count *int) {
	t.Helper()
	launchStub = func(_ *Store, workID, _, _, _, _ string, _ time.Time) (string, error) {
		*count++
		return "ratt_budget_" + workID, nil
	}
}

// openAllExecuteGates opens route, capacity and policy so the only thing
// that can stop a launch is the budget under test.
func openAllExecuteGates(t *testing.T) {
	t.Helper()
	executeStageRoutePlanFn = func(home, capability string) (string, bool) { return "claude-minimax", true }
	executeStageCapacityFn = func(home, lane string, now time.Time) (float64, bool, error) { return 30, true, nil }
	executeStagePolicyEvalFn = func(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_budget_allow"}
	}
}

// TestContinueRunHonorsExecuteBudgetAcrossSteps — the heartbeat's exact
// shape. Three ready envelopes are on the bench and the operator authorized
// ONE execute step across a three-step run. The run must launch exactly
// once; the remaining steps keep doing safe lifecycle work.
func TestContinueRunHonorsExecuteBudgetAcrossSteps(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"work_budget_a", "work_budget_b", "work_budget_c"} {
		seedReadyExecuteWork(t, s, id)
	}
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openAllExecuteGates(t)
	launches := 0
	countingLaunchStub(t, &launches)

	res, err := s.ContinueRun(fixedNow.Add(time.Minute), ContinueRunOptions{
		ContinueOptions: ContinueOptions{Actor: "codex", MaxExecuteSteps: 1},
		MaxSteps:        3,
	})
	if err != nil {
		t.Fatalf("continue-run: %v", err)
	}
	if launches != 1 {
		t.Fatalf("execute-stage launches = %d, want 1: --max-execute-steps 1 authorized ONE launch but the run performed %d across %d steps (result status=%s steps=%d)",
			launches, launches, res.StepCount, res.Status, res.StepCount)
	}
}

// TestContinueRunNeverExceedsTheStructuralLaunchCeiling — the ceiling is
// structural, not a coincidence of the fixture. Even with three ready
// envelopes and a request for two execute steps, a run launches at most
// executeStageMaxLaunchesPerRun. This test exists to make raising that
// constant a DELIBERATE act: bumping it to allow more unattended launches
// per heartbeat tick reds this test, which is the point.
func TestContinueRunNeverExceedsTheStructuralLaunchCeiling(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"work_budget2_a", "work_budget2_b", "work_budget2_c"} {
		seedReadyExecuteWork(t, s, id)
	}
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openAllExecuteGates(t)
	launches := 0
	countingLaunchStub(t, &launches)

	if _, err := s.ContinueRun(fixedNow.Add(time.Minute), ContinueRunOptions{
		ContinueOptions: ContinueOptions{Actor: "codex", MaxExecuteSteps: 2},
		MaxSteps:        3,
	}); err != nil {
		t.Fatalf("continue-run: %v", err)
	}
	if launches > executeStageMaxLaunchesPerRun {
		t.Fatalf("execute-stage launches = %d, exceeds the structural ceiling of %d", launches, executeStageMaxLaunchesPerRun)
	}
	if launches != executeStageMaxLaunchesPerRun {
		t.Fatalf("execute-stage launches = %d, want %d: the fixture opened every gate, so a shortfall means the seam stopped for an unmodelled reason", launches, executeStageMaxLaunchesPerRun)
	}
}

// TestEffectiveExecuteBudgetNeverPromisesMoreThanTheCeiling — the number the
// operator is shown must be the number the system can deliver, so a request
// above the ceiling is clamped rather than echoed back.
func TestEffectiveExecuteBudgetNeverPromisesMoreThanTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ContinueOptions
		want int
	}{
		{"flag omitted means the documented default", ContinueOptions{}, 1},
		{"explicit one", ContinueOptions{MaxExecuteSteps: 1}, 1},
		{"request above the ceiling is clamped", ContinueOptions{MaxExecuteSteps: 9}, executeStageMaxLaunchesPerRun},
		{"dry run spends nothing", ContinueOptions{MaxExecuteSteps: 3, DryRun: true}, 0},
	} {
		if got := effectiveExecuteBudget(tc.opts); got != tc.want {
			t.Errorf("%s: effectiveExecuteBudget = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestContinueWarningsNeverDenyAnExecutionThatHappened — the operator-facing
// lie. `work continue` stated "does not execute task commands" on the very
// result that carried a minted RunAttempt. A gate the operator cannot read
// is not a gate.
func TestContinueWarningsNeverDenyAnExecutionThatHappened(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_warn_exec")
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openAllExecuteGates(t)
	launches := 0
	countingLaunchStub(t, &launches)

	// MaxExecuteSteps deliberately UNSET — this is the shape that surprised
	// us: omitting the flag still permits the documented default of one
	// execute step, so the warning must not claim otherwise.
	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if launches == 0 {
		t.Fatalf("fixture did not exercise the execute stage; the warning assertion below would be vacuous")
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "does not execute") {
			t.Fatalf("result launched %d harness(es) (run_attempt_ids=%v) while warning %q told the operator it does not execute", launches, res.RunAttemptIDs, w)
		}
	}
}

// TestContinueWarningsStillDenyExecutionUnderDryRun — the mirror. A dry-run
// probe genuinely launches nothing, and the warning must say so rather than
// promise an execution the caller disabled.
func TestContinueWarningsStillDenyExecutionUnderDryRun(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_warn_dry")
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openAllExecuteGates(t)
	launches := 0
	countingLaunchStub(t, &launches)

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex", MaxExecuteSteps: 1, DryRun: true})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if launches != 0 {
		t.Fatalf("dry run launched %d harness(es); it must launch none", launches)
	}
	joined := strings.Join(res.Warnings, " | ")
	if strings.Contains(joined, "may launch") {
		t.Fatalf("dry-run warnings promise a launch that cannot happen: %s", joined)
	}
}
