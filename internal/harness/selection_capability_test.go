package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// Every DefaultRegistry row must declare what its launch can do with a
// requested model/effort/session. A new registry row without one fails here
// instead of silently inheriting "refuse everything" or, worse, a name list.
func TestEveryRegisteredHarnessHasSelectionCapabilityRow(t *testing.T) {
	reg := DefaultRegistry()
	claudeLanes := 0
	for _, name := range reg.Names() {
		h, _ := reg.Get(name)
		if h.Selection == nil || strings.TrimSpace(h.Selection.Basis) == "" {
			t.Errorf("registry row %q has no selection capability row (or no basis)", name)
			continue
		}
		if got, ok := SelectionCapabilityFor(name); !ok || got != *h.Selection {
			t.Errorf("SelectionCapabilityFor(%q) = %+v ok=%v, want the registry row", name, got, ok)
		}
		if strings.HasPrefix(name, "claude_") {
			claudeLanes++
			if *h.Selection != *claudeLaneSelection() {
				t.Errorf("claude lane %q diverges from the shared claude lane row: %+v", name, h.Selection)
			}
		}
	}
	if claudeLanes != 12 {
		t.Fatalf("claude_* registry rows with a selection row = %d, want 12", claudeLanes)
	}
	// The retired name list carried "claude_mini" (never registered) and the raw
	// non-tuple "claude" adapter must not borrow claude_code's tuple binding.
	for _, name := range []string{"claude_mini", "claude", "os-harness-entrypoints"} {
		if _, ok := SelectionCapabilityFor(name); ok {
			t.Errorf("%q must have no selection row", name)
		}
	}
	// Every registered claude lane adapter resolves to a row.
	for _, name := range []string{"claude_code", "claude_minimax", "claude_qwen", "claude_zai", "claude_zai_full", "claude_neuralwatt", "claude_gpt56_sol_wpcom", "claude_gpt56_sol_wpcom_headroom", "claude_gpt", "claude_kimi", "claude_deepseek_flash_api", "claude_deepseek_flash_nvidia"} {
		if _, ok := NewAdapterRegistry(runattempt.Open(t.TempDir())).AdapterByName(name); !ok {
			t.Errorf("claude lane %q has a row but no adapter", name)
		}
		if err := CheckSelectionApplicable(name, &runattempt.ExecutionSelectionSnapshot{RequestedEffort: "xhigh"}); err == nil || err.Error() != "selection_unsupported:"+name+":effort" {
			t.Errorf("%s effort request: err=%v, want selection_unsupported:%s:effort", name, err, name)
		}
	}
}

// A claude lane asked for an effort its wrapper cannot take is refused by the
// shared runner BEFORE mint: typed error, no RunAttempt, the command never runs.
func TestClaudeLaneRequestedEffortRefusedBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	marker := filepath.Join(t.TempDir(), "launched")
	wrapper := filepath.Join(t.TempDir(), "claude-minimax")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := newClaudeLaneAdapter(store, "claude_minimax", time.Hour).Run(context.Background(), RunInput{
		WorkID: "work_effort_refusal", Workspace: home, Command: wrapper, Prompt: "p",
		Now: time.Now().UTC(), preflighted: true,
		ExecutionSelectionSnapshot: &runattempt.ExecutionSelectionSnapshot{
			Source: "override", Harness: "claude_minimax", RequestedModelID: "MiniMax-M3", RequestedEffort: "xhigh",
			EffortEvidence: "unsupported:harness claude_minimax cannot apply a requested effort",
		},
	})
	var refusal *SelectionUnsupportedError
	if !errors.As(err, &refusal) || err.Error() != "selection_unsupported:claude_minimax:effort" {
		t.Fatalf("err = %v, want selection_unsupported:claude_minimax:effort", err)
	}
	if out.RunAttempt.ID != "" {
		t.Fatalf("refusal minted attempt %q", out.RunAttempt.ID)
	}
	if attempts, listErr := store.List(); listErr != nil || len(attempts) != 0 {
		t.Fatalf("refusal mutated the RunAttempt store: %+v err=%v", attempts, listErr)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("refused launch still ran the wrapper (stat err=%v)", statErr)
	}
}

func fakeCodexBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Codex keeps its existing behavior (effort on argv, attested applied) and the
// stamp now also carries the settings digest and the observed leg read back
// from the built argv plus the live hook posture.
func TestCodexEffortAppliedWithSettingsDigestAndObservedLeg(t *testing.T) {
	t.Setenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION", "")
	restore := SwapHookPostureReader(func(name string) (HookPostureReading, bool) {
		if name != "codex" {
			return HookPostureReading{}, false
		}
		return HookPostureReading{Posture: "enforce", ConfigPath: "/fixture/.codex/hooks.json", ConfigDigest: "sha256:fixture"}, true
	})
	defer restore()
	store := runattempt.Open(t.TempDir())
	out, err := NewCodexAdapterWithInterval(store, time.Hour).Run(context.Background(), RunInput{
		WorkID: "work_codex_observed", Harness: "codex", Workspace: t.TempDir(), Prompt: "p",
		Command: fakeCodexBinary(t), RequestedModelID: "gpt-fixture", Now: time.Now().UTC(), preflighted: true,
		ExecutionSelectionSnapshot: &runattempt.ExecutionSelectionSnapshot{
			Source: "override", Harness: "codex", RequestedModelID: "gpt-fixture", RequestedEffort: "high",
			EffortEvidence: "unobservable:codex effort application awaits adapter argv attestation",
		},
	})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("codex run: out=%+v err=%v", out, err)
	}
	ra, err := store.Get(out.RunAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	sel := ra.ExecutionSelection
	if sel == nil || sel.AppliedEffort != "high" || sel.EffortEvidence != "argv:model_reasoning_effort" {
		t.Fatalf("codex applied effort lost: %+v", sel)
	}
	if !strings.HasPrefix(sel.SettingsDigest, "sha256:") || sel.SettingsDigest != settingsDigest("codex", HookPostureReading{Posture: "enforce", ConfigPath: "/fixture/.codex/hooks.json", ConfigDigest: "sha256:fixture"}) {
		t.Fatalf("settings digest not stamped from the live hook posture: %q", sel.SettingsDigest)
	}
	obs := ra.Observed
	if obs == nil || obs.ObservedModelID != "gpt-fixture" || obs.ObservedEffort != "high" || obs.ObservedHookPosture != "enforce" {
		t.Fatalf("observed leg = %+v, want model+effort from argv and enforce posture", obs)
	}
	for _, want := range []string{"argv:-m", "argv:model_reasoning_effort", "hook_config:/fixture/.codex/hooks.json"} {
		if !strings.Contains(obs.Source, want) {
			t.Fatalf("observed source %q lacks %q", obs.Source, want)
		}
	}
	if sel.RequestedEffort != "high" || sel.RequestedModelID != "gpt-fixture" {
		t.Fatalf("requested leg rewritten by observation: %+v", sel)
	}
}

// Nothing observable (no argv/env observer for the harness, no known hook
// config) leaves Observed nil -- absent from the JSON, not a zeroed block --
// and no settings digest is invented for an unknown posture.
func TestObservedLegAbsentWhenNothingObservable(t *testing.T) {
	restore := SwapHookPostureReader(nil)
	defer restore()
	home := t.TempDir()
	store := runattempt.Open(home)
	out, err := NewOSEntrypointsAdapterWithInterval(store, time.Hour).Run(context.Background(), RunInput{
		WorkID: "work_nothing_observed", Workspace: home, Command: "/usr/bin/true", Now: time.Now().UTC(),
		ExecutionSelectionSnapshot: &runattempt.ExecutionSelectionSnapshot{Source: "project_default", Harness: "os-harness-entrypoints"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ra, err := store.Get(out.RunAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Observed != nil {
		t.Fatalf("observed leg must be absent, got %+v", ra.Observed)
	}
	if ra.ExecutionSelection == nil || ra.ExecutionSelection.SettingsDigest != "" {
		t.Fatalf("unknown hook posture must not be digested: %+v", ra.ExecutionSelection)
	}
	raw, err := json.Marshal(ra)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"observed"`) {
		t.Fatalf("absent observation serialized as a key: %s", raw)
	}
}

// A claude lane's observed model/effort come from what its wrapper exports,
// which is what will run regardless of what the selection asked for.
func TestClaudeLaneObservedLegReadsWrapperEnv(t *testing.T) {
	restore := SwapHookPostureReader(nil)
	defer restore()
	home := t.TempDir()
	store := runattempt.Open(home)
	wrapper := filepath.Join(t.TempDir(), "claude-minimax")
	for _, key := range []string{"SB_LANE_CLAUDE_MODEL", "SB_LANE_REQUESTED_EFFORT"} {
		t.Setenv(key, "") // restore on cleanup; the run must not inherit a lane's own values
		os.Unsetenv(key)
	}
	script := "#!/bin/sh\nset -eu\nexport SB_LAUNCH_PROFILE_ID='claude-minimax'\nexport SB_LANE_REQUESTED_EFFORT='xhigh'\nexport SB_LANE_CLAUDE_MODEL='MiniMax-M3[1m]'\nexit 0\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := newClaudeLaneAdapter(store, "claude_minimax", time.Hour).Run(context.Background(), RunInput{
		WorkID: "work_wrapper_env", Workspace: home, Command: wrapper, Prompt: "p", Now: time.Now().UTC(), preflighted: true,
		ExecutionSelectionSnapshot: &runattempt.ExecutionSelectionSnapshot{Source: "saved", SelectionRevision: 1, Harness: "claude_minimax", RequestedModelID: "MiniMax-M3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ra, err := store.Get(out.RunAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Observed == nil || ra.Observed.ObservedModelID != "MiniMax-M3[1m]" || ra.Observed.ObservedEffort != "xhigh" || ra.Observed.Source != "wrapper_env:"+wrapper {
		t.Fatalf("wrapper env not observed: %+v", ra.Observed)
	}
}
