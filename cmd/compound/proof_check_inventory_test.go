package main

import (
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestProofCheckSmokeInventoryGuard fails whenever a console/scripts/*smoke*.mjs file is
// neither wired into proofCheckSteps as a proof-check gate nor named in the manual-only
// allowlist below with a non-empty reason. A console smoke has gone silently unwired
// before (a new smoke lands, proof-check never learns about it, and it never runs
// anywhere) — this guard turns that into a loud, named test failure instead of quiet
// proof drift.
func TestProofCheckSmokeInventoryGuard(t *testing.T) {
	t.Parallel()
	// manualOnlySmokes lists every *smoke*.mjs that is intentionally NOT wired into
	// proofCheckSteps, with the reason it cannot run in the portable proof-check path.
	// Every entry here must still exist on disk and must NOT also be wired as a step —
	// an allowlist row with no backing reason, or a stale row for a deleted file,
	// defeats the point of the guard.
	manualOnlySmokes := map[string]string{
		"work-mutation-trail-smoke.mjs": "requires COMPOUND_BIN (a freshly built compound binary); exercised manually / in strict-local batteries, not in the portable proof-check path",
	}

	repoRoot := inventoryGuardRepoRoot(t)

	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatalf("proofCheckSteps(\"\"): %v", err)
	}

	wired := map[string]bool{}
	for _, step := range steps {
		for _, arg := range step.Command {
			if strings.HasSuffix(arg, ".mjs") {
				wired[filepath.Base(arg)] = true
			}
		}
	}

	pattern := filepath.Join(repoRoot, "console", "scripts", "*smoke*.mjs")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(matches) == 0 {
		t.Fatalf("glob %s matched no files; repo root resolution is likely wrong", pattern)
	}

	onDisk := map[string]bool{}
	for _, m := range matches {
		onDisk[filepath.Base(m)] = true
	}

	var unclassified []string
	for name := range onDisk {
		if wired[name] {
			continue
		}
		if reason, allowlisted := manualOnlySmokes[name]; allowlisted && strings.TrimSpace(reason) != "" {
			continue
		}
		unclassified = append(unclassified, name)
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf(
			"console/scripts smoke(s) neither wired into proofCheckSteps nor allowlisted with a reason: %s\n"+
				"remedy: wire each into proofCheckSteps (cmd/compound/proof_check.go) as a console-smoke step, "+
				"OR add it to manualOnlySmokes in this test with a non-empty reason",
			strings.Join(unclassified, ", "),
		)
	}

	var wiredAndAllowlisted []string
	for name := range manualOnlySmokes {
		if wired[name] {
			wiredAndAllowlisted = append(wiredAndAllowlisted, name)
		}
	}
	sort.Strings(wiredAndAllowlisted)
	if len(wiredAndAllowlisted) > 0 {
		t.Errorf(
			"console/scripts smoke(s) are both wired into proofCheckSteps and manual-only allowlisted (pick one): %s",
			strings.Join(wiredAndAllowlisted, ", "),
		)
	}

	var stale []string
	var pendingRegistration []string
	for name, reason := range manualOnlySmokes {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("manual-only allowlist entry %q has an empty reason", name)
		}
		if !onDisk[name] {
			stale = append(stale, name)
		}
		// stopgapReason was the placeholder wording for four smokes whose proof_check.go
		// registration was blocked by a live concurrent lane at allowlist-landing time
		// (2026-07-17, work_738ac4eabc98 retired the class). Built from two literals so
		// this guard's own source never re-introduces the retired phrase as one literal
		// run — the very grep this class of debt is checked against (see the packet)
		// scans for that exact contiguous wording in this file.
		stopgapReason := "registration" + " " + "pending"
		if strings.Contains(strings.ToLower(reason), stopgapReason) {
			pendingRegistration = append(pendingRegistration, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf(
			"manual-only allowlist entry(ies) reference file(s) no longer on disk (stale allowlist row, remove): %s",
			strings.Join(stale, ", "),
		)
	}
	sort.Strings(pendingRegistration)
	if len(pendingRegistration) > 0 {
		t.Errorf(
			"manual-only allowlist entry(ies) use the retired stopgap reason wording, which must not recur "+
				"(wire the smoke into proofCheckSteps and delete the row, or give it a real, non-stopgap reason): %s",
			strings.Join(pendingRegistration, ", "),
		)
	}
}

// inventoryGuardRepoRoot resolves the repository root from this test file's own path so the
// guard works regardless of the working directory `go test` is invoked from.
func inventoryGuardRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot resolve repo root")
	}
	// this file lives at <repoRoot>/cmd/compound/proof_check_inventory_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// The driver owns the unchanged, explicit per-package -timeout=40m budget.
// TestBatteryPartitionsEntireInventoryExactlyOnce pins both child commands;
// this guard proves the canonical and portable gate actually uses that driver.
func TestGoTestGateCarriesExplicitDeliberateTimeout(t *testing.T) {
	t.Parallel()
	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatalf("proofCheckSteps: %v", err)
	}
	for _, s := range steps {
		if s.Name != "go-test" {
			continue
		}
		if !s.CIPortable {
			t.Fatalf("go-test gate must stay CIPortable so the explicit timeout budget governs CI runs too; got CIPortable=false")
		}
		if want := []string{"go", "run", "./scripts/testbattery", "general-release"}; !reflect.DeepEqual(s.Command, want) {
			t.Fatalf("go-test command = %v; want budget-owning driver %v", s.Command, want)
		}
		return
	}
	t.Fatalf("no go-test step found in proofCheckSteps inventory")
}

func TestCanonicalGoGatesShareSourceInventoryDriver(t *testing.T) {
	t.Parallel()
	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"go-test":      {"go", "run", "./scripts/testbattery", "general-release"},
		"go-test-home": {"go", "run", "./scripts/testbattery", "home"},
		"go-vet":       {"go", "run", "./scripts/testbattery", "vet"},
		"go-build":     {"go", "run", "./scripts/testbattery", "build"},
	}
	for _, step := range steps {
		command, ok := want[step.Name]
		if !ok {
			continue
		}
		if !step.CIPortable || !proofCheckUsesGo(step) || !reflect.DeepEqual(step.Command, command) {
			t.Errorf("gate %s: command=%v portable=%t; want shared source driver %v", step.Name, step.Command, step.CIPortable, command)
		}
		delete(want, step.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing source gates: %v", want)
	}
}

func TestCanonicalGoGatesBindEveryInventoryToSealedHead(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	steps, err := proofCheckSteps("", head)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{"go-test": "general-release", "go-test-home": "home", "go-vet": "vet", "go-build": "build"}
	count := 0
	for _, step := range steps {
		mode, ok := modes[step.Name]
		if !ok {
			continue
		}
		count++
		want := []string{"go", "run", "./scripts/testbattery", mode, "--source-head", head}
		if !reflect.DeepEqual(step.Command, want) {
			t.Errorf("%s lost sealed source authority: %v; want %v", step.Name, step.Command, want)
		}
	}
	if count != 4 {
		t.Fatalf("sealed source gates = %d; want 4", count)
	}
}

// Pin the pre-isolation inventory, not a second call to the same generator.
func TestProofCheckPackageIsolationPreservesCanonicalGateInventory(t *testing.T) {
	t.Parallel()
	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{"go-test", "go-test-count-1-general-release"},
		{"go-test-home", "go-test-count-1-home"},
		{"credential-password-alignment-native", "credential-password-alignment-native-pg17-no-skips"},
		{"go-vet", "go-vet-all"},
		{"go-build", "go-build-all"},
		{"gofmt", "gofmt-check"},
		{"diff-check", "diff-check"},
		{"console-tsc", "console-tsc-noemit"},
		{"console-smoke", "console-smoke"},
		{"console-projection-timeout-smoke", "console-projection-timeout-smoke"},
		{"console-mutation-gate-smoke", "console-mutation-gate-smoke"},
		{"console-instance-pin-smoke", "console-instance-pin-smoke"},
		{"console-continue-button-smoke", "console-continue-button-smoke"},
		{"console-work-action-row-smoke", "console-work-action-row-smoke"},
		{"console-mutation-readiness-consumers-smoke", "console-mutation-readiness-consumers-smoke"},
		{"console-private-fallback-smoke", "console-private-fallback-smoke"},
		{"console-private-runtime-guard-smoke", "console-private-runtime-guard-smoke"},
		{"console-move-type-drift-smoke", "console-move-type-drift-smoke"},
		{"console-ai-boundary-smoke", "console-ai-boundary-smoke"},
		{"console-client-bundle-guard-smoke", "console-client-bundle-guard-smoke"},
		{"console-work-trace-smoke", "console-work-trace-smoke"},
		{"console-lineage-smoke", "console-lineage-smoke"},
		{"console-generic-actions-smoke", "console-generic-actions-smoke"},
		{"console-view-registry-smoke", "console-view-registry-smoke"},
		{"console-decide-brief-smoke", "console-decide-brief-smoke"},
		{"console-decision-execute-smoke", "console-decision-execute-smoke"},
		{"console-standing-law-smoke", "console-standing-law-smoke"},
		{"console-memory-data-smoke", "console-memory-data-smoke"},
		{"console-memory-read-honesty-smoke", "console-memory-read-honesty-smoke"},
		{"console-evidence-card-smoke", "console-evidence-card-smoke"},
		{"console-attention-consistency-smoke", "console-attention-consistency-smoke"},
		{"console-work-language-smoke", "console-work-language-smoke"},
		{"console-work-legibility-smoke", "console-work-legibility-smoke"},
		{"console-ask-triage-smoke", "console-ask-triage-smoke"},
		{"console-representation-falsifiers-smoke", "console-representation-falsifiers-smoke"},
		{"console-representation-cluster-guard-smoke", "console-representation-cluster-guard-smoke"},
		{"console-agents-consistency-smoke", "console-agents-consistency-smoke"},
		{"console-projection-doors-smoke", "console-projection-doors-smoke"},
		{"console-stream-tail-smoke", "console-stream-tail-smoke"},
		{"console-decide-decision-feedback-smoke", "console-decide-decision-feedback-smoke"},
		{"console-projection-degradation-smoke", "console-projection-degradation-smoke"},
		{"console-stream-door-smoke", "console-stream-door-smoke"},
		{"console-traffic-board-smoke", "console-traffic-board-smoke"},
		{"console-ads-smoke", "console-ads-smoke"},
		{"console-delegated-sessions-smoke", "console-delegated-sessions-smoke"},
		{"console-coordination-smoke", "console-coordination-smoke"},
		{"bun-tools-test", "bun-tools-test"},
		{"architecture-doctor", "architecture-doctor-strict"},
		{"profiles-check", "profiles-check-installed-only"},
		{"workbench-projection", "workbench-projection-refresh"},
		{"codex-pretooluse-native-gate", "codex-pretooluse-native-gate-proof"},
		{"memory-doctor", "memory-doctor"},
		{"redact-public", "redact-public-clean"},
	}
	var got [][2]string
	for _, step := range steps {
		got = append(got, [2]string{step.Name, step.ProofRef})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical gate inventory changed\n got: %v\nwant: %v", got, want)
	}
}
