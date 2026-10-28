package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

func TestHarnessGenerateApplyRequiresWorkAndRecordsGovernedAction(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	writeHarnessApplyJSON(t, targetPath, map[string]any{
		"theme": "dark",
		"autoMode": map[string]any{
			"allow": []string{"$defaults", "HandOwned"},
		},
		"hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Write"}}},
	})
	writeHarnessApplyJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow: []string{"Read"}, Deny: []string{}, Ask: []string{}, DefaultMode: "auto",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Routine local shell is auto-approved."}},
	})
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner: "harness-settings", Actor: "operator:test", SuccessPredicate: "permissions applied", State: "ready",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "operator:test", cliTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "operator:test"}, cliTestNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	installHarnessPermissionGrant(t, homePath, targetPath)

	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
	if code != 0 {
		t.Fatalf("apply code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var report harnessPermissionApplyReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if !report.Changed || report.WorkID != w.ID || report.ActionIntentID == "" || report.PolicyDecisionID == "" ||
		report.ReceiptID == "" || report.TransitionID == "" || report.PreImageRef == "" || report.PostImageRef == "" {
		t.Fatalf("apply report = %+v", report)
	}
	body, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["theme"] != "dark" || settings["hooks"] == nil {
		t.Fatalf("hand-owned settings lost: %+v", settings)
	}
	auto := settings["autoMode"].(map[string]any)["allow"].([]any)
	if strings.Join([]string{auto[0].(string), auto[1].(string), auto[2].(string)}, "|") != "$defaults|HandOwned|Routine local shell is auto-approved." {
		t.Fatalf("autoMode.allow = %+v", auto)
	}
	drift, err := harness.PermissionDrift(homePath, "claude_code")
	if err != nil || drift.Drifted {
		t.Fatalf("drift after apply = %+v err=%v", drift, err)
	}
	recorded, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(recorded.ActionIntentIDs, report.ActionIntentID) || recorded.PolicyDecisionID != report.PolicyDecisionID {
		t.Fatalf("work action binding = runs=%v policy=%s", recorded.ActionIntentIDs, recorded.PolicyDecisionID)
	}
	var action envelope.ActionIntent
	readHarnessApplyJSON(t, filepath.Join(homePath, "events", report.ActionIntentID+".json"), &action)
	if err := action.Validate(); err != nil {
		t.Fatalf("persisted action invalid: %v", err)
	}
	if action.IDs.WorkID != w.ID || action.Capability != "harness.permissions.apply" ||
		action.Action != "reconcile_harness_permissions" || action.Target.Kind != "harness_settings" ||
		action.Target.ID != "claude_code" || action.Target.Path != targetPath || action.MutationClass != "security" ||
		action.Reversibility != "easy" || action.BlastRadius != "local" {
		t.Fatalf("persisted action identity = %+v", action)
	}
	var decision envelope.PolicyDecision
	readHarnessApplyJSON(t, filepath.Join(homePath, "events", report.PolicyDecisionID+".json"), &decision)
	if err := decision.Validate(); err != nil {
		t.Fatalf("persisted decision invalid: %v", err)
	}
	if decision.ActionIntentID != action.ID || !policy.Allowed(decision) || decision.StandingGrantID != "grant_harness_permission_apply" {
		t.Fatalf("persisted policy binding = %+v", decision)
	}
	var receipt envelope.Receipt
	readHarnessApplyJSON(t, filepath.Join(homePath, "events", report.ReceiptID+".json"), &receipt)
	if err := receipt.Validate(); err != nil || receipt.Outcome != "completed" || !receipt.StateDelta.Changed ||
		!containsString(receipt.ProofRefs, report.PreImageRef) || !containsString(receipt.ProofRefs, report.PostImageRef) {
		t.Fatalf("persisted receipt = %+v err=%v", receipt, err)
	}
	var transition envelope.Transition
	readHarnessApplyJSON(t, filepath.Join(homePath, "events", report.TransitionID+".json"), &transition)
	if err := transition.Validate(); err != nil || transition.IDs.WorkID != w.ID ||
		!containsString(transition.BeforeRefs, report.PreImageRef) || !containsString(transition.AfterRefs, report.PostImageRef) ||
		!containsString(transition.ActionRefs, action.ID) {
		t.Fatalf("persisted transition = %+v err=%v", transition, err)
	}
	_, events, err := store.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	for _, ref := range []string{report.ActionIntentID, report.PolicyDecisionID, report.ReceiptID, report.TransitionID} {
		if !containsString(last.Attached, ref) {
			t.Fatalf("work event missing %s: %+v", ref, last)
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, cliTestNow.Add(4*time.Minute))
	if code != 0 {
		t.Fatalf("second apply code=%d stderr=%s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Changed {
		t.Fatal("second apply changed = true, want idempotent false")
	}
}

func TestHarnessGenerateApplyWithoutWorkIsNonMutating(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	writeHarnessApplyJSON(t, targetPath, map[string]any{"theme": "dark"})
	writeHarnessApplyJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code", AuthorityRef: harness.PermissionAuthorityRef("claude_code"), TargetPath: targetPath,
		Permissions: harness.PermissionRules{Allow: []string{"Read"}, Deny: []string{}, Ask: []string{}, DefaultMode: "auto"},
		AutoMode:    harness.AutoModePolicy{Allow: []string{"$defaults", "Routine local shell is auto-approved."}},
	})
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runHarnessGenerateApply([]string{"--harness", "claude_code", "--apply"}, &stdout, &stderr, cliTestNow); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("apply without work mutated settings")
	}
}

func TestHarnessGenerateApplyHeldPolicyIsNonMutating(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	writeHarnessApplyJSON(t, targetPath, map[string]any{"theme": "dark"})
	writeHarnessApplyJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code", AuthorityRef: harness.PermissionAuthorityRef("claude_code"), TargetPath: targetPath,
		Permissions: harness.PermissionRules{Allow: []string{"Read"}, Deny: []string{}, Ask: []string{}, DefaultMode: "auto"},
		AutoMode:    harness.AutoModePolicy{Allow: []string{"$defaults", "Routine local shell is auto-approved."}},
	})
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "harness-settings", Actor: "operator:test", SuccessPredicate: "held apply", State: "ready"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "operator:test", cliTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "operator:test"}, cliTestNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cfg := policy.DefaultConfig()
	cfg.ClassOutcome["security"] = policy.ClassRule{Outcome: "hold"}
	writeHarnessApplyJSON(t, filepath.Join(homePath, "config", "policy.yaml"), cfg)
	workBefore, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
	if code != 1 || !strings.Contains(stderr.String(), "policy hold") {
		t.Fatalf("held apply code=%d stderr=%s", code, stderr.String())
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("held policy mutated settings")
	}
	if _, err := os.Stat(harness.HarnessArtifactManifestPath(homePath)); !os.IsNotExist(err) {
		t.Fatalf("held policy wrote artifact manifest: %v", err)
	}
	recorded, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorded.ActionIntentIDs, workBefore.ActionIntentIDs) || recorded.PolicyDecisionID != workBefore.PolicyDecisionID {
		t.Fatalf("held policy recorded performed action: %+v", recorded)
	}
}

func TestHarnessGenerateApplyRequiresExplicitActor(t *testing.T) {
	homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
	installHarnessPermissionGrant(t, homePath, targetPath)
	before, _ := os.ReadFile(targetPath)
	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{"--harness", "claude_code", "--apply", "--work", w.ID, "--json"}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
	if code != 2 || !strings.Contains(stderr.String(), "--actor") {
		t.Fatalf("missing actor code=%d stderr=%s", code, stderr.String())
	}
	after, _ := os.ReadFile(targetPath)
	if !bytes.Equal(before, after) {
		t.Fatal("missing actor mutated settings")
	}
}

func TestHarnessGenerateApplyRequiresRunningWorkHeldByActor(t *testing.T) {
	for _, tc := range []struct {
		name, leaseHolder, state, actor, want string
	}{
		{"ready", "", "ready", "operator:test", "running"},
		{"wrong actor", "operator:holder", "running", "operator:test", "lease holder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, targetPath, w := setupHarnessPermissionApplyWork(t, tc.leaseHolder, tc.state)
			installHarnessPermissionGrant(t, homePath, targetPath)
			before, _ := os.ReadFile(targetPath)
			store, _ := work.Open(homePath)
			workBefore, _ := store.Get(w.ID)
			var stdout, stderr bytes.Buffer
			code := runHarnessGenerateApply([]string{"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", tc.actor, "--json"}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
			if code != 1 || !strings.Contains(strings.ToLower(stderr.String()), tc.want) {
				t.Fatalf("code=%d stderr=%s want=%q", code, stderr.String(), tc.want)
			}
			after, _ := os.ReadFile(targetPath)
			workAfter, _ := store.Get(w.ID)
			if !bytes.Equal(before, after) || !reflect.DeepEqual(workBefore, workAfter) {
				t.Fatal("invalid work admission mutated settings or work")
			}
			if _, err := os.Stat(harness.HarnessArtifactManifestPath(homePath)); !os.IsNotExist(err) {
				t.Fatalf("invalid work admission wrote manifest: %v", err)
			}
		})
	}
}

func TestHarnessGenerateApplyFailureRecordsFailedReceiptWithoutFalseSuccess(t *testing.T) {
	homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
	installHarnessPermissionGrant(t, homePath, targetPath)
	before, _ := os.ReadFile(targetPath)
	previous := applyHarnessPermissions
	applyHarnessPermissions = func(string, harness.PermissionRender, envelope.ActionIntent, envelope.PolicyDecision, harness.PermissionApplyAuthorization, harness.PermissionApplyReserve, harness.PermissionApplyAbort, harness.PermissionApplyFinalize) (harness.PermissionRender, error) {
		return harness.PermissionRender{}, errors.New("injected permission apply failure")
	}
	t.Cleanup(func() { applyHarnessPermissions = previous })
	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json"}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
	if code != 1 || !strings.Contains(stderr.String(), "injected permission apply failure") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	after, _ := os.ReadFile(targetPath)
	if !bytes.Equal(before, after) {
		t.Fatal("injected failure mutated target")
	}
	store, _ := work.Open(homePath)
	recorded, _ := store.Get(w.ID)
	if len(recorded.ActionIntentIDs) == 0 || recorded.PolicyDecisionID == "" {
		t.Fatalf("failed action work links = %+v", recorded)
	}
	var failedReceipt envelope.Receipt
	var failedTransition envelope.Transition
	entries := testEventEntries(t, homePath)
	for _, entry := range entries {
		switch {
		case strings.HasPrefix(entry.ID, "rec_"):
			readTestEventJSON(t, homePath, entry.ID, &failedReceipt)
		case strings.HasPrefix(entry.ID, "tr_"):
			readTestEventJSON(t, homePath, entry.ID, &failedTransition)
		}
	}
	if failedReceipt.Outcome != "failed" || failedReceipt.StateDelta.Changed || failedReceipt.FailureCause == nil ||
		!containsString(failedReceipt.ProofRefs, "sha256:"+permissionApplyDigest(before)) {
		t.Fatalf("failed receipt = %+v", failedReceipt)
	}
	if failedTransition.ID == "" || !containsString(failedTransition.BeforeRefs, "sha256:"+permissionApplyDigest(before)) ||
		!containsString(failedTransition.AfterRefs, "sha256:"+permissionApplyDigest(before)) {
		t.Fatalf("failed transition = %+v", failedTransition)
	}
}

func TestHarnessGenerateApplyAuthorityChangeAfterApprovalIsReceiptedAsFailure(t *testing.T) {
	homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
	installHarnessPermissionGrant(t, homePath, targetPath)
	before, _ := os.ReadFile(targetPath)
	previous := applyHarnessPermissions
	applyHarnessPermissions = func(home string, approved harness.PermissionRender, action envelope.ActionIntent, decision envelope.PolicyDecision, authorize harness.PermissionApplyAuthorization, reserve harness.PermissionApplyReserve, abort harness.PermissionApplyAbort, finalize harness.PermissionApplyFinalize) (harness.PermissionRender, error) {
		writeHarnessApplyJSON(t, harness.PermissionAuthorityPath(home, "claude_code"), harness.PermissionAuthority{
			SchemaVersion: harness.PermissionAuthoritySchemaVersion, Harness: "claude_code", AuthorityRef: harness.PermissionAuthorityRef("claude_code"), TargetPath: targetPath + ".changed",
			Permissions: harness.PermissionRules{Allow: []string{"Write"}, DefaultMode: "auto"}, AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Write"}},
		})
		return harness.ApplyPermissions(home, approved, action, decision, authorize, reserve, abort, finalize)
	}
	t.Cleanup(func() { applyHarnessPermissions = previous })
	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json"}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
	if code != 1 || !strings.Contains(stderr.String(), "approved render") || !strings.Contains(stderr.String(), "receipted") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	after, _ := os.ReadFile(targetPath)
	if !bytes.Equal(before, after) {
		t.Fatal("authority change after approval mutated approved target")
	}
	if _, err := os.Stat(targetPath + ".changed"); !os.IsNotExist(err) {
		t.Fatalf("authority change created changed target: %v", err)
	}
	store, _ := work.Open(homePath)
	recorded, _ := store.Get(w.ID)
	var receipt envelope.Receipt
	entries := testEventEntries(t, homePath, "rec_")
	for _, entry := range entries {
		if strings.HasPrefix(entry.ID, "rec_") {
			readTestEventJSON(t, homePath, entry.ID, &receipt)
		}
	}
	if recorded.PolicyDecisionID == "" || receipt.Outcome != "failed" || receipt.FailureCause == nil {
		t.Fatalf("authority mismatch missing failed work-chain truth: work=%+v receipt=%+v", recorded, receipt)
	}
}

func TestHarnessGenerateApplyRevalidatesWorkLeaseAndGrantInsideMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wantText string
		mutate   func(t *testing.T, homePath, workID, targetPath string)
	}{
		{
			name:     "work no longer running",
			wantText: "requires running work",
			mutate: func(t *testing.T, homePath, workID, _ string) {
				var current envelope.WorkEnvelope
				path := filepath.Join(homePath, "work", "work", workID+".json")
				readHarnessApplyJSON(t, path, &current)
				current.State = "held"
				writeHarnessApplyJSON(t, path, current)
			},
		},
		{
			name:     "lease holder changed",
			wantText: "lease holder",
			mutate: func(t *testing.T, homePath, workID, _ string) {
				var lease work.Lease
				path := filepath.Join(homePath, "work", "leases", workID+".json")
				readHarnessApplyJSON(t, path, &lease)
				lease.By = "operator:replacement"
				writeHarnessApplyJSON(t, path, lease)
			},
		},
		{
			name:     "standing grant changed",
			wantText: "standing grant",
			mutate: func(t *testing.T, homePath, _ string, targetPath string) {
				cfg := policy.DefaultConfig()
				cfg.StandingGrants = []policy.StandingGrant{{
					ID: "grant_replacement", Capability: "harness.permissions.apply", MutationClass: "security",
					Reversibility: "easy", BlastRadius: "local", TargetPath: targetPath,
				}}
				writeHarnessApplyJSON(t, filepath.Join(homePath, "config", "policy.yaml"), cfg)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
			installHarnessPermissionGrant(t, homePath, targetPath)
			before, err := os.ReadFile(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			previous := applyHarnessPermissions
			applyHarnessPermissions = func(home string, approved harness.PermissionRender, action envelope.ActionIntent, decision envelope.PolicyDecision, authorize harness.PermissionApplyAuthorization, reserve harness.PermissionApplyReserve, abort harness.PermissionApplyAbort, finalize harness.PermissionApplyFinalize) (harness.PermissionRender, error) {
				tc.mutate(t, home, w.ID, targetPath)
				return previous(home, approved, action, decision, authorize, reserve, abort, finalize)
			}
			t.Cleanup(func() { applyHarnessPermissions = previous })

			var stdout, stderr bytes.Buffer
			code := runHarnessGenerateApply([]string{
				"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
			}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
			if code != 1 || !strings.Contains(strings.ToLower(stderr.String()), tc.wantText) {
				t.Fatalf("code=%d stderr=%s want=%q", code, stderr.String(), tc.wantText)
			}
			after, err := os.ReadFile(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("in-lock authorization refusal mutated settings")
			}
			if _, err := os.Stat(harness.HarnessArtifactManifestPath(homePath)); !os.IsNotExist(err) {
				t.Fatalf("in-lock authorization refusal wrote manifest: %v", err)
			}
		})
	}
}

func TestHarnessGenerateApplyRevalidatesDecisionExpiryInsideMutation(t *testing.T) {
	homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
	runNow := cliTestNow.Add(3 * time.Minute)
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = []policy.StandingGrant{{
		ID: "grant_harness_permission_apply", Capability: "harness.permissions.apply", MutationClass: "security",
		Reversibility: "easy", BlastRadius: "local", TargetPath: targetPath,
		ExpiresAt: runNow.Add(time.Hour).Format(time.RFC3339),
	}}
	writeHarnessApplyJSON(t, filepath.Join(homePath, "config", "policy.yaml"), cfg)
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	previous := applyHarnessPermissions
	applyHarnessPermissions = func(_ string, _ harness.PermissionRender, action envelope.ActionIntent, decision envelope.PolicyDecision, authorize harness.PermissionApplyAuthorization, _ harness.PermissionApplyReserve, _ harness.PermissionApplyAbort, _ harness.PermissionApplyFinalize) (harness.PermissionRender, error) {
		return harness.PermissionRender{}, authorize(action, decision, runNow.Add(2*time.Hour))
	}
	t.Cleanup(func() { applyHarnessPermissions = previous })

	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, runNow)
	if code != 1 || !strings.Contains(strings.ToLower(stderr.String()), "expired") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expired decision mutated settings")
	}
	if _, err := os.Stat(harness.HarnessArtifactManifestPath(homePath)); !os.IsNotExist(err) {
		t.Fatalf("expired decision wrote manifest: %v", err)
	}
}

func TestHarnessGenerateApplyRecoversExactPublishedOutcomeBeforeNewAction(t *testing.T) {
	homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
	installHarnessPermissionGrant(t, homePath, targetPath)
	firstNow := cliTestNow.Add(3 * time.Minute)
	render, err := harness.RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	expectedActionID := actIDFor(
		"act_", w.ID, "harness.permissions.apply", "claude_code", render.Artifact.SourceRevision,
		firstNow.UTC().Format(time.RFC3339Nano),
	)
	injected := errors.New("injected receipt store interruption")
	previousRecord := recordReservedHarnessPermissionOutcome
	recordReservedHarnessPermissionOutcome = func(*work.Store, string, work.ReceiptedActionInput, time.Time) (work.ReceiptedActionResult, error) {
		return work.ReceiptedActionResult{}, injected
	}
	t.Cleanup(func() { recordReservedHarnessPermissionOutcome = previousRecord })
	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, firstNow)
	if code != 1 || !strings.Contains(strings.ToLower(stderr.String()), "pending finalization") {
		t.Fatalf("first code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(harness.PermissionApplyPendingPath(homePath, "claude_code")); err != nil {
		t.Fatalf("published mutation lost pending identity: %v", err)
	}
	if testEventExists(t, homePath, expectedActionID) {
		t.Fatal("failed finalizer emitted a competing action envelope")
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying", Actor: "operator:test"}, firstNow.Add(30*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "pending receipted action "+expectedActionID) {
		t.Fatalf("work advancement crossed published permission outcome reservation: %v", err)
	}

	writeHarnessApplyJSON(t, filepath.Join(homePath, "config", "policy.yaml"), policy.DefaultConfig())
	recordReservedHarnessPermissionOutcome = previousRecord
	stdout.Reset()
	stderr.Reset()
	code = runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, firstNow.Add(time.Hour))
	if code != 0 {
		t.Fatalf("recovery code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var report harnessPermissionApplyReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.ActionIntentID != expectedActionID || !report.Changed || !report.TargetChanged || !report.ManifestChanged {
		t.Fatalf("recovered report lost original outcome: %+v", report)
	}
	var receipt envelope.Receipt
	readHarnessApplyJSON(t, filepath.Join(homePath, "events", report.ReceiptID+".json"), &receipt)
	wantFiles := []string{targetPath, harness.HarnessArtifactManifestPath(homePath)}
	if receipt.Outcome != "completed" || !receipt.StateDelta.Changed || !reflect.DeepEqual(receipt.StateDelta.FilesChanged, wantFiles) {
		t.Fatalf("recovered receipt = %+v, want files %v", receipt, wantFiles)
	}
	if _, err := os.Stat(harness.PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("successful recovery left pending identity: %v", err)
	}
}

func TestHarnessGenerateApplyRecoversLegacyPublishedOutcomeAfterGrantRevocation(t *testing.T) {
	homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
	installHarnessPermissionGrant(t, homePath, targetPath)
	firstNow := cliTestNow.Add(3 * time.Minute)
	render, err := harness.RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	expectedActionID := actIDFor(
		"act_", w.ID, "harness.permissions.apply", "claude_code", render.Artifact.SourceRevision,
		firstNow.UTC().Format(time.RFC3339Nano),
	)
	previousRecord := recordReservedHarnessPermissionOutcome
	recordReservedHarnessPermissionOutcome = func(*work.Store, string, work.ReceiptedActionInput, time.Time) (work.ReceiptedActionResult, error) {
		return work.ReceiptedActionResult{}, errors.New("injected legacy recovery interruption")
	}
	t.Cleanup(func() { recordReservedHarnessPermissionOutcome = previousRecord })

	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, firstNow)
	if code != 1 || !strings.Contains(strings.ToLower(stderr.String()), "pending finalization") {
		t.Fatalf("first code=%d stderr=%s", code, stderr.String())
	}
	canonicalPath := harness.PermissionApplyPendingPath(homePath, "claude_code")
	legacyPath := filepath.Join(homePath, "transactions", "harness-permissions", "claude_code.json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(canonicalPath, legacyPath); err != nil {
		t.Fatal(err)
	}

	writeHarnessApplyJSON(t, filepath.Join(homePath, "config", "policy.yaml"), policy.DefaultConfig())
	recordReservedHarnessPermissionOutcome = previousRecord
	stdout.Reset()
	stderr.Reset()
	code = runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, firstNow.Add(time.Hour))
	if code != 0 {
		t.Fatalf("legacy recovery code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var report harnessPermissionApplyReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.ActionIntentID != expectedActionID || report.ReceiptID == "" || report.TransitionID == "" || !report.Changed || !report.TargetChanged || !report.ManifestChanged {
		t.Fatalf("legacy recovery lost exact recorded outcome: %+v", report)
	}
	var receipt envelope.Receipt
	readHarnessApplyJSON(t, filepath.Join(homePath, "events", report.ReceiptID+".json"), &receipt)
	wantFiles := []string{targetPath, harness.HarnessArtifactManifestPath(homePath)}
	if receipt.Outcome != "completed" || !receipt.StateDelta.Changed || !reflect.DeepEqual(receipt.StateDelta.FilesChanged, wantFiles) {
		t.Fatalf("legacy recovered receipt = %+v, want files %v", receipt, wantFiles)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("successful recovery left legacy pending identity: %v", err)
	}
}

func TestHarnessGenerateApplyReceiptsManifestOnlyMutationAsChanged(t *testing.T) {
	homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
	installHarnessPermissionGrant(t, homePath, targetPath)
	render, err := harness.RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var report harnessPermissionApplyReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.TargetChanged || !report.ManifestChanged {
		t.Fatalf("manifest-only report = %+v", report)
	}
	var receipt envelope.Receipt
	readHarnessApplyJSON(t, filepath.Join(homePath, "events", report.ReceiptID+".json"), &receipt)
	if receipt.Outcome != "completed" || !receipt.StateDelta.Changed ||
		!reflect.DeepEqual(receipt.StateDelta.FilesChanged, []string{harness.HarnessArtifactManifestPath(homePath)}) {
		t.Fatalf("manifest-only receipt = %+v", receipt)
	}
}

func TestHarnessGenerateApplyFailedPostReadRecordsObservationFailure(t *testing.T) {
	homePath, targetPath, w := setupHarnessPermissionApplyWork(t, "operator:test", "running")
	installHarnessPermissionGrant(t, homePath, targetPath)
	preimage, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	previousApply := applyHarnessPermissions
	applyHarnessPermissions = func(string, harness.PermissionRender, envelope.ActionIntent, envelope.PolicyDecision, harness.PermissionApplyAuthorization, harness.PermissionApplyReserve, harness.PermissionApplyAbort, harness.PermissionApplyFinalize) (harness.PermissionRender, error) {
		return harness.PermissionRender{}, errors.New("injected permission apply failure")
	}
	t.Cleanup(func() { applyHarnessPermissions = previousApply })
	previousRead := readPermissionTargetForObservation
	readPermissionTargetForObservation = func(path string) ([]byte, error) {
		if path == targetPath {
			return nil, os.ErrPermission
		}
		return os.ReadFile(path)
	}
	t.Cleanup(func() { readPermissionTargetForObservation = previousRead })

	var stdout, stderr bytes.Buffer
	code := runHarnessGenerateApply([]string{
		"--harness", "claude_code", "--apply", "--work", w.ID, "--actor", "operator:test", "--json",
	}, &stdout, &stderr, cliTestNow.Add(3*time.Minute))
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var receipt envelope.Receipt
	var transition envelope.Transition
	entries := testEventEntries(t, homePath)
	for _, entry := range entries {
		switch {
		case strings.HasPrefix(entry.ID, "rec_"):
			readTestEventJSON(t, homePath, entry.ID, &receipt)
		case strings.HasPrefix(entry.ID, "tr_"):
			readTestEventJSON(t, homePath, entry.ID, &transition)
		}
	}
	preimageRef := "sha256:" + permissionApplyDigest(preimage)
	if receipt.Outcome != "failed" || !receipt.StateDelta.Changed ||
		!reflect.DeepEqual(receipt.StateDelta.FilesChanged, []string{targetPath}) || receipt.FailureCause == nil ||
		!strings.Contains(strings.ToLower(receipt.FailureCause.Mechanism), "observation") {
		t.Fatalf("failed observation receipt = %+v", receipt)
	}
	if containsString(transition.AfterRefs, preimageRef) {
		t.Fatalf("failed post-read substituted preimage as postimage: %+v", transition.AfterRefs)
	}
	foundObservationError := false
	for _, ref := range transition.AfterRefs {
		if strings.HasPrefix(ref, "observation_error:permission-target-read:") {
			foundObservationError = true
		}
	}
	if !foundObservationError {
		t.Fatalf("transition missing post-read observation failure: %+v", transition.AfterRefs)
	}
}

func setupHarnessPermissionApplyWork(t *testing.T, leaseHolder, state string) (string, string, envelope.WorkEnvelope) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	writeHarnessApplyJSON(t, targetPath, map[string]any{"theme": "dark"})
	writeHarnessApplyJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion, Harness: "claude_code", AuthorityRef: harness.PermissionAuthorityRef("claude_code"), TargetPath: targetPath,
		Permissions: harness.PermissionRules{Allow: []string{"Read"}, DefaultMode: "auto"}, AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	})
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "harness-settings", Actor: "operator:test", SuccessPredicate: "permissions applied", State: "ready"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if state == "running" {
		if _, err := store.Claim(w.ID, leaseHolder, cliTestNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		w, err = store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: leaseHolder}, cliTestNow.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
	return homePath, targetPath, w
}

func installHarnessPermissionGrant(t *testing.T, homePath, targetPath string) {
	t.Helper()
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = []policy.StandingGrant{{
		ID: "grant_harness_permission_apply", Capability: "harness.permissions.apply", MutationClass: "security",
		Reversibility: "easy", BlastRadius: "local", TargetPath: targetPath,
	}}
	writeHarnessApplyJSON(t, filepath.Join(homePath, "config", "policy.yaml"), cfg)
}

func writeHarnessApplyJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readHarnessApplyJSON(t *testing.T, path string, value any) {
	t.Helper()
	body := readTestPath(t, path)
	if err := json.Unmarshal(body, value); err != nil {
		t.Fatal(err)
	}
}
