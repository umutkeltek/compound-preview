package activity

import (
	"sort"
	"time"
)

// DwellFloorSec is the minimum active duration a foreground interval must hold to survive as its own
// span. Shorter focus flickers (a 3-second glance at Slack while coding) merge into their neighbor
// rather than minting a misleading span. Deterministic, no ML.
const DwellFloorSec = 15

// rawSpan is foreground interval before dwell-floor merging. meta carries the
// most recent event's Meta map so envelope-aware classification (Phase 6) can
// distinguish per-project time without re-reading the spool. Tier-0 events
// produce rawSpan with nil/empty meta; Tier-1 events carry pane_cwd / git_remote
// / agent_kind meta that the classifier can refine on.
type rawSpan struct {
	start  time.Time
	end    time.Time
	bundle string
	app    string
	meta   map[string]string
}

// Segment reduces a day's raw events (any order) into deterministic ActivitySpans. It:
//   - coalesces consecutive same-bundle foreground into one span,
//   - closes the current span on idle_started/screen_locked/capture_gap (idle/lock time is a gap in
//     foreground, never a span),
//   - merges sub-DwellFloor flickers into their neighbor,
//   - marks EVERY span in the window coverage=incomplete if a capture_gap, permission_lost, or a seq
//     discontinuity was seen (silent outage must never read as clean activity — F11).
//
// Input events are assumed to belong to one day; ordering is normalized by seq here. Segment is pure
// and deterministic: identical input yields byte-identical spans (F12).
func Segment(events []Event) []ActivitySpan {
	if len(events) == 0 {
		return []ActivitySpan{}
	}
	ordered := make([]Event, len(events))
	copy(ordered, events)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })

	incomplete := false
	var raws []rawSpan
	var cur *rawSpan
	var lastTS time.Time
	var lastSeq int64
	haveSeq := false

	closeCur := func(at time.Time) {
		if cur != nil {
			cur.end = at
			raws = append(raws, *cur)
			cur = nil
		}
	}

	for _, ev := range ordered {
		t, err := time.Parse(time.RFC3339, ev.TS)
		if err != nil {
			// A malformed ts survived decode only if callers bypass DecodeStrict; treat defensively
			// as a coverage gap rather than silently dropping.
			incomplete = true
			continue
		}
		if haveSeq && ev.Seq != lastSeq+1 {
			incomplete = true // a seq discontinuity means the collector dropped events in this window
		}
		lastSeq = ev.Seq
		haveSeq = true
		if t.After(lastTS) {
			lastTS = t
		}

		switch ev.Kind {
		case KindAppActivated:
			if cur != nil && cur.bundle == ev.BundleID {
				continue // same app in foreground; coalesce (keep the earlier start)
			}
			closeCur(t)
			cur = &rawSpan{start: t, bundle: ev.BundleID, app: ev.AppName, meta: ev.Meta}
		case KindIdleStarted, KindScreenLocked:
			closeCur(t) // idle/lock ends the foreground interval; the gap itself is not a span
		case KindCaptureGap:
			incomplete = true
			closeCur(t)
		case KindPermissionLost:
			incomplete = true
			closeCur(t)
		case KindIdleEnded, KindScreenUnlocked:
			// Foreground is unknown until the next app_activated; nothing to open here.
		case KindWindowFocused:
			// Phase 5: Tier-1 envelope refinement. Same coalesce rule as
			// KindAppActivated — same bundle as the current foreground = stay in it
			// (keep the earlier start); different bundle = close the prior and open
			// a new one. Tier-1 events carry envelope meta (pane_cwd, git_remote, ...)
			// that downstream consumers can read off the final span; the reducer
			// itself stays bundle-shaped so it composes with Tier-0.
			//
			// Phase 6: when a Tier-1 event arrives with the same bundle, we ALSO
			// update cur.meta — the latest envelope wins. A span that covers
			// multiple project transitions within the same terminal will surface
			// the most recent project on the final span. (The span's Start time
			// keeps the FIRST event's timestamp; only meta tracks the latest.)
			if cur != nil && cur.bundle == ev.BundleID {
				cur.meta = ev.Meta
				continue
			}
			closeCur(t)
			cur = &rawSpan{start: t, bundle: ev.BundleID, app: ev.AppName, meta: ev.Meta}
		}
	}
	// Close a trailing open foreground at the last observed timestamp. If that equals its start
	// (the opener was the final event), it is a zero-length span the dwell floor drops — honest:
	// we do not know how long an unclosed foreground lasted, so we do not count it.
	closeCur(lastTS)

	merged := mergeDwellFloor(raws)
	coverage := CoverageComplete
	if incomplete {
		coverage = CoverageIncomplete
	}
	spans := make([]ActivitySpan, 0, len(merged))
	for _, r := range merged {
		cat, _ := Classify(r.bundle, r.meta)
		// Only emit Meta on the span when non-empty — Tier-0 events leave it nil,
		// and omitempty keeps the JSON shape byte-identical to pre-Phase-6 output.
		var meta map[string]string
		if len(r.meta) > 0 {
			meta = r.meta
		}
		spans = append(spans, ActivitySpan{
			Start:     r.start.UTC().Format(time.RFC3339),
			End:       r.end.UTC().Format(time.RFC3339),
			ActiveSec: int64(r.end.Sub(r.start).Seconds()),
			BundleID:  r.bundle,
			AppName:   r.app,
			Category:  cat,
			Coverage:  coverage,
			Meta:      meta,
		})
	}
	return spans
}

// mergeDwellFloor drops sub-floor flickers, absorbing their time into a neighbor, and coalesces
// adjacent contiguous same-bundle survivors. Deterministic forward pass.
func mergeDwellFloor(raws []rawSpan) []rawSpan {
	var out []rawSpan
	var carryStart time.Time
	haveCarry := false
	for _, s := range raws {
		if s.end.Sub(s.start) >= time.Duration(DwellFloorSec)*time.Second {
			start := s.start
			if haveCarry {
				start = carryStart // a leading flicker with no prior survivor prepends to this span
				haveCarry = false
			}
			s.start = start
			if n := len(out); n > 0 && out[n-1].bundle == s.bundle && !out[n-1].end.Before(s.start) {
				// contiguous same-bundle (e.g. same app on both sides of an absorbed flicker) -> coalesce
				if s.end.After(out[n-1].end) {
					out[n-1].end = s.end
				}
				continue
			}
			out = append(out, s)
			continue
		}
		// Sub-floor flicker: absorb into the previous survivor, else carry to prepend to the next.
		if n := len(out); n > 0 {
			if s.end.After(out[n-1].end) {
				out[n-1].end = s.end
			}
		} else if !haveCarry {
			carryStart = s.start
			haveCarry = true
		}
	}
	return out
}
