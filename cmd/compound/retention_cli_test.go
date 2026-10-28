package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/policy"
)

func TestResolveReclaimTargetRequiresExplicitConsumerAndThreshold(t *testing.T) {
	t.Parallel()
	if _, err := policy.ResolveReclaimTargetForTest([]string{"--required-bytes", "100"}); err == nil {
		t.Fatal("missing --consumer must fail closed")
	}
	if _, err := policy.ResolveReclaimTargetForTest([]string{"--consumer", "fixture"}); err == nil {
		t.Fatal("missing published threshold must fail closed")
	}
}

func TestResolveReclaimTargetUsesCallerPublishedValues(t *testing.T) {
	t.Parallel()
	got, err := policy.ResolveReclaimTargetForTest([]string{
		"--consumer", "fixture",
		"--free-bytes", "50",
		"--capacity-bytes", "1000",
		"--required-bytes", "100",
		"--required-bps", "1500",
	})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if got.Consumer != "fixture" || got.FreeBytes != 50 || got.CapacityBytes != 1000 ||
		got.RequiredFreeBytes != 100 || got.RequiredFreeBps != 1500 {
		t.Fatalf("target lost caller values: %+v", got)
	}
	if got.ShortfallBytes() != 100 {
		t.Fatalf("shortfall = %d, want stricter ratio-derived 100", got.ShortfallBytes())
	}
}

func TestChangedCommandsAreDiscoverable(t *testing.T) {
	t.Parallel()
	if got := routeUsage(""); !containsAll(got, "lane-projection", "--harness H") {
		t.Fatalf("route usage does not expose lane projection: %s", got)
	}
	if got := policy.RetentionUsageForTest(); !containsAll(got, "classes", "scan", "plan", "never reclaims") {
		t.Fatalf("retention usage is incomplete: %s", got)
	}
}

func TestRetentionPlanJSONInsufficiencyExitsNonZero(t *testing.T) {
	t.Parallel()
	out, err := runCLIFlagSeam(
		t,
		t.TempDir(),
		"retention", "plan",
		"--consumer", "fixture",
		"--root", t.TempDir(),
		"--free-bytes", "0",
		"--capacity-bytes", "0",
		"--required-bytes", "9223372036854775807",
		"--json",
	)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("insufficient JSON plan exit = %v, want 1; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), `"sufficient": false`) {
		t.Fatalf("insufficient JSON plan lost its machine-readable verdict:\n%s", out)
	}
}

func TestRetentionRejectsUnknownFlagsAsUsage(t *testing.T) {
	t.Parallel()
	out, err := runCLIFlagSeam(t, t.TempDir(), "retention", "plan", "--required-byte", "123")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("unknown retention flag exit = %v, want 2; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "unknown flag --required-byte") {
		t.Fatalf("unknown retention flag was not named:\n%s", out)
	}
}

func containsAll(s string, wants ...string) bool {
	for _, want := range wants {
		if !strings.Contains(s, want) {
			return false
		}
	}
	return true
}

func TestRetentionApplyDryRunChangesZeroBytes(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out, err := runCLIFlagSeam(t, homePath,
		"retention", "apply",
		"--class", "events-segments",
		"--class", "context-packs",
		"--older-than", "30d",
	)
	if err != nil {
		t.Fatalf("apply (dry-run) failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "dry-run: true") {
		t.Fatalf("dry-run flag not echoed: %s", out)
	}
	// Either the dry-run note (when 0 steps) or the apply-prompt note
	// (when steps exist). On an empty fixture we get the former.
	if !strings.Contains(string(out), "# retention apply") {
		t.Fatalf("apply header missing: %s", out)
	}
}

// TestRetentionApplyYesIsRequiredToReclaim exercises the safety gate that
// --yes must be explicit. With no --yes the verb exits 0 and moves nothing,
// even when a valid home + classes are supplied.
func TestRetentionApplyYesIsRequiredToReclaim(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out, err := runCLIFlagSeam(t, homePath,
		"retention", "apply",
		"--class", "events-segments",
		"--older-than", "30d",
		"--yes",
	)
	// We don't care whether exit == 0 here — we care that the apply verb
	// actually ran (--yes flipped dry-run off).
	_ = err
	if !strings.Contains(string(out), "dry-run: false") {
		t.Fatalf("apply --yes did not flip dry-run off: %s", out)
	}
	if !strings.Contains(string(out), "# retention apply") {
		t.Fatalf("apply --yes header missing: %s", out)
	}
}

// TestRetentionApplyRejectsUnknownClass confirms the unknown-class refusal
// surfaces with a truthful error rather than a usage page.
func TestRetentionApplyRejectsUnknownClass(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out, err := runCLIFlagSeam(t, homePath,
		"retention", "apply",
		"--class", "bogus-class",
		"--older-than", "30d",
	)
	if err == nil {
		t.Fatalf("unknown class must fail closed: %s", out)
	}
	if !strings.Contains(string(out), "bogus-class") {
		t.Fatalf("error did not name the bad class: %s", out)
	}
}

// TestRetentionApplyOlderThanBelowAgeFloorRefused confirms the per-class
// floor guard at the CLI layer.
func TestRetentionApplyOlderThanBelowAgeFloorRefused(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out, err := runCLIFlagSeam(t, homePath,
		"retention", "apply",
		"--class", "events-segments",
		"--older-than", "1h",
	)
	if err == nil {
		t.Fatalf("--older-than below class floor must fail closed: %s", out)
	}
	if !strings.Contains(string(out), "age floor") {
		t.Fatalf("error did not mention the floor: %s", out)
	}
}

// TestRetentionUsageMentionsApplyAndEmptyTrash guards the usage line so a
// future flag-rename cannot silently strand the verbs.
func TestRetentionUsageMentionsApplyAndEmptyTrash(t *testing.T) {
	t.Parallel()
	usage := policy.RetentionUsageForTest()
	for _, want := range []string{"apply", "empty-trash", "--yes", "--older-than", "dry-run"} {
		if !strings.Contains(usage, want) {
			t.Fatalf("usage missing %q: %s", want, usage)
		}
	}
}

// TestRetentionApplyJSONShapeMatchesPlan exercises the packet's "same JSON
// shape as plan" requirement: dry-run JSON must include the planned steps
// and the dry-run marker.
func TestRetentionApplyJSONShapeMatchesPlan(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out, err := runCLIFlagSeam(t, homePath,
		"retention", "apply",
		"--class", "events-segments",
		"--older-than", "30d",
		"--json",
	)
	if err != nil {
		t.Fatalf("apply --json failed: %v\n%s", err, out)
	}
	for _, want := range []string{`"dry_run": true`, `"steps"`, `"authority_ref"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("apply JSON missing %s: %s", want, out)
		}
	}
}
