package tier1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/activity"
	"compound/internal/activity/tier1/source"
	"compound/internal/adapter/awstream"
)

// cmuxFixtureRunner is a test-side cmuxRunner that returns canned JSON without
// shelling out. Used by the smoke test to avoid spawning a real cmux process.
type cmuxFixtureRunner struct{ payload []byte }

func (r cmuxFixtureRunner) Run(ctx context.Context) ([]byte, error) {
	return r.payload, nil
}

// TestActivityCLITier1CollectSmoke_AgainstFixture: end-to-end smoke for the
// Tier-1 collector. Spins up an httptest server with canned AW responses,
// builds a Collector pointing at it, and runs a bounded collect. Asserts:
//
//   - The run completes within the --duration budget (F17 — no Go daemon doctrine).
//   - SourcesCalled > 0 (the cmux source is invoked for the focused cmux surface).
//   - The Tier-1 events written to the spool pass strict-decode (F13 — content-free).
//   - BannedRefused == 0 (no leaked content).
//
// This is the runtime smoke for Phase 8. It does not test the CLI verb dispatch
// (covered by the unit-tier1 tests in tier1_test.go); it tests the integration
// of: AW reader + envelope source registry + git resolver + spool writer +
// strict decoder.
func TestActivityCLITier1CollectSmoke_AgainstFixture(t *testing.T) {
	// AW fixture: one bucket (currentwindow) + one event focused on cmux.
	var awEventsServed atomic.Int32
	awSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			awEventsServed.Add(1)
			// Window watcher emits cmux as the focused app. NO title/url/document
			// fields — those are banned meta keys (F5) and would be refused at
			// the read boundary. The collector sees the bundle_id, not the title.
			_, _ = w.Write([]byte(`[{"timestamp":1700000000.0,"duration":30,"data":{"app":"com.cmuxterm.app"}}]`))
		case strings.HasSuffix(r.URL.Path, "/buckets/") || r.URL.Path == "/api/0/buckets/":
			_, _ = w.Write([]byte(`{"aw-watcher-window_local":{"id":"aw-watcher-window_local","type":"currentwindow","client":"aw-watcher-window","hostname":"local","data":{}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer awSrv.Close()

	// cmux source wired to a fixture runner.
	cmuxSrc := source.NewCmuxSourceWithRunner(cmuxFixtureRunner{
		payload: []byte(`{"surfaces":[{"id":"surf-test","type":"agent-session","focused":true,"window_id":"win-1","workspace_id":"ws-1","pane_id":"pane-1","pane_ref":"pane:1","requested_working_directory":"/Users/example/projects/example","resume_binding":{"kind":"agent-hook","name":"Claude Code","cwd":"/Users/example/projects/example"}}]}`),
	})
	reg := source.NewRegistryFromSources(
		cmuxSrc,
		source.NewAppleScriptSource(source.AppleScriptConfig{BundleID: "com.googlecode.iterm2"}),
	)

	// AW client pointed at the test server.
	awClient := awstream.NewClient(awSrv.URL)
	spoolDir := t.TempDir()

	collector := newCollectorWithDeps(awClient, reg, spoolDir, func() time.Time {
		return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	})

	start := time.Now()
	rep := collector.Run(context.Background(), 5*time.Second)
	elapsed := time.Since(start)

	// F17: bounded by --duration. 5s invocation must complete well under
	// the 5s budget on a local fixture; we give it 3s of headroom for CI noise.
	if elapsed > 3*time.Second {
		t.Fatalf("Collector.Run(5s) must complete quickly, took %s", elapsed)
	}
	if rep.Refused {
		t.Fatalf("smoke collect must not refuse: %+v", rep)
	}
	if rep.SourcesCalled == 0 {
		t.Fatalf("smoke collect must call at least one source, got SourcesCalled=%d", rep.SourcesCalled)
	}
	if rep.BannedRefused != 0 {
		t.Fatalf("smoke collect must not refuse any banned-key envelopes; got %d", rep.BannedRefused)
	}

	// Verify the spool has at least one Tier-1 event and it passes strict decode.
	entries, err := listSpool(spoolDir)
	if err != nil {
		t.Fatalf("list spool: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("smoke collect must write at least one Tier-1 event to spool")
	}
	totalLines := 0
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rep := activity.DecodeStrict(data, 1)
		totalLines += len(rep.Accepted)
		for _, r := range rep.Refused {
			t.Errorf("Tier-1 event refused by strict decoder: %v", r.Reason)
		}
	}
	if totalLines == 0 {
		t.Fatalf("smoke collect wrote no accepted Tier-1 lines; the run produced 0 events")
	}

	// Each accepted Tier-1 event must be a KindWindowFocused with at least one
	// Tier-1-allowed meta key.
	data, _ := os.ReadFile(entries[0])
	dec := activity.DecodeStrict(data, 1)
	if len(dec.Accepted) == 0 {
		t.Fatalf("expected at least one decoded event in spool[0]")
	}
	sample := dec.Accepted[0]
	if sample.Kind != activity.KindWindowFocused {
		t.Errorf("Tier-1 event Kind = %q, want window_focused", sample.Kind)
	}
	if sample.Meta["pane_cwd"] == "" {
		t.Errorf("Tier-1 event missing pane_cwd meta: %+v", sample.Meta)
	}

	// Sanity: at least one AW request was served.
	if awEventsServed.Load() == 0 {
		t.Errorf("smoke collect never fetched AW events; check fixture wiring")
	}
	t.Logf("smoke run: events_written=%d sources_called=%d aw_hits=%d elapsed=%s",
		totalLines, rep.SourcesCalled, awEventsServed.Load(), elapsed)
}

// listSpool enumerates NDJSON files in a spool directory. Returns absolute paths.
func listSpool(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasSuffix(name, ".ndjson") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// TestSmokeTier1EnvelopeDecodesCleanly: belt-and-braces check that a fully-populated
// Tier-1 envelope (cmux surface.list payload → Collector envelope → spool event)
// decodes cleanly through the strict decoder AND has no banned meta key in the
// marshaled form. The runtime smoke covers the end-to-end flow; this static check
// covers the JSON shape contract.
func TestSmokeTier1EnvelopeDecodesCleanly(t *testing.T) {
	line := `{"schema_version":"compound.activity_observation.v0.2","seq":1,"ts":"2026-07-30T12:00:00Z","kind":"window_focused","bundle_id":"com.cmuxterm.app","app_name":"cmux","engine":{"id":"` + Tier1EngineID + `","revision":"v0.1.0"},"meta":{"pane_cwd":"/Users/example/projects/example","git_remote":"git@example.com:operator/example.git","pane_type":"agent-session","agent_kind":"agent-hook","agent_name":"Claude Code","surface_ref":"surf-test","workspace_ref":"ws-1"}}`
	rep := activity.DecodeStrict([]byte(line), 1)
	if len(rep.Accepted) != 1 {
		t.Fatalf("smoke envelope must decode; got %+v", rep)
	}
	for k := range rep.Accepted[0].Meta {
		if activity.ForbiddenMetaKeys[k] {
			t.Fatalf("Tier-1 meta key %q is in ForbiddenMetaKeys — schema invariant violated", k)
		}
	}
	b, _ := json.Marshal(rep.Accepted[0])
	for _, banned := range []string{"title", "url", "document", "text", "content", "file", "command_args", "script", "initial_command", "env", "language"} {
		if strings.Contains(string(b), `"`+banned+`"`) {
			t.Fatalf("Tier-1 event contains banned key %q in marshaled form: %s", banned, b)
		}
	}
}
