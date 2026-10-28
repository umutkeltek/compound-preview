package hook

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/policy"
)

// An "ask" is only a gate where a human can answer it. On a lane running in a
// detached pane there is nobody to press a key, so the lane sits on the prompt
// while its heartbeat, its transcript, and its run attempt all keep reporting
// "running" -- an outage that reads as work in progress. A deny carrying the
// same reason and the same approval-request id is strictly more actionable: the
// agent can take an alternative or record blocked-with-ticket against a stable
// reference.
//
// These tests run through the real Run path, not a hand-built Result, so a
// change that stops producing the PolicyDecision (or stops minting the request)
// fails here rather than passing on a fixture.

func postureHoldResult(t *testing.T) (Result, *envelope.PolicyDecision) {
	t.Helper()
	homePath := initHookApprovalHome(t)
	deps := baseDeps()
	deps.HomePath = homePath
	action := approvalPlanHoldAction("act_posture_projection", "work_posture_projection")
	res, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Enforced: true,
		WorkID: action.IDs.WorkID, Action: &action,
	}, deps, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("run pre-tool-use hold: %v", err)
	}
	pd := res.Produced[1].(*envelope.PolicyDecision)
	if pd.Outcome != "hold" {
		t.Fatalf("policy outcome=%q, want hold -- this test needs a real hold to project", pd.Outcome)
	}
	return res, pd
}

// TestUnattendedPostureProjectsHoldToDenyWithTheSameReason is the falsifier for
// the freeze: same hold, same words, only the projection differs.
func TestUnattendedPostureProjectsHoldToDenyWithTheSameReason(t *testing.T) {
	res, _ := postureHoldResult(t)

	askDecision, askReason := NativePermissionDecisionForPosture(res, harness.PostureInteractive)
	denyDecision, denyReason := NativePermissionDecisionForPosture(res, harness.PostureUnattended)

	if askDecision != "ask" {
		t.Fatalf("interactive projection=%q, want ask", askDecision)
	}
	if denyDecision != "deny" {
		t.Fatalf("unattended projection=%q, want deny -- an unanswerable prompt freezes the lane", denyDecision)
	}
	if denyReason != askReason {
		t.Fatalf("posture changed the REASON, not just the projection:\n ask:  %q\n deny: %q", askReason, denyReason)
	}
	if strings.TrimSpace(denyReason) == "" {
		t.Fatalf("unattended deny carries no reason; the agent has nothing to act on")
	}
}

// TestUnattendedDenyStillCarriesTheApprovalRequestID: the deny has to leave the
// agent a stable reference, or "blocked" becomes unrecoverable rather than
// merely blocked. The resolve command must still stay out of it -- handing a
// held agent its own approval path is the thing the ask projection never did.
func TestUnattendedDenyStillCarriesTheApprovalRequestID(t *testing.T) {
	homePath := initHookApprovalHome(t)
	deps := baseDeps()
	deps.HomePath = homePath
	action := approvalPlanHoldAction("act_posture_frontier", "work_posture_frontier")
	res, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Enforced: true,
		WorkID: action.IDs.WorkID, Action: &action,
	}, deps, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("run pre-tool-use hold: %v", err)
	}
	frontier, err := policy.LoadOperatorApprovalFrontierFromHome(homePath)
	if err != nil {
		t.Fatalf("load frontier: %v", err)
	}
	if len(frontier.ActionApprovals) != 1 {
		t.Fatalf("frontier action_approvals=%d, want the one minted request", len(frontier.ActionApprovals))
	}
	mint := frontier.ActionApprovals[0]

	decision, reason := NativePermissionDecisionForPosture(res, harness.PostureUnattended)
	if decision != "deny" {
		t.Fatalf("unattended projection=%q, want deny", decision)
	}
	if !strings.Contains(reason, mint.ID) {
		t.Fatalf("unattended deny reason=%q drops the approval request id %q", reason, mint.ID)
	}
	for _, forbidden := range []string{"action-approve", "--granted-by", "operator:", "human:"} {
		if strings.Contains(strings.ToLower(reason), forbidden) {
			t.Fatalf("unattended deny leaked the approve path %q: %q", forbidden, reason)
		}
	}
}

// TestPostureResolvesFromTheRegistryNotTheCallSite: the live adapter knows a
// harness NAME, and the registry is the single place that records what that
// name's posture is. If the seam took a posture string from the call site
// instead, every call site would become a second place to get it wrong.
func TestPostureResolvesFromTheRegistryNotTheCallSite(t *testing.T) {
	res, _ := postureHoldResult(t)
	reg := harness.DefaultRegistry()

	for _, tc := range []struct {
		harnessName string
		want        string
	}{
		{"claude_code", "ask"},
		{"claude-code", "ask"},
		{"codex", "ask"},
		{"claude_minimax", "deny"},
		{"claude_qwen", "deny"},
		{"claude_kimi", "deny"},
		{"claude_zai", "deny"},
		{"prime_agent", "deny"},
		{"hermes", "deny"},
		{"opencode", "deny"},
		{"a_harness_nobody_registered", "deny"},
	} {
		t.Run(tc.harnessName, func(t *testing.T) {
			decision, _ := NativePermissionDecisionForHarness(res, reg, tc.harnessName)
			if decision != tc.want {
				t.Fatalf("projection for %q = %q, want %q", tc.harnessName, decision, tc.want)
			}
		})
	}
}

// TestPostureNeverTurnsADenyIntoAnAsk: posture only ever decides what a HOLD
// projects to. A deny is a deny in both postures, and an allowed decision keeps
// emitting no permission decision at all.
func TestPostureNeverTurnsADenyIntoAnAsk(t *testing.T) {
	denied := Result{Produced: []envelope.Envelope{&envelope.PolicyDecision{
		Header:  envelope.Header{ID: "pol_denied"},
		Outcome: "deny",
		Reasons: []string{"target path matches forbidden pattern"},
	}}}
	for _, posture := range []string{harness.PostureInteractive, harness.PostureUnattended} {
		if decision, _ := NativePermissionDecisionForPosture(denied, posture); decision != "deny" {
			t.Fatalf("posture %q turned a deny into %q", posture, decision)
		}
	}

	allowed := Result{Produced: []envelope.Envelope{&envelope.PolicyDecision{
		Header:      envelope.Header{ID: "pol_allowed"},
		Outcome:     "allow_with_obligations",
		Obligations: []string{"emit a Receipt"},
	}}}
	for _, posture := range []string{harness.PostureInteractive, harness.PostureUnattended} {
		if decision, _ := NativePermissionDecisionForPosture(allowed, posture); decision != "" {
			t.Fatalf("posture %q emitted %q for an allowed action", posture, decision)
		}
	}
}

// TestWatchdogEnforcedBlockProjectsToDenyWithItsSummary: the budget watchdog
// answers pre-tool-use with an enforced block and NO typed PolicyDecision.
// The projection must still be a deny carrying the watchdog summary, or
// fail-closed harness shims surface their generic "compound policy gate
// unavailable" instead of the budget reason — the exact failure shape the
// omp minimax sessions hit on 2026-09-15 (bash policy path >6s in a large
// repo, every block reported as "gate unavailable").
func TestWatchdogEnforcedBlockProjectsToDenyWithItsSummary(t *testing.T) {
	answer := BudgetExpiryAnswer(BudgetCommand{
		HookName: "pre-tool-use", Harness: "oh_my_pi", Enforce: true, Live: true,
	}, 6*time.Second, fixedNow)
	if answer.Result.Event.Status != "blocked" || len(answer.Result.Produced) != 0 {
		t.Fatalf("fixture must be a decision-less enforced block, got %+v", answer.Result.Event)
	}
	for _, posture := range []string{harness.PostureInteractive, harness.PostureUnattended} {
		decision, reason := NativePermissionDecisionForPosture(answer.Result, posture)
		if decision != "deny" {
			t.Fatalf("posture %q projected watchdog block as %q, want deny", posture, decision)
		}
		if !strings.Contains(reason, "wall-clock budget") {
			t.Fatalf("reason must name the budget, got %q", reason)
		}
	}

	// Observation (non-enforced) budget answers stay fail-open and must not
	// grow a decision: there is no enforced block to project.
	observed := BudgetExpiryAnswer(BudgetCommand{
		HookName: "pre-tool-use", Harness: "oh_my_pi", Live: true,
	}, 6*time.Second, fixedNow)
	if decision, _ := NativePermissionDecisionForPosture(observed.Result, harness.PostureInteractive); decision != "" {
		t.Fatalf("fail-open watchdog answer projected %q, want no decision", decision)
	}
}
