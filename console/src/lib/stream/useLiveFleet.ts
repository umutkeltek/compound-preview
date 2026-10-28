"use client";

import { useMemo } from "react";

import { dedupeKey, type WorkEvent } from "./tail-parser";
import { useEnvelopeStream, type StreamStatus } from "./useEnvelopeStream";

/*
 * Live push plane for the real-time traffic board. Composes the existing
 * envelope-spine stream (useEnvelopeStream → /api/stream → `compound stream
 * --follow`, events carry the `by` actor) into the live "who moved what, to
 * what state, just now" overlay the board folds onto its server seed.
 *
 * This is the REAL-TIME part: a work's state + actor attribution update the
 * instant its move event lands (push), not on the 30s reconcile poll. The
 * pure fold (foldLatestMoves) is selftest-tested (useLiveFleet.selftest.ts):
 *   F1 a synthetic move lands without a poll;
 *   F5 a replayed event (reconnect backlog burst) is deduped to one move.
 */

export type LiveMove = {
  workId: string;
  by: string;
  to: string;
  ts: string;
};

/** Pure: the latest move per work, deduped by ts+work_id+to (tail-parser's
 *  dedupeKey) so a reconnect's replayed backlog never double-counts a move. */
export function foldLatestMoves(events: WorkEvent[]): Map<string, LiveMove> {
  const seen = new Set<string>();
  const latest = new Map<string, LiveMove>();
  for (const e of events) {
    const key = dedupeKey(e);
    if (seen.has(key)) continue; // F5: dedupe replayed/repeated events
    seen.add(key);
    const prev = latest.get(e.work_id);
    if (!prev || (e.ts || "") >= (prev.ts || "")) {
      latest.set(e.work_id, { workId: e.work_id, by: e.by ?? "", to: e.to, ts: e.ts });
    }
  }
  return latest;
}

export type LiveFleet = {
  status: StreamStatus;
  /** latest live move per work_id (push; empty until the stream connects). */
  moves: Map<string, LiveMove>;
};

export function useLiveFleet(): LiveFleet {
  const { events, status } = useEnvelopeStream();
  const moves = useMemo(() => foldLatestMoves(events), [events]);
  return { status, moves };
}
