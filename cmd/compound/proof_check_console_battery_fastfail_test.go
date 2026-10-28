package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunProofCheckStepConsoleBatteryFastFailRespectsBoundedBudget pins the
// dispatcher-level contract: when a console/* bun step sits in an environment that
// would otherwise hang the battery (missing node_modules, or an inherited loopback
// proxy), the step returns a NAMED reason within a bounded time budget - it never
// lets the bun auto-install CONNECT block the suite.
//
// The bug being falsified: the 2026-08-14 64-minute hang recorded for
// console-view-registry-smoke. Without the fast-fail, the dispatcher would invoke
// `bun scripts/...` from console/, bun would attempt an auto-install, the
// auto-install would walk the inherited HTTPS_PROXY into a loopback TCP that never
// serves, and the step would block. The test reproduces the falsifier condition
// (bound to a generous-but-strict 2-second budget) and asserts the dispatcher
// short-circuits.
//
// The test mocks nothing - it exercises the real runProofCheckStepContext with a
// real (but dangerously hostile) environment, then asserts:
//  1. result.Status == "failed"
//  2. result.OutputTail contains the NAMED reason "console_deps_missing"
//  3. elapsed time stays well under the budget (with the fix: sub-100ms; without
//     it: the context deadline). The elapsed check is the load-bearing one: even
//     if a future refactor routes through a different code path, the bounded-time
//     invariant must hold.
func TestRunProofCheckStepConsoleBatteryFastFailRespectsBoundedBudget(t *testing.T) {
	// Defensive env reset on case-insensitive macOS so prior subtests / harness
	// state cannot leak in and produce a false-pass.
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	os.Unsetenv("HTTPS_PROXY")
	os.Unsetenv("https_proxy")
	t.Cleanup(func() {
		os.Unsetenv("HTTPS_PROXY")
		os.Unsetenv("https_proxy")
	})

	repo := t.TempDir()
	// No console/node_modules: this is the falsifier condition.

	if _, lookErr := exec.LookPath("bun"); lookErr != nil {
		t.Skip("bun not on PATH; the dispatcher SkipIfMissing path is exercised elsewhere; this test needs bun to reach the fast-fail path")
	}

	step := proofCheckStep{
		Name:          "console-view-registry-smoke",
		Command:       []string{"bun", "scripts/view-registry-smoke.mjs"},
		Dir:           "console",
		SkipIfMissing: "bun",
		ProofRef:      "console-view-registry-smoke",
	}

	budget := 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	result := runProofCheckStepContext(ctx, repo, step, "")
	elapsed := time.Since(start)

	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed (output_tail=%q)", result.Status, result.OutputTail)
	}
	if !strings.Contains(result.OutputTail, "console_deps_missing") {
		t.Fatalf("output_tail missing NAMED reason console_deps_missing: %q", result.OutputTail)
	}
	// Sub-budget assertion: the fix returns the named reason in <100ms. Allow a
	// generous safety margin (half the budget) so a slow CI runner does not fail
	// a passing fix while still failing a regression that invokes bun.
	if elapsed >= budget/2 {
		t.Fatalf("dispatcher took %v, exceeded fast-fail budget %v: NOT short-circuiting the hang class", elapsed, budget/2)
	}
}

// TestRunProofCheckStepConsoleBatteryFastFailDetectsLoopbackProxy pins the second
// recorded signature: console/node_modules present, but HTTPS_PROXY points at a
// loopback model gateway. The fast-fail must catch it before bun walks the proxy.
func TestRunProofCheckStepConsoleBatteryFastFailDetectsLoopbackProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	os.Unsetenv("HTTPS_PROXY")
	os.Unsetenv("https_proxy")
	t.Cleanup(func() {
		os.Unsetenv("HTTPS_PROXY")
		os.Unsetenv("https_proxy")
	})
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	repo := t.TempDir()
	// Create console/node_modules so the deps-missing fast-fail does NOT fire;
	// only the proxy check should.
	if err := os.MkdirAll(filepath.Join(repo, "console", "node_modules"), 0o755); err != nil {
		t.Fatalf("mkdir console/node_modules: %v", err)
	}

	if _, lookErr := exec.LookPath("bun"); lookErr != nil {
		t.Skip("bun not on PATH")
	}

	step := proofCheckStep{
		Name:          "console-view-registry-smoke",
		Command:       []string{"bun", "scripts/view-registry-smoke.mjs"},
		Dir:           "console",
		SkipIfMissing: "bun",
		ProofRef:      "console-view-registry-smoke",
	}

	budget := 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	result := runProofCheckStepContext(ctx, repo, step, "")
	elapsed := time.Since(start)

	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed (output_tail=%q)", result.Status, result.OutputTail)
	}
	if !strings.Contains(result.OutputTail, "proxy_env_blocks_bun") {
		t.Fatalf("output_tail missing NAMED reason proxy_env_blocks_bun: %q", result.OutputTail)
	}
	if elapsed >= budget/2 {
		t.Fatalf("dispatcher took %v, exceeded fast-fail budget %v: NOT short-circuiting the hang class", elapsed, budget/2)
	}
}

// TestRunProofCheckStepConsoleBatteryFastFailIgnoresNonBunSteps asserts the policy
// is narrow: a step whose command is not bun, or whose Dir is not console/, must
// pass through the fast-fail gate and reach the normal dispatcher. This guards
// against an over-broad fix that silently re-classifies unrelated gates.
func TestRunProofCheckStepConsoleBatteryFastFailIgnoresNonBunSteps(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	os.Unsetenv("HTTPS_PROXY")
	os.Unsetenv("https_proxy")
	t.Cleanup(func() {
		os.Unsetenv("HTTPS_PROXY")
		os.Unsetenv("https_proxy")
	})
	// Even with the hostile env, a non-bun / non-console step is unaffected.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	repo := t.TempDir()
	// No console/node_modules.

	step := proofCheckStep{
		Name:     "synthetic-go-step",
		Command:  []string{"/bin/sh", "-c", "printf ok\\n; exit 0"},
		Dir:      "",
		ProofRef: "synthetic-go-step",
	}

	result := runProofCheckStepContext(context.Background(), repo, step, "")
	if result.Status == "failed" && strings.Contains(result.OutputTail, "console_deps_missing") {
		t.Fatalf("non-bun step was incorrectly fast-failed: %+v", result)
	}
	if result.Status == "failed" && strings.Contains(result.OutputTail, "proxy_env_blocks_bun") {
		t.Fatalf("non-bun step was incorrectly fast-failed: %+v", result)
	}
}
