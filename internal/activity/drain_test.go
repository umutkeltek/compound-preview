package activity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/policy"
)

// writeSpool creates a spool dir with one NDJSON day file built from the given events.
func writeSpool(t *testing.T, events []Event) string {
	t.Helper()
	dir := t.TempDir()
	var buf []byte
	day := "2026-07-15"
	if len(events) > 0 {
		day = utcDay(events[0].TS)
	}
	for _, ev := range events {
		line, _ := json.Marshal(&ev)
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, day+".ndjson"), buf, 0o600); err != nil {
		t.Fatalf("write spool: %v", err)
	}
	return dir
}

func enableSensor(t *testing.T, home string, now time.Time) policy.Config {
	t.Helper()
	importGrants(t, home, now,
		grantSpec{"grant.activity_toggle", ToggleCapability, ""},
		grantSpec{"grant.activity_drain", DrainCapability, ""},
	)
	cfg := loadCfg(t, home)
	res, err := SetEnabled(home, cfg, "work_enable", true, now)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !res.Applied {
		t.Fatalf("enable must apply, got %+v", res)
	}
	return cfg
}

var drainEvents = []Event{
	evAt(1, "2026-07-15T10:00:00Z", KindAppActivated, "com.google.Chrome", "Chrome"),
	evAt(2, "2026-07-15T10:25:00Z", KindIdleStarted, "", ""),
}

func evAt(seq int64, ts, kind, bundle, app string) Event {
	return Event{SchemaVersion: SchemaVersion, Seq: seq, TS: ts, Kind: kind, BundleID: bundle, AppName: app, Engine: Engine{ID: "compound-activity-sensor", Revision: "v0.1"}}
}

// TestFalsifier2_DrainDisabledWritesZeroRows: enabled=false -> zero rows, refused, honest reason.
func TestFalsifier2_DrainDisabledWritesZeroRows(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	importGrant(t, h, "grant.activity_drain", DrainCapability, "", now)
	cfg := loadCfg(t, h) // sensor NOT enabled
	spool := writeSpool(t, drainEvents)

	rep, err := Drain(h, spool, cfg, "work_drain", now)
	if err != nil {
		t.Fatalf("drain err: %v", err)
	}
	if !rep.Refused || rep.EventsIngested != 0 {
		t.Fatalf("disabled drain must refuse with zero rows, got %+v", rep)
	}
	if _, statErr := os.Stat(SegmentPath(h, "2026-07-15")); statErr == nil {
		t.Fatal("disabled drain must not write any segment file")
	}
}

// TestFalsifier2_DrainNoGrantWritesZeroRows: enabled but NO drain grant -> refused, zero rows.
func TestFalsifier2_DrainNoGrantWritesZeroRows(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	importGrant(t, h, "grant.activity_toggle", ToggleCapability, "", now) // ONLY the toggle grant
	cfg := loadCfg(t, h)
	res, err := SetEnabled(h, cfg, "work_enable", true, now)
	if err != nil || !res.Applied {
		t.Fatalf("enable must apply: res=%+v err=%v", res, err)
	}
	// cfg has NO drain grant.
	spool := writeSpool(t, drainEvents)
	rep, err := Drain(h, spool, cfg, "work_drain", now)
	if err != nil {
		t.Fatalf("drain err: %v", err)
	}
	if !rep.Refused || rep.EventsIngested != 0 {
		t.Fatalf("ungranted drain must refuse, got %+v", rep)
	}
}

// TestFalsifier3_ExpiredGrantRefused: a drain grant expired in the past does not authorize the drain.
func TestFalsifier3_ExpiredGrantRefused(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	// Live toggle grant (so enable succeeds) + drain grant that expired yesterday, in one source.
	importGrants(t, h, now,
		grantSpec{"grant.activity_toggle", ToggleCapability, ""},
		grantSpec{"grant.activity_drain", DrainCapability, "2026-07-14T00:00:00Z"},
	)
	cfg := loadCfg(t, h)
	res, err := SetEnabled(h, cfg, "work_enable", true, now)
	if err != nil || !res.Applied {
		t.Fatalf("enable must apply (live toggle grant): res=%+v err=%v", res, err)
	}
	spool := writeSpool(t, drainEvents)
	rep, err := Drain(h, spool, cfg, "work_drain", now)
	if err != nil {
		t.Fatalf("drain err: %v", err)
	}
	if !rep.Refused {
		t.Fatalf("expired grant must refuse the drain, got %+v", rep)
	}
	// And it refuses for the RIGHT reason (expiry / not granted), not because the sensor is disabled.
	if contains(rep.Reason, "disabled") {
		t.Fatalf("F3 must refuse on grant expiry, not a disabled sensor: %q", rep.Reason)
	}
}

// TestDrainSuccessWritesSegmentsAndSpans: a granted, enabled drain ingests events and writes both
// the segment (raw validated events) and span (reduction) files.
func TestDrainSuccessWritesSegmentsAndSpans(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)
	spool := writeSpool(t, drainEvents)

	rep, err := Drain(h, spool, cfg, "work_drain", now)
	if err != nil {
		t.Fatalf("drain err: %v", err)
	}
	if rep.Refused || rep.SegmentLinesAdded != 2 {
		t.Fatalf("granted drain should ingest 2 events, got %+v", rep)
	}
	if _, err := os.Stat(SegmentPath(h, "2026-07-15")); err != nil {
		t.Fatalf("segment file missing: %v", err)
	}
	spanData, err := os.ReadFile(SpanPath(h, "2026-07-15"))
	if err != nil {
		t.Fatalf("span file missing: %v", err)
	}
	var sf SpanFile
	if err := json.Unmarshal(spanData, &sf); err != nil {
		t.Fatalf("span decode: %v", err)
	}
	if len(sf.Spans) != 1 || sf.Spans[0].Category != CategoryBrowsing {
		t.Fatalf("expected 1 browsing span, got %+v", sf.Spans)
	}
}

// TestFalsifier12_IdempotentDrain: draining twice over the same spool yields byte-identical spans and
// no duplicate segment lines.
func TestFalsifier12_IdempotentDrain(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)
	spool := writeSpool(t, drainEvents)

	if _, err := Drain(h, spool, cfg, "work_drain", now); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	seg1, _ := os.ReadFile(SegmentPath(h, "2026-07-15"))
	span1, _ := os.ReadFile(SpanPath(h, "2026-07-15"))

	rep2, err := Drain(h, spool, cfg, "work_drain2", now)
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if rep2.SegmentLinesAdded != 0 {
		t.Fatalf("re-drain must add zero segment lines (cursor honored), added %d", rep2.SegmentLinesAdded)
	}
	seg2, _ := os.ReadFile(SegmentPath(h, "2026-07-15"))
	span2, _ := os.ReadFile(SpanPath(h, "2026-07-15"))
	if string(seg1) != string(seg2) {
		t.Fatalf("segment file changed on re-drain (duplicate lines):\n%s\n---\n%s", seg1, seg2)
	}
	if string(span1) != string(span2) {
		t.Fatalf("span file not byte-identical on re-drain:\n%s\n---\n%s", span1, span2)
	}
}

// TestFalsifier1_DrainRefusesBadLineButKeepsGood: an unknown-field line in the spool is refused and
// counted; the clean lines still produce segments (per-line fail-closed within a drain).
func TestFalsifier1_DrainRefusesBadLineButKeepsGood(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)
	dir := t.TempDir()
	body := okLine + "\n" +
		`{"schema_version":"compound.activity_observation.v0.2","seq":2,"ts":"2026-07-15T10:30:00Z","kind":"idle_started","bundle_id":"","app_name":"","engine":{"id":"e","revision":"v0.1"},"title":"leak"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-07-15.ndjson"), []byte(body), 0o600); err != nil {
		t.Fatalf("write spool: %v", err)
	}
	rep, err := Drain(h, dir, cfg, "work_drain", now)
	if err != nil {
		t.Fatalf("drain err: %v", err)
	}
	if rep.RefusedLines != 1 {
		t.Fatalf("expected 1 refused line, got %d", rep.RefusedLines)
	}
	if rep.SegmentLinesAdded != 1 {
		t.Fatalf("clean line should still be ingested, added %d", rep.SegmentLinesAdded)
	}
	seg, _ := os.ReadFile(SegmentPath(h, "2026-07-15"))
	if contains(string(seg), "leak") {
		t.Fatalf("refused content leaked into segment: %s", seg)
	}
}
