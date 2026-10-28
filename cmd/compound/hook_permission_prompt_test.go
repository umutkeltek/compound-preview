package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/policy"
)

func TestLivePreToolUseSanitizesMalformedApprovalAuthority(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	// Deliberately bypass the public write door: valid writers cannot create
	// malformed approval authority, and this control must exercise its reader.
	malformedPath := filepath.Join(homePath, "events", "approval_grant_raw_marker.json")
	if err := os.WriteFile(malformedPath, []byte("{raw_marker"), 0o644); err != nil {
		t.Fatalf("write malformed approval authority: %v", err)
	}
	grantsBefore := len(testEventEntries(t, homePath, "approval_grant_"))

	input := `{"hook_event_name":"PreToolUse","session_id":"sess_malformed_approval_authority","cwd":` +
		quoteJSON(t.TempDir()) +
		`,"tool_name":"browser.submit","tool_schema_revision":"builtin:browser.submit","tool_input":{"target":"publish","form":"permission-repro"}}`
	var code int
	out := captureStdout(t, func() {
		withStdin(t, input, func() {
			code = runHookCommand([]string{
				"pre-tool-use", "--live", "--harness", "codex", "--enforce",
			})
		})
	})
	if code == 0 {
		t.Fatalf("enforced PreToolUse exit=0, want policy gate exit\n%s", out)
	}
	var response claudeLiveHookResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatalf("parse enforced response: %v\n%s", err, out)
	}
	if got := response.HookSpecificOutput.PermissionDecision; got == "" {
		t.Fatalf("malformed authority failed open: %+v", response.HookSpecificOutput)
	}
	if got := response.HookSpecificOutput.PermissionDecisionReason; !strings.Contains(got, "baseline for mutation_class") {
		t.Fatalf("malformed authority reason=%q, want ordinary baseline hold", got)
	}
	if plans := readExactApprovalPlans(t, homePath); len(plans) != 0 {
		t.Fatalf("malformed authority created approval plans: %+v", plans)
	}
	if grantsAfter := len(testEventEntries(t, homePath, "approval_grant_")); grantsAfter != grantsBefore {
		t.Fatalf("malformed authority created approval grants: before=%d after=%d", grantsBefore, grantsAfter)
	}
	if uses := testEventEntries(t, homePath, "approval_use_"); len(uses) != 0 {
		t.Fatalf("malformed authority created approval consumption records: %+v", uses)
	}

	lowerOutput := strings.ToLower(out)
	for _, forbidden := range []string{
		"raw_marker",
		"approval_grant_raw_marker",
		"parse",
		strings.ToLower(homePath),
		"action-approve",
		"--granted-by",
		"operator:",
		"human:",
		"exact approval plan",
		"approval_plan_",
	} {
		if strings.Contains(lowerOutput, forbidden) {
			t.Fatalf("native response exposed malformed approval detail %q: %s", forbidden, out)
		}
	}
}

func TestLivePreToolUseSanitizesMatchingPlanlessApprovalAuthority(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	cwd := t.TempDir()
	input := `{"hook_event_name":"PreToolUse","session_id":"sess_planless_approval_authority","cwd":` +
		quoteJSON(cwd) +
		`,"tool_name":"browser.submit","tool_schema_revision":"builtin:browser.submit","tool_input":{"target":"publish","form":"permission-repro"}}`
	var liveInput claudeLiveHookInput
	if err := json.Unmarshal([]byte(input), &liveInput); err != nil {
		t.Fatal(err)
	}
	actions, err := liveActionIntentsForHome(homePath, "codex", liveInput, time.Now().UTC())
	if err != nil || len(actions) != 1 {
		t.Fatalf("derive live action: actions=%+v err=%v", actions, err)
	}
	// The binding must be derived from the same runtime policy config the live
	// hook evaluates with. DefaultConfig() produces a different binding digest,
	// so the stored grant never matches, ApplyMatching reports matched=false,
	// and the run falls through to plan minting -- which silently stops
	// exercising the planless sanitize path this test exists to prove.
	runtimePolicy, err := loadRuntimePolicyConfig()
	if err != nil {
		t.Fatalf("load runtime policy config: %v", err)
	}
	approval, err := policy.NewExactActionApproval(
		"approval_grant_raw_planless_marker",
		"operator:raw_planless_marker",
		policy.ActionBindingForIntent(actions[0], runtimePolicy),
		time.Now().UTC().Add(5*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(homePath)
	if _, err := approvals.Save(approval); err != nil {
		t.Fatal(err)
	}
	grantsBefore := len(testEventEntries(t, homePath, "approval_grant_"))

	var code int
	out := captureStdout(t, func() {
		withStdin(t, input, func() {
			code = runHookCommand([]string{
				"pre-tool-use", "--live", "--harness", "codex", "--enforce",
			})
		})
	})
	if code == 0 {
		t.Fatalf("enforced PreToolUse exit=0, want policy gate exit\n%s", out)
	}
	var response claudeLiveHookResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatalf("parse enforced response: %v\n%s", err, out)
	}
	if got := response.HookSpecificOutput.PermissionDecision; got == "" {
		t.Fatalf("planless authority failed open: %+v", response.HookSpecificOutput)
	}
	if got := response.HookSpecificOutput.PermissionDecisionReason; !strings.Contains(got, "baseline for mutation_class") {
		t.Fatalf("planless authority reason=%q, want ordinary baseline hold", got)
	}
	if !livePolicyReasonExists(t, homePath, "exact approval authority unavailable for operator review") {
		t.Fatal("persisted PolicyDecision omitted bounded authority hold")
	}
	if livePolicyReasonExists(t, homePath, "raw_planless_marker") {
		t.Fatal("persisted PolicyDecision exposed planless authority detail")
	}
	if plans := readExactApprovalPlans(t, homePath); len(plans) != 0 {
		t.Fatalf("planless authority created approval plans: %+v", plans)
	}
	if grantsAfter := len(testEventEntries(t, homePath, "approval_grant_")); grantsAfter != grantsBefore {
		t.Fatalf("planless authority created approval grants: before=%d after=%d", grantsBefore, grantsAfter)
	}
	if uses := testEventEntries(t, homePath, "approval_use_"); len(uses) != 0 {
		t.Fatalf("planless authority created approval consumption records: %+v", uses)
	}
	if status, err := approvals.Check(approval.ID, time.Now().UTC()); err != nil || status.Status != policy.ExactApprovalStatusReady {
		t.Fatalf("planless authority status=%+v err=%v, want ready and unconsumed", status, err)
	}

	lowerOutput := strings.ToLower(out)
	for _, forbidden := range []string{
		"raw_planless_marker",
		"approval_grant_raw_planless_marker",
		"exact approval authority",
		strings.ToLower(homePath),
		"action-approve",
		"--granted-by",
		"operator:",
		"human:",
		"exact approval plan",
		"approval_plan_",
	} {
		if strings.Contains(lowerOutput, forbidden) {
			t.Fatalf("native response exposed planless approval detail %q: %s", forbidden, out)
		}
	}
}

func TestLivePreToolUseOnlyPublishesNativePermissionDecisionWhenEnforced(t *testing.T) {
	for _, harnessName := range []string{"codex", "claude-code"} {
		t.Run(harnessName, func(t *testing.T) {
			homePath := t.TempDir()
			t.Setenv("COMPOUND_HOME", homePath)
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatalf("init home: %v", err)
			}

			input := `{"hook_event_name":"PreToolUse","session_id":"sess_observe_default_` +
				harnessName +
				`","cwd":` + quoteJSON(t.TempDir()) +
				`,"tool_name":"browser.submit","tool_schema_revision":"builtin:browser.submit","tool_input":{"target":"publish","form":"permission-repro"}}`

			var observeCode int
			observeOut := captureStdout(t, func() {
				withStdin(t, input, func() {
					observeCode = runHookCommand([]string{
						"pre-tool-use", "--live", "--harness", harnessName,
					})
				})
			})
			if observeCode != 0 {
				t.Fatalf("observe-only PreToolUse exit=%d, want 0\n%s", observeCode, observeOut)
			}
			var observed claudeLiveHookResponse
			if err := json.Unmarshal([]byte(observeOut), &observed); err != nil {
				t.Fatalf("parse observe-only response: %v\n%s", err, observeOut)
			}
			if got := observed.HookSpecificOutput.PermissionDecision; got != "" {
				t.Fatalf("observe-only native decision=%q, want empty: %+v", got, observed.HookSpecificOutput)
			}
			if !livePolicyReasonExists(t, homePath, "baseline for mutation_class") {
				t.Fatal("observe-only PreToolUse did not persist the held PolicyDecision evidence")
			}
			if plans := readExactApprovalPlans(t, homePath); len(plans) != 0 {
				t.Fatalf("observe-only PreToolUse created approval debt: %+v", plans)
			}

			enforcedInput := `{"hook_event_name":"PreToolUse","session_id":"sess_observe_enforced_` +
				harnessName +
				`","cwd":` + quoteJSON(t.TempDir()) +
				`,"tool_name":"browser.submit","tool_schema_revision":"builtin:browser.submit","tool_input":{"target":"publish","form":"permission-repro"}}`
			var enforceCode int
			enforceOut := captureStdout(t, func() {
				withStdin(t, enforcedInput, func() {
					enforceCode = runHookCommand([]string{
						"pre-tool-use", "--live", "--harness", harnessName, "--enforce",
					})
				})
			})
			if enforceCode == 0 {
				t.Fatalf("enforced PreToolUse exit=0, want policy gate exit\n%s", enforceOut)
			}
			var enforced claudeLiveHookResponse
			if err := json.Unmarshal([]byte(enforceOut), &enforced); err != nil {
				t.Fatalf("parse enforced response: %v\n%s", err, enforceOut)
			}
			if got := enforced.HookSpecificOutput.PermissionDecision; got == "" {
				t.Fatalf("enforced native decision is empty: %+v", enforced.HookSpecificOutput)
			}
			plans := readExactApprovalPlans(t, homePath)
			if len(plans) != 1 {
				t.Fatalf("enforced PreToolUse plans=%d, want one: %+v", len(plans), plans)
			}
			wantReason := "Exact approval plan " + plans[0].ID + " is ready for operator review."
			// The reason may carry a trailing operator-approval request id, but
			// never a resolve command — the forbidden-token sweep below is the
			// load-bearing check.
			if got := enforced.HookSpecificOutput.PermissionDecisionReason; !strings.HasPrefix(got, wantReason) {
				t.Fatalf("enforced native reason=%q, want prefix %q", got, wantReason)
			}
			for _, forbidden := range []string{"action-approve", "--granted-by", "operator:", "human:"} {
				if strings.Contains(strings.ToLower(enforced.HookSpecificOutput.PermissionDecisionReason), forbidden) {
					t.Fatalf("enforced native reason exposed forbidden command token %q: %q", forbidden, enforced.HookSpecificOutput.PermissionDecisionReason)
				}
			}
		})
	}
}
