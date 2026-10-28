package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/hookplan"
)

func TestContractImportClaudeZaiRegistrationIsDistinctReadyAndObserved(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("HOME", homePath)
	source := filepath.Join("..", "..", "adapters", "claude-zai", "hook-registration.ts")
	if err := contract.StageSyntheticHookHandlers(homePath, source); err != nil {
		t.Fatalf("stage claude-zai handler: %v", err)
	}

	importOut := captureStdout(t, func() {
		cmdContract([]string{"import-hooks", "--hook-registration-ts", source})
	})
	var report contract.InstalledHooksImportReport
	if err := json.Unmarshal([]byte(importOut), &report); err != nil {
		t.Fatalf("parse import report: %v\n%s", err, importOut)
	}
	if report.IncomingHookCount != 5 || report.HookCount != 5 {
		t.Fatalf("claude-zai import counts = incoming %d total %d, want 5/5", report.IncomingHookCount, report.HookCount)
	}
	events := map[string]bool{}
	for _, installed := range report.Manifest.Hooks {
		if installed.Harness != "claude_zai" {
			t.Fatalf("imported harness = %q, want claude_zai", installed.Harness)
		}
		if installed.Status != "current" {
			t.Fatalf("claude-zai %s status = %q, want current", installed.Event, installed.Status)
		}
		events[installed.Event] = true
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		if !events[event] {
			t.Fatalf("claude-zai installed-hook authority missing %s: %+v", event, report.Manifest.Hooks)
		}
	}

	profileDir := hookplan.ClaudeZaiProfileDir(homePath)
	binary := filepath.Join(homePath, ".local", "bin", "compound")
	if _, err := hookplan.InstallHooksWithConfigDir(homePath, binary, "global", []string{"claude-zai"}, profileDir, false); err != nil {
		t.Fatalf("install temporary claude-zai profile: %v", err)
	}
	authorityOut := captureStdout(t, func() {
		cmdContract([]string{"hooks-authority", "--observe-live"})
	})
	var authority contract.HooksAuthorityReport
	if err := json.Unmarshal([]byte(authorityOut), &authority); err != nil {
		t.Fatalf("parse hooks authority: %v\n%s", err, authorityOut)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse"} {
		ready := findHookReadiness(authority.HookReadiness, "claude_zai", event)
		if ready == nil || ready.Status != "ready" || ready.LiveCommand == "" {
			t.Fatalf("claude-zai %s runtime readiness = %+v, want ready live adapter", event, ready)
		}
		delta := cliHookReconciliationDelta(*authority.Reconciliation, "claude_zai", event)
		if delta == nil || delta.State != hookplan.StateReady {
			t.Fatalf("claude-zai %s reconciliation = %+v, want ready", event, delta)
		}
	}
	sessionEnd := findHookReadiness(authority.HookReadiness, "claude_zai", "SessionEnd")
	if sessionEnd == nil || sessionEnd.Status != "ready" || !sessionEnd.FixturePresent ||
		sessionEnd.FixtureRef != "fixtures/hooks/session-end.json" ||
		sessionEnd.LiveCommand != "compound hook session-end-adapter --live --harness claude_zai" {
		t.Fatalf("claude-zai SessionEnd runtime readiness = %+v, want ready bounded adapter with fixture proof", sessionEnd)
	}
	gate := findHookGateReadiness(authority.PreToolUseReadiness, "claude_zai")
	if gate == nil || gate.Status != "native_blocking" || !gate.NativeBlock || !gate.Installed {
		t.Fatalf("claude-zai PreToolUse readiness = %+v, want installed native_blocking", gate)
	}

	cwd := t.TempDir()
	compileOut := captureStdout(t, func() {
		cmdContract([]string{
			"compile",
			"--harness", "claude_zai",
			"--cwd", cwd,
			"--task", "edit source with the isolated Claude lane",
			"--repo-kind", "product",
		})
	})
	var effective envelope.EffectiveContract
	if err := json.Unmarshal([]byte(compileOut), &effective); err != nil {
		t.Fatalf("parse effective contract: %v\n%s", err, compileOut)
	}
	if effective.Harness != "claude_zai" {
		t.Fatalf("effective harness = %q, want claude_zai", effective.Harness)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		ref := "hook://claude_zai/" + event
		if !stringSliceContains(effective.ActiveHooks, ref) {
			t.Fatalf("claude-zai effective hooks = %v, missing %s", effective.ActiveHooks, ref)
		}
	}
}
