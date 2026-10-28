package hook

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// TestPreToolUseHoldMintsOperatorApprovalFrontierRequestAndMovesAllTimeCountOffSeed
// is the work_dcbe604d1916 falsifier: one blocked action produces a frontier
// entry whose id appears in the agent-visible text, and the all-time
// action-approval count moves off 3.
//
// Extended 2026-08-19 as the red-first falsifier for retiring the fabricated
// resolve command: the mint used to carry "compound policy
// operator-action-approve <id> --granted-by operator:<id>" — a verb that does
// not exist on any CLI surface — and the operator-facing PolicyDecision
// reasons instructed the operator to run it. An instruction that cannot be
// executed is worse than none: it converts a legible hold into a dead end.
// The frontier registry on a live home has never recorded a mint
// (action_approvals: []), so no persisted artifact depends on the field.
//
// Red output at this commit:
//
//	frontier registry names the nonexistent verb compound policy operator-action-approve
//	operator-facing PolicyDecision reasons instruct the nonexistent verb operator-action-approve
func TestPreToolUseHoldMintsOperatorApprovalFrontierRequestAndMovesAllTimeCountOffSeed(t *testing.T) {
	homePath := initHookApprovalHome(t)
	if err := policy.SeedOperatorActionApprovalFrontier(homePath, 3, fixedNow); err != nil {
		t.Fatalf("seed frontier count=3: %v", err)
	}
	preCount, err := policy.OperatorActionApprovalCount(homePath)
	if err != nil {
		t.Fatalf("read pre-count: %v", err)
	}
	if preCount != 3 {
		t.Fatalf("pre-count=%d, want seed of 3", preCount)
	}

	deps := baseDeps()
	deps.HomePath = homePath
	action := approvalPlanHoldAction("act_pretool_frontier_mint", "work_pretool_frontier_mint")
	res, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Enforced: true,
		WorkID: action.IDs.WorkID, Action: &action,
	}, deps, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("run pre-tool-use hold: %v", err)
	}
	pd := res.Produced[1].(*envelope.PolicyDecision)
	if pd.Outcome != "hold" {
		t.Fatalf("policy outcome=%q, want hold", pd.Outcome)
	}

	postCount, err := policy.OperatorActionApprovalCount(homePath)
	if err != nil {
		t.Fatalf("read post-count: %v", err)
	}
	if postCount == preCount {
		t.Fatalf("all-time approval count=%d unchanged after one blocked action; want off %d", postCount, preCount)
	}
	if postCount != preCount+1 {
		t.Fatalf("all-time approval count=%d, want %d (seed + 1)", postCount, preCount+1)
	}

	// Frontier entry must include the request id and nothing that instructs a
	// command that does not exist. The seed only sets the count, not the
	// entries; the mint is the only entry, so the slice length must equal 1
	// (one mint) regardless of seed.
	frontier, err := policy.LoadOperatorApprovalFrontierFromHome(homePath)
	if err != nil {
		t.Fatalf("load frontier: %v", err)
	}
	if len(frontier.ActionApprovals) != 1 {
		t.Fatalf("frontier action_approvals=%d, want 1 (the just-minted entry)", len(frontier.ActionApprovals))
	}
	mint := frontier.ActionApprovals[0]
	if mint.ActionIntentID != action.ID {
		t.Fatalf("minted intent_id=%q, want %q", mint.ActionIntentID, action.ID)
	}
	if !strings.HasPrefix(mint.ID, "opactreq_") {
		t.Fatalf("minted id=%q, want opactreq_ prefix", mint.ID)
	}
	// The persisted registry itself must not name the nonexistent verb — not
	// in the entry, not anywhere. Checked against raw bytes so a renamed or
	// re-added field cannot smuggle it back.
	rawFrontier, err := os.ReadFile(policy.OperatorApprovalFrontierPath(homePath))
	if err != nil {
		t.Fatalf("read frontier registry: %v", err)
	}
	if strings.Contains(string(rawFrontier), "operator-action-approve") {
		t.Fatalf("frontier registry names the nonexistent verb compound policy operator-action-approve: %s", string(rawFrontier))
	}

	// Agent-visible text surfaces the request id — and only the id — so the agent
	// can wait, take an alternative, or record blocked-with-ticket against a stable
	// reference. No command token may reach the agent: handing a held agent
	// an approve command would let it drive its own approval path.
	decision, reason := NativePermissionDecision(res)
	if decision != "ask" {
		t.Fatalf("permission decision=%q, want ask", decision)
	}
	if !strings.Contains(reason, mint.ID) {
		t.Fatalf("agent-visible reason=%q does not contain request id %q", reason, mint.ID)
	}
	for _, forbidden := range []string{"action-approve", "--granted-by", "operator:", "human:"} {
		if strings.Contains(strings.ToLower(reason), forbidden) {
			t.Fatalf("agent-visible reason exposed forbidden command token %q: %q", forbidden, reason)
		}
	}
	// Operator-facing PolicyDecision reasons must keep the door legible — the
	// frontier registry ref — without instructing the nonexistent verb.
	for _, r := range pd.Reasons {
		if strings.Contains(r, "operator-action-approve") {
			t.Fatalf("operator-facing PolicyDecision reasons instruct the nonexistent verb operator-action-approve: %+v", pd.Reasons)
		}
	}
	namesRegistry := false
	for _, r := range pd.Reasons {
		if strings.Contains(r, "compound://registries/operator-approval-frontier") {
			namesRegistry = true
			break
		}
	}
	if !namesRegistry {
		t.Fatalf("operator-facing PolicyDecision reasons=%+v do not name the operator-approval-frontier registry: the hold loses its legible door", pd.Reasons)
	}

	// Evidence refs must also carry the frontier ref so downstream tooling can
	// resolve the entry without scanning the agent-visible prose.
	wantFrontierRef := "compound://registries/operator-approval-frontier#" + mint.ID
	found := false
	for _, ref := range pd.EvidenceRefs {
		if ref == wantFrontierRef {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("PolicyDecision evidence refs=%v, want %q", pd.EvidenceRefs, wantFrontierRef)
	}
	if found {
		for _, effect := range res.Event.SideEffects {
			if effect == wantFrontierRef {
				return
			}
		}
		// Side-effects include the plan ref plus the frontier ref; both must be present.
		hasFrontierSideEffect := false
		for _, effect := range res.Event.SideEffects {
			if strings.HasPrefix(effect, "compound://registries/operator-approval-frontier#") {
				hasFrontierSideEffect = true
				break
			}
		}
		if !hasFrontierSideEffect {
			t.Fatalf("HookEvent side effects=%v missing operator-approval-frontier ref", res.Event.SideEffects)
		}
	}
}

// TestOperatorApprovalFrontierMintFailureLeavesHoldAuthoritative verifies the
// gate stays authoritative when the frontier registry write fails: when the
// hook cannot mint (because the registry path is unwritable) the original
// hold must still surface with a bounded reason instead of a runtime error.
func TestOperatorApprovalFrontierMintFailureLeavesHoldAuthoritative(t *testing.T) {
	// Use a home path that exists but make the registry directory unwritable
	// so the atomic write fails. t.TempDir is on a writable mount; we make
	// the registries directory read-only to force the mint to fail.
	homePath := initHookApprovalHome(t)
	readOnlyDir := homePath + "/registries"
	if err := os.Chmod(readOnlyDir, 0o500); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnlyDir, 0o755) })

	deps := baseDeps()
	deps.HomePath = homePath
	action := approvalPlanHoldAction("act_pretool_frontier_unwritable", "work_pretool_frontier_unwritable")
	res, err := Run(Fixture{
		Hook: "pre-tool-use", Harness: "codex", Enforced: true,
		WorkID: action.IDs.WorkID, Action: &action,
	}, deps, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("run pre-tool-use hold: %v", err)
	}
	pd := res.Produced[1].(*envelope.PolicyDecision)
	if pd.Outcome != "hold" {
		t.Fatalf("policy outcome=%q, want hold", pd.Outcome)
	}
	joined := strings.Join(pd.Reasons, "; ")
	if !strings.Contains(joined, "operator-approval-frontier registry unavailable for operator review") {
		t.Fatalf("unwritable-home reasons=%q, want bounded frontier-unavailable suffix", joined)
	}
}

// TestOperatorApprovalFrontierPersistedActionApprovalsAreDeterministic ensures
// repeated mints on the same home path always append, never overwrite, so the
// all-time count is monotonic and the falsifier seed moves by exactly one.
func TestOperatorApprovalFrontierPersistedActionApprovalsAreDeterministic(t *testing.T) {
	homePath := initHookApprovalHome(t)
	if err := policy.SeedOperatorActionApprovalFrontier(homePath, 3, fixedNow); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deps := baseDeps()
	deps.HomePath = homePath
	for i, actionID := range []string{"act_one", "act_two", "act_three"} {
		action := approvalPlanHoldAction(actionID, "work_pretool_deterministic")
		if _, err := Run(Fixture{
			Hook: "pre-tool-use", Harness: "codex", Enforced: true,
			WorkID: action.IDs.WorkID, Action: &action,
		}, deps, fixedNow.Add(time.Duration(i+1)*time.Second)); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	count, err := policy.OperatorActionApprovalCount(homePath)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 6 {
		t.Fatalf("all-time count=%d, want 6 (3 seed + 3 mints)", count)
	}
	raw, err := readOperatorApprovalFrontierRaw(t, homePath)
	if err != nil {
		t.Fatalf("read raw frontier: %v", err)
	}
	if got := int(raw["action_approval_count"].(float64)); got != 6 {
		t.Fatalf("persisted action_approval_count=%d, want 6", got)
	}
	approvals, ok := raw["action_approvals"].([]interface{})
	if !ok || len(approvals) != 3 {
		t.Fatalf("persisted action_approvals=%v, want 3 entries (3 mints, seed doesn't add entries)", raw["action_approvals"])
	}
}

func readOperatorApprovalFrontierRaw(t *testing.T, homePath string) (map[string]interface{}, error) {
	t.Helper()
	path := policy.OperatorActionApprovalFrontierPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
