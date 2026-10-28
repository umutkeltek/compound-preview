package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"compound/internal/contract"
	"compound/internal/envelope"
)

func TestContractImportOpenCodeRegistrationIsNonBlocking(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("HOME", homePath)
	source := filepath.Join("..", "..", "adapters", "opencode", "hook-registration.ts")
	if err := contract.StageSyntheticHookHandlers(homePath, source); err != nil {
		t.Fatalf("stage opencode handler: %v", err)
	}

	importOut := captureStdout(t, func() {
		cmdContract([]string{"import-hooks", "--hook-registration-ts", source})
	})
	var report contract.InstalledHooksImportReport
	if err := json.Unmarshal([]byte(importOut), &report); err != nil {
		t.Fatalf("parse import report: %v\n%s", err, importOut)
	}
	if report.IncomingHookCount != 3 || report.HookCount != 3 {
		t.Fatalf("opencode import counts = incoming %d total %d, want 3/3", report.IncomingHookCount, report.HookCount)
	}
	events := map[string]bool{}
	for _, installed := range report.Manifest.Hooks {
		if installed.Harness != "opencode" {
			t.Fatalf("imported harness = %q, want opencode", installed.Harness)
		}
		if installed.Status != "non_blocking" {
			t.Fatalf("opencode %s status = %q, want non_blocking", installed.Event, installed.Status)
		}
		if installed.Event == "PreToolUse" {
			t.Fatal("opencode registration must not contain PreToolUse")
		}
		events[installed.Event] = true
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse"} {
		if !events[event] {
			t.Fatalf("opencode installed-hook authority missing %s: %+v", event, report.Manifest.Hooks)
		}
	}

	authorityOut := captureStdout(t, func() {
		cmdContract([]string{"hooks-authority"})
	})
	var authority contract.HooksAuthorityReport
	if err := json.Unmarshal([]byte(authorityOut), &authority); err != nil {
		t.Fatalf("parse hooks authority: %v\n%s", err, authorityOut)
	}
	ready := map[string]bool{}
	for _, row := range authority.HookReadiness {
		if row.Harness == "opencode" && row.Status == "ready" {
			ready[row.Hook] = true
		}
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse"} {
		if !ready[event] {
			t.Fatalf("opencode hook readiness missing ready %s: %+v", event, authority.HookReadiness)
		}
	}
	var preToolReadiness *contract.HookGateReadiness
	for i := range authority.PreToolUseReadiness {
		if authority.PreToolUseReadiness[i].Harness == "opencode" {
			preToolReadiness = &authority.PreToolUseReadiness[i]
			break
		}
	}
	if preToolReadiness == nil || preToolReadiness.Status != "non_blocking" {
		t.Fatalf("opencode pre-tool readiness = %+v, want non_blocking", preToolReadiness)
	}
	if preToolReadiness.AdapterRequired || preToolReadiness.AdapterCommand != "" || len(preToolReadiness.Actions) != 0 {
		t.Fatalf("opencode pre-tool readiness must not recommend an enforcement adapter: %+v", preToolReadiness)
	}

	cwd := t.TempDir()
	compileOut := captureStdout(t, func() {
		cmdContract([]string{
			"compile",
			"--harness", "opencode",
			"--cwd", cwd,
			"--task", "inspect installed context and observation hooks",
			"--repo-kind", "product",
		})
	})
	var effective envelope.EffectiveContract
	if err := json.Unmarshal([]byte(compileOut), &effective); err != nil {
		t.Fatalf("parse effective contract: %v\n%s", err, compileOut)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse"} {
		ref := "hook://opencode/" + event
		if !stringSliceContains(effective.ActiveHooks, ref) {
			t.Fatalf("opencode effective contract hooks = %v, missing %s", effective.ActiveHooks, ref)
		}
	}
}
