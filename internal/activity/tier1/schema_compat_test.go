package tier1

import (
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/activity"
)

// F13: Tier-1 envelope events with content-bearing meta keys are REFUSED by the
// strict decoder. The invariant is enforced at THREE layers:
//
//  1. Schema: no field name in the activity.Event struct carries content.
//  2. Strict decoder: DecodeStrict refuses any meta key in ForbiddenMetaKeys.
//  3. CheckEnvelopeContentFree: per-source runtime self-test catches leaks before
//     the event reaches the spool.
//
// This test pins the layer-2 path: a synthesized NDJSON line with a Tier-1 envelope
// plus a banned meta key refuses to decode. The forbidden key is the canonical
// attack — a buggy source that accidentally populates `title`.
func TestFalsifier13_Tier1EnvelopeContentFree(t *testing.T) {
	for _, banned := range []string{"title", "url", "document", "text", "content", "file", "command_args", "script", "initial_command", "env", "language"} {
		line := `{"schema_version":"compound.activity_observation.v0.2","seq":1,"ts":"2026-07-15T10:00:00Z","kind":"window_focused","bundle_id":"com.example.ok","app_name":"X","engine":{"id":"` + Tier1EngineID + `","revision":"v0.1.0"},"meta":{"pane_cwd":"/Users/example/projects/example","` + banned + `":"leaked"}}`
		var ev activity.Event
		// DecodeStrict is the load-bearing gate. Walk the line through it.
		rep := activity.DecodeStrict([]byte(line), 1)
		if len(rep.Accepted) != 0 {
			t.Errorf("banned meta key %q must be REFUSED at strict decode, but %d event(s) accepted", banned, len(rep.Accepted))
		}
		if len(rep.Refused) == 0 {
			t.Errorf("banned meta key %q must surface a refusal, got none", banned)
		}
		if !strings.Contains(rep.Refused[0].Reason, banned) {
			t.Errorf("refusal reason must name the banned key %q, got %q", banned, rep.Refused[0].Reason)
		}
		_ = ev
	}
}

// F13 positive: a clean Tier-1 envelope decodes successfully. The envelope carries
// pane_cwd + git_remote + agent_kind — all Tier-1-allowed meta keys.
func TestFalsifier13_Tier1EnvelopeAcceptsCleanMeta(t *testing.T) {
	line := `{"schema_version":"compound.activity_observation.v0.2","seq":1,"ts":"2026-07-15T10:00:00Z","kind":"window_focused","bundle_id":"com.example.terminal","app_name":"Terminal","engine":{"id":"` + Tier1EngineID + `","revision":"v0.1.0"},"meta":{"pane_cwd":"/Users/example/projects/example","git_remote":"git@example.com:operator/example.git","pane_type":"terminal","agent_kind":"agent-hook","agent_name":"Claude Code","surface_ref":"surf-abc","workspace_ref":"ws-xyz"}}`
	rep := activity.DecodeStrict([]byte(line), 1)
	if len(rep.Accepted) != 1 {
		t.Fatalf("clean Tier-1 envelope must decode, got %+v", rep)
	}
	if rep.Accepted[0].Kind != activity.KindWindowFocused {
		t.Errorf("Kind = %q, want window_focused", rep.Accepted[0].Kind)
	}
}

// TestTier1SetsDoNotOverlap: program-time guard. Tier1AllowedMetaKeys ∩
// activity.ForbiddenMetaKeys must be empty. If a future change adds a key to
// Tier1AllowedMetaKeys that's already in ForbiddenMetaKeys, F13 would fail at
// runtime — this test catches it at unit-test time.
func TestTier1SetsDoNotOverlap(t *testing.T) {
	for k := range Tier1AllowedMetaKeys {
		if activity.ForbiddenMetaKeys[k] {
			t.Fatalf("Tier-1 allowed meta key %q is also in ForbiddenMetaKeys — schema invariant violated", k)
		}
	}
	// Conversely: every forbidden key must NOT be in the Tier-1 allowed set.
	for k := range activity.ForbiddenMetaKeys {
		if Tier1AllowedMetaKeys[k] {
			t.Fatalf("forbidden meta key %q is in Tier1AllowedMetaKeys — schema invariant violated", k)
		}
	}
}

// TestBuildTier1EventCarriesOnlyAllowedKeys: buildTier1Event must NOT carry any
// key that isn't in Tier1AllowedMetaKeys (or the engine fields). A buggy
// buildTier1Event that adds a new meta key would fail this test before reaching
// the strict decoder.
func TestBuildTier1EventCarriesOnlyAllowedKeys(t *testing.T) {
	env := mustValidEnvelopeForTest()
	ev := buildTier1Event("com.example.terminal", env, "2026-07-30T12:00:00Z")
	// Every key in Meta must be Tier-1-allowed.
	for k := range ev.Meta {
		if !Tier1AllowedMetaKeys[k] {
			t.Errorf("meta key %q not in Tier1AllowedMetaKeys — invariant violated", k)
		}
	}
	// Top-level fields must NOT include banned keys.
	b, _ := json.Marshal(ev)
	s := string(b)
	for _, banned := range []string{"title", "url", "document", "text", "content"} {
		if strings.Contains(s, `"`+banned+`"`) {
			t.Errorf("event carries banned key %q: %s", banned, s)
		}
	}
}

// TestWindowFocusedCoalescesSameBundle: the segment reducer coalesces
// KindWindowFocused events with the same bundle as the current foreground (Phase 5
// lit this up). Three Tier-1 events on the same terminal in sequence must produce
// ONE span, not three. Span durations exceed the DwellFloorSec (15s) so the
// dwell-floor merger keeps the result.
func TestWindowFocusedCoalescesSameBundle(t *testing.T) {
	events := []activity.Event{
		{SchemaVersion: activity.SchemaVersion, Seq: 1, TS: "2026-07-15T10:00:00Z", Kind: activity.KindAppActivated, BundleID: "com.example.terminal", AppName: "Terminal"},
		{SchemaVersion: activity.SchemaVersion, Seq: 2, TS: "2026-07-15T10:00:30Z", Kind: activity.KindWindowFocused, BundleID: "com.example.terminal", AppName: "Terminal", Meta: map[string]string{"pane_cwd": "/Users/example/projects/a", "git_remote": "git@example.com:a.git"}},
		{SchemaVersion: activity.SchemaVersion, Seq: 3, TS: "2026-07-15T10:01:00Z", Kind: activity.KindWindowFocused, BundleID: "com.example.terminal", AppName: "Terminal", Meta: map[string]string{"pane_cwd": "/Users/example/projects/a", "git_remote": "git@example.com:a.git"}},
	}
	spans := activity.Segment(events)
	if len(spans) != 1 {
		t.Fatalf("three same-bundle events must coalesce to 1 span; got %d: %+v", len(spans), spans)
	}
	if spans[0].BundleID != "com.example.terminal" {
		t.Errorf("BundleID = %q", spans[0].BundleID)
	}
	// Span spans 60 seconds (10:00:00 to 10:01:00).
	if spans[0].ActiveSec < 60 || spans[0].ActiveSec > 61 {
		t.Errorf("ActiveSec = %d, want ~60s", spans[0].ActiveSec)
	}
}

// TestWindowFocusedDifferentBundleOpensNewSpan: KindWindowFocused with a DIFFERENT
// bundle than the current foreground closes the prior span and opens a new one
// (same rule as KindAppActivated).
func TestWindowFocusedDifferentBundleOpensNewSpan(t *testing.T) {
	events := []activity.Event{
		{SchemaVersion: activity.SchemaVersion, Seq: 1, TS: "2026-07-15T10:00:00Z", Kind: activity.KindAppActivated, BundleID: "com.example.terminal", AppName: "Terminal"},
		{SchemaVersion: activity.SchemaVersion, Seq: 2, TS: "2026-07-15T10:01:00Z", Kind: activity.KindWindowFocused, BundleID: "com.example.browser", AppName: "Browser"},
		// Final terminal return — pushed 60s forward so the trailing span has
		// non-zero duration (Segment closes trailing spans at lastTS).
		{SchemaVersion: activity.SchemaVersion, Seq: 3, TS: "2026-07-15T10:02:00Z", Kind: activity.KindAppActivated, BundleID: "com.example.terminal", AppName: "Terminal"},
		{SchemaVersion: activity.SchemaVersion, Seq: 4, TS: "2026-07-15T10:03:00Z", Kind: activity.KindAppActivated, BundleID: "com.example.terminal", AppName: "Terminal"},
	}
	spans := activity.Segment(events)
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans (different bundles), got %d: %+v", len(spans), spans)
	}
	wantBundles := []string{"com.example.terminal", "com.example.browser", "com.example.terminal"}
	for i, want := range wantBundles {
		if spans[i].BundleID != want {
			t.Errorf("span[%d].BundleID = %q, want %q", i, spans[i].BundleID, want)
		}
	}
}

// TestPermissionLostMarksIncomplete: Tier-1 envelope collector emits
// KindPermissionLost when a source fails (TCC revocation). The reducer marks spans
// in the window as CoverageIncomplete (F11 extension).
func TestPermissionLostMarksIncomplete(t *testing.T) {
	events := []activity.Event{
		{SchemaVersion: activity.SchemaVersion, Seq: 1, TS: "2026-07-15T10:00:00Z", Kind: activity.KindAppActivated, BundleID: "com.example.terminal"},
		{SchemaVersion: activity.SchemaVersion, Seq: 2, TS: "2026-07-15T10:01:00Z", Kind: activity.KindPermissionLost},
		// Add a same-bundle second app_activated 60s later so the trailing span
		// has non-zero duration; the permission_lost between them marks both
		// spans incomplete.
		{SchemaVersion: activity.SchemaVersion, Seq: 3, TS: "2026-07-15T10:02:00Z", Kind: activity.KindAppActivated, BundleID: "com.example.terminal"},
		{SchemaVersion: activity.SchemaVersion, Seq: 4, TS: "2026-07-15T10:03:00Z", Kind: activity.KindAppActivated, BundleID: "com.example.terminal"},
	}
	spans := activity.Segment(events)
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d: %+v", len(spans), spans)
	}
	for i, s := range spans {
		if s.Coverage != activity.CoverageIncomplete {
			t.Errorf("span[%d].Coverage = %q, want incomplete", i, s.Coverage)
		}
	}
}
