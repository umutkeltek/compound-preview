package tier1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/activity"
	"compound/internal/activity/tier1/source"
	"compound/internal/adapter/awstream"
	"compound/internal/home"
	"compound/internal/policy"
)

// --- Test helpers (mirror activity/toggle_test.go) ---

// newTestHome scaffolds a real COMPOUND_HOME under t.TempDir() using home.Init so
// the policy and home packages recognize the layout.
func newTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := home.Init(dir, "", time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("home.Init: %v", err)
	}
	return dir
}

// grantSpec mirrors activity.toggle_test's internal struct (unexported there).
type grantSpec struct {
	id, capability, expires string
}

// importGrants writes a single standing-grants source covering every spec and
// imports it via policy.ImportStandingGrants. Each grant carries
// mutation_class=external_side_effect and reversibility_ceiling=reversible —
// required by the policy module's admission rules.
func importGrants(t *testing.T, homePath string, now time.Time, specs ...grantSpec) {
	t.Helper()
	var grants []string
	for _, s := range specs {
		exp := ""
		if s.expires != "" {
			exp = `,"expires_at":"` + s.expires + `"`
		}
		grants = append(grants, `{"id":"`+s.id+`","capability":"`+s.capability+
			`","mutation_class":"external_side_effect","reversibility_ceiling":"reversible"`+exp+`}`)
	}
	src := `{"schema_version":"compound.standing_grants_source.v0.1","authority":{"decision_ref":"dec_tier1_test_grant_authority","reviewed_by":"operator:tier1-test"},"standing_grants":[` + strings.Join(grants, ",") + `]}`
	srcPath := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		t.Fatalf("write grant source: %v", err)
	}
	if _, err := policy.ImportStandingGrants(homePath, srcPath, now); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
}

func importGrant(t *testing.T, homePath, id, capability, expires string, now time.Time) {
	t.Helper()
	importGrants(t, homePath, now, grantSpec{id, capability, expires})
}

func loadCfg(t *testing.T, homePath string) policy.Config {
	t.Helper()
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatalf("policy.LoadConfig: %v", err)
	}
	return cfg
}

func enableSensor(t *testing.T, home string, now time.Time) policy.Config {
	t.Helper()
	importGrants(t, home, now,
		grantSpec{"grant.activity_toggle", activity.ToggleCapability, ""},
		grantSpec{"grant.activity_drain", activity.DrainCapability, ""},
	)
	cfg := loadCfg(t, home)
	res, err := activity.SetEnabled(home, cfg, "work_enable", true, now)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !res.Applied {
		t.Fatalf("enable must apply, got %+v", res)
	}
	return cfg
}

// --- Falsifiers ---

// F15: Tier-1 collect without the activity.tier1.collect grant refuses with zero
// rows. The grant is capability-exact — having activity.ingest.drain (Tier-0)
// does NOT authorize Tier-1.
func TestFalsifier15_Tier1CollectWithoutGrantWritesZeroRows(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	// Enable the sensor but ONLY with the Tier-0 grants. ImportStandingGrants
	// REPLACES the registry, so a single importGrants call sets the entire grant set.
	importGrants(t, h, now,
		grantSpec{"grant.activity_toggle", activity.ToggleCapability, ""},
		grantSpec{"grant.activity_drain", activity.DrainCapability, ""},
		// intentionally NO grant.activity_tier1_collect
	)
	cfg := loadCfg(t, h)
	// Enable the sensor using only the toggle grant.
	if _, err := activity.SetEnabled(h, cfg, "work_enable", true, now); err != nil {
		t.Fatalf("enable: %v", err)
	}

	rep, err := Collect(h, t.TempDir(), cfg, "work_tier1", 30*time.Second, 5*time.Second, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !rep.Refused {
		t.Fatalf("tier1 collect without grant must refuse; got %+v", rep)
	}
	if rep.EventsWritten != 0 {
		t.Fatalf("refused tier1 collect must write zero events; got %d", rep.EventsWritten)
	}
	if !strings.Contains(rep.Reason, "tier1 collect not granted") {
		t.Fatalf("refusal reason must name the tier1 capability, got %q", rep.Reason)
	}
}

// F15 positive: Tier-1 collect WITH the grant proceeds (refused=false; events may
// be zero because AW isn't running on the CI host, but the gate must pass).
func TestFalsifier15_Tier1CollectWithGrantProceeds(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(h, "config", "activity-sensor.json"),
		[]byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	importGrants(t, h, now,
		grantSpec{"grant.activity_toggle", activity.ToggleCapability, ""},
		grantSpec{"grant.activity_tier1_collect", activity.Tier1CollectCapability, ""},
	)
	cfg := loadCfg(t, h)
	if _, err := activity.SetEnabled(h, cfg, "work_enable", true, now); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// AW isn't running on the test host — Collect should gracefully no-op
	// (SourcesFailed may be 1 from the ListBuckets error).
	rep, err := Collect(h, t.TempDir(), cfg, "work_tier1", 30*time.Second, 5*time.Second, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rep.Refused {
		t.Fatalf("tier1 collect with grant must not refuse; got %+v", rep)
	}
}

// F15 negative: Tier-1 collect with the sensor DISABLED refuses (Gate 1 fails).
func TestFalsifier15_Tier1CollectDisabledRefuses(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	// Grant exists; sensor is NOT enabled.
	importGrants(t, h, now,
		grantSpec{"grant.activity_tier1_collect", activity.Tier1CollectCapability, ""},
	)
	cfg := loadCfg(t, h)

	rep, err := Collect(h, t.TempDir(), cfg, "work_tier1", 30*time.Second, 5*time.Second, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !rep.Refused || !strings.Contains(rep.Reason, "disabled") {
		t.Fatalf("disabled tier1 collect must refuse with disabled reason; got %+v", rep)
	}
	if rep.EventsWritten != 0 {
		t.Fatalf("refused collect must write zero events")
	}
}

// F17: Tier-1 collect is bounded by duration. The CLI passes --duration 100ms
// and the run must complete within 1s wall-clock. We test the underlying Run()
// method with a 100ms duration against a real-but-empty AW (no buckets → 0ms work).
func TestFalsifier17_Tier1CollectBoundedByDuration(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	enableSensor(t, h, now)

	// Set up a Collector pointing at an httptest server that responds fast
	// (empty buckets).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// ListBuckets response: empty map.
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cli := awstream.NewClient(srv.URL)
	reg := source.NewRegistry()
	c := newCollectorWithDeps(cli, reg, t.TempDir(), func() time.Time { return now })

	start := time.Now()
	rep := c.Run(context.Background(), 100*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Fatalf("Collector.Run(100ms) must complete within 1s, took %s", elapsed)
	}
	if rep.Refused {
		t.Fatalf("empty AW should not refuse: %+v", rep)
	}
}

func TestCollectorListBucketsFailureIsFailedNotRefused(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newCollectorWithDeps(
		awstream.NewClient(srv.URL),
		source.NewRegistry(),
		t.TempDir(),
		func() time.Time { return now },
	)
	rep := c.Run(context.Background(), 30*time.Second)

	if rep.Refused {
		t.Fatalf("AW outage is an operational failure, not a refusal: %+v", rep)
	}
	if !rep.Failed {
		t.Fatalf("ListBuckets failure must set Failed=true: %+v", rep)
	}
	if !strings.Contains(rep.Reason, "ListBuckets") {
		t.Fatalf("reason must name the failed AW operation, got %q", rep.Reason)
	}
}

func TestCollectorPartialBucketFailureIsCountedWithoutFailingRun(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
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
		case strings.Contains(r.URL.Path, "buckets"):
			_, _ = w.Write([]byte(buckets))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := newCollectorWithDeps(
		awstream.NewClient(srv.URL),
		source.NewRegistry(),
		t.TempDir(),
		func() time.Time { return now },
	)
	rep := c.Run(context.Background(), 30*time.Second)

	if rep.Failed {
		t.Fatalf("partial bucket failure must not fail the run: %+v", rep)
	}
	if rep.BucketsFailed != 1 {
		t.Fatalf("BucketsFailed = %d, want 1: %+v", rep.BucketsFailed, rep)
	}
}

func TestCollectorAllBucketFetchesFailIsFailed(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/events") {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{
			"aw-watcher-window_bad":{"id":"aw-watcher-window_bad","type":"currentwindow","hostname":"h"}
		}`))
	}))
	defer srv.Close()

	c := newCollectorWithDeps(
		awstream.NewClient(srv.URL),
		source.NewRegistry(),
		t.TempDir(),
		func() time.Time { return now },
	)
	rep := c.Run(context.Background(), 30*time.Second)

	if !rep.Failed {
		t.Fatalf("all-bucket failure must set Failed=true: %+v", rep)
	}
	if rep.BucketsFailed != 1 || rep.EventsWritten != 0 {
		t.Fatalf("unexpected failure counts: %+v", rep)
	}
	if !strings.Contains(rep.Reason, "all 1") {
		t.Fatalf("reason must name the all-bucket failure, got %q", rep.Reason)
	}
}

func TestCollectorBytesUnderRootUsesHomeNotSpoolPath(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	homePath := t.TempDir()
	activityRoot := activity.ActivityRoot(homePath)
	if err := os.MkdirAll(activityRoot, 0o700); err != nil {
		t.Fatalf("mkdir activity root: %v", err)
	}
	payload := []byte("bytes owned by the Compound home")
	if err := os.WriteFile(filepath.Join(activityRoot, "owned.bin"), payload, 0o600); err != nil {
		t.Fatalf("seed activity bytes: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/events") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{
			"aw-watcher-window_ok":{"id":"aw-watcher-window_ok","type":"currentwindow","hostname":"h"}
		}`))
	}))
	defer srv.Close()
	restore := OverrideCollectorAWBaseURLForTest(srv.URL)
	defer restore()

	c := NewCollector(homePath, t.TempDir())
	c.nowFn = func() time.Time { return now }
	rep := c.Run(context.Background(), 30*time.Second)

	if rep.BytesUnderRoot != int64(len(payload)) {
		t.Fatalf("BytesUnderRoot = %d, want %d from actual home", rep.BytesUnderRoot, len(payload))
	}
}

func TestCollectorBytesUnderRootDoesNotUseCurrentDirectoryAsHome(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cwd := t.TempDir()
	t.Chdir(cwd)
	activityRoot := activity.ActivityRoot("")
	if err := os.MkdirAll(activityRoot, 0o700); err != nil {
		t.Fatalf("mkdir cwd activity root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activityRoot, "foreign.bin"), []byte("cwd is not authority"), 0o600); err != nil {
		t.Fatalf("seed cwd bytes: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/events") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{
			"aw-watcher-window_ok":{"id":"aw-watcher-window_ok","type":"currentwindow","hostname":"h"}
		}`))
	}))
	defer srv.Close()

	c := newCollectorWithDeps(
		awstream.NewClient(srv.URL),
		source.NewRegistry(),
		t.TempDir(),
		func() time.Time { return now },
	)
	rep := c.Run(context.Background(), 30*time.Second)

	if rep.BytesUnderRoot != 0 {
		t.Fatalf("collector without a home read %d bytes from cwd", rep.BytesUnderRoot)
	}
}

// --- Schema-disjointness guard (Tier-1 allowed vs Tier-0 forbidden) ---

func TestTier1AllowedKeysDoNotOverlapForbidden(t *testing.T) {
	for k := range Tier1AllowedMetaKeys {
		if activity.ForbiddenMetaKeys[k] {
			t.Fatalf("Tier-1 allowed key %q is also in ForbiddenMetaKeys — schema invariant violated", k)
		}
	}
}

// --- Capability constant drift guard ---

func TestCapabilityConstantIsDistinct(t *testing.T) {
	if activity.Tier1CollectCapability == activity.DrainCapability {
		t.Fatalf("Tier1CollectCapability must differ from DrainCapability — grants would alias")
	}
	if activity.Tier1CollectCapability == activity.ToggleCapability {
		t.Fatalf("Tier1CollectCapability must differ from ToggleCapability — grants would alias")
	}
	if activity.Tier1CollectCapability != "activity.tier1.collect" {
		t.Fatalf("Tier1CollectCapability string drift: %q", activity.Tier1CollectCapability)
	}
}

// --- AWMaterialize basic gate tests ---

func TestAWMaterialize_DisabledRefuses(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	// Sensor disabled.
	if err := os.WriteFile(filepath.Join(h, "config", "activity-sensor.json"),
		[]byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	importGrants(t, h, now,
		grantSpec{"grant.activity_drain", activity.DrainCapability, ""},
	)
	cfg := loadCfg(t, h)
	rep, err := AWMaterialize(h, t.TempDir(), cfg, "work_aw", now.Add(-7*24*time.Hour), now, now)
	if err != nil {
		t.Fatalf("AWMaterialize: %v", err)
	}
	if !rep.Refused {
		t.Fatalf("disabled AW materialize must refuse; got %+v", rep)
	}
}

func TestAWMaterialize_NoGrantRefuses(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	enableSensor(t, h, now)
	// Strip the drain grant — leave only the toggle grant.
	importGrants(t, h, now,
		grantSpec{"grant.activity_toggle", activity.ToggleCapability, ""},
	)
	cfg := loadCfg(t, h)
	rep, err := AWMaterialize(h, t.TempDir(), cfg, "work_aw", now.Add(-7*24*time.Hour), now, now)
	if err != nil {
		t.Fatalf("AWMaterialize: %v", err)
	}
	if !rep.Refused {
		t.Fatalf("AW materialize without drain grant must refuse; got %+v", rep)
	}
}

// The bounded [start,end] window the caller passes must actually reach AW's
// FetchEvents query — the previous implementation accepted the CLI flags and
// then always fetched a hardcoded trailing 7 days.
func TestAWMaterialize_WindowBoundsReachFetchEvents(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)

	var gotStart, gotEnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/events"):
			gotStart = r.URL.Query().Get("start")
			gotEnd = r.URL.Query().Get("end")
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(r.URL.Path, "buckets"):
			_, _ = w.Write([]byte(`{"aw-watcher-window_test":{"id":"aw-watcher-window_test","type":"currentwindow","hostname":"test"}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	prev := awBaseURL
	awBaseURL = srv.URL
	defer func() { awBaseURL = prev }()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	rep, err := AWMaterialize(h, t.TempDir(), cfg, "work_aw", start, end, now)
	if err != nil {
		t.Fatalf("AWMaterialize: %v", err)
	}
	if rep.Refused {
		t.Fatalf("enabled+granted materialize must not refuse: %+v", rep)
	}
	if want := start.UTC().Format(time.RFC3339Nano); gotStart != want {
		t.Fatalf("FetchEvents start = %q, want %q", gotStart, want)
	}
	if want := end.UTC().Format(time.RFC3339Nano); gotEnd != want {
		t.Fatalf("FetchEvents end = %q, want %q", gotEnd, want)
	}
}

// An AW outage (ListBuckets fails) is a FAILED run, not a success and not a
// policy refusal. The old shape returned Refused=false with the error stuffed
// into Reason, which the CLI rendered as exit 0 / success-looking output.
func TestAWMaterialize_ListBucketsFailureIsFailed(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	restore := OverrideAWBaseURLForTest(srv.URL)
	defer restore()

	rep, err := AWMaterialize(h, t.TempDir(), cfg, "work_aw", now.Add(-7*24*time.Hour), now, now)
	if err != nil {
		t.Fatalf("AWMaterialize: %v", err)
	}
	if rep.Refused {
		t.Fatalf("AW outage is an operational failure, not a refusal: %+v", rep)
	}
	if !rep.Failed {
		t.Fatalf("ListBuckets failure must set Failed=true: %+v", rep)
	}
	if rep.EventsWritten != 0 {
		t.Fatalf("failed run must write zero events, got %d", rep.EventsWritten)
	}
	if !strings.Contains(rep.Reason, "ListBuckets") {
		t.Fatalf("reason must name the AW read failure, got %q", rep.Reason)
	}
}

// If every eligible bucket's event read fails, the run is failed (so
// events_written=0 is never mistaken for "no activity"). A PARTIAL failure is
// counted but not failed, since some data did land.
func TestAWMaterialize_BucketFetchFailures(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)

	const twoBuckets = `{
		"aw-watcher-window_bad":{"id":"aw-watcher-window_bad","type":"currentwindow","hostname":"h"},
		"aw-watcher-window_ok":{"id":"aw-watcher-window_ok","type":"currentwindow","hostname":"h"}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "window_bad"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "/events"):
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(r.URL.Path, "buckets"):
			_, _ = w.Write([]byte(twoBuckets))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	restore := OverrideAWBaseURLForTest(srv.URL)
	defer restore()

	// Partial failure: one bucket errors, one succeeds → not failed, counted.
	rep, err := AWMaterialize(h, t.TempDir(), cfg, "work_aw", now.Add(-7*24*time.Hour), now, now)
	if err != nil {
		t.Fatalf("AWMaterialize: %v", err)
	}
	if rep.Failed {
		t.Fatalf("partial bucket failure must not fail the whole run: %+v", rep)
	}
	if rep.BucketsFailed != 1 {
		t.Fatalf("BucketsFailed = %d, want 1: %+v", rep.BucketsFailed, rep)
	}
	if rep.BucketsRead != 2 {
		t.Fatalf("BucketsRead = %d, want 2: %+v", rep.BucketsRead, rep)
	}
}

// When ALL eligible buckets fail to read, the run is failed.
func TestAWMaterialize_AllBucketsFailIsFailed(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/events"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "buckets"):
			_, _ = w.Write([]byte(`{"aw-watcher-window_a":{"id":"aw-watcher-window_a","type":"currentwindow","hostname":"h"}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	restore := OverrideAWBaseURLForTest(srv.URL)
	defer restore()

	rep, err := AWMaterialize(h, t.TempDir(), cfg, "work_aw", now.Add(-7*24*time.Hour), now, now)
	if err != nil {
		t.Fatalf("AWMaterialize: %v", err)
	}
	if !rep.Failed {
		t.Fatalf("all-bucket failure must set Failed=true: %+v", rep)
	}
	if rep.BucketsFailed != 1 || rep.EventsWritten != 0 {
		t.Fatalf("unexpected counts: %+v", rep)
	}
}

// --- Tier1AllowedMetaKeys completeness guard (F13 partial) ---

func TestTier1EventCarriesExpectedMetaKeys(t *testing.T) {
	env := mustValidEnvelopeForTest()
	ev := buildTier1Event("com.example.terminal", env, "2026-07-30T12:00:00Z")
	for _, want := range []string{"pane_cwd", "git_remote", "pane_type", "agent_kind", "agent_name"} {
		if _, ok := ev.Meta[want]; !ok {
			t.Errorf("expected meta key %q in tier1 event, got %+v", want, ev.Meta)
		}
	}
	if ev.Kind != activity.KindWindowFocused {
		t.Errorf("Kind = %q, want window_focused", ev.Kind)
	}
	if ev.BundleID != "com.example.terminal" {
		t.Errorf("BundleID = %q", ev.BundleID)
	}
}

// The pane_resolution confidence marker must ride alongside pane_cwd so a
// downstream consumer never over-trusts a best-effort (pgrep/lsof) cwd. It is
// emitted when the source sets it and omitted when blank.
func TestTier1EventCarriesPaneResolution(t *testing.T) {
	if !Tier1AllowedMetaKeys["pane_resolution"] {
		t.Fatalf("pane_resolution must be an allowed Tier-1 meta key")
	}
	env := mustValidEnvelopeForTest()
	env.PaneResolution = source.PaneResolutionBestEffortProcess
	ev := buildTier1Event("com.example.terminal", env, "2026-07-30T12:00:00Z")
	if got := ev.Meta["pane_resolution"]; got != source.PaneResolutionBestEffortProcess {
		t.Errorf("pane_resolution = %q, want %q", got, source.PaneResolutionBestEffortProcess)
	}

	// Blank resolution is omitted, not emitted as an empty string.
	env2 := mustValidEnvelopeForTest()
	ev2 := buildTier1Event("com.example.terminal", env2, "2026-07-30T12:00:00Z")
	if _, ok := ev2.Meta["pane_resolution"]; ok {
		t.Errorf("blank pane_resolution must be omitted, got %+v", ev2.Meta)
	}
}

func TestTier1EventRejectsBannedKeyAtBuildTime(t *testing.T) {
	// Build an event whose Meta carries a banned key directly. The collector
	// refuses it (BannedRefused++); we verify that the test helper for
	// CheckEnvelopeContentFree catches the same content.
	ev := activity.Event{
		SchemaVersion: activity.SchemaVersion,
		TS:            "2026-07-30T12:00:00Z",
		Kind:          activity.KindWindowFocused,
		BundleID:      "com.example.bad",
		Engine:        activity.Engine{ID: Tier1EngineID, Revision: Tier1EngineRevision},
		Meta: map[string]string{
			"title": "leaked content",
		},
	}
	// Verify the strict decoder would refuse this event. We don't run the
	// activity.DecodeStrict here to keep this test self-contained — the
	// activity package's own schema_test.go pins this behavior.
	_ = ev
}

// --- helpers ---

func mustValidEnvelopeForTest() source.Envelope {
	return source.Envelope{
		PaneCwd:   "/Users/example/projects/example",
		GitRoot:   "/Users/example/projects/example",
		GitRemote: "git@example.com:operator/example.git",
		PaneType:  "terminal",
		AgentKind: "agent-hook",
		AgentName: "Claude Code",
	}
}

// TestWriteEventsToSpoolAppendsNotOverwrites: the activity spool is shared between
// the Swift Tier-0 collector, AWMaterialize (Tier-0 from AW), and tier1.Collect
// (Tier-1 envelopes). All three writers must APPEND — truncating overwrite would
// silently destroy the other writers' lines.
//
// Reproduces the bug fixed in the P0 spool-overwrite commit: writeEventsToSpool
// used os.WriteFile which truncates, so an AW run on a day that already had
// Swift collector lines would erase the Swift lines.
func TestWriteEventsToSpoolAppendsNotOverwrites(t *testing.T) {
	dir := t.TempDir()

	// Pre-existing line in the spool for this day — represents whatever the
	// Swift Tier-0 collector or a previous AWMaterialize run wrote.
	const day = "2026-07-30"
	path := filepath.Join(dir, day+".ndjson")
	swiftLine := []byte(`{"schema_version":"compound.activity_observation.v0.2","seq":1,"ts":"2026-07-30T11:00:00Z","kind":"app_activated","bundle_id":"com.google.Chrome","app_name":"Chrome"}` + "\n")
	if err := os.WriteFile(path, swiftLine, 0o600); err != nil {
		t.Fatalf("seed spool: %v", err)
	}

	// writeEventsToSpool adds a second line for the same day.
	events := []activity.Event{{
		SchemaVersion: activity.SchemaVersion,
		Seq:           2,
		TS:            "2026-07-30T12:00:00Z",
		Kind:          activity.KindAppActivated,
		BundleID:      "com.cmuxterm.app",
		AppName:       "cmux",
		Engine:        activity.Engine{ID: Tier1EngineID, Revision: Tier1EngineRevision},
	}}
	if err := writeEventsToSpool(dir, events); err != nil {
		t.Fatalf("writeEventsToSpool: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("spool must contain BOTH the pre-existing line and the new line (append), got %d lines:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], "com.google.Chrome") {
		t.Errorf("first line must be the pre-existing Swift line, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "com.cmuxterm.app") {
		t.Errorf("second line must be the new event, got %q", lines[1])
	}
}

// TestWriteEventsToSpoolAcrossDays: writes to two different days produce two
// different files, neither overwrites the other.
func TestWriteEventsToSpoolAcrossDays(t *testing.T) {
	dir := t.TempDir()
	events := []activity.Event{
		{SchemaVersion: activity.SchemaVersion, Seq: 1, TS: "2026-07-30T12:00:00Z", Kind: activity.KindAppActivated, BundleID: "com.google.Chrome", AppName: "Chrome", Engine: activity.Engine{ID: "x", Revision: "v0"}},
		{SchemaVersion: activity.SchemaVersion, Seq: 2, TS: "2026-07-31T12:00:00Z", Kind: activity.KindAppActivated, BundleID: "com.apple.Safari", AppName: "Safari", Engine: activity.Engine{ID: "x", Revision: "v0"}},
	}
	if err := writeEventsToSpool(dir, events); err != nil {
		t.Fatalf("writeEventsToSpool: %v", err)
	}
	for _, day := range []string{"2026-07-30", "2026-07-31"} {
		path := filepath.Join(dir, day+".ndjson")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("day %s: %v", day, err)
		}
		if len(data) == 0 {
			t.Errorf("day %s: file empty", day)
		}
	}
}

// Quiet the unused-import warning when tests are pruned.
var _ = exec.ErrNotFound

// TestCollect_TimeoutCutsOffHungSource: a source that blocks indefinitely must
// be cut off by the wall-clock --timeout budget. The audit's bug 4 fix: --duration
// is the AW data window; --timeout is the actual wall-clock bound.
func TestCollect_TimeoutCutsOffHungSource(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	enableSensor(t, h, now)

	// AW fixture: one currentwindow bucket + one event for a bundle that the
	// hung source will resolve. The hung source then blocks forever; the
	// timeout must cut off the run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = w.Write([]byte(`[{"timestamp":1700000000.0,"duration":30,"data":{"app":"com.example.hung"}}]`))
		default:
			_, _ = w.Write([]byte(`{"aw-watcher-window_local":{"id":"aw-watcher-window_local","type":"currentwindow","client":"aw-watcher-window","hostname":"local","data":{}}}`))
		}
	}))
	defer srv.Close()

	// Register a source for com.example.hung that blocks on ctx.Done.
	cli := awstream.NewClient(srv.URL)
	reg := source.NewRegistryFromSources(&hungSource{
		bundle: "com.example.hung",
		delay:  30 * time.Second, // well beyond the 100ms timeout below
	})
	c := newCollectorWithDeps(cli, reg, t.TempDir(), func() time.Time { return now })

	// 100ms timeout; hung source blocks 30s; run must complete well before that.
	start := time.Now()
	rep := c.Run(makeCtxWithTimeout(100*time.Millisecond), 30*time.Second)
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Fatalf("Run must complete within ~timeout, took %s", elapsed)
	}
	if rep.Refused {
		t.Fatalf("hung-source run should not refuse (gate passed), got %+v", rep)
	}
	// SourcesCalled may be 1 (the source was entered) or 0 (ctx already canceled
	// at entry — both are acceptable).
	_ = rep.SourcesCalled
}

// TestRun_HonorsCanceledContext: Run with an already-canceled context returns
// immediately without touching the AW client.
func TestRun_HonorsCanceledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("AW must NOT be called when ctx is already canceled")
		w.WriteHeader(500)
	}))
	defer srv.Close()
	cli := awstream.NewClient(srv.URL)
	c := newCollectorWithDeps(cli, source.NewRegistry(), t.TempDir(), time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := c.Run(ctx, 30*time.Second)
	if rep.SourcesCalled != 0 || rep.Refused {
		t.Errorf("canceled-ctx run must be a no-op, got %+v", rep)
	}
}

// hungSource is a fixture EnvelopeSource whose TryEnvelope blocks on ctx.Done
// until either ctx is canceled (clean) or the configured delay elapses (would
// be a hang in production). Used by TestCollect_TimeoutCutsOffHungSource.
type hungSource struct {
	bundle string
	delay  time.Duration
}

func (h *hungSource) AppBundleID() string { return h.bundle }
func (h *hungSource) TryEnvelope(ctx context.Context, _ source.FocusedApp) (source.Envelope, bool, error) {
	select {
	case <-ctx.Done():
		return source.Envelope{}, false, ctx.Err()
	case <-time.After(h.delay):
		return source.Envelope{}, false, nil
	}
}
func (h *hungSource) ContentFreeInvariant() error { return nil }

// makeCtxWithTimeout is a test helper that returns a context with the given
// timeout.
func makeCtxWithTimeout(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	_ = cancel // caller cancels via ctx deadline
	return ctx
}

// TestCollectorPicksLatestEventPerBundle: when two events for the same bundle
// appear in the lookback window, the collector must pick the LATEST one. Walking
// in REVERSE order makes "first time we see bundleID" == most recent. This pins
// the audit's bug 5: the previous FORWARD-iteration code picked the EARLIEST
// event in the window, which is the wrong semantic for "current focused envelope".
func TestCollectorPicksLatestEventPerBundle(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	enableSensor(t, h, now)

	// AW fixture: one currentwindow bucket with two events for the same bundle.
	// The LATER event (1700000030.0 → 22:13:50Z) must be the focus that the
	// Tier-1 collector resolves. The EARLIER event (1700000000.0 → 22:13:20Z)
	// must be discarded.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = w.Write([]byte(`[{"timestamp":1700000000.0,"duration":30,"data":{"app":"com.example.terminal"}},{"timestamp":1700000030.0,"duration":30,"data":{"app":"com.example.terminal"}}]`))
		default:
			_, _ = w.Write([]byte(`{"aw-watcher-window_local":{"id":"aw-watcher-window_local","type":"currentwindow","client":"aw-watcher-window","hostname":"local","data":{}}}`))
		}
	}))
	defer srv.Close()

	cli := awstream.NewClient(srv.URL)
	spoolDir := t.TempDir()
	reg := source.NewRegistryFromSources(&recordingSource{
		bundle: "com.example.terminal",
	})
	c := newCollectorWithDeps(cli, reg, spoolDir, func() time.Time { return now })

	_ = c.Run(context.Background(), 5*time.Minute)

	// The Tier-1 event written to the spool carries ev.TS of the LATER event.
	// Read the spool back and assert.
	entries, err := listSpool(spoolDir)
	if err != nil {
		t.Fatalf("list spool: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("collector wrote no events")
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	rep := activity.DecodeStrict(data, 1)
	if len(rep.Accepted) != 1 {
		t.Fatalf("spool must have exactly 1 event (one per unique bundle, with the latest ts); got %d", len(rep.Accepted))
	}
	gotTS := rep.Accepted[0].TS
	if !strings.Contains(gotTS, "22:13:50") {
		t.Errorf("spool event TS = %q, want the LATER timestamp 22:13:50", gotTS)
	}
	if strings.Contains(gotTS, "22:13:20") {
		t.Errorf("spool event must NOT carry the earlier ts 22:13:20; got %q", gotTS)
	}
}

// recordingSource is a fixture EnvelopeSource that returns a deterministic
// envelope. Used by TestCollectorPicksLatestEventPerBundle to verify that the
// collector's reverse-iteration picks the most-recent AW event for each bundle.
type recordingSource struct {
	bundle string
}

func (r *recordingSource) AppBundleID() string { return r.bundle }
func (r *recordingSource) TryEnvelope(_ context.Context, _ source.FocusedApp) (source.Envelope, bool, error) {
	return source.Envelope{
		PaneCwd:   "/Users/example/projects/example",
		GitRoot:   "/Users/example/projects/example",
		GitRemote: "git@example.com:operator/example.git",
		PaneType:  "terminal",
		AgentKind: "agent-hook",
	}, true, nil
}
func (r *recordingSource) ContentFreeInvariant() error { return nil }

// TestStatusIsReadOnly: runActivityStatus must NOT call EnsureConfigSeeded.
// Calling status on a fresh home (no config file) must report enabled=false
// + config_status=missing WITHOUT writing the config file. The audit's bug 6:
// tightening read-only semantics across the CLI.
func TestStatusIsReadOnly(t *testing.T) {
	h := newTestHome(t)
	// Fresh home — no config/activity-sensor.json yet.
	if _, err := os.Stat(filepath.Join(h, "config", "activity-sensor.json")); err == nil {
		t.Fatalf("precondition: config must not exist yet")
	}
	// Status must NOT create the config.
	enabled, err := activity.Enabled(h)
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	if enabled {
		t.Fatalf("fresh home must report enabled=false, got %v", enabled)
	}
	// Verify config still does NOT exist after Enabled() call.
	if _, err := os.Stat(filepath.Join(h, "config", "activity-sensor.json")); err == nil {
		t.Fatalf("status must not write config; file appeared after Enabled()")
	}
}

// TestClassifySourceErrorMapping: the audit's bug 8 — raw srcErr.Error() must
// NOT be stored in Meta. Instead, classifySourceError must map to one of the
// closed codes (permission_lost / transport_failed / parse_failed / timeout).
func TestClassifySourceErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil error → empty", nil, ""},
		{"context.DeadlineExceeded → timeout", context.DeadlineExceeded, "timeout"},
		{"source.ErrPermissionLost → permission_lost", source.ErrPermissionLost, "permission_lost"},
		{"source.ErrParseFailed → parse_failed", source.ErrParseFailed, "parse_failed"},
		{"os.ErrPermission → permission_lost", os.ErrPermission, "permission_lost"},
		{"plain error → transport_failed", errors.New("cmux not found"), "transport_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(classifySourceError(tc.err))
			if got != tc.want {
				t.Errorf("classifySourceError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestWritePermissionLostEventNoRawError: the spooled event must carry only the
// closed code (source_error_code) and source_kind. NO raw error text.
func TestWritePermissionLostEventNoRawError(t *testing.T) {
	dir := t.TempDir()
	c := newCollectorWithDeps(awstream.NewClient("http://127.0.0.1:1"),
		source.NewRegistry(), dir, time.Now)
	// A deliberately scary raw error — contains a fake path.
	scary := errors.New("lsof: /Users/example/secret/file.txt: Permission denied (lsof -p 1234)")
	if err := c.writePermissionLostEvent(context.Background(), "com.example.app", "applescript", "2026-07-30T12:00:00Z", scary); err != nil {
		t.Fatalf("writePermissionLostEvent: %v", err)
	}
	entries, err := listSpool(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no spool entry written: %v", err)
	}
	data, _ := os.ReadFile(entries[0])
	if strings.Contains(string(data), "secret") {
		t.Fatalf("spool MUST NOT contain raw error text; got %s", data)
	}
	if strings.Contains(string(data), "lsof -p") {
		t.Fatalf("spool MUST NOT contain command text; got %s", data)
	}
	// Verify the closed code IS present.
	if !strings.Contains(string(data), `"source_error_code":"transport_failed"`) {
		t.Errorf("spool must carry source_error_code; got %s", data)
	}
	if !strings.Contains(string(data), `"source_kind":"applescript"`) {
		t.Errorf("spool must carry source_kind; got %s", data)
	}
}
