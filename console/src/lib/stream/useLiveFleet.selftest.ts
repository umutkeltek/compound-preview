/*
 * Depth tests for the live push plane's pure fold (the real-time board's
 * live overlay; packet 2026-07-20-realtime-multiagent-traffic-board). Would
 * FAIL if a move didn't land per-work (F1), a reconnect's replayed backlog
 * double-counted a move (F5), or the latest state didn't win. Run via
 * scripts/traffic-board-smoke.mjs.
 */

import { foldLatestMoves } from "./useLiveFleet";
import type { WorkEvent } from "./tail-parser";

type Check = { name: string; ok: boolean; detail: string };

function finalize(checks: Check[]): { ok: boolean; details: string[] } {
  return {
    ok: checks.every((c) => c.ok),
    details: checks.map((c) => `${c.ok ? "ok" : "FAIL"}: ${c.name} — ${c.detail}`),
  };
}

function ev(over: Partial<WorkEvent> = {}): WorkEvent {
  return { ts: "1", work_id: "work_1", from: "claimed", to: "running", by: "", note: "", attached: [], ...over };
}

export function runUseLiveFleetSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  // F1: a synthetic move lands, attributed to its actor — no poll involved.
  const m1 = foldLatestMoves([ev({ ts: "2", work_id: "work_1", from: "claimed", to: "running", by: "codex" })]);
  const live = m1.get("work_1");
  push("F1-synthetic-move-lands", !!live && live.to === "running" && live.by === "codex", `to=${live?.to} by=${live?.by}`);

  // F5: a replayed (reconnect backlog) duplicate dedupes to a single move.
  const dup = ev({ ts: "2", work_id: "work_1", from: "claimed", to: "running", by: "codex" });
  const m2 = foldLatestMoves([dup, { ...dup }]);
  push("F5-dedupe-replay", m2.size === 1, `size=${m2.size}`);

  // Latest-wins: a later move for the same work replaces the earlier state/actor.
  const m3 = foldLatestMoves([
    ev({ ts: "2", work_id: "work_1", to: "running", by: "codex" }),
    ev({ ts: "3", work_id: "work_1", from: "running", to: "verifying", by: "compound reviews" }),
  ]);
  const w = m3.get("work_1");
  push("latest-wins", m3.size === 1 && w?.to === "verifying" && w?.by === "compound reviews", `to=${w?.to} by=${w?.by}`);

  // The overlay is a per-work map (not a single 30-slot ticker): two works => two entries.
  const m4 = foldLatestMoves([
    ev({ ts: "2", work_id: "work_1", to: "running", by: "codex" }),
    ev({ ts: "2", work_id: "work_2", to: "claimed", by: "claude" }),
  ]);
  push("per-work-map", m4.size === 2 && m4.get("work_2")?.by === "claude", `size=${m4.size}`);

  return finalize(checks);
}
