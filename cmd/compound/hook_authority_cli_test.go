package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/contract"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/hookplan"
)

func TestInstalledHookAuthorityMutationHoldsSharedAdmissionLock(t *testing.T) {
	compoundHome := t.TempDir()
	observedHeld := false
	if err := withInstalledHookAuthorityLock(compoundHome, func() error {
		held, err := home.AutonomyAdmissionLockHeld(compoundHome)
		if err != nil {
			return err
		}
		observedHeld = held
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !observedHeld {
		t.Fatal("installed-hook authority mutation ran without the shared admission lock")
	}
	heldAfter, err := home.AutonomyAdmissionLockHeld(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	if heldAfter {
		t.Fatal("installed-hook authority mutation left the shared admission lock held")
	}
}

func TestInstalledRuntimeSupportUsesBoundedSessionEndForEveryInstalledClaudeProfile(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"claude_code":    "compound hook session-end-adapter --live --harness claude_code",
		"claude_zai":     "compound hook session-end-adapter --live --harness claude_zai",
		"claude_minimax": "compound hook session-end-adapter --live --harness claude_minimax",
		"claude_qwen":    "compound hook session-end-adapter --live --harness claude_qwen",
		// codex 0.149.x fires a native SessionEnd with transcript_path (rule_delta 2026-08-26);
		// it routes through the generic verb, never the bounded Claude adapter.
		"codex": "compound hook session-end --live --harness codex",
	}
	seen := map[string]bool{}
	for _, support := range installedHookRuntimeSupport() {
		if support.Hook != "SessionEnd" {
			continue
		}
		if support.Harness == "codex" && strings.Contains(support.LiveCommand, "session-end-adapter") {
			t.Fatalf("Codex SessionEnd must use the generic session-end verb, not the bounded Claude adapter: %+v", support)
		}
		command, ok := want[support.Harness]
		if !ok {
			t.Fatalf("unexpected SessionEnd runtime support row: %+v", support)
		}
		if support.LiveCommand != command {
			t.Fatalf("%s SessionEnd command = %q, want %q", support.Harness, support.LiveCommand, command)
		}
		seen[support.Harness] = true
	}
	for harnessName := range want {
		if !seen[harnessName] {
			t.Errorf("missing SessionEnd runtime support for %s", harnessName)
		}
	}
}

func TestHookInstallSynchronizesAuthorityWithoutPromotingInteractiveObserversToBlocking(t *testing.T) {
	for _, order := range [][]string{
		{"codex", "claude-code"},
		{"claude-code", "codex"},
	} {
		order := order
		t.Run(strings.Join(order, "-then-"), func(t *testing.T) {
			compoundHome := t.TempDir()
			harnessHome := t.TempDir()
			binaryPath := filepath.Join(harnessHome, ".local", "bin", "compound")
			if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binaryPath, []byte("synthetic compound binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COMPOUND_HOME", compoundHome)
			t.Setenv("HOME", t.TempDir())

			for _, harnessName := range order {
				captureStdout(t, func() {
					cmdHookInstall([]string{
						"--harness", harnessName,
						"--scope", "global",
						"--base", harnessHome,
						"--binary", binaryPath,
						"--json",
					})
				})
			}

			manifest, err := contract.LoadInstalledHooksManifest(compoundHome)
			if err != nil {
				t.Fatalf("load authority after sequential interactive installs: %v", err)
			}
			assertInstalledHookPosture(t, manifest, "codex", false)
			assertInstalledHookPosture(t, manifest, "claude_code", false)

			reconciled, err := contract.ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
			if err != nil {
				t.Fatal(err)
			}
			assertReconciledPreToolPosture(t, reconciled.Plan, reconciled.Reconciliation, "codex", false)
			assertReconciledPreToolPosture(t, reconciled.Plan, reconciled.Reconciliation, "claude_code", false)
			assertInteractiveMutationContract(t, compoundHome, "codex")
			assertInteractiveMutationContract(t, compoundHome, "claude_code")

			captureStdout(t, func() {
				cmdHookInstall([]string{
					"--harness", "claude-zai",
					"--scope", "global",
					"--base", harnessHome,
					"--binary", binaryPath,
					"--json",
				})
			})
			manifest, err = contract.LoadInstalledHooksManifest(compoundHome)
			if err != nil {
				t.Fatalf("load authority after isolated install: %v", err)
			}
			assertInstalledHookPosture(t, manifest, "codex", false)
			assertInstalledHookPosture(t, manifest, "claude_code", false)
			assertInstalledHookPosture(t, manifest, "claude_zai", true)

			reconciled, err = contract.ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
			if err != nil {
				t.Fatal(err)
			}
			assertReconciledPreToolPosture(t, reconciled.Plan, reconciled.Reconciliation, "claude_zai", true)
		})
	}
}

func assertInteractiveMutationContract(t *testing.T, compoundHome, harnessName string) {
	t.Helper()
	input := contract.Input{
		Harness:     harnessName,
		Cwd:         t.TempDir(),
		Task:        "edit README through the interactive harness",
		RepoKind:    "product",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	effective, err := contract.CompileWithRuntime(input, contract.DefaultPolicy(), compoundHome, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ref := "hook://" + harnessName + "/PreToolUse"
	if !stringSliceContains(effective.ActiveHooks, ref) {
		t.Fatalf("%s mutation contract omitted observation hook: %+v", harnessName, effective.ActiveHooks)
	}
	for _, excluded := range effective.Excluded {
		if excluded.Ref == ref {
			t.Fatalf("%s mutation contract marked its active observation hook excluded: %+v", harnessName, effective.Excluded)
		}
	}
	var readinessStatus string
	for _, row := range effective.HookReadiness {
		if row.Harness == harnessName && row.Hook == "PreToolUse" {
			readinessStatus = row.Status
			break
		}
	}
	if readinessStatus != "non_blocking" {
		t.Fatalf("%s mutation contract readiness = %q, want non_blocking", harnessName, readinessStatus)
	}
	pack, err := contract.PackWithCapabilities(input, contract.DefaultPolicy(), compoundHome, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.RequiredChecks) != 0 {
		t.Fatalf("%s observation-only mutation pack emitted blocking checks: %+v", harnessName, pack.RequiredChecks)
	}
}

func TestHookInstallCheckDoesNotWriteInstalledHookAuthority(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	binaryPath := filepath.Join(harnessHome, ".local", "bin", "compound")
	t.Setenv("COMPOUND_HOME", compoundHome)
	t.Setenv("HOME", t.TempDir())

	check := func() {
		t.Helper()
		captureStdout(t, func() {
			cmdHookInstall([]string{
				"--harness", "codex",
				"--scope", "global",
				"--base", harnessHome,
				"--binary", binaryPath,
				"--check",
				"--json",
			})
		})
	}

	check()
	authorityPath := contract.InstalledHooksPath(compoundHome)
	if _, err := os.Stat(authorityPath); !os.IsNotExist(err) {
		t.Fatalf("check-only install created authority at %s: %v", authorityPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("synthetic compound binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		cmdHookInstall([]string{
			"--harness", "codex",
			"--scope", "global",
			"--base", harnessHome,
			"--binary", binaryPath,
			"--json",
		})
	})
	before, err := os.ReadFile(authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	check()
	after, err := os.ReadFile(authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("check-only install changed installed-hook authority")
	}
}

func TestHookInstallRollsBackNativeConfigsWhenAuthorityPublicationFails(t *testing.T) {
	if os.Getenv("COMPOUND_HOOK_INSTALL_ROLLBACK_HELPER") == "1" {
		cmdHookInstall([]string{
			"--harness", "all",
			"--scope", "global",
			"--base", os.Getenv("COMPOUND_TEST_HARNESS_HOME"),
			"--binary", os.Getenv("COMPOUND_TEST_BINARY"),
			"--json",
		})
		os.Exit(0)
	}

	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	binaryPath := filepath.Join(harnessHome, ".local", "bin", "compound")
	claudePath := filepath.Join(harnessHome, ".claude", "settings.json")
	codexPath := filepath.Join(harnessHome, ".codex", "hooks.json")
	for _, path := range []string{binaryPath, claudePath, codexPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(binaryPath, []byte("synthetic compound binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeBefore := []byte("{\n  \"permissions\": {\"defaultMode\": \"bypassPermissions\"}\n}\n")
	codexBefore := []byte("{\n  \"custom_setting\": true\n}\n")
	if err := os.WriteFile(claudePath, claudeBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, codexBefore, 0o640); err != nil {
		t.Fatal(err)
	}
	provenancePath := contract.InstalledHooksSourceAuthorityPath(compoundHome)
	if err := os.MkdirAll(filepath.Dir(provenancePath), 0o755); err != nil {
		t.Fatal(err)
	}
	provenanceBefore := []byte("{malformed provenance\n")
	if err := os.WriteFile(provenancePath, provenanceBefore, 0o640); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(
		os.Args[0],
		"-test.run=^TestHookInstallRollsBackNativeConfigsWhenAuthorityPublicationFails$",
	)
	command.Env = append(os.Environ(),
		"COMPOUND_HOOK_INSTALL_ROLLBACK_HELPER=1",
		"COMPOUND_HOME="+compoundHome,
		"COMPOUND_TEST_HARNESS_HOME="+harnessHome,
		"COMPOUND_TEST_BINARY="+binaryPath,
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("hook install unexpectedly succeeded against malformed authority:\n%s", output)
	}
	if !strings.Contains(string(output), "publish installed-hook authority") {
		t.Fatalf("hook install failed for the wrong reason: %v\n%s", err, output)
	}

	for _, fixture := range []struct {
		path string
		body []byte
		mode os.FileMode
	}{
		{path: claudePath, body: claudeBefore, mode: 0o600},
		{path: codexPath, body: codexBefore, mode: 0o640},
		{path: provenancePath, body: provenanceBefore, mode: 0o640},
	} {
		after, readErr := os.ReadFile(fixture.path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(after) != string(fixture.body) {
			t.Fatalf("failed install changed %s:\nbefore=%s\nafter=%s", fixture.path, fixture.body, after)
		}
		info, statErr := os.Stat(fixture.path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != fixture.mode {
			t.Fatalf("failed install mode for %s = %o, want %o", fixture.path, got, fixture.mode)
		}
	}
	zaiPath := filepath.Join(hookplan.ClaudeZaiProfileDir(harnessHome), "settings.json")
	if _, statErr := os.Stat(zaiPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed install left previously absent isolated config at %s: %v", zaiPath, statErr)
	}
	authorityPath := contract.InstalledHooksPath(compoundHome)
	if _, statErr := os.Stat(authorityPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed provenance append left newly written authority at %s: %v", authorityPath, statErr)
	}
}

func assertInstalledHookPosture(t *testing.T, manifest contract.InstalledHooksManifest, harnessName string, blocking bool) {
	t.Helper()
	var preTool *contract.InstalledHook
	for i := range manifest.Hooks {
		hook := &manifest.Hooks[i]
		if hook.Harness == harnessName && hook.Event == "PreToolUse" {
			preTool = hook
			break
		}
	}
	if preTool == nil {
		t.Fatalf("%s authority omitted PreToolUse: %+v", harnessName, manifest.Hooks)
	}
	if got := preTool.Status != "non_blocking"; got != blocking {
		t.Fatalf("%s PreToolUse status = %q, blocking=%t", harnessName, preTool.Status, got)
	}
	var readiness *contract.HookGateReadiness
	rows := contract.InstalledHookGateReadiness(manifest, nativePreToolUseBlockMap(harness.DefaultRegistry()))
	for i := range rows {
		if rows[i].Harness == harnessName {
			readiness = &rows[i]
			break
		}
	}
	if readiness == nil {
		t.Fatalf("%s readiness missing", harnessName)
	}
	wantStatus := "non_blocking"
	if blocking {
		wantStatus = "native_blocking"
	}
	if readiness.Status != wantStatus {
		t.Fatalf("%s readiness = %+v, want %s", harnessName, readiness, wantStatus)
	}
}

func assertReconciledPreToolPosture(
	t *testing.T,
	plan hookplan.HookPlan,
	reconciliation hookplan.Reconciliation,
	harnessName string,
	blocking bool,
) {
	t.Helper()
	for _, binding := range plan.Bindings {
		if binding.Harness != harnessName || binding.Event != "PreToolUse" {
			continue
		}
		if binding.Blocking != blocking {
			t.Fatalf("%s plan PreToolUse blocking=%t, want %t: %+v", harnessName, binding.Blocking, blocking, binding)
		}
		delta := cliHookReconciliationDelta(reconciliation, harnessName, "PreToolUse")
		if delta == nil || delta.State != hookplan.StateReady {
			t.Fatalf("%s live PreToolUse reconciliation = %+v, want ready", harnessName, delta)
		}
		return
	}
	t.Fatalf("%s plan omitted PreToolUse: %+v", harnessName, plan.Bindings)
}

func TestHooksAuthorityObserveLiveCLI(t *testing.T) {
	if os.Getenv("COMPOUND_HOOK_AUTHORITY_CLI_HELPER") == "1" {
		cmdContract([]string{"hooks-authority", "--observe-live"})
		os.Exit(0)
	}

	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)
	registrationSource := filepath.Join("..", "..", "fixtures", "harness", "hook-registration.ts")
	importReport, err := contract.ImportHookRegistrationTS(compoundHome, registrationSource, time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ImportHookRegistrationTS() error = %v", err)
	}
	claudeSettings := filepath.Join(harnessHome, ".claude", "settings.json")
	writeHookAuthorityCLIConfig(t, claudeSettings, `{
  "hooks": {
    "SessionStart": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/RecallContext.hook.ts\""}]}],
    "UserPromptSubmit": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/IntentRecall.hook.ts\""}]}],
    "PreToolUse": [{"matcher":"*","hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/PlacementGuard.hook.ts\""}]}],
    "SessionEnd": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/SessionReceipt.hook.ts\""}]}]
  }
}`)
	writeHookAuthorityCLIConfig(t, filepath.Join(harnessHome, ".codex", "hooks.json"), `{
  "hooks": {
    "SessionStart": [{"hooks":[{"type":"command","command":"$HOME/.bun/bin/bun $HOME/.codex/hooks/RecallContext.hook.ts"}]}],
    "UserPromptSubmit": [{"hooks":[{"type":"command","command":"$HOME/.bun/bin/bun $HOME/.codex/hooks/IntentRecall.hook.ts"}]}]
  }
}`)
	settingsBefore, err := os.ReadFile(claudeSettings)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := contract.InstalledHooksPath(compoundHome)
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestHooksAuthorityObserveLiveCLI$")
	command.Env = append(os.Environ(),
		"COMPOUND_HOOK_AUTHORITY_CLI_HELPER=1",
		"COMPOUND_HOME="+compoundHome,
		"HOME="+harnessHome,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("hooks-authority --observe-live failed: %v\n%s", err, output)
	}
	var report struct {
		Privacy                 string                        `json:"privacy"`
		HookPlan                hookplan.HookPlan             `json:"hook_plan"`
		InstallationObservation hookplan.InstallationSnapshot `json:"installation_observation"`
		Reconciliation          hookplan.Reconciliation       `json:"reconciliation"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("parse hooks-authority output: %v\n%s", err, output)
	}
	if report.Privacy != "private" {
		t.Fatalf("hooks-authority privacy = %q, want private for path-bearing local report", report.Privacy)
	}
	if len(report.HookPlan.Bindings) != importReport.HookCount || len(report.InstallationObservation.Bindings) != 6 {
		t.Fatalf("hooks-authority desired/observed split = plan:%d observed:%d", len(report.HookPlan.Bindings), len(report.InstallationObservation.Bindings))
	}
	if cliHookPlanHasEvent(report.HookPlan, "claude_code", "SessionEnd") {
		t.Fatalf("hooks-authority promoted observed SessionEnd into desired plan: %+v", report.HookPlan.Bindings)
	}
	delta := cliHookReconciliationDelta(report.Reconciliation, "claude_code", "SessionEnd")
	if delta == nil || delta.State != hookplan.StateExtraUnowned {
		t.Fatalf("Claude SessionEnd delta = %+v, want extra_unowned", delta)
	}
	if strings.Contains(string(output), ".bun/bin/bun ") {
		t.Fatalf("hooks-authority leaked full hook command: %s", output)
	}
	assertCLIFileUnchanged(t, claudeSettings, settingsBefore)
	assertCLIFileUnchanged(t, manifestPath, manifestBefore)
}

func writeHookAuthorityCLIConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cliHookReconciliationDelta(reconciliation hookplan.Reconciliation, harnessName, event string) *hookplan.Delta {
	for i := range reconciliation.Deltas {
		if reconciliation.Deltas[i].Harness == harnessName && reconciliation.Deltas[i].Event == event {
			return &reconciliation.Deltas[i]
		}
	}
	return nil
}

func cliHookPlanHasEvent(plan hookplan.HookPlan, harnessName, event string) bool {
	for _, binding := range plan.Bindings {
		if binding.Harness == harnessName && binding.Event == event {
			return true
		}
	}
	return false
}

func assertCLIFileUnchanged(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("read-only hooks-authority changed %s", path)
	}
}
