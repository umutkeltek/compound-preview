package awstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/activity"
)

// awFixtureServer emulates the AW REST contract: GET /api/0/buckets/ returns a JSON
// object keyed by bucket id; GET /api/0/buckets/{id}/events returns a JSON array of
// events. Both shapes are what aw-server (Python) and aw-server-rust produce.
//
// `events` is a map from bucket id to the events that server returns. The server
// emits float-seconds timestamps for window watcher and ISO strings for the same —
// our client must accept both (F: TestNormalize_HandlesBothFloatAndISOTimestamps).
type awFixtureServer struct {
	buckets map[string]Bucket
	events  map[string][]Event
}

func newAWFixtureServer(t *testing.T, f awFixtureServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/0/buckets/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/0/buckets/")
		if path == "" {
			// /api/0/buckets/  → list all buckets
			out := make(map[string]Bucket, len(f.buckets))
			for id, b := range f.buckets {
				b.ID = id
				out[id] = b
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		// path = "{bucket_id}/events" (or anything else — return events for that bucket)
		parts := strings.Split(path, "/")
		bucketID := parts[0]
		if len(parts) >= 2 && parts[1] == "events" {
			evs := f.events[bucketID]
			if evs == nil {
				evs = []Event{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(evs)
			return
		}
		http.NotFound(w, r)
	})
	return httptest.NewServer(mux)
}

// rawEvent is a test-side helper: builds a RawEvent from a bucket + payload.
func rawEvent(bucket Bucket, ts time.Time, dur float64, data any) RawEvent {
	b, _ := json.Marshal(data)
	return RawEvent{
		Bucket: bucket,
		AWEvent: Event{
			Timestamp: ts.UTC().Format(time.RFC3339Nano),
			Duration:  dur,
			Data:      b,
		},
	}
}

func TestEvent_UnmarshalJSON_AcceptsBothShapes(t *testing.T) {
	// Float-seconds (window watcher).
	var ev Event
	if err := json.Unmarshal([]byte(`{"timestamp":1700000000.5,"duration":12.5,"data":{}}`), &ev); err != nil {
		t.Fatalf("float-seconds: %v", err)
	}
	if !strings.HasPrefix(ev.Timestamp, "2023-") {
		t.Fatalf("float-seconds: expected 2023- prefix, got %q", ev.Timestamp)
	}
	if ev.Duration != 12.5 {
		t.Fatalf("float-seconds: duration = %v, want 12.5", ev.Duration)
	}

	// ISO string (web watcher).
	ev = Event{}
	if err := json.Unmarshal([]byte(`{"timestamp":"2023-11-14T22:13:20+00:00","duration":7,"data":{}}`), &ev); err != nil {
		t.Fatalf("iso: %v", err)
	}
	if !strings.HasPrefix(ev.Timestamp, "2023-11-14T22:13:20") {
		t.Fatalf("iso: expected 2023-11-14T22:13:20 prefix, got %q", ev.Timestamp)
	}

	// Malformed.
	if err := json.Unmarshal([]byte(`{"timestamp":"not-a-date","duration":0,"data":{}}`), &ev); err == nil {
		t.Fatalf("malformed iso: must reject")
	}
}

func TestBucket_DeviceIDAndBucketType(t *testing.T) {
	b := Bucket{
		ID:       "aw-watcher-window_studio-host.example",
		Hostname: "studio-host.example",
		Client:   "aw-watcher-window",
		Metadata: map[string]interface{}{"device_id": "2e6424e0-e1d8-40b9-abf5-af5454502fb0"},
	}
	if b.DeviceID() != "2e6424e0-e1d8-40b9-abf5-af5454502fb0" {
		t.Fatalf("DeviceID = %q", b.DeviceID())
	}
	if b.BucketType() != "window" {
		t.Fatalf("BucketType = %q, want window", b.BucketType())
	}
	// Empty metadata.
	empty := Bucket{ID: "aw-watcher-afk_host-b"}
	if empty.DeviceID() != "" {
		t.Fatalf("empty metadata DeviceID = %q", empty.DeviceID())
	}
	if empty.BucketType() != "afk" {
		t.Fatalf("empty BucketType = %q", empty.BucketType())
	}
	// Malformed id.
	bogus := Bucket{ID: "not-a-watcher-bucket"}
	if bogus.BucketType() != "" {
		t.Fatalf("bogus BucketType = %q", bogus.BucketType())
	}
}

func TestClient_ListBucketsAndFetchEvents_AgainstFixture(t *testing.T) {
	// Generic hostnames, generic paths — no personal data.
	buckets := map[string]Bucket{
		"aw-watcher-window_host-a.example": {
			Hostname: "host-a.example",
			Client:   "aw-watcher-window",
			Type:     "currentwindow",
		},
		"aw-watcher-afk_host-b.example": {
			Hostname: "host-b.example",
			Client:   "aw-watcher-afk",
			Type:     "afkstatus",
		},
	}
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	events := map[string][]Event{
		"aw-watcher-window_host-a.example": {
			{Timestamp: ts.Format(time.RFC3339Nano), Duration: 30, Data: json.RawMessage(`{"app":"com.example.terminal","title":"x"}`)},
		},
		"aw-watcher-afk_host-b.example": {
			{Timestamp: ts.Format(time.RFC3339Nano), Duration: 60, Data: json.RawMessage(`{"status":"afk"}`)},
		},
	}
	srv := newAWFixtureServer(t, awFixtureServer{buckets: buckets, events: events})
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx := context.Background()

	gotBuckets, err := c.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(gotBuckets) != 2 {
		t.Fatalf("ListBuckets: got %d, want 2", len(gotBuckets))
	}

	gotEvents, err := c.FetchEvents(ctx, "aw-watcher-afk_host-b.example", time.Time{}, time.Time{}, 100)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(gotEvents) != 1 || gotEvents[0].Duration != 60 {
		t.Fatalf("FetchEvents: got %+v", gotEvents)
	}
}

// TestNormalize_JoinAcrossHostnamesByBucketType: events from 3 hostnames (window
// watcher each) collapse to a single normalized stream. Per-host host_bindings are
// preserved in the parallel slice so the caller can stamp them on outbound headers.
func TestNormalize_JoinAcrossHostnamesByBucketType(t *testing.T) {
	ts1 := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ts2 := ts1.Add(time.Minute)
	raw := []RawEvent{
		rawEvent(Bucket{ID: "aw-watcher-window_host-a.example", Hostname: "host-a.example", Client: "aw-watcher-window"},
			ts1, 30, map[string]any{"app": "com.example.terminal"}),
		rawEvent(Bucket{ID: "aw-watcher-window_host-b.example", Hostname: "host-b.example", Client: "aw-watcher-window"},
			ts1, 30, map[string]any{"app": "com.example.terminal"}),
		rawEvent(Bucket{ID: "aw-watcher-window_host-c.example", Hostname: "host-c.example", Client: "aw-watcher-window"},
			ts2, 60, map[string]any{"app": "com.example.browser"}),
	}
	events, hosts, logs := Normalize(raw)
	if logs.DuplicateTS != 1 {
		t.Fatalf("DuplicateTS = %d, want 1 (ts1 collision)", logs.DuplicateTS)
	}
	if len(events) != 3 || len(hosts) != 3 || len(events) != len(hosts) {
		t.Fatalf("len mismatch: events=%d hosts=%d", len(events), len(hosts))
	}
	// Order: (window, ts1, host-a) → (window, ts1, host-b) → (window, ts2, host-c).
	if events[0].TS != events[1].TS {
		t.Fatalf("expected ts1 then ts1 then ts2, got %s, %s, %s", events[0].TS, events[1].TS, events[2].TS)
	}
	if hosts[0].Hostname != "host-a.example" || hosts[1].Hostname != "host-b.example" || hosts[2].Hostname != "host-c.example" {
		t.Fatalf("hostnames out of order: %+v", hosts)
	}
	// All events are KindAppActivated with the expected bundles.
	for i, ev := range events {
		if ev.Kind != activity.KindAppActivated {
			t.Fatalf("event[%d].Kind = %q, want app_activated", i, ev.Kind)
		}
	}
	if events[0].BundleID != "com.example.terminal" || events[2].BundleID != "com.example.browser" {
		t.Fatalf("bundles wrong: %s, %s", events[0].BundleID, events[2].BundleID)
	}
}

// TestNormalize_AcceptsEventsWithTitleURL_StripsFromMeta: an AW window event
// carrying a `title` field is ACCEPTED, but the title is NOT mirrored into the
// activity.Event.Meta map. This is the INPUT-vs-OUTPUT content-free fix: real
// AW payloads carry title/url/file by design; dropping such events made the
// bridge useless on real data. We strip them at the output boundary by only
// reading safe, explicit fields (app / bundle_id / app_name).
func TestNormalize_AcceptsEventsWithTitleURL_StripsFromMeta(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		// Window event with title (standard AW payload) — accepted.
		rawEvent(Bucket{ID: "aw-watcher-window_host-a.example", Hostname: "host-a.example"},
			ts, 30, map[string]any{"app": "com.example.terminal", "title": "secret window title"}),
		// Web event with url + title (standard browser watcher payload) — accepted.
		rawEvent(Bucket{ID: "aw-watcher-web-chrome_host-a.example", Hostname: "host-a.example"},
			ts.Add(time.Second), 30, map[string]any{"app": "com.example.browser", "url": "https://secret.example/path", "title": "tab title"}),
		// VSCode watcher with file — accepted, file NOT mirrored.
		rawEvent(Bucket{ID: "aw-watcher-vscode_host-a.example", Hostname: "host-a.example"},
			ts.Add(2*time.Second), 30, map[string]any{"app": "com.example.editor", "file": "/path/to/leaked/file.go", "project": "/path/to/project"}),
	}
	events, _, _ := Normalize(raw)
	if len(events) != 3 {
		t.Fatalf("all three real-AW-shape events must be accepted; got %d", len(events))
	}
	// Critical: Meta must be empty for all three. Title/url/file must NEVER appear.
	for i, ev := range events {
		if len(ev.Meta) != 0 {
			t.Errorf("event[%d] Meta must be empty (title/url/file stripped from output), got %+v", i, ev.Meta)
		}
		// Belt-and-braces: marshal and grep for any banned-key string.
		b, _ := json.Marshal(ev)
		for _, banned := range []string{"title", "url", "file", "document", "text", "content"} {
			if strings.Contains(string(b), `"`+banned+`"`) {
				t.Errorf("event[%d] contains banned key %q in marshaled output: %s", i, banned, b)
			}
		}
	}
	// Bundles preserved (safe field). Sort order is by (bucket_type, ts, hostname);
	// alphabetical bucket type order is: vscode < web-chrome < window.
	wantBundles := []string{"com.example.editor", "com.example.browser", "com.example.terminal"}
	for i, w := range wantBundles {
		if events[i].BundleID != w {
			t.Errorf("event[%d].BundleID = %q, want %q", i, events[i].BundleID, w)
		}
	}
	// BannedMeta count must be zero — events were accepted, not dropped.
}

// TestNormalize_RefusesEventWithOnlyTitle: an event whose payload has ONLY
// banned keys (no app / bundle_id) is still refused — there is no safe field
// to keep, and we cannot trust the watcher.
func TestNormalize_RefusesEventWithOnlyTitle(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		rawEvent(Bucket{ID: "aw-watcher-window_host-a.example", Hostname: "host-a.example"},
			ts, 30, map[string]any{"title": "no app, no bundle, just a title"}),
	}
	events, _, logs := Normalize(raw)
	if len(events) != 0 {
		t.Fatalf("event with no safe key (app/bundle_id) must be refused; got %d events", len(events))
	}
	if logs.BadEvent != 1 {
		t.Errorf("BadEvent = %d, want 1", logs.BadEvent)
	}
}

// TestNormalize_StableAcrossRuns: F12. Two Normalize calls on identical input produce
// byte-identical events. We marshal both runs and compare.
func TestNormalize_StableAcrossRuns(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts, 30, map[string]any{"app": "com.example.x"}),
		rawEvent(Bucket{ID: "aw-watcher-window_host-b", Hostname: "host-b"}, ts.Add(time.Minute), 60, map[string]any{"app": "com.example.y"}),
		rawEvent(Bucket{ID: "aw-watcher-afk_host-a", Hostname: "host-a"}, ts.Add(2*time.Minute), 90, map[string]any{"status": "afk"}),
	}
	ev1, h1, _ := Normalize(raw)
	ev2, h2, _ := Normalize(raw)
	AssignSeq(ev1)
	AssignSeq(ev2)
	b1, _ := json.Marshal(ev1)
	b2, _ := json.Marshal(ev2)
	if !reflect.DeepEqual(b1, b2) {
		t.Fatalf("F12: bytes diverge\nrun1: %s\nrun2: %s", b1, b2)
	}
	// Hosts should also match.
	if !reflect.DeepEqual(h1, h2) {
		t.Fatalf("hosts diverge\nrun1: %+v\nrun2: %+v", h1, h2)
	}
	// Re-arranging input order must not change output — F12 also covers order invariance.
	reverse := make([]RawEvent, len(raw))
	for i, r := range raw {
		reverse[len(raw)-1-i] = r
	}
	ev3, _, _ := Normalize(reverse)
	AssignSeq(ev3)
	b3, _ := json.Marshal(ev3)
	if !reflect.DeepEqual(b1, b3) {
		t.Fatalf("F12 order invariance: reversed input produced different output\nnormal: %s\nreversed: %s", b1, b3)
	}
}

// TestNormalize_AfkBecomesIdleLifecycle: AFK status maps to KindIdleStarted (afk) and
// KindIdleEnded (not-afk). Empty bundle_id is OK for these.
func TestNormalize_AfkBecomesIdleLifecycle(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		rawEvent(Bucket{ID: "aw-watcher-afk_host-a", Hostname: "host-a"}, ts, 30, map[string]any{"status": "afk"}),
		rawEvent(Bucket{ID: "aw-watcher-afk_host-a", Hostname: "host-a"}, ts.Add(time.Minute), 30, map[string]any{"status": "not-afk"}),
		rawEvent(Bucket{ID: "aw-watcher-afk_host-a", Hostname: "host-a"}, ts.Add(2*time.Minute), 30, map[string]any{"status": "weird-state"}),
	}
	events, _, logs := Normalize(raw)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (afk + not-afk), got %d", len(events))
	}
	if events[0].Kind != activity.KindIdleStarted {
		t.Fatalf("event[0].Kind = %q, want idle_started", events[0].Kind)
	}
	if events[1].Kind != activity.KindIdleEnded {
		t.Fatalf("event[1].Kind = %q, want idle_ended", events[1].Kind)
	}
	for _, ev := range events {
		if ev.BundleID != "" {
			t.Fatalf("afk event must have empty BundleID, got %q", ev.BundleID)
		}
	}
	if logs.BadEvent != 1 {
		t.Fatalf("BadEvent = %d, want 1 (weird-state)", logs.BadEvent)
	}
}

// TestNormalize_WindowRequiresBundle: window-class events without a bundle id are
// refused. activity's requiresBundle gate would fire later; we surface it earlier so
// the caller can count.
func TestNormalize_WindowRequiresBundle(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		// Empty data object — no app, no bundle_id.
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts, 30, map[string]any{}),
		// Data is "null".
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts.Add(time.Second), 30, nil),
	}
	events, _, logs := Normalize(raw)
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
	if logs.BadEvent != 2 {
		t.Fatalf("BadEvent = %d, want 2", logs.BadEvent)
	}
}

// TestNormalize_DeterministicSeqAssignment: AssignSeq produces monotonic 1..N. Same
// input always produces the same assignment.
func TestNormalize_DeterministicSeqAssignment(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts, 30, map[string]any{"app": "com.example.x"}),
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts.Add(time.Second), 30, map[string]any{"app": "com.example.y"}),
	}
	events, _, _ := Normalize(raw)
	AssignSeq(events)
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("Seq = (%d, %d), want (1, 2)", events[0].Seq, events[1].Seq)
	}
}

// TestNormalize_NoBannedFieldsInOutput: F5 also applies to OUR output. Marshaled
// events must not carry title/url/document/text/content anywhere.
func TestNormalize_NoBannedFieldsInOutput(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts, 30, map[string]any{
			"app": "com.example.terminal",
			// Even if the source had title/url, Normalize drops the event — so
			// include it here AND in a separate "shouldn't happen" check below.
		}),
		// Pathological watcher that successfully smuggles data — the read-side
		// gate REFUSES this; output must not contain title.
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts.Add(time.Second), 30, map[string]any{
			"app":   "com.example.bad",
			"title": "secret", "url": "secret", "document": "secret", "text": "secret", "content": "secret",
		}),
	}
	events, hosts, _ := Normalize(raw)
	for i, ev := range events {
		b, _ := json.Marshal(ev)
		for _, banned := range []string{"title", "url", "document", "text", "content"} {
			if strings.Contains(strings.ToLower(string(b)), `"`+banned+`"`) {
				t.Fatalf("event[%d] output carries banned key %q: %s", i, banned, b)
			}
		}
	}
	for i, h := range hosts {
		b, _ := json.Marshal(h)
		// Host bindings may have host_id/hostname/device_id; nothing else.
		s := string(b)
		if strings.Contains(s, "title") || strings.Contains(s, "url") {
			t.Fatalf("host[%d] output carries banned key: %s", i, s)
		}
	}
}

// TestNormalize_HostBindingsPropagate: the parallel []envelope.HostBindings slice
// carries per-event hostname + device_id from the source bucket. Required for the
// cross-host correlation that motivates this whole package.
func TestNormalize_HostBindingsPropagate(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	bucket := Bucket{
		ID: "aw-watcher-window_studio-host.example", Hostname: "studio-host.example",
		Client:   "aw-watcher-window",
		Metadata: map[string]interface{}{"device_id": "stable-device-id-001"},
	}
	raw := []RawEvent{
		rawEvent(bucket, ts, 30, map[string]any{"app": "com.example.x"}),
	}
	_, hosts, _ := Normalize(raw)
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host bindings, got %d", len(hosts))
	}
	if hosts[0].Hostname != "studio-host.example" {
		t.Fatalf("Hostname = %q", hosts[0].Hostname)
	}
	if hosts[0].HostID != "stable-device-id-001" {
		t.Fatalf("HostID = %q", hosts[0].HostID)
	}
	if hosts[0].DeviceID != "aw-watcher-window" {
		t.Fatalf("DeviceID = %q", hosts[0].DeviceID)
	}
}

// Compile-time sanity: ensure the package sorts deterministically even when input
// already looks sorted — guards against a future sort.SliceStable → sort.Slice drift.
func TestNormalize_InputAlreadySorted(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		rawEvent(Bucket{ID: "aw-watcher-window_host-a.example", Hostname: "host-a.example"}, ts, 30, map[string]any{"app": "com.example.x"}),
		rawEvent(Bucket{ID: "aw-watcher-window_host-b.example", Hostname: "host-b.example"}, ts, 30, map[string]any{"app": "com.example.x"}),
	}
	// Pre-sort and verify Normalize doesn't break it.
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].Bucket.Hostname < raw[j].Bucket.Hostname })
	events, hosts, _ := Normalize(raw)
	if len(events) != 2 || hosts[0].Hostname != "host-a.example" || hosts[1].Hostname != "host-b.example" {
		t.Fatalf("sort drift: events=%d hosts=%+v", len(events), hosts)
	}
}

// TestNormalize_AppNameToBundleIDMapping: AW `app` field is often a human process
// name, not a macOS bundle id. The normalizer must translate known names
// (Chrome, Brave Browser, iTerm2, etc.) to their bundle ids and emit a safe
// pseudo-bundle for unknown names.
func TestNormalize_AppNameToBundleIDMapping(t *testing.T) {
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	raw := []RawEvent{
		// bundle_id present — always wins.
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts, 30,
			map[string]any{"app": "irrelevant process name", "bundle_id": "com.google.Chrome"}),
		// app is a reverse-DNS bundle id — accept as-is.
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts.Add(time.Second), 30,
			map[string]any{"app": "com.apple.Safari"}),
		// app is a known human name — map to bundle id.
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts.Add(2*time.Second), 30,
			map[string]any{"app": "Brave Browser"}),
		// app is unknown human name — emit aw-app:<slug>.
		rawEvent(Bucket{ID: "aw-watcher-window_host-a", Hostname: "host-a"}, ts.Add(3*time.Second), 30,
			map[string]any{"app": "Some Mystery App"}),
	}
	events, _, _ := Normalize(raw)
	if len(events) != 4 {
		t.Fatalf("expected 4 accepted events; got %d", len(events))
	}
	want := []string{"com.google.Chrome", "com.apple.Safari", "com.brave.Browser", "aw-app:some-mystery-app"}
	for i, w := range want {
		if events[i].BundleID != w {
			t.Errorf("event[%d].BundleID = %q, want %q", i, events[i].BundleID, w)
		}
	}
}
