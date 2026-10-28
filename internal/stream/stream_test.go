package stream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	compoundhome "compound/internal/home"
	"compound/internal/work"
)

// eventLine renders one work.Event as a JSONL line exactly as the ledger writes it.
func eventLine(t *testing.T, id, from, to string) string {
	t.Helper()
	b, err := json.Marshal(work.Event{TS: "2026-07-14T00:00:00Z", WorkID: id, From: from, To: to})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(b) + "\n"
}

func writeHome(t *testing.T, workJSONL, seq string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "events", "work.jsonl"), []byte(workJSONL), 0o644); err != nil {
		t.Fatal(err)
	}
	if seq != "" {
		if err := os.MkdirAll(filepath.Join(home, "ledger"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "ledger", "seq"), []byte(seq), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// TestTailerCursorResumeLosesNothing is THE cursor falsifier: a follower that
// reconnects from the exact cursor a previous read emitted must see every later
// event once, with no gap and no duplicate — even when the resume cursor lands
// mid-stream. If the byte accounting is off by one, this reds.
func TestTailerCursorResumeLosesNothing(t *testing.T) {
	first := eventLine(t, "work_a", "", "open") + eventLine(t, "work_b", "open", "claimed")
	later := eventLine(t, "work_c", "claimed", "closed") + eventLine(t, "work_d", "", "open")

	// First read from 0 consumes the first batch and reports a resume cursor.
	t1 := &tailer{consumed: 0}
	f1 := t1.feed([]byte(first))
	if len(f1) != 2 {
		t.Fatalf("first batch: want 2 frames, got %d", len(f1))
	}
	resume := t1.consumed
	if resume != int64(len(first)) {
		t.Fatalf("resume cursor %d != end of first batch %d", resume, len(first))
	}
	// The last frame's cursor equals the resume cursor (offset past its line).
	if f1[1].Cursor != resume {
		t.Fatalf("last frame cursor %d != resume %d", f1[1].Cursor, resume)
	}

	// A FRESH tailer resuming from that cursor sees exactly the later batch —
	// this is the reconnect path (--follow --cursor <resume>).
	t2 := &tailer{consumed: resume}
	f2 := t2.feed([]byte(later))
	ids := []string{}
	for _, f := range append(append([]Frame{}, f1...), f2...) {
		if f.Kind == KindWorkEvent {
			ids = append(ids, f.Event.WorkID)
		}
	}
	got := strings.Join(ids, ",")
	if got != "work_a,work_b,work_c,work_d" {
		t.Fatalf("resume lost/duplicated events: %q", got)
	}
	// Cursors must be strictly increasing across the whole stream.
	prev := int64(-1)
	for _, f := range append(append([]Frame{}, f1...), f2...) {
		if f.Cursor <= prev {
			t.Fatalf("cursor not strictly increasing: %d after %d", f.Cursor, prev)
		}
		prev = f.Cursor
	}
	if t2.consumed != int64(len(first)+len(later)) {
		t.Fatalf("final cursor %d != total bytes %d", t2.consumed, len(first)+len(later))
	}
}

// TestTailerAdversarialChunking proves the partial-line buffer is byte-accurate
// regardless of where a read boundary falls: feeding the SAME bytes one byte at
// a time must yield the same frames (and same cursors) as one big feed.
func TestTailerAdversarialChunking(t *testing.T) {
	data := eventLine(t, "work_a", "", "open") +
		eventLine(t, "work_b", "open", "claimed") +
		eventLine(t, "work_c", "claimed", "closed")

	whole := &tailer{}
	wf := whole.feed([]byte(data))

	drip := &tailer{}
	var df []Frame
	raw := []byte(data)
	for i := 0; i < len(raw); i++ {
		df = append(df, drip.feed(raw[i:i+1])...)
	}
	if len(wf) != len(df) {
		t.Fatalf("frame count differs: whole=%d drip=%d", len(wf), len(df))
	}
	for i := range wf {
		if wf[i].Event.WorkID != df[i].Event.WorkID || wf[i].Cursor != df[i].Cursor {
			t.Fatalf("frame %d differs: whole=%+v drip=%+v", i, wf[i], df[i])
		}
	}
	if whole.consumed != drip.consumed {
		t.Fatalf("final cursor differs: whole=%d drip=%d", whole.consumed, drip.consumed)
	}
}

// TestTailerMalformedLineWarns proves a corrupt row does not crash the tail and
// does not masquerade as an event: it yields a warning frame while valid rows
// on either side still parse. A blank line yields no frame at all.
func TestTailerMalformedLineWarns(t *testing.T) {
	data := eventLine(t, "work_a", "", "open") +
		"{not json}\n" +
		"\n" + // blank line: skipped silently, no warning
		`{"ts":"t","work_id":"","to":"open"}` + "\n" + // valid JSON but missing work_id
		eventLine(t, "work_b", "open", "claimed")

	frames := (&tailer{}).feed([]byte(data))
	var kinds []string
	for _, f := range frames {
		kinds = append(kinds, f.Kind)
	}
	// work_event, warning (bad json), warning (missing field), work_event —
	// the blank line contributes nothing.
	want := []string{KindWorkEvent, KindWarning, KindWarning, KindWorkEvent}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("frame kinds = %v, want %v", kinds, want)
	}
	if frames[0].Event.WorkID != "work_a" || frames[3].Event.WorkID != "work_b" {
		t.Fatalf("valid events did not survive around the malformed row: %+v", frames)
	}
}

// TestResolveOffsetTruncation proves a shrunk file (rotation/compaction) resets
// to 0 and flags truncation so the caller drops its partial buffer.
func TestResolveOffsetTruncation(t *testing.T) {
	if off, trunc := ResolveOffset(100, 40); off != 0 || !trunc {
		t.Fatalf("shrunk file: got off=%d trunc=%v, want 0,true", off, trunc)
	}
	if off, trunc := ResolveOffset(40, 100); off != 40 || trunc {
		t.Fatalf("growing file: got off=%d trunc=%v, want 40,false", off, trunc)
	}
}

// TestBuildSnapshot proves --once carries the last N events, the end-of-log
// cursor, the seq, and a parseable RFC3339 generated_at (the doors contract).
func TestBuildSnapshot(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString(eventLine(t, "work_"+itoa(i), "", "open"))
	}
	home := writeHome(t, sb.String(), "seq-42\n")

	snap, err := BuildSnapshot(home, DefaultBacklog, time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if len(snap.Events) != DefaultBacklog {
		t.Fatalf("want last %d events, got %d", DefaultBacklog, len(snap.Events))
	}
	// The last event must be the newest (work_39), oldest-first ordering.
	if snap.Events[len(snap.Events)-1].WorkID != "work_39" {
		t.Fatalf("newest event = %q, want work_39", snap.Events[len(snap.Events)-1].WorkID)
	}
	if snap.Events[0].WorkID != "work_"+itoa(40-DefaultBacklog) {
		t.Fatalf("oldest kept event = %q, want work_%d", snap.Events[0].WorkID, 40-DefaultBacklog)
	}
	wantCursor := int64(len(sb.String()))
	if snap.Cursor != wantCursor {
		t.Fatalf("cursor = %d, want end-of-log %d", snap.Cursor, wantCursor)
	}
	if snap.Seq != "seq-42" {
		t.Fatalf("seq = %q, want seq-42 (trimmed)", snap.Seq)
	}
	if _, err := time.Parse(time.RFC3339, snap.GeneratedAt); err != nil {
		t.Fatalf("generated_at %q not RFC3339: %v", snap.GeneratedAt, err)
	}
}

// TestBuildSnapshotEmptyHome proves a fresh home (no log, no seq) degrades to an
// empty, cursor-0 snapshot instead of an error, and still emits [] not null.
func TestBuildSnapshotEmptyHome(t *testing.T) {
	home := t.TempDir()
	snap, err := BuildSnapshot(home, DefaultBacklog, time.Now())
	if err != nil {
		t.Fatalf("BuildSnapshot on empty home: %v", err)
	}
	if snap.Cursor != 0 || len(snap.Events) != 0 || snap.Seq != "" {
		t.Fatalf("empty-home snapshot not clean: %+v", snap)
	}
	b, _ := json.Marshal(snap)
	if !strings.Contains(string(b), `"events":[]`) {
		t.Fatalf("events must serialize as [], got %s", b)
	}
}

// TestFollowResumeFromSnapshotCursor is the integration falsifier: a snapshot's
// cursor fed into Follow must deliver an event appended AFTER the snapshot, and
// nothing before it. This is exactly the console route's seed-then-follow path.
func TestFollowResumeFromSnapshotCursor(t *testing.T) {
	backlog := eventLine(t, "work_old1", "", "open") + eventLine(t, "work_old2", "open", "closed")
	home := writeHome(t, backlog, "1")

	snap, err := BuildSnapshot(home, DefaultBacklog, time.Now())
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}

	// Append a NEW event after the snapshot was taken.
	logPath := filepath.Join(home, "events", "work.jsonl")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(eventLine(t, "work_new", "", "open")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Follow from the snapshot cursor; a fast poll + short deadline keeps the
	// test quick. Collect frames until the new event arrives, then cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w := &frameSink{done: cancel, want: "work_new"}
	err = Follow(ctx, home, FollowOptions{
		Cursor:       snap.Cursor,
		PollInterval: 20 * time.Millisecond,
		SeqInterval:  20 * time.Millisecond,
		Heartbeat:    time.Hour, // no heartbeat noise in this window
	}, w)
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	ids := w.workEventIDs()
	if len(ids) != 1 || ids[0] != "work_new" {
		t.Fatalf("follow from snapshot cursor delivered %v, want exactly [work_new] (no backlog replay, no loss)", ids)
	}
}

func TestFollowProjectsAttachedCollaborationAsTypedFrame(t *testing.T) {
	home := t.TempDir()
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	w, err := store.Create(work.CreateInput{
		Owner:            "multiplayer",
		SuccessPredicate: "typed collaboration stream frame",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "codex:multiplayer"
	if _, err := store.Claim(w.ID, actor, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	artifacts, err := compoundhome.NewArtifactStore(home)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("typed collaboration stream body"))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := store.RecordCollaboration(w.ID, work.CollaborationInput{
		Actor:          actor,
		IdempotencyKey: "stream-turn-1",
		SessionID:      "session-a",
		BodyRef:        bodyRef,
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sink := &collaborationFrameSink{done: cancel, want: recorded.Event.ID}
	if err := Follow(ctx, home, FollowOptions{
		Cursor:       0,
		PollInterval: 20 * time.Millisecond,
		SeqInterval:  time.Hour,
		Heartbeat:    time.Hour,
	}, sink); err != nil {
		t.Fatalf("follow collaboration stream: %v", err)
	}
	var typed []Frame
	for _, frame := range sink.frames {
		if frame.Kind == KindCollaborationEvent {
			typed = append(typed, frame)
		}
	}
	if len(typed) != 1 {
		t.Fatalf("typed collaboration frames=%d, want exactly 1; all=%+v", len(typed), sink.frames)
	}
	frame := typed[0]
	if frame.Collaboration == nil || frame.Collaboration.ID != recorded.Event.ID {
		t.Fatalf("typed frame lost collaboration event: %+v", frame)
	}
	if frame.Collaboration.BodyRef != recorded.Event.BodyRef || frame.Collaboration.Actor != actor {
		t.Fatalf("typed frame metadata mismatch: %+v", frame.Collaboration)
	}
	if frame.Event == nil || frame.Event.WorkID != w.ID {
		t.Fatalf("typed frame lost source lifecycle event: %+v", frame)
	}
}

type collaborationFrameSink struct {
	frames []Frame
	want   string
	done   context.CancelFunc
}

func (s *collaborationFrameSink) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame Frame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			return 0, err
		}
		s.frames = append(s.frames, frame)
		if frame.Kind == KindCollaborationEvent && frame.Collaboration != nil && frame.Collaboration.ID == s.want {
			s.done()
		}
	}
	return len(p), nil
}

// frameSink is a test io.Writer that parses emitted JSONL frames and cancels the
// Follow context the moment the awaited work_id arrives.
type frameSink struct {
	frames []Frame
	want   string
	done   context.CancelFunc
}

func (s *frameSink) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var f Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue
		}
		s.frames = append(s.frames, f)
		if f.Kind == KindWorkEvent && f.Event != nil && f.Event.WorkID == s.want {
			s.done()
		}
	}
	return len(p), nil
}

func (s *frameSink) workEventIDs() []string {
	var ids []string
	for _, f := range s.frames {
		if f.Kind == KindWorkEvent && f.Event != nil {
			ids = append(ids, f.Event.WorkID)
		}
	}
	return ids
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
