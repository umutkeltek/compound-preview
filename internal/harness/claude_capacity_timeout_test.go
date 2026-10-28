package harness

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// claudeCapacityTimeoutFixture wires one tuple-backed Claude-family lane
// (claude_minimax) with a declared CodexBar capacity source and a synthetic
// usage-history feed under a fake $HOME. No real provider, account, or
// network is involved: the adapter resolves a fake wrapper from PATH and the
// capacity guard reads only the fixture rows.
type claudeCapacityTimeoutFixture struct {
	home      string
	userHome  string
	usagePath string
	now       time.Time
}

func writeClaudeCapacityRows(t *testing.T, path string, rows ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func claudeCapacityRow(usedPercent float64, resetsAt, sampledAt time.Time) string {
	return `{"provider":"minimax","windowKind":"5h","windowMinutes":300,"usedPercent":` +
		strconv.FormatFloat(usedPercent, 'f', -1, 64) +
		`,"resetsAt":"` + resetsAt.UTC().Format(time.RFC3339) +
		`","sampledAt":"` + sampledAt.UTC().Format(time.RFC3339) + `"}`
}

func newClaudeCapacityTimeoutFixture(t *testing.T, usedPercent float64, resetIn, sampledAgo time.Duration) claudeCapacityTimeoutFixture {
	t.Helper()
	// The wrapper must exist on PATH for tuple resolution; preflight never
	// starts it, and every refusal case below proves it stayed unstarted.
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	withPATH(t, fakeBinaryPath(t, "claude-minimax", argvLog, 0))

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "registries"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `version: 1
purpose: fixture
capability_classes: [build-implement]
models:
  fixture-minimax:
    lane: fixture minimax lane
    harness: claude_minimax
    provider_route: minimax
    tier: cheap
    packet_ceiling: multi-file
    aliases: [claude-minimax]
    capacity_source: {kind: codexbar, freshness_minutes: 15, ref: "codexbar://provider/minimax"}
    capabilities:
      build-implement: {level: capable, evidence: fixture}
routing_defaults:
  build-implement: [fixture-minimax]
`
	if err := os.WriteFile(filepath.Join(home, "registries", "model-capabilities.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	usagePath := filepath.Join(userHome, "Library", "Application Support", "CodexBar", "usage-history.jsonl")
	if err := os.MkdirAll(filepath.Dir(usagePath), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	fixture := claudeCapacityTimeoutFixture{home: home, userHome: userHome, usagePath: usagePath, now: now}
	fixture.writeRows(t, usedPercent, resetIn, sampledAgo)
	return fixture
}

func (f claudeCapacityTimeoutFixture) writeRows(t *testing.T, usedPercent float64, resetIn, sampledAgo time.Duration) {
	t.Helper()
	writeClaudeCapacityRows(t, f.usagePath, claudeCapacityRow(usedPercent, f.now.Add(resetIn), f.now.Add(-sampledAgo)))
}

func (f claudeCapacityTimeoutFixture) preflight(t *testing.T, timeout time.Duration) (RunInput, error) {
	t.Helper()
	registry := NewAdapterRegistry(runattempt.Open(f.home))
	adapter, ok := registry.AdapterByName("claude_minimax")
	if !ok {
		t.Fatal("claude_minimax adapter is not enrolled")
	}
	preflight, ok := adapter.(PreflightAdapter)
	if !ok {
		t.Fatal("claude_minimax adapter has no preflight seam")
	}
	return preflight.Preflight(context.Background(), RunInput{
		WorkID:  "work_capacity_timeout",
		Prompt:  "bounded run",
		Timeout: timeout,
		Now:     f.now,
	})
}

func (f claudeCapacityTimeoutFixture) revalidate(t *testing.T, timeout time.Duration) (RunInput, error) {
	t.Helper()
	registry := NewAdapterRegistry(runattempt.Open(f.home))
	adapter, ok := registry.AdapterByName("claude_minimax")
	if !ok {
		t.Fatal("claude_minimax adapter is not enrolled")
	}
	revalidator, ok := adapter.(GovernedPermissionAdapter)
	if !ok {
		t.Fatal("claude_minimax adapter cannot revalidate")
	}
	return revalidator.RevalidateGovernedRun(context.Background(), RunInput{
		WorkID:  "work_capacity_timeout",
		Prompt:  "bounded run",
		Timeout: timeout,
		Now:     f.now,
	})
}

func (f claudeCapacityTimeoutFixture) assertNoAttemptMinted(t *testing.T) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.home, "run_attempts", "ratt_*.json"))
	if len(files) != 0 {
		t.Fatalf("capacity decision minted RunAttempts: %v", files)
	}
}

// TestClaudeLaneExpectedMinutesRounding pins the duration->minutes contract:
// ceil on integer quotient+remainder, minimum one minute, exact multiples
// exact, and 60 preserved for zero/negative (uncapped) values. The max
// representable duration must survive without overflow.
func TestClaudeLaneExpectedMinutesRounding(t *testing.T) {
	maxMinutes := int(math.MaxInt64 / int64(time.Minute)) // 153722867
	cases := []struct {
		name    string
		timeout time.Duration
		want    int
	}{
		{"zero_uncapped", 0, 60},
		{"negative_uncapped", -time.Minute, 60},
		{"sub_minute_minimum_one", time.Nanosecond, 1},
		{"thirty_seconds", 30 * time.Second, 1},
		{"exact_minute_stays_exact", time.Minute, 1},
		{"sixty_seconds", 60 * time.Second, 1},
		{"sixty_one_seconds", 61 * time.Second, 2},
		{"ninety_seconds", 90 * time.Second, 2},
		{"eight_minutes", 8 * time.Minute, 8},
		{"sub_minute_remainder_rounds_up", 7*time.Minute + 59*time.Second + 999*time.Millisecond, 8},
		{"exact_hour_stays_exact", time.Hour, 60},
		{"max_representable_no_overflow", time.Duration(maxMinutes) * time.Minute, maxMinutes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeLaneExpectedMinutes(tc.timeout); got != tc.want {
				t.Fatalf("claudeLaneExpectedMinutes(%s) = %d, want %d", tc.timeout, got, tc.want)
			}
		})
	}
}

// TestClaudeLanePreflightAdmitsBoundedRunWithFreshCapacity is the core
// regression: an 8-minute bounded run must pass the declared-capacity
// preflight when the lane is fresh and low-usage but its quota resets 40
// minutes out. The prior literal 60-minute expectation refused exactly this
// run ("capacity expiring") even though the quota comfortably outlives the
// run. Refusals that would matter (stale/exhausted/unknown) are pinned
// separately; admission here mints no RunAttempt and starts no child.
func TestClaudeLanePreflightAdmitsBoundedRunWithFreshCapacity(t *testing.T) {
	fixture := newClaudeCapacityTimeoutFixture(t, 5, 40*time.Minute, 2*time.Minute)
	resolved, err := fixture.preflight(t, 8*time.Minute)
	if err != nil {
		t.Fatalf("bounded 8m preflight on fresh capacity: %v", err)
	}
	if filepath.Base(resolved.Command) != "claude-minimax" {
		t.Fatalf("resolved command = %q, want the tuple alias", resolved.Command)
	}
	if resolved.RequestedProviderRoute != "minimax" {
		t.Fatalf("resolved provider route = %q, want minimax", resolved.RequestedProviderRoute)
	}
	fixture.assertNoAttemptMinted(t)

	// The uncapped (zero-timeout) horizon of 60 minutes still refuses the
	// same 40-minute reset: the bounded duration, not a blanket relaxation,
	// is what admits the run above.
	if _, err := fixture.preflight(t, 0); err == nil || !strings.Contains(err.Error(), "capacity expiring") {
		t.Fatalf("uncapped preflight on a 40m reset = %v, want capacity expiring refusal (60m horizon preserved)", err)
	}
	// 60-minute horizon boundaries stay strict for uncapped runs: a reset
	// 60m30s away admits, exactly 60m away does not.
	sixty := newClaudeCapacityTimeoutFixture(t, 5, 60*time.Minute+30*time.Second, 2*time.Minute)
	if _, err := sixty.preflight(t, 0); err != nil {
		t.Fatalf("uncapped preflight with reset 60m30s away: %v", err)
	}
	exactlySixty := newClaudeCapacityTimeoutFixture(t, 5, 60*time.Minute, 2*time.Minute)
	if _, err := exactlySixty.preflight(t, 0); err == nil || !strings.Contains(err.Error(), "capacity expiring") {
		t.Fatalf("uncapped preflight with reset exactly 60m away = %v, want strict expiring refusal", err)
	}
}

// TestClaudeLanePreflightRefusesBoundedRunCrossingOrReachingReset pins the
// quota side of the fix: the rounded duration is a real bound, so a run
// whose quota resets before or exactly when the run would end is refused
// with the unchanged expiring semantics (strict reset comparison).
func TestClaudeLanePreflightRefusesBoundedRunCrossingOrReachingReset(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		resetIn time.Duration
		usedPct float64
		wantErr string
	}{
		{"eight_minute_bound_reaches_reset_exactly", 8 * time.Minute, 8 * time.Minute, 5, "capacity expiring"},
		{"eight_minute_bound_crosses_reset", 8 * time.Minute, 7 * time.Minute, 5, "capacity expiring"},
		{"ninety_seconds_round_to_two_strict_at_two", 90 * time.Second, 2 * time.Minute, 5, "capacity expiring"},
		{"ninety_seconds_admit_beyond_two", 90 * time.Second, 2*time.Minute + 30*time.Second, 5, ""},
		{"one_minute_minimum_bound", 30 * time.Second, 30 * time.Second, 5, "capacity expiring"},
		{"exhausted_wall_not_weakened_by_bound", 8 * time.Minute, 40 * time.Minute, 100, "capacity walled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClaudeCapacityTimeoutFixture(t, tc.usedPct, tc.resetIn, 2*time.Minute)
			_, err := fixture.preflight(t, tc.timeout)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("bounded preflight: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("bounded preflight error = %v, want %q", err, tc.wantErr)
			}
			fixture.assertNoAttemptMinted(t)
		})
	}
}

// TestClaudeLanePreflightStaleCapacityStillRefuses pins that freshness is
// untouched by the duration fix: a wired-but-stale declared source stays a
// refusal (capacity unknown), bounded or not.
func TestClaudeLanePreflightStaleCapacityStillRefuses(t *testing.T) {
	for _, timeout := range []time.Duration{8 * time.Minute, 0} {
		fixture := newClaudeCapacityTimeoutFixture(t, 5, 40*time.Minute, 2*time.Hour)
		if _, err := fixture.preflight(t, timeout); err == nil || !strings.Contains(err.Error(), "capacity unknown") {
			t.Fatalf("stale declared capacity (timeout=%s) = %v, want capacity unknown refusal", timeout, err)
		}
		fixture.assertNoAttemptMinted(t)
	}
}

// TestClaudeLaneRevalidationUsesTimeoutAndRejectsChangedEvidence proves the
// governed start-boundary resolver (RevalidateGovernedRun -> resolveRunInput)
// applies the same rounded-duration budget: a bounded revalidation admits the
// fresh 40m-reset lane the old 60-minute literal refused, an uncapped
// revalidation still refuses it, and any evidence change (exhausted, reset
// moved inside the bound, stale feed) is rejected against the bound. No
// RunAttempt is minted and no refusal evidence is recorded by revalidation.
func TestClaudeLaneRevalidationUsesTimeoutAndRejectsChangedEvidence(t *testing.T) {
	fixture := newClaudeCapacityTimeoutFixture(t, 5, 40*time.Minute, 2*time.Minute)

	if _, err := fixture.revalidate(t, 8*time.Minute); err != nil {
		t.Fatalf("bounded revalidation on fresh capacity: %v", err)
	}
	if _, err := fixture.revalidate(t, 0); err == nil || !strings.Contains(err.Error(), "capacity expiring") {
		t.Fatalf("uncapped revalidation on a 40m reset = %v, want capacity expiring (60m horizon preserved)", err)
	}

	fixture.writeRows(t, 100, 40*time.Minute, 2*time.Minute)
	if _, err := fixture.revalidate(t, 8*time.Minute); err == nil || !strings.Contains(err.Error(), "capacity walled") {
		t.Fatalf("revalidation after exhaustion = %v, want capacity walled", err)
	}

	fixture.writeRows(t, 5, 6*time.Minute, 2*time.Minute)
	if _, err := fixture.revalidate(t, 8*time.Minute); err == nil || !strings.Contains(err.Error(), "capacity expiring") {
		t.Fatalf("revalidation with reset inside the bound = %v, want capacity expiring", err)
	}

	fixture.writeRows(t, 5, 40*time.Minute, 2*time.Hour)
	if _, err := fixture.revalidate(t, 8*time.Minute); err == nil || !strings.Contains(err.Error(), "capacity unknown") {
		t.Fatalf("revalidation with stale feed = %v, want capacity unknown", err)
	}
	fixture.assertNoAttemptMinted(t)
}

func TestClaudeLaneCapacityRejectsUnrepresentableRoundedHorizon(t *testing.T) {
	fixture := newClaudeCapacityTimeoutFixture(t, 5, 40*time.Minute, 2*time.Minute)
	for _, check := range []struct {
		name string
		run  func(*testing.T, time.Duration) (RunInput, error)
	}{
		{"preflight", fixture.preflight}, {"revalidation", fixture.revalidate},
	} {
		t.Run(check.name, func(t *testing.T) {
			if _, err := check.run(t, time.Duration(math.MaxInt64)); err == nil || !strings.Contains(err.Error(), "capacity timeout exceeds supported horizon") {
				t.Fatalf("unrepresentable horizon error=%v", err)
			}
			fixture.assertNoAttemptMinted(t)
		})
	}
}
