package work

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestExecuteStagePreflightRefusalLeavesEnvelopeByteIdentical — DEFECT 1 red
// test. The preflight (adapter enrollment / workspace resolvability / lane
// readiness) must run BEFORE the seam claims or advances the envelope. On
// any recognized refusal, the envelope MUST stay in `ready` with no lease,
// no RunAttempt, and no event mutation. Today the seam does:
//
//	Claim(ready, actor, now)
//	-> Advance(ready, {To: "running", Actor: actor})
//	-> launchStub(...)
//
// so a preflight refusal would leave the envelope stranded in `running`
// holding a lease (the exact failure mode the release audit found).
//
// The test wires a preflight hook to refuse with each of the three named
// refusal prefixes (lane_unready:, workspace_unresolvable:, adapter_missing:)
// — the same prefixes the production wire emits. After Continue, the envelope
// must remain in `ready` and the launchStub must NOT be called. On the
// current buggy code this fails: the envelope is left in `running`.
func TestExecuteStagePreflightRefusalLeavesEnvelopeByteIdentical(t *testing.T) {
	t.Run("lane_unready_refusal", func(t *testing.T) {
		s := newStore(t)
		seedReadyExecuteWork(t, s, "work_defect1_lane_unready")
		installExecuteGrant(t, s.home, true)
		restoreAllExecuteHooks(t)
		executeStageRoutePlanFn = func(home, capability string) (lane string, ok bool) {
			return "claude-minimax", true
		}
		executeStageCapacityFn = func(home, lane string, now time.Time) (usedPercent float64, fresh bool, err error) {
			return 30, true, nil
		}
		executeStagePolicyEvalFn = func(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim {
			return policyDecisionShim{Outcome: "allow", ID: "pol_defect1_allow"}
		}
		// The preflight hook refuses with the same skip-prefix the production
		// wire would emit on a lane_unready refusal. With the bug, the seam
		// skips preflight and runs launchStub, advancing the envelope to
		// running before the refusal surfaces. With the fix, the seam runs
		// preflight first and never touches the envelope.
		restorePreflight := SwapExecuteStagePreflight(func(*Store, string, string, time.Time) error {
			return errors.New("lane_unready: synthetic claude tuple not installed")
		})
		t.Cleanup(restorePreflight)
		launchStub = func(*Store, string, string, string, string, string, time.Time) (string, error) {
			t.Error("launchStub must NOT be called when preflight refuses")
			return "", nil
		}

		before, err := s.Get("work_defect1_lane_unready")
		if err != nil {
			t.Fatalf("get before: %v", err)
		}
		if before.State != "ready" {
			t.Fatalf("precondition: envelope state = %q, want ready", before.State)
		}
		beforeState := before.State
		beforeLease := hasLease(t, s, "work_defect1_lane_unready")
		if beforeLease {
			t.Fatal("precondition: ready envelope must not hold a lease")
		}

		res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex"})
		if err != nil {
			t.Fatalf("continue: %v", err)
		}
		if res.Skipped != "lane_unready" {
			t.Fatalf("skipped = %q, want lane_unready; result=%+v", res.Skipped, res)
		}
		if res.RunAttemptIDs != nil && len(res.RunAttemptIDs) != 0 {
			t.Fatalf("run_attempt_ids = %v, want empty on a refused preflight", res.RunAttemptIDs)
		}
		if !strings.HasPrefix(res.Human, "did nothing: ") {
			t.Fatalf("human line %q must start with did nothing:", res.Human)
		}
		if !strings.Contains(res.Human, "synthetic claude tuple not installed") {
			t.Fatalf("human line %q must surface the preflight refusal detail", res.Human)
		}

		after, err := s.Get("work_defect1_lane_unready")
		if err != nil {
			t.Fatalf("get after: %v", err)
		}
		if after.State != beforeState {
			t.Fatalf("envelope state changed under preflight refusal: %q -> %q; the seam claimed or advanced before the refusal", beforeState, after.State)
		}
		if hasLease(t, s, "work_defect1_lane_unready") != beforeLease {
			t.Fatal("lease state changed under preflight refusal; the seam claimed before the refusal")
		}
	})

	t.Run("workspace_unresolvable_refusal", func(t *testing.T) {
		s := newStore(t)
		seedReadyExecuteWork(t, s, "work_defect1_workspace")
		installExecuteGrant(t, s.home, true)
		restoreAllExecuteHooks(t)
		executeStageRoutePlanFn = func(home, capability string) (lane string, ok bool) {
			return "claude-minimax", true
		}
		executeStageCapacityFn = func(home, lane string, now time.Time) (usedPercent float64, fresh bool, err error) {
			return 30, true, nil
		}
		executeStagePolicyEvalFn = func(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim {
			return policyDecisionShim{Outcome: "allow", ID: "pol_defect1_allow_ws"}
		}
		restorePreflight := SwapExecuteStagePreflight(func(*Store, string, string, time.Time) error {
			return errors.New("workspace_unresolvable: synthetic git_delivery missing")
		})
		t.Cleanup(restorePreflight)
		launchStub = func(*Store, string, string, string, string, string, time.Time) (string, error) {
			t.Error("launchStub must NOT be called when preflight refuses")
			return "", nil
		}

		res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex"})
		if err != nil {
			t.Fatalf("continue: %v", err)
		}
		if res.Skipped != "workspace_unresolvable" {
			t.Fatalf("skipped = %q, want workspace_unresolvable; result=%+v", res.Skipped, res)
		}
		after, err := s.Get("work_defect1_workspace")
		if err != nil {
			t.Fatalf("get after: %v", err)
		}
		if after.State != "ready" {
			t.Fatalf("envelope state = %q, want ready (preflight refused BEFORE any mutation)", after.State)
		}
		if hasLease(t, s, "work_defect1_workspace") {
			t.Fatal("lease granted under a workspace_unresolvable refusal; the seam claimed before the refusal")
		}
	})

	t.Run("adapter_missing_refusal", func(t *testing.T) {
		s := newStore(t)
		seedReadyExecuteWork(t, s, "work_defect1_adapter")
		installExecuteGrant(t, s.home, true)
		restoreAllExecuteHooks(t)
		executeStageRoutePlanFn = func(home, capability string) (lane string, ok bool) {
			return "claude-minimax", true
		}
		executeStageCapacityFn = func(home, lane string, now time.Time) (usedPercent float64, fresh bool, err error) {
			return 30, true, nil
		}
		executeStagePolicyEvalFn = func(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim {
			return policyDecisionShim{Outcome: "allow", ID: "pol_defect1_allow_ad"}
		}
		restorePreflight := SwapExecuteStagePreflight(func(*Store, string, string, time.Time) error {
			return errors.New("adapter_missing:synthetic_harness")
		})
		t.Cleanup(restorePreflight)
		launchStub = func(*Store, string, string, string, string, string, time.Time) (string, error) {
			t.Error("launchStub must NOT be called when preflight refuses")
			return "", nil
		}

		res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex"})
		if err != nil {
			t.Fatalf("continue: %v", err)
		}
		if res.Skipped != "adapter_missing" {
			t.Fatalf("skipped = %q, want adapter_missing; result=%+v", res.Skipped, res)
		}
		after, err := s.Get("work_defect1_adapter")
		if err != nil {
			t.Fatalf("get after: %v", err)
		}
		if after.State != "ready" {
			t.Fatalf("envelope state = %q, want ready (preflight refused BEFORE any mutation)", after.State)
		}
		if hasLease(t, s, "work_defect1_adapter") {
			t.Fatal("lease granted under an adapter_missing refusal; the seam claimed before the refusal")
		}
	})
}

// hasLease reports whether a lease file exists for workID. The execute stage's
// preflight refusal must NEVER cause a lease to be granted — the claim path
// is the only thing that creates one. We probe via the lease JSON file so the
// test does not couple to a private lease accessor.
func hasLease(t *testing.T, s *Store, workID string) bool {
	t.Helper()
	dir := s.HomePath() + "/work/leases"
	if _, err := os.Stat(dir + "/" + workID + ".json"); err == nil {
		return true
	}
	return false
}
