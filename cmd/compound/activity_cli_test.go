package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/activity"
	"compound/internal/activity/tier1"
	"compound/internal/policy"
)

// F2 at the CLI door: a fresh home (sensor disabled, no grant) must refuse `activity drain` with a
// non-zero exit, an honest reason, and ZERO rows under observed/activity/.
func TestActivityCLIDrainFailsClosedOnFreshHome(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	spool := t.TempDir()
	if err := os.WriteFile(filepath.Join(spool, "2026-07-15.ndjson"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("seed spool: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runActivity([]string{"drain", "--spool", spool, "--home", homePath}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("drain on a fresh home must exit non-zero, got 0 (stdout=%q)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "refused") {
		t.Fatalf("refusal must carry an honest reason, got %q", stderr.String())
	}
	if entries, err := os.ReadDir(activity.SegmentsRoot(homePath)); err == nil && len(entries) > 0 {
		t.Fatalf("fail-closed drain must write zero segment rows, found %d entries", len(entries))
	}
}

// The status verb is ungated read-only truth: it must report enabled=false on a fresh home and
// exit zero.
func TestActivityCLIStatusReportsDisabled(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	var stdout, stderr bytes.Buffer
	if code := runActivity([]string{"status", "--home", homePath}, &stdout, &stderr); code != 0 {
		t.Fatalf("status must exit zero, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "enabled=false") {
		t.Fatalf("fresh home must report enabled=false, got %q", stdout.String())
	}
}

// An unknown verb or bare call prints usage and exits 2 — the operator door never guesses.
func TestActivityCLIUnknownVerbPrintsUsage(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runActivity([]string{"bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown verb must exit 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("unknown verb must print usage, got %q", stderr.String())
	}
}

// aw materialize accepts --start/--end, so a malformed value must NOT be
// silently swallowed (the bug being fixed): it must exit 2 with a clear reason.
func TestActivityCLIAWMaterializeInvalidStartExits2(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	var stdout, stderr bytes.Buffer
	code := runActivity([]string{"aw", "materialize", "--start", "not-a-time", "--home", homePath}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("invalid --start must exit 2, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid --start") {
		t.Fatalf("error must name --start, got %q", stderr.String())
	}
}

// A window where --start is not before --end is a usage error, exit 2.
func TestActivityCLIAWMaterializeInvertedWindowExits2(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	var stdout, stderr bytes.Buffer
	code := runActivity([]string{"aw", "materialize",
		"--start", "2026-07-31T00:00:00Z",
		"--end", "2026-07-30T00:00:00Z",
		"--home", homePath}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("inverted window must exit 2, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "must be before") {
		t.Fatalf("error must explain the inverted window, got %q", stderr.String())
	}
}

// Direct unit coverage of the window resolver: defaults, explicit bounds, and
// the "--flag=value" form (which valueFlag alone would silently drop).
func TestParseAWMaterializeWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer

	// Default: trailing 7 days ending now.
	start, end, ok := parseAWMaterializeWindow(nil, now, &stderr)
	if !ok {
		t.Fatalf("default window must parse, stderr=%q", stderr.String())
	}
	if !end.Equal(now) || !start.Equal(now.Add(-7*24*time.Hour)) {
		t.Fatalf("default window = [%s, %s], want trailing 7d", start, end)
	}

	// Explicit space-form bounds.
	start, end, ok = parseAWMaterializeWindow([]string{
		"--start", "2026-07-30T00:00:00Z",
		"--end", "2026-07-31T00:00:00Z",
	}, now, &stderr)
	if !ok {
		t.Fatalf("explicit window must parse, stderr=%q", stderr.String())
	}
	if start.Format(time.RFC3339) != "2026-07-30T00:00:00Z" || end.Format(time.RFC3339) != "2026-07-31T00:00:00Z" {
		t.Fatalf("explicit window = [%s, %s]", start, end)
	}

	// Equals-form bound must be honored, not dropped.
	start, _, ok = parseAWMaterializeWindow([]string{"--start=2026-07-29T00:00:00Z"}, now, &stderr)
	if !ok {
		t.Fatalf("equals-form window must parse, stderr=%q", stderr.String())
	}
	if start.Format(time.RFC3339) != "2026-07-29T00:00:00Z" {
		t.Fatalf("equals-form start = %s, want 2026-07-29T00:00:00Z", start)
	}

	// Malformed end is rejected.
	if _, _, ok = parseAWMaterializeWindow([]string{"--end", "nope"}, now, &stderr); ok {
		t.Fatalf("malformed --end must not parse")
	}
}

// enableAWMaterializeHome seeds a home whose sensor is enabled and whose grant
// registry carries the toggle + drain capabilities, so `aw materialize` passes
// its governance gates and actually reaches the AW read.
func enableAWMaterializeHome(t *testing.T, homePath string, now time.Time) {
	t.Helper()
	grants := `{"schema_version":"compound.standing_grants_source.v0.1","authority":{"decision_ref":"dec_activity_cli_test_grant_authority","reviewed_by":"operator:activity-cli-test"},"standing_grants":[` +
		`{"id":"grant.activity_toggle","capability":"` + activity.ToggleCapability + `","mutation_class":"external_side_effect","reversibility_ceiling":"reversible"},` +
		`{"id":"grant.activity_drain","capability":"` + activity.DrainCapability + `","mutation_class":"external_side_effect","reversibility_ceiling":"reversible"}` +
		`]}`
	srcPath := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(srcPath, []byte(grants), 0o600); err != nil {
		t.Fatalf("write grants: %v", err)
	}
	if _, err := policy.ImportStandingGrants(homePath, srcPath, now); err != nil {
		t.Fatalf("import grants: %v", err)
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := activity.SetEnabled(homePath, cfg, "work_enable", true, now); err != nil {
		t.Fatalf("enable sensor: %v", err)
	}
}

func enableTier1CollectHome(t *testing.T, homePath string, now time.Time) {
	t.Helper()
	grants := `{"schema_version":"compound.standing_grants_source.v0.1","authority":{"decision_ref":"dec_activity_cli_test_grant_authority","reviewed_by":"operator:activity-cli-test"},"standing_grants":[` +
		`{"id":"grant.activity_toggle","capability":"` + activity.ToggleCapability + `","mutation_class":"external_side_effect","reversibility_ceiling":"reversible"},` +
		`{"id":"grant.activity_tier1_collect","capability":"` + activity.Tier1CollectCapability + `","mutation_class":"external_side_effect","reversibility_ceiling":"reversible"}` +
		`]}`
	srcPath := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(srcPath, []byte(grants), 0o600); err != nil {
		t.Fatalf("write grants: %v", err)
	}
	if _, err := policy.ImportStandingGrants(homePath, srcPath, now); err != nil {
		t.Fatalf("import grants: %v", err)
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := activity.SetEnabled(homePath, cfg, "work_enable", true, now); err != nil {
		t.Fatalf("enable sensor: %v", err)
	}
}

// The user-visible contract: when AW is unreachable, `aw materialize` must exit
// nonzero and name the failure — it must NOT print a success-looking line. This
// is the exact regression the audit flagged (AW outage rendered as exit 0).
func TestActivityCLIAWMaterializeAWDownExitsNonZero(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	enableAWMaterializeHome(t, homePath, cliTestNow)

	// AW server that errors on every request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	restore := tier1.OverrideAWBaseURLForTest(srv.URL)
	defer restore()

	// Human mode: nonzero exit, stderr names the failure.
	var stdout, stderr bytes.Buffer
	code := runActivity([]string{"aw", "materialize", "--home", homePath}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("AW down must exit nonzero, got 0 (stdout=%q)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "failed") {
		t.Fatalf("stderr must name the failure, got %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "aw materialize: events_written") {
		t.Fatalf("must not print a success line on failure, got %q", stdout.String())
	}

	// JSON mode: exit 1 with failed=true.
	stdout.Reset()
	stderr.Reset()
	code = runActivity([]string{"aw", "materialize", "--home", homePath, "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("JSON AW-down must exit 1, got %d", code)
	}
	if !strings.Contains(stdout.String(), `"failed": true`) {
		t.Fatalf("JSON must carry failed=true, got %q", stdout.String())
	}
}

func TestActivityCLITier1CollectAWDownExitsNonZero(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	enableTier1CollectHome(t, homePath, cliTestNow)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	restore := tier1.OverrideCollectorAWBaseURLForTest(srv.URL)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runActivity([]string{"tier1", "collect", "--home", homePath, "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("JSON Tier-1 AW-down must exit 1, got %d (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"failed": true`) {
		t.Fatalf("JSON must carry failed=true, got %q", stdout.String())
	}
}

func TestActivityCLITier1CollectAWDownHumanOutputIsFailure(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	enableTier1CollectHome(t, homePath, cliTestNow)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	restore := tier1.OverrideCollectorAWBaseURLForTest(srv.URL)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runActivity([]string{"tier1", "collect", "--home", homePath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("human Tier-1 AW-down must exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "tier1 collect failed") {
		t.Fatalf("stderr must name the operational failure, got %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "tier1 collect: events_written") {
		t.Fatalf("failed run must not print a success line, got %q", stdout.String())
	}
}

func TestActivityCLITier1CollectPartialBucketFailureStaysSuccessfulAndVisible(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	enableTier1CollectHome(t, homePath, cliTestNow)

	const buckets = `{
		"aw-watcher-window_bad":{"id":"aw-watcher-window_bad","type":"currentwindow","hostname":"h"},
		"aw-watcher-window_ok":{"id":"aw-watcher-window_ok","type":"currentwindow","hostname":"h"}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "window_bad"):
			http.Error(w, "down", http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "/events"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(buckets))
		}
	}))
	defer srv.Close()
	restore := tier1.OverrideCollectorAWBaseURLForTest(srv.URL)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runActivity([]string{"tier1", "collect", "--home", homePath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("partial bucket failure must remain successful, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "buckets_failed=1") {
		t.Fatalf("human output must expose partial degradation, got %q", stdout.String())
	}
}
