package work

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// R6E falsifiers: the unattended execute stage must move past a refused head
// (bounded), and it must never execute work that belongs to another executor.
//
// The bench head is the first ready item in workbench order (state priority,
// then work_id), so every test below names its items so the refused/foreign
// envelope sorts FIRST and the launchable one second — the head-blocked shape
// the production heartbeat lived in for five days (every tick since 2026-09-05
// ended execute=skipped:workspace_unresolvable because the head never cleared).

// seedReadyPickWork seeds one ready envelope with explicit origin identity so
// each test controls the eligibility inputs (owner + creator harness). The
// default owner/actor pair is the seam's own dispatcher shape.
func seedReadyPickWork(t *testing.T, s *Store, id, owner, creatorHarness string) {
	t.Helper()
	w, err := s.Create(CreateInput{
		WorkID:           id,
		Owner:            owner,
		Actor:            owner,
		CreatorHarness:   creatorHarness,
		State:            "ready",
		Risk:             "tier1",
		Privacy:          "private",
		SuccessPredicate: "execute stage pick fixture predicate",
	}, fixedNow)
	if err != nil {
		t.Fatalf("seed ready work %s: %v", id, err)
	}
	if w.State != "ready" {
		t.Fatalf("seeded work %s state = %q, want ready", id, w.State)
	}
}

// openPickGates opens route/capacity/policy the way the sibling execute-stage
// tests do, so the only thing that can stop a launch is the behavior under
// test (head skipping, eligibility, or the pick-attempt bound).
func openPickGates(t *testing.T) {
	t.Helper()
	executeStageRoutePlanFn = func(home, capability string) (string, bool) {
		return "claude-minimax", true
	}
	executeStageCapacityFn = func(home, lane string, now time.Time) (float64, bool, error) {
		return 30, true, nil
	}
	executeStagePolicyEvalFn = func(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_pick_allow"}
	}
}

// refuseWorkspaceFor installs a preflight hook that refuses exactly workID
// with the recognized workspace_unresolvable prefix (the shape the production
// wire emits) and counts how often it fired.
func refuseWorkspaceFor(t *testing.T, workID string, calls *int) {
	t.Helper()
	restore := SwapExecuteStagePreflight(func(_ *Store, id, _ string, _ time.Time) error {
		*calls++
		if id == workID {
			return errors.New("workspace_unresolvable: synthetic git_delivery missing")
		}
		return nil
	})
	t.Cleanup(restore)
}

// TestExecuteStageSkipsARefusedHeadAndLaunchesTheNextReadyEnvelope — the core
// defect. Two ready items; preflight refuses the FIRST with
// workspace_unresolvable; the second must still launch in the SAME stage call,
// the refusal must be recorded (no envelope mutation for the refused head),
// and the result must name BOTH ids so the heartbeat log tells the truth.
func TestExecuteStageSkipsARefusedHeadAndLaunchesTheNextReadyEnvelope(t *testing.T) {
	s := newStore(t)
	const headID = "work_pick_refused_a"
	const nextID = "work_pick_refused_b"
	seedReadyPickWork(t, s, headID, "orchestrator:claude_zai", "claude_code")
	seedReadyPickWork(t, s, nextID, "orchestrator:claude_zai", "claude_code")
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openPickGates(t)
	preflightCalls := 0
	refuseWorkspaceFor(t, headID, &preflightCalls)
	var launched []string
	launchStub = func(_ *Store, workID, _, _, _, _ string, _ time.Time) (string, error) {
		launched = append(launched, workID)
		return "ratt_pick_" + workID, nil
	}

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if res.Action != "execute" {
		t.Fatalf("action = %q, want execute (a refused head must not strand the tick); result=%+v", res.Action, res)
	}
	if res.WorkID != nextID {
		t.Fatalf("work_id = %q, want %s (the next ready envelope)", res.WorkID, nextID)
	}
	if len(launched) != 1 || launched[0] != nextID {
		t.Fatalf("launched = %v, want exactly [%s]", launched, nextID)
	}
	if !strings.Contains(res.Human, headID) || !strings.Contains(res.Human, nextID) {
		t.Fatalf("human line %q must name the skipped id AND the launched id", res.Human)
	}
	if !strings.Contains(res.Human, "workspace_unresolvable") {
		t.Fatalf("human line %q must name the refusal reason", res.Human)
	}
	if len(res.Refused) != 1 || res.Refused[0].WorkID != headID {
		t.Fatalf("refused list = %+v, want exactly [%s]", res.Refused, headID)
	}
	if !strings.HasPrefix(res.Refused[0].Reason, "workspace_unresolvable:") {
		t.Fatalf("refused reason = %q, want the workspace_unresolvable family with detail", res.Refused[0].Reason)
	}
	// The refused head stays ready, lease-free, untouched.
	after, err := s.Get(headID)
	if err != nil {
		t.Fatalf("get head: %v", err)
	}
	if after.State != "ready" {
		t.Fatalf("refused head state = %q, want ready (refusal must not mutate it)", after.State)
	}
	if hasLease(t, s, headID) {
		t.Fatal("refused head must not hold a lease")
	}
}

// TestExecuteStageNeverLaunchesAnEnvelopeOwnedByAnotherHarness — the safety
// seam. A ready item with origin.harness=codex sorts ahead of a ready
// orchestrator item; only the orchestrator item may launch, and the codex
// item must be byte-identical afterwards (no claim, no state change, no
// lease, no events).
func TestExecuteStageNeverLaunchesAnEnvelopeOwnedByAnotherHarness(t *testing.T) {
	s := newStore(t)
	const codexID = "work_pick_foreign_head"
	const orchID = "work_pick_orchestrator_next"
	seedReadyPickWork(t, s, codexID, "codex:omp-offline-falsifier", "codex")
	seedReadyPickWork(t, s, orchID, "orchestrator:claude_zai", "claude_code")
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openPickGates(t)
	var launched []string
	launchStub = func(_ *Store, workID, _, _, _, _ string, _ time.Time) (string, error) {
		launched = append(launched, workID)
		return "ratt_pick_" + workID, nil
	}

	codexPath := filepath.Join(s.HomePath(), "work", "work", codexID+".json")
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex envelope before: %v", err)
	}

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if res.Action != "execute" {
		t.Fatalf("action = %q, want execute (the orchestrator item was launchable); result=%+v", res.Action, res)
	}
	if res.WorkID != orchID {
		t.Fatalf("work_id = %q, want %s (only the orchestrator item)", res.WorkID, orchID)
	}
	if len(launched) != 1 || launched[0] != orchID {
		t.Fatalf("launched = %v, want exactly [%s]; the stage must not run another executor's work", launched, orchID)
	}
	if !strings.Contains(res.Human, codexID) {
		t.Fatalf("human line %q must name the skipped foreign id and why", res.Human)
	}
	if len(res.Refused) != 1 || res.Refused[0].WorkID != codexID {
		t.Fatalf("refused list = %+v, want exactly [%s]", res.Refused, codexID)
	}
	if !strings.HasPrefix(res.Refused[0].Reason, "ineligible:") {
		t.Fatalf("refused reason = %q, want the ineligible family with detail", res.Refused[0].Reason)
	}
	// A launch clears Skipped by contract (it is set only on Action=="none");
	// the refused list, not Skipped, is what names the skip on a success.
	if res.Skipped != "" {
		t.Fatalf("skipped = %q, want empty on a successful launch", res.Skipped)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex envelope after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("codex-owned envelope changed bytes; an item that belongs to another executor must never be mutated")
	}
}

// TestExecuteStageStillLaunchesAtMostOncePerCall — head skipping must not
// multiply launches. Five eligible ready items, every gate open: exactly one
// launch per Continue call (the budget constant stays untouched).
func TestExecuteStageStillLaunchesAtMostOncePerCall(t *testing.T) {
	s := newStore(t)
	ids := []string{
		"work_pick_once_a", "work_pick_once_b", "work_pick_once_c",
		"work_pick_once_d", "work_pick_once_e",
	}
	for _, id := range ids {
		seedReadyPickWork(t, s, id, "orchestrator:claude_zai", "claude_code")
	}
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openPickGates(t)
	launches := 0
	launchStub = func(_ *Store, workID, _, _, _, _ string, _ time.Time) (string, error) {
		launches++
		return "ratt_pick_once_" + workID, nil
	}

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if launches != 1 {
		t.Fatalf("launches = %d, want exactly 1: skipping refused heads must never spend more than the one-launch budget", launches)
	}
	if res.Action != "execute" {
		t.Fatalf("action = %q, want execute", res.Action)
	}
}

// pickAttemptBound pins the packet-settled value of executeStageMaxPickAttempts
// as a literal on purpose: raising the bound must red this test and force a
// conscious decision, the same way the structural launch ceiling is pinned.
const pickAttemptBound = 5

// TestExecuteStageBoundsPickAttempts — six refused heads; the stage stops
// after executeStageMaxPickAttempts (5) examinations, tells the truth about
// it, and launches nothing.
func TestExecuteStageBoundsPickAttempts(t *testing.T) {
	s := newStore(t)
	var ids []string
	for _, suffix := range []string{"a", "b", "c", "d", "e", "f"} {
		id := "work_pick_cap_" + suffix
		ids = append(ids, id)
		seedReadyPickWork(t, s, id, "orchestrator:claude_zai", "claude_code")
	}
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	openPickGates(t)
	preflightCalls := 0
	restore := SwapExecuteStagePreflight(func(_ *Store, _, _ string, _ time.Time) error {
		preflightCalls++
		return errors.New("workspace_unresolvable: every head refuses")
	})
	t.Cleanup(restore)
	launches := 0
	launchStub = func(*Store, string, string, string, string, string, time.Time) (string, error) {
		launches++
		return "ratt_pick_should_not_run", nil
	}

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex"})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if launches != 0 {
		t.Fatalf("launches = %d, want 0 (all heads refused)", launches)
	}
	if preflightCalls != pickAttemptBound {
		t.Fatalf("preflight calls = %d, want %d (the stage must stop after the pick-attempt bound)", preflightCalls, pickAttemptBound)
	}
	// The human line names every examined id and states the bound truthfully.
	for _, id := range ids[:pickAttemptBound] {
		if !strings.Contains(res.Human, id) {
			t.Fatalf("human line %q must name examined id %s", res.Human, id)
		}
	}
	if strings.Contains(res.Human, ids[pickAttemptBound]) {
		t.Fatalf("human line %q must not claim it examined %s (beyond the bound)", res.Human, ids[pickAttemptBound])
	}
	if !strings.Contains(res.Human, "pick attempt") {
		t.Fatalf("human line %q must state the pick-attempt bound", res.Human)
	}
	if len(res.Refused) != pickAttemptBound {
		t.Fatalf("refused list length = %d, want %d (one entry per examined head)", len(res.Refused), pickAttemptBound)
	}
	for _, r := range res.Refused {
		if !strings.HasPrefix(r.Reason, "workspace_unresolvable:") {
			t.Fatalf("refused entry %+v must carry the preflight refusal family", r)
		}
	}
}

// TestUnattendedEligibleRule — the settled R6E eligibility rule as ONE
// function, table-tested. Every row is a (envelope, lane) pair with the
// verdict and the ineligible-why prefix the refused list must carry.
func TestUnattendedEligibleRule(t *testing.T) {
	existingDir := t.TempDir()
	missingRoot := filepath.Join(t.TempDir(), "gone")
	origin := func(harness string) *envelope.WorkOrigin {
		return &envelope.WorkOrigin{Harness: harness, Actor: "fixture"}
	}
	empty := envelope.WorkEnvelope{Owner: ""}
	cases := []struct {
		name      string
		work      envelope.WorkEnvelope
		lane      string
		eligible  bool
		whyPrefix string
	}{
		{
			name:     "empty owner and nil origin passes on a claude lane",
			work:     empty,
			lane:     "claude-minimax",
			eligible: true,
		},
		{
			name:     "origin harness matching the claude lane family passes",
			work:     envelope.WorkEnvelope{Origin: origin("claude_code"), Owner: "orchestrator:claude_zai"},
			lane:     "claude_minimax",
			eligible: true,
		},
		{
			name:     "underscore and hyphen lane spellings agree",
			work:     envelope.WorkEnvelope{Origin: origin("claude_code"), Owner: "claude_code:orchestrator"},
			lane:     "claude-qwen",
			eligible: true,
		},
		{
			name:      "codex origin on a claude lane is ineligible",
			work:      envelope.WorkEnvelope{Origin: origin("codex"), Owner: "codex:omp-offline-falsifier"},
			lane:      "claude_minimax",
			eligible:  false,
			whyPrefix: "ineligible:harness=codex",
		},
		{
			name:      "foreign owner with claude origin is still ineligible",
			work:      envelope.WorkEnvelope{Origin: origin("claude_code"), Owner: "playtest-portfolio"},
			lane:      "claude_minimax",
			eligible:  false,
			whyPrefix: "ineligible:owner=playtest-portfolio",
		},
		{
			name:     "codex origin on the codex lane passes the harness gate",
			work:     envelope.WorkEnvelope{Origin: origin("codex"), Owner: "orchestrator:codex_lane"},
			lane:     "codex",
			eligible: true,
		},
		{
			name:     "no git delivery passes the workspace gate",
			work:     envelope.WorkEnvelope{Origin: origin("claude_code"), Owner: "orchestrator:claude_zai"},
			lane:     "claude-minimax",
			eligible: true,
		},
		{
			name: "git delivery with an existing root passes",
			work: envelope.WorkEnvelope{
				Origin:      origin("claude_code"),
				Owner:       "orchestrator:claude_zai",
				GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: existingDir},
			},
			lane:     "claude-minimax",
			eligible: true,
		},
		{
			name: "git delivery to a missing worktree is ineligible",
			work: envelope.WorkEnvelope{
				Origin:      origin("claude_code"),
				Owner:       "orchestrator:claude_zai",
				GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: missingRoot},
			},
			lane:      "claude-minimax",
			eligible:  false,
			whyPrefix: "ineligible:workspace_missing=",
		},
		{
			name: "git delivery without a root is ineligible",
			work: envelope.WorkEnvelope{
				Origin:      origin("claude_code"),
				Owner:       "orchestrator:claude_zai",
				GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: "  "},
			},
			lane:      "claude-minimax",
			eligible:  false,
			whyPrefix: "ineligible:git_delivery has no target_worktree_root",
		},
		{
			name:     "owner must be trimmed before the dispatcher check",
			work:     envelope.WorkEnvelope{Origin: origin("claude_code"), Owner: "  orchestrator:claude_zai  "},
			lane:     "claude-minimax",
			eligible: true,
		},
		{
			name: "saved selection matching the lane family passes",
			work: envelope.WorkEnvelope{
				Origin: origin("claude_code"), Owner: "orchestrator:claude_zai",
				ExecutionSelection: &envelope.ExecutionSelection{LaunchProfileID: "claude-minimax", Harness: "claude_code", Source: "explicit", Revision: 1, SelectedBy: "op", SelectedAt: "2026-09-15T00:00:00Z"},
			},
			lane:      "claude-minimax",
			eligible:  false,
			whyPrefix: "ineligible:saved execution_selection is not selected profile claude-minimax",
		},
		{
			name: "a saved selection for another harness is ineligible on this lane",
			work: envelope.WorkEnvelope{
				Origin: origin("claude_code"), Owner: "orchestrator:claude_zai",
				ExecutionSelection: &envelope.ExecutionSelection{LaunchProfileID: "omp-minimax", Harness: "oh_my_pi", Source: "explicit", Revision: 1, SelectedBy: "op", SelectedAt: "2026-09-15T00:00:00Z"},
			},
			lane:      "claude-minimax",
			eligible:  false,
			whyPrefix: "ineligible:saved execution_selection is not selected profile claude-minimax",
		},
		{
			name: "a saved codex selection is ineligible on a claude lane",
			work: envelope.WorkEnvelope{
				ExecutionSelection: &envelope.ExecutionSelection{LaunchProfileID: "codex-lane", Harness: "codex", Source: "explicit", Revision: 1, SelectedBy: "op", SelectedAt: "2026-09-15T00:00:00Z"},
			},
			lane:      "claude_minimax",
			eligible:  false,
			whyPrefix: "ineligible:saved execution_selection is not selected profile claude_minimax",
		},
		{
			name: "explicit saved selection authorizes a later executor over creator provenance",
			work: envelope.WorkEnvelope{
				Origin: origin("codex"), Owner: "orchestrator:claude_minimax",
				ExecutionSelection: &envelope.ExecutionSelection{LaunchProfileID: "claude-minimax", Harness: "claude_minimax", Source: "explicit", Revision: 1, SelectedBy: "codex:orchestrator", SelectedAt: "2026-09-22T00:00:00Z"},
			},
			lane:     "claude_minimax",
			eligible: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := unattendedEligible(tc.work, tc.lane)
			if ok != tc.eligible {
				t.Fatalf("unattendedEligible = (%v, %q), want eligible=%v", ok, why, tc.eligible)
			}
			if !tc.eligible && !strings.HasPrefix("ineligible:"+why, tc.whyPrefix) {
				t.Fatalf("why = %q, want prefix %q", why, tc.whyPrefix)
			}
		})
	}
}
