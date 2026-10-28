package main

import (
	"strings"
	"testing"
)

func TestRootUsageListsDispatchExecute(t *testing.T) {
	out := captureStderr(t, usage)
	if !strings.Contains(out, "dispatch-claim|dispatch-execute|dispatch-advance") {
		t.Fatalf("root usage omits selector execution:\n%s", out)
	}
}

func TestDispatchExecuteHelpExcludesHumanRouteSelection(t *testing.T) {
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-execute", "--help"})
	})
	for _, forbidden := range []string{"--actor", "--harness", "--model"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("dispatch-execute exposes human route selector %q:\n%s", forbidden, out)
		}
	}
}

func TestEveryWorkVerbHelpIncludesUsageAndExecutableExample(t *testing.T) {
	verbs := []string{
		"create-plan", "admit-plan", "review-card-plan", "create", "import-packet",
		"show", "list", "leases", "active-packet-proof", "proof-chain-doctor", "reconcile-run-attempts", "continue",
		"dispatch-plan", "dispatch-claim", "dispatch-execute", "dispatch-advance", "dispatch-close-plan",
		"dispatch-close", "dispatch-step", "claim-plan", "claim", "advance-plan", "advance",
		"delegate", "steer", "attach", "observe-run", "land-check", "salvage-plan",
		"action-plan", "act", "close-plan", "close",
		"set-aside", "sweep-stale",
	}
	for _, verb := range verbs {
		t.Run(verb, func(t *testing.T) {
			out := captureStdout(t, func() {
				cmdWork([]string{verb, "--help"})
			})
			if !strings.Contains(out, "usage: compound work "+verb) {
				t.Fatalf("%s help lacks verb-specific usage:\n%s", verb, out)
			}
			example := "example: compound work " + verb
			if !strings.Contains(out, example) {
				t.Fatalf("%s help lacks executable example prefix %q:\n%s", verb, example, out)
			}
			if strings.Contains(out, "<create-plan|") {
				t.Fatalf("%s help fell back to generic banner:\n%s", verb, out)
			}
		})
	}
}

func TestWorkVerbHelpShowsOwnFlags(t *testing.T) {
	tests := map[string][]string{
		"dispatch-claim":   {"--confirm", "without --confirm previews"},
		"dispatch-execute": {"--workspace", "--permission-mode", "--confirm", "without --confirm previews"},
		"delegate":         {"--harness", "--prompt", "--dry-run", "--experimental-delegate-mutations", "unsupported in v0.1"},
		"steer":            {"--text", "--enter", "--experimental-delegate-mutations", "unsupported in v0.1"},
		"show":             {"--json"},
		"close":            {"--outcome", "--move-type"},
		"sweep-stale":      {"--dry-run", "--yes", "--experimental-sweep-stale-mutations", "experimental", "unsupported in v0.1"},
	}
	for verb, fragments := range tests {
		out := captureStdout(t, func() {
			cmdWork([]string{verb, "--help"})
		})
		for _, fragment := range fragments {
			if !strings.Contains(out, fragment) {
				t.Fatalf("%s help lacks %q:\n%s", verb, fragment, out)
			}
		}
	}
}

func TestExperimentalSweepStaleMutationHelpCarriesTheGate(t *testing.T) {
	out := captureStdout(t, func() {
		cmdWork([]string{"sweep-stale", "--help"})
	})
	for _, want := range []string{
		"--yes --experimental-sweep-stale-mutations",
		"experimental",
		"unsupported in v0.1",
		"example: compound work sweep-stale --dry-run --threshold 48h",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sweep-stale help missing %q:\n%s", want, out)
		}
	}
}

func TestExperimentalDelegateMutationHelpExamplesCarryTheGate(t *testing.T) {
	tests := map[string]string{
		"delegate": "example: compound work delegate work_abc123 --harness claude-zai --experimental-delegate-mutations",
		"steer":    "example: compound work steer work_abc123 --experimental-delegate-mutations",
	}
	for verb, want := range tests {
		out := captureStdout(t, func() {
			cmdWork([]string{verb, "--help"})
		})
		if !strings.Contains(out, want) {
			t.Fatalf("%s help advertises an ungated mutation example; want %q:\n%s", verb, want, out)
		}
	}
}
