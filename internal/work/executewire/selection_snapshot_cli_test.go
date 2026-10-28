package executewire

import (
	"errors"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
)

// BuildSelectionSnapshot contract (2026-09-15): the public-CLI selection
// snapshot builder resolves override > saved > project_default and keeps
// requested vs applied apart.

func cliSavedSelection() *envelope.ExecutionSelection {
	return &envelope.ExecutionSelection{
		LaunchProfileID:       "saved-profile",
		LaunchProfileRevision: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Harness:               "claude_code",
		ProviderRoute:         "minimax/fixture-route",
		RequestedModelID:      "fixture-model",
		RequestedEffort:       "high",
		Source:                "explicit",
		Revision:              3,
		SelectedBy:            "operator",
		SelectedAt:            time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
}

func TestBindCodexSelectionBeforePreflightPreservesRequestedEffort(t *testing.T) {
	selection := cliSavedSelection()
	selection.Harness = "codex"
	selection.RequestedEffort = "medium"
	input, err := BindSelectionBeforePreflight(
		envelope.WorkEnvelope{ExecutionSelection: selection}, "codex", nil,
		harness.RunInput{Harness: "codex"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if input.ExecutionSelectionSnapshot == nil || input.ExecutionSelectionSnapshot.RequestedEffort != "medium" || input.ExecutionSelectionSnapshot.AppliedEffort != "" {
		t.Fatalf("Codex effort was silently discarded or prematurely claimed applied: %+v", input.ExecutionSelectionSnapshot)
	}
}

func TestBuildSelectionSnapshotCodexSessionIsUnobservable(t *testing.T) {
	selection := cliSavedSelection()
	selection.Harness = "codex"
	snap, err := BuildSelectionSnapshot(envelope.WorkEnvelope{ExecutionSelection: selection}, "codex", nil)
	if err != nil {
		t.Fatalf("BuildSelectionSnapshot: %v", err)
	}
	if snap.NativeSessionID != "" {
		t.Errorf("native_session_id = %q; want empty until captured", snap.NativeSessionID)
	}
	const want = "unobservable:Codex native session ID is not captured by this adapter"
	if snap.NativeSessionEvidence != want {
		t.Errorf("native_session_evidence = %q; want %q", snap.NativeSessionEvidence, want)
	}
}

func TestBuildSelectionSnapshotSavedConsumesRevisionAndCarriesEffortForClaudeFamily(t *testing.T) {
	wenv := envelope.WorkEnvelope{ExecutionSelection: cliSavedSelection()}
	snap, err := BuildSelectionSnapshot(wenv, "claude_code", nil)
	if err != nil {
		t.Fatalf("BuildSelectionSnapshot: %v", err)
	}
	if snap.Source != "saved" || snap.SelectionRevision != wenv.ExecutionSelection.Revision {
		t.Fatalf("saved snapshot must stamp the consumed revision: %+v", snap)
	}
	if snap.LaunchProfileID != "saved-profile" || snap.Harness != "claude_code" {
		t.Fatalf("saved snapshot identity wrong: %+v", snap)
	}
	if snap.ProviderRoute != "minimax/fixture-route" || snap.RequestedModelID != "fixture-model" {
		t.Fatalf("saved snapshot must carry the saved route/model: %+v", snap)
	}
	// claude lanes cannot apply a requested effort (the wrapper pins its own):
	// the request survives verbatim, the evidence says unsupported, and the
	// bind refuses instead of launching without it.
	if snap.RequestedEffort != "high" || !strings.HasPrefix(snap.EffortEvidence, "unsupported:") || snap.AppliedEffort != "" {
		t.Fatalf("claude-family effort must be kept and marked unsupported: %+v", snap)
	}
	_, err = BindSelectionBeforePreflight(wenv, "claude_code", nil, harness.RunInput{Harness: "claude_code"})
	var refusal *harness.SelectionUnsupportedError
	if !errors.As(err, &refusal) || err.Error() != "selection_unsupported:claude_code:effort" {
		t.Fatalf("bind err = %v, want selection_unsupported:claude_code:effort", err)
	}
}

// The default branch used to clear RequestedEffort and record "unsupported";
// the launch then ran without it. The request is now never cleared, for any
// harness, and every registered claude lane reads the same row (the old name
// list covered 5 names, one of them unregistered).
func TestSelectionSnapshotNeverClearsRequestedEffort(t *testing.T) {
	// Lane names, not credentials; gitleaks reads "..._api", "<next name>" as a key.
	for _, name := range []string{"os-harness-entrypoints", "oh_my_pi", "claude_kimi", "claude_deepseek_flash_api", "claude_gpt56_sol_wpcom"} { // gitleaks:allow
		selection := cliSavedSelection()
		selection.Harness = name
		selection.RequestedEffort = "xhigh"
		snap, err := BuildSelectionSnapshot(envelope.WorkEnvelope{ExecutionSelection: selection}, name, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if snap.RequestedEffort != "xhigh" || !strings.HasPrefix(snap.EffortEvidence, "unsupported:") {
			t.Fatalf("%s: requested effort cleared or mis-evidenced: %+v", name, snap)
		}
	}
	snap, err := BuildSelectionSnapshot(envelope.WorkEnvelope{ExecutionSelection: cliSavedSelection()}, "claude_kimi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snap.NativeSessionEvidence, "unsupported:wire does not pin --session-id") {
		t.Fatalf("claude_kimi session evidence not from the claude lane row: %q", snap.NativeSessionEvidence)
	}
}

// A harness that cannot bind a requested model refuses at bind, before the
// public CLI changes any envelope state.
func TestBindSelectionRefusesUnapplicableModel(t *testing.T) {
	selection := cliSavedSelection()
	selection.Harness = "os-harness-entrypoints"
	selection.RequestedEffort = ""
	_, err := BindSelectionBeforePreflight(envelope.WorkEnvelope{ExecutionSelection: selection}, "os-harness-entrypoints", nil, harness.RunInput{})
	if err == nil || err.Error() != "selection_unsupported:os-harness-entrypoints:model" {
		t.Fatalf("bind err = %v, want selection_unsupported:os-harness-entrypoints:model", err)
	}
}

func TestBuildSelectionSnapshotNoSavedSelectionFallsThroughToProjectDefault(t *testing.T) {
	snap, err := BuildSelectionSnapshot(envelope.WorkEnvelope{}, "claude_code", nil)
	if err != nil {
		t.Fatalf("BuildSelectionSnapshot: %v", err)
	}
	if snap.Source != "project_default" || snap.Harness != "claude_code" {
		t.Fatalf("project_default snapshot wrong: %+v", snap)
	}
	if snap.SelectionRevision != 0 {
		t.Fatalf("project_default must not carry a selection revision: %+v", snap)
	}
}

// A one-run override on a harness that can bind it is recorded as
// source=override with the override's own model and effort, and binds the
// codex input to that tuple; the saved selection is not consulted.
func TestBindSelectionOverrideOnCodexCarriesOverrideTuple(t *testing.T) {
	override := &envelope.ExecutionSelection{
		LaunchProfileID: "codex-high", LaunchProfileRevision: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Harness: "codex", ProviderRoute: "chatgpt-subscription", RequestedModelID: "gpt-fixture", RequestedEffort: "high",
	}
	input, err := BindSelectionBeforePreflight(envelope.WorkEnvelope{ExecutionSelection: cliSavedSelection()}, "codex", override, harness.RunInput{Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	snap := input.ExecutionSelectionSnapshot
	if snap == nil || snap.Source != "override" || snap.LaunchProfileID != "codex-high" || snap.RequestedModelID != "gpt-fixture" || snap.RequestedEffort != "high" || snap.SelectionRevision != 0 {
		t.Fatalf("override snapshot wrong: %+v", snap)
	}
	if input.RequestedModelID != "gpt-fixture" || input.RequestedProviderRoute != "chatgpt-subscription" || input.LaunchProfileID != "codex-high" {
		t.Fatalf("codex input not bound to the override tuple: %+v", input)
	}
}
