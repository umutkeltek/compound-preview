// Package stream is a read-only reader organ over the append-only work-event
// log (COMPOUND_HOME/events/work.jsonl) and the ledger sequence pulse file
// (COMPOUND_HOME/ledger/seq). It tails by BYTE CURSOR and NEVER takes the
// work-event write lock — it opens the raw log for reading only (os.Open is
// O_RDONLY), so a live console tail can never contend with an authoritative
// writer. It owns no authority object: it emits evidence frames a client (the
// console SSE route at console/src/app/api/stream/route.ts) translates into the
// existing SSE vocabulary, and it promotes nothing.
//
// The wire is single-sourced on internal/work.Event, mirroring the client-side
// contract in console/src/lib/stream/tail-parser.ts (WorkEvent): never invent a
// field the ledger does not write.
package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/eventstore"
	"compound/internal/work"
)

// Frame kinds. The --once snapshot is one object of kind "snapshot"; --follow
// emits one JSON object per line of these kinds.
const (
	KindSnapshot  = "snapshot"
	KindWorkEvent = "work_event"
	// KindCollaborationEvent is a typed projection of a work lifecycle event
	// carrying one attached work.CollaborationEvent. The lifecycle Event stays
	// on the frame so consumers retain the authoritative cursor/work linkage.
	KindCollaborationEvent = "collaboration_event"
	KindSeqPulse           = "seq_pulse"
	KindHeartbeat          = "heartbeat"
	KindWarning            = "warning"
)

// DefaultBacklog is the --once backlog line count (mirrors the console's
// BACKLOG_LINES = 25).
const DefaultBacklog = 25

func seqFilePath(home string) string { return filepath.Join(home, "ledger", "seq") }

// Snapshot is the finite --once payload: the last N work events, the current
// ledger seq, the end-of-log byte cursor (where --follow resumes losslessly),
// and generated_at (RFC3339, so this verb qualifies as a bounded projection
// door — console/src/lib/doors.ts stream-snapshot).
type Snapshot struct {
	Kind        string       `json:"kind"`
	Events      []work.Event `json:"events"`
	Cursor      int64        `json:"cursor"`
	Seq         string       `json:"seq"`
	GeneratedAt string       `json:"generated_at"`
}

// Frame is one --follow JSONL frame. Cursor is ALWAYS the byte offset into
// work.jsonl immediately AFTER the frame's source line, so a consumer that
// reconnects from the last-seen cursor resumes without loss or duplication.
type Frame struct {
	Kind          string                   `json:"kind"`
	Event         *work.Event              `json:"event,omitempty"`
	Collaboration *work.CollaborationEvent `json:"collaboration,omitempty"`
	Seq           string                   `json:"seq,omitempty"`
	Message       string                   `json:"message,omitempty"`
	Cursor        int64                    `json:"cursor"`
}

// BuildSnapshot reads the last `backlog` work events, the current seq, and the
// end-of-log cursor. Missing log/seq files degrade to an empty, cursor-0
// snapshot rather than an error (a fresh home has not written them yet).
func BuildSnapshot(home string, backlog int, now time.Time) (Snapshot, error) {
	if backlog <= 0 {
		backlog = DefaultBacklog
	}
	seq, err := readSeq(home)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Kind: KindSnapshot, Events: []work.Event{}, Seq: seq, GeneratedAt: now.UTC().Format(time.RFC3339)}

	eventStore, err := eventstore.Open(home)
	if err != nil {
		return Snapshot{}, err
	}
	data, err := eventStore.ReadWorkLog()
	if err != nil {
		if os.IsNotExist(err) {
			return snap, nil
		}
		return Snapshot{}, err
	}
	snap.Cursor = int64(len(data))

	events := make([]work.Event, 0, backlog)
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if ev, ok := decodeEvent(raw); ok {
			events = append(events, ev)
		}
	}
	if len(events) > backlog {
		events = events[len(events)-backlog:]
	}
	snap.Events = events
	return snap, nil
}

// FollowOptions configures the --follow tail loop.
type FollowOptions struct {
	Cursor       int64         // starting byte offset into work.jsonl
	PollInterval time.Duration // event-log poll cadence
	SeqInterval  time.Duration // ledger/seq poll cadence
	Heartbeat    time.Duration // heartbeat cadence
}

// ReadAvailable is the finite form of Follow's initial pump. It consumes every
// complete work-log frame currently available after cursor, applies the same
// collaboration projection, and returns the exact next byte cursor. It owns no
// cursor state; callers persist the opaque offset they consumed.
func ReadAvailable(home string, cursor int64) ([]Frame, int64, error) {
	return readAvailable(home, cursor, false)
}

// ReadAvailableStrict is the finite replay form for projections that must
// observe one complete sampled log. Unlike Follow and ReadAvailable, it rejects
// a tail that has no trailing newline rather than buffering it for a future read.
func ReadAvailableStrict(home string, cursor int64) ([]Frame, int64, error) {
	return readAvailable(home, cursor, true)
}

func readAvailable(home string, cursor int64, strict bool) ([]Frame, int64, error) {
	if cursor < 0 {
		return nil, cursor, fmt.Errorf("stream cursor must be non-negative")
	}
	eventStore, err := eventstore.Open(home)
	if err != nil {
		return nil, cursor, err
	}
	size, err := eventStore.WorkLogSize()
	if err != nil {
		if os.IsNotExist(err) {
			return []Frame{}, 0, nil
		}
		return nil, cursor, err
	}
	t := &tailer{consumed: cursor}
	if _, truncated := ResolveOffset(cursor, size); truncated {
		t.reset()
	}
	from := t.readFrom()
	if size <= from {
		return []Frame{}, t.consumed, nil
	}
	chunk, err := eventStore.ReadWorkLogAt(from, size-from)
	if err != nil {
		return nil, cursor, err
	}
	frames := t.feed(chunk)
	if strict && (len(t.partial) != 0 || t.consumed != size) {
		return nil, cursor, fmt.Errorf("strict finite stream replay ended with an incomplete frame")
	}
	for i := range frames {
		frames[i] = projectCollaborationFrame(eventStore, frames[i])
	}
	return frames, t.consumed, nil
}

func (o FollowOptions) withDefaults() FollowOptions {
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.SeqInterval <= 0 {
		o.SeqInterval = time.Second
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	return o
}

// Follow tails the work-event log and seq pulse from opts.Cursor, writing one
// JSON frame per line to w until ctx is cancelled or w fails (a broken pipe when
// the parent kills the child on client disconnect). Read-only and lock-free:
// it never opens the log for writing and never acquires the work-event lock.
func Follow(ctx context.Context, home string, opts FollowOptions, w io.Writer) error {
	opts = opts.withDefaults()
	enc := json.NewEncoder(w)
	emit := func(f Frame) error {
		if err := enc.Encode(f); err != nil {
			return err
		}
		if fl, ok := w.(interface{ Flush() error }); ok {
			_ = fl.Flush()
		}
		return nil
	}

	eventStore, err := eventstore.Open(home)
	if err != nil {
		return err
	}
	t := &tailer{consumed: opts.Cursor}

	pump := func() error {
		size, err := eventStore.WorkLogSize()
		if err != nil {
			if os.IsNotExist(err) {
				return nil // log not written yet; a later poll picks it up
			}
			return err
		}
		if _, truncated := ResolveOffset(t.consumed, size); truncated {
			t.reset() // rotation/compaction: drop any half-buffered line, re-read from 0
		}
		from := t.readFrom()
		if size <= from {
			return nil
		}
		chunk, err := eventStore.ReadWorkLogAt(from, size-from)
		if err != nil {
			return err
		}
		for _, f := range t.feed(chunk) {
			f = projectCollaborationFrame(eventStore, f)
			if err := emit(f); err != nil {
				return err
			}
		}
		return nil
	}

	// Prime: emit any events already past the cursor (normally none — the route
	// starts the follow at end-of-log after seeding backlog through --once).
	if err := pump(); err != nil {
		return err
	}
	// Track the current seq as the baseline so only a CHANGE emits a pulse
	// (parity with the pre-verb route, which emitted memory-pulse on change only).
	lastSeq, err := readSeq(home)
	if err != nil {
		return err
	}

	evTick := time.NewTicker(opts.PollInterval)
	defer evTick.Stop()
	seqTick := time.NewTicker(opts.SeqInterval)
	defer seqTick.Stop()
	hbTick := time.NewTicker(opts.Heartbeat)
	defer hbTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-evTick.C:
			if err := pump(); err != nil {
				return err
			}
		case <-seqTick.C:
			seq, err := readSeq(home)
			if err != nil {
				return err
			}
			if seq != "" && seq != lastSeq {
				lastSeq = seq
				if err := emit(Frame{Kind: KindSeqPulse, Seq: seq, Cursor: t.consumed}); err != nil {
					return err
				}
			}
		case <-hbTick.C:
			if err := emit(Frame{Kind: KindHeartbeat, Cursor: t.consumed}); err != nil {
				return err
			}
		}
	}
}

func projectCollaborationFrame(eventStore *eventstore.Store, frame Frame) Frame {
	if frame.Event == nil {
		return frame
	}
	var refs []string
	for _, attached := range frame.Event.Attached {
		if strings.HasPrefix(attached, "wco_") {
			refs = append(refs, attached)
		}
	}
	if len(refs) == 0 {
		return frame
	}
	if len(refs) != 1 {
		frame.Kind = KindWarning
		frame.Message = fmt.Sprintf("work event carries %d collaboration attachments; want 1", len(refs))
		return frame
	}
	record, err := eventStore.Read(refs[0])
	if err != nil {
		frame.Kind = KindWarning
		frame.Message = fmt.Sprintf("collaboration attachment %s is unreadable: %v", refs[0], err)
		return frame
	}
	var collaboration work.CollaborationEvent
	if err := json.Unmarshal(record.Bytes, &collaboration); err != nil {
		frame.Kind = KindWarning
		frame.Message = fmt.Sprintf("collaboration attachment %s is malformed: %v", refs[0], err)
		return frame
	}
	if err := collaboration.Validate(); err != nil {
		frame.Kind = KindWarning
		frame.Message = fmt.Sprintf("collaboration attachment %s is invalid: %v", refs[0], err)
		return frame
	}
	if collaboration.WorkID != frame.Event.WorkID ||
		!contains(frame.Event.Attached, collaboration.ActionIntentID) ||
		!contains(frame.Event.Attached, collaboration.PolicyDecisionID) {
		frame.Kind = KindWarning
		frame.Message = fmt.Sprintf("collaboration attachment %s does not match its governed work event", refs[0])
		return frame
	}
	frame.Kind = KindCollaborationEvent
	frame.Collaboration = &collaboration
	return frame
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ResolveOffset decides where to resume reading given the previously-consumed
// offset and the file's current size. A file smaller than the last offset was
// truncated or rotated: reset to 0 and signal the caller to drop any buffered
// partial line. Mirrors tail-parser.ts resolveReadOffset.
func ResolveOffset(prev, size int64) (offset int64, truncated bool) {
	if size < prev {
		return 0, true
	}
	return prev, false
}

// tailer turns raw byte chunks read sequentially from the work-event log into
// ordered Frames, tracking the byte cursor precisely so every frame carries the
// absolute offset immediately AFTER its source line. It buffers a partial
// trailing line across chunk boundaries. Pure (no I/O), so the byte-accounting,
// partial-line buffering, and malformed-line handling are unit-testable without
// a filesystem.
type tailer struct {
	consumed int64  // absolute offset up to which COMPLETE lines have been emitted
	partial  []byte // bytes after `consumed` not yet terminated by '\n'
}

// readFrom is the next absolute offset to read from (past everything buffered).
func (t *tailer) readFrom() int64 { return t.consumed + int64(len(t.partial)) }

// reset drops all buffered state and restarts at offset 0 (log truncated/rotated).
func (t *tailer) reset() {
	t.consumed = 0
	t.partial = nil
}

// feed appends chunk (whose first byte is at absolute offset t.readFrom()) and
// returns a Frame for every complete line it now contains, advancing the cursor.
func (t *tailer) feed(chunk []byte) []Frame {
	if len(chunk) == 0 {
		return nil
	}
	buf := append(t.partial, chunk...)
	var frames []Frame
	start := 0
	for {
		nl := bytes.IndexByte(buf[start:], '\n')
		if nl < 0 {
			break
		}
		lineEnd := start + nl
		afterNL := t.consumed + int64(lineEnd) + 1 // absolute offset past the '\n'
		if f, ok := frameForLine(string(buf[start:lineEnd]), afterNL); ok {
			frames = append(frames, f)
		}
		start = lineEnd + 1
	}
	t.consumed += int64(start)
	t.partial = append([]byte(nil), buf[start:]...)
	return frames
}

// frameForLine classifies one complete line: a blank line yields nothing (not a
// warning); a non-blank line that fails to decode yields a warning frame (so the
// tail is honest about a corrupt row without crashing); a valid line yields a
// work_event frame. cursor is the byte offset immediately after this line.
func frameForLine(line string, cursor int64) (Frame, bool) {
	if strings.TrimSpace(line) == "" {
		return Frame{}, false
	}
	ev, ok := decodeEvent(line)
	if !ok {
		return Frame{Kind: KindWarning, Message: "skipped malformed work-event line", Cursor: cursor}, true
	}
	e := ev
	return Frame{Kind: KindWorkEvent, Event: &e, Cursor: cursor}, true
}

// decodeEvent parses one JSONL line into a work.Event, requiring the same
// fields the client contract requires (ts, work_id, to). A malformed or
// field-incomplete line returns ok=false — the caller drops or warns, never
// fabricates.
func decodeEvent(line string) (work.Event, bool) {
	var ev work.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil {
		return work.Event{}, false
	}
	if strings.TrimSpace(ev.TS) == "" || strings.TrimSpace(ev.WorkID) == "" || strings.TrimSpace(ev.To) == "" {
		return work.Event{}, false
	}
	return ev, true
}

// readSeq returns the trimmed ledger/seq contents, or "" if the file is absent
// (parity with the pre-verb route's readSeq()).
func readSeq(home string) (string, error) {
	b, err := os.ReadFile(seqFilePath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
