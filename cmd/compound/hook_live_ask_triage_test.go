package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/contract"
	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/harvest"
	"compound/internal/home"
	"compound/internal/hookplan"
	"compound/internal/mem"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/runattempt"
	"compound/internal/work"
)

type askStopResponse struct {
	EventID  string `json:"event_id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func TestInstalledHookRuntimeSupportIncludesCodexStop(t *testing.T) {
	for _, support := range installedHookRuntimeSupport() {
		if support.Harness != "codex" || support.Hook != "Stop" {
			continue
		}
		if !support.FixturePresent || support.FixtureRef != "fixtures/hooks/stop.codex.json" || !support.LiveAdapter ||
			support.LiveCommand != "compound hook stop --live --harness codex" {
			t.Fatalf("Codex Stop runtime support incomplete: %+v", support)
		}
		return
	}
	t.Fatal("Codex Stop is live but absent from hook runtime authority")
}

func TestInstalledHookRuntimeSupportIncludesCodexPostToolUse(t *testing.T) {
	for _, support := range installedHookRuntimeSupport() {
		if support.Harness != "codex" || support.Hook != "PostToolUse" {
			continue
		}
		if !support.FixturePresent || support.FixtureRef != "fixtures/hooks/post-tool-use.codex.json" || !support.LiveAdapter ||
			support.LiveCommand != "compound hook post-tool-use --live --harness codex" {
			t.Fatalf("Codex PostToolUse runtime support incomplete: %+v", support)
		}
		return
	}
	t.Fatal("Codex PostToolUse is live but absent from hook runtime authority")
}

func TestInstalledHookRuntimeSupportCoversCanonicalClaudeFamily(t *testing.T) {
	wantFixtures := map[string]string{
		"SessionStart":     "fixtures/hooks/session-start.json",
		"UserPromptSubmit": "fixtures/hooks/user-prompt-submit.json",
		"PreToolUse":       "fixtures/hooks/pre-tool-use.claude.json",
		"PostToolUse":      "fixtures/hooks/post-tool-use.json",
		"SessionEnd":       "fixtures/hooks/session-end.json",
	}
	wantHarnesses := map[string]bool{"claude_code": true, "claude_zai": true, "claude_minimax": true, "claude_qwen": true}
	seen := map[string]int{}
	for _, support := range installedHookRuntimeSupport() {
		fixture, standardHook := wantFixtures[support.Hook]
		if !wantHarnesses[support.Harness] || !standardHook {
			continue
		}
		seen[support.Harness]++
		wantCommand := "compound hook " + hookCommandName(support.Hook) + " --live --harness " + support.Harness
		if support.Hook == "SessionEnd" {
			wantCommand = "compound hook session-end-adapter --live --harness " + support.Harness
		}
		if !support.FixturePresent || support.FixtureRef != fixture || !support.LiveAdapter || support.LiveCommand != wantCommand {
			t.Errorf("%s %s runtime support = %+v, want fixture %s and command %q", support.Harness, support.Hook, support, fixture, wantCommand)
		}
	}
	for harnessName := range wantHarnesses {
		if seen[harnessName] != 5 {
			t.Errorf("%s runtime support rows = %d, want 5", harnessName, seen[harnessName])
		}
		if !strings.Contains(hookUsage, harnessName) {
			t.Errorf("live hook usage omits canonical harness %s: %s", harnessName, hookUsage)
		}
	}
	manifest := contract.InstalledHooksManifest{}
	for harnessName := range wantHarnesses {
		for hookName := range wantFixtures {
			manifest.Hooks = append(manifest.Hooks, contract.InstalledHook{
				Harness: harnessName, Name: hookName, Ref: "hook://" + harnessName + "/" + hookName, Status: "current",
			})
		}
	}
	rows := contract.InstalledHookRuntimeReadinessRows(manifest, installedHookRuntimeSupport())
	for harnessName := range wantHarnesses {
		for hookName := range wantFixtures {
			if got := findHookReadiness(rows, harnessName, hookName); got == nil || got.Status != "ready" || !got.Installed || !got.FixturePresent || !got.LiveAdapter {
				t.Errorf("%s %s authority readiness = %+v, want ready", harnessName, hookName, got)
			}
		}
	}
}

func TestInstalledHookRuntimeSupportResolvesFixtureOutsideRepoCWD(t *testing.T) {
	t.Chdir(t.TempDir())

	var sessionEnd *contract.HookRuntimeSupport
	for _, support := range installedHookRuntimeSupport() {
		if support.Harness == "claude_code" && support.Hook == "SessionEnd" {
			copy := support
			sessionEnd = &copy
			break
		}
	}
	if sessionEnd == nil || !sessionEnd.FixturePresent || sessionEnd.FixtureRef != "fixtures/hooks/session-end.json" {
		t.Fatalf("SessionEnd installed readiness depends on repository cwd: %+v", sessionEnd)
	}

	fixture, err := loadHookFixture(hookCommandArgs{
		HookName:    "session-end",
		FixturePath: sessionEnd.FixtureRef,
	}, time.Unix(1_800_000_000, 0).UTC())
	if err != nil {
		t.Fatalf("resolve installed SessionEnd fixture outside repository cwd: %v", err)
	}
	if fixture.Hook != "session-end" || fixture.Harness != "claude_code" || fixture.SessionID != "session_end_fixture" {
		t.Fatalf("resolved embedded SessionEnd fixture = %+v", fixture)
	}
}

// TestCanonicalClaudeFamilyExecutesEveryInstalledLiveHookEvent runs every binding the
// installer actually wrote for each Claude lane. It is deliberately not "all five events":
// claude_code installs no UserPromptSubmit binding, and asserting an event the lane does
// not install would prove a hook that no session ever fires.
func TestCanonicalClaudeFamilyExecutesEveryInstalledLiveHookEvent(t *testing.T) {
	testBinary := compoundTestBinary(t)
	poisonDir := t.TempDir()
	poisonMarker := filepath.Join(poisonDir, "ambient-engine-ran")
	poison := writeExecutable(t, filepath.Join(poisonDir, "engine"),
		"#!/bin/sh\nprintf invoked > '"+strings.ReplaceAll(poisonMarker, "'", `'\''`)+"'\nexit 1\n")
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", poison)
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "ambient-profile")
	t.Setenv("COMPOUND_PANEL_BASE_URL", "https://panel.invalid")
	t.Cleanup(func() {
		if _, err := os.Stat(poisonMarker); !os.IsNotExist(err) {
			t.Errorf("installed binding fixture invoked the ambient retrieval engine: %v", err)
		}
	})
	for _, harnessCase := range []struct {
		installer string
		runtime   string
	}{
		{"claude-code", "claude_code"},
		{"claude-zai", "claude_zai"},
		{"claude-minimax", "claude_minimax"},
		{"claude-qwen", "claude_qwen"},
	} {
		t.Run(harnessCase.runtime, func(t *testing.T) {
			homePath := t.TempDir()
			workspace := t.TempDir()
			profileDir := filepath.Join(t.TempDir(), "profile")
			t.Setenv("COMPOUND_HOME", homePath)
			if err := home.Init(homePath, "canonical-claude-hook-test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			report, err := hookplan.InstallHooksWithConfigDir(
				t.TempDir(), testBinary, "global", []string{harnessCase.installer}, profileDir, false,
			)
			if err != nil {
				t.Fatalf("install %s hooks: %v", harnessCase.installer, err)
			}
			// claude_code orients once at SessionStart and installs no per-turn
			// UserPromptSubmit binding, so it carries four bindings where the isolated
			// lanes carry five.
			perTurnContext := harnessCase.installer != "claude-code"
			wantBindings := 4
			if perTurnContext {
				wantBindings = 5
			}
			if len(report.Entries) != 1 || len(report.Entries[0].Bindings) != wantBindings {
				t.Fatalf("%s install report = %+v, want one entry with %d bindings", harnessCase.installer, report, wantBindings)
			}
			bindings := map[string]hookplan.HookInstallBinding{}
			for _, binding := range report.Entries[0].Bindings {
				bindings[binding.Event] = binding
			}
			if _, installed := bindings["UserPromptSubmit"]; installed != perTurnContext {
				t.Fatalf("%s UserPromptSubmit installed=%t, want %t", harnessCase.installer, installed, perTurnContext)
			}
			preCommand := bindings["PreToolUse"].Command
			wantEnforce := harnessCase.installer != "claude-code"
			if strings.Contains(preCommand, " --enforce") != wantEnforce {
				t.Fatalf("%s PreToolUse command=%q, enforce=%t want %t", harnessCase.installer, preCommand, strings.Contains(preCommand, " --enforce"), wantEnforce)
			}

			sessionID := "sess_" + harnessCase.runtime
			target := filepath.Join(workspace, "README.md")
			events := []struct {
				name  string
				input string
			}{
				{"SessionStart", `{"hook_event_name":"SessionStart","session_id":` + quoteJSON(sessionID) + `,"cwd":` + quoteJSON(workspace) + `,"source":"startup"}`},
				{"UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":` + quoteJSON(sessionID) + `,"cwd":` + quoteJSON(workspace) + `,"prompt":"compare the selected model lane"}`},
				{"PreToolUse", `{"hook_event_name":"PreToolUse","session_id":` + quoteJSON(sessionID) + `,"cwd":` + quoteJSON(workspace) + `,"tool_name":"Read","tool_input":{"file_path":` + quoteJSON(target) + `}}`},
				{"PostToolUse", `{"hook_event_name":"PostToolUse","session_id":` + quoteJSON(sessionID) + `,"cwd":` + quoteJSON(workspace) + `,"tool_name":"Read","tool_input":{"file_path":` + quoteJSON(target) + `},"tool_response":{"type":"text","content":"ok"}}`},
				{"SessionEnd", `{"hook_event_name":"SessionEnd","session_id":` + quoteJSON(sessionID) + `,"cwd":` + quoteJSON(workspace) + `}`},
			}
			for _, event := range events {
				if event.name == "UserPromptSubmit" && !perTurnContext {
					continue
				}
				binding, ok := bindings[event.name]
				if !ok || !strings.Contains(binding.Command, "--harness "+harnessCase.runtime) {
					t.Fatalf("%s binding missing canonical runtime %s: %+v", event.name, harnessCase.runtime, binding)
				}
				command := exec.Command("/bin/sh", "-c", binding.Command)
				command.Stdin = strings.NewReader(event.input)
				command.Dir = workspace
				// These disposable homes prove installed binding semantics, not the
				// operator's engine or panel. Keep ambient workers out of the fixture.
				command.Env = append(os.Environ(),
					"COMPOUND_MEM_ENGINE_BIN=",
					"COMPOUND_MEMORY_CLI_PROFILE=",
					"COMPOUND_PANEL_BASE_URL=",
				)
				var stdout, stderr bytes.Buffer
				command.Stdout = &stdout
				command.Stderr = &stderr
				if err := command.Run(); err != nil {
					t.Fatalf("%s command failed: %v\ncommand=%s\nstdout=%s\nstderr=%s", event.name, err, binding.Command, stdout.String(), stderr.String())
				}
				if event.name == "SessionEnd" {
					var response map[string]json.RawMessage
					if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
						t.Fatalf("parse SessionEnd response: %v\n%s", err, stdout.String())
					}
					if response == nil || len(response) != 0 {
						t.Fatalf("SessionEnd response must be the generic empty object, got keys=%v\n%s", response, stdout.String())
					}
					continue
				}
				var response claudeLiveHookResponse
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
					t.Fatalf("parse %s response: %v\n%s", event.name, err, stdout.String())
				}
				if response.HookSpecificOutput.HookEventName != event.name {
					t.Fatalf("%s response event=%q", event.name, response.HookSpecificOutput.HookEventName)
				}
				context := response.HookSpecificOutput.AdditionalContext
				if event.name == "SessionStart" && (!strings.Contains(context, "Oriented as a **product** repo") ||
					!strings.Contains(context, "`"+harnessCase.runtime+"`")) {
					t.Fatalf("SessionStart returned no harness-specific orientation:\n%s", context)
				}
				if event.name == "UserPromptSubmit" && (!strings.Contains(context, "# Compound Context Injection") || !strings.Contains(context, "Orientation served this session at `hook_")) {
					t.Fatalf("UserPromptSubmit did not consume SessionStart orientation:\n%s", context)
				}
			}

			seen := map[string]int{}
			for _, entry := range testEventEntries(t, homePath, "hook_") {
				record, err := entry.Read()
				if err != nil {
					t.Fatal(err)
				}
				var event envelope.HookEvent
				if err := json.Unmarshal(record.Bytes, &event); err != nil {
					t.Fatalf("parse hook event: %v\n%s", err, record.Bytes)
				}
				if event.SchemaVersion != envelope.SVHookEvent {
					continue
				}
				if event.Harness != harnessCase.runtime {
					t.Fatalf("persisted %s harness=%q, want %q", event.HookName, event.Harness, harnessCase.runtime)
				}
				if event.Status != "ok" {
					t.Fatalf("persisted %s status=%q, want ok", event.HookName, event.Status)
				}
				seen[event.HookName]++
			}
			for _, event := range events {
				want := 1
				if event.name == "UserPromptSubmit" && !perTurnContext {
					want = 0
				}
				if seen[event.name] != want {
					t.Fatalf("persisted %s count=%d, want %d", event.name, seen[event.name], want)
				}
			}
		})
	}
}

func TestHookLiveCodexStopCoveredAskPersistsCanonicalResume(t *testing.T) {
	homePath := t.TempDir()
	userHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("HOME", userHome)
	if err := home.Init(homePath, "ask-triage-test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	ruling, err := decisions.Record(memStore, homePath, decisions.RecordInput{
		Kind:       decisions.KindRuling,
		ID:         "operator-ruling-reversible-envelope",
		Title:      "Reversible actions inside a scoped grant proceed without operator approval.",
		ReviewedBy: "operator:test",
		Scope:      "reversible work inside an existing envelope",
		Now:        cliTestNow,
	})
	if err != nil {
		t.Fatalf("record ruling: %v", err)
	}
	claimID := ruling.Import.ClaimID
	if claimID == "" {
		t.Fatalf("recorded ruling lacks claim: %+v", ruling)
	}

	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	w, err := workStore.Create(work.CreateInput{
		Owner:            "codex",
		DesiredOutcome:   "finish a scoped local implementation",
		SuccessPredicate: "the reversible implementation is verified",
		NonGoals:         []string{"external or irreversible actions"},
		State:            "ready",
		Privacy:          "private",
		Risk:             "tier1",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := workStore.Claim(w.ID, "codex", cliTestNow.Add(time.Second)); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := workStore.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex", Note: "executor started"}, cliTestNow.Add(2*time.Second)); err != nil {
		t.Fatalf("advance work: %v", err)
	}
	before, err := workStore.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeKinds := askEventKindCounts(t, homePath)

	const threadID = "019f0000-0000-7000-8000-000000000111"
	const turnID = "019f0000-0000-7000-8000-000000000222"
	writeAskCodexJob(t, threadID, turnID, w.ID)
	writeAskCodexJobNamed(t, "decoy.json", "019f0000-0000-7000-8000-000000000999", turnID, w.ID)
	transcript := writeAskCodexTranscript(t,
		"The current work is a scoped reversible edit inside the existing envelope.",
		"May I continue with this reversible edit inside the envelope?",
	)

	first := runAskStop(t, threadID, turnID, transcript, false)
	if first.Decision != "block" || !strings.Contains(first.Reason, claimID) {
		t.Fatalf("covered Stop response = %+v, want cited continuation for %s", first, claimID)
	}
	after, err := workStore.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || len(after.ActionIntentIDs) != len(before.ActionIntentIDs)+1 || after.PolicyDecisionID == before.PolicyDecisionID {
		t.Fatalf("parent work did not gain exactly one interim chain: before=%+v after=%+v", before, after)
	}
	actionID := after.ActionIntentIDs[len(after.ActionIntentIDs)-1]
	var action envelope.ActionIntent
	readJSONFile(t, filepath.Join(homePath, "events", actionID+".json"), &action)
	if action.IDs.WorkID != w.ID || action.IDs.SessionID != threadID || action.Action != "resume_lane" ||
		action.Target.Kind != "harness_turn" || !strings.Contains(action.Target.ID, threadID) || !strings.Contains(action.Target.ID, turnID) {
		t.Fatalf("resume action lost exact lane binding: %+v", action)
	}
	var decision envelope.PolicyDecision
	readJSONFile(t, filepath.Join(homePath, "events", after.PolicyDecisionID+".json"), &decision)
	if decision.IDs.WorkID != w.ID || decision.ActionIntentID != action.ID || !policy.Allowed(decision) {
		t.Fatalf("resume policy is not an allowed parent chain: %+v", decision)
	}
	var event envelope.HookEvent
	readJSONFile(t, filepath.Join(homePath, "events", first.EventID+".json"), &event)
	receiptID := askSideEffectID(event.SideEffects, "rec_")
	transitionID := askSideEffectID(event.SideEffects, "tr_")
	if event.IDs.WorkID != w.ID || event.IDs.SessionID != threadID ||
		!askSliceContainsID(event.SideEffects, action.ID) || !askSliceContainsID(event.SideEffects, decision.ID) ||
		receiptID == "" || transitionID == "" {
		t.Fatalf("Stop HookEvent lacks delivery chain: %+v", event)
	}
	var receipt envelope.Receipt
	readJSONFile(t, filepath.Join(homePath, "events", receiptID+".json"), &receipt)
	var transition envelope.Transition
	readJSONFile(t, filepath.Join(homePath, "events", transitionID+".json"), &transition)
	if receipt.IDs.WorkID != w.ID || !askSliceContainsID(receipt.ProofRefs, "claim:"+claimID) ||
		transition.IDs.WorkID != w.ID || !askSliceContainsID(transition.ActionRefs, action.ID) ||
		!askSliceContainsID(transition.AfterRefs, receipt.ID) {
		t.Fatalf("resume receipt/transition broke the cited same-work chain: receipt=%+v transition=%+v", receipt, transition)
	}
	afterKinds := askEventKindCounts(t, homePath)
	if afterKinds["receipt"] != beforeKinds["receipt"]+1 || afterKinds["transition"] != beforeKinds["transition"]+1 {
		t.Fatalf("interim resume did not create one receipted transition: before=%v after=%v", beforeKinds, afterKinds)
	}

	replay := runAskStop(t, threadID, turnID, transcript, true)
	if replay.Decision != "" || replay.Reason != "" {
		t.Fatalf("active Stop replay must not continue again: %+v", replay)
	}
	replayed, err := workStore.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.ActionIntentIDs) != len(after.ActionIntentIDs) || replayed.PolicyDecisionID != after.PolicyDecisionID {
		t.Fatalf("active Stop replay duplicated the parent chain: after=%+v replay=%+v", after, replayed)
	}
	replayKinds := askEventKindCounts(t, homePath)
	if replayKinds["receipt"] != afterKinds["receipt"] || replayKinds["transition"] != afterKinds["transition"] {
		t.Fatalf("active Stop replay created terminal proof: after=%v replay=%v", afterKinds, replayKinds)
	}
}

func TestHookLiveCodexStopNovelOrRiskAskQueuesExistingDecisionBrief(t *testing.T) {
	tests := []struct {
		name        string
		ask         string
		disposition string
	}{
		{name: "novel", ask: "Should the new partner receive a beta invite?", disposition: "novel"},
		{name: "risk floor beats matching law", ask: "May I publish this reversible envelope change externally?", disposition: "risk_floor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath, store, w, _, threadID, turnID := seedAskStopRuntime(t)
			before, err := store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeKinds := askEventKindCounts(t, homePath)
			transcript := writeAskCodexTranscript(t, "The lane is waiting for a decision.", tc.ask)

			response := runAskStop(t, threadID, turnID, transcript, false)
			if response.Decision != "" || response.Reason != "" {
				t.Fatalf("%s ask pretended to auto-resume: %+v", tc.disposition, response)
			}
			after, err := store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != before.State || len(after.ActionIntentIDs) != len(before.ActionIntentIDs) || after.PolicyDecisionID != before.PolicyDecisionID {
				t.Fatalf("%s ask mutated parent work: before=%+v after=%+v", tc.disposition, before, after)
			}
			assertAskTriageBrief(t, homePath, tc.ask, tc.disposition)
			afterKinds := askEventKindCounts(t, homePath)
			if afterKinds["receipt"] != beforeKinds["receipt"] || afterKinds["transition"] != beforeKinds["transition"] {
				t.Fatalf("%s residue created success proof: before=%v after=%v", tc.disposition, beforeKinds, afterKinds)
			}

			replay := runAskStop(t, threadID, turnID, transcript, false)
			if replay.Decision != "" || replay.Reason != "" {
				t.Fatalf("%s replay pretended to auto-resume: %+v", tc.disposition, replay)
			}
			assertAskTriageBrief(t, homePath, tc.ask, tc.disposition)
		})
	}
}

func TestHookLiveCodexStopWithoutExactLaneBindingNeverPretendsResume(t *testing.T) {
	homePath, store, w, claimID, threadID, turnID := seedAskStopRuntime(t)
	before, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeKinds := askEventKindCounts(t, homePath)
	ask := "May I continue with this reversible edit inside the envelope?"
	transcript := writeAskCodexTranscript(t, "The current work is scoped and reversible.", ask)

	response := runAskStop(t, threadID+"-unbound", turnID, transcript, false)
	if response.Decision != "" || response.Reason != "" || strings.Contains(response.Reason, claimID) {
		t.Fatalf("unbound Stop pretended to resume: %+v", response)
	}
	after, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || len(after.ActionIntentIDs) != len(before.ActionIntentIDs) || after.PolicyDecisionID != before.PolicyDecisionID {
		t.Fatalf("unbound Stop mutated parent work: before=%+v after=%+v", before, after)
	}
	assertAskTriageBrief(t, homePath, ask, "delivery_unavailable")
	afterKinds := askEventKindCounts(t, homePath)
	if afterKinds["receipt"] != beforeKinds["receipt"] || afterKinds["transition"] != beforeKinds["transition"] {
		t.Fatalf("unbound Stop created success proof: before=%v after=%v", beforeKinds, afterKinds)
	}
}

func seedAskStopRuntime(t *testing.T) (string, *work.Store, envelope.WorkEnvelope, string, string, string) {
	t.Helper()
	return seedAskStopRuntimeWithID(t, "")
}

func seedAskStopRuntimeWithID(t *testing.T, workID string) (string, *work.Store, envelope.WorkEnvelope, string, string, string) {
	t.Helper()
	now := time.Now().UTC()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("HOME", t.TempDir())
	if err := home.Init(homePath, "ask-triage-test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ruling, err := decisions.Record(memStore, homePath, decisions.RecordInput{
		Kind: decisions.KindRuling, ID: "operator-ruling-reversible-envelope",
		Title:      "Reversible actions inside a scoped grant proceed without operator approval.",
		ReviewedBy: "operator:test", Scope: "reversible work inside an existing envelope", Now: cliTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		WorkID: workID,
		Owner:  "codex", DesiredOutcome: "finish a scoped local implementation",
		SuccessPredicate: "the reversible implementation is verified", State: "ready", Privacy: "private", Risk: "tier1",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex"}, now); err != nil {
		t.Fatal(err)
	}
	const threadID = "019f0000-0000-7000-8000-000000000333"
	const turnID = "019f0000-0000-7000-8000-000000000444"
	writeAskCodexJob(t, threadID, turnID, w.ID)
	return homePath, store, w, ruling.Import.ClaimID, threadID, turnID
}

func TestHookLiveCodexStopOwnedSessionBinding(t *testing.T) {
	binary := compoundTestBinary(t)
	const coveredAsk = "May I continue with this reversible edit inside the envelope?"
	for _, tc := range []struct {
		name, workID, refs, ask, disposition, declaration, spawned string
		legacy, staleSession, staleTurn, superseded                bool
	}{
		{name: "generated typed identity"},
		{name: "underscored typed identity", workID: "work_alpha_1"},
		{name: "typed and legacy agree", workID: "work_alpha_1", refs: "work_alpha_1"},
		{name: "complete legacy custom identity", workID: "work_alpha_1", refs: "work_alpha_1", legacy: true},
		{name: "typed conflicts with legacy", workID: "work_alpha_1", refs: "work_alpha_2", disposition: "delivery_unavailable"},
		{name: "shared prefix ambiguity", workID: "work_alpha_1", refs: "work_alpha_1 work_alpha_2", legacy: true, disposition: "delivery_unavailable"},
		{name: "typed does not hide ambiguous prose", workID: "work_alpha_1", refs: "work_alpha_1 work_alpha_2", disposition: "delivery_unavailable"},
		{name: "ambiguous declaration forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", declaration: "ambiguous", disposition: "delivery_unavailable"},
		{name: "uncorroborated declaration forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", declaration: "unheld", disposition: "delivery_unavailable"},
		{name: "stale declaration forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", declaration: "stale", disposition: "delivery_unavailable"},
		{name: "malformed exact declaration forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", declaration: "malformed", disposition: "delivery_unavailable"},
		{name: "unreadable exact declaration forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", declaration: "unreadable", disposition: "delivery_unavailable"},
		{name: "mismatched exact declaration forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", declaration: "mismatched", disposition: "delivery_unavailable"},
		{name: "unrelated malformed declaration permits exact identity", workID: "work_alpha_1", refs: "work_alpha_1", declaration: "unrelated"},
		{name: "terminal spawned identity forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", spawned: "terminal", legacy: true, disposition: "delivery_unavailable"},
		{name: "unattached spawned identity forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", spawned: "unattached", legacy: true, disposition: "delivery_unavailable"},
		{name: "superseded spawned identity forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", spawned: "superseded", legacy: true, disposition: "delivery_unavailable"},
		{name: "wrong workspace spawned identity forbids legacy fallback", workID: "work_alpha_1", refs: "work_alpha_1", spawned: "wrong-workspace", legacy: true, disposition: "delivery_unavailable"},
		{name: "stale session", staleSession: true, disposition: "delivery_unavailable"},
		{name: "stale turn", staleTurn: true, disposition: "delivery_unavailable"},
		{name: "superseded standing decision", superseded: true},
		{name: "new decision", ask: "Should the new partner receive a beta invite?", disposition: "novel"},
		{name: "risk floor", ask: "May I publish this reversible envelope change externally?", disposition: "risk_floor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, store, w, claimID, threadID, turnID := seedAskStopRuntimeWithID(t, tc.workID)
			// A real prefix decoy makes accidental truncation observable, not just
			// a lookup miss. None of these other work items is owned by this session.
			for _, id := range []string{"work_alpha", "work_alpha_2"} {
				if _, err := store.Create(work.CreateInput{WorkID: id, Owner: "other", SuccessPredicate: "must stay unchanged"}, time.Now()); err != nil {
					t.Fatal(err)
				}
				actor := "other"
				if tc.declaration == "ambiguous" && id == "work_alpha_2" {
					actor = "codex"
				}
				if _, err := store.Claim(id, actor, time.Now()); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Advance(id, work.AdvanceInput{To: "running", Actor: actor}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			writeAskCodexJob(t, threadID, turnID, tc.refs)
			if tc.spawned != "" {
				attempts := runattempt.Open(homePath)
				attempt, err := attempts.Mint(w.ID, "codex", "codex", time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if attempt, err = attempts.SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
					t.Fatal(err)
				}
				workspace := "/workspace/example"
				if tc.spawned == "wrong-workspace" {
					workspace = "/workspace/elsewhere"
				}
				if attempt, err = attempts.SetWorkspace(attempt.ID, workspace); err != nil {
					t.Fatal(err)
				}
				fixture := nativeCoordinationCase{homePath: homePath, workID: w.ID, harness: "codex", receiver: "codex"}
				bindingID := fixture.recordNativeSessionBinding(t, attempt, threadID, time.Now())
				var attached []string
				if tc.spawned != "unattached" {
					attached = []string{attempt.ID}
				}
				if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex", RunAttemptIDs: attached, EvidenceRefs: []string{bindingID}}, time.Now()); err != nil {
					t.Fatal(err)
				}
				if _, err := attempts.MarkRunning(attempt.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
				if tc.spawned == "terminal" || tc.spawned == "superseded" {
					if _, err := attempts.Complete(attempt.ID, runattempt.StatusFailed, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				if tc.spawned == "superseded" {
					successor, err := attempts.Mint(w.ID, "codex", "codex", time.Now())
					if err != nil {
						t.Fatal(err)
					}
					if successor, err = attempts.SetOrigin(successor.ID, runattempt.OriginHarnessExecution); err != nil {
						t.Fatal(err)
					}
					if successor, err = attempts.SetWorkspace(successor.ID, "/workspace/example"); err != nil {
						t.Fatal(err)
					}
					bindingID := fixture.recordNativeSessionBinding(t, successor, threadID+"-successor", time.Now())
					if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex", RunAttemptIDs: []string{successor.ID}, EvidenceRefs: []string{bindingID}}, time.Now()); err != nil {
						t.Fatal(err)
					}
					if _, err := attempts.MarkRunning(successor.ID, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !tc.legacy {
				actor := "codex"
				if tc.declaration == "unheld" || tc.declaration == "stale" {
					actor = tc.declaration
				}
				if tc.declaration == "stale" {
					at := time.Now().Add(-2 * work.DefaultPresenceTTL)
					stale, err := store.Create(work.CreateInput{Owner: actor, SuccessPredicate: "stale lease cannot bind Stop"}, at)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := store.Claim(stale.ID, actor, at); err != nil {
						t.Fatal(err)
					}
				}
				// Exercise the maintained producer, not a fabricated companion
				// workId field or raw hook work_id. The resolver corroborates this
				// announcement with the actor's sole independently held fresh lease.
				cmd := exec.Command(binary, "presence", "announce", "--actor", actor, "--session", threadID, "--harness", "codex", "--cwd", "/workspace/example", "--json")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("public presence producer: %v\n%s", err, output)
				}
				if tc.declaration == "malformed" || tc.declaration == "unreadable" || tc.declaration == "mismatched" || tc.declaration == "unrelated" {
					records, err := store.ListPresenceSessions()
					if err != nil || len(records) != 1 {
						t.Fatalf("presence fixture: records=%+v err=%v", records, err)
					}
					path := filepath.Join(homePath, "observed", "presence-sessions", records[0].Key+".json")
					switch tc.declaration {
					case "malformed":
						err = os.WriteFile(path, []byte("{"), 0o600)
					case "unreadable":
						if err = os.Remove(path); err == nil {
							err = os.Mkdir(path, 0o700)
						}
					case "mismatched":
						records[0].SessionID = "a-different-session"
						blob, marshalErr := json.Marshal(records[0])
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						err = os.WriteFile(path, blob, 0o600)
					case "unrelated":
						err = os.WriteFile(filepath.Join(filepath.Dir(path), "unrelated.json"), []byte("{"), 0o600)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			oldClaimID := claimID
			if tc.superseded {
				memStore, err := mem.Open(homePath)
				if err != nil {
					t.Fatal(err)
				}
				ruling, err := decisions.Record(memStore, homePath, decisions.RecordInput{
					Kind: decisions.KindRuling, ID: "operator-ruling-reversible-successor",
					Title:      "Reversible actions inside the existing envelope may proceed with rollback evidence preserved.",
					ReviewedBy: "operator:test", Scope: "reversible work inside an existing envelope",
					UndoOf: "operator-ruling-reversible-envelope", Now: time.Now(),
				})
				if err != nil {
					t.Fatal(err)
				}
				claimID = ruling.Import.ClaimID
			}
			before, err := store.List()
			if err != nil {
				t.Fatal(err)
			}
			beforeKinds := askEventKindCounts(t, homePath)
			if tc.staleSession {
				threadID += "-stale"
			}
			if tc.staleTurn {
				turnID += "-stale"
			}
			ask := tc.ask
			if ask == "" {
				ask = coveredAsk
			}
			invoke := func(active bool) askStopResponse {
				t.Helper()
				payload, err := json.Marshal(map[string]any{
					"hook_event_name": "Stop", "session_id": threadID, "turn_id": turnID,
					"last_assistant_message": ask, "stop_hook_active": active,
					"cwd": "/workspace/example", "work_id": "work_alpha_2",
				})
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(binary, "hook", "stop", "--live", "--harness", "codex")
				cmd.Stdin = bytes.NewReader(payload)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				output, err := cmd.Output()
				if err != nil {
					t.Fatalf("public Stop: %v\n%s\n%s", err, output, stderr.String())
				}
				var response askStopResponse
				if err := json.Unmarshal(output, &response); err != nil {
					t.Fatalf("Stop JSON: %v\n%s", err, output)
				}
				return response
			}
			response := invoke(false)
			after, err := store.List()
			if err != nil {
				t.Fatal(err)
			}
			if tc.disposition != "" {
				if response.Decision != "" || response.Reason != "" || !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal mutated work or delivered continuation: response=%+v before=%+v after=%+v", response, before, after)
				}
				afterKinds := askEventKindCounts(t, homePath)
				if afterKinds["receipt"] != beforeKinds["receipt"] || afterKinds["transition"] != beforeKinds["transition"] {
					t.Fatalf("refused binding emitted continuation proof: before=%v after=%v", beforeKinds, afterKinds)
				}
				assertAskTriageBrief(t, homePath, ask, tc.disposition)
				return
			}
			if response.Decision != "block" || !strings.Contains(response.Reason, claimID) || tc.superseded && strings.Contains(response.Reason, oldClaimID) {
				t.Fatalf("owned session did not consume exact current decision %s: %+v", claimID, response)
			}
			for i, item := range after {
				if item.ID != w.ID && !reflect.DeepEqual(item, before[i]) {
					t.Fatalf("continuation changed unrelated work %s", item.ID)
				}
			}
			bound, err := store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			var action envelope.ActionIntent
			readJSONFile(t, filepath.Join(homePath, "events", bound.ActionIntentIDs[len(bound.ActionIntentIDs)-1]+".json"), &action)
			wantBinding := "work-binding:collaboration_recipient"
			if tc.legacy {
				wantBinding = "work-binding:legacy_codex_job_reference"
			}
			if !askSliceContainsID(action.EvidenceRefs, wantBinding) || action.IDs.WorkID != w.ID {
				t.Fatalf("missing exact identity/provenance: %+v", action)
			}
			afterKinds := askEventKindCounts(t, homePath)
			if afterKinds["receipt"] != beforeKinds["receipt"]+1 || afterKinds["transition"] != beforeKinds["transition"]+1 {
				t.Fatalf("expected one interim chain: before=%v after=%v", beforeKinds, afterKinds)
			}
			invoke(false)
			if replay, err := store.List(); err != nil || !reflect.DeepEqual(after, replay) {
				t.Fatalf("same-turn replay changed work: err=%v", err)
			}
			replayKinds := askEventKindCounts(t, homePath)
			if replayKinds["receipt"] != afterKinds["receipt"] || replayKinds["transition"] != afterKinds["transition"] {
				t.Fatalf("same-turn replay duplicated proof: before=%v after=%v", afterKinds, replayKinds)
			}
			if active := invoke(true); active.Decision != "" || active.Reason != "" {
				t.Fatalf("active Stop recursed: %+v", active)
			}
		})
	}
}

func assertAskTriageBrief(t *testing.T, homePath, headline, disposition string) {
	t.Helper()
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: harvest.AskTriageReviewArea}, cliTestNow)
	if err != nil {
		t.Fatalf("ask-triage queue: %v", err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("ask-triage items=%d, want exactly one: %+v", len(report.Items), report.Items)
	}
	item := report.Items[0]
	if item.Status != "review_required" || item.Band != reviewartifact.BandNeedsYou || item.Brief == nil {
		t.Fatalf("ask-triage item is not a needs-you brief: %+v", item)
	}
	brief := item.Brief
	if brief.SchemaVersion != reviewartifact.DecisionBriefSchemaVersion || brief.Headline != headline || !brief.ProposeOnly ||
		len(brief.Findings) < 2 || len(brief.Options) < 2 || brief.Options[0].ConsequenceApprove == "" ||
		brief.Recommendation.Option == "" || brief.PreparedBy == "" {
		t.Fatalf("ask-triage brief incomplete: %+v", brief)
	}
	var core harvest.AskTriageReview
	readJSONFile(t, item.JSONPath, &core)
	if core.Disposition != disposition || core.WorkID == "" && disposition != "delivery_unavailable" {
		t.Fatalf("ask-triage core disposition/binding = %+v", core)
	}
}

func runAskStop(t *testing.T, threadID, turnID, transcript string, active bool) askStopResponse {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"hook_event_name":  "Stop",
		"session_id":       threadID,
		"turn_id":          turnID,
		"transcript_path":  transcript,
		"stop_hook_active": active,
		"cwd":              "/workspace/example",
		"model":            "codex-test",
		"permission_mode":  "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStdout(t, func() {
		withStdin(t, string(payload), func() {
			code = runHookCommand([]string{"stop", "--live", "--harness", "codex"})
		})
	})
	if code != 0 {
		t.Fatalf("Stop exit=%d output=%s", code, out)
	}
	var response askStopResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatalf("parse Stop response: %v\n%s", err, out)
	}
	return response
}

func writeAskCodexTranscript(t *testing.T, userText, assistantText string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := []map[string]any{
		{"timestamp": "2026-07-11T09:14:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": userText}}}},
		{"timestamp": "2026-07-11T09:15:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": assistantText}}}},
	}
	var body strings.Builder
	for _, line := range lines {
		data, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(data)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeAskCodexJob(t *testing.T, threadID, turnID, workID string) {
	t.Helper()
	writeAskCodexJobNamed(t, "matched.json", threadID, turnID, workID)
}

func writeAskCodexJobNamed(t *testing.T, name, threadID, turnID, workID string) {
	t.Helper()
	root, ok := work.DefaultCodexJobsStateDir()
	if !ok {
		t.Fatal("Codex jobs root unavailable")
	}
	dir := filepath.Join(root, "compound-test", "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"id":       strings.TrimSuffix(name, ".json"),
		"status":   "running",
		"pid":      os.Getpid(),
		"threadId": threadID,
		"turnId":   turnID,
		"request":  map[string]any{"prompt": "Execute Compound envelope " + workID, "write": true},
	}
	writeCLIJSONFixture(t, filepath.Join(dir, name), payload)
}

func askEventKindCounts(t *testing.T, homePath string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	entries := testEventEntries(t, homePath)
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			t.Fatal(err)
		}
		kind, err := envelope.ValidateBytes(record.Bytes)
		if err == nil {
			counts[kind]++
		}
	}
	return counts
}

func askSliceContainsID(values []string, id string) bool {
	for _, value := range values {
		if strings.Contains(value, id) {
			return true
		}
	}
	return false
}

func askSideEffectID(values []string, prefix string) string {
	for _, value := range values {
		base := strings.TrimSuffix(filepath.Base(value), filepath.Ext(value))
		if strings.HasPrefix(base, prefix) {
			return base
		}
	}
	return ""
}
