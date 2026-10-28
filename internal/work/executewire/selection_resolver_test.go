package executewire

import (
	"errors"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// The installed CLI refused every --launch-profile with "no execution
// selection resolver installed" because nothing bound the seam in
// production. These tests pin that Install binds it and that the binding
// reports exactly what the owner resolution says — no invented effort, no
// alias, the owner's refusal verbatim.

func stubOwnerResolver(t *testing.T, fn work.DelegateHarnessResolverFunc) {
	t.Helper()
	prev := work.DelegateHarnessResolver
	work.DelegateHarnessResolver = fn
	t.Cleanup(func() { work.DelegateHarnessResolver = prev })
}

func TestInstallBindsTheExecutionSelectionResolver(t *testing.T) {
	prev := work.DefaultExecutionSelectionResolver
	work.DefaultExecutionSelectionResolver = nil
	t.Cleanup(func() { work.DefaultExecutionSelectionResolver = prev })
	Install()
	if work.DefaultExecutionSelectionResolver == nil {
		t.Fatal("Install left work.DefaultExecutionSelectionResolver nil — the installed CLI would refuse every --launch-profile")
	}
}

func TestResolveExecutionSelectionViaOwnerReportsTheOwnerTuple(t *testing.T) {
	stubOwnerResolver(t, func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		if req.Harness != "claude-minimax" || req.HomePath == "" {
			t.Fatalf("owner consulted with the wrong request: %+v", req)
		}
		return work.DelegateHarnessResolution{
			Harness:                "claude_minimax",
			Command:                "/opt/lanes/claude-minimax",
			LaneID:                 "claude_minimax",
			RequestedModelID:       "claude-minimax (token plan)",
			RequestedProviderRoute: "minimax",
			TupleBacked:            true,
		}, nil
	})
	now := time.Date(2026, 9, 12, 2, 0, 0, 0, time.UTC)
	res, err := ResolveExecutionSelectionViaOwner(work.ExecutionSelectionResolveRequest{HomePath: t.TempDir(), LaunchProfileID: "claude-minimax", Now: now})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Harness != "claude_minimax" || res.RequestedModelID != "claude-minimax (token plan)" || res.ProviderRoute != "minimax" || res.LaunchProfileID != "claude-minimax" {
		t.Fatalf("resolution does not mirror the owner tuple: %+v", res)
	}
	if res.RequestedEffort != "" {
		t.Fatalf("resolver invented an effort the owner never stated: %q", res.RequestedEffort)
	}

	// The full save path accepts it: this is what `work create --launch-profile` runs.
	restore := work.SwapExecutionSelectionResolver(ResolveExecutionSelectionViaOwner)
	t.Cleanup(restore)
	sel, err := work.ResolveSelection(work.ExecutionSelectionResolveRequest{HomePath: t.TempDir(), LaunchProfileID: "claude-minimax", EffortOverride: "xhigh", Now: now}, "claude_code:orchestrator", now)
	if err != nil {
		t.Fatalf("ResolveSelection through the production binding: %v", err)
	}
	if sel.Harness != "claude_minimax" || sel.RequestedEffort != "xhigh" || sel.Revision != 1 || sel.Source != "explicit" {
		t.Fatalf("saved selection wrong: %+v", sel)
	}
}

func TestResolveExecutionSelectionViaOwnerRefusesHonestly(t *testing.T) {
	stubOwnerResolver(t, func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		switch req.Harness {
		case "ghost-lane":
			return work.DelegateHarnessResolution{}, errors.New("no Compound lane tuple for ghost-lane in the model-router authority")
		case "codex":
			return work.DelegateHarnessResolution{Harness: "codex", TupleBacked: false}, nil
		}
		return work.DelegateHarnessResolution{}, errors.New("unexpected")
	})
	home := t.TempDir()
	if _, err := ResolveExecutionSelectionViaOwner(work.ExecutionSelectionResolveRequest{HomePath: home, LaunchProfileID: "ghost-lane"}); err == nil || !strings.Contains(err.Error(), "no Compound lane tuple for ghost-lane") {
		t.Fatalf("owner refusal must surface verbatim, got %v", err)
	}
	if _, err := ResolveExecutionSelectionViaOwner(work.ExecutionSelectionResolveRequest{HomePath: home, LaunchProfileID: "codex"}); err == nil || !strings.Contains(err.Error(), "not a tuple-backed lane") {
		t.Fatalf("a non-tuple lane must be refused, got %v", err)
	}
	if _, err := ResolveExecutionSelectionViaOwner(work.ExecutionSelectionResolveRequest{HomePath: home}); err == nil {
		t.Fatal("an empty launch profile id must be refused")
	}
	prev := work.DelegateHarnessResolver
	work.DelegateHarnessResolver = nil
	t.Cleanup(func() { work.DelegateHarnessResolver = prev })
	if _, err := ResolveExecutionSelectionViaOwner(work.ExecutionSelectionResolveRequest{HomePath: home, LaunchProfileID: "claude-minimax"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("a missing owner resolver must fail closed, got %v", err)
	}
}
